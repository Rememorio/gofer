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
)

type fakeProviderSocket struct {
	reads     chan []byte
	closed    chan struct{}
	closeOnce sync.Once
	mu        sync.Mutex
	writes    [][]byte
	writeErr  error
	closeErr  error
}

func newFakeProviderSocket(buffer int) *fakeProviderSocket {
	return &fakeProviderSocket{reads: make(chan []byte, buffer), closed: make(chan struct{})}
}

func (socket *fakeProviderSocket) Read(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-socket.closed:
		return nil, io.EOF
	case payload := <-socket.reads:
		return payload, nil
	}
}

func (socket *fakeProviderSocket) Write(ctx context.Context, payload []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	socket.mu.Lock()
	defer socket.mu.Unlock()
	if socket.writeErr != nil {
		return socket.writeErr
	}
	socket.writes = append(socket.writes, append([]byte(nil), payload...))
	return nil
}

func (socket *fakeProviderSocket) Close() error {
	socket.closeOnce.Do(func() { close(socket.closed) })
	return socket.closeErr
}

func (socket *fakeProviderSocket) push(value any) {
	payload, _ := json.Marshal(value)
	socket.reads <- payload
}

func (socket *fakeProviderSocket) written() [][]byte {
	socket.mu.Lock()
	defer socket.mu.Unlock()
	result := make([][]byte, len(socket.writes))
	for index := range socket.writes {
		result[index] = append([]byte(nil), socket.writes[index]...)
	}
	return result
}

func TestProviderHTTPAndRetryHelpers(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(providerTestHandler))
	defer server.Close()
	var output map[string]string
	if err := requestJSON(context.Background(), server.Client(), http.MethodPost, server.URL+"/ok", "Bearer token", map[string]bool{"x": true}, &output); err != nil || output["value"] != "yes" {
		t.Fatalf("requestJSON() = %#v, %v", output, err)
	}
	for route, wantDelay := range map[string]time.Duration{"/limited": 250 * time.Millisecond, "/body-retry": 500 * time.Millisecond} {
		err := requestJSON(context.Background(), server.Client(), http.MethodGet, server.URL+route, "", nil, nil)
		delay, retry := providerRetryable(err)
		if !retry || delay != wantDelay {
			t.Fatalf("%s retry = %v, %v (%v)", route, retry, delay, err)
		}
	}
	if err := requestJSON(context.Background(), server.Client(), http.MethodGet, server.URL+"/large", "", nil, &output); err == nil {
		t.Fatal("oversized response accepted")
	}
	if err := requestJSON(context.Background(), server.Client(), http.MethodGet, server.URL+"/invalid", "", nil, &output); err == nil {
		t.Fatal("malformed response accepted")
	}
	if err := requestJSON(context.Background(), newProviderClient(time.Second), http.MethodGet, server.URL+"/redirect", "", nil, nil); err == nil {
		t.Fatal("provider redirect accepted")
	}
	attempts := 0
	var delays []time.Duration
	err := retryProvider(context.Background(), 3, func(_ context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		return nil
	}, func() error {
		attempts++
		if attempts < 3 {
			return &providerHTTPError{status: 503}
		}
		return nil
	})
	if err != nil || attempts != 3 || len(delays) != 2 || delays[0] != 200*time.Millisecond || delays[1] != 400*time.Millisecond {
		t.Fatalf("retry = attempts %d delays %v err %v", attempts, delays, err)
	}
	if err = retryProvider(context.Background(), 2, sleepContext, func() error { return ErrInvalid }); !errors.Is(err, ErrInvalid) {
		t.Fatalf("non-retryable = %v", err)
	}
}

func providerTestHandler(writer http.ResponseWriter, request *http.Request) {
	switch request.URL.Path {
	case "/ok":
		_ = json.NewEncoder(writer).Encode(map[string]string{"value": "yes"})
	case "/limited":
		writer.Header().Set("Retry-After", "0.25")
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = writer.Write([]byte(`{"description":"slow down"}`))
	case "/body-retry":
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = writer.Write([]byte(`{"parameters":{"retry_after":0.5}}`))
	case "/large":
		_, _ = writer.Write([]byte(strings.Repeat("x", providerResponseLimit+1)))
	case "/invalid":
		_, _ = writer.Write([]byte("{"))
	case "/redirect":
		http.Redirect(writer, request, "/ok", http.StatusFound)
	}
}

func TestProviderHelpersBoundAndSplit(t *testing.T) {
	t.Parallel()
	if delay := retryAfter(http.Header{"Retry-After": []string{"bad"}}); delay != 0 {
		t.Fatalf("retryAfter = %v", delay)
	}
	message, delay := providerFailureDetails([]byte(`{"message":"busy","retry_after":301}`))
	if message != "busy" || delay != 0 {
		t.Fatalf("failure details = %q %v", message, delay)
	}
	chunks := splitUTF16("ab😀cd ef", 5)
	if strings.Join(chunks, "") != "ab😀cd ef" || len(chunks) < 2 {
		t.Fatalf("chunks = %#v", chunks)
	}
	if chunks = splitUTF16("  ", 10); chunks != nil {
		t.Fatalf("empty chunks = %#v", chunks)
	}
	set := normalizedSet([]string{" a ", "", "b"})
	if !allowed(set, "a") || allowed(set, "c") || !allowed(nil, "anything") {
		t.Fatalf("set = %#v", set)
	}
	if !listed(set, "b") || listed(nil, "anything") {
		t.Fatal("listed lookup is incorrect")
	}
	if providerReconnectDelay(0) != time.Second || providerReconnectDelay(9) != 128*time.Second {
		t.Fatal("reconnect backoff boundary is incorrect")
	}
	if !trustedProviderHost("wss-primary.slack.com", "slack.com") || trustedProviderHost("slack.com.attacker.test", "slack.com") {
		t.Fatal("trusted provider host boundary is incorrect")
	}
	attempts := 0
	if err := submitProviderMessage(context.Background(), func(context.Context, Message) error {
		attempts++
		if attempts == 1 {
			return ErrBusy
		}
		return nil
	}, validMessage()); err != nil || attempts != 2 {
		t.Fatalf("submitProviderMessage = attempts %d, %v", attempts, err)
	}
}

type sourceSender struct {
	fakeSender
	startErr error
	started  chan struct{}
	closed   bool
}

func (sender *sourceSender) Start(ctx context.Context, submit SubmitFunc) error {
	if sender.startErr != nil {
		return sender.startErr
	}
	close(sender.started)
	return submit(ctx, validMessage())
}

func (sender *sourceSender) Close() error {
	sender.closed = true
	return nil
}

func TestManagerOwnsSourceLifecycle(t *testing.T) {
	t.Parallel()
	dispatched := make(chan struct{}, 1)
	manager, _ := NewManager(Config{
		Resolver: resolverFunc(func(context.Context, string, string, string) (Identity, error) { return testIdentity(), nil }),
		Dispatcher: dispatcherFunc(func(context.Context, Request) (Reply, error) {
			dispatched <- struct{}{}
			return Reply{Text: "done"}, nil
		}),
		Dedupe: NewMemoryDedupe(), MaxInflight: 1, DedupeTTL: time.Minute,
	})
	source := &sourceSender{started: make(chan struct{})}
	if err := manager.Register(source); err != nil || manager.Start(context.Background()) != nil {
		t.Fatal(err)
	}
	select {
	case <-dispatched:
	case <-time.After(time.Second):
		t.Fatal("source message was not dispatched")
	}
	if err := manager.Register(&fakeSender{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("late register = %v", err)
	}
	if err := manager.Close(); err != nil || !source.closed {
		t.Fatalf("Close = %v, source closed %v", err, source.closed)
	}
}

func TestManagerClosesAfterSourceStartFailure(t *testing.T) {
	t.Parallel()
	manager, _ := NewManager(Config{
		Resolver:   resolverFunc(func(context.Context, string, string, string) (Identity, error) { return testIdentity(), nil }),
		Dispatcher: dispatcherFunc(func(context.Context, Request) (Reply, error) { return Reply{}, nil }),
		Dedupe:     NewMemoryDedupe(), MaxInflight: 1, DedupeTTL: time.Minute,
	})
	source := &sourceSender{startErr: errors.New("start failed"), started: make(chan struct{})}
	_ = manager.Register(source)
	if err := manager.Start(context.Background()); err == nil || !source.closed {
		t.Fatalf("Start = %v, source closed %v", err, source.closed)
	}
	if err := manager.Submit(context.Background(), validMessage()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Submit = %v", err)
	}
}
