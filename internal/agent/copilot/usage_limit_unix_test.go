//go:build unix

package copilot

import (
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
)

func TestUsageLimitRefusal(t *testing.T) {
	// The live suite keeps the host tokens, and COPILOT_GITHUB_TOKEN outranks
	// GH_TOKEN, so a real account could supply a reset these cases must not see.
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "test-token-for-unit-test")
	t.Setenv("COPILOT_HOME", t.TempDir())

	recorded := loadTestFixture(t, "usage_limit_quota.jsonl")
	beforeResult, _, ok := strings.Cut(recorded, `{"type":"result"`)
	rateLimited := strings.Replace(beforeResult, `"errorType":"quota"`, `"errorType":"rate_limit"`, 1)
	if !ok || rateLimited == beforeResult {
		t.Fatal("usage_limit_quota.jsonl carries no result line and quota errorType to derive the rate_limit variant from")
	}
	refusal := func(name, stdout string, exitCode int) agenttest.UsageLimitRefusalCase {
		dir := t.TempDir()
		adapter, err := NewCopilotAdapter()
		if err != nil {
			t.Fatalf("NewCopilotAdapter: %v", err)
		}
		command := agenttest.FakeRuntime(t, dir, "copilot", versionAwareScenario, versionAwareParams{Stdout: stdout, ExitCode: exitCode})
		return agenttest.UsageLimitRefusalCase{
			Name:          name,
			Adapter:       adapter,
			Start:         domain.StartSessionParams{WorkspacePath: dir, AgentConfig: domain.AgentConfig{Command: command}},
			Turn:          domain.RunTurnParams{Prompt: "hello"},
			WantModelOnly: true,
		}
	}

	agenttest.AssertUsageLimitRefusal(t, "copilot-cli", []agenttest.UsageLimitRefusalCase{
		refusal("recorded quota refusal with result line and exit 1", recorded, 1),
		refusal("rate_limit refusal with no result line and exit 0", rateLimited, 0),
	})
}
