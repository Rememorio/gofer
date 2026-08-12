package channel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWeComSocketLifecycleIngressAndReply(t *testing.T) {
	t.Parallel()
	provider, err := NewWeCom(WeComConfig{
		BotID: "bot", BotSecret: "secret", AllowedUsers: []string{"user"},
		Heartbeat: 5 * time.Second, RequestTimeout: time.Second, MaxAttempts: 1,
		Client: &http.Client{}, SocketURL: "ws://wecom.test", Now: func() time.Time { return time.Unix(123, 0) },
	})
	if err != nil {
		t.Fatal(err)
	}
	socket := newFakeProviderSocket(4)
	provider.dial = func(context.Context, string, *http.Client) (providerSocket, error) { return socket, nil }
	submitted := make(chan Message, 1)
	startWeComTestProvider(t, provider, socket, submitted)
	if err = provider.Start(context.Background(), func(context.Context, Message) error { return nil }); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{
		"msgid": "message-1", "aibotid": "aibot-1", "chattype": "single", "msgtype": "mixed",
		"from": map[string]string{"userid": "user"},
		"mixed": map[string]any{"msg_item": []any{
			map[string]any{"msgtype": "text", "text": map[string]string{"content": "hello"}},
			map[string]any{"msgtype": "image", "image": map[string]string{"url": "https://files.example/image", "aeskey": "secret-key"}},
		}},
	})
	socket.push(weComFrame{Command: weComCallback, Headers: weComHeaders{RequestID: "request-1", ChatID: "platform-chat", MessageID: "header-message"}, Body: body})
	var message Message
	select {
	case message = <-submitted:
	case <-time.After(time.Second):
		t.Fatal("callback not submitted")
	}
	assertWeComMessage(t, message)
	if err = provider.Send(context.Background(), Reply{Provider: WeComProvider, ChatID: "user", InReplyTo: "message-1", Text: "answer"}); err != nil {
		t.Fatal(err)
	}
	assertWeComReplies(t, waitWeComWrite(t, socket, 3))
	socket.push(weComFrame{Command: weComCallback, Headers: weComHeaders{RequestID: "request-1", ChatID: "platform-chat"}, Body: body})
	select {
	case duplicate := <-submitted:
		t.Fatalf("duplicate callback submitted: %#v", duplicate)
	case <-time.After(20 * time.Millisecond):
	}
	if err = provider.Close(); err != nil {
		t.Fatal(err)
	}
	_ = provider.Close()
	if err = provider.Start(context.Background(), func(context.Context, Message) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Fatalf("Start after Close = %v", err)
	}
}

func startWeComTestProvider(t *testing.T, provider *WeCom, socket *fakeProviderSocket, submitted chan<- Message) {
	t.Helper()
	startErr := make(chan error, 1)
	go func() {
		startErr <- provider.Start(context.Background(), func(_ context.Context, message Message) error { submitted <- message; return nil })
	}()
	auth := waitWeComWrite(t, socket, 1)[0]
	var authFrame weComOutboundFrame
	if err := json.Unmarshal(auth, &authFrame); err != nil {
		t.Fatal(err)
	}
	authBody := authFrame.Body.(map[string]any)
	if authFrame.Command != weComSubscribe || authBody["bot_id"] != "bot" || authBody["secret"] != "secret" {
		t.Fatalf("auth = %#v", authFrame)
	}
	zero := 0
	socket.push(weComFrame{Headers: weComHeaders{RequestID: authFrame.Headers.RequestID}, ErrCode: &zero})
	if err := <-startErr; err != nil {
		t.Fatal(err)
	}
}

func assertWeComMessage(t *testing.T, message Message) {
	t.Helper()
	if message.ID != "message-1" || message.WorkspaceID != "aibot-1" || message.ChatID != "user" || message.TopicID != "user" || message.Text != "hello" || len(message.Attachments) != 1 || strings.Contains(message.Attachments[0].URL, "files.example") {
		t.Fatalf("message = %#v", message)
	}
}

func assertWeComReplies(t *testing.T, writes [][]byte) {
	t.Helper()
	var working, reply weComFrame
	if err := json.Unmarshal(writes[1], &working); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(writes[2], &reply); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(working.Body), "Working on it") || !strings.Contains(string(working.Body), `"finish":false`) {
		t.Fatalf("working reply = %#v", working)
	}
	if reply.Command != weComRespondMessage || reply.Headers.RequestID != "request-1" || !strings.Contains(string(reply.Body), "answer") || !strings.Contains(string(reply.Body), `"finish":true`) {
		t.Fatalf("reply = %#v", reply)
	}
}

func TestWeComAuthenticationAndValidation(t *testing.T) {
	t.Parallel()
	for _, config := range []WeComConfig{
		{}, {BotID: "bot", BotSecret: "secret", SocketURL: "ws://openws.work.weixin.qq.com"},
		{BotID: "bot", BotSecret: "secret", SocketURL: "wss://attacker.example"},
		{BotID: "bot", BotSecret: "secret", Heartbeat: time.Second},
		{BotID: "bot", BotSecret: "secret", AllowedUsers: []string{"same", "same"}},
	} {
		if _, err := NewWeCom(config); !errors.Is(err, ErrInvalid) {
			t.Fatalf("NewWeCom(%#v) = %v", config, err)
		}
	}
	provider, _ := NewWeCom(WeComConfig{BotID: "bot", BotSecret: "secret", Heartbeat: 5 * time.Second, RequestTimeout: time.Second, MaxAttempts: 1, Client: &http.Client{}, SocketURL: "ws://wecom.test"})
	if provider.Name() != WeComProvider {
		t.Fatalf("Name = %q", provider.Name())
	}
	provider.dial = func(context.Context, string, *http.Client) (providerSocket, error) { return nil, errors.New("offline") }
	if err := provider.Start(context.Background(), func(context.Context, Message) error { return nil }); err == nil || !strings.Contains(err.Error(), "offline") {
		t.Fatalf("dial failure = %v", err)
	}
	socket := newFakeProviderSocket(1)
	provider.dial = func(context.Context, string, *http.Client) (providerSocket, error) { return socket, nil }
	result := make(chan error, 1)
	go func() {
		result <- provider.Start(context.Background(), func(context.Context, Message) error { return nil })
	}()
	auth := waitWeComWrite(t, socket, 1)[0]
	var frame weComFrame
	_ = json.Unmarshal(auth, &frame)
	code := 40001
	socket.push(weComFrame{Headers: weComHeaders{RequestID: frame.Headers.RequestID}, ErrCode: &code, ErrMsg: "bad secret"})
	if err := <-result; err == nil || !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("auth failure = %v", err)
	}
	if err := (*WeCom)(nil).Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWeComNormalizationVariantsAndSendErrors(t *testing.T) {
	t.Parallel()
	provider, _ := NewWeCom(WeComConfig{BotID: "bot", BotSecret: "secret", AllowedUsers: []string{"user"}, Heartbeat: 5 * time.Second, RequestTimeout: time.Second, MaxAttempts: 1})
	provider.now = func() time.Time { return time.Unix(100, 0) }
	for _, test := range []struct {
		name  string
		body  map[string]any
		text  string
		files int
	}{
		{name: "text quote", body: map[string]any{"msgtype": "text", "text": map[string]string{"content": "question"}, "quote": map[string]any{"text": map[string]string{"content": "prior"}}}, text: "question\n\nQuote message: prior"},
		{name: "voice", body: map[string]any{"msgtype": "voice", "voice": map[string]string{"content": "transcript"}}, text: "transcript"},
		{name: "file", body: map[string]any{"msgtype": "file", "file": map[string]string{"url": "https://files.example/f", "filename": " file.txt "}}, files: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.body["msgid"], test.body["aibotid"], test.body["from"] = "m", "aibot", map[string]string{"userid": "user"}
			raw, _ := json.Marshal(test.body)
			message, route, keep := provider.normalize(weComFrame{Headers: weComHeaders{RequestID: "r"}, Body: raw})
			if !keep || message.Text != test.text || len(message.Attachments) != test.files || route.RequestID != "r" {
				t.Fatalf("normalize = %#v, %#v, %v", message, route, keep)
			}
		})
	}
	for _, body := range []any{nil, map[string]any{"msgid": "m", "from": map[string]string{"userid": "blocked"}, "msgtype": "text", "text": map[string]string{"content": "x"}}, map[string]any{"msgid": "m", "from": map[string]string{"userid": "user"}, "msgtype": "text"}} {
		raw, _ := json.Marshal(body)
		if _, _, keep := provider.normalize(weComFrame{Body: raw}); keep {
			t.Fatalf("unexpected accepted body %#v", body)
		}
	}
	for _, reply := range []Reply{{}, {Provider: WeComProvider, ChatID: "user", Text: "x"}, {Provider: "other", ChatID: "user", InReplyTo: "m", Text: "x"}} {
		if err := provider.Send(context.Background(), reply); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Send(%#v) = %v", reply, err)
		}
	}
	provider.routes.Put("m", weComRoute{RequestID: "request", ChatID: "chat"})
	if err := provider.Send(context.Background(), Reply{Provider: WeComProvider, ChatID: "user", InReplyTo: "m", Text: "x"}); err == nil {
		t.Fatal("send without socket succeeded")
	}
	socket := newFakeProviderSocket(1)
	provider.mu.Lock()
	provider.socket = socket
	provider.mu.Unlock()
	provider.routes.Put("m2", weComRoute{RequestID: "request-2", ChatID: "platform-chat", StreamID: "stream-2"})
	if err := provider.Send(context.Background(), Reply{Provider: WeComProvider, ChatID: "user", InReplyTo: "m2", Text: strings.Repeat("a", weComTextMax+1)}); err != nil {
		t.Fatal(err)
	}
	writes := waitWeComWrite(t, socket, 2)
	var final, overflow weComFrame
	_ = json.Unmarshal(writes[0], &final)
	_ = json.Unmarshal(writes[1], &overflow)
	if final.Command != weComRespondMessage || overflow.Command != weComSendMessage || !strings.Contains(string(overflow.Body), `"chatid":"platform-chat"`) {
		t.Fatalf("split frames = %#v / %#v", final, overflow)
	}
}

func TestWeComReconnectsAfterReadFailure(t *testing.T) {
	t.Parallel()
	provider, _ := NewWeCom(WeComConfig{
		BotID: "bot", BotSecret: "secret", Heartbeat: 5 * time.Second, RequestTimeout: time.Second, MaxAttempts: 1,
		Client: &http.Client{}, SocketURL: "ws://wecom.test", Sleep: func(context.Context, time.Duration) error { return nil },
	})
	first, second := newFakeProviderSocket(2), newFakeProviderSocket(2)
	var mu sync.Mutex
	dials := 0
	provider.dial = func(context.Context, string, *http.Client) (providerSocket, error) {
		mu.Lock()
		defer mu.Unlock()
		dials++
		if dials == 1 {
			return first, nil
		}
		return second, nil
	}
	start := make(chan error, 1)
	go func() {
		start <- provider.Start(context.Background(), func(context.Context, Message) error { return nil })
	}()
	authenticateWeComSocket(t, first)
	if err := <-start; err != nil {
		t.Fatal(err)
	}
	_ = first.Close()
	authenticateWeComSocket(t, second)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		provider.mu.Lock()
		current := provider.socket
		provider.mu.Unlock()
		if current == second {
			break
		}
		time.Sleep(time.Millisecond)
	}
	provider.mu.Lock()
	current := provider.socket
	provider.mu.Unlock()
	if current != second {
		t.Fatal("reconnected socket was not installed")
	}
	if err := provider.Close(); err != nil {
		t.Fatal(err)
	}
}

func authenticateWeComSocket(t *testing.T, socket *fakeProviderSocket) {
	t.Helper()
	writes := waitWeComWrite(t, socket, 1)
	var frame weComFrame
	if err := json.Unmarshal(writes[0], &frame); err != nil {
		t.Fatal(err)
	}
	zero := 0
	socket.push(weComFrame{Headers: weComHeaders{RequestID: frame.Headers.RequestID}, ErrCode: &zero})
}

func waitWeComWrite(t *testing.T, socket *fakeProviderSocket, count int) [][]byte {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if writes := socket.written(); len(writes) >= count {
			return writes
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d writes", count)
	return nil
}
