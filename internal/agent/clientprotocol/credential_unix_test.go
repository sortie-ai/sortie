//go:build unix

package clientprotocol

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/agenttest/credentialtest"
	"github.com/sortie-ai/sortie/internal/domain"
)

type credentialAgentOutcome string

const (
	credentialAgentEndTurn  credentialAgentOutcome = "end_turn"
	credentialAgentAuthAuth credentialAgentOutcome = "auth_required"
)

const credentialAgentScenario = "clientprotocol.credential-agent"

type credentialAgentParams struct {
	Outcome credentialAgentOutcome
}

func init() {
	fakeRuntimeScenarios[credentialAgentScenario] = agenttest.Typed(runCredentialAgent)
}

func runCredentialAgent(_ []string, params credentialAgentParams) int {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)

	for scanner.Scan() {
		line := scanner.Bytes()

		var header wireHeader
		if err := json.Unmarshal(line, &header); err != nil {
			continue
		}

		switch header.Method {
		case methodInitialize:
			if err := respondInitialize(false); err != nil {
				fmt.Fprintf(os.Stderr, "credential agent: respond initialize: %v\n", err)
				return 2
			}
		case methodSessionNew:
			if err := respondSessionNew(); err != nil {
				fmt.Fprintf(os.Stderr, "credential agent: respond session/new: %v\n", err)
				return 2
			}
		case methodSessionPrompt:
			if err := respondCredentialPrompt(header.ID, params.Outcome); err != nil {
				fmt.Fprintf(os.Stderr, "credential agent: respond session/prompt: %v\n", err)
				return 2
			}
		case methodSessionClose, methodSessionDelete:
			if err := writeJSONLine(os.Stdout, outboundResponse{JSONRPC: "2.0", ID: idAsInt(header.ID), Result: struct{}{}}); err != nil {
				return 2
			}
		}
	}
	return 0
}

func idAsInt(raw json.RawMessage) int {
	var n int
	_ = json.Unmarshal(raw, &n)
	return n
}

func respondCredentialPrompt(id json.RawMessage, outcome credentialAgentOutcome) error {
	if outcome == credentialAgentAuthAuth {
		return writeJSONLine(os.Stdout, struct {
			JSONRPC string `json:"jsonrpc"`
			ID      int    `json:"id"`
			Error   struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}{JSONRPC: "2.0", ID: idAsInt(id), Error: struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}{Code: -32000, Message: "API key is invalid."}})
	}
	return writeJSONLine(os.Stdout, outboundResponse{JSONRPC: "2.0", ID: idAsInt(id), Result: promptResponse{StopReason: stopReasonEndTurn}})
}

func TestCredentialVerification(t *testing.T) {
	// Not parallel: t.Setenv carries the fake ssh stand-in on PATH.
	verifiedBin := agenttest.FakeRuntime(t, t.TempDir(), "protocol-agent", credentialAgentScenario, credentialAgentParams{Outcome: credentialAgentEndTurn})
	unverifiedBin := agenttest.FakeRuntime(t, t.TempDir(), "protocol-agent", credentialAgentScenario, credentialAgentParams{Outcome: credentialAgentAuthAuth})

	adapter, err := NewClientProtocolAdapter(map[string]any{})
	if err != nil {
		t.Fatalf("NewClientProtocolAdapter() error = %v", err)
	}

	credentialtest.AssertCredentialVerification(t, "agent-client-protocol", credentialtest.RuntimeCases(t, adapter, domain.AgentConfig{ReadTimeoutMS: 5000}, verifiedBin, unverifiedBin, "protocol-agent"))
}
