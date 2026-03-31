// Package copilot implements [domain.AgentAdapter] for the GitHub
// Copilot CLI. It launches the copilot binary as a subprocess in
// headless mode, reads newline-delimited JSON from stdout, and
// normalizes events into domain types. Registered under kind
// "copilot-cli" via an init function. Safe for concurrent use across
// sessions: callers may invoke [CopilotAdapter.RunTurn] concurrently
// for different [domain.Session] instances, but turns for a single
// session must be serialized.
package copilot

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/registry"
)

func init() {
	registry.Agents.RegisterWithMeta("copilot-cli", NewCopilotAdapter, registry.AdapterMeta{
		RequiresCommand: true,
	})
}

// Compile-time interface satisfaction check.
var _ domain.AgentAdapter = (*CopilotAdapter)(nil)

// inFlightTool tracks a tool execution that has started but whose
// corresponding tool.execution_complete has not yet arrived.
type inFlightTool struct {
	Name      string
	Timestamp time.Time
}

// CopilotAdapter satisfies [domain.AgentAdapter] by managing Copilot
// CLI subprocesses. One adapter instance serves all concurrent
// sessions; per-session state is held in [sessionState] via the
// [domain.Session] Internal field.
type CopilotAdapter struct {
	passthrough passthroughConfig
}

// sessionState is adapter-internal state stored in [domain.Session]
// Internal. It tracks the Copilot CLI session ID and subprocess
// handle across turns.
type sessionState struct {
	workspacePath    string
	command          string
	copilotSessionID string
	agentConfig      domain.AgentConfig
	turnCount        int

	// sshHost is the SSH destination for remote execution. Empty for
	// local mode.
	sshHost string

	// remoteCommand is the agent command to run on the remote host.
	// Empty for local mode.
	remoteCommand string

	// fallbackToContinue is set when a turn completes without a
	// result event containing a sessionId. On the next turn,
	// buildArgs uses --continue instead of --resume.
	fallbackToContinue bool

	// mu guards proc and waitCh for concurrent access from
	// StopSession and gracefulKill.
	mu     sync.Mutex
	proc   *os.Process
	waitCh chan struct{} // closed when cmd.Wait() completes; nil when no process is running
}

// NewCopilotAdapter creates a [CopilotAdapter] from adapter
// configuration. The config parameter is the raw map from the
// "copilot-cli" sub-object in WORKFLOW.md. Command resolution is
// deferred to [CopilotAdapter.StartSession].
func NewCopilotAdapter(config map[string]any) (domain.AgentAdapter, error) {
	pt := parsePassthroughConfig(config)
	return &CopilotAdapter{passthrough: pt}, nil
}

// StartSession validates the workspace path, resolves the copilot
// binary, and initializes per-session state. No subprocess is spawned;
// that happens in [CopilotAdapter.RunTurn].
func (a *CopilotAdapter) StartSession(ctx context.Context, params domain.StartSessionParams) (domain.Session, error) {
	if params.WorkspacePath == "" {
		return domain.Session{}, &domain.AgentError{
			Kind:    domain.ErrInvalidWorkspaceCwd,
			Message: "empty workspace path",
		}
	}

	absPath, err := filepath.Abs(params.WorkspacePath)
	if err != nil {
		return domain.Session{}, &domain.AgentError{
			Kind:    domain.ErrInvalidWorkspaceCwd,
			Message: "cannot resolve workspace path",
			Err:     err,
		}
	}

	info, err := os.Stat(absPath)
	if err != nil {
		return domain.Session{}, &domain.AgentError{
			Kind:    domain.ErrInvalidWorkspaceCwd,
			Message: "workspace path does not exist",
			Err:     err,
		}
	}
	if !info.IsDir() {
		return domain.Session{}, &domain.AgentError{
			Kind:    domain.ErrInvalidWorkspaceCwd,
			Message: "workspace path is not a directory",
		}
	}

	command := params.AgentConfig.Command
	if command == "" {
		command = "copilot"
	}

	var resolvedPath string
	var sshHost string
	var remoteCommand string

	sshHostTrimmed := strings.TrimSpace(params.SSHHost)
	if sshHostTrimmed != "" {
		// SSH mode: resolve "ssh" locally, skip local LookPath for
		// the agent command (it resolves on the remote host).
		sshPath, lookErr := exec.LookPath("ssh")
		if lookErr != nil {
			return domain.Session{}, &domain.AgentError{
				Kind:    domain.ErrAgentNotFound,
				Message: "ssh binary not found on orchestrator host",
				Err:     lookErr,
			}
		}
		resolvedPath = sshPath
		sshHost = sshHostTrimmed
		remoteCommand = command
	} else {
		var lookErr error
		resolvedPath, lookErr = exec.LookPath(command)
		if lookErr != nil {
			return domain.Session{}, &domain.AgentError{
				Kind:    domain.ErrAgentNotFound,
				Message: fmt.Sprintf("agent command %q not found", command),
				Err:     lookErr,
			}
		}

		// Canary check: validate the binary is functional (Node.js
		// 22+ runtime present). Use a 5-second timeout to avoid
		// hanging on a broken installation.
		canaryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		out, canaryErr := exec.CommandContext(canaryCtx, resolvedPath, "--version").CombinedOutput() //nolint:gosec // resolvedPath from LookPath
		if canaryErr != nil {
			return domain.Session{}, &domain.AgentError{
				Kind:    domain.ErrAgentNotFound,
				Message: "copilot binary found but not functional; ensure Node.js 22+ is available",
				Err:     canaryErr,
			}
		}
		slog.Debug("copilot version check passed", slog.String("version", strings.TrimSpace(string(out))))

		// Authentication preflight: verify at least one GitHub
		// authentication source is available.
		if authErr := checkAuth(ctx); authErr != nil {
			return domain.Session{}, authErr
		}
	}

	copilotSessionID := ""
	if params.ResumeSessionID != "" {
		copilotSessionID = params.ResumeSessionID
	}

	state := &sessionState{
		workspacePath:    absPath,
		command:          resolvedPath,
		copilotSessionID: copilotSessionID,
		agentConfig:      params.AgentConfig,
		sshHost:          sshHost,
		remoteCommand:    remoteCommand,
	}

	return domain.Session{
		ID:       copilotSessionID,
		Internal: state,
	}, nil
}

// checkAuth validates that at least one GitHub authentication source
// is available in the environment. Returns nil on success or an
// [domain.AgentError] if no source is found.
func checkAuth(ctx context.Context) error {
	for _, env := range []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		if strings.TrimSpace(os.Getenv(env)) != "" {
			return nil
		}
	}
	// No env var set. Check for gh CLI with valid auth as a fallback.
	if _, err := exec.LookPath("gh"); err == nil {
		authCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()

		cmd := exec.CommandContext(authCtx, "gh", "auth", "status") //nolint:gosec // fixed args
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard

		if err := cmd.Run(); err == nil && authCtx.Err() == nil {
			slog.Warn("no GitHub token env var set; relying on gh auth for Copilot CLI authentication")
			return nil
		}
	}
	return &domain.AgentError{
		Kind:    domain.ErrAgentNotFound,
		Message: "no GitHub authentication source found; set COPILOT_GITHUB_TOKEN, GH_TOKEN, or GITHUB_TOKEN, or run 'gh auth login' to authenticate",
	}
}

// RunTurn executes one agent turn by launching a Copilot CLI subprocess
// and reading JSONL events from stdout. Events are delivered
// synchronously via params.OnEvent.
//
// Subprocess lifecycle uses [exec.Command], not [exec.CommandContext].
// Context cancellation is monitored by a dedicated goroutine that calls
// gracefulKill, which sends SIGTERM and escalates to SIGKILL after 5
// seconds. This is intentional: [exec.CommandContext] sends SIGKILL
// (immediate, untrappable) on context cancellation by default, which
// denies the agent process any opportunity to flush output buffers or
// emit final token-usage events. The orchestrator's graceful shutdown
// relies on SIGTERM being delivered first so the agent can exit cleanly
// before the forced kill.
func (a *CopilotAdapter) RunTurn(ctx context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
	if params.OnEvent == nil {
		panic("copilot: OnEvent must be non-nil")
	}

	state := session.Internal.(*sessionState)
	baseLogger := slog.Default().With(slog.String("component", "copilot-adapter"))
	logger := logging.WithSession(baseLogger, state.copilotSessionID)

	args := buildArgs(state, params.Prompt, a.passthrough)

	var cmd *exec.Cmd
	if state.sshHost != "" {
		sshArgs := buildSSHArgs(state.sshHost, state.workspacePath, state.remoteCommand, args)
		cmd = exec.Command(state.command, sshArgs...) //nolint:gosec,noctx // args are constructed programmatically with shell quoting; context cancellation handled via gracefulKill goroutine
	} else {
		cmd = exec.Command(state.command, args...) //nolint:gosec,noctx // args are constructed programmatically; context cancellation handled via gracefulKill goroutine
	}
	cmd.Dir = state.workspacePath
	cmd.Env = os.Environ()

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return domain.TurnResult{}, &domain.AgentError{
			Kind:    domain.ErrPortExit,
			Message: "failed to create stdout pipe",
			Err:     err,
		}
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return domain.TurnResult{}, &domain.AgentError{
			Kind:    domain.ErrPortExit,
			Message: "failed to create stderr pipe",
			Err:     err,
		}
	}

	// Lock before Start to prevent a race with StopSession.
	state.mu.Lock()
	err = cmd.Start()
	if err != nil {
		state.mu.Unlock()
		return domain.TurnResult{}, &domain.AgentError{
			Kind:    domain.ErrPortExit,
			Message: "failed to start subprocess",
			Err:     err,
		}
	}
	state.proc = cmd.Process
	state.waitCh = make(chan struct{})
	waitCh := state.waitCh // local copy for cleanup closures
	state.mu.Unlock()

	state.turnCount++

	// Emit session_started before the scan loop begins. On turn 1
	// the session ID is empty — this is an accepted tradeoff since
	// Copilot CLI only reports the session ID in its result event.
	now := time.Now().UTC()
	params.OnEvent(domain.AgentEvent{
		Type:      domain.EventSessionStarted,
		Timestamp: now,
		AgentPID:  strconv.Itoa(cmd.Process.Pid),
		SessionID: state.copilotSessionID,
		Message:   "session started",
	})

	go drainStderr(stderrPipe, logger)

	// Monitor context cancellation for graceful shutdown.
	doneCh := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			gracefulKill(state)
		case <-doneCh:
		}
	}()

	// Read and parse stdout line by line.
	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	var lastResult *rawEvent
	inFlight := make(map[string]inFlightTool)
	var cumulativeOutputTokens int64

	for scanner.Scan() {
		line := scanner.Bytes()
		now = time.Now().UTC()

		event, parseErr := parseEvent(line)
		if parseErr != nil {
			params.OnEvent(domain.AgentEvent{
				Type:      domain.EventMalformed,
				Timestamp: now,
				Message:   truncate(string(line), 500),
			})
			continue
		}

		switch event.Type {
		case "assistant.message_delta":
			// Stall timer reset; ephemeral streaming content.
			params.OnEvent(domain.AgentEvent{
				Type:      domain.EventNotification,
				Timestamp: now,
			})

		case "assistant.message":
			if len(event.Data) > 0 {
				msgData, dataErr := parseAssistantMessageData(event.Data)
				if dataErr == nil {
					cumulativeOutputTokens += msgData.OutputTokens
					params.OnEvent(domain.AgentEvent{
						Type:      domain.EventTokenUsage,
						Timestamp: now,
						Usage:     normalizeUsage(nil, cumulativeOutputTokens),
					})
					params.OnEvent(domain.AgentEvent{
						Type:      domain.EventNotification,
						Timestamp: now,
						Message:   summarizeAssistantMessage(msgData),
					})
				} else {
					logger.Debug("failed to parse assistant.message data", slog.Any("error", dataErr))
					params.OnEvent(domain.AgentEvent{
						Type:      domain.EventNotification,
						Timestamp: now,
						Message:   "assistant message",
					})
				}
			}

		case "assistant.turn_start", "assistant.turn_end":
			params.OnEvent(domain.AgentEvent{
				Type:      domain.EventNotification,
				Timestamp: now,
				Message:   event.Type,
			})

		case "tool.execution_start":
			if len(event.Data) > 0 {
				toolData, dataErr := parseToolExecutionData(event.Data)
				if dataErr == nil {
					inFlight[toolData.ToolCallID] = inFlightTool{
						Name:      toolData.ToolName,
						Timestamp: time.Now(),
					}
					params.OnEvent(domain.AgentEvent{
						Type:      domain.EventNotification,
						Timestamp: now,
						Message:   fmt.Sprintf("tool started: %s", toolData.ToolName),
					})
				}
			}

		case "tool.execution_complete":
			if len(event.Data) > 0 {
				toolData, dataErr := parseToolExecutionData(event.Data)
				if dataErr == nil {
					toolName := toolData.ToolName
					var durationMS int64
					if entry, ok := inFlight[toolData.ToolCallID]; ok {
						toolName = entry.Name
						if d := time.Since(entry.Timestamp); d > 0 {
							durationMS = d.Milliseconds()
						}
						delete(inFlight, toolData.ToolCallID)
					}
					params.OnEvent(domain.AgentEvent{
						Type:           domain.EventToolResult,
						Timestamp:      now,
						ToolName:       toolName,
						ToolDurationMS: durationMS,
						ToolError:      !toolData.Success,
					})
				}
			}

		case "session.warning":
			msg := "session warning"
			if len(event.Data) > 0 {
				warnData, dataErr := parseSessionWarningData(event.Data)
				if dataErr == nil && warnData.Message != "" {
					msg = warnData.Message
				}
			}
			logger.Warn("copilot session warning", slog.String("message", msg))
			params.OnEvent(domain.AgentEvent{
				Type:      domain.EventNotification,
				Timestamp: now,
				Message:   msg,
			})

		case "session.info":
			msg := "session info"
			if len(event.Data) > 0 {
				infoData, dataErr := parseSessionInfoData(event.Data)
				if dataErr == nil && infoData.Message != "" {
					msg = infoData.Message
				}
			}
			params.OnEvent(domain.AgentEvent{
				Type:      domain.EventNotification,
				Timestamp: now,
				Message:   msg,
			})

		case "session.task_complete":
			msg := "task complete"
			if len(event.Data) > 0 {
				taskData, dataErr := parseSessionTaskCompleteData(event.Data)
				if dataErr == nil && taskData.Summary != "" {
					msg = taskData.Summary
				}
			}
			params.OnEvent(domain.AgentEvent{
				Type:      domain.EventNotification,
				Timestamp: now,
				Message:   msg,
			})

		case "session.mcp_server_status_changed", "session.mcp_servers_loaded",
			"session.tools_updated", "user.message":
			logger.Debug("copilot event logged only", slog.String("event_type", event.Type))

		case "result":
			captured := event
			lastResult = &captured

		default:
			params.OnEvent(domain.AgentEvent{
				Type:      domain.EventOtherMessage,
				Timestamp: now,
				Message:   event.Type,
			})
		}
	}

	// Check scanner error (e.g., buffer overflow).
	if scanErr := scanner.Err(); scanErr != nil {
		gracefulKill(state)
		cmd.Wait() //nolint:errcheck,gosec // best-effort reap; exit code is irrelevant on scanner failure
		close(waitCh)
		close(doneCh)
		state.mu.Lock()
		state.proc = nil
		state.waitCh = nil
		state.mu.Unlock()
		now = time.Now().UTC()
		params.OnEvent(domain.AgentEvent{
			Type:      domain.EventTurnFailed,
			Timestamp: now,
			Message:   "stdout read error: " + scanErr.Error(),
		})
		return domain.TurnResult{
				SessionID:  state.copilotSessionID,
				ExitReason: domain.EventTurnFailed,
				Usage:      normalizeUsage(nil, cumulativeOutputTokens),
			}, &domain.AgentError{
				Kind:    domain.ErrPortExit,
				Message: "stdout scanner error",
				Err:     scanErr,
			}
	}

	// Wait for process to exit.
	waitErr := cmd.Wait()
	close(waitCh)
	close(doneCh)

	// Clear subprocess reference.
	state.mu.Lock()
	state.proc = nil
	state.waitCh = nil
	state.mu.Unlock()

	now = time.Now().UTC()

	var resultUsage *rawUsage
	if lastResult != nil {
		resultUsage = lastResult.Usage
	}
	usage := normalizeUsage(resultUsage, cumulativeOutputTokens)

	// Determine exit reason.
	if ctx.Err() != nil {
		params.OnEvent(domain.AgentEvent{
			Type:      domain.EventTurnCancelled,
			Timestamp: now,
			Message:   "context cancelled",
		})
		return domain.TurnResult{
				SessionID:  state.copilotSessionID,
				ExitReason: domain.EventTurnCancelled,
				Usage:      usage,
			}, &domain.AgentError{
				Kind:    domain.ErrTurnCancelled,
				Message: "turn cancelled",
				Err:     ctx.Err(),
			}
	}

	exitCode := extractExitCode(waitErr)

	if exitCode == 127 {
		params.OnEvent(domain.AgentEvent{
			Type:      domain.EventTurnFailed,
			Timestamp: now,
			Message:   "copilot binary not found",
		})
		return domain.TurnResult{
				SessionID:  state.copilotSessionID,
				ExitReason: domain.EventTurnFailed,
				Usage:      usage,
			}, &domain.AgentError{
				Kind:    domain.ErrAgentNotFound,
				Message: "exit code 127",
			}
	}

	if wasSignaled(waitErr) {
		params.OnEvent(domain.AgentEvent{
			Type:      domain.EventTurnCancelled,
			Timestamp: now,
			Message:   "killed by signal",
		})
		return domain.TurnResult{
				SessionID:  state.copilotSessionID,
				ExitReason: domain.EventTurnCancelled,
				Usage:      usage,
			}, &domain.AgentError{
				Kind:    domain.ErrTurnCancelled,
				Message: "killed by signal",
			}
	}

	// Capture session ID from result event for subsequent turns.
	if lastResult != nil && lastResult.SessionID != "" {
		state.copilotSessionID = lastResult.SessionID
		state.fallbackToContinue = false
		logger = logging.WithSession(baseLogger, state.copilotSessionID)
	} else if state.copilotSessionID == "" {
		// No result event and no session ID from a prior turn.
		// Use --continue on the next turn to resume the most recent
		// conversation in the workspace directory.
		state.fallbackToContinue = true
	}

	if lastResult != nil {
		// Extract API duration from the result event. Attached to the
		// turn completion/failure event — not a separate EventTokenUsage —
		// to avoid inflating APIRequestCount in the orchestrator.
		var apiDurationMS int64
		if lastResult.Usage != nil {
			apiDurationMS = lastResult.Usage.TotalAPIDurMS
			logger.Info("copilot turn completed",
				slog.Int64("premium_requests", lastResult.Usage.PremiumRequests))
		}

		if lastResult.ExitCode != nil && *lastResult.ExitCode == 0 {
			params.OnEvent(domain.AgentEvent{
				Type:          domain.EventTurnCompleted,
				Timestamp:     now,
				APIDurationMS: apiDurationMS,
			})
			return domain.TurnResult{
				SessionID:  state.copilotSessionID,
				ExitReason: domain.EventTurnCompleted,
				Usage:      usage,
			}, nil
		}
		params.OnEvent(domain.AgentEvent{
			Type:          domain.EventTurnFailed,
			Timestamp:     now,
			Message:       "non-zero exit in result event",
			APIDurationMS: apiDurationMS,
		})
		return domain.TurnResult{
				SessionID:  state.copilotSessionID,
				ExitReason: domain.EventTurnFailed,
				Usage:      usage,
			}, &domain.AgentError{
				Kind:    domain.ErrTurnFailed,
				Message: "non-zero exit in result event",
			}
	}

	// No result event.
	if exitCode != 0 {
		params.OnEvent(domain.AgentEvent{
			Type:      domain.EventTurnFailed,
			Timestamp: now,
			Message:   "non-zero exit",
		})
		return domain.TurnResult{
				SessionID:  state.copilotSessionID,
				ExitReason: domain.EventTurnFailed,
				Usage:      usage,
			}, &domain.AgentError{
				Kind:    domain.ErrPortExit,
				Message: fmt.Sprintf("exit code %d", exitCode),
			}
	}

	// No result event and exit code 0 — treat as success.
	params.OnEvent(domain.AgentEvent{
		Type:      domain.EventTurnCompleted,
		Timestamp: now,
	})
	return domain.TurnResult{
		SessionID:  state.copilotSessionID,
		ExitReason: domain.EventTurnCompleted,
		Usage:      usage,
	}, nil
}

// StopSession terminates a running Copilot CLI subprocess gracefully.
// Sends SIGTERM, waits up to 5 seconds, then sends SIGKILL. Safe to
// call when no subprocess is running.
func (a *CopilotAdapter) StopSession(ctx context.Context, session domain.Session) error {
	state := session.Internal.(*sessionState)

	state.mu.Lock()
	proc := state.proc
	state.proc = nil
	waitCh := state.waitCh
	state.mu.Unlock()

	if proc == nil {
		return nil
	}

	// Send SIGTERM for graceful shutdown.
	_ = proc.Signal(syscall.SIGTERM) //nolint:errcheck // best-effort signal; process may already be dead

	select {
	case <-waitCh:
		return nil
	case <-time.After(5 * time.Second):
		_ = proc.Kill() //nolint:errcheck // best-effort kill
		return nil
	case <-ctx.Done():
		_ = proc.Kill() //nolint:errcheck // best-effort kill
		return ctx.Err()
	}
}

// gracefulKill sends SIGTERM and schedules a SIGKILL escalation after
// 5 seconds. It returns immediately; the caller (RunTurn) is
// responsible for calling cmd.Wait. Safe to call when proc is nil.
func gracefulKill(state *sessionState) {
	state.mu.Lock()
	proc := state.proc
	state.mu.Unlock()

	if proc == nil {
		return
	}

	_ = proc.Signal(syscall.SIGTERM) //nolint:errcheck // best-effort signal

	// Schedule SIGKILL escalation. The timer checks state.proc
	// under the lock; once RunTurn clears proc the kill is skipped.
	time.AfterFunc(5*time.Second, func() {
		state.mu.Lock()
		p := state.proc
		state.mu.Unlock()
		if p != nil {
			_ = p.Kill() //nolint:errcheck // best-effort kill
		}
	})
}

// drainStderr reads stderr from the subprocess line by line and logs
// each line at debug level. Returns when the pipe closes.
func drainStderr(r io.Reader, logger *slog.Logger) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		logger.Debug("agent stderr", slog.String("line", scanner.Text()))
	}
}

// extractExitCode returns the process exit code from an
// [*exec.ExitError], or -1 if the error is not an ExitError or is
// nil.
func extractExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// wasSignaled returns true if the process was terminated by a signal.
func wasSignaled(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok {
		return false
	}
	return status.Signaled()
}

// EventStream returns nil. The Copilot CLI adapter delivers events
// synchronously via the [domain.RunTurnParams] OnEvent callback.
func (a *CopilotAdapter) EventStream() <-chan domain.AgentEvent {
	return nil
}
