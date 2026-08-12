// Package app assembles Gofer's durable runtime into a production HTTP service.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Rememorio/gofer/internal/artifact"
	"github.com/Rememorio/gofer/internal/auth"
	"github.com/Rememorio/gofer/internal/browser"
	"github.com/Rememorio/gofer/internal/config"
	"github.com/Rememorio/gofer/internal/contextwindow"
	"github.com/Rememorio/gofer/internal/control"
	"github.com/Rememorio/gofer/internal/conversation"
	"github.com/Rememorio/gofer/internal/delivery"
	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/event"
	"github.com/Rememorio/gofer/internal/feedback"
	"github.com/Rememorio/gofer/internal/gateway"
	"github.com/Rememorio/gofer/internal/guardrail"
	"github.com/Rememorio/gofer/internal/loopdetect"
	"github.com/Rememorio/gofer/internal/mcp"
	"github.com/Rememorio/gofer/internal/memory"
	"github.com/Rememorio/gofer/internal/model"
	"github.com/Rememorio/gofer/internal/model/providerfactory"
	"github.com/Rememorio/gofer/internal/modellength"
	"github.com/Rememorio/gofer/internal/observe"
	"github.com/Rememorio/gofer/internal/policy"
	"github.com/Rememorio/gofer/internal/readbeforewrite"
	"github.com/Rememorio/gofer/internal/runtime"
	"github.com/Rememorio/gofer/internal/safetyfinish"
	"github.com/Rememorio/gofer/internal/sandbox"
	"github.com/Rememorio/gofer/internal/scheduler"
	"github.com/Rememorio/gofer/internal/skill"
	"github.com/Rememorio/gofer/internal/store"
	"github.com/Rememorio/gofer/internal/store/sqlstore"
	"github.com/Rememorio/gofer/internal/subagent"
	"github.com/Rememorio/gofer/internal/terminalresponse"
	"github.com/Rememorio/gofer/internal/tool"
	"github.com/Rememorio/gofer/internal/tool/builtin"
	"github.com/Rememorio/gofer/internal/toolhistory"
	"github.com/Rememorio/gofer/internal/tooloutput"
	"github.com/Rememorio/gofer/internal/uploads"
	"github.com/Rememorio/gofer/internal/webresearch"
	"github.com/Rememorio/gofer/internal/workspace"
	"github.com/Rememorio/gofer/internal/workspacechange"
)

// Service owns shared adapters and active asynchronous runs.
type Service struct {
	ctx        context.Context
	cancel     context.CancelFunc
	config     config.Config
	store      store.Store
	closeStore io.Closer
	workspaces *workspace.Manager
	uploads    *uploads.Manager
	artifacts  *artifact.Catalog
	controls   *control.Service
	feedback   feedback.Store
	browser    *browser.Manager
	research   *webresearch.Client
	mcp        *mcp.Client
	skills     *skill.Catalog
	skillMount string
	memories   memory.Store
	scheduled  scheduler.Store
	scheduler  *scheduler.Engine
	providers  map[string]configuredProvider
	metrics    *observe.Registry
	handler    http.Handler
	logger     *slog.Logger
	resources  sync.Mutex

	mu         sync.Mutex
	active     map[domain.RunID]context.CancelFunc
	wait       sync.WaitGroup
	background sync.WaitGroup
	once       sync.Once
	err        error
}

type configuredProvider struct {
	provider model.Provider
	model    string
}

// New constructs a fully wired service without opening a network listener.
func New(parent context.Context, cfg config.Config, logger *slog.Logger) (*Service, error) {
	if parent == nil {
		return nil, errors.New("app: parent context is required")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	ctx, cancel := context.WithCancel(parent)
	service := &Service{ctx: ctx, cancel: cancel, config: cfg, artifacts: artifact.NewCatalog(), providers: make(map[string]configuredProvider), active: make(map[domain.RunID]context.CancelFunc), logger: logger}
	if err := service.open(); err != nil {
		cancel()
		_ = service.Close()
		return nil, err
	}
	return service, nil
}

func (service *Service) open() error {
	var err error
	service.store, service.closeStore, err = openStore(service.ctx, service.config.Storage)
	if err != nil {
		return err
	}
	if err = service.openUploads(); err != nil {
		return err
	}
	service.workspaces, err = workspace.NewManager(workspace.Config{
		Root: service.config.Workspace.Root, MaxReadBytes: service.config.Workspace.MaxReadBytes,
		MaxWriteBytes: service.config.Workspace.MaxWriteBytes, MaxUploadBytes: service.config.Workspace.MaxUploadBytes,
		InternalOutputDirectories: []string{service.config.ToolOutput.StorageSubdir},
	})
	if err != nil {
		return err
	}
	var controlStore control.Store = control.NewInMemory()
	if provider, ok := service.store.(interface{ ControlState() control.Store }); ok {
		controlStore = provider.ControlState()
	}
	service.controls, err = control.NewService(controlStore, time.Now)
	if err != nil {
		return err
	}
	service.feedback = feedback.NewInMemory()
	if provider, ok := service.store.(interface{ FeedbackState() feedback.Store }); ok {
		service.feedback = provider.FeedbackState()
	}
	if err = service.openProviders(); err != nil {
		return err
	}
	if err = service.openBrowser(); err != nil {
		return err
	}
	if err = service.openWebResearch(); err != nil {
		return err
	}
	if err = service.openAgentExtensions(); err != nil {
		return err
	}
	if err = service.openScheduler(); err != nil {
		return err
	}
	service.metrics, err = newMetrics()
	if err != nil {
		return err
	}
	if err = service.openHandler(); err != nil {
		return err
	}
	service.startScheduler()
	return nil
}

func (service *Service) openUploads() error {
	configuration := service.config.Uploads
	var converter uploads.Converter
	if configuration.AutoConvertDocuments {
		command, err := uploads.NewCommandConverter(configuration.ConverterCommand, configuration.MaxConvertedBytes)
		if err != nil {
			return err
		}
		converter = command
	}
	manager, err := uploads.New(uploads.Config{
		AutoConvert:       configuration.AutoConvertDocuments,
		ConversionTimeout: time.Duration(configuration.ConversionTimeoutSeconds) * time.Second,
		MaxConvertedBytes: configuration.MaxConvertedBytes,
		MaxContextFiles:   configuration.MaxContextFiles,
		MaxContextChars:   configuration.MaxContextChars,
		MaxOutlineEntries: configuration.MaxOutlineEntries,
		MaxPreviewLines:   configuration.MaxPreviewLines,
	}, converter)
	if err != nil {
		return err
	}
	service.uploads = manager
	return nil
}

func (service *Service) openAgentExtensions() error {
	if service.config.Memory.Enabled {
		service.memories = memory.NewInMemory()
		if provider, ok := service.store.(interface{ MemoryState() memory.Store }); ok {
			service.memories = provider.MemoryState()
		}
	}
	if service.config.Skills.Enabled {
		var state skill.StateStore
		if provider, ok := service.store.(interface{ SkillState() skill.StateStore }); ok {
			state = provider.SkillState()
		}
		catalog, err := skill.NewCatalog(skill.Config{
			Root: service.config.Skills.Root, MaxDocumentBytes: service.config.Skills.MaxDocumentBytes,
			MaxPackageBytes: service.config.Skills.MaxPackageBytes, State: state,
		})
		if err != nil {
			return err
		}
		if err = catalog.Refresh(service.ctx); err != nil {
			return fmt.Errorf("refresh skills: %w", err)
		}
		if err = catalog.Project(service.ctx, service.config.Skills.ProjectionRoot); err != nil {
			return fmt.Errorf("project skills: %w", err)
		}
		service.skills = catalog
		service.skillMount, err = filepath.Abs(service.config.Skills.ProjectionRoot)
		if err != nil {
			return fmt.Errorf("resolve skill projection: %w", err)
		}
	}
	if service.config.MCP.Enabled {
		client, err := mcp.New(mcp.Config{Servers: mcpServers(service.config.MCP.Servers)})
		if err != nil {
			return err
		}
		if err = client.Connect(service.ctx); err != nil {
			return err
		}
		service.mcp = client
	}
	return nil
}

func mcpServers(configs []config.MCPServerConfig) []mcp.ServerConfig {
	servers := make([]mcp.ServerConfig, len(configs))
	for index, server := range configs {
		servers[index] = mcp.ServerConfig{
			Name: server.Name, Transport: mcp.Transport(server.Transport), Command: server.Command,
			Arguments: append([]string(nil), server.Arguments...), Environment: server.Environment,
			WorkingDirectory: server.WorkingDirectory, URL: server.URL, Headers: server.Headers,
			AllowInsecureHTTP: server.AllowInsecureHTTP, DisableStandaloneSSE: server.DisableStandaloneSSE,
			MaxRetries: server.MaxRetries,
		}
	}
	return servers
}

func openStore(ctx context.Context, cfg config.StorageConfig) (store.Store, io.Closer, error) {
	if cfg.Driver == "memory" {
		return store.NewMemory(), nil, nil
	}
	if cfg.Driver == "sqlite" {
		if err := prepareSQLitePath(cfg.DSN); err != nil {
			return nil, nil, err
		}
	}
	database, err := sqlstore.Open(ctx, sqlstore.Config{Driver: sqlstore.Driver(cfg.Driver), DSN: cfg.DSN})
	if err != nil {
		return nil, nil, err
	}
	return database, database, nil
}

func prepareSQLitePath(dsn string) error {
	if dsn == ":memory:" || strings.HasPrefix(dsn, "file:") {
		return nil
	}
	directory := filepath.Dir(dsn)
	if directory == "." {
		return nil
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("prepare SQLite directory: %w", err)
	}
	return nil
}

func (service *Service) openProviders() error {
	if len(service.config.Models) == 0 {
		return errors.New("app: at least one model is required")
	}
	for _, cfg := range service.config.Models {
		provider, err := providerfactory.Open(providerfactory.Config{
			Provider: cfg.Provider, APIKey: cfg.APIKey, AuthToken: cfg.AuthToken,
			BaseURL: cfg.BaseURL, MaxTokens: cfg.MaxTokens,
		})
		if err != nil {
			return fmt.Errorf("open model %s: %w", cfg.Name, err)
		}
		service.providers[cfg.Name] = configuredProvider{provider: provider, model: cfg.Model}
	}
	return nil
}

func (service *Service) openBrowser() error {
	if !service.config.Browser.Enabled {
		return nil
	}
	guard, err := browser.NewURLGuard(browser.URLGuardConfig{AllowPrivateAddresses: service.config.Browser.AllowPrivateAddresses})
	if err != nil {
		return err
	}
	factory, err := browser.NewChromeFactory(browser.ChromeConfig{
		ExecutablePath: service.config.Browser.ExecutablePath, RemoteURL: service.config.Browser.RemoteURL,
		Headful: service.config.Browser.Headful, ViewportWidth: service.config.Browser.ViewportWidth,
		ViewportHeight: service.config.Browser.ViewportHeight,
		ActionTimeout:  time.Duration(service.config.Browser.ActionTimeoutSeconds) * time.Second,
	}, guard)
	if err != nil {
		return err
	}
	service.browser, err = browser.NewManager(service.ctx, browser.ManagerConfig{
		MaxSessions: service.config.Browser.MaxSessions,
		IdleTimeout: time.Duration(service.config.Browser.IdleTimeoutSeconds) * time.Second,
		Factory:     factory,
	})
	return err
}

func (service *Service) openWebResearch() error {
	web := service.config.Web
	if !web.Search.Enabled && !web.Fetch.Enabled {
		return nil
	}
	settings := webresearch.Config{}
	if web.Search.Enabled {
		settings.Search = &webresearch.SearchConfig{
			Provider: web.Search.Provider, APIKey: web.Search.APIKey, Endpoint: web.Search.Endpoint,
			MaxResults: web.Search.MaxResults, SafeSearch: web.Search.SafeSearch,
			Timeout:               time.Duration(web.Search.TimeoutSeconds) * time.Second,
			AllowPrivateAddresses: web.Search.AllowPrivateAddresses,
		}
	}
	if web.Fetch.Enabled {
		settings.Fetch = &webresearch.FetchConfig{
			MaxResponseBytes: web.Fetch.MaxResponseBytes, MaxContentCharacters: web.Fetch.MaxContentChars,
			MaxRedirects: web.Fetch.MaxRedirects, Timeout: time.Duration(web.Fetch.TimeoutSeconds) * time.Second,
			UserAgent: web.Fetch.UserAgent, AllowPrivateAddresses: web.Fetch.AllowPrivateAddresses,
		}
	}
	var err error
	service.research, err = webresearch.New(settings)
	return err
}

func (service *Service) openHandler() error {
	gatewayHandler, err := gateway.New(gateway.Config{
		Store: service.store, Starter: service, Canceller: service, Cleaner: service,
		OwnerResolver: requestUser,
	})
	if err != nil {
		return err
	}
	apiMux := http.NewServeMux()
	apiMux.Handle("/", gatewayHandler)
	service.schedulerRoutes(apiMux)
	service.conversationServiceRoutes(apiMux)
	service.resourceRoutes(apiMux)
	service.controlRoutes(apiMux)
	service.branchRoutes(apiMux)
	service.feedbackRoutes(apiMux)
	service.usageRoutes(apiMux)
	service.memoryRoutes(apiMux)
	var api http.Handler = apiMux
	if service.config.Auth.Enabled {
		authenticator, authErr := buildAuthenticator(service.config.Auth)
		if authErr != nil {
			return authErr
		}
		api, err = (auth.Middleware{Authenticator: authenticator, Policy: auth.GatewayPolicy, Next: api}).Handler()
		if err != nil {
			return err
		}
	}
	mux := http.NewServeMux()
	mux.Handle("/", api)
	mux.HandleFunc("GET /metrics", service.serveMetrics)
	service.handler = service.observeRequests(mux)
	return nil
}

// PrepareThreadDelete rejects deletion until all local run goroutines exit.
func (service *Service) PrepareThreadDelete(ctx context.Context, threadID domain.ThreadID) error {
	service.mu.Lock()
	active := make([]domain.RunID, 0, len(service.active))
	for runID := range service.active {
		active = append(active, runID)
	}
	service.mu.Unlock()
	for _, runID := range active {
		run, err := service.store.Run(ctx, runID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err == nil && run.ThreadID == threadID {
			return store.ErrConflict
		}
	}
	return nil
}

// CleanupThread removes non-store resources after durable thread deletion.
func (service *Service) CleanupThread(ctx context.Context, threadID domain.ThreadID, ownerID string) error {
	var cleanupErr error
	if service.browser != nil {
		if err := service.browser.CloseSession(string(threadID)); err != nil && !errors.Is(err, browser.ErrNotFound) {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	tasks, err := service.scheduled.List(ctx, ownerID)
	if err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
	} else {
		for _, task := range tasks {
			if task.ThreadID == string(threadID) {
				cleanupErr = errors.Join(cleanupErr, service.scheduled.Delete(ctx, task.ID, ownerID))
			}
		}
	}
	service.artifacts.RemoveThread(threadID)
	cleanupErr = errors.Join(cleanupErr, service.controls.Delete(ctx, threadID))
	cleanupErr = errors.Join(cleanupErr, service.feedback.DeleteThread(ctx, threadID))
	cleanupErr = errors.Join(cleanupErr, service.workspaces.Remove(threadID))
	return cleanupErr
}

func buildAuthenticator(cfg config.AuthConfig) (*auth.StaticTokens, error) {
	tokens := make([]auth.Token, 0, len(cfg.Tokens))
	for _, token := range cfg.Tokens {
		permissions := make([]auth.Permission, len(token.Permissions))
		for index, permission := range token.Permissions {
			permissions[index] = auth.Permission(permission)
		}
		tokens = append(tokens, auth.Token{Secret: token.Secret, Principal: auth.Principal{ID: token.PrincipalID, Permissions: permissions}})
	}
	return auth.NewStaticTokens(tokens)
}

// Handler returns the assembled HTTP API and metrics handler.
func (service *Service) Handler() http.Handler { return service.handler }

// Start launches a persisted run independently of the request context.
func (service *Service) Start(ctx context.Context, launch gateway.StartRequest) error {
	messages, settings, err := decodeLaunch(launch.Request, time.Now())
	if err != nil {
		service.failPending(launch, err)
		return nil
	}
	provider, err := service.selectProvider(settings.model)
	if err != nil {
		service.failPending(launch, err)
		return nil
	}
	runContext := service.ctx
	if principal, ok := auth.PrincipalFromContext(ctx); ok {
		runContext = auth.WithPrincipal(runContext, principal)
	}
	runContext, cancel := context.WithCancel(runContext)
	service.mu.Lock()
	if _, exists := service.active[launch.RunID]; exists {
		service.mu.Unlock()
		cancel()
		return store.ErrConflict
	}
	service.active[launch.RunID] = cancel
	service.wait.Add(1)
	service.mu.Unlock()
	go service.execute(runContext, launch, messages, settings, provider)
	return nil
}

func (service *Service) selectProvider(name string) (configuredProvider, error) {
	if name == "" || name == "lead_agent" {
		name = service.config.Models[0].Name
	}
	provider, ok := service.providers[name]
	if !ok {
		return configuredProvider{}, fmt.Errorf("invalid model alias %q", name)
	}
	return provider, nil
}

func (service *Service) execute(ctx context.Context, launch gateway.StartRequest, messages []domain.Message, settings runSettings, provider configuredProvider) {
	defer service.finishActive(launch.RunID)
	messages, err := service.prepareConversation(ctx, launch, messages)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			service.settlePending(launch, context.Canceled)
		} else {
			service.failPending(launch, err)
		}
		return
	}
	threadWorkspace, err := service.workspaces.Open(launch.ThreadID)
	if err != nil {
		service.failPending(launch, err)
		return
	}
	defer func() { _ = threadWorkspace.Close() }()
	baseline, captureErr := workspacechange.Capture(threadWorkspace, service.workspaceChangeLimits())
	if captureErr != nil {
		service.logger.Warn("workspace baseline capture failed", "run_id", launch.RunID, "error", captureErr)
	} else {
		defer func() { _ = baseline.Close() }()
	}
	deliveryTracker := delivery.NewTracker()
	registry, middleware, children, err := service.buildTools(threadWorkspace, launch, provider, deliveryTracker)
	if err != nil {
		service.failPending(launch, err)
		return
	}
	defer func() { _ = children.Close() }()
	if current := uploads.NewContextMiddleware(service.uploads.CurrentContext(threadWorkspace, settings.uploads)); current != nil {
		middleware = append(middleware, current)
	}
	finishHooks := []runtime.FinishHook{
		subagentFinishHook(children),
		service.deliveryFinishHook(threadWorkspace, baseline, deliveryTracker, launch.RunID),
	}
	if service.skills != nil {
		settings.system = strings.TrimSpace(settings.system + "\n\n" + service.skills.IndexPrompt())
	}
	runner, err := runtime.NewRunner(runtime.RunnerConfig{
		Store: service.store, Provider: provider.provider, Tools: registry, Middleware: middleware,
		FinishHooks: finishHooks,
		MaxTurns:    service.config.Runtime.MaxTurns, MaxParallelTools: service.config.Runtime.MaxParallelTools,
	})
	if err != nil {
		service.failPending(launch, err)
		return
	}
	started := time.Now()
	result, runErr := runner.Run(ctx, runtime.Request{
		RunID: launch.RunID, Model: provider.model, System: settings.system,
		Messages: messages, MaxTokens: settings.maxTokens, Temperature: settings.temperature,
		Caller: runtime.CallerLeadAgent,
	})
	if runErr != nil {
		service.settlePending(launch, runErr)
	}
	service.assignAutomaticTitle(launch.ThreadID, result.Messages, runErr)
	status := "failed"
	if runErr == nil {
		status = string(result.Run.Status)
	} else if errors.Is(runErr, context.Canceled) {
		status = string(domain.RunCancelled)
		service.logger.Warn("run stopped", "run_id", launch.RunID, "error", runErr)
	} else {
		service.logger.Error("run failed", "run_id", launch.RunID, "error", runErr)
	}
	_ = service.metrics.Add("gofer_runs_total", map[string]string{"status": status}, 1)
	_ = service.metrics.Observe("gofer_run_duration_seconds", map[string]string{"status": status}, time.Since(started).Seconds())
}

func (service *Service) prepareConversation(ctx context.Context, launch gateway.StartRequest, incoming []domain.Message) ([]domain.Message, error) {
	history, err := conversation.Load(ctx, service.store, launch.ThreadID)
	if err != nil {
		return nil, err
	}
	combined, additions := conversation.Merge(history, incoming)
	if err = conversation.PersistInputs(ctx, service.store, launch.ThreadID, launch.RunID, additions); err != nil {
		return nil, err
	}
	return combined, nil
}

func (service *Service) buildTools(threadWorkspace *workspace.Thread, launch gateway.StartRequest, provider configuredProvider, observers ...runtime.Middleware) (*tool.Registry, []runtime.Middleware, *subagent.Manager, error) {
	registry := tool.NewRegistry()
	if err := service.registerCoreTools(registry, threadWorkspace, launch); err != nil {
		return nil, nil, nil, err
	}
	if err := service.registerExtensionTools(registry, launch.ThreadID); err != nil {
		return nil, nil, nil, err
	}
	children, err := service.newSubagents(threadWorkspace, launch, provider, observers)
	if err != nil {
		return nil, nil, nil, err
	}
	if err = (subagent.Tools{Manager: children, ParentID: string(launch.RunID)}).Register(registry); err != nil {
		_ = children.Close()
		return nil, nil, nil, err
	}
	middleware, err := service.runtimeMiddleware(launch.ThreadID, provider, threadWorkspace, registry)
	if err != nil {
		_ = children.Close()
		return nil, nil, nil, err
	}
	middleware = append(middleware, observers...)
	return registry, middleware, children, nil
}

func (service *Service) registerCoreTools(registry *tool.Registry, threadWorkspace *workspace.Thread, launch gateway.StartRequest) error {
	if err := (builtin.WorkspaceTools{Workspace: threadWorkspace, Artifacts: service.artifacts, Uploads: service.uploads}).Register(registry); err != nil {
		return err
	}
	if err := (control.Tools{Service: service.controls, ThreadID: launch.ThreadID}).Register(registry); err != nil {
		return err
	}
	executor, err := service.commandExecutor(threadWorkspace)
	if err != nil {
		return err
	}
	commandTool, err := (sandbox.CommandTool{Executor: executor}).Tool()
	if err != nil {
		return err
	}
	if err = registry.Register(commandTool); err != nil {
		return err
	}
	if service.browser != nil {
		err = (browser.Tools{Manager: service.browser, Key: string(launch.ThreadID), Workspace: threadWorkspace, Artifacts: service.artifacts}).Register(registry)
		if err != nil {
			return err
		}
	}
	if service.research != nil {
		if err = (webresearch.Tools{Client: service.research}).Register(registry); err != nil {
			return err
		}
	}
	return nil
}

func (service *Service) registerExtensionTools(registry *tool.Registry, threadID domain.ThreadID) error {
	if service.mcp != nil {
		if err := service.mcp.Register(registry); err != nil {
			return err
		}
	}
	if service.skills != nil {
		if err := registry.RegisterAll(service.skills.DescribeTool(), service.skills.ReadTool()); err != nil {
			return err
		}
	}
	if service.memories != nil {
		return (memory.Tools{Store: service.memories, Scope: memoryScope(threadID)}).Register(registry)
	}
	return nil
}

func (service *Service) runtimeMiddleware(threadID domain.ThreadID, provider configuredProvider, threadWorkspace *workspace.Thread, registry *tool.Registry) ([]runtime.Middleware, error) {
	descriptors := mergeDescriptors(builtin.PolicyDescriptors(), control.PolicyDescriptors(), sandbox.PolicyDescriptors())
	if service.browser != nil {
		descriptors = mergeDescriptors(descriptors, browser.PolicyDescriptors())
	}
	if service.research != nil {
		descriptors = mergeDescriptors(descriptors, webresearch.PolicyDescriptors())
	}
	if service.memories != nil {
		descriptors = mergeDescriptors(descriptors, memory.PolicyDescriptors())
	}
	descriptors = mergeDescriptors(descriptors, subagent.PolicyDescriptors())
	authorizer, err := policy.NewStatic(policy.DecisionAllow)
	if err != nil {
		return nil, err
	}
	policyMiddleware, err := policy.NewMiddleware(authorizer, descriptors)
	if err != nil {
		return nil, err
	}
	budget, err := tooloutput.New(toolOutputConfig(service.config.ToolOutput), threadWorkspace)
	if err != nil {
		return nil, err
	}
	guardrailConfig := guardrail.DefaultConfig()
	guardrailConfig.RemoteTools = append(guardrailConfig.RemoteTools, registry.UntrustedOutputTools()...)
	guardrailMiddleware, err := guardrail.New(guardrailConfig)
	if err != nil {
		return nil, err
	}
	var fileGate *readbeforewrite.Middleware
	if service.config.ReadBeforeWrite.Enabled {
		fileGate, err = readbeforewrite.New(readbeforewrite.Config{
			Scope: string(threadID), Files: threadWorkspace,
		})
		if err != nil {
			return nil, err
		}
	}
	middleware := []runtime.Middleware{policyMiddleware, guardrailMiddleware, budget}
	if service.memories != nil {
		memoryMiddleware, memoryErr := memory.NewMiddleware(memory.MiddlewareConfig{
			Store: service.memories, Scope: memoryScope(threadID), Limit: service.config.Memory.Limit,
			MaxChars: service.config.Memory.MaxChars,
		})
		if memoryErr != nil {
			return nil, memoryErr
		}
		middleware = append(middleware, memoryMiddleware)
	}
	compactor, err := contextwindow.New(contextwindow.Config{
		MaxTokens: service.config.Runtime.MaxContextTokens, ReserveTokens: service.config.Runtime.ReserveTokens,
		MinRecentMessages:    service.config.Runtime.MinRecentMessages,
		MaxSummaryCharacters: service.config.Runtime.MaxSummaryChars,
		Summarizer:           modelSummarizer{provider: provider.provider, model: provider.model, maxTokens: min(4096, service.config.Runtime.ReserveTokens)},
	})
	if err != nil {
		return nil, err
	}
	middleware = append(middleware, compactor)
	if fileGate != nil {
		middleware = append(middleware, fileGate)
	}
	// Safety suppression precedes loop accounting so provider-truncated tool
	// intent never contributes to repetition state or reaches execution.
	middleware = append(middleware, safetyfinish.New())
	if service.config.LoopDetection.Enabled {
		loopGuard, loopErr := loopdetect.New(loopDetectionConfig(service.config.LoopDetection))
		if loopErr != nil {
			return nil, loopErr
		}
		middleware = append(middleware, loopGuard)
	}
	historyRepair, err := toolhistory.New(toolhistory.DefaultConfig())
	if err != nil {
		return nil, err
	}
	// Repair runs after context producers so the provider sees paired tool
	// history. Later response guards do not alter existing call/result pairs.
	middleware = append(middleware, historyRepair)
	middleware = append(middleware, modellength.New())
	terminalGuard, err := terminalresponse.New(terminalresponse.DefaultConfig())
	if err != nil {
		return nil, err
	}
	middleware = append(middleware, terminalGuard)
	return middleware, nil
}

func loopDetectionConfig(source config.LoopDetectionConfig) loopdetect.Config {
	overrides := make(map[string]loopdetect.FrequencyOverride, len(source.ToolOverrides))
	for name, override := range source.ToolOverrides {
		overrides[name] = loopdetect.FrequencyOverride{Warn: override.Warn, HardLimit: override.HardLimit}
	}
	return loopdetect.Config{
		WarnThreshold: source.WarnThreshold, HardLimit: source.HardLimit, WindowSize: source.WindowSize,
		ToolFrequencyWarn: source.ToolFrequencyWarn, ToolFrequencyLimit: source.ToolFrequencyLimit,
		ToolOverrides: overrides,
	}
}

func toolOutputConfig(source config.ToolOutputConfig) tooloutput.Config {
	return tooloutput.Config{
		Enabled: source.Enabled, ExternalizeMinChars: source.ExternalizeMinChars,
		PreviewHeadChars: source.PreviewHeadChars, PreviewTailChars: source.PreviewTailChars,
		FallbackMaxChars: source.FallbackMaxChars, FallbackHeadChars: source.FallbackHeadChars,
		FallbackTailChars: source.FallbackTailChars, StorageSubdir: source.StorageSubdir,
		ExemptTools: append([]string(nil), source.ExemptTools...), ToolOverrides: source.ToolOverrides,
	}
}

func memoryScope(threadID domain.ThreadID) memory.ScopeProvider {
	return func(ctx context.Context) (memory.Scope, error) {
		userID := "local"
		if principal, ok := auth.PrincipalFromContext(ctx); ok {
			userID = principal.ID
		}
		return memory.Scope{UserID: userID, ThreadID: string(threadID)}, nil
	}
}

func (service *Service) commandExecutor(threadWorkspace *workspace.Thread) (sandbox.Executor, error) {
	cfg := service.config.Sandbox
	mounts := sandbox.MountsFromWorkspace(threadWorkspace.ExecutionMounts())
	if service.skillMount != "" {
		mounts = append(mounts, sandbox.Mount{Source: service.skillMount, Target: skill.DefaultVirtualRoot, ReadOnly: true})
	}
	commonTimeout := time.Duration(cfg.CommandTimeoutSeconds) * time.Second
	maxTimeout := time.Duration(cfg.MaxTimeoutSeconds) * time.Second
	switch cfg.Driver {
	case "local":
		return sandbox.NewLocal(sandbox.LocalConfig{Mounts: mounts, AllowHostExecution: cfg.AllowHostExecution, MaxOutputBytes: cfg.MaxOutputBytes, MaxScriptBytes: cfg.MaxScriptBytes, CommandTimeout: commonTimeout, MaxTimeout: maxTimeout})
	case "docker":
		return sandbox.NewDocker(sandbox.DockerConfig{Binary: cfg.DockerBinary, Image: cfg.Image, Mounts: mounts, NetworkEnabled: cfg.NetworkEnabled, Memory: cfg.Memory, CPUs: cfg.CPUs, PIDsLimit: cfg.PIDsLimit, MaxOutputBytes: cfg.MaxOutputBytes, MaxScriptBytes: cfg.MaxScriptBytes, CommandTimeout: commonTimeout, MaxTimeout: maxTimeout})
	default:
		return nil, fmt.Errorf("app: sandbox driver %q is not available", cfg.Driver)
	}
}

func mergeDescriptors(groups ...map[string]policy.Descriptor) map[string]policy.Descriptor {
	merged := make(map[string]policy.Descriptor)
	for _, group := range groups {
		for name, descriptor := range group {
			merged[name] = descriptor
		}
	}
	return merged
}

func (service *Service) failPending(launch gateway.StartRequest, cause error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(service.ctx), 5*time.Second)
	defer cancel()
	run, err := service.store.TransitionRun(ctx, launch.RunID, domain.RunPending, domain.RunRunning, time.Now(), "")
	if err != nil {
		return
	}
	records, err := service.store.Events(ctx, launch.RunID, 0, 0)
	if err != nil {
		return
	}
	sequence := uint64(0)
	if len(records) > 0 {
		sequence = records[len(records)-1].Sequence
	}
	started, _ := event.NewDraft(launch.ThreadID, launch.RunID, event.RunStarted, run.StartedAt, map[string]any{"attempt": run.Attempt})
	committed, err := service.store.Append(ctx, launch.RunID, sequence, started)
	if err != nil {
		return
	}
	sequence = service.appendEmptyDelivery(ctx, launch, committed[len(committed)-1].Sequence)
	failed, err := service.store.TransitionRun(ctx, launch.RunID, domain.RunRunning, domain.RunFailed, time.Now(), cause.Error())
	if err != nil {
		return
	}
	draft, _ := event.NewDraft(launch.ThreadID, launch.RunID, event.RunFailed, failed.FinishedAt, map[string]string{"error": cause.Error()})
	_, _ = service.store.Append(ctx, launch.RunID, sequence, draft)
}

func (service *Service) settlePending(launch gateway.StartRequest, cause error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(service.ctx), 5*time.Second)
	defer cancel()
	run, err := service.store.Run(ctx, launch.RunID)
	if err != nil || run.Status != domain.RunPending {
		return
	}
	if !errors.Is(cause, context.Canceled) && !errors.Is(cause, context.DeadlineExceeded) {
		service.failPending(launch, cause)
		return
	}
	run, err = service.store.TransitionRun(ctx, launch.RunID, domain.RunPending, domain.RunCancelled, time.Now(), "")
	if err != nil {
		return
	}
	records, err := service.store.Events(ctx, launch.RunID, 0, 0)
	if err != nil {
		return
	}
	sequence := uint64(0)
	if len(records) > 0 {
		sequence = records[len(records)-1].Sequence
	}
	sequence = service.appendEmptyDelivery(ctx, launch, sequence)
	draft, _ := event.NewDraft(launch.ThreadID, launch.RunID, event.RunCancelled, run.FinishedAt, map[string]string{"error": cause.Error()})
	_, _ = service.store.Append(ctx, launch.RunID, sequence, draft)
}

func (service *Service) appendEmptyDelivery(ctx context.Context, launch gateway.StartRequest, sequence uint64) uint64 {
	payload := delivery.EventPayload{Category: delivery.EventCategory, Content: delivery.EmptyReceipt()}
	draft, err := event.NewDraft(launch.ThreadID, launch.RunID, event.RunDelivery, time.Now(), payload)
	if err != nil {
		return sequence
	}
	records, err := service.store.Append(ctx, launch.RunID, sequence, draft)
	if err != nil {
		service.logger.Warn("empty delivery receipt failed", "run_id", launch.RunID, "error", err)
		return sequence
	}
	return records[len(records)-1].Sequence
}

// Cancel requests cancellation of an active run, including a not-yet-started run.
func (service *Service) Cancel(ctx context.Context, id domain.RunID) error {
	run, err := service.store.Run(ctx, id)
	if err != nil {
		return err
	}
	if run.Terminal() {
		return store.ErrConflict
	}
	service.mu.Lock()
	cancel := service.active[id]
	service.mu.Unlock()
	if cancel != nil {
		cancel()
		return nil
	}
	if run.Status == domain.RunPending {
		launch := gateway.StartRequest{RunID: run.ID, ThreadID: run.ThreadID}
		service.settlePending(launch, context.Canceled)
		return nil
	}
	return store.ErrConflict
}

func (service *Service) finishActive(id domain.RunID) {
	service.mu.Lock()
	if cancel := service.active[id]; cancel != nil {
		cancel()
		delete(service.active, id)
	}
	service.mu.Unlock()
	service.wait.Done()
}

// Close cancels active runs, closes browsers, and releases durable resources.
func (service *Service) Close() error {
	if service == nil {
		return nil
	}
	service.once.Do(func() {
		service.cancel()
		service.background.Wait()
		service.wait.Wait()
		if service.browser != nil {
			service.err = errors.Join(service.err, service.browser.Close())
		}
		if service.research != nil {
			service.research.Close()
		}
		if service.mcp != nil {
			service.err = errors.Join(service.err, service.mcp.Close())
		}
		if service.skills != nil {
			service.err = errors.Join(service.err, service.skills.RemoveProjection(service.config.Skills.ProjectionRoot))
		}
		if service.closeStore != nil {
			service.err = errors.Join(service.err, service.closeStore.Close())
		}
	})
	return service.err
}

func newMetrics() (*observe.Registry, error) {
	registry, err := observe.New(128)
	if err != nil {
		return nil, err
	}
	statuses := []string{"succeeded", "failed", "cancelled"}
	definitions := []observe.Definition{
		{Name: "gofer_http_requests_total", Help: "Total HTTP requests.", Kind: observe.Counter},
		{Name: "gofer_http_request_duration_seconds", Help: "HTTP request duration.", Kind: observe.Histogram, Buckets: []float64{0.01, 0.05, 0.1, 0.5, 1, 5, 30}},
		{Name: "gofer_runs_total", Help: "Completed agent runs.", Kind: observe.Counter, Labels: []string{"status"}, AllowedValues: map[string][]string{"status": statuses}},
		{Name: "gofer_run_duration_seconds", Help: "Agent run duration.", Kind: observe.Histogram, Labels: []string{"status"}, AllowedValues: map[string][]string{"status": statuses}, Buckets: []float64{0.1, 1, 5, 30, 120, 600, 3600}},
	}
	for _, definition := range definitions {
		if err = registry.Register(definition); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

func (service *Service) observeRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		started := time.Now()
		next.ServeHTTP(writer, request)
		_ = service.metrics.Add("gofer_http_requests_total", nil, 1)
		_ = service.metrics.Observe("gofer_http_request_duration_seconds", nil, time.Since(started).Seconds())
	})
}

func (service *Service) serveMetrics(writer http.ResponseWriter, _ *http.Request) {
	writer.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if err := service.metrics.WritePrometheus(writer); err != nil {
		http.Error(writer, "metrics unavailable", http.StatusInternalServerError)
	}
}

var _ gateway.ThreadCleaner = (*Service)(nil)

// Serve loads configuration, starts the HTTP listener, and shuts down gracefully.
func Serve(ctx context.Context, configPath string, output io.Writer) error {
	cfg, err := config.LoadFile(ctx, configPath)
	if err != nil {
		return err
	}
	level := slog.LevelInfo
	_ = level.UnmarshalText([]byte(cfg.LogLevel))
	logger := slog.New(slog.NewTextHandler(output, &slog.HandlerOptions{Level: level}))
	service, err := New(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer func() { _ = service.Close() }()
	server := &http.Server{Addr: cfg.Server.Address, Handler: service.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	errorsChannel := make(chan error, 1)
	go func() { errorsChannel <- server.ListenAndServe() }()
	logger.Info("gofer listening", "address", cfg.Server.Address)
	select {
	case err = <-errorsChannel:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		shutdownErr := server.Shutdown(shutdownContext)
		return errors.Join(ctx.Err(), shutdownErr)
	}
}
