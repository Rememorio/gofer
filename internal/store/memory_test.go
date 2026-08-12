package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/event"
)

func TestMemoryThreadAndRunLifecycle(t *testing.T) {
	t.Parallel()

	memory, thread, run := seededMemory(t)
	ctx := context.Background()

	thread.Metadata = map[string]string{"owner": "alice"}
	other := thread
	other.ID, _ = domain.NewThreadID()
	if err := memory.CreateThread(ctx, other); err != nil {
		t.Fatalf("CreateThread(other): %v", err)
	}
	other.Metadata["owner"] = "mutated"
	stored, err := memory.Thread(ctx, other.ID)
	if err != nil {
		t.Fatalf("Thread(): %v", err)
	}
	if stored.Metadata["owner"] != "alice" {
		t.Fatalf("stored metadata = %#v, want isolated copy", stored.Metadata)
	}

	started, err := memory.TransitionRun(ctx, run.ID, domain.RunPending, domain.RunRunning, time.Now(), "")
	if err != nil {
		t.Fatalf("TransitionRun(): %v", err)
	}
	if started.Status != domain.RunRunning {
		t.Fatalf("status = %s, want running", started.Status)
	}
	if _, err := memory.TransitionRun(ctx, run.ID, domain.RunPending, domain.RunSucceeded, time.Now(), ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("TransitionRun(conflict) error = %v, want ErrConflict", err)
	}
	got, err := memory.Run(ctx, run.ID)
	if err != nil || got.Status != domain.RunRunning {
		t.Fatalf("Run() = %#v, %v", got, err)
	}
}

func TestMemoryRejectsDuplicatesAndMissingParents(t *testing.T) {
	t.Parallel()

	memory, thread, run := seededMemory(t)
	ctx := context.Background()
	if err := memory.CreateThread(ctx, thread); !errors.Is(err, ErrExists) {
		t.Fatalf("CreateThread(duplicate) error = %v, want ErrExists", err)
	}
	if err := memory.CreateRun(ctx, run); !errors.Is(err, ErrExists) {
		t.Fatalf("CreateRun(duplicate) error = %v, want ErrExists", err)
	}
	missingThread, err := domain.NewThread(time.Now())
	if err != nil {
		t.Fatalf("NewThread(): %v", err)
	}
	missingRun, err := domain.NewRun(missingThread.ID, time.Now())
	if err != nil {
		t.Fatalf("NewRun(): %v", err)
	}
	if err := memory.CreateRun(ctx, missingRun); !errors.Is(err, ErrNotFound) {
		t.Fatalf("CreateRun(missing parent) error = %v, want ErrNotFound", err)
	}
}

func TestMemoryJournalAndWatch(t *testing.T) {
	t.Parallel()

	memory, thread, run := seededMemory(t)
	ctx, cancel := context.WithCancel(context.Background())
	watch, err := memory.Watch(ctx, run.ID, 0)
	if err != nil {
		t.Fatalf("Watch(): %v", err)
	}
	drafts := []event.Draft{
		newDraft(t, thread.ID, run.ID, event.RunCreated, map[string]string{"status": "pending"}),
		newDraft(t, thread.ID, run.ID, event.RunStarted, nil),
	}
	committed, err := memory.Append(ctx, run.ID, 0, drafts...)
	if err != nil {
		t.Fatalf("Append(): %v", err)
	}
	if len(committed) != 2 || committed[0].Sequence != 1 || committed[1].Sequence != 2 {
		t.Fatalf("committed events = %#v", committed)
	}
	committed[0].Data[0] = '['

	select {
	case latest := <-watch:
		if latest != 2 {
			t.Fatalf("watch sequence = %d, want 2", latest)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for journal notification")
	}

	records, err := memory.Events(ctx, run.ID, 0, 1)
	if err != nil {
		t.Fatalf("Events(): %v", err)
	}
	if len(records) != 1 || records[0].Sequence != 1 {
		t.Fatalf("Events() = %#v", records)
	}
	if records[0].Data[0] != '{' {
		t.Fatal("journal data was mutated through returned event")
	}
	records, err = memory.Events(ctx, run.ID, 1, 0)
	if err != nil || len(records) != 1 || records[0].Sequence != 2 {
		t.Fatalf("Events(after 1) = %#v, %v", records, err)
	}

	cancel()
	select {
	case _, open := <-watch:
		if open {
			t.Fatal("watch remains open after cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for watch cleanup")
	}
}

func TestMemoryWatchReplaysCurrentSequence(t *testing.T) {
	t.Parallel()

	memory, thread, run := seededMemory(t)
	draft := newDraft(t, thread.ID, run.ID, event.RunCreated, nil)
	if _, err := memory.Append(context.Background(), run.ID, 0, draft); err != nil {
		t.Fatalf("Append(): %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watch, err := memory.Watch(ctx, run.ID, 0)
	if err != nil {
		t.Fatalf("Watch(): %v", err)
	}
	if latest := <-watch; latest != 1 {
		t.Fatalf("initial watch sequence = %d, want 1", latest)
	}
}

func TestMemoryAppendConflictsAtomically(t *testing.T) {
	t.Parallel()

	memory, thread, run := seededMemory(t)
	draft := newDraft(t, thread.ID, run.ID, event.RunCreated, nil)
	const writers = 16
	var wait sync.WaitGroup
	results := make(chan error, writers)
	for range writers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := memory.Append(context.Background(), run.ID, 0, draft)
			results <- err
		}()
	}
	wait.Wait()
	close(results)
	successes := 0
	conflicts := 0
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrConflict):
			conflicts++
		default:
			t.Fatalf("Append() unexpected error: %v", err)
		}
	}
	if successes != 1 || conflicts != writers-1 {
		t.Fatalf("successes = %d, conflicts = %d", successes, conflicts)
	}
}

func TestMemoryErrors(t *testing.T) {
	t.Parallel()

	memory := NewMemory()
	ctx := context.Background()
	missingRun, err := domain.NewRunID()
	if err != nil {
		t.Fatalf("NewRunID(): %v", err)
	}
	missingThread, err := domain.NewThreadID()
	if err != nil {
		t.Fatalf("NewThreadID(): %v", err)
	}
	if _, err := memory.Thread(ctx, missingThread); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Thread() error = %v, want ErrNotFound", err)
	}
	if _, err := memory.Run(ctx, missingRun); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Run() error = %v, want ErrNotFound", err)
	}
	if _, err := memory.Events(ctx, missingRun, 0, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Events() error = %v, want ErrNotFound", err)
	}
	if _, err := memory.Watch(ctx, missingRun, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Watch() error = %v, want ErrNotFound", err)
	}
	if _, err := memory.TransitionRun(ctx, missingRun, domain.RunPending, domain.RunRunning, time.Now(), ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("TransitionRun() error = %v, want ErrNotFound", err)
	}
	if _, err := memory.Append(ctx, missingRun, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Append() error = %v, want ErrNotFound", err)
	}
}

func TestMemoryHonorsCancellation(t *testing.T) {
	t.Parallel()

	memory, thread, run := seededMemory(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := memory.CreateThread(ctx, thread); !errors.Is(err, context.Canceled) {
		t.Fatalf("CreateThread() error = %v, want context.Canceled", err)
	}
	if _, err := memory.Run(ctx, run.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if _, err := memory.Events(ctx, run.ID, 0, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("Events() error = %v, want context.Canceled", err)
	}
	if _, err := memory.Watch(ctx, run.ID, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("Watch() error = %v, want context.Canceled", err)
	}
}

func TestMemoryRejectsEventScopeMismatch(t *testing.T) {
	t.Parallel()

	memory, _, run := seededMemory(t)
	other, err := domain.NewThread(time.Now())
	if err != nil {
		t.Fatalf("NewThread(): %v", err)
	}
	draft := newDraft(t, other.ID, run.ID, event.RunCreated, nil)
	if _, err := memory.Append(context.Background(), run.ID, 0, draft); err == nil {
		t.Fatal("Append() error = nil, want scope mismatch")
	}
}

func seededMemory(t *testing.T) (*Memory, domain.Thread, domain.Run) {
	t.Helper()
	now := time.Now().UTC()
	thread, err := domain.NewThread(now)
	if err != nil {
		t.Fatalf("NewThread(): %v", err)
	}
	run, err := domain.NewRun(thread.ID, now)
	if err != nil {
		t.Fatalf("NewRun(): %v", err)
	}
	memory := NewMemory()
	if err := memory.CreateThread(context.Background(), thread); err != nil {
		t.Fatalf("CreateThread(): %v", err)
	}
	if err := memory.CreateRun(context.Background(), run); err != nil {
		t.Fatalf("CreateRun(): %v", err)
	}
	return memory, thread, run
}

func newDraft(t *testing.T, threadID domain.ThreadID, runID domain.RunID, kind event.Kind, payload any) event.Draft {
	t.Helper()
	draft, err := event.NewDraft(threadID, runID, kind, time.Now(), payload)
	if err != nil {
		t.Fatalf("NewDraft(): %v", err)
	}
	return draft
}
