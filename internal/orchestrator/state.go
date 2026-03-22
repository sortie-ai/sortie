// Package orchestrator implements the coordination layer: polling,
// dispatch, concurrency control, retry scheduling, and reconciliation.
package orchestrator

import (
	"context"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

// AgentTotals holds cumulative token and runtime counters across all ended
// agent sessions. These values are persisted to SQLite (aggregate_metrics
// table, key "agent_totals") and restored on startup.
//
// SecondsRunning tracks only ended-session time. At snapshot time, the
// caller adds elapsed time from active sessions in the Running map.
type AgentTotals struct {
	InputTokens    int64
	OutputTokens   int64
	TotalTokens    int64
	SecondsRunning float64
}

// RateLimitSnapshot holds the latest rate-limit information received from
// an agent event. The structure is intentionally opaque since rate-limit
// payload format is agent-adapter-defined.
type RateLimitSnapshot struct {
	// Data holds the raw rate-limit payload from the agent event.
	Data map[string]any

	// ReceivedAt is the UTC time the rate-limit event was observed.
	ReceivedAt time.Time
}

// RunningEntry tracks a single in-flight agent session. Created by dispatch
// and removed on worker exit or reconciliation termination.
type RunningEntry struct {
	// Identifier is the human-readable ticket key (e.g. "MT-649").
	Identifier string

	// Issue is the last-known normalized issue snapshot. Updated by
	// reconciliation when the tracker reports new state.
	Issue domain.Issue

	// SessionID is the adapter-assigned session identifier. Initially
	// empty; populated when the worker reports session_started.
	SessionID string

	// ThreadID is the adapter-assigned thread identifier. Populated by
	// adapters that expose thread/turn granularity; empty otherwise.
	ThreadID string

	// TurnID is the adapter-assigned turn identifier. Populated by
	// adapters that expose thread/turn granularity; empty otherwise.
	TurnID string

	// AgentPID is the agent subprocess PID. Initially empty; populated
	// from agent events.
	AgentPID string

	// LastAgentEvent is the most recent agent event type. Zero value
	// until the first event arrives.
	LastAgentEvent domain.AgentEventType

	// LastAgentTimestamp is the UTC time of the most recent agent event.
	// Zero value until the first event arrives.
	LastAgentTimestamp time.Time

	// LastAgentMessage is a summary of the most recent agent event
	// payload. Empty string until populated by an event.
	LastAgentMessage string

	// AgentInputTokens is the cumulative input token count for this session.
	AgentInputTokens int64

	// AgentOutputTokens is the cumulative output token count for this session.
	AgentOutputTokens int64

	// AgentTotalTokens is the cumulative total token count for this session.
	AgentTotalTokens int64

	// LastReportedInputTokens is the last absolute input token value
	// reported by the agent. Used to compute deltas and avoid double-counting.
	LastReportedInputTokens int64

	// LastReportedOutputTokens is the last absolute output token value
	// reported by the agent.
	LastReportedOutputTokens int64

	// LastReportedTotalTokens is the last absolute total token value
	// reported by the agent.
	LastReportedTotalTokens int64

	// RetryAttempt is the retry attempt number. Nil for first dispatch,
	// non-nil and >= 1 for retries and continuations.
	RetryAttempt *int

	// StartedAt is the UTC time the worker was spawned.
	StartedAt time.Time

	// TurnCount is the number of coding-agent turns completed or finalized
	// within the current worker lifetime.
	TurnCount int

	// CancelFunc cancels the per-worker context created by [DispatchIssue].
	// Called by reconciliation to stop stalled or terminal-state workers,
	// and by graceful shutdown to drain active sessions. Nil only in test
	// fixtures that bypass [DispatchIssue].
	CancelFunc context.CancelFunc

	// PendingCleanup is set by reconciliation when the tracker reports a
	// terminal state for this issue. [HandleWorkerExit] checks this flag
	// after the worker goroutine exits and performs the actual workspace
	// cleanup. This defers cleanup until the agent process has fully
	// terminated, avoiding races with active file writes or hooks.
	PendingCleanup bool
}

// RetryEntry holds the runtime state for a pending retry. The persisted
// fields (IssueID, Identifier, Attempt, DueAtMS, Error) map to
// persistence.RetryEntry. TimerHandle, scheduledAt, and scheduledDelayMS
// are runtime-only and are reconstructed on startup from persisted
// due_at timestamps.
type RetryEntry struct {
	IssueID     string
	Identifier  string
	Attempt     int
	DueAtMS     int64
	Error       string
	TimerHandle *time.Timer

	// scheduledAt records time.Now() at the moment ScheduleRetry creates
	// this entry. Because time.Time preserves the monotonic clock reading,
	// staleness detection via time.Since is immune to wall-clock jumps.
	// Zero when the entry was reconstructed from SQLite at startup.
	scheduledAt time.Time

	// scheduledDelayMS is the delay (in milliseconds) passed to
	// ScheduleRetry. Together with scheduledAt it enables a monotonic
	// staleness check: if time.Since(scheduledAt) < scheduledDelayMS the
	// timer fired before its intended moment, indicating a stale callback
	// from a replaced timer.
	scheduledDelayMS int64
}

// State is the single authoritative runtime state owned by the orchestrator.
// The running map and claimed set are in-memory for performance. The
// agent_totals and completed set are backed by SQLite and survive restarts.
//
// State is not safe for concurrent access. All mutations are serialized
// through the orchestrator's event loop goroutine.
type State struct {
	// PollIntervalMS is the current effective poll interval from config.
	PollIntervalMS int

	// MaxConcurrentAgents is the current effective global concurrency limit
	// from config.
	MaxConcurrentAgents int

	// MaxConcurrentByState holds per-state concurrency caps. State keys are
	// normalized to lowercase. An absent key means the state falls back to
	// the global limit.
	MaxConcurrentByState map[string]int

	// Running maps issue ID to the live session entry for that issue.
	// Only the orchestrator's event loop may mutate this map.
	Running map[string]*RunningEntry

	// Claimed is the set of issue IDs that are reserved by the orchestrator
	// (running, retry-queued, or in the process of being dispatched).
	// Prevents duplicate dispatch.
	Claimed map[string]struct{}

	// RetryAttempts maps issue ID to the pending retry entry.
	RetryAttempts map[string]*RetryEntry

	// Completed is a set of issue IDs that have completed at least once.
	// Bookkeeping only — not used for dispatch gating.
	Completed map[string]struct{}

	// AgentTotals holds aggregate token counts and cumulative runtime seconds
	// across all ended sessions. Active session elapsed time is computed at
	// snapshot time, not maintained continuously.
	AgentTotals AgentTotals

	// AgentRateLimits is the most recent rate-limit payload received from
	// any agent event. Nil when no rate-limit data has been observed.
	AgentRateLimits *RateLimitSnapshot
}

// NewState creates an initialized [State] with empty collections and the
// provided config values. If persisted [AgentTotals] are available from
// SQLite recovery, pass them in; otherwise pass a zero-value AgentTotals.
// Keys in maxConcurrentByState must be pre-normalized to lowercase by the
// caller; the config layer does this during parsing.
func NewState(pollIntervalMS, maxConcurrentAgents int, maxConcurrentByState map[string]int, totals AgentTotals) *State {
	if maxConcurrentByState == nil {
		maxConcurrentByState = make(map[string]int)
	}
	return &State{
		PollIntervalMS:       pollIntervalMS,
		MaxConcurrentAgents:  maxConcurrentAgents,
		MaxConcurrentByState: maxConcurrentByState,
		Running:              make(map[string]*RunningEntry),
		Claimed:              make(map[string]struct{}),
		RetryAttempts:        make(map[string]*RetryEntry),
		Completed:            make(map[string]struct{}),
		AgentTotals:          totals,
	}
}

// RunningCount returns the number of entries in the running map.
func (s *State) RunningCount() int {
	return len(s.Running)
}

// RunningCountByState returns the number of entries in the running map whose
// Issue.State matches the given state (case-insensitive).
func RunningCountByState(running map[string]*RunningEntry, state string) int {
	count := 0
	for _, entry := range running {
		if strings.EqualFold(entry.Issue.State, state) {
			count++
		}
	}
	return count
}
