//go:build unix

package procutil

import (
	"errors"
	"os/exec"
	"syscall"
)

// WasSignaled reports whether the process was terminated by a signal.
//
// Returns false when err is nil or is not an [*exec.ExitError].
func WasSignaled(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok {
		return false
	}
	return status.Signaled()
}
