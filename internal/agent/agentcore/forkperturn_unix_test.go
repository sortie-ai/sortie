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
	"github.com/sortie-ai/sortie/internal/domain"
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

// writeEscapedPgidScript creates a script that spawns a long-running
// grandchild through setsid (so it leaves the process group while still
// inheriting the parent's stdout handle), writes the grandchild PID to
// pidFile, emits a JSONL notification line to stdout, then blocks like
// writePgidScript's own leader.
//
// The grandchild writes its own PID, using $$ from inside the process
// setsid already transitioned, and the leader waits on that marker
// before continuing. Waiting on the leader's own $! instead would only
// prove the job was forked, not that its setsid() call had already
// taken effect, so a group signal delivered in that window would still
// reach a grandchild that had not yet escaped the group.
func writeEscapedPgidScript(t *testing.T, dir, pidFile string) string {
	t.Helper()
	content := fmt.Sprintf(
		"setsid sh -c 'echo $$ > %s; sleep 3600' &\n"+
			"while [ ! -s %s ]; do sleep 0.01; done\n"+
			"printf '{\"type\":\"notification\"}\\n'\n"+
			"sleep 3600\n",
		pidFile, pidFile,
	)
	return agenttest.WriteScript(t, dir, "agent-escaped-pgid", content)
}

// writeStderrOnlyDescendantScript creates a script whose background job
// redirects its own stdout away, so it inherits the parent script's stderr
// handle only, and whose parent writes a stderr line of its own, the
// descendant PID, and the notification line, then exits normally instead of
// sleeping. Using agenttest.WriteScript avoids the ETXTBSY race on Linux.
func writeStderrOnlyDescendantScript(t *testing.T, dir, pidFile string) string {
	t.Helper()
	content := fmt.Sprintf(
		"printf '%%s\\n' 'direct child stderr' >&2\n"+
			"sleep 3600 >/dev/null &\n"+
			"CHILD_PID=$!\n"+
			"printf '%%s\\n' \"$CHILD_PID\" > '%s'\n"+
			"printf '{\"type\":\"notification\"}\\n'\n",
		pidFile,
	)
	return agenttest.WriteScript(t, dir, "agent-stderr-only-descendant", content)
}

// pollPgidFileTimeout is the bound every pollPgidFile call in this file
// polls under.
const pollPgidFileTimeout = 5 * time.Second

// pollPgidFile polls pidFile until it contains a valid positive integer
// PID, or fails the test after pollPgidFileTimeout.
func pollPgidFile(t *testing.T, pidFile string) int {
	t.Helper()
	deadline := time.Now().Add(pollPgidFileTimeout)
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
	t.Fatalf("pollPgidFile(%q): no valid PID after %v", pidFile, pollPgidFileTimeout)
	return 0
}

// killEscapedGroupOnCleanup registers a best-effort SIGKILL of the
// process group led by pid, read from pidFile, so a setsid-escaped
// descendant this file's fixtures leave running does not survive past
// the test that started it.
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
	sess := NewForkPerTurnSession(target, noopHooks(), slog.Default(), 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	emit, _ := sinkEvents()
	done := make(chan struct{})
	go func() {
		defer close(done)
		sess.RunTurn(ctx, "p", emit) //nolint:errcheck // testing process group kill
	}()

	grandchildPID := pollPgidFile(t, pidFile)

	cancel()
	<-done

	assertPgidProcessDead(t, grandchildPID, 3*time.Second)
}

// TestForkPerTurnSession_DescendantHoldsStderrOnly verifies that a
// descendant which inherits only the stderr handle, and whose direct parent
// exits normally rather than blocking, no longer wedges RunTurn. Reaping the
// direct child releases the stderr drain on this platform, so the turn
// delivers the direct child's real stderr lines rather than
// procutil.AbandonedMarker, follows the subprocess's actual exit code
// instead of reporting a cancellation, and the surviving descendant is
// still cleaned up.
func TestForkPerTurnSession_DescendantHoldsStderrOnly(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	pidFile := filepath.Join(tmpDir, "descendant.pid")
	script := writeStderrOnlyDescendantScript(t, tmpDir, pidFile)

	var gotStderrLines []string
	hooks := noopHooks()
	hooks.OnFinalize = func(emit func(domain.AgentEvent), lastParsed any, exitCode int, stderrLines []string) (domain.TurnResult, *domain.AgentError) {
		gotStderrLines = stderrLines
		EmitTurnCompleted(emit, "ok", 0, domain.TokenUsage{})
		return domain.TurnResult{ExitReason: domain.EventTurnCompleted}, nil
	}

	target := newTestTarget(tmpDir, script)
	sess := NewForkPerTurnSession(target, hooks, slog.Default(), 0)
	sess.drainGrace = 200 * time.Millisecond

	emit, events := sinkEvents()
	type outcome struct {
		result domain.TurnResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := sess.RunTurn(context.Background(), "p", emit)
		done <- outcome{result, err}
	}()

	descendantPID := pollPgidFile(t, pidFile)

	var got outcome
	select {
	case got = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("RunTurn did not return within 10s")
	}

	if got.err != nil {
		t.Fatalf("RunTurn() error = %v, want nil", got.err)
	}
	if got.result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("TurnResult.ExitReason = %q, want %q", got.result.ExitReason, domain.EventTurnCompleted)
	}
	if !hasEventType(*events, domain.EventTurnCompleted) {
		t.Errorf("EventTurnCompleted not emitted; got %v", *events)
	}

	wantStderrLines := []string{"direct child stderr"}
	if len(gotStderrLines) != len(wantStderrLines) || gotStderrLines[0] != wantStderrLines[0] {
		t.Errorf("OnFinalize stderrLines = %v, want %v", gotStderrLines, wantStderrLines)
	}

	assertPgidProcessDead(t, descendantPID, 3*time.Second)
}

// TestForkPerTurnSession_InGroupDescendantHoldsStdout drives
// writePgidScript's in-group descendant through a cancelled RunTurn and
// asserts property P5 (the descendant is dead once the turn returns)
// and that the group kill releases the stdout reader before
// sess.drainGrace can fire, so no abandonment record is emitted. This is
// the unabandoned half of property P7's comparison against
// TestForkPerTurnSession_EscapedDescendantHoldsStdout: both scripts spawn
// a descendant that inherits the standard-output handle and both turns
// are ended by the same context cancellation, so an identical disposition
// and error between the two is what P7 requires.
func TestForkPerTurnSession_InGroupDescendantHoldsStdout(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	pidFile := filepath.Join(tmpDir, "descendant.pid")
	script := writePgidScript(t, tmpDir, pidFile)

	spy := &agenttest.LogSpy{}
	target := newTestTarget(tmpDir, script)
	sess := NewForkPerTurnSession(target, noopHooks(), slog.New(spy), 0)
	sess.drainGrace = 200 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	emit, events := sinkEvents()
	type outcome struct {
		result domain.TurnResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := sess.RunTurn(ctx, "p", emit)
		done <- outcome{result, err}
	}()

	descendantPID := pollPgidFile(t, pidFile)
	cancel()

	var got outcome
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunTurn did not return within 5s")
	}

	requireAgentError(t, got.err, domain.ErrTurnCancelled)
	if !hasEventType(*events, domain.EventTurnCancelled) {
		t.Errorf("EventTurnCancelled not emitted; got %v", *events)
	}

	assertPgidProcessDead(t, descendantPID, 3*time.Second)

	for _, e := range spy.Entries() {
		if e.Level == slog.LevelWarn && e.Msg == "agent stdout was not fully collected before the turn ended" {
			t.Errorf("unexpected stdout abandonment WARN record emitted; the group kill should have released the reader before the bound fired: %+v", e)
		}
	}
}

// TestForkPerTurnSession_EscapedDescendantHoldsStdout drives
// writeEscapedPgidScript's setsid descendant, which survives the group
// kill and keeps holding the standard-output handle open, through a
// cancelled RunTurn. It asserts property P1 (the turn publishes within
// sess.drainGrace, measured from the reap rather than from the turn's
// start), property P3 (the abandonment record fires exactly once), and,
// together with TestForkPerTurnSession_InGroupDescendantHoldsStdout,
// property P7 (the same disposition and error as the identical
// unabandoned turn).
func TestForkPerTurnSession_EscapedDescendantHoldsStdout(t *testing.T) {
	agenttest.RequireSetsid(t)
	t.Parallel()

	tmpDir := t.TempDir()
	pidFile := filepath.Join(tmpDir, "escaped.pid")
	script := writeEscapedPgidScript(t, tmpDir, pidFile)

	spy := &agenttest.LogSpy{}
	target := newTestTarget(tmpDir, script)
	sess := NewForkPerTurnSession(target, noopHooks(), slog.New(spy), 0)
	// Matches this file's own 200ms seam rather than forkperturn_test.go's
	// 50ms one: the kill only sends the signal before Reaper.Done closes,
	// and the descendant's death and its release of the handle follow
	// asynchronously, so a shorter grace would race the scheduler instead
	// of measuring the release.
	sess.drainGrace = 200 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	emit, events := sinkEvents()
	type outcome struct {
		result domain.TurnResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := sess.RunTurn(ctx, "p", emit)
		done <- outcome{result, err}
	}()

	pollPgidFile(t, pidFile)
	killEscapedGroupOnCleanup(t, pidFile)

	reapStart := time.Now()
	cancel()

	var got outcome
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunTurn did not return within 5s")
	}
	elapsed := time.Since(reapStart)

	if elapsed > 2*time.Second {
		t.Errorf("RunTurn published %v after cancellation, want well under 2s (sess.drainGrace=200ms plus scheduling overhead)", elapsed)
	}

	requireAgentError(t, got.err, domain.ErrTurnCancelled)
	if !hasEventType(*events, domain.EventTurnCancelled) {
		t.Errorf("EventTurnCancelled not emitted; got %v", *events)
	}

	var abandonCount int
	for _, e := range spy.Entries() {
		if e.Level == slog.LevelWarn && e.Msg == "agent stdout was not fully collected before the turn ended" {
			abandonCount++
		}
	}
	if abandonCount != 1 {
		t.Errorf("abandonment WARN record count = %d, want exactly 1", abandonCount)
	}
}
