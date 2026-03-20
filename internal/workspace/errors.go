package workspace

import "fmt"

// PathError represents a workspace operation failure. Use [errors.As]
// to extract it from errors returned by [SanitizeKey], [ComputePath],
// or [Ensure], then inspect Op for programmatic handling.
type PathError struct {
	// Op describes the failed operation: "sanitize", "resolve",
	// "containment", "create", "stat", or "conflict".
	Op string

	// Root is the workspace root path involved, if applicable.
	Root string

	// Identifier is the issue identifier involved, if applicable.
	Identifier string

	// Err is the underlying error.
	Err error
}

// Error returns a human-readable diagnostic including the operation
// and relevant context.
func (e *PathError) Error() string {
	switch {
	case e.Root != "" && e.Identifier != "":
		return fmt.Sprintf("workspace %s: root=%q identifier=%q: %v", e.Op, e.Root, e.Identifier, e.Err)
	case e.Identifier != "":
		return fmt.Sprintf("workspace %s: identifier=%q: %v", e.Op, e.Identifier, e.Err)
	case e.Root != "":
		return fmt.Sprintf("workspace %s: root=%q: %v", e.Op, e.Root, e.Err)
	default:
		return fmt.Sprintf("workspace %s: %v", e.Op, e.Err)
	}
}

// Unwrap returns the underlying error for use with [errors.Is] and
// [errors.As].
func (e *PathError) Unwrap() error {
	return e.Err
}
