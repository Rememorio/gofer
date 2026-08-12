//go:build !windows

package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/workspace"
)

func TestLocalExecuteRewritesPathsAndMasksSecrets(t *testing.T) {
	t.Parallel()

	workspaceSource := filepath.Join(t.TempDir(), "host path's workspace")
	if err := os.MkdirAll(workspaceSource, 0o700); err != nil {
		t.Fatalf("MkdirAll(): %v", err)
	}
	local := newTestLocal(t, LocalConfig{
		Mounts:             []Mount{{Source: workspaceSource, Target: workspace.WorkspaceRoot}},
		AllowHostExecution: true, InheritEnvironment: []string{},
	})
	result, err := local.Execute(context.Background(), Command{
		Script: `printf '%s\n' "$TOKEN"
printf single > '/mnt/user-data/workspace/single.txt'
printf double > "/mnt/user-data/workspace/double.txt"
pwd
printf '%s' "$TOKEN" >&2`,
		Environment: map[string]string{"TOKEN": "super-secret"},
	})
	if err != nil || !result.Successful() {
		t.Fatalf("Execute() = %#v, %v", result, err)
	}
	if strings.Contains(result.Stdout+result.Stderr, "super-secret") ||
		!strings.Contains(result.Stdout, "[REDACTED]") ||
		!strings.Contains(result.Stdout, workspace.WorkspaceRoot) {
		t.Fatalf("masked output = %#v", result)
	}
	for name, want := range map[string]string{"single.txt": "single", "double.txt": "double"} {
		data, readErr := os.ReadFile(filepath.Join(workspaceSource, name))
		if readErr != nil || string(data) != want {
			t.Fatalf("ReadFile(%s) = %q, %v", name, data, readErr)
		}
	}
}

func TestLocalNonZeroTimeoutAndCancellation(t *testing.T) {
	t.Parallel()

	local := newTestLocal(t, LocalConfig{AllowHostExecution: true})
	failed, err := local.Execute(context.Background(), Command{Script: "printf failed >&2; exit 7"})
	if err != nil || failed.ExitCode != 7 || failed.Successful() || failed.Stderr != "failed" {
		t.Fatalf("Execute(nonzero) = %#v, %v", failed, err)
	}
	timedOut, err := local.Execute(context.Background(), Command{Script: "sleep 5", Timeout: 20 * time.Millisecond})
	if err != nil || !timedOut.TimedOut || timedOut.Successful() {
		t.Fatalf("Execute(timeout) = %#v, %v", timedOut, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := local.Execute(ctx, Command{Script: "sleep 5"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute(cancelled) error = %v, want context.Canceled", err)
	}
}

func TestLocalBoundsCombinedOutput(t *testing.T) {
	t.Parallel()

	local := newTestLocal(t, LocalConfig{AllowHostExecution: true, MaxOutputBytes: 8})
	result, err := local.Execute(context.Background(), Command{
		Script: "printf 'abcdefgh'; printf 'ijklmnop' >&2",
	})
	if err != nil || !result.Truncated || result.TotalOutputBytes != 16 ||
		len(result.Stdout)+len(result.Stderr) != 8 {
		t.Fatalf("Execute(output limit) = %#v, %v", result, err)
	}
}

func TestLocalFailsClosedAndValidatesConfig(t *testing.T) {
	t.Parallel()

	local := newTestLocal(t, LocalConfig{})
	if _, err := local.Execute(context.Background(), Command{Script: "true"}); !errors.Is(err, ErrHostExecutionDisabled) {
		t.Fatalf("Execute(disabled) error = %v", err)
	}
	var nilLocal *Local
	if _, err := nilLocal.Execute(context.Background(), Command{Script: "true"}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil Execute() error = %v", err)
	}
	mounts := testMounts(t)
	invalid := []LocalConfig{
		{Mounts: mounts, Shell: " "},
		{Mounts: mounts, Shell: "/bin/sh", ShellArguments: []string{"\x00"}},
		{Mounts: mounts, InheritEnvironment: []string{"BAD-NAME"}},
		{Mounts: mounts, StaticEnvironment: map[string]string{"BAD-NAME": "x"}},
		{Mounts: []Mount{{Source: t.TempDir(), Target: workspace.WorkspaceRoot, ReadOnly: true}}},
	}
	for _, config := range invalid {
		if _, err := NewLocal(config); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("NewLocal(%#v) error = %v, want ErrInvalidConfig", config, err)
		}
	}
}

func TestLocalWorkingDirectoryAndScriptValidation(t *testing.T) {
	t.Parallel()

	local := newTestLocal(t, LocalConfig{AllowHostExecution: true, MaxScriptBytes: 4})
	for _, command := range []Command{
		{Script: "12345"},
		{Script: "true", WorkingDirectory: "/etc"},
	} {
		if _, err := local.Execute(context.Background(), command); !errors.Is(err, ErrInvalidCommand) {
			t.Fatalf("Execute(%#v) error = %v, want ErrInvalidCommand", command, err)
		}
	}
}

func TestDockerExecuteAndTimeoutCleanupWithFakeClient(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	binary := filepath.Join(directory, "docker")
	script := `#!/bin/sh
if [ "$1" = "rm" ]; then
  printf '%s' "$*" > "$(dirname "$0")/removed"
  exit 0
fi
cidfile=""
previous=""
for argument in "$@"; do
  if [ "$previous" = "cidfile" ]; then cidfile="$argument"; previous=""; continue; fi
  if [ "$argument" = "--cidfile" ]; then previous="cidfile"; fi
done
printf 'fake-container' > "$cidfile"
case "$*" in
  *gofer-timeout*) sleep 5 ;;
  *) printf '%s\n' "$*" ;;
esac
`
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatalf("WriteFile(fake Docker): %v", err)
	}
	docker := newTestDocker(t, DockerConfig{Binary: binary, MaxOutputBytes: 64 << 10})
	result, err := docker.Execute(context.Background(), Command{Script: "true"})
	if err != nil || !result.Successful() {
		t.Fatalf("Execute(fake Docker) = %#v, %v", result, err)
	}
	for _, mount := range docker.mounts {
		if strings.Contains(result.Stdout, mount.Source) {
			t.Fatalf("Docker output leaked host mount: %q", result.Stdout)
		}
	}
	timedOut, err := docker.Execute(context.Background(), Command{
		Script: "gofer-timeout", Timeout: 20 * time.Millisecond,
	})
	if err != nil || !timedOut.TimedOut {
		t.Fatalf("Execute(fake Docker timeout) = %#v, %v", timedOut, err)
	}
	removed, err := os.ReadFile(filepath.Join(directory, "removed"))
	if err != nil || !strings.Contains(string(removed), "rm --force fake-container") {
		t.Fatalf("forced removal = %q, %v", removed, err)
	}
}

func newTestLocal(t *testing.T, overrides LocalConfig) *Local {
	t.Helper()
	mounts := overrides.Mounts
	if mounts == nil {
		mounts = testMounts(t)
	}
	config := overrides
	config.Mounts = mounts
	if config.InheritEnvironment == nil {
		config.InheritEnvironment = []string{}
	}
	local, err := NewLocal(config)
	if err != nil {
		t.Fatalf("NewLocal(): %v", err)
	}
	return local
}
