//go:build unix

package codex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/domain"
)

// scenarioSignalTrapName names the fake runtime scenario a subprocess
// under a SIGTERM disposition serves: it never speaks the app-server's
// JSON-RPC protocol, since these tests drive [CodexAdapter.StopSession]
// directly against a bare sessionState rather than through StartSession.
const scenarioSignalTrapName = "signal-trap"

func init() {
	fakeScenarios[scenarioSignalTrapName] = agenttest.Typed(scenarioSignalTrap)
}

type signalTrapParams struct {
	ReadyFile string `json:"readyFile"`
	OnTerm    string `json:"onTerm"`
}

// scenarioSignalTrap installs params.OnTerm's SIGTERM disposition,
// signals readiness by writing ReadyFile only once that disposition is
// installed (so a caller's subsequent SignalGraceful cannot race the
// installation), and then hangs until killed.
func scenarioSignalTrap(_ []string, params signalTrapParams) int {
	switch params.OnTerm {
	case "ignore":
		signal.Ignore(syscall.SIGTERM)
	case "exit":
		termCh := make(chan os.Signal, 1)
		signal.Notify(termCh, syscall.SIGTERM)
		go func() {
			<-termCh
			os.Exit(0)
		}()
	default:
		fmt.Fprintf(os.Stderr, "signal-trap: unknown onTerm %q\n", params.OnTerm)
		return 2
	}

	if err := os.WriteFile(params.ReadyFile, []byte("ready"), 0o644); err != nil {
		return 1
	}
	agenttest.Hang()
	return 0
}

// startFakeCodexProcess starts a fake runtime whose only behavior is
// its SIGTERM disposition (onTerm: "ignore" leaves it running, "exit"
// ends the process at once), in its own process group, and wires a
// minimal sessionState around it with an already-closed readerDone
// (this harness has no reader goroutine for StopSession to wait for).
func startFakeCodexProcess(t *testing.T, onTerm string, stopGraceMS int) *sessionState {
	t.Helper()

	dir := t.TempDir()
	readyPath := filepath.Join(dir, "ready")
	path := agenttest.FakeRuntime(t, dir, "codex-trap", scenarioSignalTrapName, signalTrapParams{ReadyFile: readyPath, OnTerm: onTerm})

	cmd := exec.Command(path) //nolint:gosec // fixed path under t.TempDir()
	pipes, group, err := procutil.StartWithOwnedPipes(cmd, nil)
	if err != nil {
		t.Fatalf("procutil.StartWithOwnedPipes() = %v", err)
	}
	t.Cleanup(func() { _ = pipes.Close() })
	t.Cleanup(func() { _ = group.Kill() })
	reaper := procutil.StartReaper(cmd, nil)

	readerDone := make(chan struct{})
	close(readerDone)

	state := &sessionState{
		agentConfig: domain.AgentConfig{StopGraceMS: stopGraceMS},
		group:       group,
		waitCh:      reaper.Done(),
		readerDone:  readerDone,
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(readyPath); err == nil {
			return state
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("startFakeCodexProcess: readiness marker %q did not appear within 5s", readyPath)
	return nil
}

func TestStopSession_ConfiguredGraceBoundsTheWait(t *testing.T) {
	t.Parallel()

	state := startFakeCodexProcess(t, "ignore", 200)

	start := time.Now()
	err := (&CodexAdapter{}).StopSession(context.Background(), domain.Session{Internal: state})
	elapsed := time.Since(start)

	if err != nil {
		t.Errorf("StopSession() = %v, want nil", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("StopSession() force-terminated after %v, want well under the built-in 5s default (proves the configured 200ms grace bounded the wait, not DefaultStopGrace)", elapsed)
	}
}

// TestStopSession_EscalationLogging asserts the escalation records
// codex's StopSession emits: Debug on an exit inside the grace, and
// Warn naming the outcome, the configured ceiling and the elapsed wait
// when the phase ends without one. Both escalation outcomes are
// reachable here, because StopSession ends the phase on whichever of
// the grace and the caller's deadline arrives first.
func TestStopSession_EscalationLogging(t *testing.T) {
	// No t.Parallel(): installs a global slog default.

	t.Run("exit_inside_grace_emits_debug_and_no_warn", func(t *testing.T) {
		var buf bytes.Buffer
		orig := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		t.Cleanup(func() { slog.SetDefault(orig) })

		// The grace is far longer than this exit needs. The subtest proves
		// which record the clean-exit path emits, not that the grace bounds
		// anything, and a tight bound races the runner's scheduler instead
		// of testing the code.
		state := startFakeCodexProcess(t, "exit", 30000)

		if err := (&CodexAdapter{}).StopSession(context.Background(), domain.Session{Internal: state}); err != nil {
			t.Errorf("StopSession() = %v, want nil", err)
		}

		output := buf.String()
		if !strings.Contains(output, "agent exited during the graceful phase") {
			t.Errorf("StopSession() did not log the exited-inside-grace Debug record: %s", output)
		}
		if !strings.Contains(output, `outcome=exited`) {
			t.Errorf("StopSession()'s Debug record missing outcome=exited: %s", output)
		}
		if strings.Contains(output, "level=WARN") {
			t.Errorf("StopSession() logged a Warn record for a clean exit, want none: %s", output)
		}
	})

	t.Run("grace_elapsed_emits_warn_with_outcome_and_grace", func(t *testing.T) {
		var buf bytes.Buffer
		orig := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		t.Cleanup(func() { slog.SetDefault(orig) })

		state := startFakeCodexProcess(t, "ignore", 150)

		if err := (&CodexAdapter{}).StopSession(context.Background(), domain.Session{Internal: state}); err != nil {
			t.Errorf("StopSession() = %v, want nil", err)
		}

		output := buf.String()
		if !strings.Contains(output, "agent did not exit inside the graceful period and was force-terminated") {
			t.Errorf("StopSession() did not log the grace-elapsed Warn record: %s", output)
		}
		if !strings.Contains(output, `outcome="grace elapsed"`) {
			t.Errorf(`StopSession()'s Warn record missing outcome="grace elapsed": %s`, output)
		}
		if !strings.Contains(output, "grace=") {
			t.Errorf("StopSession()'s Warn record missing the configured grace ceiling: %s", output)
		}
		if !strings.Contains(output, "elapsed=") {
			t.Errorf("StopSession()'s Warn record missing the elapsed wait: %s", output)
		}
	})

	t.Run("caller_deadline_ends_the_phase_and_is_reported", func(t *testing.T) {
		var buf bytes.Buffer
		orig := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		t.Cleanup(func() { slog.SetDefault(orig) })

		// The grace is far longer than the deadline, so only a
		// StopSession that reads its context can end this phase. When
		// it ignored the context, this arm waited out the whole grace
		// and then reported success.
		state := startFakeCodexProcess(t, "ignore", 30000)

		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()

		start := time.Now()
		err := (&CodexAdapter{}).StopSession(ctx, domain.Session{Internal: state})
		elapsed := time.Since(start)

		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("StopSession() = %v, want context.DeadlineExceeded", err)
		}
		if elapsed > 10*time.Second {
			t.Errorf("StopSession() returned after %v, want the caller's deadline to end the phase far below the 30s grace", elapsed)
		}

		output := buf.String()
		if !strings.Contains(output, `outcome="caller deadline"`) {
			t.Errorf(`StopSession()'s Warn record missing outcome="caller deadline": %s`, output)
		}
		if !strings.Contains(output, "grace=30s") {
			t.Errorf("StopSession()'s Warn record did not report the configured 30s ceiling: %s", output)
		}
	})
}

// blockingStdin is an io.WriteCloser whose Close blocks until the test
// releases it, standing in for a stdin handle whose OS-level close
// never returns.
type blockingStdin struct {
	release chan struct{}
}

func (blockingStdin) Write(p []byte) (int, error) { return len(p), nil }

func (b blockingStdin) Close() error {
	<-b.release
	return nil
}

// TestStopSession_StdinCloseNeverReturns drives StopSession against a
// real subprocess that ignores the graceful signal, with state.stdin
// replaced by a handle whose Close blocks forever. StopSession must
// still send the graceful signal, reach the escalation kill, and
// return within its configured grace, proving closing stdin cannot
// hold up the rest of teardown.
func TestStopSession_StdinCloseNeverReturns(t *testing.T) {
	t.Parallel()

	state := startFakeCodexProcess(t, "ignore", 200)

	release := make(chan struct{})
	state.stdin = blockingStdin{release: release}
	t.Cleanup(func() { close(release) })

	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- (&CodexAdapter{}).StopSession(context.Background(), domain.Session{Internal: state}) }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("StopSession() error = %v, want nil", err)
		}
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Errorf("StopSession() took %v, want well under 3s (a stdin Close that never returns must not block the signal, wait, or kill that follow it)", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("StopSession() did not return within 5s against a stdin Close that never returns")
	}
}
