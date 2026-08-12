package toolhistory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/model"
	"github.com/Rememorio/gofer/internal/runtime"
)

const (
	defaultMaxMessages  = 10_000
	defaultMaxToolCalls = 10_000
	maximumBoundary     = 1_000_000
)

var (
	// ErrInvalidConfig identifies missing or unsafe repair bounds.
	ErrInvalidConfig = errors.New("invalid tool history repair configuration")
	// ErrHistoryLimit identifies a model transcript that exceeds repair bounds.
	ErrHistoryLimit = errors.New("tool history repair limit exceeded")
)

// Config bounds transient transcript repair for one model request.
type Config struct {
	MaxMessages  int
	MaxToolCalls int
	Now          func() time.Time
}

// DefaultConfig returns conservative model-request repair bounds.
func DefaultConfig() Config {
	return Config{MaxMessages: defaultMaxMessages, MaxToolCalls: defaultMaxToolCalls, Now: time.Now}
}

// Middleware restores provider-safe call/result ordering without changing the
// durable conversation supplied by the caller.
type Middleware struct {
	runtime.NopMiddleware
	config Config
}

var _ runtime.Middleware = (*Middleware)(nil)

// New validates configuration and constructs a transcript repair middleware.
func New(config Config) (*Middleware, error) {
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.MaxMessages < 1 || config.MaxMessages > maximumBoundary ||
		config.MaxToolCalls < 1 || config.MaxToolCalls > maximumBoundary {
		return nil, ErrInvalidConfig
	}
	return &Middleware{config: config}, nil
}

// BeforeModel groups results immediately after their assistant call, fills
// missing results, and removes results that have no model-visible call.
func (middleware *Middleware) BeforeModel(ctx context.Context, request *model.Request) error {
	if middleware == nil || request == nil || middleware.config.Now == nil {
		return fmt.Errorf("%w: middleware and request are required", ErrInvalidConfig)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	repaired, changed, err := middleware.repair(request.Messages)
	if err != nil {
		return err
	}
	if changed {
		request.Messages = repaired
	}
	return nil
}

func (middleware *Middleware) repair(messages []domain.Message) ([]domain.Message, bool, error) {
	if len(messages) > middleware.config.MaxMessages {
		return nil, false, fmt.Errorf("%w: messages %d exceeds %d", ErrHistoryLimit, len(messages), middleware.config.MaxMessages)
	}
	results, toolCalls, err := middleware.index(messages)
	if err != nil {
		return nil, false, err
	}
	positions := make(map[string]int, len(results))
	repaired := make([]domain.Message, 0, len(messages)+toolCalls)
	for _, message := range messages {
		if message.Role == domain.RoleTool {
			continue
		}
		repaired = append(repaired, message)
		if message.Role == domain.RoleAssistant {
			repaired, err = middleware.appendResults(repaired, message, results, positions)
			if err != nil {
				return nil, false, err
			}
		}
	}
	return repaired, !sameMessages(messages, repaired), nil
}

func (middleware *Middleware) index(messages []domain.Message) (map[string][]domain.Message, int, error) {
	results := make(map[string][]domain.Message)
	toolCalls := 0
	for _, message := range messages {
		if message.Role == domain.RoleTool {
			result, ok := messageToolResult(message)
			if ok {
				results[result.CallID] = append(results[result.CallID], message)
			}
		}
		if message.Role == domain.RoleAssistant {
			toolCalls += len(messageToolCalls(message))
			if toolCalls > middleware.config.MaxToolCalls {
				return nil, 0, fmt.Errorf("%w: tool calls %d exceeds %d", ErrHistoryLimit, toolCalls, middleware.config.MaxToolCalls)
			}
		}
	}
	return results, toolCalls, nil
}

func (middleware *Middleware) appendResults(repaired []domain.Message, assistant domain.Message, results map[string][]domain.Message, positions map[string]int) ([]domain.Message, error) {
	for _, call := range messageToolCalls(assistant) {
		position := positions[call.ID]
		queue := results[call.ID]
		if position < len(queue) {
			repaired = append(repaired, queue[position])
			positions[call.ID] = position + 1
			continue
		}
		synthetic, err := recoveryMessage(call.ID, middleware.config.Now())
		if err != nil {
			return nil, err
		}
		repaired = append(repaired, synthetic)
	}
	return repaired, nil
}

func messageToolCalls(message domain.Message) []domain.ToolCall {
	calls := make([]domain.ToolCall, 0)
	for _, content := range message.Content {
		if content.Kind == domain.ContentToolCall && content.ToolCall != nil {
			calls = append(calls, *content.ToolCall)
		}
	}
	return calls
}

func messageToolResult(message domain.Message) (domain.ToolResult, bool) {
	if len(message.Content) != 1 || message.Content[0].Kind != domain.ContentToolResult || message.Content[0].ToolResult == nil {
		return domain.ToolResult{}, false
	}
	return *message.Content[0].ToolResult, true
}

func recoveryMessage(callID string, at time.Time) (domain.Message, error) {
	output, err := json.Marshal(map[string]any{
		"code": "interrupted_tool_call", "error": "tool call was interrupted and did not return a result", "recoverable": true,
	})
	if err != nil {
		return domain.Message{}, err
	}
	id, err := domain.NewMessageID()
	if err != nil {
		return domain.Message{}, err
	}
	result := domain.ToolResult{CallID: callID, Output: output, IsError: true}
	message := domain.Message{
		ID: id, Role: domain.RoleTool, CreatedAt: at,
		Content:  []domain.Content{{Kind: domain.ContentToolResult, ToolResult: &result}},
		Metadata: map[string]string{"internal_kind": "tool_result_recovery"},
	}
	if err := message.Validate(); err != nil {
		return domain.Message{}, fmt.Errorf("build tool history recovery: %w", err)
	}
	return message, nil
}

func sameMessages(left, right []domain.Message) bool {
	return reflect.DeepEqual(left, right)
}
