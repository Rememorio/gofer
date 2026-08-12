package skill

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSkillAllowsTool(t *testing.T) {
	t.Parallel()

	if !(Skill{Enabled: true}).AllowsTool("anything") {
		t.Fatal("omitted allowed-tools should permit every tool")
	}
	limited := Skill{Enabled: true, AllowedToolsSet: true, AllowedTools: []string{"read_file"}}
	if !limited.AllowsTool("read_file") || limited.AllowsTool("write_file") {
		t.Fatalf("limited tool policy was not enforced: %#v", limited)
	}
	limited.Enabled = false
	if limited.AllowsTool("read_file") {
		t.Fatal("disabled skill should not permit tools")
	}
	if (Skill{Enabled: true, AllowedToolsSet: true}).AllowsTool("read_file") {
		t.Fatal("explicit empty allowed-tools should deny every tool")
	}
}

func TestParserRejectsAdditionalDocumentAndNodeKinds(t *testing.T) {
	t.Parallel()

	decoder := yaml.NewDecoder(bytes.NewBufferString("first\n...\n---\nsecond\n"))
	var first any
	if err := decoder.Decode(&first); err != nil {
		t.Fatalf("Decode(first): %v", err)
	}
	if err := ensureSingleYAMLDocument(decoder); !errors.Is(err, ErrInvalidSkill) {
		t.Fatalf("ensureSingleYAMLDocument() error = %v, want ErrInvalidSkill", err)
	}
	for _, kind := range []yaml.Kind{yaml.DocumentNode, yaml.SequenceNode, yaml.AliasNode, 0} {
		var requirement secretMetadata
		if err := requirement.UnmarshalYAML(&yaml.Node{Kind: kind}); err == nil {
			t.Fatalf("UnmarshalYAML(kind=%d) succeeded", kind)
		}
	}
	if _, err := validateMetadata(frontmatter{Name: "x", Description: "valid"},
		Category("unknown"), "x", DefaultVirtualRoot); !errors.Is(err, ErrInvalidSkill) {
		t.Fatalf("validateMetadata(category) error = %v, want ErrInvalidSkill", err)
	}
}

func TestCatalogRejectsUnsafeLayoutAndOversizedDocument(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(*testing.T, string) Config
	}{
		{name: "skill at category root", setup: setupCategoryRootSkill},
		{name: "category is file", setup: setupFileCategory},
		{name: "skill document is directory", setup: setupDirectoryDocument},
		{name: "oversized document", setup: setupOversizedDocument},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			config := test.setup(t, root)
			catalog, err := NewCatalog(config)
			if err != nil {
				t.Fatalf("NewCatalog(): %v", err)
			}
			if err := catalog.Refresh(context.Background()); !errors.Is(err, ErrInvalidSkill) {
				t.Fatalf("Refresh() error = %v, want ErrInvalidSkill", err)
			}
		})
	}
}

func TestCatalogRejectsSymlinkCategory(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, string(CategoryPublic))); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	catalog, err := NewCatalog(Config{Root: root})
	if err != nil {
		t.Fatalf("NewCatalog(): %v", err)
	}
	if err := catalog.Refresh(context.Background()); !errors.Is(err, ErrInvalidSkill) {
		t.Fatalf("Refresh() error = %v, want ErrInvalidSkill", err)
	}
}

func TestEmptyCatalogAndProjectionValidation(t *testing.T) {
	t.Parallel()

	catalog := mustCatalog(t, Config{Root: t.TempDir()})
	if prompt := catalog.IndexPrompt(); prompt != "" {
		t.Fatalf("IndexPrompt() = %q, want empty", prompt)
	}
	if matches := catalog.Search("anything", 0); len(matches) != 0 {
		t.Fatalf("Search() = %#v, want empty", matches)
	}
	if err := catalog.Project(context.Background(), catalog.root); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Project(root) error = %v, want ErrInvalidConfig", err)
	}
	var nilCatalog *Catalog
	if err := nilCatalog.Project(context.Background(), t.TempDir()); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil Project() error = %v, want ErrInvalidConfig", err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if err := removeProjectedDirectory(missing); err != nil {
		t.Fatalf("removeProjectedDirectory(missing): %v", err)
	}
}

func setupCategoryRootSkill(t *testing.T, root string) Config {
	t.Helper()
	directory := filepath.Join(root, string(CategoryPublic))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatalf("MkdirAll(): %v", err)
	}
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"),
		[]byte(validDocument("root", "Root", "")), 0o600); err != nil {
		t.Fatalf("WriteFile(): %v", err)
	}
	return Config{Root: root}
}

func setupFileCategory(t *testing.T, root string) Config {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, string(CategoryPublic)), []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("WriteFile(): %v", err)
	}
	return Config{Root: root}
}

func setupDirectoryDocument(t *testing.T, root string) Config {
	t.Helper()
	directory := filepath.Join(root, string(CategoryPublic), "bad", "SKILL.md")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatalf("MkdirAll(): %v", err)
	}
	return Config{Root: root}
}

func setupOversizedDocument(t *testing.T, root string) Config {
	t.Helper()
	writeSkill(t, root, CategoryPublic, "large", validDocument("large", "Large", ""), nil)
	return Config{Root: root, MaxDocumentBytes: 8}
}

func TestProjectionSkipsNestedSymlinks(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, string(CategoryPublic)), 0o700); err != nil {
		t.Fatalf("MkdirAll(): %v", err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, string(CategoryPublic), "linked")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	catalog := mustCatalog(t, Config{Root: root})
	if got := catalog.List(false); len(got) != 0 {
		t.Fatalf("List() = %#v, want empty", got)
	}
	destination := filepath.Join(t.TempDir(), "view")
	t.Cleanup(func() { _ = removeProjectedDirectory(destination) })
	if err := catalog.Project(context.Background(), destination); err != nil {
		t.Fatalf("Project(): %v", err)
	}
	entries, err := os.ReadDir(destination)
	if err != nil {
		t.Fatalf("ReadDir(): %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("projection entries = %#v, want none", entries)
	}
	if info, err := os.Stat(destination); err != nil || info.Mode().Perm() != fs.FileMode(0o555) {
		t.Fatalf("projection mode = %v, %v", info, err)
	}
}
