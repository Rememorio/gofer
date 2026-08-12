package webresearch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// SearchResult is one citation-ready normalized search result.
type SearchResult struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Snippet     string `json:"snippet,omitempty"`
	PublishedAt string `json:"published_at,omitempty"`
	Source      string `json:"source,omitempty"`
}

// SearchResponse is the provider-independent search tool result.
type SearchResponse struct {
	Query   string         `json:"query"`
	Results []SearchResult `json:"results"`
}

type searchBackend interface {
	Search(context.Context, string, int) (SearchResponse, error)
}

type httpSearchBackend struct {
	provider   string
	apiKey     string
	endpoint   string
	maxResults int
	safeSearch string
	client     *http.Client
	guard      interface {
		ValidateNavigation(context.Context, string) (string, error)
	}
}

func newSearchBackend(config SearchConfig) (searchBackend, *http.Client, error) {
	if config.MaxResults < 1 || config.MaxResults > 20 || config.Timeout <= 0 ||
		!oneOf(config.SafeSearch, "off", "moderate", "strict") || config.APIKey != strings.TrimSpace(config.APIKey) {
		return nil, nil, fmt.Errorf("%w: invalid search limits or safe-search mode", ErrInvalidConfig)
	}
	endpoint := config.Endpoint
	if config.Provider == "brave" && strings.TrimSpace(endpoint) == "" {
		endpoint = BraveEndpoint
	}
	if config.Provider == "brave" && strings.TrimSpace(config.APIKey) == "" {
		return nil, nil, fmt.Errorf("%w: Brave API key is required", ErrInvalidConfig)
	}
	if config.Provider == "searxng" && strings.TrimSpace(endpoint) == "" {
		return nil, nil, fmt.Errorf("%w: SearXNG endpoint is required", ErrInvalidConfig)
	}
	if !oneOf(config.Provider, "brave", "searxng") {
		return nil, nil, fmt.Errorf("%w: unsupported search provider %q", ErrInvalidConfig, config.Provider)
	}
	endpoint, err := validateEndpoint(endpoint)
	if err != nil {
		return nil, nil, err
	}
	if config.Provider == "searxng" {
		endpoint, err = searxNGEndpoint(endpoint)
		if err != nil {
			return nil, nil, err
		}
	}
	client, guard, err := guardedHTTPClient(config.AllowPrivateAddresses, config.Timeout, 0)
	if err != nil {
		return nil, nil, err
	}
	return &httpSearchBackend{
		provider: config.Provider, apiKey: config.APIKey, endpoint: endpoint,
		maxResults: config.MaxResults, safeSearch: config.SafeSearch, client: client, guard: guard,
	}, client, nil
}

func (backend *httpSearchBackend) Search(ctx context.Context, query string, limit int) (SearchResponse, error) {
	query = strings.TrimSpace(query)
	if query == "" || len([]rune(query)) > 400 || len(strings.Fields(query)) > 50 {
		return SearchResponse{}, fmt.Errorf("%w: query must contain 1 to 400 characters and at most 50 words", ErrInvalidInput)
	}
	if limit == 0 {
		limit = backend.maxResults
	}
	if limit < 1 || limit > backend.maxResults {
		return SearchResponse{}, fmt.Errorf("%w: max_results must be between 1 and %d", ErrInvalidInput, backend.maxResults)
	}
	requestURL, err := backend.requestURL(query, limit)
	if err != nil {
		return SearchResponse{}, err
	}
	if _, err = backend.guard.ValidateNavigation(ctx, requestURL); err != nil {
		return SearchResponse{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return SearchResponse{}, fmt.Errorf("create search request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	if backend.provider == "brave" {
		request.Header.Set("X-Subscription-Token", backend.apiKey)
	}
	response, err := backend.client.Do(request)
	if err != nil {
		return SearchResponse{}, fmt.Errorf("search request: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return SearchResponse{}, fmt.Errorf("%w: search returned HTTP %d", ErrUpstream, response.StatusCode)
	}
	body, err := readBounded(response.Body, response.ContentLength, defaultSearchResponseBytes)
	if err != nil {
		return SearchResponse{}, err
	}
	results, err := backend.decode(body)
	if err != nil {
		return SearchResponse{}, err
	}
	results = normalizeResults(results)
	if len(results) > limit {
		results = results[:limit]
	}
	return SearchResponse{Query: query, Results: results}, nil
}

func (backend *httpSearchBackend) requestURL(query string, limit int) (string, error) {
	parsed, err := url.Parse(backend.endpoint)
	if err != nil {
		return "", fmt.Errorf("%w: parse search endpoint: %w", ErrInvalidConfig, err)
	}
	values := parsed.Query()
	values.Set("q", query)
	if backend.provider == "brave" {
		values.Set("count", strconv.Itoa(limit))
		values.Set("safesearch", backend.safeSearch)
		values.Set("result_filter", "web")
		values.Set("text_decorations", "false")
	} else {
		values.Set("format", "json")
		values.Set("safesearch", strconv.Itoa(searxSafeSearch(backend.safeSearch)))
	}
	parsed.RawQuery = values.Encode()
	return parsed.String(), nil
}

func (backend *httpSearchBackend) decode(body []byte) ([]SearchResult, error) {
	if backend.provider == "brave" {
		var payload struct {
			Web struct {
				Results []struct {
					Title       string `json:"title"`
					URL         string `json:"url"`
					Description string `json:"description"`
					Age         string `json:"age"`
				} `json:"results"`
			} `json:"web"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("%w: decode Brave response: %w", ErrUpstream, err)
		}
		results := make([]SearchResult, 0, len(payload.Web.Results))
		for _, result := range payload.Web.Results {
			results = append(results, SearchResult{Title: result.Title, URL: result.URL, Snippet: result.Description, PublishedAt: result.Age, Source: "brave"})
		}
		return results, nil
	}
	var payload struct {
		Results []struct {
			Title         string   `json:"title"`
			URL           string   `json:"url"`
			Content       string   `json:"content"`
			PublishedDate string   `json:"publishedDate"`
			Engine        string   `json:"engine"`
			Engines       []string `json:"engines"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("%w: decode SearXNG response: %w", ErrUpstream, err)
	}
	results := make([]SearchResult, 0, len(payload.Results))
	for _, result := range payload.Results {
		source := result.Engine
		if source == "" {
			source = strings.Join(result.Engines, ",")
		}
		results = append(results, SearchResult{Title: result.Title, URL: result.URL, Snippet: result.Content, PublishedAt: result.PublishedDate, Source: source})
	}
	return results, nil
}

func searxNGEndpoint(rawURL string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("%w: parse SearXNG endpoint: %w", ErrInvalidConfig, err)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	if !strings.HasSuffix(parsed.Path, "/search") {
		parsed.Path += "/search"
	}
	return parsed.String(), nil
}

func searxSafeSearch(value string) int {
	switch value {
	case "off":
		return 0
	case "strict":
		return 2
	default:
		return 1
	}
}

func normalizeResults(results []SearchResult) []SearchResult {
	normalized := make([]SearchResult, 0, len(results))
	seen := make(map[string]struct{}, len(results))
	for _, result := range results {
		parsed, err := url.Parse(strings.TrimSpace(result.URL))
		if err != nil || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			continue
		}
		parsed.Fragment = ""
		result.URL = parsed.String()
		if _, duplicate := seen[result.URL]; duplicate {
			continue
		}
		seen[result.URL] = struct{}{}
		result.Title = cleanInline(result.Title)
		result.Snippet = cleanInline(result.Snippet)
		result.PublishedAt = cleanInline(result.PublishedAt)
		result.Source = cleanInline(result.Source)
		normalized = append(normalized, result)
	}
	return normalized
}

func readBounded(reader io.Reader, contentLength, maximum int64) ([]byte, error) {
	if contentLength > maximum {
		return nil, ErrResponseTooLarge
	}
	limited := io.LimitReader(reader, maximum+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read web response: %w", err)
	}
	if int64(len(data)) > maximum {
		return nil, ErrResponseTooLarge
	}
	return data, nil
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}
