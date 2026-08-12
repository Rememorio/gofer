package browser

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	// ErrCapacity identifies a session manager whose sessions are all pinned.
	ErrCapacity = errors.New("browser session capacity reached")
	// ErrNotFound identifies an unknown browser session key.
	ErrNotFound = errors.New("browser session not found")
)

// Factory creates one browser session owned by the supplied long-lived context.
type Factory func(context.Context, string) (Session, error)

// ManagerConfig configures bounded browser session retention.
type ManagerConfig struct {
	MaxSessions int
	IdleTimeout time.Duration
	Factory     Factory
	Now         func() time.Time
}

// Manager owns bounded, thread-scoped browser sessions.
type Manager struct {
	mu          sync.Mutex
	ctx         context.Context
	cancel      context.CancelFunc
	factory     Factory
	now         func() time.Time
	maxSessions int
	idleTimeout time.Duration
	sessions    map[string]*managedSession
	closed      bool
}

type managedSession struct {
	session  Session
	ready    chan struct{}
	refs     int
	lastUsed time.Time
}

// Lease pins a browser session against LRU and idle eviction.
type Lease struct {
	Session Session
	once    sync.Once
	release func()
}

// Release unpins the leased session.
func (lease *Lease) Release() {
	if lease == nil {
		return
	}
	lease.once.Do(func() {
		if lease.release != nil {
			lease.release()
		}
	})
}

// SessionStatus is safe manager observability without browser internals.
type SessionStatus struct {
	Key      string    `json:"key"`
	Pinned   int       `json:"pinned"`
	LastUsed time.Time `json:"last_used"`
	Ready    bool      `json:"ready"`
}

// NewManager validates config and constructs a bounded session manager.
func NewManager(parent context.Context, config ManagerConfig) (*Manager, error) {
	if parent == nil || config.Factory == nil || config.MaxSessions < 0 || config.IdleTimeout < 0 {
		return nil, fmt.Errorf("%w: context, factory, and non-negative limits are required", ErrInvalidConfig)
	}
	if config.MaxSessions == 0 {
		config.MaxSessions = 32
	}
	if config.IdleTimeout == 0 {
		config.IdleTimeout = 30 * time.Minute
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	ctx, cancel := context.WithCancel(parent)
	return &Manager{
		ctx: ctx, cancel: cancel, factory: config.Factory, now: config.Now,
		maxSessions: config.MaxSessions, idleTimeout: config.IdleTimeout,
		sessions: make(map[string]*managedSession),
	}, nil
}

// Acquire returns and pins the session for key, creating it at most once under concurrency.
func (manager *Manager) Acquire(ctx context.Context, key string) (*Lease, error) {
	if manager == nil {
		return nil, fmt.Errorf("%w: manager is nil", ErrInvalidConfig)
	}
	if ctx == nil {
		return nil, fmt.Errorf("%w: acquire context is nil", ErrInvalidConfig)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, fmt.Errorf("%w: session key is required", ErrInvalidConfig)
	}
	for {
		lease, wait, evicted, create, err := manager.acquireStep(key)
		if err != nil || lease != nil {
			return lease, err
		}
		if evicted != nil {
			if err := evicted.Close(); err != nil {
				return nil, fmt.Errorf("close evicted browser session: %w", err)
			}
			continue
		}
		if wait != nil {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-wait:
				continue
			}
		}
		if create {
			return manager.createSession(key)
		}
	}
}

func (manager *Manager) acquireStep(key string) (*Lease, <-chan struct{}, Session, bool, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.closed {
		return nil, nil, nil, false, ErrClosed
	}
	if current, exists := manager.sessions[key]; exists {
		if current.ready != nil {
			return nil, current.ready, nil, false, nil
		}
		current.refs++
		current.lastUsed = manager.now()
		return manager.newLease(key, current.session), nil, nil, false, nil
	}
	if evicted := manager.evictCandidate(); evicted != nil {
		return nil, nil, evicted, false, nil
	}
	if len(manager.sessions) >= manager.maxSessions {
		return nil, nil, nil, false, ErrCapacity
	}
	manager.sessions[key] = &managedSession{ready: make(chan struct{}), lastUsed: manager.now()}
	return nil, nil, nil, true, nil
}

func (manager *Manager) createSession(key string) (*Lease, error) {
	session, createErr := manager.factory(manager.ctx, key)
	manager.mu.Lock()
	current, exists := manager.sessions[key]
	if !exists {
		manager.mu.Unlock()
		if session != nil {
			_ = session.Close()
		}
		if createErr != nil {
			return nil, createErr
		}
		return nil, ErrClosed
	}
	if createErr != nil || session == nil || manager.closed {
		delete(manager.sessions, key)
		close(current.ready)
		manager.mu.Unlock()
		if session != nil {
			_ = session.Close()
		}
		if createErr != nil {
			return nil, createErr
		}
		if manager.closed {
			return nil, ErrClosed
		}
		return nil, errors.New("browser factory returned nil session")
	}
	current.session = session
	current.refs = 1
	current.lastUsed = manager.now()
	close(current.ready)
	current.ready = nil
	lease := manager.newLease(key, session)
	manager.mu.Unlock()
	return lease, nil
}

func (manager *Manager) newLease(key string, session Session) *Lease {
	return &Lease{Session: session, release: func() { manager.release(key, session) }}
}

func (manager *Manager) release(key string, session Session) {
	manager.mu.Lock()
	current, exists := manager.sessions[key]
	if exists && current.session == session {
		if current.refs > 0 {
			current.refs--
		}
		current.lastUsed = manager.now()
	}
	manager.mu.Unlock()
}

func (manager *Manager) evictCandidate() Session {
	now := manager.now()
	var selectedKey string
	var selected *managedSession
	for key, current := range manager.sessions {
		if current.ready != nil || current.refs != 0 {
			continue
		}
		expired := now.Sub(current.lastUsed) >= manager.idleTimeout
		atCapacity := len(manager.sessions) >= manager.maxSessions
		if !expired && !atCapacity {
			continue
		}
		if selected == nil || current.lastUsed.Before(selected.lastUsed) {
			selectedKey, selected = key, current
		}
	}
	if selected == nil {
		return nil
	}
	delete(manager.sessions, selectedKey)
	return selected.session
}

// CloseSession removes and closes one session by key.
func (manager *Manager) CloseSession(key string) error {
	if manager == nil {
		return nil
	}
	manager.mu.Lock()
	current, exists := manager.sessions[key]
	if exists {
		delete(manager.sessions, key)
	}
	manager.mu.Unlock()
	if !exists {
		return fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	if current.ready != nil {
		return nil
	}
	return current.session.Close()
}

// Status returns a stable session-manager snapshot.
func (manager *Manager) Status() []SessionStatus {
	if manager == nil {
		return nil
	}
	manager.mu.Lock()
	statuses := make([]SessionStatus, 0, len(manager.sessions))
	for key, current := range manager.sessions {
		statuses = append(statuses, SessionStatus{
			Key: key, Pinned: current.refs, LastUsed: current.lastUsed, Ready: current.ready == nil,
		})
	}
	manager.mu.Unlock()
	sort.Slice(statuses, func(left, right int) bool { return statuses[left].Key < statuses[right].Key })
	return statuses
}

// Close cancels creation and closes every ready session.
func (manager *Manager) Close() error {
	if manager == nil {
		return nil
	}
	manager.mu.Lock()
	if manager.closed {
		manager.mu.Unlock()
		return nil
	}
	manager.closed = true
	manager.cancel()
	sessions := make([]Session, 0, len(manager.sessions))
	for _, current := range manager.sessions {
		if current.ready == nil && current.session != nil {
			sessions = append(sessions, current.session)
		}
	}
	manager.sessions = make(map[string]*managedSession)
	manager.mu.Unlock()
	closeErrors := make([]error, 0)
	for _, session := range sessions {
		if err := session.Close(); err != nil {
			closeErrors = append(closeErrors, err)
		}
	}
	return errors.Join(closeErrors...)
}
