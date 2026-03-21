package workspace

import (
	"context"
	"log/slog"
	"os"
	"strconv"
)

// HookEnv returns the standard SORTIE_* environment variables for
// hook execution. The attempt value is formatted as a decimal string;
// a zero or negative attempt is rendered as "0".
func HookEnv(issueID, identifier, workspacePath string, attempt int) map[string]string {
	if attempt < 0 {
		attempt = 0
	}
	return map[string]string{
		"SORTIE_ISSUE_ID":         issueID,
		"SORTIE_ISSUE_IDENTIFIER": identifier,
		"SORTIE_WORKSPACE":        workspacePath,
		"SORTIE_ATTEMPT":          strconv.Itoa(attempt),
	}
}

// PrepareParams holds the inputs for workspace preparation before an
// agent run. Construct from orchestrator state at dispatch time.
type PrepareParams struct {
	// Root is the workspace root directory (from config).
	Root string

	// Identifier is the issue identifier used to derive the workspace key.
	Identifier string

	// IssueID is the tracker-assigned issue ID (for hook env vars).
	IssueID string

	// Attempt is the retry attempt number (0 or positive).
	Attempt int

	// AfterCreate is the after_create hook script. Empty means no hook.
	AfterCreate string

	// BeforeRun is the before_run hook script. Empty means no hook.
	BeforeRun string

	// HookTimeoutMS is the timeout for each hook invocation.
	HookTimeoutMS int

	// Logger is the structured logger for hook lifecycle events.
	// If nil, [slog.Default] is used.
	Logger *slog.Logger
}

// PrepareResult holds the outcome of successful workspace preparation.
type PrepareResult struct {
	// Key is the sanitized workspace directory name.
	Key string

	// Path is the absolute workspace path.
	Path string

	// CreatedNow is true when the workspace directory was newly created.
	CreatedNow bool
}

// Prepare ensures a workspace directory exists for the given issue
// and runs the applicable lifecycle hooks (after_create if newly
// created, then before_run). Returns a [PrepareResult] on success.
//
// Failure semantics:
//   - after_create failure is fatal: the newly created workspace
//     directory is removed (transactional rollback) so the next
//     retry starts with a clean slate.
//   - before_run failure is fatal: returns error. The workspace
//     directory is preserved (it may contain prior agent work).
//
// The context controls cancellation for hook execution. If the context
// is already cancelled when Prepare is called, it returns immediately
// without touching the filesystem.
func Prepare(ctx context.Context, params PrepareParams) (PrepareResult, error) {
	if err := ctx.Err(); err != nil {
		return PrepareResult{}, err
	}

	logger := params.Logger
	if logger == nil {
		logger = slog.Default()
	}

	result, err := Ensure(params.Root, params.Identifier)
	if err != nil {
		return PrepareResult{}, err
	}

	env := HookEnv(params.IssueID, params.Identifier, result.Path, params.Attempt)

	if result.CreatedNow && params.AfterCreate != "" {
		logger.InfoContext(ctx, "running hook", "hook", "after_create", "workspace", result.Path)
		_, hookErr := RunHook(ctx, HookParams{
			Script:    params.AfterCreate,
			Dir:       result.Path,
			Env:       env,
			TimeoutMS: params.HookTimeoutMS,
		})
		if hookErr != nil {
			logger.WarnContext(ctx, "after_create hook failed, rolling back workspace",
				"workspace", result.Path, "error", hookErr)
			if rmErr := os.RemoveAll(result.Path); rmErr != nil {
				logger.ErrorContext(ctx, "workspace rollback failed after after_create hook error",
					"workspace", result.Path, "rollback_error", rmErr)
			}
			return PrepareResult{}, hookErr
		}
	}

	if params.BeforeRun != "" {
		logger.InfoContext(ctx, "running hook", "hook", "before_run", "workspace", result.Path)
		_, hookErr := RunHook(ctx, HookParams{
			Script:    params.BeforeRun,
			Dir:       result.Path,
			Env:       env,
			TimeoutMS: params.HookTimeoutMS,
		})
		if hookErr != nil {
			logger.WarnContext(ctx, "before_run hook failed", "workspace", result.Path, "error", hookErr)
			return PrepareResult{}, hookErr
		}
	}

	return PrepareResult(result), nil
}

// FinishParams holds the inputs for post-agent-run hook execution.
type FinishParams struct {
	// Path is the absolute workspace directory path.
	Path string

	// Identifier is the issue identifier (for hook env vars).
	Identifier string

	// IssueID is the tracker-assigned issue ID (for hook env vars).
	IssueID string

	// Attempt is the retry attempt number.
	Attempt int

	// AfterRun is the after_run hook script. Empty means no hook.
	AfterRun string

	// HookTimeoutMS is the timeout for the hook invocation.
	HookTimeoutMS int

	// Logger is the structured logger. If nil, [slog.Default] is used.
	Logger *slog.Logger
}

// Finish runs the after_run hook if configured. Failure is logged and
// ignored; this function never returns an error.
//
// The parent context is detached via [context.WithoutCancel] so that
// teardown hooks run even when the worker context has been cancelled
// (stall timeout, SIGTERM, reconciliation). Hook execution time is
// still bounded by HookTimeoutMS.
func Finish(ctx context.Context, params FinishParams) {
	if params.AfterRun == "" {
		return
	}

	logger := params.Logger
	if logger == nil {
		logger = slog.Default()
	}

	detachedCtx := context.WithoutCancel(ctx)
	env := HookEnv(params.IssueID, params.Identifier, params.Path, params.Attempt)

	logger.InfoContext(ctx, "running hook", "hook", "after_run", "workspace", params.Path)
	_, hookErr := RunHook(detachedCtx, HookParams{
		Script:    params.AfterRun,
		Dir:       params.Path,
		Env:       env,
		TimeoutMS: params.HookTimeoutMS,
	})
	if hookErr != nil {
		logger.WarnContext(ctx, "after_run hook failed", "workspace", params.Path, "error", hookErr)
	}
}

// CleanupParams holds the inputs for workspace directory removal.
type CleanupParams struct {
	// Root is the workspace root directory.
	Root string

	// Identifier is the issue identifier.
	Identifier string

	// IssueID is the tracker-assigned issue ID (for hook env vars).
	IssueID string

	// Attempt is the retry attempt number (for hook env vars).
	Attempt int

	// BeforeRemove is the before_remove hook script. Empty means no hook.
	BeforeRemove string

	// HookTimeoutMS is the timeout for the hook invocation.
	HookTimeoutMS int

	// Logger is the structured logger. If nil, [slog.Default] is used.
	Logger *slog.Logger
}

// Cleanup removes a workspace directory for the given issue, running
// the before_remove hook first if configured. The before_remove hook
// failure is logged and ignored; removal still proceeds.
//
// The parent context is detached via [context.WithoutCancel] so that
// the before_remove hook runs even when the caller's context has been
// cancelled. Hook execution time is still bounded by HookTimeoutMS.
//
// Returns nil if the workspace directory does not exist (idempotent).
// Returns an error if path computation fails or directory removal fails.
func Cleanup(ctx context.Context, params CleanupParams) error {
	pathResult, err := ComputePath(params.Root, params.Identifier)
	if err != nil {
		return err
	}

	_, statErr := os.Stat(pathResult.Path)
	if os.IsNotExist(statErr) {
		return nil
	}

	logger := params.Logger
	if logger == nil {
		logger = slog.Default()
	}

	detachedCtx := context.WithoutCancel(ctx)
	env := HookEnv(params.IssueID, params.Identifier, pathResult.Path, params.Attempt)

	if params.BeforeRemove != "" {
		logger.InfoContext(ctx, "running hook", "hook", "before_remove", "workspace", pathResult.Path)
		_, hookErr := RunHook(detachedCtx, HookParams{
			Script:    params.BeforeRemove,
			Dir:       pathResult.Path,
			Env:       env,
			TimeoutMS: params.HookTimeoutMS,
		})
		if hookErr != nil {
			logger.WarnContext(ctx, "before_remove hook failed", "workspace", pathResult.Path, "error", hookErr)
		}
	}

	return os.RemoveAll(pathResult.Path)
}

// CleanupTerminalParams holds the inputs for batch workspace removal
// of terminal-state issues. Construct from orchestrator state during
// startup cleanup or reconciliation.
type CleanupTerminalParams struct {
	// Root is the workspace root directory (from config).
	Root string

	// Identifiers is the list of issue identifiers whose workspaces
	// should be removed. Each identifier is sanitized to a workspace
	// key before lookup.
	Identifiers []string

	// IssueIDsByIdentifier maps issue identifiers to their
	// tracker-assigned IDs. Used for hook environment variables.
	// Identifiers missing from this map use the identifier as the
	// issue ID fallback.
	IssueIDsByIdentifier map[string]string

	// BeforeRemove is the before_remove hook script. Empty means no hook.
	BeforeRemove string

	// HookTimeoutMS is the timeout for each before_remove hook invocation.
	HookTimeoutMS int

	// Logger is the structured logger for cleanup lifecycle events.
	// If nil, [slog.Default] is used.
	Logger *slog.Logger
}

// CleanupTerminalResult holds the outcome of a batch workspace
// cleanup. Inspect Removed for successful removals and Errors for
// per-identifier failures.
type CleanupTerminalResult struct {
	// Removed lists the identifiers whose workspaces were
	// successfully removed or did not exist on disk.
	Removed []string

	// Errors maps identifiers to the error encountered during their
	// cleanup. Identifiers that succeeded or had no workspace on disk
	// are not present in this map.
	Errors map[string]error
}

// CleanupTerminal removes workspace directories for terminal-state
// issues. For each identifier in params.Identifiers, it delegates to
// [Cleanup] which sanitizes the identifier, checks existence, runs the
// before_remove hook (best-effort), and removes the directory.
//
// Cleanup is best-effort per identifier: a failure removing one
// workspace does not prevent cleanup of others. Individual errors are
// collected in [CleanupTerminalResult.Errors].
func CleanupTerminal(ctx context.Context, params CleanupTerminalParams) CleanupTerminalResult {
	result := CleanupTerminalResult{
		Removed: make([]string, 0, len(params.Identifiers)),
		Errors:  make(map[string]error),
	}

	logger := params.Logger
	if logger == nil {
		logger = slog.Default()
	}

	for _, identifier := range params.Identifiers {
		issueID := identifier
		if params.IssueIDsByIdentifier != nil {
			if mapped, ok := params.IssueIDsByIdentifier[identifier]; ok {
				issueID = mapped
			}
		}

		err := Cleanup(ctx, CleanupParams{
			Root:          params.Root,
			Identifier:    identifier,
			IssueID:       issueID,
			Attempt:       0,
			BeforeRemove:  params.BeforeRemove,
			HookTimeoutMS: params.HookTimeoutMS,
			Logger:        logger,
		})
		if err != nil {
			logger.WarnContext(ctx, "workspace cleanup failed",
				"identifier", identifier, "error", err)
			result.Errors[identifier] = err
		} else {
			logger.InfoContext(ctx, "workspace cleaned",
				"identifier", identifier)
			result.Removed = append(result.Removed, identifier)
		}
	}

	return result
}
