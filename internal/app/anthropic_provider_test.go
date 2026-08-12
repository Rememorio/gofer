package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Rememorio/gofer/internal/config"
	"github.com/Rememorio/gofer/internal/domain"
)

func TestServiceRunsNativeAnthropicModel(t *testing.T) {
	t.Parallel()
	requests := make(chan []byte, 1)
	modelServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/messages" || request.Header.Get("X-Api-Key") != "test-key" {
			t.Errorf("model request = %s headers=%v", request.URL.Path, request.Header)
		}
		body, _ := io.ReadAll(request.Body)
		requests <- body
		writer.Header().Set("Content-Type", "text/event-stream")
		writeAnthropicEvents(writer,
			`{"type":"message_start","message":{"id":"msg-1","type":"message","role":"assistant","model":"claude-test","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":7,"output_tokens":1}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"native Claude response"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":4}}`,
			`{"type":"message_stop"}`,
		)
	}))
	defer modelServer.Close()

	cfg := config.Defaults()
	cfg.Storage.Driver = "memory"
	cfg.Workspace.Root = t.TempDir()
	cfg.Models = []config.ModelConfig{{
		Name: "claude", Provider: "anthropic", Model: "claude-test",
		APIKey: "test-key", BaseURL: modelServer.URL, MaxTokens: 4096,
	}}
	service, err := New(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = service.Close() }()
	server := httptest.NewServer(service.Handler())
	defer server.Close()

	threadID := createThread(t, server.URL, "")
	runID := createRun(t, server.URL, threadID, `{"assistant_id":"claude","input":{"messages":[{"role":"user","content":"hello"}]}}`, "")
	waitRun(t, server.URL, threadID, runID, domain.RunSucceeded, "")
	messages := resourceRequest[[]domain.Message](t, server.URL, http.MethodGet,
		"/api/threads/"+string(threadID)+"/messages", nil, "", http.StatusOK)
	last := messages[len(messages)-1]
	if last.Role != domain.RoleAssistant || last.Content[0].Text != "native Claude response" {
		t.Fatalf("last message = %#v", last)
	}
	var payload struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		Stream    bool   `json:"stream"`
	}
	if err = json.Unmarshal(<-requests, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Model != "claude-test" || payload.MaxTokens != 4096 || !payload.Stream {
		t.Fatalf("model payload = %#v", payload)
	}
}

func writeAnthropicEvents(writer io.Writer, events ...string) {
	for _, payload := range events {
		var envelope struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(payload), &envelope)
		_, _ = fmt.Fprintf(writer, "event: %s\ndata: %s\n\n", envelope.Type, payload)
	}
}
