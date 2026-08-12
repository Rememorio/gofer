package sqlstore

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/channel"
	"github.com/Rememorio/gofer/internal/domain"
)

func TestSQLiteChannelStatePersistsBindingsConversationsAndDedupe(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "channels.db")
	database := openSQLite(t, path)
	state := database.ChannelState()
	now := time.Now().UTC().Truncate(time.Microsecond)
	binding, _ := channel.NewBinding("alice", channel.WebhookProvider, "workspace", "external", now)
	binding.WorkspaceName, binding.ExternalUserName = "Workspace", "Alice"
	bound, err := state.Bind(ctx, binding)
	if err != nil || bound.ID != binding.ID {
		t.Fatalf("Bind() = %#v, %v", bound, err)
	}
	thread, _ := domain.NewThread(now)
	if err = database.CreateThread(ctx, thread); err != nil {
		t.Fatal(err)
	}
	mapping := channel.Conversation{
		BindingID: binding.ID, Provider: binding.Provider, ChatID: "chat", TopicID: "topic",
		ThreadID: thread.ID, CreatedAt: now, UpdatedAt: now,
	}
	if got, created, mapErr := state.MapConversation(ctx, mapping); mapErr != nil || !created || got.ThreadID != thread.ID {
		t.Fatalf("MapConversation() = %#v, %v, %v", got, created, mapErr)
	}
	claimAndCompleteChannelDelivery(t, state, now)
	if err = database.Close(); err != nil {
		t.Fatal(err)
	}

	database = openSQLite(t, path)
	t.Cleanup(func() { _ = database.Close() })
	state = database.ChannelState()
	identity, err := state.Resolve(ctx, channel.WebhookProvider, "workspace", "external")
	if err != nil || identity.UserID != "alice" || identity.BindingID != binding.ID {
		t.Fatalf("Resolve(restart) = %#v, %v", identity, err)
	}
	bindings, err := state.Bindings(ctx, "alice")
	if err != nil || len(bindings) != 1 || bindings[0].WorkspaceName != "Workspace" {
		t.Fatalf("Bindings(restart) = %#v, %v", bindings, err)
	}
	got, err := state.Conversation(ctx, binding.ID, "chat", "topic")
	if err != nil || got.ThreadID != thread.ID {
		t.Fatalf("Conversation(restart) = %#v, %v", got, err)
	}
	assertRestartedChannelDedupe(t, state, now)
}

func claimAndCompleteChannelDelivery(t *testing.T, state channel.State, now time.Time) {
	t.Helper()
	claimed, err := state.Begin(context.Background(), "delivery", now, time.Hour)
	if err != nil || !claimed {
		t.Fatalf("Begin() = %v, %v", claimed, err)
	}
	if err = state.Complete(context.Background(), "delivery", true); err != nil {
		t.Fatal(err)
	}
}

func assertRestartedChannelDedupe(t *testing.T, state channel.State, now time.Time) {
	t.Helper()
	claimed, err := state.Begin(context.Background(), "delivery", now.Add(time.Minute), time.Hour)
	if err != nil || claimed {
		t.Fatalf("duplicate Begin() = %v, %v", claimed, err)
	}
	claimed, err = state.Begin(context.Background(), "delivery", now.Add(2*time.Hour), time.Hour)
	if err != nil || !claimed {
		t.Fatalf("expired Begin() = %v, %v", claimed, err)
	}
	if err = state.Complete(context.Background(), "delivery", false); err != nil {
		t.Fatal(err)
	}
	claimed, err = state.Begin(context.Background(), "delivery", now.Add(2*time.Hour), time.Hour)
	if err != nil || !claimed {
		t.Fatalf("released Begin() = %v, %v", claimed, err)
	}
}

func TestSQLiteChannelStateOwnershipMappingAndCleanup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := openSQLite(t, filepath.Join(t.TempDir(), "channels.db"))
	defer func() { _ = database.Close() }()
	state := database.ChannelState()
	now := time.Now().UTC()
	binding, _ := channel.NewBinding("alice", "webhook", "workspace", "external", now)
	binding, _ = state.Bind(ctx, binding)
	other := assertSQLiteChannelBindingBoundaries(t, state, binding, now)
	thread, _ := domain.NewThread(now)
	_ = database.CreateThread(ctx, thread)
	mapping := channel.Conversation{BindingID: binding.ID, Provider: "webhook", ChatID: "chat", ThreadID: thread.ID, CreatedAt: now, UpdatedAt: now}
	first, created, err := state.MapConversation(ctx, mapping)
	if err != nil || !created {
		t.Fatalf("MapConversation() = %#v, %v, %v", first, created, err)
	}
	assertSQLiteChannelMappingBoundaries(t, state, database, mapping, thread, now)
	remapped, _ := domain.NewThread(now.Add(time.Second))
	if err = database.CreateThread(ctx, remapped); err != nil {
		t.Fatal(err)
	}
	mapping.ThreadID, mapping.UpdatedAt = remapped.ID, now.Add(time.Second)
	if err = state.RemapConversation(ctx, mapping); err != nil {
		t.Fatal(err)
	}
	if got, lookupErr := state.Conversation(ctx, binding.ID, "chat", ""); lookupErr != nil || got.ThreadID != remapped.ID {
		t.Fatalf("remapped conversation = %#v, %v", got, lookupErr)
	}
	missingMapping := mapping
	missingMapping.ChatID = "missing"
	if err = state.RemapConversation(ctx, missingMapping); !errors.Is(err, channel.ErrNotFound) {
		t.Fatalf("missing RemapConversation() = %v", err)
	}
	invalidMapping := mapping
	invalidMapping.ChatID = ""
	if err = state.RemapConversation(ctx, invalidMapping); !errors.Is(err, channel.ErrInvalid) {
		t.Fatalf("invalid RemapConversation() = %v", err)
	}
	if err = state.Revoke(ctx, binding.ID, "bob", now); !errors.Is(err, channel.ErrNotFound) {
		t.Fatalf("foreign Revoke() = %v", err)
	}
	if err = state.Revoke(ctx, binding.ID, "alice", now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	rebound, err := state.Bind(ctx, other)
	if err != nil || rebound.ID != binding.ID || rebound.UserID != "bob" {
		t.Fatalf("rebound = %#v, %v", rebound, err)
	}
	if _, err = state.Conversation(ctx, binding.ID, "chat", ""); !errors.Is(err, channel.ErrNotFound) {
		t.Fatalf("transferred conversation = %v", err)
	}
	if err = state.DeleteThread(ctx, thread.ID); err != nil {
		t.Fatal(err)
	}
	if err = state.Complete(ctx, "missing", true); !errors.Is(err, channel.ErrInvalid) {
		t.Fatalf("Complete(missing) = %v", err)
	}
	if _, err = state.Begin(ctx, "", now, time.Hour); !errors.Is(err, channel.ErrInvalid) {
		t.Fatalf("Begin(invalid) = %v", err)
	}
}

func assertSQLiteChannelBindingBoundaries(t *testing.T, state channel.State, binding channel.Binding, now time.Time) channel.Binding {
	t.Helper()
	ctx := context.Background()
	binding.WorkspaceName = "Updated"
	updated, err := state.Bind(ctx, binding)
	if err != nil || updated.WorkspaceName != "Updated" || updated.ID != binding.ID {
		t.Fatalf("same-owner Bind() = %#v, %v", updated, err)
	}
	if _, err = state.Bind(ctx, channel.Binding{}); !errors.Is(err, channel.ErrInvalid) {
		t.Fatalf("invalid Bind() = %v", err)
	}
	if _, err = state.Resolve(ctx, "webhook", "workspace", "missing"); !errors.Is(err, channel.ErrNotFound) {
		t.Fatalf("missing Resolve() = %v", err)
	}
	other, _ := channel.NewBinding("bob", "webhook", "workspace", "external", now.Add(time.Second))
	if _, err := state.Bind(ctx, other); !errors.Is(err, channel.ErrConflict) {
		t.Fatalf("foreign Bind() = %v", err)
	}
	if value := formatChannelTime(time.Time{}); value != "" {
		t.Fatalf("formatChannelTime(zero) = %q", value)
	}
	return other
}

func TestSQLiteChannelConnectCodesPersistAndConsumeAtomically(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "channel-connect.db")
	database := openSQLite(t, path)
	state := database.ChannelState()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := state.IssueConnectCode(ctx, "alice", "slack", now, time.Minute, 0); !errors.Is(err, channel.ErrInvalid) {
		t.Fatalf("invalid pending cap = %v", err)
	}
	if _, err := state.IssueConnectCode(ctx, "", "slack", now, time.Minute, 1); !errors.Is(err, channel.ErrInvalid) {
		t.Fatalf("invalid owner = %v", err)
	}
	if _, err := state.Connect(ctx, "invalid", channel.ConnectionIdentity{Provider: "slack", ExternalUserID: "U1"}, now); !errors.Is(err, channel.ErrInvalid) {
		t.Fatalf("invalid Connect() = %v", err)
	}
	code, err := state.IssueConnectCode(ctx, "alice", "slack", now, channel.ConnectCodeTTL, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = state.IssueConnectCode(ctx, "alice", "slack", now, channel.ConnectCodeTTL, 1); !errors.Is(err, channel.ErrBusy) {
		t.Fatalf("pending cap = %v", err)
	}
	if err = database.Close(); err != nil {
		t.Fatal(err)
	}
	database = openSQLite(t, path)
	defer func() { _ = database.Close() }()
	state = database.ChannelState()
	identity := channel.ConnectionIdentity{
		Provider: "slack", WorkspaceID: "team", WorkspaceName: "Team",
		ExternalUserID: "U1", ExternalUserName: "Alice",
	}
	if _, err = state.Connect(ctx, code.Code, channel.ConnectionIdentity{Provider: "discord", ExternalUserID: "U1"}, now); !errors.Is(err, channel.ErrNotFound) {
		t.Fatalf("provider mismatch = %v", err)
	}
	bound, err := state.Connect(ctx, code.Code, identity, now)
	if err != nil || bound.UserID != "alice" || bound.ExternalUserName != "Alice" {
		t.Fatalf("Connect() = %#v, %v", bound, err)
	}
	if _, err = state.Connect(ctx, code.Code, identity, now); !errors.Is(err, channel.ErrNotFound) {
		t.Fatalf("reused code = %v", err)
	}
	transferCode, err := state.IssueConnectCode(ctx, "bob", "slack", now, time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = state.Connect(ctx, transferCode.Code, identity, now); !errors.Is(err, channel.ErrConflict) {
		t.Fatalf("connected identity transfer = %v", err)
	}
	if err = state.Revoke(ctx, bound.ID, "alice", now); err != nil {
		t.Fatal(err)
	}
	transferred, err := state.Connect(ctx, transferCode.Code, identity, now)
	if err != nil || transferred.ID != bound.ID || transferred.UserID != "bob" {
		t.Fatalf("revoked identity transfer = %#v, %v", transferred, err)
	}
	expired, err := state.IssueConnectCode(ctx, "bob", "slack", now, time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = state.Connect(ctx, expired.Code, channel.ConnectionIdentity{Provider: "slack", ExternalUserID: "U2"}, now.Add(time.Minute)); !errors.Is(err, channel.ErrNotFound) {
		t.Fatalf("expired code = %v", err)
	}
}

func assertSQLiteChannelMappingBoundaries(t *testing.T, state channel.State, database *SQL, mapping channel.Conversation, thread domain.Thread, now time.Time) {
	t.Helper()
	otherThread, _ := domain.NewThread(now)
	_ = database.CreateThread(context.Background(), otherThread)
	mapping.ThreadID = otherThread.ID
	second, created, err := state.MapConversation(context.Background(), mapping)
	if err != nil || created || second.ThreadID != thread.ID {
		t.Fatalf("duplicate mapping = %#v, %v, %v", second, created, err)
	}
	invalidMapping := mapping
	invalidMapping.ChatID = ""
	if _, _, err = state.MapConversation(context.Background(), invalidMapping); !errors.Is(err, channel.ErrInvalid) {
		t.Fatalf("invalid mapping = %v", err)
	}
	missingBinding := mapping
	missingBinding.BindingID = "chn_11111111111111111111111111111111"
	if _, _, err = state.MapConversation(context.Background(), missingBinding); !errors.Is(err, channel.ErrNotFound) {
		t.Fatalf("missing binding mapping = %v", err)
	}
	wrongProvider := mapping
	wrongProvider.Provider = "slack"
	if _, _, err = state.MapConversation(context.Background(), wrongProvider); !errors.Is(err, channel.ErrInvalid) {
		t.Fatalf("wrong provider mapping = %v", err)
	}
}

func TestSQLiteChannelStatePropagatesCancelledOperations(t *testing.T) {
	t.Parallel()
	database := openSQLite(t, filepath.Join(t.TempDir(), "channels.db"))
	defer func() { _ = database.Close() }()
	state := database.ChannelState()
	now := time.Now().UTC()
	binding, _ := channel.NewBinding("alice", "webhook", "workspace", "external", now)
	thread, _ := domain.NewThread(now)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := state.Bind(ctx, binding); err == nil {
		t.Fatal("Bind(cancelled) succeeded")
	}
	if _, err := state.Bindings(ctx, "alice"); err == nil {
		t.Fatal("Bindings(cancelled) succeeded")
	}
	if err := state.Revoke(ctx, binding.ID, "alice", now); err == nil {
		t.Fatal("Revoke(cancelled) succeeded")
	}
	if _, err := state.Resolve(ctx, "webhook", "workspace", "external"); err == nil {
		t.Fatal("Resolve(cancelled) succeeded")
	}
	if _, err := state.Conversation(ctx, binding.ID, "chat", ""); err == nil {
		t.Fatal("Conversation(cancelled) succeeded")
	}
	mapping := channel.Conversation{BindingID: binding.ID, Provider: "webhook", ChatID: "chat", ThreadID: thread.ID, CreatedAt: now, UpdatedAt: now}
	if _, _, err := state.MapConversation(ctx, mapping); err == nil {
		t.Fatal("MapConversation(cancelled) succeeded")
	}
	if err := state.RemapConversation(ctx, mapping); err == nil {
		t.Fatal("RemapConversation(cancelled) succeeded")
	}
	if _, err := state.IssueConnectCode(ctx, "alice", "slack", now, time.Minute, 1); err == nil {
		t.Fatal("IssueConnectCode(cancelled) succeeded")
	}
	if _, err := state.Connect(ctx, "cnc_"+strings.Repeat("a", 48), channel.ConnectionIdentity{Provider: "slack", ExternalUserID: "U1"}, now); err == nil {
		t.Fatal("Connect(cancelled) succeeded")
	}
	if err := state.DeleteThread(ctx, thread.ID); err == nil {
		t.Fatal("DeleteThread(cancelled) succeeded")
	}
	if _, err := state.Begin(ctx, "delivery", now, time.Hour); err == nil {
		t.Fatal("Begin(cancelled) succeeded")
	}
	if err := state.Complete(ctx, "delivery", true); err == nil {
		t.Fatal("Complete(cancelled) succeeded")
	}
}
