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

// usageContractRegistryImportPath is the import path the checker
// resolves the "registry" package qualifier from, per file, rather
// than assuming the literal identifier "registry".
const usageContractRegistryImportPath = "github.com/sortie-ai/sortie/internal/registry"

// usageContractUndeclaredArrival and usageContractUndeclaredAttribution
// are the selector names a meta literal carries when a package spells
// the zero value out explicitly rather than omitting the key; each is
// treated identically to an omitted key.
const (
	usageContractUndeclaredArrival     = "UsageArrivalUndeclared"
	usageContractUndeclaredAttribution = "UsageAttributionUndeclared"
	usageContractNoneArrival           = "UsageArrivalNone"
	usageContractNoneAttribution       = "UsageAttributionNone"
)

// usageContractAllowlist names the packages under internal/agent/ this
// check exempts, and why: each is a support package that registers no
// agent kind. The exemption is belt alongside braces, because
// checkUsageContractPackage already draws no violation from a package
// that registers nothing; naming them keeps a later change to
// registration detection from starting to judge them. What puts a
// package under the rules is registering a kind, never its absence
// from this map.
var usageContractAllowlist = map[string]string{
	"agentcore":       "the shared decision package itself; registers no agent kind",
	"agenttest":       "the conformance-assertion package itself; registers no agent kind",
	"dispositiontest": "test-only fixture support package; registers no agent kind",
	"procutil":        "process-launch helper package; registers no agent kind",
	"sshutil":         "SSH helper package; registers no agent kind",
}

// usageContractViolation describes one place a package breaks the
// usage-reporting declaration invariant.
type usageContractViolation struct {
	pos  token.Position
	text string
}

// usageContractPackage carries one package's identity and its parsed
// non-test files.
type usageContractPackage struct {
	dirName string
	files   []*ast.File
}

// usageRegistrationFacts scans every file of one package for a call to
// registry.Agents.Register or registry.Agents.RegisterWithMeta,
// resolving the "registry" qualifier from each file's own imports. It
// reports whether the package registers an agent kind at all, whether
// the RegisterWithMeta call's third argument is a registry.AgentMeta
// composite literal, and the selector names of the UsageArrival and
// UsageAttribution values the literal carries ("" when absent, when
// Register was used, or when the third argument is not a composite
// literal).
func usageRegistrationFacts(fset *token.FileSet, files []*ast.File) (registers, metaIsLiteral bool, arrival, attribution string, pos token.Position) {
	for _, file := range files {
		registryIdent := resolveImportName(file, usageContractRegistryImportPath)
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
				lit, ok := call.Args[2].(*ast.CompositeLit)
				if !ok {
					return true
				}
				metaIsLiteral = true
				if val := mcpCompositeLitKeyValue(lit, "UsageArrival"); val != nil {
					if sel, ok := val.(*ast.SelectorExpr); ok {
						if selIdent, ok := sel.X.(*ast.Ident); ok && selIdent.Name == registryIdent {
							arrival = sel.Sel.Name
						}
					}
				}
				if val := mcpCompositeLitKeyValue(lit, "UsageAttribution"); val != nil {
					if sel, ok := val.(*ast.SelectorExpr); ok {
						if selIdent, ok := sel.X.(*ast.Ident); ok && selIdent.Name == registryIdent {
							attribution = sel.Sel.Name
						}
					}
				}
			}
			return true
		})
	}
	return registers, metaIsLiteral, arrival, attribution, pos
}

// checkUsageContractPackage evaluates the usage-disposition
// declaration rules against pkg, honoring the allowlist entries in
// usageContractAllowlist. A package this walk never observes
// registering an agent kind draws no violation, since only a
// registering package can declare a disposition at all.
func checkUsageContractPackage(fset *token.FileSet, pkg usageContractPackage) []usageContractViolation {
	if _, exempt := usageContractAllowlist[pkg.dirName]; exempt {
		return nil
	}

	registers, metaIsLiteral, arrival, attribution, pos := usageRegistrationFacts(fset, pkg.files)
	if !registers {
		return nil
	}

	if !metaIsLiteral {
		return []usageContractViolation{{
			pos:  pos,
			text: "registers an agent kind but its RegisterWithMeta call carries no registry.AgentMeta composite literal argument",
		}}
	}

	var violations []usageContractViolation
	if arrival == "" || arrival == usageContractUndeclaredArrival {
		violations = append(violations, usageContractViolation{
			pos:  pos,
			text: "registers an agent kind but declares no non-empty UsageArrival",
		})
	}
	if attribution == "" || attribution == usageContractUndeclaredAttribution {
		violations = append(violations, usageContractViolation{
			pos:  pos,
			text: "registers an agent kind but declares no non-empty UsageAttribution",
		})
	}
	if arrival != "" && attribution != "" &&
		arrival != usageContractUndeclaredArrival && attribution != usageContractUndeclaredAttribution {
		arrivalNone := arrival == usageContractNoneArrival
		attributionNone := attribution == usageContractNoneAttribution
		if arrivalNone != attributionNone {
			violations = append(violations, usageContractViolation{
				pos:  pos,
				text: "INV-1 violated: exactly one of UsageArrival and UsageAttribution names its none selector",
			})
		}
	}
	return violations
}

// TestUsageDeclarationContractInvariant walks the non-test Go files
// under internal/agent/, grouped by package directory, and fails when
// a package outside usageContractAllowlist registers an agent kind
// without both a non-undeclared UsageArrival and a non-undeclared
// UsageAttribution, or with exactly one of the two set to its none
// selector.
//
// internal/agent/ is the walk root. internal/qualification/e2e/e2e_unix.go
// also calls registry.Agents.RegisterWithMeta, for
// "qualification-e2e-fixture", deliberately outside this root: it
// registers a name rather than an adapter, its constructor returning
// an error by design because the harness supplies the adapter through
// AgentAdapterByKind, and it is test-only, build-tagged unix, imported
// by no production package. A disposition describes a runtime's
// emission behavior, and this kind has no runtime. A repository-wide
// search for RegisterWithMeta therefore finds eight registrations
// against the seven declarations this test checks.
func TestUsageDeclarationContractInvariant(t *testing.T) {
	root := ".."

	fset := token.NewFileSet()
	packages := map[string]*usageContractPackage{}
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
			// by checkUsageContractPackage when its own package is judged.
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
			pkg = &usageContractPackage{dirName: filepath.Base(dir)}
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
	registeringPackages := 0
	for _, dir := range dirOrder {
		pkg := packages[dir]
		registers, _, _, _, _ := usageRegistrationFacts(fset, pkg.files)
		if registers {
			registeringPackages++
		}
		for _, v := range checkUsageContractPackage(fset, *pkg) {
			t.Errorf("%s: %s", v.pos, v.text)
		}
	}
	if registeringPackages == 0 {
		t.Fatalf("walk of %s discovered zero packages registering an agent kind, want at least one", root)
	}
}

// TestCheckUsageContractPackage_DetectsViolations pins the checker's
// own logic against inline source fixtures, independent of the
// current state of any adapter package, so a regression in a rule is
// caught even when every real adapter happens to comply.
func TestCheckUsageContractPackage_DetectsViolations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		dirName   string
		src       string
		wantCount int
	}{
		{
			name:    "a kind registered through plain Register declares nothing",
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
			name:    "a RegisterWithMeta literal omitting both usage fields declares nothing",
			dirName: "fixture",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/registry"

func init() {
	registry.Agents.RegisterWithMeta("fixture", newFixtureAdapter, registry.AgentMeta{
		RequiresCommand: true,
	})
}
`,
			wantCount: 2,
		},
		{
			name:    "a literal spelling out the zero value for both fields is still undeclared",
			dirName: "fixture",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/registry"

func init() {
	registry.Agents.RegisterWithMeta("fixture", newFixtureAdapter, registry.AgentMeta{
		UsageArrival:     registry.UsageArrivalUndeclared,
		UsageAttribution: registry.UsageAttributionUndeclared,
	})
}
`,
			wantCount: 2,
		},
		{
			name:    "a literal declaring only UsageArrival leaves UsageAttribution undeclared",
			dirName: "fixture",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/registry"

func init() {
	registry.Agents.RegisterWithMeta("fixture", newFixtureAdapter, registry.AgentMeta{
		UsageArrival: registry.UsageArrivalIncremental,
	})
}
`,
			wantCount: 1,
		},
		{
			name:    "INV-1: arrival none paired with a non-none attribution",
			dirName: "fixture",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/registry"

func init() {
	registry.Agents.RegisterWithMeta("fixture", newFixtureAdapter, registry.AgentMeta{
		UsageArrival:     registry.UsageArrivalNone,
		UsageAttribution: registry.UsageAttributionPerModel,
	})
}
`,
			wantCount: 1,
		},
		{
			name:    "INV-1: attribution none paired with a non-none arrival",
			dirName: "fixture",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/registry"

func init() {
	registry.Agents.RegisterWithMeta("fixture", newFixtureAdapter, registry.AgentMeta{
		UsageArrival:     registry.UsageArrivalIncremental,
		UsageAttribution: registry.UsageAttributionNone,
	})
}
`,
			wantCount: 1,
		},
		{
			name:    "a compliant declaration is accepted",
			dirName: "fixture",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/registry"

func init() {
	registry.Agents.RegisterWithMeta("fixture", newFixtureAdapter, registry.AgentMeta{
		UsageArrival:     registry.UsageArrivalIncremental,
		UsageAttribution: registry.UsageAttributionPerModel,
	})
}
`,
			wantCount: 0,
		},
		{
			name:    "a compliant none/none declaration is accepted",
			dirName: "fixture",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/registry"

func init() {
	registry.Agents.RegisterWithMeta("fixture", newFixtureAdapter, registry.AgentMeta{
		UsageArrival:     registry.UsageArrivalNone,
		UsageAttribution: registry.UsageAttributionNone,
	})
}
`,
			wantCount: 0,
		},
		{
			name:    "a RegisterWithMeta call whose third argument is not a composite literal",
			dirName: "fixture",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/registry"

func init() {
	meta := buildMeta()
	registry.Agents.RegisterWithMeta("fixture", newFixtureAdapter, meta)
}
`,
			wantCount: 1,
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

			pkg := usageContractPackage{dirName: tt.dirName, files: []*ast.File{file}}
			got := checkUsageContractPackage(fset, pkg)
			if len(got) != tt.wantCount {
				t.Errorf("checkUsageContractPackage() returned %d violations, want %d: %+v", len(got), tt.wantCount, got)
			}
		})
	}
}

// TestCheckUsageContractPackage_AllowlistedPackageDrawsNoViolation
// pins that a package named in usageContractAllowlist is exempt from
// every rule, even one whose fixture would otherwise trip the
// no-declaration rule.
func TestCheckUsageContractPackage_AllowlistedPackageDrawsNoViolation(t *testing.T) {
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

	pkg := usageContractPackage{dirName: "procutil", files: []*ast.File{file}}
	got := checkUsageContractPackage(fset, pkg)
	if len(got) != 0 {
		t.Errorf("checkUsageContractPackage() for an allowlisted dirName returned %d violations, want 0: %+v", len(got), got)
	}
}
