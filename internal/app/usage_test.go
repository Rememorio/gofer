package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/event"
	"github.com/Rememorio/gofer/internal/model"
	"github.com/Rememorio/gofer/internal/usage"
)

func TestThreadTokenUsageAndRunEnrichmentHTTP(t *testing.T) {
	t.Parallel()
	modelServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer modelServer.Close()
	service, err := New(context.Background(), testConfig(t, modelServer.URL+"/v1"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = service.Close() }()
	server := httptest.NewServer(service.Handler())
	defer server.Close()
	threadID := createThread(t, server.URL, "")
	completed := seedAppUsageRun(t, service, threadID, domain.RunSucceeded, "primary", usage.CallerLeadAgent, 10, 5)
	seedAppUsageRun(t, service, threadID, domain.RunRunning, "compact", usage.CallerMiddleware, 2, 1)
	path := "/api/threads/" + string(threadID) + "/token-usage"
	assertThreadUsageResponses(t, server.URL, path)
	assertRunUsageResponses(t, server.URL, threadID, completed.ID)
	resourceRequest[map[string]string](t, server.URL, http.MethodGet, path+"?include_active=maybe", nil, "", http.StatusBadRequest)
	missing, _ := domain.NewThreadID()
	resourceRequest[map[string]string](t, server.URL, http.MethodGet, "/api/threads/"+string(missing)+"/token-usage", nil, "", http.StatusNotFound)
}

func assertThreadUsageResponses(t *testing.T, baseURL, path string) {
	t.Helper()
	summary := resourceRequest[threadUsageResponse](t, baseURL, http.MethodGet, path, nil, "", http.StatusOK)
	if summary.TotalRuns != 1 || summary.TotalTokens != 15 || summary.ByModel["primary"].Tokens != 15 || summary.ByCaller.LeadAgent != 15 {
		t.Fatalf("thread usage = %#v", summary)
	}
	if summary.ContextUsage == nil || summary.ContextUsage.TokenCount == 0 || summary.ContextUsage.MaxContextTokens == nil || summary.ContextUsage.Percentage == nil {
		t.Fatalf("context usage = %#v", summary.ContextUsage)
	}
	active := resourceRequest[threadUsageResponse](t, baseURL, http.MethodGet, path+"?include_active=true", nil, "", http.StatusOK)
	if active.TotalRuns != 2 || active.TotalTokens != 18 || active.ByCaller.Middleware != 3 || active.ByModel["compact"].Runs != 1 {
		t.Fatalf("active usage = %#v", active)
	}
}

func assertRunUsageResponses(t *testing.T, baseURL string, threadID domain.ThreadID, runID domain.RunID) {
	t.Helper()
	run := resourceRequest[usageRunResponse](t, baseURL, http.MethodGet,
		"/api/threads/"+string(threadID)+"/runs/"+string(runID), nil, "", http.StatusOK)
	if run.TotalInputTokens != 10 || run.TotalOutputTokens != 5 || run.TotalTokens != 15 || run.LLMCallCount != 1 || run.MessageCount != 1 || run.StopReason != string(model.StopEndTurn) {
		t.Fatalf("run usage = %#v", run)
	}
	runs := resourceRequest[[]usageRunResponse](t, baseURL, http.MethodGet,
		"/api/threads/"+string(threadID)+"/runs", nil, "", http.StatusOK)
	if len(runs) != 2 || runs[0].TotalTokens != 15 || runs[1].TotalTokens != 3 {
		t.Fatalf("run list usage = %#v", runs)
	}
}

type usageRunResponse struct {
	TotalInputTokens  int    `json:"total_input_tokens"`
	TotalOutputTokens int    `json:"total_output_tokens"`
	TotalTokens       int    `json:"total_tokens"`
	LLMCallCount      int    `json:"llm_call_count"`
	MessageCount      int    `json:"message_count"`
	StopReason        string `json:"stop_reason"`
}

func TestContextUsageHandlesUnknownCapacity(t *testing.T) {
	t.Parallel()
	response := contextUsage(250, 0)
	if response.TokenCount != 250 || response.MaxContextTokens != nil || response.Percentage != nil {
		t.Fatalf("contextUsage() = %#v", response)
	}
	request, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.test?include_active=1", nil)
	if include, err := usageIncludeActive(request); err != nil || !include {
		t.Fatalf("usageIncludeActive() = %v, %v", include, err)
	}
}

func seedAppUsageRun(t *testing.T, service *Service, threadID domain.ThreadID, status domain.RunStatus, modelName, caller string, input, output int) domain.Run {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	run, _ := domain.NewRun(threadID, now)
	if err := service.store.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	message, _ := domain.NewTextMessage(domain.RoleAssistant, modelName+" answer", now)
	created, _ := event.NewDraft(threadID, run.ID, event.RunCreated, now, nil)
	completed, _ := event.NewDraft(threadID, run.ID, event.MessageCompleted, now, map[string]any{
		"message": message, "model": modelName, "caller": caller,
		"usage": model.Usage{InputTokens: input, OutputTokens: output}, "stop_reason": model.StopEndTurn,
	})
	if _, err := service.store.Append(ctx, run.ID, 0, created, completed); err != nil {
		t.Fatal(err)
	}
	run, err := service.store.TransitionRun(ctx, run.ID, domain.RunPending, domain.RunRunning, now.Add(time.Second), "")
	if err != nil {
		t.Fatal(err)
	}
	if status == domain.RunRunning {
		return run
	}
	run, err = service.store.TransitionRun(ctx, run.ID, domain.RunRunning, status, now.Add(2*time.Second), "")
	if err != nil {
		t.Fatal(err)
	}
	return run
}
