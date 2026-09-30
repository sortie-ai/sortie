package claude

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/agenttest/credentialtest"
	"github.com/sortie-ai/sortie/internal/agent/agenttest/fakemodel"
	"github.com/sortie-ai/sortie/internal/domain"
)

// skipUnlessIntegration skips the current test when the SORTIE_CLAUDE_TEST
// environment variable is not set to "1", so disabled integration tests are
// reported as skipped rather than silently passing.
func skipUnlessIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("SORTIE_CLAUDE_TEST") != "1" {
		t.Skip("skipping Claude Code integration test: set SORTIE_CLAUDE_TEST=1 to enable")
	}
}

// singleTurnIntegrationConfig builds the adapter config map for
// single-turn integration tests. Session persistence is disabled to
// prevent ~/.claude/ pollution from repeated test runs, which makes the
// resulting config non-resumable: a test that resumes a session must not
// use this helper.
func singleTurnIntegrationConfig(t *testing.T) map[string]any {
	t.Helper()
	model := os.Getenv("SORTIE_CLAUDE_MODEL")
	if model == "" {
		model = "claude-haiku-4-5"
	}
	return map[string]any{
		"session_persistence": false,
		"model":               model,
	}
}

// integrationCommand returns the Claude Code binary path from the
// SORTIE_CLAUDE_COMMAND environment variable, defaulting to "claude".
func integrationCommand(t *testing.T) string {
	t.Helper()
	if cmd := os.Getenv("SORTIE_CLAUDE_COMMAND"); cmd != "" {
		return cmd
	}
	return "claude"
}

func TestIntegration_StartSession(t *testing.T) {
	skipUnlessIntegration(t)

	adapter, err := NewClaudeCodeAdapter()
	if err != nil {
		t.Fatalf("NewClaudeCodeAdapter: %v", err)
	}

	workspace := t.TempDir()

	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		Settings:      singleTurnIntegrationConfig(t),
		WorkspacePath: workspace,
		AgentConfig:   domain.AgentConfig{Command: integrationCommand(t)},
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	t.Cleanup(func() { _ = adapter.StopSession(context.Background(), session) })

	if session.ID == "" {
		t.Error("Session.ID is empty")
	}
	if session.Internal == nil {
		t.Error("Session.Internal is nil")
	}
}

func TestIntegration_StopSession(t *testing.T) {
	skipUnlessIntegration(t)

	adapter, err := NewClaudeCodeAdapter()
	if err != nil {
		t.Fatalf("NewClaudeCodeAdapter: %v", err)
	}

	workspace := t.TempDir()

	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		Settings:      singleTurnIntegrationConfig(t),
		WorkspacePath: workspace,
		AgentConfig:   domain.AgentConfig{Command: integrationCommand(t)},
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	if err := adapter.StopSession(context.Background(), session); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
}

func TestIntegration_StartSession_InvalidCommand(t *testing.T) {
	skipUnlessIntegration(t)

	adapter, err := NewClaudeCodeAdapter()
	if err != nil {
		t.Fatalf("NewClaudeCodeAdapter: %v", err)
	}

	_, err = adapter.StartSession(context.Background(), domain.StartSessionParams{
		Settings:      singleTurnIntegrationConfig(t),
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: "sortie-nonexistent-binary-99999"},
	})
	if err == nil {
		t.Fatal("expected error for nonexistent command, got nil")
	}

	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) {
		t.Fatalf("error type = %T, want *domain.AgentError", err)
	}
	if agentErr.Kind != domain.ErrAgentNotFound {
		t.Errorf("AgentError.Kind = %q, want %q", agentErr.Kind, domain.ErrAgentNotFound)
	}
}

func TestIntegration_ScriptedModel(t *testing.T) {
	skipUnlessIntegration(t)

	fakemodel.AssertConformance(t, fakemodel.Binding{
		Kind:        "claude-code",
		Passthrough: map[string]any{"model": "claude-haiku-4-5", "session_persistence": false},
		Read:        fakemodel.ReadFile,
		Launch: func(t *testing.T, env fakemodel.Environment) fakemodel.Launch {
			return fakemodel.Launch{
				Config: domain.AgentConfig{
					Command:       integrationCommand(t),
					TurnTimeoutMS: 300000,
					ReadTimeoutMS: 30000,
				},
				Env: map[string]string{
					"ANTHROPIC_BASE_URL":                       env.URL,
					"CLAUDE_CONFIG_DIR":                        filepath.Join(env.Home, ".claude"),
					"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
					// The runtime warms its connection with a HEAD request to
					// the base address unless a proxy variable is set, and the
					// endpoint answers only model requests. An https proxy
					// leaves the plain-http loopback requests direct.
					"HTTPS_PROXY": "http://127.0.0.1:9",
				},
			}
		},
		Inspect: func(t *testing.T, run fakemodel.Run) {
			if run.Environment.Scenario != fakemodel.ScenarioTurn {
				return
			}
			state, ok := run.Session.Internal.(*sessionState)
			if !ok {
				t.Fatalf("session.Internal type = %T, want *sessionState", run.Session.Internal)
			}
			// A normal turn decides at the terminal-success row and never
			// consults Work, so the disposition alone does not show the
			// observer fired against the installed runtime.
			if !state.work.Observed() {
				t.Error("state.work.Observed() = false after a scripted turn, want true")
			}
		},
	})
}

func TestIntegration_RunTurn_ContextCancellation(t *testing.T) {
	skipUnlessIntegration(t)

	adapter, err := NewClaudeCodeAdapter()
	if err != nil {
		t.Fatalf("NewClaudeCodeAdapter: %v", err)
	}

	workspace := t.TempDir()
	if err := os.WriteFile(workspace+"/dummy.txt", []byte("test"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		Settings:      singleTurnIntegrationConfig(t),
		WorkspacePath: workspace,
		AgentConfig:   domain.AgentConfig{Command: integrationCommand(t)},
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	t.Cleanup(func() { _ = adapter.StopSession(context.Background(), session) })

	var mu sync.Mutex
	var events []domain.AgentEvent
	onEvent := func(e domain.AgentEvent) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	}

	// Use a 2-second timeout: long enough for subprocess startup (~100ms)
	// but well below the minimum Claude API round-trip (~3-5s), ensuring
	// the context always expires before the turn completes.
	shortCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	prompt := "Use the Bash tool to execute the command 'sleep 15'. Do nothing else."

	result, err := adapter.RunTurn(shortCtx, session, domain.RunTurnParams{
		Prompt:  prompt,
		OnEvent: onEvent,
	})
	if err == nil {
		t.Fatal("expected error from cancelled RunTurn, got nil")
	}

	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) {
		t.Fatalf("error type = %T, want *domain.AgentError", err)
	}
	if agentErr.Kind != domain.ErrTurnCancelled {
		t.Errorf("AgentError.Kind = %q, want %q", agentErr.Kind, domain.ErrTurnCancelled)
	}
	if result.ExitReason != domain.EventTurnCancelled {
		t.Errorf("TurnResult.ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCancelled)
	}
}

func TestIntegration_SessionResume(t *testing.T) {
	skipUnlessIntegration(t)

	model := os.Getenv("SORTIE_CLAUDE_MODEL")
	if model == "" {
		model = "claude-haiku-4-5"
	}
	firstSettings := map[string]any{"model": model}
	resumedSettings := map[string]any{"model": "claude-sonnet-4-5", "effort": "low"}

	workspace := t.TempDir()
	noopEvent := func(domain.AgentEvent) {}

	adapter, err := NewClaudeCodeAdapter()
	if err != nil {
		t.Fatalf("NewClaudeCodeAdapter: %v", err)
	}

	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig:   domain.AgentConfig{Command: integrationCommand(t)},
		Settings:      firstSettings,
	})
	if err != nil {
		t.Fatalf("StartSession (first turn): %v", err)
	}
	t.Cleanup(func() { _ = adapter.StopSession(context.Background(), session) })

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	result1, err := adapter.RunTurn(ctx, session, domain.RunTurnParams{
		Prompt:  "Say exactly: turn one",
		OnEvent: noopEvent,
	})
	if err != nil {
		t.Fatalf("RunTurn (first turn): %v", err)
	}
	if result1.ExitReason != domain.EventTurnCompleted {
		t.Fatalf("first turn ExitReason = %q, want %q", result1.ExitReason, domain.EventTurnCompleted)
	}
	if result1.SessionID == "" {
		t.Fatal("first turn TurnResult.SessionID is empty")
	}

	session2, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath:   workspace,
		AgentConfig:     domain.AgentConfig{Command: integrationCommand(t)},
		ResumeSessionID: result1.SessionID,
		Settings:        resumedSettings,
	})
	if err != nil {
		t.Fatalf("StartSession (resumed turn): %v", err)
	}
	t.Cleanup(func() { _ = adapter.StopSession(context.Background(), session2) })

	ctx2, cancel2 := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel2()

	result2, err := adapter.RunTurn(ctx2, session2, domain.RunTurnParams{
		Prompt:  "What did I say in the previous message?",
		OnEvent: noopEvent,
	})
	if err != nil {
		t.Fatalf("RunTurn (resumed turn): %v", err)
	}
	if result2.ExitReason != domain.EventTurnCompleted {
		t.Errorf("resumed turn ExitReason = %q, want %q", result2.ExitReason, domain.EventTurnCompleted)
	}
	// A turn that silently started a fresh session would also complete,
	// so completion alone does not show the session was resumed. The CLI
	// appends to the existing conversation under the same identifier
	// unless --fork-session is passed, which this adapter never passes,
	// so an identifier that changed means no resume happened.
	if result2.SessionID != result1.SessionID {
		t.Errorf("resumed turn SessionID = %q, want %q: the turn did not resume the first turn's session",
			result2.SessionID, result1.SessionID)
	}
}

func TestIntegration_CredentialVerification(t *testing.T) {
	skipUnlessIntegration(t)

	passthrough := map[string]any{}
	adapter, err := NewClaudeCodeAdapter()
	if err != nil {
		t.Fatalf("NewClaudeCodeAdapter: %v", err)
	}
	params := func(t *testing.T) domain.StartSessionParams {
		return domain.StartSessionParams{
			WorkspacePath: t.TempDir(),
			AgentConfig:   domain.AgentConfig{Command: integrationCommand(t), ReadTimeoutMS: 30000},
			Settings:      passthrough,
		}
	}

	t.Run("working credential verifies", func(t *testing.T) {
		credentialtest.VerifyLiveUsage(t, "claude-code", adapter, params(t), passthrough)
	})

	t.Run("refused credential ends credential_unverified", func(t *testing.T) {
		// Not parallel: t.Setenv carries the invalid credential.
		credentialtest.SetRefusedCredential(t, "SORTIE_CLAUDE_CREDENTIAL_ENV")

		_, err := credentialtest.VerifyLive(adapter, params(t))
		credentialtest.RequireRefused(t, err)
	})
}

func TestIntegration_EarlyExit(t *testing.T) {
	skipUnlessIntegration(t)

	adapter, err := NewClaudeCodeAdapter()
	if err != nil {
		t.Fatalf("NewClaudeCodeAdapter: %v", err)
	}
	unknownSwitchConfig := domain.AgentConfig{Command: integrationCommand(t) + " --sortie-unknown-switch", ReadTimeoutMS: 30000}

	t.Run("verification session with an unknown switch", func(t *testing.T) {
		params := domain.StartSessionParams{WorkspacePath: t.TempDir(), AgentConfig: unknownSwitchConfig}
		if _, err := credentialtest.VerifyLive(adapter, params); err == nil {
			t.Skip("configured runtime accepted --sortie-unknown-switch, so it cannot exercise the early-exit report")
		} else {
			credentialtest.RequireEarlyExitReport(t, err)
		}
	})

	t.Run("working session with an unknown switch", func(t *testing.T) {
		params := domain.StartSessionParams{WorkspacePath: t.TempDir(), AgentConfig: unknownSwitchConfig}
		if err := credentialtest.RunWorkingLive(adapter, params); err == nil {
			t.Skip("configured runtime accepted --sortie-unknown-switch, so it cannot exercise the early-exit report")
		} else {
			credentialtest.RequireEarlyExitReport(t, err)
		}
	})
}

func TestIntegration_ToolServerIdentity(t *testing.T) {
	skipUnlessIntegration(t)

	agenttest.AssertToolServerIdentity(t, func(ctx context.Context, workspacePath, mcpConfigPath string) error {
		adapter, err := NewClaudeCodeAdapter()
		if err != nil {
			return err
		}

		session, err := adapter.StartSession(ctx, domain.StartSessionParams{
			Settings:      singleTurnIntegrationConfig(t),
			WorkspacePath: workspacePath,
			AgentConfig:   domain.AgentConfig{Command: integrationCommand(t)},
			MCPConfigPath: mcpConfigPath,
		})
		if err != nil {
			return err
		}
		defer func() { _ = adapter.StopSession(context.Background(), session) }()

		_, err = adapter.RunTurn(ctx, session, domain.RunTurnParams{
			Prompt:  "Say exactly: hello",
			OnEvent: func(domain.AgentEvent) {},
		})
		return err
	})
}
