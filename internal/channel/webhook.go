package channel

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Rememorio/gofer/internal/netguard"
)

const (
	// WebhookProvider is the provider name used by the generic signed adapter.
	WebhookProvider = "webhook"
	// WebhookTimestampHeader carries Unix seconds covered by the signature.
	WebhookTimestampHeader = "X-Gofer-Timestamp"
	// WebhookSignatureHeader carries the sha256= prefixed HMAC.
	WebhookSignatureHeader  = "X-Gofer-Signature"
	defaultWebhookBodyLimit = 1 << 20
)

// WebhookHandlerConfig controls the authenticated generic ingress endpoint.
type WebhookHandlerConfig struct {
	Manager      *Manager
	Secret       string
	MaxBodyBytes int64
	ClockSkew    time.Duration
	Now          func() time.Time
}

// WebhookHandler verifies and queues generic provider events.
type WebhookHandler struct {
	manager   *Manager
	secret    []byte
	maxBody   int64
	clockSkew time.Duration
	now       func() time.Time
}

// NewWebhookHandler validates config and constructs a signed ingress handler.
func NewWebhookHandler(config WebhookHandlerConfig) (*WebhookHandler, error) {
	if config.Manager == nil || len(strings.TrimSpace(config.Secret)) < 24 {
		return nil, ErrInvalid
	}
	if config.MaxBodyBytes == 0 {
		config.MaxBodyBytes = defaultWebhookBodyLimit
	}
	if config.ClockSkew == 0 {
		config.ClockSkew = 5 * time.Minute
	}
	if config.MaxBodyBytes < 1024 || config.MaxBodyBytes > 16<<20 || config.ClockSkew < time.Minute || config.ClockSkew > time.Hour {
		return nil, ErrInvalid
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &WebhookHandler{manager: config.Manager, secret: []byte(strings.TrimSpace(config.Secret)), maxBody: config.MaxBodyBytes, clockSkew: config.ClockSkew, now: config.Now}, nil
}

// ServeHTTP accepts one signed event and returns after bounded enqueueing.
func (handler *WebhookHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		writeWebhookError(writer, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if mediaType := strings.ToLower(strings.TrimSpace(strings.Split(request.Header.Get("Content-Type"), ";")[0])); mediaType != "application/json" {
		writeWebhookError(writer, http.StatusUnsupportedMediaType, "application/json is required")
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, handler.maxBody)
	body, err := io.ReadAll(request.Body)
	if err != nil {
		writeWebhookError(writer, http.StatusRequestEntityTooLarge, "request body is too large")
		return
	}
	if err = verifyWebhook(handler.secret, request.Header, body, handler.now(), handler.clockSkew); err != nil {
		writeWebhookError(writer, http.StatusUnauthorized, "invalid webhook signature")
		return
	}
	workspaceID := strings.TrimSpace(request.PathValue("workspace_id"))
	var input struct {
		ID             string            `json:"id"`
		ExternalUserID string            `json:"external_user_id"`
		ChatID         string            `json:"chat_id"`
		TopicID        string            `json:"topic_id"`
		Text           string            `json:"text"`
		Attachments    []Attachment      `json:"attachments"`
		Metadata       map[string]string `json:"metadata"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&input); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		writeWebhookError(writer, http.StatusBadRequest, "invalid webhook event")
		return
	}
	message := Message{
		ID: strings.TrimSpace(input.ID), Provider: WebhookProvider, WorkspaceID: workspaceID,
		ExternalUserID: strings.TrimSpace(input.ExternalUserID), ChatID: strings.TrimSpace(input.ChatID),
		TopicID: strings.TrimSpace(input.TopicID), Text: input.Text, Attachments: input.Attachments,
		Metadata: input.Metadata, ReceivedAt: handler.now().UTC(),
	}
	if err = handler.manager.Submit(request.Context(), message); err != nil {
		if errors.Is(err, ErrBusy) {
			writer.Header().Set("Retry-After", "1")
			writeWebhookError(writer, http.StatusServiceUnavailable, "channel queue is full")
			return
		}
		writeWebhookError(writer, http.StatusBadRequest, "invalid webhook event")
		return
	}
	writeWebhookJSON(writer, http.StatusAccepted, map[string]any{"accepted": true, "message_id": message.ID})
}

// WebhookSenderConfig controls signed generic outbound callbacks.
type WebhookSenderConfig struct {
	Endpoint              string
	Secret                string
	Timeout               time.Duration
	MaxAttempts           int
	AllowPrivateAddresses bool
	Client                *http.Client
	Now                   func() time.Time
	Sleep                 func(context.Context, time.Duration) error
}

// WebhookSender posts normalized replies to one guarded callback endpoint.
type WebhookSender struct {
	endpoint string
	secret   []byte
	client   *http.Client
	guard    *netguard.URLGuard
	attempts int
	now      func() time.Time
	sleep    func(context.Context, time.Duration) error
	owned    bool
	once     sync.Once
}

// NewWebhookSender validates and constructs a generic callback sender.
func NewWebhookSender(config WebhookSenderConfig) (*WebhookSender, error) {
	endpoint, err := validateWebhookEndpoint(config.Endpoint)
	if err != nil || len(strings.TrimSpace(config.Secret)) < 24 {
		return nil, ErrInvalid
	}
	if config.Timeout == 0 {
		config.Timeout = 10 * time.Second
	}
	if config.MaxAttempts == 0 {
		config.MaxAttempts = 3
	}
	if config.Timeout < time.Second || config.Timeout > 2*time.Minute || config.MaxAttempts < 1 || config.MaxAttempts > 5 {
		return nil, ErrInvalid
	}
	guard, err := netguard.NewURLGuard(netguard.URLGuardConfig{AllowPrivateAddresses: config.AllowPrivateAddresses})
	if err != nil {
		return nil, err
	}
	owned := config.Client == nil
	if config.Client == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		transport.DialContext = guard.DialContext
		config.Client = &http.Client{Transport: transport, Timeout: config.Timeout}
		config.Client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
			if len(via) > 3 {
				return errors.New("too many webhook redirects")
			}
			_, redirectErr := guard.ValidateNavigation(request.Context(), request.URL.String())
			return redirectErr
		}
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Sleep == nil {
		config.Sleep = sleepContext
	}
	return &WebhookSender{
		endpoint: endpoint, secret: []byte(strings.TrimSpace(config.Secret)), client: config.Client,
		guard: guard, attempts: config.MaxAttempts, now: config.Now, sleep: config.Sleep, owned: owned,
	}, nil
}

// Name returns the generic provider name.
func (*WebhookSender) Name() string { return WebhookProvider }

// Send signs and posts one isolated reply with bounded retries.
func (sender *WebhookSender) Send(ctx context.Context, reply Reply) error {
	if sender == nil || ctx == nil || reply.Provider != WebhookProvider || strings.TrimSpace(reply.ChatID) == "" || strings.TrimSpace(reply.Text) == "" {
		return ErrInvalid
	}
	body, err := json.Marshal(reply)
	if err != nil {
		return err
	}
	var lastErr error
	for attempt := 0; attempt < sender.attempts; attempt++ {
		lastErr = sender.sendOnce(ctx, body, reply.InReplyTo)
		if lastErr == nil || !retryableWebhookError(lastErr) {
			return lastErr
		}
		if attempt+1 < sender.attempts {
			if err = sender.sleep(ctx, time.Duration(1<<attempt)*100*time.Millisecond); err != nil {
				return err
			}
		}
	}
	return lastErr
}

func (sender *WebhookSender) sendOnce(ctx context.Context, body []byte, idempotencyKey string) error {
	if _, err := sender.guard.ValidateNavigation(ctx, sender.endpoint); err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, sender.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	timestamp := strconv.FormatInt(sender.now().Unix(), 10)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(WebhookTimestampHeader, timestamp)
	request.Header.Set(WebhookSignatureHeader, signWebhook(sender.secret, timestamp, body))
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response, err := sender.client.Do(request)
	if err != nil {
		return &webhookSendError{cause: err, retry: true}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	closeErr := response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return &webhookSendError{status: response.StatusCode, retry: response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500}
	}
	return closeErr
}

// Close releases idle owned HTTP connections once.
func (sender *WebhookSender) Close() error {
	if sender == nil {
		return nil
	}
	sender.once.Do(func() {
		if sender.owned {
			if transport, ok := sender.client.Transport.(*http.Transport); ok {
				transport.CloseIdleConnections()
			}
		}
	})
	return nil
}

type webhookSendError struct {
	status int
	retry  bool
	cause  error
}

func (failure *webhookSendError) Error() string {
	if failure.status != 0 {
		return fmt.Sprintf("webhook callback returned HTTP %d", failure.status)
	}
	return fmt.Sprintf("webhook callback failed: %v", failure.cause)
}
func (failure *webhookSendError) Unwrap() error { return failure.cause }

func retryableWebhookError(err error) bool {
	var failure *webhookSendError
	return errors.As(err, &failure) && failure.retry
}

func verifyWebhook(secret []byte, headers http.Header, body []byte, now time.Time, skew time.Duration) error {
	rawTimestamp := strings.TrimSpace(headers.Get(WebhookTimestampHeader))
	seconds, err := strconv.ParseInt(rawTimestamp, 10, 64)
	if err != nil {
		return ErrUnauthorized
	}
	timestamp := time.Unix(seconds, 0)
	if delta := now.Sub(timestamp); delta < -skew || delta > skew {
		return ErrUnauthorized
	}
	want := signWebhook(secret, rawTimestamp, body)
	got := strings.TrimSpace(headers.Get(WebhookSignatureHeader))
	if !hmac.Equal([]byte(got), []byte(want)) {
		return ErrUnauthorized
	}
	return nil
}

func signWebhook(secret []byte, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = io.WriteString(mac, timestamp)
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func validateWebhookEndpoint(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", ErrInvalid
	}
	return parsed.String(), nil
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func writeWebhookError(writer http.ResponseWriter, status int, message string) {
	writeWebhookJSON(writer, status, map[string]string{"error": message})
}

func writeWebhookJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
