package uploads

import (
	"strings"
	"unicode"

	"github.com/Rememorio/gofer/internal/workspace"
)

var structuralPrefixes = []string{
	"ITEM", "PART", "SECTION", "SCHEDULE", "EXHIBIT", "APPENDIX", "ANNEX", "CHAPTER",
}

const (
	maxOutlineTitleCharacters = 300
	maxPreviewCharacters      = 500
)

func extractOutline(thread *workspace.Thread, virtualPath string, maxEntries, maxPreview int) ([]OutlineEntry, []string) {
	content, err := thread.ReadFile(virtualPath, workspace.ReadOptions{})
	if err != nil {
		return nil, nil
	}
	lines := strings.Split(content.Content, "\n")
	outline := make([]OutlineEntry, 0, min(maxEntries, len(lines)))
	for index, line := range lines {
		if title := headingTitle(strings.TrimSpace(line)); title != "" {
			if len(outline) == maxEntries {
				outline = append(outline, OutlineEntry{Truncated: true})
				break
			}
			outline = append(outline, OutlineEntry{Title: truncateCharacters(title, maxOutlineTitleCharacters), Line: index + 1})
		}
	}
	if len(outline) > 0 {
		return outline, nil
	}
	preview := make([]string, 0, maxPreview)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			preview = append(preview, truncateCharacters(line, maxPreviewCharacters))
		}
		if len(preview) == maxPreview {
			break
		}
	}
	return nil, preview
}

func truncateCharacters(value string, limit int) string {
	characters := []rune(value)
	if len(characters) <= limit {
		return value
	}
	return string(characters[:limit-3]) + "..."
}

func headingTitle(line string) string {
	if strings.HasPrefix(line, "#") {
		return cleanBold(strings.TrimSpace(strings.TrimLeft(line, "#")))
	}
	if !strings.HasPrefix(line, "**") || !strings.HasSuffix(line, "**") {
		return ""
	}
	blocks, ok := boldBlocks(line)
	if !ok || len(blocks) == 0 {
		return ""
	}
	if len(blocks) == 1 && structuralHeading(blocks[0]) {
		return strings.TrimSpace(blocks[0])
	}
	if len(blocks) >= 2 && len(blocks) <= 4 && sectionNumber(blocks[0]) && containsLetter(blocks[1]) {
		return strings.Join(blocks, " ")
	}
	return ""
}

func boldBlocks(line string) ([]string, bool) {
	blocks := make([]string, 0, 4)
	for len(line) > 0 {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "**") {
			return nil, false
		}
		line = strings.TrimPrefix(line, "**")
		end := strings.Index(line, "**")
		if end < 0 || end == 0 {
			return nil, false
		}
		blocks = append(blocks, strings.TrimSpace(line[:end]))
		line = line[end+2:]
	}
	return blocks, true
}

func structuralHeading(value string) bool {
	upper := strings.ToUpper(strings.TrimSpace(value))
	for _, prefix := range structuralPrefixes {
		if upper == prefix || strings.HasPrefix(upper, prefix+" ") {
			return true
		}
	}
	return false
}

func sectionNumber(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	for index, character := range value {
		if unicode.IsDigit(character) || character == '.' {
			continue
		}
		if index == 0 && unicode.IsLetter(character) {
			continue
		}
		return false
	}
	return true
}

func containsLetter(value string) bool {
	for _, character := range value {
		if unicode.IsLetter(character) {
			return true
		}
	}
	return false
}

func cleanBold(value string) string {
	value = strings.ReplaceAll(value, "** **", " ")
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "**") && strings.HasSuffix(value, "**") && len(value) > 4 {
		value = strings.TrimSpace(value[2 : len(value)-2])
	}
	return value
}
