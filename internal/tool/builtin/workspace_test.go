package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/artifact"
	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/tool"
	"github.com/Rememorio/gofer/internal/workspace"
)

func TestWorkspaceToolsLifecycle(t *testing.T) {
	t.Parallel()

	registry, thread, catalog := toolFixture(t)
	defer func() { _ = thread.Close() }()
	executeOK(t, registry, "write_file", `{"path":"/mnt/user-data/workspace/report.txt","content":"hello world\nsecond"}`)
	read := executeOK(t, registry, "read_file", `{"path":"/mnt/user-data/workspace/report.txt","start_line":2}`)
	if !containsJSON(read, `"content":"second"`) || !containsJSON(read, `"revision":"sha256:`) {
		t.Fatalf("read output = %s", read)
	}
	executeOK(t, registry, "str_replace", `{"path":"/mnt/user-data/workspace/report.txt","old_text":"hello","new_text":"hi"}`)
	list := executeOK(t, registry, "ls", `{"path":"/mnt/user-data/workspace","max_depth":2}`)
	if !containsJSON(list, `report.txt`) {
		t.Fatalf("list output = %s", list)
	}
	glob := executeOK(t, registry, "glob", `{"path":"/mnt/user-data/workspace","pattern":"**/*.txt"}`)
	if !containsJSON(glob, `report.txt`) {
		t.Fatalf("glob output = %s", glob)
	}
	grep := executeOK(t, registry, "grep", `{"path":"/mnt/user-data/workspace","pattern":"HI","case_sensitive":false}`)
	if !containsJSON(grep, `"line_number":1`) {
		t.Fatalf("grep output = %s", grep)
	}
	executeOK(t, registry, "write_file", `{"path":"/mnt/user-data/outputs/final.md","content":"# Done"}`)
	presented := executeOK(t, registry, "present_files", `{"filepaths":["/mnt/user-data/outputs/final.md"]}`)
	if !containsJSON(presented, `"media_type":"text/markdown`) || len(catalog.List(thread.ID())) != 1 {
		t.Fatalf("presented = %s, catalog = %#v", presented, catalog.List(thread.ID()))
	}
}

func TestWorkspaceToolsListUploadsAndErrors(t *testing.T) {
	t.Parallel()

	registry, thread, _ := toolFixture(t)
	defer func() { _ = thread.Close() }()
	if _, err := thread.PutUpload("input.csv", strings.NewReader("a,b\n1,2")); err != nil {
		t.Fatalf("PutUpload(): %v", err)
	}
	listed := executeOK(t, registry, "list_uploaded_files", `{}`)
	if !containsJSON(listed, `input.csv`) {
		t.Fatalf("list uploads = %s", listed)
	}
	if names := registry.UntrustedOutputTools(); len(names) != 1 || names[0] != "list_uploaded_files" {
		t.Fatalf("untrusted tools = %#v", names)
	}
	tests := []struct {
		name      string
		arguments string
	}{
		{name: "read_file", arguments: `{"path":"/etc/passwd"}`},
		{name: "write_file", arguments: `{"path":"/mnt/user-data/uploads/x","content":"x"}`},
		{name: "str_replace", arguments: `{"path":"/mnt/user-data/workspace/missing","old_text":"x","new_text":"y"}`},
		{name: "present_files", arguments: `{"filepaths":["/mnt/user-data/workspace/report"]}`},
		{name: "grep", arguments: `{"path":"/mnt/user-data/workspace","pattern":"["}`},
	}
	for _, test := range tests {
		result := execute(t, registry, test.name, test.arguments)
		if !result.IsError {
			t.Fatalf("%s result = %#v, want tool error", test.name, result)
		}
	}
	result := execute(t, registry, "write_file", `{"path":"/mnt/user-data/workspace/x","content":"x","unknown":true}`)
	if !result.IsError {
		t.Fatalf("unknown field result = %#v, want tool error", result)
	}
	if result = execute(t, registry, "list_uploaded_files", `{"include_outline":[""]}`); !result.IsError {
		t.Fatalf("blank outline filename result = %#v", result)
	}
}

func TestWorkspaceToolRegistrationValidation(t *testing.T) {
	t.Parallel()

	if err := (WorkspaceTools{}).Register(nil); err == nil {
		t.Fatal("Register(nil) error = nil")
	}
	registry, thread, catalog := toolFixture(t)
	defer func() { _ = thread.Close() }()
	if err := (WorkspaceTools{Workspace: thread, Artifacts: catalog}).Register(registry); !errors.Is(err, tool.ErrDuplicate) {
		t.Fatalf("Register(duplicate) error = %v, want ErrDuplicate", err)
	}
	descriptors := PolicyDescriptors()
	if descriptors["write_file"].Effect != "write" || descriptors["read_file"].Effect != "read" {
		t.Fatalf("PolicyDescriptors() = %#v", descriptors)
	}
}

func TestToolHelpers(t *testing.T) {
	t.Parallel()

	var output map[string]any
	if err := decode(json.RawMessage(`{"value":1}`), &output); err != nil {
		t.Fatalf("decode(valid): %v", err)
	}
	for _, arguments := range []json.RawMessage{
		json.RawMessage(`{`), json.RawMessage(`{} {}`), json.RawMessage(`{} trailing`),
	} {
		if err := decode(arguments, &output); err == nil {
			t.Fatalf("decode(%s) error = nil", arguments)
		}
	}
	if readOnlyTool("read_file") != true || readOnlyTool("write_file") != false || readOnlyTool("unknown") != false {
		t.Fatal("readOnlyTool() returned unexpected value")
	}
	broken := functionTool("broken", "Broken result", `{"type":"object"}`,
		func(context.Context, json.RawMessage) (any, error) { return make(chan int), nil },
	)
	if _, err := broken.Execute(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("broken result Execute() error = nil")
	}
}

func toolFixture(t *testing.T) (*tool.Registry, *workspace.Thread, *artifact.Catalog) {
	t.Helper()
	manager, err := workspace.NewManager(workspace.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}
	id, err := domain.NewThreadID()
	if err != nil {
		t.Fatalf("NewThreadID(): %v", err)
	}
	thread, err := manager.Open(id)
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	catalog := artifact.NewCatalog()
	registry := tool.NewRegistry()
	if err := (WorkspaceTools{
		Workspace: thread, Artifacts: catalog, Now: func() time.Time { return time.Unix(1, 0).UTC() },
	}).Register(registry); err != nil {
		t.Fatalf("Register(): %v", err)
	}
	return registry, thread, catalog
}

func executeOK(t *testing.T, registry *tool.Registry, name, arguments string) json.RawMessage {
	t.Helper()
	result := execute(t, registry, name, arguments)
	if result.IsError {
		t.Fatalf("Execute(%s) = %s", name, result.Output)
	}
	return result.Output
}

func execute(t *testing.T, registry *tool.Registry, name, arguments string) domain.ToolResult {
	t.Helper()
	result, err := registry.Execute(context.Background(), domain.ToolCall{
		ID: "call-1", Name: name, Arguments: json.RawMessage(arguments),
	})
	if err != nil {
		t.Fatalf("Execute(%s): %v", name, err)
	}
	return result
}

func containsJSON(value json.RawMessage, fragment string) bool {
	return json.Valid(value) && bytes.Contains(value, []byte(fragment))
}
