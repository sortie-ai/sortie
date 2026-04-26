//go:build unix

package opencode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
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

func TestEventStream_ReturnsNil(t *testing.T) {
	t.Parallel()

	a, _ := NewOpenCodeAdapter(map[string]any{})
	if ch := a.EventStream(); ch != nil {
		t.Errorf("EventStream() = %v, want nil", ch)
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
		stopCh: make(chan struct{}),
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
	if result.ExitReason != domain.EventTurnEndedWithError {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnEndedWithError)
	}

	var mismatchCount int
	for _, event := range events {
		if event.Type == domain.EventSessionStarted {
			t.Fatalf("unexpected session_started event for mismatched session: %+v", event)
		}
		if event.Type == domain.EventTurnEndedWithError {
			mismatchCount++
			if !strings.Contains(event.Message, `expected "ses_expected"`) || !strings.Contains(event.Message, `got "ses_abc123"`) {
				t.Errorf("turn_ended_with_error message = %q, want mismatch details", event.Message)
			}
		}
	}
	if mismatchCount != 1 {
		t.Errorf("turn_ended_with_error count = %d, want 1", mismatchCount)
	}
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
	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) {
		t.Fatalf("error type = %T, want *domain.AgentError", err)
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
	if err != nil {
		t.Fatalf("RunTurn() error = %v, want nil", err)
	}
	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}

	var turnFailedCount int
	for _, event := range events {
		if event.Type == domain.EventTurnEndedWithError {
			t.Fatalf("unexpected turn_ended_with_error event: %+v", event)
		}
		if event.Type == domain.EventTurnFailed {
			turnFailedCount++
		}
	}
	if turnFailedCount != 1 {
		t.Errorf("turn_failed count = %d, want 1", turnFailedCount)
	}
}

func TestRunTurn_OversizedStdoutLine(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := writeOpenCodeScript(t, tmpDir, `head -c $((10*1024*1024+1)) /dev/zero | tr '\000' 'a'
printf '\n'`)

	a, _ := NewOpenCodeAdapter(map[string]any{})
	session := mustStartSession(t, a, tmpDir, script)

	_, result, err := collectEvents(t, a, session, "work")
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
	if result.ExitReason != domain.EventTurnEndedWithError {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnEndedWithError)
	}
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

func TestRunTurn_ActivityVisibilityForStallWatchdog(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := writeOpenCodeScript(t, tmpDir, `case "$1" in
  export) echo '{"messages":[]}'; exit 0;;
esac
printf '! permission requested: external_directory (/etc/*); auto-rejecting\n'
printf '{"type":"step_start","timestamp":1000,"sessionID":"ses_visibility123","part":{"id":"p1","messageID":"m1","sessionID":"ses_visibility123","snapshot":"","type":"step-start"}}\n'
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
			case strings.HasPrefix(event.Message, "! permission requested:"):
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

	// turnCtx is the context we'll cancel to trigger TurnCancelled.
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
		if err := <-errCh; err != nil {
			t.Errorf("RunTurn() error = %v, want nil on cancel", err)
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

	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()

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
