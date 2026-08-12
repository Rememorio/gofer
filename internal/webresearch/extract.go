package webresearch

import (
	"fmt"
	"io"
	"strings"
	"unicode"

	"golang.org/x/net/html"
)

var skippedElements = map[string]struct{}{
	"script": {}, "style": {}, "noscript": {}, "svg": {}, "template": {},
	"nav": {}, "footer": {}, "form": {}, "button": {},
}

const maxHTMLDepth = 256

func extractHTML(reader io.Reader) (string, string, error) {
	document, err := html.Parse(reader)
	if err != nil {
		return "", "", fmt.Errorf("%w: parse HTML: %w", ErrUpstream, err)
	}
	title := cleanInline(textOf(firstElement(document, "title")))
	root := firstElement(document, "article", "main", "body")
	if root == nil {
		root = document
	}
	var builder strings.Builder
	renderNode(&builder, root)
	return title, normalizeDocument(builder.String()), nil
}

func firstElement(root *html.Node, names ...string) *html.Node {
	for _, name := range names {
		if found := findElement(root, name); found != nil {
			return found
		}
	}
	return nil
}

func findElement(node *html.Node, name string) *html.Node {
	return findElementAtDepth(node, name, 0)
}

func findElementAtDepth(node *html.Node, name string, depth int) *html.Node {
	if node == nil || depth > maxHTMLDepth {
		return nil
	}
	if node.Type == html.ElementNode && node.Data == name {
		return node
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if found := findElementAtDepth(child, name, depth+1); found != nil {
			return found
		}
	}
	return nil
}

func textOf(node *html.Node) string {
	if node == nil {
		return ""
	}
	var builder strings.Builder
	collectText(&builder, node, 0)
	return builder.String()
}

func collectText(builder *strings.Builder, node *html.Node, depth int) {
	if depth > maxHTMLDepth {
		return
	}
	if node.Type == html.TextNode {
		builder.WriteString(node.Data)
		builder.WriteByte(' ')
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		collectText(builder, child, depth+1)
	}
}

func renderNode(builder *strings.Builder, node *html.Node) {
	renderNodeAtDepth(builder, node, 0)
}

func renderNodeAtDepth(builder *strings.Builder, node *html.Node, depth int) {
	if depth > maxHTMLDepth {
		return
	}
	if node.Type == html.ElementNode {
		if _, skip := skippedElements[node.Data]; skip {
			return
		}
		writeElementPrefix(builder, node.Data)
	}
	if node.Type == html.TextNode {
		builder.WriteString(node.Data)
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		renderNodeAtDepth(builder, child, depth+1)
	}
	if node.Type == html.ElementNode && blockElement(node.Data) {
		builder.WriteString("\n\n")
	}
}

func writeElementPrefix(builder *strings.Builder, name string) {
	switch name {
	case "br":
		builder.WriteByte('\n')
	case "li":
		builder.WriteString("\n- ")
	case "h1":
		builder.WriteString("\n\n# ")
	case "h2":
		builder.WriteString("\n\n## ")
	case "h3", "h4", "h5", "h6":
		builder.WriteString("\n\n### ")
	}
}

func blockElement(name string) bool {
	switch name {
	case "p", "div", "section", "article", "main", "aside", "blockquote", "pre", "ul", "ol", "table", "tr", "h1", "h2", "h3", "h4", "h5", "h6":
		return true
	default:
		return false
	}
}

func normalizeDocument(value string) string {
	lines := strings.Split(strings.ReplaceAll(value, "\r", ""), "\n")
	cleaned := make([]string, 0, len(lines))
	empty := true
	for _, line := range lines {
		line = cleanInline(line)
		if line == "" {
			if !empty {
				cleaned = append(cleaned, "")
				empty = true
			}
			continue
		}
		cleaned = append(cleaned, line)
		empty = false
	}
	return strings.TrimSpace(strings.Join(cleaned, "\n"))
}

func cleanInline(value string) string {
	return strings.Join(strings.FieldsFunc(value, unicode.IsSpace), " ")
}
