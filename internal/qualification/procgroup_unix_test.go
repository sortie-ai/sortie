//go:build unix

package qualification

import (
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
)

// startTrackedGroup starts cmd in its own process group through the
// production launch contract and returns the started command and its
// PGID. It registers a cleanup that signals the group and reaps its
// leader, so an unreaped zombie never lingers past the test.
func startTrackedGroup(t *testing.T, cmd *exec.Cmd) (*exec.Cmd, int) {
	t.Helper()
	procutil.SetProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start tracked process: %v", err)
	}
	pgid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = procutil.SignalProcessGroup(pgid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	})
	return cmd, pgid
}

// TestProcessGroupAbsenceOracle confirms the exact negative-PGID
// oracle: a live group is present, a killed single-member group drains
// to absence within the shared deadline, and a group whose leader dies
// while a grandchild keeps the group alive still reports present until
// the whole group is terminated.
func TestProcessGroupAbsenceOracle(t *testing.T) {
	t.Parallel()

	t.Run("a live group is present", func(t *testing.T) {
		t.Parallel()

		_, pgid := startTrackedGroup(t, exec.Command("sleep", "5")) //nolint:gosec // bounded fake local process
		present, err := ProcessGroupPresent(pgid)
		if err != nil {
			t.Fatalf("ProcessGroupPresent() error = %v, want nil", err)
		}
		if !present {
			t.Error("ProcessGroupPresent() = false for a live group, want true")
		}
	})

	t.Run("a killed group drains to absence within the deadline", func(t *testing.T) {
		t.Parallel()

		cmd, pgid := startTrackedGroup(t, exec.Command("sleep", "60")) //nolint:gosec // bounded fake local process killed below
		_ = procutil.SignalProcessGroup(pgid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
		AwaitProcessGroupAbsence(t, pgid)
		if present, err := ProcessGroupPresent(pgid); present || err != nil {
			t.Errorf("ProcessGroupPresent() = %v, %v, want false, nil after cleanup", present, err)
		}
	})

	t.Run("a surviving grandchild keeps the group present", func(t *testing.T) {
		t.Parallel()

		// The leader forks a grandchild into the same group and exits;
		// the group survives while the grandchild does.
		script := agenttest.WriteScript(t, t.TempDir(), "leader.sh", "sleep 60 &\nexit 0\n")
		cmd, pgid := startTrackedGroup(t, exec.Command(script)) //nolint:gosec // test-owned script under the test's temp directory

		// A bounded settle lets the leader fork and exit before the
		// first liveness check.
		time.Sleep(50 * time.Millisecond)
		if present, err := ProcessGroupPresent(pgid); err != nil || !present {
			t.Fatalf("ProcessGroupPresent() = %v, %v, want the group alive while the grandchild survives", present, err)
		}
		if err := procutil.SignalProcessGroup(pgid, syscall.SIGKILL); err != nil {
			t.Fatalf("SignalProcessGroup(SIGKILL) error = %v", err)
		}
		_, _ = cmd.Process.Wait()
		AwaitProcessGroupAbsence(t, pgid)
	})
}
