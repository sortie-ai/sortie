package probe

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strings"
	"testing"
)

// Run's one legitimate call site lives in live_test.go, so it is pinned across
// test files too.
var runCallSiteTargets = []string{"Run"}

// productionBindingCallSiteTargets are scored against non-test files only, so a
// direct unit test of an inducer cannot redden the guard with a second call.
var productionBindingCallSiteTargets = []string{
	"enforceNotesConsistency", "runPublishedPostureProbe",
	"induceProtocolSemantics", "induceNativeSemantics",
	"induceRuntimeIdentity", "induceEndToEnd",
	"induceWorkspaceSecurity", "induceProcessCleanup",
}

var allBindingCallSiteTargets = append(slices.Clone(runCallSiteTargets), productionBindingCallSiteTargets...)

// gradingCallSiteTargets is the sole live-composition entry point probe.Run
// calls.
var gradingCallSiteTargets = []string{"composeLive"}

// retiredGradingSymbols are the symbols composeLive superseded. Their
// reappearance in a non-test file would mean a second live-composition path
// had returned.
var retiredGradingSymbols = []string{"gradedEvidence", "NewLiveFixture"}

// bindingCallSiteCounts counts calls whose function is a bare identifier equal
// to a target, so a method call sharing the name, such as t.Run in a subtest,
// is never counted.
func bindingCallSiteCounts(file *ast.File, targets []string) map[string]int {
	return callSiteCounts(file, targets, false)
}

// gradingCallSiteCounts also counts calls reached through a package qualifier
// such as eval.Recognize; its targets share no name with a method this
// package's tests call, so the broader match is safe.
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

func scanBindingCallSites(t *testing.T, targets []string, skipTestFiles bool) map[string]int {
	t.Helper()
	return scanGoFiles(t, packageGoFiles(t, skipTestFiles), targets, bindingCallSiteCounts)
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

	t.Run("the real tree calls Run exactly once, test files included", func(t *testing.T) {
		t.Parallel()

		counts := scanBindingCallSites(t, runCallSiteTargets, false)
		for _, target := range runCallSiteTargets {
			if got := counts[target]; got != 1 {
				t.Errorf("call sites for %s = %d, want exactly 1", target, got)
			}
		}
	})

	t.Run("the real tree's non-test files call each production binding target exactly once", func(t *testing.T) {
		t.Parallel()

		counts := scanBindingCallSites(t, productionBindingCallSiteTargets, true)
		for _, target := range productionBindingCallSiteTargets {
			if got := counts[target]; got != 1 {
				t.Errorf("non-test call sites for %s = %d, want exactly 1", target, got)
			}
		}
	})

	t.Run("a fixture with the call site removed counts zero", func(t *testing.T) {
		t.Parallel()

		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "fixture.go", `package fixture

func collect() {
	// Every pinned target is deliberately absent here, standing in for
	// a removed call site.
}
`, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse fixture: %v", err)
		}
		counts := bindingCallSiteCounts(file, allBindingCallSiteTargets)
		for _, target := range allBindingCallSiteTargets {
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
	induceProtocolSemantics(t, coords, fixture, collected)
	induceNativeSemantics(t, coords, fixture, collected, surface)
	induceRuntimeIdentity(fixture)
	induceEndToEnd(t, coords, fixture, name, version)
	induceWorkspaceSecurity(t, coords, fixture)
	induceProcessCleanup(fixture)
}
`, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse fixture: %v", err)
		}
		counts := bindingCallSiteCounts(file, allBindingCallSiteTargets)
		for _, target := range allBindingCallSiteTargets {
			if got := counts[target]; got != 1 {
				t.Errorf("call sites for %s in the present-call-site fixture = %d, want 1", target, got)
			}
		}
	})

	t.Run("a fixture with each call site duplicated counts two", func(t *testing.T) {
		t.Parallel()

		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "fixture.go", `package fixture

func collect() {
	Run(t, coords)
	Run(t, coords)
	enforceNotesConsistency(nil, "", want, "")
	enforceNotesConsistency(nil, "", want, "")
	runPublishedPostureProbe(t, coords)
	runPublishedPostureProbe(t, coords)
	induceProtocolSemantics(t, coords, fixture, collected)
	induceProtocolSemantics(t, coords, fixture, collected)
	induceNativeSemantics(t, coords, fixture, collected, surface)
	induceNativeSemantics(t, coords, fixture, collected, surface)
	induceRuntimeIdentity(fixture)
	induceRuntimeIdentity(fixture)
	induceEndToEnd(t, coords, fixture, name, version)
	induceEndToEnd(t, coords, fixture, name, version)
	induceWorkspaceSecurity(t, coords, fixture)
	induceWorkspaceSecurity(t, coords, fixture)
	induceProcessCleanup(fixture)
	induceProcessCleanup(fixture)
}
`, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse fixture: %v", err)
		}
		counts := bindingCallSiteCounts(file, allBindingCallSiteTargets)
		for _, target := range allBindingCallSiteTargets {
			if got := counts[target]; got != 2 {
				t.Errorf("call sites for %s in the duplicated-call-site fixture = %d, want 2", target, got)
			}
		}
	})
}

// TestQualificationGradingHasExactlyOneCallSite asserts composeLive is the
// sole live composition entry point, called exactly once, inside Run, and
// neither retired symbol it replaced has reappeared.
func TestQualificationGradingHasExactlyOneCallSite(t *testing.T) {
	t.Parallel()

	t.Run("the real tree's non-test files call composeLive exactly once, inside Run", func(t *testing.T) {
		t.Parallel()

		locations := scanGradingCallSiteFuncNames(t, gradingCallSiteTargets)
		got := locations["composeLive"]
		if len(got) != 1 {
			t.Fatalf("non-test call sites for composeLive = %d, want exactly 1 (%v)", len(got), got)
		}
		if got[0] != "Run" {
			t.Errorf("the call site for composeLive sits inside %s, want inside Run", got[0])
		}
	})

	t.Run("neither retired symbol composeLive replaced has reappeared in a non-test file", func(t *testing.T) {
		t.Parallel()

		counts := scanBindingCallSites(t, retiredGradingSymbols, true)
		for _, target := range retiredGradingSymbols {
			if got := counts[target]; got != 0 {
				t.Errorf("non-test call sites for the retired symbol %s = %d, want 0", target, got)
			}
		}
	})

	t.Run("a fixture with the call site removed counts zero", func(t *testing.T) {
		t.Parallel()

		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "fixture.go", `package fixture

func Run() {
	// composeLive is deliberately absent here, standing in for a removed
	// call site.
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

	t.Run("a fixture where composeLive moves outside Run reports the call outside Run", func(t *testing.T) {
		t.Parallel()

		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "fixture.go", `package probe

func buildRecords(p profile.RuntimeProfile, collected collectedObservations, startedAt time.Time) []evidence.Record {
	records, _ := composeLive(p, collected, startedAt)
	return records
}

func Run() {
	buildRecords(p, collected, startedAt)
}
`, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse fixture: %v", err)
		}
		locations := gradingCallSiteFuncNames(file, gradingCallSiteTargets)
		if got := locations["composeLive"]; len(got) != 1 || got[0] != "buildRecords" {
			t.Errorf("gradingCallSiteFuncNames() composeLive enclosing = %v, want [buildRecords]: this fixture moved the call outside Run, and the gate must see it move", got)
		}
	})

	t.Run("a fixture where composeLive is duplicated inside Run counts two", func(t *testing.T) {
		t.Parallel()

		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "fixture.go", `package fixture

func Run() {
	composeLive(p, collected, startedAt)
	composeLive(p, collected, startedAt)
}
`, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse fixture: %v", err)
		}
		counts := gradingCallSiteCounts(file, gradingCallSiteTargets)
		if got := counts["composeLive"]; got != 2 {
			t.Errorf("call sites for composeLive in the duplicated-call-site fixture = %d, want 2", got)
		}
	})
}
