package event

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
)

var (
	// ErrInvalidDraft identifies an event draft that cannot be persisted.
	ErrInvalidDraft = errors.New("invalid event draft")
	// ErrInvalidEvent identifies a malformed committed journal event.
	ErrInvalidEvent = errors.New("invalid event")
)

// Kind identifies a stable event schema in the run journal.
type Kind string

// Stable run-journal event kinds.
const (
	RunCreated       Kind = "run.created"
	RunStarted       Kind = "run.started"
	RunInterrupted   Kind = "run.interrupted"
	RunCompleted     Kind = "run.completed"
	RunFailed        Kind = "run.failed"
	RunCancelled     Kind = "run.cancelled"
	MessageStarted   Kind = "message.started"
	MessageDelta     Kind = "message.delta"
	MessageCompleted Kind = "message.completed"
	ModelUsage       Kind = "model.usage"
	ModelRetry       Kind = "model.retry"
	WorkspaceChanges Kind = "workspace_changes"
	RunDelivery      Kind = "run.delivery"
	ToolStarted      Kind = "tool.started"
	ToolCompleted    Kind = "tool.completed"
	ToolFailed       Kind = "tool.failed"
	CheckpointSaved  Kind = "checkpoint.saved"
)

// Draft is an immutable event before the journal assigns its sequence.
type Draft struct {
	ID       domain.EventID  `json:"event_id"`
	ThreadID domain.ThreadID `json:"thread_id"`
	RunID    domain.RunID    `json:"run_id"`
	Kind     Kind            `json:"type"`
	Time     time.Time       `json:"timestamp"`
	Data     json.RawMessage `json:"data"`
}

// Event is a committed event at one durable, one-based sequence number.
type Event struct {
	Sequence uint64          `json:"sequence"`
	ID       domain.EventID  `json:"event_id"`
	ThreadID domain.ThreadID `json:"thread_id"`
	RunID    domain.RunID    `json:"run_id"`
	Kind     Kind            `json:"type"`
	Time     time.Time       `json:"timestamp"`
	Data     json.RawMessage `json:"data"`
}

// NewDraft serializes payload and creates a validated event draft.
func NewDraft(
	threadID domain.ThreadID,
	runID domain.RunID,
	kind Kind,
	at time.Time,
	payload any,
) (Draft, error) {
	id, err := domain.NewEventID()
	if err != nil {
		return Draft{}, err
	}
	data, err := marshalPayload(payload)
	if err != nil {
		return Draft{}, fmt.Errorf("%w: encode %s payload: %w", ErrInvalidDraft, kind, err)
	}
	draft := Draft{ID: id, ThreadID: threadID, RunID: runID, Kind: kind, Time: at, Data: data}
	if err := draft.Validate(); err != nil {
		return Draft{}, err
	}
	return draft, nil
}

// Validate verifies the event draft contract.
func (draft Draft) Validate() error {
	if _, err := domain.ParseEventID(string(draft.ID)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidDraft, err)
	}
	if _, err := domain.ParseThreadID(string(draft.ThreadID)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidDraft, err)
	}
	if _, err := domain.ParseRunID(string(draft.RunID)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidDraft, err)
	}
	if !draft.Kind.valid() {
		return fmt.Errorf("%w: unsupported kind %q", ErrInvalidDraft, draft.Kind)
	}
	if draft.Time.IsZero() {
		return fmt.Errorf("%w: timestamp is required", ErrInvalidDraft)
	}
	if len(draft.Data) == 0 || !json.Valid(draft.Data) {
		return fmt.Errorf("%w: data must be valid JSON", ErrInvalidDraft)
	}
	return nil
}

// Commit returns a committed event without modifying draft.
func (draft Draft) Commit(sequence uint64) (Event, error) {
	if err := draft.Validate(); err != nil {
		return Event{}, err
	}
	if sequence == 0 {
		return Event{}, fmt.Errorf("%w: sequence must be positive", ErrInvalidEvent)
	}
	return Event{
		Sequence: sequence,
		ID:       draft.ID,
		ThreadID: draft.ThreadID,
		RunID:    draft.RunID,
		Kind:     draft.Kind,
		Time:     draft.Time,
		Data:     append(json.RawMessage(nil), draft.Data...),
	}, nil
}

// Validate verifies the committed event contract.
func (event Event) Validate() error {
	if event.Sequence == 0 {
		return fmt.Errorf("%w: sequence must be positive", ErrInvalidEvent)
	}
	draft := Draft{
		ID:       event.ID,
		ThreadID: event.ThreadID,
		RunID:    event.RunID,
		Kind:     event.Kind,
		Time:     event.Time,
		Data:     event.Data,
	}
	if err := draft.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidEvent, err)
	}
	return nil
}

// Decode unmarshals an event payload into target.
func Decode(event Event, target any) error {
	if err := event.Validate(); err != nil {
		return err
	}
	if err := json.Unmarshal(event.Data, target); err != nil {
		return fmt.Errorf("decode %s payload: %w", event.Kind, err)
	}
	return nil
}

func marshalPayload(payload any) (json.RawMessage, error) {
	if payload == nil {
		return json.RawMessage(`{}`), nil
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (kind Kind) valid() bool {
	switch kind {
	case RunCreated, RunStarted, RunInterrupted, RunCompleted, RunFailed, RunCancelled,
		MessageStarted, MessageDelta, MessageCompleted, ModelUsage, ModelRetry, WorkspaceChanges, RunDelivery,
		ToolStarted, ToolCompleted, ToolFailed, CheckpointSaved:
		return true
	default:
		return false
	}
}
