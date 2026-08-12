package sandbox

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/Rememorio/gofer/internal/workspace"
)

// LocalConfig configures explicitly trusted command execution on the host.
// Host execution is fail-closed unless AllowHostExecution is true.
type LocalConfig struct {
	Mounts             []Mount
	AllowHostExecution bool
	Shell              string
	ShellArguments     []string
	InheritEnvironment []string
	StaticEnvironment  map[string]string
	MaxOutputBytes     int64
	MaxScriptBytes     int
	CommandTimeout     time.Duration
	MaxTimeout         time.Duration
}

// Local executes commands directly on the host with bounded output and time.
// It is a workflow boundary, not an operating-system security boundary.
type Local struct {
	mounts    []Mount
	allowHost bool
	shell     string
	shellArgs []string
	baseEnv   map[string]string
	limits    limits
}

// NewLocal validates config and constructs a host executor.
func NewLocal(config LocalConfig) (*Local, error) {
	mounts, err := validateMounts(config.Mounts)
	if err != nil {
		return nil, err
	}
	limits, err := newLimits(config.MaxOutputBytes, config.MaxScriptBytes, config.CommandTimeout, config.MaxTimeout)
	if err != nil {
		return nil, err
	}
	shell, shellArguments := localShell(config.Shell, config.ShellArguments)
	if strings.TrimSpace(shell) == "" || strings.IndexByte(shell, 0) >= 0 {
		return nil, fmt.Errorf("%w: shell is invalid", ErrInvalidConfig)
	}
	for _, argument := range shellArguments {
		if strings.IndexByte(argument, 0) >= 0 {
			return nil, fmt.Errorf("%w: shell argument contains NUL", ErrInvalidConfig)
		}
	}
	baseEnvironment, err := localEnvironment(config.InheritEnvironment, config.StaticEnvironment)
	if err != nil {
		return nil, err
	}
	workingMount, found := findMount(mounts, "/mnt/user-data/workspace")
	if !found || workingMount.ReadOnly {
		return nil, fmt.Errorf("%w: a writable /mnt/user-data/workspace mount is required", ErrInvalidConfig)
	}
	baseEnvironment["HOME"] = workingMount.Source
	baseEnvironment["GOFER_SANDBOX"] = "local"
	return &Local{
		mounts: mounts, allowHost: config.AllowHostExecution, shell: shell,
		shellArgs: append([]string(nil), shellArguments...), baseEnv: baseEnvironment,
		limits: limits,
	}, nil
}

// Execute runs one command after rewriting stable virtual paths to scoped host
// mounts. The caller must explicitly opt in to host execution.
func (local *Local) Execute(ctx context.Context, command Command) (Result, error) {
	if local == nil {
		return Result{}, fmt.Errorf("%w: local executor is nil", ErrInvalidConfig)
	}
	if !local.allowHost {
		return Result{}, ErrHostExecutionDisabled
	}
	command, err := local.limits.validate(command)
	if err != nil {
		return Result{}, err
	}
	directory, err := resolveLocalDirectory(command.WorkingDirectory, local.mounts)
	if err != nil {
		return Result{}, err
	}
	environment := mergeEnvironment(local.baseEnv, command.Environment)
	environment["PWD"] = directory
	script := rewriteVirtualPaths(command.Script, local.mounts)
	arguments := append(append([]string(nil), local.shellArgs...), script)
	result, err := executeProcess(ctx, processSpec{
		name: local.shell, arguments: arguments, directory: directory,
		environment: environmentList(environment), timeout: command.Timeout,
		maxOutput: local.limits.maxOutputBytes,
	})
	result = maskHostPaths(maskSecrets(result, command.Environment), local.mounts)
	return result, err
}

func localShell(shell string, arguments []string) (string, []string) {
	if shell != "" {
		return shell, append([]string(nil), arguments...)
	}
	if runtime.GOOS == "windows" {
		return "powershell.exe", []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-Command"}
	}
	return "/bin/sh", []string{"-lc"}
}

func localEnvironment(inherit []string, static map[string]string) (map[string]string, error) {
	if inherit == nil {
		inherit = []string{"PATH", "LANG", "LC_ALL", "TZ"}
	}
	environment := make(map[string]string, len(inherit)+len(static))
	for _, name := range inherit {
		if !environmentNamePattern.MatchString(name) {
			return nil, fmt.Errorf("%w: invalid inherited environment name %q", ErrInvalidConfig, name)
		}
		if value, found := os.LookupEnv(name); found {
			environment[name] = value
		}
	}
	if err := validateEnvironment(static); err != nil {
		return nil, fmt.Errorf("%w: static environment: %w", ErrInvalidConfig, err)
	}
	for name, value := range static {
		environment[name] = value
	}
	return environment, nil
}

func findMount(mounts []Mount, target string) (Mount, bool) {
	for _, mount := range mounts {
		if mount.Target == target {
			return mount, true
		}
	}
	return Mount{}, false
}

// MountsFromWorkspace converts trusted workspace execution mounts.
func MountsFromWorkspace(values []workspace.ExecutionMount) []Mount {
	mounts := make([]Mount, len(values))
	for index, value := range values {
		mounts[index] = Mount{Source: value.HostPath, Target: value.VirtualPath, ReadOnly: value.ReadOnly}
	}
	return mounts
}
