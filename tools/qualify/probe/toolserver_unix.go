//go:build unix

package probe

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/clientprotocol"
	"github.com/sortie-ai/sortie/internal/domain"

	"github.com/sortie-ai/sortie/tools/qualify/profile"
)

const (
	// toolServerName is the declared MCP server's name, used both in the
	// generated configuration and reported in its handshake.
	toolServerName = "sortie-probe-tools"

	probeToolName = "sortie_probe_tool"

	// toolInductionTurnBound bounds one tool-server or
	// permission-handling induction turn.
	toolInductionTurnBound = 3 * time.Minute
)

// mcpToolServerScenario names the Go fake runtime the tool-server and
// permission inducers launch as the declared stdio server.
const mcpToolServerScenario = "mcp-tool-server"

// mcpToolServerParams parameterizes mcpToolServerScenario: every
// tools/call it receives is recorded to RecordPath, one line per call.
type mcpToolServerParams struct {
	RecordPath string
}

// writeToolServerMCPConfig writes a generated MCP configuration
// declaring one stdio server at scriptPath and returns its path.
func writeToolServerMCPConfig(t *testing.T, dir, scriptPath string) string {
	t.Helper()
	path := filepath.Join(dir, "mcp.json")
	content := fmt.Sprintf(`{"mcpServers":{%q:{"type":"stdio","command":%q,"args":[]}}}`, toolServerName, scriptPath)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write induction MCP configuration %s: %v", path, err)
	}
	return path
}

// qualifiedToolName substitutes server and tool into the runtime's
// declared tool-name format.
func qualifiedToolName(toolNameFormat, server, tool string) string {
	replacer := strings.NewReplacer("{server}", server, "{tool}", tool)
	return replacer.Replace(toolNameFormat)
}

// policyPlaceholder is the entry-point token a profile carries where
// the launch wants the path of a policy file.
const policyPlaceholder = "{policy}"

// requestsToolPolicy reports whether any entry point asks its launch
// for a policy file. A file written when none is asked for would be a
// format claim about a runtime that never reads it.
func requestsToolPolicy(p profile.RuntimeProfile) bool {
	for _, entry := range p.EntryPoints {
		for _, args := range [][]string{entry.Args, entry.AskingArgs, entry.SeedArgs, entry.ResumeArgs} {
			if slices.Contains(args, policyPlaceholder) {
				return true
			}
		}
	}
	return false
}

// toolServerPolicyWriters holds, per policy-file format, the writer
// that produces a file in it. A policy file is not portable: its
// syntax, name and rule vocabulary belong to the runtime that reads it.
var toolServerPolicyWriters = map[string]func(t *testing.T, dir, qualifiedTool string) string{
	profile.ToolPolicyFormatTOMLRuleList: writeTOMLRuleListPolicyFile,
}

// writeTOMLRuleListPolicyFile writes a policy file carrying one allow
// rule for qualifiedTool as a TOML rule list, and returns its path.
// Every other tool call falls back on the launch's approval mode.
func writeTOMLRuleListPolicyFile(t *testing.T, dir, qualifiedTool string) string {
	t.Helper()
	path := filepath.Join(dir, "tool-server-policy.toml")
	content := fmt.Sprintf("[[rule]]\ntoolName = %q\ndecision = \"allow\"\npriority = 100\n", qualifiedTool)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write tool-server policy %s: %v", path, err)
	}
	return path
}

// writeToolServerPolicy returns the empty path when no entry point
// asks for one. It fails the run on an unsupported format, since an
// unreadable file would grade as a refused tool call rather than the
// harness gap it is.
func writeToolServerPolicy(t *testing.T, dir string, p profile.RuntimeProfile) string {
	t.Helper()
	write, err := toolPolicyWriterFor(p)
	if err != nil {
		t.Fatalf("resolve the tool-server policy writer: %v", err)
	}
	if write == nil {
		return ""
	}
	return write(t, dir, qualifiedToolName(p.ToolNameFormat, toolServerName, probeToolName))
}

// toolPolicyWriterFor resolves the writer for the format the profile
// states: nil when no policy file is asked for, and an error when a
// format this package writes none in is asked for.
func toolPolicyWriterFor(p profile.RuntimeProfile) (func(t *testing.T, dir, qualifiedTool string) string, error) {
	if !requestsToolPolicy(p) {
		return nil, nil
	}
	write, ok := toolServerPolicyWriters[p.ToolPolicyFormat]
	if !ok {
		return nil, fmt.Errorf("profile %q asks its launch for a policy file in format %q, and this package writes none in that format", p.RuntimeID, p.ToolPolicyFormat)
	}
	return write, nil
}

// fileHasContent reports whether path exists and is non-empty. A
// missing file is not an error: the server never created it when no
// call reached it.
func fileHasContent(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return info.Size() > 0, nil
}

// startInductionSession runs under the run's environment wrapper, so
// both surfaces answer under the same allowlist, and registers the
// session with the run that owns workspace, so process-cleanup stops
// and measures it while still graded.
func startInductionSession(t *testing.T, coords Coordinates, argv []string, workspace, mcpConfigPath string) (domain.AgentAdapter, domain.Session, error) {
	t.Helper()

	fixture := fixtureOwningLaunch(workspace)
	if fixture == nil {
		t.Fatalf("induction session workspace %s belongs to no run: a session started outside a run's own launch workspace can be neither stopped nor measured by it", workspace)
	}

	adapter, err := clientprotocol.NewClientProtocolAdapter(nil)
	if err != nil {
		return nil, domain.Session{}, fmt.Errorf("construct the induction adapter: %w", err)
	}

	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig: domain.AgentConfig{
			Kind:           "agent-client-protocol",
			Command:        fixture.controlledCommand(coords.CommandPath, argv),
			ReadTimeoutMS:  30000,
			TurnTimeoutMS:  int(toolInductionTurnBound / time.Millisecond),
			StallTimeoutMS: 60000,
		},
		MCPConfigPath: mcpConfigPath,
	})
	if err != nil {
		return adapter, domain.Session{}, err
	}
	if err := fixture.registerSession(adapter, session); err != nil {
		_ = adapter.StopSession(context.Background(), session)
		t.Fatalf("account for the induction session: %v", err)
	}
	t.Cleanup(func() {
		if _, err := fixture.stopOpenSessions(context.Background()); err != nil {
			t.Errorf("stop induction session: %v", err)
		}
		assertSessionGroupAbsent(t, session)
	})
	return adapter, session, nil
}

func containsNotification(events []domain.AgentEvent, substr string) bool {
	for _, ev := range events {
		if ev.Type == domain.EventNotification && strings.Contains(ev.Message, substr) {
			return true
		}
	}
	return false
}
