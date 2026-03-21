package orchestrator

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
)

// defaultMaxRetryBackoffMS is the fallback cap for exponential backoff when
// the configured value is zero or negative (5 minutes).
const defaultMaxRetryBackoffMS = 300_000

// backoffBaseMS is the base delay for exponential backoff (10 seconds).
const backoffBaseMS = 10_000

// continuationDelayMS is the fixed delay for continuation retries after a
// normal worker exit (1 second).
const continuationDelayMS int64 = 1_000

// WorkerExitStore is the persistence interface required by
// [HandleWorkerExit]. It is satisfied by [persistence.Store] in production
// and by test doubles in unit tests.
type WorkerExitStore interface {
	AppendRunHistory(ctx context.Context, run persistence.RunHistory) (persistence.RunHistory, error)
	UpsertAggregateMetrics(ctx context.Context, metrics persistence.AggregateMetrics) error
	UpsertSessionMetadata(ctx context.Context, meta persistence.SessionMetadata) error
	SaveRetryEntry(ctx context.Context, entry persistence.RetryEntry) error
}

// HandleWorkerExitParams holds the dependencies for [HandleWorkerExit] that
// are not part of the core [State]. This separates pure state mutation from
// I/O side effects (SQLite persistence).
type HandleWorkerExitParams struct {
	// Store is the SQLite persistence layer. Used to persist the run
	// attempt to run_history, update aggregate_metrics, and save the
	// retry entry.
	Store WorkerExitStore

	// MaxRetryBackoffMS is the configured cap for exponential backoff
	// delay (from config.Agent.MaxRetryBackoffMS).
	MaxRetryBackoffMS int

	// OnRetryFire is the callback invoked when the scheduled retry
	// timer expires. The orchestrator provides this; it routes the
	// retry timer event back into the event loop.
	OnRetryFire func(issueID string)

	// NowFunc returns the current UTC time. Injected for testability.
	// If nil, time.Now().UTC() is used.
	NowFunc func() time.Time

	// Ctx is the context for persistence operations. The event loop
	// passes its own context so graceful shutdown can deadline-cancel
	// in-flight SQLite writes. If nil, context.Background() is used.
	Ctx context.Context

	// Logger is the structured logger with orchestrator context.
	Logger *slog.Logger
}

// HandleWorkerExit processes a worker's terminal outcome. It removes the
// running entry, updates runtime totals, persists the run to SQLite, and
// schedules the appropriate retry. Must be called from the orchestrator's
// single-writer event loop.
func HandleWorkerExit(state *State, result WorkerResult, params HandleWorkerExitParams) {
	log := params.Logger
	if log == nil {
		log = slog.Default()
	}

	ctx := params.Ctx
	if ctx == nil {
		ctx = context.Background()
	}

	entry, exists := state.Running[result.IssueID]
	if !exists {
		log.Warn("worker exit for unknown issue",
			"issue_id", result.IssueID,
			"issue_identifier", result.Identifier,
		)
		return
	}
	delete(state.Running, result.IssueID)

	now := time.Now().UTC()
	if params.NowFunc != nil {
		now = params.NowFunc()
	}

	elapsed := now.Sub(entry.StartedAt).Seconds()
	if elapsed < 0 {
		elapsed = 0
	}
	state.AgentTotals.SecondsRunning += elapsed

	status := mapExitKindToStatus(result.ExitKind)
	attempt := normalizeAttempt(entry.RetryAttempt)

	runHistory := persistence.RunHistory{
		IssueID:      result.IssueID,
		Identifier:   result.Identifier,
		Attempt:      attempt,
		AgentAdapter: result.AgentAdapter,
		Workspace:    result.WorkspacePath,
		StartedAt:    entry.StartedAt.Format(time.RFC3339),
		CompletedAt:  now.Format(time.RFC3339),
		Status:       status,
		Error:        errorStringPtr(result.Error),
	}
	if _, err := params.Store.AppendRunHistory(ctx, runHistory); err != nil {
		log.Error("failed to persist run history",
			"issue_id", result.IssueID,
			"issue_identifier", result.Identifier,
			"error", err,
		)
	}

	metrics := persistence.AggregateMetrics{
		Key:            "agent_totals",
		InputTokens:    state.AgentTotals.InputTokens,
		OutputTokens:   state.AgentTotals.OutputTokens,
		TotalTokens:    state.AgentTotals.TotalTokens,
		SecondsRunning: state.AgentTotals.SecondsRunning,
		UpdatedAt:      now.Format(time.RFC3339),
	}
	if err := params.Store.UpsertAggregateMetrics(ctx, metrics); err != nil {
		log.Error("failed to persist aggregate metrics",
			"issue_id", result.IssueID,
			"issue_identifier", result.Identifier,
			"error", err,
		)
	}

	// Persist session metadata so per-session token data survives restarts.
	sessionMeta := persistence.SessionMetadata{
		IssueID:      result.IssueID,
		SessionID:    entry.SessionID,
		InputTokens:  entry.AgentInputTokens,
		OutputTokens: entry.AgentOutputTokens,
		TotalTokens:  entry.AgentTotalTokens,
		UpdatedAt:    now.Format(time.RFC3339),
	}
	if entry.AgentPID != "" {
		sessionMeta.AgentPID = &entry.AgentPID
	}
	if err := params.Store.UpsertSessionMetadata(ctx, sessionMeta); err != nil {
		log.Error("failed to persist session metadata",
			"issue_id", result.IssueID,
			"issue_identifier", result.Identifier,
			"error", err,
		)
	}

	retryScheduled := false

	switch result.ExitKind {
	case WorkerExitNormal:
		state.Completed[result.IssueID] = struct{}{}
		ScheduleRetry(state, ScheduleRetryParams{
			IssueID:    result.IssueID,
			Identifier: result.Identifier,
			Attempt:    1,
			DelayMS:    continuationDelayMS,
			Error:      "",
		}, params.OnRetryFire)
		retryScheduled = true

	case WorkerExitCancelled:
		delete(state.Claimed, result.IssueID)

	default: // WorkerExitError and any unknown kind
		classification := classifyWorkerError(result.Error)
		if classification.Retryable {
			nextAttempt := NextAttempt(entry.RetryAttempt)
			delayMS := computeBackoffDelay(nextAttempt, params.MaxRetryBackoffMS)

			var errMsg string
			if result.Error != nil {
				errMsg = "worker exited: " + result.Error.Error()
			}

			ScheduleRetry(state, ScheduleRetryParams{
				IssueID:    result.IssueID,
				Identifier: result.Identifier,
				Attempt:    nextAttempt,
				DelayMS:    delayMS,
				Error:      errMsg,
			}, params.OnRetryFire)
			retryScheduled = true
		} else {
			log.Error("non-retryable worker error, releasing claim",
				"issue_id", result.IssueID,
				"issue_identifier", result.Identifier,
				"error", result.Error,
			)
			delete(state.Claimed, result.IssueID)
		}
	}

	if retryScheduled {
		if retryEntry, ok := state.RetryAttempts[result.IssueID]; ok {
			pEntry := persistence.RetryEntry{
				IssueID:    retryEntry.IssueID,
				Identifier: retryEntry.Identifier,
				Attempt:    retryEntry.Attempt,
				DueAtMs:    retryEntry.DueAtMS,
				Error:      stringPtr(retryEntry.Error),
			}
			if err := params.Store.SaveRetryEntry(ctx, pEntry); err != nil {
				log.Error("failed to persist retry entry",
					"issue_id", result.IssueID,
					"issue_identifier", result.Identifier,
					"error", err,
				)
			}
		}
	}

}

// computeBackoffDelay returns the exponential backoff delay in milliseconds
// for the given attempt number, capped by maxRetryBackoffMS.
//
//	delay = min(10000 * 2^(attempt-1), maxRetryBackoffMS)
//
// If maxRetryBackoffMS is <= 0, the default cap of 300000 (5 minutes) is
// used. Attempt values <= 0 are treated as attempt 1.
func computeBackoffDelay(attempt int, maxRetryBackoffMS int) int64 {
	if attempt <= 0 {
		attempt = 1
	}
	if maxRetryBackoffMS <= 0 {
		maxRetryBackoffMS = defaultMaxRetryBackoffMS
	}

	delay := float64(backoffBaseMS) * math.Pow(2, float64(attempt-1))
	cap := float64(maxRetryBackoffMS)

	return int64(math.Min(delay, cap))
}

func mapExitKindToStatus(kind WorkerExitKind) string {
	switch kind {
	case WorkerExitNormal:
		return "succeeded"
	case WorkerExitError:
		return "failed"
	case WorkerExitCancelled:
		return "cancelled"
	default:
		return "failed"
	}
}

// classifyWorkerError extracts the retry classification from a worker error.
// It unwraps the error chain looking for [domain.AgentError] or
// [domain.TrackerError]. Returns retryable-with-exponential-backoff when the
// error is nil or does not wrap a classified domain error.
func classifyWorkerError(err error) domain.RetryClassification {
	if err == nil {
		return domain.RetryClassification{Retryable: true, Backoff: domain.BackoffExponential}
	}

	var agentErr *domain.AgentError
	if errors.As(err, &agentErr) {
		return agentErr.Kind.RetryClassification()
	}

	var trackerErr *domain.TrackerError
	if errors.As(err, &trackerErr) {
		return trackerErr.Kind.RetryClassification()
	}

	return domain.RetryClassification{Retryable: true, Backoff: domain.BackoffExponential}
}

func errorStringPtr(err error) *string {
	if err == nil {
		return nil
	}
	s := err.Error()
	return &s
}

func stringPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
