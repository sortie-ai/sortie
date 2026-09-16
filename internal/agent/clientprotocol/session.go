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

// sessionState is this adapter's own session state, reached through
// domain.Session.Internal. Fields set once during StartSession, before
// the pump starts, are read-only afterward from every other goroutine;
// every field the pump itself owns lives on the pump's own local state
// instead, so nothing outside the pump's goroutine can reach it. A
// third class holds a field written once on the StartSession goroutine
// after the pump has already started: safe because the pump never
// reads it, and its only reader is teardown, which runs either on that
// same goroutine while the field still holds its zero value or after
// StartSession has returned.
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
	pipes           *procutil.OwnedPipes
	stderrCollector *procutil.StderrCollector
	waitCh          <-chan struct{} // closed once the subprocess has been reaped

	// inbox is where the connection's reader delivers routed messages
	// and every control publish lands, in one order. runPump is its
	// only taker; the inbox is never closed, so the pump keeps taking
	// late controls until stopCh closes.
	inbox    *jsonrpc.Inbox[pumpItem]
	stopCh   chan struct{}
	stopOnce sync.Once
	pumpDone chan struct{}

	// closeSessionID is written once, on the StartSession goroutine,
	// immediately after resolveSession returns a session identifier and
	// before StartSession returns. It is left at its zero value when the
	// handshake does not advertise session/close. Teardown is its only
	// reader.
	closeSessionID string

	logger *slog.Logger

	// origins is the adapter-wide creation ledger, shared by every
	// session the adapter starts.
	origins *sessionOrigins

	// drainGrace bounds the post-reap release's wait for the
	// connection's own reader to end normally after the subprocess has
	// been reaped. Copied from ClientProtocolAdapter.drainGrace in
	// startSession, resolving a non-positive value to
	// procutil.DefaultDrainGrace.
	drainGrace time.Duration

	// release is the shared post-reap release that gives the
	// connection's reader up to drainGrace to end on its own before
	// giving up on it. Set once in startSession before the pump starts,
	// read-only afterward.
	release *procutil.OutputRelease
}

// pumpItem is either a message the connection's reader delivered or a
// control message published by StartSession, RunTurn, or teardown.
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

	startTurn *turnStart

	// answerOpen, when non-nil, tells the pump to answer every request
	// still open; the pump closes it once handleAnswerOpen returns.
	answerOpen chan struct{}
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

	// toolServersDelivered reports whether the session-creation request
	// carried at least one tool server.
	toolServersDelivered bool
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
func startSession(ctx context.Context, a *ClientProtocolAdapter, params domain.StartSessionParams) (domain.Session, error) {
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
		stopCh:      make(chan struct{}),
		pumpDone:    make(chan struct{}),
		logger:      slog.Default().With(slog.String("component", "clientprotocol-adapter")),
		origins:     &a.origins,
	}
	state.drainGrace = a.drainGrace
	if state.drainGrace <= 0 {
		state.drainGrace = procutil.DefaultDrainGrace
	}
	state.inbox = jsonrpc.NewInbox[pumpItem]()

	var cmd *exec.Cmd
	var launch sshutil.SSHLaunch
	if remote {
		launch = sshutil.BuildSSHLaunch(target.SSHHost, target.WorkspacePath, target.RemoteCommand, nil, target.SSHOptions())
		cmd = exec.CommandContext(ctx, target.Command, launch.Args...) //nolint:gosec // args are constructed programmatically with shell quoting
	} else {
		cmd = exec.CommandContext(ctx, target.Command, target.Args...) //nolint:gosec // args are constructed programmatically
		cmd.Dir = target.WorkspacePath
	}
	grace := procutil.StopGrace(state.agentConfig.StopGraceMS)
	procutil.SetGroupCancel(cmd, grace)
	cmd.Env = os.Environ()

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return domain.Session{}, &domain.AgentError{Kind: domain.ErrPortExit, Message: "failed to create stdin pipe", Err: err}
	}
	prefixedStdin := launch.PrefixStdin(stdinPipe)

	pipes, err := procutil.StartWithOwnedPipes(cmd, state.logger)
	if err != nil {
		var startErr *procutil.StartError
		if !errors.As(err, &startErr) {
			return domain.Session{}, &domain.AgentError{Kind: domain.ErrPortExit, Message: "failed to start subprocess", Err: err}
		}
		// Both pipe stages fail before cmd.Start, whose deferred cleanup
		// is what closes the parent's stdin end on a failed launch, so
		// this closes it instead. The process-start and resume stages
		// need none.
		switch startErr.Stage {
		case procutil.StageStdoutPipe:
			stdinPipe.Close() //nolint:errcheck,gosec // best-effort; the pipe error is what the caller needs
			return domain.Session{}, &domain.AgentError{Kind: domain.ErrPortExit, Message: "failed to create stdout pipe", Err: startErr.Err}
		case procutil.StageStderrPipe:
			stdinPipe.Close() //nolint:errcheck,gosec // best-effort; the pipe error is what the caller needs
			return domain.Session{}, &domain.AgentError{Kind: domain.ErrPortExit, Message: "failed to create stderr pipe", Err: startErr.Err}
		default: // procutil.StageProcessStart, procutil.StageProcessResume
			return domain.Session{}, &domain.AgentError{Kind: domain.ErrPortExit, Message: "failed to start subprocess", Err: startErr.Err}
		}
	}

	state.pid = cmd.Process.Pid
	state.stdinCloser = prefixedStdin
	state.pipes = pipes
	state.stderrCollector = procutil.NewStderrCollector(pipes.Stderr, state.logger)

	state.conn = jsonrpc.NewConn(prefixedStdin, pipes.Stdout, jsonrpc.Deliver(state.inbox, wrapPumpMessage),
		jsonrpc.WithVersionMember(), jsonrpc.WithMaxLineBytes(clientProtocolMaxLineBytes))

	// The reap runs independently of the connection's reader: the pipes
	// are caller-owned, so exec.Cmd.Wait closes neither read end and
	// reaping cannot cut a reader still consuming buffered output short.
	// Teardown's close_stdout and close_pipes steps are what end that
	// reader.
	reaper := procutil.StartReaper(cmd, state.logger)
	state.waitCh = reaper.Done()

	// The release ends a handshake call or a turn that would otherwise
	// wait forever on a reaped runtime whose reader did not end inside
	// the drain bound. Stderr stays nil: this kind's own
	// drain_stderr_and_reap and close_pipes teardown steps already
	// bound and release that stream, and moving the bound earlier would
	// change the pinned teardown ceiling.
	state.release = procutil.StartOutputRelease(procutil.OutputReleaseParams{
		Pipes:      pipes,
		Reaped:     reaper.Done(),
		ReaderDone: state.conn.Done(),
		OnAbandon:  state.conn.Close,
		Grace:      state.drainGrace,
		Logger:     state.logger,
	})

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

	teardownOnFailure := func() {
		graceCtx, cancel := context.WithTimeout(ctx, grace)
		defer cancel()
		runTeardown(state, defaultTeardownOrder(ctx, graceCtx, grace))
	}

	initResp, agentErr := doInitialize(ctx, state)
	if agentErr != nil {
		teardownOnFailure()
		procutil.EmitWarnLines(state.stderrCollector.Lines(), state.logger)
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
		procutil.EmitWarnLines(state.stderrCollector.Lines(), state.logger)
		return domain.Session{}, agentErr
	}
	if advertisesSessionClose(caps) {
		state.closeSessionID = sessionID
	}

	facts := &handshakeFacts{toolServersWithheld: withheld, toolServersDelivered: len(wireServers) > 0, caps: caps}
	if initResp.AgentInfo != nil {
		facts.agentInfo = *initResp.AgentInfo
		facts.agentInfoPresent = true
	}

	state.inbox.Put(pumpItem{control: &pumpControl{handshake: facts}})
	state.inbox.Put(pumpItem{control: &pumpControl{sessionID: sessionID}})

	return domain.Session{
		ID:       sessionID,
		AgentPID: strconv.Itoa(state.pid),
		Internal: state,
	}, nil
}

// wrapPumpMessage wraps msg, over its own copy, as the pump item the
// connection's reader delivers into the inbox.
func wrapPumpMessage(msg jsonrpc.Message) pumpItem {
	m := msg
	return pumpItem{msg: &m}
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

	state.inbox.Put(pumpItem{control: &pumpControl{startTurn: ts}})

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

// closeCallBound returns the ceiling the close_session teardown step
// spends waiting for a session/close response: half of the window it
// is given, truncated toward zero.
func closeCallBound(grace time.Duration) time.Duration {
	return grace / 2
}

// closeCallOutcome carries a session/close call's raw result from the
// goroutine that issues it to the step that classifies it.
type closeCallOutcome struct {
	resp jsonrpc.Response
	err  error
}

// closeSession returns a teardown step that issues one session/close
// call for state.closeSessionID, bounded by half of whatever remains
// on graceCtx. It does nothing when no identifier was recorded or the
// connection is already gone.
//
// The call itself enqueues and waits under callCtx: nothing about a
// runtime that has stopped reading its standard input can hold this
// step past its own bound, so it runs directly on teardown's goroutine
// rather than needing one of its own to bound.
func closeSession(callerCtx, graceCtx context.Context, grace time.Duration) func(state *sessionState) {
	return func(state *sessionState) {
		if state.closeSessionID == "" || state.conn == nil {
			return
		}
		conn, id := state.conn, state.closeSessionID

		// Half of what remains on graceCtx rather than half of the
		// configured grace: a caller deadline nearer than that grace
		// would otherwise let this one call spend the whole graceful
		// window and starve the signal behind it.
		bound := closeCallBound(grace)
		if deadline, ok := graceCtx.Deadline(); ok {
			if half := closeCallBound(max(time.Until(deadline), 0)); half < bound {
				bound = half
			}
		}
		callCtx, cancel := context.WithTimeout(graceCtx, bound)
		defer cancel()

		resp, err := conn.Call(callCtx, methodSessionClose, closeSessionRequest{SessionID: sessionId(id)})
		logCloseSessionOutcome(state, callerCtx, bound, closeCallOutcome{resp: resp, err: err})
	}
}

// logCloseSessionOutcome classifies a completed session/close call. A
// response with no error member logs at Debug with no fields; a
// response carrying a JSON-RPC error logs at Warn with the numeric
// code only, never the peer's message text, matching
// logContinuationFailure's own precedent. A call that did not complete
// because callCtx ended logs at Warn, naming bound and whether it was
// the step's own bound or callerCtx's own deadline that ended the wait;
// any other call failure (a closed connection, a stream end, or a
// write failure) logs at Debug with no fields, because the process is
// already going away.
func logCloseSessionOutcome(state *sessionState, callerCtx context.Context, bound time.Duration, got closeCallOutcome) {
	if got.err != nil {
		if errors.Is(got.err, context.DeadlineExceeded) || errors.Is(got.err, context.Canceled) {
			outcome := "bound elapsed"
			if callerCtx.Err() != nil {
				outcome = "caller deadline"
			}
			state.logger.Warn("session/close did not complete before the wait ended",
				slog.Duration("bound", bound), slog.String("outcome", outcome))
			return
		}
		state.logger.Debug("session/close call did not complete")
		return
	}
	if got.resp.Error != nil {
		state.logger.Warn("session/close returned an error", slog.Int("code", got.resp.Error.Code))
		return
	}
	state.logger.Debug("session closed through the protocol")
}

// stopSession runs teardown's fixed step order.
func stopSession(ctx context.Context, session domain.Session) error {
	state, ok := session.Internal.(*sessionState)
	if !ok {
		return fmt.Errorf("unexpected session internal type %T", session.Internal)
	}
	grace := procutil.StopGrace(state.agentConfig.StopGraceMS)
	graceCtx, cancel := context.WithTimeout(ctx, grace)
	defer cancel()
	runTeardown(state, defaultTeardownOrder(ctx, graceCtx, grace))
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
// close_session runs immediately after answer_open and issues
// a bounded session/close call when the handshake advertised the
// capability: it precedes the graceful signal because closing the
// session is only worth attempting while the runtime is still running
// normally. It never delays the signal past its own bound, and the
// rest of the order is unchanged.
//
// The order gives the agent a bounded graceful phase before the
// unconditional group kill: signal_graceful sends a catchable
// termination signal to the launched process group (on a remote
// launch, the local relay's group, not a runtime reached only through
// it), close_stdin then hands the runtime end-of-input immediately
// behind that signal, and await_exit waits for the process to exit and
// be reaped on its own, bounded by the graceful phase's own ceiling and
// by the caller's deadline, whichever is nearer. kill_process_group
// still runs unconditionally after that wait, whatever it observed: it
// is the backstop for a descendant that escaped the direct child or a
// runtime that ignored every signal, and it is a no-op against a group
// that has already exited. The remaining steps are unchanged:
// close_stdout releases the connection's parked read only after the
// wait, close_connection and stop_pump follow, and
// drain_stderr_and_reap collects diagnostics and reaps. close_pipes runs
// last: the pipes are caller-owned, so close_stdout alone does not
// fully release them, and closing the standard-error end has to wait
// until drain_stderr_and_reap has drained or abandoned that collector.
//
// answer_open itself waits for the pump to answer every request it had
// open and for the connection to flush those answers, so they reach
// the agent before close_session's own call and before close_stdin
// ends its standard input. The flush half shares grace with
// close_session exactly as close_session shares it with await_exit,
// so an agent that never reads a queued answer cannot let this step
// alone consume the graceful window.
func defaultTeardownOrder(callerCtx, graceCtx context.Context, grace time.Duration) []teardownStep {
	return []teardownStep{
		{name: "answer_open", run: awaitAnswerOpen(graceCtx, grace)},
		{name: "close_session", run: closeSession(callerCtx, graceCtx, grace)},
		{name: "signal_graceful", run: signalGraceful},
		{name: "close_stdin", run: closeStdin},
		{name: "await_exit", run: awaitExit(callerCtx, graceCtx, grace)},
		{name: "kill_process_group", run: killProcessGroup},
		{name: "close_stdout", run: closeStdout},
		{name: "close_connection", run: closeConnection},
		{name: "stop_pump", run: stopPump},
		{name: "drain_stderr_and_reap", run: drainStderrAndReap(callerCtx)},
		{name: "close_pipes", run: closePipes},
	}
}

// runTeardown walks steps in order, running every one.
func runTeardown(state *sessionState, steps []teardownStep) {
	for _, step := range steps {
		step.run(state)
	}
}

// signalAnswerOpen puts the answerOpen control message and returns the
// channel the pump closes once it has answered every request it had
// open, or nil when there is no inbox to put it on. The put never
// waits, and this does not wait for the pump to write the reply; a
// caller that needs to wait for that uses [awaitAnswerOpen] instead.
func signalAnswerOpen(state *sessionState) chan struct{} {
	if state.inbox == nil {
		return nil
	}
	done := make(chan struct{})
	state.inbox.Put(pumpItem{control: &pumpControl{answerOpen: done}})
	return done
}

// awaitAnswerOpen returns the answer_open teardown step: it puts the
// answerOpen control message, then waits, under one bound, both for the
// pump to finish answering every request it had open and for the
// connection's writer to flush those answers onto the wire. This is
// what lets close_session's own call and close_stdin's end of standard
// input follow only once the answers already reached the agent.
//
// The bound shares grace with close_session the same way close_session
// and await_exit already share it: half of whatever remains on
// graceCtx, so an agent that never reads a queued answer cannot let
// this step consume the whole graceful window on its own and starve
// the signal, the close, and the wait that follow. A pump that does not
// finish answering in time, or a connection already gone, leaves the
// rest of teardown to run on schedule instead of blocking it further.
func awaitAnswerOpen(graceCtx context.Context, grace time.Duration) func(state *sessionState) {
	return func(state *sessionState) {
		done := signalAnswerOpen(state)
		if done == nil {
			return
		}

		bound := closeCallBound(grace)
		if deadline, ok := graceCtx.Deadline(); ok {
			if remaining := closeCallBound(max(time.Until(deadline), 0)); remaining < bound {
				bound = remaining
			}
		}
		boundedCtx, cancel := context.WithTimeout(graceCtx, bound)
		defer cancel()

		select {
		case <-done:
		case <-boundedCtx.Done():
			return
		}
		if state.conn != nil {
			state.conn.Flush(boundedCtx) //nolint:errcheck,gosec // best-effort; teardown proceeds either way
		}
	}
}

// killProcessGroup terminates the subprocess's process group.
func killProcessGroup(state *sessionState) {
	if state.pid > 0 {
		procutil.KillProcessGroup(state.pid) //nolint:errcheck,gosec // best-effort
	}
}

// signalGraceful sends the catchable graceful-termination signal to the
// subprocess's process group, mirroring killProcessGroup's guard. It
// never waits, never returns an error, and never changes teardown's
// path when the signal cannot be delivered: kill_process_group covers
// that case unconditionally.
func signalGraceful(state *sessionState) {
	if state.pid > 0 {
		procutil.SignalGraceful(state.pid) //nolint:errcheck,gosec // best-effort
	}
}

// awaitExit returns a teardown step that waits for the subprocess to
// exit and be reaped on its own, bounded by graceCtx. graceCtx already
// carries both the graceful phase's own ceiling and callerCtx's
// deadline, whichever is nearer, so this step declares no timer of its
// own and never spawns a goroutine.
//
// On the graceCtx arm it re-reads state.waitCh with a non-blocking
// receive before classifying the outcome: graceCtx and state.waitCh can
// both be ready when this step starts, most often because the process
// was already signalled and reaped ahead of StopSession, and treating
// that race as an escalation would warn about a state loss that never
// happened.
func awaitExit(callerCtx, graceCtx context.Context, grace time.Duration) func(state *sessionState) {
	return func(state *sessionState) {
		if state.pid <= 0 || state.waitCh == nil {
			return
		}

		started := time.Now()

		select {
		case <-state.waitCh:
			state.logger.Debug("agent exited during the graceful phase", slog.String("outcome", "exited"))
			return
		case <-graceCtx.Done():
		}

		select {
		case <-state.waitCh:
			state.logger.Debug("agent exited during the graceful phase", slog.String("outcome", "exited"))
		default:
			outcome := "grace elapsed"
			if callerCtx.Err() != nil {
				outcome = "caller deadline"
			}
			state.logger.Warn("agent did not exit inside the graceful period and was force-terminated",
				slog.String("outcome", outcome), slog.Duration("grace", grace),
				slog.Duration("elapsed", time.Since(started)))
		}
	}
}

// closeStdin closes the handle the adapter writes the agent's standard
// input through, on its own goroutine so this step itself never waits
// on that close, however long it takes or whether it ever returns.
func closeStdin(state *sessionState) {
	procutil.CloseWithoutWaiting(state.stdinCloser)
}

// closeStdout closes the handle the connection reads the agent's
// standard output through. This ends a scan a descendant holding that
// pipe's write end would otherwise keep parked. It does not release the
// standard-error end; close_pipes does that once the collector's own
// bound has run.
func closeStdout(state *sessionState) {
	if state.pipes != nil {
		state.pipes.CloseStdout() //nolint:errcheck,gosec // best-effort; unparks the connection's read
	}
}

// closePipes closes both read ends the session owns. It is the final
// teardown step: drain_stderr_and_reap is what leaves the standard-error
// collector drained or abandoned, and the two startSession failure paths
// read stderrCollector.Lines after teardown returns, so a close ahead of
// that step would return fewer lines with no marker and no record.
func closePipes(state *sessionState) {
	if state.pipes != nil {
		state.pipes.Close() //nolint:errcheck,gosec // best-effort cleanup
	}
}

// closeConnection closes the JSON-RPC connection. Closing the
// connection does not release a write its own writer goroutine has
// parked on standard input, so the handle close and the group
// termination ahead of it are what release that goroutine before the
// step that waits for the pump.
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

// drainStderrAndReap returns a teardown step that waits for the bounded
// standard-error drain, reaps the process, and on a drain that has not
// finished waits once more and abandons it, so the two stderr drain
// waits spend procutil.DefaultDrainGrace at most twice in total. Only
// the reap wait is bounded by callerCtx: the group kill has already run
// by this point in the order, so cutting that wait short leaves the
// process unreaped rather than a descendant alive, and the two stderr
// drain waits keep their own bound so a shorter caller deadline does
// not cost the collected diagnostics.
func drainStderrAndReap(callerCtx context.Context) func(state *sessionState) {
	return func(state *sessionState) {
		var drained bool
		if state.stderrCollector != nil {
			drained = state.stderrCollector.WaitDone(procutil.DefaultDrainGrace)
		} else {
			drained = true
		}

		if state.waitCh != nil {
			timer := time.NewTimer(procutil.DefaultDrainGrace)
			select {
			case <-state.waitCh:
				timer.Stop()
			case <-callerCtx.Done():
				timer.Stop()
			case <-timer.C:
			}
		}

		if !drained && state.stderrCollector != nil {
			state.stderrCollector.FinishAndCollect(procutil.DefaultDrainGrace)
		}
	}
}
