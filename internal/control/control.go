package control

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
)

var (
	// ErrInvalid identifies malformed goal or todo state.
	ErrInvalid = errors.New("invalid control state")
	// ErrNotFound identifies an unknown control state object.
	ErrNotFound = errors.New("control state not found")
	// ErrConflict identifies an optimistic version mismatch.
	ErrConflict = errors.New("control state conflict")
)

// GoalStatus is the lifecycle of the single thread goal.
type GoalStatus string

const (
	// GoalActive identifies an unfinished goal.
	GoalActive GoalStatus = "active"
	// GoalComplete identifies an achieved goal.
	GoalComplete GoalStatus = "complete"
	// GoalBlocked identifies a goal that cannot currently progress.
	GoalBlocked GoalStatus = "blocked"
)

// TodoStatus is the lifecycle of one concrete work item.
type TodoStatus string

const (
	// TodoPending identifies work that has not started.
	TodoPending TodoStatus = "pending"
	// TodoInProgress identifies the one currently active work item.
	TodoInProgress TodoStatus = "in_progress"
	// TodoCompleted identifies finished work.
	TodoCompleted TodoStatus = "completed"
)

// Goal captures an explicit long-running objective and optional budget.
type Goal struct {
	Objective   string     `json:"objective"`
	Status      GoalStatus `json:"status"`
	TokenBudget int        `json:"token_budget,omitempty"`
	StartedAt   time.Time  `json:"started_at"`
	FinishedAt  time.Time  `json:"finished_at,omitempty"`
}

// Todo is one ordered plan item.
type Todo struct {
	ID        string     `json:"id"`
	Step      string     `json:"step"`
	Status    TodoStatus `json:"status"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// State is an immutable optimistic snapshot for one thread.
type State struct {
	ThreadID  domain.ThreadID `json:"thread_id"`
	Version   uint64          `json:"version"`
	Goal      *Goal           `json:"goal,omitempty"`
	Todos     []Todo          `json:"todos"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// Store is the durable optimistic control-state contract.
type Store interface {
	Load(context.Context, domain.ThreadID) (State, error)
	CompareAndSwap(context.Context, State, uint64) (State, error)
	Delete(context.Context, domain.ThreadID) error
}

// InMemory is the concurrency-safe reference Store.
type InMemory struct {
	mu     sync.RWMutex
	states map[domain.ThreadID]State
}

// NewInMemory constructs an empty control store.
func NewInMemory() *InMemory { return &InMemory{states: make(map[domain.ThreadID]State)} }

// Load returns an isolated state snapshot.
func (store *InMemory) Load(ctx context.Context, threadID domain.ThreadID) (State, error) {
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	if _, err := domain.ParseThreadID(string(threadID)); err != nil {
		return State{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	store.mu.RLock()
	state, exists := store.states[threadID]
	store.mu.RUnlock()
	if !exists {
		return State{ThreadID: threadID, Todos: []Todo{}}, nil
	}
	return cloneState(state), nil
}

// CompareAndSwap atomically stores next if its current version equals expected.
func (store *InMemory) CompareAndSwap(ctx context.Context, next State, expected uint64) (State, error) {
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	if err := next.Validate(); err != nil {
		return State{}, err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	current := store.states[next.ThreadID]
	if current.Version != expected {
		return State{}, fmt.Errorf("%w: have %d, expected %d", ErrConflict, current.Version, expected)
	}
	next.Version = expected + 1
	store.states[next.ThreadID] = cloneState(next)
	return cloneState(next), nil
}

// Delete removes control state for a deleted thread. Missing state is a no-op.
func (store *InMemory) Delete(ctx context.Context, threadID domain.ThreadID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := domain.ParseThreadID(string(threadID)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	store.mu.Lock()
	delete(store.states, threadID)
	store.mu.Unlock()
	return nil
}

// Service applies validated goal and todo transitions.
type Service struct {
	store Store
	now   func() time.Time
}

// NewService constructs a control service.
func NewService(store Store, now func() time.Time) (*Service, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: store is required", ErrInvalid)
	}
	if now == nil {
		now = time.Now
	}
	return &Service{store: store, now: now}, nil
}

// Snapshot returns current thread control state.
func (service *Service) Snapshot(ctx context.Context, threadID domain.ThreadID) (State, error) {
	return service.store.Load(ctx, threadID)
}

// CreateGoal creates a goal only when no unfinished goal exists.
func (service *Service) CreateGoal(ctx context.Context, threadID domain.ThreadID, objective string, tokenBudget int) (State, error) {
	objective = strings.TrimSpace(objective)
	if objective == "" || len(objective) > 16<<10 || tokenBudget < 0 {
		return State{}, fmt.Errorf("%w: objective and non-negative budget are required", ErrInvalid)
	}
	return service.update(ctx, threadID, func(state *State) error {
		if state.Goal != nil && state.Goal.Status == GoalActive {
			return errors.New("an active goal already exists")
		}
		now := service.now()
		state.Goal = &Goal{Objective: objective, Status: GoalActive, TokenBudget: tokenBudget, StartedAt: now}
		state.Todos = []Todo{}
		state.UpdatedAt = now
		return nil
	})
}

// SetGoal creates or replaces the thread goal and clears its todo plan.
func (service *Service) SetGoal(ctx context.Context, threadID domain.ThreadID, objective string, tokenBudget int) (State, error) {
	objective = strings.TrimSpace(objective)
	if objective == "" || len(objective) > 16<<10 || tokenBudget < 0 {
		return State{}, fmt.Errorf("%w: objective and non-negative budget are required", ErrInvalid)
	}
	return service.update(ctx, threadID, func(state *State) error {
		now := service.now()
		state.Goal = &Goal{Objective: objective, Status: GoalActive, TokenBudget: tokenBudget, StartedAt: now}
		state.Todos = []Todo{}
		state.UpdatedAt = now
		return nil
	})
}

// ClearGoal removes the goal and todo plan while retaining optimistic history.
func (service *Service) ClearGoal(ctx context.Context, threadID domain.ThreadID) (State, error) {
	return service.update(ctx, threadID, func(state *State) error {
		state.Goal = nil
		state.Todos = []Todo{}
		state.UpdatedAt = service.now()
		return nil
	})
}

// Delete removes all control state owned by a deleted thread.
func (service *Service) Delete(ctx context.Context, threadID domain.ThreadID) error {
	return service.store.Delete(ctx, threadID)
}

// SetGoalStatus completes or blocks the active goal.
func (service *Service) SetGoalStatus(ctx context.Context, threadID domain.ThreadID, status GoalStatus) (State, error) {
	if status != GoalComplete && status != GoalBlocked {
		return State{}, fmt.Errorf("%w: terminal goal status is required", ErrInvalid)
	}
	return service.update(ctx, threadID, func(state *State) error {
		if state.Goal == nil || state.Goal.Status != GoalActive {
			return errors.New("no active goal")
		}
		if status == GoalComplete {
			for _, todo := range state.Todos {
				if todo.Status != TodoCompleted {
					return errors.New("goal has unfinished todos")
				}
			}
		}
		now := service.now()
		state.Goal.Status = status
		state.Goal.FinishedAt = now
		state.UpdatedAt = now
		return nil
	})
}

// ReplaceTodos atomically replaces the ordered plan for the active goal.
func (service *Service) ReplaceTodos(ctx context.Context, threadID domain.ThreadID, todos []Todo) (State, error) {
	return service.update(ctx, threadID, func(state *State) error {
		if state.Goal == nil || state.Goal.Status != GoalActive {
			return errors.New("todos require an active goal")
		}
		now := service.now()
		normalized := make([]Todo, len(todos))
		for index, todo := range todos {
			normalized[index] = todo
			normalized[index].ID = strings.TrimSpace(todo.ID)
			if normalized[index].ID == "" {
				normalized[index].ID = fmt.Sprintf("todo-%d", index+1)
			}
			normalized[index].Step = strings.TrimSpace(todo.Step)
			normalized[index].UpdatedAt = now
		}
		state.Todos = normalized
		state.UpdatedAt = now
		return nil
	})
}

func (service *Service) update(ctx context.Context, threadID domain.ThreadID, mutate func(*State) error) (State, error) {
	for attempt := 0; attempt < 8; attempt++ {
		state, err := service.store.Load(ctx, threadID)
		if err != nil {
			return State{}, err
		}
		if err := mutate(&state); err != nil {
			return State{}, err
		}
		saved, err := service.store.CompareAndSwap(ctx, state, state.Version)
		if !errors.Is(err, ErrConflict) {
			return saved, err
		}
	}
	return State{}, ErrConflict
}

// Validate verifies state invariants.
func (state State) Validate() error {
	if _, err := domain.ParseThreadID(string(state.ThreadID)); err != nil || state.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: thread and update time are required", ErrInvalid)
	}
	if state.Goal != nil {
		if err := state.Goal.validate(); err != nil {
			return err
		}
	}
	seen := make(map[string]struct{}, len(state.Todos))
	active := 0
	for _, todo := range state.Todos {
		if strings.TrimSpace(todo.ID) == "" || strings.TrimSpace(todo.Step) == "" || len(todo.Step) > 8<<10 || todo.UpdatedAt.IsZero() || !todo.Status.valid() {
			return fmt.Errorf("%w: invalid todo", ErrInvalid)
		}
		if _, exists := seen[todo.ID]; exists {
			return fmt.Errorf("%w: duplicate todo id", ErrInvalid)
		}
		seen[todo.ID] = struct{}{}
		if todo.Status == TodoInProgress {
			active++
		}
	}
	if active > 1 {
		return fmt.Errorf("%w: at most one todo may be in progress", ErrInvalid)
	}
	return nil
}

func (goal Goal) validate() error {
	if strings.TrimSpace(goal.Objective) == "" || goal.TokenBudget < 0 || goal.StartedAt.IsZero() {
		return fmt.Errorf("%w: invalid goal", ErrInvalid)
	}
	switch goal.Status {
	case GoalActive:
		if !goal.FinishedAt.IsZero() {
			return fmt.Errorf("%w: active goal is finished", ErrInvalid)
		}
	case GoalComplete, GoalBlocked:
		if goal.FinishedAt.IsZero() || goal.FinishedAt.Before(goal.StartedAt) {
			return fmt.Errorf("%w: terminal goal needs finish time", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: invalid goal status", ErrInvalid)
	}
	return nil
}

func (status TodoStatus) valid() bool {
	return status == TodoPending || status == TodoInProgress || status == TodoCompleted
}

func cloneState(state State) State {
	cloned := state
	if state.Goal != nil {
		goal := *state.Goal
		cloned.Goal = &goal
	}
	cloned.Todos = append([]Todo(nil), state.Todos...)
	return cloned
}
