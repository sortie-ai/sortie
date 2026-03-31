package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func main() {
	s := server.NewMCPServer("sortie-kb", "0.2.0", server.WithToolCapabilities(false))

	s.AddTool(mcp.NewTool(
		"list_docs",
		mcp.WithDescription(
			"Lists all documentation files available in this knowledge base. "+
				"Returns short names to use with list_sections and get_section.",
		),
	), handleListDocs)

	s.AddTool(mcp.NewTool(
		"list_sections",
		mcp.WithDescription(
			"Returns the table of contents for a documentation file: "+
				"all section identifiers and their titles, indented by nesting level. "+
				"Use the identifier shown in brackets to fetch content with get_section.",
		),
		mcp.WithString("doc",
			mcp.Required(),
			mcp.Description(`Document name from list_docs, e.g. "architecture", "workflow-reference", "decisions/0001-use-go-as-core-runtime"`),
		),
	), handleListSections)

	s.AddTool(mcp.NewTool(
		"search_docs",
		mcp.WithDescription(
			"Full-text search across all documentation files. "+
				"Returns matching sections with the lines that contain the query terms. "+
				"All query words must appear in a line (case-insensitive). "+
				"Use this when you don't know which document or section to look in.",
		),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description(`Space-separated search terms, e.g. "workspace path containment"`),
		),
		mcp.WithString("doc",
			mcp.Description(`Limit search to one document, e.g. "architecture". Omit to search all docs.`),
		),
	), handleSearchDocs)

	s.AddTool(mcp.NewTool(
		"get_section",
		mcp.WithDescription(
			"Returns the content of a section from a documentation file. "+
				"Use list_sections first to discover valid section identifiers.",
		),
		mcp.WithString("doc",
			mcp.Required(),
			mcp.Description(`Document name from list_docs, e.g. "architecture", "decisions/0001-use-go-as-core-runtime"`),
		),
		mcp.WithString("section",
			mcp.Required(),
			mcp.Description(`Section identifier from list_sections, e.g. "3.1", "A", "Overview", "Decision Outcome"`),
		),
	), handleGetSection)

	if err := server.ServeStdio(s); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

// ── handlers ─────────────────────────────────────────────────────────────────

func handleListDocs(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	dir, err := docsDir()
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	names, err := listDocNames(dir)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(strings.Join(names, "\n")), nil
}

func handleListSections(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	doc, err := req.RequireString("doc")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	data, err := readDoc(doc)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	headings := parseHeadings(data)
	if len(headings) == 0 {
		return mcp.NewToolResultError(fmt.Sprintf("no sections found in %q", doc)), nil
	}
	return mcp.NewToolResultText(formatTOC(headings)), nil
}

func handleSearchDocs(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query, err := req.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	terms := strings.Fields(strings.ToLower(strings.TrimSpace(query)))
	if len(terms) == 0 {
		return mcp.NewToolResultError("query must not be empty"), nil
	}

	doc := req.GetString("doc", "")

	var docNames []string
	if doc != "" {
		docNames = []string{doc}
	} else {
		dir, err := docsDir()
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		docNames, err = listDocNames(dir)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
	}

	const maxResults = 20
	var results []searchHit
	for _, name := range docNames {
		data, err := readDoc(name)
		if err != nil {
			continue
		}
		results = append(results, searchDoc(name, data, terms)...)
		if len(results) >= maxResults {
			results = results[:maxResults]
			break
		}
	}

	if len(results) == 0 {
		return mcp.NewToolResultText("no matches found"), nil
	}
	return mcp.NewToolResultText(formatSearchResults(results)), nil
}

// searchHit is one matching section with up to a few representative match lines.
type searchHit struct {
	doc          string
	sectionID    string
	sectionTitle string
	sectionLines int
	matchLines   []string // lines containing the query (up to 3)
	extra        int      // additional matches not shown
}

func searchDoc(name string, data []byte, terms []string) []searchHit {
	lines := strings.Split(string(data), "\n")

	// hits maps section index → list of matching line texts.
	type entry struct {
		h       heading
		matched []string
	}
	var sections []entry
	cur := entry{h: heading{id: "", title: "(preamble)"}}

	inFence := false
	for _, line := range lines {
		if isFenceLine(line) {
			inFence = !inFence
			continue
		}
		if !inFence {
			if h, ok := parseOneLine(line); ok {
				sections = append(sections, cur)
				cur = entry{h: h}
				continue
			}
		}
		if matchesAll(line, terms) {
			cur.matched = append(cur.matched, strings.TrimSpace(line))
		}
	}
	sections = append(sections, cur)

	// Compute line counts (same logic as parseHeadings).
	headings := parseHeadings(data)
	linesByID := make(map[string]int, len(headings))
	for _, h := range headings {
		linesByID[h.id] = h.lines
	}

	var hits []searchHit
	for _, s := range sections {
		if len(s.matched) == 0 {
			continue
		}
		const maxShow = 3
		shown := s.matched
		extra := 0
		if len(shown) > maxShow {
			extra = len(shown) - maxShow
			shown = shown[:maxShow]
		}
		hits = append(hits, searchHit{
			doc:          name,
			sectionID:    s.h.id,
			sectionTitle: s.h.title,
			sectionLines: linesByID[s.h.id],
			matchLines:   shown,
			extra:        extra,
		})
	}
	return hits
}

// matchesAll reports whether line contains all terms (case-insensitive).
func matchesAll(line string, terms []string) bool {
	lower := strings.ToLower(line)
	for _, t := range terms {
		if !strings.Contains(lower, t) {
			return false
		}
	}
	return true
}

func formatSearchResults(hits []searchHit) string {
	var sb strings.Builder
	for _, h := range hits {
		if h.sectionID == "" {
			fmt.Fprintf(&sb, "[%s]  (preamble)\n", h.doc)
		} else {
			fmt.Fprintf(&sb, "[%s]  [%s]  %s  (%d lines)\n", h.doc, h.sectionID, h.sectionTitle, h.sectionLines)
		}
		for _, m := range h.matchLines {
			fmt.Fprintf(&sb, "    > %s\n", m)
		}
		if h.extra > 0 {
			fmt.Fprintf(&sb, "    … +%d more matches\n", h.extra)
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

func handleGetSection(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	doc, err := req.RequireString("doc")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	section, err := req.RequireString("section")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	data, err := readDoc(doc)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	content, err := extractSection(data, strings.TrimSpace(section))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(content), nil
}

// ── document resolution ───────────────────────────────────────────────────────

// docsDir returns the path to the docs/ directory. Checks SORTIE_DOCS_PATH
// first, then walks up from the executable until docs/ is found.
func docsDir() (string, error) {
	if p := os.Getenv("SORTIE_DOCS_PATH"); p != "" {
		return p, nil
	}
	exe, err := os.Executable()
	if err == nil {
		if exe, err = filepath.EvalSymlinks(exe); err == nil {
			for dir := filepath.Dir(exe); ; {
				candidate := filepath.Join(dir, "docs")
				if fi, err := os.Stat(candidate); err == nil && fi.IsDir() {
					return candidate, nil
				}
				parent := filepath.Dir(dir)
				if parent == dir {
					break
				}
				dir = parent
			}
		}
	}
	return "", fmt.Errorf("docs/ directory not found; set SORTIE_DOCS_PATH")
}

func listDocNames(dir string) ([]string, error) {
	var names []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		names = append(names, strings.TrimSuffix(rel, ".md"))
		return nil
	})
	return names, err
}

func readDoc(name string) ([]byte, error) {
	dir, err := docsDir()
	if err != nil {
		return nil, err
	}
	// Prevent path traversal.
	clean := filepath.Clean(name)
	if strings.HasPrefix(clean, "..") {
		return nil, fmt.Errorf("invalid doc name %q", name)
	}
	path := filepath.Join(dir, clean+".md")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read %q: %w", name, err)
	}
	return data, nil
}

// ── heading parsing ───────────────────────────────────────────────────────────

type heading struct {
	level   int
	id      string // what to pass to get_section
	title   string // display label
	lineIdx int    // 0-based line where this heading starts
	lines   int    // total lines get_section would return for this section
}

// numericHeaderRe matches "## 3. Title" and "### 3.1 Title".
// appendixHeaderRe matches "## Appendix A. Title" and "### A.1 Title".
// textHeaderRe matches any remaining markdown heading "## Some Title".
var (
	numericHeaderRe  = regexp.MustCompile(`^(#{1,6})\s+(\d+(?:\.\d+)*)[. ]`)
	appendixHeaderRe = regexp.MustCompile(`^(#{1,6})\s+(?:Appendix\s+)?([A-Z](?:\.\d+)*)[. ]`)
	textHeaderRe     = regexp.MustCompile(`^(#{1,6})\s+(.+)`)
)

func parseHeadings(data []byte) []heading {
	lines := strings.Split(string(data), "\n")
	total := len(lines)

	// First pass: collect headings with their line positions.
	var out []heading
	inFence := false
	for i, line := range lines {
		if isFenceLine(line) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if h, ok := parseOneLine(line); ok {
			h.lineIdx = i
			out = append(out, h)
		}
	}

	// Second pass: compute how many lines get_section would return for each heading.
	// A section spans from its own line to just before the next heading at the same
	// or higher (lower number) level — matching extractSection's stop condition.
	for i := range out {
		end := total
		for j := i + 1; j < len(out); j++ {
			if out[j].level <= out[i].level {
				end = out[j].lineIdx
				break
			}
		}
		out[i].lines = end - out[i].lineIdx
	}
	return out
}

var fenceRe = regexp.MustCompile("^[`~]{3}")

func isFenceLine(line string) bool { return fenceRe.MatchString(line) }

func parseOneLine(line string) (heading, bool) {
	// Numeric: "## 1. Title" or "### 3.1 Title"
	if m := numericHeaderRe.FindStringSubmatchIndex(line); m != nil {
		id := line[m[4]:m[5]]
		title := strings.TrimLeft(strings.TrimSpace(line[m[1]:]), ". ")
		return heading{level: m[3] - m[2], id: id, title: title}, true
	}
	// Appendix: "## Appendix A. Title" or "### A.1 Title"
	if m := appendixHeaderRe.FindStringSubmatchIndex(line); m != nil {
		id := line[m[4]:m[5]]
		title := strings.TrimLeft(strings.TrimSpace(line[m[1]:]), ". ")
		return heading{level: m[3] - m[2], id: id, title: title}, true
	}
	// Plain text heading: "## Overview"
	if m := textHeaderRe.FindStringSubmatch(line); m != nil {
		title := strings.TrimSpace(m[2])
		return heading{level: len(m[1]), id: title, title: title}, true
	}
	return heading{}, false
}

func formatTOC(headings []heading) string {
	if len(headings) == 0 {
		return ""
	}
	// Determine base level so top-most headings get zero indent.
	base := headings[0].level
	for _, h := range headings {
		if h.level < base {
			base = h.level
		}
	}
	var sb strings.Builder
	for _, h := range headings {
		indent := strings.Repeat("  ", h.level-base)
		fmt.Fprintf(&sb, "%s[%s]  %s  (%d lines)\n", indent, h.id, h.title, h.lines)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// ── section extraction ────────────────────────────────────────────────────────

func extractSection(data []byte, section string) (string, error) {
	lines := strings.Split(string(data), "\n")

	startIdx, startLevel := -1, 0
	inFence := false
	for i, line := range lines {
		if isFenceLine(line) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if h, ok := parseOneLine(line); ok && headingMatches(h, section) {
			startIdx = i
			startLevel = h.level
			break
		}
	}
	if startIdx == -1 {
		return "", fmt.Errorf("section %q not found", section)
	}

	out := []string{lines[startIdx]}
	inFence = false
	for i := startIdx + 1; i < len(lines); i++ {
		line := lines[i]
		if isFenceLine(line) {
			inFence = !inFence
		}
		if !inFence {
			if h, ok := parseOneLine(line); ok && h.level <= startLevel {
				break
			}
		}
		out = append(out, line)
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n"), nil
}

// headingMatches checks whether h matches the requested section identifier.
// Numeric/appendix IDs are matched exactly; text IDs case-insensitively.
func headingMatches(h heading, section string) bool {
	return strings.EqualFold(h.id, section)
}
