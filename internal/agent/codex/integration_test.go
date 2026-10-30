// Integration tests for the Codex CLI agent adapter.
//
// Required environment variables:
//
//	SORTIE_CODEX_TEST=1     enable this suite
//	CODEX_API_KEY           Codex API key; the live cases need it, the
//	                        scripted-model case needs only the gate
//
// Optional environment variables:
//
//	SORTIE_CODEX_COMMAND    override the default "codex app-server" binary command
//	SORTIE_CODEX_MODEL      override the default "gpt-5.4-mini" model
//
// Run:
//
//	SORTIE_CODEX_TEST=1 CODEX_API_KEY=... make test PKG=./internal/agent/codex/... RUN=Integration
package codex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest/credentialtest"
	"github.com/sortie-ai/sortie/internal/agent/agenttest/fakemodel"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
)

func skipUnlessCodexIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("SORTIE_CODEX_TEST") != "1" {
		t.Skip("skipping Codex integration test: set SORTIE_CODEX_TEST=1 to enable")
	}
	if os.Getenv("CODEX_API_KEY") == "" {
		t.Skip("skipping Codex integration test: CODEX_API_KEY must be set")
	}
}

func skipUnlessCodexGate(t *testing.T) {
	t.Helper()
	if os.Getenv("SORTIE_CODEX_TEST") != "1" {
		t.Skip("skipping Codex integration test: set SORTIE_CODEX_TEST=1 to enable")
	}
}

func integrationConfig() map[string]any {
	model := os.Getenv("SORTIE_CODEX_MODEL")
	if model == "" {
		model = "gpt-5.4-mini"
	}
	return map[string]any{
		"approval_policy": "never",
		"thread_sandbox":  "workspaceWrite",
		"model":           model,
	}
}

func integrationCommand() string {
	if cmd := os.Getenv("SORTIE_CODEX_COMMAND"); cmd != "" {
		return cmd
	}
	return "codex app-server"
}

func runtimeCommand() string {
	command, _, _ := strings.Cut(integrationCommand(), " app-server")
	return command
}

func integrationAgentConfig() domain.AgentConfig {
	return domain.AgentConfig{
		Command:       integrationCommand(),
		TurnTimeoutMS: 90000,
		ReadTimeoutMS: 30000,
	}
}

func gitInitWorkspace(t *testing.T) string {
	t.Helper()
	// The Codex app-server requires a git repository by default.
	dir := t.TempDir()
	cmd := exec.CommandContext(context.Background(), "git", "-C", dir, "init")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("gitInitWorkspace: git init: %v\n%s", err, out)
	}
	return dir
}

func requireAgentErrorKind(t *testing.T, err error, wantKind domain.AgentErrorKind) {
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

func mustNewAdapter(t *testing.T) *CodexAdapter {
	t.Helper()
	a, err := NewCodexAdapter()
	if err != nil {
		t.Fatalf("NewCodexAdapter: %v", err)
	}
	return a.(*CodexAdapter)
}

func TestIntegration_StartSession(t *testing.T) {
	skipUnlessCodexIntegration(t)

	adapter := mustNewAdapter(t)
	workspace := gitInitWorkspace(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	session, err := adapter.StartSession(ctx, domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig:   integrationAgentConfig(),
		Settings:      integrationConfig(),
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	t.Cleanup(func() { _ = adapter.StopSession(context.Background(), session) })

	if session.ID == "" {
		t.Error("session.ID is empty; expected non-empty thread ID from app-server")
	}
	if session.AgentPID == "" {
		t.Error("session.AgentPID is empty; expected PID of the persistent subprocess")
	}
	if session.Internal == nil {
		t.Error("session.Internal is nil")
	}

	state, ok := session.Internal.(*sessionState)
	if !ok {
		t.Fatalf("session.Internal type = %T, want *sessionState", session.Internal)
	}
	if state.model == "" {
		t.Error("state.model is empty; expected the effective model reported by thread/start")
	}
}

func TestIntegration_StopSession(t *testing.T) {
	skipUnlessCodexIntegration(t)

	adapter := mustNewAdapter(t)
	workspace := gitInitWorkspace(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	session, err := adapter.StartSession(ctx, domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig:   integrationAgentConfig(),
		Settings:      integrationConfig(),
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	if err := adapter.StopSession(context.Background(), session); err != nil {
		t.Fatalf("StopSession (idle): %v", err)
	}
	if err := adapter.StopSession(context.Background(), session); err != nil {
		t.Errorf("StopSession (second call): %v", err)
	}
}

func TestIntegration_StartSession_InvalidCommand(t *testing.T) {
	skipUnlessCodexIntegration(t)

	adapter := mustNewAdapter(t)

	_, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: "sortie-nonexistent-codex-binary-99999"},
	})
	if err == nil {
		t.Fatal("expected error for nonexistent command, got nil")
	}

	requireAgentErrorKind(t, err, domain.ErrAgentNotFound)
}

func scriptedProviderCommand(command, url string) string {
	// The redirect rides in -c overrides because the runtime ignores
	// OPENAI_BASE_URL and a project-local config file; zero retry counts make a
	// refused request end the turn instead of being replayed.
	pairs := []string{
		"model_provider=\"scripted\"",
		"model_providers.scripted.name=\"scripted\"",
		fmt.Sprintf("model_providers.scripted.base_url=%q", url+"/v1"),
		"model_providers.scripted.wire_api=\"responses\"",
		"model_providers.scripted.env_key=\"CODEX_API_KEY\"",
		"model_providers.scripted.request_max_retries=0",
		"model_providers.scripted.stream_max_retries=0",
		"model_providers.scripted.supports_websockets=false",
	}
	args := []string{command}
	for _, pair := range pairs {
		args = append(args, "-c", pair)
	}
	return strings.Join(args, " ")
}

func TestIntegration_ScriptedModel(t *testing.T) {
	skipUnlessCodexGate(t)

	fakemodel.AssertConformance(t, fakemodel.Binding{
		Kind: "codex",
		Passthrough: map[string]any{
			"approval_policy": "never",
			// The scripted tool call runs a shell command, and on a hosted CI
			// runner the workspaceWrite sandbox cannot bring up loopback in
			// its network namespace, so every command fails before it runs.
			"thread_sandbox": "dangerFullAccess",
			"model":          "scripted-model",
		},
		// Under full disk write access the runtime approves every MCP call by
		// itself, so only a narrower sandbox shows that it honors the approval
		// grant on the server. The unknown model slug keeps fallback metadata
		// with tool search and code mode off, so the request declares the MCP
		// tools themselves.
		ToolServerPassthrough: map[string]any{
			"approval_policy": "never",
			"thread_sandbox":  "workspaceWrite",
			"model":           "scripted-model",
		},
		CredentialEnv: []string{"CODEX_API_KEY"},
		Read:          fakemodel.CatFile,
		Spend: &fakemodel.SpendSources{
			Live:    domain.UsageProvenance{Source: usage.SourceOutput, Strategy: usage.StrategyLive},
			Records: domain.UsageProvenance{Source: usage.SourceJournal, Strategy: usage.StrategyRecords},
		},
		SSH: &fakemodel.SSHVariant{
			Package: "@openai/codex",
			// app-server rejects --version; the top-level command reports it.
			HostCommand: runtimeCommand(),
			RemoteCommand: func(env fakemodel.Environment) string {
				return scriptedProviderCommand("codex app-server", env.URL)
			},
		},
		Launch: func(t *testing.T, env fakemodel.Environment) fakemodel.Launch {
			return fakemodel.Launch{
				Config: domain.AgentConfig{
					Command:       scriptedProviderCommand(integrationCommand(), env.URL),
					TurnTimeoutMS: 300000,
					ReadTimeoutMS: 30000,
				},
				// CODEX_HOME also confines the runtime's automatic
				// trusted-project record to the isolated home.
				Env: map[string]string{"CODEX_HOME": env.Home},
			}
		},
		Inspect: func(t *testing.T, run fakemodel.Run) {
			if run.Environment.Scenario != fakemodel.ScenarioTurn {
				return
			}
			if run.Result.SessionID != run.Session.ID {
				t.Errorf("TurnResult.SessionID = %q, want %q", run.Result.SessionID, run.Session.ID)
			}
		},
	})
}

func TestIntegration_CredentialVerification(t *testing.T) {
	skipUnlessCodexIntegration(t)

	passthrough := map[string]any{}
	adapter, err := NewCodexAdapter()
	if err != nil {
		t.Fatalf("NewCodexAdapter: %v", err)
	}
	params := func(t *testing.T) domain.StartSessionParams {
		return domain.StartSessionParams{WorkspacePath: gitInitWorkspace(t), AgentConfig: integrationAgentConfig(), Settings: passthrough}
	}

	t.Run("working credential verifies", func(t *testing.T) {
		credentialtest.VerifyLiveUsage(t, "codex", adapter, params(t), passthrough)
	})

	t.Run("refused credential ends credential_unverified", func(t *testing.T) {
		credentialtest.SetRefusedCredential(t, "SORTIE_CODEX_CREDENTIAL_ENV")

		_, err := credentialtest.VerifyLive(adapter, params(t))
		credentialtest.RequireRefused(t, err)
	})
}

func TestIntegration_EarlyExit(t *testing.T) {
	skipUnlessCodexIntegration(t)

	adapter, err := NewCodexAdapter()
	if err != nil {
		t.Fatalf("NewCodexAdapter: %v", err)
	}
	unknownSwitchConfig := integrationAgentConfig()
	unknownSwitchConfig.Command += " --sortie-unknown-switch"

	t.Run("verification session with an unknown switch", func(t *testing.T) {
		params := domain.StartSessionParams{WorkspacePath: gitInitWorkspace(t), AgentConfig: unknownSwitchConfig}
		if _, err := credentialtest.VerifyLive(adapter, params); err == nil {
			t.Skip("configured runtime accepted --sortie-unknown-switch, so it cannot exercise the early-exit report")
		} else {
			credentialtest.RequireEarlyExitReport(t, err)
		}
	})

	t.Run("working session with an unknown switch", func(t *testing.T) {
		params := domain.StartSessionParams{WorkspacePath: gitInitWorkspace(t), AgentConfig: unknownSwitchConfig}
		session, err := adapter.StartSession(context.Background(), params)
		if err == nil {
			_ = adapter.StopSession(context.Background(), session)
			t.Skip("configured runtime accepted --sortie-unknown-switch, so it cannot exercise the early-exit report")
		} else {
			credentialtest.RequireEarlyExitReport(t, err)
		}
	})
}
