package usage

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"github.com/Rememorio/gofer/internal/conversation"
	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/event"
	"github.com/Rememorio/gofer/internal/model"
	"github.com/Rememorio/gofer/internal/store"
)

const (
	// CallerLeadAgent identifies the main conversation agent.
	CallerLeadAgent = "lead_agent"
	// CallerSubagent identifies a delegated child agent.
	CallerSubagent = "subagent"
	// CallerMiddleware identifies an auxiliary model call.
	CallerMiddleware = "middleware"
)

// ModelBreakdown aggregates token count and parent-run count for one model.
type ModelBreakdown struct {
	Tokens int `json:"tokens"`
	Runs   int `json:"runs"`
}

// CallerBreakdown attributes total tokens by execution role.
type CallerBreakdown struct {
	LeadAgent  int `json:"lead_agent"`
	Subagent   int `json:"subagent"`
	Middleware int `json:"middleware"`
}

// ModelUsage captures detailed tokens for one model within one run.
type ModelUsage struct {
	InputTokens      int
	OutputTokens     int
	ReasoningTokens  int
	CacheReadTokens  int
	CacheWriteTokens int
}

// TotalTokens returns provider-billed headline tokens without double-counting
// reasoning or cache detail already represented by input/output totals.
func (usage ModelUsage) TotalTokens() int {
	return safeAdd(usage.InputTokens, usage.OutputTokens)
}

// RunSummary is usage derived from one immutable event journal.
type RunSummary struct {
	InputTokens      int
	OutputTokens     int
	ReasoningTokens  int
	CacheReadTokens  int
	CacheWriteTokens int
	LLMCallCount     int
	LeadAgentTokens  int
	SubagentTokens   int
	MiddlewareTokens int
	MessageCount     int
	StopReason       string
	Models           map[string]ModelUsage
	Synthetic        bool
}

// TotalTokens returns all lead, child, and middleware input/output tokens.
func (summary RunSummary) TotalTokens() int {
	return safeAdd(summary.InputTokens, summary.OutputTokens)
}

// ThreadSummary aggregates eligible runs using DeerFlow's response shape.
type ThreadSummary struct {
	ThreadID          domain.ThreadID           `json:"thread_id"`
	TotalTokens       int                       `json:"total_tokens"`
	TotalInputTokens  int                       `json:"total_input_tokens"`
	TotalOutputTokens int                       `json:"total_output_tokens"`
	TotalRuns         int                       `json:"total_runs"`
	ByModel           map[string]ModelBreakdown `json:"by_model"`
	ByCaller          CallerBreakdown           `json:"by_caller"`
}

// Summarize derives usage without trusting mutable run counters.
func Summarize(records []event.Event) RunSummary {
	summary := RunSummary{Models: make(map[string]ModelUsage)}
	summary.MessageCount = len(conversation.FromEvents(records))
	seenSubagents := make(map[string]struct{})
	for _, record := range records {
		switch record.Kind {
		case event.RunCreated:
			var payload struct {
				Synthetic string `json:"synthetic"`
			}
			if event.Decode(record, &payload) == nil && payload.Synthetic != "" {
				summary.Synthetic = true
			}
		case event.MessageCompleted:
			addMessageUsage(&summary, record)
		case event.ModelUsage:
			addMessageUsage(&summary, record)
		case event.ToolCompleted:
			addSubagentUsage(&summary, record, seenSubagents)
		case event.RunStarted, event.RunInterrupted, event.RunCompleted, event.RunFailed,
			event.RunCancelled, event.MessageStarted, event.MessageDelta, event.ToolStarted,
			event.ToolFailed, event.CheckpointSaved, event.WorkspaceChanges, event.RunDelivery:
		}
	}
	return summary
}

// Aggregate returns completed run usage, optionally including running progress.
func Aggregate(ctx context.Context, repository store.Store, threadID domain.ThreadID, includeActive bool) (ThreadSummary, error) {
	runs, err := repository.Runs(ctx, threadID)
	if err != nil {
		return ThreadSummary{}, err
	}
	result := ThreadSummary{ThreadID: threadID, ByModel: make(map[string]ModelBreakdown)}
	for _, run := range runs {
		if !eligible(run.Status, includeActive) {
			continue
		}
		records, eventsErr := repository.Events(ctx, run.ID, 0, 0)
		if eventsErr != nil {
			return ThreadSummary{}, eventsErr
		}
		summary := Summarize(records)
		if summary.Synthetic {
			continue
		}
		result.TotalRuns++
		result.TotalInputTokens = safeAdd(result.TotalInputTokens, summary.InputTokens)
		result.TotalOutputTokens = safeAdd(result.TotalOutputTokens, summary.OutputTokens)
		result.TotalTokens = safeAdd(result.TotalTokens, summary.TotalTokens())
		result.ByCaller.LeadAgent = safeAdd(result.ByCaller.LeadAgent, summary.LeadAgentTokens)
		result.ByCaller.Subagent = safeAdd(result.ByCaller.Subagent, summary.SubagentTokens)
		result.ByCaller.Middleware = safeAdd(result.ByCaller.Middleware, summary.MiddlewareTokens)
		for name, modelUsage := range summary.Models {
			breakdown := result.ByModel[name]
			breakdown.Tokens = safeAdd(breakdown.Tokens, modelUsage.TotalTokens())
			breakdown.Runs++
			result.ByModel[name] = breakdown
		}
	}
	return result, nil
}

func addMessageUsage(summary *RunSummary, record event.Event) {
	var payload struct {
		Usage      *model.Usage     `json:"usage"`
		Model      string           `json:"model"`
		Caller     string           `json:"caller"`
		StopReason model.StopReason `json:"stop_reason"`
	}
	if event.Decode(record, &payload) != nil || payload.Usage == nil || !validUsage(*payload.Usage) {
		return
	}
	caller := normalizeCaller(payload.Caller)
	modelName := strings.TrimSpace(payload.Model)
	if modelName == "" {
		modelName = "unknown"
	}
	add(summary, modelName, caller, *payload.Usage, 1)
	if payload.StopReason != "" {
		summary.StopReason = string(payload.StopReason)
	}
}

func addSubagentUsage(summary *RunSummary, record event.Event, seen map[string]struct{}) {
	var payload struct {
		Call   domain.ToolCall   `json:"call"`
		Result domain.ToolResult `json:"result"`
	}
	if event.Decode(record, &payload) != nil || payload.Result.IsError || !strings.HasPrefix(payload.Call.Name, "subagent_") {
		return
	}
	for _, metadata := range taskMetadata(payload.Result.Output) {
		runID := strings.TrimSpace(metadata["run_id"])
		if runID == "" {
			continue
		}
		if _, exists := seen[runID]; exists {
			continue
		}
		usage, calls, ok := metadataUsage(metadata)
		if !ok {
			continue
		}
		seen[runID] = struct{}{}
		modelName := strings.TrimSpace(metadata["model"])
		if modelName == "" {
			modelName = "unknown"
		}
		add(summary, modelName, CallerSubagent, usage, calls)
	}
}

func add(summary *RunSummary, modelName, caller string, next model.Usage, calls int) {
	summary.InputTokens = safeAdd(summary.InputTokens, next.InputTokens)
	summary.OutputTokens = safeAdd(summary.OutputTokens, next.OutputTokens)
	summary.ReasoningTokens = safeAdd(summary.ReasoningTokens, next.ReasoningTokens)
	summary.CacheReadTokens = safeAdd(summary.CacheReadTokens, next.CacheReadTokens)
	summary.CacheWriteTokens = safeAdd(summary.CacheWriteTokens, next.CacheWriteTokens)
	summary.LLMCallCount = safeAdd(summary.LLMCallCount, calls)
	total := safeAdd(next.InputTokens, next.OutputTokens)
	switch caller {
	case CallerSubagent:
		summary.SubagentTokens = safeAdd(summary.SubagentTokens, total)
	case CallerMiddleware:
		summary.MiddlewareTokens = safeAdd(summary.MiddlewareTokens, total)
	default:
		summary.LeadAgentTokens = safeAdd(summary.LeadAgentTokens, total)
	}
	current := summary.Models[modelName]
	current.InputTokens = safeAdd(current.InputTokens, next.InputTokens)
	current.OutputTokens = safeAdd(current.OutputTokens, next.OutputTokens)
	current.ReasoningTokens = safeAdd(current.ReasoningTokens, next.ReasoningTokens)
	current.CacheReadTokens = safeAdd(current.CacheReadTokens, next.CacheReadTokens)
	current.CacheWriteTokens = safeAdd(current.CacheWriteTokens, next.CacheWriteTokens)
	summary.Models[modelName] = current
}

func taskMetadata(raw json.RawMessage) []map[string]string {
	type task struct {
		Output struct {
			Metadata map[string]string `json:"metadata"`
		} `json:"output"`
	}
	var single task
	if json.Unmarshal(raw, &single) == nil && single.Output.Metadata != nil {
		return []map[string]string{single.Output.Metadata}
	}
	var many []task
	if json.Unmarshal(raw, &many) != nil {
		return nil
	}
	result := make([]map[string]string, 0, len(many))
	for _, item := range many {
		if item.Output.Metadata != nil {
			result = append(result, item.Output.Metadata)
		}
	}
	return result
}

func metadataUsage(metadata map[string]string) (model.Usage, int, bool) {
	values := []*int{}
	usage := model.Usage{}
	values = append(values, &usage.InputTokens, &usage.OutputTokens, &usage.ReasoningTokens, &usage.CacheReadTokens, &usage.CacheWriteTokens)
	keys := []string{"input_tokens", "output_tokens", "reasoning_tokens", "cache_read_tokens", "cache_write_tokens"}
	for index, key := range keys {
		value, err := strconv.Atoi(metadata[key])
		if err != nil || value < 0 {
			return model.Usage{}, 0, false
		}
		*values[index] = value
	}
	calls, err := strconv.Atoi(metadata["llm_call_count"])
	if err != nil || calls < 0 {
		return model.Usage{}, 0, false
	}
	return usage, calls, true
}

func normalizeCaller(caller string) string {
	switch strings.TrimSpace(caller) {
	case CallerSubagent:
		return CallerSubagent
	case CallerMiddleware:
		return CallerMiddleware
	default:
		return CallerLeadAgent
	}
}

func validUsage(usage model.Usage) bool {
	return usage.InputTokens >= 0 && usage.OutputTokens >= 0 && usage.ReasoningTokens >= 0 && usage.CacheReadTokens >= 0 && usage.CacheWriteTokens >= 0
}

func eligible(status domain.RunStatus, includeActive bool) bool {
	return status == domain.RunSucceeded || status == domain.RunFailed || includeActive && status == domain.RunRunning
}

func safeAdd(current, next int) int {
	if next > 0 && current > math.MaxInt-next {
		return math.MaxInt
	}
	return current + next
}
