package channel

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// WeChatProvider is the normalized WeChat iLink channel name.
	WeChatProvider    = "wechat"
	weChatAPIBase     = "https://ilinkai.weixin.qq.com"
	weChatTextMax     = 20_000
	weChatMessageText = 1
	weChatMessageIn   = 1
	weChatMessageOut  = 2
	weChatStateDone   = 2
)

var errWeChatSessionExpired = errors.New("WeChat iLink session expired")

// WeChatConfig controls iLink long polling and replies.
type WeChatConfig struct {
	BotToken       string
	ILinkBotID     string
	ILinkAppID     string
	RouteTag       string
	ChannelVersion string
	AllowedUsers   []string
	PollTimeout    time.Duration
	RequestTimeout time.Duration
	MaxAttempts    int
	Client         *http.Client
	BaseURL        string
	Sleep          func(context.Context, time.Duration) error
	Now            func() time.Time
	UIN            func() (string, error)
}

// WeChat is a WeChat iLink long-polling provider.
type WeChat struct {
	token          string
	botID          string
	appID          string
	routeTag       string
	channelVersion string
	clientVersion  string
	allowedUsers   map[string]struct{}
	pollTimeout    time.Duration
	attempts       int
	client         *http.Client
	baseURL        string
	sleep          func(context.Context, time.Duration) error
	now            func() time.Time
	uin            func() (string, error)
	ownedClient    bool
	contexts       *routeStore[string]

	startMu sync.Mutex
	mu      sync.Mutex
	started bool
	closed  bool
	cursor  string
	cancel  context.CancelFunc
	done    chan struct{}
	once    sync.Once
}

type weChatUpdates struct {
	Ret                 int             `json:"ret"`
	ErrCode             int             `json:"errcode"`
	ErrMsg              string          `json:"errmsg"`
	Messages            []weChatMessage `json:"msgs"`
	Cursor              string          `json:"get_updates_buf"`
	LongPollTimeoutMsec int             `json:"longpolling_timeout_ms"`
}

type weChatMessage struct {
	MessageID   string       `json:"message_id"`
	MsgID       string       `json:"msg_id"`
	ClientID    string       `json:"client_id"`
	FromUserID  string       `json:"from_user_id"`
	ILinkUserID string       `json:"ilink_user_id"`
	MessageType int          `json:"message_type"`
	Context     string       `json:"context_token"`
	Items       []weChatItem `json:"item_list"`
}

type weChatItem struct {
	Type     int `json:"type"`
	TextItem struct {
		Text string `json:"text"`
	} `json:"text_item"`
	ImageItem struct {
		Media weChatMedia `json:"media"`
	} `json:"image_item"`
	FileItem struct {
		Media    weChatMedia `json:"media"`
		FileName string      `json:"file_name"`
	} `json:"file_item"`
}

type weChatMedia struct {
	FullURL string `json:"full_url"`
}

// NewWeChat validates configuration and creates an iLink provider.
func NewWeChat(config WeChatConfig) (*WeChat, error) {
	token := strings.TrimSpace(config.BotToken)
	if token == "" || len(token) > 4096 || len(config.ILinkBotID) > 512 || len(config.ILinkAppID) > 512 || len(config.RouteTag) > 256 {
		return nil, ErrInvalid
	}
	if config.ChannelVersion == "" {
		config.ChannelVersion = "1.0"
	}
	clientVersion, valid := weChatClientVersion(config.ChannelVersion)
	if !valid {
		return nil, ErrInvalid
	}
	if config.PollTimeout == 0 {
		config.PollTimeout = 35 * time.Second
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = config.PollTimeout + 10*time.Second
	}
	if config.MaxAttempts == 0 {
		config.MaxAttempts = 3
	}
	if !validWeChatTiming(config.PollTimeout, config.RequestTimeout, config.MaxAttempts) || !validChannelValueSet(config.AllowedUsers) {
		return nil, ErrInvalid
	}
	baseURL, err := resolveProviderBaseURL(config.BaseURL, weChatAPIBase, config.Client != nil)
	if err != nil {
		return nil, ErrInvalid
	}
	owned := config.Client == nil
	if config.Client == nil {
		config.Client = newProviderClient(config.RequestTimeout)
	}
	if config.Sleep == nil {
		config.Sleep = sleepContext
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.UIN == nil {
		config.UIN = randomWeChatUIN
	}
	return &WeChat{
		token: token, botID: strings.TrimSpace(config.ILinkBotID), appID: strings.TrimSpace(config.ILinkAppID), routeTag: strings.TrimSpace(config.RouteTag),
		channelVersion: config.ChannelVersion, clientVersion: clientVersion, allowedUsers: normalizedSet(config.AllowedUsers),
		pollTimeout: config.PollTimeout, attempts: config.MaxAttempts, client: config.Client, baseURL: baseURL,
		sleep: config.Sleep, now: config.Now, uin: config.UIN, ownedClient: owned,
		contexts: newRouteStore[string](4096, 24*time.Hour, config.Now),
	}, nil
}

func validWeChatTiming(pollTimeout, requestTimeout time.Duration, attempts int) bool {
	return pollTimeout >= time.Second && pollTimeout <= time.Minute && requestTimeout > pollTimeout && requestTimeout <= 2*time.Minute && validProviderAttempts(attempts)
}

func validChannelValueSet(values []string) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" || strings.TrimSpace(value) != value || len(value) > 512 {
			return false
		}
		if _, exists := seen[value]; exists {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

// Name returns the normalized provider name.
func (*WeChat) Name() string { return WeChatProvider }

// Start launches bounded iLink long polling.
func (provider *WeChat) Start(parent context.Context, submit SubmitFunc) error {
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
	ctx, cancel := context.WithCancel(parent)
	provider.cancel, provider.done, provider.started = cancel, make(chan struct{}), true
	done := provider.done
	provider.mu.Unlock()
	go provider.poll(ctx, submit, done)
	return nil
}

// Send replies with the context token carried by the inbound iLink message.
func (provider *WeChat) Send(ctx context.Context, reply Reply) error {
	if provider == nil || ctx == nil || reply.Provider != WeChatProvider || strings.TrimSpace(reply.ChatID) == "" || strings.TrimSpace(reply.Text) == "" {
		return ErrInvalid
	}
	contextToken, exists := provider.contexts.Get(reply.InReplyTo)
	if !exists {
		return fmt.Errorf("%w: WeChat context token expired", ErrInvalid)
	}
	for index, chunk := range splitUTF16(reply.Text, weChatTextMax) {
		if err := retryProvider(ctx, provider.attempts, provider.sleep, func() error {
			return provider.sendChunk(ctx, reply, contextToken, chunk, index)
		}); err != nil {
			return fmt.Errorf("send WeChat message: %w", err)
		}
	}
	provider.contexts.Delete(reply.InReplyTo)
	return nil
}

// Close idempotently stops polling and owned HTTP resources.
func (provider *WeChat) Close() error {
	if provider == nil {
		return nil
	}
	provider.once.Do(func() {
		provider.mu.Lock()
		provider.closed = true
		cancel, done, started := provider.cancel, provider.done, provider.started
		provider.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if started && done != nil {
			<-done
		}
		if provider.ownedClient {
			provider.client.CloseIdleConnections()
		}
	})
	return nil
}

func (provider *WeChat) poll(ctx context.Context, submit SubmitFunc, done chan struct{}) {
	defer close(done)
	for {
		err := provider.pollOnce(ctx, submit)
		if ctx.Err() != nil || errors.Is(err, errWeChatSessionExpired) {
			return
		}
		if err == nil {
			continue
		}
		if err = provider.sleep(ctx, providerReconnectDelay(1)); err != nil {
			return
		}
	}
}

func (provider *WeChat) pollOnce(ctx context.Context, submit SubmitFunc) error {
	provider.mu.Lock()
	cursor := provider.cursor
	provider.mu.Unlock()
	var response weChatUpdates
	if err := provider.request(ctx, "/ilink/bot/getupdates", map[string]any{
		"get_updates_buf": cursor, "base_info": provider.baseInfo(),
	}, &response); err != nil {
		return err
	}
	if err := weChatResponseError(response.Ret, response.ErrCode, response.ErrMsg); err != nil {
		return err
	}
	for index := range response.Messages {
		message, contextToken, keep := provider.normalize(response.Messages[index])
		if !keep {
			continue
		}
		provider.contexts.Put(message.ID, contextToken)
		if err := submitProviderMessage(ctx, submit, message); err != nil {
			if !errors.Is(err, ErrDuplicate) {
				provider.contexts.Delete(message.ID)
			}
			return err
		}
	}
	provider.mu.Lock()
	provider.cursor = response.Cursor
	if response.LongPollTimeoutMsec >= 1000 && response.LongPollTimeoutMsec <= 60_000 {
		provider.pollTimeout = time.Duration(response.LongPollTimeoutMsec) * time.Millisecond
	}
	provider.mu.Unlock()
	return nil
}

func (provider *WeChat) normalize(incoming weChatMessage) (Message, string, bool) {
	if incoming.MessageType != weChatMessageIn {
		return Message{}, "", false
	}
	userID := strings.TrimSpace(incoming.FromUserID)
	if userID == "" {
		userID = strings.TrimSpace(incoming.ILinkUserID)
	}
	messageID := firstString(incoming.MessageID, incoming.MsgID, incoming.ClientID, incoming.Context)
	contextToken := strings.TrimSpace(incoming.Context)
	if userID == "" || messageID == "" || contextToken == "" {
		return Message{}, "", false
	}
	text, attachments := parseWeChatItems(messageID, incoming.Items)
	_, connecting := ParseConnectCommand(text, WeChatProvider)
	if !connecting && !allowed(provider.allowedUsers, userID) {
		return Message{}, "", false
	}
	if text == "" && len(attachments) == 0 {
		return Message{}, "", false
	}
	return Message{
		ID: messageID, Provider: WeChatProvider, WorkspaceID: userID, ExternalUserID: userID, ChatID: userID,
		Text: text, Attachments: attachments, Metadata: map[string]string{
			"context_token": contextToken, "ilink_bot_id": provider.botID,
		}, ReceivedAt: provider.now().UTC(),
	}, contextToken, true
}

func parseWeChatItems(messageID string, items []weChatItem) (string, []Attachment) {
	texts := make([]string, 0)
	attachments := make([]Attachment, 0)
	for index := range items {
		item := items[index]
		switch item.Type {
		case weChatMessageText:
			if text := strings.TrimSpace(item.TextItem.Text); text != "" {
				texts = append(texts, text)
			}
		case 2:
			if strings.TrimSpace(item.ImageItem.Media.FullURL) != "" {
				attachments = append(attachments, weChatAttachment(messageID, index, "wechat-image", "image/*"))
			}
		case 4:
			if strings.TrimSpace(item.FileItem.Media.FullURL) != "" {
				name := boundedFilename(item.FileItem.FileName)
				if name == "" {
					name = "wechat-file"
				}
				attachments = append(attachments, weChatAttachment(messageID, index, name, "application/octet-stream"))
			}
		}
	}
	return strings.Join(texts, "\n"), attachments
}

func weChatAttachment(messageID string, index int, name, mediaType string) Attachment {
	digest := sha256.Sum256([]byte(messageID + "\xff" + strconv.Itoa(index)))
	return Attachment{Name: name, MediaType: mediaType, URL: "wechat://attachment/" + hex.EncodeToString(digest[:16])}
}

func (provider *WeChat) sendChunk(ctx context.Context, reply Reply, contextToken, text string, index int) error {
	payload := map[string]any{
		"msg": map[string]any{
			"from_user_id": "", "to_user_id": reply.ChatID, "client_id": weChatClientID(reply, index),
			"message_type": weChatMessageOut, "message_state": weChatStateDone, "context_token": contextToken,
			"item_list": []any{map[string]any{"type": weChatMessageText, "text_item": map[string]string{"text": text}}},
		},
		"base_info": provider.baseInfo(),
	}
	var response weChatUpdates
	if err := provider.request(ctx, "/ilink/bot/sendmessage", payload, &response); err != nil {
		return err
	}
	return weChatResponseError(response.Ret, response.ErrCode, response.ErrMsg)
}

func (provider *WeChat) request(ctx context.Context, path string, input, output any) error {
	uin, err := provider.uin()
	if err != nil {
		return err
	}
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+provider.token)
	headers.Set("AuthorizationType", "ilink_bot_token")
	headers.Set("X-WECHAT-UIN", uin)
	headers.Set("iLink-App-ClientVersion", provider.clientVersion)
	if provider.appID != "" {
		headers.Set("iLink-App-Id", provider.appID)
	}
	if provider.routeTag != "" {
		headers.Set("SKRouteTag", provider.routeTag)
	}
	return requestJSONHeaders(ctx, provider.client, http.MethodPost, provider.baseURL+path, headers, input, output)
}

func (provider *WeChat) baseInfo() map[string]string {
	return map[string]string{"channel_version": provider.channelVersion}
}

func weChatResponseError(ret, errCode int, message string) error {
	if ret == 0 && errCode == 0 {
		return nil
	}
	if ret == -14 || errCode == -14 {
		return errWeChatSessionExpired
	}
	detail := boundedProviderMessage([]byte(message))
	return &providerHTTPError{status: http.StatusServiceUnavailable, message: fmt.Sprintf("iLink ret=%d errcode=%d: %s", ret, errCode, detail)}
}

func weChatClientID(reply Reply, index int) string {
	digest := sha256.Sum256([]byte(reply.WorkspaceID + "\xff" + reply.ChatID + "\xff" + reply.InReplyTo + "\xff" + strconv.Itoa(index)))
	return "gofer_" + hex.EncodeToString(digest[:16])
}

func weChatClientVersion(version string) (string, bool) {
	parts := strings.Split(version, ".")
	if len(parts) > 3 || len(parts) == 0 {
		return "", false
	}
	values := [3]int{}
	for index, part := range parts {
		parsed, err := strconv.Atoi(part)
		if err != nil || parsed < 0 || parsed > 255 {
			return "", false
		}
		values[index] = parsed
	}
	return strconv.Itoa(values[0]<<16 | values[1]<<8 | values[2]), true
}

func randomWeChatUIN() (string, error) {
	buffer := make([]byte, 4)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	value := strconv.FormatUint(uint64(binary.BigEndian.Uint32(buffer)), 10)
	return base64.StdEncoding.EncodeToString([]byte(value)), nil
}

func firstString(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
