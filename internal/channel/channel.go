package channel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	// ErrBusy identifies a saturated ingress queue or an active conversation turn.
	ErrBusy = errors.New("channel is busy")
	// ErrClosed identifies a stopped manager.
	ErrClosed = errors.New("channel manager closed")
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
	Identity  Identity `json:"identity"`
	ThreadKey string   `json:"thread_key"`
	Message   Message  `json:"message"`
}

// Reply is one provider-independent outbound response.
type Reply struct {
	Provider    string       `json:"provider"`
	WorkspaceID string       `json:"workspace_id,omitempty"`
	ChatID      string       `json:"chat_id"`
	TopicID     string       `json:"topic_id,omitempty"`
	InReplyTo   string       `json:"in_reply_to,omitempty"`
	Text        string       `json:"text"`
	Attachments []Attachment `json:"attachments,omitempty"`
}

// IdentityResolver maps a provider identity to an internal user.
type IdentityResolver interface {
	Resolve(context.Context, string, string, string) (Identity, error)
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

// SubmitFunc accepts one normalized provider event without blocking on the
// agent turn. Providers should retain and retry events when it returns ErrBusy.
type SubmitFunc func(context.Context, Message) error

// Source is implemented by providers that own an inbound polling or socket
// connection. Start must return after the connection is ready and keep any
// background work bounded by the supplied context.
type Source interface {
	Start(context.Context, SubmitFunc) error
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
	Resolver          IdentityResolver
	Dispatcher        Dispatcher
	Dedupe            Dedupe
	MaxInflight       int
	QueueCapacity     int
	DedupeTTL         time.Duration
	UnauthorizedReply string
	OnError           func(Message, error)
	Now               func() time.Time
}

// Manager owns provider senders and bounded inbound dispatch.
type Manager struct {
	resolver          IdentityResolver
	dispatcher        Dispatcher
	dedupe            Dedupe
	slots             chan struct{}
	workers           int
	queue             chan Message
	ttl               time.Duration
	now               func() time.Time
	onError           func(Message, error)
	unauthorizedReply string
	mu                sync.RWMutex
	senders           map[string]Sender
	locks             map[string]*conversationLock
	workerCtx         context.Context
	cancel            context.CancelFunc
	wait              sync.WaitGroup
	done              chan struct{}
	started           bool
	closing           bool
	closed            bool
	closeErr          error
}

type conversationLock struct {
	mutex sync.Mutex
	users int
}

// NewManager validates dependencies and constructs a manager.
func NewManager(config Config) (*Manager, error) {
	if config.Resolver == nil || config.Dispatcher == nil || config.Dedupe == nil || config.MaxInflight < 1 || config.MaxInflight > 10_000 || config.DedupeTTL < time.Minute {
		return nil, ErrInvalid
	}
	if config.QueueCapacity == 0 {
		config.QueueCapacity = config.MaxInflight * 4
	}
	if config.QueueCapacity < config.MaxInflight || config.QueueCapacity > 100_000 {
		return nil, ErrInvalid
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Manager{
		resolver: config.Resolver, dispatcher: config.Dispatcher, dedupe: config.Dedupe,
		slots: make(chan struct{}, config.MaxInflight), workers: config.MaxInflight,
		queue: make(chan Message, config.QueueCapacity), ttl: config.DedupeTTL, now: config.Now,
		onError: config.OnError, unauthorizedReply: strings.TrimSpace(config.UnauthorizedReply),
		senders: make(map[string]Sender), locks: make(map[string]*conversationLock), done: make(chan struct{}),
	}, nil
}

// Register atomically adds a uniquely named outbound provider.
func (manager *Manager) Register(sender Sender) error {
	if sender == nil || strings.TrimSpace(sender.Name()) == "" {
		return ErrInvalid
	}
	name := strings.ToLower(strings.TrimSpace(sender.Name()))
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.closed || manager.closing {
		return ErrClosed
	}
	if manager.started {
		return ErrInvalid
	}
	if _, exists := manager.senders[name]; exists {
		return ErrInvalid
	}
	manager.senders[name] = sender
	return nil
}

// Handle authenticates, deduplicates, dispatches, and sends one inbound event.
func (manager *Manager) Handle(ctx context.Context, message Message) error {
	if ctx == nil {
		return ErrInvalid
	}
	manager.mu.RLock()
	unavailable := manager.closed || manager.closing
	manager.mu.RUnlock()
	if unavailable {
		return ErrClosed
	}
	if err := message.Validate(); err != nil {
		return err
	}
	message = normalizeMessage(message)
	key := deliveryKey(message)
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
	identity, err := manager.resolver.Resolve(ctx, message.Provider, message.WorkspaceID, message.ExternalUserID)
	if err != nil {
		if manager.unauthorizedReply != "" && manager.send(ctx, routedReply(message, Reply{Text: manager.unauthorizedReply})) == nil {
			success = true
		}
		return fmt.Errorf("%w: %w", ErrUnauthorized, err)
	}
	identity.BindingID, identity.UserID = strings.TrimSpace(identity.BindingID), strings.TrimSpace(identity.UserID)
	if !validBindingID(identity.BindingID) || identity.UserID == "" {
		return ErrUnauthorized
	}
	threadKey := threadKey(message, identity)
	unlock := manager.lockConversation(threadKey)
	defer unlock()
	reply, err := manager.dispatcher.Dispatch(ctx, Request{Identity: identity, ThreadKey: threadKey, Message: cloneMessage(message)})
	if err != nil {
		return err
	}
	reply = routedReply(message, reply)
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
		return ErrClosed
	}
	if !exists {
		return fmt.Errorf("channel provider %q is not registered", reply.Provider)
	}
	return sender.Send(ctx, cloneReply(reply))
}

// Start launches bounded background workers for asynchronous provider ingress.
func (manager *Manager) Start(parent context.Context) error {
	if manager == nil || parent == nil {
		return ErrInvalid
	}
	manager.mu.Lock()
	if manager.closed || manager.closing {
		manager.mu.Unlock()
		return ErrClosed
	}
	if manager.started {
		manager.mu.Unlock()
		return nil
	}
	manager.workerCtx, manager.cancel = context.WithCancel(parent)
	manager.started = true
	sources := make([]Source, 0, len(manager.senders))
	for _, sender := range manager.senders {
		if source, ok := sender.(Source); ok {
			sources = append(sources, source)
		}
	}
	for range manager.workers {
		manager.wait.Add(1)
		go manager.worker()
	}
	workerCtx := manager.workerCtx
	manager.mu.Unlock()
	for _, source := range sources {
		if err := source.Start(workerCtx, manager.Submit); err != nil {
			return errors.Join(err, manager.Close())
		}
	}
	return nil
}

// Submit enqueues one inbound event without creating unbounded goroutines.
func (manager *Manager) Submit(ctx context.Context, message Message) error {
	if ctx == nil {
		return ErrInvalid
	}
	if err := message.Validate(); err != nil {
		return err
	}
	manager.mu.RLock()
	started, unavailable := manager.started, manager.closed || manager.closing
	manager.mu.RUnlock()
	if unavailable {
		return ErrClosed
	}
	if !started {
		return ErrInvalid
	}
	select {
	case manager.queue <- cloneMessage(message):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		return ErrBusy
	}
}

// Stats reports bounded ingress and registered-provider state.
type Stats struct {
	Running       bool     `json:"running"`
	Providers     []string `json:"providers"`
	QueueDepth    int      `json:"queue_depth"`
	QueueCapacity int      `json:"queue_capacity"`
	MaxInflight   int      `json:"max_inflight"`
}

// Stats returns a point-in-time manager snapshot.
func (manager *Manager) Stats() Stats {
	if manager == nil {
		return Stats{}
	}
	manager.mu.RLock()
	running := manager.started && !manager.closing && !manager.closed
	manager.mu.RUnlock()
	return Stats{Running: running, Providers: manager.Providers(), QueueDepth: len(manager.queue), QueueCapacity: cap(manager.queue), MaxInflight: manager.workers}
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
		err := manager.closeErr
		manager.mu.Unlock()
		return err
	}
	if manager.closing {
		done := manager.done
		manager.mu.Unlock()
		<-done
		manager.mu.RLock()
		err := manager.closeErr
		manager.mu.RUnlock()
		return err
	}
	manager.closing = true
	cancel := manager.cancel
	senders := make([]Sender, 0, len(manager.senders))
	for _, sender := range manager.senders {
		senders = append(senders, sender)
	}
	manager.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	var failures []error
	for _, sender := range senders {
		failures = append(failures, sender.Close())
	}
	manager.wait.Wait()
	manager.mu.Lock()
	manager.closed = true
	manager.closeErr = errors.Join(failures...)
	close(manager.done)
	err := manager.closeErr
	manager.mu.Unlock()
	return err
}

// Validate verifies a bounded normalized message.
func (message Message) Validate() error {
	if err := message.validateEnvelope(); err != nil {
		return err
	}
	if err := validateAttachments(message.Attachments); err != nil {
		return err
	}
	return validateMessageMetadata(message.Metadata)
}

func (message Message) validateEnvelope() error {
	if strings.TrimSpace(message.ID) == "" || len(message.ID) > 512 || !validProvider(normalizeProvider(message.Provider)) ||
		strings.TrimSpace(message.WorkspaceID) != message.WorkspaceID || len(message.WorkspaceID) > 256 || strings.TrimSpace(message.ExternalUserID) == "" || len(message.ExternalUserID) > 256 ||
		strings.TrimSpace(message.ChatID) == "" || len(message.ChatID) > 256 || strings.TrimSpace(message.TopicID) != message.TopicID || len(message.TopicID) > 256 ||
		message.ReceivedAt.IsZero() || len(message.Text) > 1<<20 || len(message.Attachments) > 32 || len(message.Metadata) > 64 {
		return ErrInvalid
	}
	if strings.TrimSpace(message.Text) == "" && len(message.Attachments) == 0 {
		return ErrInvalid
	}
	return nil
}

func validateAttachments(attachments []Attachment) error {
	for _, attachment := range attachments {
		if strings.TrimSpace(attachment.Name) == "" || len(attachment.Name) > 512 || strings.TrimSpace(attachment.MediaType) == "" || len(attachment.MediaType) > 256 || strings.TrimSpace(attachment.URL) == "" || len(attachment.URL) > 8192 || attachment.Size < 0 {
			return ErrInvalid
		}
	}
	return nil
}

func validateMessageMetadata(metadata map[string]string) error {
	for key, value := range metadata {
		if strings.TrimSpace(key) == "" || len(key) > 128 || len(value) > 4096 {
			return ErrInvalid
		}
	}
	return nil
}

func (manager *Manager) worker() {
	defer manager.wait.Done()
	for {
		select {
		case <-manager.workerCtx.Done():
			return
		default:
		}
		select {
		case <-manager.workerCtx.Done():
			return
		case message := <-manager.queue:
			err := manager.Handle(manager.workerCtx, message)
			if err != nil && manager.onError != nil {
				manager.onError(cloneMessage(message), err)
			}
		}
	}
}

func (manager *Manager) lockConversation(key string) func() {
	manager.mu.Lock()
	entry := manager.locks[key]
	if entry == nil {
		entry = &conversationLock{}
		manager.locks[key] = entry
	}
	entry.users++
	manager.mu.Unlock()
	entry.mutex.Lock()
	return func() {
		entry.mutex.Unlock()
		manager.mu.Lock()
		entry.users--
		if entry.users == 0 {
			delete(manager.locks, key)
		}
		manager.mu.Unlock()
	}
}

func threadKey(message Message, identity Identity) string {
	return strings.Join([]string{message.Provider, message.WorkspaceID, identity.BindingID, message.ChatID, message.TopicID}, "\xff")
}

func deliveryKey(message Message) string {
	digest := sha256.Sum256([]byte(message.Provider + "\xff" + message.WorkspaceID + "\xff" + message.ChatID + "\xff" + message.ID))
	return hex.EncodeToString(digest[:])
}

func routedReply(message Message, reply Reply) Reply {
	reply.Provider, reply.WorkspaceID = message.Provider, message.WorkspaceID
	reply.ChatID, reply.TopicID, reply.InReplyTo = message.ChatID, message.TopicID, message.ID
	return reply
}

func normalizeMessage(message Message) Message {
	message.Provider = normalizeProvider(message.Provider)
	message.ID, message.ExternalUserID, message.ChatID = strings.TrimSpace(message.ID), strings.TrimSpace(message.ExternalUserID), strings.TrimSpace(message.ChatID)
	message.ReceivedAt = message.ReceivedAt.UTC()
	return message
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
