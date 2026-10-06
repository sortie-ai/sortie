//go:build unix

package procutil

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Group is the record of one launch started through a procutil start
// function. Every termination procutil performs on the launch's process
// group (Unix) or Job Object (Windows) goes through it, so no teardown
// can act on a process that later received the launch's process
// identifier. Obtain one from [StartWithOwnedPipes]; the zero value is
// not usable, and a nil *Group stands for a launch that never started.
type Group struct {
	cmd *exec.Cmd
	// mu is held across every send addressed by group number and across
	// the release, so a send never follows the release.
	mu            sync.Mutex
	released      bool
	reaperStarted bool
	// stopped is set by the first stop that finds the direct child
	// running before the release, and never cleared.
	stopped bool
}

// leaderExitFunc blocks until the direct child has exited and never
// reaps it. Only a test replaces it.
var leaderExitFunc = observeLeaderExit

// liveMemberFunc reports whether a live process other than leader
// belongs to process group pgid. Only a test replaces it.
var liveMemberFunc = hasLiveMember

// releaseSeam runs between the release of a launch's record and its
// cmd.Wait, the window in which a cancellation finds the direct child
// exited and the record released. Its zero value is a no-op; only a
// test replaces it.
var releaseSeam = func() {}

func newGroup(cmd *exec.Cmd) *Group {
	return &Group{cmd: cmd}
}

// SignalGraceful sends SIGTERM to the launch's process group. It sends
// nothing once the launch's reap has taken over, and treats a group
// that is already gone or unsignalable as success. A nil receiver does
// nothing.
func (g *Group) SignalGraceful() error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.released {
		return nil
	}
	g.markStopLocked()
	err := groupKillFunc(-g.cmd.Process.Pid, syscall.SIGTERM)
	if errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EPERM) {
		return nil
	}
	return err
}

// Kill sends SIGKILL to the launch's process group, resending it until
// the group is gone, the launch's reap has taken over, or the group
// drain bound passes. The direct child stays an unreaped member of the
// group until the reap takes over, so the bound can elapse while it
// holds the group. A nil receiver does nothing.
func (g *Group) Kill() error {
	if g == nil {
		return nil
	}
	pid := g.cmd.Process.Pid
	deadline := time.Now().Add(groupDrainBound)
	for {
		g.mu.Lock()
		if g.released {
			g.mu.Unlock()
			return nil
		}
		g.markStopLocked()
		err := groupKillFunc(-pid, syscall.SIGKILL)
		g.mu.Unlock()

		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if err != nil && !errors.Is(err, syscall.EPERM) {
			return err
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("process group %d was still held by its unreaped direct child after the %s drain bound", pid, groupDrainBound)
		}
		time.Sleep(groupDrainPollInterval)
	}
}

// captureEscalation returns the record as the target of a forced
// stage, and false when the launch's reap has already taken over.
func (g *Group) captureEscalation() (escalationTarget, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.released {
		return nil, false
	}
	return g, true
}

func (g *Group) hasRunningMember() (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return !g.released, nil
}

func (g *Group) terminateAll() error {
	return g.Kill()
}

func (g *Group) close() {}

// directChildGone reports whether err says the direct child is already
// gone.
func directChildGone(err error) bool {
	return errors.Is(err, os.ErrProcessDone)
}

// reap waits for the direct child to exit without reaping it, drains
// the group while the unreaped child still pins its identifier, then
// releases the record and reaps the child. Releasing first guarantees
// that no send addressed by group number follows the reap.
func (g *Group) reap() (waitErr error, leftover bool, cleanupErr error) {
	pid := g.cmd.Process.Pid
	if exitErr := leaderExitFunc(pid); exitErr != nil {
		cleanupErr = fmt.Errorf("observing the exit of process %d without reaping it: %w", pid, exitErr)
	} else {
		leftover, cleanupErr = reapDrain(g)
	}

	g.mu.Lock()
	g.released = true
	g.mu.Unlock()

	releaseSeam()

	waitErr = g.cmd.Wait()
	return waitErr, leftover, cleanupErr
}

// reapDrain resends SIGKILL to the group until no live member remains
// or groupDrainBound passes. The first send goes out even when no live
// member was seen: it reaches a member the membership query passed over
// while it forked, and once every member holds a pending SIGKILL none
// can create another, so the queries after it are complete.
func reapDrain(g *Group) (leftover bool, err error) {
	pid := g.cmd.Process.Pid
	deadline := time.Now().Add(groupDrainBound)

	live, queryErr := liveMemberFunc(pid, pid)
	leftover = queryErr == nil && live
	for {
		g.mu.Lock()
		killErr := groupKillFunc(-pid, syscall.SIGKILL)
		g.mu.Unlock()

		if errors.Is(killErr, syscall.ESRCH) {
			return leftover, nil
		}
		if killErr != nil && !errors.Is(killErr, syscall.EPERM) {
			return leftover, killErr
		}

		live, queryErr = liveMemberFunc(pid, pid)
		if queryErr == nil {
			if !live {
				return leftover, nil
			}
			leftover = true
		}

		if !time.Now().Before(deadline) {
			if queryErr != nil {
				return leftover, fmt.Errorf("process group %d could not be confirmed empty within the %s drain bound: %w", pid, groupDrainBound, queryErr)
			}
			return leftover, fmt.Errorf("process group %d still had a live member after the %s drain bound", pid, groupDrainBound)
		}
		time.Sleep(groupDrainPollInterval)
	}
}
