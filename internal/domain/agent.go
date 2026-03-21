package domain

import (
	"context"
	"time"
)

// AgentEventType enumerates the normalized event types that agent
// adapters emit. The orchestrator uses these to update live session
// fields and make control flow decisions.
type AgentEventType string

const (
	// EventSessionStarted indicates the agent session initialized
	// successfully.
	EventSessionStarted AgentEventType = "session_started"

	// EventStartupFailed indicates the agent session could not be
	// initialized.
	EventStartupFailed AgentEventType = "startup_failed"

	// EventTurnCompleted indicates the turn finished successfully.
	EventTurnCompleted AgentEventType = "turn_completed"

	// EventTurnFailed indicates the turn finished with a failure.
	EventTurnFailed AgentEventType = "turn_failed"

	// EventTurnCancelled indicates the turn was cancelled.
	EventTurnCancelled AgentEventType = "turn_cancelled"

	// EventTurnEndedWithError indicates the turn ended due to an
	// error condition.
	EventTurnEndedWithError AgentEventType = "turn_ended_with_error"

	// EventTurnInputRequired indicates the agent requested user
	// input. This is a hard failure per policy.
	EventTurnInputRequired AgentEventType = "turn_input_required"

	// EventApprovalAutoApproved indicates an approval request was
	// auto-resolved by the adapter.
	EventApprovalAutoApproved AgentEventType = "approval_auto_approved"

	// EventUnsupportedToolCall indicates the agent requested an
	// unsupported tool.
	EventUnsupportedToolCall AgentEventType = "unsupported_tool_call"

	// EventTokenUsage carries normalized token usage counters:
	// input_tokens, output_tokens, total_tokens.
	EventTokenUsage AgentEventType = "token_usage"

	// EventNotification carries an informational message from the
	// agent.
	EventNotification AgentEventType = "notification"

	// EventOtherMessage carries an unclassified message from the
	// agent.
	EventOtherMessage AgentEventType = "other_message"

	// EventMalformed indicates an unparseable or unrecognized message
	// from the agent.
	EventMalformed AgentEventType = "malformed"
)

// TokenUsage holds normalized token counts emitted by agent adapters.
// The orchestrator computes deltas relative to previously reported
// values to avoid double-counting.
type TokenUsage struct {
	// InputTokens is the cumulative input token count.
	InputTokens int64

	// OutputTokens is the cumulative output token count.
	OutputTokens int64

	// TotalTokens is the cumulative total token count.
	TotalTokens int64
}

// AgentEvent is a normalized event emitted by an agent adapter over the
// lifetime of an agent session, including startup and individual turns.
// The orchestrator uses these to update the live session fields in the
// running map entry and accumulate token totals.
type AgentEvent struct {
	// Type is the normalized event category.
	Type AgentEventType

	// Timestamp is the UTC time the event was observed.
	Timestamp time.Time

	// AgentPID is the agent process ID, if available. Empty string
	// when not applicable.
	AgentPID string

	// Usage contains token counts for token_usage events. Zero value
	// for non-token events.
	Usage TokenUsage

	// Message is an adapter-normalized summary of the event payload.
	// Used for last_agent_message in the running map entry and for
	// observability. May be empty.
	Message string

	// SessionID is the adapter-assigned session identifier, populated
	// on EventSessionStarted events. Empty for all other event types.
	// The orchestrator copies this into RunningEntry.SessionID when
	// non-empty, enabling live session tracking before the worker exits.
	SessionID string

	// RateLimits is the latest rate-limit payload received from the agent
	// adapter. Structure is adapter-defined and intentionally opaque to the
	// orchestrator. Non-nil when rate-limit data is available; nil otherwise.
	// Per architecture Section 13.5 (Rate-limit tracking).
	RateLimits map[string]any
}

// AgentConfig is the subset of configuration relevant to agent
// adapters. Passed into [StartSessionParams] so adapters do not
// depend on the full config package.
type AgentConfig struct {
	// Kind identifies the agent adapter (e.g. "claude-code", "mock").
	Kind string

	// Command is the shell command used to launch the agent process.
	Command string

	// TurnTimeoutMS is the maximum duration in milliseconds for a
	// single agent turn.
	TurnTimeoutMS int

	// ReadTimeoutMS is the request/response timeout in milliseconds
	// during startup and synchronous requests.
	ReadTimeoutMS int

	// StallTimeoutMS is the maximum event inactivity duration in
	// milliseconds before the orchestrator considers the session
	// stalled. Non-positive values disable stall detection.
	StallTimeoutMS int
}

// Session is an opaque handle returned by [AgentAdapter.StartSession].
// The orchestrator does not interpret or mutate the session; it passes
// the handle back to [AgentAdapter.RunTurn] and [AgentAdapter.StopSession].
// The orchestrator may copy [Session.ID] and [Session.AgentPID] into its
// own state for observability, but must treat [Session.Internal] as
// adapter-owned and opaque.
type Session struct {
	// ID is the adapter-assigned session identifier. The orchestrator
	// may copy this into an opaque session_id field in its own state
	// for observability, but does not otherwise interpret it.
	ID string

	// AgentPID is the process ID of the agent subprocess, if
	// applicable. Empty string for HTTP-based adapters.
	AgentPID string

	// Internal holds adapter-specific state. The orchestrator must
	// not read or modify this field.
	Internal any
}

// StartSessionParams contains the inputs for
// [AgentAdapter.StartSession].
type StartSessionParams struct {
	// WorkspacePath is the absolute path to the per-issue workspace
	// directory. The adapter must launch the agent with this as cwd.
	WorkspacePath string

	// AgentConfig is the typed agent configuration from WORKFLOW.md.
	// Adapters read kind-specific fields (command, timeouts, etc.).
	AgentConfig AgentConfig

	// ResumeSessionID is the session ID from a previous worker
	// attempt for the same issue. When non-empty, the adapter
	// resumes the existing conversation instead of starting fresh.
	// Used for continuation retries after normal worker exit.
	// Adapters that do not support session continuity ignore this
	// field.
	ResumeSessionID string
}

// RunTurnParams contains the inputs for [AgentAdapter.RunTurn].
type RunTurnParams struct {
	// Prompt is the fully rendered prompt for this turn.
	Prompt string

	// Issue is the normalized issue being worked on. Adapters may
	// use it for context or tool scoping.
	Issue Issue

	// OnEvent is the callback for delivering events during the turn.
	// Called zero or more times before RunTurn returns. Must be
	// non-nil. Implementations must not retain or call OnEvent after
	// RunTurn returns.
	OnEvent func(AgentEvent)
}

// TurnResult is the outcome of a single agent turn returned by
// [AgentAdapter.RunTurn].
type TurnResult struct {
	// SessionID is the opaque session identifier assigned by the
	// adapter. May change between turns for adapters that rotate
	// identifiers.
	SessionID string

	// ExitReason summarizes why the turn ended. Maps to the
	// normalized event types (turn_completed, turn_failed, etc.).
	ExitReason AgentEventType

	// Usage contains the cumulative token usage as of this turn's
	// completion. The orchestrator computes deltas relative to
	// previous reports.
	Usage TokenUsage
}

// AgentAdapter defines the contract that all coding-agent integrations
// must satisfy. Each adapter normalizes its native protocol events
// into domain types. Implementations must be safe for concurrent use
// when the orchestrator runs multiple workers.
type AgentAdapter interface {
	// StartSession launches or connects to an agent process/service
	// in the given workspace. Returns an opaque session handle. The
	// caller must eventually call [AgentAdapter.StopSession] to
	// release resources.
	StartSession(ctx context.Context, params StartSessionParams) (Session, error)

	// RunTurn executes one agent turn with the given prompt. Events
	// are delivered to the caller via params.OnEvent during
	// execution. Returns when the turn completes (success, failure,
	// or timeout). Continuation turns reuse the same [Session].
	RunTurn(ctx context.Context, session Session, params RunTurnParams) (TurnResult, error)

	// StopSession terminates the agent process/service cleanly. Must
	// be called exactly once per session. Safe to call after a failed
	// [AgentAdapter.RunTurn].
	StopSession(ctx context.Context, session Session) error

	// EventStream returns a read-only channel for adapters that push
	// events asynchronously. Synchronous adapters (which deliver
	// events via the RunTurn callback) return nil.
	EventStream() <-chan AgentEvent
}
