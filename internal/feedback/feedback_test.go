package feedback

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
)

func TestInMemoryFeedbackLifecycle(t *testing.T) {
	t.Parallel()
	fixture := memoryFeedbackFixture{ctx: context.Background(), memory: NewInMemory(), now: time.Now().UTC()}
	fixture.threadID, _ = domain.NewThreadID()
	fixture.runID, _ = domain.NewRunID()
	alice, bob := seedMemoryFeedback(t, fixture)
	updated := assertMemoryFeedbackReads(t, fixture, alice)
	assertMemoryFeedbackDeletes(t, fixture, updated, bob)
}

type memoryFeedbackFixture struct {
	ctx      context.Context
	memory   *InMemory
	threadID domain.ThreadID
	runID    domain.RunID
	now      time.Time
}

func seedMemoryFeedback(t *testing.T, fixture memoryFeedbackFixture) (Entry, Entry) {
	t.Helper()
	messageID, _ := domain.NewMessageID()
	alice, err := NewEntry(fixture.threadID, fixture.runID, " alice ", 1, string(messageID), " useful ", fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	if alice.UserID != "alice" || alice.Comment != "useful" {
		t.Fatalf("normalized entry = %#v", alice)
	}
	if err = fixture.memory.Create(fixture.ctx, alice); err != nil {
		t.Fatal(err)
	}
	duplicate, _ := NewEntry(fixture.threadID, fixture.runID, "alice", -1, "", "duplicate", fixture.now)
	if err = fixture.memory.Create(fixture.ctx, duplicate); !errors.Is(err, ErrExists) {
		t.Fatalf("Create(duplicate scope) = %v, want ErrExists", err)
	}
	bob, _ := NewEntry(fixture.threadID, fixture.runID, "bob", -1, "", "", fixture.now.Add(time.Second))
	if err = fixture.memory.Create(fixture.ctx, bob); err != nil {
		t.Fatal(err)
	}
	return alice, bob
}

func assertMemoryFeedbackReads(t *testing.T, fixture memoryFeedbackFixture, alice Entry) Entry {
	t.Helper()
	if got, err := fixture.memory.Get(fixture.ctx, alice.ID, "alice"); err != nil || got.ID != alice.ID {
		t.Fatalf("Get() = %#v, %v", got, err)
	}
	entries, err := fixture.memory.ListRun(fixture.ctx, fixture.threadID, fixture.runID, "alice", 0)
	if err != nil || len(entries) != 1 || entries[0].ID != alice.ID {
		t.Fatalf("ListRun() = %#v, %v", entries, err)
	}
	if _, err = fixture.memory.Get(fixture.ctx, alice.ID, "bob"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(other user) = %v", err)
	}
	updated, err := fixture.memory.Upsert(fixture.ctx, fixture.threadID, fixture.runID, "alice", -1, "changed", fixture.now.Add(2*time.Second))
	if err != nil || updated.ID != alice.ID || updated.MessageID != "" || updated.Rating != -1 || updated.Comment != "changed" {
		t.Fatalf("Upsert() = %#v, %v", updated, err)
	}
	stats, err := fixture.memory.Stats(fixture.ctx, fixture.threadID, fixture.runID)
	if err != nil || stats.Total != 2 || stats.Positive != 0 || stats.Negative != 2 {
		t.Fatalf("Stats() = %#v, %v", stats, err)
	}
	return updated
}

func assertMemoryFeedbackDeletes(t *testing.T, fixture memoryFeedbackFixture, updated, bob Entry) {
	t.Helper()
	if err := fixture.memory.Delete(fixture.ctx, updated.ID, "bob"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete(other user) = %v", err)
	}
	if err := fixture.memory.Delete(fixture.ctx, bob.ID, "bob"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.memory.Delete(fixture.ctx, bob.ID, "bob"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete(missing) = %v", err)
	}
	otherRunID, _ := domain.NewRunID()
	carol, err := fixture.memory.Upsert(fixture.ctx, fixture.threadID, otherRunID, "carol", 1, "new", fixture.now)
	if err != nil || carol.ID == "" {
		t.Fatalf("Upsert(new) = %#v, %v", carol, err)
	}
	if err = fixture.memory.DeleteRunUser(fixture.ctx, fixture.threadID, fixture.runID, "alice"); err != nil {
		t.Fatal(err)
	}
	if err = fixture.memory.DeleteRunUser(fixture.ctx, fixture.threadID, fixture.runID, "alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteRunUser(missing) = %v", err)
	}
	if err = fixture.memory.DeleteThread(fixture.ctx, fixture.threadID); err != nil {
		t.Fatal(err)
	}
	stats, _ := fixture.memory.Stats(fixture.ctx, fixture.threadID, otherRunID)
	if stats.Total != 0 {
		t.Fatalf("Stats(after delete) = %#v", stats)
	}
}

func TestFeedbackValidationAndCancellation(t *testing.T) {
	t.Parallel()
	threadID, _ := domain.NewThreadID()
	runID, _ := domain.NewRunID()
	messageID, _ := domain.NewMessageID()
	now := time.Now().UTC()
	valid, _ := NewEntry(threadID, runID, "user", 1, string(messageID), "ok", now)
	for name, mutate := range map[string]func(*Entry){
		"feedback ID": func(entry *Entry) { entry.ID = "bad" },
		"thread ID":   func(entry *Entry) { entry.ThreadID = "bad" },
		"run ID":      func(entry *Entry) { entry.RunID = "bad" },
		"user":        func(entry *Entry) { entry.UserID = "" },
		"rating":      func(entry *Entry) { entry.Rating = 0 },
		"message ID":  func(entry *Entry) { entry.MessageID = "bad" },
		"comment":     func(entry *Entry) { entry.Comment = strings.Repeat("x", maxCommentBytes+1) },
		"timestamp":   func(entry *Entry) { entry.CreatedAt = time.Time{} },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			mutate(&candidate)
			if err := candidate.Validate(); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Validate() = %v, want ErrInvalid", err)
			}
		})
	}
	if _, err := NewEntry(threadID, runID, "user", 2, "", "", now); !errors.Is(err, ErrInvalid) {
		t.Fatalf("NewEntry(invalid) = %v", err)
	}
	if _, err := (Query{RunScope: RunScope{ThreadID: threadID, RunID: runID}, UserID: "user", Limit: maxListLimit + 1}).Normalize(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Query.Normalize() = %v", err)
	}
	if err := (Lookup{ID: valid.ID}).Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Lookup.Validate() = %v", err)
	}
	if err := (RunScope{ThreadID: threadID, RunID: "bad"}).Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("RunScope.Validate() = %v", err)
	}
	memory := NewInMemory()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := memory.ListRun(cancelled, threadID, runID, "user", 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("ListRun(cancelled) = %v", err)
	}
	if _, err := memory.Upsert(cancelled, threadID, runID, "user", 1, "", now); !errors.Is(err, context.Canceled) {
		t.Fatalf("Upsert(cancelled) = %v", err)
	}
	if _, err := memory.Stats(context.Background(), threadID, "bad"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Stats(invalid) = %v", err)
	}
	if err := memory.DeleteThread(context.Background(), "bad"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("DeleteThread(invalid) = %v", err)
	}
}
