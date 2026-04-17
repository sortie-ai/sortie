// Integration tests for the Copilot CLI adapter.
//
// Required environment variables:
//
//	SORTIE_COPILOT_TEST=1          enable this suite
//	SORTIE_COPILOT_COMMAND         path to copilot binary (default: "copilot")
//
// Authentication: at least one of COPILOT_GITHUB_TOKEN, GH_TOKEN, or
// GITHUB_TOKEN must be set, or gh CLI must be authenticated.
//
// Run:
//
//	SORTIE_COPILOT_TEST=1 make test PKG=./internal/agent/copilot/... RUN=Integration
package copilot

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

// skipUnlessCopilotIntegration skips the current test when SORTIE_COPILOT_TEST
// is not set to "1", so disabled integration tests are reported as skipped
// rather than silently passing.
func skipUnlessCopilotIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("SORTIE_COPILOT_TEST") != "1" {
		t.Skip("skipping Copilot CLI integration test: set SORTIE_COPILOT_TEST=1 to enable")
	}
}

// integrationConfig builds the adapter config map for integration tests.
func integrationConfig() map[string]any {
	model := os.Getenv("SORTIE_COPILOT_MODEL")
	if model == "" {
		model = "claude-haiku-4-5"
	}
	return map[string]any{
		"max_autopilot_continues": float64(5),
		"model":                   model,
	}
}

// integrationCommand returns the Copilot CLI binary path from the
// SORTIE_COPILOT_COMMAND environment variable, defaulting to "copilot".
func integrationCommand() string {
	if cmd := os.Getenv("SORTIE_COPILOT_COMMAND"); cmd != "" {
		return cmd
	}
	return "copilot"
}

// assertContainsEventType asserts that at least one event in the slice
// has the given type.
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

// assertNoEventType asserts that no event in the slice has the given type.
func assertNoEventType(t *testing.T, events []domain.AgentEvent, eventType domain.AgentEventType) {
	t.Helper()
	for _, e := range events {
		if e.Type == eventType {
			t.Errorf("unexpected event type %q found with message: %s", eventType, e.Message)
			return
		}
	}
}

// collectEvents collects events from a turn using a mutex-safe callback.
// Returns the collected slice after the turn completes.
func collectEvents(t *testing.T) (onEvent func(domain.AgentEvent), collected func() []domain.AgentEvent) {
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

// --- Integration test functions ---

func TestIntegration_StartSession(t *testing.T) {
	skipUnlessCopilotIntegration(t)

	adapter, err := NewCopilotAdapter(integrationConfig())
	if err != nil {
		t.Fatalf("NewCopilotAdapter: %v", err)
	}

	workspace := t.TempDir()
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig:   domain.AgentConfig{Command: integrationCommand()},
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	t.Cleanup(func() { _ = adapter.StopSession(context.Background(), session) })

	// Copilot CLI does not pre-assign a session ID: it is set only after
	// the first turn's result event. Session.Internal must be non-nil.
	if session.Internal == nil {
		t.Error("Session.Internal is nil")
	}
}

func TestIntegration_StopSession(t *testing.T) {
	skipUnlessCopilotIntegration(t)

	adapter, err := NewCopilotAdapter(integrationConfig())
	if err != nil {
		t.Fatalf("NewCopilotAdapter: %v", err)
	}

	workspace := t.TempDir()
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig:   domain.AgentConfig{Command: integrationCommand()},
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	// StopSession before RunTurn starts must be a no-op (no process running).
	if err := adapter.StopSession(context.Background(), session); err != nil {
		t.Fatalf("StopSession (idle): %v", err)
	}
}

func TestIntegration_StartSession_InvalidCommand(t *testing.T) {
	skipUnlessCopilotIntegration(t)

	adapter, err := NewCopilotAdapter(integrationConfig())
	if err != nil {
		t.Fatalf("NewCopilotAdapter: %v", err)
	}

	_, err = adapter.StartSession(context.Background(), domain.StartSessionParams{
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

// TestIntegration_RunTurn executes a single-turn session, verifying that the
// adapter delivers the mandatory event sequence and populates TurnResult
// correctly.
func TestIntegration_RunTurn(t *testing.T) {
	skipUnlessCopilotIntegration(t)

	adapter, err := NewCopilotAdapter(integrationConfig())
	if err != nil {
		t.Fatalf("NewCopilotAdapter: %v", err)
	}

	workspace := t.TempDir()
	if err := os.WriteFile(workspace+"/hello.txt", []byte("Hello"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig:   domain.AgentConfig{Command: integrationCommand()},
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	t.Cleanup(func() { _ = adapter.StopSession(context.Background(), session) })

	onEvent, collected := collectEvents(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	result, err := adapter.RunTurn(ctx, session, domain.RunTurnParams{
		Prompt:  "Read the file hello.txt. Output EXACTLY the file content and absolutely nothing else. No preamble, no explanation.",
		OnEvent: onEvent,
	})
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}

	events := collected()

	if result.SessionID == "" {
		t.Error("TurnResult.SessionID is empty; expected session ID from result event")
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("TurnResult.ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}
	if len(events) == 0 {
		t.Fatal("no events received via OnEvent")
	}

	assertContainsEventType(t, events, domain.EventSessionStarted)
	assertContainsEventType(t, events, domain.EventTurnCompleted)
	assertNoEventType(t, events, domain.EventTurnFailed)
	assertNoEventType(t, events, domain.EventStartupFailed)

	// Copilot CLI does not expose per-message input tokens; verify
	// cumulative output tokens are positive.
	if result.Usage.OutputTokens <= 0 {
		t.Errorf("TurnResult.Usage.OutputTokens = %d, want > 0", result.Usage.OutputTokens)
	}
	if result.Usage.TotalTokens != result.Usage.OutputTokens {
		t.Errorf("TurnResult.Usage.TotalTokens = %d, want == OutputTokens (%d)",
			result.Usage.TotalTokens, result.Usage.OutputTokens)
	}

	// At least one EventTokenUsage must have been delivered.
	assertContainsEventType(t, events, domain.EventTokenUsage)

	// Verify at least one EventToolResult with a non-empty ToolName.
	// The prompt causes Copilot CLI to use the view or read tool, producing
	// tool.execution_start + tool.execution_complete events. Asserting
	// ToolName != "" confirms tool start/complete correlation succeeded.
	var foundToolResult bool
	for _, e := range events {
		if e.Type == domain.EventToolResult && e.ToolName != "" {
			foundToolResult = true
			if e.ToolDurationMS < 0 {
				t.Errorf("EventToolResult.ToolDurationMS = %d, want >= 0", e.ToolDurationMS)
			}
			break
		}
	}
	if !foundToolResult {
		var toolNames []string
		for _, e := range events {
			if e.Type == domain.EventToolResult {
				toolNames = append(toolNames, e.ToolName)
			}
		}
		t.Errorf("expected EventToolResult with non-empty ToolName; got tool results: %v", toolNames)
	}
}

// TestIntegration_RunTurn_ContextCancellation verifies that cancelling the
// context mid-turn causes RunTurn to return ErrTurnCancelled promptly and
// cleans up the subprocess.
func TestIntegration_RunTurn_ContextCancellation(t *testing.T) {
	skipUnlessCopilotIntegration(t)

	adapter, err := NewCopilotAdapter(integrationConfig())
	if err != nil {
		t.Fatalf("NewCopilotAdapter: %v", err)
	}

	workspace := t.TempDir()
	if err := os.WriteFile(workspace+"/dummy.txt", []byte("test"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig:   domain.AgentConfig{Command: integrationCommand()},
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	t.Cleanup(func() { _ = adapter.StopSession(context.Background(), session) })

	onEvent, collected := collectEvents(t)

	// Use a 2-second timeout: long enough for subprocess startup (~100ms)
	// but well below the minimum API round-trip (~3-5s), ensuring the
	// context always expires before the turn completes.
	shortCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result, err := adapter.RunTurn(shortCtx, session, domain.RunTurnParams{
		Prompt:  "Count from 1 to 1000, printing each number on a new line. Do not stop early.",
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

	assertContainsEventType(t, collected(), domain.EventSessionStarted)
	assertContainsEventType(t, collected(), domain.EventTurnCancelled)
}

// TestIntegration_ResumeSession verifies that a second turn on the same
// session uses --resume and the CLI returns the same session ID.
func TestIntegration_ResumeSession(t *testing.T) {
	skipUnlessCopilotIntegration(t)

	adapter, err := NewCopilotAdapter(integrationConfig())
	if err != nil {
		t.Fatalf("NewCopilotAdapter: %v", err)
	}

	workspace := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	session, err := adapter.StartSession(ctx, domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig:   domain.AgentConfig{Command: integrationCommand()},
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	t.Cleanup(func() { _ = adapter.StopSession(context.Background(), session) })

	// Turn 1: establish session ID.
	result1, err := adapter.RunTurn(ctx, session, domain.RunTurnParams{
		Prompt:  "Say exactly one word: hello",
		OnEvent: func(_ domain.AgentEvent) {},
	})
	if err != nil {
		t.Fatalf("RunTurn (turn 1): %v", err)
	}
	if result1.SessionID == "" {
		t.Fatal("turn 1: TurnResult.SessionID is empty")
	}
	if result1.ExitReason != domain.EventTurnCompleted {
		t.Fatalf("turn 1: ExitReason = %q, want %q", result1.ExitReason, domain.EventTurnCompleted)
	}

	// RunTurn updates state.copilotSessionID; verify internal state was
	// updated so the next turn will use --resume.
	state := session.Internal.(*sessionState)
	if state.copilotSessionID != result1.SessionID {
		t.Errorf("state.copilotSessionID = %q, want %q (should match turn 1 result)",
			state.copilotSessionID, result1.SessionID)
	}
	if state.fallbackToContinue {
		t.Error("state.fallbackToContinue = true after successful turn, want false")
	}

	// Turn 2: continuation must produce the same session ID.
	result2, err := adapter.RunTurn(ctx, session, domain.RunTurnParams{
		Prompt:  "Say exactly one word: world",
		OnEvent: func(_ domain.AgentEvent) {},
	})
	if err != nil {
		t.Fatalf("RunTurn (turn 2): %v", err)
	}
	if result2.ExitReason != domain.EventTurnCompleted {
		t.Errorf("turn 2: ExitReason = %q, want %q", result2.ExitReason, domain.EventTurnCompleted)
	}
	if result2.SessionID != result1.SessionID {
		t.Errorf("turn 2: SessionID = %q, want %q (same as turn 1)", result2.SessionID, result1.SessionID)
	}
}

// TestIntegration_ResumeSessionID verifies that StartSession with a
// ResumeSessionID propagates that ID to the first turn's --resume flag.
func TestIntegration_ResumeSessionID(t *testing.T) {
	skipUnlessCopilotIntegration(t)

	adapter, err := NewCopilotAdapter(integrationConfig())
	if err != nil {
		t.Fatalf("NewCopilotAdapter: %v", err)
	}

	workspace := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Establish real session ID from turn 1.
	session1, err := adapter.StartSession(ctx, domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig:   domain.AgentConfig{Command: integrationCommand()},
	})
	if err != nil {
		t.Fatalf("StartSession (session 1): %v", err)
	}
	result1, err := adapter.RunTurn(ctx, session1, domain.RunTurnParams{
		Prompt:  "Say exactly one word: hello",
		OnEvent: func(_ domain.AgentEvent) {},
	})
	if err != nil {
		t.Fatalf("RunTurn (session 1): %v", err)
	}
	if result1.SessionID == "" {
		t.Fatal("session 1: TurnResult.SessionID is empty")
	}

	// Resume via a fresh StartSession with the captured ID.
	session2, err := adapter.StartSession(ctx, domain.StartSessionParams{
		WorkspacePath:   workspace,
		AgentConfig:     domain.AgentConfig{Command: integrationCommand()},
		ResumeSessionID: result1.SessionID,
	})
	if err != nil {
		t.Fatalf("StartSession (resume): %v", err)
	}
	t.Cleanup(func() { _ = adapter.StopSession(context.Background(), session2) })

	// session2.ID must equal the provided ResumeSessionID so the
	// orchestrator can record continuity.
	if session2.ID != result1.SessionID {
		t.Errorf("session2.ID = %q, want %q (provided ResumeSessionID)", session2.ID, result1.SessionID)
	}

	result2, err := adapter.RunTurn(ctx, session2, domain.RunTurnParams{
		Prompt:  "Say exactly one word: world",
		OnEvent: func(_ domain.AgentEvent) {},
	})
	if err != nil {
		t.Fatalf("RunTurn (resumed session): %v", err)
	}
	if result2.ExitReason != domain.EventTurnCompleted {
		t.Errorf("resumed session: ExitReason = %q, want %q", result2.ExitReason, domain.EventTurnCompleted)
	}
	if result2.SessionID != result1.SessionID {
		t.Errorf("resumed session: SessionID = %q, want %q (same original session)",
			result2.SessionID, result1.SessionID)
	}
}
