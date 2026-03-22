package orchestrator

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
)

// ReconcileStore is the persistence interface required by
// [ReconcileRunningIssues]. Satisfied by [persistence.Store] in production
// and by test doubles in unit tests.
type ReconcileStore interface {
	SaveRetryEntry(ctx context.Context, entry persistence.RetryEntry) error
	DeleteRetryEntry(ctx context.Context, issueID string) error
}

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
}

// ReconcileRunningIssues detects stalled workers and refreshes tracker
// state for all running issues. Intended to be called from the poll tick
// before dispatch; wiring into the event loop is done by the caller.
//
// Part A cancels workers that have exceeded the configured stall timeout
// and schedules exponential-backoff retries. Part B queries the tracker
// for current issue states: terminal issues are marked for workspace
// cleanup, active issues get their in-memory snapshot updated, and
// non-active/non-terminal issues are cancelled without cleanup.
//
// Running entries are never removed by reconciliation. Cancelled workers
// exit asynchronously and are processed by [HandleWorkerExit].
func ReconcileRunningIssues(state *State, params ReconcileParams) {
	log := params.Logger
	if log == nil {
		log = slog.Default()
	}

	ctx := params.Ctx
	if ctx == nil {
		ctx = context.Background()
	}

	now := time.Now().UTC()
	if params.NowFunc != nil {
		now = params.NowFunc().UTC()
	}

	// Part A: stall detection.
	reconcileStalled(state, params, log, ctx, now)

	// Part B: tracker state refresh.
	reconcileTrackerState(state, params, log, ctx)
}

// reconcileStalled implements Part A: stall detection. For each running
// entry whose last activity exceeds the stall timeout, the worker is
// cancelled and an exponential-backoff retry is scheduled.
func reconcileStalled(state *State, params ReconcileParams, log *slog.Logger, ctx context.Context, now time.Time) {
	if params.StallTimeoutMS <= 0 {
		return
	}

	stallThreshold := int64(params.StallTimeoutMS)

	for issueID, entry := range state.Running {
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

		nextAttempt := NextAttempt(entry.RetryAttempt)

		// Skip scheduling when a retry is already present at the same or
		// higher attempt. Without this guard, every reconciliation tick
		// would replace the existing timer, pushing DueAtMS forward and
		// preventing the retry from ever firing.
		if existing, ok := state.RetryAttempts[issueID]; ok && existing.Attempt >= nextAttempt {
			log.Debug("stall retry already scheduled, skipping reschedule",
				slog.String("issue_id", issueID),
				slog.String("issue_identifier", entry.Identifier),
				slog.Int("current_attempt", existing.Attempt),
				slog.Int("next_attempt", nextAttempt),
			)
		} else {
			delayMS := computeBackoffDelay(nextAttempt, params.MaxRetryBackoffMS)

			ScheduleRetry(state, ScheduleRetryParams{
				IssueID:    issueID,
				Identifier: entry.Identifier,
				Attempt:    nextAttempt,
				DelayMS:    delayMS,
				Error:      "stall timeout exceeded",
			}, params.OnRetryFire)

			if retryEntry, ok := state.RetryAttempts[issueID]; ok {
				pEntry := persistence.RetryEntry{
					IssueID:    retryEntry.IssueID,
					Identifier: retryEntry.Identifier,
					Attempt:    retryEntry.Attempt,
					DueAtMs:    retryEntry.DueAtMS,
					Error:      stringPtr(retryEntry.Error),
				}
				if err := params.Store.SaveRetryEntry(ctx, pEntry); err != nil {
					log.Error("failed to persist stall retry entry",
						slog.String("issue_id", issueID),
						slog.String("issue_identifier", entry.Identifier),
						slog.Any("error", err),
					)
				}
			}
		}

		log.Warn("stall detected, cancelling worker",
			slog.String("issue_id", issueID),
			slog.String("issue_identifier", entry.Identifier),
			slog.Int64("elapsed_ms", elapsedMS),
			slog.Int("stall_timeout_ms", params.StallTimeoutMS),
		)
	}
}

// reconcileTrackerState implements Part B: tracker state refresh. It
// fetches current issue states for all running IDs and cancels workers
// whose issues are terminal or no longer active.
func reconcileTrackerState(state *State, params ReconcileParams, log *slog.Logger, ctx context.Context) {
	if len(state.Running) == 0 {
		return
	}

	runningIDs := make([]string, 0, len(state.Running))
	for id := range state.Running {
		runningIDs = append(runningIDs, id)
	}

	refreshed, err := params.TrackerAdapter.FetchIssueStatesByIDs(ctx, runningIDs)
	if err != nil {
		log.Warn("tracker state refresh failed, keeping workers running",
			slog.Any("error", err),
		)
		return
	}

	activeSet := stateSet(params.ActiveStates)
	terminalSet := stateSet(params.TerminalStates)

	for issueID, stateName := range refreshed {
		entry, ok := state.Running[issueID]
		if !ok {
			continue
		}

		normalized := strings.ToLower(stateName)

		if _, terminal := terminalSet[normalized]; terminal {
			if entry.CancelFunc != nil {
				entry.CancelFunc()
			}
			CancelRetry(state, issueID)
			if err := params.Store.DeleteRetryEntry(ctx, issueID); err != nil {
				log.Error("failed to delete retry entry for terminal issue",
					slog.String("issue_id", issueID),
					slog.String("issue_identifier", entry.Identifier),
					slog.Any("error", err),
				)
			}
			entry.PendingCleanup = true
			log.Info("stopping worker for terminal issue",
				slog.String("issue_id", issueID),
				slog.String("issue_identifier", entry.Identifier),
				slog.String("state", stateName),
			)
			continue
		}

		if _, active := activeSet[normalized]; active {
			entry.Issue.State = stateName
			log.Debug("refreshed issue state",
				slog.String("issue_id", issueID),
				slog.String("issue_identifier", entry.Identifier),
				slog.String("state", stateName),
			)
			continue
		}

		// Non-active, non-terminal: cancel without workspace cleanup.
		if entry.CancelFunc != nil {
			entry.CancelFunc()
		}
		CancelRetry(state, issueID)
		if err := params.Store.DeleteRetryEntry(ctx, issueID); err != nil {
			log.Error("failed to delete retry entry for non-active issue",
				slog.String("issue_id", issueID),
				slog.String("issue_identifier", entry.Identifier),
				slog.Any("error", err),
			)
		}
		log.Info("stopping worker for non-active issue",
			slog.String("issue_id", issueID),
			slog.String("issue_identifier", entry.Identifier),
			slog.String("state", stateName),
		)
	}
}
