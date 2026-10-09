// Package prompt renders per-issue prompt templates using Go
// [text/template] in strict mode. Start with [Parse] to compile a
// template body, then call [Template.Render] for each issue. Templates
// that share define blocks compile through [ParsePartials] and
// [ParseWithPartials]. Inspect [TemplateError] for structured failure
// diagnostics that name the file and line holding the fault.
package prompt

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"text/template"
	"text/template/parse"
)

// RunContext carries per-turn metadata passed to the prompt template as
// the "run" variable. Converted to a map with snake_case keys before
// template execution so workflow authors write {{ .run.turn_number }}.
type RunContext struct {
	TurnNumber     int
	MaxTurns       int
	IsContinuation bool
}

// runContextToMap converts a [RunContext] to the map representation used
// as the "run" template variable. Keys use snake_case to match the
// prompt contract exposed to workflow authors.
func runContextToMap(rc RunContext) map[string]any {
	return map[string]any{
		"turn_number":     rc.TurnNumber,
		"max_turns":       rc.MaxTurns,
		"is_continuation": rc.IsContinuation,
	}
}

// StageContext carries the stage chain position passed to the prompt
// template as the "stage" variable. Empty fields render as empty strings,
// so templates may reference them on every dispatch.
type StageContext struct {
	Current         string
	Previous        string
	PreviousOutcome string
}

// stageContextToMap converts a [StageContext] to the map representation
// used as the "stage" template variable, with snake_case keys.
func stageContextToMap(sc StageContext) map[string]any {
	return map[string]any{
		"current":          sc.Current,
		"previous":         sc.Previous,
		"previous_outcome": sc.PreviousOutcome,
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

// mainTemplateName is the parse name of every prompt template. A partial
// cannot define it, because a call to it would re-enter the prompt itself.
const mainTemplateName = "prompt"

// Template is a parsed prompt template ready for per-issue execution.
// Obtain via [Parse] or [ParseWithPartials]. Safe for concurrent
// [Template.Render] calls.
type Template struct {
	tmpl             *template.Template
	frontMatterLines int
	source           string
	body             string
	partials         *Partials
	reached          map[string]struct{}
}

// Tree returns the parsed template tree for static analysis. The tree
// is safe for read-only traversal by concurrent callers. Returns nil
// when the receiver or its underlying template is nil.
func (t *Template) Tree() *parse.Tree {
	if t == nil || t.tmpl == nil {
		return nil
	}
	return t.tmpl.Tree
}

// Mentions reports whether s occurs in the body text the template was
// parsed from, or in the Body of a partial that defines a block the
// template reaches. A nil receiver mentions nothing.
func (t *Template) Mentions(s string) bool {
	if t == nil {
		return false
	}
	if strings.Contains(t.body, s) {
		return true
	}
	for name := range t.reached {
		if owner, ok := t.partials.owner(name); ok && strings.Contains(owner.body, s) {
			return true
		}
	}
	return false
}

// Parse compiles a prompt template body with strict mode
// (missingkey=error) and the standard [FuncMap]. frontMatterLines is the
// number of lines consumed by front matter in the source file (used to
// rewrite error positions to WORKFLOW.md-relative line numbers). It
// behaves as [ParseWithPartials] with no partials, so a call to an
// undefined template fails here rather than at render time. Returns a
// [*TemplateError] with Kind [ErrTemplateParse] on failure.
func Parse(body, source string, frontMatterLines int) (*Template, error) {
	return ParseWithPartials(body, source, frontMatterLines, nil)
}

// ParseWithPartials compiles a prompt template body into its own template
// set, which holds the define blocks of partials besides the body's own.
// partials may be nil and is never modified. A syntax error, a define name
// that partials also define, and a call to an undefined template return a
// [*TemplateError] with Kind [ErrTemplateParse], located in the file that
// holds the fault.
func ParseWithPartials(body, source string, frontMatterLines int, partials *Partials) (*Template, error) {
	set, err := newSet(mainTemplateName).Parse(body)
	if err != nil {
		line := leadingLine(err, mainTemplateName)
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
	for _, b := range defineBlocks(set, mainTemplateName, body) {
		if owner, dup := partials.owner(b.name); dup {
			return nil, parseError(source, b.line+frontMatterLines, "template %q is already defined in %s", b.name, owner.path)
		}
	}
	for _, b := range partials.blockList() {
		if _, err := set.AddParseTree(b.name, b.tree); err != nil {
			return nil, fmt.Errorf("add partial template %q: %w", b.name, err)
		}
	}
	t := &Template{
		tmpl:             set,
		frontMatterLines: frontMatterLines,
		source:           source,
		body:             body,
		partials:         partials,
	}
	if err := t.checkCalls(); err != nil {
		return nil, err
	}
	return t, nil
}

func newSet(name string) *template.Template {
	return template.New(name).
		Option("missingkey=error").
		Funcs(promptFuncMap)
}

// RenderOption applies optional overrides to the template data map
// before execution. Use [WithContinuationContext] to inject reaction
// continuation data and [WithStage] to supply the stage chain position.
type RenderOption func(m map[string]any)

// continuationKeys lists all template variable names reserved for
// reaction continuation context. Each key receives a nil default in
// [Template.Render] so templates using missingkey=error do not reject
// references to absent reaction types.
var continuationKeys = []string{"ci_failure", "review_comments", "bot_review_comments", "merge_conflict", "label_review", "label_fix"}

// isContinuationKey reports whether key is a registered continuation
// template variable name.
func isContinuationKey(key string) bool {
	return slices.Contains(continuationKeys, key)
}

// WithContinuationContext returns a [RenderOption] that merges each
// key from data into the template data map. Keys must be registered
// in [continuationKeys]; passing an unregistered key panics
// (programmer error: the reaction kind must register its template key
// before use).
func WithContinuationContext(data map[string]any) RenderOption {
	for k := range data {
		if !isContinuationKey(k) {
			panic(fmt.Sprintf("prompt: continuation key %q is not in continuationKeys whitelist", k))
		}
	}
	return func(m map[string]any) {
		maps.Copy(m, data)
	}
}

// WithStage returns a [RenderOption] that replaces the "stage" template
// variable with the values in sc.
func WithStage(sc StageContext) RenderOption {
	return func(m map[string]any) {
		m["stage"] = stageContextToMap(sc)
	}
}

// Render executes the template with the given inputs and returns the
// rendered prompt string. The data map contains the top-level keys
// "issue", "attempt", "run", and "stage", plus every key in [continuationKeys]
// (currently "ci_failure", "review_comments", "bot_review_comments",
// "merge_conflict", "label_review", "label_fix").
// Continuation keys default to nil when no [RenderOption] overrides them,
// ensuring missingkey=error does not reject templates that reference these
// fields.
// Returns a [*TemplateError] with Kind [ErrTemplateRender] on failure,
// located in the file that holds the fault: a partial's path with its own
// line numbers, or the prompt template's source with line numbers adjusted
// to WORKFLOW.md-relative positions.
func (t *Template) Render(issue map[string]any, attempt any, run RunContext, opts ...RenderOption) (string, error) {
	templateVars := map[string]any{
		"issue":   issue,
		"attempt": attempt,
		"run":     runContextToMap(run),
		"stage":   stageContextToMap(StageContext{}),
	}
	for _, k := range continuationKeys {
		templateVars[k] = nil
	}

	for _, opt := range opts {
		opt(templateVars)
	}

	var buf bytes.Buffer
	if err := t.tmpl.Execute(&buf, templateVars); err != nil {
		source, line := t.renderErrorOrigin(err)
		return "", &TemplateError{
			Kind:   ErrTemplateRender,
			Source: source,
			Line:   line,
			Err:    err,
		}
	}
	return buf.String(), nil
}

// renderErrorOrigin locates an execution error from the parse name and
// line its message leads with. The parse name is matched against the names
// this template set holds because a file name may itself contain colons.
func (t *Template) renderErrorOrigin(err error) (string, int) {
	var parseName string
	var line int
	for _, name := range append(t.partials.parseNames(), mainTemplateName) {
		if n := leadingLine(err, name); n > 0 && len(name) > len(parseName) {
			parseName, line = name, n
		}
	}
	if parseName == "" {
		return t.source, 0
	}
	return t.origin(parseName, line)
}

// origin maps a line within the parse named parseName to the file that
// holds it. Only the prompt template's own text sits behind front matter.
func (t *Template) origin(parseName string, line int) (string, int) {
	if path, ok := t.partials.pathOf(parseName); ok {
		return path, line
	}
	if line > 0 {
		line += t.frontMatterLines
	}
	return t.source, line
}

// leadingLine returns the line number of a text/template error message
// that opens with "template: <parseName>:<line>", or 0 when it does not.
func leadingLine(err error, parseName string) int {
	rest, ok := strings.CutPrefix(err.Error(), "template: "+parseName+":")
	if !ok {
		return 0
	}
	digits := rest
	if end := strings.IndexFunc(rest, func(r rune) bool { return r < '0' || r > '9' }); end >= 0 {
		digits = rest[:end]
	}
	n, convErr := strconv.Atoi(digits)
	if convErr != nil {
		return 0
	}
	return n
}
