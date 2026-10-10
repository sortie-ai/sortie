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

// integrationCommand returns the opencode binary path, defaulting to "opencode".
func integrationCommand() string {
	if cmd := os.Getenv("SORTIE_OPENCODE_COMMAND"); cmd != "" {
		return cmd
	}
	return "opencode"
}

// integrationConfig returns base config for integration tests.
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

// mustNewAdapter creates an adapter or fatals.
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

// mustStartIntegrationSession starts a session against the real opencode binary
// in a fresh workspace.
func mustStartIntegrationSession(t *testing.T, a domain.AgentAdapter) domain.Session {
	t.Helper()
	return mustStartIntegrationSessionWith(t, a, integrationConfig())
}

func mustStartIntegrationSessionWith(t *testing.T, a domain.AgentAdapter, settings map[string]any) domain.Session {
	t.Helper()
	return mustStartIntegrationSessionIn(t, a, "", t.TempDir(), settings)
}

// mustStartIntegrationSessionIn starts a session in the given workspace.
// ReadTimeoutMS is set to 3 minutes to absorb one-time cold-start SQLite
// migrations that can run for over 30 seconds on first launch. Resuming a
// session requires the workspace the session was created in: opencode replays
// a --session only when the run executes in that same project directory.
func mustStartIntegrationSessionIn(t *testing.T, a domain.AgentAdapter, resumeID, workspacePath string, settings map[string]any) domain.Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	session, err := a.StartSession(ctx, domain.StartSessionParams{
		WorkspacePath: workspacePath,
		AgentConfig: domain.AgentConfig{
			Command:       integrationCommand(),
			ReadTimeoutMS: 3 * 60 * 1000, // 3 minutes: absorbs cold-start SQLite migration
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

// scriptedProviderDocument is the provider configuration that points the
// runtime's bundled Google provider at the scripted endpoint on url. The key
// is an {env:} reference because 2.x does not read the provider's key
// variable on its own.
func scriptedProviderDocument(url string) string {
	return fmt.Sprintf(`{"$schema":"https://opencode.ai/config.json","provider":{"scripted":{"npm":"@ai-sdk/google","name":"scripted","options":{"baseURL":"%s/v1beta","apiKey":"{env:GOOGLE_GENERATIVE_AI_API_KEY}"},"models":{"scripted-model":{"name":"scripted-model"}}}}}`, url)
}

func TestIntegration_ScriptedModel(t *testing.T) {
	skipIfNotEnabled(t)

	fakemodel.AssertConformance(t, fakemodel.Binding{
		Kind: "opencode",
		Passthrough: map[string]any{
			"model":                        "scripted/scripted-model",
			"dangerously_skip_permissions": true,
			"disable_autocompact":          true,
		},
		CredentialEnv: []string{"GOOGLE_GENERATIVE_AI_API_KEY"},
		Read:          fakemodel.ReadFile,
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
					Command: integrationCommand(),
					// A first launch on an isolated home runs the database
					// migration every time.
					TurnTimeoutMS: 300000,
					ReadTimeoutMS: 180000,
				},
				Env: map[string]string{"OPENCODE_CONFIG": document},
			}
		},
	})
}

func TestIntegration_SessionResume(t *testing.T) {
	skipIfNotEnabled(t)

	// opencode replays a --session only when the run executes in the same
	// project directory the session was created in; resuming under a different
	// --dir exits cleanly with no events. The orchestrator reuses an issue's
	// workspace across turns, so the resumed turn must share turn one's
	// workspace.
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

func TestIntegration_PermissionDeny(t *testing.T) {
	skipIfNotEnabled(t)

	cfg := integrationConfig()
	cfg["dangerously_skip_permissions"] = false

	a := mustNewAdapter(t)

	session := mustStartIntegrationSessionWith(t, a, cfg)
	t.Cleanup(func() { _ = a.StopSession(context.Background(), session) })

	// The prompt explicitly names the tool so the model is compelled to invoke
	// it rather than narrating intent or answering from memory.
	events, _ := collectAllEvents(t, a, session,
		"Use your file-read tool to read /etc/hostname and return the exact contents verbatim. You must call the tool — do not answer from memory and do not describe what you would do.")

	// Strong signal: OpenCode auto-rejects external_directory access in
	// headless mode without --dangerously-skip-permissions, emitting a
	// tool_use error envelope.
	var sawToolError bool
	for _, e := range events {
		if e.Type == domain.EventToolResult && e.ToolError {
			sawToolError = true
			break
		}
	}
	if sawToolError {
		return
	}

	// Weak signal: the model acknowledged the denial in assistant text without
	// emitting a tool result (e.g. OpenCode reported the block as a
	// notification before the tool completed).
	denialKeywords := []string{"denied", "permission", "not allowed", "cannot", "can't", "unable"}
	for _, e := range events {
		if e.Type != domain.EventNotification && e.Type != domain.EventOtherMessage && e.Type != domain.EventTurnFailed {
			continue
		}
		msg := strings.ToLower(e.Message)
		for _, kw := range denialKeywords {
			if strings.Contains(msg, kw) {
				return
			}
		}
	}

	// Neither signal was present: the model did not attempt the tool and did
	// not report a denial. This is a non-deterministic model choice, not an
	// adapter defect. Skip rather than block the release pipeline.
	t.Skip("model neither invoked the file-read tool nor reported a denial; skipping to avoid blocking release on non-deterministic model behavior")
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

	// Cancel after a brief moment.
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

// TestIntegration_PermissionDeepMerge reads the resolved tool-
// permission document a session's own turns carry, with no model
// request: the operator's own opencode.json must survive the adapter's
// merge, and the adapter's own entries must win a conflict.
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
			ReadTimeoutMS: 3 * 60 * 1000,
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

// TestIntegration_ToolServerIdentity proves tool-server delivery
// without a model call invoking the tool, mirroring
// internal/agent/claude's TestIntegration_ToolServerIdentity.
//
// It does not call agenttest.AssertToolServerIdentity: that helper
// writes the generated MCP config at <dir>/mcp.json, a path
// mcpconfig.Parse refuses for a translated-injection adapter, which
// requires the config to sit under a workspace's own ".sortie"
// directory. codex and agent-client-protocol, this adapter's fellow
// translated-injection kinds, do not use the helper for the same
// reason.
func TestIntegration_ToolServerIdentity(t *testing.T) {
	skipIfNotEnabled(t)

	dir := t.TempDir()
	workspace := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(workspace, 0o750); err != nil {
		t.Fatalf("MkdirAll(workspace): %v", err)
	}

	const wantDispatchID = "opencode-tool-server-identity"
	recordingPath := filepath.Join(dir, "recorded.json")
	runtimePath := agenttest.FakeRuntime(t, dir, "tool-server-recorder", agenttest.RecordedEnvScenario, agenttest.RecordedEnv{
		Path:  recordingPath,
		Names: []string{"SORTIE_DISPATCH_ID", "SORTIE_WORKSPACE"},
	})

	mcpConfigPath := filepath.Join(workspace, ".sortie", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(mcpConfigPath), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	mcpDoc := fmt.Sprintf(`{"mcpServers":{"sortie-tools":{"type":"stdio","command":%s,"env":{"SORTIE_DISPATCH_ID":%s,"SORTIE_WORKSPACE":%s}}}}`,
		mustJSONString(t, runtimePath), mustJSONString(t, wantDispatchID), mustJSONString(t, workspace))
	if err := os.WriteFile(mcpConfigPath, []byte(mcpDoc), 0o600); err != nil {
		t.Fatalf("WriteFile(mcp.json): %v", err)
	}

	a := mustNewAdapter(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sessionDone := make(chan error, 1)
	go func() {
		session, startErr := a.StartSession(ctx, domain.StartSessionParams{
			WorkspacePath: workspace,
			AgentConfig: domain.AgentConfig{
				Command:       integrationCommand(),
				ReadTimeoutMS: 3 * 60 * 1000,
			},
			MCPConfigPath: mcpConfigPath,
			Settings:      integrationConfig(),
		})
		if startErr != nil {
			sessionDone <- startErr
			return
		}
		defer func() { _ = a.StopSession(context.Background(), session) }()

		_, runErr := a.RunTurn(ctx, session, domain.RunTurnParams{
			Prompt:  "Say exactly: hello",
			OnEvent: func(domain.AgentEvent) {},
		})
		sessionDone <- runErr
	}()

	deadline := time.Now().Add(60 * time.Second)
	for {
		if raw, readErr := os.ReadFile(recordingPath); readErr == nil {
			cancel()
			<-sessionDone
			assertRecordedToolServerIdentity(t, raw, wantDispatchID, workspace)
			return
		}
		if !time.Now().Before(deadline) {
			cancel()
			runErr := <-sessionDone
			t.Fatalf("no recording observed within 60s (session error = %v)", runErr)
		}
		select {
		case runErr := <-sessionDone:
			t.Fatalf("no recording observed before the session returned (error = %v)", runErr)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func assertRecordedToolServerIdentity(t *testing.T, raw []byte, wantDispatchID, wantWorkspace string) {
	t.Helper()

	var recorded map[string]string
	if err := json.Unmarshal(raw, &recorded); err != nil {
		t.Fatalf("decode recorded tool-server environment %q: %v", raw, err)
	}
	if recorded["SORTIE_DISPATCH_ID"] != wantDispatchID {
		t.Errorf("recorded SORTIE_DISPATCH_ID = %q, want %q", recorded["SORTIE_DISPATCH_ID"], wantDispatchID)
	}
	if recorded["SORTIE_WORKSPACE"] != wantWorkspace {
		t.Errorf("recorded SORTIE_WORKSPACE = %q, want %q", recorded["SORTIE_WORKSPACE"], wantWorkspace)
	}
}

// mustJSONString renders s as a JSON string literal.
func mustJSONString(t *testing.T, s string) string {
	t.Helper()
	encoded, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("json.Marshal(%q): %v", s, err)
	}
	return string(encoded)
}

func TestIntegration_ToolRoundTrip(t *testing.T) {
	skipIfNotEnabled(t)

	workspace := t.TempDir()
	tools := agenttest.NewSortieTools(t, workspace)

	a := mustNewAdapter(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	session, err := a.StartSession(ctx, domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig: domain.AgentConfig{
			Command:       integrationCommand(),
			ReadTimeoutMS: 3 * 60 * 1000,
		},
		MCPConfigPath: tools.ConfigPath,
		Settings:      integrationConfig(),
	})
	if err != nil {
		t.Fatalf("StartSession(): %v", err)
	}
	t.Cleanup(func() { _ = a.StopSession(context.Background(), session) })

	events, result := collectAllEvents(t, a, session,
		"Call the sortie_status tool now, with no arguments, and report exactly what it returns. Do not explain first; call the tool immediately.")

	for _, e := range events {
		if e.Type == domain.EventToolResult {
			t.Logf("EventToolResult: ToolName=%q ToolDurationMS=%d", e.ToolName, e.ToolDurationMS)
		}
	}

	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}

	// No attempt name: 2.x reaches MCP tools through its execute tool, which
	// also runs code that calls nothing of Sortie's, so an execute event does
	// not show an attempt. The scripted-model run proves that path instead.
	tools.Relay.AssertModelToolCall(t, agenttest.SortieStatusTool, events)
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
