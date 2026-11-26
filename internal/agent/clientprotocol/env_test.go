package clientprotocol

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

const secretLookingValue = "s3cr3t-token-value"

func TestEnvSetting(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		env       any
		want      []sshutil.EnvVar
		wantCheck string
	}{
		{"null", nil, nil, ""},
		{"ascending names, empty value kept", map[string]any{"B": "2", "A": ""}, []sshutil.EnvVar{{Name: "A"}, {Name: "B", Value: "2"}}, ""},
		{"not a mapping", secretLookingValue, nil, "agent-client-protocol.env.wrong_type"},
		{"a non-string value", map[string]any{"TOKEN": []any{secretLookingValue}}, nil, "agent-client-protocol.env.wrong_type"},
		{"an invalid name", map[string]any{"1BAD": secretLookingValue}, nil, "agent-client-protocol.env.invalid_name"},
		{"a reserved name", map[string]any{"_sortie_complete": secretLookingValue}, nil, "agent-client-protocol.env.invalid_name"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, fault := envSetting(map[string]any{"env": tt.env})

			if tt.wantCheck != "" {
				if fault == nil || fault.check != tt.wantCheck || strings.Contains(fault.Error(), secretLookingValue) {
					t.Errorf("envSetting(%v) fault = %v, want check %q and no value in the message", tt.env, fault, tt.wantCheck)
				}
				return
			}
			if fault != nil || !slices.Equal(got, tt.want) {
				t.Errorf("envSetting(%v) = %v, %v, want %v, nil", tt.env, got, fault, tt.want)
			}
		})
	}
}

func TestPermissionsSetting(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		value     any
		want      agentcore.PermissionDeclaration
		wantCheck string
	}{
		{"null refuses", nil, agentcore.DeclaredRefusal, ""},
		{"deny-all refuses", "deny-all", agentcore.DeclaredRefusal, ""},
		{"approve-all approves", "approve-all", agentcore.DeclaredApproval, ""},
		{"not a string", true, agentcore.DeclaredRefusal, "agent-client-protocol.permissions.wrong_type"},
		{"an unknown string", "approve", agentcore.DeclaredRefusal, "agent-client-protocol.permissions.invalid_value"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, fault := permissionsSetting(map[string]any{"permissions": tt.value})

			if got != tt.want || (fault == nil) != (tt.wantCheck == "") || (fault != nil && fault.check != tt.wantCheck) {
				t.Errorf("permissionsSetting(%v) = %v, %v, want %v, check %q", tt.value, got, fault, tt.want, tt.wantCheck)
			}
		})
	}
}

func TestSettingFaultsReachValidateAndSessionStart(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		settings map[string]any
		check    string
	}{
		{"env", map[string]any{"env": map[string]any{"1BAD": "x"}}, "agent-client-protocol.env.invalid_name"},
		{"permissions", map[string]any{"permissions": "approve"}, "agent-client-protocol.permissions.invalid_value"},
		{"mcp_config", map[string]any{mcpConfigKey: 123}, "agent-client-protocol.mcp_config.wrong_type"},
		{"effort", map[string]any{registry.EffortKey: 7}, "agent-client-protocol.effort.wrong_type"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			adapter, _ := NewClientProtocolAdapter()
			_, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
				WorkspacePath: t.TempDir(),
				AgentConfig:   domain.AgentConfig{Command: "sortie-never-launched"},
				Settings:      tt.settings,
			})
			diags := validateConfig(registry.AgentConfigFields{Kind: "agent-client-protocol", Passthrough: tt.settings})

			agentErr, ok := errors.AsType[*domain.AgentError](err)
			if !ok || agentErr.Kind != domain.ErrAgentNotFound {
				t.Fatalf("StartSession(%v) error = %v, want a *domain.AgentError of kind %q", tt.settings, err, domain.ErrAgentNotFound)
			}
			want := registry.ValidationDiag{Severity: "error", Check: tt.check, Message: agentErr.Message}
			if len(diags) != 1 || diags[0] != want {
				t.Errorf("validateConfig(%v) = %+v, want [%+v]", tt.settings, diags, want)
			}
		})
	}
}
