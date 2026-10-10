package agenttest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

// MCPRelayScenario names the built-in [Scenario] that stands in for an MCP
// server command: it launches the real server named by [MCPRelay]'s Target
// with the arguments it received and relays standard input and output
// between the agent and that server unchanged, recording each JSON-RPC
// message on the way.
const MCPRelayScenario = "agenttest.mcprelay"

// MCPRelay parameterizes [MCPRelayScenario].
type MCPRelay struct {
	Target     string
	RecordPath string
}

type relayRecord struct {
	PID int             `json:"pid"`
	Dir string          `json:"dir"`
	Msg json.RawMessage `json:"msg"`
}

const (
	relayToServer = "to_server"
	relayToClient = "to_client"
)

// Run launches Target with args and relays until the server's output
// closes, then returns the server's exit status. Each newline-terminated
// message is appended to RecordPath before it is forwarded, so a message
// the agent acted on is always on the record.
func (p MCPRelay) Run(args []string) int {
	record, err := os.OpenFile(p.RecordPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600) //nolint:gosec // G304: path is the test's own t.TempDir fixture.
	if err != nil {
		fmt.Fprintf(os.Stderr, "agenttest: open MCP record: %v\n", err)
		return 2
	}
	defer record.Close() //nolint:errcheck // the record is flushed line by line

	cmd := exec.CommandContext(context.Background(), p.Target, args...) //nolint:gosec // G204: Target is the test's own sortie binary.
	cmd.Stderr = os.Stderr
	serverIn, err := cmd.StdinPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "agenttest: MCP server stdin: %v\n", err)
		return 2
	}
	serverOut, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "agenttest: MCP server stdout: %v\n", err)
		return 2
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "agenttest: start MCP server: %v\n", err)
		return 2
	}

	var mu sync.Mutex
	pid := os.Getpid()
	go func() {
		_, _ = io.Copy(serverIn, io.TeeReader(os.Stdin, &lineRecorder{mu: &mu, w: record, pid: pid, dir: relayToServer}))
		_ = serverIn.Close()
	}()

	_, copyErr := io.Copy(os.Stdout, io.TeeReader(serverOut, &lineRecorder{mu: &mu, w: record, pid: pid, dir: relayToClient}))
	waitErr := cmd.Wait()
	if copyErr != nil {
		fmt.Fprintf(os.Stderr, "agenttest: relay MCP server output: %v\n", copyErr)
		return 2
	}

	var exitErr *exec.ExitError
	switch {
	case waitErr == nil:
		return 0
	case errors.As(waitErr, &exitErr):
		return exitErr.ExitCode()
	default:
		fmt.Fprintf(os.Stderr, "agenttest: wait for MCP server: %v\n", waitErr)
		return 2
	}
}

func runMCPRelay(args []string, params MCPRelay) int {
	return params.Run(args)
}

// lineRecorder appends one record per complete line written to it. The
// agent may start the same server command more than once, so every record
// carries the relay's pid to keep request ids from different servers apart.
type lineRecorder struct {
	mu      *sync.Mutex
	w       io.Writer
	pid     int
	dir     string
	pending []byte
}

func (r *lineRecorder) Write(p []byte) (int, error) {
	r.pending = append(r.pending, p...)
	for {
		line, rest, found := bytes.Cut(r.pending, []byte("\n"))
		if !found {
			return len(p), nil
		}
		line = bytes.TrimSpace(line)
		r.pending = rest
		if len(line) == 0 || !json.Valid(line) {
			continue
		}
		encoded, err := json.Marshal(relayRecord{PID: r.pid, Dir: r.dir, Msg: line})
		if err != nil {
			return 0, err
		}
		r.mu.Lock()
		_, err = r.w.Write(append(encoded, '\n'))
		r.mu.Unlock()
		if err != nil {
			return 0, err
		}
	}
}

// RecordingMCPRelay is an MCP server command that forwards to the real
// server while recording the traffic.
type RecordingMCPRelay struct {
	// Command is what an MCP configuration names in place of the real server.
	Command    string
	RecordPath string
}

// NewRecordingMCPRelay creates under dir a [FakeRuntime] running
// [MCPRelayScenario] against target. The calling package's TestMain must
// call [Main].
func NewRecordingMCPRelay(t testing.TB, dir, target string) RecordingMCPRelay {
	t.Helper()

	recordPath := filepath.Join(dir, "mcp-traffic.jsonl")
	return RecordingMCPRelay{
		Command:    FakeRuntime(t, dir, "mcp-relay", MCPRelayScenario, MCPRelay{Target: target, RecordPath: recordPath}),
		RecordPath: recordPath,
	}
}

// AssertToolCallSucceeded fails t unless the recorded traffic holds a
// tools/call request for tool and a response to it that carries no JSON-RPC
// error. It reads the request's params.name rather than any event the
// agent adapter reports, because a runtime can label every MCP call with
// one generic tool name, or report a tool result for a call that never
// reached the server.
func (r RecordingMCPRelay) AssertToolCallSucceeded(t testing.TB, tool string) {
	t.Helper()

	reportToolCallOutcome(t, tool, r.outcome(t, tool))
}

// AssertModelToolCall is [RecordingMCPRelay.AssertToolCallSucceeded] for a
// live turn whose model decides whether to call tool. When no tools/call
// request for tool was recorded it fails t if events show the runtime
// attempted one, and skips t otherwise. An attempt is a tool_result event
// whose tool name contains tool or equals one of attemptNames, the generic
// names a runtime gives every MCP call.
func (r RecordingMCPRelay) AssertModelToolCall(t testing.TB, tool string, events []domain.AgentEvent, attemptNames ...string) {
	t.Helper()

	outcome := r.outcome(t, tool)
	if outcome.called {
		reportToolCallOutcome(t, tool, outcome)
		return
	}
	attempted := slices.ContainsFunc(events, func(e domain.AgentEvent) bool {
		return e.Type == domain.EventToolResult && (strings.Contains(e.ToolName, tool) || slices.Contains(attemptNames, e.ToolName))
	})
	if attempted {
		t.Errorf("tool call %q = the runtime reported an attempt, but no tools/call request for the tool reached the server in %d recorded messages", tool, outcome.messages)
		return
	}
	t.Skipf("model choice: the model neither called %q nor attempted to, so this turn proves nothing about the MCP path", tool)
}

func (r RecordingMCPRelay) outcome(t testing.TB, tool string) toolCallResult {
	t.Helper()

	data, err := os.ReadFile(r.RecordPath) //nolint:gosec // G304: path is the test's own t.TempDir fixture.
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tool call %q: read MCP traffic: %v", tool, err)
	}
	return requestOutcome(data, "tools/call", tool)
}

// WaitToolsListed reports whether the recorded traffic comes to hold a
// successful tools/list exchange within bound, which shows the runtime has
// read the server's tools.
func (r RecordingMCPRelay) WaitToolsListed(bound time.Duration) bool {
	deadline := time.Now().Add(bound)
	for {
		data, _ := os.ReadFile(r.RecordPath) //nolint:gosec // G304: path is the test's own t.TempDir fixture.
		if listing := requestOutcome(data, "tools/list", ""); listing.answered && !listing.failed {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func reportToolCallOutcome(t testing.TB, tool string, outcome toolCallResult) {
	t.Helper()

	switch {
	case !outcome.called:
		t.Errorf("tool call %q = no tools/call request for the tool in %d recorded messages, want one", tool, outcome.messages)
	case !outcome.answered:
		t.Errorf("tool call %q = request recorded without a response, want a response with the same id", tool)
	case outcome.failed:
		t.Errorf("tool call %q = every response to the call carried a JSON-RPC error, want one without", tool)
	}
}

type toolCallResult struct {
	messages int
	called   bool
	answered bool
	failed   bool
}

type rpcMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		Name string `json:"name"`
	} `json:"params"`
	Error json.RawMessage `json:"error"`
}

type callKey struct {
	pid int
	id  string
}

// requestOutcome reduces recorded traffic to the fate of the requests for
// method whose params.name is name. A response matches a request on relay
// pid and id; a message that carries a method is a request or notification,
// never a response, so a server-originated request reusing an id does not
// count.
func requestOutcome(data []byte, method, name string) toolCallResult {
	var out toolCallResult
	calls := make(map[callKey]bool)
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec relayRecord
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		var msg rpcMessage
		if json.Unmarshal(rec.Msg, &msg) != nil {
			continue
		}
		out.messages++

		key := callKey{pid: rec.PID, id: string(msg.ID)}
		switch {
		case rec.Dir == relayToServer && msg.Method == method && msg.Params.Name == name && len(msg.ID) > 0:
			out.called = true
			calls[key] = true
		case rec.Dir == relayToClient && msg.Method == "" && len(msg.ID) > 0 && calls[key]:
			out.answered = true
			if isNullOrAbsent(msg.Error) {
				out.failed = false
				return out
			}
			out.failed = true
		}
	}
	return out
}

func isNullOrAbsent(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}
