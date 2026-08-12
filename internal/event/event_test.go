package event

import (
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
)

func TestDraftCommitAndDecode(t *testing.T) {
	t.Parallel()

	threadID, runID := testIDs(t)
	type payload struct {
		Text string `json:"text"`
	}
	draft, err := NewDraft(threadID, runID, MessageDelta, time.Now(), payload{Text: "hello"})
	if err != nil {
		t.Fatalf("NewDraft(): %v", err)
	}
	event, err := draft.Commit(1)
	if err != nil {
		t.Fatalf("Commit(): %v", err)
	}
	if err := event.Validate(); err != nil {
		t.Fatalf("Validate(): %v", err)
	}
	var got payload
	if err := Decode(event, &got); err != nil {
		t.Fatalf("Decode(): %v", err)
	}
	if got.Text != "hello" {
		t.Fatalf("decoded text = %q, want hello", got.Text)
	}

	draft.Data[0] = '['
	if !json.Valid(event.Data) {
		t.Fatal("committed event shares mutable payload storage with draft")
	}
}

func TestNewDraftSupportsEmptyPayload(t *testing.T) {
	t.Parallel()

	threadID, runID := testIDs(t)
	draft, err := NewDraft(threadID, runID, RunStarted, time.Now(), nil)
	if err != nil {
		t.Fatalf("NewDraft(): %v", err)
	}
	if string(draft.Data) != "{}" {
		t.Fatalf("Data = %s, want {}", draft.Data)
	}
}

func TestModelUsageKindIsStable(t *testing.T) {
	t.Parallel()
	threadID, runID := testIDs(t)
	if _, err := NewDraft(threadID, runID, ModelUsage, time.Now(), map[string]int{"input_tokens": 1}); err != nil {
		t.Fatalf("NewDraft(ModelUsage) = %v", err)
	}
}

func TestNewDraftReportsEncodingFailure(t *testing.T) {
	t.Parallel()

	threadID, runID := testIDs(t)
	_, err := NewDraft(threadID, runID, RunStarted, time.Now(), math.Inf(1))
	if !errors.Is(err, ErrInvalidDraft) {
		t.Fatalf("NewDraft() error = %v, want ErrInvalidDraft", err)
	}
}

func TestDraftValidationRejectsMalformedValues(t *testing.T) {
	t.Parallel()

	threadID, runID := testIDs(t)
	valid, err := NewDraft(threadID, runID, RunCreated, time.Now(), nil)
	if err != nil {
		t.Fatalf("NewDraft(): %v", err)
	}
	tests := []func(*Draft){
		func(draft *Draft) { draft.ID = "bad" },
		func(draft *Draft) { draft.ThreadID = "bad" },
		func(draft *Draft) { draft.RunID = "bad" },
		func(draft *Draft) { draft.Kind = "unknown" },
		func(draft *Draft) { draft.Time = time.Time{} },
		func(draft *Draft) { draft.Data = json.RawMessage(`{`) },
	}
	for index, mutate := range tests {
		candidate := valid
		mutate(&candidate)
		if err := candidate.Validate(); !errors.Is(err, ErrInvalidDraft) {
			t.Fatalf("case %d Validate() error = %v, want ErrInvalidDraft", index, err)
		}
	}
}

func TestEventValidationAndDecodeErrors(t *testing.T) {
	t.Parallel()

	threadID, runID := testIDs(t)
	draft, err := NewDraft(threadID, runID, RunCreated, time.Now(), map[string]string{"ok": "yes"})
	if err != nil {
		t.Fatalf("NewDraft(): %v", err)
	}
	if _, err := draft.Commit(0); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("Commit(0) error = %v, want ErrInvalidEvent", err)
	}
	event, err := draft.Commit(1)
	if err != nil {
		t.Fatalf("Commit(1): %v", err)
	}
	event.ID = "bad"
	if err := event.Validate(); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("Validate() error = %v, want ErrInvalidEvent", err)
	}
	if err := Decode(event, &map[string]string{}); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("Decode() error = %v, want ErrInvalidEvent", err)
	}

	event, err = draft.Commit(1)
	if err != nil {
		t.Fatalf("Commit(1): %v", err)
	}
	var wrong struct {
		OK chan int `json:"ok"`
	}
	if err := Decode(event, &wrong); err == nil {
		t.Fatal("Decode() error = nil, want type error")
	}
}

func testIDs(t *testing.T) (domain.ThreadID, domain.RunID) {
	t.Helper()
	thread, err := domain.NewThread(time.Now())
	if err != nil {
		t.Fatalf("NewThread(): %v", err)
	}
	run, err := domain.NewRun(thread.ID, time.Now())
	if err != nil {
		t.Fatalf("NewRun(): %v", err)
	}
	return thread.ID, run.ID
}
