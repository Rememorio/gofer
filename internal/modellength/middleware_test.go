package modellength

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/model"
)

func TestMiddlewarePromotesVisibleLengthCappedText(t *testing.T) {
	t.Parallel()
	middleware := New()
	response := model.Response{
		Text: "partial but useful", Usage: model.Usage{InputTokens: 8, OutputTokens: 4},
		StopReason: model.StopMaxTokens,
	}
	got, err := middleware.TransformModelResponse(context.Background(), response)
	if err != nil || got.Text != response.Text || got.Usage != response.Usage || got.StopReason != model.StopModelLengthCapped {
		t.Fatalf("TransformModelResponse() = %#v, %v", got, err)
	}
	if response.StopReason != model.StopMaxTokens {
		t.Fatalf("source response changed: %#v", response)
	}
}

func TestMiddlewareLeavesUnsafeOrUnrelatedResponsesUnchanged(t *testing.T) {
	t.Parallel()
	call := domain.ToolCall{ID: "call", Name: "write_file", Arguments: json.RawMessage(`{"path":"x"}`)}
	tests := []model.Response{
		{StopReason: model.StopMaxTokens},
		{Text: " \n", StopReason: model.StopMaxTokens},
		{Text: "partial", ToolCalls: []domain.ToolCall{call}, StopReason: model.StopMaxTokens},
		{Text: "filtered", StopReason: model.StopContentFilter},
		{Text: "done", StopReason: model.StopEndTurn},
	}
	for _, response := range tests {
		got, err := New().TransformModelResponse(context.Background(), response)
		if err != nil || !reflect.DeepEqual(got, response) {
			t.Fatalf("response = %#v, got %#v, %v", response, got, err)
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
