package usage

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/event"
	"github.com/Rememorio/gofer/internal/model"
	"github.com/Rememorio/gofer/internal/store"
)

func TestSummarizeAttributesModelCallersAndSubagents(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	thread, _ := domain.NewThread(now)
	run, _ := domain.NewRun(thread.ID, now)
	user, _ := domain.NewTextMessage(domain.RoleUser, "question", now)
	lead, _ := domain.NewTextMessage(domain.RoleAssistant, "answer", now)
	middleware, _ := domain.NewTextMessage(domain.RoleAssistant, "summary", now)
	records := []event.Event{
		usageEvent(t, thread.ID, run.ID, event.RunCreated, 1, map[string]any{}),
		usageEvent(t, thread.ID, run.ID, event.MessageCompleted, 2, map[string]any{"message": user}),
		usageEvent(t, thread.ID, run.ID, event.MessageCompleted, 3, map[string]any{"message": lead, "model": "primary", "usage": model.Usage{InputTokens: 10, OutputTokens: 5, ReasoningTokens: 2}, "stop_reason": model.StopEndTurn}),
		usageEvent(t, thread.ID, run.ID, event.MessageCompleted, 4, map[string]any{"message": middleware, "model": "compact", "caller": CallerMiddleware, "usage": model.Usage{InputTokens: 2, OutputTokens: 1}}),
		usageEvent(t, thread.ID, run.ID, event.MessageCompleted, 5, map[string]any{"message": middleware, "usage": model.Usage{InputTokens: -1}}),
		usageToolEvent(t, thread.ID, run.ID, 6, childTaskOutput("child-run", "child", 7, 3, 2)),
		usageToolEvent(t, thread.ID, run.ID, 7, childTaskOutput("child-run", "child", 7, 3, 2)),
	}
	summary := Summarize(records)
	if summary.TotalTokens() != 28 || summary.InputTokens != 19 || summary.OutputTokens != 9 || summary.LLMCallCount != 4 {
		t.Fatalf("token summary = %#v", summary)
	}
	if summary.LeadAgentTokens != 15 || summary.MiddlewareTokens != 3 || summary.SubagentTokens != 10 {
		t.Fatalf("caller attribution = %#v", summary)
	}
	if summary.Models["primary"].TotalTokens() != 15 || summary.Models["compact"].TotalTokens() != 3 || summary.Models["child"].TotalTokens() != 10 {
		t.Fatalf("model attribution = %#v", summary.Models)
	}
	if summary.StopReason != string(model.StopEndTurn) || summary.MessageCount != 4 || summary.Synthetic {
		t.Fatalf("journal metadata = %#v", summary)
	}
}

func TestAggregateFiltersRunLifecycleAndSyntheticHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repository := store.NewMemory()
	now := time.Now().UTC()
	thread, _ := domain.NewThread(now)
	if err := repository.CreateThread(ctx, thread); err != nil {
		t.Fatal(err)
	}
	seedUsageRun(t, repository, thread.ID, domain.RunSucceeded, "primary", 2, 1, false)
	seedUsageRun(t, repository, thread.ID, domain.RunFailed, "primary", 3, 1, false)
	seedUsageRun(t, repository, thread.ID, domain.RunRunning, "active", 4, 1, false)
	seedUsageRun(t, repository, thread.ID, domain.RunCancelled, "cancelled", 5, 1, false)
	seedUsageRun(t, repository, thread.ID, domain.RunSucceeded, "synthetic", 100, 100, true)

	summary, err := Aggregate(ctx, repository, thread.ID, false)
	if err != nil || summary.TotalRuns != 2 || summary.TotalTokens != 7 || summary.ByModel["primary"].Runs != 2 || summary.ByCaller.LeadAgent != 7 {
		t.Fatalf("Aggregate() = %#v, %v", summary, err)
	}
	active, err := Aggregate(ctx, repository, thread.ID, true)
	if err != nil || active.TotalRuns != 3 || active.TotalTokens != 12 || active.ByModel["active"].Runs != 1 {
		t.Fatalf("Aggregate(active) = %#v, %v", active, err)
	}
	missing, _ := domain.NewThreadID()
	if _, err = Aggregate(ctx, repository, missing, false); err == nil {
		t.Fatal("Aggregate(missing thread) succeeded")
	}
}

func TestSummarizeCountsRetriedModelUsage(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	thread, _ := domain.NewThread(now)
	run, _ := domain.NewRun(thread.ID, now)
	answer, _ := domain.NewTextMessage(domain.RoleAssistant, "recovered", now)
	records := []event.Event{
		usageEvent(t, thread.ID, run.ID, event.ModelRetry, 1, map[string]any{
			"model": "primary", "caller": CallerLeadAgent,
			"usage": model.Usage{InputTokens: 4, OutputTokens: 1},
		}),
		usageEvent(t, thread.ID, run.ID, event.MessageCompleted, 2, map[string]any{
			"message": answer, "model": "primary", "caller": CallerLeadAgent,
			"usage": model.Usage{InputTokens: 3, OutputTokens: 2}, "stop_reason": model.StopEndTurn,
		}),
	}
	summary := Summarize(records)
	if summary.InputTokens != 7 || summary.OutputTokens != 3 || summary.LLMCallCount != 2 || summary.LeadAgentTokens != 10 {
		t.Fatalf("summary = %#v", summary)
	}
}

func TestUsageParsingRejectsMalformedMetadataAndSaturates(t *testing.T) {
	t.Parallel()
	if got := safeAdd(math.MaxInt-1, 2); got != math.MaxInt {
		t.Fatalf("safeAdd() = %d", got)
	}
	if normalizeCaller("unknown") != CallerLeadAgent || normalizeCaller(CallerSubagent) != CallerSubagent {
		t.Fatal("caller normalization failed")
	}
	if _, _, ok := metadataUsage(map[string]string{}); ok {
		t.Fatal("empty metadata parsed")
	}
	if metadata := taskMetadata(json.RawMessage(`not-json`)); metadata != nil {
		t.Fatalf("taskMetadata(malformed) = %#v", metadata)
	}
	list := `[{"output":{"metadata":{"run_id":"one"}}},{"output":{}}]`
	if metadata := taskMetadata(json.RawMessage(list)); len(metadata) != 1 || metadata[0]["run_id"] != "one" {
		t.Fatalf("taskMetadata(list) = %#v", metadata)
	}
}

func usageEvent(t *testing.T, threadID domain.ThreadID, runID domain.RunID, kind event.Kind, sequence uint64, payload any) event.Event {
	t.Helper()
	draft, err := event.NewDraft(threadID, runID, kind, time.Now().UTC(), payload)
	if err != nil {
		t.Fatal(err)
	}
	record, err := draft.Commit(sequence)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func usageToolEvent(t *testing.T, threadID domain.ThreadID, runID domain.RunID, sequence uint64, output json.RawMessage) event.Event {
	t.Helper()
	return usageEvent(t, threadID, runID, event.ToolCompleted, sequence, map[string]any{
		"call":   domain.ToolCall{ID: "call", Name: "subagent_wait", Arguments: json.RawMessage(`{"id":"child"}`)},
		"result": domain.ToolResult{CallID: "call", Output: output},
	})
}

func childTaskOutput(runID, modelName string, input, output, calls int) json.RawMessage {
	metadata := map[string]string{
		"run_id": runID, "model": modelName,
		"input_tokens":      strconv.Itoa(input),
		"output_tokens":     strconv.Itoa(output),
		"reasoning_tokens":  "0",
		"cache_read_tokens": "0", "cache_write_tokens": "0",
		"llm_call_count": strconv.Itoa(calls),
	}
	data, _ := json.Marshal(map[string]any{"output": map[string]any{"metadata": metadata}})
	return data
}

func seedUsageRun(t *testing.T, repository store.Store, threadID domain.ThreadID, status domain.RunStatus, modelName string, input, output int, synthetic bool) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	run, _ := domain.NewRun(threadID, now)
	if err := repository.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	createdPayload := map[string]any{}
	if synthetic {
		createdPayload["synthetic"] = "branch"
	}
	created, _ := event.NewDraft(threadID, run.ID, event.RunCreated, now, createdPayload)
	message, _ := event.NewDraft(threadID, run.ID, event.MessageCompleted, now, map[string]any{
		"model": modelName, "usage": model.Usage{InputTokens: input, OutputTokens: output},
	})
	if _, err := repository.Append(ctx, run.ID, 0, created, message); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.TransitionRun(ctx, run.ID, domain.RunPending, domain.RunRunning, now.Add(time.Second), ""); err != nil {
		t.Fatal(err)
	}
	if status == domain.RunRunning {
		return
	}
	failure := ""
	if status == domain.RunFailed {
		failure = "failed"
	}
	if _, err := repository.TransitionRun(ctx, run.ID, domain.RunRunning, status, now.Add(2*time.Second), failure); err != nil {
		t.Fatal(err)
	}
}
