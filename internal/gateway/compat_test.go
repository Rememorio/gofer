package gateway

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/event"
	"github.com/Rememorio/gofer/internal/store"
)

type completingStarter struct{ store *store.Memory }

type completingCanceller struct {
	store *store.Memory
	err   error
}

func (starter completingStarter) Start(ctx context.Context, request StartRequest) error {
	now := time.Now().UTC()
	run, err := starter.store.TransitionRun(ctx, request.RunID, domain.RunPending, domain.RunRunning, now, "")
	if err != nil {
		return err
	}
	started, _ := event.NewDraft(request.ThreadID, request.RunID, event.RunStarted, now, map[string]any{"attempt": run.Attempt})
	committed, err := starter.store.Append(ctx, request.RunID, 1, started)
	if err != nil {
		return err
	}
	run, err = starter.store.TransitionRun(ctx, request.RunID, domain.RunRunning, domain.RunSucceeded, now.Add(time.Nanosecond), "")
	if err != nil {
		return err
	}
	finished, _ := event.NewDraft(request.ThreadID, request.RunID, event.RunCompleted, run.FinishedAt, map[string]any{"turns": 0})
	_, err = starter.store.Append(ctx, request.RunID, committed[len(committed)-1].Sequence, finished)
	return err
}

func (canceller completingCanceller) Cancel(ctx context.Context, runID domain.RunID) error {
	if canceller.err != nil {
		return canceller.err
	}
	run, err := canceller.store.Run(ctx, runID)
	if err != nil {
		return err
	}
	run, err = canceller.store.TransitionRun(ctx, runID, run.Status, domain.RunCancelled, time.Now().UTC(), "")
	if err != nil {
		return err
	}
	records, err := canceller.store.Events(ctx, runID, 0, 0)
	if err != nil {
		return err
	}
	sequence := records[len(records)-1].Sequence
	draft, _ := event.NewDraft(run.ThreadID, run.ID, event.RunCancelled, run.FinishedAt, map[string]string{"error": "cancelled"})
	_, err = canceller.store.Append(ctx, runID, sequence, draft)
	return err
}

func TestRunStreamWaitJoinCompatibility(t *testing.T) {
	t.Parallel()
	memory := store.NewMemory()
	handler, err := New(Config{Store: memory, Starter: completingStarter{store: memory}})
	if err != nil {
		t.Fatal(err)
	}
	created := perform(t, handler, http.MethodPost, "/api/threads", `{}`, nil)
	var thread threadResponse
	decodeResponse(t, created, &thread)
	base := "/api/threads/" + string(thread.ThreadID) + "/runs"
	stream := perform(t, handler, http.MethodPost, base+"/stream", `{}`, nil)
	if stream.Code != http.StatusOK || stream.Header().Get("Content-Location") == "" || !strings.Contains(stream.Body.String(), "event: run.completed") {
		t.Fatalf("stream = %d %#v %s", stream.Code, stream.Header(), stream.Body.String())
	}
	waited := perform(t, handler, http.MethodPost, base+"/wait", `{}`, nil)
	if waited.Code != http.StatusOK || !strings.Contains(waited.Body.String(), `"messages":[]`) {
		t.Fatalf("wait = %d %s", waited.Code, waited.Body.String())
	}
	runs, err := memory.Runs(context.Background(), thread.ThreadID)
	if err != nil || len(runs) != 2 {
		t.Fatalf("runs = %#v, %v", runs, err)
	}
	resource := "/api/threads/" + string(thread.ThreadID) + "/runs/" + string(runs[0].ID)
	joined := perform(t, handler, http.MethodGet, resource+"/join", "", nil)
	if joined.Code != http.StatusOK || !strings.Contains(joined.Body.String(), "event: run.completed") {
		t.Fatalf("join = %d %s", joined.Code, joined.Body.String())
	}
	posted := perform(t, handler, http.MethodPost, resource+"/stream", "", nil)
	if posted.Code != http.StatusOK || !strings.Contains(posted.Body.String(), "event: run.completed") {
		t.Fatalf("posted stream = %d %s", posted.Code, posted.Body.String())
	}
	if invalid := perform(t, handler, http.MethodPost, resource+"/stream?action=bad", "", nil); invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid action = %d", invalid.Code)
	}
	messages := perform(t, handler, http.MethodGet, "/api/runs/"+string(runs[0].ID)+"/messages", "", nil)
	if messages.Code != http.StatusOK || !strings.Contains(messages.Body.String(), `"has_more":false`) {
		t.Fatalf("messages = %d %s", messages.Code, messages.Body.String())
	}
}

func TestStatelessRunCompatibilityAndScoping(t *testing.T) {
	t.Parallel()
	memory := store.NewMemory()
	alice, _ := New(Config{Store: memory, Starter: completingStarter{store: memory}, OwnerResolver: func(context.Context) string { return "alice" }})
	bob, _ := New(Config{Store: memory, Starter: completingStarter{store: memory}, OwnerResolver: func(context.Context) string { return "bob" }})
	stream := perform(t, alice, http.MethodPost, "/api/runs/stream", `{}`, nil)
	if stream.Code != http.StatusOK || !strings.Contains(stream.Header().Get("Content-Location"), "/api/threads/") {
		t.Fatalf("stateless stream = %d %#v", stream.Code, stream.Header())
	}
	threads, err := memory.Threads(context.Background(), store.ThreadQuery{OwnerID: "alice", Limit: 10})
	if err != nil || len(threads) != 1 {
		t.Fatalf("threads = %#v, %v", threads, err)
	}
	body := `{"config":{"configurable":{"thread_id":"` + string(threads[0].ID) + `"}}}`
	waited := perform(t, alice, http.MethodPost, "/api/runs/wait", body, nil)
	if waited.Code != http.StatusOK || waited.Header().Get("Content-Location") == "" {
		t.Fatalf("stateless wait = %d %s", waited.Code, waited.Body.String())
	}
	if response := perform(t, bob, http.MethodPost, "/api/runs/wait", body, nil); response.Code != http.StatusNotFound {
		t.Fatalf("cross-owner wait = %d", response.Code)
	}
	if response := perform(t, alice, http.MethodPost, "/api/runs/wait", `{"config":false}`, nil); response.Code != http.StatusBadRequest {
		t.Fatalf("invalid config = %d", response.Code)
	}
	if response := perform(t, alice, http.MethodPost, "/api/runs/stream", `{} {}`, nil); response.Code != http.StatusBadRequest {
		t.Fatalf("invalid body = %d", response.Code)
	}
}

func TestWaitForTerminalCancellationAndRunMessageErrors(t *testing.T) {
	t.Parallel()
	memory := store.NewMemory()
	handler, _ := New(Config{Store: memory})
	thread, _ := domain.NewThread(time.Now())
	_ = memory.CreateThread(context.Background(), thread)
	run, _ := domain.NewRun(thread.ID, time.Now())
	_ = memory.CreateRun(context.Background(), run)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := handler.waitForTerminal(ctx, run.ID); err == nil {
		t.Fatal("cancelled wait succeeded")
	}
	if response := perform(t, handler, http.MethodGet, "/api/runs/bad/messages", "", nil); response.Code != http.StatusBadRequest {
		t.Fatalf("bad run = %d", response.Code)
	}
	if response := perform(t, handler, http.MethodGet, "/api/runs/"+string(newRunID(t))+"/messages", "", nil); response.Code != http.StatusNotFound {
		t.Fatalf("missing run = %d", response.Code)
	}
}

func TestPostStreamCancellationCompatibility(t *testing.T) {
	t.Parallel()
	memory := store.NewMemory()
	canceller := completingCanceller{store: memory}
	handler, _ := New(Config{Store: memory, Canceller: canceller})
	created := perform(t, handler, http.MethodPost, "/api/threads", `{}`, nil)
	var thread threadResponse
	decodeResponse(t, created, &thread)
	newPendingRun := func() string {
		response := perform(t, handler, http.MethodPost, "/api/threads/"+string(thread.ThreadID)+"/runs", `{}`, nil)
		var run runResponse
		decodeResponse(t, response, &run)
		return "/api/threads/" + string(thread.ThreadID) + "/runs/" + string(run.RunID) + "/stream"
	}
	streamed := perform(t, handler, http.MethodPost, newPendingRun()+"?action=interrupt", "", nil)
	if streamed.Code != http.StatusOK || !strings.Contains(streamed.Body.String(), "event: run.cancelled") {
		t.Fatalf("cancel stream = %d %s", streamed.Code, streamed.Body.String())
	}
	waited := perform(t, handler, http.MethodPost, newPendingRun()+"?action=rollback&wait=1", "", nil)
	if waited.Code != http.StatusNoContent {
		t.Fatalf("cancel wait = %d %s", waited.Code, waited.Body.String())
	}
	if invalid := perform(t, handler, http.MethodPost, newPendingRun()+"?wait=2", "", nil); invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid wait = %d", invalid.Code)
	}
	withoutCanceller, _ := New(Config{Store: memory})
	if response := perform(t, withoutCanceller, http.MethodPost, newPendingRun()+"?action=interrupt", "", nil); response.Code != http.StatusNotImplemented {
		t.Fatalf("missing canceller = %d", response.Code)
	}
	failing, _ := New(Config{Store: memory, Canceller: completingCanceller{store: memory, err: errors.New("cancel failed")}})
	if response := perform(t, failing, http.MethodPost, newPendingRun()+"?action=interrupt", "", nil); response.Code != http.StatusInternalServerError {
		t.Fatalf("failed cancel = %d", response.Code)
	}
}
