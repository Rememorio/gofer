package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/Rememorio/gofer/internal/policy"
	"github.com/Rememorio/gofer/internal/tool"
)

// EnvironmentProvider supplies request-scoped secrets without exposing them in
// model-visible tool arguments.
type EnvironmentProvider func(context.Context) (map[string]string, error)

// CommandTool binds one scoped Executor to the standard bash tool.
type CommandTool struct {
	Executor    Executor
	Environment EnvironmentProvider
}

// Tool constructs the standard bash command tool.
func (commandTool CommandTool) Tool() (tool.Tool, error) {
	if commandTool.Executor == nil {
		return nil, fmt.Errorf("%w: command executor is required", ErrInvalidConfig)
	}
	return tool.Func{
		DefinitionValue: tool.Definition{
			Name:        "bash",
			Description: "Run a bounded shell command in the current thread sandbox. Long-running services must be backgrounded.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"description":{"type":"string","minLength":1,"maxLength":200},"command":{"type":"string","minLength":1},"working_directory":{"type":"string"},"timeout_seconds":{"type":"number","exclusiveMinimum":0,"maximum":3600}},"required":["description","command"],"additionalProperties":false}`),
		},
		ExecuteFunc: commandTool.execute,
	}, nil
}

func (commandTool CommandTool) execute(ctx context.Context, arguments json.RawMessage) (json.RawMessage, error) {
	var input struct {
		Description      string  `json:"description"`
		Command          string  `json:"command"`
		WorkingDirectory string  `json:"working_directory"`
		TimeoutSeconds   float64 `json:"timeout_seconds"`
	}
	if err := decodeToolArguments(arguments, &input); err != nil {
		return nil, err
	}
	environment := map[string]string(nil)
	if commandTool.Environment != nil {
		var err error
		environment, err = commandTool.Environment(ctx)
		if err != nil {
			return nil, fmt.Errorf("resolve command environment: %w", err)
		}
	}
	result, err := commandTool.Executor.Execute(ctx, Command{
		Script: input.Command, WorkingDirectory: input.WorkingDirectory,
		Environment: environment, Timeout: durationSeconds(input.TimeoutSeconds),
	})
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("encode command result: %w", err)
	}
	if !result.Successful() {
		return nil, tool.NewResultError(encoded)
	}
	return encoded, nil
}

// PolicyDescriptors returns the authorization contract for sandbox tools.
func PolicyDescriptors() map[string]policy.Descriptor {
	return map[string]policy.Descriptor{
		"bash": {Effect: policy.EffectExecute, ResourceFields: []string{"command"}},
	}
}

func decodeToolArguments(arguments json.RawMessage, output any) error {
	decoder := json.NewDecoder(bytes.NewReader(arguments))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return fmt.Errorf("decode bash arguments: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("decode bash arguments: multiple JSON values")
	}
	return nil
}

func durationSeconds(seconds float64) time.Duration {
	if seconds == 0 {
		return 0
	}
	return time.Duration(seconds * float64(time.Second))
}
