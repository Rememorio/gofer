package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/policy"
	"github.com/Rememorio/gofer/internal/tool"
	"github.com/Rememorio/gofer/internal/workspace"
)

func TestMountValidationAndWorkspaceConversion(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	linked := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(root, linked); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	mounts, err := validateMounts([]Mount{{Source: linked, Target: workspace.WorkspaceRoot}})
	if err != nil {
		t.Fatalf("validateMounts(): %v", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("EvalSymlinks(): %v", err)
	}
	if mounts[0].Source != resolvedRoot {
		t.Fatalf("resolved source = %q, want %q", mounts[0].Source, resolvedRoot)
	}
	converted := MountsFromWorkspace([]workspace.ExecutionMount{{
		VirtualPath: workspace.UploadsRoot, HostPath: root, ReadOnly: true,
	}})
	if !reflect.DeepEqual(converted, []Mount{{Source: root, Target: workspace.UploadsRoot, ReadOnly: true}}) {
		t.Fatalf("MountsFromWorkspace() = %#v", converted)
	}
}

func TestMountValidationRejectsUnsafeValues(t *testing.T) {
	t.Parallel()

	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile(): %v", err)
	}
	validSource := t.TempDir()
	tests := [][]Mount{
		nil,
		{{Source: "relative", Target: "/mnt/data"}},
		{{Source: file, Target: "/mnt/data"}},
		{{Source: validSource, Target: "relative"}},
		{{Source: validSource, Target: "/"}},
		{{Source: validSource, Target: "/mnt/../etc"}},
		{{Source: validSource, Target: `/mnt\data`}},
		{{Source: validSource, Target: "/mnt/data"}, {Source: validSource, Target: "/mnt/data"}},
	}
	for _, mounts := range tests {
		if _, err := validateMounts(mounts); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("validateMounts(%#v) error = %v, want ErrInvalidConfig", mounts, err)
		}
	}
}

func TestLimitsAndEnvironmentValidation(t *testing.T) {
	t.Parallel()

	if _, err := newLimits(-1, 0, 0, 0); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("newLimits(negative) error = %v", err)
	}
	if _, err := newLimits(1, 1, time.Minute, time.Second); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("newLimits(timeout) error = %v", err)
	}
	limits, err := newLimits(0, 4, time.Second, time.Minute)
	if err != nil {
		t.Fatalf("newLimits(): %v", err)
	}
	invalid := []Command{
		{}, {Script: "\x00"}, {Script: "12345"}, {Script: "ok", Timeout: -1},
		{Script: "ok", Timeout: 2 * time.Minute}, {Script: "ok", Environment: map[string]string{"BAD-NAME": "x"}},
		{Script: "ok", Environment: map[string]string{"GOOD": "x\x00"}},
	}
	for _, command := range invalid {
		if _, err := limits.validate(command); !errors.Is(err, ErrInvalidCommand) {
			t.Fatalf("validate(%#v) error = %v, want ErrInvalidCommand", command, err)
		}
	}
	valid, err := limits.validate(Command{Script: "true", Environment: map[string]string{"TOKEN": "secret"}})
	if err != nil || valid.Timeout != time.Second || valid.Environment["TOKEN"] != "secret" {
		t.Fatalf("validate(valid) = %#v, %v", valid, err)
	}
	valid.Environment["TOKEN"] = "changed"
	original := map[string]string{"TOKEN": "secret"}
	cloned := cloneEnvironment(original)
	cloned["TOKEN"] = "changed"
	if original["TOKEN"] != "secret" {
		t.Fatal("cloneEnvironment() aliased input")
	}
}

func TestEnvironmentHelpersAndSecretMasking(t *testing.T) {
	t.Parallel()

	merged := mergeEnvironment(map[string]string{"A": "one", "B": "old"}, map[string]string{"B": "new"})
	if got := environmentList(merged); !reflect.DeepEqual(got, []string{"A=one", "B=new"}) {
		t.Fatalf("environmentList() = %#v", got)
	}
	masked := maskSecrets(Result{Stdout: "long-secret short", Stderr: "short long-secret"}, map[string]string{
		"A": "short", "B": "long-secret", "EMPTY": "",
	})
	if strings.Contains(masked.Stdout+masked.Stderr, "secret") || strings.Contains(masked.Stdout+masked.Stderr, "short") {
		t.Fatalf("maskSecrets() = %#v", masked)
	}
}

func TestPathMappingHelpers(t *testing.T) {
	t.Parallel()

	source := filepath.Join(t.TempDir(), "with space")
	if err := os.MkdirAll(filepath.Join(source, "nested"), 0o700); err != nil {
		t.Fatalf("MkdirAll(): %v", err)
	}
	mounts, err := validateMounts([]Mount{{Source: source, Target: workspace.WorkspaceRoot}})
	if err != nil {
		t.Fatalf("validateMounts(): %v", err)
	}
	resolved, err := resolveLocalDirectory(workspace.WorkspaceRoot+"/nested", mounts)
	wantResolved, resolveErr := filepath.EvalSymlinks(filepath.Join(source, "nested"))
	if resolveErr != nil {
		t.Fatalf("EvalSymlinks(): %v", resolveErr)
	}
	if err != nil || resolved != wantResolved {
		t.Fatalf("resolveLocalDirectory() = %q, %v", resolved, err)
	}
	for _, value := range []string{"relative", "/mnt/other", workspace.WorkspaceRoot + "/../other"} {
		if _, err := resolveLocalDirectory(value, mounts); !errors.Is(err, ErrInvalidCommand) {
			t.Fatalf("resolveLocalDirectory(%q) error = %v", value, err)
		}
	}
	if relative, ok := relativeToMount(workspace.WorkspaceRoot+"/nested", workspace.WorkspaceRoot); !ok || relative != "nested" {
		t.Fatalf("relativeToMount() = %q, %v", relative, ok)
	}
	if !withinDirectory(filepath.Join(source, "nested"), source) || withinDirectory(filepath.Dir(source), source) {
		t.Fatal("withinDirectory() returned an invalid containment result")
	}
}

func TestVirtualPathRewriteAndMasking(t *testing.T) {
	t.Parallel()

	source := filepath.Join(t.TempDir(), "host path")
	mounts := []Mount{{Source: source, Target: workspace.WorkspaceRoot}}
	for _, script := range []string{
		"cat " + workspace.WorkspaceRoot + "/file",
		"cat \"" + workspace.WorkspaceRoot + "/file\"",
		"cat '" + workspace.WorkspaceRoot + "/file'",
	} {
		rewritten := rewriteVirtualPaths(script, mounts)
		if strings.Contains(rewritten, workspace.WorkspaceRoot) || !strings.Contains(rewritten, source) {
			t.Fatalf("rewriteVirtualPaths(%q) = %q", script, rewritten)
		}
	}
	unchanged := "printf /mnt/user-data/workspace-other"
	if got := rewriteVirtualPaths(unchanged, mounts); got != unchanged {
		t.Fatalf("rewrite sibling = %q", got)
	}
	masked := maskHostPaths(Result{Stdout: source + "/file", Stderr: source}, mounts)
	if masked.Stdout != workspace.WorkspaceRoot+"/file" || masked.Stderr != workspace.WorkspaceRoot {
		t.Fatalf("maskHostPaths() = %#v", masked)
	}
	sibling := source + "-other"
	if got := replaceHostPath(sibling, source, workspace.WorkspaceRoot); got != sibling {
		t.Fatalf("replaceHostPath(sibling) = %q", got)
	}
}

func TestCommandToolSuccessAndFailure(t *testing.T) {
	t.Parallel()

	executor := &recordingExecutor{result: Result{Stdout: "ok", ExitCode: 0}}
	commandTool := CommandTool{
		Executor: executor,
		Environment: func(context.Context) (map[string]string, error) {
			return map[string]string{"TOKEN": "secret"}, nil
		},
	}
	candidate, err := commandTool.Tool()
	if err != nil {
		t.Fatalf("Tool(): %v", err)
	}
	registry := tool.NewRegistry()
	if err := registry.Register(candidate); err != nil {
		t.Fatalf("Register(): %v", err)
	}
	result, err := registry.Execute(context.Background(), domain.ToolCall{
		ID: "1", Name: "bash", Arguments: json.RawMessage(`{"description":"test","command":"echo ok","timeout_seconds":1.5}`),
	})
	if err != nil || result.IsError || executor.command.Timeout != 1500*time.Millisecond ||
		executor.command.Environment["TOKEN"] != "secret" {
		t.Fatalf("Execute(success) = %#v, command=%#v, err=%v", result, executor.command, err)
	}
	executor.result = Result{Stderr: "failed", ExitCode: 7}
	result, err = registry.Execute(context.Background(), domain.ToolCall{
		ID: "2", Name: "bash", Arguments: json.RawMessage(`{"description":"test","command":"false"}`),
	})
	if err != nil || !result.IsError || !strings.Contains(string(result.Output), "failed") {
		t.Fatalf("Execute(failure) = %#v, %v", result, err)
	}
	descriptor := PolicyDescriptors()["bash"]
	if descriptor.Effect != policy.EffectExecute || !reflect.DeepEqual(descriptor.ResourceFields, []string{"command"}) {
		t.Fatalf("PolicyDescriptors() = %#v", descriptor)
	}
}

func TestCommandToolValidationAndErrors(t *testing.T) {
	t.Parallel()

	if _, err := (CommandTool{}).Tool(); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Tool(nil) error = %v", err)
	}
	executor := &recordingExecutor{err: errors.New("execute")}
	candidate, err := (CommandTool{Executor: executor}).Tool()
	if err != nil {
		t.Fatalf("Tool(): %v", err)
	}
	if _, err := candidate.Execute(context.Background(), json.RawMessage(`{} {}`)); err == nil {
		t.Fatal("Execute(multiple JSON) error = nil")
	}
	environmentError := errors.New("secrets unavailable")
	candidate, _ = (CommandTool{
		Executor:    executor,
		Environment: func(context.Context) (map[string]string, error) { return nil, environmentError },
	}).Tool()
	if _, err := candidate.Execute(context.Background(), json.RawMessage(`{"description":"x","command":"true"}`)); !errors.Is(err, environmentError) {
		t.Fatalf("Execute(environment) error = %v", err)
	}
	candidate, _ = (CommandTool{Executor: executor}).Tool()
	if _, err := candidate.Execute(context.Background(), json.RawMessage(`{"description":"x","command":"true"}`)); !errors.Is(err, executor.err) {
		t.Fatalf("Execute(executor) error = %v", err)
	}
}

type recordingExecutor struct {
	command Command
	result  Result
	err     error
}

func (executor *recordingExecutor) Execute(_ context.Context, command Command) (Result, error) {
	executor.command = command
	return executor.result, executor.err
}
