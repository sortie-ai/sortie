//go:build linux

package agentcore

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
)

// writePgidScript creates a script that spawns a long-running grandchild
// (sleep 3600 &), writes the grandchild PID to pidFile, emits a JSONL
// notification line to stdout, then blocks. Using agenttest.WriteScript
// avoids the ETXTBSY race on Linux.
func writePgidScript(t *testing.T, dir, pidFile string) string {
	t.Helper()
	content := fmt.Sprintf(
		"sleep 3600 &\n"+
			"CHILD_PID=$!\n"+
			"printf '%%s\\n' \"$CHILD_PID\" > '%s'\n"+
			"printf '{\"type\":\"notification\"}\\n'\n"+
			"sleep 3600\n",
		pidFile,
	)
	return agenttest.WriteScript(t, dir, "agent-pgid", content)
}

// pollPgidFile polls pidFile until it contains a valid positive integer
// PID, or fails the test after timeout.
func pollPgidFile(t *testing.T, pidFile string, timeout time.Duration) int {
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
	t.Fatalf("pollPgidFile(%q): no valid PID after %v", pidFile, timeout)
	return 0
}

// isPgidZombie reports whether pid is a zombie by reading /proc/<pid>/stat.
// Returns false if the file cannot be read.
func isPgidZombie(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	if i := strings.LastIndex(string(data), ")"); i >= 0 && i+2 < len(data) {
		return data[i+2] == 'Z'
	}
	return false
}

// assertPgidProcessDead polls until the process is gone (or a zombie) or
// the timeout expires.
func assertPgidProcessDead(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return // any error means the process is gone or unreachable
		}
		if isPgidZombie(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("assertPgidProcessDead: process %d still alive after %v", pid, timeout)
}

// TestForkPerTurnSession_ProcessGroupIsolation verifies that cancelling the
// RunTurn context sends SIGTERM to the entire process group, killing both
// the agent subprocess and any grandchildren it spawned.
func TestForkPerTurnSession_ProcessGroupIsolation(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	pidFile := filepath.Join(tmpDir, "grandchild.pid")
	script := writePgidScript(t, tmpDir, pidFile)

	target := newTestTarget(tmpDir, script)
	sess := NewForkPerTurnSession(target, noopHooks(), slog.Default())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	emit, _ := sinkEvents()
	done := make(chan struct{})
	go func() {
		defer close(done)
		sess.RunTurn(ctx, "p", emit) //nolint:errcheck // testing process group kill
	}()

	grandchildPID := pollPgidFile(t, pidFile, 5*time.Second)

	cancel()
	<-done

	assertPgidProcessDead(t, grandchildPID, 3*time.Second)
}
