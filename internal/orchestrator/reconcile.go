package orchestrator

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/workspace"
)

// ReconcileStore is the persistence interface required by
// [ReconcileRunningIssues]. Satisfied by [persistence.Store] in production
// and by test doubles in unit tests.
type ReconcileStore interface {
	SaveRetryEntry(ctx context.Context, entry persistence.RetryEntry) error
	DeleteRetryEntry(ctx context.Context, issueID string) error
	AppendRunHistory(ctx context.Context, run persistence.RunHistory) (persistence.RunHistory, error)
	UpsertReactionFingerprint(ctx context.Context, issueID, kind, fingerprint string) error
	GetReactionFingerprint(ctx context.Context, issueID, kind string) (fingerprint string, dispatched bool, err error)
	MarkReactionDispatched(ctx context.Context, issueID, kind string) error
	DeleteReactionFingerprint(ctx context.Context, issueID, kind string) error
	UpsertReactionObservation(
		ctx context.Context,
		issueID, kind, fingerprint string,
		observedAt time.Time,
	) (persistence.ReactionObservation, error)
	MarkReactionObservationDispatched(ctx context.Context, issueID, kind, fingerprint string) error
	CountWorkerRunsCompletedSince(ctx context.Context, issueID string, since time.Time) (int, error)
}

var _ ReconcileStore = (*persistence.Store)(nil)

// ReconcileParams holds the dependencies for [ReconcileRunningIssues] that
// are not part of the core [State]. This separates pure state mutation from
// I/O side effects (tracker API, SQLite persistence).
type ReconcileParams struct {
	// TrackerAdapter fetches current issue states for running issues.
	TrackerAdapter domain.TrackerAdapter

	// ActiveStates is the current list of configured active issue states.
	ActiveStates []string

	// TerminalStates is the current list of configured terminal issue states.
	TerminalStates []string

	// HandoffState is treated as a keep-running state only for running
	// reaction continuations of any known reaction kind. Does not
	// affect fresh dispatch eligibility in ShouldDispatch.
	HandoffState string

	// StallTimeoutMS is the configured stall detection threshold.
	// If <= 0, stall detection is skipped entirely.
	StallTimeoutMS int

	// MaxRetryBackoffMS is the configured cap for exponential backoff
	// delay (from config.Agent.MaxRetryBackoffMS). Used when scheduling
	// stall-detection retries.
	MaxRetryBackoffMS int

	// Store is the SQLite persistence layer for retry entry operations.
	Store ReconcileStore

	// OnRetryFire is the callback invoked when a stall-detection retry
	// timer expires. Routes back into the event loop.
	OnRetryFire func(issueID string)

	// NowFunc returns the current UTC time. Injected for testability.
	// If nil, time.Now().UTC() is used.
	NowFunc func() time.Time

	// Ctx is the context for tracker API calls and persistence operations.
	Ctx context.Context

	// Logger is the structured logger with orchestrator context.
	Logger *slog.Logger

	// Metrics records instrumentation counters for reconciliation events.
	// If nil, defaults to [domain.NoopMetrics].
	Metrics domain.Metrics

	// CIProvider is the CI status provider. When non-nil, the reconcile
	// loop polls CI status for issues in state.PendingReactions.
	CIProvider domain.CIStatusProvider

	// CIFeedback holds CI feedback tuning (max retries, escalation mode).
	// Only read when CIProvider is non-nil.
	CIFeedback config.CIFeedbackConfig

	// CIWatchWindow bounds the age of a CI PendingReaction entry, measured
	// from the last recorded head and falling back to the entry's
	// creation time before any head is recorded. Zero or negative
	// disables the bound. Supplied from reactions.ci_failure.watch_window_ms.
	CIWatchWindow time.Duration

	// SCMAdapter provides review comment fetching. When non-nil, the
	// reconcile loop polls review comments for issues with PR metadata.
	SCMAdapter domain.SCMAdapter

	// ReviewConfig holds review reaction configuration. Only read when
	// SCMAdapter is non-nil.
	ReviewConfig ReviewReactionConfig

	// ReviewPendingTTL is the maximum age of a review PendingReaction
	// entry before it is dropped. Zero disables TTL enforcement.
	ReviewPendingTTL time.Duration

	// AutoMergeConfig holds auto-merge reaction configuration. Only
	// read when AutoMergeReactionConfigured is true.
	AutoMergeConfig AutoMergeReactionConfig

	// AutoMergePendingTTL is the maximum age of an auto-merge
	// PendingReaction entry before it is dropped. Zero disables TTL
	// enforcement.
	AutoMergePendingTTL time.Duration

	// AutoMergeReactionConfigured marks whether the auto-merge feature
	// is active for the current process. Reconcile, enqueue, and
	// recovery paths gate on this flag.
	AutoMergeReactionConfigured bool

	// BotReviewConfig holds bot-review reaction configuration. Only read
	// when BotReviewConfigured is true.
	BotReviewConfig BotReviewReactionConfig

	// BotReviewPendingTTL is the maximum age of a bot-review
	// PendingReaction entry before it is dropped. Zero disables TTL
	// enforcement.
	BotReviewPendingTTL time.Duration

	// BotReviewConfigured marks whether the bot-review feature is active
	// for the current process. The reconcile pass gates on this flag
	// independently of SCMAdapter presence, since a single SCM adapter
	// may be shared with the review and auto-merge kinds.
	BotReviewConfigured bool

	// MergeConflictConfig holds merge-conflict reaction configuration.
	// Only read when MergeConflictReactionConfigured is true.
	MergeConflictConfig MergeConflictReactionConfig

	// MergeConflictPendingTTL is the maximum age of a merge-conflict
	// PendingReaction entry before it is dropped. Zero disables TTL
	// enforcement.
	MergeConflictPendingTTL time.Duration

	// MergeConflictReactionConfigured marks whether the merge-conflict
	// feature is active for the current process. Reconcile, enqueue, and
	// recovery paths gate on this flag.
	MergeConflictReactionConfigured bool

	// LabelReviewConfig holds label-review reaction configuration. Only
	// read when LabelReviewReactionConfigured is true. Unlike every
	// sibling kind there is no accompanying LabelReviewPendingTTL: a human
	// label gesture stays actionable regardless of age, so the label-review
	// pending entry has no drop-on-age branch.
	LabelReviewConfig LabelReviewReactionConfig

	// LabelReviewReactionConfigured marks whether the label-review feature
	// is active for the current process. Reconcile, enqueue, and recovery
	// paths gate on this flag.
	LabelReviewReactionConfigured bool

	// LabelFixConfig holds label-fix reaction configuration. Only read
	// when LabelFixReactionConfigured is true. Like label-review and unlike
	// every sibling kind there is no accompanying LabelFixPendingTTL: a
	// human label gesture stays actionable regardless of age, so the
	// label-fix pending entry has no drop-on-age branch.
	LabelFixConfig LabelFixReactionConfig

	// LabelFixReactionConfigured marks whether the label-fix feature is
	// active for the current process. Reconcile, enqueue, and recovery
	// paths gate on this flag.
	LabelFixReactionConfigured bool

	// MergeCompletionConfig holds merge-completion reaction
	// configuration. Only read when MergeCompletionReactionConfigured
	// is true. Unlike every expiring sibling kind there is no
	// accompanying MergeCompletionPendingTTL: a pending entry carries no
	// general expiry, so a merge can wait on human review for days. Only
	// the post-merge missing-identifier condition is bounded, by a fixed
	// thirty-minute grace period.
	MergeCompletionConfig MergeCompletionReactionConfig

	// MergeCompletionReactionConfigured marks whether the
	// merge-completion feature is active for the current process.
	// Reconcile, enqueue, and recovery paths gate on this flag.
	MergeCompletionReactionConfigured bool
}

// ReconcileRunningIssues detects stalled workers and refreshes tracker
// state for all running issues. Intended to be called from the poll tick
// before dispatch; wiring into the event loop is done by the caller.
//
// Stall detection cancels workers that have exceeded the configured stall
// timeout and schedules exponential-backoff retries. Tracker state refresh
// queries the tracker for current issue states: terminal issues are marked
// for workspace cleanup, active issues get their in-memory snapshot updated,
// and non-active/non-terminal issues are cancelled without cleanup.
//
// Running entries are never removed by reconciliation. Cancelled workers
// exit asynchronously and are processed by [HandleWorkerExit].
func ReconcileRunningIssues(state *State, params ReconcileParams) {
	log := params.Logger
	if log == nil {
		log = slog.Default()
	}

	metrics := params.Metrics
	if metrics == nil {
		metrics = &domain.NoopMetrics{}
	}

	ctx := params.Ctx
	if ctx == nil {
		ctx = context.Background()
	}

	now := time.Now().UTC()
	if params.NowFunc != nil {
		now = params.NowFunc().UTC()
	}

	// Re-arm retry entries whose timer event was dropped by the retry
	// timer channel's non-blocking send, so an undeliverable incumbent
	// cannot hold the retry slot for the process lifetime.
	reconcileOverdueRetries(state, params, log, now)

	// Cancel stalled workers and schedule exponential-backoff retries.
	reconcileStalled(state, params, log, ctx, now, metrics)

	// Refresh issue states from the tracker and stop workers for
	// terminal or non-active issues.
	reconcileTrackerState(state, params, log, ctx, metrics)

	// Poll CI status for issues with pending CI checks.
	reconcileCIStatus(state, params, log, ctx, metrics)

	// Poll review comments for issues with pending review reactions.
	reconcileReviewComments(state, params, log, ctx, metrics)

	// Poll bot-authored review comments for issues with pending
	// bot-review reactions. Runs after the human review pass and before
	// auto-merge.
	reconcileBotReviewComments(state, params, log, ctx, metrics)

	// Detect merge conflicts on managed open PRs. Runs before auto-merge
	// so a freshly observed conflict is acted on (a rebase continuation
	// scheduled) before auto-merge re-confirms its existing dirty
	// deferral on the same tick.
	reconcileMergeConflicts(state, params, log, ctx, metrics)

	// Poll auto-merge preconditions for issues with pending merge
	// reactions. Runs LAST so the CI and review reconcile passes have
	// already updated their state before the merge call inspects it.
	reconcileAutoMerge(state, params, log, ctx, metrics)

	// Detect review-label commands on managed PRs and dispatch read-only
	// review sessions. Ordering does not affect correctness: the pass is
	// fully cross-kind isolated. Placed last as the newest reaction added.
	reconcileLabelReviewCommands(state, params, log, ctx, metrics)

	// Detect fix-label commands on managed PRs and dispatch full read-write
	// fix sessions. Ordering does not affect correctness: the pass is fully
	// cross-kind isolated, mutating only label-fix state.
	reconcileLabelFixCommands(state, params, log, ctx, metrics)

	// Observe the merge state of managed pull requests and transition
	// the linked issue to the configured terminal state exactly once.
	// Placed last so a merge the orchestrator performed earlier in the
	// same tick can be observed on this pass.
	reconcileMergeCompletion(state, params, log, ctx, metrics)
}

// overdueRetryGrace bounds how long a retry entry's DueAtMS may lag the
// current tick before [reconcileOverdueRetries] treats its timer event
// as dropped and re-arms the entry with a zero delay.
const overdueRetryGrace = 60 * time.Second

// reconcileOverdueRetries re-arms retry entries whose timer event was
// never delivered, so an undeliverable incumbent cannot hold the retry
// slot for the process lifetime. Entries whose timer was never armed,
// which are the startup reconstructions still awaiting activation, are
// skipped.
func reconcileOverdueRetries(state *State, params ReconcileParams, log *slog.Logger, now time.Time) {
	graceMS := overdueRetryGrace.Milliseconds()

	var overdueIDs []string
	for issueID, entry := range state.RetryAttempts {
		if entry.TimerHandle == nil {
			continue
		}
		if now.UnixMilli()-entry.DueAtMS > graceMS {
			overdueIDs = append(overdueIDs, issueID)
		}
	}

	for _, issueID := range overdueIDs {
		entry, ok := state.RetryAttempts[issueID]
		if !ok {
			continue
		}

		overdueMS := now.UnixMilli() - entry.DueAtMS
		pausedSinceMS := entry.pausedSinceMS
		entryLog := logging.WithIssue(log, issueID, entry.Identifier)

		CancelRetry(state, issueID)
		ScheduleRetry(state, ScheduleRetryParams{
			IssueID:             issueID,
			Identifier:          entry.Identifier,
			DisplayID:           entry.DisplayID,
			Attempt:             entry.Attempt,
			Error:               entry.Error,
			LastSSHHost:         entry.LastSSHHost,
			SessionID:           entry.SessionID,
			ContinuationContext: entry.ContinuationContext,
			ReactionKind:        entry.ReactionKind,
			AgentKind:           entry.AgentKind,
			RuleName:            entry.RuleName,
			TemplateID:          entry.TemplateID,
			Logger:              entryLog,
		}, params.OnRetryFire)

		if rearmed, ok := state.RetryAttempts[issueID]; ok {
			rearmed.pausedSinceMS = pausedSinceMS
		}

		entryLog.Warn("overdue retry re-armed",
			slog.String("kind", entry.ReactionKind),
			slog.Int("attempt", entry.Attempt),
			slog.Int64("overdue_ms", overdueMS),
		)
	}
}

// reconcileStalled cancels running entries whose last activity exceeds the
// stall timeout and schedules an exponential-backoff retry for each.
func reconcileStalled(state *State, params ReconcileParams, log *slog.Logger, ctx context.Context, now time.Time, metrics domain.Metrics) {
	if params.StallTimeoutMS <= 0 {
		return
	}

	stallThreshold := int64(params.StallTimeoutMS)

	for issueID, entry := range state.Running {
		entryLog := logging.WithIssue(log, issueID, entry.Identifier)

		refTime := entry.StartedAt
		if !entry.LastAgentTimestamp.IsZero() {
			refTime = entry.LastAgentTimestamp
		}

		elapsedMS := now.Sub(refTime).Milliseconds()
		if elapsedMS <= stallThreshold {
			continue
		}

		if entry.CancelFunc != nil {
			entry.CancelFunc()
		}

		entryLog.Warn("stall detected, cancelling worker",
			slog.Int64("elapsed_ms", elapsedMS),
			slog.Int("stall_timeout_ms", params.StallTimeoutMS),
		)

		if incumbent := retrySlotIncumbent(state, issueID); incumbent != nil {
			logRetrySlotDeferral(entryLog, "stall", incumbent)
			continue
		}

		nextAttempt := NextAttempt(entry.RetryAttempt)
		delayMS := computeBackoffDelay(nextAttempt, params.MaxRetryBackoffMS)

		ScheduleRetry(state, ScheduleRetryParams{
			IssueID:             issueID,
			Identifier:          entry.Identifier,
			DisplayID:           entry.Issue.DisplayID,
			Attempt:             nextAttempt,
			DelayMS:             delayMS,
			Error:               "stall timeout exceeded",
			LastSSHHost:         entry.SSHHost,
			SessionID:           entry.SessionID,
			ContinuationContext: entry.ContinuationContext,
			ReactionKind:        entry.ReactionKind,
			AgentKind:           entry.AgentKind,
			RuleName:            entry.RuleName,
			TemplateID:          entry.TemplateID,
			Logger:              entryLog,
		}, params.OnRetryFire)
		metrics.IncRetries(triggerStall)

		if retryEntry, ok := state.RetryAttempts[issueID]; ok {
			if isKnownReactionKind(retryEntry.ReactionKind) {
				if err := params.Store.DeleteRetryEntry(ctx, issueID); err != nil {
					entryLog.Error("failed to delete persisted reaction retry entry",
						slog.Any("error", err),
					)
				}
				continue
			}

			pEntry := persistence.RetryEntry{
				IssueID:    retryEntry.IssueID,
				Identifier: retryEntry.Identifier,
				Attempt:    retryEntry.Attempt,
				DueAtMs:    retryEntry.DueAtMS,
				Error:      stringPtr(retryEntry.Error),
				RuleName:   retryEntry.RuleName,
				TemplateID: retryEntry.TemplateID,
				AgentKind:  retryEntry.AgentKind,
			}
			if err := params.Store.SaveRetryEntry(ctx, pEntry); err != nil {
				entryLog.Error("failed to persist stall retry entry",
					slog.Any("error", err),
				)
			}
		}
	}
}

// trackerObservationIDs returns the deduplicated issue ids whose tracker
// state [reconcileTrackerState] must refresh: every id in state.Running
// plus every issue id carried by an entry in state.PendingReactions.
//
// Returns an empty slice when both inputs are empty; callers must treat
// that as "make no tracker call".
func trackerObservationIDs(state *State) []string {
	seen := make(map[string]struct{}, len(state.Running)+len(state.PendingReactions))
	for id := range state.Running {
		seen[id] = struct{}{}
	}
	for _, entry := range state.PendingReactions {
		seen[entry.IssueID] = struct{}{}
	}

	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	return ids
}

// pendingReactionIdentifier returns the Identifier field of any
// state.PendingReactions entry whose IssueID equals issueID, or the empty
// string when none exists. Every pending entry for one issue carries the
// same identifier, so the first match found is sufficient.
func pendingReactionIdentifier(state *State, issueID string) string {
	for _, entry := range state.PendingReactions {
		if entry.IssueID == issueID {
			return entry.Identifier
		}
	}
	return ""
}

// terminalReleaseCounts reports one [releaseTerminalIssueState] call's
// outcome, so the caller can log a single record and suppress it when
// nothing was released.
type terminalReleaseCounts struct {
	PendingReleased  int
	AttemptsReleased int
	ClaimReleased    bool
	RetryCancelled   bool
}

// releaseTerminalIssueState drops one issue's runtime reaction bookkeeping
// and its dispatch claim: every pending reaction entry, every reaction
// attempt counter, the pending retry, and the claim. It performs no
// tracker call, no source-control call, no reaction-fingerprint read or
// write, and no workspace removal.
//
// entryLog must already carry issue_id and issue_identifier, derived by
// the caller before this function deletes the entries that hold the
// identifier. Calling releaseTerminalIssueState twice for the same issue
// is safe: the second call returns a zero-valued [terminalReleaseCounts].
func releaseTerminalIssueState(ctx context.Context, state *State, store ReconcileStore, issueID string, entryLog *slog.Logger) terminalReleaseCounts {
	prefix := issueID + ":"

	var counts terminalReleaseCounts
	for key := range state.PendingReactions {
		if strings.HasPrefix(key, prefix) {
			delete(state.PendingReactions, key)
			counts.PendingReleased++
		}
	}
	for key := range state.ReactionAttempts {
		if strings.HasPrefix(key, prefix) {
			delete(state.ReactionAttempts, key)
			counts.AttemptsReleased++
		}
	}

	if _, exists := state.RetryAttempts[issueID]; exists {
		CancelRetry(state, issueID)
		counts.RetryCancelled = true
	}
	// Deleted unconditionally: a persisted row can outlive its in-memory
	// entry across a restart.
	if err := store.DeleteRetryEntry(ctx, issueID); err != nil {
		entryLog.Error("failed to delete retry entry for terminal issue",
			slog.Any("error", err),
		)
	}
	if _, exists := state.Claimed[issueID]; exists {
		delete(state.Claimed, issueID)
		counts.ClaimReleased = true
	}

	return counts
}

// reconcileTrackerState fetches current issue states for every running
// issue and every issue holding a pending reaction entry, cancels workers
// whose issues are terminal or no longer active, and releases the runtime
// reaction bookkeeping of any issue the tracker reports terminal, whether
// or not that issue has a running worker.
func reconcileTrackerState(state *State, params ReconcileParams, log *slog.Logger, ctx context.Context, metrics domain.Metrics) {
	if params.TrackerAdapter == nil {
		return
	}

	ids := trackerObservationIDs(state)
	if len(ids) == 0 {
		return
	}

	refreshed, err := params.TrackerAdapter.FetchIssueStatesByIDs(ctx, ids)
	if err != nil {
		log.Warn("tracker state refresh failed, keeping workers running",
			slog.Any("error", err),
		)
		return
	}

	activeSet := stateSet(params.ActiveStates)
	terminalSet := stateSet(params.TerminalStates)

	for issueID, stateName := range refreshed {
		entry := state.Running[issueID]

		normalized := strings.ToLower(stateName)

		if _, terminal := terminalSet[normalized]; terminal {
			identifier := pendingReactionIdentifier(state, issueID)
			if entry != nil {
				identifier = entry.Identifier
			}
			entryLog := logging.WithIssue(log, issueID, identifier)

			if entry != nil && entry.PendingCleanup {
				continue
			}

			var counts terminalReleaseCounts
			if entry != nil {
				if entry.CancelFunc != nil {
					entry.CancelFunc()
				}
				counts = releaseTerminalIssueState(ctx, state, params.Store, issueID, entryLog)
				entry.PendingCleanup = true
				entry.ObservedTerminalState = stateName
				metrics.IncReconciliationActions(actionCleanup)
				entryLog.Info("stopping worker for terminal issue",
					slog.String("state", stateName),
				)
			} else {
				counts = releaseTerminalIssueState(ctx, state, params.Store, issueID, entryLog)
			}

			if counts.PendingReleased > 0 || counts.AttemptsReleased > 0 || counts.ClaimReleased || counts.RetryCancelled {
				entryLog.Info("released reaction state for terminal issue",
					slog.String("state", stateName),
					slog.Int("pending_released", counts.PendingReleased),
					slog.Int("attempts_released", counts.AttemptsReleased),
					slog.Bool("claim_released", counts.ClaimReleased),
					slog.Bool("retry_cancelled", counts.RetryCancelled),
				)
			}
			continue
		}

		if entry == nil {
			continue
		}

		entryLog := logging.WithIssue(log, issueID, entry.Identifier)

		if _, active := activeSet[normalized]; active {
			entry.Issue.State = stateName
			metrics.IncReconciliationActions(actionKeep)
			entryLog.Debug("refreshed issue state",
				slog.String("state", stateName),
			)
			continue
		}

		isHandoff := params.HandoffState != "" && strings.EqualFold(stateName, params.HandoffState)
		if isHandoff && isKnownReactionKind(entry.ReactionKind) {
			entry.Issue.State = stateName
			metrics.IncReconciliationActions(actionKeep)
			entryLog.Debug("keeping reaction worker running in handoff state",
				slog.String("state", stateName),
				slog.String("kind", entry.ReactionKind),
			)
			continue
		}

		// Non-active, non-terminal: cancel without workspace cleanup.
		if entry.CancelFunc != nil {
			entry.CancelFunc()
		}
		if incumbent := retrySlotIncumbent(state, issueID); incumbent != nil {
			// Skipping only one of CancelRetry or DeleteRetryEntry would
			// leave the in-memory entry and the persisted row disagreeing,
			// so both are skipped together to protect the incumbent.
			logRetrySlotDeferral(entryLog, "tracker-state", incumbent)
		} else {
			CancelRetry(state, issueID)
			if err := params.Store.DeleteRetryEntry(ctx, issueID); err != nil {
				entryLog.Error("failed to delete retry entry for non-active issue",
					slog.Any("error", err),
				)
			}
		}
		metrics.IncReconciliationActions(actionStop)
		entryLog.Info("stopping worker for non-active issue",
			slog.String("state", stateName),
		)
	}
}

// sweepEveryNTicks is the number of poll ticks between workspace sweeps.
// Running this sweep every 60 ticks is frequent enough for eventual
// consistency while avoiding unbounded tracker API load from orphaned
// non-terminal workspaces.
const sweepEveryNTicks = 60

// SweepStore is the persistence interface required by [SweepWorkspaces].
type SweepStore interface {
	LatestRunCompletionByIdentifier(ctx context.Context, identifiers []string) (map[string]string, error)
}

// sweepSummary accumulates the per-pass outcome counts reported by
// [emitSweepSummary]. The nine counters after candidates partition the
// candidate set: candidates equals their sum on every pass.
type sweepSummary struct {
	candidates           int
	excludedRunning      int
	excludedRetry        int
	excludedReaction     int
	removedTerminal      int
	removedAge           int
	retainedInWindow     int
	retainedNoActivity   int
	retainedNotEvaluated int
	failed               int
	retentionDays        int
	agePass              string // "on", "off", or "unavailable"
	trackerRead          string // "ok" or "failed"
}

// emitSweepSummary logs the per-pass sweep outcome as a single Info
// record. It is called unconditionally at the end of every pass that
// produced a candidate set, including one that removed nothing.
func emitSweepSummary(log *slog.Logger, summary sweepSummary) {
	log.Info("sweep: pass complete",
		slog.Int("candidates", summary.candidates),
		slog.Int("excluded_running", summary.excludedRunning),
		slog.Int("excluded_retry", summary.excludedRetry),
		slog.Int("excluded_reaction", summary.excludedReaction),
		slog.Int("removed_terminal", summary.removedTerminal),
		slog.Int("removed_age", summary.removedAge),
		slog.Int("retained_in_window", summary.retainedInWindow),
		slog.Int("retained_no_activity", summary.retainedNoActivity),
		slog.Int("retained_not_evaluated", summary.retainedNotEvaluated),
		slog.Int("failed", summary.failed),
		slog.Int("retention_days", summary.retentionDays),
		slog.String("age_pass", summary.agePass),
		slog.String("tracker_read", summary.trackerRead),
	)
}

// activityAnchor returns the later of the two RFC3339 timestamps and
// whether either parsed.
//
// An unparseable or empty value contributes nothing. A workspace whose
// only timestamps are unparseable therefore has no anchor, the same
// outcome as having no timestamps at all.
func activityAnchor(completedAt, pushedAt string) (time.Time, bool) {
	var best time.Time
	found := false
	for _, value := range [2]string{completedAt, pushedAt} {
		if value == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			continue
		}
		parsed = parsed.UTC()
		if !found || parsed.After(best) {
			best = parsed
			found = true
		}
	}
	return best, found
}

// buildSweepExclusions returns the map of workspace key to the reason it
// is excluded from sweep candidacy: "running", "retry", or "reaction".
//
// Precedence is running, then retry, then reaction, so a key present in
// more than one source is reported exactly once and the sweep's outcome
// counters partition the candidate set.
func buildSweepExclusions(state *State, log *slog.Logger) map[string]string {
	out := make(map[string]string)

	for _, entry := range state.Running {
		key, err := workspace.SanitizeKey(entry.Identifier)
		if err != nil {
			log.Warn("sweep: failed to sanitize running identifier",
				slog.String("identifier", entry.Identifier),
				slog.Any("error", err),
			)
			continue
		}
		out[key] = "running"
	}

	for _, entry := range state.RetryAttempts {
		key, err := workspace.SanitizeKey(entry.Identifier)
		if err != nil {
			log.Warn("sweep: failed to sanitize retry identifier",
				slog.String("identifier", entry.Identifier),
				slog.Any("error", err),
			)
			continue
		}
		if _, exists := out[key]; !exists {
			out[key] = "retry"
		}
	}

	for _, entry := range state.PendingReactions {
		if !reactionKindPinsWorkspace(entry.Kind) {
			continue
		}
		key, err := workspace.SanitizeKey(entry.Identifier)
		if err != nil {
			log.Warn("sweep: failed to sanitize pending reaction identifier",
				slog.String("identifier", entry.Identifier),
				slog.Any("error", err),
			)
			continue
		}
		if _, exists := out[key]; !exists {
			out[key] = "reaction"
		}
	}

	return out
}

// runAgePass evaluates every key in ageKeys against params.RetentionDays
// and removes those whose latest recorded activity is older than that
// window. Each early return credits every key in ageKeys to
// summary.retainedNotEvaluated, so the pass never leaves a candidate
// unaccounted for in the outcome partition.
func runAgePass(params SweepWorkspacesParams, summary *sweepSummary, ageKeys []string, now time.Time, log *slog.Logger, metrics domain.Metrics) {
	if params.RetentionDays < config.WorkspaceRetentionMinDays {
		summary.retainedNotEvaluated += len(ageKeys)
		return
	}
	if params.Store == nil {
		summary.agePass = "unavailable"
		summary.retainedNotEvaluated += len(ageKeys)
		return
	}

	summary.agePass = "on"
	retentionCutoff := now.UTC().Add(-time.Duration(params.RetentionDays) * 24 * time.Hour)

	completions, err := params.Store.LatestRunCompletionByIdentifier(params.Ctx, ageKeys)
	if err != nil {
		log.Warn("sweep: failed to read run history activity",
			slog.Any("error", err),
		)
		summary.agePass = "unavailable"
		summary.retainedNotEvaluated += len(ageKeys)
		return
	}

	for _, key := range ageKeys {
		pathResult, pathErr := workspace.ComputePath(params.WorkspaceRoot, key)
		if pathErr != nil {
			log.Warn("sweep: failed to resolve workspace path",
				slog.String("workspace_key", key),
				slog.Any("error", pathErr),
			)
			summary.failed++
			continue
		}

		meta := workspace.ReadSCMMetadata(pathResult.Path, log)
		anchor, hasAnchor := activityAnchor(completions[key], meta.PushedAt)

		if !hasAnchor {
			summary.retainedNoActivity++
			continue
		}
		if !anchor.Before(retentionCutoff) {
			summary.retainedInWindow++
			continue
		}

		cleanupErr := workspace.Cleanup(params.Ctx, workspace.CleanupParams{
			Root:          params.WorkspaceRoot,
			Identifier:    key,
			IssueID:       key,
			BeforeRemove:  params.BeforeRemoveHook,
			HookTimeoutMS: params.HookTimeoutMS,
			Logger:        log,
		})
		if cleanupErr != nil {
			log.Warn("sweep: failed to remove expired workspace",
				slog.String("workspace_key", key),
				slog.Any("error", cleanupErr),
			)
			summary.failed++
			continue
		}

		log.Info("sweep: removed expired workspace",
			slog.String("workspace_key", key),
			slog.String("last_activity", anchor.Format(time.RFC3339)),
			slog.Int("age_days", int(now.UTC().Sub(anchor).Hours()/24)),
		)
		summary.removedAge++
		metrics.IncReconciliationActions(actionSweepExpired)
	}
}

// SweepWorkspacesParams holds the dependencies for [SweepWorkspaces]. All
// fields except Logger and Metrics are required; nil Logger defaults to
// [slog.Default] and nil Metrics defaults to [domain.NoopMetrics].
type SweepWorkspacesParams struct {
	WorkspaceRoot    string
	TrackerAdapter   domain.TrackerAdapter
	TerminalStates   []string
	BeforeRemoveHook string
	HookTimeoutMS    int

	// RetentionDays is the configured workspace.retention_days window.
	// A value below [config.WorkspaceRetentionMinDays] disables the age
	// pass, reported as agePass "off".
	RetentionDays int

	// Store resolves each age candidate's latest recorded activity. A
	// nil Store disables the age pass, reported as agePass
	// "unavailable".
	Store SweepStore

	// NowFunc returns the current time. If nil, time.Now is used.
	NowFunc func() time.Time

	Ctx     context.Context
	Logger  *slog.Logger
	Metrics domain.Metrics
}

// SweepWorkspaces removes workspace directories on two independent
// grounds: the issue reached a terminal tracker state, or the
// workspace's latest recorded activity is older than the configured
// retention window. It lists workspace keys on disk, excludes any that
// belong to in-flight work, queries the tracker for the state of the
// rest, cleans up those reported terminal, and evaluates whatever
// remains against the retention window.
//
// One summary record is emitted per pass that produced a candidate set,
// whether or not anything was removed.
func SweepWorkspaces(state *State, params SweepWorkspacesParams) {
	log := params.Logger
	if log == nil {
		log = slog.Default()
	}

	metrics := params.Metrics
	if metrics == nil {
		metrics = &domain.NoopMetrics{}
	}

	now := time.Now()
	if params.NowFunc != nil {
		now = params.NowFunc()
	}

	keys, err := workspace.ListWorkspaceKeys(params.WorkspaceRoot)
	if err != nil {
		log.Warn("sweep: failed to list workspace keys",
			slog.Any("error", err),
		)
		return
	}

	summary := sweepSummary{
		candidates:    len(keys),
		retentionDays: params.RetentionDays,
		trackerRead:   "ok",
		agePass:       "off",
	}

	exclusions := buildSweepExclusions(state, log)
	remaining := make([]string, 0, len(keys))
	for _, key := range keys {
		switch exclusions[key] {
		case "running":
			summary.excludedRunning++
		case "retry":
			summary.excludedRetry++
		case "reaction":
			summary.excludedReaction++
		default:
			remaining = append(remaining, key)
		}
	}

	statesByKey := map[string]string{}
	if len(remaining) > 0 {
		statesByKey, err = params.TrackerAdapter.FetchIssueStatesByIdentifiers(params.Ctx, remaining)
		if err != nil {
			log.Warn("sweep: failed to fetch issue states",
				slog.Any("error", err),
			)
			statesByKey = map[string]string{}
			summary.trackerRead = "failed"
		}
	}

	terminalSet := make(map[string]struct{}, len(params.TerminalStates))
	for _, s := range params.TerminalStates {
		terminalSet[strings.ToLower(s)] = struct{}{}
	}

	terminalKeys := make([]string, 0, len(remaining))
	ageKeys := make([]string, 0, len(remaining))
	for _, key := range remaining {
		stateName, known := statesByKey[key]
		_, terminal := terminalSet[strings.ToLower(stateName)]
		if known && terminal {
			terminalKeys = append(terminalKeys, key)
			continue
		}
		ageKeys = append(ageKeys, key)
	}

	if len(terminalKeys) > 0 {
		log.Info("sweep: cleaning terminal workspaces",
			slog.Int("count", len(terminalKeys)),
		)

		result := workspace.CleanupTerminal(params.Ctx, workspace.CleanupTerminalParams{
			Root:          params.WorkspaceRoot,
			Identifiers:   terminalKeys,
			BeforeRemove:  params.BeforeRemoveHook,
			HookTimeoutMS: params.HookTimeoutMS,
			Logger:        log,
		})
		summary.removedTerminal += len(result.Removed)
		summary.failed += len(result.Errors)
		for range result.Removed {
			metrics.IncReconciliationActions(actionSweepCleanup)
		}
	}

	runAgePass(params, &summary, ageKeys, now, log, metrics)
	emitSweepSummary(log, summary)
}
