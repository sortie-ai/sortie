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

// qualificationImportManifest names every permitted
// github.com/sortie-ai/sortie import of this package's non-test files. It is
// empty by design: internal/qualification stays a leaf so importers do not
// inherit the orchestrator, the store, or any adapter family. A future entry
// is policed in both directions from its first commit.
var qualificationImportManifest = map[string]string{}

const qualificationSortieImportPrefix = "github.com/sortie-ai/sortie/internal/"

type qualificationImportsReporter interface {
	Errorf(format string, args ...any)
}

type qualificationParsedFile struct {
	name string
	file *ast.File
}

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

// parseQualificationDirNonTestFiles excludes _test.go files on purpose: test
// files are not part of the package's import graph as its importers see it,
// so this package's own tests may import procutil and agenttest without
// tripping the guard.
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

func TestQualificationImportsMatchTheManifest(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	files := parseQualificationDirNonTestFiles(t, fset)

	checkQualificationImportsNamed(t, fset, files, qualificationImportManifest)
	checkQualificationManifestCurrent(t, files, qualificationImportManifest)
}

type qualificationImportsFakeReporter struct {
	errors []string
}

func (f *qualificationImportsFakeReporter) Errorf(format string, _ ...any) {
	f.errors = append(f.errors, format)
}

func mustParseQualificationSrc(t *testing.T, fset *token.FileSet, name, src string) qualificationParsedFile {
	t.Helper()
	file, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution|parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse fixture %s: %v", name, err)
	}
	return qualificationParsedFile{name: name, file: file}
}

// TestQualificationImportManifestGuardCatchesRealBreaks drives direction 2
// with a non-empty synthetic manifest because it is inert against the real,
// empty one, proving the direction that never fires against the current tree
// can still fail.
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
