package domain

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const encodedIDLength = 26

var (
	idEncoding = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

	// ErrInvalidID identifies a malformed Gofer identifier.
	ErrInvalidID = errors.New("invalid identifier")
)

// ThreadID uniquely identifies a conversation thread.
type ThreadID string

// RunID uniquely identifies one execution within a thread.
type RunID string

// MessageID uniquely identifies a normalized conversation message.
type MessageID string

// EventID uniquely identifies an immutable journal event.
type EventID string

// FeedbackID uniquely identifies one user feedback record.
type FeedbackID string

// NewThreadID generates a time-sortable thread identifier.
func NewThreadID() (ThreadID, error) {
	value, err := newID("thr", time.Now(), rand.Read)
	return ThreadID(value), err
}

// ParseThreadID validates and returns value as a ThreadID.
func ParseThreadID(value string) (ThreadID, error) {
	if err := validateID(value, "thr"); err != nil {
		return "", err
	}
	return ThreadID(value), nil
}

// NewRunID generates a time-sortable run identifier.
func NewRunID() (RunID, error) {
	value, err := newID("run", time.Now(), rand.Read)
	return RunID(value), err
}

// ParseRunID validates and returns value as a RunID.
func ParseRunID(value string) (RunID, error) {
	if err := validateID(value, "run"); err != nil {
		return "", err
	}
	return RunID(value), nil
}

// NewMessageID generates a time-sortable message identifier.
func NewMessageID() (MessageID, error) {
	value, err := newID("msg", time.Now(), rand.Read)
	return MessageID(value), err
}

// ParseMessageID validates and returns value as a MessageID.
func ParseMessageID(value string) (MessageID, error) {
	if err := validateID(value, "msg"); err != nil {
		return "", err
	}
	return MessageID(value), nil
}

// NewEventID generates a time-sortable event identifier.
func NewEventID() (EventID, error) {
	value, err := newID("evt", time.Now(), rand.Read)
	return EventID(value), err
}

// ParseEventID validates and returns value as an EventID.
func ParseEventID(value string) (EventID, error) {
	if err := validateID(value, "evt"); err != nil {
		return "", err
	}
	return EventID(value), nil
}

// NewFeedbackID generates a time-sortable feedback identifier.
func NewFeedbackID() (FeedbackID, error) {
	value, err := newID("fbk", time.Now(), rand.Read)
	return FeedbackID(value), err
}

// ParseFeedbackID validates and returns value as a FeedbackID.
func ParseFeedbackID(value string) (FeedbackID, error) {
	if err := validateID(value, "fbk"); err != nil {
		return "", err
	}
	return FeedbackID(value), nil
}

func newID(prefix string, now time.Time, read func([]byte) (int, error)) (string, error) {
	var payload [16]byte
	milliseconds := uint64(now.UnixMilli())
	for index := 5; index >= 0; index-- {
		payload[index] = byte(milliseconds)
		milliseconds >>= 8
	}
	count, err := read(payload[6:])
	if err != nil {
		return "", fmt.Errorf("generate %s identifier: %w", prefix, err)
	}
	if count != len(payload[6:]) {
		return "", fmt.Errorf("generate %s identifier: %w", prefix, io.ErrUnexpectedEOF)
	}
	encoded := strings.ToLower(idEncoding.EncodeToString(payload[:]))
	return prefix + "_" + encoded, nil
}

func validateID(value, prefix string) error {
	wantPrefix := prefix + "_"
	if !strings.HasPrefix(value, wantPrefix) {
		return fmt.Errorf("%w: want %s prefix", ErrInvalidID, wantPrefix)
	}
	encoded := strings.TrimPrefix(value, wantPrefix)
	if len(encoded) != encodedIDLength {
		return fmt.Errorf("%w: want %d encoded characters", ErrInvalidID, encodedIDLength)
	}
	if _, err := idEncoding.DecodeString(strings.ToUpper(encoded)); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidID, err)
	}
	return nil
}
