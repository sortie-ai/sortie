//go:build unix

package agentcore

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
)

// --- helpers ---

// newTestTarget builds a minimal LaunchTarget pointing at absCmd with
// tmpDir as the workspace directory.
func newTestTarget(tmpDir, absCmd string) *LaunchTarget {
	return &LaunchTarget{
		Command:       absCmd,
		WorkspacePath: tmpDir,
	}
}

// noopHooks returns a ForkPerTurnHooks with all required fields set to
// no-op implementations. Individual tests override specific fields.
func noopHooks() ForkPerTurnHooks {
	return ForkPerTurnHooks{
		BuildArgs:    func(turn int, prompt string) []string { return nil },
		ParseLine:    func(line []byte, emit func(domain.AgentEvent), pid string) (any, error) { return nil, nil },
		GetUsage:     func() domain.TokenUsage { return domain.TokenUsage{} },
		GetSessionID: func() string { return "" },
		OnFinalize: func(emit func(domain.AgentEvent), lastParsed any, exitCode int, stderrLines []string) (domain.TurnResult, *domain.AgentError) {
			EmitTurnCompleted(emit, "ok", 0, domain.TokenUsage{})
			return domain.TurnResult{ExitReason: domain.EventTurnCompleted}, nil
		},
	}
}

// sinkEvents returns an emit function and a pointer to the captured
// event slice. The slice grows as events are emitted.
func sinkEvents() (func(domain.AgentEvent), *[]domain.AgentEvent) {
	var events []domain.AgentEvent
	return func(e domain.AgentEvent) { events = append(events, e) }, &events
}

// hasEventType reports whether any event in the slice has the given type.
func hasEventType(events []domain.AgentEvent, kind domain.AgentEventType) bool {
	for _, e := range events {
		if e.Type == kind {
			return true
		}
	}
	return false
}

// findEventOfType returns the first event of the given type and true,
// or the zero value and false when no matching event exists.
func findEventOfType(events []domain.AgentEvent, kind domain.AgentEventType) (domain.AgentEvent, bool) {
	for _, e := range events {
		if e.Type == kind {
			return e, true
		}
	}
	return domain.AgentEvent{}, false
}

// requireAgentError fails if err is not a *domain.AgentError with the
// expected kind.
func requireAgentError(t *testing.T, err error, want domain.AgentErrorKind) {
	t.Helper()
	if err == nil {
		t.Fatalf("RunTurn() error = nil, want *domain.AgentError kind %q", want)
	}
	var ae *domain.AgentError
	if !errors.As(err, &ae) {
		t.Fatalf("RunTurn() error type = %T (%v), want *domain.AgentError", err, err)
	}
	if ae.Kind != want {
		t.Errorf("AgentError.Kind = %q, want %q", ae.Kind, want)
	}
}

// --- tests ---

func TestForkPerTurnSession(t *testing.T) {
	t.Parallel()

	t.Run("Arm1_CtxCancelBeforeEOF", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		script := agenttest.WriteScript(t, tmpDir, "agent", `sleep 60`)
		target := newTestTarget(tmpDir, script)
		sess := NewForkPerTurnSession(target, noopHooks(), slog.Default())

		ctx, cancel := context.WithCancel(context.Background())
		emit, events := sinkEvents()
		done := make(chan error, 1)
		go func() { _, err := sess.RunTurn(ctx, "p", emit); done <- err }()

		time.Sleep(100 * time.Millisecond) // let subprocess start
		cancel()

		err := <-done
		requireAgentError(t, err, domain.ErrTurnCancelled)
		if !hasEventType(*events, domain.EventTurnCancelled) {
			t.Errorf("EventTurnCancelled not emitted; got %v", *events)
		}
	})

	t.Run("Arm1_UsageCarriage_CtxCancelBeforeEOF", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		script := agenttest.WriteScript(t, tmpDir, "agent", `sleep 60`)
		target := newTestTarget(tmpDir, script)
		stubUsage := domain.TokenUsage{InputTokens: 500, OutputTokens: 100, TotalTokens: 600}
		hooks := noopHooks()
		hooks.GetUsage = func() domain.TokenUsage { return stubUsage }
		sess := NewForkPerTurnSession(target, hooks, slog.Default())

		ctx, cancel := context.WithCancel(context.Background())
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

		time.Sleep(100 * time.Millisecond) // let subprocess start
		cancel()

		got := <-done
		requireAgentError(t, got.err, domain.ErrTurnCancelled)

		cancelled, ok := findEventOfType(*events, domain.EventTurnCancelled)
		if !ok {
			t.Fatalf("EventTurnCancelled not emitted; got %v", *events)
		}
		if cancelled.Usage != stubUsage {
			t.Errorf("EventTurnCancelled.Usage = %+v, want %+v (GetUsage stub)", cancelled.Usage, stubUsage)
		}
		if got.result.Usage != stubUsage {
			t.Errorf("TurnResult.Usage = %+v, want %+v (GetUsage stub)", got.result.Usage, stubUsage)
		}
	})

	t.Run("Arm1_CtxAlreadyCancelledBeforeStart", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		script := agenttest.WriteScript(t, tmpDir, "agent", `sleep 60`)
		target := newTestTarget(tmpDir, script)
		sess := NewForkPerTurnSession(target, noopHooks(), slog.Default())

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		emit, events := sinkEvents()

		_, err := sess.RunTurn(ctx, "p", emit)

		requireAgentError(t, err, domain.ErrTurnCancelled)
		if !hasEventType(*events, domain.EventTurnCancelled) {
			t.Errorf("EventTurnCancelled not emitted; got %v", *events)
		}
	})

	t.Run("Arm2_ScannerOverflow", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		// Write one line exceeding stdoutScannerMaxTokenSize (10 MB).
		// Also write to stderr so EmitWarnLines has content to log at WARN.
		script := agenttest.WriteScript(t, tmpDir, "agent", `echo 'arm2 error' >&2
dd if=/dev/zero bs=11000001 count=1 2>/dev/null | tr '\000' 'x'
printf '\n'
`)
		spy := &agenttest.LogSpy{}
		target := newTestTarget(tmpDir, script)
		sess := NewForkPerTurnSession(target, noopHooks(), slog.New(spy))

		emit, events := sinkEvents()
		_, err := sess.RunTurn(context.Background(), "p", emit)

		requireAgentError(t, err, domain.ErrPortExit)
		if !hasEventType(*events, domain.EventTurnFailed) {
			t.Errorf("EventTurnFailed not emitted; got %v", *events)
		}
		agenttest.RequireWarnLines(t, spy, "Arm2")
	})

	t.Run("Arm2_UsageCarriage_ScannerOverflow", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		script := agenttest.WriteScript(t, tmpDir, "agent", `dd if=/dev/zero bs=11000001 count=1 2>/dev/null | tr '\000' 'x'
printf '\n'
`)
		target := newTestTarget(tmpDir, script)
		stubUsage := domain.TokenUsage{InputTokens: 500, OutputTokens: 100, TotalTokens: 600}
		hooks := noopHooks()
		hooks.GetUsage = func() domain.TokenUsage { return stubUsage }
		sess := NewForkPerTurnSession(target, hooks, slog.Default())

		emit, events := sinkEvents()
		result, err := sess.RunTurn(context.Background(), "p", emit)

		requireAgentError(t, err, domain.ErrPortExit)

		failed, ok := findEventOfType(*events, domain.EventTurnFailed)
		if !ok {
			t.Fatalf("EventTurnFailed not emitted; got %v", *events)
		}
		if failed.Usage != stubUsage {
			t.Errorf("EventTurnFailed.Usage = %+v, want %+v (GetUsage stub)", failed.Usage, stubUsage)
		}
		if result.Usage != stubUsage {
			t.Errorf("TurnResult.Usage = %+v, want %+v (GetUsage stub)", result.Usage, stubUsage)
		}
	})

	t.Run("Arm3_CtxCancelDuringScan", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		// Emit output continuously so the scanner is actively reading when
		// the context deadline fires.
		script := agenttest.WriteScript(t, tmpDir, "agent", `while true; do echo '{}'; done`)
		target := newTestTarget(tmpDir, script)
		sess := NewForkPerTurnSession(target, noopHooks(), slog.Default())

		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		emit, _ := sinkEvents()

		_, err := sess.RunTurn(ctx, "p", emit)
		requireAgentError(t, err, domain.ErrTurnCancelled)
	})

	t.Run("Arm4_Exit127", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		script := agenttest.WriteScript(t, tmpDir, "agent", `echo 'cmd not found' >&2
exit 127`)
		spy := &agenttest.LogSpy{}
		target := newTestTarget(tmpDir, script)
		sess := NewForkPerTurnSession(target, noopHooks(), slog.New(spy))

		emit, events := sinkEvents()
		_, err := sess.RunTurn(context.Background(), "p", emit)

		requireAgentError(t, err, domain.ErrAgentNotFound)
		if !hasEventType(*events, domain.EventTurnFailed) {
			t.Errorf("EventTurnFailed not emitted; got %v", *events)
		}
		agenttest.RequireWarnLines(t, spy, "Arm4")
	})

	t.Run("Arm5_ExternalSIGTERM", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		// Script sends SIGTERM to itself, exercising the WasSignaled path.
		script := agenttest.WriteScript(t, tmpDir, "agent", `kill -TERM $$
sleep 60`)
		target := newTestTarget(tmpDir, script)
		sess := NewForkPerTurnSession(target, noopHooks(), slog.Default())

		emit, events := sinkEvents()
		_, err := sess.RunTurn(context.Background(), "p", emit)

		requireAgentError(t, err, domain.ErrTurnCancelled)
		if !hasEventType(*events, domain.EventTurnCancelled) {
			t.Errorf("EventTurnCancelled not emitted; got %v", *events)
		}
	})

	t.Run("Arm6_ParseLineResult_OnFinalizeSuccess", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		script := agenttest.WriteScript(t, tmpDir, "agent", `echo '{}'`)
		target := newTestTarget(tmpDir, script)

		type resultToken struct{}
		hooks := noopHooks()
		hooks.ParseLine = func(line []byte, emit func(domain.AgentEvent), pid string) (any, error) {
			return &resultToken{}, nil
		}
		hooks.OnFinalize = func(emit func(domain.AgentEvent), lastParsed any, exitCode int, stderrLines []string) (domain.TurnResult, *domain.AgentError) {
			if lastParsed == nil {
				return domain.TurnResult{}, &domain.AgentError{Kind: domain.ErrTurnFailed, Message: "no result"}
			}
			EmitTurnCompleted(emit, "success", 0, domain.TokenUsage{})
			return domain.TurnResult{ExitReason: domain.EventTurnCompleted}, nil
		}
		sess := NewForkPerTurnSession(target, hooks, slog.Default())

		emit, events := sinkEvents()
		_, err := sess.RunTurn(context.Background(), "p", emit)

		if err != nil {
			t.Fatalf("RunTurn() error = %v, want nil", err)
		}
		if !hasEventType(*events, domain.EventTurnCompleted) {
			t.Errorf("EventTurnCompleted not emitted; got %v", *events)
		}
	})

	t.Run("Arm7_OnFinalizeError", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		script := agenttest.WriteScript(t, tmpDir, "agent", `echo 'arm7 stderr' >&2`)
		spy := &agenttest.LogSpy{}
		target := newTestTarget(tmpDir, script)

		hooks := noopHooks()
		hooks.OnFinalize = func(emit func(domain.AgentEvent), lastParsed any, exitCode int, stderrLines []string) (domain.TurnResult, *domain.AgentError) {
			EmitTurnFailed(emit, "finalize error", 0, domain.TokenUsage{})
			return domain.TurnResult{ExitReason: domain.EventTurnFailed}, &domain.AgentError{
				Kind:    domain.ErrTurnFailed,
				Message: "arm7 error",
			}
		}
		sess := NewForkPerTurnSession(target, hooks, slog.New(spy))

		emit, events := sinkEvents()
		_, err := sess.RunTurn(context.Background(), "p", emit)

		requireAgentError(t, err, domain.ErrTurnFailed)
		if !hasEventType(*events, domain.EventTurnFailed) {
			t.Errorf("EventTurnFailed not emitted; got %v", *events)
		}
		agenttest.RequireWarnLines(t, spy, "Arm7")
	})

	t.Run("Arm8_NonZeroExitNoResult", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		script := agenttest.WriteScript(t, tmpDir, "agent", `echo 'arm8 stderr' >&2
exit 1`)
		spy := &agenttest.LogSpy{}
		target := newTestTarget(tmpDir, script)

		hooks := noopHooks()
		hooks.OnFinalize = func(emit func(domain.AgentEvent), lastParsed any, exitCode int, stderrLines []string) (domain.TurnResult, *domain.AgentError) {
			EmitTurnFailed(emit, "non-zero exit", 0, domain.TokenUsage{})
			return domain.TurnResult{ExitReason: domain.EventTurnFailed}, &domain.AgentError{
				Kind:    domain.ErrPortExit,
				Message: "exit code 1",
			}
		}
		sess := NewForkPerTurnSession(target, hooks, slog.New(spy))

		emit, events := sinkEvents()
		_, err := sess.RunTurn(context.Background(), "p", emit)

		requireAgentError(t, err, domain.ErrPortExit)
		if !hasEventType(*events, domain.EventTurnFailed) {
			t.Errorf("EventTurnFailed not emitted; got %v", *events)
		}
		agenttest.RequireWarnLines(t, spy, "Arm8")
	})

	t.Run("Arm9_ZeroExitNoResultZeroTokens", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		script := agenttest.WriteScript(t, tmpDir, "agent", `echo 'arm9 stderr' >&2`)
		spy := &agenttest.LogSpy{}
		target := newTestTarget(tmpDir, script)

		hooks := noopHooks()
		hooks.OnFinalize = func(emit func(domain.AgentEvent), lastParsed any, exitCode int, stderrLines []string) (domain.TurnResult, *domain.AgentError) {
			EmitTurnFailed(emit, "no output", 0, domain.TokenUsage{})
			return domain.TurnResult{ExitReason: domain.EventTurnFailed}, &domain.AgentError{
				Kind:    domain.ErrTurnFailed,
				Message: "agent exited without output",
			}
		}
		sess := NewForkPerTurnSession(target, hooks, slog.New(spy))

		emit, events := sinkEvents()
		_, err := sess.RunTurn(context.Background(), "p", emit)

		requireAgentError(t, err, domain.ErrTurnFailed)
		if !hasEventType(*events, domain.EventTurnFailed) {
			t.Errorf("EventTurnFailed not emitted; got %v", *events)
		}
		agenttest.RequireWarnLines(t, spy, "Arm9")
	})

	t.Run("Arm10_ZeroExitNoResultWithTokens", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		script := agenttest.WriteScript(t, tmpDir, "agent", `exit 0`)
		target := newTestTarget(tmpDir, script)

		hooks := noopHooks()
		hooks.OnFinalize = func(emit func(domain.AgentEvent), lastParsed any, exitCode int, stderrLines []string) (domain.TurnResult, *domain.AgentError) {
			EmitTurnCompleted(emit, "implicit success", 0, domain.TokenUsage{OutputTokens: 10})
			return domain.TurnResult{
				ExitReason: domain.EventTurnCompleted,
				Usage:      domain.TokenUsage{OutputTokens: 10},
			}, nil
		}
		sess := NewForkPerTurnSession(target, hooks, slog.Default())

		emit, events := sinkEvents()
		_, err := sess.RunTurn(context.Background(), "p", emit)

		if err != nil {
			t.Fatalf("RunTurn() error = %v, want nil", err)
		}
		if !hasEventType(*events, domain.EventTurnCompleted) {
			t.Errorf("EventTurnCompleted not emitted; got %v", *events)
		}
	})

	t.Run("Bound_NoFireOnLongTurn", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		// Closing the standard error handle right after the line is
		// written puts the drain at EOF while the turn is still running,
		// so the assertion that no bound fires does not depend on the
		// drain goroutine being scheduled inside the grace.
		script := agenttest.WriteScript(t, tmpDir, "agent", `echo 'long turn stderr' >&2
exec 2>&-
sleep 0.5
exit 0`)
		spy := &agenttest.LogSpy{}
		target := newTestTarget(tmpDir, script)

		var gotStderrLines []string
		hooks := noopHooks()
		hooks.OnFinalize = func(emit func(domain.AgentEvent), lastParsed any, exitCode int, stderrLines []string) (domain.TurnResult, *domain.AgentError) {
			gotStderrLines = stderrLines
			EmitTurnCompleted(emit, "ok", 0, domain.TokenUsage{})
			return domain.TurnResult{ExitReason: domain.EventTurnCompleted}, nil
		}
		sess := NewForkPerTurnSession(target, hooks, slog.New(spy))
		sess.drainGrace = 50 * time.Millisecond // ten times shorter than the subprocess lifetime

		emit, _ := sinkEvents()
		_, err := sess.RunTurn(context.Background(), "p", emit)

		if err != nil {
			t.Fatalf("RunTurn() error = %v, want nil", err)
		}

		wantStderrLines := []string{"long turn stderr"}
		if !slices.Equal(gotStderrLines, wantStderrLines) {
			t.Errorf("OnFinalize stderrLines = %v, want %v", gotStderrLines, wantStderrLines)
		}

		for _, e := range spy.Entries() {
			if e.Level == slog.LevelWarn && e.Msg == "agent stderr was not fully collected before the process was reaped" {
				t.Errorf("unexpected abandonment WARN record emitted on a turn that outlived drainGrace: %+v", e)
			}
		}
	})

	t.Run("Bound_OrdinaryTurnStderrUnmodified", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		script := agenttest.WriteScript(t, tmpDir, "agent", `echo 'first stderr line' >&2
echo 'second stderr line' >&2`)
		target := newTestTarget(tmpDir, script)

		var gotStderrLines []string
		hooks := noopHooks()
		hooks.OnFinalize = func(emit func(domain.AgentEvent), lastParsed any, exitCode int, stderrLines []string) (domain.TurnResult, *domain.AgentError) {
			gotStderrLines = stderrLines
			EmitTurnCompleted(emit, "ok", 0, domain.TokenUsage{})
			return domain.TurnResult{ExitReason: domain.EventTurnCompleted}, nil
		}
		sess := NewForkPerTurnSession(target, hooks, slog.Default())

		emit, _ := sinkEvents()
		_, err := sess.RunTurn(context.Background(), "p", emit)

		if err != nil {
			t.Fatalf("RunTurn() error = %v, want nil", err)
		}

		wantStderrLines := []string{"first stderr line", "second stderr line"}
		if !slices.Equal(gotStderrLines, wantStderrLines) {
			t.Errorf("OnFinalize stderrLines = %v, want %v", gotStderrLines, wantStderrLines)
		}
	})

	t.Run("Stop_ConcurrentWithRunTurn", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		script := agenttest.WriteScript(t, tmpDir, "agent", `sleep 60`)
		target := newTestTarget(tmpDir, script)
		sess := NewForkPerTurnSession(target, noopHooks(), slog.Default())

		emit, _ := sinkEvents()
		done := make(chan struct{})
		go func() {
			defer close(done)
			sess.RunTurn(context.Background(), "p", emit) //nolint:errcheck // testing concurrency
		}()

		time.Sleep(100 * time.Millisecond) // let subprocess start
		if err := sess.Stop(context.Background()); err != nil {
			t.Errorf("Stop() = %v", err)
		}

		select {
		case <-done:
		case <-time.After(6 * time.Second):
			t.Fatal("RunTurn did not return within 6s after Stop")
		}
	})

	t.Run("TurnCount_NotIncrementedOnStartFailure", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		target := &LaunchTarget{
			Command:       "/nonexistent-binary-does-not-exist",
			WorkspacePath: tmpDir,
		}
		var lastTurn int
		hooks := noopHooks()
		hooks.BuildArgs = func(turn int, prompt string) []string {
			lastTurn = turn
			return nil
		}
		sess := NewForkPerTurnSession(target, hooks, slog.Default())

		// First call: cmd.Start() fails → s.turns must stay 0.
		_, err := sess.RunTurn(context.Background(), "p", func(domain.AgentEvent) {})
		if err == nil {
			t.Fatal("RunTurn() with invalid command returned nil error")
		}
		if lastTurn != 1 {
			t.Fatalf("BuildArgs turn on failed start = %d, want 1", lastTurn)
		}

		// Fix the command and retry on the same session. Since s.turns was
		// not incremented, prospectiveTurn is still 1.
		target.Command = agenttest.WriteScript(t, tmpDir, "agent-ok", `exit 0`)
		sess.RunTurn(context.Background(), "p", func(domain.AgentEvent) {}) //nolint:errcheck // testing turn tracking
		if lastTurn != 1 {
			t.Errorf("BuildArgs turn after failed-start retry = %d, want 1 (turns must not be incremented on failed start)", lastTurn)
		}
	})
}
