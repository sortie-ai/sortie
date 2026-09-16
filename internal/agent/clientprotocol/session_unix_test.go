//go:build unix

package clientprotocol

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

// mcpHandshakeScript is a fake agent that answers exactly the two
// calls startSession makes before returning: initialize (always id 1,
// since it is the connection's first call) and session/new (always id
// 2). It captures the raw session/new request line to captureFile
// before answering it, so a test can inspect the exact bytes the
// adapter wrote for its own tool-server delivery.
func mcpHandshakeScript(captureFile string) string {
	return `capture='` + captureFile + `'
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*)
      printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentCapabilities":{"mcpCapabilities":{"http":true}}}}'
      ;;
    *'"method":"session/new"'*)
      printf '%s\n' "$line" >> "$capture"
      printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"sessionId":"sess-1"}}'
      ;;
  esac
done
`
}

// TestStartSessionMCPInjectionWire calls agenttest.AssertMCPInjection
// with the wire channel populated from the actual session/new request
// bytes the adapter wrote, and is what gives cmd/sortie's
// TestEveryAgentKindHasMCPInjectionCoverage a call site for this
// kind. It launches a shell-script agent, so it runs only where
// /bin/sh exists.
func TestStartSessionMCPInjectionWire(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mcpConfigPath := writeMCPConfig(t)
	capturePath := filepath.Join(dir, "session_new.jsonl")
	scriptPath := agenttest.WriteScript(t, dir, "agent.sh", mcpHandshakeScript(capturePath))

	params := domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: scriptPath},
		MCPConfigPath: mcpConfigPath,
	}

	session, err := startSession(context.Background(), &ClientProtocolAdapter{}, params)
	if err != nil {
		t.Fatalf("startSession() error = %v", err)
	}
	t.Cleanup(func() {
		if err := stopSession(context.Background(), session); err != nil {
			t.Errorf("stopSession() error = %v", err)
		}
	})

	captured, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read captured session/new request: %v", err)
	}

	declared, ok := registry.Agents.Meta("agent-client-protocol")
	if !ok {
		t.Fatal(`registry.Agents.Meta("agent-client-protocol") ok = false, want true`)
	}

	agenttest.AssertMCPInjection(t, declared.MCPInjection, mcpConfigPath, agenttest.MCPLaunchSurface{
		Wire: []string{strings.TrimSpace(string(captured))},
	})
}

// mcpHandshakeThenGracefulExitScript installs a handler for the
// graceful signal that waits delaySeconds and writes evidencePath
// before exiting, then answers startSession's handshake exactly as
// mcpHandshakeScript does, so a test can tell a catchable signal from
// an uncatchable kill.
//
// The handler is installed before the handshake rather than after it.
// A caller signals only once startSession has returned, which cannot
// happen until the handshake below has been answered, so installing
// first leaves no window in which the default disposition applies and
// the evidence is never written. The idle loop sleeps in short
// intervals because the handler does not run until the command it
// interrupts has finished.
func mcpHandshakeThenGracefulExitScript(evidencePath, delaySeconds string) string {
	return `trap 'sleep ` + delaySeconds + `; touch "` + evidencePath + `"; exit 0' TERM
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*)
      printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}'
      ;;
    *'"method":"session/new"'*)
      printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"sessionId":"sess-1"}}'
      break
      ;;
  esac
done
while :; do sleep 0.05; done
`
}

// TestStartSessionCancelledLaunchContextSignalsGracefully:
// cancelling the context a session was started with, without calling
// stopSession at all, delivers the catchable termination signal to the
// agent rather than an uncatchable kill, and the agent's handler
// evidence shows it. The property fails if either startSession's
// cmd.Cancel or its cmd.WaitDelay is removed: without them,
// exec.CommandContext's default cancellation path SIGKILLs the direct
// child instead, the handler this fixture depends on never runs, and
// the evidence file this test waits for never appears.
func TestStartSessionCancelledLaunchContextSignalsGracefully(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	evidencePath := filepath.Join(dir, "evidence")
	scriptPath := agenttest.WriteScript(t, dir, "agent.sh", mcpHandshakeThenGracefulExitScript(evidencePath, "0.4"))

	ctx, cancel := context.WithCancel(context.Background())
	session, err := startSession(ctx, &ClientProtocolAdapter{}, domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: scriptPath},
	})
	if err != nil {
		t.Fatalf("startSession() error = %v", err)
	}
	t.Cleanup(func() {
		if err := stopSession(context.Background(), session); err != nil {
			t.Errorf("stopSession() error = %v", err)
		}
	})

	cancel()
	waitForFile(t, evidencePath)
}

// stderrThenExitScript is a fake agent that writes marker to stderr
// and exits immediately without reading or answering anything on
// stdin, so startSession's initialize call fails against a closed
// connection rather than a timeout.
func stderrThenExitScript(marker string) string {
	return `printf '%s\n' '` + marker + `' 1>&2
exit 1
`
}

// TestStartSessionEmitsCollectedStderrAtWarnOnFailedInitialize confirms
// that a fake agent which exits before answering initialize still gets
// its collected stderr surfaced at Warn before startSession returns its
// error. The test fails if the EmitWarnLines call following
// doInitialize's failure branch is removed: the returned error would be
// unaffected, but the marker line would never reach the logger.
func TestStartSessionEmitsCollectedStderrAtWarnOnFailedInitialize(t *testing.T) {
	// No t.Parallel(): installs a process-wide slog default.

	const marker = "distinguishing-stderr-line-init-failure"

	dir := t.TempDir()
	scriptPath := agenttest.WriteScript(t, dir, "agent.sh", stderrThenExitScript(marker))

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(orig) })

	_, err := startSession(context.Background(), &ClientProtocolAdapter{}, domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: scriptPath},
	})

	if _, ok := errors.AsType[*domain.AgentError](err); !ok {
		t.Fatalf("startSession() error = %v (%T), want a non-nil *domain.AgentError", err, err)
	}

	output := buf.String()
	if !strings.Contains(output, marker) {
		t.Errorf("startSession() on failed initialize did not log the collected stderr line %q at Warn: %s", marker, output)
	}
	if !strings.Contains(output, "level=WARN") {
		t.Errorf("startSession()'s collected stderr line was logged below Warn: %s", output)
	}
}

// mcpHandshakeWithDetachedChildScript answers the handshake exactly as
// mcpHandshakeScript does, but first backgrounds a helper process
// under setsid: a new session and process group leader, escaping the
// group procutil.SetGroupCancel placed this script's own process in.
// The helper records its own pid to pidFile and idles, so a test can
// wait for it to exist and then check its liveness directly.
func mcpHandshakeWithDetachedChildScript(pidFile string) string {
	return mcpHandshakeWithChildScript("setsid ", pidFile)
}

// mcpHandshakeWithGroupChildScript answers the handshake and leaves one
// idle child inside the process group procutil.SetGroupCancel placed
// this script in, so teardown's group-directed termination reaches it.
func mcpHandshakeWithGroupChildScript(pidFile string) string {
	return mcpHandshakeWithChildScript("", pidFile)
}

// mcpHandshakeWithChildScript builds both of the above. launcher
// prefixes the child's own command and is what decides whether the
// child keeps the script's process group or leaves it.
func mcpHandshakeWithChildScript(launcher, pidFile string) string {
	return launcher + `sh -c 'echo $$ >"` + pidFile + `"; while :; do sleep 0.05; done' </dev/null >/dev/null 2>&1 &
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*)
      printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}'
      ;;
    *'"method":"session/new"'*)
      printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"sessionId":"sess-1"}}'
      break
      ;;
  esac
done
while :; do sleep 0.05; done
`
}

// killLaunchedGroup signals the process group the launched agent leads,
// tolerating a pid that has already gone. A child that stayed in that
// group leads no group of its own, so a kill aimed at the child's own
// pid reaches nothing; killHelperGroup is right only for the detached
// child below, which setsid makes a leader.
func killLaunchedGroup(agentPID string) {
	pid, err := strconv.Atoi(strings.TrimSpace(agentPID))
	if err != nil || pid <= 0 {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

// initThenStderrExitScript answers initialize, writes marker to stderr,
// and exits before answering session/new, so startSession's failure
// arrives from resolveSession rather than from doInitialize.
func initThenStderrExitScript(marker string) string {
	return `while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*)
      printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}'
      printf '%s\n' '` + marker + `' 1>&2
      exit 1
      ;;
  esac
done
`
}

// TestStartSessionEmitsCollectedStderrAtWarnOnFailedResolveSession
// mirrors the initialize-failure case above for the resolve-session
// failure branch. startSession emits collected stderr on two failure
// branches, and a control that reaches only the first lets the
// second's emission be deleted without a test going red.
func TestStartSessionEmitsCollectedStderrAtWarnOnFailedResolveSession(t *testing.T) {
	// No t.Parallel(): installs a process-wide slog default.

	const marker = "distinguishing-stderr-line-resolve-failure"

	dir := t.TempDir()
	scriptPath := agenttest.WriteScript(t, dir, "agent.sh", initThenStderrExitScript(marker))

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(orig) })

	_, err := startSession(context.Background(), &ClientProtocolAdapter{}, domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: scriptPath},
	})

	if _, ok := errors.AsType[*domain.AgentError](err); !ok {
		t.Fatalf("startSession() error = %v (%T), want a non-nil *domain.AgentError", err, err)
	}

	output := buf.String()
	if !strings.Contains(output, marker) {
		t.Errorf("startSession() on failed resolveSession did not log the collected stderr line %q at Warn: %s", marker, output)
	}
	if !strings.Contains(output, "level=WARN") {
		t.Errorf("startSession()'s collected stderr line was logged below Warn: %s", output)
	}
}

// TestStopSessionReachesGroupChild is the other half of the property
// the escaped-member test below evidences. Teardown does reach a
// descendant that stays in the group it was launched into, so a
// survivor outside that group means the group was escaped rather than
// that teardown reaches nothing at all. Removing procutil.SetGroupCancel
// from startSession fails this test and not that one.
func TestStopSessionReachesGroupChild(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	pidPath := filepath.Join(dir, "group-child.pid")
	scriptPath := agenttest.WriteScript(t, dir, "agent.sh", mcpHandshakeWithGroupChildScript(pidPath))

	session, err := startSession(context.Background(), &ClientProtocolAdapter{}, domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: scriptPath},
	})
	if err != nil {
		t.Fatalf("startSession() error = %v", err)
	}

	childPID := waitForPIDFile(t, pidPath, awaitTimeout)
	t.Cleanup(func() { killLaunchedGroup(session.AgentPID) })

	if err := stopSession(context.Background(), session); err != nil {
		t.Fatalf("stopSession() error = %v", err)
	}

	assertProcessGone(t, childPID, awaitTimeout)
}

// TestStopSessionDoesNotReachEscapedProcessGroupMember confirms that a
// descendant which detaches into its own process group before the
// parent exits survives stopSession's group-directed termination, so a
// leaked survivor is demonstrably detectable by a direct
// process-liveness check rather than merely assumed. This is not a
// defect in stopSession; procutil.SetGroupCancel
// and kill_process_group can only ever reach the group they targeted.
func TestStopSessionDoesNotReachEscapedProcessGroupMember(t *testing.T) {
	t.Parallel()
	agenttest.RequireSetsid(t)

	dir := t.TempDir()
	pidPath := filepath.Join(dir, "escaped.pid")
	scriptPath := agenttest.WriteScript(t, dir, "agent.sh", mcpHandshakeWithDetachedChildScript(pidPath))

	session, err := startSession(context.Background(), &ClientProtocolAdapter{}, domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: scriptPath},
	})
	if err != nil {
		t.Fatalf("startSession() error = %v", err)
	}

	escapedPID := waitForPIDFile(t, pidPath, awaitTimeout)
	t.Cleanup(func() { killHelperGroup(pidPath) })

	if err := stopSession(context.Background(), session); err != nil {
		t.Fatalf("stopSession() error = %v", err)
	}

	if err := syscall.Kill(escapedPID, 0); err != nil {
		t.Errorf("escaped process group member (pid %d) liveness probe after stopSession() = %v, want still running", escapedPID, err)
	}
}

// stderrThenExitWithDetachedHolderScript writes marker to standard
// error, backgrounds a setsid descendant that inherits this script's
// own standard-output handle (a plain shell background job keeps the
// parent's file descriptors unless it redirects them, and setsid is
// what lets the descendant survive teardown's group-directed kill),
// then exits before ever reading or answering the initialize call. The
// runtime is gone, but the escaped descendant keeps the output pipe
// from reaching end of file, so a handshake call is still in flight
// when the release gives up on it.
func stderrThenExitWithDetachedHolderScript(marker, pidFile string) string {
	return `setsid sh -c 'echo $$ > ` + pidFile + `; sleep 3600' 2>/dev/null &
while [ ! -s ` + pidFile + ` ]; do sleep 0.01; done
printf '%s\n' '` + marker + `' 1>&2
exit 7
`
}

// TestStartSessionHandshakeAbandonedByEscapedDescendantFailsWithPortExit
// asserts that a runtime that exits during the handshake while an
// escaped descendant still holds the output handle fails StartSession
// at the reap plus the injected grace with domain.ErrPortExit, and the
// runtime's own stderr line still reaches the operator.
//
// No t.Parallel(): installs a process-wide slog default.
func TestStartSessionHandshakeAbandonedByEscapedDescendantFailsWithPortExit(t *testing.T) {
	agenttest.RequireSetsid(t)

	const marker = "distinguishing-stderr-line-handshake-abandoned"
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "escaped.pid")
	t.Cleanup(func() { killHelperGroup(pidPath) })
	scriptPath := agenttest.WriteScript(t, dir, "agent.sh", stderrThenExitWithDetachedHolderScript(marker, pidPath))

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(orig) })

	const grace = 200 * time.Millisecond
	adapter := &ClientProtocolAdapter{drainGrace: grace}

	start := time.Now()
	_, err := startSession(context.Background(), adapter, domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: scriptPath},
	})
	elapsed := time.Since(start)

	agentErr, ok := errors.AsType[*domain.AgentError](err)
	if !ok {
		t.Fatalf("startSession() error = %v (%T), want a non-nil *domain.AgentError", err, err)
	}
	if agentErr.Kind != domain.ErrPortExit {
		t.Errorf("startSession() error kind = %q, want %q", agentErr.Kind, domain.ErrPortExit)
	}
	const wantMessage = "agent connection ended before responding"
	if agentErr.Message != wantMessage {
		t.Errorf("startSession() error message = %q, want %q", agentErr.Message, wantMessage)
	}
	if elapsed > 10*time.Second {
		t.Errorf("startSession() took %v, want well under the 30s handshake timeout it would have hit without the release", elapsed)
	}

	output := buf.String()
	if !hasWarnStderrLineRecord(output, marker) {
		t.Errorf("startSession() output = %s, want one record carrying level=WARN, msg=\"agent stderr\", and line=%s together (the collector also logs this same line at Debug, which must not satisfy this check on its own)", output, marker)
	}
}

// hasWarnStderrLineRecord reports whether output contains one log
// record, on a single line, that carries level=WARN, msg="agent
// stderr", and line=marker together: the three fields a
// procutil.EmitWarnLines call produces for a collected stderr line.
// The collector's own Debug-level record for the same line carries
// the same message and line fields but a different level, so the
// three checks must hold on one record rather than anywhere in the
// buffer.
func hasWarnStderrLineRecord(output, marker string) bool {
	for line := range strings.SplitSeq(output, "\n") {
		if strings.Contains(line, "level=WARN") &&
			strings.Contains(line, `msg="agent stderr"`) &&
			strings.Contains(line, "line="+marker) {
			return true
		}
	}
	return false
}

// handshakeThenExitWithDetachedHolderScript answers the two calls
// startSession makes before returning, then spawns a setsid
// descendant that inherits this script's own standard-output handle
// and exits without waiting for it, so a real subprocess reproduces
// "the runtime exits, its descendant survives holding the write end"
// for a session that has already started.
func handshakeThenExitWithDetachedHolderScript(pidFile string) string {
	return `while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*)
      printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}'
      ;;
    *'"method":"session/new"'*)
      printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"sessionId":"sess-1"}}'
      break
      ;;
  esac
done
setsid sh -c 'echo $$ > ` + pidFile + `; sleep 3600' 2>/dev/null &
while [ ! -s ` + pidFile + ` ]; do sleep 0.01; done
exit 0
`
}

// TestStopSessionReturnsBoundedAfterReleaseAbandonsOnARealSubprocess
// asserts that, with the runtime gone and an escaped descendant still
// holding the standard-output handle, once the release has abandoned,
// StopSession still returns inside its pinned teardown ceiling against
// a real subprocess. On Linux the release's own CloseStdout already
// unparks the connection's reader by the time this runs, so this alone
// does not exercise the post-abandonment stop arm runPump's select
// falls back to when a reader stays genuinely parked; that arm is
// proven separately, by an untagged test whose pipes are never wired
// to the connection under test.
func TestStopSessionReturnsBoundedAfterReleaseAbandonsOnARealSubprocess(t *testing.T) {
	t.Parallel()
	agenttest.RequireSetsid(t)

	dir := t.TempDir()
	pidPath := filepath.Join(dir, "escaped.pid")
	t.Cleanup(func() { killHelperGroup(pidPath) })
	scriptPath := agenttest.WriteScript(t, dir, "agent.sh", handshakeThenExitWithDetachedHolderScript(pidPath))

	const grace = 200 * time.Millisecond
	adapter := &ClientProtocolAdapter{drainGrace: grace}
	session, err := startSession(context.Background(), adapter, domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: scriptPath},
	})
	if err != nil {
		t.Fatalf("startSession() error = %v, want nil", err)
	}
	state, ok := session.Internal.(*sessionState)
	if !ok {
		t.Fatalf("session.Internal type = %T, want *sessionState", session.Internal)
	}

	select {
	case <-state.release.Abandoned():
	case <-time.After(5 * time.Second):
		t.Fatal("the release never abandoned the parked reader")
	}

	start := time.Now()
	if err := stopSession(context.Background(), session); err != nil {
		t.Errorf("stopSession() error = %v, want nil", err)
	}
	elapsed := time.Since(start)

	ceiling := procutil.DefaultStopGrace + 3*procutil.DefaultDrainGrace + teardownReturnOverhead
	if elapsed >= ceiling {
		t.Errorf("stopSession() took %v, want under %v (the pinned teardown ceiling)", elapsed, ceiling)
	}
}

// promptThenExitWithDetachedHolderScript answers the two calls
// startSession makes, then, once the first session/prompt arrives,
// spawns a setsid descendant that inherits this script's own
// standard-output handle and exits without answering, so a turn is in
// flight when the runtime is gone and its descendant still holds the
// write end.
func promptThenExitWithDetachedHolderScript(pidFile string) string {
	return `while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*)
      printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}'
      ;;
    *'"method":"session/new"'*)
      printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"sessionId":"sess-1"}}'
      ;;
    *'"method":"session/prompt"'*)
      break
      ;;
  esac
done
setsid sh -c 'echo $$ > ` + pidFile + `; sleep 3600' 2>/dev/null &
while [ ! -s ` + pidFile + ` ]; do sleep 0.01; done
exit 0
`
}

// TestRunTurnEndsBoundedWhenRuntimeExitsWithEscapedDescendantHoldingOutput
// asserts that, through a real session, a turn whose runtime exits while
// an escaped descendant still holds the standard-output handle ends
// within the injected grace with domain.ErrPortExit and the release's
// message rather than reaching the orchestrator's stall timeout, and
// that StopSession still returns inside its pinned ceiling afterward.
func TestRunTurnEndsBoundedWhenRuntimeExitsWithEscapedDescendantHoldingOutput(t *testing.T) {
	t.Parallel()
	agenttest.RequireSetsid(t)

	dir := t.TempDir()
	pidPath := filepath.Join(dir, "escaped.pid")
	t.Cleanup(func() { killHelperGroup(pidPath) })
	scriptPath := agenttest.WriteScript(t, dir, "agent.sh", promptThenExitWithDetachedHolderScript(pidPath))

	const grace = 200 * time.Millisecond
	adapter := &ClientProtocolAdapter{drainGrace: grace}
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: scriptPath},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v, want nil", err)
	}

	start := time.Now()
	result, runErr := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "work",
		OnEvent: func(domain.AgentEvent) {},
	})
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Errorf("RunTurn() took %v, want well under 2s (bounded by the injected drain grace %v)", elapsed, grace)
	}
	agentErr, ok := errors.AsType[*domain.AgentError](runErr)
	if !ok {
		t.Fatalf("RunTurn() error = %v (%T), want a non-nil *domain.AgentError", runErr, runErr)
	}
	if agentErr.Kind != domain.ErrPortExit {
		t.Errorf("RunTurn() error kind = %q, want %q", agentErr.Kind, domain.ErrPortExit)
	}
	if agentErr.Message != procutil.OutputAbandonedMessage {
		t.Errorf("RunTurn() error message = %q, want %q", agentErr.Message, procutil.OutputAbandonedMessage)
	}
	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("RunTurn() ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}

	stopStart := time.Now()
	if err := adapter.StopSession(context.Background(), session); err != nil {
		t.Errorf("StopSession() error = %v, want nil", err)
	}
	ceiling := procutil.DefaultStopGrace + 3*procutil.DefaultDrainGrace + teardownReturnOverhead
	if stopElapsed := time.Since(stopStart); stopElapsed >= ceiling {
		t.Errorf("StopSession() took %v, want under %v (the pinned teardown ceiling)", stopElapsed, ceiling)
	}
}

// sshStandInEnvPath returns the PATH value the ssh stand-in's dropped-
// environment child should receive: a directory holding only a
// symlink to sh, and, when includeDD is true, a symlink to dd as well.
// Building an isolated directory rather than reusing sh's own
// directory matters because a real sh and a real dd usually share one
// directory (/usr/bin, /bin), so reusing it for the no-dd case would
// resolve dd anyway.
func sshStandInEnvPath(t *testing.T, includeDD bool) string {
	t.Helper()

	shPath, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("sh not found on PATH: %v", err)
	}

	dir := t.TempDir()
	if err := os.Symlink(shPath, filepath.Join(dir, "sh")); err != nil {
		t.Fatalf("Symlink(sh): %v", err)
	}

	if includeDD {
		ddPath, err := exec.LookPath("dd")
		if err != nil {
			t.Skipf("dd not found on PATH: %v", err)
		}
		if err := os.Symlink(ddPath, filepath.Join(dir, "dd")); err != nil {
			t.Fatalf("Symlink(dd): %v", err)
		}
	}
	return dir
}

// TestStartSessionSSH_CarriesEnvironmentVariable drives a remote
// session through the stand-in ssh runtime and asserts that the fake
// agent observes a carried variable's value, delivered only through
// the SSH session's standard input rather than through the stand-in's
// own inherited environment, since the stand-in drops its environment
// before running the remote command.
func TestStartSessionSSH_CarriesEnvironmentVariable(t *testing.T) {
	// Not parallel: sets PATH and the carried variable via t.Setenv.
	dir := t.TempDir()
	sshDir := t.TempDir()

	agenttest.FakeRuntime(t, sshDir, "ssh", scenarioSSHStandIn, sshStandInParams{PATH: sshStandInEnvPath(t, true)})
	t.Setenv("PATH", sshDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	const carriedName = "CLIENTPROTOCOL_TEST_CARRY"
	const carriedValue = "carried-value-clientprotocol"
	t.Setenv(carriedName, carriedValue)

	capturePath := filepath.Join(dir, "captured.txt")
	agentPath := agenttest.FakeRuntime(t, dir, "agent", scenarioProtocolAgent, protocolAgentParams{
		Handshake:      true,
		EnvCaptureName: carriedName,
		EnvCapturePath: capturePath,
	})

	session, err := startSession(context.Background(), &ClientProtocolAdapter{}, domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: agentPath},
		SSHHost:       "user@stand-in-host",
		SSHEnvNames:   []string{carriedName},
	})
	if err != nil {
		t.Fatalf("startSession() error = %v", err)
	}
	t.Cleanup(func() {
		if err := stopSession(context.Background(), session); err != nil {
			t.Errorf("stopSession() error = %v", err)
		}
	})

	got, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("ReadFile(captured.txt): %v", err)
	}
	if string(got) != carriedValue {
		t.Errorf("remote agent observed %q, want %q", string(got), carriedValue)
	}
}

// TestStartSessionSSH_NoDDEndsAsPortExitNotAgentNotFound asserts that
// a remote host without dd fails the handshake as port_exit /
// "agent connection ended before responding", never the agent-not-
// found category, and that the guard's stderr line reaches a WARN
// "agent stderr" record.
func TestStartSessionSSH_NoDDEndsAsPortExitNotAgentNotFound(t *testing.T) {
	// Not parallel: installs a process-wide slog default and sets PATH
	// and the carried variable via t.Setenv.
	const ddMissingMessage = "sortie: dd is required on the remote host to receive environment variables"

	dir := t.TempDir()
	sshDir := t.TempDir()

	agenttest.FakeRuntime(t, sshDir, "ssh", scenarioSSHStandIn, sshStandInParams{PATH: sshStandInEnvPath(t, false)})
	t.Setenv("PATH", sshDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	const carriedName = "CLIENTPROTOCOL_TEST_CARRY_NODD"
	t.Setenv(carriedName, "some-value")

	agentPath := agenttest.FakeRuntime(t, dir, "agent", scenarioProtocolAgent, protocolAgentParams{Handshake: true})

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(orig) })

	_, err := startSession(context.Background(), &ClientProtocolAdapter{}, domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: agentPath},
		SSHHost:       "user@stand-in-host",
		SSHEnvNames:   []string{carriedName},
	})

	agentErr, ok := errors.AsType[*domain.AgentError](err)
	if !ok {
		t.Fatalf("startSession() error = %v (%T), want a non-nil *domain.AgentError", err, err)
	}
	if agentErr.Kind != domain.ErrPortExit {
		t.Errorf("startSession() error kind = %q, want %q (never the agent-not-found category)", agentErr.Kind, domain.ErrPortExit)
	}
	const wantMessage = "agent connection ended before responding"
	if agentErr.Message != wantMessage {
		t.Errorf("startSession() error message = %q, want %q", agentErr.Message, wantMessage)
	}

	output := buf.String()
	wantLineAttr := fmt.Sprintf("line=%q", ddMissingMessage)
	found := false
	for line := range strings.SplitSeq(output, "\n") {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, `msg="agent stderr"`) && strings.Contains(line, wantLineAttr) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("startSession() output = %s, want a WARN agent stderr record carrying the guard's message %q", output, ddMissingMessage)
	}
}

// captureCwdAndFirstLineThenHandshakeScript is [mcpHandshakeScript]
// extended to capture, ahead of the handshake, the process's working
// directory and the raw bytes of the very first line its standard
// input carries. A remote launch's preamble, were one ever built for
// this local invocation, would land ahead of the initialize request
// on that same first line, since [sshutil.SSHLaunch.PrefixStdin] sends
// its whole preamble on the wrapped writer's first Write with no
// trailing newline of its own; a remote launch's own branch also
// never sets cmd.Dir, so a cwd inherited from the test binary rather
// than the configured workspace is the other observable sign the
// wrong branch ran.
func captureCwdAndFirstLineThenHandshakeScript(cwdPath, firstLinePath string) string {
	return `pwd > '` + cwdPath + `'
first='` + firstLinePath + `'
capture_first=1
while IFS= read -r line; do
  if [ "$capture_first" = "1" ]; then
    printf '%s' "$line" > "$first"
    capture_first=0
  fi
  case "$line" in
    *'"method":"initialize"'*)
      printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}'
      ;;
    *'"method":"session/new"'*)
      printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"sessionId":"sess-1"}}'
      ;;
  esac
done
`
}

// TestStartSessionLocalLaunchIgnoresSSHEnvNames asserts that a local
// launch (no SSHHost) completes the same way whether or not
// StartSessionParams.SSHEnvNames names a set variable: the runtime
// runs in the configured workspace directory and the very first bytes
// its standard input carries are the initialize request alone, in
// both cases. This reddens if startSession's local branch starts
// treating a non-empty SSHEnvNames as a signal to take the remote
// path, which skips setting cmd.Dir to the workspace and would
// prepend an unwanted preamble ahead of the initialize request on the
// wire.
func TestStartSessionLocalLaunchIgnoresSSHEnvNames(t *testing.T) {
	// Not parallel: sets the carried variable via t.Setenv.
	const varName = "CLIENTPROTOCOL_TEST_LOCAL_INVARIANCE"
	t.Setenv(varName, "should-never-reach-a-local-launch")

	for _, tc := range []struct {
		name        string
		sshEnvNames []string
	}{
		{"SSHEnvNames absent", nil},
		{"SSHEnvNames naming a set variable", []string{varName}},
	} {
		dir := t.TempDir()
		cwdPath := filepath.Join(dir, "cwd.txt")
		firstLinePath := filepath.Join(dir, "first_line.txt")
		scriptPath := agenttest.WriteScript(t, dir, "agent.sh", captureCwdAndFirstLineThenHandshakeScript(cwdPath, firstLinePath))

		workspacePath := t.TempDir()
		session, err := startSession(context.Background(), &ClientProtocolAdapter{}, domain.StartSessionParams{
			WorkspacePath: workspacePath,
			AgentConfig:   domain.AgentConfig{Command: scriptPath},
			SSHEnvNames:   tc.sshEnvNames,
		})
		if err != nil {
			t.Fatalf("%s: startSession() error = %v, want nil", tc.name, err)
		}
		if err := stopSession(context.Background(), session); err != nil {
			t.Errorf("%s: stopSession() error = %v", tc.name, err)
		}

		firstLine, err := os.ReadFile(firstLinePath)
		if err != nil {
			t.Fatalf("%s: ReadFile(first_line.txt): %v", tc.name, err)
		}
		if !strings.HasPrefix(string(firstLine), "{") {
			t.Errorf("%s: first stdin line = %q, want it to start with the initialize request's own %q", tc.name, firstLine, "{")
		}

		wantCwd, err := filepath.EvalSymlinks(workspacePath)
		if err != nil {
			t.Fatalf("%s: EvalSymlinks(workspacePath): %v", tc.name, err)
		}
		gotCwdRaw, err := os.ReadFile(cwdPath)
		if err != nil {
			t.Fatalf("%s: ReadFile(cwd.txt): %v", tc.name, err)
		}
		gotCwd, err := filepath.EvalSymlinks(strings.TrimSpace(string(gotCwdRaw)))
		if err != nil {
			t.Fatalf("%s: EvalSymlinks(captured cwd): %v", tc.name, err)
		}
		if gotCwd != wantCwd {
			t.Errorf("%s: runtime cwd = %q, want the configured workspace %q", tc.name, gotCwd, wantCwd)
		}
	}
}
