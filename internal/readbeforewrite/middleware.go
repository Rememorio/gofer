package readbeforewrite

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"sync"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/model"
	"github.com/Rememorio/gofer/internal/runtime"
)

const (
	readTool    = "read_file"
	writeTool   = "write_file"
	replaceTool = "str_replace"
)

var (
	// ErrInvalidConfig identifies missing read-before-write dependencies.
	ErrInvalidConfig = errors.New("invalid read-before-write configuration")
	sharedPathLocks  = newPathLockTable()
)

// RevisionSource returns a stable digest of one complete inspectable file.
type RevisionSource interface {
	Revision(string) (string, error)
}

// Config binds one run-scoped gate to a workspace and concurrency scope.
type Config struct {
	Scope string
	Files RevisionSource
}

// Middleware enforces one fresh read for each existing-file modification.
// Marks are reconstructed from model-visible history before every turn, so
// compaction naturally invalidates reads that are no longer in context.
type Middleware struct {
	runtime.NopMiddleware
	scope string
	files RevisionSource
	mu    sync.RWMutex
	marks map[string]string
}

var (
	_ runtime.Middleware               = (*Middleware)(nil)
	_ runtime.ToolExecutionInterceptor = (*Middleware)(nil)
)

// New validates config and constructs an empty version gate.
func New(config Config) (*Middleware, error) {
	if config.Files == nil || strings.TrimSpace(config.Scope) != config.Scope || config.Scope == "" {
		return nil, fmt.Errorf("%w: scope and revision source are required", ErrInvalidConfig)
	}
	return &Middleware{scope: config.Scope, files: config.Files, marks: make(map[string]string)}, nil
}

// BeforeModel rebuilds fresh-read marks from the exact conversation that will
// be sent to the model. Missing summarized history therefore fails closed.
func (middleware *Middleware) BeforeModel(ctx context.Context, request *model.Request) error {
	if middleware == nil || middleware.files == nil || request == nil {
		return fmt.Errorf("%w: middleware and request are required", ErrInvalidConfig)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	marks := reconstructMarks(request.Messages)
	middleware.mu.Lock()
	middleware.marks = marks
	middleware.mu.Unlock()
	return nil
}

// ExecuteTool serializes same-path modifications, checks the current file
// revision, and returns a model-correctable error instead of executing a blind
// write. New files and uninspectable paths pass through to the native tool.
func (middleware *Middleware) ExecuteTool(ctx context.Context, call domain.ToolCall, next runtime.ToolExecutor) (domain.ToolResult, error) {
	if middleware == nil || middleware.files == nil || next == nil {
		return domain.ToolResult{}, fmt.Errorf("%w: middleware and next executor are required", ErrInvalidConfig)
	}
	if err := ctx.Err(); err != nil {
		return domain.ToolResult{}, err
	}
	requestedPath, normalizedPath, gated := writePath(call)
	if !gated {
		return next(ctx, call)
	}
	unlock, err := sharedPathLocks.acquire(ctx, middleware.scope+"\x00"+normalizedPath)
	if err != nil {
		return domain.ToolResult{}, err
	}
	defer unlock()

	current, err := middleware.files.Revision(requestedPath)
	if errors.Is(err, fs.ErrNotExist) {
		return next(ctx, call)
	}
	if err != nil {
		return next(ctx, call)
	}
	if middleware.mark(normalizedPath) != current {
		return blockedResult(call, requestedPath), nil
	}
	result, err := next(ctx, call)
	if err == nil && !result.IsError {
		middleware.consume(normalizedPath)
	}
	return result, err
}

type callRecord struct {
	name string
	path string
}

func reconstructMarks(messages []domain.Message) map[string]string {
	marks := make(map[string]string)
	calls := make(map[string]callRecord)
	for _, message := range messages {
		for _, content := range message.Content {
			if record, ok := recordedCall(content); ok {
				calls[content.ToolCall.ID] = record
				continue
			}
			applyHistoricalResult(content, calls, marks)
		}
	}
	return marks
}

func recordedCall(content domain.Content) (callRecord, bool) {
	if content.Kind != domain.ContentToolCall || content.ToolCall == nil {
		return callRecord{}, false
	}
	_, normalizedPath, ok := recognizedPath(*content.ToolCall)
	if !ok {
		return callRecord{}, false
	}
	return callRecord{name: content.ToolCall.Name, path: normalizedPath}, true
}

func applyHistoricalResult(content domain.Content, calls map[string]callRecord, marks map[string]string) {
	if content.Kind != domain.ContentToolResult || content.ToolResult == nil || content.ToolResult.IsError {
		return
	}
	record, ok := calls[content.ToolResult.CallID]
	if !ok {
		return
	}
	if record.name == readTool {
		if revision := resultRevision(content.ToolResult.Output); revision != "" {
			marks[record.path] = revision
		}
		return
	}
	delete(marks, record.path)
}

func resultRevision(output json.RawMessage) string {
	var result struct {
		Revision string `json:"revision"`
	}
	if err := json.Unmarshal(output, &result); err != nil || !validRevision(result.Revision) {
		return ""
	}
	return result.Revision
}

func validRevision(revision string) bool {
	digest, found := strings.CutPrefix(revision, "sha256:")
	if !found || len(digest) != 64 {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

func writePath(call domain.ToolCall) (string, string, bool) {
	if call.Name != writeTool && call.Name != replaceTool {
		return "", "", false
	}
	return recognizedPath(call)
}

func recognizedPath(call domain.ToolCall) (string, string, bool) {
	if call.Name != readTool && call.Name != writeTool && call.Name != replaceTool {
		return "", "", false
	}
	var arguments struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(call.Arguments, &arguments); err != nil || arguments.Path == "" {
		return "", "", false
	}
	return arguments.Path, path.Clean(arguments.Path), true
}

func blockedResult(call domain.ToolCall, requestedPath string) domain.ToolResult {
	output, _ := json.Marshal(map[string]any{
		"code":        "read_before_write",
		"error":       fmt.Sprintf("%s blocked: %s already exists and its current version has not been read; call read_file and retry", call.Name, requestedPath),
		"path":        requestedPath,
		"recoverable": true,
	})
	return domain.ToolResult{CallID: call.ID, Output: output, IsError: true}
}

func (middleware *Middleware) mark(normalizedPath string) string {
	middleware.mu.RLock()
	revision := middleware.marks[normalizedPath]
	middleware.mu.RUnlock()
	return revision
}

func (middleware *Middleware) consume(normalizedPath string) {
	middleware.mu.Lock()
	delete(middleware.marks, normalizedPath)
	middleware.mu.Unlock()
}

type pathLockTable struct {
	mu      sync.Mutex
	entries map[string]*pathLock
}

type pathLock struct {
	token chan struct{}
	refs  int
}

func newPathLockTable() *pathLockTable {
	return &pathLockTable{entries: make(map[string]*pathLock)}
}

func (table *pathLockTable) acquire(ctx context.Context, key string) (func(), error) {
	table.mu.Lock()
	entry := table.entries[key]
	if entry == nil {
		entry = &pathLock{token: make(chan struct{}, 1)}
		entry.token <- struct{}{}
		table.entries[key] = entry
	}
	entry.refs++
	table.mu.Unlock()

	select {
	case <-entry.token:
		return func() {
			entry.token <- struct{}{}
			table.releaseRef(key, entry)
		}, nil
	case <-ctx.Done():
		table.releaseRef(key, entry)
		return nil, ctx.Err()
	}
}

func (table *pathLockTable) releaseRef(key string, entry *pathLock) {
	table.mu.Lock()
	entry.refs--
	if entry.refs == 0 && table.entries[key] == entry {
		delete(table.entries, key)
	}
	table.mu.Unlock()
}
