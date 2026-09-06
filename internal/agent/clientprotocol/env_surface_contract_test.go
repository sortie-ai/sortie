package clientprotocol

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
// environment variable names this package owns. A string literal in an
// environment-name position naming a value outside this set is a
// violation, whether the value sits outside the transport family
// entirely or inside it while still carrying a runtime token.
var envSurfaceOwnedNames = map[string]bool{
	"SORTIE_CLIENTPROTOCOL_TEST":                         true,
	"SORTIE_CLIENTPROTOCOL_COMMAND":                      true,
	"SORTIE_CLIENTPROTOCOL_QUALIFICATION_TEST":           true,
	"SORTIE_CLIENTPROTOCOL_QUALIFICATION_COMMAND":        true,
	"SORTIE_CLIENTPROTOCOL_QUALIFICATION_MODEL":          true,
	"SORTIE_CLIENTPROTOCOL_QUALIFICATION_AUTH_ENV_NAMES": true,
	"SORTIE_CLIENTPROTOCOL_QUALIFICATION_DECLARED_GAPS":  true,
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
// LookupEnv, Setenv, or Unsetenv. The first position covers a const or
// var specification, a short variable declaration, and an assignment
// alike, including each string element of a slice literal in any of
// them, because a name's spelling is what the rule is about and the
// syntax that binds it is not. A literal elsewhere in the file, however
// it spells a SORTIE_ name, is not inspected.
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

	// reportBound inspects the values a declaration binds, and only
	// those written directly as a literal there. It never descends into
	// a call on the right-hand side, whose arguments are the other
	// position's business.
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

// scanEnvSurface parses every .go file directly inside this package's
// own directory, test files included, and aggregates the violations
// envSurfaceViolations reports for each. It does not descend into a
// subdirectory.
func scanEnvSurface(t *testing.T) []envSurfaceViolation {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}

	fset := token.NewFileSet()
	var violations []envSurfaceViolation
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, entry.Name(), nil, parser.SkipObjectResolution)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", entry.Name(), parseErr)
		}
		violations = append(violations, envSurfaceViolations(fset, file)...)
	}
	return violations
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
// environment call belongs to envSurfaceOwnedNames, and that the check
// itself cannot pass vacuously.
//
// The guard scans this package's own directory only; a second runtime's
// profile placed in a sibling package passes unseen. It reads a literal
// only where that literal is written directly into a value a
// declaration binds, whether a const or var specification, a short
// variable declaration or an assignment, or into a slice literal in
// any of those, or passed as a literal to an os or testing environment
// call; a name assembled by
// concatenation, or handed as a bare literal to a lookup function this
// package injects rather than to os or testing directly, escapes both
// positions and is not caught. It does not catch a stale literal from a
// prior naming family sitting outside those two positions, such as a
// map key or an error-message assertion that quotes the old name as
// plain text; a repository-wide text search is what catches those. And
// it mechanizes only two of three ways a runtime name could appear in
// this surface, the name or value of a declared variable and the
// identifier holding one; it does not mechanize the third, a runtime
// name written into an operator-facing message that also carries an
// owned name, which stays a matter for a separate pinned-text
// assertion.
func TestEnvSurfaceIsTransportNamed(t *testing.T) {
	t.Parallel()

	t.Run("package directory carries no non-owned SORTIE_ literal", func(t *testing.T) {
		t.Parallel()

		for _, v := range scanEnvSurface(t) {
			t.Errorf("%s: %q is not a member of envSurfaceOwnedNames", v.pos, v.literal)
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

	t.Run("a declaration outside the family reports one violation", func(t *testing.T) {
		t.Parallel()

		violations := envSurfaceScanInline(t, `package fixture

const qwenGateEnv = "SORTIE_QWEN_TEST"
`)
		if len(violations) != 1 {
			t.Fatalf("violations = %d, want 1: %v", len(violations), violations)
		}
	})

	t.Run("a declaration inside the family but outside ownedNames reports one violation", func(t *testing.T) {
		t.Parallel()

		violations := envSurfaceScanInline(t, `package fixture

const geminiGateEnv = "SORTIE_CLIENTPROTOCOL_GEMINI_TEST"
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

	t.Run("a short variable declaration outside the family reports one violation", func(t *testing.T) {
		t.Parallel()

		violations := envSurfaceScanInline(t, `package fixture

import "os"

func f() string {
	name := "SORTIE_QWEN_TEST"
	return os.Getenv(name)
}
`)
		if len(violations) != 1 {
			t.Fatalf("violations = %d, want 1: %v", len(violations), violations)
		}
	})

	t.Run("an assignment outside the family reports one violation", func(t *testing.T) {
		t.Parallel()

		violations := envSurfaceScanInline(t, `package fixture

var name string

func f() {
	name = "SORTIE_QWEN_TEST"
}
`)
		if len(violations) != 1 {
			t.Fatalf("violations = %d, want 1: %v", len(violations), violations)
		}
	})

	t.Run("a call on a declaration's right-hand side is not a bound value", func(t *testing.T) {
		t.Parallel()

		violations := envSurfaceScanInline(t, `package fixture

func helper(src string) string { return src }

func f() string {
	v := helper("SORTIE_QWEN_TEST")
	return v
}
`)
		if len(violations) != 0 {
			t.Errorf("violations = %v, want none", violations)
		}
	})

	t.Run("a declared value outside the family reports one violation regardless of the identifier's spelling", func(t *testing.T) {
		t.Parallel()

		violations := envSurfaceScanInline(t, `package fixture

const qwenGate = "SORTIE_QWEN_TEST"
`)
		if len(violations) != 1 {
			t.Fatalf("violations = %d, want 1: %v", len(violations), violations)
		}
	})
}
