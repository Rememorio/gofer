package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Rememorio/gofer/internal/config"
	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/humaninput"
)

func TestServicePersistsAndResumesStructuredClarification(t *testing.T) {
	var calls atomic.Int32
	var requestMu sync.Mutex
	requests := make([][]byte, 0, 2)
	modelServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		requestMu.Lock()
		requests = append(requests, append([]byte(nil), body...))
		requestMu.Unlock()
		writer.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			writeSSE(writer,
				multipleToolCallsChunk(
					toolChunkSpec{Index: 0, ID: "write", Name: "write_file", Arguments: `{"path":"/mnt/user-data/workspace/unsafe.txt","content":"must not run"}`},
					toolChunkSpec{Index: 1, ID: "ask", Name: humaninput.ToolName, Arguments: `{"question":"Which environment?","clarification_type":"approach_choice","context":"Deployment differs by environment.","options":["Staging","Production"]}`},
				),
				doneChunk("tool_calls"),
			)
			return
		}
		writeSSE(writer, textChunk("Deploying to Staging."), doneChunk("stop"))
	}))
	defer modelServer.Close()

	cfg := testConfig(t, modelServer.URL+"/v1")
	cfg.Storage = config.StorageConfig{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "gofer.db")}
	service, err := New(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(service.Handler())
	threadID := createThread(t, server.URL, "")
	firstRun := createRun(t, server.URL, threadID, `{"input":{"messages":[{"role":"user","content":"deploy it"}]}}`, "")
	waitRun(t, server.URL, threadID, firstRun, domain.RunInterrupted, "")
	state := fetchHumanInputState(t, server.URL, threadID)
	if len(state.OpenRequests) != 1 || state.LatestOpenRequestID != "clarification:ask" ||
		state.OpenRequests[0].Options[0].Value != "Staging" {
		t.Fatalf("human input state = %#v", state)
	}
	unsafePath := filepath.Join(cfg.Workspace.Root, "threads", string(threadID), "user-data", "workspace", "unsafe.txt")
	if exists(t, unsafePath) {
		t.Fatalf("sibling write tool executed: %s", unsafePath)
	}
	server.Close()
	if err = service.Close(); err != nil {
		t.Fatal(err)
	}

	// The pending card is journal-derived and survives a full service restart.
	service, err = New(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = service.Close() }()
	server = httptest.NewServer(service.Handler())
	defer server.Close()
	state = fetchHumanInputState(t, server.URL, threadID)
	if state.LatestOpenRequestID != "clarification:ask" {
		t.Fatalf("state after restart = %#v", state)
	}
	response := humaninput.Response{
		Version: 1, Kind: "human_input_response", Source: humaninput.ToolName,
		RequestID: state.LatestOpenRequestID, ResponseKind: "option", OptionID: "option-1", Value: "Staging",
	}
	secondRun := createRun(t, server.URL, threadID, responseRunBody(t, "Staging", response), "")
	waitRun(t, server.URL, threadID, secondRun, domain.RunSucceeded, "")
	state = fetchHumanInputState(t, server.URL, threadID)
	if len(state.OpenRequests) != 0 || state.AnsweredResponses[response.RequestID].Value != "Staging" || calls.Load() != 2 {
		t.Fatalf("answered state/calls = %#v / %d", state, calls.Load())
	}
	requestMu.Lock()
	secondModelRequest := append([]byte(nil), requests[1]...)
	requestMu.Unlock()
	if !bytes.Contains(secondModelRequest, []byte("Which environment?")) || !bytes.Contains(secondModelRequest, []byte("Staging")) {
		t.Fatalf("resumed model request = %s", secondModelRequest)
	}
	messages := resourceRequest[[]domain.Message](t, server.URL, http.MethodGet,
		"/api/threads/"+string(threadID)+"/messages", nil, "", http.StatusOK)
	if len(messages) < 5 || messages[len(messages)-2].Metadata[humaninput.ResponseMetadataKey] == "" {
		t.Fatalf("durable messages = %#v", messages)
	}

	// A replay is rejected before another model call and does not alter history.
	replayed := createRun(t, server.URL, threadID, responseRunBody(t, "Staging", response), "")
	failed := waitRun(t, server.URL, threadID, replayed, domain.RunFailed, "")
	if !strings.Contains(failed.Error, "already answered") || calls.Load() != 2 {
		t.Fatalf("replay = %#v, calls=%d", failed, calls.Load())
	}
}

func TestServiceCanDisableClarificationForNonInteractiveRuns(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	var requestMu sync.Mutex
	var secondRequest []byte
	modelServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		writer.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			writeSSE(writer,
				toolCallChunk("ask", humaninput.ToolName, `{"question":"Can I continue?","clarification_type":"suggestion"}`),
				doneChunk("tool_calls"),
			)
			return
		}
		requestMu.Lock()
		secondRequest = append([]byte(nil), body...)
		requestMu.Unlock()
		writeSSE(writer, textChunk("I proceeded with an explicit assumption."), doneChunk("stop"))
	}))
	defer modelServer.Close()
	service, err := New(context.Background(), testConfig(t, modelServer.URL+"/v1"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = service.Close() }()
	server := httptest.NewServer(service.Handler())
	defer server.Close()
	threadID := createThread(t, server.URL, "")
	runID := createRun(t, server.URL, threadID, `{"input":{"messages":[{"role":"user","content":"process webhook"}]},"context":{"disable_clarification":true}}`, "")
	waitRun(t, server.URL, threadID, runID, domain.RunSucceeded, "")
	requestMu.Lock()
	requestCopy := append([]byte(nil), secondRequest...)
	requestMu.Unlock()
	if calls.Load() != 2 || !bytes.Contains(requestCopy, []byte("best judgment")) {
		t.Fatalf("calls/request = %d / %s", calls.Load(), requestCopy)
	}
	state := fetchHumanInputState(t, server.URL, threadID)
	if len(state.OpenRequests) != 0 {
		t.Fatalf("disabled state = %#v", state)
	}
}

type toolChunkSpec struct {
	Index     int
	ID        string
	Name      string
	Arguments string
}

func multipleToolCallsChunk(calls ...toolChunkSpec) string {
	type function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}
	type wireCall struct {
		Index    int      `json:"index"`
		ID       string   `json:"id"`
		Type     string   `json:"type"`
		Function function `json:"function"`
	}
	wire := make([]wireCall, len(calls))
	for index, call := range calls {
		wire[index] = wireCall{Index: call.Index, ID: call.ID, Type: "function", Function: function{Name: call.Name, Arguments: call.Arguments}}
	}
	payload := map[string]any{
		"id": "multi", "object": "chat.completion.chunk", "created": 1, "model": "gpt-test",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": wire}, "finish_reason": nil}},
	}
	data, _ := json.Marshal(payload)
	return string(data)
}

func responseRunBody(t *testing.T, text string, response humaninput.Response) string {
	t.Helper()
	payload := map[string]any{"input": map[string]any{"messages": []any{map[string]any{
		"role": "user", "content": text,
		"additional_kwargs": map[string]any{"hide_from_ui": true, humaninput.ResponseMetadataKey: response},
	}}}}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func fetchHumanInputState(t *testing.T, baseURL string, threadID domain.ThreadID) humaninput.ThreadState {
	t.Helper()
	return resourceRequest[humaninput.ThreadState](t, baseURL, http.MethodGet,
		"/api/threads/"+string(threadID)+"/human-input", nil, "", http.StatusOK)
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	matches, err := filepath.Glob(path)
	if err != nil {
		t.Fatal(err)
	}
	return len(matches) > 0
}
