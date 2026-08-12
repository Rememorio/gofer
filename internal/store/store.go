package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/event"
)

const (
	// OwnerMetadataKey stores the authenticated owner without adding it to public payloads.
	OwnerMetadataKey = "user_id"
	maxThreadLimit   = 200
)

var (
	// ErrNotFound reports that a requested durable object does not exist.
	ErrNotFound = errors.New("not found")
	// ErrExists reports that a durable object already exists.
	ErrExists = errors.New("already exists")
	// ErrConflict reports an optimistic concurrency mismatch.
	ErrConflict = errors.New("version conflict")
	// ErrInvalidQuery reports malformed pagination or mutation input.
	ErrInvalidQuery = errors.New("invalid store query")
)

// ThreadQuery selects an owner-scoped, newest-first page of conversations.
type ThreadQuery struct {
	OwnerID  string
	Text     string
	Metadata map[string]string
	Limit    int
	Offset   int
}

// Normalize validates bounds and applies the default page size.
func (query ThreadQuery) Normalize() (ThreadQuery, error) {
	query.OwnerID = strings.TrimSpace(query.OwnerID)
	query.Text = strings.TrimSpace(query.Text)
	if query.OwnerID == "" || query.Offset < 0 || query.Limit < 0 || query.Limit > maxThreadLimit {
		return ThreadQuery{}, ErrInvalidQuery
	}
	if query.Limit == 0 {
		query.Limit = 50
	}
	if len(query.Metadata) > 50 {
		return ThreadQuery{}, ErrInvalidQuery
	}
	for key, value := range query.Metadata {
		if strings.TrimSpace(key) == "" || key == OwnerMetadataKey || len(key) > 128 || len(value) > 4096 {
			return ThreadQuery{}, ErrInvalidQuery
		}
	}
	return query, nil
}

// ThreadPatch merges user-visible conversation metadata.
type ThreadPatch struct {
	Title    *string
	Metadata map[string]string
}

// Validate rejects an empty or unbounded patch.
func (patch ThreadPatch) Validate() error {
	if patch.Title == nil && patch.Metadata == nil {
		return ErrInvalidQuery
	}
	if patch.Title != nil && len(strings.TrimSpace(*patch.Title)) > 500 || len(patch.Metadata) > 50 {
		return ErrInvalidQuery
	}
	for key, value := range patch.Metadata {
		if strings.TrimSpace(key) == "" || len(key) > 128 || len(value) > 4096 {
			return ErrInvalidQuery
		}
	}
	return nil
}

// ThreadOwnedBy applies the legacy-local ownership rule used during upgrades.
func ThreadOwnedBy(thread domain.Thread, ownerID string) bool {
	stored := strings.TrimSpace(thread.Metadata[OwnerMetadataKey])
	return stored == ownerID || ownerID == "local" && stored == ""
}

// Store owns durable thread, run, and ordered journal state.
type Store interface {
	CreateThread(context.Context, domain.Thread) error
	Thread(context.Context, domain.ThreadID) (domain.Thread, error)
	Threads(context.Context, ThreadQuery) ([]domain.Thread, error)
	PatchThread(context.Context, domain.ThreadID, ThreadPatch, time.Time) (domain.Thread, error)
	SetThreadTitleIfEmpty(context.Context, domain.ThreadID, string, time.Time) (domain.Thread, bool, error)
	DeleteThread(context.Context, domain.ThreadID) error
	CreateRun(context.Context, domain.Run) error
	Run(context.Context, domain.RunID) (domain.Run, error)
	Runs(context.Context, domain.ThreadID) ([]domain.Run, error)
	TransitionRun(context.Context, domain.RunID, domain.RunStatus, domain.RunStatus, time.Time, string) (domain.Run, error)
	Append(context.Context, domain.RunID, uint64, ...event.Draft) ([]event.Event, error)
	Events(context.Context, domain.RunID, uint64, int) ([]event.Event, error)
	Watch(context.Context, domain.RunID, uint64) (<-chan uint64, error)
}
