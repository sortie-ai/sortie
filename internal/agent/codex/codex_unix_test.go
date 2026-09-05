//go:build linux

package codex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
)

// writeFakeAppServerScript creates a script that fakes just enough of the
// codex app-server JSON-RPC handshake (initialize, account/read,
// thread/start) for StartSession to succeed, then spawns a long-running
// grandchild (sleep 3600 &), writes the grandchild's PID to pidFile, and
// blocks so the session stays "running" until it is signalled.
//
// Response ids are hardcoded (1, 2, 3) rather than parsed from the request
// lines: jsonrpc.Conn allocates ids sequentially starting at 1, and
// StartSession issues exactly these three calls in this order, so the
// sequence is deterministic. Using agenttest.WriteScript avoids the
// ETXTBSY race on Linux.
func writeFakeAppServerScript(t *testing.T, dir, pidFile string) string {
	t.Helper()
	content := fmt.Sprintf(
		"read -r _init_req\n"+
			"printf '{\"id\":1,\"result\":{}}\\n'\n"+
			"read -r _initialized_notif\n"+
			"read -r _account_read_req\n"+
			"printf '{\"id\":2,\"result\":{}}\\n'\n"+
			"read -r _thread_start_req\n"+
			"printf '{\"id\":3,\"result\":{\"thread\":{\"id\":\"fake-thread-1\"}}}\\n'\n"+
			"printf '{\"method\":\"thread/started\",\"params\":{}}\\n'\n"+
			"sleep 3600 &\n"+
			"CHILD_PID=$!\n"+
			"printf '%%s\\n' \"$CHILD_PID\" > '%s'\n"+
			"sleep 3600\n",
		pidFile,
	)
	return agenttest.WriteScript(t, dir, "fake-codex-app-server", content)
}

// pollPIDFile polls pidFile until it contains a valid positive integer PID,
// or fails the test after timeout.
func pollPIDFile(t *testing.T, pidFile string, timeout time.Duration) int {
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
	t.Fatalf("pollPIDFile(%q): no valid PID after %v", pidFile, timeout)
	return 0
}

// isZombie reports whether pid is a zombie by reading /proc/<pid>/stat.
// Returns false if the file cannot be read.
func isZombie(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	if i := strings.LastIndex(string(data), ")"); i >= 0 && i+2 < len(data) {
		return data[i+2] == 'Z'
	}
	return false
}

// assertProcessDead polls until pid is gone (or a zombie) or the timeout
// expires.
func assertProcessDead(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return // any error means the process is gone or unreachable
		}
		if isZombie(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("assertProcessDead: process %d still alive after %v", pid, timeout)
}

// TestStartSession_CancelSignalsProcessGroup verifies that cancelling the
// context passed to StartSession, after the session has already been
// established, terminates the app-server subprocess through the process
// group rather than os/exec's default of killing only the direct child, so
// a descendant the agent spawned does not survive the cancellation.
//
// Regression test for the defect described in #1013: with neither
// cmd.Cancel nor cmd.WaitDelay set, a cancelled launch context force-kills
// only cmd.Process, leaving any descendant it started (here, the "sleep
// 3600 &" job the fake app-server spawns) running.
//
// Not run with t.Parallel(): it pins CODEX_API_KEY to empty via t.Setenv so
// the fake app-server's account/read reply always takes the no-login
// branch in authenticateIfNeeded, regardless of the ambient environment,
// and t.Setenv forbids parallel use.
func TestStartSession_CancelSignalsProcessGroup(t *testing.T) {
	t.Setenv("CODEX_API_KEY", "")

	tmpDir := t.TempDir()
	pidFile := filepath.Join(tmpDir, "descendant.pid")
	script := writeFakeAppServerScript(t, tmpDir, pidFile)

	adapter, err := NewCodexAdapter(map[string]any{})
	if err != nil {
		t.Fatalf("NewCodexAdapter() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	session, err := adapter.StartSession(ctx, domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	directPID, convErr := strconv.Atoi(session.AgentPID)
	if convErr != nil {
		t.Fatalf("session.AgentPID = %q, want a PID: %v", session.AgentPID, convErr)
	}

	descendantPID := pollPIDFile(t, pidFile, 5*time.Second)

	cancel()

	assertProcessDead(t, directPID, 3*time.Second)
	assertProcessDead(t, descendantPID, 3*time.Second)
}
