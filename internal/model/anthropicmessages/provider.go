package anthropicmessages

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/model"
)

const defaultMaxTokens = 8192

var (
	// ErrInvalidConfig identifies invalid Anthropic Messages configuration.
	ErrInvalidConfig = errors.New("invalid Anthropic Messages config")
	// ErrProtocol identifies malformed or unsupported provider stream output.
	ErrProtocol = errors.New("anthropic messages protocol violation")
)

// Config configures the native Anthropic Messages adapter. Empty credentials
// and endpoint values defer to the SDK credential and endpoint chain.
type Config struct {
	APIKey     string
	AuthToken  string
	BaseURL    string
	MaxTokens  int
	HTTPClient *http.Client
}

// Provider implements model.Provider with Anthropic's official Go SDK.
type Provider struct {
	client    anthropic.Client
	maxTokens int
}

// New validates config and constructs an Anthropic Messages provider.
func New(config Config) (*Provider, error) {
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	options := make([]option.RequestOption, 0, 4)
	if config.APIKey != "" {
		options = append(options, option.WithAPIKey(config.APIKey))
	}
	if config.AuthToken != "" {
		options = append(options, option.WithAuthToken(config.AuthToken))
	}
	if config.BaseURL != "" {
		options = append(options, option.WithBaseURL(config.BaseURL))
	}
	if config.HTTPClient != nil {
		options = append(options, option.WithHTTPClient(config.HTTPClient))
	}
	maxTokens := config.MaxTokens
	if maxTokens == 0 {
		maxTokens = defaultMaxTokens
	}
	return &Provider{client: anthropic.NewClient(options...), maxTokens: maxTokens}, nil
}

// Stream opens a normalized native Messages request.
func (provider *Provider) Stream(ctx context.Context, request model.Request) (model.Stream, error) {
	if provider == nil {
		return nil, fmt.Errorf("%w: provider is nil", ErrInvalidConfig)
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	params, err := buildParams(request, provider.maxTokens)
	if err != nil {
		return nil, err
	}
	source := provider.client.Messages.NewStreaming(ctx, params)
	return &stream{
		source: source, blocks: make(map[int64]*pendingBlock), calls: make(map[int64]*pendingCall),
	}, nil
}

func validateConfig(config Config) error {
	if strings.TrimSpace(config.APIKey) != config.APIKey || strings.TrimSpace(config.AuthToken) != config.AuthToken ||
		strings.TrimSpace(config.BaseURL) != config.BaseURL {
		return fmt.Errorf("%w: credentials and endpoint must not contain surrounding whitespace", ErrInvalidConfig)
	}
	if config.APIKey != "" && config.AuthToken != "" {
		return fmt.Errorf("%w: API key and auth token are mutually exclusive", ErrInvalidConfig)
	}
	if config.MaxTokens < 0 {
		return fmt.Errorf("%w: max tokens cannot be negative", ErrInvalidConfig)
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

func buildParams(request model.Request, defaultTokens int) (anthropic.MessageNewParams, error) {
	messages, system, err := convertMessages(request)
	if err != nil {
		return anthropic.MessageNewParams{}, err
	}
	tools, err := convertTools(request.Tools)
	if err != nil {
		return anthropic.MessageNewParams{}, err
	}
	maxTokens := request.MaxTokens
	if maxTokens == 0 {
		maxTokens = defaultTokens
	}
	params := anthropic.MessageNewParams{
		Model: anthropic.Model(request.Model), MaxTokens: int64(maxTokens),
		Messages: messages, System: system, Tools: tools,
	}
	if request.Temperature != nil {
		if *request.Temperature > 1 {
			return anthropic.MessageNewParams{}, fmt.Errorf("%w: Anthropic temperature must be between 0 and 1", model.ErrInvalidRequest)
		}
		params.Temperature = anthropic.Float(*request.Temperature)
	}
	return params, nil
}

func convertMessages(request model.Request) ([]anthropic.MessageParam, []anthropic.TextBlockParam, error) {
	messages := make([]anthropic.MessageParam, 0, len(request.Messages))
	system := make([]anthropic.TextBlockParam, 0, 1)
	if request.System != "" {
		system = append(system, anthropic.TextBlockParam{Text: request.System})
	}
	for index, message := range request.Messages {
		if message.Role == domain.RoleSystem {
			blocks, err := convertSystemMessage(message.Content)
			if err != nil {
				return nil, nil, fmt.Errorf("convert message %d: %w", index, err)
			}
			system = append(system, blocks...)
			continue
		}
		converted, err := convertMessage(message)
		if err != nil {
			return nil, nil, fmt.Errorf("convert message %d: %w", index, err)
		}
		messages = append(messages, converted)
	}
	return messages, system, nil
}

func convertSystemMessage(contents []domain.Content) ([]anthropic.TextBlockParam, error) {
	blocks := make([]anthropic.TextBlockParam, 0, len(contents))
	for _, content := range contents {
		if content.Kind != domain.ContentText {
			return nil, fmt.Errorf("system content type %q is unsupported", content.Kind)
		}
		blocks = append(blocks, anthropic.TextBlockParam{Text: content.Text})
	}
	return blocks, nil
}

func convertMessage(message domain.Message) (anthropic.MessageParam, error) {
	switch message.Role {
	case domain.RoleUser:
		return convertUserMessage(message.Content)
	case domain.RoleAssistant:
		return convertAssistantMessage(message.Content)
	case domain.RoleTool:
		return convertToolMessage(message.Content)
	case domain.RoleSystem:
		return anthropic.MessageParam{}, errors.New("system messages must use the top-level system field")
	default:
		return anthropic.MessageParam{}, fmt.Errorf("unsupported role %q", message.Role)
	}
}

func convertUserMessage(contents []domain.Content) (anthropic.MessageParam, error) {
	blocks := make([]anthropic.ContentBlockParamUnion, 0, len(contents))
	for _, content := range contents {
		switch content.Kind {
		case domain.ContentText:
			blocks = append(blocks, anthropic.NewTextBlock(content.Text))
		case domain.ContentImage:
			block, err := convertImage(content)
			if err != nil {
				return anthropic.MessageParam{}, err
			}
			blocks = append(blocks, block)
		case domain.ContentToolCall, domain.ContentToolResult:
			return anthropic.MessageParam{}, fmt.Errorf("user content type %q is unsupported", content.Kind)
		default:
			return anthropic.MessageParam{}, fmt.Errorf("user content type %q is unsupported", content.Kind)
		}
	}
	return anthropic.NewUserMessage(blocks...), nil
}

func convertImage(content domain.Content) (anthropic.ContentBlockParamUnion, error) {
	if !supportedImageMediaType(content.MediaType) {
		return anthropic.ContentBlockParamUnion{}, fmt.Errorf("unsupported Anthropic image media type %q", content.MediaType)
	}
	if !strings.HasPrefix(content.URL, "data:") {
		return anthropic.NewImageBlock(anthropic.URLImageSourceParam{URL: content.URL}), nil
	}
	header, payload, ok := strings.Cut(content.URL, ",")
	wantHeader := "data:" + content.MediaType + ";base64"
	if !ok || header != wantHeader {
		return anthropic.ContentBlockParamUnion{}, errors.New("image data URL must match its media type and use base64")
	}
	if _, err := base64.StdEncoding.DecodeString(payload); err != nil {
		return anthropic.ContentBlockParamUnion{}, fmt.Errorf("decode image data URL: %w", err)
	}
	return anthropic.NewImageBlockBase64(content.MediaType, payload), nil
}

func supportedImageMediaType(mediaType string) bool {
	switch mediaType {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return true
	default:
		return false
	}
}

func convertAssistantMessage(contents []domain.Content) (anthropic.MessageParam, error) {
	blocks := make([]anthropic.ContentBlockParamUnion, 0, len(contents))
	for _, content := range contents {
		switch content.Kind {
		case domain.ContentText:
			blocks = append(blocks, anthropic.NewTextBlock(content.Text))
		case domain.ContentToolCall:
			if content.ToolCall == nil {
				return anthropic.MessageParam{}, errors.New("assistant tool call is nil")
			}
			var input any
			if err := json.Unmarshal(content.ToolCall.Arguments, &input); err != nil {
				return anthropic.MessageParam{}, fmt.Errorf("decode assistant tool call: %w", err)
			}
			blocks = append(blocks, anthropic.NewToolUseBlock(content.ToolCall.ID, input, content.ToolCall.Name))
		case domain.ContentImage, domain.ContentToolResult:
			return anthropic.MessageParam{}, fmt.Errorf("assistant content type %q is unsupported", content.Kind)
		default:
			return anthropic.MessageParam{}, fmt.Errorf("assistant content type %q is unsupported", content.Kind)
		}
	}
	return anthropic.NewAssistantMessage(blocks...), nil
}

func convertToolMessage(contents []domain.Content) (anthropic.MessageParam, error) {
	if len(contents) != 1 || contents[0].Kind != domain.ContentToolResult || contents[0].ToolResult == nil {
		return anthropic.MessageParam{}, errors.New("tool message must contain exactly one tool result")
	}
	result := contents[0].ToolResult
	return anthropic.NewUserMessage(anthropic.NewToolResultBlock(
		result.CallID, toolMessageContent(result.Output), result.IsError,
	)), nil
}

func toolMessageContent(output json.RawMessage) string {
	var text string
	if json.Unmarshal(output, &text) == nil {
		return text
	}
	return string(output)
}

func convertTools(definitions []model.ToolDefinition) ([]anthropic.ToolUnionParam, error) {
	tools := make([]anthropic.ToolUnionParam, 0, len(definitions))
	for _, definition := range definitions {
		var raw map[string]any
		if err := json.Unmarshal(definition.InputSchema, &raw); err != nil {
			return nil, fmt.Errorf("decode schema for tool %q: %w", definition.Name, err)
		}
		if schemaType, _ := raw["type"].(string); schemaType != "object" {
			return nil, fmt.Errorf("decode schema for tool %q: top-level type must be object", definition.Name)
		}
		var schema anthropic.ToolInputSchemaParam
		if err := json.Unmarshal(definition.InputSchema, &schema); err != nil {
			return nil, fmt.Errorf("decode schema for tool %q: %w", definition.Name, err)
		}
		tool := anthropic.ToolUnionParamOfTool(schema, definition.Name)
		tool.OfTool.Description = anthropic.String(definition.Description)
		tools = append(tools, tool)
	}
	return tools, nil
}

type eventSource interface {
	Next() bool
	Current() anthropic.MessageStreamEventUnion
	Err() error
	Close() error
}

type pendingBlock struct {
	kind      string
	id        string
	name      string
	initial   json.RawMessage
	arguments strings.Builder
	hasDeltas bool
}

type pendingCall struct {
	id        string
	name      string
	arguments json.RawMessage
}

type stream struct {
	source      eventSource
	queue       []model.Chunk
	blocks      map[int64]*pendingBlock
	calls       map[int64]*pendingCall
	usage       model.Usage
	stopReason  model.StopReason
	started     bool
	deltaSeen   bool
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
		return model.Chunk{}, fmt.Errorf("receive Anthropic Messages stream: %w", err)
	}
	if !stream.finished {
		return model.Chunk{}, fmt.Errorf("%w: stream ended before message_stop", ErrProtocol)
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

func (stream *stream) ingest(event anthropic.MessageStreamEventUnion) error {
	if stream.finished {
		return fmt.Errorf("%w: event received after message_stop", ErrProtocol)
	}
	switch event.Type {
	case "message_start":
		return stream.ingestMessageStart(event.Message)
	case "content_block_start":
		return stream.ingestBlockStart(event.Index, event.ContentBlock)
	case "content_block_delta":
		return stream.ingestBlockDelta(event.Index, event.Delta)
	case "content_block_stop":
		return stream.ingestBlockStop(event.Index)
	case "message_delta":
		return stream.ingestMessageDelta(event.Delta.StopReason, event.Usage)
	case "message_stop":
		return stream.ingestMessageStop()
	default:
		return fmt.Errorf("%w: unsupported event %q", ErrProtocol, event.Type)
	}
}

func (stream *stream) ingestMessageStart(message anthropic.Message) error {
	if stream.started {
		return fmt.Errorf("%w: duplicate message_start", ErrProtocol)
	}
	stream.started = true
	stream.usage = normalizeUsage(message.Usage)
	return nil
}

func (stream *stream) ingestBlockStart(index int64, content anthropic.ContentBlockStartEventContentBlockUnion) error {
	if err := stream.requireContentEvent(index); err != nil {
		return err
	}
	if _, exists := stream.blocks[index]; exists {
		return fmt.Errorf("%w: duplicate content block %d", ErrProtocol, index)
	}
	block := &pendingBlock{kind: content.Type}
	switch content.Type {
	case "text":
		if content.Text != "" {
			stream.queue = append(stream.queue, model.Chunk{Kind: model.ChunkTextDelta, Text: content.Text})
		}
	case "thinking", "redacted_thinking":
	case "tool_use":
		block.id, block.name = content.ID, content.Name
		if content.Input != nil {
			encoded, err := json.Marshal(content.Input)
			if err != nil {
				return fmt.Errorf("%w: encode tool input at block %d: %w", ErrProtocol, index, err)
			}
			block.initial = encoded
		}
	default:
		return fmt.Errorf("%w: unsupported content block %q", ErrProtocol, content.Type)
	}
	stream.blocks[index] = block
	return nil
}

func (stream *stream) ingestBlockDelta(index int64, delta anthropic.MessageStreamEventUnionDelta) error {
	if err := stream.requireContentEvent(index); err != nil {
		return err
	}
	block := stream.blocks[index]
	if block == nil {
		return fmt.Errorf("%w: delta for unopened content block %d", ErrProtocol, index)
	}
	switch delta.Type {
	case "text_delta":
		if block.kind != "text" || delta.Text == "" {
			return fmt.Errorf("%w: invalid text delta for block %d", ErrProtocol, index)
		}
		stream.queue = append(stream.queue, model.Chunk{Kind: model.ChunkTextDelta, Text: delta.Text})
	case "input_json_delta":
		if block.kind != "tool_use" {
			return fmt.Errorf("%w: tool input delta for block %d of type %q", ErrProtocol, index, block.kind)
		}
		block.hasDeltas = true
		block.arguments.WriteString(delta.PartialJSON)
	case "thinking_delta", "signature_delta":
		if block.kind != "thinking" {
			return fmt.Errorf("%w: reasoning delta for block %d of type %q", ErrProtocol, index, block.kind)
		}
	case "citations_delta":
		if block.kind != "text" {
			return fmt.Errorf("%w: citation delta for block %d of type %q", ErrProtocol, index, block.kind)
		}
	default:
		return fmt.Errorf("%w: unsupported content delta %q", ErrProtocol, delta.Type)
	}
	return nil
}

func (stream *stream) ingestBlockStop(index int64) error {
	if err := stream.requireContentEvent(index); err != nil {
		return err
	}
	block := stream.blocks[index]
	if block == nil {
		return fmt.Errorf("%w: stop for unopened content block %d", ErrProtocol, index)
	}
	delete(stream.blocks, index)
	if block.kind != "tool_use" {
		return nil
	}
	arguments := block.initial
	if block.hasDeltas {
		arguments = json.RawMessage(block.arguments.String())
	}
	stream.calls[index] = &pendingCall{id: block.id, name: block.name, arguments: arguments}
	return nil
}

func (stream *stream) ingestMessageDelta(reason anthropic.StopReason, usage anthropic.MessageDeltaUsage) error {
	if !stream.started || stream.deltaSeen {
		return fmt.Errorf("%w: unexpected message_delta", ErrProtocol)
	}
	if len(stream.blocks) != 0 {
		return fmt.Errorf("%w: message_delta with %d open content blocks", ErrProtocol, len(stream.blocks))
	}
	normalized, err := normalizeStop(reason)
	if err != nil {
		return err
	}
	if invalidCallStopPair(len(stream.calls) != 0, normalized) {
		return fmt.Errorf("%w: stop reason %q with %d tool calls", ErrProtocol, reason, len(stream.calls))
	}
	if err := stream.flushCalls(); err != nil {
		return err
	}
	stream.usage = mergeUsage(stream.usage, usage)
	stream.queue = append(stream.queue, model.Chunk{Kind: model.ChunkUsage, Usage: &stream.usage})
	stream.stopReason = normalized
	stream.deltaSeen = true
	return nil
}

func (stream *stream) ingestMessageStop() error {
	if !stream.deltaSeen {
		return fmt.Errorf("%w: message_stop before message_delta", ErrProtocol)
	}
	stream.finished = true
	return nil
}

func (stream *stream) requireContentEvent(index int64) error {
	if !stream.started || stream.deltaSeen || index < 0 {
		return fmt.Errorf("%w: unexpected content event at index %d", ErrProtocol, index)
	}
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
		call := &domain.ToolCall{ID: pending.id, Name: pending.name, Arguments: pending.arguments}
		chunk := model.Chunk{Kind: model.ChunkToolCall, ToolCall: call}
		if err := chunk.Validate(); err != nil {
			return fmt.Errorf("%w: tool call at block %d: %w", ErrProtocol, index, err)
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

func normalizeStop(reason anthropic.StopReason) (model.StopReason, error) {
	switch reason {
	case anthropic.StopReasonEndTurn, anthropic.StopReasonStopSequence:
		return model.StopEndTurn, nil
	case anthropic.StopReasonToolUse:
		return model.StopToolUse, nil
	case anthropic.StopReasonMaxTokens, anthropic.StopReasonModelContextWindowExceeded:
		return model.StopMaxTokens, nil
	case anthropic.StopReasonRefusal:
		return model.StopContentFilter, nil
	case anthropic.StopReasonPauseTurn:
		return "", fmt.Errorf("%w: pause_turn continuation is unsupported", ErrProtocol)
	default:
		return "", fmt.Errorf("%w: unsupported stop reason %q", ErrProtocol, reason)
	}
}

func invalidCallStopPair(hasCalls bool, reason model.StopReason) bool {
	if reason == model.StopMaxTokens || reason == model.StopContentFilter {
		return false
	}
	return hasCalls != (reason == model.StopToolUse)
}

func normalizeUsage(usage anthropic.Usage) model.Usage {
	return model.Usage{
		InputTokens:      boundedInt(usage.InputTokens),
		OutputTokens:     boundedInt(usage.OutputTokens),
		ReasoningTokens:  boundedInt(usage.OutputTokensDetails.ThinkingTokens),
		CacheReadTokens:  boundedInt(usage.CacheReadInputTokens),
		CacheWriteTokens: boundedInt(usage.CacheCreationInputTokens),
	}
}

func mergeUsage(current model.Usage, usage anthropic.MessageDeltaUsage) model.Usage {
	if usage.JSON.InputTokens.Valid() || usage.InputTokens != 0 {
		current.InputTokens = boundedInt(usage.InputTokens)
	}
	if usage.JSON.CacheReadInputTokens.Valid() || usage.CacheReadInputTokens != 0 {
		current.CacheReadTokens = boundedInt(usage.CacheReadInputTokens)
	}
	if usage.JSON.CacheCreationInputTokens.Valid() || usage.CacheCreationInputTokens != 0 {
		current.CacheWriteTokens = boundedInt(usage.CacheCreationInputTokens)
	}
	current.OutputTokens = boundedInt(usage.OutputTokens)
	current.ReasoningTokens = boundedInt(usage.OutputTokensDetails.ThinkingTokens)
	return current
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
