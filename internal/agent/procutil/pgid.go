package procutil

import (
	"os/exec"
	"time"
)

// SetGroupCancel prepares cmd so that cancelling the context it was
// built with tears down the whole process group rather than the direct
// child alone. It places cmd in its own process group, sends that group
// a catchable termination signal when the context is cancelled, and caps
// the wait that follows at grace before os/exec escalates to a force
// kill. A non-positive grace resolves to [DefaultStopGrace]: os/exec
// reads a zero WaitDelay as no limit, so passing zero through would
// silently remove the escalation to a force kill.
//
// Call it before [exec.Cmd.Start], on a command created with
// [exec.CommandContext]: os/exec rejects a cancellation function on a
// command built without a context.
//
// A launcher that skips it inherits os/exec's default, which kills only
// the direct child and does so uncatchably. A subprocess that flushes
// state on a clean exit never reaches that path, and every descendant it
// started outlives the cancellation.
func SetGroupCancel(cmd *exec.Cmd, grace time.Duration) {
	SetProcessGroup(cmd)
	cmd.Cancel = func() error {
		return SignalGraceful(cmd.Process.Pid)
	}
	if grace <= 0 {
		grace = DefaultStopGrace
	}
	cmd.WaitDelay = grace
}

// SetGroupKill prepares cmd so that cancelling the context it was built
// with terminates its process group (or Job Object on Windows) at once,
// with no catchable signal first. It places cmd in its own process
// group and leaves cmd.WaitDelay zero: a capture's own pipes are never
// waited on by os/exec, so nothing needs the escalation to a force kill
// that WaitDelay exists to provide.
//
// Call it before [exec.Cmd.Start], on a command created with
// [exec.CommandContext].
func SetGroupKill(cmd *exec.Cmd) {
	SetProcessGroup(cmd)
	cmd.Cancel = func() error {
		killErr := KillProcessGroup(cmd.Process.Pid)
		procErr := cmd.Process.Kill()
		if killErr != nil {
			return killErr
		}
		return procErr
	}
}
