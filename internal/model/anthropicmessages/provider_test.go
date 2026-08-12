package anthropicmessages

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/model"
)

func TestProviderStreamsParallelToolCallsAndUsage(t *testing.T) {
	t.Parallel()
	type capturedRequest struct {
		path, apiKey, authorization string
		body                        []byte
	}
	captured := make(chan capturedRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		captured <- capturedRequest{
			path: request.URL.Path, apiKey: request.Header.Get("X-Api-Key"),
			authorization: request.Header.Get("Authorization"), body: body,
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		writeEvents(writer,
			anthropicEvent("message_start", `{"type":"message_start","message":{"id":"msg-1","type":"message","role":"assistant","model":"claude-test","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":21,"output_tokens":1,"cache_read_input_tokens":5,"cache_creation_input_tokens":3}}}`),
			anthropicEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
			anthropicEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"working "}}`),
			anthropicEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
			anthropicEvent("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call-1","name":"alpha","input":{}}}`),
			anthropicEvent("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"go\"}"}}`),
			anthropicEvent("content_block_stop", `{"type":"content_block_stop","index":1}`),
			anthropicEvent("content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"call-2","name":"beta","input":{}}}`),
			anthropicEvent("content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"n\":"}}`),
			anthropicEvent("content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"2}"}}`),
			anthropicEvent("content_block_stop", `{"type":"content_block_stop","index":2}`),
			anthropicEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":8,"output_tokens_details":{"thinking_tokens":2}}}`),
			anthropicEvent("message_stop", `{"type":"message_stop"}`),
		)
	}))
	defer server.Close()

	provider, err := New(Config{APIKey: "test-key", BaseURL: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := provider.Stream(context.Background(), completeRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	response, err := model.Collect(stream, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = stream.Close(); err != nil {
		t.Fatal(err)
	}
	if response.Text != "working " || response.StopReason != model.StopToolUse || len(response.ToolCalls) != 2 {
		t.Fatalf("response = %#v", response)
	}
	if response.ToolCalls[0].ID != "call-1" || string(response.ToolCalls[1].Arguments) != `{"n":2}` {
		t.Fatalf("tool calls = %#v", response.ToolCalls)
	}
	wantUsage := model.Usage{InputTokens: 21, OutputTokens: 8, ReasoningTokens: 2, CacheReadTokens: 5, CacheWriteTokens: 3}
	if response.Usage != wantUsage {
		t.Fatalf("usage = %#v, want %#v", response.Usage, wantUsage)
	}
	got := <-captured
	if got.path != "/v1/messages" || got.apiKey != "test-key" || got.authorization != "" {
		t.Fatalf("request headers/path = %#v", got)
	}
	assertRequestBody(t, got.body)
}

func TestProviderSupportsBearerAuthentication(t *testing.T) {
	t.Parallel()
	authorization := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		authorization <- request.Header.Get("Authorization")
		writer.Header().Set("Content-Type", "text/event-stream")
		writeEvents(writer, terminalEvents()...)
	}))
	defer server.Close()
	provider, err := New(Config{AuthToken: "oauth-token", BaseURL: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := provider.Stream(context.Background(), validRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = model.Collect(stream, nil); err != nil {
		t.Fatal(err)
	}
	_ = stream.Close()
	if got := <-authorization; got != "Bearer oauth-token" {
		t.Fatalf("authorization = %q", got)
	}
}

func TestProviderAndConfigValidation(t *testing.T) {
	t.Parallel()
	invalid := []Config{
		{APIKey: " key"}, {AuthToken: " token"}, {BaseURL: "https://example.com "},
		{BaseURL: "example.com"}, {BaseURL: "file:///tmp/api"},
		{APIKey: "key", AuthToken: "token"}, {MaxTokens: -1},
	}
	for _, config := range invalid {
		if _, err := New(config); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("New(%#v) error = %v", config, err)
		}
	}
	provider, err := New(Config{})
	if err != nil || provider.maxTokens != defaultMaxTokens {
		t.Fatalf("New(defaults) = %#v, %v", provider, err)
	}
	var missing *Provider
	if _, err = missing.Stream(context.Background(), validRequest(t)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil provider error = %v", err)
	}
	if _, err = provider.Stream(context.Background(), model.Request{}); !errors.Is(err, model.ErrInvalidRequest) {
		t.Fatalf("invalid request error = %v", err)
	}
	request := validRequest(t)
	temperature := 1.5
	request.Temperature = &temperature
	if _, err = provider.Stream(context.Background(), request); !errors.Is(err, model.ErrInvalidRequest) {
		t.Fatalf("temperature error = %v", err)
	}
}

func TestMessageAndToolConversionFailures(t *testing.T) {
	t.Parallel()
	call := domain.ToolCall{ID: "call", Name: "tool", Arguments: json.RawMessage(`{}`)}
	result := domain.ToolResult{CallID: "call", Output: json.RawMessage(`{}`)}
	tests := []domain.Message{
		message(t, domain.RoleSystem, domain.Content{Kind: domain.ContentImage, URL: "https://example.test/a.png", MediaType: "image/png"}),
		message(t, domain.RoleUser, domain.Content{Kind: domain.ContentToolCall, ToolCall: &call}),
		message(t, domain.RoleAssistant, domain.Content{Kind: domain.ContentImage, URL: "https://example.test/a.png", MediaType: "image/png"}),
		{Role: domain.RoleAssistant, Content: []domain.Content{{Kind: domain.ContentToolCall}}},
		message(t, domain.RoleTool, domain.Content{Kind: domain.ContentToolResult, ToolResult: &result}, domain.Content{Kind: domain.ContentToolResult, ToolResult: &result}),
		message(t, domain.RoleTool, domain.Content{Kind: domain.ContentText, Text: "x"}),
		{Role: "developer", Content: []domain.Content{{Kind: domain.ContentText, Text: "x"}}},
	}
	for index, message := range tests {
		request := validRequest(t)
		request.Messages = []domain.Message{message}
		if _, _, err := convertMessages(request); err == nil {
			t.Fatalf("case %d conversion error = nil", index)
		}
	}
	badCall := message(t, domain.RoleAssistant, domain.Content{Kind: domain.ContentToolCall, ToolCall: &domain.ToolCall{
		ID: "call", Name: "tool", Arguments: json.RawMessage(`{`),
	}})
	if _, err := convertMessage(badCall); err == nil {
		t.Fatal("invalid historical call error = nil")
	}
	if _, err := convertImage(domain.Content{MediaType: "image/png", URL: "data:image/jpeg;base64,YQ=="}); err == nil {
		t.Fatal("mismatched image data URL error = nil")
	}
	if _, err := convertImage(domain.Content{MediaType: "image/png", URL: "data:image/png;base64,%%%"}); err == nil {
		t.Fatal("invalid image data URL error = nil")
	}
	if _, err := convertImage(domain.Content{MediaType: "image/svg+xml", URL: "https://example.test/a.svg"}); err == nil {
		t.Fatal("unsupported image media type error = nil")
	}
	if _, err := convertTools([]model.ToolDefinition{{Name: "x", Description: "x", InputSchema: json.RawMessage(`{`)}}); err == nil {
		t.Fatal("invalid tool schema error = nil")
	}
	if _, err := convertTools([]model.ToolDefinition{{Name: "x", Description: "x", InputSchema: json.RawMessage(`{"type":"array"}`)}}); err == nil {
		t.Fatal("non-object tool schema error = nil")
	}
}

func TestParameterDefaultsAndToolResultEncoding(t *testing.T) {
	t.Parallel()
	request := validRequest(t)
	params, err := buildParams(request, 2048)
	if err != nil || params.MaxTokens != 2048 {
		t.Fatalf("buildParams() = %#v, %v", params, err)
	}
	result := domain.ToolResult{CallID: "call", Output: json.RawMessage(`"plain"`), IsError: true}
	converted, err := convertToolMessage([]domain.Content{{Kind: domain.ContentToolResult, ToolResult: &result}})
	if err != nil || converted.Role != anthropic.MessageParamRoleUser || toolMessageContent(result.Output) != "plain" ||
		toolMessageContent(json.RawMessage(`{"ok":true}`)) != `{"ok":true}` {
		t.Fatalf("tool conversion = %#v, %v", converted, err)
	}
}

func TestStreamNormalizesStopReasonsAndBounds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input anthropic.StopReason
		want  model.StopReason
		err   bool
	}{
		{anthropic.StopReasonEndTurn, model.StopEndTurn, false},
		{anthropic.StopReasonStopSequence, model.StopEndTurn, false},
		{anthropic.StopReasonToolUse, model.StopToolUse, false},
		{anthropic.StopReasonMaxTokens, model.StopMaxTokens, false},
		{anthropic.StopReasonModelContextWindowExceeded, model.StopMaxTokens, false},
		{anthropic.StopReasonRefusal, model.StopContentFilter, false},
		{anthropic.StopReasonPauseTurn, "", true},
		{anthropic.StopReason("future"), "", true},
	}
	for _, test := range tests {
		got, err := normalizeStop(test.input)
		if got != test.want || (err != nil) != test.err {
			t.Fatalf("normalizeStop(%q) = %q, %v", test.input, got, err)
		}
	}
	if !invalidCallStopPair(true, model.StopEndTurn) || invalidCallStopPair(true, model.StopMaxTokens) ||
		invalidCallStopPair(true, model.StopToolUse) || !invalidCallStopPair(false, model.StopToolUse) {
		t.Fatal("invalidCallStopPair() parity mismatch")
	}
	if boundedInt(-1) != 0 || boundedInt(math.MaxInt64) != math.MaxInt || boundedInt(7) != 7 {
		t.Fatal("boundedInt() did not clamp")
	}
}

func TestStreamHandlesReasoningAndInitialToolInput(t *testing.T) {
	t.Parallel()
	stream := &stream{blocks: make(map[int64]*pendingBlock), calls: make(map[int64]*pendingCall)}
	if err := stream.ingestMessageStart(anthropic.Message{Usage: anthropic.Usage{InputTokens: 4}}); err != nil {
		t.Fatal(err)
	}
	if err := stream.ingestBlockStart(0, anthropic.ContentBlockStartEventContentBlockUnion{Type: "thinking"}); err != nil {
		t.Fatal(err)
	}
	for _, deltaType := range []string{"thinking_delta", "signature_delta"} {
		if err := stream.ingestBlockDelta(0, anthropic.MessageStreamEventUnionDelta{Type: deltaType}); err != nil {
			t.Fatal(err)
		}
	}
	if err := stream.ingestBlockStop(0); err != nil {
		t.Fatal(err)
	}
	if err := stream.ingestBlockStart(1, anthropic.ContentBlockStartEventContentBlockUnion{Type: "redacted_thinking"}); err != nil {
		t.Fatal(err)
	}
	if err := stream.ingestBlockStop(1); err != nil {
		t.Fatal(err)
	}
	if err := stream.ingestBlockStart(2, anthropic.ContentBlockStartEventContentBlockUnion{
		Type: "tool_use", ID: "call", Name: "lookup", Input: map[string]any{"id": float64(1)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := stream.ingestBlockStop(2); err != nil {
		t.Fatal(err)
	}
	usage := anthropic.MessageDeltaUsage{InputTokens: 6, OutputTokens: 3, CacheReadInputTokens: 2, CacheCreationInputTokens: 1}
	if err := stream.ingestMessageDelta(anthropic.StopReasonToolUse, usage); err != nil {
		t.Fatal(err)
	}
	if len(stream.queue) != 2 || stream.queue[0].ToolCall == nil || string(stream.queue[0].ToolCall.Arguments) != `{"id":1}` ||
		stream.queue[1].Usage == nil || stream.queue[1].Usage.InputTokens != 6 {
		t.Fatalf("queue = %#v", stream.queue)
	}
	if err := stream.ingestMessageStop(); err != nil {
		t.Fatal(err)
	}
}

func TestStreamPreservesGuardedStopToolIntent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		reason anthropic.StopReason
		want   model.StopReason
	}{
		{anthropic.StopReasonMaxTokens, model.StopMaxTokens},
		{anthropic.StopReasonRefusal, model.StopContentFilter},
	}
	for _, test := range tests {
		stream := &stream{
			started: true, blocks: make(map[int64]*pendingBlock),
			calls: map[int64]*pendingCall{0: {id: "call", name: "write_file", arguments: json.RawMessage(`{"path":"partial"}`)}},
		}
		if err := stream.ingestMessageDelta(test.reason, anthropic.MessageDeltaUsage{}); err != nil {
			t.Fatalf("reason %q: %v", test.reason, err)
		}
		if stream.stopReason != test.want || len(stream.queue) != 2 || stream.queue[0].ToolCall == nil {
			t.Fatalf("reason %q stream = %#v", test.reason, stream)
		}
	}
}

func TestStreamRejectsProtocolViolations(t *testing.T) {
	t.Parallel()
	tests := []func(*stream) error{
		func(stream *stream) error { return stream.ingestMessageStart(anthropic.Message{}) },
		func(stream *stream) error {
			return stream.ingestBlockStart(0, anthropic.ContentBlockStartEventContentBlockUnion{Type: "server_tool_use"})
		},
		func(stream *stream) error {
			stream.blocks[0] = &pendingBlock{kind: "text"}
			return stream.ingestBlockStart(0, anthropic.ContentBlockStartEventContentBlockUnion{Type: "text"})
		},
		func(stream *stream) error {
			return stream.ingestBlockDelta(2, anthropic.MessageStreamEventUnionDelta{Type: "text_delta", Text: "x"})
		},
		func(stream *stream) error {
			stream.blocks[0] = &pendingBlock{kind: "text"}
			return stream.ingestBlockDelta(0, anthropic.MessageStreamEventUnionDelta{Type: "input_json_delta", PartialJSON: `{}`})
		},
		func(stream *stream) error {
			stream.blocks[0] = &pendingBlock{kind: "tool_use"}
			return stream.ingestBlockDelta(0, anthropic.MessageStreamEventUnionDelta{Type: "text_delta", Text: "x"})
		},
		func(stream *stream) error {
			stream.blocks[0] = &pendingBlock{kind: "text"}
			return stream.ingestBlockDelta(0, anthropic.MessageStreamEventUnionDelta{Type: "future"})
		},
		func(stream *stream) error { return stream.ingestBlockStop(4) },
		func(stream *stream) error {
			stream.deltaSeen = true
			return stream.ingestMessageDelta(anthropic.StopReasonEndTurn, anthropic.MessageDeltaUsage{})
		},
		func(stream *stream) error {
			stream.blocks[0] = &pendingBlock{kind: "text"}
			return stream.ingestMessageDelta(anthropic.StopReasonEndTurn, anthropic.MessageDeltaUsage{})
		},
		func(stream *stream) error {
			stream.calls[0] = &pendingCall{id: "call", name: "x", arguments: json.RawMessage(`{}`)}
			return stream.ingestMessageDelta(anthropic.StopReasonEndTurn, anthropic.MessageDeltaUsage{})
		},
		func(stream *stream) error { return stream.ingestMessageStop() },
	}
	for index, run := range tests {
		stream := &stream{started: true, blocks: make(map[int64]*pendingBlock), calls: make(map[int64]*pendingCall)}
		if index == 0 {
			stream.started = true
		}
		if err := run(stream); !errors.Is(err, ErrProtocol) {
			t.Fatalf("case %d error = %v", index, err)
		}
	}
}

func TestStreamReceiveAndCloseFailures(t *testing.T) {
	t.Parallel()
	source := &fakeEventSource{err: errors.New("network"), closeErr: errors.New("close")}
	active := &stream{source: source, blocks: make(map[int64]*pendingBlock), calls: make(map[int64]*pendingCall)}
	if _, err := active.Recv(); err == nil || !strings.Contains(err.Error(), "network") {
		t.Fatalf("Recv() error = %v", err)
	}
	if err := active.Close(); err == nil || source.closeCalls != 1 {
		t.Fatalf("Close() = %v, calls=%d", err, source.closeCalls)
	}
	if err := active.Close(); err != nil || source.closeCalls != 1 {
		t.Fatalf("second Close() = %v, calls=%d", err, source.closeCalls)
	}
	if _, err := active.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("Recv(closed) error = %v", err)
	}

	unfinished := &stream{source: &fakeEventSource{}, blocks: make(map[int64]*pendingBlock), calls: make(map[int64]*pendingCall)}
	if _, err := unfinished.Recv(); !errors.Is(err, ErrProtocol) {
		t.Fatalf("unfinished Recv() error = %v", err)
	}
}

func assertRequestBody(t *testing.T, body []byte) {
	t.Helper()
	var payload struct {
		Model       string           `json:"model"`
		MaxTokens   int              `json:"max_tokens"`
		Temperature float64          `json:"temperature"`
		Stream      bool             `json:"stream"`
		System      []map[string]any `json:"system"`
		Messages    []struct {
			Role    string           `json:"role"`
			Content []map[string]any `json:"content"`
		} `json:"messages"`
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode request: %v\n%s", err, body)
	}
	if payload.Model != "claude-test" || payload.MaxTokens != 512 || payload.Temperature != 0.25 || !payload.Stream {
		t.Fatalf("request controls = %#v", payload)
	}
	if len(payload.System) != 2 || len(payload.Messages) != 4 || len(payload.Tools) != 1 {
		t.Fatalf("request shape = %#v", payload)
	}
	if payload.Messages[0].Role != "user" || payload.Messages[1].Role != "assistant" || payload.Messages[2].Role != "user" {
		t.Fatalf("message roles = %#v", payload.Messages)
	}
	if payload.Messages[0].Content[1]["type"] != "image" || payload.Messages[3].Content[0]["type"] != "image" {
		t.Fatalf("image blocks = %#v", payload.Messages)
	}
}

func completeRequest(t *testing.T) model.Request {
	t.Helper()
	call := domain.ToolCall{ID: "previous-call", Name: "lookup", Arguments: json.RawMessage(`{"id":1}`)}
	result := domain.ToolResult{CallID: call.ID, Output: json.RawMessage(`{"value":"found"}`)}
	temperature := 0.25
	return model.Request{
		Model: "claude-test", System: "Be precise.", MaxTokens: 512, Temperature: &temperature,
		Messages: []domain.Message{
			message(t, domain.RoleSystem, domain.Content{Kind: domain.ContentText, Text: "Follow policy."}),
			message(t, domain.RoleUser,
				domain.Content{Kind: domain.ContentText, Text: "inspect"},
				domain.Content{Kind: domain.ContentImage, URL: "https://example.test/image.png", MediaType: "image/png"},
			),
			message(t, domain.RoleAssistant,
				domain.Content{Kind: domain.ContentText, Text: "I will check."},
				domain.Content{Kind: domain.ContentToolCall, ToolCall: &call},
			),
			message(t, domain.RoleTool, domain.Content{Kind: domain.ContentToolResult, ToolResult: &result}),
			message(t, domain.RoleUser, domain.Content{Kind: domain.ContentImage, URL: "data:image/png;base64,YQ==", MediaType: "image/png"}),
		},
		Tools: []model.ToolDefinition{{
			Name: "lookup", Description: "Look up a value",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"integer"}},"required":["id"]}`),
		}},
	}
}

func validRequest(t *testing.T) model.Request {
	t.Helper()
	return model.Request{Model: "claude-test", Messages: []domain.Message{
		message(t, domain.RoleUser, domain.Content{Kind: domain.ContentText, Text: "hello"}),
	}}
}

func message(t *testing.T, role domain.Role, contents ...domain.Content) domain.Message {
	t.Helper()
	id, err := domain.NewMessageID()
	if err != nil {
		t.Fatal(err)
	}
	return domain.Message{ID: id, Role: role, Content: contents, CreatedAt: time.Now().UTC()}
}

type wireEvent struct{ name, data string }

func anthropicEvent(name, data string) wireEvent { return wireEvent{name: name, data: data} }

func writeEvents(writer io.Writer, events ...wireEvent) {
	for _, event := range events {
		_, _ = io.WriteString(writer, "event: "+event.name+"\ndata: "+event.data+"\n\n")
	}
}

func terminalEvents() []wireEvent {
	return []wireEvent{
		anthropicEvent("message_start", `{"type":"message_start","message":{"id":"msg-1","type":"message","role":"assistant","model":"claude-test","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":2,"output_tokens":1}}}`),
		anthropicEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		anthropicEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}`),
		anthropicEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
		anthropicEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}`),
		anthropicEvent("message_stop", `{"type":"message_stop"}`),
	}
}

type fakeEventSource struct {
	events     []anthropic.MessageStreamEventUnion
	index      int
	err        error
	closeErr   error
	closeCalls int
}

func (source *fakeEventSource) Next() bool {
	if source.index >= len(source.events) {
		return false
	}
	source.index++
	return true
}

func (source *fakeEventSource) Current() anthropic.MessageStreamEventUnion {
	return source.events[source.index-1]
}

func (source *fakeEventSource) Err() error { return source.err }

func (source *fakeEventSource) Close() error {
	source.closeCalls++
	return source.closeErr
}

func TestProviderExampleSurface(t *testing.T) {
	t.Parallel()
	if !strings.Contains(ErrProtocol.Error(), "protocol") {
		t.Fatalf("protocol error = %v", ErrProtocol)
	}
}
