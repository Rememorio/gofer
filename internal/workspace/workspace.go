package workspace

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Rememorio/gofer/internal/domain"
)

const (
	// VirtualRoot is the stable agent-visible root for thread data.
	VirtualRoot = "/mnt/user-data"
	// WorkspaceRoot contains agent working files.
	WorkspaceRoot = VirtualRoot + "/workspace"
	// UploadsRoot contains user-provided files and is read-only to file tools.
	UploadsRoot = VirtualRoot + "/uploads"
	// OutputsRoot contains user-facing generated artifacts.
	OutputsRoot = VirtualRoot + "/outputs"

	defaultMaxReadBytes   = 1 << 20
	defaultMaxWriteBytes  = 80 << 10
	defaultMaxUploadBytes = 32 << 20
	defaultMaxResults     = 200
	maxResultsLimit       = 1000
)

var (
	// ErrInvalidConfig identifies invalid workspace manager configuration.
	ErrInvalidConfig = errors.New("invalid workspace config")
	// ErrInvalidPath identifies a path outside the virtual thread root.
	ErrInvalidPath = errors.New("invalid workspace path")
	// ErrReadOnly identifies a write targeting protected uploaded data.
	ErrReadOnly = errors.New("workspace path is read-only")
	// ErrTooLarge identifies data that exceeds an operation limit.
	ErrTooLarge = errors.New("workspace data exceeds size limit")
	// ErrNotRegular identifies an operation that requires a regular file.
	ErrNotRegular = errors.New("workspace path is not a regular file")
	// ErrNoMatch identifies a replacement whose old text was not found.
	ErrNoMatch = errors.New("replacement text not found")
	// ErrAmbiguousMatch identifies a single replacement with multiple matches.
	ErrAmbiguousMatch = errors.New("replacement text occurs more than once")
)

var defaultIgnoredNames = map[string]struct{}{
	".git": {}, ".hg": {}, ".svn": {}, ".DS_Store": {},
	"node_modules": {}, "__pycache__": {}, ".venv": {},
	".cache": {}, ".pytest_cache": {}, "coverage": {},
}

// Config configures per-thread local workspace storage.
type Config struct {
	Root           string
	MaxReadBytes   int64
	MaxWriteBytes  int64
	MaxUploadBytes int64
}

// Manager creates isolated thread workspaces beneath one local root.
type Manager struct {
	root           string
	maxReadBytes   int64
	maxWriteBytes  int64
	maxUploadBytes int64
}

// Thread is a traversal-resistant view of one thread's user-data tree.
type Thread struct {
	id             domain.ThreadID
	hostRoot       string
	root           *os.Root
	maxReadBytes   int64
	maxWriteBytes  int64
	maxUploadBytes int64
	locks          keyedLocks
	closeOnce      sync.Once
	closeErr       error
}

// Entry describes one path without exposing its host location.
type Entry struct {
	Path       string    `json:"path"`
	Name       string    `json:"name"`
	Size       int64     `json:"size"`
	Mode       string    `json:"mode"`
	ModifiedAt time.Time `json:"modified_at"`
	Directory  bool      `json:"directory"`
}

// ExecutionMount describes one trusted host-to-sandbox directory mapping.
// HostPath is intended only for infrastructure adapters and must never be
// exposed to a model or remote API.
type ExecutionMount struct {
	VirtualPath string
	HostPath    string
	ReadOnly    bool
}

// ReadOptions selects an inclusive, one-indexed line range.
type ReadOptions struct {
	StartLine int
	EndLine   int
}

// ReadResult contains bounded text and line metadata.
type ReadResult struct {
	Content    string `json:"content"`
	StartLine  int    `json:"start_line"`
	EndLine    int    `json:"end_line"`
	TotalLines int    `json:"total_lines"`
}

// ListOptions bounds recursive directory traversal.
type ListOptions struct {
	MaxDepth   int
	MaxResults int
}

// ListResult contains stable, sorted paths.
type ListResult struct {
	Entries   []Entry `json:"entries"`
	Truncated bool    `json:"truncated"`
}

// GlobOptions controls directory inclusion and result bounds.
type GlobOptions struct {
	IncludeDirectories bool
	MaxResults         int
}

// GlobResult contains stable virtual paths.
type GlobResult struct {
	Paths     []string `json:"paths"`
	Truncated bool     `json:"truncated"`
}

// GrepOptions controls text search behavior and result bounds.
type GrepOptions struct {
	Glob          string
	Literal       bool
	CaseSensitive bool
	MaxResults    int
}

// GrepMatch is one bounded text match.
type GrepMatch struct {
	Path       string `json:"path"`
	LineNumber int    `json:"line_number"`
	Line       string `json:"line"`
}

// GrepResult contains ordered matches.
type GrepResult struct {
	Matches   []GrepMatch `json:"matches"`
	Truncated bool        `json:"truncated"`
}

// NewManager validates config and prepares the storage root.
func NewManager(config Config) (*Manager, error) {
	if strings.TrimSpace(config.Root) == "" {
		return nil, fmt.Errorf("%w: root is required", ErrInvalidConfig)
	}
	limits := []*int64{&config.MaxReadBytes, &config.MaxWriteBytes, &config.MaxUploadBytes}
	defaults := []int64{defaultMaxReadBytes, defaultMaxWriteBytes, defaultMaxUploadBytes}
	for index, limit := range limits {
		if *limit < 0 {
			return nil, fmt.Errorf("%w: size limits must not be negative", ErrInvalidConfig)
		}
		if *limit == 0 {
			*limit = defaults[index]
		}
	}
	absolute, err := filepath.Abs(config.Root)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace root: %w", err)
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("create workspace root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace root links: %w", err)
	}
	return &Manager{
		root: resolved, maxReadBytes: config.MaxReadBytes,
		maxWriteBytes: config.MaxWriteBytes, maxUploadBytes: config.MaxUploadBytes,
	}, nil
}

// Open prepares and opens an isolated workspace for threadID.
func (manager *Manager) Open(threadID domain.ThreadID) (*Thread, error) {
	if manager == nil {
		return nil, fmt.Errorf("%w: manager is nil", ErrInvalidConfig)
	}
	if _, err := domain.ParseThreadID(string(threadID)); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	hostRoot := filepath.Join(manager.root, "threads", string(threadID), "user-data")
	for _, directory := range []string{"workspace", "uploads", "outputs"} {
		if err := os.MkdirAll(filepath.Join(hostRoot, directory), 0o700); err != nil {
			return nil, fmt.Errorf("prepare thread workspace: %w", err)
		}
	}
	root, err := os.OpenRoot(hostRoot)
	if err != nil {
		return nil, fmt.Errorf("open thread workspace: %w", err)
	}
	return &Thread{
		id: threadID, hostRoot: hostRoot, root: root,
		maxReadBytes: manager.maxReadBytes, maxWriteBytes: manager.maxWriteBytes,
		maxUploadBytes: manager.maxUploadBytes,
	}, nil
}

// ID returns the owning thread identifier.
func (workspace *Thread) ID() domain.ThreadID { return workspace.id }

// ExecutionMounts returns the fixed per-thread directories used by command
// sandboxes. Uploaded files are mounted read-only; working and output files
// remain writable.
func (workspace *Thread) ExecutionMounts() []ExecutionMount {
	if workspace == nil || workspace.hostRoot == "" {
		return nil
	}
	return []ExecutionMount{
		{VirtualPath: WorkspaceRoot, HostPath: filepath.Join(workspace.hostRoot, "workspace")},
		{VirtualPath: UploadsRoot, HostPath: filepath.Join(workspace.hostRoot, "uploads"), ReadOnly: true},
		{VirtualPath: OutputsRoot, HostPath: filepath.Join(workspace.hostRoot, "outputs")},
	}
}

// Close releases the traversal-resistant root handle.
func (workspace *Thread) Close() error {
	if workspace == nil || workspace.root == nil {
		return nil
	}
	workspace.closeOnce.Do(func() { workspace.closeErr = workspace.root.Close() })
	return workspace.closeErr
}

// Inspect returns metadata for virtualPath.
func (workspace *Thread) Inspect(virtualPath string) (Entry, error) {
	relative, err := normalizeVirtualPath(virtualPath)
	if err != nil {
		return Entry{}, err
	}
	info, err := workspace.root.Stat(localName(relative))
	if err != nil {
		return Entry{}, err
	}
	return entryFromInfo(relative, info), nil
}

// OpenFile opens a regular file for bounded streaming reads.
func (workspace *Thread) OpenFile(virtualPath string) (io.ReadCloser, error) {
	relative, err := normalizeVirtualPath(virtualPath)
	if err != nil {
		return nil, err
	}
	file, err := workspace.root.Open(localName(relative))
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("%w: %s", ErrNotRegular, virtualPath)
	}
	return file, nil
}

// ReadFile reads a bounded UTF-8-compatible text file.
func (workspace *Thread) ReadFile(virtualPath string, options ReadOptions) (ReadResult, error) {
	if options.StartLine < 0 || options.EndLine < 0 || (options.EndLine > 0 && options.StartLine == 0) ||
		(options.EndLine > 0 && options.EndLine < options.StartLine) {
		return ReadResult{}, fmt.Errorf("%w: invalid line range", ErrInvalidPath)
	}
	reader, err := workspace.OpenFile(virtualPath)
	if err != nil {
		return ReadResult{}, err
	}
	defer func() { _ = reader.Close() }()
	data, err := readBounded(reader, workspace.maxReadBytes)
	if err != nil {
		return ReadResult{}, err
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return ReadResult{}, fmt.Errorf("%w: binary file", ErrNotRegular)
	}
	return selectLines(string(data), options), nil
}

// WriteFile writes or appends text under workspace or outputs.
func (workspace *Thread) WriteFile(virtualPath string, content []byte, appendMode bool) error {
	relative, err := writableRelative(virtualPath)
	if err != nil {
		return err
	}
	if int64(len(content)) > workspace.maxWriteBytes {
		return fmt.Errorf("%w: write limit is %d bytes", ErrTooLarge, workspace.maxWriteBytes)
	}
	unlock := workspace.locks.lock(relative)
	defer unlock()
	return workspace.writeLocked(relative, content, appendMode)
}

// CreateOutput atomically creates a binary artifact under OutputsRoot without
// overwriting an existing file.
func (workspace *Thread) CreateOutput(virtualPath string, content []byte) error {
	relative, err := normalizeVirtualPath(virtualPath)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(relative, "outputs/") {
		return fmt.Errorf("%w: output must be a file below %s", ErrInvalidPath, OutputsRoot)
	}
	if int64(len(content)) > workspace.maxUploadBytes {
		return fmt.Errorf("%w: output limit is %d bytes", ErrTooLarge, workspace.maxUploadBytes)
	}
	unlock := workspace.locks.lock(relative)
	defer unlock()
	if err := workspace.root.MkdirAll(localName(path.Dir(relative)), 0o700); err != nil {
		return err
	}
	return workspace.writeExclusive(relative, content)
}

// Replace performs an exact, serialized text replacement.
func (workspace *Thread) Replace(virtualPath, oldText, newText string, replaceAll bool) error {
	if oldText == "" {
		return fmt.Errorf("%w: old text is empty", ErrNoMatch)
	}
	relative, err := writableRelative(virtualPath)
	if err != nil {
		return err
	}
	unlock := workspace.locks.lock(relative)
	defer unlock()
	reader, err := workspace.root.Open(localName(relative))
	if err != nil {
		return err
	}
	data, readErr := readBounded(reader, workspace.maxReadBytes)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil {
		return errors.Join(readErr, closeErr)
	}
	count := bytes.Count(data, []byte(oldText))
	if count == 0 {
		return ErrNoMatch
	}
	if !replaceAll && count != 1 {
		return fmt.Errorf("%w: found %d matches", ErrAmbiguousMatch, count)
	}
	limit := 1
	if replaceAll {
		limit = -1
	}
	replaced := bytes.Replace(data, []byte(oldText), []byte(newText), limit)
	if int64(len(replaced)) > workspace.maxWriteBytes {
		return fmt.Errorf("%w: write limit is %d bytes", ErrTooLarge, workspace.maxWriteBytes)
	}
	return workspace.writeLocked(relative, replaced, false)
}

// PutUpload stores a bounded user upload under a collision-free filename.
func (workspace *Thread) PutUpload(filename string, reader io.Reader) (Entry, error) {
	if !validFilename(filename) {
		return Entry{}, fmt.Errorf("%w: unsafe upload filename", ErrInvalidPath)
	}
	data, err := readBounded(reader, workspace.maxUploadBytes)
	if err != nil {
		return Entry{}, err
	}
	unlock := workspace.locks.lock("uploads")
	defer unlock()
	for index := 0; ; index++ {
		relative := uploadRelative(filename, index)
		if err := workspace.writeExclusive(relative, data); err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			return Entry{}, err
		}
		return workspace.Inspect(virtualize(relative))
	}
}

// List returns paths below virtualPath up to configured bounds.
func (workspace *Thread) List(virtualPath string, options ListOptions) (ListResult, error) {
	relative, err := normalizeVirtualPath(virtualPath)
	if err != nil {
		return ListResult{}, err
	}
	if options.MaxDepth == 0 {
		options.MaxDepth = 2
	}
	if options.MaxDepth < 1 || options.MaxDepth > 20 {
		return ListResult{}, fmt.Errorf("%w: max depth must be between 1 and 20", ErrInvalidConfig)
	}
	limit, err := normalizeResultsLimit(options.MaxResults)
	if err != nil {
		return ListResult{}, err
	}
	collector := listCollector{base: relative, maxDepth: options.MaxDepth, limit: limit}
	err = fs.WalkDir(workspace.root.FS(), relative, collector.visit)
	if err != nil {
		return ListResult{}, err
	}
	sort.Slice(collector.entries, func(left, right int) bool {
		return collector.entries[left].Path < collector.entries[right].Path
	})
	return ListResult{Entries: collector.entries, Truncated: collector.truncated}, nil
}

// Glob finds paths matching a slash-separated pattern below virtualPath.
func (workspace *Thread) Glob(virtualPath, pattern string, options GlobOptions) (GlobResult, error) {
	if pattern == "" || strings.Contains(pattern, "\\") {
		return GlobResult{}, fmt.Errorf("%w: invalid glob pattern", ErrInvalidPath)
	}
	if _, err := path.Match(pattern, "probe"); err != nil {
		return GlobResult{}, fmt.Errorf("%w: %w", ErrInvalidPath, err)
	}
	limit, err := normalizeResultsLimit(options.MaxResults)
	if err != nil {
		return GlobResult{}, err
	}
	paths := make([]string, 0)
	truncated := false
	err = workspace.walkFiles(virtualPath, func(base, current string, item fs.DirEntry) error {
		if item.IsDir() && !options.IncludeDirectories {
			return nil
		}
		relative := strings.TrimPrefix(strings.TrimPrefix(current, base), "/")
		if !globMatch(pattern, relative) {
			return nil
		}
		if len(paths) >= limit {
			truncated = true
			return fs.SkipAll
		}
		paths = append(paths, virtualize(current))
		return nil
	})
	if err != nil {
		return GlobResult{}, err
	}
	sort.Strings(paths)
	return GlobResult{Paths: paths, Truncated: truncated}, nil
}

// Grep searches bounded text files below virtualPath.
func (workspace *Thread) Grep(virtualPath, pattern string, options GrepOptions) (GrepResult, error) {
	if pattern == "" {
		return GrepResult{}, fmt.Errorf("%w: grep pattern is required", ErrInvalidConfig)
	}
	limit, err := normalizeResultsLimit(options.MaxResults)
	if err != nil {
		return GrepResult{}, err
	}
	regexSource := pattern
	if options.Literal {
		regexSource = regexp.QuoteMeta(regexSource)
	}
	if !options.CaseSensitive {
		regexSource = "(?i)" + regexSource
	}
	expression, err := regexp.Compile(regexSource)
	if err != nil {
		return GrepResult{}, fmt.Errorf("%w: invalid regular expression: %w", ErrInvalidConfig, err)
	}
	matches := make([]GrepMatch, 0)
	truncated := false
	err = workspace.walkFiles(virtualPath, func(base, current string, item fs.DirEntry) error {
		if item.IsDir() {
			return nil
		}
		relative := strings.TrimPrefix(strings.TrimPrefix(current, base), "/")
		if options.Glob != "" && !globMatch(options.Glob, relative) {
			return nil
		}
		fileMatches, err := workspace.grepFile(current, expression, limit-len(matches))
		if err != nil {
			if errors.Is(err, ErrTooLarge) || errors.Is(err, ErrNotRegular) {
				return nil
			}
			return err
		}
		matches = append(matches, fileMatches...)
		if len(matches) >= limit {
			truncated = true
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return GrepResult{}, err
	}
	return GrepResult{Matches: matches, Truncated: truncated}, nil
}

func (workspace *Thread) walkFiles(virtualPath string, visit func(string, string, fs.DirEntry) error) error {
	base, err := normalizeVirtualPath(virtualPath)
	if err != nil {
		return err
	}
	return fs.WalkDir(workspace.root.FS(), base, func(current string, item fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if current == base {
			if !item.IsDir() {
				return visit(base, current, item)
			}
			return nil
		}
		if ignored(item.Name()) {
			if item.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		return visit(base, current, item)
	})
}

func (workspace *Thread) grepFile(relative string, expression *regexp.Regexp, limit int) ([]GrepMatch, error) {
	file, err := workspace.root.Open(localName(relative))
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, ErrNotRegular
	}
	if info.Size() > workspace.maxReadBytes {
		return nil, ErrTooLarge
	}
	reader := bufio.NewReader(io.LimitReader(file, workspace.maxReadBytes+1))
	matches := make([]GrepMatch, 0)
	for lineNumber := 1; ; lineNumber++ {
		line, readErr := reader.ReadString('\n')
		if strings.IndexByte(line, 0) >= 0 {
			return nil, ErrNotRegular
		}
		if expression.MatchString(line) {
			matches = append(matches, GrepMatch{
				Path: virtualize(relative), LineNumber: lineNumber, Line: summarizeLine(line),
			})
			if len(matches) >= limit {
				return matches, nil
			}
		}
		if errors.Is(readErr, io.EOF) {
			return matches, nil
		}
		if readErr != nil {
			return nil, readErr
		}
	}
}

func (workspace *Thread) writeLocked(relative string, content []byte, appendMode bool) error {
	if appendMode {
		if err := workspace.root.MkdirAll(localName(path.Dir(relative)), 0o700); err != nil {
			return err
		}
		file, err := workspace.root.OpenFile(localName(relative), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		_, writeErr := file.Write(content)
		closeErr := file.Close()
		return errors.Join(writeErr, closeErr)
	}
	return workspace.writeAtomic(relative, content)
}

func (workspace *Thread) writeAtomic(relative string, content []byte) error {
	directory := path.Dir(relative)
	if err := workspace.root.MkdirAll(localName(directory), 0o700); err != nil {
		return err
	}
	suffix, err := randomSuffix()
	if err != nil {
		return err
	}
	temporary := path.Join(directory, ".gofer-write-"+suffix+".tmp")
	file, err := workspace.root.OpenFile(localName(temporary), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(content)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		_ = workspace.root.Remove(localName(temporary))
		return errors.Join(writeErr, closeErr)
	}
	if err := workspace.root.Rename(localName(temporary), localName(relative)); err != nil {
		_ = workspace.root.Remove(localName(temporary))
		return err
	}
	return nil
}

func (workspace *Thread) writeExclusive(relative string, content []byte) error {
	file, err := workspace.root.OpenFile(localName(relative), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(content)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		_ = workspace.root.Remove(localName(relative))
		return errors.Join(writeErr, closeErr)
	}
	return nil
}

func uploadRelative(filename string, index int) string {
	extension := path.Ext(filename)
	stem := strings.TrimSuffix(filename, extension)
	candidate := filename
	if index > 0 {
		candidate = fmt.Sprintf("%s-%d%s", stem, index, extension)
	}
	return path.Join("uploads", candidate)
}

func normalizeVirtualPath(value string) (string, error) {
	if value == VirtualRoot || value == VirtualRoot+"/" {
		return ".", nil
	}
	if strings.ContainsAny(value, "\x00\\") || !strings.HasPrefix(value, VirtualRoot+"/") {
		return "", fmt.Errorf("%w: path must be below %s", ErrInvalidPath, VirtualRoot)
	}
	relative := strings.TrimPrefix(value, VirtualRoot+"/")
	if !fs.ValidPath(relative) {
		return "", fmt.Errorf("%w: non-canonical path", ErrInvalidPath)
	}
	return relative, nil
}

func writableRelative(value string) (string, error) {
	relative, err := normalizeVirtualPath(value)
	if err != nil {
		return "", err
	}
	if relative == "uploads" || strings.HasPrefix(relative, "uploads/") {
		return "", fmt.Errorf("%w: %s", ErrReadOnly, value)
	}
	if relative != "workspace" && relative != "outputs" &&
		!strings.HasPrefix(relative, "workspace/") && !strings.HasPrefix(relative, "outputs/") {
		return "", fmt.Errorf("%w: writes require workspace or outputs path", ErrInvalidPath)
	}
	return relative, nil
}

func localName(relative string) string {
	localized, err := filepath.Localize(relative)
	if err != nil {
		panic("validated workspace path could not be localized: " + err.Error())
	}
	return localized
}

func virtualize(relative string) string {
	if relative == "." {
		return VirtualRoot
	}
	return VirtualRoot + "/" + relative
}

func entryFromInfo(relative string, info fs.FileInfo) Entry {
	return Entry{
		Path: virtualize(relative), Name: info.Name(), Size: info.Size(), Mode: info.Mode().String(),
		ModifiedAt: info.ModTime().UTC(), Directory: info.IsDir(),
	}
}

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: limit is %d bytes", ErrTooLarge, limit)
	}
	return data, nil
}

func selectLines(content string, options ReadOptions) ReadResult {
	lines := strings.Split(content, "\n")
	if strings.HasSuffix(content, "\n") && len(lines) > 1 {
		lines = lines[:len(lines)-1]
	}
	total := len(lines)
	start := options.StartLine
	if start == 0 {
		start = 1
	}
	end := options.EndLine
	if end == 0 || end > total {
		end = total
	}
	if start > total {
		return ReadResult{StartLine: start, EndLine: total, TotalLines: total}
	}
	selected := strings.Join(lines[start-1:end], "\n")
	return ReadResult{Content: selected, StartLine: start, EndLine: end, TotalLines: total}
}

func normalizeResultsLimit(limit int) (int, error) {
	if limit == 0 {
		return defaultMaxResults, nil
	}
	if limit < 1 || limit > maxResultsLimit {
		return 0, fmt.Errorf("%w: max results must be between 1 and %d", ErrInvalidConfig, maxResultsLimit)
	}
	return limit, nil
}

func pathDepth(value string) int {
	if value == "." {
		return 0
	}
	return strings.Count(value, "/") + 1
}

func ignored(name string) bool {
	if _, exists := defaultIgnoredNames[name]; exists {
		return true
	}
	return strings.HasPrefix(name, ".gofer-write-") || strings.HasSuffix(name, "~") ||
		strings.HasSuffix(name, ".tmp") || strings.HasSuffix(name, ".log")
}

func globMatch(pattern, value string) bool {
	matched, _ := path.Match(pattern, value)
	if matched || !strings.HasPrefix(pattern, "**/") {
		return matched
	}
	remainder := strings.TrimPrefix(pattern, "**/")
	for {
		matched, _ = path.Match(remainder, value)
		if matched {
			return true
		}
		separator := strings.IndexByte(value, '/')
		if separator < 0 {
			return false
		}
		value = value[separator+1:]
	}
}

func summarizeLine(line string) string {
	line = strings.TrimRight(line, "\r\n")
	const maxCharacters = 200
	characters := []rune(line)
	if len(characters) <= maxCharacters {
		return line
	}
	return string(characters[:maxCharacters-3]) + "..."
}

func validFilename(filename string) bool {
	return filename != "" && filename != "." && filename != ".." && path.Base(filename) == filename &&
		!strings.ContainsAny(filename, "\x00/\\")
}

func randomSuffix() (string, error) {
	var data [8]byte
	if _, err := io.ReadFull(rand.Reader, data[:]); err != nil {
		return "", fmt.Errorf("generate temporary filename: %w", err)
	}
	return hex.EncodeToString(data[:]), nil
}

type keyedLocks struct {
	mu    sync.Mutex
	locks map[string]*keyedLock
}

type listCollector struct {
	base      string
	maxDepth  int
	limit     int
	entries   []Entry
	truncated bool
}

func (collector *listCollector) visit(current string, item fs.DirEntry, walkErr error) error {
	if walkErr != nil {
		return walkErr
	}
	if current == collector.base {
		return nil
	}
	if ignored(item.Name()) {
		return skipDirectory(item)
	}
	if pathDepth(current)-pathDepth(collector.base) > collector.maxDepth {
		return skipDirectory(item)
	}
	if len(collector.entries) >= collector.limit {
		collector.truncated = true
		return fs.SkipAll
	}
	info, err := item.Info()
	if err != nil {
		return err
	}
	collector.entries = append(collector.entries, entryFromInfo(current, info))
	return nil
}

func skipDirectory(item fs.DirEntry) error {
	if item.IsDir() {
		return fs.SkipDir
	}
	return nil
}

type keyedLock struct {
	mu   sync.Mutex
	refs int
}

func (locks *keyedLocks) lock(key string) func() {
	locks.mu.Lock()
	if locks.locks == nil {
		locks.locks = make(map[string]*keyedLock)
	}
	entry := locks.locks[key]
	if entry == nil {
		entry = &keyedLock{}
		locks.locks[key] = entry
	}
	entry.refs++
	locks.mu.Unlock()
	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		locks.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(locks.locks, key)
		}
		locks.mu.Unlock()
	}
}
