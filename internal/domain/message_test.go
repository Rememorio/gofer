package domain

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestNewTextMessage(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	message, err := NewTextMessage(RoleUser, "hello", now)
	if err != nil {
		t.Fatalf("NewTextMessage(): %v", err)
	}
	if message.Role != RoleUser || message.Content[0].Text != "hello" || !message.CreatedAt.Equal(now) {
		t.Fatalf("NewTextMessage() = %#v", message)
	}
}

func TestMessageValidateContentKinds(t *testing.T) {
	t.Parallel()

	id, err := NewMessageID()
	if err != nil {
		t.Fatalf("NewMessageID(): %v", err)
	}
	base := Message{ID: id, Role: RoleAssistant, CreatedAt: time.Now()}
	tests := []struct {
		name    string
		content Content
	}{
		{name: "text", content: Content{Kind: ContentText, Text: "ok"}},
		{name: "image", content: Content{Kind: ContentImage, URL: "https://example.test/image.png", MediaType: "image/png"}},
		{name: "tool call", content: Content{Kind: ContentToolCall, ToolCall: &ToolCall{ID: "call-1", Name: "search", Arguments: json.RawMessage(`{"q":"go"}`)}}},
		{name: "tool result", content: Content{Kind: ContentToolResult, ToolResult: &ToolResult{CallID: "call-1", Output: json.RawMessage(`{"ok":true}`)}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			message := base
			message.Content = []Content{test.content}
			if err := message.Validate(); err != nil {
				t.Fatalf("Validate(): %v", err)
			}
		})
	}
}

func TestMessageValidateRejectsMalformedMessages(t *testing.T) {
	t.Parallel()

	id, err := NewMessageID()
	if err != nil {
		t.Fatalf("NewMessageID(): %v", err)
	}
	valid := Message{ID: id, Role: RoleUser, CreatedAt: time.Now(), Content: []Content{{Kind: ContentText, Text: "ok"}}}
	tests := []struct {
		name   string
		mutate func(*Message)
	}{
		{name: "ID", mutate: func(message *Message) { message.ID = "bad" }},
		{name: "role", mutate: func(message *Message) { message.Role = "guest" }},
		{name: "time", mutate: func(message *Message) { message.CreatedAt = time.Time{} }},
		{name: "content", mutate: func(message *Message) { message.Content = nil }},
		{name: "empty text", mutate: func(message *Message) { message.Content[0].Text = "" }},
		{name: "bad image", mutate: func(message *Message) { message.Content[0] = Content{Kind: ContentImage} }},
		{name: "missing tool call", mutate: func(message *Message) { message.Content[0] = Content{Kind: ContentToolCall} }},
		{name: "bad tool call JSON", mutate: func(message *Message) {
			message.Content[0] = Content{Kind: ContentToolCall, ToolCall: &ToolCall{ID: "1", Name: "x", Arguments: json.RawMessage(`{`)}}
		}},
		{name: "missing tool result", mutate: func(message *Message) { message.Content[0] = Content{Kind: ContentToolResult} }},
		{name: "bad tool result JSON", mutate: func(message *Message) {
			message.Content[0] = Content{Kind: ContentToolResult, ToolResult: &ToolResult{CallID: "1", Output: json.RawMessage(`{`)}}
		}},
		{name: "unknown content", mutate: func(message *Message) { message.Content[0] = Content{Kind: "audio"} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			message := valid
			message.Content = append([]Content(nil), valid.Content...)
			test.mutate(&message)
			if err := message.Validate(); !errors.Is(err, ErrInvalidMessage) {
				t.Fatalf("Validate() error = %v, want ErrInvalidMessage", err)
			}
		})
	}
}

func TestNewTextMessageRejectsEmptyText(t *testing.T) {
	t.Parallel()

	_, err := NewTextMessage(RoleUser, "", time.Now())
	if !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("NewTextMessage() error = %v, want ErrInvalidMessage", err)
	}
}
