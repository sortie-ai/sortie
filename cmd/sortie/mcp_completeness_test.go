package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/registry"
)

// mcpCompletenessAgentRoot is the directory holding one package
// subdirectory per agent kind, relative to this package's own
// directory.
const mcpCompletenessAgentRoot = "../../internal/agent"

// mcpCompletenessRegistryImportPath is the import path the directory
// discovery step resolves the "registry" package qualifier from, per
// file, rather than assuming the literal identifier "registry".
const mcpCompletenessRegistryImportPath = "github.com/sortie-ai/sortie/internal/registry"

// mcpCompletenessAgenttestImportPath is the import path a kind's test
// file must import to call the shared conformance assertion.
const mcpCompletenessAgenttestImportPath = "github.com/sortie-ai/sortie/internal/agent/agenttest"

// mcpCompletenessAssertFuncName is the exported function name a
// kind's test files must call at least once.
const mcpCompletenessAssertFuncName = "AssertMCPInjection"

// resolveTestImportName returns the local identifier file binds to
// importPath, or "" when file does not import it. It reads only the
// file's own import declarations, so an aliased import is resolved
// correctly rather than assumed to keep its package name.
func resolveTestImportName(file *ast.File, importPath string) string {
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

// registeredKindInFile returns the kind string a
// registry.Agents.Register or registry.Agents.RegisterWithMeta call
// in file names as its first argument, or "" when file contains
// neither call. The registry qualifier is resolved from file's own
// imports rather than assumed literal.
func registeredKindInFile(file *ast.File) string {
	alias := resolveTestImportName(file, mcpCompletenessRegistryImportPath)
	if alias == "" {
		return ""
	}

	kind := ""
	ast.Inspect(file, func(n ast.Node) bool {
		if kind != "" {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		outer, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		inner, ok := outer.X.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := inner.X.(*ast.Ident)
		if !ok || ident.Name != alias || inner.Sel.Name != "Agents" {
			return true
		}
		if outer.Sel.Name != "Register" && outer.Sel.Name != "RegisterWithMeta" {
			return true
		}
		if len(call.Args) == 0 {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		unquoted, unquoteErr := strconv.Unquote(lit.Value)
		if unquoteErr != nil {
			return true
		}
		kind = unquoted
		return false
	})
	return kind
}

// discoverKindDirectories maps every registered kind string to its
// package directory, by parsing the non-test Go files directly under
// each immediate subdirectory of agentRoot for a
// registry.Agents.Register or RegisterWithMeta call. A kind string is
// not assumed to equal its directory name: claude-code and
// copilot-cli register under directories named claude and copilot,
// so the directory is discovered from the registration call itself.
func discoverKindDirectories(fset *token.FileSet, agentRoot string) (map[string]string, error) {
	entries, err := os.ReadDir(agentRoot)
	if err != nil {
		return nil, err
	}

	kindDirs := make(map[string]string)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(agentRoot, entry.Name())
		goFiles, globErr := filepath.Glob(filepath.Join(dir, "*.go"))
		if globErr != nil {
			continue
		}
		for _, path := range goFiles {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			file, parseErr := parser.ParseFile(fset, path, nil, 0)
			if parseErr != nil {
				continue
			}
			if kind := registeredKindInFile(file); kind != "" {
				kindDirs[kind] = dir
			}
		}
	}
	return kindDirs, nil
}

// fileCallsAssertMCPInjection reports whether file contains a call
// whose selector resolves to importAlias.AssertMCPInjection, where
// importAlias is file's own local binding for
// mcpCompletenessAgenttestImportPath.
func fileCallsAssertMCPInjection(file *ast.File) bool {
	alias := resolveTestImportName(file, mcpCompletenessAgenttestImportPath)
	if alias == "" {
		return false
	}

	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != mcpCompletenessAssertFuncName {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok || ident.Name != alias {
			return true
		}
		found = true
		return false
	})
	return found
}

// kindsMissingMCPInjectionCoverage reports, in the order kinds are
// given, every kind whose registration call cannot be found under
// agentRoot, or whose package directory's _test.go files carry no
// call resolving to agenttest.AssertMCPInjection. A kind present in
// kinds and covered by such a call is omitted from the result.
func kindsMissingMCPInjectionCoverage(kinds []string, agentRoot string) []string {
	fset := token.NewFileSet()

	kindDirs, err := discoverKindDirectories(fset, agentRoot)
	if err != nil {
		return append([]string(nil), kinds...)
	}

	var missing []string
	for _, kind := range kinds {
		dir, ok := kindDirs[kind]
		if !ok {
			missing = append(missing, kind)
			continue
		}

		testFiles, globErr := filepath.Glob(filepath.Join(dir, "*_test.go"))
		if globErr != nil || len(testFiles) == 0 {
			missing = append(missing, kind)
			continue
		}

		covered := false
		for _, path := range testFiles {
			file, parseErr := parser.ParseFile(fset, path, nil, 0)
			if parseErr != nil {
				continue
			}
			if fileCallsAssertMCPInjection(file) {
				covered = true
				break
			}
		}
		if !covered {
			missing = append(missing, kind)
		}
	}
	return missing
}

// mcpCompletenessRegisteredKinds snapshots registry.Agents.Kinds() at
// package initialization, once main's own blank imports have run
// their init() functions but before any test body executes. Reading
// the registry from inside the test function body instead would let
// this test observe test-only kinds a sibling test file registers
// into the same process-wide registry.Agents singleton from within
// its own test body (closer_test.go's registerCloseableAdapters
// registers "closeable-mock" this way), which is not one of the
// shipped adapters main.go's blank imports produce.
var mcpCompletenessRegisteredKinds = registry.Agents.Kinds()

// TestEveryAgentKindHasMCPInjectionCoverage enumerates every agent
// kind registry.Agents held once main's own blank imports had run,
// and fails by name when a registered kind has no test file calling
// agenttest.AssertMCPInjection against its own package. This closes
// the hole a hand-maintained list of "the six known kinds" would
// reopen on a seventh adapter: a kind that is declared and registered
// but never checked against its real launch surface.
func TestEveryAgentKindHasMCPInjectionCoverage(t *testing.T) {
	t.Parallel()

	if len(mcpCompletenessRegisteredKinds) == 0 {
		t.Fatal("registry.Agents.Kinds() returned no kinds, want at least the built-in adapters main.go blank-imports")
	}

	missing := kindsMissingMCPInjectionCoverage(mcpCompletenessRegisteredKinds, mcpCompletenessAgentRoot)
	if len(missing) != 0 {
		t.Errorf("agent kind(s) %v have no test file calling agenttest.AssertMCPInjection against their own package", missing)
	}
}

// --- kindsMissingMCPInjectionCoverage unit coverage ---

// writeFixtureFile writes content to dir/name, creating dir if needed.
func writeFixtureFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("MkdirAll(%q): %v", dir, err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}

// fixtureCoveredKindRegister registers "covered-kind" from a
// directory named "covered-dir", deliberately not matching the kind
// string, mirroring claude-code/claude and copilot-cli/copilot.
const fixtureCoveredKindRegister = `package covereddir

import "github.com/sortie-ai/sortie/internal/registry"

func init() {
	registry.Agents.Register("covered-kind", newCoveredAdapter)
}
`

const fixtureCoveredKindTest = `package covereddir

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
)

func TestFixtureConformance(t *testing.T) {
	agenttest.AssertMCPInjection(t, "supported", "/tmp/mcp.json", agenttest.MCPLaunchSurface{})
}
`

// fixtureUncoveredKindRegister registers "uncovered-kind" from a
// directory whose test files never call AssertMCPInjection.
const fixtureUncoveredKindRegister = `package uncovereddir

import "github.com/sortie-ai/sortie/internal/registry"

func init() {
	registry.Agents.RegisterWithMeta("uncovered-kind", newUncoveredAdapter, registry.AgentMeta{})
}
`

const fixtureUncoveredKindTest = `package uncovereddir

import "testing"

func TestFixtureSomethingElse(t *testing.T) {
	_ = t
}
`

// TestKindsMissingMCPInjectionCoverage proves the completeness
// mechanism itself can fail: a synthetic kind registered under a
// directory name that does not match its kind string, but whose test
// file calls AssertMCPInjection, is not reported; a kind whose test
// file never calls it is reported by name; and a kind with no
// registration anywhere under the fixture root is reported by name
// too.
func TestKindsMissingMCPInjectionCoverage(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "covered-dir"), "register.go", fixtureCoveredKindRegister)
	writeFixtureFile(t, filepath.Join(root, "covered-dir"), "register_test.go", fixtureCoveredKindTest)
	writeFixtureFile(t, filepath.Join(root, "uncovered-dir"), "register.go", fixtureUncoveredKindRegister)
	writeFixtureFile(t, filepath.Join(root, "uncovered-dir"), "register_test.go", fixtureUncoveredKindTest)
	// "missing-kind" is registered nowhere under root.

	kinds := []string{"covered-kind", "uncovered-kind", "missing-kind"}
	got := kindsMissingMCPInjectionCoverage(kinds, root)

	want := []string{"uncovered-kind", "missing-kind"}
	if len(got) != len(want) {
		t.Fatalf("kindsMissingMCPInjectionCoverage(%v, %q) = %v, want %v", kinds, root, got, want)
	}
	for i, k := range want {
		if got[i] != k {
			t.Errorf("kindsMissingMCPInjectionCoverage(%v, %q)[%d] = %q, want %q", kinds, root, i, got[i], k)
		}
	}
}
