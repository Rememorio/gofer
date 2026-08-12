package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type executorFunc func(context.Context, Task) (DispatchResult, error)

func (function executorFunc) Execute(ctx context.Context, task Task) (DispatchResult, error) {
	return function(ctx, task)
}

func TestSchedulesAndValidation(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	once, err := NextRun(Once, "2026-01-01T00:01:00Z", "UTC", now)
	if err != nil || !once.Equal(now.Add(time.Minute)) {
		t.Fatalf("once=%v %v", once, err)
	}
	next, err := NextRun(Cron, "*/5 * * * *", "UTC", now)
	if err != nil || !next.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("cron=%v %v", next, err)
	}
	for _, input := range []struct {
		kind       ScheduleType
		spec, zone string
	}{{Once, "bad", "UTC"}, {Cron, "bad", "UTC"}, {Cron, "* * * * *", "bad"}, {"bad", "x", "UTC"}, {Once, "2025-01-01T00:00:00Z", "UTC"}} {
		if _, err := NextRun(input.kind, input.spec, input.zone, now); !errors.Is(err, ErrInvalid) {
			t.Fatalf("NextRun(%#v)=%v", input, err)
		}
	}
}

func TestEngineDispatchesOnceAndCron(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	once := taskAt("once", Once, "2026-01-01T00:00:00Z", now)
	recurring := taskAt("cron", Cron, "* * * * *", now)
	for _, task := range []Task{once, recurring} {
		if err := store.Create(context.Background(), task); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	calls := []string{}
	engine, err := New(Config{Store: store, Executor: executorFunc(func(_ context.Context, task Task) (DispatchResult, error) {
		mu.Lock()
		calls = append(calls, task.ID)
		mu.Unlock()
		return DispatchResult{RunID: "run-" + task.ID, ThreadID: "thread"}, nil
	}), Owner: "worker", LeaseDuration: time.Minute, BatchSize: 10, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	gotOnce, _ := store.Get(context.Background(), "once")
	gotCron, _ := store.Get(context.Background(), "cron")
	if gotOnce.Status != Completed || gotOnce.RunCount != 1 || gotCron.Status != Enabled || !gotCron.NextRunAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("once=%#v cron=%#v", gotOnce, gotCron)
	}
	if len(calls) != 2 {
		t.Fatalf("calls=%#v", calls)
	}
}

func TestEngineRunTaskDispatchesOnlyRequestedOccurrence(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	requested := taskAt("requested", Once, now.Format(time.RFC3339), now)
	other := taskAt("other", Once, now.Format(time.RFC3339), now)
	for _, task := range []Task{requested, other} {
		if err := store.Create(context.Background(), task); err != nil {
			t.Fatal(err)
		}
	}
	engine, err := New(Config{Store: store, Executor: executorFunc(func(_ context.Context, task Task) (DispatchResult, error) {
		return DispatchResult{RunID: "run-" + task.ID}, nil
	}), Owner: "worker", LeaseDuration: time.Minute, BatchSize: 10, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if err = engine.RunTask(context.Background(), requested); err != nil {
		t.Fatal(err)
	}
	completed, _ := store.Get(context.Background(), requested.ID)
	untouched, _ := store.Get(context.Background(), other.ID)
	if completed.Status != Completed || untouched.Status != Enabled || untouched.RunCount != 0 {
		t.Fatalf("requested=%#v other=%#v", completed, untouched)
	}
}

func TestEngineFailureConflictAndLease(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	store := NewMemoryStore()
	task := taskAt("x", Once, now.Format(time.RFC3339), now)
	_ = store.Create(context.Background(), task)
	claimed, err := store.Claim(context.Background(), task.ID, task.NextRunAt, "one", now, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Claim(context.Background(), task.ID, task.NextRunAt, "two", now, now.Add(time.Minute)); !errors.Is(err, ErrConflict) {
		t.Fatalf("second claim=%v", err)
	}
	if _, err := store.Finish(context.Background(), claimed.ID, "two", now, time.Time{}, DispatchResult{}, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong finish=%v", err)
	}
	finished, err := store.Finish(context.Background(), claimed.ID, "one", now, time.Time{}, DispatchResult{}, errors.New("boom"))
	if err != nil || finished.Status != Failed || finished.LastError != "boom" {
		t.Fatalf("finished=%#v %v", finished, err)
	}
}

func TestMemoryStoreScopeStatusAndErrors(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	now := time.Now().UTC()
	task := taskAt("x", Cron, "* * * * *", now)
	if err := store.Create(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(context.Background(), task); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate=%v", err)
	}
	listed, _ := store.List(context.Background(), "u")
	if len(listed) != 1 {
		t.Fatal("list")
	}
	if _, err := store.Due(context.Background(), now, 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("due=%v", err)
	}
	paused, err := store.SetStatus(context.Background(), task.ID, "u", Paused, now)
	if err != nil || paused.Status != Paused {
		t.Fatalf("pause=%#v %v", paused, err)
	}
	if _, err := store.SetStatus(context.Background(), task.ID, "other", Enabled, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("scope=%v", err)
	}
	if _, err := store.SetStatus(context.Background(), task.ID, "u", Completed, now); !errors.Is(err, ErrInvalid) {
		t.Fatalf("status=%v", err)
	}
	if _, err := store.Get(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing=%v", err)
	}
}

func TestMemoryStoreUpdateTriggerAndDelete(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	task := taskAt("x", Cron, "* * * * *", now)
	if err := store.Create(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	title, prompt, schedule := " updated ", " next prompt ", "*/5 * * * *"
	updated, err := store.Update(context.Background(), task.ID, "u", Update{Title: &title, Prompt: &prompt, Schedule: &schedule}, now)
	if err != nil || updated.Title != "updated" || updated.Prompt != "next prompt" || !updated.NextRunAt.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("Update() = %#v, %v", updated, err)
	}
	triggered, err := store.Trigger(context.Background(), task.ID, "u", now.Add(time.Second))
	if err != nil || !triggered.NextRunAt.Equal(now.Add(time.Second)) {
		t.Fatalf("Trigger() = %#v, %v", triggered, err)
	}
	if _, err = store.Update(context.Background(), task.ID, "other", Update{}, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update(scope) = %v", err)
	}
	if _, err = store.Trigger(context.Background(), task.ID, "other", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Trigger(scope) = %v", err)
	}
	if err = store.Delete(context.Background(), task.ID, "other"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete(scope) = %v", err)
	}
	if err = store.Delete(context.Background(), task.ID, "u"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Get(context.Background(), task.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(deleted) = %v", err)
	}
}

func TestMemoryStoreUpdatePreservesPause(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	task := taskAt("x", Cron, "* * * * *", now)
	if err := store.Create(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	paused, err := store.SetStatus(context.Background(), task.ID, "u", Paused, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	newTitle := "still paused"
	updated, err := store.Update(context.Background(), task.ID, "u", Update{Title: &newTitle}, now.Add(2*time.Second))
	if err != nil || updated.Status != Paused || !updated.NextRunAt.Equal(paused.NextRunAt) {
		t.Fatalf("Update(paused title) = %#v, %v", updated, err)
	}
	newSchedule := "*/10 * * * *"
	updated, err = store.Update(context.Background(), task.ID, "u", Update{Schedule: &newSchedule}, now.Add(2*time.Second))
	if err != nil || updated.Status != Paused || !updated.NextRunAt.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("Update(paused schedule) = %#v, %v", updated, err)
	}
	if _, err = store.Update(context.Background(), task.ID, "u", Update{}, now); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Update(empty) = %v", err)
	}
}

func TestMemoryStoreStableDueOrderAndDispatchThread(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, id := range []string{"b", "a"} {
		if err := store.Create(context.Background(), taskAt(id, Cron, "* * * * *", now)); err != nil {
			t.Fatal(err)
		}
	}
	due, err := store.Due(context.Background(), now, 2)
	if err != nil || len(due) != 2 || due[0].ID != "a" || due[1].ID != "b" {
		t.Fatalf("Due() = %#v, %v", due, err)
	}
	claimed, err := store.Claim(context.Background(), "a", now, "worker", now, now.Add(time.Minute))
	if err != nil || !claimed.UpdatedAt.Equal(now) {
		t.Fatalf("Claim() = %#v, %v", claimed, err)
	}
	finished, err := store.Finish(context.Background(), "a", "worker", now, now.Add(time.Minute), DispatchResult{RunID: "run", ThreadID: "thread"}, nil)
	if err != nil || finished.ThreadID != "thread" {
		t.Fatalf("Finish() = %#v, %v", finished, err)
	}
}

func TestValidationConfigAndCancellation(t *testing.T) {
	t.Parallel()
	valid := taskAt("x", Cron, "* * * * *", time.Now().UTC())
	mutations := []func(*Task){func(task *Task) { task.ID = "" }, func(task *Task) { task.Status = "bad" }, func(task *Task) { task.NextRunAt = time.Time{} }, func(task *Task) { task.Schedule = "bad" }, func(task *Task) { task.Status = Running; task.LeaseOwner = "" }}
	for _, mutate := range mutations {
		candidate := valid
		mutate(&candidate)
		if err := candidate.Validate(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Validate=%v", err)
		}
	}
	config := Config{Store: NewMemoryStore(), Executor: executorFunc(func(context.Context, Task) (DispatchResult, error) { return DispatchResult{}, nil }), Owner: "x", LeaseDuration: time.Second, BatchSize: 1}
	if _, err := New(config); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Config){func(c *Config) { c.Store = nil }, func(c *Config) { c.Executor = nil }, func(c *Config) { c.Owner = "" }, func(c *Config) { c.LeaseDuration = 0 }, func(c *Config) { c.BatchSize = 0 }, func(c *Config) { c.PollInterval = time.Millisecond }} {
		candidate := config
		mutate(&candidate)
		if _, err := New(candidate); !errors.Is(err, ErrInvalid) {
			t.Fatalf("New=%v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := config.Store.Create(ctx, valid); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
}

func TestEngineRunStopsOnCancellation(t *testing.T) {
	t.Parallel()
	engine, _ := New(Config{Store: NewMemoryStore(), Executor: executorFunc(func(context.Context, Task) (DispatchResult, error) { return DispatchResult{}, nil }), Owner: "worker", LeaseDuration: time.Second, BatchSize: 1, PollInterval: 100 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := engine.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run=%v", err)
	}
	if err := (*Engine)(nil).Run(context.Background()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil Run=%v", err)
	}
	if err := (*Engine)(nil).RunOnce(context.Background()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil RunOnce=%v", err)
	}
	if err := (*Engine)(nil).RunTask(context.Background(), Task{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil RunTask=%v", err)
	}
}

func TestEngineRunReportsFailureAndKeepsPolling(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	store := NewMemoryStore()
	if err := store.Create(context.Background(), taskAt("failed", Once, now.Format(time.RFC3339), now)); err != nil {
		t.Fatal(err)
	}
	reported := make(chan error, 1)
	engine, err := New(Config{
		Store: store, Executor: executorFunc(func(context.Context, Task) (DispatchResult, error) { return DispatchResult{}, errors.New("dispatch") }),
		Owner: "worker", LeaseDuration: time.Second, BatchSize: 1, PollInterval: 100 * time.Millisecond,
		Now: func() time.Time { return now }, OnError: func(err error) { reported <- err },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- engine.Run(ctx) }()
	select {
	case err = <-reported:
		if err == nil || err.Error() != "dispatch" {
			t.Fatalf("reported = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("dispatch failure was not reported")
	}
	cancel()
	if err = <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() = %v", err)
	}
}

func taskAt(id string, kind ScheduleType, spec string, now time.Time) Task {
	return Task{ID: id, UserID: "u", Title: id, Prompt: "prompt", ScheduleType: kind, Schedule: spec, Timezone: "UTC", Status: Enabled, NextRunAt: now, CreatedAt: now, UpdatedAt: now}
}
