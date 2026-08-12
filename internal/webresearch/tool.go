package webresearch

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Rememorio/gofer/internal/policy"
	"github.com/Rememorio/gofer/internal/tool"
)

// Tools binds a research client to model-visible web tools.
type Tools struct {
	Client *Client
}

// Register adds only the capabilities configured on the client.
func (tools Tools) Register(registry *tool.Registry) error {
	if registry == nil || tools.Client == nil {
		return fmt.Errorf("%w: registry and client are required", ErrInvalidConfig)
	}
	definitions := make([]tool.Tool, 0, 2)
	if tools.Client.HasSearch() {
		definitions = append(definitions, tools.searchTool())
	}
	if tools.Client.HasFetch() {
		definitions = append(definitions, tools.fetchTool())
	}
	return registry.RegisterAll(definitions...)
}

// PolicyDescriptors returns authorization metadata for research tools.
func PolicyDescriptors() map[string]policy.Descriptor {
	return map[string]policy.Descriptor{
		"web_search": {Effect: policy.EffectNetwork, ResourceFields: []string{"query"}},
		"web_fetch":  {Effect: policy.EffectNetwork, ResourceFields: []string{"url"}},
	}
}

func (tools Tools) searchTool() tool.Tool {
	return tool.Func{
		DefinitionValue: tool.Definition{
			Name: "web_search", Description: "Search the public web and return normalized, citation-ready results.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","minLength":1,"maxLength":400},"max_results":{"type":"integer","minimum":1,"maximum":20}},"required":["query"],"additionalProperties":false}`),
			ReadOnly:    true, UntrustedOutput: true,
		},
		ExecuteFunc: func(ctx context.Context, arguments json.RawMessage) (json.RawMessage, error) {
			var input struct {
				Query      string `json:"query"`
				MaxResults int    `json:"max_results"`
			}
			if err := json.Unmarshal(arguments, &input); err != nil {
				return nil, fmt.Errorf("decode web_search arguments: %w", err)
			}
			response, err := tools.Client.Search(ctx, input.Query, input.MaxResults)
			if err != nil {
				return nil, err
			}
			return json.Marshal(response)
		},
	}
}

func (tools Tools) fetchTool() tool.Tool {
	return tool.Func{
		DefinitionValue: tool.Definition{
			Name: "web_fetch", Description: "Fetch one public HTTP(S) page and return bounded readable text.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"url":{"type":"string","minLength":1,"maxLength":4096}},"required":["url"],"additionalProperties":false}`),
			ReadOnly:    true, UntrustedOutput: true,
		},
		ExecuteFunc: func(ctx context.Context, arguments json.RawMessage) (json.RawMessage, error) {
			var input struct {
				URL string `json:"url"`
			}
			if err := json.Unmarshal(arguments, &input); err != nil {
				return nil, fmt.Errorf("decode web_fetch arguments: %w", err)
			}
			response, err := tools.Client.Fetch(ctx, input.URL)
			if err != nil {
				return nil, err
			}
			return json.Marshal(response)
		},
	}
}
