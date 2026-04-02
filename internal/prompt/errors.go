package prompt

import "fmt"

// ErrorKind classifies prompt template failures into parse-time and
// render-time categories. Callers can switch on Kind to determine
// whether the failure blocks all dispatch or only the current run
// attempt.
type ErrorKind int

const (
	// ErrTemplateParse indicates the template body contains invalid
	// syntax and could not be compiled. This blocks dispatch until the
	// workflow file is corrected.
	ErrTemplateParse ErrorKind = iota + 1

	// ErrTemplateRender indicates the template executed but failed on a
	// missing variable, broken pipeline, or FuncMap error. This fails
	// only the current run attempt.
	ErrTemplateRender
)

// TemplateError represents a structured prompt template failure.
//
// It wraps the underlying cause so [errors.As] can extract it from errors
// returned by [Parse] or [Template.Render]. Kind distinguishes
// dispatch-blocking parse failures from per-attempt render failures, and
// Line provides the operator-facing source location.
type TemplateError struct {
	// Kind distinguishes parse errors (dispatch-blocking) from render
	// errors (per-attempt).
	Kind ErrorKind

	// Source is the workflow file path for operator-facing messages.
	Source string

	// Line is the 1-based line number in the original WORKFLOW.md file
	// (front matter offset applied). Zero when the line cannot be
	// determined from the underlying error.
	Line int

	// Err is the underlying cause from text/template.
	Err error
}

// Error returns a human-readable diagnostic including the source path
// and, when available, the adjusted line number.
func (e *TemplateError) Error() string {
	kind := "render"
	if e.Kind == ErrTemplateParse {
		kind = "parse"
	}
	if e.Line > 0 {
		return fmt.Sprintf("template %s error in %s (line %d): %v", kind, e.Source, e.Line, e.Err)
	}
	return fmt.Sprintf("template %s error in %s: %v", kind, e.Source, e.Err)
}

// Unwrap returns the underlying error, enabling [errors.Is] and
// [errors.As] chains through the wrapped cause.
func (e *TemplateError) Unwrap() error {
	return e.Err
}
