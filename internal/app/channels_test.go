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
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/auth"
	"github.com/Rememorio/gofer/internal/channel"
	"github.com/Rememorio/gofer/internal/config"
	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/humaninput"
	"github.com/Rememorio/gofer/internal/store"
)

const appWebhookSecret = "app-webhook-secret-at-least-24-bytes"

type appChannelDispatcherFunc func(context.Context, channel.Request) (channel.Reply, error)

func (function appChannelDispatcherFunc) Dispatch(ctx context.Context, request channel.Request) (channel.Reply, error) {
	return function(ctx, request)
}

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
	if providers := manager.Providers(); fmt.Sprint(providers) != "[dingtalk discord feishu slack telegram wechat wecom]" {
		t.Fatalf("providers = %v", providers)
	}
	if err = manager.Close(); err != nil {
		t.Fatal(err)
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
