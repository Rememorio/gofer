package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Rememorio/gofer/internal/domain"
)

func TestAssistantCompatibilityResources(t *testing.T) {
	t.Parallel()
	modelServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer modelServer.Close()
	service, err := New(context.Background(), testConfig(t, modelServer.URL+"/v1"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = service.Close() }()
	server := httptest.NewServer(service.Handler())
	defer server.Close()
	assistants := resourceRequest[[]assistantResource](t, server.URL, http.MethodPost, "/api/assistants/search", nil, "", http.StatusOK)
	if len(assistants) != 2 || assistants[0].AssistantID != "lead_agent" || assistants[1].AssistantID != "primary" {
		t.Fatalf("assistants = %#v", assistants)
	}
	filtered := resourceRequest[[]assistantResource](t, server.URL, http.MethodPost, "/api/assistants/search", map[string]any{"graph_id": "lead_agent", "name": "primary", "limit": 1}, "", http.StatusOK)
	if len(filtered) != 1 || filtered[0].AssistantID != "primary" {
		t.Fatalf("filtered = %#v", filtered)
	}
	assistant := resourceRequest[assistantResource](t, server.URL, http.MethodGet, "/api/assistants/lead_agent", nil, "", http.StatusOK)
	if assistant.GraphID != "lead_agent" || assistant.Config["configurable"] == nil {
		t.Fatalf("assistant = %#v", assistant)
	}
	graph := resourceRequest[map[string]any](t, server.URL, http.MethodGet, "/api/assistants/lead_agent/graph", nil, "", http.StatusOK)
	if len(graph["nodes"].([]any)) != 2 {
		t.Fatalf("graph = %#v", graph)
	}
	schemas := resourceRequest[map[string]any](t, server.URL, http.MethodGet, "/api/assistants/primary/schemas", nil, "", http.StatusOK)
	if schemas["input_schema"] == nil || schemas["config_schema"] == nil {
		t.Fatalf("schemas = %#v", schemas)
	}
	resourceRequest[map[string]string](t, server.URL, http.MethodGet, "/api/assistants/missing", nil, "", http.StatusNotFound)
	resourceRequest[map[string]string](t, server.URL, http.MethodPost, "/api/assistants/search", map[string]int{"limit": 0}, "", http.StatusBadRequest)
	if _, err = service.selectProvider("lead_agent"); err != nil {
		t.Fatalf("lead provider = %v", err)
	}
}

func TestServiceRunCompatibilityHTTP(t *testing.T) {
	t.Parallel()
	modelServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writeSSE(writer,
			`{"id":"compat","object":"chat.completion.chunk","created":1,"model":"gpt-test","choices":[{"index":0,"delta":{"content":"done"},"finish_reason":null}]}`,
			`{"id":"compat","object":"chat.completion.chunk","created":1,"model":"gpt-test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		)
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
	waited := resourceRequest[struct {
		Messages []domain.Message `json:"messages"`
	}](t, server.URL, http.MethodPost, "/api/threads/"+string(threadID)+"/runs/wait", map[string]any{"assistant_id": "lead_agent", "input": map[string]any{"messages": []map[string]string{{"role": "user", "content": "hello"}}}}, "", http.StatusOK)
	if len(waited.Messages) != 2 || waited.Messages[1].Role != domain.RoleAssistant {
		t.Fatalf("waited = %#v", waited)
	}
	stream := resourceRawRequest(t, server.URL, http.MethodPost, "/api/runs/stream", map[string]any{"assistant_id": "lead_agent", "input": map[string]any{"messages": []map[string]string{{"role": "user", "content": "stream"}}}}, "", http.StatusOK)
	body, _ := io.ReadAll(stream.Body)
	_ = stream.Body.Close()
	if stream.Header.Get("Content-Location") == "" || !strings.Contains(string(body), "event: run.completed") {
		t.Fatalf("stream = %#v %s", stream.Header, body)
	}
}
