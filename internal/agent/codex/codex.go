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

// defaultCommand is what a session launches when it is given no command.
const defaultCommand = "codex app-server"

// appServerSource: usedPercent is a percent, not a fraction; each
// window states its own length and name.
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

// Compile-time interface satisfaction check.
var _ domain.AgentAdapter = (*CodexAdapter)(nil)

// CodexAdapter satisfies [domain.AgentAdapter] by managing a persistent
// codex app-server subprocess. One adapter instance serves all
// concurrent sessions; per-session state is held in [sessionState] via
// the [domain.Session] Internal field.
type CodexAdapter struct {
	// drainGrace bounds the post-reap release goroutine's wait for the
	// connection's own reader to end normally, once StartSession copies
	// it into sessionState.drainGrace. A non-positive value resolves to
	// procutil.DefaultDrainGrace. Set by a test in this package before
	// StartSession; every production caller reaches only NewCodexAdapter,
	// which leaves it at its zero value.
	drainGrace time.Duration
}

// sessionState is adapter-internal state stored in [domain.Session]
// Internal. It tracks the persistent app-server subprocess, thread
// ID, and turn state across the session lifetime.
type sessionState struct {
	target agentcore.LaunchTarget

	// tools is closed by StopSession after the process teardown.
	tools *agentcore.ToolSession

	agentConfig domain.AgentConfig
	turnCount   int

	passthrough passthroughConfig

	threadID string

	// model is the effective LLM model reported by the thread
	// operation that started or resumed this session's thread,
	// possibly later replaced by a runtime-initiated model/rerouted
	// notification. Session-scoped: set once in StartSession before
	// the turn phase opens and never reset between turns, because the
	// app-server subprocess and its thread outlive the turn.
	model string

	// conn is the JSON-RPC connection to the app-server. It owns
	// request-id allocation, the write path, and the reader goroutine
	// that classifies and delivers every message into inbox.
	conn *jsonrpc.Conn

	// release is the shared post-reap release goroutine that gives the
	// connection's reader up to drainGrace to end on its own before
	// giving up on it. Set once in StartSession before the handshake,
	// read by RunTurn through TurnEndMessage; nil receiver semantics
	// make it safe to read from any goroutine.
	release *procutil.OutputRelease

	// stderrReported latches the first call to reportStderr: a session
	// reports the runtime's standard error at most once, however many
	// handshake or turn failure paths reach it over the session's
	// life, since every later failure against an already-dead runtime
	// has nothing new to add.
	stderrReported atomic.Bool

	// reportMu serializes the stop marker below against the decision to
	// warn, so a stop cannot land between reportStderr checking it and
	// the warning reaching the operator. It is never held across the
	// drain wait, which would make a stop pay for it.
	reportMu sync.Mutex

	// stopping reports that StopSession has begun tearing the session
	// down. The stop closes the connection, which closes the inbox and
	// sends an in-flight turn down the same path a runtime that died
	// takes, so without this the operator would be warned about a
	// runtime they stopped themselves. Set before anything is closed,
	// read by reportStderr.
	stopping atomic.Bool

	// acc holds the session's run-cumulative token usage. Constructed
	// once in StartSession and never reset between turns.
	acc *agentcore.RunUsage

	// ledger converts the thread-cumulative totals the app-server reports
	// and settles each turn. Built in StartSession.
	ledger *usage.Ledger

	// rollout locates the thread's rollout file under the runtime's home;
	// nil when the thread operation reported no usable path.
	rollout *agentcore.RuntimePath

	// turnReading is the turn's last in-turn reading, reset when RunTurn
	// starts.
	turnReading *usage.SpendReading

	// windows holds the usage windows the runtime reported, merged by the
	// usage component and kept across turns because a window is announced
	// before the turn it refuses ends. Touched only by the goroutine
	// running RunTurn.
	windows []domain.UsageWindow

	// usageMeasured reports whether the run has counted a request or
	// settled a figure. Monotone: set true once and never cleared.
	usageMeasured bool

	// mu guards proc, waitCh, stdin, pipes, and stderrCollector for
	// concurrent access from StopSession, the process-exit watcher, and
	// the stderr reporting the handshake and turn failure paths reach
	// through reportStderr. It guards no write to the peer; conn owns
	// its own outgoing queue and writer goroutine.
	mu              sync.Mutex
	group           *procutil.Group
	waitCh          <-chan struct{}
	stdin           io.WriteCloser
	pipes           *procutil.OwnedPipes
	stderrCollector *procutil.StderrCollector

	// drainGrace bounds the post-reap release goroutine's wait for the
	// connection's own reader to end normally after the subprocess has
	// been reaped. Copied from CodexAdapter.drainGrace in StartSession,
	// resolving a non-positive value to procutil.DefaultDrainGrace.
	drainGrace time.Duration

	// inbox is where conn's reader delivers every routed message. The
	// handshake wait loops, then RunTurn, are its only takers.
	// readerDone is closed by the termination watcher once conn's
	// reader has exited and inbox has been closed.
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

// closeConn closes state.conn when it is non-nil, tolerating a
// session that never reached construction. jsonrpc.Conn.Close is
// idempotent, so callers do not need to guard against calling this
// more than once.
func (state *sessionState) closeConn() {
	if state.conn != nil {
		state.conn.Close()
	}
}

// reportStderr re-emits what the runtime wrote to standard error at
// WARN, the surface the other local-subprocess adapter kinds use for
// the same diagnostics, so a failed session reports what the runtime
// said rather than an exit code alone. Called on the paths where the
// runtime is gone: every handshake failure, and a turn that ends
// because the output stream did.
//
// The session's release goroutine already runs
// [procutil.StderrCollector.FinishAndCollect] once, anchored on the
// subprocess having been reaped, concurrently with its own wait for the
// output stream to end; a call reaching here after that has resolved
// costs nothing. A call that gets here first pays the bound itself, so
// reportStderr is safe to call from a path release has not yet reached.
// stderrReported latches the emission itself, so a session with more
// than one failure path reporting against the same dead runtime warns
// the operator once rather than once per path.
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

	// Re-check under the lock the stop also takes. The wait above can
	// last the whole bound, and a stop that arrived inside it must not
	// find the warning already on its way out.
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

// readerEnded reports whether the connection's reader has already
// exited, which means the runtime's output stream is closed and its
// standard-error drain will end on its own.
//
// Both signals are polled, and conn.Done() first: a call released by the
// reader's exit returns as soon as that channel closes, while
// watchTermination closes readerDone just after, so reading readerDone
// alone would miss the runtime in exactly the window a turn/start call
// fails in. Neither is ever waited on, so a turn that failed while the
// runtime is still alive does not pay the drain bound to find that out.
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

// watchTermination closes state.inbox once state.conn's reader
// goroutine has exited, then closes state.readerDone. It is the
// adapter's own signal, distinct from conn itself, to the handshake
// wait loops and to RunTurn that no further message will arrive.
func watchTermination(state *sessionState) {
	<-state.conn.Done()
	state.inbox.Close()
	close(state.readerDone)
}

// identity returns msg unchanged. It is the wrap state.conn delivers
// through, since this adapter carries jsonrpc.Message values through
// the inbox with no adaptation.
func identity(msg jsonrpc.Message) jsonrpc.Message { return msg }

// drainHandshakeMessages processes every message the handshake left
// queued in state.inbox, the same way handleOutOfTurnMessage processes
// one a wait loop observes while waiting for something else, so the
// first turn starts with nothing already queued and nothing observed
// before it is silently lost. state.threadID is already set by the
// time this runs, so it reports through the same session-scoped logger
// RunTurn uses.
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

// handleOutOfTurnMessage answers or reports one message that arrived
// outside a turn: while a handshake wait loop is waiting for something
// else, or queued in the interval between the handshake finishing and
// the first turn starting. A server-initiated request this adapter
// recognizes by method is answered the same way the turn loop answers
// it; any other request gets method-not-found, so a request still gets
// exactly one reply regardless of when it arrives. An
// mcpServer/startupStatus/updated failure is reported, through logger,
// the same way the turn loop reports it. A thread/tokenUsage/updated
// reaches the ledger, which takes the figure the app-server repeats when
// a thread resumes as the run's starting point. Every other notification
// is consumed without report: none of the session-level notifications
// observed before a turn exists names anything actionable, and nothing
// handled here is replayed into the first turn as one of its events.
func handleOutOfTurnMessage(state *sessionState, msg jsonrpc.Message, logger *slog.Logger) {
	switch msg.Kind {
	case jsonrpc.KindRequest:
		// A null id is present on the wire but names no request to
		// answer; the app-server sends none, and the matching guard in
		// RunTurn treats it the same way.
		if !msg.ID.Present() || msg.ID.IsNull() {
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

// answerElicitationRequest answers a mcpServer/elicitation/request the
// same way regardless of when it arrives: declined, since sortie runs
// unattended and no person is present to supply the requested input.
func answerElicitationRequest(state *sessionState, requestID jsonrpc.ID) {
	state.conn.Respond(requestID, map[string]any{"action": "decline"}) //nolint:errcheck,gosec // best-effort refusal
}

// answerUnrecognizedRequest answers a server-initiated request this
// adapter does not implement, in whichever phase it arrives, with the
// shared method-not-found reply.
func answerUnrecognizedRequest(state *sessionState, requestID jsonrpc.ID) {
	state.conn.RespondError(requestID, jsonrpc.MethodNotFoundCode, jsonrpc.MethodNotFoundMessage) //nolint:errcheck,gosec // best-effort
}

// reportMCPStartupFailure logs, at Warn, an
// mcpServer/startupStatus/updated notification reporting a server that
// failed to start, in whichever phase it arrives.
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

// NewCodexAdapter creates a [CodexAdapter]. Settings and command
// resolution are deferred to [CodexAdapter.StartSession].
func NewCodexAdapter() (domain.AgentAdapter, error) {
	return &CodexAdapter{}, nil
}

// StartSession parses the session's settings, validates the workspace path,
// resolves the codex binary, launches the app-server subprocess, performs
// the initialization handshake, authenticates if needed, and starts or
// resumes a thread.
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

	// killOnError is a cleanup closure used if any handshake step fails.
	killOnError := func() {
		state.mu.Lock()
		procutil.CloseWithoutWaiting(state.stdin)
		if state.pipes != nil {
			state.pipes.CloseStdout() //nolint:errcheck,gosec // unblock the reader goroutine on the read end
		}
		state.mu.Unlock()
		group.Kill() //nolint:errcheck,gosec // best-effort cleanup
		// Wait briefly for cleanup.
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

	// Create the inbox and readerDone before the connection, so the
	// sink delivering into state.inbox below has every resource it
	// touches ready before the reader goroutine can call it.
	state.inbox = jsonrpc.NewInbox[jsonrpc.Message]()
	state.readerDone = make(chan struct{})

	state.conn = jsonrpc.NewConn(prefixedStdin, pipes.Stdout, jsonrpc.Deliver(state.inbox, identity))
	// Started before the handshake so the handshake wait loops observe
	// a closed inbox, rather than timing out, when stdout ends mid-handshake.
	go watchTermination(state)
	// The release ends a handshake call or a turn that would otherwise
	// wait forever on a reaped runtime whose reader did not end inside
	// the drain bound. It captures its own copies of pipes and the
	// connection's done channel because killOnError clears state.pipes
	// under state.mu on every handshake failure path below.
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

	var threadID, model, path string
	resumed := false
	if params.ResumeSessionID != "" {
		resumedModel, resumedPath, resumeErr := resumeThread(ctx, state, params.ResumeSessionID)
		if resumeErr != nil {
			// Fallback to new thread on resume failure.
			logger.Warn("thread resume failed, starting new thread",
				slog.String("resume_id", params.ResumeSessionID),
				slog.Any("error", resumeErr))
			tid, startedModel, startedPath, startErr := startThread(ctx, state, pt, logger)
			if startErr != nil {
				observation := earlyExitObservation(ctx, startErr, target, state)
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
					Message: fmt.Sprintf("thread/start failed: %v", startErr),
					Err:     startErr,
				}
			}
			threadID = tid
			model = startedModel
			path = startedPath
		} else {
			threadID = params.ResumeSessionID
			model = resumedModel
			path = resumedPath
			resumed = true
		}
	} else {
		tid, startedModel, startedPath, startErr := startThread(ctx, state, pt, logger)
		if startErr != nil {
			observation := earlyExitObservation(ctx, startErr, target, state)
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
				Message: fmt.Sprintf("thread/start failed: %v", startErr),
				Err:     startErr,
			}
		}
		threadID = tid
		model = startedModel
		path = startedPath
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
	if resumed {
		readCtx, cancel := context.WithTimeout(ctx, agentcore.ReadTimeout(params.AgentConfig))
		ledger.Boundary(state.readRollout(readCtx, logger, true))
		cancel()
	} else {
		ledger.SessionCreated()
	}
	drainHandshakeMessages(state, logger)

	started = true
	return domain.Session{
		ID:            threadID,
		AgentPID:      strconv.Itoa(cmd.Process.Pid),
		Internal:      state,
		NoToolChannel: tools.NoToolChannel(),
	}, nil
}

// codexRefusalErrorCode and codexRefusalMessage are pinned rather than
// derived: the app-server protocol schema declares no enumerated value
// for either the error code an implementation-defined JSON-RPC refusal
// may use or the free-text field the legacy ReviewDecision denial
// requires, and the app-server acknowledges no client response, so
// neither can be confirmed accepted at runtime. codexRefusalErrorCode
// is written as the JSON-RPC error code on a server request whose
// response schema offers no result shape that could express a
// decline. codexRefusalMessage is written both as that error's message
// and as the legacy ReviewDecision denial's rejection text.
const (
	codexRefusalErrorCode = -32001
	codexRefusalMessage   = "sortie refuses requests that only a person could answer"
)

// detailAnswerToQuestion and detailWiderAccess name, for the operator,
// the specific ask behind a recognized request for human input,
// appended to agentcore.RefusalPosture's notice stem.
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

	// Build turn/start params.
	turnParams := map[string]any{
		"threadId": state.threadID,
		"input":    []map[string]any{{"type": "text", "text": params.Prompt}},
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

	resp, err := state.conn.Call(ctx, "turn/start", turnParams)
	if err != nil {
		// A call that failed because the runtime is gone has the
		// runtime's own diagnostic waiting in the collector; one that
		// failed with the runtime still alive has nothing to report and
		// must not wait for a drain that cannot finish.
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
	// cancelDeadline stays nil, and so never selectable, until the
	// ctxDone arm below arms it with a one-shot timer bounding how long
	// the loop waits for the runtime's own turn/completed after the
	// best-effort interrupt.
	var cancelDeadline <-chan time.Time

	for {
		select {
		case <-ctxDone:
			// A cancelled context's Done channel stays ready forever. Disable
			// this arm before sending the one-shot interrupt so later selects
			// wait for a terminal message, stdout to close, or the deadline
			// armed below.
			ctxDone = nil
			// Best-effort write of turn/interrupt on the already-cancelled turn.
			state.conn.SendRequest("turn/interrupt", map[string]any{ //nolint:errcheck,gosec // best-effort interrupt
				"threadId": state.threadID,
				"turnId":   turnID,
			})
			// Bound how long the loop keeps reading for the runtime's own
			// turn/completed: past this deadline the interrupt is presumed
			// lost, and the loop returns rather than reading until stdout
			// closes.
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
				// Inbox closed, subprocess stdout ended.
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
				// Only a stream end means the runtime is gone. The read
				// loop dispatches a malformed line and keeps reading, so
				// reporting here would wait the drain bound on a pipe a
				// live runtime still holds, and latch the report away
				// from the turn that really loses the runtime later.
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

			// A server-initiated request and a notification both carry
			// a method and dispatch below; an unmatched response
			// (echoed tool-call confirmation) does not.
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
					// No token-usage object on the wire: the runtime
					// reported nothing for this notification, so no
					// event is emitted and the measurement flag is
					// left untouched.
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
				// A status word is only worth reporting when the payload
				// carried one. A malformed payload, or one that omits the
				// status member, leaves it empty and keeps this message
				// unset so agentcore.DecideTurn's own fallback applies
				// instead of one built from an empty string.
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
				// A null id is present on the wire, but this adapter has
				// always treated it as no id at all, and the app-server
				// does not send one. Routing it here keeps that behavior
				// rather than changing what this adapter answers as a
				// side effect of the shared framing learning the form.
				if !requestID.Present() || requestID.IsNull() {
					logger.Debug("codex request without id ignored", slog.String("method", method))
					agentcore.EmitNotification(emit, "")
					break
				}

				// An orchestrator-initiated cancellation outranks any
				// classification of a request this turn recognizes, matching
				// the adapter's existing treatment of an interrupted turn.
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
				if msg.Kind == jsonrpc.KindRequest && msg.ID.Present() && !msg.ID.IsNull() {
					answerUnrecognizedRequest(state, msg.ID)
				}
				logger.Debug("codex message logged only", slog.String("method", method))
				agentcore.EmitNotification(emit, "")
			}
		}
	}
}

// cancelledMessage composes the terminal message for a turn/completed
// notification that arrives after the orchestrator has already cancelled
// the turn. It names the runtime's reported status and, when the payload
// carried one, appends the runtime's own error text.
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

// reroutedMessage composes the notification message for a
// model/rerouted notification, naming the model the turn moved to. An
// empty toModel, meaning the runtime rerouted the turn without naming
// a destination, is named as such rather than left blank.
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

	// Before anything is closed: closing the connection closes the
	// inbox, and a turn still running reads that as its runtime having
	// died. A session the operator stopped has nothing to explain.
	state.reportMu.Lock()
	state.stopping.Store(true)
	state.reportMu.Unlock()

	// Close the connection before closing stdin: this stops delivery and
	// fails any call still in flight first.
	state.closeConn()

	// Close stdin to signal EOF to the app-server. Closing it must not be
	// able to hold up the signal, the wait, and the kill that follow.
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

	// Wait for process exit within the configured grace period, or
	// until the caller's deadline ends it first.
	var stopErr error
	if waitCh != nil {
		grace := procutil.StopGrace(state.agentConfig.StopGraceMS)
		started := time.Now()
		escalate := func(outcome string) {
			logger.Warn("agent did not exit inside the graceful period and was force-terminated",
				slog.String("outcome", outcome), slog.Duration("grace", grace),
				slog.Duration("elapsed", time.Since(started)))
			group.Kill() //nolint:errcheck,gosec // best-effort force kill
			// Wait again briefly for cleanup.
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

	// Wait for the reader goroutine to finish after process exit.
	if state.readerDone != nil {
		select {
		case <-state.readerDone:
		case <-time.After(2 * time.Second):
			logger.Warn("reader goroutine did not exit after process termination")
		}
	}

	// Closing pipes below ends the standard-error drain: closing the
	// read end while the collector's scanner is mid-read makes it
	// return a read error, which the drain treats the same as end of
	// file. A session the operator stopped does not report the runtime's
	// standard error: the turn and handshake paths above already
	// reported a runtime that failed, and there is nothing to explain
	// for one that stopped on request.
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

	// Closing the read end unparks the drain but does not wait for it.
	// Joining it here is what makes "no collector goroutine outlives the
	// session" a guarantee rather than a race the caller usually wins.
	// The lines are discarded: reporting is the failure paths' job, and
	// stopping is not a failure.
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

// isAgentError extracts an *[domain.AgentError] from err using type
// assertion.
func isAgentError(err error, target **domain.AgentError) bool {
	ae, ok := err.(*domain.AgentError) //nolint:errorlint // direct type check is intentional
	if ok {
		*target = ae
		return true
	}
	return false
}
