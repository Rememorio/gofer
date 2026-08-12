package extension

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

var (
	// ErrInvalid identifies a malformed manifest or dependency graph.
	ErrInvalid = errors.New("invalid extension")
	// ErrDuplicate identifies a duplicate extension name.
	ErrDuplicate = errors.New("duplicate extension")
	// ErrNotFound identifies an unknown extension.
	ErrNotFound = errors.New("extension not found")
)

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// Manifest describes extension identity, dependencies, and advertised capabilities.
type Manifest struct {
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	Description  string   `json:"description"`
	Requires     []string `json:"requires,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// Extension is a managed runtime component.
type Extension interface {
	Manifest() Manifest
	Start(context.Context) error
	Close() error
}

// State identifies extension lifecycle state.
type State string

const (
	// Registered identifies an extension waiting to start.
	Registered State = "registered"
	// Started identifies a successfully started extension.
	Started State = "started"
	// Stopped identifies a closed extension.
	Stopped State = "stopped"
	// Failed identifies an extension whose startup failed.
	Failed State = "failed"
)

// Status is a safe immutable lifecycle snapshot.
type Status struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	State   State  `json:"state"`
	Error   string `json:"error,omitempty"`
}
type entry struct {
	extension Extension
	manifest  Manifest
	state     State
	err       string
}

// Registry owns extension validation, ordering, startup rollback, and shutdown.
type Registry struct {
	mu      sync.Mutex
	entries map[string]*entry
	order   []string
	started bool
	closed  bool
}

// NewRegistry constructs an empty registry.
func NewRegistry() *Registry { return &Registry{entries: make(map[string]*entry)} }

// Register validates and adds an extension before startup.
func (registry *Registry) Register(extension Extension) error {
	if extension == nil {
		return ErrInvalid
	}
	manifest := extension.Manifest()
	if err := validateManifest(manifest); err != nil {
		return err
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.started || registry.closed {
		return errors.New("extension registry lifecycle already started")
	}
	if _, exists := registry.entries[manifest.Name]; exists {
		return ErrDuplicate
	}
	manifest.Requires = append([]string(nil), manifest.Requires...)
	manifest.Capabilities = append([]string(nil), manifest.Capabilities...)
	registry.entries[manifest.Name] = &entry{extension: extension, manifest: manifest, state: Registered}
	return nil
}

// StartAll starts extensions in deterministic topological order and rolls back on failure.
func (registry *Registry) StartAll(ctx context.Context) error {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.closed || registry.started {
		return errors.New("extension registry cannot start")
	}
	order, err := registry.resolveOrder()
	if err != nil {
		return err
	}
	registry.started = true
	for _, name := range order {
		current := registry.entries[name]
		if err = current.extension.Start(ctx); err != nil {
			current.state = Failed
			current.err = err.Error()
			rollbackErr := registry.rollback()
			return errors.Join(fmt.Errorf("start extension %s: %w", name, err), rollbackErr)
		}
		current.state = Started
		registry.order = append(registry.order, name)
	}
	return nil
}

func (registry *Registry) resolveOrder() ([]string, error) {
	names := make([]string, 0, len(registry.entries))
	for name := range registry.entries {
		names = append(names, name)
	}
	sort.Strings(names)
	state := make(map[string]uint8, len(names))
	order := make([]string, 0, len(names))
	var visit func(string) error
	visit = func(name string) error {
		switch state[name] {
		case 1:
			return fmt.Errorf("%w: dependency cycle at %s", ErrInvalid, name)
		case 2:
			return nil
		}
		current, exists := registry.entries[name]
		if !exists {
			return fmt.Errorf("%w: required extension %s", ErrNotFound, name)
		}
		state[name] = 1
		requires := append([]string(nil), current.manifest.Requires...)
		sort.Strings(requires)
		for _, required := range requires {
			if err := visit(required); err != nil {
				return err
			}
		}
		state[name] = 2
		order = append(order, name)
		return nil
	}
	for _, name := range names {
		if err := visit(name); err != nil {
			return nil, err
		}
	}
	return order, nil
}

func (registry *Registry) rollback() error {
	var failures []error
	for index := len(registry.order) - 1; index >= 0; index-- {
		current := registry.entries[registry.order[index]]
		if err := current.extension.Close(); err != nil {
			failures = append(failures, err)
		}
		current.state = Stopped
	}
	registry.order = nil
	return errors.Join(failures...)
}

// Close closes started extensions in reverse dependency order.
func (registry *Registry) Close() error {
	if registry == nil {
		return nil
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.closed {
		return nil
	}
	registry.closed = true
	return registry.rollback()
}

// Status returns stable extension name order.
func (registry *Registry) Status() []Status {
	registry.mu.Lock()
	statuses := make([]Status, 0, len(registry.entries))
	for _, current := range registry.entries {
		statuses = append(statuses, Status{Name: current.manifest.Name, Version: current.manifest.Version, State: current.state, Error: current.err})
	}
	registry.mu.Unlock()
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Name < statuses[j].Name })
	return statuses
}

func validateManifest(manifest Manifest) error {
	if !namePattern.MatchString(manifest.Name) || strings.TrimSpace(manifest.Version) == "" || strings.TrimSpace(manifest.Description) == "" {
		return ErrInvalid
	}
	seen := make(map[string]struct{})
	for _, required := range manifest.Requires {
		if !namePattern.MatchString(required) || required == manifest.Name {
			return ErrInvalid
		}
		if _, exists := seen[required]; exists {
			return ErrInvalid
		}
		seen[required] = struct{}{}
	}
	for _, capability := range manifest.Capabilities {
		if !namePattern.MatchString(capability) {
			return ErrInvalid
		}
	}
	return nil
}
