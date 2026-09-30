package claude

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
)

func effortFlagValue(args []string) string {
	i := slices.Index(args, "--effort")
	if i < 0 || i+1 >= len(args) {
		return ""
	}
	return args[i+1]
}

func claudeEffortProbe(t *testing.T, settings map[string]any, verification, resumed bool, turns int) ([]string, error) {
	t.Helper()

	adapter, err := NewClaudeCodeAdapter()
	if err != nil {
		return nil, err
	}
	resumeID := ""
	if resumed {
		resumeID = "resume-1234-5678-abcd-ef0123456789"
	}
	command := agenttest.FakeRuntime(t, t.TempDir(), "fake-claude", agenttest.OutputScenario, agenttest.Output{})
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath:          t.TempDir(),
		AgentConfig:            domain.AgentConfig{Command: command},
		CredentialVerification: verification,
		ResumeSessionID:        resumeID,
		Settings:               settings,
	})
	if err != nil {
		return nil, fmt.Errorf("StartSession: %w", err)
	}

	state := session.Internal.(*sessionState)
	carried := make([]string, 0, turns)
	for turn := 1; turn <= turns; turn++ {
		carried = append(carried, effortFlagValue(buildArgs(state, turn, "probe prompt", state.passthrough)))
	}
	return carried, nil
}

func TestEffortForwarding(t *testing.T) {
	t.Parallel()

	agenttest.AssertEffortForwarding(t, claudeEffortProbe)
}
