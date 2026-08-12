package control

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/tool"
)

func TestServiceGoalTodoLifecycle(t *testing.T) {
	t.Parallel()
	threadID := newThreadID(t)
	now := time.Unix(100, 0)
	service, _ := NewService(NewInMemory(), func() time.Time { return now })
	state, err := service.CreateGoal(context.Background(), threadID, " ship gofer ", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if state.Version != 1 || state.Goal.Objective != "ship gofer" || state.Goal.Status != GoalActive {
		t.Fatalf("state=%#v", state)
	}
	state, err = service.ReplaceTodos(context.Background(), threadID, []Todo{{Step: "one", Status: TodoInProgress}, {ID: "two", Step: "two", Status: TodoPending}})
	if err != nil {
		t.Fatal(err)
	}
	if state.Version != 2 || state.Todos[0].ID != "todo-1" {
		t.Fatalf("state=%#v", state)
	}
	if _, err := service.SetGoalStatus(context.Background(), threadID, GoalComplete); err == nil {
		t.Fatal("completed unfinished goal")
	}
	state, err = service.ReplaceTodos(context.Background(), threadID, []Todo{{ID: "one", Step: "one", Status: TodoCompleted}, {ID: "two", Step: "two", Status: TodoCompleted}})
	if err != nil {
		t.Fatal(err)
	}
	state, err = service.SetGoalStatus(context.Background(), threadID, GoalComplete)
	if err != nil {
		t.Fatal(err)
	}
	if state.Goal.Status != GoalComplete || state.Goal.FinishedAt.IsZero() {
		t.Fatalf("goal=%#v", state.Goal)
	}
	if _, err := service.CreateGoal(context.Background(), threadID, "next", 0); err != nil {
		t.Fatal(err)
	}
}

func TestServiceRejectsInvalidTransitions(t *testing.T) {
	t.Parallel()
	id := newThreadID(t)
	service, _ := NewService(NewInMemory(), nil)
	ctx := context.Background()
	if _, err := service.CreateGoal(ctx, id, "", 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty=%v", err)
	}
	if _, err := service.ReplaceTodos(ctx, id, nil); err == nil {
		t.Fatal("todos without goal")
	}
	if _, err := service.SetGoalStatus(ctx, id, GoalActive); !errors.Is(err, ErrInvalid) {
		t.Fatalf("status=%v", err)
	}
	_, _ = service.CreateGoal(ctx, id, "goal", 0)
	if _, err := service.CreateGoal(ctx, id, "other", 0); err == nil {
		t.Fatal("duplicate active goal")
	}
	if _, err := service.ReplaceTodos(ctx, id, []Todo{{ID: "x", Step: "a", Status: TodoInProgress}, {ID: "y", Step: "b", Status: TodoInProgress}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("multiple active=%v", err)
	}
}

func TestServiceSetClearAndDeleteGoal(t *testing.T) {
	t.Parallel()
	id := newThreadID(t)
	store := NewInMemory()
	service, _ := NewService(store, nil)
	ctx := context.Background()
	if _, err := service.SetGoal(ctx, id, " first ", 10); err != nil {
		t.Fatal(err)
	}
	state, err := service.SetGoal(ctx, id, "second", 20)
	if err != nil || state.Goal.Objective != "second" || state.Goal.TokenBudget != 20 || len(state.Todos) != 0 {
		t.Fatalf("SetGoal() = %#v, %v", state, err)
	}
	state, err = service.ClearGoal(ctx, id)
	if err != nil || state.Goal != nil || len(state.Todos) != 0 {
		t.Fatalf("ClearGoal() = %#v, %v", state, err)
	}
	if err = service.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	state, err = service.Snapshot(ctx, id)
	if err != nil || state.Version != 0 {
		t.Fatalf("Snapshot() = %#v, %v", state, err)
	}
	if _, err = service.SetGoal(ctx, id, "", 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("SetGoal(invalid) = %v", err)
	}
}

func TestInMemoryCASIsolationAndConcurrency(t *testing.T) {
	t.Parallel()
	id := newThreadID(t)
	store := NewInMemory()
	now := time.Now()
	next := State{ThreadID: id, Goal: &Goal{Objective: "x", Status: GoalActive, StartedAt: now}, Todos: []Todo{}, UpdatedAt: now}
	saved, err := store.CompareAndSwap(context.Background(), next, 0)
	if err != nil {
		t.Fatal(err)
	}
	saved.Goal.Objective = "mutated"
	loaded, _ := store.Load(context.Background(), id)
	if loaded.Goal.Objective != "x" {
		t.Fatal("shared goal")
	}
	if _, err := store.CompareAndSwap(context.Background(), next, 0); !errors.Is(err, ErrConflict) {
		t.Fatalf("CAS=%v", err)
	}
	service, _ := NewService(store, nil)
	var wait sync.WaitGroup
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, _ = service.ReplaceTodos(context.Background(), id, []Todo{{Step: "x", Status: TodoPending}})
		}()
	}
	wait.Wait()
	loaded, _ = store.Load(context.Background(), id)
	if loaded.Version != 9 {
		t.Fatalf("version=%d", loaded.Version)
	}
}

func TestStateValidation(t *testing.T) {
	t.Parallel()
	id := newThreadID(t)
	now := time.Now()
	valid := State{ThreadID: id, Goal: &Goal{Objective: "x", Status: GoalActive, StartedAt: now}, Todos: []Todo{{ID: "1", Step: "x", Status: TodoPending, UpdatedAt: now}}, UpdatedAt: now}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	mutations := []func(*State){func(s *State) { s.ThreadID = "bad" }, func(s *State) { s.UpdatedAt = time.Time{} }, func(s *State) { s.Goal.Status = "bad" }, func(s *State) { s.Todos[0].Step = "" }, func(s *State) { s.Todos = append(s.Todos, s.Todos[0]) }}
	for _, mutate := range mutations {
		candidate := cloneState(valid)
		mutate(&candidate)
		if err := candidate.Validate(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Validate()=%v", err)
		}
	}
}

func TestToolsExecuteStructuredOperations(t *testing.T) {
	t.Parallel()
	id := newThreadID(t)
	service, _ := NewService(NewInMemory(), nil)
	registry := tool.NewRegistry()
	tools := Tools{Service: service, ThreadID: id}
	if err := tools.Register(registry); err != nil {
		t.Fatal(err)
	}
	call := func(name, args string) domain.ToolResult {
		result, err := registry.Execute(context.Background(), domain.ToolCall{ID: name, Name: name, Arguments: json.RawMessage(args)})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	if result := call("goal_create", `{"objective":"build"}`); result.IsError {
		t.Fatalf("create=%s", result.Output)
	}
	if result := call("todo_write", `{"todos":[{"step":"test","status":"completed"}]}`); result.IsError {
		t.Fatalf("todos=%s", result.Output)
	}
	if result := call("goal_update", `{"status":"complete"}`); result.IsError {
		t.Fatalf("update=%s", result.Output)
	}
	if result := call("control_read", `{}`); result.IsError {
		t.Fatalf("read=%s", result.Output)
	}
	if len(PolicyDescriptors()) != 4 {
		t.Fatal("policy descriptors")
	}
	if err := (Tools{}).Register(registry); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Register(invalid)=%v", err)
	}
}

func newThreadID(t *testing.T) domain.ThreadID {
	t.Helper()
	thread, err := domain.NewThread(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return thread.ID
}
