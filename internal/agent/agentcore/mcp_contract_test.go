package agentcore

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// mcpContractRegistryImportPath is the import path the checker
// resolves the "registry" package qualifier from, per file, rather
// than assuming the literal identifier "registry".
const mcpContractRegistryImportPath = "github.com/sortie-ai/sortie/internal/registry"

// mcpConfigPathIdentifier is the bare identifier rules Y2 and Y3 look
// for across a package's non-test files. Files feeding the check are
// parsed without comments, so a doc comment naming the field, as
// kiro's and opencode's package godoc both do, is never a reference.
const mcpConfigPathIdentifier = "MCPConfigPath"

// mcpContractUndeclaredSelector is the selector name a meta literal
// carries when a package spells the zero value out explicitly rather
// than omitting the key; it is treated identically to an omitted key.
const mcpContractUndeclaredSelector = "MCPInjectionUndeclared"

// mcpContractAllowlist names the packages under internal/agent/ this
// check exempts because they register no agent kind, and why. A
// package absent from this map that this walk reaches is expected to
// register an agent kind and declare a disposition.
var mcpContractAllowlist = map[string]string{
	"agentcore":       "the shared decision package itself; registers no agent kind",
	"agenttest":       "the conformance-assertion package itself; registers no agent kind",
	"dispositiontest": "test-only fixture support package; registers no agent kind",
	"procutil":        "process-launch helper package; registers no agent kind",
	"sshutil":         "SSH helper package; registers no agent kind",
}

// mcpContractViolation describes one place a package breaks the MCP
// injection declaration invariant.
type mcpContractViolation struct {
	pos  token.Position
	text string
}

// mcpContractPackage carries one package's identity and its parsed
// non-test files.
type mcpContractPackage struct {
	dirName string
	files   []*ast.File
}

// mcpCompositeLitKeyValue returns the value expression of the field
// keyed by the given identifier name in the composite literal expr
// denotes, or nil when expr is not such a literal or carries no such
// key.
func mcpCompositeLitKeyValue(expr ast.Expr, key string) ast.Expr {
	lit, ok := expr.(*ast.CompositeLit)
	if !ok {
		return nil
	}
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if ident, ok := kv.Key.(*ast.Ident); ok && ident.Name == key {
			return kv.Value
		}
	}
	return nil
}

// mcpPackageReferencesIdentifier reports whether any file in files
// contains the bare identifier name anywhere in its syntax tree,
// which catches both a plain reference and a selector's trailing
// field name (e.g. params.MCPConfigPath).
func mcpPackageReferencesIdentifier(files []*ast.File, name string) bool {
	for _, file := range files {
		found := false
		ast.Inspect(file, func(n ast.Node) bool {
			if found {
				return false
			}
			if ident, ok := n.(*ast.Ident); ok && ident.Name == name {
				found = true
				return false
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
}

// mcpRegistrationFacts scans every file of one package for a call to
// registry.Agents.Register or registry.Agents.RegisterWithMeta,
// resolving the "registry" qualifier from each file's own imports. It
// reports whether the package registers an agent kind at all, and,
// for RegisterWithMeta, the selector name of the MCPInjection value
// carried in the meta literal ("" when Register was used, or when the
// literal carries no MCPInjection key).
func mcpRegistrationFacts(fset *token.FileSet, files []*ast.File) (registers bool, declared string, pos token.Position) {
	for _, file := range files {
		registryIdent := resolveImportName(file, mcpContractRegistryImportPath)
		if registryIdent == "" {
			continue
		}

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			outer, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			inner, ok := outer.X.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := inner.X.(*ast.Ident)
			if !ok || ident.Name != registryIdent || inner.Sel.Name != "Agents" {
				return true
			}

			switch outer.Sel.Name {
			case "Register":
				registers = true
				pos = fset.Position(call.Pos())
			case "RegisterWithMeta":
				registers = true
				pos = fset.Position(call.Pos())
				if len(call.Args) < 3 {
					return true
				}
				val := mcpCompositeLitKeyValue(call.Args[2], "MCPInjection")
				sel, ok := val.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if selIdent, ok := sel.X.(*ast.Ident); ok && selIdent.Name == registryIdent {
					declared = sel.Sel.Name
				}
			}
			return true
		})
	}
	return registers, declared, pos
}

// checkMCPContractPackage evaluates rules Y1, Y2, and Y3 against pkg,
// honoring the allowlist entries in mcpContractAllowlist. A package
// this walk never observes registering an agent kind draws no
// violation, since only a registering package can declare a
// disposition at all.
func checkMCPContractPackage(fset *token.FileSet, pkg mcpContractPackage) []mcpContractViolation {
	if _, exempt := mcpContractAllowlist[pkg.dirName]; exempt {
		return nil
	}

	registers, declared, pos := mcpRegistrationFacts(fset, pkg.files)
	if !registers {
		return nil
	}

	if declared == "" || declared == mcpContractUndeclaredSelector {
		return []mcpContractViolation{{
			pos:  pos,
			text: "registers an agent kind but declares no non-empty MCPInjection",
		}}
	}

	referencesPath := mcpPackageReferencesIdentifier(pkg.files, mcpConfigPathIdentifier)

	var violations []mcpContractViolation
	switch declared {
	case "MCPInjectionUnsupported":
		if referencesPath {
			violations = append(violations, mcpContractViolation{
				pos:  pos,
				text: "declares MCPInjectionUnsupported but non-test files reference " + mcpConfigPathIdentifier,
			})
		}
	case "MCPInjectionSupported":
		if !referencesPath {
			violations = append(violations, mcpContractViolation{
				pos:  pos,
				text: "declares MCPInjectionSupported but non-test files never reference " + mcpConfigPathIdentifier,
			})
		}
	}
	return violations
}

// TestMCPInjectionContractInvariant walks the non-test Go files under
// internal/agent/, grouped by package directory, and fails when a
// package outside mcpContractAllowlist registers an agent kind and
// either declares no non-empty MCPInjection, declares
// MCPInjectionUnsupported while its non-test files reference
// MCPConfigPath, or declares MCPInjectionSupported while its non-test
// files never reference it.
func TestMCPInjectionContractInvariant(t *testing.T) {
	root := ".."

	fset := token.NewFileSet()
	packages := map[string]*mcpContractPackage{}
	var dirOrder []string
	parsed := 0

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			if _, exempt := mcpContractAllowlist[d.Name()]; exempt {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		dir := filepath.Dir(path)
		pkg, seen := packages[dir]
		if !seen {
			pkg = &mcpContractPackage{dirName: filepath.Base(dir)}
			packages[dir] = pkg
			dirOrder = append(dirOrder, dir)
		}

		file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			t.Errorf("parse %s: %v", path, parseErr)
			return nil
		}
		pkg.files = append(pkg.files, file)
		parsed++
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if parsed == 0 {
		t.Fatalf("root %s yielded no parsed Go files, want at least one", root)
	}

	sort.Strings(dirOrder)
	for _, dir := range dirOrder {
		for _, v := range checkMCPContractPackage(fset, *packages[dir]) {
			t.Errorf("%s: %s", v.pos, v.text)
		}
	}
}

// TestCheckMCPContractPackage_DetectsViolations pins the checker's own
// logic against inline source fixtures, independent of the current
// state of any adapter package, so a regression in a rule is caught
// even when every real adapter happens to comply. Each fixture is
// parsed the same way the walking test parses production files:
// without comments, so the negative fixture proves comment immunity
// rather than merely asserting it.
func TestCheckMCPContractPackage_DetectsViolations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		dirName   string
		src       string
		wantCount int
	}{
		{
			name:    "Y1: a kind registered through plain Register declares nothing",
			dirName: "fixture",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/registry"

func init() {
	registry.Agents.Register("fixture", newFixtureAdapter)
}
`,
			wantCount: 1,
		},
		{
			name:    "Y1: a RegisterWithMeta literal omitting MCPInjection declares nothing",
			dirName: "fixture",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/registry"

func init() {
	registry.Agents.RegisterWithMeta("fixture", newFixtureAdapter, registry.AgentMeta{
		RequiresCommand: true,
	})
}
`,
			wantCount: 1,
		},
		{
			name:    "Y1: a literal spelling out the zero value is still undeclared",
			dirName: "fixture",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/registry"

func init() {
	registry.Agents.RegisterWithMeta("fixture", newFixtureAdapter, registry.AgentMeta{
		MCPInjection: registry.MCPInjectionUndeclared,
	})
}
`,
			wantCount: 1,
		},
		{
			name:    "Y2: declares unsupported but a non-test file references MCPConfigPath",
			dirName: "fixture",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/registry"

func init() {
	registry.Agents.RegisterWithMeta("fixture", newFixtureAdapter, registry.AgentMeta{
		MCPInjection: registry.MCPInjectionUnsupported,
	})
}

var _ = MCPConfigPath
`,
			wantCount: 1,
		},
		{
			name:    "Y3: declares supported but no non-test file ever references MCPConfigPath",
			dirName: "fixture",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/registry"

func init() {
	registry.Agents.RegisterWithMeta("fixture", newFixtureAdapter, registry.AgentMeta{
		MCPInjection: registry.MCPInjectionSupported,
	})
}
`,
			wantCount: 1,
		},
		{
			// The doc comment names MCPConfigPath, but the check parses
			// without comments, so this fixture must draw zero violations.
			// Without this fixture, nothing stops a later rewrite from
			// parsing comments and taking kiro's and opencode's package
			// godoc with it.
			name:    "Y2 negative: a comment-only mention of MCPConfigPath is not a reference",
			dirName: "fixture",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/registry"

// MCPConfigPath is ignored; this adapter passes no MCP argument.
func init() {
	registry.Agents.RegisterWithMeta("fixture", newFixtureAdapter, registry.AgentMeta{
		MCPInjection: registry.MCPInjectionUnsupported,
	})
}
`,
			wantCount: 0,
		},
		{
			name:    "a compliant Supported package referencing MCPConfigPath in real code is accepted",
			dirName: "fixture",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/registry"

func init() {
	registry.Agents.RegisterWithMeta("fixture", newFixtureAdapter, registry.AgentMeta{
		MCPInjection: registry.MCPInjectionSupported,
	})
}

func useParams(mcpConfigPath string) []string {
	return []string{"--mcp-config", mcpConfigPath}
}

var _ = MCPConfigPath
`,
			wantCount: 0,
		},
		{
			name:    "a package this walk never observed registering draws no violation",
			dirName: "fixture",
			src: `package fixture

func helper() string {
	return "no registration here"
}
`,
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "fixture.go", tt.src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parser.ParseFile: %v", err)
			}

			pkg := mcpContractPackage{dirName: tt.dirName, files: []*ast.File{file}}
			got := checkMCPContractPackage(fset, pkg)
			if len(got) != tt.wantCount {
				t.Errorf("checkMCPContractPackage() returned %d violations, want %d: %+v", len(got), tt.wantCount, got)
			}
		})
	}
}

// TestCheckMCPContractPackage_AllowlistedPackageDrawsNoViolation pins
// that a package named in mcpContractAllowlist is exempt from every
// rule, even one whose fixture would otherwise trip Y1.
func TestCheckMCPContractPackage_AllowlistedPackageDrawsNoViolation(t *testing.T) {
	t.Parallel()

	const src = `package fixture

import "github.com/sortie-ai/sortie/internal/registry"

func init() {
	registry.Agents.Register("fixture", newFixtureAdapter)
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parser.ParseFile: %v", err)
	}

	pkg := mcpContractPackage{dirName: "procutil", files: []*ast.File{file}}
	got := checkMCPContractPackage(fset, pkg)
	if len(got) != 0 {
		t.Errorf("checkMCPContractPackage() for an allowlisted dirName returned %d violations, want 0: %+v", len(got), got)
	}
}
