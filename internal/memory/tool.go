package memory

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Rememorio/gofer/internal/policy"
	"github.com/Rememorio/gofer/internal/tool"
)

// Tools exposes scoped memory search, upsert, and deletion to an agent.
type Tools struct {
	Store Store
	Scope ScopeProvider
	Now   func() time.Time
	NewID func() (string, error)
}

// Register validates dependencies and registers memory tools atomically.
func (tools Tools) Register(registry *tool.Registry) error {
	if registry == nil || tools.Store == nil || tools.Scope == nil {
		return fmt.Errorf("%w: registry, store, and scope are required", ErrInvalid)
	}
	if tools.Now == nil {
		tools.Now = time.Now
	}
	if tools.NewID == nil {
		tools.NewID = NewID
	}
	return registry.RegisterAll(tools.searchTool(), tools.upsertTool(), tools.deleteTool())
}

// PolicyDescriptors returns authorization metadata for memory tools.
func PolicyDescriptors() map[string]policy.Descriptor {
	return map[string]policy.Descriptor{
		"memory_search": {Effect: policy.EffectRead},
		"memory_upsert": {Effect: policy.EffectWrite},
		"memory_delete": {Effect: policy.EffectDestructive, ResourceFields: []string{"id"}},
	}
}

func (tools Tools) searchTool() tool.Tool {
	return tools.function("memory_search", "Search memories in the authenticated user and thread scope.", `{"type":"object","properties":{"query":{"type":"string","maxLength":16384},"tags":{"type":"array","maxItems":32,"items":{"type":"string"}},"limit":{"type":"integer","minimum":1,"maximum":100}},"additionalProperties":false}`, true, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var input struct {
			Query string   `json:"query"`
			Tags  []string `json:"tags"`
			Limit int      `json:"limit"`
		}
		if err := json.Unmarshal(raw, &input); err != nil {
			return nil, err
		}
		if input.Limit == 0 {
			input.Limit = 10
		}
		scope, err := tools.Scope(ctx)
		if err != nil {
			return nil, err
		}
		return tools.Store.Search(ctx, Query{Scope: scope, Text: input.Query, Tags: input.Tags, Limit: input.Limit, Now: tools.Now()})
	})
}

func (tools Tools) upsertTool() tool.Tool {
	return tools.function("memory_upsert", "Create or replace a durable memory in the authenticated scope.", `{"type":"object","properties":{"id":{"type":"string","maxLength":128},"text":{"type":"string","minLength":1,"maxLength":65536},"tags":{"type":"array","maxItems":32,"items":{"type":"string"}},"category":{"type":"string","maxLength":128},"confidence":{"type":"number","minimum":0,"maximum":1},"source":{"type":"string","maxLength":1024},"ttl_seconds":{"type":"integer","minimum":0,"maximum":31536000}},"required":["text"],"additionalProperties":false}`, false, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var input struct {
			ID         string   `json:"id"`
			Text       string   `json:"text"`
			Tags       []string `json:"tags"`
			Category   string   `json:"category"`
			Confidence *float64 `json:"confidence"`
			Source     string   `json:"source"`
			TTLSeconds int      `json:"ttl_seconds"`
		}
		if err := json.Unmarshal(raw, &input); err != nil {
			return nil, err
		}
		if strings.TrimSpace(input.ID) == "" {
			var err error
			input.ID, err = tools.NewID()
			if err != nil {
				return nil, err
			}
		}
		scope, err := tools.Scope(ctx)
		if err != nil {
			return nil, err
		}
		now := tools.Now().UTC()
		createdAt := now
		if existing, getErr := tools.Store.Get(ctx, scope, input.ID); getErr == nil {
			createdAt = existing.CreatedAt
		} else if getErr != nil && !errors.Is(getErr, ErrNotFound) {
			return nil, getErr
		}
		if strings.TrimSpace(input.Category) == "" {
			input.Category = "context"
		}
		confidence := 0.5
		if input.Confidence != nil {
			confidence = *input.Confidence
		}
		entry := Entry{ID: input.ID, Scope: scope, Text: input.Text, Tags: input.Tags, Category: input.Category, Confidence: confidence, Source: input.Source, CreatedAt: createdAt, UpdatedAt: now}
		if input.TTLSeconds > 0 {
			entry.ExpiresAt = now.Add(time.Duration(input.TTLSeconds) * time.Second)
		}
		return entry, tools.Store.Upsert(ctx, entry)
	})
}

func (tools Tools) deleteTool() tool.Tool {
	return tools.function("memory_delete", "Delete one memory from the authenticated scope.", `{"type":"object","properties":{"id":{"type":"string","minLength":1,"maxLength":128}},"required":["id"],"additionalProperties":false}`, false, func(ctx context.Context, raw json.RawMessage) (any, error) {
		var input struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &input); err != nil {
			return nil, err
		}
		scope, err := tools.Scope(ctx)
		if err != nil {
			return nil, err
		}
		if err = tools.Store.Delete(ctx, scope, input.ID); err != nil {
			return nil, err
		}
		return map[string]any{"id": input.ID, "deleted": true}, nil
	})
}

func (tools Tools) function(name, description, schema string, readOnly bool, execute func(context.Context, json.RawMessage) (any, error)) tool.Tool {
	return tool.Func{DefinitionValue: tool.Definition{Name: name, Description: description, InputSchema: json.RawMessage(schema), ReadOnly: readOnly}, ExecuteFunc: func(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
		value, err := execute(ctx, raw)
		if err != nil {
			output, _ := json.Marshal(map[string]string{"error": err.Error()})
			return nil, tool.NewResultError(output)
		}
		return json.Marshal(value)
	}}
}

// NewID creates one cryptographically random memory identifier.
func NewID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("create memory id: %w", err)
	}
	return "mem_" + hex.EncodeToString(raw[:]), nil
}
