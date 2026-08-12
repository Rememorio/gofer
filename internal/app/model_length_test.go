package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/event"
	"github.com/Rememorio/gofer/internal/model"
	"github.com/Rememorio/gofer/internal/usage"
)

func TestServicePreservesVisibleLengthCappedResponse(t *testing.T) {
	t.Parallel()
	modelServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writeSSE(writer, textChunk("Partial answer with useful evidence."), doneChunk("length"))
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
	runID := createRun(t, server.URL, threadID, `{"input":{"messages":[{"role":"user","content":"write a long answer"}]}}`, "")
	waitRun(t, server.URL, threadID, runID, domain.RunSucceeded, "")
	records, _ := service.store.Events(context.Background(), runID, 0, 0)
	if modelStopReason(records) != string(model.StopModelLengthCapped) || usage.Summarize(records).StopReason != string(model.StopModelLengthCapped) {
		t.Fatalf("run stop=%q usage=%#v", modelStopReason(records), usage.Summarize(records))
	}
	messages := resourceRequest[[]domain.Message](t, server.URL, http.MethodGet,
		"/api/threads/"+string(threadID)+"/messages", nil, "", http.StatusOK)
	if len(messages) != 2 || messages[1].Content[0].Text != "Partial answer with useful evidence." {
		t.Fatalf("messages = %#v", messages)
	}
}

func TestServiceRejectsUnsafeLengthCappedResponses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		chunks []string
	}{
		{name: "empty", chunks: []string{doneChunk("length")}},
		{name: "tool intent", chunks: []string{
			textChunk("partial"),
			toolCallChunk("write", "write_file", `{"path":"/mnt/user-data/workspace/partial.txt","content":"unfinished"}`),
			doneChunk("length"),
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
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
			runID := createRun(t, server.URL, threadID, `{"input":{"messages":[{"role":"user","content":"write"}]}}`, "")
			waitRun(t, server.URL, threadID, runID, domain.RunFailed, "")
			records, _ := service.store.Events(context.Background(), runID, 0, 0)
			for _, record := range records {
				if record.Kind == event.ToolStarted || record.Kind == event.ToolCompleted || record.Kind == event.ToolFailed {
					t.Fatalf("length-capped tool reached execution: %#v", record)
				}
			}
		})
	}
}
