package agentcore

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// workEvidenceForbiddenConstants are the three WorkReport values property
// 4 forbids a non-test file outside the allowlisted packages from naming
// through its own agentcore import qualifier.
var workEvidenceForbiddenConstants = map[string]bool{
	"WorkPresent":      true,
	"WorkAbsent":       true,
	"WorkUnobservable": true,
}

// workEvidenceConstantAllowlist names the packages under internal/agent/
// property 4 exempts, and why. agentcore and mock mirror
// dispositionContractAllowlist's reasoning for the same two packages.
// dispositiontest is exempt because AssertWorkEvidenceConsistent derives
// its two failure messages from agentcore.DecideTurn rather than
// restating them as literals, which requires naming the same Work
// constants an adapter must never construct directly.
var workEvidenceConstantAllowlist = map[string]string{
	"agentcore":       "the shared decision package itself; the three constants are declared and consumed here",
	"mock":            "maps an operator-chosen outcome string, not parsed evidence, so it has nothing to observe",
	"dispositiontest": "derives its two failure messages from agentcore.DecideTurn rather than restating them",
}

// workEvidenceExitObservedIdentifier is the bare field identifier
// property 5 partitions packages on, whether read from a TurnEvidence
// value or written into a composite literal.
const workEvidenceExitObservedIdentifier = "ExitObserved"

// workEvidenceNewObserverFunc and workEvidenceObserveFuncs are the
// agentcore.WorkObserver constructor and observation methods property 5
// counts.
const workEvidenceNewObserverFunc = "NewWorkObserver"

// workEvidenceObserveFuncs maps each WorkSignals field name to the
// WorkObserver method that observes it, so property 5 can pair a
// declaration with its matching observation call.
var workEvidenceObserveFuncs = map[string]string{
	"AssistantOutput": "ObserveAssistantOutput",
	"ToolActivity":    "ObserveToolActivity",
}

// workEvidenceViolation describes one place a file or package breaks a
// work-evidence contract property.
type workEvidenceViolation struct {
	pos  token.Position
	text string
}

// checkWorkEvidenceConstantsFile appends a violation for every selector
// expression in file naming one of workEvidenceForbiddenConstants
// through the file's own agentcore import qualifier. The rule is
// decided from syntax alone: no go/types resolution is used.
func checkWorkEvidenceConstantsFile(fset *token.FileSet, file *ast.File) []workEvidenceViolation {
	agentcoreIdent := resolveImportName(file, dispositionAgentcoreImportPath)
	if agentcoreIdent == "" {
		return nil
	}

	var violations []workEvidenceViolation
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok || ident.Name != agentcoreIdent {
			return true
		}
		if workEvidenceForbiddenConstants[sel.Sel.Name] {
			violations = append(violations, workEvidenceViolation{
				pos:  fset.Position(sel.Pos()),
				text: agentcoreIdent + "." + sel.Sel.Name + " named outside its allowlisted package",
			})
		}
		return true
	})
	return violations
}

// boolLiteralValue reports the boolean value of expr and whether expr is
// the bare identifier true or false.
func boolLiteralValue(expr ast.Expr) (value, ok bool) {
	ident, isIdent := expr.(*ast.Ident)
	if !isIdent {
		return false, false
	}
	switch ident.Name {
	case "true":
		return true, true
	case "false":
		return false, true
	default:
		return false, false
	}
}

// workEvidenceObserverFacts carries what one package's non-test files
// say about ExitObserved, agentcore.NewWorkObserver, and the Observe
// methods, gathered from syntax alone.
type workEvidenceObserverFacts struct {
	namesExitObserved bool
	newObserverCalls  int
	newObserverPos    token.Position
	declaredTrue      map[string]bool
	observerNames     map[string]bool
	observedFields    map[string]bool
}

// workEvidenceReceiverName returns the trailing identifier of a receiver
// expression, which is the variable or field an observer is stored in:
// "work" for both `work` and `state.work`. It returns "" for a receiver
// shape this syntax-only check does not model.
func workEvidenceReceiverName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return e.Sel.Name
	}
	return ""
}

// isWorkEvidenceNewObserverCall reports whether expr is a call to
// agentcore.NewWorkObserver, with agentcoreIdent the name the enclosing
// file imports that package under.
func isWorkEvidenceNewObserverCall(expr ast.Expr, agentcoreIdent string) bool {
	if agentcoreIdent == "" {
		return false
	}
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	recv, ok := sel.X.(*ast.Ident)
	return ok && recv.Name == agentcoreIdent && sel.Sel.Name == workEvidenceNewObserverFunc
}

// workEvidenceObserverRegistrationFacts scans files for the ExitObserved
// identifier, every agentcore.NewWorkObserver call and the WorkSignals
// fields its argument sets true, and every WorkObserver Observe* method
// call, resolving the "agentcore" qualifier from each file's own
// imports.
func workEvidenceObserverRegistrationFacts(fset *token.FileSet, files []*ast.File) workEvidenceObserverFacts {
	facts := workEvidenceObserverFacts{
		declaredTrue:   map[string]bool{},
		observerNames:  map[string]bool{},
		observedFields: map[string]bool{},
	}

	for _, file := range files {
		agentcoreIdent := resolveImportName(file, dispositionAgentcoreImportPath)

		ast.Inspect(file, func(n ast.Node) bool {
			if ident, ok := n.(*ast.Ident); ok && ident.Name == workEvidenceExitObservedIdentifier {
				facts.namesExitObserved = true
			}

			switch node := n.(type) {
			case *ast.AssignStmt:
				for i, rhs := range node.Rhs {
					if i < len(node.Lhs) && isWorkEvidenceNewObserverCall(rhs, agentcoreIdent) {
						if name := workEvidenceReceiverName(node.Lhs[i]); name != "" {
							facts.observerNames[name] = true
						}
					}
				}
			case *ast.KeyValueExpr:
				if key, ok := node.Key.(*ast.Ident); ok && isWorkEvidenceNewObserverCall(node.Value, agentcoreIdent) {
					facts.observerNames[key.Name] = true
				}
			}

			call, ok := n.(*ast.CallExpr)
			if !ok || !isWorkEvidenceNewObserverCall(call, agentcoreIdent) {
				return true
			}
			facts.newObserverCalls++
			facts.newObserverPos = fset.Position(call.Pos())
			if len(call.Args) > 0 {
				for field := range workEvidenceObserveFuncs {
					if val := mcpCompositeLitKeyValue(call.Args[0], field); val != nil {
						if b, isBool := boolLiteralValue(val); isBool && b {
							facts.declaredTrue[field] = true
						}
					}
				}
			}
			return true
		})
	}

	// Observations are collected in a second pass because the constructor
	// can appear after the Observe calls in traversal order: three adapters
	// build the observer in RunTurn and feed it from a closure declared
	// higher in the same file.
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}

			// An Observe call counts only on the receiver the constructor
			// result was stored in. Matching the method name alone lets an
			// unrelated value with a same-named method satisfy a declared
			// signal while the observer itself never sees one.
			if !facts.observerNames[workEvidenceReceiverName(sel.X)] {
				return true
			}
			for _, method := range workEvidenceObserveFuncs {
				if sel.Sel.Name == method {
					facts.observedFields[method] = true
				}
			}
			return true
		})
	}
	return facts
}

// checkWorkEvidenceObserverWiring evaluates property 5 for one package's
// facts. A package that names ExitObserved MUST contain exactly one
// NewWorkObserver call declaring at least one field true, with an
// Observe call for each declared field and none for an undeclared one.
// A package that never names ExitObserved MUST contain no
// NewWorkObserver call.
func checkWorkEvidenceObserverWiring(dirName string, facts workEvidenceObserverFacts) []workEvidenceViolation {
	if !facts.namesExitObserved {
		if facts.newObserverCalls > 0 {
			return []workEvidenceViolation{{
				pos:  facts.newObserverPos,
				text: dirName + ": names no ExitObserved field but calls agentcore.NewWorkObserver",
			}}
		}
		return nil
	}

	if facts.newObserverCalls != 1 {
		return []workEvidenceViolation{{
			text: dirName + ": names ExitObserved but contains a non-test agentcore.NewWorkObserver call count of " + strconv.Itoa(facts.newObserverCalls) + ", want exactly 1",
		}}
	}

	var violations []workEvidenceViolation
	if len(facts.declaredTrue) == 0 {
		violations = append(violations, workEvidenceViolation{
			pos:  facts.newObserverPos,
			text: dirName + ": agentcore.NewWorkObserver's WorkSignals argument sets no field true",
		})
	}

	for field, method := range workEvidenceObserveFuncs {
		declared := facts.declaredTrue[field]
		observed := facts.observedFields[method]
		if declared && !observed {
			violations = append(violations, workEvidenceViolation{
				pos:  facts.newObserverPos,
				text: dirName + ": declares WorkSignals." + field + " but calls no " + method,
			})
		}
		if !declared && observed {
			violations = append(violations, workEvidenceViolation{
				pos:  facts.newObserverPos,
				text: dirName + ": calls " + method + " for a field WorkSignals does not declare true",
			})
		}
	}
	return violations
}

// TestWorkEvidenceContract_NoForbiddenConstants walks the non-test Go
// files of every package under internal/agent/ other than
// workEvidenceConstantAllowlist's entries and fails when any names
// WorkPresent, WorkAbsent, or WorkUnobservable through its own
// agentcore import qualifier, per spec property 4.
func TestWorkEvidenceContract_NoForbiddenConstants(t *testing.T) {
	root := ".."

	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			if _, exempt := workEvidenceConstantAllowlist[d.Name()]; exempt {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			t.Errorf("parse %s: %v", path, parseErr)
			return nil
		}

		for _, v := range checkWorkEvidenceConstantsFile(fset, file) {
			t.Errorf("%s: %s", v.pos, v.text)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}

// TestWorkEvidenceContract_ObserverWiring walks the non-test Go files
// under internal/agent/, grouped by package directory, and fails when a
// package that registers an agent kind breaks property 5's ExitObserved
// partition: naming ExitObserved without exactly one correctly wired
// agentcore.NewWorkObserver call, or calling agentcore.NewWorkObserver
// without naming ExitObserved. The subject set is derived from
// registration calls found in the tree, never from a hand-written
// package list.
func TestWorkEvidenceContract_ObserverWiring(t *testing.T) {
	root := ".."

	fset := token.NewFileSet()
	packages := map[string]*usageContractPackage{}
	var dirOrder []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
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
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	sort.Strings(dirOrder)
	registeringPackages := 0
	for _, dir := range dirOrder {
		pkg := packages[dir]
		registers, _, _, _, _ := usageRegistrationFacts(fset, pkg.files)
		if !registers {
			continue
		}
		registeringPackages++

		facts := workEvidenceObserverRegistrationFacts(fset, pkg.files)
		for _, v := range checkWorkEvidenceObserverWiring(pkg.dirName, facts) {
			t.Errorf("%s: %s", v.pos, v.text)
		}
	}
	if registeringPackages == 0 {
		t.Fatalf("walk of %s discovered zero packages registering an agent kind, want at least one", root)
	}
}

// TestCheckWorkEvidenceConstantsFile_DetectsViolations pins the property
// 4 checker's own logic against inline source fixtures, independent of
// the current state of any production package.
func TestCheckWorkEvidenceConstantsFile_DetectsViolations(t *testing.T) {
	t.Parallel()

	const preamble = `package fixture

import "github.com/sortie-ai/sortie/internal/agent/agentcore"

var _ = agentcore.TerminalSuccess
`

	tests := []struct {
		name      string
		src       string
		wantCount int
	}{
		{
			name: "WorkPresent selector is rejected",
			src: preamble + `
var w = agentcore.WorkPresent
`,
			wantCount: 1,
		},
		{
			name: "WorkAbsent selector is rejected",
			src: preamble + `
var w = agentcore.WorkAbsent
`,
			wantCount: 1,
		},
		{
			name: "WorkUnobservable selector is rejected",
			src: preamble + `
var w = agentcore.WorkUnobservable
`,
			wantCount: 1,
		},
		{
			name: "an unrelated agentcore selector is accepted",
			src: preamble + `
var w = agentcore.WorkReport(0)
`,
			wantCount: 0,
		},
		{
			name: "a file with no agentcore import is accepted",
			src: `package fixture

var w = 0
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

			got := checkWorkEvidenceConstantsFile(fset, file)
			if len(got) != tt.wantCount {
				t.Errorf("checkWorkEvidenceConstantsFile() returned %d violations, want %d: %+v", len(got), tt.wantCount, got)
			}
		})
	}
}

// TestCheckWorkEvidenceObserverWiring_DetectsViolations pins the
// property 5 checker's own logic against inline source fixtures,
// independent of the current state of any production package.
func TestCheckWorkEvidenceObserverWiring_DetectsViolations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		src       string
		wantCount int
	}{
		{
			name: "a correctly wired declaration of both fields is accepted",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/agent/agentcore"

func run(state *sessionState) {
	state.work = agentcore.NewWorkObserver(agentcore.WorkSignals{AssistantOutput: true, ToolActivity: true})
	state.work.ObserveAssistantOutput()
	state.work.ObserveToolActivity()
}

type sessionState struct {
	ExitObserved bool
	work         *agentcore.WorkObserver
}
`,
			wantCount: 0,
		},
		{
			name: "an Observe call on an unrelated receiver does not satisfy a declared field",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/agent/agentcore"

func run(state *sessionState, audit *auditLog) {
	state.work = agentcore.NewWorkObserver(agentcore.WorkSignals{AssistantOutput: true, ToolActivity: true})
	state.work.ObserveAssistantOutput()
	audit.ObserveToolActivity()
}

type auditLog struct{}

func (a *auditLog) ObserveToolActivity() {}

type sessionState struct {
	ExitObserved bool
	work         *agentcore.WorkObserver
}
`,
			wantCount: 1,
		},
		{
			name: "naming ExitObserved with zero NewWorkObserver calls is rejected",
			src: `package fixture

type sessionState struct {
	ExitObserved bool
}
`,
			wantCount: 1,
		},
		{
			name: "naming ExitObserved with two NewWorkObserver calls is rejected",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/agent/agentcore"

func run() {
	_ = agentcore.NewWorkObserver(agentcore.WorkSignals{AssistantOutput: true})
	_ = agentcore.NewWorkObserver(agentcore.WorkSignals{AssistantOutput: true})
}

type sessionState struct {
	ExitObserved bool
}
`,
			wantCount: 1,
		},
		{
			name: "a WorkSignals literal setting no field true is rejected",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/agent/agentcore"

func run() {
	_ = agentcore.NewWorkObserver(agentcore.WorkSignals{AssistantOutput: false})
}

type sessionState struct {
	ExitObserved bool
}
`,
			wantCount: 1,
		},
		{
			name: "a declared field with no matching Observe call is rejected",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/agent/agentcore"

func run(state *sessionState) {
	state.work = agentcore.NewWorkObserver(agentcore.WorkSignals{AssistantOutput: true, ToolActivity: true})
	state.work.ObserveAssistantOutput()
}

type sessionState struct {
	ExitObserved bool
	work         *agentcore.WorkObserver
}
`,
			wantCount: 1,
		},
		{
			name: "an Observe call for an undeclared field is rejected",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/agent/agentcore"

func run(state *sessionState) {
	state.work = agentcore.NewWorkObserver(agentcore.WorkSignals{AssistantOutput: true})
	state.work.ObserveAssistantOutput()
	state.work.ObserveToolActivity()
}

type sessionState struct {
	ExitObserved bool
	work         *agentcore.WorkObserver
}
`,
			wantCount: 1,
		},
		{
			name: "not naming ExitObserved with no NewWorkObserver call is accepted",
			src: `package fixture

func run() {}
`,
			wantCount: 0,
		},
		{
			name: "not naming ExitObserved but calling NewWorkObserver is rejected",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/agent/agentcore"

func run() {
	_ = agentcore.NewWorkObserver(agentcore.WorkSignals{AssistantOutput: true})
}
`,
			wantCount: 1,
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

			facts := workEvidenceObserverRegistrationFacts(fset, []*ast.File{file})
			got := checkWorkEvidenceObserverWiring("fixture", facts)
			if len(got) != tt.wantCount {
				t.Errorf("checkWorkEvidenceObserverWiring() returned %d violations, want %d: %+v", len(got), tt.wantCount, got)
			}
		})
	}
}
