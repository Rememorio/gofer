package modelservice

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/model"
)

func TestGeneratorReturnsCleanToolFreeText(t *testing.T) {
	t.Parallel()

	provider := &model.Scripted{Responses: [][]model.Chunk{{
		{Kind: model.ChunkTextDelta, Text: "<think>private</think>\n```text\nUseful answer\n```"},
		{Kind: model.ChunkUsage, Usage: &model.Usage{InputTokens: 5, OutputTokens: 2}},
		{Kind: model.ChunkDone, StopReason: model.StopEndTurn},
	}}}
	generator := Generator{Provider: provider, Model: "fast", Now: func() time.Time { return time.Unix(1, 0) }}
	result, err := generator.Generate(context.Background(), "Follow the task.", "Generate text.", 64)
	if err != nil {
		t.Fatalf("Generate(): %v", err)
	}
	if result.Text != "Useful answer" || result.Usage.InputTokens != 5 || len(provider.Requests) != 1 {
		t.Fatalf("result = %#v, requests = %#v", result, provider.Requests)
	}
	request := provider.Requests[0]
	if request.Model != "fast" || request.MaxTokens != 64 || request.System != "Follow the task." ||
		len(request.Messages) != 1 || request.Messages[0].Role != domain.RoleUser {
		t.Fatalf("request = %#v", request)
	}
}

func TestGeneratorAcceptsVisibleLengthCappedText(t *testing.T) {
	t.Parallel()
	provider := &model.Scripted{Responses: [][]model.Chunk{{
		{Kind: model.ChunkTextDelta, Text: "partial"},
		{Kind: model.ChunkDone, StopReason: model.StopMaxTokens},
	}}}
	result, err := (Generator{Provider: provider, Model: "fast"}).Generate(context.Background(), "system", "user", 8)
	if err != nil || result.Text != "partial" {
		t.Fatalf("Generate() = %#v, %v", result, err)
	}
}

func TestGeneratorRejectsInvalidConfigurationAndResponses(t *testing.T) {
	t.Parallel()
	for _, generator := range []Generator{{}, {Provider: &model.Scripted{}, Model: ""}} {
		if _, err := generator.Generate(context.Background(), "system", "user", 32); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("Generate(invalid) error = %v", err)
		}
	}
	provider := &model.Scripted{Responses: [][]model.Chunk{
		{{Kind: model.ChunkTextDelta, Text: "<think>only</think>"}, {Kind: model.ChunkDone, StopReason: model.StopEndTurn}},
		{{Kind: model.ChunkTextDelta, Text: "blocked"}, {Kind: model.ChunkDone, StopReason: model.StopContentFilter}},
		{{Kind: model.ChunkTextDelta, Text: "call"}, {Kind: model.ChunkToolCall, ToolCall: &domain.ToolCall{ID: "1", Name: "tool", Arguments: []byte(`{}`)}}, {Kind: model.ChunkDone, StopReason: model.StopToolUse}},
	}}
	generator := Generator{Provider: provider, Model: "fast"}
	for range 3 {
		if _, err := generator.Generate(context.Background(), "system", "user", 32); !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("Generate(invalid response) error = %v", err)
		}
	}
}

func TestGeneratorPropagatesProviderAndStreamErrors(t *testing.T) {
	t.Parallel()
	providerErr := errors.New("provider unavailable")
	if _, err := (Generator{Provider: &model.Scripted{Err: providerErr}, Model: "fast"}).Generate(context.Background(), "system", "user", 32); !errors.Is(err, providerErr) {
		t.Fatalf("Generate(provider error) = %v", err)
	}
	provider := &model.Scripted{Responses: [][]model.Chunk{{{Kind: model.ChunkTextDelta}}}}
	if _, err := (Generator{Provider: provider, Model: "fast"}).Generate(context.Background(), "system", "user", 32); err == nil {
		t.Fatal("Generate(stream error) error = nil")
	}
}

func TestCleanTextPreservesIncompleteWrappers(t *testing.T) {
	t.Parallel()
	for input, want := range map[string]string{
		"plain":                 "plain",
		"<think>unfinished":     "<think>unfinished",
		"```text\nunfinished":   "```text\nunfinished",
		"```single-line```":     "```single-line```",
		"<THINK>x</THINK> text": "text",
	} {
		if got := CleanText(input); got != want {
			t.Errorf("CleanText(%q) = %q, want %q", input, got, want)
		}
	}
}
