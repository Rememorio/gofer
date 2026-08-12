package store

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/event"
)

// Memory is a concurrency-safe reference Store for tests and ephemeral use.
type Memory struct {
	mu            sync.RWMutex
	threads       map[domain.ThreadID]domain.Thread
	runs          map[domain.RunID]domain.Run
	events        map[domain.RunID][]event.Event
	watchers      map[domain.RunID]map[uint64]chan uint64
	nextWatcherID uint64
}

// NewMemory constructs an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{
		threads:  make(map[domain.ThreadID]domain.Thread),
		runs:     make(map[domain.RunID]domain.Run),
		events:   make(map[domain.RunID][]event.Event),
		watchers: make(map[domain.RunID]map[uint64]chan uint64),
	}
}

// CreateThread persists thread if its identifier is unused.
func (memory *Memory) CreateThread(ctx context.Context, thread domain.Thread) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := thread.Validate(); err != nil {
		return err
	}
	memory.mu.Lock()
	defer memory.mu.Unlock()
	if _, exists := memory.threads[thread.ID]; exists {
		return fmt.Errorf("create thread %s: %w", thread.ID, ErrExists)
	}
	memory.threads[thread.ID] = cloneThread(thread)
	return nil
}

// Thread returns an isolated copy of the requested thread.
func (memory *Memory) Thread(ctx context.Context, id domain.ThreadID) (domain.Thread, error) {
	if err := contextError(ctx); err != nil {
		return domain.Thread{}, err
	}
	memory.mu.RLock()
	defer memory.mu.RUnlock()
	thread, exists := memory.threads[id]
	if !exists {
		return domain.Thread{}, fmt.Errorf("thread %s: %w", id, ErrNotFound)
	}
	return cloneThread(thread), nil
}

// CreateRun persists run after verifying its parent thread.
func (memory *Memory) CreateRun(ctx context.Context, run domain.Run) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := run.Validate(); err != nil {
		return err
	}
	memory.mu.Lock()
	defer memory.mu.Unlock()
	if _, exists := memory.threads[run.ThreadID]; !exists {
		return fmt.Errorf("create run %s: thread %s: %w", run.ID, run.ThreadID, ErrNotFound)
	}
	if _, exists := memory.runs[run.ID]; exists {
		return fmt.Errorf("create run %s: %w", run.ID, ErrExists)
	}
	memory.runs[run.ID] = run
	return nil
}

// Run returns the requested durable run state.
func (memory *Memory) Run(ctx context.Context, id domain.RunID) (domain.Run, error) {
	if err := contextError(ctx); err != nil {
		return domain.Run{}, err
	}
	memory.mu.RLock()
	defer memory.mu.RUnlock()
	run, exists := memory.runs[id]
	if !exists {
		return domain.Run{}, fmt.Errorf("run %s: %w", id, ErrNotFound)
	}
	return run, nil
}

// TransitionRun atomically advances a run when its current status equals expected.
func (memory *Memory) TransitionRun(
	ctx context.Context,
	id domain.RunID,
	expected domain.RunStatus,
	next domain.RunStatus,
	at time.Time,
	failure string,
) (domain.Run, error) {
	if err := contextError(ctx); err != nil {
		return domain.Run{}, err
	}
	memory.mu.Lock()
	defer memory.mu.Unlock()
	run, exists := memory.runs[id]
	if !exists {
		return domain.Run{}, fmt.Errorf("transition run %s: %w", id, ErrNotFound)
	}
	if run.Status != expected {
		return domain.Run{}, fmt.Errorf("transition run %s: have %s, expected %s: %w", id, run.Status, expected, ErrConflict)
	}
	advanced, err := run.Transition(next, at, failure)
	if err != nil {
		return domain.Run{}, err
	}
	memory.runs[id] = advanced
	return advanced, nil
}

// Append atomically commits drafts after expectedSequence.
func (memory *Memory) Append(
	ctx context.Context,
	runID domain.RunID,
	expectedSequence uint64,
	drafts ...event.Draft,
) ([]event.Event, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	memory.mu.Lock()
	defer memory.mu.Unlock()
	run, exists := memory.runs[runID]
	if !exists {
		return nil, fmt.Errorf("append run %s: %w", runID, ErrNotFound)
	}
	current := uint64(len(memory.events[runID]))
	if current != expectedSequence {
		return nil, fmt.Errorf("append run %s: have sequence %d, expected %d: %w", runID, current, expectedSequence, ErrConflict)
	}
	committed := make([]event.Event, len(drafts))
	for index, draft := range drafts {
		if draft.RunID != runID || draft.ThreadID != run.ThreadID {
			return nil, fmt.Errorf("append run %s: event scope does not match run", runID)
		}
		record, err := draft.Commit(current + uint64(index) + 1)
		if err != nil {
			return nil, err
		}
		committed[index] = record
	}
	if len(committed) == 0 {
		return committed, nil
	}
	memory.events[runID] = append(memory.events[runID], cloneEvents(committed)...)
	latest := committed[len(committed)-1].Sequence
	for _, watcher := range memory.watchers[runID] {
		select {
		case watcher <- latest:
		default:
		}
	}
	return cloneEvents(committed), nil
}

// Events reads committed events strictly after sequence, in journal order.
func (memory *Memory) Events(ctx context.Context, runID domain.RunID, sequence uint64, limit int) ([]event.Event, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	memory.mu.RLock()
	defer memory.mu.RUnlock()
	if _, exists := memory.runs[runID]; !exists {
		return nil, fmt.Errorf("events for run %s: %w", runID, ErrNotFound)
	}
	records := memory.events[runID]
	start := min(sequence, uint64(len(records)))
	end := uint64(len(records))
	if limit > 0 {
		end = min(end, start+uint64(limit))
	}
	return cloneEvents(records[start:end]), nil
}

// Watch returns edge-triggered latest-sequence notifications.
//
// Notifications may be coalesced. Consumers recover every event by calling
// Events after their last processed sequence, so a slow watcher cannot block
// durable writes or lose journal data.
func (memory *Memory) Watch(ctx context.Context, runID domain.RunID, after uint64) (<-chan uint64, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	memory.mu.Lock()
	if _, exists := memory.runs[runID]; !exists {
		memory.mu.Unlock()
		return nil, fmt.Errorf("watch run %s: %w", runID, ErrNotFound)
	}
	memory.nextWatcherID++
	id := memory.nextWatcherID
	watcher := make(chan uint64, 1)
	if memory.watchers[runID] == nil {
		memory.watchers[runID] = make(map[uint64]chan uint64)
	}
	memory.watchers[runID][id] = watcher
	current := uint64(len(memory.events[runID]))
	if current > after {
		watcher <- current
	}
	memory.mu.Unlock()

	go func() {
		<-ctx.Done()
		memory.mu.Lock()
		delete(memory.watchers[runID], id)
		close(watcher)
		memory.mu.Unlock()
	}()
	return watcher, nil
}

func contextError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("store operation: %w", err)
	}
	return nil
}

func cloneThread(thread domain.Thread) domain.Thread {
	cloned := thread
	if thread.Metadata != nil {
		cloned.Metadata = make(map[string]string, len(thread.Metadata))
		for key, value := range thread.Metadata {
			cloned.Metadata[key] = value
		}
	}
	return cloned
}

func cloneEvents(records []event.Event) []event.Event {
	cloned := make([]event.Event, len(records))
	for index, record := range records {
		cloned[index] = record
		cloned[index].Data = append([]byte(nil), record.Data...)
	}
	return cloned
}
