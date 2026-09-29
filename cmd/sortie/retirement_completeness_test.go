package main

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

var retirementCompletenessDeclaredKinds = registry.RetiredAgents.Kinds()

type retirementIssue struct {
	kind   string
	reason string
}

func checkRetirementCoverage(
	kinds []string,
	retired func(kind string) (registry.RetiredAgent, bool),
	live func(kind string) (registry.AgentMeta, bool),
) []retirementIssue {
	var issues []retirementIssue
	report := func(kind, format string, args ...any) {
		issues = append(issues, retirementIssue{kind: kind, reason: fmt.Sprintf(format, args...)})
	}

	for _, kind := range kinds {
		decl, ok := retired(kind)
		if !ok {
			continue
		}

		replacement := decl.Replacement
		switch replacementMeta, replacementLive := live(replacement); {
		case replacement == kind:
			report(kind, "names itself (%q) as its replacement", replacement)
		case !replacementLive:
			report(kind, "names replacement %q, which is not registered", replacement)
		case replacementMeta.Deprecation != nil:
			report(kind, "names replacement %q, which is deprecated", replacement)
		}
		if _, replacementRetired := retired(replacement); replacementRetired && replacement != kind {
			report(kind, "names replacement %q, which is itself retired", replacement)
		}

		if !decl.CredentialEnv.Declared() {
			report(kind, "declares no CredentialEnv")
		}

		if liveMeta, isLive := live(kind); isLive && (liveMeta.Deprecation == nil || liveMeta.Deprecation.Replacement != replacement) {
			report(kind, "is registered live without a Deprecation naming replacement %q", replacement)
		}

		if decl.Convert == nil {
			report(kind, "has a nil Convert")
			continue
		}
		issues = append(issues, checkEmptyInputConversion(kind, decl)...)
	}
	return issues
}

func checkEmptyInputConversion(kind string, decl registry.RetiredAgent) []retirementIssue {
	var issues []retirementIssue
	for _, remote := range []bool{false, true} {
		input := registry.AgentConversionInput{Remote: remote}
		first, fault := decl.Convert(input)
		if fault != nil {
			issues = append(issues, retirementIssue{kind: kind, reason: fmt.Sprintf("faults on the empty input (remote %v): %s", remote, fault.Message)})
			continue
		}
		if first.Command.IsZero() {
			issues = append(issues, retirementIssue{kind: kind, reason: fmt.Sprintf("converts the empty input to a zero command (remote %v)", remote)})
		}
		second, _ := decl.Convert(input)
		if !reflect.DeepEqual(first, second) {
			issues = append(issues, retirementIssue{kind: kind, reason: fmt.Sprintf("converts the empty input differently on a second call (remote %v)", remote)})
		}
	}
	return issues
}

func TestEveryRetiredKindDeclaresAValidConversion(t *testing.T) {
	t.Parallel()

	if len(retirementCompletenessDeclaredKinds) == 0 {
		t.Fatal("registry.RetiredAgents.Kinds() returned no kinds, want at least the declaration main.go's blank imports register")
	}

	retired := func(kind string) (registry.RetiredAgent, bool) {
		decl, err := registry.RetiredAgents.Get(kind)
		return decl, err == nil
	}
	for _, issue := range checkRetirementCoverage(retirementCompletenessDeclaredKinds, retired, registry.Agents.Meta) {
		t.Errorf("retired agent kind %q: %s", issue.kind, issue.reason)
	}
}

func TestCheckRetirementCoverage_NegativeControl(t *testing.T) {
	t.Parallel()

	convert := func(in registry.AgentConversionInput) (registry.AgentConversion, *registry.AgentConversionFault) {
		return registry.AgentConversion{Command: domain.AgentCommand{Line: "fixture-cli"}}, nil
	}
	declared := registry.DeclareCredentialEnv()
	good := registry.RetiredAgent{Replacement: "target-fixture", CredentialEnv: declared, Convert: convert}
	calls := 0

	declarations := map[string]registry.RetiredAgent{
		"good-fixture":                     good,
		"good-live-fixture":                good,
		"unregistered-target-fixture":      {Replacement: "missing-fixture", CredentialEnv: declared, Convert: convert},
		"deprecated-target-fixture":        {Replacement: "deprecated-fixture", CredentialEnv: declared, Convert: convert},
		"retired-target-fixture":           {Replacement: "other-retired-fixture", CredentialEnv: declared, Convert: convert},
		"other-retired-fixture":            good,
		"self-target-fixture":              {Replacement: "self-target-fixture", CredentialEnv: declared, Convert: convert},
		"nil-convert-fixture":              {Replacement: "target-fixture", CredentialEnv: declared},
		"undeclared-credential-fixture":    {Replacement: "target-fixture", Convert: convert},
		"live-without-deprecation-fixture": good,
		"live-other-deprecation-fixture":   good,
		"faulting-convert-fixture": {Replacement: "target-fixture", CredentialEnv: declared, Convert: func(registry.AgentConversionInput) (registry.AgentConversion, *registry.AgentConversionFault) {
			return registry.AgentConversion{}, &registry.AgentConversionFault{Key: "k", Message: "refused"}
		}},
		"zero-command-fixture": {Replacement: "target-fixture", CredentialEnv: declared, Convert: func(registry.AgentConversionInput) (registry.AgentConversion, *registry.AgentConversionFault) {
			return registry.AgentConversion{}, nil
		}},
		"nondeterministic-fixture": {Replacement: "target-fixture", CredentialEnv: declared, Convert: func(registry.AgentConversionInput) (registry.AgentConversion, *registry.AgentConversionFault) {
			calls++
			return registry.AgentConversion{Command: domain.AgentCommand{Line: fmt.Sprintf("fixture-cli-%d", calls)}}, nil
		}},
	}
	retired := func(kind string) (registry.RetiredAgent, bool) {
		decl, ok := declarations[kind]
		return decl, ok
	}
	live := func(kind string) (registry.AgentMeta, bool) {
		switch kind {
		case "target-fixture", "other-retired-fixture":
			return registry.AgentMeta{}, true
		case "deprecated-fixture":
			return registry.AgentMeta{Deprecation: &registry.AgentDeprecation{Replacement: "target-fixture"}}, true
		case "good-live-fixture":
			return registry.AgentMeta{Deprecation: &registry.AgentDeprecation{Replacement: "target-fixture"}}, true
		case "live-without-deprecation-fixture":
			return registry.AgentMeta{}, true
		case "live-other-deprecation-fixture":
			return registry.AgentMeta{Deprecation: &registry.AgentDeprecation{Replacement: "elsewhere-fixture"}}, true
		}
		return registry.AgentMeta{}, false
	}

	tests := []struct {
		kind          string
		wantReasonSub string
	}{
		{kind: "good-fixture"},
		{kind: "good-live-fixture"},
		{kind: "unregistered-target-fixture", wantReasonSub: "not registered"},
		{kind: "deprecated-target-fixture", wantReasonSub: "is deprecated"},
		{kind: "retired-target-fixture", wantReasonSub: "itself retired"},
		{kind: "self-target-fixture", wantReasonSub: "names itself"},
		{kind: "nil-convert-fixture", wantReasonSub: "nil Convert"},
		{kind: "undeclared-credential-fixture", wantReasonSub: "no CredentialEnv"},
		{kind: "live-without-deprecation-fixture", wantReasonSub: "live without a Deprecation"},
		{kind: "live-other-deprecation-fixture", wantReasonSub: "live without a Deprecation"},
		{kind: "faulting-convert-fixture", wantReasonSub: "faults on the empty input"},
		{kind: "zero-command-fixture", wantReasonSub: "zero command"},
		{kind: "nondeterministic-fixture", wantReasonSub: "differently on a second call"},
	}

	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			var reasons []string
			for _, issue := range checkRetirementCoverage([]string{tt.kind}, retired, live) {
				reasons = append(reasons, issue.reason)
			}

			joined := strings.Join(reasons, "; ")
			if tt.wantReasonSub == "" {
				if len(reasons) != 0 {
					t.Errorf("checkRetirementCoverage(%q) = %q, want no issue", tt.kind, joined)
				}
				return
			}
			if !strings.Contains(joined, tt.wantReasonSub) {
				t.Errorf("checkRetirementCoverage(%q) = %q, want an issue containing %q", tt.kind, joined, tt.wantReasonSub)
			}
		})
	}
}
