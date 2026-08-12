package tooloutput

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/model"
	"github.com/Rememorio/gofer/internal/runtime"
	"github.com/Rememorio/gofer/internal/workspace"
)

const (
	// DefaultStorageSubdir is excluded from workspace review and artifact
	// delivery so internal context spill files never look like user outputs.
	DefaultStorageSubdir = workspace.ProcessOutputDirectory
	maxCreateAttempts    = 3
)

// ErrInvalidConfig identifies an unusable tool-output budget configuration.
var ErrInvalidConfig = errors.New("invalid tool-output budget configuration")

// Config controls externalization, inline previews, and storage-failure
// fallback truncation. All limits count Unicode characters, not bytes.
type Config struct {
	Enabled             bool
	ExternalizeMinChars int
	PreviewHeadChars    int
	PreviewTailChars    int
	FallbackMaxChars    int
	FallbackHeadChars   int
	FallbackTailChars   int
	StorageSubdir       string
	ExemptTools         []string
	ToolOverrides       map[string]int
}

// DefaultConfig returns DeerFlow-compatible result-budget defaults.
func DefaultConfig() Config {
	return Config{
		Enabled: true, ExternalizeMinChars: 12_000,
		PreviewHeadChars: 2_000, PreviewTailChars: 1_000,
		FallbackMaxChars: 30_000, FallbackHeadChars: 8_000, FallbackTailChars: 3_000,
		StorageSubdir: DefaultStorageSubdir, ExemptTools: []string{"read_file", "read_file_tool"},
		ToolOverrides: make(map[string]int),
	}
}

// Middleware externalizes oversized results before they are journaled or sent
// to another model turn. It also bounds oversized legacy results in history.
type Middleware struct {
	runtime.NopMiddleware
	config    Config
	workspace *workspace.Thread
	exempt    map[string]struct{}
}

var (
	_ runtime.Middleware            = (*Middleware)(nil)
	_ runtime.ToolResultTransformer = (*Middleware)(nil)
)

// New validates config and binds a thread-scoped result store.
func New(config Config, thread *workspace.Thread) (*Middleware, error) {
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	if thread == nil {
		return nil, fmt.Errorf("%w: workspace is required", ErrInvalidConfig)
	}
	config.ExemptTools = append([]string(nil), config.ExemptTools...)
	config.ToolOverrides = cloneOverrides(config.ToolOverrides)
	exempt := make(map[string]struct{}, len(config.ExemptTools))
	for _, name := range config.ExemptTools {
		exempt[name] = struct{}{}
	}
	return &Middleware{config: config, workspace: thread, exempt: exempt}, nil
}

// TransformToolResult applies the configured budget to a newly returned tool
// result. Externalization failure is non-fatal and falls back to bounded
// inline text when that limit is enabled.
func (middleware *Middleware) TransformToolResult(ctx context.Context, call domain.ToolCall, result domain.ToolResult) (domain.ToolResult, error) {
	if middleware == nil || middleware.workspace == nil {
		return domain.ToolResult{}, fmt.Errorf("%w: middleware is not initialized", ErrInvalidConfig)
	}
	if err := ctx.Err(); err != nil {
		return domain.ToolResult{}, err
	}
	if !middleware.config.Enabled || middleware.isExempt(call.Name) {
		return result, nil
	}
	text, ok := resultText(result.Output)
	if !ok {
		return result, nil
	}
	characters := utf8.RuneCountInString(text)
	threshold := middleware.externalizeThreshold(call.Name)
	if threshold > 0 && characters > threshold {
		virtualPath, err := middleware.externalize(call.Name, text)
		if err == nil {
			preview := RenderPreview(text, call.Name, virtualPath, middleware.config.PreviewHeadChars, middleware.config.PreviewTailChars)
			return replaceOutput(result, preview), nil
		}
	}
	if middleware.config.FallbackMaxChars > 0 && characters > middleware.config.FallbackMaxChars {
		fallback := truncateFallback(text, call.Name, middleware.config.FallbackMaxChars,
			middleware.config.FallbackHeadChars, middleware.config.FallbackTailChars)
		return replaceOutput(result, fallback), nil
	}
	return result, nil
}

// BeforeModel bounds oversized historical tool results that predate result
// externalization. It never creates new spill files during request replay.
func (middleware *Middleware) BeforeModel(ctx context.Context, request *model.Request) error {
	if middleware == nil || request == nil || middleware.workspace == nil {
		return fmt.Errorf("%w: middleware and request are required", ErrInvalidConfig)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !middleware.config.Enabled || middleware.config.FallbackMaxChars <= 0 {
		return nil
	}
	toolNames := make(map[string]string)
	for messageIndex := range request.Messages {
		message, changed := middleware.boundHistoricalMessage(request.Messages[messageIndex], toolNames)
		if changed {
			request.Messages[messageIndex] = message
		}
	}
	return nil
}

func (middleware *Middleware) boundHistoricalMessage(message domain.Message, toolNames map[string]string) (domain.Message, bool) {
	changed := false
	for contentIndex, content := range message.Content {
		if content.Kind == domain.ContentToolCall && content.ToolCall != nil {
			toolNames[content.ToolCall.ID] = content.ToolCall.Name
			continue
		}
		replacement, ok := middleware.boundHistoricalResult(content, toolNames)
		if !ok {
			continue
		}
		if !changed {
			message.Content = append([]domain.Content(nil), message.Content...)
			changed = true
		}
		message.Content[contentIndex].ToolResult = replacement
	}
	return message, changed
}

func (middleware *Middleware) boundHistoricalResult(content domain.Content, toolNames map[string]string) (*domain.ToolResult, bool) {
	if content.Kind != domain.ContentToolResult || content.ToolResult == nil {
		return nil, false
	}
	name := toolNames[content.ToolResult.CallID]
	if middleware.isExempt(name) {
		return nil, false
	}
	text, ok := resultText(content.ToolResult.Output)
	if !ok || utf8.RuneCountInString(text) <= middleware.config.FallbackMaxChars {
		return nil, false
	}
	fallback := truncateFallback(text, displayToolName(name), middleware.config.FallbackMaxChars,
		middleware.config.FallbackHeadChars, middleware.config.FallbackTailChars)
	copyResult := *content.ToolResult
	copyResult.Output = marshalText(fallback)
	return &copyResult, true
}

func (middleware *Middleware) externalize(toolName, content string) (string, error) {
	for range maxCreateAttempts {
		suffix, err := randomSuffix()
		if err != nil {
			return "", err
		}
		filename := sanitizeToolName(toolName) + "-" + suffix + extension(toolName)
		virtualPath := workspace.OutputsRoot + "/" + middleware.config.StorageSubdir + "/" + filename
		if err = middleware.workspace.CreateOutput(virtualPath, []byte(content)); err == nil {
			return virtualPath, nil
		} else if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	}
	return "", fs.ErrExist
}

func (middleware *Middleware) externalizeThreshold(toolName string) int {
	if threshold, exists := middleware.config.ToolOverrides[toolName]; exists {
		return threshold
	}
	return middleware.config.ExternalizeMinChars
}

func (middleware *Middleware) isExempt(toolName string) bool {
	_, exempt := middleware.exempt[toolName]
	return exempt
}

func validateConfig(config Config) error {
	if config.ExternalizeMinChars < 0 || config.PreviewHeadChars < 0 || config.PreviewTailChars < 0 ||
		config.FallbackMaxChars < 0 || config.FallbackHeadChars < 0 || config.FallbackTailChars < 0 {
		return fmt.Errorf("%w: limits cannot be negative", ErrInvalidConfig)
	}
	if !singleSegment(config.StorageSubdir) {
		return fmt.Errorf("%w: storage subdirectory must be one directory name", ErrInvalidConfig)
	}
	seen := make(map[string]struct{}, len(config.ExemptTools))
	for _, name := range config.ExemptTools {
		if strings.TrimSpace(name) != name || name == "" {
			return fmt.Errorf("%w: invalid exempt tool name", ErrInvalidConfig)
		}
		if _, exists := seen[name]; exists {
			return fmt.Errorf("%w: duplicate exempt tool %q", ErrInvalidConfig, name)
		}
		seen[name] = struct{}{}
	}
	for name, threshold := range config.ToolOverrides {
		if strings.TrimSpace(name) != name || name == "" || threshold < 0 {
			return fmt.Errorf("%w: invalid tool override", ErrInvalidConfig)
		}
	}
	return nil
}

func singleSegment(value string) bool {
	return value != "" && value != "." && value != ".." && !strings.ContainsAny(value, "/\\\x00") && path.Base(value) == value
}

func cloneOverrides(source map[string]int) map[string]int {
	cloned := make(map[string]int, len(source))
	for name, threshold := range source {
		cloned[name] = threshold
	}
	return cloned
}

func resultText(output json.RawMessage) (string, bool) {
	raw := string(output)
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || !json.Valid([]byte(trimmed)) {
		return "", false
	}
	if strings.HasPrefix(trimmed, `"`) {
		var text string
		if json.Unmarshal([]byte(trimmed), &text) == nil {
			return text, true
		}
	}
	return raw, true
}

func replaceOutput(result domain.ToolResult, text string) domain.ToolResult {
	result.Output = marshalText(text)
	return result
}

func marshalText(text string) json.RawMessage {
	encoded, _ := json.Marshal(text)
	return encoded
}

func randomSuffix() (string, error) {
	var random [6]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate tool result filename: %w", err)
	}
	return hex.EncodeToString(random[:]), nil
}

func sanitizeToolName(name string) string {
	name = strings.ReplaceAll(name, "..", "")
	var safe strings.Builder
	for _, character := range name {
		if unicode.IsLetter(character) || unicode.IsDigit(character) || character == '-' || character == '_' {
			safe.WriteRune(character)
		}
	}
	if safe.Len() == 0 {
		return "unknown"
	}
	return safe.String()
}

func extension(toolName string) string {
	if toolName == "bash" || toolName == "bash_tool" || toolName == "web_fetch" {
		return ".log"
	}
	return ".txt"
}

func displayToolName(name string) string {
	if name == "" {
		return "unknown"
	}
	return name
}

func truncateFallback(content, toolName string, maxChars, headChars, tailChars int) string {
	runes := []rune(content)
	if maxChars <= 0 || len(runes) <= maxChars {
		return content
	}
	marker := fmt.Sprintf("\n\n[... %d chars omitted from %s output. Persistent storage unavailable. Consider narrowing the query or using more specific parameters.]\n\n", len(runes), toolName)
	markerRunes := []rune(marker)
	if len(markerRunes) >= maxChars {
		return string(runes[:maxChars])
	}
	budget := maxChars - len(markerRunes)
	headBudget := min(headChars, budget)
	tailBudget := min(tailChars, max(0, budget-headBudget))
	headEnd := snapHeadBoundary(runes, headBudget)
	tailStart := snapTailBoundary(runes, max(headEnd, len(runes)-tailBudget))
	omitted := len(runes) - headEnd - (len(runes) - tailStart)
	marker = fmt.Sprintf("\n\n[... %d chars omitted from %s output. Persistent storage unavailable. Consider narrowing the query or using more specific parameters.]\n\n", omitted, toolName)
	result := append([]rune(nil), runes[:headEnd]...)
	result = append(result, []rune(marker)...)
	result = append(result, runes[tailStart:]...)
	if len(result) > maxChars {
		result = result[:maxChars]
	}
	return string(result)
}

func snapHeadBoundary(content []rune, position int) int {
	if position <= 0 || position >= len(content) {
		return position
	}
	for index := position - 1; index >= position/2; index-- {
		if content[index] == '\n' {
			return index + 1
		}
	}
	return position
}

func snapTailBoundary(content []rune, position int) int {
	if position <= 0 || position >= len(content) {
		return position
	}
	end := position + (len(content)-position)/2
	for index := position; index < end; index++ {
		if content[index] == '\n' {
			return index + 1
		}
	}
	return position
}
