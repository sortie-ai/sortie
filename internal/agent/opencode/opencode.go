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
	"bufio"
	"context"
	"fmt"
	"io"
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
	acc            *agentcore.RunUsage
	mu             sync.Mutex
	active         *turnRuntime

	// mcpConfigContent is the translated MCP configuration document
	// delivered through the runtime's inline configuration environment
	// variable on every turn's subprocess. Empty when the session
	// carries no generated configuration, when that configuration
	// declares no server, or when the launch target is remote. Set
	// once in StartSession and never mutated after.
	mcpConfigContent string

	// usageMeasured reports whether a session export has yielded a
	// usage figure for this run. Monotone: set true once and never
	// cleared.
	usageMeasured bool
}

type turnRuntime struct {
	pid             string
	proc            *os.Process
	waitCh          chan waitResult
	lineCh          chan parsedLine
	readerDone      chan struct{}
	stopCh          chan struct{}
	stopOnce        sync.Once
	stderrCollector *procutil.StderrCollector
	firstJSONSeen   bool
	terminalError   *rawRunError
	terminalOutcome domain.AgentEventType
	waitMu          sync.Mutex
	waitRes         waitResult

	// assistantOutputSeen is true once at least one text, reasoning, or
	// tool_use part has been parsed during this turn. It is the per-turn
	// work signal for the shared turn-disposition decision.
	assistantOutputSeen bool
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
		acc:              agentcore.NewRunUsage(),
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
	procutil.SetProcessGroup(cmd)
	cmd.Cancel = func() error {
		return procutil.SignalGraceful(cmd.Process.Pid)
	}
	cmd.WaitDelay = 5 * time.Second
	cmd.Dir = state.target.WorkspacePath
	cmd.Env = env

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		state.mu.Unlock()
		return domain.TurnResult{}, &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: "create stdout pipe",
			Err:     err,
		}
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		state.mu.Unlock()
		return domain.TurnResult{}, &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: "create stderr pipe",
			Err:     err,
		}
	}

	if err := cmd.Start(); err != nil {
		state.mu.Unlock()
		return domain.TurnResult{}, &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: "start opencode subprocess",
			Err:     err,
		}
	}

	runtime := &turnRuntime{
		pid:             strconv.Itoa(cmd.Process.Pid),
		proc:            cmd.Process,
		waitCh:          make(chan waitResult, 1),
		lineCh:          make(chan parsedLine, 16),
		readerDone:      make(chan struct{}),
		stopCh:          make(chan struct{}),
		terminalOutcome: domain.EventTurnCompleted,
	}
	state.active = runtime
	state.mu.Unlock()

	if assignErr := procutil.AssignProcess(cmd.Process.Pid, cmd.Process); assignErr != nil {
		logger.Warn("process group assignment failed", slog.Any("error", assignErr))
	}

	runtime.stderrCollector = procutil.NewStderrCollector(stderrPipe, logger)
	startOpenCodeReader(stdoutPipe, runtime)
	startWait(runtime, cmd, procutil.DefaultDrainGrace)

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
	lineCh := runtime.lineCh
	waitCh := runtime.waitCh
	var exit waitResult
	processExited := false

	for {
		select {
		case parsed, ok := <-lineCh:
			if !ok {
				lineCh = nil
				if processExited {
					return a.finalizeExitedTurn(ctx, state, runtime, emit, exit)
				}
				continue
			}

			if parsed.Err != nil {
				closeStop(runtime)
				killTurnProcess(runtime)
				<-runtime.readerDone
				_ = waitForProcess(runtime)
				clearActive(state, runtime)

				ev := agentcore.TurnEvidence{Terminal: agentcore.TerminalCancelled, TerminalMessage: "turn cancelled"}
				if ctx.Err() == nil && !state.isClosed() {
					procutil.EmitWarnLines(runtime.stderrCollector.Lines(), state.logger())
					ev = agentcore.TurnEvidence{
						Terminal:          agentcore.TerminalFailure,
						TerminalErrorKind: domain.ErrResponseError,
						TerminalMessage:   "stdout read error",
						Cause:             parsed.Err,
					}
				}
				meta := agentcore.TurnMeta{SessionID: state.currentSessionID(), Usage: state.acc.Snapshot(), UsageMeasured: state.usageMeasured}
				result, agentErr := agentcore.FinalizeTurn(emit, state.logger(), ev, meta)
				if agentErr != nil {
					return result, agentErr
				}
				return result, nil
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
				continue
			}

			event := parsed.Event
			if event == nil {
				continue
			}

			if readTimeoutC != nil {
				runtime.firstJSONSeen = true
				stopTimer(readTimer)
				readTimeoutC = nil
			}

			started, mismatch := state.applySessionEvent(event.SessionID)
			if mismatch {
				message := fmt.Sprintf("session id mismatch: expected %q, got %q", state.currentSessionID(), event.SessionID)
				closeStop(runtime)
				killTurnProcess(runtime)
				<-runtime.readerDone
				_ = waitForProcess(runtime)
				procutil.EmitWarnLines(runtime.stderrCollector.Lines(), state.logger())
				clearActive(state, runtime)
				ev := agentcore.TurnEvidence{
					Terminal:          agentcore.TerminalFailure,
					TerminalErrorKind: domain.ErrResponseError,
					TerminalMessage:   message,
				}
				meta := agentcore.TurnMeta{SessionID: state.currentSessionID(), Usage: state.acc.Snapshot(), UsageMeasured: state.usageMeasured}
				result, agentErr := agentcore.FinalizeTurn(emit, state.logger(), ev, meta)
				if agentErr != nil {
					return result, agentErr
				}
				return result, nil
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
			switch event.Type {
			case "step_start":
				if _, err := parseStepStartPart(event.Part); err != nil {
					emit(domain.AgentEvent{Type: domain.EventMalformed, Timestamp: now, Message: "invalid step_start payload"})
					continue
				}
				agentcore.EmitNotification(emit, "step started")

			case "text":
				part, err := parseTextPart(event.Part)
				if err != nil {
					emit(domain.AgentEvent{Type: domain.EventMalformed, Timestamp: now, Message: "invalid text payload"})
					continue
				}
				runtime.assistantOutputSeen = true
				agentcore.EmitNotification(emit, typeutil.TruncateRunes(part.Text, 500))

			case "reasoning":
				if _, err := parseReasoningPart(event.Part); err != nil {
					emit(domain.AgentEvent{Type: domain.EventMalformed, Timestamp: now, Message: "invalid reasoning payload"})
					continue
				}
				runtime.assistantOutputSeen = true
				emit(domain.AgentEvent{
					Type:      domain.EventOtherMessage,
					Timestamp: now,
					Message:   "reasoning block",
				})

			case "tool_use":
				part, err := parseToolPart(event.Part)
				if err != nil {
					emit(domain.AgentEvent{Type: domain.EventMalformed, Timestamp: now, Message: "invalid tool_use payload"})
					continue
				}
				runtime.assistantOutputSeen = true
				emit(domain.AgentEvent{
					Type:           domain.EventToolResult,
					Timestamp:      now,
					ToolName:       part.Tool,
					ToolDurationMS: toolDuration(part.State.Time),
					ToolError:      strings.EqualFold(part.State.Status, "error"),
					Message:        typeutil.TruncateRunes(part.State.Error, 500),
				})

			case "step_finish":
				part, err := parseStepFinishPart(event.Part)
				if err != nil {
					emit(domain.AgentEvent{Type: domain.EventMalformed, Timestamp: now, Message: "invalid step_finish payload"})
					continue
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
				if runtime.terminalError == nil || !isMaskedServerError(rawRunErrorMessage(event.Error)) {
					runtime.terminalError = event.Error
				}

			default:
				emit(domain.AgentEvent{
					Type:      domain.EventMalformed,
					Timestamp: now,
					Message:   fmt.Sprintf("unknown event type: %s", event.Type),
				})
			}

		case <-waitCh:
			exit = waitForProcess(runtime)
			processExited = true
			waitCh = nil
			if lineCh == nil {
				return a.finalizeExitedTurn(ctx, state, runtime, emit, exit)
			}

		case <-ctx.Done():
			closeStop(runtime)
			killTurnProcess(runtime)
			<-runtime.readerDone
			_ = waitForProcess(runtime)
			clearActive(state, runtime)
			ev := agentcore.TurnEvidence{Terminal: agentcore.TerminalCancelled, TerminalMessage: "turn cancelled"}
			meta := agentcore.TurnMeta{SessionID: state.currentSessionID(), Usage: state.acc.Snapshot(), UsageMeasured: state.usageMeasured}
			result, agentErr := agentcore.FinalizeTurn(emit, state.logger(), ev, meta)
			if agentErr != nil {
				return result, agentErr
			}
			return result, nil

		case <-readTimeoutC:
			closeStop(runtime)
			killTurnProcess(runtime)
			<-runtime.readerDone
			_ = waitForProcess(runtime)
			procutil.EmitWarnLines(runtime.stderrCollector.Lines(), state.logger())
			clearActive(state, runtime)
			ev := agentcore.TurnEvidence{
				Terminal:          agentcore.TerminalFailure,
				TerminalErrorKind: domain.ErrResponseTimeout,
				TerminalMessage:   "timed out waiting for first opencode json event",
			}
			meta := agentcore.TurnMeta{SessionID: state.currentSessionID(), Usage: state.acc.Snapshot(), UsageMeasured: state.usageMeasured}
			result, agentErr := agentcore.FinalizeTurn(emit, state.logger(), ev, meta)
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

	return stopActiveTurn(ctx, active)
}

// EventStream returns nil because OpenCode events are delivered via the
// RunTurn callback.
func (a *OpenCodeAdapter) EventStream() <-chan domain.AgentEvent {
	return nil
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

	// A failed or empty export must not lower the run's previously
	// reported snapshot; skip the update entirely rather than replacing
	// settled with the zero value.
	snapshot := state.acc.Snapshot()
	if hasUsage(usage) {
		state.usageMeasured = true
		snapshot = state.acc.SetRunCumulative(domain.TokenUsage{
			InputTokens:     usage.InputTokens,
			OutputTokens:    usage.OutputTokens,
			CacheReadTokens: usage.CacheReadTokens,
		})
		emit(domain.AgentEvent{
			Type:      domain.EventTokenUsage,
			Timestamp: time.Now().UTC(),
			Usage:     snapshot,
			Model:     usage.Model,
		})
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

	// Work reflects this turn's own parsed parts, not the run-cumulative
	// export figure, which is non-zero on any turn after the first.
	ev := agentcore.TurnEvidence{
		ExitObserved: true,
		ExitCode:     exit.exitCode,
		Cause:        exit.err,
		Work:         agentcore.WorkAbsent,
		WorkDetail:   "no assistant output on the run stream",
	}
	if runtime.assistantOutputSeen {
		ev.Work = agentcore.WorkPresent
	}

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

	meta := agentcore.TurnMeta{
		SessionID:     sessionID,
		Usage:         snapshot,
		UsageMeasured: state.usageMeasured,
	}

	result, agentErr := agentcore.FinalizeTurn(emit, state.logger(), ev, meta)
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

func startOpenCodeReader(stdout io.Reader, runtime *turnRuntime) {
	go func() {
		defer close(runtime.lineCh)
		defer close(runtime.readerDone)

		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes)

		for scanner.Scan() {
			line := scanner.Bytes()
			event, err := parseRunEvent(line)
			parsed := parsedLine{}
			if err != nil {
				parsed.PlainText = string(line)
			} else {
				parsed.Event = &event
			}

			select {
			case runtime.lineCh <- parsed:
			case <-runtime.stopCh:
				return
			}
		}

		if err := scanner.Err(); err != nil {
			select {
			case runtime.lineCh <- parsedLine{Err: err}:
			case <-runtime.stopCh:
			}
		}
	}()
}

// startWait reaps the turn's subprocess. grace bounds the wait for the
// turn's stderr drain, once before cmd.Wait is called and once after
// the process group is killed.
func startWait(runtime *turnRuntime, cmd *exec.Cmd, grace time.Duration) {
	go func() {
		// Wait for the stdout reader to finish before calling cmd.Wait().
		// cmd.Wait() closes the stdout and stderr pipe read ends after
		// reaping the process, which races with a scanner still reading
		// buffered output on either stream if called first; for stderr
		// this can silently drop a permission-refusal warning that
		// finalizeExitedTurn depends on. The stderr wait is bounded so a
		// descendant that inherits the stderr handle and outlives the
		// direct child cannot withhold the reap.
		<-runtime.readerDone
		drained := runtime.stderrCollector.WaitDone(grace)

		waitErr := cmd.Wait()
		procutil.KillProcessGroup(cmd.Process.Pid) //nolint:errcheck,gosec // best-effort cleanup of surviving group members
		procutil.CleanupProcess(cmd.Process.Pid)

		if !drained {
			if !runtime.stderrCollector.WaitDone(grace) {
				runtime.stderrCollector.Abandon(grace)
			}
		}

		runtime.waitMu.Lock()
		runtime.waitRes = waitResult{
			exitCode: procutil.ExtractExitCode(waitErr),
			err:      waitErr,
		}
		runtime.waitMu.Unlock()

		close(runtime.waitCh)
	}()
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

func closeStop(runtime *turnRuntime) {
	runtime.stopOnce.Do(func() {
		close(runtime.stopCh)
	})
}

func stopActiveTurn(ctx context.Context, runtime *turnRuntime) error {
	if runtime == nil {
		return nil
	}

	closeStop(runtime)
	if runtime.proc == nil {
		return nil
	}

	_ = procutil.SignalGraceful(runtime.proc.Pid) //nolint:errcheck // best-effort signal; process may already be dead

	graceTimer := time.NewTimer(5 * time.Second)
	defer stopTimer(graceTimer)

	select {
	case <-runtime.waitCh:
		return nil
	case <-graceTimer.C:
		killTurnProcess(runtime)
		return nil
	case <-ctx.Done():
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
