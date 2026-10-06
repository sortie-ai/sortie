//go:build unix

package agentcore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
)

type trapParams struct {
	Marker       string
	ExitOnSignal bool
}

func trapScenario(_ []string, p trapParams) int {
	ch := make(chan os.Signal, 1)
	// On Windows the graceful stop of [procutil.Group.SignalGraceful] arrives
	// as a console control event that the runtime delivers as an interrupt
	// rather than as SIGTERM.
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)

	if err := os.WriteFile(p.Marker, []byte("ready"), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	<-ch
	if p.ExitOnSignal {
		return 0
	}
	agenttest.Hang()
	return 0
}

func init() {
	scenarios["trap"] = agenttest.Typed(trapScenario)
}

// The marker exists because a fixed sleep cannot prove the trap is installed:
// if the signal wins that race the process exits on the platform's default
// disposition and the test passes without exercising the escalation it names.
func buildTrapAgent(t *testing.T, dir string, exitOnSignal bool) (path, marker string) {
	t.Helper()
	marker = filepath.Join(dir, "trap-ready")
	path = agenttest.FakeRuntime(t, dir, "agent", "trap", trapParams{Marker: marker, ExitOnSignal: exitOnSignal})
	return path, marker
}

func waitForTrap(t *testing.T, marker string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("subprocess did not install its TERM trap within 5s (marker %q absent)", marker)
}

func TestForkPerTurnSession_GracefulSignal(t *testing.T) {
	t.Parallel()

	t.Run("Stop_ConfiguredGraceBoundsTheWait", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		script, ready := buildTrapAgent(t, tmpDir, false)
		target := newTestTarget(tmpDir, script)
		sess := NewForkPerTurnSession(target, noopHooks(), slog.Default(), 200)

		emit, _ := sinkEvents()
		done := make(chan struct{})
		go func() {
			defer close(done)
			sess.RunTurn(context.Background(), "p", emit) //nolint:errcheck // testing Stop's timing, not RunTurn's outcome
		}()

		waitForTrap(t, ready)

		start := time.Now()
		if err := sess.Stop(context.Background()); err != nil {
			t.Errorf("Stop() = %v, want nil", err)
		}
		elapsed := time.Since(start)

		if elapsed > 2*time.Second {
			t.Errorf("Stop() force-terminated after %v, want well under the built-in 5s default (proves the configured 200ms grace bounded the wait, not DefaultStopGrace)", elapsed)
		}

		select {
		case <-done:
		case <-time.After(6 * time.Second):
			t.Fatal("RunTurn did not return within 6s after Stop")
		}
	})

	t.Run("RunTurn_CancelledTurnEscalatesOnConfiguredGrace", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		// A signal that is received but not acted on, rather than a hang
		// with no signal handling at all: this proves the escalation path
		// force-kills a subprocess that ignores the graceful shutdown
		// signal, not merely one that never gets a chance to see it.
		script, ready := buildTrapAgent(t, tmpDir, false)
		target := newTestTarget(tmpDir, script)
		sess := NewForkPerTurnSession(target, noopHooks(), slog.Default(), 200)

		ctx, cancel := context.WithCancel(context.Background())
		emit, _ := sinkEvents()
		done := make(chan error, 1)
		go func() {
			_, err := sess.RunTurn(ctx, "p", emit)
			done <- err
		}()

		waitForTrap(t, ready)
		start := time.Now()
		cancel()

		var err error
		select {
		case err = <-done:
		case <-time.After(6 * time.Second):
			t.Fatal("RunTurn did not return within 6s of cancellation")
		}
		elapsed := time.Since(start)

		requireAgentError(t, err, domain.ErrTurnCancelled)
		if elapsed > 2*time.Second {
			t.Errorf("RunTurn's cancelled-turn escalation took %v, want well under the built-in 5s default (proves the configured 200ms grace bounded cmd.WaitDelay, not DefaultStopGrace)", elapsed)
		}
	})

	t.Run("Stop_EscalationLogging", func(t *testing.T) {
		t.Parallel()

		t.Run("exit_inside_grace_emits_debug_and_no_warn", func(t *testing.T) {
			t.Parallel()
			tmpDir := t.TempDir()
			script, ready := buildTrapAgent(t, tmpDir, true)
			target := newTestTarget(tmpDir, script)
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
			// The grace is far longer than this exit needs. The subtest proves
			// which record the clean-exit path emits, not that the grace bounds
			// anything, and a tight bound races the runner's scheduler instead
			// of testing the code.
			sess := NewForkPerTurnSession(target, noopHooks(), logger, 30000)

			emit, _ := sinkEvents()
			done := make(chan struct{})
			go func() {
				defer close(done)
				sess.RunTurn(context.Background(), "p", emit) //nolint:errcheck // testing log output, not the outcome
			}()
			waitForTrap(t, ready)

			if err := sess.Stop(context.Background()); err != nil {
				t.Errorf("Stop() = %v, want nil", err)
			}
			<-done

			output := buf.String()
			if !strings.Contains(output, "agent exited during the graceful phase") {
				t.Errorf("Stop() did not log the exited-inside-grace Debug record: %s", output)
			}
			if !strings.Contains(output, `outcome=exited`) {
				t.Errorf("Stop()'s Debug record missing outcome=exited: %s", output)
			}
			if strings.Contains(output, "level=WARN") {
				t.Errorf("Stop() logged a Warn record for a clean exit, want none: %s", output)
			}
		})

		t.Run("grace_elapsed_emits_warn_with_outcome_and_grace", func(t *testing.T) {
			t.Parallel()
			tmpDir := t.TempDir()
			script, ready := buildTrapAgent(t, tmpDir, false)
			target := newTestTarget(tmpDir, script)
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
			sess := NewForkPerTurnSession(target, noopHooks(), logger, 150)

			emit, _ := sinkEvents()
			done := make(chan struct{})
			go func() {
				defer close(done)
				sess.RunTurn(context.Background(), "p", emit) //nolint:errcheck // testing log output, not the outcome
			}()
			waitForTrap(t, ready)

			if err := sess.Stop(context.Background()); err != nil {
				t.Errorf("Stop() = %v, want nil", err)
			}
			<-done

			output := buf.String()
			if !strings.Contains(output, "agent did not exit inside the graceful period and was force-terminated") {
				t.Errorf("Stop() did not log the grace-elapsed Warn record: %s", output)
			}
			if !strings.Contains(output, `outcome="grace elapsed"`) {
				t.Errorf(`Stop()'s Warn record missing outcome="grace elapsed": %s`, output)
			}
			if !strings.Contains(output, "grace=") {
				t.Errorf("Stop()'s Warn record missing the configured grace ceiling: %s", output)
			}
			if !strings.Contains(output, "elapsed=") {
				t.Errorf("Stop()'s Warn record missing the elapsed wait: %s", output)
			}
		})

		t.Run("caller_deadline_emits_warn_with_that_outcome", func(t *testing.T) {
			t.Parallel()
			tmpDir := t.TempDir()
			script, ready := buildTrapAgent(t, tmpDir, false)
			target := newTestTarget(tmpDir, script)
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
			sess := NewForkPerTurnSession(target, noopHooks(), logger, 5000)

			emit, _ := sinkEvents()
			done := make(chan struct{})
			go func() {
				defer close(done)
				sess.RunTurn(context.Background(), "p", emit) //nolint:errcheck // testing log output, not the outcome
			}()
			waitForTrap(t, ready)

			stopCtx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			defer cancel()
			err := sess.Stop(stopCtx)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("Stop() error = %v, want %v", err, context.DeadlineExceeded)
			}

			select {
			case <-done:
			case <-time.After(6 * time.Second):
				t.Fatal("RunTurn did not return within 6s")
			}

			output := buf.String()
			if !strings.Contains(output, `outcome="caller deadline"`) {
				t.Errorf(`Stop()'s Warn record missing outcome="caller deadline": %s`, output)
			}
		})
	})
}
