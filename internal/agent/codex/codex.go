// Package codex implements [domain.AgentAdapter] for the OpenAI Codex
// CLI. It launches `codex app-server` as a persistent subprocess,
// communicates via JSON-RPC 2.0 over stdin/stdout (JSONL), and
// normalizes events into domain types. Registered under kind "codex"
// via an init function. Unlike the Claude Code and Copilot adapters,
// the subprocess persists across turns within a session.
package codex

import (
	"cmp"
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
	"sync/atomic"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/redact"
	"github.com/sortie-ai/sortie/internal/registry"
)

const defaultCommand = "codex app-server"

// appServerSource: the app-server's usedPercent is a percent, not a
// fraction, and each window states its own length and name.
var appServerSource = usage.Source{Windows: usage.WindowDeclaration{Share: usage.SharePercent}}

// usageSources: the rollout file records a request when its response
// completes, before the tool calls run, so a process killed during them
// keeps the request there although the app server never reports it.
var usageSources = usage.Sources{
	Live: usage.Source{
		Category: usage.SourceOutput,
		Strategy: usage.StrategyLive,
		Spend: usage.SpendDeclaration{
			Shape:             usage.SpendSessionCumulative,
			CacheInInput:      true,
			ReasoningInOutput: true,
			ServedModel:       true,
			DuringTurn:        true,
			Replay:            usage.ReplayLastFigure,
		},
	},
	Records: usage.Source{
		Category: usage.SourceJournal,
		Strategy: usage.StrategyRecords,
		Spend: usage.SpendDeclaration{
			Shape:             usage.SpendPerRequest,
			CacheInInput:      true,
			ReasoningInOutput: true,
			ServedModel:       true,
		},
	},
}

func init() {
	registry.Agents.RegisterWithMeta("codex", NewCodexAdapter, registry.AgentMeta{
		RequiresCommand:       true,
		DefaultCommand:        defaultCommand,
		ValidateAgentConfig:   validateConfig,
		MCPDialect:            registry.MCPDialectMCPServersTOML,
		UsageSources:          usageSources,
		UsageLimitRecognition: registry.UsageLimitRecognized,
		CredentialEnv:         registry.DeclareCredentialEnv(),
		EffortForwarding:      registry.EffortForwarded,
		VerificationContext: registry.DeclareVerificationContext(
			registry.VerificationKeepsSomeTools,
			registry.VerificationKeepsUserInstructions,
		),
	})
}

var _ domain.AgentAdapter = (*CodexAdapter)(nil)

// CodexAdapter satisfies [domain.AgentAdapter] with one persistent codex
// app-server subprocess per session. One adapter serves all concurrent
// sessions.
type CodexAdapter struct {
	// drainGrace overrides procutil.DefaultDrainGrace when positive; only
	// tests set it.
	drainGrace time.Duration
}

// sessionState is what [domain.Session] Internal carries.
type sessionState struct {
	target agentcore.LaunchTarget

	// tools is closed by StopSession after the process teardown.
	tools *agentcore.ToolSession

	agentConfig domain.AgentConfig
	turnCount   int

	passthrough passthroughConfig

	threadID string

	// model is never reset between turns: the subprocess and its thread
	// outlive the turn. Only a model/rerouted notification replaces it.
	model string

	conn *jsonrpc.Conn

	// release ends a call left waiting on a reaped runtime whose reader
	// did not end within drainGrace. Safe to read from any goroutine.
	release *procutil.OutputRelease

	// stderrReported makes a session warn about its dead runtime once,
	// however many failure paths reach reportStderr: a later failure
	// against the same runtime has nothing new to add.
	stderrReported atomic.Bool

	// reportMu orders stopping against reportStderr's decision to warn,
	// so a stop cannot land between the check and the warning. It is
	// never held across the drain wait, which would make a stop pay for it.
	reportMu sync.Mutex

	// stopping is set before StopSession closes anything: closing the
	// connection sends an in-flight turn down the path a dead runtime
	// takes, and the operator must not be warned about a runtime they
	// stopped themselves.
	stopping atomic.Bool

	acc    *agentcore.RunUsage
	ledger *usage.Ledger

	// rollout is nil when the thread operation reported no usable path.
	rollout *agentcore.RuntimePath

	turnReading *usage.SpendReading

	// windows survive across turns because a window is announced before
	// the turn it refuses ends. Touched only by the goroutine running
	// RunTurn.
	windows []domain.UsageWindow

	// usageMeasured is never cleared once set.
	usageMeasured bool

	// mu guards group, waitCh, stdin, pipes and stderrCollector. It
	// guards no write to the peer; conn owns its own writer goroutine.
	mu              sync.Mutex
	group           *procutil.Group
	waitCh          <-chan struct{}
	stdin           io.WriteCloser
	pipes           *procutil.OwnedPipes
	stderrCollector *procutil.StderrCollector

	drainGrace time.Duration

	// The handshake wait loops, then RunTurn, are the inbox's only
	// takers. readerDone closes after the inbox does.
	inbox      *jsonrpc.Inbox[jsonrpc.Message]
	readerDone chan struct{}

	reaper *procutil.Reaper

	credentialVerification bool

	reducedVerificationContext bool
}

func (state *sessionState) sshConnectionFailed() bool {
	return agentcore.ReaperConnectionFailed(state.target.RemoteCommand != "", state.reaper, state.drainGrace)
}

func connectionFailureErr(state *sessionState) *domain.AgentError {
	if !state.sshConnectionFailed() {
		return nil
	}
	return agentcore.ConnectionFailedError()
}

// earlyExitObservation records the shared early-exit observation for a
// startup handshake step's error, or the zero value when err carries
// no [errConnectionLost] sentinel. Call it before state.closeConn and
// killOnError, so the reaper it reads has not yet been signalled by
// either.
func earlyExitObservation(ctx context.Context, err error, target agentcore.LaunchTarget, state *sessionState) agentcore.EarlyExit {
	if !errors.Is(err, errConnectionLost) {
		return agentcore.EarlyExit{}
	}
	return agentcore.ObserveEarlyExit(ctx, target, state.reaper, state.drainGrace)
}

// startFailure builds the start error for a handshake step that lost its
// connection: the early-exit report when the runtime was observed gone,
// else the SSH connection failure, else a response error naming step.
// It never returns nil.
func startFailure(state *sessionState, observation agentcore.EarlyExit, step string, err error) *domain.AgentError {
	if report := observation.Report(state.stderrCollector); report != nil {
		return report
	}
	if sshErr := connectionFailureErr(state); sshErr != nil {
		return sshErr
	}
	return &domain.AgentError{
		Kind:    domain.ErrResponseError,
		Message: fmt.Sprintf("%s failed: %v", step, err),
		Err:     err,
	}
}

// resumedThread is what a thread/resume result reported; both fields
// are empty when the result does not decode.
type resumedThread struct {
	model string
	path  string
}

// probeResume reads the thread/resume outcome as the resume signal. A lost
// connection returns its start error here, built before the caller tears the
// runtime down, because the reaper and the collected standard error are
// still intact.
func (state *sessionState) probeResume(ctx context.Context, resumeID string) (agentcore.ResumeSignal[resumedThread], *domain.AgentError) {
	model, path, err := resumeThread(ctx, state, resumeID)
	switch {
	case err == nil:
		return agentcore.ResumeSignal[resumedThread]{
			Verdict: agentcore.ResumeCarried,
			Answer:  resumedThread{model: model, path: path},
		}, nil
	case errors.Is(err, errResumeRejected):
		return agentcore.ResumeSignal[resumedThread]{Verdict: agentcore.ResumeReplaced, Cause: err}, nil
	case errors.Is(err, errConnectionLost):
		observation := earlyExitObservation(ctx, err, state.target, state)
		return agentcore.ResumeSignal[resumedThread]{}, startFailure(state, observation, "thread/resume", err)
	default:
		return agentcore.ResumeSignal[resumedThread]{Cause: err}, nil
	}
}

// settleTurn settles the ended turn through the ledger and folds the
// settled figure into the run usage. It runs detached from ctx so a
// cancelled turn still reads the records of the requests it completed.
func (state *sessionState) settleTurn(ctx context.Context, end usage.TurnEnd, spend *agentcore.TurnSpend, logger *slog.Logger) {
	read := func(readCtx context.Context) usage.RecordsRead {
		return state.readRollout(readCtx, logger, false)
	}
	recovered := agentcore.SettleSpend(context.WithoutCancel(ctx), state.ledger, end, state.turnReading,
		read, agentcore.ReadTimeout(state.agentConfig), spend)
	if recovered != nil {
		state.acc.SetRunCumulative(recovered.Run, recovered.ByModel)
		state.usageMeasured = true
	}
}

func (state *sessionState) closeConn() {
	if state.conn != nil {
		state.conn.Close()
	}
}

// reportStderr warns with what the runtime wrote to standard error, so
// a session whose runtime is gone reports what it said rather than an
// exit code alone. It warns once per session and never after a stop. A
// call that precedes the release goroutine's own drain waits up to
// drainGrace for it.
func (state *sessionState) reportStderr(logger *slog.Logger) {
	if state.stopping.Load() {
		return
	}
	state.mu.Lock()
	collector := state.stderrCollector
	grace := state.drainGrace
	state.mu.Unlock()
	if collector == nil {
		return
	}
	if grace <= 0 {
		grace = procutil.DefaultDrainGrace
	}
	lines := collector.FinishAndCollect(grace)

	// The drain above can last the whole bound; a stop that arrived
	// inside it must not find the warning already on its way out.
	state.reportMu.Lock()
	defer state.reportMu.Unlock()
	if state.stopping.Load() {
		return
	}
	if state.stderrReported.Swap(true) {
		return
	}
	procutil.EmitWarnLines(lines, logger)
}

// readerEnded reports whether the connection's reader has exited, so the
// standard-error drain will end on its own. It never blocks: a turn that
// failed against a live runtime must not pay the drain bound.
//
// conn.Done() is polled first because a failed call returns as soon as
// it closes, just before watchTermination closes readerDone.
func (state *sessionState) readerEnded() bool {
	if state.conn != nil {
		select {
		case <-state.conn.Done():
			return true
		default:
		}
	}
	if state.readerDone == nil {
		return false
	}
	select {
	case <-state.readerDone:
		return true
	default:
		return false
	}
}

// watchTermination closes state.inbox, then state.readerDone, once
// conn's reader has exited, telling every inbox taker no message follows.
func watchTermination(state *sessionState) {
	<-state.conn.Done()
	state.inbox.Close()
	close(state.readerDone)
}

// identity is the delivery wrap: the inbox carries jsonrpc.Message as is.
func identity(msg jsonrpc.Message) jsonrpc.Message { return msg }

// drainHandshakeMessages handles every message the handshake left
// queued, so the first turn starts with an empty inbox and nothing
// observed before it is silently lost. state.threadID must be set.
func drainHandshakeMessages(state *sessionState, logger *slog.Logger) {
	sessionLogger := logging.WithSession(logger, state.threadID)
	for {
		msg, ok := state.inbox.Take()
		if !ok {
			return
		}
		handleOutOfTurnMessage(state, msg, sessionLogger)
	}
}

// handleOutOfTurnMessage answers or reports a message that arrived
// outside a turn, so every request still gets exactly one reply. A
// token-usage figure reaches the ledger, which takes the figure a
// resumed thread repeats as the run's starting point. Other
// notifications name nothing actionable before a turn exists and are
// dropped, never replayed into the first turn.
func handleOutOfTurnMessage(state *sessionState, msg jsonrpc.Message, logger *slog.Logger) {
	switch msg.Kind {
	case jsonrpc.KindRequest:
		if !answerable(msg.ID) {
			return
		}
		if msg.Method == "mcpServer/elicitation/request" {
			answerElicitationRequest(state, msg.ID)
			return
		}
		answerUnrecognizedRequest(state, msg.ID)

	case jsonrpc.KindNotification:
		switch msg.Method {
		case "mcpServer/startupStatus/updated":
			reportMCPStartupFailure(msg, logger)
		case "thread/tokenUsage/updated":
			if p, err := parseTokenUsageUpdated(msg.Params); err == nil && p.TokenUsage != nil {
				state.ledger.ObserveCumulative(p.TokenUsage.reading(state.model), false)
			}
		}
	}
}

// answerable reports whether id names a request to answer. A null id
// is present on the wire but names none; the app-server never sends one.
func answerable(id jsonrpc.ID) bool {
	return id.Present() && !id.IsNull()
}

// answerElicitationRequest declines: sortie runs unattended, so no
// person is present to supply the requested input.
func answerElicitationRequest(state *sessionState, requestID jsonrpc.ID) {
	state.conn.Respond(requestID, map[string]any{"action": "decline"}) //nolint:errcheck,gosec // best-effort refusal
}

func answerUnrecognizedRequest(state *sessionState, requestID jsonrpc.ID) {
	state.conn.RespondError(requestID, jsonrpc.MethodNotFoundCode, jsonrpc.MethodNotFoundMessage) //nolint:errcheck,gosec // best-effort
}

func reportMCPStartupFailure(msg jsonrpc.Message, logger *slog.Logger) {
	var su mcpServerStartupStatus
	if err := json.Unmarshal(msg.Params, &su); err != nil {
		logger.Debug("mcpServer/startupStatus/updated unmarshal failed", slog.Any("error", err))
		return
	}
	if su.Status == "failed" {
		reason := cmp.Or(su.FailureReason, su.Error)
		logger.Warn("MCP server failed to start",
			slog.String("mcp_server", su.Name),
			slog.String("reason", reason))
	}
}

// NewCodexAdapter returns a [CodexAdapter]. It never fails: settings
// and the command are resolved per session by [CodexAdapter.StartSession].
func NewCodexAdapter() (domain.AgentAdapter, error) {
	return &CodexAdapter{}, nil
}

// StartSession launches the app-server in the session's workspace,
// completes its handshake and login, and starts or resumes a thread.
func (a *CodexAdapter) StartSession(ctx context.Context, params domain.StartSessionParams) (domain.Session, error) {
	pt, fault := parsePassthroughConfig(params.Settings)
	if fault != nil {
		return domain.Session{}, agentcore.SettingsError(fault.Error(), fault)
	}
	target, agentErr := agentcore.ResolveLaunchTarget(params, defaultCommand)
	if agentErr != nil {
		return domain.Session{}, agentErr
	}

	logger := slog.Default().With(slog.String("component", "codex-adapter"))

	ledger, err := usage.NewLedger(usageSources.Live, usageSources.Records)
	if err != nil {
		return domain.Session{}, &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: "usage ledger refused the kind's sources",
			Err:     err,
		}
	}

	tools, agentErr := agentcore.ResolveToolDelivery(ctx, params, target, registry.MCPDialectMCPServersTOML, logger)
	if agentErr != nil {
		return domain.Session{}, agentErr
	}
	started := false
	defer func() {
		if !started {
			tools.Close() //nolint:errcheck,gosec // best-effort; the start failure is what the caller needs
		}
	}()

	var localEnv []string
	if target.RemoteCommand == "" {
		delivery := tools.Delivery(sshutil.ForwardOutcome{})
		target.Args = append(target.Args, delivery.Args...)
		localEnv = append(tools.LocalEnv(), delivery.Env...)
	}

	state := &sessionState{
		target:                 target,
		tools:                  tools,
		agentConfig:            params.AgentConfig,
		passthrough:            pt,
		acc:                    agentcore.NewRunUsage(),
		ledger:                 ledger,
		credentialVerification: params.CredentialVerification,

		reducedVerificationContext: params.ReducedVerificationContext(),
	}

	var cmd *exec.Cmd
	var launch sshutil.SSHLaunch
	if target.RemoteCommand != "" {
		sshOpts := target.SSHOptions()
		sshOpts.Forward = tools.Forward(placeToolServers)
		launch = sshutil.BuildSSHLaunch(target.SSHHost, target.WorkspacePath, target.RemoteCommand, nil, sshOpts)
		cmd = exec.CommandContext(ctx, target.Command, launch.Args...) //nolint:gosec // args are constructed programmatically with shell quoting
	} else {
		cmd = exec.CommandContext(ctx, target.Command, target.Args...) //nolint:gosec // args are constructed programmatically
	}
	grace := procutil.StopGrace(state.agentConfig.StopGraceMS)
	procutil.SetGroupCancel(cmd, grace)
	cmd.Env = append(os.Environ(), localEnv...)
	if bindErr := target.BindWorkspace(cmd); bindErr != nil {
		return domain.Session{}, bindErr
	}

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return domain.Session{}, &domain.AgentError{
			Kind:    domain.ErrPortExit,
			Message: "failed to create stdin pipe",
			Err:     err,
		}
	}
	prefixedStdin := launch.PrefixStdin(stdinPipe)

	pipes, group, err := procutil.StartWithOwnedPipes(ctx, cmd, logger)
	if err != nil {
		if cancelled := agentcore.SessionStartCancelledError(ctx, err); cancelled != nil {
			return domain.Session{}, cancelled
		}
		var startErr *procutil.StartError
		if !errors.As(err, &startErr) {
			return domain.Session{}, &domain.AgentError{
				Kind:    domain.ErrPortExit,
				Message: "failed to start app-server subprocess",
				Err:     err,
			}
		}
		// Both pipe stages fail before cmd.Start, whose deferred cleanup
		// is what closes the parent's stdin end on a failed launch, so
		// these close it instead. The process-start and resume stages
		// need none.
		switch startErr.Stage {
		case procutil.StageStdoutPipe:
			stdinPipe.Close() //nolint:errcheck,gosec // best-effort; the pipe error is what the caller needs
			return domain.Session{}, &domain.AgentError{
				Kind:    domain.ErrPortExit,
				Message: "failed to create stdout pipe",
				Err:     startErr.Err,
			}
		case procutil.StageStderrPipe:
			stdinPipe.Close() //nolint:errcheck,gosec // best-effort; the pipe error is what the caller needs
			return domain.Session{}, &domain.AgentError{
				Kind:    domain.ErrPortExit,
				Message: "failed to create stderr pipe",
				Err:     startErr.Err,
			}
		default: // procutil.StageProcessStart, procutil.StageProcessResume
			return domain.Session{}, &domain.AgentError{
				Kind:    domain.ErrPortExit,
				Message: "failed to start app-server subprocess",
				Err:     startErr.Err,
			}
		}
	}

	state.group = group
	state.stdin = prefixedStdin
	state.pipes = pipes
	state.drainGrace = a.drainGrace
	if state.drainGrace <= 0 {
		state.drainGrace = procutil.DefaultDrainGrace
	}
	state.stderrCollector = procutil.NewStderrCollector(launch.WatchStderr(pipes.Stderr), logger)

	reaper := procutil.StartReaper(cmd, logger)
	state.waitCh = reaper.Done()
	state.reaper = reaper

	killOnError := func() {
		state.mu.Lock()
		procutil.CloseWithoutWaiting(state.stdin)
		if state.pipes != nil {
			state.pipes.CloseStdout() //nolint:errcheck,gosec // unblock the reader goroutine on the read end
		}
		state.mu.Unlock()
		group.Kill() //nolint:errcheck,gosec // best-effort cleanup
		select {
		case <-state.waitCh:
		case <-time.After(3 * time.Second):
		}
		// Before the close below: standard error is read from the write
		// end going away, and closing the read end here instead would
		// drop whatever the runtime had written but the drain had not
		// yet scanned.
		state.reportStderr(logger)

		state.mu.Lock()
		state.group = nil
		state.stdin = nil
		if state.pipes != nil {
			state.pipes.Close() //nolint:errcheck,gosec // best-effort cleanup; StartSession's failure paths return no session to close these later
		}
		state.pipes = nil
		state.mu.Unlock()
	}

	// Before the connection: its reader may deliver as soon as it exists.
	state.inbox = jsonrpc.NewInbox[jsonrpc.Message]()
	state.readerDone = make(chan struct{})

	state.conn = jsonrpc.NewConn(prefixedStdin, pipes.Stdout, jsonrpc.Deliver(state.inbox, identity))
	// Started before the handshake so the handshake wait loops observe
	// a closed inbox, rather than timing out, when stdout ends mid-handshake.
	go watchTermination(state)
	// The release takes its own copy of pipes because killOnError clears
	// state.pipes on every handshake failure path below.
	state.release = procutil.StartOutputRelease(procutil.OutputReleaseParams{
		Pipes:      pipes,
		Reaped:     reaper.Done(),
		ReaderDone: state.conn.Done(),
		Stderr:     state.stderrCollector,
		Grace:      state.drainGrace,
		OnAbandon:  state.closeConn,
		Logger:     logger,
	})

	if err := initializeHandshake(ctx, state); err != nil {
		observation := earlyExitObservation(ctx, err, target, state)
		state.closeConn()
		killOnError()
		if report := observation.Report(state.stderrCollector); report != nil {
			return domain.Session{}, report
		}
		if sshErr := connectionFailureErr(state); sshErr != nil {
			return domain.Session{}, sshErr
		}
		return domain.Session{}, &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: fmt.Sprintf("handshake failed: %v", err),
			Err:     err,
		}
	}

	if err := authenticateIfNeeded(ctx, state, logger); err != nil {
		var agentErr *domain.AgentError
		if ok := isAgentError(err, &agentErr); ok {
			state.closeConn()
			killOnError()
			return domain.Session{}, agentErr
		}
		observation := earlyExitObservation(ctx, err, target, state)
		state.closeConn()
		killOnError()
		if report := observation.Report(state.stderrCollector); report != nil {
			return domain.Session{}, report
		}
		if sshErr := connectionFailureErr(state); sshErr != nil {
			return domain.Session{}, sshErr
		}
		return domain.Session{}, &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: fmt.Sprintf("authentication failed: %v", err),
			Err:     err,
		}
	}

	start, resumeErr := agentcore.ResolveSessionStart(ctx, params.ResumeSessionID,
		agentcore.ResumeProbeFunc[resumedThread](state.probeResume), logger)
	if resumeErr != nil {
		state.closeConn()
		killOnError()
		return domain.Session{}, resumeErr
	}

	var threadID, model, path string
	if start.CarriedHistory() {
		resumed := start.Answer()
		threadID, model, path = start.ResumeID(), resumed.model, resumed.path
	} else {
		var threadErr error
		threadID, model, path, threadErr = startThread(ctx, state, pt, logger)
		if threadErr != nil {
			observation := earlyExitObservation(ctx, threadErr, target, state)
			state.closeConn()
			killOnError()
			return domain.Session{}, startFailure(state, observation, "thread/start", threadErr)
		}
	}

	if awaitErr := tools.AwaitConnection(ctx); awaitErr != nil {
		state.closeConn()
		killOnError()
		return domain.Session{}, awaitErr
	}

	state.threadID = threadID
	state.model = model
	state.rollout = rolloutPath(path)
	if path != "" && state.rollout == nil {
		logger.Debug("rollout path unrecognized", slog.String("path", path))
	}
	ledger.BeginSession(start.CarriedHistory(), func() usage.RecordsRead {
		readCtx, cancel := context.WithTimeout(ctx, agentcore.ReadTimeout(params.AgentConfig))
		defer cancel()
		return state.readRollout(readCtx, logger, true)
	})
	drainHandshakeMessages(state, logger)

	started = true
	return domain.Session{
		ID:             threadID,
		AgentPID:       strconv.Itoa(cmd.Process.Pid),
		Internal:       state,
		NoToolChannel:  tools.NoToolChannel(),
		CarriedHistory: start.CarriedHistory(),
	}, nil
}

// codexRefusalErrorCode and codexRefusalMessage are pinned rather than
// derived: the app-server schema enumerates neither the error code of
// an implementation-defined refusal nor the legacy ReviewDecision
// denial text, and it acknowledges no client response, so neither can
// be confirmed accepted at runtime. The error code serves requests
// whose response schema has no shape that could express a decline.
const (
	codexRefusalErrorCode = -32001
	codexRefusalMessage   = "sortie refuses requests that only a person could answer"
)

// detailAnswerToQuestion and detailWiderAccess name, for the operator,
// the ask behind a refused request for human input.
const (
	detailAnswerToQuestion = "an answer to a question"
	detailWiderAccess      = "wider filesystem or network access"
)

// RunTurn sends a turn/start request on the existing thread and reads
// events until turn/completed. Events are delivered synchronously via
// params.OnEvent.
func (a *CodexAdapter) RunTurn(ctx context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
	if params.OnEvent == nil {
		panic("codex: OnEvent must be non-nil")
	}

	state, ok := session.Internal.(*sessionState)
	if !ok {
		return domain.TurnResult{}, &domain.AgentError{
			Kind:    domain.ErrPortExit,
			Message: fmt.Sprintf("unexpected session internal type %T", session.Internal),
		}
	}

	logger := logging.WithSession(
		slog.Default().With(slog.String("component", "codex-adapter")),
		state.threadID,
	)
	emit := state.tools.Emit(params.OnEvent, nil)

	state.turnCount++
	state.turnReading = nil

	resp, err := state.conn.Call(ctx, "turn/start", turnStartParams(state, params.Prompt))
	if err != nil {
		if state.readerEnded() {
			state.reportStderr(logger)
			if sshErr := connectionFailureErr(state); sshErr != nil {
				return domain.TurnResult{UsageMeasured: state.usageMeasured}, sshErr
			}
		}
		return domain.TurnResult{UsageMeasured: state.usageMeasured}, &domain.AgentError{
			Kind:    domain.ErrPortExit,
			Message: state.release.TurnEndMessage(fmt.Sprintf("turn/start failed: %v", err)),
			Err:     err,
		}
	}
	if resp.Error != nil {
		ev := agentcore.TurnEvidence{
			Terminal:          agentcore.TerminalFailure,
			TerminalErrorKind: domain.ErrTurnFailed,
			TerminalMessage:   fmt.Sprintf("turn/start error: %s", resp.Error.Message),
		}
		meta := agentcore.TurnMeta{SessionID: state.threadID, UsageMeasured: state.usageMeasured}
		result, agentErr := agentcore.FinalizeTurn(emit, logger, ev, meta, agentcore.TurnSpend{})
		if agentErr != nil {
			return result, agentErr
		}
		return result, nil
	}

	var turnResult turnStartResult
	if err := json.Unmarshal(resp.Result, &turnResult); err != nil {
		logger.Warn("turn/start result unmarshal failed", slog.Any("error", err))
	}
	turnID := turnResult.Turn.ID
	var spend agentcore.TurnSpend

	inFlight := agentcore.NewToolTracker()
	ctxDone := ctx.Done()
	// cancelDeadline stays nil, never selectable, until cancellation arms
	// it: past it the interrupt is presumed lost, and the loop returns
	// rather than reading until stdout closes.
	var cancelDeadline <-chan time.Time

	for {
		select {
		case <-ctxDone:
			// A cancelled context's Done stays ready forever; the interrupt
			// is sent once.
			ctxDone = nil
			state.conn.SendRequest("turn/interrupt", map[string]any{ //nolint:errcheck,gosec // best-effort interrupt
				"threadId": state.threadID,
				"turnId":   turnID,
			})
			cancelDeadline = time.After(agentcore.ReadTimeout(state.agentConfig))
			continue

		case <-cancelDeadline:
			ev := agentcore.TurnEvidence{Terminal: agentcore.TerminalCancelled}
			state.settleTurn(ctx, usage.TurnEnd{ModelReached: spend.ModelReached()}, &spend, logger)
			meta := agentcore.TurnMeta{
				SessionID:     state.threadID,
				Usage:         state.acc.Snapshot(),
				UsageByModel:  state.acc.ByModel(),
				UsageMeasured: state.usageMeasured,
			}
			result, agentErr := agentcore.FinalizeTurn(emit, logger, ev, meta, spend)
			if agentErr != nil {
				return result, agentErr
			}
			return result, nil

		case <-state.inbox.Ready():
			msg, ok := state.inbox.Take()
			if !ok {
				state.reportStderr(logger)
				ev := agentcore.TurnEvidence{
					Terminal:          agentcore.TerminalFailure,
					TerminalErrorKind: domain.ErrPortExit,
					TerminalMessage:   state.release.TurnEndMessage("subprocess stdout closed unexpectedly"),
				}
				if agentcore.ConnectionFailedForRequest(state.sshConnectionFailed(), false) {
					connErr := agentcore.ConnectionFailedError()
					ev.TerminalErrorKind = connErr.Kind
					ev.TerminalMessage = connErr.Message
					ev.Cause = connErr.Err
				}
				state.settleTurn(ctx, usage.TurnEnd{ModelReached: spend.ModelReached()}, &spend, logger)
				meta := agentcore.TurnMeta{
					SessionID:     state.threadID,
					Usage:         state.acc.Snapshot(),
					UsageByModel:  state.acc.ByModel(),
					UsageMeasured: state.usageMeasured,
				}
				result, agentErr := agentcore.FinalizeTurn(emit, logger, ev, meta, spend)
				if agentErr != nil {
					return result, agentErr
				}
				return result, nil
			}
			if msg.Kind == jsonrpc.KindMalformed || msg.Kind == jsonrpc.KindStreamEnd {
				// Only a stream end means the runtime is gone. After a
				// malformed line the runtime lives on, so reporting would
				// wait the drain bound on a pipe it still holds and latch
				// the report away from the turn that later loses it.
				if msg.Kind == jsonrpc.KindStreamEnd {
					state.reportStderr(logger)
				}
				ev := agentcore.TurnEvidence{
					Terminal:          agentcore.TerminalFailure,
					TerminalErrorKind: domain.ErrPortExit,
					TerminalMessage:   state.release.TurnEndMessage(fmt.Sprintf("stdout read error: %v", msg.Err)),
					Cause:             msg.Err,
				}
				state.settleTurn(ctx, usage.TurnEnd{ModelReached: spend.ModelReached()}, &spend, logger)
				meta := agentcore.TurnMeta{
					SessionID:     state.threadID,
					Usage:         state.acc.Snapshot(),
					UsageByModel:  state.acc.ByModel(),
					UsageMeasured: state.usageMeasured,
				}
				result, agentErr := agentcore.FinalizeTurn(emit, logger, ev, meta, spend)
				if agentErr != nil {
					return result, agentErr
				}
				return result, nil
			}

			// An unmatched response carries no method to dispatch.
			if msg.Kind != jsonrpc.KindNotification && msg.Kind != jsonrpc.KindRequest {
				continue
			}

			now := time.Now().UTC()
			method := msg.Method

			switch method {
			case "turn/started":
				if state.turnCount == 1 {
					agentcore.EmitSessionStarted(emit, session.AgentPID, state.threadID)
				} else {
					agentcore.EmitNotification(emit, "")
				}

			case "thread/tokenUsage/updated":
				p, parseErr := parseTokenUsageUpdated(msg.Params)
				if parseErr != nil {
					logger.Debug("thread/tokenUsage/updated unmarshal failed", slog.String("method", method))
					continue
				}
				if p.TokenUsage == nil {
					continue
				}
				reading := p.TokenUsage.reading(state.model)
				inTurn := turnID != "" && p.TurnID == turnID
				provisional, isNew := state.ledger.ObserveCumulative(reading, inTurn)
				if !inTurn {
					continue
				}
				spend.MarkModelReached()
				state.turnReading = &reading
				if isNew {
					snapshot := state.acc.SetTurnProvisional(provisional)
					emit(domain.AgentEvent{
						Type:         domain.EventTokenUsage,
						Timestamp:    now,
						Usage:        snapshot,
						UsageByModel: state.acc.ByModel(),
						Model:        reading.Model,
					})
					state.usageMeasured = true
				}

			case "turn/completed":
				var tc turnCompletedParams
				if err := json.Unmarshal(msg.Params, &tc); err != nil {
					logger.Warn("turn/completed unmarshal failed", slog.Any("error", err))
				}

				state.settleTurn(ctx, usage.TurnEnd{
					Replied:      tc.Turn.Status != "",
					Cancelled:    tc.Turn.Status == "interrupted",
					ModelReached: spend.ModelReached(),
				}, &spend, logger)

				var ev agentcore.TurnEvidence

				switch {
				case ctx.Err() != nil:
					ev.Terminal = agentcore.TerminalCancelled
					ev.TerminalMessage = cancelledMessage(tc.Turn.Status, tc.Turn.Error)
				case tc.Turn.Status == "completed":
					ev.Terminal = agentcore.TerminalSuccess
				case tc.Turn.Status == "interrupted":
					// An interrupt sortie did not request is a failure,
					// not a cancellation, from the orchestrator's side:
					// it must not release the claim in place of a retry.
					ev.Terminal = agentcore.TerminalFailure
					ev.TerminalErrorKind = domain.ErrTurnFailed
				case tc.Turn.Status == "failed" && tc.Turn.Error != nil:
					ev.Terminal = agentcore.TerminalFailure
					ev.TerminalErrorKind = mapCodexErrorInfo(string(tc.Turn.Error.CodexErrorInfo))
					ev.TerminalMessage = tc.Turn.Error.Message
					if ev.TerminalErrorKind == domain.ErrCredentialUnverified {
						ev.TerminalMessage = agentcore.CredentialRefusedMessage(tc.Turn.Error.Message)
					}
					if ev.TerminalErrorKind == domain.ErrUsageLimit {
						ev.UsageLimit = &domain.UsageLimit{ResetsAt: usage.RefusalReset(state.windows, state.passthrough.Model, time.Now().UTC())}
					}
				default:
					ev.Terminal = agentcore.TerminalFailure
					ev.TerminalErrorKind = domain.ErrTurnFailed
				}
				// Without a status word agentcore.DecideTurn's own fallback
				// message applies.
				if ev.TerminalMessage == "" && tc.Turn.Status != "" {
					ev.TerminalMessage = "turn " + tc.Turn.Status
				}

				meta := agentcore.TurnMeta{
					SessionID:     state.threadID,
					Usage:         state.acc.Snapshot(),
					UsageByModel:  state.acc.ByModel(),
					UsageMeasured: state.usageMeasured,
				}

				result, agentErr := agentcore.FinalizeTurn(emit, logger, ev, meta, spend)
				if agentErr != nil {
					return result, agentErr
				}
				return result, nil

			case "item/started":
				var ip itemParams
				if err := json.Unmarshal(msg.Params, &ip); err != nil {
					logger.Debug("item/started unmarshal failed", slog.Any("error", err))
					continue
				}
				item := ip.Item
				if call, isTool := ip.toolCall(); isTool {
					inFlight.Begin(item.ID, call)
					spend.MarkModelReached()
				} else if item.Type == "reasoning" {
					spend.MarkModelReached()
				}
				agentcore.EmitNotification(emit, "")

			case "item/completed":
				var ip itemParams
				if err := json.Unmarshal(msg.Params, &ip); err != nil {
					logger.Debug("item/completed unmarshal failed", slog.Any("error", err))
					continue
				}
				item := ip.Item
				if call, durationMS, ok := inFlight.End(item.ID); ok {
					emit(domain.AgentEvent{
						Type:           domain.EventToolResult,
						Timestamp:      now,
						ToolName:       call.Name,
						ToolKind:       call.Kind,
						ToolArgument:   call.Argument,
						ToolDurationMS: durationMS,
						ToolError:      item.Status == "failed" || item.Status == "declined",
					})
				}
				if item.Type == "agentMessage" || item.Type == "reasoning" {
					spend.MarkModelReached()
				}
				if item.Type == "agentMessage" && item.Text != "" {
					agentcore.EmitNotification(emit, redact.Truncate(item.Text, 200))
				}

			case "item/agentMessage/delta":
				spend.MarkModelReached()
				agentcore.EmitNotification(emit, "")

			case "item/commandExecution/outputDelta":
				agentcore.EmitNotification(emit, "")

			case "item/commandExecution/requestApproval", "item/fileChange/requestApproval",
				"applyPatchApproval", "execCommandApproval",
				"mcpServer/elicitation/request", "item/permissions/requestApproval",
				"item/tool/requestUserInput":
				requestID := msg.ID
				if !answerable(requestID) {
					logger.Debug("codex request without id ignored", slog.String("method", method))
					agentcore.EmitNotification(emit, "")
					break
				}

				// A turn the orchestrator cancelled ends cancelled, whatever
				// the request asks.
				if ctx.Err() != nil {
					ev := agentcore.TurnEvidence{Terminal: agentcore.TerminalCancelled}
					state.settleTurn(ctx, usage.TurnEnd{ModelReached: spend.ModelReached()}, &spend, logger)
					meta := agentcore.TurnMeta{
						SessionID:     state.threadID,
						Usage:         state.acc.Snapshot(),
						UsageByModel:  state.acc.ByModel(),
						UsageMeasured: state.usageMeasured,
					}
					result, agentErr := agentcore.FinalizeTurn(emit, logger, ev, meta, spend)
					return result, agentErr
				}

				switch method {
				case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
					posture := agentcore.DecideHumanRequest(agentcore.ClassPermission, true, agentcore.AnswerPending)
					state.conn.Respond(requestID, map[string]any{"decision": "decline"}) //nolint:errcheck,gosec // best-effort refusal
					agentcore.EmitNotification(emit, posture.NoticeWithDetail(""))

				case "applyPatchApproval", "execCommandApproval":
					posture := agentcore.DecideHumanRequest(agentcore.ClassPermission, true, agentcore.AnswerPending)
					state.conn.Respond(requestID, map[string]any{ //nolint:errcheck,gosec // best-effort refusal
						"decision": map[string]any{
							"denied": map[string]any{"rejection": codexRefusalMessage},
						},
					})
					agentcore.EmitNotification(emit, posture.NoticeWithDetail(""))

				case "mcpServer/elicitation/request":
					posture := agentcore.DecideHumanRequest(agentcore.ClassHumanInput, true, agentcore.AnswerPending)
					answerElicitationRequest(state, requestID)
					agentcore.EmitNotification(emit, posture.NoticeWithDetail(detailAnswerToQuestion))

					state.settleTurn(ctx, usage.TurnEnd{ModelReached: spend.ModelReached()}, &spend, logger)
					meta := agentcore.TurnMeta{
						SessionID:     state.threadID,
						Usage:         state.acc.Snapshot(),
						UsageByModel:  state.acc.ByModel(),
						UsageMeasured: state.usageMeasured,
					}
					evidence := agentcore.HumanInputEvidence(detailAnswerToQuestion)
					result, agentErr := agentcore.FinalizeTurn(emit, logger, evidence, meta, spend)
					return result, agentErr

				case "item/permissions/requestApproval":
					posture := agentcore.DecideHumanRequest(agentcore.ClassHumanInput, true, agentcore.AnswerPending)
					state.conn.RespondError(requestID, codexRefusalErrorCode, codexRefusalMessage) //nolint:errcheck,gosec // best-effort refusal
					agentcore.EmitNotification(emit, posture.NoticeWithDetail(detailWiderAccess))

					state.settleTurn(ctx, usage.TurnEnd{ModelReached: spend.ModelReached()}, &spend, logger)
					meta := agentcore.TurnMeta{
						SessionID:     state.threadID,
						Usage:         state.acc.Snapshot(),
						UsageByModel:  state.acc.ByModel(),
						UsageMeasured: state.usageMeasured,
					}
					evidence := agentcore.HumanInputEvidence(detailWiderAccess)
					result, agentErr := agentcore.FinalizeTurn(emit, logger, evidence, meta, spend)
					return result, agentErr

				case "item/tool/requestUserInput":
					posture := agentcore.DecideHumanRequest(agentcore.ClassHumanInput, true, agentcore.AnswerPending)
					state.conn.RespondError(requestID, codexRefusalErrorCode, codexRefusalMessage) //nolint:errcheck,gosec // best-effort refusal
					agentcore.EmitNotification(emit, posture.NoticeWithDetail(detailAnswerToQuestion))

					state.settleTurn(ctx, usage.TurnEnd{ModelReached: spend.ModelReached()}, &spend, logger)
					meta := agentcore.TurnMeta{
						SessionID:     state.threadID,
						Usage:         state.acc.Snapshot(),
						UsageByModel:  state.acc.ByModel(),
						UsageMeasured: state.usageMeasured,
					}
					evidence := agentcore.HumanInputEvidence(detailAnswerToQuestion)
					result, agentErr := agentcore.FinalizeTurn(emit, logger, evidence, meta, spend)
					return result, agentErr
				}

			case "turn/plan/updated":
				agentcore.EmitNotification(emit, "")

			case "turn/diff/updated":
				logger.Debug("diff updated")

			case "mcpServer/startupStatus/updated":
				reportMCPStartupFailure(msg, logger)

			case "account/rateLimits/updated":
				p, parseErr := parseRateLimitsUpdated(msg.Params)
				if parseErr != nil {
					logger.Debug("account/rateLimits/updated unmarshal failed", slog.Any("error", parseErr))
					agentcore.EmitNotification(emit, "")
					continue
				}
				at := time.Now().UTC()
				normalized := usage.Windows(appServerSource, p.windowReadings(), at)
				agentcore.EmitUsageWindows(emit, at, normalized)
				state.windows = usage.MergeWindows(state.windows, normalized)

			case "model/rerouted":
				p, parseErr := parseModelRerouted(msg.Params)
				if parseErr != nil {
					logger.Debug("model/rerouted unmarshal failed", slog.Any("error", parseErr))
					agentcore.EmitNotification(emit, reroutedMessage(""))
					continue
				}
				if p.ToModel != "" {
					state.model = p.ToModel
				}
				agentcore.EmitNotification(emit, reroutedMessage(p.ToModel))

			default:
				if msg.Kind == jsonrpc.KindRequest && answerable(msg.ID) {
					answerUnrecognizedRequest(state, msg.ID)
				}
				logger.Debug("codex message logged only", slog.String("method", method))
				agentcore.EmitNotification(emit, "")
			}
		}
	}
}

// turnStartParams resends sandboxPolicy only on the first turn or when
// the operator configured a per-turn policy.
func turnStartParams(state *sessionState, prompt string) map[string]any {
	turnParams := map[string]any{
		"threadId": state.threadID,
		"input":    []map[string]any{{"type": "text", "text": prompt}},
		"cwd":      state.target.WorkspacePath,
	}
	if state.turnCount == 1 || state.passthrough.TurnSandboxPolicy != nil {
		turnParams["sandboxPolicy"] = buildSandboxPolicy(state, state.passthrough)
	}
	if state.passthrough.Model != "" {
		turnParams["model"] = state.passthrough.Model
	}
	if state.passthrough.Effort != "" {
		turnParams["effort"] = state.passthrough.Effort
	}
	return turnParams
}

// cancelledMessage is the terminal message for a turn/completed that
// arrives after the orchestrator cancelled the turn.
func cancelledMessage(status string, turnErr *turnError) string {
	var message string
	if status == "" {
		message = "turn cancelled after the runtime reported no status"
	} else {
		message = "turn cancelled after the runtime reported status " + status
	}
	if turnErr != nil && turnErr.Message != "" {
		message += ": " + turnErr.Message
	}
	return message
}

func reroutedMessage(toModel string) string {
	if toModel == "" {
		return "model rerouted"
	}
	return "model rerouted to " + toModel
}

// StopSession terminates the persistent app-server subprocess. The
// configured stop grace is a ceiling on the wait for a clean exit; a
// caller deadline that expires first ends the graceful phase and is
// reported back, so the caller learns the stop did not complete on its
// own terms.
func (a *CodexAdapter) StopSession(ctx context.Context, session domain.Session) error {
	state, ok := session.Internal.(*sessionState)
	if !ok {
		return fmt.Errorf("unexpected session internal type %T", session.Internal)
	}

	// Before anything below is closed, so a running turn does not report
	// the stop as a dead runtime.
	state.reportMu.Lock()
	state.stopping.Store(true)
	state.reportMu.Unlock()

	// Before stdin: this stops delivery and fails any call in flight first.
	state.closeConn()

	// Closing stdin must not hold up the signal, wait and kill that follow.
	state.mu.Lock()
	procutil.CloseWithoutWaiting(state.stdin)
	waitCh := state.waitCh
	pipes := state.pipes
	group := state.group
	state.mu.Unlock()

	group.SignalGraceful() //nolint:errcheck,gosec // best-effort graceful shutdown

	logger := logging.WithSession(
		slog.Default().With(slog.String("component", "codex-adapter")),
		state.threadID,
	)

	var stopErr error
	if waitCh != nil {
		grace := procutil.StopGrace(state.agentConfig.StopGraceMS)
		started := time.Now()
		escalate := func(outcome string) {
			logger.Warn("agent did not exit inside the graceful period and was force-terminated",
				slog.String("outcome", outcome), slog.Duration("grace", grace),
				slog.Duration("elapsed", time.Since(started)))
			group.Kill() //nolint:errcheck,gosec // best-effort force kill
			select {
			case <-waitCh:
			case <-time.After(2 * time.Second):
			}
		}
		select {
		case <-waitCh:
			logger.Debug("agent exited during the graceful phase", slog.String("outcome", "exited"))
		case <-time.After(grace):
			escalate("grace elapsed")
		case <-ctx.Done():
			escalate("caller deadline")
			stopErr = ctx.Err()
		}
	}

	// Release the connection's reader before waiting for it below: a
	// descendant that inherited the output handle and outlived the
	// direct child would otherwise leave that reader parked forever.
	if pipes != nil {
		pipes.CloseStdout() //nolint:errcheck,gosec // best-effort; unparks the connection's reader for the wait below
	}

	if state.readerDone != nil {
		select {
		case <-state.readerDone:
		case <-time.After(2 * time.Second):
			logger.Warn("reader goroutine did not exit after process termination")
		}
	}

	// Closing the read end mid-read ends the standard-error drain as if
	// at end of file.
	state.mu.Lock()
	state.group = nil
	state.stdin = nil
	if state.pipes != nil {
		state.pipes.Close() //nolint:errcheck,gosec // best-effort cleanup
	}
	collector := state.stderrCollector
	state.pipes = nil
	state.waitCh = nil
	grace := state.drainGrace
	state.mu.Unlock()

	// Joining the drain is what guarantees no collector goroutine outlives
	// the session. Its lines are discarded: stopping is not a failure.
	if collector != nil {
		if grace <= 0 {
			grace = procutil.DefaultDrainGrace
		}
		collector.FinishAndCollect(grace)
	}

	if state.tools != nil {
		if closeErr := state.tools.Close(); closeErr != nil && stopErr == nil {
			stopErr = closeErr
		}
	}
	return stopErr
}

// isAgentError matches err itself, never an error it wraps.
func isAgentError(err error, target **domain.AgentError) bool {
	ae, ok := err.(*domain.AgentError) //nolint:errorlint // direct type check is intentional
	if ok {
		*target = ae
		return true
	}
	return false
}
