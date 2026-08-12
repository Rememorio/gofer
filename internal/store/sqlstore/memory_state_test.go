package sqlstore

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/memory"
)

func TestSQLiteMemoryStatePersistsRanksAndIsolates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "memory.db")
	database, err := Open(ctx, Config{Driver: SQLite, DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	stateStore := database.MemoryState()
	now := time.Now().UTC().Truncate(time.Microsecond)
	global := memory.Scope{UserID: "alice"}
	thread := memory.Scope{UserID: "alice", ThreadID: "thread"}
	seedAndAssertSQLMemory(t, stateStore, global, thread, now)
	_ = database.Close()
	database, err = Open(ctx, Config{Driver: SQLite, DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	assertPersistedAndReplaceSQLMemory(t, database.MemoryState(), global, thread, now)
}

func seedAndAssertSQLMemory(t *testing.T, stateStore memory.Store, global, thread memory.Scope, now time.Time) {
	t.Helper()
	ctx := context.Background()
	entries := []memory.Entry{
		{ID: "global", Scope: global, Text: "Go preference", Tags: []string{"code"}, Category: "preference", Confidence: 0.9, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour)},
		{ID: "thread", Scope: thread, Text: "Go runtime", Tags: []string{"code"}, CreatedAt: now, UpdatedAt: now},
		{ID: "other", Scope: memory.Scope{UserID: "bob"}, Text: "Go secret", CreatedAt: now, UpdatedAt: now},
		{ID: "expired", Scope: thread, Text: "Go expired", CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour), ExpiresAt: now},
	}
	for _, entry := range entries {
		if err := stateStore.Upsert(ctx, entry); err != nil {
			t.Fatal(err)
		}
	}
	matches, err := stateStore.Search(ctx, memory.Query{Scope: thread, Text: "Go", Tags: []string{"code"}, Limit: 10, Now: now})
	if err != nil || len(matches) != 2 || matches[0].Entry.ID != "thread" || matches[1].Entry.ID != "global" {
		t.Fatalf("Search() = %#v, %v", matches, err)
	}
	updated := entries[0]
	updated.Text = "Go updated"
	updated.UpdatedAt = now
	if err = stateStore.Upsert(ctx, updated); err != nil {
		t.Fatal(err)
	}
	got, err := stateStore.Get(ctx, global, "global")
	if err != nil || got.Text != "Go updated" || got.Category != "preference" {
		t.Fatalf("Get() = %#v, %v", got, err)
	}
	changedScope := updated
	changedScope.Scope = memory.Scope{UserID: "mallory"}
	if err = stateStore.Upsert(ctx, changedScope); !errors.Is(err, memory.ErrInvalid) {
		t.Fatalf("Upsert(scope change) = %v", err)
	}
}

func assertPersistedAndReplaceSQLMemory(t *testing.T, stateStore memory.Store, global, thread memory.Scope, now time.Time) {
	t.Helper()
	ctx := context.Background()
	got, err := stateStore.Get(ctx, global, "global")
	if err != nil || got.Text != "Go updated" {
		t.Fatalf("persisted Get() = %#v, %v", got, err)
	}
	conflicting := []memory.Entry{{ID: "other", Scope: global, Text: "collision", CreatedAt: now, UpdatedAt: now}}
	if err = stateStore.Replace(ctx, global, conflicting); !errors.Is(err, memory.ErrInvalid) {
		t.Fatalf("Replace(conflict) = %v", err)
	}
	if _, err = stateStore.Get(ctx, global, "global"); err != nil {
		t.Fatalf("conflicting replace was not atomic: %v", err)
	}
	replacement := []memory.Entry{{ID: "replacement", Scope: global, Text: "Rust preference", CreatedAt: now, UpdatedAt: now}}
	if err = stateStore.Replace(ctx, global, replacement); err != nil {
		t.Fatal(err)
	}
	if _, err = stateStore.Get(ctx, global, "global"); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("old entry = %v", err)
	}
	if _, err = stateStore.Get(ctx, thread, "thread"); err != nil {
		t.Fatalf("thread entry removed: %v", err)
	}
	if err = stateStore.Delete(ctx, global, "replacement"); err != nil {
		t.Fatalf("Delete() = %v", err)
	}
	if err = stateStore.Delete(ctx, global, "missing"); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("Delete(missing) = %v", err)
	}
	if err = stateStore.Clear(ctx, thread); err != nil {
		t.Fatal(err)
	}
	if _, err = stateStore.Get(ctx, thread, "thread"); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("cleared thread = %v", err)
	}
}

func TestSQLMemoryStateRejectsInvalidScopesAndImports(t *testing.T) {
	t.Parallel()
	stateStore := openSQLite(t, ":memory:").MemoryState()
	ctx := context.Background()
	invalid := memory.Scope{}
	if _, err := stateStore.Get(ctx, invalid, "id"); !errors.Is(err, memory.ErrInvalid) {
		t.Fatalf("Get(invalid) = %v", err)
	}
	if err := stateStore.Delete(ctx, invalid, "id"); !errors.Is(err, memory.ErrInvalid) {
		t.Fatalf("Delete(invalid) = %v", err)
	}
	if err := stateStore.Clear(ctx, invalid); !errors.Is(err, memory.ErrInvalid) {
		t.Fatalf("Clear(invalid) = %v", err)
	}
	if err := stateStore.Replace(ctx, memory.Scope{UserID: "u"}, []memory.Entry{{}}); !errors.Is(err, memory.ErrInvalid) {
		t.Fatalf("Replace(invalid) = %v", err)
	}
	if _, err := stateStore.Search(ctx, memory.Query{}); !errors.Is(err, memory.ErrInvalid) {
		t.Fatalf("Search(invalid) = %v", err)
	}
}
