package contextwindow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/model"
)

// ErrInvalidConfig identifies malformed context-window dependencies or limits.
var ErrInvalidConfig = errors.New("invalid context-window configuration")

// Summarizer converts older conversation history into a durable synopsis.
type Summarizer interface {
	Summarize(context.Context, []domain.Message) (string, error)
}

// Estimator estimates normalized model tokens for a message.
type Estimator interface {
	Tokens(domain.Message) int
}

// Config controls conversation compaction.
type Config struct {
	MaxTokens            int
	ReserveTokens        int
	MinRecentMessages    int
	MaxSummaryCharacters int
	Summarizer           Summarizer
	Estimator            Estimator
	Now                  func() time.Time
}

// Compactor is runtime middleware that replaces old messages with a summary.
type Compactor struct {
	maxTokens int
	reserve   int
	minRecent int
	maxChars  int
	summarize Summarizer
	estimate  Estimator
	now       func() time.Time
}

// New validates config and constructs context-window middleware.
func New(config Config) (*Compactor, error) {
	if config.MaxTokens <= 0 || config.ReserveTokens < 0 || config.ReserveTokens >= config.MaxTokens ||
		config.MinRecentMessages < 1 || config.MaxSummaryCharacters < 1 || config.Summarizer == nil {
		return nil, fmt.Errorf("%w: positive limits and a summarizer are required", ErrInvalidConfig)
	}
	if config.Estimator == nil {
		config.Estimator = ApproximateEstimator{}
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Compactor{maxTokens: config.MaxTokens, reserve: config.ReserveTokens,
		minRecent: config.MinRecentMessages, maxChars: config.MaxSummaryCharacters,
		summarize: config.Summarizer, estimate: config.Estimator, now: config.Now}, nil
}

// BeforeModel compacts only when the estimated prompt exceeds its budget.
func (compactor *Compactor) BeforeModel(ctx context.Context, request *model.Request) error {
	if compactor == nil || request == nil {
		return fmt.Errorf("%w: compactor and request are required", ErrInvalidConfig)
	}
	budget := compactor.maxTokens - compactor.reserve
	if messageTokens(compactor.estimate, request.Messages) <= budget {
		return nil
	}
	split := compactionSplit(request.Messages, compactor.minRecent)
	if split == 0 {
		return fmt.Errorf("context exceeds %d-token budget with no safe compaction boundary", budget)
	}
	summary, err := compactor.summarize.Summarize(ctx, cloneMessages(request.Messages[:split]))
	if err != nil {
		return fmt.Errorf("summarize context: %w", err)
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return errors.New("summarize context: empty summary")
	}
	runes := []rune(summary)
	if len(runes) > compactor.maxChars {
		summary = string(runes[:compactor.maxChars])
	}
	message, err := domain.NewTextMessage(domain.RoleSystem,
		"<conversation_summary>\n"+summary+"\n</conversation_summary>", compactor.now())
	if err != nil {
		return err
	}
	message.Metadata = map[string]string{"gofer.context": "summary"}
	request.Messages = append([]domain.Message{message}, cloneMessages(request.Messages[split:])...)
	return nil
}

// AfterModel is a no-op middleware hook.
func (*Compactor) AfterModel(context.Context, model.Response) error { return nil }

// BeforeTool is a no-op middleware hook.
func (*Compactor) BeforeTool(context.Context, domain.ToolCall) error { return nil }

// AfterTool is a no-op middleware hook.
func (*Compactor) AfterTool(context.Context, domain.ToolCall, domain.ToolResult) error { return nil }

// ApproximateEstimator conservatively estimates tokens from serialized content.
type ApproximateEstimator struct{}

// Tokens estimates roughly one token per four bytes plus message overhead.
func (ApproximateEstimator) Tokens(message domain.Message) int {
	data, err := json.Marshal(message.Content)
	if err != nil {
		return 16
	}
	return 12 + (len(data)+3)/4
}

func messageTokens(estimator Estimator, messages []domain.Message) int {
	total := 0
	for _, message := range messages {
		total += max(0, estimator.Tokens(message))
	}
	return total
}

func compactionSplit(messages []domain.Message, minimumRecent int) int {
	if len(messages) <= minimumRecent {
		return 0
	}
	split := len(messages) - minimumRecent
	for split > 0 && messages[split].Role == domain.RoleTool {
		split--
	}
	if split > 0 && hasToolCall(messages[split-1]) {
		split--
	}
	return split
}

func hasToolCall(message domain.Message) bool {
	for _, content := range message.Content {
		if content.Kind == domain.ContentToolCall {
			return true
		}
	}
	return false
}

func cloneMessages(messages []domain.Message) []domain.Message {
	cloned := make([]domain.Message, len(messages))
	for index, message := range messages {
		cloned[index] = message
		cloned[index].Content = make([]domain.Content, len(message.Content))
		for contentIndex, content := range message.Content {
			cloned[index].Content[contentIndex] = cloneContent(content)
		}
		cloned[index].Metadata = cloneMap(message.Metadata)
	}
	return cloned
}

func cloneContent(content domain.Content) domain.Content {
	cloned := content
	if content.ToolCall != nil {
		call := *content.ToolCall
		call.Arguments = append([]byte(nil), content.ToolCall.Arguments...)
		cloned.ToolCall = &call
	}
	if content.ToolResult != nil {
		result := *content.ToolResult
		result.Output = append([]byte(nil), content.ToolResult.Output...)
		cloned.ToolResult = &result
	}
	return cloned
}

func cloneMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	cloned := make(map[string]string, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}
