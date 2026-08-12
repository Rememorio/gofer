package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/event"
	"github.com/Rememorio/gofer/internal/store"
	"github.com/Rememorio/gofer/internal/usage"
	"github.com/Rememorio/gofer/internal/workspacechange"
)

const maxRequestBytes = 1 << 20

// ErrInvalidConfig identifies missing gateway dependencies or invalid limits.
var ErrInvalidConfig = errors.New("invalid gateway configuration")

// StartRequest is the normalized durable-run launch envelope.
type StartRequest struct {
	RunID    domain.RunID    `json:"run_id"`
	ThreadID domain.ThreadID `json:"thread_id"`
	Request  RunRequest      `json:"request"`
}

// RunRequest is the supported LangGraph/DeerFlow launch envelope.
type RunRequest struct {
	AssistantID       string          `json:"assistant_id,omitempty"`
	Input             json.RawMessage `json:"input,omitempty"`
	Command           json.RawMessage `json:"command,omitempty"`
	Metadata          map[string]any  `json:"metadata,omitempty"`
	Config            json.RawMessage `json:"config,omitempty"`
	Context           json.RawMessage `json:"context,omitempty"`
	CheckpointID      string          `json:"checkpoint_id,omitempty"`
	Checkpoint        json.RawMessage `json:"checkpoint,omitempty"`
	InterruptBefore   json.RawMessage `json:"interrupt_before,omitempty"`
	InterruptAfter    json.RawMessage `json:"interrupt_after,omitempty"`
	StreamMode        json.RawMessage `json:"stream_mode,omitempty"`
	StreamSubgraphs   bool            `json:"stream_subgraphs,omitempty"`
	StreamResumable   bool            `json:"stream_resumable,omitempty"`
	OnDisconnect      string          `json:"on_disconnect,omitempty"`
	MultitaskStrategy string          `json:"multitask_strategy,omitempty"`
	Webhook           json.RawMessage `json:"webhook,omitempty"`
	OnCompletion      json.RawMessage `json:"on_completion,omitempty"`
	AfterSeconds      json.RawMessage `json:"after_seconds,omitempty"`
	IfNotExists       string          `json:"if_not_exists,omitempty"`
	FeedbackKeys      json.RawMessage `json:"feedback_keys,omitempty"`
}

type threadResponse struct {
	ThreadID   domain.ThreadID   `json:"thread_id"`
	Title      string            `json:"title,omitempty"`
	Status     string            `json:"status"`
	CreatedAt  time.Time         `json:"created_at"`
	UpdatedAt  time.Time         `json:"updated_at"`
	Metadata   map[string]string `json:"metadata"`
	Values     map[string]any    `json:"values"`
	Interrupts map[string]any    `json:"interrupts"`
}

type runResponse struct {
	RunID             domain.RunID    `json:"run_id"`
	ThreadID          domain.ThreadID `json:"thread_id"`
	Status            string          `json:"status"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
	Error             string          `json:"error,omitempty"`
	TotalInputTokens  int             `json:"total_input_tokens"`
	TotalOutputTokens int             `json:"total_output_tokens"`
	TotalTokens       int             `json:"total_tokens"`
	LLMCallCount      int             `json:"llm_call_count"`
	LeadAgentTokens   int             `json:"lead_agent_tokens"`
	SubagentTokens    int             `json:"subagent_tokens"`
	MiddlewareTokens  int             `json:"middleware_tokens"`
	MessageCount      int             `json:"message_count"`
	StopReason        string          `json:"stop_reason,omitempty"`
}

// RunStarter starts a newly persisted run without coupling HTTP to the runtime implementation.
type RunStarter interface {
	Start(context.Context, StartRequest) error
}

// RunCanceller requests cancellation of an active run.
type RunCanceller interface {
	Cancel(context.Context, domain.RunID) error
}

// ThreadCleaner removes thread-scoped resources outside the durable store.
type ThreadCleaner interface {
	PrepareThreadDelete(context.Context, domain.ThreadID) error
	CleanupThread(context.Context, domain.ThreadID, string) error
}

// Config supplies durable adapters and transport policy.
type Config struct {
	Store         store.Store
	Starter       RunStarter
	Canceller     RunCanceller
	Cleaner       ThreadCleaner
	OwnerResolver func(context.Context) string
	Now           func() time.Time
	KeepAlive     time.Duration
}

// Handler is Gofer's standard-library HTTP gateway.
type Handler struct {
	store     store.Store
	starter   RunStarter
	canceller RunCanceller
	cleaner   ThreadCleaner
	owner     func(context.Context) string
	now       func() time.Time
	keepAlive time.Duration
	mux       *http.ServeMux
}

// New validates config and constructs the API handler.
func New(config Config) (*Handler, error) {
	if config.Store == nil {
		return nil, fmt.Errorf("%w: store is required", ErrInvalidConfig)
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.OwnerResolver == nil {
		config.OwnerResolver = func(context.Context) string { return "local" }
	}
	if config.KeepAlive == 0 {
		config.KeepAlive = 15 * time.Second
	}
	if config.KeepAlive < 0 {
		return nil, fmt.Errorf("%w: keepalive must be positive", ErrInvalidConfig)
	}
	handler := &Handler{store: config.Store, starter: config.Starter, canceller: config.Canceller, cleaner: config.Cleaner, owner: config.OwnerResolver, now: config.Now, keepAlive: config.KeepAlive, mux: http.NewServeMux()}
	handler.routes()
	return handler, nil
}

// ServeHTTP dispatches a versioned DeerFlow-compatible API subset.
func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	handler.mux.ServeHTTP(writer, request)
}

func (handler *Handler) routes() {
	handler.mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
	})
	handler.mux.HandleFunc("POST /api/threads", handler.createThread)
	handler.mux.HandleFunc("GET /api/threads/{thread_id}", handler.getThread)
	handler.threadRoutes()
	handler.mux.HandleFunc("POST /api/threads/{thread_id}/runs", handler.createRun)
	handler.mux.HandleFunc("POST /api/threads/{thread_id}/runs/stream", handler.streamRun)
	handler.mux.HandleFunc("POST /api/threads/{thread_id}/runs/wait", handler.waitRun)
	handler.mux.HandleFunc("GET /api/threads/{thread_id}/runs/{run_id}", handler.getRun)
	handler.mux.HandleFunc("POST /api/threads/{thread_id}/runs/{run_id}/cancel", handler.cancelRun)
	handler.mux.HandleFunc("GET /api/threads/{thread_id}/runs/{run_id}/events", handler.listEvents)
	handler.mux.HandleFunc("GET /api/threads/{thread_id}/runs/{run_id}/workspace-changes", handler.getWorkspaceChanges)
	handler.mux.HandleFunc("GET /api/threads/{thread_id}/runs/{run_id}/stream", handler.streamEvents)
	handler.mux.HandleFunc("POST /api/threads/{thread_id}/runs/{run_id}/stream", handler.postStreamEvents)
	handler.mux.HandleFunc("GET /api/threads/{thread_id}/runs/{run_id}/join", handler.streamEvents)
	handler.mux.HandleFunc("POST /api/runs/stream", handler.statelessStreamRun)
	handler.mux.HandleFunc("POST /api/runs/wait", handler.statelessWaitRun)
	handler.mux.HandleFunc("GET /api/runs/{run_id}/messages", handler.listMessagesByRunID)
}

func (handler *Handler) createThread(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		ThreadID    string            `json:"thread_id"`
		AssistantID string            `json:"assistant_id"`
		Title       string            `json:"title"`
		Metadata    map[string]string `json:"metadata"`
	}
	if err := decodeJSON(writer, request, &input); err != nil {
		return
	}
	if input.ThreadID != "" {
		id, parseErr := domain.ParseThreadID(input.ThreadID)
		if parseErr != nil {
			writeError(writer, parseErr)
			return
		}
		if existing, lookupErr := handler.ownedThread(request.Context(), id); lookupErr == nil {
			writeJSON(writer, http.StatusOK, makeThreadResponse(existing))
			return
		} else if !errors.Is(lookupErr, store.ErrNotFound) {
			writeError(writer, lookupErr)
			return
		}
	}
	thread, err := domain.NewThread(handler.now())
	if err == nil {
		if input.ThreadID != "" {
			thread.ID = domain.ThreadID(input.ThreadID)
		}
		thread.Title = strings.TrimSpace(input.Title)
		thread.Metadata = cloneMetadata(input.Metadata)
		if thread.Metadata == nil {
			thread.Metadata = make(map[string]string)
		}
		thread.Metadata[store.OwnerMetadataKey] = handler.ownerID(request.Context())
		if input.AssistantID != "" {
			if thread.Metadata == nil {
				thread.Metadata = make(map[string]string)
			}
			thread.Metadata["assistant_id"] = input.AssistantID
		}
		err = handler.store.CreateThread(request.Context(), thread)
	}
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, makeThreadResponse(thread))
}

func (handler *Handler) getThread(writer http.ResponseWriter, request *http.Request) {
	id, err := domain.ParseThreadID(request.PathValue("thread_id"))
	if err == nil {
		var thread domain.Thread
		thread, err = handler.ownedThread(request.Context(), id)
		if err == nil {
			var response threadResponse
			response, err = handler.currentThreadResponse(request.Context(), thread)
			if err == nil {
				writeJSON(writer, http.StatusOK, response)
				return
			}
		}
	}
	writeError(writer, err)
}

func (handler *Handler) createRun(writer http.ResponseWriter, request *http.Request) {
	threadID, err := domain.ParseThreadID(request.PathValue("thread_id"))
	if err != nil {
		writeError(writer, err)
		return
	}
	if _, err = handler.ownedThread(request.Context(), threadID); err != nil {
		writeError(writer, err)
		return
	}
	var input RunRequest
	if err = decodeJSON(writer, request, &input); err != nil {
		return
	}
	if err = validateRunRequest(input); err != nil {
		writeError(writer, err)
		return
	}
	run, err := handler.startRun(request.Context(), threadID, input)
	if err != nil {
		writeError(writer, err)
		return
	}
	writer.Header().Set("Content-Location", "/api/threads/"+string(threadID)+"/runs/"+string(run.ID))
	writeJSON(writer, http.StatusCreated, makeRunResponse(run))
}

func (handler *Handler) startRun(ctx context.Context, threadID domain.ThreadID, input RunRequest) (domain.Run, error) {
	run, err := domain.NewRun(threadID, handler.now())
	if err == nil {
		err = handler.store.CreateRun(ctx, run)
	}
	if err == nil {
		var draft event.Draft
		draft, err = event.NewDraft(threadID, run.ID, event.RunCreated, handler.now(), map[string]any{})
		if err == nil {
			_, err = handler.store.Append(ctx, run.ID, 0, draft)
		}
	}
	if err == nil && handler.starter != nil {
		err = handler.starter.Start(ctx, StartRequest{RunID: run.ID, ThreadID: threadID, Request: input})
	}
	return run, err
}

func (handler *Handler) getRun(writer http.ResponseWriter, request *http.Request) {
	run, err := handler.scopedRun(request)
	if err != nil {
		writeError(writer, err)
		return
	}
	response, err := handler.enrichedRunResponse(request.Context(), run)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, response)
}

func (handler *Handler) cancelRun(writer http.ResponseWriter, request *http.Request) {
	run, err := handler.scopedRun(request)
	if err != nil {
		writeError(writer, err)
		return
	}
	if handler.canceller == nil {
		writeJSON(writer, http.StatusNotImplemented, map[string]string{"error": "run cancellation is not configured"})
		return
	}
	if err = handler.canceller.Cancel(request.Context(), run.ID); err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, map[string]any{"run_id": run.ID, "cancel_requested": true})
}

func (handler *Handler) listEvents(writer http.ResponseWriter, request *http.Request) {
	run, err := handler.scopedRun(request)
	if err != nil {
		writeError(writer, err)
		return
	}
	after, limit, err := eventRange(request)
	if err != nil {
		writeError(writer, err)
		return
	}
	records, err := handler.store.Events(request.Context(), run.ID, after, limit)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, records)
}

func (handler *Handler) streamEvents(writer http.ResponseWriter, request *http.Request) {
	run, err := handler.scopedRun(request)
	if err != nil {
		writeError(writer, err)
		return
	}
	after, _, err := eventRange(request)
	if err != nil {
		writeError(writer, err)
		return
	}
	if header := strings.TrimSpace(request.Header.Get("Last-Event-ID")); header != "" {
		after, err = strconv.ParseUint(header, 10, 64)
		if err != nil {
			writeError(writer, fmt.Errorf("invalid Last-Event-ID: %w", err))
			return
		}
	}
	flusher, ok := writer.(http.Flusher)
	if !ok {
		writeError(writer, errors.New("streaming is unsupported"))
		return
	}
	watch, err := handler.store.Watch(request.Context(), run.ID, after)
	if err != nil {
		writeError(writer, err)
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("X-Accel-Buffering", "no")
	writer.WriteHeader(http.StatusOK)
	flusher.Flush()
	handler.streamJournal(request.Context(), writer, flusher, run.ID, after, watch)
}

func (handler *Handler) streamJournal(ctx context.Context, writer io.Writer, flusher http.Flusher, runID domain.RunID, after uint64, watch <-chan uint64) {
	ticker := time.NewTicker(handler.keepAlive)
	defer ticker.Stop()
	var terminalTimer *time.Timer
	var terminalTimeout <-chan time.Time
	defer func() {
		if terminalTimer != nil {
			terminalTimer.Stop()
		}
	}()
	for {
		latest, terminalEvent, streamErr := handler.writeAvailable(ctx, writer, flusher, runID, after)
		if streamErr != nil {
			return
		}
		after = latest
		if terminalEvent {
			return
		}
		current, runErr := handler.store.Run(ctx, runID)
		if runErr != nil {
			return
		}
		if runSettled(current) && terminalTimer == nil {
			terminalTimer = time.NewTimer(500 * time.Millisecond)
			terminalTimeout = terminalTimer.C
		}
		select {
		case <-ctx.Done():
			return
		case <-watch:
		case <-terminalTimeout:
			return
		case <-ticker.C:
			_, _ = io.WriteString(writer, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func (handler *Handler) writeAvailable(ctx context.Context, writer io.Writer, flusher http.Flusher, runID domain.RunID, after uint64) (uint64, bool, error) {
	records, err := handler.store.Events(ctx, runID, after, 0)
	if err != nil {
		return after, false, err
	}
	terminal := false
	for _, record := range records {
		data, marshalErr := json.Marshal(record)
		if marshalErr != nil {
			return after, false, marshalErr
		}
		if _, err = fmt.Fprintf(writer, "id: %d\nevent: %s\ndata: %s\n\n", record.Sequence, record.Kind, data); err != nil {
			return after, false, err
		}
		after = record.Sequence
		terminal = terminal || record.Kind == event.RunInterrupted || record.Kind == event.RunCompleted || record.Kind == event.RunFailed || record.Kind == event.RunCancelled
		flusher.Flush()
	}
	return after, terminal, nil
}

func runSettled(run domain.Run) bool {
	return run.Terminal() || run.Status == domain.RunInterrupted
}

func (handler *Handler) scopedRun(request *http.Request) (domain.Run, error) {
	threadID, err := domain.ParseThreadID(request.PathValue("thread_id"))
	if err != nil {
		return domain.Run{}, err
	}
	if _, err = handler.ownedThread(request.Context(), threadID); err != nil {
		return domain.Run{}, err
	}
	runID, err := domain.ParseRunID(request.PathValue("run_id"))
	if err != nil {
		return domain.Run{}, err
	}
	run, err := handler.store.Run(request.Context(), runID)
	if err != nil {
		return domain.Run{}, err
	}
	if run.ThreadID != threadID {
		return domain.Run{}, store.ErrNotFound
	}
	return run, nil
}

func (handler *Handler) getWorkspaceChanges(writer http.ResponseWriter, request *http.Request) {
	run, err := handler.scopedRun(request)
	if err != nil {
		writeError(writer, err)
		return
	}
	includeFiles, err := optionalBool(request, "include_files", true)
	if err == nil {
		var includeDiff bool
		includeDiff, err = optionalBool(request, "include_diff", true)
		if err == nil {
			var records []event.Event
			records, err = handler.store.Events(request.Context(), run.ID, 0, 0)
			if err == nil {
				writeJSON(writer, http.StatusOK, workspacechange.ResponseFromEvents(records, includeFiles, includeDiff))
				return
			}
		}
	}
	writeError(writer, err)
}

func optionalBool(request *http.Request, key string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(request.URL.Query().Get(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, store.ErrInvalidQuery
	}
	return value, nil
}

func eventRange(request *http.Request) (uint64, int, error) {
	after := uint64(0)
	limit := 0
	var err error
	if value := request.URL.Query().Get("after"); value != "" {
		after, err = strconv.ParseUint(value, 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid after: %w", err)
		}
	}
	if value := request.URL.Query().Get("limit"); value != "" {
		limit, err = strconv.Atoi(value)
		if err != nil || limit < 0 || limit > 10000 {
			return 0, 0, errors.New("invalid limit")
		}
	}
	return after, limit, nil
}

func validateRunRequest(request RunRequest) error {
	if request.StreamResumable {
		return errors.New("invalid stream_resumable: only false is supported")
	}
	if request.OnDisconnect != "" && request.OnDisconnect != "cancel" && request.OnDisconnect != "continue" {
		return errors.New("invalid on_disconnect")
	}
	if request.MultitaskStrategy != "" && request.MultitaskStrategy != "reject" &&
		request.MultitaskStrategy != "rollback" && request.MultitaskStrategy != "interrupt" {
		return errors.New("invalid multitask_strategy")
	}
	if request.IfNotExists != "" && request.IfNotExists != "create" {
		return errors.New("invalid if_not_exists")
	}
	for name, value := range map[string]json.RawMessage{
		"webhook": request.Webhook, "on_completion": request.OnCompletion,
		"after_seconds": request.AfterSeconds, "feedback_keys": request.FeedbackKeys,
	} {
		if len(value) != 0 && string(value) != "null" {
			return fmt.Errorf("invalid %s: unsupported option", name)
		}
	}
	return nil
}

func makeThreadResponse(thread domain.Thread) threadResponse {
	return threadResponse{ThreadID: thread.ID, Title: thread.Title, Status: "idle", CreatedAt: thread.CreatedAt,
		UpdatedAt: thread.UpdatedAt, Metadata: publicMetadata(thread.Metadata), Values: map[string]any{}, Interrupts: map[string]any{}}
}

func makeRunResponse(run domain.Run) runResponse {
	updated := run.FinishedAt
	if updated.IsZero() {
		updated = run.StartedAt
	}
	if updated.IsZero() {
		updated = run.CreatedAt
	}
	status := string(run.Status)
	switch run.Status {
	case domain.RunSucceeded:
		status = "success"
	case domain.RunFailed:
		status = "error"
	case domain.RunPending, domain.RunRunning, domain.RunInterrupted, domain.RunCancelled:
	}
	return runResponse{RunID: run.ID, ThreadID: run.ThreadID, Status: status,
		CreatedAt: run.CreatedAt, UpdatedAt: updated, Error: run.Error}
}

func (handler *Handler) enrichedRunResponse(ctx context.Context, run domain.Run) (runResponse, error) {
	records, err := handler.store.Events(ctx, run.ID, 0, 0)
	if err != nil {
		return runResponse{}, err
	}
	summary := usage.Summarize(records)
	response := makeRunResponse(run)
	response.TotalInputTokens = summary.InputTokens
	response.TotalOutputTokens = summary.OutputTokens
	response.TotalTokens = summary.TotalTokens()
	response.LLMCallCount = summary.LLMCallCount
	response.LeadAgentTokens = summary.LeadAgentTokens
	response.SubagentTokens = summary.SubagentTokens
	response.MiddlewareTokens = summary.MiddlewareTokens
	response.MessageCount = summary.MessageCount
	response.StopReason = summary.StopReason
	return response, nil
}

func decodeJSON(writer http.ResponseWriter, request *http.Request, target any) error {
	request.Body = http.MaxBytesReader(writer, request.Body, maxRequestBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeJSON(writer, http.StatusBadRequest, map[string]string{"error": "body must contain one JSON value"})
		return errors.New("multiple JSON values")
	}
	return nil
}

func writeError(writer http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	if errors.Is(err, store.ErrNotFound) {
		status = http.StatusNotFound
	} else if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrExists) {
		status = http.StatusConflict
	} else if errors.Is(err, store.ErrInvalidQuery) || errors.Is(err, domain.ErrInvalidID) || errors.Is(err, domain.ErrInvalidRun) || errors.Is(err, domain.ErrInvalidThread) || strings.Contains(err.Error(), "invalid ") {
		status = http.StatusBadRequest
	}
	writeJSON(writer, status, map[string]string{"error": http.StatusText(status)})
}
func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
