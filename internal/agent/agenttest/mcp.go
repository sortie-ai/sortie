package agenttest

import (
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/mcpconfig"
	"github.com/sortie-ai/sortie/internal/registry"
)

// MCPLaunchSurface is every channel by which an adapter can hand the
// generated MCP config path to the agent process for one session.
type MCPLaunchSurface struct {
	Args []string
	Env  []string
}

// AssertMCPInjection fails t unless declared, the kind's registered
// [registry.MCPInjection] disposition, agrees with surface, the
// launch surface an adapter actually produced for a session carrying
// mcpConfigPath. The caller captures surface from its own command
// construction (argument slice, environment slice, or both) for a
// session whose MCPConfigPath is mcpConfigPath.
//
// An element carries the path when it is the path exactly, or when the
// path is its tail behind a single composition marker: an adapter may
// pass the path prefixed with an '@', and an environment entry carries
// it as KEY=<path>. An element that continues past the path names a
// different file, so it is not a match; the workspace holds a
// <path>.tmp beside the generated file during generation, and an
// adapter handed that one has delivered nothing.
func AssertMCPInjection(t *testing.T, declared registry.MCPInjection, mcpConfigPath string, surface MCPLaunchSurface) {
	t.Helper()
	assertMCPInjection(t, declared, mcpConfigPath, surface)
}

// mcpInjectionReporter is the minimal reporting surface
// assertMCPInjection needs; [*testing.T] satisfies it. Splitting the
// check out from [AssertMCPInjection] lets a package-internal test
// drive the same failure-detection logic against a lightweight
// double, since a *testing.T's own failure state cannot itself be
// inspected without failing the enclosing test.
type mcpInjectionReporter interface {
	Helper()
	Errorf(format string, args ...any)
}

func assertMCPInjection(t mcpInjectionReporter, declared registry.MCPInjection, mcpConfigPath string, surface MCPLaunchSurface) {
	t.Helper()

	if mcpConfigPath == "" {
		t.Errorf("mcpConfigPath is empty, want a non-empty path to look for")
		return
	}

	hit := slices.ContainsFunc(surface.Args, func(arg string) bool {
		return carriesMCPConfigPath(arg, mcpConfigPath)
	}) || slices.ContainsFunc(surface.Env, func(env string) bool {
		return carriesMCPConfigPath(env, mcpConfigPath)
	})

	switch declared {
	case registry.MCPInjectionSupported:
		if !hit {
			t.Errorf("declared = %q, hit = %t, want the surface to carry mcpConfigPath", declared, hit)
		}
	case registry.MCPInjectionTranslated:
		assertTranslatedInjection(t, mcpConfigPath, surface)
	case registry.MCPInjectionUnsupported:
		if hit {
			t.Errorf("declared = %q, hit = %t, want the surface not to carry mcpConfigPath", declared, hit)
		}
	case registry.MCPInjectionUndeclared:
		t.Errorf("declared = %q, want a kind that declares an MCP injection disposition", declared)
	default:
		t.Errorf("declared = %q, want a supported or unsupported MCP injection disposition", declared)
	}
}

// assertTranslatedInjection fails t unless every server declared in
// the generated configuration at mcpConfigPath, and the command path
// of every stdio server among them, appears on surface. Unlike
// carriesMCPConfigPath's exact-or-composition-marker match, this
// check is a plain substring test: a translated adapter embeds each
// server's name and command inside one larger rendered value (a TOML
// inline table, a JSON document), never as a whole surface element on
// its own.
func assertTranslatedInjection(t mcpInjectionReporter, mcpConfigPath string, surface MCPLaunchSurface) {
	t.Helper()

	servers, err := mcpconfig.Parse(mcpConfigPath)
	if err != nil {
		t.Errorf("mcpconfig.Parse(%q) error = %v, want a real generated file for a translated declaration", mcpConfigPath, err)
		return
	}

	for _, server := range servers {
		if !surfaceContains(surface, server.Name) {
			t.Errorf("declared = %q, server %q not found on the launch surface", registry.MCPInjectionTranslated, server.Name)
		}
		if server.Transport == mcpconfig.TransportStdio && !surfaceContains(surface, server.Command) {
			t.Errorf("declared = %q, server %q command %q not found on the launch surface", registry.MCPInjectionTranslated, server.Name, server.Command)
		}
		if server.Transport == mcpconfig.TransportHTTP && !surfaceContains(surface, server.URL) {
			t.Errorf("declared = %q, server %q url %q not found on the launch surface", registry.MCPInjectionTranslated, server.Name, server.URL)
		}
	}
}

// surfaceContains reports whether value appears as a substring of any
// argument or environment element on surface. Returns false for an
// empty value so a stdio server with no command never passes
// vacuously.
func surfaceContains(surface MCPLaunchSurface, value string) bool {
	if value == "" {
		return false
	}
	return slices.ContainsFunc(surface.Args, func(arg string) bool {
		return strings.Contains(arg, value)
	}) || slices.ContainsFunc(surface.Env, func(env string) bool {
		return strings.Contains(env, value)
	})
}

// carriesMCPConfigPath reports whether one launch-surface element hands
// path to the agent process. The path must be the whole element, or its
// tail behind one composition marker, so an element naming a neighbouring
// file such as <path>.tmp is not read as a delivery.
func carriesMCPConfigPath(elem, path string) bool {
	if elem == path {
		return true
	}
	prefix, ok := strings.CutSuffix(elem, path)
	if !ok || prefix == "" {
		return false
	}
	switch prefix[len(prefix)-1] {
	case '@', '=':
		return true
	default:
		return false
	}
}
