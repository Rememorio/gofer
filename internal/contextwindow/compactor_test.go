package contextwindow

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/model"
)

type fixedEstimator int

func (estimator fixedEstimator) Tokens(domain.Message) int { return int(estimator) }

type summaryFunc func(context.Context, []domain.Message) (string, error)

func (function summaryFunc) Summarize(ctx context.Context, messages []domain.Message) (string, error) {
	return function(ctx, messages)
}

func TestCompactorSummarizesAtSafeBoundary(t *testing.T) {
	t.Parallel()
	messages := []domain.Message{message(t, domain.RoleUser, "one"), assistantCall(t), toolResult(t), message(t, domain.RoleUser, "latest")}
	var summarized []domain.Message
	compactor, err := New(Config{MaxTokens: 35, ReserveTokens: 5, MinRecentMessages: 2,
		MaxSummaryCharacters: 8, Estimator: fixedEstimator(10), Now: func() time.Time { return time.Unix(10, 0) },
		Summarizer: summaryFunc(func(_ context.Context, messages []domain.Message) (string, error) {
			summarized = messages
			return "1234567890", nil
		})})
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	request := model.Request{Messages: messages}
	if err := compactor.BeforeModel(context.Background(), &request); err != nil {
		t.Fatalf("BeforeModel(): %v", err)
	}
	if len(summarized) != 1 || len(request.Messages) != 4 {
		t.Fatalf("summarized=%d result=%d", len(summarized), len(request.Messages))
	}
	if request.Messages[0].Metadata["gofer.context"] != "summary" || !strings.Contains(request.Messages[0].Content[0].Text, "12345678") {
		t.Fatalf("summary = %#v", request.Messages[0])
	}
	if request.Messages[1].Role != domain.RoleAssistant || request.Messages[2].Role != domain.RoleTool {
		t.Fatal("tool exchange was split")
	}
	if err := request.Messages[0].Validate(); err != nil {
		t.Fatalf("summary Validate(): %v", err)
	}
}

func TestCompactorNoopAndFailures(t *testing.T) {
	t.Parallel()
	valid := Config{MaxTokens: 100, ReserveTokens: 10, MinRecentMessages: 1, MaxSummaryCharacters: 100,
		Summarizer: summaryFunc(func(context.Context, []domain.Message) (string, error) { return "summary", nil })}
	compactor, err := New(valid)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	request := model.Request{Messages: []domain.Message{message(t, domain.RoleUser, "short")}}
	if err := compactor.BeforeModel(context.Background(), &request); err != nil {
		t.Fatalf("BeforeModel(noop): %v", err)
	}
	if len(request.Messages) != 1 {
		t.Fatal("noop changed messages")
	}
	if err := (*Compactor)(nil).BeforeModel(context.Background(), &request); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil error = %v", err)
	}
	if err := compactor.BeforeModel(context.Background(), nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil request = %v", err)
	}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.MaxTokens = 0 }, func(c *Config) { c.ReserveTokens = -1 },
		func(c *Config) { c.ReserveTokens = c.MaxTokens }, func(c *Config) { c.MinRecentMessages = 0 },
		func(c *Config) { c.MaxSummaryCharacters = 0 }, func(c *Config) { c.Summarizer = nil },
	} {
		candidate := valid
		mutate(&candidate)
		if _, err := New(candidate); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("New(invalid) = %v", err)
		}
	}
}

func TestCompactorReportsBoundaryAndSummaryErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		summary summaryFunc
		want    string
	}{
		{"failure", func(context.Context, []domain.Message) (string, error) { return "", errors.New("boom") }, "boom"},
		{"empty", func(context.Context, []domain.Message) (string, error) { return "  ", nil }, "empty summary"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			compactor, _ := New(Config{MaxTokens: 2, MinRecentMessages: 1, MaxSummaryCharacters: 10, Summarizer: test.summary, Estimator: fixedEstimator(2)})
			request := model.Request{Messages: []domain.Message{message(t, domain.RoleUser, "a"), message(t, domain.RoleUser, "b")}}
			if err := compactor.BeforeModel(context.Background(), &request); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	compactor, _ := New(Config{MaxTokens: 1, MinRecentMessages: 1, MaxSummaryCharacters: 10,
		Summarizer: summaryFunc(func(context.Context, []domain.Message) (string, error) { return "x", nil }), Estimator: fixedEstimator(2)})
	request := model.Request{Messages: []domain.Message{message(t, domain.RoleUser, "a")}}
	if err := compactor.BeforeModel(context.Background(), &request); err == nil || !strings.Contains(err.Error(), "no safe") {
		t.Fatalf("error = %v", err)
	}
}

func TestApproximateEstimatorAndHooks(t *testing.T) {
	t.Parallel()
	msg := message(t, domain.RoleUser, "hello")
	if (ApproximateEstimator{}).Tokens(msg) <= 12 {
		t.Fatal("token estimate too low")
	}
	compactor, _ := New(Config{MaxTokens: 10, MinRecentMessages: 1, MaxSummaryCharacters: 1,
		Summarizer: summaryFunc(func(context.Context, []domain.Message) (string, error) { return "x", nil })})
	if compactor.AfterModel(context.Background(), model.Response{}) != nil || compactor.BeforeTool(context.Background(), domain.ToolCall{}) != nil || compactor.AfterTool(context.Background(), domain.ToolCall{}, domain.ToolResult{}) != nil {
		t.Fatal("no-op hook failed")
	}
}

func message(t *testing.T, role domain.Role, text string) domain.Message {
	t.Helper()
	value, err := domain.NewTextMessage(role, text, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func assistantCall(t *testing.T) domain.Message {
	t.Helper()
	value := message(t, domain.RoleAssistant, "calling")
	value.Content = append(value.Content, domain.Content{Kind: domain.ContentToolCall, ToolCall: &domain.ToolCall{ID: "1", Name: "x", Arguments: []byte(`{}`)}})
	return value
}

func toolResult(t *testing.T) domain.Message {
	t.Helper()
	value := message(t, domain.RoleTool, "result")
	value.Content = []domain.Content{{Kind: domain.ContentToolResult, ToolResult: &domain.ToolResult{CallID: "1", Output: []byte(`{}`)}}}
	return value
}
