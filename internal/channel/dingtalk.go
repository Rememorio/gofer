package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/open-dingtalk/dingtalk-stream-sdk-go/chatbot"
	dingclient "github.com/open-dingtalk/dingtalk-stream-sdk-go/client"
)

const (
	// DingTalkProvider is the normalized DingTalk channel name.
	DingTalkProvider          = "dingtalk"
	dingTalkAPIBase           = "https://api.dingtalk.com"
	dingTalkConversationP2P   = "1"
	dingTalkConversationGroup = "2"
	dingTalkTextMax           = 15_000
)

// DingTalkConfig controls DingTalk Stream events and OpenAPI replies.
type DingTalkConfig struct {
	ClientID       string
	ClientSecret   string
	AllowedUsers   []string
	RequestTimeout time.Duration
	MaxAttempts    int
	Sleep          func(context.Context, time.Duration) error
	Now            func() time.Time
}

// DingTalk is a DingTalk Stream Mode provider.
type DingTalk struct {
	clientID     string
	clientSecret string
	allowedUsers map[string]struct{}
	client       *http.Client
	apiBase      string
	attempts     int
	sleep        func(context.Context, time.Duration) error
	now          func() time.Time
	stream       dingTalkEventStream
	routes       *routeStore[dingTalkRoute]

	startMu      sync.Mutex
	mu           sync.Mutex
	started      bool
	closed       bool
	submit       SubmitFunc
	accessToken  string
	tokenExpires time.Time
	once         sync.Once
}

type dingTalkRoute struct {
	ConversationType string
	ConversationID   string
	SenderStaffID    string
}

type dingTalkEventStream interface {
	Start(context.Context) error
	Close()
}

// NewDingTalk validates configuration and creates a DingTalk provider.
func NewDingTalk(config DingTalkConfig) (*DingTalk, error) {
	clientID, clientSecret := strings.TrimSpace(config.ClientID), strings.TrimSpace(config.ClientSecret)
	if clientID == "" || clientSecret == "" || len(clientID) > 512 || len(clientSecret) > 1024 {
		return nil, ErrInvalid
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = 30 * time.Second
	}
	if config.MaxAttempts == 0 {
		config.MaxAttempts = 3
	}
	if config.RequestTimeout < time.Second || config.RequestTimeout > 2*time.Minute || !validProviderAttempts(config.MaxAttempts) {
		return nil, ErrInvalid
	}
	if config.Sleep == nil {
		config.Sleep = sleepContext
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	provider := &DingTalk{
		clientID: clientID, clientSecret: clientSecret, allowedUsers: normalizedSet(config.AllowedUsers),
		client: newProviderClient(config.RequestTimeout), apiBase: dingTalkAPIBase,
		attempts: config.MaxAttempts, sleep: config.Sleep, now: config.Now,
		routes: newRouteStore[dingTalkRoute](4096, 2*time.Hour, config.Now),
	}
	provider.stream = newDingTalkSDKStream(clientID, clientSecret, provider.handleCallback)
	return provider, nil
}

// Name returns the normalized provider name.
func (*DingTalk) Name() string { return DingTalkProvider }

// Start establishes DingTalk Stream Mode and installs bounded ingress.
func (provider *DingTalk) Start(ctx context.Context, submit SubmitFunc) error {
	if provider == nil || ctx == nil || submit == nil {
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
	provider.submit = submit
	provider.mu.Unlock()
	if err := provider.stream.Start(ctx); err != nil {
		provider.mu.Lock()
		provider.submit = nil
		provider.mu.Unlock()
		return fmt.Errorf("start DingTalk stream: %w", err)
	}
	provider.mu.Lock()
	if provider.closed {
		provider.mu.Unlock()
		provider.stream.Close()
		return ErrClosed
	}
	provider.started = true
	provider.mu.Unlock()
	return nil
}

// Send delivers a group or direct Markdown reply through DingTalk OpenAPI.
func (provider *DingTalk) Send(ctx context.Context, reply Reply) error {
	if provider == nil || ctx == nil || reply.Provider != DingTalkProvider || strings.TrimSpace(reply.ChatID) == "" || strings.TrimSpace(reply.Text) == "" {
		return ErrInvalid
	}
	route, exists := provider.routes.Get(reply.InReplyTo)
	if !exists {
		return fmt.Errorf("%w: DingTalk reply route expired", ErrInvalid)
	}
	for _, chunk := range splitUTF16(reply.Text, dingTalkTextMax) {
		if err := retryProvider(ctx, provider.attempts, provider.sleep, func() error {
			return provider.sendChunk(ctx, route, chunk)
		}); err != nil {
			return fmt.Errorf("send DingTalk message: %w", err)
		}
	}
	provider.routes.Delete(reply.InReplyTo)
	return nil
}

// Close idempotently stops Stream Mode and owned HTTP resources.
func (provider *DingTalk) Close() error {
	if provider == nil {
		return nil
	}
	provider.once.Do(func() {
		provider.mu.Lock()
		provider.closed = true
		provider.mu.Unlock()
		provider.stream.Close()
		provider.client.CloseIdleConnections()
	})
	return nil
}

func (provider *DingTalk) handleCallback(ctx context.Context, data *chatbot.BotCallbackDataModel) ([]byte, error) {
	message, route, keep := provider.normalize(data)
	if !keep {
		return nil, nil
	}
	provider.routes.Put(message.ID, route)
	provider.mu.Lock()
	submit := provider.submit
	provider.mu.Unlock()
	if submit == nil {
		return nil, ErrClosed
	}
	if err := submit(ctx, message); err != nil {
		return nil, err
	}
	return nil, nil
}

func (provider *DingTalk) normalize(data *chatbot.BotCallbackDataModel) (Message, dingTalkRoute, bool) {
	if data == nil {
		return Message{}, dingTalkRoute{}, false
	}
	conversationType := normalizeDingTalkConversation(data.ConversationType)
	userID, messageID := strings.TrimSpace(data.SenderStaffId), strings.TrimSpace(data.MsgId)
	conversationID := strings.TrimSpace(data.ConversationId)
	chatID := userID
	if conversationType == dingTalkConversationGroup {
		chatID = conversationID
	}
	if userID == "" || messageID == "" || chatID == "" || !allowed(provider.allowedUsers, userID) {
		return Message{}, dingTalkRoute{}, false
	}
	text, attachments := parseDingTalkContent(data)
	if text == "" && len(attachments) == 0 {
		return Message{}, dingTalkRoute{}, false
	}
	workspaceID, topicID := "", ""
	if conversationType == dingTalkConversationGroup {
		workspaceID, topicID = conversationID, messageID
	}
	route := dingTalkRoute{ConversationType: conversationType, ConversationID: conversationID, SenderStaffID: userID}
	return Message{
		ID: messageID, Provider: DingTalkProvider, WorkspaceID: workspaceID,
		ExternalUserID: userID, ChatID: chatID, TopicID: topicID,
		Text: text, Attachments: attachments,
		Metadata: map[string]string{
			"conversation_type": conversationType, "conversation_id": conversationID,
			"sender_nick": strings.TrimSpace(data.SenderNick), "message_type": strings.TrimSpace(data.Msgtype),
			"sender_corp_id": strings.TrimSpace(data.SenderCorpId), "chatbot_corp_id": strings.TrimSpace(data.ChatbotCorpId),
		},
		ReceivedAt: millisecondsOrNow(data.CreateAt, provider.now()),
	}, route, true
}

func parseDingTalkContent(data *chatbot.BotCallbackDataModel) (string, []Attachment) {
	text := strings.TrimSpace(data.Text.Content)
	attachments := make([]Attachment, 0)
	walkDingTalkContent(data.Content, &text, &attachments)
	return strings.TrimSpace(text), attachments
}

func walkDingTalkContent(content any, text *string, attachments *[]Attachment) {
	switch typed := content.(type) {
	case []any:
		for _, item := range typed {
			walkDingTalkContent(item, text, attachments)
		}
	case map[string]any:
		if candidate, ok := typed["text"].(string); ok && strings.TrimSpace(candidate) != "" && !containsDingTalkText(*text, candidate) {
			*text = strings.TrimSpace(*text + "\n" + candidate)
		}
		appendDingTalkAttachment(typed, attachments)
		for key, item := range typed {
			if key != "text" && key != "downloadCode" && key != "download_code" {
				walkDingTalkContent(item, text, attachments)
			}
		}
	}
}

func containsDingTalkText(text, candidate string) bool {
	candidate = strings.TrimSpace(candidate)
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == candidate {
			return true
		}
	}
	return false
}

func appendDingTalkAttachment(content map[string]any, attachments *[]Attachment) {
	code := stringMapValue(content, "downloadCode", "download_code")
	if code == "" {
		return
	}
	name := stringMapValue(content, "fileName", "filename")
	if name == "" {
		name = "dingtalk-file"
	}
	mediaType := "application/octet-stream"
	if strings.Contains(strings.ToLower(stringMapValue(content, "type", "msgtype")), "picture") {
		mediaType = "image/*"
	}
	*attachments = append(*attachments, Attachment{Name: boundedFilename(name), MediaType: mediaType, URL: "dingtalk://download/" + url.PathEscape(code)})
}

func stringMapValue(content map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := content[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func boundedFilename(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > 120 {
		value = string(runes[:120])
	}
	return value
}

func normalizeDingTalkConversation(value string) string {
	if strings.TrimSpace(value) == dingTalkConversationGroup {
		return dingTalkConversationGroup
	}
	return dingTalkConversationP2P
}

func millisecondsOrNow(milliseconds int64, fallback time.Time) time.Time {
	if milliseconds <= 0 {
		return fallback.UTC()
	}
	return time.UnixMilli(milliseconds).UTC()
}

func (provider *DingTalk) sendChunk(ctx context.Context, route dingTalkRoute, text string) error {
	token, err := provider.token(ctx)
	if err != nil {
		return err
	}
	parameter, _ := json.Marshal(map[string]string{"title": "Gofer", "text": text})
	input := map[string]any{"msgKey": "sampleMarkdown", "msgParam": string(parameter), "robotCode": provider.clientID}
	endpoint := provider.apiBase + "/v1.0/robot/oToMessages/batchSend"
	if route.ConversationType == dingTalkConversationGroup {
		endpoint = provider.apiBase + "/v1.0/robot/groupMessages/send"
		input["openConversationId"] = route.ConversationID
		if route.SenderStaffID != "" {
			input["atUserIds"] = []string{route.SenderStaffID}
		}
	} else {
		input["userIds"] = []string{route.SenderStaffID}
	}
	headers := make(http.Header)
	headers.Set("x-acs-dingtalk-access-token", token)
	var output map[string]any
	err = requestJSONHeaders(ctx, provider.client, http.MethodPost, endpoint, headers, input, &output)
	var failure *providerHTTPError
	if errors.As(err, &failure) && failure.status == http.StatusUnauthorized {
		provider.invalidateToken(token)
		return &providerHTTPError{status: http.StatusServiceUnavailable, message: "DingTalk access token expired"}
	}
	return err
}

func (provider *DingTalk) token(ctx context.Context) (string, error) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.accessToken != "" && provider.now().Before(provider.tokenExpires) {
		return provider.accessToken, nil
	}
	var response struct {
		AccessToken string `json:"accessToken"`
		ExpireIn    any    `json:"expireIn"`
	}
	if err := requestJSON(ctx, provider.client, http.MethodPost, provider.apiBase+"/v1.0/oauth2/accessToken", "", map[string]string{"appKey": provider.clientID, "appSecret": provider.clientSecret}, &response); err != nil {
		return "", err
	}
	token := strings.TrimSpace(response.AccessToken)
	if token == "" {
		return "", errors.New("DingTalk access token is empty")
	}
	expires := dingTalkExpires(response.ExpireIn)
	margin := 5 * time.Minute
	if expires <= 10*time.Minute {
		margin = expires / 2
	}
	provider.accessToken, provider.tokenExpires = token, provider.now().Add(expires-margin)
	return token, nil
}

func dingTalkExpires(value any) time.Duration {
	seconds := int64(7200)
	switch typed := value.(type) {
	case float64:
		seconds = int64(typed)
	case string:
		if parsed, err := strconv.ParseInt(typed, 10, 64); err == nil {
			seconds = parsed
		}
	}
	if seconds < 60 || seconds > 86400 {
		seconds = 7200
	}
	return time.Duration(seconds) * time.Second
}

func (provider *DingTalk) invalidateToken(token string) {
	provider.mu.Lock()
	if provider.accessToken == token {
		provider.accessToken, provider.tokenExpires = "", time.Time{}
	}
	provider.mu.Unlock()
}

type dingTalkSDKStream struct{ client *dingclient.StreamClient }

func newDingTalkSDKStream(clientID, clientSecret string, handler chatbot.IChatBotMessageHandler) *dingTalkSDKStream {
	client := dingclient.NewStreamClient(dingclient.WithAppCredential(dingclient.NewAppCredentialConfig(clientID, clientSecret)))
	client.RegisterChatBotCallbackRouter(handler)
	return &dingTalkSDKStream{client: client}
}

func (stream *dingTalkSDKStream) Start(ctx context.Context) error { return stream.client.Start(ctx) }
func (stream *dingTalkSDKStream) Close() {
	stream.client.AutoReconnect = false
	stream.client.Close()
}
