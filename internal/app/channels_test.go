package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/auth"
	"github.com/Rememorio/gofer/internal/channel"
	"github.com/Rememorio/gofer/internal/config"
	"github.com/Rememorio/gofer/internal/control"
	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/humaninput"
	"github.com/Rememorio/gofer/internal/memory"
	"github.com/Rememorio/gofer/internal/store"
)

const (
	appWebhookSecret   = "app-webhook-secret-at-least-24-bytes"
	buzzTestPrivateKey = "0000000000000000000000000000000000000000000000000000000000000001"
)

type appChannelDispatcherFunc func(context.Context, channel.Request) (channel.Reply, error)

func (function appChannelDispatcherFunc) Dispatch(ctx context.Context, request channel.Request) (channel.Reply, error) {
	return function(ctx, request)
}

type appChannelSender struct{ name string }

func (sender appChannelSender) Name() string                       { return sender.name }
func (appChannelSender) Send(context.Context, channel.Reply) error { return nil }
func (appChannelSender) Close() error                              { return nil }

func TestOpenNativeChannelsRegistersConfiguredProviders(t *testing.T) {
	t.Parallel()
	cfg := config.Defaults()
	cfg.Channels.Enabled = true
	cfg.Channels.Slack = config.ChannelSlackConfig{Enabled: true, BotToken: "bot", AppToken: "app", RequestTimeoutSeconds: 20, MaxAttempts: 3}
	cfg.Channels.Telegram = config.ChannelTelegramConfig{Enabled: true, BotToken: "bot", PollTimeoutSeconds: 30, RequestTimeoutSeconds: 45, MaxAttempts: 3}
	cfg.Channels.Discord = config.ChannelDiscordConfig{Enabled: true, BotToken: "bot", RequestTimeoutSeconds: 20, MaxAttempts: 3}
	cfg.Channels.Feishu = config.ChannelFeishuConfig{Enabled: true, AppID: "app", AppSecret: "secret", Domain: "https://open.feishu.cn", RequestTimeoutSeconds: 20, MaxAttempts: 3}
	cfg.Channels.DingTalk = config.ChannelDingTalkConfig{Enabled: true, ClientID: "client", ClientSecret: "secret", RequestTimeoutSeconds: 30, MaxAttempts: 3}
	cfg.Channels.WeCom = config.ChannelWeComConfig{Enabled: true, BotID: "bot", BotSecret: "secret", WorkingMessage: "Working", HeartbeatSeconds: 30, RequestTimeoutSeconds: 20, MaxAttempts: 3}
	cfg.Channels.WeChat = config.ChannelWeChatConfig{Enabled: true, BotToken: "token", ChannelVersion: "1.0", PollTimeoutSeconds: 35, RequestTimeoutSeconds: 45, MaxAttempts: 3}
	cfg.Channels.Buzz = config.ChannelBuzzConfig{Enabled: true, RelayURL: "ws://relay.test", PrivateKey: buzzTestPrivateKey, RequireMention: true, RequestTimeoutSeconds: 20, MaxAttempts: 3}
	state := channel.NewMemoryState()
	manager, err := channel.NewManager(channel.Config{
		Resolver: state, Dispatcher: appChannelDispatcherFunc(func(context.Context, channel.Request) (channel.Reply, error) { return channel.Reply{}, nil }),
		Dedupe: state, MaxInflight: 1, DedupeTTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{config: cfg}
	if err = service.openNativeChannels(manager); err != nil {
		t.Fatal(err)
	}
	if providers := manager.Providers(); fmt.Sprint(providers) != "[buzz dingtalk discord feishu slack telegram wechat wecom]" {
		t.Fatalf("providers = %v", providers)
	}
	if err = manager.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestGitHubWebhookIsPublicVerifiedAndDispatchesConfiguredAssistant(t *testing.T) {
	t.Parallel()
	requests := make(chan []byte, 1)
	modelServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		requests <- body
		writer.Header().Set("Content-Type", "text/event-stream")
		writeSSE(writer, textChunk("GitHub run recorded"), doneChunk("stop"))
	}))
	defer modelServer.Close()
	cfg := testConfig(t, modelServer.URL+"/v1")
	cfg.Channels.Enabled = true
	cfg.Channels.GitHub = config.ChannelGitHubConfig{
		Enabled: true, WebhookSecret: appWebhookSecret, DefaultMentionLogin: "gofer-bot", MaxBodyBytes: 1 << 20,
		Subscriptions: []config.ChannelGitHubSubscription{{
			ID: "maintainer", UserID: "local", Repository: "Rememorio/gofer", AssistantID: "primary",
			Triggers: map[string]config.ChannelGitHubTriggerConfig{"issues": {}},
		}},
	}
	cfg.Auth = config.AuthConfig{Enabled: true, Tokens: []config.AuthTokenConfig{{
		Secret: "operator-token-at-least-24-bytes", PrincipalID: "operator", Permissions: []string{string(auth.Admin)},
	}}}
	service, err := New(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = service.Close() }()
	server := httptest.NewServer(service.Handler())
	defer server.Close()
	body := []byte(`{"action":"opened","repository":{"full_name":"Rememorio/gofer"},"issue":{"number":9,"title":"Add feature","body":"Please implement it","user":{"login":"alice"}},"sender":{"login":"alice"}}`)
	response := postAppGitHubWebhook(t, server.URL, body, "issues", "delivery-9", true)
	if response.StatusCode != http.StatusAccepted {
		payload, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		t.Fatalf("GitHub webhook = %d: %s", response.StatusCode, payload)
	}
	_ = response.Body.Close()
	select {
	case modelRequest := <-requests:
		if !bytes.Contains(modelRequest, []byte("Add feature")) || !bytes.Contains(modelRequest, []byte("gh issue comment")) {
			t.Fatalf("model request = %s", modelRequest)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("GitHub run did not reach the configured assistant")
	}
	invalid := postAppGitHubWebhook(t, server.URL, body, "issues", "delivery-10", false)
	defer func() { _ = invalid.Body.Close() }()
	if invalid.StatusCode != http.StatusUnauthorized {
		t.Fatalf("invalid signature status = %d", invalid.StatusCode)
	}
}

func TestChannelWebhookPersistsConversationAcrossServiceRestart(t *testing.T) {
	var calls atomic.Int32
	var requestMu sync.Mutex
	modelRequests := make([][]byte, 0, 2)
	modelServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		requestMu.Lock()
		modelRequests = append(modelRequests, append([]byte(nil), body...))
		requestMu.Unlock()
		turn := calls.Add(1)
		writer.Header().Set("Content-Type", "text/event-stream")
		writeSSE(writer, textChunk(fmt.Sprintf("channel answer %d", turn)), doneChunk("stop"))
	}))
	defer modelServer.Close()
	replies := make(chan channel.Reply, 3)
	callback := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var reply channel.Reply
		if err := json.NewDecoder(request.Body).Decode(&reply); err != nil {
			t.Errorf("decode callback: %v", err)
		}
		replies <- reply
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer callback.Close()

	cfg := channelTestConfig(t, modelServer.URL+"/v1", callback.URL)
	cfg.Storage = config.StorageConfig{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "gofer.db")}
	service, err := New(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(service.Handler())
	postChannelEvent(t, server.URL, "event-1", "hello one")
	assertChannelReply(t, waitChannelReply(t, replies), "event-1", "channel answer 1")
	postChannelEvent(t, server.URL, "event-1", "hello one")
	select {
	case duplicate := <-replies:
		t.Fatalf("duplicate reply = %#v", duplicate)
	case <-time.After(150 * time.Millisecond):
	}
	server.Close()
	if err = service.Close(); err != nil {
		t.Fatal(err)
	}

	service, err = New(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = service.Close() }()
	server = httptest.NewServer(service.Handler())
	defer server.Close()
	postChannelEvent(t, server.URL, "event-2", "hello two")
	assertChannelReply(t, waitChannelReply(t, replies), "event-2", "channel answer 2")
	requestMu.Lock()
	secondModelRequest := append([]byte(nil), modelRequests[1]...)
	requestMu.Unlock()
	assertChannelModelHistory(t, secondModelRequest)
	assertChannelResources(t, server.URL)
}

func TestChannelWebhookCommandsAndAgentRouting(t *testing.T) {
	t.Parallel()
	var modelCalls atomic.Int32
	modelBodies := make(chan []byte, 3)
	modelServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		modelBodies <- body
		modelCalls.Add(1)
		writer.Header().Set("Content-Type", "text/event-stream")
		writeSSE(writer, textChunk("agent reply"), doneChunk("stop"))
	}))
	defer modelServer.Close()
	replies := make(chan channel.Reply, 12)
	callback := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var reply channel.Reply
		_ = json.NewDecoder(request.Body).Decode(&reply)
		replies <- reply
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer callback.Close()
	cfg := channelTestConfig(t, modelServer.URL+"/v1", callback.URL)
	installChannelTestSkill(t, &cfg)
	service, err := New(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = service.Close() }()
	server := httptest.NewServer(service.Handler())
	defer server.Close()

	assertCommand := func(identifier, text, want string) {
		t.Helper()
		postChannelEvent(t, server.URL, identifier, text)
		reply := waitChannelReply(t, replies)
		if !strings.Contains(reply.Text, want) {
			t.Fatalf("command %q reply = %q, want %q", text, reply.Text, want)
		}
	}
	assertCommand("command-1", "/status", "No active conversation.")
	assertCommand("command-2", "/new", "New conversation started.")
	assertCommand("command-3", "/models", "primary")
	assertCommand("command-4", "/memory", "0 fact(s)")
	assertCommand("command-5", "/goal Ship the release", "agent reply")
	if body := <-modelBodies; !bytes.Contains(body, []byte("Ship the release")) {
		t.Fatalf("goal model request = %s", body)
	}
	assertCommand("command-6", "/goal", "Goal: Ship the release")
	assertCommand("command-7", "/goal off", "Goal cleared.")
	assertCommand("command-8", "/bootstrap Configure profile", "agent reply")
	if body := <-modelBodies; !bytes.Contains(body, []byte("Configure profile")) ||
		!bytes.Contains(body, []byte(channelBootstrapPrompt)) {
		t.Fatalf("bootstrap model request = %s", body)
	}
	assertCommand("command-9", "/help", "/<skill-name>")
	assertCommand("command-10", "/does-not-exist", "Unknown command")
	assertCommand("command-11", "/new", "New conversation started.")
	assertCommand("command-12", "/release-notes Summarize changes", "agent reply")
	if body := <-modelBodies; !bytes.Contains(body, []byte("CHANNEL_SKILL_MARKER")) ||
		!bytes.Contains(body, []byte("Summarize changes")) {
		t.Fatalf("slash skill model request = %s", body)
	}
	if modelCalls.Load() != 3 {
		t.Fatalf("model calls = %d", modelCalls.Load())
	}
}

func TestChannelRunContext(t *testing.T) {
	t.Parallel()
	plain := context.Background()
	if channelBootstrap(plain) || channelUser(plain) != "" {
		t.Fatal("plain context contains channel state")
	}
	if environment, err := channelCommandEnvironment(plain); err != nil || environment != nil {
		t.Fatalf("plain environment = %#v, %v", environment, err)
	}

	source := context.WithValue(plain, channelBootstrapContextKey, true)
	source = context.WithValue(source, channelUserContextKey, "platform-user")
	source = context.WithValue(source, channelSkillContextKey, channelSkillActivation{Name: "report", Instructions: "Write clearly."})
	inherited := inheritChannelContext(context.Background(), source)
	activation, activated := channelSkill(inherited)
	if !channelBootstrap(inherited) || channelUser(inherited) != "platform-user" || !activated || activation.Name != "report" {
		t.Fatal("channel context was not inherited")
	}
	environment, err := channelCommandEnvironment(inherited)
	if err != nil {
		t.Fatal(err)
	}
	if environment["DEERFLOW_CHANNEL_USER_ID"] != "platform-user" || environment["GOFER_CHANNEL_USER_ID"] != "platform-user" {
		t.Fatalf("channel environment = %#v", environment)
	}
}

func installChannelTestSkill(t *testing.T, cfg *config.Config) {
	t.Helper()
	root := t.TempDir()
	directory := filepath.Join(root, "public", "release-notes")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	document := "---\nname: release-notes\ndescription: Summarize a release\n---\n# Instructions\n\nCHANNEL_SKILL_MARKER\n"
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Skills.Enabled = true
	cfg.Skills.Root = root
	cfg.Skills.ProjectionRoot = filepath.Join(t.TempDir(), "projection")
}

func assertChannelReply(t *testing.T, reply channel.Reply, inReplyTo, text string) {
	t.Helper()
	if reply.Text != text || reply.InReplyTo != inReplyTo {
		t.Fatalf("reply = %#v", reply)
	}
}

func assertChannelModelHistory(t *testing.T, modelRequest []byte) {
	t.Helper()
	for _, want := range []string{"hello one", "channel answer 1", "hello two"} {
		if !bytes.Contains(modelRequest, []byte(want)) {
			t.Fatalf("second model request missing %q: %s", want, modelRequest)
		}
	}
}

func assertChannelResources(t *testing.T, serverURL string) {
	t.Helper()
	var threads struct {
		Threads []domain.Thread `json:"threads"`
		Count   int             `json:"count"`
	}
	getChannelResource(t, serverURL+"/api/threads", &threads)
	if threads.Count != 1 || len(threads.Threads) != 1 || threads.Threads[0].Metadata["channel_chat_id"] != "chat" {
		t.Fatalf("threads = %#v", threads)
	}
	var bindings struct {
		Connections []channel.Binding `json:"connections"`
		Count       int               `json:"count"`
	}
	getChannelResource(t, serverURL+"/api/channel-connections", &bindings)
	if bindings.Count != 1 || bindings.Connections[0].UserID != "local" {
		t.Fatalf("bindings = %#v", bindings)
	}
	var status channel.Stats
	getChannelResource(t, serverURL+"/api/channels", &status)
	if !status.Running || len(status.Providers) != 1 || status.Providers[0] != channel.WebhookProvider {
		t.Fatalf("status = %#v", status)
	}
}

func TestChannelWebhookResumesHumanInputInSameThread(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	modelServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			writeSSE(writer,
				toolCallChunk("ask", humaninput.ToolName, `{"question":"Which environment?","clarification_type":"approach_choice","options":["Staging","Production"]}`),
				doneChunk("tool_calls"),
			)
			return
		}
		writeSSE(writer, textChunk("Deploying to Staging."), doneChunk("stop"))
	}))
	defer modelServer.Close()
	replies := make(chan channel.Reply, 2)
	callback := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var reply channel.Reply
		_ = json.NewDecoder(request.Body).Decode(&reply)
		replies <- reply
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer callback.Close()
	service, err := New(context.Background(), channelTestConfig(t, modelServer.URL+"/v1", callback.URL), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = service.Close() }()
	server := httptest.NewServer(service.Handler())
	defer server.Close()
	postChannelEvent(t, server.URL, "ask-1", "deploy")
	prompt := waitChannelReply(t, replies)
	if !bytes.Contains([]byte(prompt.Text), []byte("Which environment?")) || !bytes.Contains([]byte(prompt.Text), []byte("Staging")) {
		t.Fatalf("clarification reply = %#v", prompt)
	}
	postChannelEvent(t, server.URL, "answer-1", "Staging")
	answer := waitChannelReply(t, replies)
	if answer.Text != "Deploying to Staging." || calls.Load() != 2 {
		t.Fatalf("answer/calls = %#v / %d", answer, calls.Load())
	}
}

func TestChannelConnectionAPIIsOwnerScopedAndPermissioned(t *testing.T) {
	t.Parallel()
	modelServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer modelServer.Close()
	cfg := testConfig(t, modelServer.URL+"/v1")
	cfg.Channels.Enabled = true
	aliceToken, bobToken := "alice-channel-token-at-least-24", "bob-channel-token-at-least-24xx"
	cfg.Auth = config.AuthConfig{Enabled: true, Tokens: []config.AuthTokenConfig{
		{Secret: aliceToken, PrincipalID: "alice", Permissions: []string{string(auth.ChannelsRead), string(auth.ChannelsWrite)}},
		{Secret: bobToken, PrincipalID: "bob", Permissions: []string{string(auth.ChannelsRead), string(auth.ChannelsWrite)}},
	}}
	service, err := New(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = service.Close() }()
	server := httptest.NewServer(service.Handler())
	defer server.Close()
	body := map[string]string{"provider": "webhook", "workspace_id": "workspace", "external_user_id": "external"}
	var binding channel.Binding
	channelAPIRequest(t, server.URL, http.MethodPost, "/api/channel-connections", body, aliceToken, http.StatusCreated, &binding)
	if binding.UserID != "alice" {
		t.Fatalf("binding = %#v", binding)
	}
	channelAPIRequest(t, server.URL, http.MethodPost, "/api/channel-connections", body, bobToken, http.StatusConflict, nil)
	var bob struct {
		Connections []channel.Binding `json:"connections"`
		Count       int               `json:"count"`
	}
	channelAPIRequest(t, server.URL, http.MethodGet, "/api/channel-connections", nil, bobToken, http.StatusOK, &bob)
	if bob.Count != 0 {
		t.Fatalf("bob bindings = %#v", bob)
	}
	channelAPIRequest(t, server.URL, http.MethodDelete, "/api/channel-connections/"+binding.ID, nil, aliceToken, http.StatusOK, nil)
	channelAPIRequest(t, server.URL, http.MethodPost, "/api/channel-connections", body, bobToken, http.StatusCreated, &binding)
	if binding.UserID != "bob" {
		t.Fatalf("transferred binding = %#v", binding)
	}
	channelAPIRequest(t, server.URL, http.MethodGet, "/api/channels", nil, "", http.StatusUnauthorized, nil)
}

func TestChannelConnectionCodeAPIAndProviderStatus(t *testing.T) {
	t.Parallel()
	state := channel.NewMemoryState()
	manager, err := channel.NewManager(channel.Config{
		Resolver: state, Connector: state, Dedupe: state,
		Dispatcher: appChannelDispatcherFunc(func(context.Context, channel.Request) (channel.Reply, error) {
			return channel.Reply{}, nil
		}),
		MaxInflight: 1, DedupeTTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = manager.Register(appChannelSender{name: channel.TelegramProvider}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		channels: manager, channelState: state,
		config: config.Config{Channels: config.ChannelsConfig{Telegram: config.ChannelTelegramConfig{BotUsername: "GoferBot"}}},
	}
	mux := http.NewServeMux()
	service.channelRoutes(mux)
	server := httptest.NewServer(mux)
	defer server.Close()

	var issued struct {
		Provider    string `json:"provider"`
		Mode        string `json:"mode"`
		URL         string `json:"url"`
		Code        string `json:"code"`
		Instruction string `json:"instruction"`
		ExpiresIn   int    `json:"expires_in"`
	}
	channelAPIRequest(t, server.URL, http.MethodPost, "/api/channels/telegram/connect", nil, "", http.StatusOK, &issued)
	if issued.Provider != "telegram" || issued.Mode != "deep_link" || !strings.Contains(issued.URL, issued.Code) || issued.ExpiresIn != 600 {
		t.Fatalf("connect response = %#v", issued)
	}
	identity := channel.ConnectionIdentity{Provider: "telegram", WorkspaceID: "7", ExternalUserID: "7", ExternalUserName: "alice"}
	bound, err := state.Connect(context.Background(), issued.Code, identity, time.Now())
	if err != nil || bound.UserID != "local" {
		t.Fatalf("Connect() = %#v, %v", bound, err)
	}
	var providers struct {
		Enabled   bool                      `json:"enabled"`
		Providers []channelProviderResource `json:"providers"`
	}
	channelAPIRequest(t, server.URL, http.MethodGet, "/api/channels/providers", nil, "", http.StatusOK, &providers)
	if !providers.Enabled || len(providers.Providers) != 1 || providers.Providers[0].ConnectionStatus != "connected" {
		t.Fatalf("providers = %#v", providers)
	}
	var connections struct {
		Connections []channel.Binding `json:"connections"`
	}
	channelAPIRequest(t, server.URL, http.MethodGet, "/api/channels/connections", nil, "", http.StatusOK, &connections)
	if len(connections.Connections) != 1 {
		t.Fatalf("connections = %#v", connections)
	}
	channelAPIRequest(t, server.URL, http.MethodDelete, "/api/channels/connections/"+bound.ID, nil, "", http.StatusNoContent, nil)
	channelAPIRequest(t, server.URL, http.MethodPost, "/api/channels/github/connect", nil, "", http.StatusNotFound, nil)
}

func TestChannelDispatcherFailsClosedOnForeignMappingAndAnyActiveRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Now().UTC()
	repository := store.NewMemory()
	state := channel.NewMemoryState()
	binding, _ := channel.NewBinding("alice", "webhook", "workspace", "external", now)
	binding, _ = state.Bind(ctx, binding)
	foreign, _ := domain.NewThread(now)
	foreign.Metadata = map[string]string{store.OwnerMetadataKey: "bob"}
	if err := repository.CreateThread(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	_, _, _ = state.MapConversation(ctx, channel.Conversation{
		BindingID: binding.ID, Provider: "webhook", ChatID: "chat", ThreadID: foreign.ID,
		CreatedAt: now, UpdatedAt: now,
	})
	dispatcher := channelDispatcher{service: &Service{store: repository}, state: state}
	request := channel.Request{Identity: channel.Identity{BindingID: binding.ID, UserID: "alice"}, Message: channel.Message{Provider: "webhook", ChatID: "chat"}}
	if _, err := dispatcher.ensureThread(ctx, request); !errors.Is(err, channel.ErrConflict) {
		t.Fatalf("foreign ensureThread() = %v", err)
	}

	owned, _ := domain.NewThread(now)
	owned.Metadata = map[string]string{store.OwnerMetadataKey: "alice"}
	if err := repository.CreateThread(ctx, owned); err != nil {
		t.Fatal(err)
	}
	active, _ := domain.NewRun(owned.ID, now)
	if err := repository.CreateRun(ctx, active); err != nil {
		t.Fatal(err)
	}
	latest, _ := domain.NewRun(owned.ID, now.Add(time.Second))
	if err := repository.CreateRun(ctx, latest); err != nil {
		t.Fatal(err)
	}
	latest, _ = repository.TransitionRun(ctx, latest.ID, domain.RunPending, domain.RunRunning, now.Add(2*time.Second), "")
	_, _ = repository.TransitionRun(ctx, latest.ID, domain.RunRunning, domain.RunSucceeded, now.Add(3*time.Second), "")
	if err := dispatcher.ensureThreadIdle(ctx, owned.ID); !errors.Is(err, channel.ErrBusy) {
		t.Fatalf("ensureThreadIdle() = %v", err)
	}
}

func TestChannelDispatcherControlCommands(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Now().UTC()
	repository := store.NewMemory()
	state := channel.NewMemoryState()
	binding, _ := channel.NewBinding("alice", channel.SlackProvider, "team", "U1", now)
	binding, _ = state.Bind(ctx, binding)
	controls, _ := control.NewService(control.NewInMemory(), func() time.Time { return now })
	memories := memory.NewInMemory()
	if err := memories.Upsert(ctx, memory.Entry{
		ID: "fact", Scope: memory.Scope{UserID: "alice"}, Text: "Remember this",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		store: repository, controls: controls, memories: memories,
		config: config.Config{Models: []config.ModelConfig{{Name: "primary"}, {Name: "fast"}}},
	}
	dispatcher := channelDispatcher{service: service, state: state}
	request := channel.Request{
		Identity: channel.Identity{BindingID: binding.ID, UserID: "alice"},
		Message:  channel.Message{Provider: channel.SlackProvider, WorkspaceID: "team", ExternalUserID: "U1", ChatID: "chat"},
	}
	command := func(text string) channel.Reply {
		t.Helper()
		request.Message.Text = text
		reply, err := dispatcher.Dispatch(ctx, request)
		if err != nil {
			t.Fatalf("Dispatch(%q) = %v", text, err)
		}
		return reply
	}
	if reply := command("/status"); reply.Text != "No active conversation." {
		t.Fatalf("status = %q", reply.Text)
	}
	first := command("/new")
	if first.Text != "New conversation started." {
		t.Fatalf("new = %q", first.Text)
	}
	mapped, err := state.Conversation(ctx, binding.ID, "chat", "")
	if err != nil {
		t.Fatal(err)
	}
	if reply := command("/status"); reply.Text != "Active thread: "+string(mapped.ThreadID) {
		t.Fatalf("status = %q", reply.Text)
	}
	if reply := command("/models"); reply.Text != "Available models:\n• primary\n• fast" {
		t.Fatalf("models = %q", reply.Text)
	}
	if reply := command("/memory"); reply.Text != "Memory contains 1 fact(s)." {
		t.Fatalf("memory = %q", reply.Text)
	}
	if _, err = controls.SetGoal(ctx, mapped.ThreadID, "Ship it", 0); err != nil {
		t.Fatal(err)
	}
	if reply := command("/goal"); reply.Text != "Goal: Ship it" {
		t.Fatalf("goal = %q", reply.Text)
	}
	if reply := command("/goal reset"); reply.Text != "Goal cleared." {
		t.Fatalf("goal clear = %q", reply.Text)
	}
	if reply := command("/goal"); reply.Text != "No active goal." {
		t.Fatalf("cleared goal = %q", reply.Text)
	}
	if reply := command("/help"); !strings.Contains(reply.Text, "/<skill-name>") {
		t.Fatalf("help = %q", reply.Text)
	}
	if reply := command("/missing task"); !strings.HasPrefix(reply.Text, "Unknown command: /missing.") {
		t.Fatalf("unknown = %q", reply.Text)
	}
	second := command("/new")
	if second.Text != "New conversation started." {
		t.Fatalf("new conversation reply = %q", second.Text)
	}
	replaced, err := state.Conversation(ctx, binding.ID, "chat", "")
	if err != nil || replaced.ThreadID == mapped.ThreadID {
		t.Fatalf("remapped = %#v, %v", replaced, err)
	}
}

func TestChannelAssistantSelectionTrustsOnlyGitHubSubscriptions(t *testing.T) {
	t.Parallel()
	github := channel.Message{Provider: "github", Metadata: map[string]string{"assistant_id": "reviewer"}}
	webhook := channel.Message{Provider: "webhook", Metadata: map[string]string{"assistant_id": "reviewer"}}
	if got := channelAssistantID(github, "primary"); got != "reviewer" {
		t.Fatalf("GitHub assistant = %q", got)
	}
	if got := channelAssistantID(webhook, "primary"); got != "primary" {
		t.Fatalf("untrusted webhook assistant = %q", got)
	}
}

func TestChannelProtocolHelpers(t *testing.T) {
	t.Parallel()
	displayNames := map[string]string{
		channel.TelegramProvider: "Telegram", channel.SlackProvider: "Slack",
		channel.DiscordProvider: "Discord", channel.FeishuProvider: "Feishu",
		channel.DingTalkProvider: "DingTalk", channel.WeComProvider: "WeCom",
		channel.WeChatProvider: "WeChat", channel.BuzzProvider: "Buzz", "custom": "custom",
	}
	for provider, want := range displayNames {
		if got := channelProviderDisplayName(provider); got != want {
			t.Fatalf("channelProviderDisplayName(%q) = %q", provider, got)
		}
	}
	if channelConnectionMode(channel.TelegramProvider) != "deep_link" || channelConnectionMode(channel.SlackProvider) != "binding_code" {
		t.Fatal("unexpected channel connection mode")
	}
	if !containsString([]string{"discord", "slack", "telegram"}, "slack") || containsString([]string{"discord", "slack"}, "telegram") {
		t.Fatal("containsString returned an unexpected result")
	}

	input, err := channelRunInput(channel.Message{Text: "review", Attachments: []channel.Attachment{{Name: "report.pdf", MediaType: "application/pdf", URL: "https://example.test/report.pdf", Size: 42}}})
	if err != nil || !bytes.Contains(input, []byte("channel_attachments")) || !bytes.Contains(input, []byte("report.pdf")) {
		t.Fatalf("channelRunInput() = %s, %v", input, err)
	}

	errorsToStatus := []struct {
		err    error
		status int
	}{
		{channel.ErrInvalid, http.StatusBadRequest},
		{channel.ErrNotFound, http.StatusNotFound},
		{channel.ErrConflict, http.StatusConflict},
		{channel.ErrBusy, http.StatusTooManyRequests},
		{errors.New("storage failure"), http.StatusInternalServerError},
	}
	for _, test := range errorsToStatus {
		recorder := httptest.NewRecorder()
		writeChannelError(recorder, test.err)
		if recorder.Code != test.status {
			t.Fatalf("writeChannelError(%v) = %d, want %d", test.err, recorder.Code, test.status)
		}
	}
}

func channelTestConfig(t *testing.T, modelURL, callbackURL string) config.Config {
	t.Helper()
	cfg := testConfig(t, modelURL)
	cfg.Channels.Enabled = true
	cfg.Channels.Webhook = config.ChannelWebhookConfig{
		Enabled: true, Secret: appWebhookSecret, OutboundURL: callbackURL,
		TimeoutSeconds: 2, MaxAttempts: 1, MaxBodyBytes: 1 << 20,
		ClockSkewSeconds: 300, AllowPrivateAddresses: true,
	}
	cfg.Channels.Bindings = []config.ChannelBindingConfig{{
		UserID: "local", Provider: channel.WebhookProvider, WorkspaceID: "workspace", ExternalUserID: "external",
	}}
	return cfg
}

func postChannelEvent(t *testing.T, baseURL, eventID, text string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"id": eventID, "external_user_id": "external", "chat_id": "chat", "topic_id": "topic", "text": text,
	})
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(appWebhookSecret))
	_, _ = mac.Write([]byte(timestamp + "."))
	_, _ = mac.Write(body)
	request, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, baseURL+"/api/channels/webhook/workspace/events", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(channel.WebhookTimestampHeader, timestamp)
	request.Header.Set(channel.WebhookSignatureHeader, "sha256="+hex.EncodeToString(mac.Sum(nil)))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("channel event status = %d: %s", response.StatusCode, payload)
	}
}

func postAppGitHubWebhook(t *testing.T, baseURL string, body []byte, event, delivery string, valid bool) *http.Response {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(appWebhookSecret))
	_, _ = mac.Write(body)
	signature := hex.EncodeToString(mac.Sum(nil))
	if !valid {
		signature = strings.Repeat("0", sha256.Size*2)
	}
	request, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, baseURL+"/api/webhooks/github", bytes.NewReader(body))
	request.Header.Set("X-Hub-Signature-256", "sha256="+signature)
	request.Header.Set("X-GitHub-Event", event)
	request.Header.Set("X-GitHub-Delivery", delivery)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func waitChannelReply(t *testing.T, replies <-chan channel.Reply) channel.Reply {
	t.Helper()
	select {
	case reply := <-replies:
		return reply
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for channel reply")
		return channel.Reply{}
	}
}

func getChannelResource(t *testing.T, rawURL string, output any) {
	t.Helper()
	response, err := http.Get(rawURL) //nolint:noctx // Test request is bounded by the local server lifecycle.
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d", rawURL, response.StatusCode)
	}
	if err = json.NewDecoder(response.Body).Decode(output); err != nil {
		t.Fatal(err)
	}
}

func channelAPIRequest(t *testing.T, baseURL, method, requestPath string, body any, token string, want int, output any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, _ := json.Marshal(body)
		reader = bytes.NewReader(encoded)
	}
	request, _ := http.NewRequestWithContext(context.Background(), method, baseURL+requestPath, reader)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != want {
		payload, _ := io.ReadAll(response.Body)
		t.Fatalf("%s %s status = %d, want %d: %s", method, requestPath, response.StatusCode, want, payload)
	}
	if output != nil {
		if err = json.NewDecoder(response.Body).Decode(output); err != nil {
			t.Fatal(err)
		}
	}
}
