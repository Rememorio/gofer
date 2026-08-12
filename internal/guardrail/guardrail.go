package guardrail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/model"
	"github.com/Rememorio/gofer/internal/runtime"
)

const (
	userInputBegin = "--- BEGIN USER INPUT ---"
	userInputEnd   = "--- END USER INPUT ---"
	neutralBegin   = "[BEGIN USER INPUT]"
	neutralEnd     = "[END USER INPUT]"
	maxJSONDepth   = 128
)

var (
	// ErrInvalidConfig identifies malformed guardrail dependencies or tool names.
	ErrInvalidConfig = errors.New("invalid guardrail configuration")
	// ErrUnsafeContent identifies remote content that cannot be sanitized without
	// losing information or exceeding the guardrail's bounded traversal.
	ErrUnsafeContent = errors.New("unsafe remote content")
	blockedTagNames  = []string{
		"analysis", "available-deferred-tools", "available_skills", "browser_snapshot",
		"citations", "clarification_system", "conversation_summary", "critical_reminders",
		"current_date", "current_uploads", "disabled_skills", "durable_context_data",
		"file_editing_workflow", "goal_continuation", "guidelines", "ignore", "important",
		"instruction", "mcp_routing_hints", "memory", "memory_tool_system", "output_format",
		"override", "prompt", "recalled_memory", "response_style", "role", "self_update",
		"skill_index", "skill_system", "slash_skill_activation", "soul", "subagent_system",
		"system", "system-reminder", "system_reminder", "think", "thinking_style",
		"todo_list_system", "tool_restrictions", "uploaded_files", "working_directory",
	}
	blockedTagPattern = compileBlockedTagPattern(blockedTagNames)
)

// Config selects tool results that originate outside Gofer's local trust
// boundary. User input protection is always active.
type Config struct {
	RemoteTools []string
}

// DefaultConfig returns first-party DeerFlow remote-content tool names.
func DefaultConfig() Config {
	return Config{RemoteTools: []string{"image_search", "web_capture", "web_fetch", "web_search"}}
}

// Middleware sanitizes the last user message before each model call and
// selected remote tool results before persistence or context budgeting.
type Middleware struct {
	runtime.NopMiddleware
	remoteTools map[string]struct{}
}

var (
	_ runtime.Middleware            = (*Middleware)(nil)
	_ runtime.ToolResultTransformer = (*Middleware)(nil)
)

// New validates and copies one guardrail configuration.
func New(config Config) (*Middleware, error) {
	remoteTools := make(map[string]struct{}, len(config.RemoteTools))
	for _, name := range config.RemoteTools {
		if strings.TrimSpace(name) != name || name == "" {
			return nil, fmt.Errorf("%w: remote tool names cannot be blank", ErrInvalidConfig)
		}
		remoteTools[name] = struct{}{}
	}
	return &Middleware{remoteTools: remoteTools}, nil
}

// BeforeModel applies temporary, idempotent protection to historical remote
// results and the most recent user message. The durable conversation remains
// byte-for-byte unchanged.
func (middleware *Middleware) BeforeModel(ctx context.Context, request *model.Request) error {
	if middleware == nil || request == nil || middleware.remoteTools == nil {
		return fmt.Errorf("%w: middleware and request are required", ErrInvalidConfig)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := middleware.sanitizeHistoricalToolResults(request); err != nil {
		return err
	}
	middleware.sanitizeLastUserMessage(request)
	return nil
}

// TransformToolResult neutralizes string values returned by remote-content
// tools while preserving the surrounding JSON shape and result status.
func (middleware *Middleware) TransformToolResult(ctx context.Context, call domain.ToolCall, result domain.ToolResult) (domain.ToolResult, error) {
	if middleware == nil || middleware.remoteTools == nil {
		return domain.ToolResult{}, fmt.Errorf("%w: middleware is required", ErrInvalidConfig)
	}
	if err := ctx.Err(); err != nil {
		return domain.ToolResult{}, err
	}
	if _, sanitize := middleware.remoteTools[call.Name]; !sanitize {
		return result, nil
	}
	output, changed, err := sanitizeJSON(result.Output)
	if err != nil {
		return domain.ToolResult{}, fmt.Errorf("sanitize %s result: %w", call.Name, err)
	}
	if changed {
		result.Output = output
	}
	return result, nil
}

// NeutralizeUntrustedText escapes framework-authority tags and neutralizes
// user-boundary tokens while preserving all other text.
func NeutralizeUntrustedText(text string) string {
	if strings.TrimSpace(text) == "" {
		return text
	}
	text = blockedTagPattern.ReplaceAllStringFunc(text, escapeTag)
	return neutralizeBoundaryTokens(text)
}

func wrapUserInput(text string) string {
	if strings.TrimSpace(text) == "" {
		return text
	}
	text = blockedTagPattern.ReplaceAllStringFunc(text, escapeTag)
	if strings.HasPrefix(text, userInputBegin) && strings.HasSuffix(text, userInputEnd) {
		inner := text[len(userInputBegin) : len(text)-len(userInputEnd)]
		neutralized := neutralizeBoundaryTokens(inner)
		if neutralized == inner {
			return text
		}
		return userInputBegin + neutralized + userInputEnd
	}
	text = neutralizeBoundaryTokens(text)
	return userInputBegin + "\n" + text + "\n" + userInputEnd
}

func (middleware *Middleware) sanitizeHistoricalToolResults(request *model.Request) error {
	toolNames := make(map[string]string)
	for messageIndex := range request.Messages {
		message, changed, err := middleware.sanitizeHistoricalMessage(request.Messages[messageIndex], toolNames)
		if err != nil {
			return err
		}
		if changed {
			request.Messages[messageIndex] = message
		}
	}
	return nil
}

func (middleware *Middleware) sanitizeHistoricalMessage(message domain.Message, toolNames map[string]string) (domain.Message, bool, error) {
	changed := false
	for contentIndex, content := range message.Content {
		if rememberToolName(content, toolNames) {
			continue
		}
		output, sanitized, name, err := middleware.sanitizeHistoricalResult(content, toolNames)
		if err != nil {
			return domain.Message{}, false, fmt.Errorf("sanitize historical %s result: %w", name, err)
		}
		if !sanitized {
			continue
		}
		if !changed {
			message.Content = append([]domain.Content(nil), message.Content...)
			changed = true
		}
		copyResult := *content.ToolResult
		copyResult.Output = output
		message.Content[contentIndex].ToolResult = &copyResult
	}
	return message, changed, nil
}

func (middleware *Middleware) sanitizeHistoricalResult(content domain.Content, toolNames map[string]string) (json.RawMessage, bool, string, error) {
	if content.ToolResult == nil {
		return nil, false, "", nil
	}
	name := toolNames[resultCallID(content)]
	if _, remote := middleware.remoteTools[name]; !remote {
		return nil, false, name, nil
	}
	output, changed, err := sanitizeJSON(content.ToolResult.Output)
	return output, changed, name, err
}

func rememberToolName(content domain.Content, toolNames map[string]string) bool {
	if content.Kind != domain.ContentToolCall || content.ToolCall == nil {
		return false
	}
	toolNames[content.ToolCall.ID] = content.ToolCall.Name
	return true
}

func (middleware *Middleware) sanitizeLastUserMessage(request *model.Request) {
	for index := len(request.Messages) - 1; index >= 0; index-- {
		if request.Messages[index].Role != domain.RoleUser {
			continue
		}
		message, changed := sanitizeUserMessage(request.Messages[index])
		if changed {
			request.Messages[index] = message
		}
		return
	}
}

func sanitizeUserMessage(message domain.Message) (domain.Message, bool) {
	textIndexes := make([]int, 0, len(message.Content))
	textParts := make([]string, 0, len(message.Content))
	for index, content := range message.Content {
		if content.Kind == domain.ContentText {
			textIndexes = append(textIndexes, index)
			textParts = append(textParts, content.Text)
		}
	}
	if len(textIndexes) == 0 {
		return message, false
	}
	merged := strings.Join(textParts, "\n")
	processed := wrapUserInput(merged)
	if processed == merged {
		return message, false
	}
	first, last := textIndexes[0], textIndexes[len(textIndexes)-1]
	content := make([]domain.Content, 0, len(message.Content)-len(textIndexes)+1)
	content = append(content, message.Content[:first]...)
	content = append(content, domain.Content{Kind: domain.ContentText, Text: processed})
	for index := first + 1; index <= last; index++ {
		if message.Content[index].Kind != domain.ContentText {
			content = append(content, message.Content[index])
		}
	}
	content = append(content, message.Content[last+1:]...)
	message.Content = content
	return message, true
}

func resultCallID(content domain.Content) string {
	if content.Kind != domain.ContentToolResult || content.ToolResult == nil {
		return ""
	}
	return content.ToolResult.CallID
}

func sanitizeJSON(output json.RawMessage) (json.RawMessage, bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, false, fmt.Errorf("%w: invalid JSON: %w", ErrUnsafeContent, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, false, fmt.Errorf("%w: multiple JSON values", ErrUnsafeContent)
		}
		return nil, false, fmt.Errorf("%w: invalid trailing JSON: %w", ErrUnsafeContent, err)
	}
	sanitized, changed, err := sanitizeValue(value, 0)
	if err != nil || !changed {
		return output, changed, err
	}
	encoded, err := json.Marshal(sanitized)
	if err != nil {
		return nil, false, err
	}
	return encoded, true, nil
}

func sanitizeValue(value any, depth int) (any, bool, error) {
	if depth > maxJSONDepth {
		return nil, false, fmt.Errorf("%w: result exceeds maximum JSON depth", ErrUnsafeContent)
	}
	switch typed := value.(type) {
	case string:
		sanitized := NeutralizeUntrustedText(typed)
		return sanitized, sanitized != typed, nil
	case []any:
		return sanitizeSlice(typed, depth)
	case map[string]any:
		return sanitizeMap(typed, depth)
	case nil, bool, json.Number:
		return value, false, nil
	default:
		return nil, false, fmt.Errorf("%w: unsupported JSON value %T", ErrUnsafeContent, value)
	}
}

func sanitizeSlice(values []any, depth int) ([]any, bool, error) {
	sanitized := make([]any, len(values))
	changed := false
	for index, value := range values {
		next, itemChanged, err := sanitizeValue(value, depth+1)
		if err != nil {
			return nil, false, err
		}
		sanitized[index] = next
		changed = changed || itemChanged
	}
	return sanitized, changed, nil
}

func sanitizeMap(values map[string]any, depth int) (map[string]any, bool, error) {
	sanitized := make(map[string]any, len(values))
	changed := false
	for key, value := range values {
		sanitizedKey := NeutralizeUntrustedText(key)
		if _, exists := sanitized[sanitizedKey]; exists {
			return nil, false, fmt.Errorf("%w: sanitizing object keys creates duplicate %q", ErrUnsafeContent, sanitizedKey)
		}
		next, itemChanged, err := sanitizeValue(value, depth+1)
		if err != nil {
			return nil, false, err
		}
		sanitized[sanitizedKey] = next
		changed = changed || sanitizedKey != key || itemChanged
	}
	return sanitized, changed, nil
}

func compileBlockedTagPattern(names []string) *regexp.Regexp {
	copied := append([]string(nil), names...)
	sort.Strings(copied)
	for index, name := range copied {
		copied[index] = regexp.QuoteMeta(name)
	}
	return regexp.MustCompile(`(?i)<[[:space:]]*/?[[:space:]]*(` + strings.Join(copied, "|") + `)\b[^>]*>?`)
}

func escapeTag(tag string) string {
	tag = strings.ReplaceAll(tag, "<", "&lt;")
	return strings.ReplaceAll(tag, ">", "&gt;")
}

func neutralizeBoundaryTokens(text string) string {
	text = strings.ReplaceAll(text, userInputBegin, neutralBegin)
	return strings.ReplaceAll(text, userInputEnd, neutralEnd)
}
