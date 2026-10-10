package agenttest

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sortie-ai/sortie/internal/workspacekit"
)

const (
	// SortieToolsServer is the server name an MCP config from
	// [NewSortieTools] gives the Sortie MCP server, as the orchestrator does.
	SortieToolsServer = "sortie-tools"

	// SortieStatusTool is the tool [NewSortieTools] serves: the one Sortie
	// tool that needs no tracker, only a workspace.
	SortieStatusTool = "sortie_status"
)

// SortieTools is the real Sortie MCP server, configured for one workspace
// the way the orchestrator configures a session, with its traffic recorded.
type SortieTools struct {
	Relay      RecordingMCPRelay
	ConfigPath string // the MCP config, at the path the orchestrator writes it
}

// NewSortieTools builds the sortie binary and writes an MCP config at
// workspace/.sortie/mcp.json naming one server, [SortieToolsServer], which
// runs "sortie mcp-server" behind a [RecordingMCPRelay]. The calling package's
// TestMain must call [Main]. It runs the go command, so it must be called
// before a test points HOME away from the developer's toolchain.
func NewSortieTools(t testing.TB, workspace string) SortieTools {
	t.Helper()

	dir := t.TempDir()
	sortie := filepath.Join(dir, "sortie")
	build := exec.CommandContext(context.Background(), "go", "build", "-o", sortie, "github.com/sortie-ai/sortie/cmd/sortie") //nolint:gosec // G204: the output path is the test's own t.TempDir.
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build cmd/sortie: %v\n%s", err, out)
	}

	workflow := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(workflow, []byte("---\npolling:\n  interval_ms: 30000\n---\nDo something.\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", workflow, err)
	}

	relay := NewRecordingMCPRelay(t, dir, sortie)
	server := map[string]any{
		"type":    "stdio",
		"command": relay.Command,
		"args":    []string{"mcp-server", "--workflow", workflow},
		"env":     map[string]string{"SORTIE_WORKSPACE": workspace},
	}
	doc, err := json.Marshal(map[string]any{"mcpServers": map[string]any{SortieToolsServer: server}})
	if err != nil {
		t.Fatalf("encode MCP config: %v", err)
	}
	sortieDir, err := workspacekit.OpenSortieDir(workspace, true)
	if err != nil {
		t.Fatalf("open %s: %v", workspace, err)
	}
	defer sortieDir.Close() //nolint:errcheck // the handle is not needed once the config is written
	if err := workspacekit.ReplaceFile(sortieDir, "mcp.json", doc); err != nil {
		t.Fatalf("write MCP config in %s: %v", workspace, err)
	}
	return SortieTools{Relay: relay, ConfigPath: filepath.Join(workspace, workspacekit.SortieDir, "mcp.json")}
}
