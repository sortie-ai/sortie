package claude

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

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

// integrationConfig builds the adapter config map for integration tests.
// Session persistence is disabled to prevent ~/.claude/ pollution from
// repeated test runs.
func integrationConfig(t *testing.T) map[string]any {
	t.Helper()
	cfg := map[string]any{
		"session_persistence": false,
	}
	if model := os.Getenv("SORTIE_CLAUDE_MODEL"); model != "" {
		cfg["model"] = model
	}
	return cfg
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

// --- Integration test functions ---

func TestIntegration_StartSession(t *testing.T) {
	skipUnlessIntegration(t)

	adapter, err := NewClaudeCodeAdapter(integrationConfig(t))
	if err != nil {
		t.Fatalf("NewClaudeCodeAdapter: %v", err)
	}

	workspace := t.TempDir()

	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
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

	adapter, err := NewClaudeCodeAdapter(integrationConfig(t))
	if err != nil {
		t.Fatalf("NewClaudeCodeAdapter: %v", err)
	}

	workspace := t.TempDir()

	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
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

	adapter, err := NewClaudeCodeAdapter(integrationConfig(t))
	if err != nil {
		t.Fatalf("NewClaudeCodeAdapter: %v", err)
	}

	_, err = adapter.StartSession(context.Background(), domain.StartSessionParams{
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

func TestIntegration_RunTurn(t *testing.T) {
	skipUnlessIntegration(t)

	adapter, err := NewClaudeCodeAdapter(integrationConfig(t))
	if err != nil {
		t.Fatalf("NewClaudeCodeAdapter: %v", err)
	}

	workspace := t.TempDir()
	if err := os.WriteFile(workspace+"/hello.txt", []byte("Hello"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
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

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	prompt := "Read the file hello.txt. Output EXACTLY the file content and absolutely nothing else. No preamble, no explanation."

	result, err := adapter.RunTurn(ctx, session, domain.RunTurnParams{
		Prompt:  prompt,
		OnEvent: onEvent,
	})
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}

	mu.Lock()
	collected := make([]domain.AgentEvent, len(events))
	copy(collected, events)
	mu.Unlock()

	if result.SessionID == "" {
		t.Error("TurnResult.SessionID is empty")
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("TurnResult.ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}
	if len(collected) == 0 {
		t.Fatal("no events received via OnEvent")
	}

	assertContainsEventType(t, collected, domain.EventSessionStarted)
	assertContainsEventType(t, collected, domain.EventTurnCompleted)
	assertContainsEventType(t, collected, domain.EventTokenUsage)
	assertNoEventType(t, collected, domain.EventTurnFailed)
	assertNoEventType(t, collected, domain.EventStartupFailed)

	// Verify at least one EventToolResult with a non-empty ToolName.
	// The prompt causes Claude Code to use the Read tool, producing
	// tool_use + tool_result content blocks.
	var foundToolResult bool
	for _, e := range collected {
		if e.Type == domain.EventToolResult && e.ToolName != "" {
			foundToolResult = true
			if e.ToolDurationMS < 0 {
				t.Errorf("EventToolResult.ToolDurationMS = %d, want >= 0", e.ToolDurationMS)
			}
			break
		}
	}
	if !foundToolResult {
		types := make([]domain.AgentEventType, len(collected))
		for i, e := range collected {
			types[i] = e.Type
		}
		t.Errorf("expected EventToolResult with non-empty ToolName not found; got types: %v", types)
	}

	for _, e := range collected {
		if e.Type == domain.EventTokenUsage {
			if e.Usage.InputTokens <= 0 {
				t.Errorf("EventTokenUsage.InputTokens = %d, want > 0", e.Usage.InputTokens)
			}
			if e.Usage.OutputTokens <= 0 {
				t.Errorf("EventTokenUsage.OutputTokens = %d, want > 0", e.Usage.OutputTokens)
			}
			if e.Usage.TotalTokens != e.Usage.InputTokens+e.Usage.OutputTokens {
				t.Errorf("EventTokenUsage.TotalTokens = %d, want %d (input + output)",
					e.Usage.TotalTokens, e.Usage.InputTokens+e.Usage.OutputTokens)
			}
			break
		}
	}

	if result.Usage.TotalTokens <= 0 {
		t.Errorf("TurnResult.Usage.TotalTokens = %d, want > 0", result.Usage.TotalTokens)
	}
}

func TestIntegration_RunTurn_ContextCancellation(t *testing.T) {
	skipUnlessIntegration(t)

	adapter, err := NewClaudeCodeAdapter(integrationConfig(t))
	if err != nil {
		t.Fatalf("NewClaudeCodeAdapter: %v", err)
	}

	workspace := t.TempDir()
	if err := os.WriteFile(workspace+"/dummy.txt", []byte("test"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
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

	shortCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
