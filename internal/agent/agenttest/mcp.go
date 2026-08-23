package agenttest

import (
	"strings"
	"testing"

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
// session whose MCPConfigPath is mcpConfigPath; the check is substring
// containment against every element of surface.Args and surface.Env,
// so an adapter that passes the path inside a composed argument (for
// example, prefixed with an '@') is still detected.
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

	hit := false
	for _, arg := range surface.Args {
		if strings.Contains(arg, mcpConfigPath) {
			hit = true
			break
		}
	}
	if !hit {
		for _, env := range surface.Env {
			if strings.Contains(env, mcpConfigPath) {
				hit = true
				break
			}
		}
	}

	switch declared {
	case registry.MCPInjectionSupported:
		if !hit {
			t.Errorf("declared = %q, hit = %t, want the surface to carry mcpConfigPath", declared, hit)
		}
	case registry.MCPInjectionUnsupported:
		if hit {
			t.Errorf("declared = %q, hit = %t, want the surface not to carry mcpConfigPath", declared, hit)
		}
	case registry.MCPInjectionUndeclared:
		t.Errorf("declared = %q, want a kind that declares an MCP injection disposition", declared)
	}
}
