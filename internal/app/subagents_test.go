package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/contextwindow"
	"github.com/Rememorio/gofer/internal/delivery"
	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/gateway"
	"github.com/Rememorio/gofer/internal/loopdetect"
	"github.com/Rememorio/gofer/internal/model"
	"github.com/Rememorio/gofer/internal/readbeforewrite"
	"github.com/Rememorio/gofer/internal/runtime"
	"github.com/Rememorio/gofer/internal/subagent"
	"github.com/Rememorio/gofer/internal/toolhistory"
	"github.com/Rememorio/gofer/internal/workspace"
)

func TestSubagentToolsRunIsolatedChildAgent(t *testing.T) {
	t.Parallel()
	service, workspace, launch := subagentFixture(t)
	provider := &model.Scripted{Responses: [][]model.Chunk{{
		{Kind: model.ChunkTextDelta, Text: "child result"},
		{Kind: model.ChunkDone, StopReason: model.StopEndTurn},
	}}}
	registry, _, children, err := service.buildTools(workspace, launch, configuredProvider{provider: provider, model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = children.Close() }()
	spawned, err := registry.Execute(context.Background(), domain.ToolCall{
		ID: "spawn", Name: "subagent_spawn", Arguments: json.RawMessage(`{"prompt":"investigate"}`),
	})
	if err != nil || spawned.IsError {
		t.Fatalf("spawn = %#v, %v", spawned, err)
	}
	var task subagent.Task
	if err = json.Unmarshal(spawned.Output, &task); err != nil {
		t.Fatal(err)
	}
	waited, err := registry.Execute(context.Background(), domain.ToolCall{
		ID: "wait", Name: "subagent_wait", Arguments: json.RawMessage(`{"id":"` + task.ID + `"}`),
	})
	if err != nil || waited.IsError {
		t.Fatalf("wait = %#v, %v", waited, err)
	}
	if err = json.Unmarshal(waited.Output, &task); err != nil {
		t.Fatal(err)
	}
	if task.Status != subagent.Succeeded || task.Output.Text != "child result" || task.Output.Metadata["parent_run_id"] != string(launch.RunID) || task.Output.Metadata["model"] != "test" || task.Output.Metadata["llm_call_count"] != "1" || task.Output.Metadata["stop_reason"] != "end_turn" {
		t.Fatalf("task = %#v", task)
	}
	const guardedPrompt = "--- BEGIN USER INPUT ---\ninvestigate\n--- END USER INPUT ---"
	if len(provider.Requests) != 1 || provider.Requests[0].Messages[0].Content[0].Text != guardedPrompt {
		t.Fatalf("provider requests = %#v", provider.Requests)
	}
}

func TestChildExecutorSurfacesLoopCappedStopReason(t *testing.T) {
	t.Parallel()
	service, threadWorkspace, launch := subagentFixture(t)
	service.config.LoopDetection.WarnThreshold = 2
	service.config.LoopDetection.HardLimit = 4
	service.config.LoopDetection.ToolFrequencyWarn = 100
	service.config.LoopDetection.ToolFrequencyLimit = 200
	responses := make([][]model.Chunk, 4)
	for index := range responses {
		call := domain.ToolCall{
			ID: fmt.Sprintf("list-%d", index), Name: "ls",
			Arguments: json.RawMessage(`{"path":"/mnt/user-data/workspace","max_depth":1}`),
		}
		responses[index] = []model.Chunk{
			{Kind: model.ChunkToolCall, ToolCall: &call},
			{Kind: model.ChunkDone, StopReason: model.StopToolUse},
		}
	}
	provider := &model.Scripted{Responses: responses}
	executor := childExecutor{
		service: service, workspace: threadWorkspace, launch: launch,
		provider: configuredProvider{provider: provider, model: "test"},
	}
	output, err := executor.Execute(context.Background(), subagent.Request{Prompt: "loop", Depth: 1})
	if err != nil {
		t.Fatal(err)
	}
	if output.Metadata["stop_reason"] != string(model.StopLoopCapped) || output.Metadata["llm_call_count"] != "4" ||
		!strings.Contains(output.Text, "FORCED STOP") || len(provider.Requests) != 4 {
		t.Fatalf("output = %#v, requests=%d", output, len(provider.Requests))
	}
}

func TestChildExecutorRequiresTextResult(t *testing.T) {
	t.Parallel()
	service, workspace, launch := subagentFixture(t)
	provider := &model.Scripted{Responses: [][]model.Chunk{{{Kind: model.ChunkDone, StopReason: model.StopEndTurn}}}}
	executor := childExecutor{service: service, workspace: workspace, launch: launch, provider: configuredProvider{provider: provider, model: "test"}}
	if _, err := executor.Execute(context.Background(), subagent.Request{Prompt: "work", Depth: 1}); err == nil {
		t.Fatal("Execute(empty text) error = nil")
	}
	if _, err := executor.Execute(context.Background(), subagent.Request{}); err == nil {
		t.Fatal("Execute(empty prompt) error = nil")
	}
	failing := &model.Scripted{Err: errors.New("upstream")}
	executor.provider = configuredProvider{provider: failing, model: "test"}
	if output, err := executor.Execute(context.Background(), subagent.Request{Prompt: "work", Depth: 1}); err == nil || output.Metadata["run_id"] == "" || output.Metadata["llm_call_count"] != "1" {
		t.Fatal("Execute(provider failure) error = nil")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := executor.Execute(cancelled, subagent.Request{Prompt: "work", Depth: 1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute(cancelled) error = %v", err)
	}
	user, _ := domain.NewTextMessage(domain.RoleUser, "user", time.Now())
	if got := finalAssistantText([]domain.Message{user}); got != "" {
		t.Fatalf("finalAssistantText(user) = %q", got)
	}
}

func TestSubagentFinishHookCancelsAndDrainsChildren(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	manager, err := subagent.NewManager(context.Background(), subagent.Config{
		Executor: testSubagentExecutor(func(ctx context.Context, _ subagent.Request) (subagent.Output, error) {
			close(started)
			<-ctx.Done()
			return subagent.Output{}, ctx.Err()
		}),
		MaxParallel: 1, MaxChildren: 1, MaxDepth: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	task, err := manager.Spawn(context.Background(), subagent.Request{Prompt: "work", Depth: 1})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err = subagentFinishHook(manager).Finish(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	task, err = manager.Get(task.ID)
	if err != nil || task.Status != subagent.Cancelled {
		t.Fatalf("drained task = %#v, %v", task, err)
	}
	if _, err = manager.Spawn(context.Background(), subagent.Request{Prompt: "again", Depth: 1}); !errors.Is(err, subagent.ErrClosed) {
		t.Fatalf("Spawn after finish = %v", err)
	}
}

func TestChildExecutorSharesRunObservers(t *testing.T) {
	t.Parallel()
	service, threadWorkspace, launch := subagentFixture(t)
	tracker := delivery.NewTracker()
	executor := childExecutor{
		service: service, workspace: threadWorkspace, launch: launch,
		provider:  service.providers["primary"],
		observers: []runtime.Middleware{tracker},
	}
	_, middleware, err := executor.childTools()
	if err != nil {
		t.Fatal(err)
	}
	if len(middleware) == 0 || middleware[len(middleware)-1] != tracker {
		t.Fatalf("child middleware = %#v", middleware)
	}
	assertFileGateOrdering(t, middleware)
}

func assertFileGateOrdering(t *testing.T, middleware []runtime.Middleware) {
	t.Helper()
	compactorIndex, gateIndex, loopIndex, repairIndex := -1, -1, -1, -1
	for index, candidate := range middleware {
		switch candidate.(type) {
		case *contextwindow.Compactor:
			compactorIndex = index
		case *readbeforewrite.Middleware:
			gateIndex = index
		case *loopdetect.Middleware:
			loopIndex = index
		case *toolhistory.Middleware:
			repairIndex = index
		}
	}
	if compactorIndex < 0 || gateIndex <= compactorIndex || loopIndex <= gateIndex || repairIndex <= loopIndex {
		t.Fatalf("runtime guards are out of order: %#v", middleware)
	}
}

type testSubagentExecutor func(context.Context, subagent.Request) (subagent.Output, error)

func (executor testSubagentExecutor) Execute(ctx context.Context, request subagent.Request) (subagent.Output, error) {
	return executor(ctx, request)
}

func TestBuildToolsClosesChildrenOnAssemblyErrors(t *testing.T) {
	t.Parallel()
	service, threadWorkspace, launch := subagentFixture(t)
	provider := service.providers["primary"]
	controls := service.controls
	service.controls = nil
	if _, _, _, err := service.buildTools(threadWorkspace, launch, provider); err == nil {
		t.Fatal("buildTools(nil controls) error = nil")
	}
	service.controls = controls
	maxSubagents := service.config.Runtime.MaxSubagents
	service.config.Runtime.MaxSubagents = 0
	if _, _, _, err := service.buildTools(threadWorkspace, launch, provider); err == nil {
		t.Fatal("buildTools(invalid children) error = nil")
	}
	service.config.Runtime.MaxSubagents = maxSubagents
	service.config.ReadBeforeWrite.Enabled = false
	service.config.LoopDetection.Enabled = false
	_, middleware, children, err := service.buildTools(threadWorkspace, launch, provider)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = children.Close() }()
	repairSeen := false
	for _, candidate := range middleware {
		if _, ok := candidate.(*readbeforewrite.Middleware); ok {
			t.Fatal("disabled read-before-write gate was assembled")
		}
		if _, ok := candidate.(*loopdetect.Middleware); ok {
			t.Fatal("disabled loop detector was assembled")
		}
		if _, ok := candidate.(*toolhistory.Middleware); ok {
			repairSeen = true
		}
	}
	if !repairSeen {
		t.Fatal("always-on tool history repair was not assembled")
	}
}

func subagentFixture(t *testing.T) (*Service, *workspace.Thread, gateway.StartRequest) {
	t.Helper()
	cfg := testConfig(t, "https://example.invalid/v1")
	service, err := New(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	thread, _ := domain.NewThread(time.Now())
	threadWorkspace, err := service.workspaces.Open(thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = threadWorkspace.Close() })
	run, _ := domain.NewRun(thread.ID, time.Now())
	return service, threadWorkspace, gateway.StartRequest{RunID: run.ID, ThreadID: thread.ID}
}
