package app

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/event"
	"github.com/Rememorio/gofer/internal/gateway"
	"github.com/Rememorio/gofer/internal/runtime"
	"github.com/Rememorio/gofer/internal/store"
	"github.com/Rememorio/gofer/internal/subagent"
	"github.com/Rememorio/gofer/internal/tool"
	"github.com/Rememorio/gofer/internal/workspace"
)

type childExecutor struct {
	service   *Service
	workspace *workspace.Thread
	launch    gateway.StartRequest
	provider  configuredProvider
	observers []runtime.Middleware
}

func subagentFinishHook(manager *subagent.Manager) runtime.FinishHook {
	return runtime.FinishFunc(func(context.Context, runtime.EventWriter) error {
		return manager.Close()
	})
}

func (service *Service) newSubagents(threadWorkspace *workspace.Thread, launch gateway.StartRequest, provider configuredProvider, observers []runtime.Middleware) (*subagent.Manager, error) {
	parallel := min(service.config.Runtime.MaxParallelTools, service.config.Runtime.MaxSubagents)
	return subagent.NewManager(service.ctx, subagent.Config{
		Executor: childExecutor{
			service: service, workspace: threadWorkspace, launch: launch, provider: provider,
			observers: append([]runtime.Middleware(nil), observers...),
		},
		MaxParallel: parallel, MaxChildren: service.config.Runtime.MaxSubagents,
		MaxDepth: service.config.Runtime.MaxSubagentDepth,
	})
}

func (executor childExecutor) Execute(ctx context.Context, request subagent.Request) (subagent.Output, error) {
	registry, middleware, err := executor.childTools()
	if err != nil {
		return subagent.Output{}, err
	}
	memoryStore := store.NewMemory()
	thread, err := domain.NewThread(executor.serviceTime())
	if err != nil {
		return subagent.Output{}, err
	}
	if err = memoryStore.CreateThread(ctx, thread); err != nil {
		return subagent.Output{}, err
	}
	run, err := domain.NewRun(thread.ID, executor.serviceTime())
	if err != nil {
		return subagent.Output{}, err
	}
	if err = memoryStore.CreateRun(ctx, run); err != nil {
		return subagent.Output{}, err
	}
	created, err := event.NewDraft(thread.ID, run.ID, event.RunCreated, executor.serviceTime(), map[string]any{"parent_run_id": executor.launch.RunID})
	if err != nil {
		return subagent.Output{}, err
	}
	if _, err = memoryStore.Append(ctx, run.ID, 0, created); err != nil {
		return subagent.Output{}, err
	}
	message, err := domain.NewTextMessage(domain.RoleUser, request.Prompt, executor.serviceTime())
	if err != nil {
		return subagent.Output{}, err
	}
	runner, err := runtime.NewRunner(runtime.RunnerConfig{
		Store: memoryStore, Provider: executor.provider.provider, Tools: registry, Middleware: middleware,
		MaxTurns:         executor.service.config.Runtime.MaxTurns,
		MaxParallelTools: executor.service.config.Runtime.MaxParallelTools,
	})
	if err != nil {
		return subagent.Output{}, err
	}
	system := "You are a delegated subagent. Complete the assigned task independently and return a concise, evidence-based result to the parent agent."
	if executor.service.skills != nil {
		system += "\n\n" + executor.service.skills.IndexPrompt()
	}
	result, err := runner.Run(ctx, runtime.Request{RunID: run.ID, Model: executor.provider.model, System: system, Messages: []domain.Message{message}, Caller: runtime.CallerSubagent})
	if err != nil {
		return subagent.Output{Metadata: childUsageMetadata(result, executor)}, err
	}
	text := finalAssistantText(result.Messages)
	if text == "" {
		return subagent.Output{}, errors.New("subagent completed without a text result")
	}
	return subagent.Output{Text: text, Metadata: childUsageMetadata(result, executor)}, nil
}

func childUsageMetadata(result runtime.Result, executor childExecutor) map[string]string {
	return map[string]string{
		"run_id":             string(result.Run.ID),
		"parent_run_id":      string(executor.launch.RunID),
		"model":              executor.provider.model,
		"caller":             runtime.CallerSubagent,
		"input_tokens":       strconv.Itoa(result.Usage.InputTokens),
		"output_tokens":      strconv.Itoa(result.Usage.OutputTokens),
		"reasoning_tokens":   strconv.Itoa(result.Usage.ReasoningTokens),
		"cache_read_tokens":  strconv.Itoa(result.Usage.CacheReadTokens),
		"cache_write_tokens": strconv.Itoa(result.Usage.CacheWriteTokens),
		"llm_call_count":     strconv.Itoa(result.Turns),
	}
}

func (executor childExecutor) childTools() (*tool.Registry, []runtime.Middleware, error) {
	registry := tool.NewRegistry()
	launch := executor.launch
	if err := executor.service.registerCoreTools(registry, executor.workspace, launch); err != nil {
		return nil, nil, err
	}
	if err := executor.service.registerExtensionTools(registry, executor.launch.ThreadID); err != nil {
		return nil, nil, err
	}
	middleware, err := executor.service.runtimeMiddleware(executor.launch.ThreadID, executor.provider)
	if err != nil {
		return nil, nil, err
	}
	middleware = append(middleware, executor.observers...)
	return registry, middleware, nil
}

func (executor childExecutor) serviceTime() time.Time { return time.Now().UTC() }

func finalAssistantText(messages []domain.Message) string {
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role != domain.RoleAssistant {
			continue
		}
		parts := make([]string, 0)
		for _, content := range messages[index].Content {
			if content.Kind == domain.ContentText {
				parts = append(parts, content.Text)
			}
		}
		return strings.TrimSpace(strings.Join(parts, ""))
	}
	return ""
}

var _ subagent.Executor = childExecutor{}
