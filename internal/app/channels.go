package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/Rememorio/gofer/internal/auth"
	"github.com/Rememorio/gofer/internal/channel"
	"github.com/Rememorio/gofer/internal/conversation"
	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/event"
	"github.com/Rememorio/gofer/internal/gateway"
	"github.com/Rememorio/gofer/internal/humaninput"
	"github.com/Rememorio/gofer/internal/store"
)

const channelWaitPollInterval = 100 * time.Millisecond

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
	manager, err := channel.NewManager(channel.Config{
		Resolver: state, Dispatcher: channelDispatcher{service: service, state: state}, Dedupe: state,
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
	if service.config.Channels.Webhook.Enabled {
		if err = service.openWebhookChannel(manager); err != nil {
			_ = manager.Close()
			return err
		}
	}
	if err = manager.Start(service.ctx); err != nil {
		_ = manager.Close()
		return err
	}
	service.channelState, service.channels = state, manager
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
	draft, err := event.NewDraft(threadID, run.ID, event.RunCreated, time.Now(), map[string]any{
		"source": "channel", "provider": request.Message.Provider, "message_id": request.Message.ID,
	})
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
	launch := gateway.StartRequest{
		RunID: run.ID, ThreadID: threadID,
		Request: gateway.RunRequest{
			AssistantID: dispatcher.service.config.Models[0].Name, Input: input,
			Metadata: map[string]any{"source": "channel", "provider": request.Message.Provider, "message_id": request.Message.ID},
		},
	}
	launchContext := auth.WithPrincipal(ctx, auth.Principal{ID: request.Identity.UserID, Permissions: []auth.Permission{auth.Admin}})
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
	thread, err := domain.NewThread(time.Now())
	if err != nil {
		return "", err
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

func (service *Service) channelRoutes(mux *http.ServeMux) {
	if service.channels == nil {
		return
	}
	mux.HandleFunc("GET /api/channels", service.getChannelStatus)
	mux.HandleFunc("GET /api/channel-connections", service.listChannelBindings)
	mux.HandleFunc("POST /api/channel-connections", service.createChannelBinding)
	mux.HandleFunc("DELETE /api/channel-connections/{connection_id}", service.revokeChannelBinding)
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
	}
	writeResourceJSON(writer, status, map[string]string{"error": http.StatusText(status)})
}
