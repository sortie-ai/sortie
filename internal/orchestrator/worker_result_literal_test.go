package orchestrator

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var workerResultLiteralKeys = map[string]string{
	"Usage":            "localUsage",
	"UsageMeasured":    "localMeasured",
	"ModelName":        "localModelName",
	"APIRequestCount":  "localRequestCount",
	"UnaccountedTurns": "localUnaccounted",
}

type workerResultLiteralViolation struct {
	pos    token.Position
	detail string
}

func checkWorkerResultLiterals(fset *token.FileSet, files []*ast.File) (violations []workerResultLiteralViolation, literalCount int) {
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			ident, ok := lit.Type.(*ast.Ident)
			if !ok || ident.Name != "WorkerResult" {
				return true
			}
			literalCount++

			values := make(map[string]ast.Expr, len(lit.Elts))
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok {
					continue
				}
				values[key.Name] = kv.Value
			}

			for key, wantIdent := range workerResultLiteralKeys {
				value, ok := values[key]
				if !ok {
					violations = append(violations, workerResultLiteralViolation{
						pos:    fset.Position(lit.Pos()),
						detail: fmt.Sprintf("%s is missing", key),
					})
					continue
				}
				got, ok := value.(*ast.Ident)
				if !ok || got.Name != wantIdent {
					violations = append(violations, workerResultLiteralViolation{
						pos:    fset.Position(lit.Pos()),
						detail: fmt.Sprintf("%s is not set from the bare identifier %s", key, wantIdent),
					})
				}
			}
			return true
		})
	}
	return violations, literalCount
}

func workerResultKeyValue(lit *ast.CompositeLit, key string) ast.Expr {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if k, ok := kv.Key.(*ast.Ident); ok && k.Name == key {
			return kv.Value
		}
	}
	return nil
}

func isValidExitKindExpr(e ast.Expr) bool {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name == "WorkerExitNormal" || id.Name == "WorkerExitError" || id.Name == "WorkerExitCancelled"
	}
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	fn, ok := call.Fun.(*ast.Ident)
	if !ok || fn.Name != "exitKindAtEnding" || len(call.Args) != 2 {
		return false
	}
	second, ok := call.Args[1].(*ast.Ident)
	return ok && second.Name == "cancelledAtEnding"
}

func checkWorkerResultExitKind(fset *token.FileSet, files []*ast.File) (violations []workerResultLiteralViolation, literalCount int) {
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			ident, ok := lit.Type.(*ast.Ident)
			if !ok || ident.Name != "WorkerResult" {
				return true
			}
			literalCount++

			value := workerResultKeyValue(lit, "ExitKind")
			switch {
			case value == nil:
				violations = append(violations, workerResultLiteralViolation{pos: fset.Position(lit.Pos()), detail: "ExitKind is missing"})
			case !isValidExitKindExpr(value):
				violations = append(violations, workerResultLiteralViolation{pos: fset.Position(value.Pos()), detail: "ExitKind is neither a bare WorkerExit* identifier nor exitKindAtEnding(ctx, cancelledAtEnding)"})
			}
			return true
		})
	}
	return violations, literalCount
}

func checkWorkerResultNoStoppedByTokenCeilingLiteral(fset *token.FileSet, files []*ast.File) (violations []workerResultLiteralViolation) {
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			ident, ok := lit.Type.(*ast.Ident)
			if !ok || ident.Name != "WorkerResult" {
				return true
			}
			if value := workerResultKeyValue(lit, "StoppedByTokenCeiling"); value != nil {
				violations = append(violations, workerResultLiteralViolation{pos: fset.Position(value.Pos()), detail: "StoppedByTokenCeiling is set in a WorkerResult literal"})
			}
			return true
		})
	}
	return violations
}

func checkOnExitCallSites(files []*ast.File) (callCount int) {
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "OnExit" {
				return true
			}
			if recv, ok := sel.X.(*ast.Ident); ok && recv.Name == "deps" {
				callCount++
			}
			return true
		})
	}
	return callCount
}

func TestWorkerResultLiteral_Fixtures(t *testing.T) {
	t.Parallel()

	const fixtureTemplate = `package fixture

func f() {
	x := WorkerResult{%s}
	_ = x
}
`

	tests := []struct {
		name      string
		elts      string
		wantCount int
	}{
		{
			name:      "none of the mirror keys set",
			elts:      `IssueID: "x"`,
			wantCount: 5,
		},
		{
			name:      "missing Usage only",
			elts:      `UsageMeasured: localMeasured, ModelName: localModelName, APIRequestCount: localRequestCount, UnaccountedTurns: localUnaccounted`,
			wantCount: 1,
		},
		{
			name:      "missing UsageMeasured only",
			elts:      `Usage: localUsage, ModelName: localModelName, APIRequestCount: localRequestCount, UnaccountedTurns: localUnaccounted`,
			wantCount: 1,
		},
		{
			name:      "missing ModelName only",
			elts:      `Usage: localUsage, UsageMeasured: localMeasured, APIRequestCount: localRequestCount, UnaccountedTurns: localUnaccounted`,
			wantCount: 1,
		},
		{
			name:      "missing APIRequestCount only",
			elts:      `Usage: localUsage, UsageMeasured: localMeasured, ModelName: localModelName, UnaccountedTurns: localUnaccounted`,
			wantCount: 1,
		},
		{
			name:      "missing UnaccountedTurns only",
			elts:      `Usage: localUsage, UsageMeasured: localMeasured, ModelName: localModelName, APIRequestCount: localRequestCount`,
			wantCount: 1,
		},
		{
			name:      "ModelName set to a literal empty string instead of the identifier",
			elts:      `Usage: localUsage, UsageMeasured: localMeasured, ModelName: "", APIRequestCount: localRequestCount, UnaccountedTurns: localUnaccounted`,
			wantCount: 1,
		},
		{
			name:      "every key set from its identifier passes",
			elts:      `Usage: localUsage, UsageMeasured: localMeasured, ModelName: localModelName, APIRequestCount: localRequestCount, UnaccountedTurns: localUnaccounted`,
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := fmt.Sprintf(fixtureTemplate, tt.elts)
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "fixture.go", src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parser.ParseFile: %v", err)
			}

			violations, count := checkWorkerResultLiterals(fset, []*ast.File{file})
			if count != 1 {
				t.Fatalf("checkWorkerResultLiterals() found %d WorkerResult literals, want 1", count)
			}
			if len(violations) != tt.wantCount {
				t.Errorf("checkWorkerResultLiterals() = %d violations, want %d: %+v", len(violations), tt.wantCount, violations)
			}
		})
	}
}

func parseOrchestratorNonTestFiles(t *testing.T) (*token.FileSet, []*ast.File) {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("os.ReadDir(.): %v", err)
	}

	fset := token.NewFileSet()
	var files []*ast.File
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parser.ParseFile(%q): %v", name, err)
		}
		files = append(files, file)
	}
	return fset, files
}

func TestWorkerResultLiteral_RealPackage(t *testing.T) {
	t.Parallel()

	fset, files := parseOrchestratorNonTestFiles(t)

	violations, count := checkWorkerResultLiterals(fset, files)
	if count == 0 {
		t.Fatal("checkWorkerResultLiterals() found zero WorkerResult literals in internal/orchestrator, want at least one")
	}
	if len(violations) != 0 {
		t.Errorf("checkWorkerResultLiterals() found %d violations in the real package, want 0: %+v", len(violations), violations)
	}

	if violations, count := checkWorkerResultExitKind(fset, files); count == 0 {
		t.Fatal("checkWorkerResultExitKind() found zero WorkerResult literals, want at least one")
	} else if len(violations) != 0 {
		t.Errorf("checkWorkerResultExitKind() found %d violations in the real package, want 0: %+v", len(violations), violations)
	}

	if violations := checkWorkerResultNoStoppedByTokenCeilingLiteral(fset, files); len(violations) != 0 {
		t.Errorf("checkWorkerResultNoStoppedByTokenCeilingLiteral() found %d violations in the real package, want 0: %+v", len(violations), violations)
	}

	if count := checkOnExitCallSites(files); count != 1 {
		t.Errorf("checkOnExitCallSites() found %d deps.OnExit calls in the real package, want exactly 1", count)
	}
}

func TestWorkerResultLiteral_ScratchMutationDetected(t *testing.T) {
	t.Parallel()

	original, err := os.ReadFile("worker.go")
	if err != nil {
		t.Fatalf("os.ReadFile(worker.go): %v", err)
	}

	fsetBefore := token.NewFileSet()
	fileBefore, err := parser.ParseFile(fsetBefore, "worker.go", original, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parser.ParseFile(worker.go): %v", err)
	}
	violationsBefore, literalCount := checkWorkerResultLiterals(fsetBefore, []*ast.File{fileBefore})
	if len(violationsBefore) != 0 {
		t.Fatalf("checkWorkerResultLiterals() found %d violations in the unmutated worker.go, want 0: %+v", len(violationsBefore), violationsBefore)
	}

	// Remove the whole line, not a fixed spelling: gofmt re-aligns these
	// keys, so the padding between key and value shifts as keys change.
	const victimKey = "APIRequestCount:"
	idx := strings.Index(string(original), victimKey)
	if idx < 0 {
		t.Fatalf("worker.go no longer contains an %q literal key to mutate", victimKey)
	}
	lineStart := strings.LastIndexByte(string(original[:idx]), '\n') + 1
	lineEnd := idx + strings.IndexByte(string(original[idx:]), '\n') + 1
	mutated := make([]byte, 0, len(original))
	mutated = append(mutated, original[:lineStart]...)
	mutated = append(mutated, original[lineEnd:]...)

	scratchPath := filepath.Join(t.TempDir(), "worker_mutated.go")
	if err := os.WriteFile(scratchPath, mutated, 0o600); err != nil {
		t.Fatalf("os.WriteFile(%q): %v", scratchPath, err)
	}

	fsetAfter := token.NewFileSet()
	fileAfter, err := parser.ParseFile(fsetAfter, scratchPath, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parser.ParseFile(%q): %v", scratchPath, err)
	}
	violationsAfter, mutatedCount := checkWorkerResultLiterals(fsetAfter, []*ast.File{fileAfter})
	if mutatedCount != literalCount {
		t.Fatalf("checkWorkerResultLiterals() found %d WorkerResult literals after deleting one key, want %d (unchanged)", mutatedCount, literalCount)
	}
	if len(violationsAfter) != 1 {
		t.Fatalf("checkWorkerResultLiterals() = %d violations after deleting one key, want exactly 1: %+v", len(violationsAfter), violationsAfter)
	}
}

func mutateWorkerSource(t *testing.T, replacements ...string) (*token.FileSet, *ast.File) {
	t.Helper()

	if len(replacements)%2 != 0 {
		t.Fatalf("mutateWorkerSource: odd number of replacement arguments")
	}

	original, err := os.ReadFile("worker.go")
	if err != nil {
		t.Fatalf("os.ReadFile(worker.go): %v", err)
	}
	source := string(original)
	for i := 0; i < len(replacements); i += 2 {
		old, replacement := replacements[i], replacements[i+1]
		if !strings.Contains(source, old) {
			t.Fatalf("worker.go no longer contains %q to mutate", old)
		}
		source = strings.Replace(source, old, replacement, 1)
	}

	scratchPath := filepath.Join(t.TempDir(), "worker_mutated.go")
	if err := os.WriteFile(scratchPath, []byte(source), 0o600); err != nil {
		t.Fatalf("os.WriteFile(%q): %v", scratchPath, err)
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, scratchPath, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parser.ParseFile(%q): %v\n%s", scratchPath, err, source)
	}
	return fset, file
}

func TestWorkerResultLiteralRules_ScratchMutationDetected(t *testing.T) {
	t.Parallel()

	exitKindCheck := func(fset *token.FileSet, files []*ast.File) []workerResultLiteralViolation {
		v, _ := checkWorkerResultExitKind(fset, files)
		return v
	}
	onExitCheck := func(_ *token.FileSet, files []*ast.File) []workerResultLiteralViolation {
		if checkOnExitCallSites(files) == 1 {
			return nil
		}
		return []workerResultLiteralViolation{{}}
	}
	rules := []struct {
		name  string
		check func(*token.FileSet, []*ast.File) []workerResultLiteralViolation
	}{
		{"ExitKind", exitKindCheck},
		{"StoppedByTokenCeilingLiteral", checkWorkerResultNoStoppedByTokenCeilingLiteral},
		{"OnExitCallSites", onExitCheck},
	}

	tests := []struct {
		name         string
		own          string
		replacements []string
	}{
		{
			name: "ExitKind second argument inlined",
			own:  "ExitKind",
			replacements: []string{
				"exitKindAtEnding(ctx, cancelledAtEnding)",
				"exitKindAtEnding(ctx, ctx.Err() != nil)",
			},
		},
		{
			name: "StoppedByTokenCeiling set in a literal",
			own:  "StoppedByTokenCeilingLiteral",
			replacements: []string{
				"report(WorkerResult{\n\t\t\tIssueID:          issue.ID,\n\t\t\tIdentifier:       issue.Identifier,\n\t\t\tExitKind:         WorkerExitError,",
				"report(WorkerResult{\n\t\t\tIssueID:          issue.ID,\n\t\t\tIdentifier:       issue.Identifier,\n\t\t\tExitKind:         WorkerExitError,\n\t\t\tStoppedByTokenCeiling: true,",
			},
		},
		{
			name: "deps.OnExit called directly outside the report closure",
			own:  "OnExitCallSites",
			replacements: []string{
				"report(WorkerResult{\n\t\t\tIssueID:          issue.ID,\n\t\t\tIdentifier:       issue.Identifier,\n\t\t\tExitKind:         WorkerExitError,",
				"deps.OnExit(issue.ID, WorkerResult{\n\t\t\tIssueID:          issue.ID,\n\t\t\tIdentifier:       issue.Identifier,\n\t\t\tExitKind:         WorkerExitError,",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fset, file := mutateWorkerSource(t, tt.replacements...)

			for _, rule := range rules {
				want := 0
				if rule.name == tt.own {
					want = 1
				}
				if got := rule.check(fset, []*ast.File{file}); len(got) != want {
					t.Errorf("%s check = %d violations, want %d: %+v", rule.name, len(got), want, got)
				}
			}
		})
	}
}
