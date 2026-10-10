package agenttest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

// MCPRelayScenario names the built-in [Scenario] that stands in for an MCP
// server command by running [MCPRelay.Run].
const MCPRelayScenario = "agenttest.mcprelay"

// MCPRelay parameterizes [MCPRelayScenario].
type MCPRelay struct {
	Target     string
	RecordPath string
	Env        []string // names whose values the relay records before it starts Target
}

type relayRecord struct {
	PID int               `json:"pid"`
	Dir string            `json:"dir"`
	Msg json.RawMessage   `json:"msg,omitempty"`
	Env map[string]string `json:"env,omitempty"`
}

const (
	relayToServer = "to_server"
	relayToClient = "to_client"
	relayEnv      = "env"
)

// Run launches Target with args and relays stdio unchanged until the
// server's output closes, then returns the server's exit status. Each
// JSON-RPC message is appended to RecordPath before it is forwarded, so a
// message the agent acted on is always on the record.
func (p MCPRelay) Run(args []string) int {
	record, err := os.OpenFile(p.RecordPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600) //nolint:gosec // G304: path is the test's own t.TempDir fixture.
	if err != nil {
		fmt.Fprintf(os.Stderr, "agenttest: open MCP record: %v\n", err)
		return 2
	}
	defer record.Close() //nolint:errcheck // the record is flushed line by line

	if err := writeEnvRecord(record, p.Env); err != nil {
		fmt.Fprintf(os.Stderr, "agenttest: record MCP relay environment: %v\n", err)
		return 2
	}

	// cmd.Env stays nil: the recorded environment must be the server's own.
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

func writeEnvRecord(w io.Writer, names []string) error {
	values := make(map[string]string, len(names))
	for _, name := range names {
		values[name] = os.Getenv(name)
	}
	encoded, err := json.Marshal(relayRecord{PID: os.Getpid(), Dir: relayEnv, Env: values})
	if err != nil {
		return err
	}
	_, err = w.Write(append(encoded, '\n'))
	return err
}

// lineRecorder tags each record with the relay's pid because the agent may
// start the same server command more than once, and request ids repeat
// across those servers.
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
// [MCPRelayScenario] against target. The relay records the value of each name
// in env from its own environment before it starts target. The calling
// package's TestMain must call [Main].
func NewRecordingMCPRelay(t testing.TB, dir, target string, env ...string) RecordingMCPRelay {
	t.Helper()

	recordPath := filepath.Join(dir, "mcp-traffic.jsonl")
	return RecordingMCPRelay{
		Command:    FakeRuntime(t, dir, "mcp-relay", MCPRelayScenario, MCPRelay{Target: target, RecordPath: recordPath, Env: env}),
		RecordPath: recordPath,
	}
}

// AssertToolCallSucceeded fails t unless the recorded traffic holds a
// tools/call request for tool and a response to it that carries no JSON-RPC
// error. It reads the request's params.name rather than adapter events,
// because a runtime can label every MCP call with one generic tool name or
// report a result for a call that never reached the server.
//
// When env is not empty, the relay process that recorded the first such
// response must also have recorded every entry of env as its own
// environment; each missing or differing entry fails t.
func (r RecordingMCPRelay) AssertToolCallSucceeded(t testing.TB, tool string, env map[string]string) {
	t.Helper()

	outcome := r.outcome(t, tool)
	reportToolCallOutcome(t, tool, outcome)
	if !outcome.called || !outcome.answered || outcome.failed {
		return
	}
	for _, name := range slices.Sorted(maps.Keys(env)) {
		if got, ok := outcome.answeringEnv[name]; !ok || got != env[name] {
			t.Errorf("tool call %q = the answering relay recorded %s=%q, want %q", tool, name, got, env[name])
		}
	}
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

	answeringEnv map[string]string
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

// requestOutcome matches a response to a request on relay pid and id. A
// message carrying a method is never a response, because JSON-RPC lets the
// server send its own requests with an id the client already used.
func requestOutcome(data []byte, method, name string) toolCallResult {
	var out toolCallResult
	calls := make(map[callKey]bool)
	envs := make(map[int]map[string]string)
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec relayRecord
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		if rec.Dir == relayEnv {
			envs[rec.PID] = rec.Env
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
				out.answeringEnv = envs[rec.PID]
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
