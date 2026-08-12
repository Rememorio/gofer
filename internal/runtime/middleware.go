package runtime

import (
	"context"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/model"
)

// Middleware observes or constrains model and tool boundaries.
// Implementations are invoked serially in registration order.
type Middleware interface {
	BeforeModel(context.Context, *model.Request) error
	AfterModel(context.Context, model.Response) error
	BeforeTool(context.Context, domain.ToolCall) error
	AfterTool(context.Context, domain.ToolCall, domain.ToolResult) error
}

// ToolResultObserver optionally inspects the unmodified result returned by a
// tool. Observers run before result transformers and durable persistence, so
// bookkeeping middleware can retain structured facts that a context-budget
// transformer may subsequently replace with a compact synopsis.
type ToolResultObserver interface {
	ObserveToolResult(context.Context, domain.ToolCall, domain.ToolResult) error
}

// ToolResultTransformer optionally replaces a tool result before it is added
// to the durable journal or a later model request. Transformers run serially
// in middleware registration order.
type ToolResultTransformer interface {
	TransformToolResult(context.Context, domain.ToolCall, domain.ToolResult) (domain.ToolResult, error)
}

// ModelResponseTransformer optionally rewrites one fully collected response
// before validation, journaling, or tool execution. Transformers run serially
// in registration order and must preserve provider usage accounting. Returning
// ErrRetryModelResponse discards the message, journals its usage as a retry,
// and spends another normal model turn.
type ModelResponseTransformer interface {
	TransformModelResponse(context.Context, model.Response) (model.Response, error)
}

// ToolExecutor invokes one tool call at the registry boundary. Execution
// interceptors receive a next function so they can serialize, short-circuit,
// or observe the actual operation without moving tool-specific policy into the
// runtime.
type ToolExecutor func(context.Context, domain.ToolCall) (domain.ToolResult, error)

// ToolExecutionInterceptor optionally wraps actual tool execution. The first
// registered interceptor is the outermost wrapper. An interceptor that
// short-circuits must return a valid result with the original call ID.
type ToolExecutionInterceptor interface {
	ExecuteTool(context.Context, domain.ToolCall, ToolExecutor) (domain.ToolResult, error)
}

// NopMiddleware provides no-op implementations for selective embedding.
type NopMiddleware struct{}

// BeforeModel accepts the request unchanged.
func (NopMiddleware) BeforeModel(context.Context, *model.Request) error { return nil }

// AfterModel accepts the response unchanged.
func (NopMiddleware) AfterModel(context.Context, model.Response) error { return nil }

// BeforeTool allows the tool call.
func (NopMiddleware) BeforeTool(context.Context, domain.ToolCall) error { return nil }

// AfterTool accepts the tool result.
func (NopMiddleware) AfterTool(context.Context, domain.ToolCall, domain.ToolResult) error {
	return nil
}
