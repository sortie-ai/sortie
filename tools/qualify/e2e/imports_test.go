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

// importManifest freezes the harness's github.com/sortie-ai/sortie
// dependency set; the prefix covers both root-module packages and this
// module's own tool-module siblings.
var importManifest = map[string]string{
	"github.com/sortie-ai/sortie/internal/workspace":      "computing the isolated git workspace root the fixture tracker uses",
	"github.com/sortie-ai/sortie/internal/agent/procutil": "starting the fake agent's process group and signaling it",
	"github.com/sortie-ai/sortie/internal/config":         "assembling the ServiceConfig the harness's orchestrator runs",
	"github.com/sortie-ai/sortie/internal/domain":         "the AgentAdapter interface and the session/turn types the fake agent implements",
	"github.com/sortie-ai/sortie/internal/orchestrator":   "the real orchestrator the harness drives end to end",
	"github.com/sortie-ai/sortie/internal/persistence":    "opening and migrating the store the orchestrator runs against",
	"github.com/sortie-ai/sortie/internal/prompt":         "parsing the fixture's workflow prompt template",
	"github.com/sortie-ai/sortie/internal/registry":       "registering the fixture agent kind the orchestrator's preflight resolves",
	"github.com/sortie-ai/sortie/internal/tracker/file":   "the file tracker adapter the harness's fixture drives",

	"github.com/sortie-ai/sortie/tools/qualify/procgroup":    "the promoted shutdown bound and process-group primitives",
	"github.com/sortie-ai/sortie/tools/qualify/evidence":     "the record vocabulary the harness's terminal-condition record uses",
	"github.com/sortie-ai/sortie/tools/qualify/evidencetest": "the synthetic fixture timestamp the harness's terminal-condition record carries",
}

const sortieImportPrefix = "github.com/sortie-ai/sortie/"

type harnessImportsReporter interface {
	Errorf(format string, args ...any)
}

type harnessParsedFile struct {
	name string
	file *ast.File
}

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

// harnessParseMode must keep ParseComments: without it ast.File.Comments is
// empty and every //go:build line is invisible to fileHasBuildConstraint.
const harnessParseMode = parser.SkipObjectResolution | parser.ImportsOnly | parser.ParseComments

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

func TestHarnessImportsMatchTheManifest(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	files := parseHarnessDirNonTestFiles(t, fset)

	checkHarnessImportsNamed(t, fset, files, importManifest)
	checkHarnessManifestCurrent(t, files, importManifest)
	checkHarnessAtLeastOneUntagged(t, files)
}

type harnessImportsFakeReporter struct {
	errors []string
}

func (f *harnessImportsFakeReporter) Errorf(format string, _ ...any) {
	f.errors = append(f.errors, format)
}

func mustParseHarnessSrc(t *testing.T, fset *token.FileSet, name, src string) harnessParsedFile {
	t.Helper()
	file, err := parser.ParseFile(fset, name, src, harnessParseMode)
	if err != nil {
		t.Fatalf("parse fixture %s: %v", name, err)
	}
	return harnessParsedFile{name: name, file: file}
}

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

	t.Run("an import of a tool-module sibling outside the manifest reports a failure", func(t *testing.T) {
		t.Parallel()

		fset := token.NewFileSet()
		pf := mustParseHarnessSrc(t, fset, "fixture.go", `package e2e

import "github.com/sortie-ai/sortie/tools/qualify/eval"

var _ = eval.Input{}
`)

		reporter := &harnessImportsFakeReporter{}
		checkHarnessImportsNamed(reporter, fset, []harnessParsedFile{pf}, importManifest)

		if len(reporter.errors) == 0 {
			t.Fatalf("checkHarnessImportsNamed recorded no failure for a tool-module import importManifest does not name, want at least one")
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
