package toolhistory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/model"
)

func TestMiddlewareRepairsDanglingCallsAndOrphans(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	middleware := newMiddleware(t, now)
	user := textMessage(t, "start", now.Add(-time.Minute))
	assistant := assistantMessage(t, now.Add(-50*time.Second),
		toolCall("first", "read_file"), toolCall("second", "ls"))
	lateSecond := resultMessage(t, "second", false, now.Add(-40*time.Second))
	orphan := resultMessage(t, "orphan", false, now.Add(-30*time.Second))
	followup := textMessage(t, "continue", now.Add(-time.Second))
	original := []domain.Message{user, assistant, followup, orphan, lateSecond}
	originalJSON, _ := json.Marshal(original)
	request := model.Request{Messages: append([]domain.Message(nil), original...)}

	if err := middleware.BeforeModel(context.Background(), &request); err != nil {
		t.Fatal(err)
	}
	if len(request.Messages) != 5 {
		t.Fatalf("message count = %d", len(request.Messages))
	}
	if request.Messages[0].ID != user.ID || request.Messages[1].ID != assistant.ID ||
		request.Messages[4].ID != followup.ID {
		t.Fatalf("message ordering = %#v", messageIDs(request.Messages))
	}
	first := mustResult(t, request.Messages[2])
	second := mustResult(t, request.Messages[3])
	if first.CallID != "first" || !first.IsError || second.CallID != "second" || second.IsError {
		t.Fatalf("results = %#v, %#v", first, second)
	}
	if request.Messages[3].ID != lateSecond.ID || request.Messages[2].Metadata["internal_kind"] != "tool_result_recovery" {
		t.Fatalf("repaired messages = %#v", request.Messages)
	}
	if !containsJSON(t, first.Output, "code", "interrupted_tool_call") {
		t.Fatalf("synthetic output = %s", first.Output)
	}
	afterJSON, _ := json.Marshal(original)
	if !bytes.Equal(originalJSON, afterJSON) {
		t.Fatal("source messages changed")
	}
}

func TestMiddlewarePreservesValidTranscript(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	middleware := newMiddleware(t, now)
	assistant := assistantMessage(t, now, toolCall("one", "ls"), toolCall("two", "read_file"))
	one := resultMessage(t, "one", false, now)
	two := resultMessage(t, "two", true, now)
	request := model.Request{Messages: []domain.Message{assistant, one, two}}
	want := append([]domain.Message(nil), request.Messages...)

	if err := middleware.BeforeModel(context.Background(), &request); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(request.Messages, want) {
		t.Fatalf("messages changed = %#v", request.Messages)
	}
}

func TestMiddlewareRepairsEveryAssistantTurnAndDropsExcessResults(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	middleware := newMiddleware(t, now)
	first := assistantMessage(t, now, toolCall("shared", "ls"))
	second := assistantMessage(t, now, toolCall("shared", "ls"))
	firstResult := resultMessage(t, "shared", false, now)
	secondResult := resultMessage(t, "shared", true, now)
	excess := resultMessage(t, "shared", false, now)
	request := model.Request{Messages: []domain.Message{first, second, excess, secondResult, firstResult}}

	if err := middleware.BeforeModel(context.Background(), &request); err != nil {
		t.Fatal(err)
	}
	if len(request.Messages) != 4 || request.Messages[0].ID != first.ID || request.Messages[2].ID != second.ID {
		t.Fatalf("messages = %#v", messageIDs(request.Messages))
	}
	if request.Messages[1].ID != excess.ID || request.Messages[3].ID != secondResult.ID {
		t.Fatalf("result queue order = %#v", messageIDs(request.Messages))
	}
}

func TestMiddlewareRepairIsIdempotent(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	middleware := newMiddleware(t, now)
	request := model.Request{Messages: []domain.Message{assistantMessage(t, now, toolCall("missing", "ls"))}}
	if err := middleware.BeforeModel(context.Background(), &request); err != nil {
		t.Fatal(err)
	}
	want := append([]domain.Message(nil), request.Messages...)
	if err := middleware.BeforeModel(context.Background(), &request); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(request.Messages, want) {
		t.Fatalf("second repair = %#v", request.Messages)
	}
}

func TestMiddlewareBoundsAndCancellation(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	config := DefaultConfig()
	config.MaxMessages = 1
	config.MaxToolCalls = 1
	config.Now = func() time.Time { return now }
	middleware, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	request := model.Request{Messages: []domain.Message{
		textMessage(t, "one", now), textMessage(t, "two", now),
	}}
	if err = middleware.BeforeModel(context.Background(), &request); !errors.Is(err, ErrHistoryLimit) {
		t.Fatalf("message bound error = %v", err)
	}
	request.Messages = []domain.Message{assistantMessage(t, now, toolCall("one", "ls"), toolCall("two", "ls"))}
	if err = middleware.BeforeModel(context.Background(), &request); !errors.Is(err, ErrHistoryLimit) {
		t.Fatalf("call bound error = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err = middleware.BeforeModel(cancelled, &model.Request{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestNewAndBeforeModelRejectInvalidInputs(t *testing.T) {
	t.Parallel()
	for _, config := range []Config{
		{},
		{MaxMessages: -1, MaxToolCalls: 1},
		{MaxMessages: 1, MaxToolCalls: maximumBoundary + 1},
	} {
		if _, err := New(config); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("New(%#v) error = %v", config, err)
		}
	}
	middleware := newMiddleware(t, time.Now().UTC())
	if err := middleware.BeforeModel(context.Background(), nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil request error = %v", err)
	}
	var missing *Middleware
	if err := missing.BeforeModel(context.Background(), &model.Request{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil middleware error = %v", err)
	}
}

func newMiddleware(t *testing.T, now time.Time) *Middleware {
	t.Helper()
	config := DefaultConfig()
	config.Now = func() time.Time { return now }
	middleware, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return middleware
}

func textMessage(t *testing.T, text string, at time.Time) domain.Message {
	t.Helper()
	message, err := domain.NewTextMessage(domain.RoleUser, text, at)
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func assistantMessage(t *testing.T, at time.Time, calls ...domain.ToolCall) domain.Message {
	t.Helper()
	id, err := domain.NewMessageID()
	if err != nil {
		t.Fatal(err)
	}
	contents := make([]domain.Content, len(calls))
	for index := range calls {
		call := calls[index]
		contents[index] = domain.Content{Kind: domain.ContentToolCall, ToolCall: &call}
	}
	message := domain.Message{ID: id, Role: domain.RoleAssistant, Content: contents, CreatedAt: at}
	if err := message.Validate(); err != nil {
		t.Fatal(err)
	}
	return message
}

func toolCall(id, name string) domain.ToolCall {
	return domain.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(`{}`)}
}

func resultMessage(t *testing.T, callID string, failed bool, at time.Time) domain.Message {
	t.Helper()
	id, err := domain.NewMessageID()
	if err != nil {
		t.Fatal(err)
	}
	result := domain.ToolResult{CallID: callID, Output: json.RawMessage(`{"ok":true}`), IsError: failed}
	message := domain.Message{ID: id, Role: domain.RoleTool, Content: []domain.Content{{Kind: domain.ContentToolResult, ToolResult: &result}}, CreatedAt: at}
	if err := message.Validate(); err != nil {
		t.Fatal(err)
	}
	return message
}

func mustResult(t *testing.T, message domain.Message) domain.ToolResult {
	t.Helper()
	result, ok := messageToolResult(message)
	if !ok {
		t.Fatalf("message is not a tool result: %#v", message)
	}
	return result
}

func containsJSON(t *testing.T, raw json.RawMessage, key, want string) bool {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	return value[key] == want
}

func messageIDs(messages []domain.Message) []domain.MessageID {
	ids := make([]domain.MessageID, len(messages))
	for index := range messages {
		ids[index] = messages[index].ID
	}
	return ids
}
