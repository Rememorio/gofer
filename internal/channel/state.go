package channel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
)

var (
	// ErrNotFound identifies absent channel state.
	ErrNotFound = errors.New("channel state not found")
	// ErrConflict identifies an external identity owned by another user.
	ErrConflict = errors.New("channel state conflict")
)

// BindingStatus is the lifecycle state of one external account binding.
type BindingStatus string

const (
	// BindingConnected permits inbound messages to run as the owner.
	BindingConnected BindingStatus = "connected"
	// BindingRevoked retains history while rejecting new messages.
	BindingRevoked BindingStatus = "revoked"
)

// Binding assigns one provider identity to exactly one Gofer user.
type Binding struct {
	ID               string        `json:"id"`
	UserID           string        `json:"user_id"`
	Provider         string        `json:"provider"`
	WorkspaceID      string        `json:"workspace_id,omitempty"`
	WorkspaceName    string        `json:"workspace_name,omitempty"`
	ExternalUserID   string        `json:"external_user_id"`
	ExternalUserName string        `json:"external_user_name,omitempty"`
	Status           BindingStatus `json:"status"`
	CreatedAt        time.Time     `json:"created_at"`
	UpdatedAt        time.Time     `json:"updated_at"`
}

// Identity is the trusted result of resolving an inbound provider identity.
type Identity struct {
	BindingID string `json:"binding_id"`
	UserID    string `json:"user_id"`
}

// Conversation maps one provider topic to its durable Gofer thread.
type Conversation struct {
	BindingID string          `json:"binding_id"`
	Provider  string          `json:"provider"`
	ChatID    string          `json:"chat_id"`
	TopicID   string          `json:"topic_id,omitempty"`
	ThreadID  domain.ThreadID `json:"thread_id"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// State owns durable identities, conversation mappings, and delivery claims.
type State interface {
	IdentityResolver
	Dedupe
	Bind(context.Context, Binding) (Binding, error)
	Bindings(context.Context, string) ([]Binding, error)
	Revoke(context.Context, string, string, time.Time) error
	Conversation(context.Context, string, string, string) (Conversation, error)
	MapConversation(context.Context, Conversation) (Conversation, bool, error)
	DeleteThread(context.Context, domain.ThreadID) error
}

// NewBinding constructs a normalized connected binding.
func NewBinding(userID, provider, workspaceID, externalUserID string, at time.Time) (Binding, error) {
	identifier, err := newBindingID()
	if err != nil {
		return Binding{}, err
	}
	binding := Binding{
		ID: identifier, UserID: strings.TrimSpace(userID), Provider: normalizeProvider(provider),
		WorkspaceID: strings.TrimSpace(workspaceID), ExternalUserID: strings.TrimSpace(externalUserID),
		Status: BindingConnected, CreatedAt: at.UTC(), UpdatedAt: at.UTC(),
	}
	return binding, binding.Validate()
}

// Validate verifies a bounded, normalized binding.
func (binding Binding) Validate() error {
	if !validBindingID(binding.ID) || strings.TrimSpace(binding.UserID) != binding.UserID || binding.UserID == "" || len(binding.UserID) > 128 ||
		binding.Provider != normalizeProvider(binding.Provider) || !validProvider(binding.Provider) ||
		strings.TrimSpace(binding.WorkspaceID) != binding.WorkspaceID || len(binding.WorkspaceID) > 256 ||
		strings.TrimSpace(binding.ExternalUserID) != binding.ExternalUserID || binding.ExternalUserID == "" || len(binding.ExternalUserID) > 256 ||
		len(binding.WorkspaceName) > 512 || len(binding.ExternalUserName) > 512 ||
		(binding.Status != BindingConnected && binding.Status != BindingRevoked) ||
		binding.CreatedAt.IsZero() || binding.UpdatedAt.IsZero() || binding.UpdatedAt.Before(binding.CreatedAt) {
		return ErrInvalid
	}
	return nil
}

// Validate verifies one bounded conversation mapping.
func (conversation Conversation) Validate() error {
	if !validBindingID(conversation.BindingID) || conversation.Provider != normalizeProvider(conversation.Provider) || !validProvider(conversation.Provider) ||
		strings.TrimSpace(conversation.ChatID) != conversation.ChatID || conversation.ChatID == "" || len(conversation.ChatID) > 256 ||
		strings.TrimSpace(conversation.TopicID) != conversation.TopicID || len(conversation.TopicID) > 256 ||
		conversation.CreatedAt.IsZero() || conversation.UpdatedAt.IsZero() || conversation.UpdatedAt.Before(conversation.CreatedAt) {
		return ErrInvalid
	}
	if _, err := domain.ParseThreadID(string(conversation.ThreadID)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return nil
}

// MemoryState is the concurrency-safe ephemeral State implementation.
type MemoryState struct {
	mu            sync.RWMutex
	bindings      map[string]Binding
	identities    map[string]string
	conversations map[string]Conversation
	dedupe        *MemoryDedupe
}

// NewMemoryState constructs empty channel state.
func NewMemoryState() *MemoryState {
	return &MemoryState{
		bindings: make(map[string]Binding), identities: make(map[string]string),
		conversations: make(map[string]Conversation), dedupe: NewMemoryDedupe(),
	}
}

// Bind idempotently connects an identity, rejecting an active different owner.
func (state *MemoryState) Bind(ctx context.Context, binding Binding) (Binding, error) {
	if err := contextError(ctx); err != nil {
		return Binding{}, err
	}
	if err := binding.Validate(); err != nil || binding.Status != BindingConnected {
		return Binding{}, ErrInvalid
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	key := identityKey(binding.Provider, binding.WorkspaceID, binding.ExternalUserID)
	if identifier := state.identities[key]; identifier != "" {
		existing := state.bindings[identifier]
		if existing.Status == BindingConnected && existing.UserID != binding.UserID {
			return Binding{}, ErrConflict
		}
		if existing.UserID != binding.UserID {
			state.deleteBindingConversations(identifier)
		}
		existing.UserID, existing.Status, existing.UpdatedAt = binding.UserID, BindingConnected, binding.UpdatedAt
		existing.WorkspaceName, existing.ExternalUserName = binding.WorkspaceName, binding.ExternalUserName
		state.bindings[identifier] = existing
		return existing, nil
	}
	state.bindings[binding.ID] = binding
	state.identities[key] = binding.ID
	return binding, nil
}

// Bindings lists isolated owner bindings in stable newest-first order.
func (state *MemoryState) Bindings(ctx context.Context, userID string) ([]Binding, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, ErrInvalid
	}
	state.mu.RLock()
	bindings := make([]Binding, 0)
	for _, binding := range state.bindings {
		if binding.UserID == userID {
			bindings = append(bindings, binding)
		}
	}
	state.mu.RUnlock()
	sort.Slice(bindings, func(left, right int) bool {
		return bindings[left].UpdatedAt.After(bindings[right].UpdatedAt) || bindings[left].UpdatedAt.Equal(bindings[right].UpdatedAt) && bindings[left].ID > bindings[right].ID
	})
	return bindings, nil
}

// Revoke disconnects an owner-scoped binding without erasing history.
func (state *MemoryState) Revoke(ctx context.Context, identifier, userID string, at time.Time) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if !validBindingID(identifier) || strings.TrimSpace(userID) == "" || at.IsZero() {
		return ErrInvalid
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	binding, exists := state.bindings[identifier]
	if !exists || binding.UserID != strings.TrimSpace(userID) {
		return ErrNotFound
	}
	if binding.Status == BindingRevoked {
		return nil
	}
	binding.Status, binding.UpdatedAt = BindingRevoked, at.UTC()
	state.bindings[identifier] = binding
	return nil
}

// Resolve returns only an active binding's trusted internal identity.
func (state *MemoryState) Resolve(ctx context.Context, provider, workspaceID, externalUserID string) (Identity, error) {
	if err := contextError(ctx); err != nil {
		return Identity{}, err
	}
	state.mu.RLock()
	identifier := state.identities[identityKey(provider, workspaceID, externalUserID)]
	binding, exists := state.bindings[identifier]
	state.mu.RUnlock()
	if !exists || binding.Status != BindingConnected {
		return Identity{}, ErrNotFound
	}
	return Identity{BindingID: binding.ID, UserID: binding.UserID}, nil
}

// Conversation returns an active binding's topic mapping.
func (state *MemoryState) Conversation(ctx context.Context, bindingID, chatID, topicID string) (Conversation, error) {
	if err := contextError(ctx); err != nil {
		return Conversation{}, err
	}
	state.mu.RLock()
	binding := state.bindings[bindingID]
	conversation, exists := state.conversations[conversationKey(bindingID, chatID, topicID)]
	state.mu.RUnlock()
	if binding.Status != BindingConnected || !exists {
		return Conversation{}, ErrNotFound
	}
	return conversation, nil
}

// MapConversation atomically preserves the first mapping for a provider topic.
func (state *MemoryState) MapConversation(ctx context.Context, conversation Conversation) (Conversation, bool, error) {
	if err := contextError(ctx); err != nil {
		return Conversation{}, false, err
	}
	if err := conversation.Validate(); err != nil {
		return Conversation{}, false, err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	binding, exists := state.bindings[conversation.BindingID]
	if !exists || binding.Status != BindingConnected || binding.Provider != conversation.Provider {
		return Conversation{}, false, ErrNotFound
	}
	key := conversationKey(conversation.BindingID, conversation.ChatID, conversation.TopicID)
	if existing, found := state.conversations[key]; found {
		return existing, false, nil
	}
	state.conversations[key] = conversation
	return conversation, true, nil
}

// DeleteThread removes every mapping to a deleted durable thread.
func (state *MemoryState) DeleteThread(ctx context.Context, threadID domain.ThreadID) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	for key, conversation := range state.conversations {
		if conversation.ThreadID == threadID {
			delete(state.conversations, key)
		}
	}
	return nil
}

// Begin delegates delivery claims to the bounded in-memory ledger.
func (state *MemoryState) Begin(ctx context.Context, key string, now time.Time, ttl time.Duration) (bool, error) {
	return state.dedupe.Begin(ctx, key, now, ttl)
}

// Complete delegates delivery completion to the bounded in-memory ledger.
func (state *MemoryState) Complete(ctx context.Context, key string, success bool) error {
	return state.dedupe.Complete(ctx, key, success)
}

func (state *MemoryState) deleteBindingConversations(bindingID string) {
	for key, conversation := range state.conversations {
		if conversation.BindingID == bindingID {
			delete(state.conversations, key)
		}
	}
}

func newBindingID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate channel binding identifier: %w", err)
	}
	return "chn_" + hex.EncodeToString(random[:]), nil
}

func validBindingID(value string) bool {
	if len(value) != 36 || !strings.HasPrefix(value, "chn_") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "chn_"))
	return err == nil
}

func normalizeProvider(value string) string { return strings.ToLower(strings.TrimSpace(value)) }
func validProvider(value string) bool {
	if value == "" || len(value) > 32 {
		return false
	}
	for _, character := range value {
		if character < 'a' || character > 'z' {
			if character < '0' || character > '9' {
				if character != '-' && character != '_' {
					return false
				}
			}
		}
	}
	return true
}
func identityKey(provider, workspaceID, externalUserID string) string {
	return normalizeProvider(provider) + "\xff" + strings.TrimSpace(workspaceID) + "\xff" + strings.TrimSpace(externalUserID)
}
func conversationKey(bindingID, chatID, topicID string) string {
	return bindingID + "\xff" + strings.TrimSpace(chatID) + "\xff" + strings.TrimSpace(topicID)
}
func contextError(ctx context.Context) error {
	if ctx == nil {
		return ErrInvalid
	}
	return ctx.Err()
}
