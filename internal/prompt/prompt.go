// Package prompt renders per-issue prompt templates using Go
// [text/template] in strict mode. Start with [Parse] to compile a
// template body, then call [Template.Render] for each issue. Inspect
// [TemplateError] for structured failure diagnostics with
// WORKFLOW.md-relative line numbers.
package prompt

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"text/template"
)

// RunContext carries per-turn metadata passed to the prompt template as
// the "run" variable. Converted to a map with snake_case keys before
// template execution so workflow authors write {{ .run.turn_number }}
// matching the architecture doc verbatim.
type RunContext struct {
	TurnNumber     int
	MaxTurns       int
	IsContinuation bool
}

// runContextToMap converts a [RunContext] to the map representation used
// as the "run" template variable. Keys use snake_case to match the
// architecture doc naming convention.
func runContextToMap(rc RunContext) map[string]any {
	return map[string]any{
		"turn_number":     rc.TurnNumber,
		"max_turns":       rc.MaxTurns,
		"is_continuation": rc.IsContinuation,
	}
}

// promptFuncMap is the minimal, prompt-essential FuncMap shipped with
// every template. Each entry is permanent API surface.
var promptFuncMap = template.FuncMap{
	"toJSON": func(v any) (string, error) {
		b, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(b), nil
	},
	"join": func(sep string, v any) (string, error) {
		switch list := v.(type) {
		case []string:
			return strings.Join(list, sep), nil
		case []any:
			s := make([]string, len(list))
			for i, elem := range list {
				s[i] = fmt.Sprint(elem)
			}
			return strings.Join(s, sep), nil
		default:
			return "", fmt.Errorf("join: unsupported type %T, want []string or []any", v)
		}
	},
	"lower": strings.ToLower,
}

// linePattern matches text/template error messages and captures the
// template-relative line number. Handles both "name:line:col:" and
// "name:line:" formats. Compiled once at package init.
var linePattern = regexp.MustCompile(`template: [^:]+:(\d+)`)

// Template is a parsed prompt template ready for per-issue execution.
// Obtain via [Parse]. Safe for concurrent [Template.Render] calls.
type Template struct {
	tmpl             *template.Template
	frontMatterLines int
	source           string
}

// Parse compiles a prompt template body with strict mode
// (missingkey=error) and the standard [FuncMap]. frontMatterLines is the
// number of lines consumed by front matter in the source file (used to
// rewrite error positions to WORKFLOW.md-relative line numbers). Returns
// a [*TemplateError] with Kind [ErrTemplateParse] on failure.
func Parse(body, source string, frontMatterLines int) (*Template, error) {
	tmpl, err := template.New("prompt").
		Option("missingkey=error").
		Funcs(promptFuncMap).
		Parse(body)
	if err != nil {
		line := extractTemplateLine(err)
		if line > 0 {
			line += frontMatterLines
		}
		return nil, &TemplateError{
			Kind:   ErrTemplateParse,
			Source: source,
			Line:   line,
			Err:    err,
		}
	}
	return &Template{
		tmpl:             tmpl,
		frontMatterLines: frontMatterLines,
		source:           source,
	}, nil
}

// Render executes the template with the given inputs and returns the
// rendered prompt string. The data map contains exactly three top-level
// keys: "issue", "attempt", and "run". Returns a [*TemplateError] with
// Kind [ErrTemplateRender] on failure, with line numbers adjusted to
// WORKFLOW.md-relative positions.
func (t *Template) Render(issue map[string]any, attempt any, run RunContext) (string, error) {
	data := map[string]any{
		"issue":   issue,
		"attempt": attempt,
		"run":     runContextToMap(run),
	}

	var buf bytes.Buffer
	if err := t.tmpl.Execute(&buf, data); err != nil {
		line := extractTemplateLine(err)
		if line > 0 {
			line += t.frontMatterLines
		}
		return "", &TemplateError{
			Kind:   ErrTemplateRender,
			Source: t.source,
			Line:   line,
			Err:    err,
		}
	}
	return buf.String(), nil
}

// extractTemplateLine parses the template-relative line number from a
// text/template error message. Returns 0 when the pattern does not
// match.
func extractTemplateLine(err error) int {
	matches := linePattern.FindStringSubmatch(err.Error())
	if len(matches) < 2 {
		return 0
	}
	n, parseErr := strconv.Atoi(matches[1])
	if parseErr != nil {
		return 0
	}
	return n
}
