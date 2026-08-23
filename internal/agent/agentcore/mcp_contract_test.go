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
// check exempts, and why: each is a support package that registers no
// agent kind. The exemption is belt alongside braces, because
// checkMCPContractPackage already draws no violation from a package
// that registers nothing; naming them keeps a later change to
// registration detection from starting to judge them. What puts a
// package under the rules is registering a kind, never its absence
// from this map.
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
// reads name as the trailing field of a selector expression, which is
// the form an adapter takes when it reads the worker-generated path
// off its start-session parameters. Three shapes are deliberately not
// reads, because none of them hands anything to the agent process: a
// declaration of a field with the same name, a bare identifier, and
// the target of a plain assignment. A package matching only one of
// them must neither satisfy the supported rule nor breach the
// unsupported one. The match stays syntactic, resolving no symbol, so
// this check does not become a type-checking pass.
func mcpPackageReferencesIdentifier(files []*ast.File, name string) bool {
	for _, file := range files {
		if fileReadsSelector(file, name) {
			return true
		}
	}
	return false
}

// fileReadsSelector reports whether file contains a selector
// expression naming name in a position that reads it. Assignment
// targets are collected first and excluded from the second pass.
// Compound assignments are left in, since an operator such as += reads
// the field before it writes it, as is an increment for the same
// reason. A plain assignment and a range clause are the only two
// statement forms whose target can be a selector, so collecting both
// closes the set.
func fileReadsSelector(file *ast.File, name string) bool {
	assigned := make(map[ast.Node]bool)
	ast.Inspect(file, func(n ast.Node) bool {
		switch stmt := n.(type) {
		case *ast.AssignStmt:
			if stmt.Tok != token.ASSIGN {
				return true
			}
			for _, lhs := range stmt.Lhs {
				assigned[lhs] = true
			}
		case *ast.RangeStmt:
			if stmt.Tok != token.ASSIGN {
				return true
			}
			if stmt.Key != nil {
				assigned[stmt.Key] = true
			}
			if stmt.Value != nil {
				assigned[stmt.Value] = true
			}
		}
		return true
	})

	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		if found {
			return false
		}
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != name || assigned[sel] {
			return true
		}
		found = true
		return false
	})
	return found
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
			// An allowlisted directory is not pruned here, only exempted
			// by checkMCPContractPackage when its own package is judged.
			// Pruning would also hide any package nested below it, which
			// would let a registering package escape the walk entirely.
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

func start(params domain.StartSessionParams) string {
	return params.MCPConfigPath
}
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

func useParams(params domain.StartSessionParams) []string {
	return []string{"--mcp-config", params.MCPConfigPath}
}
`,
			wantCount: 0,
		},
		{
			// A field named MCPConfigPath is a declaration, not a read.
			// The package hands nothing to the agent process, so an
			// unsupported declaration is not in breach.
			name:    "Y2 negative: declaring a field of the same name is not a read",
			dirName: "fixture",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/registry"

func init() {
	registry.Agents.RegisterWithMeta("fixture", newFixtureAdapter, registry.AgentMeta{
		MCPInjection: registry.MCPInjectionUnsupported,
	})
}

type sessionParams struct {
	MCPConfigPath string
}
`,
			wantCount: 0,
		},
		{
			// The mirror of the case above: a declaration must not let a
			// package claim supported without ever reading the path.
			name:    "Y3: declaring a field of the same name does not satisfy supported",
			dirName: "fixture",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/registry"

func init() {
	registry.Agents.RegisterWithMeta("fixture", newFixtureAdapter, registry.AgentMeta{
		MCPInjection: registry.MCPInjectionSupported,
	})
}

type sessionParams struct {
	MCPConfigPath string
}
`,
			wantCount: 1,
		},
		{
			// Assigning to the field on a by-value parameter writes to a
			// local copy and hands nothing over, so it is not a read.
			name:    "Y2 negative: writing the field is not a read",
			dirName: "fixture",
			src: `package fixture

import (
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

func init() {
	registry.Agents.RegisterWithMeta("fixture", newFixtureAdapter, registry.AgentMeta{
		MCPInjection: registry.MCPInjectionUnsupported,
	})
}

func start(params domain.StartSessionParams) {
	params.MCPConfigPath = ""
}
`,
			wantCount: 0,
		},
		{
			// The mirror: a write must not let a package claim supported.
			name:    "Y3: writing the field does not satisfy supported",
			dirName: "fixture",
			src: `package fixture

import (
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

func init() {
	registry.Agents.RegisterWithMeta("fixture", newFixtureAdapter, registry.AgentMeta{
		MCPInjection: registry.MCPInjectionSupported,
	})
}

func start(params domain.StartSessionParams) {
	params.MCPConfigPath = ""
}
`,
			wantCount: 1,
		},
		{
			// A read on the right-hand side of an assignment is still a
			// read; only the target of one is excluded.
			name:    "Y3: reading the field on an assignment right-hand side satisfies supported",
			dirName: "fixture",
			src: `package fixture

import (
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

func init() {
	registry.Agents.RegisterWithMeta("fixture", newFixtureAdapter, registry.AgentMeta{
		MCPInjection: registry.MCPInjectionSupported,
	})
}

func start(params domain.StartSessionParams) string {
	var path string
	path = params.MCPConfigPath
	return path
}
`,
			wantCount: 0,
		},
		{
			// A range clause with "=" assigns to existing lvalues, so a
			// selector used as its target is written, not read.
			name:    "Y2 negative: a range-assignment target is not a read",
			dirName: "fixture",
			src: `package fixture

import (
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

func init() {
	registry.Agents.RegisterWithMeta("fixture", newFixtureAdapter, registry.AgentMeta{
		MCPInjection: registry.MCPInjectionUnsupported,
	})
}

func start(params domain.StartSessionParams, xs []string) {
	for _, params.MCPConfigPath = range xs {
	}
}
`,
			wantCount: 0,
		},
		{
			name:    "Y3: a range-assignment target does not satisfy supported",
			dirName: "fixture",
			src: `package fixture

import (
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

func init() {
	registry.Agents.RegisterWithMeta("fixture", newFixtureAdapter, registry.AgentMeta{
		MCPInjection: registry.MCPInjectionSupported,
	})
}

func start(params domain.StartSessionParams, xs []string) {
	for _, params.MCPConfigPath = range xs {
	}
}
`,
			wantCount: 1,
		},
		{
			// Ranging over the field reads it; only a target is excluded.
			name:    "Y3: ranging over the field is a read",
			dirName: "fixture",
			src: `package fixture

import (
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

func init() {
	registry.Agents.RegisterWithMeta("fixture", newFixtureAdapter, registry.AgentMeta{
		MCPInjection: registry.MCPInjectionSupported,
	})
}

func start(params domain.StartSessionParams) int {
	n := 0
	for range params.MCPConfigPath {
		n++
	}
	return n
}
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
