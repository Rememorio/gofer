package domain

import (
	"errors"
	"testing"
	"time"
)

func TestThreadValidation(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	thread, err := NewThread(now)
	if err != nil {
		t.Fatalf("NewThread(): %v", err)
	}
	if err := thread.Validate(); err != nil {
		t.Fatalf("Validate(): %v", err)
	}

	thread.UpdatedAt = now.Add(-time.Second)
	if !errors.Is(thread.Validate(), ErrInvalidThread) {
		t.Fatalf("Validate() error = %v, want ErrInvalidThread", thread.Validate())
	}
	thread.ID = "bad"
	if !errors.Is(thread.Validate(), ErrInvalidThread) {
		t.Fatalf("Validate() error = %v, want ErrInvalidThread", thread.Validate())
	}
}

func TestRunLifecycle(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	thread, err := NewThread(now)
	if err != nil {
		t.Fatalf("NewThread(): %v", err)
	}
	run, err := NewRun(thread.ID, now)
	if err != nil {
		t.Fatalf("NewRun(): %v", err)
	}
	if err := run.Validate(); err != nil {
		t.Fatalf("pending Validate(): %v", err)
	}

	run, err = run.Transition(RunRunning, now.Add(time.Second), "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if run.Attempt != 1 || run.StartedAt.IsZero() {
		t.Fatalf("started run = %#v", run)
	}
	run, err = run.Transition(RunInterrupted, now.Add(2*time.Second), "")
	if err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	run, err = run.Transition(RunRunning, now.Add(3*time.Second), "")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if run.Attempt != 2 {
		t.Fatalf("resumed attempt = %d, want 2", run.Attempt)
	}
	run, err = run.Transition(RunSucceeded, now.Add(4*time.Second), "")
	if err != nil {
		t.Fatalf("succeed: %v", err)
	}
	if !run.Terminal() || run.FinishedAt.IsZero() {
		t.Fatalf("succeeded run = %#v", run)
	}
	if err := run.Validate(); err != nil {
		t.Fatalf("succeeded Validate(): %v", err)
	}
}

func TestRunTransitionRejectsInvalidChanges(t *testing.T) {
	t.Parallel()

	now := time.Now()
	thread, err := NewThread(now)
	if err != nil {
		t.Fatalf("NewThread(): %v", err)
	}
	run, err := NewRun(thread.ID, now)
	if err != nil {
		t.Fatalf("NewRun(): %v", err)
	}

	tests := []struct {
		name    string
		next    RunStatus
		at      time.Time
		failure string
	}{
		{name: "skip running", next: RunSucceeded, at: now},
		{name: "before creation", next: RunRunning, at: now.Add(-time.Second)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := run.Transition(test.next, test.at, test.failure)
			if !errors.Is(err, ErrInvalidTransition) {
				t.Fatalf("Transition() error = %v, want ErrInvalidTransition", err)
			}
		})
	}

	running, err := run.Transition(RunRunning, now, "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := running.Transition(RunFailed, now, ""); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("Transition(failed without error) = %v, want ErrInvalidTransition", err)
	}
}

func TestRunFailure(t *testing.T) {
	t.Parallel()

	now := time.Now()
	thread, err := NewThread(now)
	if err != nil {
		t.Fatalf("NewThread(): %v", err)
	}
	run, err := NewRun(thread.ID, now)
	if err != nil {
		t.Fatalf("NewRun(): %v", err)
	}
	run, err = run.Transition(RunRunning, now, "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	run, err = run.Transition(RunFailed, now.Add(time.Second), "provider unavailable")
	if err != nil {
		t.Fatalf("fail: %v", err)
	}
	if run.Error != "provider unavailable" || !run.Terminal() {
		t.Fatalf("failed run = %#v", run)
	}
}

func TestRunValidationRejectsMalformedState(t *testing.T) {
	t.Parallel()

	now := time.Now()
	thread, err := NewThread(now)
	if err != nil {
		t.Fatalf("NewThread(): %v", err)
	}
	run, err := NewRun(thread.ID, now)
	if err != nil {
		t.Fatalf("NewRun(): %v", err)
	}
	tests := []func(*Run){
		func(run *Run) { run.ID = "bad" },
		func(run *Run) { run.ThreadID = "bad" },
		func(run *Run) { run.CreatedAt = time.Time{} },
		func(run *Run) { run.Status = "unknown" },
		func(run *Run) { run.Attempt = 1 },
		func(run *Run) { run.Status = RunRunning },
		func(run *Run) { run.Status, run.Attempt, run.StartedAt = RunRunning, 1, time.Time{} },
		func(run *Run) { run.Status, run.Attempt, run.StartedAt = RunRunning, 1, now.Add(-time.Second) },
		func(run *Run) { run.Status, run.Attempt = RunFailed, 1 },
		func(run *Run) { run.Error = "unexpected" },
		func(run *Run) { run.Status, run.Attempt = RunSucceeded, 1 },
		func(run *Run) {
			run.Status, run.Attempt = RunSucceeded, 1
			run.StartedAt, run.FinishedAt = now, now.Add(-time.Second)
		},
	}
	for index, mutate := range tests {
		candidate := run
		mutate(&candidate)
		if err := candidate.Validate(); !errors.Is(err, ErrInvalidRun) {
			t.Fatalf("case %d Validate() error = %v, want ErrInvalidRun", index, err)
		}
	}
}

func TestNewRunRejectsInvalidThread(t *testing.T) {
	t.Parallel()

	_, err := NewRun("bad", time.Now())
	if !errors.Is(err, ErrInvalidRun) {
		t.Fatalf("NewRun() error = %v, want ErrInvalidRun", err)
	}
}
