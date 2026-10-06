//go:build unix

package procutil

import (
	"log/slog"
	"os/exec"
	"time"
)

// startAndAssign places cmd in its own process group, registers its
// launch record, and starts it. Job Object assignment has no Unix
// analogue: process-group membership is established at fork time via
// Setpgid, so keepJobHandle is unused and the returned handle is always
// zero. The returned record is nil on error. The returned time is the
// moment cmd.Start returned, the zero value when it failed. A failed
// start is a [StartError] at [StageProcessStart].
func startAndAssign(cmd *exec.Cmd, _ *slog.Logger, _ bool) (*Group, uintptr, time.Time, *StartError) {
	SetProcessGroup(cmd)

	// os/exec may run Cancel as soon as Start returns, and a cancellation
	// that found no record would skip the group.
	g := newGroup(cmd)
	groups.Store(cmd, g)
	recordCancelStop(cmd, g)

	if err := cmd.Start(); err != nil {
		groups.Delete(cmd)
		return nil, 0, time.Time{}, processStartError(err)
	}
	return g, 0, time.Now(), nil
}

// drainCaptureJob is a no-op on Unix: there is no Job Object to drain.
func drainCaptureJob(_ uintptr, _ *exec.Cmd, _ time.Time, _ int64, _ *slog.Logger) {}
