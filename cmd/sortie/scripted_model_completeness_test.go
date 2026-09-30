package main

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/registry"
)

const (
	scriptedModelImportPath     = "github.com/sortie-ai/sortie/internal/agent/agenttest/fakemodel"
	scriptedModelAssertFuncName = "AssertConformance"
)

func TestEveryCommandKindHasScriptedModelCoverage(t *testing.T) {
	t.Parallel()

	kinds := earlyExitKindsRequiringCommand(mcpCompletenessRegisteredKinds, registry.Agents.Meta)
	if len(kinds) == 0 {
		t.Fatal("no registered agent kind requires an agent command, want at least one (claude-code, copilot-cli, opencode, codex, agent-client-protocol)")
	}

	missing := kindsMissingCoverage(kinds, mcpCompletenessAgentRoot, scriptedModelImportPath, scriptedModelAssertFuncName)
	if len(missing) != 0 {
		t.Errorf("agent kind(s) %v require an agent command but have no test file calling fakemodel.AssertConformance against their own package", missing)
	}
}

func TestKindsMissingScriptedModelCoverage(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "scripted-covered-dir"), "register.go", `package scriptedcovereddir

import "github.com/sortie-ai/sortie/internal/registry"

func init() {
	registry.Agents.RegisterWithMeta("scripted-covered-kind", newAdapter, registry.AgentMeta{RequiresCommand: true})
}
`)
	writeFixtureFile(t, filepath.Join(root, "scripted-covered-dir"), "integration_test.go", `package scriptedcovereddir

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest/fakemodel"
)

func TestIntegration_ScriptedModel(t *testing.T) {
	fakemodel.AssertConformance(t, fakemodel.Binding{})
}
`)
	writeFixtureFile(t, filepath.Join(root, "scripted-uncovered-dir"), "register.go", `package scripteduncovereddir

import "github.com/sortie-ai/sortie/internal/registry"

func init() {
	registry.Agents.RegisterWithMeta("scripted-uncovered-kind", newAdapter, registry.AgentMeta{RequiresCommand: true})
}
`)
	writeFixtureFile(t, filepath.Join(root, "scripted-uncovered-dir"), "integration_test.go", `package scripteduncovereddir

import "testing"

func TestIntegration_SomethingElse(t *testing.T) {}
`)

	kinds := []string{"scripted-covered-kind", "scripted-uncovered-kind", "scripted-missing-kind"}

	missing := kindsMissingCoverage(kinds, root, scriptedModelImportPath, scriptedModelAssertFuncName)

	want := []string{"scripted-uncovered-kind", "scripted-missing-kind"}
	if !slices.Equal(missing, want) {
		t.Errorf("kindsMissingCoverage(%v, %q) = %v, want %v", kinds, root, missing, want)
	}
}
