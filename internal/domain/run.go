package domain

import (
	"errors"
	"fmt"
	"time"
)

var (
	// ErrInvalidRun identifies malformed durable run state.
	ErrInvalidRun = errors.New("invalid run")
	// ErrInvalidTransition identifies an unsupported run status transition.
	ErrInvalidTransition = errors.New("invalid run transition")
)

// RunStatus identifies the durable lifecycle state of a run.
type RunStatus string

// Supported run lifecycle states.
const (
	RunPending     RunStatus = "pending"
	RunRunning     RunStatus = "running"
	RunInterrupted RunStatus = "interrupted"
	RunSucceeded   RunStatus = "succeeded"
	RunFailed      RunStatus = "failed"
	RunCancelled   RunStatus = "cancelled"
)

// Run is one durable agent execution within a thread.
type Run struct {
	ID         RunID     `json:"run_id"`
	ThreadID   ThreadID  `json:"thread_id"`
	Status     RunStatus `json:"status"`
	Attempt    uint32    `json:"attempt"`
	Error      string    `json:"error,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}

// NewRun constructs a pending run for threadID.
func NewRun(threadID ThreadID, at time.Time) (Run, error) {
	if _, err := ParseThreadID(string(threadID)); err != nil {
		return Run{}, fmt.Errorf("%w: %w", ErrInvalidRun, err)
	}
	id, err := NewRunID()
	if err != nil {
		return Run{}, err
	}
	return Run{ID: id, ThreadID: threadID, Status: RunPending, CreatedAt: at}, nil
}

// Terminal reports whether no further transitions are allowed.
func (run Run) Terminal() bool {
	switch run.Status {
	case RunSucceeded, RunFailed, RunCancelled:
		return true
	case RunPending, RunRunning, RunInterrupted:
		return false
	default:
		return false
	}
}

// Transition returns a copy advanced to next without mutating run.
func (run Run) Transition(next RunStatus, at time.Time, failure string) (Run, error) {
	if !allowedTransition(run.Status, next) {
		return Run{}, fmt.Errorf("%w: %s to %s", ErrInvalidTransition, run.Status, next)
	}
	if at.Before(run.CreatedAt) {
		return Run{}, fmt.Errorf("%w: transition precedes creation", ErrInvalidTransition)
	}

	advanced := run
	advanced.Status = next
	advanced.Error = ""
	if next == RunRunning {
		advanced.Attempt++
		if advanced.StartedAt.IsZero() {
			advanced.StartedAt = at
		}
	}
	if next == RunFailed {
		if failure == "" {
			return Run{}, fmt.Errorf("%w: failed run requires an error", ErrInvalidTransition)
		}
		advanced.Error = failure
	}
	if advanced.Terminal() {
		advanced.FinishedAt = at
	}
	return advanced, nil
}

// Validate verifies the durable run contract.
func (run Run) Validate() error {
	if err := run.validateIdentity(); err != nil {
		return err
	}
	if err := run.validateLifecycle(); err != nil {
		return err
	}
	return run.validateTimeline()
}

func (run Run) validateIdentity() error {
	if _, err := ParseRunID(string(run.ID)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRun, err)
	}
	if _, err := ParseThreadID(string(run.ThreadID)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRun, err)
	}
	return nil
}

func (run Run) validateLifecycle() error {
	if !run.Status.valid() {
		return fmt.Errorf("%w: unsupported status %q", ErrInvalidRun, run.Status)
	}
	if run.Status == RunPending && run.Attempt != 0 {
		return fmt.Errorf("%w: pending run has attempts", ErrInvalidRun)
	}
	if run.Status != RunPending && run.Attempt == 0 {
		return fmt.Errorf("%w: active run has no attempt", ErrInvalidRun)
	}
	if run.Status == RunFailed && run.Error == "" {
		return fmt.Errorf("%w: failed run has no error", ErrInvalidRun)
	}
	if run.Status != RunFailed && run.Error != "" {
		return fmt.Errorf("%w: non-failed run has an error", ErrInvalidRun)
	}
	return nil
}

func (run Run) validateTimeline() error {
	if run.CreatedAt.IsZero() {
		return fmt.Errorf("%w: created_at is required", ErrInvalidRun)
	}
	if run.Status != RunPending && run.StartedAt.IsZero() {
		return fmt.Errorf("%w: active run has no started_at", ErrInvalidRun)
	}
	if !run.StartedAt.IsZero() && run.StartedAt.Before(run.CreatedAt) {
		return fmt.Errorf("%w: started_at precedes created_at", ErrInvalidRun)
	}
	if run.Terminal() && run.FinishedAt.IsZero() {
		return fmt.Errorf("%w: terminal run has no finished_at", ErrInvalidRun)
	}
	if !run.FinishedAt.IsZero() && (run.StartedAt.IsZero() || run.FinishedAt.Before(run.StartedAt)) {
		return fmt.Errorf("%w: finished_at precedes started_at", ErrInvalidRun)
	}
	return nil
}

func allowedTransition(current, next RunStatus) bool {
	switch current {
	case RunPending:
		return next == RunRunning || next == RunCancelled
	case RunRunning:
		return next == RunInterrupted || next == RunSucceeded || next == RunFailed || next == RunCancelled
	case RunInterrupted:
		return next == RunRunning || next == RunCancelled
	case RunSucceeded, RunFailed, RunCancelled:
		return false
	default:
		return false
	}
}

func (status RunStatus) valid() bool {
	switch status {
	case RunPending, RunRunning, RunInterrupted, RunSucceeded, RunFailed, RunCancelled:
		return true
	default:
		return false
	}
}
