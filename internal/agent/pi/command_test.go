package pi

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/domain"
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
		{name: "approve null keeps the default", config: map[string]any{"approve": nil}},
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

func TestParsePassthroughConfig_WrongApproveType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		value   any
		wantGot string
	}{
		{name: "string yes", value: "yes", wantGot: "string"},
		{name: "string true", value: "true", wantGot: "string"},
		{name: "integer", value: 1, wantGot: "integer"},
		{name: "float", value: 1.5, wantGot: "float"},
		{name: "list", value: []any{true}, wantGot: "list"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, fault := parsePassthroughConfig(map[string]any{"approve": tt.value})
			if fault == nil {
				t.Fatalf("parsePassthroughConfig(approve: %v) fault = nil, want an approve type fault", tt.value)
			}
			if fault.Key != "approve" || fault.Want != "boolean" || fault.Got != tt.wantGot {
				t.Errorf("parsePassthroughConfig(approve: %v) fault = %+v, want key approve, want boolean, got %s", tt.value, *fault, tt.wantGot)
			}
			if got != (passthroughConfig{}) {
				t.Errorf("parsePassthroughConfig(approve: %v) = %+v, want the zero config with the fault", tt.value, got)
			}
		})
	}
}

func TestStartSession_RefusesWrongApproveType(t *testing.T) {
	t.Parallel()

	a, err := NewPiAdapter()
	if err != nil {
		t.Fatalf("NewPiAdapter() error = %v", err)
	}

	session, err := a.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: t.TempDir(),
		AgentConfig:   domain.AgentConfig{Command: filepath.Join(t.TempDir(), "pi")},
		Settings:      map[string]any{"approve": "yes"},
	})

	var agentErr *domain.AgentError
	if !errors.As(err, &agentErr) {
		t.Fatalf("StartSession(approve: yes) error = %v (%T), want a *domain.AgentError", err, err)
	}
	if agentErr.Message != "approve: expected boolean, got string" {
		t.Errorf("StartSession(approve: yes) Message = %q, want the approve type fault", agentErr.Message)
	}
	if session.Internal != nil {
		t.Errorf("StartSession(approve: yes) session.Internal = %v, want no session", session.Internal)
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

func TestValidateConfig_WrongApproveType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		approve     any
		wantMessage string
	}{
		{name: "string yes", approve: "yes", wantMessage: "approve: expected boolean, got string"},
		{name: "string true", approve: "true", wantMessage: "approve: expected boolean, got string"},
		{name: "integer", approve: 1, wantMessage: "approve: expected boolean, got integer"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			diags := validateConfig(registry.AgentConfigFields{Kind: "pi", Passthrough: map[string]any{"approve": tt.approve}})
			if len(diags) != 1 {
				t.Fatalf("validateConfig(approve: %v) = %+v, want one diagnostic", tt.approve, diags)
			}
			if diags[0].Check != "pi.approve.wrong_type" || diags[0].Severity != "error" || diags[0].Message != tt.wantMessage {
				t.Errorf("validateConfig(approve: %v) = %+v, want an error pi.approve.wrong_type with message %q", tt.approve, diags[0], tt.wantMessage)
			}
		})
	}

	for _, approve := range []any{nil, true, false} {
		if diags := validateConfig(registry.AgentConfigFields{Kind: "pi", Passthrough: map[string]any{"approve": approve}}); len(diags) != 0 {
			t.Errorf("validateConfig(approve: %v) = %+v, want no diagnostics", approve, diags)
		}
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
