package skill

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"strings"

	"github.com/Rememorio/gofer/internal/tool"
)

// DescribeTool constructs the progressive skill metadata discovery tool.
func (catalog *Catalog) DescribeTool() tool.Tool {
	return tool.Func{
		DefinitionValue: tool.Definition{
			Name:        "describe_skill",
			Description: "Search installed skills and return metadata and the read-only SKILL.md location.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","minLength":1},"limit":{"type":"integer","minimum":1,"maximum":20}},"required":["name"],"additionalProperties":false}`),
			ReadOnly:    true,
		},
		ExecuteFunc: func(_ context.Context, arguments json.RawMessage) (json.RawMessage, error) {
			var input struct {
				Name  string `json:"name"`
				Limit int    `json:"limit"`
			}
			if err := json.Unmarshal(arguments, &input); err != nil {
				return nil, fmt.Errorf("decode describe_skill arguments: %w", err)
			}
			if input.Limit == 0 {
				input.Limit = 5
			}
			return json.Marshal(catalog.Search(input.Name, input.Limit))
		},
	}
}

// ReadTool constructs the bounded SKILL.md content reader.
func (catalog *Catalog) ReadTool() tool.Tool {
	return tool.Func{
		DefinitionValue: tool.Definition{
			Name: "read_skill", Description: "Read an enabled skill's validated SKILL.md instructions by exact name.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","minLength":1}},"required":["name"],"additionalProperties":false}`), ReadOnly: true,
		},
		ExecuteFunc: func(ctx context.Context, arguments json.RawMessage) (json.RawMessage, error) {
			var input struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(arguments, &input); err != nil {
				return nil, fmt.Errorf("decode read_skill arguments: %w", err)
			}
			document, err := catalog.Load(ctx, input.Name)
			if err != nil {
				output, _ := json.Marshal(map[string]string{"error": err.Error()})
				return nil, tool.NewResultError(output)
			}
			return json.Marshal(map[string]string{"name": input.Name, "content": document})
		},
	}
}

// RenderDescription returns safe human-readable metadata for matching skills.
func RenderDescription(skills []Skill) string {
	blocks := make([]string, 0, len(skills))
	for _, candidate := range skills {
		allowed := "(all)"
		if candidate.AllowedToolsSet {
			allowed = strings.Join(candidate.AllowedTools, ", ")
			if allowed == "" {
				allowed = "(none)"
			}
		}
		blocks = append(blocks, fmt.Sprintf(
			"## Skill: %s\n- Description: %s\n- Allowed tools: %s\n- Location: %s",
			html.EscapeString(candidate.Name), html.EscapeString(candidate.Description),
			html.EscapeString(allowed), html.EscapeString(candidate.DocumentPath),
		))
	}
	return strings.Join(blocks, "\n\n")
}
