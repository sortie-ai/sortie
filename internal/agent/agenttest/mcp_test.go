package agenttest

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/registry"
)

// writeGeneratedMCPConfig writes a real generated MCP config file
// declaring the given servers to a fresh temp directory, and returns
// its path. The translated branch reads this file from disk, unlike
// the other three dispositions, which compare the path alone.
func writeGeneratedMCPConfig(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
	return path
}

const twoServerMCPConfig = `{"mcpServers":{"sortie-tools":{"command":"/usr/local/bin/sortie","args":["mcp-server"]},"operator-server":{"command":"/usr/local/bin/operator-mcp"}}}`

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

	t.Run("translated with every declared server and stdio command on the surface", func(t *testing.T) {
		t.Parallel()

		mcpConfigPath := writeGeneratedMCPConfig(t, twoServerMCPConfig)

		AssertMCPInjection(t, registry.MCPInjectionTranslated, mcpConfigPath,
			MCPLaunchSurface{Args: []string{
				"-c", `mcp_servers.sortie-tools={command="/usr/local/bin/sortie", args=["mcp-server"]}`,
				"-c", `mcp_servers.operator-server={command="/usr/local/bin/operator-mcp"}`,
			}})
	})

	t.Run("translated with the stdio command delivered inside a wire body", func(t *testing.T) {
		t.Parallel()

		mcpConfigPath := writeGeneratedMCPConfig(t, `{"mcpServers":{"win-server":{"command":"C:\\Program Files\\sortie\\mcp.exe"}}}`)

		AssertMCPInjection(t, registry.MCPInjectionTranslated, mcpConfigPath,
			MCPLaunchSurface{Wire: []string{
				`win-server C:\Program Files\sortie\mcp.exe launched`,
			}})
	})

	t.Run("translated with the stdio command delivered only in its JSON-escaped rendering", func(t *testing.T) {
		t.Parallel()

		mcpConfigPath := writeGeneratedMCPConfig(t, `{"mcpServers":{"win-server":{"command":"C:\\Program Files\\sortie\\mcp.exe"}}}`)

		AssertMCPInjection(t, registry.MCPInjectionTranslated, mcpConfigPath,
			MCPLaunchSurface{Wire: []string{
				`{"method":"session/new","params":{"servers":[{"name":"win-server","argv":["C:\\Program Files\\sortie\\mcp.exe"]}]}}`,
			}})
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

	translatedNothingPath := writeGeneratedMCPConfig(t, twoServerMCPConfig)
	translatedPartialPath := writeGeneratedMCPConfig(t, twoServerMCPConfig)

	windowsCommandConfigPath := writeGeneratedMCPConfig(t, `{"mcpServers":{"win-server":{"command":"C:\\Program Files\\sortie\\mcp.exe"}}}`)

	tests := []struct {
		name          string
		declared      registry.MCPInjection
		mcpConfigPath string
		surface       MCPLaunchSurface

		// wantSubstr, when non-empty, must appear in at least one
		// recorded error format string, pinning the failure to the
		// expected rule rather than merely to some rule.
		wantSubstr string
	}{
		{
			name:          "empty mcpConfigPath",
			declared:      registry.MCPInjectionSupported,
			mcpConfigPath: "",
			surface:       MCPLaunchSurface{Args: []string{"--mcp-config", "/ws/.sortie/mcp.json"}},
		},
		{
			// A translated declaration requires positive evidence:
			// a surface that carries nothing must fail, not pass
			// vacuously because the generated path itself is absent
			// (the translated branch never looks for the path).
			name:          "translated declared but the surface carries nothing",
			declared:      registry.MCPInjectionTranslated,
			mcpConfigPath: translatedNothingPath,
			surface:       MCPLaunchSurface{},
		},
		{
			// A surface carrying only one of the two declared servers
			// must still fail: partial delivery is not delivery.
			name:          "translated declared but the surface carries only some declared servers",
			declared:      registry.MCPInjectionTranslated,
			mcpConfigPath: translatedPartialPath,
			surface: MCPLaunchSurface{Args: []string{
				"-c", `mcp_servers.sortie-tools={command="/usr/local/bin/sortie"}`,
			}},
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
		{
			// The wire body names the server so the name check
			// passes, but carries the command in neither its
			// verbatim nor its JSON-escaped rendering, so the
			// command check must fail on its own.
			name:          "translated declared but the wire body carries the command in neither rendering",
			declared:      registry.MCPInjectionTranslated,
			mcpConfigPath: windowsCommandConfigPath,
			surface:       MCPLaunchSurface{Wire: []string{`{"argv":["win-server","--other-flag"]}`}},
			wantSubstr:    `command %q not found on the launch surface`,
		},
		{
			// The path appears only as encoding/json would render it
			// inside a JSON string, immediately followed by the
			// closing quote; an unsupported adapter delivering that
			// rendering has still delivered the path.
			name:          "unsupported declared but the wire body carries the path in its escaped rendering",
			declared:      registry.MCPInjectionUnsupported,
			mcpConfigPath: `C:\ws\.sortie\mcp.json`,
			surface:       MCPLaunchSurface{Wire: []string{`{"argv":["--mcp-config","C:\\ws\\.sortie\\mcp.json"]}`}},
			wantSubstr:    `want the surface not to carry mcpConfigPath`,
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
			if tt.wantSubstr != "" && !slices.ContainsFunc(reporter.errors, func(format string) bool {
				return strings.Contains(format, tt.wantSubstr)
			}) {
				t.Errorf("assertMCPInjection(%s) errors = %v, want one containing %q", tt.name, reporter.errors, tt.wantSubstr)
			}
		})
	}
}
