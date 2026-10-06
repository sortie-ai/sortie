//go:build windows

package procutil

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"

	"golang.org/x/sys/windows"
)

// Group is the record of one launch started through a procutil start
// function. Every termination procutil performs on the launch's process
// group (Unix) or Job Object (Windows) goes through it, so no teardown
// can act on a process that later received the launch's process
// identifier. Obtain one from [StartWithOwnedPipes]; the zero value is
// not usable, and a nil *Group stands for a launch that never started.
type Group struct {
	cmd *exec.Cmd
	// assigned closes once the Job Object assignment has finished,
	// whether or not it succeeded.
	assigned chan struct{}
	mu       sync.Mutex
	released bool
	// reaperStarted is guarded by mu.
	reaperStarted bool
	// stopped is guarded by mu. The first stop that finds the direct
	// child running before the release sets it, and nothing clears it.
	stopped bool
	// job is the launch's own Job Object handle, guarded by mu. It is
	// zero when assignment failed and after the release, and only the
	// launch's reaper closes it.
	job windows.Handle
}

func newGroup(cmd *exec.Cmd) *Group {
	return &Group{cmd: cmd, assigned: make(chan struct{})}
}

// finishAssignment records the outcome of the Job Object assignment,
// zero for a failed one, and releases every caller waiting on it.
func (g *Group) finishAssignment(job windows.Handle) {
	g.mu.Lock()
	g.job = job
	g.mu.Unlock()
	close(g.assigned)
}

// SignalGraceful sends CTRL_BREAK_EVENT to the launch's console process
// group. It sends nothing once os/exec has released its handle to the
// direct child, because the process identifier may then belong to
// another process. A nil receiver does nothing.
func (g *Group) SignalGraceful() error {
	if g == nil {
		return nil
	}
	<-g.assigned

	g.mu.Lock()
	g.markStopLocked()
	g.mu.Unlock()

	pid, err := dwordPID(g.cmd.Process.Pid)
	if err != nil {
		return err
	}
	var sendErr error
	if withErr := g.cmd.Process.WithHandle(func(uintptr) {
		sendErr = windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, pid)
	}); withErr != nil {
		return nil
	}
	return sendErr
}

// Kill terminates every process of the launch's Job Object, resending
// the termination until the job has no running member or the group
// drain bound passes. Without a Job Object it kills the direct child. It
// does nothing once the launch's reap has released the record. A nil
// receiver does nothing.
func (g *Group) Kill() error {
	if g == nil {
		return nil
	}
	<-g.assigned

	g.mu.Lock()
	if g.released {
		g.mu.Unlock()
		return nil
	}
	g.markStopLocked()
	if g.job == 0 {
		g.mu.Unlock()
		err := killDirectChild(g)
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return err
	}
	job, dupErr := duplicateJob(g.job)
	g.mu.Unlock()
	if dupErr != nil {
		return dupErr
	}
	defer func() { _ = windows.CloseHandle(job) }()

	return drainJobObject(g.cmd.Process.Pid, job)
}

// duplicateJob returns a handle of the caller's own to the Job Object
// job names. It does not close job.
func duplicateJob(job windows.Handle) (windows.Handle, error) {
	self := windows.CurrentProcess()
	var dup windows.Handle
	if err := windows.DuplicateHandle(self, job, self, &dup, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
		return 0, fmt.Errorf("duplicating job object handle: %w", err)
	}
	return dup, nil
}

// captureEscalation returns a target holding a duplicate of the
// launch's Job Object handle, so the forced stage acts on that job
// alone however long it takes. It returns false when the launch has no
// Job Object or its reap has already released the record.
func (g *Group) captureEscalation() (escalationTarget, bool) {
	<-g.assigned

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.released || g.job == 0 {
		return nil, false
	}
	job, err := duplicateJob(g.job)
	if err != nil {
		return nil, false
	}
	return jobEscalationTarget{pid: g.cmd.Process.Pid, job: job}, true
}

// jobEscalationTarget acts on a duplicate of one launch's Job Object
// handle and owns that duplicate.
type jobEscalationTarget struct {
	pid int
	job windows.Handle
}

func (t jobEscalationTarget) hasRunningMember() (bool, error) {
	return jobHasRunningMember(t.job)
}

func (t jobEscalationTarget) terminateAll() error {
	return drainJobObject(t.pid, t.job)
}

func (t jobEscalationTarget) close() {
	_ = windows.CloseHandle(t.job)
}

// directChildRunning reports whether the direct child of cmd has not
// exited. A failure to reach its handle means os/exec has already
// released it after the wait; a wait that fails counts as running.
func directChildRunning(cmd *exec.Cmd) bool {
	running := true
	if withErr := cmd.Process.WithHandle(func(handle uintptr) {
		event, waitErr := windows.WaitForSingleObject(windows.Handle(handle), 0)
		running = waitErr != nil || event == uint32(windows.WAIT_TIMEOUT)
	}); withErr != nil {
		return false
	}
	return running
}

// directChildGone reports whether err says the direct child is already
// gone.
func directChildGone(err error) bool {
	return processAlreadyGone(err)
}

// reap waits for the direct child, terminates what its Job Object still
// holds through the launch's own handle, then releases the record and
// closes that handle.
func (g *Group) reap() (waitErr error, leftover bool, cleanupErr error) {
	waitErr = g.cmd.Wait()

	g.mu.Lock()
	job := g.job
	g.mu.Unlock()
	if job != 0 {
		leftover, _ = jobHasRunningMember(job)
		cleanupErr = drainJobObject(g.cmd.Process.Pid, job)
	}

	g.mu.Lock()
	g.released = true
	if g.job != 0 {
		_ = windows.CloseHandle(g.job)
		g.job = 0
	}
	g.mu.Unlock()
	return waitErr, leftover, cleanupErr
}
