package loopdetect

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/model"
	"github.com/Rememorio/gofer/internal/runtime"
)

const (
	maxPendingWarnings = 4
	readLineBucketSize = 200
)

var (
	// ErrInvalidConfig identifies unsafe or inconsistent loop thresholds.
	ErrInvalidConfig  = errors.New("invalid loop detection configuration")
	identicalWarning  = "[LOOP DETECTED] You are repeating the same tool calls. Stop calling tools and produce your final answer now. If you cannot complete the task, summarize what you accomplished so far."
	identicalHardStop = "[FORCED STOP] Repeated tool calls exceeded the safety limit. Producing a final answer with results collected so far."
)

// FrequencyOverride replaces global same-tool thresholds for one tool name.
type FrequencyOverride struct {
	Warn      int
	HardLimit int
}

// Config bounds repeated-call and tool-frequency tracking for one run.
type Config struct {
	WarnThreshold      int
	HardLimit          int
	WindowSize         int
	ToolFrequencyWarn  int
	ToolFrequencyLimit int
	ToolOverrides      map[string]FrequencyOverride
	Now                func() time.Time
}

// DefaultConfig returns DeerFlow-compatible safety thresholds.
func DefaultConfig() Config {
	return Config{
		WarnThreshold: 3, HardLimit: 5, WindowSize: 20,
		ToolFrequencyWarn: 30, ToolFrequencyLimit: 50,
		ToolOverrides: make(map[string]FrequencyOverride), Now: time.Now,
	}
}

// Middleware maintains bounded loop state for one agent run.
type Middleware struct {
	runtime.NopMiddleware
	config          Config
	frequencyWindow int
	mu              sync.Mutex
	history         []string
	warnedHashes    map[string]struct{}
	toolHistory     []string
	toolCounts      map[string]int
	warnedTools     map[string]struct{}
	pending         []string
}

var (
	_ runtime.Middleware               = (*Middleware)(nil)
	_ runtime.ModelResponseTransformer = (*Middleware)(nil)
)

// New validates and copies one loop detector configuration.
func New(config Config) (*Middleware, error) {
	if config.Now == nil {
		config.Now = time.Now
	}
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	config.ToolOverrides = cloneOverrides(config.ToolOverrides)
	frequencyWindow := max(config.WindowSize, config.ToolFrequencyLimit)
	for _, override := range config.ToolOverrides {
		frequencyWindow = max(frequencyWindow, override.HardLimit)
	}
	return &Middleware{
		config: config, frequencyWindow: frequencyWindow,
		warnedHashes: make(map[string]struct{}), toolCounts: make(map[string]int),
		warnedTools: make(map[string]struct{}),
	}, nil
}

// BeforeModel drains queued warnings into one temporary user message after all
// tool results. The durable conversation is not modified.
func (middleware *Middleware) BeforeModel(ctx context.Context, request *model.Request) error {
	if middleware == nil || request == nil || middleware.config.Now == nil {
		return fmt.Errorf("%w: middleware and request are required", ErrInvalidConfig)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	middleware.mu.Lock()
	warnings := append([]string(nil), middleware.pending...)
	middleware.pending = nil
	middleware.mu.Unlock()
	if len(warnings) == 0 {
		return nil
	}
	message, err := domain.NewTextMessage(domain.RoleUser, strings.Join(warnings, "\n\n"), middleware.config.Now())
	if err != nil {
		return fmt.Errorf("build loop warning: %w", err)
	}
	message.Metadata = map[string]string{"internal_kind": "loop_warning"}
	request.Messages = append(append([]domain.Message(nil), request.Messages...), message)
	return nil
}

// TransformModelResponse tracks tool calls, queues warnings, and strips calls
// when a hard limit is reached. Provider usage is preserved byte-for-byte.
func (middleware *Middleware) TransformModelResponse(ctx context.Context, response model.Response) (model.Response, error) {
	if middleware == nil || middleware.config.Now == nil {
		return model.Response{}, fmt.Errorf("%w: middleware is required", ErrInvalidConfig)
	}
	if err := ctx.Err(); err != nil {
		return model.Response{}, err
	}
	if len(response.ToolCalls) == 0 {
		return response, nil
	}
	middleware.mu.Lock()
	decision := middleware.track(response.ToolCalls)
	if decision.warning != "" && !decision.hard {
		middleware.queueWarning(decision.warning)
	}
	middleware.mu.Unlock()
	if !decision.hard {
		return response, nil
	}
	response.Text = appendResponseText(response.Text, decision.warning)
	response.ToolCalls = nil
	response.StopReason = model.StopLoopCapped
	return response, nil
}

// Reset clears all bounded state, primarily for explicit runtime reuse.
func (middleware *Middleware) Reset() {
	if middleware == nil {
		return
	}
	middleware.mu.Lock()
	middleware.history = nil
	middleware.toolHistory = nil
	middleware.pending = nil
	clear(middleware.warnedHashes)
	clear(middleware.toolCounts)
	clear(middleware.warnedTools)
	middleware.mu.Unlock()
}

type decision struct {
	warning string
	hard    bool
}

func (middleware *Middleware) track(calls []domain.ToolCall) decision {
	signature := callSetSignature(calls)
	middleware.history = append(middleware.history, signature)
	if len(middleware.history) > middleware.config.WindowSize {
		middleware.history = append([]string(nil), middleware.history[len(middleware.history)-middleware.config.WindowSize:]...)
	}
	middleware.pruneHashWarnings()
	repeatCount := countString(middleware.history, signature)
	middleware.recordToolNames(calls)

	if repeatCount >= middleware.config.HardLimit {
		return decision{warning: identicalHardStop, hard: true}
	}
	if name, count, hard := middleware.frequencyDecision(calls, true); hard {
		return decision{warning: frequencyHardStop(name, count), hard: true}
	}
	if repeatCount >= middleware.config.WarnThreshold {
		if _, warned := middleware.warnedHashes[signature]; !warned {
			middleware.warnedHashes[signature] = struct{}{}
			return decision{warning: identicalWarning}
		}
	}
	if name, count, warn := middleware.frequencyDecision(calls, false); warn {
		middleware.warnedTools[name] = struct{}{}
		return decision{warning: frequencyWarning(name, count)}
	}
	return decision{}
}

func (middleware *Middleware) recordToolNames(calls []domain.ToolCall) {
	for _, call := range calls {
		if call.Name == "" {
			continue
		}
		middleware.toolHistory = append(middleware.toolHistory, call.Name)
		middleware.toolCounts[call.Name]++
		for len(middleware.toolHistory) > middleware.frequencyWindow {
			oldest := middleware.toolHistory[0]
			middleware.toolHistory = middleware.toolHistory[1:]
			middleware.toolCounts[oldest]--
			if middleware.toolCounts[oldest] == 0 {
				delete(middleware.toolCounts, oldest)
			}
		}
	}
}

func (middleware *Middleware) frequencyDecision(calls []domain.ToolCall, hard bool) (string, int, bool) {
	names := uniqueSortedToolNames(calls)
	for _, name := range names {
		warnLimit, hardLimit := middleware.frequencyLimits(name)
		count := middleware.toolCounts[name]
		if hard && count >= hardLimit {
			return name, count, true
		}
		if !hard {
			if count < warnLimit {
				delete(middleware.warnedTools, name)
				continue
			}
			if _, warned := middleware.warnedTools[name]; !warned {
				return name, count, true
			}
		}
	}
	return "", 0, false
}

func (middleware *Middleware) frequencyLimits(name string) (int, int) {
	if override, ok := middleware.config.ToolOverrides[name]; ok {
		return override.Warn, override.HardLimit
	}
	return middleware.config.ToolFrequencyWarn, middleware.config.ToolFrequencyLimit
}

func (middleware *Middleware) pruneHashWarnings() {
	active := make(map[string]struct{}, len(middleware.history))
	for _, signature := range middleware.history {
		active[signature] = struct{}{}
	}
	for signature := range middleware.warnedHashes {
		if _, exists := active[signature]; !exists {
			delete(middleware.warnedHashes, signature)
		}
	}
}

func (middleware *Middleware) queueWarning(warning string) {
	for _, pending := range middleware.pending {
		if pending == warning {
			return
		}
	}
	middleware.pending = append(middleware.pending, warning)
	if len(middleware.pending) > maxPendingWarnings {
		middleware.pending = append([]string(nil), middleware.pending[len(middleware.pending)-maxPendingWarnings:]...)
	}
}

func callSetSignature(calls []domain.ToolCall) string {
	keys := make([]string, 0, len(calls))
	for _, call := range calls {
		keys = append(keys, call.Name+":"+stableToolKey(call))
	}
	sort.Strings(keys)
	encoded, _ := json.Marshal(keys)
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("%x", digest[:6])
}

func stableToolKey(call domain.ToolCall) string {
	object, fallback := normalizedArguments(call.Arguments)
	if call.Name == "read_file" && object != nil {
		return readFileKey(object)
	}
	if call.Name == "write_file" || call.Name == "str_replace" {
		return fallback
	}
	stable := make(map[string]any)
	for _, field := range []string{"path", "url", "query", "command", "pattern", "glob", "cmd"} {
		if value, exists := object[field]; exists && value != nil {
			stable[field] = value
		}
	}
	if len(stable) == 0 {
		return fallback
	}
	encoded, _ := json.Marshal(stable)
	return string(encoded)
}

func normalizedArguments(raw json.RawMessage) (map[string]any, string) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, string(raw)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, string(raw)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, string(raw)
	}
	object, _ := value.(map[string]any)
	return object, string(encoded)
}

func readFileKey(arguments map[string]any) string {
	file, _ := arguments["path"].(string)
	start := integerArgument(arguments["start_line"], 1)
	end := integerArgument(arguments["end_line"], start)
	if start > end {
		start, end = end, start
	}
	start = max(start, 1)
	end = max(end, 1)
	return fmt.Sprintf("%s:%d-%d", file, (start-1)/readLineBucketSize, (end-1)/readLineBucketSize)
}

func integerArgument(value any, fallback int) int {
	switch typed := value.(type) {
	case json.Number:
		parsed, err := strconv.Atoi(string(typed))
		if err == nil {
			return parsed
		}
	case string:
		parsed, err := strconv.Atoi(typed)
		if err == nil {
			return parsed
		}
	}
	return fallback
}

func uniqueSortedToolNames(calls []domain.ToolCall) []string {
	set := make(map[string]struct{}, len(calls))
	for _, call := range calls {
		if call.Name != "" {
			set[call.Name] = struct{}{}
		}
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func countString(values []string, want string) int {
	count := 0
	for _, value := range values {
		if value == want {
			count++
		}
	}
	return count
}

func appendResponseText(existing, addition string) string {
	if existing == "" {
		return addition
	}
	return existing + "\n\n" + addition
}

func frequencyWarning(name string, count int) string {
	return fmt.Sprintf("[LOOP DETECTED] You have called %s %d times without producing a final answer. Stop calling tools and produce your final answer now. If you cannot complete the task, summarize what you accomplished so far.", name, count)
}

func frequencyHardStop(name string, count int) string {
	return fmt.Sprintf("[FORCED STOP] Tool %s was called %d times and exceeded the per-tool safety limit. Producing a final answer with results collected so far.", name, count)
}

func cloneOverrides(source map[string]FrequencyOverride) map[string]FrequencyOverride {
	cloned := make(map[string]FrequencyOverride, len(source))
	for name, override := range source {
		cloned[name] = override
	}
	return cloned
}

func validateConfig(config Config) error {
	if config.WarnThreshold < 1 || config.HardLimit < config.WarnThreshold ||
		config.WindowSize < config.HardLimit || config.WindowSize > 10_000 {
		return fmt.Errorf("%w: invalid repetition thresholds", ErrInvalidConfig)
	}
	if config.ToolFrequencyWarn < 1 || config.ToolFrequencyLimit < config.ToolFrequencyWarn || config.ToolFrequencyLimit > 100_000 {
		return fmt.Errorf("%w: invalid frequency thresholds", ErrInvalidConfig)
	}
	for name, override := range config.ToolOverrides {
		if strings.TrimSpace(name) != name || name == "" || override.Warn < 1 ||
			override.HardLimit < override.Warn || override.HardLimit > 100_000 {
			return fmt.Errorf("%w: invalid override %q", ErrInvalidConfig, name)
		}
	}
	return nil
}
