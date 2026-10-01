//go:build unix

package clientprotocol

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agenttest"
)

// Scenario names for every Go fake runtime this package's tests
// launch through [agenttest.FakeRuntime]. Registered into
// fakeRuntimeScenarios below.
const (
	scenarioProtocolAgent = "clientprotocol.protocol-agent"
	scenarioParkedAgent   = "clientprotocol.parked-agent"
	scenarioBoundedReader = "clientprotocol.bounded-reader"
	scenarioHoldOpen      = "clientprotocol.hold-open"
	scenarioSSHStandIn    = "clientprotocol.ssh-stand-in"
)

func init() {
	fakeRuntimeScenarios[scenarioProtocolAgent] = agenttest.Typed(runProtocolAgent)
	fakeRuntimeScenarios[scenarioParkedAgent] = agenttest.Typed(runParkedAgent)
	fakeRuntimeScenarios[scenarioBoundedReader] = agenttest.Typed(runBoundedReader)
	fakeRuntimeScenarios[scenarioHoldOpen] = agenttest.Typed(runHoldOpen)
	fakeRuntimeScenarios[scenarioSSHStandIn] = agenttest.Typed(runSSHStandIn)
}

// sshStandInParams configures [runSSHStandIn]: path is the PATH value
// its dropped-environment child receives.
type sshStandInParams struct {
	PATH string
}

// runSSHStandIn is a stand-in "ssh" runtime: it ignores every argument
// ahead of the last one, the remote command sshutil.BuildSSHLaunch
// produced, and runs that command through sh -c with its own
// environment dropped and replaced by params.PATH alone, and its
// standard input, output, and error inherited. Dropping the
// environment is what proves a carried variable reaches the remote
// command only through the SSH session's standard input, never
// through this process's own inherited environment.
func runSSHStandIn(args []string, params sshStandInParams) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "ssh stand-in: no arguments")
		return 2
	}
	shPath, err := exec.LookPath("sh")
	if err != nil {
		fmt.Fprintf(os.Stderr, "ssh stand-in: sh not found: %v\n", err)
		return 2
	}

	remoteCommand := args[len(args)-1]
	cmd := exec.Command(shPath, "-c", remoteCommand) //nolint:gosec // remoteCommand is the launch this test built
	cmd.Env = []string{"PATH=" + params.PATH}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "ssh stand-in: %v\n", err)
		return 2
	}
	return 0
}

// gracefulMode selects how a protocolAgent scenario reacts to the
// catchable termination signal.
type gracefulMode string

const (
	gracefulNone      gracefulMode = ""
	gracefulIgnore    gracefulMode = "ignore"
	gracefulImmediate gracefulMode = "immediate"
	gracefulDelayed   gracefulMode = "delayed"
)

// protocolAgentParams configures [runProtocolAgent], the fake agent
// used by every clientprotocol test that needs a real subprocess
// speaking (or refusing to speak) the wire handshake. Only the fields
// a given fixture cares about need to be set; the rest keep their
// zero value.
type protocolAgentParams struct {
	// ExitImmediately makes the process exit(0) before anything else
	// runs.
	ExitImmediately bool

	// StderrImmediate, if non-empty, is written to standard error and
	// the process exits(1) before reading or answering anything.
	StderrImmediate string

	// Handshake makes the process answer the two calls startSession
	// makes before returning: initialize (always id 1, since it is
	// the connection's first call) and session/new (always id 2).
	Handshake              bool
	IncludeMCPCapabilities bool
	CaptureSessionNewPath  string

	// StderrOnInit, if non-empty, is written to standard error
	// immediately after answering initialize, and the process
	// exits(1) without ever seeing session/new.
	StderrOnInit string

	// Graceful selects the process's reaction to the catchable
	// termination signal; GracefulDelay and EvidencePath apply only
	// to gracefulDelayed.
	Graceful      gracefulMode
	GracefulDelay time.Duration
	EvidencePath  string

	// ChildPath and ChildDetachedPath, at most one of which is set,
	// name an already-built fake runtime this process spawns once
	// setup completes: ChildPath stays in this process's own group,
	// ChildDetachedPath starts a new session and escapes it.
	// ChildPIDPath, if set, records the spawned child's pid as this
	// process observed it.
	ChildPath         string
	ChildDetachedPath string
	ChildPIDPath      string

	// ReadyPath, if set, is touched once setup (signal handling and
	// any child spawn) completes and before the handshake loop or
	// the idle wait begins, so a caller that signals the process
	// does not race its own startup.
	ReadyPath string

	// EnvCaptureNames and EnvCapturePath, when both set, append one
	// JSON object per process start to EnvCapturePath, mapping each
	// named variable to its value in this start's environment (empty
	// string if unset), so a test can read back what each of several
	// process starts carried.
	EnvCaptureNames []string
	EnvCapturePath  string

	// AgentInfoNames, when Handshake is set and this list is
	// non-empty, selects the agentInfo.name the initialize response
	// reports: element [min(startIndex, len-1)] of this list, where
	// startIndex is this process's 0-based position among every start
	// sharing SequencePath. A single-element list reports the same
	// name on every start. An empty list reports no agentInfo at all.
	AgentInfoNames []string

	// AgentInfoTitle, when non-empty, is the title the initialize
	// response reports alongside AgentInfoNames' selected name, on
	// every start.
	AgentInfoTitle string

	// SequencePath, when set, is a file this process appends one byte
	// to before consulting AgentInfoNames, so a fixture relaunched by
	// startSession learns its own 0-based start index. Required
	// whenever AgentInfoNames has more than one element.
	SequencePath string

	// RespondPrompts makes the process answer every session/prompt
	// with an end_turn stop reason once session/new has been
	// answered, instead of idling right after the handshake.
	RespondPrompts bool
}

// runProtocolAgent is the fake agent scenario every clientprotocol
// unix test builds through [agenttest.FakeRuntime] with a
// protocolAgentParams tailored to what it needs. It never uses a
// shell: signal handling is syscall.SIGTERM via os/signal, and a
// detached child escapes this process's group via
// syscall.SysProcAttr.Setsid rather than the external setsid binary,
// which keeps the fixture portable to platforms where that binary is
// absent.
func runProtocolAgent(_ []string, params protocolAgentParams) int {
	startIndex := 0
	if params.SequencePath != "" {
		idx, err := nextStartIndex(params.SequencePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "protocol agent: sequence: %v\n", err)
			return 2
		}
		startIndex = idx
	}
	if len(params.EnvCaptureNames) > 0 && params.EnvCapturePath != "" {
		if err := appendEnvCapture(params.EnvCapturePath, params.EnvCaptureNames); err != nil {
			fmt.Fprintf(os.Stderr, "protocol agent: env capture: %v\n", err)
			return 2
		}
	}
	if params.ExitImmediately {
		return 0
	}
	if params.StderrImmediate != "" {
		fmt.Fprintln(os.Stderr, params.StderrImmediate)
		return 1
	}

	installGraceful(params.Graceful, params.GracefulDelay, params.EvidencePath)

	if params.ChildPath != "" {
		if code := spawnChild(params.ChildPath, params.ChildPIDPath, false); code != 0 {
			return code
		}
	}
	if params.ChildDetachedPath != "" {
		if code := spawnChild(params.ChildDetachedPath, params.ChildPIDPath, true); code != 0 {
			return code
		}
	}

	if params.ReadyPath != "" {
		if err := os.WriteFile(params.ReadyPath, nil, 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "protocol agent: write ready marker: %v\n", err)
			return 2
		}
	}

	if params.Handshake {
		if code, exit := runHandshakeLoop(params, startIndex); exit {
			return code
		}
	}

	agenttest.Hang()
	return 0
}

// nextStartIndex appends one byte to path, creating it if necessary, and
// returns the 0-based count of bytes appended so far: 0 on the first call
// against a given path, 1 on the second, and so on.
func nextStartIndex(path string) (int, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	defer f.Close() //nolint:errcheck // best-effort

	if _, err := f.Write([]byte{'.'}); err != nil {
		return 0, err
	}
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return int(info.Size()) - 1, nil
}

// indexedOrLast returns list[min(idx, len(list)-1)], or "" when list is
// empty.
func indexedOrLast(list []string, idx int) string {
	if len(list) == 0 {
		return ""
	}
	if idx >= len(list) {
		idx = len(list) - 1
	}
	return list[idx]
}

// appendEnvCapture appends one JSON object to path, mapping each of names
// to its value in this process's environment.
func appendEnvCapture(path string, names []string) error {
	capture := make(map[string]string, len(names))
	for _, name := range names {
		capture[name] = os.Getenv(name)
	}
	data, err := json.Marshal(capture)
	if err != nil {
		return err
	}
	return appendCaptureLine(path, data)
}

// runHandshakeLoop reads JSON-RPC lines from standard input and answers
// initialize and session/new exactly as startSession expects, applying
// params' capture and early-exit behavior; startIndex selects this
// process's element of params.AgentInfoNames. It returns exit=true when
// the process must stop right away with code; exit=false once the
// handshake (and, with params.RespondPrompts, every prompt) has been
// answered, or standard input has closed, and the caller should fall
// through to idling.
func runHandshakeLoop(params protocolAgentParams, startIndex int) (code int, exit bool) {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)

	sessionCreated := false
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)

		var header wireHeader
		if err := json.Unmarshal(line, &header); err != nil {
			continue
		}

		switch header.Method {
		case methodInitialize:
			name := indexedOrLast(params.AgentInfoNames, startIndex)
			if err := respondInitialize(params.IncludeMCPCapabilities, name, params.AgentInfoTitle); err != nil {
				fmt.Fprintf(os.Stderr, "protocol agent: respond initialize: %v\n", err)
				return 2, true
			}
			if params.StderrOnInit != "" {
				fmt.Fprintln(os.Stderr, params.StderrOnInit)
				return 1, true
			}
		case methodSessionNew:
			if params.CaptureSessionNewPath != "" {
				if err := appendCaptureLine(params.CaptureSessionNewPath, line); err != nil {
					fmt.Fprintf(os.Stderr, "protocol agent: capture session/new: %v\n", err)
					return 2, true
				}
			}
			if err := respondSessionNew(); err != nil {
				fmt.Fprintf(os.Stderr, "protocol agent: respond session/new: %v\n", err)
				return 2, true
			}
			sessionCreated = true
			if !params.RespondPrompts {
				return 0, false
			}
		case methodSessionPrompt:
			if !sessionCreated {
				continue
			}
			if err := writeJSONLine(os.Stdout, outboundResponse{
				JSONRPC: "2.0", ID: idAsInt(header.ID), Result: promptResponse{StopReason: stopReasonEndTurn},
			}); err != nil {
				fmt.Fprintf(os.Stderr, "protocol agent: respond session/prompt: %v\n", err)
				return 2, true
			}
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "protocol agent: read input: %v\n", err)
		return 2, true
	}
	return 0, false
}

// installGraceful arms mode's reaction to SIGTERM. gracefulNone
// installs nothing, leaving the default disposition (process
// termination) in place, matching a fake agent with no signal trap of
// its own.
func installGraceful(mode gracefulMode, delay time.Duration, evidencePath string) {
	switch mode {
	case gracefulIgnore:
		// The Go equivalent of a shell "trap '' TERM": the process
		// becomes permanently immune to the signal, not just for one
		// delivery.
		signal.Ignore(syscall.SIGTERM)
	case gracefulImmediate:
		armGracefulHandler(func() { os.Exit(0) })
	case gracefulDelayed:
		armGracefulHandler(func() {
			time.Sleep(delay)
			_ = os.WriteFile(evidencePath, nil, 0o600)
			os.Exit(0)
		})
	case gracefulNone:
	}
}

// armGracefulHandler runs handle once, the first time this process
// receives SIGTERM.
func armGracefulHandler(handle func()) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM)
	go func() {
		<-sigCh
		handle()
	}()
}

// spawnChild starts path as a child process, and records its pid to
// pidPath if pidPath is non-empty. detached puts the child in a new
// session via Setsid, escaping this process's own group; otherwise the
// child inherits it. Returns a non-zero scenario exit code on failure.
func spawnChild(path, pidPath string, detached bool) int {
	cmd := exec.Command(path) //nolint:gosec // path is a fake runtime this scenario built
	if detached {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "protocol agent: start child: %v\n", err)
		return 2
	}
	if pidPath == "" {
		return 0
	}
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "protocol agent: write child pid: %v\n", err)
		return 2
	}
	return 0
}

// respondInitialize writes the initialize response startSession
// expects, using the pinned schema's own generated types.
func respondInitialize(includeMCPCapabilities bool, agentInfoName, agentInfoTitle string) error {
	resp := initializeResponse{ProtocolVersion: 1}
	if includeMCPCapabilities {
		httpSupported := true
		resp.AgentCapabilities = &agentCapabilities{MCPCapabilities: &mcpCapabilities{HTTP: &httpSupported}}
	}
	if agentInfoName != "" {
		info := &implementation{Name: agentInfoName}
		if agentInfoTitle != "" {
			info.Title = &agentInfoTitle
		}
		resp.AgentInfo = info
	}
	return writeJSONLine(os.Stdout, outboundResponse{JSONRPC: "2.0", ID: 1, Result: resp})
}

// respondSessionNew writes the session/new response startSession
// expects.
func respondSessionNew() error {
	return writeJSONLine(os.Stdout, outboundResponse{JSONRPC: "2.0", ID: 2, Result: newSessionResponse{SessionID: "sess-1"}})
}

// outboundResponse is the envelope a fake agent wraps a result in.
type outboundResponse struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Result  any    `json:"result"`
}

// outboundRequest is the envelope a fake agent wraps an
// agent-initiated call in.
type outboundRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

// writeJSONLine marshals v to w followed by a newline.
func writeJSONLine(w io.Writer, v any) error {
	return json.NewEncoder(w).Encode(v)
}

// appendCaptureLine appends line, followed by a newline, to path,
// creating it if necessary.
func appendCaptureLine(path string, line []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // path is under t.TempDir()
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // best-effort

	if _, err := f.Write(line); err != nil {
		return err
	}
	_, err = f.Write([]byte("\n"))
	return err
}

// parkedAgentParams configures [runParkedAgent], the fake agent used
// by the parked-teardown fixture. ReaderHelperPath and
// WriterHelperPath name already-built fake runtimes that detach into
// their own session and hold, respectively, the standard-input read
// end and the standard-output write end open; StderrHelperPath, when
// set, does the same for standard error.
type parkedAgentParams struct {
	ReaderHelperPath string
	WriterHelperPath string
	StderrHelperPath string
	OptionSize       int
}

// runParkedAgent hands this process's standard input, standard
// output, and (if configured) standard error to detached helper
// processes before writing a session/request_permission request whose
// selected option carries an identifier of at least OptionSize bytes,
// followed by a second, distinct request nothing ever answers, then
// idling. Each helper escapes this process's own group via Setsid, so
// killing that group later leaves the pipe ends they hold open still
// held, exactly as the fixture requires.
func runParkedAgent(_ []string, params parkedAgentParams) int {
	readerCmd := exec.Command(params.ReaderHelperPath) //nolint:gosec // path is a fake runtime this scenario built
	readerCmd.Stdin = os.Stdin
	readerCmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := readerCmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "parked agent: start reader helper: %v\n", err)
		return 2
	}

	writerCmd := exec.Command(params.WriterHelperPath) //nolint:gosec // path is a fake runtime this scenario built
	writerCmd.Stdout = os.Stdout
	writerCmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := writerCmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "parked agent: start writer helper: %v\n", err)
		return 2
	}

	if params.StderrHelperPath != "" {
		stderrCmd := exec.Command(params.StderrHelperPath) //nolint:gosec // path is a fake runtime this scenario built
		stderrCmd.Stderr = os.Stderr
		stderrCmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := stderrCmd.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "parked agent: start stderr helper: %v\n", err)
			return 2
		}
	}

	if err := os.Stdin.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "parked agent: close stdin: %v\n", err)
		return 2
	}

	if err := writeParkedRequests(os.Stdout, params.OptionSize); err != nil {
		fmt.Fprintf(os.Stderr, "parked agent: write requests: %v\n", err)
		return 2
	}

	if err := os.Stdout.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "parked agent: close stdout: %v\n", err)
		return 2
	}

	agenttest.Hang()
	return 0
}

// writeParkedRequests writes the parked-teardown fixture's two
// agent-initiated requests: a session/request_permission whose
// selected option identifier is optionSize bytes long, and a
// fs/read_text_file request nothing ever answers.
func writeParkedRequests(w io.Writer, optionSize int) error {
	title := "work"
	permissionReq := requestPermissionRequest{
		SessionID: "sess-test",
		Options: []permissionOption{
			{Kind: permissionOptionKindRejectOnce, Name: "reject", OptionID: permissionOptionId(hugeOptionID(optionSize))},
		},
		ToolCall: toolCallUpdate{ToolCallID: "tc-1", Title: &title},
	}
	if err := writeJSONLine(w, outboundRequest{JSONRPC: "2.0", ID: 1, Method: methodSessionRequestPermission, Params: permissionReq}); err != nil {
		return err
	}
	return writeJSONLine(w, outboundRequest{JSONRPC: "2.0", ID: 2, Method: methodFsReadTextFile, Params: struct{}{}})
}

// hugeOptionID returns a selected-option identifier of exactly size
// bytes: at least four mebibytes, so no pipe buffer can hold the reply
// that echoes it back.
func hugeOptionID(size int) string {
	return strings.Repeat("x", size)
}

// boundedReaderParams configures [runBoundedReader].
type boundedReaderParams struct {
	PIDPath  string
	DonePath string
	BufSize  int
}

// runBoundedReader records its own pid, performs exactly one bounded
// read of up to BufSize bytes from standard input and discards it,
// writes DonePath as durable evidence that read happened, and idles
// holding standard input's read end open without consuming any more
// of it. The one bounded read, combined with a reply many times larger
// than BufSize, is what parks a subsequent write from the other end.
func runBoundedReader(_ []string, params boundedReaderParams) int {
	if err := writePIDFile(params.PIDPath); err != nil {
		fmt.Fprintf(os.Stderr, "bounded reader: %v\n", err)
		return 2
	}

	buf := make([]byte, params.BufSize)
	_, _ = os.Stdin.Read(buf)

	if err := os.WriteFile(params.DonePath, nil, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "bounded reader: write done marker: %v\n", err)
		return 2
	}

	agenttest.Hang()
	return 0
}

// holdOpenParams configures [runHoldOpen].
type holdOpenParams struct {
	PIDPath string
}

// runHoldOpen records its own pid and idles, holding whichever
// standard stream the caller wired to it open without ever writing to
// or reading from it.
func runHoldOpen(_ []string, params holdOpenParams) int {
	if err := writePIDFile(params.PIDPath); err != nil {
		fmt.Fprintf(os.Stderr, "hold open: %v\n", err)
		return 2
	}
	agenttest.Hang()
	return 0
}

// writePIDFile records this process's own pid to path.
func writePIDFile(path string) error {
	return os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o600)
}
