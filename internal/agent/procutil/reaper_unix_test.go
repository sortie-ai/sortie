//go:build linux

package procutil

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
)

func init() {
	fakeScenarios["procutil.reaper-leader"] = agenttest.Typed(runReaperLeader)
}

type reaperLeaderParams struct {
	ChildPath string
	PIDFile   string
}

func runReaperLeader(_ []string, params reaperLeaderParams) int {
	cmd := exec.Command(params.ChildPath) //nolint:gosec // fake runtime path under t.TempDir()
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "reaper leader: start child: %v\n", err)
		return 1
	}
	if err := os.WriteFile(params.PIDFile, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "reaper leader: write pid: %v\n", err)
		return 1
	}
	return 0
}

func isReaperTestZombie(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	if i := strings.LastIndex(string(data), ")"); i >= 0 && i+2 < len(data) {
		return data[i+2] == 'Z'
	}
	return false
}

func assertReaperTestProcessDead(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		if isReaperTestZombie(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("process %d still alive after %v, want gone", pid, timeout)
}

func TestReaper_KillsProcessGroupBeforeDoneCloses(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	childPath := agenttest.FakeRuntime(t, dir, "reaper-child", agenttest.OutputScenario, agenttest.Output{Hang: true})
	leaderPath := agenttest.FakeRuntime(t, dir, "reaper-leader", "procutil.reaper-leader", reaperLeaderParams{
		ChildPath: childPath,
		PIDFile:   pidFile,
	})

	cmd := exec.Command(leaderPath) //nolint:gosec // fake runtime path under t.TempDir()
	startOwned(t, cmd)

	childPID := pollReaperTestPIDFile(t, pidFile, 5*time.Second)

	r := StartReaper(cmd, nil)
	awaitDone(t, r, 5*time.Second)

	assertReaperTestProcessDead(t, childPID, 3*time.Second)
}

func pollReaperTestPIDFile(t *testing.T, pidFile string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			pid, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if convErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pollReaperTestPIDFile(%q): no valid PID after %v", pidFile, timeout)
	return 0
}
