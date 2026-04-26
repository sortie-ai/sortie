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
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

func init() {
	registry.Agents.RegisterWithMeta("claude-code", NewClaudeCodeAdapter, registry.AgentMeta{
		RequiresCommand: true,
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
type ClaudeCodeAdapter struct {
	passthrough passthroughConfig
}

// sessionState is adapter-internal state stored in [domain.Session]
// Internal. It tracks the Claude Code session ID and per-turn scan
// state across turns.
type sessionState struct {
	target          agentcore.LaunchTarget
	claudeSessionID string
	isContinuation  bool
	agentConfig     domain.AgentConfig

	// mcpConfigPath is the worker-generated MCP config file path.
	mcpConfigPath string

	// forkSession owns the subprocess lifecycle for this session.
	forkSession *agentcore.ForkPerTurnSession

	// Per-turn scan state owned by the ParseLine and OnFinalize hook
	// closures. Reset at the top of each RunTurn call before delegating
	// to forkSession.
	acc              *agentcore.UsageAccumulator
	lastModel        string
	emittedUsage     bool
	apiCallStart     time.Time
	emittedAPITiming bool
	inFlight         *agentcore.ToolTracker
}

// NewClaudeCodeAdapter creates a [ClaudeCodeAdapter] from adapter
// configuration. The config parameter is the raw map from the
// "claude-code" sub-object in WORKFLOW.md. Command resolution is
// deferred to [ClaudeCodeAdapter.StartSession].
func NewClaudeCodeAdapter(config map[string]any) (domain.AgentAdapter, error) {
	pt := parsePassthroughConfig(config)
	return &ClaudeCodeAdapter{passthrough: pt}, nil
}

// StartSession validates the workspace path, resolves the claude binary, and
// initializes per-session state. No subprocess is spawned; that happens in
// [ClaudeCodeAdapter.RunTurn].
func (a *ClaudeCodeAdapter) StartSession(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
	target, agentErr := agentcore.ResolveLaunchTarget(params, "claude")
	if agentErr != nil {
		return domain.Session{}, agentErr
	}

	isContinuation := false
	sessionUUID := ""
	if params.ResumeSessionID != "" {
		sessionUUID = params.ResumeSessionID
		isContinuation = true
	} else {
		sessionUUID = newUUID()
	}

	state := &sessionState{
		target:          target,
		claudeSessionID: sessionUUID,
		isContinuation:  isContinuation,
		agentConfig:     params.AgentConfig,
		mcpConfigPath:   params.MCPConfigPath,
	}

	sessionLogger := logging.WithSession(slog.Default().With(slog.String("component", "claude-adapter")), sessionUUID)

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
			case "system":
				switch event.Subtype {
				case "init":
					if event.SessionID != "" {
						state.claudeSessionID = event.SessionID
					}
					agentcore.EmitSessionStarted(emit, pid, state.claudeSessionID)
					// The first API call is imminent. Start the monotonic timer.
					state.apiCallStart = time.Now()
				case "api_retry":
					agentcore.EmitNotification(emit, formatAPIRetry(event))
				default:
					agentcore.EmitNotification(emit, event.summary())
				}

			case "assistant":
				if len(event.Message) > 0 {
					var meta rawAssistantMessageMeta
					if err := json.Unmarshal(event.Message, &meta); err == nil {
						if meta.Model != "" {
							state.lastModel = meta.Model
						}
						if meta.Usage != nil {
							snapshot, ready := state.acc.AddDelta(meta.Usage.InputTokens, meta.Usage.OutputTokens, meta.Usage.CacheReadInputTokens)
							// Claude Code 2.x: tool_use-only assistant messages may
							// carry output_tokens=0 (streaming message_start snapshot).
							// Defer the token_usage event until cumulative output is
							// non-zero so the orchestrator never receives an event
							// claiming zero output tokens for a real API turn.
							if ready {
								tokenEvt := domain.AgentEvent{
									Type:      domain.EventTokenUsage,
									Timestamp: now,
									Usage:     snapshot,
									Model:     state.lastModel,
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
								state.emittedUsage = true
							} else if !state.apiCallStart.IsZero() {
								// Consume the timing window without emitting so
								// that the next user event restarts fresh timing
								// for the subsequent API call.
								state.apiCallStart = time.Time{}
							}
						}
					}
				}
				// ToolTracker.Begin stores time.Now() internally, so no separate
				// monotonic timestamp is needed before calling processToolBlocks.
				processToolBlocks(event.contentBlocks(), state.inFlight, now, emit)
				agentcore.EmitNotification(emit, summarizeAssistant(event))

			case "user":
				// Claude Code emits tool results as user-role messages.
				processToolBlocks(event.contentBlocks(), state.inFlight, now, emit)
				state.apiCallStart = time.Now() // next API call is imminent

			case "result":
				captured := event
				// Only emit token_usage from the result event when no
				// per-assistant-message usage was already emitted. This
				// avoids inflating APIRequestCount in the orchestrator.
				if !state.emittedUsage {
					state.acc.ReplaceCumulative(normalizeUsage(event.Usage))
					emit(domain.AgentEvent{
						Type:      domain.EventTokenUsage,
						Timestamp: now,
						Usage:     state.acc.Snapshot(),
						Model:     state.lastModel,
					})
				}
				return &captured, nil

			case "stream_event":
				agentcore.EmitNotification(emit, "")

			default:
				emit(domain.AgentEvent{
					Type:      domain.EventOtherMessage,
					Timestamp: now,
					Message:   event.summary(),
				})
			}

			return nil, nil
		},
		GetUsage:     func() domain.TokenUsage { return state.acc.Snapshot() },
		GetSessionID: func() string { return state.claudeSessionID },
		OnFinalize: func(emit func(domain.AgentEvent), lastParsed any, exitCode int, stderrLines []string) (domain.TurnResult, *domain.AgentError) {
			lastResult, _ := lastParsed.(*rawEvent)
			usage := state.acc.Snapshot()

			if lastResult != nil {
				// Use turn-level duration_api_ms only when no per-request
				// API timing was emitted, to avoid double-counting.
				var turnAPIDuration int64
				if !state.emittedAPITiming {
					turnAPIDuration = lastResult.DurationAPI
				}
				if lastResult.Subtype == "success" && !lastResult.IsError {
					agentcore.EmitTurnCompleted(emit, typeutil.TruncateRunes(lastResult.Result, 500), turnAPIDuration)
					return domain.TurnResult{
						SessionID:  state.claudeSessionID,
						ExitReason: domain.EventTurnCompleted,
						Usage:      usage,
					}, nil
				}
				// EmitWarnLines is called by the skeleton when agentErr is non-nil.
				agentcore.EmitTurnFailed(emit, lastResult.Subtype, turnAPIDuration)
				return domain.TurnResult{
						SessionID:  state.claudeSessionID,
						ExitReason: domain.EventTurnFailed,
						Usage:      usage,
					}, &domain.AgentError{
						Kind:    domain.ErrTurnFailed,
						Message: lastResult.Subtype,
					}
			}

			if exitCode != 0 {
				agentcore.EmitTurnFailed(emit, "non-zero exit", 0)
				return domain.TurnResult{
						SessionID:  state.claudeSessionID,
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
						SessionID:  state.claudeSessionID,
						ExitReason: domain.EventTurnFailed,
						Usage:      usage,
					}, &domain.AgentError{
						Kind:    domain.ErrTurnFailed,
						Message: "agent exited without producing output",
					}
			}

			agentcore.EmitTurnCompleted(emit, "", 0)
			return domain.TurnResult{
				SessionID:  state.claudeSessionID,
				ExitReason: domain.EventTurnCompleted,
				Usage:      usage,
			}, nil
		},
		EmitSessionStartID: nil, // Claude emits EventSessionStarted from ParseLine on "system/init"
	}

	state.forkSession = agentcore.NewForkPerTurnSession(&state.target, hooks, sessionLogger)

	return domain.Session{
		ID:       sessionUUID,
		Internal: state,
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

	// Reset per-turn scan state before delegation.
	state.acc = agentcore.NewUsageAccumulator()
	state.lastModel = ""
	state.emittedUsage = false
	state.apiCallStart = time.Time{}
	state.emittedAPITiming = false
	state.inFlight = agentcore.NewToolTracker()

	return state.forkSession.RunTurn(ctx, params.Prompt, params.OnEvent)
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

// EventStream returns nil. The Claude Code adapter delivers events
// synchronously via the [domain.RunTurnParams] OnEvent callback.
func (a *ClaudeCodeAdapter) EventStream() <-chan domain.AgentEvent {
	return nil
}

// maxToolErrorLen is the maximum byte budget for [domain.AgentEvent.Message]
// when a tool_result block carries is_error: true. Output that exceeds this
// limit is formatted as first-line-plus-tail: the first line of the error
// output is preserved (typically an exit-code header), followed by the
// omission marker "\n...\n", followed by the last bytes of the remaining
// output — ensuring that CLI failure lines at the tail are always visible.
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
// first line of s (up to the first '\n') is kept as a header — for CLI tools
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
	nlPos := strings.IndexByte(s, '\n')
	if nlPos == -1 {
		return tailBytes(s, maxLen)
	}
	firstLine := s[:nlPos]
	const sep = "\n...\n"
	tailBudget := maxLen - len(firstLine) - len(sep)
	if tailBudget <= 0 {
		return tailBytes(s, maxLen)
	}
	tail := tailBytes(s[nlPos+1:], tailBudget)
	return firstLine + sep + tail
}

// processToolBlocks scans content blocks for tool_use and tool_result
// entries. tool_use blocks are registered in inFlight; tool_result
// blocks are correlated against inFlight and emitted as
// [domain.EventToolResult] via onEvent. The wallTime parameter is the
// wall-clock timestamp written into emitted events.
func processToolBlocks(
	blocks []rawContentBlock,
	inFlight *agentcore.ToolTracker,
	wallTime time.Time,
	onEvent func(domain.AgentEvent),
) {
	for _, block := range blocks {
		if block.Type == "tool_use" && block.ID != "" {
			inFlight.Begin(block.ID, block.Name)
		}
		if block.Type == "tool_result" {
			toolName, durationMS, ok := inFlight.End(block.ToolUseID)
			if !ok {
				toolName = "unknown"
			}
			msg := "tool_result: " + toolName
			if block.IsError {
				if errText := toolResultText(block); errText != "" {
					msg = truncateToolError(stripClaudeMarkup(errText), maxToolErrorLen)
				}
			}
			onEvent(domain.AgentEvent{
				Type:           domain.EventToolResult,
				Timestamp:      wallTime,
				ToolName:       toolName,
				ToolDurationMS: durationMS,
				ToolError:      block.IsError,
				Message:        msg,
			})
		}
	}
}
