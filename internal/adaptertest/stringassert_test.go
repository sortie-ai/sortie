package adaptertest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// contractConfigPath is the import path rule STRINGASSERT walks in
// addition to the four family roots; internal/config holds no adapter
// kind, so it carries no constant of its own elsewhere in this package.
const contractConfigPath = "github.com/sortie-ai/sortie/internal/config"

// contractStringAssertAllowlist exempts the sites that read a decoded
// API payload or a test fixture rather than operator configuration from
// the discarded string-assertion idiom ban, each with the reason it is
// not the fault this rule exists to catch. Keyed by a path relative to
// internal/, joined with "/" so the key is stable across platforms;
// keying by directory alone would exempt every file in a package that
// holds both an exempt site and a converted constructor.
var contractStringAssertAllowlist = map[string]string{
	"tracker/jira/adf.go":          "parses an Atlassian Document Format node from a tracker response",
	"tracker/linear/pagination.go": "reads a GraphQL variables map the adapter itself built",
	"agent/mock/mock.go":           "parses a scripted tool-call fixture",
}

// contractStringAssertRelPath returns file's path relative to internal/,
// with OS-specific separators normalized to "/", for matching against
// [contractStringAssertAllowlist]. Every root this rule walks is passed
// to [contractWalkRoot] as a two-dot-relative path, so stripping that
// leading segment yields a path relative to internal/.
func contractStringAssertRelPath(fset *token.FileSet, file *ast.File) string {
	name := filepath.ToSlash(fset.Position(file.Pos()).Filename)
	return strings.TrimPrefix(name, "../")
}

// checkContractStringAssert walks pkg's non-test files for a discarded
// two-result type assertion to string, `value, _ := v.(string)`, the
// idiom that degrades a non-string configuration value to the empty
// string instead of reporting it. A named second result and a
// single-result assertion do not match: the first is a deliberate
// decision by the reader, and the second panics rather than coercing.
func checkContractStringAssert(fset *token.FileSet, pkg contractPackage) []contractViolation {
	var violations []contractViolation
	for _, file := range pkg.files {
		if _, exempt := contractStringAssertAllowlist[contractStringAssertRelPath(fset, file)]; exempt {
			continue
		}
		ast.Inspect(file, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok || len(assign.Lhs) != 2 || len(assign.Rhs) != 1 {
				return true
			}
			blank, ok := assign.Lhs[1].(*ast.Ident)
			if !ok || blank.Name != "_" {
				return true
			}
			typeAssert, ok := assign.Rhs[0].(*ast.TypeAssertExpr)
			if !ok || typeAssert.Type == nil {
				return true
			}
			ident, ok := typeAssert.Type.(*ast.Ident)
			if !ok || ident.Name != "string" {
				return true
			}
			violations = append(violations, contractViolation{
				pos:  fset.Position(assign.Pos()),
				text: "discarded string type assertion; call typeutil.StringField instead",
			})
			return true
		})
	}
	return violations
}

// TestCheckContractStringAssert walks internal/tracker, internal/scm,
// internal/agent, internal/notify, and internal/config, excluding
// testdata and test files, and fails when a non-test file outside
// [contractStringAssertAllowlist] contains the discarded string
// type-assertion idiom.
func TestCheckContractStringAssert(t *testing.T) {
	fset := token.NewFileSet()

	roots := []struct {
		dir        string
		importPath string
	}{
		{filepath.Join("..", "tracker"), contractTrackerFamilyPath},
		{filepath.Join("..", "scm"), contractSCMFamilyPath},
		{filepath.Join("..", "agent"), contractAgentFamilyPath},
		{filepath.Join("..", "notify"), contractNotifyFamilyPath},
		{filepath.Join("..", "config"), contractConfigPath},
	}

	var walked []contractWalkedPackage
	for _, root := range roots {
		rootWalked, _ := contractWalkRoot(t, fset, root.dir, root.importPath)
		walked = append(walked, rootWalked...)
	}

	sort.Slice(walked, func(i, j int) bool { return walked[i].dir < walked[j].dir })
	for _, w := range walked {
		for _, v := range checkContractStringAssert(fset, w.pkg) {
			t.Errorf("%s: %s", v.pos, v.text)
		}
	}
}

// TestCheckContractStringAssert_DetectsViolations pins the detector's own
// match rule against inline source fixtures: a discarded two-result
// assertion is reported, while a named second result and a single-result
// assertion are not, so the rule is proven capable of failing rather than
// matching nothing.
func TestCheckContractStringAssert_DetectsViolations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		src       string
		wantCount int
	}{
		{
			name: "a discarded two-result string assertion is rejected",
			src: `package fixture

func read(config map[string]any) string {
	value, _ := config["key"].(string)
	return value
}
`,
			wantCount: 1,
		},
		{
			name: "a named second result is accepted",
			src: `package fixture

func read(config map[string]any) string {
	value, ok := config["key"].(string)
	if !ok {
		return ""
	}
	return value
}
`,
			wantCount: 0,
		},
		{
			name: "a single-result assertion is accepted",
			src: `package fixture

func read(config map[string]any) string {
	return config["key"].(string)
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

			pkg := contractPackage{dirName: "fixture", files: []*ast.File{file}}
			got := checkContractStringAssert(fset, pkg)
			if len(got) != tt.wantCount {
				t.Errorf("checkContractStringAssert() returned %d violations, want %d: %+v", len(got), tt.wantCount, got)
			}
		})
	}
}
