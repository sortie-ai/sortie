//go:build unix

package opencode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/agenttest/dispositiontest"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/waittest"
	"github.com/sortie-ai/sortie/internal/workspacekit"
)

func writeOpenCodeScript(t *testing.T, dir, body string) string {
	t.Helper()
	return agenttest.WriteScript(t, dir, "fake-opencode", versionAnsweringScript(body))
}

func versionAnsweringScript(body string) string {
	return "case \"$1\" in\n  --version) echo 'opencode v2.0.18'; exit 0;;\nesac\n" + body
}

func fakeMinimalRuntime(t *testing.T) string {
	t.Helper()
	return writeOpenCodeScript(t, t.TempDir(), "exit 0")
}

func mustStartSession(t *testing.T, a domain.AgentAdapter, workDir, cmd string) domain.Session {
	t.Helper()
	return mustStartSessionWith(t, a, workDir, cmd, nil)
}

func mustStartSessionWith(t *testing.T, a domain.AgentAdapter, workDir, cmd string, settings map[string]any) domain.Session {
	t.Helper()
	session, err := a.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: workDir,
		AgentConfig:   domain.AgentConfig{Command: cmd},
		Settings:      settings,
	})
	if err != nil {
		t.Fatalf("StartSession(Settings=%v) error = %v", settings, err)
	}
	return session
}

func mustBuildSSHSessionWithLocalScript(t *testing.T, workDir, script, sshHost string) domain.Session {
	t.Helper()
	pt, fault := parsePassthroughConfig(map[string]any{})
	if fault != nil {
		t.Fatalf("parsePassthroughConfig() error = %v", fault)
	}
	state := &sessionState{
		target: agentcore.LaunchTarget{
			Command:       script,
			WorkspacePath: workDir,
			RemoteCommand: "opencode",
			SSHHost:       sshHost,
		},
		passthrough: pt,
		baseLogger:  slog.Default(),
		usage:       agentcore.NewTurnEndUsage(),
	}
	state.fork = newForkSession(state)
	return domain.Session{Internal: state}
}

func writeRunFixtureScript(t *testing.T, dir, fixtureName string) string {
	t.Helper()

	runPath := filepath.Join(dir, fixtureName)
	if err := os.WriteFile(runPath, loadFixture(t, fixtureName), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", fixtureName, err)
	}

	exportPath := filepath.Join(dir, "export.json")
	if err := os.WriteFile(exportPath, []byte(`{"info":{"id":""},"messages":[]}`), 0o644); err != nil {
		t.Fatalf("WriteFile(export.json): %v", err)
	}

	body := `case "$1 $2" in
  "session export") cat '` + exportPath + `'; exit 0;;
esac
cat '` + runPath + `'`

	return writeOpenCodeScript(t, dir, body)
}

func splitPermissionWarningFixture(t *testing.T) (warningLine, stdoutLine string) {
	t.Helper()
	lines := bytes.Split(bytes.TrimRight(loadFixture(t, "permission_warning_then_error.txt"), "\n"), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("permission_warning_then_error.txt has %d lines, want 2", len(lines))
	}
	return string(lines[0]), string(lines[1])
}

func writeFixtureFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
	return path
}

func hasNotification(events []domain.AgentEvent, message string) bool {
	for _, e := range events {
		if e.Type == domain.EventNotification && e.Message == message {
			return true
		}
	}
	return false
}

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

	a, err := NewOpenCodeAdapter()
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

func TestStartSession_RefusalMatchesValidateConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		settings map[string]any
		check    string
	}{
		{name: "tool overlap", settings: map[string]any{"allowed_tools": []any{"bash"}, "denied_tools": []any{"bash"}}, check: "opencode.allowed_tools.overlap"},
		{name: "effort and variant both set", settings: map[string]any{registry.EffortKey: "high", "variant": "max"}, check: "opencode." + registry.EffortKey + ".conflict"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err, launched := startRefusedSession(t, tt.settings)

			agentErr, ok := errors.AsType[*domain.AgentError](err)
			diag := hasCheck(validateConfig(registry.AgentConfigFields{Kind: "opencode", Passthrough: tt.settings}), tt.check)
			if !ok || agentErr.Kind != domain.ErrAgentNotFound || diag == nil || agentErr.Message != diag.Message || launched {
				t.Errorf("StartSession(%v) error, launched = %v, %v, want an agent_not_found *domain.AgentError carrying the message of validateConfig check %q (%+v), no launch", tt.settings, err, launched, tt.check, diag)
			}
		})
	}
}

func TestStartSession_SettingsArePerSession(t *testing.T) {
	t.Parallel()

	a, _ := NewOpenCodeAdapter()
	script := fakeMinimalRuntime(t)

	first := mustStartSessionWith(t, a, t.TempDir(), script, map[string]any{"model": "provider/model-a", "allowed_tools": []any{"read"}}).Internal.(*sessionState)
	session, err := a.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath:   t.TempDir(),
		AgentConfig:     domain.AgentConfig{Command: script},
		ResumeSessionID: "ses_prior",
		Settings:        map[string]any{"model": "provider/model-b"},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}
	resumed := session.Internal.(*sessionState)

	if first.passthrough.Model != "provider/model-a" || !slices.Equal(first.passthrough.AllowedTools, []string{"read"}) {
		t.Errorf("first session settings = %+v, want model-a with allowed_tools [read]", first.passthrough)
	}
	if resumed.passthrough.Model != "provider/model-b" || len(resumed.passthrough.AllowedTools) != 0 {
		t.Errorf("resumed session settings = %+v, want model-b with no allowed_tools", resumed.passthrough)
	}
	for turn := 1; turn <= 2; turn++ {
		if got := flagValue(buildRunArgs(resumed, resumed.passthrough), "--model"); got != "provider/model-b" {
			t.Errorf("resumed session turn %d --model = %q, want %q", turn, got, "provider/model-b")
		}
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

			a, _ := NewOpenCodeAdapter()
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

	a, _ := NewOpenCodeAdapter()
	resumeID := "ses_resume123"
	session, err := a.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath:   t.TempDir(),
		AgentConfig:     domain.AgentConfig{Command: fakeMinimalRuntime(t)},
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
	// Not parallel at this level: the "remote launch" case below calls
	// t.Setenv on its own subtest, which panics if any ancestor called
	// t.Parallel.

	write := func(t *testing.T, content string) string {
		t.Helper()
		dir := filepath.Join(t.TempDir(), workspacekit.SortieDir)
		if err := os.Mkdir(dir, 0o750); err != nil {
			t.Fatalf("Mkdir() error = %v", err)
		}
		path := filepath.Join(dir, "mcp.json")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
		return path
	}

	const withServer = `{"mcpServers":{"sortie-tools":{"command":"/usr/local/bin/sortie","args":["mcp-server"]}}}`

	tests := []struct {
		name       string
		content    string
		sshHost    string
		wantServer bool
	}{
		{name: "local launch carries the tool server", content: withServer, wantServer: true},
		{name: "remote launch without a relay carries an empty mcp member", content: withServer, sshHost: "build-host"},
		{name: "no declared server carries an empty mcp member", content: `{"mcpServers":{}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			command := fakeMinimalRuntime(t)
			if tt.sshHost == "" {
				t.Parallel()
			} else {
				// Not parallel: places a fake ssh stand-in first on PATH
				// through t.Setenv, so the version query never reaches a
				// real network host.
				sshDir := t.TempDir()
				agenttest.FakeRuntime(t, sshDir, "ssh", agenttest.OutputScenario, agenttest.Output{Stdout: "opencode v2.0.18\n"})
				t.Setenv("PATH", sshDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			}

			a, _ := NewOpenCodeAdapter()
			session, err := a.StartSession(context.Background(), domain.StartSessionParams{
				WorkspacePath: t.TempDir(),
				AgentConfig:   domain.AgentConfig{Command: command},
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
			if state.turnConfigContent == "" {
				t.Fatal("StartSession() stored an empty turn configuration, want a document carrying the title agent switch")
			}

			doc := decodeConfigDocument(t, state.turnConfigContent)

			assertTitleAgentDisabled(t, doc)
			mcp, ok := doc["mcp"]
			if !ok || strings.Contains(string(mcp), "sortie-tools") != tt.wantServer {
				t.Errorf("StartSession() stored mcp = %s (present %v), want a member naming sortie-tools = %v", mcp, ok, tt.wantServer)
			}
		})
	}
}

func TestRunTurn_WrongInternalType(t *testing.T) {
	t.Parallel()

	a, _ := NewOpenCodeAdapter()
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

func TestRunTurn_SessionIDMismatch(t *testing.T) {
	t.Parallel()

	a, _ := NewOpenCodeAdapter()
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

	a, _ := NewOpenCodeAdapter()
	tmpDir := t.TempDir()
	session := mustStartSession(t, a, tmpDir, fakeMinimalRuntime(t))

	if err := a.StopSession(context.Background(), session); err != nil {
		t.Fatalf("StopSession() error = %v, want nil", err)
	}
	if err := a.StopSession(context.Background(), session); err != nil {
		t.Fatalf("StopSession() second call error = %v, want nil", err)
	}
}

func TestStopSession_WrongInternalType(t *testing.T) {
	t.Parallel()

	a, _ := NewOpenCodeAdapter()
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

func TestRunTurn_MultiTurnAccumulation(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	runFixture := loadFixture(t, "simple_turn.jsonl")
	runPath := filepath.Join(tmpDir, "run.jsonl")
	if err := os.WriteFile(runPath, runFixture, 0o644); err != nil {
		t.Fatal(err)
	}

	export1Path := filepath.Join(tmpDir, "export1.json")
	export1 := `{"info":{"id":"ses_abc123"},"messages":[{"type":"assistant","finish":"stop","model":{"providerID":"anthropic","id":"claude-sonnet-4-5"},"tokens":{"input":0,"output":100,"total":100,"reasoning":0,"cache":{"read":0,"write":0}}}]}`
	if err := os.WriteFile(export1Path, []byte(export1), 0o644); err != nil {
		t.Fatal(err)
	}

	export2Path := filepath.Join(tmpDir, "export2.json")
	export2 := `{"info":{"id":"ses_abc123"},"messages":[` +
		`{"type":"assistant","finish":"stop","model":{"providerID":"anthropic","id":"claude-sonnet-4-5"},"tokens":{"input":0,"output":100,"total":100,"reasoning":0,"cache":{"read":0,"write":0}}},` +
		`{"type":"assistant","finish":"stop","model":{"providerID":"anthropic","id":"claude-sonnet-4-5"},"tokens":{"input":0,"output":60,"total":60,"reasoning":0,"cache":{"read":0,"write":0}}}` +
		`]}`
	if err := os.WriteFile(export2Path, []byte(export2), 0o644); err != nil {
		t.Fatal(err)
	}

	counterPath := filepath.Join(tmpDir, "export-call-count")
	script := writeOpenCodeScript(t, tmpDir, `case "$1 $2" in
  "session export")
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

	a, _ := NewOpenCodeAdapter()
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

func TestRunTurn_UsageMeasuredPersistsAcrossCancelledTurn(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	runFixture := loadFixture(t, "simple_turn.jsonl")
	runPath := filepath.Join(tmpDir, "run.jsonl")
	if err := os.WriteFile(runPath, runFixture, 0o644); err != nil {
		t.Fatal(err)
	}

	exportPath := filepath.Join(tmpDir, "export.json")
	const export = `{"info":{"id":"ses_abc123"},"messages":[{"type":"assistant","finish":"stop","model":{"providerID":"anthropic","id":"claude-sonnet-4-5"},"tokens":{"input":10,"output":20,"total":30,"reasoning":0,"cache":{"read":0,"write":0}}}]}`
	if err := os.WriteFile(exportPath, []byte(export), 0o644); err != nil {
		t.Fatal(err)
	}

	counterPath := filepath.Join(tmpDir, "turn-count")
	script := writeOpenCodeScript(t, tmpDir, `case "$1 $2" in
  "session export") cat '`+exportPath+`'; exit 0;;
esac
if [ -f '`+counterPath+`' ]; then
  printf '{"type":"step_start","timestamp":1000,"sessionID":"ses_abc123","part":{"id":"p1","messageID":"m1","sessionID":"ses_abc123","snapshot":"","type":"step-start"}}\n'
  sleep 1000
fi
touch '`+counterPath+`'
cat '`+runPath+`'`)

	outerCtx := waittest.Context(t)

	a, _ := NewOpenCodeAdapter()
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
		runErr := <-errCh
		var agentErr *domain.AgentError
		if !errors.As(runErr, &agentErr) || agentErr.Kind != domain.ErrTurnCancelled {
			t.Errorf("turn 2: RunTurn() error = %v, want AgentError{Kind: %q}", runErr, domain.ErrTurnCancelled)
		}
	case <-outerCtx.Done():
		t.Fatal("RunTurn (turn 2) did not return after context cancel")
	}
}

func TestRunTurn_SessionStartedOnce(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	fixture := loadFixture(t, "simple_turn.jsonl")
	fixturePath := filepath.Join(tmpDir, "output.jsonl")
	if err := os.WriteFile(fixturePath, fixture, 0o644); err != nil {
		t.Fatal(err)
	}

	script := writeOpenCodeScript(t, tmpDir, "cat '"+fixturePath+"'")

	a, _ := NewOpenCodeAdapter()
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
	agenttest.AssertSessionIDContract(t, []string{session.ID}, turn1Events, result1)

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

	a, _ := NewOpenCodeAdapter()
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
			script := writeRunFixtureScript(t, tmpDir, tt.fixture)

			a, _ := NewOpenCodeAdapter()
			session := mustStartSessionWith(t, a, tmpDir, script, map[string]any{"model": "nonexistent/nonexistent"})

			events, result, err := collectEvents(t, a, session, "work")
			if result.ExitReason != domain.EventTurnFailed {
				t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
			}
			var agentErr *domain.AgentError
			if !errors.As(err, &agentErr) || agentErr.Kind != domain.ErrTurnFailed {
				t.Fatalf("RunTurn() error = %v, want AgentError{Kind: %q}", err, domain.ErrTurnFailed)
			}

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

func turnFailedEvents(events []domain.AgentEvent) []domain.AgentEvent {
	var out []domain.AgentEvent
	for _, e := range events {
		if e.Type == domain.EventTurnFailed {
			out = append(out, e)
		}
	}
	return out
}

func TestRunTurn_FreeTierRefusalNamesDeniedTools(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := writeRunFixtureScript(t, tmpDir, "free_tier_refusal.jsonl")

	a, err := NewOpenCodeAdapter()
	if err != nil {
		t.Fatalf("NewOpenCodeAdapter() error = %v", err)
	}
	session := mustStartSessionWith(t, a, tmpDir, script, map[string]any{"allowed_tools": []any{"read", "glob"}})

	events, result, _ := collectEvents(t, a, session, "work")

	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}
	failed := turnFailedEvents(events)
	if len(failed) != 1 {
		t.Fatalf("turn_failed event count = %d, want 1; got %v", len(failed), events)
	}
	const wantMessage = "Error from provider (Console): OpenCode's free tier can only be used from within OpenCode" +
		"; the opencode.allowed_tools and opencode.denied_tools settings deny bash, a tool the runtime's free tier requires"
	if failed[0].Message != wantMessage {
		t.Errorf("turn_failed Message = %q, want %q", failed[0].Message, wantMessage)
	}
}

func TestRunTurn_OversizedStdoutLine(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := writeOpenCodeScript(t, tmpDir, `head -c $((10*1024*1024+1)) /dev/zero | tr '\000' 'a'
printf '\n'`)

	a, _ := NewOpenCodeAdapter()
	session := mustStartSession(t, a, tmpDir, script)

	events, result, err := collectEvents(t, a, session, "work")
	if err == nil {
		t.Fatal("RunTurn() error = nil, want oversized-line failure")
	}
	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) {
		t.Fatalf("error type = %T, want *domain.AgentError", err)
	}
	if agentErr.Kind != domain.ErrPortExit {
		t.Errorf("Kind = %q, want %q", agentErr.Kind, domain.ErrPortExit)
	}
	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}

	failedEvents := turnFailedEvents(events)
	if len(failedEvents) != 1 {
		t.Fatalf("turn_failed event count = %d, want 1", len(failedEvents))
	}
	if !strings.HasPrefix(failedEvents[0].Message, "stdout read error") {
		t.Errorf("turn_failed Message = %q, want prefix %q", failedEvents[0].Message, "stdout read error")
	}

	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		Terminal:          agentcore.TerminalFailure,
		TerminalErrorKind: domain.ErrPortExit,
		TerminalMessage:   "stdout scanner error",
	}, result, err)
}

func TestRunTurn_ReadErrorRecoversUsageFromExport(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	exportPath := filepath.Join(tmpDir, "export.json")
	const export = `{"info":{"id":"ses_abc123"},"messages":[{"type":"assistant","finish":"stop","model":{"providerID":"anthropic","id":"claude-sonnet-4-5"},"tokens":{"input":10,"output":20,"total":30,"reasoning":0,"cache":{"read":0,"write":0}}}]}`
	if err := os.WriteFile(exportPath, []byte(export), 0o644); err != nil {
		t.Fatal(err)
	}

	// The first event teaches the adapter the session id an export needs;
	// the oversized line after it is what fails the read.
	script := writeOpenCodeScript(t, tmpDir, `case "$1 $2" in
  "session export") cat '`+exportPath+`'; exit 0;;
esac
printf '{"type":"step_start","timestamp":1000,"sessionID":"ses_abc123","part":{"id":"p1","messageID":"m1","sessionID":"ses_abc123","snapshot":"","type":"step-start"}}\n'
head -c $((10*1024*1024+1)) /dev/zero | tr '\000' 'a'
printf '\n'`)

	a, _ := NewOpenCodeAdapter()
	session := mustStartSession(t, a, tmpDir, script)

	events, result, err := collectEvents(t, a, session, "work")
	if err == nil {
		t.Fatal("RunTurn() error = nil, want oversized-line failure")
	}
	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}
	if !result.UsageMeasured {
		t.Error("UsageMeasured = false; the export was readable and named a figure")
	}
	if result.Usage.InputTokens != 10 || result.Usage.OutputTokens != 20 {
		t.Errorf("Usage = in:%d out:%d, want in:10 out:20",
			result.Usage.InputTokens, result.Usage.OutputTokens)
	}

	var usageEvents []domain.AgentEvent
	for _, ev := range events {
		if ev.Type == domain.EventTokenUsage {
			usageEvents = append(usageEvents, ev)
		}
	}
	if len(usageEvents) != 1 {
		t.Fatalf("token-usage events = %d, want exactly 1", len(usageEvents))
	}
	if usageEvents[0].Model != "anthropic/claude-sonnet-4-5" {
		t.Errorf("Model = %q, want the model the export named", usageEvents[0].Model)
	}
}

func TestRunTurn_SSH_CarriesInlineConfigDocument(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	fixture := writeFixtureFile(t, tmpDir, "simple_turn.jsonl", string(loadFixture(t, "simple_turn.jsonl")))
	script := writeOpenCodeScript(t, tmpDir, "cat > '"+tmpDir+"/stdin.'$$\ncat '"+fixture+"'")

	a, _ := NewOpenCodeAdapter()
	session := mustBuildSSHSessionWithLocalScript(t, tmpDir, script, "example.test")
	state := session.Internal.(*sessionState)
	document, err := buildInlineConfig(state.passthrough, nil)
	if err != nil {
		t.Fatalf("buildInlineConfig() error = %v", err)
	}
	assertTitleAgentDisabled(t, decodeConfigDocument(t, document))
	if strings.Contains(document, "'") {
		t.Fatalf("document = %q, want no single quote", document)
	}
	state.turnConfigContent = document

	_, result, err := collectEvents(t, a, session, "work")
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Fatalf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}

	inputs, err := filepath.Glob(filepath.Join(tmpDir, "stdin.*"))
	if err != nil {
		t.Fatalf("Glob() error = %v", err)
	}
	for _, path := range inputs {
		input, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%q): %v", path, err)
		}
		if !strings.HasSuffix(string(input), "work") {
			continue
		}
		if want := "OPENCODE_CONFIG_CONTENT='" + document + "'"; !strings.Contains(string(input), want) {
			t.Errorf("run invocation standard input = %q, want it to contain %q", input, want)
		}
		return
	}
	t.Fatalf("no invocation received the prompt on standard input among %d captured inputs", len(inputs))
}

func TestRunTurn_SSH_CompletedTurnWithExit255IsNotAConnectionFailure(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	fixture := writeFixtureFile(t, tmpDir, "simple_turn.jsonl", string(loadFixture(t, "simple_turn.jsonl")))
	script := writeOpenCodeScript(t, tmpDir, "cat '"+fixture+"'\nexit 255")

	a, _ := NewOpenCodeAdapter()
	session := mustBuildSSHSessionWithLocalScript(t, tmpDir, script, "example.test")

	_, _, err := collectEvents(t, a, session, "work")

	if errors.Is(err, sshutil.ErrConnectionFailed) {
		t.Errorf("RunTurn() error = %v, wraps sshutil.ErrConnectionFailed, want a turn that produced a terminal step_finish before ssh exited 255 reported as a runtime exit", err)
	}
}

const emptyExport = `{"info":{"id":"ses_abc123"},"messages":[]}`

func exportOf(input, output int) string {
	return fmt.Sprintf(`{"info":{"id":"ses_abc123"},"messages":[{"type":"assistant","finish":"stop","model":{"providerID":"anthropic","id":"claude-sonnet-4-5"},"tokens":{"input":%d,"output":%d,"reasoning":0,"cache":{"read":0,"write":0}}}]}`, input, output)
}

func stepRun(input, output int) string {
	return fmt.Sprintf(`{"type":"step_start","timestamp":1777197446593,"sessionID":"ses_abc123","part":{"id":"prt_1","messageID":"msg_1","sessionID":"ses_abc123","snapshot":"","type":"step-start"}}
{"type":"text","timestamp":1777197446597,"sessionID":"ses_abc123","part":{"id":"prt_2","messageID":"msg_1","sessionID":"ses_abc123","type":"text","text":"done","time":{"start":1777197446595,"end":1777197446596}}}
{"type":"step_finish","timestamp":1777197446660,"sessionID":"ses_abc123","part":{"id":"prt_3","reason":"stop","messageID":"msg_1","sessionID":"ses_abc123","type":"step-finish","tokens":{"total":%d,"input":%d,"output":%d,"reasoning":0,"cache":{"write":0,"read":0}},"cost":0}}
`, input+output, input, output)
}

func writeStepScript(t *testing.T, dir string) (script, runPath, exportPath string) {
	t.Helper()

	runPath = filepath.Join(dir, "run.jsonl")
	exportPath = filepath.Join(dir, "export.json")
	script = writeOpenCodeScript(t, dir, `case "$1 $2" in
  "session export") cat '`+exportPath+`'; exit 0;;
esac
cat '`+runPath+`'`)
	return script, runPath, exportPath
}

func TestRunTurn_StepReportSettlement(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		export          string
		stepInput       int
		stepOutput      int
		wantInput       int64
		wantOutput      int64
		wantMeasured    bool
		wantUnaccounted bool
	}{
		{"export recovering nothing reports the step sum unsettled", emptyExport, 14406, 21, 14406, 21, true, true},
		{"steps adding no input and no output report no figure", emptyExport, 0, 0, 0, 0, false, true},
		{"export recovering a genuine zero is a measured zero", exportOf(0, 0), 14406, 21, 0, 0, true, false},
		{"export recovering a figure reports it alone", exportOf(10, 20), 14406, 21, 10, 20, true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			script, runPath, exportPath := writeStepScript(t, dir)
			writeFixtureFile(t, dir, filepath.Base(runPath), stepRun(tt.stepInput, tt.stepOutput))
			writeFixtureFile(t, dir, filepath.Base(exportPath), tt.export)
			a, _ := NewOpenCodeAdapter()

			events, result, err := collectEvents(t, a, mustStartSession(t, a, dir, script), "work")
			if err != nil {
				t.Fatalf("RunTurn() error = %v", err)
			}

			if result.Usage.InputTokens != tt.wantInput || result.Usage.OutputTokens != tt.wantOutput {
				t.Errorf("RunTurn().Usage = in:%d out:%d, want in:%d out:%d", result.Usage.InputTokens, result.Usage.OutputTokens, tt.wantInput, tt.wantOutput)
			}
			if result.UsageMeasured != tt.wantMeasured {
				t.Errorf("RunTurn().UsageMeasured = %v, want %v", result.UsageMeasured, tt.wantMeasured)
			}
			if result.SpendUnaccounted != tt.wantUnaccounted {
				t.Errorf("RunTurn().SpendUnaccounted = %v, want %v", result.SpendUnaccounted, tt.wantUnaccounted)
			}
			if !tt.wantMeasured {
				agenttest.AssertMeasurementAbsent(t, events, result)
			}
		})
	}
}

func TestRunTurn_StepCountsCarryUntilAnExportReplacesThem(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script, runPath, exportPath := writeStepScript(t, dir)
	a, _ := NewOpenCodeAdapter()
	session := mustStartSession(t, a, dir, script)

	turns := []struct {
		name            string
		input, output   int
		export          string
		wantIn, wantOut int64
	}{
		{"first turn without an export keeps its step counts", 100, 10, emptyExport, 100, 10},
		{"second turn carries the first turn's counts", 50, 5, emptyExport, 150, 15},
		{"an export figure replaces the carried counts", 20, 2, exportOf(400, 40), 400, 40},
		{"counts after an export start from zero", 7, 1, emptyExport, 407, 41},
	}

	for _, tt := range turns {
		writeFixtureFile(t, dir, filepath.Base(runPath), stepRun(tt.input, tt.output))
		writeFixtureFile(t, dir, filepath.Base(exportPath), tt.export)

		_, result, err := collectEvents(t, a, session, "work")
		if err != nil {
			t.Fatalf("%s: RunTurn() error = %v", tt.name, err)
		}

		if result.Usage.InputTokens != tt.wantIn || result.Usage.OutputTokens != tt.wantOut {
			t.Errorf("%s: RunTurn().Usage = in:%d out:%d, want in:%d out:%d", tt.name, result.Usage.InputTokens, result.Usage.OutputTokens, tt.wantIn, tt.wantOut)
		}
	}
}

func TestRunTurn_CleanExitWithoutStepFinishSettlesSpend(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	runFixture := loadFixture(t, "simple_turn.jsonl")
	withoutStepFinish := bytes.Join(bytes.SplitAfter(runFixture, []byte("\n"))[:2], nil)
	runPath := filepath.Join(tmpDir, "run.jsonl")
	if err := os.WriteFile(runPath, withoutStepFinish, 0o644); err != nil {
		t.Fatal(err)
	}

	exportPath := filepath.Join(tmpDir, "export.json")
	const export = `{"info":{"id":"ses_abc123"},"messages":[{"type":"assistant","finish":"stop","model":{"providerID":"anthropic","id":"claude-sonnet-4-5"},"tokens":{"input":10,"output":20,"total":30,"reasoning":0,"cache":{"read":0,"write":0}}}]}`
	if err := os.WriteFile(exportPath, []byte(export), 0o644); err != nil {
		t.Fatal(err)
	}

	script := writeOpenCodeScript(t, tmpDir, `case "$1 $2" in
  "session export") cat '`+exportPath+`'; exit 0;;
esac
cat '`+runPath+`'`)

	a, _ := NewOpenCodeAdapter()
	session := mustStartSession(t, a, tmpDir, script)

	_, result, err := collectEvents(t, a, session, "work")
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	if result.SpendUnaccounted {
		t.Error("RunTurn().SpendUnaccounted = true, want false for a turn whose process exited cleanly after the export recovered its usage")
	}
	if !result.UsageMeasured {
		t.Error("RunTurn().UsageMeasured = false, want true when the session export yielded a usage figure")
	}
	if result.Usage.InputTokens != 10 || result.Usage.OutputTokens != 20 || result.Usage.TotalTokens != 30 {
		t.Errorf("RunTurn().Usage = {input %d, output %d, total %d}, want {10, 20, 30} from the export",
			result.Usage.InputTokens, result.Usage.OutputTokens, result.Usage.TotalTokens)
	}
}

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

	script := writeOpenCodeScript(t, tmpDir, `case "$1 $2" in
  "session export") cat '`+exportPath+`'; exit 0;;
esac
cat '`+runPath+`'`)

	a, _ := NewOpenCodeAdapter()
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
	script := writeOpenCodeScript(t, tmpDir, `case "$1 $2" in
  "session export") echo '{"info":{"id":""},"messages":[]}'; exit 0;;
esac
printf '! permission requested: external_directory (/etc/*); auto-rejecting\n' >&2
printf '{"type":"step_start","timestamp":1000,"sessionID":"ses_visibility123","part":{"id":"p1","messageID":"m1","sessionID":"ses_visibility123","snapshot":"","type":"step-start"}}\n'
printf '{"type":"text","timestamp":1001,"sessionID":"ses_visibility123","part":{"id":"p3","messageID":"m1","sessionID":"ses_visibility123","type":"text","text":"working on it","time":{"start":1001,"end":1001}}}\n'
printf '{"type":"unknown_future_type","timestamp":1001,"sessionID":"ses_visibility123","data":"something"}\n'
printf '{"type":"step_finish","timestamp":1002,"sessionID":"ses_visibility123","part":{"id":"p2","messageID":"m1","sessionID":"ses_visibility123","type":"step-finish","reason":"stop"}}\n'`)

	a, _ := NewOpenCodeAdapter()
	session := mustStartSession(t, a, tmpDir, script)

	events, result, err := collectEvents(t, a, session, "work")
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Fatalf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}

	var sawPermissionWarning bool
	var blankPings int
	var sawUnknownMalformed bool
	var sawSessionStarted bool
	var sawTurnCompleted bool

	for _, event := range events {
		switch event.Type {
		case domain.EventNotification:
			switch {
			case strings.HasPrefix(event.Message, "the agent runtime refused a permission request"):
				sawPermissionWarning = true
			case event.Message == "":
				blankPings++
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
	if blankPings != 2 {
		t.Errorf("blank pings = %d, want 2 (step_start and step_finish)", blankPings)
	}
	if !sawUnknownMalformed {
		t.Error("unknown JSON envelope did not emit malformed event")
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

func TestRunTurn_ReasoningPartCountsAsWork(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := writeOpenCodeScript(t, tmpDir, `case "$1 $2" in
  "session export") echo '{"info":{"id":""},"messages":[]}'; exit 0;;
esac
printf '{"type":"reasoning","timestamp":1000,"sessionID":"ses_reasoning123","part":{"id":"p1","messageID":"m1","sessionID":"ses_reasoning123","type":"reasoning","text":"thinking it through"}}\n'`)

	a, _ := NewOpenCodeAdapter()
	session := mustStartSession(t, a, tmpDir, script)

	events, result, err := collectEvents(t, a, session, "work")
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Fatalf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}

	var blankPings int
	var sawTurnCompleted bool
	for _, event := range events {
		switch {
		case event.Type == domain.EventNotification && event.Message == "":
			blankPings++
		case event.Type == domain.EventTurnCompleted:
			sawTurnCompleted = true
		}
	}
	if blankPings != 1 {
		t.Errorf("blank pings = %d, want 1 for the reasoning part", blankPings)
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

func TestRunTurn_PermissionWarningRecognizedOnStderr(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	warningLine, stdoutLine := splitPermissionWarningFixture(t)

	stdoutPath := writeFixtureFile(t, tmpDir, "stdout.jsonl", stdoutLine+"\n")
	stderrPath := writeFixtureFile(t, tmpDir, "stderr.txt", warningLine+"\n")
	exportPath := writeFixtureFile(t, tmpDir, "export.json", `{"info":{"id":""},"messages":[]}`)

	script := writeOpenCodeScript(t, tmpDir, `case "$1 $2" in
  "session export") cat '`+exportPath+`'; exit 0;;
esac
cat '`+stderrPath+`' >&2
cat '`+stdoutPath+`'`)

	a, _ := NewOpenCodeAdapter()
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

func TestRunTurn_PermissionWarningOnStdoutIsNotRecognized(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	warningLine, stdoutLine := splitPermissionWarningFixture(t)

	stdoutPath := writeFixtureFile(t, tmpDir, "stdout.jsonl", warningLine+"\n"+stdoutLine+"\n")
	exportPath := writeFixtureFile(t, tmpDir, "export.json", `{"info":{"id":""},"messages":[]}`)

	script := writeOpenCodeScript(t, tmpDir, `case "$1 $2" in
  "session export") cat '`+exportPath+`'; exit 0;;
esac
cat '`+stdoutPath+`'`)

	a, _ := NewOpenCodeAdapter()
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

	outerCtx := waittest.Context(t)

	tmpDir := t.TempDir()
	script := writeOpenCodeScript(t, tmpDir, `case "$1 $2" in
  "session export") echo '{"info":{"id":""},"messages":[]}'; exit 0;;
esac
printf '{"type":"step_start","timestamp":1000,"sessionID":"ses_abc123","part":{"id":"p1","messageID":"m1","sessionID":"ses_abc123","snapshot":"","type":"step-start"}}\n'
sleep 1000`)

	a, _ := NewOpenCodeAdapter()
	session, err := a.StartSession(outerCtx, domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

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

func mustMakeTempDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func TestRunTurn_ExitZeroNoAssistantOutputPart(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := writeOpenCodeScript(t, tmpDir, `case "$1 $2" in
  "session export") echo '{"info":{"id":""},"messages":[]}'; exit 0;;
esac
printf '{"type":"step_start","timestamp":1000,"sessionID":"ses_c1","part":{"id":"p1","messageID":"m1","sessionID":"ses_c1","snapshot":"","type":"step-start"}}\n'
printf '{"type":"step_finish","timestamp":1001,"sessionID":"ses_c1","part":{"id":"p2","messageID":"m1","sessionID":"ses_c1","type":"step-finish","reason":"stop"}}\n'`)

	a, _ := NewOpenCodeAdapter()
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

func TestRunTurn_ExitZeroNoJSONEventAtAll(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := writeOpenCodeScript(t, tmpDir, `case "$1 $2" in
  "session export") echo '{"info":{"id":""},"messages":[]}'; exit 0;;
esac
exit 0`)

	a, _ := NewOpenCodeAdapter()
	session := mustStartSession(t, a, tmpDir, script)

	events, result, err := collectEvents(t, a, session, "work")
	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}
	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) || agentErr.Kind != domain.ErrPortExit {
		t.Fatalf("RunTurn() error = %v, want AgentError{Kind: %q}", err, domain.ErrPortExit)
	}
	const wantMessage = "the agent runtime exited before responding: exit status 0"
	if agentErr.Message != wantMessage {
		t.Errorf("AgentError.Message = %q, want %q", agentErr.Message, wantMessage)
	}

	failedEvents := turnFailedEvents(events)
	if len(failedEvents) != 1 {
		t.Fatalf("turn_failed event count = %d, want 1", len(failedEvents))
	}
}

func TestRunTurn_UnparseableLineDoesNotCountAsResponse(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := writeOpenCodeScript(t, tmpDir, `case "$1 $2" in
  "session export") echo '{"info":{"id":""},"messages":[]}'; exit 0;;
esac
echo 'Usage: opencode [options]'
exit 0`)

	a, _ := NewOpenCodeAdapter()
	session := mustStartSession(t, a, tmpDir, script)

	events, result, err := collectEvents(t, a, session, "work")
	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}
	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) {
		t.Fatalf("RunTurn() error = %v, want *domain.AgentError", err)
	}
	if _, ok := errors.AsType[*agentcore.EarlyExitError](agentErr.Err); !ok {
		t.Errorf("RunTurn() error = %v, want a chain carrying an *agentcore.EarlyExitError", err)
	}

	var malformedCount int
	for _, e := range events {
		if e.Type == domain.EventMalformed {
			malformedCount++
		}
	}
	if malformedCount != 1 {
		t.Errorf("EventMalformed count = %d, want 1; got %v", malformedCount, events)
	}
}

func TestRunTurn_NonZeroExitNoTerminalReport(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := writeOpenCodeScript(t, tmpDir, `case "$1 $2" in
  "session export") echo '{"info":{"id":""},"messages":[]}'; exit 0;;
esac
printf '{"type":"step_start","timestamp":1000,"sessionID":"ses_nonzero","part":{"id":"p1","messageID":"m1","sessionID":"ses_nonzero","snapshot":"","type":"step-start"}}\n'
exit 7`)

	a, _ := NewOpenCodeAdapter()
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

func TestRunTurn_CompletedTurnReturnsUntypedNilError(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := writeRunFixtureScript(t, tmpDir, "simple_turn.jsonl")

	a, _ := NewOpenCodeAdapter()
	session := mustStartSession(t, a, tmpDir, script)

	_, result, err := collectEvents(t, a, session, "work")
	if result.ExitReason != domain.EventTurnCompleted {
		t.Fatalf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}
	if err != nil {
		t.Errorf("RunTurn() error = %v, want nil (not a typed-nil *domain.AgentError)", err)
	}
}

func TestRunTurn_ToolOnlyNoTerminalCompletes(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := writeRunFixtureScript(t, tmpDir, "tool_success.jsonl")

	a, _ := NewOpenCodeAdapter()
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

func TestRunTurn_ToolUseMapsKindAndArgument(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	script := writeRunFixtureScript(t, tmpDir, "tool_calls_mapped.jsonl")

	a, _ := NewOpenCodeAdapter()
	session := mustStartSession(t, a, tmpDir, script)

	events, _, err := collectEvents(t, a, session, "work")
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	type call struct {
		name     string
		kind     domain.ToolKind
		argument string
	}
	want := []call{
		{"shell", domain.ToolKindShell, "go test ./..."},
		{"read", domain.ToolKindRead, "/tmp/work/main.go"},
		{"write", domain.ToolKindEdit, "/tmp/work/notes.md"},
		{"edit", domain.ToolKindEdit, "/tmp/work/main.go"},
		{"skill", domain.ToolKindSkill, "demo-skill"},
		{"grep", "", ""},
	}
	var got []call
	for _, e := range events {
		if e.Type == domain.EventToolResult {
			got = append(got, call{e.ToolName, e.ToolKind, e.ToolArgument})
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("tool_result calls = %+v, want %+v", got, want)
	}
}

func TestRunTurn_SecondTurnFailsAfterFirstTurnBothSignals(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	counterFile := filepath.Join(tmpDir, "turn-count")
	script := writeOpenCodeScript(t, tmpDir, fmt.Sprintf(`case "$1 $2" in
  "session export") echo '{"info":{"id":""},"messages":[]}'; exit 0;;
esac
if [ -f '%s' ]; then
  printf '{"type":"step_start","timestamp":1000,"sessionID":"ses_both_then_none","part":{"id":"p1","messageID":"m1","sessionID":"ses_both_then_none","snapshot":"","type":"step-start"}}\n'
else
  touch '%s'
  printf '{"type":"text","timestamp":1000,"sessionID":"ses_both_then_none","part":{"id":"p1","messageID":"m1","sessionID":"ses_both_then_none","type":"text","text":"ok","time":{"start":1000,"end":1000}}}\n'
  printf '{"type":"tool_use","timestamp":1001,"sessionID":"ses_both_then_none","part":{"id":"p2","messageID":"m1","sessionID":"ses_both_then_none","type":"tool","tool":"read","callID":"call_both","state":{"status":"completed","input":{},"output":"ok","time":{"start":1001,"end":1001}}}}\n'
fi`, counterFile, counterFile))

	a, _ := NewOpenCodeAdapter()
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

func TestRunTurn_WorkPredicateIsPerTurn(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	counterFile := filepath.Join(tmpDir, "turn-count")
	script := writeOpenCodeScript(t, tmpDir, fmt.Sprintf(`case "$1 $2" in
  "session export") echo '{"info":{"id":""},"messages":[]}'; exit 0;;
esac
if [ -f '%s' ]; then
  printf '{"type":"step_start","timestamp":1000,"sessionID":"ses_work_pin","part":{"id":"p1","messageID":"m1","sessionID":"ses_work_pin","snapshot":"","type":"step-start"}}\n'
else
  touch '%s'
  printf '{"type":"text","timestamp":1000,"sessionID":"ses_work_pin","part":{"id":"p1","messageID":"m1","sessionID":"ses_work_pin","type":"text","text":"ok","time":{"start":1000,"end":1000}}}\n'
fi`, counterFile, counterFile))

	a, _ := NewOpenCodeAdapter()
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

func TestRunTurn_CancelledTurnRecoversUsageFromExport(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	exportPath := filepath.Join(tmpDir, "export.json")
	const export = `{"info":{"id":"ses_abc123"},"messages":[{"type":"assistant","finish":"stop","model":{"providerID":"anthropic","id":"claude-sonnet-4-5"},"tokens":{"input":10,"output":20,"total":30,"reasoning":0,"cache":{"read":0,"write":0}}}]}`
	if err := os.WriteFile(exportPath, []byte(export), 0o644); err != nil {
		t.Fatal(err)
	}

	script := writeOpenCodeScript(t, tmpDir, `case "$1 $2" in
  "session export") cat '`+exportPath+`'; exit 0;;
esac
printf '{"type":"step_start","timestamp":1000,"sessionID":"ses_abc123","part":{"id":"p1","messageID":"m1","sessionID":"ses_abc123","snapshot":"","type":"step-start"}}\n'
sleep 1000`)

	outerCtx := waittest.Context(t)

	a, _ := NewOpenCodeAdapter()
	session, err := a.StartSession(outerCtx, domain.StartSessionParams{
		WorkspacePath: tmpDir,
		AgentConfig:   domain.AgentConfig{Command: script},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	turnCtx, turnCancel := context.WithCancel(outerCtx)
	gotEvent := make(chan struct{}, 1)
	resultCh := make(chan domain.TurnResult, 1)
	var mu sync.Mutex
	var usageEvents []domain.AgentEvent
	go func() {
		result, _ := a.RunTurn(turnCtx, session, domain.RunTurnParams{
			Prompt: "only prompt",
			OnEvent: func(ev domain.AgentEvent) {
				if ev.Type == domain.EventTokenUsage {
					mu.Lock()
					usageEvents = append(usageEvents, ev)
					mu.Unlock()
				}
				select {
				case gotEvent <- struct{}{}:
				default:
				}
			},
		})
		resultCh <- result
	}()

	select {
	case <-gotEvent:
	case <-outerCtx.Done():
		t.Fatal("timed out waiting for the turn's first event")
	}
	turnCancel()

	select {
	case result := <-resultCh:
		if result.ExitReason != domain.EventTurnCancelled {
			t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCancelled)
		}
		if !result.UsageMeasured {
			t.Error("UsageMeasured = false; the export was readable and named a figure")
		}
		if result.Usage.InputTokens != 10 || result.Usage.OutputTokens != 20 {
			t.Errorf("Usage = in:%d out:%d, want in:10 out:20",
				result.Usage.InputTokens, result.Usage.OutputTokens)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(usageEvents) != 1 {
			t.Fatalf("token-usage events = %d, want exactly 1", len(usageEvents))
		}
		if usageEvents[0].Model != "anthropic/claude-sonnet-4-5" {
			t.Errorf("Model = %q, want the model the export named", usageEvents[0].Model)
		}
	case <-outerCtx.Done():
		t.Fatal("RunTurn did not return after context cancel")
	}
}

func newHookTestState(t *testing.T, scriptBody string) *sessionState {
	t.Helper()

	script := agenttest.WriteScript(t, t.TempDir(), "fake-opencode", scriptBody)
	pt, fault := parsePassthroughConfig(map[string]any{})
	if fault != nil {
		t.Fatalf("parsePassthroughConfig() error = %v", fault)
	}
	return &sessionState{
		target: agentcore.LaunchTarget{
			Command:       script,
			WorkspacePath: t.TempDir(),
		},
		passthrough: pt,
		baseLogger:  slog.Default(),
	}
}

func TestEffectivePermissionsForTest(t *testing.T) {
	t.Parallel()

	t.Run("wrong internal type", func(t *testing.T) {
		t.Parallel()

		if _, _, err := EffectivePermissionsForTest(context.Background(), domain.Session{Internal: "not a sessionState"}); err == nil {
			t.Fatal("EffectivePermissionsForTest() error = nil, want non-nil")
		}
	})

	t.Run("workspace re-verification failure fails the build", func(t *testing.T) {
		t.Parallel()

		state := &sessionState{
			target: agentcore.LaunchTarget{
				Command:       "/usr/bin/opencode",
				WorkspacePath: filepath.Join(t.TempDir(), "missing"),
			},
			baseLogger: slog.Default(),
		}
		if _, _, err := EffectivePermissionsForTest(context.Background(), domain.Session{Internal: state}); err == nil {
			t.Fatal("EffectivePermissionsForTest() error = nil, want non-nil")
		}
	})

	t.Run("a missing binary fails to start", func(t *testing.T) {
		t.Parallel()

		state := &sessionState{
			target: agentcore.LaunchTarget{
				Command:       filepath.Join(t.TempDir(), "no-such-binary"),
				WorkspacePath: t.TempDir(),
			},
			baseLogger: slog.Default(),
		}
		if _, _, err := EffectivePermissionsForTest(context.Background(), domain.Session{Internal: state}); err == nil {
			t.Fatal("EffectivePermissionsForTest() error = nil, want non-nil")
		}
	})

	t.Run("a non-zero exit is an error", func(t *testing.T) {
		t.Parallel()

		state := newHookTestState(t, "exit 1")
		if _, _, err := EffectivePermissionsForTest(context.Background(), domain.Session{Internal: state}); err == nil {
			t.Fatal("EffectivePermissionsForTest() error = nil, want non-nil")
		}
	})

	t.Run("output that does not decode is an error", func(t *testing.T) {
		t.Parallel()

		state := newHookTestState(t, "echo 'not json'")
		effective, raw, err := EffectivePermissionsForTest(context.Background(), domain.Session{Internal: state})
		if err == nil {
			t.Fatal("EffectivePermissionsForTest() error = nil, want non-nil")
		}
		if effective != nil {
			t.Errorf("EffectivePermissionsForTest() effective = %v, want nil", effective)
		}
		if !strings.Contains(string(raw), "not json") {
			t.Errorf("EffectivePermissionsForTest() raw = %q, want it to carry the command's own output", raw)
		}
	})

	t.Run("a later document rule overwrites an earlier one", func(t *testing.T) {
		t.Parallel()

		const doc = `[{"type":"document","info":{"permissions":[{"resource":"*","action":"bash","effect":"allow"}]}},` +
			`{"type":"document","info":{"permissions":[{"resource":"*","action":"bash","effect":"deny"},{"resource":"*","action":"read","effect":"allow"}]}},` +
			`{"type":"other","info":{"permissions":[{"resource":"*","action":"webfetch","effect":"allow"}]}}]`

		state := newHookTestState(t, "printf '"+doc+"'")
		effective, _, err := EffectivePermissionsForTest(context.Background(), domain.Session{Internal: state})
		if err != nil {
			t.Fatalf("EffectivePermissionsForTest() error = %v", err)
		}
		want := map[string]string{"bash": "deny", "read": "allow"}
		if !maps.Equal(effective, want) {
			t.Errorf("EffectivePermissionsForTest() effective = %v, want %v", effective, want)
		}
	})
}
