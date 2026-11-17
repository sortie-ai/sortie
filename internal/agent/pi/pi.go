// Package pi implements [domain.AgentAdapter] for the Pi CLI. It launches
// one `pi -p --mode json` subprocess per Sortie turn on the shared
// fork-per-turn skeleton, normalizes the JSON event stream into domain
// events, and reports the session's cumulative token usage as each model
// response completes.
//
// The adapter declares [registry.MCPDialectNone]: it does not hand the
// worker-generated MCP configuration to pi, so Sortie's tools are not
// available to a pi session.
package pi

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/redact"
	"github.com/sortie-ai/sortie/internal/registry"
)

const defaultCommand = "pi"

// usageSources: each assistant response carries its own request's counts,
// reported as the response completes. The turn settles from the responses it
// read, split by the model that served each. Cache tokens sit outside the
// input count and reasoning tokens inside the output count.
var usageSources = usage.Sources{Live: usage.Source{
	Category: usage.SourceOutput,
	Strategy: usage.StrategyLive,
	Spend: usage.SpendDeclaration{
		Shape:             usage.SpendPerTurn,
		ReasoningInOutput: true,
		ServedModel:       true,
		DuringTurn:        true,
	},
}}

func init() {
	registry.Agents.RegisterWithMeta("pi", NewPiAdapter, registry.AgentMeta{
		RequiresCommand:               true,
		DefaultCommand:                defaultCommand,
		ValidateAgentConfig:           validateConfig,
		MCPDialect:                    registry.MCPDialectNone,
		MCPDialectNoneReason:          "pi has no tool-server configuration the adapter can hand it",
		UsageSources:                  usageSources,
		CredentialEnv:                 registry.DeclareCredentialEnv(),
		EffortForwarding:              registry.EffortNotForwarded,
		UsageLimitRecognition:         registry.UsageLimitNotRecognized,
		UsageLimitNotRecognizedReason: "pi reports a refused request as a response with stop reason error and the provider's own text, with no structured limit type, reset instant, or model scope",
		VerificationContext:           registry.DeclareVerificationContext(),
	})
}

var _ domain.AgentAdapter = (*PiAdapter)(nil)

type PiAdapter struct{}

type sessionState struct {
	target                 agentcore.LaunchTarget
	agentConfig            domain.AgentConfig
	passthrough            passthroughConfig
	sessionID              string
	sessionOpened          bool
	baseLogger             *slog.Logger
	usage                  *agentcore.RunUsage
	usageMeasured          bool
	ledger                 *usage.Ledger
	mu                     sync.Mutex
	credentialVerification bool

	reducedVerificationContext bool

	// fork owns the turn subprocess lifecycle.
	fork *agentcore.ForkPerTurnSession

	// Per-turn state, reset by RunTurn and touched only from the hooks,
	// which run on RunTurn's goroutine.
	work  *agentcore.WorkObserver
	tools *agentcore.ToolTracker

	// sawMessageEnd records that the current pi turn already delivered its
	// message_end, so turn_end does not count the same response again.
	sawMessageEnd bool

	// responses holds the largest counts read per response identity, with
	// the model that served each. unsettled records a response that was not
	// counted, either because its usage named no identity or because it
	// carried none, which leaves the turn unable to settle.
	responses map[string]response
	unsettled bool

	// compacted records a compaction in the turn. Its summarization request
	// is a model call no message_end reports.
	compacted bool

	// ended records that the turn's agent_end arrived with no retry pending.
	ended bool

	// lastStopReason and lastErrorMessage describe the last assistant
	// response. pi retries a response that stopped with "error" inside the
	// same process, so only the last one decides the turn.
	lastStopReason   string
	lastErrorMessage string
}

// NewPiAdapter creates a [PiAdapter].
func NewPiAdapter() (domain.AgentAdapter, error) {
	return &PiAdapter{}, nil
}

// StartSession parses the session's settings, checks that the installed
// Pi is at least 0.99.0, and initializes adapter-owned session state
// without starting a turn subprocess. It refuses an unusable settings
// block before launching anything, and an unreadable or older version
// once the version is read. A resumed session is continued only when pi's
// session file for it is present on the session's host; otherwise the session
// starts new.
func (a *PiAdapter) StartSession(ctx context.Context, params domain.StartSessionParams) (domain.Session, error) {
	pt, fault := parsePassthroughConfig(params.Settings)
	if fault != nil {
		return domain.Session{}, agentcore.SettingsError(fault.Error(), fault)
	}

	target, agentErr := agentcore.ResolveLaunchTarget(params, defaultCommand)
	if agentErr != nil {
		return domain.Session{}, agentErr
	}

	ledger, ledgerErr := usage.NewLedger(usageSources.Live, usageSources.Records)
	if ledgerErr != nil {
		return domain.Session{}, &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: "usage ledger refused the kind's sources",
			Err:     ledgerErr,
		}
	}

	state := &sessionState{
		target:                 target,
		agentConfig:            params.AgentConfig,
		passthrough:            pt,
		baseLogger:             slog.Default().With(slog.String("component", "pi-adapter")),
		usage:                  agentcore.NewRunUsage(),
		ledger:                 ledger,
		credentialVerification: params.CredentialVerification,

		reducedVerificationContext: params.ReducedVerificationContext(),
	}
	if versionErr := checkRuntimeVersion(ctx, state); versionErr != nil {
		return domain.Session{}, versionErr
	}

	start, startErr := agentcore.ResolveSessionStart(ctx, params.ResumeSessionID,
		sessionProbe(target, agentcore.ReadTimeout(params.AgentConfig)), state.baseLogger)
	if startErr != nil {
		return domain.Session{}, startErr
	}
	state.sessionID = start.ResumeID()
	ledger.BeginSession(start.CarriedHistory(), nil)
	state.fork = newForkSession(state)

	return domain.Session{
		ID:             state.sessionID,
		AgentPID:       "",
		Internal:       state,
		CarriedHistory: start.CarriedHistory(),
	}, nil
}

// RunTurn executes one pi turn on the session's
// [agentcore.ForkPerTurnSession] and relays normalized events via
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

	state.resetTurn()

	return state.fork.RunTurn(ctx, params.Prompt, params.OnEvent)
}

// resetTurn clears the per-turn state before a turn starts.
func (s *sessionState) resetTurn() {
	s.work = agentcore.NewWorkObserver(agentcore.WorkSignals{AssistantOutput: true, ToolActivity: true})
	s.tools = agentcore.NewToolTracker()
	s.sawMessageEnd = false
	s.responses = nil
	s.unsettled = false
	s.compacted = false
	s.ended = false
	s.lastStopReason = ""
	s.lastErrorMessage = ""
}

// StopSession terminates any active subprocess.
func (a *PiAdapter) StopSession(ctx context.Context, session domain.Session) error {
	state, ok := session.Internal.(*sessionState)
	if !ok {
		return &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: fmt.Sprintf("unexpected session internal type %T", session.Internal),
		}
	}
	return state.fork.Stop(ctx)
}

// newForkSession builds the skeleton that runs state's turns.
func newForkSession(state *sessionState) *agentcore.ForkPerTurnSession {
	hooks := agentcore.ForkPerTurnHooks{
		BuildArgs: func(_ int, _ string) []string {
			return buildRunArgs(state, state.passthrough)
		},
		ParseLine: state.parseLine,
		GetUsage: func() (domain.TokenUsage, map[string]domain.TokenUsage, bool) {
			return state.usage.Snapshot(), state.usage.ByModel(), state.usageMeasured
		},
		SettleUsage:       state.settleUsage,
		GetSessionID:      state.currentSessionID,
		OnFinalize:        state.finalizeTurn,
		PromptOnStdin:     true,
		FirstEventTimeout: agentcore.ReadTimeout(state.agentConfig),
	}
	return agentcore.NewForkPerTurnSession(&state.target, hooks, state.baseLogger, state.agentConfig.StopGraceMS)
}

// parseLine handles one stdout line as the skeleton's ParseLine hook. It
// returns a non-nil result for the agent_end that ends the run.
func (s *sessionState) parseLine(line []byte, emit func(domain.AgentEvent), _ string, spend *agentcore.TurnSpend) (any, error) {
	event, err := parseRunEvent(line)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	malformed := func(reason error) {
		emit(domain.AgentEvent{Type: domain.EventMalformed, Timestamp: now, Message: reason.Error()})
	}

	switch event.Type {
	case "session":
		id, cwd, err := event.sessionHeader()
		if err != nil {
			malformed(err)
			return nil, nil
		}
		if problem := s.checkSessionHeader(id, cwd); problem != "" {
			return nil, agentcore.EndTurn(domain.ErrResponseError, problem)
		}
		if s.applySessionEvent(id) {
			emit(domain.AgentEvent{
				Type:      domain.EventSessionStarted,
				Timestamp: now,
				SessionID: s.currentSessionID(),
				Message:   "session started",
			})
		}

	case "turn_start":
		s.sawMessageEnd = false
		agentcore.EmitNotification(emit, "")

	case "message_update":
		delta, err := event.assistantDelta()
		if err != nil {
			malformed(err)
			return nil, nil
		}
		switch delta.Type {
		case "text_start", "text_delta", "thinking_start", "thinking_delta":
			s.work.ObserveAssistantOutput()
			spend.MarkModelReached()
		case "toolcall_start", "toolcall_end":
			s.work.ObserveToolActivity()
			spend.MarkModelReached()
		}
		agentcore.EmitNotification(emit, "")

	case "message_end":
		msg, err := event.completedMessage()
		if err != nil {
			malformed(err)
			s.unsettled = true
			s.forgetLastResponse()
			return nil, nil
		}
		if msg.Role == "assistant" {
			s.sawMessageEnd = true
			s.recordResponse(msg, emit, spend)
		}

	case "turn_end":
		msg, err := event.completedMessage()
		if err != nil {
			malformed(err)
			s.unsettled = s.unsettled || !s.sawMessageEnd
			if !s.sawMessageEnd {
				s.forgetLastResponse()
			}
			return nil, nil
		}
		if msg.Role == "assistant" && !s.sawMessageEnd {
			s.recordResponse(msg, emit, spend)
		}

	case "tool_execution_start":
		if event.ToolCallID != "" {
			s.tools.Begin(event.ToolCallID, agentcore.MapToolCall(toolMappings, event.ToolName, event.Args))
		}

	case "entry_appended":
		entryType := event.entryType()
		if entryType == "compaction" {
			s.compacted = true
		}
		msg, warm, err := event.cacheWarmResponse()
		if err != nil {
			malformed(err)
			s.unsettled = s.unsettled || entryType == "usage"
			return nil, nil
		}
		if warm {
			s.recordUsage(msg, emit, spend)
		}

	case "compaction_start":
		s.compacted = true
		agentcore.EmitNotification(emit, "")

	case "compaction_end":
		if !event.Aborted && event.ErrorMessage == "" {
			spend.MarkModelReached()
		}
		agentcore.EmitNotification(emit, "")

	case "tool_execution_update", "auto_retry_start", "auto_retry_end":
		agentcore.EmitNotification(emit, "")

	case "tool_execution_end":
		if event.ToolName == "" {
			malformed(fmt.Errorf("tool_execution_end carries no toolName"))
			return nil, nil
		}
		call, durationMS, _ := s.tools.End(event.ToolCallID)
		s.work.ObserveToolActivity()
		spend.MarkModelReached()
		if event.DurationMS > 0 {
			durationMS = event.DurationMS
		}
		var message string
		if event.IsError {
			message = redact.Truncate(event.resultText(), 500)
		}
		emit(domain.AgentEvent{
			Type:           domain.EventToolResult,
			Timestamp:      now,
			ToolName:       event.ToolName,
			ToolKind:       call.Kind,
			ToolArgument:   call.Argument,
			ToolDurationMS: durationMS,
			ToolError:      event.IsError,
			Message:        message,
		})

	case "agent_start":
		s.ended = false

	case "agent_end":
		if !event.WillRetry {
			s.ended = true
			return &event, nil
		}

	default:
		if !event.known() {
			malformed(fmt.Errorf("unknown pi event type %q", event.Type))
		}
	}

	return nil, nil
}

// forgetLastResponse is called when a message that may be the last assistant
// response cannot be decoded. The earlier response's stop reason no longer
// describes the run, so the outcome is reported as unknown.
func (s *sessionState) forgetLastResponse() {
	s.lastStopReason = "undecodable"
	s.lastErrorMessage = ""
}

// recordResponse takes in one completed assistant response: its text, its
// stop reason, and its usage.
func (s *sessionState) recordResponse(msg rawMessage, emit func(domain.AgentEvent), spend *agentcore.TurnSpend) {
	s.lastStopReason = msg.StopReason
	s.lastErrorMessage = msg.ErrorMessage
	if text := msg.text(); text != "" {
		s.work.ObserveAssistantOutput()
		agentcore.EmitNotification(emit, redact.Truncate(text, 500))
	}
	s.recordUsage(msg, emit, spend)
}

// recordUsage takes in the usage of one billed request. A failed request is
// reported with zero counts, which the provider did not bill, so an empty one
// is not output the turn has to account for. A failed request that produced
// output and reports no counts is.
func (s *sessionState) recordUsage(msg rawMessage, emit func(domain.AgentEvent), spend *agentcore.TurnSpend) {
	failed := msg.StopReason == "error" || msg.StopReason == "aborted"
	reading, ok := msg.usageReading()
	output := msg.hasOutput()
	if ok || !failed || output {
		spend.MarkModelReached()
	}
	if !ok {
		if !failed || output {
			s.unsettled = true
		}
		return
	}
	if reading.Identity == "" {
		s.unsettled = true
	} else {
		s.holdResponse(msg, reading.Identity)
	}
	provisional, isNew := s.ledger.ObserveRequest(reading)
	if !isNew {
		return
	}
	emit(domain.AgentEvent{
		Type:         domain.EventTokenUsage,
		Timestamp:    time.Now().UTC(),
		Usage:        s.usage.SetTurnProvisional(provisional),
		UsageByModel: s.usage.ByModel(),
		Model:        strings.TrimSpace(reading.Model),
	})
}

// settleUsage hands the turn's response reports to the ledger. The runtime
// ends a turn itself when agent_end arrives with no retry pending.
func (s *sessionState) settleUsage(ctx context.Context, _ func(domain.AgentEvent), spend *agentcore.TurnSpend, _ bool) {
	end := usage.TurnEnd{Replied: s.ended && !s.unsettled && !s.compacted, ModelReached: spend.ModelReached()}
	noRecords := func(context.Context) usage.RecordsRead { return usage.RecordsRead{} }
	recovered := agentcore.SettleSpend(ctx, s.ledger, end, s.turnReading(), noRecords, agentcore.ReadTimeout(s.agentConfig), spend)
	if recovered != nil {
		s.usage.SetRunCumulative(recovered.Run, recovered.ByModel)
		s.usageMeasured = true
	}
}

// finalizeTurn builds the disposition of a turn whose process exited. pi
// exits 0 for a response that stopped in failure, so the last response's
// stop reason is the only evidence of how the run ended.
func (s *sessionState) finalizeTurn(emit func(domain.AgentEvent), lastParsed any, exitCode int, waitErr error, _ []string, earlyExit *domain.AgentError, spend *agentcore.TurnSpend) (domain.TurnResult, *domain.AgentError) {
	ev := agentcore.TurnEvidence{
		ExitObserved: true,
		ExitCode:     exitCode,
		WaitErr:      waitErr,
		EarlyExit:    earlyExit,
	}
	ev.Work, ev.WorkDetail = s.work.Report()

	if lastParsed != nil && s.ended {
		s.applyStopReason(&ev)
	}

	meta := agentcore.TurnMeta{
		SessionID:     s.currentSessionID(),
		Usage:         s.usage.Snapshot(),
		UsageByModel:  s.usage.ByModel(),
		UsageMeasured: s.usageMeasured,
	}
	return agentcore.FinalizeTurn(emit, s.logger(), ev, meta, *spend)
}

// applyStopReason sets ev's terminal report from the last response's stop
// reason. A run that ended without any assistant response leaves it unset, and
// so does one that ended on a tool-use response, which pi ends the run on when
// every tool of the batch asks it to terminate.
func (s *sessionState) applyStopReason(ev *agentcore.TurnEvidence) {
	switch s.lastStopReason {
	case "", "toolUse":
	case "stop":
		ev.Terminal = agentcore.TerminalSuccess
	case "length":
		ev.Terminal = agentcore.TerminalFailure
		ev.TerminalErrorKind = domain.ErrTurnTokenLimit
		ev.TerminalMessage = "pi stopped the response at the output token limit"
	case "error", "aborted":
		ev.Terminal = agentcore.TerminalFailure
		ev.TerminalMessage = redact.Truncate(s.lastErrorMessage, 500)
		if ev.TerminalMessage == "" {
			ev.TerminalMessage = "pi reported stop reason " + s.lastStopReason
		}
	default:
		ev.Terminal = agentcore.TerminalFailure
		ev.TerminalErrorKind = domain.ErrTurnOutcomeUnknown
		ev.TerminalMessage = redact.Truncate(fmt.Sprintf("pi reported an unrecognized stop reason: %q", s.lastStopReason), 500)
	}
}

// checkSessionHeader validates a session header against the session's own
// identity and workspace, and returns the problem it found or "". The
// workspace is compared only for a local launch, where the header's cwd and
// the workspace are compared as the same directory rather than as strings.
// A remote launch's paths name the remote host's file system, which Sortie
// cannot inspect, so only the session id is checked.
func (s *sessionState) checkSessionHeader(id, cwd string) string {
	if s.target.RemoteCommand == "" && !sameDir(s.target.WorkspacePath, cwd) {
		return fmt.Sprintf("session %q runs in %q, want the turn's workspace %q", id, cwd, s.target.WorkspacePath)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessionID != "" && s.sessionID != id {
		return fmt.Sprintf("session id mismatch: expected %q, got %q", s.sessionID, id)
	}
	return ""
}

func sameDir(a, b string) bool {
	infoA, err := os.Stat(a)
	if err != nil {
		return false
	}
	infoB, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(infoA, infoB)
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

// sessionDirName is the name pi gives the session directory of a working
// directory: the path without its leading separator, with every separator and
// colon replaced by "-", between "--" marks.
func sessionDirName(workingDir string) string {
	trimmed := strings.TrimPrefix(strings.TrimPrefix(filepath.Clean(workingDir), "/"), `\`)
	return "--" + strings.NewReplacer("/", "-", `\`, "-", ":", "-").Replace(trimmed) + "--"
}

// sessionProbe checks for pi's session file of the id in the session
// directory of the workspace only, because pi opens a session found under
// another project directory through a prompt on standard input. pi names that
// directory after its physical working directory, which a local launch
// resolves here and a remote launch cannot, so a symlinked remote workspace
// finds no file. A directory name holding a character the matcher reads as a
// wildcard ("*", "?", "[" or "]") cannot be matched literally, and the check
// then gives no answer.
func sessionProbe(target agentcore.LaunchTarget, bound time.Duration) agentcore.ResumeProbe[agentcore.RuntimePath] {
	workspace := target.WorkspacePath
	if target.RemoteCommand == "" {
		if resolved, err := filepath.EvalSymlinks(workspace); err == nil {
			workspace = resolved
		}
	}
	dir := sessionDirName(workspace)
	if strings.ContainsAny(dir, "*?[]") {
		return agentcore.ResumeProbeFunc[agentcore.RuntimePath](func(context.Context, string) (agentcore.ResumeSignal[agentcore.RuntimePath], *domain.AgentError) {
			return agentcore.ResumeSignal[agentcore.RuntimePath]{}, nil
		})
	}
	return agentcore.RuntimeFileProbe(target, bound, func(id string) agentcore.RuntimePath {
		return agentcore.RuntimePath{
			HomeVar:     "PI_CODING_AGENT_DIR",
			HomeDefault: []string{".pi", "agent"},
			Segments:    []string{"sessions", dir, "*_" + id + ".jsonl"},
		}
	})
}
