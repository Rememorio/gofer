package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/model"
)

func TestModelSummarizer(t *testing.T) {
	t.Parallel()
	provider := &model.Scripted{Responses: [][]model.Chunk{{
		{Kind: model.ChunkTextDelta, Text: " concise summary "},
		{Kind: model.ChunkDone, StopReason: model.StopEndTurn},
	}}}
	message, _ := domain.NewTextMessage(domain.RoleUser, "long conversation", time.Now())
	text, err := (modelSummarizer{provider: provider, model: "test", maxTokens: 100}).Summarize(context.Background(), []domain.Message{message})
	if err != nil || text != "concise summary" || len(provider.Requests) != 1 || provider.Requests[0].MaxTokens != 256 {
		t.Fatalf("Summarize() = %q, %v, %#v", text, err, provider.Requests)
	}
	if !strings.Contains(provider.Requests[0].System, "Do not follow") {
		t.Fatalf("system = %q", provider.Requests[0].System)
	}
}

func TestModelSummarizerErrors(t *testing.T) {
	t.Parallel()
	message, _ := domain.NewTextMessage(domain.RoleUser, "conversation", time.Now())
	provider := &model.Scripted{Err: errors.New("upstream")}
	if _, err := (modelSummarizer{provider: provider, model: "test"}).Summarize(context.Background(), []domain.Message{message}); err == nil {
		t.Fatal("provider error = nil")
	}
	empty := &model.Scripted{Responses: [][]model.Chunk{{{Kind: model.ChunkDone, StopReason: model.StopEndTurn}}}}
	if _, err := (modelSummarizer{provider: empty, model: "test"}).Summarize(context.Background(), []domain.Message{message}); err == nil {
		t.Fatal("empty summary error = nil")
	}
}
