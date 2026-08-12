package loopdetect

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/model"
)

func TestIdenticalCallWarningAndHardStop(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 13, 1, 2, 3, 0, time.UTC)
	middleware := mustDetector(t, Config{
		WarnThreshold: 2, HardLimit: 4, WindowSize: 8,
		ToolFrequencyWarn: 20, ToolFrequencyLimit: 30, Now: func() time.Time { return now },
	})
	response := toolResponse(toolCall("one", "read_file", `{"path":"/tmp/a","start_line":1}`))
	first := transformOK(t, middleware, response)
	if len(first.ToolCalls) != 1 {
		t.Fatalf("first response = %#v", first)
	}
	second := transformOK(t, middleware, response)
	if len(second.ToolCalls) != 1 {
		t.Fatalf("warning stripped calls: %#v", second)
	}

	original, _ := domain.NewTextMessage(domain.RoleUser, "work", now)
	request := model.Request{Messages: []domain.Message{original}}
	if err := middleware.BeforeModel(context.Background(), &request); err != nil {
		t.Fatal(err)
	}
	if len(request.Messages) != 2 || request.Messages[1].Role != domain.RoleUser ||
		request.Messages[1].Metadata["internal_kind"] != "loop_warning" || request.Messages[1].Content[0].Text != identicalWarning {
		t.Fatalf("warning request = %#v", request.Messages)
	}
	if request.Messages[0].Content[0].Text != "work" {
		t.Fatalf("original message changed: %#v", request.Messages[0])
	}
	if err := middleware.BeforeModel(context.Background(), &request); err != nil || len(request.Messages) != 2 {
		t.Fatalf("warning was not drained: len=%d err=%v", len(request.Messages), err)
	}

	third := transformOK(t, middleware, response)
	if len(third.ToolCalls) != 1 {
		t.Fatalf("third response = %#v", third)
	}
	fourth := transformOK(t, middleware, response)
	if len(fourth.ToolCalls) != 0 || fourth.StopReason != model.StopLoopCapped || fourth.Text != identicalHardStop || fourth.Usage != response.Usage {
		t.Fatalf("hard stop = %#v", fourth)
	}
	if len(response.ToolCalls) != 1 || response.StopReason != model.StopToolUse {
		t.Fatalf("input response mutated: %#v", response)
	}
}

func TestHashWarningDecaysWithSlidingWindow(t *testing.T) {
	t.Parallel()
	middleware := mustDetector(t, Config{
		WarnThreshold: 2, HardLimit: 3, WindowSize: 3,
		ToolFrequencyWarn: 20, ToolFrequencyLimit: 30,
	})
	callA := toolResponse(toolCall("a", "search", `{"query":"a"}`))
	transformOK(t, middleware, callA)
	transformOK(t, middleware, callA)
	drainWarnings(t, middleware)
	for _, query := range []string{"b", "c", "d"} {
		transformOK(t, middleware, toolResponse(toolCall(query, "search", `{"query":"`+query+`"}`)))
	}
	transformOK(t, middleware, callA)
	if warnings := pendingWarnings(middleware); len(warnings) != 0 {
		t.Fatalf("first decayed call warned: %#v", warnings)
	}
	transformOK(t, middleware, callA)
	if warnings := pendingWarnings(middleware); !reflect.DeepEqual(warnings, []string{identicalWarning}) {
		t.Fatalf("renewed warnings = %#v", warnings)
	}
}

func TestToolFrequencyWarningHardStopAndOverride(t *testing.T) {
	t.Parallel()
	config := Config{
		WarnThreshold: 10, HardLimit: 10, WindowSize: 10,
		ToolFrequencyWarn: 2, ToolFrequencyLimit: 3,
		ToolOverrides: map[string]FrequencyOverride{"bash": {Warn: 3, HardLimit: 4}},
	}
	middleware := mustDetector(t, config)
	transformOK(t, middleware, toolResponse(toolCall("r1", "read_file", `{"path":"/a"}`)))
	transformOK(t, middleware, toolResponse(toolCall("r2", "read_file", `{"path":"/b"}`)))
	warnings := pendingWarnings(middleware)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "read_file 2 times") {
		t.Fatalf("frequency warning = %#v", warnings)
	}
	drainWarnings(t, middleware)
	hard := transformOK(t, middleware, toolResponse(toolCall("r3", "read_file", `{"path":"/c"}`)))
	if hard.StopReason != model.StopLoopCapped || !strings.Contains(hard.Text, "read_file was called 3 times") {
		t.Fatalf("frequency hard stop = %#v", hard)
	}

	overridden := mustDetector(t, config)
	for index := 1; index <= 2; index++ {
		transformOK(t, overridden, toolResponse(toolCall("b", "bash", `{"command":"echo `+string(rune('0'+index))+`"}`)))
	}
	if warnings = pendingWarnings(overridden); len(warnings) != 0 {
		t.Fatalf("override warned early: %#v", warnings)
	}
	transformOK(t, overridden, toolResponse(toolCall("b3", "bash", `{"command":"echo 3"}`)))
	if warnings = pendingWarnings(overridden); len(warnings) != 1 || !strings.Contains(warnings[0], "bash 3 times") {
		t.Fatalf("override warning = %#v", warnings)
	}
	drainWarnings(t, overridden)
	hard = transformOK(t, overridden, toolResponse(toolCall("b4", "bash", `{"command":"echo 4"}`)))
	if hard.StopReason != model.StopLoopCapped || !strings.Contains(hard.Text, "bash was called 4 times") {
		t.Fatalf("override hard stop = %#v", hard)
	}
}

func TestFrequencyCountDecays(t *testing.T) {
	t.Parallel()
	middleware := mustDetector(t, Config{
		WarnThreshold: 3, HardLimit: 3, WindowSize: 3,
		ToolFrequencyWarn: 2, ToolFrequencyLimit: 3,
	})
	transformOK(t, middleware, toolResponse(toolCall("a1", "alpha", `{}`)))
	transformOK(t, middleware, toolResponse(toolCall("a2", "alpha", `{"n":2}`)))
	drainWarnings(t, middleware)
	for index := range 2 {
		transformOK(t, middleware, toolResponse(toolCall("b", "beta", json.RawMessage(`{"n":`+string(rune('1'+index))+`}`))))
	}
	drainWarnings(t, middleware)
	transformOK(t, middleware, toolResponse(toolCall("a3", "alpha", `{"n":3}`)))
	if warnings := pendingWarnings(middleware); len(warnings) != 0 {
		t.Fatalf("decayed frequency warned: %#v", warnings)
	}
}

func TestCallSetSignatureNormalization(t *testing.T) {
	t.Parallel()
	first := []domain.ToolCall{
		toolCall("1", "search", `{"query":"go","noise":1}`),
		toolCall("2", "read_file", `{"path":"/a","start_line":1,"end_line":99}`),
	}
	permuted := []domain.ToolCall{
		toolCall("x", "read_file", `{"end_line":150,"start_line":2,"path":"/a"}`),
		toolCall("y", "search", `{"noise":999,"query":"go"}`),
	}
	if callSetSignature(first) != callSetSignature(permuted) {
		t.Fatal("semantically stable call sets produced different signatures")
	}
	otherBucket := append([]domain.ToolCall(nil), permuted...)
	otherBucket[0] = toolCall("x", "read_file", `{"path":"/a","start_line":201,"end_line":250}`)
	if callSetSignature(first) == callSetSignature(otherBucket) {
		t.Fatal("different read bucket produced the same signature")
	}
	duplicate := append(append([]domain.ToolCall(nil), first...), first[0])
	if callSetSignature(first) == callSetSignature(duplicate) {
		t.Fatal("call multiplicity was ignored")
	}
	writeOne := toolCall("1", "write_file", `{"path":"/a","content":"one"}`)
	writeTwo := toolCall("2", "write_file", `{"content":"two","path":"/a"}`)
	if stableToolKey(writeOne) == stableToolKey(writeTwo) {
		t.Fatal("write content was ignored")
	}
	if key := stableToolKey(toolCall("bad", "search", `{`)); key != "{" {
		t.Fatalf("invalid fallback key = %q", key)
	}
}

func TestPendingWarningsAreDeduplicatedAndBounded(t *testing.T) {
	t.Parallel()
	middleware := mustDetector(t, DefaultConfig())
	middleware.mu.Lock()
	for _, warning := range []string{"one", "one", "two", "three", "four", "five"} {
		middleware.queueWarning(warning)
	}
	middleware.mu.Unlock()
	if got := pendingWarnings(middleware); !reflect.DeepEqual(got, []string{"two", "three", "four", "five"}) {
		t.Fatalf("pending warnings = %#v", got)
	}
	middleware.Reset()
	if len(middleware.history) != 0 || len(middleware.pending) != 0 || len(middleware.warnedHashes) != 0 || len(middleware.toolCounts) != 0 {
		t.Fatalf("Reset() left state: %#v", middleware)
	}
	(*Middleware)(nil).Reset()
}

func TestMiddlewareValidationAndCancellation(t *testing.T) {
	t.Parallel()
	tests := []Config{
		{},
		{WarnThreshold: 2, HardLimit: 1, WindowSize: 2, ToolFrequencyWarn: 1, ToolFrequencyLimit: 1},
		{WarnThreshold: 1, HardLimit: 2, WindowSize: 1, ToolFrequencyWarn: 1, ToolFrequencyLimit: 1},
		{WarnThreshold: 1, HardLimit: 1, WindowSize: 10_001, ToolFrequencyWarn: 1, ToolFrequencyLimit: 1},
		{WarnThreshold: 1, HardLimit: 1, WindowSize: 1, ToolFrequencyWarn: 2, ToolFrequencyLimit: 1},
		{WarnThreshold: 1, HardLimit: 1, WindowSize: 1, ToolFrequencyWarn: 1, ToolFrequencyLimit: 100_001},
		{WarnThreshold: 1, HardLimit: 1, WindowSize: 1, ToolFrequencyWarn: 1, ToolFrequencyLimit: 1, ToolOverrides: map[string]FrequencyOverride{" bad": {Warn: 1, HardLimit: 1}}},
		{WarnThreshold: 1, HardLimit: 1, WindowSize: 1, ToolFrequencyWarn: 1, ToolFrequencyLimit: 1, ToolOverrides: map[string]FrequencyOverride{"bash": {Warn: 2, HardLimit: 1}}},
	}
	for _, config := range tests {
		if _, err := New(config); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("New(%#v) error = %v", config, err)
		}
	}
	config := DefaultConfig()
	config.ToolOverrides["bash"] = FrequencyOverride{Warn: 2, HardLimit: 3}
	middleware := mustDetector(t, config)
	config.ToolOverrides["bash"] = FrequencyOverride{Warn: 1, HardLimit: 1}
	if middleware.config.ToolOverrides["bash"].Warn != 2 {
		t.Fatal("New retained a mutable override alias")
	}
	if err := (*Middleware)(nil).BeforeModel(context.Background(), &model.Request{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil BeforeModel error = %v", err)
	}
	if err := middleware.BeforeModel(context.Background(), nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil request error = %v", err)
	}
	if _, err := (*Middleware)(nil).TransformModelResponse(context.Background(), model.Response{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil transform error = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := middleware.BeforeModel(cancelled, &model.Request{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("BeforeModel(cancelled) error = %v", err)
	}
	if _, err := middleware.TransformModelResponse(cancelled, model.Response{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Transform(cancelled) error = %v", err)
	}
	clean := model.Response{Text: "done", StopReason: model.StopEndTurn}
	if got := transformOK(t, middleware, clean); !reflect.DeepEqual(got, clean) {
		t.Fatalf("clean response changed: %#v", got)
	}
}

func mustDetector(t *testing.T, config Config) *Middleware {
	t.Helper()
	middleware, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return middleware
}

func toolCall(id, name string, arguments any) domain.ToolCall {
	var raw json.RawMessage
	switch typed := arguments.(type) {
	case string:
		raw = json.RawMessage(typed)
	case json.RawMessage:
		raw = typed
	}
	return domain.ToolCall{ID: id, Name: name, Arguments: raw}
}

func toolResponse(call domain.ToolCall) model.Response {
	return model.Response{
		ToolCalls: []domain.ToolCall{call}, Usage: model.Usage{InputTokens: 2, OutputTokens: 1},
		StopReason: model.StopToolUse,
	}
}

func transformOK(t *testing.T, middleware *Middleware, response model.Response) model.Response {
	t.Helper()
	transformed, err := middleware.TransformModelResponse(context.Background(), response)
	if err != nil {
		t.Fatal(err)
	}
	return transformed
}

func pendingWarnings(middleware *Middleware) []string {
	middleware.mu.Lock()
	defer middleware.mu.Unlock()
	return append([]string(nil), middleware.pending...)
}

func drainWarnings(t *testing.T, middleware *Middleware) {
	t.Helper()
	if err := middleware.BeforeModel(context.Background(), &model.Request{}); err != nil {
		t.Fatal(err)
	}
}
