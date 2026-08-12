package workspacechange

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/event"
	"github.com/Rememorio/gofer/internal/workspace"
)

func TestCaptureAndCompareTextChanges(t *testing.T) {
	t.Parallel()
	thread := testWorkspace(t)
	writeWorkspace(t, thread, workspace.WorkspaceRoot+"/draft.md", "alpha\nbeta\n")
	writeWorkspace(t, thread, workspace.WorkspaceRoot+"/old.txt", "remove me\n")
	before, err := Capture(thread, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	cacheDir := before.cacheDir
	defer func() { _ = before.Close() }()

	writeWorkspace(t, thread, workspace.WorkspaceRoot+"/draft.md", "alpha\ngamma\n")
	removeWorkspaceFile(t, thread, "workspace/old.txt")
	writeWorkspace(t, thread, workspace.OutputsRoot+"/report.md", "# Report\n\nReady\n")
	result, err := CompareCurrent(thread, before, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Version != 1 || result.Summary.Created != 1 || result.Summary.Modified != 1 || result.Summary.Deleted != 1 {
		t.Fatalf("summary = %#v", result.Summary)
	}
	changes := changesByPath(result.Files)
	if !strings.Contains(changes[workspace.WorkspaceRoot+"/draft.md"].Diff, "-beta") ||
		!strings.Contains(changes[workspace.WorkspaceRoot+"/draft.md"].Diff, "+gamma") ||
		changes[workspace.OutputsRoot+"/report.md"].Status != StatusCreated ||
		changes[workspace.WorkspaceRoot+"/old.txt"].Status != StatusDeleted {
		t.Fatalf("changes = %#v", changes)
	}
	if err = before.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(cacheDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache remains: %v", err)
	}
}

func TestCaptureProtectsSensitiveBinaryLargeAndSymlinkContent(t *testing.T) {
	t.Parallel()
	thread := testWorkspace(t)
	limits := Limits{MaxFileBytesForDiff: 16}
	before, err := Capture(thread, limits)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = before.Close() }()
	writeWorkspace(t, thread, workspace.WorkspaceRoot+"/.env", "SECRET_TOKEN=never-persist\n")
	if err = thread.WriteFile(workspace.WorkspaceRoot+"/image.png", []byte("\x89PNG\r\n\x1a\n\x00binary"), false); err != nil {
		t.Fatal(err)
	}
	writeWorkspace(t, thread, workspace.WorkspaceRoot+"/large.txt", strings.Repeat("x", 32))
	target := filepath.Join(t.TempDir(), "host.txt")
	if err = os.WriteFile(target, []byte("outside-body"), 0o600); err != nil {
		t.Fatal(err)
	}
	mount := workspaceMount(t, thread, workspace.WorkspaceRoot)
	if err = os.Symlink(target, filepath.Join(mount, "link.txt")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	result, err := CompareCurrent(thread, before, limits)
	if err != nil {
		t.Fatal(err)
	}
	changes := changesByPath(result.Files)
	assertUnavailable(t, changes[workspace.WorkspaceRoot+"/.env"], ReasonSensitive)
	assertUnavailable(t, changes[workspace.WorkspaceRoot+"/image.png"], ReasonBinary)
	assertUnavailable(t, changes[workspace.WorkspaceRoot+"/large.txt"], ReasonLarge)
	link := changes[workspace.WorkspaceRoot+"/link.txt"]
	if link.Status != StatusSymlinkCreated || !sameReason(link.DiffUnavailableReason, ReasonSymlink) || link.SymlinkTargetAfter == nil {
		t.Fatalf("link change = %#v", link)
	}
	encoded, _ := json.Marshal(result)
	if bytes.Contains(encoded, []byte("never-persist")) || bytes.Contains(encoded, []byte("outside-body")) {
		t.Fatalf("protected content leaked: %s", encoded)
	}
}

func TestCaptureHandlesTextEncodingsAndSampleBoundary(t *testing.T) {
	t.Parallel()
	thread := testWorkspace(t)
	before, err := Capture(thread, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = before.Close() }()
	if err = thread.WriteFile(workspace.WorkspaceRoot+"/utf16.md", encodeUTF16("# 标题\nhello\n"), false); err != nil {
		t.Fatal(err)
	}
	boundary := strings.Repeat("a", sampleBytes-1) + "你\nrest\n"
	writeWorkspace(t, thread, workspace.WorkspaceRoot+"/boundary.md", boundary)
	if err = thread.WriteFile(workspace.WorkspaceRoot+"/bom.md", append([]byte{0xef, 0xbb, 0xbf}, []byte("# title\n")...), false); err != nil {
		t.Fatal(err)
	}
	if err = thread.WriteFile(workspace.WorkspaceRoot+"/malformed.md", []byte{0xff, 0xfe, 0x00, 0xd8}, false); err != nil {
		t.Fatal(err)
	}
	result, err := CompareCurrent(thread, before, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	changes := changesByPath(result.Files)
	if changes[workspace.WorkspaceRoot+"/utf16.md"].Binary || !strings.Contains(changes[workspace.WorkspaceRoot+"/utf16.md"].Diff, "+# 标题") {
		t.Fatalf("utf16 = %#v", changes[workspace.WorkspaceRoot+"/utf16.md"])
	}
	if changes[workspace.WorkspaceRoot+"/boundary.md"].Binary || !strings.Contains(changes[workspace.WorkspaceRoot+"/boundary.md"].Diff, "你") {
		t.Fatalf("boundary = %#v", changes[workspace.WorkspaceRoot+"/boundary.md"])
	}
	if strings.Contains(changes[workspace.WorkspaceRoot+"/bom.md"].Diff, "\ufeff") {
		t.Fatal("UTF-8 BOM leaked into diff")
	}
	assertUnavailable(t, changes[workspace.WorkspaceRoot+"/malformed.md"], ReasonBinary)
}

func TestCompareAppliesFileScanAndDiffBounds(t *testing.T) {
	t.Parallel()
	thread := testWorkspace(t)
	limits := Limits{MaxFiles: 1, MaxScannedFiles: 10, MaxTotalDiffBytes: 24}
	writeWorkspace(t, thread, workspace.WorkspaceRoot+"/existing.txt", "old\n")
	before, err := Capture(thread, limits)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = before.Close() }()
	writeWorkspace(t, thread, workspace.WorkspaceRoot+"/existing.txt", "new\n")
	writeWorkspace(t, thread, workspace.WorkspaceRoot+"/second.txt", "created\n")
	result, err := CompareCurrent(thread, before, limits)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Files) != 1 || !result.Summary.Truncated || result.Summary.Created != 1 || result.Summary.Modified != 1 {
		t.Fatalf("bounded result = %#v", result)
	}
	if result.Files[0].Diff != "" || !result.Files[0].DiffTruncated || !sameReason(result.Files[0].DiffUnavailableReason, ReasonTruncated) {
		t.Fatalf("bounded diff = %#v", result.Files[0])
	}

	limited, err := Capture(thread, Limits{MaxScannedFiles: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = limited.Close() }()
	if !limited.Truncated || len(limited.Files) != 1 {
		t.Fatalf("limited snapshot = %#v", limited)
	}
}

func TestCaptureExcludesUploadsAndProcessDirectories(t *testing.T) {
	t.Parallel()
	thread := testWorkspace(t)
	if _, err := thread.PutUpload("input.txt", strings.NewReader("user input")); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{"node_modules", ".browser-frames", ".tool-results"} {
		writeWorkspace(t, thread, workspace.WorkspaceRoot+"/"+directory+"/ignored.txt", "ignored")
	}
	writeWorkspace(t, thread, workspace.WorkspaceRoot+"/visible.txt", "visible")
	snapshot, err := Capture(thread, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snapshot.Close() }()
	if len(snapshot.Files) != 1 || snapshot.Files[workspace.WorkspaceRoot+"/visible.txt"].Path == "" {
		t.Fatalf("snapshot files = %#v", snapshot.Files)
	}
}

func TestCompareAndCaptureValidation(t *testing.T) {
	t.Parallel()
	thread := testWorkspace(t)
	if _, err := Capture(nil, Limits{}); !errors.Is(err, ErrInvalidWorkspace) {
		t.Fatalf("Capture(nil) = %v", err)
	}
	if _, err := Capture(thread, Limits{MaxFiles: -1}); !errors.Is(err, ErrInvalidLimits) {
		t.Fatalf("Capture(invalid) = %v", err)
	}
	if _, err := CompareCurrent(thread, nil, Limits{}); !errors.Is(err, ErrInvalidWorkspace) {
		t.Fatalf("CompareCurrent(nil) = %v", err)
	}
	if _, err := Compare(nil, &Snapshot{}, Limits{}); !errors.Is(err, ErrInvalidWorkspace) {
		t.Fatalf("Compare(nil) = %v", err)
	}
	before := &Snapshot{Files: map[string]FileSnapshot{"/x": {Path: "/x", Size: 1, textPath: filepath.Join(t.TempDir(), "missing")}}}
	after := &Snapshot{Files: map[string]FileSnapshot{}}
	if _, err := Compare(before, after, Limits{}); err != nil {
		t.Fatalf("deleted cached file should remain diffable: %v", err)
	}
	after.Files["/x"] = FileSnapshot{Path: "/x", Size: 2, text: "x"}
	if result, err := Compare(before, after, Limits{}); err != nil || result.Files[0].Diff != "" {
		t.Fatalf("Compare(missing cache) = %#v, %v", result, err)
	}
	var nilSnapshot *Snapshot
	if err := nilSnapshot.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestResponseProjectionUsesLatestEventAndFiltersDetails(t *testing.T) {
	t.Parallel()
	threadID, _ := domain.NewThreadID()
	runID, _ := domain.NewRunID()
	result := Result{
		Version: 1, Summary: Summary{Created: 1, Additions: 1}, Limits: DefaultLimits(),
		Files: []FileChange{{Path: workspace.OutputsRoot + "/report.md", Status: StatusCreated, Diff: "+ready"}},
	}
	old := committedEvent(t, threadID, runID, NewEventPayload(Result{Version: 1, Summary: Summary{Deleted: 1}, Files: []FileChange{}}), 1)
	latest := committedEvent(t, threadID, runID, NewEventPayload(result), 2)
	response := ResponseFromEvents([]event.Event{old, latest}, true, false)
	if !response.Available || response.Summary.Created != 1 || len(response.Files) != 1 || response.Files[0].Diff != "" {
		t.Fatalf("filtered response = %#v", response)
	}
	withoutFiles := ResponseFromEvents([]event.Event{latest}, false, true)
	if !withoutFiles.Available || withoutFiles.Files == nil || len(withoutFiles.Files) != 0 {
		t.Fatalf("without files = %#v", withoutFiles)
	}
	malformed := committedEvent(t, threadID, runID, map[string]string{"bad": "payload"}, 3)
	if got := ResponseFromEvents([]event.Event{malformed}, true, true); got.Available || got.Files == nil || got.Limits != (Limits{}) {
		t.Fatalf("malformed response = %#v", got)
	}
	if got := ResponseFromEvents(nil, true, true); got.Available || got.Version != 1 {
		t.Fatalf("empty response = %#v", got)
	}
	if payload := NewEventPayload(result); payload.Content != "1 file changed +1 -0" || payload.Category != EventCategory {
		t.Fatalf("payload = %#v", payload)
	}
	encoded, _ := json.Marshal(response)
	if !bytes.Contains(encoded, []byte(`"diff_unavailable_reason":null`)) {
		t.Fatalf("normal diff reason shape = %s", encoded)
	}
}

func testWorkspace(t *testing.T) *workspace.Thread {
	t.Helper()
	manager, err := workspace.NewManager(workspace.Config{Root: t.TempDir(), MaxWriteBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	threadID, _ := domain.NewThreadID()
	thread, err := manager.Open(threadID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = thread.Close() })
	return thread
}

func writeWorkspace(t *testing.T, thread *workspace.Thread, filename, content string) {
	t.Helper()
	if err := thread.WriteFile(filename, []byte(content), false); err != nil {
		t.Fatal(err)
	}
}

func removeWorkspaceFile(t *testing.T, thread *workspace.Thread, relative string) {
	t.Helper()
	for _, mount := range thread.ExecutionMounts() {
		if strings.HasPrefix(relative, pathBase(mount.VirtualPath)+"/") {
			if err := os.Remove(filepath.Join(filepath.Dir(mount.HostPath), filepath.FromSlash(relative))); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatalf("mount for %q not found", relative)
}

func workspaceMount(t *testing.T, thread *workspace.Thread, virtualPath string) string {
	t.Helper()
	for _, mount := range thread.ExecutionMounts() {
		if mount.VirtualPath == virtualPath {
			return mount.HostPath
		}
	}
	t.Fatalf("mount %q not found", virtualPath)
	return ""
}

func pathBase(virtualPath string) string {
	parts := strings.Split(strings.Trim(virtualPath, "/"), "/")
	return parts[len(parts)-1]
}

func changesByPath(changes []FileChange) map[string]FileChange {
	result := make(map[string]FileChange, len(changes))
	for _, change := range changes {
		result[change.Path] = change
	}
	return result
}

func assertUnavailable(t *testing.T, change FileChange, reason UnavailableReason) {
	t.Helper()
	if change.Diff != "" || !sameReason(change.DiffUnavailableReason, reason) {
		t.Fatalf("change = %#v, want reason %q", change, reason)
	}
}

func sameReason(got *UnavailableReason, want UnavailableReason) bool {
	return got != nil && *got == want
}

func encodeUTF16(value string) []byte {
	units := utf16.Encode([]rune(value))
	data := make([]byte, 2+len(units)*2)
	data[0], data[1] = 0xff, 0xfe
	for index, unit := range units {
		binary.LittleEndian.PutUint16(data[2+index*2:], unit)
	}
	return data
}

func committedEvent(t *testing.T, threadID domain.ThreadID, runID domain.RunID, payload any, sequence uint64) event.Event {
	t.Helper()
	draft, err := event.NewDraft(threadID, runID, event.WorkspaceChanges, time.Now(), payload)
	if err != nil {
		t.Fatal(err)
	}
	record, err := draft.Commit(sequence)
	if err != nil {
		t.Fatal(err)
	}
	return record
}
