package delivery

import (
	"context"
	"encoding/json"
	"path"
	"strings"
	"sync"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/runtime"
	"github.com/Rememorio/gofer/internal/workspace"
)

type presentation struct {
	path string
	tool string
}

// Tracker observes successful tool results and owns presentation state for one
// parent run and all of its child agents.
type Tracker struct {
	runtime.NopMiddleware
	mu      sync.Mutex
	entries []presentation
	seen    map[presentation]struct{}
}

// NewTracker constructs an empty run-scoped tracker.
func NewTracker() *Tracker { return &Tracker{seen: make(map[presentation]struct{})} }

// AfterTool records artifact paths from one successful structured tool result.
func (tracker *Tracker) AfterTool(_ context.Context, call domain.ToolCall, result domain.ToolResult) error {
	if tracker == nil || result.IsError {
		return nil
	}
	for _, artifactPath := range artifactPaths(result.Output) {
		tracker.record(call.Name, artifactPath)
	}
	return nil
}

// Receipt returns an isolated terminal fact and optional produced-output verdict.
func (tracker *Tracker) Receipt(producedPaths []string) Receipt {
	receipt := EmptyReceipt()
	if tracker != nil {
		tracker.mu.Lock()
		for _, entry := range tracker.entries {
			receipt.Paths = append(receipt.Paths, entry.path)
			receipt.ByTool[entry.tool] = append(receipt.ByTool[entry.tool], entry.path)
		}
		tracker.mu.Unlock()
	}
	receipt.Presented = len(receipt.Paths)
	if len(producedPaths) > 0 {
		receipt.Verdict = newVerdict(producedPaths, receipt.ByTool[ToolPresentFiles])
	}
	return receipt
}

func (tracker *Tracker) record(toolName, artifactPath string) {
	toolName = strings.TrimSpace(toolName)
	artifactPath = strings.TrimSpace(artifactPath)
	if toolName == "" || !validOutputPath(artifactPath) {
		return
	}
	entry := presentation{path: artifactPath, tool: toolName}
	tracker.mu.Lock()
	if _, exists := tracker.seen[entry]; !exists {
		tracker.seen[entry] = struct{}{}
		tracker.entries = append(tracker.entries, entry)
	}
	tracker.mu.Unlock()
}

func artifactPaths(output json.RawMessage) []string {
	var value any
	if json.Unmarshal(output, &value) != nil {
		return nil
	}
	switch typed := value.(type) {
	case []any:
		return pathsFromArtifacts(typed)
	case map[string]any:
		artifacts, _ := typed["artifacts"].([]any)
		return pathsFromArtifacts(artifacts)
	default:
		return nil
	}
}

func pathsFromArtifacts(artifacts []any) []string {
	paths := make([]string, 0, len(artifacts))
	for _, candidate := range artifacts {
		switch typed := candidate.(type) {
		case string:
			paths = append(paths, typed)
		case map[string]any:
			if artifactPath, ok := typed["path"].(string); ok {
				paths = append(paths, artifactPath)
			}
		}
	}
	return paths
}

func validOutputPath(candidate string) bool {
	return strings.HasPrefix(candidate, workspace.OutputsRoot+"/") && path.Clean(candidate) == candidate
}
