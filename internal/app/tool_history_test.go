package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/conversation"
	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/event"
)

func TestServiceRepairsInterruptedToolHistoryOnlyForModelRequest(t *testing.T) {
	t.Parallel()
	requests := make(chan []byte, 1)
	modelServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		requests <- body
		writer.Header().Set("Content-Type", "text/event-stream")
		writeSSE(writer, textChunk("recovered"), doneChunk("stop"))
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
	seedInterruptedToolHistory(t, service, threadID)
	runID := createRun(t, server.URL, threadID, `{"input":{"messages":[{"role":"user","content":"continue"}]}}`, "")
	waitRun(t, server.URL, threadID, runID, domain.RunSucceeded, "")

	assertRepairedModelRequest(t, <-requests)
	messages := resourceRequest[[]domain.Message](t, server.URL, http.MethodGet,
		"/api/threads/"+string(threadID)+"/messages", nil, "", http.StatusOK)
	toolMessages := 0
	for _, message := range messages {
		if message.Metadata["internal_kind"] == "tool_result_recovery" {
			t.Fatalf("synthetic result persisted: %#v", message)
		}
		if message.Role == domain.RoleTool {
			toolMessages++
		}
	}
	if toolMessages != 1 {
		t.Fatalf("durable tool messages = %d", toolMessages)
	}
}

func seedInterruptedToolHistory(t *testing.T, service *Service, threadID domain.ThreadID) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Add(-time.Minute)
	run, err := domain.NewRun(threadID, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = service.store.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if _, err = service.store.TransitionRun(ctx, run.ID, domain.RunPending, domain.RunRunning, now.Add(time.Second), ""); err != nil {
		t.Fatal(err)
	}
	user, _ := domain.NewTextMessage(domain.RoleUser, "start", now.Add(2*time.Second))
	if err = conversation.PersistInputs(ctx, service.store, threadID, run.ID, []domain.Message{user}); err != nil {
		t.Fatal(err)
	}
	assistant := interruptedAssistantMessage(t, "dangling", now.Add(3*time.Second))
	appendHistoryEvent(t, service, run.ID, threadID, event.MessageCompleted, assistant.CreatedAt, map[string]any{"message": assistant})
	orphan := interruptedResultMessage(t, "orphan", now.Add(4*time.Second))
	appendHistoryEvent(t, service, run.ID, threadID, event.ToolCompleted, orphan.CreatedAt, map[string]any{"message": orphan})
	if _, err = service.store.TransitionRun(ctx, run.ID, domain.RunRunning, domain.RunCancelled, now.Add(5*time.Second), ""); err != nil {
		t.Fatal(err)
	}
}

func appendHistoryEvent(t *testing.T, service *Service, runID domain.RunID, threadID domain.ThreadID, kind event.Kind, at time.Time, payload any) {
	t.Helper()
	records, err := service.store.Events(context.Background(), runID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	sequence := uint64(0)
	if len(records) != 0 {
		sequence = records[len(records)-1].Sequence
	}
	draft, err := event.NewDraft(threadID, runID, kind, at, payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.store.Append(context.Background(), runID, sequence, draft); err != nil {
		t.Fatal(err)
	}
}

func interruptedAssistantMessage(t *testing.T, callID string, at time.Time) domain.Message {
	t.Helper()
	id, err := domain.NewMessageID()
	if err != nil {
		t.Fatal(err)
	}
	call := domain.ToolCall{ID: callID, Name: "ls", Arguments: json.RawMessage(`{"path":"/mnt/user-data/workspace"}`)}
	message := domain.Message{ID: id, Role: domain.RoleAssistant, CreatedAt: at,
		Content: []domain.Content{{Kind: domain.ContentToolCall, ToolCall: &call}}}
	if err = message.Validate(); err != nil {
		t.Fatal(err)
	}
	return message
}

func interruptedResultMessage(t *testing.T, callID string, at time.Time) domain.Message {
	t.Helper()
	id, err := domain.NewMessageID()
	if err != nil {
		t.Fatal(err)
	}
	result := domain.ToolResult{CallID: callID, Output: json.RawMessage(`{"ok":true}`)}
	message := domain.Message{ID: id, Role: domain.RoleTool, CreatedAt: at,
		Content: []domain.Content{{Kind: domain.ContentToolResult, ToolResult: &result}}}
	if err = message.Validate(); err != nil {
		t.Fatal(err)
	}
	return message
}

func assertRepairedModelRequest(t *testing.T, raw []byte) {
	t.Helper()
	var request struct {
		Messages []struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			ToolCallID string          `json:"tool_call_id"`
			ToolCalls  []struct {
				ID string `json:"id"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	toolMessages := 0
	for index, message := range request.Messages {
		if message.Role != "assistant" || len(message.ToolCalls) != 1 || message.ToolCalls[0].ID != "dangling" {
			continue
		}
		if index+1 >= len(request.Messages) {
			t.Fatal("dangling assistant has no following result")
		}
		result := request.Messages[index+1]
		var content string
		if result.Role != "tool" || result.ToolCallID != "dangling" || json.Unmarshal(result.Content, &content) != nil || !json.Valid([]byte(content)) {
			t.Fatalf("recovery result = %#v", result)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(content), &payload); err != nil || payload["code"] != "interrupted_tool_call" {
			t.Fatalf("recovery payload = %#v, %v", payload, err)
		}
	}
	for _, message := range request.Messages {
		if message.Role == "tool" {
			toolMessages++
			if message.ToolCallID == "orphan" {
				t.Fatal("orphan result reached model")
			}
		}
	}
	if toolMessages != 1 {
		t.Fatalf("model-bound tool messages = %d", toolMessages)
	}
}
