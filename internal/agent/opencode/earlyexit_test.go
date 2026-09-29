package opencode

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest/credentialtest"
	"github.com/sortie-ai/sortie/internal/domain"
)

func TestEarlyExitConformance(t *testing.T) {
	// Not parallel: AssertEarlyExitReport sets PATH through t.Setenv.
	adapter, err := NewOpenCodeAdapter(map[string]any{})
	if err != nil {
		t.Fatalf("NewOpenCodeAdapter() error = %v", err)
	}

	credentialtest.AssertEarlyExitReport(t, "opencode", adapter, domain.AgentConfig{})
}
