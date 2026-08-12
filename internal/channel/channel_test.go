package channel

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type resolverFunc func(context.Context, string, string, string) (Identity, error)

func (function resolverFunc) Resolve(ctx context.Context, a, b, c string) (Identity, error) {
	return function(ctx, a, b, c)
}

type dispatcherFunc func(context.Context, Request) (Reply, error)

func (function dispatcherFunc) Dispatch(ctx context.Context, request Request) (Reply, error) {
	return function(ctx, request)
}

type fakeSender struct {
	mu      sync.Mutex
	name    string
	replies []Reply
	closed  int
	err     error
}

func (sender *fakeSender) Name() string {
	if sender.name != "" {
		return sender.name
	}
	return "test"
}
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
	manager, err := NewManager(Config{Resolver: resolverFunc(func(context.Context, string, string, string) (Identity, error) { return testIdentity(), nil }), Dispatcher: dispatcherFunc(func(_ context.Context, request Request) (Reply, error) {
		if request.ThreadKey != "test\xffworkspace\xffchn_00000000000000000000000000000000\xffchat\xfftopic" {
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
	manager, _ := NewManager(Config{Resolver: resolverFunc(func(context.Context, string, string, string) (Identity, error) { return testIdentity(), nil }), Dispatcher: dispatcherFunc(func(ctx context.Context, _ Request) (Reply, error) {
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
	manager, _ := NewManager(Config{Resolver: resolverFunc(func(context.Context, string, string, string) (Identity, error) {
		return Identity{}, errors.New("unbound")
	}), Dispatcher: dispatcherFunc(func(context.Context, Request) (Reply, error) { return Reply{}, nil }), Dedupe: NewMemoryDedupe(), MaxInflight: 1, DedupeTTL: time.Minute})
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

func TestManagerStartsBoundedQueueAndStopsWorkers(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})
	processed := make(chan string, 2)
	manager, err := NewManager(Config{
		Resolver: resolverFunc(func(context.Context, string, string, string) (Identity, error) { return testIdentity(), nil }),
		Dispatcher: dispatcherFunc(func(ctx context.Context, request Request) (Reply, error) {
			select {
			case <-started:
			default:
				close(started)
			}
			select {
			case <-ctx.Done():
				return Reply{}, ctx.Err()
			case <-release:
				processed <- request.Message.ID
				return Reply{Text: "done"}, nil
			}
		}),
		Dedupe: NewMemoryDedupe(), MaxInflight: 1, QueueCapacity: 1, DedupeTTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	mustStartManager(t, manager)
	mustSubmitMessage(t, manager, validMessage())
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	second := validMessage()
	second.ID = "2"
	mustSubmitMessage(t, manager, second)
	third := validMessage()
	third.ID = "3"
	if err = manager.Submit(context.Background(), third); !errors.Is(err, ErrBusy) {
		t.Fatalf("full queue Submit() = %v", err)
	}
	stats := manager.Stats()
	if !stats.Running || stats.QueueDepth != 1 || stats.QueueCapacity != 1 || stats.MaxInflight != 1 {
		t.Fatalf("Stats() = %#v", stats)
	}
	close(release)
	for range 2 {
		select {
		case <-processed:
		case <-time.After(time.Second):
			t.Fatal("queued message did not complete")
		}
	}
	if err = manager.Close(); err != nil {
		t.Fatal(err)
	}
	if err = manager.Submit(context.Background(), third); !errors.Is(err, ErrClosed) {
		t.Fatalf("Submit(closed) = %v", err)
	}
}

func mustStartManager(t *testing.T, manager *Manager) {
	t.Helper()
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatalf("second Start() = %v", err)
	}
}

func mustSubmitMessage(t *testing.T, manager *Manager, message Message) {
	t.Helper()
	if err := manager.Submit(context.Background(), message); err != nil {
		t.Fatal(err)
	}
}

func TestManagerSerializesOneConversationWithoutBlockingOthers(t *testing.T) {
	t.Parallel()
	var activeSame, peakSame atomic.Int32
	entered := make(chan string, 3)
	releaseSame := make(chan struct{})
	manager, _ := NewManager(Config{
		Resolver: resolverFunc(func(_ context.Context, _, _, external string) (Identity, error) {
			identity := testIdentity()
			if external == "other" {
				identity.BindingID = "chn_11111111111111111111111111111111"
			}
			return identity, nil
		}),
		Dispatcher: dispatcherFunc(func(ctx context.Context, request Request) (Reply, error) {
			if request.Message.ExternalUserID == "other" {
				entered <- "other"
				return Reply{Text: "done"}, nil
			}
			current := activeSame.Add(1)
			defer activeSame.Add(-1)
			for old := peakSame.Load(); current > old && !peakSame.CompareAndSwap(old, current); old = peakSame.Load() {
			}
			entered <- request.Message.ID
			select {
			case <-ctx.Done():
				return Reply{}, ctx.Err()
			case <-releaseSame:
				return Reply{Text: "done"}, nil
			}
		}),
		Dedupe: NewMemoryDedupe(), MaxInflight: 3, DedupeTTL: time.Minute,
	})
	_ = manager.Register(&fakeSender{})
	messages := []Message{validMessage(), validMessage(), validMessage()}
	messages[1].ID = "2"
	messages[2].ID, messages[2].ExternalUserID, messages[2].ChatID = "3", "other", "other-chat"
	var wait sync.WaitGroup
	for _, message := range messages {
		wait.Add(1)
		go func(message Message) { defer wait.Done(); _ = manager.Handle(context.Background(), message) }(message)
	}
	seenOther := false
	deadline := time.After(time.Second)
	for count := 0; count < 2; count++ {
		select {
		case value := <-entered:
			seenOther = seenOther || value == "other"
		case <-deadline:
			t.Fatal("dispatches did not enter")
		}
	}
	if !seenOther {
		t.Fatal("different conversation was blocked")
	}
	close(releaseSame)
	wait.Wait()
	if peakSame.Load() != 1 {
		t.Fatalf("same-conversation peak = %d", peakSame.Load())
	}
}

func TestManagerSendsUnauthorizedConnectionHint(t *testing.T) {
	t.Parallel()
	sender := &fakeSender{}
	manager, _ := NewManager(Config{
		Resolver:   resolverFunc(func(context.Context, string, string, string) (Identity, error) { return Identity{}, ErrNotFound }),
		Dispatcher: dispatcherFunc(func(context.Context, Request) (Reply, error) { return Reply{}, nil }),
		Dedupe:     NewMemoryDedupe(), MaxInflight: 1, DedupeTTL: time.Minute,
		UnauthorizedReply: "connect first",
	})
	_ = manager.Register(sender)
	if err := manager.Handle(context.Background(), validMessage()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Handle() = %v", err)
	}
	sender.mu.Lock()
	defer sender.mu.Unlock()
	if len(sender.replies) != 1 || sender.replies[0].Text != "connect first" || sender.replies[0].InReplyTo != "1" {
		t.Fatalf("replies = %#v", sender.replies)
	}
}

func TestManagerConsumesConnectionCommandsBeforeAuthorization(t *testing.T) {
	t.Parallel()
	state := NewMemoryState()
	now := time.Now().UTC()
	code, err := state.IssueConnectCode(context.Background(), "alice", TelegramProvider, now, ConnectCodeTTL, MaxPendingConnectCodes)
	if err != nil {
		t.Fatal(err)
	}
	sender := &fakeSender{name: TelegramProvider}
	manager, err := NewManager(Config{
		Resolver: resolverFunc(func(context.Context, string, string, string) (Identity, error) {
			t.Fatal("connect attempted identity resolution")
			return Identity{}, ErrNotFound
		}),
		Dispatcher: dispatcherFunc(func(context.Context, Request) (Reply, error) {
			t.Fatal("connect reached dispatcher")
			return Reply{}, nil
		}),
		Dedupe: state, Connector: state, MaxInflight: 1, DedupeTTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = manager.Register(sender); err != nil {
		t.Fatal(err)
	}
	message := validMessage()
	message.Provider, message.Text = TelegramProvider, "/connect "+code.Code
	message.Metadata = map[string]string{"username": "Alice", "workspace_name": "Test"}
	if err = manager.Handle(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	resolved, err := state.Resolve(context.Background(), TelegramProvider, "workspace", "external")
	if err != nil || resolved.UserID != "alice" {
		t.Fatalf("Resolve() = %#v, %v", resolved, err)
	}
	message.ID, message.Text = "2", "/connect invalid"
	if err = manager.Handle(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	sender.mu.Lock()
	defer sender.mu.Unlock()
	if len(sender.replies) != 2 || sender.replies[0].Text != "Telegram connected to Gofer." ||
		sender.replies[1].Text != "Telegram connection code is invalid or expired." {
		t.Fatalf("connect replies = %#v", sender.replies)
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
	config := Config{Resolver: resolverFunc(func(context.Context, string, string, string) (Identity, error) { return testIdentity(), nil }), Dispatcher: dispatcherFunc(func(context.Context, Request) (Reply, error) { return Reply{}, nil }), Dedupe: NewMemoryDedupe(), MaxInflight: 1, DedupeTTL: time.Minute}
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

func testIdentity() Identity {
	return Identity{BindingID: "chn_00000000000000000000000000000000", UserID: "user"}
}
