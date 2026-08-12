package tooloutput

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBuildSynopsisClassifiesStructuredOutput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
		kind    string
		want    string
	}{
		{name: "json", content: `{"items":[1,2],"source":"unit"}`, kind: "json", want: "Top-level keys"},
		{name: "xml", content: `<feed><entry/><entry/><meta/></feed>`, kind: "xml", want: "entry: 2"},
		{name: "csv", content: "name,score\nAda,98\nGrace,99\nAlan,95\n", kind: "csv", want: "columns: name, score"},
		{name: "yaml", content: "name: gofer\nsettings:\n  enabled: true\nitems:\n  - one\n", kind: "yaml", want: "Top-level keys"},
		{name: "code", content: "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(1) }\n", kind: "code", want: "func main"},
		{name: "text", content: "INFO: start\nWARN: retry\nERROR: failed\n", kind: "text", want: "failure-like"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			synopsis := BuildSynopsis(test.content, "bash")
			joined := strings.Join(append(append(synopsis.Summary, synopsis.Structure...), synopsis.NotableItems...), "\n")
			if synopsis.Kind != test.kind || !strings.Contains(joined, test.want) {
				t.Fatalf("BuildSynopsis() = %#v", synopsis)
			}
		})
	}
}

func TestSynopsisAvoidsTabIndentedTableAndBoundsHugeInput(t *testing.T) {
	t.Parallel()
	indented := "tree:\n\tdirectory one\n\tdirectory two\n\tdirectory three\n"
	if synopsis := BuildSynopsis(indented, "bash"); synopsis.Kind == "tsv" {
		t.Fatalf("tab-indented output classified as %#v", synopsis)
	}
	huge := strings.Repeat("x", maxSynopsisInputBytes+1)
	synopsis := BuildSynopsis(huge, "tool")
	if synopsis.Kind != "unknown" || synopsis.Sample == "" || len(synopsis.Sample) > maxExcerptCharacters*2+8 {
		t.Fatalf("huge synopsis = %#v", synopsis)
	}
}

func TestRenderPreviewIncludesBoundedRawSampleAndAccess(t *testing.T) {
	t.Parallel()
	content := "HEAD\n" + strings.Repeat("middle\n", 100) + "TAIL"
	preview := RenderPreview(content, "bash", "/mnt/user-data/outputs/.tool-results/bash-a.log", 20, 10)
	for _, want := range []string{"Preview kind: text", "Raw sample (head + tail)", "HEAD", "TAIL", "read_file", "start_line and end_line"} {
		if !strings.Contains(preview, want) {
			t.Fatalf("preview missing %q:\n%s", want, preview)
		}
	}
}

func TestFallbackAlwaysHonorsCharacterLimit(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("甲乙丙丁\n", 500)
	for _, maximum := range []int{50, 200, 1000} {
		fallback := truncateFallback(content, "bash", maximum, maximum/2, maximum/4)
		if got := utf8.RuneCountInString(fallback); got > maximum {
			t.Fatalf("truncateFallback(%d) = %d chars", maximum, got)
		}
	}
	if got := truncateFallback("short", "tool", 100, 20, 10); got != "short" {
		t.Fatalf("short fallback = %q", got)
	}
}
