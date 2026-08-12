package webresearch

import (
	"bytes"
	"context"
	"fmt"
	"mime"
	"net/http"
	"strings"

	"golang.org/x/net/html/charset"
)

// FetchResponse is bounded text extracted from one resolved web document.
type FetchResponse struct {
	URL         string `json:"url"`
	FinalURL    string `json:"final_url"`
	Title       string `json:"title,omitempty"`
	Content     string `json:"content"`
	ContentType string `json:"content_type"`
	Truncated   bool   `json:"truncated"`
}

type fetcher struct {
	client *http.Client
	guard  interface {
		ValidateNavigation(context.Context, string) (string, error)
	}
	maxResponseBytes     int64
	maxContentCharacters int
	userAgent            string
}

func newFetcher(config FetchConfig) (*fetcher, *http.Client, error) {
	if config.MaxResponseBytes < 1024 || config.MaxResponseBytes > 100<<20 ||
		config.MaxContentCharacters < 128 || config.MaxContentCharacters > 2_000_000 ||
		config.MaxRedirects < 0 || config.MaxRedirects > 20 || config.Timeout <= 0 ||
		strings.TrimSpace(config.UserAgent) != config.UserAgent || config.UserAgent == "" || strings.ContainsAny(config.UserAgent, "\r\n") {
		return nil, nil, fmt.Errorf("%w: invalid fetch limits or user agent", ErrInvalidConfig)
	}
	client, guard, err := guardedHTTPClient(config.AllowPrivateAddresses, config.Timeout, config.MaxRedirects)
	if err != nil {
		return nil, nil, err
	}
	return &fetcher{
		client: client, guard: guard, maxResponseBytes: config.MaxResponseBytes,
		maxContentCharacters: config.MaxContentCharacters, userAgent: config.UserAgent,
	}, client, nil
}

func (fetcher *fetcher) Fetch(ctx context.Context, rawURL string) (FetchResponse, error) {
	if len(rawURL) > 4096 {
		return FetchResponse{}, fmt.Errorf("%w: URL exceeds 4096 bytes", ErrInvalidInput)
	}
	validated, err := fetcher.guard.ValidateNavigation(ctx, rawURL)
	if err != nil {
		return FetchResponse{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, validated, nil)
	if err != nil {
		return FetchResponse{}, fmt.Errorf("create fetch request: %w", err)
	}
	request.Header.Set("Accept", "text/html,application/xhtml+xml,application/json,text/plain;q=0.9")
	request.Header.Set("User-Agent", fetcher.userAgent)
	response, err := fetcher.client.Do(request)
	if err != nil {
		return FetchResponse{}, fmt.Errorf("fetch request: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return FetchResponse{}, fmt.Errorf("%w: fetch returned HTTP %d", ErrUpstream, response.StatusCode)
	}
	body, err := readBounded(response.Body, response.ContentLength, fetcher.maxResponseBytes)
	if err != nil {
		return FetchResponse{}, err
	}
	mediaType, err := responseMediaType(response.Header.Get("Content-Type"), body)
	if err != nil {
		return FetchResponse{}, err
	}
	decoded, err := decodeText(body, response.Header.Get("Content-Type"))
	if err != nil {
		return FetchResponse{}, fmt.Errorf("decode web response: %w", err)
	}
	title, content, err := extractContent(mediaType, decoded)
	if err != nil {
		return FetchResponse{}, err
	}
	content, truncated := truncateRunes(content, fetcher.maxContentCharacters)
	return FetchResponse{
		URL: validated, FinalURL: response.Request.URL.String(), Title: title,
		Content: content, ContentType: mediaType, Truncated: truncated,
	}, nil
}

func responseMediaType(header string, body []byte) (string, error) {
	mediaType, _, err := mime.ParseMediaType(header)
	if err != nil && strings.TrimSpace(header) != "" {
		return "", fmt.Errorf("%w: malformed Content-Type", ErrUnsupportedContent)
	}
	if mediaType == "" {
		mediaType, _, _ = mime.ParseMediaType(http.DetectContentType(body))
	}
	mediaType = strings.ToLower(mediaType)
	if !oneOf(mediaType, "text/html", "application/xhtml+xml", "text/plain", "application/json") {
		return "", fmt.Errorf("%w: %s", ErrUnsupportedContent, mediaType)
	}
	return mediaType, nil
}

func decodeText(body []byte, contentType string) ([]byte, error) {
	reader, err := charset.NewReader(bytes.NewReader(body), contentType)
	if err != nil {
		return nil, err
	}
	return readBounded(reader, -1, int64(len(body))*4+4)
}

func extractContent(mediaType string, body []byte) (string, string, error) {
	if mediaType == "text/html" || mediaType == "application/xhtml+xml" {
		return extractHTML(bytes.NewReader(body))
	}
	return "", strings.TrimSpace(string(body)), nil
}

func truncateRunes(value string, maximum int) (string, bool) {
	runes := []rune(value)
	if len(runes) <= maximum {
		return value, false
	}
	return strings.TrimSpace(string(runes[:maximum])), true
}
