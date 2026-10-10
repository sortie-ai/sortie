package opencode_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/agenttest/credentialtest"
	"github.com/sortie-ai/sortie/internal/agent/agenttest/fakemodel"
	"github.com/sortie-ai/sortie/internal/agent/opencode"
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

func mustStartIntegrationSession(t *testing.T, a domain.AgentAdapter) domain.Session {
	t.Helper()
	return mustStartIntegrationSessionWith(t, a, integrationConfig())
}

func mustStartIntegrationSessionWith(t *testing.T, a domain.AgentAdapter, settings map[string]any) domain.Session {
	t.Helper()
	return mustStartIntegrationSessionIn(t, a, "", t.TempDir(), settings)
}

// coldStartReadTimeoutMS absorbs the SQLite migration a first launch runs,
// which can take over 30 seconds.
const coldStartReadTimeoutMS = 3 * 60 * 1000

func mustStartIntegrationSessionIn(t *testing.T, a domain.AgentAdapter, resumeID, workspacePath string, settings map[string]any) domain.Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	session, err := a.StartSession(ctx, domain.StartSessionParams{
		WorkspacePath: workspacePath,
		AgentConfig: domain.AgentConfig{
			Command:       integrationCommand(),
			ReadTimeoutMS: coldStartReadTimeoutMS,
		},
		ResumeSessionID: resumeID,
		Settings:        settings,
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

// The key is an {env:} reference because opencode 2.x does not read the
// provider's key variable on its own.
func scriptedProviderDocument(url string) string {
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

	fakemodel.AssertConformance(t, fakemodel.Binding{
		Kind:          "opencode",
		Passthrough:   scriptedSettings(true),
		CredentialEnv: []string{"GOOGLE_GENERATIVE_AI_API_KEY"},
		Read:          fakemodel.ReadFile,
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
	})
}

func TestIntegration_SessionResume(t *testing.T) {
	skipIfNotEnabled(t)

	// opencode replays a --session only from the project directory the
	// session was created in; under another --dir the resume exits cleanly
	// with no events.
	workspace := t.TempDir()

	a := mustNewAdapter(t)
	session := mustStartIntegrationSessionIn(t, a, "", workspace, integrationConfig())
	t.Cleanup(func() { _ = a.StopSession(context.Background(), session) })

	_, result1 := collectAllEvents(t, a, session, "Say: turn one")
	if result1.ExitReason != domain.EventTurnCompleted {
		t.Fatalf("turn 1 ExitReason = %q, want completed", result1.ExitReason)
	}
	sessionID := result1.SessionID
	if sessionID == "" {
		t.Fatal("turn 1 SessionID is empty")
	}

	resumedSettings := integrationConfig()
	resumedSettings["disable_autocompact"] = false
	session2 := mustStartIntegrationSessionIn(t, a, sessionID, workspace, resumedSettings)
	t.Cleanup(func() { _ = a.StopSession(context.Background(), session2) })

	_, result2 := collectAllEvents(t, a, session2, "What did I say in the previous message?")
	if result2.ExitReason != domain.EventTurnCompleted {
		t.Errorf("resumed turn ExitReason = %q, want completed", result2.ExitReason)
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

func TestIntegration_TurnCancellation(t *testing.T) {
	skipIfNotEnabled(t)

	a := mustNewAdapter(t)
	session := mustStartIntegrationSession(t, a)
	t.Cleanup(func() { _ = a.StopSession(context.Background(), session) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	turnCtx, turnCancel := context.WithCancel(ctx)
	resultCh := make(chan domain.TurnResult, 1)
	go func() {
		result, _ := a.RunTurn(turnCtx, session, domain.RunTurnParams{
			Prompt:  "Count to 1000 slowly, outputting each number on its own line",
			OnEvent: func(_ domain.AgentEvent) {},
		})
		resultCh <- result
	}()

	time.Sleep(500 * time.Millisecond)
	turnCancel()

	select {
	case result := <-resultCh:
		if result.ExitReason != domain.EventTurnCancelled {
			t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCancelled)
		}
	case <-ctx.Done():
		t.Fatal("RunTurn did not return after context cancel")
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
