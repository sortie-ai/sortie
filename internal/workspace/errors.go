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

// HookError represents a hook execution failure. Use [errors.As] to
// extract it from errors returned by [RunHook], then inspect Op for
// programmatic handling.
type HookError struct {
	// Op describes the failure mode: "validate", "start", "run", or
	// "timeout".
	Op string

	// Script is the hook script that was executed (or attempted),
	// truncated for safety in log output.
	Script string

	// ExitCode is the process exit code, or -1 if the process did
	// not exit normally (timeout, start failure).
	ExitCode int

	// Output is the combined stdout+stderr captured before failure,
	// truncated to [MaxHookOutputBytes].
	Output string

	// Err is the underlying error.
	Err error
}

// Error returns a human-readable diagnostic including the operation,
// exit code, and truncated script context.
func (e *HookError) Error() string {
	if e.ExitCode >= 0 {
		return fmt.Sprintf("hook %s: exit_code=%d: %v", e.Op, e.ExitCode, e.Err)
	}
	return fmt.Sprintf("hook %s: %v", e.Op, e.Err)
}

// Unwrap returns the underlying error for use with [errors.Is] and
// [errors.As].
func (e *HookError) Unwrap() error {
	return e.Err
}
