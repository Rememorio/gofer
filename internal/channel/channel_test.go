package channel

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type resolverFunc func(context.Context, string, string, string) (string, error)

func (function resolverFunc) Resolve(ctx context.Context, a, b, c string) (string, error) {
	return function(ctx, a, b, c)
}

type dispatcherFunc func(context.Context, Request) (Reply, error)

func (function dispatcherFunc) Dispatch(ctx context.Context, request Request) (Reply, error) {
	return function(ctx, request)
}

type fakeSender struct {
	mu      sync.Mutex
	replies []Reply
	closed  int
	err     error
}

func (*fakeSender) Name() string { return "test" }
func (sender *fakeSender) Send(_ context.Context, reply Reply) error {
	sender.mu.Lock()
	defer sender.mu.Unlock()
	sender.replies = append(sender.replies, reply)
	return sender.err
}
func (sender *fakeSender) Close() error {
	sender.mu.Lock()
	defer sender.mu.Unlock()
	sender.closed++
	return sender.err
}

func TestManagerAuthenticatesDispatchesAndDeduplicates(t *testing.T) {
	t.Parallel()
	sender := &fakeSender{}
	manager, err := NewManager(Config{Resolver: resolverFunc(func(context.Context, string, string, string) (string, error) { return "user", nil }), Dispatcher: dispatcherFunc(func(_ context.Context, request Request) (Reply, error) {
		if request.ThreadKey != "test:workspace:user:topic" {
			t.Errorf("key=%q", request.ThreadKey)
		}
		request.Message.Metadata["x"] = "changed"
		return Reply{Text: "done"}, nil
	}), Dedupe: NewMemoryDedupe(), MaxInflight: 2, DedupeTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(sender); err != nil {
		t.Fatal(err)
	}
	message := validMessage()
	if err := manager.Handle(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if err := manager.Handle(context.Background(), message); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate=%v", err)
	}
	sender.mu.Lock()
	defer sender.mu.Unlock()
	if len(sender.replies) != 1 || sender.replies[0].Text != "done" || sender.replies[0].ChatID != "chat" {
		t.Fatalf("replies=%#v", sender.replies)
	}
	if message.Metadata["x"] != "y" {
		t.Fatal("message shares metadata")
	}
}

func TestManagerBoundsInflightAndRetriesFailures(t *testing.T) {
	t.Parallel()
	var active, peak atomic.Int32
	release := make(chan struct{})
	sender := &fakeSender{}
	manager, _ := NewManager(Config{Resolver: resolverFunc(func(context.Context, string, string, string) (string, error) { return "u", nil }), Dispatcher: dispatcherFunc(func(ctx context.Context, _ Request) (Reply, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			old := peak.Load()
			if current <= old || peak.CompareAndSwap(old, current) {
				break
			}
		}
		select {
		case <-ctx.Done():
			return Reply{}, ctx.Err()
		case <-release:
			return Reply{Text: "x"}, nil
		}
	}), Dedupe: NewMemoryDedupe(), MaxInflight: 1, DedupeTTL: time.Minute})
	_ = manager.Register(sender)
	first := validMessage()
	second := validMessage()
	second.ID = "2"
	var wait sync.WaitGroup
	for _, message := range []Message{first, second} {
		wait.Add(1)
		go func(message Message) { defer wait.Done(); _ = manager.Handle(context.Background(), message) }(message)
	}
	deadline := time.Now().Add(time.Second)
	for active.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	close(release)
	wait.Wait()
	if peak.Load() != 1 {
		t.Fatalf("peak=%d", peak.Load())
	}
	sender.err = errors.New("send")
	third := validMessage()
	third.ID = "3"
	if err := manager.Handle(context.Background(), third); err == nil {
		t.Fatal("send failure succeeded")
	}
	sender.err = nil
	if err := manager.Handle(context.Background(), third); err != nil {
		t.Fatalf("retry=%v", err)
	}
}

func TestManagerAuthorizationProvidersAndClose(t *testing.T) {
	t.Parallel()
	sender := &fakeSender{}
	manager, _ := NewManager(Config{Resolver: resolverFunc(func(context.Context, string, string, string) (string, error) { return "", errors.New("unbound") }), Dispatcher: dispatcherFunc(func(context.Context, Request) (Reply, error) { return Reply{}, nil }), Dedupe: NewMemoryDedupe(), MaxInflight: 1, DedupeTTL: time.Minute})
	if err := manager.Register(sender); err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(sender); !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate sender=%v", err)
	}
	if providers := manager.Providers(); len(providers) != 1 || providers[0] != "test" {
		t.Fatalf("providers=%#v", providers)
	}
	if err := manager.Handle(context.Background(), validMessage()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("auth=%v", err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if err := manager.Register(sender); err == nil {
		t.Fatal("register closed")
	}
	if sender.closed != 1 {
		t.Fatalf("closed=%d", sender.closed)
	}
}

func TestDedupeValidationAndExpiry(t *testing.T) {
	t.Parallel()
	store := NewMemoryDedupe()
	now := time.Now()
	if claimed, err := store.Begin(context.Background(), "x", now, time.Minute); err != nil || !claimed {
		t.Fatalf("begin=%v %v", claimed, err)
	}
	if claimed, _ := store.Begin(context.Background(), "x", now, time.Minute); claimed {
		t.Fatal("duplicate claimed")
	}
	if err := store.Complete(context.Background(), "x", false); err != nil {
		t.Fatal(err)
	}
	if claimed, _ := store.Begin(context.Background(), "x", now, time.Minute); !claimed {
		t.Fatal("failed claim not released")
	}
	if err := store.Complete(context.Background(), "x", true); err != nil {
		t.Fatal(err)
	}
	if claimed, _ := store.Begin(context.Background(), "x", now.Add(2*time.Minute), time.Minute); !claimed {
		t.Fatal("expired not pruned")
	}
	if _, err := store.Begin(context.Background(), "", now, time.Minute); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid=%v", err)
	}
	if err := store.Complete(context.Background(), "missing", true); !errors.Is(err, ErrInvalid) {
		t.Fatalf("complete=%v", err)
	}
}

func TestMessageAndConfigValidation(t *testing.T) {
	t.Parallel()
	valid := validMessage()
	for _, mutate := range []func(*Message){func(m *Message) { m.ID = "" }, func(m *Message) { m.Text = "" }, func(m *Message) { m.ReceivedAt = time.Time{} }, func(m *Message) { m.Attachments = []Attachment{{}} }} {
		candidate := valid
		mutate(&candidate)
		if err := candidate.Validate(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Validate=%v", err)
		}
	}
	config := Config{Resolver: resolverFunc(func(context.Context, string, string, string) (string, error) { return "u", nil }), Dispatcher: dispatcherFunc(func(context.Context, Request) (Reply, error) { return Reply{}, nil }), Dedupe: NewMemoryDedupe(), MaxInflight: 1, DedupeTTL: time.Minute}
	if _, err := NewManager(config); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Config){func(c *Config) { c.Resolver = nil }, func(c *Config) { c.Dispatcher = nil }, func(c *Config) { c.Dedupe = nil }, func(c *Config) { c.MaxInflight = 0 }, func(c *Config) { c.DedupeTTL = 0 }} {
		candidate := config
		mutate(&candidate)
		if _, err := NewManager(candidate); !errors.Is(err, ErrInvalid) {
			t.Fatalf("New=%v", err)
		}
	}
}

func validMessage() Message {
	return Message{ID: "1", Provider: "test", WorkspaceID: "workspace", ExternalUserID: "external", ChatID: "chat", TopicID: "topic", Text: "hello", Metadata: map[string]string{"x": "y"}, ReceivedAt: time.Now()}
}
