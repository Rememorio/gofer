package skill

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Project atomically copies enabled packages into a read-only local view.
func (catalog *Catalog) Project(ctx context.Context, destination string) error {
	if catalog == nil {
		return fmt.Errorf("%w: catalog is nil", ErrInvalidConfig)
	}
	absolute, err := filepath.Abs(destination)
	if err != nil {
		return fmt.Errorf("resolve skill projection: %w", err)
	}
	if absolute == filepath.VolumeName(absolute)+string(filepath.Separator) || absolute == catalog.root {
		return fmt.Errorf("%w: unsafe projection destination", ErrInvalidConfig)
	}
	parent := filepath.Dir(absolute)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(parent, ".gofer-skills-")
	if err != nil {
		return err
	}
	keepStaging := false
	defer func() {
		if !keepStaging {
			_ = removeProjectedDirectory(staging)
		}
	}()

	records := catalog.enabledRecords()
	for _, candidate := range records {
		target := filepath.Join(staging, string(candidate.skill.Category), filepath.FromSlash(candidate.skill.RelativePath))
		if err := copyPackage(ctx, candidate.hostDir, target, catalog.maxPackageBytes); err != nil {
			return fmt.Errorf("project skill %s: %w", candidate.skill.Name, err)
		}
	}
	if err := os.Chmod(staging, 0o555); err != nil {
		return err
	}
	if err := replaceDirectory(staging, absolute); err != nil {
		return err
	}
	keepStaging = true
	return nil
}

func (catalog *Catalog) enabledRecords() []record {
	catalog.mu.RLock()
	records := make([]record, 0, len(catalog.records))
	for _, candidate := range catalog.records {
		if candidate.skill.Enabled {
			records = append(records, candidate)
		}
	}
	catalog.mu.RUnlock()
	sort.Slice(records, func(left, right int) bool {
		if records[left].skill.Category == records[right].skill.Category {
			return records[left].skill.RelativePath < records[right].skill.RelativePath
		}
		return records[left].skill.Category < records[right].skill.Category
	})
	return records
}

func copyPackage(ctx context.Context, source, target string, maxBytes int64) error {
	root, err := os.OpenRoot(source)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	var total int64
	err = fs.WalkDir(root.FS(), ".", func(relative string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%w: package contains symlink %s", ErrInvalidSkill, relative)
		}
		destination := filepath.Join(target, filepath.FromSlash(relative))
		if entry.IsDir() {
			return os.MkdirAll(destination, 0o755)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: package contains non-regular file %s", ErrInvalidSkill, relative)
		}
		total += info.Size()
		if total > maxBytes {
			return fmt.Errorf("%w: package exceeds %d bytes", ErrInvalidSkill, maxBytes)
		}
		return copyProjectedFile(root, relative, destination, info.Mode())
	})
	if err != nil {
		return err
	}
	return filepath.WalkDir(target, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return os.Chmod(current, 0o555)
		}
		return nil
	})
}

func copyProjectedFile(root *os.Root, relative, destination string, sourceMode fs.FileMode) error {
	source, err := root.Open(filepath.FromSlash(relative))
	if err != nil {
		return err
	}
	mode := fs.FileMode(0o444)
	if sourceMode&0o111 != 0 {
		mode = 0o555
	}
	target, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		_ = source.Close()
		return err
	}
	_, copyErr := io.Copy(target, source)
	if copyErr == nil {
		copyErr = target.Sync()
	}
	closeErr := errors.Join(source.Close(), target.Close())
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(destination)
		return errors.Join(copyErr, closeErr)
	}
	return nil
}

func replaceDirectory(staging, destination string) error {
	_, err := os.Lstat(destination)
	if errors.Is(err, fs.ErrNotExist) {
		return os.Rename(staging, destination)
	}
	if err != nil {
		return err
	}
	backup, err := uniqueBackupPath(destination)
	if err != nil {
		return err
	}
	if err := os.Rename(destination, backup); err != nil {
		return err
	}
	if err := os.Rename(staging, destination); err != nil {
		restoreErr := os.Rename(backup, destination)
		return errors.Join(err, restoreErr)
	}
	return removeProjectedDirectory(backup)
}

// removeProjectedDirectory makes a read-only projection traversable before removal.
func removeProjectedDirectory(directory string) error {
	if err := os.Chmod(directory, 0o700); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := filepath.WalkDir(directory, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return os.Chmod(current, 0o700)
		}
		return nil
	}); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.RemoveAll(directory)
}

func uniqueBackupPath(destination string) (string, error) {
	parent := filepath.Dir(destination)
	prefix := ".gofer-skills-backup-" + strings.TrimSpace(filepath.Base(destination)) + "-"
	temporary, err := os.MkdirTemp(parent, prefix)
	if err != nil {
		return "", err
	}
	if err := os.Remove(temporary); err != nil {
		return "", err
	}
	return temporary, nil
}
