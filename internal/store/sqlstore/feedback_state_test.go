package sqlstore

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/feedback"
)

func TestSQLiteFeedbackPersistsAndAggregates(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "feedback.db")
	database := openSQLite(t, path)
	thread, run := seed(t, database)
	alice, bob := seedSQLFeedback(t, database.FeedbackState(), thread, run)
	_ = database.Close()
	database = openSQLite(t, path)
	defer func() { _ = database.Close() }()
	assertPersistedSQLFeedback(t, database.FeedbackState(), thread, run, alice, bob)
}

func seedSQLFeedback(t *testing.T, stateStore feedback.Store, thread domain.Thread, run domain.Run) (feedback.Entry, feedback.Entry) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	messageID, _ := domain.NewMessageID()
	alice, _ := feedback.NewEntry(thread.ID, run.ID, "alice", 1, string(messageID), "good", now)
	if err := stateStore.Create(ctx, alice); err != nil {
		t.Fatal(err)
	}
	duplicate, _ := feedback.NewEntry(thread.ID, run.ID, "alice", -1, "", "bad", now)
	if err := stateStore.Create(ctx, duplicate); !errors.Is(err, feedback.ErrExists) {
		t.Fatalf("Create(duplicate) = %v, want ErrExists", err)
	}
	bob, _ := feedback.NewEntry(thread.ID, run.ID, "bob", -1, "", "", now.Add(time.Second))
	if err := stateStore.Create(ctx, bob); err != nil {
		t.Fatal(err)
	}
	updated, err := stateStore.Upsert(ctx, thread.ID, run.ID, "alice", -1, "revised", now.Add(2*time.Second))
	if err != nil || updated.ID != alice.ID || updated.MessageID != "" || updated.Comment != "revised" {
		t.Fatalf("Upsert() = %#v, %v", updated, err)
	}
	stats, err := stateStore.Stats(ctx, thread.ID, run.ID)
	if err != nil || stats.Total != 2 || stats.Negative != 2 {
		t.Fatalf("Stats() = %#v, %v", stats, err)
	}
	return alice, bob
}

func assertPersistedSQLFeedback(t *testing.T, stateStore feedback.Store, thread domain.Thread, run domain.Run, alice, bob feedback.Entry) {
	t.Helper()
	ctx := context.Background()
	got, err := stateStore.Get(ctx, alice.ID, "alice")
	if err != nil || got.Comment != "revised" {
		t.Fatalf("persisted Get() = %#v, %v", got, err)
	}
	entries, err := stateStore.ListRun(ctx, thread.ID, run.ID, "alice", 0)
	if err != nil || len(entries) != 1 || entries[0].ID != alice.ID {
		t.Fatalf("ListRun() = %#v, %v", entries, err)
	}
	if _, err = stateStore.Get(ctx, alice.ID, "bob"); !errors.Is(err, feedback.ErrNotFound) {
		t.Fatalf("Get(other user) = %v", err)
	}
	if err = stateStore.DeleteRunUser(ctx, thread.ID, run.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	if err = stateStore.DeleteRunUser(ctx, thread.ID, run.ID, "alice"); !errors.Is(err, feedback.ErrNotFound) {
		t.Fatalf("DeleteRunUser(missing) = %v", err)
	}
	if err = stateStore.Delete(ctx, bob.ID, "bob"); err != nil {
		t.Fatal(err)
	}
	if err = stateStore.Delete(ctx, bob.ID, "bob"); !errors.Is(err, feedback.ErrNotFound) {
		t.Fatalf("Delete(missing) = %v", err)
	}
}

func TestSQLFeedbackValidationAndThreadCleanup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := openSQLite(t, ":memory:")
	defer func() { _ = database.Close() }()
	thread, run := seed(t, database)
	stateStore := database.FeedbackState()
	entry, _ := feedback.NewEntry(thread.ID, run.ID, "alice", 1, "", "", time.Now())
	if err := stateStore.Create(ctx, entry); err != nil {
		t.Fatal(err)
	}
	if _, err := stateStore.ListRun(ctx, thread.ID, run.ID, "", 1); !errors.Is(err, feedback.ErrInvalid) {
		t.Fatalf("ListRun(invalid) = %v", err)
	}
	if _, err := stateStore.Stats(ctx, thread.ID, "bad"); !errors.Is(err, feedback.ErrInvalid) {
		t.Fatalf("Stats(invalid) = %v", err)
	}
	if err := stateStore.Delete(ctx, "bad", "alice"); !errors.Is(err, feedback.ErrInvalid) {
		t.Fatalf("Delete(invalid) = %v", err)
	}
	if err := stateStore.DeleteThread(ctx, "bad"); !errors.Is(err, feedback.ErrInvalid) {
		t.Fatalf("DeleteThread(invalid) = %v", err)
	}
	if err := stateStore.DeleteThread(ctx, thread.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := stateStore.Get(ctx, entry.ID, "alice"); !errors.Is(err, feedback.ErrNotFound) {
		t.Fatalf("Get(after cleanup) = %v", err)
	}
}
