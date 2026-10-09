//go:build windows

package workspace

import (
	"context"
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
	capture, startErr := startHookCapture(hookCtx, cmd, procutil.CaptureParams{Stdout: buf, Stderr: buf})

	var result procutil.CaptureResult
	if startErr == nil {
		result = capture.Wait()
	}
	return classifyHook(hookCtx, params, formatHookOutput(buf), result, startErr)
}
