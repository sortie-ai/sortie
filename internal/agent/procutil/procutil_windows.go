//go:build windows

package procutil

import (
	"errors"
	"os/exec"
)

// WasSignaled reports whether the process was terminated by a signal
// equivalent on Windows.
//
// Returns true for STATUS_CONTROL_C_EXIT (0xC000013A), which indicates
// the process was terminated by CTRL_BREAK_EVENT or by
// [KillProcessGroup] via TerminateJobObject. Also returns true for
// exit code -1, which Go's os package returns when the process was
// killed externally and the exit code cannot be determined. Returns
// false for all other exit codes and when err is nil or not an
// [*exec.ExitError].
func WasSignaled(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	code := exitErr.ExitCode()
	// 0xC000013A (STATUS_CONTROL_C_EXIT) is produced by CTRL_BREAK_EVENT
	// and by TerminateJobObject with our sentinel. -1 is returned by Go
	// when the process was killed externally and the exit code cannot be
	// retrieved via GetExitCodeProcess.
	return code == int(jobTerminateExitCode) || code == -1
}
