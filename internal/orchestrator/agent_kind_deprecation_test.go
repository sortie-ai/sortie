package orchestrator

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/registry"
)

func fixtureDeprecationLookup(kind string) (registry.AgentMeta, bool) {
	switch kind {
	case "deprecated-fixture":
		return registry.AgentMeta{Deprecation: &registry.AgentDeprecation{Replacement: "agent-client-protocol"}}, true
	case "agent-client-protocol":
		return registry.AgentMeta{}, true
	default:
		return registry.AgentMeta{}, false
	}
}

// TestAgentKindDeprecations_DeprecatedKindDrawsOneAdvisory reaches the
// same deprecated kind through agent.kind, the dispatch default, and a
// dispatch rule at once, so a passing result also proves
// orderedUniqueAgentKinds's dedup collapses the three reaches into one
// advisory.
func TestAgentKindDeprecations_DeprecatedKindDrawsOneAdvisory(t *testing.T) {
	t.Parallel()

	cfg := config.ServiceConfig{
		Agent: config.AgentConfig{Kind: "deprecated-fixture"},
		Dispatch: config.DispatchConfig{
			Default: config.DispatchSelection{AgentKind: "deprecated-fixture"},
			Rules: []config.DispatchRule{
				{Name: "bug-rule", Selection: config.DispatchSelection{AgentKind: "deprecated-fixture"}},
			},
		},
	}

	got := AgentKindDeprecations(cfg, fixtureDeprecationLookup)
	if len(got) != 1 {
		t.Fatalf("AgentKindDeprecations(deprecated-fixture named by agent.kind, dispatch default, and a rule) = %+v, want exactly 1 advisory", got)
	}

	advisory := got[0]
	if advisory.Check != "agent.kind.deprecated" {
		t.Errorf("Advisory.Check = %q, want %q", advisory.Check, "agent.kind.deprecated")
	}
	wantText := `agent kind "deprecated-fixture" is deprecated and will be removed in a later release; use agent kind "agent-client-protocol" instead`
	if advisory.Text != wantText {
		t.Errorf("Advisory.Text = %q, want %q", advisory.Text, wantText)
	}
	if advisory.Message != "agent kind is deprecated and will be removed in a later release" {
		t.Errorf("Advisory.Message = %q, want %q", advisory.Message, "agent kind is deprecated and will be removed in a later release")
	}
	if len(advisory.Attrs) != 2 ||
		advisory.Attrs[0].Key != "agent_kind" || advisory.Attrs[0].Value.String() != "deprecated-fixture" ||
		advisory.Attrs[1].Key != "replacement_kind" || advisory.Attrs[1].Value.String() != "agent-client-protocol" {
		t.Errorf("Advisory.Attrs = %v, want [agent_kind=deprecated-fixture replacement_kind=agent-client-protocol]", advisory.Attrs)
	}
}
