// Package opencode implements [domain.AgentAdapter] for the OpenCode CLI.
// It launches one `opencode run --format json` subprocess per turn,
// normalizes stdout envelopes into domain events, and recovers final token
// usage with `opencode export --sanitize`.
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
		RequiresCommand: true,
	})
}

var _ domain.AgentAdapter = (*OpenCodeAdapter)(nil)

type OpenCodeAdapter struct {
	passthrough passthroughConfig
}

type sessionState struct {
	target        agentcore.LaunchTarget
	agentConfig   domain.AgentConfig
	passthrough   passthroughConfig
	sessionID     string
	turnCount     int
	sessionOpened bool
	closed        bool
	baseLogger    *slog.Logger
	mu            sync.Mutex
	active        *turnRuntime
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
}

type waitResult struct {
	exitCode int
	err      error
}

// NewOpenCodeAdapter creates an [OpenCodeAdapter] from the raw "opencode"
// adapter configuration in WORKFLOW.md.
func NewOpenCodeAdapter(config map[string]any) (domain.AgentAdapter, error) {
	pt, err := parsePassthroughConfig(config)
	if err != nil {
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

	state := &sessionState{
		target:      target,
		agentConfig: params.AgentConfig,
		passthrough: a.passthrough,
		sessionID:   params.ResumeSessionID,
		baseLogger:  slog.Default().With(slog.String("component", "opencode-adapter")),
	}

	return domain.Session{
		ID:       params.WorkspacePath,
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
				if ctx.Err() != nil || state.isClosed() {
					closeStop(runtime)
					killTurnProcess(runtime)
					<-runtime.readerDone
					_ = waitForProcess(runtime)
					clearActive(state, runtime)
					agentcore.EmitTurnCancelled(emit, "turn cancelled")
					return domain.TurnResult{
						SessionID:  state.currentSessionID(),
						ExitReason: domain.EventTurnCancelled,
					}, nil
				}

				emitTurnEndedWithError(emit, "stdout read error")
				closeStop(runtime)
				killTurnProcess(runtime)
				<-runtime.readerDone
				_ = waitForProcess(runtime)
				procutil.EmitWarnLines(runtime.stderrCollector.Lines(), state.logger())
				clearActive(state, runtime)
				return domain.TurnResult{
						SessionID:  state.currentSessionID(),
						ExitReason: domain.EventTurnEndedWithError,
					}, &domain.AgentError{
						Kind:    domain.ErrResponseError,
						Message: "stdout read error",
						Err:     parsed.Err,
					}
			}

			if parsed.PlainText != "" {
				if readTimeoutC != nil {
					resetTimer(readTimer, readTimeout)
				}

				plainText := typeutil.TruncateRunes(parsed.PlainText, 500)
				if isPermissionWarning(parsed.PlainText) {
					agentcore.EmitNotification(emit, plainText)
				} else {
					emit(domain.AgentEvent{
						Type:      domain.EventMalformed,
						Timestamp: time.Now().UTC(),
						Message:   plainText,
					})
				}
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
				emitTurnEndedWithError(emit, message)
				closeStop(runtime)
				killTurnProcess(runtime)
				<-runtime.readerDone
				_ = waitForProcess(runtime)
				procutil.EmitWarnLines(runtime.stderrCollector.Lines(), state.logger())
				clearActive(state, runtime)
				return domain.TurnResult{
						SessionID:  state.currentSessionID(),
						ExitReason: domain.EventTurnEndedWithError,
					}, &domain.AgentError{
						Kind:    domain.ErrResponseError,
						Message: message,
					}
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
				agentcore.EmitNotification(emit, typeutil.TruncateRunes(part.Text, 500))

			case "reasoning":
				if _, err := parseReasoningPart(event.Part); err != nil {
					emit(domain.AgentEvent{Type: domain.EventMalformed, Timestamp: now, Message: "invalid reasoning payload"})
					continue
				}
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
				runtime.terminalError = event.Error
				agentcore.EmitTurnFailed(emit, rawRunErrorMessage(event.Error), 0)

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
			agentcore.EmitTurnCancelled(emit, "turn cancelled")
			return domain.TurnResult{
				SessionID:  state.currentSessionID(),
				ExitReason: domain.EventTurnCancelled,
			}, nil

		case <-readTimeoutC:
			emitTurnEndedWithError(emit, "timed out waiting for first opencode json event")
			closeStop(runtime)
			killTurnProcess(runtime)
			<-runtime.readerDone
			_ = waitForProcess(runtime)
			procutil.EmitWarnLines(runtime.stderrCollector.Lines(), state.logger())
			clearActive(state, runtime)
			return domain.TurnResult{
					SessionID:  state.currentSessionID(),
					ExitReason: domain.EventTurnEndedWithError,
				}, &domain.AgentError{
					Kind:    domain.ErrResponseTimeout,
					Message: "timed out waiting for first opencode json event",
				}
		}
	}
}

// StopSession marks the session closed and terminates any active subprocess.
func (a *OpenCodeAdapter) StopSession(_ context.Context, session domain.Session) error {
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
	state.mu.Unlock()

	if active == nil {
		return nil
	}

	closeStop(active)
	killTurnProcess(active)
	<-active.readerDone
	_ = waitForProcess(active)
	clearActive(state, active)

	return nil
}

// EventStream returns nil because OpenCode events are delivered via the
// RunTurn callback.
func (a *OpenCodeAdapter) EventStream() <-chan domain.AgentEvent {
	return nil
}

func (a *OpenCodeAdapter) finalizeExitedTurn(ctx context.Context, state *sessionState, runtime *turnRuntime, emit func(domain.AgentEvent), exit waitResult) (domain.TurnResult, error) {
	usage := queryExportUsage(ctx, state)
	usageSnapshot := domain.TokenUsage{
		InputTokens:     usage.InputTokens,
		OutputTokens:    usage.OutputTokens,
		TotalTokens:     usage.TotalTokens,
		CacheReadTokens: usage.CacheReadTokens,
	}
	if hasUsage(usage) {
		emit(domain.AgentEvent{
			Type:      domain.EventTokenUsage,
			Timestamp: time.Now().UTC(),
			Usage:     usageSnapshot,
			Model:     usage.Model,
		})
	}

	clearActive(state, runtime)
	stderrLines := runtime.stderrCollector.Lines()
	sessionID := state.currentSessionID()

	if runtime.terminalError != nil {
		procutil.EmitWarnLines(stderrLines, state.logger())
		return domain.TurnResult{
			SessionID:  sessionID,
			ExitReason: domain.EventTurnFailed,
			Usage:      usageSnapshot,
		}, nil
	}

	if ctx.Err() != nil {
		agentcore.EmitTurnCancelled(emit, "turn cancelled")
		return domain.TurnResult{
			SessionID:  sessionID,
			ExitReason: domain.EventTurnCancelled,
			Usage:      usageSnapshot,
		}, nil
	}

	if state.isClosed() {
		agentcore.EmitTurnCancelled(emit, "turn cancelled")
		return domain.TurnResult{
			SessionID:  sessionID,
			ExitReason: domain.EventTurnCancelled,
			Usage:      usageSnapshot,
		}, nil
	}

	if !runtime.firstJSONSeen {
		procutil.EmitWarnLines(stderrLines, state.logger())
		emitTurnEndedWithError(emit, "process exited before first opencode json event")
		return domain.TurnResult{
				SessionID:  sessionID,
				ExitReason: domain.EventTurnEndedWithError,
				Usage:      usageSnapshot,
			}, &domain.AgentError{
				Kind:    domain.ErrPortExit,
				Message: "process exited before first opencode json event",
				Err:     exit.err,
			}
	}

	if exit.err != nil || exit.exitCode != 0 {
		procutil.EmitWarnLines(stderrLines, state.logger())
		message := portExitMessage(exit)
		emitTurnEndedWithError(emit, message)
		return domain.TurnResult{
				SessionID:  sessionID,
				ExitReason: domain.EventTurnEndedWithError,
				Usage:      usageSnapshot,
			}, &domain.AgentError{
				Kind:    domain.ErrPortExit,
				Message: message,
				Err:     exit.err,
			}
	}

	agentcore.EmitTurnCompleted(emit, "", 0)
	return domain.TurnResult{
		SessionID:  sessionID,
		ExitReason: domain.EventTurnCompleted,
		Usage:      usageSnapshot,
	}, nil
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

func startWait(runtime *turnRuntime, cmd *exec.Cmd) {
	go func() {
		// Wait for the reader goroutine to finish before calling
		// cmd.Wait(). cmd.Wait() closes the stdout pipe read end, which
		// races with the scanner in startOpenCodeReader if called before
		// the reader has drained all buffered output.
		<-runtime.readerDone

		waitErr := cmd.Wait()
		procutil.KillProcessGroup(cmd.Process.Pid) //nolint:errcheck,gosec // best-effort cleanup of surviving group members
		procutil.CleanupProcess(cmd.Process.Pid)

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

func emitTurnEndedWithError(emit func(domain.AgentEvent), message string) {
	emit(domain.AgentEvent{
		Type:      domain.EventTurnEndedWithError,
		Timestamp: time.Now().UTC(),
		Message:   message,
	})
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

func portExitMessage(exit waitResult) string {
	if exit.exitCode > 0 {
		return fmt.Sprintf("opencode exited with code %d", exit.exitCode)
	}
	if exit.err != nil {
		return fmt.Sprintf("opencode exited unexpectedly: %v", exit.err)
	}
	return "opencode exited unexpectedly"
}

func hasUsage(usage exportUsage) bool {
	return usage.InputTokens > 0 || usage.OutputTokens > 0 || usage.TotalTokens > 0 || usage.CacheReadTokens > 0
}
