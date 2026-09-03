//go:build unix

package kiro

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
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

// fakeChatScript is the body of a fake kiro-cli that answers the StartSession
// "whoami" canary and, for any other invocation (the "chat" turn), replays a
// fixed stdout/stderr pair and exit code. Driving the real
// agentcore.ForkPerTurnSession with this binary exercises the adapter's
// OnFinalize and ParseLine closures end to end without a live kiro-cli.
//
// stdoutBody and stderrBody are written verbatim (no trailing newline added),
// preserving the exact bytes the fixtures specify.
func fakeChatScript(t *testing.T, dir, stdoutBody, stderrBody string, chatExit int) string {
	t.Helper()
	outFile := filepath.Join(dir, "chat_stdout")
	errFile := filepath.Join(dir, "chat_stderr")
	if err := os.WriteFile(outFile, []byte(stdoutBody), 0o644); err != nil {
		t.Fatalf("writing chat stdout fixture: %v", err)
	}
	if err := os.WriteFile(errFile, []byte(stderrBody), 0o644); err != nil {
		t.Fatalf("writing chat stderr fixture: %v", err)
	}

	body := fmt.Sprintf(`if [ "$1" = "whoami" ]; then
  printf '%%s\n' 'Authenticated with API key'
  exit 0
fi
cat '%s'
cat '%s' >&2
exit %d
`, outFile, errFile, chatExit)

	return agenttest.WriteScript(t, dir, "kiro-cli", body)
}

// fakeChatScriptWithArgsLog behaves exactly like fakeChatScript, except
// every non-whoami invocation appends its argument list to argsLog
// before replaying stdoutBody and stderrBody, so a test can inspect
// what buildArgs produced for each turn.
func fakeChatScriptWithArgsLog(t *testing.T, dir, stdoutBody, stderrBody string, chatExit int, argsLog string) string {
	t.Helper()
	outFile := filepath.Join(dir, "chat_stdout")
	errFile := filepath.Join(dir, "chat_stderr")
	if err := os.WriteFile(outFile, []byte(stdoutBody), 0o644); err != nil {
		t.Fatalf("writing chat stdout fixture: %v", err)
	}
	if err := os.WriteFile(errFile, []byte(stderrBody), 0o644); err != nil {
		t.Fatalf("writing chat stderr fixture: %v", err)
	}

	body := fmt.Sprintf(`if [ "$1" = "whoami" ]; then
  printf '%%s\n' 'Authenticated with API key'
  exit 0
fi
printf '%%s\n' "$*" >> '%s'
cat '%s'
cat '%s' >&2
exit %d
`, argsLog, outFile, errFile, chatExit)

	return agenttest.WriteScript(t, dir, "kiro-cli", body)
}

// setValidAPIKey sets a usable KIRO_API_KEY for the test. It is incompatible
// with t.Parallel because t.Setenv mutates process state.
func setValidAPIKey(t *testing.T) {
	t.Helper()
	t.Setenv("KIRO_API_KEY", "kiro-test-key")
}

// mustStartSession starts a Kiro session against the given fake binary and
// returns the adapter, the session, and the adapter-internal state. The caller
// must have set a valid KIRO_API_KEY so the whoami canary passes.
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

// The observed success trailer and auth-failure line use the exact literals
// from the adapter research notes: stderr carries "▸ Credits: … • Time: …" on
// a turn that ran, and "Authentication failed." on an invalid-credential turn.
const (
	creditsLine  = "▸ Credits: 0.01 • Time: 1s\n"
	authFailLine = "Authentication failed. Your API key may be invalid or expired.\n"
)

func TestOnFinalize_SuccessWithCredits(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	setValidAPIKey(t)

	bin := fakeChatScript(t, t.TempDir(), "\x1b[38;5;141m> \x1b[0mPONG", creditsLine, 0)
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

func TestOnFinalize_AuthFailed(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	setValidAPIKey(t)

	bin := fakeChatScript(t, t.TempDir(), "", authFailLine, 0)
	adapter, session, state := mustStartSession(t, bin)

	events, result, err := runChatTurn(t, adapter, session, "ping")

	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("result.ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}
	requireAgentError(t, err, domain.ErrResponseError)
	if !hasEventType(events, domain.EventTurnFailed) {
		t.Error("EventTurnFailed not delivered for auth failure")
	}
	if result.Usage != (domain.TokenUsage{}) {
		t.Errorf("result.Usage = %+v, want zero TokenUsage", result.Usage)
	}
	if state.resumeRequested {
		t.Error("state.resumeRequested = true, want false after an auth-failed turn")
	}

	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		Terminal:          agentcore.TerminalFailure,
		TerminalErrorKind: domain.ErrResponseError,
		TerminalMessage:   "kiro authentication failed",
		ExitObserved:      true,
		ExitCode:          0,
		Work:              agentcore.WorkUnobservable,
		WorkDetail:        "no credits trailer on stderr",
	}, result, err)
}

func TestOnFinalize_ExitZeroNoSignal(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	setValidAPIKey(t)

	// Exit 0 with neither the credits trailer nor the auth-failure line: a
	// bare exit 0 is never a success.
	bin := fakeChatScript(t, t.TempDir(), "some transcript text", "a warning with no markers\n", 0)
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
	// credits-trailer detail, so an operator can grep one string across
	// every adapter and still see kiro's own signal.
	const wantMessage = "agent exited without producing output: no credits trailer on stderr"
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
		Work:         agentcore.WorkUnobservable,
		WorkDetail:   "no credits trailer on stderr",
	}, result, err)
}

func TestOnFinalize_NonZeroExit(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	setValidAPIKey(t)

	bin := fakeChatScript(t, t.TempDir(), "", "kiro: internal error\n", 1)
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

// TestOnFinalize_AuthLineWithStdoutIsNotAuthError verifies the auth-failure arm
// requires empty stdout: an auth line accompanied by transcript text falls
// through to the bare-exit-0 arm (ErrTurnFailed), not the auth arm.
func TestOnFinalize_AuthLineWithStdoutIsNotAuthError(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	setValidAPIKey(t)

	bin := fakeChatScript(t, t.TempDir(), "partial answer", authFailLine, 0)
	adapter, session, _ := mustStartSession(t, bin)

	_, result, err := runChatTurn(t, adapter, session, "ping")

	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("result.ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}
	requireAgentError(t, err, domain.ErrTurnFailed)

	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		ExitObserved: true,
		ExitCode:     0,
		Work:         agentcore.WorkUnobservable,
		WorkDetail:   "no credits trailer on stderr",
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
			bin := fakeChatScript(t, t.TempDir(), tt.stdoutBody, tt.stderrBody, tt.chatExit)
			adapter, session, _ := mustStartSession(t, bin)

			events, result, _ := runChatTurn(t, adapter, session, "ping")

			agenttest.AssertMeasurementAbsent(t, events, result)
		})
	}
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
	bin := fakeChatScriptWithArgsLog(t, dir, "PONG", creditsLine, 0, argsLog)
	adapter, session, state := mustStartSession(t, bin)

	_, result1, err := runChatTurn(t, adapter, session, "first")
	if err != nil {
		t.Fatalf("RunTurn(first) error = %v", err)
	}
	if result1.ExitReason != domain.EventTurnCompleted {
		t.Fatalf("RunTurn(first).ExitReason = %q, want %q", result1.ExitReason, domain.EventTurnCompleted)
	}
	if !state.resumeRequested {
		t.Fatal("state.resumeRequested = false after a successful turn, want true")
	}

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

func TestStartSession_MissingCredential(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	t.Setenv("KIRO_API_KEY", "")

	bin := fakeChatScript(t, t.TempDir(), "", "", 0)
	adapter, err := NewKiroAdapter(map[string]any{})
	if err != nil {
		t.Fatalf("NewKiroAdapter: %v", err)
	}

	_, err = adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: bin},
	})
	requireAgentError(t, err, domain.ErrResponseError)
}

func TestStartSession_InvalidCredential(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	t.Setenv("KIRO_API_KEY", "kiro-test-key")

	// whoami reports the auth-failure marker, so the canary must reject the key.
	dir := t.TempDir()
	bin := agenttest.WriteScript(t, dir, "kiro-cli", "printf '%s\\n' 'Authentication failed.'\nexit 0")

	adapter, err := NewKiroAdapter(map[string]any{})
	if err != nil {
		t.Fatalf("NewKiroAdapter: %v", err)
	}

	_, err = adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: bin},
	})
	requireAgentError(t, err, domain.ErrResponseError)
}

// TestStartSession_CanaryNonZeroExitIsResponseError locks the classification of
// the whoami-canary failure arm: when the canary binary resolves but its whoami
// invocation exits non-zero, the credential preflight returns ErrResponseError
// (a retryable credential/runtime problem), never ErrAgentNotFound. The binary
// here is already on PATH and resolvable, so a missing-agent classification
// would be wrong. The fake's whoami arm exits 1 to drive the canaryErr != nil
// branch; the 5s timeout path is intentionally not exercised.
func TestStartSession_CanaryNonZeroExitIsResponseError(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	setValidAPIKey(t)

	dir := t.TempDir()
	bin := agenttest.WriteScript(t, dir, "kiro-cli", `if [ "$1" = "whoami" ]; then
  exit 1
fi
exit 0`)

	adapter, err := NewKiroAdapter(map[string]any{})
	if err != nil {
		t.Fatalf("NewKiroAdapter: %v", err)
	}

	_, err = adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: bin},
	})
	requireAgentError(t, err, domain.ErrResponseError)
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
		AgentConfig:   domain.AgentConfig{Command: "/bin/sh"},
	})
	requireAgentError(t, err, domain.ErrInvalidWorkspaceCwd)
}

func TestBuildSSHRemoteCmd(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		remoteCommand string
		apiKey        string
		want          string
	}{
		{
			name:          "empty key returns command unchanged",
			remoteCommand: "kiro-cli",
			apiKey:        "",
			want:          "kiro-cli",
		},
		{
			name:          "key is prepended and shell-quoted",
			remoteCommand: "kiro-cli",
			apiKey:        "abc123",
			want:          "KIRO_API_KEY='abc123' kiro-cli",
		},
		{
			name:          "key with single quote is escaped",
			remoteCommand: "kiro-cli",
			apiKey:        "a'b",
			want:          `KIRO_API_KEY='a'\''b' kiro-cli`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := buildSSHRemoteCmd(tt.remoteCommand, tt.apiKey)
			if got != tt.want {
				t.Errorf("buildSSHRemoteCmd(%q, %q) = %q, want %q", tt.remoteCommand, tt.apiKey, got, tt.want)
			}
		})
	}
}

// TestStartSession_SSHModeInjectsKey verifies SSH mode skips the credential
// canary and injects KIRO_API_KEY inline into the remote command.
func TestStartSession_SSHModeInjectsKey(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel.
	t.Setenv("KIRO_API_KEY", "ssh-key-value")

	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh not available on PATH")
	}

	adapter, err := NewKiroAdapter(map[string]any{})
	if err != nil {
		t.Fatalf("NewKiroAdapter: %v", err)
	}

	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: "kiro-cli"},
		SSHHost:       "dev-host.example.com",
	})
	if err != nil {
		t.Fatalf("StartSession() (SSH mode) error = %v", err)
	}

	state := session.Internal.(*sessionState)
	if !strings.Contains(state.target.RemoteCommand, "KIRO_API_KEY='ssh-key-value'") {
		t.Errorf("state.target.RemoteCommand = %q, want it to inject the API key inline", state.target.RemoteCommand)
	}
	if !strings.Contains(state.target.RemoteCommand, "kiro-cli") {
		t.Errorf("state.target.RemoteCommand = %q, want it to retain the kiro-cli command", state.target.RemoteCommand)
	}
}
