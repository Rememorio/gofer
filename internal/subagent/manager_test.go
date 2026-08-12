package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/tool"
)

type executorFunc func(context.Context, Request) (Output, error)

func (function executorFunc) Execute(ctx context.Context, request Request) (Output, error) {
	return function(ctx, request)
}

type blockingExecutor struct {
	active  atomic.Int32
	peak    atomic.Int32
	release <-chan struct{}
}

func (executor *blockingExecutor) Execute(ctx context.Context, request Request) (Output, error) {
	current := executor.active.Add(1)
	defer executor.active.Add(-1)
	for {
		old := executor.peak.Load()
		if current <= old || executor.peak.CompareAndSwap(old, current) {
			break
		}
	}
	select {
	case <-ctx.Done():
		return Output{}, ctx.Err()
	case <-executor.release:
		return Output{Text: request.Prompt}, nil
	}
}

func TestManagerBoundsParallelismAndFansInEvents(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	executor := &blockingExecutor{release: release}
	var id atomic.Int32
	manager, err := NewManager(context.Background(), Config{Executor: executor, MaxParallel: 2, MaxChildren: 4, MaxDepth: 2, NewID: func() (string, error) { return fmt.Sprintf("sub-%d", id.Add(1)), nil }})
	if err != nil {
		t.Fatal(err)
	}
	tasks := make([]Task, 4)
	for i := range tasks {
		tasks[i], err = manager.Spawn(context.Background(), Request{Depth: 1, Prompt: fmt.Sprintf("%d", i)})
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := manager.Spawn(context.Background(), Request{Depth: 1, Prompt: "overflow"}); !errors.Is(err, ErrCapacity) {
		t.Fatalf("capacity=%v", err)
	}
	deadline := time.Now().Add(time.Second)
	for executor.peak.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if executor.peak.Load() != 2 {
		t.Fatalf("peak=%d", executor.peak.Load())
	}
	close(release)
	for _, task := range tasks {
		done, waitErr := manager.Wait(context.Background(), task.ID)
		if waitErr != nil || done.Status != Succeeded {
			t.Fatalf("done=%#v err=%v", done, waitErr)
		}
	}
	if events := manager.Events(0, 0); len(events) != 12 {
		t.Fatalf("events=%#v", events)
	} else {
		for i, event := range events {
			if event.Sequence != uint64(i+1) {
				t.Fatal("unordered events")
			}
		}
	}
	if len(manager.List()) != 4 {
		t.Fatal("list")
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestManagerCancellationFailuresAndClose(t *testing.T) {
	t.Parallel()
	var id atomic.Int32
	started := make(chan struct{})
	once := sync.Once{}
	manager, _ := NewManager(context.Background(), Config{Executor: executorFunc(func(ctx context.Context, request Request) (Output, error) {
		if request.Prompt == "fail" {
			return Output{Metadata: map[string]string{"usage": "preserved"}}, errors.New("boom")
		}
		once.Do(func() { close(started) })
		<-ctx.Done()
		return Output{}, ctx.Err()
	}), MaxParallel: 1, MaxChildren: 3, MaxDepth: 1, NewID: func() (string, error) { return fmt.Sprint(id.Add(1)), nil }})
	failed, _ := manager.Spawn(context.Background(), Request{Depth: 1, Prompt: "fail"})
	result, _ := manager.Wait(context.Background(), failed.ID)
	if result.Status != Failed || result.Error != "boom" || result.Output.Metadata["usage"] != "preserved" {
		t.Fatalf("failed=%#v", result)
	}
	running, _ := manager.Spawn(context.Background(), Request{Depth: 1, Prompt: "wait"})
	<-started
	if err := manager.Cancel(running.ID); err != nil {
		t.Fatal(err)
	}
	result, _ = manager.Wait(context.Background(), running.ID)
	if result.Status != Cancelled {
		t.Fatalf("cancelled=%#v", result)
	}
	if err := manager.Cancel("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Cancel=%v", err)
	}
	if _, err := manager.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get=%v", err)
	}
	if _, err := manager.Wait(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Wait=%v", err)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Spawn(context.Background(), Request{Depth: 1, Prompt: "x"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Spawn closed=%v", err)
	}
}

func TestManagerValidationAndIsolation(t *testing.T) {
	t.Parallel()
	valid := Config{Executor: executorFunc(func(context.Context, Request) (Output, error) { return Output{Text: "ok"}, nil }), MaxParallel: 1, MaxChildren: 1, MaxDepth: 1}
	for _, mutate := range []func(*Config){func(c *Config) { c.Executor = nil }, func(c *Config) { c.MaxParallel = 0 }, func(c *Config) { c.MaxChildren = 0 }, func(c *Config) { c.MaxDepth = 0 }, func(c *Config) { c.MaxParallel = 2 }} {
		candidate := valid
		mutate(&candidate)
		if _, err := NewManager(context.Background(), candidate); !errors.Is(err, ErrInvalid) {
			t.Fatalf("NewManager=%v", err)
		}
	}
	manager, _ := NewManager(context.Background(), valid)
	if _, err := manager.Spawn(context.Background(), Request{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Spawn invalid=%v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := manager.Spawn(cancelled, Request{Depth: 1, Prompt: "x"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Spawn cancel=%v", err)
	}
	task, err := manager.Spawn(context.Background(), Request{Depth: 1, Prompt: "x", Metadata: map[string]string{"k": "v"}})
	if err != nil {
		t.Fatal(err)
	}
	task.Request.Metadata["k"] = "bad"
	done, _ := manager.Wait(context.Background(), task.ID)
	if done.Request.Metadata["k"] != "v" {
		t.Fatal("shared metadata")
	}
	_ = manager.Close()
}

func TestToolsManageChildren(t *testing.T) {
	t.Parallel()
	manager, _ := NewManager(context.Background(), Config{Executor: executorFunc(func(_ context.Context, request Request) (Output, error) {
		return Output{Text: request.Prompt}, nil
	}), MaxParallel: 1, MaxChildren: 2, MaxDepth: 2, NewID: func() (string, error) { return "child", nil }})
	registry := tool.NewRegistry()
	if err := (Tools{Manager: manager, ParentID: "root", Depth: 0}).Register(registry); err != nil {
		t.Fatal(err)
	}
	execute := func(name, arguments string) domain.ToolResult {
		result, err := registry.Execute(context.Background(), domain.ToolCall{ID: name, Name: name, Arguments: json.RawMessage(arguments)})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	spawned := execute("subagent_spawn", `{"prompt":"research"}`)
	if spawned.IsError {
		t.Fatalf("spawn = %s", spawned.Output)
	}
	if waited := execute("subagent_wait", `{"id":"child"}`); waited.IsError {
		t.Fatalf("wait = %s", waited.Output)
	}
	if listed := execute("subagent_list", `{}`); listed.IsError {
		t.Fatalf("list = %s", listed.Output)
	}
	if got := execute("subagent_get", `{"id":"child"}`); got.IsError {
		t.Fatalf("get = %s", got.Output)
	}
	if missing := execute("subagent_cancel", `{"id":"missing"}`); !missing.IsError {
		t.Fatal("cancel missing succeeded")
	}
	if len(PolicyDescriptors()) != 5 {
		t.Fatal("policy descriptors")
	}
	if err := (Tools{}).Register(registry); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Register invalid = %v", err)
	}
	_ = manager.Close()
}
