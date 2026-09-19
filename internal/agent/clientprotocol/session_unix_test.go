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

// mcpHandshakeThenGracefulExitScript installs a TERM handler that waits
// delaySeconds and writes evidencePath, then answers the handshake. The handler
// is installed before the handshake so there is no window in which the default
// disposition applies: a caller signals only after startSession has returned,
// which cannot happen until the handshake is answered.
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

// Cancelling the launch context signals the agent gracefully rather than with
// an uncatchable kill. Without startSession's cmd.Cancel and cmd.WaitDelay,
// exec.CommandContext SIGKILLs the child, the handler never runs, and the
// evidence file never appears.
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

// stderrThenExitScript writes marker to stderr and exits without answering
// stdin, so initialize fails against a closed connection rather than a timeout.
func stderrThenExitScript(marker string) string {
	return `printf '%s\n' '` + marker + `' 1>&2
exit 1
`
}

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

// mcpHandshakeWithDetachedChildScript backgrounds a helper under setsid, a new
// session and group leader that escapes the group SetGroupCancel placed this
// script in.
func mcpHandshakeWithDetachedChildScript(pidFile string) string {
	return mcpHandshakeWithChildScript("setsid ", pidFile)
}

// mcpHandshakeWithGroupChildScript leaves one idle child inside the script's
// process group, so teardown's group-directed termination reaches it.
func mcpHandshakeWithGroupChildScript(pidFile string) string {
	return mcpHandshakeWithChildScript("", pidFile)
}

// mcpHandshakeWithChildScript builds both of the above; launcher decides whether
// the child keeps the script's process group or leaves it.
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

// killLaunchedGroup signals the process group the launched agent leads. A child
// that stayed in that group leads no group of its own, so killHelperGroup is
// right only for the detached child, which setsid makes a leader.
func killLaunchedGroup(agentPID string) {
	pid, err := strconv.Atoi(strings.TrimSpace(agentPID))
	if err != nil || pid <= 0 {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

// initThenStderrExitScript answers initialize, writes marker to stderr, and
// exits before session/new, so the failure arrives from resolveSession rather
// than doInitialize.
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

// Teardown reaches a descendant that stays in the group it was launched into,
// so a survivor outside that group means the group was escaped rather than that
// teardown reaches nothing. Removing SetGroupCancel fails this test, not the
// escaped-member one.
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

// A descendant that detaches into its own process group survives the
// group-directed termination. This is not a defect: kill_process_group can only
// reach the group it targeted.
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

// stderrThenExitWithDetachedHolderScript backgrounds a setsid descendant that
// inherits this script's stdout handle, then exits before answering initialize.
// The runtime is gone, but the escaped descendant keeps the output pipe from
// reaching EOF, so a handshake call is still in flight when the release gives up.
func stderrThenExitWithDetachedHolderScript(marker, pidFile string) string {
	return `setsid sh -c 'echo $$ > ` + pidFile + `; sleep 3600' 2>/dev/null &
while [ ! -s ` + pidFile + ` ]; do sleep 0.01; done
printf '%s\n' '` + marker + `' 1>&2
exit 7
`
}

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

// hasWarnStderrLineRecord reports whether one log line carries level=WARN,
// msg="agent stderr", and line=marker together. The collector logs the same
// line at Debug, so the three checks must hold on one record, not anywhere.
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

// handshakeThenExitWithDetachedHolderScript answers the two startup calls, then
// spawns a setsid descendant that inherits this script's stdout handle and
// exits, so the runtime is gone while its descendant still holds the write end.
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

// On Linux the release's CloseStdout already unparks the reader by the time
// this runs, so this does not exercise the post-abandonment stop arm runPump
// falls back to when a reader stays genuinely parked; that arm is proven
// separately by an untagged test whose pipes are never wired to the connection.
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

// promptThenExitWithDetachedHolderScript answers the two startup calls, then on
// the first session/prompt spawns a setsid descendant holding this script's
// stdout handle and exits, so a turn is in flight when the runtime is gone.
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

// sshStandInEnvPath returns a PATH directory holding a symlink to sh, and to dd
// when includeDD is set. An isolated directory matters because a real sh and dd
// usually share one directory, so reusing it for the no-dd case would resolve
// dd anyway.
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

// captureCwdAndFirstLineThenHandshakeScript captures, ahead of the handshake,
// the working directory and the raw bytes of the first stdin line. A remote
// launch would land its preamble ahead of initialize on that first line and
// would never set cmd.Dir, so both are observable signs the wrong branch ran.
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
