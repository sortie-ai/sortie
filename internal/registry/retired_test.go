package registry_test

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/registry"
)

const (
	declaredRetiredKind   = "fixture-retired-declared"
	undeclaredRetiredKind = "fixture-retired-undeclared"
	fixtureReplacement    = "fixture-replacement"
)

// A declaration under a live kind's name needs a live kind; the blank adapter
// imports elsewhere in this package's tests guarantee the binary holds some.
var shadowedLiveKind = registry.Agents.Kinds()[0]

func init() {
	registry.RetiredAgents.Register(declaredRetiredKind, fixtureDeclaration())
	registry.RetiredAgents.Register(shadowedLiveKind, fixtureDeclaration())
}

func fixtureDeclaration() registry.RetiredAgent {
	return registry.RetiredAgent{
		Replacement:   fixtureReplacement,
		CredentialEnv: registry.DeclareCredentialEnv("FIXTURE_KEY"),
		Convert: func(registry.AgentConversionInput) (registry.AgentConversion, *registry.AgentConversionFault) {
			return registry.AgentConversion{}, nil
		},
	}
}

func TestRetiredAgentOf(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		kind            string
		wantOK          bool
		wantReplacement string
	}{
		{name: "declared kind", kind: declaredRetiredKind, wantOK: true, wantReplacement: fixtureReplacement},
		{name: "undeclared kind", kind: undeclaredRetiredKind},
		{name: "empty kind", kind: ""},
		{name: "kind registered both ways runs as the live kind", kind: shadowedLiveKind},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := registry.RetiredAgentOf(tt.kind)

			if ok != tt.wantOK {
				t.Fatalf("RetiredAgentOf(%q) ok = %v, want %v", tt.kind, ok, tt.wantOK)
			}
			if got.Replacement != tt.wantReplacement {
				t.Errorf("RetiredAgentOf(%q).Replacement = %q, want %q", tt.kind, got.Replacement, tt.wantReplacement)
			}
		})
	}
}

func TestRetiredAgentOf_ShadowedKindIsDeclaredAndLive(t *testing.T) {
	t.Parallel()

	if !registry.Agents.Has(shadowedLiveKind) {
		t.Fatalf("Agents.Has(%q) = false, want true", shadowedLiveKind)
	}
	if _, err := registry.RetiredAgents.Get(shadowedLiveKind); err != nil {
		t.Fatalf("RetiredAgents.Get(%q) = %v, want the declaration", shadowedLiveKind, err)
	}
}
