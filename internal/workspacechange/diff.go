package workspacechange

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/pmezard/go-difflib/difflib"
)

// Compare constructs a bounded, stable workspace review from two snapshots.
func Compare(before, after *Snapshot, limits Limits) (Result, error) {
	if before == nil || after == nil {
		return Result{}, fmt.Errorf("%w: both snapshots are required", ErrInvalidWorkspace)
	}
	resolved, err := limits.normalized()
	if err != nil {
		return Result{}, err
	}
	state := compareState{limits: resolved, files: make([]FileChange, 0), truncated: before.Truncated || after.Truncated}
	for _, virtualPath := range unionPaths(before, after) {
		left, leftOK := before.Files[virtualPath]
		right, rightOK := after.Files[virtualPath]
		if leftOK && rightOK && sameFile(left, right) {
			continue
		}
		if err := state.addChange(virtualPath, optionalSnapshot(left, leftOK), optionalSnapshot(right, rightOK)); err != nil {
			return Result{}, err
		}
	}
	state.summary.Truncated = state.truncated
	publicLimits := resolved
	publicLimits.ExcludedDirectoryName = ""
	return Result{Version: resultVersion, Summary: state.summary, Files: state.files, Limits: publicLimits}, nil
}

type compareState struct {
	limits         Limits
	summary        Summary
	files          []FileChange
	totalDiffBytes int64
	truncated      bool
}

func (state *compareState) addChange(virtualPath string, before, after *FileSnapshot) error {
	status := changeStatus(before, after)
	state.countStatus(status)
	remaining := max(int64(0), state.limits.MaxTotalDiffBytes-state.totalDiffBytes)
	diff, additions, deletions, diffTruncated, reason, err := buildDiff(virtualPath, before, after, remaining)
	if err != nil {
		return err
	}
	state.totalDiffBytes += int64(len([]byte(diff)))
	state.summary.Additions += additions
	state.summary.Deletions += deletions
	if diffTruncated || reason == ReasonLarge || reason == ReasonTruncated {
		state.truncated = true
	}
	if len(state.files) >= state.limits.MaxFiles {
		state.truncated = true
		return nil
	}
	state.files = append(state.files, newFileChange(virtualPath, status, before, after, diffResult{
		diff: diff, additions: additions, deletions: deletions, truncated: diffTruncated, reason: reason,
	}))
	return nil
}

func (state *compareState) countStatus(status Status) {
	switch status {
	case StatusCreated:
		state.summary.Created++
	case StatusModified:
		state.summary.Modified++
	case StatusDeleted:
		state.summary.Deleted++
	case StatusSymlinkCreated:
		state.summary.SymlinkCreated++
	}
}

type diffResult struct {
	diff      string
	additions int
	deletions int
	truncated bool
	reason    UnavailableReason
}

func newFileChange(virtualPath string, status Status, before, after *FileSnapshot, result diffResult) FileChange {
	sample := after
	if sample == nil {
		sample = before
	}
	return FileChange{
		Path: virtualPath, Root: sample.Root, Status: status,
		Binary: sample.Binary, Sensitive: sample.Sensitive,
		SizeBefore: sizePointer(before), SizeAfter: sizePointer(after),
		SHA256Before: stringPointer(before, func(file *FileSnapshot) string { return file.SHA256 }),
		SHA256After:  stringPointer(after, func(file *FileSnapshot) string { return file.SHA256 }),
		Diff:         result.diff, DiffTruncated: result.truncated, DiffUnavailableReason: reasonPointer(result.reason),
		Additions: result.additions, Deletions: result.deletions, Symlink: sample.Symlink,
		SymlinkTargetBefore: stringPointer(before, func(file *FileSnapshot) string { return file.SymlinkTarget }),
		SymlinkTargetAfter:  stringPointer(after, func(file *FileSnapshot) string { return file.SymlinkTarget }),
	}
}

func buildDiff(virtualPath string, before, after *FileSnapshot, remaining int64) (string, int, int, bool, UnavailableReason, error) {
	if reason := unavailableReason(before, after); reason != "" {
		return "", 0, 0, false, reason, nil
	}
	left, leftOK := snapshotText(before)
	right, rightOK := snapshotText(after)
	if !leftOK || !rightOK {
		return "", 0, 0, false, "", nil
	}
	diff, err := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
		A: difflib.SplitLines(left), B: difflib.SplitLines(right),
		FromFile: "a" + virtualPath, ToFile: "b" + virtualPath, Context: 3,
	})
	if err != nil {
		return "", 0, 0, false, "", fmt.Errorf("build workspace diff: %w", err)
	}
	additions, deletions := countDiffLines(diff)
	if int64(len([]byte(diff))) > remaining {
		return "", additions, deletions, true, ReasonTruncated, nil
	}
	return strings.TrimSuffix(diff, "\n"), additions, deletions, false, "", nil
}

func unavailableReason(before, after *FileSnapshot) UnavailableReason {
	files := []*FileSnapshot{before, after}
	for _, preferred := range []UnavailableReason{ReasonSymlink, ReasonSensitive, ReasonBinary, ReasonLarge} {
		for _, file := range files {
			if file != nil && file.ContentUnavailableReason == preferred {
				return preferred
			}
		}
	}
	return ""
}

func snapshotText(snapshot *FileSnapshot) (string, bool) {
	if snapshot == nil {
		return "", true
	}
	if snapshot.textPath != "" {
		data, err := os.ReadFile(snapshot.textPath)
		if err != nil {
			return "", false
		}
		return string(data), true
	}
	if snapshot.text != "" || snapshot.Size == 0 {
		return snapshot.text, true
	}
	return "", false
}

func countDiffLines(diff string) (int, int) {
	additions, deletions := 0, 0
	for index, line := range strings.Split(diff, "\n") {
		if index < 2 {
			continue
		}
		if strings.HasPrefix(line, "+") {
			additions++
		} else if strings.HasPrefix(line, "-") {
			deletions++
		}
	}
	return additions, deletions
}

func changeStatus(before, after *FileSnapshot) Status {
	if after != nil && after.Symlink && (before == nil || !before.Symlink) {
		return StatusSymlinkCreated
	}
	if before == nil {
		return StatusCreated
	}
	if after == nil {
		return StatusDeleted
	}
	return StatusModified
}

func sameFile(before, after FileSnapshot) bool {
	if before.Symlink != after.Symlink {
		return false
	}
	if before.Symlink {
		return before.SymlinkTarget == after.SymlinkTarget && before.ModifiedNanos == after.ModifiedNanos
	}
	if before.SHA256 != "" && after.SHA256 != "" {
		return before.SHA256 == after.SHA256
	}
	return before.Size == after.Size && before.ModifiedNanos == after.ModifiedNanos
}

func changedPaths(before, after *Snapshot) map[string]struct{} {
	changed := make(map[string]struct{})
	for _, virtualPath := range unionPaths(before, after) {
		left, leftOK := before.Files[virtualPath]
		right, rightOK := after.Files[virtualPath]
		if !leftOK || !rightOK || !sameFile(left, right) {
			changed[virtualPath] = struct{}{}
		}
	}
	return changed
}

func unionPaths(before, after *Snapshot) []string {
	paths := make(map[string]struct{}, len(before.Files)+len(after.Files))
	for virtualPath := range before.Files {
		paths[virtualPath] = struct{}{}
	}
	for virtualPath := range after.Files {
		paths[virtualPath] = struct{}{}
	}
	result := make([]string, 0, len(paths))
	for virtualPath := range paths {
		result = append(result, virtualPath)
	}
	sort.Strings(result)
	return result
}

func optionalSnapshot(snapshot FileSnapshot, present bool) *FileSnapshot {
	if !present {
		return nil
	}
	return &snapshot
}

func sizePointer(snapshot *FileSnapshot) *int64 {
	if snapshot == nil {
		return nil
	}
	value := snapshot.Size
	return &value
}

func stringPointer(snapshot *FileSnapshot, value func(*FileSnapshot) string) *string {
	if snapshot == nil || value(snapshot) == "" {
		return nil
	}
	result := value(snapshot)
	return &result
}

func reasonPointer(reason UnavailableReason) *UnavailableReason {
	if reason == "" {
		return nil
	}
	return &reason
}

func changedRegularOutputPaths(before, after *Snapshot) []string {
	paths := make([]string, 0)
	for virtualPath, file := range after.Files {
		if file.Root != "outputs" || file.Symlink {
			continue
		}
		previous, exists := before.Files[virtualPath]
		if !exists || !sameFile(previous, file) {
			paths = append(paths, virtualPath)
		}
	}
	sort.Strings(paths)
	return paths
}
