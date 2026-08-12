package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrInvalidMessage identifies a message that violates the normalized schema.
var ErrInvalidMessage = errors.New("invalid message")

// Role identifies the participant that produced a message.
type Role string

// Supported normalized message roles.
const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ContentKind identifies the representation of a message content block.
type ContentKind string

// Supported normalized content block kinds.
const (
	ContentText       ContentKind = "text"
	ContentImage      ContentKind = "image"
	ContentToolCall   ContentKind = "tool_call"
	ContentToolResult ContentKind = "tool_result"
)

// Content is one normalized block in a model-independent message.
type Content struct {
	Kind       ContentKind `json:"type"`
	Text       string      `json:"text,omitempty"`
	MediaType  string      `json:"media_type,omitempty"`
	URL        string      `json:"url,omitempty"`
	ToolCall   *ToolCall   `json:"tool_call,omitempty"`
	ToolResult *ToolResult `json:"tool_result,omitempty"`
}

// ToolCall describes one model-requested tool invocation.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ToolResult describes the structured outcome of one tool invocation.
type ToolResult struct {
	CallID    string          `json:"call_id"`
	Output    json.RawMessage `json:"output"`
	IsError   bool            `json:"is_error,omitempty"`
	Interrupt bool            `json:"interrupt,omitempty"`
}

// Message is Gofer's durable, provider-independent conversation record.
type Message struct {
	ID        MessageID         `json:"id"`
	Role      Role              `json:"role"`
	Content   []Content         `json:"content"`
	CreatedAt time.Time         `json:"created_at"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// NewTextMessage constructs and validates a single-block text message.
func NewTextMessage(role Role, text string, at time.Time) (Message, error) {
	id, err := NewMessageID()
	if err != nil {
		return Message{}, err
	}
	message := Message{
		ID:        id,
		Role:      role,
		Content:   []Content{{Kind: ContentText, Text: text}},
		CreatedAt: at,
	}
	if err := message.Validate(); err != nil {
		return Message{}, err
	}
	return message, nil
}

// Validate verifies the normalized message contract.
func (message Message) Validate() error {
	if _, err := ParseMessageID(string(message.ID)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidMessage, err)
	}
	if !message.Role.valid() {
		return fmt.Errorf("%w: unsupported role %q", ErrInvalidMessage, message.Role)
	}
	if message.CreatedAt.IsZero() {
		return fmt.Errorf("%w: created_at is required", ErrInvalidMessage)
	}
	if len(message.Content) == 0 {
		return fmt.Errorf("%w: content is required", ErrInvalidMessage)
	}
	callIDs := make(map[string]struct{})
	for index, block := range message.Content {
		if err := block.validate(); err != nil {
			return fmt.Errorf("%w: content %d: %w", ErrInvalidMessage, index, err)
		}
		if block.Kind != ContentToolCall {
			continue
		}
		if _, duplicate := callIDs[block.ToolCall.ID]; duplicate {
			return fmt.Errorf("%w: duplicate tool call ID %q", ErrInvalidMessage, block.ToolCall.ID)
		}
		callIDs[block.ToolCall.ID] = struct{}{}
	}
	return nil
}

func (role Role) valid() bool {
	switch role {
	case RoleSystem, RoleUser, RoleAssistant, RoleTool:
		return true
	default:
		return false
	}
}

func (content Content) validate() error {
	switch content.Kind {
	case ContentText:
		if content.Text == "" {
			return errors.New("text is required")
		}
	case ContentImage:
		if content.URL == "" || content.MediaType == "" {
			return errors.New("image URL and media type are required")
		}
	case ContentToolCall:
		return validateToolCall(content.ToolCall)
	case ContentToolResult:
		return validateToolResult(content.ToolResult)
	default:
		return fmt.Errorf("unsupported type %q", content.Kind)
	}
	return nil
}

func validateToolCall(call *ToolCall) error {
	if call == nil || strings.TrimSpace(call.ID) == "" || strings.TrimSpace(call.Name) == "" {
		return errors.New("tool call ID and name are required")
	}
	if len(call.Arguments) == 0 || !json.Valid(call.Arguments) {
		return errors.New("tool call arguments must be valid JSON")
	}
	return nil
}

func validateToolResult(result *ToolResult) error {
	if result == nil || strings.TrimSpace(result.CallID) == "" {
		return errors.New("tool result call ID is required")
	}
	if len(result.Output) == 0 || !json.Valid(result.Output) {
		return errors.New("tool result output must be valid JSON")
	}
	return nil
}
