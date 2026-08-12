package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestIDRoundTrips(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		newID func() (string, error)
		parse func(string) error
	}{
		{name: "thread", newID: func() (string, error) { value, err := NewThreadID(); return string(value), err }, parse: func(value string) error { _, err := ParseThreadID(value); return err }},
		{name: "run", newID: func() (string, error) { value, err := NewRunID(); return string(value), err }, parse: func(value string) error { _, err := ParseRunID(value); return err }},
		{name: "message", newID: func() (string, error) { value, err := NewMessageID(); return string(value), err }, parse: func(value string) error { _, err := ParseMessageID(value); return err }},
		{name: "event", newID: func() (string, error) { value, err := NewEventID(); return string(value), err }, parse: func(value string) error { _, err := ParseEventID(value); return err }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			value, err := test.newID()
			if err != nil {
				t.Fatalf("generate ID: %v", err)
			}
			if err := test.parse(value); err != nil {
				t.Fatalf("parse generated ID %q: %v", value, err)
			}
		})
	}
}

func TestNewIDSortsByTime(t *testing.T) {
	t.Parallel()

	read := func(buffer []byte) (int, error) {
		clear(buffer)
		return len(buffer), nil
	}
	first, err := newID("run", time.UnixMilli(1), read)
	if err != nil {
		t.Fatalf("new first ID: %v", err)
	}
	second, err := newID("run", time.UnixMilli(2), read)
	if err != nil {
		t.Fatalf("new second ID: %v", err)
	}
	if first >= second {
		t.Fatalf("IDs are not time sortable: %q >= %q", first, second)
	}
}

func TestNewIDReportsEntropyFailure(t *testing.T) {
	t.Parallel()

	want := errors.New("entropy unavailable")
	_, err := newID("run", time.Time{}, func([]byte) (int, error) { return 0, want })
	if !errors.Is(err, want) {
		t.Fatalf("newID() error = %v, want wrapped %v", err, want)
	}
}

func TestNewIDReportsShortEntropyRead(t *testing.T) {
	t.Parallel()

	_, err := newID("run", time.Time{}, func([]byte) (int, error) { return 1, nil })
	if err == nil {
		t.Fatal("newID() error = nil, want short-read error")
	}
}

func TestParseIDsRejectMalformedValues(t *testing.T) {
	t.Parallel()

	valid, err := NewThreadID()
	if err != nil {
		t.Fatalf("NewThreadID(): %v", err)
	}

	values := []string{
		"run_" + strings.Repeat("0", encodedIDLength),
		"thr_short",
		"thr_" + strings.Repeat("u", encodedIDLength),
	}
	for _, value := range values {
		_, err := ParseThreadID(value)
		if !errors.Is(err, ErrInvalidID) {
			t.Fatalf("ParseThreadID(%q) error = %v, want ErrInvalidID", value, err)
		}
	}

	if _, err := ParseRunID(string(valid)); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("ParseRunID(thread ID) error = %v, want ErrInvalidID", err)
	}
}
