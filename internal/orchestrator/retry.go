package orchestrator

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/notify/route"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/pricing"
	"github.com/sortie-ai/sortie/internal/prompt"
)

// pausedRetryMaxDwell bounds how long a known-reaction retry may be
// rescheduled consecutively because the issue's own state does not permit a
// dispatch, before [HandleRetryTimer] drops it rather than holding the
// retry slot for the process lifetime.
const pausedRetryMaxDwell = 30 * time.Minute

// RetryTimerStore is the persistence interface required by
// [HandleRetryTimer].
type RetryTimerStore interface {
	SaveRetryEntry(ctx context.Context, entry persistence.RetryEntry) error
	DeleteRetryEntry(ctx context.Context, issueID string) error
	UpsertLivePass(ctx context.Context, pass persistence.LivePass) error
	DeleteLivePass(ctx context.Context, issueID, kind string) error
	CountRunHistoryByIssue(ctx context.Context, issueID string) (int, error)
	QueryConsecutiveHandoffAbsenceCounts(ctx context.Context, issueIDs []string) (map[string]int, error)
	LatestAgentRuns(ctx context.Context, issueIDs []string) (map[string]persistence.RunHistory, error)
	TokenUsageByIssue(ctx context.Context, issueID string) (persistence.IssueTokenUsage, error)
	MarkReactionDispatched(ctx context.Context, issueID, kind string) error
	UpsertParkedIssue(ctx context.Context, entry persistence.ParkedIssue) error
	DeleteParkedIssue(ctx context.Context, issueID string) error
	ResetHandoffAbsenceSequence(ctx context.Context, issueID string) error
	UpsertBudgetHoldNotice(ctx context.Context, notice persistence.BudgetHoldNotice) error
	MarkStageHopDispatched(ctx context.Context, issueID string) error
	DeleteStageHop(ctx context.Context, issueID string) error
	RecordIssueOutcome(ctx context.Context, outcome persistence.IssueOutcome) error
	UsageCompletedBetween(ctx context.Context, since, until time.Time) (persistence.IssueTokenUsage, error)
}

// HandleRetryTimerParams holds the dependencies for [HandleRetryTimer]
// that are not part of the core [State], separating pure state mutation
// from I/O side effects.
type HandleRetryTimerParams struct {
	Store RetryTimerStore

	TrackerAdapter domain.TrackerAdapter

	// Router selects the destinations of the budget-hold event. Nil
	// routes nothing.
	Router *route.Router

	ActiveStates []string

	TerminalStates []string

	// Dispatch is the dispatch configuration in force, read to compute the
	// stage render of a dispatch.
	Dispatch config.DispatchConfig

	// HandoffState is the configured tracker handoff state. Known reaction
	// retries may dispatch while the fetched issue equals it. Empty means
	// no handoff-state retry eligibility applies.
	HandoffState string

	MaxRetryBackoffMS int

	// ReleaseHeld releases a retry the instance spend ceiling or a
	// usage-limit hold holds instead of re-arming it, keeping its persisted
	// entry for a later process.
	ReleaseHeld bool

	// ConfiguredModelOf returns the model a dispatch of the resolution is
	// configured with. Nil reads the popped entry's ConfiguredModel.
	ConfiguredModelOf func(DispatchResolution) string

	// MakeWorkerFn constructs a [WorkerFunc]. The retry handler resolves
	// the adapter and the attempt's settings before invoking it; the
	// reaction kind selects the read-only worker posture for label-review
	// dispatches.
	MakeWorkerFn func(resumeSessionID, sshHost, agentKind, templateID, reactionKind string, adapter domain.AgentAdapter, attempt AttemptSettings) WorkerFunc

	// AgentAdapterByKind resolves the agent adapter for the given kind.
	// Required when MakeWorkerFn is set. On an unknown kind the retry
	// handler logs and reschedules the retry with backoff, keeping the
	// claim.
	AgentAdapterByKind func(kind string) (domain.AgentAdapter, error)

	// ResolveSelection maps the selection frozen on the retry entry and
	// the freshly fetched issue to the selection the retry dispatches on.
	// Nil dispatches on the frozen selection.
	ResolveSelection func(frozen DispatchResolution, issue domain.Issue) DispatchResolution

	// DefaultAgentKind is the workflow-wide default coalesced onto a
	// popped retry entry whose AgentKind is empty (legacy rows persisted
	// before dispatch rule routing).
	DefaultAgentKind string

	OnRetryFire func(issueID string)

	Ctx context.Context

	Logger *slog.Logger

	// MaxSessions is the per-issue effort budget. When > 0, the claim is
	// released instead of dispatching once completed sessions reach it.
	MaxSessions int

	MaxConsecutiveAbsences int

	// HandoffParkingLabel is the review-comments escalation label. Empty
	// falls back to "needs-human".
	HandoffParkingLabel string

	// HandoffEvidencePolicy is the configured evidence policy. Under the
	// off policy no absence is recorded, so the absence gate is not
	// consulted.
	HandoffEvidencePolicy config.HandoffEvidencePolicy

	// MaxTokens is the per-issue token budget. When > 0, the claim is
	// released instead of dispatching once summed tokens reach it.
	MaxTokens int

	// MaxCostUSD is the per-issue money budget. When > 0, the claim is
	// released instead of dispatching once the issue's estimated cost
	// reaches it.
	MaxCostUSD float64

	// SpendCeilings holds the instance spend ceiling of each window; a
	// window absent or at 0 is off. While any window has reached its ceiling
	// the retry is kept scheduled instead of dispatching.
	SpendCeilings map[SpendWindow]float64

	// PollIntervalMS is the recheck delay of a retry held by a spend
	// ceiling.
	PollIntervalMS int

	// NowFunc reads the clock for the spend ceiling and usage-limit hold
	// evaluation. Nil reads [time.Now].
	NowFunc func() time.Time

	// Metrics records retry timer instrumentation. If nil, defaults to
	// [domain.NoopMetrics].
	Metrics domain.Metrics

	// HostPool is the SSH host pool. May be nil (local mode).
	HostPool *HostPool

	// WorkflowFile is the base filename of the active WORKFLOW.md file.
	WorkflowFile string

	// ResolveAttemptSettings resolves the retry attempt's settings from the
	// configuration in force now; sshHost is empty for a local launch.
	// Required. An attempt whose settings carry refusals is rescheduled.
	ResolveAttemptSettings func(selection DispatchResolution, sshHost string) AttemptSettings
}

// putBackSchedule returns the Attempt and delay in milliseconds for putting
// back a retry that could not start. A fire that an operator request brought
// forward keeps the retry's Attempt and the due time it had before the
// request, so a request never leaves a retry later than it was; any other
// fire takes the arm's attempt and its backoff.
func putBackSchedule(popped *RetryEntry, attempt int, maxRetryBackoffMS int) (int, int64) {
	if popped.dueBeforeRequestMS != 0 {
		return popped.Attempt, max(0, popped.dueBeforeRequestMS-time.Now().UnixMilli())
	}
	return attempt, computeBackoffDelay(attempt, maxRetryBackoffMS)
}

// retryClock returns the instant the spend ceiling and usage-limit hold
// evaluation of a retry fire reads.
func retryClock(params HandleRetryTimerParams) time.Time {
	if params.NowFunc != nil {
		return params.NowFunc()
	}
	return time.Now()
}

// HandleRetryTimer processes a retry timer event for the given issue: it
// removes the retry entry, re-fetches and validates the issue, checks slot
// availability, and either dispatches, reschedules, or releases the claim.
// It reports whether params.ReleaseHeld released a held retry.
// Must be called from the single-writer event loop.
func HandleRetryTimer(state *State, issueID string, params HandleRetryTimerParams) (heldReleased bool) {
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

	// A missing entry means the timer raced a cancellation or a newer
	// ScheduleRetry; a stale one means a replacement timer will fire at the
	// correct time. Either way, skip.
	popped, exists := state.RetryAttempts[issueID]
	if !exists {
		log.Debug("retry timer for unknown entry",
			slog.String("issue_id", issueID),
		)
		return
	}
	if isStaleRetryTimer(popped) {
		log.Debug("stale retry timer, entry was rescheduled",
			slog.String("issue_id", issueID),
			slog.Int64("due_at_ms", popped.DueAtMS),
		)
		return
	}
	if popped.TimerHandle != nil {
		popped.TimerHandle.Stop()
	}
	delete(state.RetryAttempts, issueID)

	log = logging.WithIssue(log, issueID, popped.Identifier)

	rearm := func(attempt int, delayMS, dueAtMS int64, retryErr string) {
		ScheduleRetry(state, ScheduleRetryParams{
			IssueID:             issueID,
			Identifier:          popped.Identifier,
			DisplayID:           popped.DisplayID,
			IssueIdentity:       popped.IssueIdentity,
			Attempt:             attempt,
			DelayMS:             delayMS,
			DueAtMS:             dueAtMS,
			Error:               retryErr,
			LastSSHHost:         popped.LastSSHHost,
			SessionID:           popped.SessionID,
			ContinuationContext: popped.ContinuationContext,
			ReactionKind:        popped.ReactionKind,
			Continuation:        popped.Continuation,
			AgentKind:           popped.AgentKind,
			ConfiguredModel:     popped.ConfiguredModel,
			RuleName:            popped.RuleName,
			StageLineage:        popped.StageLineage,
			RuleSettingsApplied: popped.RuleSettingsApplied,
			TemplateID:          popped.TemplateID,
			Logger:              log,
			Trigger:             triggerTimer,
			OriginTrigger:       popped.OriginTrigger,
			UsageLimitNotified:  popped.UsageLimitNotified,
		}, params.OnRetryFire)
	}

	reschedule := func(attempt int, delayMS int64, retryErr string) {
		rearm(attempt, delayMS, 0, retryErr)
		if isKnownReactionKind(popped.ReactionKind) {
			if err := params.Store.DeleteRetryEntry(ctx, issueID); err != nil {
				log.Error("failed to delete persisted reaction retry entry",
					slog.Any("error", err),
				)
			}
		} else {
			persistRetryEntry(ctx, log, params.Store, state, issueID)
		}
		metrics.IncRetries(triggerTimer)
	}

	// rearmHeld re-arms the retry unchanged for dueAt and marks the new entry
	// so a later slot or host shortage keeps its attempt.
	rearmHeld := func(dueAt time.Time) {
		rearm(popped.Attempt, 0, dueAt.UnixMilli(), popped.Error)
		if entry, ok := state.RetryAttempts[issueID]; ok {
			entry.holdRearmed = true
		}
	}

	// ScheduleRetry builds a fresh entry, so the flag is applied to it
	// afterwards, and only by the arms that mean to keep it.
	setNoStateObserved := func(observed bool) {
		if entry, ok := state.RetryAttempts[issueID]; ok {
			entry.noStateObserved = observed
		}
	}

	// The cancelled worker may not yet have exited, leaving the issue in
	// Running; re-dispatching would overwrite the entry and spawn a
	// duplicate. Reschedule at the same attempt to fire after the exit.
	if _, running := state.Running[issueID]; running {
		attempt, delayMS := putBackSchedule(popped, popped.Attempt, params.MaxRetryBackoffMS)
		log.Debug("worker still running, rescheduling retry",
			slog.Int("attempt", attempt),
		)
		reschedule(attempt, delayMS, "worker still running")
		setNoStateObserved(popped.noStateObserved)
		return
	}

	// Two gates precede any fetch or dispatch, both exempt for a known
	// reaction continuation: an already-parked issue is refused regardless
	// of policy, and an issue whose absence count just reached the ceiling
	// is parked (skipped under the off policy, which records no absence).
	if !isKnownReactionKind(popped.ReactionKind) {
		if parked := state.Parked[issueID]; parked != nil {
			log.Info("issue parked, releasing claim",
				slog.String("reason", parked.Reason),
			)
			delete(state.Claimed, issueID)
			if err := params.Store.DeleteRetryEntry(ctx, issueID); err != nil {
				log.Error("failed to delete retry entry for parked issue",
					slog.Any("error", err),
				)
			}
			return
		}

		if params.HandoffEvidencePolicy.Effective() != config.HandoffEvidenceOff {
			absenceCeiling := handoffAbsenceCeiling(params.MaxConsecutiveAbsences)
			absenceCounts, absenceErr := params.Store.QueryConsecutiveHandoffAbsenceCounts(ctx, []string{issueID})
			consecutiveAbsences := absenceCounts[issueID]
			if absenceErr != nil {
				consecutiveAbsences = max(popped.Attempt, 1)
				log.Warn("handoff absence count query failed, using retry attempt fallback",
					slog.Int("consecutive_absences", consecutiveAbsences),
					slog.Any("error", absenceErr),
				)
			}
			if consecutiveAbsences >= absenceCeiling {
				parkHandoffAbsence(
					state,
					ctx,
					params.Store,
					params.TrackerAdapter,
					metrics,
					issueID,
					popped.Identifier,
					popped.DisplayID,
					popped.IssueIdentity,
					parkedRun{
						RuleName:        popped.RuleName,
						StageLineage:    popped.StageLineage,
						AgentKind:       popped.AgentKind,
						ConfiguredModel: popped.ConfiguredModel,
						HandOver:        latestParkHandOver(ctx, params.Store, issueID, log),
					},
					"",
					consecutiveAbsences,
					absenceCeiling,
					params.HandoffParkingLabel,
					log,
				)
				return
			}
		}
	}

	// blockBudget releases the claim and drops the retry entry for an issue
	// whose budget is exhausted. Each later gate may call this after an
	// earlier one did, overwriting the entry so an issue exhausted on several
	// axes reports the money budget, then the token budget. ExhaustedAt is taken from the
	// announcement memory when it already knows this issue under the same
	// reason, so a hold surviving a gap in candidacy keeps its onset time.
	blocked := false
	blockBudget := func(reason string, usedSessions int, usage *persistence.IssueTokenUsage) {
		at := time.Now().UTC()
		if told, wasTold := state.BudgetAnnounced[issueID]; wasTold && told.Reason == reason {
			at = told.At
		}
		entry := &BudgetExhaustedEntry{
			Identifier:      popped.Identifier,
			DisplayID:       popped.DisplayID,
			IssueIdentity:   popped.IssueIdentity,
			Reason:          reason,
			UsedSessions:    usedSessions,
			BudgetSessions:  params.MaxSessions,
			BudgetTokens:    int64(params.MaxTokens),
			BudgetUSD:       params.MaxCostUSD,
			ExhaustedAt:     at,
			RuleName:        popped.RuleName,
			StageLineage:    popped.StageLineage,
			AgentKind:       popped.AgentKind,
			ConfiguredModel: popped.ConfiguredModel,
		}
		if usage != nil {
			entry.setTokenUsage(*usage)
		}
		state.BudgetExhausted[issueID] = entry
		delete(state.Claimed, issueID)
		recordBudgetPass(ctx, params.Store, issueID, entry, log)
		if !blocked {
			if err := params.Store.DeleteRetryEntry(ctx, issueID); err != nil {
				log.Error("failed to delete retry entry after budget exhaustion",
					slog.Any("error", err),
				)
			}
		}
		blocked = true
	}

	// Held retries keep their attempt, error, and persisted row so the hold
	// costs no backoff and no budget; only the due time moves.
	if hold := evaluateSpendCeiling(state, spendCeilingParams{
		Ctx:      ctx,
		Store:    params.Store,
		Ceilings: params.SpendCeilings,
		Rates:    state.Rates,
		Now:      retryClock(params),
		Logger:   params.Logger,
	}); hold != SkipNone {
		if params.ReleaseHeld {
			delete(state.Claimed, issueID)
			log.Warn("retry held by spend ceiling, releasing claim",
				slog.String("reason", string(hold)),
				slog.Int("attempt", popped.Attempt),
			)
			return true
		}
		rearmHeld(time.Now().Add(time.Duration(params.PollIntervalMS) * time.Millisecond))
		if entry, ok := state.RetryAttempts[issueID]; ok {
			entry.noStateObserved = popped.noStateObserved
		}
		log.Debug("retry held by spend ceiling, rechecking",
			slog.String("reason", string(hold)),
			slog.Int("attempt", popped.Attempt),
			slog.Int64("delay_ms", int64(params.PollIntervalMS)),
		)
		return
	}

	// Effort budget gate: release the claim if completed sessions have
	// reached max_sessions. Runs before the tracker fetch.
	if params.MaxSessions > 0 {
		count, countErr := params.Store.CountRunHistoryByIssue(ctx, issueID)
		if countErr != nil {
			log.Warn("effort budget check failed, proceeding with dispatch",
				slog.Any("error", countErr),
			)
		} else if count >= params.MaxSessions {
			log.Warn("effort budget exhausted, blocking re-dispatch",
				slog.String("reason", budgetReasonSession),
				slog.Int("count", count),
				slog.Int("max_sessions", params.MaxSessions),
			)
			blockBudget(budgetReasonSession, count, nil)
		}
	}

	// Token and money gates share one usage read. They run after the session
	// gate, token before money, so an issue exhausted on several axes reports
	// the money budget, then the token budget; a query error fails open on
	// both axes, leaving any session block above intact.
	if params.MaxTokens > 0 || params.MaxCostUSD > 0 {
		usage, usageErr := params.Store.TokenUsageByIssue(ctx, issueID)
		if usageErr != nil {
			if params.MaxTokens > 0 {
				log.Warn("token budget check failed, proceeding with dispatch",
					slog.Any("error", usageErr),
				)
			}
			if params.MaxCostUSD > 0 {
				log.Warn("money budget check failed, proceeding with dispatch",
					slog.Any("error", usageErr),
				)
			}
		} else {
			if params.MaxTokens > 0 {
				if usage.UncachedTokens >= int64(params.MaxTokens) {
					log.Warn("token budget exhausted, blocking re-dispatch",
						slog.String("reason", budgetReasonToken),
						slog.Int64("used_tokens", usage.UncachedTokens),
						slog.Int("budget_tokens", params.MaxTokens),
						slog.Int("used_sessions", usage.Sessions),
						slog.Int("budget_sessions", params.MaxSessions),
					)
					blockBudget(budgetReasonToken, usage.Sessions, &usage)
				} else {
					if blocked {
						// Session gate already blocked; enrich the entry with the
						// token fields, mirroring the rebuild's enrichment.
						state.BudgetExhausted[issueID].setTokenUsage(usage)
					}
					if usage.SpendIncomplete() {
						warnTokenBudgetIncomplete(log, usage, params.MaxTokens)
					}
				}
			}
			if params.MaxCostUSD > 0 {
				used, status := pricing.IssueCost(recordedOf(usage), pricing.SessionCost{}, !usage.SpendIncomplete())
				if pricing.CeilingReached(used, params.MaxCostUSD) {
					log.Warn("money budget exhausted, blocking re-dispatch",
						slog.String("reason", budgetReasonMoney),
						slog.Float64("used_usd", used),
						slog.Float64("budget_usd", params.MaxCostUSD),
						slog.Int("used_sessions", usage.Sessions),
						slog.Int("budget_sessions", params.MaxSessions),
					)
					blockBudget(budgetReasonMoney, usage.Sessions, &usage)
					state.BudgetExhausted[issueID].setMoneyUsage(usage)
				} else {
					if blocked {
						state.BudgetExhausted[issueID].setMoneyUsage(usage)
					}
					if status != pricing.CostEstimated {
						warnMoneyBudgetIncomplete(log, usage, params.MaxCostUSD)
					}
				}
			}
		}
	}

	if blocked {
		held := state.BudgetExhausted[issueID]
		told, wasTold := state.BudgetAnnounced[issueID]
		if !wasTold || told.Reason != held.Reason {
			state.BudgetAnnounced[issueID] = BudgetAnnouncement{Reason: held.Reason, At: held.ExhaustedAt}
			metrics.IncBudgetExhaustions(held.Reason)
		}

		if state.BudgetHoldNoticed[issueID] != held.Reason {
			delivery := params.Router.Route(budgetHeldNotification(issueID, held))
			if !delivery.Empty() && budgetHoldNoticeAllowed(state, time.Now().UTC()) {
				postBudgetHoldNotice(state, budgetHoldNoticeParams{
					IssueID:  issueID,
					Entry:    held,
					Store:    params.Store,
					Delivery: delivery,
					Metrics:  metrics,
					Logger:   log,
					Ctx:      ctx,
				})
			}
		}
		return
	}

	// Re-validate with a single-issue fetch rather than a full candidate
	// sweep: one API request regardless of the tracker's active count.
	issue, err := params.TrackerAdapter.FetchIssueByID(ctx, issueID)
	if err != nil {
		var trackerErr *domain.TrackerError
		if errors.As(err, &trackerErr) && trackerErr.Kind == domain.ErrTrackerNotFound {
			log.Info("issue no longer exists in tracker, releasing claim")
			delete(state.Claimed, issueID)

			if delErr := params.Store.DeleteRetryEntry(ctx, issueID); delErr != nil {
				log.Error("failed to delete retry entry from store",
					slog.Any("error", delErr),
				)
			}
			return
		}

		// Transient error (transport, API, payload): reschedule the retry.
		nextAttempt, delayMS := putBackSchedule(popped, popped.Attempt+1, params.MaxRetryBackoffMS)

		log.Error("retry issue fetch failed, rescheduling",
			slog.Int("attempt", nextAttempt),
			slog.Int64("delay_ms", delayMS),
			slog.Any("error", err),
		)

		reschedule(nextAttempt, delayMS, "retry issue fetch failed")
		return
	}

	// reschedule reads popped when it runs, so every later reschedule of
	// this fire carries the identity. A title or link already held is never
	// replaced.
	if popped.IssueIdentity.isZero() {
		popped.DisplayID = issue.DisplayID
	}
	popped.IssueIdentity = popped.IssueIdentity.filledFrom(issueIdentityOf(issue))

	// A person's label edit can pass through zero state labels for a
	// moment, and a released retry loses its session and continuation
	// context, so the claim goes only at the second consecutive reading.
	// The stage hop record stays: reconcile decides on it.
	if issue.State == "" {
		if !confirmNoState(&popped.noStateObserved) {
			attempt, delayMS := putBackSchedule(popped, popped.Attempt, params.MaxRetryBackoffMS)
			log.Info("deferred retry for issue without a state label",
				slog.Int("attempt", attempt),
				slog.Int64("delay_ms", delayMS),
			)
			reschedule(attempt, delayMS, "issue without a state label")
			setNoStateObserved(true)
			return
		}

		log.Info("issue without a state label, releasing claim")
		delete(state.Claimed, issueID)

		if err := params.Store.DeleteRetryEntry(ctx, issueID); err != nil {
			log.Error("failed to delete retry entry from store",
				slog.Any("error", err),
			)
		}
		return
	}

	activeSet := stateSet(params.ActiveStates)
	terminalSet := stateSet(params.TerminalStates)
	normalizedState := strings.ToLower(issue.State)
	_, isActive := activeSet[normalizedState]
	_, isTerminal := terminalSet[normalizedState]
	isKnownReaction := isKnownReactionKind(popped.ReactionKind)
	isHandoff := params.HandoffState != "" && strings.EqualFold(issue.State, params.HandoffState)

	if popped.ReactionKind != "" && !isKnownReaction {
		log.Warn("found unknown reaction kind in retry entry",
			slog.String("kind", popped.ReactionKind),
			slog.String("issue_state", issue.State),
		)
	}

	// pausedReschedule bounds the two arms below that reschedule a known
	// reaction retry because the issue's state does not permit dispatch.
	// Unbounded, either arm would re-occupy the slot on every fire for the
	// process lifetime; the entry is dropped once its consecutive paused
	// dwell reaches pausedRetryMaxDwell.
	pausedReschedule := func(nextAttempt int, delayMS int64, reason string, logReschedule func()) {
		now := time.Now()
		pausedSince := now
		if popped.pausedSinceMS != 0 {
			pausedSince = time.UnixMilli(popped.pausedSinceMS)
		}
		dwell := now.Sub(pausedSince)
		if dwell >= pausedRetryMaxDwell {
			delete(state.Claimed, issueID)
			if err := params.Store.DeleteRetryEntry(ctx, issueID); err != nil {
				log.Error("failed to delete retry entry after paused-dwell drop",
					slog.Any("error", err),
				)
			}
			log.Warn("paused reaction retry exceeded max dwell, dropping",
				slog.String("kind", popped.ReactionKind),
				slog.String("issue_state", issue.State),
				slog.Int("attempt", popped.Attempt),
				slog.Int64("dwell_ms", dwell.Milliseconds()),
				slog.Int64("max_dwell_ms", pausedRetryMaxDwell.Milliseconds()),
			)
			return
		}
		logReschedule()
		reschedule(nextAttempt, delayMS, reason)
		if entry, ok := state.RetryAttempts[issueID]; ok {
			entry.pausedSinceMS = pausedSince.UnixMilli()
		}
	}

	if issue.ID == "" || issue.Identifier == "" || issue.Title == "" {
		log.Info("issue missing required fields, releasing claim")
		delete(state.Claimed, issueID)

		if err := params.Store.DeleteRetryEntry(ctx, issueID); err != nil {
			log.Error("failed to delete retry entry from store",
				slog.Any("error", err),
			)
		}
		return
	}

	switch {
	case isTerminal:
		resetStageHop(ctx, state, params.Store, issueID, stageResetTerminal, log)
	case !isActive:
		resetStageHop(ctx, state, params.Store, issueID, stageResetNotActive, log)
	}

	if isTerminal {
		log.Info("issue in terminal state, releasing claim",
			slog.String("issue_state", issue.State),
		)
		recordIssueOutcome(ctx, params.Store, terminalOutcome(issueID, issue.State, time.Now()), log)
		delete(state.Claimed, issueID)

		if err := params.Store.DeleteRetryEntry(ctx, issueID); err != nil {
			log.Error("failed to delete retry entry from store",
				slog.Any("error", err),
			)
		}
		return
	}

	if isActive {
		if isBlockedByNonTerminalSet(issue, terminalSet) {
			if isKnownReaction {
				nextAttempt, delayMS := putBackSchedule(popped, popped.Attempt+1, params.MaxRetryBackoffMS)

				pausedReschedule(nextAttempt, delayMS, "reaction retry paused by an open blocking issue", func() {
					log.Info("issue blocked, rescheduling reaction retry",
						slog.String("issue_state", issue.State),
						slog.String("kind", popped.ReactionKind),
						slog.Int("attempt", nextAttempt),
						slog.Int64("delay_ms", delayMS),
					)
				})
				return
			}

			log.Info("issue blocked, releasing claim",
				slog.String("issue_state", issue.State),
			)
			delete(state.Claimed, issueID)

			if err := params.Store.DeleteRetryEntry(ctx, issueID); err != nil {
				log.Error("failed to delete retry entry from store",
					slog.Any("error", err),
				)
			}
			return
		}
	} else if !isKnownReaction {
		log.Info("issue no longer in active state, releasing claim",
			slog.String("issue_state", issue.State),
		)
		delete(state.Claimed, issueID)

		if err := params.Store.DeleteRetryEntry(ctx, issueID); err != nil {
			log.Error("failed to delete retry entry from store",
				slog.Any("error", err),
			)
		}
		return
	} else if !isHandoff {
		nextAttempt, delayMS := putBackSchedule(popped, popped.Attempt+1, params.MaxRetryBackoffMS)

		pausedReschedule(nextAttempt, delayMS, "reaction retry paused by issue state", func() {
			log.Info("reaction retry paused by issue state, rescheduling",
				slog.String("issue_state", issue.State),
				slog.String("kind", popped.ReactionKind),
				slog.Int("attempt", nextAttempt),
				slog.Int64("delay_ms", delayMS),
			)
		})
		return
	}

	if !HasAvailableSlots(state, issue.State) {
		if popped.holdRearmed {
			log.Warn("no available orchestrator slots, rescheduling retry",
				slog.String("issue_state", issue.State),
				slog.Int("attempt", popped.Attempt),
				slog.Int64("delay_ms", int64(params.PollIntervalMS)),
			)
			rearmHeld(time.Now().Add(time.Duration(params.PollIntervalMS) * time.Millisecond))
			return
		}

		nextAttempt, delayMS := putBackSchedule(popped, popped.Attempt+1, params.MaxRetryBackoffMS)

		log.Warn("no available orchestrator slots, rescheduling retry",
			slog.String("issue_state", issue.State),
			slog.Int("attempt", nextAttempt),
			slog.Int64("delay_ms", delayMS),
		)

		reschedule(nextAttempt, delayMS, "no available orchestrator slots")
		return
	}

	var host string
	if params.HostPool != nil && params.HostPool.IsSSHEnabled() {
		var ok bool
		host, ok = params.HostPool.AcquireHost(issueID, popped.LastSSHHost)
		if !ok {
			if popped.holdRearmed {
				log.Warn("no available SSH hosts, rescheduling retry",
					slog.Int("attempt", popped.Attempt),
					slog.Int64("delay_ms", int64(params.PollIntervalMS)),
				)
				rearmHeld(time.Now().Add(time.Duration(params.PollIntervalMS) * time.Millisecond))
				return
			}

			nextAttempt, delayMS := putBackSchedule(popped, popped.Attempt+1, params.MaxRetryBackoffMS)

			log.Warn("no available SSH hosts, rescheduling retry",
				slog.Int("attempt", nextAttempt),
				slog.Int64("delay_ms", delayMS),
			)

			reschedule(nextAttempt, delayMS, "no available SSH hosts")
			return
		}
	}

	if params.MakeWorkerFn == nil {
		panic("HandleRetryTimer: nil MakeWorkerFn")
	}

	if params.AgentAdapterByKind == nil {
		panic("HandleRetryTimer: nil AgentAdapterByKind")
	}

	if params.ResolveAttemptSettings == nil {
		panic("HandleRetryTimer: nil ResolveAttemptSettings")
	}

	// Legacy retry rows persisted before dispatch rule routing carry an
	// empty AgentKind; coalesce to the workflow-wide default.
	agentKind := popped.AgentKind
	if agentKind == "" {
		agentKind = params.DefaultAgentKind
	}
	templateID := popped.TemplateID
	ruleName := popped.RuleName
	resumeSessionID := popped.SessionID

	if params.ResolveSelection != nil {
		frozen := DispatchResolution{AgentKind: agentKind, TemplateID: templateID, RuleName: ruleName}
		selection := params.ResolveSelection(frozen, issue)
		if selection.AgentKind != frozen.AgentKind || selection.TemplateID != frozen.TemplateID || selection.RuleName != frozen.RuleName {
			log.Info("retry dispatching on the selection the configuration in force gives it",
				slog.String("agent_kind", frozen.AgentKind),
				slog.String("template_id", frozen.TemplateID),
				slog.String("rule_name", frozen.RuleName),
				slog.String("dispatch_agent_kind", selection.AgentKind),
				slog.String("dispatch_template_id", selection.TemplateID),
				slog.String("dispatch_rule_name", selection.RuleName),
			)
		}
		resumeSessionID = resumeSessionFor(resumeSessionID, frozen, selection)
		agentKind, templateID, ruleName = selection.AgentKind, selection.TemplateID, selection.RuleName
	}

	if popped.dueBeforeRequestMS == 0 {
		configuredModel := popped.ConfiguredModel
		if params.ConfiguredModelOf != nil {
			configuredModel = params.ConfiguredModelOf(DispatchResolution{AgentKind: agentKind, TemplateID: templateID, RuleName: ruleName})
		}
		ownRetry := popped.OriginTrigger == triggerUsageLimit
		grace := time.Duration(params.PollIntervalMS) * time.Millisecond
		if _, _, release, held := usageLimitGate(state, agentKind, configuredModel, ownRetry, grace, retryClock(params)); held {
			if params.HostPool != nil && host != "" {
				params.HostPool.ReleaseHost(issueID)
			}
			if params.ReleaseHeld {
				delete(state.Claimed, issueID)
				log.Warn("retry held by usage limit, releasing claim",
					slog.String("agent_kind", agentKind),
					slog.Time("release_at", release),
					slog.Int("attempt", popped.Attempt),
				)
				return true
			}
			rearmHeld(release)
			log.Debug("retry held by usage limit, rechecking at release",
				slog.String("agent_kind", agentKind),
				slog.Time("release_at", release),
			)
			return
		}
	}

	adapter, adapterErr := params.AgentAdapterByKind(agentKind)
	if adapterErr != nil {
		nextAttempt, delayMS := putBackSchedule(popped, popped.Attempt+1, params.MaxRetryBackoffMS)

		log.Error("retry agent kind unavailable",
			slog.String("rule_name", ruleName),
			slog.String("agent_kind", agentKind),
			slog.Int("attempt", nextAttempt),
			slog.Int64("delay_ms", delayMS),
			slog.Any("error", adapterErr),
		)
		if params.HostPool != nil && host != "" {
			params.HostPool.ReleaseHost(issueID)
		}
		reschedule(nextAttempt, delayMS, "retry agent kind unavailable")
		return
	}

	attemptSettings := params.ResolveAttemptSettings(DispatchResolution{AgentKind: agentKind, TemplateID: templateID, RuleName: ruleName}, host)
	if len(attemptSettings.Refusals) > 0 {
		nextAttempt, delayMS := putBackSchedule(popped, popped.Attempt+1, params.MaxRetryBackoffMS)

		log.Error("retry agent settings refused",
			slog.Any("error", PreflightResult{Errors: attemptSettings.Refusals}),
			slog.String("rule_name", ruleName),
			slog.String("agent_kind", agentKind),
			slog.String("check", attemptSettings.Refusals[0].Check),
			slog.String("diagnostic", attemptSettings.Refusals[0].Message),
			slog.Int("attempt", nextAttempt),
			slog.Int64("delay_ms", delayMS),
		)
		if params.HostPool != nil && host != "" {
			params.HostPool.ReleaseHost(issueID)
		}
		reschedule(nextAttempt, delayMS, "retry agent settings refused")
		return
	}

	if ruleName == popped.RuleName && popped.RuleSettingsApplied && attemptSettings.Settings.RuleName == "" {
		log.Info("rule settings no longer present, attempt runs on the kind's top-level settings",
			slog.String("rule_name", ruleName),
			slog.String("agent_kind", agentKind),
		)
	}

	// NextAttempt increments only on the next worker exit, not at dispatch,
	// so pass the popped attempt as-is.
	attempt := popped.Attempt
	dispatchCtx := ctx
	if popped.ContinuationContext != nil {
		dispatchCtx = WithContinuationContext(ctx, popped.ContinuationContext)
	}
	if popped.UsageLimitNotified {
		dispatchCtx = withUsageLimitNotified(dispatchCtx)
	}
	lineage := popped.StageLineage
	if ruleName != popped.RuleName {
		lineage = freshLineage(state, issueID, ruleName)
	}
	lineage = withChainID(lineage)
	dispatchCtx = withStageRender(dispatchCtx, prompt.StageContext{
		Current:         stageCurrent(params.Dispatch, ruleName),
		Previous:        lineage.PreviousRule,
		PreviousOutcome: lineage.PreviousOutcome,
	})
	DispatchIssue(dispatchCtx, state, issue, &attempt, host, params.MakeWorkerFn(resumeSessionID, host, agentKind, templateID, popped.ReactionKind, adapter, attemptSettings))
	if entry := state.Running[issue.ID]; entry != nil {
		entry.WorkflowFile = params.WorkflowFile
		entry.AgentKind = agentKind
		entry.RuleName = ruleName
		entry.StageLineage = lineage
		startPass(entry)
		markStageHopDispatched(ctx, state, params.Store, issueID, log)
		entry.RuleSettingsApplied = attemptSettings.Settings.RuleName != ""
		entry.ConfiguredModel = attemptSettings.Settings.Model
		entry.ConfiguredEffort = attemptSettings.Settings.Effort
		entry.TemplateID = templateID
		entry.ContinuationContext = popped.ContinuationContext
		entry.ReactionKind = popped.ReactionKind
		entry.Trigger = popped.OriginTrigger
		entry.UsageLimitNotified = popped.UsageLimitNotified
		entry.Continuation = popped.Continuation
		entry.UsageArrival, entry.UsageAttribution = attemptSettings.UsageArrival, attemptSettings.UsageAttribution
		entry.CarriesTokenFigure = attemptSettings.CarriesTokenFigure
		freezeIssueTokenBaseline(ctx, state, issueID, params.Store, params.Logger)
		recordRunningPass(ctx, params.Store, issueID, entry, log)
	}
	metrics.IncDispatches(outcomeSuccess)

	// Mark the reaction fingerprint dispatched only after dispatch
	// succeeds. Non-reaction retries have no fingerprint row.
	if isKnownReaction {
		if err := params.Store.MarkReactionDispatched(ctx, issueID, popped.ReactionKind); err != nil {
			log.Warn("failed to mark reaction dispatched after retry dispatch",
				slog.String("kind", popped.ReactionKind),
				slog.Any("error", err),
			)
		}
	}

	log.Info("retried issue dispatched",
		slog.Int("attempt", attempt),
		slog.String("agent_kind", agentKind),
		slog.String("rule_name", ruleName),
		slog.String("model", attemptSettings.Settings.Model),
		slog.String("effort", attemptSettings.Settings.Effort),
	)

	// DispatchIssue's CancelRetry clears the in-memory entry, but the
	// SQLite row persisted by worker exit must be cleaned up too.
	if err := params.Store.DeleteRetryEntry(ctx, issueID); err != nil {
		log.Error("failed to delete retry entry from store after dispatch",
			slog.Any("error", err),
		)
	}
	return false
}

// isStaleRetryTimer reports whether the entry belongs to a newer
// ScheduleRetry than the timer that just fired. The monotonic-clock check
// via time.Since is immune to wall-clock adjustments. A zero scheduledAt
// (startup-reconstructed entry) is never stale: it has no predecessor to
// race with.
func isStaleRetryTimer(entry *RetryEntry) bool {
	if !entry.scheduledAt.IsZero() {
		return time.Since(entry.scheduledAt) < time.Duration(entry.scheduledDelayMS)*time.Millisecond
	}
	return false
}

// retryEntryRow returns the persisted form of entry. Labels are copied and
// a nil Labels stays nil.
func retryEntryRow(entry *RetryEntry) persistence.RetryEntry {
	return persistence.RetryEntry{
		IssueID:     entry.IssueID,
		Identifier:  entry.Identifier,
		DisplayID:   entry.DisplayID,
		IssueTitle:  entry.IssueIdentity.Title,
		IssueURL:    entry.IssueIdentity.URL,
		IssueType:   entry.IssueIdentity.IssueType,
		IssueLabels: slices.Clone(entry.IssueIdentity.Labels),
		IssueState:  entry.IssueIdentity.State,
		Attempt:     entry.Attempt,
		DueAtMs:     entry.DueAtMS,
		Error:       stringPtr(entry.Error),
		SessionID:   stringPtr(entry.SessionID),
		RuleName:    entry.RuleName,
		TemplateID:  entry.TemplateID,
		AgentKind:   entry.AgentKind,
		Trigger:     entry.Trigger,

		OriginTrigger:        entry.OriginTrigger,
		UsageLimitNotified:   entry.UsageLimitNotified,
		ConfiguredModel:      entry.ConfiguredModel,
		StagePrevious:        entry.StageLineage.PreviousRule,
		StagePreviousOutcome: entry.StageLineage.PreviousOutcome,
		ChainID:              entry.StageLineage.ChainID,
		DispatchedAt:         formatDispatchedAt(entry.StageLineage.DispatchedAt),
	}
}

// latestParkHandOver reads the issue's newest agent run and derives the
// hand-over a park taken now keeps from it. A failed read logs and yields
// the zero hand-over, since the park must proceed whatever the store says.
func latestParkHandOver(ctx context.Context, store RetryTimerStore, issueID string, log *slog.Logger) parkHandOver {
	runs, err := store.LatestAgentRuns(ctx, []string{issueID})
	if err != nil {
		log.Warn("park dispatch context read failed", slog.Any("error", err))
		return parkHandOver{}
	}
	return parkHandOverOf(runs[issueID])
}

// resumeSessionFor returns sessionID when a run on the ran selection may
// continue it under the selection in force, which holds when both name the
// same agent kind and template, and "" otherwise. The rule name does not
// matter: a rule that picks the same kind and template keeps the session
// usable.
func resumeSessionFor(sessionID string, ran, inForce DispatchResolution) string {
	if ran.AgentKind != inForce.AgentKind || ran.TemplateID != inForce.TemplateID {
		return ""
	}
	return sessionID
}

// persistRetryEntry saves the in-memory retry entry for issueID to SQLite.
// Errors are logged but do not block in-memory state transitions.
func persistRetryEntry(ctx context.Context, log *slog.Logger, store RetryTimerStore, state *State, issueID string) {
	retryEntry, ok := state.RetryAttempts[issueID]
	if !ok {
		return
	}

	pEntry := retryEntryRow(retryEntry)
	if err := store.SaveRetryEntry(ctx, pEntry); err != nil {
		log.Error("failed to persist retry entry",
			slog.Any("error", err),
		)
	}
}
