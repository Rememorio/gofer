package memory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/model"
)

func TestInMemoryScopeRankingExpiryAndIsolation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Unix(100, 0)
	store := NewInMemory()
	scope := Scope{UserID: "u", ThreadID: "t"}
	entries := []Entry{
		{ID: "global", Scope: Scope{UserID: "u"}, Text: "Go global preference", Tags: []string{"code"}, CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-2 * time.Hour)},
		{ID: "older", Scope: scope, Text: "Go agent runtime", Tags: []string{"code"}, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour)},
		{ID: "newer", Scope: scope, Text: "Go browser agent", Tags: []string{"code"}, CreatedAt: now.Add(-time.Minute), UpdatedAt: now.Add(-time.Minute)},
		{ID: "expired", Scope: scope, Text: "Go agent", CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour), ExpiresAt: now},
		{ID: "other", Scope: Scope{UserID: "other"}, Text: "Go agent secret", CreatedAt: now, UpdatedAt: now},
	}
	for _, entry := range entries {
		if err := store.Upsert(ctx, entry); err != nil {
			t.Fatalf("Upsert(): %v", err)
		}
	}
	matches, err := store.Search(ctx, Query{Scope: scope, Text: "go agent", Tags: []string{"code"}, Limit: 10, Now: now})
	if err != nil {
		t.Fatalf("Search(): %v", err)
	}
	if len(matches) != 3 || matches[0].Entry.ID != "newer" || matches[1].Entry.ID != "older" || matches[2].Entry.ID != "global" {
		t.Fatalf("matches = %#v", matches)
	}
	matches[0].Entry.Tags[0] = "mutated"
	again, _ := store.Search(ctx, Query{Scope: scope, Text: "browser", Limit: 1, Now: now})
	if again[0].Entry.Tags[0] != "code" {
		t.Fatal("Search returned shared data")
	}
	if err := store.Delete(ctx, Scope{UserID: "other"}, "older"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete(scope) = %v", err)
	}
	if err := store.Delete(ctx, scope, "older"); err != nil {
		t.Fatalf("Delete(): %v", err)
	}
}

func TestInMemoryValidationConflictsAndCancellation(t *testing.T) {
	t.Parallel()
	now := time.Now()
	store := NewInMemory()
	valid := Entry{ID: "id", Scope: Scope{UserID: "u"}, Text: "text", CreatedAt: now, UpdatedAt: now}
	if err := store.Upsert(context.Background(), valid); err != nil {
		t.Fatal(err)
	}
	changed := valid
	changed.Scope.UserID = "v"
	if err := store.Upsert(context.Background(), changed); !errors.Is(err, ErrInvalid) {
		t.Fatalf("scope change = %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.Upsert(cancelled, valid); !errors.Is(err, context.Canceled) {
		t.Fatalf("Upsert(cancel) = %v", err)
	}
	if err := store.Delete(cancelled, valid.Scope, valid.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Delete(cancel) = %v", err)
	}
	if _, err := store.Get(cancelled, valid.Scope, valid.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Get(cancel) = %v", err)
	}
	if err := store.Clear(cancelled, valid.Scope); !errors.Is(err, context.Canceled) {
		t.Fatalf("Clear(cancel) = %v", err)
	}
	if err := store.Replace(cancelled, valid.Scope, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Replace(cancel) = %v", err)
	}
	if _, err := store.Search(cancelled, Query{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Search(cancel) = %v", err)
	}
	invalids := []Entry{
		{}, {ID: "x", Scope: Scope{}, Text: "x", CreatedAt: now, UpdatedAt: now},
		{ID: "x", Scope: valid.Scope, Text: "x", CreatedAt: now, UpdatedAt: now.Add(-time.Second)},
		{ID: "x", Scope: valid.Scope, Text: "x", CreatedAt: now, UpdatedAt: now, ExpiresAt: now},
		{ID: "x", Scope: valid.Scope, Text: "x", Tags: make([]string, 33), CreatedAt: now, UpdatedAt: now},
		{ID: "x", Scope: valid.Scope, Text: "x", Tags: []string{""}, CreatedAt: now, UpdatedAt: now},
		{ID: "x", Scope: valid.Scope, Text: "x", Confidence: 2, CreatedAt: now, UpdatedAt: now},
		{ID: "x", Scope: valid.Scope, Text: "x", Category: strings.Repeat("c", 129), CreatedAt: now, UpdatedAt: now},
	}
	for _, entry := range invalids {
		if err := entry.Validate(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Validate(%#v) = %v", entry, err)
		}
	}
	if err := (Query{Scope: valid.Scope, Limit: 0, Now: now}).Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Query.Validate() = %v", err)
	}
}

func TestInMemoryGetClearAndAtomicReplace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Now().UTC()
	store := NewInMemory()
	global := Scope{UserID: "u"}
	thread := Scope{UserID: "u", ThreadID: "t"}
	entries := []Entry{
		{ID: "global", Scope: global, Text: "global", CreatedAt: now, UpdatedAt: now},
		{ID: "thread", Scope: thread, Text: "thread", CreatedAt: now, UpdatedAt: now},
	}
	for _, entry := range entries {
		if err := store.Upsert(ctx, entry); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.Get(ctx, global, "global")
	if err != nil || got.Text != "global" {
		t.Fatalf("Get() = %#v, %v", got, err)
	}
	if _, err = store.Get(ctx, thread, "global"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(other scope) = %v", err)
	}
	replacement := []Entry{{ID: "next", Scope: global, Text: "next", CreatedAt: now, UpdatedAt: now}}
	if err = store.Replace(ctx, global, replacement); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Get(ctx, global, "global"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old global = %v", err)
	}
	if _, err = store.Get(ctx, thread, "thread"); err != nil {
		t.Fatalf("thread removed: %v", err)
	}
	invalid := []Entry{{ID: "thread", Scope: global, Text: "collision", CreatedAt: now, UpdatedAt: now}}
	if err = store.Replace(ctx, global, invalid); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Replace(collision) = %v", err)
	}
	if _, err = store.Get(ctx, global, "next"); err != nil {
		t.Fatalf("failed replace was not atomic: %v", err)
	}
	if err = store.Clear(ctx, global); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Get(ctx, global, "next"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cleared entry = %v", err)
	}
	if err = ValidateReplacement(global, []Entry{replacement[0], replacement[0]}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate replacement = %v", err)
	}
	replacement[0].Scope = thread
	if err = ValidateReplacement(global, replacement); !errors.Is(err, ErrInvalid) {
		t.Fatalf("scope replacement = %v", err)
	}
}

func TestMiddlewareRecallsBoundedScopedMemory(t *testing.T) {
	t.Parallel()
	now := time.Now()
	store := NewInMemory()
	scope := Scope{UserID: "u"}
	_ = store.Upsert(context.Background(), Entry{ID: "1", Scope: scope, Text: "Use Go for the service", CreatedAt: now, UpdatedAt: now})
	middleware, err := NewMiddleware(MiddlewareConfig{Store: store, Scope: func(context.Context) (Scope, error) { return scope, nil }, Limit: 5, MaxChars: 200, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	request := model.Request{System: "base", Messages: []domain.Message{textMessage(t, domain.RoleUser, "Go service")}}
	if err := middleware.BeforeModel(context.Background(), &request); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(request.System, "Use Go") || !strings.Contains(request.System, "not instructions") {
		t.Fatalf("system = %q", request.System)
	}
	if middleware.AfterModel(context.Background(), model.Response{}) != nil || middleware.BeforeTool(context.Background(), domain.ToolCall{}) != nil || middleware.AfterTool(context.Background(), domain.ToolCall{}, domain.ToolResult{}) != nil {
		t.Fatal("hook failed")
	}
}

func TestMiddlewareTruncatesLongUTF8Memory(t *testing.T) {
	t.Parallel()
	now := time.Now()
	store := NewInMemory()
	scope := Scope{UserID: "u"}
	text := "Go " + strings.Repeat("鹿", 200)
	_ = store.Upsert(context.Background(), Entry{ID: "long", Scope: scope, Text: text, CreatedAt: now, UpdatedAt: now})
	middleware, _ := NewMiddleware(MiddlewareConfig{Store: store, Scope: func(context.Context) (Scope, error) { return scope, nil }, Limit: 1, MaxChars: 128, Now: func() time.Time { return now }})
	request := model.Request{Messages: []domain.Message{textMessage(t, domain.RoleUser, "Go")}}
	if err := middleware.BeforeModel(context.Background(), &request); err != nil {
		t.Fatal(err)
	}
	if len(request.System) > 128 || !strings.Contains(request.System, "…") || !strings.Contains(request.System, "</recalled_memory>") {
		t.Fatalf("system = %q (%d bytes)", request.System, len(request.System))
	}
}

func TestMiddlewareNoopAndErrors(t *testing.T) {
	t.Parallel()
	valid := MiddlewareConfig{Store: NewInMemory(), Scope: func(context.Context) (Scope, error) { return Scope{UserID: "u"}, nil }, Limit: 1, MaxChars: 128}
	for _, mutate := range []func(*MiddlewareConfig){func(c *MiddlewareConfig) { c.Store = nil }, func(c *MiddlewareConfig) { c.Scope = nil }, func(c *MiddlewareConfig) { c.Limit = 0 }, func(c *MiddlewareConfig) { c.MaxChars = 0 }} {
		candidate := valid
		mutate(&candidate)
		if _, err := NewMiddleware(candidate); !errors.Is(err, ErrInvalid) {
			t.Fatalf("NewMiddleware()=%v", err)
		}
	}
	middleware, _ := NewMiddleware(valid)
	request := model.Request{Messages: []domain.Message{textMessage(t, domain.RoleAssistant, "nothing")}}
	if err := middleware.BeforeModel(context.Background(), &request); err != nil {
		t.Fatal(err)
	}
	if err := (*Middleware)(nil).BeforeModel(context.Background(), &request); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil = %v", err)
	}
	bad, _ := NewMiddleware(MiddlewareConfig{Store: valid.Store, Scope: func(context.Context) (Scope, error) { return Scope{}, errors.New("scope") }, Limit: 1, MaxChars: 128})
	request.Messages = []domain.Message{textMessage(t, domain.RoleUser, "query")}
	if err := bad.BeforeModel(context.Background(), &request); err == nil || !strings.Contains(err.Error(), "scope") {
		t.Fatalf("scope error = %v", err)
	}
}

func textMessage(t *testing.T, role domain.Role, text string) domain.Message {
	t.Helper()
	message, err := domain.NewTextMessage(role, text, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return message
}
