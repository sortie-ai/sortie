package main

import (
	"go/ast"
	"go/parser"
	"go/token"
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

	var delivering []string
	for _, kind := range kinds {
		if meta, _ := registry.Agents.Meta(kind); meta.MCPDialect.DeliversTools() {
			delivering = append(delivering, kind)
		}
	}
	if len(delivering) == 0 {
		t.Fatal("no command-requiring agent kind declares an MCP dialect that delivers tools, want at least one")
	}
	if noSSH := kindsMissingBindingKey(delivering, mcpCompletenessAgentRoot, scriptedModelImportPath, "SSH"); len(noSSH) != 0 {
		t.Errorf("agent kind(s) %v deliver tools but their fakemodel.Binding declares no SSH variant, want an SSH field naming the runtime package, an install script or a skip reason", noSSH)
	}

	if unopted := spendRatchetViolations(kinds, mcpCompletenessAgentRoot, scriptedModelImportPath); len(unopted) != 0 {
		t.Errorf("agent kind(s) %v have a fakemodel.Binding without a Spend field, want one naming the spend sources their runtime declares", unopted)
	}
}

func spendRatchetViolations(kinds []string, agentRoot, importPath string) []string {
	return kindsMissingBindingKey(kinds, agentRoot, importPath, "Spend")
}

func kindsMissingBindingKey(kinds []string, agentRoot, importPath, key string) []string {
	fset := token.NewFileSet()
	kindDirs, err := discoverKindDirectories(fset, agentRoot)
	if err != nil {
		return slices.Clone(kinds)
	}

	var missing []string
	for _, kind := range kinds {
		dir, ok := kindDirs[kind]
		if !ok {
			missing = append(missing, kind)
			continue
		}
		testFiles, _ := filepath.Glob(filepath.Join(dir, "*_test.go"))
		declared := slices.ContainsFunc(testFiles, func(path string) bool {
			file, parseErr := parser.ParseFile(fset, path, nil, 0)
			return parseErr == nil && fileDeclaresBindingKey(file, importPath, key)
		})
		if !declared {
			missing = append(missing, kind)
		}
	}
	return missing
}

func fileDeclaresBindingKey(file *ast.File, importPath, key string) bool {
	alias := resolveTestImportName(file, importPath)
	if alias == "" {
		return false
	}
	declared := false
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || !isRunnableTestFunc(fn) {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || !isSelector(lit.Type, alias, "Binding") {
				return true
			}
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if name, isIdent := kv.Key.(*ast.Ident); isIdent && name.Name == key && !isNilIdent(kv.Value) {
					declared = true
				}
			}
			return !declared
		})
	}
	return declared
}

func isSelector(expr ast.Expr, pkg, name string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == pkg
}

func isNilIdent(expr ast.Expr) bool {
	ident, ok := expr.(*ast.Ident)
	return ok && ident.Name == "nil"
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

func TestKindsMissingScriptedModelSSHVariant(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	register := func(dir, kind string) {
		writeFixtureFile(t, filepath.Join(root, dir), "register.go", `package fixture

import "github.com/sortie-ai/sortie/internal/registry"

func init() {
	registry.Agents.RegisterWithMeta("`+kind+`", newAdapter, registry.AgentMeta{RequiresCommand: true})
}
`)
	}
	binding := func(dir, body string) {
		writeFixtureFile(t, filepath.Join(root, dir), "integration_test.go", `package fixture

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest/fakemodel"
)

func TestIntegration_ScriptedModel(t *testing.T) {
	fakemodel.AssertConformance(t, fakemodel.Binding{`+body+`})
}
`)
	}
	register("install-dir", "ssh-install-kind")
	binding("install-dir", `SSH: &fakemodel.SSHVariant{Install: "true"}`)
	register("skip-dir", "ssh-skip-kind")
	binding("skip-dir", `SSH: &fakemodel.SSHVariant{Skip: "no HTTP tool servers"}`)
	register("absent-dir", "ssh-absent-kind")
	binding("absent-dir", `Kind: "ssh-absent-kind"`)
	register("nil-dir", "ssh-nil-kind")
	binding("nil-dir", `SSH: nil`)

	kinds := []string{"ssh-install-kind", "ssh-skip-kind", "ssh-absent-kind", "ssh-nil-kind", "ssh-unregistered-kind"}

	missing := kindsMissingBindingKey(kinds, root, scriptedModelImportPath, "SSH")

	want := []string{"ssh-absent-kind", "ssh-nil-kind", "ssh-unregistered-kind"}
	if !slices.Equal(missing, want) {
		t.Errorf("kindsMissingBindingKey(%v, %q, \"SSH\") = %v, want %v", kinds, root, missing, want)
	}
}

func TestSpendRatchetViolations(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	for _, tt := range []struct{ dir, kind, body string }{
		{"opted-dir", "opted-kind", `Spend: &fakemodel.SpendSources{}`},
		{"nil-spend-dir", "nil-spend-kind", `Spend: nil`},
		{"absent-dir", "absent-kind", `Kind: "absent-kind"`},
	} {
		writeFixtureFile(t, filepath.Join(root, tt.dir), "register.go", `package fixture

import "github.com/sortie-ai/sortie/internal/registry"

func init() {
	registry.Agents.RegisterWithMeta("`+tt.kind+`", newAdapter, registry.AgentMeta{RequiresCommand: true})
}
`)
		writeFixtureFile(t, filepath.Join(root, tt.dir), "integration_test.go", `package fixture

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest/fakemodel"
)

func TestIntegration_ScriptedModel(t *testing.T) {
	fakemodel.AssertConformance(t, fakemodel.Binding{`+tt.body+`})
}
`)
	}

	kinds := []string{"opted-kind", "nil-spend-kind", "absent-kind"}

	unopted := spendRatchetViolations(kinds, root, scriptedModelImportPath)

	if want := []string{"nil-spend-kind", "absent-kind"}; !slices.Equal(unopted, want) {
		t.Errorf("spendRatchetViolations(%v, %q) = %v, want %v", kinds, root, unopted, want)
	}
}
