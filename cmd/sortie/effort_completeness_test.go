package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/registry"
)

const effortCompletenessAssertFuncName = "AssertEffortForwarding"

type effortIssueArm string

const (
	effortArmUnregistered effortIssueArm = "unregistered"
	effortArmUndeclared   effortIssueArm = "undeclared"
	effortArmUncovered    effortIssueArm = "forwarded without a conformance test"
	effortArmReadsKey     effortIssueArm = "not forwarded but reads the key"
)

type effortIssue struct {
	kind string
	arm  effortIssueArm
}

func referencesEffortAPI(file *ast.File) bool {
	alias := resolveTestImportName(file, mcpCompletenessRegistryImportPath)
	if alias == "" {
		return false
	}

	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return !found
		}
		ident, ok := sel.X.(*ast.Ident)
		if ok && ident.Name == alias && (sel.Sel.Name == "EffortSetting" || sel.Sel.Name == "EffortKey") {
			found = true
		}
		return !found
	})
	return found
}

func packageReferencesEffortAPI(fset *token.FileSet, dir string) bool {
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return false
	}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			continue
		}
		if referencesEffortAPI(file) {
			return true
		}
	}
	return false
}

func checkEffortCoverage(kinds []string, agentRoot string, lookup func(kind string) (registry.AgentMeta, bool)) []effortIssue {
	fset := token.NewFileSet()
	kindDirs, err := discoverKindDirectories(fset, agentRoot)
	if err != nil {
		kindDirs = nil
	}

	var issues []effortIssue
	for _, kind := range kinds {
		meta, registered := lookup(kind)
		if !registered {
			issues = append(issues, effortIssue{kind: kind, arm: effortArmUnregistered})
			continue
		}
		switch meta.EffortForwarding {
		case registry.EffortForwarded:
			if len(kindsMissingCoverage([]string{kind}, agentRoot, mcpCompletenessAgenttestImportPath, effortCompletenessAssertFuncName)) != 0 {
				issues = append(issues, effortIssue{kind: kind, arm: effortArmUncovered})
			}
		case registry.EffortNotForwarded:
			if dir, ok := kindDirs[kind]; ok && packageReferencesEffortAPI(fset, dir) {
				issues = append(issues, effortIssue{kind: kind, arm: effortArmReadsKey})
			}
		default:
			issues = append(issues, effortIssue{kind: kind, arm: effortArmUndeclared})
		}
	}
	return issues
}

func TestEveryAgentKindDeclaresEffortForwarding(t *testing.T) {
	t.Parallel()

	if len(mcpCompletenessRegisteredKinds) == 0 {
		t.Fatal("registry.Agents.Kinds() returned no kinds, want at least the built-in adapters main.go blank-imports")
	}

	issues := checkEffortCoverage(mcpCompletenessRegisteredKinds, mcpCompletenessAgentRoot, registry.Agents.Meta)
	for _, issue := range issues {
		t.Errorf("agent kind %q: %s", issue.kind, issue.arm)
	}
}

func TestCheckEffortCoverage_NegativeControl(t *testing.T) {
	t.Parallel()

	registerSource := func(pkg, kind, extra string) string {
		return "package " + pkg + "\n\nimport \"github.com/sortie-ai/sortie/internal/registry\"\n\nfunc init() {\n\tregistry.Agents.Register(\"" + kind + "\", newAdapter)\n}\n" + extra
	}
	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "covered-dir"), "register.go", registerSource("covereddir", "forwarded-covered-fixture", ""))
	writeFixtureFile(t, filepath.Join(root, "covered-dir"), "register_test.go", `package covereddir

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
)

func TestFixtureEffortForwarding(t *testing.T) {
	agenttest.AssertEffortForwarding(t, nil)
}
`)
	writeFixtureFile(t, filepath.Join(root, "uncovered-dir"), "register.go", registerSource("uncovereddir", "forwarded-uncovered-fixture", ""))
	writeFixtureFile(t, filepath.Join(root, "uncovered-dir"), "register_test.go", "package uncovereddir\n\nimport \"testing\"\n\nfunc TestFixtureSomethingElse(t *testing.T) {}\n")
	writeFixtureFile(t, filepath.Join(root, "undeclared-dir"), "register.go", registerSource("undeclareddir", "undeclared-fixture", ""))
	writeFixtureFile(t, filepath.Join(root, "clean-dir"), "register.go", registerSource("cleandir", "not-forwarded-clean-fixture", ""))
	writeFixtureFile(t, filepath.Join(root, "reader-dir"), "register.go", registerSource("readerdir", "not-forwarded-reader-fixture", "\nvar _ = registry.EffortSetting\n"))
	writeFixtureFile(t, filepath.Join(root, "key-dir"), "register.go", registerSource("keydir", "not-forwarded-key-fixture", "\nvar _ = registry.EffortKey\n"))
	writeFixtureFile(t, filepath.Join(root, "test-only-dir"), "register.go", registerSource("testonlydir", "not-forwarded-test-only-fixture", ""))
	writeFixtureFile(t, filepath.Join(root, "test-only-dir"), "register_test.go", "package testonlydir\n\nimport (\n\t\"testing\"\n\n\t\"github.com/sortie-ai/sortie/internal/registry\"\n)\n\nfunc TestFixtureReadsKey(t *testing.T) {\n\t_ = registry.EffortKey\n}\n")

	declarations := map[string]registry.EffortForwarding{
		"forwarded-covered-fixture":       registry.EffortForwarded,
		"forwarded-uncovered-fixture":     registry.EffortForwarded,
		"undeclared-fixture":              registry.EffortForwardingUndeclared,
		"not-forwarded-clean-fixture":     registry.EffortNotForwarded,
		"not-forwarded-reader-fixture":    registry.EffortNotForwarded,
		"not-forwarded-key-fixture":       registry.EffortNotForwarded,
		"not-forwarded-test-only-fixture": registry.EffortNotForwarded,
	}
	lookup := func(kind string) (registry.AgentMeta, bool) {
		declared, ok := declarations[kind]
		return registry.AgentMeta{EffortForwarding: declared}, ok
	}

	kinds := []string{
		"forwarded-covered-fixture", "forwarded-uncovered-fixture", "undeclared-fixture",
		"not-forwarded-clean-fixture", "not-forwarded-reader-fixture", "not-forwarded-key-fixture",
		"not-forwarded-test-only-fixture", "unregistered-fixture",
	}

	got := checkEffortCoverage(kinds, root, lookup)

	want := []effortIssue{
		{kind: "forwarded-uncovered-fixture", arm: effortArmUncovered},
		{kind: "undeclared-fixture", arm: effortArmUndeclared},
		{kind: "not-forwarded-reader-fixture", arm: effortArmReadsKey},
		{kind: "not-forwarded-key-fixture", arm: effortArmReadsKey},
		{kind: "unregistered-fixture", arm: effortArmUnregistered},
	}
	if !slices.Equal(got, want) {
		t.Errorf("checkEffortCoverage(%v, %q) = %v, want %v", kinds, root, got, want)
	}
}
