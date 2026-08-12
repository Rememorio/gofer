package channel

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
)

func TestMemoryStateBindingLifecycleAndOwnership(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := NewMemoryState()
	now := time.Now().UTC()
	binding, err := NewBinding("alice", "webhook", "workspace", "external", now)
	if err != nil {
		t.Fatal(err)
	}
	binding.WorkspaceName, binding.ExternalUserName = "Workspace", "Alice"
	bound, err := state.Bind(ctx, binding)
	if err != nil || bound.ID != binding.ID {
		t.Fatalf("Bind() = %#v, %v", bound, err)
	}
	identity, err := state.Resolve(ctx, "WEBHOOK", "workspace", "external")
	if err != nil || identity.BindingID != binding.ID || identity.UserID != "alice" {
		t.Fatalf("Resolve() = %#v, %v", identity, err)
	}
	bindings, err := state.Bindings(ctx, "alice")
	if err != nil || len(bindings) != 1 || bindings[0].ExternalUserName != "Alice" {
		t.Fatalf("Bindings() = %#v, %v", bindings, err)
	}

	other, _ := NewBinding("bob", "webhook", "workspace", "external", now.Add(time.Second))
	if _, err = state.Bind(ctx, other); !errors.Is(err, ErrConflict) {
		t.Fatalf("other owner Bind() = %v", err)
	}
	if err = state.Revoke(ctx, binding.ID, "bob", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign Revoke() = %v", err)
	}
	if err = state.Revoke(ctx, binding.ID, "alice", now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = state.Resolve(ctx, "webhook", "workspace", "external"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked Resolve() = %v", err)
	}
	rebound, err := state.Bind(ctx, other)
	if err != nil || rebound.ID != binding.ID || rebound.UserID != "bob" || rebound.Status != BindingConnected {
		t.Fatalf("rebind = %#v, %v", rebound, err)
	}
	if err = state.Revoke(ctx, rebound.ID, "bob", now.Add(3*time.Second)); err != nil || state.Revoke(ctx, rebound.ID, "bob", now.Add(4*time.Second)) != nil {
		t.Fatalf("idempotent revoke = %v", err)
	}
}

func TestMemoryStateConversationFirstWriterAndCleanup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	state := NewMemoryState()
	now := time.Now().UTC()
	binding, _ := NewBinding("user", "webhook", "workspace", "external", now)
	binding, _ = state.Bind(ctx, binding)
	first := validConversation(t, binding, "chat", "topic", now)
	mapped, created, err := state.MapConversation(ctx, first)
	if err != nil || !created || mapped.ThreadID != first.ThreadID {
		t.Fatalf("first MapConversation() = %#v, %v, %v", mapped, created, err)
	}
	second := validConversation(t, binding, "chat", "topic", now.Add(time.Second))
	mapped, created, err = state.MapConversation(ctx, second)
	if err != nil || created || mapped.ThreadID != first.ThreadID {
		t.Fatalf("second MapConversation() = %#v, %v, %v", mapped, created, err)
	}
	got, err := state.Conversation(ctx, binding.ID, "chat", "topic")
	if err != nil || got.ThreadID != first.ThreadID {
		t.Fatalf("Conversation() = %#v, %v", got, err)
	}
	if err = state.DeleteThread(ctx, first.ThreadID); err != nil {
		t.Fatal(err)
	}
	if _, err = state.Conversation(ctx, binding.ID, "chat", "topic"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted Conversation() = %v", err)
	}
	invalid := first
	invalid.Provider = "other"
	if _, _, err = state.MapConversation(ctx, invalid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong provider mapping = %v", err)
	}
}

func TestChannelStateValidationAndCancellation(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	for _, input := range [][4]string{{"", "webhook", "w", "e"}, {"u", "Bad Provider", "w", "e"}, {"u", "webhook", strings.Repeat("w", 257), "e"}, {"u", "webhook", "w", ""}} {
		if _, err := NewBinding(input[0], input[1], input[2], input[3], now); !errors.Is(err, ErrInvalid) {
			t.Fatalf("NewBinding(%q) = %v", input, err)
		}
	}
	state := NewMemoryState()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	binding, _ := NewBinding("u", "webhook", "w", "e", now)
	if _, err := state.Bind(ctx, binding); !errors.Is(err, context.Canceled) {
		t.Fatalf("Bind(cancelled) = %v", err)
	}
	if _, err := state.Bindings(context.Background(), ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Bindings(empty) = %v", err)
	}
	if err := state.Revoke(context.Background(), "bad", "u", now); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Revoke(invalid) = %v", err)
	}
	if err := (Conversation{}).Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Conversation.Validate() = %v", err)
	}
}

func validConversation(t *testing.T, binding Binding, chatID, topicID string, now time.Time) Conversation {
	t.Helper()
	threadID, err := domain.NewThreadID()
	if err != nil {
		t.Fatal(err)
	}
	return Conversation{
		BindingID: binding.ID, Provider: binding.Provider, ChatID: chatID, TopicID: topicID,
		ThreadID: threadID, CreatedAt: now, UpdatedAt: now,
	}
}
