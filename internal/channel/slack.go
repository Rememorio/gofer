package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// SlackProvider is the normalized Slack channel name.
	SlackProvider = "slack"
	slackBaseURL  = "https://slack.com/api"
	slackTextMax  = 40_000
)

var slackBoldPattern = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)

// SlackConfig controls Slack Socket Mode and Web API delivery.
type SlackConfig struct {
	BotToken       string
	AppToken       string
	BotUserID      string
	AllowedUsers   []string
	RequestTimeout time.Duration
	MaxAttempts    int
	Client         *http.Client
	BaseURL        string
	Sleep          func(context.Context, time.Duration) error
}

// Slack is a Socket Mode Slack provider.
type Slack struct {
	botToken     string
	appToken     string
	botUserID    string
	allowedUsers map[string]struct{}
	client       *http.Client
	baseURL      string
	attempts     int
	sleep        func(context.Context, time.Duration) error
	dial         providerSocketDialer
	ownedClient  bool
	allowWS      bool

	mu       sync.Mutex
	startMu  sync.Mutex
	started  bool
	closed   bool
	cancel   context.CancelFunc
	done     chan struct{}
	socket   providerSocket
	closeErr error
	once     sync.Once
}

// NewSlack validates configuration and creates a Slack provider.
func NewSlack(config SlackConfig) (*Slack, error) {
	botToken, appToken := strings.TrimSpace(config.BotToken), strings.TrimSpace(config.AppToken)
	if botToken == "" || appToken == "" || len(botToken) > 1024 || len(appToken) > 1024 {
		return nil, ErrInvalid
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = 20 * time.Second
	}
	if config.MaxAttempts == 0 {
		config.MaxAttempts = 3
	}
	if config.RequestTimeout < time.Second || config.RequestTimeout > 2*time.Minute || !validProviderAttempts(config.MaxAttempts) {
		return nil, ErrInvalid
	}
	injected := config.Client != nil
	baseURL, err := resolveProviderBaseURL(config.BaseURL, slackBaseURL, injected)
	if err != nil {
		return nil, ErrInvalid
	}
	if config.Client == nil {
		config.Client = newProviderClient(config.RequestTimeout)
	}
	if config.Sleep == nil {
		config.Sleep = sleepContext
	}
	return &Slack{
		botToken: botToken, appToken: appToken, botUserID: strings.TrimPrefix(strings.TrimSpace(config.BotUserID), "@"),
		allowedUsers: normalizedSet(config.AllowedUsers), client: config.Client, baseURL: baseURL,
		attempts: config.MaxAttempts, sleep: config.Sleep, dial: dialProviderSocket,
		ownedClient: !injected, allowWS: injected,
	}, nil
}

// Name returns the normalized provider name.
func (*Slack) Name() string { return SlackProvider }

// Start verifies credentials, opens Socket Mode, and launches reconnecting ingress.
func (provider *Slack) Start(parent context.Context, submit SubmitFunc) error {
	if provider == nil || parent == nil || submit == nil {
		return ErrInvalid
	}
	provider.startMu.Lock()
	defer provider.startMu.Unlock()
	provider.mu.Lock()
	if provider.closed {
		provider.mu.Unlock()
		return ErrClosed
	}
	if provider.started {
		provider.mu.Unlock()
		return nil
	}
	provider.mu.Unlock()
	if err := provider.verify(parent); err != nil {
		return fmt.Errorf("verify Slack bot: %w", err)
	}
	socket, err := provider.openSocket(parent)
	if err != nil {
		return fmt.Errorf("open Slack Socket Mode: %w", err)
	}
	provider.mu.Lock()
	if provider.closed || provider.started {
		provider.mu.Unlock()
		_ = socket.Close()
		if provider.closed {
			return ErrClosed
		}
		return nil
	}
	ctx, cancel := context.WithCancel(parent)
	provider.cancel, provider.done, provider.socket, provider.started = cancel, make(chan struct{}), socket, true
	done := provider.done
	provider.mu.Unlock()
	go func() {
		defer close(done)
		provider.run(ctx, submit, socket)
	}()
	return nil
}

// Send posts a thread-aware Slack mrkdwn message with bounded retries.
func (provider *Slack) Send(ctx context.Context, reply Reply) error {
	if provider == nil || ctx == nil || reply.Provider != SlackProvider || strings.TrimSpace(reply.ChatID) == "" || strings.TrimSpace(reply.Text) == "" {
		return ErrInvalid
	}
	for _, chunk := range splitUTF16(reply.Text, slackTextMax) {
		input := map[string]any{"channel": reply.ChatID, "text": slackText(chunk), "mrkdwn": true}
		if reply.TopicID != "" {
			input["thread_ts"] = reply.TopicID
		}
		if err := retryProvider(ctx, provider.attempts, provider.sleep, func() error {
			var response slackResponse
			err := requestJSON(ctx, provider.client, http.MethodPost, provider.endpoint("chat.postMessage"), "Bearer "+provider.botToken, input, &response)
			if err == nil {
				err = response.err()
			}
			return err
		}); err != nil {
			return fmt.Errorf("send Slack message: %w", err)
		}
	}
	return nil
}

// Close stops reconnects, closes the current socket, and releases resources.
func (provider *Slack) Close() error {
	if provider == nil {
		return nil
	}
	provider.once.Do(func() {
		provider.mu.Lock()
		provider.closed = true
		cancel, socket, done := provider.cancel, provider.socket, provider.done
		provider.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if socket != nil {
			provider.closeErr = socket.Close()
		}
		if done != nil {
			<-done
		}
		if provider.ownedClient {
			provider.client.CloseIdleConnections()
		}
	})
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.closeErr
}

type slackResponse struct {
	OK     bool   `json:"ok"`
	Error  string `json:"error"`
	URL    string `json:"url"`
	UserID string `json:"user_id"`
}

func (response slackResponse) err() error {
	if response.OK {
		return nil
	}
	return &providerHTTPError{status: http.StatusBadRequest, message: response.Error}
}

type slackEnvelope struct {
	Type       string          `json:"type"`
	EnvelopeID string          `json:"envelope_id"`
	Payload    json.RawMessage `json:"payload"`
}

type slackEventPayload struct {
	EventID string     `json:"event_id"`
	TeamID  string     `json:"team_id"`
	Team    string     `json:"team"`
	Event   slackEvent `json:"event"`
}

type slackEvent struct {
	Type        string      `json:"type"`
	Subtype     string      `json:"subtype"`
	BotID       string      `json:"bot_id"`
	User        string      `json:"user"`
	Text        string      `json:"text"`
	Channel     string      `json:"channel"`
	Team        string      `json:"team"`
	Timestamp   string      `json:"ts"`
	ThreadTS    string      `json:"thread_ts"`
	ClientMsgID string      `json:"client_msg_id"`
	Files       []slackFile `json:"files"`
}

type slackFile struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	MimeType string `json:"mimetype"`
	Size     int64  `json:"size"`
}

func (provider *Slack) verify(ctx context.Context) error {
	var response slackResponse
	err := requestJSON(ctx, provider.client, http.MethodPost, provider.endpoint("auth.test"), "Bearer "+provider.botToken, nil, &response)
	if err != nil {
		return err
	}
	if err = response.err(); err != nil {
		return err
	}
	if provider.botUserID == "" {
		provider.botUserID = response.UserID
	}
	if provider.botUserID == "" {
		return ErrInvalid
	}
	return nil
}

func (provider *Slack) openSocket(ctx context.Context) (providerSocket, error) {
	var response slackResponse
	err := requestJSON(ctx, provider.client, http.MethodPost, provider.endpoint("apps.connections.open"), "Bearer "+provider.appToken, nil, &response)
	if err != nil {
		return nil, err
	}
	if err = response.err(); err != nil {
		return nil, err
	}
	parsed, err := url.Parse(response.URL)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || parsed.Scheme != "wss" && !(provider.allowWS && parsed.Scheme == "ws") || !provider.allowWS && !trustedProviderHost(parsed.Hostname(), "slack.com") {
		return nil, ErrInvalid
	}
	return provider.dial(ctx, parsed.String(), provider.client)
}

func (provider *Slack) run(ctx context.Context, submit SubmitFunc, socket providerSocket) {
	for ctx.Err() == nil {
		reconnect := provider.readSocket(ctx, submit, socket)
		_ = socket.Close()
		provider.clearSocket(socket)
		if !reconnect || ctx.Err() != nil {
			return
		}
		for failures := 1; ctx.Err() == nil; failures++ {
			if provider.sleep(ctx, providerReconnectDelay(failures)) != nil {
				return
			}
			next, err := provider.openSocket(ctx)
			if err != nil {
				continue
			}
			socket = next
			provider.setSocket(socket)
			break
		}
	}
}

func (provider *Slack) readSocket(ctx context.Context, submit SubmitFunc, socket providerSocket) bool {
	for ctx.Err() == nil {
		payload, err := socket.Read(ctx)
		if err != nil {
			return true
		}
		var envelope slackEnvelope
		if json.Unmarshal(payload, &envelope) != nil {
			continue
		}
		if envelope.Type == "disconnect" {
			return true
		}
		ack := true
		if envelope.Type == "events_api" {
			ack = provider.handleEvent(ctx, submit, envelope.Payload)
		}
		if ack && envelope.EnvelopeID != "" {
			encoded, _ := json.Marshal(map[string]string{"envelope_id": envelope.EnvelopeID})
			if socket.Write(ctx, encoded) != nil {
				return true
			}
		}
	}
	return false
}

func (provider *Slack) handleEvent(ctx context.Context, submit SubmitFunc, raw json.RawMessage) bool {
	var payload slackEventPayload
	if json.Unmarshal(raw, &payload) != nil {
		return true
	}
	message, keep := provider.normalize(payload)
	if !keep {
		return true
	}
	err := submit(ctx, message)
	return err == nil || errors.Is(err, ErrDuplicate)
}

func (provider *Slack) normalize(payload slackEventPayload) (Message, bool) {
	event := payload.Event
	if event.Type != "message" && event.Type != "app_mention" || event.BotID != "" || event.Subtype != "" {
		return Message{}, false
	}
	if !allowed(provider.allowedUsers, event.User) {
		candidate := strings.TrimSpace(event.Text)
		if event.Type == "app_mention" {
			candidate = stripSlackMention(candidate, provider.botUserID)
		}
		if _, connecting := ParseConnectCommand(candidate, SlackProvider); !connecting {
			return Message{}, false
		}
	}
	text := strings.TrimSpace(event.Text)
	if event.Type == "app_mention" {
		text = stripSlackMention(text, provider.botUserID)
	}
	attachments := slackAttachments(event.Files)
	if text == "" && len(attachments) == 0 || event.User == "" || event.Channel == "" || event.Timestamp == "" {
		return Message{}, false
	}
	workspaceID := payload.TeamID
	if workspaceID == "" {
		workspaceID = payload.Team
	}
	if workspaceID == "" {
		workspaceID = event.Team
	}
	topicID := event.ThreadTS
	if topicID == "" {
		topicID = event.Timestamp
	}
	metadata := map[string]string{"event_id": payload.EventID}
	if event.ClientMsgID != "" {
		metadata["client_msg_id"] = event.ClientMsgID
	}
	return Message{
		ID: event.Timestamp, Provider: SlackProvider, WorkspaceID: workspaceID,
		ExternalUserID: event.User, ChatID: event.Channel, TopicID: topicID,
		Text: text, Attachments: attachments, Metadata: metadata, ReceivedAt: slackTimestamp(event.Timestamp),
	}, true
}

func slackAttachments(files []slackFile) []Attachment {
	attachments := make([]Attachment, 0, len(files))
	for _, file := range files {
		if file.ID == "" {
			continue
		}
		name, mediaType := strings.TrimSpace(file.Name), strings.TrimSpace(file.MimeType)
		if name == "" {
			name = "slack-file"
		}
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}
		if file.Size < 0 {
			file.Size = 0
		}
		attachments = append(attachments, Attachment{Name: name, MediaType: mediaType, URL: "slack://file/" + url.PathEscape(file.ID), Size: file.Size})
	}
	return attachments
}

func stripSlackMention(text, botUserID string) string {
	for _, prefix := range []string{"<@" + botUserID + ">", "<@!" + botUserID + ">"} {
		if botUserID != "" && strings.HasPrefix(text, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(text, prefix))
		}
	}
	return text
}

func slackText(text string) string {
	escaped := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(text)
	lines := strings.Split(escaped, "\n")
	for index, line := range lines {
		if strings.HasPrefix(line, "&gt;") {
			lines[index] = ">" + strings.TrimPrefix(line, "&gt;")
		}
	}
	return slackBoldPattern.ReplaceAllString(strings.Join(lines, "\n"), "*$1*")
}

func slackTimestamp(value string) time.Time {
	seconds := value
	if index := strings.IndexByte(seconds, '.'); index >= 0 {
		seconds = seconds[:index]
	}
	unix, err := strconv.ParseInt(seconds, 10, 64)
	if err != nil {
		return time.Now().UTC()
	}
	return time.Unix(unix, 0).UTC()
}

func (provider *Slack) endpoint(method string) string { return provider.baseURL + "/" + method }

func (provider *Slack) setSocket(socket providerSocket) {
	provider.mu.Lock()
	if !provider.closed {
		provider.socket = socket
	}
	provider.mu.Unlock()
}

func (provider *Slack) clearSocket(socket providerSocket) {
	provider.mu.Lock()
	if provider.socket == socket {
		provider.socket = nil
	}
	provider.mu.Unlock()
}
