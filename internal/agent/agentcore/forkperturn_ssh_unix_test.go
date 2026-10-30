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
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/mcpconfig"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/waittest"
)

const ddMissingMessage = "sortie: dd is required on the remote host to receive environment variables"

func TestForkPerTurnSession_SSH_CarriesEnvironmentVariable(t *testing.T) {
	tmpDir := t.TempDir()
	agentDir := t.TempDir()

	sshPath := newStandIn(t, true, agenttest.SSHForwardSilent)

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

func TestForkPerTurnSession_SSH_NoCarriedNameLeavesLaunchUnchanged(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	agentDir := t.TempDir()

	sshPath := newStandIn(t, false, agenttest.SSHForwardSilent)

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

func TestForkPerTurnSession_SSH_NoDDEndsAsPortExitNotAgentNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	agentDir := t.TempDir()

	sshPath := newStandIn(t, false, agenttest.SSHForwardSilent)

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
	hooks.OnFinalize = func(emit func(domain.AgentEvent), _ any, exitCode int, _ error, _ []string, _ *domain.AgentError, _ *TurnSpend) (domain.TurnResult, *domain.AgentError) {
		return FinalizeTurn(emit, slog.Default(), TurnEvidence{ExitObserved: true, ExitCode: exitCode}, TurnMeta{}, TurnSpend{})
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
			hooks.ParseLine = func(line []byte, _ func(domain.AgentEvent), _ string, _ *TurnSpend) (any, error) {
				return string(line), nil
			}
			hooks.OnFinalize = func(emit func(domain.AgentEvent), _ any, _ int, _ error, _ []string, _ *domain.AgentError, _ *TurnSpend) (domain.TurnResult, *domain.AgentError) {
				EmitTurnFailed(emit, "exit code 255", 0, domain.TokenUsage{}, nil)
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

func placeNothing(mcpconfig.Delivery) sshutil.LatePreamble {
	return sshutil.LatePreamble{}
}

func forwardedHooks(session *ToolSession) ForkPerTurnHooks {
	hooks := noopHooks()
	hooks.Tools = session
	hooks.ToolPlacement = placeNothing
	return hooks
}

func TestForkPerTurnSession_SSH_ForwardedTurnCarriesSecretsOnlyOnStdin(t *testing.T) {
	t.Parallel()

	session := resolveRemote(t, agenttest.SSHForwardAllocated, fullConfig(t))
	endpoint, ok := session.currentRelay().EndpointFor(allocated(session.currentRelay().HostPort()))
	if !ok {
		t.Fatal("EndpointFor(allocated) = false, want true")
	}
	capturePath := filepath.Join(t.TempDir(), "captured.txt")
	session.target.RemoteCommand = agenttest.WriteScript(t, t.TempDir(), "agent.sh",
		"printf '%s\\n%s' \"$"+mcpconfig.BearerEnv+"\" \"$"+headerEnv+"\" > '"+capturePath+"'\n")
	sess := NewForkPerTurnSession(&session.target, forwardedHooks(session.ToolSession), slog.Default(), 0)

	emit, _ := sinkEvents()
	if _, err := sess.RunTurn(context.Background(), "p", emit); err != nil {
		t.Fatalf("RunTurn() error = %v, want nil", err)
	}

	if got, want := readWire(t, capturePath), endpoint.Bearer+"\n"+operatorHeader; got != want {
		t.Errorf("remote agent observed %q, want %q", got, want)
	}
	argv, stdin := session.wire()
	for _, secret := range []string{endpoint.Bearer, operatorHeader, envBlockSecret} {
		if strings.Contains(argv, secret) {
			t.Errorf("ssh argv = %q, want no %q", argv, secret)
		}
	}
	for _, assignment := range []string{
		mcpconfig.BearerEnv + "='" + endpoint.Bearer + "'",
		headerEnv + "='" + operatorHeader + "'",
	} {
		if !strings.Contains(stdin, assignment) {
			t.Errorf("ssh stdin = %q, want late assignment %q", stdin, assignment)
		}
	}
	if strings.Contains(stdin, envBlockSecret) {
		t.Errorf("ssh stdin = %q, want no env-block credential", stdin)
	}
}

func TestForkPerTurnSession_SSH_UnallocatedForwardStillRunsAgentWithoutBearer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		forward agenttest.SSHForward
	}{
		{name: "refused", forward: agenttest.SSHForwardRefused},
		{name: "unconfirmed", forward: agenttest.SSHForwardSilent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			session := resolveRemote(t, tt.forward, fullConfig(t))
			capturePath := filepath.Join(t.TempDir(), "captured.txt")
			session.target.RemoteCommand = agenttest.WriteScript(t, t.TempDir(), "agent.sh",
				"printf '%s' \"${"+mcpconfig.BearerEnv+"-unset}\" > '"+capturePath+"'\n")
			sess := NewForkPerTurnSession(&session.target, forwardedHooks(session.ToolSession), slog.Default(), 0)

			emit, _ := sinkEvents()
			result, err := sess.RunTurn(context.Background(), "p", emit)

			if err != nil || result.ExitReason != domain.EventTurnCompleted {
				t.Fatalf("RunTurn() = %v, %v, want a completed turn", result.ExitReason, err)
			}
			if got := readWire(t, capturePath); got != "unset" {
				t.Errorf("remote agent observed bearer %q, want it unset", got)
			}
			if _, stdin := session.wire(); strings.Contains(stdin, mcpconfig.BearerEnv) {
				t.Errorf("ssh stdin = %q, want no bearer assignment", stdin)
			}
		})
	}
}

func newBarrierStandIn(t *testing.T) string {
	t.Helper()

	binDir := t.TempDir()
	for _, tool := range []string{"sh", "flock", "setsid", "sleep", "rm"} {
		path, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("%s not found on PATH: %v", tool, err)
		}
		if err := os.Symlink(path, filepath.Join(binDir, tool)); err != nil {
			t.Fatalf("Symlink(%s): %v", tool, err)
		}
	}
	return agenttest.NewSSHStandIn(t, t.TempDir(), agenttest.SSHStandIn{PATH: binDir})
}

func TestForkPerTurnSession_SSH_StoppedTurnSettlesOnlyAfterTheRemoteTreeExited(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ready, exited, gate := filepath.Join(dir, "ready"), filepath.Join(dir, "exited"), filepath.Join(dir, "gate")
	if err := syscall.Mkfifo(gate, 0o600); err != nil {
		t.Fatalf("Mkfifo: %v", err)
	}
	gateHolder, err := os.OpenFile(gate, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open gate: %v", err)
	}
	release := sync.OnceFunc(func() { _ = gateHolder.Close() })
	t.Cleanup(release)

	agentScript := agenttest.WriteScript(t, t.TempDir(), "agent.sh",
		"setsid sh -c 'exec 5<\"$3\"; : >\"$1\"; read _ <&5; : >\"$2\"' sh '"+ready+"' '"+exited+"' '"+gate+"' >/dev/null 2>&1 &\nwait\n")
	target := &LaunchTarget{
		Command:       newBarrierStandIn(t),
		SSHHost:       "user@stand-in-host",
		WorkspacePath: t.TempDir(),
		RemoteCommand: agentScript,
		StopGrace:     30 * time.Second,
	}
	settled := make(chan bool, 1)
	hooks := noopHooks()
	hooks.SettleUsage = func(_ context.Context, _ func(domain.AgentEvent), _ *TurnSpend, _ bool) {
		_, err := os.Stat(exited)
		settled <- err == nil
	}
	sess := NewForkPerTurnSession(target, hooks, slog.Default(), 0)

	emit, _ := sinkEvents()
	done := make(chan struct{})
	go func() {
		defer close(done)
		sess.RunTurn(context.Background(), "p", emit) //nolint:errcheck // the stop's ordering is under test, not the turn's outcome
	}()
	waittest.File(t, ready)

	if err := sess.Stop(waittest.Context(t)); err != nil {
		t.Fatalf("Stop() = %v, want nil", err)
	}
	waittest.Never(t, settled, 300*time.Millisecond, "SettleUsage while the remote agent tree is alive")
	release()

	if remoteTreeExited := waittest.Receive(t, settled, "SettleUsage"); !remoteTreeExited {
		t.Errorf("SettleUsage saw the remote agent tree exited = %v, want true", remoteTreeExited)
	}
	waittest.Receive(t, done, "RunTurn return")
}
