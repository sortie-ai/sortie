package evidence

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// evidenceImportManifest is empty by design: tools/qualify/evidence stays a
// leaf so importers do not inherit any other tool-module package.
var evidenceImportManifest = map[string]string{}

const evidenceSortieImportPrefix = "github.com/sortie-ai/sortie/"

type evidenceImportsReporter interface {
	Errorf(format string, args ...any)
}

type evidenceParsedFile struct {
	name string
	file *ast.File
}

func evidenceFileImports(pf evidenceParsedFile) []string {
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

func checkEvidenceImportsNamed(r evidenceImportsReporter, fset *token.FileSet, files []evidenceParsedFile, manifest map[string]string) {
	for _, pf := range files {
		for _, imp := range pf.file.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			if !strings.HasPrefix(path, evidenceSortieImportPrefix) {
				continue
			}
			if _, ok := manifest[path]; !ok {
				r.Errorf("%s: %s imports %q, which evidenceImportManifest does not name", fset.Position(imp.Pos()), pf.name, path)
			}
		}
	}
}

func checkEvidenceManifestCurrent(r evidenceImportsReporter, files []evidenceParsedFile, manifest map[string]string) {
	imported := map[string]bool{}
	for _, pf := range files {
		for _, path := range evidenceFileImports(pf) {
			imported[path] = true
		}
	}
	for path := range manifest {
		if !imported[path] {
			r.Errorf("evidenceImportManifest names %q, but no parsed non-test file imports it", path)
		}
	}
}

// parseEvidenceDirNonTestFiles excludes _test.go files: test files are not
// part of the package's import graph as its importers see it.
func parseEvidenceDirNonTestFiles(t *testing.T, fset *token.FileSet) []evidenceParsedFile {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}

	var files []evidenceParsedFile
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution|parser.ImportsOnly)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", name, parseErr)
		}
		files = append(files, evidenceParsedFile{name: name, file: file})
	}
	if len(files) == 0 {
		t.Fatalf("package directory yielded no parsed non-test Go file, want at least one")
	}
	return files
}

func TestEvidenceImportsMatchTheManifest(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	files := parseEvidenceDirNonTestFiles(t, fset)

	checkEvidenceImportsNamed(t, fset, files, evidenceImportManifest)
	checkEvidenceManifestCurrent(t, files, evidenceImportManifest)
}

type evidenceImportsFakeReporter struct {
	errors []string
}

func (f *evidenceImportsFakeReporter) Errorf(format string, _ ...any) {
	f.errors = append(f.errors, format)
}

func mustParseEvidenceSrc(t *testing.T, fset *token.FileSet, name, src string) evidenceParsedFile {
	t.Helper()
	file, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution|parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse fixture %s: %v", name, err)
	}
	return evidenceParsedFile{name: name, file: file}
}

// TestEvidenceImportManifestGuardCatchesRealBreaks proves the guard can
// fail before trusting that it passes against the real, empty manifest.
func TestEvidenceImportManifestGuardCatchesRealBreaks(t *testing.T) {
	t.Parallel()

	t.Run("any sortie import reports a failure against the empty manifest", func(t *testing.T) {
		t.Parallel()

		fset := token.NewFileSet()
		pf := mustParseEvidenceSrc(t, fset, "fixture.go", `package evidence

import "github.com/sortie-ai/sortie/tools/qualify/profile"

var _ = profile.RuntimeProfile{}
`)

		reporter := &evidenceImportsFakeReporter{}
		checkEvidenceImportsNamed(reporter, fset, []evidenceParsedFile{pf}, evidenceImportManifest)

		if len(reporter.errors) == 0 {
			t.Fatalf("checkEvidenceImportsNamed recorded no failure for a sortie import against the empty manifest, want at least one")
		}
	})

	t.Run("a synthetic manifest entry no file imports reports a failure", func(t *testing.T) {
		t.Parallel()

		fset := token.NewFileSet()
		pf := mustParseEvidenceSrc(t, fset, "fixture.go", `package evidence

import "time"

var _ = time.Second
`)
		manifest := map[string]string{
			"github.com/sortie-ai/sortie/tools/qualify/profile": "not imported by any fixture file, deliberately stale",
		}

		reporter := &evidenceImportsFakeReporter{}
		checkEvidenceManifestCurrent(reporter, []evidenceParsedFile{pf}, manifest)

		if len(reporter.errors) == 0 {
			t.Fatalf("checkEvidenceManifestCurrent recorded no failure for a synthetic manifest entry no fixture file imports, want at least one")
		}
	})
}
