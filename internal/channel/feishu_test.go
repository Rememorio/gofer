package channel

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

type fakeFeishuStream struct {
	mu       sync.Mutex
	starts   int
	closes   int
	startErr error
}

func (stream *fakeFeishuStream) Start(context.Context) error {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	stream.starts++
	return stream.startErr
}

func (stream *fakeFeishuStream) Close() {
	stream.mu.Lock()
	stream.closes++
	stream.mu.Unlock()
}

type fakeFeishuMessenger struct {
	mu       sync.Mutex
	calls    []Reply
	keys     []string
	failures int
}

func (messenger *fakeFeishuMessenger) Send(_ context.Context, reply Reply, key string) error {
	messenger.mu.Lock()
	defer messenger.mu.Unlock()
	messenger.calls = append(messenger.calls, reply)
	messenger.keys = append(messenger.keys, key)
	if messenger.failures > 0 {
		messenger.failures--
		return &providerHTTPError{status: 503}
	}
	return nil
}

func TestFeishuLifecycleAndIngress(t *testing.T) {
	t.Parallel()
	provider, err := NewFeishu(FeishuConfig{AppID: "app", AppSecret: "secret", AllowedUsers: []string{"ou_user"}, RequestTimeout: time.Second, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	stream := &fakeFeishuStream{}
	provider.stream = stream
	var submitted Message
	submit := func(_ context.Context, message Message) error {
		submitted = message
		return nil
	}
	if err = provider.Start(context.Background(), submit); err != nil {
		t.Fatal(err)
	}
	if err = provider.Start(context.Background(), submit); err != nil || stream.starts != 1 {
		t.Fatalf("second Start = %v, starts %d", err, stream.starts)
	}
	event := feishuTestEvent("ou_user", "m1", "chat1", "group", `{"text":"hello","nested":[{"image_key":"img/key"},{"file_key":"file-key"}]}`)
	event.Event.Message.RootId = feishuString("root1")
	if err = provider.handleEvent(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if submitted.ID != "m1" || submitted.TopicID != "root1" || submitted.Text != "hello" || len(submitted.Attachments) != 2 {
		t.Fatalf("submitted = %#v", submitted)
	}
	if submitted.ReceivedAt.UnixMilli() != 1_700_000_000_000 || submitted.Metadata["event_id"] != "event1" {
		t.Fatalf("metadata/timestamp = %#v, %v", submitted.Metadata, submitted.ReceivedAt)
	}
	if err = provider.Close(); err != nil {
		t.Fatal(err)
	}
	_ = provider.Close()
	if stream.closes != 1 {
		t.Fatalf("closes = %d", stream.closes)
	}
	if err = provider.Start(context.Background(), submit); !errors.Is(err, ErrClosed) {
		t.Fatalf("Start after Close = %v", err)
	}
}

func TestFeishuSendSplitsRetriesAndKeys(t *testing.T) {
	t.Parallel()
	provider, _ := NewFeishu(FeishuConfig{
		AppID: "app", AppSecret: "secret", RequestTimeout: time.Second, MaxAttempts: 2,
		Sleep: func(context.Context, time.Duration) error { return nil },
	})
	messenger := &fakeFeishuMessenger{failures: 1}
	provider.messenger = messenger
	reply := Reply{Provider: FeishuProvider, WorkspaceID: "tenant", ChatID: "chat", TopicID: "topic", InReplyTo: "message", Text: strings.Repeat("界", feishuTextMax+1)}
	if err := provider.Send(context.Background(), reply); err != nil {
		t.Fatal(err)
	}
	if len(messenger.calls) != 3 || len(messenger.calls[0].Text) != feishuTextMax*len("界") || messenger.calls[2].Text != "界" {
		t.Fatalf("calls = %d, chunk bytes = %d/%q", len(messenger.calls), len(messenger.calls[0].Text), messenger.calls[len(messenger.calls)-1].Text)
	}
	if messenger.keys[0] != messenger.keys[1] || messenger.keys[1] == messenger.keys[2] || len(messenger.keys[0]) != 64 {
		t.Fatalf("idempotency keys = %#v", messenger.keys)
	}
	for _, invalid := range []Reply{{}, {Provider: "other", ChatID: "chat", Text: "x"}, {Provider: FeishuProvider, Text: "x"}} {
		if err := provider.Send(context.Background(), invalid); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Send(%#v) = %v", invalid, err)
		}
	}
}

func TestFeishuNormalizationFiltersAndContent(t *testing.T) {
	t.Parallel()
	provider, _ := NewFeishu(FeishuConfig{AppID: "app", AppSecret: "secret", AllowedUsers: []string{"allowed"}, RequestTimeout: time.Second, MaxAttempts: 1})
	if provider.Name() != FeishuProvider {
		t.Fatalf("Name = %q", provider.Name())
	}
	p2p := feishuTestEvent("allowed", "m2", "chat2", "p2p", `{"content":[[{"tag":"text","text":" first "},{"tag":"text","text":"second"}]]}`)
	message, keep := provider.normalize(p2p)
	if !keep || message.TopicID != "" || message.Text != "first\n\nsecond" {
		t.Fatalf("p2p normalize = %#v, %v", message, keep)
	}
	bot := feishuTestEvent("allowed", "m", "c", "p2p", `{"text":"x"}`)
	bot.Event.Sender.SenderType = feishuString("bot")
	blocked := feishuTestEvent("blocked", "m", "c", "p2p", `{"text":"x"}`)
	empty := feishuTestEvent("allowed", "m", "c", "p2p", `{}`)
	for _, event := range []*larkim.P2MessageReceiveV1{nil, {}, bot, blocked, empty} {
		if _, accepted := provider.normalize(event); accepted {
			t.Fatalf("unexpected accepted event %#v", event)
		}
	}
	if text, attachments := parseFeishuContent("not-json"); text != "" || attachments != nil {
		t.Fatalf("invalid content = %q, %#v", text, attachments)
	}
	fallback := time.Unix(100, 0)
	if got := unixMilliseconds("bad", fallback); !got.Equal(fallback.UTC()) {
		t.Fatalf("fallback time = %v", got)
	}
	if got := firstValue(nil, feishuString(" value ")); got != "value" || value(nil) != "" {
		t.Fatalf("pointer helpers = %q", got)
	}
}

func TestFeishuValidationAndFailures(t *testing.T) {
	t.Parallel()
	for _, config := range []FeishuConfig{
		{}, {AppID: "app", AppSecret: "secret", Domain: "http://open.feishu.cn"},
		{AppID: "app", AppSecret: "secret", Domain: "https://example.com"},
		{AppID: "app", AppSecret: "secret", RequestTimeout: time.Millisecond},
		{AppID: "app", AppSecret: "secret", RequestTimeout: time.Second, MaxAttempts: 6},
	} {
		if _, err := NewFeishu(config); !errors.Is(err, ErrInvalid) {
			t.Fatalf("NewFeishu(%#v) = %v", config, err)
		}
	}
	provider, _ := NewFeishu(FeishuConfig{AppID: "app", AppSecret: "secret", RequestTimeout: time.Second, MaxAttempts: 1})
	stream := &fakeFeishuStream{startErr: errors.New("offline")}
	provider.stream = stream
	if err := provider.Start(context.Background(), func(context.Context, Message) error { return nil }); err == nil || !strings.Contains(err.Error(), "offline") {
		t.Fatalf("Start failure = %v", err)
	}
	if err := provider.handleEvent(context.Background(), feishuTestEvent("user", "m", "c", "p2p", `{"text":"x"}`)); !errors.Is(err, ErrClosed) {
		t.Fatalf("event before Start = %v", err)
	}
	messenger := &fakeFeishuMessenger{failures: 1}
	provider.messenger = messenger
	if err := provider.Send(context.Background(), Reply{Provider: FeishuProvider, ChatID: "c", Text: "x"}); err == nil {
		t.Fatal("messenger failure was ignored")
	}
	if err := (*Feishu)(nil).Close(); err != nil {
		t.Fatal(err)
	}
	if larkResponseError(nil) == nil {
		t.Fatal("nil Lark response accepted")
	}
}

func feishuTestEvent(userID, messageID, chatID, chatType, content string) *larkim.P2MessageReceiveV1 {
	return &larkim.P2MessageReceiveV1{
		EventV2Base: &larkevent.EventV2Base{Header: &larkevent.EventHeader{EventID: "event1", TenantKey: "tenant1"}},
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{SenderId: &larkim.UserId{OpenId: feishuString(userID)}, SenderType: feishuString("user")},
			Message: &larkim.EventMessage{
				MessageId: feishuString(messageID), ChatId: feishuString(chatID), ChatType: feishuString(chatType),
				MessageType: feishuString("text"), Content: feishuString(content), CreateTime: feishuString("1700000000000"),
			},
		},
	}
}

func feishuString(value string) *string { return &value }
