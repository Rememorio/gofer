package channel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-dingtalk/dingtalk-stream-sdk-go/chatbot"
)

type fakeDingTalkStream struct {
	mu       sync.Mutex
	starts   int
	closes   int
	startErr error
}

func (stream *fakeDingTalkStream) Start(context.Context) error {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	stream.starts++
	return stream.startErr
}

func (stream *fakeDingTalkStream) Close() {
	stream.mu.Lock()
	stream.closes++
	stream.mu.Unlock()
}

func TestDingTalkLifecycleAndIngress(t *testing.T) {
	t.Parallel()
	provider, err := NewDingTalk(DingTalkConfig{ClientID: "client", ClientSecret: "secret", AllowedUsers: []string{"staff"}, RequestTimeout: time.Second, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	stream := &fakeDingTalkStream{}
	provider.stream = stream
	var submitted Message
	submit := func(_ context.Context, message Message) error { submitted = message; return nil }
	if err = provider.Start(context.Background(), submit); err != nil {
		t.Fatal(err)
	}
	if err = provider.Start(context.Background(), submit); err != nil || stream.starts != 1 {
		t.Fatalf("second Start = %v, starts %d", err, stream.starts)
	}
	callback := dingTalkTestCallback(dingTalkConversationGroup)
	callback.Content = map[string]any{"items": []any{map[string]any{"text": "caption", "downloadCode": "code/1", "fileName": "  report   2026.pdf  "}}}
	if _, err = provider.handleCallback(context.Background(), callback); err != nil {
		t.Fatal(err)
	}
	if submitted.ID != "msg1" || submitted.WorkspaceID != "conversation1" || submitted.ChatID != "conversation1" || submitted.TopicID != "msg1" || submitted.Text != "hello\ncaption" || len(submitted.Attachments) != 1 {
		t.Fatalf("submitted = %#v", submitted)
	}
	if submitted.Attachments[0].Name != "report 2026.pdf" || submitted.ReceivedAt.UnixMilli() != 1_700_000_000_000 {
		t.Fatalf("attachment/time = %#v, %v", submitted.Attachments, submitted.ReceivedAt)
	}
	if _, exists := provider.routes.Get("msg1"); !exists {
		t.Fatal("reply route not retained")
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

func TestDingTalkOpenAPIGroupDirectAndTokenRefresh(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	tokenCalls, sendCalls := 0, 0
	paths := make([]string, 0, 4)
	bodies := make([]map[string]any, 0, 4)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if request.URL.Path == "/v1.0/oauth2/accessToken" {
			tokenCalls++
			_ = json.NewEncoder(writer).Encode(map[string]any{"accessToken": "token" + string(rune('0'+tokenCalls)), "expireIn": 7200})
			return
		}
		sendCalls++
		paths = append(paths, request.URL.Path)
		var body map[string]any
		_ = json.NewDecoder(request.Body).Decode(&body)
		bodies = append(bodies, body)
		if request.Header.Get("x-acs-dingtalk-access-token") == "token1" {
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = writer.Write([]byte(`{"message":"expired"}`))
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"processQueryKey": "ok"})
	}))
	defer server.Close()
	provider, _ := NewDingTalk(DingTalkConfig{
		ClientID: "client", ClientSecret: "secret", RequestTimeout: time.Second, MaxAttempts: 2,
		Sleep: func(context.Context, time.Duration) error { return nil },
	})
	provider.apiBase, provider.client = server.URL, server.Client()
	provider.routes.Put("group-message", dingTalkRoute{ConversationType: dingTalkConversationGroup, ConversationID: "open-conversation", SenderStaffID: "staff"})
	if err := provider.Send(context.Background(), Reply{Provider: DingTalkProvider, ChatID: "open-conversation", InReplyTo: "group-message", Text: "group reply"}); err != nil {
		t.Fatal(err)
	}
	provider.routes.Put("direct-message", dingTalkRoute{ConversationType: dingTalkConversationP2P, SenderStaffID: "staff"})
	if err := provider.Send(context.Background(), Reply{Provider: DingTalkProvider, ChatID: "staff", InReplyTo: "direct-message", Text: "direct reply"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if tokenCalls != 2 || sendCalls != 3 || paths[0] != "/v1.0/robot/groupMessages/send" || paths[2] != "/v1.0/robot/oToMessages/batchSend" {
		t.Fatalf("calls token/send = %d/%d, paths %#v", tokenCalls, sendCalls, paths)
	}
	if bodies[1]["openConversationId"] != "open-conversation" || len(bodies[1]["atUserIds"].([]any)) != 1 || len(bodies[2]["userIds"].([]any)) != 1 {
		t.Fatalf("bodies = %#v", bodies)
	}
}

func TestDingTalkNormalizeFiltersAndAttachments(t *testing.T) {
	t.Parallel()
	provider, _ := NewDingTalk(DingTalkConfig{ClientID: "client", ClientSecret: "secret", AllowedUsers: []string{"staff"}, RequestTimeout: time.Second, MaxAttempts: 1, Now: func() time.Time { return time.Unix(123, 0) }})
	if provider.Name() != DingTalkProvider {
		t.Fatalf("Name = %q", provider.Name())
	}
	direct := dingTalkTestCallback("unexpected")
	direct.SenderCorpId, direct.CreateAt = "", 0
	direct.Content = []any{map[string]any{"download_code": "image-code", "filename": strings.Repeat("x", 130), "type": "picture"}}
	message, route, keep := provider.normalize(direct)
	if !keep || message.ChatID != "staff" || message.TopicID != "" || message.WorkspaceID != "" || route.ConversationType != dingTalkConversationP2P {
		t.Fatalf("direct normalize = %#v, %#v, %v", message, route, keep)
	}
	if len(message.Attachments) != 1 || message.Attachments[0].MediaType != "image/*" || len([]rune(message.Attachments[0].Name)) != 120 || !message.ReceivedAt.Equal(time.Unix(123, 0).UTC()) {
		t.Fatalf("attachment/time = %#v, %v", message.Attachments, message.ReceivedAt)
	}
	blocked := dingTalkTestCallback(dingTalkConversationP2P)
	blocked.SenderStaffId = "other"
	empty := dingTalkTestCallback(dingTalkConversationP2P)
	empty.Text.Content, empty.Content = "", nil
	for _, callback := range []*chatbot.BotCallbackDataModel{nil, blocked, empty} {
		if _, _, accepted := provider.normalize(callback); accepted {
			t.Fatalf("unexpected accepted callback %#v", callback)
		}
	}
	connecting := dingTalkTestCallback(dingTalkConversationP2P)
	connecting.SenderStaffId, connecting.Text.Content = "other", "/connect code"
	if message, _, accepted := provider.normalize(connecting); !accepted || message.Text != "/connect code" {
		t.Fatalf("disallowed connect = %#v, %v", message, accepted)
	}
	if _, err := provider.handleCallback(context.Background(), dingTalkTestCallback(dingTalkConversationP2P)); !errors.Is(err, ErrClosed) {
		t.Fatalf("callback before Start = %v", err)
	}
}

func TestDingTalkValidationErrorsAndExpiry(t *testing.T) {
	t.Parallel()
	for _, config := range []DingTalkConfig{
		{}, {ClientID: "client", ClientSecret: "secret", RequestTimeout: time.Millisecond},
		{ClientID: "client", ClientSecret: "secret", RequestTimeout: time.Second, MaxAttempts: 6},
	} {
		if _, err := NewDingTalk(config); !errors.Is(err, ErrInvalid) {
			t.Fatalf("NewDingTalk(%#v) = %v", config, err)
		}
	}
	if dingTalkExpires("60") != time.Minute || dingTalkExpires(float64(86400)) != 24*time.Hour || dingTalkExpires("bad") != 2*time.Hour || dingTalkExpires(float64(1)) != 2*time.Hour {
		t.Fatal("expiry boundaries are incorrect")
	}
	provider, _ := NewDingTalk(DingTalkConfig{ClientID: "client", ClientSecret: "secret", RequestTimeout: time.Second, MaxAttempts: 1})
	stream := &fakeDingTalkStream{startErr: errors.New("offline")}
	provider.stream = stream
	if err := provider.Start(context.Background(), func(context.Context, Message) error { return nil }); err == nil || !strings.Contains(err.Error(), "offline") {
		t.Fatalf("Start failure = %v", err)
	}
	for _, invalid := range []Reply{{}, {Provider: DingTalkProvider, ChatID: "staff", Text: "x"}, {Provider: "other", ChatID: "staff", InReplyTo: "m", Text: "x"}} {
		if err := provider.Send(context.Background(), invalid); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Send(%#v) = %v", invalid, err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, `{"expireIn":"120"}`)
	}))
	defer server.Close()
	provider.apiBase, provider.client = server.URL, server.Client()
	if _, err := provider.token(context.Background()); err == nil {
		t.Fatal("empty access token accepted")
	}
	if err := (*DingTalk)(nil).Close(); err != nil {
		t.Fatal(err)
	}
}

func dingTalkTestCallback(conversationType string) *chatbot.BotCallbackDataModel {
	return &chatbot.BotCallbackDataModel{
		ConversationId: "conversation1", ChatbotCorpId: "bot-corp", MsgId: "msg1", SenderNick: "User",
		SenderStaffId: "staff", CreateAt: 1_700_000_000_000, SenderCorpId: "sender-corp",
		ConversationType: conversationType, Text: chatbot.BotCallbackDataTextModel{Content: "hello"}, Msgtype: "text",
	}
}
