package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/event"
	"github.com/Rememorio/gofer/internal/humaninput"
	"github.com/Rememorio/gofer/internal/model"
	"github.com/Rememorio/gofer/internal/store"
)

func TestInterruptedHumanInputSettlesCompatibilityEndpoints(t *testing.T) {
	t.Parallel()
	memory := store.NewMemory()
	now := time.Now().UTC()
	thread, _ := domain.NewThread(now)
	requireNoError(t, memory.CreateThread(context.Background(), thread))
	run, _ := domain.NewRun(thread.ID, now)
	requireNoError(t, memory.CreateRun(context.Background(), run))
	created, _ := event.NewDraft(thread.ID, run.ID, event.RunCreated, now, nil)
	_, err := memory.Append(context.Background(), run.ID, 0, created)
	requireNoError(t, err)
	run, _ = memory.TransitionRun(context.Background(), run.ID, domain.RunPending, domain.RunRunning, now.Add(time.Second), "")
	request := persistClarification(t, memory, run, now.Add(2*time.Second))
	run, _ = memory.TransitionRun(context.Background(), run.ID, domain.RunRunning, domain.RunInterrupted, now.Add(3*time.Second), "")
	records, _ := memory.Events(context.Background(), run.ID, 0, 0)
	interrupted, _ := event.NewDraft(thread.ID, run.ID, event.RunInterrupted, now.Add(3*time.Second), map[string]any{"stop_reason": model.StopHumanInput})
	_, err = memory.Append(context.Background(), run.ID, records[len(records)-1].Sequence, interrupted)
	requireNoError(t, err)
	handler, err := New(Config{Store: memory, KeepAlive: time.Millisecond})
	requireNoError(t, err)
	base := "/api/threads/" + string(thread.ID)
	stream := perform(t, handler, http.MethodGet, base+"/runs/"+string(run.ID)+"/stream", "", nil)
	assertOKResponse(t, stream, "event: run.interrupted")
	settled, err := handler.waitForTerminal(context.Background(), run.ID)
	requireNoError(t, err)
	assertRunStatus(t, settled, domain.RunInterrupted)
	state := perform(t, handler, http.MethodGet, base+"/state", "", nil)
	assertOKResponse(t, state, `"next":["human_input"]`, request.RequestID)
	humanState := perform(t, handler, http.MethodGet, base+"/human-input", "", nil)
	assertOKResponse(t, humanState, `"latest_open_request_id":"`+request.RequestID+`"`)
	threadResource := perform(t, handler, http.MethodGet, base, "", nil)
	assertOKResponse(t, threadResource, `"status":"interrupted"`, `"interrupts":{"human_input"`)
	resource := perform(t, handler, http.MethodGet, base+"/runs/"+string(run.ID), "", nil)
	assertOKResponse(t, resource, `"status":"interrupted"`, `"stop_reason":"human_input"`)
	deleted := perform(t, handler, http.MethodDelete, base, "", nil)
	assertOKResponse(t, deleted)
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func assertRunStatus(t *testing.T, run domain.Run, want domain.RunStatus) {
	t.Helper()
	if run.Status != want {
		t.Fatalf("run status = %s, want %s", run.Status, want)
	}
}

func assertOKResponse(t *testing.T, response *httptest.ResponseRecorder, contains ...string) {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	for _, fragment := range contains {
		if !strings.Contains(response.Body.String(), fragment) {
			t.Fatalf("response does not contain %q: %s", fragment, response.Body.String())
		}
	}
}

func persistClarification(t *testing.T, memory *store.Memory, run domain.Run, at time.Time) humaninput.Request {
	t.Helper()
	call := domain.ToolCall{
		ID: "ask", Name: humaninput.ToolName,
		Arguments: json.RawMessage(`{"question":"Which environment?","clarification_type":"missing_info"}`),
	}
	request, fallback, err := humaninput.BuildRequest(call)
	if err != nil {
		t.Fatal(err)
	}
	output, err := humaninput.MarshalToolOutput(request, fallback)
	if err != nil {
		t.Fatal(err)
	}
	assistant := domain.Message{ID: newMessageID(t), Role: domain.RoleAssistant, CreatedAt: at,
		Content: []domain.Content{{Kind: domain.ContentToolCall, ToolCall: &call}}}
	result := domain.ToolResult{CallID: call.ID, Output: output, Interrupt: true}
	toolMessage := domain.Message{ID: newMessageID(t), Role: domain.RoleTool, CreatedAt: at.Add(time.Nanosecond),
		Content: []domain.Content{{Kind: domain.ContentToolResult, ToolResult: &result}}}
	first, _ := event.NewDraft(run.ThreadID, run.ID, event.MessageCompleted, at, map[string]any{"message": assistant})
	second, _ := event.NewDraft(run.ThreadID, run.ID, event.ToolCompleted, at.Add(time.Nanosecond), map[string]any{"call": call, "result": result, "message": toolMessage})
	if _, err = memory.Append(context.Background(), run.ID, 1, first, second); err != nil {
		t.Fatal(err)
	}
	return request
}

func newMessageID(t *testing.T) domain.MessageID {
	t.Helper()
	id, err := domain.NewMessageID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
