package procutil

import (
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
