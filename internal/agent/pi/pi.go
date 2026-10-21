// Package pi implements [domain.AgentAdapter] for the Pi CLI. It
// launches one `pi -p --mode json` subprocess per Sortie turn,
// normalizes the JSON event stream into domain events, and settles the
// session's cumulative token usage from the one usage figure each pi
// turn_end carries.
//
// The adapter declares [registry.MCPInjectionUnsupported]: it does not
// hand the worker-generated MCP configuration to pi, so Sortie's tools
// are not available to a pi session.
package pi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
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
	"github.com/sortie-ai/sortie/internal/redact"
	"github.com/sortie-ai/sortie/internal/registry"
)

const defaultCommand = "pi"

func init() {
	registry.Agents.RegisterWithMeta("pi", NewPiAdapter, registry.AgentMeta{
		RequiresCommand:     true,
		DefaultCommand:      defaultCommand,
		ValidateAgentConfig: validateConfig,
		MCPInjection:        registry.MCPInjectionUnsupported,
		UsageArrival:        registry.UsageArrivalTurnEnd,
		UsageAttribution:    registry.UsageAttributionPerModel,
		CredentialEnv:       registry.DeclareCredentialEnv(),
		EffortForwarding:    registry.EffortNotForwarded,
	})
}

var _ domain.AgentAdapter = (*PiAdapter)(nil)

type PiAdapter struct{}

type sessionState struct {
	target        agentcore.LaunchTarget
	agentConfig   domain.AgentConfig
	passthrough   passthroughConfig
	sessionID     string
	sessionOpened bool
	closed        bool
	baseLogger    *slog.Logger
	usage         *agentcore.TurnEndUsage
	mu            sync.Mutex
	active        *turnRuntime

	// total is the session's run-cumulative token usage: the sum of
	// every model response every turn of this session produced. It is
	// only read and written on the goroutine executing the session's
	// single active turn, which the orchestrator never overlaps.
	total domain.TokenUsage

	// drainGrace bounds every post-exit wait a turn performs on its
	// stdout reader once the subprocess has been reaped. Written once by
	// StartSession, or by a test before a session's first turn; RunTurn
	// reads it when it builds each turn's own turnRuntime.
	drainGrace time.Duration

	credentialVerification bool
}

type turnRuntime struct {
	pid             string
	proc            *os.Process
	tools           *agentcore.ToolTracker
	group           *procutil.Group
	waitCh          chan waitResult
	reapedCh        chan struct{}
	reader          *procutil.StdoutReader
	stderrCollector *procutil.StderrCollector
	firstJSONSeen   bool
	waitMu          sync.Mutex
	waitRes         waitResult

	// output records whether the runtime has written any readable line
	// this turn, which is what separates an early exit from a turn that
	// responded and then failed.
	output agentcore.OutputWatch

	// drainGrace is the bound startWait and every post-exit reader wait
	// applies once the subprocess has been reaped. Recorded from
	// sessionState.drainGrace when the turn is launched.
	drainGrace time.Duration

	// work is the per-turn work-evidence observer for the shared
	// turn-disposition decision.
	work *agentcore.WorkObserver

	// usage is this turn's own model responses, summed. usageModel
	// names the model the last counted response came from, which is
	// what a turn carrying several models reports.
	usage      domain.TokenUsage
	usageModel string

	// spend is the turn's evidence for the shared spend rule. The turn's
	// figure is settled once pi ends its run without a retry pending.
	spend agentcore.TurnSpend

	// assistantFailure holds the failure of the last assistant response, or ""
	// when it completed. pi retries a response that stopped with "error"
	// inside the same process, so only the last response decides the turn.
	// pi --mode json exits 0 for "error" and "aborted", so this is the
	// only evidence such a turn failed.
	assistantFailure string

	// sawAssistantMessageEnd records whether the current pi turn already
	// delivered its authoritative message_end, so turn_end does not
	// report the same assistant response's text a second time.
	sawAssistantMessageEnd bool
}

type waitResult struct {
	exitCode int
	err      error
	// stopped is the launch record's verdict: a stop began while the
	// subprocess was still running.
	stopped bool
}

// NewPiAdapter creates an [PiAdapter]. Settings are resolved per
// session in [PiAdapter.StartSession] from
// [domain.StartSessionParams.Settings].
func NewPiAdapter() (domain.AgentAdapter, error) {
	return &PiAdapter{}, nil
}

// StartSession resolves the launch target and initializes adapter-owned
// session state without starting a turn subprocess.
func (a *PiAdapter) StartSession(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
	pt, faults := parsePassthroughConfig(params.Settings)
	if len(faults) > 0 {
		messages := make([]string, len(faults))
		for i, f := range faults {
			messages[i] = f.Message
		}
		firstErr := faults[0].Err
		if firstErr == nil {
			firstErr = errors.New(faults[0].Message)
		}
		return domain.Session{}, agentcore.SettingsError(strings.Join(messages, "; "), firstErr)
	}
	target, agentErr := agentcore.ResolveLaunchTarget(params, defaultCommand)
	if agentErr != nil {
		return domain.Session{}, agentErr
	}

	state := &sessionState{
		target:                 target,
		agentConfig:            params.AgentConfig,
		passthrough:            pt,
		sessionID:              params.ResumeSessionID,
		baseLogger:             slog.Default().With(slog.String("component", "pi-adapter")),
		usage:                  agentcore.NewTurnEndUsage(),
		drainGrace:             procutil.DefaultDrainGrace,
		credentialVerification: params.CredentialVerification,
	}

	return domain.Session{
		ID:       state.sessionID,
		AgentPID: "",
		Internal: state,
	}, nil
}

// RunTurn executes one Pi turn by starting a subprocess, reading its
// stdout through a single reader goroutine, and relaying normalized events via
// params.OnEvent.
func (a *PiAdapter) RunTurn(ctx context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
	if params.OnEvent == nil {
		panic("pi: OnEvent must be non-nil")
	}

	state, ok := session.Internal.(*sessionState)
	if !ok {
		return domain.TurnResult{}, &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: fmt.Sprintf("unexpected session internal type %T", session.Internal),
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

	cmdArgs := buildRunArgs(state, state.passthrough)
	logger := state.loggerLocked()

	var cmd *exec.Cmd
	var launch sshutil.SSHLaunch
	if state.target.RemoteCommand != "" {
		launch = sshutil.BuildSSHLaunch(
			state.target.SSHHost,
			state.target.WorkspacePath,
			state.target.RemoteCommand,
			cmdArgs,
			state.target.SSHOptions(),
		)
		cmd = exec.CommandContext(ctx, state.target.Command, launch.Args...) //nolint:gosec // args are constructed programmatically with shell quoting
	} else {
		allArgs := append(slices.Clone(state.target.Args), cmdArgs...)
		cmd = exec.CommandContext(ctx, state.target.Command, allArgs...) //nolint:gosec // args are constructed programmatically
	}
	procutil.SetGroupCancel(cmd, procutil.StopGrace(state.agentConfig.StopGraceMS))
	cmd.Env = os.Environ()
	if bindErr := state.target.BindWorkspace(cmd); bindErr != nil {
		state.mu.Unlock()
		return domain.TurnResult{}, bindErr
	}
	cmd.Stdin = buildTurnStdin(params.Prompt, launch.StdinReader())

	pipes, group, err := procutil.StartWithOwnedPipes(ctx, cmd, logger)
	if err != nil {
		state.mu.Unlock()

		if cancelled := agentcore.SessionStartCancelledError(ctx, err); cancelled != nil {
			return domain.TurnResult{}, cancelled
		}
		var startErr *procutil.StartError
		if !errors.As(err, &startErr) {
			return domain.TurnResult{}, &domain.AgentError{
				Kind:    domain.ErrResponseError,
				Message: "start pi subprocess",
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
		default: // procutil.StageProcessStart, procutil.StageProcessResume
			return domain.TurnResult{}, &domain.AgentError{
				Kind:    domain.ErrResponseError,
				Message: "start pi subprocess",
				Err:     startErr.Err,
			}
		}
	}
	// The package's only close of either pipe end: covers all five
	// return paths this turn can take, none of which precedes it.
	defer pipes.Close() //nolint:errcheck,gosec // best-effort cleanup

	runtime := &turnRuntime{
		pid:        strconv.Itoa(cmd.Process.Pid),
		proc:       cmd.Process,
		tools:      agentcore.NewToolTracker(),
		group:      group,
		waitCh:     make(chan waitResult, 1),
		reapedCh:   make(chan struct{}),
		drainGrace: state.drainGrace,
		work:       agentcore.NewWorkObserver(agentcore.WorkSignals{AssistantOutput: true, ToolActivity: true}),
	}
	state.active = runtime
	state.mu.Unlock()

	runtime.stderrCollector = procutil.NewStderrCollector(pipes.Stderr, logger)
	runtime.reader = procutil.NewStdoutReader(pipes.Stdout, logger)
	startWait(runtime, cmd, logger)

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
	ctxDone := ctx.Done()

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

			plainText := redact.Truncate(parsed.PlainText, 500)
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

		runtime.output.Observe(line)
		runtime.firstJSONSeen = true
		if readTimeoutC != nil {
			stopTimer(readTimer)
			readTimeoutC = nil
		}

		now := time.Now().UTC()
		malformed := func(reason error) {
			emit(domain.AgentEvent{
				Type:      domain.EventMalformed,
				Timestamp: now,
				Message:   reason.Error(),
			})
		}

		if rawEvent.Type == "session" {
			id, cwd, err := rawEvent.sessionHeader()
			if err != nil {
				malformed(err)
				return domain.TurnResult{}, nil, false
			}
			if problem := state.checkSessionHeader(id, cwd); problem != "" {
				killTurnProcess(runtime)
				_ = waitForProcess(runtime)
				drainReaderBounded(runtime.reader, runtime.drainGrace)
				procutil.EmitWarnLines(runtime.stderrCollector.Lines(), state.logger())
				clearActive(state, runtime)
				ev := agentcore.TurnEvidence{
					Terminal:          agentcore.TerminalFailure,
					TerminalErrorKind: domain.ErrResponseError,
					TerminalMessage:   problem,
				}
				result, agentErr := state.finalizeTurn(emit, ev, state.currentSessionID(), runtime)
				return result, agentErr, true
			}
			if state.applySessionEvent(id) {
				emit(domain.AgentEvent{
					Type:      domain.EventSessionStarted,
					Timestamp: now,
					SessionID: state.currentSessionID(),
					Message:   "session started",
				})
			}
			return domain.TurnResult{}, nil, false
		}

		switch rawEvent.Type {
		case "turn_start":
			runtime.sawAssistantMessageEnd = false

		case "message_update":
			delta, err := rawEvent.assistantDelta()
			if err != nil {
				malformed(err)
				return domain.TurnResult{}, nil, false
			}
			switch delta.Type {
			case "text_start", "text_delta", "thinking_start", "thinking_delta":
				runtime.work.ObserveAssistantOutput()
				runtime.spend.MarkModelReached()
			case "toolcall_start", "toolcall_end":
				runtime.work.ObserveToolActivity()
				runtime.spend.MarkModelReached()
			}

		case "message_end":
			msg, err := rawEvent.completedMessage()
			if err != nil {
				malformed(err)
				return domain.TurnResult{}, nil, false
			}
			if msg.Role != "assistant" {
				return domain.TurnResult{}, nil, false
			}
			runtime.sawAssistantMessageEnd = true
			runtime.spend.MarkModelReached()
			runtime.assistantFailure = msg.failure()
			if text := msg.text(); text != "" {
				runtime.work.ObserveAssistantOutput()
				agentcore.EmitNotification(emit, redact.Truncate(text, 500))
			}

		case "turn_end":
			msg, err := rawEvent.completedMessage()
			if err != nil {
				malformed(err)
				return domain.TurnResult{}, nil, false
			}
			// turn_end names the same assistant response the matching
			// message_end already reported, so it is the fallback for
			// the text and the only source of the response's usage.
			if msg.Role == "assistant" && !runtime.sawAssistantMessageEnd {
				runtime.assistantFailure = msg.failure()
				if text := msg.text(); text != "" {
					runtime.work.ObserveAssistantOutput()
					agentcore.EmitNotification(emit, redact.Truncate(text, 500))
				}
			}
			if msg.Role == "assistant" && msg.Usage != nil {
				runtime.spend.MarkModelReached()
				runtime.usage = addUsage(runtime.usage, msg.Usage.tokenUsage())
				if msg.Model != "" {
					runtime.usageModel = msg.Model
				}
			}

		case "agent_end":
			if !rawEvent.WillRetry {
				runtime.spend.MarkSettled()
			}

		case "tool_execution_start":
			if rawEvent.ToolCallID != "" {
				runtime.tools.Begin(rawEvent.ToolCallID, rawEvent.ToolName)
			}

		case "tool_execution_end":
			_, durationMS, _ := runtime.tools.End(rawEvent.ToolCallID)
			if rawEvent.ToolName == "" {
				malformed(fmt.Errorf("tool_execution_end carries no toolName"))
				return domain.TurnResult{}, nil, false
			}
			runtime.work.ObserveToolActivity()
			runtime.spend.MarkModelReached()
			if rawEvent.DurationMS > 0 {
				durationMS = rawEvent.DurationMS
			}
			var message string
			if rawEvent.IsError {
				message = redact.Truncate(rawEvent.resultText(), 500)
			}
			emit(domain.AgentEvent{
				Type:           domain.EventToolResult,
				Timestamp:      now,
				ToolName:       rawEvent.ToolName,
				ToolDurationMS: durationMS,
				ToolError:      rawEvent.IsError,
				Message:        message,
			})

		default:
			if !rawEvent.known() {
				malformed(fmt.Errorf("unknown pi event type %q", rawEvent.Type))
			}
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
			return a.finalizeExitedTurn(state, runtime, emit, exit)
		}

		select {
		case line, ok := <-lineCh:
			if !ok {
				lineCh = nil
				if readErr := runtime.reader.Err(); readErr != nil && !errors.Is(readErr, procutil.ErrStdoutAbandoned) {
					stoppedBeforeKill := runtime.group.Stopped()
					killTurnProcess(runtime)
					_ = waitForProcess(runtime)
					clearActive(state, runtime)

					ev := agentcore.TurnEvidence{Terminal: agentcore.TerminalCancelled, TerminalMessage: "turn cancelled"}
					if !stoppedBeforeKill {
						procutil.EmitWarnLines(runtime.stderrCollector.Lines(), state.logger())
						ev = agentcore.TurnEvidence{
							Terminal:          agentcore.TerminalFailure,
							TerminalErrorKind: domain.ErrResponseError,
							TerminalMessage:   "stdout read error",
							Cause:             readErr,
						}
					}
					result, agentErr := state.finalizeTurn(emit, ev, state.currentSessionID(), runtime)
					if agentErr != nil {
						return result, agentErr
					}
					return result, nil
				}
				if processExited {
					return a.finalizeExitedTurn(state, runtime, emit, exit)
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
				return a.finalizeExitedTurn(state, runtime, emit, exit)
			}

		case <-postExitC:
			if result, agentErr, done := drainLinesBounded(lineCh, runtime, postExitAt, handleLine); done {
				return result, agentErr
			}
			return a.finalizeExitedTurn(state, runtime, emit, exit)

		case <-ctxDone:
			killTurnProcess(runtime)
			if !waitForProcess(runtime).stopped {
				// The subprocess had exited on its own before the stop
				// reached it, so the turn finishes like one whose context
				// is live: the closed wait channel takes the exit arm next.
				ctxDone = nil
				continue
			}
			drainReaderBounded(runtime.reader, runtime.drainGrace)
			clearActive(state, runtime)
			ev := agentcore.TurnEvidence{Terminal: agentcore.TerminalCancelled, TerminalMessage: "turn cancelled"}
			result, agentErr := state.finalizeTurn(emit, ev, state.currentSessionID(), runtime)
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
				TerminalMessage:   "timed out waiting for first pi json event",
			}
			result, agentErr := state.finalizeTurn(emit, ev, state.currentSessionID(), runtime)
			if agentErr != nil {
				return result, agentErr
			}
			return result, nil
		}
	}
}

// StopSession marks the session closed and terminates any active subprocess.
func (a *PiAdapter) StopSession(ctx context.Context, session domain.Session) error {
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
// the subprocess has exited.
func (a *PiAdapter) finalizeExitedTurn(state *sessionState, runtime *turnRuntime, emit func(domain.AgentEvent), exit waitResult) (domain.TurnResult, error) {
	clearActive(state, runtime)
	stderrLines := runtime.stderrCollector.Lines()
	sessionID := state.currentSessionID()

	ev := agentcore.TurnEvidence{
		ExitObserved: true,
		ExitCode:     exit.exitCode,
		WaitErr:      exit.err,
	}
	ev.Work, ev.WorkDetail = runtime.work.Report()

	if !exit.stopped {
		ev.EarlyExit = runtime.output.ExitedBeforeOutput(state.target, exit.err).Report(runtime.stderrCollector)
	}

	sshFailed := state.target.RemoteCommand != "" && sshutil.ConnectionFailed(exit.exitCode)
	hasTerminalResult := runtime.assistantFailure != ""

	switch {
	case agentcore.ConnectionFailedForRequest(sshFailed, hasTerminalResult):
		connErr := agentcore.ConnectionFailedError()
		ev.Terminal = agentcore.TerminalFailure
		ev.TerminalErrorKind = connErr.Kind
		ev.TerminalMessage = connErr.Message
		ev.Cause = connErr.Err

	case exit.stopped:
		ev.Terminal = agentcore.TerminalCancelled
		ev.TerminalMessage = "turn cancelled"
		ev.Cause = nil

	case runtime.assistantFailure != "":
		// pi --mode json applies no exit-code check to a failed
		// response, so this branch is what keeps a 0-exit provider
		// failure from being recorded as a successful turn.
		procutil.EmitWarnLines(stderrLines, state.logger())
		ev.Terminal = agentcore.TerminalFailure
		ev.TerminalErrorKind = domain.ErrResponseError
		ev.TerminalMessage = runtime.assistantFailure
		if excerpt := stderrExcerpt(stderrLines); excerpt != "" {
			ev.TerminalMessage += ": " + excerpt
		}

	case !runtime.firstJSONSeen:
		procutil.EmitWarnLines(stderrLines, state.logger())

	case exit.err != nil || exit.exitCode != 0:
		procutil.EmitWarnLines(stderrLines, state.logger())
	}

	result, agentErr := state.finalizeTurn(emit, ev, sessionID, runtime)
	if agentErr != nil {
		return result, agentErr
	}
	return result, nil
}

// finalizeTurn folds this turn's own model responses into the session's
// run-cumulative total and ends the turn with the figure it reports. The
// figure is absent when the turn produced no usage at all.
func (s *sessionState) finalizeTurn(emit func(domain.AgentEvent), ev agentcore.TurnEvidence, sessionID string, runtime *turnRuntime) (domain.TurnResult, *domain.AgentError) {
	var recovered *agentcore.RecoveredUsage
	if runtime.usage != (domain.TokenUsage{}) {
		s.total = addUsage(s.total, runtime.usage)
		recovered = &agentcore.RecoveredUsage{Run: s.total, Model: runtime.usageModel}
	}
	return s.usage.Finalize(emit, s.logger(), ev, sessionID, 0, recovered, runtime.spend)
}

// checkSessionHeader validates a session header against the session's
// own identity and workspace, and returns the problem it found or "".
func (s *sessionState) checkSessionHeader(id, cwd string) string {
	if want := filepath.Clean(s.target.WorkspacePath); filepath.Clean(cwd) != want {
		return fmt.Sprintf("session %q runs in %q, want the turn's workspace %q", id, cwd, s.target.WorkspacePath)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessionID != "" && s.sessionID != id {
		return fmt.Sprintf("session id mismatch: expected %q, got %q", s.sessionID, id)
	}
	return ""
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

// applySessionEvent adopts the header's session id, and reports whether
// this is the first header of the Sortie session, so session_started is
// emitted exactly once even across a resume.
func (s *sessionState) applySessionEvent(eventSessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.sessionID == "" {
		s.sessionID = eventSessionID
	}
	if s.sessionOpened {
		return false
	}
	s.sessionOpened = true
	return true
}

// startWait reaps the turn's subprocess independently of its stdout
// reader and, once the reap and group kill have run, bounds the wait
// for the turn's stderr drain before reading it.
func startWait(runtime *turnRuntime, cmd *exec.Cmd, logger *slog.Logger) {
	go func() {
		reaper := procutil.StartReaper(cmd, logger)
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
			stopped:  reaper.Stopped(),
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

	if runtime.group != nil {
		_ = runtime.group.SignalGraceful() //nolint:errcheck // best-effort signal; process may already be dead
	}

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
	if runtime.group != nil {
		_ = runtime.group.Kill() //nolint:errcheck,gosec // best-effort cleanup
		return
	}
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

// stderrExcerptLines and stderrExcerptRunes bound how much of a failed
// turn's stderr reaches its terminal report.
const (
	stderrExcerptLines = 3
	stderrExcerptRunes = 300
)

// stderrExcerpt returns a bounded excerpt of the turn's stderr lines
// for its terminal failure message, or "" when the turn wrote none.
func stderrExcerpt(lines []string) string {
	var kept []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			kept = append(kept, trimmed)
		}
		if len(kept) == stderrExcerptLines {
			break
		}
	}
	return redact.Truncate(strings.Join(kept, "; "), stderrExcerptRunes)
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
