package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Rememorio/gofer/internal/domain"
)

var (
	// ErrInvalidRequest identifies a malformed normalized model request.
	ErrInvalidRequest = errors.New("invalid model request")
	// ErrInvalidChunk identifies malformed provider stream output.
	ErrInvalidChunk = errors.New("invalid model chunk")
)

// Provider opens normalized streaming model requests.
type Provider interface {
	Stream(context.Context, Request) (Stream, error)
}

// Stream yields normalized chunks until io.EOF and must be closed by callers.
type Stream interface {
	Recv() (Chunk, error)
	Close() error
}

// Request is a provider-independent model invocation.
type Request struct {
	Model       string           `json:"model"`
	System      string           `json:"system,omitempty"`
	Messages    []domain.Message `json:"messages"`
	Tools       []ToolDefinition `json:"tools,omitempty"`
	MaxTokens   int              `json:"max_tokens,omitempty"`
	Temperature *float64         `json:"temperature,omitempty"`
}

// ToolDefinition describes one model-visible tool.
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// ChunkKind identifies one normalized stream item.
type ChunkKind string

// Supported normalized stream chunk kinds.
const (
	ChunkTextDelta ChunkKind = "text_delta"
	ChunkToolCall  ChunkKind = "tool_call"
	ChunkUsage     ChunkKind = "usage"
	ChunkDone      ChunkKind = "done"
)

// Chunk is one atomic provider-independent stream item.
type Chunk struct {
	Kind       ChunkKind        `json:"type"`
	Text       string           `json:"text,omitempty"`
	ToolCall   *domain.ToolCall `json:"tool_call,omitempty"`
	Usage      *Usage           `json:"usage,omitempty"`
	StopReason StopReason       `json:"stop_reason,omitempty"`
}

// Usage is normalized token accounting for one model invocation.
type Usage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	ReasoningTokens  int `json:"reasoning_tokens,omitempty"`
	CacheReadTokens  int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
}

// StopReason identifies why a model invocation ended.
type StopReason string

// Supported normalized stop reasons.
const (
	StopEndTurn       StopReason = "end_turn"
	StopToolUse       StopReason = "tool_use"
	StopMaxTokens     StopReason = "max_tokens"
	StopContentFilter StopReason = "content_filter"
	// StopLoopCapped identifies a host-side safety stop before repetitive tool
	// calls were executed. Providers never need to emit this reason directly.
	StopLoopCapped StopReason = "loop_capped"
	// StopTerminalError identifies a visible host fallback after the provider
	// returned no final answer twice. Providers never emit this reason.
	StopTerminalError StopReason = "terminal_error"
	// StopModelLengthCapped preserves a visible provider response that reached
	// its output-token limit. Providers emit StopMaxTokens; host middleware
	// promotes only safe terminal text to this run-level reason.
	StopModelLengthCapped StopReason = "model_length_capped"
	// StopSafetyCapped identifies a provider safety stop after any tool intent
	// was suppressed. Providers emit StopContentFilter; host middleware
	// promotes the repaired terminal response to this run-level reason.
	StopSafetyCapped StopReason = "safety_capped"
)

// Response is the fully collected result of one model stream.
type Response struct {
	Text       string            `json:"text,omitempty"`
	ToolCalls  []domain.ToolCall `json:"tool_calls,omitempty"`
	Usage      Usage             `json:"usage"`
	StopReason StopReason        `json:"stop_reason"`
}

// Validate verifies the normalized model request contract.
func (request Request) Validate() error {
	if strings.TrimSpace(request.Model) == "" {
		return fmt.Errorf("%w: model is required", ErrInvalidRequest)
	}
	if len(request.Messages) == 0 {
		return fmt.Errorf("%w: messages are required", ErrInvalidRequest)
	}
	for index, message := range request.Messages {
		if err := message.Validate(); err != nil {
			return fmt.Errorf("%w: message %d: %w", ErrInvalidRequest, index, err)
		}
	}
	for index, definition := range request.Tools {
		if err := definition.Validate(); err != nil {
			return fmt.Errorf("%w: tool %d: %w", ErrInvalidRequest, index, err)
		}
	}
	if request.MaxTokens < 0 {
		return fmt.Errorf("%w: max_tokens must not be negative", ErrInvalidRequest)
	}
	if request.Temperature != nil && (*request.Temperature < 0 || *request.Temperature > 2) {
		return fmt.Errorf("%w: temperature must be between 0 and 2", ErrInvalidRequest)
	}
	return nil
}

// Validate verifies a model-visible tool definition.
func (definition ToolDefinition) Validate() error {
	if strings.TrimSpace(definition.Name) == "" || strings.TrimSpace(definition.Description) == "" {
		return errors.New("tool name and description are required")
	}
	if len(definition.InputSchema) == 0 || !json.Valid(definition.InputSchema) {
		return errors.New("tool input schema must be valid JSON")
	}
	return nil
}

// Validate verifies a normalized stream chunk.
func (chunk Chunk) Validate() error {
	switch chunk.Kind {
	case ChunkTextDelta:
		if chunk.Text == "" {
			return fmt.Errorf("%w: text delta is empty", ErrInvalidChunk)
		}
	case ChunkToolCall:
		if err := validateToolCall(chunk.ToolCall); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidChunk, err)
		}
	case ChunkUsage:
		if chunk.Usage == nil || !chunk.Usage.valid() {
			return fmt.Errorf("%w: usage is missing or negative", ErrInvalidChunk)
		}
	case ChunkDone:
		if !chunk.StopReason.valid() {
			return fmt.Errorf("%w: invalid stop reason %q", ErrInvalidChunk, chunk.StopReason)
		}
	default:
		return fmt.Errorf("%w: unsupported type %q", ErrInvalidChunk, chunk.Kind)
	}
	return nil
}

// Collect consumes stream, invoking onChunk after validation for every item.
func Collect(stream Stream, onChunk func(Chunk) error) (Response, error) {
	if stream == nil {
		return Response{}, errors.New("collect model stream: stream is nil")
	}
	var response Response
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Response{}, fmt.Errorf("receive model stream: %w", err)
		}
		if err := chunk.Validate(); err != nil {
			return Response{}, err
		}
		if onChunk != nil {
			if err := onChunk(chunk); err != nil {
				return Response{}, err
			}
		}
		response.apply(chunk)
	}
	if response.StopReason == "" {
		return Response{}, fmt.Errorf("%w: stream ended without done chunk", ErrInvalidChunk)
	}
	return response, nil
}

func (response *Response) apply(chunk Chunk) {
	switch chunk.Kind {
	case ChunkTextDelta:
		response.Text += chunk.Text
	case ChunkToolCall:
		response.ToolCalls = append(response.ToolCalls, *chunk.ToolCall)
	case ChunkUsage:
		response.Usage = *chunk.Usage
	case ChunkDone:
		response.StopReason = chunk.StopReason
	}
}

func validateToolCall(call *domain.ToolCall) error {
	if call == nil || strings.TrimSpace(call.ID) == "" || strings.TrimSpace(call.Name) == "" {
		return errors.New("tool call ID and name are required")
	}
	if len(call.Arguments) == 0 || !json.Valid(call.Arguments) {
		return errors.New("tool call arguments must be valid JSON")
	}
	return nil
}

func (usage Usage) valid() bool {
	return usage.InputTokens >= 0 && usage.OutputTokens >= 0 && usage.ReasoningTokens >= 0 &&
		usage.CacheReadTokens >= 0 && usage.CacheWriteTokens >= 0
}

func (reason StopReason) valid() bool {
	switch reason {
	case StopEndTurn, StopToolUse, StopMaxTokens, StopContentFilter, StopLoopCapped,
		StopTerminalError, StopModelLengthCapped, StopSafetyCapped:
		return true
	default:
		return false
	}
}
