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
	"github.com/sortie-ai/sortie/internal/agent/mcpconfig"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

func init() {
	registry.Agents.RegisterWithMeta("codex", NewCodexAdapter, registry.AgentMeta{
		RequiresCommand:     true,
		ValidateAgentConfig: validateConfig,
		MCPInjection:        registry.MCPInjectionTranslated,
		UsageArrival:        registry.UsageArrivalIncremental,
		UsageAttribution:    registry.UsageAttributionPerModel,
	})
}

// Compile-time interface satisfaction check.
var _ domain.AgentAdapter = (*CodexAdapter)(nil)

// CodexAdapter satisfies [domain.AgentAdapter] by managing a persistent
// codex app-server subprocess. One adapter instance serves all
// concurrent sessions; per-session state is held in [sessionState] via
// the [domain.Session] Internal field.
type CodexAdapter struct {
	passthrough passthroughConfig

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
	target      agentcore.LaunchTarget
	agentConfig domain.AgentConfig
	turnCount   int

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
	// that classifies and routes every message.
	conn *jsonrpc.Conn

	// turnPhase reports whether the session has moved past the
	// handshake. It is read only by the reader goroutine, inside the
	// handler bound to this state, and written once by
	// beginTurnPhase, so it is an atomic.Bool rather than
	// mutex-guarded.
	turnPhase atomic.Bool

	// outputAbandoned reports that the release path gave up on the
	// connection's reader: the runtime had been reaped and the reader
	// had not ended when the drain bound expired. Written once by the
	// release goroutine before it closes anything, read by RunTurn
	// when the connection's end decides a turn, so it is an
	// atomic.Bool rather than mutex-guarded.
	outputAbandoned atomic.Bool

	// stderrReported latches the first call to reportStderr: a session
	// reports the runtime's standard error at most once, however many
	// handshake or turn failure paths reach it over the session's
	// life, since every later failure against an already-dead runtime
	// has nothing new to add.
	stderrReported atomic.Bool

	// acc holds the session's run-cumulative token usage. Constructed
	// once in StartSession and never reset between turns.
	acc *agentcore.RunUsage

	// baseline is the thread-cumulative total to subtract from a
	// matching thread/tokenUsage/updated notification's total to
	// recover this run's own contribution. Per run; never reset
	// between turns. Resolved on the first notification whose turnId
	// matches the run's current turn.
	baseline    domain.TokenUsage
	baselineSet bool

	// usageMeasured reports whether at least one
	// thread/tokenUsage/updated notification carrying a token-usage
	// object has been processed for this run. Monotone: set true once
	// and never cleared.
	usageMeasured bool

	// mu guards proc, waitCh, stdin, pipes, and stderrCollector for
	// concurrent access from StopSession, the process-exit watcher, and
	// the stderr reporting the handshake and turn failure paths reach
	// through reportStderr. It guards no write to the peer; conn owns
	// its own write mutex.
	mu              sync.Mutex
	proc            *os.Process
	waitCh          <-chan struct{}
	stdin           io.WriteCloser
	pipes           *procutil.OwnedPipes
	stderrCollector *procutil.StderrCollector

	// drainGrace bounds the post-reap release goroutine's wait for the
	// connection's own reader to end normally after the subprocess has
	// been reaped. Copied from CodexAdapter.drainGrace in StartSession,
	// resolving a non-positive value to procutil.DefaultDrainGrace.
	drainGrace time.Duration

	// Session-scoped delivery channel. The handler bound to this
	// state, invoked on conn's reader goroutine, delivers every
	// routed message to msgCh, which the handshake wait loops and
	// RunTurn read. stopCh is closed by StopSession to unblock the
	// handler if msgCh is full during the turn phase. readerDone is
	// closed by the termination watcher once conn's reader has
	// exited and msgCh has been closed. closeStop guards against
	// double-closing stopCh when StopSession is called more than
	// once.
	msgCh      chan jsonrpc.Message
	readerDone chan struct{}
	stopCh     chan struct{}
	closeStop  sync.Once
}

// closeConnAndStop closes stopCh and conn together, inside the
// sync.Once that guards stopCh, so a session torn down from more than
// one failure path neither double-closes a channel nor races between
// the two teardown paths. It tolerates state.conn and state.stopCh
// being nil, which a session that never reached construction leaves
// unset.
func (state *sessionState) closeConnAndStop() {
	state.closeStop.Do(func() {
		if state.stopCh != nil {
			close(state.stopCh)
		}
		if state.conn != nil {
			state.conn.Close()
		}
	})
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

// sessionHandler returns the [jsonrpc.Handler] bound to state. Before
// the turn phase begins, it performs a non-blocking send and drops
// the message on a full msgCh, since nothing drains msgCh while a
// handshake wait loop is itself the one reading it directly. Once
// beginTurnPhase runs, it becomes the same two-arm blocking send
// RunTurn's caller depends on for back-pressure and shutdown.
func sessionHandler(state *sessionState) jsonrpc.Handler {
	return func(msg jsonrpc.Message) {
		if !state.turnPhase.Load() {
			select {
			case state.msgCh <- msg:
			default:
			}
			return
		}
		select {
		case state.msgCh <- msg:
		case <-state.stopCh:
		}
	}
}

// watchTermination closes state.msgCh once state.conn's reader
// goroutine has exited, then closes state.readerDone. It is the
// adapter's own signal, distinct from conn itself, to the handshake
// wait loops and to RunTurn that no further message will arrive.
func watchTermination(state *sessionState) {
	<-state.conn.Done()
	close(state.msgCh)
	close(state.readerDone)
}

// outputAbandonedMessage is a turn's terminal message when the release
// path gave up on the connection's reader, and nothing else: the WARN
// record it accompanies keeps its own text. It states what the adapter
// observed rather than why, because the give-up arm cannot tell a
// reader parked in a read from one parked in the handler apart, and it
// names no channel, field, method, or file.
const outputAbandonedMessage = "the agent runtime exited before the session finished collecting its output"

// turnEndMessage returns outputAbandonedMessage when the release path
// gave up on state's connection, and fallback otherwise.
func turnEndMessage(state *sessionState, fallback string) string {
	if state.outputAbandoned.Load() {
		return outputAbandonedMessage
	}
	return fallback
}

// release starts a goroutine that waits for the subprocess to be
// reaped, then gives the connection's own reader up to grace to end on
// its own before giving up on it. Without it, a runtime that dies with
// its output handle still open, or with the session's message channel
// full, leaves every handshake call and turn waiting on a reader that
// cannot end. Every value it touches is captured at construction
// rather than read from session state, so a failure path that clears
// state.pipes under state.mu cannot race it.
//
// The give-up arm frees both ways a reader can be stuck: closing the
// standard-output read end releases one parked in a read, and closing
// the stop channel releases one parked inside the handler on a full
// message channel. It does not reach a runtime that is still alive:
// that reader is freed only once the reaper fires.
//
// release also anchors the session's one standard-error bound: once
// the subprocess is reaped, a second goroutine runs
// [procutil.StderrCollector.FinishAndCollect] concurrently with the
// standard-output wait above, both bounded by the same grace, so a
// reportStderr call reaching either path afterward finds the drain
// already finished or abandoned rather than paying grace again.
func release(state *sessionState, pipes *procutil.OwnedPipes, collector *procutil.StderrCollector, grace time.Duration, connDone, reaperDone <-chan struct{}, logger *slog.Logger) {
	go func() {
		<-reaperDone

		go collector.FinishAndCollect(grace)

		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case <-connDone:
		case <-timer.C:
			state.outputAbandoned.Store(true)
			pipes.CloseStdout() //nolint:errcheck,gosec // best-effort; releases a reader parked on a dead runtime's descendant
			state.closeConnAndStop()
			logger.Warn("agent stdout was not fully collected before the session ended",
				slog.Duration("drain_bound", grace))
		}
	}()
}

// beginTurnPhase discards whatever the handshake-phase handler
// buffered into msgCh and moves the session into the turn phase, so
// the first turn starts with nothing already queued, exactly as it
// does before the handshake completes today.
func beginTurnPhase(state *sessionState) {
	for {
		select {
		case _, ok := <-state.msgCh:
			if !ok {
				state.turnPhase.Store(true)
				return
			}
		default:
			state.turnPhase.Store(true)
			return
		}
	}
}

// NewCodexAdapter creates a [CodexAdapter] from adapter configuration.
// The config parameter is the raw map from the "codex" sub-object in
// WORKFLOW.md. Command resolution is deferred to
// [CodexAdapter.StartSession].
func NewCodexAdapter(config map[string]any) (domain.AgentAdapter, error) {
	pt, fault := parsePassthroughConfig(config)
	if fault != nil {
		return nil, fault
	}
	adapter := &CodexAdapter{passthrough: pt}

	return adapter, nil
}

// StartSession validates the workspace path, resolves the codex binary,
// launches the app-server subprocess, performs the initialization handshake,
// authenticates if needed, and starts or resumes a thread.
func (a *CodexAdapter) StartSession(ctx context.Context, params domain.StartSessionParams) (domain.Session, error) {
	target, agentErr := agentcore.ResolveLaunchTarget(params, "codex app-server")
	if agentErr != nil {
		return domain.Session{}, agentErr
	}

	if params.MCPConfigPath != "" && target.RemoteCommand == "" {
		servers, parseErr := mcpconfig.Parse(params.MCPConfigPath)
		if parseErr != nil {
			return domain.Session{}, &domain.AgentError{
				Kind:    domain.ErrResponseError,
				Message: fmt.Sprintf("parse MCP config: %v", parseErr),
				Err:     parseErr,
			}
		}
		overrideArgs, renderErr := renderMCPServerOverrides(servers, os.Environ())
		if renderErr != nil {
			return domain.Session{}, &domain.AgentError{
				Kind:    domain.ErrResponseError,
				Message: fmt.Sprintf("render MCP server overrides: %v", renderErr),
				Err:     renderErr,
			}
		}
		target.Args = append(target.Args, overrideArgs...)
	}

	state := &sessionState{
		target:      target,
		agentConfig: params.AgentConfig,
		acc:         agentcore.NewRunUsage(),
	}

	var cmd *exec.Cmd
	if target.RemoteCommand != "" {
		remoteCmd := buildSSHRemoteCmd(target.RemoteCommand, os.Getenv("CODEX_API_KEY"))
		sshArgs := sshutil.BuildSSHArgs(target.SSHHost, target.WorkspacePath, remoteCmd, nil, sshutil.SSHOptions{
			StrictHostKeyChecking: target.SSHStrictHostKeyChecking,
		})
		cmd = exec.CommandContext(ctx, target.Command, sshArgs...) //nolint:gosec // args are constructed programmatically with shell quoting
	} else {
		cmd = exec.CommandContext(ctx, target.Command, target.Args...) //nolint:gosec // args are constructed programmatically
	}
	grace := procutil.StopGrace(state.agentConfig.StopGraceMS)
	procutil.SetGroupCancel(cmd, grace)
	cmd.Dir = target.WorkspacePath
	cmd.Env = os.Environ()

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return domain.Session{}, &domain.AgentError{
			Kind:    domain.ErrPortExit,
			Message: "failed to create stdin pipe",
			Err:     err,
		}
	}

	pipes, err := procutil.StartWithOwnedPipes(cmd)
	if err != nil {
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
		// these close it instead. The process-start stage needs none.
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
		default: // procutil.StageProcessStart
			return domain.Session{}, &domain.AgentError{
				Kind:    domain.ErrPortExit,
				Message: "failed to start app-server subprocess",
				Err:     startErr.Err,
			}
		}
	}

	logger := slog.Default().With(slog.String("component", "codex-adapter"))
	if assignErr := procutil.AssignProcess(cmd.Process.Pid, cmd.Process); assignErr != nil {
		logger.Warn("process group assignment failed", slog.Any("error", assignErr))
	}

	state.proc = cmd.Process
	state.stdin = stdinPipe
	state.pipes = pipes
	state.drainGrace = a.drainGrace
	if state.drainGrace <= 0 {
		state.drainGrace = procutil.DefaultDrainGrace
	}
	state.stderrCollector = procutil.NewStderrCollector(pipes.Stderr, logger)

	reaper := procutil.StartReaper(cmd)
	state.waitCh = reaper.Done()

	// killOnError is a cleanup closure used if any handshake step fails.
	killOnError := func() {
		state.mu.Lock()
		if state.stdin != nil {
			state.stdin.Close() //nolint:errcheck,gosec // best-effort cleanup
		}
		if state.pipes != nil {
			state.pipes.CloseStdout() //nolint:errcheck,gosec // unblock the reader goroutine on the read end
		}
		state.mu.Unlock()
		procutil.KillProcessGroup(cmd.Process.Pid) //nolint:errcheck,gosec // best-effort cleanup
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
		state.proc = nil
		state.stdin = nil
		if state.pipes != nil {
			state.pipes.Close() //nolint:errcheck,gosec // best-effort cleanup; StartSession's failure paths return no session to close these later
		}
		state.pipes = nil
		state.mu.Unlock()
	}

	// Create stopCh, msgCh, and readerDone before the connection, so
	// the handler (bound to state below) has every resource it
	// touches ready before the reader goroutine can call it.
	state.stopCh = make(chan struct{})
	state.msgCh = make(chan jsonrpc.Message, 16)
	state.readerDone = make(chan struct{})

	state.conn = jsonrpc.NewConn(stdinPipe, pipes.Stdout, sessionHandler(state))
	// Started before the handshake so the handshake wait loops observe
	// a closed msgCh, rather than timing out, when stdout ends mid-handshake.
	go watchTermination(state)
	// release ends a handshake call or a turn that would otherwise wait
	// forever on a reaped runtime whose reader did not end inside the
	// drain bound. It captures its own copies of pipes and the
	// connection's done channel because killOnError clears state.pipes
	// under state.mu on every handshake failure path below.
	release(state, pipes, state.stderrCollector, state.drainGrace, state.conn.Done(), reaper.Done(), logger)

	if err := initializeHandshake(ctx, state); err != nil {
		state.closeConnAndStop()
		killOnError()
		return domain.Session{}, &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: fmt.Sprintf("handshake failed: %v", err),
			Err:     err,
		}
	}

	if err := authenticateIfNeeded(ctx, state); err != nil {
		state.closeConnAndStop()
		killOnError()
		var agentErr *domain.AgentError
		if ok := isAgentError(err, &agentErr); ok {
			return domain.Session{}, agentErr
		}
		return domain.Session{}, &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: fmt.Sprintf("authentication failed: %v", err),
			Err:     err,
		}
	}

	var threadID, model string
	if params.ResumeSessionID != "" {
		resumedModel, resumeErr := resumeThread(ctx, state, params.ResumeSessionID)
		if resumeErr != nil {
			// Fallback to new thread on resume failure.
			logger.Warn("thread resume failed, starting new thread",
				slog.String("resume_id", params.ResumeSessionID),
				slog.Any("error", resumeErr))
			tid, startedModel, startErr := startThread(ctx, state, a.passthrough)
			if startErr != nil {
				state.closeConnAndStop()
				killOnError()
				return domain.Session{}, &domain.AgentError{
					Kind:    domain.ErrResponseError,
					Message: fmt.Sprintf("thread/start failed: %v", startErr),
					Err:     startErr,
				}
			}
			threadID = tid
			model = startedModel
		} else {
			threadID = params.ResumeSessionID
			model = resumedModel
		}
	} else {
		tid, startedModel, startErr := startThread(ctx, state, a.passthrough)
		if startErr != nil {
			state.closeConnAndStop()
			killOnError()
			return domain.Session{}, &domain.AgentError{
				Kind:    domain.ErrResponseError,
				Message: fmt.Sprintf("thread/start failed: %v", startErr),
				Err:     startErr,
			}
		}
		threadID = tid
		model = startedModel
	}

	state.threadID = threadID
	state.model = model
	beginTurnPhase(state)

	return domain.Session{
		ID:       threadID,
		AgentPID: strconv.Itoa(cmd.Process.Pid),
		Internal: state,
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

	state.turnCount++

	// Build turn/start params.
	turnParams := map[string]any{
		"threadId": state.threadID,
		"input":    []map[string]any{{"type": "text", "text": params.Prompt}},
		"cwd":      state.target.WorkspacePath,
	}

	if state.turnCount == 1 || a.passthrough.TurnSandboxPolicy != nil {
		turnParams["sandboxPolicy"] = buildSandboxPolicy(state, a.passthrough)
	}
	if a.passthrough.Model != "" {
		turnParams["model"] = a.passthrough.Model
	}
	if a.passthrough.Effort != "" {
		turnParams["effort"] = a.passthrough.Effort
	}

	resp, err := state.conn.Call(ctx, "turn/start", turnParams)
	if err != nil {
		// A call that failed because the runtime is gone has the
		// runtime's own diagnostic waiting in the collector; one that
		// failed with the runtime still alive has nothing to report and
		// must not wait for a drain that cannot finish.
		if state.readerEnded() {
			state.reportStderr(logger)
		}
		return domain.TurnResult{UsageMeasured: state.usageMeasured}, &domain.AgentError{
			Kind:    domain.ErrPortExit,
			Message: turnEndMessage(state, fmt.Sprintf("turn/start failed: %v", err)),
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
		result, agentErr := agentcore.FinalizeTurn(params.OnEvent, logger, ev, meta)
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
			cancelDeadline = time.After(readTimeout(state))
			continue

		case <-cancelDeadline:
			ev := agentcore.TurnEvidence{Terminal: agentcore.TerminalCancelled}
			meta := agentcore.TurnMeta{
				SessionID:     state.threadID,
				Usage:         state.acc.Snapshot(),
				UsageMeasured: state.usageMeasured,
			}
			result, agentErr := agentcore.FinalizeTurn(params.OnEvent, logger, ev, meta)
			if agentErr != nil {
				return result, agentErr
			}
			return result, nil

		case msg, ok := <-state.msgCh:
			if !ok {
				// Channel closed, subprocess stdout ended.
				state.reportStderr(logger)
				ev := agentcore.TurnEvidence{
					Terminal:          agentcore.TerminalFailure,
					TerminalErrorKind: domain.ErrPortExit,
					TerminalMessage:   turnEndMessage(state, "subprocess stdout closed unexpectedly"),
				}
				meta := agentcore.TurnMeta{
					SessionID:     state.threadID,
					Usage:         state.acc.Snapshot(),
					UsageMeasured: state.usageMeasured,
				}
				result, agentErr := agentcore.FinalizeTurn(params.OnEvent, logger, ev, meta)
				if agentErr != nil {
					return result, agentErr
				}
				return result, nil
			}
			if msg.Kind == jsonrpc.KindMalformed || msg.Kind == jsonrpc.KindStreamEnd {
				state.reportStderr(logger)
				ev := agentcore.TurnEvidence{
					Terminal:          agentcore.TerminalFailure,
					TerminalErrorKind: domain.ErrPortExit,
					TerminalMessage:   turnEndMessage(state, fmt.Sprintf("stdout read error: %v", msg.Err)),
					Cause:             msg.Err,
				}
				meta := agentcore.TurnMeta{
					SessionID:     state.threadID,
					Usage:         state.acc.Snapshot(),
					UsageMeasured: state.usageMeasured,
				}
				result, agentErr := agentcore.FinalizeTurn(params.OnEvent, logger, ev, meta)
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
					agentcore.EmitSessionStarted(params.OnEvent, session.AgentPID, state.threadID)
				} else {
					agentcore.EmitNotification(params.OnEvent, "turn started")
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
				state.usageMeasured = true
				// A turn/start response that failed to unmarshal leaves
				// turnID empty; adopt the first notification's turn id
				// rather than treating every notification of this turn as
				// belonging to another turn.
				if turnID == "" {
					turnID = p.TurnID
				}
				if p.TurnID != turnID {
					state.baseline = maxUsage(state.baseline, normalizeBreakdown(p.TokenUsage.Total))
					continue
				}
				if !state.baselineSet {
					state.baseline = subtractUsage(normalizeBreakdown(p.TokenUsage.Total), normalizeBreakdown(p.TokenUsage.Last))
					state.baselineSet = true
				}
				snapshot := state.acc.SetRunCumulative(subtractUsage(normalizeBreakdown(p.TokenUsage.Total), state.baseline))
				params.OnEvent(domain.AgentEvent{
					Type:      domain.EventTokenUsage,
					Timestamp: now,
					Usage:     snapshot,
					Model:     state.model,
				})

			case "turn/completed":
				var tc turnCompletedParams
				if err := json.Unmarshal(msg.Params, &tc); err != nil {
					logger.Warn("turn/completed unmarshal failed", slog.Any("error", err))
				}

				snapshot := state.acc.Snapshot()

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
					ev.TerminalErrorKind = mapCodexErrorInfo(tc.Turn.Error.CodexErrorInfo)
					ev.TerminalMessage = tc.Turn.Error.Message
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
					Usage:         snapshot,
					UsageMeasured: state.usageMeasured,
				}

				result, agentErr := agentcore.FinalizeTurn(params.OnEvent, logger, ev, meta)
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
				switch item.Type {
				case "commandExecution", "fileChange", "mcpToolCall", "dynamicToolCall":
					toolName := cmp.Or(item.Command, item.Type)
					inFlight.Begin(item.ID, toolName)
					agentcore.EmitNotification(params.OnEvent, summarizeItem(item.Type, item.ID))
				default:
					agentcore.EmitNotification(params.OnEvent, summarizeItem(item.Type, item.ID))
				}

			case "item/completed":
				var ip itemParams
				if err := json.Unmarshal(msg.Params, &ip); err != nil {
					logger.Debug("item/completed unmarshal failed", slog.Any("error", err))
					continue
				}
				item := ip.Item
				if toolName, durationMS, ok := inFlight.End(item.ID); ok {
					params.OnEvent(domain.AgentEvent{
						Type:           domain.EventToolResult,
						Timestamp:      now,
						ToolName:       toolName,
						ToolDurationMS: durationMS,
					})
				}
				if item.Type == "agentMessage" && item.Text != "" {
					agentcore.EmitNotification(params.OnEvent, typeutil.TruncateRunes(item.Text, 200))
				}

			case "item/agentMessage/delta", "item/commandExecution/outputDelta":
				agentcore.EmitNotification(params.OnEvent, "")

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
					params.OnEvent(domain.AgentEvent{
						Type:      domain.EventOtherMessage,
						Timestamp: now,
						Message:   method,
					})
					break
				}

				// An orchestrator-initiated cancellation outranks any
				// classification of a request this turn recognizes, matching
				// the adapter's existing treatment of an interrupted turn.
				if ctx.Err() != nil {
					ev := agentcore.TurnEvidence{Terminal: agentcore.TerminalCancelled}
					meta := agentcore.TurnMeta{
						SessionID:     state.threadID,
						Usage:         state.acc.Snapshot(),
						UsageMeasured: state.usageMeasured,
					}
					result, agentErr := agentcore.FinalizeTurn(params.OnEvent, logger, ev, meta)
					return result, agentErr
				}

				switch method {
				case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
					posture := agentcore.DecideHumanRequest(agentcore.ClassPermission, true, agentcore.AnswerPending)
					state.conn.Respond(requestID, map[string]any{"decision": "decline"}) //nolint:errcheck,gosec // best-effort refusal
					agentcore.EmitNotification(params.OnEvent, posture.NoticeWithDetail(""))

				case "applyPatchApproval", "execCommandApproval":
					posture := agentcore.DecideHumanRequest(agentcore.ClassPermission, true, agentcore.AnswerPending)
					state.conn.Respond(requestID, map[string]any{ //nolint:errcheck,gosec // best-effort refusal
						"decision": map[string]any{
							"denied": map[string]any{"rejection": codexRefusalMessage},
						},
					})
					agentcore.EmitNotification(params.OnEvent, posture.NoticeWithDetail(""))

				case "mcpServer/elicitation/request":
					posture := agentcore.DecideHumanRequest(agentcore.ClassHumanInput, true, agentcore.AnswerPending)
					state.conn.Respond(requestID, map[string]any{"action": "decline"}) //nolint:errcheck,gosec // best-effort refusal
					agentcore.EmitNotification(params.OnEvent, posture.NoticeWithDetail(detailAnswerToQuestion))

					meta := agentcore.TurnMeta{
						SessionID:     state.threadID,
						Usage:         state.acc.Snapshot(),
						UsageMeasured: state.usageMeasured,
					}
					evidence := agentcore.HumanInputEvidence(detailAnswerToQuestion)
					result, agentErr := agentcore.FinalizeTurn(params.OnEvent, logger, evidence, meta)
					return result, agentErr

				case "item/permissions/requestApproval":
					posture := agentcore.DecideHumanRequest(agentcore.ClassHumanInput, true, agentcore.AnswerPending)
					state.conn.RespondError(requestID, codexRefusalErrorCode, codexRefusalMessage) //nolint:errcheck,gosec // best-effort refusal
					agentcore.EmitNotification(params.OnEvent, posture.NoticeWithDetail(detailWiderAccess))

					meta := agentcore.TurnMeta{
						SessionID:     state.threadID,
						Usage:         state.acc.Snapshot(),
						UsageMeasured: state.usageMeasured,
					}
					evidence := agentcore.HumanInputEvidence(detailWiderAccess)
					result, agentErr := agentcore.FinalizeTurn(params.OnEvent, logger, evidence, meta)
					return result, agentErr

				case "item/tool/requestUserInput":
					posture := agentcore.DecideHumanRequest(agentcore.ClassHumanInput, true, agentcore.AnswerPending)
					state.conn.RespondError(requestID, codexRefusalErrorCode, codexRefusalMessage) //nolint:errcheck,gosec // best-effort refusal
					agentcore.EmitNotification(params.OnEvent, posture.NoticeWithDetail(detailAnswerToQuestion))

					meta := agentcore.TurnMeta{
						SessionID:     state.threadID,
						Usage:         state.acc.Snapshot(),
						UsageMeasured: state.usageMeasured,
					}
					evidence := agentcore.HumanInputEvidence(detailAnswerToQuestion)
					result, agentErr := agentcore.FinalizeTurn(params.OnEvent, logger, evidence, meta)
					return result, agentErr
				}

			case "turn/plan/updated":
				agentcore.EmitNotification(params.OnEvent, "plan updated")

			case "turn/diff/updated":
				logger.Debug("diff updated")

			case "mcpServer/startupStatus/updated":
				var su mcpServerStartupStatus
				if err := json.Unmarshal(msg.Params, &su); err != nil {
					logger.Debug("mcpServer/startupStatus/updated unmarshal failed", slog.Any("error", err))
					continue
				}
				if su.Status == "failed" {
					reason := cmp.Or(su.FailureReason, su.Error)
					logger.Warn("MCP server failed to start",
						slog.String("mcp_server", su.Name),
						slog.String("reason", reason))
				}

			case "model/rerouted":
				p, parseErr := parseModelRerouted(msg.Params)
				if parseErr != nil {
					logger.Debug("model/rerouted unmarshal failed", slog.Any("error", parseErr))
					agentcore.EmitNotification(params.OnEvent, method)
					continue
				}
				if p.ToModel != "" {
					state.model = p.ToModel
				}
				agentcore.EmitNotification(params.OnEvent, reroutedMessage(p.ToModel))

			default:
				params.OnEvent(domain.AgentEvent{
					Type:      domain.EventOtherMessage,
					Timestamp: now,
					Message:   method,
				})
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

	// Signal the reader goroutine to stop and close the connection
	// before closing stdin, preventing the handler from blocking on a
	// full msgCh during teardown.
	state.closeConnAndStop()

	// Close stdin to signal EOF to the app-server.
	state.mu.Lock()
	if state.stdin != nil {
		state.stdin.Close() //nolint:errcheck,gosec // best-effort cleanup
	}
	waitCh := state.waitCh
	pipes := state.pipes
	pid := 0
	if state.proc != nil {
		pid = state.proc.Pid
	}
	state.mu.Unlock()

	if pid > 0 {
		procutil.SignalGraceful(pid) //nolint:errcheck,gosec // best-effort graceful shutdown
	}

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
			if pid > 0 {
				procutil.KillProcessGroup(pid) //nolint:errcheck,gosec // best-effort force kill
			}
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
	state.proc = nil
	state.stdin = nil
	if state.pipes != nil {
		state.pipes.Close() //nolint:errcheck,gosec // best-effort cleanup
	}
	state.pipes = nil
	state.waitCh = nil
	state.mu.Unlock()

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
