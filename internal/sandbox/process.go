package sandbox

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type processSpec struct {
	name        string
	arguments   []string
	directory   string
	environment []string
	timeout     time.Duration
	maxOutput   int64
}

type outputBudget struct {
	mu        sync.Mutex
	remaining int64
	total     int64
	truncated bool
}

type boundedCapture struct {
	budget *outputBudget
	data   []byte
}

func (capture *boundedCapture) Write(data []byte) (int, error) {
	capture.budget.mu.Lock()
	defer capture.budget.mu.Unlock()
	capture.budget.total += int64(len(data))
	keep := int64(len(data))
	if keep > capture.budget.remaining {
		keep = capture.budget.remaining
		capture.budget.truncated = true
	}
	if keep > 0 {
		capture.data = append(capture.data, data[:keep]...)
		capture.budget.remaining -= keep
	}
	return len(data), nil
}

func executeProcess(ctx context.Context, spec processSpec) (Result, error) {
	executionContext, cancel := context.WithTimeoutCause(ctx, spec.timeout, ErrTimedOut)
	defer cancel()
	command := exec.CommandContext(executionContext, spec.name, spec.arguments...)
	command.Dir = spec.directory
	command.Env = append([]string(nil), spec.environment...)
	prepareProcess(command)
	command.Cancel = func() error { return terminateProcess(command.Process) }
	command.WaitDelay = 2 * time.Second
	budget := &outputBudget{remaining: spec.maxOutput}
	stdout := &boundedCapture{budget: budget}
	stderr := &boundedCapture{budget: budget}
	command.Stdout, command.Stderr = stdout, stderr
	started := time.Now()
	err := command.Run()
	duration := time.Since(started)
	result := processResult(command, stdout, stderr, budget, duration)
	if err == nil {
		return result, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, ctxErr
	}
	if errors.Is(context.Cause(executionContext), ErrTimedOut) {
		result.TimedOut = true
		return result, nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return result, nil
	}
	return result, err
}

func processResult(
	command *exec.Cmd,
	stdout, stderr *boundedCapture,
	budget *outputBudget,
	duration time.Duration,
) Result {
	exitCode := -1
	if command.ProcessState != nil {
		exitCode = command.ProcessState.ExitCode()
	}
	budget.mu.Lock()
	total, truncated := budget.total, budget.truncated
	budget.mu.Unlock()
	return Result{
		Stdout:   strings.ToValidUTF8(string(stdout.data), "�"),
		Stderr:   strings.ToValidUTF8(string(stderr.data), "�"),
		ExitCode: exitCode, Duration: duration, DurationMillis: duration.Milliseconds(),
		Truncated: truncated, TotalOutputBytes: total,
	}
}
