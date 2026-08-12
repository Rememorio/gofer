package humaninput

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/model"
	"github.com/Rememorio/gofer/internal/runtime"
	"github.com/Rememorio/gofer/internal/tool"
)

const disabledMessage = "Clarification is disabled in this context because no human is present to answer synchronously. Proceed with your best judgment, carry out the requested action, and state any assumptions in the final response."

// Middleware makes clarification exclusive, returns a structured artifact,
// and asks the runtime to settle the run as interrupted.
type Middleware struct {
	runtime.NopMiddleware
	Disabled bool
}

type disabledContextKey struct{}

// WithDisabled marks a non-interactive run so clarification becomes a normal
// instruction to proceed instead of a dead-ending pause.
func WithDisabled(ctx context.Context) context.Context {
	return context.WithValue(ctx, disabledContextKey{}, true)
}

var (
	_ runtime.Middleware               = (*Middleware)(nil)
	_ runtime.ModelResponseTransformer = (*Middleware)(nil)
	_ runtime.ToolExecutionInterceptor = (*Middleware)(nil)
)

// TransformModelResponse prevents side-effecting sibling calls from running
// in parallel with a request that must pause for human input.
func (*Middleware) TransformModelResponse(_ context.Context, response model.Response) (model.Response, error) {
	for _, call := range response.ToolCalls {
		if call.Name != ToolName {
			continue
		}
		response.ToolCalls = []domain.ToolCall{call}
		response.StopReason = model.StopToolUse
		return response, nil
	}
	return response, nil
}

// ExecuteTool intercepts ask_clarification before its placeholder handler.
func (middleware *Middleware) ExecuteTool(ctx context.Context, call domain.ToolCall, next runtime.ToolExecutor) (domain.ToolResult, error) {
	if call.Name != ToolName {
		return next(ctx, call)
	}
	if err := ctx.Err(); err != nil {
		return domain.ToolResult{}, err
	}
	if middleware != nil && (middleware.Disabled || disabledFromContext(ctx)) {
		output, _ := json.Marshal(map[string]string{"content": disabledMessage})
		return domain.ToolResult{CallID: call.ID, Output: output}, nil
	}
	return clarificationResult(call), nil
}

func clarificationResult(call domain.ToolCall) domain.ToolResult {
	request, fallback, err := BuildRequest(call)
	if err != nil {
		output, _ := json.Marshal(map[string]string{"error": err.Error()})
		return domain.ToolResult{CallID: call.ID, Output: output, IsError: true}
	}
	output, err := MarshalToolOutput(request, fallback)
	if err != nil {
		data, _ := json.Marshal(map[string]string{"error": err.Error()})
		return domain.ToolResult{CallID: call.ID, Output: data, IsError: true}
	}
	return domain.ToolResult{CallID: call.ID, Output: output, Interrupt: true}
}

func disabledFromContext(ctx context.Context) bool {
	disabled, _ := ctx.Value(disabledContextKey{}).(bool)
	return disabled
}

// Tool returns the model-visible schema. Production execution is intercepted
// by Middleware so the call ID can become the stable request ID.
func Tool() tool.Tool {
	return tool.Func{
		DefinitionValue: tool.Definition{
			Name:        ToolName,
			Description: "Ask one structured clarification when required information, an approach choice, explicit risk confirmation, or user approval is needed. The run pauses after this tool and continues only after the user replies.",
			InputSchema: json.RawMessage(`{
  "type":"object",
  "additionalProperties":false,
  "required":["question","clarification_type"],
  "properties":{
    "question":{"type":"string","minLength":1,"maxLength":8000},
    "clarification_type":{"type":"string","enum":["missing_info","ambiguous_requirement","approach_choice","risk_confirmation","suggestion"]},
    "context":{"type":["string","null"],"maxLength":8000},
    "options":{"type":["array","string","null"],"items":{"type":"string"},"maxItems":24},
    "fields":{"type":["array","string","null"],"maxItems":16,"items":{"type":"object","required":["name"],"additionalProperties":false,"properties":{"name":{"type":"string","minLength":1,"maxLength":200},"label":{"type":"string","maxLength":200},"type":{"type":"string","enum":["text","textarea","number","select","multi_select","checkbox","date"]},"required":{"type":["boolean","string","number"]},"options":{"type":["array","string","null"],"items":{"type":"string"},"maxItems":24},"placeholder":{"type":"string","maxLength":200}}}}
  }
}`),
		},
		ExecuteFunc: func(context.Context, json.RawMessage) (json.RawMessage, error) {
			return nil, errors.New("clarification middleware is required")
		},
	}
}
