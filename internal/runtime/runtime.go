package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	// CallerLeadAgent identifies primary conversation model calls.
	CallerLeadAgent = "lead_agent"
	// CallerSubagent identifies delegated child model calls.
	CallerSubagent = "subagent"
	// CallerMiddleware identifies auxiliary model calls.
	CallerMiddleware = "middleware"
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
	// ErrRetryModelResponse asks the runner to discard the current empty model
	// message and spend another bounded model turn. Response-transforming
	// middleware may return this sentinel before persistence.
	ErrRetryModelResponse = errors.New("retry model response")
	// ErrTerminalResponse identifies a provider that produced no visible final
	// response after bounded recovery.
	ErrTerminalResponse = errors.New("model returned no final response after one automatic retry")
)

// RunnerConfig configures a durable agent runner.
type RunnerConfig struct {
	Store            store.Store
	Provider         model.Provider
	Tools            *tool.Registry
	Middleware       []Middleware
	FinishHooks      []FinishHook
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
	finishHooks      []FinishHook
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
	Caller      string
}

// Result is the completed durable run and normalized conversation state.
type Result struct {
	Run        domain.Run
	Messages   []domain.Message
	Usage      model.Usage
	Turns      int
	StopReason model.StopReason
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
	for _, hook := range config.FinishHooks {
		if hook == nil {
			return nil, fmt.Errorf("%w: finish hook is nil", ErrInvalidRunner)
		}
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Runner{
		store:            config.Store,
		provider:         config.Provider,
		tools:            config.Tools,
		middleware:       append([]Middleware(nil), config.Middleware...),
		finishHooks:      append([]FinishHook(nil), config.FinishHooks...),
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
	err = execution.finishError(ctx, err)
	return Result{
		Run: execution.state, Messages: append([]domain.Message(nil), execution.messages...),
		Usage: execution.usage, Turns: execution.turns, StopReason: execution.stopReason,
	}, err
}

type execution struct {
	runner     *Runner
	request    Request
	state      domain.Run
	messages   []domain.Message
	journal    journal
	usage      model.Usage
	turns      int
	stopReason model.StopReason
	finalized  bool
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

type usageRecorderKey struct{}

type modelTurnsRemainingKey struct{}

type usageRecorder func(context.Context, string, string, model.Usage) error

// RecordModelUsage durably attributes an auxiliary model call when invoked
// inside a runtime middleware hook. Outside an active run it is a no-op.
func RecordModelUsage(ctx context.Context, modelName, caller string, usage model.Usage) error {
	if ctx == nil {
		return fmt.Errorf("%w: context is required", model.ErrInvalidRequest)
	}
	record, ok := ctx.Value(usageRecorderKey{}).(usageRecorder)
	if !ok {
		return nil
	}
	return record(ctx, modelName, caller, usage)
}

// RemainingModelTurns returns the number of provider calls still available
// after the active call. It is available only inside runtime middleware hooks.
func RemainingModelTurns(ctx context.Context) (int, bool) {
	if ctx == nil {
		return 0, false
	}
	remaining, ok := ctx.Value(modelTurnsRemainingKey{}).(int)
	return remaining, ok
}

func (runner *Runner) newExecution(ctx context.Context, request Request) (*execution, error) {
	request.Caller = strings.TrimSpace(request.Caller)
	if request.Caller == "" {
		request.Caller = CallerLeadAgent
	}
	if request.Caller != CallerLeadAgent && request.Caller != CallerSubagent && request.Caller != CallerMiddleware {
		return nil, fmt.Errorf("%w: unsupported caller", model.ErrInvalidRequest)
	}
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
		if errors.Is(err, ErrRetryModelResponse) {
			continue
		}
		if err != nil {
			return Result{}, err
		}
		execution.messages = append(execution.messages, message)
		if len(response.ToolCalls) == 0 {
			execution.stopReason = response.StopReason
			if response.StopReason == model.StopTerminalError {
				return Result{}, ErrTerminalResponse
			}
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
	ctx = context.WithValue(ctx, usageRecorderKey{}, usageRecorder(execution.recordModelUsage))
	ctx = context.WithValue(ctx, modelTurnsRemainingKey{}, execution.runner.maxTurns-execution.turns)
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
	response, err = execution.runner.transformModelResponse(ctx, response)
	if err != nil {
		if errors.Is(err, ErrRetryModelResponse) {
			if retryErr := execution.recordModelRetry(ctx, response); retryErr != nil {
				return model.Response{}, domain.Message{}, retryErr
			}
			return response, domain.Message{}, err
		}
		return model.Response{}, domain.Message{}, err
	}
	if err := validateResponse(response); err != nil {
		return model.Response{}, domain.Message{}, err
	}
	message, err := assistantMessage(response, execution.runner.now())
	if err != nil {
		return model.Response{}, domain.Message{}, err
	}
	if err := execution.journal.append(ctx, event.MessageCompleted, map[string]any{
		"message": message, "usage": response.Usage, "stop_reason": response.StopReason,
		"model": execution.request.Model, "caller": execution.request.Caller,
	}); err != nil {
		return model.Response{}, domain.Message{}, err
	}
	if err := execution.runner.afterModel(ctx, response); err != nil {
		return model.Response{}, domain.Message{}, err
	}
	addUsage(&execution.usage, response.Usage)
	return response, message, nil
}

func (execution *execution) recordModelUsage(ctx context.Context, modelName, caller string, next model.Usage) error {
	modelName = strings.TrimSpace(modelName)
	caller = strings.TrimSpace(caller)
	if modelName == "" || caller != CallerLeadAgent && caller != CallerSubagent && caller != CallerMiddleware ||
		next.InputTokens < 0 || next.OutputTokens < 0 || next.ReasoningTokens < 0 || next.CacheReadTokens < 0 || next.CacheWriteTokens < 0 {
		return fmt.Errorf("%w: invalid model usage", model.ErrInvalidRequest)
	}
	if err := execution.journal.append(ctx, event.ModelUsage, map[string]any{"model": modelName, "caller": caller, "usage": next}); err != nil {
		return err
	}
	addUsage(&execution.usage, next)
	return nil
}

func (execution *execution) recordModelRetry(ctx context.Context, response model.Response) error {
	if err := execution.journal.append(ctx, event.ModelRetry, map[string]any{
		"turn": execution.turns, "reason": "model_response_recovery",
		"model": execution.request.Model, "caller": execution.request.Caller, "usage": response.Usage,
	}); err != nil {
		return err
	}
	addUsage(&execution.usage, response.Usage)
	return nil
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
			outcomes[index].result, outcomes[index].err = execution.runner.executeTool(ctx, outcomes[index].call)
		}(index)
	}
	wait.Wait()
}

func (runner *Runner) executeTool(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
	execute := ToolExecutor(runner.tools.Execute)
	for index := len(runner.middleware) - 1; index >= 0; index-- {
		interceptor, ok := runner.middleware[index].(ToolExecutionInterceptor)
		if !ok {
			continue
		}
		next := execute
		execute = func(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
			return interceptor.ExecuteTool(ctx, call, next)
		}
	}
	result, err := execute(ctx, call)
	if err != nil {
		return domain.ToolResult{}, err
	}
	if result.CallID != call.ID || len(result.Output) == 0 || !json.Valid(result.Output) {
		return domain.ToolResult{}, fmt.Errorf("execute tool %s: %w: interceptor changed identity or JSON validity", call.Name, ErrProtocol)
	}
	return result, nil
}

func (execution *execution) persistToolOutcomes(ctx context.Context, outcomes []toolOutcome) ([]domain.Message, error) {
	messages := make([]domain.Message, 0, len(outcomes))
	for _, outcome := range outcomes {
		if outcome.err != nil {
			return nil, outcome.err
		}
		if err := execution.runner.observeToolResult(ctx, outcome.call, outcome.result); err != nil {
			return nil, err
		}
		result, err := execution.runner.transformToolResult(ctx, outcome.call, outcome.result)
		if err != nil {
			return nil, err
		}
		kind := event.ToolCompleted
		if result.IsError {
			kind = event.ToolFailed
		}
		message, err := toolResultMessage(result, execution.runner.now())
		if err != nil {
			return nil, err
		}
		if err := execution.journal.append(ctx, kind, map[string]any{
			"call": outcome.call, "result": result, "message": message,
		}); err != nil {
			return nil, err
		}
		if err := execution.runner.afterTool(ctx, outcome.call, result); err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, nil
}

func (runner *Runner) observeToolResult(ctx context.Context, call domain.ToolCall, result domain.ToolResult) error {
	for _, middleware := range runner.middleware {
		observer, ok := middleware.(ToolResultObserver)
		if !ok {
			continue
		}
		observed := result
		observed.Output = append(json.RawMessage(nil), result.Output...)
		if err := observer.ObserveToolResult(ctx, call, observed); err != nil {
			return fmt.Errorf("observe tool %s result: %w", call.Name, err)
		}
	}
	return nil
}

func (runner *Runner) transformToolResult(ctx context.Context, call domain.ToolCall, result domain.ToolResult) (domain.ToolResult, error) {
	for _, middleware := range runner.middleware {
		transformer, ok := middleware.(ToolResultTransformer)
		if !ok {
			continue
		}
		transformed, err := transformer.TransformToolResult(ctx, call, result)
		if err != nil {
			return domain.ToolResult{}, fmt.Errorf("transform tool %s result: %w", call.Name, err)
		}
		if transformed.CallID != result.CallID || transformed.IsError != result.IsError ||
			len(transformed.Output) == 0 || !json.Valid(transformed.Output) {
			return domain.ToolResult{}, fmt.Errorf("transform tool %s result: %w: transformer changed identity, status, or JSON validity", call.Name, ErrProtocol)
		}
		result = transformed
	}
	return result, nil
}

func (runner *Runner) transformModelResponse(ctx context.Context, response model.Response) (model.Response, error) {
	for _, middleware := range runner.middleware {
		transformer, ok := middleware.(ModelResponseTransformer)
		if !ok {
			continue
		}
		transformed, err := transformer.TransformModelResponse(ctx, response)
		if err != nil && !errors.Is(err, ErrRetryModelResponse) {
			return model.Response{}, fmt.Errorf("transform model response: %w", err)
		}
		if transformed.Usage != response.Usage {
			return model.Response{}, fmt.Errorf("transform model response: %w: transformer changed usage", ErrProtocol)
		}
		response = transformed
		if err != nil {
			return response, fmt.Errorf("transform model response: %w", err)
		}
	}
	return response, nil
}

func (execution *execution) complete(ctx context.Context) (Result, error) {
	if err := execution.finalize(ctx); err != nil {
		return Result{}, err
	}
	run, err := execution.runner.store.TransitionRun(
		ctx, execution.state.ID, domain.RunRunning, domain.RunSucceeded, execution.runner.now(), "",
	)
	if err != nil {
		return Result{}, err
	}
	execution.state = run
	if err := execution.journal.append(ctx, event.RunCompleted, map[string]any{
		"turns": execution.turns, "usage": execution.usage,
		"model": execution.request.Model, "caller": execution.request.Caller,
		"stop_reason": execution.stopReason,
	}); err != nil {
		return Result{}, err
	}
	return Result{
		Run: run, Messages: append([]domain.Message(nil), execution.messages...),
		Usage: execution.usage, Turns: execution.turns, StopReason: execution.stopReason,
	}, nil
}

func (execution *execution) finishError(ctx context.Context, cause error) error {
	if execution.state.Status != domain.RunRunning {
		return cause
	}
	background, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	finalizeErr := execution.finalize(background)
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
	appendErr := execution.journal.append(background, kind, map[string]any{
		"error": cause.Error(), "stop_reason": execution.stopReason,
	})
	return errors.Join(cause, finalizeErr, transitionErr, appendErr)
}

func (execution *execution) finalize(ctx context.Context) error {
	if execution.finalized {
		return nil
	}
	execution.finalized = true
	writer := eventWriterFunc(execution.journal.append)
	var finishErr error
	for _, hook := range execution.runner.finishHooks {
		if hook == nil {
			finishErr = errors.Join(finishErr, fmt.Errorf("%w: finish hook is nil", ErrInvalidRunner))
			continue
		}
		if err := hook.Finish(ctx, writer); err != nil {
			finishErr = errors.Join(finishErr, fmt.Errorf("finish run: %w", err))
		}
	}
	return finishErr
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
	if response.StopReason == model.StopTerminalError {
		message.Metadata = map[string]string{
			"internal_kind": "terminal_response_fallback", "error_reason": ErrTerminalResponse.Error(),
		}
	}
	if response.StopReason == model.StopSafetyCapped {
		message.Metadata = map[string]string{"internal_kind": "safety_termination"}
	}
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
	callIDs := make(map[string]struct{}, len(response.ToolCalls))
	for _, call := range response.ToolCalls {
		if strings.TrimSpace(call.ID) == "" || strings.TrimSpace(call.Name) == "" || len(call.Arguments) == 0 || !json.Valid(call.Arguments) {
			return fmt.Errorf("%w: invalid transformed tool call", ErrProtocol)
		}
		if _, duplicate := callIDs[call.ID]; duplicate {
			return fmt.Errorf("%w: duplicate transformed tool call ID %q", ErrProtocol, call.ID)
		}
		callIDs[call.ID] = struct{}{}
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
