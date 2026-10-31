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
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/clientprotocol/usagesource"
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

// clientProtocolMaxLineBytes raises the connection's line bound from the shared
// one-mebibyte default to the ten megabytes the local launch contract recommends.
const clientProtocolMaxLineBytes = 10 * 1024 * 1024

// pinnedProtocolVersion is the wire version this adapter was generated against.
// A handshake reporting any other value ends the session: the protocol defines
// no error for a version mismatch and leaves the disconnect to the client.
const pinnedProtocolVersion = 1

// errorCodeAuthRequired is the protocol's "Authentication required" code.
const errorCodeAuthRequired = -32000

// sessionState is this adapter's session state, reached through
// domain.Session.Internal. Fields set before the pump starts are read-only
// afterward; fields the pump owns say so. closeSessionID is the exception:
// written after the pump starts, safe because only teardown reads it.
type sessionState struct {
	target      agentcore.LaunchTarget
	agentConfig domain.AgentConfig

	// launch is the zero value on a local launch.
	launch sshutil.SSHLaunch

	// caps is built before the pump starts and owned by the pump from that point
	// on: nothing outside the pump goroutine may mutate the value it points to.
	caps *capabilityRecord

	conn *jsonrpc.Conn

	pid             int
	group           *procutil.Group
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

	// closeSessionID is written once on the StartSession goroutine, after
	// resolveSession returns, and stays zero when the handshake advertises no
	// teardown method.
	closeSessionID string

	closeMethod string

	credentialVerification bool

	// initializeMeta is the clientCapabilities._meta object the initialize
	// request carries; nil adds nothing.
	initializeMeta json.RawMessage

	// configuredModel is the model setting of the settings block, empty when
	// absent or not a string. It scopes a refusal's reset to the held model.
	configuredModel string

	launchEnv []string

	// launchAddition is what the launch carried for the selected declaration's
	// launch addition variable; its name is empty when no addition applied. It
	// is kept apart from launchEnv because a remote launch carries the value in
	// its SSH options.
	launchAddition carriedValue

	// sessionID is written once by finishSession, before the pump learns the
	// handshake.
	sessionID string

	logger *slog.Logger

	// origins is the adapter-wide creation ledger, shared by every session.
	origins *sessionOrigins

	// drainGrace bounds how long the release waits for the reader to end after
	// the reap; a non-positive adapter value resolves to procutil.DefaultDrainGrace.
	drainGrace time.Duration

	// release gives the reader up to drainGrace to end on its own.
	release *procutil.OutputRelease

	// usage is not safe for concurrent use; the pump goroutine is its only user
	// once it starts.
	usage *agentcore.TurnEndUsage

	// verification is nil when no addition claimed the launch; teardown closes it.
	verification usagesource.VerificationAddition

	// tools is closed by stopSession or by a failed start.
	tools *agentcore.ToolSession

	// earlyExit is recorded by translateCallError on the StartSession goroutine
	// and read by startSession's failure branches.
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

	query *replayQuery

	startTurn *turnStart

	// pulled and records are published by the goroutine that ran the pull or the
	// read, so the ledger is touched only on the pump's goroutine.
	pulled  *pullResult
	records *recordsResult

	// answerOpen, when non-nil, tells the pump to answer every request still
	// open; the pump closes it once handleAnswerOpen returns.
	answerOpen chan struct{}
}

// usageObserved is one turn's settled spend. recovered is nil when the ledger
// settled no figure, a measurement that does not exist rather than one of zero.
type usageObserved struct {
	turn      *activeTurn
	recovered *agentcore.RecoveredUsage
}

// recordsResult is one turn's records read; turn lets a result arriving after
// that turn ended be dropped.
type recordsResult struct {
	turn    *activeTurn
	read    usage.RecordsRead
	refusal usagesource.RecordedRefusal
}

// pullResult is one turn's usage pull outcome; turn lets a result arriving after
// that turn ended be dropped.
type pullResult struct {
	turn    *activeTurn
	windows []domain.UsageWindow
	err     error
}

// replayQuery is what a session/load or session/resume call publishes once its
// response is known, so the pump applies the sessionContinuation lowering. A nil
// reply means the call failed: the pump lowers the entry and answers nothing.
// Otherwise reply has capacity one: true once a chunk replayed for the expectLoad
// identifier is observed, false once the bounded wait for it elapses.
type replayQuery struct {
	reply chan bool
}

// handshakeFacts is what StartSession decoded from the initialize response,
// carried to the pump as a fact rather than re-derived there.
type handshakeFacts struct {
	agentInfo        implementation
	agentInfoPresent bool
	caps             agentCapabilities

	toolServersDelivered bool

	// declaration is the effective declaration the session is measured under.
	declaration usagesource.Declaration

	// puller is nil when the declaration has no pull or the session verifies a
	// credential.
	puller *usage.Puller

	// ledger is nil when the declaration declares no spend or is unsupported.
	ledger *usage.Ledger
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

// startSession launches the runtime, performs the initialize handshake, and
// creates or continues a session. A non-empty ResumeSessionID picks the
// continuation route the handshake advertises; every other case, and an
// unconfirmed continuation, creates the session with session/new.
func startSession(ctx context.Context, a *ClientProtocolAdapter, params domain.StartSessionParams, usage *agentcore.TurnEndUsage) (domain.Session, error) {
	if _, fault := typeutil.StringField(params.Settings, mcpConfigKey); fault != nil {
		return domain.Session{}, agentcore.SettingsError(fault.Error(), fault)
	}

	target, agentErr := agentcore.ResolveLaunchTarget(params, "")
	if agentErr != nil {
		return domain.Session{}, agentErr
	}

	remote := target.RemoteCommand != ""

	tools, agentErr := agentcore.ResolveToolDelivery(ctx, params, target, registry.MCPDialectACPSession, slog.Default())
	if agentErr != nil {
		return domain.Session{}, agentErr
	}

	declarations := a.declarations
	if declarations == nil {
		declarations = usagesource.Declarations
	}

	var verification usagesource.VerificationAddition
	var verificationSwitches usagesource.VerificationSwitches
	if params.ReducedVerificationContext() {
		verification, verificationSwitches, _ = usagesource.ClaimVerification(target, os.Environ())
	}

	launchChoice := chooseDeclaration(declarations, nil, target).declaration
	addition := launchChoice.LaunchAddition
	configuredModel, _ := registry.ModelSetting(params.Settings)

	// Every failure below returns without a session, so this is the only release
	// of what the start acquired.
	var state *sessionState
	started := false
	defer func() {
		if started {
			return
		}
		tools.Close() //nolint:errcheck,gosec // best-effort; the start failure is what the caller needs
		switch {
		case state != nil:
			closeVerification(state)
		case verification != nil:
			verification.Close()
		}
	}()

	state = &sessionState{
		target:                 target,
		agentConfig:            params.AgentConfig,
		stopCh:                 make(chan struct{}),
		pumpDone:               make(chan struct{}),
		logger:                 slog.Default().With(slog.String("component", "clientprotocol-adapter")),
		origins:                &a.origins,
		credentialVerification: params.CredentialVerification,
		verification:           verification,
		tools:                  tools,
		initializeMeta:         launchChoice.InitializeMeta,
		configuredModel:        configuredModel,
	}
	state.drainGrace = a.drainGrace
	if state.drainGrace <= 0 {
		state.drainGrace = procutil.DefaultDrainGrace
	}
	state.inbox = jsonrpc.NewInbox[pumpItem]()

	var cmd *exec.Cmd
	var launch sshutil.SSHLaunch
	if remote {
		var settings []sshutil.EnvVar
		if addition.Merge != nil {
			current, set := target.RuntimeEnv(os.Environ(), addition.Name)
			value, _ := addition.Merge(current, set)
			settings = append(settings, sshutil.EnvVar{Name: addition.Name, Value: value})
		}
		opts := target.SSHOptions(settings...)
		if addition.Merge != nil {
			state.launchAddition = carriedIn(opts.Env, addition.Name)
		}
		opts.Forward = tools.Forward(nil)
		launch = sshutil.BuildSSHLaunch(target.SSHHost, target.WorkspacePath, target.RemoteCommand, nil, opts)
		cmd = exec.CommandContext(ctx, target.Command, launch.Args...) //nolint:gosec // args are constructed programmatically with shell quoting
	} else {
		cmd = exec.CommandContext(ctx, target.Command, slices.Concat(target.Args, verificationSwitches.Args)...) //nolint:gosec // args are constructed programmatically
	}
	cmd.Env = slices.Concat(os.Environ(), verificationSwitches.Env)
	if !remote && addition.Merge != nil {
		cmd.Env = mergeAdditionInto(target, cmd.Env, addition)
		value, set := target.RuntimeEnv(cmd.Env, addition.Name)
		state.launchAddition = carriedValue{name: addition.Name, value: value, set: set}
	}
	state.launchEnv = cmd.Env
	if !remote {
		if bindErr := target.BindWorkspace(cmd); bindErr != nil {
			return domain.Session{}, bindErr
		}
	}
	grace := procutil.StopGrace(state.agentConfig.StopGraceMS)
	procutil.SetGroupCancel(cmd, grace)

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return domain.Session{}, &domain.AgentError{Kind: domain.ErrPortExit, Message: "failed to create stdin pipe", Err: err}
	}
	prefixedStdin := launch.PrefixStdin(stdinPipe)

	pipes, group, err := procutil.StartWithOwnedPipes(ctx, cmd, state.logger)
	if err != nil {
		if cancelled := agentcore.SessionStartCancelledError(ctx, err); cancelled != nil {
			return domain.Session{}, cancelled
		}
		var startErr *procutil.StartError
		if !errors.As(err, &startErr) {
			return domain.Session{}, &domain.AgentError{Kind: domain.ErrPortExit, Message: "failed to start subprocess", Err: err}
		}
		// Both pipe stages fail before cmd.Start, whose cleanup would have closed
		// the parent's stdin end.
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
	state.group = group
	state.stdinCloser = prefixedStdin
	state.pipes = pipes
	state.launch = launch
	state.stderrCollector = procutil.NewStderrCollector(launch.WatchStderr(pipes.Stderr), state.logger)

	state.conn = jsonrpc.NewConn(prefixedStdin, pipes.Stdout, jsonrpc.Deliver(state.inbox, wrapPumpMessage),
		jsonrpc.WithVersionMember(), jsonrpc.WithMaxLineBytes(clientProtocolMaxLineBytes))

	// The pipes are caller-owned, so exec.Cmd.Wait closes neither read end and
	// cannot cut short a reader still consuming buffered output; teardown's
	// close_stdout and close_pipes end that reader.
	reaper := procutil.StartReaper(cmd, state.logger)
	state.waitCh = reaper.Done()
	state.reaper = reaper

	// The release ends a call or turn that would wait forever on a reaped
	// runtime whose reader did not end inside the drain bound. Stderr stays nil:
	// drain_stderr_and_reap and close_pipes already bound it, and an earlier
	// bound would change the pinned teardown ceiling.
	state.release = procutil.StartOutputRelease(procutil.OutputReleaseParams{
		Pipes:      pipes,
		Reaped:     reaper.Done(),
		ReaderDone: state.conn.Done(),
		OnAbandon:  state.conn.Close,
		Grace:      state.drainGrace,
		Logger:     state.logger,
	})

	// The pump is state.caps' sole mutator once started; nothing here may touch
	// it afterward.
	state.caps = newCapabilityRecord()
	state.usage = usage

	// The pump starts before the handshake so it is the sole mutator of protocol
	// state; what the handshake learns reaches it as control messages.
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

	session, err := finishSession(ctx, state, params, target, initResp, declarations)
	started = err == nil
	return session, err
}

// finishSession opens the session the handshake began and hands the pump what
// the handshake and the open result established.
func finishSession(ctx context.Context, state *sessionState, params domain.StartSessionParams, target agentcore.LaunchTarget, initResp *initializeResponse, declarations *registry.Registry[usagesource.Declaration, struct{}]) (domain.Session, error) {
	grace := procutil.StopGrace(state.agentConfig.StopGraceMS)
	teardownOnFailure := func() {
		graceCtx, cancel := context.WithTimeout(ctx, grace)
		defer cancel()
		runTeardown(state, defaultTeardownOrder(ctx, graceCtx, grace))
	}

	allowHTTP := initResp.AgentCapabilities != nil &&
		initResp.AgentCapabilities.MCPCapabilities != nil &&
		initResp.AgentCapabilities.MCPCapabilities.HTTP != nil &&
		*initResp.AgentCapabilities.MCPCapabilities.HTTP
	wired, agentErr := settleToolDelivery(ctx, state, allowHTTP)
	if agentErr != nil {
		teardownOnFailure()
		return domain.Session{}, agentErr
	}
	if params.CredentialVerification {
		// Non-nil so the wire carries mcpServers: [] rather than null.
		wired = []mcpServer{}
	}

	var caps agentCapabilities
	if initResp.AgentCapabilities != nil {
		caps = *initResp.AgentCapabilities
	}

	sessionID, created, session, agentErr := resolveSession(ctx, state, params.ResumeSessionID, caps, target.WorkspacePath, wired)
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

	choice := chooseDeclaration(declarations, initResp.AgentInfo, target)
	if choice.mismatch {
		state.logger.Warn("agent runtime differs from its configured command",
			slog.String("configured_runtime", choice.configured),
			slog.String("handshake_runtime", initResp.AgentInfo.Name),
			slog.String("usage_declaration", choice.key),
		)
	}
	facts := &handshakeFacts{
		toolServersDelivered: len(wired) > 0,
		caps:                 caps,
		declaration:          choice.declaration,
	}
	state.sessionID = sessionID
	facts.puller = newSessionPuller(facts.declaration, state, params.CredentialVerification)
	if initResp.AgentInfo != nil {
		facts.agentInfo = *initResp.AgentInfo
		facts.agentInfoPresent = true
	}
	facts.ledger = newSessionLedger(ctx, state, choice.declaration, created, session)

	state.inbox.Put(pumpItem{control: &pumpControl{handshake: facts}})
	state.inbox.Put(pumpItem{control: &pumpControl{sessionID: sessionID}})

	if agentErr := state.tools.AwaitConnection(ctx); agentErr != nil {
		teardownOnFailure()
		return domain.Session{}, agentErr
	}

	return domain.Session{
		ID:            sessionID,
		AgentPID:      strconv.Itoa(state.pid),
		NoToolChannel: state.tools.NoToolChannel(),
		Internal:      state,
	}, nil
}

// newSessionLedger returns the session's spend ledger, or nil when the
// declaration declares neither a token shape nor an amount or the ledger
// refuses it. A session the runtime
// created has no history, so its records boundary is known without a read. The
// records already on the host of a continued session are read here, before the
// first prompt, so none of them is counted as this run's spend. A launch whose
// addition is not in effect makes every turn of the session a lower bound, as
// does a session the declaration's rule names from session, the raw result of
// the call that opened it.
func newSessionLedger(ctx context.Context, state *sessionState, decl usagesource.Declaration, created bool, session json.RawMessage) *usage.Ledger {
	if decl.Live.Spend.Shape == usage.SpendUndeclared && len(decl.Live.Spend.Amounts) == 0 {
		return nil
	}
	if !additionInEffect(state, decl.LaunchAddition) {
		decl.Live.Spend.LowerBound = true
	}
	if decl.UnrecordedRequests != nil && decl.UnrecordedRequests(session) {
		decl.Live.Spend.LowerBound = true
	}
	ledger, err := usage.NewLedger(decl.Live, decl.Records)
	if err != nil {
		state.logger.Warn("spend declaration unsupported", slog.Any("error", err))
		return nil
	}
	switch {
	case created:
		ledger.SessionCreated()
	case decl.ReadRecords != nil:
		ledger.Boundary(readSessionRecords(ctx, state, decl))
	}
	return ledger
}

// carriedValue is the value a launch carries for one environment variable.
type carriedValue struct {
	name  string
	value string
	set   bool
}

// carriedIn returns what the SSH options carry for name.
func carriedIn(carried []sshutil.EnvVar, name string) carriedValue {
	for _, entry := range carried {
		if entry.Name == name {
			return carriedValue{name: name, value: entry.Value, set: true}
		}
	}
	return carriedValue{name: name}
}

// mergeAdditionInto returns env with one entry for the addition's variable,
// holding the addition merged into the last value env gave it. Merging into
// the last value keeps a verification launch's own document for the variable.
func mergeAdditionInto(target agentcore.LaunchTarget, env []string, addition usagesource.LaunchAddition) []string {
	current, set := target.RuntimeEnv(env, addition.Name)
	value, inEffect := addition.Merge(current, set)
	if !set && !inEffect {
		return env
	}
	kept := slices.DeleteFunc(slices.Clone(env), func(entry string) bool {
		return isEnvEntryNamed(entry, addition.Name)
	})
	return append(kept, addition.Name+"="+value)
}

// isEnvEntryNamed reports whether entry assigns name, which Windows compares
// without regard to case.
func isEnvEntryNamed(entry, name string) bool {
	key, _, found := strings.Cut(entry, "=")
	if runtime.GOOS == "windows" {
		return found && strings.EqualFold(key, name)
	}
	return found && key == name
}

// carried returns the value the launch carries for name.
func (s *sessionState) carried(name string) (string, bool) {
	if s.launchAddition.name == name {
		return s.launchAddition.value, s.launchAddition.set
	}
	return s.target.RuntimeEnv(s.launchEnv, name)
}

// additionInEffect reports whether the launch carries addition's request-off
// setting: merging it into the value the launch carries changes nothing. A
// declaration without an addition has nothing to be out of effect.
func additionInEffect(state *sessionState, addition usagesource.LaunchAddition) bool {
	if addition.Merge == nil {
		return true
	}
	current, set := state.carried(addition.Name)
	merged, inEffect := addition.Merge(current, set)
	return inEffect && merged == current
}

// settleToolDelivery returns the tool servers for the session-creation
// request: Sortie's server reaches a runtime advertising HTTP support through
// the local relay, or on a remote launch through the forward whose outcome it
// waits for. A runtime without HTTP support gets no HTTP server.
func settleToolDelivery(ctx context.Context, state *sessionState, allowHTTP bool) ([]mcpServer, *domain.AgentError) {
	var outcome sshutil.ForwardOutcome
	if state.target.RemoteCommand != "" {
		var err error
		outcome, err = state.launch.WaitOutcome(ctx)
		if err != nil {
			return nil, agentcore.SessionStartCancelledError(ctx, &procutil.StartError{
				Stage:     procutil.StageProcessStart,
				Err:       err,
				Cancelled: true,
			})
		}
	} else if allowHTTP {
		state.tools.ServeLocal(ctx)
	}

	if !allowHTTP && state.tools.WithholdForRuntime() {
		state.logger.Warn("configured tool servers were not delivered: the agent does not advertise HTTP tool-server support")
	}

	servers, err := decodeServers(state.tools.Delivery(outcome).Entries)
	if err != nil {
		return nil, &domain.AgentError{Kind: domain.ErrResponseError, Message: err.Error(), Err: err}
	}
	return servers, nil
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
	callCtx, cancel := context.WithTimeout(ctx, agentcore.FirstResponseTimeout(state.agentConfig))
	defer cancel()

	no := false
	req := initializeRequest{
		ProtocolVersion: protocolVersion(pinnedProtocolVersion),
		ClientCapabilities: &clientCapabilities{
			Meta:     state.initializeMeta,
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
// list. It returns the decoded response and the agent's raw result, which
// carries members the decoded struct does not.
func doNewSession(ctx context.Context, state *sessionState, cwd string, servers []mcpServer) (*newSessionResponse, json.RawMessage, *domain.AgentError) {
	callCtx, cancel := context.WithTimeout(ctx, agentcore.ReadTimeout(state.agentConfig))
	defer cancel()

	req := newSessionRequest{Cwd: cwd, MCPServers: servers}
	resp, err := state.conn.Call(callCtx, methodSessionNew, req)
	if agentErr := translateCallError(state, err, callCtx, ctx); agentErr != nil {
		return nil, nil, agentErr
	}
	if resp.Error != nil {
		if resp.Error.Code == errorCodeAuthRequired {
			return nil, nil, agentcore.CredentialRefusedError(quoteJSONRPCError(resp.Error), nil)
		}
		return nil, nil, &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: fmt.Sprintf("session/new error %d: %s", resp.Error.Code, quoteJSONRPCError(resp.Error)),
		}
	}

	var newResp newSessionResponse
	if err := json.Unmarshal(resp.Result, &newResp); err != nil {
		return nil, nil, &domain.AgentError{Kind: domain.ErrResponseError, Message: "session/new response did not decode", Err: err}
	}
	return &newResp, resp.Result, nil
}

// translateCallError maps a jsonrpc.Conn.Call failure to a normalized error: a
// timeout against callCtx's own deadline is response_timeout, anything else is
// port_exit. A non-timeout failure records the early-exit observation under
// startCtx, the context startSession received, for its failure branches.
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

	replyTimer := time.NewTimer(agentcore.ReadTimeout(state.agentConfig))
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

// callBound returns half of grace, or half of what remains on graceCtx when that
// is less: a nearer caller deadline would otherwise let one call spend the whole
// window and starve the steps behind it.
func callBound(graceCtx context.Context, grace time.Duration) time.Duration {
	bound := closeCallBound(grace)
	if deadline, ok := graceCtx.Deadline(); ok {
		bound = min(bound, closeCallBound(max(time.Until(deadline), 0)))
	}
	return bound
}

type closeCallOutcome struct {
	resp jsonrpc.Response
	err  error
}

// closeSession returns a teardown step that issues one session/close call for
// state.closeSessionID, bounded by [callBound]. It does nothing when no
// identifier was recorded or the connection is gone. A runtime that stopped
// reading its stdin cannot hold it past that bound.
func closeSession(callerCtx, graceCtx context.Context, grace time.Duration) func(state *sessionState) {
	return func(state *sessionState) {
		if state.closeSessionID == "" || state.conn == nil {
			return
		}
		conn, id, method := state.conn, state.closeSessionID, state.closeMethod
		if method == "" {
			method = methodSessionClose
		}

		bound := callBound(graceCtx, grace)
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

// logCloseSessionOutcome logs a session/close outcome. A JSON-RPC error logs at
// Warn with the numeric code only, never the peer's message text, as does a call
// cut off by callCtx; any other failure logs at Debug, the process being on its
// way out.
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
	closeVerification(state)
	if state.tools != nil {
		return state.tools.Close()
	}
	return nil
}

type teardownStep struct {
	name string
	run  func(state *sessionState)
}

// defaultTeardownOrder returns StopSession's steps. Every step guards its own
// preconditions, so a partially-constructed session never panics.
//
// The order matters. answer_open runs first so open requests reach the agent
// before close_session and close_stdin; its flush shares grace so an agent that
// never reads a queued answer cannot consume the whole graceful window.
// close_session runs while the runtime is still healthy and never delays the
// graceful signal past its own bound. The graceful phase (signal_graceful,
// close_stdin, await_exit) lets the process exit on its own before
// kill_process_group, the unconditional backstop for an escaped descendant or a
// runtime that ignored every signal. close_stdout releases the parked read only
// after that wait. close_pipes runs last: the pipes are caller-owned, and the
// standard-error end must stay open until drain_stderr_and_reap has drained or
// abandoned its collector.
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

func runTeardown(state *sessionState, steps []teardownStep) {
	for _, step := range steps {
		step.run(state)
	}
}

// signalAnswerOpen puts the answerOpen control message and returns the channel
// the pump closes once every open request is answered, or nil without an inbox.
// [awaitAnswerOpen] does the waiting.
func signalAnswerOpen(state *sessionState) chan struct{} {
	if state.inbox == nil {
		return nil
	}
	done := make(chan struct{})
	state.inbox.Put(pumpItem{control: &pumpControl{answerOpen: done}})
	return done
}

// awaitAnswerOpen returns the answer_open teardown step: it waits under one
// [callBound] for the pump to answer every open request and for the connection
// to flush those answers.
func awaitAnswerOpen(graceCtx context.Context, grace time.Duration) func(state *sessionState) {
	return func(state *sessionState) {
		done := signalAnswerOpen(state)
		if done == nil {
			return
		}

		boundedCtx, cancel := context.WithTimeout(graceCtx, callBound(graceCtx, grace))
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
	state.group.Kill() //nolint:errcheck,gosec // best-effort
}

// signalGraceful sends the catchable termination signal to the process group. A
// failed delivery changes nothing: kill_process_group covers it.
func signalGraceful(state *sessionState) {
	state.group.SignalGraceful() //nolint:errcheck,gosec // best-effort
}

// awaitExit returns a teardown step that waits, bounded by graceCtx, for the
// subprocess to exit and be reaped. On the graceCtx arm it re-checks
// state.waitCh before classifying: both can be ready when the process was reaped
// ahead of StopSession, and calling that an escalation would warn about a loss
// that never happened.
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

// closeStdin closes the agent's standard-input handle without waiting on it.
func closeStdin(state *sessionState) {
	procutil.CloseWithoutWaiting(state.stdinCloser)
}

// closeStdout closes the agent's standard-output handle, ending a scan a
// descendant holding the write end would keep parked. close_pipes releases the
// standard-error end.
func closeStdout(state *sessionState) {
	if state.pipes != nil {
		state.pipes.CloseStdout() //nolint:errcheck,gosec // best-effort; unparks the connection's read
	}
}

// closePipes closes both read ends the session owns. It runs last because the
// startSession failure paths read stderrCollector.Lines after teardown, and an
// earlier close would silently return fewer lines.
func closePipes(state *sessionState) {
	if state.pipes != nil {
		state.pipes.Close() //nolint:errcheck,gosec // best-effort cleanup
	}
}

// closeConnection closes the JSON-RPC connection. It cannot release a writer
// parked on standard input; the handle close and group kill ahead of it do.
func closeConnection(state *sessionState) {
	if state.conn != nil {
		state.conn.Close()
	}
}

// stopPump signals the pump to exit and waits for it; the standard-output close
// ahead of it guarantees the wait ends.
func stopPump(state *sessionState) {
	if state.stopCh == nil {
		return
	}
	state.stopOnce.Do(func() { close(state.stopCh) })
	if state.pumpDone != nil {
		<-state.pumpDone
	}
}

// drainStderrAndReap returns a teardown step that drains standard error, reaps
// the process, and abandons a drain that did not finish. Only the reap wait
// honors callerCtx: the group kill already ran, so cutting it short leaves a
// process unreaped rather than a descendant alive, while the stderr waits keep
// their own bound so a short caller deadline does not cost the diagnostics.
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
