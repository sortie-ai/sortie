package orchestrator

import (
	"fmt"
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/registry"
)

func fixtureEffortLookup(kind string) (registry.AgentMeta, bool) {
	switch kind {
	case "required-command-fixture":
		return registry.AgentMeta{RequiresCommand: true, EffortForwarding: registry.EffortNotForwarded}, true
	case "default-command-fixture":
		return registry.AgentMeta{DefaultCommand: "runtime", EffortForwarding: registry.EffortNotForwarded}, true
	case "no-agent-fixture":
		return registry.AgentMeta{EffortForwarding: registry.EffortNotForwarded}, true
	case "forwarding-fixture":
		return registry.AgentMeta{RequiresCommand: true, EffortForwarding: registry.EffortForwarded}, true
	case "undeclared-fixture":
		return registry.AgentMeta{RequiresCommand: true}, true
	default:
		return registry.AgentMeta{}, false
	}
}

func effortBlock(value any) map[string]any {
	return map[string]any{registry.EffortKey: value}
}

func effortConfigFor(agentKind, dispatchDefault string, ruleKinds []string, blocks map[string]map[string]any) config.ServiceConfig {
	cfg := config.ServiceConfig{
		Agent:    config.AgentConfig{Kind: agentKind},
		Dispatch: config.DispatchConfig{Default: config.DispatchSelection{AgentKind: dispatchDefault}},
	}
	for i, kind := range ruleKinds {
		cfg.Dispatch.Rules = append(cfg.Dispatch.Rules, config.DispatchRule{
			Name:      fmt.Sprintf("rule-%d", i),
			Selection: config.DispatchSelection{AgentKind: kind},
		})
	}
	for kind, block := range blocks {
		cfg.SetExtensionSection(kind, block)
	}
	return cfg
}

func advisoryKinds(advisories []config.Advisory) []string {
	kinds := make([]string, 0, len(advisories))
	for _, advisory := range advisories {
		if len(advisory.Attrs) == 1 && advisory.Attrs[0].Key == "agent_kind" {
			kinds = append(kinds, advisory.Attrs[0].Value.String())
			continue
		}
		kinds = append(kinds, fmt.Sprintf("<unexpected attrs %v>", advisory.Attrs))
	}
	return kinds
}

func TestAgentKindEffortAdvisories_Selection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		agentKind string
		dispatch  string
		rules     []string
		blocks    map[string]map[string]any
		want      []string
	}{
		{
			name:      "non-empty string draws one advisory",
			agentKind: "required-command-fixture",
			blocks:    map[string]map[string]any{"required-command-fixture": effortBlock("high")},
			want:      []string{"required-command-fixture"},
		},
		{
			name:      "non-string value draws one advisory",
			agentKind: "required-command-fixture",
			blocks:    map[string]map[string]any{"required-command-fixture": effortBlock(7)},
			want:      []string{"required-command-fixture"},
		},
		{
			name:      "empty string draws none",
			agentKind: "required-command-fixture",
			blocks:    map[string]map[string]any{"required-command-fixture": effortBlock("")},
		},
		{
			name:      "null value draws none",
			agentKind: "required-command-fixture",
			blocks:    map[string]map[string]any{"required-command-fixture": effortBlock(nil)},
		},
		{
			name:      "absent key draws none",
			agentKind: "required-command-fixture",
			blocks:    map[string]map[string]any{"required-command-fixture": {"mcp_config": "/ws/.sortie/mcp.json"}},
		},
		{
			name:      "block-less kind draws none",
			agentKind: "required-command-fixture",
		},
		{
			name:      "forwarding kind draws none",
			agentKind: "forwarding-fixture",
			blocks:    map[string]map[string]any{"forwarding-fixture": effortBlock("high")},
		},
		{
			name:      "undeclared kind draws none",
			agentKind: "undeclared-fixture",
			blocks:    map[string]map[string]any{"undeclared-fixture": effortBlock("high")},
		},
		{
			name:      "unregistered kind draws none",
			agentKind: "unregistered-fixture",
			blocks:    map[string]map[string]any{"unregistered-fixture": effortBlock("high")},
		},
		{
			name:      "kind no selector reaches draws none",
			agentKind: "forwarding-fixture",
			blocks:    map[string]map[string]any{"required-command-fixture": effortBlock("high")},
		},
		{
			name:      "kinds come in selector order across agent kind, dispatch default, and rules",
			agentKind: "no-agent-fixture",
			dispatch:  "required-command-fixture",
			rules:     []string{"forwarding-fixture", "default-command-fixture"},
			blocks: map[string]map[string]any{
				"required-command-fixture": effortBlock("high"),
				"forwarding-fixture":       effortBlock("high"),
				"default-command-fixture":  effortBlock("high"),
				"no-agent-fixture":         effortBlock("high"),
			},
			want: []string{"no-agent-fixture", "required-command-fixture", "default-command-fixture"},
		},
		{
			name:      "one kind reached three ways draws one advisory",
			agentKind: "required-command-fixture",
			dispatch:  "required-command-fixture",
			rules:     []string{"required-command-fixture"},
			blocks:    map[string]map[string]any{"required-command-fixture": effortBlock("high")},
			want:      []string{"required-command-fixture"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := effortConfigFor(tt.agentKind, tt.dispatch, tt.rules, tt.blocks)

			got := advisoryKinds(AgentKindEffortAdvisories(cfg, fixtureEffortLookup))

			if !slices.Equal(got, tt.want) {
				t.Errorf("AgentKindEffortAdvisories(...) kinds = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAgentKindEffortAdvisories_Fields(t *testing.T) {
	t.Parallel()

	const remedy = "passes no reasoning level to its agent; where the agent takes a reasoning option on its command line, write it in agent.command"

	tests := []struct {
		name string
		kind string
		want string
	}{
		{
			name: "kind requiring a command carries the agent.command remedy",
			kind: "required-command-fixture",
			want: fmt.Sprintf("required-command-fixture.%s has no effect: agent kind %q %s", registry.EffortKey, "required-command-fixture", remedy),
		},
		{
			name: "kind with a default command carries the agent.command remedy",
			kind: "default-command-fixture",
			want: fmt.Sprintf("default-command-fixture.%s has no effect: agent kind %q %s", registry.EffortKey, "default-command-fixture", remedy),
		},
		{
			name: "kind launching no agent says so",
			kind: "no-agent-fixture",
			want: fmt.Sprintf("no-agent-fixture.%s has no effect: agent kind %q launches no agent", registry.EffortKey, "no-agent-fixture"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := effortConfigFor(tt.kind, "", nil, map[string]map[string]any{tt.kind: effortBlock("high")})

			got := AgentKindEffortAdvisories(cfg, fixtureEffortLookup)

			if len(got) != 1 {
				t.Fatalf("AgentKindEffortAdvisories(%s) = %+v, want exactly 1 advisory", tt.kind, got)
			}
			advisory := got[0]
			if advisory.Check != "agent.effort.not_forwarded" {
				t.Errorf("Advisory.Check = %q, want %q", advisory.Check, "agent.effort.not_forwarded")
			}
			if advisory.Text != tt.want {
				t.Errorf("Advisory.Text = %q, want %q", advisory.Text, tt.want)
			}
			if advisory.Message != "effort setting has no effect for this agent kind" {
				t.Errorf("Advisory.Message = %q, want %q", advisory.Message, "effort setting has no effect for this agent kind")
			}
			if len(advisory.Attrs) != 1 || advisory.Attrs[0].Key != "agent_kind" || advisory.Attrs[0].Value.String() != tt.kind {
				t.Errorf("Advisory.Attrs = %v, want [agent_kind=%s]", advisory.Attrs, tt.kind)
			}
		})
	}
}
