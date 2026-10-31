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

	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/pricing"
	"github.com/sortie-ai/sortie/internal/prompt"
	"github.com/sortie-ai/sortie/internal/redact"
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
	triggerUsageLimit   = "usage_limit"
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
// Money budget takes precedence over the token budget, which takes
// precedence over the session budget.
const (
	budgetReasonMoney   = domain.BudgetHoldReasonMoney
	budgetReasonToken   = domain.BudgetHoldReasonToken
	budgetReasonSession = domain.BudgetHoldReasonSession
)

// knownBudgetReasons lists every dispatch-gate budget reason in the order
// the gauge reports them. A reason added above must be added here too, so
// the gauge recompute reports a value for it on every pass.
var knownBudgetReasons = []string{budgetReasonSession, budgetReasonToken, budgetReasonMoney}

// AgentTotals holds cumulative token and runtime counters across all ended
// agent sessions, persisted to SQLite and restored on startup.
// SecondsRunning tracks only ended-session time; the snapshot adds active
// sessions' elapsed time.
type AgentTotals struct {
	InputTokens      int64
	OutputTokens     int64
	TotalTokens      int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	SecondsRunning   float64

	// UnmeasuredSessions counts ended sessions whose usage was never
	// recorded, cumulative across restarts. The counters above exclude
	// these sessions.
	UnmeasuredSessions int64
}

// RunningEntry tracks a single in-flight agent session. Created by dispatch
// and removed on worker exit or reconciliation termination.
type RunningEntry struct {
	// Identifier is the human-readable ticket key (e.g. "MT-649").
	Identifier string

	// Issue is the last-known normalized issue snapshot. After dispatch,
	// State and Labels change only through [recordIssueRead].
	Issue domain.Issue

	// DispatchLabels is the dispatch read's labels, set by [DispatchIssue]
	// and never reassigned. A stage hop takes its stage labels from here
	// alone, so a label added during the run is never removed by a hop.
	DispatchLabels []string

	// IssueReadAt is the instant the read held in Issue.State and
	// Issue.Labels was started; zero while they hold the dispatch read.
	IssueReadAt time.Time

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

	CacheWriteTokens int64

	LastReportedCacheWriteTokens int64

	// UsageByModel is the run's per-model token totals, which sum to the
	// run's token counters in every class. Written only by applyUsageDelta.
	UsageByModel map[string]domain.TokenUsage

	// ReportedAmounts is the run's amounts the agent reported, each in its
	// own unit, sorted by unit then model. Written only by [HandleAgentEvent]
	// and never added to a token or money total outside the pricing fallback.
	ReportedAmounts []domain.ReportedAmount

	ModelName string

	// APIRequestCount is the number of token_usage events received.
	// Whether that counts as model API requests is resolved separately by
	// apiRequestsMeasured.
	APIRequestCount int

	// RequestsByModel maps model name to token_usage event count. Nil
	// until the first token_usage event carrying a model name arrives.
	RequestsByModel map[string]int

	// Skills holds every skill call of the run in completion order. Only the
	// event loop appends to it.
	Skills []domain.SkillCall

	// RetryAttempt is nil for first dispatch, non-nil and >= 1 for retries
	// and continuations.
	RetryAttempt *int

	StartedAt time.Time

	// TurnCount is the number of coding-agent turns started this worker
	// lifetime, self-review turns included.
	TurnCount int

	// CancelFunc cancels the run context, cascading to the worker context
	// nested under it. Nil only in test fixtures that bypass [DispatchIssue].
	CancelFunc context.CancelFunc

	// CancelCauseFunc cancels the same run context as CancelFunc and records
	// the cause the worker reads back. Nil only in test fixtures that bypass
	// [DispatchIssue].
	CancelCauseFunc context.CancelCauseFunc

	// CeilingCancelFunc cancels the worker context with cause
	// [errCeilingStop]. Nil only in test fixtures that bypass [DispatchIssue].
	CeilingCancelFunc context.CancelFunc

	// PendingCleanup is set by reconciliation on a terminal-state
	// observation; [HandleWorkerExit] performs the cleanup after the
	// worker goroutine exits, deferring it until the agent process has
	// terminated to avoid races with active writes.
	PendingCleanup bool

	// ObservedTerminalState is the terminal state reconciliation observed
	// while the worker ran. Set beside PendingCleanup and never cleared: a
	// terminal observation is final for the entry's lifetime.
	ObservedTerminalState string

	// WorkspacePath is the absolute workspace directory, populated when the
	// worker reports it prepared, or in [HandleWorkerExit] before cleanup
	// when that report never arrived. The PendingCleanup path cleans
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

	// Trigger is the OriginTrigger of the retry whose fire dispatched the
	// entry; empty for a poll-tick dispatch. Runtime-only.
	Trigger string

	// UsageLimitNotified is the UsageLimitNotified of the retry whose fire
	// dispatched the entry; false for a poll-tick dispatch. Runtime-only.
	UsageLimitNotified bool

	// Continuation names the pull request and try of a reaction
	// continuation; the zero value for every other dispatch. Runtime-only.
	Continuation ContinuationTarget

	// SelfReviewActive is true during the self-review phase. Mutated only
	// by the event loop via selfReviewCh.
	SelfReviewActive bool

	SelfReviewIteration int

	// AgentKind is the adapter kind (e.g. "claude-code") from the config
	// active at dispatch.
	AgentKind string

	// RuleName is the dispatch rule the run was dispatched under. Empty
	// when the workflow-wide fallback fired, "default" when the default
	// block matched.
	RuleName string

	// StageLineage is the frozen stage.previous pair and chain this run
	// was dispatched with; its ChainID is never empty.
	StageLineage StageLineage

	// FirstAfterHop reports that the poll tick dispatched this run as the
	// first run after the issue's stage hop. A retry never sets it.
	// Runtime-only.
	FirstAfterHop bool

	// RuleSettingsApplied reports that the rule's settings block applied
	// to this attempt.
	RuleSettingsApplied bool

	// ConfiguredModel and ConfiguredEffort are what the operator asked
	// for, empty when unset; ModelName is what the runtime reported.
	ConfiguredModel  string
	ConfiguredEffort string

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

	// CarriesTokenFigure is whether this session's kind can carry a token
	// figure on its launch, frozen at dispatch and on retry.
	CarriesTokenFigure bool

	// APIRequestCountAtLastTurnEnd is APIRequestCount as of the most recent
	// turn-terminal event, zero until the first turn ends.
	APIRequestCountAtLastTurnEnd int

	// IssueTokensCompleted is the issue's uncached tokens summed from
	// run_history across completed sessions, read at dispatch and replaced
	// by a confirming read. 0 when the dispatch read failed.
	IssueTokensCompleted int64

	// IssueUsage is the issue's completed-session usage as of the last
	// successful read on this entry, written on the event loop at dispatch
	// and by a confirming read. Nil while no read has succeeded.
	IssueUsage *persistence.IssueTokenUsage

	// UnaccountedTurns is the running session's count of turns that spent
	// tokens no figure was proven to cover, as its worker last relayed it.
	// Written only on the event loop.
	UnaccountedTurns int

	// CeilingStopRequest is set once the ceiling decides to stop this
	// run. Non-nil alone does not mean the ceiling ended it; decided at exit.
	CeilingStopRequest *CeilingStopRequest

	// TokenCeilingQueryWarned latches once the ceiling's confirming read
	// has failed and been reported for this run.
	TokenCeilingQueryWarned bool

	// MoneyCeilingQueryWarned latches once the money ceiling's confirming
	// read has failed and been reported for this run.
	MoneyCeilingQueryWarned bool

	// WorkerDone is the Done() channel of the worker context
	// [DispatchIssue] creates. Nil only in test fixtures that bypass
	// DispatchIssue, where a non-blocking receive reads it as open.
	WorkerDone <-chan struct{}

	// TokenWarningReached latches once the token warning record has
	// been emitted for this run. Set once, never cleared.
	TokenWarningReached bool

	// MetadataWritePending is true when an incremental session_metadata
	// write is owed ahead of [sessionMetadataWriteInterval], set by the
	// token warning reaching its threshold and cleared once that write
	// succeeds.
	MetadataWritePending bool

	// NoStateObserved is true after a reconcile read the issue in no state
	// and no later reconcile read it in a state. Set and cleared only by
	// [reconcileTrackerState].
	NoStateObserved bool

	// events is the run's retained agent events, registered by
	// [DispatchIssue]. Nil when [State.RetainedEvents] is nil.
	events *runEventBuffer
}

// RetryEntry holds the runtime state for a pending retry. IssueID,
// Identifier, DisplayID, IssueIdentity, Attempt, DueAtMS, and Error map to
// persistence.RetryEntry;
// the rest is runtime-only and reconstructed on startup from persisted
// due_at timestamps.
type RetryEntry struct {
	IssueID       string
	Identifier    string
	DisplayID     string
	IssueIdentity IssueIdentity
	SessionID     string
	Attempt       int
	DueAtMS       int64
	Error         string
	TimerHandle   *time.Timer

	// LastSSHHost is the SSH host from the previous attempt, passed as the
	// preferred host to [HostPool.AcquireHost]. Runtime-only.
	LastSSHHost string

	// noStateObserved is true when the fire that scheduled this entry read
	// the issue in no state. Runtime-only.
	noStateObserved bool

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

	// Trigger is the sortie_retries_total trigger label the schedule that
	// created the entry recorded; empty when that schedule records none.
	Trigger string

	// OriginTrigger is what started the retry, kept through every
	// put-back by the retry timer; empty when the retry never recorded it.
	OriginTrigger string

	// UsageLimitNotified reports that the run this retry follows ended in a
	// provider usage-limit refusal, so the next run publishes no session.started
	// event and, if refused, no session.failed event.
	UsageLimitNotified bool

	// RuleName is the dispatch rule of the run this retry follows.
	RuleName string

	// StageLineage is the frozen stage lineage of the run this retry
	// follows.
	StageLineage StageLineage

	// RuleSettingsApplied reports that the run this retry follows ran
	// with its rule's settings block.
	RuleSettingsApplied bool

	// TemplateID is the resolved template registry key of the run this
	// retry follows.
	TemplateID string

	// AgentKind is the adapter kind of the run this retry follows.
	AgentKind string

	// ConfiguredModel is the model the run this retry follows was configured
	// with; empty when unset.
	ConfiguredModel string

	// Continuation names the pull request and try of the reaction
	// continuation this retry dispatches; the zero value for every other
	// retry. Runtime-only.
	Continuation ContinuationTarget

	// pausedSinceMS is the wall-clock ms at which this entry first took one
	// of HandleRetryTimer's paused-by-issue-state arms. Zero means not
	// paused. Runtime-only: never persisted, never on ScheduleRetryParams.
	pausedSinceMS int64

	// holdRearmed is true when HandleRetryTimer re-armed this entry because a
	// spend ceiling or a usage-limit hold held it, so a later slot or host
	// shortage keeps Attempt. Runtime-only.
	holdRearmed bool

	// dueBeforeRequestMS is the DueAtMS the entry had before the first request
	// that made it due now; zero when no request did. Runtime-only.
	dueBeforeRequestMS int64
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

// attemptCounting classifies whether a kind's pass counts tries in
// [State.ReactionAttempts]. It is a named type rather than a bool so a
// registry literal cannot swap it with pinsWorkspace and still compile.
type attemptCounting bool

const (
	countsAttempts   attemptCounting = true
	countsNoAttempts attemptCounting = false
)

// escalation classifies whether a kind's pass escalates to a person. It is
// a named type for the same reason as [attemptCounting].
type escalation bool

const (
	escalates      escalation = true
	neverEscalates escalation = false
)

// reactionKindSpec classifies one registered reaction kind.
type reactionKindSpec struct {
	configKey     string          // key of the reactions section of WORKFLOW.md the kind is configured under
	pinsWorkspace bool            // a pending entry of the kind excludes its workspace from sweep candidacy
	attempts      attemptCounting // the kind's pass counts tries in State.ReactionAttempts
	escalates     escalation      // the kind's pass escalates and keeps a durable escalation record
}

// reactionKinds is the single registry of reaction kinds. Presence in the
// map means the kind is known. Every entry is an unkeyed literal so the
// compiler rejects a kind registered without its configuration key, its
// pin classification, its attempt counting, or its escalation class.
var reactionKinds = map[string]reactionKindSpec{
	ReactionKindCI:              {"ci_failure", true, countsAttempts, escalates},
	ReactionKindReview:          {"review_comments", true, countsAttempts, escalates},
	ReactionKindBotReview:       {"bot_review", true, countsAttempts, escalates},
	ReactionKindAutoMerge:       {"auto_merge", true, countsAttempts, escalates},
	ReactionKindMergeConflict:   {"merge_conflicts", true, countsAttempts, escalates},
	ReactionKindLabelReview:     {"label_commands", false, countsNoAttempts, neverEscalates},
	ReactionKindLabelFix:        {"label_commands", false, countsNoAttempts, neverEscalates},
	ReactionKindMergeCompletion: {"merge_completion", false, countsAttempts, escalates},
}

// EscalatingReactionKinds returns the registered reaction kinds whose pass
// escalates, sorted.
func EscalatingReactionKinds() []string {
	kinds := make([]string, 0, len(reactionKinds))
	for kind, spec := range reactionKinds {
		if spec.escalates == escalates {
			kinds = append(kinds, kind)
		}
	}
	slices.Sort(kinds)
	return kinds
}

// readKeepsRun reports whether a tracker read of state leaves the run
// going: an active state, or the handoff state for a known reaction kind.
// Both reconcile and the post-transition read gate on it, so a read that
// would stop the run is never recorded.
func readKeepsRun(entry *RunningEntry, state string, activeStates []string, handoffState string) bool {
	if state == "" {
		return false
	}
	if isActiveState(state, activeStates) {
		return true
	}
	return handoffState != "" && strings.EqualFold(state, handoffState) && isKnownReactionKind(entry.ReactionKind)
}

// recordIssueRead stores read as the entry's state and labels unless an
// equal or newer read is already recorded. Both instants must be unconverted
// [time.Now] values: .UTC() strips the monotonic reading that makes the
// comparison immune to wall-clock steps.
func recordIssueRead(entry *RunningEntry, read domain.IssueStateAndLabels, startedAt time.Time) {
	if !startedAt.After(entry.IssueReadAt) {
		return
	}
	entry.Issue.State = read.State
	entry.Issue.Labels = read.Labels
	entry.IssueReadAt = startedAt
}

func isKnownReactionKind(kind string) bool {
	_, known := reactionKinds[kind]
	return known
}

// reactionKindPinsWorkspace reports whether a pending reaction entry of
// the given kind excludes its workspace from sweep candidacy. An
// unregistered kind returns true, the non-destructive default: retention
// is safe for a kind the rest of the system does not yet classify.
func reactionKindPinsWorkspace(kind string) bool {
	spec, known := reactionKinds[kind]
	if !known {
		return true
	}
	return spec.pinsWorkspace
}

// ReactionConfigKey returns the reactions key kind is configured under, or
// "" for a kind outside the registry, the empty kind included.
func ReactionConfigKey(kind string) string {
	return reactionKinds[kind].configKey
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

	// IssueIdentity is the issue as the run that created the reaction read
	// it, with the handoff target as State when that run's exit moved the
	// issue to it. Restored from run history it holds the title and link
	// alone, with nil Labels.
	IssueIdentity IssueIdentity

	// Attempt is the overall run attempt number from the completed worker.
	Attempt int

	// Kind is the reaction type constant (e.g. ReactionKindCI).
	Kind string

	// LastSSHHost is the SSH host from the completed worker, used for host
	// preference on fix redispatch.
	LastSSHHost string

	// CreatedAt is when the worker exit or startup recovery created the
	// entry; never rewritten.
	CreatedAt time.Time

	// PendingAttempts counts consecutive passes that reached no dispatch,
	// used for exponential backoff.
	PendingAttempts int

	// PendingRetryAt is the earliest time to poll again. Zero means ready
	// immediately.
	PendingRetryAt time.Time

	// dueBeforeRequest is the PendingRetryAt the entry had before a request
	// made its check due now; zero when no request did. Runtime-only.
	dueBeforeRequest time.Time

	// attemptsBeforeRequest is the PendingAttempts the entry had then; a new
	// pull request head zeroes it. Runtime-only.
	attemptsBeforeRequest int

	// HeadRecordedAt is when this process recorded the commit currently
	// evaluated. Zero until first recorded, and zero for a startup-restored
	// entry, because a head recorded by a previous process cannot bound a
	// query over this process's activity. The exception is a restored
	// standing escalation of a kind whose subject is the head: it takes the
	// escalation time, which no run of the previous process can precede.
	// Read only by kinds whose subject is the pull request head.
	HeadRecordedAt time.Time

	// EscalatedAt is when the standing escalation began; zero while none
	// stands.
	EscalatedAt time.Time

	// EscalationTrigger is why the standing escalation began; empty while
	// none stands.
	EscalationTrigger ReactionEscalationTrigger

	// EscalationSubject is what the standing escalation was about, in the
	// kind's own terms; empty while none stands or when the escalation has
	// no subject to latch on.
	EscalationSubject string

	// NoStateObserved reports that a tracker reading found the issue in no
	// state while an escalation stands. Two consecutive readings end it.
	NoStateObserved bool

	// resumeAfterRestart marks a merge-completion entry restored at startup
	// with a standing escalation: its first due pass runs the normal path
	// once.
	resumeAfterRestart bool

	// escalationUnsaved marks a standing escalation whose record could not
	// be saved; each later due pass of the entry saves it again.
	escalationUnsaved bool

	// WatchRestartedAt is when a retry-slot deferral last restarted the
	// entry's watch window; zero when none has.
	WatchRestartedAt time.Time

	// PullRequestURL is the pull request's web page as the latest read of
	// it reported; empty until a read reports one.
	PullRequestURL string

	// Wait is why the entry waits for its next check, as the pass that last
	// returned it to the pending set recorded it; empty until one has.
	Wait string

	// LastCheck is what the latest completed check observed; nil until one
	// completes, and always nil for kinds that record no result.
	LastCheck *ReactionCheck

	// KindData holds kind-specific typed data (CI uses [*CIReactionData]).
	// Each kind's reconcile function asserts the type once.
	KindData any

	// AgentKind is the adapter kind of the completed worker.
	AgentKind string

	// RuleName is the rule name of the completed worker.
	RuleName string

	// StageLineage is the frozen stage lineage of the completed worker.
	StageLineage StageLineage

	// RuleSettingsApplied reports that the completed worker ran with its
	// rule's settings block.
	RuleSettingsApplied bool

	// TemplateID is the template key of the completed worker.
	TemplateID string

	// ConfiguredModel is the model the completed worker was configured with;
	// empty when unset.
	ConfiguredModel string

	// Triage is the in-flight or finished triage run for the current
	// subject. Nil when none started, cleared when the fingerprint moves.
	// Runtime-only.
	Triage *ReactionTriageRun
}

// ReactionCheck is what one completed check of a pending reaction observed.
// Exactly one of CI, Comments, and Mergeability is set.
type ReactionCheck struct {
	ObservedAt   time.Time
	HeadSHA      string // the head commit the check read; ci and merge-conflict only
	CI           *CICheck
	Comments     *CommentCheck
	Mergeability *MergeabilityCheck
}

// CICheck is a CI verdict and its check runs by outcome.
type CICheck struct {
	Status        string   // the verdict the reaction acted on
	FailingChecks []string // names of the failing runs in the provider's order; never nil
	Failing       int
	Pending       int
	Passing       int
}

// CommentCheck counts the comments a review check found actionable and, of
// those, the ones the reaction acts on next.
type CommentCheck struct {
	Actionable int
	New        int
	// Handled reports that the newest triage answer recorded for this
	// comment set is handled, so no run is given it.
	Handled bool
	// AwaitingApproval reports that no comment is actionable and the review
	// decision says an approval rule is unmet. Only the review pass sets it.
	AwaitingApproval bool
}

// awaits reports whether check records a pull request waiting on a required
// approval.
func awaits(check *ReactionCheck) bool {
	return check != nil && check.Comments != nil && check.Comments.AwaitingApproval
}

// MergeabilityCheck reports whether the pull request merges without
// conflicts; a nil Mergeable means the provider had not computed it.
type MergeabilityCheck struct{ Mergeable *bool }

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
	Owner    string
	Repo     string
	Branch   string
	SHA      string
}

// ReviewReactionData holds review-specific fields for a pending review
// reaction, stored in [PendingReaction.KindData] for Kind ==
// [ReactionKindReview]. Owner and Repo come from [domain.SCMMetadata]
// (scm.json), never from tracker project config.
type ReviewReactionData struct {
	PRNumber int
	Owner    string
	Repo     string
	Branch   string
	SHA      string

	// LastEventAt is the timestamp of the most recently detected review
	// comment, used for debounce gating.
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
	Owner    string
	Repo     string
	Branch   string
	SHA      string
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
	Owner    string
	Repo     string
	Branch   string
	SHA      string
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
	Owner    string
	Repo     string
	Branch   string
	SHA      string
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
	Owner    string
	Repo     string

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
	Owner    string
	Repo     string
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
	IssueIdentity      IssueIdentity
	Reason             string   // budgetReasonSession, budgetReasonToken, or budgetReasonMoney
	UsedSessions       int      // completed sessions counted from run history
	BudgetSessions     int      // configured agent.max_sessions; 0 means unlimited
	UsedTokens         *int64   // uncached tokens; nil when no ceiling read this issue's usage
	BudgetTokens       int64    // configured agent.max_tokens; 0 means unlimited
	UnmeasuredSessions *int     // nil exactly when UsedTokens is nil; else runs whose spend is unknown
	StoppedInFlight    *int     // nil exactly when UsedTokens is nil; else sessions a budget ceiling stopped mid-run
	UnaccountedTurns   int      // taken from the read that set UsedTokens; 0 while UsedTokens is nil
	UsedUSD            *float64 // estimated cost in USD; nil when the money axis did not read this issue
	UsedUSDComplete    *bool    // nil exactly when UsedUSD is nil
	BudgetUSD          float64  // configured agent.max_cost_usd; 0 means unlimited
	ExhaustedAt        time.Time

	// RuleName, StageLineage, AgentKind, and ConfiguredModel describe the
	// dispatch the hold blocks. The record holds a dispatch context exactly
	// when AgentKind is non-empty.
	RuleName        string
	StageLineage    StageLineage
	AgentKind       string
	ConfiguredModel string
}

// setTokenUsage fills the token-axis fields from one read of the issue's
// run history. It copies the counts, so the entry never aliases the
// caller's usage value.
func (e *BudgetExhaustedEntry) setTokenUsage(usage persistence.IssueTokenUsage) {
	used, unmeasured, stopped := usage.UncachedTokens, usage.UnmeasuredSessions, usage.StoppedInFlight
	e.UsedTokens = &used
	e.UnmeasuredSessions = &unmeasured
	e.StoppedInFlight = &stopped
	e.UnaccountedTurns = usage.UnaccountedTurns
}

// clonePointer returns a pointer to a copy of *p, or nil for a nil p.
func clonePointer[T any](p *T) *T {
	if p == nil {
		return nil
	}
	value := *p
	return &value
}

// SpendIncomplete reports whether a token sum carrying the given counts
// leaves spend out, making the sum a lower bound.
func SpendIncomplete(unmeasuredSessions, unaccountedTurns int) bool {
	return persistence.IssueTokenUsage{
		UnmeasuredSessions: unmeasuredSessions,
		UnaccountedTurns:   unaccountedTurns,
	}.SpendIncomplete()
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
	Identifier string
	DisplayID  string
	// IssueIdentity is the issue's title, link, issue type, and labels as
	// read for the park; the zero value holds no read. Its State is never
	// served: ParkedState is.
	IssueIdentity IssueIdentity
	Reason        string // "agent_blocked" or "handoff_absence"
	ParkedState   string // tracker state observed at park time; empty when unobserved
	Label         string // parking label the orchestrator applied
	LabelApplied  bool
	ParkedAt      time.Time

	// RuleName, StageLineage, AgentKind, and ConfiguredModel describe the
	// run the park follows. The park records a run exactly when AgentKind
	// is non-empty.
	RuleName        string
	StageLineage    StageLineage
	AgentKind       string
	ConfiguredModel string

	// SessionID, TemplateID, and Statement are what the park hands to the
	// first run after a release, as parkHandOverOf derives them from the
	// run the park follows. TemplateID is empty for the workflow body
	// template.
	SessionID  string
	TemplateID string
	Statement  string

	// ReleasedAt and ReleaseGesture record a person's release; both are
	// zero while the park holds.
	ReleasedAt     time.Time
	ReleaseGesture string // "label_removed" or "state_changed"
}

// IssueIdentity names an issue to a person beside its key: the tracker's
// title, web link, issue type, labels, and state as the orchestrator read
// them. The zero value holds no read of the issue. Labels is nil while the
// labels are unread and an empty non-nil slice when a read found none.
type IssueIdentity struct {
	Title     string
	URL       string
	IssueType string
	Labels    []string
	State     string
}

// issueIdentityOf returns the identity issue carries, unmasked. Labels is
// a new non-nil slice.
func issueIdentityOf(issue domain.Issue) IssueIdentity {
	labels := make([]string, len(issue.Labels))
	copy(labels, issue.Labels)
	return IssueIdentity{
		Title:     issue.Title,
		URL:       issue.URL,
		IssueType: issue.IssueType,
		Labels:    labels,
		State:     issue.State,
	}
}

// isZero reports whether id holds no read of its issue.
func (id IssueIdentity) isZero() bool {
	return id.Title == "" && id.URL == "" && id.IssueType == "" && id.Labels == nil && id.State == ""
}

// holdsPartialRead reports whether id names its issue by title or link but
// never read its labels: an identity restored from a retry row or a run
// history row written before those facts were kept.
func (id IssueIdentity) holdsPartialRead() bool {
	return (id.Title != "" || id.URL != "") && id.Labels == nil
}

// filledFrom returns id completed by read. A zero id becomes read; a
// partial read takes the type, labels, and state of read and keeps its
// title and link, so a held title or link is never replaced; any other id
// is returned unchanged.
func (id IssueIdentity) filledFrom(read IssueIdentity) IssueIdentity {
	switch {
	case id.isZero():
		return read
	case id.holdsPartialRead():
		id.IssueType = read.IssueType
		id.Labels = read.Labels
		id.State = read.State
	}
	return id
}

// maskedIdentity returns id with every secret registered for redaction
// replaced by its marker. A nil Labels stays nil.
func maskedIdentity(id IssueIdentity) IssueIdentity {
	masked := IssueIdentity{
		Title:     redact.Mask(id.Title),
		URL:       redact.Mask(id.URL),
		IssueType: redact.Mask(id.IssueType),
		State:     redact.Mask(id.State),
	}
	if id.Labels != nil {
		masked.Labels = maskedLabels(id.Labels)
	}
	return masked
}

// maskedLabels returns a new slice of labels with every registered secret
// replaced by its marker; it is empty and non-nil when labels is. State
// keeps the unmasked labels because stage advancement reads them.
func maskedLabels(labels []string) []string {
	masked := make([]string, len(labels))
	for i, label := range labels {
		masked[i] = redact.Mask(label)
	}
	return masked
}

// StageLineage is the frozen stage.previous pair and chain identifier of a
// selection.
type StageLineage struct {
	// PreviousRule is the rule whose hop led to the dispatch; empty when
	// none did.
	PreviousRule string

	// PreviousOutcome is "succeeded" or "no_change"; empty when
	// PreviousRule is empty.
	PreviousOutcome string

	// ChainID groups the runs of one pass through a chain of stages. It is
	// empty only on a value read from a row written before chains were
	// recorded, and never on a [RunningEntry].
	ChainID string

	// DispatchedAt is the start of the first run of the pass ChainID names.
	// It is zero only on a value restored from a row that could not be
	// dated.
	DispatchedAt time.Time
}

// formatDispatchedAt returns the persisted form of a pass dispatch time:
// RFC 3339 with nanoseconds in UTC, so a restored value equals the served
// one. The zero time becomes the empty string.
func formatDispatchedAt(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.UTC().Format(time.RFC3339Nano)
}

// parseDispatchedAt reverses [formatDispatchedAt]. An empty or malformed
// value yields the zero time.
func parseDispatchedAt(value string) time.Time {
	at, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return at.UTC()
}

// ContinuationTarget is the pull request a reaction continuation works on
// and which try of its reaction it is. The zero value continues no reaction.
type ContinuationTarget struct {
	PRNumber int
	Owner    string
	Repo     string
	Attempt  int // 0 for a kind that counts no tries
}

// RuleAgent is the agent kind and configured model a dispatch of one rule
// resolves to under the configuration in force.
type RuleAgent struct {
	AgentKind       string
	ConfiguredModel string
}

// StageHopEntry is the runtime view of one issue's stage_hops row.
type StageHopEntry struct {
	Identifier       string
	Count            int
	SourceRule       string
	TargetRule       string
	TargetLabel      string
	PreviousOutcome  string
	SourceDispatchID string

	// TargetObserved reports that a tracker read has shown TargetLabel on
	// the issue since the hop, which releases the dispatch hold.
	TargetObserved bool

	HoppedAt time.Time

	// ChainID is the chain of the run whose exit made the hop.
	ChainID string

	// DisplayID is the qualified form of Identifier the hopping run read;
	// empty when Identifier is display-ready.
	DisplayID string

	// IssueIdentity holds the title, issue type, and state the hopping run
	// read and the labels the hop leaves on the issue.
	IssueIdentity IssueIdentity

	// DispatchedAt is the start of the first run of the pass ChainID names.
	DispatchedAt time.Time

	// RunDispatched reports that a run of the issue was dispatched after the
	// hop, which ends the issue's listing between stages.
	RunDispatched bool

	// NoStateObserved is true after a reconcile read the issue in no state
	// and no later reconcile read it in a state. Set and cleared only by
	// [reconcileTrackerState].
	NoStateObserved bool
}

// State is the single authoritative runtime state owned by the
// orchestrator. Not safe for concurrent access: all mutations are
// serialized through the event loop goroutine, except WorkerWg and
// TrackerOpsWg, which are goroutine-safe. The agent_totals and completed set are backed by SQLite
// and survive restarts.
type State struct {
	// WorkerWg tracks in-flight worker goroutines spawned by
	// [DispatchIssue]. Callers dispatching outside the Run() loop can
	// Wait() before cleanup.
	WorkerWg sync.WaitGroup

	// TrackerOpsWg tracks fire-and-forget tracker API goroutines, drained
	// after worker shutdown so they are not orphaned on exit. A worker adds
	// to it for the detached part of the session.started delivery before
	// it reports its exit and only while its context is live, and the
	// drain cancels every worker before it waits, so an add cannot race
	// the wait.
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

	// Rates are the rates in force; a nil value prices nothing. Only the
	// event loop writes them, and always by replacing the pointer, so a
	// snapshot can share it without a lock.
	Rates *pricing.Rates

	MaxConcurrentAgents int

	// MaxConcurrentByState holds per-state concurrency caps, keys
	// lowercased. An absent key falls back to the global limit.
	MaxConcurrentByState map[string]int

	// MaxTokens mirrors config.Agent.MaxTokens for the in-flight token
	// ceiling, refreshed on every poll tick. 0 means unlimited. Written
	// only on the event-loop goroutine.
	MaxTokens int

	// MaxCostUSD mirrors config.Agent.MaxCostUSD for the money ceiling,
	// refreshed on every poll tick. 0 means unlimited. Written only on the
	// event-loop goroutine.
	MaxCostUSD float64

	// MaxSessions mirrors config.Agent.MaxSessions, refreshed on every poll
	// tick. 0 means unlimited. Written only on the event-loop goroutine.
	MaxSessions int

	// StallTimeoutMS mirrors config.Agent.StallTimeoutMS as of the latest
	// poll tick. 0 means stall detection is off. Written only on the
	// event-loop goroutine.
	StallTimeoutMS int

	// RetainedEvents holds the event buffer of every running run. Nil
	// retains nothing.
	RetainedEvents *RunEventLog

	// StageChains and HopCeiling mirror config.Dispatch.Chains and
	// config.Dispatch.HopCeiling, refreshed on every poll tick. Set before the
	// event loop serves a snapshot. Written only on the event-loop goroutine.
	StageChains [][]string
	HopCeiling  int

	// RuleAgents maps the name of each rule a stage hop can target to the
	// agent kind and model a dispatch of it resolves to. It mirrors the
	// configuration, is refreshed on every poll tick, and is set before the
	// event loop serves a snapshot. Written only on the event-loop
	// goroutine.
	RuleAgents map[string]RuleAgent

	// ReactionMaxAttempts maps a reaction kind to the ceiling its pass
	// applies to [State.ReactionAttempts]; a kind with none is absent. Set
	// before the event loop serves a snapshot and replaced every poll tick.
	// Written only on the event-loop goroutine.
	ReactionMaxAttempts map[string]int

	// ReactionWatchWindows maps a reaction kind to the watch window its pass
	// applies; a kind with none, or with a window of 0, is absent. Set before
	// the event loop serves a snapshot and replaced every poll tick. Written
	// only on the event-loop goroutine.
	ReactionWatchWindows map[string]time.Duration

	// TokenWarningThreshold mirrors config.Agent.TokenWarningThreshold()
	// for the in-flight token warning, refreshed on every poll tick. 0
	// disables the warning. Written only on the event-loop goroutine.
	TokenWarningThreshold int

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

	// ParkReleases holds, per issue ID, the park a person released until the
	// issue's next primary dispatch from the poll tick consumes it or a new
	// park replaces it. An issue ID is never in both ParkReleases and Parked.
	// The durable mirror is the same parked_issues row, with its release
	// columns set. Only the event loop mutates this map.
	ParkReleases map[string]*ParkedEntry

	// AttentionAcknowledgements maps an attention item ID to when a person
	// acknowledged the item. The durable mirror is the
	// attention_acknowledgements table. Only the event loop mutates this map.
	AttentionAcknowledgements map[string]time.Time

	// StageHops maps issue ID to the issue's consecutive automatic stage
	// hops since the last reset. The durable mirror is the stage_hops
	// table. Only the event loop may mutate this map.
	StageHops map[string]*StageHopEntry

	// ActiveOrder maps the ID of each issue the row lists hold to the time it
	// is ordered by. Runtime-only; written only on the event-loop goroutine.
	ActiveOrder map[string]time.Time

	// AgentTotals holds aggregate token counts and cumulative runtime
	// seconds across all ended sessions. Active-session elapsed time is
	// computed at snapshot time.
	AgentTotals AgentTotals

	// UsageWindows maps an agent kind to the newest report of each of its
	// usage windows. Runtime-only.
	UsageWindows map[string][]domain.UsageWindow

	// ReactionAttempts maps composite key (issueID:kind) to the count of
	// reaction-triggered continuations. Reset when the issue leaves Running
	// or RetryAttempts. Runtime-only.
	ReactionAttempts map[string]int

	// ReactionHandedOffComments maps composite key (issueID:kind) to the set
	// of [domain.ReviewComment] IDs the issue's runs were given for that
	// kind, a cache over the reaction_handoffs rows. A present
	// key is authoritative for this process; an absent key means the rows
	// were not loaded, never that the set is empty. Only the review and
	// bot-review kinds use it.
	ReactionHandedOffComments map[string]map[string]struct{}

	// ReactionReportedComments maps composite key (issueID:kind) to the IDs
	// of bot-review comments a spent budget or a triage escalation already
	// reported to a person. It is deleted wherever the kind's
	// [State.ReactionAttempts] entry is and never reaches the store.
	// Runtime-only.
	ReactionReportedComments map[string]map[string]struct{}

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

	// Held maps the ID of each candidate the latest dispatch pass held back
	// to its hold. Replaced wholesale by a pass that ran the dispatch loop
	// and left alone by a tick that returned before it; never persisted.
	// Mutated only by the event loop.
	Held map[string]*HeldEntry

	// UsageLimitHolds maps each hold key to the provider usage limit that
	// stops new sessions under it. Never nil. Restored from the store before
	// the first tick and mutated only by the event loop.
	UsageLimitHolds map[UsageLimitHoldKey]UsageLimitHold

	// TokenBudgetIncomplete is the set of issue IDs whose per-issue token
	// spend is below max_tokens but only a lower bound, so the ceiling could
	// not be fully evaluated. Rebuilt wholesale each tick when the token
	// budget is enabled, cleared when not. Owned by the event loop.
	TokenBudgetIncomplete map[string]struct{}

	// MoneyBudgetIncomplete is the set of issue IDs whose per-issue
	// estimated cost is below max_cost_usd but only a lower bound. Rebuilt
	// wholesale each tick when the money ceiling is enabled, cleared when
	// not. Owned by the event loop.
	MoneyBudgetIncomplete map[string]struct{}

	// SpendCeilings holds the latest evaluation of each configured instance
	// spend window, daily first. Nil while no window is configured.
	// Written only by evaluateSpendCeiling on the event loop.
	SpendCeilings []SpendCeilingStatus
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

type stageRenderCtxKey struct{}

// withStageRender returns a child context carrying the stage render
// context a worker passes to every prompt render.
func withStageRender(ctx context.Context, render prompt.StageContext) context.Context {
	return context.WithValue(ctx, stageRenderCtxKey{}, render)
}

// stageRenderFrom extracts the value injected by [withStageRender], or the
// zero [prompt.StageContext] when absent.
func stageRenderFrom(ctx context.Context) prompt.StageContext {
	render, _ := ctx.Value(stageRenderCtxKey{}).(prompt.StageContext)
	return render
}

type parkRenderCtxKey struct{}

// withParkRender returns a child context carrying the park hand-over the
// worker renders on turn one of its run.
func withParkRender(ctx context.Context, pc prompt.ParkContext) context.Context {
	return context.WithValue(ctx, parkRenderCtxKey{}, pc)
}

// parkRenderFrom extracts the value injected by [withParkRender] and reports
// whether one was injected.
func parkRenderFrom(ctx context.Context) (prompt.ParkContext, bool) {
	pc, ok := ctx.Value(parkRenderCtxKey{}).(prompt.ParkContext)
	return pc, ok
}

// NewState creates an initialized [State] with empty collections and the
// provided config values. maxConcurrentByState keys must be pre-normalized
// to lowercase by the caller.
func NewState(pollIntervalMS, maxConcurrentAgents, maxTokens int, maxConcurrentByState map[string]int, totals AgentTotals) *State {
	if maxConcurrentByState == nil {
		maxConcurrentByState = make(map[string]int)
	}
	return &State{
		PollIntervalMS:            pollIntervalMS,
		MaxConcurrentAgents:       maxConcurrentAgents,
		MaxTokens:                 maxTokens,
		MaxConcurrentByState:      maxConcurrentByState,
		Running:                   make(map[string]*RunningEntry),
		Claimed:                   make(map[string]struct{}),
		RetryAttempts:             make(map[string]*RetryEntry),
		Completed:                 make(map[string]struct{}),
		BudgetExhausted:           make(map[string]*BudgetExhaustedEntry),
		BudgetAnnounced:           make(map[string]BudgetAnnouncement),
		BudgetHoldNoticed:         make(map[string]string),
		Parked:                    make(map[string]*ParkedEntry),
		ParkReleases:              make(map[string]*ParkedEntry),
		AttentionAcknowledgements: make(map[string]time.Time),
		UsageLimitHolds:           make(map[UsageLimitHoldKey]UsageLimitHold),
		StageHops:                 make(map[string]*StageHopEntry),
		ActiveOrder:               make(map[string]time.Time),
		AgentTotals:               totals,
		RetainedEvents:            NewRunEventLog(),
		ReactionAttempts:          make(map[string]int),
		ReactionHandedOffComments: make(map[string]map[string]struct{}),
		ReactionReportedComments:  make(map[string]map[string]struct{}),
		PendingReactions:          make(map[string]*PendingReaction),
		AutoMergeAuthLogged:       make(map[string]struct{}),
		TokenBudgetIncomplete:     make(map[string]struct{}),
		MoneyBudgetIncomplete:     make(map[string]struct{}),
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

// SnapshotIssueUsage is a running issue's completed-session usage,
// value-copied by [RuntimeSnapshot]. UncachedTokens is the figure compared
// with the token budget. The recorded aggregates carry the cost the completed
// sessions were priced at when they finished.
type SnapshotIssueUsage struct {
	Sessions           int     `json:"sessions"`
	TotalTokens        int64   `json:"total_tokens"`
	UncachedTokens     int64   `json:"uncached_tokens"`
	UnmeasuredSessions int     `json:"unmeasured_sessions"`
	UnaccountedTurns   int     `json:"unaccounted_turns"`
	RecordedCostUSD    float64 `json:"recorded_cost_usd"`
	PricedSessions     int     `json:"priced_sessions"`
	PartialSessions    int     `json:"partial_sessions"`
	UnpricedSessions   int     `json:"unpriced_sessions"`
}

// SnapshotStagePosition is where a row's rule stands on its stage chain.
type SnapshotStagePosition struct {
	Chain      []string `json:"chain"`
	Current    string   `json:"current"`
	HopCount   int      `json:"hop_count"`
	HopCeiling int      `json:"hop_ceiling"`
}

// SnapshotContinuation is the pull request a running reaction continuation
// works on and which try of its reaction it is.
type SnapshotContinuation struct {
	Number     int    `json:"number"`
	Repository string `json:"repository"` // owner/repo, masked
	Attempt    int    `json:"attempt"`    // 0 for a kind that counts no tries
}

// SnapshotBlocker is a blocker that holds a candidate. State is masked and
// empty when the tracker returned none.
type SnapshotBlocker struct {
	IssueID    string `json:"issue_id"`
	Identifier string `json:"issue_identifier"`
	DisplayID  string `json:"display_identifier,omitempty"`
	State      string `json:"state,omitempty"`
}

// SnapshotSlotHold is the agent slot limit that held a candidate. State is
// masked and set exactly for [SlotScopeState].
type SnapshotSlotHold struct {
	Scope SlotScope `json:"scope"`
	State string    `json:"state,omitempty"`
	Used  int       `json:"used"`
	Limit int       `json:"limit"`
}

// SnapshotHold is a read-only view of the hold the latest dispatch pass gave
// one candidate. Blockers is nil unless Reason is [SkipBlockedBy], Slot is nil
// unless it is [SkipAgentSlotsFull], ResetsAt is zero unless it is a spend
// reason or [SkipUsageLimit], and AgentKind, ModelOnly, and Model are set for
// [SkipUsageLimit] only.
type SnapshotHold struct {
	Reason    SkipReason        `json:"reason"`
	Since     time.Time         `json:"held_since"`
	Blockers  []SnapshotBlocker `json:"blockers"`
	Slot      *SnapshotSlotHold `json:"slot"`
	ResetsAt  time.Time         `json:"resets_at"`
	AgentKind string            `json:"agent_kind,omitempty"`
	ModelOnly bool              `json:"model_only,omitempty"`
	Model     string            `json:"model,omitempty"`
}

// SnapshotHeldEntry is a read-only view of one held candidate. IssueTitle,
// IssueURL, IssueType, IssueLabels, and State are masked, and IssueLabels is
// never nil.
type SnapshotHeldEntry struct {
	IssueID     string       `json:"issue_id"`
	Identifier  string       `json:"issue_identifier"`
	DisplayID   string       `json:"display_identifier,omitempty"`
	IssueTitle  string       `json:"issue_title,omitempty"`
	IssueURL    string       `json:"issue_url,omitempty"`
	IssueType   string       `json:"issue_type,omitempty"`
	IssueLabels []string     `json:"issue_labels,omitzero"`
	State       string       `json:"state,omitempty"`
	Hold        SnapshotHold `json:"hold"`
}

// SnapshotStageHop is a read-only view of one issue between stages: a hop was
// made and no run of the issue was dispatched since. IssueTitle, IssueURL,
// IssueType, IssueLabels, and State are masked. IssueLabels is nil exactly
// when the record holds no read of the labels. StagePosition is nil when
// RuleName is on no stage chain, and AgentKind and ConfiguredModel are empty
// when the configuration names no rule with a stage label for it. Hold is nil
// while the latest dispatch pass recorded no hold for the issue.
type SnapshotStageHop struct {
	IssueID      string    `json:"issue_id"`
	Identifier   string    `json:"issue_identifier"`
	DisplayID    string    `json:"display_identifier,omitempty"`
	IssueTitle   string    `json:"issue_title,omitempty"`
	IssueURL     string    `json:"issue_url,omitempty"`
	IssueType    string    `json:"issue_type,omitempty"`
	IssueLabels  []string  `json:"issue_labels,omitzero"`
	State        string    `json:"state,omitempty"`
	SourceRule   string    `json:"source_rule"`
	TargetRule   string    `json:"target_rule"`
	TargetLabel  string    `json:"target_label"`
	HoppedAt     time.Time `json:"hopped_at"`
	DispatchedAt time.Time `json:"dispatched_at"`
	ChainID      string    `json:"-"`

	RuleName        string                 `json:"rule_name,omitempty"`
	StagePosition   *SnapshotStagePosition `json:"stage_position"`
	AgentKind       string                 `json:"agent_kind,omitempty"`
	ConfiguredModel string                 `json:"configured_model,omitempty"`
	Hold            *SnapshotHold          `json:"hold"`
}

// SnapshotRunningEntry is a read-only view of a running session, produced
// by [RuntimeSnapshot]. IssueTitle, IssueURL, IssueType, IssueLabels, and
// State are masked, and IssueLabels is never nil. IssueUsage is nil exactly
// when no usage read has succeeded for the run. StagePosition is nil when
// the entry's rule is on no stage chain, and Continuation is nil unless the
// session continues a reaction.
type SnapshotRunningEntry struct {
	IssueID             string                    `json:"issue_id"`
	Identifier          string                    `json:"issue_identifier"`
	DisplayID           string                    `json:"display_identifier,omitempty"`
	IssueTitle          string                    `json:"issue_title,omitempty"`
	IssueURL            string                    `json:"issue_url,omitempty"`
	IssueLabels         []string                  `json:"issue_labels"`
	IssueType           string                    `json:"issue_type,omitempty"`
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
	CacheWriteTokens    int64                     `json:"cache_write_tokens"`
	ModelName           string                    `json:"model_name,omitempty"`
	APIRequestCount     int                       `json:"api_request_count"`
	RequestsByModel     map[string]int            `json:"requests_by_model,omitempty"`
	Skills              []domain.SkillCall        `json:"skills,omitempty"`
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
	StageLineage        StageLineage              `json:"stage_lineage"`
	StagePosition       *SnapshotStagePosition    `json:"stage_position"`
	ReactionKind        string                    `json:"reaction_kind,omitempty"`
	ReactionConfigKey   string                    `json:"reaction_config_key,omitempty"`
	Continuation        *SnapshotContinuation     `json:"continuation,omitempty"`
	ConfiguredModel     string                    `json:"configured_model,omitempty"`
	ConfiguredEffort    string                    `json:"configured_effort,omitempty"`
	UsageMeasured       bool                      `json:"tokens_measured"`
	EstimatedCostUSD    *float64                  `json:"estimated_cost_usd"`
	CostStatus          string                    `json:"cost_status"`
	UsageArrival        registry.UsageArrival     `json:"usage_arrival"`
	UsageAttribution    registry.UsageAttribution `json:"usage_attribution"`
	TokensPending       bool                      `json:"tokens_pending"`
	TokensAwaited       bool                      `json:"tokens_awaited"`
	IssueUsage          *SnapshotIssueUsage       `json:"issue_usage,omitempty"`
	UnaccountedTurns    int                       `json:"unaccounted_turns"`
	DispatchID          string                    `json:"dispatch_id"`

	// QuietAt and StallAt are the instants half and all of the stall timeout
	// after StallReference; both are zero with stall detection off.
	QuietAt time.Time `json:"quiet_at"`
	StallAt time.Time `json:"stall_at"`

	// StallReference is the last agent event instant, else the start of the
	// run: the instant the stall timeout is measured from.
	StallReference time.Time `json:"-"`
}

// UncachedTokens returns the session's total tokens less its cache reads,
// floored at zero: the figure the token budget compares.
func (e SnapshotRunningEntry) UncachedTokens() int64 {
	return domain.TokenUsage{TotalTokens: e.AgentTotalTokens, CacheReadTokens: e.CacheReadTokens}.Uncached()
}

// SnapshotRetryEntry is a read-only view of a pending retry, produced by
// [RuntimeSnapshot]. IssueTitle, IssueURL, IssueType, IssueLabels, and State
// are masked. IssueLabels is nil exactly when the entry holds no read of the
// labels. StagePosition is nil when the entry's rule is on no stage chain.
type SnapshotRetryEntry struct {
	IssueID           string                 `json:"issue_id"`
	Identifier        string                 `json:"issue_identifier"`
	DisplayID         string                 `json:"display_identifier,omitempty"`
	IssueTitle        string                 `json:"issue_title,omitempty"`
	IssueURL          string                 `json:"issue_url,omitempty"`
	IssueType         string                 `json:"issue_type,omitempty"`
	IssueLabels       []string               `json:"issue_labels,omitzero"`
	State             string                 `json:"state,omitempty"`
	Attempt           int                    `json:"attempt"`
	DueAtMS           int64                  `json:"due_at_ms"`
	Error             string                 `json:"error"`
	Trigger           string                 `json:"trigger,omitempty"`
	ReactionKind      string                 `json:"reaction_kind,omitempty"`
	ReactionConfigKey string                 `json:"reaction_config_key,omitempty"`
	RuleName          string                 `json:"rule_name,omitempty"`
	AgentKind         string                 `json:"agent_kind,omitempty"`
	ConfiguredModel   string                 `json:"configured_model,omitempty"`
	StageLineage      StageLineage           `json:"stage_lineage"`
	StagePosition     *SnapshotStagePosition `json:"stage_position"`
	Paused            bool                   `json:"paused"`
}

// SnapshotParkedEntry is a read-only view of one parked issue, produced by
// [RuntimeSnapshot]. IssueTitle, IssueURL, IssueType, IssueLabels, and
// ParkedState are masked. IssueLabels is nil exactly when the entry holds no
// read of the labels. StagePosition is nil when the park follows no run or
// RuleName is on no stage chain, and DispatchedAt is zero when the park
// follows no run or that run's pass is undated.
type SnapshotParkedEntry struct {
	IssueID      string    `json:"issue_id"`
	Identifier   string    `json:"issue_identifier"`
	DisplayID    string    `json:"display_identifier,omitempty"`
	IssueTitle   string    `json:"issue_title,omitempty"`
	IssueURL     string    `json:"issue_url,omitempty"`
	IssueType    string    `json:"issue_type,omitempty"`
	IssueLabels  []string  `json:"issue_labels,omitzero"`
	DispatchedAt time.Time `json:"dispatched_at"`
	Reason       string    `json:"reason"`
	ParkedState  string    `json:"parked_state,omitempty"`
	Label        string    `json:"label"`
	LabelApplied bool      `json:"label_applied"`
	ParkedAt     time.Time `json:"parked_at"`
	ChainID      string    `json:"-"`

	RuleName        string                 `json:"rule_name,omitempty"`
	StagePosition   *SnapshotStagePosition `json:"stage_position"`
	AgentKind       string                 `json:"agent_kind,omitempty"`
	ConfiguredModel string                 `json:"configured_model,omitempty"`
}

// SnapshotBudgetEntry is a read-only, value-copied view of one issue held
// out of dispatch by a budget ceiling, produced by [RuntimeSnapshot].
// IssueTitle, IssueURL, IssueType, IssueLabels, and State are masked.
// IssueLabels is nil exactly when the entry holds no read of the labels.
// StagePosition is nil when RuleName is on no stage chain.
type SnapshotBudgetEntry struct {
	IssueID            string    `json:"issue_id"`
	Identifier         string    `json:"issue_identifier"`
	DisplayID          string    `json:"display_identifier,omitempty"`
	IssueTitle         string    `json:"issue_title,omitempty"`
	IssueURL           string    `json:"issue_url,omitempty"`
	IssueType          string    `json:"issue_type,omitempty"`
	IssueLabels        []string  `json:"issue_labels,omitzero"`
	State              string    `json:"state,omitempty"`
	Reason             string    `json:"reason"`
	UsedSessions       int       `json:"used_sessions"`
	BudgetSessions     int       `json:"budget_sessions"`
	UsedTokens         *int64    `json:"used_tokens"`
	BudgetTokens       int64     `json:"budget_tokens"`
	UnmeasuredSessions *int      `json:"unmeasured_sessions"`
	StoppedInFlight    *int      `json:"stopped_in_flight"`
	UnaccountedTurns   int       `json:"unaccounted_turns"`
	UsedUSD            *float64  `json:"used_usd"`
	UsedUSDComplete    *bool     `json:"used_usd_complete"`
	BudgetUSD          float64   `json:"budget_usd"`
	ExhaustedAt        time.Time `json:"exhausted_at"`

	RuleName        string                 `json:"rule_name,omitempty"`
	StageLineage    StageLineage           `json:"stage_lineage"`
	StagePosition   *SnapshotStagePosition `json:"stage_position"`
	AgentKind       string                 `json:"agent_kind,omitempty"`
	ConfiguredModel string                 `json:"configured_model,omitempty"`
}

// SnapshotPendingReaction is a read-only view of one pending reaction,
// produced by [RuntimeSnapshot]. Every runtime-derived string is masked,
// IssueType, IssueLabels, and State included. IssueLabels is nil exactly when
// the reaction holds no read of the labels. StagePosition is nil when
// RuleName is on no stage chain.
type SnapshotPendingReaction struct {
	IssueID      string              `json:"issue_id"`
	Identifier   string              `json:"issue_identifier"`
	DisplayID    string              `json:"display_identifier,omitempty"`
	IssueTitle   string              `json:"issue_title,omitempty"`
	IssueURL     string              `json:"issue_url,omitempty"`
	IssueType    string              `json:"issue_type,omitempty"`
	IssueLabels  []string            `json:"issue_labels,omitzero"`
	State        string              `json:"state,omitempty"`
	Kind         string              `json:"kind"`
	ConfigKey    string              `json:"config_key"`
	PullRequest  SnapshotPullRequest `json:"pull_request"`
	CreatedAt    time.Time           `json:"created_at"`
	DispatchedAt time.Time           `json:"dispatched_at"`
	ChainID      string              `json:"-"`
	NextPollAt   time.Time           `json:"next_poll_at"`
	Wait         string              `json:"wait,omitempty"`
	UsedAttempts *int                `json:"used_attempts"`
	MaxAttempts  *int                `json:"max_attempts"`
	// EscalatedAt is when the standing escalation began; nil while none stands.
	EscalatedAt *time.Time `json:"escalated_at"`
	// EscalationTrigger is why the standing escalation began; set exactly
	// when EscalatedAt is non-nil.
	EscalationTrigger string `json:"escalation_trigger,omitempty"`
	// WatchEndsAt is when the reaction stops watching unless its watch
	// restarts first; nil for a kind without a watch window or a window of 0,
	// while an escalation stands, and while the latest check awaits approval.
	WatchEndsAt *time.Time     `json:"watch_ends_at"`
	LastCheck   *ReactionCheck `json:"last_check"`

	RuleName        string                 `json:"rule_name,omitempty"`
	StagePosition   *SnapshotStagePosition `json:"stage_position"`
	AgentKind       string                 `json:"agent_kind,omitempty"`
	ConfiguredModel string                 `json:"configured_model,omitempty"`
}

// SnapshotPullRequest is the pull request a pending reaction watches.
type SnapshotPullRequest struct {
	Number     int    `json:"number"`
	Repository string `json:"repository"`
	URL        string `json:"url,omitempty"`
	Branch     string `json:"branch,omitempty"`
	HeadSHA    string `json:"head_sha,omitempty"`
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
	CacheWriteTokens    int64   `json:"cache_write_tokens"`
	SecondsRunning      float64 `json:"seconds_running"`
	UnmeasuredSessions  int64   `json:"unmeasured_sessions"`
	RunningUnreported   int     `json:"running_unreported"`
	RunningNonReporting int     `json:"running_non_reporting"`
}

// SnapshotUsageWindows is one agent kind's usage windows at snapshot time.
type SnapshotUsageWindows struct {
	// ReportedAt is the newest report among all held windows, current or not.
	ReportedAt time.Time `json:"reported_at"`

	// Windows holds the current windows only; never nil.
	Windows []domain.UsageWindow `json:"windows"`
}

// RuntimeSnapshotResult is a point-in-time capture of the orchestrator's
// runtime state, produced by [RuntimeSnapshot].
type RuntimeSnapshotResult struct {
	GeneratedAt          time.Time                       `json:"generated_at"`
	Running              []SnapshotRunningEntry          `json:"running"`
	Retrying             []SnapshotRetryEntry            `json:"retrying"`
	AgentTotals          SnapshotAgentTotals             `json:"agent_totals"`
	UsageWindows         map[string]SnapshotUsageWindows `json:"usage_windows"`
	BudgetExhaustedCount int                             `json:"budget_exhausted_count"`
	BudgetExhausted      []SnapshotBudgetEntry           `json:"budget_exhausted"`
	ParkedCount          int                             `json:"parked_count"`
	Parked               []SnapshotParkedEntry           `json:"parked"`

	// PendingReactions is never nil.
	PendingReactions []SnapshotPendingReaction `json:"pending_reactions"`

	// BetweenStages is never nil.
	BetweenStages []SnapshotStageHop `json:"between_stages"`

	// Held is never nil.
	Held []SnapshotHeldEntry `json:"held"`

	// ActiveOrder maps every issue ID the row lists hold to its order time.
	// It shares nothing with State.
	ActiveOrder map[string]time.Time `json:"-"`

	// MaxTokens, MaxCostUSD, and MaxSessions are the token ceiling, money
	// ceiling, and session budget in force at capture; 0 means no limit.
	MaxTokens   int     `json:"max_tokens"`
	MaxCostUSD  float64 `json:"max_cost_usd"`
	MaxSessions int     `json:"max_sessions"`

	// Rates is State.Rates at capture; a nil value prices nothing. Safe to
	// read without a lock because the rates are replaced, never mutated.
	Rates *pricing.Rates `json:"-"`

	// AttentionAcknowledgements is a copy of State.AttentionAcknowledgements,
	// keyed by attention item ID.
	AttentionAcknowledgements map[string]time.Time `json:"-"`

	// SpendCeilings is a copy of State.SpendCeilings.
	SpendCeilings []SpendCeilingStatus `json:"-"`
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

// usageStillArriving reports whether a session that has recorded no figure
// may still record one. Past the arrival's own moment (turn start for
// incremental, turn end for turn-end) it will not.
func usageStillArriving(arrival registry.UsageArrival, turnCount int, lastEvent domain.AgentEventType) bool {
	switch arrival {
	case registry.UsageArrivalIncremental:
		return turnCount == 0
	case registry.UsageArrivalTurnEnd:
		return !turnHasEnded(turnCount, lastEvent)
	default:
		return false
	}
}

// turnHasEnded reports whether any turn reached a terminal event. A second
// turn cannot begin before the first ends, so a count above one settles it
// without the event.
func turnHasEnded(turnCount int, lastEvent domain.AgentEventType) bool {
	return turnCount > 1 || isTurnTerminalEvent(lastEvent)
}

// SnapshotIssueUsageOf copies a running entry's usage read into the shape the
// snapshot serves. A nil read stays nil so the absence of a successful read is
// not served as zero usage.
func SnapshotIssueUsageOf(usage *persistence.IssueTokenUsage) *SnapshotIssueUsage {
	if usage == nil {
		return nil
	}
	return &SnapshotIssueUsage{
		Sessions:           usage.Sessions,
		TotalTokens:        usage.TotalTokens,
		UncachedTokens:     usage.UncachedTokens,
		UnmeasuredSessions: usage.UnmeasuredSessions,
		UnaccountedTurns:   usage.UnaccountedTurns,
		RecordedCostUSD:    usage.RecordedCostUSD,
		PricedSessions:     usage.PricedSessions,
		PartialSessions:    usage.PartialSessions,
		UnpricedSessions:   usage.UnpricedSessions,
	}
}

// stallReference returns the instant the stall timeout of entry is measured
// from: its last agent event, else the start of the run. The stall check and
// the snapshot's stall instants share it so they cannot disagree.
func stallReference(entry *RunningEntry) time.Time {
	if !entry.LastAgentTimestamp.IsZero() {
		return entry.LastAgentTimestamp
	}
	return entry.StartedAt
}

// stallInstants returns the instants half and all of timeoutMS after
// reference, and zero values when timeoutMS is not positive.
func stallInstants(reference time.Time, timeoutMS int) (quietAt, stallAt time.Time) {
	if timeoutMS <= 0 {
		return time.Time{}, time.Time{}
	}
	timeout := time.Duration(timeoutMS) * time.Millisecond
	return reference.Add(timeout / 2), reference.Add(timeout)
}

func spendOfRunningEntry(entry *RunningEntry) pricing.Spend {
	return pricing.Spend{
		Kind:     entry.AgentKind,
		Model:    cmp.Or(entry.ModelName, entry.ConfiguredModel),
		Measured: entry.UsageMeasured,
		Totals: domain.TokenUsage{
			InputTokens:      entry.AgentInputTokens,
			OutputTokens:     entry.AgentOutputTokens,
			TotalTokens:      entry.AgentTotalTokens,
			CacheReadTokens:  entry.CacheReadTokens,
			CacheWriteTokens: entry.CacheWriteTokens,
		},
		Parts:            entry.UsageByModel,
		UnaccountedTurns: entry.UnaccountedTurns,
		Reported:         entry.ReportedAmounts,
	}
}

// RuntimeSnapshot captures a point-in-time view of the orchestrator's
// runtime state. now is normalized to UTC internally.
//
// Running, Retrying, Parked, PendingReactions, BetweenStages, Held, and AgentTotals are copied out and
// may be retained without synchronization. UsageWindows is copied out, and the UsedPercent values its
// windows point to are never written.
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
		tokensAwaited := entry.CarriesTokenFigure && !entry.UsageMeasured &&
			usageStillArriving(entry.UsageArrival, entry.TurnCount, entry.LastAgentEvent)

		switch {
		case !entry.CarriesTokenFigure:
			runningNonReporting++
		case !entry.UsageMeasured:
			// A session whose figure is no longer coming is non-reporting,
			// not still waiting.
			if tokensAwaited {
				runningUnreported++
			} else {
				runningNonReporting++
			}
		}

		// Absence is the only way to say the breakdown means nothing;
		// gating it here states the rule once.
		var modelRequests map[string]int
		if requestsMeasured && entry.UsageAttribution.NamesModel() && entry.RequestsByModel != nil {
			modelRequests = make(map[string]int, len(entry.RequestsByModel))
			maps.Copy(modelRequests, entry.RequestsByModel)
		}
		identity := maskedIdentity(issueIdentityOf(entry.Issue))
		quietAt, stallAt := stallInstants(stallReference(entry), state.StallTimeoutMS)
		liveCost, liveStatus := pricing.PriceSession(spendOfRunningEntry(entry), state.Rates)
		snap.Running = append(snap.Running, SnapshotRunningEntry{
			IssueID:             entry.Issue.ID,
			Identifier:          entry.Identifier,
			DisplayID:           entry.Issue.DisplayID,
			IssueTitle:          identity.Title,
			IssueURL:            identity.URL,
			IssueLabels:         maskedLabels(entry.Issue.Labels),
			IssueType:           identity.IssueType,
			State:               redact.Mask(entry.Issue.State),
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
			CacheWriteTokens:    entry.CacheWriteTokens,
			ModelName:           entry.ModelName,
			APIRequestCount:     entry.APIRequestCount,
			RequestsByModel:     modelRequests,
			Skills:              slices.Clone(entry.Skills),
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
			StageLineage:        entry.StageLineage,
			StagePosition:       stagePosition(state, entry.Issue.ID, entry.RuleName, entry.StageLineage.PreviousRule),
			ReactionKind:        entry.ReactionKind,
			ReactionConfigKey:   ReactionConfigKey(entry.ReactionKind),
			Continuation:        snapshotContinuation(entry.Continuation),
			ConfiguredModel:     entry.ConfiguredModel,
			ConfiguredEffort:    entry.ConfiguredEffort,
			UsageMeasured:       entry.UsageMeasured,
			EstimatedCostUSD:    liveCost,
			CostStatus:          string(liveStatus),
			UsageArrival:        entry.UsageArrival,
			UsageAttribution:    entry.UsageAttribution,
			TokensPending: entry.UsageArrival == registry.UsageArrivalTurnEnd &&
				entry.UsageMeasured && !isTurnTerminalEvent(entry.LastAgentEvent),
			TokensAwaited:    tokensAwaited,
			IssueUsage:       SnapshotIssueUsageOf(entry.IssueUsage),
			UnaccountedTurns: entry.UnaccountedTurns,
			DispatchID:       entry.DispatchID,
			QuietAt:          quietAt,
			StallAt:          stallAt,
			StallReference:   stallReference(entry),
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
		identity := maskedIdentity(entry.IssueIdentity)
		snap.Retrying = append(snap.Retrying, SnapshotRetryEntry{
			IssueID:     entry.IssueID,
			Identifier:  entry.Identifier,
			DisplayID:   entry.DisplayID,
			IssueTitle:  identity.Title,
			IssueURL:    identity.URL,
			IssueType:   identity.IssueType,
			IssueLabels: identity.Labels,
			State:       identity.State,
			Attempt:     entry.Attempt,
			DueAtMS:     entry.DueAtMS,
			Error:       entry.Error,

			Trigger:           entry.Trigger,
			ReactionKind:      entry.ReactionKind,
			ReactionConfigKey: ReactionConfigKey(entry.ReactionKind),
			RuleName:          entry.RuleName,
			AgentKind:         entry.AgentKind,
			ConfiguredModel:   entry.ConfiguredModel,
			StageLineage:      entry.StageLineage,
			StagePosition:     stagePosition(state, entry.IssueID, entry.RuleName, entry.StageLineage.PreviousRule),
			Paused:            entry.pausedSinceMS != 0,
		})
	}

	snap.AgentTotals = SnapshotAgentTotals{
		InputTokens:         state.AgentTotals.InputTokens,
		OutputTokens:        state.AgentTotals.OutputTokens,
		TotalTokens:         state.AgentTotals.TotalTokens,
		CacheReadTokens:     state.AgentTotals.CacheReadTokens,
		CacheWriteTokens:    state.AgentTotals.CacheWriteTokens,
		SecondsRunning:      state.AgentTotals.SecondsRunning + activeElapsedTotal,
		UnmeasuredSessions:  state.AgentTotals.UnmeasuredSessions,
		RunningUnreported:   runningUnreported,
		RunningNonReporting: runningNonReporting,
	}

	snap.MaxTokens = state.MaxTokens
	snap.MaxCostUSD = state.MaxCostUSD
	snap.MaxSessions = state.MaxSessions
	snap.Rates = state.Rates
	snap.AttentionAcknowledgements = maps.Clone(state.AttentionAcknowledgements)
	snap.SpendCeilings = slices.Clone(state.SpendCeilings)

	snap.BudgetExhaustedCount = len(state.BudgetExhausted)
	budgetExhausted := make([]SnapshotBudgetEntry, 0, len(state.BudgetExhausted))
	for id, entry := range state.BudgetExhausted {
		identity := maskedIdentity(entry.IssueIdentity)
		budgetExhausted = append(budgetExhausted, SnapshotBudgetEntry{
			IssueID:            id,
			Identifier:         entry.Identifier,
			DisplayID:          entry.DisplayID,
			IssueTitle:         identity.Title,
			IssueURL:           identity.URL,
			IssueType:          identity.IssueType,
			IssueLabels:        identity.Labels,
			State:              identity.State,
			Reason:             entry.Reason,
			UsedSessions:       entry.UsedSessions,
			BudgetSessions:     entry.BudgetSessions,
			UsedTokens:         entry.UsedTokens,
			BudgetTokens:       entry.BudgetTokens,
			UnmeasuredSessions: entry.UnmeasuredSessions,
			StoppedInFlight:    entry.StoppedInFlight,
			UnaccountedTurns:   entry.UnaccountedTurns,
			UsedUSD:            clonePointer(entry.UsedUSD),
			UsedUSDComplete:    clonePointer(entry.UsedUSDComplete),
			BudgetUSD:          entry.BudgetUSD,
			ExhaustedAt:        entry.ExhaustedAt,
			RuleName:           entry.RuleName,
			StageLineage:       entry.StageLineage,
			StagePosition:      stagePosition(state, id, entry.RuleName, entry.StageLineage.PreviousRule),
			AgentKind:          entry.AgentKind,
			ConfiguredModel:    entry.ConfiguredModel,
		})
	}
	slices.SortFunc(budgetExhausted, func(a, b SnapshotBudgetEntry) int {
		return cmp.Or(cmp.Compare(a.Identifier, b.Identifier), cmp.Compare(a.IssueID, b.IssueID))
	})
	snap.BudgetExhausted = budgetExhausted

	snap.ParkedCount = len(state.Parked)
	parked := make([]SnapshotParkedEntry, 0, len(state.Parked))
	for id, entry := range state.Parked {
		identity := maskedIdentity(entry.IssueIdentity)
		parked = append(parked, SnapshotParkedEntry{
			IssueID:      id,
			Identifier:   entry.Identifier,
			DisplayID:    entry.DisplayID,
			IssueTitle:   identity.Title,
			IssueURL:     identity.URL,
			IssueType:    identity.IssueType,
			IssueLabels:  identity.Labels,
			DispatchedAt: entry.StageLineage.DispatchedAt,
			Reason:       entry.Reason,
			ParkedState:  redact.Mask(entry.ParkedState),
			Label:        entry.Label,
			LabelApplied: entry.LabelApplied,
			ParkedAt:     entry.ParkedAt,
			ChainID:      entry.StageLineage.ChainID,

			RuleName:        entry.RuleName,
			StagePosition:   stagePosition(state, id, entry.RuleName, entry.StageLineage.PreviousRule),
			AgentKind:       entry.AgentKind,
			ConfiguredModel: entry.ConfiguredModel,
		})
	}
	slices.SortFunc(parked, func(a, b SnapshotParkedEntry) int {
		return cmp.Or(cmp.Compare(a.Identifier, b.Identifier), cmp.Compare(a.IssueID, b.IssueID))
	})
	snap.Parked = parked

	snap.PendingReactions = snapshotPendingReactions(state)
	snap.BetweenStages = snapshotBetweenStages(state)
	snap.Held = snapshotHeld(state)
	snap.ActiveOrder = activeOrderOf(state)

	snap.UsageWindows = snapshotUsageWindows(state, now)

	return snap
}

// snapshotUsageWindows returns the per-kind windows current at now; never nil.
func snapshotUsageWindows(state *State, now time.Time) map[string]SnapshotUsageWindows {
	windows := make(map[string]SnapshotUsageWindows, len(state.UsageWindows))
	for kind, held := range state.UsageWindows {
		var reportedAt time.Time
		for _, w := range held {
			if w.ReportedAt.After(reportedAt) {
				reportedAt = w.ReportedAt
			}
		}
		windows[kind] = SnapshotUsageWindows{
			ReportedAt: reportedAt,
			Windows:    usage.CurrentWindows(held, now),
		}
	}
	return windows
}

// snapshotPendingReactions builds the pending-reaction rows sorted by issue
// identifier, issue ID, and kind. It shares no slice, map, or pointer with
// state and masks every runtime-derived string.
func snapshotPendingReactions(state *State) []SnapshotPendingReaction {
	rows := make([]SnapshotPendingReaction, 0, len(state.PendingReactions))
	for _, entry := range state.PendingReactions {
		pr := pullRequestOf(entry)
		identity := maskedIdentity(entry.IssueIdentity)
		row := SnapshotPendingReaction{
			IssueID:     entry.IssueID,
			Identifier:  entry.Identifier,
			DisplayID:   entry.DisplayID,
			IssueTitle:  identity.Title,
			IssueURL:    identity.URL,
			IssueType:   identity.IssueType,
			IssueLabels: identity.Labels,
			State:       identity.State,
			Kind:        entry.Kind,
			ConfigKey:   ReactionConfigKey(entry.Kind),
			PullRequest: SnapshotPullRequest{
				Number:     pr.Number,
				Repository: redact.Mask(pr.Owner + "/" + pr.Repo),
				URL:        redact.Mask(entry.PullRequestURL),
				Branch:     redact.Mask(pr.Branch),
				HeadSHA:    redact.Mask(pr.HeadSHA),
			},
			CreatedAt:    entry.CreatedAt,
			DispatchedAt: entry.StageLineage.DispatchedAt,
			ChainID:      entry.StageLineage.ChainID,
			NextPollAt:   cmp.Or(entry.PendingRetryAt, entry.CreatedAt),
			Wait:         entry.Wait,
			LastCheck:    snapshotReactionCheck(entry.LastCheck),

			RuleName:        entry.RuleName,
			StagePosition:   stagePosition(state, entry.IssueID, entry.RuleName, entry.StageLineage.PreviousRule),
			AgentKind:       entry.AgentKind,
			ConfiguredModel: entry.ConfiguredModel,
		}
		if reactionKinds[entry.Kind].attempts == countsAttempts {
			used := state.ReactionAttempts[ReactionKey(entry.IssueID, entry.Kind)]
			row.UsedAttempts = &used
		}
		if ceiling, ok := state.ReactionMaxAttempts[entry.Kind]; ok {
			row.MaxAttempts = &ceiling
		}
		if !entry.EscalatedAt.IsZero() {
			escalatedAt := entry.EscalatedAt
			row.EscalatedAt = &escalatedAt
			row.EscalationTrigger = string(entry.EscalationTrigger)
		}
		if window, ok := state.ReactionWatchWindows[entry.Kind]; ok && entry.EscalatedAt.IsZero() && !awaits(entry.LastCheck) {
			watchEndsAt := watchBasis(entry).Add(window)
			row.WatchEndsAt = &watchEndsAt
		}
		rows = append(rows, row)
	}
	slices.SortFunc(rows, func(a, b SnapshotPendingReaction) int {
		return cmp.Or(
			cmp.Compare(a.Identifier, b.Identifier),
			cmp.Compare(a.IssueID, b.IssueID),
			cmp.Compare(a.Kind, b.Kind),
		)
	})
	return rows
}

// watchBasis returns the instant the pass for entry's kind measures its watch
// window from.
func watchBasis(entry *PendingReaction) time.Time {
	switch entry.Kind {
	case ReactionKindCI:
		return cmp.Or(entry.HeadRecordedAt, entry.CreatedAt)
	case ReactionKindAutoMerge:
		return entry.CreatedAt
	default:
		return watchWindowStart(entry)
	}
}

// snapshotBetweenStages builds the rows of issues whose hop no dispatched run
// has followed, sorted by issue identifier, then issue ID. An issue that is
// running or queued for a retry is left out, so a failed write of the
// dispatch flag or a retry restored after a restart does not list it twice.
func snapshotBetweenStages(state *State) []SnapshotStageHop {
	rows := make([]SnapshotStageHop, 0, len(state.StageHops))
	for id, hop := range state.StageHops {
		if !listedBetweenStages(state, id, hop) {
			continue
		}
		identity := maskedIdentity(hop.IssueIdentity)
		agent := state.RuleAgents[hop.TargetRule]
		row := SnapshotStageHop{
			IssueID:      id,
			Identifier:   hop.Identifier,
			DisplayID:    hop.DisplayID,
			IssueTitle:   identity.Title,
			IssueURL:     identity.URL,
			IssueType:    identity.IssueType,
			IssueLabels:  identity.Labels,
			State:        identity.State,
			SourceRule:   hop.SourceRule,
			TargetRule:   hop.TargetRule,
			TargetLabel:  hop.TargetLabel,
			HoppedAt:     hop.HoppedAt,
			DispatchedAt: hop.DispatchedAt,
			ChainID:      hop.ChainID,

			RuleName:        hop.TargetRule,
			StagePosition:   stagePosition(state, id, hop.TargetRule, hop.SourceRule),
			AgentKind:       agent.AgentKind,
			ConfiguredModel: agent.ConfiguredModel,
		}
		if held := state.Held[id]; held != nil {
			hold := snapshotHold(held)
			row.Hold = &hold
		}
		rows = append(rows, row)
	}
	slices.SortFunc(rows, func(a, b SnapshotStageHop) int {
		return cmp.Or(cmp.Compare(a.Identifier, b.Identifier), cmp.Compare(a.IssueID, b.IssueID))
	})
	return rows
}

// listedBetweenStages reports whether hop puts the issue in the between-stages
// list: no dispatched run has followed it, and the issue is neither running
// nor queued for a retry.
func listedBetweenStages(state *State, issueID string, hop *StageHopEntry) bool {
	if hop.RunDispatched {
		return false
	}
	if _, running := state.Running[issueID]; running {
		return false
	}
	_, retrying := state.RetryAttempts[issueID]
	return !retrying
}

// snapshotContinuation returns the view of target, or nil for the zero value.
func snapshotContinuation(target ContinuationTarget) *SnapshotContinuation {
	if target == (ContinuationTarget{}) {
		return nil
	}
	return &SnapshotContinuation{
		Number:     target.PRNumber,
		Repository: redact.Mask(target.Owner + "/" + target.Repo),
		Attempt:    target.Attempt,
	}
}

// stagePosition returns where rule stands on the stage chains in force, or
// nil when no chain holds it. The chain is copied, so the result shares
// nothing with state.
func stagePosition(state *State, issueID, rule, previous string) *SnapshotStagePosition {
	chain := StageChainOf(state.StageChains, rule, previous)
	if chain == nil {
		return nil
	}
	position := &SnapshotStagePosition{
		Chain:      chain,
		Current:    rule,
		HopCeiling: state.HopCeiling,
	}
	if hop, ok := state.StageHops[issueID]; ok {
		position.HopCount = hop.Count
	}
	return position
}

// StageChainOf returns a copy of the chain in chains that holds rule,
// preferring one that lists previous immediately before rule when previous
// is non-empty; nil when no chain holds rule. The copy keeps callers from
// aliasing configuration.
func StageChainOf(chains [][]string, rule, previous string) []string {
	chain := chainHolding(chains, rule, previous)
	if chain == nil && previous != "" {
		chain = chainHolding(chains, rule, "")
	}
	return slices.Clone(chain)
}

// chainHolding returns the first chain that holds rule and, when previous is
// non-empty, lists previous immediately before it; nil when none does.
func chainHolding(chains [][]string, rule, previous string) []string {
	for _, chain := range chains {
		i := slices.Index(chain, rule)
		if i < 0 {
			continue
		}
		if previous == "" || (i > 0 && chain[i-1] == previous) {
			return chain
		}
	}
	return nil
}

// snapshotReactionCheck deep-copies check with the head and failing check
// names masked. A nil check stays nil; FailingChecks of a CI check is never
// nil.
func snapshotReactionCheck(check *ReactionCheck) *ReactionCheck {
	if check == nil {
		return nil
	}
	out := &ReactionCheck{
		ObservedAt: check.ObservedAt,
		HeadSHA:    redact.Mask(check.HeadSHA),
	}
	if check.CI != nil {
		failing := make([]string, len(check.CI.FailingChecks))
		for i, name := range check.CI.FailingChecks {
			failing[i] = redact.Mask(name)
		}
		ci := *check.CI
		ci.FailingChecks = failing
		out.CI = &ci
	}
	if check.Comments != nil {
		comments := *check.Comments
		out.Comments = &comments
	}
	if check.Mergeability != nil {
		mergeability := MergeabilityCheck{}
		if check.Mergeability.Mergeable != nil {
			mergeable := *check.Mergeability.Mergeable
			mergeability.Mergeable = &mergeable
		}
		out.Mergeability = &mergeability
	}
	return out
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
	if err := checkEscalation(cfg.Escalation); err != nil {
		return ReviewReactionConfig{}, err
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
	if err := checkEscalation(cfg.Escalation); err != nil {
		return BotReviewReactionConfig{}, err
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

// checkEscalation reports an error unless escalation is a value a
// reaction's escalation setting accepts.
func checkEscalation(escalation string) error {
	switch escalation {
	case "label", "comment", "none":
		return nil
	default:
		return fmt.Errorf("invalid escalation: must be \"label\", \"comment\", or \"none\", got %q", escalation)
	}
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
	if err := checkEscalation(cfg.Escalation); err != nil {
		return AutoMergeReactionConfig{}, err
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
	if err := checkEscalation(cfg.Escalation); err != nil {
		return MergeConflictReactionConfig{}, err
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
// The state lists are read from tracker as written, so the offline
// validator and the runtime terminal drop agree on what "terminal" means.
func BuildMergeCompletionReactionConfig(rc config.ReactionConfig, tracker config.TrackerConfig) (MergeCompletionReactionConfig, error) {
	cfg := MergeCompletionReactionConfig{
		PollIntervalMS:  60000,
		Escalation:      rc.Escalation,
		EscalationLabel: rc.EscalationLabel,
		MaxRetries:      rc.MaxRetries,
	}

	if cfg.Escalation == "" {
		cfg.Escalation = "label"
	}
	if err := checkEscalation(cfg.Escalation); err != nil {
		return MergeCompletionReactionConfig{}, err
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

	if strings.EqualFold(cfg.TargetState, tracker.HandoffState) {
		return MergeCompletionReactionConfig{}, fmt.Errorf("invalid target_state %q: must not equal tracker.handoff_state", cfg.TargetState)
	}
	if _, active := stateSet(tracker.ActiveStates)[strings.ToLower(cfg.TargetState)]; active {
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
