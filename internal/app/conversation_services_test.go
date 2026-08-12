package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/store"
)

func TestConversationServiceHTTPWorkflow(t *testing.T) {
	t.Parallel()

	modelServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		writer.Header().Set("Content-Type", "text/event-stream")
		switch {
		case bytes.Contains(body, []byte("rough draft")):
			writeTextResponse(writer, "Rewrite this with concrete acceptance criteria.")
		case bytes.Contains(body, []byte("follow-up questions")):
			writeTextResponse(writer, "```json\n[\"Can you show an example?\",\"What are the tradeoffs?\",\"What are the tradeoffs?\"]\n```")
		default:
			t.Errorf("unexpected model request: %s", body)
			writeTextResponse(writer, "unexpected")
		}
	}))
	defer modelServer.Close()
	service, err := New(context.Background(), testConfig(t, modelServer.URL+"/v1"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = service.Close() }()
	server := httptest.NewServer(service.Handler())
	defer server.Close()
	threadID := createThread(t, server.URL, "")

	polished := resourceRequest[struct {
		RewrittenText string `json:"rewritten_text"`
		Changed       bool   `json:"changed"`
	}](t, server.URL, http.MethodPost, "/api/input-polish", map[string]string{
		"text": "/data-analysis make this better", "locale": "en-US", "thread_id": string(threadID),
	}, "", http.StatusOK)
	if polished.RewrittenText != "/data-analysis Rewrite this with concrete acceptance criteria." || !polished.Changed {
		t.Fatalf("polished = %#v", polished)
	}
	settings := resourceRequest[map[string]any](t, server.URL, http.MethodGet, "/api/suggestions/config", nil, "", http.StatusOK)
	if settings["enabled"] != true || settings["max_suggestions"] != float64(3) {
		t.Fatalf("suggestions config = %#v", settings)
	}
	suggested := resourceRequest[struct {
		Suggestions []string `json:"suggestions"`
	}](t, server.URL, http.MethodPost, "/api/threads/"+string(threadID)+"/suggestions", map[string]any{
		"messages": []map[string]string{{"role": "user", "content": "Explain Go contexts"}, {"role": "assistant", "content": "They propagate cancellation."}},
		"n":        3,
	}, "", http.StatusOK)
	if len(suggested.Suggestions) != 2 || suggested.Suggestions[0] != "Can you show an example?" {
		t.Fatalf("suggestions = %#v", suggested.Suggestions)
	}
}

func TestConversationServiceValidationAndDisabledBehavior(t *testing.T) {
	t.Parallel()

	modelServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writeTextResponse(writer, "not used")
	}))
	defer modelServer.Close()
	cfg := testConfig(t, modelServer.URL+"/v1")
	cfg.InputPolish.Enabled = false
	cfg.Suggestions.Enabled = false
	service, err := New(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = service.Close() }()
	server := httptest.NewServer(service.Handler())
	defer server.Close()
	threadID := createThread(t, server.URL, "")
	resourceRequest[map[string]string](t, server.URL, http.MethodPost, "/api/input-polish", map[string]string{"text": "draft"}, "", http.StatusNotFound)
	disabled := resourceRequest[struct {
		Suggestions []string `json:"suggestions"`
	}](t, server.URL, http.MethodPost, "/api/threads/"+string(threadID)+"/suggestions", map[string]any{
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
	}, "", http.StatusOK)
	if len(disabled.Suggestions) != 0 {
		t.Fatalf("disabled suggestions = %#v", disabled.Suggestions)
	}
	resourceRequest[map[string]string](t, server.URL, http.MethodPost, "/api/threads/not-an-id/suggestions", map[string]any{}, "", http.StatusBadRequest)
}

func TestConversationServiceRejectsInvalidRequestsAndFailsSoftly(t *testing.T) {
	t.Parallel()

	modelServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer modelServer.Close()
	cfg := testConfig(t, modelServer.URL+"/v1")
	cfg.InputPolish.MaxChars = 5
	service, err := New(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = service.Close() }()
	server := httptest.NewServer(service.Handler())
	defer server.Close()
	threadID := createThread(t, server.URL, "")
	for _, body := range []any{
		map[string]string{"text": ""}, map[string]string{"text": "123456"}, map[string]string{"text": "ok", "locale": strings.Repeat("x", 65)},
	} {
		resourceRequest[map[string]string](t, server.URL, http.MethodPost, "/api/input-polish", body, "", http.StatusBadRequest)
	}
	resourceRequest[map[string]string](t, server.URL, http.MethodPost, "/api/input-polish", map[string]string{"text": "okay"}, "", http.StatusServiceUnavailable)
	for _, body := range []any{
		map[string]any{"messages": []map[string]string{{"role": "tool", "content": "x"}}, "n": 1},
		map[string]any{"messages": []map[string]string{{"role": "user", "content": "x"}}, "n": 6},
		map[string]any{"messages": []map[string]string{{"role": "user", "content": "x"}}, "model_name": "missing"},
	} {
		resourceRequest[map[string]string](t, server.URL, http.MethodPost, "/api/threads/"+string(threadID)+"/suggestions", body, "", http.StatusBadRequest)
	}
	failed := resourceRequest[struct {
		Suggestions []string `json:"suggestions"`
	}](t, server.URL, http.MethodPost, "/api/threads/"+string(threadID)+"/suggestions", map[string]any{
		"messages": []map[string]string{{"role": "user", "content": "x"}}, "n": 1,
	}, "", http.StatusOK)
	if len(failed.Suggestions) != 0 {
		t.Fatalf("failed suggestions = %#v", failed.Suggestions)
	}
}

func TestServiceAssignsFallbackTitleAfterFirstRun(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	modelServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeTextResponse(writer, "Done")
	}))
	defer modelServer.Close()
	service, err := New(context.Background(), testConfig(t, modelServer.URL+"/v1"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = service.Close() }()
	server := httptest.NewServer(service.Handler())
	defer server.Close()
	threadID := createUntitledThread(t, server.URL)
	runID := createRun(t, server.URL, threadID, `{"assistant_id":"primary","input":{"messages":[{"role":"user","content":"Analyze quarterly revenue trends for leadership"}]}}`, "")
	waitRun(t, server.URL, threadID, runID, domain.RunSucceeded, "")
	title := waitThreadTitle(t, service, threadID)
	if title != "Analyze quarterly revenue trends for leadership" || calls.Load() != 1 {
		t.Fatalf("title/calls = %q/%d", title, calls.Load())
	}
}

func TestServiceUsesConfiguredTitleModelWithoutOverwritingManualTitle(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	modelServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		calls.Add(1)
		if bytes.Contains(body, []byte("concise conversation title")) {
			writeTextResponse(writer, "\"Quarterly Revenue Analysis\"")
			return
		}
		writeTextResponse(writer, "Done")
	}))
	defer modelServer.Close()
	cfg := testConfig(t, modelServer.URL+"/v1")
	cfg.Title.ModelName = "primary"
	service, err := New(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = service.Close() }()
	server := httptest.NewServer(service.Handler())
	defer server.Close()
	threadID := createUntitledThread(t, server.URL)
	runID := createRun(t, server.URL, threadID, `{"assistant_id":"primary","input":{"messages":[{"role":"user","content":"Analyze revenue"}]}}`, "")
	waitRun(t, server.URL, threadID, runID, domain.RunSucceeded, "")
	if title := waitThreadTitle(t, service, threadID); title != "Quarterly Revenue Analysis" {
		t.Fatalf("generated title = %q", title)
	}
	if calls.Load() != 2 {
		t.Fatalf("model calls = %d, want 2", calls.Load())
	}
	manual := "Manual title"
	if _, err = service.store.PatchThread(context.Background(), threadID, storePatchTitle(manual), time.Now()); err != nil {
		t.Fatal(err)
	}
	service.assignAutomaticTitle(threadID, []domain.Message{newTextMessage(t, domain.RoleUser, "Another")}, context.Canceled)
	thread, _ := service.store.Thread(context.Background(), threadID)
	if thread.Title != manual || calls.Load() != 2 {
		t.Fatalf("manual title/calls = %q/%d", thread.Title, calls.Load())
	}
}

func TestConversationServiceHelpers(t *testing.T) {
	t.Parallel()
	if got := parseSuggestions(`prefix [" One ","One","Two\nlines",""] suffix`, 3); len(got) != 2 || got[1] != "Two lines" {
		t.Fatalf("parseSuggestions() = %#v", got)
	}
	if got := parseSuggestions("not json", 3); len(got) != 0 {
		t.Fatalf("parseSuggestions(invalid) = %#v", got)
	}
	if preserveSlashCommand("plain", " rewritten ") != "rewritten" || leadingSlashCommand("/") != "" || leadingSlashCommand("/skill") != "/skill" {
		t.Fatal("slash command helpers returned unexpected values")
	}
	if got := preserveSlashCommand("/skill task", "/skillful rewrite"); got != "/skill /skillful rewrite" {
		t.Fatalf("preserveSlashCommand(prefix collision) = %q", got)
	}
	if got := normalizeTitle("one two three four", 3, 100); got != "one two three" {
		t.Fatalf("normalizeTitle(words) = %q", got)
	}
	if got := fallbackTitle(strings.Repeat("界", 60), 6, 20); len([]rune(got)) != 20 || !strings.HasSuffix(got, "...") {
		t.Fatalf("fallbackTitle() = %q", got)
	}
	first, second := newTextMessage(t, domain.RoleUser, "first"), newTextMessage(t, domain.RoleUser, "second")
	if _, _, ok := firstExchange([]domain.Message{first, second}); ok {
		t.Fatal("firstExchange(two users) = true")
	}
	if validSuggestionMessages([]suggestionMessage{{Role: "user", Content: ""}}) {
		t.Fatal("validSuggestionMessages(empty) = true")
	}
}

func writeTextResponse(writer http.ResponseWriter, text string) {
	writer.Header().Set("Content-Type", "text/event-stream")
	writeSSE(writer,
		fmt.Sprintf(`{"id":"aux","object":"chat.completion.chunk","created":1,"model":"gpt-test","choices":[{"index":0,"delta":{"content":%q},"finish_reason":null}]}`, text),
		`{"id":"aux","object":"chat.completion.chunk","created":1,"model":"gpt-test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	)
}

func createUntitledThread(t *testing.T, baseURL string) domain.ThreadID {
	t.Helper()
	created := resourceRequest[struct {
		ThreadID domain.ThreadID `json:"thread_id"`
	}](t, baseURL, http.MethodPost, "/api/threads", map[string]any{}, "", http.StatusOK)
	return created.ThreadID
}

func waitThreadTitle(t *testing.T, service *Service, threadID domain.ThreadID) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		thread, err := service.store.Thread(context.Background(), threadID)
		if err == nil && thread.Title != "" {
			return thread.Title
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("thread title was not assigned")
	return ""
}

func newTextMessage(t *testing.T, role domain.Role, text string) domain.Message {
	t.Helper()
	message, err := domain.NewTextMessage(role, text, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return message
}

func storePatchTitle(title string) store.ThreadPatch {
	return store.ThreadPatch{Title: &title}
}
