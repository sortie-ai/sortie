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

// envSurfaceOwnedNames is the complete set of SORTIE_-prefixed
// environment variable names this package owns: the five qualification
// coordinates. A string literal in an environment-name position naming
// a value outside this set is a violation, whether the value sits
// outside the transport family entirely or inside it while still
// carrying a runtime token.
var envSurfaceOwnedNames = map[string]bool{
	"SORTIE_CLIENTPROTOCOL_QUALIFICATION_TEST":           true,
	"SORTIE_CLIENTPROTOCOL_QUALIFICATION_COMMAND":        true,
	"SORTIE_CLIENTPROTOCOL_QUALIFICATION_MODEL":          true,
	"SORTIE_CLIENTPROTOCOL_QUALIFICATION_AUTH_ENV_NAMES": true,
	"SORTIE_CLIENTPROTOCOL_QUALIFICATION_PROFILE":        true,
}

// envSurfaceCallSelectors are the selector names an environment-access
// call's first argument is checked under. Matching the selector alone,
// with no type resolution, covers os.Getenv, os.LookupEnv, os.Setenv,
// os.Unsetenv, and t.Setenv regardless of the receiver's static type.
var envSurfaceCallSelectors = map[string]bool{
	"Getenv":    true,
	"LookupEnv": true,
	"Setenv":    true,
	"Unsetenv":  true,
}

// envSurfaceViolation is one string literal found in an environment-name
// position whose value begins with SORTIE_ but is not a member of
// envSurfaceOwnedNames.
type envSurfaceViolation struct {
	pos     token.Position
	literal string
}

// envSurfaceViolations reports every violation in file's two
// environment-name positions: the value a declaration binds to a name,
// and the first argument of a call whose callee selects Getenv,
// LookupEnv, Setenv, or Unsetenv.
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

// envSurfaceLiterals returns every SORTIE_-prefixed string literal
// found anywhere in file, in either environment-name position or not,
// for the staleness-direction check: an owned name that appears
// nowhere in the package has moved out from under this allowlist.
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

// envSurfaceReporter is the subset of *testing.T
// checkEnvSurfaceOwnedNamesCurrent calls, factored out so it can be
// driven by a fake reporter against synthetic input.
type envSurfaceReporter interface {
	Errorf(format string, args ...any)
}

// checkEnvSurfaceOwnedNamesCurrent reports the staleness direction:
// an owned-set entry no literal in literals names. Against the real
// package and the real envSurfaceOwnedNames this is inert; a synthetic
// owned set naming a coordinate absent from a fixture literal set
// proves the direction can fail.
func checkEnvSurfaceOwnedNamesCurrent(r envSurfaceReporter, owned map[string]bool, literals map[string]bool) {
	for name := range owned {
		if !literals[name] {
			r.Errorf("owned name %q is not found as a literal anywhere in the package", name)
		}
	}
}

// envSurfaceFakeReporter records Errorf calls instead of failing the
// enclosing test.
type envSurfaceFakeReporter struct {
	errors []string
}

func (f *envSurfaceFakeReporter) Errorf(format string, _ ...any) {
	f.errors = append(f.errors, format)
}

// scanEnvSurface parses every .go file directly inside this package's
// own directory, test files included, and aggregates the violations
// envSurfaceViolations reports for each, and every SORTIE_-prefixed
// literal envSurfaceLiterals finds.
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
		for name := range envSurfaceLiterals(file) {
			literals[name] = true
		}
	}
	return violations, literals
}

// envSurfaceScanInline parses src as a single fixture file and returns
// the violations envSurfaceViolations reports for it.
func envSurfaceScanInline(t *testing.T, src string) []envSurfaceViolation {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse inline fixture: %v", err)
	}
	return envSurfaceViolations(fset, file)
}

// TestEnvSurfaceIsTransportNamed proves, by construction, that every
// SORTIE_-prefixed literal this package declares or hands to an
// environment call belongs to envSurfaceOwnedNames, that the check
// itself cannot pass vacuously, and that every owned-set entry is
// found as a literal somewhere in the package, so a coordinate that
// moves out of this package cannot leave a green allowlist behind.
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
