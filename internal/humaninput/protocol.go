// Package humaninput implements DeerFlow's structured clarification protocol.
package humaninput

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/Rememorio/gofer/internal/domain"
)

const (
	// ToolName is the model-visible clarification tool.
	ToolName = "ask_clarification"
	// ResponseMetadataKey stores the canonical v1 response on a user message.
	ResponseMetadataKey = "human_input_response"
	// HideFromUIKey mirrors DeerFlow's hidden response-message marker.
	HideFromUIKey = "hide_from_ui"

	maxFields          = 16
	maxOptions         = 24
	maxFieldTextRunes  = 200
	maxQuestionRunes   = 8_000
	maxContextRunes    = 8_000
	maxResponseRunes   = 32_000
	maxRequestIDRunes  = 512
	maxFieldsJSONBytes = 16 << 10
)

var (
	// ErrInvalidRequest identifies malformed clarification arguments or artifacts.
	ErrInvalidRequest = errors.New("invalid human input request")
	// ErrInvalidResponse identifies malformed, stale, or mismatched user input.
	ErrInvalidResponse = errors.New("invalid human input response")

	xmlTagPattern = regexp.MustCompile(`</?[A-Za-z_][\w:.-]*(?:\s[^<>]*?)?\s*/?>`)
	reservedNames = map[string]struct{}{
		"__proto__": {}, "constructor": {}, "prototype": {}, "toString": {},
		"toLocaleString": {}, "valueOf": {}, "hasOwnProperty": {},
		"isPrototypeOf": {}, "propertyIsEnumerable": {}, "__defineGetter__": {},
		"__defineSetter__": {}, "__lookupGetter__": {}, "__lookupSetter__": {},
	}
)

// ClarificationType explains why the agent cannot proceed safely yet.
type ClarificationType string

// DeerFlow-compatible clarification categories.
const (
	MissingInfo          ClarificationType = "missing_info"
	AmbiguousRequirement ClarificationType = "ambiguous_requirement"
	ApproachChoice       ClarificationType = "approach_choice"
	RiskConfirmation     ClarificationType = "risk_confirmation"
	Suggestion           ClarificationType = "suggestion"
)

// InputMode selects the response UI rendered by a client.
type InputMode string

// Supported interaction modes.
const (
	FreeText        InputMode = "free_text"
	SingleChoice    InputMode = "single_choice"
	ChoiceWithOther InputMode = "choice_with_other"
	Form            InputMode = "form"
)

// FieldType identifies one structured form control.
type FieldType string

// Supported form controls.
const (
	FieldText        FieldType = "text"
	FieldTextarea    FieldType = "textarea"
	FieldNumber      FieldType = "number"
	FieldSelect      FieldType = "select"
	FieldMultiSelect FieldType = "multi_select"
	FieldCheckbox    FieldType = "checkbox"
	FieldDate        FieldType = "date"
)

// Option is one stable client-visible choice.
type Option struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Value string `json:"value"`
}

// Field is one normalized v2 form entry.
type Field struct {
	Name        string    `json:"name"`
	Label       string    `json:"label"`
	Type        FieldType `json:"type"`
	Required    bool      `json:"required"`
	Placeholder string    `json:"placeholder,omitempty"`
	Options     []Option  `json:"options,omitempty"`
}

// Request is a structured human-input artifact embedded in a tool result.
type Request struct {
	Version           int               `json:"version"`
	Kind              string            `json:"kind"`
	Source            string            `json:"source"`
	RequestID         string            `json:"request_id"`
	ToolCallID        string            `json:"tool_call_id,omitempty"`
	ClarificationType ClarificationType `json:"clarification_type,omitempty"`
	Question          string            `json:"question"`
	Context           *string           `json:"context,omitempty"`
	InputMode         InputMode         `json:"input_mode"`
	Options           []Option          `json:"options,omitempty"`
	Fields            []Field           `json:"fields,omitempty"`
}

// Response is the v1 metadata attached to a user's answer message.
type Response struct {
	Version      int    `json:"version"`
	Kind         string `json:"kind"`
	Source       string `json:"source"`
	RequestID    string `json:"request_id"`
	ResponseKind string `json:"response_kind"`
	OptionID     string `json:"option_id,omitempty"`
	Value        string `json:"value"`
}

type toolOutput struct {
	Content    string   `json:"content"`
	Artifact   artifact `json:"artifact"`
	HumanInput Request  `json:"human_input"`
}

type artifact struct {
	HumanInput Request `json:"human_input"`
}

// BuildRequest normalizes a model-produced clarification call. Structurally
// invalid forms degrade atomically to choices or free text.
func BuildRequest(call domain.ToolCall) (Request, string, error) {
	if call.Name != ToolName || strings.TrimSpace(call.ID) == "" {
		return Request{}, "", fmt.Errorf("%w: tool call identity is invalid", ErrInvalidRequest)
	}
	arguments, err := decodeObject(call.Arguments)
	if err != nil {
		return Request{}, "", fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	question := cleanText(arguments["question"])
	if question == "" || utf8.RuneCountInString(question) > maxQuestionRunes {
		return Request{}, "", fmt.Errorf("%w: question is empty or too long", ErrInvalidRequest)
	}
	clarificationType := normalizeClarificationType(arguments["clarification_type"])
	fields := normalizeFields(arguments["fields"])
	options := normalizeOptions(arguments["options"])
	mode, version := FreeText, 1
	if len(fields) > 0 {
		mode, version = Form, 2
		options = nil
	} else if len(options) > 0 {
		mode = ChoiceWithOther
	}
	request := Request{
		Version: version, Kind: "human_input_request", Source: ToolName,
		RequestID: stableRequestID(call.ID, question), ToolCallID: call.ID,
		ClarificationType: clarificationType, Question: question,
		InputMode: mode, Options: options, Fields: fields,
	}
	if raw, exists := arguments["context"]; exists && raw != nil {
		contextText := cleanText(raw)
		if utf8.RuneCountInString(contextText) > maxContextRunes {
			return Request{}, "", fmt.Errorf("%w: context is too long", ErrInvalidRequest)
		}
		request.Context = &contextText
	}
	if err = request.Validate(); err != nil {
		return Request{}, "", err
	}
	return request, formatRequest(request), nil
}

// MarshalToolOutput creates both DeerFlow's artifact shape and a direct copy
// for clients consuming Gofer's normalized tool result.
func MarshalToolOutput(request Request, fallback string) (json.RawMessage, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(toolOutput{
		Content: fallback, Artifact: artifact{HumanInput: request}, HumanInput: request,
	})
	return json.RawMessage(data), err
}

// RequestFromOutput reads a request from either supported tool-output location.
func RequestFromOutput(output json.RawMessage) (Request, bool) {
	var value struct {
		Artifact   artifact `json:"artifact"`
		HumanInput Request  `json:"human_input"`
	}
	if json.Unmarshal(output, &value) != nil {
		return Request{}, false
	}
	request := value.HumanInput
	if request.RequestID == "" {
		request = value.Artifact.HumanInput
	}
	return request, request.Validate() == nil
}

// ParseResponse validates and canonicalizes a response payload.
func ParseResponse(raw json.RawMessage) (Response, error) {
	if len(raw) == 0 || !json.Valid(raw) {
		return Response{}, fmt.Errorf("%w: response must be valid JSON", ErrInvalidResponse)
	}
	var response Response
	if err := strictDecode(raw, &response); err != nil {
		return Response{}, fmt.Errorf("%w: %w", ErrInvalidResponse, err)
	}
	response.Source = strings.TrimSpace(response.Source)
	response.RequestID = strings.TrimSpace(response.RequestID)
	response.OptionID = strings.TrimSpace(response.OptionID)
	response.Value = strings.TrimSpace(response.Value)
	if response.Version != 1 || response.Kind != "human_input_response" || response.Source == "" ||
		response.RequestID == "" || utf8.RuneCountInString(response.RequestID) > maxRequestIDRunes ||
		response.Value == "" || utf8.RuneCountInString(response.Value) > maxResponseRunes {
		return Response{}, ErrInvalidResponse
	}
	switch response.ResponseKind {
	case "text":
		if response.OptionID != "" {
			return Response{}, ErrInvalidResponse
		}
	case "option":
		if response.OptionID == "" {
			return Response{}, ErrInvalidResponse
		}
	default:
		return Response{}, ErrInvalidResponse
	}
	return response, nil
}

// ResponseMetadata returns the canonical JSON string stored on a message.
func ResponseMetadata(response Response) (string, error) {
	data, err := json.Marshal(response)
	if err != nil {
		return "", err
	}
	canonical, err := ParseResponse(data)
	if err != nil {
		return "", err
	}
	data, err = json.Marshal(canonical)
	return string(data), err
}

// ResponseFromMessage reads a canonical structured answer from user metadata.
func ResponseFromMessage(message domain.Message) (Response, bool) {
	if message.Role != domain.RoleUser || message.Metadata == nil {
		return Response{}, false
	}
	response, err := ParseResponse(json.RawMessage(message.Metadata[ResponseMetadataKey]))
	return response, err == nil
}

// Hidden reports whether a message is an internal, non-visible carrier.
func Hidden(message domain.Message) bool {
	return strings.EqualFold(strings.TrimSpace(message.Metadata[HideFromUIKey]), "true")
}

// Validate enforces the client-visible request contract.
func (request Request) Validate() error {
	if err := request.validateHeader(); err != nil {
		return err
	}
	if (request.Version == 2) != (request.InputMode == Form) || request.Version != 1 && request.Version != 2 {
		return ErrInvalidRequest
	}
	if request.InputMode == Form {
		return request.validateFormMode()
	}
	return request.validateLegacyMode()
}

func (request Request) validateHeader() error {
	if request.Kind != "human_input_request" || request.Source == "" || request.RequestID == "" {
		return ErrInvalidRequest
	}
	if utf8.RuneCountInString(request.RequestID) > maxRequestIDRunes || strings.TrimSpace(request.Question) == "" {
		return ErrInvalidRequest
	}
	if utf8.RuneCountInString(request.Question) > maxQuestionRunes {
		return ErrInvalidRequest
	}
	if request.Context != nil && utf8.RuneCountInString(*request.Context) > maxContextRunes {
		return ErrInvalidRequest
	}
	if normalizeClarificationType(string(request.ClarificationType)) != request.ClarificationType {
		return ErrInvalidRequest
	}
	return nil
}

func (request Request) validateFormMode() error {
	if len(request.Fields) == 0 || len(request.Options) != 0 {
		return ErrInvalidRequest
	}
	return validateFields(request.Fields)
}

func (request Request) validateLegacyMode() error {
	if request.InputMode != FreeText && request.InputMode != SingleChoice && request.InputMode != ChoiceWithOther {
		return ErrInvalidRequest
	}
	if request.InputMode != FreeText && len(request.Options) == 0 || len(request.Options) > maxOptions {
		return ErrInvalidRequest
	}
	return validateOptions(request.Options)
}

func decodeObject(raw json.RawMessage) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil || value == nil {
		return nil, errors.New("arguments must be an object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("arguments contain trailing data")
	}
	return value, nil
}

func strictDecode(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("multiple JSON values")
	}
	return nil
}

func normalizeClarificationType(value any) ClarificationType {
	typed := ClarificationType(cleanText(value))
	switch typed {
	case MissingInfo, AmbiguousRequirement, ApproachChoice, RiskConfirmation, Suggestion:
		return typed
	default:
		return MissingInfo
	}
}

func normalizeOptions(value any) []Option {
	options, _ := normalizeOptionsChecked(value)
	return options
}

func normalizeOptionsChecked(value any) ([]Option, bool) {
	values, valid := optionValues(value)
	if !valid || len(values) > maxOptions {
		return nil, false
	}
	options := make([]Option, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		text := strings.TrimSpace(xmlTagPattern.ReplaceAllString(fmt.Sprint(value), ""))
		if text == "" {
			continue
		}
		if utf8.RuneCountInString(text) > maxFieldTextRunes {
			return nil, false
		}
		if _, exists := seen[text]; exists {
			continue
		}
		seen[text] = struct{}{}
		index := len(options) + 1
		options = append(options, Option{ID: fmt.Sprintf("option-%d", index), Label: text, Value: text})
	}
	return options, true
}

func optionValues(value any) ([]any, bool) {
	if value == nil {
		return nil, true
	}
	if text, ok := value.(string); ok {
		var decoded any
		if json.Unmarshal([]byte(text), &decoded) == nil {
			return optionValues(decoded)
		}
		return []any{text}, true
	}
	switch typed := value.(type) {
	case []any:
		return typed, true
	case map[string]any:
		flattened := make([]any, 0)
		flattenScalars(typed, &flattened)
		return flattened, true
	case json.Number, float64, bool:
		return []any{typed}, true
	default:
		return nil, false
	}
}

func flattenScalars(value any, output *[]any) {
	switch typed := value.(type) {
	case map[string]any:
		for _, nested := range typed {
			flattenScalars(nested, output)
		}
	case []any:
		for _, nested := range typed {
			flattenScalars(nested, output)
		}
	case string, json.Number, float64:
		*output = append(*output, typed)
	}
}

func normalizeFields(value any) []Field {
	entries, ok := decodeFieldEntries(value)
	if !ok {
		return nil
	}
	fields := make([]Field, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, raw := range entries {
		field, valid := normalizeField(raw, seen)
		if !valid {
			return nil
		}
		fields = append(fields, field)
	}
	data, _ := json.Marshal(fields)
	if len(data) > maxFieldsJSONBytes {
		return nil
	}
	return fields
}

func decodeFieldEntries(value any) ([]any, bool) {
	if text, ok := value.(string); ok {
		if json.Unmarshal([]byte(text), &value) != nil {
			return nil, false
		}
	}
	entries, ok := value.([]any)
	if !ok || len(entries) == 0 || len(entries) > maxFields {
		return nil, false
	}
	return entries, true
}

func normalizeField(raw any, seen map[string]struct{}) (Field, bool) {
	entry, valid := raw.(map[string]any)
	if !valid {
		return Field{}, false
	}
	name := cleanText(entry["name"])
	if name == "" || tooLong(name) {
		return Field{}, false
	}
	if _, reserved := reservedNames[name]; reserved {
		return Field{}, false
	}
	if _, duplicate := seen[name]; duplicate {
		return Field{}, false
	}
	seen[name] = struct{}{}
	label := cleanText(entry["label"])
	if label == "" {
		label = name
	}
	placeholder := cleanText(entry["placeholder"])
	if tooLong(label) || tooLong(placeholder) {
		return Field{}, false
	}
	fieldType, options, valid := normalizeFieldOptions(name, entry)
	if !valid {
		return Field{}, false
	}
	return Field{
		Name: name, Label: label, Type: fieldType, Required: normalizeBool(entry["required"]),
		Placeholder: placeholder, Options: options,
	}, true
}

func normalizeFieldOptions(name string, entry map[string]any) (FieldType, []Option, bool) {
	fieldType := normalizeFieldType(entry["type"])
	if fieldType != FieldSelect && fieldType != FieldMultiSelect {
		return fieldType, nil, true
	}
	options, valid := normalizeOptionsChecked(entry["options"])
	if !valid {
		return "", nil, false
	}
	if len(options) == 0 {
		return FieldText, nil, true
	}
	for index := range options {
		options[index].ID = fmt.Sprintf("%s-option-%d", name, index+1)
	}
	return fieldType, options, true
}

func normalizeFieldType(value any) FieldType {
	typed := FieldType(cleanText(value))
	switch typed {
	case FieldText, FieldTextarea, FieldNumber, FieldSelect, FieldMultiSelect, FieldCheckbox, FieldDate:
		return typed
	default:
		return FieldText
	}
}

func normalizeBool(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case json.Number:
		return typed.String() != "0"
	case float64:
		return typed != 0
	case string:
		return strings.EqualFold(strings.TrimSpace(typed), "true")
	default:
		return false
	}
}

func validateFields(fields []Field) error {
	if len(fields) == 0 || len(fields) > maxFields {
		return ErrInvalidRequest
	}
	seen := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		if field.Name == "" || field.Label == "" || tooLong(field.Name) || tooLong(field.Label) || tooLong(field.Placeholder) {
			return ErrInvalidRequest
		}
		if _, exists := seen[field.Name]; exists {
			return ErrInvalidRequest
		}
		seen[field.Name] = struct{}{}
		if _, exists := reservedNames[field.Name]; exists {
			return ErrInvalidRequest
		}
		if normalizeFieldType(string(field.Type)) != field.Type {
			return ErrInvalidRequest
		}
		if (field.Type == FieldSelect || field.Type == FieldMultiSelect) != (len(field.Options) > 0) {
			return ErrInvalidRequest
		}
		if err := validateOptions(field.Options); err != nil {
			return err
		}
	}
	data, _ := json.Marshal(fields)
	if len(data) > maxFieldsJSONBytes {
		return ErrInvalidRequest
	}
	return nil
}

func validateOptions(options []Option) error {
	seenIDs, seenValues := make(map[string]struct{}, len(options)), make(map[string]struct{}, len(options))
	for _, option := range options {
		if option.ID == "" || option.Label == "" || option.Value == "" || tooLong(option.Label) || tooLong(option.Value) {
			return ErrInvalidRequest
		}
		if _, exists := seenIDs[option.ID]; exists {
			return ErrInvalidRequest
		}
		if _, exists := seenValues[option.Value]; exists {
			return ErrInvalidRequest
		}
		seenIDs[option.ID], seenValues[option.Value] = struct{}{}, struct{}{}
	}
	return nil
}

func stableRequestID(callID, question string) string {
	requestID := "clarification:" + strings.TrimSpace(callID)
	if utf8.RuneCountInString(requestID) <= maxRequestIDRunes {
		return requestID
	}
	digest := sha256.Sum256([]byte(callID + "\x00" + question))
	return "clarification:" + hex.EncodeToString(digest[:8])
}

func formatRequest(request Request) string {
	parts := []string{formatRequestHeading(request)}
	if len(request.Fields) > 0 {
		return strings.Join(append(parts, formatFieldLines(request.Fields)...), "\n")
	}
	if len(request.Options) > 0 {
		parts = append(parts, "")
		for index, option := range request.Options {
			parts = append(parts, fmt.Sprintf("  %d. %s", index+1, option.Label))
		}
	}
	return strings.Join(parts, "\n")
}

func formatRequestHeading(request Request) string {
	icons := map[ClarificationType]string{
		MissingInfo: "❓", AmbiguousRequirement: "🤔", ApproachChoice: "🔀",
		RiskConfirmation: "⚠️", Suggestion: "💡",
	}
	if request.Context != nil && *request.Context != "" {
		return icons[request.ClarificationType] + " " + *request.Context + "\n\n" + request.Question
	}
	return icons[request.ClarificationType] + " " + request.Question
}

func formatFieldLines(fields []Field) []string {
	lines := make([]string, 1, len(fields)+3)
	for index, field := range fields {
		lines = append(lines, formatFieldLine(index+1, field))
	}
	return append(lines, "", "Please reply with a value for each field.")
}

func formatFieldLine(index int, field Field) string {
	line := fmt.Sprintf("  %d. %s", index, field.Label)
	if field.Required {
		line += " (required)"
	}
	if len(field.Options) == 0 {
		return line
	}
	labels := make([]string, len(field.Options))
	for optionIndex := range field.Options {
		labels[optionIndex] = field.Options[optionIndex].Label
	}
	line += " — options: " + strings.Join(labels, " / ")
	if field.Type == FieldMultiSelect {
		line += " (multiple allowed)"
	}
	return line
}

func cleanText(value any) string {
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(text)
}

func tooLong(value string) bool { return utf8.RuneCountInString(value) > maxFieldTextRunes }
