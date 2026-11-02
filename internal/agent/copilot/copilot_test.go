package copilot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/agenttest/dispositiontest"
	"github.com/sortie-ai/sortie/internal/agent/mcpconfig"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/waittest"
)

var copilotUUIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-4[0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)

const firstTurnOnlyScenario = "copilot.turn-counter"

type firstTurnOnlyOutput struct {
	CounterFile string
	Stdout      string
}

// Other test files of this package add their scenarios here from init.
var fakeScenarios = map[string]agenttest.Scenario{
	firstTurnOnlyScenario: agenttest.Typed(runFirstTurnOnlyScenario),
}

func TestMain(m *testing.M) {
	// A fake runtime's turn pulls quota with the host environment, so an
	// inherited GitHub token would reach the real endpoint; the live suite
	// keeps it.
	if os.Getenv("SORTIE_COPILOT_TEST") != "1" {
		for _, name := range []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
			_ = os.Unsetenv(name)
		}
	}
	agenttest.Main(m, fakeScenarios)
}

func runFirstTurnOnlyScenario(_ []string, params firstTurnOnlyOutput) int {
	if _, err := os.Stat(params.CounterFile); err == nil {
		return 0
	}
	if err := os.WriteFile(params.CounterFile, nil, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "turn counter: create marker: %v\n", err)
		return 1
	}
	return agenttest.Output{Stdout: params.Stdout}.Run()
}

func firstTurnOnlyBinary(t *testing.T, stdout string) string {
	t.Helper()
	dir := t.TempDir()
	return agenttest.FakeRuntime(t, dir, "copilot", firstTurnOnlyScenario, firstTurnOnlyOutput{
		CounterFile: filepath.Join(dir, "turn-count"),
		Stdout:      stdout,
	})
}

func fakeCopilotBinary(t *testing.T) string {
	t.Helper()
	return agenttest.FakeRuntime(t, t.TempDir(), "copilot", agenttest.OutputScenario, agenttest.Output{})
}

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

func TestNewCopilotAdapter(t *testing.T) {
	t.Parallel()

	adapter, err := NewCopilotAdapter()
	if err != nil {
		t.Fatalf("NewCopilotAdapter() error = %v", err)
	}
	if adapter == nil {
		t.Fatal("adapter is nil")
	}
}

func stageHeldSession(t *testing.T, sessionID string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("COPILOT_HOME", home)
	path := filepath.Join(home, "session-state", sessionID, "events.jsonl")
	if err := errors.Join(os.MkdirAll(filepath.Dir(path), 0o755), os.WriteFile(path, nil, 0o600)); err != nil {
		t.Fatalf("staging journal %q: %v", path, err)
	}
}

func startSettingsSession(t *testing.T, adapter domain.AgentAdapter, settings map[string]any, resumeID string) *sessionState {
	t.Helper()

	if resumeID != "" {
		stageHeldSession(t, resumeID)
	}
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath:   t.TempDir(),
		AgentConfig:     domain.AgentConfig{Command: fakeCopilotBinary(t)},
		ResumeSessionID: resumeID,
		Settings:        settings,
	})
	if err != nil {
		t.Fatalf("StartSession(Settings=%v) error = %v", settings, err)
	}
	return session.Internal.(*sessionState)
}

func TestStartSession_SettingsArePerSession(t *testing.T) {
	adapter, _ := NewCopilotAdapter()
	first := startSettingsSession(t, adapter, map[string]any{
		"model": "gpt-5", "effort": "high", "max_autopilot_continues": float64(15), "agent": "custom",
		"disable_builtin_mcps": true, "no_custom_instructions": true, "experimental": true,
	}, "")
	resumed := startSettingsSession(t, adapter, map[string]any{"model": "model-b"}, "aa778ea0-6eab-4ce9-b87e-11d6d33dab4f")

	if pt := first.passthrough; pt.Model != "gpt-5" || pt.MaxAutopilotContinues != 15 || pt.Agent != "custom" || !pt.DisableBuiltinMCPs || !pt.NoCustomInstructions || !pt.Experimental {
		t.Errorf("first session settings = %+v, want every key parsed", pt)
	}
	assertHasArgPair(t, buildArgs(first, 1, "p", first.passthrough), reasoningEffortFlag, "high")
	for turn := 1; turn <= 2; turn++ {
		args := buildArgs(resumed, turn, "p", resumed.passthrough)
		assertHasArgPair(t, args, "--model", "model-b")
		assertNoFlag(t, args, reasoningEffortFlag)
	}
}

func TestRegistration(t *testing.T) {
	t.Parallel()

	factory, err := registry.Agents.Get("copilot-cli")
	if err != nil {
		t.Fatalf("registry.Agents.Get(\"copilot-cli\") error = %v", err)
	}
	adapter, err := factory()
	if err != nil {
		t.Fatalf("factory() error = %v", err)
	}
	if _, ok := adapter.(*CopilotAdapter); !ok {
		t.Errorf("factory() type = %T, want *CopilotAdapter", adapter)
	}

	meta, ok := registry.Agents.Meta("copilot-cli")
	if !ok {
		t.Fatal(`registry.Agents.Meta("copilot-cli") reported not registered`)
	}
	if !meta.RequiresCommand {
		t.Error("AgentMeta.RequiresCommand = false, want true")
	}
}

func TestStartSession(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		setup   func(t *testing.T) domain.StartSessionParams
		wantErr domain.AgentErrorKind
	}{
		{
			name: "empty workspace path",
			setup: func(_ *testing.T) domain.StartSessionParams {
				return domain.StartSessionParams{}
			},
			wantErr: domain.ErrInvalidWorkspaceCwd,
		},
		{
			name: "non-existent workspace path",
			setup: func(_ *testing.T) domain.StartSessionParams {
				return domain.StartSessionParams{
					WorkspacePath: "/nonexistent/sortie-test-path-12345",
					AgentConfig:   domain.AgentConfig{Command: "/bin/sh"},
				}
			},
			wantErr: domain.ErrInvalidWorkspaceCwd,
		},
		{
			name: "workspace path is a file not a directory",
			setup: func(t *testing.T) domain.StartSessionParams {
				t.Helper()
				tmpFile := filepath.Join(t.TempDir(), "notadir")
				if err := os.WriteFile(tmpFile, []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
				return domain.StartSessionParams{
					WorkspacePath: tmpFile,
					AgentConfig:   domain.AgentConfig{Command: "/bin/sh"},
				}
			},
			wantErr: domain.ErrInvalidWorkspaceCwd,
		},
		{
			name: "agent command not found on PATH",
			setup: func(t *testing.T) domain.StartSessionParams {
				t.Helper()
				return domain.StartSessionParams{
					WorkspacePath: t.TempDir(),
					AgentConfig:   domain.AgentConfig{Command: "sortie-nonexistent-copilot-12345"},
				}
			},
			wantErr: domain.ErrAgentNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			adapter, _ := NewCopilotAdapter()
			params := tt.setup(t)
			_, err := adapter.StartSession(context.Background(), params)
			requireAgentError(t, err, tt.wantErr)
		})
	}
}

func TestStartSession_NewSession(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, _ := NewCopilotAdapter()
	fakeBin := fakeCopilotBinary(t)
	workspace := t.TempDir()
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig:   domain.AgentConfig{Command: fakeBin},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	if !copilotUUIDPattern.MatchString(session.ID) {
		t.Errorf("session.ID = %q, want a v4 UUID", session.ID)
	}

	state, ok := session.Internal.(*sessionState)
	if !ok {
		t.Fatalf("session.Internal type = %T, want *sessionState", session.Internal)
	}
	if state.copilotSessionID != session.ID {
		t.Errorf("state.copilotSessionID = %q, want %q", state.copilotSessionID, session.ID)
	}
	if state.target.WorkspacePath != workspace {
		t.Errorf("state.target.WorkspacePath = %q, want %q", state.target.WorkspacePath, workspace)
	}
	if state.isContinuation {
		t.Error("state.isContinuation = true, want false for a new session")
	}
	if state.target.SSHHost != "" {
		t.Errorf("state.target.SSHHost = %q, want empty for local mode", state.target.SSHHost)
	}
}

func TestSessionContinuity(t *testing.T) {
	adapter, _ := NewCopilotAdapter()

	agenttest.AssertSessionContinuity(t, func(t *testing.T, held, resumeID string) (domain.Session, error) {
		stageHeldSession(t, held)
		params := domain.StartSessionParams{WorkspacePath: t.TempDir(), AgentConfig: domain.AgentConfig{Command: fakeCopilotBinary(t)}, ResumeSessionID: resumeID}
		return adapter.StartSession(context.Background(), params)
	})
}

func TestStartSession_DefaultCommand(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, _ := NewCopilotAdapter()
	_, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{},
	})
	if err == nil {
		return // a host with copilot installed starts the session
	}
	requireAgentError(t, err, domain.ErrAgentNotFound)
}

func TestStartSession_SSHMode(t *testing.T) {
	sshPath := agenttest.NewSSHStandIn(t, t.TempDir(), agenttest.SSHStandIn{PATH: os.Getenv("PATH"), Forward: agenttest.SSHForwardAllocated})
	t.Setenv("PATH", filepath.Dir(sshPath)+string(os.PathListSeparator)+os.Getenv("PATH"))

	adapter, _ := NewCopilotAdapter()
	workspace := t.TempDir()
	hostPath := agenttest.WriteSeededMCPConfig(t, workspace)
	session, lookupErr := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig:   domain.AgentConfig{Command: "copilot"},
		SSHHost:       "dev-host.example.com",
		MCPConfigPath: hostPath,
		Settings:      map[string]any{"mcp_config": "/op/mcp.json"},
	})
	if lookupErr != nil {
		t.Fatalf("StartSession() (SSH mode) error = %v", lookupErr)
	}
	t.Cleanup(func() { _ = adapter.StopSession(t.Context(), session) })

	state := session.Internal.(*sessionState)
	if state.target.SSHHost != "dev-host.example.com" {
		t.Errorf("state.target.SSHHost = %q, want %q", state.target.SSHHost, "dev-host.example.com")
	}
	if state.target.RemoteCommand != "copilot" {
		t.Errorf("state.target.RemoteCommand = %q, want %q", state.target.RemoteCommand, "copilot")
	}
	if state.target.Command != sshPath {
		t.Errorf("state.target.Command = %q, want %q (ssh binary)", state.target.Command, sshPath)
	}

	args := buildArgs(state, 1, "do work", state.passthrough)

	for _, hostFile := range []string{hostPath, "/op/mcp.json"} {
		if i := slices.IndexFunc(args, func(arg string) bool { return strings.Contains(arg, hostFile) }); i >= 0 {
			t.Errorf("buildArgs() (SSH mode) carries host path %q in argument %q, want none: [%s]", hostFile, args[i], strings.Join(args, " "))
		}
	}
}

func TestPlaceMCPConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		document string
		want     []string
	}{
		{"document becomes the inline flag value", `{"mcpServers":{}}`, []string{"--additional-mcp-config", `{"mcpServers":{}}`}},
		{"no document places nothing", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			late := placeMCPConfig(mcpconfig.Delivery{Document: []byte(tt.document)})

			if !slices.Equal(late.Args, tt.want) || len(late.Env) != 0 {
				t.Errorf("placeMCPConfig(%q) = %+v, want args %q and no env", tt.document, late, tt.want)
			}
		})
	}
}

func TestStartSession_SSHHostWhitespaceOnly(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, _ := NewCopilotAdapter()
	fakeBin := fakeCopilotBinary(t)
	workspace := t.TempDir()

	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig:   domain.AgentConfig{Command: fakeBin},
		SSHHost:       "   ",
	})
	if err != nil {
		t.Fatalf("StartSession(SSHHost=%q) error = %v", "   ", err)
	}

	state := session.Internal.(*sessionState)
	if state.target.SSHHost != "" {
		t.Errorf("state.target.SSHHost = %q, want empty (whitespace-only SSHHost treated as local mode)", state.target.SSHHost)
	}
	if state.target.RemoteCommand != "" {
		t.Errorf("state.target.RemoteCommand = %q, want empty for local mode", state.target.RemoteCommand)
	}
}

func fakeCopilotBinaryWithOutput(t *testing.T, content string, exitCode int) string {
	t.Helper()
	return agenttest.FakeRuntime(t, t.TempDir(), "copilot", agenttest.OutputScenario, agenttest.Output{
		Stdout:   content,
		ExitCode: exitCode,
	})
}

func newTestSession(t *testing.T, workspace string) (domain.AgentAdapter, domain.Session) {
	t.Helper()
	adapter, err := NewCopilotAdapter()
	if err != nil {
		t.Fatalf("NewCopilotAdapter: %v", err)
	}
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig:   domain.AgentConfig{Command: fakeCopilotBinary(t)},
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	return adapter, session
}

func loadTestFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("loadTestFixture(%q): %v", name, err)
	}
	return string(data)
}

func hasEventType(events []domain.AgentEvent, typ domain.AgentEventType) bool {
	for _, e := range events {
		if e.Type == typ {
			return true
		}
	}
	return false
}

func assertNoTokenUsageAfter(t *testing.T, events []domain.AgentEvent, terminal domain.AgentEventType) {
	t.Helper()
	at := slices.IndexFunc(events, func(e domain.AgentEvent) bool { return e.Type == terminal })
	if at < 0 {
		return
	}
	if slices.ContainsFunc(events[at+1:], func(e domain.AgentEvent) bool { return e.Type == domain.EventTokenUsage }) {
		t.Errorf("phantom EventTokenUsage emitted after %s", terminal)
	}
}

func findEventByType(events []domain.AgentEvent, typ domain.AgentEventType) (domain.AgentEvent, bool) {
	for _, e := range events {
		if e.Type == typ {
			return e, true
		}
	}
	return domain.AgentEvent{}, false
}

func TestRunTurn_HappyPath(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")
	// A journal read resolves the real user home otherwise, and a journal
	// there under the same session id would leak real usage into the turn.
	t.Setenv("COPILOT_HOME", t.TempDir())

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.target.Command = fakeCopilotBinaryWithOutput(t, loadTestFixture(t, "simple_session.jsonl"), 0)

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt: "say hello",
		OnEvent: func(e domain.AgentEvent) {
			events = append(events, e)
		},
	})

	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("result.ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}
	const wantSessionID = "aa778ea0-6eab-4ce9-b87e-11d6d33dab4f"
	if result.SessionID != wantSessionID {
		t.Errorf("result.SessionID = %q, want %q", result.SessionID, wantSessionID)
	}
	if state.copilotSessionID != wantSessionID {
		t.Errorf("state.copilotSessionID = %q, want %q", state.copilotSessionID, wantSessionID)
	}
	agenttest.AssertSessionIDContract(t, []string{session.ID}, events, result)
	for _, typ := range []domain.AgentEventType{
		domain.EventSessionStarted,
		domain.EventTurnCompleted,
	} {
		if !hasEventType(events, typ) {
			t.Errorf("event type %q not delivered", typ)
		}
	}

	if e, ok := findEventByType(events, domain.EventTurnCompleted); ok {
		const wantAPIDurationMS int64 = 6866 // simple_session.jsonl's totalApiDurationMs
		if e.APIDurationMS != wantAPIDurationMS {
			t.Errorf("EventTurnCompleted.APIDurationMS = %d, want %d", e.APIDurationMS, wantAPIDurationMS)
		}
	} else {
		t.Error("EventTurnCompleted not found in events")
	}

	assertNoTokenUsageAfter(t, events, domain.EventTurnCompleted)

	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		Terminal:     agentcore.TerminalSuccess,
		ExitObserved: true,
		ExitCode:     0,
		Work:         agentcore.WorkPresent,
	}, result, err)

	agenttest.AssertMeasurementAbsent(t, events, result)
}

func TestRunTurn_ExitCode127(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.target.Command = fakeCopilotBinaryWithOutput(t, "", 127)

	var events []domain.AgentEvent
	_, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		OnEvent: func(e domain.AgentEvent) { events = append(events, e) },
	})

	requireAgentError(t, err, domain.ErrPortExit)
	if !hasEventType(events, domain.EventTurnFailed) {
		t.Error("EventTurnFailed not delivered for exit code 127")
	}
}

func TestRunTurn_NonZeroExitNoResult(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.target.Command = fakeCopilotBinaryWithOutput(t, "", 1)

	var events []domain.AgentEvent
	_, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		OnEvent: func(e domain.AgentEvent) { events = append(events, e) },
	})

	requireAgentError(t, err, domain.ErrPortExit)
	if !hasEventType(events, domain.EventTurnFailed) {
		t.Error("EventTurnFailed not delivered for non-zero exit")
	}

	var agentErr *domain.AgentError
	const wantMessage = "the agent runtime exited before responding: exit status 1"
	if errors.As(err, &agentErr) && agentErr.Message != wantMessage {
		t.Errorf("AgentError.Message = %q, want %q", agentErr.Message, wantMessage)
	}
}

func TestRunTurn_NoOutputExitZero(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.target.Command = fakeCopilotBinaryWithOutput(t, "", 0)

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		OnEvent: func(e domain.AgentEvent) { events = append(events, e) },
	})

	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}
	requireAgentError(t, err, domain.ErrPortExit)
	if !hasEventType(events, domain.EventTurnFailed) {
		t.Error("EventTurnFailed not delivered for no-output exit 0")
	}

	var agentErr *domain.AgentError
	const wantMessage = "the agent runtime exited before responding: exit status 0"
	if errors.As(err, &agentErr) && agentErr.Message != wantMessage {
		t.Errorf("AgentError.Message = %q, want %q", agentErr.Message, wantMessage)
	}
}

func TestRunTurn_SingleSignalNoTerminalCompletes(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	tests := []struct {
		name  string
		jsonl string
	}{
		{
			name:  "assistant output only",
			jsonl: `{"type":"assistant.message","data":{"content":"hello there","toolRequests":[]}}` + "\n",
		},
		{
			name:  "tool activity only",
			jsonl: loadTestFixture(t, "tool_only_no_assistant_content.jsonl"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adapter, session := newTestSession(t, t.TempDir())
			state := session.Internal.(*sessionState)
			state.target.Command = fakeCopilotBinaryWithOutput(t, tt.jsonl, 0)

			result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
				OnEvent: func(domain.AgentEvent) {},
			})

			if err != nil {
				t.Fatalf("RunTurn() error = %v", err)
			}
			if result.ExitReason != domain.EventTurnCompleted {
				t.Errorf("RunTurn().ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
			}
		})
	}
}

func TestRunTurn_NoOutputTokensNoResultCompletes(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	fixture := loadTestFixture(t, "tool_use_no_output_tokens_no_result.jsonl")
	if strings.Contains(fixture, "outputTokens") {
		t.Fatalf("fixture tool_use_no_output_tokens_no_result.jsonl contains an output-token field, want none")
	}

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.target.Command = fakeCopilotBinaryWithOutput(t, fixture, 0)

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		OnEvent: func(e domain.AgentEvent) { events = append(events, e) },
	})

	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("RunTurn().ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}
	if !hasEventType(events, domain.EventToolResult) {
		t.Error("EventToolResult not delivered for the retained tool.execution_complete record")
	}
}

func TestRunTurn_WorkSignalsObservedAcrossFixtureCorpus(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	t.Run("assistant content alone is observed", func(t *testing.T) {
		t.Setenv("COPILOT_HOME", t.TempDir())
		adapter, session := newTestSession(t, t.TempDir())
		state := session.Internal.(*sessionState)
		state.target.Command = fakeCopilotBinaryWithOutput(t, loadTestFixture(t, "simple_session.jsonl"), 0)

		if _, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
			OnEvent: func(domain.AgentEvent) {},
		}); err != nil {
			t.Fatalf("RunTurn() error = %v", err)
		}
		if !state.work.Observed() {
			t.Error("state.work.Observed() = false after simple_session.jsonl, want true")
		}
	})

	t.Run("tool call alone is observed", func(t *testing.T) {
		adapter, session := newTestSession(t, t.TempDir())
		state := session.Internal.(*sessionState)
		state.target.Command = fakeCopilotBinaryWithOutput(t, loadTestFixture(t, "tool_only_no_assistant_content.jsonl"), 0)

		if _, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
			OnEvent: func(domain.AgentEvent) {},
		}); err != nil {
			t.Fatalf("RunTurn() error = %v", err)
		}
		if !state.work.Observed() {
			t.Error("state.work.Observed() = false after tool_only_no_assistant_content.jsonl, want true")
		}
	})
}

func TestRunTurn_SecondTurnFailsAfterFirstTurnBothSignals(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.target.Command = firstTurnOnlyBinary(t, loadTestFixture(t, "tool_use_no_output_tokens_no_result.jsonl"))

	result1, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		OnEvent: func(domain.AgentEvent) {},
	})
	if err != nil {
		t.Fatalf("RunTurn(first) error = %v", err)
	}
	if result1.ExitReason != domain.EventTurnCompleted {
		t.Fatalf("RunTurn(first).ExitReason = %q, want %q", result1.ExitReason, domain.EventTurnCompleted)
	}

	result2, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		OnEvent: func(domain.AgentEvent) {},
	})
	if result2.ExitReason != domain.EventTurnFailed {
		t.Errorf("RunTurn(second).ExitReason = %q, want %q (a first turn with both signals must not carry forward)", result2.ExitReason, domain.EventTurnFailed)
	}
	requireAgentError(t, err, domain.ErrPortExit)
}

func TestRunTurn_PartialOutputNoResultExitZero(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")
	t.Setenv("COPILOT_HOME", t.TempDir())

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)

	const jsonl = `{"type":"assistant.message","timestamp":"2026-04-08T00:00:00Z","data":{"role":"assistant","content":"hello"}}` + "\n"
	state.target.Command = fakeCopilotBinaryWithOutput(t, jsonl, 0)

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		OnEvent: func(e domain.AgentEvent) { events = append(events, e) },
	})

	if err != nil {
		t.Fatalf("expected nil error for partial output exit 0, got %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}
	if !hasEventType(events, domain.EventTurnCompleted) {
		t.Error("EventTurnCompleted not delivered for partial-output exit 0")
	}
	agenttest.AssertMeasurementAbsent(t, events, result)

	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		ExitObserved: true,
		ExitCode:     0,
		Work:         agentcore.WorkPresent,
	}, result, err)
}

func TestRunTurn_StderrWarnOnNoOutputExitZero(t *testing.T) {
	// No t.Parallel(): installs a global slog default.
	spy := agenttest.InstallLogSpy(t)
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.target.Command = fakeCopilotBinaryWithStderrAndExit(t, "Invalid JSON in --additional-mcp-config", 0)

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "do the thing",
		OnEvent: func(e domain.AgentEvent) { events = append(events, e) },
	})
	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}
	requireAgentError(t, err, domain.ErrPortExit)

	warnLines := agenttest.RequireWarnLines(t, spy, "agent exited without producing output")
	found := false
	for _, line := range warnLines {
		if strings.Contains(line, "Invalid JSON") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("WARN lines %v do not contain \"Invalid JSON\"", warnLines)
	}
}

func TestRunTurn_ContextCancelled(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, session := newTestSession(t, t.TempDir())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var events []domain.AgentEvent
	_, err := adapter.RunTurn(ctx, session, domain.RunTurnParams{
		OnEvent: func(e domain.AgentEvent) { events = append(events, e) },
	})

	requireAgentError(t, err, domain.ErrTurnCancelled)
	if !hasEventType(events, domain.EventTurnCancelled) {
		t.Error("EventTurnCancelled not delivered")
	}
}

func TestRunTurn_MalformedLines(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.target.Command = fakeCopilotBinaryWithOutput(t, loadTestFixture(t, "malformed_lines.jsonl"), 0)

	var events []domain.AgentEvent
	_, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		OnEvent: func(e domain.AgentEvent) { events = append(events, e) },
	})

	if err != nil {
		t.Fatalf("RunTurn() unexpected error = %v", err)
	}
	if !hasEventType(events, domain.EventMalformed) {
		t.Error("EventMalformed not delivered for malformed JSONL line")
	}
}

func TestRunTurn_ToolUseEvents(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.target.Command = fakeCopilotBinaryWithOutput(t, loadTestFixture(t, "tool_calls_mapped.jsonl"), 0)

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		OnEvent: func(e domain.AgentEvent) { events = append(events, e) },
	})

	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("result.ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}

	type call struct {
		name     string
		kind     domain.ToolKind
		argument string
	}
	want := []call{
		{"bash", domain.ToolKindShell, "go test ./..."},
		{"view", domain.ToolKindRead, "/tmp/copilot-test/main.go"},
		{"edit", domain.ToolKindEdit, "/tmp/copilot-test/main.go"},
		{"skill", domain.ToolKindSkill, "demo-skill"},
		{"github-mcp-server.get_issue", "", ""},
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

	dispositiontest.AssertWorkEvidenceConsistent(t, events, result, err)
}

// With --no-ask-user every recognized request is a permission request with
// no reply channel, so the runtime's own refusal must not end the turn.
func TestRunTurn_ToolDeniedContinuesTurn(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	const deniedJSONL = `{"type":"tool.execution_complete","data":{"toolCallId":"tc-1","toolName":"bash","success":false,"error":{"code":"denied","message":"denied by policy"}}}
{"type":"session.task_complete","data":{"summary":"done","success":true}}
{"type":"result","timestamp":"2026-03-30T22:19:28.097Z","sessionId":"dd112233-4455-6677-8899-aabbccddeeff","exitCode":0,"usage":{"premiumRequests":0,"totalApiDurationMs":0,"sessionDurationMs":0}}`

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.target.Command = fakeCopilotBinaryWithOutput(t, deniedJSONL, 0)

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		OnEvent: func(e domain.AgentEvent) { events = append(events, e) },
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("ExitReason = %q, want %q (turn must continue past the runtime's own refusal)", result.ExitReason, domain.EventTurnCompleted)
	}

	wantNotice := agentcore.DecideHumanRequest(agentcore.ClassPermission, false, agentcore.AnswerRuntimeRefused).Notice
	var found bool
	for _, ev := range events {
		if ev.Type == domain.EventNotification && ev.Message == wantNotice {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("no EventNotification with Message = %q found among events %v", wantNotice, events)
	}
}

func TestRunTurn_NonZeroResultExitCode(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	const failResultJSONL = `{"type":"result","timestamp":"2026-03-30T22:19:28.097Z","sessionId":"cc990fc2-1234-5678-9abc-def012345678","exitCode":1,"usage":{"premiumRequests":0,"totalApiDurationMs":0,"sessionDurationMs":0}}`

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.target.Command = fakeCopilotBinaryWithOutput(t, failResultJSONL, 0)

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		OnEvent: func(e domain.AgentEvent) { events = append(events, e) },
	})

	requireAgentError(t, err, domain.ErrTurnFailed)
	if !hasEventType(events, domain.EventTurnFailed) {
		t.Error("EventTurnFailed not delivered for non-zero result exit code")
	}

	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		Terminal:        agentcore.TerminalFailure,
		TerminalMessage: "non-zero exit in result event",
		ExitObserved:    true,
		ExitCode:        0,
		Work:            agentcore.WorkAbsent,
	}, result, err)
}

const (
	providerFailureMessage = "400 scripted model: request 1 (POST /v1/messages) arrived after the script was exhausted"
	failedResultLine       = `{"type":"result","timestamp":"2026-03-30T22:19:28.097Z","sessionId":"cc990fc2-1234-5678-9abc-def012345678","exitCode":1,"usage":{"premiumRequests":0,"totalApiDurationMs":0,"sessionDurationMs":0}}` + "\n"
	noProviderMessage      = "non-zero exit in result event"
)

func sessionErrorLine(data string) string {
	return `{"type":"session.error","timestamp":"2026-03-30T22:19:27.500Z","data":` + data + `}` + "\n"
}

func sessionErrorMessageLine(message string) string {
	return sessionErrorLine(`{"message":` + strconv.Quote(message) + `}`)
}

func runFailedTurn(t *testing.T, adapter domain.AgentAdapter, session domain.Session) (*domain.AgentError, domain.AgentEvent, []domain.AgentEvent) {
	t.Helper()

	var events []domain.AgentEvent
	_, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		OnEvent: func(e domain.AgentEvent) { events = append(events, e) },
	})

	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) {
		t.Fatalf("RunTurn error = %v (%T), want *domain.AgentError", err, err)
	}
	if agentErr.Kind != domain.ErrTurnFailed {
		t.Errorf("RunTurn AgentError.Kind = %q, want %q", agentErr.Kind, domain.ErrTurnFailed)
	}
	failed, ok := findEventByType(events, domain.EventTurnFailed)
	if !ok {
		t.Fatalf("RunTurn events = %v, want an EventTurnFailed", events)
	}
	return agentErr, failed, events
}

func TestRunTurn_SessionErrorMessageIsFailureReason(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	longMessage := strings.Repeat("provider detail ", 512)

	tests := []struct {
		name   string
		stdout string
		want   string
	}{
		{
			name:   "session error then non-zero result",
			stdout: sessionErrorMessageLine(providerFailureMessage) + failedResultLine,
			want:   providerFailureMessage,
		},
		{
			name:   "no session error keeps the old text",
			stdout: failedResultLine,
			want:   noProviderMessage,
		},
		{
			name:   "two session errors keep the last",
			stdout: sessionErrorMessageLine("first failure") + sessionErrorMessageLine(providerFailureMessage) + failedResultLine,
			want:   providerFailureMessage,
		},
		{
			name:   "message is trimmed",
			stdout: sessionErrorMessageLine("  \t"+providerFailureMessage+"\n ") + failedResultLine,
			want:   providerFailureMessage,
		},
		{
			name:   "message is not truncated",
			stdout: sessionErrorMessageLine(longMessage) + failedResultLine,
			want:   strings.TrimSpace(longMessage),
		},
		{
			name:   "empty message replaces an earlier one",
			stdout: sessionErrorMessageLine(providerFailureMessage) + sessionErrorLine(`{}`) + failedResultLine,
			want:   noProviderMessage,
		},
		{
			name:   "undecodable payload keeps an earlier message",
			stdout: sessionErrorMessageLine(providerFailureMessage) + sessionErrorLine(`"not an object"`) + failedResultLine,
			want:   providerFailureMessage,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adapter, session := newTestSession(t, t.TempDir())
			state := session.Internal.(*sessionState)
			state.target.Command = fakeCopilotBinaryWithOutput(t, tt.stdout, 0)

			agentErr, failed, _ := runFailedTurn(t, adapter, session)

			if agentErr.Message != tt.want {
				t.Errorf("RunTurn AgentError.Message = %q, want %q", agentErr.Message, tt.want)
			}
			if failed.Message != tt.want {
				t.Errorf("EventTurnFailed.Message = %q, want %q", failed.Message, tt.want)
			}
		})
	}
}

func TestRunTurn_SessionErrorEmitsBlankPing(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.target.Command = fakeCopilotBinaryWithOutput(t, sessionErrorMessageLine(providerFailureMessage)+failedResultLine, 0)

	_, _, events := runFailedTurn(t, adapter, session)

	var blankPings int
	for _, e := range events {
		if e.Message == "session.error" {
			t.Errorf("%s event leaked the protocol event name as its message", e.Type)
		}
		if e.Type == domain.EventNotification && e.Message == "" {
			blankPings++
		}
	}
	if blankPings == 0 {
		t.Errorf("RunTurn events = %v, want a blank ping for session.error", events)
	}
}

func TestRunTurn_SessionErrorMessageDoesNotLeakIntoNextTurn(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)

	state.target.Command = fakeCopilotBinaryWithOutput(t, sessionErrorMessageLine(providerFailureMessage)+failedResultLine, 0)
	first, _, _ := runFailedTurn(t, adapter, session)
	if first.Message != providerFailureMessage {
		t.Fatalf("RunTurn(first) AgentError.Message = %q, want %q", first.Message, providerFailureMessage)
	}

	state.target.Command = fakeCopilotBinaryWithOutput(t, failedResultLine, 0)
	second, failed, _ := runFailedTurn(t, adapter, session)

	if second.Message != noProviderMessage {
		t.Errorf("RunTurn(second) AgentError.Message = %q, want %q", second.Message, noProviderMessage)
	}
	if failed.Message != noProviderMessage {
		t.Errorf("RunTurn(second) EventTurnFailed.Message = %q, want %q", failed.Message, noProviderMessage)
	}
}

func TestRunTurn_TurnFailed_APIDurationMS(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	const failJSONL = `{"type":"result","timestamp":"2026-03-30T22:19:28.097Z","sessionId":"cc990fc2-1234-5678-9abc-def012345678","exitCode":1,"usage":{"premiumRequests":2,"totalApiDurationMs":5000,"sessionDurationMs":9000}}`

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.target.Command = fakeCopilotBinaryWithOutput(t, failJSONL, 0)

	var events []domain.AgentEvent
	_, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		OnEvent: func(e domain.AgentEvent) { events = append(events, e) },
	})

	requireAgentError(t, err, domain.ErrTurnFailed)

	e, ok := findEventByType(events, domain.EventTurnFailed)
	if !ok {
		t.Fatal("EventTurnFailed not delivered")
	}
	const wantAPIDurationMS int64 = 5000
	if e.APIDurationMS != wantAPIDurationMS {
		t.Errorf("EventTurnFailed.APIDurationMS = %d, want %d", e.APIDurationMS, wantAPIDurationMS)
	}

	assertNoTokenUsageAfter(t, events, domain.EventTurnFailed)
}

func TestRunTurn_ContextCancelledBeforeStart(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, session := newTestSession(t, t.TempDir())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(ctx, session, domain.RunTurnParams{
		Prompt: "test",
		OnEvent: func(e domain.AgentEvent) {
			events = append(events, e)
		},
	})
	requireAgentError(t, err, domain.ErrTurnCancelled)
	if result.ExitReason != domain.EventTurnCancelled {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCancelled)
	}
	if !hasEventType(events, domain.EventTurnCancelled) {
		t.Error("EventTurnCancelled not delivered on pre-cancelled context")
	}
}

func TestStopSession_NilProc(t *testing.T) {
	t.Parallel()

	adapter, err := NewCopilotAdapter()
	if err != nil {
		t.Fatalf("NewCopilotAdapter: %v", err)
	}
	session := domain.Session{
		Internal: &sessionState{
			target: agentcore.LaunchTarget{WorkspacePath: t.TempDir()},
		},
	}
	if err := adapter.StopSession(context.Background(), session); err != nil {
		t.Fatalf("StopSession(nil proc) error = %v", err)
	}
}

func TestStopSession_TerminatesProcess(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	hangBin := agenttest.FakeRuntime(t, t.TempDir(), "copilot", agenttest.OutputScenario, agenttest.Output{Hang: true})

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.target.Command = hangBin

	processStarted := make(chan struct{}, 1)
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		adapter.RunTurn(context.Background(), session, domain.RunTurnParams{ //nolint:errcheck // error is irrelevant for this test
			OnEvent: func(e domain.AgentEvent) {
				if e.Type == domain.EventSessionStarted {
					select {
					case processStarted <- struct{}{}:
					default:
					}
				}
			},
		})
	}()

	waittest.Receive(t, processStarted, "process start")

	if err := adapter.StopSession(context.Background(), session); err != nil {
		t.Fatalf("StopSession() error = %v", err)
	}

	waittest.Receive(t, runDone, "RunTurn return after StopSession")
}

func fakeCopilotBinaryWithStderrAndExit(t *testing.T, stderrLine string, exitCode int) string {
	t.Helper()
	return agenttest.FakeRuntime(t, t.TempDir(), "copilot", agenttest.OutputScenario, agenttest.Output{
		Stderr:   stderrLine + "\n",
		ExitCode: exitCode,
	})
}

func TestRunTurn_StderrWarnOnExitCode127(t *testing.T) {
	// No t.Parallel(): installs a global slog default.
	spy := agenttest.InstallLogSpy(t)
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.target.Command = fakeCopilotBinaryWithStderrAndExit(t, "license check failed: no valid license", 127)

	result, runErr := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "do the thing",
		OnEvent: func(domain.AgentEvent) {},
	})
	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}
	var agentErr *domain.AgentError
	if !errors.As(runErr, &agentErr) || agentErr.Kind != domain.ErrPortExit {
		t.Errorf("error = %v, want AgentError{Kind: %q}", runErr, domain.ErrPortExit)
	}

	warnLines := agenttest.RequireWarnLines(t, spy, "exit code 127")
	if !strings.Contains(warnLines[0], "license check failed") {
		t.Errorf("WARN line = %q, want it to contain \"license check failed\"", warnLines[0])
	}
}

func TestRunTurn_StderrWarnOnNonZeroExit(t *testing.T) {
	// No t.Parallel(): installs a global slog default.
	spy := agenttest.InstallLogSpy(t)
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.target.Command = fakeCopilotBinaryWithStderrAndExit(t, "internal agent panic", 1)

	result, runErr := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "do the thing",
		OnEvent: func(domain.AgentEvent) {},
	})
	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}
	var agentErr *domain.AgentError
	if !errors.As(runErr, &agentErr) || agentErr.Kind != domain.ErrPortExit {
		t.Errorf("error = %v, want AgentError{Kind: %q}", runErr, domain.ErrPortExit)
	}

	warnLines := agenttest.RequireWarnLines(t, spy, "non-zero exit")
	if !strings.Contains(warnLines[0], "internal agent panic") {
		t.Errorf("WARN line = %q, want it to contain \"internal agent panic\"", warnLines[0])
	}
}

func TestRunTurn_StderrNoWarnOnSuccess(t *testing.T) {
	// No t.Parallel(): installs a global slog default.
	spy := agenttest.InstallLogSpy(t)
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)

	const successJSONL = `{"type":"session.task_complete","data":{"summary":"done","success":true}}
{"type":"result","timestamp":"2026-03-30T22:19:28.097Z","sessionId":"no-warn-success-sess","exitCode":0,"usage":{"premiumRequests":0,"totalApiDurationMs":0,"sessionDurationMs":0}}`
	state.target.Command = agenttest.FakeRuntime(t, t.TempDir(), "copilot", agenttest.OutputScenario, agenttest.Output{
		Stdout: successJSONL + "\n",
		Stderr: "minor diagnostic\n",
	})

	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "do the thing",
		OnEvent: func(domain.AgentEvent) {},
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}

	if warnLines := spy.WarnLines(); len(warnLines) != 0 {
		t.Errorf("success path produced %d WARN lines for stderr, want 0; got %v", len(warnLines), warnLines)
	}
}

const streamCheckpointLine = `{"type":"session.usage_checkpoint","data":{"accountingSnapshot":{"modelMetrics":{"claude-sonnet-5":{"usage":{"inputTokens":193011,"outputTokens":596,"cacheReadTokens":154053,"cacheWriteTokens":38948,"reasoningTokens":149}}}}}}`

func TestAssertUsageReporting(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")
	t.Setenv("COPILOT_HOME", t.TempDir())

	lines := strings.Split(strings.TrimRight(loadTestFixture(t, "tool_use_no_output_tokens.jsonl"), "\n"), "\n")
	resultAt := len(lines) - 1
	stdout := strings.Join(slices.Concat(lines[:resultAt], []string{streamCheckpointLine}, lines[resultAt:]), "\n") + "\n"

	adapter, session := newTestSession(t, t.TempDir())
	session.Internal.(*sessionState).target.Command = fakeCopilotBinaryWithOutput(t, stdout, 0)

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		Prompt:  "read main.go",
		OnEvent: func(e domain.AgentEvent) { events = append(events, e) },
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}

	agenttest.AssertUsageReporting(t, "copilot-cli", []agenttest.UsageReportingCase{
		{Name: "checkpoint supplies the figure", Events: events, Result: result},
	})
}

func TestRunTurn_UnnormalizableCheckpointSupersedesEarlierOne(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")
	t.Setenv("COPILOT_HOME", t.TempDir())

	unnormalizable := strings.Replace(streamCheckpointLine, `"inputTokens":193011`, `"inputTokens":null`, 1)
	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.target.Command = fakeCopilotBinaryWithOutput(t, streamCheckpointLine+"\n"+unnormalizable+"\n"+`{"type":"result","sessionId":"s","exitCode":1}`+"\n", 0)

	_, _ = adapter.RunTurn(context.Background(), session, domain.RunTurnParams{Prompt: "p", OnEvent: func(domain.AgentEvent) {}})

	if state.turnReading == nil || len(state.turnReading.ByModel) != 1 || state.turnReading.ByModel[0].Input != nil {
		t.Errorf("turnReading = %+v, want the later checkpoint with input unknown", state.turnReading)
	}
	if state.turnReplied {
		t.Error("turnReplied = true after a result with exit code 1, want false")
	}
}

func TestRunTurn_SuccessfulResultZeroOutputTokens(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	const successJSONL = `{"type":"session.task_complete","data":{"summary":"done","success":true}}
{"type":"result","timestamp":"2026-04-08T00:00:01Z","sessionId":"zero-output-success-session","exitCode":0,"usage":{"premiumRequests":1,"totalApiDurationMs":10}}`

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.target.Command = fakeCopilotBinaryWithOutput(t, successJSONL, 0)

	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		OnEvent: func(domain.AgentEvent) {},
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}

	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		Terminal:     agentcore.TerminalSuccess,
		ExitObserved: true,
		ExitCode:     0,
		Work:         agentcore.WorkAbsent,
	}, result, err)
}

func TestRunTurn_SuccessMessageStaysEmpty(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")
	t.Setenv("COPILOT_HOME", t.TempDir())

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.target.Command = fakeCopilotBinaryWithOutput(t, loadTestFixture(t, "simple_session.jsonl"), 0)

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		OnEvent: func(e domain.AgentEvent) { events = append(events, e) },
	})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Fatalf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}

	e, ok := findEventByType(events, domain.EventTurnCompleted)
	if !ok {
		t.Fatal("turn_completed event not delivered")
	}
	if e.Message != "" {
		t.Errorf("turn_completed Message = %q, want empty (copilot's result event carries no completion text)", e.Message)
	}
}

func TestRunTurn_WorkPredicateIsPerTurn(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)

	const outputJSONL = `{"type":"assistant.message","timestamp":"2026-04-08T00:00:00Z","data":{"role":"assistant","content":"hello","outputTokens":10}}` + "\n"
	state.target.Command = firstTurnOnlyBinary(t, outputJSONL)

	result1, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		OnEvent: func(domain.AgentEvent) {},
	})
	if err != nil {
		t.Fatalf("RunTurn(first) error = %v", err)
	}
	if result1.ExitReason != domain.EventTurnCompleted {
		t.Fatalf("RunTurn(first).ExitReason = %q, want %q", result1.ExitReason, domain.EventTurnCompleted)
	}

	result2, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		OnEvent: func(domain.AgentEvent) {},
	})
	if result2.ExitReason != domain.EventTurnFailed {
		t.Errorf("RunTurn(second).ExitReason = %q, want %q (per-turn work predicate must not carry the first turn's output forward)", result2.ExitReason, domain.EventTurnFailed)
	}
	requireAgentError(t, err, domain.ErrPortExit)
}

func TestRunTurn_CompleteVersusIncompleteDisposition(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")
	t.Setenv("COPILOT_HOME", t.TempDir())

	completeAdapter, completeSession := newTestSession(t, t.TempDir())
	completeState := completeSession.Internal.(*sessionState)
	completeState.target.Command = fakeCopilotBinaryWithOutput(t, loadTestFixture(t, "simple_session.jsonl"), 0)

	completeResult, completeErr := completeAdapter.RunTurn(context.Background(), completeSession, domain.RunTurnParams{
		OnEvent: func(domain.AgentEvent) {},
	})
	if completeErr != nil {
		t.Fatalf("RunTurn(complete) error = %v", completeErr)
	}
	if completeResult.ExitReason != domain.EventTurnCompleted {
		t.Errorf("RunTurn(complete).ExitReason = %q, want %q", completeResult.ExitReason, domain.EventTurnCompleted)
	}
	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		Terminal:     agentcore.TerminalSuccess,
		ExitObserved: true,
		ExitCode:     0,
		Work:         agentcore.WorkPresent,
	}, completeResult, completeErr)

	incompleteAdapter, incompleteSession := newTestSession(t, t.TempDir())
	incompleteState := incompleteSession.Internal.(*sessionState)
	incompleteState.target.Command = fakeCopilotBinaryWithOutput(t, loadTestFixture(t, "autopilot_no_report.jsonl"), 0)

	incompleteResult, incompleteErr := incompleteAdapter.RunTurn(context.Background(), incompleteSession, domain.RunTurnParams{
		OnEvent: func(domain.AgentEvent) {},
	})
	if incompleteResult.ExitReason != domain.EventTurnFailed {
		t.Errorf("RunTurn(incomplete).ExitReason = %q, want %q", incompleteResult.ExitReason, domain.EventTurnFailed)
	}
	requireAgentError(t, incompleteErr, domain.ErrTurnIncomplete)
	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		Terminal:        agentcore.TerminalIncomplete,
		TerminalMessage: "raise copilot-cli.max_autopilot_continues if the turn needs more steps",
	}, incompleteResult, incompleteErr)

	if completeResult.ExitReason == incompleteResult.ExitReason {
		t.Errorf("ExitReason for the complete and incomplete endings both = %q, want them to differ", completeResult.ExitReason)
	}
}

func TestRunTurn_TaskCompleteSuccessFalse(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	const summary = "ran into a blocker"
	stream := fmt.Sprintf(`{"type":"session.task_complete","data":{"summary":%q,"success":false}}
{"type":"result","timestamp":"2026-04-08T00:00:04Z","sessionId":"success-false-sess","exitCode":0,"usage":{"premiumRequests":0,"totalApiDurationMs":0}}`, summary)

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.target.Command = fakeCopilotBinaryWithOutput(t, stream, 0)

	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		OnEvent: func(domain.AgentEvent) {},
	})

	if result.ExitReason != domain.EventTurnFailed {
		t.Errorf("RunTurn().ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}
	requireAgentError(t, err, domain.ErrTurnFailed)
	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{
		Terminal:          agentcore.TerminalFailure,
		TerminalErrorKind: domain.ErrTurnFailed,
		TerminalMessage:   completionFailureMessage(summary),
	}, result, err)
}

func TestRunTurn_TaskCompleteSuccessKeyAbsent(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	const stream = `{"type":"session.task_complete","data":{"summary":"all set"}}
{"type":"result","timestamp":"2026-04-08T00:00:05Z","sessionId":"absent-flag-sess","exitCode":0,"usage":{"premiumRequests":0,"totalApiDurationMs":0}}`

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.target.Command = fakeCopilotBinaryWithOutput(t, stream, 0)

	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		OnEvent: func(domain.AgentEvent) {},
	})

	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("RunTurn().ExitReason = %q, want %q (absent success key defaults to success)", result.ExitReason, domain.EventTurnCompleted)
	}
	dispositiontest.AssertDispositionContract(t, agentcore.TurnEvidence{Terminal: agentcore.TerminalSuccess}, result, err)
}

func TestRunTurn_IncompleteEndingLogsContinuationCounts(t *testing.T) {
	// No t.Parallel(): installs a global slog default. agenttest.LogSpy
	// reports only the "line" attribute, and this test reads others.
	var logOutput bytes.Buffer
	previousDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logOutput, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previousDefault) })

	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	adapter, session := newTestSession(t, t.TempDir())
	state := session.Internal.(*sessionState)
	state.target.Command = fakeCopilotBinaryWithOutput(t, loadTestFixture(t, "autopilot_no_report.jsonl"), 0)

	result, err := adapter.RunTurn(context.Background(), session, domain.RunTurnParams{
		OnEvent: func(domain.AgentEvent) {},
	})
	if result.ExitReason != domain.EventTurnFailed {
		t.Fatalf("RunTurn().ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}
	requireAgentError(t, err, domain.ErrTurnIncomplete)

	got := logOutput.String()
	if !strings.Contains(got, `msg="copilot turn ended without a task-completion report"`) {
		t.Errorf("log output missing the expected warn message; got:\n%s", got)
	}
	if !strings.Contains(got, "autopilot_continuations_observed=1") {
		t.Errorf("log output missing autopilot_continuations_observed=1 (the fixture carries exactly one isAutopilotContinuation line); got:\n%s", got)
	}
	if !strings.Contains(got, "max_autopilot_continues=50") {
		t.Errorf("log output missing max_autopilot_continues=50 (the unconfigured default); got:\n%s", got)
	}
}
