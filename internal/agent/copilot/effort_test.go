package copilot

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

const reasoningEffortFlag = "--reasoning-effort"

func reasoningEffortValue(args []string) string {
	i := slices.Index(args, reasoningEffortFlag)
	if i < 0 || i+1 >= len(args) {
		return ""
	}
	return args[i+1]
}

func copilotEffortProbe(t *testing.T, settings map[string]any, verification, resumed bool, turns int) ([]string, error) {
	t.Helper()

	adapter, err := NewCopilotAdapter()
	if err != nil {
		return nil, err
	}
	resumeID := ""
	if resumed {
		resumeID = "aa778ea0-6eab-4ce9-b87e-11d6d33dab4f"
	}
	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath:          t.TempDir(),
		AgentConfig:            domain.AgentConfig{Command: fakeCopilotBinary(t)},
		ResumeSessionID:        resumeID,
		CredentialVerification: verification,
		Settings:               settings,
	})
	if err != nil {
		return nil, fmt.Errorf("StartSession: %w", err)
	}
	state := session.Internal.(*sessionState)

	carried := make([]string, 0, turns)
	for turn := 1; turn <= turns; turn++ {
		carried = append(carried, reasoningEffortValue(buildArgs(state, turn, "probe prompt", state.passthrough)))
	}
	return carried, nil
}

func TestEffortForwarding(t *testing.T) {
	t.Parallel()

	agenttest.AssertEffortForwarding(t, copilotEffortProbe)
}

func TestEffortFlagPosition(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		passthrough map[string]any
		wantEffort  bool
	}{
		{
			name:        "set effort sits directly after model and before agent",
			passthrough: map[string]any{"model": "gpt-5", registry.EffortKey: "high", "agent": "reviewer"},
			wantEffort:  true,
		},
		{
			name:        "unset effort adds no flag",
			passthrough: map[string]any{"model": "gpt-5", "agent": "reviewer"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pt, fault := parsePassthroughConfig(tt.passthrough)
			if fault != nil {
				t.Fatalf("parsePassthroughConfig(%v) fault = %v", tt.passthrough, fault)
			}

			args := buildArgs(&sessionState{}, 1, "p", pt)

			modelAt := slices.Index(args, "--model")
			agentAt := slices.Index(args, "--agent")
			effortAt := slices.Index(args, reasoningEffortFlag)
			if !tt.wantEffort {
				if effortAt >= 0 {
					t.Errorf("buildArgs(%v) = %q, want no %s", tt.passthrough, args, reasoningEffortFlag)
				}
				if agentAt != modelAt+2 {
					t.Errorf("buildArgs(%v) = %q, want --agent directly after the --model pair", tt.passthrough, args)
				}
				return
			}
			if effortAt != modelAt+2 || agentAt != effortAt+2 {
				t.Errorf("buildArgs(%v) = %q, want %s directly after the --model pair and directly before --agent", tt.passthrough, args, reasoningEffortFlag)
			}
		})
	}
}
