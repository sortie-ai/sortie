package probe

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// bindingCallSiteTargets are the three calls the live profile must make
// exactly once: Run itself, the notes-consistency binding, and the
// published-posture probe. No measurement can be removed by deleting
// its call site alone while this count stays pinned at one. Run is on
// the list because the two bindings below run inside it, so pinning
// only those two would keep a fully implemented but uncalled Run green:
// a collection nothing invokes measures nothing, whatever it contains.
var bindingCallSiteTargets = []string{"Run", "enforceNotesConsistency", "runPublishedPostureProbe"}

// gradingCallSiteTargets are the two calls that build every
// collection's evidence: gradedEvidence itself, and the one
// qualification.NewFixture call gradedEvidence makes.
var gradingCallSiteTargets = []string{"gradedEvidence", "NewFixture"}

// bindingCallSiteCounts walks file's AST and counts, for every name in
// targets, the *ast.CallExpr nodes whose called function is a bare
// identifier equal to that name. It matches bare identifiers only, so
// a method call sharing a target's name, such as t.Run in a subtest,
// is never mistaken for the package-level call under test.
func bindingCallSiteCounts(file *ast.File, targets []string) map[string]int {
	return callSiteCounts(file, targets, false)
}

// gradingCallSiteCounts walks file's AST and counts, for every name in
// targets, the *ast.CallExpr nodes whose called function is either a
// bare identifier equal to that name or a selector expression whose
// selector is equal to that name, so a call reached through a package
// qualifier such as qualification.NewFixture counts the same as a bare
// call to a name declared in the same package. Its two targets,
// gradedEvidence and NewFixture, share no name with a method this
// package's own tests call, so the broader match carries no risk of
// the false positive a shared method name like Run would produce.
func gradingCallSiteCounts(file *ast.File, targets []string) map[string]int {
	return callSiteCounts(file, targets, true)
}

// callSiteCounts is the shared walk: matchSelectors controls whether a
// selector's own name counts alongside a bare identifier, and node may be a
// whole file or a single function body.
func callSiteCounts(node ast.Node, targets []string, matchSelectors bool) map[string]int {
	counts := make(map[string]int, len(targets))
	ast.Inspect(node, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := ""
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			name = fun.Name
		case *ast.SelectorExpr:
			if matchSelectors {
				name = fun.Sel.Name
			}
		}
		for _, target := range targets {
			if name == target {
				counts[target]++
			}
		}
		return true
	})
	return counts
}

func packageGoFiles(t *testing.T, skipTestFiles bool) []string {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}

	var names []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		if skipTestFiles && strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		names = append(names, entry.Name())
	}
	return names
}

// scanBindingCallSites parses every .go file directly under this
// package's own directory, test files included since the call sites
// under test live in _test.go files, and totals bindingCallSiteCounts
// across them.
func scanBindingCallSites(t *testing.T, targets []string) map[string]int {
	t.Helper()
	return scanGoFiles(t, packageGoFiles(t, false), targets, bindingCallSiteCounts)
}

// gradingCallSiteFuncNames returns the enclosing function name for each
// counted call site, in source order, so moving a call to a different function
// reddens even though it still exists exactly once.
func gradingCallSiteFuncNames(file *ast.File, targets []string) map[string][]string {
	names := make(map[string][]string, len(targets))
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		counts := callSiteCounts(fn.Body, targets, true)
		for _, target := range targets {
			for range counts[target] {
				names[target] = append(names[target], fn.Name.Name)
			}
		}
	}
	return names
}

// scanGradingCallSiteFuncNames merges gradingCallSiteFuncNames across the
// non-test files only, so a call site a test adds cannot mask a missing
// production call site.
func scanGradingCallSiteFuncNames(t *testing.T, targets []string) map[string][]string {
	t.Helper()

	fset := token.NewFileSet()
	total := make(map[string][]string, len(targets))
	for _, name := range packageGoFiles(t, true) {
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for target, funcNames := range gradingCallSiteFuncNames(file, targets) {
			total[target] = append(total[target], funcNames...)
		}
	}
	return total
}

func scanGoFiles(t *testing.T, names []string, targets []string, count func(*ast.File, []string) map[string]int) map[string]int {
	t.Helper()

	fset := token.NewFileSet()
	total := make(map[string]int, len(targets))
	for _, name := range names {
		file, parseErr := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", name, parseErr)
		}
		for target, n := range count(file, targets) {
			total[target] += n
		}
	}
	return total
}

func TestQualificationBindingHasExactlyOneCallSite(t *testing.T) {
	t.Parallel()

	t.Run("the real tree calls each binding exactly once", func(t *testing.T) {
		t.Parallel()

		counts := scanBindingCallSites(t, bindingCallSiteTargets)
		for _, target := range bindingCallSiteTargets {
			if got := counts[target]; got != 1 {
				t.Errorf("call sites for %s = %d, want exactly 1", target, got)
			}
		}
	})

	t.Run("a fixture with the call site removed counts zero", func(t *testing.T) {
		t.Parallel()

		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "fixture.go", `package fixture

func collect() {
	// Run, enforceNotesConsistency and runPublishedPostureProbe are
	// deliberately absent here, standing in for a removed call site.
}
`, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse fixture: %v", err)
		}
		counts := bindingCallSiteCounts(file, bindingCallSiteTargets)
		for _, target := range bindingCallSiteTargets {
			if got := counts[target]; got != 0 {
				t.Errorf("call sites for %s in the call-site-removed fixture = %d, want 0", target, got)
			}
		}
	})

	t.Run("a fixture with one call site each counts one", func(t *testing.T) {
		t.Parallel()

		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "fixture.go", `package fixture

func collect() {
	Run(t, coords)
	enforceNotesConsistency(nil, "", want, "")
	runPublishedPostureProbe(t, coords)
}
`, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse fixture: %v", err)
		}
		counts := bindingCallSiteCounts(file, bindingCallSiteTargets)
		for _, target := range bindingCallSiteTargets {
			if got := counts[target]; got != 1 {
				t.Errorf("call sites for %s in the present-call-site fixture = %d, want 1", target, got)
			}
		}
	})
}

func TestQualificationGradingHasExactlyOneCallSite(t *testing.T) {
	t.Parallel()

	t.Run("the real tree's non-test files call each grading target exactly once, inside its expected enclosing function", func(t *testing.T) {
		t.Parallel()

		wantEnclosingFunc := map[string]string{"gradedEvidence": "Run", "NewFixture": "gradedEvidence"}
		locations := scanGradingCallSiteFuncNames(t, gradingCallSiteTargets)
		for _, target := range gradingCallSiteTargets {
			got := locations[target]
			if len(got) != 1 {
				t.Errorf("non-test call sites for %s = %d, want exactly 1 (%v)", target, len(got), got)
				continue
			}
			if got[0] != wantEnclosingFunc[target] {
				t.Errorf("the call site for %s sits inside %s, want inside %s", target, got[0], wantEnclosingFunc[target])
			}
		}
	})

	t.Run("a fixture with the call site removed counts zero", func(t *testing.T) {
		t.Parallel()

		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "fixture.go", `package fixture

func gradedEvidence() {
	// qualification.NewFixture is deliberately absent here, standing
	// in for a removed call site, and this function's own name is not
	// a call expression.
}
`, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse fixture: %v", err)
		}
		counts := gradingCallSiteCounts(file, gradingCallSiteTargets)
		for _, target := range gradingCallSiteTargets {
			if got := counts[target]; got != 0 {
				t.Errorf("call sites for %s in the call-site-removed fixture = %d, want 0", target, got)
			}
		}
	})

	t.Run("a fixture with one call site each counts one", func(t *testing.T) {
		t.Parallel()

		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "fixture.go", `package probe

func gradedEvidence(profile qualification.RuntimeProfile) *qualification.Fixture {
	return qualification.NewFixture(qualification.FixtureNotObserved, profile.AbsentSurfaces...)
}

func Run() {
	gradedEvidence(profile)
}
`, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse fixture: %v", err)
		}
		counts := gradingCallSiteCounts(file, gradingCallSiteTargets)
		for _, target := range gradingCallSiteTargets {
			if got := counts[target]; got != 1 {
				t.Errorf("call sites for %s in the present-call-site fixture = %d, want 1", target, got)
			}
		}
	})

	t.Run("a fixture where NewFixture moves from gradedEvidence into Run reports the call inside Run", func(t *testing.T) {
		t.Parallel()

		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "fixture.go", `package probe

func gradedEvidence(fixture *qualification.Fixture) *qualification.Fixture {
	return fixture
}

func Run() {
	fixture := qualification.NewFixture(qualification.FixtureUnmeasured)
	gradedEvidence(fixture)
}
`, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse fixture: %v", err)
		}
		locations := gradingCallSiteFuncNames(file, gradingCallSiteTargets)
		if got := locations["NewFixture"]; len(got) != 1 || got[0] != "Run" {
			t.Errorf("gradingCallSiteFuncNames() NewFixture enclosing = %v, want [Run]: this fixture moved the call there, and the gate must see it move", got)
		}
	})

	t.Run("a fixture where gradedEvidence moves outside Run reports the call outside Run", func(t *testing.T) {
		t.Parallel()

		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "fixture.go", `package probe

func gradedEvidence(profile qualification.RuntimeProfile) *qualification.Fixture {
	return qualification.NewFixture(qualification.FixtureNotObserved, profile.AbsentSurfaces...)
}

func buildFixture(profile qualification.RuntimeProfile) *qualification.Fixture {
	return gradedEvidence(profile)
}

func Run() {
	buildFixture(profile)
}
`, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse fixture: %v", err)
		}
		locations := gradingCallSiteFuncNames(file, gradingCallSiteTargets)
		if got := locations["gradedEvidence"]; len(got) != 1 || got[0] != "buildFixture" {
			t.Errorf("gradingCallSiteFuncNames() gradedEvidence enclosing = %v, want [buildFixture]: this fixture moved the call outside Run, and the gate must see it move", got)
		}
	})
}
