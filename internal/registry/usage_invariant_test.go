package registry_test

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/registry"

	// Trigger adapter init() registrations for every kind this
	// invariant walk checks.
	_ "github.com/sortie-ai/sortie/internal/agent/claude"
	_ "github.com/sortie-ai/sortie/internal/agent/clientprotocol"
	_ "github.com/sortie-ai/sortie/internal/agent/codex"
	_ "github.com/sortie-ai/sortie/internal/agent/copilot"
	_ "github.com/sortie-ai/sortie/internal/agent/kiro"
	_ "github.com/sortie-ai/sortie/internal/agent/mock"
	_ "github.com/sortie-ai/sortie/internal/agent/opencode"
)

// TestUsageDispositionInvariants walks every registered agent kind's
// declared pair and every entry in its UsageSessionRules, checking
// INV-1 (arrival is none if and only if attribution is none) and
// INV-3 (every value lies inside the declared set) for each.
func TestUsageDispositionInvariants(t *testing.T) {
	for _, kind := range registry.Agents.Kinds() {
		meta, ok := registry.Agents.Meta(kind)
		if !ok {
			t.Fatalf("Agents.Meta(%q) reported not registered immediately after Kinds() listed it", kind)
		}

		checkUsagePairInvariants(t, kind, -1, meta.UsageArrival, meta.UsageAttribution)
		for i, rule := range meta.UsageSessionRules {
			checkUsagePairInvariants(t, kind, i, rule.Arrival, rule.Attribution)
		}
	}
}

// checkUsagePairInvariants checks INV-1 and INV-3 for one pair, named
// by kind and ruleIndex (-1 for the declared pair, otherwise the
// UsageSessionRules index it came from).
func checkUsagePairInvariants(t *testing.T, kind string, ruleIndex int, arrival registry.UsageArrival, attribution registry.UsageAttribution) {
	t.Helper()

	switch arrival {
	case registry.UsageArrivalIncremental, registry.UsageArrivalTurnEnd, registry.UsageArrivalNone:
	default:
		t.Errorf("kind %q, rule %d: UsageArrival = %q, want a value inside the declared set", kind, ruleIndex, arrival)
	}

	switch attribution {
	case registry.UsageAttributionPerModel, registry.UsageAttributionSessionTotal, registry.UsageAttributionNone:
	default:
		t.Errorf("kind %q, rule %d: UsageAttribution = %q, want a value inside the declared set", kind, ruleIndex, attribution)
	}

	arrivalNone := arrival == registry.UsageArrivalNone
	attributionNone := attribution == registry.UsageAttributionNone
	if arrivalNone != attributionNone {
		t.Errorf("kind %q, rule %d: INV-1 violated: arrival = %q, attribution = %q", kind, ruleIndex, arrival, attribution)
	}
}
