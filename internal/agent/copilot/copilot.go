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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/registry"
)

// sessionIDPattern matches the safe path-segment character set a
// copilot session id must satisfy before it is joined into a
// session-state path.
var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// copilotToolDeniedCode is the tool.execution_complete error code the CLI
// reports when its own non-interactive permission policy refuses a tool
// call and the session continues.
const copilotToolDeniedCode = "denied"

func init() {
	registry.Agents.RegisterWithMeta("copilot-cli", NewCopilotAdapter, registry.AgentMeta{
		RequiresCommand:     true,
		ValidateAgentConfig: validateConfig,
		MCPInjection:        registry.MCPInjectionSupported,
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
	baseLogger       *slog.Logger

	// mcpConfigPath is the worker-generated MCP config file path.
	mcpConfigPath string

	// fallbackToContinue is set when a turn completes without a
	// result event containing a sessionId. On the next turn,
	// buildArgs uses --continue instead of --resume.
	fallbackToContinue bool

	// forkSession owns the subprocess lifecycle for this session.
	forkSession *agentcore.ForkPerTurnSession

	// acc holds the session's run-cumulative token usage. Constructed
	// once in StartSession and never reset between turns.
	acc *agentcore.RunUsage

	// admittedAPICalls holds the API call identifiers whose output-token
	// measurement this run has already admitted. Run-scoped: allocated
	// once in StartSession and never reset between turns, because the
	// CLI resumes a disk-persisted session and a later turn's stream can
	// replay an earlier turn's records, so a run-scoped set suppresses
	// only a genuine repeat.
	admittedAPICalls map[string]struct{}

	// runCreatedSession is true when this run started a new session
	// (StartSessionParams.ResumeSessionID was empty), as opposed to
	// resuming one from a prior worker attempt. Recorded once in
	// StartSession; used by the post-exit recovery baseline logic to
	// distinguish a run with nothing to recover from one that cannot
	// separate its own spend from a resumed session's prior spend.
	runCreatedSession bool

	// baseline is the session-cumulative total to subtract from a
	// successful session-state read to recover this run's own
	// contribution. Resolved once per run; never reset between turns.
	baseline         domain.TokenUsage
	baselineResolved bool

	// recoveryUnavailable is set when this run cannot separate its own
	// spend from a resumed session's prior spend after missing its
	// first read attempt. Once set, the output-only provisional figure
	// stands for the remainder of the run.
	recoveryUnavailable bool

	// priorFinalizeOccurred is true once any turn of this run has
	// completed OnFinalize, regardless of whether a session-state read
	// was attempted, skipped, or failed. It distinguishes a read that
	// is this run's first read attempt from one that is not.
	priorFinalizeOccurred bool

	// sessionIDRejectionLogged suppresses repeating the invalid-session-id
	// Warn on every turn of a run whose session id never becomes valid.
	sessionIDRejectionLogged bool

	// assistantFieldSeen is true once an admitted output-token
	// measurement of either wire shape has been observed. Monotone: set
	// true once and never cleared.
	assistantFieldSeen bool

	// Per-turn scan state owned by the ParseLine and OnFinalize hook
	// closures. Reset at the top of each RunTurn call before delegating
	// to forkSession.
	turnOutputTokens int64
	inFlight         *agentcore.ToolTracker

	// turnCompletionSeen is true once a session.task_complete event has
	// arrived this turn.
	turnCompletionSeen bool

	// turnCompletionSuccess is false only when a session.task_complete
	// payload parsed this turn and its success field was explicitly
	// false.
	turnCompletionSuccess bool

	// turnCompletionSummary is the session.task_complete report's
	// summary, empty when the report never arrived or its payload was
	// unreadable.
	turnCompletionSummary string

	// turnContinuations counts the user.message events this turn whose
	// payload marked them as autopilot continuations. Diagnostic only;
	// never consulted by the turn disposition.
	turnContinuations int
}

func (s *sessionState) logger() *slog.Logger {
	if s.copilotSessionID == "" {
		return s.baseLogger
	}
	return logging.WithSession(s.baseLogger, s.copilotSessionID)
}

func (s *sessionState) refreshForkLogger() {
	if s.forkSession != nil {
		s.forkSession.SetLogger(s.logger())
	}
}

// admitOutputTokens applies the single admission rule shared by both
// wire shapes that carry a per-message output-token count. It reports
// false without mutating any state when tokens is nil or when id is
// non-empty and already admitted this run; a non-empty id is recorded
// so a repeat sighting is not admitted again, while an empty id is
// never recorded, so every measurement lacking one is admitted. On
// admission it updates the turn's output-token total, takes a
// provisional run-cumulative snapshot, and emits one
// [domain.EventTokenUsage] event.
func (s *sessionState) admitOutputTokens(id string, tokens *int64, emit func(domain.AgentEvent), now time.Time) (admitted bool) { //nolint:unparam // both call sites in ParseLine ignore the result today; the rule reports it so a future caller can branch on admission without changing this signature
	if tokens == nil {
		return false
	}
	if id != "" {
		if _, seen := s.admittedAPICalls[id]; seen {
			return false
		}
		s.admittedAPICalls[id] = struct{}{}
	}

	s.assistantFieldSeen = true
	s.turnOutputTokens += *tokens
	snapshot := s.acc.SetTurnProvisional(domain.TokenUsage{OutputTokens: s.turnOutputTokens})
	emit(domain.AgentEvent{
		Type:      domain.EventTokenUsage,
		Timestamp: now,
		Usage:     snapshot,
	})
	return true
}

// recoverUsage attempts to recover this run's authoritative token usage
// from the runtime's session-state journal after the subprocess has
// exited. It returns the run-cumulative snapshot to carry on the
// terminal event and TurnResult.Usage, and whether the journal read
// itself yielded a usage figure for the run.
//
// The read is skipped, and the output-only provisional snapshot already
// held in s.acc stands, when the session id is unknown or fails the
// path-segment check, when the launch target is in SSH mode, when
// recovery has already been marked unavailable for this run, or when
// the read fails or finds no session.shutdown record; journalMeasured
// is false in every one of those cases. On a successful read, the
// baseline is resolved once per run: at the run's first read attempt it
// is the record preceding the current one (zero when none exists); at a
// later first-successful read it is zero when this run created the
// session, and otherwise recovery is marked unavailable for the
// remainder of the run because the boundary record that would separate
// this run's spend from a resumed session's prior spend is no longer
// available.
func (s *sessionState) recoverUsage(logger *slog.Logger) (usage domain.TokenUsage, journalMeasured bool) {
	hadPriorFinalize := s.priorFinalizeOccurred
	defer func() { s.priorFinalizeOccurred = true }()

	if s.recoveryUnavailable {
		return s.acc.Snapshot(), false
	}

	remote := s.target.RemoteCommand != ""
	sessionID := s.copilotSessionID
	validID := sessionID != "" && sessionIDPattern.MatchString(sessionID)

	if remote || !validID {
		if sessionID != "" && !validID && !s.sessionIDRejectionLogged {
			logger.Warn("copilot session id rejected for session-state read",
				slog.String("reason", "invalid_session_id"))
			s.sessionIDRejectionLogged = true
		}
		return s.acc.Snapshot(), false
	}

	root, err := sessionStateRoot(os.Getenv, os.UserHomeDir)
	if err != nil {
		logger.Debug("copilot session-state root resolution failed",
			slog.String("session_id", sessionID), slog.String("reason", "root_unresolved"))
		return s.acc.Snapshot(), false
	}
	eventsPath := filepath.Join(root, sessionID, "events.jsonl")

	current, previous, found, err := readSessionUsage(eventsPath)
	if err != nil {
		if errors.Is(err, errSessionStateCapExceeded) {
			logger.Warn("copilot session-state read abandoned",
				slog.String("session_id", sessionID), slog.String("reason", "cap_exceeded"))
		} else {
			logger.Debug("copilot session-state read failed",
				slog.String("session_id", sessionID), slog.String("reason", "unreadable"))
		}
		return s.acc.Snapshot(), false
	}

	if !s.baselineResolved {
		switch {
		case !hadPriorFinalize:
			s.baseline = previous
		case s.runCreatedSession:
			s.baseline = domain.TokenUsage{}
		default:
			s.recoveryUnavailable = true
			logger.Warn("copilot session-state recovery abandoned for this run",
				slog.String("session_id", sessionID), slog.String("reason", "boundary_record_unavailable"))
			return s.acc.Snapshot(), false
		}
		s.baselineResolved = true
	}

	if !found {
		logger.Debug("copilot session-state has no session.shutdown record yet",
			slog.String("session_id", sessionID), slog.String("reason", "no_record"))
		return s.acc.Snapshot(), false
	}

	return s.acc.SetRunCumulative(subtractUsage(current, s.baseline)), true
}

// NewCopilotAdapter creates a [CopilotAdapter] from adapter
// configuration. The config parameter is the raw map from the
// "copilot-cli" sub-object in WORKFLOW.md. Command resolution is
// deferred to [CopilotAdapter.StartSession].
func NewCopilotAdapter(config map[string]any) (domain.AgentAdapter, error) {
	pt, fault := parsePassthroughConfig(config)
	if fault != nil {
		return nil, fault
	}
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
		target:            target,
		copilotSessionID:  copilotSessionID,
		agentConfig:       params.AgentConfig,
		baseLogger:        slog.Default().With(slog.String("component", "copilot-adapter")),
		mcpConfigPath:     params.MCPConfigPath,
		runCreatedSession: params.ResumeSessionID == "",
		acc:               agentcore.NewRunUsage(),
		admittedAPICalls:  make(map[string]struct{}),
	}

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
						state.admitOutputTokens(msgData.APICallID, msgData.OutputTokens, emit, now)
						agentcore.EmitNotification(emit, summarizeAssistantMessage(msgData))
					} else {
						state.logger().Debug("failed to parse assistant.message data", slog.Any("error", dataErr))
						agentcore.EmitNotification(emit, "assistant message")
					}
				}

			case "model.message":
				if len(event.Data) == 0 {
					state.logger().Debug("copilot event logged only", slog.String("event_type", event.Type))
					break
				}
				payload, dataErr := parseModelMessageData(event.Data)
				if dataErr != nil {
					state.logger().Debug("failed to parse model.message data", slog.Any("error", dataErr))
					break
				}
				if payload.Message.Role != "assistant" {
					state.logger().Debug("copilot event logged only", slog.String("event_type", event.Type))
					break
				}
				state.admitOutputTokens(payload.Message.APICallID, payload.Message.OutputTokens, emit, now)

			case "model.messages_snapshot":
				state.logger().Debug("copilot event logged only", slog.String("event_type", event.Type))

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

						// The CLI's own non-interactive permission policy
						// denied this tool call and the session continues;
						// --no-ask-user closes the human-question path, so
						// every recognized request here is a permission
						// request with no reply channel.
						if !toolData.Success && toolData.Error != nil && toolData.Error.Code == copilotToolDeniedCode {
							posture := agentcore.DecideHumanRequest(agentcore.ClassPermission, false, agentcore.AnswerRuntimeRefused)
							agentcore.EmitNotification(emit, posture.Notice)
						}
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
				state.logger().Warn("copilot session warning", slog.String("message", msg))
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
				state.turnCompletionSeen = true
				state.turnCompletionSuccess = true
				msg := "task complete"
				if len(event.Data) > 0 {
					taskData, dataErr := parseSessionTaskCompleteData(event.Data)
					if dataErr == nil {
						state.turnCompletionSummary = taskData.Summary
						if taskData.Success != nil {
							state.turnCompletionSuccess = *taskData.Success
						}
						if taskData.Summary != "" {
							msg = taskData.Summary
						}
					}
				}
				agentcore.EmitNotification(emit, msg)

			case "session.mcp_server_status_changed", "session.mcp_servers_loaded",
				"session.tools_updated":
				state.logger().Debug("copilot event logged only", slog.String("event_type", event.Type))

			case "user.message":
				if len(event.Data) > 0 {
					msgData, dataErr := parseUserMessageData(event.Data)
					if dataErr == nil && msgData.IsAutopilotContinuation {
						state.turnContinuations++
					}
				}
				state.logger().Debug("copilot event logged only", slog.String("event_type", event.Type))

			case "result":
				return new(event), nil

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
				state.refreshForkLogger()
			} else if state.copilotSessionID == "" {
				// No result event and no session ID from a prior turn.
				// Use --continue on the next turn to resume the most recent
				// conversation in the workspace directory.
				state.fallbackToContinue = true
			}

			usage, journalMeasured := state.recoverUsage(state.logger())
			measured := journalMeasured || state.assistantFieldSeen

			// Work tests this turn's own output, not the run cumulative,
			// which is non-zero on any second turn.
			ev := agentcore.TurnEvidence{
				ExitObserved: true,
				ExitCode:     exitCode,
				Work:         agentcore.WorkAbsent,
			}
			if state.turnOutputTokens > 0 {
				ev.Work = agentcore.WorkPresent
			}

			var apiDurationMS int64
			if lastResult != nil {
				if lastResult.Usage != nil {
					apiDurationMS = lastResult.Usage.TotalAPIDurMS
					state.logger().Info("copilot turn completed",
						slog.Int64("premium_requests", lastResult.Usage.PremiumRequests))
				}

				switch {
				case lastResult.ExitCode == nil || *lastResult.ExitCode != 0:
					ev.Terminal = agentcore.TerminalFailure
					ev.TerminalMessage = "non-zero exit in result event"
				case !state.turnCompletionSeen:
					ev.Terminal = agentcore.TerminalIncomplete
					ev.TerminalMessage = "raise copilot-cli.max_autopilot_continues if the turn needs more steps"
					state.logger().Warn("copilot turn ended without a task-completion report",
						slog.Int("autopilot_continuations_observed", state.turnContinuations),
						slog.Int("max_autopilot_continues", effectiveMaxAutopilotContinues(a.passthrough)))
				case !state.turnCompletionSuccess:
					ev.Terminal = agentcore.TerminalFailure
					ev.TerminalErrorKind = domain.ErrTurnFailed
					ev.TerminalMessage = completionFailureMessage(state.turnCompletionSummary)
				default:
					ev.Terminal = agentcore.TerminalSuccess
				}
			}

			meta := agentcore.TurnMeta{
				SessionID:     state.copilotSessionID,
				Usage:         usage,
				UsageMeasured: measured,
				APIDurationMS: apiDurationMS,
			}

			return agentcore.FinalizeTurn(emit, state.logger(), ev, meta)
		},
		// Copilot emits EventSessionStarted before the scan loop using the
		// current session ID (empty on turn 1; populated on turns 2+ from
		// the previous turn's terminal result).
		EmitSessionStartID: func() string { return state.copilotSessionID },
	}

	state.forkSession = agentcore.NewForkPerTurnSession(&state.target, hooks, state.logger())

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

	state.refreshForkLogger()

	// Reset per-turn scan state before delegation. state.acc is
	// constructed once in StartSession and carries the run-cumulative
	// snapshot across turns, so it is not reset here.
	state.turnOutputTokens = 0
	state.inFlight = agentcore.NewToolTracker()
	state.turnCompletionSeen = false
	state.turnCompletionSuccess = false
	state.turnCompletionSummary = ""
	state.turnContinuations = 0

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
