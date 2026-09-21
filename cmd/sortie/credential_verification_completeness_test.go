package main

import (
	"path/filepath"
	"slices"
	"testing"
)

const credentialCompletenessImportPath = "github.com/sortie-ai/sortie/internal/agent/agenttest/credentialtest"

const credentialCompletenessAssertFuncName = "AssertCredentialVerification"

func TestEveryAgentKindHasCredentialVerificationCoverage(t *testing.T) {
	t.Parallel()

	if len(mcpCompletenessRegisteredKinds) == 0 {
		t.Fatal("registry.Agents.Kinds() returned no kinds, want at least the built-in adapters main.go blank-imports")
	}

	missing := kindsMissingCoverage(mcpCompletenessRegisteredKinds, mcpCompletenessAgentRoot, credentialCompletenessImportPath, credentialCompletenessAssertFuncName)
	if len(missing) != 0 {
		t.Errorf("agent kind(s) %v have no test file calling credentialtest.AssertCredentialVerification against their own package", missing)
	}
}

func TestKindsMissingCredentialVerificationCoverage(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "covered-dir"), "register.go", `package covereddir

import "github.com/sortie-ai/sortie/internal/registry"

func init() {
	registry.Agents.Register("credential-covered-kind", newAdapter)
}
`)
	writeFixtureFile(t, filepath.Join(root, "covered-dir"), "register_test.go", `package covereddir

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest/credentialtest"
)

func TestFixtureCredentialConformance(t *testing.T) {
	credentialtest.AssertCredentialVerification(t, "credential-covered-kind", nil)
}
`)
	writeFixtureFile(t, filepath.Join(root, "uncovered-dir"), "register.go", `package uncovereddir

import "github.com/sortie-ai/sortie/internal/registry"

func init() {
	registry.Agents.Register("credential-uncovered-kind", newAdapter)
}
`)
	writeFixtureFile(t, filepath.Join(root, "uncovered-dir"), "register_test.go", `package uncovereddir

import "testing"

func TestFixtureSomethingElse(t *testing.T) {}
`)

	kinds := []string{"credential-covered-kind", "credential-uncovered-kind", "credential-missing-kind"}
	got := kindsMissingCoverage(kinds, root, credentialCompletenessImportPath, credentialCompletenessAssertFuncName)

	want := []string{"credential-uncovered-kind", "credential-missing-kind"}
	if !slices.Equal(got, want) {
		t.Errorf("kindsMissingCoverage(%v, %q) = %v, want %v", kinds, root, got, want)
	}
}
