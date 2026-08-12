package terminalresponse

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/model"
	"github.com/Rememorio/gofer/internal/runtime"
)

func TestMiddlewareRetriesEmptyPostToolResponseOnce(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 13, 13, 0, 0, 0, time.UTC)
	middleware := newMiddleware(t, now)
	messages := postToolMessages(t, now)
	sourceJSON, _ := json.Marshal(messages)
	request := model.Request{Messages: append([]domain.Message(nil), messages...)}
	if err := middleware.BeforeModel(context.Background(), &request); err != nil {
		t.Fatal(err)
	}
	response := model.Response{Usage: model.Usage{InputTokens: 3}, StopReason: model.StopEndTurn}
	got, err := middleware.TransformModelResponse(context.Background(), response)
	if !errors.Is(err, runtime.ErrRetryModelResponse) || got.Usage != response.Usage {
		t.Fatalf("TransformModelResponse() = %#v, %v", got, err)
	}

	retry := model.Request{Messages: append([]domain.Message(nil), messages...)}
	if err = middleware.BeforeModel(context.Background(), &retry); err != nil {
		t.Fatal(err)
	}
	if len(retry.Messages) != len(messages)+1 || retry.Messages[len(retry.Messages)-1].Metadata["internal_kind"] != "terminal_response_recovery" ||
		!strings.Contains(retry.Messages[len(retry.Messages)-1].Content[0].Text, "previous response") {
		t.Fatalf("retry messages = %#v", retry.Messages)
	}
	afterJSON, _ := json.Marshal(messages)
	if !bytes.Equal(sourceJSON, afterJSON) {
		t.Fatal("durable source messages changed")
	}
	got, err = middleware.TransformModelResponse(context.Background(), response)
	if err != nil || got.StopReason != model.StopTerminalError || !strings.Contains(got.Text, "no final response") || got.Usage != response.Usage {
		t.Fatalf("fallback = %#v, %v", got, err)
	}
}

func TestMiddlewareRecoveryBudgetSurvivesAnotherToolCall(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	middleware := newMiddleware(t, now)
	request := model.Request{Messages: postToolMessages(t, now)}
	if err := middleware.BeforeModel(context.Background(), &request); err != nil {
		t.Fatal(err)
	}
	empty := model.Response{StopReason: model.StopEndTurn}
	if _, err := middleware.TransformModelResponse(context.Background(), empty); !errors.Is(err, runtime.ErrRetryModelResponse) {
		t.Fatal(err)
	}
	call := domain.ToolCall{ID: "again", Name: "lookup", Arguments: json.RawMessage(`{}`)}
	toolResponse := model.Response{ToolCalls: []domain.ToolCall{call}, StopReason: model.StopToolUse}
	if got, err := middleware.TransformModelResponse(context.Background(), toolResponse); err != nil || len(got.ToolCalls) != 1 {
		t.Fatalf("tool response = %#v, %v", got, err)
	}
	request = model.Request{Messages: postToolMessages(t, now)}
	if err := middleware.BeforeModel(context.Background(), &request); err != nil {
		t.Fatal(err)
	}
	got, err := middleware.TransformModelResponse(context.Background(), empty)
	if err != nil || got.StopReason != model.StopTerminalError {
		t.Fatalf("second empty = %#v, %v", got, err)
	}
}

func TestMiddlewareIgnoresNonPostToolAndVisibleResponses(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	middleware := newMiddleware(t, now)
	user, _ := domain.NewTextMessage(domain.RoleUser, "hello", now)
	request := model.Request{Messages: []domain.Message{user}}
	if err := middleware.BeforeModel(context.Background(), &request); err != nil {
		t.Fatal(err)
	}
	tests := []model.Response{
		{StopReason: model.StopEndTurn},
		{Text: "visible", StopReason: model.StopEndTurn},
		{Text: " ", StopReason: model.StopMaxTokens},
		{ToolCalls: []domain.ToolCall{{ID: "call", Name: "lookup", Arguments: json.RawMessage(`{}`)}}, StopReason: model.StopToolUse},
	}
	for _, response := range tests {
		got, err := middleware.TransformModelResponse(context.Background(), response)
		if err != nil || !reflect.DeepEqual(got, response) {
			t.Fatalf("response = %#v, got %#v, %v", response, got, err)
		}
	}
	internal, _ := domain.NewTextMessage(domain.RoleUser, "internal", now)
	internal.Metadata = map[string]string{"internal_kind": "test"}
	request.Messages = []domain.Message{internal, postToolMessages(t, now)[2]}
	if err := middleware.BeforeModel(context.Background(), &request); err != nil {
		t.Fatal(err)
	}
	if got, err := middleware.TransformModelResponse(context.Background(), model.Response{StopReason: model.StopEndTurn}); err != nil || got.StopReason != model.StopEndTurn {
		t.Fatalf("internal-only turn = %#v, %v", got, err)
	}
}

func TestMiddlewareResetAndInvalidInputs(t *testing.T) {
	t.Parallel()
	if _, err := New(Config{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New() error = %v", err)
	}
	now := time.Now().UTC()
	middleware := newMiddleware(t, now)
	if err := middleware.BeforeModel(context.Background(), nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil request error = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := middleware.BeforeModel(cancelled, &model.Request{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled before error = %v", err)
	}
	if _, err := middleware.TransformModelResponse(cancelled, model.Response{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled transform error = %v", err)
	}
	var missing *Middleware
	if err := missing.BeforeModel(context.Background(), &model.Request{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil middleware before error = %v", err)
	}
	if _, err := missing.TransformModelResponse(context.Background(), model.Response{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil middleware transform error = %v", err)
	}
	middleware.Reset()
	middleware.Reset()
	missing.Reset()
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

func postToolMessages(t *testing.T, now time.Time) []domain.Message {
	t.Helper()
	user, _ := domain.NewTextMessage(domain.RoleUser, "check", now)
	assistantID, _ := domain.NewMessageID()
	call := domain.ToolCall{ID: "lookup", Name: "lookup", Arguments: json.RawMessage(`{}`)}
	assistant := domain.Message{ID: assistantID, Role: domain.RoleAssistant, CreatedAt: now,
		Content: []domain.Content{{Kind: domain.ContentToolCall, ToolCall: &call}}}
	resultID, _ := domain.NewMessageID()
	result := domain.ToolResult{CallID: call.ID, Output: json.RawMessage(`{"status":"ok"}`)}
	toolMessage := domain.Message{ID: resultID, Role: domain.RoleTool, CreatedAt: now,
		Content: []domain.Content{{Kind: domain.ContentToolResult, ToolResult: &result}}}
	for _, message := range []domain.Message{assistant, toolMessage} {
		if err := message.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	return []domain.Message{user, assistant, toolMessage}
}
