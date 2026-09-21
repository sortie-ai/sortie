package copilot

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/agenttest/credentialtest"
	"github.com/sortie-ai/sortie/internal/domain"
)

const versionAwareScenario = "copilot.version-aware"

type versionAwareParams struct {
	Stdout   string
	ExitCode int
}

func init() {
	fakeScenarios[versionAwareScenario] = agenttest.Typed(runVersionAwareScenario)
}

func runVersionAwareScenario(args []string, params versionAwareParams) int {
	if len(args) > 0 && args[0] == "--version" {
		return agenttest.Output{Stdout: "copilot version 1.2.3\n"}.Run()
	}
	return agenttest.Output{Stdout: params.Stdout, ExitCode: params.ExitCode}.Run()
}

func TestCredentialVerification(t *testing.T) {
	// Not parallel: t.Setenv carries GH_TOKEN and the fake ssh stand-in
	// on PATH.
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")

	const successJSONL = `{"type":"session.task_complete","data":{"summary":"SORTIE_CREDENTIAL_OK","success":true}}
{"type":"result","timestamp":"2026-03-30T22:19:28.097Z","sessionId":"verify-ok","exitCode":0,"usage":{"premiumRequests":0,"totalApiDurationMs":0,"sessionDurationMs":0}}`

	verifiedBin := agenttest.FakeRuntime(t, t.TempDir(), "copilot", versionAwareScenario, versionAwareParams{Stdout: successJSONL, ExitCode: 0})
	unverifiedBin := agenttest.FakeRuntime(t, t.TempDir(), "copilot", versionAwareScenario, versionAwareParams{ExitCode: 1})

	adapter, err := NewCopilotAdapter(map[string]any{})
	if err != nil {
		t.Fatalf("NewCopilotAdapter() error = %v", err)
	}

	credentialtest.AssertCredentialVerification(t, "copilot-cli", credentialtest.RuntimeCases(t, adapter, domain.AgentConfig{}, verifiedBin, unverifiedBin, "copilot"))
}
