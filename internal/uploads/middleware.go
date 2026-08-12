package uploads

import (
	"context"
	"fmt"
	"strings"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/model"
	"github.com/Rememorio/gofer/internal/runtime"
)

// ContextMiddleware injects verified current-turn uploads into model requests
// without changing durable conversation messages.
type ContextMiddleware struct {
	runtime.NopMiddleware
	section string
}

// NewContextMiddleware returns nil when there is no current-upload section.
func NewContextMiddleware(section string) *ContextMiddleware {
	section = strings.TrimSpace(section)
	if section == "" {
		return nil
	}
	return &ContextMiddleware{section: section}
}

// BeforeModel prepends the bounded upload section to the latest user message.
func (middleware *ContextMiddleware) BeforeModel(ctx context.Context, request *model.Request) error {
	if middleware == nil || middleware.section == "" || request == nil {
		return fmt.Errorf("%w: upload context middleware and request are required", ErrInvalidConfig)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for index := len(request.Messages) - 1; index >= 0; index-- {
		if request.Messages[index].Role != domain.RoleUser {
			continue
		}
		message := request.Messages[index]
		message.Content = append([]domain.Content(nil), message.Content...)
		for contentIndex := range message.Content {
			if message.Content[contentIndex].Kind != domain.ContentText {
				continue
			}
			if strings.HasPrefix(message.Content[contentIndex].Text, middleware.section+"\n\n") {
				return nil
			}
			message.Content[contentIndex].Text = middleware.section + "\n\n" + message.Content[contentIndex].Text
			request.Messages[index] = message
			return nil
		}
		return nil
	}
	return nil
}
