package modellength

import (
	"context"
	"errors"
	"strings"

	"github.com/Rememorio/gofer/internal/model"
	"github.com/Rememorio/gofer/internal/runtime"
)

// ErrInvalidMiddleware identifies a nil model-length guard.
var ErrInvalidMiddleware = errors.New("invalid model length middleware")

// Middleware promotes visible terminal max-token responses to an explicit
// capped outcome without altering their content or provider usage.
type Middleware struct{ runtime.NopMiddleware }

var (
	_ runtime.Middleware               = (*Middleware)(nil)
	_ runtime.ModelResponseTransformer = (*Middleware)(nil)
)

// New constructs a stateless model-length guard.
func New() *Middleware { return &Middleware{} }

// TransformModelResponse preserves only terminal visible text. Empty capped
// responses and capped responses carrying tool intent remain protocol errors.
func (middleware *Middleware) TransformModelResponse(ctx context.Context, response model.Response) (model.Response, error) {
	if middleware == nil {
		return model.Response{}, ErrInvalidMiddleware
	}
	if err := ctx.Err(); err != nil {
		return model.Response{}, err
	}
	if response.StopReason != model.StopMaxTokens || len(response.ToolCalls) != 0 || strings.TrimSpace(response.Text) == "" {
		return response, nil
	}
	response.StopReason = model.StopModelLengthCapped
	return response, nil
}
