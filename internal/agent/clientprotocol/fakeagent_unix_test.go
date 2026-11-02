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
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
)

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

type sshStandInParams struct {
	PATH string
}

// runSSHStandIn runs the remote command, the last argument, through sh -c.
// The environment is dropped to PATH alone so a carried variable can reach
// the command only through the session's standard input.
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

type gracefulMode string

const (
	gracefulNone      gracefulMode = ""
	gracefulIgnore    gracefulMode = "ignore"
	gracefulImmediate gracefulMode = "immediate"
	gracefulDelayed   gracefulMode = "delayed"
)

type protocolAgentParams struct {
	ExitImmediately bool
	StderrImmediate string

	// Handshake answers initialize as id 1 and session/new as id 2, the
	// order startSession calls them in.
	Handshake              bool
	IncludeMCPCapabilities bool
	CaptureSessionNewPath  string
	StderrOnInit           string

	Graceful      gracefulMode
	GracefulDelay time.Duration
	EvidencePath  string

	ChildPath         string
	ChildDetachedPath string
	ChildPIDPath      string

	// ReadyPath is touched once signal handling and any child are set up,
	// so a test that signals the process does not race its startup.
	ReadyPath string

	EnvCaptureNames []string
	EnvCapturePath  string

	RespondPrompts bool

	// HeldSession is the one identifier session/resume succeeds for; any
	// other gets the error a runtime that never stored the session sends.
	HeldSession string
}

// runProtocolAgent avoids a shell and the external setsid binary so the
// fixture also runs where that binary is absent.
func runProtocolAgent(_ []string, params protocolAgentParams) int {
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
		if code, exit := runHandshakeLoop(params); exit {
			return code
		}
	}

	agenttest.Hang()
	return 0
}

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

// runHandshakeLoop returns exit=true when the process must stop with code,
// and exit=false when the caller should fall through to idling.
func runHandshakeLoop(params protocolAgentParams) (code int, exit bool) {
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
			if err := respondInitializeHolding(params.IncludeMCPCapabilities, params.HeldSession); err != nil {
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
			if err := respondSessionNewTo(idAsInt(header.ID)); err != nil {
				fmt.Fprintf(os.Stderr, "protocol agent: respond session/new: %v\n", err)
				return 2, true
			}
			sessionCreated = true
			if !params.RespondPrompts {
				return 0, false
			}
		case negativeControlMethod:
			if err := respondError(idAsInt(header.ID), jsonrpc.MethodNotFoundCode); err != nil {
				fmt.Fprintf(os.Stderr, "protocol agent: respond negative control: %v\n", err)
				return 2, true
			}
		case methodSessionResume:
			if err := respondResume(params.HeldSession, line, idAsInt(header.ID)); err != nil {
				fmt.Fprintf(os.Stderr, "protocol agent: respond session/resume: %v\n", err)
				return 2, true
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

func installGraceful(mode gracefulMode, delay time.Duration, evidencePath string) {
	switch mode {
	case gracefulIgnore:
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

func armGracefulHandler(handle func()) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM)
	go func() {
		<-sigCh
		handle()
	}()
}

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

func respondInitialize(includeMCPCapabilities bool) error {
	return respondInitializeHolding(includeMCPCapabilities, "")
}

func respondInitializeHolding(includeMCPCapabilities bool, held string) error {
	resp := initializeResponse{ProtocolVersion: 1}
	if includeMCPCapabilities || held != "" {
		resp.AgentCapabilities = &agentCapabilities{}
	}
	if includeMCPCapabilities {
		httpSupported := true
		resp.AgentCapabilities.MCPCapabilities = &mcpCapabilities{HTTP: &httpSupported}
	}
	if held != "" {
		resp.AgentCapabilities.SessionCapabilities = &sessionCapabilities{Resume: &sessionResumeCapabilities{}}
	}
	return writeJSONLine(os.Stdout, outboundResponse{JSONRPC: "2.0", ID: 1, Result: resp})
}

func respondSessionNew() error {
	return respondSessionNewTo(2)
}

func respondSessionNewTo(id int) error {
	return writeJSONLine(os.Stdout, outboundResponse{JSONRPC: "2.0", ID: id, Result: newSessionResponse{SessionID: "sess-1"}})
}

func respondResume(held string, line []byte, id int) error {
	var request struct {
		Params resumeSessionRequest `json:"params"`
	}
	if err := json.Unmarshal(line, &request); err != nil {
		return err
	}
	if string(request.Params.SessionID) != held {
		return respondError(id, sessionNotFoundCode)
	}
	return writeJSONLine(os.Stdout, outboundResponse{JSONRPC: "2.0", ID: id, Result: resumeSessionResponse{}})
}

const sessionNotFoundCode = -32002

type outboundError struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Error   struct {
		Code int `json:"code"`
	} `json:"error"`
}

func respondError(id, code int) error {
	response := outboundError{JSONRPC: "2.0", ID: id}
	response.Error.Code = code
	return writeJSONLine(os.Stdout, response)
}

type outboundResponse struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Result  any    `json:"result"`
}

type outboundRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

func writeJSONLine(w io.Writer, v any) error {
	return json.NewEncoder(w).Encode(v)
}

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

type parkedAgentParams struct {
	ReaderHelperPath string
	WriterHelperPath string
	StderrHelperPath string
	OptionSize       int
}

// runParkedAgent hands its standard streams to helpers in their own
// sessions, so killing this process's group leaves the pipe ends held open.
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

func hugeOptionID(size int) string {
	return strings.Repeat("x", size)
}

type boundedReaderParams struct {
	PIDPath  string
	DonePath string
	BufSize  int
}

// runBoundedReader reads once and then holds standard input open unread,
// so a reply many times larger than BufSize parks its writer.
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

type holdOpenParams struct {
	PIDPath string
}

func runHoldOpen(_ []string, params holdOpenParams) int {
	if err := writePIDFile(params.PIDPath); err != nil {
		fmt.Fprintf(os.Stderr, "hold open: %v\n", err)
		return 2
	}
	agenttest.Hang()
	return 0
}

func writePIDFile(path string) error {
	return os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o600)
}
