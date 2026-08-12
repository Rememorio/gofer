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
  - name: native-claude
    provider: anthropic
    model: claude-test
    auth_token: $ANTHROPIC_AUTH_TOKEN
    max_tokens: 4096
`
	environment := map[string]string{"TEST_API_KEY": "secret", "ANTHROPIC_AUTH_TOKEN": "auth-token"}
	config, err := Load(strings.NewReader(raw), WithEnvLookup(func(name string) (string, bool) {
		value, exists := environment[name]
		return value, exists
	}))
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if config.Server.Address != "0.0.0.0:9000" || config.Models[0].APIKey != "secret" {
		t.Fatalf("Load() = %#v", config)
	}
	if config.Models[1].AuthToken != "auth-token" || config.Models[1].MaxTokens != 4096 {
		t.Fatalf("Anthropic model = %#v", config.Models[1])
	}
	if config.Runtime.MaxTurns != Defaults().Runtime.MaxTurns {
		t.Fatalf("default runtime was not preserved: %#v", config.Runtime)
	}
	if !config.ReadBeforeWrite.Enabled {
		t.Fatal("default read-before-write gate was not preserved")
	}
	if !config.LoopDetection.Enabled || config.LoopDetection.WarnThreshold != 3 ||
		config.LoopDetection.HardLimit != 5 || config.LoopDetection.ToolFrequencyLimit != 50 {
		t.Fatalf("default loop detection was not preserved: %#v", config.LoopDetection)
	}
	if config.Sandbox.CommandTimeoutSeconds != 600 || config.Sandbox.NetworkEnabled {
		t.Fatalf("default sandbox limits were not preserved: %#v", config.Sandbox)
	}
	if config.Browser.MaxSessions != 32 || config.Browser.Enabled {
		t.Fatalf("default browser settings were not preserved: %#v", config.Browser)
	}
	if !config.ToolOutput.Enabled || config.ToolOutput.ExternalizeMinChars != 12_000 ||
		config.ToolOutput.StorageSubdir != ".tool-results" || len(config.ToolOutput.ExemptTools) != 2 {
		t.Fatalf("default tool output budget was not preserved: %#v", config.ToolOutput)
	}
}

func TestWebDefaults(t *testing.T) {
	t.Parallel()

	web := Defaults().Web
	if web.Search.Enabled || web.Search.Provider != "brave" || web.Search.MaxResults != 5 ||
		web.Fetch.Enabled || web.Fetch.MaxResponseBytes != 2<<20 {
		t.Fatalf("Defaults().Web = %#v", web)
	}
}

func TestConversationServiceDefaults(t *testing.T) {
	t.Parallel()

	config := Defaults()
	if !config.Title.Enabled || config.Title.MaxWords != 6 || config.Title.MaxChars != 60 ||
		!config.Suggestions.Enabled || config.Suggestions.MaxSuggestions != 3 ||
		!config.InputPolish.Enabled || config.InputPolish.MaxChars != 4000 {
		t.Fatalf("conversation service defaults = %#v, %#v, %#v", config.Title, config.Suggestions, config.InputPolish)
	}
}

func TestConversationServicesAcceptKnownModelAliases(t *testing.T) {
	t.Parallel()

	config := Defaults()
	config.Models = []ModelConfig{{Name: "fast", Provider: "openai", Model: "gpt-test"}}
	config.Title.ModelName = "fast"
	config.InputPolish.ModelName = "fast"
	if err := config.Validate(); err != nil {
		t.Fatalf("Validate(): %v", err)
	}
}

func TestLoadCanDisableRuntimeGuards(t *testing.T) {
	t.Parallel()
	config, err := Load(strings.NewReader("config_version: 1\nloop_detection:\n  enabled: false\nread_before_write:\n  enabled: false\nstorage:\n  driver: memory\n"))
	if err != nil {
		t.Fatal(err)
	}
	if config.ReadBeforeWrite.Enabled {
		t.Fatal("read-before-write remained enabled")
	}
	if config.LoopDetection.Enabled {
		t.Fatal("loop detection remained enabled")
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
		{name: "runtime subagent depth", mutate: func(config *Config) { config.Runtime.MaxSubagentDepth = 0 }},
		{name: "runtime context", mutate: func(config *Config) { config.Runtime.MaxContextTokens = 0 }},
		{name: "runtime reserve", mutate: func(config *Config) { config.Runtime.ReserveTokens = config.Runtime.MaxContextTokens }},
		{name: "runtime recent", mutate: func(config *Config) { config.Runtime.MinRecentMessages = 0 }},
		{name: "runtime summary", mutate: func(config *Config) { config.Runtime.MaxSummaryChars = 0 }},
		{name: "loop warning zero", mutate: func(config *Config) { config.LoopDetection.WarnThreshold = 0 }},
		{name: "loop hard before warning", mutate: func(config *Config) { config.LoopDetection.HardLimit = 2 }},
		{name: "loop window short", mutate: func(config *Config) { config.LoopDetection.WindowSize = 4 }},
		{name: "loop window high", mutate: func(config *Config) { config.LoopDetection.WindowSize = 10_001 }},
		{name: "loop frequency zero", mutate: func(config *Config) { config.LoopDetection.ToolFrequencyWarn = 0 }},
		{name: "loop frequency order", mutate: func(config *Config) { config.LoopDetection.ToolFrequencyLimit = 20 }},
		{name: "loop frequency high", mutate: func(config *Config) { config.LoopDetection.ToolFrequencyLimit = 100_001 }},
		{name: "loop blank override", mutate: func(config *Config) {
			config.LoopDetection.ToolOverrides = map[string]ToolFrequencyOverride{" bad": {Warn: 1, HardLimit: 2}}
		}},
		{name: "loop override order", mutate: func(config *Config) {
			config.LoopDetection.ToolOverrides = map[string]ToolFrequencyOverride{"bash": {Warn: 2, HardLimit: 1}}
		}},
		{name: "tool output negative", mutate: func(config *Config) { config.ToolOutput.FallbackMaxChars = -1 }},
		{name: "tool output nested directory", mutate: func(config *Config) { config.ToolOutput.StorageSubdir = "cache/results" }},
		{name: "tool output dot directory", mutate: func(config *Config) { config.ToolOutput.StorageSubdir = ".." }},
		{name: "tool output spaced directory", mutate: func(config *Config) { config.ToolOutput.StorageSubdir = " cache" }},
		{name: "tool output duplicate exemption", mutate: func(config *Config) { config.ToolOutput.ExemptTools = []string{"read", "read"} }},
		{name: "tool output blank exemption", mutate: func(config *Config) { config.ToolOutput.ExemptTools = []string{" read"} }},
		{name: "tool output invalid override", mutate: func(config *Config) { config.ToolOutput.ToolOverrides = map[string]int{"tool": -1} }},
		{name: "read gate budgeted reads", mutate: func(config *Config) { config.ToolOutput.ExemptTools = []string{"read_file_tool"} }},
		{name: "storage driver", mutate: func(config *Config) { config.Storage.Driver = "bad" }},
		{name: "storage DSN", mutate: func(config *Config) { config.Storage.DSN = "" }},
		{name: "workspace root", mutate: func(config *Config) { config.Workspace.Root = "" }},
		{name: "workspace read limit", mutate: func(config *Config) { config.Workspace.MaxReadBytes = 0 }},
		{name: "workspace write limit", mutate: func(config *Config) { config.Workspace.MaxWriteBytes = 0 }},
		{name: "workspace upload limit", mutate: func(config *Config) { config.Workspace.MaxUploadBytes = 0 }},
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
		{name: "web search provider", mutate: func(config *Config) { config.Web.Search.Provider = "other" }},
		{name: "web search results low", mutate: func(config *Config) { config.Web.Search.MaxResults = 0 }},
		{name: "web search results high", mutate: func(config *Config) { config.Web.Search.MaxResults = 21 }},
		{name: "web search timeout", mutate: func(config *Config) { config.Web.Search.TimeoutSeconds = 0 }},
		{name: "web search safe mode", mutate: func(config *Config) { config.Web.Search.SafeSearch = "maybe" }},
		{name: "web search key whitespace", mutate: func(config *Config) { config.Web.Search.APIKey = " key" }},
		{name: "web search bad endpoint", mutate: func(config *Config) { config.Web.Search.Endpoint = "file:///tmp/index" }},
		{name: "web search endpoint credentials", mutate: func(config *Config) { config.Web.Search.Endpoint = "https://user@example.com" }},
		{name: "web Brave key", mutate: func(config *Config) { config.Web.Search.Enabled = true }},
		{name: "web SearXNG endpoint", mutate: func(config *Config) {
			config.Web.Search.Enabled, config.Web.Search.Provider = true, "searxng"
		}},
		{name: "web fetch response low", mutate: func(config *Config) { config.Web.Fetch.MaxResponseBytes = 1023 }},
		{name: "web fetch response high", mutate: func(config *Config) { config.Web.Fetch.MaxResponseBytes = 101 << 20 }},
		{name: "web fetch content low", mutate: func(config *Config) { config.Web.Fetch.MaxContentChars = 127 }},
		{name: "web fetch content high", mutate: func(config *Config) { config.Web.Fetch.MaxContentChars = 2_000_001 }},
		{name: "web fetch timeout", mutate: func(config *Config) { config.Web.Fetch.TimeoutSeconds = 0 }},
		{name: "web fetch redirects low", mutate: func(config *Config) { config.Web.Fetch.MaxRedirects = -1 }},
		{name: "web fetch redirects high", mutate: func(config *Config) { config.Web.Fetch.MaxRedirects = 21 }},
		{name: "web fetch user agent blank", mutate: func(config *Config) { config.Web.Fetch.UserAgent = "" }},
		{name: "web fetch user agent whitespace", mutate: func(config *Config) { config.Web.Fetch.UserAgent = " Gofer" }},
		{name: "web fetch user agent newline", mutate: func(config *Config) { config.Web.Fetch.UserAgent = "Gofer\nBad" }},
		{name: "skill size", mutate: func(config *Config) { config.Skills.MaxPackageBytes = 0 }},
		{name: "skill enabled root", mutate: func(config *Config) { config.Skills.Enabled, config.Skills.Root = true, "" }},
		{name: "MCP missing servers", mutate: func(config *Config) { config.MCP.Enabled = true }},
		{name: "MCP bad transport", mutate: func(config *Config) { config.MCP.Servers = []MCPServerConfig{{Name: "x", Transport: "bad"}} }},
		{name: "MCP missing command", mutate: func(config *Config) { config.MCP.Servers = []MCPServerConfig{{Name: "x", Transport: "stdio"}} }},
		{name: "MCP missing URL", mutate: func(config *Config) {
			config.MCP.Servers = []MCPServerConfig{{Name: "x", Transport: "streamable_http"}}
		}},
		{name: "MCP duplicate", mutate: func(config *Config) {
			server := MCPServerConfig{Name: "x", Transport: "stdio", Command: "x"}
			config.MCP.Servers = []MCPServerConfig{server, server}
		}},
		{name: "memory limit", mutate: func(config *Config) { config.Memory.Limit = 0 }},
		{name: "memory chars", mutate: func(config *Config) { config.Memory.MaxChars = 127 }},
		{name: "auth missing tokens", mutate: func(config *Config) { config.Auth.Enabled = true }},
		{name: "auth short secret", mutate: func(config *Config) {
			config.Auth = AuthConfig{Enabled: true, Tokens: []AuthTokenConfig{{Secret: "short", PrincipalID: "u", Permissions: []string{"admin"}}}}
		}},
		{name: "auth duplicate principal", mutate: func(config *Config) {
			token := AuthTokenConfig{Secret: "012345678901234567890123", PrincipalID: "u", Permissions: []string{"admin"}}
			config.Auth = AuthConfig{Enabled: true, Tokens: []AuthTokenConfig{token, token}}
		}},
		{name: "scheduler poll", mutate: func(config *Config) { config.Scheduler.PollIntervalSeconds = 0 }},
		{name: "scheduler lease", mutate: func(config *Config) {
			config.Scheduler.LeaseDurationSeconds = 1
			config.Scheduler.PollIntervalSeconds = 2
		}},
		{name: "scheduler batch", mutate: func(config *Config) { config.Scheduler.BatchSize = 1001 }},
		{name: "channel inflight", mutate: func(config *Config) { config.Channels.MaxInflight = 0 }},
		{name: "channel dedupe", mutate: func(config *Config) { config.Channels.DedupeTTLSeconds = 59 }},
		{name: "title words low", mutate: func(config *Config) { config.Title.MaxWords = 0 }},
		{name: "title words high", mutate: func(config *Config) { config.Title.MaxWords = 21 }},
		{name: "title chars low", mutate: func(config *Config) { config.Title.MaxChars = 9 }},
		{name: "title chars high", mutate: func(config *Config) { config.Title.MaxChars = 201 }},
		{name: "title model whitespace", mutate: func(config *Config) { config.Title.ModelName = " primary" }},
		{name: "suggestions low", mutate: func(config *Config) { config.Suggestions.MaxSuggestions = 0 }},
		{name: "suggestions high", mutate: func(config *Config) { config.Suggestions.MaxSuggestions = 6 }},
		{name: "polish chars low", mutate: func(config *Config) { config.InputPolish.MaxChars = 0 }},
		{name: "polish chars high", mutate: func(config *Config) { config.InputPolish.MaxChars = 100_001 }},
		{name: "polish model whitespace", mutate: func(config *Config) { config.InputPolish.ModelName = " primary" }},
		{name: "title unknown model", mutate: func(config *Config) { config.Title.ModelName = "missing" }},
		{name: "polish unknown model", mutate: func(config *Config) { config.InputPolish.ModelName = "missing" }},
		{name: "model field", mutate: func(config *Config) { config.Models = []ModelConfig{{Name: "x"}} }},
		{name: "model unsupported provider", mutate: func(config *Config) {
			config.Models = []ModelConfig{{Name: "x", Provider: "other", Model: "m"}}
		}},
		{name: "model spaced identity", mutate: func(config *Config) {
			config.Models = []ModelConfig{{Name: " x", Provider: "openai", Model: "m"}}
		}},
		{name: "model negative max tokens", mutate: func(config *Config) {
			config.Models = []ModelConfig{{Name: "x", Provider: "anthropic", Model: "m", MaxTokens: -1}}
		}},
		{name: "OpenAI Anthropic settings", mutate: func(config *Config) {
			config.Models = []ModelConfig{{Name: "x", Provider: "openai", Model: "m", AuthToken: "token"}}
		}},
		{name: "model duplicate credentials", mutate: func(config *Config) {
			config.Models = []ModelConfig{{Name: "x", Provider: "anthropic", Model: "m", APIKey: "key", AuthToken: "token"}}
		}},
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
