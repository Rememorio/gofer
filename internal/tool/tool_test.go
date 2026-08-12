package tool

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Rememorio/gofer/internal/domain"
)

var echoSchema = json.RawMessage(`{
  "type": "object",
  "properties": {"text": {"type": "string", "minLength": 1}},
  "required": ["text"],
  "additionalProperties": false
}`)

func TestRegistryRegisterAndExecute(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	definition := Definition{Name: "echo", Description: "Echo text", InputSchema: append(json.RawMessage(nil), echoSchema...), ReadOnly: true}
	tool := Func{DefinitionValue: definition, ExecuteFunc: func(_ context.Context, arguments json.RawMessage) (json.RawMessage, error) {
		return append(json.RawMessage(nil), arguments...), nil
	}}
	if err := registry.Register(tool); err != nil {
		t.Fatalf("Register(): %v", err)
	}
	definition.InputSchema[0] = '['

	definitions := registry.Definitions()
	if len(definitions) != 1 || definitions[0].Name != "echo" || !json.Valid(definitions[0].InputSchema) {
		t.Fatalf("Definitions() = %#v", definitions)
	}
	result, err := registry.Execute(context.Background(), domain.ToolCall{
		ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{"text":"hello"}`),
	})
	if err != nil {
		t.Fatalf("Execute(): %v", err)
	}
	if result.IsError || string(result.Output) != `{"text":"hello"}` {
		t.Fatalf("Execute() = %#v", result)
	}
}

func TestRegistryDefinitionsAreSorted(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	for _, name := range []string{"zeta", "alpha"} {
		if err := registry.Register(Func{
			DefinitionValue: Definition{Name: name, Description: name, InputSchema: json.RawMessage(`{"type":"object"}`)},
			ExecuteFunc:     func(context.Context, json.RawMessage) (json.RawMessage, error) { return json.RawMessage(`{}`), nil },
		}); err != nil {
			t.Fatalf("Register(%s): %v", name, err)
		}
	}
	definitions := registry.Definitions()
	if definitions[0].Name != "alpha" || definitions[1].Name != "zeta" {
		t.Fatalf("Definitions() = %#v", definitions)
	}
}

func TestRegistryRegisterAllIsAtomic(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	alpha := testTool("alpha")
	if err := registry.RegisterAll(alpha, testTool("beta")); err != nil {
		t.Fatalf("RegisterAll(): %v", err)
	}
	if err := registry.RegisterAll(testTool("gamma"), alpha); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("RegisterAll(existing duplicate) error = %v, want ErrDuplicate", err)
	}
	if len(registry.Definitions()) != 2 {
		t.Fatalf("definitions after existing duplicate = %#v", registry.Definitions())
	}
	if err := registry.RegisterAll(testTool("gamma"), testTool("gamma")); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("RegisterAll(batch duplicate) error = %v, want ErrDuplicate", err)
	}
	if len(registry.Definitions()) != 2 {
		t.Fatalf("definitions after batch duplicate = %#v", registry.Definitions())
	}
	if err := registry.RegisterAll(testTool("gamma"), nil); !errors.Is(err, ErrInvalidDefinition) {
		t.Fatalf("RegisterAll(nil) error = %v, want ErrInvalidDefinition", err)
	}
	if len(registry.Definitions()) != 2 {
		t.Fatalf("definitions after invalid batch = %#v", registry.Definitions())
	}
}

func TestRegistryReturnsModelCorrectableErrors(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	if err := registry.Register(Func{
		DefinitionValue: Definition{Name: "fail", Description: "Fail", InputSchema: echoSchema},
		ExecuteFunc: func(context.Context, json.RawMessage) (json.RawMessage, error) {
			return nil, errors.New("tool failed")
		},
	}); err != nil {
		t.Fatalf("Register(): %v", err)
	}
	tests := []domain.ToolCall{
		{ID: "1", Name: "missing", Arguments: json.RawMessage(`{}`)},
		{ID: "2", Name: "fail", Arguments: json.RawMessage(`{`)},
		{ID: "2b", Name: "fail", Arguments: json.RawMessage(`{} {}`)},
		{ID: "3", Name: "fail", Arguments: json.RawMessage(`{"text":1}`)},
		{ID: "4", Name: "fail", Arguments: json.RawMessage(`{"text":"ok"}`)},
	}
	for _, call := range tests {
		result, err := registry.Execute(context.Background(), call)
		if err != nil {
			t.Fatalf("Execute(%s): %v", call.ID, err)
		}
		if !result.IsError || !json.Valid(result.Output) {
			t.Fatalf("Execute(%s) = %#v, want JSON error result", call.ID, result)
		}
	}
}

func TestRegistryRejectsInvalidRegistrations(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	if err := registry.Register(nil); !errors.Is(err, ErrInvalidDefinition) {
		t.Fatalf("Register(nil) error = %v, want ErrInvalidDefinition", err)
	}
	tests := []Definition{
		{Name: "bad name", Description: "x", InputSchema: json.RawMessage(`{}`)},
		{Name: "x", Description: "", InputSchema: json.RawMessage(`{}`)},
		{Name: "x", Description: "x", InputSchema: json.RawMessage(`{`)},
		{Name: "x", Description: "x", InputSchema: json.RawMessage(`{"type":"unknown"}`)},
	}
	for _, definition := range tests {
		err := registry.Register(Func{DefinitionValue: definition})
		if !errors.Is(err, ErrInvalidDefinition) {
			t.Fatalf("Register(%#v) error = %v, want ErrInvalidDefinition", definition, err)
		}
	}

	valid := Func{DefinitionValue: Definition{Name: "echo", Description: "Echo", InputSchema: echoSchema}}
	if err := registry.Register(valid); err != nil {
		t.Fatalf("Register(valid): %v", err)
	}
	if err := registry.Register(valid); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("Register(duplicate) error = %v, want ErrDuplicate", err)
	}
}

func TestRegistryPropagatesHostFailures(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	if err := registry.Register(Func{
		DefinitionValue: Definition{Name: "invalid", Description: "Invalid output", InputSchema: json.RawMessage(`{"type":"object"}`)},
		ExecuteFunc:     func(context.Context, json.RawMessage) (json.RawMessage, error) { return json.RawMessage(`{`), nil },
	}); err != nil {
		t.Fatalf("Register(): %v", err)
	}
	if _, err := registry.Execute(context.Background(), domain.ToolCall{ID: "1", Name: "invalid", Arguments: json.RawMessage(`{}`)}); !errors.Is(err, ErrInvalidOutput) {
		t.Fatalf("Execute() error = %v, want ErrInvalidOutput", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := registry.Execute(ctx, domain.ToolCall{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute(cancelled) error = %v, want context.Canceled", err)
	}
}

func TestRegistryPreservesStructuredResultErrors(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	structured := json.RawMessage(`{"content":[{"type":"text","text":"remote failed"}],"isError":true}`)
	if err := registry.Register(Func{
		DefinitionValue: Definition{Name: "remote", Description: "Remote", InputSchema: json.RawMessage(`{"type":"object"}`)},
		ExecuteFunc: func(context.Context, json.RawMessage) (json.RawMessage, error) {
			return nil, NewResultError(structured)
		},
	}); err != nil {
		t.Fatalf("Register(): %v", err)
	}
	result, err := registry.Execute(context.Background(), domain.ToolCall{ID: "1", Name: "remote", Arguments: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("Execute(): %v", err)
	}
	if !result.IsError || string(result.Output) != string(structured) {
		t.Fatalf("Execute() = %#v", result)
	}

	invalid := testTool("invalid_error").(Func)
	invalid.ExecuteFunc = func(context.Context, json.RawMessage) (json.RawMessage, error) {
		return nil, NewResultError(json.RawMessage(`{`))
	}
	if err := registry.Register(invalid); err != nil {
		t.Fatalf("Register(invalid error): %v", err)
	}
	if _, err := registry.Execute(context.Background(), domain.ToolCall{ID: "2", Name: "invalid_error", Arguments: json.RawMessage(`{}`)}); !errors.Is(err, ErrInvalidOutput) {
		t.Fatalf("Execute(invalid error) error = %v, want ErrInvalidOutput", err)
	}
}

func TestFuncWithoutExecutor(t *testing.T) {
	t.Parallel()

	_, err := (Func{}).Execute(context.Background(), nil)
	if err == nil {
		t.Fatal("Execute() error = nil")
	}
}

func testTool(name string) Tool {
	return Func{
		DefinitionValue: Definition{Name: name, Description: name, InputSchema: json.RawMessage(`{"type":"object"}`)},
		ExecuteFunc:     func(context.Context, json.RawMessage) (json.RawMessage, error) { return json.RawMessage(`{}`), nil },
	}
}
