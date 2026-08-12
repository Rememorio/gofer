package memory

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

var (
	// ErrInvalid identifies a malformed memory entry, scope, or query.
	ErrInvalid = errors.New("invalid memory")
	// ErrNotFound identifies an unknown memory in the requested scope.
	ErrNotFound = errors.New("memory not found")
)

// Scope prevents memories from leaking between users, threads, or agents.
type Scope struct {
	UserID   string `json:"user_id"`
	ThreadID string `json:"thread_id,omitempty"`
	AgentID  string `json:"agent_id,omitempty"`
}

// Entry is one durable long-term memory.
type Entry struct {
	ID        string    `json:"id"`
	Scope     Scope     `json:"scope"`
	Text      string    `json:"text"`
	Tags      []string  `json:"tags,omitempty"`
	Source    string    `json:"source,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

// Query controls scoped lexical retrieval.
type Query struct {
	Scope Scope
	Text  string
	Tags  []string
	Limit int
	Now   time.Time
}

// Match is a ranked isolated copy of a memory entry.
type Match struct {
	Entry Entry `json:"entry"`
	Score int   `json:"score"`
}

// Store is the replaceable durable memory contract.
type Store interface {
	Upsert(context.Context, Entry) error
	Delete(context.Context, Scope, string) error
	Search(context.Context, Query) ([]Match, error)
}

// InMemory is the concurrency-safe reference Store.
type InMemory struct {
	mu      sync.RWMutex
	entries map[string]Entry
}

// NewInMemory constructs an empty memory store.
func NewInMemory() *InMemory { return &InMemory{entries: make(map[string]Entry)} }

// Upsert creates or replaces an entry without permitting scope changes.
func (store *InMemory) Upsert(ctx context.Context, entry Entry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := entry.Validate(); err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if current, exists := store.entries[entry.ID]; exists && current.Scope != entry.Scope {
		return fmt.Errorf("%w: scope is immutable", ErrInvalid)
	}
	store.entries[entry.ID] = cloneEntry(entry)
	return nil
}

// Delete removes an entry only when its complete scope matches.
func (store *InMemory) Delete(ctx context.Context, scope Scope, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := scope.Validate(); err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	entry, exists := store.entries[id]
	if !exists || entry.Scope != scope {
		return ErrNotFound
	}
	delete(store.entries, id)
	return nil
}

// Search returns stable score, recency, then identifier order.
func (store *InMemory) Search(ctx context.Context, query Query) ([]Match, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := query.Validate(); err != nil {
		return nil, err
	}
	terms := words(query.Text)
	tags := normalizedSet(query.Tags)
	store.mu.RLock()
	matches := make([]Match, 0)
	for _, entry := range store.entries {
		if !scopeIncludes(query.Scope, entry.Scope) || (!entry.ExpiresAt.IsZero() && !entry.ExpiresAt.After(query.Now)) {
			continue
		}
		score := scoreEntry(entry, terms, tags)
		if len(terms) != 0 || len(tags) != 0 {
			if score == 0 {
				continue
			}
		}
		matches = append(matches, Match{Entry: cloneEntry(entry), Score: score})
	}
	store.mu.RUnlock()
	sort.Slice(matches, func(left, right int) bool {
		if matches[left].Score != matches[right].Score {
			return matches[left].Score > matches[right].Score
		}
		if !matches[left].Entry.UpdatedAt.Equal(matches[right].Entry.UpdatedAt) {
			return matches[left].Entry.UpdatedAt.After(matches[right].Entry.UpdatedAt)
		}
		return matches[left].Entry.ID < matches[right].Entry.ID
	})
	if len(matches) > query.Limit {
		matches = matches[:query.Limit]
	}
	return matches, nil
}

// Validate verifies a complete memory entry.
func (entry Entry) Validate() error {
	if strings.TrimSpace(entry.ID) == "" || len(entry.ID) > 128 || strings.TrimSpace(entry.Text) == "" || len(entry.Text) > 64<<10 {
		return fmt.Errorf("%w: id and bounded text are required", ErrInvalid)
	}
	if err := entry.Scope.Validate(); err != nil {
		return err
	}
	if entry.CreatedAt.IsZero() || entry.UpdatedAt.Before(entry.CreatedAt) {
		return fmt.Errorf("%w: invalid timestamps", ErrInvalid)
	}
	if !entry.ExpiresAt.IsZero() && !entry.ExpiresAt.After(entry.CreatedAt) {
		return fmt.Errorf("%w: expiry must follow creation", ErrInvalid)
	}
	if len(entry.Tags) > 32 {
		return fmt.Errorf("%w: too many tags", ErrInvalid)
	}
	for _, tag := range entry.Tags {
		if strings.TrimSpace(tag) == "" || len(tag) > 64 {
			return fmt.Errorf("%w: invalid tag", ErrInvalid)
		}
	}
	return nil
}

// Validate verifies a tenant scope. Thread and agent are optional refinements.
func (scope Scope) Validate() error {
	if strings.TrimSpace(scope.UserID) == "" || len(scope.UserID) > 256 || len(scope.ThreadID) > 256 || len(scope.AgentID) > 256 {
		return fmt.Errorf("%w: valid user scope is required", ErrInvalid)
	}
	return nil
}

// Validate verifies a bounded retrieval request.
func (query Query) Validate() error {
	if err := query.Scope.Validate(); err != nil {
		return err
	}
	if query.Limit < 1 || query.Limit > 100 || len(query.Text) > 16<<10 || query.Now.IsZero() {
		return fmt.Errorf("%w: invalid search limits", ErrInvalid)
	}
	return nil
}

func scoreEntry(entry Entry, terms, tags map[string]struct{}) int {
	text := words(entry.Text)
	entryTags := normalizedSet(entry.Tags)
	score := 0
	for term := range terms {
		if _, exists := text[term]; exists {
			score += 2
		}
	}
	for tag := range tags {
		if _, exists := entryTags[tag]; exists {
			score += 3
		}
	}
	return score
}

func scopeIncludes(query, entry Scope) bool {
	if query.UserID != entry.UserID {
		return false
	}
	if entry.ThreadID != "" && entry.ThreadID != query.ThreadID {
		return false
	}
	return entry.AgentID == "" || entry.AgentID == query.AgentID
}

func words(value string) map[string]struct{} {
	return normalizedSet(strings.FieldsFunc(value, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) }))
}

func normalizedSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "" {
			set[value] = struct{}{}
		}
	}
	return set
}

func cloneEntry(entry Entry) Entry { entry.Tags = append([]string(nil), entry.Tags...); return entry }
