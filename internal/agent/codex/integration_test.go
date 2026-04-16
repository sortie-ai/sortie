// Integration tests for the Codex CLI agent adapter.
//
// Required environment variables:
//
//	SORTIE_CODEX_TEST=1     enable this suite
//	CODEX_API_KEY           Codex API key for authentication
//
// Run:
//
//	SORTIE_CODEX_TEST=1 CODEX_API_KEY=... make test PKG=./internal/agent/codex/... RUN=Integration
package codex

import (
	"context"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

// skipUnlessCodexIntegration skips the current test when SORTIE_CODEX_TEST
// is not set to "1", so disabled integration tests are reported as skipped
// rather than silently passing.
func skipUnlessCodexIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("SORTIE_CODEX_TEST") != "1" {
		t.Skip("skipping Codex integration test: set SORTIE_CODEX_TEST=1 to enable")
	}
	if os.Getenv("CODEX_API_KEY") == "" {
		t.Skip("skipping Codex integration test: CODEX_API_KEY must be set")
	}
}

// integrationCommand returns the Codex CLI binary command from the
// SORTIE_CODEX_COMMAND environment variable, defaulting to "codex app-server".
func integrationCommand() string {
	if cmd := os.Getenv("SORTIE_CODEX_COMMAND"); cmd != "" {
		return cmd
	}
	return "codex app-server"
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

func TestCodexAdapter_Integration(t *testing.T) {
	skipUnlessCodexIntegration(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Create a temp directory with git init for the workspace.
	dir := t.TempDir()
	gitCmd := exec.CommandContext(ctx, "git", "-C", dir, "init")
	if out, err := gitCmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	adapter, err := NewCodexAdapter(map[string]any{})
	if err != nil {
		t.Fatalf("NewCodexAdapter() error = %v", err)
	}

	session, err := adapter.StartSession(ctx, domain.StartSessionParams{
		WorkspacePath: dir,
		AgentConfig: domain.AgentConfig{
			Command:       integrationCommand(),
			TurnTimeoutMS: 60000,
			ReadTimeoutMS: 30000,
		},
	})
	if err != nil {
		t.Fatalf("StartSession() error = %v", err)
	}

	if session.ID == "" {
		t.Error("session.ID is empty")
	}

	var (
		mu     sync.Mutex
		events []domain.AgentEvent
	)

	result, runErr := adapter.RunTurn(ctx, session, domain.RunTurnParams{
		Prompt: "Say hello in exactly one word. Do not write any code.",
		OnEvent: func(ev domain.AgentEvent) {
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
		},
	})

	if runErr != nil {
		t.Logf("RunTurn() error = %v (non-fatal: recording for diagnostics)", runErr)
	}

	if result.SessionID != session.ID {
		t.Errorf("TurnResult.SessionID = %q, want %q", result.SessionID, session.ID)
	}

	mu.Lock()
	capturedEvents := append([]domain.AgentEvent(nil), events...)
	mu.Unlock()

	t.Logf("received %d events, exit reason: %q", len(capturedEvents), result.ExitReason)
	assertContainsEventType(t, capturedEvents, domain.EventSessionStarted)

	if stopErr := adapter.StopSession(ctx, session); stopErr != nil {
		t.Errorf("StopSession() error = %v", stopErr)
	}
}
