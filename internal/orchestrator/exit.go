package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/workspace"
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

	// BeforeRemoveHook is the before_remove hook script (from config).
	// Empty means no hook.
	BeforeRemoveHook string

	// HookTimeoutMS is the timeout for hook invocations (from config).
	HookTimeoutMS int

	// TrackerAdapter is the tracker integration used to perform handoff
	// transitions. Required when HandoffState is non-empty. Nil is safe
	// when HandoffState is empty.
	TrackerAdapter domain.TrackerAdapter

	// HandoffState is the target tracker state for orchestrator-initiated
	// handoff transitions (from config.Tracker.HandoffState). Empty string
	// means no handoff transition; the existing continuation retry fires.
	HandoffState string

	// ActiveStates is the current list of configured active issue states
	// (from config.Tracker.ActiveStates). Used to determine whether the
	// issue is still in an active state at worker exit time. The check is
	// case-insensitive.
	ActiveStates []string

	// Metrics records instrumentation counters for worker exit events.
	// If nil, defaults to [domain.NoopMetrics].
	Metrics domain.Metrics

	// CommentsConfig holds the boolean flags for tracker comments on
	// completion and failure. Read from config.Tracker.Comments by the
	// event loop caller.
	CommentsConfig config.TrackerCommentsConfig

	// HostPool is the SSH host pool for releasing hosts on worker exit.
	// If nil, no host pool release occurs (local-mode or tests).
	HostPool *HostPool
}

// HandleWorkerExit processes a worker's terminal outcome. It removes the
// running entry, updates runtime totals, persists the run to SQLite, and
// schedules the appropriate retry. Must be called from the orchestrator's
// single-writer event loop.
func HandleWorkerExit(state *State, workerResult WorkerResult, params HandleWorkerExitParams) {
	log := params.Logger
	if log == nil {
		log = slog.Default()
	}
	log = logging.WithIssue(log, workerResult.IssueID, workerResult.Identifier)

	metrics := params.Metrics
	if metrics == nil {
		metrics = &domain.NoopMetrics{}
	}

	ctx := params.Ctx
	if ctx == nil {
		ctx = context.Background()
	}

	entry, exists := state.Running[workerResult.IssueID]
	if !exists {
		log.Warn("worker exit for unknown issue")
		return
	}
	delete(state.Running, workerResult.IssueID)

	// Release the SSH host slot so it becomes available for other issues.
	// ReleaseHost is a no-op when issueID has no assignment, so calling
	// it unconditionally is safe and more robust than gating on SSHHost.
	if params.HostPool != nil {
		params.HostPool.ReleaseHost(workerResult.IssueID)
	}

	// Enrich with session context now that both sources are available.
	// Prefer workerResult.SessionID (authoritative from the adapter) over
	// entry.SessionID (depends on EventSessionStarted processing order).
	if sid := workerResult.SessionID; sid != "" {
		log = logging.WithSession(log, sid)
	} else if entry.SessionID != "" {
		log = logging.WithSession(log, entry.SessionID)
	}

	// Capture the actual workspace path from the worker result so that
	// PendingCleanup operates on the real directory, not a path
	// reconstructed from potentially-changed config.
	if entry.WorkspacePath == "" && workerResult.WorkspacePath != "" {
		entry.WorkspacePath = workerResult.WorkspacePath
	}

	// Deferred workspace cleanup: reconciliation marks terminal issues with
	// PendingCleanup so cleanup runs only after the worker has fully exited.
	// Guarded on WorkspacePath being non-empty — if the worker exited before
	// workspace preparation, there is no directory to clean.
	if entry.PendingCleanup && entry.WorkspacePath != "" {
		if err := workspace.CleanupByPath(ctx, workspace.CleanupByPathParams{
			Path:          entry.WorkspacePath,
			Identifier:    entry.Identifier,
			IssueID:       workerResult.IssueID,
			Attempt:       normalizeAttempt(entry.RetryAttempt),
			BeforeRemove:  params.BeforeRemoveHook,
			HookTimeoutMS: params.HookTimeoutMS,
			Logger:        log,
		}); err != nil {
			log.Warn("workspace cleanup failed",
				slog.Any("error", err),
			)
		}
	}

	now := time.Now().UTC()
	if params.NowFunc != nil {
		now = params.NowFunc().UTC()
	}

	elapsed := now.Sub(entry.StartedAt).Seconds()
	if elapsed < 0 {
		elapsed = 0
	}
	state.AgentTotals.SecondsRunning += elapsed

	exitType := mapExitKindToExitType(workerResult.ExitKind)
	if workerResult.SoftStop {
		exitType = exitTypeSoftStop
	}
	metrics.IncWorkerExits(exitType)
	metrics.ObserveWorkerDuration(exitType, elapsed)
	metrics.AddAgentRuntime(elapsed)

	status := mapExitKindToStatus(workerResult.ExitKind)

	// RunHistory.Attempt is 1-based for display: first dispatch = 1,
	// first retry = 2, etc. normalizeAttempt returns the 0-based retry
	// counter (nil → 0), so add 1 for the overall run attempt number.
	runHistory := persistence.RunHistory{
		IssueID:        workerResult.IssueID,
		Identifier:     workerResult.Identifier,
		DisplayID:      entry.Issue.DisplayID,
		Attempt:        normalizeAttempt(entry.RetryAttempt) + 1,
		AgentAdapter:   workerResult.AgentAdapter,
		Workspace:      workerResult.WorkspacePath,
		StartedAt:      entry.StartedAt.Format(time.RFC3339),
		CompletedAt:    now.Format(time.RFC3339),
		Status:         status,
		Error:          errorStringPtr(workerResult.Error),
		WorkflowFile:   entry.WorkflowFile,
		TurnsCompleted: workerResult.TurnsCompleted,
	}
	if _, err := params.Store.AppendRunHistory(ctx, runHistory); err != nil {
		log.Error("failed to persist run history",
			slog.Any("error", err),
		)
	}

	aggMetrics := persistence.AggregateMetrics{
		Key:             "agent_totals",
		InputTokens:     state.AgentTotals.InputTokens,
		OutputTokens:    state.AgentTotals.OutputTokens,
		TotalTokens:     state.AgentTotals.TotalTokens,
		CacheReadTokens: state.AgentTotals.CacheReadTokens,
		SecondsRunning:  state.AgentTotals.SecondsRunning,
		UpdatedAt:       now.Format(time.RFC3339),
	}
	if err := params.Store.UpsertAggregateMetrics(ctx, aggMetrics); err != nil {
		log.Error("failed to persist aggregate metrics",
			slog.Any("error", err),
		)
	}

	// Persist session metadata so per-session token data survives restarts.
	// Prefer workerResult.SessionID: the worker carries the authoritative value
	// directly from the adapter, while entry.SessionID depends on
	// EventSessionStarted having been processed before exit.
	sessionID := workerResult.SessionID
	if sessionID == "" {
		sessionID = entry.SessionID
	}
	sessionMeta := persistence.SessionMetadata{
		IssueID:         workerResult.IssueID,
		SessionID:       sessionID,
		InputTokens:     entry.AgentInputTokens,
		OutputTokens:    entry.AgentOutputTokens,
		TotalTokens:     entry.AgentTotalTokens,
		CacheReadTokens: entry.CacheReadTokens,
		ModelName:       entry.ModelName,
		APIRequestCount: entry.APIRequestCount,
		UpdatedAt:       now.Format(time.RFC3339),
	}
	if entry.AgentPID != "" {
		sessionMeta.AgentPID = &entry.AgentPID
	}
	if err := params.Store.UpsertSessionMetadata(ctx, sessionMeta); err != nil {
		log.Error("failed to persist session metadata",
			slog.Any("error", err),
		)
	}

	retryScheduled := false
	nextAttempt := 0

	switch workerResult.ExitKind {
	case WorkerExitNormal:
		state.Completed[workerResult.IssueID] = struct{}{}

		// Determine whether the issue is still in an active tracker state.
		// When ActiveStates is nil or empty, default to true (pessimistic —
		// backward compatibility guard: continuation retry fires).
		issueIsActive := len(params.ActiveStates) == 0 || isActiveState(entry.Issue.State, params.ActiveStates)

		switch {
		case workerResult.SoftStop:
			// Agent signaled a recognized A2O status (blocked,
			// needs-human-review). Suppress continuation retry and
			// release the claim immediately.
			log.Info("continuation retry suppressed",
				slog.String("reason", workerResult.SoftStopReason),
			)
			CancelRetry(state, workerResult.IssueID)
			delete(state.Claimed, workerResult.IssueID)

		case params.HandoffState != "" && issueIsActive:
			// Handoff: issue is active and handoff_state is configured.
			// Guard against nil TrackerAdapter (misconfiguration or test
			// that sets HandoffState without providing an adapter).
			if params.TrackerAdapter == nil {
				log.Warn("handoff configured but tracker adapter is nil, scheduling continuation retry",
					slog.String("handoff_state", params.HandoffState),
				)
				metrics.IncHandoffTransitions(handoffError)
				ScheduleRetry(state, ScheduleRetryParams{
					IssueID:     workerResult.IssueID,
					Identifier:  workerResult.Identifier,
					DisplayID:   entry.Issue.DisplayID,
					Attempt:     NextAttempt(entry.RetryAttempt),
					DelayMS:     continuationDelayMS,
					Error:       "",
					LastSSHHost: workerResult.SSHHost,
				}, params.OnRetryFire)
				metrics.IncRetries(triggerContinuation)
				retryScheduled = true
			} else if err := params.TrackerAdapter.TransitionIssue(ctx, workerResult.IssueID, params.HandoffState); err != nil {
				log.Warn("handoff transition failed, scheduling continuation retry",
					slog.String("handoff_state", params.HandoffState),
					slog.Any("error", err),
				)
				metrics.IncHandoffTransitions(handoffError)
				ScheduleRetry(state, ScheduleRetryParams{
					IssueID:     workerResult.IssueID,
					Identifier:  workerResult.Identifier,
					DisplayID:   entry.Issue.DisplayID,
					Attempt:     NextAttempt(entry.RetryAttempt),
					DelayMS:     continuationDelayMS,
					Error:       "",
					LastSSHHost: workerResult.SSHHost,
				}, params.OnRetryFire)
				metrics.IncRetries(triggerContinuation)
				retryScheduled = true
			} else {
				log.Info("handoff transition succeeded, releasing claim",
					slog.String("handoff_state", params.HandoffState),
				)
				metrics.IncHandoffTransitions(handoffSuccess)
				CancelRetry(state, workerResult.IssueID)
				delete(state.Claimed, workerResult.IssueID)
			}

		case issueIsActive:
			// No handoff configured but issue is still active:
			// schedule continuation retry (existing behavior).
			ScheduleRetry(state, ScheduleRetryParams{
				IssueID:     workerResult.IssueID,
				Identifier:  workerResult.Identifier,
				DisplayID:   entry.Issue.DisplayID,
				Attempt:     NextAttempt(entry.RetryAttempt),
				DelayMS:     continuationDelayMS,
				Error:       "",
				LastSSHHost: workerResult.SSHHost,
			}, params.OnRetryFire)
			metrics.IncRetries(triggerContinuation)
			retryScheduled = true

		default:
			// Issue is not in an active state: cancel any pending retry
			// and release claim.
			if params.HandoffState != "" {
				metrics.IncHandoffTransitions(handoffSkipped)
			}
			CancelRetry(state, workerResult.IssueID)
			delete(state.Claimed, workerResult.IssueID)
		}

	case WorkerExitCancelled:
		// Only release the claim if no retry has been pre-scheduled by
		// reconciliation stall detection. A pre-scheduled retry needs the
		// claim to prevent duplicate dispatch.
		if _, hasRetry := state.RetryAttempts[workerResult.IssueID]; !hasRetry {
			delete(state.Claimed, workerResult.IssueID)
		}

	default: // WorkerExitError and any unknown kind
		classification := classifyWorkerError(workerResult.Error)
		if classification.Retryable {
			nextAttempt = NextAttempt(entry.RetryAttempt)
			delayMS := computeBackoffDelay(nextAttempt, params.MaxRetryBackoffMS)

			log.Warn("worker run failed, scheduling retry",
				slog.Any("error", workerResult.Error),
				slog.Int("next_attempt", nextAttempt),
				slog.Int64("delay_ms", delayMS),
			)

			var errMsg string
			if workerResult.Error != nil {
				errMsg = "worker exited: " + workerResult.Error.Error()
			}

			ScheduleRetry(state, ScheduleRetryParams{
				IssueID:     workerResult.IssueID,
				Identifier:  workerResult.Identifier,
				DisplayID:   entry.Issue.DisplayID,
				Attempt:     nextAttempt,
				DelayMS:     delayMS,
				Error:       errMsg,
				LastSSHHost: workerResult.SSHHost,
			}, params.OnRetryFire)
			metrics.IncRetries(triggerError)
			retryScheduled = true
		} else {
			log.Error("worker run failed, non-retryable, releasing claim",
				slog.Any("error", workerResult.Error),
			)
			delete(state.Claimed, workerResult.IssueID)
		}
	}

	if retryScheduled {
		if retryEntry, ok := state.RetryAttempts[workerResult.IssueID]; ok {
			pEntry := persistence.RetryEntry{
				IssueID:    retryEntry.IssueID,
				Identifier: retryEntry.Identifier,
				Attempt:    retryEntry.Attempt,
				DueAtMs:    retryEntry.DueAtMS,
				Error:      stringPtr(retryEntry.Error),
			}
			if err := params.Store.SaveRetryEntry(ctx, pEntry); err != nil {
				log.Error("failed to persist retry entry",
					slog.Any("error", err),
				)
			}
		}
	}

	// Build comment text synchronously (to capture all exit-time data),
	// then fire the CommentIssue API call in a detached goroutine so
	// the event loop is never blocked.
	var commentText string
	var lifecycle string

	sessionID = workerResult.SessionID
	if sessionID == "" {
		sessionID = entry.SessionID
	}
	runDuration := now.Sub(entry.StartedAt)
	if runDuration < 0 {
		runDuration = 0
	}

	switch workerResult.ExitKind {
	case WorkerExitNormal:
		if params.CommentsConfig.OnCompletion {
			if workerResult.SoftStop {
				commentText = buildSoftStopComment(sessionID, runDuration, workerResult.TurnsCompleted, workerResult.SoftStopReason)
			} else {
				commentText = buildCompletionComment(sessionID, runDuration, workerResult.TurnsCompleted, retryScheduled)
			}
			lifecycle = "completion"
		}
	case WorkerExitCancelled:
		// No comment on cancellation.
	default:
		if params.CommentsConfig.OnFailure {
			commentText = buildFailureComment(sessionID, runDuration, workerResult.Error, retryScheduled, nextAttempt)
			lifecycle = "failure"
		}
	}

	if commentText != "" && params.TrackerAdapter != nil {
		issueID := workerResult.IssueID
		tracker := params.TrackerAdapter
		m := metrics
		commentLog := log
		lc := lifecycle
		ct := commentText

		go func() {
			dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()

			if err := tracker.CommentIssue(dctx, issueID, ct); err != nil {
				commentLog.Warn("tracker comment failed",
					slog.String("lifecycle", lc),
					slog.Any("error", err),
				)
				m.IncTrackerComments(lc, "error")
			} else {
				commentLog.Info("tracker comment posted",
					slog.String("lifecycle", lc),
				)
				m.IncTrackerComments(lc, "success")
			}
		}()
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

func mapExitKindToExitType(kind WorkerExitKind) string {
	switch kind {
	case WorkerExitNormal:
		return exitTypeNormal
	case WorkerExitError:
		return exitTypeError
	case WorkerExitCancelled:
		return exitTypeCancelled
	default:
		return exitTypeError
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

// buildCompletionComment returns the tracker comment text for a normal
// session exit. retryScheduled distinguishes "completed (re-queuing)"
// from "completed".
func buildCompletionComment(sessionID string, elapsed time.Duration, turnsCompleted int, retryScheduled bool) string {
	if sessionID == "" {
		sessionID = "unknown"
	}
	headline := "Sortie session completed."
	if retryScheduled {
		headline = "Sortie session completed (re-queuing)."
	}
	return fmt.Sprintf("%s\nSession: %s\nDuration: %s\nTurns: %d",
		headline, sessionID, elapsed.Truncate(time.Second).String(), turnsCompleted)
}

// buildFailureComment returns the tracker comment text for an error
// session exit.
func buildFailureComment(sessionID string, elapsed time.Duration, exitErr error, retryScheduled bool, nextAttempt int) string {
	if sessionID == "" {
		sessionID = "unknown"
	}
	errStr := "unknown error"
	if exitErr != nil {
		errStr = exitErr.Error()
		if len(errStr) > 200 {
			errStr = errStr[:200] + "..."
		}
	}
	retryLine := "Retry: no — not retryable"
	if retryScheduled {
		retryLine = fmt.Sprintf("Retry: yes (attempt %d)", nextAttempt)
	}
	return fmt.Sprintf("Sortie session failed.\nSession: %s\nDuration: %s\nError: %s\n%s",
		sessionID, elapsed.Truncate(time.Second).String(), errStr, retryLine)
}

// buildSoftStopComment returns the tracker comment text for a worker
// exit triggered by a recognized A2O status signal.
func buildSoftStopComment(sessionID string, elapsed time.Duration, turnsCompleted int, reason string) string {
	if sessionID == "" {
		sessionID = "unknown"
	}
	return fmt.Sprintf("Sortie session completed (agent signaled: %s).\nSession: %s\nDuration: %s\nTurns: %d",
		reason, sessionID, elapsed.Truncate(time.Second).String(), turnsCompleted)
}
