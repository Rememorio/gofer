package app

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/gateway"
)

func TestDecodeLaunchAcceptsDeerFlowAndLangChainMessages(t *testing.T) {
	t.Parallel()
	temperature := 0.4
	request := gateway.RunRequest{
		AssistantID: "fallback",
		Input: json.RawMessage(`{"messages":[
			{"type":"system","content":"rules"},
			{"type":"human","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]},
			{"role":"assistant","content":"checking","tool_calls":[{"id":"call-1","name":"lookup","args":{"q":"go"}}]},
			{"role":"tool","tool_call_id":"call-1","content":{"answer":42}}
		]}`),
		Config:  json.RawMessage(`{"configurable":{"model":"configured"},"system":"base","max_tokens":100}`),
		Context: json.RawMessage(`{"model":"primary","system_prompt":"final","temperature":0.4}`),
	}
	messages, settings, err := decodeLaunch(request, time.Unix(10, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 4 || messages[0].Role != domain.RoleSystem || messages[1].Role != domain.RoleUser || messages[2].Role != domain.RoleAssistant || messages[3].Role != domain.RoleTool {
		t.Fatalf("messages = %#v", messages)
	}
	if len(messages[1].Content) != 2 || messages[1].Content[1].Kind != domain.ContentImage || len(messages[2].Content) != 2 {
		t.Fatalf("content = %#v / %#v", messages[1].Content, messages[2].Content)
	}
	if messages[3].Content[0].ToolResult.CallID != "call-1" || string(messages[3].Content[0].ToolResult.Output) != `{"answer":42}` {
		t.Fatalf("tool result = %#v", messages[3])
	}
	if settings.model != "primary" || settings.system != "final" || settings.maxTokens != 100 || settings.temperature == nil || *settings.temperature != temperature {
		t.Fatalf("settings = %#v", settings)
	}
}

func TestDecodeLaunchAcceptsArrayAndStringToolOutput(t *testing.T) {
	t.Parallel()
	messages, settings, err := decodeLaunch(gateway.RunRequest{
		Input:   json.RawMessage(`[{"role":"user","content":"hello"},{"role":"assistant","content":"call","tool_calls":[{"id":"x","name":"echo","arguments":{"v":1}}]},{"role":"tool","tool_call_id":"x","content":"ok"}]`),
		Context: json.RawMessage(`null`),
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 || string(messages[2].Content[0].ToolResult.Output) != `"ok"` || settings.model != "" {
		t.Fatalf("messages/settings = %#v %#v", messages, settings)
	}
}

func TestDecodeLaunchAcceptsCurrentUploadMetadata(t *testing.T) {
	t.Parallel()
	messages, settings, err := decodeLaunch(gateway.RunRequest{Input: json.RawMessage(`{"messages":[
		{"role":"user","content":"old","files":[{"filename":"old.txt","size":1}]},
		{"role":"assistant","content":"reply"},
		{"role":"user","content":"new","additional_kwargs":{"files":[{"filename":"report.pdf","size":42}],"client":"web"}}
	]}`)}, time.Now())
	if err != nil || len(messages) != 3 || len(settings.uploads) != 1 || settings.uploads[0].Filename != "report.pdf" || settings.uploads[0].Size != 42 {
		t.Fatalf("messages/settings = %#v / %#v, %v", messages, settings, err)
	}
}

func TestDecodeLaunchPreservesHumanInputResponse(t *testing.T) {
	t.Parallel()
	messages, settings, err := decodeLaunch(gateway.RunRequest{
		Input:   json.RawMessage(`{"messages":[{"role":"user","content":"Staging","additional_kwargs":{"hide_from_ui":true,"human_input_response":{"version":1,"kind":"human_input_response","source":"ask_clarification","request_id":"clarification:ask","response_kind":"option","option_id":"option-1","value":"Staging"}}}]}`),
		Context: json.RawMessage(`{"disable_clarification":true}`),
	}, time.Now())
	if err != nil || len(messages) != 1 || messages[0].Metadata["hide_from_ui"] != "true" ||
		!strings.Contains(messages[0].Metadata["human_input_response"], `"request_id":"clarification:ask"`) || !settings.disableClarification {
		t.Fatalf("messages/settings = %#v / %#v, %v", messages, settings, err)
	}
}

func TestDecodeLaunchRejectsMalformedInputs(t *testing.T) {
	t.Parallel()
	tests := []gateway.RunRequest{
		{},
		{Input: json.RawMessage(`{}`)},
		{Input: json.RawMessage(`{"messages":[]}`)},
		{Input: json.RawMessage(`{"messages":[{"role":"alien","content":"x"}]}`)},
		{Input: json.RawMessage(`{"messages":[{"role":"user","content":""}]}`)},
		{Input: json.RawMessage(`{"messages":[{"role":"user","content":[]}]}`)},
		{Input: json.RawMessage(`{"messages":[{"role":"user","content":[{"type":"audio","url":"x"}]}]}`)},
		{Input: json.RawMessage(`{"messages":[{"role":"assistant","content":"x","tool_calls":[{"name":"bad"}]}]}`)},
		{Input: json.RawMessage(`{"messages":[{"role":"tool","content":"x"}]}`)},
		{Input: json.RawMessage(`{"messages":[{"role":"user","content":"x","unknown":true}]}`)},
		{Input: json.RawMessage(`{"messages":[{"role":"user","content":"x"}]}`), Context: json.RawMessage(`{`)},
		{Input: json.RawMessage(`{"messages":[{"role":"user","content":"x"}]}`), Context: json.RawMessage(`{"max_tokens":-1}`)},
		{Input: json.RawMessage(`{"messages":[{"role":"user","content":"x"}]}`), Context: json.RawMessage(`{"temperature":3}`)},
		{Input: json.RawMessage(`{"messages":[{"role":"user","content":"x","additional_kwargs":[]}]}`)},
		{Input: json.RawMessage(`{"messages":[{"role":"user","content":"x","additional_kwargs":{"hide_from_ui":"yes"}}]}`)},
		{Input: json.RawMessage(`{"messages":[{"role":"user","content":"x","additional_kwargs":{"human_input_response":{}}}]}`)},
		{Input: json.RawMessage(`{"messages":[{"role":"assistant","content":"x","additional_kwargs":{"human_input_response":{"version":1,"kind":"human_input_response","source":"ask_clarification","request_id":"r","response_kind":"text","value":"x"}}}]}`)},
		{Input: json.RawMessage(`{"messages":[{"role":"user","content":"x","files":{}}]}`)},
		{Input: json.RawMessage(`{"messages":[{"role":"user","content":"x","files":[{"filename":"x","size":-1}]}]}`)},
	}
	for index, request := range tests {
		if _, _, err := decodeLaunch(request, time.Now()); err == nil {
			t.Fatalf("case %d: error = nil", index)
		}
	}
}

func TestInputHelpersRejectTrailingAndInvalidBlocks(t *testing.T) {
	t.Parallel()
	var value map[string]any
	if err := strictJSON([]byte(`{} {}`), &value); err == nil {
		t.Fatal("strictJSON() error = nil")
	}
	if _, err := normalizeRole("", ""); err == nil {
		t.Fatal("normalizeRole() error = nil")
	}
	if _, err := decodeContentBlocks(json.RawMessage(`false`)); err == nil {
		t.Fatal("decodeContentBlocks() error = nil")
	}
	if _, err := decodeContentBlocks(json.RawMessage(`[{"type":"text","text":""}]`)); err == nil || !strings.Contains(err.Error(), "text") {
		t.Fatalf("decodeContentBlocks(empty text) error = %v", err)
	}
}
