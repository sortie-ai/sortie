package adaptertest

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// toolModuleImportPrefix names the nested maintainer-tooling module. No root
// module file may import a package under it: the module boundary is a
// compiler-enforced one-way dependency (the tool module depends on the root
// module via a replace directive, never the reverse), and this guard
// verifies the source rather than relying on the absence of a go.work file on
// the machine running it.
const toolModuleImportPrefix = "github.com/sortie-ai/sortie/tools/qualify"

// toolModuleWalkExcludedDir names a directory this walk never descends into.
// "tools/qualify" is a separate Go module; reading its source through
// parser.ParseFile is not an import and must not be conflated with one.
// "testdata" carries no Go source relevant to import graphs.
func toolModuleWalkExcludedDir(name, relPath string) bool {
	return name == "testdata" || name == ".git" || relPath == filepath.Join("tools", "qualify")
}

type toolModuleImportsReporter interface {
	Errorf(format string, args ...any)
}

// checkNoToolModuleImports parses every .go file (test and non-test) under
// root, excluding the nested tool module, and reports any import path
// carrying toolModuleImportPrefix.
func checkNoToolModuleImports(r toolModuleImportsReporter, fset *token.FileSet, root string) int {
	parsed := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			rel, relErr := filepath.Rel(root, path)
			if relErr == nil && toolModuleWalkExcludedDir(d.Name(), rel) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution|parser.ImportsOnly)
		if parseErr != nil {
			r.Errorf("parse %s: %v", path, parseErr)
			return nil
		}
		parsed++

		for _, imp := range file.Imports {
			importPath, unquoteErr := strconv.Unquote(imp.Path.Value)
			if unquoteErr != nil {
				continue
			}
			if importPath == toolModuleImportPrefix || strings.HasPrefix(importPath, toolModuleImportPrefix+"/") {
				r.Errorf("%s: %s imports %q, a package under the nested tool module; the root module MUST NOT depend on it", fset.Position(imp.Pos()), path, importPath)
			}
		}
		return nil
	})
	if err != nil {
		r.Errorf("walk %s: %v", root, err)
	}
	return parsed
}

// TestNoRootModuleImportOfToolModule asserts that no file in the root
// module imports any tools/qualify package. Verified by inspecting import
// declarations directly, so the check holds regardless of whether a go.work
// file exists on the machine running it.
func TestNoRootModuleImportOfToolModule(t *testing.T) {
	fset := token.NewFileSet()
	parsed := checkNoToolModuleImports(t, fset, filepath.Join("..", ".."))
	if parsed == 0 {
		t.Fatal("checkNoToolModuleImports walked zero Go files, want at least one")
	}
}

type toolModuleImportsFakeReporter struct {
	errors []string
}

func (f *toolModuleImportsFakeReporter) Errorf(format string, _ ...any) {
	f.errors = append(f.errors, format)
}

// TestNoRootModuleImportOfToolModule_CatchesRealBreaks proves the guard can
// fail: it walks a scratch directory carrying a file that imports the tool
// module, inert against the real tree, which never does.
func TestNoRootModuleImportOfToolModule_CatchesRealBreaks(t *testing.T) {
	dir := t.TempDir()
	src := `package fixture

import "github.com/sortie-ai/sortie/tools/qualify/evidence"

var _ = evidence.Verdict("")
`
	if _, err := parser.ParseFile(token.NewFileSet(), "fixture.go", src, parser.SkipObjectResolution); err != nil {
		t.Fatalf("fixture does not parse: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fixture.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}

	fset := token.NewFileSet()
	reporter := &toolModuleImportsFakeReporter{}
	parsed := checkNoToolModuleImports(reporter, fset, dir)

	if parsed == 0 {
		t.Fatal("checkNoToolModuleImports walked zero Go files in the fixture directory, want one")
	}
	if len(reporter.errors) == 0 {
		t.Fatal("checkNoToolModuleImports recorded no failure for a fixture file importing the tool module, want at least one")
	}
}
