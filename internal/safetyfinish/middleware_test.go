package safetyfinish

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/model"
)

func TestMiddlewareSuppressesSafetyCappedTools(t *testing.T) {
	t.Parallel()
	call := domain.ToolCall{ID: "write", Name: "write_file", Arguments: json.RawMessage(`{"path":"partial"}`)}
	response := model.Response{
		Text: "partial text", ToolCalls: []domain.ToolCall{call},
		Usage: model.Usage{InputTokens: 7, OutputTokens: 2}, StopReason: model.StopContentFilter,
	}
	got, err := New().TransformModelResponse(context.Background(), response)
	if err != nil || len(got.ToolCalls) != 0 || got.StopReason != model.StopSafetyCapped ||
		got.Usage != response.Usage || !strings.Contains(got.Text, "partial text") || !strings.Contains(got.Text, "suppressed") {
		t.Fatalf("TransformModelResponse() = %#v, %v", got, err)
	}
	if len(response.ToolCalls) != 1 || response.StopReason != model.StopContentFilter {
		t.Fatalf("source response changed: %#v", response)
	}
}

func TestMiddlewareBackfillsEmptySafetyResponse(t *testing.T) {
	t.Parallel()
	response := model.Response{StopReason: model.StopContentFilter}
	got, err := New().TransformModelResponse(context.Background(), response)
	if err != nil || got.StopReason != model.StopSafetyCapped || !strings.Contains(got.Text, "returned no content") {
		t.Fatalf("TransformModelResponse() = %#v, %v", got, err)
	}
}

func TestMiddlewarePreservesVisibleSafetyText(t *testing.T) {
	t.Parallel()
	response := model.Response{Text: "I cannot help with that.", StopReason: model.StopContentFilter}
	got, err := New().TransformModelResponse(context.Background(), response)
	if err != nil || got.Text != response.Text || got.StopReason != model.StopSafetyCapped {
		t.Fatalf("TransformModelResponse() = %#v, %v", got, err)
	}
}

func TestMiddlewareIgnoresOtherResponses(t *testing.T) {
	t.Parallel()
	responses := []model.Response{
		{Text: "done", StopReason: model.StopEndTurn},
		{Text: "partial", StopReason: model.StopMaxTokens},
		{Text: "loop", StopReason: model.StopLoopCapped},
	}
	for _, response := range responses {
		got, err := New().TransformModelResponse(context.Background(), response)
		if err != nil || !reflect.DeepEqual(got, response) {
			t.Fatalf("response=%#v got=%#v error=%v", response, got, err)
		}
	}
}

func TestMiddlewareRejectsNilAndCancellation(t *testing.T) {
	t.Parallel()
	var middleware *Middleware
	if _, err := middleware.TransformModelResponse(context.Background(), model.Response{}); !errors.Is(err, ErrInvalidMiddleware) {
		t.Fatalf("nil middleware error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New().TransformModelResponse(ctx, model.Response{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}
