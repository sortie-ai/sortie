package copilot

import (
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
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

func copilotEffortProbe(t *testing.T, passthrough map[string]any, verification bool, turns int) ([]string, error) {
	t.Helper()

	adapter, err := NewCopilotAdapter(passthrough)
	if err != nil {
		return nil, err
	}
	pt := adapter.(*CopilotAdapter).passthrough
	state := &sessionState{
		copilotSessionID:       "aa778ea0-6eab-4ce9-b87e-11d6d33dab4f",
		credentialVerification: verification,
	}

	carried := make([]string, 0, turns)
	for turn := 1; turn <= turns; turn++ {
		carried = append(carried, reasoningEffortValue(buildArgs(state, turn, "probe prompt", pt)))
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

			adapter, err := NewCopilotAdapter(tt.passthrough)
			if err != nil {
				t.Fatalf("NewCopilotAdapter(%v) error = %v", tt.passthrough, err)
			}

			args := buildArgs(&sessionState{}, 1, "p", adapter.(*CopilotAdapter).passthrough)

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
