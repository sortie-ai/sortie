package orchestrator

import (
	"context"
	"encoding/json"
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

// defaultMaxRetryBackoffMS is the fallback exponential-backoff cap when the
// configured value is non-positive (5 minutes).
const defaultMaxRetryBackoffMS = 300_000

// backoffBaseMS is the base delay for exponential backoff (10 seconds).
const backoffBaseMS = 10_000

// continuationDelayMS is the fixed delay for continuation retries after a
// normal exit (1 second).
const continuationDelayMS int64 = 1_000

// WorkerExitStore is the persistence interface required by
// [HandleWorkerExit].
type WorkerExitStore interface {
	AppendRunHistory(ctx context.Context, run persistence.RunHistory) (persistence.RunHistory, error)
	UpsertAggregateMetrics(ctx context.Context, metrics persistence.AggregateMetrics) error
	UpsertSessionMetadata(ctx context.Context, meta persistence.SessionMetadata) error
	SaveRetryEntry(ctx context.Context, entry persistence.RetryEntry) error
	DeleteRetryEntry(ctx context.Context, issueID string) error
	QueryConsecutiveHandoffAbsenceCounts(ctx context.Context, issueIDs []string) (map[string]int, error)
	ResetHandoffAbsenceSequence(ctx context.Context, issueID string) error
	UpsertParkedIssue(ctx context.Context, entry persistence.ParkedIssue) error
	DeleteParkedIssue(ctx context.Context, issueID string) error
	CountWorkerRunsCompletedSince(ctx context.Context, issueID string, since time.Time) (int, error)
}

// HandleWorkerExitParams holds the dependencies for [HandleWorkerExit] that
// are not part of the core [State], separating pure state mutation from I/O.
type HandleWorkerExitParams struct {
	Store WorkerExitStore

	MaxRetryBackoffMS int

	MaxConsecutiveAbsences int

	// HandoffParkingLabel is the review-comments escalation label. Empty
	// falls back to "needs-human".
	HandoffParkingLabel string

	// OnRetryFire routes an expired retry timer back into the event loop.
	OnRetryFire func(issueID string)

	// NowFunc returns the current UTC time; nil uses time.Now().UTC().
	// Injected for testability.
	NowFunc func() time.Time

	// Ctx is the context for persistence, passed by the event loop so
	// shutdown can deadline-cancel in-flight writes. Nil uses
	// context.Background().
	Ctx context.Context

	Logger *slog.Logger

	// BeforeRemoveHook is the before_remove hook script. Empty means none.
	BeforeRemoveHook string

	HookTimeoutMS int

	// TrackerAdapter performs handoff transitions. Required when
	// HandoffState is non-empty; nil is safe when it is empty.
	TrackerAdapter domain.TrackerAdapter

	// HandoffState is the target state for orchestrator-initiated handoff
	// transitions. Empty means no handoff; the continuation retry fires.
	HandoffState string

	// NoChangeState is the target for a run that declared no change was
	// needed. Empty falls back to HandoffState.
	NoChangeState string

	// ActiveStates determines whether the issue is still active at exit
	// (case-insensitive).
	ActiveStates []string

	// TerminalStates suppresses the handoff when the issue already reached a
	// terminal state (case-insensitive). An empty list classifies no state
	// as terminal.
	TerminalStates []string

	// Metrics records worker-exit counters. If nil, defaults to
	// [domain.NoopMetrics].
	Metrics domain.Metrics

	// CommentsConfig holds the completion/failure comment flags.
	CommentsConfig config.TrackerCommentsConfig

	// HostPool releases hosts on exit. Nil means no release (local or tests).
	HostPool *HostPool

	// CIProvider, when non-nil, lets HandleWorkerExit seed a pending CI
	// reaction on normal exits.
	CIProvider domain.CIStatusProvider

	// SCMAdapter, when non-nil and the workspace SCM metadata carries PR
	// identity, lets HandleWorkerExit seed pending SCM reactions on normal
	// exits.
	SCMAdapter domain.SCMAdapter

	// The *ReactionConfigured flags mark whether each feature is active for
	// the process; each enqueue path gates on its flag and a non-nil
	// SCMAdapter.
	AutoMergeReactionConfigured bool

	BotReviewReactionConfigured bool

	MergeConflictReactionConfigured bool

	LabelReviewReactionConfigured bool

	LabelFixReactionConfigured bool

	MergeCompletionReactionConfigured bool
}

// resolveTerminalObservation returns the freshest tracker state observation
// for this exit and a source label ("reconcile", "worker", or "snapshot").
// It feeds the terminal test only; active-state classification keeps reading
// entry.Issue.State. Never empty when entry.Issue.State is non-empty.
func resolveTerminalObservation(entry *RunningEntry, result WorkerResult) (state string, source string) {
	if entry.ObservedTerminalState != "" {
		return entry.ObservedTerminalState, "reconcile"
	}
	if result.ObservedIssueState != "" {
		return result.ObservedIssueState, "worker"
	}
	return entry.Issue.State, "snapshot"
}

// resolveExitTarget returns the tracker state the handoff arm transitions to
// and whether the run declared no change. A declared run moves to
// params.NoChangeState when set; every other run, and a declared run with
// empty NoChangeState, moves to params.HandoffState.
func resolveExitTarget(params HandleWorkerExitParams, workerResult WorkerResult) (target string, declared bool) {
	declared = workerResult.SoftStop && workerResult.SoftStopReason == string(workspace.StatusNoChangeNeeded)
	if declared && params.NoChangeState != "" {
		return params.NoChangeState, declared
	}
	return params.HandoffState, declared
}

// HandleWorkerExit processes a worker's terminal outcome: removes the running
// entry, updates totals, persists to SQLite, and schedules the appropriate
// retry. Must be called from the single-writer event loop.
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
	if entry.CancelFunc != nil {
		entry.CancelFunc()
	}

	// Reconcile the entry's token totals from the worker's own figure before
	// the run_history/session_metadata/aggregate writes below, so a trailing
	// usage event a full agentEventCh dropped cannot leave the recorded
	// total below what the worker spent. The monotone-delta rule applies a
	// zero delta when Usage is already fully accounted for.
	applyUsageDelta(state, entry, workerResult.Usage, metrics)

	// Reconcile model name and request count the same way: the worker's
	// mirror folded every event the entry applied, plus any a full
	// agentEventCh dropped, so they are never behind the entry's own.
	if workerResult.ModelName != "" {
		entry.ModelName = workerResult.ModelName
	}
	entry.APIRequestCount = max(entry.APIRequestCount, workerResult.APIRequestCount)

	// measured ORs the entry's flag with the worker's mirror, after the
	// reconciliation above so a measurement that arrived only on the worker
	// result still counts.
	measured := entry.UsageMeasured || workerResult.UsageMeasured

	// ReleaseHost is a no-op when issueID has no assignment, so calling it
	// unconditionally is safe.
	if params.HostPool != nil {
		params.HostPool.ReleaseHost(workerResult.IssueID)
	}

	// Prefer workerResult.SessionID (authoritative from the adapter) over
	// entry.SessionID, whose EventSessionStarted a full agentEventCh may
	// have dropped.
	if sid := workerResult.SessionID; sid != "" {
		log = logging.WithSession(log, sid)
	} else if entry.SessionID != "" {
		log = logging.WithSession(log, entry.SessionID)
	}

	warnCeilingMeasuredNothing(log, entry, state.MaxTokens, measured)

	// Capture the actual workspace path so PendingCleanup operates on the
	// real directory, not one reconstructed from possibly-changed config.
	if entry.WorkspacePath == "" && workerResult.WorkspacePath != "" {
		entry.WorkspacePath = workerResult.WorkspacePath
	}

	// Deferred workspace cleanup for terminal issues, run only after the
	// worker has fully exited. Guarded on WorkspacePath being non-empty.
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
	if !measured {
		state.AgentTotals.UnmeasuredSessions++
	}

	exitType := mapExitKindToExitType(workerResult.ExitKind)
	if workerResult.SoftStop {
		exitType = exitTypeSoftStop
	}
	metrics.IncWorkerExits(exitType)
	metrics.ObserveWorkerDuration(exitType, elapsed)
	metrics.AddAgentRuntime(elapsed)

	// Resolve the normal-exit disposition far enough to know whether the
	// evidence policy changes this run's persisted status. Evidence is
	// consulted only where the handoff conditions already hold, and only
	// after the blocked and terminal dispositions are ruled out.
	var issueIsActive bool
	var blockedSoftStop bool
	var drivesIssue bool
	var handoffPath bool
	var observation string
	var observationSource string
	var terminal bool
	var evidenceResult handoffEvidenceResult
	var evidenceWithheld bool
	var evidenceWorkObserved bool
	var evidenceErr error
	if workerResult.ExitKind == WorkerExitNormal {
		issueIsActive = len(params.ActiveStates) == 0 || isActiveState(entry.Issue.State, params.ActiveStates)
		blockedSoftStop = workerResult.SoftStop && workerResult.SoftStopReason == string(workspace.StatusBlocked)
		drivesIssue = dispatchPostureForReactionKind(entry.ReactionKind).DrivesIssueState()
		handoffPath = params.HandoffState != "" && issueIsActive && !blockedSoftStop && drivesIssue
		observation, observationSource = resolveTerminalObservation(entry, workerResult)
		terminal = isTerminalState(observation, params.TerminalStates)

		policy := workerResult.HandoffEvidencePolicy.Effective()
		if handoffPath && !terminal && policy != config.HandoffEvidenceOff {
			evidenceResult = evaluateHandoffEvidence(ctx, workerResult, log)
			if evidenceResult.Verdict == handoffEvidenceUndetermined {
				log.Info("handoff evidence not determinable",
					slog.String("policy", string(policy)),
					slog.String("verdict", string(evidenceResult.Verdict)),
					slog.String("reason", evidenceResult.Reason),
					slog.Int("turns_completed", workerResult.TurnsCompleted),
				)
			}
			evidenceWithheld = handoffEvidenceWithholds(policy, evidenceResult)

			// Before recording an absence failure, re-read the tracker state
			// once: the resolved observation may be stale by the time
			// teardown completed. With no terminal states configured the
			// read cannot change the disposition, so skip it.
			if evidenceWithheld && len(params.TerminalStates) > 0 && params.TrackerAdapter != nil {
				if verified, verifyErr := params.TrackerAdapter.FetchIssueStatesByIDs(ctx, []string{workerResult.IssueID}); verifyErr != nil {
					log.Warn("withheld handoff verification read failed, recording withheld handoff",
						slog.Any("error", verifyErr),
						slog.String("state_source", observationSource),
					)
				} else if verifiedState, ok := verified[workerResult.IssueID]; ok && isTerminalState(verifiedState, params.TerminalStates) {
					log.Info("withheld handoff suppressed for terminal issue",
						slog.String("state", verifiedState),
						slog.String("state_source", "verified"),
						slog.String("handoff_state", params.HandoffState),
						slog.String("policy", string(policy)),
						slog.String("verdict", string(evidenceResult.Verdict)),
						slog.String("reason", evidenceResult.Reason),
						slog.Int("turns_completed", workerResult.TurnsCompleted),
					)
					observation = verifiedState
					observationSource = "verified"
					terminal = true
					evidenceWithheld = false
				}
			}

			if evidenceWithheld {
				evidenceErr = handoffEvidenceFailure(policy, evidenceResult)
				metrics.IncHandoffTransitions(handoffWithheld)
			} else if evidenceResult.Verdict == handoffWorkObserved {
				// A positive verdict releases a park. The durable reset
				// recorded after the run_history write below holds even if
				// the later tracker handoff write fails.
				evidenceWorkObserved = true
				if _, parked := state.Parked[workerResult.IssueID]; parked {
					unparkIssue(ctx, state, workerResult.IssueID, unparkTriggerEvidenceObserved, params.Store, log)
				}
			}
		}
	}

	status := mapExitKindToStatus(workerResult.ExitKind)
	runError := workerResult.Error

	// Distinct from a stall, terminal-state, or shutdown cancel: attribute
	// the stop to the budget.
	request := entry.TokenCeilingStopRequest
	if workerResult.ExitKind == WorkerExitCancelled && workerResult.StoppedByTokenCeiling && request != nil {
		status = "budget_stopped"
		runError = tokenCeilingStopError(entry.IssueTokensCompleted+entry.AgentTotalTokens, request.BudgetTokens)
		reportTokenCeilingStop(log, metrics, request)
	}

	// A needs-a-person ending gets its own status only when the worker's own
	// error, not a shutdown racing it, stopped the run. mapExitKindToStatus
	// already reports a live-context cancel as "cancelled", so this guard
	// alone keeps a shutdown from being relabelled. The error is always
	// wrapped, so it MUST be unwrapped rather than asserted.
	if status == "failed" {
		if agentErr, ok := errors.AsType[*domain.AgentError](runError); ok && agentErr.Kind == domain.ErrTurnInputRequired {
			status = "needs_person"
		}
	}

	if evidenceWithheld {
		status = "failed"
		runError = evidenceErr
	}

	// RunHistory.Attempt is 1-based for display; normalizeAttempt returns the
	// 0-based retry counter, so add 1.
	runHistory := persistence.RunHistory{
		IssueID:          workerResult.IssueID,
		Identifier:       workerResult.Identifier,
		DisplayID:        entry.Issue.DisplayID,
		Attempt:          normalizeAttempt(entry.RetryAttempt) + 1,
		AgentAdapter:     workerResult.AgentAdapter,
		Workspace:        workerResult.WorkspacePath,
		StartedAt:        entry.StartedAt.Format(time.RFC3339),
		CompletedAt:      now.Format(time.RFC3339),
		Status:           status,
		Error:            errorStringPtr(runError),
		WorkflowFile:     entry.WorkflowFile,
		TurnsCompleted:   workerResult.TurnsCompleted,
		RuleName:         entry.RuleName,
		TemplateID:       entry.TemplateID,
		InputTokens:      entry.AgentInputTokens,
		OutputTokens:     entry.AgentOutputTokens,
		TotalTokens:      entry.AgentTotalTokens,
		CacheReadTokens:  entry.CacheReadTokens,
		TokensMeasured:   measured,
		UnaccountedTurns: workerResult.UnaccountedTurns,
	}
	// A row recording no measurement must carry zero in all four token
	// columns. The reconciliation above can populate them from a worker
	// figure that never asserted a measurement, so zero explicitly.
	if !measured {
		runHistory.InputTokens = 0
		runHistory.OutputTokens = 0
		runHistory.TotalTokens = 0
		runHistory.CacheReadTokens = 0
	}
	if workerResult.ReviewMetadata != nil {
		data, marshalErr := json.Marshal(workerResult.ReviewMetadata)
		if marshalErr != nil {
			log.Warn("failed to marshal review metadata", slog.Any("error", marshalErr))
		} else {
			runHistory.ReviewMetadata = new(string(data))
		}
	}

	// Prefer workerResult.SessionID (authoritative from the adapter) over
	// entry.SessionID, which depends on EventSessionStarted having been
	// processed before exit.
	sessionID := workerResult.SessionID
	if sessionID == "" {
		sessionID = entry.SessionID
	}
	// The entry lags the worker's tally when the context ended before a
	// turn-started message was delivered; a turn that errored still ran.
	turnsSeen := max(entry.TurnCount, workerResult.TurnsStarted)

	// An unmeasured count is stored as zero so a reader cannot find a figure
	// contradicting the qualifier beside it.
	requestsMeasured := apiRequestsMeasured(entry.UsageArrival, turnsSeen, entry.APIRequestCount)
	requestCount := 0
	if requestsMeasured {
		requestCount = entry.APIRequestCount
	}
	sessionMeta := persistence.SessionMetadata{
		IssueID:   workerResult.IssueID,
		SessionID: sessionID,
		// The dispatch's row is cleared here, ahead of the run_history write
		// below: once that row exists, a session_metadata row still keyed to
		// this dispatch would add the run's spend a second time. These are
		// separate writes; a clear that fails while the append succeeds
		// leaves that double count as an accepted risk.
		DispatchID:          "",
		InputTokens:         entry.AgentInputTokens,
		OutputTokens:        entry.AgentOutputTokens,
		TotalTokens:         entry.AgentTotalTokens,
		CacheReadTokens:     entry.CacheReadTokens,
		ModelName:           entry.ModelName,
		APIRequestCount:     requestCount,
		APIRequestsMeasured: requestsMeasured,
		UpdatedAt:           now.Format(time.RFC3339),
	}
	if entry.AgentPID != "" {
		sessionMeta.AgentPID = &entry.AgentPID
	}
	if err := params.Store.UpsertSessionMetadata(ctx, sessionMeta); err != nil {
		log.Error("failed to persist session metadata",
			slog.Any("error", err),
		)
	}

	runHistoryPersisted := true
	if _, err := params.Store.AppendRunHistory(ctx, runHistory); err != nil {
		runHistoryPersisted = false
		log.Error("failed to persist run history",
			slog.Any("error", err),
		)
	}

	if evidenceWorkObserved {
		if err := params.Store.ResetHandoffAbsenceSequence(ctx, workerResult.IssueID); err != nil {
			log.Error("failed to reset handoff absence sequence",
				slog.Any("error", err),
			)
		}
	}

	consecutiveAbsences := 0
	absenceCeiling := handoffAbsenceCeiling(params.MaxConsecutiveAbsences)
	absenceParked := false
	if evidenceWithheld {
		counts, countErr := params.Store.QueryConsecutiveHandoffAbsenceCounts(ctx, []string{workerResult.IssueID})
		if countErr != nil {
			// Conservative fallback when SQLite cannot answer. It may
			// include other failure attempts, which can only stop sooner;
			// it cannot reopen an otherwise exhausted loop.
			consecutiveAbsences = NextAttempt(entry.RetryAttempt)
			log.Warn("handoff absence count query failed, using retry attempt fallback",
				slog.Int("consecutive_absences", consecutiveAbsences),
				slog.Any("error", countErr),
			)
		} else {
			consecutiveAbsences = counts[workerResult.IssueID]
			if !runHistoryPersisted {
				consecutiveAbsences++
			}
			if consecutiveAbsences == 0 {
				consecutiveAbsences = 1
			}
		}

		log.Warn("handoff withheld by evidence policy",
			slog.String("policy", string(workerResult.HandoffEvidencePolicy.Effective())),
			slog.String("verdict", string(evidenceResult.Verdict)),
			slog.String("reason", evidenceResult.Reason),
			slog.Int("turns_completed", workerResult.TurnsCompleted),
			slog.Int("consecutive_absences", consecutiveAbsences),
		)

		if consecutiveAbsences >= absenceCeiling {
			parkHandoffAbsence(
				state,
				ctx,
				params.Store,
				params.TrackerAdapter,
				metrics,
				workerResult.IssueID,
				workerResult.Identifier,
				entry.Issue.DisplayID,
				observation,
				consecutiveAbsences,
				absenceCeiling,
				params.HandoffParkingLabel,
				log,
			)
			absenceParked = true
		}
	}

	aggMetrics := persistence.AggregateMetrics{
		Key:                "agent_totals",
		InputTokens:        state.AgentTotals.InputTokens,
		OutputTokens:       state.AgentTotals.OutputTokens,
		TotalTokens:        state.AgentTotals.TotalTokens,
		CacheReadTokens:    state.AgentTotals.CacheReadTokens,
		SecondsRunning:     state.AgentTotals.SecondsRunning,
		UnmeasuredSessions: state.AgentTotals.UnmeasuredSessions,
		UpdatedAt:          now.Format(time.RFC3339),
	}
	if err := params.Store.UpsertAggregateMetrics(ctx, aggMetrics); err != nil {
		log.Error("failed to persist aggregate metrics",
			slog.Any("error", err),
		)
	}

	retryScheduled := false
	// retryDeferred is true when this exit found the retry slot held by a
	// foreign incumbent and left it untouched. Distinct from retryScheduled:
	// both mean work remains queued, but only retryScheduled means this exit
	// created or replaced the retry entry.
	retryDeferred := false
	// claimRetainedForIncumbent is true on the two arms that keep the claim
	// solely to protect a foreign incumbent (non-active default and
	// successful-handoff). The reaction-enqueue gate treats the claim as
	// released on those arms, so a deferral changes only the retry slot.
	claimRetainedForIncumbent := false
	nextAttempt := 0

	switch workerResult.ExitKind {
	case WorkerExitNormal:
		state.Completed[workerResult.IssueID] = struct{}{}

		_, claimedAtExit := state.Claimed[workerResult.IssueID]
		terminalSuppressed := false

		switch {
		case blockedSoftStop:
			// Blocked agents have no further work; suppress continuation and
			// release the claim.
			log.Info("continuation retry suppressed",
				slog.String("reason", workerResult.SoftStopReason),
			)
			if drivesIssue {
				parkIssue(state, parkIssueParams{
					IssueID:        workerResult.IssueID,
					Identifier:     workerResult.Identifier,
					DisplayID:      entry.Issue.DisplayID,
					ObservedState:  observation,
					Reason:         parkReasonAgentBlocked,
					Label:          params.HandoffParkingLabel,
					Store:          params.Store,
					TrackerAdapter: params.TrackerAdapter,
					Metrics:        metrics,
					Logger:         log,
					Ctx:            ctx,
				})
			} else {
				CancelRetry(state, workerResult.IssueID)
				delete(state.Claimed, workerResult.IssueID)
			}

		case terminal:
			// The tracker already reports a terminal state; overwriting it
			// with the handoff state would undo the operator's own action.
			log.Info("handoff suppressed for terminal issue",
				slog.String("state", observation),
				slog.String("state_source", observationSource),
				slog.String("handoff_state", params.HandoffState),
			)
			if params.HandoffState != "" {
				metrics.IncHandoffTransitions(handoffSkipped)
			}
			terminalSuppressed = true
			CancelRetry(state, workerResult.IssueID)
			delete(state.Claimed, workerResult.IssueID)

		case handoffPath && evidenceWithheld && absenceParked:
			// Parking already cancelled the sequence, deleted any persisted
			// retry, released the claim, and installed the durable runtime gate.

		case handoffPath && evidenceWithheld:
			// A withheld handoff is an unsuccessful disposition even though
			// the process exited normally: keep the claim and use the
			// exponential-backoff failure lane, not the continuation lane.
			if incumbent := retrySlotIncumbent(state, workerResult.IssueID); incumbent != nil {
				logRetrySlotDeferral(log, "evidence-backoff", incumbent)
				nextAttempt = incumbent.Attempt
				retryDeferred = true
			} else {
				nextAttempt = NextAttempt(entry.RetryAttempt)
				delayMS := computeBackoffDelay(nextAttempt, params.MaxRetryBackoffMS)
				log.Warn("handoff evidence failure, scheduling retry",
					slog.Any("error", evidenceErr),
					slog.Int("next_attempt", nextAttempt),
					slog.Int64("delay_ms", delayMS),
				)
				ScheduleRetry(state, ScheduleRetryParams{
					IssueID:             workerResult.IssueID,
					Identifier:          workerResult.Identifier,
					DisplayID:           entry.Issue.DisplayID,
					Attempt:             nextAttempt,
					DelayMS:             delayMS,
					Error:               "worker exited: " + evidenceErr.Error(),
					LastSSHHost:         workerResult.SSHHost,
					ContinuationContext: entry.ContinuationContext,
					ReactionKind:        entry.ReactionKind,
					AgentKind:           entry.AgentKind,
					RuleName:            entry.RuleName,
					TemplateID:          entry.TemplateID,
					Logger:              log,
				}, params.OnRetryFire)
				metrics.IncRetries(triggerError)
				retryScheduled = true
			}

		case handoffPath:
			// Handoff: issue active and handoff_state configured. The target
			// is resolved once, ahead of every record and the transition, so
			// a declared run's target and provenance are visible without
			// reading workspace state.
			resolvedTarget, noChangeDeclared := resolveExitTarget(params, workerResult)

			// Guard against nil TrackerAdapter (misconfiguration or a test
			// that sets HandoffState without an adapter).
			if params.TrackerAdapter == nil {
				metrics.IncHandoffTransitions(handoffError)
				if workerResult.SoftStop {
					log.Warn("handoff configured but tracker adapter is nil, releasing claim",
						slog.String("handoff_state", params.HandoffState),
						slog.String("target_state", resolvedTarget),
						slog.Bool("no_change_declared", noChangeDeclared),
					)
					CancelRetry(state, workerResult.IssueID)
					delete(state.Claimed, workerResult.IssueID)
				} else if incumbent := retrySlotIncumbent(state, workerResult.IssueID); incumbent != nil {
					logRetrySlotDeferral(log, triggerContinuation, incumbent)
					retryDeferred = true
				} else {
					log.Warn("handoff configured but tracker adapter is nil, scheduling continuation retry",
						slog.String("handoff_state", params.HandoffState),
						slog.String("target_state", resolvedTarget),
						slog.Bool("no_change_declared", noChangeDeclared),
					)
					ScheduleRetry(state, ScheduleRetryParams{
						IssueID:     workerResult.IssueID,
						Identifier:  workerResult.Identifier,
						DisplayID:   entry.Issue.DisplayID,
						Attempt:     NextAttempt(entry.RetryAttempt),
						DelayMS:     continuationDelayMS,
						Error:       "",
						LastSSHHost: workerResult.SSHHost,
						SessionID:   sessionID,
						AgentKind:   entry.AgentKind,
						RuleName:    entry.RuleName,
						TemplateID:  entry.TemplateID,
						Logger:      log,
					}, params.OnRetryFire)
					metrics.IncRetries(triggerContinuation)
					retryScheduled = true
				}
			} else {
				// Verify the tracker state immediately before the write: the
				// resolved observation may be stale by the time teardown
				// completed. With no terminal states configured the read
				// cannot suppress the write, so skip it.
				verifiedTerminal := false
				if len(params.TerminalStates) > 0 {
					if verified, verifyErr := params.TrackerAdapter.FetchIssueStatesByIDs(ctx, []string{workerResult.IssueID}); verifyErr != nil {
						log.Warn("handoff verification read failed, proceeding with handoff",
							slog.Any("error", verifyErr),
							slog.String("state_source", observationSource),
							slog.String("target_state", resolvedTarget),
							slog.Bool("no_change_declared", noChangeDeclared),
						)
					} else if verifiedState, ok := verified[workerResult.IssueID]; ok && isTerminalState(verifiedState, params.TerminalStates) {
						log.Info("handoff suppressed for terminal issue",
							slog.String("state", verifiedState),
							slog.String("state_source", "verified"),
							slog.String("handoff_state", params.HandoffState),
							slog.String("target_state", resolvedTarget),
							slog.Bool("no_change_declared", noChangeDeclared),
						)
						if params.HandoffState != "" {
							metrics.IncHandoffTransitions(handoffSkipped)
						}
						terminalSuppressed = true
						verifiedTerminal = true
					}
				}

				if verifiedTerminal {
					CancelRetry(state, workerResult.IssueID)
					delete(state.Claimed, workerResult.IssueID)
				} else if err := params.TrackerAdapter.TransitionIssue(ctx, workerResult.IssueID, resolvedTarget); err != nil {
					metrics.IncHandoffTransitions(handoffError)
					if workerResult.SoftStop {
						log.Warn("handoff transition failed, releasing claim",
							slog.String("handoff_state", params.HandoffState),
							slog.String("target_state", resolvedTarget),
							slog.Bool("no_change_declared", noChangeDeclared),
							slog.Any("error", err),
						)
						CancelRetry(state, workerResult.IssueID)
						delete(state.Claimed, workerResult.IssueID)
					} else if incumbent := retrySlotIncumbent(state, workerResult.IssueID); incumbent != nil {
						logRetrySlotDeferral(log, triggerContinuation, incumbent)
						retryDeferred = true
					} else {
						log.Warn("handoff transition failed, scheduling continuation retry",
							slog.String("handoff_state", params.HandoffState),
							slog.String("target_state", resolvedTarget),
							slog.Bool("no_change_declared", noChangeDeclared),
							slog.Any("error", err),
						)
						ScheduleRetry(state, ScheduleRetryParams{
							IssueID:     workerResult.IssueID,
							Identifier:  workerResult.Identifier,
							DisplayID:   entry.Issue.DisplayID,
							Attempt:     NextAttempt(entry.RetryAttempt),
							DelayMS:     continuationDelayMS,
							Error:       "",
							LastSSHHost: workerResult.SSHHost,
							SessionID:   sessionID,
							AgentKind:   entry.AgentKind,
							RuleName:    entry.RuleName,
							TemplateID:  entry.TemplateID,
							Logger:      log,
						}, params.OnRetryFire)
						metrics.IncRetries(triggerContinuation)
						retryScheduled = true
					}
				} else if incumbent := retrySlotIncumbent(state, workerResult.IssueID); incumbent != nil {
					// A successful handoff is not a stop signal: work queued
					// during the session is still valid, so the incumbent is
					// kept rather than cancelled.
					logRetrySlotDeferral(log, triggerContinuation, incumbent)
					metrics.IncHandoffTransitions(handoffSuccess)
					log.Info("handoff transition succeeded, incumbent preserved",
						slog.String("handoff_state", params.HandoffState),
						slog.String("target_state", resolvedTarget),
						slog.Bool("no_change_declared", noChangeDeclared),
					)
					retryDeferred = true
					claimRetainedForIncumbent = true
				} else {
					log.Info("handoff transition succeeded, releasing claim",
						slog.String("handoff_state", params.HandoffState),
						slog.String("target_state", resolvedTarget),
						slog.Bool("no_change_declared", noChangeDeclared),
					)
					metrics.IncHandoffTransitions(handoffSuccess)
					CancelRetry(state, workerResult.IssueID)
					delete(state.Claimed, workerResult.IssueID)
				}
			}

		case workerResult.SoftStop:
			// Catch-all for soft-stop reasons not handled above. Release the
			// claim without retry.
			if workerResult.SoftStopReason != string(workspace.StatusBlocked) &&
				workerResult.SoftStopReason != string(workspace.StatusNeedsHumanReview) &&
				workerResult.SoftStopReason != string(workspace.StatusNoChangeNeeded) {
				log.Warn("unrecognized soft-stop reason",
					slog.String("reason", workerResult.SoftStopReason),
				)
			}
			log.Info("continuation retry suppressed",
				slog.String("reason", workerResult.SoftStopReason),
			)
			CancelRetry(state, workerResult.IssueID)
			delete(state.Claimed, workerResult.IssueID)

		case issueIsActive && drivesIssue:
			// No handoff configured but issue still active: schedule
			// continuation retry unless the slot is occupied.
			if incumbent := retrySlotIncumbent(state, workerResult.IssueID); incumbent != nil {
				logRetrySlotDeferral(log, triggerContinuation, incumbent)
				retryDeferred = true
			} else {
				ScheduleRetry(state, ScheduleRetryParams{
					IssueID:     workerResult.IssueID,
					Identifier:  workerResult.Identifier,
					DisplayID:   entry.Issue.DisplayID,
					Attempt:     NextAttempt(entry.RetryAttempt),
					DelayMS:     continuationDelayMS,
					Error:       "",
					LastSSHHost: workerResult.SSHHost,
					SessionID:   sessionID,
					AgentKind:   entry.AgentKind,
					RuleName:    entry.RuleName,
					TemplateID:  entry.TemplateID,
					Logger:      log,
				}, params.OnRetryFire)
				metrics.IncRetries(triggerContinuation)
				retryScheduled = true
			}

		default:
			// Issue not active: cancel any pending retry and release the
			// claim, unless a foreign incumbent holds the slot, which is
			// exactly the population this arm would otherwise strand.
			if params.HandoffState != "" {
				metrics.IncHandoffTransitions(handoffSkipped)
			}
			if incumbent := retrySlotIncumbent(state, workerResult.IssueID); incumbent != nil {
				logRetrySlotDeferral(log, triggerContinuation, incumbent)
				retryDeferred = true
				claimRetainedForIncumbent = true
			} else {
				CancelRetry(state, workerResult.IssueID)
				delete(state.Claimed, workerResult.IssueID)
			}
		}

		_, stillClaimed := state.Claimed[workerResult.IssueID]
		// A claim retained solely to protect an incumbent must not widen
		// reaction seeding, so evaluate as if it had been released.
		if claimRetainedForIncumbent {
			stillClaimed = false
		}
		reactionEnqueueAllowed := claimedAtExit && (handoffPath || stillClaimed) && !terminalSuppressed

		// Seed a pending CI check when CI provider and SCM adapter are both
		// configured and the workspace carries PR identity: the reaction
		// cannot resolve a head for a branch with no PR. Handoff paths stay
		// eligible even after the claim is released.
		if params.CIProvider != nil && params.SCMAdapter != nil && workerResult.WorkspacePath != "" {
			if reactionEnqueueAllowed {
				scm := workspace.ReadSCMMetadata(workerResult.WorkspacePath, log)
				if scm.PRNumber > 0 && scm.Owner != "" && scm.Repo != "" && scm.Branch != "" {
					nowCI := time.Now().UTC()
					if params.NowFunc != nil {
						nowCI = params.NowFunc().UTC()
					}
					rkey := ReactionKey(workerResult.IssueID, ReactionKindCI)
					state.PendingReactions[rkey] = &PendingReaction{
						IssueID:     workerResult.IssueID,
						Identifier:  workerResult.Identifier,
						DisplayID:   entry.Issue.DisplayID,
						Attempt:     normalizeAttempt(entry.RetryAttempt) + 1,
						Kind:        ReactionKindCI,
						LastSSHHost: workerResult.SSHHost,
						CreatedAt:   nowCI,
						KindData: &CIReactionData{
							PRNumber: scm.PRNumber,
							Owner:    scm.Owner,
							Repo:     scm.Repo,
							Branch:   scm.Branch,
							SHA:      scm.SHA,
						},
						AgentKind:  entry.AgentKind,
						RuleName:   entry.RuleName,
						TemplateID: entry.TemplateID,
					}
				} else if scm.Branch != "" {
					log.Debug("ci watch not seeded: workspace metadata missing pull request identity",
						slog.Int("pr_number", scm.PRNumber),
						slog.String("owner", scm.Owner),
						slog.String("repo", scm.Repo),
					)
				}
			}
		}

		// Seed a pending review check when the SCM adapter is configured and
		// the workspace has PR metadata with repository identity.
		if params.SCMAdapter != nil && workerResult.WorkspacePath != "" {
			if reactionEnqueueAllowed {
				scm := workspace.ReadSCMMetadata(workerResult.WorkspacePath, log)
				if scm.PRNumber > 0 && scm.Branch != "" && scm.Owner != "" && scm.Repo != "" {
					nowReview := time.Now().UTC()
					if params.NowFunc != nil {
						nowReview = params.NowFunc().UTC()
					}
					rkey := ReactionKey(workerResult.IssueID, ReactionKindReview)
					// Skip if already present to preserve in-progress debounce.
					if _, exists := state.PendingReactions[rkey]; !exists {
						state.PendingReactions[rkey] = &PendingReaction{
							IssueID:     workerResult.IssueID,
							Identifier:  workerResult.Identifier,
							DisplayID:   entry.Issue.DisplayID,
							Attempt:     normalizeAttempt(entry.RetryAttempt) + 1,
							Kind:        ReactionKindReview,
							LastSSHHost: workerResult.SSHHost,
							CreatedAt:   nowReview,
							KindData: &ReviewReactionData{
								PRNumber: scm.PRNumber,
								Owner:    scm.Owner,
								Repo:     scm.Repo,
								Branch:   scm.Branch,
								SHA:      scm.SHA,
							},
							AgentKind:  entry.AgentKind,
							RuleName:   entry.RuleName,
							TemplateID: entry.TemplateID,
						}
					}
				}
			}
		}

		// Seed a pending bot-review check when the SCM adapter is configured,
		// bot-review is enabled, and the workspace has PR metadata.
		// Independent of the review-kind enqueue: both fire from the same exit.
		if params.SCMAdapter != nil && params.BotReviewReactionConfigured && workerResult.WorkspacePath != "" {
			if reactionEnqueueAllowed {
				scm := workspace.ReadSCMMetadata(workerResult.WorkspacePath, log)
				if scm.PRNumber > 0 && scm.Branch != "" && scm.Owner != "" && scm.Repo != "" {
					nowBotReview := time.Now().UTC()
					if params.NowFunc != nil {
						nowBotReview = params.NowFunc().UTC()
					}
					rkey := ReactionKey(workerResult.IssueID, ReactionKindBotReview)
					if _, exists := state.PendingReactions[rkey]; !exists {
						state.PendingReactions[rkey] = &PendingReaction{
							IssueID:     workerResult.IssueID,
							Identifier:  workerResult.Identifier,
							DisplayID:   entry.Issue.DisplayID,
							Attempt:     normalizeAttempt(entry.RetryAttempt) + 1,
							Kind:        ReactionKindBotReview,
							LastSSHHost: workerResult.SSHHost,
							CreatedAt:   nowBotReview,
							KindData: &BotReviewReactionData{
								PRNumber: scm.PRNumber,
								Owner:    scm.Owner,
								Repo:     scm.Repo,
								Branch:   scm.Branch,
								SHA:      scm.SHA,
							},
							AgentKind:  entry.AgentKind,
							RuleName:   entry.RuleName,
							TemplateID: entry.TemplateID,
						}
					}
				}
			}
		}

		// Seed a pending auto-merge entry when the SCM adapter is configured,
		// auto-merge is enabled, and the workspace has PR metadata.
		if params.SCMAdapter != nil && params.AutoMergeReactionConfigured && workerResult.WorkspacePath != "" {
			if reactionEnqueueAllowed {
				scm := workspace.ReadSCMMetadata(workerResult.WorkspacePath, log)
				if scm.PRNumber > 0 && scm.Branch != "" && scm.Owner != "" && scm.Repo != "" {
					nowMerge := time.Now().UTC()
					if params.NowFunc != nil {
						nowMerge = params.NowFunc().UTC()
					}
					rkey := ReactionKey(workerResult.IssueID, ReactionKindAutoMerge)
					if _, exists := state.PendingReactions[rkey]; !exists {
						state.PendingReactions[rkey] = &PendingReaction{
							IssueID:     workerResult.IssueID,
							Identifier:  workerResult.Identifier,
							DisplayID:   entry.Issue.DisplayID,
							Attempt:     normalizeAttempt(entry.RetryAttempt) + 1,
							Kind:        ReactionKindAutoMerge,
							LastSSHHost: workerResult.SSHHost,
							CreatedAt:   nowMerge,
							KindData: &AutoMergeReactionData{
								PRNumber: scm.PRNumber,
								Owner:    scm.Owner,
								Repo:     scm.Repo,
								Branch:   scm.Branch,
								SHA:      scm.SHA,
							},
							AgentKind:  entry.AgentKind,
							RuleName:   entry.RuleName,
							TemplateID: entry.TemplateID,
						}
					}
				}
			}
		}

		// Seed a pending merge-conflict entry when the SCM adapter is
		// configured, merge-conflict is enabled, and the workspace has PR
		// metadata.
		if params.SCMAdapter != nil && params.MergeConflictReactionConfigured && workerResult.WorkspacePath != "" {
			if reactionEnqueueAllowed {
				scm := workspace.ReadSCMMetadata(workerResult.WorkspacePath, log)
				if scm.PRNumber > 0 && scm.Branch != "" && scm.Owner != "" && scm.Repo != "" {
					nowMergeConflict := time.Now().UTC()
					if params.NowFunc != nil {
						nowMergeConflict = params.NowFunc().UTC()
					}
					rkey := ReactionKey(workerResult.IssueID, ReactionKindMergeConflict)
					if _, exists := state.PendingReactions[rkey]; !exists {
						state.PendingReactions[rkey] = &PendingReaction{
							IssueID:     workerResult.IssueID,
							Identifier:  workerResult.Identifier,
							DisplayID:   entry.Issue.DisplayID,
							Attempt:     normalizeAttempt(entry.RetryAttempt) + 1,
							Kind:        ReactionKindMergeConflict,
							LastSSHHost: workerResult.SSHHost,
							CreatedAt:   nowMergeConflict,
							KindData: &MergeConflictReactionData{
								PRNumber: scm.PRNumber,
								Owner:    scm.Owner,
								Repo:     scm.Repo,
								Branch:   scm.Branch,
								SHA:      scm.SHA,
							},
							AgentKind:  entry.AgentKind,
							RuleName:   entry.RuleName,
							TemplateID: entry.TemplateID,
						}
					}
				}
			}
		}

		// Seed a pending label-review entry when the SCM adapter is
		// configured, label-review is enabled, and the workspace has PR
		// metadata. No branch is needed (read-only has no checkout). A
		// read-only session's own exit never seeds: reactionEnqueueAllowed is
		// always false for it, since it is excluded from the handoff path and
		// releases the claim.
		if params.SCMAdapter != nil && params.LabelReviewReactionConfigured && workerResult.WorkspacePath != "" {
			if reactionEnqueueAllowed {
				scm := workspace.ReadSCMMetadata(workerResult.WorkspacePath, log)
				if scm.PRNumber > 0 && scm.Owner != "" && scm.Repo != "" {
					nowLabelReview := time.Now().UTC()
					if params.NowFunc != nil {
						nowLabelReview = params.NowFunc().UTC()
					}
					rkey := ReactionKey(workerResult.IssueID, ReactionKindLabelReview)
					if _, exists := state.PendingReactions[rkey]; !exists {
						state.PendingReactions[rkey] = &PendingReaction{
							IssueID:     workerResult.IssueID,
							Identifier:  workerResult.Identifier,
							DisplayID:   entry.Issue.DisplayID,
							Attempt:     normalizeAttempt(entry.RetryAttempt) + 1,
							Kind:        ReactionKindLabelReview,
							LastSSHHost: workerResult.SSHHost,
							CreatedAt:   nowLabelReview,
							KindData: &LabelReviewReactionData{
								PRNumber: scm.PRNumber,
								Owner:    scm.Owner,
								Repo:     scm.Repo,
							},
							AgentKind:  entry.AgentKind,
							RuleName:   entry.RuleName,
							TemplateID: entry.TemplateID,
						}
					}
				}
			}
		}

		// Seed a pending label-fix entry when the SCM adapter is configured,
		// label-fix is enabled, and the workspace has PR metadata with a head
		// branch (the fix session checks it out). A fix session's own exit
		// never seeds (reactionEnqueueAllowed is false); repeatability comes
		// from the reconcile re-enqueue on dispatch.
		if params.SCMAdapter != nil && params.LabelFixReactionConfigured && workerResult.WorkspacePath != "" {
			if reactionEnqueueAllowed {
				scm := workspace.ReadSCMMetadata(workerResult.WorkspacePath, log)
				if scm.PRNumber > 0 && scm.Owner != "" && scm.Repo != "" && scm.Branch != "" {
					nowLabelFix := time.Now().UTC()
					if params.NowFunc != nil {
						nowLabelFix = params.NowFunc().UTC()
					}
					rkey := ReactionKey(workerResult.IssueID, ReactionKindLabelFix)
					if _, exists := state.PendingReactions[rkey]; !exists {
						state.PendingReactions[rkey] = &PendingReaction{
							IssueID:     workerResult.IssueID,
							Identifier:  workerResult.Identifier,
							DisplayID:   entry.Issue.DisplayID,
							Attempt:     normalizeAttempt(entry.RetryAttempt) + 1,
							Kind:        ReactionKindLabelFix,
							LastSSHHost: workerResult.SSHHost,
							CreatedAt:   nowLabelFix,
							KindData: &LabelFixReactionData{
								PRNumber: scm.PRNumber,
								Owner:    scm.Owner,
								Repo:     scm.Repo,
								Branch:   scm.Branch,
							},
							AgentKind:  entry.AgentKind,
							RuleName:   entry.RuleName,
							TemplateID: entry.TemplateID,
						}
					}
				}
			}
		}

		// Seed a pending merge-completion entry when the SCM adapter is
		// configured, merge-completion is enabled, and the workspace has PR
		// metadata. No branch is needed (the pass performs no checkout).
		if params.SCMAdapter != nil && params.MergeCompletionReactionConfigured && workerResult.WorkspacePath != "" {
			if reactionEnqueueAllowed {
				scm := workspace.ReadSCMMetadata(workerResult.WorkspacePath, log)
				if scm.PRNumber > 0 && scm.Owner != "" && scm.Repo != "" {
					nowMergeCompletion := time.Now().UTC()
					if params.NowFunc != nil {
						nowMergeCompletion = params.NowFunc().UTC()
					}
					rkey := ReactionKey(workerResult.IssueID, ReactionKindMergeCompletion)
					if _, exists := state.PendingReactions[rkey]; !exists {
						state.PendingReactions[rkey] = &PendingReaction{
							IssueID:     workerResult.IssueID,
							Identifier:  workerResult.Identifier,
							DisplayID:   entry.Issue.DisplayID,
							Attempt:     normalizeAttempt(entry.RetryAttempt) + 1,
							Kind:        ReactionKindMergeCompletion,
							LastSSHHost: workerResult.SSHHost,
							CreatedAt:   nowMergeCompletion,
							KindData: &MergeCompletionReactionData{
								PRNumber: scm.PRNumber,
								Owner:    scm.Owner,
								Repo:     scm.Repo,
							},
							AgentKind:  entry.AgentKind,
							RuleName:   entry.RuleName,
							TemplateID: entry.TemplateID,
						}
					}
				}
			}
		}

	case WorkerExitCancelled:
		// Release the claim only if reconciliation stall detection has not
		// pre-scheduled a retry, which needs the claim to prevent duplicate
		// dispatch.
		if _, hasRetry := state.RetryAttempts[workerResult.IssueID]; !hasRetry {
			delete(state.Claimed, workerResult.IssueID)
		}

	default: // WorkerExitError and any unknown kind
		classification := classifyWorkerError(workerResult.Error)
		if classification.Retryable {
			if incumbent := retrySlotIncumbent(state, workerResult.IssueID); incumbent != nil {
				logRetrySlotDeferral(log, "error-backoff", incumbent)
				nextAttempt = incumbent.Attempt
				retryDeferred = true
			} else {
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
					IssueID:             workerResult.IssueID,
					Identifier:          workerResult.Identifier,
					DisplayID:           entry.Issue.DisplayID,
					Attempt:             nextAttempt,
					DelayMS:             delayMS,
					Error:               errMsg,
					LastSSHHost:         workerResult.SSHHost,
					ContinuationContext: entry.ContinuationContext,
					ReactionKind:        entry.ReactionKind,
					AgentKind:           entry.AgentKind,
					RuleName:            entry.RuleName,
					TemplateID:          entry.TemplateID,
					Logger:              log,
				}, params.OnRetryFire)
				metrics.IncRetries(triggerError)
				retryScheduled = true
			}
		} else {
			log.Error("worker run failed, non-retryable, releasing claim",
				slog.Any("error", workerResult.Error),
			)
			delete(state.Claimed, workerResult.IssueID)
		}
	}

	if retryScheduled {
		if retryEntry, ok := state.RetryAttempts[workerResult.IssueID]; ok {
			if !isKnownReactionKind(retryEntry.ReactionKind) {
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
				if err := params.Store.SaveRetryEntry(ctx, pEntry); err != nil {
					log.Error("failed to persist retry entry",
						slog.Any("error", err),
					)
				}
			}
		}
	}

	// Build the comment text synchronously to capture exit-time data, then
	// fire CommentIssue in a detached goroutine so the event loop never
	// blocks.
	var commentText string
	var lifecycle string

	sessionID = workerResult.SessionID
	if sessionID == "" {
		sessionID = entry.SessionID
	}
	runDuration := max(now.Sub(entry.StartedAt), 0)

	// A deferral leaves work queued exactly as a scheduled retry does, so the
	// comments report re-queuing for either outcome.
	retryPending := retryScheduled || retryDeferred

	switch workerResult.ExitKind {
	case WorkerExitNormal:
		if evidenceWithheld {
			if params.CommentsConfig.OnFailure {
				commentText = buildFailureComment(sessionID, runDuration, evidenceErr, retryPending, nextAttempt)
				lifecycle = "failure"
			}
		} else if params.CommentsConfig.OnCompletion {
			if workerResult.SoftStop {
				commentText = buildSoftStopComment(sessionID, runDuration, workerResult.TurnsCompleted, workerResult.SoftStopReason)
			} else {
				commentText = buildCompletionComment(sessionID, runDuration, workerResult.TurnsCompleted, retryPending)
			}
			lifecycle = "completion"
		}
	case WorkerExitCancelled:
		// No comment on cancellation.
	default:
		if params.CommentsConfig.OnFailure {
			commentText = buildFailureComment(sessionID, runDuration, workerResult.Error, retryPending, nextAttempt)
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

		state.TrackerOpsWg.Go(func() {
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
		})
	}

}

// computeBackoffDelay returns the exponential backoff delay in ms for the
// given attempt, capped by maxRetryBackoffMS:
//
//	delay = min(10000 * 2^(attempt-1), maxRetryBackoffMS)
//
// A non-positive maxRetryBackoffMS uses the 5-minute default; attempt <= 0
// is treated as 1.
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

// classifyWorkerError extracts the retry classification from a worker error,
// unwrapping for [domain.AgentError] or [domain.TrackerError]. Returns
// retryable-with-exponential-backoff when err is nil or unclassified.
func classifyWorkerError(err error) domain.RetryClassification {
	if err == nil {
		return domain.RetryClassification{Retryable: true, Backoff: domain.BackoffExponential}
	}

	if agentErr, ok := errors.AsType[*domain.AgentError](err); ok {
		return agentErr.Kind.RetryClassification()
	}

	if trackerErr, ok := errors.AsType[*domain.TrackerError](err); ok {
		return trackerErr.Kind.RetryClassification()
	}

	return domain.RetryClassification{Retryable: true, Backoff: domain.BackoffExponential}
}

func errorStringPtr(err error) *string {
	if err == nil {
		return nil
	}
	return new(err.Error())
}

func stringPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// buildCompletionComment returns the tracker comment for a normal exit.
// retryScheduled distinguishes "completed (re-queuing)" from "completed".
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

// buildFailureComment returns the tracker comment for an error exit.
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

// buildSoftStopComment returns the tracker comment for an exit triggered by
// a recognized A2O status signal.
func buildSoftStopComment(sessionID string, elapsed time.Duration, turnsCompleted int, reason string) string {
	if sessionID == "" {
		sessionID = "unknown"
	}
	return fmt.Sprintf("Sortie session completed (agent signaled: %s).\nSession: %s\nDuration: %s\nTurns: %d",
		reason, sessionID, elapsed.Truncate(time.Second).String(), turnsCompleted)
}
