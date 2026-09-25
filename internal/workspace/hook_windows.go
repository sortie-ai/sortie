//go:build windows

package workspace

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
)

// RunHook executes a hook script via cmd.exe on Windows, enforcing a
// timeout and capturing output. The subprocess is created suspended
// and assigned to a Job Object with KILL_ON_JOB_CLOSE before it is
// resumed, so nothing it spawns can start before the job assignment
// takes effect.
//
// On success (exit code 0), returns a [HookResult] with truncated
// output. On failure, returns a [*HookError] with Op indicating the
// failure mode:
//   - "validate": invalid params
//   - "start": subprocess could not be started, or could not be
//     resumed
//   - "run": subprocess exited with non-zero exit code
//   - "timeout": subprocess exceeded TimeoutMS or parent ctx cancelled
//
// Output is always captured and truncated to [MaxHookOutputBytes],
// even on failure, so callers can log diagnostic output. The hook's
// process tree, and its Job Object, are terminated at its exit,
// whatever the exit status, so a process the script leaves running
// does not outlive it. A hook whose Job Object assignment failed runs
// without one, and the teardown then reaches only the direct process.
func RunHook(ctx context.Context, params HookParams) (HookResult, error) {
	if err := validateParams(params); err != nil {
		return HookResult{}, err
	}

	hookCtx, cancel := context.WithTimeout(ctx, time.Duration(params.TimeoutMS)*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(hookCtx, "cmd.exe", "/C", params.Script) //nolint:gosec // G204: hook scripts are from trusted workflow configuration
	cmd.Dir = params.Dir
	cmd.Env = hookEnv(params.Env)

	// A hook that overran its timeout, or whose caller cancelled ctx,
	// has already had its whole budget, so cancellation force-kills
	// the tree rather than signalling gracefully first.
	procutil.SetGroupKill(cmd)

	buf := procutil.NewTailBuffer(MaxHookOutputBytes)
	capture, startErr := procutil.StartCapture(cmd, procutil.CaptureParams{Stdout: buf, Stderr: buf})

	var (
		waitErr       error
		leftover      bool
		endedOnItsOwn bool
	)
	if startErr == nil {
		result := capture.Wait()
		waitErr = result.WaitErr
		// The capture drains output after the direct child is reaped,
		// and the job drain adds to that wait, so a descendant that
		// outlives the script can carry the context past its deadline
		// once the script itself has already finished. The wait records
		// which of the two happened; the context read below no longer
		// can. Leftovers a script that ended on its own left behind are
		// the ones worth reporting.
		endedOnItsOwn = !procutil.StoppedByCancellation(waitErr)
		leftover = result.TerminatedLeftovers && endedOnItsOwn
	}
	output := formatHookOutput(buf)

	if startErr == nil && waitErr == nil {
		return HookResult{Output: output, TerminatedLeftovers: leftover}, nil
	}

	// Check context error BEFORE *exec.ExitError. A process killed on
	// timeout also produces an ExitError with signal status, and a
	// context already done when StartCapture ran, or done during the
	// resume window, keeps the hook from ever running, reporting the
	// same context error a Wait failure would. Checking context first
	// ensures correct classification in every case. A script that
	// reached its own exit is excluded: its status is the answer,
	// whatever the context says once the drain that followed it has
	// returned.
	if !endedOnItsOwn && hookCtx.Err() == context.DeadlineExceeded {
		return HookResult{}, &HookError{
			Op:       "timeout",
			Script:   truncateScript(params.Script),
			ExitCode: -1,
			Output:   output,
			Err:      fmt.Errorf("hook timed out after %dms: %w", params.TimeoutMS, context.DeadlineExceeded),
		}
	}

	if !endedOnItsOwn && hookCtx.Err() == context.Canceled {
		return HookResult{}, &HookError{
			Op:       "timeout",
			Script:   truncateScript(params.Script),
			ExitCode: -1,
			Output:   output,
			Err:      fmt.Errorf("hook cancelled: %w", context.Canceled),
		}
	}

	if startErr != nil {
		var stageErr *procutil.StartError
		if errors.As(startErr, &stageErr) && stageErr.Stage == procutil.StageProcessResume {
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

	if exitErr, ok := errors.AsType[*exec.ExitError](waitErr); ok {
		return HookResult{}, &HookError{
			Op:                  "run",
			Script:              truncateScript(params.Script),
			ExitCode:            exitErr.ExitCode(),
			Output:              output,
			TerminatedLeftovers: leftover,
			Err:                 waitErr,
		}
	}

	return HookResult{}, &HookError{
		Op:                  "start",
		Script:              truncateScript(params.Script),
		ExitCode:            -1,
		Output:              output,
		TerminatedLeftovers: leftover,
		Err:                 waitErr,
	}
}
