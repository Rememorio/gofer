package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/Rememorio/gofer/internal/auth"
	"github.com/Rememorio/gofer/internal/channel"
	"github.com/Rememorio/gofer/internal/conversation"
	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/event"
	"github.com/Rememorio/gofer/internal/gateway"
	"github.com/Rememorio/gofer/internal/humaninput"
	"github.com/Rememorio/gofer/internal/memory"
	"github.com/Rememorio/gofer/internal/skill"
	"github.com/Rememorio/gofer/internal/store"
)

const (
	channelWaitPollInterval = 100 * time.Millisecond
	channelBootstrapPrompt  = "This is a bootstrap session. Help the user initialize the workspace and persistent agent profile. Prefer the enabled bootstrap skill when available."
)

type channelContextKey uint8

const (
	channelBootstrapContextKey channelContextKey = iota
	channelUserContextKey
	channelSkillContextKey
)

type channelSkillActivation struct {
	Name         string
	Instructions string
}

type channelDispatcher struct {
	service *Service
	state   channel.State
}

func (service *Service) openChannels() error {
	if !service.config.Channels.Enabled {
		return nil
	}
	state := channel.State(channel.NewMemoryState())
	if provider, ok := service.store.(interface{ ChannelState() channel.State }); ok {
		state = provider.ChannelState()
	}
	if err := service.bootstrapChannelBindings(state); err != nil {
		return err
	}
	manager, err := channel.NewManager(channel.Config{
		Resolver: state, Dispatcher: channelDispatcher{service: service, state: state}, Dedupe: state,
		Connector:   state,
		MaxInflight: service.config.Channels.MaxInflight, QueueCapacity: service.config.Channels.QueueCapacity,
		DedupeTTL:         time.Duration(service.config.Channels.DedupeTTLSeconds) * time.Second,
		UnauthorizedReply: service.config.Channels.UnauthorizedReply,
		OnError: func(message channel.Message, failure error) {
			if !errors.Is(failure, channel.ErrDuplicate) && !errors.Is(failure, context.Canceled) {
				service.logger.Warn("channel message failed", "provider", message.Provider, "message_id", message.ID, "error", failure)
			}
		},
	})
	if err != nil {
		return err
	}
	if err = service.registerChannels(manager); err != nil {
		_ = manager.Close()
		return err
	}
	if err = manager.Start(service.ctx); err != nil {
		_ = manager.Close()
		return err
	}
	service.channelState, service.channels = state, manager
	return nil
}

func (service *Service) bootstrapChannelBindings(state channel.State) error {
	for _, configured := range service.config.Channels.Bindings {
		binding, err := channel.NewBinding(configured.UserID, configured.Provider, configured.WorkspaceID, configured.ExternalUserID, time.Now())
		if err != nil {
			return err
		}
		binding.WorkspaceName, binding.ExternalUserName = configured.WorkspaceName, configured.ExternalUserName
		if _, err = state.Bind(service.ctx, binding); err != nil {
			return fmt.Errorf("bind configured channel identity: %w", err)
		}
	}
	if service.config.Channels.GitHub.Enabled {
		for _, subscription := range service.config.Channels.GitHub.Subscriptions {
			binding, bindingErr := channel.NewBinding(subscription.UserID, "github", subscription.Repository, subscription.ID, time.Now())
			if bindingErr != nil {
				return bindingErr
			}
			binding.WorkspaceName, binding.ExternalUserName = subscription.Repository, subscription.ID
			if _, bindingErr = state.Bind(service.ctx, binding); bindingErr != nil {
				return fmt.Errorf("bind GitHub subscription: %w", bindingErr)
			}
		}
	}
	return nil
}

func (service *Service) registerChannels(manager *channel.Manager) error {
	var err error
	if service.config.Channels.Webhook.Enabled {
		if err = service.openWebhookChannel(manager); err != nil {
			return err
		}
	}
	if err = service.openNativeChannels(manager); err != nil {
		return err
	}
	if err = service.openGitHubChannel(manager); err != nil {
		return err
	}
	return nil
}

func (service *Service) openNativeChannels(manager *channel.Manager) error {
	openers := []func(*channel.Manager) error{
		service.openSlackChannel, service.openTelegramChannel, service.openDiscordChannel,
		service.openFeishuChannel, service.openDingTalkChannel, service.openWeComChannel, service.openWeChatChannel,
		service.openBuzzChannel,
	}
	for _, open := range openers {
		if err := open(manager); err != nil {
			return err
		}
	}
	return nil
}

func (service *Service) openSlackChannel(manager *channel.Manager) error {
	configured := service.config.Channels.Slack
	if !configured.Enabled {
		return nil
	}
	provider, err := channel.NewSlack(channel.SlackConfig{
		BotToken: configured.BotToken, AppToken: configured.AppToken,
		BotUserID: configured.BotUserID, AllowedUsers: configured.AllowedUsers,
		RequestTimeout: time.Duration(configured.RequestTimeoutSeconds) * time.Second, MaxAttempts: configured.MaxAttempts,
	})
	return registerNativeChannel(manager, provider, err)
}

func (service *Service) openTelegramChannel(manager *channel.Manager) error {
	configured := service.config.Channels.Telegram
	if !configured.Enabled {
		return nil
	}
	provider, err := channel.NewTelegram(channel.TelegramConfig{
		BotToken: configured.BotToken, AllowedUsers: configured.AllowedUsers,
		PollTimeout:    time.Duration(configured.PollTimeoutSeconds) * time.Second,
		RequestTimeout: time.Duration(configured.RequestTimeoutSeconds) * time.Second, MaxAttempts: configured.MaxAttempts,
	})
	return registerNativeChannel(manager, provider, err)
}

func (service *Service) openDiscordChannel(manager *channel.Manager) error {
	configured := service.config.Channels.Discord
	if !configured.Enabled {
		return nil
	}
	provider, err := channel.NewDiscord(channel.DiscordConfig{
		BotToken: configured.BotToken, AllowedGuilds: configured.AllowedGuilds,
		AllowedChannels: configured.AllowedChannels, MentionOnly: configured.MentionOnly, ThreadMode: configured.ThreadMode,
		RequestTimeout: time.Duration(configured.RequestTimeoutSeconds) * time.Second, MaxAttempts: configured.MaxAttempts,
	})
	return registerNativeChannel(manager, provider, err)
}

func (service *Service) openFeishuChannel(manager *channel.Manager) error {
	configured := service.config.Channels.Feishu
	if !configured.Enabled {
		return nil
	}
	provider, err := channel.NewFeishu(channel.FeishuConfig{
		AppID: configured.AppID, AppSecret: configured.AppSecret, Domain: configured.Domain, AllowedUsers: configured.AllowedUsers,
		RequestTimeout: time.Duration(configured.RequestTimeoutSeconds) * time.Second, MaxAttempts: configured.MaxAttempts,
	})
	return registerNativeChannel(manager, provider, err)
}

func (service *Service) openDingTalkChannel(manager *channel.Manager) error {
	configured := service.config.Channels.DingTalk
	if !configured.Enabled {
		return nil
	}
	provider, err := channel.NewDingTalk(channel.DingTalkConfig{
		ClientID: configured.ClientID, ClientSecret: configured.ClientSecret, AllowedUsers: configured.AllowedUsers,
		RequestTimeout: time.Duration(configured.RequestTimeoutSeconds) * time.Second, MaxAttempts: configured.MaxAttempts,
	})
	return registerNativeChannel(manager, provider, err)
}

func (service *Service) openWeComChannel(manager *channel.Manager) error {
	configured := service.config.Channels.WeCom
	if !configured.Enabled {
		return nil
	}
	provider, err := channel.NewWeCom(channel.WeComConfig{
		BotID: configured.BotID, BotSecret: configured.BotSecret, WorkingMessage: configured.WorkingMessage,
		AllowedUsers: configured.AllowedUsers, Heartbeat: time.Duration(configured.HeartbeatSeconds) * time.Second,
		RequestTimeout: time.Duration(configured.RequestTimeoutSeconds) * time.Second, MaxAttempts: configured.MaxAttempts,
	})
	return registerNativeChannel(manager, provider, err)
}

func (service *Service) openWeChatChannel(manager *channel.Manager) error {
	configured := service.config.Channels.WeChat
	if !configured.Enabled {
		return nil
	}
	provider, err := channel.NewWeChat(channel.WeChatConfig{
		BotToken: configured.BotToken, ILinkBotID: configured.ILinkBotID, ILinkAppID: configured.ILinkAppID,
		RouteTag: configured.RouteTag, ChannelVersion: configured.ChannelVersion, AllowedUsers: configured.AllowedUsers,
		PollTimeout:    time.Duration(configured.PollTimeoutSeconds) * time.Second,
		RequestTimeout: time.Duration(configured.RequestTimeoutSeconds) * time.Second, MaxAttempts: configured.MaxAttempts,
	})
	return registerNativeChannel(manager, provider, err)
}

func (service *Service) openGitHubChannel(manager *channel.Manager) error {
	configured := service.config.Channels.GitHub
	if !configured.Enabled {
		return nil
	}
	subscriptions := make([]channel.GitHubSubscription, 0, len(configured.Subscriptions))
	for _, subscription := range configured.Subscriptions {
		triggers := make(map[string]channel.GitHubTrigger, len(subscription.Triggers))
		for eventName, trigger := range subscription.Triggers {
			triggers[eventName] = channel.GitHubTrigger{
				Actions: trigger.Actions, RequireMention: trigger.RequireMention,
				MentionLogin: trigger.MentionLogin, AllowAuthors: trigger.AllowAuthors,
			}
		}
		subscriptions = append(subscriptions, channel.GitHubSubscription{
			ID: subscription.ID, Repository: subscription.Repository, AssistantID: subscription.AssistantID,
			BotLogin: subscription.BotLogin, DefaultMentionLogin: configured.DefaultMentionLogin, Triggers: triggers,
		})
	}
	provider, err := channel.NewGitHub(channel.GitHubConfig{
		Manager: manager, Secret: configured.WebhookSecret, AllowUnverified: configured.AllowUnverified,
		MaxBodyBytes: configured.MaxBodyBytes, Subscriptions: subscriptions,
	})
	if err != nil {
		return err
	}
	if err = manager.Register(provider); err != nil {
		return errors.Join(err, provider.Close())
	}
	service.githubWebhook = provider
	return nil
}

func (service *Service) openBuzzChannel(manager *channel.Manager) error {
	configured := service.config.Channels.Buzz
	if !configured.Enabled {
		return nil
	}
	provider, err := channel.NewBuzz(channel.BuzzConfig{
		RelayURL: configured.RelayURL, PrivateKey: configured.PrivateKey, RelayPublicKey: configured.RelayPublicKey,
		AllowedUsers: configured.AllowedUsers, RequireMention: configured.RequireMention,
		MentionFreeChannels: configured.MentionFreeChannels,
		RequestTimeout:      time.Duration(configured.RequestTimeoutSeconds) * time.Second, MaxAttempts: configured.MaxAttempts,
	})
	return registerNativeChannel(manager, provider, err)
}

func registerNativeChannel(manager *channel.Manager, provider channel.Sender, err error) error {
	if err != nil {
		return err
	}
	if err = manager.Register(provider); err != nil {
		_ = provider.Close()
		return err
	}
	return nil
}

func (service *Service) openWebhookChannel(manager *channel.Manager) error {
	configured := service.config.Channels.Webhook
	sender, err := channel.NewWebhookSender(channel.WebhookSenderConfig{
		Endpoint: configured.OutboundURL, Secret: configured.Secret,
		Timeout: time.Duration(configured.TimeoutSeconds) * time.Second, MaxAttempts: configured.MaxAttempts,
		AllowPrivateAddresses: configured.AllowPrivateAddresses,
	})
	if err != nil {
		return err
	}
	if err = manager.Register(sender); err != nil {
		_ = sender.Close()
		return err
	}
	handler, err := channel.NewWebhookHandler(channel.WebhookHandlerConfig{
		Manager: manager, Secret: configured.Secret, MaxBodyBytes: configured.MaxBodyBytes,
		ClockSkew: time.Duration(configured.ClockSkewSeconds) * time.Second,
	})
	if err != nil {
		return err
	}
	service.channelWebhook = handler
	return nil
}

func (dispatcher channelDispatcher) Dispatch(ctx context.Context, request channel.Request) (channel.Reply, error) {
	if command, ok := channel.ParseCommand(request.Message.Text); ok {
		return dispatcher.dispatchCommand(ctx, request, command)
	}
	commandText := channel.StripLeadingMentions(request.Message.Text)
	if strings.HasPrefix(commandText, "/") {
		request.Message.Text = commandText
		return dispatcher.dispatchSlashSkill(ctx, request)
	}
	return dispatcher.dispatchAgent(ctx, request)
}

func (dispatcher channelDispatcher) dispatchAgent(ctx context.Context, request channel.Request) (channel.Reply, error) {
	threadID, err := dispatcher.ensureThread(ctx, request)
	if err != nil {
		return channel.Reply{}, err
	}
	if err = dispatcher.ensureThreadIdle(ctx, threadID); err != nil {
		return channel.Reply{}, err
	}
	run, err := domain.NewRun(threadID, time.Now())
	if err == nil {
		err = dispatcher.service.store.CreateRun(ctx, run)
	}
	if err != nil {
		return channel.Reply{}, err
	}
	runMetadata := map[string]any{
		"source": "channel", "provider": request.Message.Provider, "message_id": request.Message.ID,
		"channel_user_id": request.Message.ExternalUserID,
	}
	if channelBootstrap(ctx) {
		runMetadata["is_bootstrap"] = true
	}
	if activation, ok := channelSkill(ctx); ok {
		runMetadata["skill"] = activation.Name
	}
	draft, err := event.NewDraft(threadID, run.ID, event.RunCreated, time.Now(), cloneAnyMap(runMetadata))
	if err == nil {
		_, err = dispatcher.service.store.Append(ctx, run.ID, 0, draft)
	}
	if err != nil {
		return channel.Reply{}, err
	}
	input, err := channelRunInput(request.Message)
	if err != nil {
		return channel.Reply{}, err
	}
	assistantID := channelAssistantID(request.Message, dispatcher.service.config.Models[0].Name)
	launch := gateway.StartRequest{
		RunID: run.ID, ThreadID: threadID,
		Request: gateway.RunRequest{
			AssistantID: assistantID, Input: input,
			Metadata: cloneAnyMap(runMetadata),
		},
	}
	launchContext := auth.WithPrincipal(ctx, auth.Principal{ID: request.Identity.UserID, Permissions: []auth.Permission{auth.Admin}})
	launchContext = context.WithValue(launchContext, channelUserContextKey, request.Message.ExternalUserID)
	if err = dispatcher.service.Start(launchContext, launch); err != nil {
		dispatcher.service.failPending(launch, err)
		return channel.Reply{}, err
	}
	run, err = waitChannelRun(ctx, dispatcher.service.store, run.ID)
	if err != nil {
		return channel.Reply{}, err
	}
	return dispatcher.reply(ctx, run)
}

func (dispatcher channelDispatcher) dispatchCommand(ctx context.Context, request channel.Request, command channel.Command) (channel.Reply, error) {
	switch command.Name {
	case channel.CommandBootstrap:
		request.Message.Text = command.Args
		if request.Message.Text == "" {
			request.Message.Text = "Initialize workspace"
		}
		return dispatcher.dispatchAgent(context.WithValue(ctx, channelBootstrapContextKey, true), request)
	case channel.CommandNew:
		_, err := dispatcher.freshThread(ctx, request)
		if err != nil {
			return channel.Reply{}, err
		}
		return channel.Reply{Text: "New conversation started."}, nil
	case channel.CommandStatus:
		threadID, found, err := dispatcher.mappedThread(ctx, request)
		if err != nil {
			return channel.Reply{}, err
		}
		if !found {
			return channel.Reply{Text: "No active conversation."}, nil
		}
		return channel.Reply{Text: "Active thread: " + string(threadID)}, nil
	case channel.CommandModels:
		return channel.Reply{Text: dispatcher.modelList()}, nil
	case channel.CommandMemory:
		return dispatcher.memoryStatus(ctx, request.Identity.UserID)
	case channel.CommandGoal:
		return dispatcher.goalCommand(ctx, request, command.Args)
	case channel.CommandHelp:
		return channel.Reply{Text: channelCommandHelp}, nil
	default:
		return channel.Reply{Text: unknownChannelCommand(request.Message.Text)}, nil
	}
}

func (dispatcher channelDispatcher) dispatchSlashSkill(ctx context.Context, request channel.Request) (channel.Reply, error) {
	fields := strings.Fields(request.Message.Text)
	if len(fields) == 0 || dispatcher.service.skills == nil {
		return channel.Reply{Text: unknownChannelCommand(request.Message.Text)}, nil
	}
	name := strings.TrimPrefix(strings.ToLower(fields[0]), "/")
	if at := strings.IndexByte(name, '@'); at > 0 {
		name = name[:at]
	}
	metadata, err := dispatcher.service.skills.Get(name)
	if errors.Is(err, skill.ErrNotFound) {
		return channel.Reply{Text: unknownChannelCommand(request.Message.Text)}, nil
	}
	if err != nil {
		return channel.Reply{}, err
	}
	if !metadata.Enabled {
		return channel.Reply{Text: "Skill `/" + name + "` is installed but disabled. Enable it before using slash activation."}, nil
	}
	document, err := dispatcher.service.skills.Load(ctx, name)
	if err != nil {
		return channel.Reply{}, err
	}
	activation := channelSkillActivation{Name: name, Instructions: document}
	return dispatcher.dispatchAgent(context.WithValue(ctx, channelSkillContextKey, activation), request)
}

func (dispatcher channelDispatcher) modelList() string {
	names := make([]string, 0, len(dispatcher.service.config.Models))
	for _, configured := range dispatcher.service.config.Models {
		names = append(names, configured.Name)
	}
	if len(names) == 0 {
		return "No models configured."
	}
	return "Available models:\n• " + strings.Join(names, "\n• ")
}

func (dispatcher channelDispatcher) memoryStatus(ctx context.Context, userID string) (channel.Reply, error) {
	if dispatcher.service.memories == nil {
		return channel.Reply{Text: "Memory contains 0 fact(s)."}, nil
	}
	matches, err := dispatcher.service.memories.Search(ctx, memory.Query{
		Scope: memory.Scope{UserID: userID}, Limit: 100, Now: time.Now().UTC(),
	})
	if err != nil {
		return channel.Reply{}, err
	}
	return channel.Reply{Text: fmt.Sprintf("Memory contains %d fact(s).", len(matches))}, nil
}

func (dispatcher channelDispatcher) goalCommand(ctx context.Context, request channel.Request, arguments string) (channel.Reply, error) {
	threadID, found, err := dispatcher.mappedThread(ctx, request)
	if err != nil {
		return channel.Reply{}, err
	}
	arguments = strings.TrimSpace(arguments)
	if arguments == "" {
		return dispatcher.goalStatus(ctx, threadID, found)
	}
	if isGoalClear(arguments) {
		return dispatcher.clearGoal(ctx, threadID, found)
	}
	if len([]rune(arguments)) > 4000 {
		return channel.Reply{Text: "Goal objective must be at most 4000 characters."}, nil
	}
	return dispatcher.setGoal(ctx, request, threadID, found, arguments)
}

func (dispatcher channelDispatcher) goalStatus(ctx context.Context, threadID domain.ThreadID, found bool) (channel.Reply, error) {
	if !found {
		return channel.Reply{Text: "No active goal."}, nil
	}
	state, err := dispatcher.service.controls.Snapshot(ctx, threadID)
	if err != nil {
		return channel.Reply{}, err
	}
	if state.Goal == nil {
		return channel.Reply{Text: "No active goal."}, nil
	}
	return channel.Reply{Text: "Goal: " + state.Goal.Objective}, nil
}

func (dispatcher channelDispatcher) clearGoal(ctx context.Context, threadID domain.ThreadID, found bool) (channel.Reply, error) {
	if !found {
		return channel.Reply{Text: "Goal cleared."}, nil
	}
	if err := dispatcher.ensureThreadIdle(ctx, threadID); err != nil {
		return channel.Reply{}, err
	}
	if _, err := dispatcher.service.controls.ClearGoal(ctx, threadID); err != nil {
		return channel.Reply{}, err
	}
	return channel.Reply{Text: "Goal cleared."}, nil
}

func (dispatcher channelDispatcher) setGoal(ctx context.Context, request channel.Request, threadID domain.ThreadID, found bool, objective string) (channel.Reply, error) {
	var err error
	if !found {
		threadID, err = dispatcher.ensureThread(ctx, request)
		if err != nil {
			return channel.Reply{}, err
		}
	}
	if err = dispatcher.ensureThreadIdle(ctx, threadID); err != nil {
		return channel.Reply{}, err
	}
	if _, err = dispatcher.service.controls.SetGoal(ctx, threadID, objective, 0); err != nil {
		return channel.Reply{}, err
	}
	request.Message.Text = objective
	return dispatcher.dispatchAgent(ctx, request)
}

func isGoalClear(arguments string) bool {
	switch strings.ToLower(strings.TrimSpace(arguments)) {
	case "clear", "reset", "off":
		return true
	default:
		return false
	}
}

func unknownChannelCommand(text string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return "Unknown command. Available commands: " + strings.Join(channelCommandNames(), " | ")
	}
	return "Unknown command: " + fields[0] + ". Available commands: " + strings.Join(channelCommandNames(), " | ")
}

func channelCommandNames() []string {
	names := []string{"/bootstrap", "/goal", "/help", "/memory", "/models", "/new", "/status"}
	sort.Strings(names)
	return names
}

const channelCommandHelp = `Available commands:
/bootstrap — Start a bootstrap session
/goal [condition|clear] — Set, show, or clear an active goal
/new — Start a new conversation
/status — Show current thread info
/models — List available models
/memory — Show memory status
/<skill-name> <task> — Activate an enabled skill for one turn
/help — Show this help`

func channelAssistantID(message channel.Message, fallback string) string {
	configured := strings.TrimSpace(message.Metadata["assistant_id"])
	if message.Provider == "github" && configured != "" {
		return configured
	}
	return fallback
}

func (dispatcher channelDispatcher) ensureThread(ctx context.Context, request channel.Request) (domain.ThreadID, error) {
	threadID, found, err := dispatcher.mappedThread(ctx, request)
	if err != nil || found {
		return threadID, err
	}
	return dispatcher.createMappedThread(ctx, request)
}

func (dispatcher channelDispatcher) mappedThread(ctx context.Context, request channel.Request) (domain.ThreadID, bool, error) {
	mapped, err := dispatcher.state.Conversation(ctx, request.Identity.BindingID, request.Message.ChatID, request.Message.TopicID)
	if errors.Is(err, channel.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	thread, err := dispatcher.service.store.Thread(ctx, mapped.ThreadID)
	if err == nil {
		if store.ThreadOwnedBy(thread, request.Identity.UserID) {
			return mapped.ThreadID, true, nil
		}
		return "", false, channel.ErrConflict
	}
	if !errors.Is(err, store.ErrNotFound) {
		return "", false, err
	}
	if err = dispatcher.state.DeleteThread(context.WithoutCancel(ctx), mapped.ThreadID); err != nil {
		return "", false, err
	}
	return "", false, nil
}

func (dispatcher channelDispatcher) createMappedThread(ctx context.Context, request channel.Request) (domain.ThreadID, error) {
	thread, err := dispatcher.createChannelThread(ctx, request)
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	mapping := channel.Conversation{
		BindingID: request.Identity.BindingID, Provider: request.Message.Provider,
		ChatID: request.Message.ChatID, TopicID: request.Message.TopicID, ThreadID: thread.ID,
		CreatedAt: now, UpdatedAt: now,
	}
	actual, created, err := dispatcher.state.MapConversation(ctx, mapping)
	if err != nil {
		_ = dispatcher.service.store.DeleteThread(context.WithoutCancel(ctx), thread.ID)
		return "", err
	}
	if !created && actual.ThreadID != thread.ID {
		_ = dispatcher.service.store.DeleteThread(context.WithoutCancel(ctx), thread.ID)
		existing, lookupErr := dispatcher.service.store.Thread(ctx, actual.ThreadID)
		if lookupErr != nil {
			return "", lookupErr
		}
		if !store.ThreadOwnedBy(existing, request.Identity.UserID) {
			return "", channel.ErrConflict
		}
		return actual.ThreadID, nil
	}
	return thread.ID, nil
}

func (dispatcher channelDispatcher) createChannelThread(ctx context.Context, request channel.Request) (domain.Thread, error) {
	thread, err := domain.NewThread(time.Now())
	if err != nil {
		return domain.Thread{}, err
	}
	thread.Metadata = map[string]string{
		store.OwnerMetadataKey: request.Identity.UserID, "source": "channel",
		"channel_provider": request.Message.Provider, "channel_workspace_id": request.Message.WorkspaceID,
		"channel_chat_id": request.Message.ChatID, "channel_binding_id": request.Identity.BindingID,
	}
	if request.Message.TopicID != "" {
		thread.Metadata["channel_topic_id"] = request.Message.TopicID
	}
	if err = dispatcher.service.store.CreateThread(ctx, thread); err != nil {
		return domain.Thread{}, err
	}
	return thread, nil
}

func (dispatcher channelDispatcher) freshThread(ctx context.Context, request channel.Request) (domain.ThreadID, error) {
	_, found, err := dispatcher.mappedThread(ctx, request)
	if err != nil {
		return "", err
	}
	if !found {
		return dispatcher.createMappedThread(ctx, request)
	}
	thread, err := dispatcher.createChannelThread(ctx, request)
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	mapping := channel.Conversation{
		BindingID: request.Identity.BindingID, Provider: request.Message.Provider,
		ChatID: request.Message.ChatID, TopicID: request.Message.TopicID, ThreadID: thread.ID,
		CreatedAt: now, UpdatedAt: now,
	}
	if err = dispatcher.state.RemapConversation(ctx, mapping); err != nil {
		_ = dispatcher.service.store.DeleteThread(context.WithoutCancel(ctx), thread.ID)
		return "", err
	}
	return thread.ID, nil
}

func (dispatcher channelDispatcher) ensureThreadIdle(ctx context.Context, threadID domain.ThreadID) error {
	runs, err := dispatcher.service.store.Runs(ctx, threadID)
	if err != nil {
		return err
	}
	for _, run := range runs {
		if run.Status == domain.RunPending || run.Status == domain.RunRunning {
			return channel.ErrBusy
		}
	}
	return nil
}

func (dispatcher channelDispatcher) reply(ctx context.Context, run domain.Run) (channel.Reply, error) {
	if run.Status == domain.RunFailed {
		return channel.Reply{}, fmt.Errorf("channel run %s failed: %s", run.ID, run.Error)
	}
	if run.Status == domain.RunCancelled {
		return channel.Reply{}, context.Canceled
	}
	if run.Status == domain.RunInterrupted {
		messages, err := conversation.Load(ctx, dispatcher.service.store, run.ThreadID)
		if err != nil {
			return channel.Reply{}, err
		}
		state := humaninput.State(messages)
		if len(state.OpenRequests) == 0 {
			return channel.Reply{}, errors.New("channel run interrupted without pending human input")
		}
		return channel.Reply{Text: humaninput.FormatRequest(state.OpenRequests[len(state.OpenRequests)-1])}, nil
	}
	if run.Status != domain.RunSucceeded {
		return channel.Reply{}, channel.ErrBusy
	}
	messages, err := conversation.LoadRun(ctx, dispatcher.service.store, run.ID)
	if err != nil {
		return channel.Reply{}, err
	}
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role != domain.RoleAssistant {
			continue
		}
		parts := make([]string, 0)
		for _, content := range messages[index].Content {
			if content.Kind == domain.ContentText && strings.TrimSpace(content.Text) != "" {
				parts = append(parts, strings.TrimSpace(content.Text))
			}
		}
		if len(parts) > 0 {
			return channel.Reply{Text: strings.Join(parts, "\n\n")}, nil
		}
	}
	return channel.Reply{}, errors.New("channel run completed without an assistant response")
}

func channelRunInput(message channel.Message) (json.RawMessage, error) {
	text := message.Text
	if len(message.Attachments) > 0 {
		manifest, err := json.Marshal(message.Attachments)
		if err != nil {
			return nil, err
		}
		text = strings.TrimSpace(text + "\n\n<channel_attachments>\n" + string(manifest) + "\n</channel_attachments>")
	}
	return json.Marshal(map[string]any{"messages": []map[string]string{{"role": "user", "content": text}}})
}

func waitChannelRun(ctx context.Context, repository store.Store, runID domain.RunID) (domain.Run, error) {
	updates, err := repository.Watch(ctx, runID, 0)
	if err != nil {
		return domain.Run{}, err
	}
	ticker := time.NewTicker(channelWaitPollInterval)
	defer ticker.Stop()
	for {
		run, lookupErr := repository.Run(ctx, runID)
		if lookupErr != nil || run.Status == domain.RunInterrupted || run.Terminal() {
			return run, lookupErr
		}
		select {
		case <-ctx.Done():
			return domain.Run{}, ctx.Err()
		case _, open := <-updates:
			if !open {
				updates = nil
			}
		case <-ticker.C:
		}
	}
}

func cloneAnyMap(source map[string]any) map[string]any {
	cloned := make(map[string]any, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func channelBootstrap(ctx context.Context) bool {
	bootstrap, _ := ctx.Value(channelBootstrapContextKey).(bool)
	return bootstrap
}

func channelUser(ctx context.Context) string {
	userID, _ := ctx.Value(channelUserContextKey).(string)
	return userID
}

func channelSkill(ctx context.Context) (channelSkillActivation, bool) {
	activation, ok := ctx.Value(channelSkillContextKey).(channelSkillActivation)
	return activation, ok && activation.Name != "" && activation.Instructions != ""
}

func inheritChannelContext(destination, source context.Context) context.Context {
	if channelBootstrap(source) {
		destination = context.WithValue(destination, channelBootstrapContextKey, true)
	}
	if userID := channelUser(source); userID != "" {
		destination = context.WithValue(destination, channelUserContextKey, userID)
	}
	if activation, ok := channelSkill(source); ok {
		destination = context.WithValue(destination, channelSkillContextKey, activation)
	}
	return destination
}

func channelSkillPrompt(activation channelSkillActivation) string {
	return fmt.Sprintf("<activated_skill name=%q>\nThe user explicitly activated this enabled skill for the current turn. Follow its instructions.\n%s\n</activated_skill>", activation.Name, activation.Instructions)
}

func channelCommandEnvironment(ctx context.Context) (map[string]string, error) {
	userID := channelUser(ctx)
	if userID == "" {
		return nil, nil
	}
	return map[string]string{
		"DEERFLOW_CHANNEL_USER_ID": userID,
		"GOFER_CHANNEL_USER_ID":    userID,
	}, nil
}

func (service *Service) channelRoutes(mux *http.ServeMux) {
	if service.channels == nil {
		return
	}
	mux.HandleFunc("GET /api/channels", service.getChannelStatus)
	mux.HandleFunc("GET /api/channels/providers", service.getChannelProviders)
	mux.HandleFunc("GET /api/channels/connections", service.listChannelBindings)
	mux.HandleFunc("POST /api/channels/{provider}/connect", service.issueChannelConnectCode)
	mux.HandleFunc("DELETE /api/channels/connections/{connection_id}", service.disconnectChannelBinding)
	mux.HandleFunc("GET /api/channel-connections", service.listChannelBindings)
	mux.HandleFunc("POST /api/channel-connections", service.createChannelBinding)
	mux.HandleFunc("DELETE /api/channel-connections/{connection_id}", service.revokeChannelBinding)
}

type channelProviderResource struct {
	Provider         string `json:"provider"`
	DisplayName      string `json:"display_name"`
	Enabled          bool   `json:"enabled"`
	Configured       bool   `json:"configured"`
	Connectable      bool   `json:"connectable"`
	AuthMode         string `json:"auth_mode"`
	ConnectionStatus string `json:"connection_status"`
}

func (service *Service) getChannelProviders(writer http.ResponseWriter, request *http.Request) {
	bindings, err := service.channelState.Bindings(request.Context(), requestUser(request.Context()))
	if err != nil {
		writeChannelError(writer, err)
		return
	}
	statuses := make(map[string]channel.BindingStatus)
	for _, binding := range bindings {
		if _, exists := statuses[binding.Provider]; !exists {
			statuses[binding.Provider] = binding.Status
		}
	}
	resources := make([]channelProviderResource, 0)
	for _, provider := range service.channels.Providers() {
		if !channel.ConnectableProvider(provider) {
			continue
		}
		status := "not_connected"
		if statuses[provider] == channel.BindingConnected {
			status = string(channel.BindingConnected)
		} else if statuses[provider] == channel.BindingRevoked {
			status = string(channel.BindingRevoked)
		}
		resources = append(resources, channelProviderResource{
			Provider: provider, DisplayName: channelProviderDisplayName(provider), Enabled: true,
			Configured: true, Connectable: true, AuthMode: channelConnectionMode(provider), ConnectionStatus: status,
		})
	}
	writeResourceJSON(writer, http.StatusOK, map[string]any{"enabled": true, "providers": resources})
}

func (service *Service) issueChannelConnectCode(writer http.ResponseWriter, request *http.Request) {
	provider := strings.ToLower(strings.TrimSpace(request.PathValue("provider")))
	if !channel.ConnectableProvider(provider) || !containsString(service.channels.Providers(), provider) {
		writeResourceJSON(writer, http.StatusNotFound, map[string]string{"error": "Unknown channel provider"})
		return
	}
	code, err := service.channelState.IssueConnectCode(
		request.Context(), requestUser(request.Context()), provider, time.Now().UTC(),
		channel.ConnectCodeTTL, channel.MaxPendingConnectCodes,
	)
	if err != nil {
		writeChannelError(writer, err)
		return
	}
	instruction := "Send /connect " + code.Code + " to the Gofer " + channelProviderDisplayName(provider) + " bot."
	mode, connectURL := channelConnectionMode(provider), ""
	if provider == channel.TelegramProvider {
		instruction = "Send /start " + code.Code + " to the Gofer Telegram bot."
		if username := strings.TrimPrefix(service.config.Channels.Telegram.BotUsername, "@"); username != "" {
			connectURL = "https://t.me/" + username + "?start=" + code.Code
		}
	}
	writeResourceJSON(writer, http.StatusOK, map[string]any{
		"provider": provider, "mode": mode, "url": connectURL, "code": code.Code,
		"instruction": instruction, "expires_in": int(channel.ConnectCodeTTL.Seconds()),
	})
}

func (service *Service) disconnectChannelBinding(writer http.ResponseWriter, request *http.Request) {
	identifier := path.Base(request.PathValue("connection_id"))
	if err := service.channelState.Revoke(request.Context(), identifier, requestUser(request.Context()), time.Now()); err != nil {
		writeChannelError(writer, err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

func channelConnectionMode(provider string) string {
	if provider == channel.TelegramProvider {
		return "deep_link"
	}
	return "binding_code"
}

func channelProviderDisplayName(provider string) string {
	switch provider {
	case channel.TelegramProvider:
		return "Telegram"
	case channel.SlackProvider:
		return "Slack"
	case channel.DiscordProvider:
		return "Discord"
	case channel.FeishuProvider:
		return "Feishu"
	case channel.DingTalkProvider:
		return "DingTalk"
	case channel.WeComProvider:
		return "WeCom"
	case channel.WeChatProvider:
		return "WeChat"
	case channel.BuzzProvider:
		return "Buzz"
	default:
		return provider
	}
}

func containsString(values []string, target string) bool {
	index := sort.SearchStrings(values, target)
	return index < len(values) && values[index] == target
}

func (service *Service) getChannelStatus(writer http.ResponseWriter, _ *http.Request) {
	writeResourceJSON(writer, http.StatusOK, service.channels.Stats())
}

func (service *Service) listChannelBindings(writer http.ResponseWriter, request *http.Request) {
	bindings, err := service.channelState.Bindings(request.Context(), requestUser(request.Context()))
	if err != nil {
		writeChannelError(writer, err)
		return
	}
	writeResourceJSON(writer, http.StatusOK, map[string]any{"connections": bindings, "count": len(bindings)})
}

func (service *Service) createChannelBinding(writer http.ResponseWriter, request *http.Request) {
	var input struct {
		Provider         string `json:"provider"`
		WorkspaceID      string `json:"workspace_id"`
		WorkspaceName    string `json:"workspace_name"`
		ExternalUserID   string `json:"external_user_id"`
		ExternalUserName string `json:"external_user_name"`
	}
	if err := decodeAssistantJSON(writer, request, &input); err != nil {
		return
	}
	binding, err := channel.NewBinding(requestUser(request.Context()), input.Provider, input.WorkspaceID, input.ExternalUserID, time.Now())
	if err == nil {
		binding.WorkspaceName, binding.ExternalUserName = strings.TrimSpace(input.WorkspaceName), strings.TrimSpace(input.ExternalUserName)
		if err = binding.Validate(); err == nil {
			binding, err = service.channelState.Bind(request.Context(), binding)
		}
	}
	if err != nil {
		writeChannelError(writer, err)
		return
	}
	writeResourceJSON(writer, http.StatusCreated, binding)
}

func (service *Service) revokeChannelBinding(writer http.ResponseWriter, request *http.Request) {
	identifier := path.Base(request.PathValue("connection_id"))
	if err := service.channelState.Revoke(request.Context(), identifier, requestUser(request.Context()), time.Now()); err != nil {
		writeChannelError(writer, err)
		return
	}
	writeResourceJSON(writer, http.StatusOK, map[string]any{"success": true, "connection_id": identifier})
}

func writeChannelError(writer http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, channel.ErrInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, channel.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, channel.ErrConflict):
		status = http.StatusConflict
	case errors.Is(err, channel.ErrBusy):
		status = http.StatusTooManyRequests
	}
	writeResourceJSON(writer, status, map[string]string{"error": http.StatusText(status)})
}
