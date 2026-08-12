package humaninput

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/model"
	"github.com/Rememorio/gofer/internal/runtime"
	"github.com/Rememorio/gofer/internal/tool"
)

func TestBuildRequestNormalizesChoices(t *testing.T) {
	t.Parallel()
	call := clarificationCall("choice", `{
		"question":" Which deployment? ","clarification_type":"approach_choice",
		"context":"Two safe paths exist.","options":"[\"<b>Blue</b>\",\"Green\",\"Blue\",\"\"]"
	}`)
	request, fallback, err := BuildRequest(call)
	if err != nil {
		t.Fatal(err)
	}
	if request.Version != 1 || request.InputMode != ChoiceWithOther || request.RequestID != "clarification:choice" ||
		request.Context == nil || len(request.Options) != 2 || request.Options[0].Label != "Blue" ||
		request.Options[1].ID != "option-2" || !strings.Contains(fallback, "🔀 Two safe paths exist.") ||
		!strings.Contains(fallback, "2. Green") {
		t.Fatalf("request/fallback = %#v / %q", request, fallback)
	}
	output, err := MarshalToolOutput(request, fallback)
	if err != nil {
		t.Fatal(err)
	}
	decoded, ok := RequestFromOutput(output)
	if !ok || decoded.RequestID != request.RequestID {
		t.Fatalf("RequestFromOutput() = %#v, %v", decoded, ok)
	}
	var envelope map[string]any
	if err = json.Unmarshal(output, &envelope); err != nil || envelope["artifact"] == nil || envelope["human_input"] == nil {
		t.Fatalf("output = %s, %v", output, err)
	}
}

func TestBuildRequestNormalizesStructuredForm(t *testing.T) {
	t.Parallel()
	call := clarificationCall("form", `{
		"question":"Release settings?","clarification_type":"risk_confirmation",
		"fields":[
			{"name":"environment","label":"Environment","type":"select","required":"true","options":["Stage","Prod"],"placeholder":"Choose one"},
			{"name":"notes","type":"unknown","required":0},
			{"name":"regions","type":"multi_select","options":["EU","US"]},
			{"name":"approved","type":"checkbox","required":1}
		]
	}`)
	request, fallback, err := BuildRequest(call)
	if err != nil {
		t.Fatal(err)
	}
	if request.Version != 2 || request.InputMode != Form || len(request.Fields) != 4 || len(request.Options) != 0 {
		t.Fatalf("request = %#v", request)
	}
	if request.Fields[0].Type != FieldSelect || !request.Fields[0].Required || request.Fields[0].Options[1].ID != "environment-option-2" ||
		request.Fields[1].Type != FieldText || request.Fields[1].Label != "notes" || request.Fields[3].Type != FieldCheckbox ||
		!strings.Contains(fallback, "multiple allowed") || !strings.Contains(fallback, "Please reply with a value for each field.") {
		t.Fatalf("fields/fallback = %#v / %q", request.Fields, fallback)
	}
}

func TestBuildRequestDegradesMalformedFormsAtomically(t *testing.T) {
	t.Parallel()
	tests := []string{
		`[{"name":"constructor","type":"text"}]`,
		`[{"name":"same"},{"name":"same"}]`,
		`[{"name":"ok"},false]`,
	}
	for _, fields := range tests {
		call := clarificationCall("degrade", `{"question":"Choose?","clarification_type":"suggestion","options":["A","B"],"fields":`+fields+`}`)
		request, _, err := BuildRequest(call)
		if err != nil || request.InputMode != ChoiceWithOther || len(request.Fields) != 0 {
			t.Fatalf("fields %s: request=%#v err=%v", fields, request, err)
		}
	}
	call := clarificationCall("benign", `{"question":"Choose?","clarification_type":"suggestion","fields":[{"name":"choice","type":"select","options":[]}]}`)
	request, _, err := BuildRequest(call)
	if err != nil || request.InputMode != Form || request.Fields[0].Type != FieldText {
		t.Fatalf("option-less select = %#v, %v", request, err)
	}
	tooMany := make([]string, maxOptions+1)
	for index := range tooMany {
		tooMany[index] = quoted("value")
	}
	call = clarificationCall("bounded", `{"question":"Choose?","clarification_type":"suggestion","options":["fallback"],"fields":[{"name":"choice","type":"select","options":[`+strings.Join(tooMany, ",")+`]}]}`)
	request, _, err = BuildRequest(call)
	if err != nil || request.InputMode != ChoiceWithOther || len(request.Fields) != 0 {
		t.Fatalf("over-cap field options = %#v, %v", request, err)
	}
}

func TestBuildRequestRejectsInvalidPrimaryArguments(t *testing.T) {
	t.Parallel()
	longQuestion := strings.Repeat("q", maxQuestionRunes+1)
	tests := []domain.ToolCall{
		{ID: "x", Name: "other", Arguments: json.RawMessage(`{}`)},
		{ID: "", Name: ToolName, Arguments: json.RawMessage(`{}`)},
		clarificationCall("x", `[]`),
		clarificationCall("x", `{}`),
		clarificationCall("x", `{"question":`+quoted(longQuestion)+`}`),
		clarificationCall("x", `{"question":"x","context":`+quoted(strings.Repeat("c", maxContextRunes+1))+`}`),
	}
	for index, call := range tests {
		if _, _, err := BuildRequest(call); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("case %d: error = %v", index, err)
		}
	}
	if _, ok := RequestFromOutput(json.RawMessage(`{"human_input":false}`)); ok {
		t.Fatal("malformed output was accepted")
	}
}

func TestResponseProtocolAndMetadata(t *testing.T) {
	t.Parallel()
	textRaw := json.RawMessage(`{"version":1,"kind":"human_input_response","source":" ask_clarification ","request_id":" clarification:x ","response_kind":"text","value":" yes "}`)
	response, err := ParseResponse(textRaw)
	if err != nil || response.Value != "yes" || response.RequestID != "clarification:x" {
		t.Fatalf("ParseResponse() = %#v, %v", response, err)
	}
	metadata, err := ResponseMetadata(response)
	if err != nil {
		t.Fatal(err)
	}
	message := userMessage(t, "yes", map[string]string{ResponseMetadataKey: metadata, HideFromUIKey: "TRUE"})
	decoded, ok := ResponseFromMessage(message)
	if !ok || decoded.Source != ToolName || !Hidden(message) {
		t.Fatalf("response/message = %#v / %#v", decoded, message)
	}
	invalid := []json.RawMessage{
		nil,
		json.RawMessage(`{"version":2,"kind":"human_input_response","source":"x","request_id":"r","response_kind":"text","value":"x"}`),
		json.RawMessage(`{"version":1,"kind":"bad","source":"x","request_id":"r","response_kind":"text","value":"x"}`),
		json.RawMessage(`{"version":1,"kind":"human_input_response","source":"x","request_id":"r","response_kind":"option","value":"x"}`),
		json.RawMessage(`{"version":1,"kind":"human_input_response","source":"x","request_id":"r","response_kind":"other","value":"x"}`),
		json.RawMessage(`{"version":1,"kind":"human_input_response","source":"x","request_id":"r","response_kind":"text","option_id":"o","value":"x"}`),
		json.RawMessage(`{"version":1,"kind":"human_input_response","source":"x","request_id":"r","response_kind":"text","value":"x","extra":true}`),
	}
	for index, raw := range invalid {
		if _, parseErr := ParseResponse(raw); !errors.Is(parseErr, ErrInvalidResponse) {
			t.Fatalf("case %d: error = %v", index, parseErr)
		}
	}
}

func TestMiddlewareInterruptsAndMakesClarificationExclusive(t *testing.T) {
	t.Parallel()
	middleware := &Middleware{}
	clarification := clarificationCall("ask", `{"question":"Proceed?","clarification_type":"risk_confirmation","options":["Yes","No"]}`)
	response, err := middleware.TransformModelResponse(context.Background(), model.Response{
		Text: "thinking", ToolCalls: []domain.ToolCall{
			{ID: "write", Name: "write_file", Arguments: json.RawMessage(`{}`)}, clarification,
		}, StopReason: model.StopToolUse,
	})
	if err != nil || len(response.ToolCalls) != 1 || response.ToolCalls[0].ID != "ask" {
		t.Fatalf("TransformModelResponse() = %#v, %v", response, err)
	}
	result, err := middleware.ExecuteTool(context.Background(), clarification, func(context.Context, domain.ToolCall) (domain.ToolResult, error) {
		t.Fatal("next executor was called")
		return domain.ToolResult{}, nil
	})
	if err != nil || result.IsError || !result.Interrupt {
		t.Fatalf("ExecuteTool() = %#v, %v", result, err)
	}
	request, ok := RequestFromOutput(result.Output)
	if !ok || request.ToolCallID != "ask" {
		t.Fatalf("output = %s", result.Output)
	}
	nextCalled := false
	ordinary := domain.ToolCall{ID: "echo", Name: "echo", Arguments: json.RawMessage(`{}`)}
	result, err = middleware.ExecuteTool(context.Background(), ordinary, func(_ context.Context, call domain.ToolCall) (domain.ToolResult, error) {
		nextCalled = true
		return domain.ToolResult{CallID: call.ID, Output: json.RawMessage(`{"ok":true}`)}, nil
	})
	if err != nil || !nextCalled || result.CallID != "echo" {
		t.Fatalf("ordinary result = %#v, %v", result, err)
	}
}

func TestMiddlewareDisabledAndInvalidCallsContinue(t *testing.T) {
	t.Parallel()
	call := clarificationCall("ask", `{"question":"Proceed?","clarification_type":"missing_info"}`)
	tests := []struct {
		middleware *Middleware
		ctx        context.Context
	}{{middleware: &Middleware{Disabled: true}, ctx: context.Background()}, {middleware: &Middleware{}, ctx: WithDisabled(context.Background())}}
	for _, test := range tests {
		result, err := test.middleware.ExecuteTool(test.ctx, call, nil)
		if err != nil || result.Interrupt || result.IsError || !strings.Contains(string(result.Output), "best judgment") {
			t.Fatalf("disabled result = %#v, %v", result, err)
		}
	}
	invalid := clarificationCall("bad", `{}`)
	result, err := (&Middleware{}).ExecuteTool(context.Background(), invalid, nil)
	if err != nil || !result.IsError || result.Interrupt {
		t.Fatalf("invalid result = %#v, %v", result, err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = (&Middleware{}).ExecuteTool(cancelled, call, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled error = %v", err)
	}
	registry := tool.NewRegistry()
	if err = registry.Register(Tool()); err != nil {
		t.Fatalf("Register(Tool()) = %v", err)
	}
}

func TestRequestValidationRejectsMalformedArtifacts(t *testing.T) {
	t.Parallel()
	base, _, _ := BuildRequest(clarificationCall("x", `{"question":"Question?","clarification_type":"missing_info"}`))
	mutations := []func(*Request){
		func(request *Request) { request.Kind = "bad" },
		func(request *Request) { request.Version = 2 },
		func(request *Request) { request.InputMode = "bad" },
		func(request *Request) { request.ClarificationType = "bad" },
		func(request *Request) { request.Question = "" },
		func(request *Request) { request.RequestID = strings.Repeat("r", maxRequestIDRunes+1) },
		func(request *Request) { request.InputMode, request.Options = SingleChoice, nil },
		func(request *Request) {
			request.Version, request.InputMode, request.Fields = 2, Form, []Field{{Name: "x", Label: "X", Type: FieldSelect}}
		},
	}
	for index, mutate := range mutations {
		candidate := base
		mutate(&candidate)
		if err := candidate.Validate(); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("case %d: error = %v", index, err)
		}
	}
}

func clarificationCall(id, arguments string) domain.ToolCall {
	return domain.ToolCall{ID: id, Name: ToolName, Arguments: json.RawMessage(arguments)}
}

func quoted(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func userMessage(t *testing.T, text string, metadata map[string]string) domain.Message {
	t.Helper()
	message, err := domain.NewTextMessage(domain.RoleUser, text, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	message.Metadata = metadata
	return message
}

var _ runtime.ToolExecutionInterceptor = (*Middleware)(nil)
