package channel

import (
	"bytes"
	"context"
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
	"unicode/utf16"
)

const providerResponseLimit = 2 << 20

type providerHTTPError struct {
	status     int
	message    string
	retryAfter time.Duration
}

type providerNetworkError struct{ message string }

func (failure *providerNetworkError) Error() string {
	return "channel provider request failed: " + failure.message
}

func (failure *providerHTTPError) Error() string {
	if failure.message == "" {
		return fmt.Sprintf("channel provider returned HTTP %d", failure.status)
	}
	return fmt.Sprintf("channel provider returned HTTP %d: %s", failure.status, failure.message)
}

func providerRetryable(err error) (time.Duration, bool) {
	var failure *providerHTTPError
	if errors.As(err, &failure) {
		return failure.retryAfter, failure.status == http.StatusTooManyRequests || failure.status >= 500
	}
	var networkFailure *providerNetworkError
	if errors.As(err, &networkFailure) {
		return 0, true
	}
	return 0, false
}

func providerReconnectDelay(failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	if failures > 8 {
		failures = 8
	}
	return time.Duration(1<<(failures-1)) * time.Second
}

func submitProviderMessage(ctx context.Context, submit SubmitFunc, message Message) error {
	for {
		err := submit(ctx, message)
		if !errors.Is(err, ErrBusy) {
			return err
		}
		if err = sleepContext(ctx, 100*time.Millisecond); err != nil {
			return err
		}
	}
}

func retryProvider(ctx context.Context, attempts int, sleep func(context.Context, time.Duration) error, operation func() error) error {
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		lastErr = operation()
		retryAfter, retry := providerRetryable(lastErr)
		if lastErr == nil || !retry || attempt+1 == attempts {
			return lastErr
		}
		if retryAfter <= 0 {
			retryAfter = time.Duration(1<<attempt) * 200 * time.Millisecond
		}
		if err := sleep(ctx, retryAfter); err != nil {
			return err
		}
	}
	return lastErr
}

func requestJSON(ctx context.Context, client *http.Client, method, endpoint, authorization string, input, output any) error {
	headers := make(http.Header)
	if authorization != "" {
		headers.Set("Authorization", authorization)
	}
	return requestJSONHeaders(ctx, client, method, endpoint, headers, input, output)
}

func requestJSONHeaders(ctx context.Context, client *http.Client, method, endpoint string, headers http.Header, input, output any) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for name, values := range headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	response, err := client.Do(request)
	if err != nil {
		return sanitizeProviderNetworkError(err)
	}
	limited := io.LimitReader(response.Body, providerResponseLimit+1)
	data, err := io.ReadAll(limited)
	closeErr := response.Body.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if len(data) > providerResponseLimit {
		return &providerHTTPError{status: response.StatusCode, message: "response too large"}
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		message, bodyRetry := providerFailureDetails(data)
		headerRetry := retryAfter(response.Header)
		if bodyRetry > 0 {
			headerRetry = bodyRetry
		}
		return &providerHTTPError{
			status: response.StatusCode, message: message, retryAfter: headerRetry,
		}
	}
	if output == nil || len(data) == 0 {
		return nil
	}
	if err = json.Unmarshal(data, output); err != nil {
		return fmt.Errorf("decode channel provider response: %w", err)
	}
	return nil
}

func sanitizeProviderNetworkError(err error) error {
	var urlFailure *url.Error
	if errors.As(err, &urlFailure) && urlFailure.Err != nil {
		return &providerNetworkError{message: urlFailure.Err.Error()}
	}
	return &providerNetworkError{message: err.Error()}
}

func retryAfter(headers http.Header) time.Duration {
	value := strings.TrimSpace(headers.Get("Retry-After"))
	seconds, err := strconv.ParseFloat(value, 64)
	if err == nil && seconds > 0 && seconds <= 300 {
		return time.Duration(seconds * float64(time.Second))
	}
	return 0
}

func boundedProviderMessage(data []byte) string {
	message := strings.TrimSpace(string(data))
	if len(message) > 512 {
		message = message[:512]
	}
	return message
}

func providerFailureDetails(data []byte) (string, time.Duration) {
	var envelope struct {
		Message     string `json:"message"`
		Description string `json:"description"`
		Error       string `json:"error"`
		Parameters  struct {
			RetryAfter float64 `json:"retry_after"`
		} `json:"parameters"`
		RetryAfter float64 `json:"retry_after"`
	}
	if json.Unmarshal(data, &envelope) != nil {
		return boundedProviderMessage(data), 0
	}
	message := envelope.Description
	if message == "" {
		message = envelope.Message
	}
	if message == "" {
		message = envelope.Error
	}
	if message == "" {
		message = boundedProviderMessage(data)
	}
	seconds := envelope.Parameters.RetryAfter
	if seconds <= 0 {
		seconds = envelope.RetryAfter
	}
	if seconds > 0 && seconds <= 300 {
		return message, time.Duration(seconds * float64(time.Second))
	}
	return message, 0
}

func newProviderClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	return &http.Client{
		Transport: transport, Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func resolveProviderBaseURL(raw, fallback string, injectedClient bool) (string, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(raw), "/")
	if baseURL == "" {
		baseURL = fallback
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", ErrInvalid
	}
	if parsed.Scheme != "https" && !injectedClient {
		return "", ErrInvalid
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return "", ErrInvalid
	}
	return baseURL, nil
}

func validProviderAttempts(attempts int) bool { return attempts >= 1 && attempts <= 5 }

func trustedProviderHost(host, domain string) bool {
	host, domain = strings.ToLower(strings.TrimSpace(host)), strings.ToLower(strings.TrimSpace(domain))
	return host == domain || strings.HasSuffix(host, "."+domain)
}

type routeEntry[T any] struct {
	value     T
	createdAt time.Time
}

type routeStore[T any] struct {
	mu      sync.Mutex
	entries map[string]routeEntry[T]
	limit   int
	ttl     time.Duration
	now     func() time.Time
}

func newRouteStore[T any](limit int, ttl time.Duration, now func() time.Time) *routeStore[T] {
	if now == nil {
		now = time.Now
	}
	return &routeStore[T]{entries: make(map[string]routeEntry[T]), limit: limit, ttl: ttl, now: now}
}

func (store *routeStore[T]) Put(key string, value T) {
	if store == nil || key == "" {
		return
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	now := store.now()
	store.prune(now)
	if len(store.entries) >= store.limit {
		store.deleteOldest()
	}
	store.entries[key] = routeEntry[T]{value: value, createdAt: now}
}

func (store *routeStore[T]) Get(key string) (T, bool) {
	var zero T
	if store == nil || key == "" {
		return zero, false
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	now := store.now()
	store.prune(now)
	entry, exists := store.entries[key]
	return entry.value, exists
}

func (store *routeStore[T]) Delete(key string) {
	if store == nil {
		return
	}
	store.mu.Lock()
	delete(store.entries, key)
	store.mu.Unlock()
}

func (store *routeStore[T]) prune(now time.Time) {
	for key, entry := range store.entries {
		if now.Sub(entry.createdAt) >= store.ttl {
			delete(store.entries, key)
		}
	}
}

func (store *routeStore[T]) deleteOldest() {
	oldestKey := ""
	var oldest time.Time
	for key, entry := range store.entries {
		if oldestKey == "" || entry.createdAt.Before(oldest) {
			oldestKey, oldest = key, entry.createdAt
		}
	}
	delete(store.entries, oldestKey)
}

func normalizedSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			set[value] = struct{}{}
		}
	}
	return set
}

func allowed(set map[string]struct{}, value string) bool {
	_, exists := set[value]
	return len(set) == 0 || exists
}

func listed(set map[string]struct{}, value string) bool {
	_, exists := set[value]
	return exists
}

func splitUTF16(text string, limit int) []string {
	if strings.TrimSpace(text) == "" || limit < 1 {
		return nil
	}
	runes := []rune(text)
	chunks := make([]string, 0, len(runes)/limit+1)
	for len(runes) > 0 {
		end, units, lastBreak := 0, 0, -1
		for end < len(runes) {
			width := len(utf16.Encode([]rune{runes[end]}))
			if units+width > limit {
				break
			}
			units += width
			if runes[end] == '\n' || runes[end] == ' ' || runes[end] == '\t' {
				lastBreak = end + 1
			}
			end++
		}
		if end < len(runes) && lastBreak > 0 && lastBreak >= end/2 {
			end = lastBreak
		}
		if end == 0 {
			end = 1
		}
		chunk := string(runes[:end])
		if strings.TrimSpace(chunk) != "" {
			chunks = append(chunks, chunk)
		}
		runes = runes[end:]
	}
	return chunks
}
