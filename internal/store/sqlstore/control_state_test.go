package sqlstore

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/control"
	"github.com/Rememorio/gofer/internal/domain"
)

func TestSQLiteControlStatePersistsAndComparesVersions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "control.db")
	database, err := Open(ctx, Config{Driver: SQLite, DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	thread, err := domain.NewThread(time.Now())
	if err != nil || database.CreateThread(ctx, thread) != nil {
		t.Fatalf("create thread: %v", err)
	}
	stateStore := database.ControlState()
	now := time.Now().UTC()
	next := control.State{ThreadID: thread.ID, Goal: &control.Goal{Objective: "ship", Status: control.GoalActive, StartedAt: now}, Todos: []control.Todo{}, UpdatedAt: now}
	saved, err := stateStore.CompareAndSwap(ctx, next, 0)
	if err != nil || saved.Version != 1 {
		t.Fatalf("CompareAndSwap() = %#v, %v", saved, err)
	}
	if _, err = stateStore.CompareAndSwap(ctx, next, 0); !errors.Is(err, control.ErrConflict) {
		t.Fatalf("stale CompareAndSwap() = %v", err)
	}
	next = saved
	next.UpdatedAt = now.Add(time.Second)
	updated, err := stateStore.CompareAndSwap(ctx, next, saved.Version)
	if err != nil || updated.Version != 2 {
		t.Fatalf("update CompareAndSwap() = %#v, %v", updated, err)
	}
	_ = database.Close()
	database, err = Open(ctx, Config{Driver: SQLite, DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	stateStore = database.ControlState()
	loaded, err := stateStore.Load(ctx, thread.ID)
	if err != nil || loaded.Version != 2 || loaded.Goal == nil || loaded.Goal.Objective != "ship" {
		t.Fatalf("Load() = %#v, %v", loaded, err)
	}
	if err = stateStore.Delete(ctx, thread.ID); err != nil {
		t.Fatal(err)
	}
	loaded, err = stateStore.Load(ctx, thread.ID)
	if err != nil || loaded.Version != 0 {
		t.Fatalf("Load(deleted) = %#v, %v", loaded, err)
	}
}

func TestSQLControlStateRejectsInvalidInput(t *testing.T) {
	t.Parallel()
	database := openSQLite(t, ":memory:")
	stateStore := database.ControlState()
	if _, err := stateStore.Load(context.Background(), "bad"); !errors.Is(err, control.ErrInvalid) {
		t.Fatalf("Load(invalid) = %v", err)
	}
	if err := stateStore.Delete(context.Background(), "bad"); !errors.Is(err, control.ErrInvalid) {
		t.Fatalf("Delete(invalid) = %v", err)
	}
}
