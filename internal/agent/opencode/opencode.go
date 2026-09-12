// Package opencode implements [domain.AgentAdapter] for the OpenCode CLI.
// It launches one `opencode run --format json` subprocess per turn,
// normalizes stdout envelopes into domain events, and recovers final token
// usage with `opencode export --sanitize`. When a turn fails and the run
// stream carried nothing but opencode's masked generic server error, the
// adapter consults `opencode models` to reconstruct the unknown-model
// diagnostic.
//
// The CLI accepts no MCP configuration path as an argument, so on a local
// launch the adapter translates the file named by
// [domain.StartSessionParams] MCPConfigPath into OpenCode's own
// configuration form and delivers it in the turn's environment. A remote
// launch receives none: the only delivery route there is the command line,
// where the document's credentials would be readable by any user of the
// host.
package opencode

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

func init() {
	registry.Agents.RegisterWithMeta("opencode", NewOpenCodeAdapter, registry.AgentMeta{
		RequiresCommand:     true,
		ValidateAgentConfig: validateConfig,
		MCPInjection:        registry.MCPInjectionTranslated,
		UsageArrival:        registry.UsageArrivalTurnEnd,
		UsageAttribution:    registry.UsageAttributionPerModel,
	})
}

var _ domain.AgentAdapter = (*OpenCodeAdapter)(nil)

type OpenCodeAdapter struct {
	passthrough passthroughConfig
}

type sessionState struct {
	target         agentcore.LaunchTarget
	agentConfig    domain.AgentConfig
	passthrough    passthroughConfig
	sessionID      string
	turnCount      int
	sessionOpened  bool
	closed         bool
	baseLogger     *slog.Logger
	createdSession bool
	runStartedAtMS int64
	usage          *agentcore.TurnEndUsage
	mu             sync.Mutex
	active         *turnRuntime

	// drainGrace bounds every post-exit wait a turn performs on its
	// stdout reader once the subprocess has been reaped. Written once by
	// StartSession, or by a test before a session's first turn; RunTurn
	// reads it when it builds each turn's own turnRuntime.
	drainGrace time.Duration

	// mcpConfigContent is the translated MCP configuration document
	// delivered through the runtime's inline configuration environment
	// variable on every turn's subprocess. Empty when the session
	// carries no generated configuration, when that configuration
	// declares no server, or when the launch target is remote. Set
	// once in StartSession and never mutated after.
	mcpConfigContent string
}

type turnRuntime struct {
	pid             string
	proc            *os.Process
	waitCh          chan waitResult
	reapedCh        chan struct{}
	reader          *procutil.StdoutReader
	stderrCollector *procutil.StderrCollector
	firstJSONSeen   bool
	terminalError   *rawRunError
	terminalOutcome domain.AgentEventType
	waitMu          sync.Mutex
	waitRes         waitResult

	// drainGrace is the bound startWait and every post-exit reader wait
	// applies once the subprocess has been reaped. Recorded from
	// sessionState.drainGrace when the turn is launched.
	drainGrace time.Duration

	// work is the per-turn work-evidence observer for the shared
	// turn-disposition decision.
	work *agentcore.WorkObserver
}

type waitResult struct {
	exitCode int
	err      error
}

// NewOpenCodeAdapter creates an [OpenCodeAdapter] from the raw "opencode"
// adapter configuration in WORKFLOW.md.
func NewOpenCodeAdapter(config map[string]any) (domain.AgentAdapter, error) {
	pt, fault := parsePassthroughConfig(config)
	if fault != nil {
		return nil, fault
	}
	if err := checkCrossField(pt); err != nil {
		return nil, err
	}
	return &OpenCodeAdapter{passthrough: pt}, nil
}

// StartSession resolves the launch target and initializes adapter-owned
// session state without starting an OpenCode subprocess.
func (a *OpenCodeAdapter) StartSession(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
	target, agentErr := agentcore.ResolveLaunchTarget(params, "opencode")
	if agentErr != nil {
		return domain.Session{}, agentErr
	}

	mcpConfigContent, mcpErr := buildMCPConfigContent(params.MCPConfigPath, target.RemoteCommand != "")
	if mcpErr != nil {
		return domain.Session{}, &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: fmt.Sprintf("translate MCP config: %v", mcpErr),
			Err:     mcpErr,
		}
	}

	state := &sessionState{
		target:           target,
		agentConfig:      params.AgentConfig,
		passthrough:      a.passthrough,
		sessionID:        params.ResumeSessionID,
		baseLogger:       slog.Default().With(slog.String("component", "opencode-adapter")),
		createdSession:   params.ResumeSessionID == "",
		runStartedAtMS:   time.Now().UnixMilli(),
		usage:            agentcore.NewTurnEndUsage(),
		drainGrace:       procutil.DefaultDrainGrace,
		mcpConfigContent: mcpConfigContent,
	}

	return domain.Session{
		ID:       state.sessionID,
		AgentPID: "",
		Internal: state,
	}, nil
}

// RunTurn executes one OpenCode turn by starting a subprocess, reading its
// stdout through a single reader goroutine, and relaying normalized events via
// params.OnEvent.
func (a *OpenCodeAdapter) RunTurn(ctx context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
	if params.OnEvent == nil {
		panic("opencode: OnEvent must be non-nil")
	}

	state, ok := session.Internal.(*sessionState)
	if !ok {
		return domain.TurnResult{}, &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: fmt.Sprintf("unexpected session internal type %T", session.Internal),
		}
	}

	env, err := buildRunEnv(os.Environ(), a.passthrough)
	if err != nil {
		return domain.TurnResult{}, &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: "build opencode environment",
			Err:     err,
		}
	}
	env = appendMCPConfigEnv(env, state.mcpConfigContent)

	managedEnv, err := buildManagedEnv(a.passthrough)
	if err != nil {
		return domain.TurnResult{}, &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: "build opencode managed environment",
			Err:     err,
		}
	}

	state.mu.Lock()
	if state.closed {
		state.mu.Unlock()
		return domain.TurnResult{}, &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: "session already stopped",
		}
	}
	if state.active != nil {
		state.mu.Unlock()
		return domain.TurnResult{}, &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: "session already has an active turn",
		}
	}
	state.turnCount++
	cmdArgs := buildRunArgs(state, params.Prompt, a.passthrough)
	logger := state.loggerLocked()

	var cmd *exec.Cmd
	if state.target.RemoteCommand != "" {
		remoteCommand := buildSSHRemoteCommand(state.target.RemoteCommand, managedEnv)
		sshArgs := sshutil.BuildSSHArgs(
			state.target.SSHHost,
			state.target.WorkspacePath,
			remoteCommand,
			cmdArgs,
			sshutil.SSHOptions{StrictHostKeyChecking: state.target.SSHStrictHostKeyChecking},
		)
		cmd = exec.CommandContext(ctx, state.target.Command, sshArgs...) //nolint:gosec // args are constructed programmatically with shell quoting
	} else {
		allArgs := append(slices.Clone(state.target.Args), cmdArgs...)
		cmd = exec.CommandContext(ctx, state.target.Command, allArgs...) //nolint:gosec // args are constructed programmatically
	}
	procutil.SetGroupCancel(cmd, procutil.StopGrace(state.agentConfig.StopGraceMS))
	cmd.Dir = state.target.WorkspacePath
	cmd.Env = env

	pipes, err := procutil.StartWithOwnedPipes(cmd)
	if err != nil {
		state.mu.Unlock()

		var startErr *procutil.StartError
		if !errors.As(err, &startErr) {
			return domain.TurnResult{}, &domain.AgentError{
				Kind:    domain.ErrResponseError,
				Message: "start opencode subprocess",
				Err:     err,
			}
		}
		switch startErr.Stage {
		case procutil.StageStdoutPipe:
			return domain.TurnResult{}, &domain.AgentError{
				Kind:    domain.ErrResponseError,
				Message: "create stdout pipe",
				Err:     startErr.Err,
			}
		case procutil.StageStderrPipe:
			return domain.TurnResult{}, &domain.AgentError{
				Kind:    domain.ErrResponseError,
				Message: "create stderr pipe",
				Err:     startErr.Err,
			}
		default: // procutil.StageProcessStart
			return domain.TurnResult{}, &domain.AgentError{
				Kind:    domain.ErrResponseError,
				Message: "start opencode subprocess",
				Err:     startErr.Err,
			}
		}
	}
	// The package's only close of either pipe end: covers all five
	// return paths this turn can take, none of which precedes it.
	defer pipes.Close() //nolint:errcheck,gosec // best-effort cleanup

	runtime := &turnRuntime{
		pid:             strconv.Itoa(cmd.Process.Pid),
		proc:            cmd.Process,
		waitCh:          make(chan waitResult, 1),
		reapedCh:        make(chan struct{}),
		drainGrace:      state.drainGrace,
		terminalOutcome: domain.EventTurnCompleted,
		work:            agentcore.NewWorkObserver(agentcore.WorkSignals{AssistantOutput: true, ToolActivity: true}),
	}
	state.active = runtime
	state.mu.Unlock()

	if assignErr := procutil.AssignProcess(cmd.Process.Pid, cmd.Process); assignErr != nil {
		logger.Warn("process group assignment failed", slog.Any("error", assignErr))
	}

	runtime.stderrCollector = procutil.NewStderrCollector(pipes.Stderr, logger)
	runtime.reader = procutil.NewStdoutReader(pipes.Stdout, logger)
	startWait(runtime, cmd)

	emit := func(event domain.AgentEvent) {
		if state.target.RemoteCommand == "" {
			event.AgentPID = runtime.pid
		}
		params.OnEvent(event)
	}

	readTimeout := readTimeout(state)
	readTimer := time.NewTimer(readTimeout)
	defer stopTimer(readTimer)

	readTimeoutC := readTimer.C
	lineCh := runtime.reader.Stream()
	waitCh := runtime.waitCh
	reapedCh := runtime.reapedCh
	var postExitC <-chan time.Time
	var postExitAt time.Time
	var exit waitResult
	processExited := false

	// handleLine applies one raw stdout line's per-event path: parsing,
	// the first-JSON latch, and the event switch. It is shared between
	// the turn's primary select loop and the post-exit drain below, so a
	// line arriving in either place is processed identically. done
	// reports that the turn's own result is ready to return.
	handleLine := func(line []byte) (domain.TurnResult, error, bool) {
		event, parseErr := parseRunEvent(line)
		var parsed parsedLine
		if parseErr != nil {
			parsed.PlainText = string(line)
		} else {
			parsed.Event = &event
		}

		if parsed.PlainText != "" {
			if readTimeoutC != nil {
				resetTimer(readTimer, readTimeout)
			}

			plainText := typeutil.TruncateRunes(parsed.PlainText, 500)
			emit(domain.AgentEvent{
				Type:      domain.EventMalformed,
				Timestamp: time.Now().UTC(),
				Message:   plainText,
			})
			return domain.TurnResult{}, nil, false
		}

		rawEvent := parsed.Event
		if rawEvent == nil {
			return domain.TurnResult{}, nil, false
		}

		runtime.firstJSONSeen = true
		if readTimeoutC != nil {
			stopTimer(readTimer)
			readTimeoutC = nil
		}

		started, mismatch := state.applySessionEvent(rawEvent.SessionID)
		if mismatch {
			message := fmt.Sprintf("session id mismatch: expected %q, got %q", state.currentSessionID(), rawEvent.SessionID)
			killTurnProcess(runtime)
			_ = waitForProcess(runtime)
			drainReaderBounded(runtime.reader, runtime.drainGrace)
			procutil.EmitWarnLines(runtime.stderrCollector.Lines(), state.logger())
			clearActive(state, runtime)
			ev := agentcore.TurnEvidence{
				Terminal:          agentcore.TerminalFailure,
				TerminalErrorKind: domain.ErrResponseError,
				TerminalMessage:   message,
			}
			result, agentErr := state.usage.Finalize(emit, state.logger(), ev, state.currentSessionID(), 0, nil)
			return result, agentErr, true
		}
		if started {
			emit(domain.AgentEvent{
				Type:      domain.EventSessionStarted,
				Timestamp: time.Now().UTC(),
				SessionID: state.currentSessionID(),
				Message:   "session started",
			})
		}

		now := time.Now().UTC()
		switch rawEvent.Type {
		case "step_start":
			if _, err := parseStepStartPart(rawEvent.Part); err != nil {
				emit(domain.AgentEvent{Type: domain.EventMalformed, Timestamp: now, Message: "invalid step_start payload"})
				return domain.TurnResult{}, nil, false
			}
			agentcore.EmitNotification(emit, "step started")

		case "text":
			part, err := parseTextPart(rawEvent.Part)
			if err != nil {
				emit(domain.AgentEvent{Type: domain.EventMalformed, Timestamp: now, Message: "invalid text payload"})
				return domain.TurnResult{}, nil, false
			}
			runtime.work.ObserveAssistantOutput()
			agentcore.EmitNotification(emit, typeutil.TruncateRunes(part.Text, 500))

		case "reasoning":
			if _, err := parseReasoningPart(rawEvent.Part); err != nil {
				emit(domain.AgentEvent{Type: domain.EventMalformed, Timestamp: now, Message: "invalid reasoning payload"})
				return domain.TurnResult{}, nil, false
			}
			runtime.work.ObserveAssistantOutput()
			emit(domain.AgentEvent{
				Type:      domain.EventOtherMessage,
				Timestamp: now,
				Message:   "reasoning block",
			})

		case "tool_use":
			part, err := parseToolPart(rawEvent.Part)
			if err != nil {
				emit(domain.AgentEvent{Type: domain.EventMalformed, Timestamp: now, Message: "invalid tool_use payload"})
				return domain.TurnResult{}, nil, false
			}
			runtime.work.ObserveToolActivity()
			emit(domain.AgentEvent{
				Type:           domain.EventToolResult,
				Timestamp:      now,
				ToolName:       part.Tool,
				ToolDurationMS: toolDuration(part.State.Time),
				ToolError:      strings.EqualFold(part.State.Status, "error"),
				Message:        typeutil.TruncateRunes(part.State.Error, 500),
			})

		case "step_finish":
			part, err := parseStepFinishPart(rawEvent.Part)
			if err != nil {
				emit(domain.AgentEvent{Type: domain.EventMalformed, Timestamp: now, Message: "invalid step_finish payload"})
				return domain.TurnResult{}, nil, false
			}
			agentcore.EmitNotification(emit, fmt.Sprintf("step finished: %s", part.Reason))

		case "error":
			runtime.terminalOutcome = domain.EventTurnFailed
			// One failure can surface as two error events: the
			// actionable diagnostic the session publishes, and the
			// generic placeholder the run command reports when the
			// failure is not in its API error schema. Their order on
			// the stream is not guaranteed, so keep whichever event
			// carries detail rather than whichever arrives last.
			if runtime.terminalError == nil || !isMaskedServerError(rawRunErrorMessage(rawEvent.Error)) {
				runtime.terminalError = rawEvent.Error
			}

		default:
			emit(domain.AgentEvent{
				Type:      domain.EventMalformed,
				Timestamp: now,
				Message:   fmt.Sprintf("unknown event type: %s", rawEvent.Type),
			})
		}

		return domain.TurnResult{}, nil, false
	}

	for {
		// Checked ahead of the select, not as one of its arms: a
		// descendant that keeps writing holds the line arm ready, and
		// the choice among ready arms is random, so the post-exit arm
		// could be passed over for as long as that descendant talks.
		if !postExitAt.IsZero() && !time.Now().Before(postExitAt) {
			if result, agentErr, done := drainLinesBounded(lineCh, runtime, postExitAt, handleLine); done {
				return result, agentErr
			}
			return a.finalizeExitedTurn(ctx, state, runtime, emit, exit)
		}

		select {
		case line, ok := <-lineCh:
			if !ok {
				lineCh = nil
				if readErr := runtime.reader.Err(); readErr != nil && !errors.Is(readErr, procutil.ErrStdoutAbandoned) {
					killTurnProcess(runtime)
					_ = waitForProcess(runtime)
					clearActive(state, runtime)

					ev := agentcore.TurnEvidence{Terminal: agentcore.TerminalCancelled, TerminalMessage: "turn cancelled"}
					if ctx.Err() == nil && !state.isClosed() {
						procutil.EmitWarnLines(runtime.stderrCollector.Lines(), state.logger())
						ev = agentcore.TurnEvidence{
							Terminal:          agentcore.TerminalFailure,
							TerminalErrorKind: domain.ErrResponseError,
							TerminalMessage:   "stdout read error",
							Cause:             readErr,
						}
					}
					result, agentErr := state.usage.Finalize(emit, state.logger(), ev, state.currentSessionID(), 0, nil)
					if agentErr != nil {
						return result, agentErr
					}
					return result, nil
				}
				if processExited {
					return a.finalizeExitedTurn(ctx, state, runtime, emit, exit)
				}
				continue
			}

			if result, agentErr, done := handleLine(line); done {
				return result, agentErr
			}

		case <-reapedCh:
			// Disabled once taken: the channel stays ready after it
			// closes, and a live arm would spin for as long as the
			// stderr bound runs.
			reapedCh = nil
			stopTimer(readTimer)
			readTimeoutC = nil

		case <-waitCh:
			exit = waitForProcess(runtime)
			processExited = true
			waitCh = nil
			postExitC = time.After(runtime.drainGrace)
			postExitAt = time.Now().Add(runtime.drainGrace)
			if lineCh == nil {
				return a.finalizeExitedTurn(ctx, state, runtime, emit, exit)
			}

		case <-postExitC:
			if result, agentErr, done := drainLinesBounded(lineCh, runtime, postExitAt, handleLine); done {
				return result, agentErr
			}
			return a.finalizeExitedTurn(ctx, state, runtime, emit, exit)

		case <-ctx.Done():
			killTurnProcess(runtime)
			_ = waitForProcess(runtime)
			drainReaderBounded(runtime.reader, runtime.drainGrace)
			clearActive(state, runtime)
			ev := agentcore.TurnEvidence{Terminal: agentcore.TerminalCancelled, TerminalMessage: "turn cancelled"}
			result, agentErr := state.usage.Finalize(emit, state.logger(), ev, state.currentSessionID(), 0, nil)
			if agentErr != nil {
				return result, agentErr
			}
			return result, nil

		case <-readTimeoutC:
			// Both arms can be ready at once, and the choice between
			// them is random: the reap wins on the merits, because a
			// subprocess already gone did not time out.
			select {
			case <-reapedCh:
				reapedCh = nil
				stopTimer(readTimer)
				readTimeoutC = nil
				continue
			default:
			}

			killTurnProcess(runtime)
			_ = waitForProcess(runtime)
			drainReaderBounded(runtime.reader, runtime.drainGrace)
			procutil.EmitWarnLines(runtime.stderrCollector.Lines(), state.logger())
			clearActive(state, runtime)
			ev := agentcore.TurnEvidence{
				Terminal:          agentcore.TerminalFailure,
				TerminalErrorKind: domain.ErrResponseTimeout,
				TerminalMessage:   "timed out waiting for first opencode json event",
			}
			result, agentErr := state.usage.Finalize(emit, state.logger(), ev, state.currentSessionID(), 0, nil)
			if agentErr != nil {
				return result, agentErr
			}
			return result, nil
		}
	}
}

// StopSession marks the session closed and terminates any active subprocess.
func (a *OpenCodeAdapter) StopSession(ctx context.Context, session domain.Session) error {
	state, ok := session.Internal.(*sessionState)
	if !ok {
		return &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: fmt.Sprintf("unexpected session internal type %T", session.Internal),
		}
	}

	state.mu.Lock()
	state.closed = true
	active := state.active
	state.active = nil
	state.mu.Unlock()

	return stopActiveTurn(ctx, active, procutil.StopGrace(state.agentConfig.StopGraceMS), state.logger())
}

// finalizeExitedTurn builds and emits the turn's terminal disposition once
// the subprocess has exited. It also scans the turn's stderr lines for a
// denied-permission warning, which the runtime writes to stderr rather than
// stdout; recognizing it here rather than mid-turn means the corresponding
// notification arrives at turn end rather than as it happens, and it does
// not reset the read timer the way a stdout line does, neither of which is
// a regression because the warning has never actually reached stdout.
func (a *OpenCodeAdapter) finalizeExitedTurn(ctx context.Context, state *sessionState, runtime *turnRuntime, emit func(domain.AgentEvent), exit waitResult) (domain.TurnResult, error) {
	window := int64(0)
	if !state.createdSession {
		window = state.runStartedAtMS
	}
	usage := queryExportUsage(ctx, state, window)

	var recovered *agentcore.RecoveredUsage
	if hasUsage(usage) {
		recovered = &agentcore.RecoveredUsage{
			Run: domain.TokenUsage{
				InputTokens:     usage.InputTokens,
				OutputTokens:    usage.OutputTokens,
				CacheReadTokens: usage.CacheReadTokens,
			},
			Model: usage.Model,
		}
	}

	clearActive(state, runtime)
	stderrLines := runtime.stderrCollector.Lines()
	for _, line := range stderrLines {
		if !isPermissionWarning(line) {
			continue
		}
		posture := agentcore.DecideHumanRequest(agentcore.ClassPermission, false, agentcore.AnswerRuntimeRefused)
		agentcore.EmitNotification(emit, posture.Notice)
	}
	sessionID := state.currentSessionID()

	ev := agentcore.TurnEvidence{
		ExitObserved: true,
		ExitCode:     exit.exitCode,
		Cause:        exit.err,
	}
	ev.Work, ev.WorkDetail = runtime.work.Report()

	switch {
	case runtime.terminalOutcome == domain.EventTurnFailed:
		ev.Terminal = agentcore.TerminalFailure
		ev.TerminalErrorKind = domain.ErrTurnFailed
		ev.Cause = nil
		ev.TerminalMessage = rawRunErrorMessage(runtime.terminalError)
		if isMaskedServerError(ev.TerminalMessage) {
			if detail, ok := queryModelNotFound(ctx, state); ok {
				state.logger().Debug("recovered masked opencode failure detail", slog.String("detail", detail))
				ev.TerminalMessage = detail
			}
		}
		procutil.EmitWarnLines(stderrLines, state.logger())

	case ctx.Err() != nil || state.isClosed():
		ev.Terminal = agentcore.TerminalCancelled
		ev.TerminalMessage = "turn cancelled"
		ev.Cause = nil

	case !runtime.firstJSONSeen:
		procutil.EmitWarnLines(stderrLines, state.logger())

	case exit.err != nil || exit.exitCode != 0:
		procutil.EmitWarnLines(stderrLines, state.logger())
	}

	result, agentErr := state.usage.Finalize(emit, state.logger(), ev, sessionID, 0, recovered)
	if agentErr != nil {
		return result, agentErr
	}
	return result, nil
}

func (s *sessionState) logger() *slog.Logger {
	sessionID := s.currentSessionID()
	if sessionID == "" {
		return s.baseLogger
	}
	return logging.WithSession(s.baseLogger, sessionID)
}

// loggerLocked returns a logger for s, reading sessionID without acquiring
// s.mu. Callers must already hold s.mu.
func (s *sessionState) loggerLocked() *slog.Logger {
	if s.sessionID == "" {
		return s.baseLogger
	}
	return logging.WithSession(s.baseLogger, s.sessionID)
}

func (s *sessionState) currentSessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}

func (s *sessionState) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *sessionState) applySessionEvent(eventSessionID string) (bool, bool) {
	if eventSessionID == "" {
		return false, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.sessionID == "" {
		s.sessionID = eventSessionID
	} else if s.sessionID != eventSessionID {
		return false, true
	}

	if s.sessionOpened {
		return false, false
	}
	s.sessionOpened = true
	return true, false
}

// startWait reaps the turn's subprocess independently of its stdout
// reader and, once the reap and group kill have run, bounds the wait
// for the turn's stderr drain before reading it.
func startWait(runtime *turnRuntime, cmd *exec.Cmd) {
	go func() {
		reaper := procutil.StartReaper(cmd)
		<-reaper.Done()

		// The turn's exit is published below, behind a stderr bound that
		// can spend the whole drain grace. Anything that must react to
		// the subprocess being gone rather than to its result reads this
		// channel instead, so the delay cannot be mistaken for a turn
		// still running.
		close(runtime.reapedCh)

		runtime.stderrCollector.FinishAndCollect(runtime.drainGrace)

		runtime.waitMu.Lock()
		runtime.waitRes = waitResult{
			exitCode: procutil.ExtractExitCode(reaper.Err()),
			err:      reaper.Err(),
		}
		runtime.waitMu.Unlock()

		close(runtime.waitCh)
	}()
}

// drainReaderBounded discards every line still receivable from reader
// until its scan ends or grace elapses. Stream is unbuffered, so a
// reader parked on a send finishes only once something receives; this
// keeps the group kill's release of that reader from being confused
// with an abandonment, because Abandon is called only when the timer
// wins.
func drainReaderBounded(reader *procutil.StdoutReader, grace time.Duration) {
	timer := time.NewTimer(grace)
	defer stopTimer(timer)
	for {
		select {
		case _, ok := <-reader.Stream():
			if !ok {
				return
			}
		case <-timer.C:
			reader.Abandon(grace)
			return
		}
	}
}

func waitForProcess(runtime *turnRuntime) waitResult {
	<-runtime.waitCh
	runtime.waitMu.Lock()
	defer runtime.waitMu.Unlock()
	return runtime.waitRes
}

func clearActive(state *sessionState, runtime *turnRuntime) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.active == runtime {
		state.active = nil
	}
}

// stopActiveTurn signals runtime's subprocess to exit gracefully and
// waits up to grace for it to do so on its own before force-terminating
// its process group. logger receives the graceful phase's outcome.
func stopActiveTurn(ctx context.Context, runtime *turnRuntime, grace time.Duration, logger *slog.Logger) error {
	if runtime == nil {
		return nil
	}

	if runtime.proc == nil {
		return nil
	}

	_ = procutil.SignalGraceful(runtime.proc.Pid) //nolint:errcheck // best-effort signal; process may already be dead

	started := time.Now()
	graceTimer := time.NewTimer(grace)
	defer stopTimer(graceTimer)

	select {
	case <-runtime.waitCh:
		logger.Debug("agent exited during the graceful phase", slog.String("outcome", "exited"))
		return nil
	case <-graceTimer.C:
		logger.Warn("agent did not exit inside the graceful period and was force-terminated",
			slog.String("outcome", "grace elapsed"), slog.Duration("grace", grace),
			slog.Duration("elapsed", time.Since(started)))
		killTurnProcess(runtime)
		return nil
	case <-ctx.Done():
		logger.Warn("agent did not exit inside the graceful period and was force-terminated",
			slog.String("outcome", "caller deadline"), slog.Duration("grace", grace),
			slog.Duration("elapsed", time.Since(started)))
		killTurnProcess(runtime)
		return ctx.Err()
	}
}

func killTurnProcess(runtime *turnRuntime) {
	if runtime == nil || runtime.proc == nil {
		return
	}
	procutil.KillProcessGroup(runtime.proc.Pid) //nolint:errcheck,gosec // best-effort cleanup
}

func stopTimer(timer *time.Timer) {
	if timer == nil {
		return
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func resetTimer(timer *time.Timer, timeout time.Duration) {
	stopTimer(timer)
	timer.Reset(timeout)
}

func readTimeout(state *sessionState) time.Duration {
	if state.agentConfig.ReadTimeoutMS > 0 {
		return time.Duration(state.agentConfig.ReadTimeoutMS) * time.Millisecond
	}
	return 30 * time.Second
}

func exportTimeout(state *sessionState) time.Duration {
	timeout := 2 * readTimeout(state)
	if timeout <= 0 || timeout > 30*time.Second {
		return 30 * time.Second
	}
	return timeout
}

func isPermissionWarning(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "! permission requested:")
}

func toolDuration(partTime rawPartTime) int64 {
	if partTime.End <= partTime.Start {
		return 0
	}
	return partTime.End - partTime.Start
}

func rawRunErrorMessage(runErr *rawRunError) string {
	if runErr == nil {
		return "opencode reported an unknown error"
	}
	if runErr.Data != nil {
		if message, ok := runErr.Data["message"].(string); ok && message != "" {
			return message
		}
	}
	if runErr.Name != "" {
		return runErr.Name
	}
	return "opencode reported an unknown error"
}

// maskedServerErrorMessage is the placeholder opencode emits on the run
// stream when its server error boundary swallows an unhandled internal
// failure, hiding the underlying cause from the JSON events.
const maskedServerErrorMessage = "Unexpected server error. Check server logs for details."

// isMaskedServerError reports whether message carries no failure detail
// beyond opencode's generic server-error placeholder.
func isMaskedServerError(message string) bool {
	return strings.TrimSpace(message) == maskedServerErrorMessage
}

func hasUsage(usage exportUsage) bool {
	return usage.InputTokens > 0 || usage.OutputTokens > 0 || usage.TotalTokens > 0 || usage.CacheReadTokens > 0
}

// drainLinesBounded takes whatever the reader has already produced and
// gives up on it at capAt, the one deadline the post-exit wait carries:
// starting a fresh grace here would let a descendant that keeps writing
// spend two of them. The deadline is tested before each line for the
// same reason the caller tests it before its select: a descendant that
// keeps writing holds the channel ready, so an arm that merely competes
// with it can be passed over indefinitely.
func drainLinesBounded(
	lineCh <-chan []byte,
	runtime *turnRuntime,
	capAt time.Time,
	handleLine func([]byte) (domain.TurnResult, error, bool),
) (domain.TurnResult, error, bool) {
	for time.Now().Before(capAt) {
		select {
		case line, ok := <-lineCh:
			if !ok {
				return domain.TurnResult{}, nil, false
			}
			if result, agentErr, done := handleLine(line); done {
				return result, agentErr, true
			}
		default:
			runtime.reader.Abandon(runtime.drainGrace)
			return domain.TurnResult{}, nil, false
		}
	}
	runtime.reader.Abandon(runtime.drainGrace)
	return domain.TurnResult{}, nil, false
}
