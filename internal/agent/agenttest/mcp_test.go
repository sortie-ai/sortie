package agenttest

import (
	"testing"

	"github.com/sortie-ai/sortie/internal/registry"
)

// mcpFakeReporter is a minimal [mcpInjectionReporter] double that
// records Errorf calls instead of failing the enclosing test, so a
// deliberately-violating input can be driven through
// assertMCPInjection without reddening the smoke test itself.
type mcpFakeReporter struct {
	errors []string
}

func (f *mcpFakeReporter) Helper() {}

func (f *mcpFakeReporter) Errorf(format string, args ...any) {
	f.errors = append(f.errors, format)
}

// TestAssertMCPInjection_Passing exercises the exported
// AssertMCPInjection entry point against a real *testing.T for both
// declared dispositions: Supported with a surface that carries the
// path, and Unsupported with a surface that does not. Neither case
// must report a failure.
func TestAssertMCPInjection_Passing(t *testing.T) {
	t.Parallel()

	t.Run("supported with the path in Args", func(t *testing.T) {
		t.Parallel()

		AssertMCPInjection(t, registry.MCPInjectionSupported, "/ws/.sortie/mcp.json",
			MCPLaunchSurface{Args: []string{"--mcp-config", "/ws/.sortie/mcp.json"}})
	})

	t.Run("supported with the path composed inside an argument", func(t *testing.T) {
		t.Parallel()

		AssertMCPInjection(t, registry.MCPInjectionSupported, "/ws/.sortie/mcp.json",
			MCPLaunchSurface{Args: []string{"--additional-mcp-config", "@/ws/.sortie/mcp.json"}})
	})

	t.Run("supported with the path in Env", func(t *testing.T) {
		t.Parallel()

		AssertMCPInjection(t, registry.MCPInjectionSupported, "/ws/.sortie/mcp.json",
			MCPLaunchSurface{Env: []string{"MCP_CONFIG=/ws/.sortie/mcp.json"}})
	})

	t.Run("unsupported with an empty surface", func(t *testing.T) {
		t.Parallel()

		AssertMCPInjection(t, registry.MCPInjectionUnsupported, "/ws/.sortie/mcp.json", MCPLaunchSurface{})
	})

	t.Run("unsupported with an unrelated surface", func(t *testing.T) {
		t.Parallel()

		AssertMCPInjection(t, registry.MCPInjectionUnsupported, "/ws/.sortie/mcp.json",
			MCPLaunchSurface{Args: []string{"--model", "gpt-5"}})
	})

	t.Run("unsupported with a neighbouring file that extends the path", func(t *testing.T) {
		t.Parallel()

		// The workspace holds mcp.json.tmp beside the generated file
		// during generation. An element naming it delivers nothing, so
		// an unsupported adapter must not be failed for carrying it.
		AssertMCPInjection(t, registry.MCPInjectionUnsupported, "/ws/.sortie/mcp.json",
			MCPLaunchSurface{
				Args: []string{"--mcp-config", "/ws/.sortie/mcp.json.tmp"},
				Env:  []string{"BACKUP=/ws/.sortie/mcp.json-disabled"},
			})
	})
}

// TestAssertMCPInjection_Violating drives assertMCPInjection through a
// fakeReporter for every failure arm the check defines: an empty path,
// Supported declared but not hit, Unsupported declared but hit, and
// Undeclared regardless of the surface. Each case must record at least
// one failure.
func TestAssertMCPInjection_Violating(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		declared      registry.MCPInjection
		mcpConfigPath string
		surface       MCPLaunchSurface
	}{
		{
			name:          "empty mcpConfigPath",
			declared:      registry.MCPInjectionSupported,
			mcpConfigPath: "",
			surface:       MCPLaunchSurface{Args: []string{"--mcp-config", "/ws/.sortie/mcp.json"}},
		},
		{
			name:          "supported declared but the surface carries nothing",
			declared:      registry.MCPInjectionSupported,
			mcpConfigPath: "/ws/.sortie/mcp.json",
			surface:       MCPLaunchSurface{},
		},
		{
			name:          "unsupported declared but the surface carries the path in Args",
			declared:      registry.MCPInjectionUnsupported,
			mcpConfigPath: "/ws/.sortie/mcp.json",
			surface:       MCPLaunchSurface{Args: []string{"--mcp-config", "/ws/.sortie/mcp.json"}},
		},
		{
			name:          "unsupported declared but the surface carries the path in Env",
			declared:      registry.MCPInjectionUnsupported,
			mcpConfigPath: "/ws/.sortie/mcp.json",
			surface:       MCPLaunchSurface{Env: []string{"MCP_CONFIG=/ws/.sortie/mcp.json"}},
		},
		{
			name:          "undeclared regardless of the surface",
			declared:      registry.MCPInjectionUndeclared,
			mcpConfigPath: "/ws/.sortie/mcp.json",
			surface:       MCPLaunchSurface{},
		},
		{
			// Delivering a neighbouring file is not delivering the
			// generated one: a supported adapter handed <path>.tmp has
			// injected nothing, and the assertion must say so.
			name:          "supported declared but the surface carries only a file extending the path",
			declared:      registry.MCPInjectionSupported,
			mcpConfigPath: "/ws/.sortie/mcp.json",
			surface:       MCPLaunchSurface{Args: []string{"--mcp-config", "/ws/.sortie/mcp.json.tmp"}},
		},
		{
			name:          "supported declared but an env value only extends the path",
			declared:      registry.MCPInjectionSupported,
			mcpConfigPath: "/ws/.sortie/mcp.json",
			surface:       MCPLaunchSurface{Env: []string{"BACKUP=/ws/.sortie/mcp.json-disabled"}},
		},
		{
			// MCPInjection is a string type, so a value outside the
			// declared set is representable. Without the switch's
			// default arm such a value matches no case and the
			// assertion returns having checked nothing, which is the
			// one way this helper can pass while measuring nothing.
			name:          "a disposition outside the declared set",
			declared:      registry.MCPInjection("conditional"),
			mcpConfigPath: "/ws/.sortie/mcp.json",
			surface:       MCPLaunchSurface{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reporter := &mcpFakeReporter{}
			assertMCPInjection(reporter, tt.declared, tt.mcpConfigPath, tt.surface)

			if len(reporter.errors) == 0 {
				t.Errorf("assertMCPInjection(%s) recorded no failures, want at least one", tt.name)
			}
		})
	}
}
