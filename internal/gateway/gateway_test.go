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

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/event"
	"github.com/Rememorio/gofer/internal/store"
)

type lifecycle struct {
	mu        sync.Mutex
	started   []StartRequest
	cancelled []domain.RunID
	err       error
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
