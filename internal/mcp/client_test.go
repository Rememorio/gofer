package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/tool"
)

func TestClientConnectRegisterExecute(t *testing.T) {
	t.Parallel()

	alpha := &fakeSession{pages: map[string]*sdk.ListToolsResult{
		"": {
			Tools:      []*sdk.Tool{{Name: "echo.tool", Description: "Echo", InputSchema: objectSchema()}},
			NextCursor: "next",
		},
		"next": {Tools: []*sdk.Tool{{Name: "fallback", InputSchema: nil}}},
	}, callResult: &sdk.CallToolResult{
		Content: []sdk.Content{&sdk.TextContent{Text: "ok"}}, StructuredContent: map[string]any{"ok": true},
	}}
	beta := &fakeSession{pages: map[string]*sdk.ListToolsResult{
		"": {Tools: []*sdk.Tool{{Name: "lookup", Description: "Lookup", InputSchema: objectSchema()}}},
	}, callResult: &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "found"}}}}
	connector := &fakeConnector{sessions: map[string]session{"alpha": alpha, "beta": beta}}
	config := Config{Servers: []ServerConfig{
		{Name: "alpha", Transport: TransportStdio, Command: "alpha"},
		{Name: "beta", Transport: TransportStdio, Command: "beta"},
	}}
	client := newClient(config, connector)
	if err := client.Connect(context.Background()); err != nil {
		t.Fatalf("Connect(): %v", err)
	}
	if err := client.Connect(context.Background()); !errors.Is(err, ErrAlreadyConnected) {
		t.Fatalf("Connect(second) error = %v, want ErrAlreadyConnected", err)
	}
	tools, err := client.Tools()
	if err != nil {
		t.Fatalf("Tools(): %v", err)
	}
	names := toolNames(tools)
	wantNames := []string{"mcp__alpha__echo_tool", "mcp__alpha__fallback", "mcp__beta__lookup"}
	if !reflect.DeepEqual(names, wantNames) {
		t.Fatalf("tool names = %#v, want %#v", names, wantNames)
	}
	definition := tools[0].Definition()
	definition.InputSchema[0] = '['
	if tools[0].Definition().InputSchema[0] == '[' {
		t.Fatal("Definition() returned mutable schema alias")
	}

	registry := tool.NewRegistry()
	if err := client.Register(registry); err != nil {
		t.Fatalf("Register(): %v", err)
	}
	result, err := registry.Execute(context.Background(), domain.ToolCall{
		ID: "call-1", Name: "mcp__alpha__echo_tool", Arguments: json.RawMessage(`{"n":2}`),
	})
	if err != nil || result.IsError || !json.Valid(result.Output) {
		t.Fatalf("Execute() = %#v, %v", result, err)
	}
	if len(alpha.calls) != 1 || alpha.calls[0].Name != "echo.tool" {
		t.Fatalf("remote calls = %#v", alpha.calls)
	}
	arguments := alpha.calls[0].Arguments.(map[string]any)
	if _, ok := arguments["n"].(json.Number); !ok {
		t.Fatalf("remote numeric argument type = %T, want json.Number", arguments["n"])
	}

	if err := client.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	if alpha.closeCalls != 1 || beta.closeCalls != 1 {
		t.Fatalf("close calls = %d, %d", alpha.closeCalls, beta.closeCalls)
	}
	if _, err := client.Tools(); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("Tools(after close) error = %v, want ErrNotConnected", err)
	}
	if err := client.Refresh(context.Background()); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("Refresh(after close) error = %v, want ErrNotConnected", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close(second): %v", err)
	}
}

func TestClientRefreshAndClose(t *testing.T) {
	t.Parallel()

	remote := &fakeSession{pages: map[string]*sdk.ListToolsResult{
		"": {Tools: []*sdk.Tool{{Name: "old", InputSchema: objectSchema()}}},
	}}
	client := newClient(singleServerConfig("remote"), &fakeConnector{
		sessions: map[string]session{"remote": remote},
	})
	if err := client.Connect(context.Background()); err != nil {
		t.Fatalf("Connect(): %v", err)
	}
	remote.pages = map[string]*sdk.ListToolsResult{
		"": {Tools: []*sdk.Tool{{Name: "fresh", InputSchema: objectSchema()}}},
	}
	if err := client.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh(): %v", err)
	}
	tools, err := client.Tools()
	if err != nil || !reflect.DeepEqual(toolNames(tools), []string{"mcp__remote__fresh"}) {
		t.Fatalf("Tools(after refresh) = %#v, %v", toolNames(tools), err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	if err := client.Refresh(context.Background()); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("Refresh(after close) error = %v, want ErrNotConnected", err)
	}
}

func TestOfficialSDKInMemoryInteroperability(t *testing.T) {
	t.Parallel()

	serverTransport, clientTransport := sdk.NewInMemoryTransports()
	server := sdk.NewServer(&sdk.Implementation{Name: "test-server", Version: "1.0.0"}, nil)
	sdk.AddTool(server, &sdk.Tool{Name: "greet", Description: "Greet someone"},
		func(_ context.Context, _ *sdk.CallToolRequest, input greetingInput) (*sdk.CallToolResult, greetingOutput, error) {
			return nil, greetingOutput{Message: "hello " + input.Name}, nil
		},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Run(ctx, serverTransport) }()

	official := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	officialSession, err := official.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("official Connect(): %v", err)
	}
	client := newClient(singleServerConfig("memory"), &fakeConnector{
		sessions: map[string]session{"memory": officialSession},
	})
	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect(): %v", err)
	}
	registry := tool.NewRegistry()
	if err := client.Register(registry); err != nil {
		t.Fatalf("Register(): %v", err)
	}
	result, err := registry.Execute(ctx, domain.ToolCall{
		ID: "call-1", Name: "mcp__memory__greet", Arguments: json.RawMessage(`{"name":"Gofer"}`),
	})
	if err != nil || result.IsError || !strings.Contains(string(result.Output), "hello Gofer") {
		t.Fatalf("Execute() = %#v, %v", result, err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatalf("server Run(): %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not stop after client close")
	}
}

func TestClientPreservesRemoteToolErrors(t *testing.T) {
	t.Parallel()

	remote := &fakeSession{
		pages: map[string]*sdk.ListToolsResult{"": {Tools: []*sdk.Tool{{Name: "fail", InputSchema: objectSchema()}}}},
		callResult: &sdk.CallToolResult{
			Content: []sdk.Content{&sdk.TextContent{Text: "bad input"}}, IsError: true,
		},
	}
	client := newClient(singleServerConfig("remote"), &fakeConnector{sessions: map[string]session{"remote": remote}})
	if err := client.Connect(context.Background()); err != nil {
		t.Fatalf("Connect(): %v", err)
	}
	defer func() { _ = client.Close() }()
	registry := tool.NewRegistry()
	if err := client.Register(registry); err != nil {
		t.Fatalf("Register(): %v", err)
	}
	result, err := registry.Execute(context.Background(), domain.ToolCall{
		ID: "1", Name: "mcp__remote__fail", Arguments: json.RawMessage(`{}`),
	})
	if err != nil || !result.IsError || !strings.Contains(string(result.Output), "bad input") {
		t.Fatalf("Execute() = %#v, %v", result, err)
	}
}

func TestClientConnectFailuresAreAtomic(t *testing.T) {
	t.Parallel()

	first := &fakeSession{pages: map[string]*sdk.ListToolsResult{"": {}}}
	connector := &fakeConnector{
		sessions: map[string]session{"first": first}, failServer: "second", connectErr: errors.New("offline"),
	}
	config := Config{Servers: []ServerConfig{
		{Name: "first", Transport: TransportStdio, Command: "one"},
		{Name: "second", Transport: TransportStdio, Command: "two"},
	}}
	client := newClient(config, connector)
	if err := client.Connect(context.Background()); !errors.Is(err, connector.connectErr) {
		t.Fatalf("Connect() error = %v, want %v", err, connector.connectErr)
	}
	if first.closeCalls != 1 {
		t.Fatalf("first close calls = %d, want 1", first.closeCalls)
	}
	if _, err := client.Tools(); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("Tools() error = %v, want ErrNotConnected", err)
	}
}

func TestCloseSerializesWithConnect(t *testing.T) {
	t.Parallel()

	remote := &fakeSession{pages: map[string]*sdk.ListToolsResult{"": {}}}
	started := make(chan struct{})
	release := make(chan struct{})
	client := newClient(singleServerConfig("remote"), blockingConnector{
		started: started, release: release, remote: remote,
	})
	connectDone := make(chan error, 1)
	go func() { connectDone <- client.Connect(context.Background()) }()
	<-started
	closeDone := make(chan error, 1)
	go func() { closeDone <- client.Close() }()
	select {
	case err := <-closeDone:
		t.Fatalf("Close() returned during Connect(): %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	if err := <-connectDone; err != nil {
		t.Fatalf("Connect(): %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("Close(): %v", err)
	}
	if remote.closeCalls != 1 {
		t.Fatalf("close calls = %d, want 1", remote.closeCalls)
	}
}

func TestDiscoveryRejectsInvalidServers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		session session
	}{
		{name: "nil session", session: nil},
		{name: "list error", session: &fakeSession{listErr: errors.New("list")}},
		{name: "nil result", session: &fakeSession{}},
		{name: "unnamed tool", session: &fakeSession{pages: map[string]*sdk.ListToolsResult{
			"": {Tools: []*sdk.Tool{{}}},
		}}},
		{name: "nil tool", session: &fakeSession{pages: map[string]*sdk.ListToolsResult{
			"": {Tools: []*sdk.Tool{nil}},
		}}},
		{name: "array schema", session: &fakeSession{pages: map[string]*sdk.ListToolsResult{
			"": {Tools: []*sdk.Tool{{Name: "x", InputSchema: []any{}}}},
		}}},
		{name: "unencodable schema", session: &fakeSession{pages: map[string]*sdk.ListToolsResult{
			"": {Tools: []*sdk.Tool{{Name: "x", InputSchema: make(chan int)}}},
		}}},
		{name: "repeated cursor", session: &fakeSession{pages: map[string]*sdk.ListToolsResult{
			"": {NextCursor: "same"}, "same": {NextCursor: "same"},
		}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := discoverServer(context.Background(), "remote", test.session); !errors.Is(err, ErrDiscovery) {
				t.Fatalf("discoverServer() error = %v, want ErrDiscovery", err)
			}
		})
	}

	collision := &fakeSession{pages: map[string]*sdk.ListToolsResult{
		"": {Tools: []*sdk.Tool{
			{Name: "a.b", InputSchema: objectSchema()},
			{Name: "a_b", InputSchema: objectSchema()},
		}},
	}}
	servers := []ServerConfig{{Name: "remote"}}
	if _, err := discoverAll(context.Background(), servers, map[string]session{"remote": collision}); !errors.Is(err, ErrDiscovery) {
		t.Fatalf("discoverAll(collision) error = %v, want ErrDiscovery", err)
	}
}

func TestRemoteToolProtocolFailures(t *testing.T) {
	t.Parallel()

	remote := remoteTool{remoteName: "x", session: &fakeSession{}}
	for _, arguments := range []json.RawMessage{json.RawMessage(`null`), json.RawMessage(`{`), json.RawMessage(`{} {}`)} {
		if _, err := remote.Execute(context.Background(), arguments); err == nil {
			t.Fatalf("Execute(%s) error = nil", arguments)
		}
	}
	remote.session = &fakeSession{callErr: errors.New("call")}
	if _, err := remote.Execute(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("Execute(call error) error = nil")
	}
	remote.session = &fakeSession{}
	if _, err := remote.Execute(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("Execute(nil result) error = nil")
	}
}

func TestConfigValidation(t *testing.T) {
	t.Parallel()

	valid := []Config{
		singleServerConfig("stdio"),
		{Servers: []ServerConfig{{
			Name: "http", Transport: TransportStreamableHTTP, URL: "https://example.com/mcp",
			Headers: map[string]string{"Authorization": "Bearer token"},
		}}},
		{Servers: []ServerConfig{{
			Name: "local", Transport: TransportStreamableHTTP, URL: "http://127.0.0.1:8000/mcp",
			AllowInsecureHTTP: true,
		}}},
	}
	for _, config := range valid {
		if _, err := New(config); err != nil {
			t.Fatalf("New(valid) error = %v", err)
		}
	}
	invalidServers := []ServerConfig{
		{Name: "bad name", Transport: TransportStdio, Command: "x"},
		{Name: "x", Transport: "unknown"},
		{Name: "x", Transport: TransportStdio},
		{Name: "x", Transport: TransportStdio, Command: " x "},
		{Name: "x", Transport: TransportStdio, Command: "x", URL: "https://example.com"},
		{Name: "x", Transport: TransportStdio, Command: "x", WorkingDirectory: "relative"},
		{Name: "x", Transport: TransportStdio, Command: "x", Environment: map[string]string{"BAD-NAME": "x"}},
		{Name: "x", Transport: TransportStdio, Command: "x\x00"},
		{Name: "x", Transport: TransportStdio, Command: "x", Arguments: []string{"\x00"}},
		{Name: "x", Transport: TransportStreamableHTTP, URL: "relative"},
		{Name: "x", Transport: TransportStreamableHTTP, URL: "http://example.com/mcp"},
		{Name: "x", Transport: TransportStreamableHTTP, URL: "https://user@example.com/mcp"},
		{Name: "x", Transport: TransportStreamableHTTP, URL: "https://example.com/mcp#fragment"},
		{Name: "x", Transport: TransportStreamableHTTP, URL: "https://example.com", Command: "x"},
		{Name: "x", Transport: TransportStreamableHTTP, URL: "https://example.com", Headers: map[string]string{"Bad Header": "x"}},
		{Name: "x", Transport: TransportStreamableHTTP, URL: "https://example.com", Headers: map[string]string{"X-Test": "x\nforged"}},
	}
	for _, server := range invalidServers {
		if _, err := New(Config{Servers: []ServerConfig{server}}); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("New(%#v) error = %v, want ErrInvalidConfig", server, err)
		}
	}
	if _, err := New(Config{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New(empty) error = %v, want ErrInvalidConfig", err)
	}
	duplicate := singleServerConfig("same")
	duplicate.Servers = append(duplicate.Servers, duplicate.Servers[0])
	if _, err := New(duplicate); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New(duplicate) error = %v, want ErrInvalidConfig", err)
	}
}

func TestTransportsAndHeaders(t *testing.T) {
	t.Parallel()

	stdio, err := makeTransport(context.Background(), ServerConfig{
		Name: "x", Transport: TransportStdio, Command: "program", Arguments: []string{"arg"},
		Environment: map[string]string{"GOFER_TEST_ENV": "value"}, WorkingDirectory: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("makeTransport(stdio): %v", err)
	}
	command := stdio.(*sdk.CommandTransport).Command
	if command.Path != "program" || !reflect.DeepEqual(command.Args, []string{"program", "arg"}) || command.Dir == "" {
		t.Fatalf("command = %#v", command)
	}
	if !containsString(command.Env, "GOFER_TEST_ENV=value") {
		t.Fatalf("environment does not contain configured value")
	}

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("Authorization = %q", request.Header.Get("Authorization"))
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	base := server.Client()
	client := clientWithHeaders(base, map[string]string{"Authorization": "Bearer secret"})
	request, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	request.Header.Set("Authorization", "old")
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("Do(): %v", err)
	}
	_ = response.Body.Close()
	if request.Header.Get("Authorization") != "old" {
		t.Fatal("header transport mutated original request")
	}
	httpTransport, err := makeTransport(context.Background(), ServerConfig{
		Name: "x", Transport: TransportStreamableHTTP, URL: server.URL,
		HTTPClient: base, Headers: map[string]string{"Authorization": "Bearer secret"},
		AllowInsecureHTTP: true, DisableStandaloneSSE: true, MaxRetries: -1,
	})
	if err != nil {
		t.Fatalf("makeTransport(http): %v", err)
	}
	streamable := httpTransport.(*sdk.StreamableClientTransport)
	if streamable.Endpoint != server.URL || !streamable.DisableStandaloneSSE || streamable.MaxRetries != -1 {
		t.Fatalf("streamable transport = %#v", streamable)
	}
	if _, err := makeTransport(context.Background(), ServerConfig{Transport: "bad"}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("makeTransport(bad) error = %v, want ErrInvalidConfig", err)
	}
}

func TestClientMethodValidationAndCloseErrors(t *testing.T) {
	t.Parallel()

	var nilClient *Client
	if err := nilClient.Connect(context.Background()); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil Connect() error = %v, want ErrInvalidConfig", err)
	}
	if _, err := nilClient.Tools(); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil Tools() error = %v, want ErrInvalidConfig", err)
	}
	if err := nilClient.Close(); err != nil {
		t.Fatalf("nil Close(): %v", err)
	}
	client := newClient(singleServerConfig("remote"), &fakeConnector{})
	if err := client.Register(nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Register(nil) error = %v, want ErrInvalidConfig", err)
	}
	if err := client.Register(tool.NewRegistry()); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("Register(disconnected) error = %v, want ErrNotConnected", err)
	}
	remote := &fakeSession{pages: map[string]*sdk.ListToolsResult{"": {}}, closeErr: errors.New("close")}
	client = newClient(singleServerConfig("remote"), &fakeConnector{sessions: map[string]session{"remote": remote}})
	if err := client.Connect(context.Background()); err != nil {
		t.Fatalf("Connect(): %v", err)
	}
	if err := client.Close(); !errors.Is(err, remote.closeErr) {
		t.Fatalf("Close() error = %v, want %v", err, remote.closeErr)
	}
}

func TestExposedNameIsSafeAndStable(t *testing.T) {
	t.Parallel()

	remote := strings.Repeat("long.tool/name ", 10)
	first := exposedName("server", remote)
	second := exposedName("server", remote)
	if first != second || len(first) != maxToolNameLength || strings.ContainsAny(first, ". /") {
		t.Fatalf("exposedName() = %q, %q", first, second)
	}
}

func objectSchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": true}
}

func singleServerConfig(name string) Config {
	return Config{Servers: []ServerConfig{{Name: name, Transport: TransportStdio, Command: "server"}}}
}

func toolNames(tools []tool.Tool) []string {
	names := make([]string, len(tools))
	for index, candidate := range tools {
		names[index] = candidate.Definition().Name
	}
	return names
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

type fakeConnector struct {
	mu         sync.Mutex
	sessions   map[string]session
	failServer string
	connectErr error
}

type blockingConnector struct {
	started chan<- struct{}
	release <-chan struct{}
	remote  session
}

func (connector blockingConnector) connect(context.Context, ServerConfig) (session, error) {
	close(connector.started)
	<-connector.release
	return connector.remote, nil
}

type greetingInput struct {
	Name string `json:"name"`
}

type greetingOutput struct {
	Message string `json:"message"`
}

func (connector *fakeConnector) connect(_ context.Context, config ServerConfig) (session, error) {
	connector.mu.Lock()
	defer connector.mu.Unlock()
	if config.Name == connector.failServer {
		return nil, connector.connectErr
	}
	return connector.sessions[config.Name], nil
}

type fakeSession struct {
	mu         sync.Mutex
	pages      map[string]*sdk.ListToolsResult
	listErr    error
	callResult *sdk.CallToolResult
	callErr    error
	calls      []*sdk.CallToolParams
	closeErr   error
	closeCalls int
}

func (session *fakeSession) ListTools(_ context.Context, params *sdk.ListToolsParams) (*sdk.ListToolsResult, error) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.listErr != nil {
		return nil, session.listErr
	}
	return session.pages[params.Cursor], nil
}

func (session *fakeSession) CallTool(_ context.Context, params *sdk.CallToolParams) (*sdk.CallToolResult, error) {
	session.mu.Lock()
	defer session.mu.Unlock()
	session.calls = append(session.calls, params)
	return session.callResult, session.callErr
}

func (session *fakeSession) Close() error {
	session.mu.Lock()
	defer session.mu.Unlock()
	session.closeCalls++
	return session.closeErr
}

var _ io.Closer = (*Client)(nil)
