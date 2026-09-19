package probe

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// Literals in this file are excluded from the staleness scan: it spells
// every owned name out, so counting them would satisfy the check from the
// allowlist itself even after a coordinate was deleted from the package.
const envSurfaceAllowlistFile = "env_surface_test.go"

var envSurfaceOwnedNames = map[string]bool{
	"SORTIE_CLIENTPROTOCOL_QUALIFICATION_TEST":           true,
	"SORTIE_CLIENTPROTOCOL_QUALIFICATION_COMMAND":        true,
	"SORTIE_CLIENTPROTOCOL_QUALIFICATION_MODEL":          true,
	"SORTIE_CLIENTPROTOCOL_QUALIFICATION_AUTH_ENV_NAMES": true,
	"SORTIE_CLIENTPROTOCOL_QUALIFICATION_PROFILE":        true,
}

// Matching the selector name alone, with no type resolution, covers
// os.Getenv/LookupEnv/Setenv/Unsetenv and t.Setenv regardless of receiver.
var envSurfaceCallSelectors = map[string]bool{
	"Getenv":    true,
	"LookupEnv": true,
	"Setenv":    true,
	"Unsetenv":  true,
}

type envSurfaceViolation struct {
	pos     token.Position
	literal string
}

func envSurfaceViolations(fset *token.FileSet, file *ast.File) []envSurfaceViolation {
	var violations []envSurfaceViolation
	report := func(lit *ast.BasicLit) {
		if lit.Kind != token.STRING {
			return
		}
		value, err := strconv.Unquote(lit.Value)
		if err != nil {
			return
		}
		if !strings.HasPrefix(value, "SORTIE_") || envSurfaceOwnedNames[value] {
			return
		}
		violations = append(violations, envSurfaceViolation{
			pos:     fset.Position(lit.Pos()),
			literal: value,
		})
	}

	reportBound := func(values []ast.Expr) {
		for _, value := range values {
			switch expr := value.(type) {
			case *ast.BasicLit:
				report(expr)
			case *ast.CompositeLit:
				for _, elt := range expr.Elts {
					if lit, ok := elt.(*ast.BasicLit); ok {
						report(lit)
					}
				}
			}
		}
	}

	ast.Inspect(file, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.ValueSpec:
			reportBound(v.Values)
		case *ast.AssignStmt:
			if v.Tok == token.DEFINE || v.Tok == token.ASSIGN {
				reportBound(v.Rhs)
			}
		case *ast.CallExpr:
			sel, ok := v.Fun.(*ast.SelectorExpr)
			if !ok || !envSurfaceCallSelectors[sel.Sel.Name] || len(v.Args) == 0 {
				return true
			}
			if lit, ok := v.Args[0].(*ast.BasicLit); ok {
				report(lit)
			}
		}
		return true
	})
	return violations
}

func envSurfaceLiterals(file *ast.File) map[string]bool {
	found := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		if strings.HasPrefix(value, "SORTIE_") {
			found[value] = true
		}
		return true
	})
	return found
}

type envSurfaceReporter interface {
	Errorf(format string, args ...any)
}

func checkEnvSurfaceOwnedNamesCurrent(r envSurfaceReporter, owned map[string]bool, literals map[string]bool) {
	for name := range owned {
		if !literals[name] {
			r.Errorf("owned name %q is not found as a literal anywhere in the package", name)
		}
	}
}

type envSurfaceFakeReporter struct {
	errors []string
}

func (f *envSurfaceFakeReporter) Errorf(format string, _ ...any) {
	f.errors = append(f.errors, format)
}

func scanEnvSurface(t *testing.T) ([]envSurfaceViolation, map[string]bool) {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}

	fset := token.NewFileSet()
	var violations []envSurfaceViolation
	literals := map[string]bool{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, entry.Name(), nil, parser.SkipObjectResolution)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", entry.Name(), parseErr)
		}
		violations = append(violations, envSurfaceViolations(fset, file)...)
		if entry.Name() == envSurfaceAllowlistFile {
			continue
		}
		for name := range envSurfaceLiterals(file) {
			literals[name] = true
		}
	}
	return violations, literals
}

func envSurfaceScanInline(t *testing.T, src string) []envSurfaceViolation {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse inline fixture: %v", err)
	}
	return envSurfaceViolations(fset, file)
}

func TestEnvSurfaceIsTransportNamed(t *testing.T) {
	t.Parallel()

	t.Run("package directory carries no non-owned SORTIE_ literal", func(t *testing.T) {
		t.Parallel()

		violations, _ := scanEnvSurface(t)
		for _, v := range violations {
			t.Errorf("%s: %q is not a member of envSurfaceOwnedNames", v.pos, v.literal)
		}
	})

	t.Run("every owned name is found as a literal somewhere in the package", func(t *testing.T) {
		t.Parallel()

		_, literals := scanEnvSurface(t)
		reporter := &envSurfaceFakeReporter{}
		checkEnvSurfaceOwnedNamesCurrent(reporter, envSurfaceOwnedNames, literals)
		for _, msg := range reporter.errors {
			t.Error(msg)
		}
	})

	t.Run("a synthetic owned-set entry with no matching literal fails the staleness direction", func(t *testing.T) {
		t.Parallel()

		synthetic := map[string]bool{"SORTIE_CLIENTPROTOCOL_QUALIFICATION_NOT_A_REAL_COORDINATE": true}
		fixtureLiterals := map[string]bool{"SORTIE_CLIENTPROTOCOL_QUALIFICATION_TEST": true}

		reporter := &envSurfaceFakeReporter{}
		checkEnvSurfaceOwnedNamesCurrent(reporter, synthetic, fixtureLiterals)
		if len(reporter.errors) == 0 {
			t.Fatal("checkEnvSurfaceOwnedNamesCurrent recorded no failure for a synthetic owned-set entry absent from the literal set, want at least one")
		}
	})

	t.Run("a call outside the family reports one violation", func(t *testing.T) {
		t.Parallel()

		violations := envSurfaceScanInline(t, `package fixture

import "os"

func f() string {
	v, _ := os.LookupEnv("SORTIE_QWEN_TEST")
	return v
}
`)
		if len(violations) != 1 {
			t.Fatalf("violations = %d, want 1: %v", len(violations), violations)
		}
	})

	t.Run("a declaration inside the family but outside ownedNames reports one violation", func(t *testing.T) {
		t.Parallel()

		violations := envSurfaceScanInline(t, `package fixture

const geminiGateEnv = "SORTIE_CLIENTPROTOCOL_QUALIFICATION_GEMINI_TEST"
`)
		if len(violations) != 1 {
			t.Fatalf("violations = %d, want 1: %v", len(violations), violations)
		}
	})

	t.Run("a bare sentinel compared outside a declaration or call reports none", func(t *testing.T) {
		t.Parallel()

		violations := envSurfaceScanInline(t, `package fixture

import "strings"

const prompt = "Reply with the token SORTIE_AUTH_OK when finished."

func f(output string) bool {
	return strings.Contains(output, "SORTIE_AUTH_OK")
}
`)
		if len(violations) != 0 {
			t.Errorf("violations = %v, want none", violations)
		}
	})
}
