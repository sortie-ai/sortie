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

	adapter, err := NewClaudeCodeAdapter(singleTurnIntegrationConfig(t))
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

	adapter, err := NewClaudeCodeAdapter(singleTurnIntegrationConfig(t))
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

	adapter, err := NewClaudeCodeAdapter(singleTurnIntegrationConfig(t))
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

// TestIntegration_RunTurn drives one real turn against the live
// claude-code binary and asserts turn_completed, satisfying the shared
// disposition decision's live-runtime obligation for this adapter: the
// only check that can catch an evidence mapping that is internally
// consistent but wrong against the actual wire format.
func TestIntegration_RunTurn(t *testing.T) {
	skipUnlessIntegration(t)

	adapter, err := NewClaudeCodeAdapter(singleTurnIntegrationConfig(t))
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

	// Verify at least one EventToolResult with a correlated ToolName.
	// The prompt causes Claude Code to use the Read tool, producing
	// tool_use + tool_result content blocks. Asserting != "unknown"
	// validates that tool_use↔tool_result correlation succeeded.
	var foundToolResult bool
	for _, e := range collected {
		if e.Type == domain.EventToolResult && e.ToolName != "" && e.ToolName != "unknown" {
			foundToolResult = true
			if e.ToolDurationMS < 0 {
				t.Errorf("EventToolResult.ToolDurationMS = %d, want >= 0", e.ToolDurationMS)
			}
			break
		}
	}
	if !foundToolResult {
		var toolNames []string
		for _, e := range collected {
			if e.Type == domain.EventToolResult {
				toolNames = append(toolNames, e.ToolName)
			}
		}
		t.Errorf("expected EventToolResult with correlated ToolName; got tool results: %v", toolNames)
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

	adapter, err := NewClaudeCodeAdapter(singleTurnIntegrationConfig(t))
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

// TestIntegration_SessionResume drives two real turns against the live
// claude-code binary in the same workspace: a first turn to completion,
// then a second turn from a freshly constructed adapter that resumes the
// first turn's session ID, asserting the resumed turn also completes.
// It builds its own config with session persistence left at its
// default, rather than calling singleTurnIntegrationConfig, because
// that helper disables the persistence this test exercises.
func TestIntegration_SessionResume(t *testing.T) {
	skipUnlessIntegration(t)

	model := os.Getenv("SORTIE_CLAUDE_MODEL")
	if model == "" {
		model = "claude-haiku-4-5"
	}
	cfg := map[string]any{"model": model}

	workspace := t.TempDir()
	noopEvent := func(domain.AgentEvent) {}

	adapter, err := NewClaudeCodeAdapter(cfg)
	if err != nil {
		t.Fatalf("NewClaudeCodeAdapter: %v", err)
	}

	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig:   domain.AgentConfig{Command: integrationCommand(t)},
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

	adapter2, err := NewClaudeCodeAdapter(cfg)
	if err != nil {
		t.Fatalf("NewClaudeCodeAdapter (resumed turn): %v", err)
	}

	session2, err := adapter2.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath:   workspace,
		AgentConfig:     domain.AgentConfig{Command: integrationCommand(t)},
		ResumeSessionID: result1.SessionID,
	})
	if err != nil {
		t.Fatalf("StartSession (resumed turn): %v", err)
	}
	t.Cleanup(func() { _ = adapter2.StopSession(context.Background(), session2) })

	ctx2, cancel2 := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel2()

	result2, err := adapter2.RunTurn(ctx2, session2, domain.RunTurnParams{
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
