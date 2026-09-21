package codex

import (
	"bufio"
	"io"
	"os"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/agenttest/credentialtest"
	"github.com/sortie-ai/sortie/internal/domain"
)

const scenarioCredentialFullTurn = "codex.credential-full-turn"

type credentialFullTurnParams struct {
	Status string `json:"status,omitempty"`
}

func init() {
	fakeScenarios[scenarioCredentialFullTurn] = agenttest.Typed(func(_ []string, params credentialFullTurnParams) int {
		return serveCredentialFullTurn(newFakeAppServerScanner(), os.Stdout, params.Status)
	})
}

func serveCredentialFullTurn(client *bufio.Scanner, out io.Writer, status string) int {
	if !answerPreThreadHandshake(client, out) {
		return 1
	}
	threadStart, ok := nextFrame(client)
	if !ok {
		return 1
	}
	if !writeJSON(out, rpcResult{ID: threadStart.ID, Result: map[string]any{"thread": map[string]any{"id": "fake-thread-1"}}}) {
		return 1
	}
	if !writeJSON(out, rpcNotification{Method: "thread/started"}) {
		return 1
	}

	turnStart, ok := nextFrame(client)
	if !ok {
		return 1
	}
	if !writeJSON(out, rpcResult{ID: turnStart.ID, Result: map[string]any{"turn": map[string]any{"id": "t1"}}}) {
		return 1
	}
	if !writeJSON(out, rpcNotification{Method: "turn/completed", Params: map[string]any{"turn": map[string]any{"id": "t1", "status": status}}}) {
		return 1
	}

	for client.Scan() {
	}
	return 0
}

// With no CODEX_API_KEY the handshake attempts no login, so a refused
// credential surfaces as a failed turn.
func TestCredentialVerification(t *testing.T) {
	// Not parallel: t.Setenv carries the fake ssh stand-in on PATH.
	verifiedBin := agenttest.FakeRuntime(t, t.TempDir(), "codex", scenarioCredentialFullTurn, credentialFullTurnParams{Status: "completed"})
	unverifiedBin := agenttest.FakeRuntime(t, t.TempDir(), "codex", scenarioCredentialFullTurn, credentialFullTurnParams{Status: "failed"})

	adapter, err := NewCodexAdapter(map[string]any{})
	if err != nil {
		t.Fatalf("NewCodexAdapter() error = %v", err)
	}

	credentialtest.AssertCredentialVerification(t, "codex", credentialtest.RuntimeCases(t, adapter, domain.AgentConfig{}, verifiedBin, unverifiedBin, "codex"))
}
