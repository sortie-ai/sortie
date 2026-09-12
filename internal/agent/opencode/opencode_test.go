//go:build unix

package opencode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/agenttest/dispositiontest"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

// writeOpenCodeScript writes an executable shell script named fake-opencode
// in dir with the given body and returns its path.
func writeOpenCodeScript(t *testing.T, dir, body string) string {
	t.Helper()
	return agenttest.WriteScript(t, dir, "fake-opencode", body)
}

// mustStartSession starts a session with the given command or fatals.
func mustStartSession(t *testing.T, a domain.AgentAdapter, workDir, cmd string) domain.Session {
	t.Helper()
	session, err := a.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: workDir,
		AgentConfig:   domain.AgentConfig{Command: cmd},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	return session
}

func writeRunFixtureScript(t *testing.T, dir, fixtureName string) string {
	t.Helper()

	runPath := filepath.Join(dir, fixtureName)
	if err := os.WriteFile(runPath, loadFixture(t, fixtureName), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", fixtureName, err)
	}

	exportPath := filepath.Join(dir, "export.json")
	if err := os.WriteFile(exportPath, []byte(`{"messages":[]}`), 0o644); err != nil {
		t.Fatalf("WriteFile(export.json): %v", err)
	}

	body := `case "$1" in
  export) cat '` + exportPath + `'; exit 0;;
esac
cat '` + runPath + `'`

	return writeOpenCodeScript(t, dir, body)
}

// splitPermissionWarningFixture returns the two lines of
// testdata/permission_warning_then_error.txt: the plain-text permission
// warning the opencode runtime writes to stderr, and the tool_use JSON
// envelope it writes to stdout.
func splitPermissionWarningFixture(t *testing.T) (warningLine, stdoutLine string) {
	t.Helper()
	lines := bytes.Split(bytes.TrimRight(loadFixture(t, "permission_warning_then_error.txt"), "\n"), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("permission_warning_then_error.txt has %d lines, want 2", len(lines))
	}
	return string(lines[0]), string(lines[1])
}

// writeFixtureFile writes content to name inside dir, fataling on error.
func writeFixtureFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
	return path
}

// hasNotification reports whether events contains an EventNotification
// with exactly message.
func hasNotification(events []domain.AgentEvent, message string) bool {
	for _, e := range events {
		if e.Type == domain.EventNotification && e.Message == message {
			return true
		}
	}
	return false
}

// collectEvents runs a turn and collects all emitted events.
func collectEvents(t *testing.T, a domain.AgentAdapter, session domain.Session, prompt string) ([]domain.AgentEvent, domain.TurnResult, error) {
	t.Helper()
	var events []domain.AgentEvent
	result, err := a.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt: prompt,
		OnEvent: func(e domain.AgentEvent) {
			events = append(events, e)
		},
	})
	return events, result, err
}

func TestNewOpenCodeAdapter(t *testing.T) {
	t.Parallel()

	a, err := NewOpenCodeAdapter(map[string]any{})
	if err != nil {
		t.Fatalf("NewOpenCodeAdapter() error = %v", err)
	}
	if a == nil {
		t.Fatal("adapter is nil")
	}
	if _, ok := a.(*OpenCodeAdapter); !ok {
		t.Errorf("adapter type = %T, want *OpenCodeAdapter", a)
	}
}

// TestNewOpenCodeAdapter_OverlapErrorMatchesValidateConfig asserts that
// NewOpenCodeAdapter's constructor refusal and validateConfig's offline
// diagnostic report byte-identical text for the same overlapping
// configuration, so the two surfaces can never disagree.
func TestNewOpenCodeAdapter_OverlapErrorMatchesValidateConfig(t *testing.T) {
	t.Parallel()

	config := map[string]any{
		"allowed_tools": []any{"bash"},
		"denied_tools":  []any{"bash"},
	}

	_, err := NewOpenCodeAdapter(config)
	if err == nil {
		t.Fatal("NewOpenCodeAdapter() error = nil, want overlap error")
	}

	diags := validateConfig(registry.AgentConfigFields{Kind: "opencode", Passthrough: config})
	diag := hasCheck(diags, "opencode.allowed_tools.overlap")
	if diag == nil {
		t.Fatalf("validateConfig() missing check %q; got %+v", "opencode.allowed_tools.overlap", diags)
	}

	if err.Error() != diag.Message {
		t.Errorf("NewOpenCodeAdapter() error = %q, validateConfig() diagnostic message = %q, want identical", err.Error(), diag.Message)
	}
}

func TestStartSession_InvalidWorkspace(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		params  domain.StartSessionParams
		wantErr domain.AgentErrorKind
	}{
		{
			name: "empty_workspace_path",
			params: domain.StartSessionParams{
				AgentConfig: domain.AgentConfig{Command: "/bin/sh"},
			},
			wantErr: domain.ErrInvalidWorkspaceCwd,
		},
		{
			name: "non_existent_workspace",
			params: domain.StartSessionParams{
				WorkspacePath: "/nonexistent/path/sortie-test-xyz",
				AgentConfig:   domain.AgentConfig{Command: "/bin/sh"},
			},
			wantErr: domain.ErrInvalidWorkspaceCwd,
		},
		{
			name: "command_not_found",
			params: domain.StartSessionParams{
				WorkspacePath: mustMakeTempDir(t),
				AgentConfig:   domain.AgentConfig{Command: "sortie-nonexistent-binary-opencode-xyz"},
			},
			wantErr: domain.ErrAgentNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			a, _ := NewOpenCodeAdapter(map[string]any{})
			_, err := a.StartSession(context.Background(), tt.params)
			if err == nil {
				t.Fatal("StartSession() error = nil, want error")
			}
			var agentErr *domain.AgentError
			if !errors.As(err, &agentErr) {
				t.Fatalf("error type = %T, want *domain.AgentError", err)
			}
			if agentErr.Kind != tt.wantErr {
				t.Errorf("Kind = %q, want %q", agentErr.Kind, tt.wantErr)
			}
		})
	}
}

func TestStartSession_ResumeSession(t *testing.T) {
	t.Parallel()

	a, _ := NewOpenCodeAdapter(map[string]any{})
	resumeID := "ses_resume123"
	session, err := a.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath:   t.TempDir(),
		AgentConfig:     domain.AgentConfig{Command: "/bin/sh"},
		ResumeSessionID: resumeID,
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	state := session.Internal.(*sessionState)
	if state.sessionID != resumeID {
		t.Errorf("sessionID = %q, want %q", state.sessionID, resumeID)
	}
	if session.ID != resumeID {
		t.Errorf("session.ID = %q, want %q", session.ID, resumeID)
	}
}

func TestStartSession_MCPConfigContent(t *testing.T) {
	t.Parallel()

	write := func(t *testing.T, content string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "mcp.json")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
		return path
	}

	const withServer = `{"mcpServers":{"sortie-tools":{"command":"/usr/local/bin/sortie","args":["mcp-server"]}}}`

	tests := []struct {
		name    string
		content string
		sshHost string
		want    bool
	}{
		{name: "local launch carries the document", content: withServer, want: true},
		{name: "remote launch carries none", content: withServer, sshHost: "build-host", want: false},
		{name: "no declared server carries none", content: `{"mcpServers":{}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			a, _ := NewOpenCodeAdapter(map[string]any{})
			session, err := a.StartSession(context.Background(), domain.StartSessionParams{
				WorkspacePath: t.TempDir(),
				AgentConfig:   domain.AgentConfig{Command: "/bin/sh"},
				MCPConfigPath: write(t, tt.content),
				SSHHost:       tt.sshHost,
			})
			if err != nil {
				t.Fatalf("StartSession() error = %v", err)
			}

			state, ok := session.Internal.(*sessionState)
			if !ok {
				t.Fatalf("session.Internal = %T, want *sessionState", session.Internal)
			}
			if got := state.mcpConfigContent != ""; got != tt.want {
				t.Errorf("StartSession() delivered content = %v, want %v (content %q)", got, tt.want, state.mcpConfigContent)
			}
		})
	}
}

func TestRunTurn_WrongInternalType(t *testing.T) {
	t.Parallel()

	a, _ := NewOpenCodeAdapter(map[string]any{})
	session := domain.Session{
		ID:       "test",
		Internal: "not-a-session-state",
	}

	_, err := a.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "work",
		OnEvent: func(_ domain.AgentEvent) {},
	})
	if err == nil {
		t.Fatal("RunTurn() error = nil, want error for wrong internal type")
	}
	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) {
		t.Fatalf("error type = %T, want *domain.AgentError", err)
	}
	if agentErr.Kind != domain.ErrResponseError {
		t.Errorf("Kind = %q, want %q", agentErr.Kind, domain.ErrResponseError)
	}
}

func TestRunTurn_ClosedSession(t *testing.T) {
	t.Parallel()

	a, _ := NewOpenCodeAdapter(map[string]any{})
	tmpDir := t.TempDir()
	session := mustStartSession(t, a, tmpDir, "/bin/sh")

	state := session.Internal.(*sessionState)
	state.mu.Lock()
	state.closed = true
	state.mu.Unlock()

	_, err := a.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "work",
		OnEvent: func(_ domain.AgentEvent) {},
	})
	if err == nil {
		t.Fatal("RunTurn() error = nil, want error for closed session")
	}
	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) {
		t.Fatalf("error type = %T, want *domain.AgentError", err)
	}
	if agentErr.Kind != domain.ErrResponseError {
		t.Errorf("Kind = %q, want %q", agentErr.Kind, domain.ErrResponseError)
	}
}

func TestRunTurn_ConcurrentRunRejected(t *testing.T) {
	t.Parallel()

	a, _ := NewOpenCodeAdapter(map[string]any{})
	tmpDir := t.TempDir()
	session := mustStartSession(t, a, tmpDir, "/bin/sh")

	state := session.Internal.(*sessionState)
	state.mu.Lock()
	state.active = &turnRuntime{
		waitCh: make(chan waitResult),
	}
	state.mu.Unlock()

	_, err := a.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "work",
		OnEvent: func(_ domain.AgentEvent) {},
	})
	if err == nil {
		t.Fatal("RunTurn() error = nil, want error for concurrent turn")
	}
	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) {
		t.Fatalf("error type = %T, want *domain.AgentError", err)
	}
	if agentErr.Kind != domain.ErrResponseError {
		t.Errorf("Kind = %q, want %q", agentErr.Kind, domain.ErrResponseError)
	}
}

func TestRunTurn_SessionIDMismatch(t *testing.T) {
	t.Parallel()

	a, _ := NewOpenCodeAdapter(map[string]any{})
	tmpDir := t.TempDir()
	script := writeRunFixtureScript(t, tmpDir, "simple_turn.jsonl")

	session, err := a.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath:   tmpDir,
		AgentConfig:     domain.AgentConfig{Command: script},
		ResumeSessionID: "ses_expected",
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	events, result, runErr := collectEvents(t, a, session, "work")
	if runErr == nil {
		t.Fatal("RunTurn() error = nil, want session mismatch error")
	}
	var agentErr *domain.AgentError
	if !errors.As(runErr, &agentErr) {
		t.Fatalf("error type = %T, want *domain.AgentError", runErr)
	}
	if agentErr.Kind != domain.ErrResponseError {
		t.Errorf("Kind = %q, want %q", agentErr.Kind, domain.ErrResponseError)
	}
	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}

	var mismatchCount int
	var mismatchMessage string
	for _, event := range events {
		if event.Type == domain.EventSessionStarted {
			t.Fatalf("unexpected session_started event for mismatched session: %+v", event)
		}
		if event.Type == domain.EventTurnFailed {
			mismatchCount++
			mismatchMessage = event.Message
			if !strings.Contains(event.Message, `expected "ses_expected"`) || !strings.Contains(event.Message, `got "ses_abc123"`) {
				t.Errorf("turn_failed message = %q, want mismatch details", event.Message)
			}
		}
	}
	if mismatchCount != 1 {
		t.Errorf("turn_failed count = %d, want 1", mismatchCount)
	}

	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		Terminal:          agentcore.TerminalFailure,
		TerminalErrorKind: domain.ErrResponseError,
		TerminalMessage:   mismatchMessage,
	}, result, runErr)
}

func TestStopSession_NoActiveTurn(t *testing.T) {
	t.Parallel()

	a, _ := NewOpenCodeAdapter(map[string]any{})
	tmpDir := t.TempDir()
	session := mustStartSession(t, a, tmpDir, "/bin/sh")

	if err := a.StopSession(context.Background(), session); err != nil {
		t.Fatalf("StopSession() error = %v, want nil", err)
	}
	// Double stop should also return nil.
	if err := a.StopSession(context.Background(), session); err != nil {
		t.Fatalf("StopSession() second call error = %v, want nil", err)
	}
}

func TestStopSession_ContextDeadline(t *testing.T) {
	t.Parallel()

	testCtx, testCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer testCancel()

	tmpDir := t.TempDir()
	script := writeOpenCodeScript(t, tmpDir, `case "$1" in
  export) echo '{"messages":[]}'; exit 0;;
esac
trap '' TERM
printf '{"type":"step_start","timestamp":1000,"sessionID":"ses_abc123","part":{"id":"p1","messageID":"m1","sessionID":"ses_abc123","snapshot":"","type":"step-start"}}\n'
while :; do sleep 1; done`)

	a, _ := NewOpenCodeAdapter(map[string]any{})
	session, err := a.StartSession(testCtx, domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	gotEvent := make(chan struct{}, 1)
	resultCh := make(chan domain.TurnResult, 1)
	errCh := make(chan error, 1)
	go func() {
		result, runErr := a.RunTurn(context.Background(), session, domain.RunTurnParams{
			Prompt: "work",
			OnEvent: func(_ domain.AgentEvent) {
				select {
				case gotEvent <- struct{}{}:
				default:
				}
			},
		})
		resultCh <- result
		errCh <- runErr
	}()

	select {
	case <-gotEvent:
	case <-testCtx.Done():
		t.Fatal("timed out waiting for first event")
	}

	stopCtx, stopCancel := context.WithTimeout(testCtx, 50*time.Millisecond)
	defer stopCancel()

	err = a.StopSession(stopCtx, session)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("StopSession() error = %v, want %v", err, context.DeadlineExceeded)
	}

	select {
	case result := <-resultCh:
		if result.ExitReason != domain.EventTurnCancelled {
			t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCancelled)
		}
		runErr := <-errCh
		var agentErr *domain.AgentError
		if !errors.As(runErr, &agentErr) || agentErr.Kind != domain.ErrTurnCancelled {
			t.Errorf("RunTurn() error = %v, want AgentError{Kind: %q}", runErr, domain.ErrTurnCancelled)
		}
	case <-testCtx.Done():
		t.Fatal("RunTurn did not return after StopSession timeout")
	}
}

// startTurnRuntimeProcess starts script as a real subprocess in its own
// process group and wires a minimal turnRuntime around it, wiring
// waitCh to close once the process is reaped. Used to exercise
// stopActiveTurn directly, without the rest of RunTurn's JSONL parsing.
// startTurnRuntimeProcess writes scriptBody with a leading trap
// statement, starts it, and waits for it to touch a readiness marker
// before returning, so the caller's subsequent SignalGraceful cannot
// race the shell installing its trap. scriptBody's first line MUST be
// a "trap ..." statement; the marker touch is inserted right after it.
func startTurnRuntimeProcess(t *testing.T, scriptBody string) *turnRuntime {
	t.Helper()

	dir := t.TempDir()
	readyPath := filepath.Join(dir, "ready")
	trapLine, rest, ok := strings.Cut(scriptBody, "\n")
	if !ok || !strings.HasPrefix(trapLine, "trap ") {
		t.Fatalf("startTurnRuntimeProcess: scriptBody must start with a trap statement, got %q", scriptBody)
	}
	script := trapLine + "\ntouch '" + readyPath + "'\n" + rest
	scriptPath := agenttest.WriteScript(t, dir, "agent.sh", script)

	cmd := exec.Command(scriptPath) //nolint:gosec // fixed path under t.TempDir()
	procutil.SetProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start() = %v", err)
	}
	t.Cleanup(func() { procutil.KillProcessGroup(cmd.Process.Pid) }) //nolint:errcheck // best-effort cleanup

	runtime := &turnRuntime{
		proc:   cmd.Process,
		waitCh: make(chan waitResult),
	}
	go func() {
		waitErr := cmd.Wait()
		runtime.waitMu.Lock()
		runtime.waitRes = waitResult{exitCode: procutil.ExtractExitCode(waitErr), err: waitErr}
		runtime.waitMu.Unlock()
		close(runtime.waitCh)
	}()

	waitForReady(t, readyPath)
	return runtime
}

// waitForReady polls until path exists, failing t after 5 seconds. Used
// to synchronize with a script that touches path only after installing
// a signal trap, so a caller's subsequent signal cannot race the trap
// installation.
func waitForReady(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("waitForReady(%q): marker did not appear within 5s", path)
}

// TestStopActiveTurn_GraceCoverage calls stopActiveTurn directly so the
// configured grace and its escalation log records can be asserted
// without spending the built-in five-second default in wall clock.
func TestStopActiveTurn_GraceCoverage(t *testing.T) {
	t.Parallel()

	t.Run("configured_grace_bounds_the_wait", func(t *testing.T) {
		t.Parallel()
		runtime := startTurnRuntimeProcess(t, `trap '' TERM
while :; do :; done`)

		start := time.Now()
		err := stopActiveTurn(context.Background(), runtime, 200*time.Millisecond, slog.Default())
		elapsed := time.Since(start)

		if err != nil {
			t.Errorf("stopActiveTurn() = %v, want nil", err)
		}
		if elapsed > 2*time.Second {
			t.Errorf("stopActiveTurn() force-terminated after %v, want well under the built-in 5s default (proves the configured 200ms grace bounded the wait, not DefaultStopGrace)", elapsed)
		}
	})

	t.Run("exit_inside_grace_emits_debug_and_no_warn", func(t *testing.T) {
		t.Parallel()
		runtime := startTurnRuntimeProcess(t, `trap 'exit 0' TERM
while :; do :; done`)
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		// The grace is far longer than this exit needs. The subtest proves
		// which record the clean-exit path emits, not that the grace bounds
		// anything, and a tight bound races the runner's scheduler instead
		// of testing the code.
		if err := stopActiveTurn(context.Background(), runtime, 30*time.Second, logger); err != nil {
			t.Errorf("stopActiveTurn() = %v, want nil", err)
		}

		output := buf.String()
		if !strings.Contains(output, "agent exited during the graceful phase") {
			t.Errorf("stopActiveTurn() did not log the exited-inside-grace Debug record: %s", output)
		}
		if !strings.Contains(output, `outcome=exited`) {
			t.Errorf("stopActiveTurn()'s Debug record missing outcome=exited: %s", output)
		}
		if strings.Contains(output, "level=WARN") {
			t.Errorf("stopActiveTurn() logged a Warn record for a clean exit, want none: %s", output)
		}
	})

	t.Run("grace_elapsed_emits_warn_with_outcome_and_grace", func(t *testing.T) {
		t.Parallel()
		runtime := startTurnRuntimeProcess(t, `trap '' TERM
while :; do :; done`)
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		if err := stopActiveTurn(context.Background(), runtime, 150*time.Millisecond, logger); err != nil {
			t.Errorf("stopActiveTurn() = %v, want nil", err)
		}

		output := buf.String()
		if !strings.Contains(output, "agent did not exit inside the graceful period and was force-terminated") {
			t.Errorf("stopActiveTurn() did not log the grace-elapsed Warn record: %s", output)
		}
		if !strings.Contains(output, `outcome="grace elapsed"`) {
			t.Errorf(`stopActiveTurn()'s Warn record missing outcome="grace elapsed": %s`, output)
		}
		if !strings.Contains(output, "grace=") {
			t.Errorf("stopActiveTurn()'s Warn record missing the configured grace ceiling: %s", output)
		}
		if !strings.Contains(output, "elapsed=") {
			t.Errorf("stopActiveTurn()'s Warn record missing the elapsed wait: %s", output)
		}
	})

	t.Run("caller_deadline_emits_warn_with_that_outcome", func(t *testing.T) {
		t.Parallel()
		runtime := startTurnRuntimeProcess(t, `trap '' TERM
while :; do :; done`)
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		if err := stopActiveTurn(ctx, runtime, 5*time.Second, logger); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("stopActiveTurn() = %v, want %v", err, context.DeadlineExceeded)
		}

		output := buf.String()
		if !strings.Contains(output, `outcome="caller deadline"`) {
			t.Errorf(`stopActiveTurn()'s Warn record missing outcome="caller deadline": %s`, output)
		}
	})
}

// TestRunTurn_CancelledTurnEscalatesOnConfiguredGrace asserts that a
// cancelled turn's process-group cancellation, wired through
// procutil.SetGroupCancel inside RunTurn, escalates on the session's
// configured grace rather than the built-in five-second default.
func TestRunTurn_CancelledTurnEscalatesOnConfiguredGrace(t *testing.T) {
	t.Parallel()

	testCtx, testCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer testCancel()

	tmpDir := t.TempDir()
	// A shell builtin busy-loop rather than a forked "sleep": a forked
	// child would inherit the shell's ignored TERM disposition across
	// exec and keep the stdout pipe open after os/exec's own WaitDelay
	// escalation kills only the direct child, hanging the reader
	// instead of exercising the bounded escalation this test measures.
	script := writeOpenCodeScript(t, tmpDir, `case "$1" in
  export) echo '{"messages":[]}'; exit 0;;
esac
trap '' TERM
printf '{"type":"step_start","timestamp":1000,"sessionID":"ses_abc123","part":{"id":"p1","messageID":"m1","sessionID":"ses_abc123","snapshot":"","type":"step-start"}}\n'
while :; do :; done`)

	a, _ := NewOpenCodeAdapter(map[string]any{})
	session, err := a.StartSession(testCtx, domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: script, StopGraceMS: 200},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	gotEvent := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, runErr := a.RunTurn(ctx, session, domain.RunTurnParams{
			Prompt: "work",
			OnEvent: func(_ domain.AgentEvent) {
				select {
				case gotEvent <- struct{}{}:
				default:
				}
			},
		})
		done <- runErr
	}()

	select {
	case <-gotEvent:
	case <-testCtx.Done():
		t.Fatal("timed out waiting for first event")
	}

	start := time.Now()
	cancel()

	var runErr error
	select {
	case runErr = <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("RunTurn did not return within 6s of cancellation")
	}
	elapsed := time.Since(start)

	var agentErr *domain.AgentError
	if !errors.As(runErr, &agentErr) || agentErr.Kind != domain.ErrTurnCancelled {
		t.Errorf("RunTurn() error = %v, want AgentError{Kind: %q}", runErr, domain.ErrTurnCancelled)
	}
	if elapsed > 2*time.Second {
		t.Errorf("RunTurn's cancelled-turn escalation took %v, want well under the built-in 5s default (proves the configured 200ms grace bounded cmd.WaitDelay, not DefaultStopGrace)", elapsed)
	}
}

func TestStopSession_WrongInternalType(t *testing.T) {
	t.Parallel()

	a, _ := NewOpenCodeAdapter(map[string]any{})
	session := domain.Session{
		ID:       "test",
		Internal: "not-a-session-state",
	}

	err := a.StopSession(context.Background(), session)
	if err == nil {
		t.Fatal("StopSession() error = nil, want error for wrong internal type")
	}
	if _, ok := errors.AsType[*domain.AgentError](err); !ok {
		t.Fatalf("error type = %T, want *domain.AgentError", err)
	}
}

// TestRunTurn_MultiTurnAccumulation drives two turns on one session
// where the underlying opencode export reports the session-cumulative
// total after each turn (100 output tokens after turn 1, 160 after
// turn 2), and asserts the adapter's run-cumulative snapshot after
// turn 2 reports 160 output tokens rather than the 100 a per-turn
// reset would leave in place.
func TestRunTurn_MultiTurnAccumulation(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	runFixture := loadFixture(t, "simple_turn.jsonl")
	runPath := filepath.Join(tmpDir, "run.jsonl")
	if err := os.WriteFile(runPath, runFixture, 0o644); err != nil {
		t.Fatal(err)
	}

	export1Path := filepath.Join(tmpDir, "export1.json")
	export1 := `{"messages":[{"info":{"role":"assistant","sessionID":"ses_abc123","providerID":"anthropic","modelID":"claude-sonnet-4-5","tokens":{"input":0,"output":100,"total":100,"cache":{"read":0,"write":0}}}}]}`
	if err := os.WriteFile(export1Path, []byte(export1), 0o644); err != nil {
		t.Fatal(err)
	}

	export2Path := filepath.Join(tmpDir, "export2.json")
	export2 := `{"messages":[` +
		`{"info":{"role":"assistant","sessionID":"ses_abc123","providerID":"anthropic","modelID":"claude-sonnet-4-5","tokens":{"input":0,"output":100,"total":100,"cache":{"read":0,"write":0}}}},` +
		`{"info":{"role":"assistant","sessionID":"ses_abc123","providerID":"anthropic","modelID":"claude-sonnet-4-5","tokens":{"input":0,"output":60,"total":60,"cache":{"read":0,"write":0}}}}` +
		`]}`
	if err := os.WriteFile(export2Path, []byte(export2), 0o644); err != nil {
		t.Fatal(err)
	}

	counterPath := filepath.Join(tmpDir, "export-call-count")
	script := writeOpenCodeScript(t, tmpDir, `case "$1" in
  export)
    if [ -f '`+counterPath+`' ]; then
      cat '`+export2Path+`'
    else
      touch '`+counterPath+`'
      cat '`+export1Path+`'
    fi
    exit 0
    ;;
esac
cat '`+runPath+`'`)

	a, _ := NewOpenCodeAdapter(map[string]any{})
	session := mustStartSession(t, a, tmpDir, script)

	var allEvents []domain.AgentEvent

	events1, result1, err := collectEvents(t, a, session, "first prompt")
	if err != nil {
		t.Fatalf("RunTurn (turn 1) error = %v", err)
	}
	if result1.Usage.OutputTokens != 100 {
		t.Errorf("turn 1: Usage.OutputTokens = %d, want 100", result1.Usage.OutputTokens)
	}
	allEvents = append(allEvents, events1...)

	events2, result2, err := collectEvents(t, a, session, "second prompt")
	if err != nil {
		t.Fatalf("RunTurn (turn 2) error = %v", err)
	}
	if result2.Usage.OutputTokens != 160 {
		t.Errorf("turn 2: Usage.OutputTokens = %d, want 160 (session-cumulative, not the max of the two turns)", result2.Usage.OutputTokens)
	}
	allEvents = append(allEvents, events2...)

	agenttest.AssertUsageContract(t, allEvents)
	agenttest.AssertModelReported(t, allEvents, "anthropic/claude-sonnet-4-5")
}

// TestRunTurn_UsageMeasuredPersistsAcrossCancelledTurn drives two turns
// on one session: the first turn's export yields a usage figure and
// measures the run, and the second turn is cancelled via context before
// it completes. It asserts the second turn's UsageMeasured stays true,
// since the measured latch lives in the session's TurnEndUsage rather
// than being derived per turn.
func TestRunTurn_UsageMeasuredPersistsAcrossCancelledTurn(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	runFixture := loadFixture(t, "simple_turn.jsonl")
	runPath := filepath.Join(tmpDir, "run.jsonl")
	if err := os.WriteFile(runPath, runFixture, 0o644); err != nil {
		t.Fatal(err)
	}

	exportPath := filepath.Join(tmpDir, "export.json")
	const export = `{"messages":[{"info":{"role":"assistant","sessionID":"ses_abc123","providerID":"anthropic","modelID":"claude-sonnet-4-5","tokens":{"input":10,"output":20,"total":30,"cache":{"read":0,"write":0}}}}]}`
	if err := os.WriteFile(exportPath, []byte(export), 0o644); err != nil {
		t.Fatal(err)
	}

	// The first invocation completes normally and leaves counterPath
	// behind; the second invocation, detecting it, emits one event and
	// blocks until the test cancels its turn's context.
	counterPath := filepath.Join(tmpDir, "turn-count")
	script := writeOpenCodeScript(t, tmpDir, `case "$1" in
  export) cat '`+exportPath+`'; exit 0;;
esac
if [ -f '`+counterPath+`' ]; then
  printf '{"type":"step_start","timestamp":1000,"sessionID":"ses_abc123","part":{"id":"p1","messageID":"m1","sessionID":"ses_abc123","snapshot":"","type":"step-start"}}\n'
  sleep 1000
fi
touch '`+counterPath+`'
cat '`+runPath+`'`)

	outerCtx, outerCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer outerCancel()

	a, _ := NewOpenCodeAdapter(map[string]any{})
	session, err := a.StartSession(outerCtx, domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	result1, err := a.RunTurn(outerCtx, session, domain.RunTurnParams{
		Prompt:  "first prompt",
		OnEvent: func(domain.AgentEvent) {},
	})
	if err != nil {
		t.Fatalf("RunTurn (turn 1) error = %v", err)
	}
	if !result1.UsageMeasured {
		t.Fatal("turn 1: UsageMeasured = false, want true")
	}

	turnCtx, turnCancel := context.WithCancel(outerCtx)
	gotEvent := make(chan struct{}, 1)
	resultCh := make(chan domain.TurnResult, 1)
	errCh := make(chan error, 1)
	go func() {
		result, runErr := a.RunTurn(turnCtx, session, domain.RunTurnParams{
			Prompt: "second prompt",
			OnEvent: func(domain.AgentEvent) {
				select {
				case gotEvent <- struct{}{}:
				default:
				}
			},
		})
		resultCh <- result
		errCh <- runErr
	}()

	select {
	case <-gotEvent:
	case <-outerCtx.Done():
		t.Fatal("timed out waiting for turn 2's first event")
	}
	turnCancel()

	select {
	case result2 := <-resultCh:
		if result2.ExitReason != domain.EventTurnCancelled {
			t.Errorf("turn 2: ExitReason = %q, want %q", result2.ExitReason, domain.EventTurnCancelled)
		}
		if !result2.UsageMeasured {
			t.Error("turn 2 (cancelled after a measured turn): UsageMeasured = false, want true")
		}
		<-errCh
	case <-outerCtx.Done():
		t.Fatal("RunTurn (turn 2) did not return after context cancel")
	}
}

func TestRunTurn_SessionStartedOnce(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	// Write fixture to a stable path the script can cat.
	fixture := loadFixture(t, "simple_turn.jsonl")
	fixturePath := filepath.Join(tmpDir, "output.jsonl")
	if err := os.WriteFile(fixturePath, fixture, 0o644); err != nil {
		t.Fatal(err)
	}

	script := writeOpenCodeScript(t, tmpDir, "cat '"+fixturePath+"'")

	a, _ := NewOpenCodeAdapter(map[string]any{})
	session := mustStartSession(t, a, tmpDir, script)

	countType := func(events []domain.AgentEvent, typ domain.AgentEventType) int {
		n := 0
		for _, e := range events {
			if e.Type == typ {
				n++
			}
		}
		return n
	}

	// First turn: session_started fires exactly once.
	turn1Events, result1, err := collectEvents(t, a, session, "first prompt")
	if err != nil {
		t.Fatalf("RunTurn (turn 1) error = %v", err)
	}
	if result1.ExitReason != domain.EventTurnCompleted {
		t.Errorf("turn 1 ExitReason = %q, want %q", result1.ExitReason, domain.EventTurnCompleted)
	}
	if n := countType(turn1Events, domain.EventSessionStarted); n != 1 {
		t.Errorf("turn 1: session_started count = %d, want 1", n)
	}

	// Second turn on the same session: session_started must not fire again.
	turn2Events, result2, err := collectEvents(t, a, session, "second prompt")
	if err != nil {
		t.Fatalf("RunTurn (turn 2) error = %v", err)
	}
	if result2.ExitReason != domain.EventTurnCompleted {
		t.Errorf("turn 2 ExitReason = %q, want %q", result2.ExitReason, domain.EventTurnCompleted)
	}
	if n := countType(turn2Events, domain.EventSessionStarted); n != 0 {
		t.Errorf("turn 2: session_started count = %d, want 0 (already opened)", n)
	}
}

func TestRunTurn_LogicalFailureExitZero(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := writeRunFixtureScript(t, tmpDir, "logical_failure_exit0.jsonl")

	a, _ := NewOpenCodeAdapter(map[string]any{})
	session := mustStartSession(t, a, tmpDir, script)

	events, result, err := collectEvents(t, a, session, "work")
	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}
	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != domain.ErrTurnFailed {
		t.Fatalf("RunTurn() error = %v, want AgentError{Kind: %q}", err, domain.ErrTurnFailed)
	}

	var turnFailedCount int
	var turnFailedMessage string
	for _, event := range events {
		if event.Type == domain.EventTurnEndedWithError {
			t.Fatalf("unexpected turn_ended_with_error event: %+v", event)
		}
		if event.Type == domain.EventTurnFailed {
			turnFailedCount++
			turnFailedMessage = event.Message
		}
	}
	if turnFailedCount != 1 {
		t.Errorf("turn_failed count = %d, want 1", turnFailedCount)
	}

	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		Terminal:          agentcore.TerminalFailure,
		TerminalErrorKind: domain.ErrTurnFailed,
		TerminalMessage:   turnFailedMessage,
		ExitObserved:      true,
		ExitCode:          0,
		Work:              agentcore.WorkAbsent,
		WorkDetail:        "no assistant output on the run stream",
	}, result, err)
}

// writeUnreachableModelsScript writes a fake opencode whose run stream is
// fixtureName and whose models subcommand always fails, so a reported detail
// that names the unknown model can only have come from the run stream.
func writeUnreachableModelsScript(t *testing.T, dir, fixtureName string) string {
	t.Helper()

	runPath := filepath.Join(dir, fixtureName)
	if err := os.WriteFile(runPath, loadFixture(t, fixtureName), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", fixtureName, err)
	}

	exportPath := filepath.Join(dir, "export.json")
	if err := os.WriteFile(exportPath, []byte(`{"messages":[]}`), 0o644); err != nil {
		t.Fatalf("WriteFile(export.json): %v", err)
	}

	body := `case "$1" in
  export) cat '` + exportPath + `'; exit 0;;
  models) exit 1;;
esac
cat '` + runPath + `'`

	return writeOpenCodeScript(t, dir, body)
}

// TestRunTurn_LogicalFailureDualError covers a failure that emits both the
// actionable diagnostic and opencode's masked placeholder on the run stream.
// Either order is possible, and the operator must see the diagnostic in both.
func TestRunTurn_LogicalFailureDualError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		fixture string
	}{
		{name: "diagnostic first", fixture: "logical_failure_dual_error.jsonl"},
		{name: "placeholder first", fixture: "logical_failure_dual_error_reversed.jsonl"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tmpDir := t.TempDir()
			script := writeUnreachableModelsScript(t, tmpDir, tt.fixture)

			a, _ := NewOpenCodeAdapter(map[string]any{"model": "nonexistent/nonexistent"})
			session := mustStartSession(t, a, tmpDir, script)

			events, result, err := collectEvents(t, a, session, "work")
			if result.ExitReason != domain.EventTurnFailed {
				t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
			}
			var agentErr *domain.AgentError
			if !errors.As(err, &agentErr) || agentErr.Kind != domain.ErrTurnFailed {
				t.Fatalf("RunTurn() error = %v, want AgentError{Kind: %q}", err, domain.ErrTurnFailed)
			}

			// Exactly one terminal event fires per turn: the two error
			// events collapse into a single turn_failed carrying the
			// detail, not the placeholder that shares the stream with it.
			var turnFailedMessages []string
			for _, event := range events {
				if event.Type == domain.EventTurnEndedWithError {
					t.Fatalf("unexpected turn_ended_with_error event: %+v", event)
				}
				if event.Type == domain.EventTurnFailed {
					turnFailedMessages = append(turnFailedMessages, event.Message)
				}
			}
			if len(turnFailedMessages) != 1 {
				t.Fatalf("turn_failed count = %d, want 1; messages = %q", len(turnFailedMessages), turnFailedMessages)
			}
			const wantMessage = "Model not found: nonexistent/nonexistent."
			if turnFailedMessages[0] != wantMessage {
				t.Errorf("turn_failed message = %q, want %q (the diagnostic from the run stream)", turnFailedMessages[0], wantMessage)
			}

			dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
				Terminal:          agentcore.TerminalFailure,
				TerminalErrorKind: domain.ErrTurnFailed,
				TerminalMessage:   turnFailedMessages[0],
				ExitObserved:      true,
				ExitCode:          0,
				Work:              agentcore.WorkAbsent,
				WorkDetail:        "no assistant output on the run stream",
			}, result, err)
		})
	}
}

// writeMaskedRunScript writes a fake opencode whose run stream emits only the
// masked generic server error and whose models subcommand runs modelsCase.
func writeMaskedRunScript(t *testing.T, dir, modelsCase string) string {
	t.Helper()

	runPath := filepath.Join(dir, "logical_failure_masked_error.jsonl")
	if err := os.WriteFile(runPath, loadFixture(t, "logical_failure_masked_error.jsonl"), 0o644); err != nil {
		t.Fatalf("WriteFile(logical_failure_masked_error.jsonl): %v", err)
	}

	exportPath := filepath.Join(dir, "export.json")
	if err := os.WriteFile(exportPath, []byte(`{"messages":[]}`), 0o644); err != nil {
		t.Fatalf("WriteFile(export.json): %v", err)
	}

	body := `case "$1" in
  export) cat '` + exportPath + `'; exit 0;;
  models) ` + modelsCase + `;;
esac
cat '` + runPath + `'`

	return writeOpenCodeScript(t, dir, body)
}

func collectTurnFailedMessages(events []domain.AgentEvent) []string {
	var messages []string
	for _, event := range events {
		if event.Type == domain.EventTurnFailed {
			messages = append(messages, event.Message)
		}
	}
	return messages
}

// turnFailedEvents returns every turn_failed event in events, in order.
func turnFailedEvents(events []domain.AgentEvent) []domain.AgentEvent {
	var out []domain.AgentEvent
	for _, e := range events {
		if e.Type == domain.EventTurnFailed {
			out = append(out, e)
		}
	}
	return out
}

func TestRunTurn_MaskedErrorRecoversModelNotFound(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := writeMaskedRunScript(t, tmpDir, `printf 'opencode/big-pickle\nanthropic/claude-sonnet-4-6\n'; exit 0`)

	a, _ := NewOpenCodeAdapter(map[string]any{"model": "nonexistent/nonexistent"})
	session := mustStartSession(t, a, tmpDir, script)

	events, result, err := collectEvents(t, a, session, "work")
	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}
	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != domain.ErrTurnFailed {
		t.Fatalf("RunTurn() error = %v, want AgentError{Kind: %q}", err, domain.ErrTurnFailed)
	}

	// Exactly one terminal event fires per turn even when the
	// masked-model recovery succeeds: the recovered detail replaces the
	// masked relay message in place, rather than the two-event trail
	// this arm produced before the shared decision.
	messages := collectTurnFailedMessages(events)
	if len(messages) != 1 {
		t.Fatalf("turn_failed count = %d, want 1 (the recovered detail replaces the masked relay), messages=%q", len(messages), messages)
	}
	const wantMessage = "Model not found: nonexistent/nonexistent"
	if messages[0] != wantMessage {
		t.Errorf("turn_failed message = %q, want %q", messages[0], wantMessage)
	}

	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		Terminal:          agentcore.TerminalFailure,
		TerminalErrorKind: domain.ErrTurnFailed,
		TerminalMessage:   wantMessage,
		ExitObserved:      true,
		ExitCode:          0,
		Work:              agentcore.WorkAbsent,
		WorkDetail:        "no assistant output on the run stream",
	}, result, err)
}

func TestRunTurn_MaskedErrorModelListed(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := writeMaskedRunScript(t, tmpDir, `printf 'opencode/big-pickle\nexisting/model\n'; exit 0`)

	a, _ := NewOpenCodeAdapter(map[string]any{"model": "existing/model"})
	session := mustStartSession(t, a, tmpDir, script)

	events, result, err := collectEvents(t, a, session, "work")
	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}
	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != domain.ErrTurnFailed {
		t.Fatalf("RunTurn() error = %v, want AgentError{Kind: %q}", err, domain.ErrTurnFailed)
	}

	messages := collectTurnFailedMessages(events)
	if len(messages) != 1 {
		t.Fatalf("turn_failed count = %d, want 1 (listed model must not be reported missing), messages=%q", len(messages), messages)
	}
	if !strings.Contains(messages[0], "Unexpected server error") {
		t.Errorf("turn_failed message = %q, want substring %q", messages[0], "Unexpected server error")
	}

	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		Terminal:          agentcore.TerminalFailure,
		TerminalErrorKind: domain.ErrTurnFailed,
		TerminalMessage:   messages[0],
		ExitObserved:      true,
		ExitCode:          0,
		Work:              agentcore.WorkAbsent,
		WorkDetail:        "no assistant output on the run stream",
	}, result, err)
}

func TestRunTurn_MaskedErrorModelsCommandFails(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := writeMaskedRunScript(t, tmpDir, `exit 1`)

	a, _ := NewOpenCodeAdapter(map[string]any{"model": "nonexistent/nonexistent"})
	session := mustStartSession(t, a, tmpDir, script)

	events, result, err := collectEvents(t, a, session, "work")
	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}
	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != domain.ErrTurnFailed {
		t.Fatalf("RunTurn() error = %v, want AgentError{Kind: %q}", err, domain.ErrTurnFailed)
	}

	messages := collectTurnFailedMessages(events)
	if len(messages) != 1 {
		t.Fatalf("turn_failed count = %d, want 1 (failed listing must not invent detail), messages=%q", len(messages), messages)
	}
	if !strings.Contains(messages[0], "Unexpected server error") {
		t.Errorf("turn_failed message = %q, want substring %q", messages[0], "Unexpected server error")
	}

	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		Terminal:          agentcore.TerminalFailure,
		TerminalErrorKind: domain.ErrTurnFailed,
		TerminalMessage:   messages[0],
		ExitObserved:      true,
		ExitCode:          0,
		Work:              agentcore.WorkAbsent,
		WorkDetail:        "no assistant output on the run stream",
	}, result, err)
}

func TestRunTurn_MaskedErrorNoModelConfigured(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	sentinel := filepath.Join(tmpDir, "models-invoked")
	script := writeMaskedRunScript(t, tmpDir, `touch '`+sentinel+`'; exit 0`)

	a, _ := NewOpenCodeAdapter(map[string]any{})
	session := mustStartSession(t, a, tmpDir, script)

	events, result, err := collectEvents(t, a, session, "work")
	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}
	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != domain.ErrTurnFailed {
		t.Fatalf("RunTurn() error = %v, want AgentError{Kind: %q}", err, domain.ErrTurnFailed)
	}

	messages := collectTurnFailedMessages(events)
	if len(messages) != 1 {
		t.Fatalf("turn_failed count = %d, want 1, messages=%q", len(messages), messages)
	}
	if _, statErr := os.Stat(sentinel); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("models subcommand was invoked without a configured model (sentinel stat err = %v)", statErr)
	}

	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		Terminal:          agentcore.TerminalFailure,
		TerminalErrorKind: domain.ErrTurnFailed,
		TerminalMessage:   messages[0],
		ExitObserved:      true,
		ExitCode:          0,
		Work:              agentcore.WorkAbsent,
		WorkDetail:        "no assistant output on the run stream",
	}, result, err)
}

func TestRunTurn_OversizedStdoutLine(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := writeOpenCodeScript(t, tmpDir, `head -c $((10*1024*1024+1)) /dev/zero | tr '\000' 'a'
printf '\n'`)

	a, _ := NewOpenCodeAdapter(map[string]any{})
	session := mustStartSession(t, a, tmpDir, script)

	events, result, err := collectEvents(t, a, session, "work")
	if err == nil {
		t.Fatal("RunTurn() error = nil, want oversized-line failure")
	}
	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) {
		t.Fatalf("error type = %T, want *domain.AgentError", err)
	}
	if agentErr.Kind != domain.ErrResponseError {
		t.Errorf("Kind = %q, want %q", agentErr.Kind, domain.ErrResponseError)
	}
	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}

	failedEvents := turnFailedEvents(events)
	if len(failedEvents) != 1 {
		t.Fatalf("turn_failed event count = %d, want 1", len(failedEvents))
	}
	if failedEvents[0].Message != "stdout read error" {
		t.Errorf("turn_failed Message = %q, want %q", failedEvents[0].Message, "stdout read error")
	}

	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		Terminal:          agentcore.TerminalFailure,
		TerminalErrorKind: domain.ErrResponseError,
		TerminalMessage:   "stdout read error",
	}, result, err)
}

func TestRunTurn_EventAgentPID(t *testing.T) {
	t.Parallel()

	t.Run("local_events_include_pid", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		script := writeRunFixtureScript(t, tmpDir, "simple_turn.jsonl")

		a, _ := NewOpenCodeAdapter(map[string]any{})
		session := mustStartSession(t, a, tmpDir, script)

		events, result, err := collectEvents(t, a, session, "work")
		if err != nil {
			t.Fatalf("RunTurn() error = %v", err)
		}
		if result.ExitReason != domain.EventTurnCompleted {
			t.Fatalf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
		}
		if len(events) == 0 {
			t.Fatal("events = 0, want > 0")
		}

		wantPID := events[0].AgentPID
		if wantPID == "" {
			t.Fatal("first event AgentPID is empty, want subprocess pid")
		}
		for _, event := range events {
			if event.AgentPID != wantPID {
				t.Errorf("event %q AgentPID = %q, want %q", event.Type, event.AgentPID, wantPID)
			}
		}
	})

	t.Run("ssh_events_leave_pid_empty", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		script := writeRunFixtureScript(t, tmpDir, "simple_turn.jsonl")

		a, _ := NewOpenCodeAdapter(map[string]any{})
		session, err := a.StartSession(context.Background(), domain.StartSessionParams{
			WorkspacePath: tmpDir,
			AgentConfig:   domain.AgentConfig{Command: "opencode"},
			SSHHost:       "example.test",
		})
		if err != nil {
			t.Fatalf("StartSession() error = %v", err)
		}
		state := session.Internal.(*sessionState)
		state.target.Command = script

		events, result, err := collectEvents(t, a, session, "work")
		if err != nil {
			t.Fatalf("RunTurn() error = %v", err)
		}
		if result.ExitReason != domain.EventTurnCompleted {
			t.Fatalf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
		}
		if len(events) == 0 {
			t.Fatal("events = 0, want > 0")
		}
		for _, event := range events {
			if event.AgentPID != "" {
				t.Errorf("event %q AgentPID = %q, want empty in ssh mode", event.Type, event.AgentPID)
			}
		}
	})
}

// TestRunTurn_UsageMeasured_AbsentWhenExportYieldsNoUsage verifies that a
// turn whose session export carries no usage figure (an empty messages
// list) reports the run unmeasured.
func TestRunTurn_UsageMeasured_AbsentWhenExportYieldsNoUsage(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := writeRunFixtureScript(t, tmpDir, "simple_turn.jsonl")

	a, _ := NewOpenCodeAdapter(map[string]any{})
	session := mustStartSession(t, a, tmpDir, script)

	events, result, err := collectEvents(t, a, session, "work")
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	agenttest.AssertMeasurementAbsent(t, events, result)

	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		ExitObserved: true,
		ExitCode:     0,
		Work:         agentcore.WorkPresent,
	}, result, err)
}

// TestRunTurn_UsageMeasured_TrueWhenExportYieldsUsage verifies that a
// turn whose session export carries a usage figure reports the run
// measured.
func TestRunTurn_UsageMeasured_TrueWhenExportYieldsUsage(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	runFixture := loadFixture(t, "simple_turn.jsonl")
	runPath := filepath.Join(tmpDir, "run.jsonl")
	if err := os.WriteFile(runPath, runFixture, 0o644); err != nil {
		t.Fatal(err)
	}

	exportPath := filepath.Join(tmpDir, "export.json")
	const export = `{"messages":[{"info":{"role":"assistant","sessionID":"ses_abc123","providerID":"anthropic","modelID":"claude-sonnet-4-5","tokens":{"input":10,"output":20,"total":30,"cache":{"read":0,"write":0}}}}]}`
	if err := os.WriteFile(exportPath, []byte(export), 0o644); err != nil {
		t.Fatal(err)
	}

	script := writeOpenCodeScript(t, tmpDir, `case "$1" in
  export) cat '`+exportPath+`'; exit 0;;
esac
cat '`+runPath+`'`)

	a, _ := NewOpenCodeAdapter(map[string]any{})
	session := mustStartSession(t, a, tmpDir, script)

	_, result, err := collectEvents(t, a, session, "work")
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	if !result.UsageMeasured {
		t.Error("RunTurn().UsageMeasured = false, want true when the session export yielded a usage figure")
	}
}

// TestAssertUsageReporting proves opencode's registered
// usage-reporting declaration (turn_end, per_model) against a real
// event stream: one usage figure, carrying the exported model,
// arriving after the run's tool_result, since finalizeExitedTurn
// queries the export only after the subprocess exits.
func TestAssertUsageReporting(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	runFixture := loadFixture(t, "tool_success.jsonl")
	runPath := filepath.Join(tmpDir, "run.jsonl")
	if err := os.WriteFile(runPath, runFixture, 0o644); err != nil {
		t.Fatal(err)
	}

	exportPath := filepath.Join(tmpDir, "export.json")
	exportFixture := loadFixture(t, "export_usage.json")
	if err := os.WriteFile(exportPath, exportFixture, 0o644); err != nil {
		t.Fatal(err)
	}

	script := writeOpenCodeScript(t, tmpDir, `case "$1" in
  export) cat '`+exportPath+`'; exit 0;;
esac
cat '`+runPath+`'`)

	a, _ := NewOpenCodeAdapter(map[string]any{})
	session := mustStartSession(t, a, tmpDir, script)

	events, result, err := collectEvents(t, a, session, "work")
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	agenttest.AssertUsageReporting(t, "opencode", []agenttest.UsageReportingCase{
		{Name: "one figure after the tool result, carrying the exported model", Events: events, Result: result},
	})
}

func TestRunTurn_ActivityVisibilityForStallWatchdog(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	// A "text" part carries the turn's own work evidence, keeping this
	// turn on the turn_completed row so the test still exercises
	// notification, malformed-event, and session-lifecycle visibility
	// during an otherwise-successful turn, rather than becoming a
	// duplicate of the dedicated zero-work-row pin.
	script := writeOpenCodeScript(t, tmpDir, `case "$1" in
  export) echo '{"messages":[]}'; exit 0;;
esac
printf '! permission requested: external_directory (/etc/*); auto-rejecting\n' >&2
printf '{"type":"step_start","timestamp":1000,"sessionID":"ses_visibility123","part":{"id":"p1","messageID":"m1","sessionID":"ses_visibility123","snapshot":"","type":"step-start"}}\n'
printf '{"type":"text","timestamp":1001,"sessionID":"ses_visibility123","part":{"id":"p3","messageID":"m1","sessionID":"ses_visibility123","type":"text","text":"working on it","time":{"start":1001,"end":1001}}}\n'
printf '{"type":"unknown_future_type","timestamp":1001,"sessionID":"ses_visibility123","data":"something"}\n'
printf '{"type":"step_finish","timestamp":1002,"sessionID":"ses_visibility123","part":{"id":"p2","messageID":"m1","sessionID":"ses_visibility123","type":"step-finish","reason":"stop"}}\n'`)

	a, _ := NewOpenCodeAdapter(map[string]any{})
	session := mustStartSession(t, a, tmpDir, script)

	events, result, err := collectEvents(t, a, session, "work")
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Fatalf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}

	var sawPermissionWarning bool
	var sawStepStarted bool
	var sawUnknownMalformed bool
	var sawStepFinished bool
	var sawSessionStarted bool
	var sawTurnCompleted bool

	for _, event := range events {
		switch event.Type {
		case domain.EventNotification:
			switch {
			case strings.HasPrefix(event.Message, "the agent runtime refused a permission request"):
				sawPermissionWarning = true
			case event.Message == "step started":
				sawStepStarted = true
			case event.Message == "step finished: stop":
				sawStepFinished = true
			}
		case domain.EventMalformed:
			if strings.Contains(event.Message, "unknown event type") {
				sawUnknownMalformed = true
			}
		case domain.EventSessionStarted:
			sawSessionStarted = true
		case domain.EventTurnCompleted:
			sawTurnCompleted = true
		}
	}

	if !sawPermissionWarning {
		t.Error("permission warning notification was not emitted")
	}
	if !sawStepStarted {
		t.Error("step_start notification was not emitted")
	}
	if !sawUnknownMalformed {
		t.Error("unknown JSON envelope did not emit malformed event")
	}
	if !sawStepFinished {
		t.Error("step_finish notification was not emitted")
	}
	if !sawSessionStarted {
		t.Error("session_started event was not emitted")
	}
	if !sawTurnCompleted {
		t.Error("turn_completed event was not emitted")
	}

	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		ExitObserved: true,
		ExitCode:     0,
		Work:         agentcore.WorkPresent,
	}, result, err)
}

// TestRunTurn_ReasoningPartCountsAsWork drives a run stream carrying a
// "reasoning" part and nothing else: no text, no tool_use. Reasoning is
// one of this adapter's two declared assistant-output feed points, so it
// alone must satisfy WorkObserver and the turn must complete as
// turn_completed rather than falling to the zero-work row.
func TestRunTurn_ReasoningPartCountsAsWork(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := writeOpenCodeScript(t, tmpDir, `case "$1" in
  export) echo '{"messages":[]}'; exit 0;;
esac
printf '{"type":"reasoning","timestamp":1000,"sessionID":"ses_reasoning123","part":{"id":"p1","messageID":"m1","sessionID":"ses_reasoning123","type":"reasoning","text":"thinking it through"}}\n'`)

	a, _ := NewOpenCodeAdapter(map[string]any{})
	session := mustStartSession(t, a, tmpDir, script)

	events, result, err := collectEvents(t, a, session, "work")
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Fatalf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}

	var sawReasoningBlock bool
	var sawTurnCompleted bool
	for _, event := range events {
		switch {
		case event.Type == domain.EventOtherMessage && event.Message == "reasoning block":
			sawReasoningBlock = true
		case event.Type == domain.EventTurnCompleted:
			sawTurnCompleted = true
		}
	}
	if !sawReasoningBlock {
		t.Error("reasoning block event was not emitted")
	}
	if !sawTurnCompleted {
		t.Error("turn_completed event was not emitted")
	}

	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		ExitObserved: true,
		ExitCode:     0,
		Work:         agentcore.WorkPresent,
	}, result, err)
}

// TestRunTurn_PermissionWarningRecognizedOnStderr drives the
// permission_warning_then_error.txt fixture split across the two real
// streams the opencode runtime uses: the denial warning on stderr, the
// tool_use envelope alone on stdout. Previously both lines
// arrived on stdout and the recognition branch that would have read the
// warning there was dead code, so a fixture that did not distinguish the
// streams let a test pass without exercising the refusal path at all. See
// TestRunTurn_PermissionWarningOnStdoutIsNotRecognized for the negative
// control this test alone cannot provide.
func TestRunTurn_PermissionWarningRecognizedOnStderr(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	warningLine, stdoutLine := splitPermissionWarningFixture(t)

	stdoutPath := writeFixtureFile(t, tmpDir, "stdout.jsonl", stdoutLine+"\n")
	stderrPath := writeFixtureFile(t, tmpDir, "stderr.txt", warningLine+"\n")
	exportPath := writeFixtureFile(t, tmpDir, "export.json", `{"messages":[]}`)

	script := writeOpenCodeScript(t, tmpDir, `case "$1" in
  export) cat '`+exportPath+`'; exit 0;;
esac
cat '`+stderrPath+`' >&2
cat '`+stdoutPath+`'`)

	a, _ := NewOpenCodeAdapter(map[string]any{})
	session := mustStartSession(t, a, tmpDir, script)

	events, result, err := collectEvents(t, a, session, "work")
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Fatalf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}

	wantNotice := agentcore.DecideHumanRequest(agentcore.ClassPermission, false, agentcore.AnswerRuntimeRefused).Notice
	if !hasNotification(events, wantNotice) {
		t.Errorf("events = %v, want an EventNotification with Message %q for the stderr-delivered permission warning", events, wantNotice)
	}
}

// TestRunTurn_PermissionWarningOnStdoutIsNotRecognized is the negative
// control for TestRunTurn_PermissionWarningRecognizedOnStderr: it delivers
// the same fixture's warning line on stdout instead of stderr, the shape
// the fixture previously modeled, and asserts the permission
// refusal notification is absent; the line is malformed input on stdout
// instead, since the dead stdout-side filter was removed. Together
// the two tests carry the guarantee that recognition fires on the stream
// the runtime actually uses and nowhere else: this one fails if
// recognition leaked back onto stdout or the fixture regressed to its old
// single-stream shape, and its counterpart fails if isPermissionWarning
// stopped firing on stderr at all.
func TestRunTurn_PermissionWarningOnStdoutIsNotRecognized(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	warningLine, stdoutLine := splitPermissionWarningFixture(t)

	stdoutPath := writeFixtureFile(t, tmpDir, "stdout.jsonl", warningLine+"\n"+stdoutLine+"\n")
	exportPath := writeFixtureFile(t, tmpDir, "export.json", `{"messages":[]}`)

	script := writeOpenCodeScript(t, tmpDir, `case "$1" in
  export) cat '`+exportPath+`'; exit 0;;
esac
cat '`+stdoutPath+`'`)

	a, _ := NewOpenCodeAdapter(map[string]any{})
	session := mustStartSession(t, a, tmpDir, script)

	events, _, err := collectEvents(t, a, session, "work")
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	wantNotice := agentcore.DecideHumanRequest(agentcore.ClassPermission, false, agentcore.AnswerRuntimeRefused).Notice
	if hasNotification(events, wantNotice) {
		t.Errorf("events = %v, unexpectedly contains the permission-refusal notification %q for a warning delivered on stdout", events, wantNotice)
	}

	var sawMalformed bool
	for _, e := range events {
		if e.Type == domain.EventMalformed && strings.Contains(e.Message, "permission requested") {
			sawMalformed = true
			break
		}
	}
	if !sawMalformed {
		t.Error("no EventMalformed for the stdout-delivered permission warning line")
	}
}

func TestRunTurn_TurnCancelledOnContextCancel(t *testing.T) {
	t.Parallel()

	outerCtx, outerCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer outerCancel()

	tmpDir := t.TempDir()
	// Script: emit one JSON event on a run call, then block until killed.
	// Handle export subcommand immediately so queryExportUsage doesn't block.
	script := writeOpenCodeScript(t, tmpDir, `case "$1" in
  export) echo '{"messages":[]}'; exit 0;;
esac
printf '{"type":"step_start","timestamp":1000,"sessionID":"ses_abc123","part":{"id":"p1","messageID":"m1","sessionID":"ses_abc123","snapshot":"","type":"step-start"}}\n'
sleep 1000`)

	a, _ := NewOpenCodeAdapter(map[string]any{})
	session, err := a.StartSession(outerCtx, domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	// turnCtx is the context this test cancels to trigger TurnCancelled.
	turnCtx, turnCancel := context.WithCancel(outerCtx)

	gotEvent := make(chan struct{}, 1)
	resultCh := make(chan domain.TurnResult, 1)
	errCh := make(chan error, 1)
	go func() {
		result, runErr := a.RunTurn(turnCtx, session, domain.RunTurnParams{
			Prompt: "work",
			OnEvent: func(_ domain.AgentEvent) {
				select {
				case gotEvent <- struct{}{}:
				default:
				}
			},
		})
		resultCh <- result
		errCh <- runErr
	}()

	// Wait for the subprocess to emit the first event.
	select {
	case <-gotEvent:
	case <-outerCtx.Done():
		t.Fatal("timed out waiting for first event")
	}

	turnCancel()

	select {
	case result := <-resultCh:
		if result.ExitReason != domain.EventTurnCancelled {
			t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCancelled)
		}
		err := <-errCh
		var agentErr *domain.AgentError
		if !errors.As(err, &agentErr) || agentErr.Kind != domain.ErrTurnCancelled {
			t.Errorf("RunTurn() error = %v, want AgentError{Kind: %q}", err, domain.ErrTurnCancelled)
		}
	case <-outerCtx.Done():
		t.Fatal("RunTurn did not return after context cancel")
	}
}

func TestRunTurn_StopSessionUnblocksReader(t *testing.T) {
	t.Parallel()

	// testCtx bounds the assertion deadline; runCtx is separate so
	// ctx.Done() in RunTurn's main loop doesn't race with the test's
	// resultCh select.
	testCtx, testCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer testCancel()

	runCtx := t.Context()

	tmpDir := t.TempDir()
	// Script: emit one JSON event on a run call, then block until killed.
	// Handle export subcommand immediately so queryExportUsage doesn't block.
	script := writeOpenCodeScript(t, tmpDir, `case "$1" in
  export) echo '{"messages":[]}'; exit 0;;
esac
printf '{"type":"step_start","timestamp":1000,"sessionID":"ses_abc123","part":{"id":"p1","messageID":"m1","sessionID":"ses_abc123","snapshot":"","type":"step-start"}}\n'
sleep 1000`)

	a, _ := NewOpenCodeAdapter(map[string]any{})
	session, err := a.StartSession(testCtx, domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	gotEvent := make(chan struct{}, 1)
	resultCh := make(chan domain.TurnResult, 1)
	go func() {
		result, _ := a.RunTurn(runCtx, session, domain.RunTurnParams{
			Prompt: "work",
			OnEvent: func(_ domain.AgentEvent) {
				select {
				case gotEvent <- struct{}{}:
				default:
				}
			},
		})
		resultCh <- result
	}()

	// Wait for the subprocess to be active.
	select {
	case <-gotEvent:
	case <-testCtx.Done():
		t.Fatal("timed out waiting for first event")
	}

	if err := a.StopSession(testCtx, session); err != nil {
		t.Fatalf("StopSession() error = %v", err)
	}

	select {
	case result := <-resultCh:
		if result.ExitReason != domain.EventTurnCancelled {
			t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCancelled)
		}
	case <-testCtx.Done():
		t.Fatal("RunTurn did not return after StopSession")
	}
}

// mustMakeTempDir is a helper that returns a temporary directory path.
// Used in test table initialization where t.TempDir() cannot be called
// inside a struct literal.
func mustMakeTempDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// TestRunTurn_ExitZeroNoAssistantOutputPart pins that a process which
// exits 0 having parsed JSON events but never a text, reasoning, or
// tool_use part reports turn_failed, not the silent success this class
// of bug produced before the shared decision.
func TestRunTurn_ExitZeroNoAssistantOutputPart(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := writeOpenCodeScript(t, tmpDir, `case "$1" in
  export) echo '{"messages":[]}'; exit 0;;
esac
printf '{"type":"step_start","timestamp":1000,"sessionID":"ses_c1","part":{"id":"p1","messageID":"m1","sessionID":"ses_c1","snapshot":"","type":"step-start"}}\n'
printf '{"type":"step_finish","timestamp":1001,"sessionID":"ses_c1","part":{"id":"p2","messageID":"m1","sessionID":"ses_c1","type":"step-finish","reason":"stop"}}\n'`)

	a, _ := NewOpenCodeAdapter(map[string]any{})
	session := mustStartSession(t, a, tmpDir, script)

	events, result, err := collectEvents(t, a, session, "work")
	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}
	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != domain.ErrTurnFailed {
		t.Fatalf("RunTurn() error = %v, want AgentError{Kind: %q}", err, domain.ErrTurnFailed)
	}
	const wantMessage = "agent exited without producing output: no message from the agent and no tool call"
	if agentErr.Message != wantMessage {
		t.Errorf("AgentError.Message = %q, want %q", agentErr.Message, wantMessage)
	}

	failedEvents := turnFailedEvents(events)
	if len(failedEvents) != 1 {
		t.Fatalf("turn_failed event count = %d, want 1", len(failedEvents))
	}

	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		ExitObserved: true,
		ExitCode:     0,
		Work:         agentcore.WorkAbsent,
		WorkDetail:   "no message from the agent and no tool call",
	}, result, err)
}

// TestRunTurn_ExitZeroNoJSONEventAtAll pins an emptier variant of the
// no-output case: the process exits 0 having emitted nothing parseable
// at all. It routes to the same zero-work row rather than letting the
// adapter pre-classify a bare exit 0 as a success.
func TestRunTurn_ExitZeroNoJSONEventAtAll(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := writeOpenCodeScript(t, tmpDir, `case "$1" in
  export) echo '{"messages":[]}'; exit 0;;
esac
exit 0`)

	a, _ := NewOpenCodeAdapter(map[string]any{})
	session := mustStartSession(t, a, tmpDir, script)

	events, result, err := collectEvents(t, a, session, "work")
	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}
	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != domain.ErrTurnFailed {
		t.Fatalf("RunTurn() error = %v, want AgentError{Kind: %q}", err, domain.ErrTurnFailed)
	}
	const wantMessage = "agent exited without producing output: no message from the agent and no tool call"
	if agentErr.Message != wantMessage {
		t.Errorf("AgentError.Message = %q, want %q", agentErr.Message, wantMessage)
	}

	failedEvents := turnFailedEvents(events)
	if len(failedEvents) != 1 {
		t.Fatalf("turn_failed event count = %d, want 1", len(failedEvents))
	}

	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		ExitObserved: true,
		ExitCode:     0,
		Work:         agentcore.WorkAbsent,
		WorkDetail:   "no message from the agent and no tool call",
	}, result, err)
}

// TestRunTurn_NonZeroExitNoTerminalReport pins the non-zero-exit
// transport-class abort: the disposition, kind, and message pair are the
// same ones every adapter produces for this row.
func TestRunTurn_NonZeroExitNoTerminalReport(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := writeOpenCodeScript(t, tmpDir, `case "$1" in
  export) echo '{"messages":[]}'; exit 0;;
esac
printf '{"type":"step_start","timestamp":1000,"sessionID":"ses_nonzero","part":{"id":"p1","messageID":"m1","sessionID":"ses_nonzero","snapshot":"","type":"step-start"}}\n'
exit 7`)

	a, _ := NewOpenCodeAdapter(map[string]any{})
	session := mustStartSession(t, a, tmpDir, script)

	events, result, err := collectEvents(t, a, session, "work")
	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}
	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != domain.ErrPortExit {
		t.Fatalf("RunTurn() error = %v, want AgentError{Kind: %q}", err, domain.ErrPortExit)
	}
	if agentErr.Message != "exit code 7" {
		t.Errorf("AgentError.Message = %q, want %q", agentErr.Message, "exit code 7")
	}

	failedEvents := turnFailedEvents(events)
	if len(failedEvents) != 1 {
		t.Fatalf("turn_failed event count = %d, want 1", len(failedEvents))
	}
	if failedEvents[0].Message != "non-zero exit" {
		t.Errorf("turn_failed Message = %q, want %q", failedEvents[0].Message, "non-zero exit")
	}

	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		ExitObserved: true,
		ExitCode:     7,
		Work:         agentcore.WorkAbsent,
	}, result, err)
}

// TestRunTurn_ReadTimeoutBeforeFirstJSONEvent pins the read-timeout
// transport-class abort: the disposition, kind, and message pair are the
// same ones today's arm already produces.
func TestRunTurn_ReadTimeoutBeforeFirstJSONEvent(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := writeOpenCodeScript(t, tmpDir, `case "$1" in
  export) echo '{"messages":[]}'; exit 0;;
esac
sleep 5`)

	a, _ := NewOpenCodeAdapter(map[string]any{})
	session, err := a.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: script, ReadTimeoutMS: 100},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	events, result, err := collectEvents(t, a, session, "work")
	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}
	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != domain.ErrResponseTimeout {
		t.Fatalf("RunTurn() error = %v, want AgentError{Kind: %q}", err, domain.ErrResponseTimeout)
	}
	const wantMessage = "timed out waiting for first opencode json event"
	if agentErr.Message != wantMessage {
		t.Errorf("AgentError.Message = %q, want %q", agentErr.Message, wantMessage)
	}

	failedEvents := turnFailedEvents(events)
	if len(failedEvents) != 1 {
		t.Fatalf("turn_failed event count = %d, want 1", len(failedEvents))
	}

	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		Terminal:          agentcore.TerminalFailure,
		TerminalErrorKind: domain.ErrResponseTimeout,
		TerminalMessage:   wantMessage,
	}, result, err)
}

// TestRunTurn_CompletedTurnReturnsUntypedNilError pins that a completed
// turn's returned error interface is genuinely nil, not a typed-nil
// *domain.AgentError promoted to a non-nil error interface.
func TestRunTurn_CompletedTurnReturnsUntypedNilError(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := writeRunFixtureScript(t, tmpDir, "simple_turn.jsonl")

	a, _ := NewOpenCodeAdapter(map[string]any{})
	session := mustStartSession(t, a, tmpDir, script)

	_, result, err := collectEvents(t, a, session, "work")
	if result.ExitReason != domain.EventTurnCompleted {
		t.Fatalf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}
	if err != nil {
		t.Errorf("RunTurn() error = %v, want nil (not a typed-nil *domain.AgentError)", err)
	}
}

// TestRunTurn_ToolOnlyNoTerminalCompletes drives the committed
// tool_success.jsonl fixture, which carries a completed tool_use part and
// no text or reasoning part at all. opencode's normal path sets no
// terminal report, so DecideTurn genuinely consults Work here: reporting
// turn_completed is proof the observer's ToolActivity field fired from
// this shape of the committed fixture corpus, and it doubles as the
// tool-activity-only case among the DecideTurn conformance checks the
// committed fixture corpus exercises.
func TestRunTurn_ToolOnlyNoTerminalCompletes(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := writeRunFixtureScript(t, tmpDir, "tool_success.jsonl")

	a, _ := NewOpenCodeAdapter(map[string]any{})
	session := mustStartSession(t, a, tmpDir, script)

	events, result, err := collectEvents(t, a, session, "work")
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}

	dispositiontest.AssertWorkEvidenceConsistent(t, events, result, err)
}

// TestRunTurn_SecondTurnFailsAfterFirstTurnBothSignals pins that a
// session's second turn, whose stream carries neither declared signal,
// reports turn_failed even though the first turn on the same session
// carried both a text part and a completed tool_use part.
func TestRunTurn_SecondTurnFailsAfterFirstTurnBothSignals(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	counterFile := filepath.Join(tmpDir, "turn-count")
	script := writeOpenCodeScript(t, tmpDir, fmt.Sprintf(`case "$1" in
  export) echo '{"messages":[]}'; exit 0;;
esac
if [ -f '%s' ]; then
  printf '{"type":"step_start","timestamp":1000,"sessionID":"ses_both_then_none","part":{"id":"p1","messageID":"m1","sessionID":"ses_both_then_none","snapshot":"","type":"step-start"}}\n'
else
  touch '%s'
  printf '{"type":"text","timestamp":1000,"sessionID":"ses_both_then_none","part":{"id":"p1","messageID":"m1","sessionID":"ses_both_then_none","type":"text","text":"ok","time":{"start":1000,"end":1000}}}\n'
  printf '{"type":"tool_use","timestamp":1001,"sessionID":"ses_both_then_none","part":{"id":"p2","messageID":"m1","sessionID":"ses_both_then_none","type":"tool","tool":"read","callID":"call_both","state":{"status":"completed","input":{},"output":"ok","time":{"start":1001,"end":1001}}}}\n'
fi`, counterFile, counterFile))

	a, _ := NewOpenCodeAdapter(map[string]any{})
	session := mustStartSession(t, a, tmpDir, script)

	_, result1, err := collectEvents(t, a, session, "first")
	if err != nil {
		t.Fatalf("RunTurn(first) error = %v", err)
	}
	if result1.ExitReason != domain.EventTurnCompleted {
		t.Fatalf("RunTurn(first).ExitReason = %q, want %q", result1.ExitReason, domain.EventTurnCompleted)
	}

	_, result2, err := collectEvents(t, a, session, "second")
	if result2.ExitReason != domain.EventTurnFailed {
		t.Errorf("RunTurn(second).ExitReason = %q, want %q (a first turn with both signals must not carry forward)", result2.ExitReason, domain.EventTurnFailed)
	}
	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != domain.ErrTurnFailed {
		t.Errorf("RunTurn(second) error = %v, want AgentError{Kind: %q}", err, domain.ErrTurnFailed)
	}
}

// TestRunTurn_WorkPredicateIsPerTurn pins that the work predicate reads
// this turn's own parsed parts, not the run cumulative. The first turn
// produces a text part and completes; the second turn, on the same
// session, produces none and must fail rather than inherit the first
// turn's evidence.
func TestRunTurn_WorkPredicateIsPerTurn(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	counterFile := filepath.Join(tmpDir, "turn-count")
	script := writeOpenCodeScript(t, tmpDir, fmt.Sprintf(`case "$1" in
  export) echo '{"messages":[]}'; exit 0;;
esac
if [ -f '%s' ]; then
  printf '{"type":"step_start","timestamp":1000,"sessionID":"ses_work_pin","part":{"id":"p1","messageID":"m1","sessionID":"ses_work_pin","snapshot":"","type":"step-start"}}\n'
else
  touch '%s'
  printf '{"type":"text","timestamp":1000,"sessionID":"ses_work_pin","part":{"id":"p1","messageID":"m1","sessionID":"ses_work_pin","type":"text","text":"ok","time":{"start":1000,"end":1000}}}\n'
fi`, counterFile, counterFile))

	a, _ := NewOpenCodeAdapter(map[string]any{})
	session := mustStartSession(t, a, tmpDir, script)

	_, result1, err := collectEvents(t, a, session, "first")
	if err != nil {
		t.Fatalf("RunTurn(first) error = %v", err)
	}
	if result1.ExitReason != domain.EventTurnCompleted {
		t.Fatalf("RunTurn(first).ExitReason = %q, want %q", result1.ExitReason, domain.EventTurnCompleted)
	}

	_, result2, err := collectEvents(t, a, session, "second")
	if result2.ExitReason != domain.EventTurnFailed {
		t.Errorf("RunTurn(second).ExitReason = %q, want %q (per-turn work predicate must not carry the first turn's output forward)", result2.ExitReason, domain.EventTurnFailed)
	}
	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != domain.ErrTurnFailed {
		t.Errorf("RunTurn(second) error = %v, want AgentError{Kind: %q}", err, domain.ErrTurnFailed)
	}
}

// shellQuote wraps s in single quotes for embedding in a generated shell
// script, escaping any embedded single quote.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// writeEscapedHolderSpawn returns shell script text that starts a
// setsid-detached background job holding whichever standard streams
// redirect leaves unredirected, and blocks the leader until the job's
// own marker at pidFile proves its setsid() transition has already
// completed. Waiting on that self-written marker, rather than on the
// leader's own $!, is what keeps a later group-kill from racing the
// descendant's escape: $! only proves the job was forked, not that it
// has already left the process group, and setsid (without --fork) does
// not create a second process to wait for, so nothing else observes
// that transition from outside.
func writeEscapedHolderSpawn(pidFile, redirect string) string {
	return fmt.Sprintf(
		"setsid sh -c 'echo $$ > %s; sleep 3600' %s &\n"+
			"while [ ! -s %s ]; do sleep 0.01; done\n",
		pidFile, redirect, pidFile,
	)
}

// abandonmentWarnCount counts spy entries matching StdoutReader.Abandon's
// fixed WARN record.
func abandonmentWarnCount(spy *agenttest.LogSpy) int {
	var n int
	for _, e := range spy.Entries() {
		if e.Level == slog.LevelWarn && e.Msg == "agent stdout was not fully collected before the turn ended" {
			n++
		}
	}
	return n
}

// isOpenCodeTestZombie reports whether pid is a zombie by reading
// /proc/<pid>/stat. Returns false if the file cannot be read.
func isOpenCodeTestZombie(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	if i := strings.LastIndex(string(data), ")"); i >= 0 && i+2 < len(data) {
		return data[i+2] == 'Z'
	}
	return false
}

// assertOpenCodeProcessDead polls until pid is gone or a zombie, or
// fails t after timeout.
func assertOpenCodeProcessDead(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		if isOpenCodeTestZombie(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("process %d still alive after %v, want gone", pid, timeout)
}

// killEscapedGroupOnCleanup registers a best-effort SIGKILL of the
// process group led by the PID recorded in pidFile, so a setsid-escaped
// descendant this file's fixtures leave running does not survive past
// the test that started it. It tolerates a pidFile that never appears.
func killEscapedGroupOnCleanup(t *testing.T, pidFile string) {
	t.Helper()
	t.Cleanup(func() {
		data, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		pid, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
		if convErr != nil || pid <= 0 {
			return
		}
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	})
}

// readPIDFile reads a PID a script already wrote to path, fataling t if
// the file is missing or does not hold a valid positive PID.
func readPIDFile(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) = %v", path, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		t.Fatalf("readPIDFile(%q) = %q, want a valid PID", path, data)
	}
	return pid
}

// pollOpenCodePIDFile polls pidFile until it contains a valid positive
// PID, or fails t after timeout.
func pollOpenCodePIDFile(t *testing.T, pidFile string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			pid, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if convErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pollOpenCodePIDFile(%q): no valid PID after %v", pidFile, timeout)
	return 0
}

// writeOpenCodeInGroupDescendantScript builds a script whose direct
// child starts a long-running background job without redirecting its
// own standard output, so the job inherits the same pipe write end,
// writes the job's PID to pidFile, emits one step_start event carrying
// sessionID, and exits normally. The reaper's unconditional group kill
// is what ends the descendant once the turn returns.
func writeOpenCodeInGroupDescendantScript(t *testing.T, dir, pidFile, sessionID string) string {
	t.Helper()
	body := fmt.Sprintf(`case "$1" in
  export) echo '{"messages":[]}'; exit 0;;
esac
sleep 3600 &
printf '%%s\n' "$!" > %s
printf '{"type":"step_start","timestamp":1000,"sessionID":"%s","part":{"id":"p1","messageID":"m1","sessionID":"%s","snapshot":"","type":"step-start"}}\n'
exit 0
`, shellQuote(pidFile), sessionID, sessionID)
	return writeOpenCodeScript(t, dir, body)
}

// writeOpenCodeEscapedDescendantScript behaves like
// writeOpenCodeInGroupDescendantScript, except the background job is
// started through setsid, so it leaves the process group while still
// inheriting the standard-output handle and survives the group kill.
func writeOpenCodeEscapedDescendantScript(t *testing.T, dir, pidFile, sessionID string) string {
	t.Helper()
	body := fmt.Sprintf(`case "$1" in
  export) echo '{"messages":[]}'; exit 0;;
esac
%sprintf '{"type":"step_start","timestamp":1000,"sessionID":"%s","part":{"id":"p1","messageID":"m1","sessionID":"%s","snapshot":"","type":"step-start"}}\n'
exit 0
`, writeEscapedHolderSpawn(pidFile, "2>/dev/null"), sessionID, sessionID)
	return writeOpenCodeScript(t, dir, body)
}

// TestRunTurn_DescendantHoldsStdout drives the two scripts above through
// the normal waitCh/postExitC completion path (never through cancellation
// or StopSession) and asserts properties P1, P3, P5, and P7: the
// in-group descendant is dead once the turn returns and no abandonment
// record fires, because the reaper's unconditional group kill releases
// the reader before sessionState.drainGrace can fire; the escaped
// descendant survives, the turn still publishes within that bound, and
// exactly one abandonment record is emitted; and both variants publish
// the identical disposition and error.
func TestRunTurn_DescendantHoldsStdout(t *testing.T) {
	t.Parallel()

	t.Run("in-group descendant: no abandonment, descendant dies", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		pidFile := filepath.Join(tmpDir, "descendant.pid")
		script := writeOpenCodeInGroupDescendantScript(t, tmpDir, pidFile, "ses_ingroup")

		spy := &agenttest.LogSpy{}
		a, _ := NewOpenCodeAdapter(map[string]any{})
		session := mustStartSession(t, a, tmpDir, script)
		state := session.Internal.(*sessionState)
		state.baseLogger = slog.New(spy)
		state.drainGrace = 200 * time.Millisecond

		start := time.Now()
		_, result, err := collectEvents(t, a, session, "work")
		elapsed := time.Since(start)

		if elapsed > 3*time.Second {
			t.Errorf("RunTurn() took %v, want well under 3s", elapsed)
		}
		if result.ExitReason != domain.EventTurnFailed {
			t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
		}
		var agentErr *domain.AgentError
		if !errors.As(err, &agentErr) || agentErr.Kind != domain.ErrTurnFailed {
			t.Errorf("RunTurn() error = %v, want AgentError{Kind: %q}", err, domain.ErrTurnFailed)
		}

		assertOpenCodeProcessDead(t, readPIDFile(t, pidFile), 3*time.Second)

		if got := abandonmentWarnCount(spy); got != 0 {
			t.Errorf("abandonment WARN count = %d, want 0 (the group kill releases an in-group descendant before the bound fires)", got)
		}
	})

	t.Run("escaped descendant: exactly one abandonment record, turn still bounded", func(t *testing.T) {
		agenttest.RequireSetsid(t)
		t.Parallel()

		tmpDir := t.TempDir()
		pidFile := filepath.Join(tmpDir, "escaped.pid")
		killEscapedGroupOnCleanup(t, pidFile)
		script := writeOpenCodeEscapedDescendantScript(t, tmpDir, pidFile, "ses_ingroup")

		spy := &agenttest.LogSpy{}
		a, _ := NewOpenCodeAdapter(map[string]any{})
		session := mustStartSession(t, a, tmpDir, script)
		state := session.Internal.(*sessionState)
		state.baseLogger = slog.New(spy)
		state.drainGrace = 200 * time.Millisecond

		start := time.Now()
		_, result, err := collectEvents(t, a, session, "work")
		elapsed := time.Since(start)

		if elapsed > 3*time.Second {
			t.Errorf("RunTurn() took %v, want well under 3s (property P1: bounded by sessionState.drainGrace)", elapsed)
		}

		if got := abandonmentWarnCount(spy); got != 1 {
			t.Errorf("abandonment WARN count = %d, want exactly 1 (property P3)", got)
		}

		if result.ExitReason != domain.EventTurnFailed {
			t.Errorf("ExitReason = %q, want %q (same disposition as the unabandoned in-group variant, property P7)", result.ExitReason, domain.EventTurnFailed)
		}
		var agentErr *domain.AgentError
		if !errors.As(err, &agentErr) || agentErr.Kind != domain.ErrTurnFailed {
			t.Errorf("RunTurn() error = %v, want AgentError{Kind: %q} (property P7)", err, domain.ErrTurnFailed)
		}
	})
}

// TestRunTurn_EscapedStderrHolderKeepsLinesUnblocked drives an escaped
// descendant that holds only the standard-error handle open through a
// turn whose standard output completes normally, and asserts that
// startWait's post-reap stderr bound still runs and does not leave
// finalizeExitedTurn's stderrCollector.Lines() call blocked. This is the
// regression test the risk assessment names for rewriting
// opencode.startWait: the package's only Abandon call must survive the
// rewrite onto procutil.StartReaper.
func TestRunTurn_EscapedStderrHolderKeepsLinesUnblocked(t *testing.T) {
	agenttest.RequireSetsid(t)
	t.Parallel()

	tmpDir := t.TempDir()
	pidFile := filepath.Join(tmpDir, "stderr-holder.pid")
	killEscapedGroupOnCleanup(t, pidFile)
	script := writeOpenCodeScript(t, tmpDir, fmt.Sprintf(`case "$1" in
  export) echo '{"messages":[]}'; exit 0;;
esac
printf 'direct child stderr\n' >&2
%sprintf '{"type":"step_start","timestamp":1000,"sessionID":"ses_stderr_holder","part":{"id":"p1","messageID":"m1","sessionID":"ses_stderr_holder","snapshot":"","type":"step-start"}}\n'
printf '{"type":"text","timestamp":1001,"sessionID":"ses_stderr_holder","part":{"id":"p2","messageID":"m1","sessionID":"ses_stderr_holder","type":"text","text":"done","time":{"start":1001,"end":1001}}}\n'
exit 0
`, writeEscapedHolderSpawn(pidFile, ">/dev/null")))

	a, _ := NewOpenCodeAdapter(map[string]any{})
	session := mustStartSession(t, a, tmpDir, script)
	session.Internal.(*sessionState).drainGrace = 200 * time.Millisecond

	done := make(chan struct{})
	var result domain.TurnResult
	var runErr error
	go func() {
		defer close(done)
		_, result, runErr = collectEvents(t, a, session, "work")
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("RunTurn did not return within 3s; stderrCollector.Lines() may be blocked on the escaped stderr holder")
	}

	if runErr != nil {
		t.Errorf("RunTurn() error = %v, want nil", runErr)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}
}

// TestRunTurn_ContextCancellationArm_BoundedDrain exercises property P8
// on opencode's context-cancellation early-return arm: with a descendant
// holding the standard-output handle, the turn still publishes within
// sessionState.drainGrace and emits exactly one abandonment record; with
// no descendant, or one that dies with the group, it publishes without
// spending the bound and without the record.
func TestRunTurn_ContextCancellationArm_BoundedDrain(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		spawn         func(pidFile string) string
		requireSetsid bool
		wantAbandon   bool
		wantDead      bool
	}{
		{name: "no descendant"},
		{
			name:     "in-group descendant",
			spawn:    func(pidFile string) string { return "sleep 3600 &\nprintf '%s\\n' \"$!\" > " + pidFile + "\n" },
			wantDead: true,
		},
		{
			name:          "escaped descendant",
			spawn:         func(pidFile string) string { return writeEscapedHolderSpawn(pidFile, "2>/dev/null") },
			requireSetsid: true,
			wantAbandon:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.requireSetsid {
				agenttest.RequireSetsid(t)
			}
			t.Parallel()

			tmpDir := t.TempDir()
			pidFile := filepath.Join(tmpDir, "descendant.pid")
			killEscapedGroupOnCleanup(t, pidFile)
			spawn := ""
			if tt.spawn != nil {
				spawn = tt.spawn(pidFile)
			}
			script := writeOpenCodeScript(t, tmpDir, fmt.Sprintf(`case "$1" in
  export) echo '{"messages":[]}'; exit 0;;
esac
%sprintf '{"type":"step_start","timestamp":1000,"sessionID":"ses_ctxcancel","part":{"id":"p1","messageID":"m1","sessionID":"ses_ctxcancel","snapshot":"","type":"step-start"}}\n'
sleep 3600
`, spawn))

			spy := &agenttest.LogSpy{}
			a, _ := NewOpenCodeAdapter(map[string]any{})
			session := mustStartSession(t, a, tmpDir, script)
			state := session.Internal.(*sessionState)
			state.baseLogger = slog.New(spy)
			state.drainGrace = 200 * time.Millisecond

			ctx, cancel := context.WithCancel(context.Background())
			gotEvent := make(chan struct{}, 1)
			type outcome struct {
				result domain.TurnResult
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				result, runErr := a.RunTurn(ctx, session, domain.RunTurnParams{
					Prompt: "work",
					OnEvent: func(domain.AgentEvent) {
						select {
						case gotEvent <- struct{}{}:
						default:
						}
					},
				})
				done <- outcome{result, runErr}
			}()

			select {
			case <-gotEvent:
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for the first event")
			}

			start := time.Now()
			cancel()

			var got outcome
			select {
			case got = <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("RunTurn did not return within 3s of cancellation")
			}
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Errorf("RunTurn published %v after cancellation, want well under 2s", elapsed)
			}

			if got.result.ExitReason != domain.EventTurnCancelled {
				t.Errorf("ExitReason = %q, want %q", got.result.ExitReason, domain.EventTurnCancelled)
			}
			var agentErr *domain.AgentError
			if !errors.As(got.err, &agentErr) || agentErr.Kind != domain.ErrTurnCancelled {
				t.Errorf("RunTurn() error = %v, want AgentError{Kind: %q}", got.err, domain.ErrTurnCancelled)
			}

			wantCount := 0
			if tt.wantAbandon {
				wantCount = 1
			}
			if got := abandonmentWarnCount(spy); got != wantCount {
				t.Errorf("abandonment WARN count = %d, want %d", got, wantCount)
			}

			if tt.wantDead {
				assertOpenCodeProcessDead(t, readPIDFile(t, pidFile), 3*time.Second)
			}
		})
	}
}

// TestRunTurn_SessionMismatchArm_BoundedDrain exercises property P8 on
// opencode's session-mismatch early-return arm: with an escaped
// descendant holding the standard-output handle, the mismatch turn still
// publishes within sessionState.drainGrace and emits exactly one
// abandonment record.
func TestRunTurn_SessionMismatchArm_BoundedDrain(t *testing.T) {
	agenttest.RequireSetsid(t)
	t.Parallel()

	tmpDir := t.TempDir()
	pidFile := filepath.Join(tmpDir, "escaped.pid")
	killEscapedGroupOnCleanup(t, pidFile)
	script := writeOpenCodeScript(t, tmpDir, fmt.Sprintf(`case "$1" in
  export) echo '{"messages":[]}'; exit 0;;
esac
%sprintf '{"type":"step_start","timestamp":1000,"sessionID":"ses_mismatch","part":{"id":"p1","messageID":"m1","sessionID":"ses_mismatch","snapshot":"","type":"step-start"}}\n'
sleep 3600
`, writeEscapedHolderSpawn(pidFile, "2>/dev/null")))

	spy := &agenttest.LogSpy{}
	a, _ := NewOpenCodeAdapter(map[string]any{})
	session, err := a.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath:   tmpDir,
		AgentConfig:     domain.AgentConfig{Command: script},
		ResumeSessionID: "ses_expected",
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	state := session.Internal.(*sessionState)
	state.baseLogger = slog.New(spy)
	state.drainGrace = 300 * time.Millisecond

	start := time.Now()
	_, result, runErr := collectEvents(t, a, session, "work")
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Errorf("RunTurn() took %v after a session mismatch, want well under 2s (bounded by sessionState.drainGrace)", elapsed)
	}

	var agentErr *domain.AgentError
	if !errors.As(runErr, &agentErr) || agentErr.Kind != domain.ErrResponseError {
		t.Fatalf("RunTurn() error = %v, want AgentError{Kind: %q}", runErr, domain.ErrResponseError)
	}
	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}

	if got := abandonmentWarnCount(spy); got != 1 {
		t.Errorf("abandonment WARN count = %d, want exactly 1", got)
	}
}

// TestRunTurn_ReadTimeoutDoesNotFireAfterObservedExit covers guarantee
// O12 and the second half of property P8: agent.read_timeout_ms is
// configured shorter than the injected sessionState.drainGrace, and the
// subprocess exits without ever emitting a JSON event while an escaped
// descendant holds the output handle. The published disposition must be
// the exit-based one, never ErrResponseTimeout: the exit is observed and
// the read timer disarmed well before read_timeout_ms could elapse.
func TestRunTurn_ReadTimeoutDoesNotFireAfterObservedExit(t *testing.T) {
	agenttest.RequireSetsid(t)
	t.Parallel()

	tmpDir := t.TempDir()
	pidFile := filepath.Join(tmpDir, "escaped.pid")
	killEscapedGroupOnCleanup(t, pidFile)
	script := writeOpenCodeScript(t, tmpDir, fmt.Sprintf(`case "$1" in
  export) echo '{"messages":[]}'; exit 0;;
esac
%sexit 0
`, writeEscapedHolderSpawn(pidFile, "2>/dev/null")))

	a, _ := NewOpenCodeAdapter(map[string]any{})
	session, err := a.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: tmpDir,
		// The leader's own exit waits on the escaped descendant's marker
		// loop (writeEscapedHolderSpawn), polling every 10ms, so the
		// read timeout needs enough margin over that polling latency to
		// keep this deterministic rather than racing the scheduler.
		AgentConfig: domain.AgentConfig{Command: script, ReadTimeoutMS: 300},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	session.Internal.(*sessionState).drainGrace = 800 * time.Millisecond

	_, result, runErr := collectEvents(t, a, session, "work")

	var agentErr *domain.AgentError
	if errors.As(runErr, &agentErr) && agentErr.Kind == domain.ErrResponseTimeout {
		t.Fatalf("RunTurn() reported ErrResponseTimeout, want the exit-based disposition (guarantee O12): err = %v", runErr)
	}
	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("ExitReason = %q, want %q (exit 0, no output at all)", result.ExitReason, domain.EventTurnFailed)
	}
}

// writeOpenCodeLatchMoveScript builds the fixture the P8 latch-move case
// needs: a direct child that writes one standard-error line, writes
// nothing to standard output, and exits; and an escaped descendant that
// inherits the standard-output handle alone (never standard error too),
// blocks on gatePath, then writes one JSON event that is neither an
// error event nor a session-id mismatch, and exits.
func writeOpenCodeLatchMoveScript(t *testing.T, dir, gatePath, pidFile, sessionID string) string {
	t.Helper()
	descendant := fmt.Sprintf(
		"echo $$ > %s; "+
			"while [ ! -f %s ]; do sleep 0.02; done; "+
			`printf '{"type":"text","timestamp":1001,"sessionID":"%s","part":{"id":"p2","messageID":"m1","sessionID":"%s","type":"text","text":"done","time":{"start":1001,"end":1001}}}\n'`,
		pidFile, shellQuote(gatePath), sessionID, sessionID,
	)
	body := fmt.Sprintf(`case "$1" in
  export) echo '{"messages":[]}'; exit 0;;
esac
printf 'direct child stderr\n' >&2
setsid sh -c %s 2>/dev/null &
while [ ! -s %s ]; do sleep 0.01; done
exit 0
`, shellQuote(descendant), shellQuote(pidFile))
	return writeOpenCodeScript(t, dir, body)
}

// TestRunTurn_LatchSetDuringPostExitDrain covers P8's third case: a turn
// whose first JSON event is received only after the exit has been
// observed must still set the first-JSON latch, so finalizeExitedTurn
// does not re-emit the direct child's standard error at WARN on a turn
// that otherwise succeeded. The gate opens no earlier than
// agent.read_timeout_ms after the direct child has already exited and
// started the descendant, so by the time it opens the read timer has
// long been disarmed by the observed exit.
func TestRunTurn_LatchSetDuringPostExitDrain(t *testing.T) {
	agenttest.RequireSetsid(t)
	t.Parallel()

	tmpDir := t.TempDir()
	gatePath := filepath.Join(tmpDir, "gate")
	pidFile := filepath.Join(tmpDir, "descendant.pid")
	script := writeOpenCodeLatchMoveScript(t, tmpDir, gatePath, pidFile, "ses_latch")

	spy := &agenttest.LogSpy{}
	const readTimeoutMS = 150
	a, _ := NewOpenCodeAdapter(map[string]any{})
	session, err := a.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: script, ReadTimeoutMS: readTimeoutMS},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	state := session.Internal.(*sessionState)
	state.baseLogger = slog.New(spy)
	state.drainGrace = 2 * time.Second

	done := make(chan struct{})
	var result domain.TurnResult
	var runErr error
	go func() {
		defer close(done)
		_, result, runErr = collectEvents(t, a, session, "work")
	}()

	descendantPID := pollOpenCodePIDFile(t, pidFile, 5*time.Second)
	time.Sleep(readTimeoutMS * time.Millisecond)
	if err := os.WriteFile(gatePath, nil, 0o644); err != nil {
		t.Fatalf("WriteFile(gate) = %v", err)
	}
	t.Cleanup(func() { syscall.Kill(-descendantPID, syscall.SIGKILL) }) //nolint:errcheck // best-effort cleanup

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("RunTurn did not return within 3s of opening the gate")
	}

	var agentErr *domain.AgentError
	if errors.As(runErr, &agentErr) && agentErr.Kind == domain.ErrResponseTimeout {
		t.Fatalf("RunTurn() reported ErrResponseTimeout, want the exit-based disposition: err = %v", runErr)
	}
	_ = result

	for _, e := range spy.Entries() {
		if e.Level == slog.LevelWarn && e.Msg == "agent stderr" {
			t.Errorf("finalizeExitedTurn re-emitted the direct child's stderr at WARN, want none: the latch must be set from the post-exit JSON event: %+v", e)
		}
	}
}

// TestRunTurn_ReadTimeoutDoesNotFireWhileStderrBoundRuns drives the gap
// between the reap and the turn's published result. The wait goroutine
// reaps first and only then applies the stderr bound, so an escaped
// descendant holding the standard-error handle delays the result
// channel by the whole drain grace. A read timeout shorter than that
// delay fires inside the gap, on a subprocess that has already exited
// and been reaped, unless the timer is disarmed from the reap itself.
func TestRunTurn_ReadTimeoutDoesNotFireWhileStderrBoundRuns(t *testing.T) {
	agenttest.RequireSetsid(t)
	t.Parallel()

	tmpDir := t.TempDir()
	pidFile := filepath.Join(tmpDir, "stderr-holder-timeout.pid")
	killEscapedGroupOnCleanup(t, pidFile)
	script := writeOpenCodeScript(t, tmpDir, fmt.Sprintf(`case "$1" in
  export) echo '{"messages":[]}'; exit 0;;
esac
printf 'direct child stderr\n' >&2
%sexit 0
`, writeEscapedHolderSpawn(pidFile, ">/dev/null")))

	a, _ := NewOpenCodeAdapter(map[string]any{})
	session, err := a.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: script, ReadTimeoutMS: 200},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	// Longer than the read timeout, so the escaped holder keeps the
	// result channel shut for a window the timer would otherwise win.
	session.Internal.(*sessionState).drainGrace = 1500 * time.Millisecond

	_, result, runErr := collectEvents(t, a, session, "work")

	var agentErr *domain.AgentError
	if errors.As(runErr, &agentErr) && agentErr.Kind == domain.ErrResponseTimeout {
		t.Fatalf("RunTurn() reported ErrResponseTimeout on a reaped subprocess: "+
			"the read timer outlived the reap by the stderr bound; err = %v", runErr)
	}
	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("ExitReason = %q, want %q (exit 0, no output at all)", result.ExitReason, domain.EventTurnFailed)
	}
}
