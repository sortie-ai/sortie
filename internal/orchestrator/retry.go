package orchestrator

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/registry"
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
	CountRunHistoryByIssue(ctx context.Context, issueID string) (int, error)
	QueryConsecutiveHandoffAbsenceCounts(ctx context.Context, issueIDs []string) (map[string]int, error)
	TokenUsageByIssue(ctx context.Context, issueID string) (persistence.IssueTokenUsage, error)
	MarkReactionDispatched(ctx context.Context, issueID, kind string) error
	UpsertParkedIssue(ctx context.Context, entry persistence.ParkedIssue) error
	DeleteParkedIssue(ctx context.Context, issueID string) error
	ResetHandoffAbsenceSequence(ctx context.Context, issueID string) error
	UpsertBudgetHoldNotice(ctx context.Context, notice persistence.BudgetHoldNotice) error
}

// HandleRetryTimerParams holds the dependencies for [HandleRetryTimer]
// that are not part of the core [State], separating pure state mutation
// from I/O side effects.
type HandleRetryTimerParams struct {
	Store RetryTimerStore

	TrackerAdapter domain.TrackerAdapter

	ActiveStates []string

	TerminalStates []string

	// HandoffState is the configured tracker handoff state. Known reaction
	// retries may dispatch while the fetched issue equals it. Empty means
	// no handoff-state retry eligibility applies.
	HandoffState string

	MaxRetryBackoffMS int

	// MakeWorkerFn constructs a [WorkerFunc]. The retry handler resolves
	// the adapter through AgentAdapterByKind before invoking it; the
	// reaction kind selects the read-only worker posture for label-review
	// dispatches.
	MakeWorkerFn func(resumeSessionID, sshHost, agentKind, templateID, reactionKind string, adapter domain.AgentAdapter, usageArrival registry.UsageArrival) WorkerFunc

	// AgentAdapterByKind resolves the agent adapter for the given kind.
	// Required when MakeWorkerFn is set. On an unknown kind the retry
	// handler logs, releases the claim, and deletes the persisted row.
	AgentAdapterByKind func(kind string) (domain.AgentAdapter, error)

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

	// Metrics records retry timer instrumentation. If nil, defaults to
	// [domain.NoopMetrics].
	Metrics domain.Metrics

	// HostPool is the SSH host pool. May be nil (local mode).
	HostPool *HostPool

	// WorkflowFile is the base filename of the active WORKFLOW.md file.
	WorkflowFile string

	// ResolveUsageDisposition resolves the usage-reporting disposition for
	// the agent kind and SSH host (empty for a local launch). Required;
	// the resolved pair is frozen onto the running entry alongside
	// AgentKind.
	ResolveUsageDisposition func(kind, sshHost string) (registry.UsageArrival, registry.UsageAttribution)
}

// HandleRetryTimer processes a retry timer event for the given issue: it
// removes the retry entry, re-fetches and validates the issue, checks slot
// availability, and either dispatches, reschedules, or releases the claim.
// Must be called from the single-writer event loop.
func HandleRetryTimer(state *State, issueID string, params HandleRetryTimerParams) {
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

	reschedule := func(attempt int, delayMS int64, retryErr string) {
		ScheduleRetry(state, ScheduleRetryParams{
			IssueID:             issueID,
			Identifier:          popped.Identifier,
			DisplayID:           popped.DisplayID,
			Attempt:             attempt,
			DelayMS:             delayMS,
			Error:               retryErr,
			LastSSHHost:         popped.LastSSHHost,
			SessionID:           popped.SessionID,
			ContinuationContext: popped.ContinuationContext,
			ReactionKind:        popped.ReactionKind,
			AgentKind:           popped.AgentKind,
			RuleName:            popped.RuleName,
			TemplateID:          popped.TemplateID,
			Logger:              log,
		}, params.OnRetryFire)
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

	// The cancelled worker may not yet have exited, leaving the issue in
	// Running; re-dispatching would overwrite the entry and spawn a
	// duplicate. Reschedule at the same attempt to fire after the exit.
	if _, running := state.Running[issueID]; running {
		delayMS := computeBackoffDelay(popped.Attempt, params.MaxRetryBackoffMS)
		log.Debug("worker still running, rescheduling retry",
			slog.Int("attempt", popped.Attempt),
		)
		reschedule(popped.Attempt, delayMS, popped.Error)
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
	// whose budget is exhausted. The token gate may call this after the
	// session gate did, overwriting the entry so an issue exhausted on both
	// axes reports the token budget. ExhaustedAt is taken from the
	// announcement memory when it already knows this issue under the same
	// reason, so a hold surviving a gap in candidacy keeps its onset time.
	blocked := false
	blockBudget := func(reason string, usedSessions int, usedTokens *int64, unmeasuredSessions *int, stoppedInFlight *int) {
		at := time.Now().UTC()
		if told, wasTold := state.BudgetAnnounced[issueID]; wasTold && told.Reason == reason {
			at = told.At
		}
		state.BudgetExhausted[issueID] = &BudgetExhaustedEntry{
			Identifier:         popped.Identifier,
			DisplayID:          popped.DisplayID,
			Reason:             reason,
			UsedSessions:       usedSessions,
			BudgetSessions:     params.MaxSessions,
			UsedTokens:         usedTokens,
			BudgetTokens:       int64(params.MaxTokens),
			UnmeasuredSessions: unmeasuredSessions,
			StoppedInFlight:    stoppedInFlight,
			ExhaustedAt:        at,
		}
		delete(state.Claimed, issueID)
		if !blocked {
			if err := params.Store.DeleteRetryEntry(ctx, issueID); err != nil {
				log.Error("failed to delete retry entry after budget exhaustion",
					slog.Any("error", err),
				)
			}
		}
		blocked = true
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
			blockBudget(budgetReasonSession, count, nil, nil, nil)
		}
	}

	// Token budget gate: release the claim if summed tokens have reached
	// max_tokens. Evaluated after the session gate so an issue exhausted on
	// both axes reports the token budget; a query error fails open on the
	// token axis only, leaving any session block above intact.
	if params.MaxTokens > 0 {
		usage, usageErr := params.Store.TokenUsageByIssue(ctx, issueID)
		if usageErr != nil {
			log.Warn("token budget check failed, proceeding with dispatch",
				slog.Any("error", usageErr),
			)
		} else if usage.TotalTokens >= int64(params.MaxTokens) {
			log.Warn("token budget exhausted, blocking re-dispatch",
				slog.String("reason", budgetReasonToken),
				slog.Int64("used_tokens", usage.TotalTokens),
				slog.Int("budget_tokens", params.MaxTokens),
				slog.Int("used_sessions", usage.Sessions),
				slog.Int("budget_sessions", params.MaxSessions),
			)
			blockBudget(budgetReasonToken, usage.Sessions, &usage.TotalTokens, &usage.UnmeasuredSessions, &usage.StoppedInFlight)
		} else {
			if blocked {
				// Session gate already blocked; enrich the entry with both
				// token fields, mirroring the rebuild's enrichment.
				state.BudgetExhausted[issueID].UsedTokens = &usage.TotalTokens
				state.BudgetExhausted[issueID].UnmeasuredSessions = &usage.UnmeasuredSessions
				state.BudgetExhausted[issueID].StoppedInFlight = &usage.StoppedInFlight
			}
			if usage.UnmeasuredSessions > 0 {
				log.Warn("token budget cannot be fully evaluated, allowing dispatch",
					slog.Int64("used_tokens", usage.TotalTokens),
					slog.Int64("budget_tokens", int64(params.MaxTokens)),
					slog.Int("unmeasured_sessions", usage.UnmeasuredSessions),
				)
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

		if params.TrackerAdapter != nil &&
			state.BudgetHoldNoticed[issueID] != held.Reason &&
			budgetHoldNoticeAllowed(state, time.Now().UTC()) {
			postBudgetHoldNotice(state, budgetHoldNoticeParams{
				IssueID:        issueID,
				Entry:          held,
				Store:          params.Store,
				TrackerAdapter: params.TrackerAdapter,
				Metrics:        metrics,
				Logger:         log,
				Ctx:            ctx,
			})
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
		nextAttempt := popped.Attempt + 1
		delayMS := computeBackoffDelay(nextAttempt, params.MaxRetryBackoffMS)

		log.Error("retry issue fetch failed, rescheduling",
			slog.Int("attempt", nextAttempt),
			slog.Int64("delay_ms", delayMS),
			slog.Any("error", err),
		)

		reschedule(nextAttempt, delayMS, "retry issue fetch failed")
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
	pausedReschedule := func(nextAttempt int, delayMS int64, logReschedule func()) {
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
		reschedule(nextAttempt, delayMS, popped.Error)
		if entry, ok := state.RetryAttempts[issueID]; ok {
			entry.pausedSinceMS = pausedSince.UnixMilli()
		}
	}

	if issue.ID == "" || issue.Identifier == "" || issue.Title == "" || issue.State == "" {
		log.Info("issue missing required fields, releasing claim")
		delete(state.Claimed, issueID)

		if err := params.Store.DeleteRetryEntry(ctx, issueID); err != nil {
			log.Error("failed to delete retry entry from store",
				slog.Any("error", err),
			)
		}
		return
	}

	if isTerminal {
		log.Info("issue in terminal state, releasing claim",
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

	if isActive {
		if isBlockedByNonTerminalSet(issue, terminalSet) {
			if isKnownReaction {
				nextAttempt := popped.Attempt + 1
				delayMS := computeBackoffDelay(nextAttempt, params.MaxRetryBackoffMS)

				pausedReschedule(nextAttempt, delayMS, func() {
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
		nextAttempt := popped.Attempt + 1
		delayMS := computeBackoffDelay(nextAttempt, params.MaxRetryBackoffMS)

		pausedReschedule(nextAttempt, delayMS, func() {
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
		nextAttempt := popped.Attempt + 1
		delayMS := computeBackoffDelay(nextAttempt, params.MaxRetryBackoffMS)

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
			nextAttempt := popped.Attempt + 1
			delayMS := computeBackoffDelay(nextAttempt, params.MaxRetryBackoffMS)

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

	if params.ResolveUsageDisposition == nil {
		panic("HandleRetryTimer: nil ResolveUsageDisposition")
	}

	// Legacy retry rows persisted before dispatch rule routing carry an
	// empty AgentKind; coalesce to the workflow-wide default.
	agentKind := popped.AgentKind
	if agentKind == "" {
		agentKind = params.DefaultAgentKind
	}

	adapter, adapterErr := params.AgentAdapterByKind(agentKind)
	if adapterErr != nil {
		log.Error("retry agent kind unavailable",
			slog.String("rule_name", popped.RuleName),
			slog.String("agent_kind", agentKind),
			slog.Any("error", adapterErr),
		)
		delete(state.Claimed, issueID)
		if delErr := params.Store.DeleteRetryEntry(ctx, issueID); delErr != nil {
			log.Error("failed to delete retry entry after agent kind lookup failure",
				slog.Any("error", delErr),
			)
		}
		if params.HostPool != nil && host != "" {
			params.HostPool.ReleaseHost(issueID)
		}
		metrics.IncRetries(triggerError)
		return
	}

	// NextAttempt increments only on the next worker exit, not at dispatch,
	// so pass the popped attempt as-is.
	attempt := popped.Attempt
	dispatchCtx := ctx
	if popped.ContinuationContext != nil {
		dispatchCtx = WithContinuationContext(ctx, popped.ContinuationContext)
	}
	arrival, attribution := params.ResolveUsageDisposition(agentKind, host)
	DispatchIssue(dispatchCtx, state, issue, &attempt, host, params.MakeWorkerFn(popped.SessionID, host, agentKind, popped.TemplateID, popped.ReactionKind, adapter, arrival))
	if entry := state.Running[issue.ID]; entry != nil {
		entry.WorkflowFile = params.WorkflowFile
		entry.AgentKind = agentKind
		entry.RuleName = popped.RuleName
		entry.TemplateID = popped.TemplateID
		entry.ContinuationContext = popped.ContinuationContext
		entry.ReactionKind = popped.ReactionKind
		entry.UsageArrival, entry.UsageAttribution = arrival, attribution
		freezeIssueTokenBaseline(ctx, state, issueID, params.Store, params.Logger)
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
	)

	// DispatchIssue's CancelRetry clears the in-memory entry, but the
	// SQLite row persisted by worker exit must be cleaned up too.
	if err := params.Store.DeleteRetryEntry(ctx, issueID); err != nil {
		log.Error("failed to delete retry entry from store after dispatch",
			slog.Any("error", err),
		)
	}
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

// persistRetryEntry saves the in-memory retry entry for issueID to SQLite.
// Errors are logged but do not block in-memory state transitions.
func persistRetryEntry(ctx context.Context, log *slog.Logger, store RetryTimerStore, state *State, issueID string) {
	retryEntry, ok := state.RetryAttempts[issueID]
	if !ok {
		return
	}

	pEntry := persistence.RetryEntry{
		IssueID:    retryEntry.IssueID,
		Identifier: retryEntry.Identifier,
		Attempt:    retryEntry.Attempt,
		DueAtMs:    retryEntry.DueAtMS,
		Error:      stringPtr(retryEntry.Error),
		SessionID:  stringPtr(retryEntry.SessionID),
		RuleName:   retryEntry.RuleName,
		TemplateID: retryEntry.TemplateID,
		AgentKind:  retryEntry.AgentKind,
	}
	if err := store.SaveRetryEntry(ctx, pEntry); err != nil {
		log.Error("failed to persist retry entry",
			slog.Any("error", err),
		)
	}
}
