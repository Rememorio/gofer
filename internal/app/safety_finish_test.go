package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/event"
	"github.com/Rememorio/gofer/internal/model"
	"github.com/Rememorio/gofer/internal/usage"
)

type safetyFinishCase struct {
	name       string
	chunks     []string
	wantText   string
	suppressed bool
}

func TestServiceRepairsProviderSafetyTerminations(t *testing.T) {
	t.Parallel()
	tests := []safetyFinishCase{
		{
			name: "visible text", chunks: []string{
				textChunk("I cannot help with that."), doneChunk("content_filter"),
			}, wantText: "I cannot help with that.",
		},
		{
			name: "empty", chunks: []string{doneChunk("content_filter")},
			wantText: "returned no content",
		},
		{
			name: "tool intent", chunks: []string{
				textChunk("partial unsafe output"),
				toolCallChunk("write", "write_file", `{"path":"/mnt/user-data/workspace/partial.txt","content":"unfinished"}`),
				doneChunk("content_filter"),
			}, wantText: "suppressed", suppressed: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			runSafetyFinishCase(t, test)
		})
	}
}

func runSafetyFinishCase(t *testing.T, test safetyFinishCase) {
	t.Helper()
	modelServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writeSSE(writer, test.chunks...)
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
	runID := createRun(t, server.URL, threadID, `{"input":{"messages":[{"role":"user","content":"request"}]}}`, "")
	waitRun(t, server.URL, threadID, runID, domain.RunSucceeded, "")
	records, _ := service.store.Events(context.Background(), runID, 0, 0)
	if modelStopReason(records) != string(model.StopSafetyCapped) || usage.Summarize(records).StopReason != string(model.StopSafetyCapped) {
		t.Fatalf("stop=%q usage=%#v", modelStopReason(records), usage.Summarize(records))
	}
	assertNoToolEvents(t, records)
	messages := resourceRequest[[]domain.Message](t, server.URL, http.MethodGet,
		"/api/threads/"+string(threadID)+"/messages", nil, "", http.StatusOK)
	last := messages[len(messages)-1]
	if last.Role != domain.RoleAssistant || last.Metadata["internal_kind"] != "safety_termination" ||
		!strings.Contains(last.Content[0].Text, test.wantText) {
		t.Fatalf("last message = %#v", last)
	}
	if test.suppressed && hasToolCallContent(last) {
		t.Fatalf("suppressed tool persisted: %#v", last)
	}
}

func assertNoToolEvents(t *testing.T, records []event.Event) {
	t.Helper()
	for _, record := range records {
		if record.Kind == event.ToolStarted || record.Kind == event.ToolCompleted || record.Kind == event.ToolFailed {
			t.Fatalf("safety-capped tool reached execution: %#v", record)
		}
	}
}

func hasToolCallContent(message domain.Message) bool {
	for _, content := range message.Content {
		if content.Kind == domain.ContentToolCall {
			return true
		}
	}
	return false
}
