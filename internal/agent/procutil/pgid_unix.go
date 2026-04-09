//go:build unix

package procutil

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// SetProcessGroup configures cmd to start in its own process group.
// Must be called before [exec.Cmd.Start]. Any pre-existing
// [syscall.SysProcAttr] fields are preserved.
func SetProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// SignalProcessGroup sends sig to the entire process group led by pid.
// Returns nil if the process group no longer exists (ESRCH), since
// group expiry is expected during best-effort cleanup.
func SignalProcessGroup(pid int, sig syscall.Signal) error {
	err := syscall.Kill(-pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// KillProcessGroup sends SIGKILL to the entire process group led by
// pid. Returns nil if the process group no longer exists.
func KillProcessGroup(pid int) error {
	return SignalProcessGroup(pid, syscall.SIGKILL)
}

// SignalGraceful sends SIGTERM to the entire process group led by pid.
func SignalGraceful(pid int) error {
	return SignalProcessGroup(pid, syscall.SIGTERM)
}

// AssignProcess is a no-op on Unix. Process group membership is
// established at fork time via Setpgid.
func AssignProcess(_ int, _ *os.Process) error { return nil }

// CleanupProcess is a no-op on Unix. Process group resources are
// managed by the kernel.
func CleanupProcess(_ int) {}
