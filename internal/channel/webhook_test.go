package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const webhookTestSecret = "test-webhook-secret-at-least-24-bytes"

type webhookCaptureSender struct {
	replies chan Reply
	closed  atomic.Int32
}

func (*webhookCaptureSender) Name() string { return WebhookProvider }
func (sender *webhookCaptureSender) Send(_ context.Context, reply Reply) error {
	sender.replies <- reply
	return nil
}
func (sender *webhookCaptureSender) Close() error {
	sender.closed.Add(1)
	return nil
}

func TestWebhookHandlerAuthenticatesQueuesAndRoutes(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0).UTC()
	state := NewMemoryState()
	binding, _ := NewBinding("alice", WebhookProvider, "workspace", "external", now)
	_, _ = state.Bind(context.Background(), binding)
	dispatched := make(chan Request, 1)
	manager, err := NewManager(Config{
		Resolver: state, Dedupe: state, MaxInflight: 1, QueueCapacity: 1, DedupeTTL: time.Hour,
		Dispatcher: dispatcherFunc(func(_ context.Context, request Request) (Reply, error) {
			dispatched <- request
			return Reply{Text: "answer"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	sender := &webhookCaptureSender{replies: make(chan Reply, 1)}
	if err = manager.Register(sender); err != nil {
		t.Fatal(err)
	}
	if err = manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	handler, err := NewWebhookHandler(WebhookHandlerConfig{Manager: manager, Secret: webhookTestSecret, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"id":"event-1","external_user_id":"external","chat_id":"chat","topic_id":"topic","text":"hello","metadata":{"event":"message"}}`)
	request := signedWebhookRequest(t, now, body)
	request.SetPathValue("workspace_id", "workspace")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || !bytes.Contains(response.Body.Bytes(), []byte(`"accepted":true`)) {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	select {
	case got := <-dispatched:
		if got.Identity.UserID != "alice" || got.Message.Metadata["event"] != "message" {
			t.Fatalf("dispatch = %#v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("message was not dispatched")
	}
	select {
	case reply := <-sender.replies:
		if reply.Text != "answer" || reply.ChatID != "chat" || reply.InReplyTo != "event-1" || reply.Provider != WebhookProvider {
			t.Fatalf("reply = %#v", reply)
		}
	case <-time.After(time.Second):
		t.Fatal("reply was not sent")
	}
}

func TestWebhookHandlerRejectsInvalidBoundaries(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0).UTC()
	state := NewMemoryState()
	manager, _ := NewManager(Config{
		Resolver: state, Dedupe: state, MaxInflight: 1, QueueCapacity: 1, DedupeTTL: time.Minute,
		Dispatcher: dispatcherFunc(func(context.Context, Request) (Reply, error) { return Reply{}, nil }),
	})
	_ = manager.Start(context.Background())
	t.Cleanup(func() { _ = manager.Close() })
	handler, _ := NewWebhookHandler(WebhookHandlerConfig{Manager: manager, Secret: webhookTestSecret, MaxBodyBytes: 1024, Now: func() time.Time { return now }})
	valid := []byte(`{"id":"one","external_user_id":"u","chat_id":"c","text":"hello"}`)
	tests := []struct {
		name   string
		make   func() *http.Request
		status int
	}{
		{name: "method", make: func() *http.Request {
			return httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
		}, status: http.StatusMethodNotAllowed},
		{name: "media", make: func() *http.Request {
			request := signedWebhookRequest(t, now, valid)
			request.Header.Set("Content-Type", "text/plain")
			return request
		}, status: http.StatusUnsupportedMediaType},
		{name: "missing signature", make: func() *http.Request {
			request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/", bytes.NewReader(valid))
			request.Header.Set("Content-Type", "application/json")
			return request
		}, status: http.StatusUnauthorized},
		{name: "stale", make: func() *http.Request { return signedWebhookRequest(t, now.Add(-time.Hour), valid) }, status: http.StatusUnauthorized},
		{name: "tampered", make: func() *http.Request {
			request := signedWebhookRequest(t, now, valid)
			request.Body = io.NopCloser(bytes.NewReader(append(valid, ' ')))
			return request
		}, status: http.StatusUnauthorized},
		{name: "unknown field", make: func() *http.Request { return signedWebhookRequest(t, now, []byte(`{"unknown":true}`)) }, status: http.StatusBadRequest},
		{name: "invalid message", make: func() *http.Request { return signedWebhookRequest(t, now, []byte(`{"id":"one"}`)) }, status: http.StatusBadRequest},
		{name: "too large", make: func() *http.Request { return signedWebhookRequest(t, now, bytes.Repeat([]byte("x"), 1025)) }, status: http.StatusRequestEntityTooLarge},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, test.make())
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.status, response.Body.String())
			}
		})
	}
	if _, err := NewWebhookHandler(WebhookHandlerConfig{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("NewWebhookHandler(invalid) = %v", err)
	}
}

func TestWebhookSenderSignsRetriesAndStopsOnPermanentFailure(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0).UTC()
	var calls atomic.Int32
	var mu sync.Mutex
	var received Reply
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		if err := verifyWebhook([]byte(webhookTestSecret), request.Header, body, now, time.Minute); err != nil {
			t.Errorf("signature = %v", err)
		}
		if request.Header.Get("Idempotency-Key") != "event-1" {
			t.Errorf("idempotency key = %q", request.Header.Get("Idempotency-Key"))
		}
		mu.Lock()
		_ = json.Unmarshal(body, &received)
		mu.Unlock()
		if calls.Add(1) == 1 {
			writer.WriteHeader(http.StatusBadGateway)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	sender, err := NewWebhookSender(WebhookSenderConfig{
		Endpoint: server.URL, Secret: webhookTestSecret, AllowPrivateAddresses: true,
		Timeout: time.Second, MaxAttempts: 3, Now: func() time.Time { return now },
		Sleep: func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = sender.Send(context.Background(), Reply{Provider: WebhookProvider, ChatID: "chat", InReplyTo: "event-1", Text: "answer"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := received
	mu.Unlock()
	if calls.Load() != 2 || got.Text != "answer" || got.ChatID != "chat" {
		t.Fatalf("calls/reply = %d / %#v", calls.Load(), got)
	}
	if err = sender.Close(); err != nil || sender.Close() != nil {
		t.Fatalf("Close() = %v", err)
	}

	permanentCalls := atomic.Int32{}
	permanent := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		permanentCalls.Add(1)
		writer.WriteHeader(http.StatusBadRequest)
	}))
	defer permanent.Close()
	failed, _ := NewWebhookSender(WebhookSenderConfig{
		Endpoint: permanent.URL, Secret: webhookTestSecret, AllowPrivateAddresses: true,
		Timeout: time.Second, MaxAttempts: 3, Now: func() time.Time { return now },
	})
	defer func() { _ = failed.Close() }()
	if err = failed.Send(context.Background(), Reply{Provider: WebhookProvider, ChatID: "chat", Text: "answer"}); err == nil || permanentCalls.Load() != 1 {
		t.Fatalf("permanent failure = %v, calls=%d", err, permanentCalls.Load())
	}
}

func TestWebhookSenderValidationAndCancellation(t *testing.T) {
	t.Parallel()
	for _, config := range []WebhookSenderConfig{{}, {Endpoint: "ftp://example.com", Secret: webhookTestSecret}, {Endpoint: "https://example.com", Secret: "short"}, {Endpoint: "https://example.com", Secret: webhookTestSecret, Timeout: time.Millisecond}, {Endpoint: "https://example.com", Secret: webhookTestSecret, MaxAttempts: 6}} {
		if _, err := NewWebhookSender(config); !errors.Is(err, ErrInvalid) {
			t.Fatalf("NewWebhookSender(%#v) = %v", config, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepContext(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("sleepContext() = %v", err)
	}
	var nilSender *WebhookSender
	if err := nilSender.Close(); err != nil {
		t.Fatal(err)
	}
}

func signedWebhookRequest(t *testing.T, at time.Time, body []byte) *http.Request {
	t.Helper()
	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/channels/webhook/workspace/events", bytes.NewReader(body))
	timestamp := strconv.FormatInt(at.Unix(), 10)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(WebhookTimestampHeader, timestamp)
	request.Header.Set(WebhookSignatureHeader, signWebhook([]byte(webhookTestSecret), timestamp, body))
	return request
}
