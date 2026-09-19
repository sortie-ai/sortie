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
