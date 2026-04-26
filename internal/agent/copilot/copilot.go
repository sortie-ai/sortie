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
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
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
// Internal. It tracks the Copilot CLI session ID and per-turn scan
// state across turns.
type sessionState struct {
	target           agentcore.LaunchTarget
	copilotSessionID string
	agentConfig      domain.AgentConfig

	// mcpConfigPath is the worker-generated MCP config file path.
	mcpConfigPath string

	// fallbackToContinue is set when a turn completes without a
	// result event containing a sessionId. On the next turn,
	// buildArgs uses --continue instead of --resume.
	fallbackToContinue bool

	// forkSession owns the subprocess lifecycle for this session.
	forkSession *agentcore.ForkPerTurnSession

	// Per-turn scan state owned by the ParseLine and OnFinalize hook
	// closures. Reset at the top of each RunTurn call before delegating
	// to forkSession.
	acc      *agentcore.UsageAccumulator
	inFlight *agentcore.ToolTracker
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

	baseLogger := slog.Default().With(slog.String("component", "copilot-adapter"))
	sessionLogger := logging.WithSession(baseLogger, copilotSessionID)

	hooks := agentcore.ForkPerTurnHooks{
		BuildArgs: func(turn int, prompt string) []string {
			return buildArgs(state, turn, prompt, a.passthrough)
		},
		ParseLine: func(line []byte, emit func(domain.AgentEvent), pid string) (any, error) {
			now := time.Now().UTC()

			event, parseErr := parseEvent(line)
			if parseErr != nil {
				return nil, parseErr
			}

			switch event.Type {
			case "assistant.message_delta":
				// Stall timer reset; ephemeral streaming content.
				agentcore.EmitNotification(emit, "")

			case "assistant.message":
				if len(event.Data) > 0 {
					msgData, dataErr := parseAssistantMessageData(event.Data)
					if dataErr == nil {
						snapshot, _ := state.acc.AddDelta(0, msgData.OutputTokens, 0)
						emit(domain.AgentEvent{
							Type:      domain.EventTokenUsage,
							Timestamp: now,
							Usage:     snapshot,
						})
						agentcore.EmitNotification(emit, summarizeAssistantMessage(msgData))
					} else {
						sessionLogger.Debug("failed to parse assistant.message data", slog.Any("error", dataErr))
						agentcore.EmitNotification(emit, "assistant message")
					}
				}

			case "assistant.turn_start", "assistant.turn_end":
				agentcore.EmitNotification(emit, event.Type)

			case "tool.execution_start":
				if len(event.Data) > 0 {
					toolData, dataErr := parseToolExecutionData(event.Data)
					if dataErr == nil {
						state.inFlight.Begin(toolData.ToolCallID, toolData.ToolName)
						agentcore.EmitNotification(emit, fmt.Sprintf("tool started: %s", toolData.ToolName))
					}
				}

			case "tool.execution_complete":
				if len(event.Data) > 0 {
					toolData, dataErr := parseToolExecutionData(event.Data)
					if dataErr == nil {
						toolName, durationMS, ok := state.inFlight.End(toolData.ToolCallID)
						if !ok {
							toolName = toolData.ToolName
						}
						emit(domain.AgentEvent{
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
				sessionLogger.Warn("copilot session warning", slog.String("message", msg))
				agentcore.EmitNotification(emit, msg)

			case "session.info":
				msg := "session info"
				if len(event.Data) > 0 {
					infoData, dataErr := parseSessionInfoData(event.Data)
					if dataErr == nil && infoData.Message != "" {
						msg = infoData.Message
					}
				}
				agentcore.EmitNotification(emit, msg)

			case "session.task_complete":
				msg := "task complete"
				if len(event.Data) > 0 {
					taskData, dataErr := parseSessionTaskCompleteData(event.Data)
					if dataErr == nil && taskData.Summary != "" {
						msg = taskData.Summary
					}
				}
				agentcore.EmitNotification(emit, msg)

			case "session.mcp_server_status_changed", "session.mcp_servers_loaded",
				"session.tools_updated", "user.message":
				sessionLogger.Debug("copilot event logged only", slog.String("event_type", event.Type))

			case "result":
				captured := event
				return &captured, nil

			default:
				emit(domain.AgentEvent{
					Type:      domain.EventOtherMessage,
					Timestamp: now,
					Message:   event.Type,
				})
			}

			return nil, nil
		},
		GetUsage:     func() domain.TokenUsage { return state.acc.Snapshot() },
		GetSessionID: func() string { return state.copilotSessionID },
		OnFinalize: func(emit func(domain.AgentEvent), lastParsed any, exitCode int, stderrLines []string) (domain.TurnResult, *domain.AgentError) {
			lastResult, _ := lastParsed.(*rawEvent)

			// Capture session ID from result event for subsequent turns.
			if lastResult != nil && lastResult.SessionID != "" {
				state.copilotSessionID = lastResult.SessionID
				state.fallbackToContinue = false
			} else if state.copilotSessionID == "" {
				// No result event and no session ID from a prior turn.
				// Use --continue on the next turn to resume the most recent
				// conversation in the workspace directory.
				state.fallbackToContinue = true
			}

			usage := state.acc.Snapshot()

			if lastResult != nil {
				// Extract API duration from the result event.
				var apiDurationMS int64
				if lastResult.Usage != nil {
					apiDurationMS = lastResult.Usage.TotalAPIDurMS
					logging.WithSession(baseLogger, state.copilotSessionID).Info("copilot turn completed",
						slog.Int64("premium_requests", lastResult.Usage.PremiumRequests))
				}

				if lastResult.ExitCode != nil && *lastResult.ExitCode == 0 {
					agentcore.EmitTurnCompleted(emit, "", apiDurationMS)
					return domain.TurnResult{
						SessionID:  state.copilotSessionID,
						ExitReason: domain.EventTurnCompleted,
						Usage:      usage,
					}, nil
				}
				// EmitWarnLines is called by the skeleton when agentErr is non-nil.
				agentcore.EmitTurnFailed(emit, "non-zero exit in result event", apiDurationMS)
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
				agentcore.EmitTurnFailed(emit, "non-zero exit", 0)
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
			if state.acc.Snapshot().OutputTokens == 0 {
				sessionLogger.Warn("agent exited without producing output, treating as failure")
				agentcore.EmitTurnFailed(emit, "agent exited without producing output", 0)
				return domain.TurnResult{
						SessionID:  state.copilotSessionID,
						ExitReason: domain.EventTurnFailed,
						Usage:      usage,
					}, &domain.AgentError{
						Kind:    domain.ErrTurnFailed,
						Message: "agent exited without producing output",
					}
			}

			agentcore.EmitTurnCompleted(emit, "", 0)
			return domain.TurnResult{
				SessionID:  state.copilotSessionID,
				ExitReason: domain.EventTurnCompleted,
				Usage:      usage,
			}, nil
		},
		// Copilot emits EventSessionStarted before the scan loop using the
		// current session ID (empty on turn 1; populated on turns 2+ from
		// the previous turn's terminal result).
		EmitSessionStartID: func() string { return state.copilotSessionID },
	}

	state.forkSession = agentcore.NewForkPerTurnSession(&state.target, hooks, sessionLogger)

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

// RunTurn executes one agent turn by delegating to the session's
// [agentcore.ForkPerTurnSession]. Per-turn scan state is reset before
// delegation so each turn starts with a fresh accumulator and clean state.
func (a *CopilotAdapter) RunTurn(ctx context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
	if params.OnEvent == nil {
		panic("copilot: OnEvent must be non-nil")
	}

	state, ok := session.Internal.(*sessionState)
	if !ok {
		return domain.TurnResult{}, fmt.Errorf("unexpected session internal type %T", session.Internal)
	}

	// Reset per-turn scan state before delegation.
	state.acc = agentcore.NewUsageAccumulator()
	state.inFlight = agentcore.NewToolTracker()

	return state.forkSession.RunTurn(ctx, params.Prompt, params.OnEvent)
}

// StopSession terminates a running Copilot CLI subprocess gracefully by
// delegating to the session's [agentcore.ForkPerTurnSession].
func (a *CopilotAdapter) StopSession(ctx context.Context, session domain.Session) error {
	state, ok := session.Internal.(*sessionState)
	if !ok {
		return fmt.Errorf("unexpected session internal type %T", session.Internal)
	}
	if state.forkSession == nil {
		return nil
	}
	return state.forkSession.Stop(ctx)
}

// EventStream returns nil. The Copilot CLI adapter delivers events
// synchronously via the [domain.RunTurnParams] OnEvent callback.
func (a *CopilotAdapter) EventStream() <-chan domain.AgentEvent {
	return nil
}
