package channel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// WeComProvider is the normalized WeCom AI Bot channel name.
	WeComProvider       = "wecom"
	weComSocketURL      = "wss://openws.work.weixin.qq.com"
	weComSocketHost     = "openws.work.weixin.qq.com"
	weComTextMax        = 20_000
	weComSubscribe      = "aibot_subscribe"
	weComHeartbeat      = "ping"
	weComCallback       = "aibot_msg_callback"
	weComRespondMessage = "aibot_respond_msg"
	weComSendMessage    = "aibot_send_msg"
)

// WeComConfig controls the WeCom AI Bot WebSocket provider.
type WeComConfig struct {
	BotID          string
	BotSecret      string
	AllowedUsers   []string
	WorkingMessage string
	Heartbeat      time.Duration
	RequestTimeout time.Duration
	MaxAttempts    int
	Client         *http.Client
	SocketURL      string
	Sleep          func(context.Context, time.Duration) error
	Now            func() time.Time
}

// WeCom is a native WeCom AI Bot WebSocket provider.
type WeCom struct {
	botID          string
	botSecret      string
	allowedUsers   map[string]struct{}
	workingMessage string
	heartbeat      time.Duration
	authTimeout    time.Duration
	attempts       int
	client         *http.Client
	socketURL      string
	sleep          func(context.Context, time.Duration) error
	now            func() time.Time
	dial           providerSocketDialer
	ownedClient    bool
	allowWS        bool
	routes         *routeStore[weComRoute]
	seen           *routeStore[struct{}]

	startMu  sync.Mutex
	writeMu  sync.Mutex
	mu       sync.Mutex
	started  bool
	closed   bool
	cancel   context.CancelFunc
	done     chan struct{}
	socket   providerSocket
	closeErr error
	once     sync.Once
}

type weComRoute struct {
	RequestID string
	ChatID    string
	StreamID  string
}

type weComFrame struct {
	Command string          `json:"cmd,omitempty"`
	Headers weComHeaders    `json:"headers"`
	Body    json.RawMessage `json:"body,omitempty"`
	ErrCode *int            `json:"errcode,omitempty"`
	ErrMsg  string          `json:"errmsg,omitempty"`
}

type weComOutboundFrame struct {
	Command string       `json:"cmd,omitempty"`
	Headers weComHeaders `json:"headers"`
	Body    any          `json:"body,omitempty"`
}

type weComHeaders struct {
	RequestID string `json:"req_id,omitempty"`
	ChatID    string `json:"chat_id,omitempty"`
	MessageID string `json:"msg_id,omitempty"`
}

type weComMessageBody struct {
	MessageID string `json:"msgid"`
	AIbotID   string `json:"aibotid"`
	ChatType  string `json:"chattype"`
	MsgType   string `json:"msgtype"`
	From      struct {
		UserID string `json:"userid"`
	} `json:"from"`
	Text struct {
		Content string `json:"content"`
	} `json:"text"`
	Quote struct {
		Text struct {
			Content string `json:"content"`
		} `json:"text"`
	} `json:"quote"`
	Voice struct {
		Content string `json:"content"`
	} `json:"voice"`
	Image weComMedia `json:"image"`
	File  weComMedia `json:"file"`
	Mixed struct {
		Items []weComMixedItem `json:"msg_item"`
	} `json:"mixed"`
}

type weComMedia struct {
	URL    string `json:"url"`
	AESKey string `json:"aeskey"`
	Name   string `json:"filename"`
}

type weComMixedItem struct {
	MsgType string `json:"msgtype"`
	Text    struct {
		Content string `json:"content"`
	} `json:"text"`
	Image weComMedia `json:"image"`
	File  weComMedia `json:"file"`
}

// NewWeCom validates configuration and creates a WeCom provider.
func NewWeCom(config WeComConfig) (*WeCom, error) {
	botID, secret := strings.TrimSpace(config.BotID), strings.TrimSpace(config.BotSecret)
	if botID == "" || secret == "" || len(botID) > 512 || len(secret) > 2048 || !validChannelValueSet(config.AllowedUsers) {
		return nil, ErrInvalid
	}
	if config.WorkingMessage == "" {
		config.WorkingMessage = "Working on it..."
	}
	if len(config.WorkingMessage) > 2000 {
		return nil, ErrInvalid
	}
	if config.Heartbeat == 0 {
		config.Heartbeat = 30 * time.Second
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = 20 * time.Second
	}
	if config.MaxAttempts == 0 {
		config.MaxAttempts = 3
	}
	if config.Heartbeat < 5*time.Second || config.Heartbeat > 2*time.Minute || config.RequestTimeout < time.Second || config.RequestTimeout > 2*time.Minute || !validProviderAttempts(config.MaxAttempts) {
		return nil, ErrInvalid
	}
	injected := config.Client != nil
	if config.Client == nil {
		config.Client = newProviderClient(config.RequestTimeout)
	}
	socketURL, err := validateWeComSocketURL(config.SocketURL, injected)
	if err != nil {
		return nil, err
	}
	if config.Sleep == nil {
		config.Sleep = sleepContext
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &WeCom{
		botID: botID, botSecret: secret, allowedUsers: normalizedSet(config.AllowedUsers), workingMessage: config.WorkingMessage,
		heartbeat: config.Heartbeat, authTimeout: config.RequestTimeout, attempts: config.MaxAttempts, client: config.Client, socketURL: socketURL,
		sleep: config.Sleep, now: config.Now, dial: dialProviderSocket, ownedClient: !injected, allowWS: injected,
		routes: newRouteStore[weComRoute](4096, 2*time.Hour, config.Now),
		seen:   newRouteStore[struct{}](8192, 24*time.Hour, config.Now),
	}, nil
}

func validateWeComSocketURL(raw string, allowWS bool) (string, error) {
	if strings.TrimSpace(raw) == "" {
		raw = weComSocketURL
	}
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", ErrInvalid
	}
	if parsed.Scheme != "wss" && !(allowWS && parsed.Scheme == "ws") {
		return "", ErrInvalid
	}
	if !allowWS && !trustedProviderHost(parsed.Hostname(), weComSocketHost) {
		return "", ErrInvalid
	}
	return parsed.String(), nil
}

// Name returns the normalized provider name.
func (*WeCom) Name() string { return WeComProvider }

// Start authenticates the WebSocket before launching reconnecting ingress.
func (provider *WeCom) Start(parent context.Context, submit SubmitFunc) error {
	if provider == nil || parent == nil || submit == nil {
		return ErrInvalid
	}
	provider.startMu.Lock()
	defer provider.startMu.Unlock()
	provider.mu.Lock()
	if provider.closed || provider.started {
		provider.mu.Unlock()
		if provider.closed {
			return ErrClosed
		}
		return nil
	}
	provider.mu.Unlock()
	socket, err := provider.openAuthenticatedSocket(parent)
	if err != nil {
		return fmt.Errorf("open WeCom AI Bot socket: %w", err)
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

// Send replies over the authenticated socket using the original callback route.
func (provider *WeCom) Send(ctx context.Context, reply Reply) error {
	if provider == nil || ctx == nil || reply.Provider != WeComProvider || strings.TrimSpace(reply.ChatID) == "" || strings.TrimSpace(reply.Text) == "" {
		return ErrInvalid
	}
	route, exists := provider.routes.Get(reply.InReplyTo)
	if !exists {
		return fmt.Errorf("%w: WeCom reply route expired", ErrInvalid)
	}
	for index, chunk := range splitUTF16(reply.Text, weComTextMax) {
		if err := retryProvider(ctx, provider.attempts, provider.sleep, func() error {
			return provider.sendChunk(ctx, route, reply, chunk, index)
		}); err != nil {
			return fmt.Errorf("send WeCom message: %w", err)
		}
	}
	provider.routes.Delete(reply.InReplyTo)
	return nil
}

// Close stops reconnects and releases the active socket.
func (provider *WeCom) Close() error {
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

func (provider *WeCom) openAuthenticatedSocket(ctx context.Context) (providerSocket, error) {
	authCtx, cancel := context.WithTimeout(ctx, provider.authTimeout)
	defer cancel()
	socket, err := provider.dial(authCtx, provider.socketURL, provider.client)
	if err != nil {
		return nil, err
	}
	requestID := provider.requestID(weComSubscribe, "")
	auth := weComOutboundFrame{
		Command: weComSubscribe, Headers: weComHeaders{RequestID: requestID},
		Body: map[string]string{"bot_id": provider.botID, "secret": provider.botSecret},
	}
	if err = writeWeComFrame(authCtx, socket, auth); err != nil {
		_ = socket.Close()
		return nil, err
	}
	for {
		payload, readErr := socket.Read(authCtx)
		if readErr != nil {
			_ = socket.Close()
			return nil, sanitizeProviderNetworkError(readErr)
		}
		var frame weComFrame
		if json.Unmarshal(payload, &frame) != nil || frame.Headers.RequestID != requestID {
			continue
		}
		if frame.ErrCode != nil && *frame.ErrCode != 0 {
			_ = socket.Close()
			return nil, fmt.Errorf("WeCom authentication failed: %s", boundedProviderMessage([]byte(frame.ErrMsg)))
		}
		return socket, nil
	}
}

func (provider *WeCom) run(ctx context.Context, submit SubmitFunc, socket providerSocket) {
	for ctx.Err() == nil {
		reconnect := provider.serve(ctx, submit, socket)
		_ = socket.Close()
		provider.clearSocket(socket)
		if !reconnect || ctx.Err() != nil {
			return
		}
		for failures := 1; ctx.Err() == nil; failures++ {
			if provider.sleep(ctx, providerReconnectDelay(failures)) != nil {
				return
			}
			next, err := provider.openAuthenticatedSocket(ctx)
			if err != nil {
				continue
			}
			socket = next
			provider.setSocket(socket)
			break
		}
	}
}

func (provider *WeCom) serve(ctx context.Context, submit SubmitFunc, socket providerSocket) bool {
	heartbeatCtx, cancel := context.WithCancel(ctx)
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		provider.sendHeartbeats(heartbeatCtx, socket)
	}()
	defer func() { cancel(); <-heartbeatDone }()
	for ctx.Err() == nil {
		payload, err := socket.Read(ctx)
		if err != nil {
			return true
		}
		var frame weComFrame
		if json.Unmarshal(payload, &frame) != nil || frame.Command != weComCallback {
			continue
		}
		message, route, keep := provider.normalize(frame)
		if !keep {
			continue
		}
		if _, duplicate := provider.seen.Get(message.ID); duplicate {
			continue
		}
		provider.seen.Put(message.ID, struct{}{})
		provider.routes.Put(message.ID, route)
		if err = provider.sendWorking(ctx, socket, route); err != nil {
			provider.seen.Delete(message.ID)
			return true
		}
		if err = submitProviderMessage(ctx, submit, message); err != nil && !errors.Is(err, ErrDuplicate) {
			provider.seen.Delete(message.ID)
			return !errors.Is(err, ErrClosed) && ctx.Err() == nil
		}
	}
	return false
}

func (provider *WeCom) sendHeartbeats(ctx context.Context, socket providerSocket) {
	ticker := time.NewTicker(provider.heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			frame := weComOutboundFrame{Command: weComHeartbeat, Headers: weComHeaders{RequestID: provider.requestID(weComHeartbeat, "")}}
			if provider.write(ctx, socket, frame) != nil {
				_ = socket.Close()
				return
			}
		}
	}
}

func (provider *WeCom) normalize(frame weComFrame) (Message, weComRoute, bool) {
	var body weComMessageBody
	if json.Unmarshal(frame.Body, &body) != nil {
		return Message{}, weComRoute{}, false
	}
	userID := strings.TrimSpace(body.From.UserID)
	messageID := firstString(body.MessageID, frame.Headers.MessageID, frame.Headers.RequestID)
	if userID == "" || messageID == "" || frame.Headers.RequestID == "" || !allowed(provider.allowedUsers, userID) {
		return Message{}, weComRoute{}, false
	}
	text, attachments := parseWeComBody(messageID, body)
	if text == "" && len(attachments) == 0 {
		return Message{}, weComRoute{}, false
	}
	chatID := userID
	return Message{
			ID: messageID, Provider: WeComProvider, WorkspaceID: strings.TrimSpace(body.AIbotID),
			ExternalUserID: userID, ChatID: chatID, TopicID: userID, Text: text, Attachments: attachments,
			Metadata:   map[string]string{"aibot_id": strings.TrimSpace(body.AIbotID), "chat_type": strings.TrimSpace(body.ChatType), "message_type": strings.TrimSpace(body.MsgType)},
			ReceivedAt: provider.now().UTC(),
		}, weComRoute{
			RequestID: frame.Headers.RequestID, ChatID: firstString(frame.Headers.ChatID, chatID),
			StreamID: provider.requestID("stream", messageID),
		}, true
}

func parseWeComBody(messageID string, body weComMessageBody) (string, []Attachment) {
	texts := make([]string, 0)
	attachments := make([]Attachment, 0)
	appendText := func(value string) {
		if value = strings.TrimSpace(value); value != "" {
			texts = append(texts, value)
		}
	}
	switch body.MsgType {
	case "text":
		appendText(body.Text.Content)
		if quote := strings.TrimSpace(body.Quote.Text.Content); quote != "" {
			appendText("Quote message: " + quote)
		}
	case "voice":
		appendText(body.Voice.Content)
	case "image":
		appendWeComAttachment(&attachments, messageID, 0, body.Image, "wecom-image", "image/*")
	case "file":
		appendWeComAttachment(&attachments, messageID, 0, body.File, firstString(body.File.Name, "wecom-file"), "application/octet-stream")
	case "mixed":
		for index := range body.Mixed.Items {
			item := body.Mixed.Items[index]
			switch item.MsgType {
			case "text":
				appendText(item.Text.Content)
			case "image":
				appendWeComAttachment(&attachments, messageID, index, item.Image, "wecom-image", "image/*")
			case "file":
				appendWeComAttachment(&attachments, messageID, index, item.File, firstString(item.File.Name, "wecom-file"), "application/octet-stream")
			}
		}
	}
	return strings.Join(texts, "\n\n"), attachments
}

func appendWeComAttachment(attachments *[]Attachment, messageID string, index int, media weComMedia, name, mediaType string) {
	if strings.TrimSpace(media.URL) == "" {
		return
	}
	digest := sha256.Sum256([]byte(messageID + "\xff" + strconv.Itoa(index)))
	*attachments = append(*attachments, Attachment{
		Name: boundedFilename(name), MediaType: mediaType, URL: "wecom://attachment/" + hex.EncodeToString(digest[:16]),
	})
}

func (provider *WeCom) sendChunk(ctx context.Context, route weComRoute, reply Reply, text string, index int) error {
	provider.mu.Lock()
	socket := provider.socket
	provider.mu.Unlock()
	if socket == nil {
		return &providerNetworkError{message: "WeCom socket is not connected"}
	}
	frame := weComOutboundFrame{
		Command: weComRespondMessage, Headers: weComHeaders{RequestID: route.RequestID},
		Body: map[string]any{"msgtype": "stream", "stream": map[string]any{"id": route.StreamID, "finish": true, "content": text}},
	}
	if index > 0 {
		frame.Command = weComSendMessage
		frame.Headers.RequestID = provider.requestID(weComSendMessage, reply.InReplyTo+strconv.Itoa(index))
		frame.Body = map[string]any{"chatid": firstString(route.ChatID, reply.ChatID), "msgtype": "markdown", "markdown": map[string]string{"content": text}}
	}
	return provider.write(ctx, socket, frame)
}

func (provider *WeCom) sendWorking(ctx context.Context, socket providerSocket, route weComRoute) error {
	frame := weComOutboundFrame{
		Command: weComRespondMessage, Headers: weComHeaders{RequestID: route.RequestID},
		Body: map[string]any{
			"msgtype": "stream",
			"stream":  map[string]any{"id": route.StreamID, "finish": false, "content": provider.workingMessage},
		},
	}
	return provider.write(ctx, socket, frame)
}

func (provider *WeCom) write(ctx context.Context, socket providerSocket, frame weComOutboundFrame) error {
	provider.writeMu.Lock()
	defer provider.writeMu.Unlock()
	return writeWeComFrame(ctx, socket, frame)
}

func writeWeComFrame(ctx context.Context, socket providerSocket, frame weComOutboundFrame) error {
	payload, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	if err = socket.Write(ctx, payload); err != nil {
		return sanitizeProviderNetworkError(err)
	}
	return nil
}

func (provider *WeCom) requestID(prefix, key string) string {
	digest := sha256.Sum256([]byte(provider.botID + "\xff" + key + "\xff" + strconv.FormatInt(provider.now().UnixNano(), 10)))
	return prefix + "_" + hex.EncodeToString(digest[:8])
}

func (provider *WeCom) setSocket(socket providerSocket) {
	provider.mu.Lock()
	if !provider.closed {
		provider.socket = socket
	}
	provider.mu.Unlock()
}

func (provider *WeCom) clearSocket(socket providerSocket) {
	provider.mu.Lock()
	if provider.socket == socket {
		provider.socket = nil
	}
	provider.mu.Unlock()
}
