package kiro

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

// skipIfWorkObserverIntegrationDisabled skips the test unless
// SORTIE_KIRO_TEST=1, and again unless KIRO_API_KEY is set. Without a
// credential a headless kiro-cli chat blocks on interactive device login
// indefinitely, so the second guard prevents a hang.
func skipIfWorkObserverIntegrationDisabled(t *testing.T) {
	t.Helper()
	if os.Getenv("SORTIE_KIRO_TEST") != "1" {
		t.Skip("set SORTIE_KIRO_TEST=1 to run kiro integration tests")
	}
	if os.Getenv("KIRO_API_KEY") == "" {
		t.Skip("set KIRO_API_KEY to run kiro integration tests")
	}
}

// workObserverIntegrationCommand returns the kiro-cli binary path,
// defaulting to "kiro-cli".
func workObserverIntegrationCommand() string {
	if cmd := os.Getenv("SORTIE_KIRO_COMMAND"); cmd != "" {
		return cmd
	}
	return "kiro-cli"
}

// TestKiroAdapter_WorkObserverIntegration drives one real turn against
// the live kiro-cli binary and reads state.work.Observed() from the
// session's internal *sessionState, which this package alone can reach.
// A normal turn sets a positive terminal report through the credits
// trailer, so DecideTurn decides at the terminal-success row and never
// consults Work; turn_completed is a precondition here, not the
// assertion, and the observer must still have recorded the runtime's own
// assistant output.
func TestKiroAdapter_WorkObserverIntegration(t *testing.T) {
	skipIfWorkObserverIntegrationDisabled(t)

	cfg := map[string]any{}
	if model := os.Getenv("SORTIE_KIRO_MODEL"); model != "" {
		cfg["model"] = model
	}
	adapter, err := NewKiroAdapter(cfg)
	if err != nil {
		t.Fatalf("NewKiroAdapter(): %v", err)
	}

	startCtx, startCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer startCancel()

	session, err := adapter.StartSession(startCtx, domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: workObserverIntegrationCommand()},
	})
	if err != nil {
		t.Fatalf("StartSession(): %v", err)
	}
	t.Cleanup(func() { _ = adapter.StopSession(context.Background(), session) })

	turnCtx, turnCancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer turnCancel()

	result, err := adapter.RunTurn(turnCtx, session, domain.RunTurnParams{
		Prompt:  "Reply with exactly: PONG",
		OnEvent: func(domain.AgentEvent) {},
	})
	if err != nil {
		t.Fatalf("RunTurn(): %v", err)
	}
	if result.ExitReason != domain.EventTurnCompleted {
		t.Fatalf("result.ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}

	state, ok := session.Internal.(*sessionState)
	if !ok {
		t.Fatalf("session.Internal type = %T, want *sessionState", session.Internal)
	}
	if !state.work.Observed() {
		t.Error("state.work.Observed() = false after a real turn, want true")
	}
}
