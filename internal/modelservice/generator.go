package modelservice

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/model"
)

var (
	// ErrInvalidConfig identifies an unusable auxiliary model generator.
	ErrInvalidConfig = errors.New("invalid model service configuration")
	// ErrInvalidResponse identifies empty, tool-bearing, or unsafe model output.
	ErrInvalidResponse = errors.New("invalid model service response")

	thinkBlockPattern = regexp.MustCompile(`(?is)<think(?:\s[^>]*)?>.*?</think\s*>`)
)

// Generator makes short tool-free calls through one normalized provider.
type Generator struct {
	Provider model.Provider
	Model    string
	Now      func() time.Time
}

// Result contains cleaned text and exact provider usage.
type Result struct {
	Text  string
	Usage model.Usage
}

// Generate runs one bounded system-and-user request.
func (generator Generator) Generate(ctx context.Context, system, user string, maxTokens int) (Result, error) {
	if generator.Provider == nil || strings.TrimSpace(generator.Model) == "" ||
		strings.TrimSpace(system) == "" || strings.TrimSpace(user) == "" || maxTokens < 1 || maxTokens > 4096 {
		return Result{}, ErrInvalidConfig
	}
	if generator.Now == nil {
		generator.Now = time.Now
	}
	message, err := domain.NewTextMessage(domain.RoleUser, user, generator.Now())
	if err != nil {
		return Result{}, err
	}
	stream, err := generator.Provider.Stream(ctx, model.Request{
		Model: generator.Model, System: system, Messages: []domain.Message{message}, MaxTokens: maxTokens,
	})
	if err != nil {
		return Result{}, fmt.Errorf("open model service stream: %w", err)
	}
	response, collectErr := model.Collect(stream, nil)
	closeErr := stream.Close()
	if collectErr != nil || closeErr != nil {
		return Result{}, errors.Join(collectErr, closeErr)
	}
	text := CleanText(response.Text)
	if text == "" || len(response.ToolCalls) != 0 ||
		response.StopReason != model.StopEndTurn && response.StopReason != model.StopMaxTokens {
		return Result{}, fmt.Errorf("%w: stop reason %q, tool calls %d", ErrInvalidResponse, response.StopReason, len(response.ToolCalls))
	}
	return Result{Text: text, Usage: response.Usage}, nil
}

// CleanText removes complete reasoning blocks and a complete outer Markdown
// code fence while preserving unfinished content.
func CleanText(value string) string {
	value = strings.TrimSpace(thinkBlockPattern.ReplaceAllString(value, ""))
	if !strings.HasPrefix(value, "```") || !strings.HasSuffix(value, "```") {
		return value
	}
	firstLine := strings.IndexByte(value, '\n')
	if firstLine < 0 || firstLine+1 > len(value)-3 {
		return value
	}
	return strings.TrimSpace(value[firstLine+1 : len(value)-3])
}
