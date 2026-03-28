package jira

import "strings"

// blockLevelTypes is the set of ADF node types that produce a trailing
// newline when flattened to plain text. This covers structural blocks
// defined by the Atlassian Document Format specification.
var blockLevelTypes = map[string]bool{
	"paragraph":    true,
	"heading":      true,
	"bulletList":   true,
	"orderedList":  true,
	"listItem":     true,
	"blockquote":   true,
	"codeBlock":    true,
	"rule":         true,
	"mediaSingle":  true,
	"mediaGroup":   true,
	"table":        true,
	"tableRow":     true,
	"tableCell":    true,
	"tableHeader":  true,
	"panel":        true,
	"decisionList": true,
	"decisionItem": true,
	"taskList":     true,
	"taskItem":     true,
}

// flattenADF recursively extracts plain text from an Atlassian
// Document Format node tree. The node parameter is the result of
// unmarshaling ADF JSON into any via [encoding/json]. Block-level
// nodes receive a trailing newline; text nodes yield their text value.
// Nil or non-map input returns an empty string. Trailing whitespace
// is trimmed from the final result.
func flattenADF(node any) string {
	var b strings.Builder
	flattenADFNode(&b, node)
	return strings.TrimRight(b.String(), "\n ")
}

func flattenADFNode(b *strings.Builder, node any) {
	m, ok := node.(map[string]any)
	if !ok || m == nil {
		return
	}

	nodeType, _ := m["type"].(string)

	if nodeType == "text" {
		text, _ := m["text"].(string)
		b.WriteString(text)
		return
	}

	if content, ok := m["content"].([]any); ok {
		for _, child := range content {
			flattenADFNode(b, child)
		}
	}

	if blockLevelTypes[nodeType] {
		b.WriteByte('\n')
	}
}

// buildADFComment wraps plain text in an ADF document suitable for
// the Jira REST API v3 comment endpoint. Each line of the input text
// becomes a separate paragraph node so that line breaks render
// correctly in Jira's UI.
func buildADFComment(text string) map[string]any {
	lines := strings.Split(text, "\n")
	paragraphs := make([]any, 0, len(lines))

	for _, line := range lines {
		var content []any
		if line != "" {
			content = []any{
				map[string]any{
					"type": "text",
					"text": line,
				},
			}
		} else {
			content = []any{}
		}
		paragraphs = append(paragraphs, map[string]any{
			"type":    "paragraph",
			"content": content,
		})
	}

	return map[string]any{
		"body": map[string]any{
			"version": 1,
			"type":    "doc",
			"content": paragraphs,
		},
	}
}
