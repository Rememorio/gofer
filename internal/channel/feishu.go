package channel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

const (
	// FeishuProvider is the normalized Feishu/Lark channel name.
	FeishuProvider = "feishu"
	feishuDomain   = "https://open.feishu.cn"
	feishuTextMax  = 30_000
)

// FeishuConfig controls Feishu/Lark long-connection events and replies.
type FeishuConfig struct {
	AppID          string
	AppSecret      string
	Domain         string
	AllowedUsers   []string
	RequestTimeout time.Duration
	MaxAttempts    int
	Sleep          func(context.Context, time.Duration) error
}

// Feishu is a Feishu/Lark long-connection provider.
type Feishu struct {
	allowedUsers map[string]struct{}
	attempts     int
	sleep        func(context.Context, time.Duration) error
	stream       feishuEventStream
	messenger    feishuMessenger

	startMu sync.Mutex
	mu      sync.Mutex
	started bool
	closed  bool
	submit  SubmitFunc
	once    sync.Once
}

type feishuEventStream interface {
	Start(context.Context) error
	Close()
}

type feishuMessenger interface {
	Send(context.Context, Reply, string) error
}

// NewFeishu validates configuration and creates a Feishu provider.
func NewFeishu(config FeishuConfig) (*Feishu, error) {
	appID, appSecret := strings.TrimSpace(config.AppID), strings.TrimSpace(config.AppSecret)
	if appID == "" || appSecret == "" || len(appID) > 512 || len(appSecret) > 1024 {
		return nil, ErrInvalid
	}
	if config.Domain == "" {
		config.Domain = feishuDomain
	}
	domain, err := validateFeishuDomain(config.Domain)
	if err != nil {
		return nil, err
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
	if config.Sleep == nil {
		config.Sleep = sleepContext
	}
	provider := &Feishu{allowedUsers: normalizedSet(config.AllowedUsers), attempts: config.MaxAttempts, sleep: config.Sleep}
	dispatcher := dispatcher.NewEventDispatcher("", "").OnP2MessageReceiveV1(provider.handleEvent)
	provider.stream = newLarkEventStream(appID, appSecret, domain, dispatcher)
	provider.messenger = newLarkMessenger(appID, appSecret, domain, config.RequestTimeout)
	return provider, nil
}

// Name returns the normalized provider name.
func (*Feishu) Name() string { return FeishuProvider }

// Start opens the SDK-managed long connection and waits until it is ready.
func (provider *Feishu) Start(ctx context.Context, submit SubmitFunc) error {
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
		return fmt.Errorf("start Feishu long connection: %w", err)
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

// Send replies to the inbound Feishu message with provider idempotency keys.
func (provider *Feishu) Send(ctx context.Context, reply Reply) error {
	if provider == nil || ctx == nil || reply.Provider != FeishuProvider || strings.TrimSpace(reply.ChatID) == "" || strings.TrimSpace(reply.Text) == "" {
		return ErrInvalid
	}
	for index, chunk := range splitUTF16(reply.Text, feishuTextMax) {
		chunkReply := reply
		chunkReply.Text = chunk
		idempotencyKey := feishuIdempotencyKey(reply, index)
		if err := retryProvider(ctx, provider.attempts, provider.sleep, func() error {
			return provider.messenger.Send(ctx, chunkReply, idempotencyKey)
		}); err != nil {
			return fmt.Errorf("send Feishu message: %w", err)
		}
	}
	return nil
}

// Close idempotently stops the long connection.
func (provider *Feishu) Close() error {
	if provider == nil {
		return nil
	}
	provider.once.Do(func() {
		provider.mu.Lock()
		provider.closed = true
		provider.mu.Unlock()
		provider.stream.Close()
	})
	return nil
}

func (provider *Feishu) handleEvent(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
	message, keep := provider.normalize(event)
	if !keep {
		return nil
	}
	provider.mu.Lock()
	submit := provider.submit
	provider.mu.Unlock()
	if submit == nil {
		return ErrClosed
	}
	return submit(ctx, message)
}

func (provider *Feishu) normalize(event *larkim.P2MessageReceiveV1) (Message, bool) {
	if event == nil || event.EventV2Base == nil || event.EventV2Base.Header == nil || event.Event == nil || event.Event.Message == nil || event.Event.Sender == nil || event.Event.Sender.SenderId == nil {
		return Message{}, false
	}
	source, incoming := event.Event.Sender, event.Event.Message
	if value(source.SenderType) == "bot" {
		return Message{}, false
	}
	userID := firstValue(source.SenderId.OpenId, source.SenderId.UserId, source.SenderId.UnionId)
	messageID, chatID := value(incoming.MessageId), value(incoming.ChatId)
	if userID == "" || messageID == "" || chatID == "" {
		return Message{}, false
	}
	text, attachments := parseFeishuContent(value(incoming.Content))
	_, connecting := ParseConnectCommand(text, FeishuProvider)
	if !connecting && !allowed(provider.allowedUsers, userID) {
		return Message{}, false
	}
	if text == "" && len(attachments) == 0 {
		return Message{}, false
	}
	topicID := ""
	if value(incoming.ChatType) != "p2p" {
		topicID = firstValue(incoming.RootId, incoming.ThreadId, incoming.ParentId, incoming.MessageId)
	}
	received := unixMilliseconds(value(incoming.CreateTime), time.Now())
	return Message{
		ID: messageID, Provider: FeishuProvider, WorkspaceID: chatID,
		ExternalUserID: userID, ChatID: chatID, TopicID: topicID,
		Text: text, Attachments: attachments,
		Metadata: map[string]string{
			"event_id": event.EventV2Base.Header.EventID, "tenant_key": event.EventV2Base.Header.TenantKey,
			"chat_type": value(incoming.ChatType), "message_type": value(incoming.MessageType),
		},
		ReceivedAt: received,
	}, true
}

func parseFeishuContent(raw string) (string, []Attachment) {
	var content any
	if json.Unmarshal([]byte(raw), &content) != nil {
		return "", nil
	}
	texts := make([]string, 0)
	attachments := make([]Attachment, 0)
	walkFeishuContent(content, &texts, &attachments)
	return strings.TrimSpace(strings.Join(texts, "\n\n")), attachments
}

func walkFeishuContent(content any, texts *[]string, attachments *[]Attachment) {
	switch typed := content.(type) {
	case []any:
		for _, item := range typed {
			walkFeishuContent(item, texts, attachments)
		}
	case map[string]any:
		appendFeishuMap(typed, texts, attachments)
		for key, item := range typed {
			if key != "text" && key != "image_key" && key != "file_key" {
				walkFeishuContent(item, texts, attachments)
			}
		}
	}
}

func appendFeishuMap(content map[string]any, texts *[]string, attachments *[]Attachment) {
	if text, ok := content["text"].(string); ok && strings.TrimSpace(text) != "" {
		*texts = append(*texts, strings.TrimSpace(text))
	}
	for _, descriptor := range []struct {
		key       string
		mediaType string
		name      string
	}{
		{key: "image_key", mediaType: "image/*", name: "feishu-image"},
		{key: "file_key", mediaType: "application/octet-stream", name: "feishu-file"},
	} {
		key, ok := content[descriptor.key].(string)
		if ok && strings.TrimSpace(key) != "" {
			*attachments = append(*attachments, Attachment{Name: descriptor.name, MediaType: descriptor.mediaType, URL: "feishu://" + descriptor.key + "/" + url.PathEscape(key)})
		}
	}
}

func validateFeishuDomain(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(raw), "/"))
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", ErrInvalid
	}
	if parsed.Hostname() != "open.feishu.cn" && parsed.Hostname() != "open.larksuite.com" {
		return "", ErrInvalid
	}
	return parsed.String(), nil
}

func feishuIdempotencyKey(reply Reply, index int) string {
	digest := sha256.Sum256([]byte(reply.WorkspaceID + "\xff" + reply.ChatID + "\xff" + reply.InReplyTo + "\xff" + strconv.Itoa(index)))
	return hex.EncodeToString(digest[:])
}

func unixMilliseconds(raw string, fallback time.Time) time.Time {
	milliseconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || milliseconds <= 0 {
		return fallback.UTC()
	}
	return time.UnixMilli(milliseconds).UTC()
}

func value(pointer *string) string {
	if pointer == nil {
		return ""
	}
	return strings.TrimSpace(*pointer)
}

func firstValue(pointers ...*string) string {
	for _, pointer := range pointers {
		if candidate := value(pointer); candidate != "" {
			return candidate
		}
	}
	return ""
}

type larkEventStream struct {
	client *larkws.Client
	ready  chan struct{}
	once   sync.Once
}

func newLarkEventStream(appID, appSecret, domain string, handler *dispatcher.EventDispatcher) *larkEventStream {
	stream := &larkEventStream{ready: make(chan struct{})}
	stream.client = larkws.NewClient(appID, appSecret,
		larkws.WithDomain(domain), larkws.WithEventHandler(handler),
		larkws.WithLogLevel(larkcore.LogLevelError),
		larkws.WithOnReady(func() { stream.once.Do(func() { close(stream.ready) }) }),
	)
	return stream
}

func (stream *larkEventStream) Start(ctx context.Context) error {
	result := make(chan error, 1)
	go func() { result <- stream.client.Start(ctx) }()
	select {
	case <-ctx.Done():
		stream.client.Close()
		return ctx.Err()
	case err := <-result:
		if err == nil {
			return ErrClosed
		}
		return err
	case <-stream.ready:
		return nil
	}
}

func (stream *larkEventStream) Close() { stream.client.Close() }

type larkMessenger struct{ client *lark.Client }

func newLarkMessenger(appID, appSecret, domain string, timeout time.Duration) *larkMessenger {
	return &larkMessenger{client: lark.NewClient(appID, appSecret, lark.WithOpenBaseUrl(domain), lark.WithReqTimeout(timeout), lark.WithLogLevel(larkcore.LogLevelError))}
}

func (messenger *larkMessenger) Send(ctx context.Context, reply Reply, idempotencyKey string) error {
	content, err := json.Marshal(map[string]string{"text": reply.Text})
	if err != nil {
		return err
	}
	if reply.InReplyTo != "" {
		body := larkim.NewReplyMessageReqBodyBuilder().Content(string(content)).MsgType("text").ReplyInThread(reply.TopicID != "").Uuid(idempotencyKey).Build()
		request := larkim.NewReplyMessageReqBuilder().MessageId(reply.InReplyTo).Body(body).Build()
		response, sendErr := messenger.client.Im.V1.Message.Reply(ctx, request)
		if sendErr != nil {
			return &providerNetworkError{message: sendErr.Error()}
		}
		if response == nil || !response.Success() {
			return larkResponseError(response)
		}
		return nil
	}
	body := larkim.NewCreateMessageReqBodyBuilder().ReceiveId(reply.ChatID).MsgType("text").Content(string(content)).Uuid(idempotencyKey).Build()
	request := larkim.NewCreateMessageReqBuilder().ReceiveIdType("chat_id").Body(body).Build()
	response, sendErr := messenger.client.Im.V1.Message.Create(ctx, request)
	if sendErr != nil {
		return &providerNetworkError{message: sendErr.Error()}
	}
	if response == nil || !response.Success() {
		if response == nil {
			return errors.New("empty Feishu response")
		}
		return &providerHTTPError{status: 502, message: fmt.Sprintf("Feishu API code %d: %s", response.Code, response.Msg)}
	}
	return nil
}

func larkResponseError(response *larkim.ReplyMessageResp) error {
	if response == nil {
		return errors.New("empty Feishu response")
	}
	return &providerHTTPError{status: 502, message: fmt.Sprintf("Feishu API code %d: %s", response.Code, response.Msg)}
}
