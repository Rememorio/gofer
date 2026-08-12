package artifact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/workspace"
)

var (
	// ErrInvalidArtifact identifies invalid artifact metadata or paths.
	ErrInvalidArtifact = errors.New("invalid artifact")
	// ErrNotFound identifies an artifact absent from a thread catalog.
	ErrNotFound = errors.New("artifact not found")
)

// Artifact is a user-visible generated file.
type Artifact struct {
	ThreadID    domain.ThreadID `json:"thread_id"`
	Path        string          `json:"path"`
	Name        string          `json:"name"`
	MediaType   string          `json:"media_type"`
	Size        int64           `json:"size"`
	ModifiedAt  time.Time       `json:"modified_at"`
	PresentedAt time.Time       `json:"presented_at"`
}

// Catalog stores deduplicated artifact metadata by thread and virtual path.
type Catalog struct {
	mu      sync.RWMutex
	entries map[domain.ThreadID]map[string]Artifact
}

// NewCatalog constructs an empty artifact catalog.
func NewCatalog() *Catalog {
	return &Catalog{entries: make(map[domain.ThreadID]map[string]Artifact)}
}

// Present validates and records outputs paths in input order.
func (catalog *Catalog) Present(
	ctx context.Context,
	thread *workspace.Thread,
	paths []string,
	at time.Time,
) ([]Artifact, error) {
	if catalog == nil || thread == nil {
		return nil, fmt.Errorf("%w: catalog and workspace are required", ErrInvalidArtifact)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if at.IsZero() || len(paths) == 0 {
		return nil, fmt.Errorf("%w: paths and presentation time are required", ErrInvalidArtifact)
	}
	unique := make(map[string]struct{}, len(paths))
	artifacts := make([]Artifact, 0, len(paths))
	for _, virtualPath := range paths {
		if _, exists := unique[virtualPath]; exists {
			continue
		}
		unique[virtualPath] = struct{}{}
		if !isOutputPath(virtualPath) {
			return nil, fmt.Errorf("%w: only %s files can be presented", ErrInvalidArtifact, workspace.OutputsRoot)
		}
		if thread.IsInternalOutputPath(virtualPath) {
			return nil, fmt.Errorf("%w: internal output files cannot be presented", ErrInvalidArtifact)
		}
		entry, err := thread.Inspect(virtualPath)
		if err != nil {
			return nil, err
		}
		if entry.Directory {
			return nil, fmt.Errorf("%w: %s is a directory", ErrInvalidArtifact, virtualPath)
		}
		mediaType := mediaTypeForPath(virtualPath)
		artifacts = append(artifacts, Artifact{
			ThreadID: thread.ID(), Path: virtualPath, Name: path.Base(virtualPath),
			MediaType: mediaType, Size: entry.Size, ModifiedAt: entry.ModifiedAt, PresentedAt: at.UTC(),
		})
	}
	catalog.mu.Lock()
	threadEntries := catalog.entries[thread.ID()]
	if threadEntries == nil {
		threadEntries = make(map[string]Artifact)
		catalog.entries[thread.ID()] = threadEntries
	}
	for _, artifact := range artifacts {
		threadEntries[artifact.Path] = artifact
	}
	catalog.mu.Unlock()
	return append([]Artifact(nil), artifacts...), nil
}

// List returns a stable snapshot ordered by virtual path.
func (catalog *Catalog) List(threadID domain.ThreadID) []Artifact {
	if catalog == nil {
		return nil
	}
	catalog.mu.RLock()
	artifacts := make([]Artifact, 0, len(catalog.entries[threadID]))
	for _, artifact := range catalog.entries[threadID] {
		artifacts = append(artifacts, artifact)
	}
	catalog.mu.RUnlock()
	sort.Slice(artifacts, func(left, right int) bool { return artifacts[left].Path < artifacts[right].Path })
	return artifacts
}

// RemoveThread forgets all presented artifacts for one deleted thread.
func (catalog *Catalog) RemoveThread(threadID domain.ThreadID) {
	if catalog == nil {
		return
	}
	catalog.mu.Lock()
	delete(catalog.entries, threadID)
	catalog.mu.Unlock()
}

// Open verifies catalog membership and opens an artifact for streaming.
func (catalog *Catalog) Open(thread *workspace.Thread, virtualPath string) (io.ReadCloser, Artifact, error) {
	if catalog == nil || thread == nil {
		return nil, Artifact{}, fmt.Errorf("%w: catalog and workspace are required", ErrInvalidArtifact)
	}
	if thread.IsInternalOutputPath(virtualPath) {
		return nil, Artifact{}, fmt.Errorf("%w: internal output", ErrInvalidArtifact)
	}
	catalog.mu.RLock()
	artifact, exists := catalog.entries[thread.ID()][virtualPath]
	catalog.mu.RUnlock()
	if !exists {
		return nil, Artifact{}, fmt.Errorf("%w: %s", ErrNotFound, virtualPath)
	}
	reader, err := thread.OpenFile(virtualPath)
	if err != nil {
		return nil, Artifact{}, err
	}
	return reader, artifact, nil
}

// Inspect returns one generated or uploaded file without catalog membership.
func Inspect(thread *workspace.Thread, virtualPath string) (Artifact, error) {
	if thread == nil || !isPublicPath(virtualPath) || thread.IsInternalOutputPath(virtualPath) {
		return Artifact{}, ErrInvalidArtifact
	}
	entry, err := thread.Inspect(virtualPath)
	if err != nil {
		return Artifact{}, err
	}
	if entry.Directory {
		return Artifact{}, ErrInvalidArtifact
	}
	return Artifact{ThreadID: thread.ID(), Path: virtualPath, Name: path.Base(virtualPath), MediaType: mediaTypeForPath(virtualPath), Size: entry.Size, ModifiedAt: entry.ModifiedAt}, nil
}

// OpenFile opens one generated or uploaded regular file for HTTP streaming.
func OpenFile(thread *workspace.Thread, virtualPath string) (io.ReadCloser, Artifact, error) {
	metadata, err := Inspect(thread, virtualPath)
	if err != nil {
		return nil, Artifact{}, err
	}
	reader, err := thread.OpenFile(virtualPath)
	return reader, metadata, err
}

func isOutputPath(virtualPath string) bool {
	return strings.HasPrefix(virtualPath, workspace.OutputsRoot+"/") &&
		path.Clean(virtualPath) == virtualPath
}

func isPublicPath(virtualPath string) bool {
	return (strings.HasPrefix(virtualPath, workspace.OutputsRoot+"/") || strings.HasPrefix(virtualPath, workspace.UploadsRoot+"/")) && path.Clean(virtualPath) == virtualPath
}

func mediaTypeForPath(virtualPath string) string {
	switch strings.ToLower(path.Ext(virtualPath)) {
	case ".md", ".markdown":
		return "text/markdown; charset=utf-8"
	case ".txt", ".log", ".csv", ".tsv":
		return "text/plain; charset=utf-8"
	case ".json":
		return "application/json"
	case ".yaml", ".yml":
		return "application/yaml"
	default:
		mediaType := mime.TypeByExtension(path.Ext(virtualPath))
		if mediaType == "" {
			return "application/octet-stream"
		}
		return mediaType
	}
}
