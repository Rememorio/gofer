package terminalresponse

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/model"
	"github.com/Rememorio/gofer/internal/runtime"
)

const (
	recoveryPrompt = "<system_reminder>\nYour previous response after tool execution was empty. Review the tool results already present in the conversation and provide a concise, user-visible final response. Do not call another tool unless it is strictly necessary.\n</system_reminder>"
	fallbackText   = "The model completed the tool run but returned no final response, including after one automatic retry. Please try again or use a different model."
)

// ErrInvalidConfig identifies an unusable middleware dependency.
var ErrInvalidConfig = errors.New("invalid terminal response configuration")

// Config supplies deterministic construction dependencies.
type Config struct {
	Now func() time.Time
}

// DefaultConfig returns production terminal-response settings.
func DefaultConfig() Config { return Config{Now: time.Now} }

// Middleware spends at most one additional model turn to recover an empty
// post-tool response, then emits a visible error fallback.
type Middleware struct {
	runtime.NopMiddleware
	now       func() time.Time
	mu        sync.Mutex
	retryUsed bool
	pending   bool
	postTool  bool
}

var (
	_ runtime.Middleware               = (*Middleware)(nil)
	_ runtime.ModelResponseTransformer = (*Middleware)(nil)
)

// New validates configuration and constructs a per-run middleware.
func New(config Config) (*Middleware, error) {
	if config.Now == nil {
		return nil, ErrInvalidConfig
	}
	return &Middleware{now: config.Now}, nil
}

// BeforeModel records whether the active user turn contains a tool result and
// appends a pending recovery reminder only to the transient provider request.
func (middleware *Middleware) BeforeModel(ctx context.Context, request *model.Request) error {
	if middleware == nil || middleware.now == nil || request == nil {
		return fmt.Errorf("%w: middleware and request are required", ErrInvalidConfig)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	middleware.mu.Lock()
	middleware.postTool = hasCurrentTurnToolResult(request.Messages)
	pending := middleware.pending
	middleware.pending = false
	middleware.mu.Unlock()
	if !pending {
		return nil
	}
	message, err := domain.NewTextMessage(domain.RoleUser, recoveryPrompt, middleware.now())
	if err != nil {
		return fmt.Errorf("build terminal response reminder: %w", err)
	}
	message.Metadata = map[string]string{"internal_kind": "terminal_response_recovery"}
	request.Messages = append(append([]domain.Message(nil), request.Messages...), message)
	return nil
}

// TransformModelResponse requests one bounded retry for an empty post-tool
// terminal response. A repeated empty response becomes a visible run error.
func (middleware *Middleware) TransformModelResponse(ctx context.Context, response model.Response) (model.Response, error) {
	if middleware == nil || middleware.now == nil {
		return model.Response{}, ErrInvalidConfig
	}
	if err := ctx.Err(); err != nil {
		return model.Response{}, err
	}
	if response.StopReason != model.StopEndTurn || len(response.ToolCalls) != 0 || strings.TrimSpace(response.Text) != "" {
		return response, nil
	}
	middleware.mu.Lock()
	defer middleware.mu.Unlock()
	if !middleware.postTool {
		return response, nil
	}
	remaining, bounded := runtime.RemainingModelTurns(ctx)
	if !middleware.retryUsed && (!bounded || remaining > 0) {
		middleware.retryUsed = true
		middleware.pending = true
		return response, runtime.ErrRetryModelResponse
	}
	response.Text = fallbackText
	response.StopReason = model.StopTerminalError
	return response, nil
}

// Reset clears per-run recovery state for explicit middleware reuse.
func (middleware *Middleware) Reset() {
	if middleware == nil {
		return
	}
	middleware.mu.Lock()
	middleware.retryUsed = false
	middleware.pending = false
	middleware.postTool = false
	middleware.mu.Unlock()
}

func hasCurrentTurnToolResult(messages []domain.Message) bool {
	latestUser := -1
	for index, message := range messages {
		if message.Role == domain.RoleUser && message.Metadata["internal_kind"] == "" {
			latestUser = index
		}
	}
	if latestUser < 0 {
		return false
	}
	for _, message := range messages[latestUser+1:] {
		if message.Role == domain.RoleTool {
			return true
		}
	}
	return false
}
