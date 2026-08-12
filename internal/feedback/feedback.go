package feedback

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
)

const (
	maxUserIDBytes  = 256
	maxCommentBytes = 16 << 10
	maxListLimit    = 100
)

var (
	// ErrInvalid identifies malformed feedback or query input.
	ErrInvalid = errors.New("invalid feedback")
	// ErrNotFound identifies feedback outside the requested scope.
	ErrNotFound = errors.New("feedback not found")
	// ErrExists identifies a duplicate feedback identifier.
	ErrExists = errors.New("feedback already exists")
)

// Entry is one positive or negative rating for a durable run.
type Entry struct {
	ID        domain.FeedbackID `json:"feedback_id"`
	RunID     domain.RunID      `json:"run_id"`
	ThreadID  domain.ThreadID   `json:"thread_id"`
	UserID    string            `json:"user_id,omitempty"`
	MessageID string            `json:"message_id,omitempty"`
	Rating    int               `json:"rating"`
	Comment   string            `json:"comment,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
}

// Stats summarizes ratings across all users for one run.
type Stats struct {
	RunID    domain.RunID `json:"run_id"`
	Total    int          `json:"total"`
	Positive int          `json:"positive"`
	Negative int          `json:"negative"`
}

// RunScope identifies one run within its parent thread.
type RunScope struct {
	ThreadID domain.ThreadID
	RunID    domain.RunID
}

// Validate checks both durable identifiers.
func (scope RunScope) Validate() error {
	if _, err := domain.ParseThreadID(string(scope.ThreadID)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if _, err := domain.ParseRunID(string(scope.RunID)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return nil
}

// Query selects one user's bounded feedback list for a run.
type Query struct {
	RunScope
	UserID string
	Limit  int
}

// Normalize validates the query and applies its default limit.
func (query Query) Normalize() (Query, error) {
	if err := query.RunScope.Validate(); err != nil {
		return Query{}, err
	}
	query.UserID = strings.TrimSpace(query.UserID)
	if query.UserID == "" || len(query.UserID) > maxUserIDBytes || query.Limit < 0 || query.Limit > maxListLimit {
		return Query{}, ErrInvalid
	}
	if query.Limit == 0 {
		query.Limit = maxListLimit
	}
	return query, nil
}

// Lookup selects one feedback record within its user boundary.
type Lookup struct {
	ID     domain.FeedbackID
	UserID string
}

// Validate checks the feedback identifier and user boundary.
func (lookup Lookup) Validate() error {
	if _, err := domain.ParseFeedbackID(string(lookup.ID)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	lookup.UserID = strings.TrimSpace(lookup.UserID)
	if lookup.UserID == "" || len(lookup.UserID) > maxUserIDBytes {
		return ErrInvalid
	}
	return nil
}

// Store is the durable feedback persistence contract.
type Store interface {
	Create(context.Context, Entry) error
	Upsert(context.Context, domain.ThreadID, domain.RunID, string, int, string, time.Time) (Entry, error)
	Get(context.Context, domain.FeedbackID, string) (Entry, error)
	ListRun(context.Context, domain.ThreadID, domain.RunID, string, int) ([]Entry, error)
	Stats(context.Context, domain.ThreadID, domain.RunID) (Stats, error)
	Delete(context.Context, domain.FeedbackID, string) error
	DeleteRunUser(context.Context, domain.ThreadID, domain.RunID, string) error
	DeleteThread(context.Context, domain.ThreadID) error
}

// NewEntry constructs validated feedback.
func NewEntry(threadID domain.ThreadID, runID domain.RunID, userID string, rating int, messageID, comment string, at time.Time) (Entry, error) {
	id, err := domain.NewFeedbackID()
	if err != nil {
		return Entry{}, err
	}
	entry := Entry{ID: id, ThreadID: threadID, RunID: runID, UserID: strings.TrimSpace(userID), Rating: rating, MessageID: strings.TrimSpace(messageID), Comment: strings.TrimSpace(comment), CreatedAt: at}
	if err = entry.Validate(); err != nil {
		return Entry{}, err
	}
	return entry, nil
}

// Validate enforces identifiers, ownership, rating, and bounded text.
func (entry Entry) Validate() error {
	if _, err := domain.ParseFeedbackID(string(entry.ID)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if err := (RunScope{ThreadID: entry.ThreadID, RunID: entry.RunID}).Validate(); err != nil {
		return err
	}
	if entry.UserID == "" || len(entry.UserID) > maxUserIDBytes || entry.Rating != 1 && entry.Rating != -1 || len(entry.Comment) > maxCommentBytes || entry.CreatedAt.IsZero() {
		return ErrInvalid
	}
	if entry.MessageID != "" {
		if _, err := domain.ParseMessageID(entry.MessageID); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalid, err)
		}
	}
	return nil
}

// InMemory is the concurrency-safe reference Store.
type InMemory struct {
	mu      sync.RWMutex
	entries map[domain.FeedbackID]Entry
}

// NewInMemory constructs an empty feedback store.
func NewInMemory() *InMemory {
	return &InMemory{entries: make(map[domain.FeedbackID]Entry)}
}

// Create stores a new feedback record.
func (memory *InMemory) Create(ctx context.Context, entry Entry) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := entry.Validate(); err != nil {
		return err
	}
	memory.mu.Lock()
	defer memory.mu.Unlock()
	for id, current := range memory.entries {
		if id == entry.ID || current.ThreadID == entry.ThreadID && current.RunID == entry.RunID && current.UserID == entry.UserID {
			return ErrExists
		}
	}
	memory.entries[entry.ID] = entry
	return nil
}

// Upsert creates or replaces the current user's run-level rating.
func (memory *InMemory) Upsert(ctx context.Context, threadID domain.ThreadID, runID domain.RunID, userID string, rating int, comment string, at time.Time) (Entry, error) {
	if err := contextError(ctx); err != nil {
		return Entry{}, err
	}
	entry, err := NewEntry(threadID, runID, userID, rating, "", comment, at)
	if err != nil {
		return Entry{}, err
	}
	memory.mu.Lock()
	defer memory.mu.Unlock()
	for id, current := range memory.entries {
		if current.ThreadID == threadID && current.RunID == runID && current.UserID == entry.UserID {
			entry.ID = id
			memory.entries[id] = entry
			return entry, nil
		}
	}
	memory.entries[entry.ID] = entry
	return entry, nil
}

// Get returns feedback only to its creating user.
func (memory *InMemory) Get(ctx context.Context, id domain.FeedbackID, userID string) (Entry, error) {
	if err := contextError(ctx); err != nil {
		return Entry{}, err
	}
	if err := (Lookup{ID: id, UserID: userID}).Validate(); err != nil {
		return Entry{}, err
	}
	memory.mu.RLock()
	entry, exists := memory.entries[id]
	memory.mu.RUnlock()
	if !exists || entry.UserID != strings.TrimSpace(userID) {
		return Entry{}, ErrNotFound
	}
	return entry, nil
}

// ListRun returns oldest-first feedback for one user and run.
func (memory *InMemory) ListRun(ctx context.Context, threadID domain.ThreadID, runID domain.RunID, userID string, limit int) ([]Entry, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	query, err := (Query{RunScope: RunScope{ThreadID: threadID, RunID: runID}, UserID: userID, Limit: limit}).Normalize()
	if err != nil {
		return nil, err
	}
	memory.mu.RLock()
	entries := make([]Entry, 0)
	for _, entry := range memory.entries {
		if entry.ThreadID == threadID && entry.RunID == runID && entry.UserID == query.UserID {
			entries = append(entries, entry)
		}
	}
	memory.mu.RUnlock()
	sort.Slice(entries, func(left, right int) bool {
		return entries[left].CreatedAt.Before(entries[right].CreatedAt)
	})
	if len(entries) > query.Limit {
		entries = entries[:query.Limit]
	}
	return entries, nil
}

// Stats counts all ratings for one thread-scoped run.
func (memory *InMemory) Stats(ctx context.Context, threadID domain.ThreadID, runID domain.RunID) (Stats, error) {
	if err := contextError(ctx); err != nil {
		return Stats{}, err
	}
	if err := (RunScope{ThreadID: threadID, RunID: runID}).Validate(); err != nil {
		return Stats{}, err
	}
	stats := Stats{RunID: runID}
	memory.mu.RLock()
	for _, entry := range memory.entries {
		if entry.ThreadID != threadID || entry.RunID != runID {
			continue
		}
		stats.Total++
		if entry.Rating == 1 {
			stats.Positive++
		} else {
			stats.Negative++
		}
	}
	memory.mu.RUnlock()
	return stats, nil
}

// Delete removes one user-owned feedback record.
func (memory *InMemory) Delete(ctx context.Context, id domain.FeedbackID, userID string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := (Lookup{ID: id, UserID: userID}).Validate(); err != nil {
		return err
	}
	memory.mu.Lock()
	defer memory.mu.Unlock()
	entry, exists := memory.entries[id]
	if !exists || entry.UserID != strings.TrimSpace(userID) {
		return ErrNotFound
	}
	delete(memory.entries, id)
	return nil
}

// DeleteRunUser removes every rating the user submitted for one run.
func (memory *InMemory) DeleteRunUser(ctx context.Context, threadID domain.ThreadID, runID domain.RunID, userID string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	query, err := (Query{RunScope: RunScope{ThreadID: threadID, RunID: runID}, UserID: userID, Limit: 1}).Normalize()
	if err != nil {
		return err
	}
	memory.mu.Lock()
	defer memory.mu.Unlock()
	deleted := false
	for id, entry := range memory.entries {
		if entry.ThreadID == threadID && entry.RunID == runID && entry.UserID == query.UserID {
			delete(memory.entries, id)
			deleted = true
		}
	}
	if !deleted {
		return ErrNotFound
	}
	return nil
}

// DeleteThread removes feedback owned by a deleted thread.
func (memory *InMemory) DeleteThread(ctx context.Context, threadID domain.ThreadID) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if _, err := domain.ParseThreadID(string(threadID)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	memory.mu.Lock()
	for id, entry := range memory.entries {
		if entry.ThreadID == threadID {
			delete(memory.entries, id)
		}
	}
	memory.mu.Unlock()
	return nil
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return ErrInvalid
	}
	return ctx.Err()
}
