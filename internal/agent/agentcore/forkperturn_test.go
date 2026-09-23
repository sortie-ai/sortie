package agentcore

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/domain"
)

// overflowParams parameterizes the "overflow" scenario: it writes Stderr (if
// any), then a single stdout line of Size 'x' bytes plus a trailing newline.
type overflowParams struct {
	Stderr string
	Size   int
}

// overflowScenario produces a single stdout line large enough to exceed the
// scanner's per-token limit, exercising the scan-error arm without shelling
// out to dd and tr.
func overflowScenario(_ []string, p overflowParams) int {
	if p.Stderr != "" {
		io.WriteString(os.Stderr, p.Stderr) //nolint:errcheck // best-effort fixture output
	}
	io.WriteString(os.Stdout, strings.Repeat("x", p.Size)+"\n") //nolint:errcheck // best-effort fixture output
	return 0
}

// repeatLineParams parameterizes the "repeatLine" scenario: it writes Line
// followed by a newline in an unbroken loop until killed.
type repeatLineParams struct {
	Line string
}

// repeatLineScenario keeps the scanner actively reading so a caller can
// exercise cancellation while a read is in flight.
func repeatLineScenario(_ []string, p repeatLineParams) int {
	for {
		if _, err := io.WriteString(os.Stdout, p.Line+"\n"); err != nil {
			return 0
		}
	}
}

// stderrThenSleepParams parameterizes the "stderrThenSleep" scenario: it
// writes Stderr, closes its own stderr handle, sleeps Sleep, then exits 0.
type stderrThenSleepParams struct {
	Stderr string
	Sleep  time.Duration
}

// stderrThenSleepScenario puts the stderr drain at EOF while the turn is
// still running (stdout stays open through Sleep), so a caller can assert
// that no abandonment bound fires on an ordinary long-running turn.
func stderrThenSleepScenario(_ []string, p stderrThenSleepParams) int {
	io.WriteString(os.Stderr, p.Stderr) //nolint:errcheck // best-effort fixture output
	os.Stderr.Close()                   //nolint:errcheck,gosec // deliberately closing early to simulate a closed handle
	time.Sleep(p.Sleep)
	return 0
}

func init() {
	scenarios["overflow"] = agenttest.Typed(overflowScenario)
	scenarios["repeatLine"] = agenttest.Typed(repeatLineScenario)
	scenarios["stderrThenSleep"] = agenttest.Typed(stderrThenSleepScenario)
}

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
		GetUsage:     func() (domain.TokenUsage, bool) { return domain.TokenUsage{}, false },
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

// wantUsageVerdictSnapshot is the non-zero snapshot every usageVerdictDouble
// in this file reports, paired with the measured value each subtest
// controls.
var wantUsageVerdictSnapshot = domain.TokenUsage{InputTokens: 700, OutputTokens: 300, TotalTokens: 1000}

// usageVerdictDouble returns a GetUsage double that reports usage and
// measured on every call, together with a pointer to the number of times it
// was called.
func usageVerdictDouble(usage domain.TokenUsage, measured bool) (func() (domain.TokenUsage, bool), *int) {
	calls := new(int)
	return func() (domain.TokenUsage, bool) {
		*calls++
		return usage, measured
	}, calls
}

// assertUsageVerdictCarried checks that result and the terminal event of
// type terminalType both carry wantUsageVerdictSnapshot and wantMeasured,
// that err is a *domain.AgentError of the given kind, and that the
// GetUsage double behind calls was invoked exactly once.
func assertUsageVerdictCarried(
	t *testing.T,
	result domain.TurnResult,
	err error,
	events []domain.AgentEvent,
	terminalType domain.AgentEventType,
	wantKind domain.AgentErrorKind,
	wantMeasured bool,
	calls int,
) {
	t.Helper()

	if calls != 1 {
		t.Errorf("GetUsage call count = %d, want 1", calls)
	}
	if result.UsageMeasured != wantMeasured {
		t.Errorf("TurnResult.UsageMeasured = %v, want %v", result.UsageMeasured, wantMeasured)
	}
	if result.Usage != wantUsageVerdictSnapshot {
		t.Errorf("TurnResult.Usage = %+v, want %+v", result.Usage, wantUsageVerdictSnapshot)
	}

	terminal, ok := findEventOfType(events, terminalType)
	if !ok {
		t.Fatalf("%s not emitted; got %v", terminalType, events)
	}
	if terminal.Usage != wantUsageVerdictSnapshot {
		t.Errorf("%s.Usage = %+v, want %+v", terminalType, terminal.Usage, wantUsageVerdictSnapshot)
	}

	requireAgentError(t, err, wantKind)
}

// runWithSessionStartCancel calls sess.RunTurn, cancelling its context the
// moment the first EventSessionStarted event arrives from a ParseLine hook
// that emits it on the first line it sees. It waits up to 5s for RunTurn to
// return and fails the test if it does not.
func runWithSessionStartCancel(t *testing.T, sess *ForkPerTurnSession) (domain.TurnResult, []domain.AgentEvent, error) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	var events []domain.AgentEvent
	emit := func(e domain.AgentEvent) {
		events = append(events, e)
		if e.Type == domain.EventSessionStarted {
			cancel()
		}
	}

	type outcome struct {
		result domain.TurnResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := sess.RunTurn(ctx, "p", emit)
		done <- outcome{result, err}
	}()

	select {
	case got := <-done:
		return got.result, events, got.err
	case <-time.After(5 * time.Second):
		t.Fatal("RunTurn did not return within 5s")
		return domain.TurnResult{}, nil, nil
	}
}

// parseLineEmitSessionStartedOnce returns a ParseLine hook that emits
// EventSessionStarted the first time it is called and does nothing on
// every later call.
func parseLineEmitSessionStartedOnce() func([]byte, func(domain.AgentEvent), string) (any, error) {
	var started bool
	return func(_ []byte, emit func(domain.AgentEvent), pid string) (any, error) {
		if !started {
			started = true
			EmitSessionStarted(emit, pid, "")
		}
		return nil, nil
	}
}

func TestForkPerTurnSession(t *testing.T) {
	t.Parallel()

	t.Run("Arm1_CtxCancelBeforeEOF", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		script := agenttest.FakeRuntime(t, tmpDir, "agent", agenttest.OutputScenario, agenttest.Output{Hang: true})
		target := newTestTarget(tmpDir, script)
		sess := NewForkPerTurnSession(target, noopHooks(), slog.Default(), 0)

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
		script := agenttest.FakeRuntime(t, tmpDir, "agent", agenttest.OutputScenario, agenttest.Output{Hang: true})
		target := newTestTarget(tmpDir, script)
		stubUsage := domain.TokenUsage{InputTokens: 500, OutputTokens: 100, TotalTokens: 600}
		hooks := noopHooks()
		hooks.GetUsage = func() (domain.TokenUsage, bool) { return stubUsage, true }
		sess := NewForkPerTurnSession(target, hooks, slog.Default(), 0)

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
		script := agenttest.FakeRuntime(t, tmpDir, "agent", agenttest.OutputScenario, agenttest.Output{Hang: true})
		target := newTestTarget(tmpDir, script)
		sess := NewForkPerTurnSession(target, noopHooks(), slog.Default(), 0)

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
		// One line exceeding stdoutScannerMaxTokenSize (10 MB), plus stderr
		// content so EmitWarnLines has something to log at WARN.
		script := agenttest.FakeRuntime(t, tmpDir, "agent", "overflow", overflowParams{Stderr: "arm2 error\n", Size: 11000001})
		spy := &agenttest.LogSpy{}
		target := newTestTarget(tmpDir, script)
		sess := NewForkPerTurnSession(target, noopHooks(), slog.New(spy), 0)

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
		script := agenttest.FakeRuntime(t, tmpDir, "agent", "overflow", overflowParams{Size: 11000001})
		target := newTestTarget(tmpDir, script)
		stubUsage := domain.TokenUsage{InputTokens: 500, OutputTokens: 100, TotalTokens: 600}
		hooks := noopHooks()
		hooks.GetUsage = func() (domain.TokenUsage, bool) { return stubUsage, true }
		sess := NewForkPerTurnSession(target, hooks, slog.Default(), 0)

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
		script := agenttest.FakeRuntime(t, tmpDir, "agent", "repeatLine", repeatLineParams{Line: "{}"})
		target := newTestTarget(tmpDir, script)
		sess := NewForkPerTurnSession(target, noopHooks(), slog.Default(), 0)

		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		emit, _ := sinkEvents()

		_, err := sess.RunTurn(ctx, "p", emit)
		requireAgentError(t, err, domain.ErrTurnCancelled)
	})

	t.Run("Arm4_Exit127", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		script := agenttest.FakeRuntime(t, tmpDir, "agent", agenttest.OutputScenario, agenttest.Output{Stderr: "cmd not found\n", ExitCode: 127})
		spy := &agenttest.LogSpy{}
		target := newTestTarget(tmpDir, script)
		sess := NewForkPerTurnSession(target, noopHooks(), slog.New(spy), 0)

		emit, events := sinkEvents()
		_, err := sess.RunTurn(context.Background(), "p", emit)

		requireAgentError(t, err, domain.ErrAgentNotFound)
		if !hasEventType(*events, domain.EventTurnFailed) {
			t.Errorf("EventTurnFailed not emitted; got %v", *events)
		}
		agenttest.RequireWarnLines(t, spy, "Arm4")
	})

	for _, tc := range []struct {
		name     string
		measured bool
	}{
		{"MeasuredTrue", true},
		{"MeasuredFalse", false},
	} {
		measured := tc.measured
		t.Run("Arm1_UsageVerdict_PostScanCancel_"+tc.name, func(t *testing.T) {
			t.Parallel()
			tmpDir := t.TempDir()
			script := agenttest.FakeRuntime(t, tmpDir, "agent", agenttest.OutputScenario, agenttest.Output{Stdout: "{}\n", Hang: true})
			target := newTestTarget(tmpDir, script)

			getUsage, calls := usageVerdictDouble(wantUsageVerdictSnapshot, measured)
			hooks := noopHooks()
			hooks.GetUsage = getUsage
			hooks.ParseLine = parseLineEmitSessionStartedOnce()
			sess := NewForkPerTurnSession(target, hooks, slog.Default(), 0)

			result, events, err := runWithSessionStartCancel(t, sess)

			assertUsageVerdictCarried(t, result, err, events, domain.EventTurnCancelled, domain.ErrTurnCancelled, measured, *calls)
		})
	}

	for _, tc := range []struct {
		name     string
		measured bool
	}{
		{"MeasuredTrue", true},
		{"MeasuredFalse", false},
	} {
		measured := tc.measured
		t.Run("Arm2_UsageVerdict_ScannerOverflow_"+tc.name, func(t *testing.T) {
			t.Parallel()
			tmpDir := t.TempDir()
			script := agenttest.FakeRuntime(t, tmpDir, "agent", "overflow", overflowParams{Size: 11000001})
			target := newTestTarget(tmpDir, script)

			getUsage, calls := usageVerdictDouble(wantUsageVerdictSnapshot, measured)
			hooks := noopHooks()
			hooks.GetUsage = getUsage
			sess := NewForkPerTurnSession(target, hooks, slog.Default(), 0)

			emit, events := sinkEvents()
			result, err := sess.RunTurn(context.Background(), "p", emit)

			assertUsageVerdictCarried(t, result, err, *events, domain.EventTurnFailed, domain.ErrPortExit, measured, *calls)
		})
	}

	for _, tc := range []struct {
		name     string
		measured bool
	}{
		{"MeasuredTrue", true},
		{"MeasuredFalse", false},
	} {
		measured := tc.measured
		t.Run("Arm4_UsageVerdict_Exit127_"+tc.name, func(t *testing.T) {
			t.Parallel()
			tmpDir := t.TempDir()
			script := agenttest.FakeRuntime(t, tmpDir, "agent", agenttest.OutputScenario, agenttest.Output{ExitCode: 127})
			target := newTestTarget(tmpDir, script)

			getUsage, calls := usageVerdictDouble(wantUsageVerdictSnapshot, measured)
			hooks := noopHooks()
			hooks.GetUsage = getUsage
			sess := NewForkPerTurnSession(target, hooks, slog.Default(), 0)

			emit, events := sinkEvents()
			result, err := sess.RunTurn(context.Background(), "p", emit)

			assertUsageVerdictCarried(t, result, err, *events, domain.EventTurnFailed, domain.ErrAgentNotFound, measured, *calls)
		})
	}

	for _, tc := range []struct {
		name     string
		measured bool
	}{
		{"MeasuredTrue", true},
		{"MeasuredFalse", false},
	} {
		measured := tc.measured
		t.Run("StartFailure_ContextCancelled_UsageVerdict_"+tc.name, func(t *testing.T) {
			t.Parallel()
			tmpDir := t.TempDir()
			target := &LaunchTarget{
				Command:       "/nonexistent-binary-does-not-exist",
				WorkspacePath: tmpDir,
			}

			getUsage, calls := usageVerdictDouble(wantUsageVerdictSnapshot, measured)
			hooks := noopHooks()
			hooks.GetUsage = getUsage
			sess := NewForkPerTurnSession(target, hooks, slog.Default(), 0)

			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			emit, events := sinkEvents()

			result, err := sess.RunTurn(ctx, "p", emit)

			assertUsageVerdictCarried(t, result, err, *events, domain.EventTurnCancelled, domain.ErrTurnCancelled, measured, *calls)
		})
	}

	for _, tc := range []struct {
		name     string
		measured bool
	}{
		{"MeasuredTrue", true},
		{"MeasuredFalse", false},
	} {
		measured := tc.measured
		t.Run("StartFailure_NotCancelled_UsageVerdict_"+tc.name, func(t *testing.T) {
			t.Parallel()
			tmpDir := t.TempDir()
			target := &LaunchTarget{
				Command:       "/nonexistent-binary-does-not-exist",
				WorkspacePath: tmpDir,
			}

			const wantSessionID = "start-failure-session"
			getUsage, calls := usageVerdictDouble(wantUsageVerdictSnapshot, measured)
			hooks := noopHooks()
			hooks.GetUsage = getUsage
			hooks.GetSessionID = func() string { return wantSessionID }
			sess := NewForkPerTurnSession(target, hooks, slog.Default(), 0)

			emit, events := sinkEvents()
			result, err := sess.RunTurn(context.Background(), "p", emit)

			if *calls != 1 {
				t.Errorf("GetUsage call count = %d, want 1", *calls)
			}
			if result.Usage != wantUsageVerdictSnapshot {
				t.Errorf("TurnResult.Usage = %+v, want %+v", result.Usage, wantUsageVerdictSnapshot)
			}
			if result.UsageMeasured != measured {
				t.Errorf("TurnResult.UsageMeasured = %v, want %v", result.UsageMeasured, measured)
			}
			if result.SessionID != wantSessionID {
				t.Errorf("TurnResult.SessionID = %q, want %q", result.SessionID, wantSessionID)
			}
			if result.ExitReason != "" {
				t.Errorf("TurnResult.ExitReason = %q, want empty", result.ExitReason)
			}
			if len(*events) != 0 {
				t.Errorf("emit received %d events, want 0: %v", len(*events), *events)
			}
			requireAgentError(t, err, domain.ErrPortExit)
		})
	}

	t.Run("Arm6_ParseLineResult_OnFinalizeSuccess", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		script := agenttest.FakeRuntime(t, tmpDir, "agent", agenttest.OutputScenario, agenttest.Output{Stdout: "{}\n"})
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
		sess := NewForkPerTurnSession(target, hooks, slog.Default(), 0)

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
		script := agenttest.FakeRuntime(t, tmpDir, "agent", agenttest.OutputScenario, agenttest.Output{Stderr: "arm7 stderr\n"})
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
		sess := NewForkPerTurnSession(target, hooks, slog.New(spy), 0)

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
		script := agenttest.FakeRuntime(t, tmpDir, "agent", agenttest.OutputScenario, agenttest.Output{Stderr: "arm8 stderr\n", ExitCode: 1})
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
		sess := NewForkPerTurnSession(target, hooks, slog.New(spy), 0)

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
		script := agenttest.FakeRuntime(t, tmpDir, "agent", agenttest.OutputScenario, agenttest.Output{Stderr: "arm9 stderr\n"})
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
		sess := NewForkPerTurnSession(target, hooks, slog.New(spy), 0)

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
		script := agenttest.FakeRuntime(t, tmpDir, "agent", agenttest.OutputScenario, agenttest.Output{})
		target := newTestTarget(tmpDir, script)

		hooks := noopHooks()
		hooks.OnFinalize = func(emit func(domain.AgentEvent), lastParsed any, exitCode int, stderrLines []string) (domain.TurnResult, *domain.AgentError) {
			EmitTurnCompleted(emit, "implicit success", 0, domain.TokenUsage{OutputTokens: 10})
			return domain.TurnResult{
				ExitReason: domain.EventTurnCompleted,
				Usage:      domain.TokenUsage{OutputTokens: 10},
			}, nil
		}
		sess := NewForkPerTurnSession(target, hooks, slog.Default(), 0)

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
		script := agenttest.FakeRuntime(t, tmpDir, "agent", "stderrThenSleep", stderrThenSleepParams{
			Stderr: "long turn stderr\n",
			Sleep:  500 * time.Millisecond,
		})
		spy := &agenttest.LogSpy{}
		target := newTestTarget(tmpDir, script)

		var gotStderrLines []string
		hooks := noopHooks()
		hooks.OnFinalize = func(emit func(domain.AgentEvent), lastParsed any, exitCode int, stderrLines []string) (domain.TurnResult, *domain.AgentError) {
			gotStderrLines = stderrLines
			EmitTurnCompleted(emit, "ok", 0, domain.TokenUsage{})
			return domain.TurnResult{ExitReason: domain.EventTurnCompleted}, nil
		}
		sess := NewForkPerTurnSession(target, hooks, slog.New(spy), 0)
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
		script := agenttest.FakeRuntime(t, tmpDir, "agent", agenttest.OutputScenario, agenttest.Output{
			Stderr: "first stderr line\nsecond stderr line\n",
		})
		target := newTestTarget(tmpDir, script)

		var gotStderrLines []string
		hooks := noopHooks()
		hooks.OnFinalize = func(emit func(domain.AgentEvent), lastParsed any, exitCode int, stderrLines []string) (domain.TurnResult, *domain.AgentError) {
			gotStderrLines = stderrLines
			EmitTurnCompleted(emit, "ok", 0, domain.TokenUsage{})
			return domain.TurnResult{ExitReason: domain.EventTurnCompleted}, nil
		}
		sess := NewForkPerTurnSession(target, hooks, slog.Default(), 0)

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
		script := agenttest.FakeRuntime(t, tmpDir, "agent", agenttest.OutputScenario, agenttest.Output{Hang: true})
		target := newTestTarget(tmpDir, script)
		sess := NewForkPerTurnSession(target, noopHooks(), slog.Default(), 0)

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

	t.Run("StopGrace_ConfiguredValueResolved", func(t *testing.T) {
		t.Parallel()
		sess := NewForkPerTurnSession(&LaunchTarget{}, noopHooks(), slog.Default(), 250)

		if sess.stopGrace != 250*time.Millisecond {
			t.Errorf("NewForkPerTurnSession(..., 250).stopGrace = %v, want %v", sess.stopGrace, 250*time.Millisecond)
		}
	})

	t.Run("StopGrace_AbsentDefaultsToBuiltIn", func(t *testing.T) {
		t.Parallel()
		sess := NewForkPerTurnSession(&LaunchTarget{}, noopHooks(), slog.Default(), 0)

		if sess.stopGrace != procutil.DefaultStopGrace {
			t.Errorf("NewForkPerTurnSession(..., 0).stopGrace = %v, want %v", sess.stopGrace, procutil.DefaultStopGrace)
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
		sess := NewForkPerTurnSession(target, hooks, slog.Default(), 0)

		// First call: cmd.Start() fails, so s.turns must stay 0.
		_, err := sess.RunTurn(context.Background(), "p", func(domain.AgentEvent) {})
		if err == nil {
			t.Fatal("RunTurn() with invalid command returned nil error")
		}
		if lastTurn != 1 {
			t.Fatalf("BuildArgs turn on failed start = %d, want 1", lastTurn)
		}

		// Fix the command and retry on the same session. Since s.turns was
		// not incremented, prospectiveTurn is still 1.
		target.Command = agenttest.FakeRuntime(t, tmpDir, "agent-ok", agenttest.OutputScenario, agenttest.Output{})
		sess.RunTurn(context.Background(), "p", func(domain.AgentEvent) {}) //nolint:errcheck // testing turn tracking
		if lastTurn != 1 {
			t.Errorf("BuildArgs turn after failed-start retry = %d, want 1 (turns must not be incremented on failed start)", lastTurn)
		}
	})
}

// TestForkPerTurnSession_LocalLaunchIgnoresSSHEnvNames asserts that a
// local launch (RemoteCommand empty) behaves identically whether or
// not LaunchTarget.SSHEnvNames names a set variable: the local branch
// never reads SSHEnvNames, so both launches complete the same way.
//
// Not a subtest of TestForkPerTurnSession: that function calls
// t.Parallel(), and t.Setenv panics under a parallel ancestor.
func TestForkPerTurnSession_LocalLaunchIgnoresSSHEnvNames(t *testing.T) {
	const varName = "SORTIE_FORKPERTURN_LOCAL_INVARIANCE"
	t.Setenv(varName, "should-never-reach-a-local-launch")

	tmpDir := t.TempDir()
	script := agenttest.FakeRuntime(t, tmpDir, "agent", agenttest.OutputScenario, agenttest.Output{})

	baseline := newTestTarget(tmpDir, script)
	withNames := &LaunchTarget{
		Command:       baseline.Command,
		WorkspacePath: baseline.WorkspacePath,
		SSHEnvNames:   []string{varName},
	}

	for _, tc := range []struct {
		name   string
		target *LaunchTarget
	}{
		{"SSHEnvNames absent", baseline},
		{"SSHEnvNames naming a set variable", withNames},
	} {
		sess := NewForkPerTurnSession(tc.target, noopHooks(), slog.Default(), 0)
		emit, events := sinkEvents()
		result, err := sess.RunTurn(context.Background(), "p", emit)
		if err != nil {
			t.Fatalf("%s: RunTurn() error = %v, want nil", tc.name, err)
		}
		if result.ExitReason != domain.EventTurnCompleted {
			t.Errorf("%s: ExitReason = %q, want %q", tc.name, result.ExitReason, domain.EventTurnCompleted)
		}
		if !hasEventType(*events, domain.EventTurnCompleted) {
			t.Errorf("%s: EventTurnCompleted not emitted; got %v", tc.name, *events)
		}
	}
}

func TestForkPerTurnSession_RunTurn_LinkedWorkspaceRefusedBeforeStart(t *testing.T) {
	t.Parallel()

	scriptDir := t.TempDir()
	script := agenttest.FakeRuntime(t, scriptDir, "agent", agenttest.OutputScenario, agenttest.Output{})

	linkTarget := t.TempDir()
	link := filepath.Join(t.TempDir(), "workspace-link")
	if err := os.Symlink(linkTarget, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("symlink creation requires elevated privileges on Windows")
		}
		t.Fatalf("Symlink: %v", err)
	}

	target := newTestTarget(link, script)
	sess := NewForkPerTurnSession(target, noopHooks(), slog.Default(), 0)

	emit, events := sinkEvents()
	result, err := sess.RunTurn(context.Background(), "p", emit)

	requireAgentError(t, err, domain.ErrInvalidWorkspaceCwd)
	if hasEventType(*events, domain.EventSessionStarted) {
		t.Errorf("RunTurn(linked workspace) emitted session_started; want none, got %v", *events)
	}
	if result.SessionID != "" {
		t.Errorf("RunTurn(linked workspace) result.SessionID = %q, want empty", result.SessionID)
	}

	if !sess.mu.TryLock() {
		t.Fatal("session mutex is still held after a bind failure, want it released")
	}
	sess.mu.Unlock()
}
