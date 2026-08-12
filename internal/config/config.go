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
	Version  int           `yaml:"config_version" json:"config_version"`
	LogLevel string        `yaml:"log_level" json:"log_level"`
	Server   ServerConfig  `yaml:"server" json:"server"`
	Runtime  RuntimeConfig `yaml:"runtime" json:"runtime"`
	Storage  StorageConfig `yaml:"storage" json:"storage"`
	Sandbox  SandboxConfig `yaml:"sandbox" json:"sandbox"`
	Models   []ModelConfig `yaml:"models" json:"models"`
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
	Driver string `yaml:"driver" json:"driver"`
	Image  string `yaml:"image" json:"image,omitempty"`
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
		Sandbox: SandboxConfig{Driver: "local"},
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
	if !oneOf(config.Sandbox.Driver, "local", "docker", "remote") {
		return fmt.Errorf("%w: unsupported sandbox.driver %q", ErrInvalid, config.Sandbox.Driver)
	}
	if config.Sandbox.Driver == "docker" && strings.TrimSpace(config.Sandbox.Image) == "" {
		return fmt.Errorf("%w: sandbox.image is required for docker", ErrInvalid)
	}
	return validateModels(config.Models)
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
