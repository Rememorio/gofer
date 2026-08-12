package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/event"
	"github.com/Rememorio/gofer/internal/model"
	"github.com/Rememorio/gofer/internal/store"
	"github.com/Rememorio/gofer/internal/tool"
)

const (
	defaultMaxTurns         = 100
	defaultMaxParallelTools = 8
)

var (
	// ErrInvalidRunner identifies missing or invalid runtime dependencies.
	ErrInvalidRunner = errors.New("invalid runner")
	// ErrNotRunnable identifies a run that cannot be started or resumed.
	ErrNotRunnable = errors.New("run is not runnable")
	// ErrTurnLimit identifies a run that exhausted its configured model turns.
	ErrTurnLimit = errors.New("model turn limit reached")
	// ErrModelTruncated identifies a response stopped by a model limit or filter.
	ErrModelTruncated = errors.New("model response was truncated")
	// ErrProtocol identifies inconsistent normalized model output.
	ErrProtocol = errors.New("model protocol violation")
)

// RunnerConfig configures a durable agent runner.
type RunnerConfig struct {
	Store            store.Store
	Provider         model.Provider
	Tools            *tool.Registry
	Middleware       []Middleware
	MaxTurns         int
	MaxParallelTools int
	Now              func() time.Time
}

// Runner coordinates one durable agent execution at a time.
type Runner struct {
	store            store.Store
	provider         model.Provider
	tools            *tool.Registry
	middleware       []Middleware
	maxTurns         int
	maxParallelTools int
	now              func() time.Time
}

// Request supplies provider and conversation state for a run.
type Request struct {
	RunID       domain.RunID
	Model       string
	System      string
	Messages    []domain.Message
	MaxTokens   int
	Temperature *float64
}

// Result is the completed durable run and normalized conversation state.
type Result struct {
	Run      domain.Run
	Messages []domain.Message
	Usage    model.Usage
	Turns    int
}

// NewRunner validates config and constructs a runner.
func NewRunner(config RunnerConfig) (*Runner, error) {
	if config.Store == nil || config.Provider == nil {
		return nil, fmt.Errorf("%w: store and provider are required", ErrInvalidRunner)
	}
	if config.Tools == nil {
		config.Tools = tool.NewRegistry()
	}
	if config.MaxTurns == 0 {
		config.MaxTurns = defaultMaxTurns
	}
	if config.MaxParallelTools == 0 {
		config.MaxParallelTools = defaultMaxParallelTools
	}
	if config.MaxTurns < 0 || config.MaxParallelTools < 0 {
		return nil, fmt.Errorf("%w: execution limits must be positive", ErrInvalidRunner)
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Runner{
		store:            config.Store,
		provider:         config.Provider,
		tools:            config.Tools,
		middleware:       append([]Middleware(nil), config.Middleware...),
		maxTurns:         config.MaxTurns,
		maxParallelTools: config.MaxParallelTools,
		now:              config.Now,
	}, nil
}

// Run starts or resumes request.RunID and drives it to a terminal state.
func (runner *Runner) Run(ctx context.Context, request Request) (Result, error) {
	execution, err := runner.newExecution(ctx, request)
	if err != nil {
		return Result{}, err
	}
	result, err := execution.run(ctx)
	if err == nil {
		return result, nil
	}
	return Result{}, execution.finishError(ctx, err)
}

type execution struct {
	runner   *Runner
	request  Request
	state    domain.Run
	messages []domain.Message
	journal  journal
	usage    model.Usage
	turns    int
}

type journal struct {
	store    store.Store
	runID    domain.RunID
	threadID domain.ThreadID
	sequence uint64
	now      func() time.Time
}

type toolOutcome struct {
	call   domain.ToolCall
	result domain.ToolResult
	err    error
	skip   bool
}

func (runner *Runner) newExecution(ctx context.Context, request Request) (*execution, error) {
	run, err := runner.store.Run(ctx, request.RunID)
	if err != nil {
		return nil, err
	}
	if run.Status != domain.RunPending && run.Status != domain.RunInterrupted {
		return nil, fmt.Errorf("%w: status is %s", ErrNotRunnable, run.Status)
	}
	if len(request.Messages) == 0 {
		return nil, fmt.Errorf("%w: messages are required", model.ErrInvalidRequest)
	}
	records, err := runner.store.Events(ctx, run.ID, 0, 0)
	if err != nil {
		return nil, err
	}
	sequence := uint64(0)
	if len(records) > 0 {
		sequence = records[len(records)-1].Sequence
	}
	return &execution{
		runner:   runner,
		request:  request,
		state:    run,
		messages: append([]domain.Message(nil), request.Messages...),
		journal: journal{
			store: runner.store, runID: run.ID, threadID: run.ThreadID,
			sequence: sequence, now: runner.now,
		},
	}, nil
}

func (execution *execution) run(ctx context.Context) (Result, error) {
	if err := execution.start(ctx); err != nil {
		return Result{}, err
	}
	for execution.turns < execution.runner.maxTurns {
		execution.turns++
		response, message, err := execution.modelTurn(ctx)
		if err != nil {
			return Result{}, err
		}
		execution.messages = append(execution.messages, message)
		if err := validateResponse(response); err != nil {
			return Result{}, err
		}
		if len(response.ToolCalls) == 0 {
			return execution.complete(ctx)
		}
		results, err := execution.toolTurn(ctx, response.ToolCalls)
		if err != nil {
			return Result{}, err
		}
		execution.messages = append(execution.messages, results...)
	}
	return Result{}, ErrTurnLimit
}

func (execution *execution) start(ctx context.Context) error {
	expected := execution.state.Status
	run, err := execution.runner.store.TransitionRun(
		ctx, execution.state.ID, expected, domain.RunRunning, execution.runner.now(), "",
	)
	if err != nil {
		return err
	}
	execution.state = run
	return execution.journal.append(ctx, event.RunStarted, map[string]any{
		"attempt": run.Attempt,
		"resumed": expected == domain.RunInterrupted,
	})
}

func (execution *execution) modelTurn(ctx context.Context) (model.Response, domain.Message, error) {
	request := model.Request{
		Model:       execution.request.Model,
		System:      execution.request.System,
		Messages:    append([]domain.Message(nil), execution.messages...),
		Tools:       execution.runner.tools.Definitions(),
		MaxTokens:   execution.request.MaxTokens,
		Temperature: execution.request.Temperature,
	}
	if err := execution.runner.beforeModel(ctx, &request); err != nil {
		return model.Response{}, domain.Message{}, err
	}
	if err := request.Validate(); err != nil {
		return model.Response{}, domain.Message{}, err
	}
	stream, err := execution.runner.provider.Stream(ctx, request)
	if err != nil {
		return model.Response{}, domain.Message{}, fmt.Errorf("open model stream: %w", err)
	}
	if err := execution.journal.append(ctx, event.MessageStarted, map[string]int{"turn": execution.turns}); err != nil {
		_ = stream.Close()
		return model.Response{}, domain.Message{}, err
	}
	response, err := execution.collect(ctx, stream)
	if err != nil {
		return model.Response{}, domain.Message{}, err
	}
	message, err := assistantMessage(response, execution.runner.now())
	if err != nil {
		return model.Response{}, domain.Message{}, err
	}
	if err := execution.journal.append(ctx, event.MessageCompleted, map[string]any{
		"message": message, "usage": response.Usage, "stop_reason": response.StopReason,
	}); err != nil {
		return model.Response{}, domain.Message{}, err
	}
	if err := execution.runner.afterModel(ctx, response); err != nil {
		return model.Response{}, domain.Message{}, err
	}
	addUsage(&execution.usage, response.Usage)
	return response, message, nil
}

func (execution *execution) collect(ctx context.Context, stream model.Stream) (model.Response, error) {
	response, collectErr := model.Collect(stream, func(chunk model.Chunk) error {
		if chunk.Kind != model.ChunkTextDelta {
			return nil
		}
		return execution.journal.append(ctx, event.MessageDelta, map[string]string{"text": chunk.Text})
	})
	closeErr := stream.Close()
	if collectErr != nil || closeErr != nil {
		return model.Response{}, errors.Join(collectErr, closeErr)
	}
	return response, nil
}

func (execution *execution) toolTurn(ctx context.Context, calls []domain.ToolCall) ([]domain.Message, error) {
	outcomes := make([]toolOutcome, len(calls))
	for index, call := range calls {
		outcomes[index] = toolOutcome{call: call}
		if err := execution.runner.beforeTool(ctx, call); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			outcomes[index].result = toolErrorResult(call.ID, err)
			outcomes[index].skip = true
		}
		if err := execution.journal.append(ctx, event.ToolStarted, map[string]any{"call": call}); err != nil {
			return nil, err
		}
	}
	execution.executeTools(ctx, outcomes)
	return execution.persistToolOutcomes(ctx, outcomes)
}

func (execution *execution) executeTools(ctx context.Context, outcomes []toolOutcome) {
	semaphore := make(chan struct{}, execution.runner.maxParallelTools)
	var wait sync.WaitGroup
	for index := range outcomes {
		if outcomes[index].skip {
			continue
		}
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				outcomes[index].err = ctx.Err()
				return
			}
			outcomes[index].result, outcomes[index].err = execution.runner.tools.Execute(ctx, outcomes[index].call)
		}(index)
	}
	wait.Wait()
}

func (execution *execution) persistToolOutcomes(ctx context.Context, outcomes []toolOutcome) ([]domain.Message, error) {
	messages := make([]domain.Message, 0, len(outcomes))
	for _, outcome := range outcomes {
		if outcome.err != nil {
			return nil, outcome.err
		}
		kind := event.ToolCompleted
		if outcome.result.IsError {
			kind = event.ToolFailed
		}
		message, err := toolResultMessage(outcome.result, execution.runner.now())
		if err != nil {
			return nil, err
		}
		if err := execution.journal.append(ctx, kind, map[string]any{
			"call": outcome.call, "result": outcome.result, "message": message,
		}); err != nil {
			return nil, err
		}
		if err := execution.runner.afterTool(ctx, outcome.call, outcome.result); err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, nil
}

func (execution *execution) complete(ctx context.Context) (Result, error) {
	run, err := execution.runner.store.TransitionRun(
		ctx, execution.state.ID, domain.RunRunning, domain.RunSucceeded, execution.runner.now(), "",
	)
	if err != nil {
		return Result{}, err
	}
	execution.state = run
	if err := execution.journal.append(ctx, event.RunCompleted, map[string]any{
		"turns": execution.turns, "usage": execution.usage,
	}); err != nil {
		return Result{}, err
	}
	return Result{
		Run: run, Messages: append([]domain.Message(nil), execution.messages...),
		Usage: execution.usage, Turns: execution.turns,
	}, nil
}

func (execution *execution) finishError(ctx context.Context, cause error) error {
	if execution.state.Status != domain.RunRunning {
		return cause
	}
	background, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	status := domain.RunFailed
	kind := event.RunFailed
	failure := cause.Error()
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		status = domain.RunCancelled
		kind = event.RunCancelled
		failure = ""
	}
	run, transitionErr := execution.runner.store.TransitionRun(
		background, execution.state.ID, domain.RunRunning, status, execution.runner.now(), failure,
	)
	if transitionErr == nil {
		execution.state = run
	}
	appendErr := execution.journal.append(background, kind, map[string]string{"error": cause.Error()})
	return errors.Join(cause, transitionErr, appendErr)
}

func (journal *journal) append(ctx context.Context, kind event.Kind, payload any) error {
	draft, err := event.NewDraft(journal.threadID, journal.runID, kind, journal.now(), payload)
	if err != nil {
		return err
	}
	records, err := journal.store.Append(ctx, journal.runID, journal.sequence, draft)
	if err != nil {
		return err
	}
	journal.sequence = records[len(records)-1].Sequence
	return nil
}

func (runner *Runner) beforeModel(ctx context.Context, request *model.Request) error {
	for _, middleware := range runner.middleware {
		if err := middleware.BeforeModel(ctx, request); err != nil {
			return fmt.Errorf("before model: %w", err)
		}
	}
	return nil
}

func (runner *Runner) afterModel(ctx context.Context, response model.Response) error {
	for _, middleware := range runner.middleware {
		if err := middleware.AfterModel(ctx, response); err != nil {
			return fmt.Errorf("after model: %w", err)
		}
	}
	return nil
}

func (runner *Runner) beforeTool(ctx context.Context, call domain.ToolCall) error {
	for _, middleware := range runner.middleware {
		if err := middleware.BeforeTool(ctx, call); err != nil {
			return fmt.Errorf("before tool %s: %w", call.Name, err)
		}
	}
	return nil
}

func (runner *Runner) afterTool(ctx context.Context, call domain.ToolCall, result domain.ToolResult) error {
	for _, middleware := range runner.middleware {
		if err := middleware.AfterTool(ctx, call, result); err != nil {
			return fmt.Errorf("after tool %s: %w", call.Name, err)
		}
	}
	return nil
}

func assistantMessage(response model.Response, at time.Time) (domain.Message, error) {
	id, err := domain.NewMessageID()
	if err != nil {
		return domain.Message{}, err
	}
	contents := make([]domain.Content, 0, len(response.ToolCalls)+1)
	if response.Text != "" {
		contents = append(contents, domain.Content{Kind: domain.ContentText, Text: response.Text})
	}
	for index := range response.ToolCalls {
		call := response.ToolCalls[index]
		contents = append(contents, domain.Content{Kind: domain.ContentToolCall, ToolCall: &call})
	}
	message := domain.Message{ID: id, Role: domain.RoleAssistant, Content: contents, CreatedAt: at}
	if err := message.Validate(); err != nil {
		return domain.Message{}, fmt.Errorf("build assistant message: %w", err)
	}
	return message, nil
}

func toolResultMessage(result domain.ToolResult, at time.Time) (domain.Message, error) {
	id, err := domain.NewMessageID()
	if err != nil {
		return domain.Message{}, err
	}
	copyResult := result
	message := domain.Message{
		ID: id, Role: domain.RoleTool, CreatedAt: at,
		Content: []domain.Content{{Kind: domain.ContentToolResult, ToolResult: &copyResult}},
	}
	if err := message.Validate(); err != nil {
		return domain.Message{}, fmt.Errorf("build tool result message: %w", err)
	}
	return message, nil
}

func validateResponse(response model.Response) error {
	if response.StopReason == model.StopMaxTokens || response.StopReason == model.StopContentFilter {
		return fmt.Errorf("%w: %s", ErrModelTruncated, response.StopReason)
	}
	hasTools := len(response.ToolCalls) > 0
	if hasTools != (response.StopReason == model.StopToolUse) {
		return fmt.Errorf("%w: stop reason %s with %d tool calls", ErrProtocol, response.StopReason, len(response.ToolCalls))
	}
	return nil
}

func toolErrorResult(callID string, cause error) domain.ToolResult {
	data, _ := json.Marshal(map[string]string{"error": cause.Error()})
	return domain.ToolResult{CallID: callID, Output: data, IsError: true}
}

func addUsage(usage *model.Usage, next model.Usage) {
	usage.InputTokens += next.InputTokens
	usage.OutputTokens += next.OutputTokens
	usage.ReasoningTokens += next.ReasoningTokens
	usage.CacheReadTokens += next.CacheReadTokens
	usage.CacheWriteTokens += next.CacheWriteTokens
}
