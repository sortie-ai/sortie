package clientprotocol

import (
	"os"
	"path/filepath"
	"testing"
)

// writeMCPConfig writes a generated MCP configuration file declaring
// one stdio server, and returns its path.
func writeMCPConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mcp.json")
	content := `{"mcpServers":{"sortie-tools":{"type":"stdio","command":"/usr/local/bin/sortie","args":["mcp-server"],"env":{"SORTIE_ISSUE_ID":"abc-123"}}}}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
	return path
}

// TestParseMCPServersRemoteLaunchEmpty confirms a remote launch sends
// an empty tool-server list regardless of what the generated
// configuration declares.
func TestParseMCPServersRemoteLaunchEmpty(t *testing.T) {
	t.Parallel()

	path := writeMCPConfig(t)

	parsed, agentErr := parseMCPServers(path, true)
	if agentErr != nil {
		t.Fatalf("parseMCPServers(remote) error = %v", agentErr)
	}

	servers, withheld := parsed.wireServers(true)
	if len(servers) != 0 {
		t.Errorf("wireServers() = %d servers, want 0 on a remote launch", len(servers))
	}
	if servers == nil {
		t.Error("wireServers() = nil, want a non-nil empty slice")
	}
	if withheld {
		t.Error("wireServers() withheld = true, want false: nothing was offered to withhold")
	}
}

// TestParseMCPServersRemoteLaunchIgnoresPath confirms the empty
// tool-server list holds even when mcpConfigPath is empty, the other
// input a remote launch might vary.
func TestParseMCPServersRemoteLaunchIgnoresPath(t *testing.T) {
	t.Parallel()

	parsed, agentErr := parseMCPServers("", true)
	if agentErr != nil {
		t.Fatalf("parseMCPServers(remote, empty path) error = %v", agentErr)
	}
	if len(parsed.servers) != 0 {
		t.Errorf("parseMCPServers(remote, empty path).servers = %v, want empty", parsed.servers)
	}
}
