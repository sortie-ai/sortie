// Package opencode implements [domain.AgentAdapter] for the OpenCode CLI.
// It launches one `opencode run --format json` subprocess per turn,
// normalizes stdout envelopes into domain events, and settles token usage
// through the usage ledger from the run's step reports and the session export
// (`opencode session export --standalone --sanitize`).
//
// The CLI accepts no MCP configuration path, so the adapter places the
// session's tool-server delivery as the "mcp" member of the inline
// configuration document in the turn's environment; a remote launch with a
// tool relay carries it as a late SSH preamble entry.
package opencode

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/mcpconfig"
	"github.com/sortie-ai/sortie/internal/agent/opencode/sessionexport"
	"github.com/sortie-ai/sortie/internal/agent/opencode/usagelimits"
	"github.com/sortie-ai/sortie/internal/agent/procutil"
	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/redact"
	"github.com/sortie-ai/sortie/internal/registry"
)

// defaultCommand is what a session launches when it is given no command.
const defaultCommand = "opencode"

// usageSources: each step_finish report carries one request's token counts,
// and the session export holds the counts per model for every finished
// message. Older releases usually drop the report of the turn's final text
// step; the reports then fall short of the turn and the export settles it.
var usageSources = usage.Sources{
	Live: usage.Source{
		Category: usage.SourceOutput,
		Strategy: usage.StrategyLive,
		Spend:    usage.SpendDeclaration{Shape: usage.SpendPerRequest},
	},
	Records: sessionexport.Records,
}

func init() {
	registry.Agents.RegisterWithMeta("opencode", NewOpenCodeAdapter, registry.AgentMeta{
		RequiresCommand:       true,
		DefaultCommand:        defaultCommand,
		ValidateAgentConfig:   validateConfig,
		MCPDialect:            registry.MCPDialectMCPMemberJSON,
		UsageSources:          usageSources,
		CredentialEnv:         registry.DeclareCredentialEnv(),
		EffortForwarding:      registry.EffortForwarded,
		UsageLimitRecognition: registry.UsageLimitRecognized,
		VerificationContext: registry.DeclareVerificationContext(
			registry.VerificationKeepsSomeTools,
			registry.VerificationKeepsProjectInstructions,
			registry.VerificationKeepsUserInstructions,
			registry.VerificationKeepsSystemPrompt,
		),
	})
}

var _ domain.AgentAdapter = (*OpenCodeAdapter)(nil)

type OpenCodeAdapter struct{}

type sessionState struct {
	target        agentcore.LaunchTarget
	agentConfig   domain.AgentConfig
	passthrough   passthroughConfig
	sessionID     string
	sessionOpened bool
	baseLogger    *slog.Logger
	tools         *agentcore.ToolSession
	usage         *agentcore.TurnEndUsage
	mu            sync.Mutex

	// ledger settles every turn; built in StartSession and never reset.
	ledger *usage.Ledger

	// fork owns the turn subprocess lifecycle. Built by newForkSession.
	fork *agentcore.ForkPerTurnSession

	// turnConfigContent is the inline configuration document every turn
	// carries through OPENCODE_CONFIG_CONTENT. Never empty, because the
	// document always carries the title-agent switch. Set once in
	// StartSession and never mutated after.
	turnConfigContent string

	credentialVerification bool

	reducedVerificationContext bool

	// Per-turn state, reset by RunTurn and touched only from the hooks,
	// which run on RunTurn's goroutine.
	work         *agentcore.WorkObserver
	runtimeError *rawRunError
	outcome      domain.AgentEventType

	// settling is the report of the step that ended the turn; nil when none
	// arrived.
	settling *usage.SpendReading

	// children are the subagent calls the turn showed, in first-sighting
	// order.
	children []childSession

	// runtimeErrorAtMS is the timestamp of the event runtimeError came from.
	runtimeErrorAtMS int64
}

// NewOpenCodeAdapter creates an [OpenCodeAdapter].
func NewOpenCodeAdapter() (domain.AgentAdapter, error) {
	return &OpenCodeAdapter{}, nil
}

// StartSession parses the session's settings, checks that the installed
// OpenCode is a 2.x release, and initializes session state without
// starting a turn subprocess. It refuses an unusable settings block, a
// set pure, and a model-variant setting without a plain model before
// launching anything, and a major other than 2 once the version is read.
func (a *OpenCodeAdapter) StartSession(ctx context.Context, params domain.StartSessionParams) (domain.Session, error) {
	pt, fault := parsePassthroughConfig(params.Settings)
	if fault != nil {
		return domain.Session{}, agentcore.SettingsError(fault.Error(), fault)
	}
	if err := checkCrossField(pt); err != nil {
		return domain.Session{}, agentcore.SettingsError(err.Error(), err)
	}

	target, agentErr := agentcore.ResolveLaunchTarget(params, defaultCommand)
	if agentErr != nil {
		return domain.Session{}, agentErr
	}

	baseLogger := slog.Default().With(slog.String("component", "opencode-adapter"))

	ledger, ledgerErr := usage.NewLedger(usageSources.Live, usageSources.Records)
	if ledgerErr != nil {
		return domain.Session{}, &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: "usage ledger refused the kind's sources",
			Err:     ledgerErr,
		}
	}

	tools, toolsErr := agentcore.ResolveToolDelivery(ctx, params, target, registry.MCPDialectMCPMemberJSON, baseLogger)
	if toolsErr != nil {
		return domain.Session{}, toolsErr
	}

	state := &sessionState{
		target:                 target,
		agentConfig:            params.AgentConfig,
		passthrough:            pt,
		sessionID:              params.ResumeSessionID,
		baseLogger:             baseLogger,
		tools:                  tools,
		usage:                  agentcore.NewTurnEndUsage(),
		ledger:                 ledger,
		credentialVerification: params.CredentialVerification,

		reducedVerificationContext: params.ReducedVerificationContext(),
	}

	if versionErr := checkRuntimeVersion(ctx, state); versionErr != nil {
		_ = tools.Close()
		return domain.Session{}, versionErr
	}

	if params.ResumeSessionID == "" {
		ledger.SessionCreated()
	} else {
		readCtx, cancel := context.WithTimeout(ctx, agentcore.ReadTimeout(params.AgentConfig))
		ledger.Boundary(state.readRecords(readCtx))
		cancel()
	}

	var mcp []byte
	if tools.Configured() {
		mcp = tools.Delivery(sshutil.ForwardOutcome{}).Document
	}
	turnConfigContent, buildErr := buildTurnConfig(state.passthrough, state.reducedVerificationContext, mcp)
	if buildErr != nil {
		_ = tools.Close()
		return domain.Session{}, &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: "build opencode environment",
			Err:     buildErr,
		}
	}
	state.turnConfigContent = turnConfigContent
	state.fork = newForkSession(state)

	return domain.Session{
		ID:            state.sessionID,
		AgentPID:      "",
		Internal:      state,
		NoToolChannel: tools.NoToolChannel(),
	}, nil
}

// RunTurn executes one OpenCode turn on the session's
// [agentcore.ForkPerTurnSession] and relays normalized events via
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

	state.work = agentcore.NewWorkObserver(agentcore.WorkSignals{AssistantOutput: true, ToolActivity: true})
	state.runtimeError = nil
	state.runtimeErrorAtMS = 0
	state.outcome = domain.EventTurnCompleted
	state.settling = nil
	state.children = nil

	return state.fork.RunTurn(ctx, params.Prompt, params.OnEvent)
}

// StopSession terminates any active subprocess and, for a credential
// verification session, deletes the session the run created.
func (a *OpenCodeAdapter) StopSession(ctx context.Context, session domain.Session) error {
	state, ok := session.Internal.(*sessionState)
	if !ok {
		return &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: fmt.Sprintf("unexpected session internal type %T", session.Internal),
		}
	}

	stopErr := state.fork.Stop(ctx)

	if state.credentialVerification {
		if sessionID := state.currentSessionID(); sessionID != "" {
			deleteVerificationSession(ctx, state, sessionID)
		}
	}

	return stopErr
}

// newForkSession builds the skeleton that runs state's turns.
func newForkSession(state *sessionState) *agentcore.ForkPerTurnSession {
	hooks := agentcore.ForkPerTurnHooks{
		BuildArgs: func(_ int, _ string) []string {
			return buildRunArgs(state, state.passthrough)
		},
		ParseLine: state.parseLine,
		GetUsage: func() (domain.TokenUsage, map[string]domain.TokenUsage, bool) {
			return state.usage.Snapshot(), state.usage.ByModel(), state.usage.Measured()
		},
		GetSessionID:      state.currentSessionID,
		OnFinalize:        state.finalizeTurn,
		PromptOnStdin:     true,
		FirstEventTimeout: agentcore.FirstResponseTimeout(state.agentConfig),
		SettleUsage:       state.settleUsage,
		TurnEnv:           state.turnEnv,
		Tools:             state.tools,
		ToolPlacement:     state.placeTools,
	}
	hooks.PullWindows = usage.NewPuller(usagelimits.Source, agentcore.AuxiliaryTimeout(state.agentConfig), state.credentialVerification, opencodePull(state)).Pull
	hooks.RefusalWindowReset = state.refusalReadsWindows
	return agentcore.NewForkPerTurnSession(&state.target, hooks, state.baseLogger, state.agentConfig.StopGraceMS)
}

// opencodePull returns the session's usage-limits pull, which runs the usage
// command through the same host-command operation as the session export.
func opencodePull(state *sessionState) func(context.Context) ([]usage.WindowReading, error) {
	return func(ctx context.Context) ([]usage.WindowReading, error) {
		return usagelimits.Pull(ctx, func(ctx context.Context, args []string) ([]byte, error) {
			return state.target.RunHostCommand(ctx, buildRunEnv(os.Environ()), state.runtimeCommand(args, 0))
		})
	}
}

// turnEnv returns the environment every turn runs with and the settings an
// SSH launch carries. A launch that places the inline configuration late
// omits it here.
func (s *sessionState) turnEnv() ([]string, []sshutil.EnvVar) {
	managedEnv := buildManagedEnv()
	if !s.declaresLate() {
		managedEnv["OPENCODE_CONFIG_CONTENT"] = s.turnConfigContent
	}
	return buildTurnEnv(s), sortedEnvVars(managedEnv)
}

// declaresLate reports whether the skeleton places a late entry: a remote
// launch whose relay announced a listener. Without one the document must travel
// with the static settings or the runtime would start without its tool policy.
func (s *sessionState) declaresLate() bool {
	return s.tools != nil && s.target.RemoteCommand != "" && s.tools.Forward(nil) != nil
}

// placeTools carries the whole inline configuration as a late entry, because
// its tool-server entry names a remote port known only once the forward is
// allocated.
func (s *sessionState) placeTools(d mcpconfig.Delivery) sshutil.LatePreamble {
	document, err := buildTurnConfig(s.passthrough, s.reducedVerificationContext, d.Document)
	if err != nil {
		s.logger().Error("failed to render late opencode configuration", slog.Any("error", err))
		return sshutil.LatePreamble{}
	}
	return sshutil.LatePreamble{Env: []sshutil.EnvVar{{Name: "OPENCODE_CONFIG_CONTENT", Value: document}}}
}

// deleteVerificationSession only logs a failure, exit 1 for an unknown
// identifier included: a leftover session does not fail the run.
func deleteVerificationSession(ctx context.Context, state *sessionState, sessionID string) {
	deleteCtx, cancel := context.WithTimeout(ctx, agentcore.AuxiliaryTimeout(state.agentConfig))
	defer cancel()

	cmd, buildErr := auxiliaryCommand(deleteCtx, state, deleteArgs(sessionID))
	if buildErr != nil {
		state.logger().Warn("failed to delete credential verification session", slog.Any("error", buildErr))
		return
	}
	result, startErr := procutil.RunCapture(deleteCtx, cmd, procutil.StopGrace(state.agentConfig.StopGraceMS), procutil.CaptureParams{})
	if startErr != nil {
		state.logger().Warn("failed to delete credential verification session", slog.Any("error", startErr))
		return
	}
	if result.WaitErr != nil {
		state.logger().Warn("failed to delete credential verification session", slog.Any("error", result.WaitErr))
	}
}

// parseLine handles one stdout line as the skeleton's ParseLine hook. It
// returns a non-nil result for an event that ends the run's output: a
// turn-ending step_finish or an error event.
func (s *sessionState) parseLine(line []byte, emit func(domain.AgentEvent), _ string, spend *agentcore.TurnSpend) (any, error) {
	event, err := parseRunEvent(line)
	if err != nil {
		return nil, err
	}

	started, mismatch := s.applySessionEvent(event.SessionID)
	if mismatch {
		return nil, agentcore.EndTurn(domain.ErrResponseError,
			fmt.Sprintf("session id mismatch: expected %q, got %q", s.currentSessionID(), event.SessionID))
	}
	if started {
		emit(domain.AgentEvent{
			Type:      domain.EventSessionStarted,
			Timestamp: time.Now().UTC(),
			SessionID: s.currentSessionID(),
			Message:   "session started",
		})
	}

	now := time.Now().UTC()
	switch event.Type {
	case "step_start":
		if _, err := parseStepStartPart(event.Part); err != nil {
			emit(domain.AgentEvent{Type: domain.EventMalformed, Timestamp: now, Message: "invalid step_start payload"})
			return nil, nil
		}
		agentcore.EmitNotification(emit, "")

	case "text":
		part, err := parseTextPart(event.Part)
		if err != nil {
			emit(domain.AgentEvent{Type: domain.EventMalformed, Timestamp: now, Message: "invalid text payload"})
			return nil, nil
		}
		s.work.ObserveAssistantOutput()
		spend.MarkModelReached()
		agentcore.EmitNotification(emit, redact.Truncate(part.Text, 500))

	case "reasoning":
		if _, err := parseReasoningPart(event.Part); err != nil {
			emit(domain.AgentEvent{Type: domain.EventMalformed, Timestamp: now, Message: "invalid reasoning payload"})
			return nil, nil
		}
		s.work.ObserveAssistantOutput()
		spend.MarkModelReached()
		agentcore.EmitNotification(emit, "")

	case "tool_use":
		part, err := parseToolPart(event.Part)
		if err != nil {
			emit(domain.AgentEvent{Type: domain.EventMalformed, Timestamp: now, Message: "invalid tool_use payload"})
			return nil, nil
		}
		s.work.ObserveToolActivity()
		spend.MarkModelReached()
		s.noteSubagentCall(part)
		call := agentcore.MapToolCall(toolMappings, part.Tool, part.State.Input)
		emit(domain.AgentEvent{
			Type:           domain.EventToolResult,
			Timestamp:      now,
			ToolName:       call.Name,
			ToolKind:       call.Kind,
			ToolArgument:   call.Argument,
			ToolDurationMS: toolDuration(part.State.Time),
			ToolError:      strings.EqualFold(part.State.Status, "error"),
			Message:        redact.Truncate(part.State.Error, 500),
		})

	case "step_finish":
		part, err := parseStepFinishPart(event.Part)
		if err != nil {
			emit(domain.AgentEvent{Type: domain.EventMalformed, Timestamp: now, Message: "invalid step_finish payload"})
			return nil, nil
		}
		spend.MarkModelReached()
		reading := sessionexport.Tokens(part.Tokens)
		reading.Identity = part.ID
		s.ledger.ObserveRequest(reading)
		agentcore.EmitNotification(emit, "")
		if stepFinishEndsTurn(part.Reason) {
			s.settling = &reading
			return &event, nil
		}

	case "error":
		s.outcome = domain.EventTurnFailed
		if readings := freeLimitReadings(event.Error, event.Timestamp); len(readings) > 0 {
			agentcore.EmitUsageWindows(emit, now, usage.Windows(freeLimitSource, readings, now))
		}
		// One failure can surface as two error events: the actionable
		// diagnostic the session publishes, and the generic placeholder
		// the run command reports when the failure is not in its API
		// error schema. Their order on the stream is not guaranteed, so
		// keep whichever event carries detail rather than whichever
		// arrives last.
		if s.runtimeError == nil || !isMaskedServerError(rawRunErrorMessage(event.Error)) {
			s.runtimeError = event.Error
			s.runtimeErrorAtMS = event.Timestamp
		}
		return &event, nil

	default:
		emit(domain.AgentEvent{
			Type:      domain.EventMalformed,
			Timestamp: now,
			Message:   fmt.Sprintf("unknown event type: %s", event.Type),
		})
	}

	return nil, nil
}

// settleUsage hands the turn's step reports and session export to the ledger.
// The runtime ends a turn itself when its finishing step reports or its process
// exits clean; an error envelope leaves the turn unsettled.
func (s *sessionState) settleUsage(ctx context.Context, emit func(domain.AgentEvent), spend *agentcore.TurnSpend, exitedClean bool) {
	end := usage.TurnEnd{
		Replied:         s.runtimeError == nil && (s.settling != nil || exitedClean),
		ModelReached:    spend.ModelReached(),
		Children:        len(s.children) > 0,
		ChildrenSettled: s.childrenSettled(),
	}
	recovered := agentcore.SettleSpend(ctx, s.ledger, end, s.settling, s.readRecords, agentcore.ReadTimeout(s.agentConfig), spend)
	s.usage.Settle(emit, recovered)
}

// noteSubagentCall records the turn's first sighting of a subagent call.
func (s *sessionState) noteSubagentCall(part rawToolPart) {
	child, ok := subagentCall(part, s.currentSessionID())
	if !ok || slices.ContainsFunc(s.children, func(seen childSession) bool { return seen.call == child.call }) {
		return
	}
	s.children = append(s.children, child)
}

// finalizeTurn builds the disposition of a turn whose process exited. The
// runtime writes a denied-permission warning to stderr rather than stdout,
// so its notification arrives here, at turn end.
func (s *sessionState) finalizeTurn(emit func(domain.AgentEvent), _ any, exitCode int, waitErr error, stderrLines []string, earlyExit *domain.AgentError, spend *agentcore.TurnSpend) (domain.TurnResult, *domain.AgentError) {
	for _, line := range stderrLines {
		if !isPermissionWarning(line) {
			continue
		}
		posture := agentcore.DecideHumanRequest(agentcore.ClassPermission, false, agentcore.AnswerRuntimeRefused)
		agentcore.EmitNotification(emit, posture.Notice)
	}

	ev := agentcore.TurnEvidence{
		ExitObserved: true,
		ExitCode:     exitCode,
		WaitErr:      waitErr,
		EarlyExit:    earlyExit,
	}
	ev.Work, ev.WorkDetail = s.work.Report()

	if s.outcome == domain.EventTurnFailed {
		ev.Terminal = agentcore.TerminalFailure
		ev.TerminalErrorKind = domain.ErrTurnFailed
		ev.TerminalMessage = rawRunErrorMessage(s.runtimeError) + freeTierRefusalClause(s.runtimeError, s.passthrough)
		if limit, ok := s.refusal(); ok {
			ev.TerminalErrorKind = domain.ErrUsageLimit
			ev.UsageLimit = &limit
		}
	}

	return s.usage.Finalize(emit, s.logger(), ev, s.currentSessionID(), 0, nil, *spend)
}

// refusal returns the usage limit the turn's runtime error is, with the reset
// the error's own delay gives; ok is false for any other error.
func (s *sessionState) refusal() (domain.UsageLimit, bool) {
	if s.runtimeError == nil {
		return domain.UsageLimit{}, false
	}
	limit, ok := usagelimits.Refusal(s.runtimeError.Type)
	if !ok {
		return domain.UsageLimit{}, false
	}
	limit.ResetsAt = usagelimits.RetryAt(s.runtimeErrorAtMS, s.runtimeError.RetryAfterMs)
	return limit, true
}

// refusalReadsWindows reports whether the turn's runtime error is a refusal the
// OpenCode console meters, so an exhausted console window may supply its reset.
func (s *sessionState) refusalReadsWindows() bool {
	return s.runtimeError != nil && usagelimits.ConsoleMetered(s.runtimeError.Type)
}

func (s *sessionState) logger() *slog.Logger {
	sessionID := s.currentSessionID()
	if sessionID == "" {
		return s.baseLogger
	}
	return logging.WithSession(s.baseLogger, sessionID)
}

func (s *sessionState) currentSessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
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

func isPermissionWarning(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(agentcore.SanitizeLine(line)), "! permission requested:")
}

func toolDuration(partTime rawPartTime) int64 {
	if partTime.End <= partTime.Start {
		return 0
	}
	return partTime.End - partTime.Start
}

// rawRunErrorMessage returns the first non-empty of runErr's message or
// type, falling back to a generic message when neither carries text.
func rawRunErrorMessage(runErr *rawRunError) string {
	if runErr == nil {
		return "opencode reported an unknown error"
	}
	if runErr.Message != "" {
		return runErr.Message
	}
	if runErr.Type != "" {
		return runErr.Type
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

// stepFinishEndsTurn reports whether reason ends the turn rather than
// continuing with another step after tool calls.
func stepFinishEndsTurn(reason string) bool {
	return reason != "tool-calls"
}
