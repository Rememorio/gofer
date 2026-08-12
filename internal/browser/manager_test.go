package browser

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestManagerCreatesOnceAndPinsLeases(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	releaseFactory := make(chan struct{})
	session := &stubSession{}
	var calls int
	var callMu sync.Mutex
	manager := newTestManager(t, ManagerConfig{Factory: func(context.Context, string) (Session, error) {
		callMu.Lock()
		calls++
		callMu.Unlock()
		close(started)
		<-releaseFactory
		return session, nil
	}})
	firstDone := make(chan *Lease, 1)
	go func() {
		lease, _ := manager.Acquire(context.Background(), "thread")
		firstDone <- lease
	}()
	<-started
	secondDone := make(chan *Lease, 1)
	go func() {
		lease, _ := manager.Acquire(context.Background(), "thread")
		secondDone <- lease
	}()
	close(releaseFactory)
	first, second := <-firstDone, <-secondDone
	if first == nil || second == nil || first.Session != second.Session {
		t.Fatalf("leases = %#v, %#v", first, second)
	}
	callMu.Lock()
	actualCalls := calls
	callMu.Unlock()
	if actualCalls != 1 {
		t.Fatalf("factory calls = %d, want 1", actualCalls)
	}
	statuses := manager.Status()
	if len(statuses) != 1 || statuses[0].Pinned != 2 || !statuses[0].Ready {
		t.Fatalf("Status() = %#v", statuses)
	}
	first.Release()
	first.Release()
	second.Release()
	if statuses := manager.Status(); statuses[0].Pinned != 0 {
		t.Fatalf("Status(after release) = %#v", statuses)
	}
}

func TestManagerCapacityLRUAndIdleEviction(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	sessions := make(map[string]*stubSession)
	manager := newTestManager(t, ManagerConfig{
		MaxSessions: 1, IdleTimeout: time.Hour, Now: func() time.Time { return now },
		Factory: func(_ context.Context, key string) (Session, error) {
			session := &stubSession{}
			sessions[key] = session
			return session, nil
		},
	})
	first, err := manager.Acquire(context.Background(), "one")
	if err != nil {
		t.Fatalf("Acquire(one): %v", err)
	}
	if _, err := manager.Acquire(context.Background(), "two"); !errors.Is(err, ErrCapacity) {
		t.Fatalf("Acquire(capacity) error = %v", err)
	}
	first.Release()
	now = now.Add(time.Minute)
	second, err := manager.Acquire(context.Background(), "two")
	if err != nil {
		t.Fatalf("Acquire(two): %v", err)
	}
	second.Release()
	if sessions["one"].closeCalls != 1 {
		t.Fatalf("LRU close calls = %d", sessions["one"].closeCalls)
	}

	now = now.Add(2 * time.Hour)
	third, err := manager.Acquire(context.Background(), "three")
	if err != nil {
		t.Fatalf("Acquire(three): %v", err)
	}
	third.Release()
	if sessions["two"].closeCalls != 1 {
		t.Fatalf("idle close calls = %d", sessions["two"].closeCalls)
	}
}

func TestManagerWaitCancellationAndFactoryFailure(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	manager := newTestManager(t, ManagerConfig{Factory: func(context.Context, string) (Session, error) {
		close(started)
		<-release
		return &stubSession{}, nil
	}})
	firstDone := make(chan error, 1)
	go func() {
		lease, err := manager.Acquire(context.Background(), "thread")
		if lease != nil {
			lease.Release()
		}
		firstDone <- err
	}()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := manager.Acquire(ctx, "thread"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire(cancelled) error = %v", err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Acquire(): %v", err)
	}

	factoryError := errors.New("launch failed")
	failing := newTestManager(t, ManagerConfig{Factory: func(context.Context, string) (Session, error) {
		return nil, factoryError
	}})
	if _, err := failing.Acquire(context.Background(), "thread"); !errors.Is(err, factoryError) {
		t.Fatalf("Acquire(factory error) = %v", err)
	}
}

func TestManagerCloseSessionStatusAndClose(t *testing.T) {
	t.Parallel()

	closeError := errors.New("close failed")
	sessions := make(map[string]*stubSession)
	manager := newTestManager(t, ManagerConfig{Factory: func(_ context.Context, key string) (Session, error) {
		session := &stubSession{}
		if key == "bad" {
			session.closeErr = closeError
		}
		sessions[key] = session
		return session, nil
	}})
	lease, _ := manager.Acquire(context.Background(), "good")
	lease.Release()
	if err := manager.CloseSession("good"); err != nil || sessions["good"].closeCalls != 1 {
		t.Fatalf("CloseSession() = %v, calls=%d", err, sessions["good"].closeCalls)
	}
	if err := manager.CloseSession("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("CloseSession(missing) error = %v", err)
	}
	lease, _ = manager.Acquire(context.Background(), "bad")
	lease.Release()
	if err := manager.Close(); !errors.Is(err, closeError) {
		t.Fatalf("Close() error = %v", err)
	}
	if err := manager.Close(); err != nil {
		t.Fatalf("Close(second): %v", err)
	}
	if _, err := manager.Acquire(context.Background(), "new"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Acquire(closed) error = %v", err)
	}
}

func TestManagerValidationAndNilMethods(t *testing.T) {
	t.Parallel()

	invalid := []ManagerConfig{{}, {Factory: func(context.Context, string) (Session, error) { return nil, nil }, MaxSessions: -1}}
	for _, config := range invalid {
		if _, err := NewManager(context.Background(), config); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("NewManager(%#v) error = %v", config, err)
		}
	}
	manager := newTestManager(t, ManagerConfig{Factory: func(context.Context, string) (Session, error) {
		return &stubSession{}, nil
	}})
	if _, err := manager.Acquire(context.Background(), " "); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Acquire(empty key) error = %v", err)
	}
	var nilManager *Manager
	if _, err := nilManager.Acquire(context.Background(), "key"); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil Acquire() error = %v", err)
	}
	if status := nilManager.Status(); status != nil {
		t.Fatalf("nil Status() = %#v", status)
	}
	if err := nilManager.Close(); err != nil {
		t.Fatalf("nil Close(): %v", err)
	}
	var nilLease *Lease
	nilLease.Release()
}

func newTestManager(t *testing.T, config ManagerConfig) *Manager {
	t.Helper()
	manager, err := NewManager(context.Background(), config)
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}

type stubSession struct {
	closeCalls int
	closeErr   error
}

func (*stubSession) Navigate(context.Context, string) (Snapshot, error) { return Snapshot{}, nil }
func (*stubSession) Snapshot(context.Context) (Snapshot, error)         { return Snapshot{}, nil }
func (*stubSession) Click(context.Context, int) (Snapshot, error)       { return Snapshot{}, nil }
func (*stubSession) Type(context.Context, int, string, bool) (Snapshot, error) {
	return Snapshot{}, nil
}
func (*stubSession) Scroll(context.Context, int, int) (Snapshot, error) { return Snapshot{}, nil }
func (*stubSession) Back(context.Context) (Snapshot, error)             { return Snapshot{}, nil }
func (*stubSession) Screenshot(context.Context, bool) ([]byte, error)   { return []byte("png"), nil }
func (session *stubSession) Close() error {
	session.closeCalls++
	return session.closeErr
}
