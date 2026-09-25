//go:build unix

package agentcore

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/domain"
)

// ddMissingMessage mirrors sshutil's own guard message: the only line
// the remote guard writes to standard error when the remote host has
// no dd on PATH.
const ddMissingMessage = "sortie: dd is required on the remote host to receive environment variables"

// sshStandInEnvPath returns the PATH value a stand-in ssh script's
// dropped-environment child should receive: a directory holding only a
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

// writeSSHStandIn builds a fake "ssh" executable in dir that drops its
// own environment and runs its final argument, the remote command
// BuildSSHLaunch produced, through sh -c with the constrained PATH
// sshStandInEnvPath returns and its own standard input inherited. It
// ignores every option ahead of that final argument, which is how a
// real ssh client is invoked: destination options first, the remote
// command last.
func writeSSHStandIn(t *testing.T, dir string, includeDD bool) string {
	t.Helper()
	envPath := sshStandInEnvPath(t, includeDD)
	content := `last=""
for a in "$@"; do last="$a"; done
exec env -i 'PATH=` + envPath + `' sh -c "$last"
`
	return agenttest.WriteScript(t, dir, "ssh", content)
}

// TestForkPerTurnSession_SSH_CarriesEnvironmentVariable drives a
// remote turn through a stand-in ssh on PATH and asserts that the fake
// remote agent observes a carried variable's value, delivered only
// through the SSH session's standard input rather than through the
// stand-in's own inherited environment, since the stand-in drops its
// environment before running the remote command.
func TestForkPerTurnSession_SSH_CarriesEnvironmentVariable(t *testing.T) {
	// Not parallel: sets PATH and the carried variable via t.Setenv.
	tmpDir := t.TempDir()
	sshDir := t.TempDir()
	agentDir := t.TempDir()

	sshPath := writeSSHStandIn(t, sshDir, true)
	t.Setenv("PATH", sshDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	const carriedName = "SORTIE_FORKPERTURN_TEST_CARRY"
	const carriedValue = "carried-value-forkperturn"
	t.Setenv(carriedName, carriedValue)

	capturePath := filepath.Join(tmpDir, "captured.txt")
	agentScript := agenttest.WriteScript(t, agentDir, "agent.sh",
		"printf '%s' \"$"+carriedName+"\" > '"+capturePath+"'\n")

	target := &LaunchTarget{
		Command:       sshPath,
		SSHHost:       "user@stand-in-host",
		WorkspacePath: tmpDir,
		RemoteCommand: agentScript,
		SSHEnvNames:   []string{carriedName},
	}

	sess := NewForkPerTurnSession(target, noopHooks(), slog.Default(), 0)

	emit, events := sinkEvents()
	result, err := sess.RunTurn(context.Background(), "p", emit)
	if err != nil {
		t.Fatalf("RunTurn() error = %v, want nil", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("RunTurn() ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}
	if !hasEventType(*events, domain.EventTurnCompleted) {
		t.Errorf("EventTurnCompleted not emitted; got %v", *events)
	}

	got, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("ReadFile(captured.txt): %v", err)
	}
	if string(got) != carriedValue {
		t.Errorf("remote agent observed %q, want %q", string(got), carriedValue)
	}
}

// TestForkPerTurnSession_SSH_NoCarriedNameLeavesLaunchUnchanged asserts
// that a remote turn with no carried name and no setting still
// completes normally with no environment-carrying preamble involved:
// the fake remote agent needs no dd, only sh.
func TestForkPerTurnSession_SSH_NoCarriedNameLeavesLaunchUnchanged(t *testing.T) {
	// Not parallel: sets PATH via t.Setenv.
	tmpDir := t.TempDir()
	sshDir := t.TempDir()
	agentDir := t.TempDir()

	sshPath := writeSSHStandIn(t, sshDir, false)
	t.Setenv("PATH", sshDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	agentScript := agenttest.WriteScript(t, agentDir, "agent.sh", "exit 0\n")

	target := &LaunchTarget{
		Command:       sshPath,
		SSHHost:       "user@stand-in-host",
		WorkspacePath: tmpDir,
		RemoteCommand: agentScript,
	}

	sess := NewForkPerTurnSession(target, noopHooks(), slog.Default(), 0)

	emit, events := sinkEvents()
	result, err := sess.RunTurn(context.Background(), "p", emit)
	if err != nil {
		t.Fatalf("RunTurn() error = %v, want nil (no carried name means no import step, so a PATH with no dd is irrelevant)", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("RunTurn() ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}
	if !hasEventType(*events, domain.EventTurnCompleted) {
		t.Errorf("EventTurnCompleted not emitted; got %v", *events)
	}
}

// TestForkPerTurnSession_SSH_NoDDEndsAsPortExitNotAgentNotFound
// asserts that a remote host without dd fails the turn under the
// category ForkPerTurnSession selects for a non-zero, non-127 exit
// status, never the agent-not-found category, and that the guard's
// stderr line reaches a WARN "agent stderr" record.
func TestForkPerTurnSession_SSH_NoDDEndsAsPortExitNotAgentNotFound(t *testing.T) {
	// Not parallel: sets PATH and the carried variable via t.Setenv.
	tmpDir := t.TempDir()
	sshDir := t.TempDir()
	agentDir := t.TempDir()

	sshPath := writeSSHStandIn(t, sshDir, false)
	t.Setenv("PATH", sshDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	const carriedName = "SORTIE_FORKPERTURN_TEST_CARRY_NODD"
	t.Setenv(carriedName, "some-value")

	agentScript := agenttest.WriteScript(t, agentDir, "agent.sh", "echo should-not-run\n")

	target := &LaunchTarget{
		Command:       sshPath,
		SSHHost:       "user@stand-in-host",
		WorkspacePath: tmpDir,
		RemoteCommand: agentScript,
		SSHEnvNames:   []string{carriedName},
	}

	hooks := noopHooks()
	hooks.OnFinalize = func(emit func(domain.AgentEvent), _ any, exitCode int, _ []string, _ *domain.AgentError) (domain.TurnResult, *domain.AgentError) {
		return FinalizeTurn(emit, slog.Default(), TurnEvidence{ExitObserved: true, ExitCode: exitCode}, TurnMeta{})
	}
	spy := &agenttest.LogSpy{}
	sess := NewForkPerTurnSession(target, hooks, slog.New(spy), 0)

	emit, events := sinkEvents()
	_, err := sess.RunTurn(context.Background(), "p", emit)

	requireAgentError(t, err, domain.ErrPortExit)
	if !hasEventType(*events, domain.EventTurnFailed) {
		t.Errorf("EventTurnFailed not emitted; got %v", *events)
	}

	var agentErr *domain.AgentError
	if errors.As(err, &agentErr) && agentErr.Message != "exit code 1" {
		t.Errorf("AgentError.Message = %q, want %q (never the agent-not-found category, which exit code 127 alone selects)", agentErr.Message, "exit code 1")
	}

	lines := agenttest.RequireWarnLines(t, spy, "SSH_NoDD")
	if !slices.Contains(lines, ddMissingMessage) {
		t.Errorf("WARN agent stderr lines = %v, want one equal to the guard's message %q", lines, ddMissingMessage)
	}
}

// TestForkPerTurnSession_SSH_RemoteExitTwoYieldsEarlyExitReport pins
// that a remote exit relayed by ssh that is not OpenSSH's own
// connection-failure status yields the early-exit report, not
// agentcore.ConnectionFailedError, and the report's chain carries no
// sshutil.ErrConnectionFailed.
func TestForkPerTurnSession_SSH_RemoteExitTwoYieldsEarlyExitReport(t *testing.T) {
	t.Parallel()

	target := &LaunchTarget{
		Command:       agenttest.WriteScript(t, t.TempDir(), "ssh", "exit 2\n"),
		SSHHost:       "user@stand-in-host",
		WorkspacePath: t.TempDir(),
		RemoteCommand: "irrelevant-remote-command",
	}
	sess := NewForkPerTurnSession(target, hooksWithEarlyExitDisposition(), slog.Default(), 0)

	emit, _ := sinkEvents()
	_, err := sess.RunTurn(context.Background(), "p", emit)

	if errors.Is(err, sshutil.ErrConnectionFailed) {
		t.Fatalf("RunTurn() error = %v, wraps sshutil.ErrConnectionFailed, want the early-exit report instead", err)
	}
	requireAgentError(t, err, domain.ErrPortExit)
	var agentErr *domain.AgentError
	errors.As(err, &agentErr)
	var earlyExitErr *EarlyExitError
	if !errors.As(agentErr.Err, &earlyExitErr) {
		t.Fatalf("AgentError.Err = %v, want an *EarlyExitError", agentErr.Err)
	}
	if earlyExitErr.Status() != "exit status 2" {
		t.Errorf("EarlyExitError.Status() = %q, want %q", earlyExitErr.Status(), "exit status 2")
	}
}

// An ssh exit of 255 is a connection failure only when the runtime
// produced no output first; after output, the runtime connected and ran.
func TestForkPerTurnSession_SSH_Exit255(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		script   string
		wantConn bool
		wantKind domain.AgentErrorKind
	}{
		{name: "no output is a connection failure", script: "exit 255\n", wantConn: true, wantKind: domain.ErrPortExit},
		{name: "after output is not", script: "echo the-runtime-answered\nexit 255\n", wantConn: false, wantKind: domain.ErrTurnFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			target := &LaunchTarget{
				Command:       agenttest.WriteScript(t, t.TempDir(), "ssh", tt.script),
				SSHHost:       "user@stand-in-host",
				WorkspacePath: t.TempDir(),
				RemoteCommand: "irrelevant-remote-command",
			}
			hooks := noopHooks()
			hooks.ParseLine = func(line []byte, _ func(domain.AgentEvent), _ string) (any, error) {
				return string(line), nil
			}
			hooks.OnFinalize = func(emit func(domain.AgentEvent), _ any, _ int, _ []string, _ *domain.AgentError) (domain.TurnResult, *domain.AgentError) {
				EmitTurnFailed(emit, "exit code 255", 0, domain.TokenUsage{})
				return domain.TurnResult{ExitReason: domain.EventTurnFailed}, &domain.AgentError{Kind: domain.ErrTurnFailed, Message: "exit code 255"}
			}
			sess := NewForkPerTurnSession(target, hooks, slog.Default(), 0)

			emit, _ := sinkEvents()
			_, err := sess.RunTurn(context.Background(), "p", emit)

			if got := errors.Is(err, sshutil.ErrConnectionFailed); got != tt.wantConn {
				t.Fatalf("RunTurn() error = %v, wraps sshutil.ErrConnectionFailed = %v, want %v", err, got, tt.wantConn)
			}
			requireAgentError(t, err, tt.wantKind)
		})
	}
}
