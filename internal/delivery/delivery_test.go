package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/event"
	"github.com/Rememorio/gofer/internal/runtime"
	"github.com/Rememorio/gofer/internal/workspace"
)

func TestTrackerBuildsIsolatedAttributedReceipt(t *testing.T) {
	t.Parallel()
	tracker := NewTracker()
	presentOutput := json.RawMessage(`[{"path":"/mnt/user-data/outputs/report.md"},{"path":"/mnt/user-data/outputs/appendix.md"}]`)
	call := domain.ToolCall{ID: "one", Name: ToolPresentFiles, Arguments: json.RawMessage(`{}`)}
	result := domain.ToolResult{CallID: call.ID, Output: presentOutput}
	if err := tracker.AfterTool(context.Background(), call, result); err != nil {
		t.Fatal(err)
	}
	_ = tracker.AfterTool(context.Background(), call, result)
	browser := domain.ToolCall{ID: "two", Name: "browser_screenshot", Arguments: json.RawMessage(`{}`)}
	_ = tracker.AfterTool(context.Background(), browser, domain.ToolResult{CallID: browser.ID, Output: json.RawMessage(`{"artifacts":[{"path":"/mnt/user-data/outputs/shot.png"}]}`)})
	_ = tracker.AfterTool(context.Background(), call, domain.ToolResult{CallID: call.ID, Output: presentOutput, IsError: true})

	receipt := tracker.Receipt([]string{workspace.OutputsRoot + "/report.md", workspace.OutputsRoot + "/new.md"})
	if receipt.Presented != 3 || len(receipt.Paths) != 3 || len(receipt.ByTool[ToolPresentFiles]) != 2 {
		t.Fatalf("receipt = %#v", receipt)
	}
	if receipt.Verdict == nil || !receipt.Satisfied || receipt.Stage != "presented" || len(receipt.MatchedPaths) != 1 || receipt.MatchedPaths[0] != workspace.OutputsRoot+"/report.md" {
		t.Fatalf("verdict = %#v", receipt.Verdict)
	}
	receipt.Paths[0] = "mutated"
	if next := tracker.Receipt(nil); next.Paths[0] == "mutated" || next.Verdict != nil {
		t.Fatalf("tracker state leaked = %#v", next)
	}
	var _ runtime.Middleware = tracker
}

func TestTrackerValidatesArtifactOutputShapes(t *testing.T) {
	t.Parallel()
	tracker := NewTracker()
	for index, output := range []json.RawMessage{
		json.RawMessage(`{"artifacts":["/mnt/user-data/outputs/one.txt",{"path":"/mnt/user-data/outputs/two.txt"}]}`),
		json.RawMessage(`[{"path":"/mnt/user-data/workspace/private.txt"},{"path":"/mnt/user-data/outputs/../bad"}]`),
		json.RawMessage(`{"artifacts":"bad"}`), json.RawMessage(`null`), json.RawMessage(`{`),
	} {
		call := domain.ToolCall{ID: "call", Name: "tool", Arguments: json.RawMessage(`{}`)}
		if err := tracker.AfterTool(context.Background(), call, domain.ToolResult{CallID: "call", Output: output}); err != nil {
			t.Fatalf("AfterTool(%d) = %v", index, err)
		}
	}
	if receipt := tracker.Receipt(nil); receipt.Presented != 2 || len(receipt.ByTool["tool"]) != 2 {
		t.Fatalf("receipt = %#v", receipt)
	}
	var nilTracker *Tracker
	if err := nilTracker.AfterTool(context.Background(), domain.ToolCall{}, domain.ToolResult{}); err != nil {
		t.Fatal(err)
	}
}

func TestDeliveryVerdictStagesAndCompletionErrors(t *testing.T) {
	t.Parallel()
	tracker := NewTracker()
	notStarted := tracker.Receipt([]string{workspace.OutputsRoot + "/report.md"})
	if notStarted.Stage != "not_started" || notStarted.Satisfied || !errors.Is(CompletionError(notStarted, nil), ErrIncomplete) {
		t.Fatalf("not started = %#v", notStarted)
	}
	tracker.record(ToolPresentFiles, workspace.OutputsRoot+"/other.md")
	mismatched := tracker.Receipt([]string{workspace.OutputsRoot + "/report.md"})
	if mismatched.Stage != "mismatched" || mismatched.Satisfied {
		t.Fatalf("mismatched = %#v", mismatched)
	}
	tracker.record(ToolPresentFiles, workspace.OutputsRoot+"/reports")
	covered := tracker.Receipt([]string{workspace.OutputsRoot + "/reports/daily.md"})
	if covered.Stage != "presented" || !covered.Satisfied || CompletionError(covered, nil) != nil {
		t.Fatalf("covered = %#v", covered)
	}
	persistErr := errors.New("store down")
	if err := CompletionError(mismatched, persistErr); !errors.Is(err, ErrIncomplete) || !errors.Is(err, ErrReceiptFailed) {
		t.Fatalf("combined error = %v", err)
	}
	if err := CompletionError(tracker.Receipt(nil), persistErr); err != nil {
		t.Fatalf("chat receipt error = %v", err)
	}
}

func TestReceiptJSONShape(t *testing.T) {
	t.Parallel()
	base, err := json.Marshal(NewTracker().Receipt(nil))
	if err != nil || string(base) != `{"presented":0,"paths":[],"by_tool":{}}` {
		t.Fatalf("base JSON = %s, %v", base, err)
	}
	detailed, err := json.Marshal(NewTracker().Receipt([]string{workspace.OutputsRoot + "/x"}))
	if err != nil || !json.Valid(detailed) || !containsJSONFields(detailed, "verification", "produced_paths", "presented_paths", "matched_paths", "stage", "satisfied") {
		t.Fatalf("detailed JSON = %s, %v", detailed, err)
	}
}

func TestPersistRetriesAndHonorsCancellation(t *testing.T) {
	original := receiptRetryDelays
	receiptRetryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { receiptRetryDelays = original })
	writer := &testWriter{failures: 2}
	if err := Persist(context.Background(), writer, NewTracker().Receipt(nil)); err != nil {
		t.Fatal(err)
	}
	if writer.calls != 3 || writer.kind != event.RunDelivery {
		t.Fatalf("writer = %#v", writer)
	}
	if payload, ok := writer.payload.(EventPayload); !ok || payload.Category != EventCategory {
		t.Fatalf("payload = %#v", writer.payload)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Persist(ctx, &testWriter{failures: 3}, NewTracker().Receipt(nil)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Persist(cancelled) = %v", err)
	}
	if err := Persist(context.Background(), nil, Receipt{}); err == nil {
		t.Fatal("Persist(nil writer) error = nil")
	}
}

type testWriter struct {
	mu       sync.Mutex
	failures int
	calls    int
	kind     event.Kind
	payload  any
}

func (writer *testWriter) Append(_ context.Context, kind event.Kind, payload any) error {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	writer.calls++
	writer.kind, writer.payload = kind, payload
	if writer.calls <= writer.failures {
		return errors.New("temporary")
	}
	return nil
}

func containsJSONFields(data []byte, fields ...string) bool {
	var object map[string]any
	if json.Unmarshal(data, &object) != nil {
		return false
	}
	for _, field := range fields {
		if _, ok := object[field]; !ok {
			return false
		}
	}
	return true
}
