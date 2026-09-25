package main

import (
	"fmt"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/redact"
	"github.com/sortie-ai/sortie/internal/registry"
)

// credentialEnvCompletenessRegisteredKinds snapshots registry.Agents.Kinds()
// at package initialization, once main's own blank imports have run their
// init() functions but before any test body executes, for the same reason
// mcpCompletenessRegisteredKinds does: reading the registry from inside a
// test function body would let this test observe a test-only kind a
// sibling test file registers into the same process-wide registry.Agents
// singleton from within its own test body, which main.go's blank imports
// never produce.
var credentialEnvCompletenessRegisteredKinds = registry.Agents.Kinds()

// credentialEnvIssue names one kind whose CredentialEnv declaration
// fails the completeness check, together with the reason.
type credentialEnvIssue struct {
	kind   string
	reason string
}

// checkCredentialEnvCoverage validates every kind in kinds against
// lookup, in the given order, and returns one credentialEnvIssue per
// kind that is not registered, whose CredentialEnv is undeclared, that
// declares a name failing sshutil.IsEnvName, that declares a name
// sshutil reserves for the SSH carrier, that declares a name
// redact.IsSecretName rejects, or that declares a name more than
// once.
func checkCredentialEnvCoverage(kinds []string, lookup func(kind string) (registry.AgentMeta, bool)) []credentialEnvIssue {
	var issues []credentialEnvIssue
	for _, kind := range kinds {
		meta, ok := lookup(kind)
		if !ok {
			issues = append(issues, credentialEnvIssue{kind: kind, reason: "is not registered"})
			continue
		}
		if !meta.CredentialEnv.Declared() {
			issues = append(issues, credentialEnvIssue{kind: kind, reason: "CredentialEnv is undeclared"})
			continue
		}

		seen := make(map[string]bool)
		for _, name := range meta.CredentialEnv.Names() {
			switch {
			case !sshutil.IsEnvName(name):
				issues = append(issues, credentialEnvIssue{kind: kind, reason: fmt.Sprintf("declares %q, which fails sshutil.IsEnvName", name)})
			case sshutil.IsReservedEnvName(name):
				issues = append(issues, credentialEnvIssue{kind: kind, reason: fmt.Sprintf("declares %q, which sshutil reserves", name)})
			case !redact.IsSecretName(name):
				issues = append(issues, credentialEnvIssue{kind: kind, reason: fmt.Sprintf("declares %q, which fails redact.IsSecretName", name)})
			case seen[name]:
				issues = append(issues, credentialEnvIssue{kind: kind, reason: fmt.Sprintf("declares %q more than once", name)})
			default:
				seen[name] = true
			}
		}
	}
	return issues
}

// TestEveryAgentKindHasCredentialEnvCoverage enumerates every agent
// kind registry.Agents held once main's own blank imports had run and
// fails, naming the kind, when checkCredentialEnvCoverage reports an
// issue for it. This closes the hole a hand-maintained list of "the
// seven known kinds" would reopen on an eighth adapter: a kind that is
// registered but declares no credential names, or declares one that is
// not a valid environment variable name.
func TestEveryAgentKindHasCredentialEnvCoverage(t *testing.T) {
	t.Parallel()

	if len(credentialEnvCompletenessRegisteredKinds) == 0 {
		t.Fatal("registry.Agents.Kinds() returned no kinds, want at least the built-in adapters main.go blank-imports")
	}

	issues := checkCredentialEnvCoverage(credentialEnvCompletenessRegisteredKinds, registry.Agents.Meta)
	for _, issue := range issues {
		t.Errorf("agent kind %q: %s", issue.kind, issue.reason)
	}
}

// TestCheckCredentialEnvCoverage_NegativeControl proves the
// completeness mechanism itself can fail: a fixture kind whose lookup
// returns the zero CredentialEnv is reported, one declaring a name
// that fails sshutil.IsEnvName is reported, one declaring a name
// sshutil reserves is reported, one declaring a name redact.IsSecretName
// rejects is reported, one declaring the same name twice is reported,
// and a kind declaring valid, non-repeating names is not.
func TestCheckCredentialEnvCoverage_NegativeControl(t *testing.T) {
	t.Parallel()

	lookup := func(kind string) (registry.AgentMeta, bool) {
		switch kind {
		case "declared-ok-fixture":
			return registry.AgentMeta{CredentialEnv: registry.DeclareCredentialEnv("GOOD_API_KEY")}, true
		case "undeclared-fixture":
			return registry.AgentMeta{}, true
		case "invalid-name-fixture":
			return registry.AgentMeta{CredentialEnv: registry.DeclareCredentialEnv("1BAD")}, true
		case "reserved-name-fixture":
			return registry.AgentMeta{CredentialEnv: registry.DeclareCredentialEnv("_sortie_complete")}, true
		case "not-a-secret-name-fixture":
			return registry.AgentMeta{CredentialEnv: registry.DeclareCredentialEnv("GOOD_NAME")}, true
		case "duplicate-name-fixture":
			return registry.AgentMeta{CredentialEnv: registry.DeclareCredentialEnv("DUP", "DUP")}, true
		default:
			return registry.AgentMeta{}, false
		}
	}

	kinds := []string{"declared-ok-fixture", "undeclared-fixture", "invalid-name-fixture", "reserved-name-fixture", "not-a-secret-name-fixture", "duplicate-name-fixture", "unregistered-fixture"}
	issues := checkCredentialEnvCoverage(kinds, lookup)

	byKind := make(map[string]bool, len(issues))
	for _, issue := range issues {
		byKind[issue.kind] = true
	}

	if byKind["declared-ok-fixture"] {
		t.Error("declared-ok-fixture reported an issue, want none: it declares one valid, non-repeating name")
	}
	for _, wantReported := range []string{"undeclared-fixture", "invalid-name-fixture", "reserved-name-fixture", "not-a-secret-name-fixture", "duplicate-name-fixture", "unregistered-fixture"} {
		if !byKind[wantReported] {
			t.Errorf("%s reported no issue, want one", wantReported)
		}
	}
}
