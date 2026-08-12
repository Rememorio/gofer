package safetyfinish

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Rememorio/gofer/internal/model"
	"github.com/Rememorio/gofer/internal/runtime"
)

const (
	toolSuppression = "The model provider stopped this response with a safety-related signal. Any tool calls produced in this turn were suppressed because their arguments may be truncated and unsafe to execute. Please rephrase the request or ask for a narrower output."
	emptyResponse   = "The model provider stopped this response with a safety-related signal and returned no content. Please rephrase your request or start a new conversation."
)

// ErrInvalidMiddleware identifies a nil safety response guard.
var ErrInvalidMiddleware = errors.New("invalid safety finish middleware")

// Middleware converts normalized content-filter stops into visible, tool-free
// terminal responses while preserving provider usage.
type Middleware struct{ runtime.NopMiddleware }

var (
	_ runtime.Middleware               = (*Middleware)(nil)
	_ runtime.ModelResponseTransformer = (*Middleware)(nil)
)

// New constructs a stateless provider-safety response guard.
func New() *Middleware { return &Middleware{} }

// TransformModelResponse suppresses every safety-capped tool call before
// validation, journaling, loop accounting, or execution.
func (middleware *Middleware) TransformModelResponse(ctx context.Context, response model.Response) (model.Response, error) {
	if middleware == nil {
		return model.Response{}, ErrInvalidMiddleware
	}
	if err := ctx.Err(); err != nil {
		return model.Response{}, err
	}
	if response.StopReason != model.StopContentFilter {
		return response, nil
	}
	hadTools := len(response.ToolCalls) != 0
	response.ToolCalls = nil
	response.StopReason = model.StopSafetyCapped
	if hadTools {
		response.Text = appendText(response.Text, toolSuppression)
	} else if strings.TrimSpace(response.Text) == "" {
		response.Text = emptyResponse
	}
	return response, nil
}

func appendText(existing, addition string) string {
	if existing == "" {
		return addition
	}
	return fmt.Sprintf("%s\n\n%s", existing, addition)
}
