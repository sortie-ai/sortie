// Integration tests for the Copilot CLI adapter.
//
// Required environment variables:
//
//	SORTIE_COPILOT_TEST=1          enable this suite
//
// Optional environment variables:
//
//	SORTIE_COPILOT_COMMAND         override the default "copilot" binary
//	SORTIE_COPILOT_MODEL           override the default "gpt-5-mini" model
//
// Authentication: the live cases need one of COPILOT_GITHUB_TOKEN, GH_TOKEN or
// GITHUB_TOKEN, or an authenticated gh CLI; the scripted-model case needs only
// the gate.
//
// Run:
//
//	SORTIE_COPILOT_TEST=1 make test PKG=./internal/agent/copilot/... RUN=Integration
package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest/credentialtest"
	"github.com/sortie-ai/sortie/internal/agent/agenttest/fakemodel"
	"github.com/sortie-ai/sortie/internal/domain"
)

func skipUnlessCopilotIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("SORTIE_COPILOT_TEST") != "1" {
		t.Skip("skipping Copilot CLI integration test: set SORTIE_COPILOT_TEST=1 to enable")
	}
}

func integrationConfig() map[string]any {
	model := os.Getenv("SORTIE_COPILOT_MODEL")
	if model == "" {
		model = "gpt-5-mini"
	}
	return map[string]any{
		"max_autopilot_continues": float64(5),
		"model":                   model,
	}
}

func integrationCommand() string {
	if cmd := os.Getenv("SORTIE_COPILOT_COMMAND"); cmd != "" {
		return cmd
	}
	return "copilot"
}

func TestIntegration_StartSession(t *testing.T) {
	skipUnlessCopilotIntegration(t)

	adapter, err := NewCopilotAdapter()
	if err != nil {
		t.Fatalf("NewCopilotAdapter: %v", err)
	}

	workspace := t.TempDir()
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		Settings:      integrationConfig(),
		WorkspacePath: workspace,
		AgentConfig:   domain.AgentConfig{Command: integrationCommand()},
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	t.Cleanup(func() { _ = adapter.StopSession(context.Background(), session) })

	// Copilot CLI does not pre-assign a session ID: it is set only after
	// the first turn's result event.
	if session.Internal == nil {
		t.Error("Session.Internal is nil")
	}
}

func TestIntegration_StopSession(t *testing.T) {
	skipUnlessCopilotIntegration(t)

	adapter, err := NewCopilotAdapter()
	if err != nil {
		t.Fatalf("NewCopilotAdapter: %v", err)
	}

	workspace := t.TempDir()
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		Settings:      integrationConfig(),
		WorkspacePath: workspace,
		AgentConfig:   domain.AgentConfig{Command: integrationCommand()},
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	if err := adapter.StopSession(context.Background(), session); err != nil {
		t.Fatalf("StopSession (idle): %v", err)
	}
}

func TestIntegration_StartSession_InvalidCommand(t *testing.T) {
	skipUnlessCopilotIntegration(t)

	adapter, err := NewCopilotAdapter()
	if err != nil {
		t.Fatalf("NewCopilotAdapter: %v", err)
	}

	_, err = adapter.StartSession(context.Background(), domain.StartSessionParams{
		Settings:      integrationConfig(),
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: "sortie-nonexistent-copilot-99999"},
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
	skipUnlessCopilotIntegration(t)

	t.Setenv("COPILOT_GITHUB_TOKEN", "unused-token-for-quota-endpoint")

	binding := fakemodel.Binding{
		Kind:             "copilot-cli",
		Passthrough:      map[string]any{"model": "scripted-model", "max_autopilot_continues": float64(5)},
		CredentialEnv:    []string{"COPILOT_PROVIDER_API_KEY"},
		UserInstructions: filepath.Join(".copilot", "copilot-instructions.md"),
		Read:             fakemodel.ReadFile,
		SSH: &fakemodel.SSHVariant{
			Package:       "@github/copilot",
			HostCommand:   integrationCommand(),
			RemoteCommand: func(fakemodel.Environment) string { return "copilot" },
		},
		Finish: func(text string) fakemodel.ToolChoice {
			summary, _ := json.Marshal(map[string]string{"summary": text})
			return fakemodel.Named("task_complete", summary)
		},
		Launch: func(t *testing.T, env fakemodel.Environment) fakemodel.Launch {
			return fakemodel.Launch{
				Config: domain.AgentConfig{
					Command:       integrationCommand(),
					TurnTimeoutMS: 300000,
					ReadTimeoutMS: 30000,
				},
				Env: map[string]string{
					"COPILOT_PROVIDER_BASE_URL": env.URL,
					"COPILOT_PROVIDER_TYPE":     "anthropic",
					"COPILOT_OFFLINE":           "1",
					"COPILOT_HOME":              filepath.Join(env.Home, ".copilot"),
				},
			}
		},
		Inspect: func(t *testing.T, run fakemodel.Run) {
			for _, ev := range run.Events {
				if len(ev.UsageWindows) != 0 {
					t.Errorf("event %q reports %d usage windows on a custom provider, want none", ev.Type, len(ev.UsageWindows))
				}
			}
			if run.Environment.Scenario != fakemodel.ScenarioTurn {
				return
			}
			state, ok := run.Session.Internal.(*sessionState)
			if !ok {
				t.Fatalf("session.Internal type = %T, want *sessionState", run.Session.Internal)
			}
			// The disposition never consults Work on a normal turn, so only
			// the observer shows it fired.
			if !state.work.Observed() {
				t.Error("state.work.Observed() = false after a scripted turn, want true")
			}
		},
	}
	fakemodel.AssertConformance(t, binding)

	// The runtime retries a 429 that names no Retry-After six times before it
	// refuses.
	refusal := slices.Repeat([]fakemodel.Response{{Status: http.StatusTooManyRequests, Text: "Rate limit exceeded."}}, 6)
	for _, remote := range []bool{false, true} {
		t.Run(fmt.Sprintf("usage limit refusal remote=%v", remote), func(t *testing.T) {
			run := fakemodel.RunCustomTurn(t, binding, fakemodel.CustomTurn{Script: refusal, Prompt: "Reply with the word ok.", Remote: remote})

			var agentErr *domain.AgentError
			if !errors.As(run.Err, &agentErr) || agentErr.Kind != domain.ErrUsageLimit || agentErr.UsageLimit == nil || !agentErr.UsageLimit.ModelOnly {
				t.Fatalf("RunTurn error = %v, want a model-only usage_limit error", run.Err)
			}
		})
	}
}

func TestIntegration_CredentialVerification(t *testing.T) {
	skipUnlessCopilotIntegration(t)

	passthrough := map[string]any{}
	adapter, err := NewCopilotAdapter()
	if err != nil {
		t.Fatalf("NewCopilotAdapter: %v", err)
	}
	params := func(t *testing.T) domain.StartSessionParams {
		return domain.StartSessionParams{
			WorkspacePath: t.TempDir(),
			AgentConfig:   domain.AgentConfig{Command: integrationCommand(), ReadTimeoutMS: 30000},
			Settings:      passthrough,
		}
	}

	t.Run("working credential verifies and leaves no session state", func(t *testing.T) {
		t.Setenv("COPILOT_HOME", t.TempDir())

		result := credentialtest.VerifyLiveUsage(t, "copilot-cli", adapter, params(t), passthrough)
		if result.SessionID == "" {
			t.Fatal("VerifyCredential() TurnResult.SessionID is empty, want the minted verification session id")
		}
		root, rootErr := sessionStateRoot(os.Getenv, os.UserHomeDir)
		if rootErr != nil {
			t.Fatalf("sessionStateRoot: %v", rootErr)
		}
		if _, statErr := os.Stat(filepath.Join(root, result.SessionID)); !os.IsNotExist(statErr) {
			t.Errorf("session-state directory for verification session %q still present after the step, want deleted (stat error = %v)", result.SessionID, statErr)
		}
	})

	t.Run("refused credential ends credential_unverified", func(t *testing.T) {
		credentialtest.SetRefusedCredential(t, "SORTIE_COPILOT_CREDENTIAL_ENV")

		_, err := credentialtest.VerifyLive(adapter, params(t))
		credentialtest.RequireRefused(t, err)
	})
}

func TestIntegration_EarlyExit(t *testing.T) {
	skipUnlessCopilotIntegration(t)

	adapter, err := NewCopilotAdapter()
	if err != nil {
		t.Fatalf("NewCopilotAdapter: %v", err)
	}
	unknownSwitchConfig := domain.AgentConfig{Command: integrationCommand() + " --sortie-unknown-switch", ReadTimeoutMS: 30000}

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
