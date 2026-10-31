package claude

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest/credentialtest"
	"github.com/sortie-ai/sortie/internal/agent/agenttest/fakemodel"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/waittest"
)

func skipUnlessIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("SORTIE_CLAUDE_TEST") != "1" {
		t.Skip("skipping Claude Code integration test: set SORTIE_CLAUDE_TEST=1 to enable")
	}
}

func singleTurnIntegrationConfig(t *testing.T) map[string]any {
	t.Helper()
	model := os.Getenv("SORTIE_CLAUDE_MODEL")
	if model == "" {
		model = "claude-haiku-4-5"
	}
	return map[string]any{
		// Off so repeated runs leave nothing in ~/.claude; such a session
		// cannot be resumed.
		"session_persistence": false,
		"model":               model,
	}
}

func integrationCommand(t *testing.T) string {
	t.Helper()
	if cmd := os.Getenv("SORTIE_CLAUDE_COMMAND"); cmd != "" {
		return cmd
	}
	return "claude"
}

func usageWindowsOf(events []domain.AgentEvent) []domain.UsageWindow {
	var windows []domain.UsageWindow
	for _, e := range events {
		windows = append(windows, e.UsageWindows...)
	}
	return windows
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

func scriptedLaunch(t *testing.T, env fakemodel.Environment) fakemodel.Launch {
	t.Helper()
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
}

func TestIntegration_ScriptedModel(t *testing.T) {
	skipUnlessIntegration(t)

	binding := fakemodel.Binding{
		Kind: "claude-code",
		// Persistence stays on: the continuation scenario resumes a session the
		// runtime stored, and the files stay in the test's temporary home.
		Passthrough:         map[string]any{"model": "claude-haiku-4-5"},
		UserInstructions:    filepath.Join(".claude", "CLAUDE.md"),
		ProjectInstructions: "CLAUDE.md",
		Read:                fakemodel.ReadFile,
		Spend: &fakemodel.SpendSources{
			Live:             domain.UsageProvenance{Source: usage.SourceOutput, Strategy: usage.StrategyLive},
			Records:          domain.UsageProvenance{Source: usage.SourceJournal, Strategy: usage.StrategyRecords},
			RecordsAtTurnEnd: usageSources.Records.WrittenAtTurnEnd,
		},
		SSH: &fakemodel.SSHVariant{
			Package:       "@anthropic-ai/claude-code",
			HostCommand:   integrationCommand(t),
			RemoteCommand: func(fakemodel.Environment) string { return "claude" },
		},
		Launch: scriptedLaunch,
		Inspect: func(t *testing.T, run fakemodel.Run) {
			if run.Environment.Scenario != fakemodel.ScenarioTurn {
				return
			}
			state, ok := run.Session.Internal.(*sessionState)
			if !ok {
				t.Fatalf("session.Internal type = %T, want *sessionState", run.Session.Internal)
			}
			// A normal turn never consults Work, so the disposition alone
			// does not show the observer fired against the installed runtime.
			if !state.work.Observed() {
				t.Error("state.work.Observed() = false after a scripted turn, want true")
			}
		},
	}

	fakemodel.AssertConformance(t, binding)

	t.Run("subscription rate limits", func(t *testing.T) {
		const oauthSentinel = "sortie-scripted-oauth-token-731948265"
		subscription := binding
		subscription.Launch = func(t *testing.T, env fakemodel.Environment) fakemodel.Launch {
			launch := binding.Launch(t, env)
			launch.Env["ANTHROPIC_API_KEY"] = ""
			launch.Env["ANTHROPIC_AUTH_TOKEN"] = ""
			return launch
		}

		run := fakemodel.RunCustomTurn(t, subscription, fakemodel.CustomTurn{
			Script: []fakemodel.Response{{
				Text: "ok",
				Header: http.Header{
					"Anthropic-Ratelimit-Unified-Status":                 {"allowed_warning"},
					"Anthropic-Ratelimit-Unified-Reset":                  {"4102444800"},
					"Anthropic-Ratelimit-Unified-Representative-Claim":   {"five_hour"},
					"Anthropic-Ratelimit-Unified-5h-Utilization":         {"0.9"},
					"Anthropic-Ratelimit-Unified-5h-Reset":               {"4102444800"},
					"Anthropic-Ratelimit-Unified-5h-Surpassed-Threshold": {"0.9"},
					"Anthropic-Ratelimit-Unified-5h-Status":              {"allowed_warning"},
				},
			}},
			Prompt:   "Reply with the word ok.",
			Sentinel: oauthSentinel,
		})

		if run.Result.ExitReason != domain.EventTurnCompleted {
			t.Fatalf("TurnResult.ExitReason = %q, want %q (err %v)", run.Result.ExitReason, domain.EventTurnCompleted, run.Err)
		}
		for _, ex := range run.Exchanges {
			if got, want := ex.Header.Get("Authorization"), "Bearer "+oauthSentinel; ex.Kind == fakemodel.ExchangeTurn && got != want {
				t.Errorf("turn exchange %d Authorization = %q, want %q", ex.Seq, got, want)
			}
		}
		fakemodel.AssertContainment(t, run)

		warned := usageWindowsOf(run.Events)
		if len(warned) != 1 {
			t.Fatalf("usage windows of the warned turn = %+v, want exactly one", warned)
		}
		if w := warned[0]; w.Key != "five_hour" || w.Label != "5-hour limit" || w.Status != domain.UsageWindowWarning {
			t.Errorf("warned window = {Key %q, Label %q, Status %q}, want {%q, %q, %q}", w.Key, w.Label, w.Status, "five_hour", "5-hour limit", domain.UsageWindowWarning)
		}
		if w := warned[0]; w.UsedPercent == nil || *w.UsedPercent != 90 {
			t.Errorf("warned window UsedPercent = %v, want 90", warned[0].UsedPercent)
		}
		if w, want := warned[0], time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC); !w.ResetsAt.Equal(want) {
			t.Errorf("warned window ResetsAt = %v, want %v", w.ResetsAt, want)
		}

		refused := fakemodel.RunCustomTurn(t, subscription, fakemodel.CustomTurn{
			Script: []fakemodel.Response{{
				Status: http.StatusTooManyRequests,
				Header: http.Header{
					"Anthropic-Ratelimit-Unified-Status":               {"rejected"},
					"Anthropic-Ratelimit-Unified-Reset":                {"4102444800"},
					"Anthropic-Ratelimit-Unified-Representative-Claim": {"five_hour"},
					"Anthropic-Ratelimit-Unified-Overage-Status":       {"rejected"},
				},
			}},
			Prompt:   "Reply with the word ok.",
			Sentinel: oauthSentinel,
		})

		var agentErr *domain.AgentError
		if !errors.As(refused.Err, &agentErr) {
			t.Fatalf("refused turn error = %v, want *domain.AgentError", refused.Err)
		}
		if agentErr.Kind != domain.ErrUsageLimit || agentErr.UsageLimit == nil {
			t.Fatalf("refused turn error kind = %q (UsageLimit %v), want %q with a UsageLimit", agentErr.Kind, agentErr.UsageLimit, domain.ErrUsageLimit)
		}
		if want := time.Unix(4102444800, 0); !agentErr.UsageLimit.ResetsAt.Equal(want) {
			t.Errorf("UsageLimit.ResetsAt = %v, want %v", agentErr.UsageLimit.ResetsAt, want)
		}

		rejected := usageWindowsOf(refused.Events)
		if len(rejected) != 1 {
			t.Fatalf("usage windows of the refused turn = %+v, want exactly one", rejected)
		}
		if w := rejected[0]; w.Key != "five_hour" || w.Status != domain.UsageWindowExhausted {
			t.Errorf("refused window = {Key %q, Status %q}, want {%q, %q}", w.Key, w.Status, "five_hour", domain.UsageWindowExhausted)
		}
	})
}

func usdAmountOf(amounts []domain.ReportedAmount, model string) float64 {
	for _, a := range amounts {
		if a.Unit == domain.UnitUSD && a.Model == model {
			return a.Amount
		}
	}
	return 0
}

func TestIntegration_CostUSDIsPerTurn(t *testing.T) {
	skipUnlessIntegration(t)

	const model = "claude-sonnet-4-5"
	answer := fakemodel.Response{Text: "ok", Usage: fakemodel.Usage{Prompt: 1200, Candidates: 40}}
	server := fakemodel.Start(t, []fakemodel.Response{answer, answer})
	home := t.TempDir()
	for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(name, home)
	}
	t.Setenv("NO_PROXY", "127.0.0.1,localhost")
	t.Setenv("ANTHROPIC_API_KEY", "sortie-scripted-credential")
	launch := scriptedLaunch(t, fakemodel.Environment{URL: server.URL(), Home: home})
	for name, value := range launch.Env {
		t.Setenv(name, value)
	}
	adapter, err := NewClaudeCodeAdapter()
	if err != nil {
		t.Fatalf("NewClaudeCodeAdapter: %v", err)
	}
	ctx := waittest.Context(t)
	session, err := adapter.StartSession(ctx, domain.StartSessionParams{
		Settings:      map[string]any{"model": model},
		WorkspacePath: t.TempDir(),
		AgentConfig:   launch.Config,
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	t.Cleanup(func() { _ = adapter.StopSession(waittest.CleanupContext(t), session) })

	var amounts [2]float64
	for i := range amounts {
		result, err := adapter.RunTurn(ctx, session, domain.RunTurnParams{Prompt: "Reply with the word ok.", OnEvent: func(domain.AgentEvent) {}})
		if err != nil {
			t.Fatalf("RunTurn %d: %v", i+1, err)
		}
		amounts[i] = usdAmountOf(result.ReportedAmounts, model)
	}

	if amounts[0] <= 0 {
		t.Fatalf("USD amount of %q after turn 1 = %v, want above 0", model, amounts[0])
	}
	if amounts[1] != 2*amounts[0] {
		t.Errorf("USD amount of %q after turn 2 = %v, want %v, twice turn 1", model, amounts[1], 2*amounts[0])
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
