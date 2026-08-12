package sqlstore

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/store"
)

func TestSQLiteThreadCatalogPatchListAndDelete(t *testing.T) {
	t.Parallel()
	database := openSQLite(t, filepath.Join(t.TempDir(), "gofer.db"))
	defer func() { _ = database.Close() }()
	now := time.Now().UTC()
	thread, _ := domain.NewThread(now)
	thread.Title = "Initial"
	thread.Metadata = map[string]string{store.OwnerMetadataKey: "alice", "project": "gofer"}
	if err := database.CreateThread(context.Background(), thread); err != nil {
		t.Fatal(err)
	}
	run, _ := domain.NewRun(thread.ID, now)
	if err := database.CreateRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	title := "Updated"
	patched, err := database.PatchThread(context.Background(), thread.ID, store.ThreadPatch{Title: &title, Metadata: map[string]string{"pinned": "true"}}, now.Add(time.Second))
	if err != nil || patched.Title != title || patched.Metadata["project"] != "gofer" || patched.Metadata["pinned"] != "true" {
		t.Fatalf("PatchThread() = %#v, %v", patched, err)
	}
	threads, err := database.Threads(context.Background(), store.ThreadQuery{OwnerID: "alice", Text: "date", Metadata: map[string]string{"project": "gofer"}})
	if err != nil || len(threads) != 1 || threads[0].ID != thread.ID {
		t.Fatalf("Threads() = %#v, %v", threads, err)
	}
	runs, err := database.Runs(context.Background(), thread.ID)
	if err != nil || len(runs) != 1 || runs[0].ID != run.ID {
		t.Fatalf("Runs() = %#v, %v", runs, err)
	}
	if err = database.DeleteThread(context.Background(), thread.ID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("DeleteThread(active) = %v", err)
	}
	running, _ := database.TransitionRun(context.Background(), run.ID, domain.RunPending, domain.RunRunning, now.Add(time.Second), "")
	_, _ = database.TransitionRun(context.Background(), running.ID, domain.RunRunning, domain.RunSucceeded, now.Add(2*time.Second), "")
	if err = database.DeleteThread(context.Background(), thread.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = database.Thread(context.Background(), thread.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Thread(deleted) = %v", err)
	}
}

func TestSQLiteSetsGeneratedTitleOnlyWhenEmpty(t *testing.T) {
	t.Parallel()
	database := openSQLite(t, filepath.Join(t.TempDir(), "gofer.db"))
	defer func() { _ = database.Close() }()
	now := time.Now().UTC()
	thread, _ := domain.NewThread(now)
	if err := database.CreateThread(context.Background(), thread); err != nil {
		t.Fatal(err)
	}
	generated, changed, err := database.SetThreadTitleIfEmpty(context.Background(), thread.ID, " Generated ", now.Add(time.Second))
	if err != nil || !changed || generated.Title != "Generated" {
		t.Fatalf("SetThreadTitleIfEmpty() = %#v, %v, %v", generated, changed, err)
	}
	generated, changed, err = database.SetThreadTitleIfEmpty(context.Background(), thread.ID, "Replacement", now.Add(2*time.Second))
	if err != nil || changed || generated.Title != "Generated" {
		t.Fatalf("SetThreadTitleIfEmpty(existing) = %#v, %v, %v", generated, changed, err)
	}
	missing, _ := domain.NewThreadID()
	if _, _, err = database.SetThreadTitleIfEmpty(context.Background(), missing, "x", now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("SetThreadTitleIfEmpty(missing) = %v", err)
	}
	if _, _, err = database.SetThreadTitleIfEmpty(context.Background(), thread.ID, " ", now); !errors.Is(err, store.ErrInvalidQuery) {
		t.Fatalf("SetThreadTitleIfEmpty(invalid) = %v", err)
	}
	older, _ := domain.NewThread(now)
	if err = database.CreateThread(context.Background(), older); err != nil {
		t.Fatal(err)
	}
	if _, _, err = database.SetThreadTitleIfEmpty(context.Background(), older.ID, "Older", now.Add(-time.Second)); !errors.Is(err, domain.ErrInvalidThread) {
		t.Fatalf("SetThreadTitleIfEmpty(old timestamp) = %v", err)
	}
	stored, _ := database.Thread(context.Background(), older.ID)
	if stored.Title != "" {
		t.Fatalf("failed title transaction persisted %q", stored.Title)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err = database.SetThreadTitleIfEmpty(cancelled, older.ID, "Cancelled", now.Add(time.Second)); !errors.Is(err, context.Canceled) {
		t.Fatalf("SetThreadTitleIfEmpty(cancelled) = %v", err)
	}
}

func TestSQLiteThreadCatalogErrorsAndEmptyPages(t *testing.T) {
	t.Parallel()
	database := openSQLite(t, filepath.Join(t.TempDir(), "gofer.db"))
	defer func() { _ = database.Close() }()
	missing, _ := domain.NewThread(time.Now())
	if _, err := database.PatchThread(context.Background(), missing.ID, store.ThreadPatch{Metadata: map[string]string{"x": "y"}}, time.Now()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("PatchThread(missing) = %v", err)
	}
	if err := database.DeleteThread(context.Background(), missing.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("DeleteThread(missing) = %v", err)
	}
	if _, err := database.Runs(context.Background(), missing.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Runs(missing) = %v", err)
	}
	threads, err := database.Threads(context.Background(), store.ThreadQuery{OwnerID: "local", Offset: 100})
	if err != nil || len(threads) != 0 {
		t.Fatalf("Threads(empty) = %#v, %v", threads, err)
	}
	thread, _ := domain.NewThread(time.Now())
	if err = database.CreateThread(context.Background(), thread); err != nil {
		t.Fatal(err)
	}
	if _, err = database.PatchThread(context.Background(), thread.ID, store.ThreadPatch{Metadata: map[string]string{"team": "core"}}, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	threads, err = database.Threads(context.Background(), store.ThreadQuery{OwnerID: "local", Metadata: map[string]string{"team": "other"}})
	if err != nil || len(threads) != 0 {
		t.Fatalf("Threads(metadata miss) = %#v, %v", threads, err)
	}
	if _, err = database.PatchThread(context.Background(), thread.ID, store.ThreadPatch{Metadata: map[string]string{"x": "y"}}, time.Time{}); !errors.Is(err, store.ErrInvalidQuery) {
		t.Fatalf("PatchThread(invalid time) = %v", err)
	}
	if err = database.DeleteThread(context.Background(), thread.ID); err != nil {
		t.Fatal(err)
	}
}

func TestThreadQueryDialectAndLiteralEscaping(t *testing.T) {
	t.Parallel()
	query, err := (store.ThreadQuery{OwnerID: "alice", Text: `100%_done`, Metadata: map[string]string{"team": "core"}, Limit: 10}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	postgres := &SQL{driver: Postgres}
	statement, arguments := postgres.threadQuery(query)
	if !strings.Contains(statement, "metadata::jsonb") || !strings.Contains(statement, "$7") || len(arguments) != 7 {
		t.Fatalf("postgres query = %q, %#v", statement, arguments)
	}
	if got := arguments[2]; got != `%100\%\_done%` {
		t.Fatalf("escaped search = %q", got)
	}
	sqlite := &SQL{driver: SQLite}
	statement, arguments = sqlite.threadQuery(query)
	if !strings.Contains(statement, "json_each") || arguments[3] != "team" {
		t.Fatalf("sqlite query = %q, %#v", statement, arguments)
	}
}
