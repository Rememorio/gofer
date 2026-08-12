package config

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
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
	Version         int                   `yaml:"config_version" json:"config_version"`
	LogLevel        string                `yaml:"log_level" json:"log_level"`
	Server          ServerConfig          `yaml:"server" json:"server"`
	Runtime         RuntimeConfig         `yaml:"runtime" json:"runtime"`
	LoopDetection   LoopDetectionConfig   `yaml:"loop_detection" json:"loop_detection"`
	ReadBeforeWrite ReadBeforeWriteConfig `yaml:"read_before_write" json:"read_before_write"`
	ToolOutput      ToolOutputConfig      `yaml:"tool_output" json:"tool_output"`
	Storage         StorageConfig         `yaml:"storage" json:"storage"`
	Workspace       WorkspaceConfig       `yaml:"workspace" json:"workspace"`
	Uploads         UploadsConfig         `yaml:"uploads" json:"uploads"`
	Sandbox         SandboxConfig         `yaml:"sandbox" json:"sandbox"`
	Browser         BrowserConfig         `yaml:"browser" json:"browser"`
	Web             WebConfig             `yaml:"web" json:"web"`
	Skills          SkillsConfig          `yaml:"skills" json:"skills"`
	MCP             MCPConfig             `yaml:"mcp" json:"mcp"`
	Memory          MemoryConfig          `yaml:"memory" json:"memory"`
	Auth            AuthConfig            `yaml:"auth" json:"auth"`
	Scheduler       SchedulerConfig       `yaml:"scheduler" json:"scheduler"`
	Channels        ChannelsConfig        `yaml:"channels" json:"channels"`
	Title           TitleConfig           `yaml:"title" json:"title"`
	Suggestions     SuggestionsConfig     `yaml:"suggestions" json:"suggestions"`
	InputPolish     InputPolishConfig     `yaml:"input_polish" json:"input_polish"`
	Models          []ModelConfig         `yaml:"models" json:"models"`
}

// ReadBeforeWriteConfig controls the version gate for modifying existing
// workspace and output files.
type ReadBeforeWriteConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
}

// LoopDetectionConfig controls repeated-call and per-tool frequency guards.
type LoopDetectionConfig struct {
	Enabled            bool                             `yaml:"enabled" json:"enabled"`
	WarnThreshold      int                              `yaml:"warn_threshold" json:"warn_threshold"`
	HardLimit          int                              `yaml:"hard_limit" json:"hard_limit"`
	WindowSize         int                              `yaml:"window_size" json:"window_size"`
	ToolFrequencyWarn  int                              `yaml:"tool_freq_warn" json:"tool_freq_warn"`
	ToolFrequencyLimit int                              `yaml:"tool_freq_hard_limit" json:"tool_freq_hard_limit"`
	ToolOverrides      map[string]ToolFrequencyOverride `yaml:"tool_freq_overrides" json:"tool_freq_overrides"`
}

// ToolFrequencyOverride replaces global frequency thresholds for one tool.
type ToolFrequencyOverride struct {
	Warn      int `yaml:"warn" json:"warn"`
	HardLimit int `yaml:"hard_limit" json:"hard_limit"`
}

// ServerConfig controls the HTTP gateway listener.
type ServerConfig struct {
	Address string `yaml:"address" json:"address"`
}

// RuntimeConfig controls bounded agent execution.
type RuntimeConfig struct {
	MaxTurns          int `yaml:"max_turns" json:"max_turns"`
	MaxParallelTools  int `yaml:"max_parallel_tools" json:"max_parallel_tools"`
	MaxSubagents      int `yaml:"max_subagents" json:"max_subagents"`
	MaxSubagentDepth  int `yaml:"max_subagent_depth" json:"max_subagent_depth"`
	EventBuffer       int `yaml:"event_buffer" json:"event_buffer"`
	MaxContextTokens  int `yaml:"max_context_tokens" json:"max_context_tokens"`
	ReserveTokens     int `yaml:"reserve_tokens" json:"reserve_tokens"`
	MinRecentMessages int `yaml:"min_recent_messages" json:"min_recent_messages"`
	MaxSummaryChars   int `yaml:"max_summary_chars" json:"max_summary_chars"`
}

// ToolOutputConfig bounds tool results before they re-enter model context.
type ToolOutputConfig struct {
	Enabled             bool           `yaml:"enabled" json:"enabled"`
	ExternalizeMinChars int            `yaml:"externalize_min_chars" json:"externalize_min_chars"`
	PreviewHeadChars    int            `yaml:"preview_head_chars" json:"preview_head_chars"`
	PreviewTailChars    int            `yaml:"preview_tail_chars" json:"preview_tail_chars"`
	FallbackMaxChars    int            `yaml:"fallback_max_chars" json:"fallback_max_chars"`
	FallbackHeadChars   int            `yaml:"fallback_head_chars" json:"fallback_head_chars"`
	FallbackTailChars   int            `yaml:"fallback_tail_chars" json:"fallback_tail_chars"`
	StorageSubdir       string         `yaml:"storage_subdir" json:"storage_subdir"`
	ExemptTools         []string       `yaml:"exempt_tools" json:"exempt_tools"`
	ToolOverrides       map[string]int `yaml:"tool_overrides" json:"tool_overrides"`
}

// StorageConfig selects the durable state adapter.
type StorageConfig struct {
	Driver string `yaml:"driver" json:"driver"`
	DSN    string `yaml:"dsn" json:"dsn,omitempty"`
}

// WorkspaceConfig controls isolated per-thread files and transfer limits.
type WorkspaceConfig struct {
	Root           string `yaml:"root" json:"root"`
	MaxReadBytes   int64  `yaml:"max_read_bytes" json:"max_read_bytes"`
	MaxWriteBytes  int64  `yaml:"max_write_bytes" json:"max_write_bytes"`
	MaxUploadBytes int64  `yaml:"max_upload_bytes" json:"max_upload_bytes"`
}

// UploadsConfig controls request-wide upload limits, optional document
// conversion, and bounded current-upload context.
type UploadsConfig struct {
	MaxFiles                 int      `yaml:"max_files" json:"max_files"`
	MaxTotalBytes            int64    `yaml:"max_total_bytes" json:"max_total_bytes"`
	AutoConvertDocuments     bool     `yaml:"auto_convert_documents" json:"auto_convert_documents"`
	ConverterCommand         []string `yaml:"converter_command" json:"converter_command"`
	ConversionTimeoutSeconds int      `yaml:"conversion_timeout_seconds" json:"conversion_timeout_seconds"`
	MaxConvertedBytes        int64    `yaml:"max_converted_bytes" json:"max_converted_bytes"`
	MaxContextFiles          int      `yaml:"max_context_files" json:"max_context_files"`
	MaxContextChars          int      `yaml:"max_context_chars" json:"max_context_chars"`
	MaxOutlineEntries        int      `yaml:"max_outline_entries" json:"max_outline_entries"`
	MaxPreviewLines          int      `yaml:"max_preview_lines" json:"max_preview_lines"`
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

// WebConfig controls optional bounded web research tools.
type WebConfig struct {
	Search WebSearchConfig `yaml:"search" json:"search"`
	Fetch  WebFetchConfig  `yaml:"fetch" json:"fetch"`
}

// WebSearchConfig selects a normalized web-search provider.
type WebSearchConfig struct {
	Enabled               bool   `yaml:"enabled" json:"enabled"`
	Provider              string `yaml:"provider" json:"provider"`
	APIKey                string `yaml:"api_key" json:"-"`
	Endpoint              string `yaml:"endpoint" json:"endpoint,omitempty"`
	MaxResults            int    `yaml:"max_results" json:"max_results"`
	TimeoutSeconds        int    `yaml:"timeout_seconds" json:"timeout_seconds"`
	SafeSearch            string `yaml:"safe_search" json:"safe_search"`
	AllowPrivateAddresses bool   `yaml:"allow_private_addresses" json:"allow_private_addresses"`
}

// WebFetchConfig controls direct text document retrieval.
type WebFetchConfig struct {
	Enabled               bool   `yaml:"enabled" json:"enabled"`
	MaxResponseBytes      int64  `yaml:"max_response_bytes" json:"max_response_bytes"`
	MaxContentChars       int    `yaml:"max_content_chars" json:"max_content_chars"`
	TimeoutSeconds        int    `yaml:"timeout_seconds" json:"timeout_seconds"`
	MaxRedirects          int    `yaml:"max_redirects" json:"max_redirects"`
	UserAgent             string `yaml:"user_agent" json:"user_agent"`
	AllowPrivateAddresses bool   `yaml:"allow_private_addresses" json:"allow_private_addresses"`
}

// SkillsConfig controls local progressive-disclosure skill packages.
type SkillsConfig struct {
	Enabled          bool   `yaml:"enabled" json:"enabled"`
	Root             string `yaml:"root" json:"root,omitempty"`
	ProjectionRoot   string `yaml:"projection_root" json:"projection_root,omitempty"`
	MaxDocumentBytes int64  `yaml:"max_document_bytes" json:"max_document_bytes"`
	MaxPackageBytes  int64  `yaml:"max_package_bytes" json:"max_package_bytes"`
}

// MCPConfig controls trusted external Model Context Protocol servers.
type MCPConfig struct {
	Enabled bool              `yaml:"enabled" json:"enabled"`
	Servers []MCPServerConfig `yaml:"servers" json:"servers"`
}

// MCPServerConfig configures one stdio or Streamable HTTP MCP endpoint.
type MCPServerConfig struct {
	Name                 string            `yaml:"name" json:"name"`
	Transport            string            `yaml:"transport" json:"transport"`
	Command              string            `yaml:"command" json:"command,omitempty"`
	Arguments            []string          `yaml:"arguments" json:"arguments,omitempty"`
	Environment          map[string]string `yaml:"environment" json:"-"`
	WorkingDirectory     string            `yaml:"working_directory" json:"working_directory,omitempty"`
	URL                  string            `yaml:"url" json:"url,omitempty"`
	Headers              map[string]string `yaml:"headers" json:"-"`
	AllowInsecureHTTP    bool              `yaml:"allow_insecure_http" json:"allow_insecure_http"`
	DisableStandaloneSSE bool              `yaml:"disable_standalone_sse" json:"disable_standalone_sse"`
	MaxRetries           int               `yaml:"max_retries" json:"max_retries"`
}

// MemoryConfig controls scoped memory recall and agent memory tools.
type MemoryConfig struct {
	Enabled  bool `yaml:"enabled" json:"enabled"`
	Limit    int  `yaml:"limit" json:"limit"`
	MaxChars int  `yaml:"max_chars" json:"max_chars"`
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
	Enabled           bool                   `yaml:"enabled" json:"enabled"`
	MaxInflight       int                    `yaml:"max_inflight" json:"max_inflight"`
	QueueCapacity     int                    `yaml:"queue_capacity" json:"queue_capacity"`
	DedupeTTLSeconds  int                    `yaml:"dedupe_ttl_seconds" json:"dedupe_ttl_seconds"`
	UnauthorizedReply string                 `yaml:"unauthorized_reply" json:"unauthorized_reply"`
	Bindings          []ChannelBindingConfig `yaml:"bindings" json:"bindings"`
	Webhook           ChannelWebhookConfig   `yaml:"webhook" json:"webhook"`
	Slack             ChannelSlackConfig     `yaml:"slack" json:"slack"`
	Telegram          ChannelTelegramConfig  `yaml:"telegram" json:"telegram"`
	Discord           ChannelDiscordConfig   `yaml:"discord" json:"discord"`
	Feishu            ChannelFeishuConfig    `yaml:"feishu" json:"feishu"`
	DingTalk          ChannelDingTalkConfig  `yaml:"dingtalk" json:"dingtalk"`
	WeCom             ChannelWeComConfig     `yaml:"wecom" json:"wecom"`
	WeChat            ChannelWeChatConfig    `yaml:"wechat" json:"wechat"`
	GitHub            ChannelGitHubConfig    `yaml:"github" json:"github"`
	Buzz              ChannelBuzzConfig      `yaml:"buzz" json:"buzz"`
}

// ChannelBindingConfig bootstraps one operator-approved external identity.
type ChannelBindingConfig struct {
	UserID           string `yaml:"user_id" json:"user_id"`
	Provider         string `yaml:"provider" json:"provider"`
	WorkspaceID      string `yaml:"workspace_id" json:"workspace_id,omitempty"`
	WorkspaceName    string `yaml:"workspace_name" json:"workspace_name,omitempty"`
	ExternalUserID   string `yaml:"external_user_id" json:"external_user_id"`
	ExternalUserName string `yaml:"external_user_name" json:"external_user_name,omitempty"`
}

// ChannelWebhookConfig controls the generic signed webhook adapter.
type ChannelWebhookConfig struct {
	Enabled               bool   `yaml:"enabled" json:"enabled"`
	Secret                string `yaml:"secret" json:"-"`
	OutboundURL           string `yaml:"outbound_url" json:"outbound_url,omitempty"`
	TimeoutSeconds        int    `yaml:"timeout_seconds" json:"timeout_seconds"`
	MaxAttempts           int    `yaml:"max_attempts" json:"max_attempts"`
	MaxBodyBytes          int64  `yaml:"max_body_bytes" json:"max_body_bytes"`
	ClockSkewSeconds      int    `yaml:"clock_skew_seconds" json:"clock_skew_seconds"`
	AllowPrivateAddresses bool   `yaml:"allow_private_addresses" json:"allow_private_addresses"`
}

// ChannelSlackConfig controls the Slack Socket Mode provider.
type ChannelSlackConfig struct {
	Enabled               bool     `yaml:"enabled" json:"enabled"`
	BotToken              string   `yaml:"bot_token" json:"-"`
	AppToken              string   `yaml:"app_token" json:"-"`
	BotUserID             string   `yaml:"bot_user_id" json:"bot_user_id,omitempty"`
	AllowedUsers          []string `yaml:"allowed_users" json:"allowed_users"`
	RequestTimeoutSeconds int      `yaml:"request_timeout_seconds" json:"request_timeout_seconds"`
	MaxAttempts           int      `yaml:"max_attempts" json:"max_attempts"`
}

// ChannelTelegramConfig controls the Telegram Bot API provider.
type ChannelTelegramConfig struct {
	Enabled               bool     `yaml:"enabled" json:"enabled"`
	BotToken              string   `yaml:"bot_token" json:"-"`
	AllowedUsers          []string `yaml:"allowed_users" json:"allowed_users"`
	PollTimeoutSeconds    int      `yaml:"poll_timeout_seconds" json:"poll_timeout_seconds"`
	RequestTimeoutSeconds int      `yaml:"request_timeout_seconds" json:"request_timeout_seconds"`
	MaxAttempts           int      `yaml:"max_attempts" json:"max_attempts"`
}

// ChannelDiscordConfig controls the Discord Gateway provider.
type ChannelDiscordConfig struct {
	Enabled               bool     `yaml:"enabled" json:"enabled"`
	BotToken              string   `yaml:"bot_token" json:"-"`
	AllowedGuilds         []string `yaml:"allowed_guilds" json:"allowed_guilds"`
	AllowedChannels       []string `yaml:"allowed_channels" json:"allowed_channels"`
	MentionOnly           bool     `yaml:"mention_only" json:"mention_only"`
	ThreadMode            bool     `yaml:"thread_mode" json:"thread_mode"`
	RequestTimeoutSeconds int      `yaml:"request_timeout_seconds" json:"request_timeout_seconds"`
	MaxAttempts           int      `yaml:"max_attempts" json:"max_attempts"`
}

// ChannelFeishuConfig controls the Feishu/Lark long-connection provider.
type ChannelFeishuConfig struct {
	Enabled               bool     `yaml:"enabled" json:"enabled"`
	AppID                 string   `yaml:"app_id" json:"app_id"`
	AppSecret             string   `yaml:"app_secret" json:"-"`
	Domain                string   `yaml:"domain" json:"domain,omitempty"`
	AllowedUsers          []string `yaml:"allowed_users" json:"allowed_users"`
	RequestTimeoutSeconds int      `yaml:"request_timeout_seconds" json:"request_timeout_seconds"`
	MaxAttempts           int      `yaml:"max_attempts" json:"max_attempts"`
}

// ChannelDingTalkConfig controls the DingTalk Stream Mode provider.
type ChannelDingTalkConfig struct {
	Enabled               bool     `yaml:"enabled" json:"enabled"`
	ClientID              string   `yaml:"client_id" json:"client_id"`
	ClientSecret          string   `yaml:"client_secret" json:"-"`
	AllowedUsers          []string `yaml:"allowed_users" json:"allowed_users"`
	RequestTimeoutSeconds int      `yaml:"request_timeout_seconds" json:"request_timeout_seconds"`
	MaxAttempts           int      `yaml:"max_attempts" json:"max_attempts"`
}

// ChannelWeComConfig controls the WeCom AI Bot WebSocket provider.
type ChannelWeComConfig struct {
	Enabled               bool     `yaml:"enabled" json:"enabled"`
	BotID                 string   `yaml:"bot_id" json:"bot_id"`
	BotSecret             string   `yaml:"bot_secret" json:"-"`
	WorkingMessage        string   `yaml:"working_message" json:"working_message"`
	AllowedUsers          []string `yaml:"allowed_users" json:"allowed_users"`
	HeartbeatSeconds      int      `yaml:"heartbeat_seconds" json:"heartbeat_seconds"`
	RequestTimeoutSeconds int      `yaml:"request_timeout_seconds" json:"request_timeout_seconds"`
	MaxAttempts           int      `yaml:"max_attempts" json:"max_attempts"`
}

// ChannelWeChatConfig controls the WeChat iLink long-polling provider.
type ChannelWeChatConfig struct {
	Enabled               bool     `yaml:"enabled" json:"enabled"`
	BotToken              string   `yaml:"bot_token" json:"-"`
	ILinkBotID            string   `yaml:"ilink_bot_id" json:"ilink_bot_id,omitempty"`
	ILinkAppID            string   `yaml:"ilink_app_id" json:"ilink_app_id,omitempty"`
	RouteTag              string   `yaml:"route_tag" json:"route_tag,omitempty"`
	ChannelVersion        string   `yaml:"channel_version" json:"channel_version"`
	AllowedUsers          []string `yaml:"allowed_users" json:"allowed_users"`
	PollTimeoutSeconds    int      `yaml:"poll_timeout_seconds" json:"poll_timeout_seconds"`
	RequestTimeoutSeconds int      `yaml:"request_timeout_seconds" json:"request_timeout_seconds"`
	MaxAttempts           int      `yaml:"max_attempts" json:"max_attempts"`
}

// ChannelGitHubConfig controls verified repository webhook fan-out.
type ChannelGitHubConfig struct {
	Enabled             bool                        `yaml:"enabled" json:"enabled"`
	WebhookSecret       string                      `yaml:"webhook_secret" json:"-"`
	AllowUnverified     bool                        `yaml:"allow_unverified" json:"allow_unverified"`
	DefaultMentionLogin string                      `yaml:"default_mention_login" json:"default_mention_login,omitempty"`
	MaxBodyBytes        int64                       `yaml:"max_body_bytes" json:"max_body_bytes"`
	Subscriptions       []ChannelGitHubSubscription `yaml:"subscriptions" json:"subscriptions"`
}

// ChannelGitHubSubscription binds repository events to one Gofer owner and assistant.
type ChannelGitHubSubscription struct {
	ID          string                                `yaml:"id" json:"id"`
	UserID      string                                `yaml:"user_id" json:"user_id"`
	Repository  string                                `yaml:"repository" json:"repository"`
	AssistantID string                                `yaml:"assistant_id" json:"assistant_id,omitempty"`
	BotLogin    string                                `yaml:"bot_login" json:"bot_login,omitempty"`
	Triggers    map[string]ChannelGitHubTriggerConfig `yaml:"triggers" json:"triggers"`
}

// ChannelGitHubTriggerConfig filters one explicitly subscribed GitHub event.
type ChannelGitHubTriggerConfig struct {
	Actions        []string `yaml:"actions" json:"actions,omitempty"`
	RequireMention *bool    `yaml:"require_mention" json:"require_mention,omitempty"`
	MentionLogin   string   `yaml:"mention_login" json:"mention_login,omitempty"`
	AllowAuthors   []string `yaml:"allow_authors" json:"allow_authors,omitempty"`
}

// ChannelBuzzConfig controls the signed Buzz/Nostr relay provider.
type ChannelBuzzConfig struct {
	Enabled               bool     `yaml:"enabled" json:"enabled"`
	RelayURL              string   `yaml:"relay_url" json:"relay_url"`
	PrivateKey            string   `yaml:"private_key" json:"-"`
	RelayPublicKey        string   `yaml:"relay_public_key" json:"relay_public_key,omitempty"`
	AllowedUsers          []string `yaml:"allowed_users" json:"allowed_users"`
	RequireMention        bool     `yaml:"require_mention" json:"require_mention"`
	MentionFreeChannels   []string `yaml:"mention_free_channels" json:"mention_free_channels"`
	RequestTimeoutSeconds int      `yaml:"request_timeout_seconds" json:"request_timeout_seconds"`
	MaxAttempts           int      `yaml:"max_attempts" json:"max_attempts"`
}

// TitleConfig controls automatic first-exchange conversation titles.
type TitleConfig struct {
	Enabled   bool   `yaml:"enabled" json:"enabled"`
	MaxWords  int    `yaml:"max_words" json:"max_words"`
	MaxChars  int    `yaml:"max_chars" json:"max_chars"`
	ModelName string `yaml:"model_name" json:"model_name,omitempty"`
}

// SuggestionsConfig controls follow-up question generation.
type SuggestionsConfig struct {
	Enabled        bool `yaml:"enabled" json:"enabled"`
	MaxSuggestions int  `yaml:"max_suggestions" json:"max_suggestions"`
}

// InputPolishConfig controls pre-send draft rewriting.
type InputPolishConfig struct {
	Enabled   bool   `yaml:"enabled" json:"enabled"`
	MaxChars  int    `yaml:"max_chars" json:"max_chars"`
	ModelName string `yaml:"model_name" json:"model_name,omitempty"`
}

// ModelConfig names one model exposed to agent runs.
type ModelConfig struct {
	Name      string         `yaml:"name" json:"name"`
	Provider  string         `yaml:"provider" json:"provider"`
	Model     string         `yaml:"model" json:"model"`
	APIKey    string         `yaml:"api_key" json:"-"`
	AuthToken string         `yaml:"auth_token" json:"-"`
	BaseURL   string         `yaml:"base_url" json:"base_url,omitempty"`
	MaxTokens int            `yaml:"max_tokens" json:"max_tokens,omitempty"`
	Options   map[string]any `yaml:"options" json:"options,omitempty"`
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
			MaxTurns:          100,
			MaxParallelTools:  8,
			MaxSubagents:      8,
			MaxSubagentDepth:  4,
			EventBuffer:       128,
			MaxContextTokens:  120_000,
			ReserveTokens:     8_000,
			MinRecentMessages: 8,
			MaxSummaryChars:   20_000,
		},
		LoopDetection: LoopDetectionConfig{
			Enabled: true, WarnThreshold: 3, HardLimit: 5, WindowSize: 20,
			ToolFrequencyWarn: 30, ToolFrequencyLimit: 50,
			ToolOverrides: make(map[string]ToolFrequencyOverride),
		},
		ReadBeforeWrite: ReadBeforeWriteConfig{Enabled: true},
		ToolOutput: ToolOutputConfig{
			Enabled: true, ExternalizeMinChars: 12_000,
			PreviewHeadChars: 2_000, PreviewTailChars: 1_000,
			FallbackMaxChars: 30_000, FallbackHeadChars: 8_000, FallbackTailChars: 3_000,
			StorageSubdir: ".tool-results", ExemptTools: []string{"read_file", "read_file_tool"},
			ToolOverrides: make(map[string]int),
		},
		Storage: StorageConfig{Driver: "sqlite", DSN: ".gofer/gofer.db"},
		Workspace: WorkspaceConfig{
			Root: ".gofer/workspaces", MaxReadBytes: 1 << 20,
			MaxWriteBytes: 80 << 10, MaxUploadBytes: 32 << 20,
		},
		Uploads: UploadsConfig{
			MaxFiles: 10, MaxTotalBytes: 100 << 20, ConverterCommand: []string{"markitdown", "{input}"},
			ConversionTimeoutSeconds: 120, MaxConvertedBytes: 1 << 20,
			MaxContextFiles: 10, MaxContextChars: 50_000, MaxOutlineEntries: 50, MaxPreviewLines: 5,
		},
		Sandbox: SandboxConfig{
			Driver: "local", DockerBinary: "docker", CommandTimeoutSeconds: 600,
			MaxTimeoutSeconds: 3600, MaxOutputBytes: 1 << 20, MaxScriptBytes: 64 << 10,
			Memory: "1g", CPUs: 2, PIDsLimit: 256,
		},
		Browser: BrowserConfig{
			MaxSessions: 32, IdleTimeoutSeconds: 1800, ActionTimeoutSeconds: 30,
			ViewportWidth: 1280, ViewportHeight: 720,
		},
		Web: WebConfig{
			Search: WebSearchConfig{Provider: "brave", MaxResults: 5, TimeoutSeconds: 15, SafeSearch: "moderate"},
			Fetch: WebFetchConfig{
				MaxResponseBytes: 2 << 20, MaxContentChars: 20_000, TimeoutSeconds: 20,
				MaxRedirects: 5, UserAgent: "Gofer/1.0 (+https://github.com/Rememorio/gofer)",
			},
		},
		Skills:    SkillsConfig{Root: "skills", ProjectionRoot: ".gofer/skills", MaxDocumentBytes: 1 << 20, MaxPackageBytes: 10 << 20},
		MCP:       MCPConfig{},
		Memory:    MemoryConfig{Enabled: true, Limit: 5, MaxChars: 8 << 10},
		Auth:      AuthConfig{},
		Scheduler: SchedulerConfig{PollIntervalSeconds: 5, LeaseDurationSeconds: 300, BatchSize: 32},
		Channels: ChannelsConfig{
			MaxInflight: 32, QueueCapacity: 128, DedupeTTLSeconds: 86400,
			UnauthorizedReply: "This channel identity is not connected to Gofer.",
			Webhook:           ChannelWebhookConfig{TimeoutSeconds: 10, MaxAttempts: 3, MaxBodyBytes: 1 << 20, ClockSkewSeconds: 300},
			Slack:             ChannelSlackConfig{RequestTimeoutSeconds: 20, MaxAttempts: 3},
			Telegram:          ChannelTelegramConfig{PollTimeoutSeconds: 30, RequestTimeoutSeconds: 45, MaxAttempts: 3},
			Discord:           ChannelDiscordConfig{RequestTimeoutSeconds: 20, MaxAttempts: 3},
			Feishu:            ChannelFeishuConfig{Domain: "https://open.feishu.cn", RequestTimeoutSeconds: 20, MaxAttempts: 3},
			DingTalk:          ChannelDingTalkConfig{RequestTimeoutSeconds: 30, MaxAttempts: 3},
			WeCom:             ChannelWeComConfig{WorkingMessage: "Working on it...", HeartbeatSeconds: 30, RequestTimeoutSeconds: 20, MaxAttempts: 3},
			WeChat:            ChannelWeChatConfig{ChannelVersion: "1.0", PollTimeoutSeconds: 35, RequestTimeoutSeconds: 45, MaxAttempts: 3},
			GitHub:            ChannelGitHubConfig{MaxBodyBytes: 1 << 20},
			Buzz:              ChannelBuzzConfig{RequireMention: true, RequestTimeoutSeconds: 20, MaxAttempts: 3},
		},
		Title: TitleConfig{Enabled: true, MaxWords: 6, MaxChars: 60},
		Suggestions: SuggestionsConfig{
			Enabled: true, MaxSuggestions: 3,
		},
		InputPolish: InputPolishConfig{Enabled: true, MaxChars: 4000},
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
	if err := validateToolOutput(config.ToolOutput); err != nil {
		return err
	}
	if err := validateLoopDetection(config.LoopDetection); err != nil {
		return err
	}
	if config.ReadBeforeWrite.Enabled && config.ToolOutput.Enabled && !contains(config.ToolOutput.ExemptTools, "read_file") {
		return fmt.Errorf("%w: read_file must be exempt from tool output budgeting when read_before_write is enabled", ErrInvalid)
	}
	if err := validateBrowser(config.Browser); err != nil {
		return err
	}
	if err := validateWeb(config.Web); err != nil {
		return err
	}
	if err := validateUploads(config.Uploads, config.Workspace); err != nil {
		return err
	}
	if err := validateAgentExtensions(config.Skills, config.MCP, config.Memory); err != nil {
		return err
	}
	if err := validateServices(config.Auth, config.Scheduler, config.Channels); err != nil {
		return err
	}
	if err := validateConversationServices(config.Title, config.Suggestions, config.InputPolish); err != nil {
		return err
	}
	if err := validateModels(config.Models); err != nil {
		return err
	}
	if err := validateGitHubAssistants(config.Channels.GitHub, config.Models); err != nil {
		return err
	}
	return validateServiceModelAliases(config)
}

func validateUploads(uploads UploadsConfig, workspace WorkspaceConfig) error {
	if !validUploadTransferLimits(uploads, workspace) || !validUploadConversionLimits(uploads, workspace) ||
		!validUploadContextLimits(uploads) {
		return fmt.Errorf("%w: invalid upload limits", ErrInvalid)
	}
	return validateUploadConverterCommand(uploads)
}

func validUploadTransferLimits(uploads UploadsConfig, workspace WorkspaceConfig) bool {
	return uploads.MaxFiles >= 1 && uploads.MaxFiles <= 100 &&
		uploads.MaxTotalBytes >= workspace.MaxUploadBytes && uploads.MaxTotalBytes <= 10<<30
}

func validUploadConversionLimits(uploads UploadsConfig, workspace WorkspaceConfig) bool {
	return uploads.ConversionTimeoutSeconds >= 1 && uploads.ConversionTimeoutSeconds <= 1800 &&
		uploads.MaxConvertedBytes >= 1024 && uploads.MaxConvertedBytes <= workspace.MaxReadBytes &&
		uploads.MaxConvertedBytes <= 100<<20
}

func validUploadContextLimits(uploads UploadsConfig) bool {
	return uploads.MaxContextFiles >= 1 && uploads.MaxContextFiles <= 100 &&
		uploads.MaxContextChars >= 1024 && uploads.MaxContextChars <= 1_000_000 &&
		uploads.MaxOutlineEntries >= 1 && uploads.MaxOutlineEntries <= 1000 &&
		uploads.MaxPreviewLines >= 1 && uploads.MaxPreviewLines <= 100
}

func validateUploadConverterCommand(uploads UploadsConfig) error {
	foundInput := false
	for _, argument := range uploads.ConverterCommand {
		if strings.TrimSpace(argument) == "" || strings.IndexByte(argument, 0) >= 0 {
			return fmt.Errorf("%w: invalid upload converter command", ErrInvalid)
		}
		foundInput = foundInput || strings.Contains(argument, "{input}")
	}
	if uploads.AutoConvertDocuments && (len(uploads.ConverterCommand) == 0 || !foundInput) {
		return fmt.Errorf("%w: upload converter command with {input} is required", ErrInvalid)
	}
	return nil
}

func validateConversationServices(title TitleConfig, suggestions SuggestionsConfig, polish InputPolishConfig) error {
	if title.MaxWords < 1 || title.MaxWords > 20 || title.MaxChars < 10 || title.MaxChars > 200 ||
		strings.TrimSpace(title.ModelName) != title.ModelName {
		return fmt.Errorf("%w: invalid title configuration", ErrInvalid)
	}
	if suggestions.MaxSuggestions < 1 || suggestions.MaxSuggestions > 5 {
		return fmt.Errorf("%w: invalid suggestions configuration", ErrInvalid)
	}
	if polish.MaxChars < 1 || polish.MaxChars > 100_000 || strings.TrimSpace(polish.ModelName) != polish.ModelName {
		return fmt.Errorf("%w: invalid input polish configuration", ErrInvalid)
	}
	return nil
}

func validateServiceModelAliases(config Config) error {
	aliases := map[string]string{"title.model_name": config.Title.ModelName, "input_polish.model_name": config.InputPolish.ModelName}
	known := make(map[string]struct{}, len(config.Models))
	for _, model := range config.Models {
		known[model.Name] = struct{}{}
	}
	for field, alias := range aliases {
		if alias == "" {
			continue
		}
		if _, exists := known[alias]; !exists {
			return fmt.Errorf("%w: %s references unknown model %q", ErrInvalid, field, alias)
		}
	}
	return nil
}

func validateLoopDetection(config LoopDetectionConfig) error {
	if config.WarnThreshold < 1 || config.HardLimit < config.WarnThreshold || config.WindowSize < config.HardLimit || config.WindowSize > 10_000 {
		return fmt.Errorf("%w: invalid loop detection repetition limits", ErrInvalid)
	}
	if config.ToolFrequencyWarn < 1 || config.ToolFrequencyLimit < config.ToolFrequencyWarn || config.ToolFrequencyLimit > 100_000 {
		return fmt.Errorf("%w: invalid loop detection frequency limits", ErrInvalid)
	}
	for name, override := range config.ToolOverrides {
		if strings.TrimSpace(name) != name || name == "" || override.Warn < 1 || override.HardLimit < override.Warn || override.HardLimit > 100_000 {
			return fmt.Errorf("%w: invalid loop detection override %q", ErrInvalid, name)
		}
	}
	return nil
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
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
	return validateChannels(channels)
}

func validateChannels(channels ChannelsConfig) error {
	if channels.MaxInflight < 1 || channels.MaxInflight > 10_000 || channels.QueueCapacity < channels.MaxInflight || channels.QueueCapacity > 100_000 || channels.DedupeTTLSeconds < 60 || len(channels.UnauthorizedReply) > 2000 {
		return fmt.Errorf("%w: invalid channel limits", ErrInvalid)
	}
	if err := validateChannelWebhook(channels.Enabled, channels.Webhook); err != nil {
		return err
	}
	if err := validateNativeChannels(channels); err != nil {
		return err
	}
	return validateChannelBindings(channels.Bindings)
}

func validateNativeChannels(channels ChannelsConfig) error {
	if err := validateSlackChannel(channels.Enabled, channels.Slack); err != nil {
		return err
	}
	if err := validateTelegramChannel(channels.Enabled, channels.Telegram); err != nil {
		return err
	}
	if err := validateDiscordChannel(channels.Enabled, channels.Discord); err != nil {
		return err
	}
	if err := validateFeishuChannel(channels.Enabled, channels.Feishu); err != nil {
		return err
	}
	if err := validateDingTalkChannel(channels.Enabled, channels.DingTalk); err != nil {
		return err
	}
	if err := validateWeComChannel(channels.Enabled, channels.WeCom); err != nil {
		return err
	}
	if err := validateWeChatChannel(channels.Enabled, channels.WeChat); err != nil {
		return err
	}
	if err := validateGitHubChannel(channels.Enabled, channels.GitHub); err != nil {
		return err
	}
	return validateBuzzChannel(channels.Enabled, channels.Buzz)
}

func validateSlackChannel(channelsEnabled bool, channel ChannelSlackConfig) error {
	if !channel.Enabled {
		return nil
	}
	if !channelsEnabled || strings.TrimSpace(channel.BotToken) == "" || strings.TrimSpace(channel.AppToken) == "" ||
		channel.RequestTimeoutSeconds < 1 || channel.RequestTimeoutSeconds > 120 || channel.MaxAttempts < 1 || channel.MaxAttempts > 5 ||
		!validChannelIDs(channel.AllowedUsers) || len(channel.BotUserID) > 256 {
		return fmt.Errorf("%w: invalid Slack channel configuration", ErrInvalid)
	}
	return nil
}

func validateTelegramChannel(channelsEnabled bool, channel ChannelTelegramConfig) error {
	if !channel.Enabled {
		return nil
	}
	if !channelsEnabled || strings.TrimSpace(channel.BotToken) == "" || channel.PollTimeoutSeconds < 1 || channel.PollTimeoutSeconds > 50 ||
		channel.RequestTimeoutSeconds <= channel.PollTimeoutSeconds || channel.RequestTimeoutSeconds > 120 ||
		channel.MaxAttempts < 1 || channel.MaxAttempts > 5 || !validChannelIDs(channel.AllowedUsers) {
		return fmt.Errorf("%w: invalid Telegram channel configuration", ErrInvalid)
	}
	return nil
}

func validateDiscordChannel(channelsEnabled bool, channel ChannelDiscordConfig) error {
	if !channel.Enabled {
		return nil
	}
	if !channelsEnabled || strings.TrimSpace(channel.BotToken) == "" || channel.RequestTimeoutSeconds < 1 || channel.RequestTimeoutSeconds > 120 ||
		channel.MaxAttempts < 1 || channel.MaxAttempts > 5 || !validChannelIDs(channel.AllowedGuilds) || !validChannelIDs(channel.AllowedChannels) {
		return fmt.Errorf("%w: invalid Discord channel configuration", ErrInvalid)
	}
	return nil
}

func validateFeishuChannel(channelsEnabled bool, channel ChannelFeishuConfig) error {
	if !channel.Enabled {
		return nil
	}
	if !channelsEnabled || strings.TrimSpace(channel.AppID) == "" || strings.TrimSpace(channel.AppSecret) == "" ||
		channel.Domain != "https://open.feishu.cn" && channel.Domain != "https://open.larksuite.com" ||
		channel.RequestTimeoutSeconds < 1 || channel.RequestTimeoutSeconds > 120 || channel.MaxAttempts < 1 || channel.MaxAttempts > 5 ||
		!validChannelIDs(channel.AllowedUsers) {
		return fmt.Errorf("%w: invalid Feishu channel configuration", ErrInvalid)
	}
	return nil
}

func validateDingTalkChannel(channelsEnabled bool, channel ChannelDingTalkConfig) error {
	if !channel.Enabled {
		return nil
	}
	if !channelsEnabled || strings.TrimSpace(channel.ClientID) == "" || strings.TrimSpace(channel.ClientSecret) == "" ||
		channel.RequestTimeoutSeconds < 1 || channel.RequestTimeoutSeconds > 120 || channel.MaxAttempts < 1 || channel.MaxAttempts > 5 ||
		!validChannelIDs(channel.AllowedUsers) {
		return fmt.Errorf("%w: invalid DingTalk channel configuration", ErrInvalid)
	}
	return nil
}

func validateWeComChannel(channelsEnabled bool, channel ChannelWeComConfig) error {
	if !channel.Enabled {
		return nil
	}
	if !channelsEnabled || strings.TrimSpace(channel.BotID) == "" || strings.TrimSpace(channel.BotSecret) == "" ||
		len(channel.WorkingMessage) > 2000 || channel.HeartbeatSeconds < 5 || channel.HeartbeatSeconds > 120 ||
		channel.RequestTimeoutSeconds < 1 || channel.RequestTimeoutSeconds > 120 || channel.MaxAttempts < 1 || channel.MaxAttempts > 5 ||
		!validChannelIDs(channel.AllowedUsers) {
		return fmt.Errorf("%w: invalid WeCom channel configuration", ErrInvalid)
	}
	return nil
}

func validateWeChatChannel(channelsEnabled bool, channel ChannelWeChatConfig) error {
	if !channel.Enabled {
		return nil
	}
	if !channelsEnabled || strings.TrimSpace(channel.BotToken) == "" || len(channel.ILinkBotID) > 512 || len(channel.ILinkAppID) > 512 || len(channel.RouteTag) > 256 ||
		!validWeChatVersion(channel.ChannelVersion) || channel.PollTimeoutSeconds < 1 || channel.PollTimeoutSeconds > 60 ||
		channel.RequestTimeoutSeconds <= channel.PollTimeoutSeconds || channel.RequestTimeoutSeconds > 120 ||
		channel.MaxAttempts < 1 || channel.MaxAttempts > 5 || !validChannelIDs(channel.AllowedUsers) {
		return fmt.Errorf("%w: invalid WeChat channel configuration", ErrInvalid)
	}
	return nil
}

func validWeChatVersion(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) < 1 || len(parts) > 3 {
		return false
	}
	for _, part := range parts {
		parsed, err := strconv.Atoi(part)
		if err != nil || parsed < 0 || parsed > 255 {
			return false
		}
	}
	return true
}

func validateGitHubChannel(channelsEnabled bool, github ChannelGitHubConfig) error {
	if !github.Enabled {
		return nil
	}
	if !channelsEnabled || !github.AllowUnverified && len(github.WebhookSecret) < 24 ||
		strings.TrimSpace(github.WebhookSecret) != github.WebhookSecret || github.MaxBodyBytes < 1024 || github.MaxBodyBytes > 16<<20 ||
		!validGitHubLogin(github.DefaultMentionLogin, true) || len(github.Subscriptions) == 0 {
		return fmt.Errorf("%w: invalid GitHub channel configuration", ErrInvalid)
	}
	seen := make(map[string]struct{}, len(github.Subscriptions))
	for _, subscription := range github.Subscriptions {
		if err := validateGitHubSubscription(subscription, seen); err != nil {
			return err
		}
	}
	return nil
}

func validateBuzzChannel(channelsEnabled bool, buzz ChannelBuzzConfig) error {
	if !buzz.Enabled {
		return nil
	}
	parsed, err := url.Parse(strings.TrimSpace(buzz.RelayURL))
	if !channelsEnabled || err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" ||
		parsed.Scheme != "ws" && parsed.Scheme != "wss" || !validNostrKeyText(buzz.PrivateKey, false) ||
		!validNostrKeyText(buzz.RelayPublicKey, true) || !validNostrKeyList(buzz.AllowedUsers) ||
		!validChannelIDs(buzz.MentionFreeChannels) || buzz.RequestTimeoutSeconds < 1 || buzz.RequestTimeoutSeconds > 120 ||
		buzz.MaxAttempts < 1 || buzz.MaxAttempts > 5 {
		return fmt.Errorf("%w: invalid Buzz channel configuration", ErrInvalid)
	}
	return nil
}

func validNostrKeyList(values []string) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !validNostrKeyText(value, false) {
			return false
		}
		normalized := strings.ToLower(value)
		if _, duplicate := seen[normalized]; duplicate {
			return false
		}
		seen[normalized] = struct{}{}
	}
	return true
}

func validNostrKeyText(value string, allowEmpty bool) bool {
	if value == "" {
		return allowEmpty
	}
	if strings.TrimSpace(value) != value || len(value) < 32 || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if character > 127 || character <= 32 {
			return false
		}
	}
	return true
}

func validateGitHubSubscription(subscription ChannelGitHubSubscription, seen map[string]struct{}) error {
	identifier := strings.TrimSpace(subscription.ID)
	if identifier == "" || identifier != subscription.ID || len(identifier) > 128 || strings.TrimSpace(subscription.UserID) == "" ||
		strings.TrimSpace(subscription.UserID) != subscription.UserID || len(subscription.UserID) > 128 ||
		!validGitHubRepository(subscription.Repository) || len(subscription.AssistantID) > 128 || strings.TrimSpace(subscription.AssistantID) != subscription.AssistantID ||
		!validGitHubLogin(subscription.BotLogin, true) || len(subscription.Triggers) == 0 {
		return fmt.Errorf("%w: invalid GitHub subscription", ErrInvalid)
	}
	if _, duplicate := seen[strings.ToLower(identifier)]; duplicate {
		return fmt.Errorf("%w: duplicate GitHub subscription", ErrInvalid)
	}
	seen[strings.ToLower(identifier)] = struct{}{}
	for event, trigger := range subscription.Triggers {
		if !oneOf(event, "ping", "issues", "issue_comment", "pull_request", "pull_request_review", "pull_request_review_comment") ||
			!validGitHubTrigger(trigger) {
			return fmt.Errorf("%w: invalid GitHub trigger", ErrInvalid)
		}
	}
	return nil
}

func validGitHubTrigger(trigger ChannelGitHubTriggerConfig) bool {
	if !validGitHubLogin(trigger.MentionLogin, true) || !validGitHubLogins(trigger.AllowAuthors) {
		return false
	}
	seen := make(map[string]struct{}, len(trigger.Actions))
	for _, action := range trigger.Actions {
		if action == "" || strings.TrimSpace(action) != action || len(action) > 64 {
			return false
		}
		if _, duplicate := seen[action]; duplicate {
			return false
		}
		seen[action] = struct{}{}
	}
	return true
}

func validGitHubRepository(repository string) bool {
	if repository == "" || strings.TrimSpace(repository) != repository || len(repository) > 200 {
		return false
	}
	parts := strings.Split(repository, "/")
	return len(parts) == 2 && validGitHubRepositoryPart(parts[0]) && validGitHubRepositoryPart(parts[1])
}

func validGitHubRepositoryPart(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	for _, character := range value {
		if !asciiLetterOrDigit(character) && character != '-' && character != '_' && character != '.' {
			return false
		}
	}
	return true
}

func validGitHubLogins(values []string) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !validGitHubLogin(value, false) {
			return false
		}
		normalized := strings.ToLower(value)
		if _, duplicate := seen[normalized]; duplicate {
			return false
		}
		seen[normalized] = struct{}{}
	}
	return true
}

func validGitHubLogin(value string, allowEmpty bool) bool {
	if value == "" {
		return allowEmpty
	}
	if strings.TrimSpace(value) != value || len(value) > 39 || value[0] == '-' || value[len(value)-1] == '-' || strings.Contains(value, "--") {
		return false
	}
	for _, character := range value {
		if !asciiLetterOrDigit(character) && character != '-' {
			return false
		}
	}
	return true
}

func asciiLetterOrDigit(character rune) bool {
	return character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9'
}

func validChannelIDs(values []string) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != value || value == "" || len(value) > 256 {
			return false
		}
		if _, exists := seen[value]; exists {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func validateChannelWebhook(channelsEnabled bool, webhook ChannelWebhookConfig) error {
	if !webhook.Enabled {
		return nil
	}
	if !channelsEnabled || len(strings.TrimSpace(webhook.Secret)) < 24 || strings.TrimSpace(webhook.OutboundURL) == "" || webhook.TimeoutSeconds < 1 || webhook.TimeoutSeconds > 120 || webhook.MaxAttempts < 1 || webhook.MaxAttempts > 5 || webhook.MaxBodyBytes < 1024 || webhook.MaxBodyBytes > 16<<20 || webhook.ClockSkewSeconds < 60 || webhook.ClockSkewSeconds > 3600 {
		return fmt.Errorf("%w: invalid channel webhook configuration", ErrInvalid)
	}
	parsed, err := url.Parse(strings.TrimSpace(webhook.OutboundURL))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("%w: invalid channel webhook outbound_url", ErrInvalid)
	}
	return nil
}

func validateChannelBindings(bindings []ChannelBindingConfig) error {
	identities := make(map[string]struct{}, len(bindings))
	for _, binding := range bindings {
		provider := strings.ToLower(strings.TrimSpace(binding.Provider))
		key := provider + "\xff" + strings.TrimSpace(binding.WorkspaceID) + "\xff" + strings.TrimSpace(binding.ExternalUserID)
		if strings.TrimSpace(binding.UserID) == "" || len(binding.UserID) > 128 || !validChannelProvider(provider) || provider != binding.Provider ||
			strings.TrimSpace(binding.WorkspaceID) != binding.WorkspaceID || len(binding.WorkspaceID) > 256 || strings.TrimSpace(binding.ExternalUserID) == "" || len(binding.ExternalUserID) > 256 ||
			len(binding.WorkspaceName) > 512 || len(binding.ExternalUserName) > 512 {
			return fmt.Errorf("%w: invalid channel binding", ErrInvalid)
		}
		if _, duplicate := identities[key]; duplicate {
			return fmt.Errorf("%w: duplicate channel binding", ErrInvalid)
		}
		identities[key] = struct{}{}
	}
	return nil
}

func validChannelProvider(value string) bool {
	if value == "" || len(value) > 32 {
		return false
	}
	for _, character := range value {
		if character < 'a' || character > 'z' {
			if character < '0' || character > '9' {
				if character != '-' && character != '_' {
					return false
				}
			}
		}
	}
	return true
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
	if err := validateRuntime(config.Runtime); err != nil {
		return err
	}
	if !oneOf(config.Storage.Driver, "memory", "sqlite", "postgres") {
		return fmt.Errorf("%w: unsupported storage.driver %q", ErrInvalid, config.Storage.Driver)
	}
	if config.Storage.Driver != "memory" && strings.TrimSpace(config.Storage.DSN) == "" {
		return fmt.Errorf("%w: storage.dsn is required for %s", ErrInvalid, config.Storage.Driver)
	}
	if strings.TrimSpace(config.Workspace.Root) == "" || config.Workspace.MaxReadBytes <= 0 ||
		config.Workspace.MaxWriteBytes <= 0 || config.Workspace.MaxUploadBytes <= 0 {
		return fmt.Errorf("%w: workspace root and size limits are required", ErrInvalid)
	}
	return nil
}

func validateRuntime(runtime RuntimeConfig) error {
	if runtime.MaxTurns <= 0 || runtime.MaxParallelTools <= 0 ||
		runtime.MaxSubagents <= 0 || runtime.MaxSubagentDepth <= 0 ||
		runtime.EventBuffer <= 0 || runtime.MaxContextTokens <= 0 ||
		runtime.ReserveTokens < 0 || runtime.ReserveTokens >= runtime.MaxContextTokens ||
		runtime.MinRecentMessages <= 0 || runtime.MaxSummaryChars <= 0 {
		return fmt.Errorf("%w: runtime limits must be positive", ErrInvalid)
	}
	return nil
}

func validateToolOutput(output ToolOutputConfig) error {
	if output.ExternalizeMinChars < 0 || output.PreviewHeadChars < 0 || output.PreviewTailChars < 0 ||
		output.FallbackMaxChars < 0 || output.FallbackHeadChars < 0 || output.FallbackTailChars < 0 {
		return fmt.Errorf("%w: tool output limits cannot be negative", ErrInvalid)
	}
	if !singlePathSegment(output.StorageSubdir) {
		return fmt.Errorf("%w: tool_output.storage_subdir must be one directory name", ErrInvalid)
	}
	seen := make(map[string]struct{}, len(output.ExemptTools))
	for _, name := range output.ExemptTools {
		if strings.TrimSpace(name) != name || name == "" {
			return fmt.Errorf("%w: tool output exempt tool names cannot be empty", ErrInvalid)
		}
		if _, exists := seen[name]; exists {
			return fmt.Errorf("%w: duplicate tool output exemption %q", ErrInvalid, name)
		}
		seen[name] = struct{}{}
	}
	for name, threshold := range output.ToolOverrides {
		if strings.TrimSpace(name) != name || name == "" || threshold < 0 {
			return fmt.Errorf("%w: invalid tool output override", ErrInvalid)
		}
	}
	return nil
}

func singlePathSegment(value string) bool {
	return strings.TrimSpace(value) == value && value != "" && value != "." && value != ".." && !strings.ContainsAny(value, "/\\\x00")
}

func validateAgentExtensions(skills SkillsConfig, mcp MCPConfig, memory MemoryConfig) error {
	if skills.MaxDocumentBytes <= 0 || skills.MaxPackageBytes <= 0 ||
		(skills.Enabled && (strings.TrimSpace(skills.Root) == "" || strings.TrimSpace(skills.ProjectionRoot) == "")) {
		return fmt.Errorf("%w: invalid skills configuration", ErrInvalid)
	}
	if mcp.Enabled && len(mcp.Servers) == 0 {
		return fmt.Errorf("%w: MCP servers are required when enabled", ErrInvalid)
	}
	names := make(map[string]struct{}, len(mcp.Servers))
	for _, server := range mcp.Servers {
		if strings.TrimSpace(server.Name) == "" || !oneOf(server.Transport, "stdio", "streamable_http") || server.MaxRetries < 0 {
			return fmt.Errorf("%w: invalid MCP server", ErrInvalid)
		}
		if server.Transport == "stdio" && strings.TrimSpace(server.Command) == "" {
			return fmt.Errorf("%w: MCP stdio command is required", ErrInvalid)
		}
		if server.Transport == "streamable_http" && strings.TrimSpace(server.URL) == "" {
			return fmt.Errorf("%w: MCP HTTP URL is required", ErrInvalid)
		}
		if _, exists := names[server.Name]; exists {
			return fmt.Errorf("%w: duplicate MCP server", ErrInvalid)
		}
		names[server.Name] = struct{}{}
	}
	if memory.Limit < 1 || memory.Limit > 100 || memory.MaxChars < 128 {
		return fmt.Errorf("%w: invalid memory limits", ErrInvalid)
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

func validateWeb(web WebConfig) error {
	if err := validateWebSearch(web.Search); err != nil {
		return err
	}
	return validateWebFetch(web.Fetch)
}

func validateWebSearch(search WebSearchConfig) error {
	if !oneOf(search.Provider, "brave", "searxng") || search.MaxResults < 1 || search.MaxResults > 20 ||
		search.TimeoutSeconds < 1 || search.TimeoutSeconds > 120 || !oneOf(search.SafeSearch, "off", "moderate", "strict") {
		return fmt.Errorf("%w: invalid web search provider or limits", ErrInvalid)
	}
	if search.APIKey != strings.TrimSpace(search.APIKey) {
		return fmt.Errorf("%w: web search API key has surrounding whitespace", ErrInvalid)
	}
	if search.Endpoint != "" {
		if err := validateHTTPURL(search.Endpoint); err != nil {
			return fmt.Errorf("%w: web search endpoint: %w", ErrInvalid, err)
		}
	}
	if search.Enabled && search.Provider == "brave" && search.APIKey == "" {
		return fmt.Errorf("%w: Brave API key is required when web search is enabled", ErrInvalid)
	}
	if search.Enabled && search.Provider == "searxng" && search.Endpoint == "" {
		return fmt.Errorf("%w: SearXNG endpoint is required when web search is enabled", ErrInvalid)
	}
	return nil
}

func validateWebFetch(fetch WebFetchConfig) error {
	if fetch.MaxResponseBytes < 1024 || fetch.MaxResponseBytes > 100<<20 ||
		fetch.MaxContentChars < 128 || fetch.MaxContentChars > 2_000_000 ||
		fetch.TimeoutSeconds < 1 || fetch.TimeoutSeconds > 120 || fetch.MaxRedirects < 0 || fetch.MaxRedirects > 20 ||
		strings.TrimSpace(fetch.UserAgent) != fetch.UserAgent || fetch.UserAgent == "" || strings.ContainsAny(fetch.UserAgent, "\r\n") {
		return fmt.Errorf("%w: invalid web fetch limits or user agent", ErrInvalid)
	}
	return nil
}

func validateHTTPURL(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" || parsed.User != nil ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return errors.New("absolute HTTP(S) URL without credentials is required")
	}
	return nil
}

func validateModels(models []ModelConfig) error {
	names := make(map[string]struct{}, len(models))
	for index, model := range models {
		if strings.TrimSpace(model.Name) == "" || strings.TrimSpace(model.Provider) == "" || strings.TrimSpace(model.Model) == "" {
			return fmt.Errorf("%w: models[%d] requires name, provider, and model", ErrInvalid, index)
		}
		if strings.TrimSpace(model.Name) != model.Name || strings.TrimSpace(model.Provider) != model.Provider ||
			strings.TrimSpace(model.Model) != model.Model || !oneOf(model.Provider, "openai", "anthropic") || model.MaxTokens < 0 {
			return fmt.Errorf("%w: invalid models[%d] identity, provider, or token limit", ErrInvalid, index)
		}
		if model.Provider == "openai" && model.AuthToken != "" {
			return fmt.Errorf("%w: models[%d] uses Anthropic-only configuration", ErrInvalid, index)
		}
		if model.APIKey != "" && model.AuthToken != "" {
			return fmt.Errorf("%w: models[%d] credentials are mutually exclusive", ErrInvalid, index)
		}
		if _, duplicate := names[model.Name]; duplicate {
			return fmt.Errorf("%w: duplicate model name %q", ErrInvalid, model.Name)
		}
		names[model.Name] = struct{}{}
	}
	return nil
}

func validateGitHubAssistants(github ChannelGitHubConfig, models []ModelConfig) error {
	if !github.Enabled {
		return nil
	}
	aliases := map[string]struct{}{"lead_agent": {}}
	for _, model := range models {
		aliases[model.Name] = struct{}{}
	}
	for _, subscription := range github.Subscriptions {
		if subscription.AssistantID == "" {
			continue
		}
		if _, exists := aliases[subscription.AssistantID]; !exists {
			return fmt.Errorf("%w: unknown GitHub assistant %q", ErrInvalid, subscription.AssistantID)
		}
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
