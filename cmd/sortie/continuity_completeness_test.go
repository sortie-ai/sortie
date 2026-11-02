package main

import "testing"

const continuityCompletenessAssertFuncName = "AssertSessionContinuity"

func TestEveryAgentKindHasSessionContinuityCoverage(t *testing.T) {
	t.Parallel()

	if len(mcpCompletenessRegisteredKinds) == 0 {
		t.Fatal("registry.Agents.Kinds() returned no kinds, want at least the built-in adapters main.go blank-imports")
	}

	missing := kindsMissingCoverage(mcpCompletenessRegisteredKinds, mcpCompletenessAgentRoot, mcpCompletenessAgenttestImportPath, continuityCompletenessAssertFuncName)
	if len(missing) != 0 {
		t.Errorf("agent kind(s) %v have no test file calling agenttest.AssertSessionContinuity against their own package", missing)
	}
}
