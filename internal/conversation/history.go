// Package conversation reconstructs durable, cross-run message history.
package conversation

import (
	"context"
	"reflect"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/event"
	"github.com/Rememorio/gofer/internal/store"
)

// Load returns validated messages across every run in durable journal order.
func Load(ctx context.Context, repository store.Store, threadID domain.ThreadID) ([]domain.Message, error) {
	runs, err := repository.Runs(ctx, threadID)
	if err != nil {
		return nil, err
	}
	messages := make([]domain.Message, 0)
	seen := make(map[domain.MessageID]struct{})
	for _, run := range runs {
		records, eventsErr := repository.Events(ctx, run.ID, 0, 0)
		if eventsErr != nil {
			return nil, eventsErr
		}
		for _, message := range FromEvents(records) {
			if _, exists := seen[message.ID]; exists {
				continue
			}
			seen[message.ID] = struct{}{}
			messages = append(messages, message)
		}
	}
	return messages, nil
}

// LoadRun returns validated messages recorded by one run.
func LoadRun(ctx context.Context, repository store.Store, runID domain.RunID) ([]domain.Message, error) {
	records, err := repository.Events(ctx, runID, 0, 0)
	if err != nil {
		return nil, err
	}
	return FromEvents(records), nil
}

// FromEvents extracts complete input, assistant, and tool messages.
func FromEvents(records []event.Event) []domain.Message {
	messages := make([]domain.Message, 0)
	for _, record := range records {
		if record.Kind != event.MessageCompleted && record.Kind != event.ToolCompleted && record.Kind != event.ToolFailed {
			continue
		}
		var payload struct {
			Message domain.Message `json:"message"`
		}
		if event.Decode(record, &payload) != nil || payload.Message.Validate() != nil {
			continue
		}
		messages = append(messages, payload.Message)
	}
	return messages
}

// Merge appends only the non-overlapping suffix of incoming to history.
func Merge(history, incoming []domain.Message) (combined, additions []domain.Message) {
	overlap := min(len(history), len(incoming))
	for overlap > 0 && !sameSequence(history[len(history)-overlap:], incoming[:overlap]) {
		overlap--
	}
	additions = append([]domain.Message(nil), incoming[overlap:]...)
	combined = make([]domain.Message, 0, len(history)+len(additions))
	combined = append(combined, history...)
	combined = append(combined, additions...)
	return combined, additions
}

// PersistInputs appends normalized new input messages before agent execution.
func PersistInputs(ctx context.Context, repository store.Store, threadID domain.ThreadID, runID domain.RunID, messages []domain.Message) error {
	if len(messages) == 0 {
		return nil
	}
	records, err := repository.Events(ctx, runID, 0, 0)
	if err != nil {
		return err
	}
	sequence := uint64(0)
	if len(records) > 0 {
		sequence = records[len(records)-1].Sequence
	}
	drafts := make([]event.Draft, len(messages))
	for index, message := range messages {
		drafts[index], err = event.NewDraft(threadID, runID, event.MessageCompleted, message.CreatedAt, map[string]any{"message": message, "input": true})
		if err != nil {
			return err
		}
	}
	_, err = repository.Append(ctx, runID, sequence, drafts...)
	return err
}

func sameSequence(left, right []domain.Message) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Role != right[index].Role || !reflect.DeepEqual(left[index].Content, right[index].Content) {
			return false
		}
	}
	return true
}
