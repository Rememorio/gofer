package memory

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/tool"
)

func TestMemoryToolsLifecycle(t *testing.T) {
	t.Parallel()
	now := time.Unix(100, 0)
	scope := Scope{UserID: "user", ThreadID: "thread"}
	store := NewInMemory()
	registry := tool.NewRegistry()
	tools := Tools{Store: store, Scope: func(context.Context) (Scope, error) { return scope, nil }, Now: func() time.Time { return now }, NewID: func() (string, error) { return "generated", nil }}
	if err := tools.Register(registry); err != nil {
		t.Fatal(err)
	}
	result := executeMemoryTool(t, registry, "memory_upsert", `{"text":"Use Go for services","tags":["preference"],"ttl_seconds":60}`)
	if result.IsError || string(result.Output) == "" {
		t.Fatalf("upsert = %#v", result)
	}
	matches := executeMemoryTool(t, registry, "memory_search", `{"query":"Go","limit":5}`)
	var found []Match
	if err := json.Unmarshal(matches.Output, &found); err != nil || len(found) != 1 || found[0].Entry.ID != "generated" {
		t.Fatalf("search = %#v, %v", found, err)
	}
	deleted := executeMemoryTool(t, registry, "memory_delete", `{"id":"generated"}`)
	if deleted.IsError {
		t.Fatalf("delete = %#v", deleted)
	}
	if len(PolicyDescriptors()) != 3 {
		t.Fatalf("descriptors = %#v", PolicyDescriptors())
	}
}

func TestMemoryToolsFailures(t *testing.T) {
	t.Parallel()
	registry := tool.NewRegistry()
	if err := (Tools{}).Register(registry); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Register() error = %v", err)
	}
	failing := Tools{Store: NewInMemory(), Scope: func(context.Context) (Scope, error) { return Scope{}, errors.New("scope") }, NewID: func() (string, error) { return "", errors.New("id") }}
	if err := failing.Register(registry); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, arguments string }{
		{"memory_search", `{"query":"x"}`},
		{"memory_upsert", `{"text":"x"}`},
		{"memory_delete", `{"id":"x"}`},
	} {
		result := executeMemoryTool(t, registry, test.name, test.arguments)
		if !result.IsError {
			t.Fatalf("%s = %#v", test.name, result)
		}
	}
	if _, err := newMemoryID(); err != nil {
		t.Fatal(err)
	}
}

func executeMemoryTool(t *testing.T, registry *tool.Registry, name, arguments string) domain.ToolResult {
	t.Helper()
	result, err := registry.Execute(context.Background(), domain.ToolCall{ID: "call", Name: name, Arguments: json.RawMessage(arguments)})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
