package workspacechange

import (
	"errors"
	"os"
	"sync"
)

const (
	// EventCategory is the stable run-event category for file-change evidence.
	EventCategory = "workspace"
	// MetadataKey is the stable event payload key containing the review result.
	MetadataKey   = "workspace_changes"
	resultVersion = 1
)

var (
	// ErrInvalidLimits identifies negative workspace-change bounds.
	ErrInvalidLimits = errors.New("invalid workspace change limits")
	// ErrInvalidWorkspace identifies a missing or unusable thread workspace.
	ErrInvalidWorkspace = errors.New("invalid workspace")
)

// Status identifies how a path changed during one run.
type Status string

const (
	// StatusCreated identifies a newly created regular path.
	StatusCreated Status = "created"
	// StatusModified identifies a changed existing path.
	StatusModified Status = "modified"
	// StatusDeleted identifies a removed path.
	StatusDeleted Status = "deleted"
	// StatusSymlinkCreated identifies a symlink newly occupying a path.
	StatusSymlinkCreated Status = "symlink_created"
)

// UnavailableReason explains why textual diff content was withheld.
type UnavailableReason string

const (
	// ReasonBinary withholds binary content.
	ReasonBinary UnavailableReason = "binary"
	// ReasonLarge withholds files beyond the per-file bound.
	ReasonLarge UnavailableReason = "large"
	// ReasonSensitive withholds content from secret-looking paths.
	ReasonSensitive UnavailableReason = "sensitive"
	// ReasonTruncated identifies a diff beyond the aggregate bound.
	ReasonTruncated UnavailableReason = "truncated"
	// ReasonSymlink withholds content behind symbolic links.
	ReasonSymlink UnavailableReason = "symlink"
)

// Limits bounds scan work, response cardinality, and persisted diff content.
type Limits struct {
	MaxFiles            int   `json:"max_files,omitempty"`
	MaxScannedFiles     int   `json:"max_scanned_files,omitempty"`
	MaxFileBytesForDiff int64 `json:"max_file_bytes_for_diff,omitempty"`
	MaxTotalDiffBytes   int64 `json:"max_total_diff_bytes,omitempty"`
}

// DefaultLimits returns the DeerFlow-compatible workspace review bounds.
func DefaultLimits() Limits {
	return Limits{
		MaxFiles: 200, MaxScannedFiles: 2000,
		MaxFileBytesForDiff: 256 << 10, MaxTotalDiffBytes: 1 << 20,
	}
}

func (limits Limits) normalized() (Limits, error) {
	if limits.MaxFiles < 0 || limits.MaxScannedFiles < 0 || limits.MaxFileBytesForDiff < 0 || limits.MaxTotalDiffBytes < 0 {
		return Limits{}, ErrInvalidLimits
	}
	defaults := DefaultLimits()
	if limits.MaxFiles == 0 {
		limits.MaxFiles = defaults.MaxFiles
	}
	if limits.MaxScannedFiles == 0 {
		limits.MaxScannedFiles = defaults.MaxScannedFiles
	}
	if limits.MaxFileBytesForDiff == 0 {
		limits.MaxFileBytesForDiff = defaults.MaxFileBytesForDiff
	}
	if limits.MaxTotalDiffBytes == 0 {
		limits.MaxTotalDiffBytes = defaults.MaxTotalDiffBytes
	}
	return limits, nil
}

// FileSnapshot is immutable metadata for one scanned path.
type FileSnapshot struct {
	Path                     string
	Root                     string
	Size                     int64
	ModifiedNanos            int64
	SHA256                   string
	Binary                   bool
	Sensitive                bool
	ContentUnavailableReason UnavailableReason
	Symlink                  bool
	SymlinkTarget            string
	text                     string
	textPath                 string
}

// Snapshot owns a point-in-time scan and its private text cache.
type Snapshot struct {
	Files     map[string]FileSnapshot
	Truncated bool
	cacheDir  string
	closeOnce sync.Once
	closeErr  error
}

// Close removes private baseline text retained for later diff construction.
func (snapshot *Snapshot) Close() error {
	if snapshot == nil {
		return nil
	}
	snapshot.closeOnce.Do(func() {
		if snapshot.cacheDir != "" {
			snapshot.closeErr = os.RemoveAll(snapshot.cacheDir)
		}
	})
	return snapshot.closeErr
}

// FileChange describes one created, modified, deleted, or replaced path.
type FileChange struct {
	Path                  string             `json:"path"`
	Root                  string             `json:"root"`
	Status                Status             `json:"status"`
	Binary                bool               `json:"binary"`
	Sensitive             bool               `json:"sensitive"`
	SizeBefore            *int64             `json:"size_before"`
	SizeAfter             *int64             `json:"size_after"`
	SHA256Before          *string            `json:"sha256_before"`
	SHA256After           *string            `json:"sha256_after"`
	Diff                  string             `json:"diff"`
	DiffTruncated         bool               `json:"diff_truncated"`
	DiffUnavailableReason *UnavailableReason `json:"diff_unavailable_reason"`
	Additions             int                `json:"additions"`
	Deletions             int                `json:"deletions"`
	Symlink               bool               `json:"symlink"`
	SymlinkTargetBefore   *string            `json:"symlink_target_before"`
	SymlinkTargetAfter    *string            `json:"symlink_target_after"`
}

// Summary contains aggregate line and file counts for one run.
type Summary struct {
	Created        int  `json:"created"`
	Modified       int  `json:"modified"`
	Deleted        int  `json:"deleted"`
	SymlinkCreated int  `json:"symlink_created"`
	Additions      int  `json:"additions"`
	Deletions      int  `json:"deletions"`
	Truncated      bool `json:"truncated"`
}

// Result is the versioned durable workspace-change contract.
type Result struct {
	Version int          `json:"version"`
	Summary Summary      `json:"summary"`
	Files   []FileChange `json:"files"`
	Limits  Limits       `json:"limits"`
}

// HasChanges reports whether a result contains any changed paths.
func (result Result) HasChanges() bool {
	return result.Summary.Created+result.Summary.Modified+result.Summary.Deleted+result.Summary.SymlinkCreated > 0
}

// EventPayload is persisted before a run's terminal journal event.
type EventPayload struct {
	Category         string `json:"category"`
	Content          string `json:"content"`
	WorkspaceChanges Result `json:"workspace_changes"`
}

// Response is the workspace-change review returned by the HTTP API.
type Response struct {
	Available bool `json:"available"`
	Result
}
