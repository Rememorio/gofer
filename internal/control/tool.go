package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/policy"
	"github.com/Rememorio/gofer/internal/tool"
)

// Tools binds control operations to one thread.
type Tools struct {
	Service  *Service
	ThreadID domain.ThreadID
}

// Register atomically registers goal and todo tools.
func (tools Tools) Register(registry *tool.Registry) error {
	if registry == nil || tools.Service == nil {
		return fmt.Errorf("%w: registry and service are required", ErrInvalid)
	}
	return registry.RegisterAll(tools.definitions()...)
}

// PolicyDescriptors returns authorization metadata for control tools.
func PolicyDescriptors() map[string]policy.Descriptor {
	return map[string]policy.Descriptor{"control_read": {Effect: policy.EffectRead}, "goal_create": {Effect: policy.EffectWrite}, "goal_update": {Effect: policy.EffectWrite}, "todo_write": {Effect: policy.EffectWrite}}
}

func (tools Tools) definitions() []tool.Tool {
	return []tool.Tool{
		tools.function("control_read", "Read the current goal and todo plan.", `{"type":"object","additionalProperties":false}`, true, func(ctx context.Context, _ json.RawMessage) (State, error) {
			return tools.Service.Snapshot(ctx, tools.ThreadID)
		}),
		tools.function("goal_create", "Create an explicit long-running goal.", `{"type":"object","properties":{"objective":{"type":"string","minLength":1,"maxLength":16384},"token_budget":{"type":"integer","minimum":0}},"required":["objective"],"additionalProperties":false}`, false, func(ctx context.Context, raw json.RawMessage) (State, error) {
			var input struct {
				Objective   string `json:"objective"`
				TokenBudget int    `json:"token_budget"`
			}
			if err := decode(raw, &input); err != nil {
				return State{}, err
			}
			return tools.Service.CreateGoal(ctx, tools.ThreadID, input.Objective, input.TokenBudget)
		}),
		tools.function("goal_update", "Mark the active goal complete or blocked.", `{"type":"object","properties":{"status":{"type":"string","enum":["complete","blocked"]}},"required":["status"],"additionalProperties":false}`, false, func(ctx context.Context, raw json.RawMessage) (State, error) {
			var input struct {
				Status GoalStatus `json:"status"`
			}
			if err := decode(raw, &input); err != nil {
				return State{}, err
			}
			return tools.Service.SetGoalStatus(ctx, tools.ThreadID, input.Status)
		}),
		tools.function("todo_write", "Replace the ordered todo plan for the active goal.", `{"type":"object","properties":{"todos":{"type":"array","maxItems":256,"items":{"type":"object","properties":{"id":{"type":"string","maxLength":128},"step":{"type":"string","minLength":1,"maxLength":8192},"status":{"type":"string","enum":["pending","in_progress","completed"]}},"required":["step","status"],"additionalProperties":false}}},"required":["todos"],"additionalProperties":false}`, false, func(ctx context.Context, raw json.RawMessage) (State, error) {
			var input struct {
				Todos []Todo `json:"todos"`
			}
			if err := decode(raw, &input); err != nil {
				return State{}, err
			}
			return tools.Service.ReplaceTodos(ctx, tools.ThreadID, input.Todos)
		}),
	}
}

func (tools Tools) function(name, description, schema string, readOnly bool, execute func(context.Context, json.RawMessage) (State, error)) tool.Tool {
	return tool.Func{DefinitionValue: tool.Definition{Name: name, Description: description, InputSchema: json.RawMessage(schema), ReadOnly: readOnly}, ExecuteFunc: func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		state, err := execute(ctx, raw)
		if err != nil {
			output, _ := json.Marshal(map[string]string{"error": err.Error()})
			return nil, tool.NewResultError(output)
		}
		return json.Marshal(state)
	}}
}

func decode(raw json.RawMessage, output any) error {
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
