package humaninput

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
)

func TestStateTracksStructuredAndLegacyAnswers(t *testing.T) {
	t.Parallel()
	first := requestMessages(t, "first", `{"question":"Choose?","clarification_type":"approach_choice","options":["A","B"]}`)
	second := requestMessages(t, "second", `{"question":"Explain?","clarification_type":"missing_info"}`)
	response := Response{
		Version: 1, Kind: "human_input_response", Source: ToolName,
		RequestID: "clarification:first", ResponseKind: "option", OptionID: "option-2", Value: "B",
	}
	metadata, _ := ResponseMetadata(response)
	answer := userMessage(t, "B", map[string]string{ResponseMetadataKey: metadata, HideFromUIKey: "true"})
	messages := append(append(first, answer), second...)
	state := State(messages)
	if len(state.Requests) != 2 || len(state.OpenRequests) != 1 || state.LatestOpenRequestID != "clarification:second" ||
		state.AnsweredResponses["clarification:first"].Value != "B" {
		t.Fatalf("State() = %#v", state)
	}
	legacy := userMessage(t, "Here are the details", nil)
	state = State(append(messages, legacy))
	if len(state.OpenRequests) != 0 || state.AnsweredResponses["clarification:second"].Value != "Here are the details" {
		t.Fatalf("legacy State() = %#v", state)
	}
	hidden := userMessage(t, "internal", map[string]string{HideFromUIKey: "true"})
	state = State(append(second, hidden))
	if len(state.OpenRequests) != 1 {
		t.Fatalf("hidden State() = %#v", state)
	}
}

func TestValidateIncomingResponses(t *testing.T) {
	t.Parallel()
	history := requestMessages(t, "choice", `{"question":"Choose?","clarification_type":"approach_choice","options":["A","B"]}`)
	valid := responseMessage(t, Response{
		Version: 1, Kind: "human_input_response", Source: ToolName,
		RequestID: "clarification:choice", ResponseKind: "option", OptionID: "option-1", Value: "A",
	})
	if err := ValidateIncoming(history, []domain.Message{valid}); err != nil {
		t.Fatalf("ValidateIncoming(valid) = %v", err)
	}
	tests := []domain.Message{
		responseMessage(t, Response{Version: 1, Kind: "human_input_response", Source: ToolName, RequestID: "missing", ResponseKind: "text", Value: "x"}),
		responseMessage(t, Response{Version: 1, Kind: "human_input_response", Source: "other", RequestID: "clarification:choice", ResponseKind: "text", Value: "x"}),
		responseMessage(t, Response{Version: 1, Kind: "human_input_response", Source: ToolName, RequestID: "clarification:choice", ResponseKind: "option", OptionID: "option-9", Value: "Z"}),
	}
	for index, message := range tests {
		if err := ValidateIncoming(history, []domain.Message{message}); !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("case %d: error = %v", index, err)
		}
	}
	if err := ValidateIncoming(append(history, valid), []domain.Message{valid}); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("replay error = %v", err)
	}
	malformed := userMessage(t, "x", map[string]string{ResponseMetadataKey: `{}`})
	if err := ValidateIncoming(history, []domain.Message{malformed}); !IsInvalidResponse(err) {
		t.Fatalf("malformed error = %v", err)
	}
	if err := ValidateIncoming(history, []domain.Message{userMessage(t, "plain", nil)}); err != nil {
		t.Fatalf("legacy error = %v", err)
	}
}

func TestStateIgnoresUnrelatedToolResults(t *testing.T) {
	t.Parallel()
	message := domain.Message{
		ID: domain.MessageID("msg_01H00000000000000000000000"), Role: domain.RoleTool,
		Content:   []domain.Content{{Kind: domain.ContentToolResult, ToolResult: &domain.ToolResult{CallID: "x", Output: json.RawMessage(`{"ok":true}`)}}},
		CreatedAt: time.Now().UTC(),
	}
	state := State([]domain.Message{message})
	if len(state.Requests) != 0 || len(state.OpenRequests) != 0 {
		t.Fatalf("State() = %#v", state)
	}
}

func requestMessages(t *testing.T, id, arguments string) []domain.Message {
	t.Helper()
	call := clarificationCall(id, arguments)
	request, fallback, err := BuildRequest(call)
	if err != nil {
		t.Fatal(err)
	}
	output, err := MarshalToolOutput(request, fallback)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	assistant := domain.Message{
		ID: mustMessageID(t), Role: domain.RoleAssistant, CreatedAt: now,
		Content: []domain.Content{{Kind: domain.ContentToolCall, ToolCall: &call}},
	}
	result := domain.ToolResult{CallID: id, Output: output, Interrupt: true}
	toolMessage := domain.Message{
		ID: mustMessageID(t), Role: domain.RoleTool, CreatedAt: now.Add(time.Nanosecond),
		Content: []domain.Content{{Kind: domain.ContentToolResult, ToolResult: &result}},
	}
	return []domain.Message{assistant, toolMessage}
}

func responseMessage(t *testing.T, response Response) domain.Message {
	t.Helper()
	metadata, err := ResponseMetadata(response)
	if err != nil {
		t.Fatal(err)
	}
	return userMessage(t, response.Value, map[string]string{ResponseMetadataKey: metadata})
}

func mustMessageID(t *testing.T) domain.MessageID {
	t.Helper()
	id, err := domain.NewMessageID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
