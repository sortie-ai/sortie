//go:build unix

package kiro

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/domain"
)

// scenarioKiroSSHStandIn names the Go fake runtime registered below into
// fakeScenarios.
const scenarioKiroSSHStandIn = "kiro.ssh-stand-in"

func init() {
	fakeScenarios[scenarioKiroSSHStandIn] = agenttest.Typed(runKiroSSHStandIn)
}

// kiroSSHStandInParams configures [runKiroSSHStandIn]: PATH is the PATH
// value its dropped-environment child receives.
type kiroSSHStandInParams struct {
	PATH string
}

// runKiroSSHStandIn is a stand-in "ssh" runtime: it ignores every
// argument ahead of the last one, the remote command
// sshutil.BuildSSHLaunch produced, and runs that command through sh -c
// with its own environment dropped and replaced by params.PATH alone.
func runKiroSSHStandIn(args []string, params kiroSSHStandInParams) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "ssh stand-in: no arguments")
		return 2
	}
	shPath, err := exec.LookPath("sh")
	if err != nil {
		fmt.Fprintf(os.Stderr, "ssh stand-in: sh not found: %v\n", err)
		return 2
	}

	remoteCommand := args[len(args)-1]
	cmd := exec.Command(shPath, "-c", remoteCommand) //nolint:gosec // remoteCommand is the launch this test built
	cmd.Env = []string{"PATH=" + params.PATH}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "ssh stand-in: %v\n", err)
		return 2
	}
	return 0
}

// kiroSSHStandInEnvPath returns the PATH value the ssh stand-in's
// dropped-environment child should receive: a directory holding only a
// symlink to sh, and, when includeDD is true, a symlink to dd as well.
func kiroSSHStandInEnvPath(t *testing.T, includeDD bool) string {
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

func TestCheckCredential_RemoteHostWithoutDDYieldsEarlyExitReport(t *testing.T) {
	// Not parallel: t.Setenv carries the fake ssh stand-in on PATH and
	// the carried environment variable.
	sshDir := t.TempDir()
	sshPath := agenttest.FakeRuntime(t, sshDir, "ssh", scenarioKiroSSHStandIn, kiroSSHStandInParams{PATH: kiroSSHStandInEnvPath(t, false)})

	const carriedName = "KIRO_TEST_CARRY_NODD"
	t.Setenv(carriedName, "some-value")

	target := agentcore.LaunchTarget{
		Command:       sshPath,
		RemoteCommand: "kiro-cli",
		SSHHost:       "user@stand-in-host",
		SSHEnvNames:   []string{carriedName},
		WorkspacePath: t.TempDir(),
	}

	agentErr := checkCredential(context.Background(), target, 5000, slog.Default())
	if agentErr == nil {
		t.Fatal("checkCredential() = nil, want the early-exit report")
	}
	if agentErr.Kind != domain.ErrPortExit {
		t.Errorf("checkCredential() Kind = %q, want %q", agentErr.Kind, domain.ErrPortExit)
	}
	var earlyExitErr *agentcore.EarlyExitError
	if !errors.As(agentErr.Err, &earlyExitErr) {
		t.Fatalf("checkCredential() error = %v, chain does not hold an *agentcore.EarlyExitError", agentErr)
	}
	if earlyExitErr.Status() != "exit status 1" {
		t.Errorf("EarlyExitError.Status() = %q, want %q", earlyExitErr.Status(), "exit status 1")
	}
	const ddMissingMessage = "sortie: dd is required on the remote host to receive environment variables"
	if !strings.Contains(earlyExitErr.Output(), ddMissingMessage) {
		t.Errorf("EarlyExitError.Output() = %q, want it to contain %q", earlyExitErr.Output(), ddMissingMessage)
	}
}

func TestCheckCredential_RemoteSSHExit255YieldsConnectionFailed(t *testing.T) {
	// Not parallel: t.Setenv carries the fake ssh stand-in on PATH.
	sshDir := t.TempDir()
	sshPath := agenttest.FakeRuntime(t, sshDir, "ssh", agenttest.OutputScenario, agenttest.Output{ExitCode: 255})

	target := agentcore.LaunchTarget{
		Command:       sshPath,
		RemoteCommand: "kiro-cli",
		SSHHost:       "user@stand-in-host",
		WorkspacePath: t.TempDir(),
	}

	agentErr := checkCredential(context.Background(), target, 5000, slog.Default())
	if agentErr == nil {
		t.Fatal("checkCredential() = nil, want a connection-failed error")
	}
	if agentErr.Kind != domain.ErrPortExit || !errors.Is(agentErr, sshutil.ErrConnectionFailed) {
		t.Errorf("checkCredential() = %v, want port_exit wrapping sshutil.ErrConnectionFailed", agentErr)
	}
}

func TestCheckCredential_CancelledContextMidWhoamiKeepsTodaysResult(t *testing.T) {
	// Not parallel: t.Setenv is incompatible with t.Parallel.
	ws := t.TempDir()
	cli := newKiroCLI(t, t.TempDir(), chatParams{WhoamiExitCode: 1, WhoamiHangMS: 500})
	target := agentcore.LaunchTarget{Command: cli, WorkspacePath: ws}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	agentErr := checkCredential(ctx, target, 5000, slog.Default())
	if agentErr == nil {
		t.Fatal("checkCredential() = nil, want today's non-report result for a cancelled guard")
	}
	if _, isEarlyExit := errors.AsType[*agentcore.EarlyExitError](agentErr.Err); isEarlyExit {
		t.Errorf("checkCredential() = %v, must never be the early-exit report once the guard's own context ended", agentErr)
	}
	if agentErr.Kind != domain.ErrCredentialUnverified {
		t.Errorf("checkCredential() Kind = %q, want %q", agentErr.Kind, domain.ErrCredentialUnverified)
	}
}

func TestCheckCredential_LaunchesWhoamiWithFormatJSON(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	argsLog := filepath.Join(t.TempDir(), "whoami-args.log")
	cli := newKiroCLI(t, t.TempDir(), chatParams{WhoamiArgsLogPath: argsLog})
	target := agentcore.LaunchTarget{Command: cli, WorkspacePath: ws}

	if agentErr := checkCredential(context.Background(), target, 5000, slog.Default()); agentErr != nil {
		t.Fatalf("checkCredential() = %v, want nil", agentErr)
	}

	data, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatalf("os.ReadFile(%q): %v", argsLog, err)
	}
	if got := strings.TrimSpace(string(data)); got != "whoami --format json" {
		t.Errorf("whoami invocation args = %q, want %q", got, "whoami --format json")
	}
}
