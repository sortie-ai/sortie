// Package claude implements [domain.AgentAdapter] for the Claude Code
// CLI. It launches Claude Code as a subprocess in headless mode,
// reads newline-delimited JSON from stdout, and normalizes events into
// domain types. Registered under kind "claude-code" via an init
// function. Safe for concurrent use: each [ClaudeCodeAdapter.RunTurn]
// call operates on an independent subprocess.
package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/claude/ratelimit"
	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/redact"
	"github.com/sortie-ai/sortie/internal/registry"
)

// defaultCommand is what a session launches when it is given no command.
const defaultCommand = "claude"

// effortEnvName is the variable Claude Code lets outrank --effort without
// reporting it, so a session that sets the level launches without it.
const effortEnvName = "CLAUDE_CODE_EFFORT_LEVEL"

// unknownEffortPrefix starts the standard-error line Claude Code writes
// when it does not recognize the --effort value and runs at the model's
// default level instead.
const unknownEffortPrefix = "Warning: Unknown --effort value"

// usageSources declares what each source counts. The stream reports each
// process's own requests split by served model and carries no history of
// earlier processes; the journal holds one line per completed request and
// keeps the history of earlier runs in place.
var usageSources = usage.Sources{
	Live: usage.Source{
		Category: usage.SourceOutput,
		Strategy: usage.StrategyLive,
		Windows:  ratelimit.Windows,
		Spend: usage.SpendDeclaration{
			Shape:             usage.SpendPerTurn,
			ReasoningInOutput: true,
			ServedModel:       true,
			DuringTurn:        true,
		},
	},
	Records: usage.Source{
		Category: usage.SourceJournal,
		Strategy: usage.StrategyRecords,
		Spend: usage.SpendDeclaration{
			Shape:             usage.SpendPerRequest,
			ReasoningInOutput: true,
			ServedModel:       true,
		},
	},
}

func init() {
	registry.Agents.RegisterWithMeta("claude-code", NewClaudeCodeAdapter, registry.AgentMeta{
		RequiresCommand:        true,
		DefaultCommand:         defaultCommand,
		ValidateAgentConfig:    validateConfig,
		MCPDialect:             registry.MCPDialectMCPServersJSONAlwaysLoad,
		SessionResumeBlockedBy: sessionResumeBlockedBy,
		UsageSources:           usageSources,
		UsageLimitRecognition:  registry.UsageLimitRecognized,
		CredentialEnv:          registry.DeclareCredentialEnv("ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"),
		VerificationContext:    registry.DeclareVerificationContext(),
		EffortForwarding:       registry.EffortForwarded,
	})
}

// Compile-time interface satisfaction check.
var _ domain.AgentAdapter = (*ClaudeCodeAdapter)(nil)

// ansiEscapeRE matches VT100/ANSI CSI SGR escape sequences emitted by CLI
// tools for color and formatting (e.g. \x1b[31m, \x1b[0m). Compiled once at
// program startup; applied inside stripClaudeMarkup.
var ansiEscapeRE = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// ClaudeCodeAdapter satisfies [domain.AgentAdapter] by managing Claude
// Code CLI subprocesses. One adapter instance serves all concurrent
// sessions; per-session state is held in [sessionState] via the
// [domain.Session] Internal field.

// syntheticModel is the model Claude Code stamps on a message it writes
// itself, such as an API error or an interruption notice. It names no model,
// so it must not replace the model that did the work.
const syntheticModel = "<synthetic>"

const authenticationFailedError = "authentication_failed"

type ClaudeCodeAdapter struct{}

// sessionState is adapter-internal state stored in [domain.Session]
// Internal. It tracks the Claude Code session ID and per-turn scan
// state across turns.
type sessionState struct {
	target          agentcore.LaunchTarget
	claudeSessionID string
	isContinuation  bool
	passthrough     passthroughConfig
	agentConfig     domain.AgentConfig
	baseLogger      *slog.Logger

	// mcpConfigPath is the derived MCP config file; empty on a remote launch.
	mcpConfigPath string

	credentialVerification bool

	reducedVerificationContext bool

	// forkSession owns the subprocess lifecycle for this session.
	forkSession *agentcore.ForkPerTurnSession

	// ledger counts the session's spend from the stream and the journal.
	ledger *usage.Ledger

	// journalDir is the journal's project directory name: the working
	// directory the runtime reports, or the workspace path before it has.
	journalDir string

	// subagents holds the task ids the stream named, in first-seen order.
	subagents []string

	// acc holds the session's run-cumulative token usage. Constructed
	// once in StartSession and never reset between turns.
	acc *agentcore.RunUsage

	// usageMeasured reports whether a streamed request or a settled figure
	// has been observed for this session. Monotone: set true once and never
	// cleared.
	usageMeasured bool

	// Per-turn scan state owned by the ParseLine and OnFinalize hook
	// closures. Reset at the top of each RunTurn call before delegating
	// to forkSession.
	turnReplied      bool
	turnMembers      []json.RawMessage
	apiCallStart     time.Time
	emittedAPITiming bool
	inFlight         *agentcore.ToolTracker
	work             *agentcore.WorkObserver

	// humanInputEnds and humanInputDetail record a recognized request that
	// only a person could answer, observed in ParseLine on a
	// system/permission_denied event whose [agentcore.RefusalPosture] read
	// EndAttempt true. OnFinalize applies the consequence, ahead of its
	// normal disposition logic, because ParseLine runs while the
	// subprocess is still active and cannot itself call
	// [agentcore.FinalizeTurn].
	humanInputEnds   bool
	humanInputDetail string

	// authRefused records an assistant event reporting a refused login,
	// observed in ParseLine and applied in OnFinalize to a failed result.
	authRefused bool

	// rateLimit is the latest rate_limit_event of the turn, observed in
	// ParseLine and read in OnFinalize to tell a subscription refusal from
	// an API-key 429.
	rateLimit *ratelimit.Info

	// effortReported records that the session has already logged the
	// runtime's report of an unrecognized effort value. Touched only from
	// the OnFinalize hook.
	effortReported bool
}

func (s *sessionState) logger() *slog.Logger {
	if s.claudeSessionID == "" {
		return s.baseLogger
	}
	return logging.WithSession(s.baseLogger, s.claudeSessionID)
}

// relayEffortReport logs the first standard-error line that starts with
// [unknownEffortPrefix] as a warning, once per session, so the operator
// learns the configured level was not applied. It does nothing when no
// effort is configured.
func (s *sessionState) relayEffortReport(effort string, stderrLines []string) {
	if s.effortReported || effort == "" {
		return
	}
	idx := slices.IndexFunc(stderrLines, func(line string) bool {
		return strings.HasPrefix(line, unknownEffortPrefix)
	})
	if idx < 0 {
		return
	}
	s.logger().Warn("reasoning level not applied by the agent",
		slog.String("agent_kind", "claude-code"),
		slog.String(registry.EffortKey, effort),
		slog.String("line", stderrLines[idx]))
	s.effortReported = true
}

// NewClaudeCodeAdapter creates a [ClaudeCodeAdapter]. Settings and command
// resolution are deferred to [ClaudeCodeAdapter.StartSession].
func NewClaudeCodeAdapter() (domain.AgentAdapter, error) {
	return &ClaudeCodeAdapter{}, nil
}

// askUserQuestionTool is the built-in Claude Code tool name that carries a
// genuine question to a person. A system/permission_denied event naming it
// is a request for human input rather than a request for consent to act,
// even though the runtime resolves both through the same denial mechanism.
const askUserQuestionTool = "AskUserQuestion"

// detailAnswerToQuestion names, for the operator, the specific ask behind
// a recognized request for human input, appended to
// [agentcore.RefusalPosture]'s notice stem and to the turn's terminal
// message.
const detailAnswerToQuestion = "an answer to a question"

// StartSession parses the session's settings, validates the workspace path,
// resolves the claude binary, and initializes per-session state. No
// subprocess is spawned; that happens in [ClaudeCodeAdapter.RunTurn].
func (a *ClaudeCodeAdapter) StartSession(ctx context.Context, params domain.StartSessionParams) (domain.Session, error) {
	pt, fault := parsePassthroughConfig(params.Settings)
	if fault != nil {
		return domain.Session{}, agentcore.SettingsError(fault.Error(), fault)
	}
	target, agentErr := agentcore.ResolveLaunchTarget(params, defaultCommand)
	if agentErr != nil {
		return domain.Session{}, agentErr
	}
	if pt.Effort != "" {
		target.WithheldEnv = []string{effortEnvName}
	}

	isContinuation := false
	sessionUUID := ""
	if params.ResumeSessionID != "" {
		sessionUUID = params.ResumeSessionID
		isContinuation = true
	} else {
		uuid, uuidErr := agentcore.NewUUIDv4()
		if uuidErr != nil {
			return domain.Session{}, &domain.AgentError{
				Kind:    domain.ErrAgentNotFound,
				Message: "could not generate a session id",
				Err:     uuidErr,
			}
		}
		sessionUUID = uuid
	}

	ledger, ledgerErr := usage.NewLedger(usageSources.Live, usageSources.Records)
	if ledgerErr != nil {
		return domain.Session{}, &domain.AgentError{
			Kind:    domain.ErrResponseError,
			Message: "usage ledger refused the kind's sources",
			Err:     ledgerErr,
		}
	}

	baseLogger := slog.Default().With(slog.String("component", "claude-adapter"))

	tools, toolsErr := agentcore.ResolveToolDelivery(ctx, params, target, registry.MCPDialectMCPServersJSONAlwaysLoad, baseLogger)
	if toolsErr != nil {
		return domain.Session{}, toolsErr
	}

	mcpConfigPath := ""
	if target.RemoteCommand == "" && tools.Configured() && !params.CredentialVerification {
		var err error
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
		claudeSessionID:        sessionUUID,
		isContinuation:         isContinuation,
		passthrough:            pt,
		agentConfig:            params.AgentConfig,
		baseLogger:             baseLogger,
		mcpConfigPath:          mcpConfigPath,
		credentialVerification: params.CredentialVerification,
		ledger:                 ledger,
		journalDir:             journalDir(target.WorkspacePath),
		acc:                    agentcore.NewRunUsage(),

		reducedVerificationContext: params.ReducedVerificationContext(),
	}

	if isContinuation {
		readCtx, cancel := context.WithTimeout(ctx, agentcore.ReadTimeout(params.AgentConfig))
		ledger.Boundary(state.readJournal(readCtx))
		cancel()
	} else {
		ledger.SessionCreated()
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
			case "system":
				switch event.Subtype {
				case "init":
					if event.SessionID != "" {
						state.claudeSessionID = event.SessionID
					}
					if event.Cwd != "" {
						state.journalDir = journalDir(event.Cwd)
					}
					agentcore.EmitSessionStarted(emit, pid, state.claudeSessionID)
					// The first API call is imminent. Start the monotonic timer.
					state.apiCallStart = time.Now()
				case "api_retry":
					agentcore.EmitNotification(emit, formatAPIRetry(event))
				case "permission_denied":
					// The runtime resolved this request on its own: --dangerously-
					// skip-permissions (or a validated bypassPermissions mode)
					// exposes no reply channel, so replyChannel is false on every
					// row this event reads. AskUserQuestion is the one built-in
					// tool that carries a genuine question rather than a request
					// for consent to act; every other tool name is a permission
					// request.
					class := agentcore.ClassPermission
					detail := ""
					if event.ToolName == askUserQuestionTool {
						class = agentcore.ClassHumanInput
						detail = detailAnswerToQuestion
					}
					posture := agentcore.DecideHumanRequest(class, false, agentcore.AnswerRuntimeRefused)
					if posture.EndAttempt {
						state.humanInputEnds = true
						state.humanInputDetail = detail
					}
					agentcore.EmitNotification(emit, posture.NoticeWithDetail(detail))
				case "task_started", "task_progress", "task_notification":
					// A sub-agent task event proves the model served this turn
					// output; its usage figure is a context size, not spend.
					spend.MarkModelReached()
					if event.Subtype == "task_started" && event.TaskID != "" && !slices.Contains(state.subagents, event.TaskID) {
						state.subagents = append(state.subagents, event.TaskID)
					}
					agentcore.EmitNotification(emit, "")
				default:
					// Task and background-task bookkeeping carries no
					// operator-readable text; a blank ping keeps stall
					// detection alive without cluttering the run timeline.
					agentcore.EmitNotification(emit, "")
				}

			case "assistant":
				if event.ErrorField == authenticationFailedError {
					state.authRefused = true
				}
				if len(event.Message) > 0 {
					var meta rawAssistantMessageMeta
					if err := json.Unmarshal(event.Message, &meta); err == nil {
						if meta.Model != "" && meta.Model != syntheticModel {
							spend.MarkModelReached()
						}
						if reading, ok := requestReading(meta); ok {
							provisional, isNew := state.ledger.ObserveRequest(reading)
							snapshot := state.acc.SetTurnProvisional(provisional)

							// Claude Code repeats one message id across every
							// streamed event of the same model request; only the
							// id's first sighting represents a new API request.
							if isNew {
								state.usageMeasured = true
								tokenEvt := domain.AgentEvent{
									Type:         domain.EventTokenUsage,
									Timestamp:    now,
									Usage:        snapshot,
									UsageByModel: state.acc.ByModel(),
									Model:        reading.Model,
								}
								if !state.apiCallStart.IsZero() {
									dur := time.Since(state.apiCallStart).Milliseconds()
									if dur <= 0 {
										dur = 1 // clamp so the orchestrator accumulates this measurement
									}
									tokenEvt.APIDurationMS = dur
									state.apiCallStart = time.Time{}
									state.emittedAPITiming = true
								}
								emit(tokenEvt)
							}
						}
					}
				}
				for _, block := range event.contentBlocks() {
					if block.Type == "text" && block.Text != "" {
						state.work.ObserveAssistantOutput()
						break
					}
				}
				// ToolTracker.Begin stores time.Now() internally, so no separate
				// monotonic timestamp is needed before calling processToolBlocks.
				processToolBlocks(event.contentBlocks(), state.inFlight, state.work, now, emit)
				agentcore.EmitNotification(emit, summarizeAssistant(event))

			case "user":
				// Claude Code emits tool results as user-role messages.
				processToolBlocks(event.contentBlocks(), state.inFlight, state.work, now, emit)
				state.apiCallStart = time.Now() // next API call is imminent

			case "result":
				captured := event
				state.turnReplied = true
				state.turnMembers = append(state.turnMembers, event.ModelUsageSinceLastResult)
				return &captured, nil

			case "rate_limit_event":
				if event.RateLimitInfo != nil {
					state.rateLimit = event.RateLimitInfo
				}
				at := time.Now().UTC()
				agentcore.EmitUsageWindows(emit, at, usage.Windows(usageSources.Live, event.RateLimitInfo.Readings(), at))

			case "stream_event":
				agentcore.EmitNotification(emit, "")

			default:
				agentcore.EmitNotification(emit, "")
			}

			return nil, nil
		},
		SettleUsage: func(ctx context.Context, _ func(domain.AgentEvent), spend *agentcore.TurnSpend, _ bool) {
			end := usage.TurnEnd{Replied: state.turnReplied, ModelReached: spend.ModelReached()}
			recovered := agentcore.SettleSpend(ctx, state.ledger, end, liveReading(state.turnMembers),
				state.readJournal, agentcore.ReadTimeout(state.agentConfig), spend)
			if recovered != nil {
				state.acc.SetRunCumulative(recovered.Run, recovered.ByModel)
				state.usageMeasured = true
			}
		},
		GetUsage: func() (domain.TokenUsage, map[string]domain.TokenUsage, bool) {
			return state.acc.Snapshot(), state.acc.ByModel(), state.usageMeasured
		},
		GetSessionID: func() string { return state.claudeSessionID },
		OnFinalize: func(emit func(domain.AgentEvent), lastParsed any, exitCode int, waitErr error, stderrLines []string, earlyExit *domain.AgentError, spend *agentcore.TurnSpend) (domain.TurnResult, *domain.AgentError) {
			usage := state.acc.Snapshot()

			finalize := func(ev agentcore.TurnEvidence, meta agentcore.TurnMeta) (domain.TurnResult, *domain.AgentError) {
				result, agentErr := agentcore.FinalizeTurn(emit, state.logger(), ev, meta, *spend)
				if agentErr == nil {
					state.relayEffortReport(state.passthrough.Effort, stderrLines)
				}
				return result, agentErr
			}

			// A recognized request that only a person could answer, observed
			// in ParseLine, overrides the turn's normal success/failure
			// disposition regardless of how the subprocess exited.
			if state.humanInputEnds {
				evidence := agentcore.HumanInputEvidence(state.humanInputDetail)
				meta := agentcore.TurnMeta{
					SessionID:     state.claudeSessionID,
					Usage:         usage,
					UsageByModel:  state.acc.ByModel(),
					UsageMeasured: state.usageMeasured,
				}
				return finalize(evidence, meta)
			}

			lastResult, _ := lastParsed.(*rawEvent)

			ev := agentcore.TurnEvidence{
				ExitObserved: true,
				ExitCode:     exitCode,
				WaitErr:      waitErr,
				EarlyExit:    earlyExit,
			}
			ev.Work, ev.WorkDetail = state.work.Report()

			var turnAPIDuration int64
			if lastResult != nil {
				// Use turn-level duration_api_ms only when no per-request
				// API timing was emitted, to avoid double-counting.
				if !state.emittedAPITiming {
					turnAPIDuration = lastResult.DurationAPI
				}
				if lastResult.Subtype == "success" && !lastResult.IsError {
					ev.Terminal = agentcore.TerminalSuccess
					ev.TerminalMessage = redact.Truncate(lastResult.Result, 500)
				} else {
					ev.Terminal = agentcore.TerminalFailure
					if strings.TrimSpace(lastResult.Result) != "" {
						ev.TerminalMessage = redact.Truncate(lastResult.Result, 500)
					} else {
						ev.TerminalMessage = lastResult.Subtype
					}
					if state.authRefused {
						ev.TerminalErrorKind = domain.ErrCredentialUnverified
						ev.TerminalMessage = agentcore.CredentialRefusedMessage(ev.TerminalMessage)
					} else if limit := usageLimitOf(lastResult, state.rateLimit); limit != nil {
						ev.TerminalErrorKind = domain.ErrUsageLimit
						ev.UsageLimit = limit
					}
				}
			}

			meta := agentcore.TurnMeta{
				SessionID:     state.claudeSessionID,
				Usage:         usage,
				UsageByModel:  state.acc.ByModel(),
				UsageMeasured: state.usageMeasured,
				APIDurationMS: turnAPIDuration,
			}

			return finalize(ev, meta)
		},
		EmitSessionStartID: nil, // Claude emits EventSessionStarted from ParseLine on "system/init"
	}

	state.forkSession = agentcore.NewForkPerTurnSession(&state.target, hooks, state.baseLogger, state.agentConfig.StopGraceMS)

	return domain.Session{
		ID:            sessionUUID,
		Internal:      state,
		NoToolChannel: tools.NoToolChannel(),
	}, nil
}

// RunTurn executes one agent turn by delegating to the session's
// [agentcore.ForkPerTurnSession]. Per-turn scan state is reset before
// delegation so each turn starts with a fresh accumulator and clean state.
func (a *ClaudeCodeAdapter) RunTurn(ctx context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
	if params.OnEvent == nil {
		panic("claude: OnEvent must be non-nil")
	}

	state, ok := session.Internal.(*sessionState)
	if !ok {
		return domain.TurnResult{}, &domain.AgentError{
			Kind:    domain.ErrPortExit,
			Message: fmt.Sprintf("unexpected session internal type %T", session.Internal),
		}
	}

	// Reset per-turn scan state before delegation. state.acc, state.ledger,
	// and state.subagents are constructed once in StartSession and carry the
	// run's usage across turns, so they are not reset here.
	state.turnReplied = false
	state.turnMembers = nil
	state.apiCallStart = time.Time{}
	state.emittedAPITiming = false
	state.inFlight = agentcore.NewToolTracker()
	state.work = agentcore.NewWorkObserver(agentcore.WorkSignals{AssistantOutput: true, ToolActivity: true})
	state.humanInputEnds = false
	state.humanInputDetail = ""
	state.authRefused = false
	state.rateLimit = nil

	return state.forkSession.RunTurn(ctx, params.Prompt, params.OnEvent)
}

// usageLimitOf returns the refusal a failed result reports, or nil when the
// result is not a subscription allowance refusal. An API-key 429 carries no
// rejected rate_limit_event, and an overage that is not itself rejected means
// the account can still spend, so neither counts.
func usageLimitOf(result *rawEvent, info *ratelimit.Info) *domain.UsageLimit {
	if !result.IsError || result.APIErrorStatus != http.StatusTooManyRequests {
		return nil
	}
	if info == nil || info.Status != "rejected" {
		return nil
	}
	if info.OverageStatus != "" && info.OverageStatus != "rejected" {
		return nil
	}
	limit := &domain.UsageLimit{
		ModelOnly: ratelimit.ModelOnly(info.RateLimitType),
	}
	if info.ResetsAt > 0 {
		limit.ResetsAt = time.Unix(info.ResetsAt, 0).UTC()
	}
	return limit
}

// StopSession terminates a running Claude Code subprocess gracefully by
// delegating to the session's [agentcore.ForkPerTurnSession].
func (a *ClaudeCodeAdapter) StopSession(ctx context.Context, session domain.Session) error {
	state, ok := session.Internal.(*sessionState)
	if !ok {
		return fmt.Errorf("unexpected session internal type %T", session.Internal)
	}
	if state.forkSession == nil {
		return nil
	}
	return state.forkSession.Stop(ctx)
}

// maxToolErrorLen is the maximum byte budget for [domain.AgentEvent.Message]
// when a tool_result block carries is_error: true. Output that exceeds this
// limit is formatted as first-line-plus-tail: the first line of the error
// output is preserved (typically an exit-code header), followed by the
// omission marker "\n...\n", followed by the last bytes of the remaining
// output, ensuring that CLI failure lines at the tail are always visible.
const maxToolErrorLen = 2048

// toolResultText extracts a human-readable string from a tool_result
// content block. It handles the two shapes emitted by Claude Code:
// a plain JSON string and an array of typed content objects. Returns
// an empty string when neither shape matches or when both Text and
// Content are empty.
func toolResultText(block rawContentBlock) string {
	if block.Text != "" {
		return block.Text
	}
	if len(block.Content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(block.Content, &s); err == nil && s != "" {
		return s
	}
	var contentObjects []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(block.Content, &contentObjects); err == nil {
		for _, co := range contentObjects {
			if co.Type == "text" && co.Text != "" {
				return co.Text
			}
		}
	}
	return ""
}

// stripClaudeMarkup removes Claude Code-specific markup from a raw error
// string before it reaches [domain.AgentEvent.Message]. First, if s is
// wrapped in a <tool_use_error>…</tool_use_error> envelope (Claude Code's
// internal error protocol), the outermost wrapper is stripped and the inner
// content is kept; inner angle brackets are left intact. Then, ANSI SGR
// escape sequences (e.g. \x1b[31m…\x1b[0m) are removed so that structured
// log fields contain plain text that grep can match directly. If neither
// condition applies the original string is returned unchanged.
func stripClaudeMarkup(s string) string {
	s = strings.TrimSpace(s)
	const open = "<tool_use_error>"
	const close = "</tool_use_error>"
	if strings.HasPrefix(s, open) && strings.HasSuffix(s, close) {
		s = strings.TrimSpace(s[len(open) : len(s)-len(close)])
	}
	return ansiEscapeRE.ReplaceAllString(s, "")
}

// tailBytes returns the last n bytes of s aligned to a valid UTF-8 rune
// boundary. If len(s) <= n, s is returned unchanged. When the n-byte suffix
// begins mid-rune, start is advanced past the continuation bytes so the
// result always starts on a rune boundary; the result may be up to 3 bytes
// shorter than n in that case.
func tailBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	start := len(s) - n
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}

// truncateToolError returns s within maxLen bytes, preserving the most useful
// content for operator log inspection. When s fits in maxLen it is returned
// unchanged. For longer strings the algorithm is first-line-plus-tail: the
// first line of s (up to the first '\n') is kept as a header, for CLI tools
// this is typically an exit-code line such as "Exit code 2". The omission
// marker "\n...\n" separates the header from the tail, and the remaining byte
// budget is filled by the last bytes of s after the first line, so that
// failure lines at the tail (e.g. "FAIL pkg 0.5s") are always included.
//
// When no newline is present, or when the first line alone exceeds the budget,
// tailBytes(s, maxLen) is returned instead. All truncation respects UTF-8 rune
// boundaries.
func truncateToolError(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	before, after, ok := strings.Cut(s, "\n")
	if !ok {
		return tailBytes(s, maxLen)
	}
	firstLine := before
	const sep = "\n...\n"
	tailBudget := maxLen - len(firstLine) - len(sep)
	if tailBudget <= 0 {
		return tailBytes(s, maxLen)
	}
	tail := tailBytes(after, tailBudget)
	return firstLine + sep + tail
}

// processToolBlocks scans content blocks for tool_use and tool_result
// entries. tool_use blocks are registered in inFlight; tool_result
// blocks are correlated against inFlight and emitted as
// [domain.EventToolResult] via onEvent. The wallTime parameter is the
// wall-clock timestamp written into emitted events. work observes tool
// activity at either block, because a requested call counts at any
// point in its life, not only at completion.
func processToolBlocks(
	blocks []rawContentBlock,
	inFlight *agentcore.ToolTracker,
	work *agentcore.WorkObserver,
	wallTime time.Time,
	onEvent func(domain.AgentEvent),
) {
	for _, block := range blocks {
		if block.Type == "tool_use" && block.ID != "" {
			work.ObserveToolActivity()
			inFlight.Begin(block.ID, toolCallOf(block.Name, block.Input))
		}
		if block.Type == "tool_result" {
			work.ObserveToolActivity()
			call, durationMS, _ := inFlight.End(block.ToolUseID)
			msg := "tool_result"
			if call.Name != "" {
				msg += ": " + call.Name
			}
			if block.IsError {
				if errText := toolResultText(block); errText != "" {
					msg = truncateToolError(redact.Mask(stripClaudeMarkup(errText)), maxToolErrorLen)
				}
			}
			onEvent(domain.AgentEvent{
				Type:           domain.EventToolResult,
				Timestamp:      wallTime,
				ToolName:       call.Name,
				ToolKind:       call.Kind,
				ToolArgument:   call.Argument,
				ToolDurationMS: durationMS,
				ToolError:      block.IsError,
				Message:        msg,
			})
		}
	}
}
