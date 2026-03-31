package copilot

import (
	"context"
	"os"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

func skipUnlessCopilotIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("SORTIE_COPILOT_TEST") != "1" {
		t.Skip("skipping Copilot CLI integration test: set SORTIE_COPILOT_TEST=1 to enable")
	}
}

// TestIntegration_SimpleSession runs a single-turn headless session against
// a real Copilot CLI installation. Requires SORTIE_COPILOT_TEST=1 and a
// valid GitHub authentication source (COPILOT_GITHUB_TOKEN, GH_TOKEN, or
// GITHUB_TOKEN).
func TestIntegration_SimpleSession(t *testing.T) {
	skipUnlessCopilotIntegration(t)

	workspace := t.TempDir()
	adapter, err := NewCopilotAdapter(map[string]any{
		"max_autopilot_continues": float64(3),
	})
	if err != nil {
		t.Fatalf("NewCopilotAdapter: %v", err)
	}

	ctx := context.Background()
	session, err := adapter.StartSession(ctx, domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig:   domain.AgentConfig{Command: "copilot"},
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	var events []domain.AgentEvent
	result, runErr := adapter.RunTurn(ctx, session, domain.RunTurnParams{
		Prompt: "Say exactly one word: hello",
		OnEvent: func(ev domain.AgentEvent) {
			events = append(events, ev)
		},
	})
	if runErr != nil {
		t.Fatalf("RunTurn: %v", runErr)
	}

	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}
	if result.SessionID == "" {
		t.Error("TurnResult.SessionID is empty; expected session ID from result event")
	}

	// At minimum we should have received a session_started and turn_completed event.
	var foundStarted, foundCompleted bool
	for _, ev := range events {
		switch ev.Type {
		case domain.EventSessionStarted:
			foundStarted = true
		case domain.EventTurnCompleted:
			foundCompleted = true
		}
	}
	if !foundStarted {
		t.Error("did not receive EventSessionStarted")
	}
	if !foundCompleted {
		t.Error("did not receive EventTurnCompleted")
	}

	// StopSession should be a no-op since RunTurn already exited.
	if err := adapter.StopSession(ctx, session); err != nil {
		t.Errorf("StopSession after completed turn: %v", err)
	}
}

// TestIntegration_ContinuationTurn verifies that a second turn using
// --resume <session_id> produces the same session ID in the result event.
func TestIntegration_ContinuationTurn(t *testing.T) {
	skipUnlessCopilotIntegration(t)

	workspace := t.TempDir()
	adapter, err := NewCopilotAdapter(map[string]any{
		"max_autopilot_continues": float64(3),
	})
	if err != nil {
		t.Fatalf("NewCopilotAdapter: %v", err)
	}

	ctx := context.Background()
	session, err := adapter.StartSession(ctx, domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig:   domain.AgentConfig{Command: "copilot"},
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}

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

	// Turn 2: continuation must use the same session ID.
	// Update the session's Internal state to reflect the captured session ID.
	state := session.Internal.(*sessionState)
	state.copilotSessionID = result1.SessionID

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

	if err := adapter.StopSession(ctx, session); err != nil {
		t.Errorf("StopSession: %v", err)
	}
}
