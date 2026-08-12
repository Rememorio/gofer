package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DiscordProvider is the normalized Discord channel name.
	DiscordProvider = "discord"
	discordBaseURL  = "https://discord.com/api/v10"
	discordTextMax  = 2000
	discordIntents  = 1 | 1<<9 | 1<<12 | 1<<15
)

// DiscordConfig controls Discord Gateway ingress and REST delivery.
type DiscordConfig struct {
	BotToken        string
	AllowedGuilds   []string
	AllowedChannels []string
	MentionOnly     bool
	ThreadMode      bool
	RequestTimeout  time.Duration
	MaxAttempts     int
	Client          *http.Client
	BaseURL         string
	Sleep           func(context.Context, time.Duration) error
}

// Discord is a Gateway v10 Discord bot provider.
type Discord struct {
	token           string
	allowedGuilds   map[string]struct{}
	allowedChannels map[string]struct{}
	mentionOnly     bool
	threadMode      bool
	client          *http.Client
	baseURL         string
	attempts        int
	sleep           func(context.Context, time.Duration) error
	dial            providerSocketDialer
	ownedClient     bool
	allowWS         bool

	mu        sync.Mutex
	startMu   sync.Mutex
	writeMu   sync.Mutex
	started   bool
	closed    bool
	cancel    context.CancelFunc
	done      chan struct{}
	socket    providerSocket
	gateway   string
	sessionID string
	resumeURL string
	botUserID string
	sequence  int64
	channels  map[string]discordChannel
	closeErr  error
	once      sync.Once
}

// NewDiscord validates configuration and creates a Discord provider.
func NewDiscord(config DiscordConfig) (*Discord, error) {
	token := strings.TrimSpace(config.BotToken)
	if token == "" || len(token) > 1024 {
		return nil, ErrInvalid
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = 20 * time.Second
	}
	if config.MaxAttempts == 0 {
		config.MaxAttempts = 3
	}
	if config.RequestTimeout < time.Second || config.RequestTimeout > 2*time.Minute || config.MaxAttempts < 1 || config.MaxAttempts > 5 {
		return nil, ErrInvalid
	}
	baseURL := strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
	if baseURL == "" {
		baseURL = discordBaseURL
	}
	parsed, err := url.Parse(baseURL)
	injected := config.Client != nil
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Scheme != "https" && !injected {
		return nil, ErrInvalid
	}
	if config.Client == nil {
		config.Client = newProviderClient(config.RequestTimeout)
	}
	if config.Sleep == nil {
		config.Sleep = sleepContext
	}
	return &Discord{
		token: token, allowedGuilds: normalizedSet(config.AllowedGuilds), allowedChannels: normalizedSet(config.AllowedChannels),
		mentionOnly: config.MentionOnly, threadMode: config.ThreadMode || config.MentionOnly, client: config.Client,
		baseURL: baseURL, attempts: config.MaxAttempts, sleep: config.Sleep, dial: dialProviderSocket,
		ownedClient: !injected, allowWS: injected, channels: make(map[string]discordChannel),
	}, nil
}

// Name returns the normalized provider name.
func (*Discord) Name() string { return DiscordProvider }

// Start discovers the Gateway, verifies its Hello, and launches the session.
func (provider *Discord) Start(parent context.Context, submit SubmitFunc) error {
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
	gateway, err := provider.discoverGateway(parent)
	if err != nil {
		return fmt.Errorf("discover Discord Gateway: %w", err)
	}
	socket, hello, err := provider.openGateway(parent, gateway)
	if err != nil {
		return fmt.Errorf("open Discord Gateway: %w", err)
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
	provider.gateway, provider.cancel, provider.done = gateway, cancel, make(chan struct{})
	provider.socket, provider.started = socket, true
	done := provider.done
	provider.mu.Unlock()
	go func() {
		defer close(done)
		provider.run(ctx, submit, socket, hello)
	}()
	return nil
}

// Send posts a message to the Discord thread or channel selected by the reply.
func (provider *Discord) Send(ctx context.Context, reply Reply) error {
	if provider == nil || ctx == nil || reply.Provider != DiscordProvider || strings.TrimSpace(reply.ChatID) == "" || strings.TrimSpace(reply.Text) == "" {
		return ErrInvalid
	}
	target := reply.TopicID
	if target == "" {
		target = reply.ChatID
	}
	for _, chunk := range splitUTF16(reply.Text, discordTextMax) {
		input := map[string]any{"content": chunk, "allowed_mentions": map[string]any{"parse": []string{}}}
		if err := retryProvider(ctx, provider.attempts, provider.sleep, func() error {
			var output map[string]any
			err := requestJSON(ctx, provider.client, http.MethodPost, provider.endpoint("/channels/"+url.PathEscape(target)+"/messages"), "Bot "+provider.token, input, &output)
			return err
		}); err != nil {
			return fmt.Errorf("send Discord message: %w", err)
		}
	}
	return nil
}

// Close terminates the Gateway session and releases owned resources.
func (provider *Discord) Close() error {
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

type discordGatewayDiscovery struct {
	URL string `json:"url"`
}

type discordGatewayPacket struct {
	Op       int             `json:"op"`
	Data     json.RawMessage `json:"d"`
	Sequence *int64          `json:"s"`
	Type     string          `json:"t"`
}

type discordHello struct {
	HeartbeatInterval float64 `json:"heartbeat_interval"`
}

type discordReady struct {
	SessionID        string `json:"session_id"`
	ResumeGatewayURL string `json:"resume_gateway_url"`
	User             struct {
		ID string `json:"id"`
	} `json:"user"`
}

type discordAuthor struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Bot      bool   `json:"bot"`
}

type discordAttachment struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	URL         string `json:"url"`
	Size        int64  `json:"size"`
}

type discordMessage struct {
	ID          string              `json:"id"`
	ChannelID   string              `json:"channel_id"`
	GuildID     string              `json:"guild_id"`
	Content     string              `json:"content"`
	Author      discordAuthor       `json:"author"`
	Attachments []discordAttachment `json:"attachments"`
}

type discordChannel struct {
	ID       string `json:"id"`
	Type     int    `json:"type"`
	ParentID string `json:"parent_id"`
}

func (channel discordChannel) isThread() bool {
	return channel.Type == 10 || channel.Type == 11 || channel.Type == 12
}

func (provider *Discord) discoverGateway(ctx context.Context) (string, error) {
	var discovery discordGatewayDiscovery
	err := requestJSON(ctx, provider.client, http.MethodGet, provider.endpoint("/gateway/bot"), "Bot "+provider.token, nil, &discovery)
	if err != nil {
		return "", err
	}
	return provider.validateGatewayURL(discovery.URL)
}

func (provider *Discord) validateGatewayURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || parsed.Scheme != "wss" && !(provider.allowWS && parsed.Scheme == "ws") || !provider.allowWS && !trustedProviderHost(parsed.Hostname(), "discord.gg") {
		return "", ErrInvalid
	}
	query := parsed.Query()
	query.Set("v", "10")
	query.Set("encoding", "json")
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func (provider *Discord) openGateway(ctx context.Context, endpoint string) (providerSocket, discordHello, error) {
	socket, err := provider.dial(ctx, endpoint, provider.client)
	if err != nil {
		return nil, discordHello{}, err
	}
	payload, err := socket.Read(ctx)
	if err != nil {
		_ = socket.Close()
		return nil, discordHello{}, err
	}
	var packet discordGatewayPacket
	var hello discordHello
	if json.Unmarshal(payload, &packet) != nil || packet.Op != 10 || json.Unmarshal(packet.Data, &hello) != nil || hello.HeartbeatInterval < 1000 || hello.HeartbeatInterval > float64((5*time.Minute)/time.Millisecond) {
		_ = socket.Close()
		return nil, discordHello{}, ErrInvalid
	}
	return socket, hello, nil
}

func (provider *Discord) run(ctx context.Context, submit SubmitFunc, socket providerSocket, hello discordHello) {
	for ctx.Err() == nil {
		provider.serveGateway(ctx, submit, socket, hello)
		_ = socket.Close()
		provider.clearSocket(socket)
		if ctx.Err() != nil {
			return
		}
		for failures := 1; ctx.Err() == nil; failures++ {
			if provider.sleep(ctx, providerReconnectDelay(failures)) != nil {
				return
			}
			nextSocket, nextHello, err := provider.openGateway(ctx, provider.nextGateway())
			if err != nil {
				continue
			}
			socket, hello = nextSocket, nextHello
			provider.setSocket(socket)
			break
		}
	}
}

func (provider *Discord) serveGateway(ctx context.Context, submit SubmitFunc, socket providerSocket, hello discordHello) {
	connectionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var awaiting atomic.Bool
	if err := provider.authenticateGateway(connectionCtx, socket); err != nil {
		return
	}
	go provider.heartbeat(connectionCtx, socket, time.Duration(hello.HeartbeatInterval)*time.Millisecond, &awaiting)
	for connectionCtx.Err() == nil {
		payload, err := socket.Read(connectionCtx)
		if err != nil {
			return
		}
		var packet discordGatewayPacket
		if json.Unmarshal(payload, &packet) != nil {
			continue
		}
		if packet.Sequence != nil {
			provider.setSequence(*packet.Sequence)
		}
		switch packet.Op {
		case 0:
			if provider.handleDispatch(connectionCtx, submit, packet) != nil {
				continue
			}
		case 1:
			_ = provider.writeGateway(connectionCtx, socket, 1, provider.heartbeatSequence())
		case 7:
			return
		case 9:
			var resumable bool
			_ = json.Unmarshal(packet.Data, &resumable)
			if !resumable {
				provider.clearSession()
			}
			return
		case 11:
			awaiting.Store(false)
		}
	}
}

func (provider *Discord) authenticateGateway(ctx context.Context, socket providerSocket) error {
	provider.mu.Lock()
	sessionID, sequence := provider.sessionID, provider.sequence
	provider.mu.Unlock()
	if sessionID != "" {
		return provider.writeGateway(ctx, socket, 6, map[string]any{"token": provider.token, "session_id": sessionID, "seq": sequence})
	}
	return provider.writeGateway(ctx, socket, 2, map[string]any{
		"token": provider.token, "intents": discordIntents,
		"properties": map[string]string{"os": "go", "browser": "gofer", "device": "gofer"},
	})
}

func (provider *Discord) heartbeat(ctx context.Context, socket providerSocket, interval time.Duration, awaiting *atomic.Bool) {
	initial := time.NewTimer(time.Duration(rand.Float64() * float64(interval)))
	defer initial.Stop()
	select {
	case <-ctx.Done():
		return
	case <-initial.C:
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if awaiting.Swap(true) {
			_ = socket.Close()
			return
		}
		if provider.writeGateway(ctx, socket, 1, provider.heartbeatSequence()) != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (provider *Discord) heartbeatSequence() any {
	sequence := provider.currentSequence()
	if sequence == 0 {
		return nil
	}
	return sequence
}

func (provider *Discord) handleDispatch(ctx context.Context, submit SubmitFunc, packet discordGatewayPacket) error {
	if packet.Type == "READY" {
		var ready discordReady
		if json.Unmarshal(packet.Data, &ready) == nil {
			provider.setSession(ready)
		}
		return nil
	}
	if packet.Type != "MESSAGE_CREATE" {
		return nil
	}
	var inbound discordMessage
	if err := json.Unmarshal(packet.Data, &inbound); err != nil {
		return err
	}
	message, keep := provider.normalize(ctx, inbound)
	if !keep {
		return nil
	}
	return submitProviderMessage(ctx, submit, message)
}

func (provider *Discord) normalize(ctx context.Context, inbound discordMessage) (Message, bool) {
	provider.mu.Lock()
	botUserID := provider.botUserID
	provider.mu.Unlock()
	if inbound.ID == "" || inbound.ChannelID == "" || inbound.Author.ID == "" || inbound.Author.Bot || inbound.Author.ID == botUserID || !allowed(provider.allowedGuilds, inbound.GuildID) {
		return Message{}, false
	}
	text, hasMention := discordMessageText(inbound.Content, botUserID)
	attachments := discordAttachments(inbound.Attachments)
	if text == "" && len(attachments) == 0 {
		return Message{}, false
	}
	chatID, topicID, accepted := provider.discordRoute(ctx, inbound, hasMention)
	if !accepted {
		return Message{}, false
	}
	return Message{
		ID: inbound.ID, Provider: DiscordProvider, WorkspaceID: inbound.GuildID,
		ExternalUserID: inbound.Author.ID, ChatID: chatID, TopicID: topicID,
		Text: text, Attachments: attachments,
		Metadata:   map[string]string{"guild_id": inbound.GuildID, "channel_id": inbound.ChannelID, "username": inbound.Author.Username},
		ReceivedAt: discordSnowflakeTime(inbound.ID),
	}, true
}

func discordMessageText(content, botUserID string) (string, bool) {
	text := strings.TrimSpace(content)
	standard, nickname := "<@"+botUserID+">", "<@!"+botUserID+">"
	hasMention := botUserID != "" && (strings.Contains(text, standard) || strings.Contains(text, nickname))
	if hasMention {
		text = strings.TrimSpace(strings.NewReplacer(standard, "", nickname, "").Replace(text))
	}
	return text, hasMention
}

func (provider *Discord) discordRoute(ctx context.Context, inbound discordMessage, hasMention bool) (string, string, bool) {
	channel, err := provider.channel(ctx, inbound.ChannelID)
	if err != nil {
		channel = discordChannel{ID: inbound.ChannelID}
	}
	chatID, topicID := inbound.ChannelID, inbound.ChannelID
	if channel.isThread() {
		if channel.ParentID != "" {
			chatID = channel.ParentID
		}
	} else {
		if inbound.GuildID != "" && provider.mentionOnly && !hasMention && !listed(provider.allowedChannels, inbound.ChannelID) {
			return "", "", false
		}
		if provider.threadMode {
			if threadID, createErr := provider.createThread(ctx, inbound); createErr == nil {
				topicID = threadID
			}
		}
	}
	return chatID, topicID, true
}

func discordAttachments(files []discordAttachment) []Attachment {
	attachments := make([]Attachment, 0, len(files))
	for _, file := range files {
		parsed, err := url.Parse(file.URL)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || file.ID == "" {
			continue
		}
		name, mediaType := strings.TrimSpace(file.Filename), strings.TrimSpace(file.ContentType)
		if name == "" {
			name = "discord-file"
		}
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}
		if file.Size < 0 {
			file.Size = 0
		}
		attachments = append(attachments, Attachment{Name: name, MediaType: mediaType, URL: parsed.String(), Size: file.Size})
	}
	return attachments
}

func (provider *Discord) channel(ctx context.Context, channelID string) (discordChannel, error) {
	provider.mu.Lock()
	if cached, exists := provider.channels[channelID]; exists {
		provider.mu.Unlock()
		return cached, nil
	}
	provider.mu.Unlock()
	var channel discordChannel
	err := requestJSON(ctx, provider.client, http.MethodGet, provider.endpoint("/channels/"+url.PathEscape(channelID)), "Bot "+provider.token, nil, &channel)
	if err != nil {
		return discordChannel{}, err
	}
	provider.mu.Lock()
	if len(provider.channels) >= 1024 {
		clear(provider.channels)
	}
	provider.channels[channelID] = channel
	provider.mu.Unlock()
	return channel, nil
}

func (provider *Discord) createThread(ctx context.Context, inbound discordMessage) (string, error) {
	name := "gofer-" + inbound.Author.Username + "-" + inbound.ID
	if len(name) > 100 {
		name = name[:100]
	}
	input := map[string]any{"name": name, "auto_archive_duration": 1440}
	var thread discordChannel
	err := requestJSON(ctx, provider.client, http.MethodPost, provider.endpoint("/channels/"+url.PathEscape(inbound.ChannelID)+"/messages/"+url.PathEscape(inbound.ID)+"/threads"), "Bot "+provider.token, input, &thread)
	if err != nil || thread.ID == "" {
		return "", errors.Join(err, ErrInvalid)
	}
	provider.mu.Lock()
	provider.channels[thread.ID] = discordChannel{ID: thread.ID, Type: 11, ParentID: inbound.ChannelID}
	provider.mu.Unlock()
	return thread.ID, nil
}

func discordSnowflakeTime(identifier string) time.Time {
	value, err := strconv.ParseUint(identifier, 10, 64)
	if err != nil {
		return time.Now().UTC()
	}
	milliseconds := int64(value>>22) + 1420070400000
	return time.UnixMilli(milliseconds).UTC()
}

func (provider *Discord) writeGateway(ctx context.Context, socket providerSocket, opcode int, data any) error {
	payload, err := json.Marshal(map[string]any{"op": opcode, "d": data})
	if err != nil {
		return err
	}
	provider.writeMu.Lock()
	defer provider.writeMu.Unlock()
	return socket.Write(ctx, payload)
}

func (provider *Discord) endpoint(route string) string { return provider.baseURL + route }

func (provider *Discord) currentSequence() int64 {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.sequence
}

func (provider *Discord) setSequence(sequence int64) {
	provider.mu.Lock()
	provider.sequence = sequence
	provider.mu.Unlock()
}

func (provider *Discord) setSession(ready discordReady) {
	provider.mu.Lock()
	provider.sessionID, provider.resumeURL, provider.botUserID = ready.SessionID, ready.ResumeGatewayURL, ready.User.ID
	provider.mu.Unlock()
}

func (provider *Discord) clearSession() {
	provider.mu.Lock()
	provider.sessionID, provider.resumeURL, provider.sequence = "", "", 0
	provider.mu.Unlock()
}

func (provider *Discord) nextGateway() string {
	provider.mu.Lock()
	raw := provider.resumeURL
	if raw == "" {
		raw = provider.gateway
	}
	provider.mu.Unlock()
	endpoint, err := provider.validateGatewayURL(raw)
	if err != nil {
		return provider.gateway
	}
	return endpoint
}

func (provider *Discord) setSocket(socket providerSocket) {
	provider.mu.Lock()
	if !provider.closed {
		provider.socket = socket
	}
	provider.mu.Unlock()
}

func (provider *Discord) clearSocket(socket providerSocket) {
	provider.mu.Lock()
	if provider.socket == socket {
		provider.socket = nil
	}
	provider.mu.Unlock()
}
