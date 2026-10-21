package pi

import (
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/registry"
)

func newTestSessionState(workspacePath, sessionID string) *sessionState {
	return &sessionState{
		target:    agentcore.LaunchTarget{WorkspacePath: workspacePath},
		sessionID: sessionID,
	}
}

func TestParsePassthroughConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		config map[string]any
		want   passthroughConfig
	}{
		{name: "defaults", config: map[string]any{}},
		{name: "model", config: map[string]any{"model": "anthropic/claude-sonnet-4-5"}, want: passthroughConfig{Model: "anthropic/claude-sonnet-4-5"}},
		{name: "approve", config: map[string]any{"approve": true}, want: passthroughConfig{Approve: true}},
		{name: "approve false", config: map[string]any{"approve": false}},
		{name: "effort is not read", config: map[string]any{"effort": "high"}},
		{name: "mcp_config is not read", config: map[string]any{"mcp_config": "./mcp-servers.json"}},
		{name: "another key is ignored", config: map[string]any{"project_trust": "approve", "unknown": 1}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, fault := parsePassthroughConfig(tt.config)
			if fault != nil {
				t.Fatalf("parsePassthroughConfig() fault = %v", fault)
			}
			if got != tt.want {
				t.Errorf("parsePassthroughConfig() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParsePassthroughConfig_WrongModelType(t *testing.T) {
	t.Parallel()

	_, fault := parsePassthroughConfig(map[string]any{"model": 7})
	if fault == nil || fault.Key != "model" {
		t.Fatalf("parsePassthroughConfig() fault = %v, want a model type fault", fault)
	}
}

func TestValidateConfig(t *testing.T) {
	t.Parallel()

	accepted := map[string]any{
		"model":      "anthropic/claude-sonnet-4-5",
		"effort":     "high",
		"mcp_config": "./mcp-servers.json",
		"approve":    true,
	}
	if diags := validateConfig(registry.AgentConfigFields{Kind: "pi", Passthrough: accepted}); len(diags) != 0 {
		t.Errorf("validateConfig() = %+v, want no diagnostics", diags)
	}

	diags := validateConfig(registry.AgentConfigFields{Kind: "pi", Passthrough: map[string]any{"model": 7}})
	if len(diags) != 1 || diags[0].Check != "pi.model.wrong_type" || diags[0].Severity != "error" {
		t.Errorf("validateConfig() = %+v, want one pi.model.wrong_type error", diags)
	}
}

func TestBuildRunArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		sessionID    string
		verification bool
		reduced      bool
		pt           passthroughConfig
		want         []string
	}{
		{name: "fresh session", want: []string{"-p", "--mode", "json", "--no-approve"}},
		{name: "resume", sessionID: "ses_abc", want: []string{"-p", "--mode", "json", "--session", "ses_abc", "--no-approve"}},
		{name: "model", pt: passthroughConfig{Model: "anthropic/claude-sonnet-4-5"}, want: []string{"-p", "--mode", "json", "--model", "anthropic/claude-sonnet-4-5", "--no-approve"}},
		{name: "approve", pt: passthroughConfig{Approve: true}, want: []string{"-p", "--mode", "json", "--approve"}},
		{name: "credential verification is ephemeral", verification: true, want: []string{"-p", "--mode", "json", "--no-session", "--no-approve"}},
		{name: "reduced credential verification drops the agent context", verification: true, reduced: true, pt: passthroughConfig{Model: "anthropic/claude-sonnet-4-5"}, want: []string{"-p", "--mode", "json", "--no-session", "--no-tools", "--tools", "", "--no-context-files", "--no-skills", "--system-prompt", agentcore.VerificationSystemPrompt, "--append-system-prompt", "", "--model", "anthropic/claude-sonnet-4-5", "--no-approve"}},
		{name: "credential verification never resumes", sessionID: "ses_abc", verification: true, want: []string{"-p", "--mode", "json", "--no-session", "--no-approve"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := newTestSessionState("/tmp/workspace", tt.sessionID)
			state.credentialVerification = tt.verification
			state.reducedVerificationContext = tt.reduced
			got := buildRunArgs(state, tt.pt)
			if !slices.Equal(got, tt.want) {
				t.Errorf("buildRunArgs() = [%s], want [%s]", strings.Join(got, " "), strings.Join(tt.want, " "))
			}
		})
	}
}
