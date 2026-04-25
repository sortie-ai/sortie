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
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
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
)

func init() {
	registry.Agents.RegisterWithMeta("copilot-cli", NewCopilotAdapter, registry.AgentMeta{
		RequiresCommand: true,
	})
}

// Compile-time interface satisfaction check.
var _ domain.AgentAdapter = (*CopilotAdapter)(nil)

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
	target           agentcore.LaunchTarget
	copilotSessionID string
	agentConfig      domain.AgentConfig
	turnCount        int

	// mcpConfigPath is the worker-generated MCP config file path.
	mcpConfigPath string

	// fallbackToContinue is set when a turn completes without a
	// result event containing a sessionId. On the next turn,
	// buildArgs uses --continue instead of --resume.
	fallbackToContinue bool

	// mu guards proc and waitCh for concurrent access from
	// StopSession and the cmd.Cancel callback.
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

// StartSession validates the workspace path, resolves the copilot binary, and
// initializes per-session state. No subprocess is spawned; that happens in
// [CopilotAdapter.RunTurn].
func (a *CopilotAdapter) StartSession(ctx context.Context, params domain.StartSessionParams) (domain.Session, error) {
	target, agentErr := agentcore.ResolveLaunchTarget(params, "copilot")
	if agentErr != nil {
		return domain.Session{}, agentErr
	}

	// Canary check and auth preflight are local-mode only.
	if target.RemoteCommand == "" {
		canaryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		out, canaryErr := exec.CommandContext(canaryCtx, target.Command, "--version").CombinedOutput() //nolint:gosec // target.Command from LookPath
		if canaryErr != nil {
			return domain.Session{}, &domain.AgentError{
				Kind:    domain.ErrAgentNotFound,
				Message: "copilot binary found but not functional; ensure Node.js 22+ is available",
				Err:     canaryErr,
			}
		}
		slog.Debug("copilot version check passed", slog.String("version", strings.TrimSpace(string(out))))

		if authErr := checkAuth(ctx); authErr != nil {
			return domain.Session{}, authErr
		}
	}

	copilotSessionID := ""
	if params.ResumeSessionID != "" {
		copilotSessionID = params.ResumeSessionID
	}

	state := &sessionState{
		target:           target,
		copilotSessionID: copilotSessionID,
		agentConfig:      params.AgentConfig,
		mcpConfigPath:    params.MCPConfigPath,
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
// The subprocess is placed in its own process group via
// [procutil.SetProcessGroup]. On context cancellation, [exec.Cmd].Cancel
// sends a platform-appropriate graceful shutdown signal to the entire
// process group via [procutil.SignalGraceful], and [exec.Cmd].WaitDelay
// of 5 seconds provides a grace period before Go force-kills the group
// leader. After [exec.Cmd.Wait] returns, a best-effort force kill is
// sent to the group via [procutil.KillProcessGroup] to reap any
// children that survived the graceful signal.
func (a *CopilotAdapter) RunTurn(ctx context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
	if params.OnEvent == nil {
		panic("copilot: OnEvent must be non-nil")
	}

	state, ok := session.Internal.(*sessionState)
	if !ok {
		return domain.TurnResult{}, fmt.Errorf("unexpected session internal type %T", session.Internal)
	}
	baseLogger := slog.Default().With(slog.String("component", "copilot-adapter"))
	logger := logging.WithSession(baseLogger, state.copilotSessionID)

	args := buildArgs(state, params.Prompt, a.passthrough)

	cmdCtx, cancelCmd := context.WithCancel(ctx)
	defer cancelCmd()

	var cmd *exec.Cmd
	if state.target.RemoteCommand != "" {
		sshArgs := sshutil.BuildSSHArgs(state.target.SSHHost, state.target.WorkspacePath, state.target.RemoteCommand, args, sshutil.SSHOptions{
			StrictHostKeyChecking: state.target.SSHStrictHostKeyChecking,
		})
		cmd = exec.CommandContext(cmdCtx, state.target.Command, sshArgs...) //nolint:gosec // args are constructed programmatically with shell quoting
	} else {
		allArgs := append(state.target.Args, args...)                       //nolint:gocritic // intentional: target.Args has cap==len so append always allocates
		cmd = exec.CommandContext(cmdCtx, state.target.Command, allArgs...) //nolint:gosec // args are constructed programmatically
	}
	procutil.SetProcessGroup(cmd)
	cmd.Cancel = func() error {
		return procutil.SignalGraceful(cmd.Process.Pid)
	}
	cmd.WaitDelay = 5 * time.Second
	cmd.Dir = state.target.WorkspacePath
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
		if ctx.Err() != nil {
			agentcore.EmitTurnCancelled(params.OnEvent, "context cancelled")
			return domain.TurnResult{
					SessionID:  state.copilotSessionID,
					ExitReason: domain.EventTurnCancelled,
				}, &domain.AgentError{
					Kind:    domain.ErrTurnCancelled,
					Message: "turn cancelled",
					Err:     ctx.Err(),
				}
		}
		return domain.TurnResult{}, &domain.AgentError{
			Kind:    domain.ErrPortExit,
			Message: "failed to start subprocess",
			Err:     err,
		}
	}
	if assignErr := procutil.AssignProcess(cmd.Process.Pid, cmd.Process); assignErr != nil {
		logger.Warn("process group assignment failed", slog.Any("error", assignErr))
	}
	state.proc = cmd.Process
	state.waitCh = make(chan struct{})
	waitCh := state.waitCh // local copy for cleanup closures
	state.mu.Unlock()

	state.turnCount++

	// Emit session_started before the scan loop begins. On turn 1
	// the session ID is empty — this is an accepted tradeoff since
	// Copilot CLI only reports the session ID in its result event.
	agentcore.EmitSessionStarted(params.OnEvent, strconv.Itoa(cmd.Process.Pid), state.copilotSessionID)

	stderrCollector := procutil.NewStderrCollector(stderrPipe, logger)

	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	var lastResult *rawEvent
	inFlight := agentcore.NewToolTracker()
	acc := agentcore.NewUsageAccumulator()

	for scanner.Scan() {
		line := scanner.Bytes()
		now := time.Now().UTC()

		event, parseErr := parseEvent(line)
		if parseErr != nil {
			agentcore.EmitMalformed(params.OnEvent, line)
			continue
		}

		switch event.Type {
		case "assistant.message_delta":
			// Stall timer reset; ephemeral streaming content.
			agentcore.EmitNotification(params.OnEvent, "")

		case "assistant.message":
			if len(event.Data) > 0 {
				msgData, dataErr := parseAssistantMessageData(event.Data)
				if dataErr == nil {
					snapshot, _ := acc.AddDelta(0, msgData.OutputTokens, 0)
					params.OnEvent(domain.AgentEvent{
						Type:      domain.EventTokenUsage,
						Timestamp: now,
						Usage:     snapshot,
					})
					agentcore.EmitNotification(params.OnEvent, summarizeAssistantMessage(msgData))
				} else {
					logger.Debug("failed to parse assistant.message data", slog.Any("error", dataErr))
					agentcore.EmitNotification(params.OnEvent, "assistant message")
				}
			}

		case "assistant.turn_start", "assistant.turn_end":
			agentcore.EmitNotification(params.OnEvent, event.Type)

		case "tool.execution_start":
			if len(event.Data) > 0 {
				toolData, dataErr := parseToolExecutionData(event.Data)
				if dataErr == nil {
					inFlight.Begin(toolData.ToolCallID, toolData.ToolName)
					agentcore.EmitNotification(params.OnEvent, fmt.Sprintf("tool started: %s", toolData.ToolName))
				}
			}

		case "tool.execution_complete":
			if len(event.Data) > 0 {
				toolData, dataErr := parseToolExecutionData(event.Data)
				if dataErr == nil {
					toolName, durationMS, ok := inFlight.End(toolData.ToolCallID)
					if !ok {
						toolName = toolData.ToolName
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
			agentcore.EmitNotification(params.OnEvent, msg)

		case "session.info":
			msg := "session info"
			if len(event.Data) > 0 {
				infoData, dataErr := parseSessionInfoData(event.Data)
				if dataErr == nil && infoData.Message != "" {
					msg = infoData.Message
				}
			}
			agentcore.EmitNotification(params.OnEvent, msg)

		case "session.task_complete":
			msg := "task complete"
			if len(event.Data) > 0 {
				taskData, dataErr := parseSessionTaskCompleteData(event.Data)
				if dataErr == nil && taskData.Summary != "" {
					msg = taskData.Summary
				}
			}
			agentcore.EmitNotification(params.OnEvent, msg)

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

	if scanErr := scanner.Err(); scanErr != nil {
		cancelCmd()
		stderrLines := stderrCollector.Lines()     // drain before cmd.Wait() closes the pipe
		cmd.Wait()                                 //nolint:errcheck,gosec // best-effort reap; exit code is irrelevant on scanner failure
		procutil.KillProcessGroup(cmd.Process.Pid) //nolint:errcheck,gosec // best-effort cleanup of surviving group members
		procutil.CleanupProcess(cmd.Process.Pid)
		close(waitCh)
		state.mu.Lock()
		state.proc = nil
		state.waitCh = nil
		state.mu.Unlock()

		// Context cancellation propagates through exec.CommandContext
		// and can surface as a pipe read error. Treat as cancellation.
		if ctx.Err() != nil {
			agentcore.EmitTurnCancelled(params.OnEvent, "context cancelled")
			return domain.TurnResult{
					SessionID:  state.copilotSessionID,
					ExitReason: domain.EventTurnCancelled,
					Usage:      acc.Snapshot(),
				}, &domain.AgentError{
					Kind:    domain.ErrTurnCancelled,
					Message: "turn cancelled",
					Err:     ctx.Err(),
				}
		}

		procutil.EmitWarnLines(stderrLines, logger)
		agentcore.EmitTurnFailed(params.OnEvent, "stdout read error: "+scanErr.Error(), 0)
		return domain.TurnResult{
				SessionID:  state.copilotSessionID,
				ExitReason: domain.EventTurnFailed,
				Usage:      acc.Snapshot(),
			}, &domain.AgentError{
				Kind:    domain.ErrPortExit,
				Message: "stdout scanner error",
				Err:     scanErr,
			}
	}

	// Drain stderr before cmd.Wait() to avoid losing buffered data:
	// cmd.Wait() closes the pipe read end, which can prevent the drain
	// goroutine from reading data that the process already wrote.
	stderrLines := stderrCollector.Lines()

	waitErr := cmd.Wait()
	procutil.KillProcessGroup(cmd.Process.Pid) //nolint:errcheck,gosec // best-effort cleanup of surviving group members
	procutil.CleanupProcess(cmd.Process.Pid)
	close(waitCh)

	state.mu.Lock()
	state.proc = nil
	state.waitCh = nil
	state.mu.Unlock()

	usage := acc.Snapshot()

	if ctx.Err() != nil {
		agentcore.EmitTurnCancelled(params.OnEvent, "context cancelled")
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

	exitCode := procutil.ExtractExitCode(waitErr)

	if exitCode == 127 {
		procutil.EmitWarnLines(stderrLines, logger)
		agentcore.EmitTurnFailed(params.OnEvent, "copilot binary not found", 0)
		return domain.TurnResult{
				SessionID:  state.copilotSessionID,
				ExitReason: domain.EventTurnFailed,
				Usage:      usage,
			}, &domain.AgentError{
				Kind:    domain.ErrAgentNotFound,
				Message: "exit code 127",
			}
	}

	if procutil.WasSignaled(waitErr) {
		agentcore.EmitTurnCancelled(params.OnEvent, "killed by signal")
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
			agentcore.EmitTurnCompleted(params.OnEvent, "", apiDurationMS)
			return domain.TurnResult{
				SessionID:  state.copilotSessionID,
				ExitReason: domain.EventTurnCompleted,
				Usage:      usage,
			}, nil
		}
		procutil.EmitWarnLines(stderrLines, logger)
		agentcore.EmitTurnFailed(params.OnEvent, "non-zero exit in result event", apiDurationMS)
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
		procutil.EmitWarnLines(stderrLines, logger)
		agentcore.EmitTurnFailed(params.OnEvent, "non-zero exit", 0)
		return domain.TurnResult{
				SessionID:  state.copilotSessionID,
				ExitReason: domain.EventTurnFailed,
				Usage:      usage,
			}, &domain.AgentError{
				Kind:    domain.ErrPortExit,
				Message: fmt.Sprintf("exit code %d", exitCode),
			}
	}

	// No result event and exit code 0.
	if acc.Snapshot().OutputTokens == 0 {
		procutil.EmitWarnLines(stderrLines, logger)
		logger.Warn("agent exited without producing output, treating as failure")
		agentcore.EmitTurnFailed(params.OnEvent, "agent exited without producing output", 0)
		return domain.TurnResult{
				SessionID:  state.copilotSessionID,
				ExitReason: domain.EventTurnFailed,
				Usage:      usage,
			}, &domain.AgentError{
				Kind:    domain.ErrTurnFailed,
				Message: "agent exited without producing output",
			}
	}

	agentcore.EmitTurnCompleted(params.OnEvent, "", 0)
	return domain.TurnResult{
		SessionID:  state.copilotSessionID,
		ExitReason: domain.EventTurnCompleted,
		Usage:      usage,
	}, nil
}

// StopSession terminates a running Copilot CLI subprocess gracefully.
// Sends a platform-appropriate graceful shutdown signal via
// [procutil.SignalGraceful], waits up to 5 seconds, then force-kills
// the process group via [procutil.KillProcessGroup]. Safe to call when
// no subprocess is running.
func (a *CopilotAdapter) StopSession(ctx context.Context, session domain.Session) error {
	state, ok := session.Internal.(*sessionState)
	if !ok {
		return fmt.Errorf("unexpected session internal type %T", session.Internal)
	}

	state.mu.Lock()
	proc := state.proc
	state.proc = nil
	waitCh := state.waitCh
	state.mu.Unlock()

	if proc == nil {
		return nil
	}

	// Send a graceful shutdown signal to the process group. Process
	// may already be dead; signal is best-effort.
	_ = procutil.SignalGraceful(proc.Pid)

	select {
	case <-waitCh:
		return nil
	case <-time.After(5 * time.Second):
		_ = procutil.KillProcessGroup(proc.Pid) //nolint:errcheck // best-effort kill
		return nil
	case <-ctx.Done():
		_ = procutil.KillProcessGroup(proc.Pid) //nolint:errcheck // best-effort kill
		return ctx.Err()
	}
}

// EventStream returns nil. The Copilot CLI adapter delivers events
// synchronously via the [domain.RunTurnParams] OnEvent callback.
func (a *CopilotAdapter) EventStream() <-chan domain.AgentEvent {
	return nil
}
