package kiro

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/agenttest/credentialtest"
	"github.com/sortie-ai/sortie/internal/agent/agenttest/dispositiontest"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

// TestNewKiroAdapter_TrustToolsConflictMatchesValidateConfig asserts that
// NewKiroAdapter's constructor refusal and validateConfig's offline
// diagnostic report byte-identical text for the same conflicting
// configuration, so the two surfaces can never disagree.
func TestNewKiroAdapter_TrustToolsConflictMatchesValidateConfig(t *testing.T) {
	t.Parallel()

	config := map[string]any{
		"trust_all_tools": true,
		"trust_tools":     []any{"read", "grep"},
	}

	_, err := NewKiroAdapter(config)
	if err == nil {
		t.Fatal("NewKiroAdapter() error = nil, want trust_tools conflict error")
	}

	diags := validateConfig(registry.AgentConfigFields{Kind: "kiro", Passthrough: config})
	diag := hasCheck(diags, "kiro.trust_tools.conflict")
	if diag == nil {
		t.Fatalf("validateConfig() missing check %q; got %+v", "kiro.trust_tools.conflict", diags)
	}

	if err.Error() != diag.Message {
		t.Errorf("NewKiroAdapter() error = %q, validateConfig() diagnostic message = %q, want identical", err.Error(), diag.Message)
	}
}

// chatScenario is the [agenttest.Scenario] a fake kiro-cli runs: it answers
// the StartSession "whoami" canary and, for any other invocation (the
// "chat" turn), replays the configured stdout/stderr pair and exit code.
// Driving the real agentcore.ForkPerTurnSession against this binary
// exercises the adapter's OnFinalize and ParseLine closures end to end
// without a live kiro-cli.
const chatScenario = "kiro-chat"

// chatParams parameterizes [chatScenario].
type chatParams struct {
	WhoamiExitCode int

	// Stdout, Stderr and ExitCode are replayed verbatim for any
	// invocation other than "whoami".
	Stdout   string
	Stderr   string
	ExitCode int

	// ArgsLogPath, when non-empty, appends each non-whoami invocation's
	// argument list to the named file before replaying Stdout/Stderr, so
	// a test can inspect what buildArgs produced for each turn.
	ArgsLogPath string

	// MarkerFile, when non-empty, switches the reply after the first
	// non-whoami invocation: that call replays Stdout and creates
	// MarkerFile, and every later call replays MarkerStdout instead.
	MarkerFile   string
	MarkerStdout string
}

func runChat(args []string, p chatParams) int {
	if len(args) > 0 && args[0] == "whoami" {
		if p.WhoamiExitCode != 0 {
			return p.WhoamiExitCode
		}
		fmt.Print("Authenticated with API key\n")
		return 0
	}

	stdout := p.Stdout
	if p.MarkerFile != "" {
		if _, err := os.Stat(p.MarkerFile); err == nil {
			stdout = p.MarkerStdout
		} else if err := os.WriteFile(p.MarkerFile, nil, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "kiro fake: write marker file: %v\n", err)
			return 2
		}
	}

	if p.ArgsLogPath != "" {
		f, err := os.OpenFile(p.ArgsLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "kiro fake: open args log: %v\n", err)
			return 2
		}
		_, writeErr := fmt.Fprintln(f, strings.Join(args, " "))
		closeErr := f.Close()
		if err := errors.Join(writeErr, closeErr); err != nil {
			fmt.Fprintf(os.Stderr, "kiro fake: write args log: %v\n", err)
			return 2
		}
	}

	fmt.Print(stdout)
	fmt.Fprint(os.Stderr, p.Stderr)
	return p.ExitCode
}

// newKiroCLI creates a fake kiro-cli executable in dir running
// [chatScenario] with params, and returns its path.
func newKiroCLI(t *testing.T, dir string, params chatParams) string {
	t.Helper()
	return agenttest.FakeRuntime(t, dir, "kiro-cli", chatScenario, params)
}

// fakeScenarios collects every non-default fake-runtime scenario this
// package's tests register. Platform-specific test files add their own
// entries via init.
var fakeScenarios = map[string]agenttest.Scenario{
	chatScenario: agenttest.Typed(runChat),
}

func TestMain(m *testing.M) {
	agenttest.Main(m, fakeScenarios)
}

// setValidAPIKey sets a usable KIRO_API_KEY for the test. It is incompatible
// with t.Parallel because t.Setenv mutates process state.
func setValidAPIKey(t *testing.T) {
	t.Helper()
	t.Setenv("KIRO_API_KEY", "kiro-test-key")
}

func mustStartSession(t *testing.T, command string) (domain.AgentAdapter, domain.Session, *sessionState) {
	t.Helper()
	adapter, err := NewKiroAdapter(map[string]any{})
	if err != nil {
		t.Fatalf("NewKiroAdapter: %v", err)
	}
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: command},
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	state, ok := session.Internal.(*sessionState)
	if !ok {
		t.Fatalf("session.Internal type = %T, want *sessionState", session.Internal)
	}
	return adapter, session, state
}

// runChatTurn runs a single turn and returns the collected events and result.
func runChatTurn(t *testing.T, adapter domain.AgentAdapter, session domain.Session, prompt string) ([]domain.AgentEvent, domain.TurnResult, error) {
	t.Helper()
	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt: prompt,
		OnEvent: func(e domain.AgentEvent) {
			events = append(events, e)
		},
	})
	return events, result, err
}

// requireAgentError asserts err is a *domain.AgentError with the given Kind.
func requireAgentError(t *testing.T, err error, wantKind domain.AgentErrorKind) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with kind %q, got nil", wantKind)
	}
	var ae *domain.AgentError
	if !errors.As(err, &ae) {
		t.Fatalf("error type = %T, want *domain.AgentError", err)
	}
	if ae.Kind != wantKind {
		t.Errorf("AgentError.Kind = %q, want %q", ae.Kind, wantKind)
	}
}

// hasEventType reports whether any event in events matches typ.
func hasEventType(events []domain.AgentEvent, typ domain.AgentEventType) bool {
	for _, e := range events {
		if e.Type == typ {
			return true
		}
	}
	return false
}

// findEventByType returns the first event matching typ and true, or a
// zero value and false when no matching event exists.
func findEventByType(events []domain.AgentEvent, typ domain.AgentEventType) (domain.AgentEvent, bool) {
	for _, e := range events {
		if e.Type == typ {
			return e, true
		}
	}
	return domain.AgentEvent{}, false
}

// The observed success trailer and auth-failure line use the exact
// literals a real kiro-cli run produces: stderr carries
// "▸ Credits: … • Time: …" on a turn that ran, and "Authentication
// failed." on an invalid-credential turn.
const (
	creditsLine  = "▸ Credits: 0.01 • Time: 1s\n"
	authFailLine = "Authentication failed. Your API key may be invalid or expired.\n"
)

func TestOnFinalize_SuccessWithCredits(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	setValidAPIKey(t)

	bin := newKiroCLI(t, t.TempDir(), chatParams{Stdout: "\x1b[38;5;141m> \x1b[0mPONG", Stderr: creditsLine})
	adapter, session, state := mustStartSession(t, bin)

	events, result, err := runChatTurn(t, adapter, session, "ping")
	if err != nil {
		t.Fatalf("RunTurn() error = %v, want nil", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("result.ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}
	if result.Usage != (domain.TokenUsage{}) {
		t.Errorf("result.Usage = %+v, want zero TokenUsage", result.Usage)
	}
	if !hasEventType(events, domain.EventTurnCompleted) {
		t.Error("EventTurnCompleted not delivered")
	}
	if !state.resumeRequested {
		t.Error("state.resumeRequested = false, want true after a turn with the credits trailer")
	}

	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		Terminal:     agentcore.TerminalSuccess,
		ExitObserved: true,
		ExitCode:     0,
		Work:         agentcore.WorkUnobservable,
		WorkDetail:   "no credits trailer on stderr",
	}, result, err)
}

func TestOnFinalize_AuthFailureLineIsNotClassified(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	setValidAPIKey(t)

	bin := newKiroCLI(t, t.TempDir(), chatParams{Stderr: authFailLine})
	adapter, session, state := mustStartSession(t, bin)

	events, result, err := runChatTurn(t, adapter, session, "ping")

	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("result.ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}
	requireAgentError(t, err, domain.ErrTurnFailed)
	if !hasEventType(events, domain.EventTurnFailed) {
		t.Error("EventTurnFailed not delivered")
	}
	if result.Usage != (domain.TokenUsage{}) {
		t.Errorf("result.Usage = %+v, want zero TokenUsage", result.Usage)
	}
	if state.resumeRequested {
		t.Error("state.resumeRequested = true, want false after a failed turn")
	}

	const wantMessage = "agent exited without producing output: no message from the agent"
	var agentErr *domain.AgentError
	if errors.As(err, &agentErr) && agentErr.Message != wantMessage {
		t.Errorf("AgentError.Message = %q, want %q", agentErr.Message, wantMessage)
	}

	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		ExitObserved: true,
		ExitCode:     0,
		Work:         agentcore.WorkAbsent,
		WorkDetail:   "no message from the agent",
	}, result, err)
}

func TestOnFinalize_ExitZeroNoSignal(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	setValidAPIKey(t)

	// Exit 0 with neither the credits trailer nor the auth-failure line,
	// and stdout carrying only whitespace: the observer's stricter
	// trim-then-check threshold means a whitespace-only line is never
	// work, so a bare exit 0 with nothing behind it is never a success.
	bin := newKiroCLI(t, t.TempDir(), chatParams{Stdout: "   \n", Stderr: "a warning with no markers\n"})
	adapter, session, state := mustStartSession(t, bin)

	events, result, err := runChatTurn(t, adapter, session, "ping")

	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("result.ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}
	requireAgentError(t, err, domain.ErrTurnFailed)
	if !hasEventType(events, domain.EventTurnFailed) {
		t.Error("EventTurnFailed not delivered for bare exit 0")
	}
	if state.resumeRequested {
		t.Error("state.resumeRequested = true, want false after a no-credits turn")
	}

	// The zero-work message carries the family-wide stem plus the
	// declared-signal detail, so an operator can grep one string across
	// every adapter and still see kiro's own signal.
	const wantMessage = "agent exited without producing output: no message from the agent"
	turnFailed, ok := findEventByType(events, domain.EventTurnFailed)
	if !ok {
		t.Fatal("turn_failed event not found")
	}
	if turnFailed.Message != wantMessage {
		t.Errorf("turn_failed Message = %q, want %q", turnFailed.Message, wantMessage)
	}
	var agentErr *domain.AgentError
	if errors.As(err, &agentErr) && agentErr.Message != wantMessage {
		t.Errorf("AgentError.Message = %q, want %q", agentErr.Message, wantMessage)
	}

	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		ExitObserved: true,
		ExitCode:     0,
		Work:         agentcore.WorkAbsent,
		WorkDetail:   "no message from the agent",
	}, result, err)
}

func TestOnFinalize_NonZeroExit(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	setValidAPIKey(t)

	bin := newKiroCLI(t, t.TempDir(), chatParams{Stderr: "kiro: internal error\n", ExitCode: 1})
	adapter, session, _ := mustStartSession(t, bin)

	events, result, err := runChatTurn(t, adapter, session, "ping")

	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("result.ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}
	requireAgentError(t, err, domain.ErrPortExit)
	if !hasEventType(events, domain.EventTurnFailed) {
		t.Error("EventTurnFailed not delivered for non-zero exit")
	}

	// The non-zero-exit row deliberately uses two different texts: the
	// event message names the class, the error message names the code.
	turnFailed, ok := findEventByType(events, domain.EventTurnFailed)
	if !ok {
		t.Fatal("turn_failed event not found")
	}
	if turnFailed.Message != "non-zero exit" {
		t.Errorf("turn_failed Message = %q, want %q", turnFailed.Message, "non-zero exit")
	}
	var agentErr *domain.AgentError
	if errors.As(err, &agentErr) && agentErr.Message != "exit code 1" {
		t.Errorf("AgentError.Message = %q, want %q", agentErr.Message, "exit code 1")
	}

	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		ExitObserved: true,
		ExitCode:     1,
		Work:         agentcore.WorkUnobservable,
		WorkDetail:   "no credits trailer on stderr",
	}, result, err)
}

func TestOnFinalize_AuthLineWithStdoutIsNotAuthError(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	setValidAPIKey(t)

	bin := newKiroCLI(t, t.TempDir(), chatParams{Stdout: "partial answer", Stderr: authFailLine})
	adapter, session, _ := mustStartSession(t, bin)

	_, result, err := runChatTurn(t, adapter, session, "ping")

	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("result.ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}
	if err != nil {
		t.Errorf("RunTurn() error = %v, want nil", err)
	}

	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		ExitObserved: true,
		ExitCode:     0,
		Work:         agentcore.WorkPresent,
	}, result, err)
}

// TestOnFinalize_TranscriptNoCreditsNoAuthCompletes pins that a zero
// exit with a transcript on stdout and no credits trailer reports
// turn_completed, because the observer's own report of the non-blank
// stdout line is now the positive signal, not a bare exit code.
func TestOnFinalize_TranscriptNoCreditsNoAuthCompletes(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	setValidAPIKey(t)

	bin := newKiroCLI(t, t.TempDir(), chatParams{Stdout: "the answer is 42", Stderr: "a warning with no markers\n"})
	adapter, session, state := mustStartSession(t, bin)

	_, result, err := runChatTurn(t, adapter, session, "ping")

	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("result.ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}
	if err != nil {
		t.Errorf("RunTurn() error = %v, want nil", err)
	}
	if state.resumeRequested {
		t.Error("state.resumeRequested = true, want false (no credits trailer on this turn)")
	}

	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		ExitObserved: true,
		ExitCode:     0,
		Work:         agentcore.WorkPresent,
	}, result, err)
}

// TestOnFinalize_NoTokenEvent verifies that across every owned OnFinalize row,
// TurnResult.Usage stays zero, no EventTokenUsage is ever emitted, and the
// run is reported unmeasured: the headless kiro-cli runtime reports no
// token counts on any exit path, so TurnResult.UsageMeasured stays at its
// Go zero value regardless of how the turn ended.
func TestOnFinalize_NoTokenEvent(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	setValidAPIKey(t)

	tests := []struct {
		name       string
		stdoutBody string
		stderrBody string
		chatExit   int
	}{
		{"success with credits", "\x1b[0mPONG", creditsLine, 0},
		{"auth failed", "", authFailLine, 0},
		{"exit zero no signal", "transcript", "noise\n", 0},
		{"non-zero exit", "", "boom\n", 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// No t.Parallel(): the shared KIRO_API_KEY env is process-global.
			bin := newKiroCLI(t, t.TempDir(), chatParams{Stdout: tt.stdoutBody, Stderr: tt.stderrBody, ExitCode: tt.chatExit})
			adapter, session, _ := mustStartSession(t, bin)

			events, result, _ := runChatTurn(t, adapter, session, "ping")

			agenttest.AssertMeasurementAbsent(t, events, result)
		})
	}
}

// TestAssertUsageReporting proves kiro's registered usage-reporting
// declaration (none, none) against a real event stream from a
// successful headless run: the runtime reports credits, never token
// counts, so the turn's measurement contract stays empty.
func TestAssertUsageReporting(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	setValidAPIKey(t)

	bin := newKiroCLI(t, t.TempDir(), chatParams{Stdout: "\x1b[0mPONG", Stderr: creditsLine})
	adapter, session, _ := mustStartSession(t, bin)

	events, result, err := runChatTurn(t, adapter, session, "ping")
	if err != nil {
		t.Fatalf("RunTurn() error = %v, want nil", err)
	}

	agenttest.AssertUsageReporting(t, "kiro", []agenttest.UsageReportingCase{
		{Name: "successful headless run reports credits, not tokens", Events: events, Result: result},
	})
}

// TestOnFinalize_ResumeRequestedOnSecondTurn pins the resumeRequested
// side effect against being dropped in the move to the shared decision:
// a successful turn sets it, and the next turn's argument list carries
// the resume flag as a result.
func TestOnFinalize_ResumeRequestedOnSecondTurn(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	setValidAPIKey(t)

	dir := t.TempDir()
	argsLog := filepath.Join(dir, "args.log")
	bin := newKiroCLI(t, dir, chatParams{Stdout: "PONG", Stderr: creditsLine, ArgsLogPath: argsLog})
	adapter, session, state := mustStartSession(t, bin)

	events1, result1, err := runChatTurn(t, adapter, session, "first")
	if err != nil {
		t.Fatalf("RunTurn(first) error = %v", err)
	}
	if result1.ExitReason != domain.EventTurnCompleted {
		t.Fatalf("RunTurn(first).ExitReason = %q, want %q", result1.ExitReason, domain.EventTurnCompleted)
	}
	if !state.resumeRequested {
		t.Fatal("state.resumeRequested = false after a successful turn, want true")
	}
	agenttest.AssertSessionIDContract(t, []string{session.ID}, events1, result1)

	if _, _, err := runChatTurn(t, adapter, session, "second"); err != nil {
		t.Fatalf("RunTurn(second) error = %v", err)
	}

	logged, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatalf("reading captured args: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(logged)), "\n")
	if len(lines) != 2 {
		t.Fatalf("captured chat invocations = %d, want 2; log = %q", len(lines), string(logged))
	}
	if strings.Contains(lines[0], "--resume") {
		t.Errorf("first turn args = %q, want no --resume flag", lines[0])
	}
	if !strings.Contains(lines[1], "--resume") {
		t.Errorf("second turn args = %q, want the --resume flag", lines[1])
	}
}

// TestOnFinalize_SecondTurnFailsAfterFirstTurnNonBlankStdout pins that a
// session's second turn, whose stdout carries only whitespace, reports
// turn_failed even though the first turn on the same session had a
// non-blank transcript line.
func TestOnFinalize_SecondTurnFailsAfterFirstTurnNonBlankStdout(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	setValidAPIKey(t)

	dir := t.TempDir()
	counterFile := filepath.Join(dir, "turn-count")
	bin := newKiroCLI(t, dir, chatParams{MarkerFile: counterFile, Stdout: "the answer is 42\n", MarkerStdout: "   \n"})

	adapter, session, _ := mustStartSession(t, bin)

	_, result1, err := runChatTurn(t, adapter, session, "first")
	if err != nil {
		t.Fatalf("RunTurn(first) error = %v", err)
	}
	if result1.ExitReason != domain.EventTurnCompleted {
		t.Fatalf("RunTurn(first).ExitReason = %q, want %q", result1.ExitReason, domain.EventTurnCompleted)
	}

	_, result2, err := runChatTurn(t, adapter, session, "second")
	if result2.ExitReason != domain.EventTurnFailed {
		t.Errorf("RunTurn(second).ExitReason = %q, want %q (a first turn's non-blank line must not carry forward)", result2.ExitReason, domain.EventTurnFailed)
	}
	requireAgentError(t, err, domain.ErrTurnFailed)
}

func TestRunTurn_NilOnEventPanics(t *testing.T) {
	t.Parallel()

	adapter, err := NewKiroAdapter(map[string]any{})
	if err != nil {
		t.Fatalf("NewKiroAdapter: %v", err)
	}

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("RunTurn with nil OnEvent did not panic")
		}
		if msg, ok := r.(string); !ok || !strings.Contains(msg, "kiro") {
			t.Errorf("panic value = %v, want a kiro-prefixed message", r)
		}
	}()

	_, _ = adapter.RunTurn(context.Background(), domain.Session{Internal: &sessionState{}}, domain.RunTurnParams{OnEvent: nil})
}

func TestRunTurn_WrongInternalType(t *testing.T) {
	t.Parallel()

	adapter, err := NewKiroAdapter(map[string]any{})
	if err != nil {
		t.Fatalf("NewKiroAdapter: %v", err)
	}

	_, err = adapter.RunTurn(context.Background(), domain.Session{Internal: "not a sessionState"}, domain.RunTurnParams{
		OnEvent: func(domain.AgentEvent) {},
	})
	requireAgentError(t, err, domain.ErrResponseError)
}

func TestStopSession_WrongInternalType(t *testing.T) {
	t.Parallel()

	adapter, err := NewKiroAdapter(map[string]any{})
	if err != nil {
		t.Fatalf("NewKiroAdapter: %v", err)
	}

	err = adapter.StopSession(context.Background(), domain.Session{Internal: 123})
	requireAgentError(t, err, domain.ErrResponseError)
}

func TestStopSession_NilForkSession(t *testing.T) {
	t.Parallel()

	adapter, err := NewKiroAdapter(map[string]any{})
	if err != nil {
		t.Fatalf("NewKiroAdapter: %v", err)
	}

	session := domain.Session{Internal: &sessionState{}}
	if err := adapter.StopSession(context.Background(), session); err != nil {
		t.Errorf("StopSession(nil forkSession) error = %v, want nil", err)
	}
}

func TestStartSession_WorkingSessionSkipsCredentialGuard(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	setValidAPIKey(t)

	bin := newKiroCLI(t, t.TempDir(), chatParams{WhoamiExitCode: 1})
	adapter, err := NewKiroAdapter(map[string]any{})
	if err != nil {
		t.Fatalf("NewKiroAdapter: %v", err)
	}

	_, err = adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: bin},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v, want nil for a working session", err)
	}
}

func TestStartSession_CredentialVerificationGuard(t *testing.T) {
	var listingFailureLogs bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&listingFailureLogs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(orig) })

	tests := []struct {
		name           string
		whoamiExitCode int
		wantErr        bool
		wantMessage    string
	}{
		{
			name:           "exit 0 passes regardless of output content",
			whoamiExitCode: 0,
		},
		{
			name:           "non-zero exit fails with the exit status in the message",
			whoamiExitCode: 1,
			wantErr:        true,
			wantMessage:    "the agent runtime reports no usable credential: whoami exited with status 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// t.Setenv is incompatible with t.Parallel.
			bin := newKiroCLI(t, t.TempDir(), chatParams{WhoamiExitCode: tt.whoamiExitCode})
			adapter, err := NewKiroAdapter(map[string]any{})
			if err != nil {
				t.Fatalf("NewKiroAdapter: %v", err)
			}

			_, err = adapter.StartSession(context.Background(), domain.StartSessionParams{
				WorkspacePath:          t.TempDir(),
				AgentConfig:            domain.AgentConfig{Command: bin},
				CredentialVerification: true,
			})

			if !tt.wantErr {
				if err != nil {
					t.Fatalf("StartSession() error = %v, want nil", err)
				}
				return
			}
			requireAgentError(t, err, domain.ErrCredentialUnverified)
			var agentErr *domain.AgentError
			if errors.As(err, &agentErr) && agentErr.Message != tt.wantMessage {
				t.Errorf("AgentError.Message = %q, want %q", agentErr.Message, tt.wantMessage)
			}
		})
	}

	if !strings.Contains(listingFailureLogs.String(), "component=kiro-adapter") {
		t.Errorf("listing-failure Warn = %q, want component=kiro-adapter", listingFailureLogs.String())
	}
}

func TestStartSession_BinaryNotFound(t *testing.T) {
	t.Parallel()

	adapter, err := NewKiroAdapter(map[string]any{})
	if err != nil {
		t.Fatalf("NewKiroAdapter: %v", err)
	}

	_, err = adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: "sortie-nonexistent-kiro-12345"},
	})
	requireAgentError(t, err, domain.ErrAgentNotFound)
}

func TestStartSession_InvalidWorkspace(t *testing.T) {
	t.Parallel()

	adapter, err := NewKiroAdapter(map[string]any{})
	if err != nil {
		t.Fatalf("NewKiroAdapter: %v", err)
	}

	_, err = adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: "/nonexistent/sortie-kiro-test-path-12345",
		AgentConfig:   domain.AgentConfig{Command: "kiro-cli"},
	})
	requireAgentError(t, err, domain.ErrInvalidWorkspaceCwd)
}

func TestCheckCredential_SuccessYieldsNilError(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	cli := newKiroCLI(t, t.TempDir(), chatParams{})
	target := agentcore.LaunchTarget{Command: cli, WorkspacePath: ws}

	if agentErr := checkCredential(context.Background(), target, 5000); agentErr != nil {
		t.Fatalf("checkCredential() error = %v, want nil", agentErr)
	}
}

func TestListWorkspaceConversations_SuccessYieldsNilError(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	listing := []kiroSessionListGroup{{
		Cwd:      ws,
		Sessions: []kiroSessionListing{{SessionID: "abc", Source: "classic", Title: "t"}},
	}}
	data, err := json.Marshal(listing)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	cli := newKiroCLI(t, t.TempDir(), chatParams{Stdout: string(data)})
	target := agentcore.LaunchTarget{Command: cli, WorkspacePath: ws}

	sessions, err := listWorkspaceConversations(context.Background(), target, 5*time.Second, 5000)
	if err != nil {
		t.Fatalf("listWorkspaceConversations() error = %v, want nil", err)
	}
	if len(sessions) != 1 {
		t.Errorf("listWorkspaceConversations() returned %d sessions, want 1", len(sessions))
	}
}

func TestDeleteConversation_SuccessYieldsNilError(t *testing.T) {
	t.Parallel()

	ws := t.TempDir()
	cli := newKiroCLI(t, t.TempDir(), chatParams{})
	target := agentcore.LaunchTarget{Command: cli, WorkspacePath: ws}

	if err := deleteConversation(context.Background(), target, "abc", 5*time.Second, 5000); err != nil {
		t.Fatalf("deleteConversation() error = %v, want nil", err)
	}
}

// The whoami guard accepts any key, so a refused credential surfaces
// only as a turn with no credits trailer.
func TestCredentialVerification(t *testing.T) {
	// Not parallel: t.Setenv carries the fake ssh stand-in on PATH.
	verifiedBin := newKiroCLI(t, t.TempDir(), chatParams{Stderr: creditsLine})
	unverifiedBin := newKiroCLI(t, t.TempDir(), chatParams{})

	adapter, err := NewKiroAdapter(map[string]any{})
	if err != nil {
		t.Fatalf("NewKiroAdapter: %v", err)
	}

	credentialtest.AssertCredentialVerification(t, "kiro", credentialtest.RuntimeCases(t, adapter, domain.AgentConfig{}, verifiedBin, unverifiedBin, "kiro-cli"))
}
