package procutil

import (
	"os"
	"os/exec"
	"time"
)

// SetGroupCancel configures graceful then forced cancellation of cmd's process group.
// A non-positive grace uses [DefaultStopGrace].
//
// Call it before [exec.Cmd.Start], on a command created with
// [exec.CommandContext]: os/exec rejects a cancellation function on a
// command built without a context.
func SetGroupCancel(cmd *exec.Cmd, grace time.Duration) {
	if grace <= 0 {
		grace = DefaultStopGrace
	}
	SetProcessGroup(cmd)
	cmd.Cancel = func() error {
		g := lookupGroup(cmd)
		if g == nil {
			return cmd.Process.Kill()
		}
		err := g.SignalGraceful()
		armGroupEscalation(g, grace)
		return err
	}
	cmd.WaitDelay = grace
}

// SetGroupKill configures immediate forced cancellation of cmd's process group.
//
// Call it before [exec.Cmd.Start], on a command created with
// [exec.CommandContext].
func SetGroupKill(cmd *exec.Cmd) {
	SetProcessGroup(cmd)
	cmd.Cancel = func() error {
		g := lookupGroup(cmd)
		if g == nil {
			return cmd.Process.Kill()
		}
		killErr := g.Kill()
		procErr := killDirectChild(g)
		if killErr != nil {
			return killErr
		}
		return procErr
	}
}

// killDirectChild kills g's direct child and reports [os.ErrProcessDone]
// when the launch is already released or the child already gone. It
// holds the record's mutex so that on Unix the kill precedes the reap.
func killDirectChild(g *Group) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.released {
		return os.ErrProcessDone
	}
	err := g.cmd.Process.Kill()
	if directChildGone(err) {
		return os.ErrProcessDone
	}
	return err
}
