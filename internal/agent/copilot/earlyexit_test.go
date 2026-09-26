package copilot

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest/credentialtest"
	"github.com/sortie-ai/sortie/internal/domain"
)

func TestEarlyExitConformance(t *testing.T) {
	// Not parallel: AssertEarlyExitReport sets PATH through t.Setenv.
	adapter, err := NewCopilotAdapter(map[string]any{})
	if err != nil {
		t.Fatalf("NewCopilotAdapter() error = %v", err)
	}

	credentialtest.AssertEarlyExitReport(t, "copilot-cli", adapter, domain.AgentConfig{}, credentialtest.StructuredOutput)
}
