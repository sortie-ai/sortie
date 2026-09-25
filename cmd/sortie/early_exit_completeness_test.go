package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// earlyExitJSONRPCImportPath is the import path a kind's own,
// non-test source calls NewConn through when it speaks a persistent
// JSON-RPC handshake it can exit before answering.
const earlyExitJSONRPCImportPath = "github.com/sortie-ai/sortie/internal/agent/jsonrpc"

// earlyExitAssertFuncName is the exported function name a handshake
// kind's test files must call at least once.
const earlyExitAssertFuncName = "AssertEarlyExitReport"

// packageCallsJSONRPCNewConn reports whether any non-test .go file
// directly under dir calls jsonrpc.NewConn, resolved through that
// file's own import alias for [earlyExitJSONRPCImportPath].
func packageCallsJSONRPCNewConn(fset *token.FileSet, dir string) bool {
	goFiles, globErr := filepath.Glob(filepath.Join(dir, "*.go"))
	if globErr != nil {
		return false
	}

	for _, path := range goFiles {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			continue
		}
		alias := resolveTestImportName(file, earlyExitJSONRPCImportPath)
		if alias == "" {
			continue
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
			if !ok || sel.Sel.Name != "NewConn" {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if !ok || ident.Name != alias {
				return true
			}
			found = true
			return false
		})
		if found {
			return true
		}
	}
	return false
}

// earlyExitHandshakeKinds returns the subset of kinds whose registered
// package directory, discovered under agentRoot, calls
// jsonrpc.NewConn in a non-test file.
func earlyExitHandshakeKinds(kinds []string, agentRoot string) []string {
	fset := token.NewFileSet()
	kindDirs, err := discoverKindDirectories(fset, agentRoot)
	if err != nil {
		return nil
	}

	var handshakeKinds []string
	for _, kind := range kinds {
		dir, ok := kindDirs[kind]
		if ok && packageCallsJSONRPCNewConn(fset, dir) {
			handshakeKinds = append(handshakeKinds, kind)
		}
	}
	return handshakeKinds
}

// TestEveryEarlyExitKindHasCoverage enumerates every registered agent
// kind whose package calls jsonrpc.NewConn, and fails by name when
// such a kind has no test file calling
// credentialtest.AssertEarlyExitReport against its own package. It
// also fails when it finds no such kind, so a registry rewrite that
// drops every handshake kind cannot silence this test by vacuity.
func TestEveryEarlyExitKindHasCoverage(t *testing.T) {
	t.Parallel()

	handshakeKinds := earlyExitHandshakeKinds(mcpCompletenessRegisteredKinds, mcpCompletenessAgentRoot)
	if len(handshakeKinds) == 0 {
		t.Fatal("no registered agent kind's package calls jsonrpc.NewConn, want at least one (agent-client-protocol, codex)")
	}

	missing := kindsMissingCoverage(handshakeKinds, mcpCompletenessAgentRoot, credentialCompletenessImportPath, earlyExitAssertFuncName)
	if len(missing) != 0 {
		t.Errorf("agent kind(s) %v call jsonrpc.NewConn but have no test file calling credentialtest.AssertEarlyExitReport against their own package", missing)
	}
}

// TestKindsMissingEarlyExitCoverage proves earlyExitHandshakeKinds and
// kindsMissingCoverage together report the right verdict for three
// shapes: a handshake package with the assertion, one without it, and
// a package that never speaks the handshake at all.
func TestKindsMissingEarlyExitCoverage(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "handshake-covered-dir"), "register.go", `package handshakecovereddir

import (
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/registry"
)

func init() {
	registry.Agents.Register("earlyexit-covered-kind", newAdapter)
}

func connect() {
	jsonrpc.NewConn(nil, nil, nil)
}
`)
	writeFixtureFile(t, filepath.Join(root, "handshake-covered-dir"), "register_test.go", `package handshakecovereddir

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/agenttest/credentialtest"
)

func TestFixtureEarlyExitConformance(t *testing.T) {
	credentialtest.AssertEarlyExitReport(t, "earlyexit-covered-kind", nil, config)
}
`)
	writeFixtureFile(t, filepath.Join(root, "handshake-uncovered-dir"), "register.go", `package handshakeuncovereddir

import (
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/registry"
)

func init() {
	registry.Agents.Register("earlyexit-uncovered-kind", newAdapter)
}

func connect() {
	jsonrpc.NewConn(nil, nil, nil)
}
`)
	writeFixtureFile(t, filepath.Join(root, "handshake-uncovered-dir"), "register_test.go", `package handshakeuncovereddir

import "testing"

func TestFixtureSomethingElse(t *testing.T) {}
`)
	writeFixtureFile(t, filepath.Join(root, "no-handshake-dir"), "register.go", `package nohandshakedir

import "github.com/sortie-ai/sortie/internal/registry"

func init() {
	registry.Agents.Register("earlyexit-no-handshake-kind", newAdapter)
}
`)
	writeFixtureFile(t, filepath.Join(root, "no-handshake-dir"), "register_test.go", `package nohandshakedir

import "testing"

func TestFixtureSomethingElse(t *testing.T) {}
`)

	kinds := []string{"earlyexit-covered-kind", "earlyexit-uncovered-kind", "earlyexit-no-handshake-kind"}

	handshakeKinds := earlyExitHandshakeKinds(kinds, root)
	wantHandshakeKinds := []string{"earlyexit-covered-kind", "earlyexit-uncovered-kind"}
	if len(handshakeKinds) != len(wantHandshakeKinds) {
		t.Fatalf("earlyExitHandshakeKinds(%v, %q) = %v, want %v", kinds, root, handshakeKinds, wantHandshakeKinds)
	}
	for i, kind := range wantHandshakeKinds {
		if handshakeKinds[i] != kind {
			t.Fatalf("earlyExitHandshakeKinds(%v, %q) = %v, want %v", kinds, root, handshakeKinds, wantHandshakeKinds)
		}
	}

	missing := kindsMissingCoverage(handshakeKinds, root, credentialCompletenessImportPath, earlyExitAssertFuncName)
	want := []string{"earlyexit-uncovered-kind"}
	if len(missing) != len(want) || missing[0] != want[0] {
		t.Errorf("kindsMissingCoverage(%v, %q) = %v, want %v", handshakeKinds, root, missing, want)
	}
}
