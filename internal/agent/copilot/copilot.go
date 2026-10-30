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
	"cmp"
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/copilot/quota"
	"github.com/sortie-ai/sortie/internal/agent/copilot/sessionstate"
	"github.com/sortie-ai/sortie/internal/agent/copilot/usagelimits"
	"github.com/sortie-ai/sortie/internal/agent/mcpconfig"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/workspacekit"
)

const derivedConfigName = "copilot-mcp.json"

// writeDerivedConfig returns the path a local launch hands to
// --additional-mcp-config.
func writeDerivedConfig(workspacePath string, tools *agentcore.ToolSession) (string, error) {
	doc := tools.Delivery(sshutil.ForwardOutcome{}).Document
	if err := workspacekit.WriteSortieFile(workspacePath, derivedConfigName, doc); err != nil {
		return "", err
	}
	return filepath.Join(workspacePath, workspacekit.SortieDir, derivedConfigName), nil
}

// placeMCPConfig passes the delivery inline: a host path names nothing on the
// remote host.
func placeMCPConfig(d mcpconfig.Delivery) sshutil.LatePreamble {
	if len(d.Document) == 0 {
		return sshutil.LatePreamble{}
	}
	return sshutil.LatePreamble{Args: []string{"--additional-mcp-config", string(d.Document)}}
}

// copilotToolDeniedCode is the tool.execution_complete error code the CLI
// reports when its own non-interactive permission policy refuses a tool
// call and the session continues.
const copilotToolDeniedCode = "denied"

const defaultCommand = "copilot"

// usageSources: the usage checkpoint on the event stream is the running total
// per served model, the journal's per-request lines settle a turn the
// checkpoints left unsettled, and the result event's premium requests are a
// vendor unit.
var usageSources = usage.Sources{
	Live: usage.Source{
		Category: usage.SourceOutput,
		Strategy: usage.StrategyLive,
		Spend: usage.SpendDeclaration{
			Shape:             usage.SpendSessionCumulative,
			CacheInInput:      true,
			ReasoningInOutput: true,
			ServedModel:       true,
			VendorUnit:        "premium requests",
		},
	},
	Records: sessionstate.Records,
}

func init() {
	registry.Agents.RegisterWithMeta("copilot-cli", NewCopilotAdapter, registry.AgentMeta{
		RequiresCommand:       true,
		DefaultCommand:        defaultCommand,
		ValidateAgentConfig:   validateConfig,
		MCPDialect:            registry.MCPDialectMCPServersJSON,
		UsageSources:          usageSources,
		CredentialEnv:         registry.DeclareCredentialEnv("COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"),
		VerificationContext:   registry.DeclareVerificationContext(registry.VerificationKeepsSomeTools, registry.VerificationKeepsSystemPrompt),
		EffortForwarding:      registry.EffortForwarded,
		UsageLimitRecognition: registry.UsageLimitRecognized,
	})
}

var _ domain.AgentAdapter = (*CopilotAdapter)(nil)

// CopilotAdapter satisfies [domain.AgentAdapter] by managing Copilot
// CLI subprocesses. One adapter instance serves all concurrent
// sessions; per-session state is held in [sessionState] via the
// [domain.Session] Internal field.
type CopilotAdapter struct{}

type sessionState struct {
	target           agentcore.LaunchTarget
	copilotSessionID string
	passthrough      passthroughConfig
	agentConfig      domain.AgentConfig
	baseLogger       *slog.Logger

	// mcpConfigPath is empty on a remote launch, where the declaration travels
	// inline.
	mcpConfigPath string

	isContinuation bool

	credentialVerification bool

	reducedVerificationContext bool

	forkSession *agentcore.ForkPerTurnSession

	// usage is built once in StartSession and never reset between turns.
	usage *agentcore.TurnEndUsage

	// ledger converts the checkpoints' session totals into the run's spend and
	// decides when it is complete. Never reset between turns.
	ledger *usage.Ledger

	// Per-turn scan state, owned by the ParseLine and OnFinalize hooks and
	// reset at the top of each RunTurn.
	inFlight *agentcore.ToolTracker
	work     *agentcore.WorkObserver

	turnCompletionSeen bool

	// turnCompletionSuccess is false only when a session.task_complete payload
	// carried an explicit false success field.
	turnCompletionSuccess bool

	turnCompletionSummary string

	// turnErrorMessage is the failure reason when the result event reports a
	// failed exit.
	turnErrorMessage string

	// turnRefusal is the first session error of the turn that is a usage-limit
	// refusal; nil when the turn saw none.
	turnRefusal *usagelimits.Refusal

	// turnContinuations is diagnostic only; the turn disposition never reads
	// it.
	turnContinuations int

	turnResultSeen bool

	// turnReading is the turn's last usage checkpoint; nil when it saw none.
	turnReading *usage.SpendReading

	// turnReplied is true when the result event reported exit code 0.
	turnReplied bool

	// turnAborted is true when the runtime reported the turn aborted. The
	// final checkpoint and result that follow an abort omit the request in
	// flight, so they never settle the turn complete.
	turnAborted bool
}

func (s *sessionState) logger() *slog.Logger {
	if s.copilotSessionID == "" {
		return s.baseLogger
	}
	return logging.WithSession(s.baseLogger, s.copilotSessionID)
}

// NewCopilotAdapter creates a [CopilotAdapter]. Settings and command
// resolution are deferred to [CopilotAdapter.StartSession].
func NewCopilotAdapter() (domain.AgentAdapter, error) {
	return &CopilotAdapter{}, nil
}

// StartSession parses the session's settings, validates the workspace path,
// resolves the copilot binary, and initializes per-session state. A local
// session runs a version canary of the binary; turns spawn their
// subprocess in [CopilotAdapter.RunTurn].
func (a *CopilotAdapter) StartSession(ctx context.Context, params domain.StartSessionParams) (domain.Session, error) {
	pt, fault := parsePassthroughConfig(params.Settings)
	if fault != nil {
		return domain.Session{}, agentcore.SettingsError(fault.Error(), fault)
	}
	target, agentErr := agentcore.ResolveLaunchTarget(params, defaultCommand)
	if agentErr != nil {
		return domain.Session{}, agentErr
	}

	if target.RemoteCommand == "" {
		if _, canaryErr := agentcore.QueryRuntimeVersion(ctx, target, params.AgentConfig, nil, nil); canaryErr != nil {
			if canaryErr.Kind == domain.ErrPortExit {
				canaryErr.Message = "copilot binary found but not functional; ensure Node.js 22+ is available: " + canaryErr.Message
			}
			return domain.Session{}, canaryErr
		}
	}

	copilotSessionID := params.ResumeSessionID
	if copilotSessionID == "" {
		uuid, uuidErr := agentcore.NewUUIDv4()
		if uuidErr != nil {
			return domain.Session{}, &domain.AgentError{
				Kind:    domain.ErrAgentNotFound,
				Message: "could not generate a session id",
				Err:     uuidErr,
			}
		}
		copilotSessionID = uuid
	}

	baseLogger := slog.Default().With(slog.String("component", "copilot-adapter"))

	ledger, err := usage.NewLedger(usageSources.Live, usageSources.Records)
	if err != nil {
		return domain.Session{}, &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: "usage ledger refused the kind's sources",
			Err:     err,
		}
	}

	tools, toolsErr := agentcore.ResolveToolDelivery(ctx, params, target, registry.MCPDialectMCPServersJSON, baseLogger)
	if toolsErr != nil {
		return domain.Session{}, toolsErr
	}

	mcpConfigPath := ""
	if target.RemoteCommand == "" && tools.Configured() && !params.CredentialVerification {
		mcpConfigPath, err = writeDerivedConfig(target.WorkspacePath, tools)
		if err != nil {
			_ = tools.Close()
			return domain.Session{}, &domain.AgentError{
				Kind:    domain.ErrResponseError,
				Message: fmt.Sprintf("prepare MCP config: %v", err),
				Err:     err,
			}
		}
	}

	state := &sessionState{
		target:                 target,
		copilotSessionID:       copilotSessionID,
		passthrough:            pt,
		agentConfig:            params.AgentConfig,
		baseLogger:             baseLogger,
		mcpConfigPath:          mcpConfigPath,
		isContinuation:         params.ResumeSessionID != "",
		credentialVerification: params.CredentialVerification,
		usage:                  agentcore.NewTurnEndUsage(),
		ledger:                 ledger,

		reducedVerificationContext: params.ReducedVerificationContext(),
	}

	if params.ResumeSessionID == "" {
		ledger.SessionCreated()
	} else {
		readCtx, cancel := context.WithTimeout(ctx, agentcore.ReadTimeout(params.AgentConfig))
		ledger.Boundary(state.readJournal(readCtx))
		cancel()
	}

	hooks := agentcore.ForkPerTurnHooks{
		Tools:         tools,
		ToolPlacement: placeMCPConfig,
		BuildArgs: func(turn int, prompt string) []string {
			return buildArgs(state, turn, prompt, state.passthrough)
		},
		ParseLine: func(line []byte, emit func(domain.AgentEvent), pid string, spend *agentcore.TurnSpend) (any, error) {
			now := time.Now().UTC()

			event, parseErr := parseEvent(line)
			if parseErr != nil {
				return nil, parseErr
			}

			switch event.Type {
			case "assistant.message_delta":
				spend.MarkModelReached()
				// The event type itself names an assistant message, so it
				// is a work signal even though its payload is unparsed.
				state.work.ObserveAssistantOutput()
				// Empty notification: resets the stall timer only.
				agentcore.EmitNotification(emit, "")

			case "assistant.message":
				spend.MarkModelReached()
				if len(event.Data) > 0 {
					msgData, dataErr := parseAssistantMessageData(event.Data)
					if dataErr == nil {
						if msgData.Content != "" {
							state.work.ObserveAssistantOutput()
						}
						if len(msgData.ToolRequests) > 0 {
							state.work.ObserveToolActivity()
						}
						agentcore.EmitNotification(emit, summarizeAssistantMessage(msgData))
					} else {
						state.logger().Debug("failed to parse assistant.message data", slog.Any("error", dataErr))
						agentcore.EmitNotification(emit, "")
					}
				}

			case "model.messages_snapshot":
				state.logger().Debug("copilot event logged only", slog.String("event_type", event.Type))

			case "assistant.turn_start", "assistant.turn_end":
				agentcore.EmitNotification(emit, "")

			case "tool.execution_start":
				spend.MarkModelReached()
				if len(event.Data) > 0 {
					toolData, dataErr := parseToolExecutionData(event.Data)
					if dataErr == nil {
						state.work.ObserveToolActivity()
						state.inFlight.Begin(toolData.ToolCallID, toolData.toolCall())
						agentcore.EmitNotification(emit, "")
					}
				}

			case "tool.execution_complete":
				if len(event.Data) > 0 {
					toolData, dataErr := parseToolExecutionData(event.Data)
					if dataErr == nil {
						state.work.ObserveToolActivity()
						call, durationMS, _ := state.inFlight.End(toolData.ToolCallID)
						emit(domain.AgentEvent{
							Type:           domain.EventToolResult,
							Timestamp:      now,
							ToolName:       call.Name,
							ToolKind:       call.Kind,
							ToolArgument:   call.Argument,
							ToolDurationMS: durationMS,
							ToolError:      !toolData.Success,
						})

						// --no-ask-user closes the human-question path, so a
						// denied call here is always a permission request
						// with no reply channel.
						if !toolData.Success && toolData.Error != nil && toolData.Error.Code == copilotToolDeniedCode {
							posture := agentcore.DecideHumanRequest(agentcore.ClassPermission, false, agentcore.AnswerRuntimeRefused)
							agentcore.EmitNotification(emit, posture.Notice)
						}
					}
				}

			case "session.warning":
				msg := ""
				if len(event.Data) > 0 {
					warnData, dataErr := parseSessionWarningData(event.Data)
					if dataErr == nil {
						msg = warnData.Message
					}
				}
				state.logger().Warn("copilot session warning", slog.String("message", cmp.Or(msg, "session warning")))
				agentcore.EmitNotification(emit, msg)

			case "session.info":
				msg := ""
				if len(event.Data) > 0 {
					infoData, dataErr := parseSessionInfoData(event.Data)
					if dataErr == nil {
						msg = infoData.Message
					}
				}
				agentcore.EmitNotification(emit, msg)

			case "session.task_complete":
				state.turnCompletionSeen = true
				state.turnCompletionSuccess = true
				msg := ""
				if len(event.Data) > 0 {
					taskData, dataErr := parseSessionTaskCompleteData(event.Data)
					if dataErr == nil {
						state.turnCompletionSummary = taskData.Summary
						if taskData.Success != nil {
							state.turnCompletionSuccess = *taskData.Success
						}
						msg = taskData.Summary
					}
				}
				agentcore.EmitNotification(emit, msg)

			case "session.mcp_server_status_changed", "session.mcp_servers_loaded",
				"session.tools_updated":
				state.logger().Debug("copilot event logged only", slog.String("event_type", event.Type))

			case usagelimits.SessionErrorEvent:
				if len(event.Data) > 0 {
					sessionErr, dataErr := usagelimits.ParseSessionError(event.Data)
					if dataErr == nil {
						state.turnErrorMessage = sessionErr.Message
						if refusal, ok := usagelimits.RefusalOf(sessionErr); ok && state.turnRefusal == nil {
							state.turnRefusal = &refusal
						}
					}
				}
				agentcore.EmitNotification(emit, "")

			case "user.message":
				if len(event.Data) > 0 {
					msgData, dataErr := parseUserMessageData(event.Data)
					if dataErr == nil && msgData.IsAutopilotContinuation {
						state.turnContinuations++
					}
				}
				state.logger().Debug("copilot event logged only", slog.String("event_type", event.Type))

			case "session.usage_checkpoint":
				if reading, ok := sessionstate.UsageCheckpoint(line); ok {
					state.turnReading = &reading
				}

			case "abort":
				state.turnAborted = true

			case "result":
				if event.SessionID != "" {
					state.copilotSessionID = event.SessionID
				}
				state.turnResultSeen = true
				state.turnReplied = event.ExitCode != nil && *event.ExitCode == 0
				return new(event), nil

			default:
				state.logger().Debug("copilot event logged only", slog.String("event_type", event.Type))
				agentcore.EmitNotification(emit, "")
			}

			return nil, nil
		},
		GetUsage: func() (domain.TokenUsage, map[string]domain.TokenUsage, bool) {
			return state.usage.Snapshot(), state.usage.ByModel(), state.usage.Measured()
		},
		GetSessionID: func() string { return state.copilotSessionID },
		SettleUsage: func(ctx context.Context, emit func(domain.AgentEvent), spend *agentcore.TurnSpend, _ bool) {
			end := usage.TurnEnd{Replied: state.turnReplied, Cancelled: state.turnAborted, ModelReached: spend.ModelReached()}
			recovered := agentcore.SettleSpend(ctx, state.ledger, end, state.turnReading,
				state.readJournal, agentcore.ReadTimeout(state.agentConfig), spend)
			state.usage.Settle(emit, recovered)
		},
		OnFinalize: func(emit func(domain.AgentEvent), lastParsed any, exitCode int, waitErr error, stderrLines []string, earlyExit *domain.AgentError, spend *agentcore.TurnSpend) (domain.TurnResult, *domain.AgentError) {
			lastResult, _ := lastParsed.(*rawEvent)

			ev := agentcore.TurnEvidence{
				ExitObserved: true,
				ExitCode:     exitCode,
				WaitErr:      waitErr,
				EarlyExit:    earlyExit,
			}
			ev.Work, ev.WorkDetail = state.work.Report()

			var apiDurationMS int64
			if lastResult != nil && lastResult.Usage != nil {
				apiDurationMS = lastResult.Usage.TotalAPIDurMS
				state.logger().Info("copilot turn completed",
					slog.Int64("premium_requests", lastResult.Usage.PremiumRequests))
			}

			switch {
			case state.turnRefusal != nil:
				limit := state.turnRefusal.Limit
				ev.Terminal = agentcore.TerminalFailure
				ev.TerminalErrorKind = domain.ErrUsageLimit
				ev.UsageLimit = &limit
				ev.TerminalMessage = state.turnErrorMessage
			case lastResult == nil:
			case lastResult.ExitCode == nil || *lastResult.ExitCode != 0:
				ev.Terminal = agentcore.TerminalFailure
				ev.TerminalMessage = "non-zero exit in result event"
				if state.turnErrorMessage != "" {
					ev.TerminalMessage = state.turnErrorMessage
				}
			case !state.turnCompletionSeen:
				ev.Terminal = agentcore.TerminalIncomplete
				ev.TerminalMessage = "raise copilot-cli.max_autopilot_continues if the turn needs more steps"
				state.logger().Warn("copilot turn ended without a task-completion report",
					slog.Int("autopilot_continuations_observed", state.turnContinuations),
					slog.Int("max_autopilot_continues", effectiveMaxAutopilotContinues(state.passthrough)))
			case !state.turnCompletionSuccess:
				ev.Terminal = agentcore.TerminalFailure
				ev.TerminalErrorKind = domain.ErrTurnFailed
				ev.TerminalMessage = completionFailureMessage(state.turnCompletionSummary)
			default:
				ev.Terminal = agentcore.TerminalSuccess
			}

			return state.usage.Finalize(emit, state.logger(), ev, state.copilotSessionID, apiDurationMS, nil, *spend)
		},
		EmitSessionStartID: func() string { return state.copilotSessionID },
	}

	// The adapter sets no TurnEnv hook, so a turn runs with the host
	// environment and a nil env reads exactly that.
	hooks.PullWindows = usage.NewPuller(quota.Source, agentcore.AuxiliaryTimeout(params.AgentConfig), params.CredentialVerification, copilotPull(target)).Pull
	hooks.RefusalWindowReset = func() bool { return state.turnRefusal != nil && state.turnRefusal.QuotaWindow }

	state.forkSession = agentcore.NewForkPerTurnSession(&state.target, hooks, state.baseLogger, state.agentConfig.StopGraceMS)

	return domain.Session{
		ID:            copilotSessionID,
		Internal:      state,
		NoToolChannel: tools.NoToolChannel(),
	}, nil
}

// RunTurn executes one agent turn on the session's
// [agentcore.ForkPerTurnSession], with per-turn scan state reset first.
func (a *CopilotAdapter) RunTurn(ctx context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
	if params.OnEvent == nil {
		panic("copilot: OnEvent must be non-nil")
	}

	state, ok := session.Internal.(*sessionState)
	if !ok {
		return domain.TurnResult{}, fmt.Errorf("unexpected session internal type %T", session.Internal)
	}

	// state.usage is run-cumulative and deliberately not reset.
	state.inFlight = agentcore.NewToolTracker()
	state.work = agentcore.NewWorkObserver(agentcore.WorkSignals{AssistantOutput: true, ToolActivity: true})
	state.turnCompletionSeen = false
	state.turnCompletionSuccess = false
	state.turnCompletionSummary = ""
	state.turnErrorMessage = ""
	state.turnRefusal = nil
	state.turnContinuations = 0
	state.turnResultSeen = false
	state.turnReading = nil
	state.turnReplied = false
	state.turnAborted = false

	return state.forkSession.RunTurn(ctx, params.Prompt, params.OnEvent)
}

// StopSession terminates a running Copilot CLI subprocess gracefully by
// delegating to the session's [agentcore.ForkPerTurnSession].
func (a *CopilotAdapter) StopSession(ctx context.Context, session domain.Session) error {
	state, ok := session.Internal.(*sessionState)
	if !ok {
		return fmt.Errorf("unexpected session internal type %T", session.Internal)
	}
	var stopErr error
	if state.forkSession != nil {
		stopErr = state.forkSession.Stop(ctx)
	}
	if state.credentialVerification && state.copilotSessionID != "" {
		deleteVerificationSession(ctx, state.target, state.copilotSessionID, agentcore.AuxiliaryTimeout(state.agentConfig), state.agentConfig.StopGraceMS, state.logger())
	}
	return stopErr
}

// hostQuotaRuntime reaches the Copilot runtime through the session's launch
// target.
type hostQuotaRuntime struct {
	target agentcore.LaunchTarget
}

func (r hostQuotaRuntime) Env(name string) (string, bool) {
	return r.target.RuntimeEnv(nil, name)
}

func (r hostQuotaRuntime) Exchange(ctx context.Context, args []string, limit int64, exchange func(stdin io.Writer, stdout io.Reader) error) error {
	return r.target.RunHostExchange(ctx, nil, agentcore.HostCommand{
		LaunchWords: len(r.target.CommandWords()),
		Args:        args,
		InSession:   true,
		Limit:       limit,
	}, exchange)
}

// copilotPull returns the quota pull for one session. A turn runs with the
// host environment and the adapter sets no TurnEnv hook, so a nil env reads
// exactly that.
func copilotPull(target agentcore.LaunchTarget) func(context.Context) ([]usage.WindowReading, error) {
	return func(ctx context.Context) ([]usage.WindowReading, error) {
		return quota.Pull(ctx, hostQuotaRuntime{target: target})
	}
}
