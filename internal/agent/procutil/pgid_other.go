//go:build !unix

package procutil

import (
	"os"
	"os/exec"
	"syscall"
)

// SetProcessGroup is a no-op on non-Unix platforms. Process group
// management requires Unix-specific syscalls.
func SetProcessGroup(_ *exec.Cmd) {}

// SignalProcessGroup signals the process directly on non-Unix
// platforms. There are no group semantics; only the single PID
// receives the signal.
//
// On Windows, [os.Process.Signal] supports only [os.Kill] and
// [os.Interrupt]. Sending [syscall.SIGTERM] returns a "not supported
// by windows" error. This fallback is a stub, not a working
// contract.
func SignalProcessGroup(pid int, sig syscall.Signal) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(sig)
}

// KillProcessGroup kills the process directly on non-Unix platforms.
// Unlike [SignalProcessGroup], this works on Windows because it
// delegates to [os.Process.Kill].
func KillProcessGroup(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
