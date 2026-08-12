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

func TestTelegramLongPollingLifecycle(t *testing.T) {
	t.Parallel()
	var polls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case strings.HasSuffix(request.URL.Path, "/getMe"):
			_, _ = writer.Write([]byte(`{"ok":true,"result":{"id":99,"is_bot":true}}`))
		case strings.HasSuffix(request.URL.Path, "/getUpdates"):
			if polls.Add(1) == 1 {
				_, _ = writer.Write([]byte(`{"ok":true,"result":[{"update_id":50,"message":{"message_id":12,"date":1700000000,"from":{"id":7,"username":"alice"},"chat":{"id":-9,"type":"group"},"text":"hello","photo":[{"file_id":"small","file_unique_id":"s","file_size":1},{"file_id":"large/id","file_unique_id":"l","file_size":10}]}}]}`))
				return
			}
			<-request.Context().Done()
		}
	}))
	defer server.Close()
	provider, err := NewTelegram(TelegramConfig{
		BotToken: "secret", AllowedUsers: []string{"7"}, PollTimeout: time.Second,
		RequestTimeout: 2 * time.Second, Client: server.Client(), BaseURL: server.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if provider.Name() != TelegramProvider {
		t.Fatalf("Name = %q", provider.Name())
	}
	received := make(chan Message, 1)
	if err = provider.Start(context.Background(), func(_ context.Context, message Message) error {
		received <- message
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case message := <-received:
		if message.ID != "12" || message.WorkspaceID != "-9" || message.ExternalUserID != "7" || message.TopicID != "12" || message.Metadata["username"] != "alice" {
			t.Fatalf("message = %#v", message)
		}
		if len(message.Attachments) != 1 || message.Attachments[0].URL != "telegram://file/large%2Fid" || message.Attachments[0].Size != 10 {
			t.Fatalf("attachments = %#v", message.Attachments)
		}
	case <-time.After(time.Second):
		t.Fatal("Telegram update not submitted")
	}
	if err = provider.Start(context.Background(), func(context.Context, Message) error { return nil }); err != nil {
		t.Fatalf("second Start = %v", err)
	}
	if err = provider.Close(); err != nil {
		t.Fatal(err)
	}
	if err = provider.Start(context.Background(), func(context.Context, Message) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Fatalf("Start after close = %v", err)
	}
}

func TestTelegramPollingRetainsBusyUpdate(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"ok":true,"result":[{"update_id":8,"message":{"message_id":3,"date":1,"from":{"id":2},"chat":{"id":2,"type":"private"},"text":"hello"}}]}`))
	}))
	defer server.Close()
	provider, _ := NewTelegram(TelegramConfig{BotToken: "secret", PollTimeout: time.Second, RequestTimeout: 2 * time.Second, Client: server.Client(), BaseURL: server.URL})
	offset, err := provider.pollOnce(context.Background(), func(context.Context, Message) error { return ErrBusy }, 4)
	if !errors.Is(err, ErrBusy) || offset != 4 {
		t.Fatalf("pollOnce = %d, %v", offset, err)
	}
	offset, err = provider.pollOnce(context.Background(), func(context.Context, Message) error { return nil }, 4)
	if err != nil || offset != 9 {
		t.Fatalf("pollOnce accepted = %d, %v", offset, err)
	}
}

func TestTelegramSendSplitsRepliesAndRetries(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	var mu sync.Mutex
	var inputs []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if calls.Add(1) == 1 {
			writer.WriteHeader(http.StatusTooManyRequests)
			_, _ = writer.Write([]byte(`{"ok":false,"parameters":{"retry_after":0.01}}`))
			return
		}
		var input map[string]any
		_ = json.NewDecoder(request.Body).Decode(&input)
		mu.Lock()
		inputs = append(inputs, input)
		mu.Unlock()
		_, _ = writer.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer server.Close()
	var slept []time.Duration
	provider, _ := NewTelegram(TelegramConfig{
		BotToken: "secret", PollTimeout: time.Second, RequestTimeout: 2 * time.Second,
		MaxAttempts: 2, Client: server.Client(), BaseURL: server.URL,
		Sleep: func(_ context.Context, duration time.Duration) error { slept = append(slept, duration); return nil },
	})
	err := provider.Send(context.Background(), Reply{Provider: TelegramProvider, ChatID: "7", InReplyTo: "42", Text: strings.Repeat("😀", 2050)})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls.Load() != 3 || len(inputs) != 2 || len(slept) != 1 || slept[0] != 10*time.Millisecond {
		t.Fatalf("calls=%d inputs=%d slept=%v", calls.Load(), len(inputs), slept)
	}
	if inputs[0]["reply_parameters"] == nil || inputs[1]["reply_parameters"] != nil {
		t.Fatalf("reply routing = %#v", inputs)
	}
}

func TestTelegramNormalizeAndValidation(t *testing.T) {
	t.Parallel()
	provider, err := NewTelegram(TelegramConfig{BotToken: "x", AllowedUsers: []string{"1"}, PollTimeout: time.Second, RequestTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	message, keep := provider.normalize(telegramUpdate{UpdateID: 1, Message: &telegramMessage{
		MessageID: 2, From: &telegramUser{ID: 1}, Chat: telegramChat{ID: 1, Type: "private"},
		Caption: "file", Document: &telegramFile{FileID: "doc", FileName: "", MimeType: "", FileSize: -1},
	}})
	if !keep || message.TopicID != "" || len(message.Attachments) != 1 || message.Attachments[0].Size != 0 || message.Attachments[0].Name != "telegram-file" {
		t.Fatalf("normalize = %#v, %v", message, keep)
	}
	if _, keep = provider.normalize(telegramUpdate{Message: &telegramMessage{From: &telegramUser{ID: 2}, Chat: telegramChat{ID: 1}, Text: "no"}}); keep {
		t.Fatal("disallowed user accepted")
	}
	if _, keep = provider.normalize(telegramUpdate{Message: &telegramMessage{From: &telegramUser{ID: 1, IsBot: true}}}); keep {
		t.Fatal("bot accepted")
	}
	for _, config := range []TelegramConfig{
		{},
		{BotToken: "x", PollTimeout: time.Second, RequestTimeout: time.Second},
		{BotToken: "x", PollTimeout: 51 * time.Second, RequestTimeout: time.Minute},
		{BotToken: "x", PollTimeout: time.Second, RequestTimeout: 2 * time.Second, BaseURL: "http://example.com"},
	} {
		if _, err = NewTelegram(config); !errors.Is(err, ErrInvalid) {
			t.Fatalf("NewTelegram(%#v) = %v", config, err)
		}
	}
	if err = provider.Send(context.Background(), Reply{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid Send = %v", err)
	}
}

func TestTelegramAPIFailure(t *testing.T) {
	t.Parallel()
	response := telegramResponse[telegramUser]{Description: "bad token"}
	if err := response.err(); err == nil {
		t.Fatal("failed response accepted")
	}
	provider, _ := NewTelegram(TelegramConfig{BotToken: "secret", PollTimeout: time.Second, RequestTimeout: 2 * time.Second, Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial failed")
	})}, BaseURL: "http://provider.invalid"})
	err := provider.verify(context.Background())
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("verify error = %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
