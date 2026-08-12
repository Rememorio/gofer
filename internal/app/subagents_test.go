package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/gateway"
	"github.com/Rememorio/gofer/internal/model"
	"github.com/Rememorio/gofer/internal/subagent"
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
	if task.Status != subagent.Succeeded || task.Output.Text != "child result" || task.Output.Metadata["parent_run_id"] != string(launch.RunID) {
		t.Fatalf("task = %#v", task)
	}
	if len(provider.Requests) != 1 || provider.Requests[0].Messages[0].Content[0].Text != "investigate" {
		t.Fatalf("provider requests = %#v", provider.Requests)
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
	if _, err := executor.Execute(context.Background(), subagent.Request{Prompt: "work", Depth: 1}); err == nil {
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
