package e2e

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// importManifest maps each github.com/sortie-ai/sortie/internal import
// the harness's non-test files may carry to the reason it is required.
// It freezes the harness's dependency set so the package cannot grow
// into a general-purpose bridge: an import this map does not name, or
// an entry no non-test file imports, is a guard failure.
var importManifest = map[string]string{
	"github.com/sortie-ai/sortie/internal/qualification":  "the promoted shutdown bound and process-group primitives",
	"github.com/sortie-ai/sortie/internal/workspace":      "computing the isolated git workspace root the fixture tracker uses",
	"github.com/sortie-ai/sortie/internal/agent/procutil": "starting the fake agent's process group and signaling it",
	"github.com/sortie-ai/sortie/internal/config":         "assembling the ServiceConfig the harness's orchestrator runs",
	"github.com/sortie-ai/sortie/internal/domain":         "the AgentAdapter interface and the session/turn types the fake agent implements",
	"github.com/sortie-ai/sortie/internal/orchestrator":   "the real orchestrator the harness drives end to end",
	"github.com/sortie-ai/sortie/internal/persistence":    "opening and migrating the store the orchestrator runs against",
	"github.com/sortie-ai/sortie/internal/prompt":         "parsing the fixture's workflow prompt template",
	"github.com/sortie-ai/sortie/internal/registry":       "registering the fixture agent kind the orchestrator's preflight resolves",
	"github.com/sortie-ai/sortie/internal/tracker/file":   "the file tracker adapter the harness's fixture drives",
}

// sortieImportPrefix marks the import-path namespace direction 1 and
// direction 2 police. A standard-library or third-party import is out
// of scope for both.
const sortieImportPrefix = "github.com/sortie-ai/sortie/internal/"

// harnessImportsReporter is the subset of *testing.T the checks below
// call, factored out so the meta-test can drive them against a fake
// that records failures instead of reddening its own run.
type harnessImportsReporter interface {
	Errorf(format string, args ...any)
}

// harnessParsedFile pairs a parsed file's name with its AST, decoupled
// from disk so the checks below can be driven by a real directory read
// or by synthetic in-memory source alike.
type harnessParsedFile struct {
	name string
	file *ast.File
}

// harnessFileImports returns every import path pf.file declares,
// unquoted.
func harnessFileImports(pf harnessParsedFile) []string {
	paths := make([]string, 0, len(pf.file.Imports))
	for _, imp := range pf.file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		paths = append(paths, path)
	}
	return paths
}

// checkHarnessImportsNamed reports direction 1: an import under
// sortieImportPrefix that manifest does not name.
func checkHarnessImportsNamed(r harnessImportsReporter, fset *token.FileSet, files []harnessParsedFile, manifest map[string]string) {
	for _, pf := range files {
		for _, imp := range pf.file.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			if !strings.HasPrefix(path, sortieImportPrefix) {
				continue
			}
			if _, ok := manifest[path]; !ok {
				r.Errorf("%s: %s imports %q, which importManifest does not name", fset.Position(imp.Pos()), pf.name, path)
			}
		}
	}
}

// checkHarnessManifestCurrent reports direction 2: a manifest entry no
// parsed file imports. This is the staleness direction, and it is the
// exact failure mode of the contract-suite allowlist mechanism this
// guard replaces: an entry covering nothing MUST fail rather than pass
// silently.
func checkHarnessManifestCurrent(r harnessImportsReporter, files []harnessParsedFile, manifest map[string]string) {
	imported := map[string]bool{}
	for _, pf := range files {
		for _, path := range harnessFileImports(pf) {
			imported[path] = true
		}
	}
	for path := range manifest {
		if !imported[path] {
			r.Errorf("importManifest names %q, but no parsed non-test file imports it", path)
		}
	}
}

// fileHasBuildConstraint reports whether file carries a comment group
// positioned before its package clause holding a line beginning
// "//go:build".
func fileHasBuildConstraint(file *ast.File) bool {
	for _, group := range file.Comments {
		if group.Pos() >= file.Package {
			continue
		}
		for _, c := range group.List {
			if strings.HasPrefix(c.Text, "//go:build") {
				return true
			}
		}
	}
	return false
}

// checkHarnessAtLeastOneUntagged reports direction 3: a count of zero
// parsed non-test files carrying no build constraint. This makes the
// package's loadability requirement executable on every platform in
// `go test ./...`, instead of a one-time manual check a later edit can
// silently undo: a package whose every file is constrained out builds
// under a ./... pattern, which skips it, and fails the moment it is
// named directly.
//
// The threshold is zero, not one. Loadability is a zero-versus-nonzero
// condition, so a second unconstrained file -- a portable helper, a
// types file -- preserves it. Pinning "exactly one" would redden a
// correct tree, and its only repair under time pressure is to edit the
// threshold, after which the check has taught nothing.
func checkHarnessAtLeastOneUntagged(r harnessImportsReporter, files []harnessParsedFile) {
	untagged := 0
	for _, pf := range files {
		if !fileHasBuildConstraint(pf.file) {
			untagged++
		}
	}
	if untagged == 0 {
		r.Errorf("parsed %d non-test file(s), every one carrying a //go:build line; want at least one unconstrained file so the package loads when named directly", len(files))
	}
}

// harnessParseMode is the parser.ParseFile mode every check in this
// file relies on. ParseComments is load-bearing: without it
// ast.File.Comments is empty, a //go:build line is invisible, and
// fileHasBuildConstraint reports every file as unconstrained.
// Measured on go1.26.1: SkipObjectResolution|ImportsOnly alone yields
// zero comment groups for a //go:build unix file;
// adding ParseComments yields one and still captures the imports. Do
// not drop this flag to match internal/adaptertest/contract_test.go or
// internal/config/extensions_contract_test.go, which parse test files
// only and have no build-constraint direction to protect.
const harnessParseMode = parser.SkipObjectResolution | parser.ImportsOnly | parser.ParseComments

// parseHarnessDirNonTestFiles reads "." (the package directory go test
// sets as the working directory) and parses every entry ending in .go
// and not in _test.go, so a build-tagged file is parsed for its
// imports and its build comment on every host regardless of GOOS.
func parseHarnessDirNonTestFiles(t *testing.T, fset *token.FileSet) []harnessParsedFile {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}

	var files []harnessParsedFile
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, name, nil, harnessParseMode)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", name, parseErr)
		}
		files = append(files, harnessParsedFile{name: name, file: file})
	}
	if len(files) == 0 {
		t.Fatalf("package directory yielded no parsed non-test Go file, want at least one")
	}
	return files
}

// TestHarnessImportsMatchTheManifest proves internal/qualification/e2e
// imports exactly its declared manifest, in both directions, and that
// at least one of its non-test files carries no build constraint.
func TestHarnessImportsMatchTheManifest(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	files := parseHarnessDirNonTestFiles(t, fset)

	checkHarnessImportsNamed(t, fset, files, importManifest)
	checkHarnessManifestCurrent(t, files, importManifest)
	checkHarnessAtLeastOneUntagged(t, files)
}

// harnessImportsFakeReporter records Errorf calls instead of failing
// the enclosing test, so TestHarnessImportManifestGuardCatchesRealBreaks
// can drive TestHarnessImportsMatchTheManifest's own checks against
// deliberately-broken synthetic input without reddening this test
// file's own run.
type harnessImportsFakeReporter struct {
	errors []string
}

func (f *harnessImportsFakeReporter) Errorf(format string, _ ...any) {
	f.errors = append(f.errors, format)
}

// mustParseHarnessSrc parses src as a single fixture file under fset,
// using the same mode the real checks run under.
func mustParseHarnessSrc(t *testing.T, fset *token.FileSet, name, src string) harnessParsedFile {
	t.Helper()
	file, err := parser.ParseFile(fset, name, src, harnessParseMode)
	if err != nil {
		t.Fatalf("parse fixture %s: %v", name, err)
	}
	return harnessParsedFile{name: name, file: file}
}

// TestHarnessImportManifestGuardCatchesRealBreaks proves each of the
// three directions TestHarnessImportsMatchTheManifest checks is itself
// capable of failing, not merely capable of passing against the
// current tree, following the shape of
// TestContractIdentityRule_StalenessGuardCatchesRealBreaks and its
// contractStalenessFakeReporter (internal/adaptertest/contract_test.go).
func TestHarnessImportManifestGuardCatchesRealBreaks(t *testing.T) {
	t.Parallel()

	t.Run("an import outside the manifest reports a failure", func(t *testing.T) {
		t.Parallel()

		fset := token.NewFileSet()
		pf := mustParseHarnessSrc(t, fset, "fixture.go", `package e2e

import "github.com/sortie-ai/sortie/internal/agent/mock"

var _ = mock.New
`)

		reporter := &harnessImportsFakeReporter{}
		checkHarnessImportsNamed(reporter, fset, []harnessParsedFile{pf}, importManifest)

		if len(reporter.errors) == 0 {
			t.Fatalf("checkHarnessImportsNamed recorded no failure for an import importManifest does not name, want at least one")
		}
	})

	t.Run("a manifest entry no file imports reports a failure", func(t *testing.T) {
		t.Parallel()

		fset := token.NewFileSet()
		pf := mustParseHarnessSrc(t, fset, "fixture.go", `package e2e

import "github.com/sortie-ai/sortie/internal/domain"

var _ = domain.Issue{}
`)
		manifest := map[string]string{
			"github.com/sortie-ai/sortie/internal/domain":       "used by the fixture file above",
			"github.com/sortie-ai/sortie/internal/orchestrator": "not imported by any fixture file, deliberately stale",
		}

		reporter := &harnessImportsFakeReporter{}
		checkHarnessManifestCurrent(reporter, []harnessParsedFile{pf}, manifest)

		if len(reporter.errors) == 0 {
			t.Fatalf("checkHarnessManifestCurrent recorded no failure for a manifest entry no fixture file imports, want at least one")
		}
	})

	// This subtest, not the threshold check, is what pins ParseComments.
	// Under a fail-on-zero threshold a mode without ParseComments makes
	// every file look unconstrained, which passes; only asking whether a
	// build-constrained file is recognized as one catches the dropped
	// flag.
	t.Run("a build-constrained synthetic file is recognized as constrained, pinning ParseComments", func(t *testing.T) {
		t.Parallel()

		fset := token.NewFileSet()
		tagged := mustParseHarnessSrc(t, fset, "tagged.go", `//go:build unix

package e2e
`)
		untagged := mustParseHarnessSrc(t, fset, "untagged.go", `package e2e
`)

		if !fileHasBuildConstraint(tagged.file) {
			t.Fatal("fileHasBuildConstraint reported a //go:build unix file as unconstrained; harnessParseMode has lost parser.ParseComments")
		}
		if fileHasBuildConstraint(untagged.file) {
			t.Fatal("fileHasBuildConstraint reported a file with no //go:build line as constrained")
		}
	})

	t.Run("only build-constrained synthetic files report a failure", func(t *testing.T) {
		t.Parallel()

		fset := token.NewFileSet()
		a := mustParseHarnessSrc(t, fset, "a.go", `//go:build unix

package e2e
`)
		b := mustParseHarnessSrc(t, fset, "b.go", `//go:build windows

package e2e
`)

		reporter := &harnessImportsFakeReporter{}
		checkHarnessAtLeastOneUntagged(reporter, []harnessParsedFile{a, b})

		if len(reporter.errors) == 0 {
			t.Fatalf("checkHarnessAtLeastOneUntagged recorded no failure for synthetic files that are all build-constrained, want at least one")
		}
	})

	// The threshold is at least one, not exactly one: a second
	// unconstrained file preserves loadability and must not report.
	t.Run("two untagged synthetic files pass", func(t *testing.T) {
		t.Parallel()

		fset := token.NewFileSet()
		a := mustParseHarnessSrc(t, fset, "a.go", `package e2e
`)
		b := mustParseHarnessSrc(t, fset, "b.go", `package e2e
`)

		reporter := &harnessImportsFakeReporter{}
		checkHarnessAtLeastOneUntagged(reporter, []harnessParsedFile{a, b})

		if len(reporter.errors) != 0 {
			t.Fatalf("checkHarnessAtLeastOneUntagged recorded %d failure(s) for two unconstrained synthetic files, want none: %v", len(reporter.errors), reporter.errors)
		}
	})
}
