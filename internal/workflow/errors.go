package workflow

import "fmt"

// ErrorKind classifies workflow loading failures into one of the
// recognized error categories. Callers can switch on Kind to determine
// recovery strategy without string-matching error messages.
type ErrorKind int

const (
	// ErrMissingFile indicates the workflow file could not be read.
	ErrMissingFile ErrorKind = iota + 1

	// ErrParseError indicates the YAML front matter is syntactically invalid.
	ErrParseError

	// ErrFrontMatterNotMap indicates the YAML front matter decoded to a
	// non-map type (scalar, list, or null).
	ErrFrontMatterNotMap
)

// WorkflowError represents a structured workflow loading failure. Use
// [errors.As] to extract it from the error returned by [Load], then
// inspect Kind and Path for programmatic handling.
type WorkflowError struct {
	Kind ErrorKind
	Path string
	Err  error
}

// Error returns a human-readable description of the failure. For parse
// errors the message includes a hint about the most common cause — a
// missing closing delimiter — to aid operator debugging.
func (e *WorkflowError) Error() string {
	switch e.Kind {
	case ErrMissingFile:
		if e.Err != nil {
			return fmt.Sprintf("workflow file not found: %s: %v", e.Path, e.Err)
		}
		return fmt.Sprintf("workflow file not found: %s", e.Path)
	case ErrParseError:
		if e.Err != nil {
			return fmt.Sprintf("failed to parse YAML front matter in %s (did you forget the closing '---' delimiter?): %v", e.Path, e.Err)
		}
		return fmt.Sprintf("failed to parse YAML front matter in %s (did you forget the closing '---' delimiter?)", e.Path)
	case ErrFrontMatterNotMap:
		if e.Err != nil {
			return fmt.Sprintf("workflow front matter in %s must be a YAML map: %v", e.Path, e.Err)
		}
		return fmt.Sprintf("workflow front matter in %s must be a YAML map", e.Path)
	default:
		if e.Err != nil {
			return fmt.Sprintf("workflow error in %s: %v", e.Path, e.Err)
		}
		return fmt.Sprintf("workflow error in %s", e.Path)
	}
}

// Unwrap returns the underlying error, enabling [errors.Is] and
// [errors.As] chains through the wrapped cause.
func (e *WorkflowError) Unwrap() error {
	return e.Err
}
