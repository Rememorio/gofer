package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/event"
	"github.com/Rememorio/gofer/internal/model"
)

func TestServiceWarnsAndCapsRepeatedToolCalls(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	var warningSeen atomic.Bool
	modelServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		turn := calls.Add(1)
		if turn == 3 {
			warningSeen.Store(bytes.Contains(body, []byte("LOOP DETECTED")))
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		writeSSE(writer,
			toolCallChunk(fmt.Sprintf("list-%d", turn), "ls", `{"path":"/mnt/user-data/workspace","max_depth":1}`),
			doneChunk("tool_calls"),
		)
	}))
	defer modelServer.Close()
	cfg := testConfig(t, modelServer.URL+"/v1")
	cfg.LoopDetection.WarnThreshold = 2
	cfg.LoopDetection.HardLimit = 4
	cfg.LoopDetection.ToolFrequencyWarn = 100
	cfg.LoopDetection.ToolFrequencyLimit = 200
	service, err := New(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = service.Close() }()
	server := httptest.NewServer(service.Handler())
	defer server.Close()

	threadID := createThread(t, server.URL, "")
	runID := createRun(t, server.URL, threadID, `{"input":{"messages":[{"role":"user","content":"list files"}]}}`, "")
	waitRun(t, server.URL, threadID, runID, domain.RunSucceeded, "")
	if calls.Load() != 4 || !warningSeen.Load() {
		t.Fatalf("model calls=%d warning_seen=%v", calls.Load(), warningSeen.Load())
	}
	records, err := service.store.Events(context.Background(), runID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	toolCompletions := 0
	stopReason := modelStopReason(records)
	for _, record := range records {
		if record.Kind == event.ToolCompleted {
			toolCompletions++
		}
	}
	if toolCompletions != 3 || stopReason != string(model.StopLoopCapped) {
		t.Fatalf("tool completions=%d stop_reason=%q", toolCompletions, stopReason)
	}
	messages := resourceRequest[[]domain.Message](t, server.URL, http.MethodGet,
		"/api/threads/"+string(threadID)+"/messages", nil, "", http.StatusOK)
	userMessages := 0
	for _, message := range messages {
		if message.Role == domain.RoleUser {
			userMessages++
		}
	}
	last := messages[len(messages)-1]
	if userMessages != 1 || last.Role != domain.RoleAssistant || !strings.Contains(last.Content[0].Text, "FORCED STOP") {
		t.Fatalf("durable messages = %#v", messages)
	}
}

func modelStopReason(records []event.Event) string {
	for index := len(records) - 1; index >= 0; index-- {
		if records[index].Kind != event.RunCompleted {
			continue
		}
		var payload struct {
			StopReason string `json:"stop_reason"`
		}
		if json.Unmarshal(records[index].Data, &payload) == nil {
			return payload.StopReason
		}
	}
	return ""
}
