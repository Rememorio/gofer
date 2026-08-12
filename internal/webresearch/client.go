package webresearch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Rememorio/gofer/internal/netguard"
)

const (
	// BraveEndpoint is the default Brave Web Search API endpoint.
	BraveEndpoint              = "https://api.search.brave.com/res/v1/web/search"
	defaultSearchResponseBytes = int64(1 << 20)
)

var (
	// ErrInvalidConfig identifies incomplete or unsafe research configuration.
	ErrInvalidConfig = errors.New("invalid web research configuration")
	// ErrInvalidInput identifies an invalid search or fetch request.
	ErrInvalidInput = errors.New("invalid web research input")
	// ErrUpstream identifies an unsuccessful remote HTTP response.
	ErrUpstream = errors.New("web research upstream error")
	// ErrResponseTooLarge identifies a response exceeding its byte budget.
	ErrResponseTooLarge = errors.New("web response exceeds byte limit")
	// ErrUnsupportedContent identifies a response that is not text or HTML.
	ErrUnsupportedContent = errors.New("unsupported web content type")
)

// Config enables independently configured search and fetch capabilities.
type Config struct {
	Search *SearchConfig
	Fetch  *FetchConfig
}

// SearchConfig configures one supported web-search provider.
type SearchConfig struct {
	Provider              string
	APIKey                string
	Endpoint              string
	MaxResults            int
	SafeSearch            string
	Timeout               time.Duration
	AllowPrivateAddresses bool
}

// FetchConfig bounds direct web document retrieval.
type FetchConfig struct {
	MaxResponseBytes      int64
	MaxContentCharacters  int
	MaxRedirects          int
	Timeout               time.Duration
	UserAgent             string
	AllowPrivateAddresses bool
}

// Client owns optional search and fetch adapters and their guarded transports.
type Client struct {
	search  searchBackend
	fetch   *fetcher
	clients []*http.Client
}

// New constructs configured research capabilities without making a request.
func New(config Config) (*Client, error) {
	if config.Search == nil && config.Fetch == nil {
		return nil, fmt.Errorf("%w: search or fetch must be enabled", ErrInvalidConfig)
	}
	client := &Client{}
	if config.Search != nil {
		backend, httpClient, err := newSearchBackend(*config.Search)
		if err != nil {
			return nil, err
		}
		client.search = backend
		client.clients = append(client.clients, httpClient)
	}
	if config.Fetch != nil {
		fetcher, httpClient, err := newFetcher(*config.Fetch)
		if err != nil {
			client.Close()
			return nil, err
		}
		client.fetch = fetcher
		client.clients = append(client.clients, httpClient)
	}
	return client, nil
}

// HasSearch reports whether web search is configured.
func (client *Client) HasSearch() bool { return client != nil && client.search != nil }

// HasFetch reports whether direct document retrieval is configured.
func (client *Client) HasFetch() bool { return client != nil && client.fetch != nil }

// Search queries the configured provider and returns normalized results.
func (client *Client) Search(ctx context.Context, query string, limit int) (SearchResponse, error) {
	if !client.HasSearch() {
		return SearchResponse{}, fmt.Errorf("%w: search is disabled", ErrInvalidConfig)
	}
	return client.search.Search(ctx, query, limit)
}

// Fetch retrieves and extracts one text document.
func (client *Client) Fetch(ctx context.Context, rawURL string) (FetchResponse, error) {
	if !client.HasFetch() {
		return FetchResponse{}, fmt.Errorf("%w: fetch is disabled", ErrInvalidConfig)
	}
	return client.fetch.Fetch(ctx, rawURL)
}

// Close releases idle transport connections. It is safe to call repeatedly.
func (client *Client) Close() {
	if client == nil {
		return
	}
	for _, httpClient := range client.clients {
		if transport, ok := httpClient.Transport.(*http.Transport); ok {
			transport.CloseIdleConnections()
		}
	}
}

func guardedHTTPClient(allowPrivate bool, timeout time.Duration, maxRedirects int) (*http.Client, *netguard.URLGuard, error) {
	if timeout <= 0 || maxRedirects < 0 {
		return nil, nil, fmt.Errorf("%w: timeout must be positive and redirects cannot be negative", ErrInvalidConfig)
	}
	guard, err := netguard.NewURLGuard(netguard.URLGuardConfig{AllowPrivateAddresses: allowPrivate})
	if err != nil {
		return nil, nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = guard.DialContext
	client := &http.Client{Transport: transport, Timeout: timeout}
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) > maxRedirects {
			return fmt.Errorf("%w: more than %d redirects", ErrUpstream, maxRedirects)
		}
		_, validateErr := guard.ValidateNavigation(request.Context(), request.URL.String())
		return validateErr
	}
	return client, guard, nil
}

func validateEndpoint(rawURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("%w: endpoint must be an absolute HTTP(S) URL without credentials", ErrInvalidConfig)
	}
	parsed.Fragment = ""
	return parsed.String(), nil
}
