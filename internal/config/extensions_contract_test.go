package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// extensionsConfigImportPath is the import path the checker resolves
// the "config" package qualifier from, per file, so a call to
// ResolveAgentSettings is caught regardless of the local import alias.
const extensionsConfigImportPath = "github.com/sortie-ai/sortie/internal/config"

// extensionsX1Allowlist names the internal/config functions permitted
// to index the extensions field or an identifier named extensions.
// Membership is verified against the tree, not copied from the source
// spec: ExtensionSection, ExtensionValue, and SetExtensionSection own
// the field selector form (c.extensions[...]); mergeExtensionSection
// and resolveExtensionEnvRefs own the identifier form on a parameter
// named extensions; NewServiceConfig owns the identifier form on the
// local map it assembles before storing it in the field.
// lookupExtensionValue indexes only a local variable named current,
// never extensions itself, so it can never trip the rule below, but it
// stays listed because it is the function the field's own godoc names
// as an owner of kind-scoped reads.
var extensionsX1Allowlist = map[string]bool{
	"ExtensionSection":        true,
	"ExtensionValue":          true,
	"SetExtensionSection":     true,
	"mergeExtensionSection":   true,
	"lookupExtensionValue":    true,
	"resolveExtensionEnvRefs": true,
	"NewServiceConfig":        true,
}

// extensionsViolation describes one place a file breaks the closed
// access-path invariant this checker enforces.
type extensionsViolation struct {
	pos  token.Position
	text string
}

// resolveExtensionsImportName returns the local identifier a file binds
// to importPath, or "" when the file does not import it. It reads only
// the file's own import declarations, never assumes a literal package
// name, so an aliased import cannot evade the check.
func resolveExtensionsImportName(file *ast.File, importPath string) string {
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != importPath {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		segments := strings.Split(path, "/")
		return segments[len(segments)-1]
	}
	return ""
}

// isExtensionsIndexBase reports whether expr is the base of an index
// expression that X1 forbids outside the allow-list: the extensions
// field selector, or a bare identifier named extensions. Enclosing
// parentheses are stripped first, since the parser wraps a
// parenthesized expression in a node of its own and the shape the rule
// forbids is the same with or without them.
func isExtensionsIndexBase(expr ast.Expr) bool {
	switch e := ast.Unparen(expr).(type) {
	case *ast.Ident:
		return e.Name == "extensions"
	case *ast.SelectorExpr:
		return e.Sel.Name == "extensions"
	}
	return false
}

// isAgentKindSelector reports whether expr is a selector expression
// ending in Agent.Kind, the shape of the original defect spelled in
// the new API. Parentheses are stripped at both selector levels for
// the reason [isExtensionsIndexBase] gives.
func isAgentKindSelector(expr ast.Expr) bool {
	sel, ok := ast.Unparen(expr).(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Kind" {
		return false
	}
	inner, ok := ast.Unparen(sel.X).(*ast.SelectorExpr)
	return ok && inner.Sel.Name == "Agent"
}

// checkExtensionsX1 appends a violation for every index expression
// inside file whose base is the extensions field selector or an
// identifier named extensions, in a function outside
// extensionsX1Allowlist. The caller restricts this check to
// internal/config's own non-test files, per X1's scope.
func checkExtensionsX1(fset *token.FileSet, file *ast.File) []extensionsViolation {
	var violations []extensionsViolation
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil || extensionsX1Allowlist[fn.Name.Name] {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			idx, ok := n.(*ast.IndexExpr)
			if !ok || !isExtensionsIndexBase(idx.X) {
				return true
			}
			violations = append(violations, extensionsViolation{
				pos:  fset.Position(idx.Pos()),
				text: "func " + fn.Name.Name + " indexes extensions directly outside the allow-list",
			})
			return true
		})
	}
	return violations
}

// checkExtensionsX2X3 appends a violation for every call inside file
// that breaks X2 (a non-literal argument to ExtensionSection or
// ExtensionValue) or X3 (a ResolveAgentSettings call whose kind
// argument is a selector ending in Agent.Kind). Both rules apply
// anywhere in the two walked roots, not only inside internal/config.
func checkExtensionsX2X3(fset *token.FileSet, file *ast.File) []extensionsViolation {
	configIdent := resolveExtensionsImportName(file, extensionsConfigImportPath)

	var violations []extensionsViolation
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		switch fun := ast.Unparen(call.Fun).(type) {
		case *ast.SelectorExpr:
			switch fun.Sel.Name {
			case "ExtensionSection", "ExtensionValue":
				if len(call.Args) != 1 {
					return true
				}
				if lit, ok := ast.Unparen(call.Args[0]).(*ast.BasicLit); ok && lit.Kind == token.STRING {
					return true
				}
				violations = append(violations, extensionsViolation{
					pos:  fset.Position(call.Pos()),
					text: fun.Sel.Name + " called with a non-literal name argument",
				})
			case "ResolveAgentSettings":
				ident, ok := ast.Unparen(fun.X).(*ast.Ident)
				if !ok || configIdent == "" || ident.Name != configIdent {
					return true
				}
				violations = append(violations, checkResolveAgentSettingsKind(fset, call)...)
			}
		case *ast.Ident:
			if fun.Name == "ResolveAgentSettings" {
				violations = append(violations, checkResolveAgentSettingsKind(fset, call)...)
			}
		}
		return true
	})
	return violations
}

// checkResolveAgentSettingsKind reports X3 for a single
// ResolveAgentSettings call whose kind argument (the second parameter)
// is a selector expression ending in Agent.Kind.
func checkResolveAgentSettingsKind(fset *token.FileSet, call *ast.CallExpr) []extensionsViolation {
	if len(call.Args) < 2 || !isAgentKindSelector(call.Args[1]) {
		return nil
	}
	return []extensionsViolation{{
		pos:  fset.Position(call.Pos()),
		text: "ResolveAgentSettings called with a selector ending in Agent.Kind instead of the session's frozen kind",
	}}
}

// TestExtensionsContract walks the non-test Go files under internal/
// and cmd/, skipping testdata directories, and fails when any file
// breaks the closed access-path invariant this work establishes: a
// direct index of the extensions map outside internal/config's own
// allow-listed owner functions (X1), a call to ExtensionSection or
// ExtensionValue whose section name is not a string literal (X2), or a
// call to ResolveAgentSettings whose kind argument reaches for the
// configuration's default kind instead of the session's frozen one
// (X3).
func TestExtensionsContract(t *testing.T) {
	fset := token.NewFileSet()
	internalConfigDir := filepath.Join("..", "config")

	roots := []string{
		filepath.Join(".."),
		filepath.Join("..", "..", "cmd"),
	}

	for _, root := range roots {
		parsed := 0
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}

			isTestFile := strings.HasSuffix(path, "_test.go")
			mode := parser.SkipObjectResolution
			if isTestFile {
				mode |= parser.ImportsOnly
			}
			file, parseErr := parser.ParseFile(fset, path, nil, mode)
			if parseErr != nil {
				t.Errorf("parse %s: %v", path, parseErr)
				return nil
			}
			parsed++
			if isTestFile {
				return nil
			}

			var violations []extensionsViolation
			if filepath.Dir(path) == internalConfigDir {
				violations = append(violations, checkExtensionsX1(fset, file)...)
			}
			violations = append(violations, checkExtensionsX2X3(fset, file)...)
			for _, v := range violations {
				t.Errorf("%s: %s", v.pos, v.text)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
		if parsed == 0 {
			t.Fatalf("root %s yielded no parsed Go files, want at least one", root)
		}
	}
}

// TestExtensionsContract_DetectsViolations pins the checker's own logic
// against inline source fixtures, independent of the current state of
// any production file, so a regression in a rule is caught even when
// every real caller happens to comply.
func TestExtensionsContract_DetectsViolations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		src       string
		wantCount int
	}{
		{
			name: "X1: a function outside the allow-list indexes extensions directly",
			src: `package fixture

func leakExtensions(extensions map[string]any) any {
	return extensions["worker"]
}
`,
			wantCount: 1,
		},
		{
			name: "X2: ExtensionSection called with a variable argument instead of a string literal",
			src: `package fixture

func readSection(cfg ServiceConfig, name string) map[string]any {
	return cfg.ExtensionSection(name)
}
`,
			wantCount: 1,
		},
		{
			name: "X3: ResolveAgentSettings called with a selector ending in Agent.Kind",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/config"

func resolve(cfg ServiceConfig, dir string) config.AgentSettings {
	return config.ResolveAgentSettings(cfg, cfg.Agent.Kind, dir)
}
`,
			wantCount: 1,
		},
		{
			name: "parentheses do not hide any of the three shapes",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/config"

func leakExtensions(cfg ServiceConfig) any {
	return (cfg.extensions)["worker"]
}

func readSection(cfg ServiceConfig, name string) map[string]any {
	return (cfg.ExtensionSection)(name)
}

func resolve(cfg ServiceConfig, dir string) config.AgentSettings {
	return config.ResolveAgentSettings(cfg, (cfg.Agent).Kind, dir)
}
`,
			wantCount: 3,
		},
		{
			name: "a parenthesized string literal is still a literal section name",
			src: `package fixture

func readSection(cfg ServiceConfig) map[string]any {
	return cfg.ExtensionSection(("worker"))
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

			got := checkExtensionsX1(fset, file)
			got = append(got, checkExtensionsX2X3(fset, file)...)
			if len(got) != tt.wantCount {
				t.Errorf("got %d violations, want %d: %+v", len(got), tt.wantCount, got)
			}
		})
	}
}
