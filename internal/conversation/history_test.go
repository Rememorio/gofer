package conversation

import (
	"context"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/event"
	"github.com/Rememorio/gofer/internal/store"
)

func TestPersistLoadAndMergeConversation(t *testing.T) {
	t.Parallel()
	repository := store.NewMemory()
	now := time.Now().UTC()
	thread, _ := domain.NewThread(now)
	if err := repository.CreateThread(context.Background(), thread); err != nil {
		t.Fatal(err)
	}
	firstRun, _ := domain.NewRun(thread.ID, now)
	if err := repository.CreateRun(context.Background(), firstRun); err != nil {
		t.Fatal(err)
	}
	created, _ := event.NewDraft(thread.ID, firstRun.ID, event.RunCreated, now, nil)
	if _, err := repository.Append(context.Background(), firstRun.ID, 0, created); err != nil {
		t.Fatal(err)
	}
	user, _ := domain.NewTextMessage(domain.RoleUser, "hello", now.Add(time.Second))
	if err := PersistInputs(context.Background(), repository, thread.ID, firstRun.ID, []domain.Message{user}); err != nil {
		t.Fatal(err)
	}
	assistant, _ := domain.NewTextMessage(domain.RoleAssistant, "hi", now.Add(2*time.Second))
	completed, _ := event.NewDraft(thread.ID, firstRun.ID, event.MessageCompleted, assistant.CreatedAt, map[string]any{"message": assistant})
	if _, err := repository.Append(context.Background(), firstRun.ID, 2, completed); err != nil {
		t.Fatal(err)
	}

	history, err := Load(context.Background(), repository, thread.ID)
	if err != nil || len(history) != 2 || history[0].ID != user.ID || history[1].ID != assistant.ID {
		t.Fatalf("Load() = %#v, %v", history, err)
	}
	runMessages, err := LoadRun(context.Background(), repository, firstRun.ID)
	if err != nil || len(runMessages) != 2 {
		t.Fatalf("LoadRun() = %#v, %v", runMessages, err)
	}
	next, _ := domain.NewTextMessage(domain.RoleUser, "again", now.Add(3*time.Second))
	combined, additions := Merge(history, []domain.Message{user, assistant, next})
	if len(combined) != 3 || len(additions) != 1 || additions[0].ID != next.ID {
		t.Fatalf("Merge() = %#v, %#v", combined, additions)
	}
	combined, additions = Merge(history, []domain.Message{next})
	if len(combined) != 3 || len(additions) != 1 {
		t.Fatalf("Merge(new only) = %#v, %#v", combined, additions)
	}
}

func TestFromEventsSkipsNonMessagesAndMalformedPayloads(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	thread, _ := domain.NewThread(now)
	run, _ := domain.NewRun(thread.ID, now)
	nonMessage, _ := event.NewDraft(thread.ID, run.ID, event.RunStarted, now, nil)
	malformed, _ := event.NewDraft(thread.ID, run.ID, event.MessageCompleted, now, map[string]string{"message": "bad"})
	first, _ := nonMessage.Commit(1)
	second, _ := malformed.Commit(2)
	if messages := FromEvents([]event.Event{first, second}); len(messages) != 0 {
		t.Fatalf("FromEvents() = %#v", messages)
	}
	if err := PersistInputs(context.Background(), store.NewMemory(), thread.ID, run.ID, nil); err != nil {
		t.Fatal(err)
	}
}

func TestSeedCreatesTerminalBranchHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repository := store.NewMemory()
	now := time.Now().UTC()
	thread, _ := domain.NewThread(now)
	if err := repository.CreateThread(ctx, thread); err != nil {
		t.Fatal(err)
	}
	user, _ := domain.NewTextMessage(domain.RoleUser, "question", now.Add(-time.Hour))
	assistant, _ := domain.NewTextMessage(domain.RoleAssistant, "answer", now.Add(-time.Hour+time.Second))
	runID, err := Seed(ctx, repository, thread.ID, []domain.Message{user, assistant}, now)
	if err != nil {
		t.Fatal(err)
	}
	run, err := repository.Run(ctx, runID)
	if err != nil || !run.Terminal() || run.Status != domain.RunSucceeded {
		t.Fatalf("run = %#v, %v", run, err)
	}
	messages, err := Load(ctx, repository, thread.ID)
	if err != nil || len(messages) != 2 || messages[1].ID != assistant.ID {
		t.Fatalf("messages = %#v, %v", messages, err)
	}
	records, _ := repository.Events(ctx, runID, 0, 0)
	if len(records) != 5 || records[len(records)-1].Kind != event.RunCompleted {
		t.Fatalf("events = %#v", records)
	}
	if _, err = Seed(ctx, repository, thread.ID, []domain.Message{{}}, now); err == nil {
		t.Fatal("invalid seed succeeded")
	}
}
