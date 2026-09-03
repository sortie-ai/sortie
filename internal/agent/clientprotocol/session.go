package clientprotocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/domain"
)

// clientProtocolMaxLineBytes is the connection's line bound, raised from
// the shared package's one-mebibyte default to the ten megabytes the
// local subprocess launch contract recommends and the shared
// fork-per-turn path already uses.
const clientProtocolMaxLineBytes = 10 * 1024 * 1024

// pinnedProtocolVersion is the wire version this adapter was generated
// against. A handshake reporting any other value ends the session: the
// protocol defines no error for a version mismatch and leaves the
// decision to disconnect to the client.
const pinnedProtocolVersion = 1

// defaultReadTimeout bounds a synchronous wait on the agent when
// agent.read_timeout_ms is not set.
const defaultReadTimeout = 30 * time.Second

// pumpChannelCapacity bounds the pump's single input channel. The
// registered handler's send blocks past this capacity by design: the
// pump drains it continuously, and the bound only keeps an ordinary
// burst of updates from serializing one at a time through the
// scheduler.
const pumpChannelCapacity = 64

// sessionState is this adapter's own session state, reached through
// domain.Session.Internal. Fields set once during StartSession, before
// the pump starts, are read-only afterward from every other goroutine;
// every field the pump itself owns lives on the pump's own local state
// instead, so nothing outside the pump's goroutine can reach it.
type sessionState struct {
	target      agentcore.LaunchTarget
	agentConfig domain.AgentConfig

	// caps is built with its stage-one states before the pump starts and
	// is owned by the pump from that point on: nothing outside the pump
	// goroutine may mutate the value it points to.
	caps *capabilityRecord

	conn *jsonrpc.Conn

	pid             int
	stdinCloser     io.Closer
	stdoutCloser    io.Closer
	stderrCollector *procutil.StderrCollector
	waitCh          chan struct{} // closed once the subprocess has been reaped

	itemCh   chan pumpItem
	stopCh   chan struct{}
	stopOnce sync.Once
	pumpDone chan struct{}

	logger *slog.Logger
}

// pumpItem is either a message routed from the connection's handler or
// a control message published by StartSession, RunTurn, or teardown.
// Exactly one field is set.
type pumpItem struct {
	msg     *jsonrpc.Message
	control *pumpControl
}

// pumpControl carries what StartSession learned on its own goroutine
// and what RunTurn and teardown ask of the pump. Exactly one field is
// set.
type pumpControl struct {
	handshake *handshakeFacts
	sessionID string

	// expectLoad is published before a session/load call, naming the
	// identifier being loaded. It lets the pump treat that identifier
	// as the session's own before the definitive sessionID control
	// message arrives, and count any chunk replayed for it.
	expectLoad string

	// query carries the continuation verdict of a session/load or
	// session/resume call once that call's own response is known. See
	// [replayQuery].
	query *replayQuery

	startTurn  *turnStart
	answerOpen bool
}

// replayQuery is the control message a session/load or session/resume
// continuation call publishes once its own response is known, so the
// pump, the capability record's sole mutator, applies the
// sessionContinuation lowering. A call already known to have failed
// carries a nil reply: the pump lowers the entry at once and answers
// nothing, because the caller already has its verdict and is not
// waiting. A session/load call that answered success carries a
// buffered reply of capacity one instead: the pump answers true as
// soon as it observes a chunk replayed for the identifier the
// expectLoad control message named, or lowers the entry itself and
// answers false once its own bounded wait for that chunk elapses.
type replayQuery struct {
	reply chan bool
}

// handshakeFacts is what StartSession decoded from the initialize
// response, decided once on StartSession's own goroutine and carried to
// the pump as a fact rather than re-derived there.
type handshakeFacts struct {
	agentInfo        implementation
	agentInfoPresent bool
	caps             agentCapabilities

	// toolServersWithheld reports whether the generated configuration
	// declared an HTTP tool server that this handshake's advertised MCP
	// capabilities do not support, so it was omitted from session/new.
	toolServersWithheld bool
}

// turnStart is what RunTurn publishes to start one turn.
type turnStart struct {
	prompt   string
	sink     chan domain.AgentEvent
	resultCh chan turnEnd
	done     chan struct{}
	cancelCh chan struct{}
	reply    chan turnVerdict
}

// turnVerdict is the pump's accept-or-reject answer to a startTurn
// control message, delivered on that message's own capacity-one reply
// channel.
type turnVerdict struct {
	accepted bool
	err      *domain.AgentError
}

// turnEnd is the pump's final report for one turn, delivered on the
// turn's own capacity-one result channel.
type turnEnd struct {
	result domain.TurnResult
	err    *domain.AgentError
}

// readTimeout returns the read timeout duration from the agent config,
// defaulting to defaultReadTimeout.
func readTimeout(state *sessionState) time.Duration {
	if state.agentConfig.ReadTimeoutMS > 0 {
		return time.Duration(state.agentConfig.ReadTimeoutMS) * time.Millisecond
	}
	return defaultReadTimeout
}

// startSession launches the runtime, performs the initialize handshake,
// and creates or continues a session. A non-empty ResumeSessionID picks
// the continuation route the handshake advertises; every other case,
// and a continuation that is not confirmed, creates the session with
// session/new instead. The session capability record is built with its
// stage-one states before the pump starts, and the pump applies
// handshake- and continuation-based lowering to it once the
// corresponding control message arrives.
func startSession(ctx context.Context, params domain.StartSessionParams) (domain.Session, error) {
	target, agentErr := agentcore.ResolveLaunchTarget(params, "")
	if agentErr != nil {
		return domain.Session{}, agentErr
	}

	remote := target.RemoteCommand != ""

	parsedServers, agentErr := parseMCPServers(params.MCPConfigPath, remote)
	if agentErr != nil {
		return domain.Session{}, agentErr
	}

	state := &sessionState{
		target:      target,
		agentConfig: params.AgentConfig,
		itemCh:      make(chan pumpItem, pumpChannelCapacity),
		stopCh:      make(chan struct{}),
		pumpDone:    make(chan struct{}),
		logger:      slog.Default().With(slog.String("component", "clientprotocol-adapter")),
	}

	var cmd *exec.Cmd
	if remote {
		sshArgs := sshutil.BuildSSHArgs(target.SSHHost, target.WorkspacePath, target.RemoteCommand, nil, sshutil.SSHOptions{
			StrictHostKeyChecking: target.SSHStrictHostKeyChecking,
		})
		cmd = exec.CommandContext(ctx, target.Command, sshArgs...) //nolint:gosec // args are constructed programmatically with shell quoting
	} else {
		cmd = exec.CommandContext(ctx, target.Command, target.Args...) //nolint:gosec // args are constructed programmatically
		cmd.Dir = target.WorkspacePath
	}
	procutil.SetProcessGroup(cmd)
	cmd.Env = os.Environ()

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return domain.Session{}, &domain.AgentError{Kind: domain.ErrPortExit, Message: "failed to create stdin pipe", Err: err}
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return domain.Session{}, &domain.AgentError{Kind: domain.ErrPortExit, Message: "failed to create stdout pipe", Err: err}
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return domain.Session{}, &domain.AgentError{Kind: domain.ErrPortExit, Message: "failed to create stderr pipe", Err: err}
	}

	if err := cmd.Start(); err != nil {
		return domain.Session{}, &domain.AgentError{Kind: domain.ErrPortExit, Message: "failed to start subprocess", Err: err}
	}

	if assignErr := procutil.AssignProcess(cmd.Process.Pid, cmd.Process); assignErr != nil {
		state.logger.Warn("process group assignment failed", slog.Any("error", assignErr))
	}

	state.pid = cmd.Process.Pid
	state.stdinCloser = stdinPipe
	state.stdoutCloser = stdoutPipe
	state.waitCh = make(chan struct{})
	state.stderrCollector = procutil.NewStderrCollector(stderrPipe, state.logger)

	state.conn = jsonrpc.NewConn(stdinPipe, stdoutPipe, pumpHandler(state.itemCh, state.stopCh),
		jsonrpc.WithVersionMember(), jsonrpc.WithMaxLineBytes(clientProtocolMaxLineBytes))

	// The reaper waits for the connection's reader to finish before it
	// reaps. cmd.Wait closes the pipe StdoutPipe returned, so reaping
	// while the reader is still consuming would truncate the stream: an
	// agent that writes its final response and exits would have that
	// response discarded and the turn reported as a lost subprocess.
	// Teardown closes the read handle itself, so this wait always ends.
	go func() {
		<-state.conn.Done()
		cmd.Wait()                                 //nolint:errcheck,gosec // exit code is not read; teardown only waits for the process to be gone
		procutil.KillProcessGroup(cmd.Process.Pid) //nolint:errcheck,gosec // best-effort cleanup of surviving group members
		procutil.CleanupProcess(cmd.Process.Pid)
		close(state.waitCh)
	}()

	// The capability record is built here, on this goroutine, with its
	// stage-one states, before the pump starts. The pump's start orders
	// this write exactly as it orders the launch target beside it:
	// StartSession must not touch state.caps after this point.
	state.caps = newCapabilityRecord(remote)

	// Start the pump before the handshake, so it is the sole mutator of
	// session protocol state from this point on; StartSession publishes
	// what it learns as control messages rather than writing that state
	// itself.
	go runPump(state)

	teardownOnFailure := func() { runTeardown(state, defaultTeardownOrder()) }

	initResp, agentErr := doInitialize(ctx, state)
	if agentErr != nil {
		teardownOnFailure()
		return domain.Session{}, agentErr
	}

	allowHTTP := initResp.AgentCapabilities != nil &&
		initResp.AgentCapabilities.MCPCapabilities != nil &&
		initResp.AgentCapabilities.MCPCapabilities.HTTP != nil &&
		*initResp.AgentCapabilities.MCPCapabilities.HTTP
	wireServers, withheld := parsedServers.wireServers(allowHTTP)
	if withheld {
		state.logger.Warn("configured tool servers were not delivered: the agent does not advertise HTTP tool-server support")
	}

	var caps agentCapabilities
	if initResp.AgentCapabilities != nil {
		caps = *initResp.AgentCapabilities
	}

	sessionID, agentErr := resolveSession(ctx, state, params.ResumeSessionID, caps, target.WorkspacePath, wireServers)
	if agentErr != nil {
		teardownOnFailure()
		return domain.Session{}, agentErr
	}

	facts := &handshakeFacts{toolServersWithheld: withheld, caps: caps}
	if initResp.AgentInfo != nil {
		facts.agentInfo = *initResp.AgentInfo
		facts.agentInfoPresent = true
	}

	state.itemCh <- pumpItem{control: &pumpControl{handshake: facts}}
	state.itemCh <- pumpItem{control: &pumpControl{sessionID: sessionID}}

	return domain.Session{
		ID:       sessionID,
		AgentPID: strconv.Itoa(state.pid),
		Internal: state,
	}, nil
}

// pumpHandler returns the jsonrpc.Handler registered on the connection.
// It does nothing but hand the message to the pump, with a two-arm
// select against the session's stop channel, so that teardown cannot
// leave the connection's reader goroutine blocked handing off a message
// the pump will never drain.
func pumpHandler(itemCh chan pumpItem, stopCh chan struct{}) jsonrpc.Handler {
	return func(msg jsonrpc.Message) {
		m := msg
		select {
		case itemCh <- pumpItem{msg: &m}:
		case <-stopCh:
		}
	}
}

// doInitialize sends the initialize request and validates the pinned
// protocol version.
func doInitialize(ctx context.Context, state *sessionState) (*initializeResponse, *domain.AgentError) {
	callCtx, cancel := context.WithTimeout(ctx, readTimeout(state))
	defer cancel()

	no := false
	req := initializeRequest{
		ProtocolVersion: protocolVersion(pinnedProtocolVersion),
		ClientCapabilities: &clientCapabilities{
			Fs:       &fileSystemCapabilities{ReadTextFile: &no, WriteTextFile: &no},
			Terminal: &no,
		},
	}

	resp, err := state.conn.Call(callCtx, methodInitialize, req)
	if agentErr := translateCallError(err, callCtx); agentErr != nil {
		return nil, agentErr
	}
	if resp.Error != nil {
		return nil, &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: fmt.Sprintf("initialize error %d: %s", resp.Error.Code, resp.Error.Message),
		}
	}

	var initResp initializeResponse
	if err := json.Unmarshal(resp.Result, &initResp); err != nil {
		return nil, &domain.AgentError{Kind: domain.ErrResponseError, Message: "initialize response did not decode", Err: err}
	}
	if int64(initResp.ProtocolVersion) != pinnedProtocolVersion {
		return nil, &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: fmt.Sprintf("agent reported protocol version %d, want %d", initResp.ProtocolVersion, pinnedProtocolVersion),
		}
	}
	return &initResp, nil
}

// doNewSession sends session/new with the resolved cwd and the already-
// filtered server list.
func doNewSession(ctx context.Context, state *sessionState, cwd string, servers []mcpServer) (*newSessionResponse, *domain.AgentError) {
	callCtx, cancel := context.WithTimeout(ctx, readTimeout(state))
	defer cancel()

	req := newSessionRequest{Cwd: cwd, MCPServers: servers}
	resp, err := state.conn.Call(callCtx, methodSessionNew, req)
	if agentErr := translateCallError(err, callCtx); agentErr != nil {
		return nil, agentErr
	}
	if resp.Error != nil {
		return nil, &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: fmt.Sprintf("session/new error %d: %s", resp.Error.Code, resp.Error.Message),
		}
	}

	var newResp newSessionResponse
	if err := json.Unmarshal(resp.Result, &newResp); err != nil {
		return nil, &domain.AgentError{Kind: domain.ErrResponseError, Message: "session/new response did not decode", Err: err}
	}
	return &newResp, nil
}

// translateCallError maps a jsonrpc.Conn.Call failure to the normalized
// failure table: a timeout against callCtx's own deadline is
// response_timeout, and every other failure (a closed connection, a
// stream end, or the outer context ending) is port_exit, the loss of
// the subprocess.
func translateCallError(err error, callCtx context.Context) *domain.AgentError {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) && callCtx.Err() == context.DeadlineExceeded {
		return &domain.AgentError{Kind: domain.ErrResponseTimeout, Message: "timed out waiting for a response", Err: err}
	}
	return &domain.AgentError{Kind: domain.ErrPortExit, Message: "agent connection ended before responding", Err: err}
}

// textContentBlock builds a well-formed text content block from
// scratch, reusing the generator's own discriminant-injection helper:
// contentBlock's generated form only round-trips an already-decoded
// value, so a client that writes one, rather than echoing one it read,
// builds the bytes itself.
func textContentBlock(text string) contentBlock {
	data, err := marshalWithDiscriminant(textContent{Text: text}, "type", contentBlockText)
	if err != nil {
		panic(fmt.Sprintf("clientprotocol: marshal text content block: %v", err))
	}
	return contentBlock{Type: contentBlockText, Remainder: data}
}

// runTurn publishes a startTurn control message and relays the pump's
// events and final result to the caller.
func runTurn(ctx context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
	if params.OnEvent == nil {
		panic("clientprotocol: OnEvent must be non-nil")
	}

	state, ok := session.Internal.(*sessionState)
	if !ok {
		return domain.TurnResult{}, &domain.AgentError{
			Kind:    domain.ErrPortExit,
			Message: fmt.Sprintf("unexpected session internal type %T", session.Internal),
		}
	}

	ts := &turnStart{
		prompt:   params.Prompt,
		sink:     make(chan domain.AgentEvent),
		resultCh: make(chan turnEnd, 1),
		done:     make(chan struct{}),
		cancelCh: make(chan struct{}),
		reply:    make(chan turnVerdict, 1),
	}

	sendTimer := time.NewTimer(readTimeout(state))
	select {
	case state.itemCh <- pumpItem{control: &pumpControl{startTurn: ts}}:
		sendTimer.Stop()
	case <-ctx.Done():
		sendTimer.Stop()
		close(ts.done)
		return domain.TurnResult{}, ctx.Err()
	case <-sendTimer.C:
		close(ts.done)
		return domain.TurnResult{}, &domain.AgentError{
			Kind:    domain.ErrResponseTimeout,
			Message: "timed out publishing the turn to the agent connection",
			Err:     context.DeadlineExceeded,
		}
	}

	replyTimer := time.NewTimer(readTimeout(state))
	var verdict turnVerdict
	select {
	case verdict = <-ts.reply:
		replyTimer.Stop()
	case <-ctx.Done():
		replyTimer.Stop()
		close(ts.done)
		close(ts.cancelCh)
		return domain.TurnResult{}, ctx.Err()
	case <-replyTimer.C:
		close(ts.done)
		close(ts.cancelCh)
		return domain.TurnResult{}, &domain.AgentError{
			Kind:    domain.ErrResponseTimeout,
			Message: "timed out waiting for the agent connection to start the turn",
			Err:     context.DeadlineExceeded,
		}
	}
	if !verdict.accepted {
		close(ts.done)
		return domain.TurnResult{}, verdict.err
	}

	ctxDone := ctx.Done()
	for {
		select {
		case ev := <-ts.sink:
			params.OnEvent(ev)
		case end := <-ts.resultCh:
			close(ts.done)
			if end.err != nil {
				return end.result, end.err
			}
			return end.result, nil
		case <-ctxDone:
			ctxDone = nil
			close(ts.cancelCh)
		}
	}
}

// stopSession runs teardown's fixed step order.
func stopSession(_ context.Context, session domain.Session) error {
	state, ok := session.Internal.(*sessionState)
	if !ok {
		return fmt.Errorf("unexpected session internal type %T", session.Internal)
	}
	runTeardown(state, defaultTeardownOrder())
	return nil
}

// teardownStep is one action of StopSession's fixed order.
type teardownStep struct {
	name string
	run  func(state *sessionState)
}

// defaultTeardownOrder is the one place StopSession's step order is
// expressed. Every step tolerates a session that never reached a
// running state: each guards its own preconditions and does nothing
// when they are not met, so StopSession never panics on a
// partially-constructed session.
//
// No step waits in front of terminating the process group and closing
// the standard-input and standard-output handles: a write the pump has
// begun and the agent has stopped reading cannot complete on its own,
// and closing those handles is what releases it, in the order below,
// before the connection is closed.
func defaultTeardownOrder() []teardownStep {
	return []teardownStep{
		{name: "answer_open", run: signalAnswerOpen},
		{name: "kill_process_group", run: killProcessGroup},
		{name: "close_stdin", run: closeStdin},
		{name: "close_stdout", run: closeStdout},
		{name: "close_connection", run: closeConnection},
		{name: "stop_pump", run: stopPump},
		{name: "drain_stderr_and_reap", run: drainStderrAndReap},
	}
}

// runTeardown walks steps in order, running every one.
func runTeardown(state *sessionState, steps []teardownStep) {
	for _, step := range steps {
		step.run(state)
	}
}

// signalAnswerOpen sends the answerOpen control message with a send
// that gives up when the channel has no room, and does not wait for the
// pump to write the reply.
func signalAnswerOpen(state *sessionState) {
	if state.itemCh == nil {
		return
	}
	select {
	case state.itemCh <- pumpItem{control: &pumpControl{answerOpen: true}}:
	default:
		state.logger.Warn("teardown could not signal the pump to answer any open request: its input channel had no room")
	}
}

// killProcessGroup terminates the subprocess's process group.
func killProcessGroup(state *sessionState) {
	if state.pid > 0 {
		procutil.KillProcessGroup(state.pid) //nolint:errcheck,gosec // best-effort
	}
}

// closeStdin closes the handle the adapter writes the agent's standard
// input through. This fails a write the pump has parked on that pipe,
// letting the pump resume.
func closeStdin(state *sessionState) {
	if state.stdinCloser != nil {
		state.stdinCloser.Close() //nolint:errcheck,gosec // best-effort; unparks a write blocked on this pipe
	}
}

// closeStdout closes the handle the connection reads the agent's
// standard output through. This ends a scan a descendant holding that
// pipe's write end would otherwise keep parked.
func closeStdout(state *sessionState) {
	if state.stdoutCloser != nil {
		state.stdoutCloser.Close() //nolint:errcheck,gosec // best-effort; unparks the connection's read
	}
}

// closeConnection closes the JSON-RPC connection. Closing it before the
// process group is terminated and the handles above are closed would
// wait on the same parked write those two actions exist to release.
func closeConnection(state *sessionState) {
	if state.conn != nil {
		state.conn.Close()
	}
}

// stopPump signals the pump to exit and waits for it, which the
// standard-output close above makes certain.
func stopPump(state *sessionState) {
	if state.stopCh == nil {
		return
	}
	state.stopOnce.Do(func() { close(state.stopCh) })
	if state.pumpDone != nil {
		<-state.pumpDone
	}
}

// drainStderrAndReap waits for the bounded standard-error drain, reaps
// the process, and on a drain that has not finished waits once more and
// abandons it, so this step spends procutil.DefaultDrainGrace at most
// twice in total.
func drainStderrAndReap(state *sessionState) {
	var drained bool
	if state.stderrCollector != nil {
		drained = state.stderrCollector.WaitDone(procutil.DefaultDrainGrace)
	} else {
		drained = true
	}

	if state.waitCh != nil {
		<-state.waitCh
	}

	if !drained && state.stderrCollector != nil {
		if !state.stderrCollector.WaitDone(procutil.DefaultDrainGrace) {
			state.stderrCollector.Abandon(procutil.DefaultDrainGrace)
		}
	}
}
