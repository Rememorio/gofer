package tooloutput

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	maxSynopsisInputBytes = 5_000_000
	maxSynopsisItems      = 18
	maxExcerptCharacters  = 420
)

var (
	codeDeclaration = regexp.MustCompile(`(?m)^\s*(?:func|function|def|class|type|interface|struct)\s+([A-Za-z_][A-Za-z0-9_]*)`)
	codeImport      = regexp.MustCompile(`(?m)^\s*(?:import\s+(?:\([^)]*\)|["']?([A-Za-z0-9_./-]+))|from\s+([A-Za-z0-9_./-]+)\s+import|#include\s*[<"]([^>"]+))`)
)

// Synopsis is a deterministic, network-free summary of one tool result.
type Synopsis struct {
	Kind         string
	Title        string
	Summary      []string
	Structure    []string
	NotableItems []string
	Sample       string
}

// BuildSynopsis classifies content and extracts bounded structural signals.
func BuildSynopsis(content, toolName string) Synopsis {
	if content == "" {
		return Synopsis{Kind: "unknown", Title: "Empty output", Summary: []string{"The tool returned an empty string."}}
	}
	if len(content) > maxSynopsisInputBytes {
		return Synopsis{Kind: "unknown", Title: "Oversized output",
			Summary: []string{fmt.Sprintf("The output has %d characters (%.1f MB); structural parsing was skipped.", utf8.RuneCountInString(content), float64(len(content))/(1<<20))},
			Sample:  headTailSample(content, maxExcerptCharacters, maxExcerptCharacters)}
	}
	if synopsis, ok := jsonSynopsis(content); ok {
		return synopsis
	}
	if synopsis, ok := xmlSynopsis(content); ok {
		return synopsis
	}
	if synopsis, ok := tableSynopsis(content, '\t', "tsv"); ok {
		return synopsis
	}
	if synopsis, ok := tableSynopsis(content, ',', "csv"); ok {
		return synopsis
	}
	if synopsis, ok := yamlSynopsis(content); ok {
		return synopsis
	}
	if synopsis, ok := sourceSynopsis(content); ok {
		return synopsis
	}
	return textSynopsis(content, toolName)
}

// RenderPreview renders an externalized output synopsis, bounded raw sample,
// and an agent-readable file reference.
func RenderPreview(content, toolName, virtualPath string, headChars, tailChars int) string {
	synopsis := BuildSynopsis(content, toolName)
	lines := []string{
		fmt.Sprintf("[Full %s output saved to %s (%d chars, ~%d tokens).]", toolName, virtualPath, utf8.RuneCountInString(content), len(content)/4),
		fmt.Sprintf("[Preview kind: %s. This is a structured synopsis, not the complete result.]", synopsis.Kind),
		"", synopsis.Title + ":",
	}
	lines = appendBullets(lines, synopsis.Summary)
	if len(synopsis.Structure) > 0 {
		lines = append(lines, "", "Structure:")
		lines = appendBullets(lines, synopsis.Structure)
	}
	if len(synopsis.NotableItems) > 0 {
		lines = append(lines, "", "Notable items:")
		lines = appendBullets(lines, synopsis.NotableItems)
	}
	sample := synopsis.Sample
	if sample == "" {
		sample = headTailSample(content, headChars, tailChars)
	}
	if sample != "" {
		lines = append(lines, "", "Raw sample (head + tail):", sample)
	}
	lines = append(lines, "", "Access:", "- Use read_file on "+virtualPath+" with start_line and end_line to inspect the complete output.")
	return strings.Join(lines, "\n")
}

func jsonSynopsis(content string) (Synopsis, bool) {
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return Synopsis{}, false
	}
	synopsis := Synopsis{Kind: "json", Title: "JSON output"}
	switch typed := value.(type) {
	case map[string]any:
		keys := slices.Sorted(maps.Keys(typed))
		synopsis.Summary = []string{fmt.Sprintf("JSON object with %d top-level keys.", len(keys)), "Top-level keys: " + strings.Join(clippedStrings(keys), ", ")}
		for _, key := range clippedStrings(keys) {
			synopsis.Structure = append(synopsis.Structure, key+": "+shape(typed[key]))
			if scalar, ok := scalarText(typed[key]); ok {
				synopsis.NotableItems = append(synopsis.NotableItems, "$."+key+": "+scalar)
			}
		}
	case []any:
		synopsis.Summary = []string{fmt.Sprintf("JSON array with %d items.", len(typed))}
		if len(typed) > 0 {
			synopsis.Structure = []string{"first item: " + shape(typed[0])}
		}
	default:
		synopsis.Summary = []string{"JSON scalar: " + shape(typed)}
	}
	return synopsis, true
}

func xmlSynopsis(content string) (Synopsis, bool) {
	decoder := xml.NewDecoder(strings.NewReader(content))
	depth := 0
	root := ""
	children := make(map[string]int)
	for tokens := 0; tokens < 10_000; tokens++ {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Synopsis{}, false
		}
		switch typed := token.(type) {
		case xml.StartElement:
			depth++
			if depth == 1 {
				root = typed.Name.Local
			} else if depth == 2 {
				children[typed.Name.Local]++
			}
		case xml.EndElement:
			depth--
		}
	}
	if root == "" {
		return Synopsis{}, false
	}
	keys := slices.Sorted(maps.Keys(children))
	structure := []string{"root tag: " + root}
	for _, key := range clippedStrings(keys) {
		structure = append(structure, fmt.Sprintf("%s: %d", key, children[key]))
	}
	return Synopsis{Kind: "xml", Title: "XML output", Summary: []string{"XML document with root tag " + root + "."}, Structure: structure}, true
}

func tableSynopsis(content string, delimiter rune, kind string) (Synopsis, bool) {
	if !strings.ContainsRune(content, delimiter) || delimiter == '\t' && hasTabIndentedLines(content) {
		return Synopsis{}, false
	}
	reader := csv.NewReader(strings.NewReader(content))
	reader.Comma, reader.FieldsPerRecord, reader.LazyQuotes = delimiter, -1, true
	rows := make([][]string, 0, 51)
	for len(rows) < 51 {
		row, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Synopsis{}, false
		}
		rows = append(rows, row)
	}
	if len(rows) < 3 || len(rows[0]) < 2 {
		return Synopsis{}, false
	}
	columns := len(rows[0])
	for _, row := range rows[1:] {
		if len(row) != columns {
			return Synopsis{}, false
		}
	}
	headers := clippedStrings(rows[0])
	first := make([]string, 0, len(headers))
	for index, header := range headers {
		first = append(first, clip(strings.TrimSpace(header), 40)+"="+clip(strings.TrimSpace(rows[1][index]), 80))
	}
	label := strings.ToUpper(kind)
	return Synopsis{Kind: kind, Title: label + " output",
		Summary:   []string{fmt.Sprintf("%s table with at least %d data rows and %d columns.", label, len(rows)-1, columns)},
		Structure: []string{"columns: " + strings.Join(headers, ", "), "first data row: " + strings.Join(first, " | ")}}, true
}

func yamlSynopsis(content string) (Synopsis, bool) {
	var document yaml.Node
	if yaml.Unmarshal([]byte(content), &document) != nil || len(document.Content) == 0 {
		return Synopsis{}, false
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode || len(root.Content) < 4 {
		return Synopsis{}, false
	}
	keys := make([]string, 0, len(root.Content)/2)
	structure := make([]string, 0, len(root.Content)/2)
	seen := make(map[string]struct{}, len(root.Content)/2)
	for index := 0; index+1 < len(root.Content) && len(keys) < maxSynopsisItems; index += 2 {
		key, value := root.Content[index].Value, root.Content[index+1]
		if _, duplicate := seen[key]; duplicate {
			return Synopsis{}, false
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
		structure = append(structure, key+": "+yamlShape(value))
	}
	if logKeyCount(keys)*2 >= len(keys) {
		return Synopsis{}, false
	}
	return Synopsis{Kind: "yaml", Title: "YAML output",
		Summary:   []string{fmt.Sprintf("YAML object with %d top-level keys.", len(root.Content)/2), "Top-level keys: " + strings.Join(keys, ", ")},
		Structure: structure}, true
}

func logKeyCount(keys []string) int {
	count := 0
	for _, key := range keys {
		switch strings.ToUpper(key) {
		case "DEBUG", "ERROR", "FATAL", "INFO", "TRACE", "WARN", "WARNING":
			count++
		}
	}
	return count
}

func sourceSynopsis(content string) (Synopsis, bool) {
	declarations := codeDeclaration.FindAllStringSubmatch(content, maxSynopsisItems)
	imports := codeImport.FindAllStringSubmatch(content, maxSynopsisItems)
	if len(declarations) == 0 && len(imports) == 0 {
		return Synopsis{}, false
	}
	importNames := make([]string, 0, len(imports))
	for _, match := range imports {
		for _, candidate := range match[1:] {
			if candidate != "" {
				importNames = append(importNames, candidate)
				break
			}
		}
	}
	structure := []string{fmt.Sprintf("line count: %d", lineCount(content))}
	if len(importNames) > 0 {
		structure = append(structure, "imports: "+strings.Join(importNames, ", "))
	}
	notable := make([]string, 0, len(declarations))
	for _, declaration := range declarations {
		notable = append(notable, strings.TrimSpace(declaration[0]))
	}
	return Synopsis{Kind: "code", Title: "Source code output", Summary: []string{fmt.Sprintf("Detected %d declarations.", len(declarations))}, Structure: structure, NotableItems: notable}, true
}

func textSynopsis(content, toolName string) Synopsis {
	lines := strings.Split(content, "\n")
	nonempty, warnings, failures := 0, 0, 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		nonempty++
		lower := strings.ToLower(trimmed)
		if strings.Contains(lower, "warn") {
			warnings++
		}
		if strings.Contains(lower, "error") || strings.Contains(lower, "failed") || strings.Contains(lower, "fatal") {
			failures++
		}
	}
	summary := []string{fmt.Sprintf("Text output from %s with %d lines (%d non-empty).", displayToolName(toolName), len(lines), nonempty)}
	if warnings+failures > 0 {
		summary = append(summary, fmt.Sprintf("Detected %d warning-like and %d failure-like lines.", warnings, failures))
	}
	return Synopsis{Kind: "text", Title: "Text output", Summary: summary}
}

func shape(value any) string {
	switch typed := value.(type) {
	case map[string]any:
		return fmt.Sprintf("object with %d keys", len(typed))
	case []any:
		return fmt.Sprintf("array length %d", len(typed))
	case string:
		return fmt.Sprintf("string length %d", utf8.RuneCountInString(typed))
	case nil:
		return "null"
	default:
		return fmt.Sprintf("%T", typed)
	}
}

func scalarText(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return fmt.Sprintf("%q", clip(typed, 100)), true
	case json.Number:
		return typed.String(), true
	case bool:
		return fmt.Sprintf("%t", typed), true
	case nil:
		return "null", true
	default:
		return "", false
	}
}

func yamlShape(node *yaml.Node) string {
	switch node.Kind {
	case yaml.MappingNode:
		return "object"
	case yaml.SequenceNode:
		return "array"
	case yaml.ScalarNode:
		return node.Tag
	case yaml.DocumentNode:
		return "document"
	case yaml.AliasNode:
		return "alias"
	default:
		return "unknown"
	}
}

func hasTabIndentedLines(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "\t") {
			return true
		}
	}
	return false
}

func headTailSample(content string, headCharacters, tailCharacters int) string {
	runes := []rune(content)
	headCharacters, tailCharacters = max(0, headCharacters), max(0, tailCharacters)
	if headCharacters+tailCharacters == 0 {
		return ""
	}
	if len(runes) <= headCharacters+tailCharacters {
		return content
	}
	head := strings.TrimSuffix(string(runes[:headCharacters]), "\n")
	tail := strings.TrimPrefix(string(runes[len(runes)-tailCharacters:]), "\n")
	if head == "" {
		return tail
	}
	if tail == "" {
		return head
	}
	return head + "\n...\n" + tail
}

func appendBullets(lines, items []string) []string {
	for _, item := range clippedStrings(items) {
		lines = append(lines, "- "+item)
	}
	return lines
}

func clippedStrings(values []string) []string {
	if len(values) <= maxSynopsisItems {
		return values
	}
	return values[:maxSynopsisItems]
}

func clip(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	if limit <= 3 {
		return string(runes[:limit])
	}
	return string(runes[:limit-3]) + "..."
}

func lineCount(content string) int {
	return bytes.Count([]byte(content), []byte{'\n'}) + 1
}
