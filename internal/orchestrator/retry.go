package orchestrator

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/persistence"
)

// RetryTimerStore is the persistence interface required by
// [HandleRetryTimer]. It is satisfied by [persistence.Store] in production
// and by test doubles in unit tests.
type RetryTimerStore interface {
	SaveRetryEntry(ctx context.Context, entry persistence.RetryEntry) error
	DeleteRetryEntry(ctx context.Context, issueID string) error
	CountRunHistoryByIssue(ctx context.Context, issueID string) (int, error)
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
	// Not used directly by HandleRetryTimer because presence in the
	// candidate set returned by FetchCandidateIssues implies active state.
	ActiveStates []string

	// TerminalStates is the current list of configured terminal issue
	// states. Used to evaluate the blocker rule via
	// [IsBlockedByNonTerminal] before dispatch.
	TerminalStates []string

	// MaxRetryBackoffMS is the configured cap for exponential backoff
	// delay (from config.Agent.MaxRetryBackoffMS). Used when re-scheduling
	// a retry after fetch failure or slot exhaustion.
	MaxRetryBackoffMS int

	// MakeWorkerFn constructs a [WorkerFunc] for the given resume
	// session ID and SSH host. Replaces the former WorkerFn field to
	// allow the retry handler to pass the acquired SSH host at fire time.
	MakeWorkerFn func(resumeSessionID string, sshHost string) WorkerFunc

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
		ScheduleRetry(state, ScheduleRetryParams{
			IssueID:    issueID,
			Identifier: popped.Identifier,
			DisplayID:  popped.DisplayID,
			Attempt:    popped.Attempt,
			DelayMS:    delayMS,
			Error:      popped.Error,
		}, params.OnRetryFire)
		persistRetryEntry(ctx, log, params.Store, state, issueID)
		metrics.IncRetries(triggerTimer)
		return
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
				slog.Int("count", count),
				slog.Int("max_sessions", params.MaxSessions),
			)
			state.BudgetExhausted[issueID] = struct{}{}
			delete(state.Claimed, issueID)
			if err := params.Store.DeleteRetryEntry(ctx, issueID); err != nil {
				log.Error("failed to delete retry entry after budget exhaustion",
					slog.Any("error", err),
				)
			}
			return
		}
	}

	// Re-fetch active candidates from the tracker.
	candidates, err := params.TrackerAdapter.FetchCandidateIssues(ctx)
	if err != nil {
		nextAttempt := popped.Attempt + 1
		delayMS := computeBackoffDelay(nextAttempt, params.MaxRetryBackoffMS)

		log.Error("retry poll failed, rescheduling",
			slog.Int("attempt", nextAttempt),
			slog.Int64("delay_ms", delayMS),
			slog.Any("error", err),
		)

		ScheduleRetry(state, ScheduleRetryParams{
			IssueID:    issueID,
			Identifier: popped.Identifier,
			DisplayID:  popped.DisplayID,
			Attempt:    nextAttempt,
			DelayMS:    delayMS,
			Error:      "retry poll failed",
		}, params.OnRetryFire)

		persistRetryEntry(ctx, log, params.Store, state, issueID)
		metrics.IncRetries(triggerTimer)
		return
	}

	// Locate the issue by ID in the fetched candidates.
	issue, found := findIssueByID(candidates, issueID)
	if !found {
		log.Info("issue no longer active, releasing claim")
		delete(state.Claimed, issueID)

		if err := params.Store.DeleteRetryEntry(ctx, issueID); err != nil {
			log.Error("failed to delete retry entry from store",
				slog.Any("error", err),
			)
		}
		return
	}

	// Validate retry eligibility — required fields, terminal
	// state exclusion, and blocker rule. FetchCandidateIssues returns
	// active-state issues, but a mis-configured active_states list or an
	// adapter that returns terminal issues would bypass the normal
	// ShouldDispatch terminal-state gate. The explicit check keeps the
	// retry path consistent with the main dispatch path.
	terminalSet := stateSet(params.TerminalStates)
	_, isTerminal := terminalSet[strings.ToLower(issue.State)]
	if issue.ID == "" || issue.Identifier == "" || issue.Title == "" || issue.State == "" ||
		isTerminal ||
		isBlockedByNonTerminalSet(issue, terminalSet) {
		log.Info("issue no longer eligible for retry, releasing claim")
		delete(state.Claimed, issueID)

		if err := params.Store.DeleteRetryEntry(ctx, issueID); err != nil {
			log.Error("failed to delete retry entry from store",
				slog.Any("error", err),
			)
		}
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

		ScheduleRetry(state, ScheduleRetryParams{
			IssueID:    issueID,
			Identifier: popped.Identifier,
			DisplayID:  popped.DisplayID,
			Attempt:    nextAttempt,
			DelayMS:    delayMS,
			Error:      "no available orchestrator slots",
		}, params.OnRetryFire)

		persistRetryEntry(ctx, log, params.Store, state, issueID)
		metrics.IncRetries(triggerTimer)
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

			ScheduleRetry(state, ScheduleRetryParams{
				IssueID:    issueID,
				Identifier: popped.Identifier,
				DisplayID:  popped.DisplayID,
				Attempt:    nextAttempt,
				DelayMS:    delayMS,
				Error:      "no available SSH hosts",
			}, params.OnRetryFire)

			persistRetryEntry(ctx, log, params.Store, state, issueID)
			metrics.IncRetries(triggerTimer)
			return
		}
	}

	if params.MakeWorkerFn == nil {
		panic("HandleRetryTimer: nil MakeWorkerFn")
	}

	// Dispatch the issue with the popped entry's attempt number.
	// Pass the popped attempt as-is; NextAttempt increments only on the
	// next worker exit, not at dispatch time.
	attempt := popped.Attempt
	dispatchCtx := ctx
	if popped.CIFailureContext != nil {
		dispatchCtx = WithCIFailureContext(ctx, popped.CIFailureContext)
	}
	DispatchIssue(dispatchCtx, state, issue, &attempt, host, params.MakeWorkerFn("", host))
	if entry := state.Running[issue.ID]; entry != nil {
		entry.WorkflowFile = params.WorkflowFile
		entry.CIFailureContext = popped.CIFailureContext
	}
	metrics.IncDispatches(outcomeSuccess)

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
	}
	if err := store.SaveRetryEntry(ctx, pEntry); err != nil {
		log.Error("failed to persist retry entry",
			slog.Any("error", err),
		)
	}
}

// findIssueByID performs a linear scan of the candidate list looking for
// an issue whose ID matches the given id. Returns the issue and true if
// found, or a zero-value Issue and false otherwise.
func findIssueByID(issues []domain.Issue, id string) (domain.Issue, bool) {
	for _, issue := range issues {
		if issue.ID == id {
			return issue, true
		}
	}
	return domain.Issue{}, false
}
