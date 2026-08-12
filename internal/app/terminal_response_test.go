package app

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/event"
	"github.com/Rememorio/gofer/internal/model"
	"github.com/Rememorio/gofer/internal/usage"
)

func TestServiceRetriesEmptyPostToolResponse(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	var reminderSeen atomic.Bool
	modelServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body := readRequestBody(request)
		turn := calls.Add(1)
		writer.Header().Set("Content-Type", "text/event-stream")
		switch turn {
		case 1:
			writeSSE(writer, toolCallChunk("list", "ls", `{"path":"/mnt/user-data/workspace","max_depth":1}`), doneChunk("tool_calls"))
		case 2:
			writeSSE(writer, doneChunk("stop"))
		default:
			reminderSeen.Store(bytes.Contains(body, []byte("previous response after tool execution was empty")))
			writeSSE(writer, textChunk("Recovered final answer."), doneChunk("stop"))
		}
	}))
	defer modelServer.Close()
	service, server := terminalResponseService(t, modelServer.URL+"/v1", 5)
	defer func() { _ = service.Close() }()
	defer server.Close()

	threadID := createThread(t, server.URL, "")
	runID := createRun(t, server.URL, threadID, `{"input":{"messages":[{"role":"user","content":"list files"}]}}`, "")
	waitRun(t, server.URL, threadID, runID, domain.RunSucceeded, "")
	if calls.Load() != 3 || !reminderSeen.Load() {
		t.Fatalf("calls=%d reminder=%v", calls.Load(), reminderSeen.Load())
	}
	records, _ := service.store.Events(context.Background(), runID, 0, 0)
	if countRetryEvents(records) != 1 || usage.Summarize(records).LLMCallCount != 3 {
		t.Fatalf("events=%#v usage=%#v", records, usage.Summarize(records))
	}
	messages := resourceRequest[[]domain.Message](t, server.URL, http.MethodGet,
		"/api/threads/"+string(threadID)+"/messages", nil, "", http.StatusOK)
	if len(messages) != 4 || messages[len(messages)-1].Content[0].Text != "Recovered final answer." {
		t.Fatalf("messages = %#v", messages)
	}
	for _, message := range messages {
		if message.Metadata["internal_kind"] == "terminal_response_recovery" {
			t.Fatalf("recovery reminder persisted: %#v", message)
		}
	}
}

func TestServiceFailsVisiblyAfterSecondEmptyPostToolResponse(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	modelServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		turn := calls.Add(1)
		writer.Header().Set("Content-Type", "text/event-stream")
		if turn == 1 {
			writeSSE(writer, toolCallChunk("list", "ls", `{"path":"/mnt/user-data/workspace","max_depth":1}`), doneChunk("tool_calls"))
			return
		}
		writeSSE(writer, doneChunk("stop"))
	}))
	defer modelServer.Close()
	service, server := terminalResponseService(t, modelServer.URL+"/v1", 5)
	defer func() { _ = service.Close() }()
	defer server.Close()

	threadID := createThread(t, server.URL, "")
	runID := createRun(t, server.URL, threadID, `{"input":{"messages":[{"role":"user","content":"list files"}]}}`, "")
	run := waitRun(t, server.URL, threadID, runID, domain.RunFailed, "")
	if calls.Load() != 3 || !strings.Contains(run.Error, "no final response") {
		t.Fatalf("calls=%d run=%#v", calls.Load(), run)
	}
	records, _ := service.store.Events(context.Background(), runID, 0, 0)
	if countRetryEvents(records) != 1 || failedStopReason(records) != string(model.StopTerminalError) || usage.Summarize(records).LLMCallCount != 3 {
		t.Fatalf("retry=%d stop=%q usage=%#v", countRetryEvents(records), failedStopReason(records), usage.Summarize(records))
	}
	messages := resourceRequest[[]domain.Message](t, server.URL, http.MethodGet,
		"/api/threads/"+string(threadID)+"/messages", nil, "", http.StatusOK)
	last := messages[len(messages)-1]
	if last.Role != domain.RoleAssistant || last.Metadata["internal_kind"] != "terminal_response_fallback" ||
		!strings.Contains(last.Content[0].Text, "no final response") {
		t.Fatalf("last message = %#v", last)
	}
}

func TestServiceFallsBackWithoutExceedingTurnLimit(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	modelServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		turn := calls.Add(1)
		writer.Header().Set("Content-Type", "text/event-stream")
		if turn == 1 {
			writeSSE(writer, toolCallChunk("list", "ls", `{"path":"/mnt/user-data/workspace","max_depth":1}`), doneChunk("tool_calls"))
			return
		}
		writeSSE(writer, doneChunk("stop"))
	}))
	defer modelServer.Close()
	service, server := terminalResponseService(t, modelServer.URL+"/v1", 2)
	defer func() { _ = service.Close() }()
	defer server.Close()

	threadID := createThread(t, server.URL, "")
	runID := createRun(t, server.URL, threadID, `{"input":{"messages":[{"role":"user","content":"list files"}]}}`, "")
	run := waitRun(t, server.URL, threadID, runID, domain.RunFailed, "")
	records, _ := service.store.Events(context.Background(), runID, 0, 0)
	if calls.Load() != 2 || countRetryEvents(records) != 0 || !strings.Contains(run.Error, "no final response") {
		t.Fatalf("calls=%d retries=%d run=%#v", calls.Load(), countRetryEvents(records), run)
	}
}

func terminalResponseService(t *testing.T, baseURL string, maxTurns int) (*Service, *httptest.Server) {
	t.Helper()
	cfg := testConfig(t, baseURL)
	cfg.Runtime.MaxTurns = maxTurns
	service, err := New(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	return service, httptest.NewServer(service.Handler())
}

func readRequestBody(request *http.Request) []byte {
	defer func() { _ = request.Body.Close() }()
	payload, _ := io.ReadAll(request.Body)
	return payload
}

func countRetryEvents(records []event.Event) int {
	count := 0
	for _, record := range records {
		if record.Kind == event.ModelRetry {
			count++
		}
	}
	return count
}

func failedStopReason(records []event.Event) string {
	for _, record := range records {
		if record.Kind != event.RunFailed {
			continue
		}
		var payload struct {
			StopReason string `json:"stop_reason"`
		}
		if event.Decode(record, &payload) == nil {
			return payload.StopReason
		}
	}
	return ""
}
