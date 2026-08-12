package subagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/Rememorio/gofer/internal/policy"
	"github.com/Rememorio/gofer/internal/tool"
)

// Tools binds a parent task identity and depth to child-agent operations.
type Tools struct {
	Manager  *Manager
	ParentID string
	Depth    int
}

// Register atomically registers child-agent lifecycle tools.
func (tools Tools) Register(registry *tool.Registry) error {
	if registry == nil || tools.Manager == nil || tools.Depth < 0 {
		return fmt.Errorf("%w: registry, manager, and non-negative parent depth are required", ErrInvalid)
	}
	return registry.RegisterAll(tools.definitions()...)
}

// PolicyDescriptors returns authorization metadata for subagent tools.
func PolicyDescriptors() map[string]policy.Descriptor {
	return map[string]policy.Descriptor{
		"subagent_spawn":  {Effect: policy.EffectExecute},
		"subagent_get":    {Effect: policy.EffectRead},
		"subagent_wait":   {Effect: policy.EffectRead},
		"subagent_list":   {Effect: policy.EffectRead},
		"subagent_cancel": {Effect: policy.EffectExecute},
	}
}

func (tools Tools) definitions() []tool.Tool {
	return []tool.Tool{
		tools.function("subagent_spawn", "Start a bounded child agent for an independent task.", `{"type":"object","properties":{"prompt":{"type":"string","minLength":1,"maxLength":65536},"metadata":{"type":"object","additionalProperties":{"type":"string"}}},"required":["prompt"],"additionalProperties":false}`, false, func(ctx context.Context, raw json.RawMessage) (any, error) {
			var input struct {
				Prompt   string            `json:"prompt"`
				Metadata map[string]string `json:"metadata"`
			}
			if err := decodeArguments(raw, &input); err != nil {
				return nil, err
			}
			return tools.Manager.Spawn(ctx, Request{ParentID: tools.ParentID, Depth: tools.Depth + 1, Prompt: input.Prompt, Metadata: input.Metadata})
		}),
		tools.idFunction("subagent_get", "Read one child-agent task without waiting.", func(ctx context.Context, id string) (Task, error) { return tools.Manager.Get(id) }),
		tools.idFunction("subagent_wait", "Wait for one child-agent task to finish.", func(ctx context.Context, id string) (Task, error) { return tools.Manager.Wait(ctx, id) }),
		tools.function("subagent_list", "List child-agent tasks.", `{"type":"object","additionalProperties":false}`, true, func(context.Context, json.RawMessage) (any, error) { return tools.Manager.List(), nil }),
		tools.function("subagent_cancel", "Request cancellation of one child-agent task.", idSchema, false, func(_ context.Context, raw json.RawMessage) (any, error) {
			id, err := decodeID(raw)
			if err != nil {
				return nil, err
			}
			return map[string]bool{"cancelled": true}, tools.Manager.Cancel(id)
		}),
	}
}

const idSchema = `{"type":"object","properties":{"id":{"type":"string","minLength":1}},"required":["id"],"additionalProperties":false}`

func (tools Tools) idFunction(name, description string, execute func(context.Context, string) (Task, error)) tool.Tool {
	return tools.function(name, description, idSchema, true, func(ctx context.Context, raw json.RawMessage) (any, error) {
		id, err := decodeID(raw)
		if err != nil {
			return nil, err
		}
		return execute(ctx, id)
	})
}

func (tools Tools) function(name, description, schema string, readOnly bool, execute func(context.Context, json.RawMessage) (any, error)) tool.Tool {
	return tool.Func{DefinitionValue: tool.Definition{Name: name, Description: description, InputSchema: json.RawMessage(schema), ReadOnly: readOnly}, ExecuteFunc: func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		value, err := execute(ctx, raw)
		if err != nil {
			output, _ := json.Marshal(map[string]string{"error": err.Error()})
			return nil, tool.NewResultError(output)
		}
		return json.Marshal(value)
	}}
}

func decodeID(raw json.RawMessage) (string, error) {
	var input struct {
		ID string `json:"id"`
	}
	if err := decodeArguments(raw, &input); err != nil {
		return "", err
	}
	return input.ID, nil
}

func decodeArguments(raw json.RawMessage, output any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("multiple JSON values")
	}
	return nil
}
