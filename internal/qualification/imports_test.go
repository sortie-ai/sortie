package qualification

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// qualificationImportManifest names every github.com/sortie-ai/sortie
// import internal/qualification's own non-test files may carry. It is
// empty by design: internal/qualification stays a leaf so that
// clientprotocol's (and any future family-root package's) test files
// can depend on it without pulling in the orchestrator, the store, or
// any adapter family. A future entry is checked in both directions
// from its first commit; see checkQualificationManifestCurrent.
var qualificationImportManifest = map[string]string{}

// qualificationSortieImportPrefix marks the import-path namespace
// direction 1 and direction 2 police. A standard-library or
// third-party import is out of scope for both.
const qualificationSortieImportPrefix = "github.com/sortie-ai/sortie/internal/"

// qualificationImportsReporter is the subset of *testing.T the checks
// below call, factored out so the meta-test can drive them against a
// fake that records failures instead of reddening its own run.
type qualificationImportsReporter interface {
	Errorf(format string, args ...any)
}

// qualificationParsedFile pairs a parsed file's name with its AST,
// decoupled from disk so the checks below can be driven by a real
// directory read or by synthetic in-memory source alike.
type qualificationParsedFile struct {
	name string
	file *ast.File
}

// qualificationFileImports returns every import path pf.file declares,
// unquoted.
func qualificationFileImports(pf qualificationParsedFile) []string {
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

// checkQualificationImportsNamed reports direction 1: an import under
// qualificationSortieImportPrefix that manifest does not name. Applied
// with the real, empty qualificationImportManifest, this bans every
// github.com/sortie-ai/sortie/ import from a non-test file of this
// package outright: internal/qualification stays a leaf, which is
// broader than banning only internal/orchestrator, because leaf is the
// property the package's placement as a family-root dependency rests
// on and the zero-import form needs no exception list to keep current.
func checkQualificationImportsNamed(r qualificationImportsReporter, fset *token.FileSet, files []qualificationParsedFile, manifest map[string]string) {
	for _, pf := range files {
		for _, imp := range pf.file.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			if !strings.HasPrefix(path, qualificationSortieImportPrefix) {
				continue
			}
			if _, ok := manifest[path]; !ok {
				r.Errorf("%s: %s imports %q, which qualificationImportManifest does not name", fset.Position(imp.Pos()), pf.name, path)
			}
		}
	}
}

// checkQualificationManifestCurrent reports direction 2: a manifest
// entry no parsed file imports. Against the real, empty
// qualificationImportManifest this direction is inert, since iterating
// zero entries reports nothing; it stays wired so that a future
// non-empty manifest is checked in both directions from its first
// commit, and TestQualificationImportManifestGuardCatchesRealBreaks
// drives it with a non-empty synthetic manifest to prove it can fail.
func checkQualificationManifestCurrent(r qualificationImportsReporter, files []qualificationParsedFile, manifest map[string]string) {
	imported := map[string]bool{}
	for _, pf := range files {
		for _, path := range qualificationFileImports(pf) {
			imported[path] = true
		}
	}
	for path := range manifest {
		if !imported[path] {
			r.Errorf("qualificationImportManifest names %q, but no parsed non-test file imports it", path)
		}
	}
}

// parseQualificationDirNonTestFiles reads "." (the package directory
// go test sets as the working directory) and parses every entry
// ending in .go and not in _test.go. Test files are excluded on
// purpose: a package's test files are not part of its import graph as
// its importers see it, so internal/qualification's own test files may
// import internal/agent/procutil and internal/agent/agenttest for the
// promoted process-group oracle's controls without tripping this
// guard.
func parseQualificationDirNonTestFiles(t *testing.T, fset *token.FileSet) []qualificationParsedFile {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}

	var files []qualificationParsedFile
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution|parser.ImportsOnly)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", name, parseErr)
		}
		files = append(files, qualificationParsedFile{name: name, file: file})
	}
	if len(files) == 0 {
		t.Fatalf("package directory yielded no parsed non-test Go file, want at least one")
	}
	return files
}

// TestQualificationImportsMatchTheManifest proves internal/qualification's
// own non-test files import no github.com/sortie-ai/sortie package,
// making section 3.1's leaf property executable instead of a
// review-time assertion.
func TestQualificationImportsMatchTheManifest(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	files := parseQualificationDirNonTestFiles(t, fset)

	checkQualificationImportsNamed(t, fset, files, qualificationImportManifest)
	checkQualificationManifestCurrent(t, files, qualificationImportManifest)
}

// qualificationImportsFakeReporter records Errorf calls instead of
// failing the enclosing test, so
// TestQualificationImportManifestGuardCatchesRealBreaks can drive
// TestQualificationImportsMatchTheManifest's own checks against
// deliberately-broken synthetic input without reddening this test
// file's own run.
type qualificationImportsFakeReporter struct {
	errors []string
}

func (f *qualificationImportsFakeReporter) Errorf(format string, _ ...any) {
	f.errors = append(f.errors, format)
}

// mustParseQualificationSrc parses src as a single fixture file under
// fset, using the same mode the real checks run under.
func mustParseQualificationSrc(t *testing.T, fset *token.FileSet, name, src string) qualificationParsedFile {
	t.Helper()
	file, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution|parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse fixture %s: %v", name, err)
	}
	return qualificationParsedFile{name: name, file: file}
}

// TestQualificationImportManifestGuardCatchesRealBreaks proves both
// directions TestQualificationImportsMatchTheManifest checks are
// themselves capable of failing, following the shape built for
// internal/qualification/e2e's own meta-test
// (TestHarnessImportManifestGuardCatchesRealBreaks). Direction 2 is
// inert against the real, empty qualificationImportManifest, so this
// test drives it with a non-empty synthetic manifest instead, proving
// the direction that never fires against the current tree is still
// able to fail.
func TestQualificationImportManifestGuardCatchesRealBreaks(t *testing.T) {
	t.Parallel()

	t.Run("any sortie import reports a failure against the empty manifest", func(t *testing.T) {
		t.Parallel()

		fset := token.NewFileSet()
		pf := mustParseQualificationSrc(t, fset, "fixture.go", `package qualification

import "github.com/sortie-ai/sortie/internal/domain"

var _ = domain.Issue{}
`)

		reporter := &qualificationImportsFakeReporter{}
		checkQualificationImportsNamed(reporter, fset, []qualificationParsedFile{pf}, qualificationImportManifest)

		if len(reporter.errors) == 0 {
			t.Fatalf("checkQualificationImportsNamed recorded no failure for a sortie import against the empty manifest, want at least one")
		}
	})

	t.Run("a synthetic manifest entry no file imports reports a failure", func(t *testing.T) {
		t.Parallel()

		fset := token.NewFileSet()
		pf := mustParseQualificationSrc(t, fset, "fixture.go", `package qualification

import "time"

var _ = time.Second
`)
		manifest := map[string]string{
			"github.com/sortie-ai/sortie/internal/domain": "not imported by any fixture file, deliberately stale",
		}

		reporter := &qualificationImportsFakeReporter{}
		checkQualificationManifestCurrent(reporter, []qualificationParsedFile{pf}, manifest)

		if len(reporter.errors) == 0 {
			t.Fatalf("checkQualificationManifestCurrent recorded no failure for a synthetic manifest entry no fixture file imports, want at least one")
		}
	})
}
