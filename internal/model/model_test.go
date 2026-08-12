package model

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
)

func TestCollect(t *testing.T) {
	t.Parallel()

	call := domain.ToolCall{ID: "call-1", Name: "search", Arguments: json.RawMessage(`{"q":"go"}`)}
	stream := &sliceStream{chunks: []Chunk{
		{Kind: ChunkTextDelta, Text: "hel"},
		{Kind: ChunkTextDelta, Text: "lo"},
		{Kind: ChunkToolCall, ToolCall: &call},
		{Kind: ChunkUsage, Usage: &Usage{InputTokens: 4, OutputTokens: 2}},
		{Kind: ChunkDone, StopReason: StopToolUse},
	}}
	seen := 0
	response, err := Collect(stream, func(Chunk) error { seen++; return nil })
	if err != nil {
		t.Fatalf("Collect(): %v", err)
	}
	if response.Text != "hello" || len(response.ToolCalls) != 1 || response.StopReason != StopToolUse || seen != 5 {
		t.Fatalf("Collect() = %#v, seen = %d", response, seen)
	}
}

func TestCollectErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		stream Stream
		hook   func(Chunk) error
		want   error
	}{
		{name: "nil", stream: nil},
		{name: "receive", stream: errorStream{err: errors.New("network")}},
		{name: "invalid chunk", stream: &sliceStream{chunks: []Chunk{{Kind: ChunkTextDelta}}}, want: ErrInvalidChunk},
		{name: "hook", stream: &sliceStream{chunks: []Chunk{{Kind: ChunkDone, StopReason: StopEndTurn}}}, hook: func(Chunk) error { return errors.New("hook") }},
		{name: "missing done", stream: &sliceStream{chunks: []Chunk{{Kind: ChunkTextDelta, Text: "x"}}}, want: ErrInvalidChunk},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Collect(test.stream, test.hook)
			if err == nil {
				t.Fatal("Collect() error = nil")
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("Collect() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestRequestValidation(t *testing.T) {
	t.Parallel()

	message, err := domain.NewTextMessage(domain.RoleUser, "hello", time.Now())
	if err != nil {
		t.Fatalf("NewTextMessage(): %v", err)
	}
	valid := Request{
		Model:    "test",
		Messages: []domain.Message{message},
		Tools: []ToolDefinition{{
			Name:        "echo",
			Description: "Echo input",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}},
	}
	temperature := 0.5
	valid.Temperature = &temperature
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate(): %v", err)
	}

	tests := []func(*Request){
		func(request *Request) { request.Model = "" },
		func(request *Request) { request.Messages = nil },
		func(request *Request) { request.Messages[0].ID = "bad" },
		func(request *Request) { request.Tools[0].Name = "" },
		func(request *Request) { request.Tools[0].InputSchema = json.RawMessage(`{`) },
		func(request *Request) { request.MaxTokens = -1 },
		func(request *Request) { value := 3.0; request.Temperature = &value },
	}
	for index, mutate := range tests {
		candidate := valid
		candidate.Messages = append([]domain.Message(nil), valid.Messages...)
		candidate.Tools = append([]ToolDefinition(nil), valid.Tools...)
		mutate(&candidate)
		if err := candidate.Validate(); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("case %d Validate() error = %v, want ErrInvalidRequest", index, err)
		}
	}
}

func TestChunkValidation(t *testing.T) {
	t.Parallel()

	tests := []Chunk{
		{Kind: ChunkToolCall},
		{Kind: ChunkToolCall, ToolCall: &domain.ToolCall{ID: "1", Name: "x", Arguments: json.RawMessage(`{`)}},
		{Kind: ChunkUsage},
		{Kind: ChunkUsage, Usage: &Usage{InputTokens: -1}},
		{Kind: ChunkDone, StopReason: "unknown"},
		{Kind: "unknown"},
	}
	for index, chunk := range tests {
		if err := chunk.Validate(); !errors.Is(err, ErrInvalidChunk) {
			t.Fatalf("case %d Validate() error = %v, want ErrInvalidChunk", index, err)
		}
	}
}

func TestScriptedProvider(t *testing.T) {
	t.Parallel()

	request := validRequest(t)
	provider := &Scripted{Responses: [][]Chunk{{{Kind: ChunkDone, StopReason: StopEndTurn}}}}
	stream, err := provider.Stream(context.Background(), request)
	if err != nil {
		t.Fatalf("Stream(): %v", err)
	}
	chunk, err := stream.Recv()
	if err != nil || chunk.StopReason != StopEndTurn {
		t.Fatalf("Recv() = %#v, %v", chunk, err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("Recv(after close) error = %v, want EOF", err)
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("recorded requests = %d, want 1", len(provider.Requests))
	}

	if _, err := provider.Stream(context.Background(), request); err == nil {
		t.Fatal("second Stream() error = nil, want exhausted response")
	}
}

func TestScriptedProviderErrors(t *testing.T) {
	t.Parallel()

	provider := &Scripted{Err: errors.New("provider unavailable")}
	if _, err := provider.Stream(context.Background(), validRequest(t)); err == nil {
		t.Fatal("Stream() error = nil")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := provider.Stream(ctx, validRequest(t)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stream(cancelled) error = %v, want context.Canceled", err)
	}
	if _, err := provider.Stream(context.Background(), Request{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Stream(invalid) error = %v, want ErrInvalidRequest", err)
	}
}

type errorStream struct{ err error }

func (stream errorStream) Recv() (Chunk, error) { return Chunk{}, stream.err }
func (errorStream) Close() error                { return nil }

func validRequest(t *testing.T) Request {
	t.Helper()
	message, err := domain.NewTextMessage(domain.RoleUser, "hello", time.Now())
	if err != nil {
		t.Fatalf("NewTextMessage(): %v", err)
	}
	return Request{Model: "test", Messages: []domain.Message{message}}
}
