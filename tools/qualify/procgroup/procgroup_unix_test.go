//go:build unix

package procgroup

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
)

// procgroupLeaderScenario is a fake leader that backgrounds hangPath in its
// own process group and exits immediately, mirroring a shell leader that
// returns before the job it started.
const procgroupLeaderScenario = "leader"

func spawnDetachedGroupChild(_ []string, hangPath string) int {
	cmd := exec.Command(hangPath) //nolint:gosec // hangPath is a fake runtime this test built under its own temp directory
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "leader: start %s: %v\n", hangPath, err)
		return 1
	}
	return 0
}

func TestMain(m *testing.M) {
	agenttest.Main(m, map[string]agenttest.Scenario{
		procgroupLeaderScenario: agenttest.Typed(spawnDetachedGroupChild),
	})
}

// startTrackedGroup starts cmd in its own process group and returns it with
// its PGID, registering a cleanup that kills the group and reaps its leader
// so no zombie lingers past the test.
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

func TestProcessGroupAbsenceOracle(t *testing.T) {
	t.Parallel()

	t.Run("a live group is present", func(t *testing.T) {
		t.Parallel()

		hangPath := agenttest.FakeRuntime(t, t.TempDir(), "runtime", agenttest.OutputScenario, agenttest.Output{Hang: true})
		_, pgid := startTrackedGroup(t, exec.Command(hangPath)) //nolint:gosec // hangPath is a fake runtime this test built under its own temp directory
		present, err := Present(pgid)
		if err != nil {
			t.Fatalf("Present() error = %v, want nil", err)
		}
		if !present {
			t.Error("Present() = false for a live group, want true")
		}
	})

	t.Run("a killed group drains to absence within the deadline", func(t *testing.T) {
		t.Parallel()

		hangPath := agenttest.FakeRuntime(t, t.TempDir(), "runtime", agenttest.OutputScenario, agenttest.Output{Hang: true})
		cmd, pgid := startTrackedGroup(t, exec.Command(hangPath)) //nolint:gosec // hangPath is a fake runtime this test built under its own temp directory
		_ = procutil.SignalProcessGroup(pgid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
		AwaitAbsence(t, pgid)
		if present, err := Present(pgid); present || err != nil {
			t.Errorf("Present() = %v, %v, want false, nil after cleanup", present, err)
		}
	})

	t.Run("a surviving grandchild keeps the group present", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		hangPath := agenttest.FakeRuntime(t, dir, "grandchild", agenttest.OutputScenario, agenttest.Output{Hang: true})
		script := agenttest.FakeRuntime(t, dir, "leader", procgroupLeaderScenario, hangPath)
		cmd, pgid := startTrackedGroup(t, exec.Command(script)) //nolint:gosec // script is a fake runtime this test built under its own temp directory

		time.Sleep(50 * time.Millisecond)
		if present, err := Present(pgid); err != nil || !present {
			t.Fatalf("Present() = %v, %v, want the group alive while the grandchild survives", present, err)
		}
		if err := procutil.SignalProcessGroup(pgid, syscall.SIGKILL); err != nil {
			t.Fatalf("SignalProcessGroup(SIGKILL) error = %v", err)
		}
		_, _ = cmd.Process.Wait()
		AwaitAbsence(t, pgid)
	})
}

// TestProcessGroupPresentRejectsUnqueryableIDs pins the guard on ids at or
// below 1, which would otherwise answer present for a survivor that never
// existed.
func TestProcessGroupPresentRejectsUnqueryableIDs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		pgid int
	}{
		{name: "zero addresses the caller's own process group", pgid: 0},
		{name: "one addresses every process the caller may signal", pgid: 1},
		{name: "a negative id addresses a process rather than a group", pgid: -5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			present, err := Present(tt.pgid)
			if err == nil {
				t.Fatalf("Present(%d) error = nil, want a rejection", tt.pgid)
			}
			if present {
				t.Errorf("Present(%d) present = true, want false alongside the rejection", tt.pgid)
			}
		})
	}
}
