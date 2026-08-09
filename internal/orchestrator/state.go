package orchestrator

import (
	"context"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
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
	actionSweepExpired = "sweep_expired"

	handoffSuccess = "success"
	handoffError   = "error"
	handoffSkipped = "skipped"
)

// Budget reason values recorded in [State.BudgetExhaustedReason] and the
// runtime snapshot. budgetReasonToken takes precedence when both budgets
// are exhausted for one issue.
const (
	budgetReasonToken   = "token_budget"
	budgetReasonSession = "session_budget"
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

	// ObservedTerminalState is the terminal tracker state name that
	// reconciliation observed for this issue while the worker was still
	// running. Empty when reconciliation observed no terminal state. Set
	// beside PendingCleanup and never cleared: a terminal observation is
	// final for the lifetime of the entry.
	ObservedTerminalState string

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

	// ReactionKind is the reaction type that caused this worker dispatch.
	// Empty for first dispatches and non-reaction retries. Runtime-only;
	// not stored in SQLite run history or session metadata.
	ReactionKind string

	// SelfReviewActive is true when the worker is in the self-review phase.
	// Mutated only by the event loop via selfReviewCh.
	SelfReviewActive bool

	// SelfReviewIteration is the current review iteration (0 when not in review).
	// Mutated only by the event loop via selfReviewCh.
	SelfReviewIteration int

	// AgentKind is the adapter kind string (e.g. "claude-code") from the
	// workflow config active at dispatch time. Used for token cost
	// estimation on the dashboard.
	AgentKind string

	// RuleName is the dispatch rule name frozen at initial dispatch.
	// Empty when no rule matched and the workflow-wide fallback fired,
	// or "default" when the dispatch default block matched. Used in
	// logs, metrics, and the passive dashboard display.
	RuleName string

	// TemplateID is the resolved template registry key frozen at
	// initial dispatch. Empty selects the WORKFLOW.md body template.
	// Used by the worker to look up the parsed template via the
	// workflow manager's per-ID index.
	TemplateID string

	// LastMetadataWrite is the UTC time of the most recent incremental
	// session_metadata write for this issue. Owned by the single-writer
	// event loop and used to throttle in-session metadata writes. Zero
	// means no incremental write has occurred, so the first token_usage
	// event writes immediately.
	LastMetadataWrite time.Time
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
	// ReactionKindCI). Empty for non-reaction retries. Known non-empty
	// values cause HandleRetryTimer to call MarkReactionDispatched after
	// successful dispatch. Runtime-only (not persisted to SQLite).
	ReactionKind string

	// RuleName is the dispatch rule name frozen at initial dispatch.
	// Propagated verbatim through every retry so the freeze-on-dispatch
	// contract holds across reschedule, slot-exhaustion, and reaction
	// continuation paths.
	RuleName string

	// TemplateID is the resolved template registry key frozen at
	// initial dispatch. Propagated verbatim through every retry.
	TemplateID string

	// AgentKind is the adapter kind frozen at initial dispatch.
	// Propagated through every retry so [HandleRetryTimer] can look up
	// the adapter without re-running rule resolution.
	AgentKind string
}

// ReactionKindCI is the reaction kind constant for CI failure reactions.
const ReactionKindCI = "ci"

// ReactionKindReview is the reaction kind constant for PR review comment
// reactions.
const ReactionKindReview = "review"

// ReactionKindBotReview is the reaction kind constant for automated
// review bot PR comment reactions. The user-facing YAML key is
// reactions.bot_review; the short form lives here as the runtime and
// persisted discriminator.
const ReactionKindBotReview = "bot-review"

// ReactionKindAutoMerge is the reaction kind constant for auto-merge
// reactions. The user-facing YAML key is reactions.auto_merge; the
// short form lives here as the runtime and persisted discriminator.
const ReactionKindAutoMerge = "merge"

// ReactionKindMergeConflict is the reaction kind constant for
// merge-conflict reactions. The user-facing YAML key is
// reactions.merge_conflicts; the short form lives here as the runtime
// and persisted discriminator.
const ReactionKindMergeConflict = "merge-conflict"

// ReactionKindLabelReview is the reaction kind constant for the read-only
// PR review command triggered by the review label. The user-facing YAML
// block is reactions.label_commands; the short form lives here as the
// runtime and persisted discriminator, matching the YAML-versus-runtime
// asymmetry the sibling kinds document.
const ReactionKindLabelReview = "label-review"

// ReactionKindLabelFix is the reaction kind constant for the read-write
// PR fix command triggered by the fix label. The user-facing YAML block
// is reactions.label_commands; the short form lives here as the runtime
// and persisted discriminator, matching the YAML-versus-runtime
// asymmetry the sibling kinds document.
const ReactionKindLabelFix = "label-fix"

// ReactionKindMergeCompletion is the reaction kind constant for the
// merge-completion reaction. The user-facing YAML key is
// reactions.merge_completion; the short form lives here as the runtime
// and persisted discriminator, matching the YAML-versus-runtime
// asymmetry the sibling kinds document.
const ReactionKindMergeCompletion = "merge-completion"

// AutoMergePreflightRetryDelay is the delay between the initial
// auto-merge preflight failure (transport-class) and its single
// scheduled retry. The retry runs at most once per orchestrator
// lifetime.
const AutoMergePreflightRetryDelay time.Duration = 5 * time.Minute

// reactionKindPins is the single registry of reaction kinds. A kind
// present in the map is a known kind; its value reports whether a
// pending entry of that kind pins its workspace against sweep
// candidacy. Both [isKnownReactionKind] and [reactionKindPinsWorkspace]
// read this map, so a kind cannot be recognized without carrying an
// explicit pin classification.
var reactionKindPins = map[string]bool{
	ReactionKindCI:              true,
	ReactionKindReview:          true,
	ReactionKindBotReview:       true,
	ReactionKindAutoMerge:       true,
	ReactionKindMergeConflict:   true,
	ReactionKindLabelReview:     false,
	ReactionKindLabelFix:        false,
	ReactionKindMergeCompletion: false,
}

func isKnownReactionKind(kind string) bool {
	_, known := reactionKindPins[kind]
	return known
}

// reactionKindPinsWorkspace reports whether a pending reaction entry of
// the given kind excludes its workspace from sweep candidacy.
//
// An unregistered kind returns true, the non-destructive default,
// because retention rather than removal is the safe outcome for a kind
// the rest of the system does not yet classify.
func reactionKindPinsWorkspace(kind string) bool {
	pins, known := reactionKindPins[kind]
	if !known {
		return true
	}
	return pins
}

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

	// AgentKind is the dispatch-frozen adapter kind captured from the
	// completed worker. Propagated into the reaction continuation
	// retry so the same adapter handles the follow-up turn.
	AgentKind string

	// RuleName is the dispatch-frozen rule name captured from the
	// completed worker. Propagated for logs and metrics so the
	// continuation appears under the same rule as the original
	// dispatch.
	RuleName string

	// TemplateID is the dispatch-frozen template registry key
	// captured from the completed worker. Propagated so the
	// continuation renders the same template as the original
	// dispatch.
	TemplateID string
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

// BotReviewReactionData holds bot-review-specific fields for a pending
// bot-review reaction. Stored in [PendingReaction.KindData] for reactions
// with Kind == [ReactionKindBotReview]. Owner, Repo, Branch, and SHA are
// sourced from [domain.SCMMetadata] (written by the agent to scm.json),
// never from the tracker project configuration.
//
// Unlike [ReviewReactionData], there is no debounce timestamp: bot-review
// dispatches immediately.
type BotReviewReactionData struct {
	// PRNumber is the pull request number.
	PRNumber int

	// Owner is the repository owner.
	Owner string

	// Repo is the repository name.
	Repo string

	// Branch is the git branch name (PR head).
	Branch string

	// SHA is the git commit SHA at the last known push.
	SHA string
}

// BotReviewReactionConfig holds validated bot-review-specific
// configuration extracted from [config.ReactionConfig] at startup.
type BotReviewReactionConfig struct {
	Escalation           string
	EscalationLabel      string
	PollIntervalMS       int
	MaxContinuationTurns int
	BotUsernames         []string
}

// AutoMergeReactionData holds auto-merge-specific fields for a pending
// auto-merge reaction. Stored in [PendingReaction.KindData] for
// reactions with Kind == [ReactionKindAutoMerge]. Owner, Repo, Branch,
// and SHA are sourced from [domain.SCMMetadata] (written by the agent
// to scm.json), never from the tracker project configuration.
type AutoMergeReactionData struct {
	// PRNumber is the pull request number.
	PRNumber int

	// Owner is the repository owner.
	Owner string

	// Repo is the repository name.
	Repo string

	// Branch is the git branch name (PR head).
	Branch string

	// SHA is the git commit SHA at the last known push.
	SHA string
}

// AutoMergeReactionConfig holds validated auto-merge-specific
// configuration extracted from [config.ReactionConfig] at startup.
type AutoMergeReactionConfig struct {
	Strategy        domain.MergeStrategy
	RequireCI       bool
	DeleteBranch    bool
	PollIntervalMS  int
	Escalation      string
	EscalationLabel string
	MaxRetries      int
}

// MergeConflictReactionData holds merge-conflict-specific fields for a
// pending merge-conflict reaction. Stored in [PendingReaction.KindData]
// for reactions with Kind == [ReactionKindMergeConflict]. PRNumber,
// Owner, Repo, and Branch are sourced from [domain.SCMMetadata] (written
// by the agent to scm.json), never from the tracker project
// configuration.
//
// The rebase base branch is not stored here. It is read live from
// [domain.PRMergeStatus.BaseBranch] on each reconcile tick and threaded
// into the continuation data, so the agent always rebases onto the PR's
// current target rather than a value snapshotted at enqueue time.
type MergeConflictReactionData struct {
	// PRNumber is the pull request number.
	PRNumber int

	// Owner is the repository owner.
	Owner string

	// Repo is the repository name.
	Repo string

	// Branch is the PR head branch (the branch the agent rebases).
	Branch string

	// SHA is the git commit SHA at the last known push; carried for
	// logging and parity with the sibling reaction data.
	SHA string
}

// MergeConflictReactionConfig holds validated merge-conflict-specific
// configuration extracted from [config.ReactionConfig] at startup.
type MergeConflictReactionConfig struct {
	Escalation      string
	EscalationLabel string
	PollIntervalMS  int
	MaxRetries      int
}

// LabelReviewReactionData holds the label-review command's per-PR
// detection state. Stored in [PendingReaction.KindData] for reactions
// with Kind == [ReactionKindLabelReview]. PRNumber, Owner, and Repo are
// sourced from [domain.SCMMetadata] (written by the agent to scm.json),
// never from the tracker project configuration.
type LabelReviewReactionData struct {
	// PRNumber is the pull request number whose journal is polled.
	PRNumber int

	// Owner is the repository owner.
	Owner string

	// Repo is the repository name.
	Repo string

	// HighWaterMark caches the newest processed journal position as an
	// opaque "<RFC3339Nano>|<id>" string; empty means unset. The persisted
	// source of truth is the reaction_fingerprints row.
	HighWaterMark string

	// LastActor is the login from the most recently confirmed command,
	// carried into the dispatch context and logs.
	LastActor string
}

// LabelFixReactionData holds the label-fix command's per-PR detection
// state. Stored in [PendingReaction.KindData] for reactions with
// Kind == [ReactionKindLabelFix]. PRNumber, Owner, Repo, and Branch are
// sourced from [domain.SCMMetadata]; Branch is the PR head branch the
// dispatched fix session checks out and pushes to.
type LabelFixReactionData struct {
	PRNumber      int
	Owner         string
	Repo          string
	Branch        string // PR head branch to check out and push to
	HighWaterMark string // opaque "<RFC3339-9-digit-UTC>|<id>" position; empty means unset
	LastActor     string
}

// LabelReviewReactionConfig holds the validated label-review runtime
// configuration resolved from [config.LabelCommandsConfig]. ReviewLabel is
// the normalized label that triggers the command; empty disables the
// review command even when the block is otherwise active. PollIntervalMS
// is the detection poll interval, already clamped to the floor.
type LabelReviewReactionConfig struct {
	Provider       string
	ReviewLabel    string
	PollIntervalMS int
}

// LabelFixReactionConfig holds the validated label-fix runtime
// configuration resolved from [config.LabelCommandsConfig]. FixLabel is
// the normalized label that triggers the command; empty disables the fix
// command even when the block is otherwise active. PollIntervalMS is the
// detection poll interval, already clamped to the floor.
type LabelFixReactionConfig struct {
	Provider       string
	FixLabel       string
	PollIntervalMS int
}

// MergeCompletionReactionData holds merge-completion-specific fields for
// a pending merge-completion reaction. Stored in [PendingReaction.KindData]
// for reactions with Kind == [ReactionKindMergeCompletion]. PRNumber,
// Owner, and Repo are sourced from [domain.SCMMetadata] (written by the
// agent to scm.json), never from the tracker project configuration.
//
// Unlike the checkout-bearing sibling kinds, no branch or commit SHA is
// carried: the pass performs no checkout and fingerprints on the merge
// commit identifier observed live, not on a value snapshotted at
// enqueue time.
type MergeCompletionReactionData struct {
	// PRNumber is the pull request number.
	PRNumber int

	// Owner is the repository owner.
	Owner string

	// Repo is the repository name.
	Repo string
}

// MergeCompletionReactionConfig holds validated merge-completion-specific
// configuration extracted from [config.ReactionConfig] at startup.
type MergeCompletionReactionConfig struct {
	TargetState     string
	PollIntervalMS  int
	Escalation      string
	EscalationLabel string
	MaxRetries      int
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
	// has reached or exceeded the configured max_sessions budget, or
	// whose summed run_history total_tokens has reached or exceeded the
	// configured max_tokens budget. Rebuilt from batch SQLite queries at
	// the start of each poll tick when either budget is enabled. Updated
	// inline by [HandleRetryTimer] on budget exhaustion. [ShouldDispatch]
	// checks this set as a dispatch gate. Cleared when both budgets are 0.
	BudgetExhausted map[string]struct{}

	// BudgetExhaustedReason maps issue ID to the budget that fired for
	// that issue, either "token_budget" or "session_budget". It is kept
	// in lockstep with BudgetExhausted: every issue in BudgetExhausted
	// has a reason entry, and no entry survives for an issue that leaves
	// the set. When both budgets are exhausted for one issue the reason
	// is "token_budget". Owned by the single-writer event loop.
	BudgetExhaustedReason map[string]string

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

	// AutoMergePreflightFailed marks the auto-merge subsystem as
	// disabled for the lifetime of the process when the startup
	// preflight detects a missing token scope or a transport failure
	// the bounded retry could not heal. Read by reconcileAutoMerge to
	// drop pending merge entries with a single warn log per entry.
	AutoMergePreflightFailed bool

	// AutoMergePreflightRetryDueAt schedules a one-shot retry of the
	// auto-merge preflight after a transport-class startup failure.
	// Zero means no retry is scheduled. Cleared by the event-loop
	// goroutine when the retry is consumed; never re-set after the
	// consume runs.
	AutoMergePreflightRetryDueAt time.Time

	// AutoMergeAuthLogged tracks issues whose runtime ErrSCMAuth on
	// MergePR has already produced a structured ERROR log, so the log
	// fires at most once per issue per orchestrator lifetime.
	AutoMergeAuthLogged map[string]struct{}

	// MergeCompletionTerminalDriftLogged suppresses the reload-drift
	// warning emitted when the configured merge-completion target state
	// is no longer a member of the runtime terminal-state list. Set on
	// the onset of the condition and cleared once the target is present
	// again, so a later onset produces a further warning. Runtime-only;
	// mutated only by the reconcile pass on the event-loop goroutine.
	MergeCompletionTerminalDriftLogged bool

	// SweepTickCounter tracks poll ticks since the last workspace sweep.
	// Incremented by handleTick; reset to zero when the sweep fires.
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
		PollIntervalMS:        pollIntervalMS,
		MaxConcurrentAgents:   maxConcurrentAgents,
		MaxConcurrentByState:  maxConcurrentByState,
		Running:               make(map[string]*RunningEntry),
		Claimed:               make(map[string]struct{}),
		RetryAttempts:         make(map[string]*RetryEntry),
		Completed:             make(map[string]struct{}),
		BudgetExhausted:       make(map[string]struct{}),
		BudgetExhaustedReason: make(map[string]string),
		AgentTotals:           totals,
		ReactionAttempts:      make(map[string]int),
		PendingReactions:      make(map[string]*PendingReaction),
		AutoMergeAuthLogged:   make(map[string]struct{}),
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
	AgentKind           string                `json:"agent_kind,omitempty"`
	RuleName            string                `json:"rule_name,omitempty"`
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
	GeneratedAt           time.Time              `json:"generated_at"`
	Running               []SnapshotRunningEntry `json:"running"`
	Retrying              []SnapshotRetryEntry   `json:"retrying"`
	AgentTotals           SnapshotAgentTotals    `json:"agent_totals"`
	RateLimits            map[string]any         `json:"rate_limits"`
	BudgetExhaustedCount  int                    `json:"budget_exhausted_count"`
	BudgetExhausted       []string               `json:"budget_exhausted,omitempty"`
	BudgetExhaustedReason map[string]string      `json:"budget_exhausted_reason,omitempty"`
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
			maps.Copy(modelRequests, entry.RequestsByModel)
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
			AgentKind:           entry.AgentKind,
			RuleName:            entry.RuleName,
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

		reasons := make(map[string]string, len(state.BudgetExhausted))
		for _, id := range ids {
			reasons[id] = state.BudgetExhaustedReason[id]
		}
		snap.BudgetExhaustedReason = reasons
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

// BuildBotReviewReactionConfig extracts and validates bot-review-specific
// configuration from a [config.ReactionConfig]. Returns an error for
// invalid values.
//
// The poll-interval and continuation-turn defaults differ from
// [BuildReviewReactionConfig]: bot comments arrive in bulk on push and are
// dispatched immediately, so the poll cadence is tighter and the retry
// budget larger. No debounce field is read.
func BuildBotReviewReactionConfig(rc config.ReactionConfig) (BotReviewReactionConfig, error) {
	cfg := BotReviewReactionConfig{
		Escalation:           rc.Escalation,
		EscalationLabel:      rc.EscalationLabel,
		PollIntervalMS:       60000,
		MaxContinuationTurns: 5,
	}

	if cfg.Escalation == "" {
		cfg.Escalation = "label"
	}
	if cfg.Escalation != "label" && cfg.Escalation != "comment" {
		return BotReviewReactionConfig{}, fmt.Errorf("invalid escalation %q: must be \"label\" or \"comment\"", cfg.Escalation)
	}

	if cfg.EscalationLabel == "" {
		cfg.EscalationLabel = "needs-human"
	}

	if v, ok := rc.Extra["poll_interval_ms"]; ok {
		n, err := toInt(v)
		if err != nil {
			return BotReviewReactionConfig{}, fmt.Errorf("invalid poll_interval_ms: %w", err)
		}
		if n < 30000 {
			return BotReviewReactionConfig{}, fmt.Errorf("poll_interval_ms must be >= 30000, got %d", n)
		}
		cfg.PollIntervalMS = n
	}

	if v, ok := rc.Extra["max_continuation_turns"]; ok {
		n, err := toInt(v)
		if err != nil {
			return BotReviewReactionConfig{}, fmt.Errorf("invalid max_continuation_turns: %w", err)
		}
		if n <= 0 {
			return BotReviewReactionConfig{}, fmt.Errorf("max_continuation_turns must be positive, got %d", n)
		}
		cfg.MaxContinuationTurns = n
	}

	if v, ok := rc.Extra["bot_usernames"]; ok {
		list, lok := v.([]any)
		if !lok {
			return BotReviewReactionConfig{}, fmt.Errorf("invalid bot_usernames: expected list, got %T", v)
		}
		names := make([]string, 0, len(list))
		for i, elem := range list {
			s, sok := elem.(string)
			if !sok {
				return BotReviewReactionConfig{}, fmt.Errorf("invalid bot_usernames[%d]: expected string, got %T", i, elem)
			}
			names = append(names, s)
		}
		cfg.BotUsernames = names
	}

	return cfg, nil
}

// BuildAutoMergeReactionConfig extracts and validates auto-merge
// specific configuration from a [config.ReactionConfig]. Returns an
// error for invalid values.
func BuildAutoMergeReactionConfig(rc config.ReactionConfig) (AutoMergeReactionConfig, error) {
	cfg := AutoMergeReactionConfig{
		Strategy:        domain.StrategySquash,
		RequireCI:       true,
		DeleteBranch:    true,
		PollIntervalMS:  60000,
		Escalation:      rc.Escalation,
		EscalationLabel: rc.EscalationLabel,
		MaxRetries:      rc.MaxRetries,
	}

	if cfg.Escalation == "" {
		cfg.Escalation = "comment"
	}
	if cfg.Escalation != "label" && cfg.Escalation != "comment" {
		return AutoMergeReactionConfig{}, fmt.Errorf("invalid escalation %q: must be \"label\" or \"comment\"", cfg.Escalation)
	}

	if cfg.EscalationLabel == "" {
		cfg.EscalationLabel = "needs-human"
	}

	if v, ok := rc.Extra["strategy"]; ok {
		s, sok := v.(string)
		if !sok {
			return AutoMergeReactionConfig{}, fmt.Errorf("invalid strategy: expected string, got %T", v)
		}
		switch s {
		case "":
			cfg.Strategy = domain.StrategySquash
		case string(domain.StrategyMerge), string(domain.StrategySquash), string(domain.StrategyRebase):
			cfg.Strategy = domain.MergeStrategy(s)
		default:
			return AutoMergeReactionConfig{}, fmt.Errorf("invalid strategy %q: must be \"merge\", \"squash\", or \"rebase\"", s)
		}
	}

	if v, ok := rc.Extra["require_ci"]; ok {
		b, bok := v.(bool)
		if !bok {
			return AutoMergeReactionConfig{}, fmt.Errorf("invalid require_ci: expected bool, got %T", v)
		}
		cfg.RequireCI = b
	}

	if v, ok := rc.Extra["delete_branch"]; ok {
		b, bok := v.(bool)
		if !bok {
			return AutoMergeReactionConfig{}, fmt.Errorf("invalid delete_branch: expected bool, got %T", v)
		}
		cfg.DeleteBranch = b
	}

	if v, ok := rc.Extra["poll_interval_ms"]; ok {
		n, err := toInt(v)
		if err != nil {
			return AutoMergeReactionConfig{}, fmt.Errorf("invalid poll_interval_ms: %w", err)
		}
		if n < 30000 {
			return AutoMergeReactionConfig{}, fmt.Errorf("poll_interval_ms must be >= 30000, got %d", n)
		}
		cfg.PollIntervalMS = n
	}

	return cfg, nil
}

// BuildMergeConflictReactionConfig extracts and validates
// merge-conflict-specific configuration from a [config.ReactionConfig].
// Returns an error for invalid values.
//
// rc.MaxRetries is consumed verbatim; the per-kind default of 1 for
// merge_conflicts is applied earlier in config parsing, so this builder
// reads the already-coerced, non-negative value without re-applying it.
func BuildMergeConflictReactionConfig(rc config.ReactionConfig) (MergeConflictReactionConfig, error) {
	cfg := MergeConflictReactionConfig{
		Escalation:      rc.Escalation,
		EscalationLabel: rc.EscalationLabel,
		PollIntervalMS:  60000,
		MaxRetries:      rc.MaxRetries,
	}

	if cfg.Escalation == "" {
		cfg.Escalation = "label"
	}
	if cfg.Escalation != "label" && cfg.Escalation != "comment" {
		return MergeConflictReactionConfig{}, fmt.Errorf("invalid escalation %q: must be \"label\" or \"comment\"", cfg.Escalation)
	}

	if cfg.EscalationLabel == "" {
		cfg.EscalationLabel = "needs-human"
	}

	if v, ok := rc.Extra["poll_interval_ms"]; ok {
		n, err := toInt(v)
		if err != nil {
			return MergeConflictReactionConfig{}, fmt.Errorf("invalid poll_interval_ms: %w", err)
		}
		if n < 30000 {
			return MergeConflictReactionConfig{}, fmt.Errorf("poll_interval_ms must be >= 30000, got %d", n)
		}
		cfg.PollIntervalMS = n
	}

	return cfg, nil
}

// BuildLabelReviewReactionConfig copies the validated label-review runtime
// configuration out of the parsed label_commands block. Unlike the sibling
// Build*ReactionConfig functions it returns no error: the input is already
// fully validated and defaulted by config parsing, so there is nothing left
// to validate here.
func BuildLabelReviewReactionConfig(cfg config.LabelCommandsConfig) LabelReviewReactionConfig {
	return LabelReviewReactionConfig{
		Provider:       cfg.Provider,
		ReviewLabel:    cfg.ReviewLabel,
		PollIntervalMS: cfg.PollIntervalMS,
	}
}

// BuildLabelFixReactionConfig copies the validated label-fix runtime
// configuration out of the parsed label_commands block. Like
// BuildLabelReviewReactionConfig it returns no error: the input is
// already fully validated and defaulted by config parsing, so there is
// nothing left to validate here.
func BuildLabelFixReactionConfig(cfg config.LabelCommandsConfig) LabelFixReactionConfig {
	return LabelFixReactionConfig{
		Provider:       cfg.Provider,
		FixLabel:       cfg.FixLabel,
		PollIntervalMS: cfg.PollIntervalMS,
	}
}

// BuildMergeCompletionReactionConfig extracts and validates
// merge-completion-specific configuration from a [config.ReactionConfig].
// Returns an error naming the offending field for invalid values.
//
// meta resolves the default active-state list only; the terminal list is
// read from tracker as written, with no fallback to
// [registry.TrackerMeta.DefaultTerminalStates], so the offline validator
// and the runtime terminal drop agree on what "terminal" means. The
// function performs no I/O and no network call.
func BuildMergeCompletionReactionConfig(rc config.ReactionConfig, tracker config.TrackerConfig, meta registry.TrackerMeta) (MergeCompletionReactionConfig, error) {
	cfg := MergeCompletionReactionConfig{
		PollIntervalMS:  60000,
		Escalation:      rc.Escalation,
		EscalationLabel: rc.EscalationLabel,
		MaxRetries:      rc.MaxRetries,
	}

	if cfg.Escalation == "" {
		cfg.Escalation = "label"
	}
	if cfg.Escalation != "label" && cfg.Escalation != "comment" {
		return MergeCompletionReactionConfig{}, fmt.Errorf("invalid escalation %q: must be \"label\" or \"comment\"", cfg.Escalation)
	}

	if cfg.EscalationLabel == "" {
		cfg.EscalationLabel = "needs-human"
	}

	if tracker.HandoffState == "" {
		return MergeCompletionReactionConfig{}, fmt.Errorf("tracker.handoff_state is required when reactions.merge_completion is configured")
	}
	if len(tracker.TerminalStates) == 0 {
		return MergeCompletionReactionConfig{}, fmt.Errorf("tracker.terminal_states must be written when reactions.merge_completion is configured")
	}

	if raw, ok := rc.Extra["target_state"]; ok {
		s, sok := raw.(string)
		if !sok {
			return MergeCompletionReactionConfig{}, fmt.Errorf("invalid target_state: expected string, got %T", raw)
		}
		cfg.TargetState = s
	}
	if cfg.TargetState == "" {
		return MergeCompletionReactionConfig{}, fmt.Errorf("target_state is required")
	}

	effectiveActive := tracker.ActiveStates
	if len(effectiveActive) == 0 {
		effectiveActive = meta.DefaultActiveStates
	}

	if strings.EqualFold(cfg.TargetState, tracker.HandoffState) {
		return MergeCompletionReactionConfig{}, fmt.Errorf("invalid target_state %q: must not equal tracker.handoff_state", cfg.TargetState)
	}
	if _, active := stateSet(effectiveActive)[strings.ToLower(cfg.TargetState)]; active {
		return MergeCompletionReactionConfig{}, fmt.Errorf("invalid target_state %q: must not be a member of tracker.active_states", cfg.TargetState)
	}
	if _, terminal := stateSet(tracker.TerminalStates)[strings.ToLower(cfg.TargetState)]; !terminal {
		return MergeCompletionReactionConfig{}, fmt.Errorf("invalid target_state %q: must be a member of tracker.terminal_states", cfg.TargetState)
	}

	if v, ok := rc.Extra["poll_interval_ms"]; ok {
		n, err := toInt(v)
		if err != nil {
			return MergeCompletionReactionConfig{}, fmt.Errorf("invalid poll_interval_ms: %w", err)
		}
		if n < 30000 {
			return MergeCompletionReactionConfig{}, fmt.Errorf("poll_interval_ms must be >= 30000, got %d", n)
		}
		cfg.PollIntervalMS = n
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
