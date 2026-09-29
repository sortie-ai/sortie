package claude

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest/credentialtest"
	"github.com/sortie-ai/sortie/internal/domain"
)

func TestEarlyExitConformance(t *testing.T) {
	// Not parallel: AssertEarlyExitReport sets PATH through t.Setenv.
	adapter, err := NewClaudeCodeAdapter(map[string]any{})
	if err != nil {
		t.Fatalf("NewClaudeCodeAdapter() error = %v", err)
	}

	credentialtest.AssertEarlyExitReport(t, "claude-code", adapter, domain.AgentConfig{})
}
