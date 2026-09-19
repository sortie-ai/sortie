package orchestrator

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

// Metric label values passed to domain.Metrics methods.
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

	handoffSuccess  = "success"
	handoffError    = "error"
	handoffSkipped  = "skipped"
	handoffWithheld = "withheld"
)

// Dispatch-gate reason values recorded in [BudgetExhaustedEntry.Reason].
// Token budget takes precedence over the session budget.
const (
	budgetReasonToken   = "token_budget"
	budgetReasonSession = "session_budget"
)

// knownBudgetReasons lists every dispatch-gate budget reason in the order
// the gauge reports them. A reason added above must be added here too, so
// the gauge recompute reports a value for it on every pass.
var knownBudgetReasons = []string{budgetReasonSession, budgetReasonToken}

// AgentTotals holds cumulative token and runtime counters across all ended
// agent sessions, persisted to SQLite and restored on startup.
// SecondsRunning tracks only ended-session time; the snapshot adds active
// sessions' elapsed time.
type AgentTotals struct {
	InputTokens     int64
	OutputTokens    int64
	TotalTokens     int64
	CacheReadTokens int64
	SecondsRunning  float64

	// UnmeasuredSessions counts ended sessions whose usage was never
	// recorded, cumulative across restarts. The counters above exclude
	// these sessions.
	UnmeasuredSessions int64
}

// RateLimitSnapshot holds the latest rate-limit payload from an agent
// event. Opaque because the payload format is agent-adapter-defined.
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

	// Issue is the last-known normalized issue snapshot, updated by
	// reconciliation.
	Issue domain.Issue

	// SessionID is the adapter-assigned session identifier, empty until
	// the worker reports session_started.
	SessionID string

	// DispatchID is minted by [DispatchIssue] and never reassigned for
	// the lifetime of the entry.
	DispatchID string

	ThreadID string

	TurnID string

	AgentPID string

	LastAgentEvent domain.AgentEventType

	LastAgentTimestamp time.Time

	LastAgentMessage string

	AgentInputTokens int64

	AgentOutputTokens int64

	AgentTotalTokens int64

	// LastReportedInputTokens is the last absolute input value reported by
	// the agent, used to compute deltas and avoid double-counting.
	LastReportedInputTokens int64

	LastReportedOutputTokens int64

	LastReportedTotalTokens int64

	CacheReadTokens int64

	LastReportedCacheReadTokens int64

	ModelName string

	// APIRequestCount is the number of token_usage events received.
	// Whether that counts as model API requests is resolved separately by
	// apiRequestsMeasured.
	APIRequestCount int

	// RequestsByModel maps model name to token_usage event count. Nil
	// until the first token_usage event carrying a model name arrives.
	RequestsByModel map[string]int

	// RetryAttempt is nil for first dispatch, non-nil and >= 1 for retries
	// and continuations.
	RetryAttempt *int

	StartedAt time.Time

	// TurnCount is the number of coding-agent turns started this worker
	// lifetime, self-review turns included.
	TurnCount int

	// CancelFunc cancels the per-worker context. Nil only in test fixtures
	// that bypass [DispatchIssue].
	CancelFunc context.CancelFunc

	// PendingCleanup is set by reconciliation on a terminal-state
	// observation; [HandleWorkerExit] performs the cleanup after the
	// worker goroutine exits, deferring it until the agent process has
	// terminated to avoid races with active writes.
	PendingCleanup bool

	// ObservedTerminalState is the terminal state reconciliation observed
	// while the worker ran. Set beside PendingCleanup and never cleared: a
	// terminal observation is final for the entry's lifetime.
	ObservedTerminalState string

	// WorkspacePath is the absolute workspace directory, populated in
	// [HandleWorkerExit] before cleanup. The PendingCleanup path cleans
	// this actual directory rather than reconstructing it from config,
	// which may have changed via reload.
	WorkspacePath string

	// WorkflowFile is the base filename of the WORKFLOW.md active at
	// dispatch, persisted in run_history.
	WorkflowFile string

	// SSHHost is the SSH host this worker executes on, empty for local.
	SSHHost string

	// ToolTimeMs is cumulative tool-call execution time, from tool_result
	// events.
	ToolTimeMs int64

	// APITimeMs is cumulative LLM API wait time, from any event carrying
	// APIDurationMS > 0.
	APITimeMs int64

	// ContinuationContext carries reaction continuation data from a retry
	// into the worker session. Nil for normal dispatches.
	ContinuationContext map[string]any

	// ReactionKind is the reaction type that caused this dispatch, empty
	// for first dispatches and non-reaction retries. Runtime-only.
	ReactionKind string

	// SelfReviewActive is true during the self-review phase. Mutated only
	// by the event loop via selfReviewCh.
	SelfReviewActive bool

	SelfReviewIteration int

	// AgentKind is the adapter kind (e.g. "claude-code") from the config
	// active at dispatch.
	AgentKind string

	// RuleName is the dispatch rule frozen at initial dispatch. Empty when
	// the workflow-wide fallback fired, "default" when the default block
	// matched.
	RuleName string

	// TemplateID is the resolved template registry key frozen at dispatch.
	// Empty selects the WORKFLOW.md body template.
	TemplateID string

	// LastMetadataWrite throttles in-session metadata writes. Zero means
	// none yet, so the first token_usage event writes immediately.
	LastMetadataWrite time.Time

	// UsageMeasured is true once a usage measurement has been reported this
	// session. Monotone; never set when UsageArrival is none.
	UsageMeasured bool

	// UsageArrival and UsageAttribution are the usage-reporting disposition
	// resolved for this session, frozen at dispatch. Owned exclusively by
	// the single-writer event loop.
	UsageArrival     registry.UsageArrival
	UsageAttribution registry.UsageAttribution

	// APIRequestCountAtLastTurnEnd is APIRequestCount as of the most recent
	// turn-terminal event, zero until the first turn ends.
	APIRequestCountAtLastTurnEnd int

	// IssueTokensCompleted is the issue's summed run_history total_tokens
	// across completed sessions, read at dispatch and replaced by a
	// confirming read. 0 when the dispatch read failed.
	IssueTokensCompleted int64

	// TokenCeilingStopped latches once the in-flight ceiling has stopped
	// this run.
	TokenCeilingStopped bool

	// TokenCeilingQueryWarned latches once the ceiling's confirming read
	// has failed and been reported for this run.
	TokenCeilingQueryWarned bool

	// TokenCeilingAtStop is the ceiling in force when this run was stopped.
	// A reload can move the configured ceiling between the stop and the
	// exit, and the durable record must name the ceiling the run hit.
	// Meaningful only while TokenCeilingStopped is true.
	TokenCeilingAtStop int
}

// RetryEntry holds the runtime state for a pending retry. IssueID,
// Identifier, Attempt, DueAtMS, and Error map to persistence.RetryEntry;
// the rest is runtime-only and reconstructed on startup from persisted
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

	// LastSSHHost is the SSH host from the previous attempt, passed as the
	// preferred host to [HostPool.AcquireHost]. Runtime-only.
	LastSSHHost string

	// scheduledAt is time.Now() when ScheduleRetry created this entry. Its
	// monotonic reading makes staleness detection immune to wall-clock
	// jumps. Zero when reconstructed from SQLite at startup.
	scheduledAt time.Time

	// scheduledDelayMS is the delay passed to ScheduleRetry. With
	// scheduledAt it enables the monotonic staleness check: a fire before
	// its intended moment indicates a stale callback from a replaced timer.
	scheduledDelayMS int64

	// ContinuationContext carries reaction continuation data for
	// first-turn template injection. Nil for non-reaction retries.
	ContinuationContext map[string]any

	// ReactionKind is the reaction type that triggered this retry. A known
	// non-empty value causes MarkReactionDispatched after successful
	// dispatch. Runtime-only.
	ReactionKind string

	// RuleName is the dispatch rule frozen at initial dispatch, propagated
	// verbatim through every retry.
	RuleName string

	// TemplateID is the resolved template registry key frozen at initial
	// dispatch, propagated verbatim through every retry.
	TemplateID string

	// AgentKind is the adapter kind frozen at initial dispatch, propagated
	// so [HandleRetryTimer] resolves the adapter without re-running rule
	// resolution.
	AgentKind string

	// pausedSinceMS is the wall-clock ms at which this entry first took one
	// of HandleRetryTimer's paused-by-issue-state arms. Zero means not
	// paused. Runtime-only: never persisted, never on ScheduleRetryParams.
	pausedSinceMS int64
}

// ReactionKindCI is the reaction kind for CI failure reactions.
const ReactionKindCI = "ci"

// ReactionKindReview is the reaction kind for PR review comment reactions.
const ReactionKindReview = "review"

// ReactionKindBotReview is the reaction kind for automated review-bot PR
// comment reactions. The YAML key is reactions.bot_review.
const ReactionKindBotReview = "bot-review"

// ReactionKindAutoMerge is the reaction kind for auto-merge reactions. The
// YAML key is reactions.auto_merge.
const ReactionKindAutoMerge = "merge"

// ReactionKindMergeConflict is the reaction kind for merge-conflict
// reactions. The YAML key is reactions.merge_conflicts.
const ReactionKindMergeConflict = "merge-conflict"

// ReactionKindLabelReview is the reaction kind for the read-only PR review
// command triggered by the review label. The YAML block is
// reactions.label_commands.
const ReactionKindLabelReview = "label-review"

// ReactionKindLabelFix is the reaction kind for the read-write PR fix
// command triggered by the fix label. The YAML block is
// reactions.label_commands.
const ReactionKindLabelFix = "label-fix"

// ReactionKindMergeCompletion is the reaction kind for the
// merge-completion reaction. The YAML key is reactions.merge_completion.
const ReactionKindMergeCompletion = "merge-completion"

// AutoMergePreflightRetryDelay is the delay between the initial auto-merge
// preflight failure and its single scheduled retry, which runs at most
// once per orchestrator lifetime.
const AutoMergePreflightRetryDelay time.Duration = 5 * time.Minute

// reactionWatchWindowDefaultMS is the default pending-entry watch window
// for the four reaction kinds watchWindowMS serves.
const reactionWatchWindowDefaultMS = 1800000

// watchWindowMS reads the optional watch_window_ms key shared by
// review_comments, bot_review, auto_merge, and merge_conflicts, returning
// def when absent.
func watchWindowMS(extra map[string]any, def int) (int, error) {
	v, ok := extra["watch_window_ms"]
	if !ok {
		return def, nil
	}
	n, err := toInt(v)
	if err != nil {
		return 0, fmt.Errorf("invalid watch_window_ms: %w", err)
	}
	if err := config.ValidateWatchWindowMS(n); err != nil {
		return 0, fmt.Errorf("watch_window_ms %s", err)
	}
	return n, nil
}

// reactionKindPins is the single registry of reaction kinds. Presence in
// the map means the kind is known; the value reports whether a pending
// entry pins its workspace against sweep candidacy. Both
// [isKnownReactionKind] and [reactionKindPinsWorkspace] read it, so a kind
// cannot be recognized without an explicit pin classification.
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
// the given kind excludes its workspace from sweep candidacy. An
// unregistered kind returns true, the non-destructive default: retention
// is safe for a kind the rest of the system does not yet classify.
func reactionKindPinsWorkspace(kind string) bool {
	pins, known := reactionKindPins[kind]
	if !known {
		return true
	}
	return pins
}

// ReactionKey returns the composite map key for a pending reaction.
// Callers must not pass IDs containing the colon delimiter.
func ReactionKey(issueID, kind string) string {
	return issueID + ":" + kind
}

// PendingReaction records that an issue needs external signal
// reconciliation. Created by worker exit handlers or external event
// receivers, consumed by per-kind reconcile functions. Runtime-only;
// cross-restart dedup uses reaction_fingerprints.
type PendingReaction struct {
	IssueID string

	// Identifier is the human-readable ticket key (e.g. "MT-649").
	Identifier string

	DisplayID string

	// Attempt is the overall run attempt number from the completed worker.
	Attempt int

	// Kind is the reaction type constant (e.g. ReactionKindCI).
	Kind string

	// LastSSHHost is the SSH host from the completed worker, used for host
	// preference on fix redispatch.
	LastSSHHost string

	CreatedAt time.Time

	// PendingAttempts counts consecutive passes that reached no dispatch,
	// used for exponential backoff.
	PendingAttempts int

	// PendingRetryAt is the earliest time to poll again. Zero means ready
	// immediately.
	PendingRetryAt time.Time

	// HeadRecordedAt is when this process recorded the commit currently
	// evaluated. Zero until first recorded, and zero for a startup-restored
	// entry, because a head recorded by a previous process cannot bound a
	// query over this process's activity. Read only by kinds whose subject
	// is the pull request head.
	HeadRecordedAt time.Time

	// EscalatedForCurrentHead reports whether escalation has been applied
	// for the recorded commit. Cleared when a different head is recorded,
	// so an exhausted budget escalates at most once per commit. Read only
	// by kinds whose subject is the pull request head.
	EscalatedForCurrentHead bool

	// KindData holds kind-specific typed data (CI uses [*CIReactionData]).
	// Each kind's reconcile function asserts the type once.
	KindData any

	// AgentKind is the dispatch-frozen adapter kind from the completed
	// worker, propagated so the same adapter handles the follow-up turn.
	AgentKind string

	// RuleName is the dispatch-frozen rule name from the completed worker,
	// propagated so the continuation appears under the same rule.
	RuleName string

	// TemplateID is the dispatch-frozen template key from the completed
	// worker, propagated so the continuation renders the same template.
	TemplateID string

	// Triage is the in-flight or finished triage run for the current
	// subject. Nil when none started, cleared when the fingerprint moves.
	// Runtime-only.
	Triage *ReactionTriageRun
}

// CIReactionData holds CI-specific fields for a pending CI reaction,
// stored in [PendingReaction.KindData] for Kind == [ReactionKindCI].
// PRNumber, Owner, and Repo come from [domain.SCMMetadata] (scm.json),
// never from tracker project config.
//
// SHA is the head recorded at worker exit, not the polled ref: the reaction
// reads the live head from [domain.PRMergeStatus.HeadSHA] each tick, so a
// commit pushed after handoff is the one the verdict describes.
type CIReactionData struct {
	PRNumber int

	// Owner is the repository owner.
	Owner string

	// Repo is the repository name.
	Repo string

	// Branch is the git branch name from SCM metadata.
	Branch string

	// SHA is the git commit SHA from SCM metadata, carried for logging
	// and parity with the sibling reaction data.
	SHA string
}

// ReviewReactionData holds review-specific fields for a pending review
// reaction, stored in [PendingReaction.KindData] for Kind ==
// [ReactionKindReview]. Owner and Repo come from [domain.SCMMetadata]
// (scm.json), never from tracker project config.
type ReviewReactionData struct {
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

// ReviewReactionConfig holds validated review-specific configuration from
// [config.ReactionConfig].
type ReviewReactionConfig struct {
	Escalation           string
	EscalationLabel      string
	PollIntervalMS       int
	DebounceMS           int
	MaxContinuationTurns int
	WatchWindowMS        int

	// Triage is the frozen triage config, copied verbatim at construction.
	Triage config.ReactionTriageConfig
}

// BotReviewReactionData holds bot-review-specific fields for a pending
// bot-review reaction, stored in [PendingReaction.KindData] for Kind ==
// [ReactionKindBotReview]. Owner, Repo, Branch, and SHA come from
// [domain.SCMMetadata] (scm.json), never from tracker project config.
// Unlike [ReviewReactionData] there is no debounce timestamp: bot-review
// dispatches immediately.
type BotReviewReactionData struct {
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
// configuration from [config.ReactionConfig].
type BotReviewReactionConfig struct {
	Escalation           string
	EscalationLabel      string
	PollIntervalMS       int
	MaxContinuationTurns int
	BotUsernames         []string
	WatchWindowMS        int

	// Triage is the frozen triage config, copied verbatim at construction.
	Triage config.ReactionTriageConfig
}

// AutoMergeReactionData holds auto-merge-specific fields for a pending
// auto-merge reaction, stored in [PendingReaction.KindData] for Kind ==
// [ReactionKindAutoMerge]. Owner, Repo, Branch, and SHA come from
// [domain.SCMMetadata] (scm.json), never from tracker project config.
type AutoMergeReactionData struct {
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
// configuration from [config.ReactionConfig].
type AutoMergeReactionConfig struct {
	Strategy        domain.MergeStrategy
	RequireCI       bool
	DeleteBranch    bool
	PollIntervalMS  int
	Escalation      string
	EscalationLabel string
	MaxRetries      int
	WatchWindowMS   int
}

// MergeConflictReactionData holds merge-conflict-specific fields for a
// pending merge-conflict reaction, stored in [PendingReaction.KindData]
// for Kind == [ReactionKindMergeConflict]. PRNumber, Owner, Repo, and
// Branch come from [domain.SCMMetadata] (scm.json), never from tracker
// project config.
//
// The rebase base branch is not stored: it is read live from
// [domain.PRMergeStatus.BaseBranch] each tick, so the agent rebases onto
// the PR's current target rather than a value snapshotted at enqueue.
type MergeConflictReactionData struct {
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
// configuration from [config.ReactionConfig].
type MergeConflictReactionConfig struct {
	Escalation      string
	EscalationLabel string
	PollIntervalMS  int
	MaxRetries      int
	WatchWindowMS   int

	// Triage is the frozen triage config, copied verbatim at construction.
	Triage config.ReactionTriageConfig
}

// LabelReviewReactionData holds the label-review command's per-PR
// detection state, stored in [PendingReaction.KindData] for Kind ==
// [ReactionKindLabelReview]. PRNumber, Owner, and Repo come from
// [domain.SCMMetadata] (scm.json), never from tracker project config.
type LabelReviewReactionData struct {
	PRNumber int

	// Owner is the repository owner.
	Owner string

	// Repo is the repository name.
	Repo string

	// HighWaterMark caches the newest processed journal position as an
	// opaque "<RFC3339Nano>|<id>" string; empty means unset. The persisted
	// source of truth is the reaction_fingerprints row.
	HighWaterMark string

	// LastActor is the login from the most recently confirmed command.
	LastActor string
}

// LabelFixReactionData holds the label-fix command's per-PR detection
// state, stored in [PendingReaction.KindData] for Kind ==
// [ReactionKindLabelFix]. Fields come from [domain.SCMMetadata]; Branch is
// the PR head branch the fix session checks out and pushes to.
type LabelFixReactionData struct {
	PRNumber      int
	Owner         string
	Repo          string
	Branch        string // PR head branch to check out and push to
	HighWaterMark string // opaque "<RFC3339-9-digit-UTC>|<id>" position; empty means unset
	LastActor     string
}

// LabelReviewReactionConfig holds the validated label-review runtime
// configuration from [config.LabelCommandsConfig]. An empty ReviewLabel
// disables the review command. PollIntervalMS is already clamped to the
// floor.
type LabelReviewReactionConfig struct {
	Provider       string
	ReviewLabel    string
	PollIntervalMS int
}

// LabelFixReactionConfig holds the validated label-fix runtime
// configuration from [config.LabelCommandsConfig]. An empty FixLabel
// disables the fix command. PollIntervalMS is already clamped to the
// floor.
type LabelFixReactionConfig struct {
	Provider       string
	FixLabel       string
	PollIntervalMS int
}

// MergeCompletionReactionData holds merge-completion-specific fields for a
// pending merge-completion reaction, stored in [PendingReaction.KindData]
// for Kind == [ReactionKindMergeCompletion]. Fields come from
// [domain.SCMMetadata] (scm.json), never from tracker project config.
// Unlike the checkout-bearing kinds no branch or SHA is carried: the pass
// performs no checkout and fingerprints on the live merge commit.
type MergeCompletionReactionData struct {
	PRNumber int

	// Owner is the repository owner.
	Owner string

	// Repo is the repository name.
	Repo string
}

// MergeCompletionReactionConfig holds validated merge-completion-specific
// configuration from [config.ReactionConfig].
type MergeCompletionReactionConfig struct {
	TargetState     string
	PollIntervalMS  int
	Escalation      string
	EscalationLabel string
	MaxRetries      int
}

// BudgetExhaustedEntry is the runtime view of one issue held out of
// dispatch by a per-issue budget ceiling. Owned by the single-writer event
// loop; replaced wholesale by the per-tick rebuild, updated in place by the
// retry lane's block.
type BudgetExhaustedEntry struct {
	Identifier         string // human-readable ticket key; empty when the candidate carried none
	DisplayID          string // qualified form of Identifier; empty when Identifier is display-ready
	Reason             string // budgetReasonSession or budgetReasonToken
	UsedSessions       int    // completed sessions counted from run history
	BudgetSessions     int    // configured agent.max_sessions; 0 means unlimited
	UsedTokens         *int64 // nil when the token ceiling was not evaluated for this issue
	BudgetTokens       int64  // configured agent.max_tokens; 0 means unlimited
	UnmeasuredSessions *int   // nil exactly when UsedTokens is nil; else runs whose spend is unknown
	StoppedInFlight    *int   // nil exactly when UsedTokens is nil; else sessions the token ceiling stopped mid-run
	ExhaustedAt        time.Time
}

// BudgetAnnouncement remembers what the operator was already told about a
// budget hold, so a hold that leaves and re-enters the candidate set is not
// announced twice.
type BudgetAnnouncement struct {
	Reason string
	At     time.Time
}

// ParkedEntry is the runtime view of one issue held out of primary
// dispatch until the orchestrator observes that a person acted on it.
type ParkedEntry struct {
	Identifier   string
	DisplayID    string
	Reason       string // "agent_blocked" or "handoff_absence"
	ParkedState  string // tracker state observed at park time; empty when unobserved
	Label        string // parking label the orchestrator applied
	LabelApplied bool
	ParkedAt     time.Time
}

// State is the single authoritative runtime state owned by the
// orchestrator. Not safe for concurrent access: all mutations are
// serialized through the event loop goroutine, except WorkerWg, which is
// goroutine-safe. The agent_totals and completed set are backed by SQLite
// and survive restarts.
type State struct {
	// WorkerWg tracks in-flight worker goroutines spawned by
	// [DispatchIssue]. Callers dispatching outside the Run() loop can
	// Wait() before cleanup.
	WorkerWg sync.WaitGroup

	// TrackerOpsWg tracks fire-and-forget tracker API goroutines, drained
	// after worker shutdown so they are not orphaned on exit.
	TrackerOpsWg sync.WaitGroup

	// TriageWg tracks in-flight reaction triage goroutines so shutdown can
	// drain them.
	TriageWg sync.WaitGroup

	// TriageInFlight counts running triage subprocesses. The event loop
	// increments on start; the runner goroutine decrements on return, so
	// the count stays true even when no pass consumes the outcome. Atomic
	// because its two writers are different goroutines.
	TriageInFlight atomic.Int64

	PollIntervalMS int

	MaxConcurrentAgents int

	// MaxConcurrentByState holds per-state concurrency caps, keys
	// lowercased. An absent key falls back to the global limit.
	MaxConcurrentByState map[string]int

	// MaxTokens mirrors config.Agent.MaxTokens for the in-flight token
	// ceiling, refreshed on every poll tick. 0 means unlimited. Written
	// only on the event-loop goroutine.
	MaxTokens int

	// Running maps issue ID to the live session entry. Only the event loop
	// may mutate this map.
	Running map[string]*RunningEntry

	// Claimed is the set of issue IDs reserved by the orchestrator
	// (running, retry-queued, or being dispatched), preventing duplicate
	// dispatch.
	Claimed map[string]struct{}

	RetryAttempts map[string]*RetryEntry

	// Completed is the set of issue IDs that have completed at least once.
	// Bookkeeping only, not used for dispatch gating.
	Completed map[string]struct{}

	// BudgetExhausted maps issue ID to the runtime view of an issue blocked
	// by a durable run_history-derived budget. Written only on the event
	// loop, from the poll tick's rebuild (which replaces the map wholesale)
	// and the retry lane's blockBudget (which writes one entry). Read by
	// the dispatch membership tests and by [RuntimeSnapshot].
	BudgetExhausted map[string]*BudgetExhaustedEntry

	// BudgetAnnounced maps issue ID to what the operator was already told
	// about a budget hold, so a hold that re-enters the candidate set under
	// the same reason is not announced twice. Records a fact about this
	// process's own log and dies with the process. An entry survives until
	// the hold clears through a candidate observation under the current
	// ceiling, or until restart.
	BudgetAnnounced map[string]BudgetAnnouncement

	// BudgetHoldNoticed maps issue ID to the budget-hold reason already
	// announced on the tracker item. It mirrors the budget_hold_notices
	// table, loaded from it at startup. It MUST NOT be collapsed into
	// BudgetAnnounced: one records a fact about this process, the other a
	// fact about the tracker that outlives the process. An entry survives
	// until the issue is next observed as an unheld candidate, or until
	// both budgets are disabled.
	BudgetHoldNoticed map[string]string

	// BudgetHoldNoticeWindowStart and BudgetHoldNoticesInWindow pace the
	// notice-write bound shared by the rebuild and the retry lane: at most
	// maxBudgetHoldNoticesPerWindow notices per budgetHoldNoticeWindow,
	// counted across both lanes. A restart begins a fresh window.
	BudgetHoldNoticeWindowStart time.Time
	BudgetHoldNoticesInWindow   int

	// Parked maps issue ID to an issue held out of primary dispatch until a
	// person acts on it. The durable mirror is the parked_issues table.
	Parked map[string]*ParkedEntry

	// AgentTotals holds aggregate token counts and cumulative runtime
	// seconds across all ended sessions. Active-session elapsed time is
	// computed at snapshot time.
	AgentTotals AgentTotals

	// AgentRateLimits is the most recent rate-limit payload from any agent
	// event. Nil when none observed.
	AgentRateLimits *RateLimitSnapshot

	// ReactionAttempts maps composite key (issueID:kind) to the count of
	// reaction-triggered continuations. Reset when the issue leaves Running
	// or RetryAttempts. Runtime-only.
	ReactionAttempts map[string]int

	// PendingReactions maps composite key (issueID:kind) to a
	// [PendingReaction]. Populated by [HandleWorkerExit], consumed by
	// per-kind reconcile functions. Runtime-only.
	PendingReactions map[string]*PendingReaction

	// AutoMergePreflightFailed disables the auto-merge subsystem for the
	// process lifetime when startup preflight detects a missing token scope
	// or an unrecoverable transport failure.
	AutoMergePreflightFailed bool

	// AutoMergePreflightRetryDueAt schedules a one-shot preflight retry
	// after a transport-class startup failure. Zero means none scheduled;
	// cleared by the event loop when consumed and never re-set.
	AutoMergePreflightRetryDueAt time.Time

	// AutoMergeAuthLogged tracks issues whose runtime ErrSCMAuth on MergePR
	// already logged, so the log fires at most once per issue per lifetime.
	AutoMergeAuthLogged map[string]struct{}

	// MergeCompletionTerminalDriftLogged suppresses the reload-drift warning
	// while the merge-completion target state is no longer terminal. Cleared
	// once the target is terminal again, so a later onset warns anew.
	// Runtime-only; mutated only by the reconcile pass on the event loop.
	MergeCompletionTerminalDriftLogged bool

	// SweepTickCounter tracks poll ticks since the last workspace sweep,
	// reset when the sweep fires. Runtime-only.
	SweepTickCounter int

	// BlockerReadOffset is the position, among a tick's needy candidates, at
	// which the per-pass blocker-read budget resumes. Mutated once per tick
	// by the event loop. Runtime-only.
	BlockerReadOffset int

	// TokenBudgetIncomplete is the set of issue IDs whose per-issue token
	// spend is below max_tokens but only a lower bound, so the ceiling could
	// not be fully evaluated. Rebuilt wholesale each tick when the token
	// budget is enabled, cleared when not. Owned by the event loop.
	TokenBudgetIncomplete map[string]struct{}
}

// continuationCtxKey is the context key for reaction continuation data
// passed from dispatch to the worker goroutine.
type continuationCtxKey struct{}

// WithContinuationContext returns a child context carrying reaction
// continuation data, read by [ContinuationFromContext].
func WithContinuationContext(ctx context.Context, data map[string]any) context.Context {
	return context.WithValue(ctx, continuationCtxKey{}, data)
}

// ContinuationFromContext extracts the continuation map injected by
// [WithContinuationContext], or nil when absent.
func ContinuationFromContext(ctx context.Context) map[string]any {
	v, _ := ctx.Value(continuationCtxKey{}).(map[string]any)
	return v
}

// NewState creates an initialized [State] with empty collections and the
// provided config values. maxConcurrentByState keys must be pre-normalized
// to lowercase by the caller.
func NewState(pollIntervalMS, maxConcurrentAgents, maxTokens int, maxConcurrentByState map[string]int, totals AgentTotals) *State {
	if maxConcurrentByState == nil {
		maxConcurrentByState = make(map[string]int)
	}
	return &State{
		PollIntervalMS:        pollIntervalMS,
		MaxConcurrentAgents:   maxConcurrentAgents,
		MaxTokens:             maxTokens,
		MaxConcurrentByState:  maxConcurrentByState,
		Running:               make(map[string]*RunningEntry),
		Claimed:               make(map[string]struct{}),
		RetryAttempts:         make(map[string]*RetryEntry),
		Completed:             make(map[string]struct{}),
		BudgetExhausted:       make(map[string]*BudgetExhaustedEntry),
		BudgetAnnounced:       make(map[string]BudgetAnnouncement),
		BudgetHoldNoticed:     make(map[string]string),
		Parked:                make(map[string]*ParkedEntry),
		AgentTotals:           totals,
		ReactionAttempts:      make(map[string]int),
		PendingReactions:      make(map[string]*PendingReaction),
		AutoMergeAuthLogged:   make(map[string]struct{}),
		TokenBudgetIncomplete: make(map[string]struct{}),
	}
}

func (s *State) RunningCount() int {
	return len(s.Running)
}

// RunningCountByState counts running entries whose Issue.State matches
// state (case-insensitive).
func RunningCountByState(running map[string]*RunningEntry, state string) int {
	count := 0
	for _, entry := range running {
		if strings.EqualFold(entry.Issue.State, state) {
			count++
		}
	}
	return count
}

// SnapshotRunningEntry is a read-only view of a running session, produced
// by [RuntimeSnapshot].
type SnapshotRunningEntry struct {
	IssueID             string                    `json:"issue_id"`
	Identifier          string                    `json:"issue_identifier"`
	DisplayID           string                    `json:"display_identifier,omitempty"`
	State               string                    `json:"state"`
	SessionID           string                    `json:"session_id"`
	TurnCount           int                       `json:"turn_count"`
	LastAgentEvent      domain.AgentEventType     `json:"last_event"`
	LastAgentTimestamp  time.Time                 `json:"last_event_at"`
	LastAgentMessage    string                    `json:"last_message"`
	StartedAt           time.Time                 `json:"started_at"`
	AgentInputTokens    int64                     `json:"input_tokens"`
	AgentOutputTokens   int64                     `json:"output_tokens"`
	AgentTotalTokens    int64                     `json:"total_tokens"`
	CacheReadTokens     int64                     `json:"cache_read_tokens"`
	ModelName           string                    `json:"model_name,omitempty"`
	APIRequestCount     int                       `json:"api_request_count"`
	RequestsByModel     map[string]int            `json:"requests_by_model,omitempty"`
	APIRequestsMeasured bool                      `json:"api_requests_measured"`
	WorkspacePath       string                    `json:"workspace_path"`
	SSHHost             string                    `json:"ssh_host,omitempty"`
	ToolTimeMs          int64                     `json:"tool_time_ms"`
	APITimeMs           int64                     `json:"api_time_ms"`
	WorkflowFile        string                    `json:"workflow_file,omitempty"`
	SelfReviewActive    bool                      `json:"self_review_active,omitempty"`
	SelfReviewIteration int                       `json:"self_review_iteration,omitempty"`
	AgentKind           string                    `json:"agent_kind,omitempty"`
	RuleName            string                    `json:"rule_name,omitempty"`
	UsageMeasured       bool                      `json:"tokens_measured"`
	UsageArrival        registry.UsageArrival     `json:"usage_arrival"`
	UsageAttribution    registry.UsageAttribution `json:"usage_attribution"`
	TokensPending       bool                      `json:"tokens_pending"`
}

// SnapshotRetryEntry is a read-only view of a pending retry, produced by
// [RuntimeSnapshot].
type SnapshotRetryEntry struct {
	IssueID    string `json:"issue_id"`
	Identifier string `json:"issue_identifier"`
	DisplayID  string `json:"display_identifier,omitempty"`
	Attempt    int    `json:"attempt"`
	DueAtMS    int64  `json:"due_at_ms"`
	Error      string `json:"error"`
}

// SnapshotBudgetEntry is a read-only, value-copied view of one issue held
// out of dispatch by a budget ceiling, produced by [RuntimeSnapshot].
type SnapshotBudgetEntry struct {
	IssueID            string    `json:"issue_id"`
	Identifier         string    `json:"issue_identifier"`
	DisplayID          string    `json:"display_identifier,omitempty"`
	Reason             string    `json:"reason"`
	UsedSessions       int       `json:"used_sessions"`
	BudgetSessions     int       `json:"budget_sessions"`
	UsedTokens         *int64    `json:"used_tokens"`
	BudgetTokens       int64     `json:"budget_tokens"`
	UnmeasuredSessions *int      `json:"unmeasured_sessions"`
	ExhaustedAt        time.Time `json:"exhausted_at"`
}

// SnapshotAgentTotals holds aggregate token counts and runtime seconds at a
// point in time. Unlike [AgentTotals], SecondsRunning includes active
// sessions' elapsed time.
//
// UnmeasuredSessions, RunningUnreported, and RunningNonReporting count, by
// reason, the sessions the token counters leave out: ended sessions with no
// recorded usage, running sessions whose figure has not arrived and may
// still, and running sessions reporting no usage.
type SnapshotAgentTotals struct {
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	TotalTokens         int64   `json:"total_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	SecondsRunning      float64 `json:"seconds_running"`
	UnmeasuredSessions  int64   `json:"unmeasured_sessions"`
	RunningUnreported   int     `json:"running_unreported"`
	RunningNonReporting int     `json:"running_non_reporting"`
}

// RuntimeSnapshotResult is a point-in-time capture of the orchestrator's
// runtime state, produced by [RuntimeSnapshot].
type RuntimeSnapshotResult struct {
	GeneratedAt          time.Time              `json:"generated_at"`
	Running              []SnapshotRunningEntry `json:"running"`
	Retrying             []SnapshotRetryEntry   `json:"retrying"`
	AgentTotals          SnapshotAgentTotals    `json:"agent_totals"`
	RateLimits           map[string]any         `json:"rate_limits"`
	BudgetExhaustedCount int                    `json:"budget_exhausted_count"`
	BudgetExhausted      []SnapshotBudgetEntry  `json:"budget_exhausted"`
	ParkedCount          int                    `json:"parked_count"`
	Parked               []string               `json:"parked,omitempty"`
	ParkedReason         map[string]string      `json:"parked_reason,omitempty"`
}

// ActiveElapsedSeconds sums wall-clock elapsed seconds across all running
// sessions at now. Zero-StartedAt entries are skipped; negative elapsed is
// clamped to zero against clock skew.
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

// apiRequestsMeasured reports whether a session's API request count is a
// measurement of model API requests. Only an arrival reporting during the
// turn can produce one, and only once a figure has arrived or while no turn
// has begun: a session past its first turn with nothing counted measured
// nothing.
func apiRequestsMeasured(arrival registry.UsageArrival, turnCount, apiRequestCount int) bool {
	if !arrival.ReportsDuringTurn() {
		return false
	}
	if apiRequestCount > 0 {
		return true
	}
	return turnCount == 0
}

// RuntimeSnapshot captures a point-in-time view of the orchestrator's
// runtime state. now is normalized to UTC internally.
//
// Running, Retrying, and AgentTotals are copied out and may be retained
// without synchronization. RateLimits is shallow-copied and may alias
// nested mutable values; callers must treat it and its referents as
// read-only.
//
// Must be called from the event loop goroutine; [State] is not safe for
// concurrent access.
func RuntimeSnapshot(state *State, now time.Time) RuntimeSnapshotResult {
	now = now.UTC()
	snap := RuntimeSnapshotResult{
		GeneratedAt: now,
		Running:     make([]SnapshotRunningEntry, 0, len(state.Running)),
		Retrying:    make([]SnapshotRetryEntry, 0, len(state.RetryAttempts)),
	}

	var activeElapsedTotal float64
	var runningUnreported, runningNonReporting int
	for _, entry := range state.Running {
		requestsMeasured := apiRequestsMeasured(entry.UsageArrival, entry.TurnCount, entry.APIRequestCount)

		switch {
		case entry.UsageArrival == registry.UsageArrivalNone:
			runningNonReporting++
		case entry.UsageArrival.ReportsAnyFigure() && !entry.UsageMeasured:
			runningUnreported++
		}

		// Absence is the only way to say the breakdown means nothing;
		// gating it here states the rule once.
		var modelRequests map[string]int
		if requestsMeasured && entry.UsageAttribution.NamesModel() && entry.RequestsByModel != nil {
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
			APIRequestsMeasured: requestsMeasured,
			WorkspacePath:       entry.WorkspacePath,
			SSHHost:             entry.SSHHost,
			ToolTimeMs:          entry.ToolTimeMs,
			APITimeMs:           entry.APITimeMs,
			WorkflowFile:        entry.WorkflowFile,
			SelfReviewActive:    entry.SelfReviewActive,
			SelfReviewIteration: entry.SelfReviewIteration,
			AgentKind:           entry.AgentKind,
			RuleName:            entry.RuleName,
			UsageMeasured:       entry.UsageMeasured,
			UsageArrival:        entry.UsageArrival,
			UsageAttribution:    entry.UsageAttribution,
			TokensPending: entry.UsageArrival == registry.UsageArrivalTurnEnd &&
				entry.UsageMeasured && !isTurnTerminalEvent(entry.LastAgentEvent),
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
		InputTokens:         state.AgentTotals.InputTokens,
		OutputTokens:        state.AgentTotals.OutputTokens,
		TotalTokens:         state.AgentTotals.TotalTokens,
		CacheReadTokens:     state.AgentTotals.CacheReadTokens,
		SecondsRunning:      state.AgentTotals.SecondsRunning + activeElapsedTotal,
		UnmeasuredSessions:  state.AgentTotals.UnmeasuredSessions,
		RunningUnreported:   runningUnreported,
		RunningNonReporting: runningNonReporting,
	}

	snap.BudgetExhaustedCount = len(state.BudgetExhausted)
	budgetExhausted := make([]SnapshotBudgetEntry, 0, len(state.BudgetExhausted))
	for id, entry := range state.BudgetExhausted {
		budgetExhausted = append(budgetExhausted, SnapshotBudgetEntry{
			IssueID:            id,
			Identifier:         entry.Identifier,
			DisplayID:          entry.DisplayID,
			Reason:             entry.Reason,
			UsedSessions:       entry.UsedSessions,
			BudgetSessions:     entry.BudgetSessions,
			UsedTokens:         entry.UsedTokens,
			BudgetTokens:       entry.BudgetTokens,
			UnmeasuredSessions: entry.UnmeasuredSessions,
			ExhaustedAt:        entry.ExhaustedAt,
		})
	}
	slices.SortFunc(budgetExhausted, func(a, b SnapshotBudgetEntry) int {
		return cmp.Or(cmp.Compare(a.Identifier, b.Identifier), cmp.Compare(a.IssueID, b.IssueID))
	})
	snap.BudgetExhausted = budgetExhausted

	snap.ParkedCount = len(state.Parked)
	if len(state.Parked) > 0 {
		ids := make([]string, 0, len(state.Parked))
		for id := range state.Parked {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		snap.Parked = ids

		reasons := make(map[string]string, len(state.Parked))
		for _, id := range ids {
			reasons[id] = state.Parked[id].Reason
		}
		snap.ParkedReason = reasons
	}

	if state.AgentRateLimits != nil {
		snap.RateLimits = maps.Clone(state.AgentRateLimits.Data)
	}

	return snap
}

// BuildReviewReactionConfig extracts and validates review-specific
// configuration from a [config.ReactionConfig].
func BuildReviewReactionConfig(rc config.ReactionConfig) (ReviewReactionConfig, error) {
	cfg := ReviewReactionConfig{
		Escalation:           rc.Escalation,
		EscalationLabel:      rc.EscalationLabel,
		PollIntervalMS:       120000,
		DebounceMS:           60000,
		MaxContinuationTurns: 3,
		WatchWindowMS:        reactionWatchWindowDefaultMS,
		Triage:               rc.Triage,
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

	window, err := watchWindowMS(rc.Extra, cfg.WatchWindowMS)
	if err != nil {
		return ReviewReactionConfig{}, err
	}
	cfg.WatchWindowMS = window

	return cfg, nil
}

// BuildBotReviewReactionConfig extracts and validates bot-review-specific
// configuration from a [config.ReactionConfig].
//
// Defaults differ from [BuildReviewReactionConfig]: bot comments arrive in
// bulk on push and dispatch immediately, so the poll cadence is tighter,
// the retry budget larger, and no debounce is read.
func BuildBotReviewReactionConfig(rc config.ReactionConfig) (BotReviewReactionConfig, error) {
	cfg := BotReviewReactionConfig{
		Escalation:           rc.Escalation,
		EscalationLabel:      rc.EscalationLabel,
		PollIntervalMS:       60000,
		MaxContinuationTurns: 5,
		WatchWindowMS:        reactionWatchWindowDefaultMS,
		Triage:               rc.Triage,
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

	window, err := watchWindowMS(rc.Extra, cfg.WatchWindowMS)
	if err != nil {
		return BotReviewReactionConfig{}, err
	}
	cfg.WatchWindowMS = window

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

// BuildAutoMergeReactionConfig extracts and validates auto-merge-specific
// configuration from a [config.ReactionConfig].
func BuildAutoMergeReactionConfig(rc config.ReactionConfig) (AutoMergeReactionConfig, error) {
	cfg := AutoMergeReactionConfig{
		Strategy:        domain.StrategySquash,
		RequireCI:       true,
		DeleteBranch:    true,
		PollIntervalMS:  60000,
		Escalation:      rc.Escalation,
		EscalationLabel: rc.EscalationLabel,
		MaxRetries:      rc.MaxRetries,
		WatchWindowMS:   reactionWatchWindowDefaultMS,
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

	window, err := watchWindowMS(rc.Extra, cfg.WatchWindowMS)
	if err != nil {
		return AutoMergeReactionConfig{}, err
	}
	cfg.WatchWindowMS = window

	return cfg, nil
}

// BuildMergeConflictReactionConfig extracts and validates
// merge-conflict-specific configuration from a [config.ReactionConfig].
// rc.MaxRetries is consumed verbatim; the per-kind default is applied
// earlier in config parsing.
func BuildMergeConflictReactionConfig(rc config.ReactionConfig) (MergeConflictReactionConfig, error) {
	cfg := MergeConflictReactionConfig{
		Escalation:      rc.Escalation,
		EscalationLabel: rc.EscalationLabel,
		PollIntervalMS:  60000,
		MaxRetries:      rc.MaxRetries,
		WatchWindowMS:   reactionWatchWindowDefaultMS,
		Triage:          rc.Triage,
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

	window, err := watchWindowMS(rc.Extra, cfg.WatchWindowMS)
	if err != nil {
		return MergeConflictReactionConfig{}, err
	}
	cfg.WatchWindowMS = window

	return cfg, nil
}

// BuildLabelReviewReactionConfig copies the validated label-review runtime
// configuration out of the parsed label_commands block. It returns no
// error: config parsing already validated and defaulted the input.
func BuildLabelReviewReactionConfig(cfg config.LabelCommandsConfig) LabelReviewReactionConfig {
	return LabelReviewReactionConfig{
		Provider:       cfg.Provider,
		ReviewLabel:    cfg.ReviewLabel,
		PollIntervalMS: cfg.PollIntervalMS,
	}
}

// BuildLabelFixReactionConfig copies the validated label-fix runtime
// configuration out of the parsed label_commands block. It returns no
// error: config parsing already validated and defaulted the input.
func BuildLabelFixReactionConfig(cfg config.LabelCommandsConfig) LabelFixReactionConfig {
	return LabelFixReactionConfig{
		Provider:       cfg.Provider,
		FixLabel:       cfg.FixLabel,
		PollIntervalMS: cfg.PollIntervalMS,
	}
}

// BuildMergeCompletionReactionConfig extracts and validates
// merge-completion-specific configuration from a [config.ReactionConfig].
//
// meta resolves the default active-state list only; the terminal list is
// read from tracker as written, with no fallback to
// [registry.TrackerMeta.DefaultTerminalStates], so the offline validator
// and the runtime terminal drop agree on what "terminal" means.
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
		parsed, ok := config.IntFromNumber(n)
		if !ok {
			return 0, config.ErrIntegerOutOfRange
		}
		return parsed, nil
	case int64, uint64:
		parsed, ok := config.IntFromNumber(n)
		if !ok {
			return 0, config.ErrIntegerOutOfRange
		}
		return parsed, nil
	default:
		return 0, fmt.Errorf("expected numeric value, got %T", v)
	}
}
