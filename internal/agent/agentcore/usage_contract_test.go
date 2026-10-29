package agentcore

import (
	"fmt"
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

// usageContractAllowlist names the support packages under internal/agent/
// the wiring rule skips: they register no agent kind and may construct a
// TurnEndUsage for reasons of their own.
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

// registersAgentKind reports whether any file calls registry.Agents.Register or RegisterWithMeta.
func registersAgentKind(files []*ast.File) bool {
	registers := false
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
			if !ok || (outer.Sel.Name != "Register" && outer.Sel.Name != "RegisterWithMeta") {
				return true
			}
			inner, ok := outer.X.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := inner.X.(*ast.Ident)
			if ok && ident.Name == registryIdent && inner.Sel.Name == "Agents" {
				registers = true
			}
			return true
		})
	}
	return registers
}

// turnEndUsageNewTurnEndUsageFunc and turnEndUsageFinalizeTurnFunc are
// the agentcore symbols the turn_end wiring rule counts calls to.
const (
	turnEndUsageNewTurnEndUsageFunc = "NewTurnEndUsage"
	turnEndUsageFinalizeTurnFunc    = "FinalizeTurn"
)

// turnEndUsageEventTokenUsageConst is the domain constant a turn_end
// package's non-test files must not reference: the wiring rule requires
// every token_usage event to originate from the shared TurnEndUsage
// report, never from a direct domain.EventTokenUsage literal in the
// adapter's own package.
const turnEndUsageEventTokenUsageConst = "EventTokenUsage"

// turnEndUsageNewCall records one call to agentcore.NewTurnEndUsage
// found in a package's non-test files: where it is, and whether it
// sits directly in the body of a method declaration named StartSession,
// outside every function literal.
type turnEndUsageNewCall struct {
	pos                token.Position
	inStartSessionBody bool
}

// turnEndUsageFacts carries what one package's non-test files say about
// agentcore.NewTurnEndUsage, agentcore.FinalizeTurn, and
// domain.EventTokenUsage, gathered from syntax alone.
type turnEndUsageFacts struct {
	newCalls            []turnEndUsageNewCall
	finalizeTurnCalls   []token.Position
	eventTokenUsageRefs []token.Position
}

// turnEndUsageWalkContext is the enclosing declaration a node was found
// under: the nearest *ast.FuncDecl (nil outside every function), and
// whether the node also sits inside a *ast.FuncLit nested somewhere
// between that FuncDecl and the node itself.
type turnEndUsageWalkContext struct {
	funcDecl  *ast.FuncDecl
	inFuncLit bool
}

// isStartSessionMethodBody reports whether ctx names a method
// declaration (a *ast.FuncDecl with a receiver) called StartSession,
// with the node found directly in its body, outside every function
// literal.
func (ctx turnEndUsageWalkContext) isStartSessionMethodBody() bool {
	return ctx.funcDecl != nil && ctx.funcDecl.Recv != nil &&
		ctx.funcDecl.Name.Name == "StartSession" && !ctx.inFuncLit
}

// turnEndUsageCallVisitor walks a file tracking, for every *ast.CallExpr
// it visits, the turnEndUsageWalkContext the call was found under.
// Entering a *ast.FuncDecl resets the context to that declaration, with
// inFuncLit false; entering a *ast.FuncLit carries the enclosing
// FuncDecl forward but sets inFuncLit true. Both cases recurse into
// their own body with a child visitor and return nil, so the default
// ast.Walk traversal never double-visits their children.
type turnEndUsageCallVisitor struct {
	ctx     turnEndUsageWalkContext
	visitor func(call *ast.CallExpr, ctx turnEndUsageWalkContext)
}

func (v *turnEndUsageCallVisitor) Visit(n ast.Node) ast.Visitor {
	if n == nil {
		return nil
	}
	switch node := n.(type) {
	case *ast.FuncDecl:
		if node.Body == nil {
			return nil
		}
		child := &turnEndUsageCallVisitor{
			ctx:     turnEndUsageWalkContext{funcDecl: node},
			visitor: v.visitor,
		}
		ast.Walk(child, node.Body)
		return nil
	case *ast.FuncLit:
		child := &turnEndUsageCallVisitor{
			ctx:     turnEndUsageWalkContext{funcDecl: v.ctx.funcDecl, inFuncLit: true},
			visitor: v.visitor,
		}
		ast.Walk(child, node.Body)
		return nil
	case *ast.CallExpr:
		v.visitor(node, v.ctx)
	}
	return v
}

// turnEndUsageRegistrationFacts scans every file of one package for
// calls to agentcore.NewTurnEndUsage and agentcore.FinalizeTurn, and
// for references to domain.EventTokenUsage, resolving each qualifier
// per file via resolveImportName.
func turnEndUsageRegistrationFacts(fset *token.FileSet, files []*ast.File) turnEndUsageFacts {
	var facts turnEndUsageFacts

	for _, file := range files {
		agentcoreIdent := resolveImportName(file, dispositionAgentcoreImportPath)
		domainIdent := resolveImportName(file, dispositionDomainImportPath)

		if agentcoreIdent != "" {
			ast.Walk(&turnEndUsageCallVisitor{
				visitor: func(call *ast.CallExpr, ctx turnEndUsageWalkContext) {
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok {
						return
					}
					ident, ok := sel.X.(*ast.Ident)
					if !ok || ident.Name != agentcoreIdent {
						return
					}
					switch sel.Sel.Name {
					case turnEndUsageNewTurnEndUsageFunc:
						facts.newCalls = append(facts.newCalls, turnEndUsageNewCall{
							pos:                fset.Position(call.Pos()),
							inStartSessionBody: ctx.isStartSessionMethodBody(),
						})
					case turnEndUsageFinalizeTurnFunc:
						facts.finalizeTurnCalls = append(facts.finalizeTurnCalls, fset.Position(call.Pos()))
					}
				},
			}, file)
		}

		if domainIdent != "" {
			ast.Inspect(file, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				ident, ok := sel.X.(*ast.Ident)
				if !ok || ident.Name != domainIdent || sel.Sel.Name != turnEndUsageEventTokenUsageConst {
					return true
				}
				facts.eventTokenUsageRefs = append(facts.eventTokenUsageRefs, fset.Position(sel.Pos()))
				return true
			})
		}
	}
	return facts
}

// checkTurnEndUsageWiring judges a package that constructs
// agentcore.NewTurnEndUsage, which is how a package opts into the shared
// turn-end report. It MUST construct exactly one, directly in a
// StartSession method body outside every function literal, MUST NOT call
// agentcore.FinalizeTurn, and MUST NOT reference domain.EventTokenUsage.
func checkTurnEndUsageWiring(facts turnEndUsageFacts) []usageContractViolation {
	if len(facts.newCalls) == 0 {
		return nil
	}

	var violations []usageContractViolation
	switch len(facts.newCalls) {
	case 1:
		call := facts.newCalls[0]
		if !call.inStartSessionBody {
			violations = append(violations, usageContractViolation{
				pos:  call.pos,
				text: "constructs agentcore.TurnEndUsage but its agentcore.NewTurnEndUsage call does not sit directly in a StartSession method body, outside every function literal",
			})
		}
	default:
		violations = append(violations, usageContractViolation{
			pos:  facts.newCalls[0].pos,
			text: fmt.Sprintf("constructs agentcore.TurnEndUsage but contains %d agentcore.NewTurnEndUsage calls, want exactly 1", len(facts.newCalls)),
		})
	}

	for _, pos := range facts.finalizeTurnCalls {
		violations = append(violations, usageContractViolation{
			pos:  pos,
			text: "constructs agentcore.TurnEndUsage but calls agentcore.FinalizeTurn directly; the shared TurnEndUsage report owns that call",
		})
	}
	for _, pos := range facts.eventTokenUsageRefs {
		violations = append(violations, usageContractViolation{
			pos:  pos,
			text: "constructs agentcore.TurnEndUsage but references domain.EventTokenUsage directly; the shared TurnEndUsage report owns that event",
		})
	}
	return violations
}

var spendFieldNames = map[string]bool{
	"SpendUnaccounted": true,
	"SpendCoverable":   true,
	"CoversSession":    true,
}

// spendFieldWriterDirs names the directories that may touch a spend field
// by name: agentcore writes the turn-result fields, and the usage ledger's
// settlement carries a field of the same name as its input to that rule.
var spendFieldWriterDirs = map[string]bool{
	"agentcore": true,
	"usage":     true,
}

func checkSpendFieldWriters(fset *token.FileSet, pkg usageContractPackage) []usageContractViolation {
	if spendFieldWriterDirs[pkg.dirName] {
		return nil
	}

	var violations []usageContractViolation
	for _, file := range pkg.files {
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.AssignStmt:
				for _, lhs := range node.Lhs {
					if sel, ok := lhs.(*ast.SelectorExpr); ok && spendFieldNames[sel.Sel.Name] {
						violations = append(violations, usageContractViolation{
							pos:  fset.Position(sel.Pos()),
							text: "assigns " + sel.Sel.Name + " outside agentcore; only the shared TurnSpend rule writes a spend field",
						})
					}
				}
			case *ast.KeyValueExpr:
				if key, ok := node.Key.(*ast.Ident); ok && spendFieldNames[key.Name] {
					violations = append(violations, usageContractViolation{
						pos:  fset.Position(key.Pos()),
						text: "sets " + key.Name + " in a composite literal outside agentcore; only the shared TurnSpend rule writes a spend field",
					})
				}
			}
			return true
		})
	}
	return violations
}

func TestUsageDeclarationContractInvariant(t *testing.T) {
	t.Parallel()

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
			// Not pruned: pruning would also hide a registering package nested below.
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
	turnEndPackages := 0
	for _, dir := range dirOrder {
		pkg := packages[dir]
		if registersAgentKind(pkg.files) {
			registeringPackages++
		}
		if _, exempt := usageContractAllowlist[pkg.dirName]; exempt {
			continue
		}
		facts := turnEndUsageRegistrationFacts(fset, pkg.files)
		if len(facts.newCalls) > 0 {
			turnEndPackages++
		}
		for _, v := range checkTurnEndUsageWiring(facts) {
			t.Errorf("%s: %s", v.pos, v.text)
		}
	}
	for _, dir := range dirOrder {
		for _, v := range checkSpendFieldWriters(fset, *packages[dir]) {
			t.Errorf("%s: %s", v.pos, v.text)
		}
	}
	if registeringPackages == 0 {
		t.Fatalf("walk of %s discovered zero packages registering an agent kind, want at least one", root)
	}
	if turnEndPackages == 0 {
		t.Fatalf("walk of %s discovered zero packages constructing agentcore.NewTurnEndUsage, want at least one", root)
	}
}

func TestCheckTurnEndUsageWiring_DetectsViolations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		src       string
		wantCount int
	}{
		{
			name: "two calls",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/agent/agentcore"

type adapter struct{}

func (a *adapter) StartSession() {
	_ = agentcore.NewTurnEndUsage()
	_ = agentcore.NewTurnEndUsage()
}
`,
			wantCount: 1,
		},
		{
			name: "the one call sits in RunTurn, not StartSession",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/agent/agentcore"

type adapter struct{}

func (a *adapter) RunTurn() {
	_ = agentcore.NewTurnEndUsage()
}
`,
			wantCount: 1,
		},
		{
			name: "the one call sits in a function literal declared inside StartSession",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/agent/agentcore"

type adapter struct{}

func (a *adapter) StartSession() func() *agentcore.TurnEndUsage {
	return func() *agentcore.TurnEndUsage {
		return agentcore.NewTurnEndUsage()
	}
}
`,
			wantCount: 1,
		},
		{
			name: "a FinalizeTurn call is present",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/agent/agentcore"

type adapter struct{}

func (a *adapter) StartSession() {
	_ = agentcore.NewTurnEndUsage()
}

func (a *adapter) OnFinalize() {
	agentcore.FinalizeTurn(nil, nil, agentcore.TurnEvidence{}, agentcore.TurnMeta{})
}
`,
			wantCount: 1,
		},
		{
			name: "a domain.EventTokenUsage reference under an aliased import",
			src: `package fixture

import (
	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	dom "github.com/sortie-ai/sortie/internal/domain"
)

type adapter struct{}

func (a *adapter) StartSession() {
	_ = agentcore.NewTurnEndUsage()
}

func (a *adapter) emit() dom.AgentEvent {
	return dom.AgentEvent{Type: dom.EventTokenUsage}
}
`,
			wantCount: 1,
		},
		{
			name: "one call directly in StartSession draws none",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/agent/agentcore"

type adapter struct{}

func (a *adapter) StartSession() {
	_ = agentcore.NewTurnEndUsage()
}
`,
			wantCount: 0,
		},
		{
			name: "a package constructing no TurnEndUsage and naming domain.EventTokenUsage draws none",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/domain"

type adapter struct{}

func (a *adapter) emit() domain.AgentEvent {
	return domain.AgentEvent{Type: domain.EventTokenUsage}
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

			facts := turnEndUsageRegistrationFacts(fset, []*ast.File{file})
			got := checkTurnEndUsageWiring(facts)
			if len(got) != tt.wantCount {
				t.Errorf("checkTurnEndUsageWiring() returned %d violations, want %d: %+v", len(got), tt.wantCount, got)
			}
		})
	}
}

func TestCheckSpendFieldWriters_FlagsAssignmentAndLiteralKey(t *testing.T) {
	t.Parallel()

	const src = `package fixture

func finish(result *Result) Result {
	result.SpendUnaccounted = true
	return Result{CoversSession: true}
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parser.ParseFile: %v", err)
	}

	got := checkSpendFieldWriters(fset, usageContractPackage{dirName: "fixture", files: []*ast.File{file}})

	if len(got) != 2 {
		t.Errorf("checkSpendFieldWriters() returned %d violations, want 2: %+v", len(got), got)
	}
}
