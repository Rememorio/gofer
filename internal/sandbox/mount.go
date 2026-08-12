package sandbox

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

func resolveLocalDirectory(virtualPath string, mounts []Mount) (string, error) {
	if virtualPath == "" {
		virtualPath = "/mnt/user-data/workspace"
	}
	if !validTarget(virtualPath) {
		return "", fmt.Errorf("%w: working directory must be a clean absolute sandbox path", ErrInvalidCommand)
	}
	for _, mount := range mounts {
		relative, matches := relativeToMount(virtualPath, mount.Target)
		if !matches {
			continue
		}
		candidate := filepath.Join(mount.Source, filepath.FromSlash(relative))
		resolved, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			return "", fmt.Errorf("%w: resolve working directory: %w", ErrInvalidCommand, err)
		}
		if !withinDirectory(resolved, mount.Source) {
			return "", fmt.Errorf("%w: working directory escapes mount", ErrInvalidCommand)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return "", err
		}
		if !info.IsDir() {
			return "", fmt.Errorf("%w: working directory is not a directory", ErrInvalidCommand)
		}
		return resolved, nil
	}
	return "", fmt.Errorf("%w: working directory is outside configured mounts", ErrInvalidCommand)
}

func relativeToMount(value, root string) (string, bool) {
	if value == root {
		return "", true
	}
	if strings.HasPrefix(value, root+"/") {
		return strings.TrimPrefix(value, root+"/"), true
	}
	return "", false
}

func withinDirectory(candidate, root string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func rewriteVirtualPaths(script string, mounts []Mount) string {
	var output strings.Builder
	quote := byte(0)
	for index := 0; index < len(script); {
		if script[index] == '\\' && quote != '\'' && index+1 < len(script) {
			output.WriteString(script[index : index+2])
			index += 2
			continue
		}
		if script[index] == '\'' || script[index] == '"' {
			if quote == 0 {
				quote = script[index]
			} else if quote == script[index] {
				quote = 0
			}
			output.WriteByte(script[index])
			index++
			continue
		}
		mount, found := matchingMount(script, index, mounts)
		if !found {
			output.WriteByte(script[index])
			index++
			continue
		}
		output.WriteString(quoteHostPath(mount.Source, quote))
		index += len(mount.Target)
	}
	return output.String()
}

func matchingMount(script string, index int, mounts []Mount) (Mount, bool) {
	for _, mount := range mounts {
		if !strings.HasPrefix(script[index:], mount.Target) || !pathBoundary(script, index, len(mount.Target)) {
			continue
		}
		return mount, true
	}
	return Mount{}, false
}

func pathBoundary(script string, index, length int) bool {
	if index > 0 && isPathCharacter(script[index-1]) {
		return false
	}
	next := index + length
	return next == len(script) || script[next] == '/' || !isPathCharacter(script[next])
}

func isPathCharacter(value byte) bool {
	return value == '_' || value == '-' || value == '.' || value == '/' ||
		(value >= '0' && value <= '9') || (value >= 'A' && value <= 'Z') || (value >= 'a' && value <= 'z')
}

func quoteHostPath(value string, quote byte) string {
	switch quote {
	case '\'':
		return "'" + shellQuote(value) + "'"
	case '"':
		replacer := strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "$", "\\$", "`", "\\`")
		return replacer.Replace(value)
	default:
		return shellQuote(value)
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func maskHostPaths(result Result, mounts []Mount) Result {
	sorted := append([]Mount(nil), mounts...)
	sort.Slice(sorted, func(left, right int) bool { return len(sorted[left].Source) > len(sorted[right].Source) })
	for _, mount := range sorted {
		result.Stdout = replaceHostPath(result.Stdout, mount.Source, mount.Target)
		result.Stderr = replaceHostPath(result.Stderr, mount.Source, mount.Target)
	}
	return result
}

func replaceHostPath(value, host, virtual string) string {
	var output strings.Builder
	for offset := 0; offset < len(value); {
		relative := strings.Index(value[offset:], host)
		if relative < 0 {
			output.WriteString(value[offset:])
			break
		}
		index := offset + relative
		end := index + len(host)
		if hostPathBoundary(value, index, end) {
			output.WriteString(value[offset:index])
			output.WriteString(virtual)
			offset = end
			continue
		}
		output.WriteString(value[offset:end])
		offset = end
	}
	return output.String()
}

func hostPathBoundary(value string, start, end int) bool {
	if start > 0 && isPathCharacter(value[start-1]) {
		return false
	}
	return end == len(value) || value[end] == byte(filepath.Separator) || !isPathCharacter(value[end])
}

func cleanContainerWorkingDirectory(value string, mounts []Mount) (string, error) {
	if value == "" {
		value = "/mnt/user-data/workspace"
	}
	if !validTarget(value) {
		return "", fmt.Errorf("%w: invalid container working directory", ErrInvalidCommand)
	}
	for _, mount := range mounts {
		if _, matches := relativeToMount(value, mount.Target); matches {
			return path.Clean(value), nil
		}
	}
	return "", fmt.Errorf("%w: working directory is outside configured mounts", ErrInvalidCommand)
}
