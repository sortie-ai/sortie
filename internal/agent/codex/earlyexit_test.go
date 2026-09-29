package codex

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest/credentialtest"
	"github.com/sortie-ai/sortie/internal/domain"
)

func TestEarlyExitConformance(t *testing.T) {
	// Not parallel: AssertEarlyExitReport sets PATH through t.Setenv.
	adapter, err := NewCodexAdapter(map[string]any{})
	if err != nil {
		t.Fatalf("NewCodexAdapter() error = %v", err)
	}

	credentialtest.AssertEarlyExitReport(t, "codex", adapter, domain.AgentConfig{})
}
