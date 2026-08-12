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
