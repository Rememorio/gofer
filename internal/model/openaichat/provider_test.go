package openaichat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/model"
)

func TestProviderStreamsParallelToolCallsAndUsage(t *testing.T) {
	t.Parallel()

	type capturedRequest struct {
		path          string
		authorization string
		body          []byte
	}
	captured := make(chan capturedRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		captured <- capturedRequest{
			path: request.URL.Path, authorization: request.Header.Get("Authorization"), body: body,
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, strings.Join([]string{
			`data: {"id":"chat-1","object":"chat.completion.chunk","created":1,"model":"gpt-test","choices":[{"index":0,"delta":{"role":"assistant","content":"working "},"finish_reason":null}]}`,
			"",
			`data: {"id":"chat-1","object":"chat.completion.chunk","created":1,"model":"gpt-test","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call-2","type":"function","function":{"name":"beta","arguments":"{\"n\":"}},{"index":0,"id":"call-1","type":"function","function":{"name":"alpha","arguments":"{\"q\":\"go\"}"}}]},"finish_reason":null}]}`,
			"",
			`data: {"id":"chat-1","object":"chat.completion.chunk","created":1,"model":"gpt-test","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"2}"}}]},"finish_reason":null}]}`,
			"",
			`data: {"id":"chat-1","object":"chat.completion.chunk","created":1,"model":"gpt-test","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			"",
			`data: {"id":"chat-1","object":"chat.completion.chunk","created":1,"model":"gpt-test","choices":[],"usage":{"prompt_tokens":21,"completion_tokens":8,"total_tokens":29,"prompt_tokens_details":{"cached_tokens":5,"cache_write_tokens":3},"completion_tokens_details":{"reasoning_tokens":2}}}`,
			"",
			"data: [DONE]",
			"",
		}, "\n"))
	}))
	defer server.Close()

	provider, err := New(Config{APIKey: "test-key", BaseURL: server.URL + "/v1", HTTPClient: server.Client()})
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	request := completeRequest(t)
	stream, err := provider.Stream(context.Background(), request)
	if err != nil {
		t.Fatalf("Stream(): %v", err)
	}
	response, err := model.Collect(stream, nil)
	if err != nil {
		t.Fatalf("Collect(): %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	if response.Text != "working " || response.StopReason != model.StopToolUse {
		t.Fatalf("response = %#v", response)
	}
	if len(response.ToolCalls) != 2 || response.ToolCalls[0].ID != "call-1" || response.ToolCalls[1].ID != "call-2" {
		t.Fatalf("tool calls = %#v", response.ToolCalls)
	}
	if string(response.ToolCalls[1].Arguments) != `{"n":2}` {
		t.Fatalf("second arguments = %s", response.ToolCalls[1].Arguments)
	}
	wantUsage := model.Usage{
		InputTokens: 21, OutputTokens: 8, ReasoningTokens: 2, CacheReadTokens: 5, CacheWriteTokens: 3,
	}
	if response.Usage != wantUsage {
		t.Fatalf("usage = %#v, want %#v", response.Usage, wantUsage)
	}

	got := <-captured
	if got.path != "/v1/chat/completions" || got.authorization != "Bearer test-key" {
		t.Fatalf("request path = %q, authorization = %q", got.path, got.authorization)
	}
	assertRequestBody(t, got.body)
}

func TestProviderAndConfigValidation(t *testing.T) {
	t.Parallel()

	invalid := []Config{
		{APIKey: " key"},
		{BaseURL: "https://example.com/v1 "},
		{BaseURL: "example.com/v1"},
		{BaseURL: "file:///tmp/api"},
	}
	for _, config := range invalid {
		if _, err := New(config); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("New(%#v) error = %v, want ErrInvalidConfig", config, err)
		}
	}
	if _, err := New(Config{}); err != nil {
		t.Fatalf("New(defaults): %v", err)
	}

	var provider *Provider
	if _, err := provider.Stream(context.Background(), validRequest(t)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil Stream() error = %v, want ErrInvalidConfig", err)
	}
	configured, err := New(Config{})
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	if _, err := configured.Stream(context.Background(), model.Request{}); !errors.Is(err, model.ErrInvalidRequest) {
		t.Fatalf("Stream(invalid) error = %v, want ErrInvalidRequest", err)
	}
}

func TestMessageConversionFailures(t *testing.T) {
	t.Parallel()

	call := domain.ToolCall{ID: "call", Name: "tool", Arguments: json.RawMessage(`{}`)}
	result := domain.ToolResult{CallID: "call", Output: json.RawMessage(`{}`)}
	tests := []struct {
		name    string
		message domain.Message
	}{
		{name: "system image", message: message(t, domain.RoleSystem, domain.Content{Kind: domain.ContentImage, URL: "https://example.com/a.png", MediaType: "image/png"})},
		{name: "user tool call", message: message(t, domain.RoleUser, domain.Content{Kind: domain.ContentToolCall, ToolCall: &call})},
		{name: "assistant image", message: message(t, domain.RoleAssistant, domain.Content{Kind: domain.ContentImage, URL: "https://example.com/a.png", MediaType: "image/png"})},
		{name: "assistant nil call", message: domain.Message{Role: domain.RoleAssistant, Content: []domain.Content{{Kind: domain.ContentToolCall}}}},
		{name: "tool multiple", message: message(t, domain.RoleTool,
			domain.Content{Kind: domain.ContentToolResult, ToolResult: &result},
			domain.Content{Kind: domain.ContentToolResult, ToolResult: &result},
		)},
		{name: "tool wrong content", message: message(t, domain.RoleTool, domain.Content{Kind: domain.ContentText, Text: "x"})},
		{name: "unsupported role", message: domain.Message{Role: "developer", Content: []domain.Content{{Kind: domain.ContentText, Text: "x"}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := convertMessage(test.message); err == nil {
				t.Fatal("convertMessage() error = nil")
			}
		})
	}

	definition := model.ToolDefinition{Name: "bad", Description: "Bad schema", InputSchema: json.RawMessage(`[]`)}
	if _, err := convertTools([]model.ToolDefinition{definition}); err == nil {
		t.Fatal("convertTools(array schema) error = nil")
	}
}

func TestToolMessageContentDecodesJSONString(t *testing.T) {
	t.Parallel()
	if got := toolMessageContent(json.RawMessage(`"preview\nline"`)); got != "preview\nline" {
		t.Fatalf("toolMessageContent(string) = %q", got)
	}
	if got := toolMessageContent(json.RawMessage(`{"ok":true}`)); got != `{"ok":true}` {
		t.Fatalf("toolMessageContent(object) = %q", got)
	}
}

func TestStreamProtocolFailures(t *testing.T) {
	t.Parallel()

	providerError := errors.New("upstream reset")
	tests := []struct {
		name   string
		chunks []openai.ChatCompletionChunk
		err    error
	}{
		{name: "missing finish"},
		{name: "source error", err: providerError},
		{name: "multiple choices", chunks: []openai.ChatCompletionChunk{{Choices: []openai.ChatCompletionChunkChoice{{Index: 0}, {Index: 1}}}}},
		{name: "wrong choice index", chunks: []openai.ChatCompletionChunk{{Choices: []openai.ChatCompletionChunkChoice{{Index: 2}}}}},
		{name: "unknown finish", chunks: []openai.ChatCompletionChunk{{Choices: []openai.ChatCompletionChunkChoice{{Index: 0, FinishReason: "pause"}}}}},
		{name: "tool finish without calls", chunks: []openai.ChatCompletionChunk{{Choices: []openai.ChatCompletionChunkChoice{{Index: 0, FinishReason: "tool_calls"}}}}},
		{name: "calls with stop finish", chunks: []openai.ChatCompletionChunk{
			toolDelta("one", "echo", "{}"),
			{Choices: []openai.ChatCompletionChunkChoice{{Index: 0, FinishReason: "stop"}}},
		}},
		{name: "duplicate finish", chunks: []openai.ChatCompletionChunk{
			{Choices: []openai.ChatCompletionChunkChoice{{Index: 0, FinishReason: "stop"}}},
			{Choices: []openai.ChatCompletionChunkChoice{{Index: 0, FinishReason: "stop"}}},
		}},
		{name: "changed call ID", chunks: []openai.ChatCompletionChunk{
			toolDelta("one", "echo", "{"), toolDelta("two", "", "}"),
		}},
		{name: "changed call name", chunks: []openai.ChatCompletionChunk{
			toolDelta("one", "echo", "{"), toolDelta("", "other", "}"),
		}},
		{name: "unsupported tool", chunks: []openai.ChatCompletionChunk{{Choices: []openai.ChatCompletionChunkChoice{{
			Index: 0, Delta: openai.ChatCompletionChunkChoiceDelta{ToolCalls: []openai.ChatCompletionChunkChoiceDeltaToolCall{{Index: 0, Type: "custom"}}},
		}}}}},
		{name: "negative tool index", chunks: []openai.ChatCompletionChunk{{Choices: []openai.ChatCompletionChunkChoice{{
			Index: 0, Delta: openai.ChatCompletionChunkChoiceDelta{ToolCalls: []openai.ChatCompletionChunkChoiceDeltaToolCall{{Index: -1, Type: "function"}}},
		}}}}},
		{name: "invalid call", chunks: []openai.ChatCompletionChunk{
			toolDelta("", "echo", "not-json"),
			{Choices: []openai.ChatCompletionChunkChoice{{Index: 0, FinishReason: "tool_calls"}}},
		}},
		{name: "choice after finish", chunks: []openai.ChatCompletionChunk{
			{Choices: []openai.ChatCompletionChunkChoice{{Index: 0, FinishReason: "stop"}}},
			{Choices: []openai.ChatCompletionChunkChoice{{Index: 0, Delta: openai.ChatCompletionChunkChoiceDelta{Content: "late"}}}},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := &fakeSource{chunks: test.chunks, err: test.err}
			stream := &stream{source: source, calls: make(map[int64]*pendingCall)}
			for {
				_, err := stream.Recv()
				if err == nil {
					continue
				}
				if errors.Is(err, io.EOF) {
					t.Fatal("Recv() error = EOF, want protocol or source error")
				}
				if test.err != nil && !errors.Is(err, test.err) {
					t.Fatalf("Recv() error = %v, want %v", err, test.err)
				}
				break
			}
		})
	}
}

func TestStreamCloseAndStopNormalization(t *testing.T) {
	t.Parallel()

	tests := map[string]model.StopReason{
		"stop": model.StopEndTurn, "tool_calls": model.StopToolUse,
		"function_call": model.StopToolUse, "length": model.StopMaxTokens,
		"content_filter": model.StopContentFilter,
	}
	for input, want := range tests {
		got, err := normalizeStop(input)
		if err != nil || got != want {
			t.Fatalf("normalizeStop(%q) = %q, %v, want %q", input, got, err, want)
		}
	}

	source := &fakeSource{closeErr: errors.New("close")}
	stream := &stream{source: source, calls: make(map[int64]*pendingCall)}
	if err := stream.Close(); !errors.Is(err, source.closeErr) {
		t.Fatalf("Close() error = %v, want %v", err, source.closeErr)
	}
	if err := stream.Close(); err != nil || source.closeCalls != 1 {
		t.Fatalf("second Close() = %v, calls = %d", err, source.closeCalls)
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("Recv(closed) error = %v, want EOF", err)
	}
}

func TestStreamPreservesGuardedStopToolIntent(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{"length", "content_filter"} {
		t.Run(reason, func(t *testing.T) {
			stream := &stream{source: &fakeSource{chunks: []openai.ChatCompletionChunk{
				toolDelta("call", "write_file", `{"path":"partial"}`),
				{Choices: []openai.ChatCompletionChunkChoice{{Index: 0, FinishReason: reason}}},
			}}, calls: make(map[int64]*pendingCall)}
			response, err := model.Collect(stream, nil)
			if err != nil || len(response.ToolCalls) != 1 {
				t.Fatalf("Collect() = %#v, %v", response, err)
			}
			want, _ := normalizeStop(reason)
			if response.StopReason != want {
				t.Fatalf("stop reason = %q, want %q", response.StopReason, want)
			}
		})
	}
}

func assertRequestBody(t *testing.T, body []byte) {
	t.Helper()
	var payload struct {
		Model             string           `json:"model"`
		Messages          []map[string]any `json:"messages"`
		Tools             []map[string]any `json:"tools"`
		MaxTokens         int              `json:"max_completion_tokens"`
		Temperature       float64          `json:"temperature"`
		ParallelToolCalls bool             `json:"parallel_tool_calls"`
		StreamOptions     struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode request body: %v\n%s", err, body)
	}
	if payload.Model != "gpt-test" || payload.MaxTokens != 512 || payload.Temperature != 0.25 {
		t.Fatalf("request controls = %#v", payload)
	}
	if !payload.ParallelToolCalls || !payload.StreamOptions.IncludeUsage {
		t.Fatalf("stream controls = %#v", payload)
	}
	if len(payload.Messages) != 5 || len(payload.Tools) != 1 {
		t.Fatalf("request messages/tools = %d/%d\n%s", len(payload.Messages), len(payload.Tools), body)
	}
	if payload.Messages[0]["role"] != "system" || payload.Messages[2]["role"] != "assistant" || payload.Messages[3]["role"] != "tool" {
		t.Fatalf("message roles = %#v", payload.Messages)
	}
}

func completeRequest(t *testing.T) model.Request {
	t.Helper()
	call := domain.ToolCall{ID: "previous-call", Name: "lookup", Arguments: json.RawMessage(`{"id":1}`)}
	result := domain.ToolResult{CallID: call.ID, Output: json.RawMessage(`{"value":"found"}`)}
	temperature := 0.25
	return model.Request{
		Model: "gpt-test", System: "Be precise.", MaxTokens: 512, Temperature: &temperature,
		Messages: []domain.Message{
			message(t, domain.RoleUser,
				domain.Content{Kind: domain.ContentText, Text: "inspect "},
				domain.Content{Kind: domain.ContentImage, URL: "https://example.com/image.png", MediaType: "image/png"},
			),
			message(t, domain.RoleAssistant,
				domain.Content{Kind: domain.ContentText, Text: "I will check."},
				domain.Content{Kind: domain.ContentToolCall, ToolCall: &call},
			),
			message(t, domain.RoleTool, domain.Content{Kind: domain.ContentToolResult, ToolResult: &result}),
			message(t, domain.RoleUser, domain.Content{Kind: domain.ContentText, Text: "continue"}),
		},
		Tools: []model.ToolDefinition{{
			Name: "lookup", Description: "Look up a value",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"integer"}},"required":["id"]}`),
		}},
	}
}

func validRequest(t *testing.T) model.Request {
	t.Helper()
	return model.Request{Model: "gpt-test", Messages: []domain.Message{
		message(t, domain.RoleUser, domain.Content{Kind: domain.ContentText, Text: "hello"}),
	}}
}

func message(t *testing.T, role domain.Role, contents ...domain.Content) domain.Message {
	t.Helper()
	id, err := domain.NewMessageID()
	if err != nil {
		t.Fatalf("NewMessageID(): %v", err)
	}
	return domain.Message{ID: id, Role: role, Content: contents, CreatedAt: time.Now().UTC()}
}

func toolDelta(id, name, arguments string) openai.ChatCompletionChunk {
	return openai.ChatCompletionChunk{Choices: []openai.ChatCompletionChunkChoice{{
		Index: 0,
		Delta: openai.ChatCompletionChunkChoiceDelta{ToolCalls: []openai.ChatCompletionChunkChoiceDeltaToolCall{{
			Index: 0, ID: id, Type: "function",
			Function: openai.ChatCompletionChunkChoiceDeltaToolCallFunction{Name: name, Arguments: arguments},
		}}},
	}}}
}

type fakeSource struct {
	chunks     []openai.ChatCompletionChunk
	index      int
	err        error
	closeErr   error
	closeCalls int
}

func (source *fakeSource) Next() bool {
	if source.index >= len(source.chunks) {
		return false
	}
	source.index++
	return true
}

func (source *fakeSource) Current() openai.ChatCompletionChunk {
	return source.chunks[source.index-1]
}

func (source *fakeSource) Err() error { return source.err }

func (source *fakeSource) Close() error {
	source.closeCalls++
	return source.closeErr
}

func ExampleProvider() {
	provider, err := New(Config{})
	if err != nil {
		panic(err)
	}
	fmt.Printf("%T\n", provider)
	// Output: *openaichat.Provider
}
