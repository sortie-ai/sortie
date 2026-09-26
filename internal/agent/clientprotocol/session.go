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
	"github.com/sortie-ai/sortie/internal/agent/clientprotocol/usagesource"
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/domain"
)

// clientProtocolMaxLineBytes raises the connection's line bound from the shared
// package's one-mebibyte default to the ten megabytes the local subprocess
// launch contract recommends.
const clientProtocolMaxLineBytes = 10 * 1024 * 1024

// pinnedProtocolVersion is the wire version this adapter was generated against.
// A handshake reporting any other value ends the session: the protocol defines
// no error for a version mismatch and leaves the disconnect to the client.
const pinnedProtocolVersion = 1

// defaultReadTimeout bounds a synchronous wait on the agent when
// agent.read_timeout_ms is not set.
const defaultReadTimeout = 30 * time.Second

// errorCodeAuthRequired is the protocol's "Authentication required"
// code, returned when the runtime refuses its credential.
const errorCodeAuthRequired = -32000

// sessionState is this adapter's session state, reached through
// domain.Session.Internal. Fields set once during StartSession before the pump
// starts are read-only afterward; fields the pump owns are documented as such.
// closeSessionID is a third class: written once on the StartSession goroutine
// after the pump starts, safe because the pump never reads it and its only
// reader is teardown.
type sessionState struct {
	target      agentcore.LaunchTarget
	agentConfig domain.AgentConfig

	// caps is built before the pump starts and owned by the pump from that point
	// on: nothing outside the pump goroutine may mutate the value it points to.
	caps *capabilityRecord

	conn *jsonrpc.Conn

	pid             int
	stdinCloser     io.Closer
	pipes           *procutil.OwnedPipes
	stderrCollector *procutil.StderrCollector
	waitCh          <-chan struct{} // closed once the subprocess has been reaped

	reaper *procutil.Reaper

	// inbox is where the connection's reader delivers routed messages and every
	// control publish lands, in one order. runPump is its only taker; it is
	// never closed, so the pump keeps taking late controls until stopCh closes.
	inbox    *jsonrpc.Inbox[pumpItem]
	stopCh   chan struct{}
	stopOnce sync.Once
	pumpDone chan struct{}

	// closeSessionID is written once, on the StartSession goroutine, after
	// resolveSession returns and before StartSession returns. It is left at zero
	// when the handshake advertises no teardown method. Teardown is its only
	// reader.
	closeSessionID string

	closeMethod string

	credentialVerification bool

	logger *slog.Logger

	// origins is the adapter-wide creation ledger, shared by every session.
	origins *sessionOrigins

	// drainGrace bounds the post-reap release's wait for the connection's reader
	// to end normally after the subprocess has been reaped. Copied from
	// ClientProtocolAdapter.drainGrace, resolving a non-positive value to
	// procutil.DefaultDrainGrace.
	drainGrace time.Duration

	// release gives the connection's reader up to drainGrace to end on its own
	// before giving up. Set once before the pump starts, read-only afterward.
	release *procutil.OutputRelease

	// usage owns the session's run-cumulative snapshot and measurement verdict,
	// built here and handed to the pump before it starts. It is not safe for
	// concurrent use; the pump goroutine is its only user afterward.
	usage *agentcore.TurnEndUsage

	// reader is the measurement source that claimed this session's launch, or
	// nil when none did. Set once before the pump starts; the pump is its only
	// user afterward, apart from teardown's release of it.
	reader usageReader

	// earlyExit is translateCallError's observation of whether the runtime
	// exited on its own before this start's connection loss, recorded on the
	// StartSession goroutine and read by startSession's two failure branches.
	earlyExit agentcore.EarlyExit
}

// pumpItem is either a message the connection's reader delivered or a control
// message. Exactly one field is set.
type pumpItem struct {
	msg     *jsonrpc.Message
	control *pumpControl
}

// pumpControl carries what StartSession learned and what RunTurn and teardown
// ask of the pump. Exactly one field is set.
type pumpControl struct {
	handshake *handshakeFacts
	sessionID string

	// expectLoad names the identifier a session/load call is about to load, so
	// the pump treats it as the session's own before the definitive sessionID
	// control message arrives, and counts any chunk replayed for it.
	expectLoad string

	// query carries the continuation verdict of a session/load or
	// session/resume call once that call's response is known. See [replayQuery].
	query *replayQuery

	startTurn *turnStart

	// usage carries what one turn's drain read, published by the goroutine that
	// ran the drain so the usage accumulator is touched only on the pump's own
	// goroutine.
	usage *usageObserved

	// answerOpen, when non-nil, tells the pump to answer every request still
	// open; the pump closes it once handleAnswerOpen returns.
	answerOpen chan struct{}
}

// usageObserved is one turn's drain result. turn identifies the turn the drain
// was armed for, so a result arriving after that turn ended is discarded rather
// than applied to its successor. recovered is nil when neither source produced
// a record, a measurement that does not exist rather than one of zero.
// completeness grades recovered and says nothing about whether it exists.
type usageObserved struct {
	turn         *activeTurn
	recovered    *agentcore.RecoveredUsage
	source       string
	completeness usagesource.Completeness
}

// replayQuery is the control message a session/load or session/resume
// continuation call publishes once its response is known, so the pump applies
// the sessionContinuation lowering. A nil reply means the call already failed:
// the pump lowers the entry at once and answers nothing. A success carries a
// buffered reply of capacity one: the pump answers true once it observes a
// chunk replayed for the expectLoad identifier, or lowers the entry and answers
// false once its bounded wait for that chunk elapses.
type replayQuery struct {
	reply chan bool
}

// handshakeFacts is what StartSession decoded from the initialize response,
// carried to the pump as a fact rather than re-derived there.
type handshakeFacts struct {
	agentInfo        implementation
	agentInfoPresent bool
	caps             agentCapabilities

	// toolServersWithheld reports whether the generated configuration declared
	// an HTTP tool server the handshake's MCP capabilities do not support, so it
	// was omitted from session/new.
	toolServersWithheld bool

	// toolServersDelivered reports whether the session-creation request carried
	// at least one tool server.
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

// turnVerdict is the pump's accept-or-reject answer to a startTurn control
// message, delivered on that message's capacity-one reply channel.
type turnVerdict struct {
	accepted bool
	err      *domain.AgentError
}

// turnEnd is the pump's final report for one turn, delivered on the turn's
// capacity-one result channel.
type turnEnd struct {
	result domain.TurnResult
	err    *domain.AgentError
}

func readTimeout(state *sessionState) time.Duration {
	if state.agentConfig.ReadTimeoutMS > 0 {
		return time.Duration(state.agentConfig.ReadTimeoutMS) * time.Millisecond
	}
	return defaultReadTimeout
}

// startSession launches the runtime, performs the initialize handshake, and
// creates or continues a session. A non-empty ResumeSessionID picks the
// continuation route the handshake advertises; every other case, and a
// continuation that is not confirmed, creates the session with session/new.
// The capability record is built before the pump starts, and the pump applies
// handshake- and continuation-based lowering to it.
func startSession(ctx context.Context, a *ClientProtocolAdapter, params domain.StartSessionParams, usage *agentcore.TurnEndUsage) (domain.Session, error) {
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
		target:                 target,
		agentConfig:            params.AgentConfig,
		stopCh:                 make(chan struct{}),
		pumpDone:               make(chan struct{}),
		logger:                 slog.Default().With(slog.String("component", "clientprotocol-adapter")),
		origins:                &a.origins,
		credentialVerification: params.CredentialVerification,
	}
	state.drainGrace = a.drainGrace
	if state.drainGrace <= 0 {
		state.drainGrace = procutil.DefaultDrainGrace
	}
	state.inbox = jsonrpc.NewInbox[pumpItem]()

	// The reader is chosen before launch because a source needs the launch to
	// carry its assignments.
	reader, readerEnv := selectUsageReader(target)
	state.reader = reader

	var cmd *exec.Cmd
	var launch sshutil.SSHLaunch
	if remote {
		launch = sshutil.BuildSSHLaunch(target.SSHHost, target.WorkspacePath, target.RemoteCommand, nil, target.SSHOptions())
		cmd = exec.CommandContext(ctx, target.Command, launch.Args...) //nolint:gosec // args are constructed programmatically with shell quoting
	} else {
		cmd = exec.CommandContext(ctx, target.Command, target.Args...) //nolint:gosec // args are constructed programmatically
	}
	cmd.Env = append(os.Environ(), readerEnv...)
	if !remote {
		if bindErr := target.BindWorkspace(cmd); bindErr != nil {
			return domain.Session{}, bindErr
		}
	}
	grace := procutil.StopGrace(state.agentConfig.StopGraceMS)
	procutil.SetGroupCancel(cmd, grace)

	// Every failure below returns without a session, so nothing else releases
	// what the claim armed.
	started := false
	defer func() {
		if !started {
			releaseUsageReader(state)
		}
	}()

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
		// Both pipe stages fail before cmd.Start, whose deferred cleanup would
		// otherwise close the parent's stdin end, so this closes it instead.
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

	// The reap runs independently of the connection's reader: the pipes are
	// caller-owned, so exec.Cmd.Wait closes neither read end and cannot cut a
	// reader still consuming buffered output short. Teardown's close_stdout and
	// close_pipes steps end that reader.
	reaper := procutil.StartReaper(cmd, state.logger)
	state.waitCh = reaper.Done()
	state.reaper = reaper

	// The release ends a handshake call or a turn that would otherwise wait
	// forever on a reaped runtime whose reader did not end inside the drain
	// bound. Stderr stays nil: drain_stderr_and_reap and close_pipes already
	// bound that stream, and moving the bound earlier would change the pinned
	// teardown ceiling.
	state.release = procutil.StartOutputRelease(procutil.OutputReleaseParams{
		Pipes:      pipes,
		Reaped:     reaper.Done(),
		ReaderDone: state.conn.Done(),
		OnAbandon:  state.conn.Close,
		Grace:      state.drainGrace,
		Logger:     state.logger,
	})

	// The capability record is built here, before the pump starts, so the pump
	// is its sole mutator afterward: StartSession must not touch state.caps past
	// this point.
	state.caps = newCapabilityRecord(remote, reader != nil)
	state.usage = usage

	// Start the pump before the handshake, so it is the sole mutator of session
	// protocol state from here on; StartSession publishes what it learns as
	// control messages rather than writing that state itself.
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
		if report := state.earlyExit.Report(state.stderrCollector); report != nil {
			return domain.Session{}, report
		}
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
	if params.CredentialVerification {
		// Non-nil so the wire carries mcpServers: [] rather than null.
		wireServers = []mcpServer{}
	}

	var caps agentCapabilities
	if initResp.AgentCapabilities != nil {
		caps = *initResp.AgentCapabilities
	}

	sessionID, agentErr := resolveSession(ctx, state, params.ResumeSessionID, caps, target.WorkspacePath, wireServers)
	if agentErr != nil {
		teardownOnFailure()
		procutil.EmitWarnLines(state.stderrCollector.Lines(), state.logger)
		if report := state.earlyExit.Report(state.stderrCollector); report != nil {
			return domain.Session{}, report
		}
		return domain.Session{}, agentErr
	}
	switch {
	case params.CredentialVerification && advertisesSessionDelete(caps):
		state.closeSessionID = sessionID
		state.closeMethod = methodSessionDelete
	case advertisesSessionClose(caps):
		state.closeSessionID = sessionID
		state.closeMethod = methodSessionClose
	}

	facts := &handshakeFacts{toolServersWithheld: withheld, toolServersDelivered: len(wireServers) > 0, caps: caps}
	if initResp.AgentInfo != nil {
		facts.agentInfo = *initResp.AgentInfo
		facts.agentInfoPresent = true
	}

	state.inbox.Put(pumpItem{control: &pumpControl{handshake: facts}})
	state.inbox.Put(pumpItem{control: &pumpControl{sessionID: sessionID}})

	started = true
	return domain.Session{
		ID:       sessionID,
		AgentPID: strconv.Itoa(state.pid),
		Internal: state,
	}, nil
}

// wrapPumpMessage wraps msg, over its own copy, as the pump item the reader
// delivers into the inbox.
func wrapPumpMessage(msg jsonrpc.Message) pumpItem {
	m := msg
	return pumpItem{msg: &m}
}

// doInitialize sends the initialize request and validates the pinned protocol
// version.
func doInitialize(ctx context.Context, state *sessionState) (*initializeResponse, *domain.AgentError) {
	callCtx, cancel := context.WithTimeout(ctx, max(readTimeout(state), agentcore.CredentialExchangeBound))
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
	if agentErr := translateCallError(state, err, callCtx, ctx); agentErr != nil {
		return nil, agentErr
	}
	if resp.Error != nil {
		if resp.Error.Code == errorCodeAuthRequired {
			return nil, agentcore.CredentialRefusedError(quoteJSONRPCError(resp.Error), nil)
		}
		return nil, &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: fmt.Sprintf("initialize error %d: %s", resp.Error.Code, quoteJSONRPCError(resp.Error)),
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

// doNewSession sends session/new with the resolved cwd and filtered server
// list.
func doNewSession(ctx context.Context, state *sessionState, cwd string, servers []mcpServer) (*newSessionResponse, *domain.AgentError) {
	callCtx, cancel := context.WithTimeout(ctx, readTimeout(state))
	defer cancel()

	req := newSessionRequest{Cwd: cwd, MCPServers: servers}
	resp, err := state.conn.Call(callCtx, methodSessionNew, req)
	if agentErr := translateCallError(state, err, callCtx, ctx); agentErr != nil {
		return nil, agentErr
	}
	if resp.Error != nil {
		if resp.Error.Code == errorCodeAuthRequired {
			return nil, agentcore.CredentialRefusedError(quoteJSONRPCError(resp.Error), nil)
		}
		return nil, &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: fmt.Sprintf("session/new error %d: %s", resp.Error.Code, quoteJSONRPCError(resp.Error)),
		}
	}

	var newResp newSessionResponse
	if err := json.Unmarshal(resp.Result, &newResp); err != nil {
		return nil, &domain.AgentError{Kind: domain.ErrResponseError, Message: "session/new response did not decode", Err: err}
	}
	return &newResp, nil
}

// translateCallError maps a jsonrpc.Conn.Call failure to the normalized failure
// table: a timeout against callCtx's own deadline is response_timeout, and every
// other failure is port_exit, the loss of the subprocess.
//
// startCtx is the context startSession itself received, not callCtx's own
// deadline; a non-timeout failure records the shared early-exit observation
// under it, for startSession's failure branches to report.
func translateCallError(state *sessionState, err error, callCtx context.Context, startCtx context.Context) *domain.AgentError {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) && callCtx.Err() == context.DeadlineExceeded {
		return &domain.AgentError{Kind: domain.ErrResponseTimeout, Message: "timed out waiting for a response", Err: err}
	}
	state.earlyExit = agentcore.ObserveEarlyExit(startCtx, state.target, state.reaper, state.drainGrace)
	if state.sshConnectionFailed() {
		return agentcore.ConnectionFailedError()
	}
	return &domain.AgentError{Kind: domain.ErrPortExit, Message: "agent connection ended before responding", Err: err}
}

func (state *sessionState) sshConnectionFailed() bool {
	return agentcore.ReaperConnectionFailed(state.target.RemoteCommand != "", state.reaper, state.drainGrace)
}

// textContentBlock builds a text content block from scratch: contentBlock's
// generated form only round-trips an already-decoded value, so a client that
// writes one, rather than echoing one it read, builds the bytes itself.
func textContentBlock(text string) contentBlock {
	data, err := marshalWithDiscriminant(textContent{Text: text}, "type", contentBlockText)
	if err != nil {
		panic(fmt.Sprintf("clientprotocol: marshal text content block: %v", err))
	}
	return contentBlock{Type: contentBlockText, Remainder: data}
}

// runTurn publishes a startTurn control message and relays the pump's events
// and final result to the caller.
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

// closeCallBound returns half of the window the close_session step is given,
// truncated toward zero.
func closeCallBound(grace time.Duration) time.Duration {
	return grace / 2
}

type closeCallOutcome struct {
	resp jsonrpc.Response
	err  error
}

// closeSession returns a teardown step that issues one session/close call for
// state.closeSessionID, bounded by half of whatever remains on graceCtx. It
// does nothing when no identifier was recorded or the connection is already
// gone. It runs on teardown's own goroutine: nothing about a runtime that has
// stopped reading its stdin can hold it past its bound.
func closeSession(callerCtx, graceCtx context.Context, grace time.Duration) func(state *sessionState) {
	return func(state *sessionState) {
		if state.closeSessionID == "" || state.conn == nil {
			return
		}
		conn, id, method := state.conn, state.closeSessionID, state.closeMethod
		if method == "" {
			method = methodSessionClose
		}

		// Half of what remains on graceCtx, not half of the configured grace: a
		// nearer caller deadline would otherwise let this call spend the whole
		// window and starve the signal behind it.
		bound := closeCallBound(grace)
		if deadline, ok := graceCtx.Deadline(); ok {
			if half := closeCallBound(max(time.Until(deadline), 0)); half < bound {
				bound = half
			}
		}
		callCtx, cancel := context.WithTimeout(graceCtx, bound)
		defer cancel()

		var resp jsonrpc.Response
		var err error
		if method == methodSessionDelete {
			resp, err = conn.Call(callCtx, methodSessionDelete, deleteSessionRequest{SessionID: sessionId(id)})
		} else {
			resp, err = conn.Call(callCtx, methodSessionClose, closeSessionRequest{SessionID: sessionId(id)})
		}
		logCloseSessionOutcome(state, callerCtx, bound, method, closeCallOutcome{resp: resp, err: err})
	}
}

// logCloseSessionOutcome classifies a completed session/close call. A JSON-RPC
// error logs at Warn with the numeric code only, never the peer's message text.
// A call that did not complete because callCtx ended logs at Warn; any other
// failure logs at Debug, because the process is already going away.
func logCloseSessionOutcome(state *sessionState, callerCtx context.Context, bound time.Duration, method string, got closeCallOutcome) {
	if method == methodSessionDelete {
		logDeleteSessionOutcome(state, callerCtx, bound, got)
		return
	}
	if got.err != nil {
		if isCallContextEnded(got.err) {
			state.logger.Warn("session/close did not complete before the wait ended",
				slog.Duration("bound", bound), slog.String("outcome", waitEndOutcome(callerCtx)))
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

// logDeleteSessionOutcome logs every failure at Warn, unlike session/close,
// because a failed delete leaves the verification session stored.
func logDeleteSessionOutcome(state *sessionState, callerCtx context.Context, bound time.Duration, got closeCallOutcome) {
	const failed = "failed to delete credential verification session"
	switch {
	case got.err != nil && isCallContextEnded(got.err):
		state.logger.Warn(failed, slog.Duration("bound", bound), slog.String("outcome", waitEndOutcome(callerCtx)))
	case got.err != nil:
		state.logger.Warn(failed, slog.Any("error", got.err))
	case got.resp.Error != nil:
		state.logger.Warn(failed, slog.Int("code", got.resp.Error.Code))
	default:
		state.logger.Debug("session deleted through the protocol")
	}
}

func isCallContextEnded(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

func waitEndOutcome(callerCtx context.Context) string {
	if callerCtx.Err() != nil {
		return "caller deadline"
	}
	return "bound elapsed"
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
	releaseUsageReader(state)
	return nil
}

type teardownStep struct {
	name string
	run  func(state *sessionState)
}

// defaultTeardownOrder expresses StopSession's step order. Every step guards its
// own preconditions and does nothing when they are not met, so StopSession never
// panics on a partially-constructed session.
//
// The order matters. close_session runs while the runtime is still running
// normally, and never delays the graceful signal past its own bound. The
// graceful phase (signal_graceful, close_stdin, await_exit) gives the process a
// bounded chance to exit on its own before kill_process_group runs
// unconditionally as the backstop for a descendant that escaped or a runtime
// that ignored every signal. close_stdout releases the connection's parked read
// only after that wait. close_pipes runs last because the pipes are
// caller-owned and closing the standard-error end must wait until
// drain_stderr_and_reap has drained or abandoned that collector. answer_open
// runs first so open requests reach the agent before close_session's call and
// close_stdin; its flush shares grace so an agent that never reads a queued
// answer cannot consume the whole graceful window on its own.
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

// releaseUsageReader releases whatever the measurement source armed. It runs
// after teardown, once the pump has stopped and no drain is still reading what
// it removes.
func releaseUsageReader(state *sessionState) {
	if state.reader == nil {
		return
	}
	state.reader.Close()
}

func runTeardown(state *sessionState, steps []teardownStep) {
	for _, step := range steps {
		step.run(state)
	}
}

// signalAnswerOpen puts the answerOpen control message and returns the channel
// the pump closes once it has answered every open request, or nil when there is
// no inbox. It does not wait for that reply; [awaitAnswerOpen] does.
func signalAnswerOpen(state *sessionState) chan struct{} {
	if state.inbox == nil {
		return nil
	}
	done := make(chan struct{})
	state.inbox.Put(pumpItem{control: &pumpControl{answerOpen: done}})
	return done
}

// awaitAnswerOpen returns the answer_open teardown step: it puts the answerOpen
// control message, then waits under one bound both for the pump to answer every
// open request and for the connection to flush those answers. The bound is half
// of whatever remains on graceCtx, so an agent that never reads a queued answer
// cannot consume the whole graceful window and starve the steps that follow.
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

func killProcessGroup(state *sessionState) {
	if state.pid > 0 {
		procutil.KillProcessGroup(state.pid) //nolint:errcheck,gosec // best-effort
	}
}

// signalGraceful sends the catchable graceful-termination signal to the
// subprocess's process group. It never waits and never changes teardown's path
// when the signal cannot be delivered: kill_process_group covers that case.
func signalGraceful(state *sessionState) {
	if state.pid > 0 {
		procutil.SignalGraceful(state.pid) //nolint:errcheck,gosec // best-effort
	}
}

// awaitExit returns a teardown step that waits for the subprocess to exit and be
// reaped, bounded by graceCtx, which already carries both the graceful phase's
// ceiling and callerCtx's deadline. On the graceCtx arm it re-reads
// state.waitCh with a non-blocking receive before classifying the outcome:
// graceCtx and state.waitCh can both be ready when the process was signalled
// and reaped ahead of StopSession, and treating that race as an escalation would
// warn about a state loss that never happened.
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

// closeStdin closes the agent's standard-input handle on its own goroutine, so
// this step never waits on that close however long it takes.
func closeStdin(state *sessionState) {
	procutil.CloseWithoutWaiting(state.stdinCloser)
}

// closeStdout closes the agent's standard-output handle, ending a scan a
// descendant holding that pipe's write end would otherwise keep parked. It does
// not release the standard-error end; close_pipes does that.
func closeStdout(state *sessionState) {
	if state.pipes != nil {
		state.pipes.CloseStdout() //nolint:errcheck,gosec // best-effort; unparks the connection's read
	}
}

// closePipes closes both read ends the session owns. It is the final teardown
// step: drain_stderr_and_reap leaves the standard-error collector drained or
// abandoned, and the two startSession failure paths read stderrCollector.Lines
// after teardown returns, so a close ahead of that step would return fewer
// lines with no record.
func closePipes(state *sessionState) {
	if state.pipes != nil {
		state.pipes.Close() //nolint:errcheck,gosec // best-effort cleanup
	}
}

// closeConnection closes the JSON-RPC connection. This does not release a write
// its own writer goroutine has parked on standard input; the handle close and
// the group termination ahead of it release that goroutine.
func closeConnection(state *sessionState) {
	if state.conn != nil {
		state.conn.Close()
	}
}

// stopPump signals the pump to exit and waits for it, which the standard-output
// close above makes certain.
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
// standard-error drain, reaps the process, and on an unfinished drain waits
// once more and abandons it. Only the reap wait is bounded by callerCtx: the
// group kill has already run, so cutting it short leaves the process unreaped
// rather than a descendant alive, and the stderr drain waits keep their own
// bound so a shorter caller deadline does not cost the collected diagnostics.
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
