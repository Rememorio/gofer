package store

import (
	"context"
	"errors"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/event"
)

var (
	// ErrNotFound reports that a requested durable object does not exist.
	ErrNotFound = errors.New("not found")
	// ErrExists reports that a durable object already exists.
	ErrExists = errors.New("already exists")
	// ErrConflict reports an optimistic concurrency mismatch.
	ErrConflict = errors.New("version conflict")
)

// Store owns durable thread, run, and ordered journal state.
type Store interface {
	CreateThread(context.Context, domain.Thread) error
	Thread(context.Context, domain.ThreadID) (domain.Thread, error)
	CreateRun(context.Context, domain.Run) error
	Run(context.Context, domain.RunID) (domain.Run, error)
	TransitionRun(context.Context, domain.RunID, domain.RunStatus, domain.RunStatus, time.Time, string) (domain.Run, error)
	Append(context.Context, domain.RunID, uint64, ...event.Draft) ([]event.Event, error)
	Events(context.Context, domain.RunID, uint64, int) ([]event.Event, error)
	Watch(context.Context, domain.RunID, uint64) (<-chan uint64, error)
}
