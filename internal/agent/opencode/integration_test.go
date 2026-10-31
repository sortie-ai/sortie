package opencode_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/agenttest/credentialtest"
	"github.com/sortie-ai/sortie/internal/agent/agenttest/fakemodel"
	"github.com/sortie-ai/sortie/internal/agent/opencode"
	"github.com/sortie-ai/sortie/internal/agent/opencode/sessionexport"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/registry"
)

func skipIfNotEnabled(t *testing.T) {
	t.Helper()
	if os.Getenv("SORTIE_OPENCODE_TEST") != "1" {
		t.Skip("set SORTIE_OPENCODE_TEST=1 to run opencode integration tests")
	}
}

func integrationCommand() string {
	if cmd := os.Getenv("SORTIE_OPENCODE_COMMAND"); cmd != "" {
		return cmd
	}
	return "opencode"
}

func integrationConfig() map[string]any {
	model := os.Getenv("SORTIE_OPENCODE_MODEL")
	if model == "" {
		model = "opencode/big-pickle"
	}

	cfg := map[string]any{
		"dangerously_skip_permissions": true,
		"disable_autocompact":          true,
		"model":                        model,
	}
	return cfg
}

func mustNewAdapter(t *testing.T) domain.AgentAdapter {
	t.Helper()
	factory, err := registry.Agents.Get("opencode")
	if err != nil {
		t.Fatalf("registry.Agents.Get(opencode): %v", err)
	}
	a, err := factory()
	if err != nil {
		t.Fatalf("factory(): %v", err)
	}
	return a
}

// coldStartReadTimeoutMS absorbs the SQLite migration a first launch runs,
// which can take over 30 seconds.
const coldStartReadTimeoutMS = 3 * 60 * 1000

func mustStartIntegrationSessionWith(t *testing.T, a domain.AgentAdapter, settings map[string]any) domain.Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	session, err := a.StartSession(ctx, domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig: domain.AgentConfig{
			Command:       integrationCommand(),
			ReadTimeoutMS: coldStartReadTimeoutMS,
		},
		Settings: settings,
	})
	if err != nil {
		t.Fatalf("StartSession(): %v", err)
	}
	return session
}

func collectAllEvents(t *testing.T, a domain.AgentAdapter, session domain.Session, prompt string) ([]domain.AgentEvent, domain.TurnResult) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), agenttest.LiveTurnBound)
	defer cancel()

	var events []domain.AgentEvent
	result, err := a.RunTurn(ctx, session, domain.RunTurnParams{
		Prompt: prompt,
		OnEvent: func(e domain.AgentEvent) {
			events = append(events, e)
		},
	})
	if err != nil {
		t.Logf("RunTurn error: %v", err)
	}
	return events, result
}

func scriptedProviderDocument(url string) string {
	// The key is an {env:} reference because opencode 2.x does not read the
	// provider's key variable on its own.
	return fmt.Sprintf(`{"$schema":"https://opencode.ai/config.json","provider":{"scripted":{"npm":"@ai-sdk/google","name":"scripted","options":{"baseURL":"%s/v1beta","apiKey":"{env:GOOGLE_GENERATIVE_AI_API_KEY}"},"models":{"scripted-model":{"name":"scripted-model"}}}}}`, url)
}

func TestIntegration_ScriptedModel(t *testing.T) {
	skipIfNotEnabled(t)

	scriptedSettings := func(skipPermissions bool) map[string]any {
		return map[string]any{
			"model":                        "scripted/scripted-model",
			"dangerously_skip_permissions": skipPermissions,
			"disable_autocompact":          true,
		}
	}

	binding := fakemodel.Binding{
		Kind:                "opencode",
		Passthrough:         scriptedSettings(true),
		CredentialEnv:       []string{"GOOGLE_GENERATIVE_AI_API_KEY"},
		Read:                fakemodel.ReadFile,
		ReportedModelPrefix: "scripted/",
		Spend: &fakemodel.SpendSources{
			Live:              domain.UsageProvenance{Source: usage.SourceJournal, Strategy: usage.StrategyRecords},
			Records:           domain.UsageProvenance{Source: usage.SourceJournal, Strategy: usage.StrategyRecords},
			RecordsAtTurnEnd:  sessionexport.Records.WrittenAtTurnEnd,
			CancelEndsProcess: true,
		},
		SSH: &fakemodel.SSHVariant{
			Package:       "@opencode/cli",
			HostCommand:   integrationCommand(),
			RemoteCommand: func(fakemodel.Environment) string { return "opencode" },
		},
		PermissionRefusal: &fakemodel.PermissionRefusal{
			Passthrough: scriptedSettings(false),
			Guarded:     fakemodel.ReadFile,
			Notice:      agentcore.DecideHumanRequest(agentcore.ClassPermission, false, agentcore.AnswerRuntimeRefused).Notice,
		},
		// 2.x offers MCP tools to the model only inside its Code Mode
		// runtime, which the execute tool scripts.
		SortieStatus: fakemodel.Named("execute", json.RawMessage(fmt.Sprintf(
			`{"code":"return await tools[\"%s\"].%s()"}`, agenttest.SortieToolsServer, agenttest.SortieStatusTool))),
		Launch: func(t *testing.T, env fakemodel.Environment) fakemodel.Launch {
			// The adapter drops an inherited OPENCODE_CONFIG_CONTENT and writes
			// its own, so the provider rides in a file OPENCODE_CONFIG names.
			document := filepath.Join(env.Home, "scripted-provider.json")
			if err := os.WriteFile(document, []byte(scriptedProviderDocument(env.URL)), 0o600); err != nil {
				t.Fatalf("write %s: %v", document, err)
			}
			return fakemodel.Launch{
				Config: domain.AgentConfig{
					Command:       integrationCommand(),
					TurnTimeoutMS: 300000,
					ReadTimeoutMS: coldStartReadTimeoutMS,
				},
				Env: map[string]string{"OPENCODE_CONFIG": document},
			}
		},
	}

	fakemodel.AssertConformance(t, binding)

	t.Run("export failure leaves the tool step counts as a lower bound", func(t *testing.T) {
		runtime := agenttest.WriteScript(t, t.TempDir(), "export-failing-opencode",
			"if [ \"$1\" = session ] && [ \"$2\" = export ]; then exit 1; fi\nexec "+integrationCommand()+" \"$@\"\n")
		failing := binding
		failing.Launch = func(t *testing.T, env fakemodel.Environment) fakemodel.Launch {
			launch := binding.Launch(t, env)
			launch.Config.Command = runtime
			return launch
		}
		file := filepath.Join(t.TempDir(), "scripted.txt")
		if err := os.WriteFile(file, []byte("scripted\n"), 0o600); err != nil {
			t.Fatalf("WriteFile(%q): %v", file, err)
		}

		toolCall := fakemodel.Usage{Prompt: 211, Candidates: 13, Thoughts: 5, CachedContent: 53}
		run := fakemodel.RunCustomTurn(t, failing, fakemodel.CustomTurn{
			Script: []fakemodel.Response{
				{Call: binding.Read(file), Usage: toolCall},
				{Text: "ok", Usage: fakemodel.Usage{Prompt: 307, Candidates: 17, Thoughts: 7, CachedContent: 61}},
			},
			Prompt: "Read the file, then reply with the word ok.",
		})

		if run.Err != nil {
			t.Fatalf("RunTurn() error = %v", run.Err)
		}
		want := domain.TokenUsage{
			InputTokens:     toolCall.Prompt,
			OutputTokens:    toolCall.Candidates + toolCall.Thoughts,
			TotalTokens:     toolCall.Prompt + toolCall.Candidates + toolCall.Thoughts,
			CacheReadTokens: toolCall.CachedContent,
		}
		if !run.Result.UsageMeasured {
			t.Error("RunTurn().UsageMeasured = false, want true")
		}
		if got := run.Result.Usage; got != want {
			t.Errorf("RunTurn().Usage = %+v, want %+v, the tool-call request alone", got, want)
		}
		wantProvenance := []domain.UsageProvenance{{Source: usage.SourceOutput, Strategy: usage.StrategyLive}}
		if got := run.Result.UsageProvenance; !slices.Equal(got, wantProvenance) {
			t.Errorf("RunTurn().UsageProvenance = %+v, want %+v", got, wantProvenance)
		}
		if !run.Result.SpendUnaccounted {
			t.Error("RunTurn().SpendUnaccounted = false, want true: the failed export leaves the figure a lower bound")
		}
	})

	for _, remote := range []bool{false, true} {
		name := "free usage limit"
		if remote {
			name += " remote"
		}
		t.Run(name, func(t *testing.T) {
			const retryAfter = time.Hour
			start := time.Now().Truncate(time.Millisecond)

			run := fakemodel.RunCustomTurn(t, binding, fakemodel.CustomTurn{
				Script: []fakemodel.Response{{
					Status: http.StatusTooManyRequests,
					Header: http.Header{"Retry-After": {"3600"}},
					Body:   []byte(`{"type": "error", "error": {"type": "FreeUsageLimitError", "message": "Rate limit exceeded. Please try again later."}}`),
				}},
				Prompt: "Reply with the word ok.",
				Remote: remote,
			})

			end := time.Now()
			var agentErr *domain.AgentError
			if !errors.As(run.Err, &agentErr) || agentErr.Kind != domain.ErrUsageLimit || agentErr.UsageLimit == nil {
				t.Fatalf("RunTurn() error = %v, want a usage_limit AgentError carrying a UsageLimit", run.Err)
			}
			if !agentErr.UsageLimit.ModelOnly {
				t.Error("UsageLimit.ModelOnly = false, want true")
			}
			assertResetWithin(t, "UsageLimit.ResetsAt", agentErr.UsageLimit.ResetsAt, start.Add(retryAfter), end.Add(retryAfter))
			var windows []domain.UsageWindow
			for _, e := range run.Events {
				windows = append(windows, e.UsageWindows...)
			}
			if len(windows) != 1 {
				t.Fatalf("usage windows of the refused turn = %+v, want exactly one", windows)
			}
			w := windows[0]
			if w.Label != "Free usage limit" || w.Status != domain.UsageWindowExhausted {
				t.Errorf("window = {Label %q, Status %q}, want {%q, %q}", w.Label, w.Status, "Free usage limit", domain.UsageWindowExhausted)
			}
			assertResetWithin(t, "window ResetsAt", w.ResetsAt, start.Add(retryAfter).Truncate(time.Second), end.Add(retryAfter))
		})
	}
}

func assertResetWithin(t *testing.T, what string, got, earliest, latest time.Time) {
	t.Helper()
	if got.Before(earliest) || got.After(latest) {
		t.Errorf("%s = %v, want within [%v, %v]", what, got, earliest, latest)
	}
}

func TestIntegration_InvalidModelFailure(t *testing.T) {
	skipIfNotEnabled(t)

	cfg := integrationConfig()
	cfg["model"] = "nonexistent/nonexistent"

	a := mustNewAdapter(t)

	session := mustStartIntegrationSessionWith(t, a, cfg)
	t.Cleanup(func() { _ = a.StopSession(context.Background(), session) })

	events, result := collectAllEvents(t, a, session, "Reply with exactly: hello")
	if result.ExitReason != domain.EventTurnFailed {
		t.Fatalf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnFailed)
	}

	var sawTurnFailed, sawModelNotFound bool
	for _, event := range events {
		if event.Type != domain.EventTurnFailed {
			continue
		}
		sawTurnFailed = true
		if strings.Contains(event.Message, "Model unavailable: nonexistent/nonexistent") {
			sawModelNotFound = true
		}
	}
	if !sawTurnFailed {
		t.Fatalf("expected turn_failed event for invalid model, events=%+v", events)
	}
	if !sawModelNotFound {
		t.Errorf("expected at least one turn_failed event with invalid-model detail, events=%+v", events)
	}
}

func TestIntegration_PermissionDeepMerge(t *testing.T) {
	skipIfNotEnabled(t)

	workspace := t.TempDir()
	const operatorConfig = `{"permission":{"webfetch":"allow","sortie_probe_operator_key":"ask"}}`
	if err := os.WriteFile(filepath.Join(workspace, "opencode.json"), []byte(operatorConfig), 0o600); err != nil {
		t.Fatalf("WriteFile(opencode.json): %v", err)
	}

	toolServerPath := agenttest.FakeRuntime(t, t.TempDir(), "tool-server", agenttest.OutputScenario, agenttest.Output{})
	mcpConfigPath := filepath.Join(workspace, ".sortie", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(mcpConfigPath), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	mcpDoc := fmt.Sprintf(`{"mcpServers":{"sortie-tools":{"type":"stdio","command":%s}}}`, mustJSONString(t, toolServerPath))
	if err := os.WriteFile(mcpConfigPath, []byte(mcpDoc), 0o600); err != nil {
		t.Fatalf("WriteFile(mcp.json): %v", err)
	}

	cfg := integrationConfig()
	cfg["allowed_tools"] = []any{"read", "glob"}

	a := mustNewAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := a.StartSession(ctx, domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig: domain.AgentConfig{
			Command:       integrationCommand(),
			ReadTimeoutMS: coldStartReadTimeoutMS,
		},
		MCPConfigPath: mcpConfigPath,
		Settings:      cfg,
	})
	if err != nil {
		t.Fatalf("StartSession(): %v", err)
	}
	t.Cleanup(func() { _ = a.StopSession(context.Background(), session) })

	permCtx, permCancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer permCancel()
	effective, raw, err := opencode.EffectivePermissionsForTest(permCtx, session)
	if err != nil {
		t.Fatalf("EffectivePermissionsForTest(): %v (raw output: %s)", err, raw)
	}

	if effective["sortie_probe_operator_key"] != "ask" {
		t.Errorf("effective[%q] = %q, want %q (the operator's own entry must survive the merge)",
			"sortie_probe_operator_key", effective["sortie_probe_operator_key"], "ask")
	}
	if effective["webfetch"] != "deny" {
		t.Errorf("effective[%q] = %q, want %q (the adapter's entry must win the conflict)", "webfetch", effective["webfetch"], "deny")
	}
	if effective["read"] != "allow" {
		t.Errorf("effective[%q] = %q, want %q", "read", effective["read"], "allow")
	}
	if effective["glob"] != "allow" {
		t.Errorf("effective[%q] = %q, want %q", "glob", effective["glob"], "allow")
	}
	if effective["shell"] != "deny" {
		t.Errorf("effective[%q] = %q, want %q", "shell", effective["shell"], "deny")
	}
}

func mustJSONString(t *testing.T, s string) string {
	t.Helper()
	encoded, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("json.Marshal(%q): %v", s, err)
	}
	return string(encoded)
}

func TestIntegration_CredentialVerification(t *testing.T) {
	skipIfNotEnabled(t)

	defaults, err := config.NewServiceConfig(map[string]any{})
	if err != nil {
		t.Fatalf("config.NewServiceConfig(defaults) error = %v", err)
	}
	adapter := mustNewAdapter(t)
	params := domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: integrationCommand(), ReadTimeoutMS: defaults.Agent.ReadTimeoutMS},
		Settings:      integrationConfig(),
	}
	credentialtest.VerifyLiveUsage(t, "opencode", adapter, params, integrationConfig())

	t.Run("refused credential ends credential_unverified", func(t *testing.T) {
		credentialtest.SetRefusedCredential(t, "SORTIE_OPENCODE_CREDENTIAL_ENV")

		_, err := credentialtest.VerifyLive(adapter, params)
		credentialtest.RequireRefused(t, err)
	})
}

func TestIntegration_EarlyExit(t *testing.T) {
	skipIfNotEnabled(t)

	adapter := mustNewAdapter(t)
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
