package agenttest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	ConfigPath string
	Identity   map[string]string
}

// NewSortieTools builds the sortie binary and writes an MCP config at
// workspace/.sortie/mcp.json, the path the orchestrator uses, naming one
// server, [SortieToolsServer], which runs "sortie mcp-server" behind a
// [RecordingMCPRelay]. The config's env block is [SortieTools].Identity:
// SORTIE_WORKSPACE is workspace and SORTIE_DISPATCH_ID is random per call, so
// no inherited variable can carry it.
//
// The calling package's TestMain must call [Main]. It runs the go command, so
// it must be called before a test points HOME away from the developer's
// toolchain.
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

	dispatchID := make([]byte, 16)
	if _, err := rand.Read(dispatchID); err != nil {
		t.Fatalf("read random bytes: %v", err)
	}
	identity := map[string]string{
		"SORTIE_WORKSPACE":   workspace,
		"SORTIE_DISPATCH_ID": hex.EncodeToString(dispatchID),
	}

	relay := NewRecordingMCPRelay(t, dir, sortie, slices.Sorted(maps.Keys(identity))...)
	server := map[string]any{
		"type":    "stdio",
		"command": relay.Command,
		"args":    []string{"mcp-server", "--workflow", workflow},
		"env":     identity,
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
	return SortieTools{Relay: relay, ConfigPath: filepath.Join(workspace, workspacekit.SortieDir, "mcp.json"), Identity: identity}
}
