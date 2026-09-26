package kiro

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest/credentialtest"
	"github.com/sortie-ai/sortie/internal/domain"
)

// TestEarlyExitConformance's verification-request case ends at the
// whoami guard, which carries the configured switch too, distinct from
// TestVerifyCredential_ChatEarlyExitReport's dedicated chat-path case.
func TestEarlyExitConformance(t *testing.T) {
	// Not parallel: AssertEarlyExitReport sets PATH through t.Setenv.
	adapter, err := NewKiroAdapter(map[string]any{})
	if err != nil {
		t.Fatalf("NewKiroAdapter() error = %v", err)
	}

	credentialtest.AssertEarlyExitReport(t, "kiro", adapter, domain.AgentConfig{}, credentialtest.PlainTextOutput)
}
