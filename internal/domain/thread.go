package domain

import (
	"errors"
	"fmt"
	"time"
)

// ErrInvalidThread identifies malformed durable thread state.
var ErrInvalidThread = errors.New("invalid thread")

// Thread groups messages and runs within one user-visible conversation.
type Thread struct {
	ID        ThreadID          `json:"thread_id"`
	Title     string            `json:"title,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

// NewThread constructs a new thread at the supplied time.
func NewThread(at time.Time) (Thread, error) {
	id, err := NewThreadID()
	if err != nil {
		return Thread{}, err
	}
	return Thread{ID: id, CreatedAt: at, UpdatedAt: at}, nil
}

// Validate verifies the durable thread contract.
func (thread Thread) Validate() error {
	if _, err := ParseThreadID(string(thread.ID)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidThread, err)
	}
	if thread.CreatedAt.IsZero() || thread.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: timestamps are required", ErrInvalidThread)
	}
	if thread.UpdatedAt.Before(thread.CreatedAt) {
		return fmt.Errorf("%w: updated_at precedes created_at", ErrInvalidThread)
	}
	return nil
}
