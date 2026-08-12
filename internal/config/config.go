package config

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const currentVersion = 1

var (
	// ErrInvalid identifies configuration that violates the current schema.
	ErrInvalid = errors.New("invalid configuration")
	// ErrMissingEnv identifies an unresolved environment-variable reference.
	ErrMissingEnv = errors.New("missing environment variable")
)

// Config is Gofer's versioned root configuration.
type Config struct {
	Version   int             `yaml:"config_version" json:"config_version"`
	LogLevel  string          `yaml:"log_level" json:"log_level"`
	Server    ServerConfig    `yaml:"server" json:"server"`
	Runtime   RuntimeConfig   `yaml:"runtime" json:"runtime"`
	Storage   StorageConfig   `yaml:"storage" json:"storage"`
	Sandbox   SandboxConfig   `yaml:"sandbox" json:"sandbox"`
	Browser   BrowserConfig   `yaml:"browser" json:"browser"`
	Auth      AuthConfig      `yaml:"auth" json:"auth"`
	Scheduler SchedulerConfig `yaml:"scheduler" json:"scheduler"`
	Channels  ChannelsConfig  `yaml:"channels" json:"channels"`
	Models    []ModelConfig   `yaml:"models" json:"models"`
}

// ServerConfig controls the HTTP gateway listener.
type ServerConfig struct {
	Address string `yaml:"address" json:"address"`
}

// RuntimeConfig controls bounded agent execution.
type RuntimeConfig struct {
	MaxTurns         int `yaml:"max_turns" json:"max_turns"`
	MaxParallelTools int `yaml:"max_parallel_tools" json:"max_parallel_tools"`
	MaxSubagents     int `yaml:"max_subagents" json:"max_subagents"`
	EventBuffer      int `yaml:"event_buffer" json:"event_buffer"`
}

// StorageConfig selects the durable state adapter.
type StorageConfig struct {
	Driver string `yaml:"driver" json:"driver"`
	DSN    string `yaml:"dsn" json:"dsn,omitempty"`
}

// SandboxConfig selects the host-execution boundary.
type SandboxConfig struct {
	Driver                string  `yaml:"driver" json:"driver"`
	AllowHostExecution    bool    `yaml:"allow_host_execution" json:"allow_host_execution"`
	Image                 string  `yaml:"image" json:"image,omitempty"`
	DockerBinary          string  `yaml:"docker_binary" json:"docker_binary,omitempty"`
	NetworkEnabled        bool    `yaml:"network_enabled" json:"network_enabled"`
	CommandTimeoutSeconds int     `yaml:"command_timeout_seconds" json:"command_timeout_seconds"`
	MaxTimeoutSeconds     int     `yaml:"max_timeout_seconds" json:"max_timeout_seconds"`
	MaxOutputBytes        int64   `yaml:"max_output_bytes" json:"max_output_bytes"`
	MaxScriptBytes        int     `yaml:"max_script_bytes" json:"max_script_bytes"`
	Memory                string  `yaml:"memory" json:"memory,omitempty"`
	CPUs                  float64 `yaml:"cpus" json:"cpus,omitempty"`
	PIDsLimit             int     `yaml:"pids_limit" json:"pids_limit,omitempty"`
}

// BrowserConfig controls bounded thread-scoped Chrome automation.
type BrowserConfig struct {
	Enabled               bool   `yaml:"enabled" json:"enabled"`
	ExecutablePath        string `yaml:"executable_path" json:"executable_path,omitempty"`
	RemoteURL             string `yaml:"remote_url" json:"-"`
	Headful               bool   `yaml:"headful" json:"headful"`
	AllowPrivateAddresses bool   `yaml:"allow_private_addresses" json:"allow_private_addresses"`
	MaxSessions           int    `yaml:"max_sessions" json:"max_sessions"`
	IdleTimeoutSeconds    int    `yaml:"idle_timeout_seconds" json:"idle_timeout_seconds"`
	ActionTimeoutSeconds  int    `yaml:"action_timeout_seconds" json:"action_timeout_seconds"`
	ViewportWidth         int    `yaml:"viewport_width" json:"viewport_width"`
	ViewportHeight        int    `yaml:"viewport_height" json:"viewport_height"`
}

// AuthConfig controls optional bearer authentication at the HTTP boundary.
type AuthConfig struct {
	Enabled bool              `yaml:"enabled" json:"enabled"`
	Tokens  []AuthTokenConfig `yaml:"tokens" json:"-"`
}

// AuthTokenConfig maps one opaque secret to a principal and permissions.
type AuthTokenConfig struct {
	Secret      string   `yaml:"secret" json:"-"`
	PrincipalID string   `yaml:"principal_id" json:"principal_id"`
	Permissions []string `yaml:"permissions" json:"permissions"`
}

// SchedulerConfig controls leased scheduled-task dispatch.
type SchedulerConfig struct {
	Enabled              bool `yaml:"enabled" json:"enabled"`
	PollIntervalSeconds  int  `yaml:"poll_interval_seconds" json:"poll_interval_seconds"`
	LeaseDurationSeconds int  `yaml:"lease_duration_seconds" json:"lease_duration_seconds"`
	BatchSize            int  `yaml:"batch_size" json:"batch_size"`
}

// ChannelsConfig controls normalized inbound message dispatch.
type ChannelsConfig struct {
	Enabled          bool `yaml:"enabled" json:"enabled"`
	MaxInflight      int  `yaml:"max_inflight" json:"max_inflight"`
	DedupeTTLSeconds int  `yaml:"dedupe_ttl_seconds" json:"dedupe_ttl_seconds"`
}

// ModelConfig names one model exposed to agent runs.
type ModelConfig struct {
	Name     string         `yaml:"name" json:"name"`
	Provider string         `yaml:"provider" json:"provider"`
	Model    string         `yaml:"model" json:"model"`
	APIKey   string         `yaml:"api_key" json:"-"`
	BaseURL  string         `yaml:"base_url" json:"base_url,omitempty"`
	Options  map[string]any `yaml:"options" json:"options,omitempty"`
}

// Option customizes configuration loading.
type Option func(*loadOptions)

type loadOptions struct {
	lookupEnv func(string) (string, bool)
}

// WithEnvLookup replaces environment lookup, primarily for deterministic tests.
func WithEnvLookup(lookup func(string) (string, bool)) Option {
	return func(options *loadOptions) {
		options.lookupEnv = lookup
	}
}

// Defaults returns a valid baseline configuration without model credentials.
func Defaults() Config {
	return Config{
		Version:  currentVersion,
		LogLevel: "info",
		Server:   ServerConfig{Address: "127.0.0.1:8001"},
		Runtime: RuntimeConfig{
			MaxTurns:         100,
			MaxParallelTools: 8,
			MaxSubagents:     8,
			EventBuffer:      128,
		},
		Storage: StorageConfig{Driver: "sqlite", DSN: ".gofer/gofer.db"},
		Sandbox: SandboxConfig{
			Driver: "local", DockerBinary: "docker", CommandTimeoutSeconds: 600,
			MaxTimeoutSeconds: 3600, MaxOutputBytes: 1 << 20, MaxScriptBytes: 64 << 10,
			Memory: "1g", CPUs: 2, PIDsLimit: 256,
		},
		Browser: BrowserConfig{
			MaxSessions: 32, IdleTimeoutSeconds: 1800, ActionTimeoutSeconds: 30,
			ViewportWidth: 1280, ViewportHeight: 720,
		},
		Auth:      AuthConfig{},
		Scheduler: SchedulerConfig{PollIntervalSeconds: 5, LeaseDurationSeconds: 300, BatchSize: 32},
		Channels:  ChannelsConfig{MaxInflight: 32, DedupeTTLSeconds: 86400},
	}
}

// Load decodes strict YAML after resolving environment-variable references.
func Load(reader io.Reader, options ...Option) (Config, error) {
	settings := loadOptions{lookupEnv: os.LookupEnv}
	for _, option := range options {
		option(&settings)
	}
	if settings.lookupEnv == nil {
		return Config{}, fmt.Errorf("%w: environment lookup is nil", ErrInvalid)
	}

	raw, err := io.ReadAll(reader)
	if err != nil {
		return Config{}, fmt.Errorf("read configuration: %w", err)
	}
	expanded, err := expandEnvironment(string(raw), settings.lookupEnv)
	if err != nil {
		return Config{}, err
	}

	config := Defaults()
	decoder := yaml.NewDecoder(strings.NewReader(expanded))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("%w: decode YAML: %w", ErrInvalid, err)
	}
	if err := ensureSingleDocument(decoder); err != nil {
		return Config{}, err
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

// LoadFile reads a configuration file after honoring context cancellation.
func LoadFile(ctx context.Context, path string, options ...Option) (Config, error) {
	if err := ctx.Err(); err != nil {
		return Config{}, fmt.Errorf("load configuration: %w", err)
	}
	file, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open configuration: %w", err)
	}
	defer func() { _ = file.Close() }()
	config, err := Load(file, options...)
	if err != nil {
		return Config{}, fmt.Errorf("load %s: %w", path, err)
	}
	return config, nil
}

// Validate verifies the current configuration contract.
func (config Config) Validate() error {
	if err := config.validateCore(); err != nil {
		return err
	}
	if err := validateSandbox(config.Sandbox); err != nil {
		return err
	}
	if err := validateBrowser(config.Browser); err != nil {
		return err
	}
	if err := validateServices(config.Auth, config.Scheduler, config.Channels); err != nil {
		return err
	}
	return validateModels(config.Models)
}

func validateServices(auth AuthConfig, scheduler SchedulerConfig, channels ChannelsConfig) error {
	if auth.Enabled && len(auth.Tokens) == 0 {
		return fmt.Errorf("%w: auth tokens are required when enabled", ErrInvalid)
	}
	principals := make(map[string]struct{}, len(auth.Tokens))
	for _, token := range auth.Tokens {
		if len(strings.TrimSpace(token.Secret)) < 24 || strings.TrimSpace(token.PrincipalID) == "" || len(token.Permissions) == 0 {
			return fmt.Errorf("%w: invalid auth token", ErrInvalid)
		}
		if _, exists := principals[token.PrincipalID]; exists {
			return fmt.Errorf("%w: duplicate auth principal", ErrInvalid)
		}
		principals[token.PrincipalID] = struct{}{}
	}
	if scheduler.PollIntervalSeconds < 1 || scheduler.LeaseDurationSeconds < scheduler.PollIntervalSeconds || scheduler.BatchSize < 1 || scheduler.BatchSize > 1000 {
		return fmt.Errorf("%w: invalid scheduler limits", ErrInvalid)
	}
	if channels.MaxInflight < 1 || channels.MaxInflight > 10_000 || channels.DedupeTTLSeconds < 60 {
		return fmt.Errorf("%w: invalid channel limits", ErrInvalid)
	}
	return nil
}

func (config Config) validateCore() error {
	if config.Version != currentVersion {
		return fmt.Errorf("%w: config_version must be %d", ErrInvalid, currentVersion)
	}
	if !oneOf(config.LogLevel, "debug", "info", "warn", "error") {
		return fmt.Errorf("%w: unsupported log_level %q", ErrInvalid, config.LogLevel)
	}
	if _, _, err := net.SplitHostPort(config.Server.Address); err != nil {
		return fmt.Errorf("%w: server.address: %w", ErrInvalid, err)
	}
	if config.Runtime.MaxTurns <= 0 || config.Runtime.MaxParallelTools <= 0 ||
		config.Runtime.MaxSubagents <= 0 || config.Runtime.EventBuffer <= 0 {
		return fmt.Errorf("%w: runtime limits must be positive", ErrInvalid)
	}
	if !oneOf(config.Storage.Driver, "memory", "sqlite", "postgres") {
		return fmt.Errorf("%w: unsupported storage.driver %q", ErrInvalid, config.Storage.Driver)
	}
	if config.Storage.Driver != "memory" && strings.TrimSpace(config.Storage.DSN) == "" {
		return fmt.Errorf("%w: storage.dsn is required for %s", ErrInvalid, config.Storage.Driver)
	}
	return nil
}

func validateSandbox(sandbox SandboxConfig) error {
	if !oneOf(sandbox.Driver, "local", "docker", "remote") {
		return fmt.Errorf("%w: unsupported sandbox.driver %q", ErrInvalid, sandbox.Driver)
	}
	if sandbox.Driver == "docker" && strings.TrimSpace(sandbox.Image) == "" {
		return fmt.Errorf("%w: sandbox.image is required for docker", ErrInvalid)
	}
	if sandbox.CommandTimeoutSeconds <= 0 || sandbox.MaxTimeoutSeconds <= 0 ||
		sandbox.CommandTimeoutSeconds > sandbox.MaxTimeoutSeconds ||
		sandbox.MaxOutputBytes <= 0 || sandbox.MaxScriptBytes <= 0 {
		return fmt.Errorf("%w: sandbox command limits must be positive and ordered", ErrInvalid)
	}
	if strings.TrimSpace(sandbox.DockerBinary) == "" || sandbox.CPUs <= 0 ||
		sandbox.PIDsLimit <= 0 || strings.TrimSpace(sandbox.Memory) == "" {
		return fmt.Errorf("%w: sandbox runtime and resource settings are required", ErrInvalid)
	}
	if sandbox.Driver != "local" && sandbox.AllowHostExecution {
		return fmt.Errorf("%w: allow_host_execution is only valid for local sandbox", ErrInvalid)
	}
	return nil
}

func validateBrowser(browser BrowserConfig) error {
	if browser.MaxSessions <= 0 || browser.MaxSessions > 1024 ||
		browser.IdleTimeoutSeconds <= 0 || browser.ActionTimeoutSeconds <= 0 ||
		browser.ActionTimeoutSeconds > 600 || browser.ViewportWidth < 320 ||
		browser.ViewportWidth > 7680 || browser.ViewportHeight < 200 || browser.ViewportHeight > 4320 {
		return fmt.Errorf("%w: invalid browser limits or viewport", ErrInvalid)
	}
	if strings.IndexByte(browser.ExecutablePath, 0) >= 0 || strings.IndexByte(browser.RemoteURL, 0) >= 0 {
		return fmt.Errorf("%w: browser endpoint contains NUL", ErrInvalid)
	}
	return nil
}

func validateModels(models []ModelConfig) error {
	names := make(map[string]struct{}, len(models))
	for index, model := range models {
		if strings.TrimSpace(model.Name) == "" || strings.TrimSpace(model.Provider) == "" || strings.TrimSpace(model.Model) == "" {
			return fmt.Errorf("%w: models[%d] requires name, provider, and model", ErrInvalid, index)
		}
		if _, duplicate := names[model.Name]; duplicate {
			return fmt.Errorf("%w: duplicate model name %q", ErrInvalid, model.Name)
		}
		names[model.Name] = struct{}{}
	}
	return nil
}

func expandEnvironment(raw string, lookup func(string) (string, bool)) (string, error) {
	missing := make(map[string]struct{})
	expanded := os.Expand(raw, func(name string) string {
		if value, exists := lookup(name); exists {
			return value
		}
		missing[name] = struct{}{}
		return ""
	})
	if len(missing) == 0 {
		return expanded, nil
	}
	names := make([]string, 0, len(missing))
	for name := range missing {
		names = append(names, name)
	}
	sort.Strings(names)
	return "", fmt.Errorf("%w: %s", ErrMissingEnv, strings.Join(names, ", "))
}

func ensureSingleDocument(decoder *yaml.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return fmt.Errorf("%w: decode trailing YAML: %w", ErrInvalid, err)
	}
	return fmt.Errorf("%w: multiple YAML documents are not supported", ErrInvalid)
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}
