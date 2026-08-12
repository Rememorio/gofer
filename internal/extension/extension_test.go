package extension

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type fakeExtension struct {
	manifest           Manifest
	started, closed    *[]string
	startErr, closeErr error
}

func (extension *fakeExtension) Manifest() Manifest { return extension.manifest }
func (extension *fakeExtension) Start(context.Context) error {
	*extension.started = append(*extension.started, extension.manifest.Name)
	return extension.startErr
}
func (extension *fakeExtension) Close() error {
	*extension.closed = append(*extension.closed, extension.manifest.Name)
	return extension.closeErr
}

func TestRegistryDependencyOrderAndReverseClose(t *testing.T) {
	t.Parallel()
	var started, closed []string
	registry := NewRegistry()
	extensions := []*fakeExtension{{manifest: manifest("api", "store"), started: &started, closed: &closed}, {manifest: manifest("store"), started: &started, closed: &closed}, {manifest: manifest("channel", "api"), started: &started, closed: &closed}}
	for _, extension := range extensions {
		if err := registry.Register(extension); err != nil {
			t.Fatal(err)
		}
	}
	if err := registry.StartAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(started, []string{"store", "api", "channel"}) {
		t.Fatalf("started=%#v", started)
	}
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(closed, []string{"channel", "api", "store"}) {
		t.Fatalf("closed=%#v", closed)
	}
	statuses := registry.Status()
	if len(statuses) != 3 || statuses[0].Name != "api" || statuses[0].State != Stopped {
		t.Fatalf("statuses=%#v", statuses)
	}
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryRollbackOnStartupFailure(t *testing.T) {
	t.Parallel()
	var started, closed []string
	registry := NewRegistry()
	store := &fakeExtension{manifest: manifest("store"), started: &started, closed: &closed}
	api := &fakeExtension{manifest: manifest("api", "store"), started: &started, closed: &closed, startErr: errors.New("boom")}
	_ = registry.Register(api)
	_ = registry.Register(store)
	if err := registry.StartAll(context.Background()); err == nil {
		t.Fatal("StartAll succeeded")
	}
	if !reflect.DeepEqual(closed, []string{"store"}) {
		t.Fatalf("closed=%#v", closed)
	}
	statuses := registry.Status()
	if statuses[0].State != Failed || statuses[1].State != Stopped {
		t.Fatalf("statuses=%#v", statuses)
	}
}

func TestRegistryRejectsInvalidGraphsAndLifecycle(t *testing.T) {
	t.Parallel()
	var started, closed []string
	makeExtension := func(value Manifest) *fakeExtension {
		return &fakeExtension{manifest: value, started: &started, closed: &closed}
	}
	registry := NewRegistry()
	if err := registry.Register(nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil=%v", err)
	}
	invalid := []Manifest{{}, {Name: "Bad", Version: "1", Description: "x"}, {Name: "x", Version: "", Description: "x"}, {Name: "x", Version: "1", Description: "x", Requires: []string{"x"}}, {Name: "x", Version: "1", Description: "x", Capabilities: []string{"Bad"}}}
	for _, manifest := range invalid {
		if err := registry.Register(makeExtension(manifest)); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Register(%#v)=%v", manifest, err)
		}
	}
	valid := makeExtension(manifest("x"))
	if err := registry.Register(valid); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(valid); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate=%v", err)
	}
	if err := registry.StartAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(makeExtension(manifest("y"))); err == nil {
		t.Fatal("late register")
	}
	if err := registry.StartAll(context.Background()); err == nil {
		t.Fatal("second start")
	}
}

func TestRegistryMissingDependencyAndCycle(t *testing.T) {
	t.Parallel()
	var started, closed []string
	missing := NewRegistry()
	_ = missing.Register(&fakeExtension{manifest: manifest("a", "missing"), started: &started, closed: &closed})
	if err := missing.StartAll(context.Background()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing=%v", err)
	}
	cycle := NewRegistry()
	_ = cycle.Register(&fakeExtension{manifest: manifest("a", "b"), started: &started, closed: &closed})
	_ = cycle.Register(&fakeExtension{manifest: manifest("b", "a"), started: &started, closed: &closed})
	if err := cycle.StartAll(context.Background()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("cycle=%v", err)
	}
	var nilRegistry *Registry
	if err := nilRegistry.Close(); err != nil {
		t.Fatal(err)
	}
}

func manifest(name string, requires ...string) Manifest {
	return Manifest{Name: name, Version: "1.0.0", Description: name, Requires: requires, Capabilities: []string{"tools"}}
}
