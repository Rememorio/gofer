package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/event"
	"github.com/Rememorio/gofer/internal/model"
	"github.com/Rememorio/gofer/internal/store"
	"github.com/Rememorio/gofer/internal/tool"
)

func TestRunnerCompletesTextTurn(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t, [][]model.Chunk{{
		{Kind: model.ChunkTextDelta, Text: "hel"},
		{Kind: model.ChunkTextDelta, Text: "lo"},
		{Kind: model.ChunkUsage, Usage: &model.Usage{InputTokens: 3, OutputTokens: 2}},
		{Kind: model.ChunkDone, StopReason: model.StopEndTurn},
	}}, nil)
	result, err := fixture.runner.Run(context.Background(), fixture.request)
	if err != nil {
		t.Fatalf("Run(): %v", err)
	}
	if result.Run.Status != domain.RunSucceeded || result.Turns != 1 || result.Usage.InputTokens != 3 || result.StopReason != model.StopEndTurn {
		t.Fatalf("Result = %#v", result)
	}
	if len(result.Messages) != 2 || result.Messages[1].Content[0].Text != "hello" {
		t.Fatalf("Messages = %#v", result.Messages)
	}
	wantKinds := []event.Kind{
		event.RunStarted, event.MessageStarted, event.MessageDelta,
		event.MessageDelta, event.MessageCompleted, event.RunCompleted,
	}
	assertEventKinds(t, fixture.store, fixture.run.ID, wantKinds)
}

func TestRunnerRecordsAuxiliaryModelUsage(t *testing.T) {
	t.Parallel()
	fixture := newFixtureWithMiddleware(t, [][]model.Chunk{{
		{Kind: model.ChunkUsage, Usage: &model.Usage{InputTokens: 3, OutputTokens: 2}},
		{Kind: model.ChunkTextDelta, Text: "done"},
		{Kind: model.ChunkDone, StopReason: model.StopEndTurn},
	}}, nil, []Middleware{usageMiddleware{}})
	result, err := fixture.runner.Run(context.Background(), fixture.request)
	if err != nil || result.Usage.InputTokens != 7 || result.Usage.OutputTokens != 3 {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
	assertEventKinds(t, fixture.store, fixture.run.ID, []event.Kind{
		event.RunStarted, event.ModelUsage, event.MessageStarted, event.MessageDelta,
		event.MessageCompleted, event.RunCompleted,
	})
	if err = RecordModelUsage(context.Background(), "unused", CallerMiddleware, model.Usage{}); err != nil {
		t.Fatalf("RecordModelUsage(outside run) = %v", err)
	}
}

func TestRunnerRecordsFinishEventsBeforeTerminalEvent(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t, [][]model.Chunk{{
		{Kind: model.ChunkTextDelta, Text: "done"},
		{Kind: model.ChunkDone, StopReason: model.StopEndTurn},
	}}, nil)
	var calls atomic.Int32
	hook := FinishFunc(func(ctx context.Context, writer EventWriter) error {
		calls.Add(1)
		return writer.Append(ctx, event.WorkspaceChanges, map[string]string{"content": "changed"})
	})
	runner, err := NewRunner(RunnerConfig{Store: fixture.store, Provider: fixture.provider, FinishHooks: []FinishHook{hook}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), fixture.request)
	if err != nil || result.Run.Status != domain.RunSucceeded || calls.Load() != 1 {
		t.Fatalf("Run() = %#v, %v, calls=%d", result, err, calls.Load())
	}
	assertEventKinds(t, fixture.store, fixture.run.ID, []event.Kind{
		event.RunStarted, event.MessageStarted, event.MessageDelta, event.MessageCompleted,
		event.WorkspaceChanges, event.RunCompleted,
	})
}

func TestRunnerFailsOnceWhenFinishHookFails(t *testing.T) {
	t.Parallel()
	fixture := newFixture(t, [][]model.Chunk{{
		{Kind: model.ChunkTextDelta, Text: "done"},
		{Kind: model.ChunkDone, StopReason: model.StopEndTurn},
	}}, nil)
	var calls atomic.Int32
	var laterCalled atomic.Bool
	hookErr := errors.New("finish unavailable")
	hook := FinishFunc(func(context.Context, EventWriter) error {
		calls.Add(1)
		return hookErr
	})
	later := FinishFunc(func(context.Context, EventWriter) error {
		laterCalled.Store(true)
		return nil
	})
	runner, err := NewRunner(RunnerConfig{Store: fixture.store, Provider: fixture.provider, FinishHooks: []FinishHook{hook, later}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), fixture.request)
	if !errors.Is(err, hookErr) || result.Run.Status != domain.RunFailed || calls.Load() != 1 || !laterCalled.Load() {
		t.Fatalf("Run() = %#v, %v, calls=%d", result, err, calls.Load())
	}
	assertEventKinds(t, fixture.store, fixture.run.ID, []event.Kind{
		event.RunStarted, event.MessageStarted, event.MessageDelta, event.MessageCompleted, event.RunFailed,
	})
}

func TestRunnerExecutesToolLoop(t *testing.T) {
	t.Parallel()

	call := domain.ToolCall{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{"text":"hello"}`)}
	registry := tool.NewRegistry()
	if err := registry.Register(echoTool(nil)); err != nil {
		t.Fatalf("Register(): %v", err)
	}
	fixture := newFixture(t, [][]model.Chunk{
		{{Kind: model.ChunkToolCall, ToolCall: &call}, {Kind: model.ChunkDone, StopReason: model.StopToolUse}},
		{{Kind: model.ChunkTextDelta, Text: "done"}, {Kind: model.ChunkDone, StopReason: model.StopEndTurn}},
	}, registry)
	result, err := fixture.runner.Run(context.Background(), fixture.request)
	if err != nil {
		t.Fatalf("Run(): %v", err)
	}
	if result.Turns != 2 || len(result.Messages) != 4 {
		t.Fatalf("Result = %#v", result)
	}
	toolMessage := result.Messages[2]
	if toolMessage.Role != domain.RoleTool || toolMessage.Content[0].ToolResult.IsError {
		t.Fatalf("tool message = %#v", toolMessage)
	}
	if len(fixture.provider.Requests) != 2 || len(fixture.provider.Requests[1].Messages) != 3 {
		t.Fatalf("provider requests = %#v", fixture.provider.Requests)
	}
	assertEventKinds(t, fixture.store, fixture.run.ID, []event.Kind{
		event.RunStarted, event.MessageStarted, event.MessageCompleted,
		event.ToolStarted, event.ToolCompleted,
		event.MessageStarted, event.MessageDelta, event.MessageCompleted, event.RunCompleted,
	})
}

func TestRunnerRunsToolsInParallel(t *testing.T) {
	t.Parallel()

	var active atomic.Int32
	var maximum atomic.Int32
	registry := tool.NewRegistry()
	if err := registry.Register(echoTool(func() {
		current := active.Add(1)
		for {
			observed := maximum.Load()
			if current <= observed || maximum.CompareAndSwap(observed, current) {
				break
			}
		}
		time.Sleep(40 * time.Millisecond)
		active.Add(-1)
	})); err != nil {
		t.Fatalf("Register(): %v", err)
	}
	first := domain.ToolCall{ID: "1", Name: "echo", Arguments: json.RawMessage(`{"text":"one"}`)}
	second := domain.ToolCall{ID: "2", Name: "echo", Arguments: json.RawMessage(`{"text":"two"}`)}
	fixture := newFixture(t, [][]model.Chunk{
		{{Kind: model.ChunkToolCall, ToolCall: &first}, {Kind: model.ChunkToolCall, ToolCall: &second}, {Kind: model.ChunkDone, StopReason: model.StopToolUse}},
		{{Kind: model.ChunkTextDelta, Text: "done"}, {Kind: model.ChunkDone, StopReason: model.StopEndTurn}},
	}, registry)
	if _, err := fixture.runner.Run(context.Background(), fixture.request); err != nil {
		t.Fatalf("Run(): %v", err)
	}
	if maximum.Load() != 2 {
		t.Fatalf("maximum concurrent tools = %d, want 2", maximum.Load())
	}
}

func TestRunnerReturnsToolFailuresToModel(t *testing.T) {
	t.Parallel()

	call := domain.ToolCall{ID: "call-1", Name: "missing", Arguments: json.RawMessage(`{}`)}
	fixture := newFixture(t, [][]model.Chunk{
		{{Kind: model.ChunkToolCall, ToolCall: &call}, {Kind: model.ChunkDone, StopReason: model.StopToolUse}},
		{{Kind: model.ChunkTextDelta, Text: "recovered"}, {Kind: model.ChunkDone, StopReason: model.StopEndTurn}},
	}, nil)
	result, err := fixture.runner.Run(context.Background(), fixture.request)
	if err != nil {
		t.Fatalf("Run(): %v", err)
	}
	if !result.Messages[2].Content[0].ToolResult.IsError {
		t.Fatalf("tool result = %#v, want error", result.Messages[2])
	}
	assertContainsEvent(t, fixture.store, fixture.run.ID, event.ToolFailed)
}

func TestRunnerMiddlewareLifecycleAndDenial(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	registry := tool.NewRegistry()
	if err := registry.Register(echoTool(func() { calls.Add(1) })); err != nil {
		t.Fatalf("Register(): %v", err)
	}
	call := domain.ToolCall{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{"text":"x"}`)}
	recorder := &recordingMiddleware{denyTool: true}
	fixture := newFixtureWithMiddleware(t, [][]model.Chunk{
		{{Kind: model.ChunkToolCall, ToolCall: &call}, {Kind: model.ChunkDone, StopReason: model.StopToolUse}},
		{{Kind: model.ChunkTextDelta, Text: "done"}, {Kind: model.ChunkDone, StopReason: model.StopEndTurn}},
	}, registry, []Middleware{recorder})
	result, err := fixture.runner.Run(context.Background(), fixture.request)
	if err != nil {
		t.Fatalf("Run(): %v", err)
	}
	if calls.Load() != 0 || !result.Messages[2].Content[0].ToolResult.IsError {
		t.Fatalf("tool calls = %d, result = %#v", calls.Load(), result.Messages[2])
	}
	want := []string{"before_model", "after_model", "before_tool", "after_tool", "before_model", "after_model"}
	if got := recorder.events(); !equalStrings(got, want) {
		t.Fatalf("middleware events = %#v, want %#v", got, want)
	}
}

func TestRunnerPersistsToolOutcomeBeforeAfterMiddleware(t *testing.T) {
	t.Parallel()

	registry := tool.NewRegistry()
	if err := registry.Register(echoTool(nil)); err != nil {
		t.Fatalf("Register(): %v", err)
	}
	call := domain.ToolCall{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{"text":"x"}`)}
	fixture := newFixtureWithMiddleware(t, [][]model.Chunk{{
		{Kind: model.ChunkToolCall, ToolCall: &call},
		{Kind: model.ChunkDone, StopReason: model.StopToolUse},
	}}, registry, []Middleware{failingAfterToolMiddleware{}})
	if _, err := fixture.runner.Run(context.Background(), fixture.request); err == nil {
		t.Fatal("Run() error = nil")
	}
	assertRunStatus(t, fixture.store, fixture.run.ID, domain.RunFailed)
	assertContainsEvent(t, fixture.store, fixture.run.ID, event.ToolCompleted)
}

func TestRunnerObservesRawResultThenPersistsTransformation(t *testing.T) {
	t.Parallel()
	registry := tool.NewRegistry()
	if err := registry.Register(echoTool(nil)); err != nil {
		t.Fatal(err)
	}
	call := domain.ToolCall{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{"text":"raw"}`)}
	boundary := &resultBoundaryMiddleware{mutateObserved: true}
	fixture := newFixtureWithMiddleware(t, [][]model.Chunk{
		{{Kind: model.ChunkToolCall, ToolCall: &call}, {Kind: model.ChunkDone, StopReason: model.StopToolUse}},
		{{Kind: model.ChunkTextDelta, Text: "done"}, {Kind: model.ChunkDone, StopReason: model.StopEndTurn}},
	}, registry, []Middleware{boundary})
	result, err := fixture.runner.Run(context.Background(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(result.Messages[2].Content[0].ToolResult.Output); got != `{"budgeted":true}` {
		t.Fatalf("persisted output = %s", got)
	}
	if boundary.observed != `{"text":"raw"}` || boundary.transformInput != `{"text":"raw"}` || boundary.after != `{"budgeted":true}` {
		t.Fatalf("boundary = %#v", boundary)
	}
	if got := string(fixture.provider.Requests[1].Messages[2].Content[0].ToolResult.Output); got != `{"budgeted":true}` {
		t.Fatalf("model output = %s", got)
	}
}

func TestRunnerToolExecutionInterceptorsComposeAndShortCircuit(t *testing.T) {
	t.Parallel()
	events := make([]string, 0, 5)
	registry := tool.NewRegistry()
	if err := registry.Register(echoTool(func() { events = append(events, "tool") })); err != nil {
		t.Fatal(err)
	}
	outer := &executionInterceptor{name: "outer", events: &events}
	inner := &executionInterceptor{name: "inner", events: &events}
	runner := &Runner{tools: registry, middleware: []Middleware{outer, inner}}
	call := domain.ToolCall{ID: "call", Name: "echo", Arguments: json.RawMessage(`{"text":"hello"}`)}
	result, err := runner.executeTool(context.Background(), call)
	if err != nil || string(result.Output) != `{"text":"hello"}` {
		t.Fatalf("executeTool() = %#v, %v", result, err)
	}
	want := []string{"outer:before", "inner:before", "tool", "inner:after", "outer:after"}
	if !equalStrings(events, want) {
		t.Fatalf("events = %#v, want %#v", events, want)
	}

	events = nil
	outer.shortCircuit = true
	result, err = runner.executeTool(context.Background(), call)
	if err != nil || !result.IsError || string(result.Output) != `{"blocked":true}` {
		t.Fatalf("short circuit = %#v, %v", result, err)
	}
	if !equalStrings(events, []string{"outer:before"}) {
		t.Fatalf("short-circuit events = %#v", events)
	}
}

func TestRunnerRejectsInvalidToolInterceptorResults(t *testing.T) {
	t.Parallel()
	registry := tool.NewRegistry()
	if err := registry.Register(echoTool(nil)); err != nil {
		t.Fatal(err)
	}
	call := domain.ToolCall{ID: "call", Name: "echo", Arguments: json.RawMessage(`{"text":"hello"}`)}
	tests := []struct {
		name   string
		result domain.ToolResult
		err    error
		want   error
	}{
		{name: "identity", result: domain.ToolResult{CallID: "other", Output: json.RawMessage(`{}`)}, want: ErrProtocol},
		{name: "empty output", result: domain.ToolResult{CallID: "call"}, want: ErrProtocol},
		{name: "invalid output", result: domain.ToolResult{CallID: "call", Output: json.RawMessage(`no`)}, want: ErrProtocol},
		{name: "error", err: errInterceptorFailure, want: errInterceptorFailure},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			middleware := &fixedExecutionInterceptor{result: test.result, err: test.err}
			runner := &Runner{tools: registry, middleware: []Middleware{middleware}}
			_, err := runner.executeTool(context.Background(), call)
			if !errors.Is(err, test.want) {
				t.Fatalf("executeTool() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestRunnerModelResponseTransformersComposeAndPreserveUsage(t *testing.T) {
	t.Parallel()
	response := model.Response{Text: "base", Usage: model.Usage{InputTokens: 3, OutputTokens: 2}, StopReason: model.StopEndTurn}
	first := &responseBoundaryMiddleware{suffix: ":first"}
	second := &responseBoundaryMiddleware{suffix: ":second"}
	runner := &Runner{middleware: []Middleware{first, second}}
	got, err := runner.transformModelResponse(context.Background(), response)
	if err != nil || got.Text != "base:first:second" || got.Usage != response.Usage {
		t.Fatalf("transformModelResponse() = %#v, %v", got, err)
	}
	if response.Text != "base" {
		t.Fatalf("source response changed: %#v", response)
	}

	first.changeUsage = true
	if _, err = runner.transformModelResponse(context.Background(), response); !errors.Is(err, ErrProtocol) {
		t.Fatalf("usage mutation error = %v", err)
	}
	first.changeUsage = false
	first.err = errInterceptorFailure
	if _, err = runner.transformModelResponse(context.Background(), response); !errors.Is(err, errInterceptorFailure) {
		t.Fatalf("transformer error = %v", err)
	}
}

func TestRunnerFailsBeforeToolPersistenceOnResultBoundaryError(t *testing.T) {
	t.Parallel()
	registry := tool.NewRegistry()
	if err := registry.Register(echoTool(nil)); err != nil {
		t.Fatal(err)
	}
	call := domain.ToolCall{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{"text":"raw"}`)}
	tests := []struct{ stage, want string }{
		{stage: "observe", want: "observe result"},
		{stage: "transform", want: "transform result"},
		{stage: "invalid", want: "protocol violation"},
	}
	for _, test := range tests {
		t.Run(test.stage, func(t *testing.T) {
			boundary := &resultBoundaryMiddleware{fail: test.stage}
			fixture := newFixtureWithMiddleware(t, [][]model.Chunk{{
				{Kind: model.ChunkToolCall, ToolCall: &call}, {Kind: model.ChunkDone, StopReason: model.StopToolUse},
			}}, registry, []Middleware{boundary})
			if _, err := fixture.runner.Run(context.Background(), fixture.request); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Run() error = %v", err)
			}
			assertRunStatus(t, fixture.store, fixture.run.ID, domain.RunFailed)
			records, err := fixture.store.Events(context.Background(), fixture.run.ID, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, record := range records {
				if record.Kind == event.ToolCompleted {
					t.Fatalf("tool result persisted after %s failure", test.stage)
				}
			}
		})
	}
}

func TestNopMiddleware(t *testing.T) {
	t.Parallel()

	middleware := NopMiddleware{}
	ctx := context.Background()
	if err := middleware.BeforeModel(ctx, &model.Request{}); err != nil {
		t.Fatalf("BeforeModel(): %v", err)
	}
	if err := middleware.AfterModel(ctx, model.Response{}); err != nil {
		t.Fatalf("AfterModel(): %v", err)
	}
	if err := middleware.BeforeTool(ctx, domain.ToolCall{}); err != nil {
		t.Fatalf("BeforeTool(): %v", err)
	}
	if err := middleware.AfterTool(ctx, domain.ToolCall{}, domain.ToolResult{}); err != nil {
		t.Fatalf("AfterTool(): %v", err)
	}
}

func TestRunnerFailsAtTurnLimit(t *testing.T) {
	t.Parallel()

	call := domain.ToolCall{ID: "1", Name: "missing", Arguments: json.RawMessage(`{}`)}
	fixture := newFixture(t, [][]model.Chunk{{
		{Kind: model.ChunkToolCall, ToolCall: &call},
		{Kind: model.ChunkDone, StopReason: model.StopToolUse},
	}}, nil)
	fixture.runner.maxTurns = 1
	_, err := fixture.runner.Run(context.Background(), fixture.request)
	if !errors.Is(err, ErrTurnLimit) {
		t.Fatalf("Run() error = %v, want ErrTurnLimit", err)
	}
	assertRunStatus(t, fixture.store, fixture.run.ID, domain.RunFailed)
	assertContainsEvent(t, fixture.store, fixture.run.ID, event.RunFailed)
}

func TestRunnerPersistsProviderFailure(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t, nil, nil)
	fixture.provider.Err = errors.New("provider unavailable")
	_, err := fixture.runner.Run(context.Background(), fixture.request)
	if err == nil {
		t.Fatal("Run() error = nil")
	}
	assertRunStatus(t, fixture.store, fixture.run.ID, domain.RunFailed)
	assertContainsEvent(t, fixture.store, fixture.run.ID, event.RunFailed)
}

func TestRunnerPersistsCancellation(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	fixture.runner.provider = blockingProvider{ctx: ctx}
	done := make(chan error, 1)
	go func() {
		_, err := fixture.runner.Run(ctx, fixture.request)
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run() did not stop after cancellation")
	}
	assertRunStatus(t, fixture.store, fixture.run.ID, domain.RunCancelled)
	assertContainsEvent(t, fixture.store, fixture.run.ID, event.RunCancelled)
}

func TestRunnerRejectsProtocolFailures(t *testing.T) {
	t.Parallel()
	duplicate := domain.ToolCall{ID: "same", Name: "echo", Arguments: json.RawMessage(`{"text":"x"}`)}

	tests := []struct {
		name   string
		chunks []model.Chunk
		want   error
	}{
		{name: "truncated", chunks: []model.Chunk{{Kind: model.ChunkTextDelta, Text: "partial"}, {Kind: model.ChunkDone, StopReason: model.StopMaxTokens}}, want: ErrModelTruncated},
		{name: "tool reason without calls", chunks: []model.Chunk{{Kind: model.ChunkTextDelta, Text: "x"}, {Kind: model.ChunkDone, StopReason: model.StopToolUse}}, want: ErrProtocol},
		{name: "duplicate tool IDs", chunks: []model.Chunk{{Kind: model.ChunkToolCall, ToolCall: &duplicate}, {Kind: model.ChunkToolCall, ToolCall: &duplicate}, {Kind: model.ChunkDone, StopReason: model.StopToolUse}}, want: ErrProtocol},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newFixture(t, [][]model.Chunk{test.chunks}, nil)
			result, err := fixture.runner.Run(context.Background(), fixture.request)
			if !errors.Is(err, test.want) {
				t.Fatalf("Run() error = %v, want %v", err, test.want)
			}
			if result.Run.ID != fixture.run.ID || result.Turns != 1 {
				t.Fatalf("partial result = %#v", result)
			}
			assertRunStatus(t, fixture.store, fixture.run.ID, domain.RunFailed)
		})
	}
}

func TestRunnerResumesInterruptedRun(t *testing.T) {
	t.Parallel()

	fixture := newFixture(t, [][]model.Chunk{{
		{Kind: model.ChunkTextDelta, Text: "resumed"},
		{Kind: model.ChunkDone, StopReason: model.StopEndTurn},
	}}, nil)
	started, err := fixture.store.TransitionRun(context.Background(), fixture.run.ID, domain.RunPending, domain.RunRunning, time.Now(), "")
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	interrupted, err := fixture.store.TransitionRun(context.Background(), started.ID, domain.RunRunning, domain.RunInterrupted, time.Now(), "")
	if err != nil {
		t.Fatalf("interrupt run: %v", err)
	}
	fixture.run = interrupted
	result, err := fixture.runner.Run(context.Background(), fixture.request)
	if err != nil {
		t.Fatalf("Run(): %v", err)
	}
	if result.Run.Attempt != 2 {
		t.Fatalf("attempt = %d, want 2", result.Run.Attempt)
	}
}

func TestRunnerRejectsInvalidConstructionAndRunState(t *testing.T) {
	t.Parallel()

	if _, err := NewRunner(RunnerConfig{}); !errors.Is(err, ErrInvalidRunner) {
		t.Fatalf("NewRunner() error = %v, want ErrInvalidRunner", err)
	}
	memory := store.NewMemory()
	provider := &model.Scripted{}
	if _, err := NewRunner(RunnerConfig{Store: memory, Provider: provider, MaxTurns: -1}); !errors.Is(err, ErrInvalidRunner) {
		t.Fatalf("NewRunner(negative) error = %v, want ErrInvalidRunner", err)
	}
	if _, err := NewRunner(RunnerConfig{Store: memory, Provider: provider, FinishHooks: []FinishHook{nil}}); !errors.Is(err, ErrInvalidRunner) {
		t.Fatalf("NewRunner(nil finish hook) error = %v", err)
	}
	invalidCaller := newFixture(t, nil, nil)
	invalidCaller.request.Caller = "unknown"
	if _, err := invalidCaller.runner.Run(context.Background(), invalidCaller.request); !errors.Is(err, model.ErrInvalidRequest) {
		t.Fatalf("Run(invalid caller) error = %v", err)
	}

	fixture := newFixture(t, nil, nil)
	started, err := fixture.store.TransitionRun(context.Background(), fixture.run.ID, domain.RunPending, domain.RunRunning, time.Now(), "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	_, err = fixture.store.TransitionRun(context.Background(), started.ID, domain.RunRunning, domain.RunSucceeded, time.Now(), "")
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if _, err := fixture.runner.Run(context.Background(), fixture.request); !errors.Is(err, ErrNotRunnable) {
		t.Fatalf("Run(terminal) error = %v, want ErrNotRunnable", err)
	}
	fixture.request.RunID, _ = domain.NewRunID()
	if _, err := fixture.runner.Run(context.Background(), fixture.request); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Run(missing) error = %v, want ErrNotFound", err)
	}
}

type fixture struct {
	runner   *Runner
	store    *store.Memory
	provider *model.Scripted
	run      domain.Run
	request  Request
}

type usageMiddleware struct{ NopMiddleware }

func (usageMiddleware) BeforeModel(ctx context.Context, _ *model.Request) error {
	return RecordModelUsage(ctx, "compact", CallerMiddleware, model.Usage{InputTokens: 4, OutputTokens: 1})
}

func newFixture(t *testing.T, responses [][]model.Chunk, registry *tool.Registry) fixture {
	t.Helper()
	return newFixtureWithMiddleware(t, responses, registry, nil)
}

func newFixtureWithMiddleware(t *testing.T, responses [][]model.Chunk, registry *tool.Registry, middleware []Middleware) fixture {
	t.Helper()
	memory := store.NewMemory()
	now := time.Now().UTC()
	thread, err := domain.NewThread(now)
	if err != nil {
		t.Fatalf("NewThread(): %v", err)
	}
	if err := memory.CreateThread(context.Background(), thread); err != nil {
		t.Fatalf("CreateThread(): %v", err)
	}
	run, err := domain.NewRun(thread.ID, now)
	if err != nil {
		t.Fatalf("NewRun(): %v", err)
	}
	if err := memory.CreateRun(context.Background(), run); err != nil {
		t.Fatalf("CreateRun(): %v", err)
	}
	message, err := domain.NewTextMessage(domain.RoleUser, "hello", now)
	if err != nil {
		t.Fatalf("NewTextMessage(): %v", err)
	}
	provider := &model.Scripted{Responses: responses}
	runner, err := NewRunner(RunnerConfig{
		Store: memory, Provider: provider, Tools: registry, Middleware: middleware,
		MaxTurns: 4, MaxParallelTools: 2,
	})
	if err != nil {
		t.Fatalf("NewRunner(): %v", err)
	}
	return fixture{
		runner: runner, store: memory, provider: provider, run: run,
		request: Request{RunID: run.ID, Model: "test", Messages: []domain.Message{message}},
	}
}

func echoTool(before func()) tool.Tool {
	return tool.Func{
		DefinitionValue: tool.Definition{
			Name: "echo", Description: "Echo text",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`),
		},
		ExecuteFunc: func(_ context.Context, arguments json.RawMessage) (json.RawMessage, error) {
			if before != nil {
				before()
			}
			return append(json.RawMessage(nil), arguments...), nil
		},
	}
}

func assertEventKinds(t *testing.T, memory *store.Memory, runID domain.RunID, want []event.Kind) {
	t.Helper()
	records, err := memory.Events(context.Background(), runID, 0, 0)
	if err != nil {
		t.Fatalf("Events(): %v", err)
	}
	got := make([]event.Kind, len(records))
	for index, record := range records {
		got[index] = record.Kind
	}
	if len(got) != len(want) {
		t.Fatalf("event kinds = %#v, want %#v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("event kinds = %#v, want %#v", got, want)
		}
	}
}

func assertContainsEvent(t *testing.T, memory *store.Memory, runID domain.RunID, kind event.Kind) {
	t.Helper()
	records, err := memory.Events(context.Background(), runID, 0, 0)
	if err != nil {
		t.Fatalf("Events(): %v", err)
	}
	for _, record := range records {
		if record.Kind == kind {
			return
		}
	}
	t.Fatalf("events do not contain %s: %#v", kind, records)
}

func assertRunStatus(t *testing.T, memory *store.Memory, runID domain.RunID, want domain.RunStatus) {
	t.Helper()
	run, err := memory.Run(context.Background(), runID)
	if err != nil {
		t.Fatalf("Run(): %v", err)
	}
	if run.Status != want {
		t.Fatalf("run status = %s, want %s", run.Status, want)
	}
}

type recordingMiddleware struct {
	NopMiddleware
	mu       sync.Mutex
	log      []string
	denyTool bool
}

type failingAfterToolMiddleware struct{ NopMiddleware }

func (failingAfterToolMiddleware) AfterTool(context.Context, domain.ToolCall, domain.ToolResult) error {
	return errors.New("after tool failed")
}

type resultBoundaryMiddleware struct {
	NopMiddleware
	observed       string
	transformInput string
	after          string
	fail           string
	mutateObserved bool
}

type executionInterceptor struct {
	NopMiddleware
	name         string
	events       *[]string
	shortCircuit bool
}

func (interceptor *executionInterceptor) ExecuteTool(ctx context.Context, call domain.ToolCall, next ToolExecutor) (domain.ToolResult, error) {
	*interceptor.events = append(*interceptor.events, interceptor.name+":before")
	if interceptor.shortCircuit {
		return domain.ToolResult{CallID: call.ID, Output: json.RawMessage(`{"blocked":true}`), IsError: true}, nil
	}
	result, err := next(ctx, call)
	*interceptor.events = append(*interceptor.events, interceptor.name+":after")
	return result, err
}

var errInterceptorFailure = errors.New("interceptor failure marker")

type fixedExecutionInterceptor struct {
	NopMiddleware
	result domain.ToolResult
	err    error
}

type responseBoundaryMiddleware struct {
	NopMiddleware
	suffix      string
	changeUsage bool
	err         error
}

func (middleware *responseBoundaryMiddleware) TransformModelResponse(_ context.Context, response model.Response) (model.Response, error) {
	if middleware.err != nil {
		return model.Response{}, middleware.err
	}
	response.Text += middleware.suffix
	if middleware.changeUsage {
		response.Usage.InputTokens++
	}
	return response, nil
}

func (interceptor *fixedExecutionInterceptor) ExecuteTool(context.Context, domain.ToolCall, ToolExecutor) (domain.ToolResult, error) {
	return interceptor.result, interceptor.err
}

func (middleware *resultBoundaryMiddleware) ObserveToolResult(_ context.Context, _ domain.ToolCall, result domain.ToolResult) error {
	if middleware.fail == "observe" {
		return errors.New("observe result failed")
	}
	middleware.observed = string(result.Output)
	if middleware.mutateObserved && len(result.Output) > 0 {
		result.Output[0] = 'x'
	}
	return nil
}

func (middleware *resultBoundaryMiddleware) TransformToolResult(_ context.Context, _ domain.ToolCall, result domain.ToolResult) (domain.ToolResult, error) {
	middleware.transformInput = string(result.Output)
	if middleware.fail == "transform" {
		return domain.ToolResult{}, errors.New("transform result failed")
	}
	if middleware.fail == "invalid" {
		result.CallID = "different"
		return result, nil
	}
	result.Output = json.RawMessage(`{"budgeted":true}`)
	return result, nil
}

func (middleware *resultBoundaryMiddleware) AfterTool(_ context.Context, _ domain.ToolCall, result domain.ToolResult) error {
	middleware.after = string(result.Output)
	return nil
}

func (middleware *recordingMiddleware) BeforeModel(context.Context, *model.Request) error {
	middleware.record("before_model")
	return nil
}

func (middleware *recordingMiddleware) AfterModel(context.Context, model.Response) error {
	middleware.record("after_model")
	return nil
}

func (middleware *recordingMiddleware) BeforeTool(context.Context, domain.ToolCall) error {
	middleware.record("before_tool")
	if middleware.denyTool {
		return errors.New("denied")
	}
	return nil
}

func (middleware *recordingMiddleware) AfterTool(context.Context, domain.ToolCall, domain.ToolResult) error {
	middleware.record("after_tool")
	return nil
}

func (middleware *recordingMiddleware) record(value string) {
	middleware.mu.Lock()
	middleware.log = append(middleware.log, value)
	middleware.mu.Unlock()
}

func (middleware *recordingMiddleware) events() []string {
	middleware.mu.Lock()
	defer middleware.mu.Unlock()
	return append([]string(nil), middleware.log...)
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

type blockingProvider struct{ ctx context.Context }

func (provider blockingProvider) Stream(context.Context, model.Request) (model.Stream, error) {
	return &blockingStream{ctx: provider.ctx}, nil
}

type blockingStream struct{ ctx context.Context }

func (stream *blockingStream) Recv() (model.Chunk, error) {
	<-stream.ctx.Done()
	return model.Chunk{}, stream.ctx.Err()
}

func (*blockingStream) Close() error { return nil }

var _ io.Closer = (*blockingStream)(nil)
