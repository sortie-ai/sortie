package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// usageCompletenessAgenttestImportPath is the import path a kind's
// test file must import to call the shared usage-reporting
// conformance assertion.
const usageCompletenessAgenttestImportPath = "github.com/sortie-ai/sortie/internal/agent/agenttest"

// usageCompletenessAssertFuncName is the exported function name a
// kind's test files must call at least once.
const usageCompletenessAssertFuncName = "AssertUsageReporting"

// fileCallsAssertUsageReporting reports whether file contains a call
// whose selector resolves to importAlias.AssertUsageReporting, where
// importAlias is file's own local binding for
// usageCompletenessAgenttestImportPath.
func fileCallsAssertUsageReporting(file *ast.File) bool {
	alias := resolveTestImportName(file, usageCompletenessAgenttestImportPath)
	if alias == "" {
		return false
	}

	found := false
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || !isRunnableTestFunc(fn) {
			continue
		}
		if found {
			break
		}
		inspectForUsageAssertCall(fn.Body, alias, &found)
	}
	return found
}

// inspectForUsageAssertCall sets *found when body calls the
// conformance assertion through alias. Only a test function's own
// body is walked: a call sitting in a helper nothing runs would
// otherwise count as coverage, which is the failure this check exists
// to catch.
func inspectForUsageAssertCall(body *ast.BlockStmt, alias string, found *bool) {
	ast.Inspect(body, func(n ast.Node) bool {
		if *found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != usageCompletenessAssertFuncName {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok || ident.Name != alias {
			return true
		}
		*found = true
		return false
	})
}

// kindsMissingUsageReportingCoverage reports, in the order kinds are
// given, every kind whose registration call cannot be found under
// agentRoot, or whose package directory's _test.go files carry no
// call resolving to agenttest.AssertUsageReporting. A kind present in
// kinds and covered by such a call is omitted from the result.
func kindsMissingUsageReportingCoverage(kinds []string, agentRoot string) []string {
	fset := token.NewFileSet()

	kindDirs, err := discoverKindDirectories(fset, agentRoot)
	if err != nil {
		return append([]string(nil), kinds...)
	}

	var missing []string
	for _, kind := range kinds {
		dir, ok := kindDirs[kind]
		if !ok {
			missing = append(missing, kind)
			continue
		}

		testFiles, globErr := filepath.Glob(filepath.Join(dir, "*_test.go"))
		if globErr != nil || len(testFiles) == 0 {
			missing = append(missing, kind)
			continue
		}

		covered := false
		for _, path := range testFiles {
			file, parseErr := parser.ParseFile(fset, path, nil, 0)
			if parseErr != nil {
				continue
			}
			if fileCallsAssertUsageReporting(file) {
				covered = true
				break
			}
		}
		if !covered {
			missing = append(missing, kind)
		}
	}
	return missing
}

// TestEveryAgentKindHasUsageReportingCoverage enumerates every agent
// kind registry.Agents held once main's own blank imports had run,
// and fails by name when a registered kind has no test file calling
// agenttest.AssertUsageReporting against its own package. This closes
// the hole a hand-maintained list of "the six known kinds" would
// reopen on a seventh adapter: a kind that declares a usage
// disposition but never checked it against its real event stream.
func TestEveryAgentKindHasUsageReportingCoverage(t *testing.T) {
	t.Parallel()

	if len(mcpCompletenessRegisteredKinds) == 0 {
		t.Fatal("registry.Agents.Kinds() returned no kinds, want at least the built-in adapters main.go blank-imports")
	}

	missing := kindsMissingUsageReportingCoverage(mcpCompletenessRegisteredKinds, mcpCompletenessAgentRoot)
	if len(missing) != 0 {
		t.Errorf("agent kind(s) %v have no test file calling agenttest.AssertUsageReporting against their own package", missing)
	}
}

// fixtureUsageCoveredKindRegister registers "covered-kind" from a
// directory named "covered-dir", deliberately not matching the kind
// string, mirroring claude-code/claude and copilot-cli/copilot.
const fixtureUsageCoveredKindRegister = `package covereddir

import "github.com/sortie-ai/sortie/internal/registry"

func init() {
	registry.Agents.Register("covered-kind", newCoveredAdapter)
}
`

const fixtureUsageCoveredKindTest = `package covereddir

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
)

func TestFixtureConformance(t *testing.T) {
	agenttest.AssertUsageReporting(t, "covered-kind", nil)
}
`

// fixtureUsageUncoveredKindRegister registers "uncovered-kind" from a
// directory whose test files never call AssertUsageReporting.
const fixtureUsageUncoveredKindRegister = `package uncovereddir

import "github.com/sortie-ai/sortie/internal/registry"

func init() {
	registry.Agents.RegisterWithMeta("uncovered-kind", newUncoveredAdapter, registry.AgentMeta{})
}
`

const fixtureUsageUncoveredKindTest = `package uncovereddir

import "testing"

func TestFixtureSomethingElse(t *testing.T) {
	_ = t
}
`

// fixtureUsageHelperOnlyKindRegister registers "helper-only-kind" from
// a directory whose test file calls AssertUsageReporting only from a
// helper no test runs.
const fixtureUsageHelperOnlyKindRegister = `package helperonlydir

import "github.com/sortie-ai/sortie/internal/registry"

func init() {
	registry.Agents.RegisterWithMeta("helper-only-kind", newHelperOnlyAdapter, registry.AgentMeta{})
}
`

const fixtureUsageHelperOnlyKindTest = `package helperonlydir

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
)

func assertNothingRunsThis(t *testing.T) {
	agenttest.AssertUsageReporting(t, "helper-only-kind", nil)
}

func Testlowercase(t *testing.T) {
	agenttest.AssertUsageReporting(t, "helper-only-kind", nil)
}

func TestWrongSignature(b *testing.B) {
	agenttest.AssertUsageReporting(b, "helper-only-kind", nil)
}

func TestFixtureUnrelated(t *testing.T) {
	_ = t
}
`

// TestKindsMissingUsageReportingCoverage proves the completeness
// mechanism itself can fail: a synthetic kind registered under a
// directory name that does not match its kind string, but whose test
// file calls AssertUsageReporting, is not reported; a kind whose test
// file never calls it is reported by name; a kind whose only calls
// sit in declarations the test binary never runs -- a helper, a
// lower-case suffix, a non-T signature -- is reported too, since an
// unreachable assertion is not coverage; and a kind with no
// registration anywhere under the fixture root is reported by name.
func TestKindsMissingUsageReportingCoverage(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "covered-dir"), "register.go", fixtureUsageCoveredKindRegister)
	writeFixtureFile(t, filepath.Join(root, "covered-dir"), "register_test.go", fixtureUsageCoveredKindTest)
	writeFixtureFile(t, filepath.Join(root, "uncovered-dir"), "register.go", fixtureUsageUncoveredKindRegister)
	writeFixtureFile(t, filepath.Join(root, "uncovered-dir"), "register_test.go", fixtureUsageUncoveredKindTest)
	writeFixtureFile(t, filepath.Join(root, "helper-only-dir"), "register.go", fixtureUsageHelperOnlyKindRegister)
	writeFixtureFile(t, filepath.Join(root, "helper-only-dir"), "register_test.go", fixtureUsageHelperOnlyKindTest)
	// "missing-kind" is registered nowhere under root.

	kinds := []string{"covered-kind", "uncovered-kind", "helper-only-kind", "missing-kind"}
	got := kindsMissingUsageReportingCoverage(kinds, root)

	want := []string{"uncovered-kind", "helper-only-kind", "missing-kind"}
	if len(got) != len(want) {
		t.Fatalf("kindsMissingUsageReportingCoverage(%v, %q) = %v, want %v", kinds, root, got, want)
	}
	for i, k := range want {
		if got[i] != k {
			t.Errorf("kindsMissingUsageReportingCoverage(%v, %q)[%d] = %q, want %q", kinds, root, i, got[i], k)
		}
	}
}
