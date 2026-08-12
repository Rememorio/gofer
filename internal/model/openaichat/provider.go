package openaichat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/shared"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/model"
)

var (
	// ErrInvalidConfig identifies an invalid OpenAI Chat adapter configuration.
	ErrInvalidConfig = errors.New("invalid OpenAI Chat config")
	// ErrProtocol identifies malformed or unsupported provider stream output.
	ErrProtocol = errors.New("OpenAI Chat protocol violation")
)

// Config configures an OpenAI or OpenAI-compatible Chat Completions endpoint.
// Empty credentials and endpoint values defer to the SDK environment defaults.
type Config struct {
	APIKey     string
	BaseURL    string
	HTTPClient *http.Client
}

// Provider implements model.Provider with the official OpenAI Go SDK.
type Provider struct {
	client openai.Client
}

// New validates config and constructs an OpenAI Chat provider.
func New(config Config) (*Provider, error) {
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	options := make([]option.RequestOption, 0, 3)
	if config.APIKey != "" {
		options = append(options, option.WithAPIKey(config.APIKey))
	}
	if config.BaseURL != "" {
		options = append(options, option.WithBaseURL(config.BaseURL))
	}
	if config.HTTPClient != nil {
		options = append(options, option.WithHTTPClient(config.HTTPClient))
	}
	return &Provider{client: openai.NewClient(options...)}, nil
}

// Stream opens a normalized streaming Chat Completions request.
func (provider *Provider) Stream(ctx context.Context, request model.Request) (model.Stream, error) {
	if provider == nil {
		return nil, fmt.Errorf("%w: provider is nil", ErrInvalidConfig)
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	params, err := buildParams(request)
	if err != nil {
		return nil, err
	}
	source := provider.client.Chat.Completions.NewStreaming(ctx, params)
	return &stream{source: source, calls: make(map[int64]*pendingCall)}, nil
}

func validateConfig(config Config) error {
	if strings.TrimSpace(config.APIKey) != config.APIKey || strings.TrimSpace(config.BaseURL) != config.BaseURL {
		return fmt.Errorf("%w: credentials and endpoint must not contain surrounding whitespace", ErrInvalidConfig)
	}
	if config.BaseURL == "" {
		return nil
	}
	endpoint, err := url.Parse(config.BaseURL)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return fmt.Errorf("%w: base URL must be an absolute HTTP(S) URL", ErrInvalidConfig)
	}
	return nil
}

func buildParams(request model.Request) (openai.ChatCompletionNewParams, error) {
	messages, err := convertMessages(request)
	if err != nil {
		return openai.ChatCompletionNewParams{}, err
	}
	tools, err := convertTools(request.Tools)
	if err != nil {
		return openai.ChatCompletionNewParams{}, err
	}
	params := openai.ChatCompletionNewParams{
		Model:    shared.ChatModel(request.Model),
		Messages: messages,
		Tools:    tools,
		StreamOptions: openai.ChatCompletionStreamOptionsParam{
			IncludeUsage: param.NewOpt(true),
		},
	}
	if request.MaxTokens > 0 {
		params.MaxCompletionTokens = param.NewOpt(int64(request.MaxTokens))
	}
	if request.Temperature != nil {
		params.Temperature = param.NewOpt(*request.Temperature)
	}
	if len(tools) > 0 {
		params.ParallelToolCalls = param.NewOpt(true)
	}
	return params, nil
}

func convertMessages(request model.Request) ([]openai.ChatCompletionMessageParamUnion, error) {
	capacity := len(request.Messages)
	if request.System != "" {
		capacity++
	}
	messages := make([]openai.ChatCompletionMessageParamUnion, 0, capacity)
	if request.System != "" {
		messages = append(messages, openai.SystemMessage(request.System))
	}
	for index, message := range request.Messages {
		converted, err := convertMessage(message)
		if err != nil {
			return nil, fmt.Errorf("convert message %d: %w", index, err)
		}
		messages = append(messages, converted)
	}
	return messages, nil
}

func convertMessage(message domain.Message) (openai.ChatCompletionMessageParamUnion, error) {
	switch message.Role {
	case domain.RoleSystem:
		text, err := textOnly(message.Content)
		return openai.SystemMessage(text), err
	case domain.RoleUser:
		return convertUserMessage(message.Content)
	case domain.RoleAssistant:
		return convertAssistantMessage(message.Content)
	case domain.RoleTool:
		return convertToolMessage(message.Content)
	default:
		return openai.ChatCompletionMessageParamUnion{}, fmt.Errorf("unsupported role %q", message.Role)
	}
}

func convertUserMessage(contents []domain.Content) (openai.ChatCompletionMessageParamUnion, error) {
	parts := make([]openai.ChatCompletionContentPartUnionParam, 0, len(contents))
	for _, content := range contents {
		switch content.Kind {
		case domain.ContentText:
			parts = append(parts, openai.TextContentPart(content.Text))
		case domain.ContentImage:
			parts = append(parts, openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{
				URL: content.URL,
			}))
		case domain.ContentToolCall, domain.ContentToolResult:
			return openai.ChatCompletionMessageParamUnion{}, fmt.Errorf("user content type %q is unsupported", content.Kind)
		default:
			return openai.ChatCompletionMessageParamUnion{}, fmt.Errorf("user content type %q is unsupported", content.Kind)
		}
	}
	return openai.UserMessage(parts), nil
}

func convertAssistantMessage(contents []domain.Content) (openai.ChatCompletionMessageParamUnion, error) {
	var text strings.Builder
	calls := make([]openai.ChatCompletionMessageToolCallUnionParam, 0)
	for _, content := range contents {
		switch content.Kind {
		case domain.ContentText:
			text.WriteString(content.Text)
		case domain.ContentToolCall:
			call := content.ToolCall
			if call == nil {
				return openai.ChatCompletionMessageParamUnion{}, errors.New("assistant tool call is nil")
			}
			calls = append(calls, openai.ChatCompletionMessageToolCallUnionParam{
				OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
					ID: call.ID,
					Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
						Name: call.Name, Arguments: string(call.Arguments),
					},
				},
			})
		case domain.ContentImage, domain.ContentToolResult:
			return openai.ChatCompletionMessageParamUnion{}, fmt.Errorf("assistant content type %q is unsupported", content.Kind)
		default:
			return openai.ChatCompletionMessageParamUnion{}, fmt.Errorf("assistant content type %q is unsupported", content.Kind)
		}
	}
	assistant := openai.ChatCompletionAssistantMessageParam{ToolCalls: calls}
	if text.Len() > 0 {
		assistant.Content.OfString = param.NewOpt(text.String())
	}
	return openai.ChatCompletionMessageParamUnion{OfAssistant: &assistant}, nil
}

func convertToolMessage(contents []domain.Content) (openai.ChatCompletionMessageParamUnion, error) {
	if len(contents) != 1 || contents[0].Kind != domain.ContentToolResult || contents[0].ToolResult == nil {
		return openai.ChatCompletionMessageParamUnion{}, errors.New("tool message must contain exactly one tool result")
	}
	result := contents[0].ToolResult
	return openai.ToolMessage(toolMessageContent(result.Output), result.CallID), nil
}

func toolMessageContent(output json.RawMessage) string {
	var text string
	if json.Unmarshal(output, &text) == nil {
		return text
	}
	return string(output)
}

func textOnly(contents []domain.Content) (string, error) {
	var text strings.Builder
	for _, content := range contents {
		if content.Kind != domain.ContentText {
			return "", fmt.Errorf("system content type %q is unsupported", content.Kind)
		}
		text.WriteString(content.Text)
	}
	return text.String(), nil
}

func convertTools(definitions []model.ToolDefinition) ([]openai.ChatCompletionToolUnionParam, error) {
	tools := make([]openai.ChatCompletionToolUnionParam, 0, len(definitions))
	for _, definition := range definitions {
		var parameters shared.FunctionParameters
		if err := json.Unmarshal(definition.InputSchema, &parameters); err != nil {
			return nil, fmt.Errorf("decode schema for tool %q: %w", definition.Name, err)
		}
		tools = append(tools, openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
			Name: definition.Name, Description: param.NewOpt(definition.Description), Parameters: parameters,
		}))
	}
	return tools, nil
}

type chunkSource interface {
	Next() bool
	Current() openai.ChatCompletionChunk
	Err() error
	Close() error
}

type pendingCall struct {
	id        string
	name      string
	arguments strings.Builder
}

type stream struct {
	source      chunkSource
	queue       []model.Chunk
	calls       map[int64]*pendingCall
	stopReason  model.StopReason
	finished    bool
	doneEmitted bool
	closed      bool
}

func (stream *stream) Recv() (model.Chunk, error) {
	if len(stream.queue) > 0 {
		return stream.pop(), nil
	}
	if stream.closed || stream.doneEmitted {
		return model.Chunk{}, io.EOF
	}
	for stream.source.Next() {
		if err := stream.ingest(stream.source.Current()); err != nil {
			return model.Chunk{}, err
		}
		if len(stream.queue) > 0 {
			return stream.pop(), nil
		}
	}
	if err := stream.source.Err(); err != nil {
		return model.Chunk{}, fmt.Errorf("receive OpenAI Chat stream: %w", err)
	}
	if !stream.finished {
		return model.Chunk{}, fmt.Errorf("%w: stream ended without finish reason", ErrProtocol)
	}
	stream.doneEmitted = true
	return model.Chunk{Kind: model.ChunkDone, StopReason: stream.stopReason}, nil
}

func (stream *stream) Close() error {
	if stream.closed {
		return nil
	}
	stream.closed = true
	return stream.source.Close()
}

func (stream *stream) ingest(chunk openai.ChatCompletionChunk) error {
	if len(chunk.Choices) > 1 {
		return fmt.Errorf("%w: received %d choices", ErrProtocol, len(chunk.Choices))
	}
	if len(chunk.Choices) == 1 {
		if err := stream.ingestChoice(chunk.Choices[0]); err != nil {
			return err
		}
	}
	if chunk.JSON.Usage.Valid() {
		usage := normalizeUsage(chunk.Usage)
		stream.queue = append(stream.queue, model.Chunk{Kind: model.ChunkUsage, Usage: &usage})
	}
	return nil
}

func (stream *stream) ingestChoice(choice openai.ChatCompletionChunkChoice) error {
	if stream.finished {
		return fmt.Errorf("%w: choice received after finish", ErrProtocol)
	}
	if choice.Index != 0 {
		return fmt.Errorf("%w: unexpected choice index %d", ErrProtocol, choice.Index)
	}
	if choice.Delta.Content != "" {
		stream.queue = append(stream.queue, model.Chunk{Kind: model.ChunkTextDelta, Text: choice.Delta.Content})
	}
	if choice.Delta.Refusal != "" {
		stream.queue = append(stream.queue, model.Chunk{Kind: model.ChunkTextDelta, Text: choice.Delta.Refusal})
	}
	if err := stream.accumulateCalls(choice.Delta.ToolCalls); err != nil {
		return err
	}
	if choice.FinishReason == "" {
		return nil
	}
	reason, err := normalizeStop(choice.FinishReason)
	if err != nil {
		return err
	}
	hasCalls := len(stream.calls) > 0
	if invalidCallStopPair(hasCalls, reason) {
		return fmt.Errorf("%w: finish reason %q with %d tool calls", ErrProtocol, choice.FinishReason, len(stream.calls))
	}
	if err := stream.flushCalls(); err != nil {
		return err
	}
	stream.stopReason = reason
	stream.finished = true
	return nil
}

func invalidCallStopPair(hasCalls bool, reason model.StopReason) bool {
	if reason == model.StopMaxTokens || reason == model.StopContentFilter {
		return false
	}
	return hasCalls != (reason == model.StopToolUse)
}

func (stream *stream) accumulateCalls(deltas []openai.ChatCompletionChunkChoiceDeltaToolCall) error {
	if stream.finished && len(deltas) > 0 {
		return fmt.Errorf("%w: tool call delta after finish", ErrProtocol)
	}
	for _, delta := range deltas {
		if err := stream.accumulateCall(delta); err != nil {
			return err
		}
	}
	return nil
}

func (stream *stream) accumulateCall(delta openai.ChatCompletionChunkChoiceDeltaToolCall) error {
	if delta.Index < 0 {
		return fmt.Errorf("%w: negative tool call index %d", ErrProtocol, delta.Index)
	}
	if delta.Type != "" && delta.Type != "function" {
		return fmt.Errorf("%w: unsupported tool type %q", ErrProtocol, delta.Type)
	}
	call := stream.calls[delta.Index]
	if call == nil {
		call = &pendingCall{}
		stream.calls[delta.Index] = call
	}
	if delta.ID != "" {
		if call.id != "" && call.id != delta.ID {
			return fmt.Errorf("%w: tool call %d changed ID", ErrProtocol, delta.Index)
		}
		call.id = delta.ID
	}
	if delta.Function.Name != "" {
		if call.name != "" && call.name != delta.Function.Name {
			return fmt.Errorf("%w: tool call %d changed name", ErrProtocol, delta.Index)
		}
		call.name = delta.Function.Name
	}
	call.arguments.WriteString(delta.Function.Arguments)
	return nil
}

func (stream *stream) flushCalls() error {
	indexes := make([]int64, 0, len(stream.calls))
	for index := range stream.calls {
		indexes = append(indexes, index)
	}
	sort.Slice(indexes, func(left, right int) bool { return indexes[left] < indexes[right] })
	for _, index := range indexes {
		pending := stream.calls[index]
		arguments := json.RawMessage(pending.arguments.String())
		call := &domain.ToolCall{ID: pending.id, Name: pending.name, Arguments: arguments}
		chunk := model.Chunk{Kind: model.ChunkToolCall, ToolCall: call}
		if err := chunk.Validate(); err != nil {
			return fmt.Errorf("%w: tool call %d: %w", ErrProtocol, index, err)
		}
		stream.queue = append(stream.queue, chunk)
	}
	clear(stream.calls)
	return nil
}

func (stream *stream) pop() model.Chunk {
	chunk := stream.queue[0]
	stream.queue = stream.queue[1:]
	return chunk
}

func normalizeStop(reason string) (model.StopReason, error) {
	switch reason {
	case "stop":
		return model.StopEndTurn, nil
	case "tool_calls", "function_call":
		return model.StopToolUse, nil
	case "length":
		return model.StopMaxTokens, nil
	case "content_filter":
		return model.StopContentFilter, nil
	default:
		return "", fmt.Errorf("%w: unsupported finish reason %q", ErrProtocol, reason)
	}
}

func normalizeUsage(usage openai.CompletionUsage) model.Usage {
	return model.Usage{
		InputTokens:      boundedInt(usage.PromptTokens),
		OutputTokens:     boundedInt(usage.CompletionTokens),
		ReasoningTokens:  boundedInt(usage.CompletionTokensDetails.ReasoningTokens),
		CacheReadTokens:  boundedInt(usage.PromptTokensDetails.CachedTokens),
		CacheWriteTokens: boundedInt(usage.PromptTokensDetails.CacheWriteTokens),
	}
}

func boundedInt(value int64) int {
	if value > int64(math.MaxInt) {
		return math.MaxInt
	}
	if value < 0 {
		return 0
	}
	return int(value)
}
