package channel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDiscordGatewayLifecycle(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/gateway/bot":
			if request.Header.Get("Authorization") != "Bot secret" {
				t.Errorf("authorization = %q", request.Header.Get("Authorization"))
			}
			_, _ = writer.Write([]byte(`{"url":"ws://gateway.test"}`))
		case "/channels/C1":
			_, _ = writer.Write([]byte(`{"id":"C1","type":0}`))
		}
	}))
	defer server.Close()
	socket := newFakeProviderSocket(4)
	socket.push(map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 60000}})
	provider, err := NewDiscord(DiscordConfig{
		BotToken: "secret", MentionOnly: true, RequestTimeout: time.Second,
		Client: server.Client(), BaseURL: server.URL,
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
	socket.push(map[string]any{"op": 0, "s": 1, "t": "READY", "d": map[string]any{"session_id": "session", "resume_gateway_url": "ws://resume.test", "user": map[string]string{"id": "BOT"}}})
	socket.push(map[string]any{"op": 0, "s": 2, "t": "MESSAGE_CREATE", "d": map[string]any{
		"id": "1174109847031779328", "channel_id": "C1", "guild_id": "G1", "content": "<@BOT> hello",
		"author": map[string]any{"id": "U1", "username": "alice", "bot": false},
	}})
	select {
	case message := <-received:
		if message.Provider != DiscordProvider || message.Text != "hello" || message.WorkspaceID != "G1" || message.ChatID != "C1" || message.TopicID != "C1" {
			t.Fatalf("message = %#v", message)
		}
	case <-time.After(time.Second):
		t.Fatal("Discord message not submitted")
	}
	eventually(t, time.Second, func() bool { return len(socket.written()) >= 1 })
	var identify map[string]any
	_ = json.Unmarshal(socket.written()[0], &identify)
	if identify["op"] != float64(2) {
		t.Fatalf("identify = %#v", identify)
	}
	if err = provider.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDiscordThreadRoutingAndAttachments(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/channels/C":
			_, _ = writer.Write([]byte(`{"id":"C","type":0}`))
		case request.Method == http.MethodPost && request.URL.Path == "/channels/C/messages/M/threads":
			_, _ = writer.Write([]byte(`{"id":"THREAD","type":11,"parent_id":"C"}`))
		case request.Method == http.MethodGet && request.URL.Path == "/channels/ORPHAN":
			_, _ = writer.Write([]byte(`{"id":"ORPHAN","type":11,"parent_id":"PARENT"}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	provider, _ := NewDiscord(DiscordConfig{
		BotToken: "secret", MentionOnly: true, ThreadMode: true, RequestTimeout: time.Second,
		Client: server.Client(), BaseURL: server.URL,
	})
	provider.botUserID = "BOT"
	message, keep := provider.normalize(context.Background(), discordMessage{
		ID: "M", ChannelID: "C", GuildID: "G", Content: "<@!BOT> inspect",
		Author: discordAuthor{ID: "U", Username: "alice"},
		Attachments: []discordAttachment{
			{ID: "A", Filename: "a.txt", ContentType: "text/plain", URL: "https://cdn.example/a", Size: 2},
			{ID: "B", URL: "http://unsafe.example/b"},
		},
	})
	if !keep || message.Text != "inspect" || message.ChatID != "C" || message.TopicID != "THREAD" || len(message.Attachments) != 1 {
		t.Fatalf("normalize = %#v, %v", message, keep)
	}
	message, keep = provider.normalize(context.Background(), discordMessage{
		ID: "N", ChannelID: "ORPHAN", GuildID: "G", Content: "follow up", Author: discordAuthor{ID: "U"},
	})
	if !keep || message.ChatID != "PARENT" || message.TopicID != "ORPHAN" {
		t.Fatalf("thread normalize = %#v, %v", message, keep)
	}
	if _, keep = provider.normalize(context.Background(), discordMessage{ID: "X", ChannelID: "C", GuildID: "G", Content: "no mention", Author: discordAuthor{ID: "U"}}); keep {
		t.Fatal("mention-free channel message accepted")
	}
}

func TestDiscordSendSplitsAndRetries(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	var mu sync.Mutex
	var inputs []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if calls.Add(1) == 1 {
			writer.WriteHeader(http.StatusTooManyRequests)
			_, _ = writer.Write([]byte(`{"message":"limited","retry_after":0.02}`))
			return
		}
		var input map[string]any
		_ = json.NewDecoder(request.Body).Decode(&input)
		mu.Lock()
		inputs = append(inputs, input)
		mu.Unlock()
		_, _ = writer.Write([]byte(`{"id":"sent"}`))
	}))
	defer server.Close()
	var delay time.Duration
	provider, _ := NewDiscord(DiscordConfig{
		BotToken: "secret", RequestTimeout: time.Second, MaxAttempts: 2,
		Client: server.Client(), BaseURL: server.URL,
		Sleep: func(_ context.Context, duration time.Duration) error { delay = duration; return nil },
	})
	err := provider.Send(context.Background(), Reply{Provider: DiscordProvider, ChatID: "C", TopicID: "T", Text: strings.Repeat("😀", 1001)})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls.Load() != 3 || len(inputs) != 2 || delay != 20*time.Millisecond {
		t.Fatalf("calls=%d inputs=%d delay=%v", calls.Load(), len(inputs), delay)
	}
}

func TestDiscordFiltersAndGatewayResume(t *testing.T) {
	t.Parallel()
	provider, _ := NewDiscord(DiscordConfig{
		BotToken: "secret", AllowedGuilds: []string{"G"}, AllowedChannels: []string{"C"},
		MentionOnly: true, RequestTimeout: time.Second,
		Client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: ioNopCloser(`{"id":"C","type":0}`)}, nil
		})}, BaseURL: "http://discord.test",
	})
	provider.botUserID, provider.sessionID, provider.sequence = "BOT", "session", 7
	message, keep := provider.normalize(context.Background(), discordMessage{ID: "1", ChannelID: "C", GuildID: "G", Content: "allowed channel", Author: discordAuthor{ID: "U"}})
	if !keep || message.Text != "allowed channel" {
		t.Fatalf("allowed channel = %#v, %v", message, keep)
	}
	for _, inbound := range []discordMessage{
		{ID: "1", ChannelID: "C", GuildID: "other", Content: "x", Author: discordAuthor{ID: "U"}},
		{ID: "1", ChannelID: "C", GuildID: "G", Content: "x", Author: discordAuthor{ID: "U", Bot: true}},
		{ID: "1", ChannelID: "C", GuildID: "G", Content: "x", Author: discordAuthor{ID: "BOT"}},
	} {
		if _, keep = provider.normalize(context.Background(), inbound); keep {
			t.Fatalf("filtered message accepted: %#v", inbound)
		}
	}
	socket := newFakeProviderSocket(0)
	if err := provider.authenticateGateway(context.Background(), socket); err != nil {
		t.Fatal(err)
	}
	var resume map[string]any
	_ = json.Unmarshal(socket.written()[0], &resume)
	if resume["op"] != float64(6) {
		t.Fatalf("resume = %#v", resume)
	}
	provider.clearSession()
	if provider.heartbeatSequence() != nil {
		t.Fatalf("heartbeat sequence = %#v", provider.heartbeatSequence())
	}
}

func TestDiscordValidationAndMalformedGateway(t *testing.T) {
	t.Parallel()
	for _, config := range []DiscordConfig{
		{},
		{BotToken: "x", RequestTimeout: time.Second, MaxAttempts: 6},
		{BotToken: "x", RequestTimeout: time.Second, BaseURL: "http://example.com"},
	} {
		if _, err := NewDiscord(config); !errors.Is(err, ErrInvalid) {
			t.Fatalf("NewDiscord(%#v) = %v", config, err)
		}
	}
	provider, _ := NewDiscord(DiscordConfig{BotToken: "x", RequestTimeout: time.Second})
	if err := provider.Send(context.Background(), Reply{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid Send = %v", err)
	}
	for _, raw := range []string{"", "http://gateway.example", "wss://user@gateway.example", "wss://gateway.example/#fragment"} {
		if _, err := provider.validateGatewayURL(raw); !errors.Is(err, ErrInvalid) {
			t.Fatalf("validateGatewayURL(%q) = %v", raw, err)
		}
	}
	socket := newFakeProviderSocket(1)
	socket.push(map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 10}})
	provider.dial = func(context.Context, string, *http.Client) (providerSocket, error) { return socket, nil }
	if _, _, err := provider.openGateway(context.Background(), "wss://gateway.example?v=10"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("openGateway = %v", err)
	}
	if discordSnowflakeTime("bad").IsZero() || discordSnowflakeTime("1174109847031779328").Year() < 2023 {
		t.Fatal("snowflake timestamps are invalid")
	}
}

func TestDiscordGatewayControlPackets(t *testing.T) {
	t.Parallel()
	provider, _ := NewDiscord(DiscordConfig{
		BotToken: "secret", RequestTimeout: time.Second,
		Client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: ioNopCloser(`{"id":"C","type":0}`)}, nil
		})}, BaseURL: "http://discord.test",
	})
	if provider.Name() != DiscordProvider {
		t.Fatalf("Name = %q", provider.Name())
	}
	provider.botUserID = "BOT"
	socket := newFakeProviderSocket(8)
	socket.push(map[string]any{"op": 0, "s": 4, "t": "READY", "d": map[string]any{"session_id": "S", "resume_gateway_url": "ws://resume.test", "user": map[string]string{"id": "BOT"}}})
	socket.push(map[string]any{"op": 1})
	socket.push(map[string]any{"op": 11})
	socket.push(map[string]any{"op": 0, "s": 5, "t": "MESSAGE_CREATE", "d": map[string]any{
		"id": "1174109847031779328", "channel_id": "C", "content": "hello", "author": map[string]any{"id": "U"},
	}})
	socket.push(map[string]any{"op": 7})
	var submits atomic.Int32
	provider.serveGateway(context.Background(), func(context.Context, Message) error {
		if submits.Add(1) == 1 {
			return ErrBusy
		}
		return nil
	}, socket, discordHello{HeartbeatInterval: 60_000})
	if submits.Load() != 2 || provider.currentSequence() != 5 || provider.nextGateway() != "ws://resume.test?encoding=json&v=10" {
		t.Fatalf("submits=%d sequence=%d gateway=%q", submits.Load(), provider.currentSequence(), provider.nextGateway())
	}
	if len(socket.written()) < 2 {
		t.Fatalf("gateway writes = %q", socket.written())
	}
	provider.setSocket(socket)
	provider.clearSocket(socket)
}

func TestDiscordInvalidSessionClearsResume(t *testing.T) {
	t.Parallel()
	provider, _ := NewDiscord(DiscordConfig{BotToken: "secret", RequestTimeout: time.Second})
	provider.sessionID, provider.sequence = "session", 9
	socket := newFakeProviderSocket(2)
	socket.push(map[string]any{"op": 9, "d": false})
	provider.serveGateway(context.Background(), func(context.Context, Message) error { return nil }, socket, discordHello{HeartbeatInterval: 60_000})
	if provider.sessionID != "" || provider.currentSequence() != 0 {
		t.Fatalf("session=%q sequence=%d", provider.sessionID, provider.currentSequence())
	}
}

func TestDiscordStartAndOpenFailures(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/gateway/bot" {
			_, _ = writer.Write([]byte(`{"url":"ws://gateway.test"}`))
		}
	}))
	defer server.Close()
	provider, _ := NewDiscord(DiscordConfig{BotToken: "secret", RequestTimeout: time.Second, Client: server.Client(), BaseURL: server.URL})
	provider.dial = func(context.Context, string, *http.Client) (providerSocket, error) {
		return nil, errors.New("dial failed")
	}
	if err := provider.Start(context.Background(), func(context.Context, Message) error { return nil }); err == nil {
		t.Fatal("gateway dial failure accepted")
	}
	_ = provider.Close()
	if err := provider.Start(context.Background(), func(context.Context, Message) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Fatalf("Start after close = %v", err)
	}
}

func ioNopCloser(value string) *testReadCloser {
	return &testReadCloser{Reader: strings.NewReader(value)}
}

type testReadCloser struct{ *strings.Reader }

func (reader *testReadCloser) Close() error { return nil }
