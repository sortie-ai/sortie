package domain

import (
	"context"
	"time"
)

// AgentEventType enumerates the normalized event types that agent
// adapters emit.
type AgentEventType string

const (
	EventSessionStarted AgentEventType = "session_started"
	EventStartupFailed  AgentEventType = "startup_failed"
	EventTurnCompleted  AgentEventType = "turn_completed"
	EventTurnFailed     AgentEventType = "turn_failed"
	EventTurnCancelled  AgentEventType = "turn_cancelled"

	EventTurnEndedWithError AgentEventType = "turn_ended_with_error"

	// EventTurnInputRequired is a declared, non-retryable ending: the
	// agent asked for a decision only a person could give.
	EventTurnInputRequired AgentEventType = "turn_input_required"

	EventTokenUsage   AgentEventType = "token_usage"
	EventNotification AgentEventType = "notification"
	EventOtherMessage AgentEventType = "other_message"
	EventMalformed    AgentEventType = "malformed"

	// EventToolResult indicates a tool call completed, with ToolName and
	// ToolDurationMS populated when the adapter can observe them.
	EventToolResult AgentEventType = "tool_result"
)

// TokenUsage holds normalized token counts emitted by agent adapters.
// Every field is cumulative over the agent session an adapter opened
// with StartSession and excludes usage a resumed session accumulated
// before StartSession, so re-reporting a figure is safe: the orchestrator
// folds deltas against what it already recorded. Adapters compute
// TotalTokens as InputTokens plus OutputTokens rather than passing a
// vendor total through.
type TokenUsage struct {
	// InputTokens is cumulative, including prompt-cache reads and writes.
	InputTokens int64

	// OutputTokens is cumulative, including reasoning tokens.
	OutputTokens int64

	// TotalTokens equals InputTokens plus OutputTokens.
	TotalTokens int64

	// CacheReadTokens is the cache-read subset of InputTokens, never
	// added to any other counter. Zero when the adapter reports no cache
	// data.
	CacheReadTokens int64
}

// AgentEvent is a normalized event emitted by an agent adapter over the
// lifetime of an agent session, including startup and individual turns.
type AgentEvent struct {
	Type AgentEventType

	// Timestamp is the UTC time the event was observed.
	Timestamp time.Time

	// AgentPID is the agent process ID, or empty when not applicable.
	AgentPID string

	// Usage carries the session's run-cumulative token counts as of this
	// event. The zero value means the event carries no usage information,
	// not that the session has consumed no tokens.
	Usage TokenUsage

	// Message is an adapter-normalized summary of the event payload. May
	// be empty.
	Message string

	// SessionID is the adapter-assigned session identifier, populated on
	// EventSessionStarted and empty otherwise. The orchestrator copies a
	// non-empty value into RunningEntry.SessionID.
	SessionID string

	// Model is the LLM model identifier reported on token_usage events,
	// or empty when unknown.
	Model string

	// RateLimits is the latest adapter-defined rate-limit payload, opaque
	// to the orchestrator. Nil when unavailable.
	RateLimits map[string]any

	// APIDurationMS is the LLM API wait time in milliseconds for this
	// event; a value > 0 contributes to the session's cumulative API
	// time. Zero when unavailable.
	APIDurationMS int64

	// ToolName is the tool that completed, for tool_result events.
	ToolName string

	// ToolDurationMS is the tool call's wall-clock time in milliseconds,
	// for tool_result events. Zero when unavailable.
	ToolDurationMS int64

	// ToolError reports whether a tool_result call returned an error.
	ToolError bool
}

// AgentConfig is the subset of configuration relevant to agent
// adapters, passed into [StartSessionParams] so adapters do not depend
// on the full config package.
type AgentConfig struct {
	// Kind identifies the agent adapter (e.g. "claude-code", "mock").
	Kind string

	// Command launches the agent process. Locally it is split on
	// whitespace into an argv the adapter execs directly with no shell;
	// in SSH mode it is passed through unsplit to the remote shell.
	Command string

	// TurnTimeoutMS bounds a single agent turn. Always positive: the
	// config layer rejects a non-positive value rather than treating it
	// as a disable sentinel.
	TurnTimeoutMS int

	// ReadTimeoutMS is the request/response timeout during startup and
	// synchronous requests.
	ReadTimeoutMS int

	// StallTimeoutMS bounds event inactivity before the session is
	// considered stalled. Non-positive disables stall detection.
	StallTimeoutMS int

	// StopGraceMS is how long an adapter waits after a catchable
	// termination signal before force-terminating the process group. A
	// value from workflow config is always positive; a value assembled
	// in code may be zero, so adapters resolve it through the shared
	// helper that maps non-positive to the built-in grace rather than
	// reading it as no limit.
	StopGraceMS int
}

// Session is an opaque handle returned by [AgentAdapter.StartSession].
// The orchestrator passes the handle back to RunTurn and StopSession and
// may copy ID and AgentPID for observability, but must treat Internal as
// adapter-owned and opaque.
type Session struct {
	// ID is the adapter-assigned session identifier.
	ID string

	// AgentPID is the agent subprocess PID, or empty for HTTP-based
	// adapters.
	AgentPID string

	// Internal holds adapter-specific state the orchestrator must not
	// read or modify.
	Internal any
}

// StartSessionParams contains the inputs for
// [AgentAdapter.StartSession].
type StartSessionParams struct {
	// WorkspacePath is the absolute per-issue workspace directory the
	// adapter must launch the agent with as cwd.
	WorkspacePath string

	// AgentConfig is the typed agent configuration from WORKFLOW.md.
	AgentConfig AgentConfig

	// ResumeSessionID, when non-empty, resumes the existing conversation
	// from a previous worker attempt instead of starting fresh. Adapters
	// without session continuity ignore it.
	ResumeSessionID string

	// SSHHost, when non-empty, launches the agent on that host via SSH
	// instead of as a local subprocess.
	SSHHost string

	// SSHStrictHostKeyChecking is the OpenSSH StrictHostKeyChecking value
	// for remote sessions; adapters default to "accept-new" when empty.
	// Only meaningful when SSHHost is non-empty.
	SSHStrictHostKeyChecking string

	// SSHEnvNames lists environment variable names to carry from the
	// orchestrator's environment into a remote session. It carries names
	// only; adapters resolve each value at launch. Only meaningful when
	// SSHHost is non-empty.
	SSHEnvNames []string

	// MCPConfigPath, when non-empty, is the absolute merged MCP config
	// path adapters inject via their MCP config CLI flag. When empty,
	// adapters use the operator-configured mcp_config passthrough.
	MCPConfigPath string

	// CredentialVerification marks a session that carries only the
	// request proving the runtime's credential works.
	CredentialVerification bool
}

// RunTurnParams contains the inputs for [AgentAdapter.RunTurn].
type RunTurnParams struct {
	// Prompt is the fully rendered prompt for this turn.
	Prompt string

	// Issue is the normalized issue being worked on.
	Issue Issue

	// OnEvent delivers events during the turn. Must be non-nil.
	// Implementations must not retain or call it after RunTurn returns.
	OnEvent func(AgentEvent)
}

// TurnResult is the outcome of a single agent turn returned by
// [AgentAdapter.RunTurn].
type TurnResult struct {
	// SessionID is the opaque session identifier, which may change
	// between turns for adapters that rotate identifiers.
	SessionID string

	// ExitReason summarizes why the turn ended, mapping to the
	// normalized event types.
	ExitReason AgentEventType

	// Usage carries the session's run-cumulative token counts as of this
	// turn's completion, not just this turn's contribution.
	Usage TokenUsage

	// UsageMeasured reports whether the adapter observed at least one
	// usage figure for the session. A false value with zero Usage means
	// spend is unknown, not zero. Once true for a session it must not
	// become false on a later turn.
	UsageMeasured bool

	// SpendUnaccounted reports that this turn issued a model request no
	// figure was proven to cover in full, making the run's summed total a
	// lower bound. Unlike UsageMeasured it is per turn and not monotone,
	// and any partial figure is still reported in Usage.
	SpendUnaccounted bool
}

// AgentAdapter is the contract every coding-agent integration must
// satisfy, normalizing native protocol events into domain types.
// Implementations must be safe for concurrent use across workers.
type AgentAdapter interface {
	// StartSession launches or connects to an agent in the given
	// workspace and returns an opaque handle. The caller must eventually
	// call StopSession.
	StartSession(ctx context.Context, params StartSessionParams) (Session, error)

	// RunTurn executes one turn, delivering events via params.OnEvent,
	// and returns when the turn completes. Continuation turns reuse the
	// same [Session].
	RunTurn(ctx context.Context, session Session, params RunTurnParams) (TurnResult, error)

	// StopSession terminates the agent cleanly. Must be called exactly
	// once per session, and is safe to call after a failed RunTurn.
	StopSession(ctx context.Context, session Session) error
}
