package procutil

import (
	"os"
	"os/exec"
	"sync"
)

// groups maps the *exec.Cmd of every launch started through a procutil
// start function to that launch's [Group]. The command pointer is the
// only key: nothing in this package finds a record, a Job Object handle
// or any other teardown state by process identifier, because the
// operating system may give an identifier to another launch once its
// holder has been reaped.
var groups sync.Map

// lookupGroup returns the record of the launch cmd belongs to, or nil
// when cmd was not started through a procutil start function or its
// reaper has already unregistered it.
func lookupGroup(cmd *exec.Cmd) *Group {
	v, ok := groups.Load(cmd)
	if !ok {
		return nil
	}
	g, ok := v.(*Group)
	if !ok {
		return nil
	}
	return g
}

// Stopped reports whether a stop began while the launch's direct child
// was still running, as opposed to the child reaching an exit of its
// own. It is live while the launch runs and final once the launch's reap
// has released the record. A nil receiver reports false.
func (g *Group) Stopped() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stopped
}

// markStopLocked records that a stop is about to act and reports
// whether the direct child was running when it did. A stop that finds
// the child already exited, or the record released, records nothing.
// The caller holds g.mu, which is what orders the probe ahead of the
// stop's own effect. A probe that fails counts as running.
func (g *Group) markStopLocked() bool {
	if g.released || !directChildRunning(g.cmd) {
		return false
	}
	g.stopped = true
	return true
}

// recordCancelStop wraps cmd.Cancel, whichever function installed it, so
// the cancellation os/exec runs is recorded as a stop. When the direct
// child had already exited, the wrapper reports [os.ErrProcessDone]:
// os/exec would otherwise replace the nil wait error of a child that
// exited zero with the context's error.
func recordCancelStop(cmd *exec.Cmd, g *Group) {
	inner := cmd.Cancel
	if inner == nil {
		return
	}
	cmd.Cancel = func() error {
		g.mu.Lock()
		running := g.markStopLocked()
		g.mu.Unlock()

		err := inner()
		if !running {
			return os.ErrProcessDone
		}
		return err
	}
}

// claimReaper marks the record's reaper as started and reports whether
// it already was, so a launch is never reaped by two goroutines.
func (g *Group) claimReaper() (alreadyStarted bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	alreadyStarted = g.reaperStarted
	g.reaperStarted = true
	return alreadyStarted
}

// escalationTarget is what the forced stage of a graceful cancellation
// acts on, resolved from the record when the cancellation starts.
type escalationTarget interface {
	// hasRunningMember reports whether the launch's tree can still be
	// terminated through this target.
	hasRunningMember() (bool, error)
	// terminateAll terminates the launch's tree.
	terminateAll() error
	// close releases whatever the target holds.
	close()
}
