package channel

import (
	"context"
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
	// TelegramProvider is the normalized Telegram channel name.
	TelegramProvider = "telegram"
	telegramBaseURL  = "https://api.telegram.org"
	telegramTextMax  = 4096
)

// TelegramConfig controls Bot API long polling and outbound delivery.
type TelegramConfig struct {
	BotToken       string
	AllowedUsers   []string
	PollTimeout    time.Duration
	RequestTimeout time.Duration
	MaxAttempts    int
	Client         *http.Client
	BaseURL        string
	Sleep          func(context.Context, time.Duration) error
	Now            func() time.Time
}

// Telegram is a long-polling Telegram Bot API provider.
type Telegram struct {
	token        string
	allowedUsers map[string]struct{}
	pollTimeout  time.Duration
	attempts     int
	client       *http.Client
	baseURL      string
	sleep        func(context.Context, time.Duration) error
	now          func() time.Time
	ownedClient  bool

	mu      sync.Mutex
	startMu sync.Mutex
	started bool
	closed  bool
	cancel  context.CancelFunc
	done    chan struct{}
	once    sync.Once
}

// NewTelegram validates configuration and creates a Telegram provider.
func NewTelegram(config TelegramConfig) (*Telegram, error) {
	token := strings.TrimSpace(config.BotToken)
	if token == "" || len(token) > 512 {
		return nil, ErrInvalid
	}
	if config.PollTimeout == 0 {
		config.PollTimeout = 30 * time.Second
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = config.PollTimeout + 15*time.Second
	}
	if config.MaxAttempts == 0 {
		config.MaxAttempts = 3
	}
	if !validTelegramTiming(config.PollTimeout, config.RequestTimeout, config.MaxAttempts) {
		return nil, ErrInvalid
	}
	baseURL, err := resolveProviderBaseURL(config.BaseURL, telegramBaseURL, config.Client != nil)
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
	return &Telegram{
		token: token, allowedUsers: normalizedSet(config.AllowedUsers), pollTimeout: config.PollTimeout,
		attempts: config.MaxAttempts, client: config.Client, baseURL: baseURL, sleep: config.Sleep,
		now: config.Now, ownedClient: owned,
	}, nil
}

func validTelegramTiming(pollTimeout, requestTimeout time.Duration, attempts int) bool {
	return pollTimeout >= time.Second && pollTimeout <= 50*time.Second &&
		requestTimeout > pollTimeout && requestTimeout <= 2*time.Minute && validProviderAttempts(attempts)
}

// Name returns the normalized provider name.
func (*Telegram) Name() string { return TelegramProvider }

// Start verifies the bot token and launches bounded long polling.
func (provider *Telegram) Start(parent context.Context, submit SubmitFunc) error {
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
	if err := provider.verify(parent); err != nil {
		return fmt.Errorf("verify Telegram bot: %w", err)
	}
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
	go func() {
		defer close(done)
		provider.poll(ctx, submit)
	}()
	return nil
}

// Send delivers one final response, splitting it at Telegram's UTF-16 limit.
func (provider *Telegram) Send(ctx context.Context, reply Reply) error {
	if provider == nil || ctx == nil || reply.Provider != TelegramProvider || strings.TrimSpace(reply.ChatID) == "" || strings.TrimSpace(reply.Text) == "" {
		return ErrInvalid
	}
	chunks := splitUTF16(reply.Text, telegramTextMax)
	for index, chunk := range chunks {
		input := map[string]any{"chat_id": reply.ChatID, "text": chunk}
		if index == 0 {
			if messageID, err := strconv.ParseInt(reply.InReplyTo, 10, 64); err == nil && messageID > 0 {
				input["reply_parameters"] = map[string]any{"message_id": messageID, "allow_sending_without_reply": true}
			}
		}
		if err := retryProvider(ctx, provider.attempts, provider.sleep, func() error {
			var response telegramResponse[telegramSentMessage]
			err := requestJSON(ctx, provider.client, http.MethodPost, provider.endpoint("sendMessage"), "", input, &response)
			if err == nil {
				err = response.err()
			}
			return err
		}); err != nil {
			return fmt.Errorf("send Telegram message: %w", err)
		}
	}
	return nil
}

// Close cancels polling and releases owned HTTP resources once.
func (provider *Telegram) Close() error {
	if provider == nil {
		return nil
	}
	provider.once.Do(func() {
		provider.mu.Lock()
		provider.closed = true
		cancel, done := provider.cancel, provider.done
		provider.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if done != nil {
			<-done
		}
		if provider.ownedClient {
			provider.client.CloseIdleConnections()
		}
	})
	return nil
}

type telegramResponse[T any] struct {
	OK          bool   `json:"ok"`
	Result      T      `json:"result"`
	Description string `json:"description"`
}

func (response telegramResponse[T]) err() error {
	if response.OK {
		return nil
	}
	return &providerHTTPError{status: http.StatusBadGateway, message: response.Description}
}

type telegramUser struct {
	ID       int64  `json:"id"`
	IsBot    bool   `json:"is_bot"`
	Username string `json:"username"`
}

type telegramChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type telegramFile struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileName     string `json:"file_name"`
	MimeType     string `json:"mime_type"`
	FileSize     int64  `json:"file_size"`
}

type telegramMessage struct {
	MessageID       int64            `json:"message_id"`
	MessageThreadID int64            `json:"message_thread_id"`
	Date            int64            `json:"date"`
	From            *telegramUser    `json:"from"`
	Chat            telegramChat     `json:"chat"`
	Text            string           `json:"text"`
	Caption         string           `json:"caption"`
	ReplyTo         *telegramMessage `json:"reply_to_message"`
	Photo           []telegramFile   `json:"photo"`
	Document        *telegramFile    `json:"document"`
}

type telegramUpdate struct {
	UpdateID int64            `json:"update_id"`
	Message  *telegramMessage `json:"message"`
}

type telegramSentMessage struct {
	MessageID int64 `json:"message_id"`
}

func (provider *Telegram) verify(ctx context.Context) error {
	var response telegramResponse[telegramUser]
	err := requestJSON(ctx, provider.client, http.MethodPost, provider.endpoint("getMe"), "", nil, &response)
	if err != nil {
		return err
	}
	return response.err()
}

func (provider *Telegram) poll(ctx context.Context, submit SubmitFunc) {
	var offset int64
	for ctx.Err() == nil {
		next, err := provider.pollOnce(ctx, submit, offset)
		if err == nil {
			offset = next
			continue
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, ErrClosed) {
			return
		}
		if sleepErr := provider.sleep(ctx, time.Second); sleepErr != nil {
			return
		}
	}
}

func (provider *Telegram) pollOnce(ctx context.Context, submit SubmitFunc, offset int64) (int64, error) {
	input := map[string]any{
		"offset": offset, "limit": 100, "timeout": int(provider.pollTimeout.Seconds()),
		"allowed_updates": []string{"message"},
	}
	var response telegramResponse[[]telegramUpdate]
	err := requestJSON(ctx, provider.client, http.MethodPost, provider.endpoint("getUpdates"), "", input, &response)
	if err != nil {
		return offset, err
	}
	if err = response.err(); err != nil {
		return offset, err
	}
	for _, update := range response.Result {
		next := update.UpdateID + 1
		message, keep := provider.normalize(update)
		if keep {
			if err = submit(ctx, message); err != nil {
				return offset, err
			}
		}
		if next > offset {
			offset = next
		}
	}
	return offset, nil
}

func (provider *Telegram) normalize(update telegramUpdate) (Message, bool) {
	inbound := update.Message
	if inbound == nil || inbound.From == nil || inbound.From.IsBot {
		return Message{}, false
	}
	userID := strconv.FormatInt(inbound.From.ID, 10)
	if !allowed(provider.allowedUsers, userID) {
		return Message{}, false
	}
	text := strings.TrimSpace(inbound.Text)
	if text == "" {
		text = strings.TrimSpace(inbound.Caption)
	}
	attachments := telegramAttachments(inbound)
	if text == "" && len(attachments) == 0 {
		return Message{}, false
	}
	chatID := strconv.FormatInt(inbound.Chat.ID, 10)
	topicID := ""
	if inbound.Chat.Type != "private" {
		switch {
		case inbound.MessageThreadID > 0:
			topicID = strconv.FormatInt(inbound.MessageThreadID, 10)
		case inbound.ReplyTo != nil:
			topicID = strconv.FormatInt(inbound.ReplyTo.MessageID, 10)
		default:
			topicID = strconv.FormatInt(inbound.MessageID, 10)
		}
	}
	received := time.Unix(inbound.Date, 0).UTC()
	if inbound.Date <= 0 {
		received = provider.now().UTC()
	}
	metadata := map[string]string{"update_id": strconv.FormatInt(update.UpdateID, 10), "chat_type": inbound.Chat.Type}
	if inbound.From.Username != "" {
		metadata["username"] = inbound.From.Username
	}
	return Message{
		ID: strconv.FormatInt(inbound.MessageID, 10), Provider: TelegramProvider,
		WorkspaceID: chatID, ExternalUserID: userID, ChatID: chatID, TopicID: topicID,
		Text: text, Attachments: attachments, Metadata: metadata, ReceivedAt: received,
	}, true
}

func telegramAttachments(message *telegramMessage) []Attachment {
	attachments := make([]Attachment, 0, 2)
	if len(message.Photo) > 0 {
		photo := message.Photo[len(message.Photo)-1]
		if photo.FileID != "" {
			name := "photo-" + photo.FileUniqueID + ".jpg"
			attachments = append(attachments, telegramAttachment(photo, name, "image/jpeg"))
		}
	}
	if message.Document != nil && message.Document.FileID != "" {
		attachments = append(attachments, telegramAttachment(*message.Document, message.Document.FileName, message.Document.MimeType))
	}
	return attachments
}

func telegramAttachment(file telegramFile, name, mediaType string) Attachment {
	if strings.TrimSpace(name) == "" {
		name = "telegram-file"
	}
	if strings.TrimSpace(mediaType) == "" {
		mediaType = "application/octet-stream"
	}
	if file.FileSize < 0 {
		file.FileSize = 0
	}
	return Attachment{Name: name, MediaType: mediaType, URL: "telegram://file/" + url.PathEscape(file.FileID), Size: file.FileSize}
}

func (provider *Telegram) endpoint(method string) string {
	return provider.baseURL + "/bot" + url.PathEscape(provider.token) + "/" + method
}
