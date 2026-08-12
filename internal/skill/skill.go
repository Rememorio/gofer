package skill

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const (
	// DefaultVirtualRoot is the agent-visible root for skill projections.
	DefaultVirtualRoot      = "/mnt/skills"
	defaultMaxDocumentBytes = 1 << 20
	defaultMaxPackageBytes  = 10 << 20
)

var (
	// ErrInvalidConfig identifies invalid catalog configuration.
	ErrInvalidConfig = errors.New("invalid skill configuration")
	// ErrInvalidSkill identifies a malformed or unsafe skill package.
	ErrInvalidSkill = errors.New("invalid skill")
	// ErrNotFound identifies an unknown skill name.
	ErrNotFound = errors.New("skill not found")
	// ErrDisabled identifies an installed but disabled skill.
	ErrDisabled = errors.New("skill is disabled")
	// ErrDuplicate identifies colliding skill names.
	ErrDuplicate = errors.New("duplicate skill")
)

// Category identifies a skill's source and mutability class.
type Category string

// Supported skill categories.
const (
	CategoryPublic      Category = "public"
	CategoryCustom      Category = "custom"
	CategoryIntegration Category = "integrations"
	CategoryLegacy      Category = "legacy"
)

var categories = []Category{
	CategoryPublic, CategoryCustom, CategoryIntegration, CategoryLegacy,
}

// SecretRequirement declares a request-scoped environment secret.
type SecretRequirement struct {
	Name     string `json:"name"`
	Optional bool   `json:"optional,omitempty"`
}

// Skill is validated, provider-independent skill metadata.
type Skill struct {
	Name              string              `json:"name"`
	Description       string              `json:"description"`
	License           string              `json:"license,omitempty"`
	Category          Category            `json:"category"`
	RelativePath      string              `json:"relative_path"`
	DocumentPath      string              `json:"document_path"`
	AllowedTools      []string            `json:"allowed_tools,omitempty"`
	AllowedToolsSet   bool                `json:"allowed_tools_set"`
	RequiredSecrets   []SecretRequirement `json:"required_secrets,omitempty"`
	SecretsAutonomous bool                `json:"secrets_autonomous"`
	Enabled           bool                `json:"enabled"`
	Compatibility     string              `json:"compatibility,omitempty"`
	Version           string              `json:"version,omitempty"`
	Author            string              `json:"author,omitempty"`
}

// AllowsTool reports whether the enabled skill permits an exact tool name.
// An omitted allowed-tools field permits every tool; an explicit empty list
// permits none.
func (skill Skill) AllowsTool(name string) bool {
	if !skill.Enabled {
		return false
	}
	if !skill.AllowedToolsSet {
		return true
	}
	for _, allowed := range skill.AllowedTools {
		if allowed == name {
			return true
		}
	}
	return false
}

// Key uniquely identifies state for an installed skill.
type Key struct {
	Category Category
	Name     string
}

// StateStore persists enabled overrides independently from package files.
type StateStore interface {
	Get(context.Context, Key) (enabled bool, found bool, err error)
	Set(context.Context, Key, bool) error
}

// MemoryState is a concurrency-safe reference StateStore.
type MemoryState struct {
	mu     sync.RWMutex
	values map[Key]bool
}

// NewMemoryState constructs an empty enabled-state store.
func NewMemoryState() *MemoryState {
	return &MemoryState{values: make(map[Key]bool)}
}

// Get returns one enabled override.
func (state *MemoryState) Get(ctx context.Context, key Key) (bool, bool, error) {
	if err := ctx.Err(); err != nil {
		return false, false, err
	}
	state.mu.RLock()
	value, found := state.values[key]
	state.mu.RUnlock()
	return value, found, nil
}

// Set writes one enabled override.
func (state *MemoryState) Set(ctx context.Context, key Key, enabled bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	state.mu.Lock()
	state.values[key] = enabled
	state.mu.Unlock()
	return nil
}

// Config configures a local skill catalog.
type Config struct {
	Root             string
	VirtualRoot      string
	MaxDocumentBytes int64
	MaxPackageBytes  int64
	State            StateStore
}

// Catalog is an atomic snapshot of validated skill packages.
type Catalog struct {
	mu               sync.RWMutex
	root             string
	virtualRoot      string
	maxDocumentBytes int64
	maxPackageBytes  int64
	state            StateStore
	records          map[string]record
}

type record struct {
	skill    Skill
	hostDir  string
	hostFile string
}

// NewCatalog validates config and constructs an empty catalog.
func NewCatalog(config Config) (*Catalog, error) {
	if strings.TrimSpace(config.Root) == "" {
		return nil, fmt.Errorf("%w: root is required", ErrInvalidConfig)
	}
	if config.VirtualRoot == "" {
		config.VirtualRoot = DefaultVirtualRoot
	}
	if !validVirtualRoot(config.VirtualRoot) {
		return nil, fmt.Errorf("%w: virtual root must be a clean absolute POSIX path", ErrInvalidConfig)
	}
	if config.MaxDocumentBytes < 0 || config.MaxPackageBytes < 0 {
		return nil, fmt.Errorf("%w: size limits must not be negative", ErrInvalidConfig)
	}
	if config.MaxDocumentBytes == 0 {
		config.MaxDocumentBytes = defaultMaxDocumentBytes
	}
	if config.MaxPackageBytes == 0 {
		config.MaxPackageBytes = defaultMaxPackageBytes
	}
	if config.State == nil {
		config.State = NewMemoryState()
	}
	absolute, err := filepath.Abs(config.Root)
	if err != nil {
		return nil, fmt.Errorf("resolve skill root: %w", err)
	}
	return &Catalog{
		root: absolute, virtualRoot: config.VirtualRoot,
		maxDocumentBytes: config.MaxDocumentBytes, maxPackageBytes: config.MaxPackageBytes,
		state: config.State, records: make(map[string]record),
	}, nil
}

// Refresh scans every category and atomically replaces the catalog snapshot.
func (catalog *Catalog) Refresh(ctx context.Context) error {
	if catalog == nil {
		return fmt.Errorf("%w: catalog is nil", ErrInvalidConfig)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	next := make(map[string]record)
	for _, category := range categories {
		categoryRecords, err := catalog.scanCategory(ctx, category)
		if err != nil {
			return err
		}
		for _, candidate := range categoryRecords {
			if previous, duplicate := next[candidate.skill.Name]; duplicate {
				return fmt.Errorf("%w: %q in %s and %s", ErrDuplicate, candidate.skill.Name,
					previous.skill.Category, candidate.skill.Category)
			}
			next[candidate.skill.Name] = candidate
		}
	}
	catalog.mu.Lock()
	catalog.records = next
	catalog.mu.Unlock()
	return nil
}

// List returns a stable metadata snapshot ordered by name.
func (catalog *Catalog) List(enabledOnly bool) []Skill {
	if catalog == nil {
		return nil
	}
	catalog.mu.RLock()
	skills := make([]Skill, 0, len(catalog.records))
	for _, candidate := range catalog.records {
		if enabledOnly && !candidate.skill.Enabled {
			continue
		}
		skills = append(skills, cloneSkill(candidate.skill))
	}
	catalog.mu.RUnlock()
	sort.Slice(skills, func(left, right int) bool { return skills[left].Name < skills[right].Name })
	return skills
}

// Get returns metadata by exact name.
func (catalog *Catalog) Get(name string) (Skill, error) {
	if catalog == nil {
		return Skill{}, fmt.Errorf("%w: catalog is nil", ErrInvalidConfig)
	}
	catalog.mu.RLock()
	candidate, exists := catalog.records[name]
	catalog.mu.RUnlock()
	if !exists {
		return Skill{}, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return cloneSkill(candidate.skill), nil
}

// SetEnabled persists and applies one enabled override.
func (catalog *Catalog) SetEnabled(ctx context.Context, name string, enabled bool) error {
	if catalog == nil {
		return fmt.Errorf("%w: catalog is nil", ErrInvalidConfig)
	}
	catalog.mu.RLock()
	candidate, exists := catalog.records[name]
	catalog.mu.RUnlock()
	if !exists {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	key := Key{Category: candidate.skill.Category, Name: name}
	if err := catalog.state.Set(ctx, key, enabled); err != nil {
		return err
	}
	catalog.mu.Lock()
	candidate, exists = catalog.records[name]
	if !exists || candidate.skill.Category != key.Category {
		catalog.mu.Unlock()
		return fmt.Errorf("%w: skill changed during state update", ErrNotFound)
	}
	candidate.skill.Enabled = enabled
	catalog.records[name] = candidate
	catalog.mu.Unlock()
	return nil
}

// Load returns the current validated SKILL.md document for an enabled skill.
func (catalog *Catalog) Load(ctx context.Context, name string) (string, error) {
	if catalog == nil {
		return "", fmt.Errorf("%w: catalog is nil", ErrInvalidConfig)
	}
	catalog.mu.RLock()
	candidate, exists := catalog.records[name]
	catalog.mu.RUnlock()
	if !exists {
		return "", fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if !candidate.skill.Enabled {
		return "", fmt.Errorf("%w: %s", ErrDisabled, name)
	}
	parsed, document, err := parseFile(ctx, candidate.hostFile, candidate.skill.Category,
		candidate.skill.RelativePath, catalog.virtualRoot, catalog.maxDocumentBytes)
	if err != nil {
		return "", err
	}
	if parsed.Name != candidate.skill.Name {
		return "", fmt.Errorf("%w: skill identity changed during load", ErrInvalidSkill)
	}
	return document, nil
}

// Search ranks enabled skills by exact name, name terms, then description terms.
func (catalog *Catalog) Search(query string, limit int) []Skill {
	if limit <= 0 {
		limit = 5
	}
	terms := strings.Fields(strings.ToLower(query))
	type scored struct {
		skill Skill
		score int
	}
	scores := make([]scored, 0)
	for _, candidate := range catalog.List(true) {
		name := strings.ToLower(candidate.Name)
		description := strings.ToLower(candidate.Description)
		score := 0
		if name == strings.ToLower(strings.TrimSpace(query)) {
			score += 1000
		}
		for _, term := range terms {
			if strings.Contains(name, term) {
				score += 20
			}
			if strings.Contains(description, term) {
				score += 5
			}
		}
		if score > 0 {
			scores = append(scores, scored{skill: candidate, score: score})
		}
	}
	sort.Slice(scores, func(left, right int) bool {
		if scores[left].score == scores[right].score {
			return scores[left].skill.Name < scores[right].skill.Name
		}
		return scores[left].score > scores[right].score
	})
	if len(scores) > limit {
		scores = scores[:limit]
	}
	result := make([]Skill, len(scores))
	for index := range scores {
		result[index] = scores[index].skill
	}
	return result
}

// IndexPrompt renders a name-only progressive-discovery prompt section.
func (catalog *Catalog) IndexPrompt() string {
	skills := catalog.List(true)
	if len(skills) == 0 {
		return ""
	}
	names := make([]string, len(skills))
	for index, candidate := range skills {
		names[index] = html.EscapeString(candidate.Name)
	}
	return fmt.Sprintf(`<skill_system>
Use skills for optimized task-specific workflows.
1. Check <skill_index> for a matching name.
2. Call describe_skill to inspect its metadata and location.
3. Call read_skill only when relevant, then follow the returned SKILL.md precisely.
<skill_index>
%s
</skill_index>
Skills are mounted read-only at: %s
</skill_system>`, strings.Join(names, ", "), html.EscapeString(catalog.virtualRoot))
}

func (catalog *Catalog) scanCategory(ctx context.Context, category Category) ([]record, error) {
	categoryRoot := filepath.Join(catalog.root, string(category))
	info, err := os.Lstat(categoryRoot)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("%w: category %s must be a non-symlink directory", ErrInvalidSkill, category)
	}
	return catalog.scanDirectory(ctx, category, categoryRoot, categoryRoot)
}

func (catalog *Catalog) scanDirectory(
	ctx context.Context,
	category Category,
	categoryRoot string,
	current string,
) ([]record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(current)
	if err != nil {
		return nil, err
	}
	hasDocument, err := hasSkillDocument(entries)
	if err != nil {
		return nil, err
	}
	if hasDocument {
		candidate, err := catalog.readRecord(ctx, category, categoryRoot, current)
		if err != nil {
			return nil, err
		}
		return []record{candidate}, nil
	}
	return catalog.scanChildren(ctx, category, categoryRoot, current, entries)
}

func hasSkillDocument(entries []fs.DirEntry) (bool, error) {
	for _, entry := range entries {
		if entry.Name() != "SKILL.md" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return false, err
		}
		if entry.Type()&fs.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return false, fmt.Errorf("%w: SKILL.md must be a regular non-symlink file", ErrInvalidSkill)
		}
		return true, nil
	}
	return false, nil
}

func (catalog *Catalog) readRecord(
	ctx context.Context,
	category Category,
	categoryRoot string,
	current string,
) (record, error) {
	relative, err := filepath.Rel(categoryRoot, current)
	if err != nil {
		return record{}, err
	}
	relative = filepath.ToSlash(relative)
	if !fs.ValidPath(relative) || relative == "." {
		return record{}, fmt.Errorf("%w: skill package must be below its category root", ErrInvalidSkill)
	}
	hostFile := filepath.Join(current, "SKILL.md")
	skill, _, err := parseFile(ctx, hostFile, category, relative, catalog.virtualRoot, catalog.maxDocumentBytes)
	if err != nil {
		return record{}, err
	}
	enabled, found, err := catalog.state.Get(ctx, Key{Category: category, Name: skill.Name})
	if err != nil {
		return record{}, err
	}
	if found {
		skill.Enabled = enabled
	}
	return record{skill: skill, hostDir: current, hostFile: hostFile}, nil
}

func (catalog *Catalog) scanChildren(
	ctx context.Context,
	category Category,
	categoryRoot string,
	current string,
	entries []fs.DirEntry,
) ([]record, error) {
	records := make([]record, 0)
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&fs.ModeSymlink != 0 || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		nested, err := catalog.scanDirectory(ctx, category, categoryRoot, filepath.Join(current, entry.Name()))
		if err != nil {
			return nil, err
		}
		records = append(records, nested...)
	}
	return records, nil
}

func cloneSkill(skill Skill) Skill {
	skill.AllowedTools = append([]string(nil), skill.AllowedTools...)
	skill.RequiredSecrets = append([]SecretRequirement(nil), skill.RequiredSecrets...)
	return skill
}

func validVirtualRoot(value string) bool {
	return strings.HasPrefix(value, "/") && value != "/" && path.Clean(value) == value &&
		!strings.Contains(value, "\\")
}
