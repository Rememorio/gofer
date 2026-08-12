package channel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSlackSocketModeLifecycle(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/auth.test":
			if request.Header.Get("Authorization") != "Bearer xoxb-secret" {
				t.Errorf("authorization = %q", request.Header.Get("Authorization"))
			}
			_, _ = writer.Write([]byte(`{"ok":true,"user_id":"UBOT"}`))
		case "/apps.connections.open":
			_, _ = writer.Write([]byte(`{"ok":true,"url":"ws://socket.test/link"}`))
		}
	}))
	defer server.Close()
	socket := newFakeProviderSocket(4)
	provider, err := NewSlack(SlackConfig{
		BotToken: "xoxb-secret", AppToken: "xapp-secret", AllowedUsers: []string{"U1"},
		RequestTimeout: time.Second, Client: server.Client(), BaseURL: server.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	provider.dial = func(context.Context, string, *http.Client) (providerSocket, error) { return socket, nil }
	received := make(chan Message, 1)
	if err = provider.Start(context.Background(), func(_ context.Context, message Message) error {
		received <- message
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	socket.push(map[string]any{
		"type": "events_api", "envelope_id": "env-1",
		"payload": map[string]any{
			"event_id": "event-1", "team_id": "T1",
			"event": map[string]any{"type": "app_mention", "user": "U1", "channel": "C1", "ts": "1700000000.123", "text": "<@UBOT> hello"},
		},
	})
	select {
	case message := <-received:
		if message.Provider != SlackProvider || message.Text != "hello" || message.WorkspaceID != "T1" || message.TopicID != "1700000000.123" || message.Metadata["event_id"] != "event-1" {
			t.Fatalf("message = %#v", message)
		}
	case <-time.After(time.Second):
		t.Fatal("Slack event not submitted")
	}
	eventually(t, time.Second, func() bool { return len(socket.written()) == 1 })
	var ack map[string]string
	_ = json.Unmarshal(socket.written()[0], &ack)
	if ack["envelope_id"] != "env-1" {
		t.Fatalf("ack = %#v", ack)
	}
	if err = provider.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSlackBusyEventIsNotAcknowledged(t *testing.T) {
	t.Parallel()
	provider, _ := NewSlack(SlackConfig{BotToken: "bot", AppToken: "app", BotUserID: "BOT", RequestTimeout: time.Second})
	socket := newFakeProviderSocket(2)
	socket.push(map[string]any{
		"type": "events_api", "envelope_id": "busy",
		"payload": map[string]any{"team_id": "T", "event": map[string]any{"type": "message", "user": "U", "channel": "C", "ts": "1", "text": "hello"}},
	})
	socket.push(map[string]any{"type": "disconnect"})
	if reconnect := provider.readSocket(context.Background(), func(context.Context, Message) error { return ErrBusy }, socket); !reconnect {
		t.Fatal("disconnect did not request reconnect")
	}
	if writes := socket.written(); len(writes) != 0 {
		t.Fatalf("busy event acknowledged: %q", writes)
	}
}

func TestSlackSendFormatsAndRetries(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	var mu sync.Mutex
	var input map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if calls.Add(1) == 1 {
			writer.Header().Set("Retry-After", "0.01")
			writer.WriteHeader(http.StatusTooManyRequests)
			return
		}
		mu.Lock()
		_ = json.NewDecoder(request.Body).Decode(&input)
		mu.Unlock()
		_, _ = writer.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	var delay time.Duration
	provider, _ := NewSlack(SlackConfig{
		BotToken: "bot", AppToken: "app", BotUserID: "BOT", RequestTimeout: time.Second,
		MaxAttempts: 2, Client: server.Client(), BaseURL: server.URL,
		Sleep: func(_ context.Context, duration time.Duration) error { delay = duration; return nil },
	})
	err := provider.Send(context.Background(), Reply{Provider: SlackProvider, ChatID: "C", TopicID: "1.2", Text: "**bold** & <tag>\n> quote"})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls.Load() != 2 || delay != 10*time.Millisecond || input["thread_ts"] != "1.2" || input["text"] != "*bold* &amp; &lt;tag&gt;\n> quote" {
		t.Fatalf("calls=%d delay=%v input=%#v", calls.Load(), delay, input)
	}
}

func TestSlackNormalizeFiltersAndAttachments(t *testing.T) {
	t.Parallel()
	provider, _ := NewSlack(SlackConfig{BotToken: "bot", AppToken: "app", BotUserID: "BOT", AllowedUsers: []string{"U"}, RequestTimeout: time.Second})
	message, keep := provider.normalize(slackEventPayload{Team: "T", EventID: "E", Event: slackEvent{
		Type: "message", User: "U", Channel: "C", Timestamp: "1700000000.1", ThreadTS: "root", ClientMsgID: "client",
		Files: []slackFile{{ID: "F", Name: "", MimeType: "", Size: -2}},
	}})
	if !keep || message.TopicID != "root" || len(message.Attachments) != 1 || message.Attachments[0].Name != "slack-file" || message.Attachments[0].Size != 0 {
		t.Fatalf("normalize = %#v, %v", message, keep)
	}
	for _, event := range []slackEvent{
		{Type: "reaction_added", User: "U", Text: "x"},
		{Type: "message", User: "other", Text: "x"},
		{Type: "message", User: "U", BotID: "B", Text: "x"},
		{Type: "message", User: "U", Subtype: "edited", Text: "x"},
	} {
		if _, keep = provider.normalize(slackEventPayload{Event: event}); keep {
			t.Fatalf("filtered event accepted: %#v", event)
		}
	}
	connect, keep := provider.normalize(slackEventPayload{Team: "T", Event: slackEvent{
		Type: "message", User: "other", Channel: "C", Timestamp: "2", Text: "/connect code",
	}})
	if !keep || connect.Text != "/connect code" {
		t.Fatalf("disallowed connect = %#v, %v", connect, keep)
	}
	if got := stripSlackMention("<@!BOT> hi", "BOT"); got != "hi" {
		t.Fatalf("strip mention = %q", got)
	}
	if timestamp := slackTimestamp("bad"); timestamp.IsZero() {
		t.Fatal("fallback timestamp is zero")
	}
}

func TestSlackReconnectsAndValidates(t *testing.T) {
	t.Parallel()
	var opens atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/auth.test" {
			_, _ = writer.Write([]byte(`{"ok":true,"user_id":"BOT"}`))
			return
		}
		opens.Add(1)
		_, _ = writer.Write([]byte(`{"ok":true,"url":"ws://socket.test/link"}`))
	}))
	defer server.Close()
	first, second := newFakeProviderSocket(1), newFakeProviderSocket(1)
	first.push(map[string]any{"type": "disconnect"})
	provider, _ := NewSlack(SlackConfig{
		BotToken: "bot", AppToken: "app", RequestTimeout: time.Second,
		Client: server.Client(), BaseURL: server.URL, Sleep: func(context.Context, time.Duration) error { return nil },
	})
	var dials atomic.Int32
	provider.dial = func(context.Context, string, *http.Client) (providerSocket, error) {
		if dials.Add(1) == 1 {
			return first, nil
		}
		return second, nil
	}
	if provider.Name() != SlackProvider {
		t.Fatalf("Name = %q", provider.Name())
	}
	if err := provider.Start(context.Background(), func(context.Context, Message) error { return nil }); err != nil {
		t.Fatal(err)
	}
	eventually(t, time.Second, func() bool { return dials.Load() >= 2 && opens.Load() >= 2 })
	_ = provider.Close()

	for _, config := range []SlackConfig{
		{},
		{BotToken: "bot", AppToken: "app", RequestTimeout: 0, MaxAttempts: 6},
		{BotToken: "bot", AppToken: "app", RequestTimeout: time.Second, BaseURL: "http://example.com"},
	} {
		if _, err := NewSlack(config); !errors.Is(err, ErrInvalid) {
			t.Fatalf("NewSlack(%#v) = %v", config, err)
		}
	}
	if err := provider.Send(context.Background(), Reply{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid Send = %v", err)
	}
}

func TestSlackStartFailureAndClosed(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/auth.test" {
			_, _ = writer.Write([]byte(`{"ok":false,"error":"invalid_auth"}`))
		}
	}))
	defer server.Close()
	provider, _ := NewSlack(SlackConfig{BotToken: "bot", AppToken: "app", RequestTimeout: time.Second, Client: server.Client(), BaseURL: server.URL})
	if err := provider.Start(context.Background(), func(context.Context, Message) error { return nil }); err == nil {
		t.Fatal("invalid credentials accepted")
	}
	_ = provider.Close()
	if err := provider.Start(context.Background(), func(context.Context, Message) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Fatalf("Start after close = %v", err)
	}
}

func TestSlackRejectsUntrustedSocketHost(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"ok":true,"url":"wss://slack.com.attacker.test/link"}`))
	}))
	defer server.Close()
	provider, _ := NewSlack(SlackConfig{BotToken: "bot", AppToken: "app", RequestTimeout: time.Second})
	provider.baseURL, provider.client = server.URL, server.Client()
	if _, err := provider.openSocket(context.Background()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("openSocket = %v", err)
	}
}

func TestSlackClosePreservesError(t *testing.T) {
	t.Parallel()
	provider, _ := NewSlack(SlackConfig{BotToken: "bot", AppToken: "app", RequestTimeout: time.Second})
	want := errors.New("close failed")
	provider.socket = &fakeProviderSocket{reads: make(chan []byte), closed: make(chan struct{}), closeErr: want}
	if err := provider.Close(); !errors.Is(err, want) {
		t.Fatalf("first Close = %v", err)
	}
	if err := provider.Close(); !errors.Is(err, want) {
		t.Fatalf("second Close = %v", err)
	}
}

func eventually(t *testing.T, timeout time.Duration, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not met")
}
