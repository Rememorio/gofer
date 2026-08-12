package channel

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWeChatPollAndSend(t *testing.T) {
	t.Parallel()
	fixture := &weChatAPIFixture{test: t}
	server := httptest.NewServer(http.HandlerFunc(fixture.handle))
	defer server.Close()
	provider, err := NewWeChat(WeChatConfig{
		BotToken: "token", ILinkBotID: "bot", ILinkAppID: "app", RouteTag: "route", ChannelVersion: "1.2.3",
		PollTimeout: time.Second, RequestTimeout: 2 * time.Second, MaxAttempts: 1,
		Client: server.Client(), BaseURL: server.URL, UIN: func() (string, error) { return "uin", nil },
		Now: func() time.Time { return time.Unix(123, 0) },
	})
	if err != nil {
		t.Fatal(err)
	}
	var inbound Message
	if err = provider.pollOnce(context.Background(), func(_ context.Context, message Message) error { inbound = message; return nil }); err != nil {
		t.Fatal(err)
	}
	assertWeChatInbound(t, inbound)
	provider.mu.Lock()
	if provider.cursor != "cursor-2" || provider.pollTimeout != 40*time.Second {
		t.Fatalf("cursor/timeout = %q/%v", provider.cursor, provider.pollTimeout)
	}
	provider.mu.Unlock()
	reply := Reply{Provider: WeChatProvider, WorkspaceID: "wx-user", ChatID: "wx-user", InReplyTo: "message-1", Text: "answer"}
	if err = provider.Send(context.Background(), reply); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	message := fixture.sent["msg"].(map[string]any)
	items := message["item_list"].([]any)
	if message["context_token"] != "context-1" || message["to_user_id"] != "wx-user" || items[0].(map[string]any)["type"] != float64(1) || !strings.HasPrefix(message["client_id"].(string), "gofer_") {
		t.Fatalf("sent = %#v", fixture.sent)
	}
	if strings.Join(fixture.paths, ",") != "/ilink/bot/getupdates,/ilink/bot/sendmessage" {
		t.Fatalf("paths = %#v", fixture.paths)
	}
}

type weChatAPIFixture struct {
	test  *testing.T
	mu    sync.Mutex
	paths []string
	sent  map[string]any
}

func (fixture *weChatAPIFixture) handle(writer http.ResponseWriter, request *http.Request) {
	fixture.mu.Lock()
	fixture.paths = append(fixture.paths, request.URL.Path)
	fixture.mu.Unlock()
	expectHeader(fixture.test, request, "Authorization", "Bearer token")
	expectHeader(fixture.test, request, "AuthorizationType", "ilink_bot_token")
	expectHeader(fixture.test, request, "X-WECHAT-UIN", "uin")
	expectHeader(fixture.test, request, "iLink-App-ClientVersion", "66051")
	expectHeader(fixture.test, request, "iLink-App-Id", "app")
	expectHeader(fixture.test, request, "SKRouteTag", "route")
	if request.URL.Path == "/ilink/bot/getupdates" {
		writeWeChatUpdates(writer)
		return
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	_ = json.NewDecoder(request.Body).Decode(&fixture.sent)
	_ = json.NewEncoder(writer).Encode(map[string]int{"ret": 0})
}

func expectHeader(t *testing.T, request *http.Request, name, want string) {
	t.Helper()
	if got := request.Header.Get(name); got != want {
		t.Errorf("%s = %q, want %q", name, got, want)
	}
}

func writeWeChatUpdates(writer http.ResponseWriter) {
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"ret": 0, "get_updates_buf": "cursor-2", "longpolling_timeout_ms": 40000,
		"msgs": []any{map[string]any{
			"message_id": "message-1", "message_type": 1, "from_user_id": "wx-user", "context_token": "context-1",
			"item_list": []any{
				map[string]any{"type": 1, "text_item": map[string]string{"text": " hello "}},
				map[string]any{"type": 2, "image_item": map[string]any{"media": map[string]string{"full_url": "https://cdn.example/image"}}},
				map[string]any{"type": 4, "file_item": map[string]any{"file_name": " report.pdf ", "media": map[string]string{"full_url": "https://cdn.example/file"}}},
			},
		}},
	})
}

func assertWeChatInbound(t *testing.T, inbound Message) {
	t.Helper()
	if inbound.ID != "message-1" || inbound.Provider != WeChatProvider || inbound.WorkspaceID != "wx-user" || inbound.Text != "hello" || len(inbound.Attachments) != 2 || inbound.Metadata["context_token"] != "context-1" {
		t.Fatalf("inbound = %#v", inbound)
	}
	if inbound.Attachments[1].Name != "report.pdf" || strings.Contains(inbound.Attachments[0].URL, "cdn.example") {
		t.Fatalf("attachments = %#v", inbound.Attachments)
	}
}

func TestWeChatLifecycleStopsPolling(t *testing.T) {
	t.Parallel()
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		select {
		case <-requestStarted:
		default:
			close(requestStarted)
		}
		select {
		case <-request.Context().Done():
		case <-releaseRequest:
		}
	}))
	defer server.Close()
	provider, _ := NewWeChat(WeChatConfig{
		BotToken: "token", PollTimeout: time.Second, RequestTimeout: 2 * time.Second, MaxAttempts: 1,
		Client: server.Client(), BaseURL: server.URL, UIN: func() (string, error) { return "uin", nil },
	})
	submit := func(context.Context, Message) error { return nil }
	if err := provider.Start(context.Background(), submit); err != nil {
		t.Fatal(err)
	}
	if err := provider.Start(context.Background(), submit); err != nil {
		t.Fatal(err)
	}
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("polling did not start")
	}
	if err := provider.Close(); err != nil {
		t.Fatal(err)
	}
	close(releaseRequest)
	_ = provider.Close()
	if err := provider.Start(context.Background(), submit); !errors.Is(err, ErrClosed) {
		t.Fatalf("Start after Close = %v", err)
	}
	if err := (*WeChat)(nil).Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWeChatNormalizationAndCursorSafety(t *testing.T) {
	t.Parallel()
	provider, _ := NewWeChat(WeChatConfig{BotToken: "token", AllowedUsers: []string{"allowed"}, PollTimeout: time.Second, RequestTimeout: 2 * time.Second, MaxAttempts: 1, UIN: func() (string, error) { return "uin", nil }})
	if provider.Name() != WeChatProvider {
		t.Fatalf("Name = %q", provider.Name())
	}
	base := weChatMessage{MessageID: "m", MessageType: weChatMessageIn, FromUserID: "allowed", Context: "ctx"}
	base.Items = make([]weChatItem, 1)
	base.Items[0].Type, base.Items[0].TextItem.Text = weChatMessageText, "one"
	message, token, keep := provider.normalize(base)
	if !keep || token != "ctx" || message.Text != "one" || message.ReceivedAt.IsZero() {
		t.Fatalf("normalize = %#v, %q, %v", message, token, keep)
	}
	blocked, wrongType, empty := base, base, base
	blocked.FromUserID = "blocked"
	wrongType.MessageType = 2
	empty.Items = nil
	for _, incoming := range []weChatMessage{blocked, wrongType, empty, {}} {
		if _, _, accepted := provider.normalize(incoming); accepted {
			t.Fatalf("unexpected accepted message %#v", incoming)
		}
	}
	blocked.Items[0].TextItem.Text = "/connect code"
	if message, _, accepted := provider.normalize(blocked); !accepted || message.Text != "/connect code" {
		t.Fatalf("disallowed connect = %#v, %v", message, accepted)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"ret": 0, "get_updates_buf": "next", "msgs": []any{map[string]any{
				"message_id": "m2", "message_type": 1, "from_user_id": "allowed", "context_token": "ctx2",
				"item_list": []any{map[string]any{"type": 1, "text_item": map[string]string{"text": "two"}}},
			}},
		})
	}))
	defer server.Close()
	provider.client, provider.baseURL = server.Client(), server.URL
	if err := provider.pollOnce(context.Background(), func(context.Context, Message) error { return ErrClosed }); !errors.Is(err, ErrClosed) {
		t.Fatalf("poll submit error = %v", err)
	}
	provider.mu.Lock()
	cursor := provider.cursor
	provider.mu.Unlock()
	if cursor != "" {
		t.Fatalf("cursor advanced to %q after submit failure", cursor)
	}
}

func TestWeChatValidationAndProtocolErrors(t *testing.T) {
	t.Parallel()
	for _, config := range []WeChatConfig{
		{}, {BotToken: "token", ChannelVersion: "1.bad"},
		{BotToken: "token", AllowedUsers: []string{"same", "same"}},
		{BotToken: "token", PollTimeout: time.Millisecond, RequestTimeout: time.Second},
		{BotToken: "token", PollTimeout: time.Second, RequestTimeout: time.Second},
	} {
		if _, err := NewWeChat(config); !errors.Is(err, ErrInvalid) {
			t.Fatalf("NewWeChat(%#v) = %v", config, err)
		}
	}
	if version, ok := weChatClientVersion("255.0.1"); !ok || version != "16711681" {
		t.Fatalf("client version = %q, %v", version, ok)
	}
	if _, ok := weChatClientVersion("1.2.3.4"); ok {
		t.Fatal("long client version accepted")
	}
	uin, err := randomWeChatUIN()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(uin)
	if err != nil || len(decoded) == 0 {
		t.Fatalf("UIN = %q, %v", uin, err)
	}
	if !errors.Is(weChatResponseError(-14, 0, "expired"), errWeChatSessionExpired) || weChatResponseError(0, 0, "ignored") != nil || weChatResponseError(0, 10, "error") == nil {
		t.Fatal("response error classification is incorrect")
	}
	if _, retry := providerRetryable(weChatResponseError(1, 2, "busy")); !retry {
		t.Fatal("transient iLink failure is not retryable")
	}
	provider, _ := NewWeChat(WeChatConfig{BotToken: "token", PollTimeout: time.Second, RequestTimeout: 2 * time.Second, MaxAttempts: 1, UIN: func() (string, error) { return "", errors.New("random failed") }})
	if err = provider.request(context.Background(), "/x", nil, nil); err == nil {
		t.Fatal("UIN failure was ignored")
	}
	for _, reply := range []Reply{{}, {Provider: WeChatProvider, ChatID: "chat", Text: "x"}, {Provider: "other", ChatID: "chat", InReplyTo: "m", Text: "x"}} {
		if err = provider.Send(context.Background(), reply); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Send(%#v) = %v", reply, err)
		}
	}
}
