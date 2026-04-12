package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
)

// Label value constants for metric instrumentation. Unexported; used as
// arguments to domain.Metrics methods throughout the orchestrator.
const (
	outcomeSuccess = "success"
	outcomeError   = "error"
	outcomeSkipped = "skipped"

	exitTypeNormal    = "normal"
	exitTypeError     = "error"
	exitTypeCancelled = "cancelled"
	exitTypeSoftStop  = "soft_stop"

	triggerError        = "error"
	triggerContinuation = "continuation"
	triggerTimer        = "timer"
	triggerStall        = "stall"
	triggerCIFix        = "ci_fix"

	actionStop         = "stop"
	actionCleanup      = "cleanup"
	actionKeep         = "keep"
	actionSweepCleanup = "sweep_cleanup"

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

	// ContinuationContext carries reaction continuation data from a retry
	// into the worker session. Populated by HandleRetryTimer when the
	// retry carries reaction context. Nil for normal dispatches.
	ContinuationContext map[string]any

	// SelfReviewActive is true when the worker is in the self-review phase.
	// Mutated only by the event loop via selfReviewCh.
	SelfReviewActive bool

	// SelfReviewIteration is the current review iteration (0 when not in review).
	// Mutated only by the event loop via selfReviewCh.
	SelfReviewIteration int
}

// RetryEntry holds the runtime state for a pending retry. The persisted
// fields (IssueID, Identifier, Attempt, DueAtMS, Error) map to
// persistence.RetryEntry. TimerHandle, scheduledAt, and scheduledDelayMS
// are runtime-only and are reconstructed on startup from persisted
// due_at timestamps.
type RetryEntry struct {
	IssueID     string
	Identifier  string
	DisplayID   string
	SessionID   string
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

	// ContinuationContext carries reaction continuation data for template
	// injection on the first turn of the retry worker. Populated by
	// reconcile functions when scheduling a continuation retry. Nil for
	// non-reaction retries.
	ContinuationContext map[string]any

	// ReactionKind is the reaction type that triggered this retry (e.g.
	// ReactionKindCI). Empty for non-reaction retries. When non-empty,
	// HandleRetryTimer calls MarkReactionDispatched after successful
	// dispatch. Runtime-only (not persisted to SQLite).
	ReactionKind string
}

// ReactionKindCI is the reaction kind constant for CI failure reactions.
const ReactionKindCI = "ci"

// ReactionKindReview is the reaction kind constant for PR review comment
// reactions.
const ReactionKindReview = "review"

// ReactionKey returns the composite map key for a pending reaction.
// Callers must not pass IDs containing colons; the delimiter is a plain
// colon between issueID and kind.
func ReactionKey(issueID, kind string) string {
	return issueID + ":" + kind
}

// PendingReaction records that an issue needs external signal
// reconciliation. Created by worker exit handlers or external event
// receivers. Consumed by per-kind reconcile functions during the
// reconcile tick. Runtime-only (not persisted to SQLite — cross-restart
// deduplication uses reaction_fingerprints).
type PendingReaction struct {
	// IssueID is the domain issue ID.
	IssueID string

	// Identifier is the human-readable ticket key (e.g. "MT-649").
	Identifier string

	// DisplayID is the display identifier passed through to dispatch.
	DisplayID string

	// Attempt is the overall run attempt number from the completed worker.
	Attempt int

	// Kind is the reaction type constant (e.g. ReactionKindCI).
	Kind string

	// LastSSHHost is the SSH host from the completed worker, used for
	// host preference on fix redispatch.
	LastSSHHost string

	// CreatedAt is the UTC time the entry was created.
	CreatedAt time.Time

	// PendingAttempts is the number of times the entry has been
	// re-enqueued without a definitive result (pending status or
	// transient fetch error). Used to compute exponential backoff.
	PendingAttempts int

	// PendingRetryAt is the earliest UTC time at which the reconcile
	// function should poll again. Zero means ready immediately.
	PendingRetryAt time.Time

	// KindData holds kind-specific typed data. CI reactions use
	// [*CIReactionData]; future reaction types define their own structs.
	// The reconcile function for each kind is responsible for a single
	// type assertion at the top of its loop body.
	KindData any
}

// CIReactionData holds CI-specific fields for a pending CI reaction.
// Stored in [PendingReaction.KindData] for reactions with Kind ==
// [ReactionKindCI].
type CIReactionData struct {
	// Branch is the git branch name from SCM metadata.
	Branch string

	// SHA is the git commit SHA from SCM metadata. When non-empty, used
	// as the ref for CIStatusProvider.FetchCIStatus.
	SHA string
}

// ReviewReactionData holds review-specific fields for a pending review
// reaction. Stored in [PendingReaction.KindData] for reactions with
// Kind == [ReactionKindReview]. Owner and Repo are sourced from
// [domain.SCMMetadata] (written by the agent to scm.json), never from
// the tracker project configuration.
type ReviewReactionData struct {
	// PRNumber is the pull request number.
	PRNumber int

	// Owner is the repository owner.
	Owner string

	// Repo is the repository name.
	Repo string

	// Branch is the git branch name.
	Branch string

	// SHA is the git commit SHA at the last known push.
	SHA string

	// LastEventAt is the UTC timestamp of the most recently detected
	// review comment. Used for debounce gating.
	LastEventAt time.Time
}

// ReviewReactionConfig holds validated review-specific configuration
// extracted from [config.ReactionConfig] at startup.
type ReviewReactionConfig struct {
	Escalation           string
	EscalationLabel      string
	PollIntervalMS       int
	DebounceMS           int
	MaxContinuationTurns int
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

	// TrackerOpsWg tracks fire-and-forget goroutines that perform
	// best-effort tracker API calls (comments, labels). Drained after
	// worker shutdown completes so these goroutines are not orphaned
	// on process exit.
	TrackerOpsWg sync.WaitGroup

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

	// BudgetExhausted is the set of issue IDs whose run_history count
	// has reached or exceeded the configured max_sessions budget.
	// Rebuilt from a batch SQLite query at the start of each poll tick
	// when max_sessions > 0. Updated inline by [HandleRetryTimer] on
	// budget exhaustion. [ShouldDispatch] checks this set as a dispatch
	// gate. Cleared when max_sessions is 0.
	BudgetExhausted map[string]struct{}

	// AgentTotals holds aggregate token counts and cumulative runtime seconds
	// across all ended sessions. Active session elapsed time is computed at
	// snapshot time, not maintained continuously.
	AgentTotals AgentTotals

	// AgentRateLimits is the most recent rate-limit payload received from
	// any agent event. Nil when no rate-limit data has been observed.
	AgentRateLimits *RateLimitSnapshot

	// ReactionAttempts maps composite key (issueID:kind) to the number of
	// reaction-triggered continuations dispatched for that combination.
	// Reset when the issue leaves the Running or RetryAttempts maps.
	// Runtime-only (not persisted).
	ReactionAttempts map[string]int

	// PendingReactions maps composite key (issueID:kind) to a
	// [PendingReaction]. Populated by [HandleWorkerExit] when a normal
	// exit occurs and a reaction provider is configured. Consumed by
	// per-kind reconcile functions during the reconcile tick. Runtime-only
	// (not persisted).
	PendingReactions map[string]*PendingReaction

	// SweepTickCounter tracks poll ticks since the last terminal workspace
	// sweep. Incremented by handleTick; reset to zero when the sweep fires.
	// Runtime-only (not persisted).
	SweepTickCounter int
}

// continuationCtxKey is the context key for reaction continuation data
// passed from the dispatch site to the worker goroutine via
// context.WithValue.
type continuationCtxKey struct{}

// WithContinuationContext returns a child context carrying reaction
// continuation data for prompt injection. The worker reads this with
// [ContinuationFromContext].
func WithContinuationContext(ctx context.Context, data map[string]any) context.Context {
	return context.WithValue(ctx, continuationCtxKey{}, data)
}

// ContinuationFromContext extracts the continuation map injected by
// [WithContinuationContext]. Returns nil when no continuation data is
// present.
func ContinuationFromContext(ctx context.Context) map[string]any {
	v, _ := ctx.Value(continuationCtxKey{}).(map[string]any)
	return v
}

// ClearReactionsForIssue removes all PendingReactions and
// ReactionAttempts entries whose key starts with the given issue ID
// prefix, and deletes the corresponding reaction_fingerprints rows from
// SQLite. The SQLite call is best-effort: a failure is logged at warn
// level but does not block the caller.
func ClearReactionsForIssue(ctx context.Context, state *State, store ReconcileStore, issueID string, log *slog.Logger) {
	prefix := issueID + ":"
	for key := range state.PendingReactions {
		if strings.HasPrefix(key, prefix) {
			delete(state.PendingReactions, key)
		}
	}
	for key := range state.ReactionAttempts {
		if strings.HasPrefix(key, prefix) {
			delete(state.ReactionAttempts, key)
		}
	}
	if err := store.DeleteReactionFingerprintsByIssue(ctx, issueID); err != nil {
		if log == nil {
			log = slog.Default()
		}
		log.WarnContext(ctx, "failed to delete reaction fingerprints",
			slog.String("issue_id", issueID),
			slog.Any("error", err),
		)
	}
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
		BudgetExhausted:      make(map[string]struct{}),
		AgentTotals:          totals,
		ReactionAttempts:     make(map[string]int),
		PendingReactions:     make(map[string]*PendingReaction),
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
	IssueID             string                `json:"issue_id"`
	Identifier          string                `json:"issue_identifier"`
	DisplayID           string                `json:"display_identifier,omitempty"`
	State               string                `json:"state"`
	SessionID           string                `json:"session_id"`
	TurnCount           int                   `json:"turn_count"`
	LastAgentEvent      domain.AgentEventType `json:"last_event"`
	LastAgentTimestamp  time.Time             `json:"last_event_at"`
	LastAgentMessage    string                `json:"last_message"`
	StartedAt           time.Time             `json:"started_at"`
	AgentInputTokens    int64                 `json:"input_tokens"`
	AgentOutputTokens   int64                 `json:"output_tokens"`
	AgentTotalTokens    int64                 `json:"total_tokens"`
	CacheReadTokens     int64                 `json:"cache_read_tokens"`
	ModelName           string                `json:"model_name,omitempty"`
	APIRequestCount     int                   `json:"api_request_count"`
	RequestsByModel     map[string]int        `json:"requests_by_model,omitempty"`
	WorkspacePath       string                `json:"workspace_path"`
	SSHHost             string                `json:"ssh_host,omitempty"`
	ToolTimeMs          int64                 `json:"tool_time_ms"`
	APITimeMs           int64                 `json:"api_time_ms"`
	WorkflowFile        string                `json:"workflow_file,omitempty"`
	SelfReviewActive    bool                  `json:"self_review_active,omitempty"`
	SelfReviewIteration int                   `json:"self_review_iteration,omitempty"`
}

// SnapshotRetryEntry is a read-only view of a pending retry for
// observability consumers. Produced by [RuntimeSnapshot].
type SnapshotRetryEntry struct {
	IssueID    string `json:"issue_id"`
	Identifier string `json:"issue_identifier"`
	DisplayID  string `json:"display_identifier,omitempty"`
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
	GeneratedAt          time.Time              `json:"generated_at"`
	Running              []SnapshotRunningEntry `json:"running"`
	Retrying             []SnapshotRetryEntry   `json:"retrying"`
	AgentTotals          SnapshotAgentTotals    `json:"agent_totals"`
	RateLimits           map[string]any         `json:"rate_limits"`
	BudgetExhaustedCount int                    `json:"budget_exhausted_count"`
	BudgetExhausted      []string               `json:"budget_exhausted,omitempty"`
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
	snap := RuntimeSnapshotResult{
		GeneratedAt: now,
		Running:     make([]SnapshotRunningEntry, 0, len(state.Running)),
		Retrying:    make([]SnapshotRetryEntry, 0, len(state.RetryAttempts)),
	}

	var activeElapsedTotal float64
	for _, entry := range state.Running {
		var modelRequests map[string]int
		if entry.RequestsByModel != nil {
			modelRequests = make(map[string]int, len(entry.RequestsByModel))
			for k, v := range entry.RequestsByModel {
				modelRequests[k] = v
			}
		}
		snap.Running = append(snap.Running, SnapshotRunningEntry{
			IssueID:             entry.Issue.ID,
			Identifier:          entry.Identifier,
			DisplayID:           entry.Issue.DisplayID,
			State:               entry.Issue.State,
			SessionID:           entry.SessionID,
			TurnCount:           entry.TurnCount,
			LastAgentEvent:      entry.LastAgentEvent,
			LastAgentTimestamp:  entry.LastAgentTimestamp,
			LastAgentMessage:    entry.LastAgentMessage,
			StartedAt:           entry.StartedAt,
			AgentInputTokens:    entry.AgentInputTokens,
			AgentOutputTokens:   entry.AgentOutputTokens,
			AgentTotalTokens:    entry.AgentTotalTokens,
			CacheReadTokens:     entry.CacheReadTokens,
			ModelName:           entry.ModelName,
			APIRequestCount:     entry.APIRequestCount,
			RequestsByModel:     modelRequests,
			WorkspacePath:       entry.WorkspacePath,
			SSHHost:             entry.SSHHost,
			ToolTimeMs:          entry.ToolTimeMs,
			APITimeMs:           entry.APITimeMs,
			WorkflowFile:        entry.WorkflowFile,
			SelfReviewActive:    entry.SelfReviewActive,
			SelfReviewIteration: entry.SelfReviewIteration,
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
		snap.Retrying = append(snap.Retrying, SnapshotRetryEntry{
			IssueID:    entry.IssueID,
			Identifier: entry.Identifier,
			DisplayID:  entry.DisplayID,
			Attempt:    entry.Attempt,
			DueAtMS:    entry.DueAtMS,
			Error:      entry.Error,
		})
	}

	snap.AgentTotals = SnapshotAgentTotals{
		InputTokens:     state.AgentTotals.InputTokens,
		OutputTokens:    state.AgentTotals.OutputTokens,
		TotalTokens:     state.AgentTotals.TotalTokens,
		CacheReadTokens: state.AgentTotals.CacheReadTokens,
		SecondsRunning:  state.AgentTotals.SecondsRunning + activeElapsedTotal,
	}

	snap.BudgetExhaustedCount = len(state.BudgetExhausted)
	if len(state.BudgetExhausted) > 0 {
		ids := make([]string, 0, len(state.BudgetExhausted))
		for id := range state.BudgetExhausted {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		snap.BudgetExhausted = ids
	}

	if state.AgentRateLimits != nil {
		snap.RateLimits = maps.Clone(state.AgentRateLimits.Data)
	}

	return snap
}

// BuildReviewReactionConfig extracts and validates review-specific
// configuration from a [config.ReactionConfig]. Returns an error for
// invalid values.
func BuildReviewReactionConfig(rc config.ReactionConfig) (ReviewReactionConfig, error) {
	cfg := ReviewReactionConfig{
		Escalation:           rc.Escalation,
		EscalationLabel:      rc.EscalationLabel,
		PollIntervalMS:       120000,
		DebounceMS:           60000,
		MaxContinuationTurns: 3,
	}

	if cfg.Escalation == "" {
		cfg.Escalation = "label"
	}
	if cfg.Escalation != "label" && cfg.Escalation != "comment" {
		return ReviewReactionConfig{}, fmt.Errorf("invalid escalation %q: must be \"label\" or \"comment\"", cfg.Escalation)
	}

	if cfg.EscalationLabel == "" {
		cfg.EscalationLabel = "needs-human"
	}

	if v, ok := rc.Extra["poll_interval_ms"]; ok {
		n, err := toInt(v)
		if err != nil {
			return ReviewReactionConfig{}, fmt.Errorf("invalid poll_interval_ms: %w", err)
		}
		if n < 30000 {
			return ReviewReactionConfig{}, fmt.Errorf("poll_interval_ms must be >= 30000, got %d", n)
		}
		cfg.PollIntervalMS = n
	}

	if v, ok := rc.Extra["debounce_ms"]; ok {
		n, err := toInt(v)
		if err != nil {
			return ReviewReactionConfig{}, fmt.Errorf("invalid debounce_ms: %w", err)
		}
		if n < 0 {
			return ReviewReactionConfig{}, fmt.Errorf("debounce_ms must be non-negative, got %d", n)
		}
		cfg.DebounceMS = n
	}

	if v, ok := rc.Extra["max_continuation_turns"]; ok {
		n, err := toInt(v)
		if err != nil {
			return ReviewReactionConfig{}, fmt.Errorf("invalid max_continuation_turns: %w", err)
		}
		if n <= 0 {
			return ReviewReactionConfig{}, fmt.Errorf("max_continuation_turns must be positive, got %d", n)
		}
		cfg.MaxContinuationTurns = n
	}

	return cfg, nil
}

// toInt converts a YAML-decoded value (typically int or float64) to int.
// Fractional, NaN, and infinite float64 values are rejected.
func toInt(v any) (int, error) {
	switch n := v.(type) {
	case int:
		return n, nil
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, fmt.Errorf("expected finite numeric value, got %v", n)
		}
		if n != math.Trunc(n) {
			return 0, fmt.Errorf("expected integer value, got fractional %v", n)
		}
		return int(n), nil
	case int64:
		return int(n), nil
	default:
		return 0, fmt.Errorf("expected numeric value, got %T", v)
	}
}
