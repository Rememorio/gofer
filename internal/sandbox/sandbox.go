package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	defaultMaxOutputBytes = 1 << 20
	defaultMaxScriptBytes = 64 << 10
	defaultTimeout        = 10 * time.Minute
	defaultMaxTimeout     = time.Hour
)

var (
	// ErrInvalidConfig identifies malformed sandbox configuration.
	ErrInvalidConfig = errors.New("invalid sandbox configuration")
	// ErrInvalidCommand identifies a malformed command request.
	ErrInvalidCommand = errors.New("invalid sandbox command")
	// ErrUnavailable identifies an execution backend that cannot be used.
	ErrUnavailable = errors.New("sandbox unavailable")
	// ErrHostExecutionDisabled identifies fail-closed local execution.
	ErrHostExecutionDisabled = errors.New("host command execution is disabled")
	// ErrTimedOut identifies a command terminated by its own wall-clock limit.
	ErrTimedOut = errors.New("sandbox command timed out")

	environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Mount maps one host directory to a stable absolute sandbox path.
type Mount struct {
	Source   string
	Target   string
	ReadOnly bool
}

// Command is one bounded shell command request.
type Command struct {
	Script           string
	WorkingDirectory string
	Environment      map[string]string
	Timeout          time.Duration
}

// Result is a bounded command outcome. Non-zero exits and timeouts are data;
// infrastructure failures and parent cancellation are returned as Go errors.
type Result struct {
	Stdout           string        `json:"stdout"`
	Stderr           string        `json:"stderr"`
	ExitCode         int           `json:"exit_code"`
	Duration         time.Duration `json:"-"`
	DurationMillis   int64         `json:"duration_ms"`
	TimedOut         bool          `json:"timed_out"`
	Truncated        bool          `json:"truncated"`
	TotalOutputBytes int64         `json:"total_output_bytes"`
}

// Successful reports whether a command completed with exit status zero.
func (result Result) Successful() bool { return !result.TimedOut && result.ExitCode == 0 }

// Executor runs commands inside one already-scoped execution environment.
type Executor interface {
	Execute(context.Context, Command) (Result, error)
}

type limits struct {
	maxOutputBytes int64
	maxScriptBytes int
	defaultTimeout time.Duration
	maxTimeout     time.Duration
}

func newLimits(maxOutputBytes int64, maxScriptBytes int, commandTimeout, maxTimeout time.Duration) (limits, error) {
	if maxOutputBytes < 0 || maxScriptBytes < 0 || commandTimeout < 0 || maxTimeout < 0 {
		return limits{}, fmt.Errorf("%w: limits must not be negative", ErrInvalidConfig)
	}
	if maxOutputBytes == 0 {
		maxOutputBytes = defaultMaxOutputBytes
	}
	if maxScriptBytes == 0 {
		maxScriptBytes = defaultMaxScriptBytes
	}
	if commandTimeout == 0 {
		commandTimeout = defaultTimeout
	}
	if maxTimeout == 0 {
		maxTimeout = defaultMaxTimeout
	}
	if commandTimeout > maxTimeout {
		return limits{}, fmt.Errorf("%w: default timeout exceeds maximum timeout", ErrInvalidConfig)
	}
	return limits{
		maxOutputBytes: maxOutputBytes, maxScriptBytes: maxScriptBytes,
		defaultTimeout: commandTimeout, maxTimeout: maxTimeout,
	}, nil
}

func (limits limits) validate(command Command) (Command, error) {
	if strings.TrimSpace(command.Script) == "" || strings.IndexByte(command.Script, 0) >= 0 {
		return Command{}, fmt.Errorf("%w: script is empty or contains NUL", ErrInvalidCommand)
	}
	if len(command.Script) > limits.maxScriptBytes {
		return Command{}, fmt.Errorf("%w: script exceeds %d bytes", ErrInvalidCommand, limits.maxScriptBytes)
	}
	if command.Timeout < 0 {
		return Command{}, fmt.Errorf("%w: timeout must not be negative", ErrInvalidCommand)
	}
	if command.Timeout == 0 {
		command.Timeout = limits.defaultTimeout
	}
	if command.Timeout > limits.maxTimeout {
		return Command{}, fmt.Errorf("%w: timeout exceeds %s", ErrInvalidCommand, limits.maxTimeout)
	}
	if err := validateEnvironment(command.Environment); err != nil {
		return Command{}, err
	}
	command.Environment = cloneEnvironment(command.Environment)
	return command, nil
}

func validateMounts(mounts []Mount) ([]Mount, error) {
	if len(mounts) == 0 {
		return nil, fmt.Errorf("%w: at least one mount is required", ErrInvalidConfig)
	}
	validated := make([]Mount, 0, len(mounts))
	targets := make(map[string]struct{}, len(mounts))
	for index, mount := range mounts {
		candidate, err := validateMount(mount)
		if err != nil {
			return nil, fmt.Errorf("%w: mounts[%d]: %w", ErrInvalidConfig, index, err)
		}
		if _, duplicate := targets[candidate.Target]; duplicate {
			return nil, fmt.Errorf("%w: duplicate mount target %q", ErrInvalidConfig, candidate.Target)
		}
		targets[candidate.Target] = struct{}{}
		validated = append(validated, candidate)
	}
	sort.Slice(validated, func(left, right int) bool {
		return len(validated[left].Target) > len(validated[right].Target)
	})
	return validated, nil
}

func validateMount(mount Mount) (Mount, error) {
	if !filepath.IsAbs(mount.Source) {
		return Mount{}, errors.New("mount source must be absolute")
	}
	info, err := os.Stat(mount.Source)
	if err != nil {
		return Mount{}, fmt.Errorf("inspect mount source: %w", err)
	}
	if !info.IsDir() {
		return Mount{}, errors.New("mount source must be a directory")
	}
	resolved, err := filepath.EvalSymlinks(mount.Source)
	if err != nil {
		return Mount{}, fmt.Errorf("resolve mount source: %w", err)
	}
	if !validTarget(mount.Target) {
		return Mount{}, errors.New("mount target must be a clean absolute POSIX path other than root")
	}
	mount.Source = resolved
	return mount, nil
}

func validTarget(target string) bool {
	return strings.HasPrefix(target, "/") && target != "/" && path.Clean(target) == target &&
		!strings.Contains(target, "\\")
}

func validateEnvironment(environment map[string]string) error {
	for name, value := range environment {
		if !environmentNamePattern.MatchString(name) || strings.IndexByte(value, 0) >= 0 {
			return fmt.Errorf("%w: invalid environment entry %q", ErrInvalidCommand, name)
		}
	}
	return nil
}

func cloneEnvironment(source map[string]string) map[string]string {
	cloned := make(map[string]string, len(source))
	for name, value := range source {
		cloned[name] = value
	}
	return cloned
}

func environmentList(environment map[string]string) []string {
	names := make([]string, 0, len(environment))
	for name := range environment {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]string, 0, len(names))
	for _, name := range names {
		result = append(result, name+"="+environment[name])
	}
	return result
}

func mergeEnvironment(base, override map[string]string) map[string]string {
	merged := cloneEnvironment(base)
	for name, value := range override {
		merged[name] = value
	}
	return merged
}

func maskSecrets(result Result, environment map[string]string) Result {
	values := make([]string, 0, len(environment))
	for _, value := range environment {
		if value != "" {
			values = append(values, value)
		}
	}
	sort.Slice(values, func(left, right int) bool { return len(values[left]) > len(values[right]) })
	for _, value := range values {
		result.Stdout = strings.ReplaceAll(result.Stdout, value, "[REDACTED]")
		result.Stderr = strings.ReplaceAll(result.Stderr, value, "[REDACTED]")
	}
	return result
}
