package subagent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	// ErrInvalid identifies malformed manager configuration or child input.
	ErrInvalid = errors.New("invalid subagent request")
	// ErrCapacity identifies exhaustion of the configured child count.
	ErrCapacity = errors.New("subagent capacity reached")
	// ErrNotFound identifies an unknown child task.
	ErrNotFound = errors.New("subagent not found")
	// ErrClosed identifies a stopped child manager.
	ErrClosed = errors.New("subagent manager closed")
)

// Status identifies a child task lifecycle state.
type Status string

const (
	// Queued identifies a child waiting for an execution slot.
	Queued Status = "queued"
	// Running identifies a child currently executing.
	Running Status = "running"
	// Succeeded identifies a child with a successful output.
	Succeeded Status = "succeeded"
	// Failed identifies a child whose executor returned an error.
	Failed Status = "failed"
	// Cancelled identifies a child stopped through context cancellation.
	Cancelled Status = "cancelled"
)

// Request is the isolated input supplied to a child agent.
type Request struct {
	ParentID string            `json:"parent_id,omitempty"`
	Depth    int               `json:"depth"`
	Prompt   string            `json:"prompt"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// Output is the normalized child-agent result.
type Output struct {
	Text     string            `json:"text"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// Task is a stable child execution snapshot.
type Task struct {
	ID         string    `json:"id"`
	Request    Request   `json:"request"`
	Status     Status    `json:"status"`
	Output     Output    `json:"output,omitempty"`
	Error      string    `json:"error,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
}

// Event is an ordered fan-in record from all child tasks.
type Event struct {
	Sequence uint64    `json:"sequence"`
	TaskID   string    `json:"task_id"`
	Status   Status    `json:"status"`
	Time     time.Time `json:"time"`
}

// Executor runs one isolated child request.
type Executor interface {
	Execute(context.Context, Request) (Output, error)
}

// Config controls child count, depth, and parallelism.
type Config struct {
	Executor    Executor
	MaxParallel int
	MaxChildren int
	MaxDepth    int
	Now         func() time.Time
	NewID       func() (string, error)
}

// Manager owns child lifecycle, cancellation, and ordered event fan-in.
type Manager struct {
	mu                    sync.RWMutex
	ctx                   context.Context
	cancel                context.CancelFunc
	executor              Executor
	semaphore             chan struct{}
	maxChildren, maxDepth int
	now                   func() time.Time
	newID                 func() (string, error)
	tasks                 map[string]*managed
	events                []Event
	closed                bool
	wait                  sync.WaitGroup
}

type managed struct {
	task   Task
	cancel context.CancelFunc
	done   chan struct{}
}

// NewManager validates config and constructs a child manager.
func NewManager(parent context.Context, config Config) (*Manager, error) {
	if parent == nil || config.Executor == nil || config.MaxParallel < 1 || config.MaxChildren < 1 || config.MaxDepth < 1 || config.MaxParallel > config.MaxChildren {
		return nil, fmt.Errorf("%w: context, executor, and ordered positive limits are required", ErrInvalid)
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.NewID == nil {
		config.NewID = randomID
	}
	ctx, cancel := context.WithCancel(parent)
	return &Manager{ctx: ctx, cancel: cancel, executor: config.Executor, semaphore: make(chan struct{}, config.MaxParallel), maxChildren: config.MaxChildren, maxDepth: config.MaxDepth, now: config.Now, newID: config.NewID, tasks: make(map[string]*managed)}, nil
}

// Spawn validates and asynchronously queues one child task.
func (manager *Manager) Spawn(ctx context.Context, request Request) (Task, error) {
	if manager == nil {
		return Task{}, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return Task{}, err
	}
	request.Prompt = strings.TrimSpace(request.Prompt)
	if request.Prompt == "" || len(request.Prompt) > 64<<10 || request.Depth < 1 || request.Depth > manager.maxDepth {
		return Task{}, ErrInvalid
	}
	id, err := manager.newID()
	if err != nil {
		return Task{}, fmt.Errorf("create subagent id: %w", err)
	}
	if strings.TrimSpace(id) == "" {
		return Task{}, ErrInvalid
	}
	manager.mu.Lock()
	if manager.closed {
		manager.mu.Unlock()
		return Task{}, ErrClosed
	}
	if len(manager.tasks) >= manager.maxChildren {
		manager.mu.Unlock()
		return Task{}, ErrCapacity
	}
	if _, exists := manager.tasks[id]; exists {
		manager.mu.Unlock()
		return Task{}, fmt.Errorf("%w: duplicate id", ErrInvalid)
	}
	taskContext, cancel := context.WithCancel(manager.ctx)
	current := &managed{task: Task{ID: id, Request: cloneRequest(request), Status: Queued, CreatedAt: manager.now()}, cancel: cancel, done: make(chan struct{})}
	manager.tasks[id] = current
	manager.appendEventLocked(id, Queued)
	manager.wait.Add(1)
	snapshot := cloneTask(current.task)
	manager.mu.Unlock()
	go manager.run(taskContext, current)
	return snapshot, nil
}

func (manager *Manager) run(ctx context.Context, current *managed) {
	defer manager.wait.Done()
	select {
	case manager.semaphore <- struct{}{}:
		defer func() { <-manager.semaphore }()
	case <-ctx.Done():
		manager.finish(current, Output{}, ctx.Err())
		return
	}
	manager.mu.Lock()
	current.task.Status = Running
	current.task.StartedAt = manager.now()
	manager.appendEventLocked(current.task.ID, Running)
	manager.mu.Unlock()
	output, err := manager.executor.Execute(ctx, cloneRequest(current.task.Request))
	manager.finish(current, output, err)
}

func (manager *Manager) finish(current *managed, output Output, err error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		current.task.Status = Cancelled
	} else if err != nil {
		current.task.Status = Failed
		current.task.Error = err.Error()
	} else {
		current.task.Status = Succeeded
		current.task.Output = cloneOutput(output)
	}
	current.task.FinishedAt = manager.now()
	manager.appendEventLocked(current.task.ID, current.task.Status)
	close(current.done)
}

// Get returns an isolated task snapshot.
func (manager *Manager) Get(id string) (Task, error) {
	if manager == nil {
		return Task{}, ErrNotFound
	}
	manager.mu.RLock()
	current, exists := manager.tasks[id]
	if !exists {
		manager.mu.RUnlock()
		return Task{}, ErrNotFound
	}
	task := cloneTask(current.task)
	manager.mu.RUnlock()
	return task, nil
}

// Wait blocks until a task reaches a terminal state or ctx ends.
func (manager *Manager) Wait(ctx context.Context, id string) (Task, error) {
	manager.mu.RLock()
	current, exists := manager.tasks[id]
	if !exists {
		manager.mu.RUnlock()
		return Task{}, ErrNotFound
	}
	done := current.done
	manager.mu.RUnlock()
	select {
	case <-ctx.Done():
		return Task{}, ctx.Err()
	case <-done:
		return manager.Get(id)
	}
}

// Cancel requests cancellation without blocking for executor shutdown.
func (manager *Manager) Cancel(id string) error {
	manager.mu.RLock()
	current, exists := manager.tasks[id]
	if exists {
		current.cancel()
	}
	manager.mu.RUnlock()
	if !exists {
		return ErrNotFound
	}
	return nil
}

// List returns stable creation-time then identifier order.
func (manager *Manager) List() []Task {
	if manager == nil {
		return nil
	}
	manager.mu.RLock()
	tasks := make([]Task, 0, len(manager.tasks))
	for _, current := range manager.tasks {
		tasks = append(tasks, cloneTask(current.task))
	}
	manager.mu.RUnlock()
	sort.Slice(tasks, func(i, j int) bool {
		if !tasks[i].CreatedAt.Equal(tasks[j].CreatedAt) {
			return tasks[i].CreatedAt.Before(tasks[j].CreatedAt)
		}
		return tasks[i].ID < tasks[j].ID
	})
	return tasks
}

// Events returns isolated ordered fan-in records strictly after sequence.
func (manager *Manager) Events(sequence uint64, limit int) []Event {
	if manager == nil {
		return nil
	}
	manager.mu.RLock()
	start := min(sequence, uint64(len(manager.events)))
	end := uint64(len(manager.events))
	if limit > 0 {
		end = min(end, start+uint64(limit))
	}
	events := append([]Event(nil), manager.events[start:end]...)
	manager.mu.RUnlock()
	return events
}

// Close cancels all children and waits for their shutdown.
func (manager *Manager) Close() error {
	if manager == nil {
		return nil
	}
	manager.mu.Lock()
	if manager.closed {
		manager.mu.Unlock()
		return nil
	}
	manager.closed = true
	manager.cancel()
	manager.mu.Unlock()
	manager.wait.Wait()
	return nil
}

func (manager *Manager) appendEventLocked(id string, status Status) {
	manager.events = append(manager.events, Event{Sequence: uint64(len(manager.events) + 1), TaskID: id, Status: status, Time: manager.now()})
}

func randomID() (string, error) {
	data := make([]byte, 12)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return "sub_" + hex.EncodeToString(data), nil
}
func cloneRequest(request Request) Request {
	request.Metadata = cloneMap(request.Metadata)
	return request
}
func cloneOutput(output Output) Output { output.Metadata = cloneMap(output.Metadata); return output }
func cloneTask(task Task) Task {
	task.Request = cloneRequest(task.Request)
	task.Output = cloneOutput(task.Output)
	return task
}
func cloneMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	cloned := make(map[string]string, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}
