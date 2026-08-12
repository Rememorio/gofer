package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/conversation"
	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/event"
	"github.com/Rememorio/gofer/internal/model"
	"github.com/Rememorio/gofer/internal/store"
	"github.com/Rememorio/gofer/internal/workspacechange"
)

type lifecycle struct {
	mu        sync.Mutex
	started   []StartRequest
	cancelled []domain.RunID
	err       error
}

type cleanupRecorder struct {
	threadID   domain.ThreadID
	ownerID    string
	prepareErr error
}

type eventErrorStore struct{ store.Store }

func (eventErrorStore) Events(context.Context, domain.RunID, uint64, int) ([]event.Event, error) {
	return nil, errors.New("events unavailable")
}

func (cleaner *cleanupRecorder) PrepareThreadDelete(context.Context, domain.ThreadID) error {
	return cleaner.prepareErr
}

func (cleaner *cleanupRecorder) CleanupThread(_ context.Context, threadID domain.ThreadID, ownerID string) error {
	cleaner.threadID, cleaner.ownerID = threadID, ownerID
	return nil
}

func (lifecycle *lifecycle) Start(_ context.Context, request StartRequest) error {
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	lifecycle.started = append(lifecycle.started, request)
	return lifecycle.err
}

func (lifecycle *lifecycle) Cancel(_ context.Context, id domain.RunID) error {
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	lifecycle.cancelled = append(lifecycle.cancelled, id)
	return lifecycle.err
}

func TestThreadRunEventAndSSELifecycle(t *testing.T) {
	t.Parallel()
	memory := store.NewMemory()
	lifecycle := &lifecycle{}
	now := time.Unix(100, 0)
	handler, err := New(Config{Store: memory, Starter: lifecycle, Canceller: lifecycle, Now: func() time.Time { return now }, KeepAlive: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}

	created := perform(t, handler, http.MethodPost, "/api/threads", `{"title":" Work ","metadata":{"k":"v"}}`, nil)
	if created.Code != http.StatusOK {
		t.Fatalf("create thread = %d %s", created.Code, created.Body.String())
	}
	var thread domain.Thread
	decodeResponse(t, created, &thread)
	if thread.Title != "Work" {
		t.Fatalf("thread = %#v", thread)
	}
	got := perform(t, handler, http.MethodGet, "/api/threads/"+string(thread.ID), "", nil)
	if got.Code != http.StatusOK {
		t.Fatalf("get thread = %d", got.Code)
	}

	runResponse := perform(t, handler, http.MethodPost, "/api/threads/"+string(thread.ID)+"/runs", `{"assistant_id":"lead_agent","input":{"messages":[]},"metadata":{"source":"test"},"on_disconnect":"continue","multitask_strategy":"reject","if_not_exists":"create"}`, nil)
	if runResponse.Code != http.StatusCreated || runResponse.Header().Get("Content-Location") == "" {
		t.Fatalf("create run = %d %s", runResponse.Code, runResponse.Body.String())
	}
	var run domain.Run
	decodeResponse(t, runResponse, &run)
	lifecycle.mu.Lock()
	started := append([]StartRequest(nil), lifecycle.started...)
	lifecycle.mu.Unlock()
	if len(started) != 1 || started[0].RunID != run.ID || started[0].Request.Metadata["source"] != "test" || string(started[0].Request.Input) != `{"messages":[]}` {
		t.Fatalf("started = %#v", started)
	}

	draft, _ := event.NewDraft(thread.ID, run.ID, event.MessageDelta, now.Add(time.Second), map[string]string{"text": "hello"})
	if _, err := memory.Append(context.Background(), run.ID, 1, draft); err != nil {
		t.Fatal(err)
	}
	if _, err := memory.TransitionRun(context.Background(), run.ID, domain.RunPending, domain.RunRunning, now.Add(time.Second), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := memory.TransitionRun(context.Background(), run.ID, domain.RunRunning, domain.RunSucceeded, now.Add(2*time.Second), ""); err != nil {
		t.Fatal(err)
	}

	base := "/api/threads/" + string(thread.ID) + "/runs/" + string(run.ID)
	events := perform(t, handler, http.MethodGet, base+"/events?after=1&limit=1", "", nil)
	var records []event.Event
	decodeResponse(t, events, &records)
	if len(records) != 1 || records[0].Sequence != 2 {
		t.Fatalf("events = %#v", records)
	}
	stream := perform(t, handler, http.MethodGet, base+"/stream", "", map[string]string{"Last-Event-ID": "1"})
	if stream.Code != http.StatusOK || !strings.Contains(stream.Body.String(), "id: 2\nevent: message.delta") {
		t.Fatalf("stream = %d %q", stream.Code, stream.Body.String())
	}
}

func TestCancelHealthAndErrors(t *testing.T) {
	t.Parallel()
	memory := store.NewMemory()
	lifecycle := &lifecycle{}
	handler, _ := New(Config{Store: memory, Canceller: lifecycle})
	health := perform(t, handler, http.MethodGet, "/healthz", "", nil)
	if health.Code != http.StatusOK {
		t.Fatal(health.Code)
	}
	thread, _ := domain.NewThread(time.Now())
	_ = memory.CreateThread(context.Background(), thread)
	run, _ := domain.NewRun(thread.ID, time.Now())
	_ = memory.CreateRun(context.Background(), run)
	base := "/api/threads/" + string(thread.ID) + "/runs/" + string(run.ID)
	if response := perform(t, handler, http.MethodGet, base, "", nil); response.Code != http.StatusOK {
		t.Fatalf("get run=%d", response.Code)
	}
	if response := perform(t, handler, http.MethodPost, base+"/cancel", `{}`, nil); response.Code != http.StatusAccepted {
		t.Fatalf("cancel=%d", response.Code)
	}
	if response := perform(t, handler, http.MethodGet, "/api/threads/bad", "", nil); response.Code != http.StatusBadRequest {
		t.Fatalf("bad id=%d", response.Code)
	}
	if response := perform(t, handler, http.MethodGet, base+"/events?limit=-1", "", nil); response.Code != http.StatusBadRequest {
		t.Fatalf("bad range=%d", response.Code)
	}
	if response := perform(t, handler, http.MethodGet, base+"/events?after=bad", "", nil); response.Code != http.StatusBadRequest {
		t.Fatalf("bad after=%d", response.Code)
	}
	if response := perform(t, handler, http.MethodGet, base+"/stream", "", map[string]string{"Last-Event-ID": "bad"}); response.Code != http.StatusBadRequest {
		t.Fatalf("bad event id=%d", response.Code)
	}
	if response := perform(t, handler, http.MethodPost, "/api/threads", `{"unknown":1}`, nil); response.Code != http.StatusBadRequest {
		t.Fatalf("bad body=%d", response.Code)
	}
	if response := perform(t, handler, http.MethodPost, "/api/threads", `{} {}`, nil); response.Code != http.StatusBadRequest {
		t.Fatalf("multiple body=%d", response.Code)
	}
	other, _ := domain.NewThread(time.Now())
	_ = memory.CreateThread(context.Background(), other)
	if response := perform(t, handler, http.MethodGet, "/api/threads/"+string(other.ID)+"/runs/"+string(run.ID), "", nil); response.Code != http.StatusNotFound {
		t.Fatalf("scope=%d", response.Code)
	}
	withoutCancel, _ := New(Config{Store: memory})
	if response := perform(t, withoutCancel, http.MethodPost, base+"/cancel", `{}`, nil); response.Code != http.StatusNotImplemented {
		t.Fatalf("not implemented=%d", response.Code)
	}
}

func TestWorkspaceChangesReviewLifecycle(t *testing.T) {
	t.Parallel()
	memory := store.NewMemory()
	thread, _ := domain.NewThread(time.Now())
	if err := memory.CreateThread(context.Background(), thread); err != nil {
		t.Fatal(err)
	}
	run, _ := domain.NewRun(thread.ID, time.Now())
	if err := memory.CreateRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	result := workspacechange.Result{
		Version: 1, Summary: workspacechange.Summary{Created: 1, Additions: 1}, Limits: workspacechange.DefaultLimits(),
		Files: []workspacechange.FileChange{{Path: "/mnt/user-data/outputs/report.md", Status: workspacechange.StatusCreated, Diff: "+ready"}},
	}
	draft, _ := event.NewDraft(thread.ID, run.ID, event.WorkspaceChanges, time.Now(), workspacechange.NewEventPayload(result))
	if _, err := memory.Append(context.Background(), run.ID, 0, draft); err != nil {
		t.Fatal(err)
	}
	handler, _ := New(Config{Store: memory})
	base := "/api/threads/" + string(thread.ID) + "/runs/" + string(run.ID) + "/workspace-changes"

	response := perform(t, handler, http.MethodGet, base, "", nil)
	var review workspacechange.Response
	decodeResponse(t, response, &review)
	if response.Code != http.StatusOK || !review.Available || len(review.Files) != 1 || review.Files[0].Diff != "+ready" {
		t.Fatalf("review = %d %#v", response.Code, review)
	}
	withoutDiff := perform(t, handler, http.MethodGet, base+"?include_diff=false", "", nil)
	decodeResponse(t, withoutDiff, &review)
	if len(review.Files) != 1 || review.Files[0].Diff != "" {
		t.Fatalf("without diff = %#v", review)
	}
	withoutFiles := perform(t, handler, http.MethodGet, base+"?include_files=false", "", nil)
	decodeResponse(t, withoutFiles, &review)
	if review.Files == nil || len(review.Files) != 0 {
		t.Fatalf("without files = %#v", review)
	}
	if got := perform(t, handler, http.MethodGet, base+"?include_diff=bad", "", nil); got.Code != http.StatusBadRequest {
		t.Fatalf("invalid query = %d", got.Code)
	}
}

func TestWorkspaceChangesReviewHandlesEmptyScopeAndStoreErrors(t *testing.T) {
	t.Parallel()
	memory := store.NewMemory()
	thread, _ := domain.NewThread(time.Now())
	_ = memory.CreateThread(context.Background(), thread)
	run, _ := domain.NewRun(thread.ID, time.Now())
	_ = memory.CreateRun(context.Background(), run)
	handler, _ := New(Config{Store: memory})
	base := "/api/threads/" + string(thread.ID) + "/runs/" + string(run.ID) + "/workspace-changes"
	empty := perform(t, handler, http.MethodGet, base, "", nil)
	var review workspacechange.Response
	decodeResponse(t, empty, &review)
	if empty.Code != http.StatusOK || review.Available || review.Files == nil {
		t.Fatalf("empty = %d %#v", empty.Code, review)
	}
	otherThread, _ := domain.NewThread(time.Now())
	_ = memory.CreateThread(context.Background(), otherThread)
	wrong := "/api/threads/" + string(otherThread.ID) + "/runs/" + string(run.ID) + "/workspace-changes"
	if got := perform(t, handler, http.MethodGet, wrong, "", nil); got.Code != http.StatusNotFound {
		t.Fatalf("wrong scope = %d", got.Code)
	}
	errorHandler, _ := New(Config{Store: eventErrorStore{Store: memory}})
	if got := perform(t, errorHandler, http.MethodGet, base, "", nil); got.Code != http.StatusInternalServerError {
		t.Fatalf("event error = %d", got.Code)
	}
}

func TestNewAndLifecycleFailures(t *testing.T) {
	t.Parallel()
	if _, err := New(Config{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New nil=%v", err)
	}
	if _, err := New(Config{Store: store.NewMemory(), KeepAlive: -1}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New keepalive=%v", err)
	}
	memory := store.NewMemory()
	lifecycle := &lifecycle{err: errors.New("start failed")}
	handler, _ := New(Config{Store: memory, Starter: lifecycle})
	thread, _ := domain.NewThread(time.Now())
	_ = memory.CreateThread(context.Background(), thread)
	response := perform(t, handler, http.MethodPost, "/api/threads/"+string(thread.ID)+"/runs", `{}`, nil)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("start error=%d", response.Code)
	}
}

func TestCancelFailureAndEmptyTerminalStream(t *testing.T) {
	t.Parallel()
	memory := store.NewMemory()
	lifecycle := &lifecycle{err: errors.New("cancel failed")}
	handler, _ := New(Config{Store: memory, Canceller: lifecycle})
	thread, _ := domain.NewThread(time.Now())
	_ = memory.CreateThread(context.Background(), thread)
	run, _ := domain.NewRun(thread.ID, time.Now())
	_ = memory.CreateRun(context.Background(), run)
	running, _ := memory.TransitionRun(context.Background(), run.ID, domain.RunPending, domain.RunRunning, time.Now(), "")
	_, _ = memory.TransitionRun(context.Background(), running.ID, domain.RunRunning, domain.RunSucceeded, time.Now(), "")
	base := "/api/threads/" + string(thread.ID) + "/runs/" + string(run.ID)
	if response := perform(t, handler, http.MethodPost, base+"/cancel", `{}`, nil); response.Code != http.StatusInternalServerError {
		t.Fatalf("cancel failure=%d", response.Code)
	}
	if response := perform(t, handler, http.MethodGet, base+"/stream", "", nil); response.Code != http.StatusOK || response.Body.Len() != 0 {
		t.Fatalf("empty stream=%d %q", response.Code, response.Body.String())
	}
}

func TestLiveStreamKeepAliveAndContextCancellation(t *testing.T) {
	t.Parallel()
	memory := store.NewMemory()
	handler, _ := New(Config{Store: memory, KeepAlive: time.Millisecond})
	thread, _ := domain.NewThread(time.Now())
	_ = memory.CreateThread(context.Background(), thread)
	run, _ := domain.NewRun(thread.ID, time.Now())
	_ = memory.CreateRun(context.Background(), run)
	path := "/api/threads/" + string(thread.ID) + "/runs/" + string(run.ID) + "/stream"
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { handler.ServeHTTP(response, request); close(done) }()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream did not stop")
	}
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), ": keepalive") {
		t.Fatalf("stream=%d %q", response.Code, response.Body.String())
	}
}

func TestRunAndEventLookupErrors(t *testing.T) {
	t.Parallel()
	memory := store.NewMemory()
	handler, _ := New(Config{Store: memory})
	thread, _ := domain.NewThread(time.Now())
	_ = memory.CreateThread(context.Background(), thread)
	missingThread := newThreadID(t)
	if response := perform(t, handler, http.MethodPost, "/api/threads/"+string(missingThread)+"/runs", `{}`, nil); response.Code != http.StatusNotFound {
		t.Fatalf("missing thread=%d", response.Code)
	}
	run, _ := domain.NewRun(thread.ID, time.Now())
	_ = memory.CreateRun(context.Background(), run)
	base := "/api/threads/" + string(thread.ID) + "/runs/" + string(run.ID)
	if response := perform(t, handler, http.MethodGet, base+"/events", "", nil); response.Code != http.StatusOK {
		t.Fatalf("empty events=%d", response.Code)
	}
	if response := perform(t, handler, http.MethodGet, "/api/threads/"+string(thread.ID)+"/runs/bad", "", nil); response.Code != http.StatusBadRequest {
		t.Fatalf("bad run id=%d", response.Code)
	}
	if response := perform(t, handler, http.MethodGet, "/api/threads/"+string(thread.ID)+"/runs/"+string(newRunID(t)), "", nil); response.Code != http.StatusNotFound {
		t.Fatalf("missing run=%d", response.Code)
	}
}

func TestRunRequestCompatibilityValidation(t *testing.T) {
	t.Parallel()
	valid := []RunRequest{{}, {OnDisconnect: "cancel", MultitaskStrategy: "rollback", IfNotExists: "create"}, {OnDisconnect: "continue", MultitaskStrategy: "interrupt", Webhook: json.RawMessage(`null`)}}
	for _, request := range valid {
		if err := validateRunRequest(request); err != nil {
			t.Fatalf("valid %#v: %v", request, err)
		}
	}
	invalid := []RunRequest{
		{StreamResumable: true}, {OnDisconnect: "detach"}, {MultitaskStrategy: "parallel"},
		{IfNotExists: "reject"}, {Webhook: json.RawMessage(`{}`)}, {OnCompletion: json.RawMessage(`"x"`)},
		{AfterSeconds: json.RawMessage(`1`)}, {FeedbackKeys: json.RawMessage(`[]`)},
	}
	for _, request := range invalid {
		if err := validateRunRequest(request); err == nil {
			t.Fatalf("invalid accepted: %#v", request)
		}
	}
}

func TestCompatibilityResponsesAndIdempotentThreadCreate(t *testing.T) {
	t.Parallel()
	memory := store.NewMemory()
	handler, _ := New(Config{Store: memory})
	thread, _ := domain.NewThread(time.Now())
	body := `{"thread_id":"` + string(thread.ID) + `","assistant_id":"lead","metadata":{"x":"y"}}`
	first := perform(t, handler, http.MethodPost, "/api/threads", body, nil)
	if first.Code != http.StatusOK {
		t.Fatalf("first=%d", first.Code)
	}
	second := perform(t, handler, http.MethodPost, "/api/threads", body, nil)
	if second.Code != http.StatusOK {
		t.Fatalf("second=%d", second.Code)
	}
	stored, err := memory.Thread(context.Background(), thread.ID)
	if err != nil || stored.Metadata["assistant_id"] != "lead" {
		t.Fatalf("stored=%#v %v", stored, err)
	}
	for _, test := range []struct {
		status domain.RunStatus
		want   string
	}{{domain.RunPending, "pending"}, {domain.RunRunning, "running"}, {domain.RunSucceeded, "success"}, {domain.RunFailed, "error"}} {
		run := domain.Run{Status: test.status, CreatedAt: time.Unix(1, 0), StartedAt: time.Unix(2, 0), FinishedAt: time.Unix(3, 0), Error: "x"}
		response := makeRunResponse(run)
		if response.Status != test.want || response.UpdatedAt != run.FinishedAt {
			t.Fatalf("response=%#v", response)
		}
	}
}

func TestThreadManagementAndOwnerIsolation(t *testing.T) {
	t.Parallel()
	memory := store.NewMemory()
	alice, _ := New(Config{Store: memory, OwnerResolver: func(context.Context) string { return "alice" }})
	bob, _ := New(Config{Store: memory, OwnerResolver: func(context.Context) string { return "bob" }})
	created := perform(t, alice, http.MethodPost, "/api/threads", `{"title":"Project Alpha","metadata":{"team":"core","user_id":"mallory"}}`, nil)
	var thread threadResponse
	decodeResponse(t, created, &thread)
	if created.Code != http.StatusOK || thread.Metadata[store.OwnerMetadataKey] != "" {
		t.Fatalf("created = %d %#v", created.Code, thread)
	}
	stored, _ := memory.Thread(context.Background(), thread.ThreadID)
	if stored.Metadata[store.OwnerMetadataKey] != "alice" {
		t.Fatalf("stored owner = %#v", stored.Metadata)
	}
	if response := perform(t, bob, http.MethodGet, "/api/threads/"+string(thread.ThreadID), "", nil); response.Code != http.StatusNotFound {
		t.Fatalf("cross-owner get = %d", response.Code)
	}
	listed := perform(t, alice, http.MethodGet, "/api/threads?q=alpha&limit=10", "", nil)
	var page struct {
		Threads []threadResponse `json:"threads"`
		Count   int              `json:"count"`
	}
	decodeResponse(t, listed, &page)
	if len(page.Threads) != 1 || page.Count != 1 {
		t.Fatalf("list = %#v", page)
	}
	searched := perform(t, alice, http.MethodPost, "/api/threads/search", `{"metadata":{"team":"core"}}`, nil)
	var results []threadResponse
	decodeResponse(t, searched, &results)
	if len(results) != 1 || results[0].ThreadID != thread.ThreadID {
		t.Fatalf("search = %#v", results)
	}
	patched := perform(t, alice, http.MethodPatch, "/api/threads/"+string(thread.ThreadID), `{"title":"Renamed","metadata":{"pinned":"true","user_id":"bob"}}`, nil)
	decodeResponse(t, patched, &thread)
	if patched.Code != http.StatusOK || thread.Title != "Renamed" || thread.Metadata["pinned"] != "true" {
		t.Fatalf("patch = %d %#v", patched.Code, thread)
	}
	stored, _ = memory.Thread(context.Background(), thread.ThreadID)
	if stored.Metadata[store.OwnerMetadataKey] != "alice" {
		t.Fatalf("patch changed owner = %#v", stored.Metadata)
	}
	if response := perform(t, alice, http.MethodGet, "/api/threads?limit=0", "", nil); response.Code != http.StatusBadRequest {
		t.Fatalf("invalid page = %d", response.Code)
	}
}

func TestThreadHistoryStateAndDeletion(t *testing.T) {
	t.Parallel()
	memory := store.NewMemory()
	cleaner := &cleanupRecorder{}
	handler, _ := New(Config{Store: memory, Cleaner: cleaner})
	created := perform(t, handler, http.MethodPost, "/api/threads", `{"title":"History"}`, nil)
	var thread threadResponse
	decodeResponse(t, created, &thread)
	createdRun := perform(t, handler, http.MethodPost, "/api/threads/"+string(thread.ThreadID)+"/runs", `{}`, nil)
	var run runResponse
	decodeResponse(t, createdRun, &run)
	now := time.Now().UTC()
	user, _ := domain.NewTextMessage(domain.RoleUser, "hello", now)
	if err := conversation.PersistInputs(context.Background(), memory, thread.ThreadID, run.RunID, []domain.Message{user}); err != nil {
		t.Fatal(err)
	}
	assistant, _ := domain.NewTextMessage(domain.RoleAssistant, "hi", now.Add(time.Second))
	draft, _ := event.NewDraft(thread.ThreadID, run.RunID, event.MessageCompleted, assistant.CreatedAt, map[string]any{
		"message": assistant, "model": "primary", "caller": "lead_agent",
		"usage": model.Usage{InputTokens: 4, OutputTokens: 2}, "stop_reason": model.StopEndTurn,
	})
	if _, err := memory.Append(context.Background(), run.RunID, 2, draft); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/api/threads/" + string(thread.ThreadID) + "/messages",
		"/api/threads/" + string(thread.ThreadID) + "/runs/" + string(run.RunID) + "/messages",
	} {
		response := perform(t, handler, http.MethodGet, path, "", nil)
		var messages []domain.Message
		decodeResponse(t, response, &messages)
		if response.Code != http.StatusOK || len(messages) != 2 {
			t.Fatalf("messages %s = %d %#v", path, response.Code, messages)
		}
	}
	limited := perform(t, handler, http.MethodGet, "/api/threads/"+string(thread.ThreadID)+"/messages?limit=1", "", nil)
	var limitedMessages []domain.Message
	decodeResponse(t, limited, &limitedMessages)
	if len(limitedMessages) != 1 || limitedMessages[0].ID != assistant.ID {
		t.Fatalf("limited messages = %#v", limitedMessages)
	}
	if response := perform(t, handler, http.MethodGet, "/api/threads/"+string(thread.ThreadID)+"/messages?limit=bad", "", nil); response.Code != http.StatusBadRequest {
		t.Fatalf("invalid message limit = %d", response.Code)
	}
	runs := perform(t, handler, http.MethodGet, "/api/threads/"+string(thread.ThreadID)+"/runs", "", nil)
	var listed []runResponse
	decodeResponse(t, runs, &listed)
	if len(listed) != 1 || listed[0].RunID != run.RunID {
		t.Fatalf("runs = %#v", listed)
	}
	assertGatewayRunUsage(t, handler, thread.ThreadID, run.RunID, listed[0])
	state := perform(t, handler, http.MethodGet, "/api/threads/"+string(thread.ThreadID)+"/state", "", nil)
	if state.Code != http.StatusOK || !strings.Contains(state.Body.String(), `"title":"History"`) || !strings.Contains(state.Body.String(), `"hello"`) {
		t.Fatalf("state = %d %s", state.Code, state.Body.String())
	}
	deletePath := "/api/threads/" + string(thread.ThreadID)
	if response := perform(t, handler, http.MethodDelete, deletePath, "", nil); response.Code != http.StatusConflict {
		t.Fatalf("delete active = %d", response.Code)
	}
	running, _ := memory.TransitionRun(context.Background(), run.RunID, domain.RunPending, domain.RunRunning, now, "")
	_, _ = memory.TransitionRun(context.Background(), running.ID, domain.RunRunning, domain.RunSucceeded, now.Add(time.Second), "")
	if response := perform(t, handler, http.MethodDelete, deletePath, "", nil); response.Code != http.StatusOK {
		t.Fatalf("delete = %d %s", response.Code, response.Body.String())
	}
	if cleaner.threadID != thread.ThreadID || cleaner.ownerID != "local" {
		t.Fatalf("cleanup = %#v", cleaner)
	}
}

func assertGatewayRunUsage(t *testing.T, handler *Handler, threadID domain.ThreadID, runID domain.RunID, listed runResponse) {
	t.Helper()
	if listed.TotalTokens != 6 || listed.LLMCallCount != 1 || listed.StopReason != string(model.StopEndTurn) {
		t.Fatalf("run usage = %#v", listed)
	}
	gotRun := perform(t, handler, http.MethodGet, "/api/threads/"+string(threadID)+"/runs/"+string(runID), "", nil)
	var enriched runResponse
	decodeResponse(t, gotRun, &enriched)
	if gotRun.Code != http.StatusOK || enriched.TotalInputTokens != 4 || enriched.MessageCount != 2 {
		t.Fatalf("get run = %d %#v", gotRun.Code, enriched)
	}
}

func TestThreadManagementErrorResponses(t *testing.T) {
	t.Parallel()
	memory := store.NewMemory()
	cleaner := &cleanupRecorder{prepareErr: errors.New("busy cleanup")}
	handler, _ := New(Config{Store: memory, Cleaner: cleaner, OwnerResolver: func(context.Context) string { return "" }})
	missing := newThreadID(t)
	for _, path := range []string{
		"/api/threads/" + string(missing) + "/state",
		"/api/threads/" + string(missing) + "/runs",
		"/api/threads/" + string(missing) + "/messages",
	} {
		if response := perform(t, handler, http.MethodGet, path, "", nil); response.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d", path, response.Code)
		}
	}
	if response := perform(t, handler, http.MethodGet, "/api/threads/"+string(missing)+"/runs/"+string(newRunID(t))+"/messages", "", nil); response.Code != http.StatusNotFound {
		t.Fatalf("missing run messages = %d", response.Code)
	}
	if response := perform(t, handler, http.MethodPost, "/api/threads/search", `{"limit":201}`, nil); response.Code != http.StatusBadRequest {
		t.Fatalf("invalid search = %d", response.Code)
	}
	if response := perform(t, handler, http.MethodGet, "/api/threads?offset=bad", "", nil); response.Code != http.StatusBadRequest {
		t.Fatalf("invalid offset = %d", response.Code)
	}
	created := perform(t, handler, http.MethodPost, "/api/threads", `{}`, nil)
	var thread threadResponse
	decodeResponse(t, created, &thread)
	if response := perform(t, handler, http.MethodPatch, "/api/threads/"+string(thread.ThreadID), `{}`, nil); response.Code != http.StatusBadRequest {
		t.Fatalf("empty patch = %d", response.Code)
	}
	if response := perform(t, handler, http.MethodDelete, "/api/threads/"+string(thread.ThreadID), "", nil); response.Code != http.StatusInternalServerError {
		t.Fatalf("cleanup preparation = %d", response.Code)
	}
	if _, err := memory.Thread(context.Background(), thread.ThreadID); err != nil {
		t.Fatalf("prepare failure deleted thread: %v", err)
	}
	if metadata := publicMetadata(nil); len(metadata) != 0 {
		t.Fatalf("publicMetadata(nil) = %#v", metadata)
	}
}

func TestRunUsageEventFailuresAreReported(t *testing.T) {
	t.Parallel()
	memory := store.NewMemory()
	thread, _ := domain.NewThread(time.Now())
	if err := memory.CreateThread(context.Background(), thread); err != nil {
		t.Fatal(err)
	}
	run, _ := domain.NewRun(thread.ID, time.Now())
	if err := memory.CreateRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	handler, err := New(Config{Store: eventErrorStore{Store: memory}})
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/threads/" + string(thread.ID) + "/runs"
	if response := perform(t, handler, http.MethodGet, base+"/"+string(run.ID), "", nil); response.Code != http.StatusInternalServerError {
		t.Fatalf("get run event failure = %d", response.Code)
	}
	if response := perform(t, handler, http.MethodGet, base, "", nil); response.Code != http.StatusInternalServerError {
		t.Fatalf("list runs event failure = %d", response.Code)
	}
}

func perform(t *testing.T, handler http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequestWithContext(context.Background(), method, path, bytes.NewBufferString(body))
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func decodeResponse(t *testing.T, response *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), target); err != nil {
		t.Fatalf("decode %q: %v", response.Body.String(), err)
	}
}

func newThreadID(t *testing.T) domain.ThreadID {
	t.Helper()
	thread, _ := domain.NewThread(time.Now())
	return thread.ID
}
func newRunID(t *testing.T) domain.RunID {
	t.Helper()
	run, _ := domain.NewRun(newThreadID(t), time.Now())
	return run.ID
}
