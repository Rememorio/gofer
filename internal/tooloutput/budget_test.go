package tooloutput

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/model"
	"github.com/Rememorio/gofer/internal/workspace"
)

func TestMiddlewareExternalizesCompleteResult(t *testing.T) {
	t.Parallel()
	thread := newTestWorkspace(t, 1<<20)
	config := testConfig()
	config.ExternalizeMinChars = 40
	config.PreviewHeadChars, config.PreviewTailChars = 20, 10
	middleware, err := New(config, thread)
	if err != nil {
		t.Fatal(err)
	}
	content := " \n" + `{"meta":{"source":"unit"},"items":[1,2,3],"payload":"` + strings.Repeat("x", 120) + `"}` + "\n "
	call := domain.ToolCall{ID: "call", Name: "../../api/tool", Arguments: json.RawMessage(`{}`)}
	result := domain.ToolResult{CallID: call.ID, Output: json.RawMessage(content), IsError: true}
	transformed, err := middleware.TransformToolResult(context.Background(), call, result)
	if err != nil {
		t.Fatal(err)
	}
	var preview string
	if json.Unmarshal(transformed.Output, &preview) != nil || !strings.Contains(preview, "Preview kind: json") ||
		!strings.Contains(preview, "Use read_file on ") || !transformed.IsError {
		t.Fatalf("transformed = %#v, preview = %q", transformed, preview)
	}
	virtualPath := between(preview, "saved to ", " (")
	if !strings.HasPrefix(virtualPath, workspace.OutputsRoot+"/"+DefaultStorageSubdir+"/apitool-") || strings.Contains(virtualPath, "..") {
		t.Fatalf("externalized path = %q", virtualPath)
	}
	reader, err := thread.OpenFile(virtualPath)
	if err != nil {
		t.Fatal(err)
	}
	stored, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || string(stored) != content {
		t.Fatalf("stored = %q, read=%v close=%v", stored, readErr, closeErr)
	}
}

func TestMiddlewareFallbackAndPassThroughRules(t *testing.T) {
	t.Parallel()
	thread := newTestWorkspace(t, 16)
	config := testConfig()
	config.ExternalizeMinChars = 10
	config.FallbackMaxChars = 220
	config.FallbackHeadChars, config.FallbackTailChars = 60, 24
	config.ToolOverrides = map[string]int{"quiet": 0}
	middleware, err := New(config, thread)
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("你好\n", 100)
	result := domain.ToolResult{CallID: "call", Output: marshalText(long)}
	transformed, err := middleware.TransformToolResult(context.Background(), domain.ToolCall{ID: "call", Name: "bash"}, result)
	if err != nil {
		t.Fatal(err)
	}
	var fallback string
	if json.Unmarshal(transformed.Output, &fallback) != nil || utf8.RuneCountInString(fallback) > 220 ||
		!strings.Contains(fallback, "Persistent storage unavailable") {
		t.Fatalf("fallback = %q (%d chars)", fallback, utf8.RuneCountInString(fallback))
	}
	for _, name := range []string{"read_file", "quiet"} {
		candidate := result
		if name == "quiet" {
			candidate.Output = marshalText(strings.Repeat("q", 40))
		}
		got, transformErr := middleware.TransformToolResult(context.Background(), domain.ToolCall{ID: "call", Name: name}, candidate)
		if transformErr != nil || string(got.Output) != string(candidate.Output) {
			t.Fatalf("TransformToolResult(%s) = %s, %v", name, got.Output, transformErr)
		}
	}
	exact := domain.ToolResult{CallID: "call", Output: marshalText(strings.Repeat("x", 10))}
	got, err := middleware.TransformToolResult(context.Background(), domain.ToolCall{ID: "call", Name: "tool"}, exact)
	if err != nil || string(got.Output) != string(exact.Output) {
		t.Fatalf("threshold result = %s, %v", got.Output, err)
	}
}

func TestMiddlewareBoundsHistoricalResultsWithoutMutatingSource(t *testing.T) {
	t.Parallel()
	thread := newTestWorkspace(t, 1<<20)
	config := testConfig()
	config.FallbackMaxChars = 220
	config.FallbackHeadChars, config.FallbackTailChars = 60, 20
	middleware, err := New(config, thread)
	if err != nil {
		t.Fatal(err)
	}
	call := domain.ToolCall{ID: "one", Name: "search", Arguments: json.RawMessage(`{}`)}
	readCall := domain.ToolCall{ID: "two", Name: "read_file", Arguments: json.RawMessage(`{}`)}
	long := marshalText(strings.Repeat("x", 500))
	readLong := marshalText(strings.Repeat("y", 500))
	original := []domain.Message{
		{Role: domain.RoleAssistant, Content: []domain.Content{{Kind: domain.ContentToolCall, ToolCall: &call}, {Kind: domain.ContentToolCall, ToolCall: &readCall}}},
		{Role: domain.RoleTool, Content: []domain.Content{{Kind: domain.ContentToolResult, ToolResult: &domain.ToolResult{CallID: call.ID, Output: long}}}},
		{Role: domain.RoleTool, Content: []domain.Content{{Kind: domain.ContentToolResult, ToolResult: &domain.ToolResult{CallID: readCall.ID, Output: readLong}}}},
	}
	request := model.Request{Messages: append([]domain.Message(nil), original...)}
	if err = middleware.BeforeModel(context.Background(), &request); err != nil {
		t.Fatal(err)
	}
	var bounded string
	if json.Unmarshal(request.Messages[1].Content[0].ToolResult.Output, &bounded) != nil || utf8.RuneCountInString(bounded) > 220 || !strings.Contains(bounded, "omitted") {
		t.Fatalf("historical result = %q", bounded)
	}
	if string(request.Messages[2].Content[0].ToolResult.Output) != string(readLong) || string(original[1].Content[0].ToolResult.Output) != string(long) {
		t.Fatal("exempt or source history was mutated")
	}
}

func TestMiddlewareRejectsInvalidUse(t *testing.T) {
	t.Parallel()
	thread := newTestWorkspace(t, 1<<20)
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "negative", mutate: func(config *Config) { config.FallbackMaxChars = -1 }},
		{name: "nested directory", mutate: func(config *Config) { config.StorageSubdir = "cache/results" }},
		{name: "dot directory", mutate: func(config *Config) { config.StorageSubdir = ".." }},
		{name: "duplicate exemption", mutate: func(config *Config) { config.ExemptTools = []string{"read", "read"} }},
		{name: "blank exemption", mutate: func(config *Config) { config.ExemptTools = []string{" read"} }},
		{name: "bad override", mutate: func(config *Config) { config.ToolOverrides = map[string]int{"": 1} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := testConfig()
			test.mutate(&config)
			if _, err := New(config, thread); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("New() error = %v", err)
			}
		})
	}
	config := testConfig()
	if _, err := New(config, nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New(nil workspace) error = %v", err)
	}
	var middleware *Middleware
	if _, err := middleware.TransformToolResult(context.Background(), domain.ToolCall{}, domain.ToolResult{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil TransformToolResult() error = %v", err)
	}
	valid, err := New(config, thread)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = valid.TransformToolResult(cancelled, domain.ToolCall{}, domain.ToolResult{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled TransformToolResult() error = %v", err)
	}
	if err = valid.BeforeModel(context.Background(), nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("BeforeModel(nil) error = %v", err)
	}
}

func TestDisabledMiddlewarePassesThrough(t *testing.T) {
	t.Parallel()
	thread := newTestWorkspace(t, 1<<20)
	config := testConfig()
	config.Enabled = false
	middleware, err := New(config, thread)
	if err != nil {
		t.Fatal(err)
	}
	result := domain.ToolResult{CallID: "call", Output: marshalText(strings.Repeat("x", 1000))}
	got, err := middleware.TransformToolResult(context.Background(), domain.ToolCall{Name: "tool"}, result)
	if err != nil || string(got.Output) != string(result.Output) {
		t.Fatalf("TransformToolResult() = %s, %v", got.Output, err)
	}
	request := model.Request{}
	if err = middleware.BeforeModel(context.Background(), &request); err != nil {
		t.Fatalf("BeforeModel() = %v", err)
	}
}

func newTestWorkspace(t *testing.T, maxUploadBytes int64) *workspace.Thread {
	t.Helper()
	manager, err := workspace.NewManager(workspace.Config{Root: t.TempDir(), MaxUploadBytes: maxUploadBytes})
	if err != nil {
		t.Fatal(err)
	}
	threadID, err := domain.NewThreadID()
	if err != nil {
		t.Fatal(err)
	}
	thread, err := manager.Open(threadID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = thread.Close() })
	return thread
}

func testConfig() Config {
	config := DefaultConfig()
	config.ExternalizeMinChars = 100
	config.FallbackMaxChars = 200
	return config
}

func between(value, prefix, suffix string) string {
	start := strings.Index(value, prefix)
	if start < 0 {
		return ""
	}
	start += len(prefix)
	end := strings.Index(value[start:], suffix)
	if end < 0 {
		return ""
	}
	return value[start : start+end]
}
