package delivery

import (
	"context"
	"errors"
	"time"

	"github.com/Rememorio/gofer/internal/event"
)

var receiptRetryDelays = []time.Duration{100 * time.Millisecond, 500 * time.Millisecond}

// EventWriter is the journal surface required for receipt persistence.
type EventWriter interface {
	Append(context.Context, event.Kind, any) error
}

// Persist appends one receipt with short bounded retries for transient stores.
func Persist(ctx context.Context, writer EventWriter, receipt Receipt) error {
	if writer == nil {
		return errors.New("delivery event writer is required")
	}
	payload := EventPayload{Category: EventCategory, Content: receipt}
	var err error
	for attempt := 0; attempt <= len(receiptRetryDelays); attempt++ {
		if err = writer.Append(ctx, event.RunDelivery, payload); err == nil {
			return nil
		}
		if attempt < len(receiptRetryDelays) {
			if waitErr := waitRetry(ctx, receiptRetryDelays[attempt]); waitErr != nil {
				return errors.Join(err, waitErr)
			}
		}
	}
	return err
}

func waitRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
