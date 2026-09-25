package main

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/registry"
)

// earlyExitAssertFuncName is the exported function name a kind
// requiring an agent command's test files must call at least once.
const earlyExitAssertFuncName = "AssertEarlyExitReport"

// earlyExitKindsRequiringCommand returns the subset of kinds whose
// registry.AgentMeta, resolved through lookup, reports RequiresCommand
// true: a kind that requires an agent command launches a runtime and
// so can exit before it responds. The production call site passes
// registry.Agents.Meta; a fixture test substitutes its own lookup.
func earlyExitKindsRequiringCommand(kinds []string, lookup func(kind string) (registry.AgentMeta, bool)) []string {
	var requiring []string
	for _, kind := range kinds {
		meta, ok := lookup(kind)
		if ok && meta.RequiresCommand {
			requiring = append(requiring, kind)
		}
	}
	return requiring
}

// TestEveryEarlyExitKindHasCoverage enumerates every registered agent
// kind whose registry.AgentMeta reports RequiresCommand true, and
// fails by name when such a kind has no test file calling
// credentialtest.AssertEarlyExitReport against its own package. It
// also fails when it finds no such kind, so a registry rewrite that
// drops every command-requiring kind cannot silence this test by
// vacuity.
func TestEveryEarlyExitKindHasCoverage(t *testing.T) {
	t.Parallel()

	kinds := earlyExitKindsRequiringCommand(mcpCompletenessRegisteredKinds, registry.Agents.Meta)
	if len(kinds) == 0 {
		t.Fatal("no registered agent kind requires an agent command, want at least one (claude-code, copilot-cli, kiro, opencode, codex, agent-client-protocol)")
	}

	missing := kindsMissingCoverage(kinds, mcpCompletenessAgentRoot, credentialCompletenessImportPath, earlyExitAssertFuncName)
	if len(missing) != 0 {
		t.Errorf("agent kind(s) %v require an agent command but have no test file calling credentialtest.AssertEarlyExitReport against their own package", missing)
	}
}

// TestKindsMissingEarlyExitCoverage proves earlyExitKindsRequiringCommand
// and kindsMissingCoverage together report the right verdict for three
// shapes: a command-requiring kind with the assertion, one without it,
// and a kind that requires no command at all, which must be excluded
// from the filtered set the same way mock is excluded in production.
func TestKindsMissingEarlyExitCoverage(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "earlyexit-covered-dir"), "register.go", `package earlyexitcovereddir

import "github.com/sortie-ai/sortie/internal/registry"

func init() {
	registry.Agents.RegisterWithMeta("earlyexit-covered-kind", newAdapter, registry.AgentMeta{RequiresCommand: true})
}
`)
	writeFixtureFile(t, filepath.Join(root, "earlyexit-covered-dir"), "register_test.go", `package earlyexitcovereddir

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest/credentialtest"
)

func TestFixtureEarlyExitConformance(t *testing.T) {
	credentialtest.AssertEarlyExitReport(t, "earlyexit-covered-kind", nil, config)
}
`)
	writeFixtureFile(t, filepath.Join(root, "earlyexit-uncovered-dir"), "register.go", `package earlyexituncovereddir

import "github.com/sortie-ai/sortie/internal/registry"

func init() {
	registry.Agents.RegisterWithMeta("earlyexit-uncovered-kind", newAdapter, registry.AgentMeta{RequiresCommand: true})
}
`)
	writeFixtureFile(t, filepath.Join(root, "earlyexit-uncovered-dir"), "register_test.go", `package earlyexituncovereddir

import "testing"

func TestFixtureSomethingElse(t *testing.T) {}
`)

	kinds := []string{"earlyexit-covered-kind", "earlyexit-uncovered-kind", "earlyexit-no-command-kind"}
	lookup := func(kind string) (registry.AgentMeta, bool) {
		switch kind {
		case "earlyexit-covered-kind", "earlyexit-uncovered-kind":
			return registry.AgentMeta{RequiresCommand: true}, true
		case "earlyexit-no-command-kind":
			return registry.AgentMeta{RequiresCommand: false}, true
		default:
			return registry.AgentMeta{}, false
		}
	}

	requiring := earlyExitKindsRequiringCommand(kinds, lookup)
	wantRequiring := []string{"earlyexit-covered-kind", "earlyexit-uncovered-kind"}
	if !slices.Equal(requiring, wantRequiring) {
		t.Fatalf("earlyExitKindsRequiringCommand(%v) = %v, want %v", kinds, requiring, wantRequiring)
	}

	missing := kindsMissingCoverage(requiring, root, credentialCompletenessImportPath, earlyExitAssertFuncName)
	want := []string{"earlyexit-uncovered-kind"}
	if !slices.Equal(missing, want) {
		t.Errorf("kindsMissingCoverage(%v, %q) = %v, want %v", requiring, root, missing, want)
	}
}
