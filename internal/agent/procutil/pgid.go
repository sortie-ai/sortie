package procutil

import (
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
		err := SignalGraceful(cmd.Process.Pid)
		armGroupEscalation(cmd.Process.Pid, grace)
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
		killErr := KillProcessGroup(cmd.Process.Pid)
		procErr := cmd.Process.Kill()
		if killErr != nil {
			return killErr
		}
		return procErr
	}
}
