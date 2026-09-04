package clientprotocol

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"unicode"
)

// geminiIdentityToken is the one identity token this supplemental guard
// enforces: production code must not name the runtime this issue
// measures, in literals or in identifiers. Tests and operator
// documentation may.
const geminiIdentityToken = "gemini"

// geminiIdentityViolation is one place a scanned production file breaks
// the identity rule.
type geminiIdentityViolation struct {
	pos  token.Position
	text string
}

// geminiIdentityWordsFromIdent splits an identifier into lowercase words
// on every case transition and underscore, so GeminiCommand,
// GEMINI_CLI_HOME, and useGeminiFlag all expose the token.
func geminiIdentityWordsFromIdent(name string) []string {
	var words []string
	var current []rune
	flush := func() {
		if len(current) > 0 {
			words = append(words, strings.ToLower(string(current)))
			current = current[:0]
		}
	}
	runes := []rune(name)
	for i, r := range runes {
		if r == '_' {
			flush()
			continue
		}
		if i > 0 && unicode.IsUpper(r) && !unicode.IsUpper(runes[i-1]) {
			flush()
		}
		if i > 0 && unicode.IsUpper(runes[i-1]) && unicode.IsLower(r) && len(current) > 1 {
			last := current[len(current)-1]
			current = current[:len(current)-1]
			flush()
			current = append(current, last)
		}
		current = append(current, r)
	}
	flush()
	return words
}

// geminiIdentityWordsFromLiteral splits a string literal into lowercase
// words on every character that is neither a letter nor a digit.
func geminiIdentityWordsFromLiteral(value string) []string {
	return strings.FieldsFunc(value, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// geminiIdentityArm1Violations reports every string literal or identifier
// in file, outside an import declaration, whose word sequence carries the
// identity token.
func geminiIdentityArm1Violations(fset *token.FileSet, file *ast.File) []geminiIdentityViolation {
	var violations []geminiIdentityViolation
	report := func(pos token.Pos, kind, spelling string, words []string) {
		if slices.Contains(words, geminiIdentityToken) {
			violations = append(violations, geminiIdentityViolation{
				pos:  fset.Position(pos),
				text: kind + " " + spelling + " names the " + geminiIdentityToken + " identity token",
			})
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.ImportSpec:
			return false
		case *ast.BasicLit:
			if v.Kind != token.STRING {
				return true
			}
			report(v.Pos(), "string literal", v.Value, geminiIdentityWordsFromLiteral(v.Value))
		case *ast.Ident:
			report(v.Pos(), "identifier", v.Name, geminiIdentityWordsFromIdent(v.Name))
		}
		return true
	})
	return violations
}

// geminiIdentityExprContainsAgentInfo reports whether expr's syntax tree
// contains the identifier agentInfo, the recorded runtime identity.
func geminiIdentityExprContainsAgentInfo(expr ast.Expr) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if found {
			return false
		}
		if ident, ok := n.(*ast.Ident); ok && ident.Name == "agentInfo" {
			found = true
			return false
		}
		return true
	})
	return found
}

// geminiIdentityArm2Violations reports every equality or inequality
// comparison, switch tag, case expression, and map index in file whose
// operand contains agentInfo: the recorded runtime identity may be
// logged as evidence, but production behavior must not branch on it or
// on any identity proxy.
func geminiIdentityArm2Violations(fset *token.FileSet, file *ast.File) []geminiIdentityViolation {
	var violations []geminiIdentityViolation
	reject := func(pos token.Pos, what string) {
		violations = append(violations, geminiIdentityViolation{
			pos:  fset.Position(pos),
			text: what + " branches on agentInfo; branch on an advertised capability or an observed message shape instead",
		})
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.BinaryExpr:
			if (v.Op == token.EQL || v.Op == token.NEQ) &&
				(geminiIdentityExprContainsAgentInfo(v.X) || geminiIdentityExprContainsAgentInfo(v.Y)) {
				reject(v.Pos(), "an equality comparison")
			}
		case *ast.SwitchStmt:
			if v.Tag != nil && geminiIdentityExprContainsAgentInfo(v.Tag) {
				reject(v.Pos(), "a switch tag")
			}
		case *ast.CaseClause:
			for _, expr := range v.List {
				if geminiIdentityExprContainsAgentInfo(expr) {
					reject(expr.Pos(), "a case expression")
				}
			}
		case *ast.IndexExpr:
			if geminiIdentityExprContainsAgentInfo(v.Index) {
				reject(v.Pos(), "a map index")
			}
		}
		return true
	})
	return violations
}

// scanGeminiProductionIdentity walks the production non-test Go files
// under cmd/ and internal/ and applies both identity arms. Test files,
// testdata trees, and everything outside those two roots are excluded,
// so required Gemini references in tests and operator documentation stay
// legal.
func scanGeminiProductionIdentity(t *testing.T) ([]string, []geminiIdentityViolation) {
	t.Helper()

	fset := token.NewFileSet()
	var scanned []string
	var violations []geminiIdentityViolation

	for _, root := range []string{filepath.Join("..", "..", "..", "cmd"), filepath.Join("..", "..", "..", "internal")} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				if d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if parseErr != nil {
				t.Fatalf("parse %s: %v", path, parseErr)
			}
			scanned = append(scanned, path)
			violations = append(violations, geminiIdentityArm1Violations(fset, file)...)
			violations = append(violations, geminiIdentityArm2Violations(fset, file)...)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	return scanned, violations
}

// TestGeminiQualificationAddsNoProductionIdentityBranch scans the
// production non-test Go files under cmd/ and internal/ for Gemini
// identity literals and identity-proxy comparisons, proves the scan
// covers the production tree, and pins the scanner's own detection
// against inline fixtures so the guard cannot pass vacuously.
func TestGeminiQualificationAddsNoProductionIdentityBranch(t *testing.T) {
	t.Parallel()

	t.Run("production tree carries no gemini identity literal", func(t *testing.T) {
		t.Parallel()

		scanned, violations := scanGeminiProductionIdentity(t)
		if len(scanned) == 0 {
			t.Fatal("the scan covered no production files, want at least one")
		}
		for _, v := range violations {
			if strings.Contains(v.text, geminiIdentityToken+" identity token") {
				t.Errorf("%s: %s", v.pos, v.text)
			}
		}
	})

	t.Run("production tree carries no identity-proxy comparison", func(t *testing.T) {
		t.Parallel()

		_, violations := scanGeminiProductionIdentity(t)
		for _, v := range violations {
			if strings.Contains(v.text, "branches on agentInfo") {
				t.Errorf("%s: %s", v.pos, v.text)
			}
		}
	})

	t.Run("scan covers the production tree", func(t *testing.T) {
		t.Parallel()

		scanned, _ := scanGeminiProductionIdentity(t)
		for _, want := range []string{
			filepath.Join("cmd", "sortie", "main.go"),
			filepath.Join("internal", "agent", "clientprotocol", "pump.go"),
			filepath.Join("internal", "agent", "clientprotocol", "schemagen", "main.go"),
		} {
			found := slices.ContainsFunc(scanned, func(path string) bool {
				return strings.HasSuffix(path, want)
			})
			if !found {
				t.Errorf("scan missed the production file %s", want)
			}
		}

		_, thisFile, _, _ := runtime.Caller(0)
		if slices.Contains(scanned, filepath.Clean(thisFile)) {
			t.Error("the scan covered its own test file, want test files excluded")
		}
	})

	t.Run("scanner flags a gemini literal", func(t *testing.T) {
		t.Parallel()

		violations := geminiScanInline(t, `package fixture

func f() string { return "gemini --acp" }
`)
		if len(violations) != 1 {
			t.Fatalf("inline scan violations = %d, want 1: %v", len(violations), violations)
		}
		if !strings.Contains(violations[0].text, geminiIdentityToken+" identity token") {
			t.Errorf("violation text = %q, want it to name the identity token", violations[0].text)
		}
	})

	t.Run("scanner flags a gemini identifier word", func(t *testing.T) {
		t.Parallel()

		violations := geminiScanInline(t, `package fixture

func pickGeminiCommand() string { return "" }
`)
		if len(violations) != 1 {
			t.Fatalf("inline scan violations = %d, want 1: %v", len(violations), violations)
		}
	})

	t.Run("scanner flags an agentInfo comparison", func(t *testing.T) {
		t.Parallel()

		violations := geminiScanInline(t, `package fixture

func f(agentInfo string) bool {
	return agentInfo == "x"
}
`)
		if len(violations) != 1 {
			t.Fatalf("inline scan violations = %d, want 1: %v", len(violations), violations)
		}
		if !strings.Contains(violations[0].text, "branches on agentInfo") {
			t.Errorf("violation text = %q, want it to name the identity-proxy branch", violations[0].text)
		}
	})

	t.Run("scanner accepts the generic kind", func(t *testing.T) {
		t.Parallel()

		violations := geminiScanInline(t, `package fixture

const kind = "agent-client-protocol"

func f(agentInfo string) string {
	return agentInfo
}
`)
		if len(violations) != 0 {
			t.Errorf("inline scan violations = %v, want none for generic naming and unbranched identity logging", violations)
		}
	})
}

// geminiScanInline parses src as a single fixture file and returns the
// violations both identity arms report for it.
func geminiScanInline(t *testing.T, src string) []geminiIdentityViolation {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse inline fixture: %v", err)
	}
	violations := geminiIdentityArm1Violations(fset, file)
	violations = append(violations, geminiIdentityArm2Violations(fset, file)...)
	return violations
}
