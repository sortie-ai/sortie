//go:build linux

package codex

import (
	"context"
	"errors"
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
			// wait returns as soon as a trapped signal arrives, while a
			// sleep loop runs the handler only after the current sleep
			// ends. That delay competes with the grace period before the
			// group is killed outright, which is the whole budget this
			// fixture has to write its marker in.
			"sleep 3600 &\n"+
			"wait\n",
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

// killEscapedGroupOnCleanup registers a best-effort SIGKILL of the
// process group led by the PID recorded in pidFile, read lazily at
// cleanup time so a setsid-escaped descendant a fixture starts in the
// background does not need to have already written it when this is
// called. It tolerates a pidFile that never appears.
func killEscapedGroupOnCleanup(t *testing.T, pidFile string) {
	t.Helper()
	t.Cleanup(func() {
		data, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		pid, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
		if convErr != nil || pid <= 0 {
			return
		}
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

// writeFakeAppServerScriptEscapedTurn creates a script that fakes the
// codex app-server handshake exactly as writeFakeAppServerScript does,
// then answers one turn/start call (id 4, the next identifier
// jsonrpc.Conn allocates after the handshake's three calls) and starts a
// setsid grandchild that inherits standard output and exits without
// waiting for it. Property P12 uses it to drive a real turn whose
// runtime exits while an escaped descendant still holds the output
// handle.
func writeFakeAppServerScriptEscapedTurn(t *testing.T, dir, pidFile string) string {
	t.Helper()
	content := fmt.Sprintf(`read -r _init_req
printf '{"id":1,"result":{}}\n'
read -r _initialized_notif
read -r _account_read_req
printf '{"id":2,"result":{}}\n'
read -r _thread_start_req
printf '{"id":3,"result":{"thread":{"id":"fake-thread-1"}}}\n'
printf '{"method":"thread/started","params":{}}\n'
read -r _turn_start_req
printf '{"id":4,"result":{"turn":{"id":"t1"}}}\n'
setsid sh -c 'echo $$ > %s; sleep 3600' 2>/dev/null &
while [ ! -s %s ]; do sleep 0.01; done
exit 0
`, pidFile, pidFile)
	return agenttest.WriteScript(t, dir, "fake-codex-app-server-escaped-turn", content)
}

// TestStartSession_ReleaseEndsTurnWhenEscapedDescendantHoldsOutput
// covers property P12: a session whose runtime exits while an escaped
// descendant holds the output handle publishes the turn's outcome and
// completes StopSession within the injected CodexAdapter.drainGrace
// bound, leaving no goroutine of the session running.
//
// Not run with t.Parallel(): it pins CODEX_API_KEY via t.Setenv, which
// forbids parallel use.
func TestStartSession_ReleaseEndsTurnWhenEscapedDescendantHoldsOutput(t *testing.T) {
	t.Setenv("CODEX_API_KEY", "")
	agenttest.RequireSetsid(t)

	tmpDir := t.TempDir()
	pidFile := filepath.Join(tmpDir, "escaped.pid")
	killEscapedGroupOnCleanup(t, pidFile)
	script := writeFakeAppServerScriptEscapedTurn(t, tmpDir, pidFile)

	adapter := &CodexAdapter{drainGrace: 300 * time.Millisecond}
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v, want nil", err)
	}
	state, ok := session.Internal.(*sessionState)
	if !ok {
		t.Fatalf("session.Internal type = %T, want *sessionState", session.Internal)
	}

	start := time.Now()
	result, runErr := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "work",
		OnEvent: func(domain.AgentEvent) {},
	})
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Errorf("RunTurn() took %v, want well under 2s (bounded by CodexAdapter.drainGrace)", elapsed)
	}

	var agentErr *domain.AgentError
	if !errors.As(runErr, &agentErr) || agentErr.Kind != domain.ErrPortExit {
		t.Fatalf("RunTurn() error = %v, want AgentError{Kind: %q}", runErr, domain.ErrPortExit)
	}
	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}
	// This turn is past its turn/start response, so the connection's end
	// reaches it through the event loop rather than through the call.
	// Whichever of those two arms wins the race reports the abandonment,
	// so asserting the message here is what keeps either of them from
	// silently reverting to its transport text.
	if agentErr.Message != outputAbandonedMessage {
		t.Errorf("AgentError.Message = %q, want %q", agentErr.Message, outputAbandonedMessage)
	}

	stopStart := time.Now()
	if err := adapter.StopSession(context.Background(), session); err != nil {
		t.Errorf("StopSession() = %v, want nil", err)
	}
	if elapsed := time.Since(stopStart); elapsed > 2*time.Second {
		t.Errorf("StopSession() took %v, want well under 2s", elapsed)
	}

	select {
	case <-state.readerDone:
	default:
		t.Error("session leak: the connection's reader goroutine is still running after StopSession returned")
	}
}
