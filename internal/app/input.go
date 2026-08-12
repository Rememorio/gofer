package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/gateway"
)

type runSettings struct {
	model       string
	system      string
	maxTokens   int
	temperature *float64
}

type wireMessage struct {
	Role       string          `json:"role"`
	Type       string          `json:"type"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"tool_call_id"`
	ToolCalls  []wireToolCall  `json:"tool_calls"`
}

type wireToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Args      json.RawMessage `json:"args"`
	Arguments json.RawMessage `json:"arguments"`
}

func decodeLaunch(request gateway.RunRequest, now time.Time) ([]domain.Message, runSettings, error) {
	settings, err := decodeSettings(request)
	if err != nil {
		return nil, runSettings{}, err
	}
	rawMessages, err := messageList(request.Input)
	if err != nil {
		return nil, runSettings{}, err
	}
	messages := make([]domain.Message, 0, len(rawMessages))
	for index, raw := range rawMessages {
		message, decodeErr := decodeMessage(raw, now.Add(time.Duration(index)*time.Nanosecond))
		if decodeErr != nil {
			return nil, runSettings{}, fmt.Errorf("invalid input message %d: %w", index, decodeErr)
		}
		messages = append(messages, message)
	}
	if len(messages) == 0 {
		return nil, runSettings{}, errors.New("invalid input: messages are required")
	}
	return messages, settings, nil
}

func decodeSettings(request gateway.RunRequest) (runSettings, error) {
	settings := runSettings{model: strings.TrimSpace(request.AssistantID)}
	for _, raw := range []json.RawMessage{request.Config, request.Context} {
		var err error
		settings, err = mergeSettings(settings, raw)
		if err != nil {
			return runSettings{}, err
		}
	}
	if settings.maxTokens < 0 || (settings.temperature != nil && (*settings.temperature < 0 || *settings.temperature > 2)) {
		return runSettings{}, errors.New("invalid run config: max_tokens or temperature is out of range")
	}
	return settings, nil
}

func mergeSettings(settings runSettings, raw json.RawMessage) (runSettings, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return settings, nil
	}
	var value struct {
		Model        string   `json:"model"`
		System       string   `json:"system"`
		SystemPrompt string   `json:"system_prompt"`
		MaxTokens    int      `json:"max_tokens"`
		Temperature  *float64 `json:"temperature"`
		Configurable *struct {
			Model string `json:"model"`
		} `json:"configurable"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return runSettings{}, fmt.Errorf("invalid run config: %w", err)
	}
	if value.Configurable != nil && value.Configurable.Model != "" {
		settings.model = strings.TrimSpace(value.Configurable.Model)
	}
	if value.Model != "" {
		settings.model = strings.TrimSpace(value.Model)
	}
	if value.System != "" {
		settings.system = value.System
	}
	if value.SystemPrompt != "" {
		settings.system = value.SystemPrompt
	}
	if value.MaxTokens != 0 {
		settings.maxTokens = value.MaxTokens
	}
	if value.Temperature != nil {
		settings.temperature = value.Temperature
	}
	return settings, nil
}

func messageList(raw json.RawMessage) ([]json.RawMessage, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, errors.New("invalid input: messages are required")
	}
	var envelope struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Messages != nil {
		return envelope.Messages, nil
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(raw, &messages); err != nil {
		return nil, fmt.Errorf("invalid input: expected a messages object or array: %w", err)
	}
	return messages, nil
}

func decodeMessage(raw json.RawMessage, at time.Time) (domain.Message, error) {
	var input wireMessage
	if err := strictJSON(raw, &input); err != nil {
		return domain.Message{}, err
	}
	role, err := normalizeRole(input.Role, input.Type)
	if err != nil {
		return domain.Message{}, err
	}
	contents, err := decodeContents(role, input)
	if err != nil {
		return domain.Message{}, err
	}
	id, err := domain.NewMessageID()
	if err != nil {
		return domain.Message{}, err
	}
	message := domain.Message{ID: id, Role: role, Content: contents, CreatedAt: at.UTC()}
	return message, message.Validate()
}

func normalizeRole(role, messageType string) (domain.Role, error) {
	value := strings.ToLower(strings.TrimSpace(role))
	if value == "" {
		value = strings.ToLower(strings.TrimSpace(messageType))
	}
	switch value {
	case "human", "user":
		return domain.RoleUser, nil
	case "ai", "assistant":
		return domain.RoleAssistant, nil
	case "system":
		return domain.RoleSystem, nil
	case "tool":
		return domain.RoleTool, nil
	default:
		return "", fmt.Errorf("unsupported role %q", value)
	}
}

func decodeContents(role domain.Role, input wireMessage) ([]domain.Content, error) {
	if role == domain.RoleTool {
		return decodeToolResult(input)
	}
	contents, err := decodeContentBlocks(input.Content)
	if err != nil {
		return nil, err
	}
	if role != domain.RoleAssistant || len(input.ToolCalls) == 0 {
		return contents, nil
	}
	for _, rawCall := range input.ToolCalls {
		arguments := rawCall.Arguments
		if len(arguments) == 0 {
			arguments = rawCall.Args
		}
		if len(arguments) == 0 {
			arguments = json.RawMessage(`{}`)
		}
		if rawCall.ID == "" || rawCall.Name == "" || !json.Valid(arguments) {
			return nil, errors.New("assistant tool call requires id, name, and JSON arguments")
		}
		call := domain.ToolCall{ID: rawCall.ID, Name: rawCall.Name, Arguments: append(json.RawMessage(nil), arguments...)}
		contents = append(contents, domain.Content{Kind: domain.ContentToolCall, ToolCall: &call})
	}
	return contents, nil
}

func decodeContentBlocks(raw json.RawMessage) ([]domain.Content, error) {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if text == "" {
			return nil, errors.New("message content is empty")
		}
		return []domain.Content{{Kind: domain.ContentText, Text: text}}, nil
	}
	var blocks []struct {
		Type      string `json:"type"`
		Text      string `json:"text"`
		URL       string `json:"url"`
		MediaType string `json:"media_type"`
		ImageURL  *struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil || len(blocks) == 0 {
		return nil, errors.New("message content must be a string or non-empty block array")
	}
	contents := make([]domain.Content, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if block.Text == "" {
				return nil, errors.New("text content is empty")
			}
			contents = append(contents, domain.Content{Kind: domain.ContentText, Text: block.Text})
		case "image", "image_url":
			url := block.URL
			if block.ImageURL != nil {
				url = block.ImageURL.URL
			}
			if url == "" {
				return nil, errors.New("image content URL is empty")
			}
			mediaType := block.MediaType
			if mediaType == "" {
				mediaType = "image/*"
			}
			contents = append(contents, domain.Content{Kind: domain.ContentImage, URL: url, MediaType: mediaType})
		default:
			return nil, fmt.Errorf("unsupported content block %q", block.Type)
		}
	}
	return contents, nil
}

func decodeToolResult(input wireMessage) ([]domain.Content, error) {
	if strings.TrimSpace(input.ToolCallID) == "" {
		return nil, errors.New("tool message requires tool_call_id")
	}
	var output json.RawMessage
	if !json.Valid(input.Content) {
		return nil, errors.New("tool content must be valid JSON")
	}
	var text string
	if err := json.Unmarshal(input.Content, &text); err == nil {
		output, _ = json.Marshal(text)
	} else {
		output = append(json.RawMessage(nil), input.Content...)
	}
	result := domain.ToolResult{CallID: input.ToolCallID, Output: output}
	return []domain.Content{{Kind: domain.ContentToolResult, ToolResult: &result}}, nil
}

func strictJSON(raw []byte, output any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("multiple JSON values")
	}
	return nil
}
