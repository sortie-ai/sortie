package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/workspacekit"
)

// MaxHookOutputBytes is the maximum number of bytes retained from a
// hook's combined stdout and stderr. When output exceeds this cap the
// earliest bytes are dropped so the retained value holds the tail,
// where a failing hook's error message almost always is. The cap is
// deliberately small so the captured output, emitted verbatim as one
// structured log field, stays under the single-line size limit that
// common log collectors impose before they split a record.
const MaxHookOutputBytes = 8 * 1024

// maxScriptDisplayLen is the maximum number of bytes of a hook script
// included in error messages.
const maxScriptDisplayLen = 200

// HookParams holds the inputs for a single hook invocation.
type HookParams struct {
	// Script is the shell script body to execute via "sh -c".
	// Must be non-empty.
	Script string

	// Dir is the absolute workspace directory path used as the
	// subprocess cwd. Must exist and be a directory.
	Dir string

	// Env holds the SORTIE_* environment variables injected into the
	// hook subprocess. The map is populated by the caller; [RunHook]
	// does not modify or extend it.
	Env map[string]string

	// TimeoutMS is the maximum execution time in milliseconds.
	// Sourced from [config.HooksConfig] TimeoutMS (default 60000).
	// Must be positive.
	TimeoutMS int
}

// HookResult holds the outcome of a successful hook execution (exit
// code 0). Output contains the combined stdout and stderr, retained as
// the last [MaxHookOutputBytes] bytes.
type HookResult struct {
	// Output is the combined stdout+stderr of the hook: the last
	// [MaxHookOutputBytes] bytes, prefixed with a truncation marker
	// when earlier output was dropped.
	Output string

	// TerminatedLeftovers is true when the hook exited on its own and
	// its termination reached a process of its tree other than the
	// hook script itself.
	TerminatedLeftovers bool
}

// startHookCapture is the hook's process start. Only a test replaces it,
// to return a start failure at a chosen moment.
var startHookCapture = procutil.StartCapture

// classifyHook turns the outcome of one hook launch into RunHook's
// return. Whether the hook was cancelled is the launch record's verdict,
// a cancelled start or a stop that began while the script ran; hookCtx is
// read only to choose between the timeout and the cancellation message,
// because only hookCtx stops a hook. A script that reached its own exit
// is reported by its status however long the drain after it ran, and
// only such a script's leftovers are reported.
func classifyHook(hookCtx context.Context, params HookParams, output string, result procutil.CaptureResult, startErr error) (HookResult, error) {
	if startErr != nil {
		stageErr, isStageErr := errors.AsType[*procutil.StartError](startErr)
		switch {
		case isStageErr && stageErr.Cancelled:
			return HookResult{}, hookTimeoutError(hookCtx, params, output)
		case isStageErr && stageErr.Stage == procutil.StageProcessResume:
			return HookResult{}, &HookError{
				Op:       "start",
				Script:   truncateScript(params.Script),
				ExitCode: -1,
				Output:   output,
				Err:      fmt.Errorf("resume hook process: %w", stageErr.Unwrap()),
			}
		}
		return HookResult{}, &HookError{
			Op:       "start",
			Script:   truncateScript(params.Script),
			ExitCode: -1,
			Output:   output,
			Err:      startErr,
		}
	}

	if result.Stopped {
		return HookResult{}, hookTimeoutError(hookCtx, params, output)
	}

	if result.WaitErr == nil {
		return HookResult{Output: output, TerminatedLeftovers: result.TerminatedLeftovers}, nil
	}

	if exitErr, ok := errors.AsType[*exec.ExitError](result.WaitErr); ok {
		return HookResult{}, &HookError{
			Op:                  "run",
			Script:              truncateScript(params.Script),
			ExitCode:            exitErr.ExitCode(),
			Output:              output,
			TerminatedLeftovers: result.TerminatedLeftovers,
			Err:                 result.WaitErr,
		}
	}

	return HookResult{}, &HookError{
		Op:                  "start",
		Script:              truncateScript(params.Script),
		ExitCode:            -1,
		Output:              output,
		TerminatedLeftovers: result.TerminatedLeftovers,
		Err:                 result.WaitErr,
	}
}

func hookTimeoutError(hookCtx context.Context, params HookParams, output string) *HookError {
	cause := fmt.Errorf("hook cancelled: %w", context.Canceled)
	if errors.Is(hookCtx.Err(), context.DeadlineExceeded) {
		cause = fmt.Errorf("hook timed out after %dms: %w", params.TimeoutMS, context.DeadlineExceeded)
	}
	return &HookError{
		Op:       "timeout",
		Script:   truncateScript(params.Script),
		ExitCode: -1,
		Output:   output,
		Err:      cause,
	}
}

// truncateScript returns s unchanged if it fits within
// maxScriptDisplayLen bytes, otherwise returns the first
// maxScriptDisplayLen bytes followed by "...".
func truncateScript(s string) string {
	if len(s) <= maxScriptDisplayLen {
		return s
	}
	return s[:maxScriptDisplayLen] + "..."
}

// formatHookOutput returns buf's retained bytes, prefixed with a
// one-line truncation marker naming [MaxHookOutputBytes] when buf
// discarded earlier output.
func formatHookOutput(buf *procutil.TailBuffer) string {
	retained := string(buf.Bytes())
	if !buf.Truncated() {
		return retained
	}
	return fmt.Sprintf("[truncated: showing last %d bytes of hook output]\n%s", MaxHookOutputBytes, retained)
}

// hookEnv builds a restricted environment for the hook subprocess.
// Only variables in allowedEnvKeys and variables whose name starts
// with "SORTIE_" are inherited from the parent process. Variables in
// override take precedence over same-named parent variables.
func hookEnv(override map[string]string) []string {
	parent := os.Environ()
	env := make([]string, 0, len(allowedEnvKeys)+len(override))
	overrideNorm := make(map[string]bool, len(override))
	for k := range override {
		overrideNorm[normalizeEnvKey(k)] = true
	}
	for _, entry := range parent {
		k, _, _ := strings.Cut(entry, "=")
		norm := normalizeEnvKey(k)
		if !allowedEnvKeys[norm] && !strings.HasPrefix(norm, "SORTIE_") {
			continue
		}
		if overrideNorm[norm] {
			continue
		}
		env = append(env, entry)
	}
	for k, v := range override {
		env = append(env, k+"="+v)
	}
	return env
}

// validateParams checks HookParams preconditions and returns a
// *HookError with Op "validate" on any violation.
func validateParams(params HookParams) error {
	if params.Script == "" {
		return &HookError{
			Op:       "validate",
			Script:   "",
			ExitCode: -1,
			Err:      errors.New("script must not be empty"),
		}
	}

	if params.Dir == "" {
		return &HookError{
			Op:       "validate",
			Script:   truncateScript(params.Script),
			ExitCode: -1,
			Err:      errors.New("dir must not be empty"),
		}
	}

	if err := workspacekit.VerifyDir(params.Dir); err != nil {
		return &HookError{
			Op:       "validate",
			Script:   truncateScript(params.Script),
			ExitCode: -1,
			Err:      fmt.Errorf("dir %q: %w", params.Dir, err),
		}
	}

	if params.TimeoutMS <= 0 {
		return &HookError{
			Op:       "validate",
			Script:   truncateScript(params.Script),
			ExitCode: -1,
			Err:      errors.New("timeout_ms must be positive"),
		}
	}

	return nil
}
