// Integration tests for the Codex CLI agent adapter.
//
// Required environment variables:
//
//	SORTIE_CODEX_TEST=1     enable this suite
//	CODEX_API_KEY           Codex API key for authentication; the live cases
//	                        need it, the scripted-model case reads no
//	                        credential and needs only the gate
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
	"sync"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest/credentialtest"
	"github.com/sortie-ai/sortie/internal/agent/agenttest/fakemodel"
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

func integrationAgentConfig() domain.AgentConfig {
	return domain.AgentConfig{
		Command:       integrationCommand(),
		TurnTimeoutMS: 90000,
		ReadTimeoutMS: 30000,
	}
}

// The Codex app-server requires a git repository by default.
func gitInitWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.CommandContext(context.Background(), "git", "-C", dir, "init")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("gitInitWorkspace: git init: %v\n%s", err, out)
	}
	return dir
}

func assertContainsEventType(t *testing.T, events []domain.AgentEvent, eventType domain.AgentEventType) {
	t.Helper()
	for _, e := range events {
		if e.Type == eventType {
			return
		}
	}
	types := make([]domain.AgentEventType, len(events))
	for i, e := range events {
		types[i] = e.Type
	}
	t.Errorf("expected event type %q not found; got types: %v", eventType, types)
}

func assertNoEventType(t *testing.T, events []domain.AgentEvent, eventType domain.AgentEventType) {
	t.Helper()
	for _, e := range events {
		if e.Type == eventType {
			t.Errorf("unexpected event type %q found with message: %q", eventType, e.Message)
			return
		}
	}
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

func makeEventCollector(t *testing.T) (onEvent func(domain.AgentEvent), collected func() []domain.AgentEvent) {
	t.Helper()
	var mu sync.Mutex
	var events []domain.AgentEvent
	onEvent = func(e domain.AgentEvent) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	}
	collected = func() []domain.AgentEvent {
		mu.Lock()
		defer mu.Unlock()
		out := make([]domain.AgentEvent, len(events))
		copy(out, events)
		return out
	}
	return onEvent, collected
}

func mustNewAdapter(t *testing.T) *CodexAdapter {
	t.Helper()
	a, err := NewCodexAdapter()
	if err != nil {
		t.Fatalf("NewCodexAdapter: %v", err)
	}
	return a.(*CodexAdapter)
}

func mustStartSession(t *testing.T, ctx context.Context, adapter *CodexAdapter, workspace string) domain.Session {
	t.Helper()
	session, err := adapter.StartSession(ctx, domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig:   integrationAgentConfig(),
		Settings:      integrationConfig(),
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	t.Cleanup(func() { _ = adapter.StopSession(context.Background(), session) })
	return session
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

// The redirect rides in -c overrides because the runtime ignores
// OPENAI_BASE_URL and a project-local config file; zero retry counts make a
// refused request end the turn instead of being replayed.
func scriptedProviderCommand(url string) string {
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
	args := []string{integrationCommand()}
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
		Launch: func(t *testing.T, env fakemodel.Environment) fakemodel.Launch {
			return fakemodel.Launch{
				Config: domain.AgentConfig{
					Command:       scriptedProviderCommand(env.URL),
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

func TestIntegration_RunTurn_StopDuringTurn(t *testing.T) {
	skipUnlessCodexIntegration(t)

	adapter := mustNewAdapter(t)
	workspace := gitInitWorkspace(t)

	outerCtx, outerCancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer outerCancel()

	session, err := adapter.StartSession(outerCtx, domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig:   integrationAgentConfig(),
		Settings:      integrationConfig(),
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	type turnOutcome struct {
		result domain.TurnResult
		err    error
	}
	outcomeCh := make(chan turnOutcome, 1)

	go func() {
		r, e := adapter.RunTurn(outerCtx, session, domain.RunTurnParams{
			Prompt:  "Execute the shell command: sleep 30",
			OnEvent: func(_ domain.AgentEvent) {},
		})
		outcomeCh <- turnOutcome{result: r, err: e}
	}()

	// 400ms is well above the turn/start round-trip latency.
	time.Sleep(400 * time.Millisecond)

	if stopErr := adapter.StopSession(context.Background(), session); stopErr != nil {
		t.Errorf("StopSession during turn: %v", stopErr)
	}

	select {
	case outcome := <-outcomeCh:
		if outcome.err == nil {
			t.Error("RunTurn returned nil after StopSession was called mid-turn; expected an error")
		}
		t.Logf("RunTurn returned error after StopSession: %v", outcome.err)
	case <-time.After(10 * time.Second):
		t.Error("RunTurn did not unblock within 10s after StopSession; possible deadlock in reader goroutine")
	}
}

func TestIntegration_MultiTurn(t *testing.T) {
	skipUnlessCodexIntegration(t)

	adapter := mustNewAdapter(t)
	workspace := gitInitWorkspace(t)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	session := mustStartSession(t, ctx, adapter, workspace)
	state := session.Internal.(*sessionState)
	state.mu.Lock()
	groupAfterStart := state.group
	state.mu.Unlock()
	if groupAfterStart == nil {
		t.Fatal("state.group = nil after StartSession, want the subprocess's launch record")
	}

	onEvent1, collected1 := makeEventCollector(t)
	result1, err := adapter.RunTurn(ctx, session, domain.RunTurnParams{
		Prompt:  "Say exactly one word: hello",
		OnEvent: onEvent1,
	})
	if err != nil {
		t.Fatalf("RunTurn (turn 1): %v", err)
	}
	if result1.ExitReason != domain.EventTurnCompleted {
		t.Fatalf("RunTurn (turn 1): ExitReason = %q, want %q", result1.ExitReason, domain.EventTurnCompleted)
	}
	if result1.SessionID != session.ID {
		t.Errorf("turn 1: TurnResult.SessionID = %q, want %q", result1.SessionID, session.ID)
	}
	assertContainsEventType(t, collected1(), domain.EventSessionStarted)

	if state.turnCount != 1 {
		t.Errorf("state.turnCount after turn 1 = %d, want 1", state.turnCount)
	}

	state.mu.Lock()
	groupAfterTurn1 := state.group
	state.mu.Unlock()
	if groupAfterTurn1 != groupAfterStart {
		t.Errorf("launch record changed after turn 1: before=%p after=%p (persistent subprocess must survive turns)", groupAfterStart, groupAfterTurn1)
	}

	onEvent2, collected2 := makeEventCollector(t)
	result2, err := adapter.RunTurn(ctx, session, domain.RunTurnParams{
		Prompt:  "Say exactly one word: world",
		OnEvent: onEvent2,
	})
	if err != nil {
		t.Fatalf("RunTurn (turn 2): %v", err)
	}
	if result2.ExitReason != domain.EventTurnCompleted {
		t.Errorf("RunTurn (turn 2): ExitReason = %q, want %q", result2.ExitReason, domain.EventTurnCompleted)
	}
	if result2.SessionID != session.ID {
		t.Errorf("turn 2: TurnResult.SessionID = %q, want %q (must remain same thread)", result2.SessionID, session.ID)
	}

	events2 := collected2()
	assertNoEventType(t, events2, domain.EventSessionStarted)
	assertContainsEventType(t, events2, domain.EventTurnCompleted)

	if state.turnCount != 2 {
		t.Errorf("state.turnCount after turn 2 = %d, want 2", state.turnCount)
	}

	state.mu.Lock()
	groupAfterTurn2 := state.group
	state.mu.Unlock()
	if groupAfterTurn2 != groupAfterStart {
		t.Errorf("launch record changed after turn 2: original=%p current=%p (persistent subprocess must survive all turns)", groupAfterStart, groupAfterTurn2)
	}

	if result1.SessionID != result2.SessionID {
		t.Errorf("SessionID changed between turns: turn1=%q turn2=%q (same thread must be reused)", result1.SessionID, result2.SessionID)
	}
}

func TestIntegration_ResumeSession(t *testing.T) {
	skipUnlessCodexIntegration(t)

	workspace := gitInitWorkspace(t)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	adapter1 := mustNewAdapter(t)
	session1, err := adapter1.StartSession(ctx, domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig:   integrationAgentConfig(),
		Settings:      integrationConfig(),
	})
	if err != nil {
		t.Fatalf("StartSession (original): %v", err)
	}

	result1, err := adapter1.RunTurn(ctx, session1, domain.RunTurnParams{
		Prompt:  "Say exactly one word: hello",
		OnEvent: func(_ domain.AgentEvent) {},
	})
	if err != nil {
		t.Fatalf("RunTurn (original): %v", err)
	}
	if result1.ExitReason != domain.EventTurnCompleted {
		t.Fatalf("original turn: ExitReason = %q, want %q", result1.ExitReason, domain.EventTurnCompleted)
	}
	originalThreadID := result1.SessionID
	if originalThreadID == "" {
		t.Fatal("original turn: TurnResult.SessionID is empty")
	}

	// Stop the original session so the thread is persisted.
	if err := adapter1.StopSession(context.Background(), session1); err != nil {
		t.Fatalf("StopSession (original): %v", err)
	}

	resumedSettings := integrationConfig()
	resumedSettings["model"] = "gpt-5.4"
	resumedSettings["effort"] = "low"

	session2, err := adapter1.StartSession(ctx, domain.StartSessionParams{
		WorkspacePath:   workspace,
		AgentConfig:     integrationAgentConfig(),
		ResumeSessionID: originalThreadID,
		Settings:        resumedSettings,
	})
	if err != nil {
		t.Fatalf("StartSession (resume): %v", err)
	}
	t.Cleanup(func() { _ = adapter1.StopSession(context.Background(), session2) })

	if session2.ID != originalThreadID {
		t.Errorf("resumed session.ID = %q, want %q (provided ResumeSessionID)", session2.ID, originalThreadID)
	}

	onEvent, collected := makeEventCollector(t)
	result2, err := adapter1.RunTurn(ctx, session2, domain.RunTurnParams{
		Prompt:  "Say exactly one word: world",
		OnEvent: onEvent,
	})
	if err != nil {
		t.Fatalf("RunTurn (resumed): %v", err)
	}
	if result2.ExitReason != domain.EventTurnCompleted {
		t.Errorf("resumed turn: ExitReason = %q, want %q", result2.ExitReason, domain.EventTurnCompleted)
	}
	if result2.SessionID != originalThreadID {
		t.Errorf("resumed turn: TurnResult.SessionID = %q, want %q", result2.SessionID, originalThreadID)
	}

	resumedEvents := collected()
	assertContainsEventType(t, resumedEvents, domain.EventTokenUsage)
	for _, e := range resumedEvents {
		if e.Type == domain.EventTokenUsage && e.Model == "" {
			t.Error("resumed turn: EventTokenUsage.Model is empty; expected the effective model reported by thread/resume")
		}
	}
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
