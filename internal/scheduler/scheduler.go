package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

var (
	// ErrInvalid identifies malformed scheduled task state or configuration.
	ErrInvalid = errors.New("invalid scheduled task")
	// ErrNotFound identifies an unknown task.
	ErrNotFound = errors.New("scheduled task not found")
	// ErrConflict identifies an active lease or optimistic state mismatch.
	ErrConflict = errors.New("scheduled task conflict")
)

// Status identifies scheduled task lifecycle state.
type Status string

const (
	// Enabled identifies a task eligible for dispatch.
	Enabled Status = "enabled"
	// Running identifies a leased task in flight.
	Running Status = "running"
	// Paused identifies a task disabled by its owner.
	Paused Status = "paused"
	// Completed identifies a finished one-time task.
	Completed Status = "completed"
	// Failed identifies a one-time task whose dispatch failed.
	Failed Status = "failed"
)

// ScheduleType identifies a one-time or recurring schedule.
type ScheduleType string

const (
	// Once runs at one RFC3339 timestamp.
	Once ScheduleType = "once"
	// Cron runs on a standard five-field cron expression.
	Cron ScheduleType = "cron"
)

// Task is one durable scheduled agent launch.
type Task struct {
	ID             string       `json:"id"`
	UserID         string       `json:"user_id"`
	ThreadID       string       `json:"thread_id,omitempty"`
	Title          string       `json:"title"`
	Prompt         string       `json:"prompt"`
	ScheduleType   ScheduleType `json:"schedule_type"`
	Schedule       string       `json:"schedule"`
	Timezone       string       `json:"timezone"`
	Status         Status       `json:"status"`
	NextRunAt      time.Time    `json:"next_run_at,omitempty"`
	LastRunAt      time.Time    `json:"last_run_at,omitempty"`
	LastRunID      string       `json:"last_run_id,omitempty"`
	LastError      string       `json:"last_error,omitempty"`
	LeaseOwner     string       `json:"lease_owner,omitempty"`
	LeaseExpiresAt time.Time    `json:"lease_expires_at,omitempty"`
	RunCount       uint64       `json:"run_count"`
	CreatedAt      time.Time    `json:"created_at"`
	UpdatedAt      time.Time    `json:"updated_at"`
}

// DispatchResult identifies the run created for a scheduled task.
type DispatchResult struct {
	RunID    string `json:"run_id"`
	ThreadID string `json:"thread_id"`
}

// Executor launches one scheduled task.
type Executor interface {
	Execute(context.Context, Task) (DispatchResult, error)
}

// Store owns durable task state and atomic leases.
type Store interface {
	Create(context.Context, Task) error
	Get(context.Context, string) (Task, error)
	List(context.Context, string) ([]Task, error)
	Update(context.Context, string, string, Update, time.Time) (Task, error)
	Delete(context.Context, string, string) error
	Trigger(context.Context, string, string, time.Time) (Task, error)
	Due(context.Context, time.Time, int) ([]Task, error)
	Claim(context.Context, string, time.Time, string, time.Time, time.Time) (Task, error)
	Finish(context.Context, string, string, time.Time, time.Time, DispatchResult, error) (Task, error)
	SetStatus(context.Context, string, string, Status, time.Time) (Task, error)
}

// Update is a partial mutable scheduled-task definition.
type Update struct {
	Title        *string       `json:"title,omitempty"`
	Prompt       *string       `json:"prompt,omitempty"`
	ScheduleType *ScheduleType `json:"schedule_type,omitempty"`
	Schedule     *string       `json:"schedule,omitempty"`
	Timezone     *string       `json:"timezone,omitempty"`
}

func (update Update) empty() bool {
	return update.Title == nil && update.Prompt == nil && update.ScheduleType == nil && update.Schedule == nil && update.Timezone == nil
}

func (update Update) changesSchedule() bool {
	return update.ScheduleType != nil || update.Schedule != nil || update.Timezone != nil
}

// MemoryStore is the concurrency-safe reference Store.
type MemoryStore struct {
	mu    sync.RWMutex
	tasks map[string]Task
}

// NewMemoryStore constructs an empty task store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{tasks: make(map[string]Task)} }

// Create stores a unique validated task.
func (store *MemoryStore) Create(ctx context.Context, task Task) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := task.Validate(); err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, exists := store.tasks[task.ID]; exists {
		return ErrConflict
	}
	store.tasks[task.ID] = task
	return nil
}

// Get returns one task.
func (store *MemoryStore) Get(ctx context.Context, id string) (Task, error) {
	if err := ctx.Err(); err != nil {
		return Task{}, err
	}
	store.mu.RLock()
	task, exists := store.tasks[id]
	store.mu.RUnlock()
	if !exists {
		return Task{}, ErrNotFound
	}
	return task, nil
}

// List returns stable task order scoped to one user.
func (store *MemoryStore) List(ctx context.Context, userID string) ([]Task, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	store.mu.RLock()
	tasks := make([]Task, 0)
	for _, task := range store.tasks {
		if task.UserID == userID {
			tasks = append(tasks, task)
		}
	}
	store.mu.RUnlock()
	sort.Slice(tasks, func(i, j int) bool {
		return tasks[i].CreatedAt.Before(tasks[j].CreatedAt) || tasks[i].CreatedAt.Equal(tasks[j].CreatedAt) && tasks[i].ID < tasks[j].ID
	})
	return tasks, nil
}

// Update changes mutable task fields and recomputes the next occurrence.
func (store *MemoryStore) Update(ctx context.Context, id, userID string, update Update, at time.Time) (Task, error) {
	if err := ctx.Err(); err != nil {
		return Task{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	task, exists := store.tasks[id]
	if !exists || task.UserID != userID {
		return Task{}, ErrNotFound
	}
	if task.Status == Running {
		return Task{}, ErrConflict
	}
	if update.empty() {
		return Task{}, ErrInvalid
	}
	applyUpdate(&task, update)
	if update.changesSchedule() {
		next, err := NextRun(task.ScheduleType, task.Schedule, task.Timezone, at)
		if err != nil {
			return Task{}, err
		}
		task.NextRunAt = next
		if task.Status != Paused {
			task.Status = Enabled
		}
	}
	task.UpdatedAt = at
	if err := task.Validate(); err != nil {
		return Task{}, err
	}
	store.tasks[id] = task
	return task, nil
}

// Delete removes one owned task unless it is currently leased.
func (store *MemoryStore) Delete(ctx context.Context, id, userID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	task, exists := store.tasks[id]
	if !exists || task.UserID != userID {
		return ErrNotFound
	}
	if task.Status == Running {
		return ErrConflict
	}
	delete(store.tasks, id)
	return nil
}

// Trigger makes one owned non-running task immediately due.
func (store *MemoryStore) Trigger(ctx context.Context, id, userID string, at time.Time) (Task, error) {
	if err := ctx.Err(); err != nil {
		return Task{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	task, exists := store.tasks[id]
	if !exists || task.UserID != userID {
		return Task{}, ErrNotFound
	}
	if task.Status == Running {
		return Task{}, ErrConflict
	}
	task.Status, task.NextRunAt, task.UpdatedAt = Enabled, at, at
	store.tasks[id] = task
	return task, nil
}

func applyUpdate(task *Task, update Update) {
	if update.Title != nil {
		task.Title = strings.TrimSpace(*update.Title)
	}
	if update.Prompt != nil {
		task.Prompt = strings.TrimSpace(*update.Prompt)
	}
	if update.ScheduleType != nil {
		task.ScheduleType = *update.ScheduleType
	}
	if update.Schedule != nil {
		task.Schedule = strings.TrimSpace(*update.Schedule)
	}
	if update.Timezone != nil {
		task.Timezone = strings.TrimSpace(*update.Timezone)
	}
}

// Due returns enabled tasks whose schedule arrived and lease is free.
func (store *MemoryStore) Due(ctx context.Context, now time.Time, limit int) ([]Task, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit < 1 {
		return nil, ErrInvalid
	}
	store.mu.RLock()
	tasks := make([]Task, 0)
	for _, task := range store.tasks {
		eligible := task.Status == Enabled || task.Status == Running && !task.LeaseExpiresAt.After(now)
		if eligible && !task.NextRunAt.After(now) && (task.LeaseExpiresAt.IsZero() || !task.LeaseExpiresAt.After(now)) {
			tasks = append(tasks, task)
		}
	}
	store.mu.RUnlock()
	sort.Slice(tasks, func(i, j int) bool {
		return tasks[i].NextRunAt.Before(tasks[j].NextRunAt) || tasks[i].NextRunAt.Equal(tasks[j].NextRunAt) && tasks[i].ID < tasks[j].ID
	})
	if len(tasks) > limit {
		tasks = tasks[:limit]
	}
	return tasks, nil
}

// Claim atomically leases a due task at expectedNext.
func (store *MemoryStore) Claim(ctx context.Context, id string, expectedNext time.Time, owner string, now, leaseUntil time.Time) (Task, error) {
	if err := ctx.Err(); err != nil {
		return Task{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	task, exists := store.tasks[id]
	if !exists {
		return Task{}, ErrNotFound
	}
	eligible := task.Status == Enabled || task.Status == Running && !task.LeaseExpiresAt.After(now)
	if !eligible || !task.NextRunAt.Equal(expectedNext) || (task.LeaseExpiresAt.After(now) && task.LeaseOwner != "") {
		return Task{}, ErrConflict
	}
	task.Status = Running
	task.LeaseOwner = owner
	task.LeaseExpiresAt = leaseUntil
	task.UpdatedAt = now
	store.tasks[id] = task
	return task, nil
}

// Finish releases a matching lease and records dispatch outcome.
func (store *MemoryStore) Finish(ctx context.Context, id, owner string, at, next time.Time, result DispatchResult, dispatchErr error) (Task, error) {
	if err := ctx.Err(); err != nil {
		return Task{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	task, exists := store.tasks[id]
	if !exists {
		return Task{}, ErrNotFound
	}
	if task.Status != Running || task.LeaseOwner != owner {
		return Task{}, ErrConflict
	}
	task.LastRunAt = at
	task.RunCount++
	task.LastRunID = result.RunID
	if task.ThreadID == "" && result.ThreadID != "" {
		task.ThreadID = result.ThreadID
	}
	task.LeaseOwner = ""
	task.LeaseExpiresAt = time.Time{}
	task.UpdatedAt = at
	if dispatchErr != nil {
		task.LastError = dispatchErr.Error()
	} else {
		task.LastError = ""
	}
	if task.ScheduleType == Once {
		if dispatchErr != nil {
			task.Status = Failed
		} else {
			task.Status = Completed
		}
		task.NextRunAt = time.Time{}
	} else {
		task.Status = Enabled
		task.NextRunAt = next
	}
	store.tasks[id] = task
	return task, nil
}

// SetStatus changes an owned non-running task to enabled or paused.
func (store *MemoryStore) SetStatus(ctx context.Context, id, userID string, status Status, at time.Time) (Task, error) {
	if err := ctx.Err(); err != nil {
		return Task{}, err
	}
	if status != Enabled && status != Paused {
		return Task{}, ErrInvalid
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	task, exists := store.tasks[id]
	if !exists || task.UserID != userID {
		return Task{}, ErrNotFound
	}
	if task.Status == Running {
		return Task{}, ErrConflict
	}
	if status == Enabled && task.NextRunAt.IsZero() {
		next, err := NextRun(task.ScheduleType, task.Schedule, task.Timezone, at)
		if err != nil {
			return Task{}, err
		}
		task.NextRunAt = next
	}
	task.Status = status
	task.UpdatedAt = at
	store.tasks[id] = task
	return task, nil
}

// Validate verifies durable task invariants and schedule syntax.
func (task Task) Validate() error {
	if strings.TrimSpace(task.ID) == "" || strings.TrimSpace(task.UserID) == "" || strings.TrimSpace(task.Title) == "" || strings.TrimSpace(task.Prompt) == "" || len(task.Prompt) > 1<<20 || task.CreatedAt.IsZero() || task.UpdatedAt.IsZero() {
		return ErrInvalid
	}
	if _, err := parseSchedule(task.ScheduleType, task.Schedule, task.Timezone); err != nil {
		return err
	}
	switch task.Status {
	case Enabled, Running, Paused, Completed, Failed:
	default:
		return ErrInvalid
	}
	if task.Status == Enabled && task.NextRunAt.IsZero() {
		return ErrInvalid
	}
	if task.Status == Running && (task.LeaseOwner == "" || task.LeaseExpiresAt.IsZero()) {
		return ErrInvalid
	}
	return nil
}

// NextRun computes the next UTC occurrence strictly after now.
func NextRun(scheduleType ScheduleType, spec, timezone string, now time.Time) (time.Time, error) {
	schedule, err := parseSchedule(scheduleType, spec, timezone)
	if err != nil {
		return time.Time{}, err
	}
	if scheduleType == Once {
		at := schedule.Next(now.Add(-time.Nanosecond))
		if !at.After(now) {
			return time.Time{}, ErrInvalid
		}
		return at.UTC(), nil
	}
	return schedule.Next(now).UTC(), nil
}

type onceSchedule struct{ at time.Time }

func (schedule onceSchedule) Next(time.Time) time.Time { return schedule.at }
func parseSchedule(scheduleType ScheduleType, spec, timezone string) (cron.Schedule, error) {
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return nil, fmt.Errorf("%w: timezone: %w", ErrInvalid, err)
	}
	switch scheduleType {
	case Once:
		at, parseErr := time.ParseInLocation(time.RFC3339, spec, location)
		if parseErr != nil {
			return nil, fmt.Errorf("%w: once schedule: %w", ErrInvalid, parseErr)
		}
		return onceSchedule{at: at}, nil
	case Cron:
		schedule, parseErr := cron.ParseStandard(spec)
		if parseErr != nil {
			return nil, fmt.Errorf("%w: cron schedule: %w", ErrInvalid, parseErr)
		}
		return locationSchedule{schedule: schedule, location: location}, nil
	default:
		return nil, ErrInvalid
	}
}

type locationSchedule struct {
	schedule cron.Schedule
	location *time.Location
}

func (schedule locationSchedule) Next(after time.Time) time.Time {
	return schedule.schedule.Next(after.In(schedule.location))
}

// Engine leases and dispatches due tasks.
type Engine struct {
	store         Store
	executor      Executor
	owner         string
	leaseDuration time.Duration
	batch         int
	now           func() time.Time
	pollInterval  time.Duration
	onError       func(error)
}

// Config controls scheduler ownership and bounded dispatch.
type Config struct {
	Store         Store
	Executor      Executor
	Owner         string
	LeaseDuration time.Duration
	BatchSize     int
	Now           func() time.Time
	PollInterval  time.Duration
	OnError       func(error)
}

// New validates config and constructs an engine.
func New(config Config) (*Engine, error) {
	if config.PollInterval == 0 {
		config.PollInterval = time.Second
	}
	if config.Store == nil || config.Executor == nil || strings.TrimSpace(config.Owner) == "" || config.LeaseDuration < time.Second || config.PollInterval < 100*time.Millisecond || config.BatchSize < 1 || config.BatchSize > 1000 {
		return nil, ErrInvalid
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Engine{store: config.Store, executor: config.Executor, owner: config.Owner, leaseDuration: config.LeaseDuration, batch: config.BatchSize, now: config.Now, pollInterval: config.PollInterval, onError: config.OnError}, nil
}

// Run dispatches immediately and after every poll interval until cancellation.
// Individual poll and dispatch failures are reported through Config.OnError.
func (engine *Engine) Run(ctx context.Context) error {
	if engine == nil {
		return ErrInvalid
	}
	for {
		if err := engine.RunOnce(ctx); err != nil && ctx.Err() == nil && engine.onError != nil {
			engine.onError(err)
		}
		timer := time.NewTimer(engine.pollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// RunOnce claims and synchronously dispatches one bounded due batch.
func (engine *Engine) RunOnce(ctx context.Context) error {
	if engine == nil {
		return ErrInvalid
	}
	now := engine.now().UTC()
	tasks, err := engine.store.Due(ctx, now, engine.batch)
	if err != nil {
		return err
	}
	var failures []error
	for _, task := range tasks {
		dispatchErr := engine.dispatch(ctx, task, now)
		if errors.Is(dispatchErr, ErrConflict) {
			continue
		}
		failures = append(failures, dispatchErr)
	}
	return errors.Join(failures...)
}

// RunTask claims and dispatches exactly one task occurrence.
func (engine *Engine) RunTask(ctx context.Context, task Task) error {
	if engine == nil {
		return ErrInvalid
	}
	return engine.dispatch(ctx, task, engine.now().UTC())
}

func (engine *Engine) dispatch(ctx context.Context, task Task, now time.Time) error {
	claimed, err := engine.store.Claim(ctx, task.ID, task.NextRunAt, engine.owner, now, now.Add(engine.leaseDuration))
	if err != nil {
		return err
	}
	result, dispatchErr := engine.executor.Execute(ctx, claimed)
	next := time.Time{}
	if claimed.ScheduleType == Cron {
		next, _ = NextRun(Cron, claimed.Schedule, claimed.Timezone, now)
	}
	_, finishErr := engine.store.Finish(context.WithoutCancel(ctx), claimed.ID, engine.owner, engine.now().UTC(), next, result, dispatchErr)
	return errors.Join(dispatchErr, finishErr)
}
