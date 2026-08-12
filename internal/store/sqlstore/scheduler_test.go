package sqlstore

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/scheduler"
)

func TestSQLiteScheduledTaskDefinitionLifecycle(t *testing.T) {
	t.Parallel()
	database := openSQLite(t, filepath.Join(t.TempDir(), "gofer.db"))
	defer func() { _ = database.Close() }()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	task := scheduledTask("task", scheduler.Cron, "* * * * *", now)
	if err := database.Create(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if err := database.Create(context.Background(), task); !errors.Is(err, scheduler.ErrConflict) {
		t.Fatalf("duplicate = %v", err)
	}
	got, err := database.Get(context.Background(), task.ID)
	if err != nil || got.ID != task.ID {
		t.Fatalf("Get() = %#v, %v", got, err)
	}
	listed, err := database.List(context.Background(), task.UserID)
	if err != nil || len(listed) != 1 {
		t.Fatalf("List() = %#v, %v", listed, err)
	}
	title, schedule := "updated", "*/5 * * * *"
	updated, err := database.Update(context.Background(), task.ID, task.UserID, scheduler.Update{Title: &title, Schedule: &schedule}, now)
	if err != nil || updated.Title != title || !updated.NextRunAt.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("Update() = %#v, %v", updated, err)
	}
	listed, err = database.List(context.Background(), "other")
	if err != nil || len(listed) != 0 {
		t.Fatalf("List(other) = %#v, %v", listed, err)
	}
}

func TestSQLiteScheduledTaskDispatchLifecycle(t *testing.T) {
	t.Parallel()
	database := openSQLite(t, filepath.Join(t.TempDir(), "gofer.db"))
	defer func() { _ = database.Close() }()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	task := scheduledTask("task", scheduler.Cron, "* * * * *", now)
	if err := database.Create(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	triggered, err := database.Trigger(context.Background(), task.ID, task.UserID, now)
	if err != nil || !triggered.NextRunAt.Equal(now) {
		t.Fatalf("Trigger() = %#v, %v", triggered, err)
	}
	due, err := database.Due(context.Background(), now, 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("Due() = %#v, %v", due, err)
	}
	claimed, err := database.Claim(context.Background(), task.ID, now, "worker", now, now.Add(time.Minute))
	if err != nil || claimed.Status != scheduler.Running {
		t.Fatalf("Claim() = %#v, %v", claimed, err)
	}
	if _, err = database.Claim(context.Background(), task.ID, now, "other", now, now.Add(time.Minute)); !errors.Is(err, scheduler.ErrConflict) {
		t.Fatalf("second Claim() = %v", err)
	}
	if err = database.Delete(context.Background(), task.ID, task.UserID); !errors.Is(err, scheduler.ErrConflict) {
		t.Fatalf("Delete(running) = %v", err)
	}
	finished, err := database.Finish(context.Background(), task.ID, "worker", now.Add(time.Second), now.Add(time.Minute), scheduler.DispatchResult{RunID: "run", ThreadID: "thread"}, nil)
	if err != nil || finished.Status != scheduler.Enabled || finished.LastRunID != "run" || finished.ThreadID != "thread" || finished.RunCount != 1 {
		t.Fatalf("Finish() = %#v, %v", finished, err)
	}
}

func TestSQLiteScheduledTaskDeletePaused(t *testing.T) {
	t.Parallel()
	database := openSQLite(t, filepath.Join(t.TempDir(), "gofer.db"))
	defer func() { _ = database.Close() }()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	task := scheduledTask("task", scheduler.Cron, "* * * * *", now)
	if err := database.Create(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	paused, err := database.SetStatus(context.Background(), task.ID, task.UserID, scheduler.Paused, now)
	if err != nil || paused.Status != scheduler.Paused {
		t.Fatalf("SetStatus() = %#v, %v", paused, err)
	}
	if err = database.Delete(context.Background(), task.ID, task.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err = database.Get(context.Background(), task.ID); !errors.Is(err, scheduler.ErrNotFound) {
		t.Fatalf("Get(deleted) = %v", err)
	}
}

func TestSQLiteScheduledTaskUpdatePreservesPause(t *testing.T) {
	t.Parallel()
	database := openSQLite(t, filepath.Join(t.TempDir(), "gofer.db"))
	defer func() { _ = database.Close() }()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	task := scheduledTask("task", scheduler.Cron, "* * * * *", now)
	if err := database.Create(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	paused, err := database.SetStatus(context.Background(), task.ID, task.UserID, scheduler.Paused, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	newTitle := "paused"
	updated, err := database.Update(context.Background(), task.ID, task.UserID, scheduler.Update{Title: &newTitle}, now.Add(2*time.Second))
	if err != nil || updated.Status != scheduler.Paused || !updated.NextRunAt.Equal(paused.NextRunAt) {
		t.Fatalf("Update(paused) = %#v, %v", updated, err)
	}
	if _, err = database.Update(context.Background(), task.ID, task.UserID, scheduler.Update{}, now); !errors.Is(err, scheduler.ErrInvalid) {
		t.Fatalf("Update(empty) = %v", err)
	}
}

func TestSQLiteScheduledTaskFailuresAndOnce(t *testing.T) {
	t.Parallel()
	database := openSQLite(t, filepath.Join(t.TempDir(), "gofer.db"))
	defer func() { _ = database.Close() }()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	task := scheduledTask("once", scheduler.Once, now.Add(time.Minute).Format(time.RFC3339), now)
	if err := database.Create(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Update(context.Background(), task.ID, "other", scheduler.Update{}, now); !errors.Is(err, scheduler.ErrNotFound) {
		t.Fatalf("Update(scope) = %v", err)
	}
	if _, err := database.Trigger(context.Background(), task.ID, "other", now); !errors.Is(err, scheduler.ErrNotFound) {
		t.Fatalf("Trigger(scope) = %v", err)
	}
	if _, err := database.SetStatus(context.Background(), task.ID, task.UserID, scheduler.Completed, now); !errors.Is(err, scheduler.ErrInvalid) {
		t.Fatalf("SetStatus(invalid) = %v", err)
	}
	triggered, _ := database.Trigger(context.Background(), task.ID, task.UserID, now)
	_, _ = database.Claim(context.Background(), task.ID, triggered.NextRunAt, "worker", now, now.Add(time.Minute))
	finished, err := database.Finish(context.Background(), task.ID, "worker", now, time.Time{}, scheduler.DispatchResult{}, errors.New("dispatch"))
	if err != nil || finished.Status != scheduler.Failed || finished.LastError != "dispatch" {
		t.Fatalf("Finish(failed) = %#v, %v", finished, err)
	}
	if _, err = database.Finish(context.Background(), task.ID, "worker", now, time.Time{}, scheduler.DispatchResult{}, nil); !errors.Is(err, scheduler.ErrConflict) {
		t.Fatalf("Finish(unleased) = %v", err)
	}
	if _, err = database.Due(context.Background(), now, 0); !errors.Is(err, scheduler.ErrInvalid) {
		t.Fatalf("Due(invalid) = %v", err)
	}
	if err = database.Delete(context.Background(), "missing", task.UserID); !errors.Is(err, scheduler.ErrNotFound) {
		t.Fatalf("Delete(missing) = %v", err)
	}
}

func scheduledTask(id string, kind scheduler.ScheduleType, spec string, now time.Time) scheduler.Task {
	return scheduler.Task{ID: id, UserID: "user", Title: id, Prompt: "prompt", ScheduleType: kind, Schedule: spec, Timezone: "UTC", Status: scheduler.Enabled, NextRunAt: now.Add(time.Minute), CreatedAt: now, UpdatedAt: now}
}
