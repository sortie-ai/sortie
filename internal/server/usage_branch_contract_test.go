package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// usageBranchAgentKindField is the field name this file's rule
// forbids from appearing in a presentation branch: a comparison
// operand, a switch tag, a case expression, or a call argument.
const usageBranchAgentKindField = "AgentKind"

// usageBranchDeclarationSet names every function this file's contract
// walks, grouped by the non-test file that declares it: every
// function that computes or formats a usage-derived value for the
// dashboard or the JSON API. internal/orchestrator is outside this
// set: every AgentKind read there propagates the value into another
// struct or a log/coalesce site (for example recovery.go's
// legacy-rehydration audit log and retry.go's default-kind
// coalescing), never into a presentation branch, so it carries no
// subject for this rule to walk.
var usageBranchDeclarationSet = map[string][]string{
	"dashboard.go": {
		"buildDashboardData",
		"usageReportingRow",
		"usageAttributionClause",
		"usageModelRow",
		"usageAPIRequestsRow",
		"usageTokensRow",
		"usageEstCostRow",
	},
	"handler.go": {
		"toStateResponse",
		"toRunningEntryResponse",
	},
}

// usageBranchContractViolation describes one place a walked function
// reads AgentKind outside the one position this rule admits: the index
// operand of an index expression.
type usageBranchContractViolation struct {
	pos  token.Position
	text string
}

// astParentVisitor walks a tree with ast.Walk, calling fn with each
// node and its immediate parent. ast.Walk's own contract - a Visit(nil)
// call after a node's children are done - is what lets a stack track
// parents without a second pass or a third-party AST package.
type astParentVisitor struct {
	stack []ast.Node
	fn    func(n, parent ast.Node)
}

func (v *astParentVisitor) Visit(n ast.Node) ast.Visitor {
	if n == nil {
		if len(v.stack) > 0 {
			v.stack = v.stack[:len(v.stack)-1]
		}
		return nil
	}
	var parent ast.Node
	if len(v.stack) > 0 {
		parent = v.stack[len(v.stack)-1]
	}
	v.fn(n, parent)
	v.stack = append(v.stack, n)
	return v
}

// agentKindReadViolations reports every read of the AgentKind field
// inside body that does not appear as the index operand of an index
// expression: a comparison operand, a switch tag, a case expression,
// a call argument, or any other position. A read is a selector
// expression whose field name is AgentKind; the scope is the selector
// itself, not the string-typed local variable a caller might later
// assign it to, which this function does not track.
func agentKindReadViolations(fset *token.FileSet, body ast.Node) []usageBranchContractViolation {
	var violations []usageBranchContractViolation

	ast.Walk(&astParentVisitor{fn: func(n, parent ast.Node) {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != usageBranchAgentKindField {
			return
		}
		if idx, ok := parent.(*ast.IndexExpr); ok && idx.Index == n {
			return
		}
		violations = append(violations, usageBranchContractViolation{
			pos:  fset.Position(sel.Pos()),
			text: "AgentKind is read outside a map index: a presentation function may index a rate table by it, never branch on it",
		})
	}}, body)

	return violations
}

// TestUsageBranchAgentKindContract walks the named declaration set in
// usageBranchDeclarationSet and fails when any of them reads
// AgentKind anywhere except as a map-index operand. The rate table is
// indexed by AgentKind, at run time from operator-supplied config,
// and an index expression cannot itself encode a per-kind
// presentation branch, so that one position is admitted and every
// other is not.
func TestUsageBranchAgentKindContract(t *testing.T) {
	fset := token.NewFileSet()

	found := 0
	for file, names := range usageBranchDeclarationSet {
		path := filepath.Join(".", file)
		parsed, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}

		wanted := make(map[string]bool, len(names))
		for _, name := range names {
			wanted[name] = true
		}

		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || !wanted[fn.Name.Name] {
				continue
			}
			found++
			for _, v := range agentKindReadViolations(fset, fn.Body) {
				t.Errorf("%s: %s: %s", v.pos, fn.Name.Name, v.text)
			}
		}
	}

	wantCount := 0
	for _, names := range usageBranchDeclarationSet {
		wantCount += len(names)
	}
	if found != wantCount {
		t.Fatalf("found %d of %d named declarations; want every entry in usageBranchDeclarationSet located", found, wantCount)
	}
}

// TestAgentKindReadViolations_DetectsViolations pins the checker's
// own logic against inline source fixtures, independent of the
// current state of dashboard.go or handler.go, so a regression in the
// rule is caught even when both files happen to comply.
func TestAgentKindReadViolations_DetectsViolations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		src       string
		wantCount int
	}{
		{
			name: "index operand is admitted",
			src: `package fixture
func f(rates map[string]int, e entry) int {
	return rates[e.AgentKind]
}`,
			wantCount: 0,
		},
		{
			name: "comparison operand is a violation",
			src: `package fixture
func f(e entry) bool {
	return e.AgentKind == "mock"
}`,
			wantCount: 1,
		},
		{
			name: "switch tag is a violation",
			src: `package fixture
func f(e entry) string {
	switch e.AgentKind {
	case "mock":
		return "m"
	}
	return ""
}`,
			wantCount: 1,
		},
		{
			name: "case expression is a violation",
			src: `package fixture
func f(e entry, x string) string {
	switch x {
	case e.AgentKind:
		return "m"
	}
	return ""
}`,
			wantCount: 1,
		},
		{
			name: "call argument is a violation",
			src: `package fixture
func f(e entry) string {
	return strings.ToUpper(e.AgentKind)
}`,
			wantCount: 1,
		},
		{
			name: "the rate map renamed to a local alias is still admitted",
			src: `package fixture
func f(rates map[string]int, e entry) int {
	aliased := rates
	return aliased[e.AgentKind]
}`,
			wantCount: 0,
		},
		{
			name: "the rate map reached through a struct field is still admitted",
			src: `package fixture
func f(s state, e entry) int {
	return s.rates[e.AgentKind]
}`,
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
			fn, ok := file.Decls[0].(*ast.FuncDecl)
			if !ok {
				t.Fatalf("fixture's first declaration is not a function")
			}

			got := agentKindReadViolations(fset, fn.Body)
			if len(got) != tt.wantCount {
				t.Errorf("agentKindReadViolations() returned %d violations, want %d: %+v", len(got), tt.wantCount, got)
			}
		})
	}
}
