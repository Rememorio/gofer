package webresearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/policy"
	"github.com/Rememorio/gofer/internal/tool"
)

func TestBraveSearchNormalizesResults(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-Subscription-Token") != "secret" || request.URL.Query().Get("q") != "go agents" ||
			request.URL.Query().Get("count") != "2" || request.URL.Query().Get("safesearch") != "moderate" ||
			request.URL.Query().Get("result_filter") != "web" || request.URL.Query().Get("text_decorations") != "false" {
			t.Errorf("request = %#v, headers = %#v", request.URL.Query(), request.Header)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"web":{"results":[`+
			`{"title":" Go\nAgents ","url":"https://example.com/a#section","description":" useful\tresult ","age":"today"},`+
			`{"title":"duplicate","url":"https://example.com/a#other","description":"ignored"},`+
			`{"title":"bad","url":"file:///tmp/a","description":"ignored"},`+
			`{"title":"Second","url":"https://example.org/b","description":"other"}`+
			`]}}`)
	}))
	defer server.Close()
	client := newTestClient(t, Config{Search: &SearchConfig{
		Provider: "brave", APIKey: "secret", Endpoint: server.URL,
		MaxResults: 2, SafeSearch: "moderate", Timeout: time.Second, AllowPrivateAddresses: true,
	}})
	defer client.Close()

	response, err := client.Search(context.Background(), " go agents ", 0)
	if err != nil {
		t.Fatalf("Search(): %v", err)
	}
	if response.Query != "go agents" || len(response.Results) != 2 {
		t.Fatalf("Search() = %#v", response)
	}
	result := response.Results[0]
	if result.Title != "Go Agents" || result.URL != "https://example.com/a" ||
		result.Snippet != "useful result" || result.Source != "brave" {
		t.Fatalf("result = %#v", result)
	}
	if response.Results[1].URL != "https://example.org/b" {
		t.Fatalf("second result = %#v", response.Results[1])
	}
}

func TestSearXNGSearchMapsProviderResponse(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/instance/search" || request.URL.Query().Get("format") != "json" ||
			request.URL.Query().Get("safesearch") != "2" {
			t.Errorf("request URL = %s", request.URL.String())
		}
		_, _ = io.WriteString(writer, `{"results":[`+
			`{"title":"One","url":"https://one.example","content":"first","publishedDate":"2026-01-02","engines":["duckduckgo","brave"]},`+
			`{"title":"Two","url":"https://two.example","content":"second","engine":"google"},`+
			`{"title":"Three","url":"https://three.example","content":"third"}`+
			`]}`)
	}))
	defer server.Close()
	client := newTestClient(t, Config{Search: &SearchConfig{
		Provider: "searxng", Endpoint: server.URL + "/instance/", MaxResults: 3,
		SafeSearch: "strict", Timeout: time.Second, AllowPrivateAddresses: true,
	}})
	defer client.Close()
	response, err := client.Search(context.Background(), "research", 2)
	if err != nil || len(response.Results) != 2 {
		t.Fatalf("Search() = %#v, %v", response, err)
	}
	if response.Results[0].Source != "duckduckgo,brave" || response.Results[1].Source != "google" {
		t.Fatalf("sources = %#v", response.Results)
	}
}

func TestSearchRejectsInputsAndUpstreamFailures(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Query().Get("q") {
		case "status":
			writer.WriteHeader(http.StatusTooManyRequests)
		case "broken":
			_, _ = io.WriteString(writer, `{`)
		case "large":
			writer.Header().Set("Content-Length", fmt.Sprint(defaultSearchResponseBytes+1))
			_, _ = io.WriteString(writer, `{}`)
		default:
			_, _ = io.WriteString(writer, `{"web":{"results":[]}}`)
		}
	}))
	defer server.Close()
	client := newTestClient(t, Config{Search: &SearchConfig{
		Provider: "brave", APIKey: "secret", Endpoint: server.URL,
		MaxResults: 2, SafeSearch: "off", Timeout: time.Second, AllowPrivateAddresses: true,
	}})
	defer client.Close()
	for _, test := range []struct {
		query string
		limit int
		want  error
	}{
		{query: " ", want: ErrInvalidInput},
		{query: strings.Repeat("界", 401), want: ErrInvalidInput},
		{query: strings.Repeat("word ", 51), want: ErrInvalidInput},
		{query: "ok", limit: 3, want: ErrInvalidInput},
		{query: "status", want: ErrUpstream},
		{query: "broken", want: ErrUpstream},
		{query: "large", want: ErrResponseTooLarge},
	} {
		if _, err := client.Search(context.Background(), test.query, test.limit); !errors.Is(err, test.want) {
			t.Errorf("Search(%q, %d) error = %v, want %v", test.query, test.limit, err, test.want)
		}
	}
}

func TestSearchNeverForwardsCredentialsAcrossRedirects(t *testing.T) {
	t.Parallel()

	contacted := make(chan string, 1)
	destination := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		contacted <- request.Header.Get("X-Subscription-Token")
	}))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, destination.URL, http.StatusFound)
	}))
	defer origin.Close()
	client := newTestClient(t, Config{Search: &SearchConfig{
		Provider: "brave", APIKey: "secret", Endpoint: origin.URL,
		MaxResults: 1, SafeSearch: "off", Timeout: time.Second, AllowPrivateAddresses: true,
	}})
	defer client.Close()
	if _, err := client.Search(context.Background(), "redirect", 1); !errors.Is(err, ErrUpstream) {
		t.Fatalf("Search(redirect) error = %v", err)
	}
	select {
	case header := <-contacted:
		t.Fatalf("redirect destination received credential %q", header)
	default:
	}
}

func TestFetchExtractsHTMLAndFollowsGuardedRedirect(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/start" {
			http.Redirect(writer, request, "/article", http.StatusFound)
			return
		}
		if request.Header.Get("User-Agent") != "Gofer Test" {
			t.Errorf("User-Agent = %q", request.Header.Get("User-Agent"))
		}
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(writer, `<!doctype html><html><head><title> Example  Page </title><script>secret()</script></head>`+
			`<body><nav>menu</nav><main><article><h1>Heading</h1><p>Hello <strong>world</strong>.</p>`+
			`<ul><li>First</li><li>Second</li></ul><footer>legal</footer></article></main></body></html>`)
	}))
	defer server.Close()
	client := newTestClient(t, Config{Fetch: &FetchConfig{
		MaxResponseBytes: 1 << 20, MaxContentCharacters: 10_000, MaxRedirects: 2,
		Timeout: time.Second, UserAgent: "Gofer Test", AllowPrivateAddresses: true,
	}})
	defer client.Close()
	response, err := client.Fetch(context.Background(), server.URL+"/start")
	if err != nil {
		t.Fatalf("Fetch(): %v", err)
	}
	if response.URL != server.URL+"/start" || response.FinalURL != server.URL+"/article" ||
		response.Title != "Example Page" || response.ContentType != "text/html" || response.Truncated {
		t.Fatalf("Fetch() = %#v", response)
	}
	if !strings.Contains(response.Content, "# Heading") || !strings.Contains(response.Content, "Hello world.") ||
		strings.Contains(response.Content, "secret") || strings.Contains(response.Content, "menu") || strings.Contains(response.Content, "legal") {
		t.Fatalf("content = %q", response.Content)
	}
}

func TestFetchHandlesTextJSONCharsetAndTruncation(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/latin":
			writer.Header().Set("Content-Type", "text/plain; charset=iso-8859-1")
			_, _ = writer.Write([]byte{'c', 'a', 'f', 0xe9})
		case "/json":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"message":"hello"}`)
		default:
			writer.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(writer, strings.Repeat("界", 200))
		}
	}))
	defer server.Close()
	client := newTestClient(t, Config{Fetch: &FetchConfig{
		MaxResponseBytes: 4096, MaxContentCharacters: 128, MaxRedirects: 1,
		Timeout: time.Second, UserAgent: "Gofer Test", AllowPrivateAddresses: true,
	}})
	defer client.Close()
	latin, err := client.Fetch(context.Background(), server.URL+"/latin")
	if err != nil || latin.Content != "café" || latin.Truncated {
		t.Fatalf("latin fetch = %#v, %v", latin, err)
	}
	jsonResult, err := client.Fetch(context.Background(), server.URL+"/json")
	if err != nil || jsonResult.Content != `{"message":"hello"}` || jsonResult.ContentType != "application/json" {
		t.Fatalf("JSON fetch = %#v, %v", jsonResult, err)
	}
	truncated, err := client.Fetch(context.Background(), server.URL+"/long")
	if err != nil || !truncated.Truncated || len([]rune(truncated.Content)) != 128 {
		t.Fatalf("truncated fetch = %d, %#v, %v", len([]rune(truncated.Content)), truncated, err)
	}
}

func TestFetchRejectsUnsafeAndInvalidResponses(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/status":
			writer.WriteHeader(http.StatusNotFound)
		case "/binary":
			writer.Header().Set("Content-Type", "image/png")
			_, _ = writer.Write([]byte("PNG"))
		case "/badtype":
			writer.Header().Set("Content-Type", "not a type;")
			_, _ = io.WriteString(writer, "text")
		case "/large":
			writer.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(writer, strings.Repeat("x", 2049))
		case "/redirect":
			http.Redirect(writer, request, "/status", http.StatusFound)
		}
	}))
	defer server.Close()
	client := newTestClient(t, Config{Fetch: &FetchConfig{
		MaxResponseBytes: 2048, MaxContentCharacters: 1000, MaxRedirects: 0,
		Timeout: time.Second, UserAgent: "Gofer Test", AllowPrivateAddresses: true,
	}})
	defer client.Close()
	for _, test := range []struct {
		path string
		want error
	}{
		{path: "/status", want: ErrUpstream},
		{path: "/binary", want: ErrUnsupportedContent},
		{path: "/badtype", want: ErrUnsupportedContent},
		{path: "/large", want: ErrResponseTooLarge},
		{path: "/redirect", want: ErrUpstream},
	} {
		if _, err := client.Fetch(context.Background(), server.URL+test.path); !errors.Is(err, test.want) {
			t.Errorf("Fetch(%s) error = %v, want %v", test.path, err, test.want)
		}
	}
	if _, err := client.Fetch(context.Background(), strings.Repeat("x", 4097)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Fetch(long URL) error = %v", err)
	}
	blocked := newTestClient(t, Config{Fetch: &FetchConfig{
		MaxResponseBytes: 2048, MaxContentCharacters: 1000, MaxRedirects: 1,
		Timeout: time.Second, UserAgent: "Gofer Test",
	}})
	defer blocked.Close()
	if _, err := blocked.Fetch(context.Background(), server.URL); err == nil {
		t.Fatal("Fetch(private server) error = nil")
	}
}

func TestClientAndToolValidation(t *testing.T) {
	t.Parallel()

	invalid := []Config{
		{},
		{Search: &SearchConfig{Provider: "other", MaxResults: 1, SafeSearch: "off", Timeout: time.Second}},
		{Search: &SearchConfig{Provider: "brave", MaxResults: 1, SafeSearch: "off", Timeout: time.Second}},
		{Search: &SearchConfig{Provider: "brave", APIKey: " secret", MaxResults: 1, SafeSearch: "off", Timeout: time.Second}},
		{Search: &SearchConfig{Provider: "searxng", MaxResults: 1, SafeSearch: "off", Timeout: time.Second}},
		{Search: &SearchConfig{Provider: "brave", APIKey: "x", Endpoint: "file:///tmp/x", MaxResults: 1, SafeSearch: "off", Timeout: time.Second}},
		{Fetch: &FetchConfig{}},
		{Fetch: &FetchConfig{MaxResponseBytes: 2048, MaxContentCharacters: 128, MaxRedirects: 1, Timeout: time.Second, UserAgent: " Gofer"}},
	}
	for _, config := range invalid {
		if _, err := New(config); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("New(%#v) error = %v", config, err)
		}
	}
	var nilClient *Client
	if nilClient.HasSearch() || nilClient.HasFetch() {
		t.Fatal("nil client reports capabilities")
	}
	if _, err := nilClient.Search(context.Background(), "x", 1); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil Search() error = %v", err)
	}
	if _, err := nilClient.Fetch(context.Background(), "https://example.com"); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil Fetch() error = %v", err)
	}
	nilClient.Close()
	if err := (Tools{}).Register(tool.NewRegistry()); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Tools.Register() error = %v", err)
	}
}

func TestResearchToolsAreReadOnlyUntrustedNetworkTools(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/search" {
			_, _ = io.WriteString(writer, `{"results":[{"title":"Result","url":"https://example.com","content":"snippet"}]}`)
			return
		}
		writer.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(writer, "document")
	}))
	defer server.Close()
	client := newTestClient(t, Config{
		Search: &SearchConfig{Provider: "searxng", Endpoint: server.URL, MaxResults: 2, SafeSearch: "moderate", Timeout: time.Second, AllowPrivateAddresses: true},
		Fetch:  &FetchConfig{MaxResponseBytes: 2048, MaxContentCharacters: 1000, MaxRedirects: 1, Timeout: time.Second, UserAgent: "Gofer Test", AllowPrivateAddresses: true},
	})
	defer client.Close()
	registry := tool.NewRegistry()
	if err := (Tools{Client: client}).Register(registry); err != nil {
		t.Fatalf("Register(): %v", err)
	}
	if names := registry.UntrustedOutputTools(); len(names) != 2 || names[0] != "web_fetch" || names[1] != "web_search" {
		t.Fatalf("untrusted tools = %#v", names)
	}
	for _, call := range []domain.ToolCall{
		{ID: "search", Name: "web_search", Arguments: json.RawMessage(`{"query":"go"}`)},
		{ID: "fetch", Name: "web_fetch", Arguments: json.RawMessage(`{"url":"` + server.URL + `/document"}`)},
	} {
		result, err := registry.Execute(context.Background(), call)
		if err != nil || result.IsError || !json.Valid(result.Output) {
			t.Fatalf("Execute(%s) = %#v, %v", call.Name, result, err)
		}
	}
	descriptors := PolicyDescriptors()
	if descriptors["web_search"].Effect != policy.EffectNetwork || descriptors["web_search"].ResourceFields[0] != "query" ||
		descriptors["web_fetch"].Effect != policy.EffectNetwork || descriptors["web_fetch"].ResourceFields[0] != "url" {
		t.Fatalf("descriptors = %#v", descriptors)
	}
}

func TestExtractHTMLFallsBackToDocument(t *testing.T) {
	t.Parallel()

	title, content, err := extractHTML(strings.NewReader(`<html><head><title>A &amp; B</title></head><div><h2>Intro</h2><p>Text<br>line</p></div></html>`))
	if err != nil || title != "A & B" || !strings.Contains(content, "## Intro") || !strings.Contains(content, "Text") {
		t.Fatalf("extractHTML() = %q, %q, %v", title, content, err)
	}
	if firstElement(nil, "body") != nil || textOf(nil) != "" {
		t.Fatal("nil HTML helpers returned content")
	}
}

func newTestClient(t *testing.T, config Config) *Client {
	t.Helper()
	client, err := New(config)
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	return client
}
