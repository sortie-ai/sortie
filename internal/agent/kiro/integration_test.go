package kiro_test

import (
	"context"
	"os"
	"testing"
	"time"

	_ "github.com/sortie-ai/sortie/internal/agent/kiro"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

// skipIfNotEnabled skips the test unless SORTIE_KIRO_TEST=1, and again unless
// KIRO_API_KEY is set. Without a credential a headless kiro-cli chat blocks on
// interactive device login indefinitely, so the second guard prevents a hang.
func skipIfNotEnabled(t *testing.T) {
	t.Helper()
	if os.Getenv("SORTIE_KIRO_TEST") != "1" {
		t.Skip("set SORTIE_KIRO_TEST=1 to run kiro integration tests")
	}
	if os.Getenv("KIRO_API_KEY") == "" {
		t.Skip("set KIRO_API_KEY to run kiro integration tests")
	}
}

// integrationCommand returns the kiro-cli binary path, defaulting to "kiro-cli".
func integrationCommand() string {
	if cmd := os.Getenv("SORTIE_KIRO_COMMAND"); cmd != "" {
		return cmd
	}
	return "kiro-cli"
}

// mustNewAdapter constructs the registered kiro adapter or fatals.
func mustNewAdapter(t *testing.T) domain.AgentAdapter {
	t.Helper()
	factory, err := registry.Agents.Get("kiro")
	if err != nil {
		t.Fatalf("registry.Agents.Get(kiro): %v", err)
	}
	cfg := map[string]any{}
	if model := os.Getenv("SORTIE_KIRO_MODEL"); model != "" {
		cfg["model"] = model
	}
	a, err := factory(cfg)
	if err != nil {
		t.Fatalf("factory(): %v", err)
	}
	return a
}

// TestKiroAdapter_Integration drives one real turn against the live
// kiro-cli binary and asserts turn_completed, satisfying the shared
// disposition decision's live-runtime obligation for this adapter: the
// only check that can catch an evidence mapping that is internally
// consistent but wrong against the actual wire format.
func TestKiroAdapter_Integration(t *testing.T) {
	skipIfNotEnabled(t)

	adapter := mustNewAdapter(t)

	startCtx, startCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer startCancel()

	session, err := adapter.StartSession(startCtx, domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: integrationCommand()},
	})
	if err != nil {
		t.Fatalf("StartSession(): %v", err)
	}
	t.Cleanup(func() { _ = adapter.StopSession(context.Background(), session) })

	// The turn timeout is the mandatory backstop against the login hang.
	turnCtx, turnCancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer turnCancel()

	var events []domain.AgentEvent
	result, err := adapter.RunTurn(turnCtx, session, domain.RunTurnParams{
		Prompt: "Reply with exactly: PONG",
		OnEvent: func(e domain.AgentEvent) {
			events = append(events, e)
		},
	})
	if err != nil {
		t.Fatalf("RunTurn(): %v", err)
	}

	if result.ExitReason != domain.EventTurnCompleted {
		t.Errorf("result.ExitReason = %q, want %q", result.ExitReason, domain.EventTurnCompleted)
	}
	if len(events) == 0 {
		t.Error("no events reached the OnEvent callback, want at least one")
	}

	if err := adapter.StopSession(context.Background(), session); err != nil {
		t.Errorf("StopSession(): %v", err)
	}
}
