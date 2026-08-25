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
)

// pausedRetryMaxDwell bounds how long a known-reaction retry may be
// rescheduled consecutively because the issue's own state does not permit
// a dispatch, before [HandleRetryTimer] drops it instead of holding the
// retry slot for the process lifetime. Matches the 30-minute pending-entry
// TTLs the reaction reconcile passes already use, because a paused retry
// is the retry-slot analogue of a pending reaction that never becomes
// actionable.
const pausedRetryMaxDwell = 30 * time.Minute

// RetryTimerStore is the persistence interface required by
// [HandleRetryTimer]. It is satisfied by [persistence.Store] in production
// and by test doubles in unit tests.
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
// that are not part of the core [State]. This separates pure state mutation
// from I/O side effects (tracker API calls and SQLite persistence).
type HandleRetryTimerParams struct {
	// Store is the SQLite persistence layer. Used to persist re-scheduled
	// retry entries and to delete entries when the claim is released or
	// dispatch succeeds.
	Store RetryTimerStore

	// TrackerAdapter fetches candidate issues used to validate the retried
	// issue is still active and eligible.
	TrackerAdapter domain.TrackerAdapter

	// ActiveStates is the current list of configured active issue states.
	// Used by HandleRetryTimer to validate that the retried issue's current
	// state is still active after the single-issue fetch.
	ActiveStates []string

	// TerminalStates is the current list of configured terminal issue
	// states. Used to evaluate the blocker rule via
	// [IsBlockedByNonTerminal] before dispatch.
	TerminalStates []string

	// HandoffState is the configured tracker handoff state. Known
	// reaction retries (ReactionKindCI, ReactionKindReview) may dispatch
	// while the fetched issue equals this state. Empty means no
	// handoff-state retry eligibility applies.
	HandoffState string

	// MaxRetryBackoffMS is the configured cap for exponential backoff
	// delay (from config.Agent.MaxRetryBackoffMS). Used when re-scheduling
	// a retry after fetch failure or slot exhaustion.
	MaxRetryBackoffMS int

	// MakeWorkerFn constructs a [WorkerFunc] for the given resume
	// session ID, SSH host, dispatch-frozen agent kind and template
	// ID, dispatch reaction kind, and resolved adapter. The retry
	// handler resolves the adapter through AgentAdapterByKind before
	// invoking this constructor; the closure no longer performs adapter
	// lookup itself. The reaction kind selects the read-only worker
	// posture for label-review dispatches.
	MakeWorkerFn func(resumeSessionID, sshHost, agentKind, templateID, reactionKind string, adapter domain.AgentAdapter) WorkerFunc

	// AgentAdapterByKind resolves the agent adapter for the given
	// kind. Required when MakeWorkerFn is set. Returns a wrapped
	// *registry.RegistryError when the kind is unknown; the retry
	// handler logs the error, releases the claim, and deletes the
	// persisted retry row.
	AgentAdapterByKind func(kind string) (domain.AgentAdapter, error)

	// DefaultAgentKind is the workflow-wide default agent kind used
	// when a popped retry entry carries an empty AgentKind (legacy
	// rows persisted before dispatch rule routing was added). The
	// retry handler coalesces the empty value to this default before
	// adapter resolution, worker construction, and running-entry
	// assignment so downstream state reflects the actual kind used.
	DefaultAgentKind string

	// OnRetryFire is the callback for re-scheduled retry timers.
	// Routes back into the event loop.
	OnRetryFire func(issueID string)

	// Ctx is the context for tracker API calls and persistence operations.
	Ctx context.Context

	// Logger is the structured logger with orchestrator context.
	Logger *slog.Logger

	// MaxSessions is the configured per-issue effort budget (from
	// config.Agent.MaxSessions). When > 0, HandleRetryTimer counts
	// completed sessions for the issue from run_history and releases
	// the claim instead of dispatching if the count has reached the
	// budget. When 0, no budget is enforced.
	MaxSessions int

	// MaxConsecutiveAbsences is the configured consecutive
	// handoff-absence ceiling (from
	// config.Agent.MaxConsecutiveAbsences).
	MaxConsecutiveAbsences int

	// HandoffParkingLabel is the review-comments escalation label captured
	// at orchestrator construction. Empty falls back to "needs-human".
	HandoffParkingLabel string

	// HandoffEvidencePolicy is the configured evidence policy. Under the
	// off policy no absence is ever recorded, so the absence gate is not
	// consulted. The zero value applies the default policy.
	HandoffEvidencePolicy config.HandoffEvidencePolicy

	// MaxTokens is the configured per-issue token budget (from
	// config.Agent.MaxTokens). When > 0, HandleRetryTimer sums
	// run_history total_tokens for the issue and releases the claim
	// instead of dispatching if the sum has reached the budget. When 0,
	// no token budget is enforced.
	MaxTokens int

	// Metrics records instrumentation counters for retry timer events.
	// If nil, defaults to [domain.NoopMetrics].
	Metrics domain.Metrics

	// HostPool is the SSH host pool for host acquisition on retry.
	// May be nil (local mode).
	HostPool *HostPool

	// WorkflowFile is the base filename of the active WORKFLOW.md file.
	// Recorded on the RunningEntry for observability.
	WorkflowFile string
}

// HandleRetryTimer processes a retry timer event for the given issue.
// It removes the retry entry, re-fetches candidate issues, locates and
// validates the issue, checks worker slot availability, and either
// dispatches the issue, reschedules the retry, or releases the claim.
// It must be called from the orchestrator's single-writer event loop.
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

	// Pop the retry entry. If missing, the timer raced with a cancellation
	// or a subsequent ScheduleRetry — no-op. If the entry was replaced by
	// a newer ScheduleRetry after this timer's goroutine was already
	// enqueued, skip to let the replacement timer fire at the correct time.
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

	// Guard: if the cancelled worker has not yet exited, the issue is
	// still in the Running map. Re-scheduling a dispatch would overwrite
	// the running entry and spawn a duplicate worker. Reschedule the
	// retry with the same attempt so it fires again after the exit is
	// processed.
	if _, running := state.Running[issueID]; running {
		delayMS := computeBackoffDelay(popped.Attempt, params.MaxRetryBackoffMS)
		log.Debug("worker still running, rescheduling retry",
			slog.Int("attempt", popped.Attempt),
		)
		reschedule(popped.Attempt, delayMS, popped.Error)
		return
	}

	// The retry lane carries two decisions ahead of any tracker fetch or
	// worker dispatch, both exempt for a known reaction continuation: an
	// already-parked issue is refused regardless of evidence policy (the
	// park gate is policy-independent), and an issue whose absence count
	// has just reached the ceiling is parked, skipped under the off policy
	// because that policy records no absence at all.
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
	// whose budget is exhausted, recording the firing ceiling's reason and
	// numbers. The block is identical whichever ceiling triggers it; only
	// the recorded entry differs. The token gate may call this after the
	// session gate already did, overwriting the entry with the token
	// budget so an issue exhausted on both axes reports the token budget.
	// ExhaustedAt is sourced from the announcement memory when it already
	// knows this issue under the same reason, so a hold that survives a
	// gap in candidacy keeps reporting when it began.
	blocked := false
	blockBudget := func(reason string, usedSessions int, usedTokens *int64, unmeasuredSessions *int) {
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

	// Effort budget gate: when max_sessions > 0, count completed sessions
	// for this issue and release the claim if the budget is exhausted.
	// Runs before the tracker fetch to avoid a wasted network call.
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
			blockBudget(budgetReasonSession, count, nil, nil)
		}
	}

	// Token budget gate: when max_tokens > 0, sum completed-session tokens
	// for this issue and release the claim if the budget is exhausted.
	// Evaluated after the session gate so that an issue exhausted on both
	// axes reports the token budget. A token-query error fails open on the
	// token axis only: a session block already recorded above still holds.
	// Runs before the tracker fetch.
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
			blockBudget(budgetReasonToken, usage.Sessions, &usage.TotalTokens, &usage.UnmeasuredSessions)
		} else {
			if blocked {
				// The session gate already blocked; the token axis was
				// evaluated for this issue on this pass, so enrich the
				// entry with both token fields, mirroring the rebuild's
				// own enrichment.
				state.BudgetExhausted[issueID].UsedTokens = &usage.TotalTokens
				state.BudgetExhausted[issueID].UnmeasuredSessions = &usage.UnmeasuredSessions
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

	// Re-validate the issue with a single tracker API call instead of a
	// full candidate sweep. FetchIssueByID costs one API request regardless
	// of how many active issues the tracker holds.
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
	// reaction retry because the issue's current state does not permit a
	// dispatch. Left unbounded, either arm re-occupies the slot on every
	// fire and holds it for the process lifetime; pausedSince tracks how
	// long the entry has taken one of these arms consecutively, and the
	// entry is dropped rather than rescheduled once that dwell reaches
	// pausedRetryMaxDwell.
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

	// Check slot availability for the issue's state.
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

	// Acquire SSH host with preference from the previous attempt.
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

	// Legacy retry rows persisted before dispatch rule routing was
	// added carry an empty AgentKind. Coalesce to the workflow-wide
	// default so adapter resolution, worker construction, and the
	// running-entry record below all observe a concrete kind. The
	// one-shot audit log is emitted at recovery time by PopulateRetries.
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

	// Dispatch the issue with the popped entry's attempt number.
	// Pass the popped attempt as-is; NextAttempt increments only on the
	// next worker exit, not at dispatch time.
	attempt := popped.Attempt
	dispatchCtx := ctx
	if popped.ContinuationContext != nil {
		dispatchCtx = WithContinuationContext(ctx, popped.ContinuationContext)
	}
	DispatchIssue(dispatchCtx, state, issue, &attempt, host, params.MakeWorkerFn(popped.SessionID, host, agentKind, popped.TemplateID, popped.ReactionKind, adapter))
	if entry := state.Running[issue.ID]; entry != nil {
		entry.WorkflowFile = params.WorkflowFile
		entry.AgentKind = agentKind
		entry.RuleName = popped.RuleName
		entry.TemplateID = popped.TemplateID
		entry.ContinuationContext = popped.ContinuationContext
		entry.ReactionKind = popped.ReactionKind
	}
	metrics.IncDispatches(outcomeSuccess)

	// Mark the reaction fingerprint dispatched only after actual dispatch
	// succeeds. Scoped to reaction-triggered retries; non-reaction retries
	// have no fingerprint row to update.
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

	// Defense-in-depth: delete the SQLite row persisted by worker exit.
	// DispatchIssue calls CancelRetry which clears the in-memory entry,
	// but the persisted row must also be cleaned up.
	if err := params.Store.DeleteRetryEntry(ctx, issueID); err != nil {
		log.Error("failed to delete retry entry from store after dispatch",
			slog.Any("error", err),
		)
	}
}

// isStaleRetryTimer reports whether the entry belongs to a newer
// ScheduleRetry call than the timer that just fired. When scheduledAt is
// set (entries created by [ScheduleRetry]), the check uses Go's monotonic
// clock via time.Since, making it immune to wall-clock adjustments (NTP,
// suspend/resume). When scheduledAt is zero (entries reconstructed from
// SQLite at startup), the timer is always treated as non-stale because
// startup-reconstructed entries have no stale predecessor to race with.
func isStaleRetryTimer(entry *RetryEntry) bool {
	if !entry.scheduledAt.IsZero() {
		return time.Since(entry.scheduledAt) < time.Duration(entry.scheduledDelayMS)*time.Millisecond
	}
	return false
}

// persistRetryEntry saves the current in-memory retry entry for issueID to
// SQLite. Errors are logged but do not prevent in-memory state transitions.
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
