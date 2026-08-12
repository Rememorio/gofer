package skill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/tool"
)

func TestCatalogRefreshLoadSearchAndState(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeSkill(t, root, CategoryPublic, "data-analysis", validDocument("data-analysis", "Analyze tabular data", `
license: MIT
allowed-tools: [read_file, grep]
required-secrets:
  - DATA_TOKEN
  - name: OPTIONAL_TOKEN
    optional: true
secrets-autonomous: false
compatibility: gofer
version: "1.0"
author: team
`), map[string]string{"scripts/run.sh": "#!/bin/sh\necho ok\n"})
	writeSkill(t, root, CategoryCustom, "team/report-writer", validDocument("report-writer", "Write polished reports", "allowed-tools: []\n"), nil)
	writeSkill(t, root, CategoryPublic, "data-analysis/evals/fixture", validDocument("ignored-fixture", "Must be ignored", ""), nil)
	state := NewMemoryState()
	if err := state.Set(context.Background(), Key{Category: CategoryCustom, Name: "report-writer"}, false); err != nil {
		t.Fatalf("state.Set(): %v", err)
	}
	catalog, err := NewCatalog(Config{Root: root, State: state})
	if err != nil {
		t.Fatalf("NewCatalog(): %v", err)
	}
	if err := catalog.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh(): %v", err)
	}
	assertCatalogMetadata(t, catalog)
	assertCatalogOperations(t, catalog)
}

func assertCatalogMetadata(t *testing.T, catalog *Catalog) {
	t.Helper()
	all := catalog.List(false)
	if names := skillNames(all); !reflect.DeepEqual(names, []string{"data-analysis", "report-writer"}) {
		t.Fatalf("skill names = %#v", names)
	}
	data := all[0]
	if !data.Enabled || data.AllowedToolsSet != true || len(data.AllowedTools) != 2 ||
		len(data.RequiredSecrets) != 2 || data.SecretsAutonomous {
		t.Fatalf("data skill = %#v", data)
	}
	report := all[1]
	if report.Enabled || !report.AllowedToolsSet || len(report.AllowedTools) != 0 {
		t.Fatalf("report skill = %#v", report)
	}
}

func assertCatalogOperations(t *testing.T, catalog *Catalog) {
	t.Helper()
	if _, err := catalog.Load(context.Background(), "report-writer"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Load(disabled) error = %v, want ErrDisabled", err)
	}
	if err := catalog.SetEnabled(context.Background(), "report-writer", true); err != nil {
		t.Fatalf("SetEnabled(): %v", err)
	}
	document, err := catalog.Load(context.Background(), "report-writer")
	if err != nil || !strings.Contains(document, "# Instructions") {
		t.Fatalf("Load() = %q, %v", document, err)
	}
	search := catalog.Search("tabular analysis", 5)
	if len(search) != 1 || search[0].Name != "data-analysis" {
		t.Fatalf("Search() = %#v", search)
	}
	if got := catalog.Search("data", 1); len(got) != 1 || got[0].Name != "data-analysis" {
		t.Fatalf("Search(limit) = %#v", got)
	}
	prompt := catalog.IndexPrompt()
	if !strings.Contains(prompt, "data-analysis, report-writer") || !strings.Contains(prompt, "describe_skill") {
		t.Fatalf("IndexPrompt() = %q", prompt)
	}
	if _, err := catalog.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(missing) error = %v, want ErrNotFound", err)
	}
	if err := catalog.SetEnabled(context.Background(), "missing", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetEnabled(missing) error = %v, want ErrNotFound", err)
	}
}

func TestCatalogProjectionIsAtomicAndReadOnly(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeSkill(t, root, CategoryPublic, "one", validDocument("one", "First skill", ""), map[string]string{
		"scripts/run.sh":      "#!/bin/sh\n",
		"references/guide.md": "guide",
	})
	writeSkill(t, root, CategoryCustom, "two", validDocument("two", "Second skill", ""), nil)
	catalog := mustCatalog(t, Config{Root: root})
	if err := catalog.SetEnabled(context.Background(), "two", false); err != nil {
		t.Fatalf("SetEnabled(): %v", err)
	}
	destination := filepath.Join(t.TempDir(), "view")
	t.Cleanup(func() { _ = removeProjectedDirectory(destination) })
	if err := catalog.Project(context.Background(), destination); err != nil {
		t.Fatalf("Project(): %v", err)
	}
	oneSkill := filepath.Join(destination, "public", "one", "SKILL.md")
	if data, err := os.ReadFile(oneSkill); err != nil || !strings.Contains(string(data), "First skill") {
		t.Fatalf("projected SKILL.md = %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(destination, "custom", "two", "SKILL.md")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("disabled projection Stat() error = %v, want not exist", err)
	}
	info, err := os.Stat(filepath.Join(destination, "public", "one", "scripts", "run.sh"))
	if err != nil {
		t.Fatalf("Stat(script): %v", err)
	}
	if info.Mode().Perm()&0o222 != 0 {
		t.Fatalf("projected script mode = %o, want read-only", info.Mode().Perm())
	}
	if err := catalog.SetEnabled(context.Background(), "one", false); err != nil {
		t.Fatalf("SetEnabled(one): %v", err)
	}
	if err := catalog.SetEnabled(context.Background(), "two", true); err != nil {
		t.Fatalf("SetEnabled(two): %v", err)
	}
	if err := catalog.Project(context.Background(), destination); err != nil {
		t.Fatalf("Project(replace): %v", err)
	}
	if _, err := os.Stat(oneSkill); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stale skill Stat() error = %v, want not exist", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "custom", "two", "SKILL.md")); err != nil {
		t.Fatalf("enabled skill missing: %v", err)
	}
}

func TestDescribeToolAndRendering(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeSkill(t, root, CategoryPublic, "chart", validDocument("chart", "Create charts & dashboards", "allowed-tools: [read_file]\n"), nil)
	catalog := mustCatalog(t, Config{Root: root})
	registry := tool.NewRegistry()
	if err := registry.Register(catalog.DescribeTool()); err != nil {
		t.Fatalf("Register(): %v", err)
	}
	if err := registry.Register(catalog.ReadTool()); err != nil {
		t.Fatalf("Register(read): %v", err)
	}
	result, err := registry.Execute(context.Background(), domain.ToolCall{
		ID: "1", Name: "describe_skill", Arguments: json.RawMessage(`{"name":"chart"}`),
	})
	if err != nil || result.IsError || !strings.Contains(string(result.Output), "/mnt/skills/public/chart/SKILL.md") {
		t.Fatalf("Execute() = %#v, %v", result, err)
	}
	read, err := registry.Execute(context.Background(), domain.ToolCall{
		ID: "2", Name: "read_skill", Arguments: json.RawMessage(`{"name":"chart"}`),
	})
	if err != nil || read.IsError || !strings.Contains(string(read.Output), "# Instructions") {
		t.Fatalf("ReadTool() = %#v, %v", read, err)
	}
	rendered := RenderDescription(catalog.List(true))
	if !strings.Contains(rendered, "Create charts &amp; dashboards") || !strings.Contains(rendered, "read_file") {
		t.Fatalf("RenderDescription() = %q", rendered)
	}
}

func TestCatalogRejectsInvalidPackages(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		document string
	}{
		{name: "no frontmatter", document: "# Body\n"},
		{name: "unclosed", document: "---\nname: x\n"},
		{name: "invalid YAML", document: "---\nname: [\n---\nBody"},
		{name: "unknown field", document: validDocument("x", "desc", "unknown: true\n")},
		{name: "bad name", document: validDocument("Bad_Name", "desc", "")},
		{name: "empty description", document: validDocument("x", "", "")},
		{name: "HTML description", document: validDocument("x", "has <tag>", "")},
		{name: "empty body", document: "---\nname: x\ndescription: desc\n---\n"},
		{name: "allowed scalar", document: validDocument("x", "desc", "allowed-tools: bash\n")},
		{name: "allowed empty", document: validDocument("x", "desc", "allowed-tools: [read_file, '']\n")},
		{name: "allowed duplicate", document: validDocument("x", "desc", "allowed-tools: [bash, bash]\n")},
		{name: "secrets scalar", document: validDocument("x", "desc", "required-secrets: TOKEN\n")},
		{name: "bad secret", document: validDocument("x", "desc", "required-secrets: [BAD-NAME]\n")},
		{name: "unknown secret field", document: validDocument("x", "desc", "required-secrets: [{name: TOKEN, typo: true}]\n")},
		{name: "duplicate secret", document: validDocument("x", "desc", "required-secrets: [TOKEN, TOKEN]\n")},
		{name: "autonomous scalar", document: validDocument("x", "desc", "secrets-autonomous: nope\n")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeSkill(t, root, CategoryPublic, "x", test.document, nil)
			catalog, err := NewCatalog(Config{Root: root})
			if err != nil {
				t.Fatalf("NewCatalog(): %v", err)
			}
			if err := catalog.Refresh(context.Background()); !errors.Is(err, ErrInvalidSkill) {
				t.Fatalf("Refresh() error = %v, want ErrInvalidSkill", err)
			}
		})
	}
}

func TestCatalogRejectsDuplicatesAndChangedIdentity(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeSkill(t, root, CategoryPublic, "one", validDocument("same", "Public", ""), nil)
	writeSkill(t, root, CategoryCustom, "two", validDocument("same", "Custom", ""), nil)
	catalog, err := NewCatalog(Config{Root: root})
	if err != nil {
		t.Fatalf("NewCatalog(): %v", err)
	}
	if err := catalog.Refresh(context.Background()); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("Refresh(duplicate) error = %v, want ErrDuplicate", err)
	}

	root = t.TempDir()
	filename := writeSkill(t, root, CategoryPublic, "stable", validDocument("stable", "Stable", ""), nil)
	catalog = mustCatalog(t, Config{Root: root})
	if err := os.WriteFile(filename, []byte(validDocument("changed", "Changed", "")), 0o600); err != nil {
		t.Fatalf("WriteFile(changed): %v", err)
	}
	if _, err := catalog.Load(context.Background(), "stable"); !errors.Is(err, ErrInvalidSkill) {
		t.Fatalf("Load(changed) error = %v, want ErrInvalidSkill", err)
	}
}

func TestCatalogConfigContextAndMemoryState(t *testing.T) {
	t.Parallel()

	invalid := []Config{
		{}, {Root: t.TempDir(), VirtualRoot: "relative"},
		{Root: t.TempDir(), VirtualRoot: "/"}, {Root: t.TempDir(), MaxDocumentBytes: -1},
	}
	for _, config := range invalid {
		if _, err := NewCatalog(config); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("NewCatalog(%#v) error = %v, want ErrInvalidConfig", config, err)
		}
	}
	var catalog *Catalog
	if err := catalog.Refresh(context.Background()); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil Refresh() error = %v, want ErrInvalidConfig", err)
	}
	if _, err := catalog.Get("x"); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil Get() error = %v, want ErrInvalidConfig", err)
	}
	if _, err := catalog.Load(context.Background(), "x"); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil Load() error = %v, want ErrInvalidConfig", err)
	}
	if err := catalog.SetEnabled(context.Background(), "x", true); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil SetEnabled() error = %v, want ErrInvalidConfig", err)
	}
	if got := catalog.List(true); got != nil {
		t.Fatalf("nil List() = %#v", got)
	}
	state := NewMemoryState()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := state.Get(ctx, Key{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("state.Get(cancelled) error = %v", err)
	}
	if err := state.Set(ctx, Key{}, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("state.Set(cancelled) error = %v", err)
	}
}

func TestProjectionRejectsSymlinksAndSizeLimit(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeSkill(t, root, CategoryPublic, "linked", validDocument("linked", "Linked", ""), nil)
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatalf("WriteFile(secret): %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "public", "linked", "secret")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	catalog := mustCatalog(t, Config{Root: root})
	if err := catalog.Project(context.Background(), filepath.Join(t.TempDir(), "view")); !errors.Is(err, ErrInvalidSkill) {
		t.Fatalf("Project(symlink) error = %v, want ErrInvalidSkill", err)
	}

	root = t.TempDir()
	writeSkill(t, root, CategoryPublic, "large", validDocument("large", "Large", ""), map[string]string{"data": strings.Repeat("x", 100)})
	catalog = mustCatalog(t, Config{Root: root, MaxPackageBytes: 50})
	if err := catalog.Project(context.Background(), filepath.Join(t.TempDir(), "view")); !errors.Is(err, ErrInvalidSkill) {
		t.Fatalf("Project(large) error = %v, want ErrInvalidSkill", err)
	}
}

func mustCatalog(t *testing.T, config Config) *Catalog {
	t.Helper()
	catalog, err := NewCatalog(config)
	if err != nil {
		t.Fatalf("NewCatalog(): %v", err)
	}
	if err := catalog.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh(): %v", err)
	}
	return catalog
}

func writeSkill(
	t *testing.T,
	root string,
	category Category,
	relative string,
	document string,
	resources map[string]string,
) string {
	t.Helper()
	directory := filepath.Join(root, string(category), filepath.FromSlash(relative))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatalf("MkdirAll(): %v", err)
	}
	filename := filepath.Join(directory, "SKILL.md")
	if err := os.WriteFile(filename, []byte(document), 0o600); err != nil {
		t.Fatalf("WriteFile(SKILL.md): %v", err)
	}
	for name, content := range resources {
		resource := filepath.Join(directory, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(resource), 0o700); err != nil {
			t.Fatalf("MkdirAll(resource): %v", err)
		}
		mode := fs.FileMode(0o600)
		if strings.HasSuffix(name, ".sh") {
			mode = 0o700
		}
		if err := os.WriteFile(resource, []byte(content), mode); err != nil {
			t.Fatalf("WriteFile(resource): %v", err)
		}
	}
	return filename
}

func validDocument(name, description, extras string) string {
	return fmt.Sprintf("---\nname: %s\ndescription: %q\n%s---\n# Instructions\n\nDo the work.\n", name, description, extras)
}

func skillNames(skills []Skill) []string {
	names := make([]string, len(skills))
	for index, candidate := range skills {
		names[index] = candidate.Name
	}
	return names
}
