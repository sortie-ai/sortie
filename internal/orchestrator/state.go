// Package orchestrator implements the coordination layer: polling,
// dispatch, concurrency control, retry scheduling, and reconciliation.
package orchestrator

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

// Label value constants for metric instrumentation. Unexported; used as
// arguments to domain.Metrics methods throughout the orchestrator.
const (
	outcomeSuccess = "success"
	outcomeError   = "error"

	exitTypeNormal    = "normal"
	exitTypeError     = "error"
	exitTypeCancelled = "cancelled"

	triggerError        = "error"
	triggerContinuation = "continuation"
	triggerTimer        = "timer"
	triggerStall        = "stall"

	actionStop    = "stop"
	actionCleanup = "cleanup"
	actionKeep    = "keep"

	handoffSuccess = "success"
	handoffError   = "error"
	handoffSkipped = "skipped"
)

// AgentTotals holds cumulative token and runtime counters across all ended
// agent sessions. These values are persisted to SQLite (aggregate_metrics
// table, key "agent_totals") and restored on startup.
//
// SecondsRunning tracks only ended-session time. At snapshot time, the
// caller adds elapsed time from active sessions in the Running map.
type AgentTotals struct {
	InputTokens     int64
	OutputTokens    int64
	TotalTokens     int64
	CacheReadTokens int64
	SecondsRunning  float64
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

	// CacheReadTokens is the cumulative cache-read token count for this session.
	CacheReadTokens int64

	// LastReportedCacheReadTokens is the last absolute cache-read value
	// reported by the agent. Used to compute deltas.
	LastReportedCacheReadTokens int64

	// ModelName is the latest LLM model identifier reported by the agent.
	// Empty when no model has been reported.
	ModelName string

	// APIRequestCount is the number of token_usage events received for
	// this session. Each token_usage event corresponds to one API request
	// round-trip from the agent.
	APIRequestCount int

	// RequestsByModel maps model name to the count of token_usage events
	// attributed to that model. Nil until the first token_usage event
	// carrying a model name arrives.
	RequestsByModel map[string]int

	// RetryAttempt is the retry attempt number. Nil for first dispatch,
	// non-nil and >= 1 for retries and continuations.
	RetryAttempt *int

	// StartedAt is the UTC time the worker was spawned.
	StartedAt time.Time

	// TurnCount is the number of coding-agent turns started within the
	// current worker lifetime.
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

	// WorkspacePath is the absolute path to the workspace directory used
	// by this worker. Populated from [WorkerResult.WorkspacePath] in
	// [HandleWorkerExit] before cleanup runs. Empty if the worker exited
	// before workspace preparation succeeded. Used by the PendingCleanup
	// code path to clean the actual directory instead of reconstructing
	// it from config (which may have changed via dynamic config reload).
	WorkspacePath string

	// WorkflowFile is the base filename of the WORKFLOW.md file that was
	// active when this session was dispatched (e.g. "WORKFLOW.md").
	// Recorded for observability and persisted in run_history.
	WorkflowFile string

	// SSHHost is the SSH host that this worker is executing on. Empty
	// for local execution. Used by [HandleWorkerExit] for host pool
	// release, [RuntimeSnapshot] for observability, and the retry
	// timer for host preference.
	SSHHost string

	// ToolTimeMs is the cumulative tool call execution time in
	// milliseconds, accumulated from tool_result events.
	ToolTimeMs int64

	// APITimeMs is the cumulative LLM API response wait time in
	// milliseconds, accumulated from any agent event carrying
	// APIDurationMS > 0.
	APITimeMs int64
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

	// LastSSHHost is the SSH host from the previous worker attempt.
	// Runtime-only (not persisted to SQLite). Used by
	// [HandleRetryTimer] to pass as preferred host to [HostPool.AcquireHost].
	LastSSHHost string

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
// through the orchestrator's event loop goroutine. The exception is
// WorkerWg, which is inherently goroutine-safe.
type State struct {
	// WorkerWg tracks in-flight worker goroutines spawned by
	// [DispatchIssue]. Callers that invoke dispatch outside the Run()
	// event loop (e.g. direct handleTick calls) can Wait() on this
	// group to ensure all goroutines have completed before cleanup.
	WorkerWg sync.WaitGroup

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

// SnapshotRunningEntry is a read-only view of a single running session
// for observability consumers. Produced by [RuntimeSnapshot].
type SnapshotRunningEntry struct {
	IssueID            string                `json:"issue_id"`
	Identifier         string                `json:"issue_identifier"`
	State              string                `json:"state"`
	SessionID          string                `json:"session_id"`
	TurnCount          int                   `json:"turn_count"`
	LastAgentEvent     domain.AgentEventType `json:"last_event"`
	LastAgentTimestamp time.Time             `json:"last_event_at"`
	LastAgentMessage   string                `json:"last_message"`
	StartedAt          time.Time             `json:"started_at"`
	AgentInputTokens   int64                 `json:"input_tokens"`
	AgentOutputTokens  int64                 `json:"output_tokens"`
	AgentTotalTokens   int64                 `json:"total_tokens"`
	CacheReadTokens    int64                 `json:"cache_read_tokens"`
	ModelName          string                `json:"model_name,omitempty"`
	APIRequestCount    int                   `json:"api_request_count"`
	RequestsByModel    map[string]int        `json:"requests_by_model,omitempty"`
	WorkspacePath      string                `json:"workspace_path"`
	SSHHost            string                `json:"ssh_host,omitempty"`
	ToolTimeMs         int64                 `json:"tool_time_ms"`
	APITimeMs          int64                 `json:"api_time_ms"`
	WorkflowFile       string                `json:"workflow_file,omitempty"`
}

// SnapshotRetryEntry is a read-only view of a pending retry for
// observability consumers. Produced by [RuntimeSnapshot].
type SnapshotRetryEntry struct {
	IssueID    string `json:"issue_id"`
	Identifier string `json:"issue_identifier"`
	Attempt    int    `json:"attempt"`
	DueAtMS    int64  `json:"due_at_ms"`
	Error      string `json:"error"`
}

// SnapshotAgentTotals holds aggregate token counts and runtime seconds
// at a point in time. Unlike [AgentTotals], SecondsRunning includes
// elapsed time from currently active sessions.
type SnapshotAgentTotals struct {
	InputTokens     int64   `json:"input_tokens"`
	OutputTokens    int64   `json:"output_tokens"`
	TotalTokens     int64   `json:"total_tokens"`
	CacheReadTokens int64   `json:"cache_read_tokens"`
	SecondsRunning  float64 `json:"seconds_running"`
}

// RuntimeSnapshotResult is a point-in-time capture of the orchestrator's
// runtime state for observability consumers. Produced by [RuntimeSnapshot].
type RuntimeSnapshotResult struct {
	GeneratedAt time.Time              `json:"generated_at"`
	Running     []SnapshotRunningEntry `json:"running"`
	Retrying    []SnapshotRetryEntry   `json:"retrying"`
	AgentTotals SnapshotAgentTotals    `json:"agent_totals"`
	RateLimits  map[string]any         `json:"rate_limits"`
}

// ActiveElapsedSeconds returns the sum of wall-clock elapsed seconds
// across all running sessions at the given point in time. Entries with
// a zero StartedAt are skipped; negative elapsed values are clamped to
// zero to guard against clock skew.
func ActiveElapsedSeconds(state *State, now time.Time) float64 {
	var total float64
	for _, entry := range state.Running {
		if entry.StartedAt.IsZero() {
			continue
		}
		elapsed := now.Sub(entry.StartedAt).Seconds()
		if elapsed < 0 {
			elapsed = 0
		}
		total += elapsed
	}
	return total
}

// RuntimeSnapshot captures a point-in-time view of the orchestrator's
// runtime state. The now parameter controls the snapshot timestamp and
// the active-session elapsed time computation; it is normalized to UTC
// internally. Callers on the event loop goroutine pass time.Now();
// test callers pass a fixed time for deterministic assertions.
//
// The returned result contains copied-out data for Running, Retrying,
// and AgentTotals — callers may serialize or retain those fields without
// synchronization concerns.
//
// RateLimits is shallow-copied from State.AgentRateLimits.Data and may
// alias nested mutable values. Callers must treat RateLimits and any
// values it references as read-only and only perform concurrent reads.
//
// Must be called from the orchestrator's event loop goroutine. [State]
// is not safe for concurrent access.
func RuntimeSnapshot(state *State, now time.Time) RuntimeSnapshotResult {
	now = now.UTC()
	result := RuntimeSnapshotResult{
		GeneratedAt: now,
		Running:     make([]SnapshotRunningEntry, 0, len(state.Running)),
		Retrying:    make([]SnapshotRetryEntry, 0, len(state.RetryAttempts)),
	}

	var activeElapsedTotal float64
	for _, entry := range state.Running {
		var rbm map[string]int
		if entry.RequestsByModel != nil {
			rbm = make(map[string]int, len(entry.RequestsByModel))
			for k, v := range entry.RequestsByModel {
				rbm[k] = v
			}
		}
		result.Running = append(result.Running, SnapshotRunningEntry{
			IssueID:            entry.Issue.ID,
			Identifier:         entry.Identifier,
			State:              entry.Issue.State,
			SessionID:          entry.SessionID,
			TurnCount:          entry.TurnCount,
			LastAgentEvent:     entry.LastAgentEvent,
			LastAgentTimestamp: entry.LastAgentTimestamp,
			LastAgentMessage:   entry.LastAgentMessage,
			StartedAt:          entry.StartedAt,
			AgentInputTokens:   entry.AgentInputTokens,
			AgentOutputTokens:  entry.AgentOutputTokens,
			AgentTotalTokens:   entry.AgentTotalTokens,
			CacheReadTokens:    entry.CacheReadTokens,
			ModelName:          entry.ModelName,
			APIRequestCount:    entry.APIRequestCount,
			RequestsByModel:    rbm,
			WorkspacePath:      entry.WorkspacePath,
			SSHHost:            entry.SSHHost,
			ToolTimeMs:         entry.ToolTimeMs,
			APITimeMs:          entry.APITimeMs,
			WorkflowFile:       entry.WorkflowFile,
		})

		if !entry.StartedAt.IsZero() {
			elapsed := now.Sub(entry.StartedAt).Seconds()
			if elapsed < 0 {
				elapsed = 0
			}
			activeElapsedTotal += elapsed
		}
	}

	for _, entry := range state.RetryAttempts {
		result.Retrying = append(result.Retrying, SnapshotRetryEntry{
			IssueID:    entry.IssueID,
			Identifier: entry.Identifier,
			Attempt:    entry.Attempt,
			DueAtMS:    entry.DueAtMS,
			Error:      entry.Error,
		})
	}

	result.AgentTotals = SnapshotAgentTotals{
		InputTokens:     state.AgentTotals.InputTokens,
		OutputTokens:    state.AgentTotals.OutputTokens,
		TotalTokens:     state.AgentTotals.TotalTokens,
		CacheReadTokens: state.AgentTotals.CacheReadTokens,
		SecondsRunning:  state.AgentTotals.SecondsRunning + activeElapsedTotal,
	}

	if state.AgentRateLimits != nil {
		result.RateLimits = shallowCopyMap(state.AgentRateLimits.Data)
	}

	return result
}
