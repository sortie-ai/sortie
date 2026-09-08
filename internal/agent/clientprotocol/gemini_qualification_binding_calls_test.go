package clientprotocol

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// bindingCallSiteTargets are the two calls the live profile must make
// exactly once: the notes-consistency binding and the published-posture
// probe. Neither measurement can be removed by deleting its call site
// alone while this count stays pinned at one.
var bindingCallSiteTargets = []string{"enforceNotesConsistency", "geminiRunPublishedPostureProbe"}

// bindingCallSiteCounts walks file's AST and counts, for every name in
// targets, the *ast.CallExpr nodes whose called function is a bare
// identifier equal to that name.
func bindingCallSiteCounts(file *ast.File, targets []string) map[string]int {
	counts := make(map[string]int, len(targets))
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		ident, ok := call.Fun.(*ast.Ident)
		if !ok {
			return true
		}
		for _, target := range targets {
			if ident.Name == target {
				counts[target]++
			}
		}
		return true
	})
	return counts
}

// scanBindingCallSites parses every .go file directly under this
// package's own directory, test files included since the call sites
// under test live in _test.go files, and totals bindingCallSiteCounts
// across them.
func scanBindingCallSites(t *testing.T, targets []string) map[string]int {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}

	fset := token.NewFileSet()
	total := make(map[string]int, len(targets))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, entry.Name(), nil, parser.SkipObjectResolution)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", entry.Name(), parseErr)
		}
		for name, count := range bindingCallSiteCounts(file, targets) {
			total[name] += count
		}
	}
	return total
}

// parseBindingCallSitesSrc parses src as a single fixture file and
// returns its bindingCallSiteCounts.
func parseBindingCallSitesSrc(t *testing.T, src string) map[string]int {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return bindingCallSiteCounts(file, bindingCallSiteTargets)
}

// TestGeminiQualificationBindingHasExactlyOneCallSite confirms the live
// profile calls enforceNotesConsistency and geminiRunPublishedPostureProbe
// exactly once each, and proves the scanner itself can fail by running
// it against a synthetic fixture with the call site removed and against
// one where it is present.
func TestGeminiQualificationBindingHasExactlyOneCallSite(t *testing.T) {
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

		counts := parseBindingCallSitesSrc(t, `package fixture

func collect() {
	// enforceNotesConsistency and geminiRunPublishedPostureProbe are
	// deliberately absent here, standing in for a removed call site.
}
`)
		for _, target := range bindingCallSiteTargets {
			if got := counts[target]; got != 0 {
				t.Errorf("call sites for %s in the call-site-removed fixture = %d, want 0", target, got)
			}
		}
	})

	t.Run("a fixture with one call site each counts one", func(t *testing.T) {
		t.Parallel()

		counts := parseBindingCallSitesSrc(t, `package fixture

func collect() {
	enforceNotesConsistency(nil, "", want, "")
	geminiRunPublishedPostureProbe(t, config)
}
`)
		for _, target := range bindingCallSiteTargets {
			if got := counts[target]; got != 1 {
				t.Errorf("call sites for %s in the present-call-site fixture = %d, want 1", target, got)
			}
		}
	})
}
