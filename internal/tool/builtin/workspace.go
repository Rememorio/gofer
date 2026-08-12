package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/Rememorio/gofer/internal/artifact"
	"github.com/Rememorio/gofer/internal/policy"
	"github.com/Rememorio/gofer/internal/tool"
	"github.com/Rememorio/gofer/internal/workspace"
)

// WorkspaceTools binds one thread workspace to the standard file tools.
type WorkspaceTools struct {
	Workspace *workspace.Thread
	Artifacts *artifact.Catalog
	Now       func() time.Time
}

// Register validates dependencies and registers the standard workspace tools.
func (tools WorkspaceTools) Register(registry *tool.Registry) error {
	if registry == nil || tools.Workspace == nil || tools.Artifacts == nil {
		return errors.New("register workspace tools: registry, workspace, and artifact catalog are required")
	}
	if tools.Now == nil {
		tools.Now = time.Now
	}
	for _, candidate := range tools.definitions() {
		if err := registry.Register(candidate); err != nil {
			return err
		}
	}
	return nil
}

// PolicyDescriptors returns the resource extraction contract for built-in tools.
func PolicyDescriptors() map[string]policy.Descriptor {
	return map[string]policy.Descriptor{
		"read_file":           {Effect: policy.EffectRead, ResourceFields: []string{"path"}},
		"write_file":          {Effect: policy.EffectWrite, ResourceFields: []string{"path"}},
		"str_replace":         {Effect: policy.EffectWrite, ResourceFields: []string{"path"}},
		"ls":                  {Effect: policy.EffectRead, ResourceFields: []string{"path"}},
		"glob":                {Effect: policy.EffectRead, ResourceFields: []string{"path"}},
		"grep":                {Effect: policy.EffectRead, ResourceFields: []string{"path"}},
		"present_files":       {Effect: policy.EffectRead, ResourceFields: []string{"filepaths"}},
		"list_uploaded_files": {Effect: policy.EffectRead},
	}
}

func (tools WorkspaceTools) definitions() []tool.Tool {
	return []tool.Tool{
		tools.readFileTool(), tools.writeFileTool(), tools.replaceTool(), tools.listTool(),
		tools.globTool(), tools.grepTool(), tools.presentFilesTool(), tools.listUploadsTool(),
	}
}

func (tools WorkspaceTools) readFileTool() tool.Tool {
	return functionTool(
		"read_file", "Read a UTF-8 text file, optionally selecting an inclusive one-indexed line range.",
		`{"type":"object","properties":{"path":{"type":"string"},"start_line":{"type":"integer","minimum":1},"end_line":{"type":"integer","minimum":1}},"required":["path"],"additionalProperties":false}`,
		func(_ context.Context, arguments json.RawMessage) (any, error) {
			var input struct {
				Path      string `json:"path"`
				StartLine int    `json:"start_line"`
				EndLine   int    `json:"end_line"`
			}
			if err := decode(arguments, &input); err != nil {
				return nil, err
			}
			return tools.Workspace.ReadFile(input.Path, workspace.ReadOptions{
				StartLine: input.StartLine, EndLine: input.EndLine,
			})
		},
	)
}

func (tools WorkspaceTools) writeFileTool() tool.Tool {
	return functionTool(
		"write_file", "Write UTF-8 text under /mnt/user-data/workspace or /mnt/user-data/outputs.",
		`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"},"append":{"type":"boolean"}},"required":["path","content"],"additionalProperties":false}`,
		func(_ context.Context, arguments json.RawMessage) (any, error) {
			var input struct {
				Path    string `json:"path"`
				Content string `json:"content"`
				Append  bool   `json:"append"`
			}
			if err := decode(arguments, &input); err != nil {
				return nil, err
			}
			if err := tools.Workspace.WriteFile(input.Path, []byte(input.Content), input.Append); err != nil {
				return nil, err
			}
			return map[string]any{"path": input.Path, "bytes_written": len(input.Content), "appended": input.Append}, nil
		},
	)
}

func (tools WorkspaceTools) replaceTool() tool.Tool {
	return functionTool(
		"str_replace", "Replace exact text in a workspace or output file; a single replacement must be unambiguous.",
		`{"type":"object","properties":{"path":{"type":"string"},"old_text":{"type":"string","minLength":1},"new_text":{"type":"string"},"replace_all":{"type":"boolean"}},"required":["path","old_text","new_text"],"additionalProperties":false}`,
		func(_ context.Context, arguments json.RawMessage) (any, error) {
			var input struct {
				Path       string `json:"path"`
				OldText    string `json:"old_text"`
				NewText    string `json:"new_text"`
				ReplaceAll bool   `json:"replace_all"`
			}
			if err := decode(arguments, &input); err != nil {
				return nil, err
			}
			if err := tools.Workspace.Replace(input.Path, input.OldText, input.NewText, input.ReplaceAll); err != nil {
				return nil, err
			}
			return map[string]any{"path": input.Path, "replaced": true}, nil
		},
	)
}

func (tools WorkspaceTools) listTool() tool.Tool {
	return functionTool(
		"ls", "List files and directories under a virtual workspace path with bounded depth and results.",
		`{"type":"object","properties":{"path":{"type":"string"},"max_depth":{"type":"integer","minimum":1,"maximum":20},"max_results":{"type":"integer","minimum":1,"maximum":1000}},"required":["path"],"additionalProperties":false}`,
		func(_ context.Context, arguments json.RawMessage) (any, error) {
			var input struct {
				Path       string `json:"path"`
				MaxDepth   int    `json:"max_depth"`
				MaxResults int    `json:"max_results"`
			}
			if err := decode(arguments, &input); err != nil {
				return nil, err
			}
			return tools.Workspace.List(input.Path, workspace.ListOptions{
				MaxDepth: input.MaxDepth, MaxResults: input.MaxResults,
			})
		},
	)
}

func (tools WorkspaceTools) globTool() tool.Tool {
	return functionTool(
		"glob", "Find virtual paths matching a slash-separated glob below a workspace path.",
		`{"type":"object","properties":{"path":{"type":"string"},"pattern":{"type":"string","minLength":1},"include_dirs":{"type":"boolean"},"max_results":{"type":"integer","minimum":1,"maximum":1000}},"required":["path","pattern"],"additionalProperties":false}`,
		func(_ context.Context, arguments json.RawMessage) (any, error) {
			var input struct {
				Path        string `json:"path"`
				Pattern     string `json:"pattern"`
				IncludeDirs bool   `json:"include_dirs"`
				MaxResults  int    `json:"max_results"`
			}
			if err := decode(arguments, &input); err != nil {
				return nil, err
			}
			return tools.Workspace.Glob(input.Path, input.Pattern, workspace.GlobOptions{
				IncludeDirectories: input.IncludeDirs, MaxResults: input.MaxResults,
			})
		},
	)
}

func (tools WorkspaceTools) grepTool() tool.Tool {
	return functionTool(
		"grep", "Search bounded text files by regular expression or literal text.",
		`{"type":"object","properties":{"path":{"type":"string"},"pattern":{"type":"string","minLength":1},"glob":{"type":"string"},"literal":{"type":"boolean"},"case_sensitive":{"type":"boolean"},"max_results":{"type":"integer","minimum":1,"maximum":1000}},"required":["path","pattern"],"additionalProperties":false}`,
		func(_ context.Context, arguments json.RawMessage) (any, error) {
			var input struct {
				Path          string `json:"path"`
				Pattern       string `json:"pattern"`
				Glob          string `json:"glob"`
				Literal       bool   `json:"literal"`
				CaseSensitive bool   `json:"case_sensitive"`
				MaxResults    int    `json:"max_results"`
			}
			if err := decode(arguments, &input); err != nil {
				return nil, err
			}
			return tools.Workspace.Grep(input.Path, input.Pattern, workspace.GrepOptions{
				Glob: input.Glob, Literal: input.Literal, CaseSensitive: input.CaseSensitive,
				MaxResults: input.MaxResults,
			})
		},
	)
}

func (tools WorkspaceTools) presentFilesTool() tool.Tool {
	return functionTool(
		"present_files", "Present generated files from /mnt/user-data/outputs to the user.",
		`{"type":"object","properties":{"filepaths":{"type":"array","items":{"type":"string"},"minItems":1,"maxItems":100}},"required":["filepaths"],"additionalProperties":false}`,
		func(ctx context.Context, arguments json.RawMessage) (any, error) {
			var input struct {
				Filepaths []string `json:"filepaths"`
			}
			if err := decode(arguments, &input); err != nil {
				return nil, err
			}
			return tools.Artifacts.Present(ctx, tools.Workspace, input.Filepaths, tools.Now())
		},
	)
}

func (tools WorkspaceTools) listUploadsTool() tool.Tool {
	return functionTool(
		"list_uploaded_files", "List files uploaded to the current thread.",
		`{"type":"object","properties":{"path":{"type":"string","const":"/mnt/user-data/uploads"},"max_results":{"type":"integer","minimum":1,"maximum":1000}},"additionalProperties":false}`,
		func(_ context.Context, arguments json.RawMessage) (any, error) {
			var input struct {
				Path       string `json:"path"`
				MaxResults int    `json:"max_results"`
			}
			if err := decode(arguments, &input); err != nil {
				return nil, err
			}
			if input.Path == "" {
				input.Path = workspace.UploadsRoot
			}
			return tools.Workspace.List(input.Path, workspace.ListOptions{MaxDepth: 1, MaxResults: input.MaxResults})
		},
	)
}

func functionTool(
	name, description, schema string,
	execute func(context.Context, json.RawMessage) (any, error),
) tool.Tool {
	return tool.Func{
		DefinitionValue: tool.Definition{
			Name: name, Description: description, InputSchema: json.RawMessage(schema), ReadOnly: readOnlyTool(name),
		},
		ExecuteFunc: func(ctx context.Context, arguments json.RawMessage) (json.RawMessage, error) {
			value, err := execute(ctx, arguments)
			if err != nil {
				return nil, err
			}
			encoded, err := json.Marshal(value)
			if err != nil {
				return nil, fmt.Errorf("encode %s result: %w", name, err)
			}
			return encoded, nil
		},
	}
}

func decode(arguments json.RawMessage, output any) error {
	decoder := json.NewDecoder(bytes.NewReader(arguments))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return fmt.Errorf("decode tool arguments: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("decode tool arguments: multiple JSON values")
		}
		return fmt.Errorf("decode trailing tool arguments: %w", err)
	}
	return nil
}

func readOnlyTool(name string) bool {
	switch name {
	case "read_file", "ls", "glob", "grep", "list_uploaded_files", "present_files":
		return true
	case "write_file", "str_replace":
		return false
	default:
		return false
	}
}
