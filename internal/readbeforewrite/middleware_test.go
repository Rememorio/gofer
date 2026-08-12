package readbeforewrite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/model"
	"github.com/Rememorio/gofer/internal/runtime"
	"github.com/Rememorio/gofer/internal/workspace"
)

const testPath = workspace.OutputsRoot + "/report.md"

func TestGateBlocksBlindWritesAndAllowsNewFiles(t *testing.T) {
	t.Parallel()
	source := &fakeRevisionSource{revisions: map[string]string{testPath: testRevision("one")}}
	middleware := mustGate(t, "blind", source)
	var calls atomic.Int32
	next := func(_ context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		calls.Add(1)
		return successResult(call), nil
	}

	blocked, err := middleware.ExecuteTool(context.Background(), writeCall("blocked", testPath, false), next)
	if err != nil || !blocked.IsError || calls.Load() != 0 {
		t.Fatalf("blind write = %#v, calls=%d, err=%v", blocked, calls.Load(), err)
	}
	var payload struct {
		Code        string `json:"code"`
		Path        string `json:"path"`
		Recoverable bool   `json:"recoverable"`
	}
	if err = json.Unmarshal(blocked.Output, &payload); err != nil || payload.Code != "read_before_write" || payload.Path != testPath || !payload.Recoverable {
		t.Fatalf("blocked payload = %#v, %v", payload, err)
	}

	created, err := middleware.ExecuteTool(context.Background(), writeCall("new", workspace.OutputsRoot+"/new.md", false), next)
	if err != nil || created.IsError || calls.Load() != 1 {
		t.Fatalf("new write = %#v, calls=%d, err=%v", created, calls.Load(), err)
	}
	passthrough := domain.ToolCall{ID: "bash", Name: "bash", Arguments: json.RawMessage(`{"command":"true"}`)}
	if _, err = middleware.ExecuteTool(context.Background(), passthrough, next); err != nil || calls.Load() != 2 {
		t.Fatalf("non-file tool calls=%d, err=%v", calls.Load(), err)
	}
	malformed := domain.ToolCall{ID: "bad", Name: writeTool, Arguments: json.RawMessage(`{"content":"x"}`)}
	if _, err = middleware.ExecuteTool(context.Background(), malformed, next); err != nil || calls.Load() != 3 {
		t.Fatalf("malformed call calls=%d, err=%v", calls.Load(), err)
	}
}

func TestGateUsesFreshHistoryAndConsumesSuccessfulWrite(t *testing.T) {
	t.Parallel()
	revision := testRevision("one")
	source := &fakeRevisionSource{revisions: map[string]string{testPath: revision}}
	middleware := mustGate(t, "fresh", source)
	request := model.Request{Messages: readHistory("read-1", revision)}
	if err := middleware.BeforeModel(context.Background(), &request); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	next := func(_ context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		calls.Add(1)
		return successResult(call), nil
	}
	first, err := middleware.ExecuteTool(context.Background(), writeCall("write-1", testPath, true), next)
	if err != nil || first.IsError || calls.Load() != 1 {
		t.Fatalf("fresh write = %#v, calls=%d, err=%v", first, calls.Load(), err)
	}
	second, err := middleware.ExecuteTool(context.Background(), writeCall("write-2", testPath, true), next)
	if err != nil || !second.IsError || calls.Load() != 1 {
		t.Fatalf("second write = %#v, calls=%d, err=%v", second, calls.Load(), err)
	}
}

func TestGateRejectsStaleAndSummarizedMarks(t *testing.T) {
	t.Parallel()
	source := &fakeRevisionSource{revisions: map[string]string{testPath: testRevision("two")}}
	middleware := mustGate(t, "stale", source)
	request := model.Request{Messages: readHistory("read", testRevision("one"))}
	if err := middleware.BeforeModel(context.Background(), &request); err != nil {
		t.Fatal(err)
	}
	if result, err := middleware.ExecuteTool(context.Background(), writeCall("stale", testPath, false), successfulExecutor); err != nil || !result.IsError {
		t.Fatalf("stale write = %#v, %v", result, err)
	}
	request.Messages = []domain.Message{{Role: domain.RoleUser, Content: []domain.Content{{Kind: domain.ContentText, Text: "summary"}}}}
	if err := middleware.BeforeModel(context.Background(), &request); err != nil {
		t.Fatal(err)
	}
	if result, err := middleware.ExecuteTool(context.Background(), writeCall("summarized", testPath, false), successfulExecutor); err != nil || !result.IsError {
		t.Fatalf("summarized write = %#v, %v", result, err)
	}
}

func TestHistoryReconstructionTracksLatestSuccessfulVersion(t *testing.T) {
	t.Parallel()
	v1, v2 := testRevision("one"), testRevision("two")
	messages := append(readHistory("read-1", v1), writeHistory("write", testPath, false)...)
	if marks := reconstructMarks(messages); len(marks) != 0 {
		t.Fatalf("marks after write = %#v", marks)
	}
	messages = append(messages, readHistory("read-2", v2)...)
	if marks := reconstructMarks(messages); marks[testPath] != v2 {
		t.Fatalf("latest marks = %#v", marks)
	}
	messages = append(messages, writeHistory("failed", testPath, true)...)
	if marks := reconstructMarks(messages); marks[testPath] != v2 {
		t.Fatalf("failed write removed mark: %#v", marks)
	}
	invalid := readHistory("invalid", "forged")
	if marks := reconstructMarks(invalid); len(marks) != 0 {
		t.Fatalf("invalid revision accepted: %#v", marks)
	}
}

func TestGateNormalizesHistoryPaths(t *testing.T) {
	t.Parallel()
	revision := testRevision("one")
	alias := workspace.OutputsRoot + "/draft/../report.md"
	source := &fakeRevisionSource{revisions: map[string]string{alias: revision}}
	middleware := mustGate(t, "normalize", source)
	request := model.Request{Messages: readHistory("read", revision)}
	if err := middleware.BeforeModel(context.Background(), &request); err != nil {
		t.Fatal(err)
	}
	if result, err := middleware.ExecuteTool(context.Background(), writeCall("write", alias, false), successfulExecutor); err != nil || result.IsError {
		t.Fatalf("normalized write = %#v, %v", result, err)
	}
}

func TestGateFailsOpenWhenRevisionCannotBeInspected(t *testing.T) {
	t.Parallel()
	source := &fakeRevisionSource{errs: map[string]error{testPath: errors.New("unavailable")}}
	middleware := mustGate(t, "fail-open", source)
	var called bool
	result, err := middleware.ExecuteTool(context.Background(), writeCall("write", testPath, false), func(_ context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		called = true
		return successResult(call), nil
	})
	if err != nil || result.IsError || !called {
		t.Fatalf("fail-open write = %#v, called=%v, err=%v", result, called, err)
	}
}

func TestGateSerializesParallelWritesWithinOneRun(t *testing.T) {
	t.Parallel()
	thread := testWorkspace(t)
	if err := thread.WriteFile(testPath, []byte("one"), false); err != nil {
		t.Fatal(err)
	}
	revision, err := thread.Revision(testPath)
	if err != nil {
		t.Fatal(err)
	}
	middleware := mustGate(t, "same-run", thread)
	request := model.Request{Messages: readHistory("read", revision)}
	if err = middleware.BeforeModel(context.Background(), &request); err != nil {
		t.Fatal(err)
	}
	executor := workspaceAppendExecutor(thread)
	results := parallelWrites(t, middleware, middleware, executor)
	if countSuccessful(results) != 1 {
		t.Fatalf("parallel results = %#v", results)
	}
	read, err := thread.ReadFile(testPath, workspace.ReadOptions{})
	if err != nil || read.Content != "one+x" {
		t.Fatalf("content = %#v, %v", read, err)
	}
}

func TestGateSerializesParallelWritesAcrossAgents(t *testing.T) {
	t.Parallel()
	thread := testWorkspace(t)
	if err := thread.WriteFile(testPath, []byte("one"), false); err != nil {
		t.Fatal(err)
	}
	revision, err := thread.Revision(testPath)
	if err != nil {
		t.Fatal(err)
	}
	first := mustGate(t, "shared-thread", thread)
	second := mustGate(t, "shared-thread", thread)
	request := model.Request{Messages: readHistory("read", revision)}
	if err = first.BeforeModel(context.Background(), &request); err != nil {
		t.Fatal(err)
	}
	if err = second.BeforeModel(context.Background(), &request); err != nil {
		t.Fatal(err)
	}
	results := parallelWrites(t, first, second, workspaceAppendExecutor(thread))
	if countSuccessful(results) != 1 {
		t.Fatalf("cross-agent results = %#v", results)
	}
}

func TestGateCancellationAndValidation(t *testing.T) {
	t.Parallel()
	if _, err := New(Config{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New(empty) error = %v", err)
	}
	if _, err := New(Config{Scope: " bad", Files: &fakeRevisionSource{}}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New(spaced) error = %v", err)
	}
	middleware := mustGate(t, "cancel", &fakeRevisionSource{revisions: map[string]string{testPath: testRevision("one")}})
	if err := (*Middleware)(nil).BeforeModel(context.Background(), &model.Request{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil BeforeModel error = %v", err)
	}
	if err := middleware.BeforeModel(context.Background(), nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil request error = %v", err)
	}
	if _, err := middleware.ExecuteTool(context.Background(), domain.ToolCall{}, nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil next error = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := middleware.BeforeModel(cancelled, &model.Request{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("BeforeModel(cancelled) error = %v", err)
	}
	if _, err := middleware.ExecuteTool(cancelled, writeCall("cancel", testPath, false), successfulExecutor); !errors.Is(err, context.Canceled) {
		t.Fatalf("ExecuteTool(cancelled) error = %v", err)
	}
}

func TestPathLockCancellationCleansEntry(t *testing.T) {
	t.Parallel()
	table := newPathLockTable()
	unlock, err := table.acquire(context.Background(), "key")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, waitErr := table.acquire(ctx, "key")
		done <- waitErr
	}()
	cancel()
	if err = <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("acquire(cancelled) error = %v", err)
	}
	unlock()
	table.mu.Lock()
	remaining := len(table.entries)
	table.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("lock entries = %d", remaining)
	}
}

type fakeRevisionSource struct {
	revisions map[string]string
	errs      map[string]error
}

func (source *fakeRevisionSource) Revision(path string) (string, error) {
	if err := source.errs[path]; err != nil {
		return "", err
	}
	if revision, ok := source.revisions[path]; ok {
		return revision, nil
	}
	return "", fs.ErrNotExist
}

func mustGate(t *testing.T, scope string, files RevisionSource) *Middleware {
	t.Helper()
	middleware, err := New(Config{Scope: scope, Files: files})
	if err != nil {
		t.Fatal(err)
	}
	return middleware
}

func testRevision(seed string) string {
	return fmt.Sprintf("sha256:%064x", seed)
}

func writeCall(id, file string, appendMode bool) domain.ToolCall {
	arguments, _ := json.Marshal(map[string]any{"path": file, "content": "+x", "append": appendMode})
	return domain.ToolCall{ID: id, Name: writeTool, Arguments: arguments}
}

func successResult(call domain.ToolCall) domain.ToolResult {
	return domain.ToolResult{CallID: call.ID, Output: json.RawMessage(`{"ok":true}`)}
}

func successfulExecutor(_ context.Context, call domain.ToolCall) (domain.ToolResult, error) {
	return successResult(call), nil
}

func readHistory(id, revision string) []domain.Message {
	output, _ := json.Marshal(map[string]any{"content": "visible", "revision": revision})
	return toolHistory(domain.ToolCall{ID: id, Name: readTool, Arguments: pathArguments(testPath)}, domain.ToolResult{CallID: id, Output: output})
}

func writeHistory(id, file string, failed bool) []domain.Message {
	call := writeCall(id, file, false)
	return toolHistory(call, domain.ToolResult{CallID: id, Output: json.RawMessage(`{"ok":true}`), IsError: failed})
}

func toolHistory(call domain.ToolCall, result domain.ToolResult) []domain.Message {
	return []domain.Message{
		{Role: domain.RoleAssistant, Content: []domain.Content{{Kind: domain.ContentToolCall, ToolCall: &call}}},
		{Role: domain.RoleTool, Content: []domain.Content{{Kind: domain.ContentToolResult, ToolResult: &result}}},
	}
}

func pathArguments(file string) json.RawMessage {
	arguments, _ := json.Marshal(map[string]string{"path": file})
	return arguments
}

func testWorkspace(t *testing.T) *workspace.Thread {
	t.Helper()
	manager, err := workspace.NewManager(workspace.Config{Root: t.TempDir()})
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

func workspaceAppendExecutor(thread *workspace.Thread) runtime.ToolExecutor {
	return func(_ context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		if err := thread.WriteFile(testPath, []byte("+x"), true); err != nil {
			return domain.ToolResult{}, err
		}
		return successResult(call), nil
	}
}

func parallelWrites(t *testing.T, first, second *Middleware, executor runtime.ToolExecutor) []domain.ToolResult {
	t.Helper()
	start := make(chan struct{})
	results := make([]domain.ToolResult, 2)
	errs := make([]error, 2)
	var wait sync.WaitGroup
	for index, middleware := range []*Middleware{first, second} {
		wait.Add(1)
		go func(index int, middleware *Middleware) {
			defer wait.Done()
			<-start
			results[index], errs[index] = middleware.ExecuteTool(context.Background(), writeCall(fmt.Sprintf("write-%d", index), testPath, true), executor)
		}(index, middleware)
	}
	close(start)
	wait.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	return results
}

func countSuccessful(results []domain.ToolResult) int {
	count := 0
	for _, result := range results {
		if !result.IsError {
			count++
		}
	}
	return count
}

func TestPathLockWaitIsBounded(t *testing.T) {
	t.Parallel()
	table := newPathLockTable()
	unlock, err := table.acquire(context.Background(), "bounded")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err = table.acquire(ctx, "bounded"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("acquire(timeout) error = %v", err)
	}
}
