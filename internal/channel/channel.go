package channel

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
	// ErrInvalid identifies a malformed message or manager configuration.
	ErrInvalid = errors.New("invalid channel message")
	// ErrUnauthorized identifies an external identity without an active binding.
	ErrUnauthorized = errors.New("unauthorized channel identity")
	// ErrDuplicate identifies an inbound delivery already in progress or completed.
	ErrDuplicate = errors.New("duplicate channel message")
)

// Attachment is provider-independent inbound file metadata.
type Attachment struct {
	Name      string `json:"name"`
	MediaType string `json:"media_type"`
	URL       string `json:"url"`
	Size      int64  `json:"size"`
}

// Message is one normalized inbound provider event.
type Message struct {
	ID             string            `json:"id"`
	Provider       string            `json:"provider"`
	WorkspaceID    string            `json:"workspace_id,omitempty"`
	ExternalUserID string            `json:"external_user_id"`
	ChatID         string            `json:"chat_id"`
	TopicID        string            `json:"topic_id,omitempty"`
	Text           string            `json:"text"`
	Attachments    []Attachment      `json:"attachments,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
	ReceivedAt     time.Time         `json:"received_at"`
}

// Request is an authenticated agent dispatch.
type Request struct {
	UserID    string  `json:"user_id"`
	ThreadKey string  `json:"thread_key"`
	Message   Message `json:"message"`
}

// Reply is one provider-independent outbound response.
type Reply struct {
	Provider    string       `json:"provider"`
	WorkspaceID string       `json:"workspace_id,omitempty"`
	ChatID      string       `json:"chat_id"`
	TopicID     string       `json:"topic_id,omitempty"`
	Text        string       `json:"text"`
	Attachments []Attachment `json:"attachments,omitempty"`
}

// IdentityResolver maps a provider identity to an internal user.
type IdentityResolver interface {
	Resolve(context.Context, string, string, string) (string, error)
}

// Dispatcher runs one authenticated channel turn.
type Dispatcher interface {
	Dispatch(context.Context, Request) (Reply, error)
}

// Sender delivers an outbound reply through its provider.
type Sender interface {
	Name() string
	Send(context.Context, Reply) error
	Close() error
}

// Dedupe owns atomic inbound idempotency claims.
type Dedupe interface {
	Begin(context.Context, string, time.Time, time.Duration) (bool, error)
	Complete(context.Context, string, bool) error
}

// MemoryDedupe is a concurrency-safe TTL idempotency store.
type MemoryDedupe struct {
	mu      sync.Mutex
	entries map[string]dedupeEntry
}
type dedupeEntry struct {
	expires  time.Time
	complete bool
}

// NewMemoryDedupe constructs an empty dedupe store.
func NewMemoryDedupe() *MemoryDedupe { return &MemoryDedupe{entries: make(map[string]dedupeEntry)} }

// Begin atomically claims a key, pruning expired entries first.
func (store *MemoryDedupe) Begin(ctx context.Context, key string, now time.Time, ttl time.Duration) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if key == "" || ttl <= 0 {
		return false, ErrInvalid
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	for candidate, entry := range store.entries {
		if !entry.expires.After(now) {
			delete(store.entries, candidate)
		}
	}
	if _, exists := store.entries[key]; exists {
		return false, nil
	}
	store.entries[key] = dedupeEntry{expires: now.Add(ttl)}
	return true, nil
}

// Complete retains successful claims and releases failed attempts for retry.
func (store *MemoryDedupe) Complete(ctx context.Context, key string, success bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	entry, exists := store.entries[key]
	if !exists {
		return ErrInvalid
	}
	if !success {
		delete(store.entries, key)
		return nil
	}
	entry.complete = true
	store.entries[key] = entry
	return nil
}

// Config controls identity, concurrency, and delivery idempotency.
type Config struct {
	Resolver    IdentityResolver
	Dispatcher  Dispatcher
	Dedupe      Dedupe
	MaxInflight int
	DedupeTTL   time.Duration
	Now         func() time.Time
}

// Manager owns provider senders and bounded inbound dispatch.
type Manager struct {
	resolver   IdentityResolver
	dispatcher Dispatcher
	dedupe     Dedupe
	slots      chan struct{}
	ttl        time.Duration
	now        func() time.Time
	mu         sync.RWMutex
	senders    map[string]Sender
	closed     bool
}

// NewManager validates dependencies and constructs a manager.
func NewManager(config Config) (*Manager, error) {
	if config.Resolver == nil || config.Dispatcher == nil || config.Dedupe == nil || config.MaxInflight < 1 || config.MaxInflight > 10_000 || config.DedupeTTL < time.Minute {
		return nil, ErrInvalid
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Manager{resolver: config.Resolver, dispatcher: config.Dispatcher, dedupe: config.Dedupe, slots: make(chan struct{}, config.MaxInflight), ttl: config.DedupeTTL, now: config.Now, senders: make(map[string]Sender)}, nil
}

// Register atomically adds a uniquely named outbound provider.
func (manager *Manager) Register(sender Sender) error {
	if sender == nil || strings.TrimSpace(sender.Name()) == "" {
		return ErrInvalid
	}
	name := strings.ToLower(strings.TrimSpace(sender.Name()))
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.closed {
		return errors.New("channel manager closed")
	}
	if _, exists := manager.senders[name]; exists {
		return ErrInvalid
	}
	manager.senders[name] = sender
	return nil
}

// Handle authenticates, deduplicates, dispatches, and sends one inbound event.
func (manager *Manager) Handle(ctx context.Context, message Message) error {
	if err := message.Validate(); err != nil {
		return err
	}
	key := message.Provider + "\xff" + message.WorkspaceID + "\xff" + message.ID
	claimed, err := manager.dedupe.Begin(ctx, key, manager.now(), manager.ttl)
	if err != nil {
		return err
	}
	if !claimed {
		return ErrDuplicate
	}
	success := false
	defer func() { _ = manager.dedupe.Complete(context.WithoutCancel(ctx), key, success) }()
	select {
	case manager.slots <- struct{}{}:
		defer func() { <-manager.slots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	userID, err := manager.resolver.Resolve(ctx, message.Provider, message.WorkspaceID, message.ExternalUserID)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnauthorized, err)
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return ErrUnauthorized
	}
	reply, err := manager.dispatcher.Dispatch(ctx, Request{UserID: userID, ThreadKey: threadKey(message, userID), Message: cloneMessage(message)})
	if err != nil {
		return err
	}
	reply.Provider = message.Provider
	reply.WorkspaceID = message.WorkspaceID
	reply.ChatID = message.ChatID
	reply.TopicID = message.TopicID
	if err = manager.send(ctx, reply); err != nil {
		return err
	}
	success = true
	return nil
}

func (manager *Manager) send(ctx context.Context, reply Reply) error {
	manager.mu.RLock()
	sender, exists := manager.senders[strings.ToLower(reply.Provider)]
	closed := manager.closed
	manager.mu.RUnlock()
	if closed {
		return errors.New("channel manager closed")
	}
	if !exists {
		return fmt.Errorf("channel provider %q is not registered", reply.Provider)
	}
	return sender.Send(ctx, cloneReply(reply))
}

// Providers returns registered provider names in stable order.
func (manager *Manager) Providers() []string {
	manager.mu.RLock()
	names := make([]string, 0, len(manager.senders))
	for name := range manager.senders {
		names = append(names, name)
	}
	manager.mu.RUnlock()
	sort.Strings(names)
	return names
}

// Close idempotently closes all provider senders.
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
	senders := make([]Sender, 0, len(manager.senders))
	for _, sender := range manager.senders {
		senders = append(senders, sender)
	}
	manager.mu.Unlock()
	var failures []error
	for _, sender := range senders {
		failures = append(failures, sender.Close())
	}
	return errors.Join(failures...)
}

// Validate verifies a bounded normalized message.
func (message Message) Validate() error {
	if strings.TrimSpace(message.ID) == "" || strings.TrimSpace(message.Provider) == "" || strings.TrimSpace(message.ExternalUserID) == "" || strings.TrimSpace(message.ChatID) == "" || message.ReceivedAt.IsZero() || len(message.Text) > 1<<20 || len(message.Attachments) > 32 {
		return ErrInvalid
	}
	if strings.TrimSpace(message.Text) == "" && len(message.Attachments) == 0 {
		return ErrInvalid
	}
	for _, attachment := range message.Attachments {
		if strings.TrimSpace(attachment.Name) == "" || strings.TrimSpace(attachment.MediaType) == "" || strings.TrimSpace(attachment.URL) == "" || attachment.Size < 0 {
			return ErrInvalid
		}
	}
	return nil
}

func threadKey(message Message, userID string) string {
	topic := message.TopicID
	if topic == "" {
		topic = message.ChatID
	}
	return message.Provider + ":" + message.WorkspaceID + ":" + userID + ":" + topic
}
func cloneMessage(message Message) Message {
	message.Attachments = append([]Attachment(nil), message.Attachments...)
	message.Metadata = cloneMap(message.Metadata)
	return message
}
func cloneReply(reply Reply) Reply {
	reply.Attachments = append([]Attachment(nil), reply.Attachments...)
	return reply
}
func cloneMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	cloned := make(map[string]string, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}
