//go:build unix

package opencode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
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

func TestRunTurn_ActivityVisibilityForStallWatchdog(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	// A "text" part carries the turn's own work evidence, keeping this
	// turn on row R7 (turn_completed) so the test still exercises
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
	const wantMessage = "agent exited without producing output: no assistant output on the run stream"
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
		WorkDetail:   "no assistant output on the run stream",
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
	const wantMessage = "agent exited without producing output: no assistant output on the run stream"
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
		WorkDetail:   "no assistant output on the run stream",
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

// TestStartWait_BlocksOnStderrDrainBeforeCmdWait drives startWait directly
// with a stderrCollector built over an [io.Pipe] whose write end the test
// holds open, decoupling the collector's drain from the race-prone real
// stderr pipe entirely: instead of racing cmd.Wait's pipe close against a
// concurrent read (the shape of the original bug, and why the fix's own
// regression test flaked roughly one run in three), the drain is blocked
// on a synchronization primitive the test controls outright, so the guard
// under test either blocks forever or doesn't - no timing luck involved.
//
// The first arm keeps the pipe open under a 5-second grace: startWait's
// goroutine cannot reach cmd.Wait while the pipe is held open, so
// runtime.waitCh must still be open after a generous bounded wait.
// Removing the WaitDone guard in startWait lets the goroutine call
// cmd.Wait immediately after readerDone closes; since the underlying
// process ("true") has already exited, waitCh closes within a few
// milliseconds, deterministically failing the first select below.
//
// The second arm never closes the pipe at all, under a 100-millisecond
// grace: startWait must still close waitCh, with a real waitResult built
// from cmd.Wait rather than a synthesized one, and the abandoned
// collector must report [procutil.AbandonedMarker] rather than block.
func TestStartWait_BlocksOnStderrDrainBeforeCmdWait(t *testing.T) {
	t.Parallel()

	t.Run("stderr drain held open blocks the wait", func(t *testing.T) {
		t.Parallel()

		pr, pw := io.Pipe()
		t.Cleanup(func() { _ = pw.Close() })
		collector := procutil.NewStderrCollector(pr, slog.Default())

		cmd := exec.Command("true")
		procutil.SetProcessGroup(cmd)
		if err := cmd.Start(); err != nil {
			t.Fatalf("cmd.Start() = %v", err)
		}

		readerDone := make(chan struct{})
		close(readerDone)

		runtime := &turnRuntime{
			readerDone:      readerDone,
			stderrCollector: collector,
			waitCh:          make(chan waitResult, 1),
		}

		startWait(runtime, cmd, 5*time.Second)

		select {
		case <-runtime.waitCh:
			t.Fatal("startWait closed waitCh before the stderr drain finished; " +
				"the WaitDone guard is missing or bypassed")
		case <-time.After(300 * time.Millisecond):
		}

		if err := pw.Close(); err != nil {
			t.Fatalf("pw.Close() = %v", err)
		}

		select {
		case <-runtime.waitCh:
		case <-time.After(5 * time.Second):
			t.Fatal("startWait did not close waitCh after the stderr drain finished")
		}
	})

	t.Run("stderr drain never reaching EOF is abandoned within the grace", func(t *testing.T) {
		t.Parallel()

		pr, pw := io.Pipe()
		t.Cleanup(func() { _ = pw.Close() })
		collector := procutil.NewStderrCollector(pr, slog.Default())

		cmd := exec.Command("true")
		procutil.SetProcessGroup(cmd)
		if err := cmd.Start(); err != nil {
			t.Fatalf("cmd.Start() = %v", err)
		}

		readerDone := make(chan struct{})
		close(readerDone)

		runtime := &turnRuntime{
			readerDone:      readerDone,
			stderrCollector: collector,
			waitCh:          make(chan waitResult, 1),
		}

		startWait(runtime, cmd, 100*time.Millisecond)

		select {
		case <-runtime.waitCh:
		case <-time.After(5 * time.Second):
			t.Fatal("startWait did not close waitCh within 5 seconds of a stderr drain that never reaches EOF")
		}

		runtime.waitMu.Lock()
		got := runtime.waitRes
		runtime.waitMu.Unlock()
		if got.exitCode != 0 || got.err != nil {
			t.Errorf("waitResult = {exitCode: %d, err: %v}, want {exitCode: 0, err: nil}", got.exitCode, got.err)
		}

		linesCh := make(chan []string, 1)
		go func() { linesCh <- collector.Lines() }()
		select {
		case lines := <-linesCh:
			if want := []string{procutil.AbandonedMarker}; !slices.Equal(lines, want) {
				t.Errorf("Lines() after abandonment = %v, want %v", lines, want)
			}
		case <-time.After(1 * time.Second):
			t.Fatal("Lines() did not return the abandonment marker within 1 second")
		}
	})
}

// TestStartWait_NoBoundFiresOnLongTurn pins that a turn whose stdout reader
// outlives the stderr grace is not mistaken for an abandoned drain. The
// stderr write end is closed synchronously before startWait is invoked, so
// the drain has already finished by the time the grace would matter; this
// removes any dependency on a goroutine being scheduled inside the short
// grace window. runtime.readerDone closes only after 300 milliseconds, so
// the wait-channel assertion at 200 milliseconds catches an implementation
// that anchors the stderr bound on anything earlier than that close.
func TestStartWait_NoBoundFiresOnLongTurn(t *testing.T) {
	// No t.Parallel(): installs a global slog default.
	spy := agenttest.InstallLogSpy(t)

	pr, pw := io.Pipe()
	collector := procutil.NewStderrCollector(pr, slog.Default())
	if _, err := io.WriteString(pw, "stderr line\n"); err != nil {
		t.Fatalf("pw.Write(stderr line) = %v", err)
	}
	if err := pw.Close(); err != nil {
		t.Fatalf("pw.Close() = %v", err)
	}
	select {
	case <-collector.Done():
	case <-time.After(1 * time.Second):
		t.Fatal("collector.Done() did not close after the write end was closed")
	}

	cmd := exec.Command("true")
	procutil.SetProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start() = %v", err)
	}

	readerDone := make(chan struct{})
	go func() {
		time.Sleep(300 * time.Millisecond)
		close(readerDone)
	}()

	runtime := &turnRuntime{
		readerDone:      readerDone,
		stderrCollector: collector,
		waitCh:          make(chan waitResult, 1),
	}

	startWait(runtime, cmd, 50*time.Millisecond)

	select {
	case <-runtime.waitCh:
		t.Fatal("startWait closed waitCh before runtime.readerDone closed; " +
			"the wait on the stdout reader must stay unbounded")
	case <-time.After(200 * time.Millisecond):
	}

	select {
	case <-runtime.waitCh:
	case <-time.After(5 * time.Second):
		t.Fatal("startWait did not close waitCh after runtime.readerDone closed")
	}

	for _, e := range spy.Entries() {
		if e.Level == slog.LevelWarn && e.Msg == "agent stderr was not fully collected before the process was reaped" {
			t.Errorf("startWait() emitted an abandonment warning on a turn that outlived the grace; entry = %+v", e)
		}
	}

	want := []string{"stderr line"}
	if got := collector.Lines(); !slices.Equal(got, want) {
		t.Errorf("Lines() = %v, want %v", got, want)
	}
}
