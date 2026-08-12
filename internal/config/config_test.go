package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultsValidate(t *testing.T) {
	t.Parallel()

	config := Defaults()
	if err := config.Validate(); err != nil {
		t.Fatalf("Defaults().Validate(): %v", err)
	}
}

func TestLoadStrictYAMLAndEnvironment(t *testing.T) {
	t.Parallel()

	raw := `
config_version: 1
log_level: debug
server:
  address: 0.0.0.0:9000
storage:
  driver: memory
  dsn: ""
sandbox:
  driver: docker
  image: gofer-sandbox:latest
models:
  - name: primary
    provider: openai
    model: gpt-test
    api_key: $TEST_API_KEY
    options:
      temperature: 0.2
`
	config, err := Load(strings.NewReader(raw), WithEnvLookup(func(name string) (string, bool) {
		if name == "TEST_API_KEY" {
			return "secret", true
		}
		return "", false
	}))
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if config.Server.Address != "0.0.0.0:9000" || config.Models[0].APIKey != "secret" {
		t.Fatalf("Load() = %#v", config)
	}
	if config.Runtime.MaxTurns != Defaults().Runtime.MaxTurns {
		t.Fatalf("default runtime was not preserved: %#v", config.Runtime)
	}
	if config.Sandbox.CommandTimeoutSeconds != 600 || config.Sandbox.NetworkEnabled {
		t.Fatalf("default sandbox limits were not preserved: %#v", config.Sandbox)
	}
	if config.Browser.MaxSessions != 32 || config.Browser.Enabled {
		t.Fatalf("default browser settings were not preserved: %#v", config.Browser)
	}
}

func TestLoadRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want error
	}{
		{name: "unknown field", raw: "config_version: 1\nunknown: true\n", want: ErrInvalid},
		{name: "missing environment", raw: "config_version: 1\nmodels:\n  - name: x\n    provider: openai\n    model: x\n    api_key: $MISSING\n", want: ErrMissingEnv},
		{name: "multiple documents", raw: "config_version: 1\n---\nconfig_version: 1\n", want: ErrInvalid},
		{name: "bad YAML", raw: "config_version: [\n", want: ErrInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Load(strings.NewReader(test.raw), WithEnvLookup(func(string) (string, bool) { return "", false }))
			if !errors.Is(err, test.want) {
				t.Fatalf("Load() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestValidateRejectsInvalidConfigurations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "version", mutate: func(config *Config) { config.Version = 2 }},
		{name: "log level", mutate: func(config *Config) { config.LogLevel = "trace" }},
		{name: "address", mutate: func(config *Config) { config.Server.Address = "bad" }},
		{name: "runtime", mutate: func(config *Config) { config.Runtime.MaxTurns = 0 }},
		{name: "storage driver", mutate: func(config *Config) { config.Storage.Driver = "bad" }},
		{name: "storage DSN", mutate: func(config *Config) { config.Storage.DSN = "" }},
		{name: "sandbox driver", mutate: func(config *Config) { config.Sandbox.Driver = "bad" }},
		{name: "docker image", mutate: func(config *Config) { config.Sandbox.Driver, config.Sandbox.Image = "docker", "" }},
		{name: "sandbox timeout", mutate: func(config *Config) { config.Sandbox.CommandTimeoutSeconds = 0 }},
		{name: "sandbox timeout order", mutate: func(config *Config) { config.Sandbox.MaxTimeoutSeconds = 1 }},
		{name: "sandbox output", mutate: func(config *Config) { config.Sandbox.MaxOutputBytes = 0 }},
		{name: "sandbox script", mutate: func(config *Config) { config.Sandbox.MaxScriptBytes = 0 }},
		{name: "sandbox binary", mutate: func(config *Config) { config.Sandbox.DockerBinary = "" }},
		{name: "sandbox CPU", mutate: func(config *Config) { config.Sandbox.CPUs = 0 }},
		{name: "sandbox PIDs", mutate: func(config *Config) { config.Sandbox.PIDsLimit = 0 }},
		{name: "sandbox memory", mutate: func(config *Config) { config.Sandbox.Memory = "" }},
		{name: "host execution driver", mutate: func(config *Config) {
			config.Sandbox.Driver, config.Sandbox.Image, config.Sandbox.AllowHostExecution = "docker", "gofer:test", true
		}},
		{name: "browser sessions zero", mutate: func(config *Config) { config.Browser.MaxSessions = 0 }},
		{name: "browser sessions high", mutate: func(config *Config) { config.Browser.MaxSessions = 1025 }},
		{name: "browser idle timeout", mutate: func(config *Config) { config.Browser.IdleTimeoutSeconds = 0 }},
		{name: "browser action timeout zero", mutate: func(config *Config) { config.Browser.ActionTimeoutSeconds = 0 }},
		{name: "browser action timeout high", mutate: func(config *Config) { config.Browser.ActionTimeoutSeconds = 601 }},
		{name: "browser viewport width low", mutate: func(config *Config) { config.Browser.ViewportWidth = 319 }},
		{name: "browser viewport width high", mutate: func(config *Config) { config.Browser.ViewportWidth = 7681 }},
		{name: "browser viewport height low", mutate: func(config *Config) { config.Browser.ViewportHeight = 199 }},
		{name: "browser viewport height high", mutate: func(config *Config) { config.Browser.ViewportHeight = 4321 }},
		{name: "browser executable NUL", mutate: func(config *Config) { config.Browser.ExecutablePath = "chrome\x00" }},
		{name: "browser remote NUL", mutate: func(config *Config) { config.Browser.RemoteURL = "ws://chrome\x00" }},
		{name: "model field", mutate: func(config *Config) { config.Models = []ModelConfig{{Name: "x"}} }},
		{name: "model duplicate", mutate: func(config *Config) {
			config.Models = []ModelConfig{{Name: "x", Provider: "p", Model: "m"}, {Name: "x", Provider: "p", Model: "m"}}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := Defaults()
			test.mutate(&config)
			if err := config.Validate(); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Validate() error = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestLoadFile(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	if err := os.WriteFile(path, []byte("config_version: 1\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(): %v", err)
	}
	config, err := LoadFile(context.Background(), path)
	if err != nil {
		t.Fatalf("LoadFile(): %v", err)
	}
	if config.Version != 1 {
		t.Fatalf("Version = %d, want 1", config.Version)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := LoadFile(ctx, path); !errors.Is(err, context.Canceled) {
		t.Fatalf("LoadFile(cancelled) error = %v, want context.Canceled", err)
	}
	if _, err := LoadFile(context.Background(), filepath.Join(directory, "missing")); err == nil {
		t.Fatal("LoadFile(missing) error = nil")
	}
}

func TestLoadReportsReadAndLookupErrors(t *testing.T) {
	t.Parallel()

	if _, err := Load(errorReader{}); err == nil {
		t.Fatal("Load(errorReader) error = nil")
	}
	if _, err := Load(strings.NewReader("config_version: 1"), WithEnvLookup(nil)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Load(nil lookup) error = %v, want ErrInvalid", err)
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) {
	return 0, errors.New("read failed")
}
