package runtime

import (
	"context"

	"github.com/Rememorio/gofer/internal/event"
)

// EventWriter appends a non-terminal event through the active run journal.
// Finish hooks use it so their event remains ordered before the terminal event.
type EventWriter interface {
	Append(context.Context, event.Kind, any) error
}

// FinishHook runs exactly once after agent work stops and before the durable
// terminal transition and event. Hooks should keep cleanup and failure paths
// bounded by the supplied context.
type FinishHook interface {
	Finish(context.Context, EventWriter) error
}

// FinishFunc adapts a function to FinishHook.
type FinishFunc func(context.Context, EventWriter) error

// Finish invokes function.
func (function FinishFunc) Finish(ctx context.Context, writer EventWriter) error {
	return function(ctx, writer)
}

type eventWriterFunc func(context.Context, event.Kind, any) error

func (function eventWriterFunc) Append(ctx context.Context, kind event.Kind, payload any) error {
	return function(ctx, kind, payload)
}
