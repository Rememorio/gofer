package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Rememorio/gofer/internal/buildinfo"
)

func TestRunHelp(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := Run(context.Background(), nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("Run() code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "Go-native super agent") {
		t.Fatalf("Run() stdout = %q, want help", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("Run() stderr = %q, want empty", stderr.String())
	}
}

func TestRunHelpFlag(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	code := Run(context.Background(), []string{"--help"}, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("Run() code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "Usage:") {
		t.Fatalf("Run() stdout = %q, want usage", stdout.String())
	}
}

func TestRunVersionText(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	code := Run(context.Background(), []string{"--version"}, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("Run() code = %d, want 0", code)
	}
	if !strings.HasPrefix(stdout.String(), "gofer ") {
		t.Fatalf("Run() stdout = %q, want version", stdout.String())
	}
}

func TestRunVersionJSON(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := Run(context.Background(), []string{"version", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("Run() code = %d, want 0; stderr = %q", code, stderr.String())
	}

	var got buildinfo.Info
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("decode version JSON: %v", err)
	}
	if got.Version == "" {
		t.Fatal("version JSON has an empty version")
	}
}

func TestRunUnknownCommand(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := Run(context.Background(), []string{"missing"}, &stdout, &stderr)
	if !IsUsageError(code) {
		t.Fatalf("Run() code = %d, want usage error", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("Run() stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "unknown command") {
		t.Fatalf("Run() stderr = %q, want unknown-command error", stderr.String())
	}
}

func TestRunCancelledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var stderr bytes.Buffer
	code := Run(ctx, nil, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Fatalf("Run() code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "context canceled") {
		t.Fatalf("Run() stderr = %q, want cancellation", stderr.String())
	}
}

func TestRunVersionRejectsArguments(t *testing.T) {
	t.Parallel()

	var stderr bytes.Buffer
	code := Run(context.Background(), []string{"version", "extra"}, &bytes.Buffer{}, &stderr)
	if !IsUsageError(code) {
		t.Fatalf("Run() code = %d, want usage error", code)
	}
	if !strings.Contains(stderr.String(), "usage") {
		t.Fatalf("Run() stderr = %q, want usage", stderr.String())
	}
}

func TestRunVersionReportsEncodingFailure(t *testing.T) {
	t.Parallel()

	var stderr bytes.Buffer
	code := Run(context.Background(), []string{"version", "--json"}, errorWriter{}, &stderr)
	if code != 1 {
		t.Fatalf("Run() code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "encode version") {
		t.Fatalf("Run() stderr = %q, want encoding error", stderr.String())
	}
}

func TestIsUsageError(t *testing.T) {
	t.Parallel()

	if !IsUsageError(2) {
		t.Fatal("IsUsageError(2) = false, want true")
	}
	if IsUsageError(1) {
		t.Fatal("IsUsageError(1) = true, want false")
	}
	if got := exitCode(1).Error(); got != "exit code 1" {
		t.Fatalf("exitCode.Error() = %q, want %q", got, "exit code 1")
	}
	if errors.Is(exitCode(2), errors.New("different")) {
		t.Fatal("exitCode unexpectedly matches a different error type")
	}
}

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}
