package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Rememorio/gofer/internal/tool"
)

const (
	maxDiscoveryPages  = 100
	maxDiscoveredTools = 1000
	maxToolNameLength  = 64
)

var (
	// ErrInvalidConfig identifies malformed MCP client or server configuration.
	ErrInvalidConfig = errors.New("invalid MCP configuration")
	// ErrNotConnected identifies operations requiring active MCP sessions.
	ErrNotConnected = errors.New("MCP client is not connected")
	// ErrAlreadyConnected identifies a repeated Connect call.
	ErrAlreadyConnected = errors.New("MCP client is already connected")
	// ErrDiscovery identifies invalid or unbounded server tool discovery.
	ErrDiscovery = errors.New("MCP tool discovery failed")

	serverNamePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$`)
	environmentPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	headerNamePattern  = regexp.MustCompile("^[!#$%&'*+\\-.^_`|~0-9A-Za-z]+$")
)

// Transport identifies an MCP connection transport.
type Transport string

// Supported MCP transports.
const (
	TransportStdio          Transport = "stdio"
	TransportStreamableHTTP Transport = "streamable_http"
)

// Config configures an MCP client and its servers.
type Config struct {
	Servers []ServerConfig
}

// ServerConfig configures one trusted MCP server.
type ServerConfig struct {
	Name                 string
	Transport            Transport
	Command              string
	Arguments            []string
	Environment          map[string]string
	WorkingDirectory     string
	URL                  string
	Headers              map[string]string
	HTTPClient           *http.Client
	AllowInsecureHTTP    bool
	DisableStandaloneSSE bool
	MaxRetries           int
}

// Client owns MCP sessions and their current atomic tool projection.
type Client struct {
	lifecycle sync.Mutex
	mu        sync.RWMutex
	config    Config
	connector connector
	sessions  map[string]session
	tools     []tool.Tool
	connected bool
}

// New validates config and constructs a disconnected MCP client.
func New(config Config) (*Client, error) {
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	implementation := &sdk.Implementation{Name: "gofer", Version: "dev"}
	return newClient(config, &sdkConnector{client: sdk.NewClient(implementation, nil)}), nil
}

func newClient(config Config, connector connector) *Client {
	return &Client{config: cloneConfig(config), connector: connector}
}

// Connect opens every configured server and atomically publishes discovered tools.
func (client *Client) Connect(ctx context.Context) error {
	if client == nil || client.connector == nil {
		return fmt.Errorf("%w: client is not configured", ErrInvalidConfig)
	}
	client.lifecycle.Lock()
	defer client.lifecycle.Unlock()
	client.mu.Lock()
	if client.connected {
		client.mu.Unlock()
		return ErrAlreadyConnected
	}
	client.mu.Unlock()

	sessions := make(map[string]session, len(client.config.Servers))
	for _, server := range client.config.Servers {
		connected, err := client.connector.connect(ctx, server)
		if err != nil {
			return errors.Join(fmt.Errorf("connect MCP server %s: %w", server.Name, err), closeSessions(sessions))
		}
		sessions[server.Name] = connected
	}
	tools, err := discoverAll(ctx, client.config.Servers, sessions)
	if err != nil {
		return errors.Join(err, closeSessions(sessions))
	}

	client.mu.Lock()
	defer client.mu.Unlock()
	if client.connected {
		return errors.Join(ErrAlreadyConnected, closeSessions(sessions))
	}
	client.sessions = sessions
	client.tools = tools
	client.connected = true
	return nil
}

// Refresh atomically replaces the tool projection from active sessions.
func (client *Client) Refresh(ctx context.Context) error {
	if client == nil {
		return fmt.Errorf("%w: client is nil", ErrInvalidConfig)
	}
	client.mu.RLock()
	if !client.connected {
		client.mu.RUnlock()
		return ErrNotConnected
	}
	sessions := cloneSessions(client.sessions)
	servers := append([]ServerConfig(nil), client.config.Servers...)
	client.mu.RUnlock()
	tools, err := discoverAll(ctx, servers, sessions)
	if err != nil {
		return err
	}
	client.mu.Lock()
	if !client.connected {
		client.mu.Unlock()
		return ErrNotConnected
	}
	client.tools = tools
	client.mu.Unlock()
	return nil
}

// Tools returns a stable snapshot sorted by exposed tool name.
func (client *Client) Tools() ([]tool.Tool, error) {
	if client == nil {
		return nil, fmt.Errorf("%w: client is nil", ErrInvalidConfig)
	}
	client.mu.RLock()
	defer client.mu.RUnlock()
	if !client.connected {
		return nil, ErrNotConnected
	}
	return append([]tool.Tool(nil), client.tools...), nil
}

// Register atomically adds the current MCP tool projection to registry.
func (client *Client) Register(registry *tool.Registry) error {
	if registry == nil {
		return fmt.Errorf("%w: tool registry is required", ErrInvalidConfig)
	}
	tools, err := client.Tools()
	if err != nil {
		return err
	}
	return registry.RegisterAll(tools...)
}

// Close closes all active sessions and clears the tool projection.
func (client *Client) Close() error {
	if client == nil {
		return nil
	}
	client.lifecycle.Lock()
	defer client.lifecycle.Unlock()
	client.mu.Lock()
	sessions := client.sessions
	client.sessions = nil
	client.tools = nil
	client.connected = false
	client.mu.Unlock()
	return closeSessions(sessions)
}

type session interface {
	ListTools(context.Context, *sdk.ListToolsParams) (*sdk.ListToolsResult, error)
	CallTool(context.Context, *sdk.CallToolParams) (*sdk.CallToolResult, error)
	Close() error
}

type connector interface {
	connect(context.Context, ServerConfig) (session, error)
}

type sdkConnector struct {
	client *sdk.Client
}

func (connector *sdkConnector) connect(ctx context.Context, config ServerConfig) (session, error) {
	transport, err := makeTransport(ctx, config)
	if err != nil {
		return nil, err
	}
	return connector.client.Connect(ctx, transport, nil)
}

func makeTransport(ctx context.Context, config ServerConfig) (sdk.Transport, error) {
	switch config.Transport {
	case TransportStdio:
		command := exec.CommandContext(ctx, config.Command, config.Arguments...)
		command.Dir = config.WorkingDirectory
		command.Env = mergedEnvironment(config.Environment)
		return &sdk.CommandTransport{Command: command}, nil
	case TransportStreamableHTTP:
		return &sdk.StreamableClientTransport{
			Endpoint: config.URL, HTTPClient: clientWithHeaders(config.HTTPClient, config.Headers),
			DisableStandaloneSSE: config.DisableStandaloneSSE, MaxRetries: config.MaxRetries,
		}, nil
	default:
		return nil, fmt.Errorf("%w: unsupported transport %q", ErrInvalidConfig, config.Transport)
	}
}

type remoteTool struct {
	definition tool.Definition
	remoteName string
	session    session
}

func (remote remoteTool) Definition() tool.Definition {
	definition := remote.definition
	definition.InputSchema = append(json.RawMessage(nil), definition.InputSchema...)
	return definition
}

func (remote remoteTool) Execute(ctx context.Context, arguments json.RawMessage) (json.RawMessage, error) {
	value, err := decodeObject(arguments)
	if err != nil {
		return nil, err
	}
	result, err := remote.session.CallTool(ctx, &sdk.CallToolParams{Name: remote.remoteName, Arguments: value})
	if err != nil {
		return nil, fmt.Errorf("call remote MCP tool %s: %w", remote.remoteName, err)
	}
	if result == nil {
		return nil, errors.New("call remote MCP tool: nil result")
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("encode remote MCP result: %w", err)
	}
	if result.IsError || result.NeedsInput() {
		return nil, tool.NewResultError(encoded)
	}
	return encoded, nil
}

func discoverAll(ctx context.Context, servers []ServerConfig, sessions map[string]session) ([]tool.Tool, error) {
	tools := make([]tool.Tool, 0)
	names := make(map[string]string)
	for _, server := range servers {
		discovered, err := discoverServer(ctx, server.Name, sessions[server.Name])
		if err != nil {
			return nil, err
		}
		for _, candidate := range discovered {
			name := candidate.Definition().Name
			if owner, exists := names[name]; exists {
				return nil, fmt.Errorf("%w: exposed tool %q collides between %s and %s", ErrDiscovery, name, owner, server.Name)
			}
			names[name] = server.Name
			tools = append(tools, candidate)
		}
		if len(tools) > maxDiscoveredTools {
			return nil, fmt.Errorf("%w: more than %d tools", ErrDiscovery, maxDiscoveredTools)
		}
	}
	sort.Slice(tools, func(left, right int) bool {
		return tools[left].Definition().Name < tools[right].Definition().Name
	})
	return tools, nil
}

func discoverServer(ctx context.Context, serverName string, remote session) ([]tool.Tool, error) {
	if remote == nil {
		return nil, fmt.Errorf("%w: server %s has no session", ErrDiscovery, serverName)
	}
	tools := make([]tool.Tool, 0)
	cursor := ""
	seenCursors := make(map[string]struct{})
	for page := 0; page < maxDiscoveryPages; page++ {
		result, err := remote.ListTools(ctx, &sdk.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, fmt.Errorf("%w: list server %s: %w", ErrDiscovery, serverName, err)
		}
		if result == nil {
			return nil, fmt.Errorf("%w: server %s returned nil tool list", ErrDiscovery, serverName)
		}
		for _, remoteDefinition := range result.Tools {
			adapted, err := adaptTool(serverName, remoteDefinition, remote)
			if err != nil {
				return nil, err
			}
			tools = append(tools, adapted)
			if len(tools) > maxDiscoveredTools {
				return nil, fmt.Errorf("%w: server %s has more than %d tools", ErrDiscovery, serverName, maxDiscoveredTools)
			}
		}
		if result.NextCursor == "" {
			return tools, nil
		}
		if _, repeated := seenCursors[result.NextCursor]; repeated {
			return nil, fmt.Errorf("%w: server %s repeated cursor", ErrDiscovery, serverName)
		}
		seenCursors[result.NextCursor] = struct{}{}
		cursor = result.NextCursor
	}
	return nil, fmt.Errorf("%w: server %s exceeded %d pages", ErrDiscovery, serverName, maxDiscoveryPages)
}

func adaptTool(serverName string, remote *sdk.Tool, remoteSession session) (tool.Tool, error) {
	if remote == nil || strings.TrimSpace(remote.Name) == "" {
		return nil, fmt.Errorf("%w: server %s returned unnamed tool", ErrDiscovery, serverName)
	}
	schema := remote.InputSchema
	if schema == nil {
		schema = map[string]any{"type": "object"}
	}
	encodedSchema, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("%w: encode schema for %s: %w", ErrDiscovery, remote.Name, err)
	}
	var object map[string]any
	if err := json.Unmarshal(encodedSchema, &object); err != nil || object == nil {
		return nil, fmt.Errorf("%w: tool %s schema must be a JSON object", ErrDiscovery, remote.Name)
	}
	description := strings.TrimSpace(remote.Description)
	if description == "" {
		description = fmt.Sprintf("MCP tool %s from server %s", remote.Name, serverName)
	}
	return remoteTool{
		definition: tool.Definition{
			Name: exposedName(serverName, remote.Name), Description: description, InputSchema: encodedSchema,
			UntrustedOutput: true,
		},
		remoteName: remote.Name,
		session:    remoteSession,
	}, nil
}

func exposedName(serverName, remoteName string) string {
	raw := "mcp__" + sanitizeName(serverName) + "__" + sanitizeName(remoteName)
	if len(raw) <= maxToolNameLength {
		return raw
	}
	digest := sha256.Sum256([]byte(raw))
	suffix := hex.EncodeToString(digest[:4])
	return raw[:maxToolNameLength-len(suffix)-1] + "_" + suffix
}

func sanitizeName(value string) string {
	var builder strings.Builder
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '_' || character == '-' {
			builder.WriteRune(character)
		} else {
			builder.WriteByte('_')
		}
	}
	return builder.String()
}

func decodeObject(arguments json.RawMessage) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(arguments))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode MCP tool arguments: %w", err)
	}
	if value == nil {
		return nil, errors.New("decode MCP tool arguments: object is required")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("decode MCP tool arguments: multiple JSON values")
	}
	return value, nil
}

func validateConfig(config Config) error {
	if len(config.Servers) == 0 {
		return fmt.Errorf("%w: at least one server is required", ErrInvalidConfig)
	}
	names := make(map[string]struct{}, len(config.Servers))
	for index, server := range config.Servers {
		if err := validateServer(server); err != nil {
			return fmt.Errorf("%w: servers[%d]: %w", ErrInvalidConfig, index, err)
		}
		if _, duplicate := names[server.Name]; duplicate {
			return fmt.Errorf("%w: duplicate server name %q", ErrInvalidConfig, server.Name)
		}
		names[server.Name] = struct{}{}
	}
	return nil
}

func validateServer(server ServerConfig) error {
	if !serverNamePattern.MatchString(server.Name) {
		return errors.New("server name must contain only letters, digits, underscores, or hyphens")
	}
	switch server.Transport {
	case TransportStdio:
		return validateStdioServer(server)
	case TransportStreamableHTTP:
		return validateHTTPServer(server)
	default:
		return fmt.Errorf("unsupported transport %q", server.Transport)
	}
}

func validateStdioServer(server ServerConfig) error {
	if strings.TrimSpace(server.Command) == "" || strings.TrimSpace(server.Command) != server.Command ||
		server.URL != "" || len(server.Headers) > 0 || server.HTTPClient != nil || server.AllowInsecureHTTP {
		return errors.New("stdio transport requires command and forbids HTTP configuration")
	}
	if strings.ContainsRune(server.Command, 0) {
		return errors.New("stdio command contains NUL")
	}
	for _, argument := range server.Arguments {
		if strings.ContainsRune(argument, 0) {
			return errors.New("stdio argument contains NUL")
		}
	}
	if server.WorkingDirectory != "" && !filepath.IsAbs(server.WorkingDirectory) {
		return errors.New("stdio working directory must be absolute")
	}
	for name, value := range server.Environment {
		if !environmentPattern.MatchString(name) || strings.ContainsRune(value, 0) {
			return fmt.Errorf("invalid environment entry %q", name)
		}
	}
	return nil
}

func validateHTTPServer(server ServerConfig) error {
	if server.Command != "" || len(server.Arguments) > 0 || len(server.Environment) > 0 || server.WorkingDirectory != "" {
		return errors.New("streamable HTTP transport forbids process configuration")
	}
	endpoint, err := url.Parse(server.URL)
	if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.Fragment != "" ||
		(endpoint.Scheme != "https" && endpoint.Scheme != "http") {
		return errors.New("streamable HTTP URL must be absolute HTTP(S)")
	}
	if endpoint.Scheme == "http" && !server.AllowInsecureHTTP {
		return errors.New("plain HTTP requires allow_insecure_http")
	}
	for name, value := range server.Headers {
		if !validHeader(name, value) {
			return fmt.Errorf("invalid HTTP header %q", name)
		}
	}
	return nil
}

func validHeader(name, value string) bool {
	return headerNamePattern.MatchString(name) && http.CanonicalHeaderKey(name) != "" &&
		!strings.ContainsAny(value, "\r\n")
}

func mergedEnvironment(values map[string]string) []string {
	environment := append([]string(nil), os.Environ()...)
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		environment = append(environment, name+"="+values[name])
	}
	return environment
}

type headerRoundTripper struct {
	base    http.RoundTripper
	headers http.Header
}

func (transport headerRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header = request.Header.Clone()
	for name, values := range transport.headers {
		clone.Header.Del(name)
		for _, value := range values {
			clone.Header.Add(name, value)
		}
	}
	return transport.base.RoundTrip(clone)
}

func clientWithHeaders(base *http.Client, values map[string]string) *http.Client {
	client := &http.Client{}
	if base != nil {
		*client = *base
	}
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	headers := make(http.Header, len(values))
	for name, value := range values {
		headers.Set(name, value)
	}
	client.Transport = headerRoundTripper{base: transport, headers: headers}
	return client
}

func closeSessions(sessions map[string]session) error {
	names := make([]string, 0, len(sessions))
	for name := range sessions {
		names = append(names, name)
	}
	sort.Strings(names)
	errorsFound := make([]error, 0)
	for _, name := range names {
		if err := sessions[name].Close(); err != nil {
			errorsFound = append(errorsFound, fmt.Errorf("close MCP server %s: %w", name, err))
		}
	}
	return errors.Join(errorsFound...)
}

func cloneSessions(source map[string]session) map[string]session {
	cloned := make(map[string]session, len(source))
	for name, remote := range source {
		cloned[name] = remote
	}
	return cloned
}

func cloneConfig(config Config) Config {
	cloned := Config{Servers: make([]ServerConfig, len(config.Servers))}
	for index, server := range config.Servers {
		server.Arguments = append([]string(nil), server.Arguments...)
		server.Environment = cloneMap(server.Environment)
		server.Headers = cloneMap(server.Headers)
		cloned.Servers[index] = server
	}
	return cloned
}

func cloneMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	cloned := make(map[string]string, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}
