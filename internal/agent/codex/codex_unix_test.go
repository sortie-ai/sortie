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

// writeDescendantScript creates the script the fake app-server spawns as
// a background job. It publishes its own PID to pidFile, then idles until
// a catchable termination signal arrives and records that it caught one
// by writing markerFile.
//
// The marker is the evidence that a graceful signal reached the whole
// process group rather than only its leader: this script is a grandchild
// of the adapter, so it can only be signalled through the group.
func writeDescendantScript(t *testing.T, dir, pidFile, markerFile string) string {
	t.Helper()
	content := fmt.Sprintf(
		"MARKER='%s'\n"+
			"PID_FILE='%s'\n"+
			"trap 'printf terminated > \"$MARKER\"; exit 0' TERM\n"+
			"printf '%%s\\n' \"$$\" > \"$PID_FILE\"\n"+
			"while :; do sleep 1; done\n",
		markerFile, pidFile,
	)
	return agenttest.WriteScript(t, dir, "fake-codex-descendant", content)
}

// writeFakeAppServerScript creates a script that fakes just enough of the
// codex app-server JSON-RPC handshake (initialize, account/read,
// thread/start) for StartSession to succeed, then spawns descendantScript
// as a background job and blocks until that descendant exits.
//
// The script waits for its descendant inside its own TERM handler rather
// than exiting immediately. That ordering is what makes the assertions
// deterministic: the adapter's exit watcher force-kills the group only
// after cmd.Wait returns, and cmd.Wait cannot return while this script is
// still waiting, so the force kill can never race the descendant's
// handler.
//
// Response ids are hardcoded (1, 2, 3) rather than parsed from the
// request lines: jsonrpc.Conn allocates ids sequentially starting at 1,
// and StartSession issues exactly these three calls in this order, so the
// sequence is deterministic. Using agenttest.WriteScript avoids the
// ETXTBSY race on Linux.
func writeFakeAppServerScript(t *testing.T, dir, descendantScript string) string {
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
			"'%s' &\n"+
			"DESCENDANT_PID=$!\n"+
			"trap 'wait \"$DESCENDANT_PID\"; exit 0' TERM\n"+
			"wait \"$DESCENDANT_PID\"\n",
		descendantScript,
	)
	return agenttest.WriteScript(t, dir, "fake-codex-app-server", content)
}

// pollPIDFile polls pidFile until it contains a valid positive integer
// PID, or fails the test after timeout.
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
	t.Fatalf("pollPIDFile(%q) = no valid PID after %v, want a PID", pidFile, timeout)
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
func assertProcessDead(t *testing.T, label string, pid int, timeout time.Duration) {
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
	t.Errorf("assertProcessDead(%s, %d) = alive after %v, want gone", label, pid, timeout)
}

// awaitFile polls until path exists or the timeout expires, reporting
// whether it appeared.
func awaitFile(t *testing.T, path string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// killGroupOnCleanup registers a best-effort force kill of the process
// group led by pid, so a failing assertion cannot strand the subprocess
// or its descendants for the rest of the run.
func killGroupOnCleanup(t *testing.T, pid int) {
	t.Helper()
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	})
}

// TestStartSession_CancelSignalsProcessGroup verifies that cancelling the
// context passed to StartSession, after the session has been established,
// tears the app-server down through its process group: the group receives
// a catchable termination signal rather than os/exec's default force kill
// of the direct child alone, and no descendant survives.
//
// The load-bearing assertion is the descendant's marker. A descendant can
// only be reached through the group, and it can only write the marker if
// the signal was catchable, so the marker fails both when the signal
// never reaches the group and when it arrives as an uncatchable kill.
// Asserting only that the descendant is gone would not distinguish the
// two: the adapter's exit watcher force-kills the group unconditionally
// once cmd.Wait returns, and that alone reaps the descendant either way.
//
// Not run with t.Parallel(): it pins CODEX_API_KEY to empty via t.Setenv
// so the fake app-server's account/read reply always takes the no-login
// branch in authenticateIfNeeded, regardless of the ambient environment,
// and t.Setenv forbids parallel use.
func TestStartSession_CancelSignalsProcessGroup(t *testing.T) {
	t.Setenv("CODEX_API_KEY", "")

	tmpDir := t.TempDir()
	pidFile := filepath.Join(tmpDir, "descendant.pid")
	markerFile := filepath.Join(tmpDir, "descendant.terminated")
	descendant := writeDescendantScript(t, tmpDir, pidFile, markerFile)
	script := writeFakeAppServerScript(t, tmpDir, descendant)

	adapter, err := NewCodexAdapter(map[string]any{})
	if err != nil {
		t.Fatalf("NewCodexAdapter() error = %v, want nil", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	session, err := adapter.StartSession(ctx, domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v, want nil", err)
	}

	directPID, convErr := strconv.Atoi(session.AgentPID)
	if convErr != nil {
		t.Fatalf("session.AgentPID = %q, want a PID: %v", session.AgentPID, convErr)
	}
	killGroupOnCleanup(t, directPID)

	descendantPID := pollPIDFile(t, pidFile, 5*time.Second)

	cancel()

	if !awaitFile(t, markerFile, 5*time.Second) {
		t.Errorf("cancelling the launch context left %q absent after %v, want the descendant to have caught a graceful signal", markerFile, 5*time.Second)
	}
	assertProcessDead(t, "app-server", directPID, 3*time.Second)
	assertProcessDead(t, "descendant", descendantPID, 3*time.Second)
}
