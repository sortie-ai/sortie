//go:build unix

package clientprotocol

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest/credentialtest"
	"github.com/sortie-ai/sortie/internal/domain"
)

func TestEarlyExitConformance(t *testing.T) {
	// Not parallel: AssertEarlyExitReport sets PATH through t.Setenv.
	adapter, err := NewClientProtocolAdapter(map[string]any{})
	if err != nil {
		t.Fatalf("NewClientProtocolAdapter() error = %v", err)
	}

	credentialtest.AssertEarlyExitReport(t, "agent-client-protocol", adapter, domain.AgentConfig{ReadTimeoutMS: 5000}, credentialtest.StructuredOutput)
}
