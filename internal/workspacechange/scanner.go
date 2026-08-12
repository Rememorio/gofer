package workspacechange

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/Rememorio/gofer/internal/workspace"
)

const sampleBytes = 4096

var excludedDirectoryNames = map[string]struct{}{
	".browser-frames": {}, ".cache": {}, ".git": {}, ".hg": {}, ".next": {},
	".svn": {}, ".tool-results": {}, ".venv": {}, "__pycache__": {},
	"build": {}, "dist": {}, "node_modules": {},
}

var binaryExtensions = map[string]struct{}{
	".7z": {}, ".avif": {}, ".bmp": {}, ".class": {}, ".db": {}, ".dll": {},
	".dmg": {}, ".doc": {}, ".docx": {}, ".exe": {}, ".gif": {}, ".gz": {},
	".ico": {}, ".jar": {}, ".jpeg": {}, ".jpg": {}, ".mov": {}, ".mp3": {},
	".mp4": {}, ".o": {}, ".pdf": {}, ".png": {}, ".pyc": {}, ".so": {},
	".tar": {}, ".webp": {}, ".xls": {}, ".xlsx": {}, ".zip": {},
}

var sensitivePatterns = []string{
	".env", ".env.*", "*api_key*", "*apikey*", "*.key", "*.pem",
	"*credential*", "*password*", "*private_key*", "*secret*", "*token*",
}

type scanRoot struct {
	name          string
	hostPath      string
	virtualPrefix string
}

type scanOptions struct {
	limits       Limits
	includeText  bool
	textPaths    map[string]struct{}
	textCacheDir string
}

// Capture records a bounded baseline of a thread's workspace and outputs.
// Uploads are intentionally outside the review surface.
func Capture(thread *workspace.Thread, limits Limits) (*Snapshot, error) {
	resolved, roots, err := prepareScan(thread, limits)
	if err != nil {
		return nil, err
	}
	cacheDir, err := os.MkdirTemp("", "gofer-workspace-changes-")
	if err != nil {
		return nil, fmt.Errorf("create workspace change cache: %w", err)
	}
	snapshot, err := scan(roots, scanOptions{limits: resolved, includeText: true, textCacheDir: cacheDir})
	if err != nil {
		_ = os.RemoveAll(cacheDir)
		return nil, err
	}
	snapshot.cacheDir = cacheDir
	return snapshot, nil
}

// CompareCurrent captures only changed post-run text and compares it with a
// baseline. The caller retains ownership of before and must close it.
func CompareCurrent(thread *workspace.Thread, before *Snapshot, limits Limits) (Result, error) {
	result, _, err := ReviewCurrent(thread, before, limits)
	return result, err
}

// ReviewCurrent compares a baseline and independently returns every changed
// regular output within the scan bound. Produced paths are not limited by the
// smaller user-facing file-detail bound.
func ReviewCurrent(thread *workspace.Thread, before *Snapshot, limits Limits) (Result, []string, error) {
	if before == nil {
		return Result{}, nil, fmt.Errorf("%w: baseline is required", ErrInvalidWorkspace)
	}
	resolved, roots, err := prepareScan(thread, limits)
	if err != nil {
		return Result{}, nil, err
	}
	metadata, err := scan(roots, scanOptions{limits: resolved})
	if err != nil {
		return Result{}, nil, err
	}
	paths := changedPaths(before, metadata)
	after, err := scan(roots, scanOptions{limits: resolved, includeText: true, textPaths: paths})
	if err != nil {
		return Result{}, nil, err
	}
	result, err := Compare(before, after, resolved)
	return result, changedRegularOutputPaths(before, metadata), err
}

func prepareScan(thread *workspace.Thread, limits Limits) (Limits, []scanRoot, error) {
	resolved, err := limits.normalized()
	if err != nil {
		return Limits{}, nil, err
	}
	if thread == nil {
		return Limits{}, nil, fmt.Errorf("%w: thread is required", ErrInvalidWorkspace)
	}
	roots := make([]scanRoot, 0, 2)
	for _, mount := range thread.ExecutionMounts() {
		switch mount.VirtualPath {
		case workspace.WorkspaceRoot:
			roots = append(roots, scanRoot{name: "workspace", hostPath: mount.HostPath, virtualPrefix: mount.VirtualPath})
		case workspace.OutputsRoot:
			roots = append(roots, scanRoot{name: "outputs", hostPath: mount.HostPath, virtualPrefix: mount.VirtualPath})
		}
	}
	if len(roots) != 2 {
		return Limits{}, nil, fmt.Errorf("%w: workspace and outputs roots are required", ErrInvalidWorkspace)
	}
	return resolved, roots, nil
}

func scan(roots []scanRoot, options scanOptions) (*Snapshot, error) {
	snapshot := &Snapshot{Files: make(map[string]FileSnapshot)}
	scanned := 0
	for _, root := range roots {
		truncated, err := scanRootFiles(root, options, snapshot.Files, &scanned)
		if err != nil {
			return nil, err
		}
		if truncated {
			snapshot.Truncated = true
			break
		}
	}
	return snapshot, nil
}

func scanRootFiles(root scanRoot, options scanOptions, files map[string]FileSnapshot, scanned *int) (bool, error) {
	if _, err := os.Stat(root.hostPath); errorsIsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("inspect workspace root: %w", err)
	}
	scanner := rootScanner{root: root, options: options, files: files, scanned: scanned}
	err := filepath.WalkDir(root.hostPath, scanner.visit)
	return scanner.truncated, err
}

type rootScanner struct {
	root      scanRoot
	options   scanOptions
	files     map[string]FileSnapshot
	scanned   *int
	truncated bool
}

func (scanner *rootScanner) visit(hostPath string, entry fs.DirEntry, walkErr error) error {
	if walkErr != nil {
		if errorsIsNotExist(walkErr) {
			return nil
		}
		return walkErr
	}
	if hostPath == scanner.root.hostPath {
		return nil
	}
	if entry.IsDir() {
		if ignoredDirectory(entry.Name()) {
			return fs.SkipDir
		}
		return nil
	}
	if *scanner.scanned >= scanner.options.limits.MaxScannedFiles {
		scanner.truncated = true
		return fs.SkipAll
	}
	return scanner.addFile(hostPath)
}

func (scanner *rootScanner) addFile(hostPath string) error {
	snapshot, err := snapshotPath(scanner.root, hostPath, scanner.options)
	if err != nil {
		if errorsIsNotExist(err) {
			return nil
		}
		return err
	}
	if snapshot != nil {
		scanner.files[snapshot.Path] = *snapshot
		(*scanner.scanned)++
	}
	return nil
}

func snapshotPath(root scanRoot, hostPath string, options scanOptions) (*FileSnapshot, error) {
	info, err := os.Lstat(hostPath)
	if err != nil {
		return nil, err
	}
	relative, err := filepath.Rel(root.hostPath, hostPath)
	if err != nil {
		return nil, err
	}
	virtualPath := root.virtualPrefix + "/" + filepath.ToSlash(relative)
	if info.Mode()&os.ModeSymlink != 0 {
		return snapshotSymlink(root.name, virtualPath, hostPath, info), nil
	}
	if !info.Mode().IsRegular() {
		return nil, nil
	}
	return snapshotRegular(root.name, virtualPath, hostPath, info, options)
}

func snapshotSymlink(root, virtualPath, hostPath string, info fs.FileInfo) *FileSnapshot {
	target, _ := os.Readlink(hostPath)
	return &FileSnapshot{
		Path: virtualPath, Root: root, Size: info.Size(), ModifiedNanos: info.ModTime().UnixNano(),
		Sensitive: isSensitivePath(virtualPath), ContentUnavailableReason: ReasonSymlink,
		Symlink: true, SymlinkTarget: target,
	}
}

func snapshotRegular(root, virtualPath, hostPath string, info fs.FileInfo, options scanOptions) (*FileSnapshot, error) {
	snapshot := &FileSnapshot{
		Path: virtualPath, Root: root, Size: info.Size(), ModifiedNanos: info.ModTime().UnixNano(),
		Sensitive: isSensitivePath(virtualPath),
	}
	if snapshot.Sensitive {
		snapshot.ContentUnavailableReason = ReasonSensitive
		return snapshot, nil
	}
	sample, err := readPrefix(hostPath, sampleBytes)
	if err != nil {
		return nil, err
	}
	snapshot.Binary = isBinaryPath(hostPath, sample)
	if snapshot.Binary {
		snapshot.ContentUnavailableReason = ReasonBinary
	}
	if info.Size() > options.limits.MaxFileBytesForDiff {
		if !snapshot.Binary {
			snapshot.ContentUnavailableReason = ReasonLarge
		}
		return snapshot, nil
	}
	data, err := readBoundedFile(hostPath, options.limits.MaxFileBytesForDiff)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(data)
	snapshot.SHA256 = hex.EncodeToString(digest[:])
	if snapshot.Binary || !shouldLoadText(virtualPath, options) {
		return snapshot, nil
	}
	text, ok := decodeText(data)
	if !ok {
		snapshot.Binary = true
		snapshot.ContentUnavailableReason = ReasonBinary
		return snapshot, nil
	}
	if options.textCacheDir == "" {
		snapshot.text = text
		return snapshot, nil
	}
	snapshot.textPath, err = cacheText(options.textCacheDir, virtualPath, text)
	return snapshot, err
}

func shouldLoadText(virtualPath string, options scanOptions) bool {
	if !options.includeText {
		return false
	}
	if options.textPaths == nil {
		return true
	}
	_, ok := options.textPaths[virtualPath]
	return ok
}

func cacheText(directory, virtualPath, text string) (string, error) {
	digest := sha256.Sum256([]byte(virtualPath))
	target := filepath.Join(directory, hex.EncodeToString(digest[:]))
	if err := os.WriteFile(target, []byte(text), 0o600); err != nil {
		return "", fmt.Errorf("cache workspace text: %w", err)
	}
	return target, nil
}

func readPrefix(filename string, limit int64) ([]byte, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	return io.ReadAll(io.LimitReader(file, limit))
}

func readBoundedFile(filename string, limit int64) ([]byte, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file grew beyond workspace diff limit")
	}
	return data, nil
}

func isBinaryPath(filename string, sample []byte) bool {
	if _, ok := binaryExtensions[strings.ToLower(filepath.Ext(filename))]; ok {
		return true
	}
	if hasUTF16BOM(sample) {
		_, ok := decodeUTF16(sample)
		return !ok
	}
	return bytes.IndexByte(sample, 0) >= 0 || !validUTF8Prefix(sample)
}

func validUTF8Prefix(data []byte) bool {
	if utf8.Valid(data) {
		return true
	}
	for suffix := 1; suffix <= 3 && suffix < len(data); suffix++ {
		prefix, tail := data[:len(data)-suffix], data[len(data)-suffix:]
		if utf8.Valid(prefix) && !utf8.FullRune(tail) {
			return true
		}
	}
	return false
}

func decodeText(data []byte) (string, bool) {
	if hasUTF16BOM(data) {
		return decodeUTF16(data)
	}
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	if !utf8.Valid(data) {
		return "", false
	}
	return string(data), true
}

func hasUTF16BOM(data []byte) bool {
	return len(data) >= 2 && (data[0] == 0xff && data[1] == 0xfe || data[0] == 0xfe && data[1] == 0xff)
}

func decodeUTF16(data []byte) (string, bool) {
	if len(data) < 2 || len(data)%2 != 0 {
		return "", false
	}
	var order binary.ByteOrder = binary.LittleEndian
	if data[0] == 0xfe {
		order = binary.BigEndian
	}
	units := make([]uint16, 0, (len(data)-2)/2)
	for index := 2; index < len(data); index += 2 {
		units = append(units, order.Uint16(data[index:index+2]))
	}
	if !validUTF16Units(units) {
		return "", false
	}
	return string(utf16.Decode(units)), true
}

func validUTF16Units(units []uint16) bool {
	for index := 0; index < len(units); index++ {
		unit := units[index]
		if unit >= 0xdc00 && unit <= 0xdfff {
			return false
		}
		if unit >= 0xd800 && unit <= 0xdbff {
			if index+1 >= len(units) || units[index+1] < 0xdc00 || units[index+1] > 0xdfff {
				return false
			}
			index++
		}
	}
	return true
}

func isSensitivePath(virtualPath string) bool {
	normalized := strings.ToLower(virtualPath)
	parts := strings.Split(strings.Trim(normalized, "/"), "/")
	for _, pattern := range sensitivePatterns {
		if matchedPathPattern(pattern, normalized, parts) {
			return true
		}
	}
	return false
}

func matchedPathPattern(pattern, normalized string, parts []string) bool {
	if matched, _ := path.Match(pattern, path.Base(normalized)); matched {
		return true
	}
	for _, part := range parts {
		if matched, _ := path.Match(pattern, part); matched {
			return true
		}
	}
	return false
}

func ignoredDirectory(name string) bool {
	_, ignored := excludedDirectoryNames[name]
	return ignored
}

func errorsIsNotExist(err error) bool { return err != nil && os.IsNotExist(err) }
