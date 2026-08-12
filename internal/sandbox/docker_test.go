package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/workspace"
)

func TestDockerBuildInvocationIsHardened(t *testing.T) {
	t.Parallel()

	docker := newTestDocker(t, DockerConfig{NetworkEnabled: false})
	invocation, err := docker.buildInvocation(Command{
		Script: "printf ok", WorkingDirectory: workspace.WorkspaceRoot,
		Environment: map[string]string{"TOKEN": "super-secret"},
	})
	if err != nil {
		t.Fatalf("buildInvocation(): %v", err)
	}
	defer invocation.cleanup()
	joined := strings.Join(invocation.arguments, " ")
	for _, required := range []string{
		"--read-only", "--cap-drop ALL", "--security-opt no-new-privileges",
		"--network none", "--pids-limit 256", "--memory 1g", "--cpus 2",
		"--workdir " + workspace.WorkspaceRoot, "--env-file", "gofer:test", "/bin/sh -lc printf ok",
		workspace.UploadsRoot + ":ro", workspace.WorkspaceRoot + ":rw",
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("Docker arguments missing %q: %s", required, joined)
		}
	}
	if strings.Contains(joined, "super-secret") {
		t.Fatal("Docker arguments expose an environment secret")
	}
	data, err := os.ReadFile(invocation.envFile)
	if err != nil || !strings.Contains(string(data), "TOKEN=super-secret") {
		t.Fatalf("environment file = %q, %v", data, err)
	}
	info, err := os.Stat(invocation.envFile)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("environment file mode = %v, %v", info, err)
	}
	if _, err := os.Stat(invocation.cidFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cid file Stat() error = %v, want not exist", err)
	}
}

func TestDockerNetworkAndResourceOptions(t *testing.T) {
	t.Parallel()

	docker := newTestDocker(t, DockerConfig{
		NetworkEnabled: true, User: "1000:1000", Memory: "2g", CPUs: 1.5,
		PIDsLimit: 32, TmpfsSizeBytes: 16 << 20,
	})
	invocation, err := docker.buildInvocation(Command{Script: "true"})
	if err != nil {
		t.Fatalf("buildInvocation(): %v", err)
	}
	defer invocation.cleanup()
	arguments := invocation.arguments
	if containsArgumentPair(arguments, "--network", "none") {
		t.Fatal("network-enabled invocation contains --network none")
	}
	for key, value := range map[string]string{
		"--user": "1000:1000", "--memory": "2g", "--cpus": "1.5", "--pids-limit": "32",
	} {
		if !containsArgumentPair(arguments, key, value) {
			t.Fatalf("arguments missing %s %s: %#v", key, value, arguments)
		}
	}
}

func TestDockerValidation(t *testing.T) {
	t.Parallel()

	validMounts := testMounts(t)
	invalid := []DockerConfig{
		{Image: "", Mounts: validMounts},
		{Image: "bad image", Mounts: validMounts},
		{Binary: " docker ", Image: "gofer:test", Mounts: validMounts},
		{Image: "gofer:test", Mounts: validMounts, Memory: "zero"},
		{Image: "gofer:test", Mounts: validMounts, CPUs: -1},
		{Image: "gofer:test", Mounts: validMounts, CPUs: 300},
		{Image: "gofer:test", Mounts: validMounts, PIDsLimit: 2},
		{Image: "gofer:test", Mounts: validMounts, TmpfsSizeBytes: 2},
		{Image: "gofer:test", Mounts: validMounts, User: "bad\nuser"},
		{Image: "gofer:test", Mounts: []Mount{{Source: t.TempDir(), Target: workspace.WorkspaceRoot, ReadOnly: true}}},
	}
	for _, config := range invalid {
		if _, err := NewDocker(config); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("NewDocker(%#v) error = %v, want ErrInvalidConfig", config, err)
		}
	}
	var nilDocker *Docker
	if nilDocker.Available() {
		t.Fatal("nil Docker is available")
	}
	if _, err := nilDocker.Execute(t.Context(), Command{Script: "true"}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil Execute() error = %v", err)
	}
}

func TestDockerUnavailableAndEnvironmentNewline(t *testing.T) {
	t.Parallel()

	docker := newTestDocker(t, DockerConfig{Binary: filepath.Join(t.TempDir(), "missing")})
	if docker.Available() {
		t.Fatal("missing Docker binary is available")
	}
	if _, err := docker.Execute(t.Context(), Command{Script: "true"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Execute(unavailable) error = %v", err)
	}
	if _, err := docker.buildInvocation(Command{
		Script: "true", Environment: map[string]string{"TOKEN": "line1\nline2"},
	}); !errors.Is(err, ErrInvalidCommand) {
		t.Fatalf("buildInvocation(newline) error = %v, want ErrInvalidCommand", err)
	}
}

func TestDockerInvocationCleanup(t *testing.T) {
	t.Parallel()

	docker := newTestDocker(t, DockerConfig{})
	invocation, err := docker.buildInvocation(Command{Script: "true"})
	if err != nil {
		t.Fatalf("buildInvocation(): %v", err)
	}
	if err := os.WriteFile(invocation.cidFile, []byte("cid"), 0o600); err != nil {
		t.Fatalf("WriteFile(cid): %v", err)
	}
	paths := []string{invocation.envFile, invocation.cidFile}
	invocation.cleanup()
	for _, path := range paths {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Stat(%q) error = %v, want not exist", path, err)
		}
	}
	if _, err := uniqueTemporaryPath("gofer-test-"); err != nil {
		t.Fatalf("uniqueTemporaryPath(): %v", err)
	}
}

func newTestDocker(t *testing.T, overrides DockerConfig) *Docker {
	t.Helper()
	config := DockerConfig{
		Binary: overrides.Binary, Image: "gofer:test", Mounts: testMounts(t),
		NetworkEnabled: overrides.NetworkEnabled, User: overrides.User, Memory: overrides.Memory,
		CPUs: overrides.CPUs, PIDsLimit: overrides.PIDsLimit, TmpfsSizeBytes: overrides.TmpfsSizeBytes,
		MaxOutputBytes: overrides.MaxOutputBytes, MaxScriptBytes: overrides.MaxScriptBytes,
		CommandTimeout: overrides.CommandTimeout, MaxTimeout: overrides.MaxTimeout,
	}
	docker, err := NewDocker(config)
	if err != nil {
		t.Fatalf("NewDocker(): %v", err)
	}
	return docker
}

func testMounts(t *testing.T) []Mount {
	t.Helper()
	workspaceSource := t.TempDir()
	uploadsSource := t.TempDir()
	outputsSource := t.TempDir()
	return []Mount{
		{Source: workspaceSource, Target: workspace.WorkspaceRoot},
		{Source: uploadsSource, Target: workspace.UploadsRoot, ReadOnly: true},
		{Source: outputsSource, Target: workspace.OutputsRoot},
	}
}

func containsArgumentPair(arguments []string, key, value string) bool {
	for index := 0; index+1 < len(arguments); index++ {
		if reflect.DeepEqual(arguments[index:index+2], []string{key, value}) {
			return true
		}
	}
	return false
}

func TestResultSuccessful(t *testing.T) {
	t.Parallel()

	if !(Result{}).Successful() || (Result{ExitCode: 1}).Successful() || (Result{TimedOut: true}).Successful() {
		t.Fatal("Result.Successful() returned an invalid result")
	}
	if durationSeconds(1.25) != 1250*time.Millisecond || durationSeconds(0) != 0 {
		t.Fatal("durationSeconds() returned an invalid duration")
	}
}
