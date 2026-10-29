//go:build unix

package probe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/clientprotocol"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/workspacekit"

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
// declaring the fixture server as the JSON object entry and returns its
// path.
func writeToolServerMCPConfig(t *testing.T, dir, entry string) string {
	t.Helper()
	sortieDir := filepath.Join(dir, workspacekit.SortieDir)
	if err := os.Mkdir(sortieDir, 0o750); err != nil && !os.IsExist(err) {
		t.Fatalf("create %s: %v", sortieDir, err)
	}
	path := filepath.Join(sortieDir, "mcp.json")
	content := fmt.Sprintf(`{"mcpServers":{%q:%s}}`, toolServerName, entry)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write induction MCP configuration %s: %v", path, err)
	}
	return path
}

// declareToolServer returns the path of the configuration declaring
// the fixture server: over HTTP when an earlier launch of this run saw
// the runtime advertise HTTP MCP servers, because some runtimes load no
// other kind, and over stdio otherwise.
func (f *sharedFixture) declareToolServer(t *testing.T, dir, recordPath string) string {
	t.Helper()
	if advertisesHTTPToolServers(f.wireTraceDir) {
		return writeToolServerMCPConfig(t, dir, fmt.Sprintf(`{"type":"http","url":%q}`, serveToolServerHTTP(t, recordPath)))
	}
	scriptPath := agenttest.FakeRuntime(t, dir, "mcp-server", mcpToolServerScenario, mcpToolServerParams{RecordPath: recordPath})
	return writeToolServerMCPConfig(t, dir, fmt.Sprintf(`{"type":"stdio","command":%q,"args":[]}`, scriptPath))
}

// advertisesHTTPToolServers reports whether any launch captured under
// traceDir answered initialize with mcpCapabilities.http set.
func advertisesHTTPToolServers(traceDir string) bool {
	if traceDir == "" {
		return false
	}
	paths, err := filepath.Glob(filepath.Join(traceDir, wireTraceFilePrefix+"*.jsonl"))
	if err != nil {
		return false
	}
	for _, path := range paths {
		content, err := os.ReadFile(path) //nolint:gosec // a capture this collection's own launch wrote under its own trace directory
		if err != nil {
			continue
		}
		for line := range strings.SplitSeq(string(content), "\n") {
			var message struct {
				Result struct {
					AgentCapabilities struct {
						MCPCapabilities struct {
							HTTP bool `json:"http"`
						} `json:"mcpCapabilities"`
					} `json:"agentCapabilities"`
				} `json:"result"`
			}
			if json.Unmarshal([]byte(line), &message) == nil && message.Result.AgentCapabilities.MCPCapabilities.HTTP {
				return true
			}
		}
	}
	return false
}

// serveToolServerHTTP serves the fixture tool server on a loopback port
// until t's cleanups run and returns its URL. Calls are recorded to
// recordPath as the stdio fake records them.
func serveToolServerHTTP(t *testing.T, recordPath string) string {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the HTTP tool server: %v", err)
	}
	server := &http.Server{Handler: toolServerHandler(recordPath), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return "http://" + listener.Addr().String() + "/mcp"
}

// toolServerHandler replies in plain JSON and refuses GET, so a client
// finds no event stream to open.
func toolServerHandler(recordPath string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				ProtocolVersion string `json:"protocolVersion"`
			} `json:"params"`
		}
		if json.Unmarshal(body, &req) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}

		var reply string
		switch req.Method {
		case "initialize":
			version := req.Params.ProtocolVersion
			if version == "" {
				version = "2024-11-05"
			}
			reply = fmt.Sprintf(`"result":{"protocolVersion":%q,"capabilities":{"tools":{}},"serverInfo":{"name":%q,"version":"0.0.0"}}`, version, toolServerName)
		case "tools/list":
			reply = fmt.Sprintf(`"result":{"tools":[{"name":%q,"description":"records that the qualification probe induced a call","inputSchema":{"type":"object","properties":{}}}]}`, probeToolName)
		case "tools/call":
			if err := appendToolCall(recordPath, body); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			reply = `"result":{"content":[{"type":"text","text":"ok"}]}`
		default:
			reply = `"error":{"code":-32601,"message":"method not found"}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,%s}`, req.ID, reply)
	})
}

func appendToolCall(path string, call []byte) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // path is under the caller's own t.TempDir()
	if err != nil {
		return err
	}
	_, err = f.Write(append(slices.Clone(call), '\n'))
	return errors.Join(err, f.Close())
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

// withModelEnviron returns env with the profile's model_env entry for model
// in place of any variable of the same name, and env itself when the profile
// carries its model through model_args.
func withModelEnviron(env []string, p profile.RuntimeProfile, model string) []string {
	entry, ok := p.ModelEnviron(model)
	if !ok {
		return env
	}
	name, _, _ := strings.Cut(entry, "=")
	carried := slices.DeleteFunc(slices.Clone(env), func(variable string) bool {
		return strings.HasPrefix(variable, name+"=")
	})
	return append(carried, entry)
}

// modelEnvironPrefix returns the words that put the model_env entry in front
// of a command string, which the adapter splits on whitespace without a
// shell, and nil when the profile carries its model through model_args.
func modelEnvironPrefix(p profile.RuntimeProfile, model string) []string {
	entries := withModelEnviron(nil, p, model)
	if len(entries) == 0 {
		return nil
	}
	return append([]string{"env"}, entries...)
}

// modelCarryingCommand renders the launch of coords' runtime with argv through
// the run's environment wrapper, carrying the model_env entry the profile
// states.
func (f *sharedFixture) modelCarryingCommand(coords Coordinates, argv []string) string {
	launch := append(modelEnvironPrefix(coords.Profile, coords.Model), coords.CommandPath)
	return f.controlledCommand(strings.Join(launch, " "), argv)
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

	adapter, err := clientprotocol.NewClientProtocolAdapter()
	if err != nil {
		return nil, domain.Session{}, fmt.Errorf("construct the induction adapter: %w", err)
	}

	session, err := adapter.StartSession(context.Background(), domain.StartSessionParams{
		WorkspacePath: workspace,
		AgentConfig: domain.AgentConfig{
			Kind:           "agent-client-protocol",
			Command:        fixture.modelCarryingCommand(coords, argv),
			ReadTimeoutMS:  30000,
			TurnTimeoutMS:  int(toolInductionTurnBound / time.Millisecond),
			StallTimeoutMS: 60000,
		},
		MCPConfigPath: mcpConfigPath,
	})
	if err != nil {
		return adapter, domain.Session{}, err
	}
	group, err := fixture.registerSession(adapter, session)
	if err != nil {
		_ = adapter.StopSession(context.Background(), session)
		t.Fatalf("account for the induction session: %v", err)
	}
	t.Cleanup(func() {
		if _, err := fixture.stopOpenSessions(context.Background()); err != nil {
			t.Errorf("stop induction session: %v", err)
		}
		requireTornDown(t, group)
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
