package orchestrator

import (
	"context"
	"log/slog"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
)

// RetryTimerStore is the persistence interface required by
// [HandleRetryTimer]. It is satisfied by [persistence.Store] in production
// and by test doubles in unit tests.
type RetryTimerStore interface {
	SaveRetryEntry(ctx context.Context, entry persistence.RetryEntry) error
	DeleteRetryEntry(ctx context.Context, issueID string) error
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

	// WorkerFn is the worker function to pass to [DispatchIssue].
	WorkerFn WorkerFunc

	// OnRetryFire is the callback for re-scheduled retry timers.
	// Routes back into the event loop.
	OnRetryFire func(issueID string)

	// Ctx is the context for tracker API calls and persistence operations.
	Ctx context.Context

	// Logger is the structured logger with orchestrator context.
	Logger *slog.Logger
}

// HandleRetryTimer processes a retry timer event for the given issue.
// It implements the on_retry_timer algorithm from architecture Section 16.6:
// pop the retry entry, re-fetch candidates, find the issue, check slot
// availability, and either dispatch or reschedule/release. Must be called
// from the orchestrator's single-writer event loop.
func HandleRetryTimer(state *State, issueID string, params HandleRetryTimerParams) {
	log := params.Logger
	if log == nil {
		log = slog.Default()
	}

	ctx := params.Ctx
	if ctx == nil {
		ctx = context.Background()
	}

	// Step 1: Pop the retry entry. If missing, the timer raced with a
	// cancellation or a subsequent ScheduleRetry — no-op.
	popped, exists := state.RetryAttempts[issueID]
	if !exists {
		log.Debug("retry timer for unknown entry",
			"issue_id", issueID,
		)
		return
	}
	if popped.TimerHandle != nil {
		popped.TimerHandle.Stop()
	}
	delete(state.RetryAttempts, issueID)

	// Step 2: Re-fetch active candidates from the tracker.
	candidates, err := params.TrackerAdapter.FetchCandidateIssues(ctx)
	if err != nil {
		nextAttempt := popped.Attempt + 1
		delayMS := computeBackoffDelay(nextAttempt, params.MaxRetryBackoffMS)

		log.Error("retry poll failed, rescheduling",
			"issue_id", issueID,
			"issue_identifier", popped.Identifier,
			"attempt", nextAttempt,
			"delay_ms", delayMS,
			"error", err,
		)

		ScheduleRetry(state, ScheduleRetryParams{
			IssueID:    issueID,
			Identifier: popped.Identifier,
			Attempt:    nextAttempt,
			DelayMS:    delayMS,
			Error:      "retry poll failed",
		}, params.OnRetryFire)

		persistRetryEntry(ctx, log, params.Store, state, issueID)
		return
	}

	// Step 3: Find the issue by ID in fetched candidates.
	issue, found := findIssueByID(candidates, issueID)
	if !found {
		log.Info("issue no longer active, releasing claim",
			"issue_id", issueID,
			"issue_identifier", popped.Identifier,
		)
		delete(state.Claimed, issueID)

		if err := params.Store.DeleteRetryEntry(ctx, issueID); err != nil {
			log.Error("failed to delete retry entry from store",
				"issue_id", issueID,
				"error", err,
			)
		}
		return
	}

	// Step 3b: Validate retry eligibility — required fields and blocker
	// rule. FetchCandidateIssues confirms active state; these additional
	// checks guard against issues that gained blockers or lost required
	// data between retry schedule and timer fire.
	if issue.ID == "" || issue.Identifier == "" || issue.Title == "" || issue.State == "" ||
		IsBlockedByNonTerminal(issue, params.TerminalStates) {
		log.Info("issue no longer eligible for retry, releasing claim",
			"issue_id", issueID,
			"issue_identifier", popped.Identifier,
		)
		delete(state.Claimed, issueID)

		if err := params.Store.DeleteRetryEntry(ctx, issueID); err != nil {
			log.Error("failed to delete retry entry from store",
				"issue_id", issueID,
				"error", err,
			)
		}
		return
	}

	// Step 4: Check slot availability for the issue's state.
	if !HasAvailableSlots(state, issue.State) {
		nextAttempt := popped.Attempt + 1
		delayMS := computeBackoffDelay(nextAttempt, params.MaxRetryBackoffMS)

		log.Warn("no available orchestrator slots, rescheduling retry",
			"issue_id", issueID,
			"issue_identifier", popped.Identifier,
			"issue_state", issue.State,
			"attempt", nextAttempt,
			"delay_ms", delayMS,
		)

		ScheduleRetry(state, ScheduleRetryParams{
			IssueID:    issueID,
			Identifier: popped.Identifier,
			Attempt:    nextAttempt,
			DelayMS:    delayMS,
			Error:      "no available orchestrator slots",
		}, params.OnRetryFire)

		persistRetryEntry(ctx, log, params.Store, state, issueID)
		return
	}

	// Step 5: Dispatch the issue with the popped entry's attempt number.
	// Pass the popped attempt as-is; NextAttempt increments only on the
	// next worker exit, not at dispatch time.
	attempt := popped.Attempt
	DispatchIssue(ctx, state, issue, &attempt, params.WorkerFn)

	log.Info("retried issue dispatched",
		"issue_id", issueID,
		"issue_identifier", issue.Identifier,
		"attempt", attempt,
	)

	// Defense-in-depth: delete the SQLite row persisted by worker exit.
	// DispatchIssue calls CancelRetry which clears the in-memory entry,
	// but the persisted row must also be cleaned up.
	if err := params.Store.DeleteRetryEntry(ctx, issueID); err != nil {
		log.Error("failed to delete retry entry from store after dispatch",
			"issue_id", issueID,
			"error", err,
		)
	}
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
			"issue_id", issueID,
			"issue_identifier", retryEntry.Identifier,
			"error", err,
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
