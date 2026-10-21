//go:build unix

package pi

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest/credentialtest"
	"github.com/sortie-ai/sortie/internal/domain"
)

func TestCredentialVerification(t *testing.T) {
	// Not parallel: RuntimeCases puts the fake ssh stand-in on PATH.
	verifiedBin := writeRunFixtureScript(t, t.TempDir(), "simple_turn.jsonl")
	unverifiedBin := writeRunFixtureScript(t, t.TempDir(), "assistant_error.jsonl")

	adapter, err := NewPiAdapter()
	if err != nil {
		t.Fatalf("NewPiAdapter() error = %v", err)
	}

	credentialtest.AssertCredentialVerification(t, "pi", credentialtest.RuntimeCases(t, adapter, domain.AgentConfig{}, verifiedBin, unverifiedBin, "pi"))
}
