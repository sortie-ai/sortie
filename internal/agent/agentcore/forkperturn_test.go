package agentcore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/waittest"
)

type trapParams struct {
	Marker       string
	ExitOnSignal bool
	ExitCode     int
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
		return p.ExitCode
	}
	agenttest.Hang()
	return 0
}

func init() {
	scenarios["trap"] = agenttest.Typed(trapScenario)
}

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

func lineThenSelfSignalScenario(_ []string, _ struct{}) int {
	fmt.Println(`{"type":"notification"}`)
	if runtime.GOOS == "windows" {
		return int(uint32(0xC000013A))
	}
	if proc, err := os.FindProcess(os.Getpid()); err == nil {
		_ = proc.Signal(syscall.SIGTERM)
	}
	agenttest.Hang()
	return 0
}

func init() {
	scenarios["lineThenSelfSignal"] = agenttest.Typed(lineThenSelfSignalScenario)
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
		BuildArgs: func(turn int, prompt string) []string { return nil },
		ParseLine: func(line []byte, emit func(domain.AgentEvent), pid string, _ *TurnSpend) (any, error) {
			return nil, nil
		},
		GetUsage:     func() (domain.TokenUsage, map[string]domain.TokenUsage, bool) { return domain.TokenUsage{}, nil, false },
		GetSessionID: func() string { return "" },
		OnFinalize: func(emit func(domain.AgentEvent), lastParsed any, exitCode int, _ error, stderrLines []string, _ *domain.AgentError, _ *TurnSpend) (domain.TurnResult, *domain.AgentError) {
			EmitTurnCompleted(emit, "ok", 0, domain.TokenUsage{}, nil)
			return domain.TurnResult{ExitReason: domain.EventTurnCompleted}, nil
		},
	}
}

// hooksWithEarlyExitDisposition returns a ForkPerTurnHooks whose
// OnFinalize copies earlyExit onto TurnEvidence.EarlyExit and decides
// through the shared FinalizeTurn, the way every production adapter's
// own OnFinalize hook does. Tests exercising the skeleton's early-exit
// computation use this in place of noopHooks, whose default OnFinalize
// ignores earlyExit entirely.
func hooksWithEarlyExitDisposition() ForkPerTurnHooks {
	hooks := noopHooks()
	hooks.OnFinalize = func(emit func(domain.AgentEvent), _ any, exitCode int, _ error, _ []string, earlyExit *domain.AgentError, _ *TurnSpend) (domain.TurnResult, *domain.AgentError) {
		return FinalizeTurn(emit, slog.Default(), TurnEvidence{
			ExitObserved: true,
			ExitCode:     exitCode,
			EarlyExit:    earlyExit,
		}, TurnMeta{}, TurnSpend{})
	}
	return hooks
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
		t.Errorf("AgentError.Kind = %q (%v), want %q", ae.Kind, ae, want)
	}
}

// wantUsageVerdictSnapshot is the non-zero snapshot every usageVerdictDouble
// in this file reports, paired with the measured value each subtest
// controls.
var wantUsageVerdictSnapshot = domain.TokenUsage{InputTokens: 700, OutputTokens: 300, TotalTokens: 1000}

// usageVerdictDouble returns a GetUsage double that reports usage and
// measured on every call, together with a pointer to the number of times it
// was called.
func usageVerdictDouble(usage domain.TokenUsage, measured bool) (func() (domain.TokenUsage, map[string]domain.TokenUsage, bool), *int) {
	calls := new(int)
	return func() (domain.TokenUsage, map[string]domain.TokenUsage, bool) {
		*calls++
		return usage, nil, measured
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
// that emits it on the first line it sees.
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

	got := waittest.Receive(t, done, "RunTurn return")
	return got.result, events, got.err
}

// parseLineEmitSessionStartedOnce returns a ParseLine hook that emits
// EventSessionStarted the first time it is called and does nothing on
// every later call.
func parseLineEmitSessionStartedOnce() func([]byte, func(domain.AgentEvent), string, *TurnSpend) (any, error) {
	var started bool
	return func(_ []byte, emit func(domain.AgentEvent), pid string, _ *TurnSpend) (any, error) {
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
		ready := filepath.Join(tmpDir, "ready")
		script := agenttest.FakeRuntime(t, tmpDir, "agent", agenttest.OutputScenario, agenttest.Output{Hang: true, ReadyFile: ready})
		target := newTestTarget(tmpDir, script)
		sess := NewForkPerTurnSession(target, noopHooks(), slog.Default(), 0)

		ctx, cancel := context.WithCancel(context.Background())
		emit, events := sinkEvents()
		done := make(chan error, 1)
		go func() { _, err := sess.RunTurn(ctx, "p", emit); done <- err }()

		waittest.File(t, ready)
		cancel()

		err := waittest.Receive(t, done, "RunTurn return after cancel")
		requireAgentError(t, err, domain.ErrTurnCancelled)
		if !hasEventType(*events, domain.EventTurnCancelled) {
			t.Errorf("EventTurnCancelled not emitted; got %v", *events)
		}
	})

	t.Run("Arm1_UsageCarriage_CtxCancelBeforeEOF", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		ready := filepath.Join(tmpDir, "ready")
		script := agenttest.FakeRuntime(t, tmpDir, "agent", agenttest.OutputScenario, agenttest.Output{Hang: true, ReadyFile: ready})
		target := newTestTarget(tmpDir, script)
		stubUsage := domain.TokenUsage{InputTokens: 500, OutputTokens: 100, TotalTokens: 600}
		hooks := noopHooks()
		hooks.GetUsage = func() (domain.TokenUsage, map[string]domain.TokenUsage, bool) { return stubUsage, nil, true }
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

		waittest.File(t, ready)
		cancel()

		got := waittest.Receive(t, done, "RunTurn outcome after cancel")
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
		hooks.GetUsage = func() (domain.TokenUsage, map[string]domain.TokenUsage, bool) { return stubUsage, nil, true }
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
		// the context is cancelled.
		script := agenttest.FakeRuntime(t, tmpDir, "agent", "repeatLine", repeatLineParams{Line: "{}"})
		target := newTestTarget(tmpDir, script)
		scanning := make(chan struct{})
		hooks := noopHooks()
		var scanned bool
		hooks.ParseLine = func([]byte, func(domain.AgentEvent), string, *TurnSpend) (any, error) {
			if !scanned {
				scanned = true
				close(scanning)
			}
			return nil, nil
		}
		sess := NewForkPerTurnSession(target, hooks, slog.Default(), 0)

		ctx, cancel := context.WithCancel(waittest.Context(t))
		defer cancel()
		emit, _ := sinkEvents()
		done := make(chan error, 1)
		go func() { _, err := sess.RunTurn(ctx, "p", emit); done <- err }()

		waittest.Receive(t, scanning, "first scanned line")
		cancel()
		requireAgentError(t, waittest.Receive(t, done, "RunTurn error"), domain.ErrTurnCancelled)
	})

	t.Run("Arm4_Exit127_NoOutputYieldsEarlyExitReport", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		script := agenttest.FakeRuntime(t, tmpDir, "agent", agenttest.OutputScenario, agenttest.Output{Stderr: "cmd not found\n", ExitCode: 127})
		spy := &agenttest.LogSpy{}
		target := newTestTarget(tmpDir, script)
		sess := NewForkPerTurnSession(target, hooksWithEarlyExitDisposition(), slog.New(spy), 0)

		emit, events := sinkEvents()
		_, err := sess.RunTurn(context.Background(), "p", emit)

		requireAgentError(t, err, domain.ErrPortExit)
		if !hasEventType(*events, domain.EventTurnFailed) {
			t.Errorf("EventTurnFailed not emitted; got %v", *events)
		}
		var agentErr *domain.AgentError
		errors.As(err, &agentErr)
		const wantMessage = "the agent runtime exited before responding: exit status 127"
		if agentErr.Message != wantMessage {
			t.Errorf("AgentError.Message = %q, want %q", agentErr.Message, wantMessage)
		}
		var earlyExitErr *EarlyExitError
		if !errors.As(agentErr.Err, &earlyExitErr) {
			t.Fatalf("AgentError.Err = %v, want an *EarlyExitError", agentErr.Err)
		}
		if earlyExitErr.Output() != "cmd not found" {
			t.Errorf("EarlyExitError.Output() = %q, want %q", earlyExitErr.Output(), "cmd not found")
		}
		agenttest.RequireWarnLines(t, spy, "Arm4")
	})

	t.Run("Arm4_Exit127_ReadableLineKeepsAgentNotFound", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		script := agenttest.FakeRuntime(t, tmpDir, "agent", agenttest.OutputScenario, agenttest.Output{Stdout: "a readable line\n", ExitCode: 127})
		spy := &agenttest.LogSpy{}
		target := newTestTarget(tmpDir, script)
		sess := NewForkPerTurnSession(target, hooksWithEarlyExitDisposition(), slog.New(spy), 0)

		emit, events := sinkEvents()
		_, err := sess.RunTurn(context.Background(), "p", emit)

		requireAgentError(t, err, domain.ErrAgentNotFound)
		if !hasEventType(*events, domain.EventTurnFailed) {
			t.Errorf("EventTurnFailed not emitted; got %v", *events)
		}
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
			script := agenttest.FakeRuntime(t, tmpDir, "agent", agenttest.OutputScenario, agenttest.Output{Stdout: "a readable line\n", ExitCode: 127})
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
		hooks.ParseLine = func(line []byte, emit func(domain.AgentEvent), pid string, _ *TurnSpend) (any, error) {
			return &resultToken{}, nil
		}
		hooks.OnFinalize = func(emit func(domain.AgentEvent), lastParsed any, exitCode int, _ error, stderrLines []string, _ *domain.AgentError, _ *TurnSpend) (domain.TurnResult, *domain.AgentError) {
			if lastParsed == nil {
				return domain.TurnResult{}, &domain.AgentError{Kind: domain.ErrTurnFailed, Message: "no result"}
			}
			EmitTurnCompleted(emit, "success", 0, domain.TokenUsage{}, nil)
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

	for _, tc := range []struct {
		name       string
		endByHook  bool
		wantEnding usage.TurnEnding
	}{
		{"cancelled turn", false, usage.EndingCancelled},
		{"turn ended through EndTurn", true, usage.EndingProtocolBroken},
	} {
		t.Run("PullWindows_Ending_"+tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(waittest.Context(t))
			defer cancel()
			var gotEnding usage.TurnEnding
			hooks := noopHooks()
			hooks.ParseLine = func([]byte, func(domain.AgentEvent), string, *TurnSpend) (any, error) {
				if tc.endByHook {
					return nil, EndTurn(domain.ErrResponseError, "ended by hook")
				}
				cancel()
				return nil, nil
			}
			hooks.PullWindows = func(_ context.Context, ending usage.TurnEnding) ([]domain.UsageWindow, error) {
				gotEnding = ending
				return nil, nil
			}
			sess := NewForkPerTurnSession(fakeTarget(t, false, agenttest.OutputScenario, agenttest.Output{Stdout: "line\n", Hang: true}), hooks, slog.Default(), 100)

			_, err := sess.RunTurn(ctx, "p", func(domain.AgentEvent) {})

			if err == nil || gotEnding != tc.wantEnding {
				t.Errorf("RunTurn() = %v, PullWindows ending = %d, want an error and %d", err, gotEnding, tc.wantEnding)
			}
		})
	}

	t.Run("Arm7_OnFinalizeError", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		script := agenttest.FakeRuntime(t, tmpDir, "agent", agenttest.OutputScenario, agenttest.Output{Stderr: "arm7 stderr\n"})
		spy := &agenttest.LogSpy{}
		target := newTestTarget(tmpDir, script)

		hooks := noopHooks()
		hooks.OnFinalize = func(emit func(domain.AgentEvent), lastParsed any, exitCode int, _ error, stderrLines []string, _ *domain.AgentError, _ *TurnSpend) (domain.TurnResult, *domain.AgentError) {
			EmitTurnFailed(emit, "finalize error", 0, domain.TokenUsage{}, nil)
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
		hooks.OnFinalize = func(emit func(domain.AgentEvent), lastParsed any, exitCode int, _ error, stderrLines []string, _ *domain.AgentError, _ *TurnSpend) (domain.TurnResult, *domain.AgentError) {
			EmitTurnFailed(emit, "non-zero exit", 0, domain.TokenUsage{}, nil)
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
		hooks.OnFinalize = func(emit func(domain.AgentEvent), lastParsed any, exitCode int, _ error, stderrLines []string, _ *domain.AgentError, _ *TurnSpend) (domain.TurnResult, *domain.AgentError) {
			EmitTurnFailed(emit, "no output", 0, domain.TokenUsage{}, nil)
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
		hooks.OnFinalize = func(emit func(domain.AgentEvent), lastParsed any, exitCode int, _ error, stderrLines []string, _ *domain.AgentError, _ *TurnSpend) (domain.TurnResult, *domain.AgentError) {
			EmitTurnCompleted(emit, "implicit success", 0, domain.TokenUsage{OutputTokens: 10}, nil)
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
		hooks.OnFinalize = func(emit func(domain.AgentEvent), lastParsed any, exitCode int, _ error, stderrLines []string, _ *domain.AgentError, _ *TurnSpend) (domain.TurnResult, *domain.AgentError) {
			gotStderrLines = stderrLines
			EmitTurnCompleted(emit, "ok", 0, domain.TokenUsage{}, nil)
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
		hooks.OnFinalize = func(emit func(domain.AgentEvent), lastParsed any, exitCode int, _ error, stderrLines []string, _ *domain.AgentError, _ *TurnSpend) (domain.TurnResult, *domain.AgentError) {
			gotStderrLines = stderrLines
			EmitTurnCompleted(emit, "ok", 0, domain.TokenUsage{}, nil)
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
		ready := filepath.Join(tmpDir, "ready")
		script := agenttest.FakeRuntime(t, tmpDir, "agent", agenttest.OutputScenario, agenttest.Output{Hang: true, ReadyFile: ready})
		target := newTestTarget(tmpDir, script)
		sess := NewForkPerTurnSession(target, noopHooks(), slog.Default(), 0)

		emit, _ := sinkEvents()
		done := make(chan struct{})
		go func() {
			defer close(done)
			sess.RunTurn(context.Background(), "p", emit) //nolint:errcheck // testing concurrency
		}()

		waittest.File(t, ready)
		if err := sess.Stop(context.Background()); err != nil {
			t.Errorf("Stop() = %v", err)
		}

		waittest.Receive(t, done, "RunTurn return after Stop")
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

// TestForkPerTurnSession_EarlyExit_StderrOnlyAcrossExitStatuses pins
// that a turn whose runtime writes only to standard error and exits on
// its own, with no readable line on standard output, makes
// RunTurn return the early-exit report exactly once regardless of exit
// status, with Status() matching the exit and Output() the rendered
// stderr, and emits exactly one turn_failed event carrying the report's
// message.
func TestForkPerTurnSession_EarlyExit_StderrOnlyAcrossExitStatuses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		exitCode   int
		wantStatus string
	}{
		{name: "exit status 0", exitCode: 0, wantStatus: "exit status 0"},
		{name: "exit status 1", exitCode: 1, wantStatus: "exit status 1"},
		{name: "exit status 2", exitCode: 2, wantStatus: "exit status 2"},
		{name: "exit status 127", exitCode: 127, wantStatus: "exit status 127"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tmpDir := t.TempDir()
			script := agenttest.FakeRuntime(t, tmpDir, "agent", agenttest.OutputScenario, agenttest.Output{Stderr: "boom\n", ExitCode: tt.exitCode})
			target := newTestTarget(tmpDir, script)
			sess := NewForkPerTurnSession(target, hooksWithEarlyExitDisposition(), slog.Default(), 0)

			emit, events := sinkEvents()
			_, err := sess.RunTurn(context.Background(), "p", emit)

			requireAgentError(t, err, domain.ErrPortExit)
			var agentErr *domain.AgentError
			errors.As(err, &agentErr)
			var earlyExitErr *EarlyExitError
			if !errors.As(agentErr.Err, &earlyExitErr) {
				t.Fatalf("AgentError.Err = %v, want an *EarlyExitError", agentErr.Err)
			}
			if earlyExitErr.Status() != tt.wantStatus {
				t.Errorf("EarlyExitError.Status() = %q, want %q", earlyExitErr.Status(), tt.wantStatus)
			}
			if earlyExitErr.Output() != "boom" {
				t.Errorf("EarlyExitError.Output() = %q, want %q", earlyExitErr.Output(), "boom")
			}

			failed := 0
			for _, e := range *events {
				if e.Type == domain.EventTurnFailed {
					failed++
					if e.Message != agentErr.Message {
						t.Errorf("turn_failed Message = %q, want %q", e.Message, agentErr.Message)
					}
				}
			}
			if failed != 1 {
				t.Errorf("turn_failed event count = %d, want 1", failed)
			}
		})
	}
}

// TestForkPerTurnSession_EarlyExit_UnreadableStdoutVariants pins that a
// turn whose standard output held only empty lines, white space, or
// escape sequences yields the same report an empty stream does.
func TestForkPerTurnSession_EarlyExit_UnreadableStdoutVariants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		stdout string
	}{
		{name: "empty line", stdout: "\n"},
		{name: "whitespace-only line", stdout: "   \t  \n"},
		{name: "escape-sequence-only line", stdout: "\x1b[31m\x1b[0m\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tmpDir := t.TempDir()
			script := agenttest.FakeRuntime(t, tmpDir, "agent", agenttest.OutputScenario, agenttest.Output{Stdout: tt.stdout, Stderr: "boom\n", ExitCode: 1})
			target := newTestTarget(tmpDir, script)
			sess := NewForkPerTurnSession(target, hooksWithEarlyExitDisposition(), slog.Default(), 0)

			emit, _ := sinkEvents()
			_, err := sess.RunTurn(context.Background(), "p", emit)

			requireAgentError(t, err, domain.ErrPortExit)
		})
	}
}

// TestForkPerTurnSession_EarlyExit_ReadableLineKeepsExitCodeOutcome
// pins that a turn that wrote one readable line, JSON or plain text,
// keeps today's non-zero-exit outcome instead of the early-exit
// report.
func TestForkPerTurnSession_EarlyExit_ReadableLineKeepsExitCodeOutcome(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		stdout string
	}{
		{name: "JSON event", stdout: `{"type":"notification"}` + "\n"},
		{name: "plain text", stdout: "the runtime answered\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tmpDir := t.TempDir()
			script := agenttest.FakeRuntime(t, tmpDir, "agent", agenttest.OutputScenario, agenttest.Output{Stdout: tt.stdout, ExitCode: 1})
			target := newTestTarget(tmpDir, script)
			sess := NewForkPerTurnSession(target, hooksWithEarlyExitDisposition(), slog.Default(), 0)

			emit, _ := sinkEvents()
			_, err := sess.RunTurn(context.Background(), "p", emit)

			var agentErr *domain.AgentError
			if !errors.As(err, &agentErr) || agentErr.Kind != domain.ErrPortExit {
				t.Fatalf("RunTurn() error = %v, want a port_exit *domain.AgentError", err)
			}
			if agentErr.Message != "exit code 1" {
				t.Errorf("AgentError.Message = %q, want %q (the early-exit report must not apply once a line was read)", agentErr.Message, "exit code 1")
			}
		})
	}
}

// TestForkPerTurnSession_EarlyExit_RejectedLineIsNotAResponse pins that
// a line ParseLine rejects never counts as the turn's response, even
// though the runtime wrote it and exited cleanly: only a line the
// adapter's own decoder accepts satisfies OutputWatch.
func TestForkPerTurnSession_EarlyExit_RejectedLineIsNotAResponse(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := agenttest.FakeRuntime(t, tmpDir, "agent", agenttest.OutputScenario, agenttest.Output{Stdout: "not valid JSON\n"})
	target := newTestTarget(tmpDir, script)
	hooks := hooksWithEarlyExitDisposition()
	hooks.ParseLine = func(line []byte, emit func(domain.AgentEvent), pid string, _ *TurnSpend) (any, error) {
		return nil, errors.New("line is not valid JSON")
	}
	sess := NewForkPerTurnSession(target, hooks, slog.Default(), 0)

	emit, events := sinkEvents()
	_, err := sess.RunTurn(context.Background(), "p", emit)

	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) {
		t.Fatalf("RunTurn() error = %v, want *domain.AgentError", err)
	}
	if _, ok := errors.AsType[*EarlyExitError](agentErr.Err); !ok {
		t.Errorf("RunTurn() error = %v, want a chain carrying an *EarlyExitError", err)
	}
	if !hasEventType(*events, domain.EventMalformed) {
		t.Errorf("EventMalformed not emitted for the rejected line; got %v", *events)
	}
}

// TestForkPerTurnSession_EarlyExit_StopSignaledSuppressesReport pins
// that a turn whose process Stop signaled reports cancellation, never
// the early-exit report or a non-zero exit, even though the runtime
// wrote nothing and ended with a status of its own choosing. A runtime
// that handles the graceful signal exits however it likes, so the
// verdict cannot come from the exit status.
func TestForkPerTurnSession_EarlyExit_StopSignaledSuppressesReport(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	marker := filepath.Join(tmpDir, "trap-ready")
	script := agenttest.FakeRuntime(t, tmpDir, "agent", "trap", trapParams{Marker: marker, ExitOnSignal: true, ExitCode: 3})
	target := newTestTarget(tmpDir, script)
	sess := NewForkPerTurnSession(target, hooksWithEarlyExitDisposition(), slog.Default(), 0)

	emit, events := sinkEvents()
	done := make(chan error, 1)
	go func() {
		_, err := sess.RunTurn(context.Background(), "p", emit)
		done <- err
	}()

	waittest.File(t, marker)
	if err := sess.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() = %v", err)
	}

	err := waittest.Receive(t, done, "RunTurn return after Stop")

	requireAgentError(t, err, domain.ErrTurnCancelled)
	if !hasEventType(*events, domain.EventTurnCancelled) {
		t.Errorf("EventTurnCancelled not emitted; got %v", *events)
	}
}

// TestForkPerTurnSession_EarlyExit_WarnLinesEmittedOnce pins that a
// turn failing with the early-exit report still emits its "agent
// stderr" WARN records once per collected line, unaffected by the row
// selection.
func TestForkPerTurnSession_EarlyExit_WarnLinesEmittedOnce(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := agenttest.FakeRuntime(t, tmpDir, "agent", agenttest.OutputScenario, agenttest.Output{
		Stderr:   "first stderr line\nsecond stderr line\n",
		ExitCode: 1,
	})
	spy := &agenttest.LogSpy{}
	target := newTestTarget(tmpDir, script)
	sess := NewForkPerTurnSession(target, hooksWithEarlyExitDisposition(), slog.New(spy), 0)

	emit, _ := sinkEvents()
	_, err := sess.RunTurn(context.Background(), "p", emit)

	requireAgentError(t, err, domain.ErrPortExit)
	lines := agenttest.RequireWarnLines(t, spy, "EarlyExit_WarnLinesEmittedOnce")
	want := []string{"first stderr line", "second stderr line"}
	if !slices.Equal(lines, want) {
		t.Errorf("WARN agent stderr lines = %v, want %v", lines, want)
	}
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

func stubStartWithOwnedPipes(t *testing.T, fn func(context.Context, *exec.Cmd, *slog.Logger) (*procutil.OwnedPipes, *procutil.Group, error)) {
	t.Helper()
	orig := startWithOwnedPipes
	t.Cleanup(func() { startWithOwnedPipes = orig })
	startWithOwnedPipes = fn
}

func TestForkPerTurnSession_SignalExitAfterOutputIsAPortExitWithTheWaitError(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := agenttest.FakeRuntime(t, tmpDir, "agent", "lineThenSelfSignal", struct{}{})
	hooks := noopHooks()
	hooks.OnFinalize = func(emit func(domain.AgentEvent), _ any, exitCode int, waitErr error, _ []string, earlyExit *domain.AgentError, _ *TurnSpend) (domain.TurnResult, *domain.AgentError) {
		return FinalizeTurn(emit, slog.Default(), TurnEvidence{
			ExitObserved: true,
			ExitCode:     exitCode,
			WaitErr:      waitErr,
			EarlyExit:    earlyExit,
		}, TurnMeta{}, TurnSpend{})
	}
	sess := NewForkPerTurnSession(newTestTarget(tmpDir, script), hooks, slog.Default(), 0)

	emit, events := sinkEvents()
	_, err := sess.RunTurn(context.Background(), "p", emit)

	requireAgentError(t, err, domain.ErrPortExit)
	exitErr, ok := errors.AsType[*exec.ExitError](err)
	if !ok {
		t.Fatalf("RunTurn() error = %v, want a chain carrying the *exec.ExitError the wait returned", err)
	}
	wantExitCode := -1
	if runtime.GOOS == "windows" {
		wantExitCode = int(uint32(0xC000013A))
	}
	if got := exitErr.ExitCode(); got != wantExitCode {
		t.Errorf("ExitError.ExitCode() = %d, want %d", got, wantExitCode)
	}
	if !hasEventType(*events, domain.EventTurnFailed) {
		t.Errorf("EventTurnFailed not emitted; got %v", *events)
	}
	if hasEventType(*events, domain.EventTurnCancelled) {
		t.Errorf("EventTurnCancelled emitted for a signal Sortie did not send; got %v", *events)
	}
}

func TestForkPerTurnSession_ResumeFailureAfterTheDeadline(t *testing.T) {
	tests := []struct {
		name          string
		cancelled     bool
		wantKind      domain.AgentErrorKind
		wantCancelled bool
	}{
		{name: "start failed without a cancellation", cancelled: false, wantKind: domain.ErrPortExit, wantCancelled: false},
		{name: "start refused by a cancellation", cancelled: true, wantKind: domain.ErrTurnCancelled, wantCancelled: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			script := agenttest.FakeRuntime(t, tmpDir, "agent", agenttest.OutputScenario, agenttest.Output{})
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			stubStartWithOwnedPipes(t, func(context.Context, *exec.Cmd, *slog.Logger) (*procutil.OwnedPipes, *procutil.Group, error) {
				cancel()
				return nil, nil, &procutil.StartError{
					Stage:     procutil.StageProcessResume,
					Err:       errors.New("injected resume failure"),
					Cancelled: tt.cancelled,
				}
			})
			sess := NewForkPerTurnSession(newTestTarget(tmpDir, script), noopHooks(), slog.Default(), 0)

			emit, events := sinkEvents()
			_, err := sess.RunTurn(ctx, "p", emit)

			requireAgentError(t, err, tt.wantKind)
			if got := hasEventType(*events, domain.EventTurnCancelled); got != tt.wantCancelled {
				t.Errorf("EventTurnCancelled emitted = %t, want %t; events %v", got, tt.wantCancelled, *events)
			}
		})
	}
}

type timedLine struct {
	Delay time.Duration
	Line  string
}

func timedLinesScenario(_ []string, steps []timedLine) int {
	for _, step := range steps {
		time.Sleep(step.Delay)
		if step.Line != "" {
			fmt.Println(step.Line)
		}
	}
	return 0
}

func init() {
	scenarios["timedLines"] = agenttest.Typed(timedLinesScenario)
}

const turnPrompt = "the prompt"

var carriedEnv = []sshutil.EnvVar{{Name: "SORTIE_TEST_CARRY", Value: "carried-value"}}

func carriedPreamble() string {
	preamble, _ := io.ReadAll(sshutil.BuildSSHLaunch("host", "/ws", "cmd", nil, sshutil.SSHOptions{Env: carriedEnv}).StdinReader()) //nolint:errcheck // an in-memory reader
	return string(preamble)
}

func fakeTarget(t *testing.T, remote bool, scenario string, params any) *LaunchTarget {
	t.Helper()
	dir := t.TempDir()
	target := newTestTarget(dir, agenttest.FakeRuntime(t, dir, "agent", scenario, params))
	if remote {
		target.SSHHost, target.RemoteCommand = "user@stand-in-host", "irrelevant-remote-command"
	}
	return target
}

func TestForkPerTurnSession_LaunchSetup(t *testing.T) {
	var launched *exec.Cmd
	stubStartWithOwnedPipes(t, func(_ context.Context, cmd *exec.Cmd, _ *slog.Logger) (*procutil.OwnedPipes, *procutil.Group, error) {
		launched = cmd
		return nil, nil, errors.New("not started")
	})
	preamble := carriedPreamble()

	tests := []struct {
		name          string
		remote        bool
		promptOnStdin bool
		wantStdin     string
	}{
		{"local with the prompt on stdin", false, true, turnPrompt},
		{"local without it", false, false, ""},
		{"remote puts the prompt after the preamble", true, true, preamble + turnPrompt},
		{"remote without it carries the preamble only", true, false, preamble},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hooks := noopHooks()
			hooks.PromptOnStdin = tt.promptOnStdin
			hooks.TurnEnv = func() ([]string, []sshutil.EnvVar) { return []string{"KEEP=1", "DROP=2"}, carriedEnv }
			target := newTestTarget(t.TempDir(), "agent")
			target.WithheldEnv = []string{"DROP"}
			if tt.remote {
				target.SSHHost, target.RemoteCommand = "user@stand-in-host", "agent"
			}

			_, _ = NewForkPerTurnSession(target, hooks, slog.Default(), 0).RunTurn(context.Background(), turnPrompt, func(domain.AgentEvent) {}) //nolint:errcheck // the stubbed start always fails

			var stdin []byte
			if launched.Stdin != nil {
				stdin, _ = io.ReadAll(launched.Stdin) //nolint:errcheck // an in-memory reader
			}
			if string(stdin) != tt.wantStdin {
				t.Errorf("RunTurn(remote=%t, PromptOnStdin=%t) stdin = %q, want %q", tt.remote, tt.promptOnStdin, stdin, tt.wantStdin)
			}
			if !slices.Contains(launched.Env, "KEEP=1") || slices.Contains(launched.Env, "DROP=2") {
				t.Errorf("RunTurn(remote=%t) process environment = %v, want KEEP=1 and the withheld DROP removed", tt.remote, launched.Env)
			}
		})
	}
}

func TestForkPerTurnSession_EndTurn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		stopFirst   bool
		wantKind    domain.AgentErrorKind
		wantMessage string
		wantSettle  int
	}{
		{"wrapped signal", false, domain.ErrResponseError, "session id mismatch", 0},
		{"stop begun first", true, domain.ErrTurnCancelled, "turn cancelled", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var sess *ForkPerTurnSession
			parsed, settled := 0, 0
			hooks := noopHooks()
			hooks.ParseLine = func([]byte, func(domain.AgentEvent), string, *TurnSpend) (any, error) {
				parsed++
				if tt.stopFirst {
					if err := sess.Stop(context.Background()); err != nil {
						t.Errorf("Stop() = %v, want nil", err)
					}
				}
				return nil, fmt.Errorf("parse: %w", EndTurn(domain.ErrResponseError, "session id mismatch"))
			}
			hooks.SettleUsage = func(context.Context, func(domain.AgentEvent), *TurnSpend) { settled++ }
			target := fakeTarget(t, false, agenttest.OutputScenario, agenttest.Output{Stdout: "one\ntwo\nthree\n", Hang: true})
			sess = NewForkPerTurnSession(target, hooks, slog.Default(), 100)

			emit, _ := sinkEvents()
			_, err := sess.RunTurn(context.Background(), turnPrompt, emit)

			requireAgentError(t, err, tt.wantKind)
			var agentErr *domain.AgentError
			if errors.As(err, &agentErr) && agentErr.Message != tt.wantMessage {
				t.Errorf("AgentError.Message = %q, want %q", agentErr.Message, tt.wantMessage)
			}
			if parsed != 1 || settled != tt.wantSettle {
				t.Errorf("ParseLine calls = %d, SettleUsage calls = %d, want 1 and %d", parsed, settled, tt.wantSettle)
			}
		})
	}
}

func TestForkPerTurnSession_FirstEventTimeout(t *testing.T) {
	t.Parallel()

	const ms = time.Millisecond
	tests := []struct {
		name        string
		steps       []timedLine
		wantTimeout bool
	}{
		{"no line", []timedLine{{time.Hour, ""}}, true},
		{"rejected line restarts the wait", []timedLine{{300 * ms, "bad"}, {350 * ms, "good"}, {700 * ms, ""}}, false},
		{"accepted line ends the wait", []timedLine{{100 * ms, "good"}, {900 * ms, ""}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			hooks := noopHooks()
			hooks.FirstEventTimeout = 500 * ms
			hooks.ParseLine = func(line []byte, _ func(domain.AgentEvent), _ string, _ *TurnSpend) (any, error) {
				if string(line) == "bad" {
					return nil, errors.New("rejected")
				}
				return nil, nil
			}
			sess := NewForkPerTurnSession(fakeTarget(t, false, "timedLines", tt.steps), hooks, slog.Default(), 0)

			emit, _ := sinkEvents()
			_, err := sess.RunTurn(context.Background(), turnPrompt, emit)

			if tt.wantTimeout {
				requireAgentError(t, err, domain.ErrResponseTimeout)
			} else if err != nil {
				t.Errorf("RunTurn() error = %v, want nil", err)
			}
		})
	}
}

func TestForkPerTurnSession_SettleUsage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		scenario string
		params   any
		remote   bool
		cancel   bool
		deadline time.Duration
		wantKind domain.AgentErrorKind
	}{
		{name: "exit", scenario: agenttest.OutputScenario, params: agenttest.Output{}},
		{name: "cancel", scenario: agenttest.OutputScenario, params: agenttest.Output{Hang: true}, cancel: true, wantKind: domain.ErrTurnCancelled},
		{name: "read failure", scenario: "overflow", params: overflowParams{Size: 11000001}, wantKind: domain.ErrPortExit},
		{name: "first-event deadline", scenario: agenttest.OutputScenario, params: agenttest.Output{Hang: true}, deadline: 100 * time.Millisecond, wantKind: domain.ErrResponseTimeout},
		{name: "ssh connection failure", scenario: agenttest.OutputScenario, params: agenttest.Output{ExitCode: 255}, remote: true, wantKind: domain.ErrPortExit},
		{name: "exit code 127", scenario: agenttest.OutputScenario, params: agenttest.Output{Stdout: "line\n", ExitCode: 127}, wantKind: domain.ErrAgentNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			settled := domain.TokenUsage{InputTokens: 7, OutputTokens: 3, TotalTokens: 10}
			var usage domain.TokenUsage
			var settleCtxErr error
			calls := 0
			hooks := noopHooks()
			hooks.FirstEventTimeout = tt.deadline
			hooks.GetUsage = func() (domain.TokenUsage, map[string]domain.TokenUsage, bool) { return usage, nil, true }
			hooks.SettleUsage = func(ctx context.Context, emit func(domain.AgentEvent), _ *TurnSpend) {
				calls++
				settleCtxErr = ctx.Err()
				usage = settled
				emit(domain.AgentEvent{Type: domain.EventNotification})
				emit(domain.AgentEvent{Type: domain.EventNotification, AgentPID: "own"})
			}
			hooks.OnFinalize = func(emit func(domain.AgentEvent), _ any, _ int, _ error, _ []string, _ *domain.AgentError, _ *TurnSpend) (domain.TurnResult, *domain.AgentError) {
				return domain.TurnResult{ExitReason: domain.EventTurnCompleted, Usage: usage}, nil
			}
			if tt.cancel {
				hooks.EmitSessionStartID = func() string { cancel(); return "" }
			}
			sess := NewForkPerTurnSession(fakeTarget(t, tt.remote, tt.scenario, tt.params), hooks, slog.Default(), 0)

			emit, events := sinkEvents()
			result, err := sess.RunTurn(ctx, turnPrompt, emit)

			if tt.wantKind != "" {
				requireAgentError(t, err, tt.wantKind)
			} else if err != nil {
				t.Fatalf("RunTurn() error = %v, want nil", err)
			}
			if calls != 1 || settleCtxErr != nil || result.Usage != settled {
				t.Errorf("SettleUsage calls = %d, ctx.Err() = %v, RunTurn() Usage = %+v, want 1, nil and %+v", calls, settleCtxErr, result.Usage, settled)
			}
			var pids []string
			for _, e := range *events {
				if e.Type == domain.EventNotification {
					pids = append(pids, e.AgentPID)
				}
			}
			if len(pids) != 2 || pids[0] == "" || pids[1] != "own" {
				t.Errorf("RunTurn() settle event AgentPIDs = %q, want the turn pid then \"own\"", pids)
			}
		})
	}
}
