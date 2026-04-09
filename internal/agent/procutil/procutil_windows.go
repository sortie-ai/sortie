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
// [KillProcessGroup] via TerminateJobObject. Returns false for all
// other exit codes and when err is nil or not an [*exec.ExitError].
func WasSignaled(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	code := uint32(exitErr.ExitCode())
	return code == 0xC000013A
}
