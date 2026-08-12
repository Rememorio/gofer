package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/model"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

var (
	// ErrInvalidDefinition identifies a malformed tool definition or schema.
	ErrInvalidDefinition = errors.New("invalid tool definition")
	// ErrDuplicate identifies a duplicate tool registration.
	ErrDuplicate = errors.New("duplicate tool")
	// ErrInvalidOutput identifies tool output that is not valid JSON.
	ErrInvalidOutput = errors.New("invalid tool output")

	toolNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
)

// ResultError is a model-correctable tool failure with structured JSON output.
type ResultError struct {
	Output json.RawMessage
}

// NewResultError copies output and constructs a model-correctable failure.
func NewResultError(output json.RawMessage) error {
	return &ResultError{Output: append(json.RawMessage(nil), output...)}
}

// Error implements error without embedding potentially sensitive output text.
func (*ResultError) Error() string { return "tool returned an error result" }

// Definition describes one executable tool and its JSON Schema input contract.
type Definition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	ReadOnly    bool            `json:"read_only,omitempty"`
}

// Tool executes validated JSON arguments and returns JSON output.
type Tool interface {
	Definition() Definition
	Execute(context.Context, json.RawMessage) (json.RawMessage, error)
}

// Func adapts a function into a Tool.
type Func struct {
	DefinitionValue Definition
	ExecuteFunc     func(context.Context, json.RawMessage) (json.RawMessage, error)
}

// Definition returns the function tool's immutable description.
func (function Func) Definition() Definition {
	return cloneDefinition(function.DefinitionValue)
}

// Execute invokes the function tool.
func (function Func) Execute(ctx context.Context, arguments json.RawMessage) (json.RawMessage, error) {
	if function.ExecuteFunc == nil {
		return nil, errors.New("tool execute function is nil")
	}
	return function.ExecuteFunc(ctx, arguments)
}

// Registry is a concurrency-safe collection of compiled tools.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]registeredTool
}

type registeredTool struct {
	tool       Tool
	definition Definition
	schema     *jsonschema.Schema
}

// NewRegistry constructs an empty tool registry.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]registeredTool)}
}

// Register validates and atomically adds a tool.
func (registry *Registry) Register(tool Tool) error {
	return registry.RegisterAll(tool)
}

// RegisterAll validates and atomically adds a batch of tools.
func (registry *Registry) RegisterAll(tools ...Tool) error {
	prepared := make([]registeredTool, 0, len(tools))
	names := make(map[string]struct{}, len(tools))
	for _, candidate := range tools {
		if candidate == nil {
			return fmt.Errorf("%w: tool is nil", ErrInvalidDefinition)
		}
		definition := candidate.Definition()
		schema, err := compileDefinition(definition)
		if err != nil {
			return err
		}
		if _, duplicate := names[definition.Name]; duplicate {
			return fmt.Errorf("register tool %s: %w", definition.Name, ErrDuplicate)
		}
		names[definition.Name] = struct{}{}
		prepared = append(prepared, registeredTool{
			tool: candidate, definition: cloneDefinition(definition), schema: schema,
		})
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	for _, registered := range prepared {
		if _, exists := registry.tools[registered.definition.Name]; exists {
			return fmt.Errorf("register tool %s: %w", registered.definition.Name, ErrDuplicate)
		}
	}
	for _, registered := range prepared {
		registry.tools[registered.definition.Name] = registered
	}
	return nil
}

// Definitions returns model-visible definitions in stable name order.
func (registry *Registry) Definitions() []model.ToolDefinition {
	registry.mu.RLock()
	definitions := make([]model.ToolDefinition, 0, len(registry.tools))
	for _, registered := range registry.tools {
		definition := registered.definition
		definitions = append(definitions, model.ToolDefinition{
			Name:        definition.Name,
			Description: definition.Description,
			InputSchema: append(json.RawMessage(nil), definition.InputSchema...),
		})
	}
	registry.mu.RUnlock()
	sort.Slice(definitions, func(left, right int) bool {
		return definitions[left].Name < definitions[right].Name
	})
	return definitions
}

// Execute validates call and invokes its registered tool.
//
// Unknown tools, schema violations, and handler failures are returned as
// IsError tool results so the model can correct them. Context cancellation and
// invalid handler output are host failures and are returned as Go errors.
func (registry *Registry) Execute(ctx context.Context, call domain.ToolCall) (domain.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return domain.ToolResult{}, err
	}
	registry.mu.RLock()
	registered, exists := registry.tools[call.Name]
	registry.mu.RUnlock()
	if !exists {
		return errorResult(call.ID, fmt.Sprintf("unknown tool %q", call.Name)), nil
	}
	value, err := decodeArguments(call.Arguments)
	if err != nil {
		return errorResult(call.ID, err.Error()), nil
	}
	if err := registered.schema.Validate(value); err != nil {
		return errorResult(call.ID, fmt.Sprintf("arguments do not match schema: %v", err)), nil
	}
	output, err := registered.tool.Execute(ctx, append(json.RawMessage(nil), call.Arguments...))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return domain.ToolResult{}, ctxErr
		}
		var resultError *ResultError
		if errors.As(err, &resultError) {
			if len(resultError.Output) == 0 || !json.Valid(resultError.Output) {
				return domain.ToolResult{}, fmt.Errorf("execute tool %s: %w", call.Name, ErrInvalidOutput)
			}
			return domain.ToolResult{
				CallID: call.ID, Output: append(json.RawMessage(nil), resultError.Output...), IsError: true,
			}, nil
		}
		return errorResult(call.ID, err.Error()), nil
	}
	if len(output) == 0 || !json.Valid(output) {
		return domain.ToolResult{}, fmt.Errorf("execute tool %s: %w", call.Name, ErrInvalidOutput)
	}
	return domain.ToolResult{CallID: call.ID, Output: append(json.RawMessage(nil), output...)}, nil
}

func compileDefinition(definition Definition) (*jsonschema.Schema, error) {
	if !toolNamePattern.MatchString(definition.Name) || strings.TrimSpace(definition.Description) == "" {
		return nil, fmt.Errorf("%w: name or description is invalid", ErrInvalidDefinition)
	}
	if len(definition.InputSchema) == 0 || !json.Valid(definition.InputSchema) {
		return nil, fmt.Errorf("%w: input schema must be valid JSON", ErrInvalidDefinition)
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(definition.InputSchema))
	if err != nil {
		return nil, fmt.Errorf("%w: decode input schema: %w", ErrInvalidDefinition, err)
	}
	location := "gofer://tools/" + url.PathEscape(definition.Name) + "/input-schema"
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(location, document); err != nil {
		return nil, fmt.Errorf("%w: add input schema: %w", ErrInvalidDefinition, err)
	}
	schema, err := compiler.Compile(location)
	if err != nil {
		return nil, fmt.Errorf("%w: compile input schema: %w", ErrInvalidDefinition, err)
	}
	return schema, nil
}

func decodeArguments(arguments json.RawMessage) (any, error) {
	if len(arguments) == 0 {
		return nil, errors.New("arguments are required")
	}
	decoder := json.NewDecoder(bytes.NewReader(arguments))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("arguments must be valid JSON: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("arguments contain multiple JSON values")
		}
		return nil, fmt.Errorf("decode trailing arguments: %w", err)
	}
	return value, nil
}

func errorResult(callID, message string) domain.ToolResult {
	data, _ := json.Marshal(map[string]string{"error": message})
	return domain.ToolResult{CallID: callID, Output: data, IsError: true}
}

func cloneDefinition(definition Definition) Definition {
	definition.InputSchema = append(json.RawMessage(nil), definition.InputSchema...)
	return definition
}
