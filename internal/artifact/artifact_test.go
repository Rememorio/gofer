package artifact

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
	"github.com/Rememorio/gofer/internal/workspace"
)

func TestCatalogPresentListAndOpen(t *testing.T) {
	t.Parallel()

	thread := testWorkspace(t)
	defer func() { _ = thread.Close() }()
	paths := []string{workspace.OutputsRoot + "/z.txt", workspace.OutputsRoot + "/a.json"}
	for _, name := range paths {
		if err := thread.WriteFile(name, []byte("content"), false); err != nil {
			t.Fatalf("WriteFile(%s): %v", name, err)
		}
	}
	catalog := NewCatalog()
	at := time.Now().UTC()
	presented, err := catalog.Present(context.Background(), thread, []string{paths[0], paths[0], paths[1]}, at)
	if err != nil {
		t.Fatalf("Present(): %v", err)
	}
	if len(presented) != 2 || presented[0].Path != paths[0] || presented[1].MediaType != "application/json" {
		t.Fatalf("presented = %#v", presented)
	}
	listed := catalog.List(thread.ID())
	if len(listed) != 2 || listed[0].Path != paths[1] || listed[1].Path != paths[0] {
		t.Fatalf("List() = %#v", listed)
	}
	catalog.RemoveThread(thread.ID())
	if listed = catalog.List(thread.ID()); len(listed) != 0 {
		t.Fatalf("List(after remove) = %#v", listed)
	}
	if _, err = catalog.Present(context.Background(), thread, paths, at); err != nil {
		t.Fatal(err)
	}
	reader, metadata, err := catalog.Open(thread, paths[0])
	if err != nil {
		t.Fatalf("Open(): %v", err)
	}
	data, _ := io.ReadAll(reader)
	_ = reader.Close()
	if string(data) != "content" || metadata.Path != paths[0] {
		t.Fatalf("Open() = %q, %#v", data, metadata)
	}
}

func TestCatalogValidation(t *testing.T) {
	t.Parallel()

	thread := testWorkspace(t)
	defer func() { _ = thread.Close() }()
	catalog := NewCatalog()
	at := time.Now().UTC()
	if _, err := catalog.Present(context.Background(), thread, nil, at); !errors.Is(err, ErrInvalidArtifact) {
		t.Fatalf("Present(empty) error = %v, want ErrInvalidArtifact", err)
	}
	if _, err := catalog.Present(context.Background(), thread, []string{workspace.WorkspaceRoot + "/x"}, at); !errors.Is(err, ErrInvalidArtifact) {
		t.Fatalf("Present(workspace) error = %v, want ErrInvalidArtifact", err)
	}
	if _, err := catalog.Present(context.Background(), thread, []string{workspace.OutputsRoot}, at); !errors.Is(err, ErrInvalidArtifact) {
		t.Fatalf("Present(directory) error = %v, want ErrInvalidArtifact", err)
	}
	internalPath := workspace.OutputsRoot + "/" + workspace.ProcessOutputDirectory + "/raw.txt"
	if err := thread.WriteFile(internalPath, []byte("private"), false); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Present(context.Background(), thread, []string{internalPath}, at); !errors.Is(err, ErrInvalidArtifact) {
		t.Fatalf("Present(internal) error = %v, want ErrInvalidArtifact", err)
	}
	if _, err := Inspect(thread, internalPath); !errors.Is(err, ErrInvalidArtifact) {
		t.Fatalf("Inspect(internal) error = %v, want ErrInvalidArtifact", err)
	}
	if _, _, err := OpenFile(thread, internalPath); !errors.Is(err, ErrInvalidArtifact) {
		t.Fatalf("OpenFile(internal) error = %v, want ErrInvalidArtifact", err)
	}
	if _, _, err := catalog.Open(thread, internalPath); !errors.Is(err, ErrInvalidArtifact) {
		t.Fatalf("Open(internal) error = %v, want ErrInvalidArtifact", err)
	}
	if _, _, err := catalog.Open(thread, workspace.OutputsRoot+"/missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open(missing) error = %v, want ErrNotFound", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := catalog.Present(ctx, thread, []string{"x"}, at); !errors.Is(err, context.Canceled) {
		t.Fatalf("Present(cancelled) error = %v, want context.Canceled", err)
	}
	var nilCatalog *Catalog
	if got := nilCatalog.List(thread.ID()); got != nil {
		t.Fatalf("nil List() = %#v", got)
	}
	if _, err := nilCatalog.Present(context.Background(), thread, []string{"x"}, at); !errors.Is(err, ErrInvalidArtifact) {
		t.Fatalf("nil Present() error = %v, want ErrInvalidArtifact", err)
	}
	if _, _, err := nilCatalog.Open(thread, "x"); !errors.Is(err, ErrInvalidArtifact) {
		t.Fatalf("nil Open() error = %v, want ErrInvalidArtifact", err)
	}
	metadata, err := Inspect(thread, workspace.OutputsRoot+"/binary")
	if err != nil || metadata.Size != 2 || metadata.MediaType != "application/octet-stream" {
		t.Fatalf("Inspect() = %#v, %v", metadata, err)
	}
	reader, opened, err := OpenFile(thread, workspace.OutputsRoot+"/binary")
	if err != nil || opened.Path != metadata.Path {
		t.Fatalf("OpenFile() = %#v, %v", opened, err)
	}
	_ = reader.Close()
	if _, err = Inspect(thread, workspace.WorkspaceRoot+"/binary"); !errors.Is(err, ErrInvalidArtifact) {
		t.Fatalf("Inspect(private) = %v", err)
	}
}

func testWorkspace(t *testing.T) *workspace.Thread {
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
	if err := thread.WriteFile(workspace.OutputsRoot+"/binary", []byte(strings.Repeat("x", 2)), false); err != nil {
		t.Fatalf("WriteFile(binary): %v", err)
	}
	return thread
}
