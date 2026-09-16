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

// groupKillFunc is killProcessGroupReportingLeftover's underlying
// signal call. Only a test replaces it, to simulate a member the
// bounded wait never confirms gone.
var groupKillFunc = syscall.Kill

// killProcessGroupReportingLeftover sends SIGKILL to the process group
// led by pid, resending it every groupDrainPollInterval until a send
// finds no group left to receive it or groupDrainBound passes, and
// reports whether the first send reached a member: a direct child
// StartReaper has already reaped no longer belongs to the group, so a
// first send that reaches a live process means at least one other
// member was still alive to receive it.
//
// SIGKILL delivery is asynchronous and does not reap a member stuck in
// an uninterruptible wait, so a member the first send reached can
// outlive it; resending on every poll also reaches a member the group
// gains after the first send, the same way the Windows drain this
// shares its bound with repeats its own termination call. A non-nil
// error means the group still answered a send when the bound ran out,
// so a descendant may have survived the reap.
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

// assignProcess is a no-op on Unix. Process group membership is
// established at fork time via Setpgid.
func assignProcess(_ int, _ *os.Process) error { return nil }

// CleanupProcess is a no-op on Unix. Process group resources are
// managed by the kernel.
func CleanupProcess(_ int) {}

// groupHasMember reports whether the process group led by pid still
// holds a member, by sending signal 0 to it: kill(2) delivers nothing
// for signal 0 and reports only whether a target to deliver to exists.
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

// groupEscalationTarget is the process group an armed escalation
// force-terminates at the stop grace deadline, gated on a membership
// observation. The zero value is not usable; construct it with
// [captureGroupEscalation].
type groupEscalationTarget struct {
	pid int
}

// captureGroupEscalation takes the target the escalation
// force-terminates for the process group led by pid. It always reports
// true: a process group identifier needs no resource held on its
// behalf.
func captureGroupEscalation(pid int) (groupEscalationTarget, bool) {
	return groupEscalationTarget{pid: pid}, true
}

// hasMember reports whether t's process group still holds a member.
func (t groupEscalationTarget) hasMember() (bool, error) {
	return groupHasMember(t.pid)
}

// terminate force-terminates t's process group, resending until the
// group reports no member left or the group-drain bound elapses.
func (t groupEscalationTarget) terminate() error {
	return KillProcessGroup(t.pid)
}

// release is a no-op: t holds no resource to give back.
func (t groupEscalationTarget) release() {}
