//go:build unix

package procutil

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
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
// pid, resending it until the group reports no member or
// groupDrainBound passes. Returns nil if the process group no longer
// exists within that bound.
func KillProcessGroup(pid int) error {
	_, err := killProcessGroupReportingLeftover(pid)
	return err
}

var groupKillFunc = syscall.Kill

// SIGKILL delivery is asynchronous and does not reap a member stuck in
// an uninterruptible wait, so the signal is resent until the group drains.
func killProcessGroupReportingLeftover(pid int) (leftover bool, err error) {
	deadline := time.Now().Add(groupDrainBound)
	for {
		killErr := groupKillFunc(-pid, syscall.SIGKILL)
		if errors.Is(killErr, syscall.ESRCH) {
			return leftover, nil
		}
		if killErr != nil {
			return leftover, killErr
		}
		leftover = true

		if !time.Now().Before(deadline) {
			return leftover, fmt.Errorf("process group %d still had a member after the %s drain bound", pid, groupDrainBound)
		}
		time.Sleep(groupDrainPollInterval)
	}
}

// SignalGraceful sends SIGTERM to the entire process group led by pid.
func SignalGraceful(pid int) error {
	return SignalProcessGroup(pid, syscall.SIGTERM)
}

func assignProcess(_ int, _ *os.Process) error { return nil }

// CleanupProcess releases platform-specific process-group resources.
func CleanupProcess(_ int) {}

func groupHasMember(pid int) (bool, error) {
	err := groupKillFunc(-pid, 0)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	return false, err
}

type groupEscalationTarget struct {
	pid int
}

func captureGroupEscalation(pid int) (groupEscalationTarget, bool) {
	return groupEscalationTarget{pid: pid}, true
}

func (t groupEscalationTarget) hasRunningMember() (bool, error) {
	return groupHasMember(t.pid)
}

func (t groupEscalationTarget) terminateAll() error {
	return KillProcessGroup(t.pid)
}

func (t groupEscalationTarget) close() {}
