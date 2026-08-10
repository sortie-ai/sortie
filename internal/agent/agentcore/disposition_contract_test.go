package agentcore

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// dispositionDomainImportPath is the import path the check resolves the
// "domain" package qualifier from, per file, rather than assuming the
// literal identifier "domain".
const dispositionDomainImportPath = "github.com/sortie-ai/sortie/internal/domain"

// dispositionAgentcoreImportPath is the import path the check resolves
// the "agentcore" package qualifier from, so a call to one of the three
// Emit* helpers is caught regardless of the local import alias.
const dispositionAgentcoreImportPath = "github.com/sortie-ai/sortie/internal/agent/agentcore"

// dispositionForbiddenEventConstants are the four terminal-event
// constants a package other than agentcore or mock must not assign to
// domain.TurnResult.ExitReason or domain.AgentEvent.Type directly; a
// package that assigns any other domain.AgentEventType constant (a
// non-terminal event) is unaffected by the invariant.
var dispositionForbiddenEventConstants = map[string]bool{
	"EventTurnCompleted":      true,
	"EventTurnFailed":         true,
	"EventTurnCancelled":      true,
	"EventTurnEndedWithError": true,
}

// dispositionForbiddenEmitFuncs are the three agentcore helpers reserved
// for [FinalizeTurn] alone; no other package may call them directly.
var dispositionForbiddenEmitFuncs = map[string]bool{
	"EmitTurnCompleted": true,
	"EmitTurnFailed":    true,
	"EmitTurnCancelled": true,
}

// dispositionContractAllowlist names the packages under internal/agent/
// this check exempts, and why. agentcore is exempt because it is the
// shared decision itself: FinalizeTurn is the one legal constructor of
// these values. mock is exempt because it maps an operator-chosen
// outcome string to a disposition, not parsed runtime evidence, so it
// has nothing to decide.
var dispositionContractAllowlist = map[string]string{
	"agentcore": "the shared decision package itself; FinalizeTurn is the sole permitted constructor and emitter",
	"mock":      "maps an operator-chosen outcome string, not parsed evidence, so it has nothing to decide",
}

// dispositionViolation describes one place a file breaks the
// shared-decision invariant.
type dispositionViolation struct {
	pos  token.Position
	text string
}

// resolveImportName returns the local identifier a file binds to
// importPath, or "" when the file does not import it. It reads only the
// file's own import declarations, never assumes a literal package name,
// so an aliased import cannot evade the check.
func resolveImportName(file *ast.File, importPath string) string {
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != importPath {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		segments := strings.Split(path, "/")
		return segments[len(segments)-1]
	}
	return ""
}

// isDomainSelector reports whether expr is a selector expression naming
// domainIdent.selName, e.g. domain.TurnResult when domainIdent is
// "domain".
func isDomainSelector(expr ast.Expr, domainIdent, selName string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	return ident.Name == domainIdent && sel.Sel.Name == selName
}

// isAcceptedEventConstant reports whether expr is the accepted form for
// an ExitReason or Type field assignment: a selector expression on
// domainIdent naming an AgentEventType constant other than the four
// terminal-event constants FinalizeTurn alone may produce.
func isAcceptedEventConstant(expr ast.Expr, domainIdent string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	if !ok || ident.Name != domainIdent {
		return false
	}
	if !strings.HasPrefix(sel.Sel.Name, "Event") {
		return false
	}
	return !dispositionForbiddenEventConstants[sel.Sel.Name]
}

// checkDispositionContractFile walks one parsed file's AST and appends a
// violation for every composite-literal field assignment or Emit* call
// that breaks the shared-decision invariant. The rule is decided from
// syntax alone: no ast.Ident.Obj, ast.Object, or go/types resolution is
// used, so the check cannot trip the project's own staticcheck SA1019
// rule and does not become a type-checking pass.
func checkDispositionContractFile(fset *token.FileSet, file *ast.File) []dispositionViolation {
	domainIdent := resolveImportName(file, dispositionDomainImportPath)
	agentcoreIdent := resolveImportName(file, dispositionAgentcoreImportPath)

	var violations []dispositionViolation

	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CompositeLit:
			if domainIdent == "" {
				return true
			}
			var fieldName, litName string
			switch {
			case isDomainSelector(node.Type, domainIdent, "TurnResult"):
				fieldName, litName = "ExitReason", "domain.TurnResult"
			case isDomainSelector(node.Type, domainIdent, "AgentEvent"):
				fieldName, litName = "Type", "domain.AgentEvent"
			default:
				return true
			}
			for _, elt := range node.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok || key.Name != fieldName {
					continue
				}
				if !isAcceptedEventConstant(kv.Value, domainIdent) {
					violations = append(violations, dispositionViolation{
						pos:  fset.Position(kv.Pos()),
						text: litName + "{" + fieldName + ": ...} assigns a form other than a domain.Event* selector naming a non-terminal constant",
					})
				}
			}

		case *ast.CallExpr:
			if agentcoreIdent == "" {
				return true
			}
			sel, ok := node.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if !ok || ident.Name != agentcoreIdent {
				return true
			}
			if dispositionForbiddenEmitFuncs[sel.Sel.Name] {
				violations = append(violations, dispositionViolation{
					pos:  fset.Position(node.Pos()),
					text: "call to " + agentcoreIdent + "." + sel.Sel.Name + " outside FinalizeTurn",
				})
			}
		}
		return true
	})

	return violations
}

// TestDispositionContractInvariant walks the non-test Go files of every
// package under internal/agent/ other than the allowlisted ones and
// fails when any breaks the shared-decision invariant: a
// domain.TurnResult composite literal assigning ExitReason, or a
// domain.AgentEvent composite literal assigning Type, to anything other
// than a selector on the file's own domain import naming a non-terminal
// event constant; or a direct call to agentcore.EmitTurnCompleted,
// agentcore.EmitTurnFailed, or agentcore.EmitTurnCancelled.
func TestDispositionContractInvariant(t *testing.T) {
	root := ".."

	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			if _, exempt := dispositionContractAllowlist[d.Name()]; exempt {
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

		for _, v := range checkDispositionContractFile(fset, file) {
			t.Errorf("%s: %s", v.pos, v.text)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}

// TestCheckDispositionContractFile_DetectsViolations pins the checker's
// own logic against inline source fixtures, independent of the current
// state of any production package, so a regression in the syntactic
// rule itself is caught even if every adapter happens to comply today.
func TestCheckDispositionContractFile_DetectsViolations(t *testing.T) {
	t.Parallel()

	const preamble = `package fixture

import (
	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/domain"
)

var _ = agentcore.FinalizeTurn
`

	tests := []struct {
		name      string
		src       string
		wantCount int
	}{
		{
			name: "variable indirection into ExitReason is rejected",
			src: preamble + `
func f(reason domain.AgentEventType) domain.TurnResult {
	return domain.TurnResult{ExitReason: reason}
}
`,
			wantCount: 1,
		},
		{
			name: "forbidden terminal constant on ExitReason is rejected",
			src: preamble + `
func f() domain.TurnResult {
	return domain.TurnResult{ExitReason: domain.EventTurnCompleted}
}
`,
			wantCount: 1,
		},
		{
			name: "forbidden terminal constant on AgentEvent Type is rejected",
			src: preamble + `
func f() domain.AgentEvent {
	return domain.AgentEvent{Type: domain.EventTurnFailed}
}
`,
			wantCount: 1,
		},
		{
			name: "direct EmitTurnFailed call is rejected",
			src: preamble + `
func f(emit func(domain.AgentEvent)) {
	agentcore.EmitTurnFailed(emit, "boom", 0, domain.TokenUsage{})
}
`,
			wantCount: 1,
		},
		{
			name: "non-terminal event constant on ExitReason is accepted",
			src: preamble + `
func f() domain.TurnResult {
	return domain.TurnResult{ExitReason: domain.EventNotification}
}
`,
			wantCount: 0,
		},
		{
			name: "literal with no ExitReason field is accepted",
			src: preamble + `
func f() domain.TurnResult {
	return domain.TurnResult{SessionID: "s"}
}
`,
			wantCount: 0,
		},
		{
			name: "unrelated call through the agentcore identifier is accepted",
			src: preamble + `
func f() {
	agentcore.EmitNotification(nil, "hi")
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

			got := checkDispositionContractFile(fset, file)
			if len(got) != tt.wantCount {
				t.Errorf("checkDispositionContractFile() returned %d violations, want %d: %+v", len(got), tt.wantCount, got)
			}
		})
	}
}

// TestResolveImportName pins that the qualifier is read from the file's
// own import declaration, including an aliased import, rather than
// assumed to be the literal identifier "domain".
func TestResolveImportName(t *testing.T) {
	t.Parallel()

	const src = `package fixture

import d "github.com/sortie-ai/sortie/internal/domain"

var _ = d.EventNotification
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parser.ParseFile: %v", err)
	}

	got := resolveImportName(file, dispositionDomainImportPath)
	if got != "d" {
		t.Errorf("resolveImportName() = %q, want %q (the aliased local name)", got, "d")
	}

	gotAbsent := resolveImportName(file, dispositionAgentcoreImportPath)
	if gotAbsent != "" {
		t.Errorf("resolveImportName() for an unimported path = %q, want empty", gotAbsent)
	}
}
