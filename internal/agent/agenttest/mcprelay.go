package agenttest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/waittest"
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

	// Req and Accept are set on HTTP records only. Req pairs a response with
	// its own request when several clients reuse a JSON-RPC id.
	Req    int     `json:"req,omitempty"`
	Accept *string `json:"accept,omitempty"`
}

const httpFlag = "--http"

const (
	relayToServer = "to_server"
	relayToClient = "to_client"
	relayEnv      = "env"
)

// Run launches Target with args and relays stdio unchanged until the
// server's output closes, then returns the server's exit status. Each
// JSON-RPC message is appended to RecordPath before it is forwarded, so a
// message the agent acted on is always on the record.
//
// When args hold --http, the target announces its HTTP listener as a first
// stdout line {"listening":"127.0.0.1:<port>"}. The relay then announces its
// own listener the same way and records each forwarded JSON body as a message,
// so the traffic reads the same as the stdio relay's.
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
	var copyErr error
	if slices.Contains(args, httpFlag) {
		go func() {
			_, _ = io.Copy(serverIn, os.Stdin)
			_ = serverIn.Close()
		}()
		copyErr = relayHTTP(serverOut, &recorder{mu: &mu, w: record, pid: pid})
	} else {
		go func() {
			_, _ = io.Copy(serverIn, io.TeeReader(os.Stdin, &lineRecorder{mu: &mu, w: record, pid: pid, dir: relayToServer}))
			_ = serverIn.Close()
		}()
		_, copyErr = io.Copy(os.Stdout, io.TeeReader(serverOut, &lineRecorder{mu: &mu, w: record, pid: pid, dir: relayToClient}))
	}
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

type recorder struct {
	mu  *sync.Mutex
	w   io.Writer
	pid int
}

func (r *recorder) add(dir string, msg []byte) error {
	return r.addRecord(relayRecord{Dir: dir}, msg)
}

func (r *recorder) addRecord(rec relayRecord, msg []byte) error {
	msg = bytes.TrimSpace(msg)
	if len(msg) == 0 || !json.Valid(msg) {
		return nil
	}
	rec.PID, rec.Msg = r.pid, msg
	encoded, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err = r.w.Write(append(encoded, '\n'))
	return err
}

func (r *lineRecorder) Write(p []byte) (int, error) {
	r.pending = append(r.pending, p...)
	for {
		line, rest, found := bytes.Cut(r.pending, []byte("\n"))
		if !found {
			return len(p), nil
		}
		r.pending = rest
		if err := (&recorder{mu: r.mu, w: r.w, pid: r.pid}).add(r.dir, line); err != nil {
			return 0, err
		}
	}
}

func relayHTTP(serverOut io.Reader, rec *recorder) error {
	out := bufio.NewReader(serverOut)
	first, err := out.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("read the target's listening event: %w", err)
	}
	var event struct {
		Listening string `json:"listening"`
	}
	if err := json.Unmarshal(first, &event); err != nil || event.Listening == "" {
		return fmt.Errorf("the target's first line %q is not a listening event", bytes.TrimSpace(first))
	}

	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	srv := &http.Server{Handler: &httpForwarder{target: event.Listening, rec: rec}, ReadHeaderTimeout: waittest.HangGuard}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close() //nolint:errcheck // the relay process ends right after, so no caller could act on a close error

	announced, err := json.Marshal(struct {
		Listening string `json:"listening"`
	}{ln.Addr().String()})
	if err != nil {
		return err
	}
	if _, err := os.Stdout.Write(append(announced, '\n')); err != nil {
		return err
	}
	_, err = io.Copy(os.Stdout, out)
	return err
}

type httpForwarder struct {
	target string
	rec    *recorder
	seq    atomic.Int64
}

var forwardClient = &http.Client{
	Transport: &http.Transport{DisableCompression: true},
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func (f *httpForwarder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req := int(f.seq.Add(1))
	accept := r.Header.Get("Accept")
	if err := f.rec.addRecord(relayRecord{Dir: relayToServer, Req: req, Accept: &accept}, body); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	out, err := http.NewRequestWithContext(r.Context(), r.Method, "http://"+f.target+r.URL.RequestURI(), bytes.NewReader(body)) //nolint:gosec // G704: the target is the test's own server on loopback.
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out.Header = r.Header.Clone()
	out.Host = r.Host
	resp, err := forwardClient.Do(out) //nolint:gosec // G704: the target is the test's own server on loopback.
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close() //nolint:errcheck // the body is read to its end before the close, so a close error adds nothing
	if isEventStream(resp.Header) {
		f.relayEvents(w, resp, relayRecord{Dir: relayToClient, Req: req, Accept: &accept})
		return
	}
	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if err := f.rec.addRecord(relayRecord{Dir: relayToClient, Req: req, Accept: &accept}, answer); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	maps.Copy(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(answer)
}

func isEventStream(header http.Header) bool {
	mediaType, _, err := mime.ParseMediaType(header.Get("Content-Type"))
	return err == nil && mediaType == "text/event-stream"
}

// relayEvents forwards an event stream one event at a time, recording each
// event's data before forwarding it. A stream may stay open for the whole
// session, and its client waits on the first event, so the stream is never
// buffered to its end.
func (f *httpForwarder) relayEvents(w http.ResponseWriter, resp *http.Response, rec relayRecord) {
	maps.Copy(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	flusher := http.NewResponseController(w)
	events := bufio.NewReader(resp.Body)
	for {
		event, readErr := readEvent(events)
		if len(event) > 0 {
			if err := f.rec.addRecord(rec, eventData(event)); err != nil {
				return
			}
			if _, err := w.Write(event); err != nil {
				return
			}
			if err := flusher.Flush(); err != nil {
				return
			}
		}
		if readErr != nil {
			return
		}
	}
}

// readEvent returns the lines of one event through the blank line that ends
// it, or what is left of the stream along with the read error.
func readEvent(r *bufio.Reader) ([]byte, error) {
	var event []byte
	for {
		line, err := r.ReadBytes('\n')
		event = append(event, line...)
		if err != nil || len(bytes.TrimRight(line, "\r\n")) == 0 {
			return event, err
		}
	}
}

func eventData(event []byte) []byte {
	var data [][]byte
	for line := range bytes.Lines(event) {
		if payload, ok := bytes.CutPrefix(bytes.TrimRight(line, "\r\n"), []byte("data:")); ok {
			data = append(data, bytes.TrimPrefix(payload, []byte(" ")))
		}
	}
	return bytes.Join(data, []byte("\n"))
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

// AssertAcceptNamesBothMedia fails t unless the record holds at least one HTTP
// request and every one names application/json and text/event-stream in
// Accept. The server's own check admits wildcards, so only this shows what a
// runtime really sends.
func (r RecordingMCPRelay) AssertAcceptNamesBothMedia(t testing.TB, runtime string) {
	t.Helper()

	data, err := os.ReadFile(r.RecordPath) //nolint:gosec // G304: path is the test's own t.TempDir fixture.
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s Accept: read MCP traffic: %v", runtime, err)
	}
	accepts := recordedAccepts(data)
	if len(accepts) == 0 {
		t.Errorf("%s Accept = no HTTP request recorded, want at least one", runtime)
	}
	for _, accept := range accepts {
		if !namesMedia(accept, "application/json") || !namesMedia(accept, "text/event-stream") {
			t.Errorf("%s Accept = %q, want both application/json and text/event-stream", runtime, accept)
		}
	}
}

func recordedAccepts(data []byte) []string {
	var accepts []string
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		var rec relayRecord
		if json.Unmarshal(line, &rec) == nil && rec.Dir == relayToServer && rec.Accept != nil {
			accepts = append(accepts, *rec.Accept)
		}
	}
	return accepts
}

func namesMedia(accept, media string) bool {
	for part := range strings.SplitSeq(accept, ",") {
		name, _, _ := strings.Cut(part, ";")
		if strings.EqualFold(strings.TrimSpace(name), media) {
			return true
		}
	}
	return false
}

// WaitToolsListed reports whether the recorded traffic comes to hold a
// successful tools/list exchange within bound, which shows the runtime has
// read the server's tools.
func (r RecordingMCPRelay) WaitToolsListed(bound time.Duration) bool {
	return waittest.UntilWithin(bound, 20*time.Millisecond, func() error {
		data, _ := os.ReadFile(r.RecordPath) //nolint:gosec // G304: path is the test's own t.TempDir fixture.
		if listing := requestOutcome(data, "tools/list", ""); !listing.answered || listing.failed {
			return errors.New("no successful tools/list exchange recorded")
		}
		return nil
	}) == nil
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
	req int
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

		key := callKey{pid: rec.PID, req: rec.Req, id: string(msg.ID)}
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
