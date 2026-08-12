package workspace

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/Rememorio/gofer/internal/domain"
)

func TestWorkspaceFileLifecycle(t *testing.T) {
	t.Parallel()

	workspace := newTestWorkspace(t, Config{})
	defer func() { _ = workspace.Close() }()
	file := WorkspaceRoot + "/notes/report.txt"
	if err := workspace.WriteFile(file, []byte("one\ntwo\nthree\n"), false); err != nil {
		t.Fatalf("WriteFile(): %v", err)
	}
	if err := workspace.WriteFile(file, []byte("four\n"), true); err != nil {
		t.Fatalf("WriteFile(append): %v", err)
	}
	result, err := workspace.ReadFile(file, ReadOptions{StartLine: 2, EndLine: 3})
	if err != nil {
		t.Fatalf("ReadFile(): %v", err)
	}
	if result.Content != "two\nthree" || result.StartLine != 2 || result.EndLine != 3 || result.TotalLines != 4 {
		t.Fatalf("ReadFile() = %#v", result)
	}
	if err := workspace.Replace(file, "two", "TWO", false); err != nil {
		t.Fatalf("Replace(): %v", err)
	}
	if err := workspace.Replace(file, "missing", "x", false); !errors.Is(err, ErrNoMatch) {
		t.Fatalf("Replace(missing) error = %v, want ErrNoMatch", err)
	}
	if err := workspace.Replace(file, "o", "x", false); !errors.Is(err, ErrAmbiguousMatch) {
		t.Fatalf("Replace(ambiguous) error = %v, want ErrAmbiguousMatch", err)
	}
	if err := workspace.Replace(file, "o", "O", true); err != nil {
		t.Fatalf("Replace(all): %v", err)
	}
	all, err := workspace.ReadFile(file, ReadOptions{})
	if err != nil {
		t.Fatalf("ReadFile(all): %v", err)
	}
	if all.Content != "One\nTWO\nthree\nfOur" {
		t.Fatalf("content = %q", all.Content)
	}
	entry, err := workspace.Inspect(file)
	if err != nil || entry.Path != file || entry.Directory || entry.Size == 0 {
		t.Fatalf("Inspect() = %#v, %v", entry, err)
	}
	reader, err := workspace.OpenFile(file)
	if err != nil {
		t.Fatalf("OpenFile(): %v", err)
	}
	data, _ := io.ReadAll(reader)
	_ = reader.Close()
	if string(data) != "One\nTWO\nthree\nfOur\n" {
		t.Fatalf("OpenFile content = %q", data)
	}
}

func TestWorkspaceRejectsUnsafeAndOversizedOperations(t *testing.T) {
	t.Parallel()

	workspace := newTestWorkspace(t, Config{MaxReadBytes: 8, MaxWriteBytes: 8, MaxUploadBytes: 8})
	defer func() { _ = workspace.Close() }()
	unsafe := []string{
		"relative", "/mnt/user-data-other/file", VirtualRoot + "/../secret",
		VirtualRoot + "/workspace/../../secret", VirtualRoot + `\workspace\file`,
	}
	for _, value := range unsafe {
		if _, err := workspace.Inspect(value); !errors.Is(err, ErrInvalidPath) {
			t.Fatalf("Inspect(%q) error = %v, want ErrInvalidPath", value, err)
		}
	}
	if err := workspace.WriteFile(UploadsRoot+"/user.txt", []byte("x"), false); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("WriteFile(upload) error = %v, want ErrReadOnly", err)
	}
	if err := workspace.WriteFile(VirtualRoot+"/other/file", []byte("x"), false); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("WriteFile(other) error = %v, want ErrInvalidPath", err)
	}
	if err := workspace.WriteFile(WorkspaceRoot+"/large", []byte("123456789"), false); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("WriteFile(large) error = %v, want ErrTooLarge", err)
	}
	if _, err := workspace.PutUpload("large.bin", bytes.NewReader([]byte("123456789"))); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("PutUpload(large) error = %v, want ErrTooLarge", err)
	}
	for _, filename := range []string{"", ".", "..", "../x", `dir\x`} {
		if _, err := workspace.PutUpload(filename, strings.NewReader("x")); !errors.Is(err, ErrInvalidPath) {
			t.Fatalf("PutUpload(%q) error = %v, want ErrInvalidPath", filename, err)
		}
	}
	if err := workspace.WriteFile(WorkspaceRoot+"/read-large", []byte("12345678"), false); err != nil {
		t.Fatalf("WriteFile(read-large): %v", err)
	}
	if err := workspace.WriteFile(WorkspaceRoot+"/read-large", []byte("9"), true); err != nil {
		t.Fatalf("WriteFile(append): %v", err)
	}
	if _, err := workspace.ReadFile(WorkspaceRoot+"/read-large", ReadOptions{}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("ReadFile(large) error = %v, want ErrTooLarge", err)
	}
	if _, err := workspace.ReadFile(WorkspaceRoot+"/read-large", ReadOptions{EndLine: 1}); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("ReadFile(range) error = %v, want ErrInvalidPath", err)
	}
	if _, err := workspace.ReadFile(WorkspaceRoot, ReadOptions{}); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("ReadFile(directory) error = %v, want ErrNotRegular", err)
	}
}

func TestWorkspaceRootBlocksSymlinkEscape(t *testing.T) {
	t.Parallel()

	workspace := newTestWorkspace(t, Config{})
	defer func() { _ = workspace.Close() }()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	link := filepath.Join(workspace.hostRoot, "workspace", "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	escape := WorkspaceRoot + "/escape/secret.txt"
	if _, err := workspace.ReadFile(escape, ReadOptions{}); err == nil {
		t.Fatal("ReadFile(symlink escape) error = nil")
	}
	if err := workspace.WriteFile(escape, []byte("changed"), false); err == nil {
		t.Fatal("WriteFile(symlink escape) error = nil")
	}
	data, err := os.ReadFile(secret)
	if err != nil || string(data) != "secret" {
		t.Fatalf("outside secret = %q, %v", data, err)
	}
}

func TestWorkspaceUploadsAreCollisionFree(t *testing.T) {
	t.Parallel()

	workspace := newTestWorkspace(t, Config{})
	defer func() { _ = workspace.Close() }()
	first, err := workspace.PutUpload("report.txt", strings.NewReader("first"))
	if err != nil {
		t.Fatalf("PutUpload(first): %v", err)
	}
	second, err := workspace.PutUpload("report.txt", strings.NewReader("second"))
	if err != nil {
		t.Fatalf("PutUpload(second): %v", err)
	}
	if first.Path != UploadsRoot+"/report.txt" || second.Path != UploadsRoot+"/report-1.txt" {
		t.Fatalf("upload paths = %q, %q", first.Path, second.Path)
	}
	if err := workspace.RemoveUpload("report.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.Inspect(UploadsRoot + "/report.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Inspect(deleted upload) = %v", err)
	}
	if err := workspace.RemoveUpload("../bad"); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("RemoveUpload(invalid) = %v", err)
	}
	if err := workspace.RemoveUpload("missing.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("RemoveUpload(missing) = %v", err)
	}
}

func TestManagerRemovesOnlyValidatedThreadWorkspace(t *testing.T) {
	t.Parallel()
	manager, err := NewManager(Config{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	threadID, _ := domain.NewThreadID()
	thread, err := manager.Open(threadID)
	if err != nil {
		t.Fatal(err)
	}
	if err = thread.WriteFile(WorkspaceRoot+"/file.txt", []byte("content"), false); err != nil {
		t.Fatal(err)
	}
	_ = thread.Close()
	if err = manager.Remove(threadID); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(manager.root, "threads", string(threadID))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("thread workspace remains: %v", err)
	}
	if err = manager.Remove(domain.ThreadID("bad")); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Remove(invalid) = %v", err)
	}
}

func TestWorkspaceConcurrentUploadsAreCollisionFree(t *testing.T) {
	t.Parallel()

	manager, err := NewManager(Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}
	threadID, err := domain.NewThreadID()
	if err != nil {
		t.Fatalf("NewThreadID(): %v", err)
	}
	first, err := manager.Open(threadID)
	if err != nil {
		t.Fatalf("Open(first): %v", err)
	}
	defer func() { _ = first.Close() }()
	second, err := manager.Open(threadID)
	if err != nil {
		t.Fatalf("Open(second): %v", err)
	}
	defer func() { _ = second.Close() }()
	workspaces := []*Thread{first, second}
	paths := make(chan string, len(workspaces))
	var wait sync.WaitGroup
	for index, thread := range workspaces {
		wait.Add(1)
		go func(index int, thread *Thread) {
			defer wait.Done()
			entry, err := thread.PutUpload("same.txt", strings.NewReader(fmt.Sprintf("%d", index)))
			if err != nil {
				t.Errorf("PutUpload(%d): %v", index, err)
				return
			}
			paths <- entry.Path
		}(index, thread)
	}
	wait.Wait()
	close(paths)
	got := make([]string, 0, len(workspaces))
	for value := range paths {
		got = append(got, value)
	}
	sort.Strings(got)
	want := []string{UploadsRoot + "/same-1.txt", UploadsRoot + "/same.txt"}
	if !equalStrings(got, want) {
		t.Fatalf("upload paths = %#v, want %#v", got, want)
	}
}

func TestWorkspaceListGlobAndGrep(t *testing.T) {
	t.Parallel()

	workspace := newTestWorkspace(t, Config{MaxReadBytes: 64})
	defer func() { _ = workspace.Close() }()
	files := map[string]string{
		WorkspaceRoot + "/root.go":             "package root\n// Needle\n",
		WorkspaceRoot + "/src/main.go":         "package main\n// needle\n",
		WorkspaceRoot + "/src/deep/helper.txt": "Needle helper\n",
		WorkspaceRoot + "/node_modules/x.go":   "needle ignored\n",
		WorkspaceRoot + "/binary.dat":          "a\x00needle\n",
	}
	for name, content := range files {
		if err := workspace.WriteFile(name, []byte(content), false); err != nil {
			t.Fatalf("WriteFile(%s): %v", name, err)
		}
	}
	list, err := workspace.List(WorkspaceRoot, ListOptions{MaxDepth: 1, MaxResults: 20})
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	paths := entryPaths(list.Entries)
	if contains(paths, WorkspaceRoot+"/src/deep/helper.txt") || contains(paths, WorkspaceRoot+"/node_modules") {
		t.Fatalf("List paths = %#v", paths)
	}
	globbed, err := workspace.Glob(WorkspaceRoot, "**/*.go", GlobOptions{})
	if err != nil {
		t.Fatalf("Glob(): %v", err)
	}
	if got := globbed.Paths; !equalStrings(got, []string{WorkspaceRoot + "/root.go", WorkspaceRoot + "/src/main.go"}) {
		t.Fatalf("Glob paths = %#v", got)
	}
	grep, err := workspace.Grep(WorkspaceRoot, "needle", GrepOptions{Glob: "**/*", MaxResults: 10})
	if err != nil {
		t.Fatalf("Grep(): %v", err)
	}
	if len(grep.Matches) != 3 || grep.Matches[0].LineNumber < 1 {
		t.Fatalf("Grep matches = %#v", grep.Matches)
	}
	literal, err := workspace.Grep(WorkspaceRoot+"/root.go", "Need", GrepOptions{Literal: true, CaseSensitive: true})
	if err != nil || len(literal.Matches) != 1 {
		t.Fatalf("Grep(file) = %#v, %v", literal, err)
	}
}

func TestWorkspaceConcurrentAppend(t *testing.T) {
	t.Parallel()

	workspace := newTestWorkspace(t, Config{})
	defer func() { _ = workspace.Close() }()
	file := OutputsRoot + "/events.log"
	const writers = 20
	var wait sync.WaitGroup
	for index := 0; index < writers; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			if err := workspace.WriteFile(file, []byte(fmt.Sprintf("%02d\n", index)), true); err != nil {
				t.Errorf("WriteFile(%d): %v", index, err)
			}
		}(index)
	}
	wait.Wait()
	result, err := workspace.ReadFile(file, ReadOptions{})
	if err != nil {
		t.Fatalf("ReadFile(): %v", err)
	}
	lines := strings.Split(result.Content, "\n")
	sort.Strings(lines)
	if len(lines) != writers || lines[0] != "00" || lines[writers-1] != "19" {
		t.Fatalf("lines = %#v", lines)
	}
}

func TestManagerAndCloseValidation(t *testing.T) {
	t.Parallel()

	if _, err := NewManager(Config{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("NewManager(empty) error = %v, want ErrInvalidConfig", err)
	}
	if _, err := NewManager(Config{Root: t.TempDir(), MaxReadBytes: -1}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("NewManager(negative) error = %v, want ErrInvalidConfig", err)
	}
	var manager *Manager
	if _, err := manager.Open("bad"); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil Open() error = %v, want ErrInvalidConfig", err)
	}
	manager, err := NewManager(Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}
	if _, err := manager.Open("bad"); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("Open(bad ID) error = %v, want ErrInvalidConfig", err)
	}
	workspace := newTestWorkspaceFromManager(t, manager)
	if workspace.ID() == "" {
		t.Fatal("ID() is empty")
	}
	if err := workspace.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	if err := workspace.Close(); err != nil {
		t.Fatalf("Close(second): %v", err)
	}
	var nilWorkspace *Thread
	if err := nilWorkspace.Close(); err != nil {
		t.Fatalf("nil Close(): %v", err)
	}
}

func TestWorkspaceSearchAndRangeValidation(t *testing.T) {
	t.Parallel()

	workspace := newTestWorkspace(t, Config{MaxReadBytes: 16, MaxWriteBytes: 32})
	defer func() { _ = workspace.Close() }()
	if err := workspace.WriteFile(WorkspaceRoot+"/a.txt", []byte("first\nsecond\n"), false); err != nil {
		t.Fatalf("WriteFile(): %v", err)
	}
	root, err := workspace.Inspect(VirtualRoot)
	if err != nil || root.Path != VirtualRoot || !root.Directory {
		t.Fatalf("Inspect(root) = %#v, %v", root, err)
	}
	beyond, err := workspace.ReadFile(WorkspaceRoot+"/a.txt", ReadOptions{StartLine: 10})
	if err != nil || beyond.Content != "" || beyond.TotalLines != 2 {
		t.Fatalf("ReadFile(beyond) = %#v, %v", beyond, err)
	}
	if err := workspace.Replace(WorkspaceRoot+"/a.txt", "", "x", false); !errors.Is(err, ErrNoMatch) {
		t.Fatalf("Replace(empty) error = %v, want ErrNoMatch", err)
	}
	if err := workspace.Replace(WorkspaceRoot+"/a.txt", "first", strings.Repeat("x", 40), false); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Replace(large) error = %v, want ErrTooLarge", err)
	}
	invalidLists := []ListOptions{{MaxDepth: -1}, {MaxDepth: 21}, {MaxResults: 1001}}
	for _, options := range invalidLists {
		if _, err := workspace.List(WorkspaceRoot, options); err == nil {
			t.Fatalf("List(%#v) error = nil", options)
		}
	}
	if _, err := workspace.Glob(WorkspaceRoot, "", GlobOptions{}); err == nil {
		t.Fatal("Glob(empty) error = nil")
	}
	if _, err := workspace.Glob(WorkspaceRoot, "[", GlobOptions{}); err == nil {
		t.Fatal("Glob(invalid) error = nil")
	}
	if _, err := workspace.Glob(WorkspaceRoot, "*", GlobOptions{MaxResults: 1001}); err == nil {
		t.Fatal("Glob(limit) error = nil")
	}
	withDirectories, err := workspace.Glob(VirtualRoot, "**/workspace", GlobOptions{IncludeDirectories: true})
	if err != nil || len(withDirectories.Paths) != 1 {
		t.Fatalf("Glob(directories) = %#v, %v", withDirectories, err)
	}
	if _, err := workspace.Grep(WorkspaceRoot, "", GrepOptions{}); err == nil {
		t.Fatal("Grep(empty) error = nil")
	}
	if _, err := workspace.Grep(WorkspaceRoot, "[", GrepOptions{}); err == nil {
		t.Fatal("Grep(regex) error = nil")
	}
	if _, err := workspace.Grep(WorkspaceRoot, "x", GrepOptions{MaxResults: 1001}); err == nil {
		t.Fatal("Grep(limit) error = nil")
	}
}

func TestWorkspaceResultTruncation(t *testing.T) {
	t.Parallel()

	workspace := newTestWorkspace(t, Config{})
	defer func() { _ = workspace.Close() }()
	longLine := strings.Repeat("x", 220) + " needle"
	for index, content := range []string{"needle one", "needle two", longLine} {
		name := fmt.Sprintf("%s/%d.txt", WorkspaceRoot, index)
		if err := workspace.WriteFile(name, []byte(content), false); err != nil {
			t.Fatalf("WriteFile(%d): %v", index, err)
		}
	}
	listed, err := workspace.List(WorkspaceRoot, ListOptions{MaxDepth: 1, MaxResults: 1})
	if err != nil || !listed.Truncated || len(listed.Entries) != 1 {
		t.Fatalf("List(truncated) = %#v, %v", listed, err)
	}
	globbed, err := workspace.Glob(WorkspaceRoot, "*.txt", GlobOptions{MaxResults: 1})
	if err != nil || !globbed.Truncated || len(globbed.Paths) != 1 {
		t.Fatalf("Glob(truncated) = %#v, %v", globbed, err)
	}
	grep, err := workspace.Grep(WorkspaceRoot, "needle", GrepOptions{MaxResults: 2})
	if err != nil || !grep.Truncated || len(grep.Matches) != 2 {
		t.Fatalf("Grep(truncated) = %#v, %v", grep, err)
	}
	long, err := workspace.Grep(WorkspaceRoot+"/2.txt", "needle", GrepOptions{})
	if err != nil || len(long.Matches) != 1 || len([]rune(long.Matches[0].Line)) != 200 {
		t.Fatalf("Grep(long line) = %#v, %v", long, err)
	}
}

func TestExecutionMounts(t *testing.T) {
	t.Parallel()

	workspace := newTestWorkspace(t, Config{})
	defer func() { _ = workspace.Close() }()
	mounts := workspace.ExecutionMounts()
	if len(mounts) != 3 {
		t.Fatalf("ExecutionMounts() = %#v", mounts)
	}
	if mounts[0].VirtualPath != WorkspaceRoot || mounts[0].ReadOnly ||
		mounts[1].VirtualPath != UploadsRoot || !mounts[1].ReadOnly ||
		mounts[2].VirtualPath != OutputsRoot || mounts[2].ReadOnly {
		t.Fatalf("ExecutionMounts() = %#v", mounts)
	}
	for _, mount := range mounts {
		if !filepath.IsAbs(mount.HostPath) {
			t.Fatalf("host path is not absolute: %q", mount.HostPath)
		}
	}
	var nilWorkspace *Thread
	if mounts := nilWorkspace.ExecutionMounts(); mounts != nil {
		t.Fatalf("nil ExecutionMounts() = %#v", mounts)
	}
}

func TestCreateBinaryOutput(t *testing.T) {
	t.Parallel()

	workspace := newTestWorkspace(t, Config{MaxUploadBytes: 4})
	defer func() { _ = workspace.Close() }()
	name := OutputsRoot + "/captures/page.png"
	if err := workspace.CreateOutput(name, []byte{0, 1, 2}); err != nil {
		t.Fatalf("CreateOutput(): %v", err)
	}
	if err := workspace.CreateOutput(name, []byte("new")); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("CreateOutput(existing) error = %v, want fs.ErrExist", err)
	}
	if err := workspace.CreateOutput(WorkspaceRoot+"/bad", []byte("x")); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("CreateOutput(workspace) error = %v", err)
	}
	if err := workspace.CreateOutput(OutputsRoot, []byte("x")); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("CreateOutput(root) error = %v", err)
	}
	if err := workspace.CreateOutput(OutputsRoot+"/large", []byte("12345")); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("CreateOutput(large) error = %v", err)
	}
}

func newTestWorkspace(t *testing.T, config Config) *Thread {
	t.Helper()
	config.Root = t.TempDir()
	manager, err := NewManager(config)
	if err != nil {
		t.Fatalf("NewManager(): %v", err)
	}
	return newTestWorkspaceFromManager(t, manager)
}

func newTestWorkspaceFromManager(t *testing.T, manager *Manager) *Thread {
	t.Helper()
	threadID, err := domain.NewThreadID()
	if err != nil {
		t.Fatalf("NewThreadID(): %v", err)
	}
	workspace, err := manager.Open(threadID)
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	return workspace
}

func entryPaths(entries []Entry) []string {
	paths := make([]string, len(entries))
	for index, entry := range entries {
		paths[index] = entry.Path
	}
	return paths
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
