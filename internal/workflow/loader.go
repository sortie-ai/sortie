// Package workflow loads and parses Sortie workflow files. A workflow
// file combines YAML front matter (structured configuration) with a
// Markdown prompt template in a single document. Start with [Load] to
// obtain a [Workflow] value from a file path, and inspect [WorkflowError]
// for structured failure diagnostics.
package workflow

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// utf8BOM is the byte-order mark that Windows Notepad and some editors
// prepend to UTF-8 files. It must be stripped before delimiter detection
// or the opening "---" will not be recognized.
const utf8BOM = "\xef\xbb\xbf"

// Workflow holds the two payloads extracted from a workflow file: the
// YAML front matter decoded into a generic map, and the raw Markdown
// prompt body for downstream template parsing.
type Workflow struct {
	// Config is the YAML front matter decoded as a map. Empty map
	// (never nil) when front matter is absent.
	Config map[string]any

	// PromptTemplate is the Markdown body after the closing ---
	// delimiter, trimmed of leading and trailing whitespace.
	PromptTemplate string

	// FrontMatterLines is the number of lines from the start of the
	// file through and including the closing --- delimiter. Zero when
	// the file has no front matter block. Used by the prompt renderer
	// to rewrite template error line numbers to file-relative positions.
	FrontMatterLines int
}

// Load reads the workflow file at path and returns the parsed [Workflow].
// It returns a [*WorkflowError] for every expected failure mode: missing
// file, invalid YAML, and non-map front matter.
func Load(path string) (Workflow, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: path is the user-configured workflow file location
	if err != nil {
		return Workflow{}, &WorkflowError{Kind: ErrMissingFile, Path: path, Err: err}
	}

	content := strings.TrimPrefix(string(raw), utf8BOM)
	content = strings.ReplaceAll(content, "\r\n", "\n")

	// Check whether the first line is a front matter opening delimiter.
	// Allow optional trailing whitespace on the delimiter line, consistent
	// with closing delimiter handling.
	firstNL := strings.IndexByte(content, '\n')
	if firstNL == -1 || strings.TrimRight(content[:firstNL], " \t") != "---" {
		return Workflow{
			Config:         make(map[string]any),
			PromptTemplate: strings.TrimSpace(content),
		}, nil
	}

	rest := content[firstNL+1:] // skip opening delimiter line

	fmBytes, promptBody, closingFound := splitAtClosingDelimiter(rest)

	var parsed any
	if err := yaml.Unmarshal([]byte(fmBytes), &parsed); err != nil {
		return Workflow{}, &WorkflowError{Kind: ErrParseError, Path: path, Err: err}
	}

	config, ok := parsed.(map[string]any)
	if !ok {
		if parsed == nil {
			// Empty or comment-only YAML between delimiters. Treat as empty
			// config rather than an error — this matches the behavior of
			// Hugo, Jekyll, and Astro for files like "---\n---\n".
			config = make(map[string]any)
		} else {
			return Workflow{}, &WorkflowError{
				Kind: ErrFrontMatterNotMap,
				Path: path,
				Err:  fmt.Errorf("got %T", parsed),
			}
		}
	}

	// Count lines consumed by front matter: opening delimiter (1) +
	// YAML body lines + closing delimiter (1 if found).
	fmLines := 1 + strings.Count(fmBytes, "\n")
	if closingFound {
		fmLines++
	}

	return Workflow{
		Config:           config,
		PromptTemplate:   strings.TrimSpace(promptBody),
		FrontMatterLines: fmLines,
	}, nil
}

// splitAtClosingDelimiter scans content for a line that is exactly "---"
// (with optional trailing whitespace). It returns the front matter text
// before the delimiter, the prompt body after it, and whether the closing
// delimiter was found. When no closing delimiter is found the entire
// content is treated as front matter and the prompt body is empty.
//
// A manual line scanner is used instead of strings.Cut or strings.SplitN
// on "\n---\n" because the closing delimiter tolerates trailing whitespace
// (e.g. "---   \n"), which a fixed-pattern split cannot express.
func splitAtClosingDelimiter(content string) (frontMatter, promptBody string, found bool) {
	offset := 0
	for offset < len(content) {
		nlIdx := strings.IndexByte(content[offset:], '\n')

		var line string
		if nlIdx == -1 {
			line = content[offset:]
		} else {
			line = content[offset : offset+nlIdx]
		}

		if strings.TrimRight(line, " \t") == "---" {
			frontMatter = content[:offset]
			if nlIdx == -1 {
				promptBody = ""
			} else {
				promptBody = content[offset+nlIdx+1:]
			}
			return frontMatter, promptBody, true
		}

		if nlIdx == -1 {
			break
		}
		offset += nlIdx + 1
	}

	return content, "", false
}
