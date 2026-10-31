package orchestrator

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/notify/route"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/pricing"
	"github.com/sortie-ai/sortie/internal/redact"
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
	AppendRunHistoryWithEvents(ctx context.Context, run persistence.RunHistory, events []persistence.RunEvent, limits persistence.RunEventLimits) (persistence.RunHistory, error)
	UpsertAggregateMetrics(ctx context.Context, metrics persistence.AggregateMetrics) error
	UpsertSessionMetadata(ctx context.Context, meta persistence.SessionMetadata) error
	SaveRetryEntry(ctx context.Context, entry persistence.RetryEntry) error
	DeleteRetryEntry(ctx context.Context, issueID string) error
	DeleteLivePass(ctx context.Context, issueID, kind string) error
	DeleteReactionEscalations(ctx context.Context, issueID string) error
	QueryConsecutiveHandoffAbsenceCounts(ctx context.Context, issueIDs []string) (map[string]int, error)
	ResetHandoffAbsenceSequence(ctx context.Context, issueID string) error
	UpsertParkedIssue(ctx context.Context, entry persistence.ParkedIssue) error
	DeleteParkedIssue(ctx context.Context, issueID string) error
	CountWorkerRunsCompletedSince(ctx context.Context, issueID string, since time.Time) (int, error)
	AddReactionHandedOffComments(ctx context.Context, issueID, kind string, commentIDs []string) error
	RecordStageHop(ctx context.Context, hop persistence.StageHop) error
	DeleteStageHop(ctx context.Context, issueID string) error
	RecordRunStageResult(ctx context.Context, runID int64, targetRule, result string) error
	RecordIssueOutcome(ctx context.Context, outcome persistence.IssueOutcome) error
	RecordReactionActivity(ctx context.Context, records []persistence.ReactionActivity) error
	SaveUsageLimitHold(ctx context.Context, hold persistence.UsageLimitHold) error
	DeleteUsageLimitHold(ctx context.Context, agentKind string, modelOnly bool, model string) error
}

// HandleWorkerExitParams holds the dependencies for [HandleWorkerExit] that
// are not part of the core [State], separating pure state mutation from I/O.
type HandleWorkerExitParams struct {
	Store WorkerExitStore

	// RunEventRetry holds the events of a run whose events part failed to
	// store and retries them. Nil holds nothing: a failed events part ends
	// at its warning.
	RunEventRetry *RunEventRetry

	MaxRetryBackoffMS int

	// ReleaseHeld releases the claim of a run refused by a usage limit
	// instead of scheduling its retry; the persisted hold stays for a later
	// process.
	ReleaseHeld bool

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

	// Rates are the rates of the configuration loaded for this exit. A reload
	// can land between ticks, so the run is priced under these. Nil leaves
	// [State.Rates] as it is.
	Rates *pricing.Rates

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

	// Dispatch is the dispatch configuration in force, which holds the
	// rule's next link and the hop ceiling read at this exit.
	Dispatch config.DispatchConfig

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

	// Router selects the destinations of the session events the exit
	// publishes. Nil routes nothing.
	Router *route.Router

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

	ReviewReactionConfigured bool

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

// retainStartupFailure retains a startup_failed event carrying the start
// error when the session failed to start and its adapter emitted none. A
// start that ended in cancellation retains none, because Sortie stopped it.
func retainStartupFailure(entry *RunningEntry, result WorkerResult, at time.Time) {
	if result.StartupError == nil || result.ExitKind != WorkerExitError ||
		entry.events == nil || entry.events.hasStartupFailed() {
		return
	}
	retainEvent(entry, domain.AgentEvent{
		Type:      domain.EventStartupFailed,
		Timestamp: at,
		Message:   result.StartupError.Error(),
	})
}

// HandleWorkerExit processes a worker's terminal outcome: removes the running
// entry, updates totals, persists to SQLite, and schedules the appropriate
// retry. It returns the outcome of the attempt it recorded, or "" for a result
// with no running entry. Must be called from the single-writer event loop.
func HandleWorkerExit(state *State, workerResult WorkerResult, params HandleWorkerExitParams) RunOutcome {
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
		return ""
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
	if workerResult.ModelName != "" && workerResult.ModelName != entry.ModelName {
		metrics.PrepareModel(workerResult.ModelName)
	}
	applyUsageDelta(state, entry, workerResult.Usage, workerResult.UsageByModel, workerResult.ModelName, metrics)

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
	model := metricModel("", entry)
	metrics.IncWorkerExits(exitType, model)
	metrics.ObserveWorkerDuration(exitType, elapsed)
	metrics.AddAgentRuntime(model, elapsed)

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

	runError := workerResult.Error

	// Distinct from a stall, terminal-state, or shutdown cancel: attribute
	// the stop to the budget.
	request := entry.CeilingStopRequest
	status := attemptStatus(workerResult.ExitKind, workerResult.StoppedByCeiling && request != nil, workerResult.Error)
	if status == "budget_stopped" {
		runError = ceilingStopError(state, entry, request)
		reportCeilingStop(log, metrics, request)
	} else if cancelErr := cancelRunError(workerResult.CancelCause); workerResult.ExitKind == WorkerExitCancelled && cancelErr != nil {
		runError = cancelErr
	}

	if evidenceWithheld {
		status = "failed"
		runError = evidenceErr
	}

	var usageReset time.Time
	var usageLimit *domain.UsageLimit
	var usageWaits, usageRefused bool
	if workerResult.ExitKind == WorkerExitError {
		usageReset, usageLimit, usageWaits = usageLimitReset(workerResult.Error, now)
		usageRefused = isUsageLimitRefusal(workerResult.Error)
	}
	// A run with no token figure, or one known only in vendor units, cannot
	// prove it did no work, so it keeps counting toward the session budget.
	if usageWaits && measured && entry.AgentOutputTokens == 0 {
		status = "usage_limited"
	}

	issueIdentity := issueIdentityOf(entry.Issue)

	// Prefer workerResult.SessionID (authoritative from the adapter) over
	// entry.SessionID, which depends on EventSessionStarted having been
	// processed before exit.
	sessionID := workerResult.SessionID
	if sessionID == "" {
		sessionID = entry.SessionID
	}

	// A retry restored from a row that never stored what started it holds
	// neither a trigger nor a reaction kind; its origin is unknown, not none.
	var trigger *string
	if entry.RetryAttempt == nil || entry.Trigger != "" || entry.ReactionKind != "" {
		trigger = new(entry.Trigger)
	}

	declaredToken, declaredStatement := declaredOutcome(workerResult)
	outcome := runOutcomeOf(status, declaredToken)

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
		ConfiguredModel:  entry.ConfiguredModel,
		ConfiguredEffort: entry.ConfiguredEffort,
		ReportedModel:    entry.ModelName,
		InputTokens:      entry.AgentInputTokens,
		OutputTokens:     entry.AgentOutputTokens,
		TotalTokens:      entry.AgentTotalTokens,
		CacheReadTokens:  entry.CacheReadTokens,
		CacheWriteTokens: entry.CacheWriteTokens,
		TokensMeasured:   measured,
		UnaccountedTurns: workerResult.UnaccountedTurns,

		UsageCompleteness: persistence.UsageCompletenessOf(measured, workerResult.UnaccountedTurns),
		ReportedAmounts:   runReportedAmounts(usage.MergeAmounts(entry.ReportedAmounts, workerResult.ReportedAmounts)),

		IssueTitle: issueIdentity.Title,
		IssueURL:   issueIdentity.URL,

		StagePrevious:        entry.StageLineage.PreviousRule,
		StagePreviousOutcome: entry.StageLineage.PreviousOutcome,
		ChainID:              entry.StageLineage.ChainID,
		DispatchedAt:         formatDispatchedAt(entry.StageLineage.DispatchedAt),

		Trigger:      trigger,
		ReactionKind: entry.ReactionKind,
		SessionID:    sessionID,
		SSHHost:      entry.SSHHost,

		FirstAfterHop:   entry.FirstAfterHop,
		AgentBlocked:    blockedSoftStop,
		DeclaredOutcome: declaredToken,
		AgentText:       declaredStatement,
	}
	// A row recording no measurement must carry zero in all five token
	// columns. The reconciliation above can populate them from a worker
	// figure that never asserted a measurement, so zero explicitly.
	if !measured {
		runHistory.InputTokens = 0
		runHistory.OutputTokens = 0
		runHistory.TotalTokens = 0
		runHistory.CacheReadTokens = 0
		runHistory.CacheWriteTokens = 0
	} else {
		runHistory.ModelUsage = runModelUsage(entry.UsageByModel)
		runHistory.UsageProvenance = usage.FormatProvenance(workerResult.UsageProvenance)
	}
	if params.Rates != nil {
		state.Rates = params.Rates
	}
	recordRunCost(&runHistory, state.Rates)
	if workerResult.ReviewMetadata != nil {
		masked := maskedReviewMetadata(*workerResult.ReviewMetadata)
		data, marshalErr := json.Marshal(masked)
		if marshalErr != nil {
			log.Warn("failed to marshal review metadata", slog.Any("error", marshalErr))
		} else {
			runHistory.ReviewMetadata = new(string(data))
		}
	}
	// A nil slice marshals to null, which would read as a row that recorded
	// no skill calls.
	calls := entry.Skills
	if calls == nil {
		calls = []domain.SkillCall{}
	}
	if data, marshalErr := json.Marshal(calls); marshalErr != nil {
		log.Warn("failed to marshal skill calls", slog.Any("error", marshalErr))
	} else {
		runHistory.Skills = new(string(data))
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
		// leaves that double count as an accepted risk. The live estimate is
		// cleared for the same reason.
		DispatchID:          "",
		EstimatedCostUSD:    nil,
		CostStatus:          "",
		InputTokens:         entry.AgentInputTokens,
		OutputTokens:        entry.AgentOutputTokens,
		TotalTokens:         entry.AgentTotalTokens,
		CacheReadTokens:     entry.CacheReadTokens,
		CacheWriteTokens:    entry.CacheWriteTokens,
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

	retainStartupFailure(entry, workerResult, now)
	var eventRows []persistence.RunEvent
	if entry.events != nil {
		eventRows = entry.events.storeRows()
	}
	runHistory.DispatchID = new(entry.DispatchID)

	runHistoryPersisted := true
	var runID int64
	row, appendErr := params.Store.AppendRunHistoryWithEvents(ctx, runHistory, eventRows, runEventStoreLimits)
	var eventsErr *persistence.RunEventsError
	switch {
	case errors.As(appendErr, &eventsErr):
		log.Warn("failed to persist run events",
			slog.String("dispatch_id", entry.DispatchID),
			slog.Int("events", len(eventRows)),
			slog.Any("error", eventsErr),
		)
		runID = row.ID
		if params.RunEventRetry != nil {
			params.RunEventRetry.Hold(HeldRunEvents{
				IssueID:     workerResult.IssueID,
				Identifier:  workerResult.Identifier,
				DispatchID:  entry.DispatchID,
				CompletedAt: runHistory.CompletedAt,
				Events:      eventRows,
			})
		}
	case appendErr != nil:
		runHistoryPersisted = false
		log.Error("failed to persist run history",
			slog.Any("error", appendErr),
		)
	default:
		runID = row.ID
	}
	state.RetainedEvents.release(entry.DispatchID)

	identity := sessionEvent{
		IssueID:    workerResult.IssueID,
		Identifier: cmp.Or(workerResult.Identifier, entry.Identifier),
		DisplayID:  entry.Issue.DisplayID,
		DispatchID: entry.DispatchID,
		SessionID:  cmp.Or(workerResult.SessionID, entry.SessionID),
		Attempt:    workerResult.Attempt,
		Agent:      workerResult.AgentAdapter,
	}

	if evidenceWorkObserved {
		if err := params.Store.ResetHandoffAbsenceSequence(ctx, workerResult.IssueID); err != nil {
			log.Error("failed to reset handoff absence sequence",
				slog.Any("error", err),
			)
		}
	}

	followedRun := parkedRun{
		RuleName:        entry.RuleName,
		StageLineage:    entry.StageLineage,
		AgentKind:       entry.AgentKind,
		ConfiguredModel: entry.ConfiguredModel,
		HandOver:        parkHandOverOf(runHistory),
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
				issueIdentityOf(entry.Issue),
				followedRun,
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
		CacheWriteTokens:   state.AgentTotals.CacheWriteTokens,
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
		clearUsageLimitHolds(ctx, log, params.Store, state, entry)

		for kind, commentIDs := range workerResult.HandedOffComments {
			recordHandedOffComments(ctx, state, params.Store, workerResult.IssueID, kind, commentIDs, log)
		}

		_, claimedAtExit := state.Claimed[workerResult.IssueID]
		terminalSuppressed := false
		hopMade := false

		// A successful handoff replaces the state the run read, and a
		// reaction waits hours or days, so it records the state the exit
		// wrote.
		reactionIdentity := issueIdentityOf(entry.Issue)

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
					IssueIdentity:  issueIdentityOf(entry.Issue),
					Run:            followedRun,
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
			resetStageHop(ctx, state, params.Store, workerResult.IssueID, stageResetTerminal, log)
			recordIssueOutcome(ctx, params.Store, terminalOutcome(workerResult.IssueID, observation, now), log)

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
					Error:               WorkerExitedErrorPrefix + evidenceErr.Error(),
					LastSSHHost:         workerResult.SSHHost,
					ContinuationContext: entry.ContinuationContext,
					ReactionKind:        entry.ReactionKind,
					Continuation:        entry.Continuation,
					AgentKind:           entry.AgentKind,
					ConfiguredModel:     entry.ConfiguredModel,
					RuleName:            entry.RuleName,
					StageLineage:        entry.StageLineage,
					RuleSettingsApplied: entry.RuleSettingsApplied,
					TemplateID:          entry.TemplateID,
					Logger:              log,

					IssueIdentity: issueIdentityOf(entry.Issue),
					Trigger:       triggerError,
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
						IssueID:             workerResult.IssueID,
						Identifier:          workerResult.Identifier,
						DisplayID:           entry.Issue.DisplayID,
						Attempt:             NextAttempt(entry.RetryAttempt),
						DelayMS:             continuationDelayMS,
						Error:               "",
						LastSSHHost:         workerResult.SSHHost,
						SessionID:           sessionID,
						AgentKind:           entry.AgentKind,
						ConfiguredModel:     entry.ConfiguredModel,
						RuleName:            entry.RuleName,
						StageLineage:        entry.StageLineage,
						RuleSettingsApplied: entry.RuleSettingsApplied,
						TemplateID:          entry.TemplateID,
						Logger:              log,

						IssueIdentity: issueIdentityOf(entry.Issue),
						Trigger:       triggerContinuation,
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
				var verifiedTerminalState string
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
						verifiedTerminalState = verifiedState
					}
				}

				var hop stageHopDecision
				if !verifiedTerminal {
					hop = advanceStage(ctx, state, entry, workerResult, params, noChangeDeclared, log)
					if hop.Result != "" {
						reportStageHop(ctx, state, params, runID, identity, hop, log)
					}
				}

				if verifiedTerminal {
					CancelRetry(state, workerResult.IssueID)
					delete(state.Claimed, workerResult.IssueID)
					resetStageHop(ctx, state, params.Store, workerResult.IssueID, stageResetTerminal, log)
					recordIssueOutcome(ctx, params.Store, terminalOutcome(workerResult.IssueID, verifiedTerminalState, now), log)
				} else if hop.made() {
					hopMade = true
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
							IssueID:             workerResult.IssueID,
							Identifier:          workerResult.Identifier,
							DisplayID:           entry.Issue.DisplayID,
							Attempt:             NextAttempt(entry.RetryAttempt),
							DelayMS:             continuationDelayMS,
							Error:               "",
							LastSSHHost:         workerResult.SSHHost,
							SessionID:           sessionID,
							AgentKind:           entry.AgentKind,
							ConfiguredModel:     entry.ConfiguredModel,
							RuleName:            entry.RuleName,
							StageLineage:        entry.StageLineage,
							RuleSettingsApplied: entry.RuleSettingsApplied,
							TemplateID:          entry.TemplateID,
							Logger:              log,

							IssueIdentity: issueIdentityOf(entry.Issue),
							Trigger:       triggerContinuation,
						}, params.OnRetryFire)
						metrics.IncRetries(triggerContinuation)
						retryScheduled = true
					}
				} else if incumbent := retrySlotIncumbent(state, workerResult.IssueID); incumbent != nil {
					// A successful handoff is not a stop signal: work queued
					// during the session is still valid, so the incumbent is
					// kept rather than cancelled.
					logRetrySlotDeferral(log, triggerContinuation, incumbent)
					recordIssueOutcome(ctx, params.Store, handoffOutcome(workerResult.IssueID, resolvedTarget, params.TerminalStates, now), log)
					metrics.IncHandoffTransitions(handoffSuccess)
					reactionIdentity.State = resolvedTarget
					log.Info("handoff transition succeeded, incumbent preserved",
						slog.String("handoff_state", params.HandoffState),
						slog.String("target_state", resolvedTarget),
						slog.Bool("no_change_declared", noChangeDeclared),
					)
					resetStageHop(ctx, state, params.Store, workerResult.IssueID, stageResetHandoff, log)
					retryDeferred = true
					claimRetainedForIncumbent = true
				} else {
					recordIssueOutcome(ctx, params.Store, handoffOutcome(workerResult.IssueID, resolvedTarget, params.TerminalStates, now), log)
					reactionIdentity.State = resolvedTarget
					log.Info("handoff transition succeeded, releasing claim",
						slog.String("handoff_state", params.HandoffState),
						slog.String("target_state", resolvedTarget),
						slog.Bool("no_change_declared", noChangeDeclared),
					)
					metrics.IncHandoffTransitions(handoffSuccess)
					CancelRetry(state, workerResult.IssueID)
					delete(state.Claimed, workerResult.IssueID)
					resetStageHop(ctx, state, params.Store, workerResult.IssueID, stageResetHandoff, log)
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
					IssueID:             workerResult.IssueID,
					Identifier:          workerResult.Identifier,
					DisplayID:           entry.Issue.DisplayID,
					Attempt:             NextAttempt(entry.RetryAttempt),
					DelayMS:             continuationDelayMS,
					Error:               "",
					LastSSHHost:         workerResult.SSHHost,
					SessionID:           sessionID,
					AgentKind:           entry.AgentKind,
					ConfiguredModel:     entry.ConfiguredModel,
					RuleName:            entry.RuleName,
					StageLineage:        entry.StageLineage,
					RuleSettingsApplied: entry.RuleSettingsApplied,
					TemplateID:          entry.TemplateID,
					Logger:              log,

					IssueIdentity: issueIdentityOf(entry.Issue),
					Trigger:       triggerContinuation,
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
			if !issueIsActive {
				resetStageHop(ctx, state, params.Store, workerResult.IssueID, stageResetNotActive, log)
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
		// A made hop leaves the issue active on its next stage, which owns any
		// reaction from here on, so the run that hopped seeds none.
		reactionEnqueueAllowed := claimedAtExit && (handoffPath || stillClaimed) && !terminalSuppressed && !hopMade
		// A fix session leaves the issue in the handoff state and releases
		// its claim, but its push gives the pull request a new head, so the
		// watches keyed on that head must start over.
		fixExit := claimedAtExit && dispatchPostureForReactionKind(entry.ReactionKind) == PostureFix && !blockedSoftStop && !terminalSuppressed
		headWatchesAllowed := reactionEnqueueAllowed || fixExit
		var seededKinds []string

		// Seed a pending CI check when CI provider and SCM adapter are both
		// configured and the workspace carries PR identity: the reaction
		// cannot resolve a head for a branch with no PR. Handoff paths stay
		// eligible even after the claim is released.
		if params.CIProvider != nil && params.SCMAdapter != nil && workerResult.WorkspacePath != "" {
			if headWatchesAllowed {
				scm := workspace.ReadSCMMetadata(workerResult.WorkspacePath, log)
				if scm.PRNumber > 0 && scm.Owner != "" && scm.Repo != "" && scm.Branch != "" {
					nowCI := time.Now().UTC()
					if params.NowFunc != nil {
						nowCI = params.NowFunc().UTC()
					}
					rkey := ReactionKey(workerResult.IssueID, ReactionKindCI)
					seededKinds = append(seededKinds, ReactionKindCI)
					replaced := state.PendingReactions[rkey]
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
						AgentKind:           entry.AgentKind,
						ConfiguredModel:     entry.ConfiguredModel,
						RuleName:            entry.RuleName,
						StageLineage:        entry.StageLineage,
						RuleSettingsApplied: entry.RuleSettingsApplied,
						TemplateID:          entry.TemplateID,

						IssueIdentity: reactionIdentity,
					}
					carryEscalation(state.PendingReactions[rkey], replaced)
				} else if scm.Branch != "" {
					log.Debug("ci watch not seeded: workspace metadata missing pull request identity",
						slog.Int("pr_number", scm.PRNumber),
						slog.String("owner", scm.Owner),
						slog.String("repo", scm.Repo),
					)
				}
			}
		}

		// Seed a pending review check when the SCM adapter is configured, the
		// review reaction is enabled (ReviewReactionConfigured), and the
		// workspace has PR metadata with repository identity. Without the
		// flag any other SCM-backed reaction would also seed a review entry,
		// whose zero-valued config escalates the first actionable comment.
		if params.SCMAdapter != nil && params.ReviewReactionConfigured && workerResult.WorkspacePath != "" {
			if headWatchesAllowed {
				scm := workspace.ReadSCMMetadata(workerResult.WorkspacePath, log)
				if scm.PRNumber > 0 && scm.Branch != "" && scm.Owner != "" && scm.Repo != "" {
					nowReview := time.Now().UTC()
					if params.NowFunc != nil {
						nowReview = params.NowFunc().UTC()
					}
					rkey := ReactionKey(workerResult.IssueID, ReactionKindReview)
					// Skip if already present to preserve in-progress debounce.
					if _, exists := state.PendingReactions[rkey]; !exists {
						seededKinds = append(seededKinds, ReactionKindReview)
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
							AgentKind:           entry.AgentKind,
							ConfiguredModel:     entry.ConfiguredModel,
							RuleName:            entry.RuleName,
							StageLineage:        entry.StageLineage,
							RuleSettingsApplied: entry.RuleSettingsApplied,
							TemplateID:          entry.TemplateID,

							IssueIdentity: reactionIdentity,
						}
					}
				}
			}
		}

		// Seed a pending bot-review check when the SCM adapter is configured,
		// bot-review is enabled, and the workspace has PR metadata.
		// Independent of the review-kind enqueue: both fire from the same exit.
		if params.SCMAdapter != nil && params.BotReviewReactionConfigured && workerResult.WorkspacePath != "" {
			if headWatchesAllowed {
				scm := workspace.ReadSCMMetadata(workerResult.WorkspacePath, log)
				if scm.PRNumber > 0 && scm.Branch != "" && scm.Owner != "" && scm.Repo != "" {
					nowBotReview := time.Now().UTC()
					if params.NowFunc != nil {
						nowBotReview = params.NowFunc().UTC()
					}
					rkey := ReactionKey(workerResult.IssueID, ReactionKindBotReview)
					if _, exists := state.PendingReactions[rkey]; !exists {
						seededKinds = append(seededKinds, ReactionKindBotReview)
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
							AgentKind:           entry.AgentKind,
							ConfiguredModel:     entry.ConfiguredModel,
							RuleName:            entry.RuleName,
							StageLineage:        entry.StageLineage,
							RuleSettingsApplied: entry.RuleSettingsApplied,
							TemplateID:          entry.TemplateID,

							IssueIdentity: reactionIdentity,
						}
					}
				}
			}
		}

		// Seed a pending auto-merge entry when the SCM adapter is configured,
		// auto-merge is enabled, and the workspace has PR metadata.
		if params.SCMAdapter != nil && params.AutoMergeReactionConfigured && workerResult.WorkspacePath != "" {
			if headWatchesAllowed {
				scm := workspace.ReadSCMMetadata(workerResult.WorkspacePath, log)
				if scm.PRNumber > 0 && scm.Branch != "" && scm.Owner != "" && scm.Repo != "" {
					nowMerge := time.Now().UTC()
					if params.NowFunc != nil {
						nowMerge = params.NowFunc().UTC()
					}
					rkey := ReactionKey(workerResult.IssueID, ReactionKindAutoMerge)
					if _, exists := state.PendingReactions[rkey]; !exists {
						seededKinds = append(seededKinds, ReactionKindAutoMerge)
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
							AgentKind:           entry.AgentKind,
							ConfiguredModel:     entry.ConfiguredModel,
							RuleName:            entry.RuleName,
							StageLineage:        entry.StageLineage,
							RuleSettingsApplied: entry.RuleSettingsApplied,
							TemplateID:          entry.TemplateID,

							IssueIdentity: reactionIdentity,
						}
					}
				}
			}
		}

		// Seed a pending merge-conflict entry when the SCM adapter is
		// configured, merge-conflict is enabled, and the workspace has PR
		// metadata.
		if params.SCMAdapter != nil && params.MergeConflictReactionConfigured && workerResult.WorkspacePath != "" {
			if headWatchesAllowed {
				scm := workspace.ReadSCMMetadata(workerResult.WorkspacePath, log)
				if scm.PRNumber > 0 && scm.Branch != "" && scm.Owner != "" && scm.Repo != "" {
					nowMergeConflict := time.Now().UTC()
					if params.NowFunc != nil {
						nowMergeConflict = params.NowFunc().UTC()
					}
					rkey := ReactionKey(workerResult.IssueID, ReactionKindMergeConflict)
					if _, exists := state.PendingReactions[rkey]; !exists {
						seededKinds = append(seededKinds, ReactionKindMergeConflict)
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
							AgentKind:           entry.AgentKind,
							ConfiguredModel:     entry.ConfiguredModel,
							RuleName:            entry.RuleName,
							StageLineage:        entry.StageLineage,
							RuleSettingsApplied: entry.RuleSettingsApplied,
							TemplateID:          entry.TemplateID,

							IssueIdentity: reactionIdentity,
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
						seededKinds = append(seededKinds, ReactionKindLabelReview)
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
							AgentKind:           entry.AgentKind,
							ConfiguredModel:     entry.ConfiguredModel,
							RuleName:            entry.RuleName,
							StageLineage:        entry.StageLineage,
							RuleSettingsApplied: entry.RuleSettingsApplied,
							TemplateID:          entry.TemplateID,

							IssueIdentity: reactionIdentity,
						}
					}
				}
			}
		}

		// Seed a pending label-fix entry when the SCM adapter is configured,
		// label-fix is enabled, and the workspace has PR metadata with a head
		// branch (the fix session checks it out). A fix session's own exit
		// never seeds it; repeatability comes from the reconcile re-enqueue
		// on dispatch.
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
						seededKinds = append(seededKinds, ReactionKindLabelFix)
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
							AgentKind:           entry.AgentKind,
							ConfiguredModel:     entry.ConfiguredModel,
							RuleName:            entry.RuleName,
							StageLineage:        entry.StageLineage,
							RuleSettingsApplied: entry.RuleSettingsApplied,
							TemplateID:          entry.TemplateID,

							IssueIdentity: reactionIdentity,
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
						seededKinds = append(seededKinds, ReactionKindMergeCompletion)
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
							AgentKind:           entry.AgentKind,
							ConfiguredModel:     entry.ConfiguredModel,
							RuleName:            entry.RuleName,
							StageLineage:        entry.StageLineage,
							RuleSettingsApplied: entry.RuleSettingsApplied,
							TemplateID:          entry.TemplateID,

							IssueIdentity: reactionIdentity,
						}
					}
				}
			}
		}

		recordReactionActivity(ctx, params.Store, workerResult.IssueID, entry.StageLineage,
			persistence.ReactionActivityWatch, seededKinds, now, log)

	case WorkerExitCancelled:
		// Release the claim only if reconciliation stall detection has not
		// pre-scheduled a retry, which needs the claim to prevent duplicate
		// dispatch.
		if _, hasRetry := state.RetryAttempts[workerResult.IssueID]; !hasRetry {
			delete(state.Claimed, workerResult.IssueID)
		}

	default: // WorkerExitError and any unknown kind
		classification := classifyWorkerError(workerResult.Error)
		var usageHold UsageLimitHold
		if usageWaits {
			key := UsageLimitHoldKey{AgentKind: entry.AgentKind, ModelOnly: usageLimit.ModelOnly}
			if key.ModelOnly {
				key.Model = entry.ConfiguredModel
			}
			usageHold = holdUsageLimit(ctx, log, params.Store, state, key, usageReset, now)
		}
		if classification.Retryable {
			incumbent := retrySlotIncumbent(state, workerResult.IssueID)
			switch {
			case usageWaits && params.ReleaseHeld:
				log.Warn("worker run refused by usage limit, releasing claim",
					slog.Time("resets_at", usageHold.ResetsAt),
				)
				delete(state.Claimed, workerResult.IssueID)

			case incumbent != nil:
				logRetrySlotDeferral(log, "error-backoff", incumbent)
				nextAttempt = incumbent.Attempt
				retryDeferred = true

			default:
				nextAttempt = NextAttempt(entry.RetryAttempt)
				delayMS := computeBackoffDelay(nextAttempt, params.MaxRetryBackoffMS)

				var errMsg string
				if workerResult.Error != nil {
					errMsg = redact.Mask(WorkerExitedErrorPrefix + workerResult.Error.Error())
				}

				retry := ScheduleRetryParams{
					IssueID:             workerResult.IssueID,
					Identifier:          workerResult.Identifier,
					DisplayID:           entry.Issue.DisplayID,
					Attempt:             nextAttempt,
					DelayMS:             delayMS,
					Error:               errMsg,
					LastSSHHost:         workerResult.SSHHost,
					ContinuationContext: entry.ContinuationContext,
					ReactionKind:        entry.ReactionKind,
					Continuation:        entry.Continuation,
					AgentKind:           entry.AgentKind,
					ConfiguredModel:     entry.ConfiguredModel,
					RuleName:            entry.RuleName,
					StageLineage:        entry.StageLineage,
					RuleSettingsApplied: entry.RuleSettingsApplied,
					TemplateID:          entry.TemplateID,
					Logger:              log,

					IssueIdentity:      issueIdentityOf(entry.Issue),
					Trigger:            triggerError,
					UsageLimitNotified: usageRefused,
				}
				if usageWaits {
					retry.DelayMS = 0
					retry.DueAtMS = usageLimitRetryDue(usageHold, now, delayMS).UnixMilli()
					retry.Trigger = triggerUsageLimit
					log.Warn("worker run refused by usage limit, retrying at reset",
						slog.Time("resets_at", usageHold.ResetsAt),
						slog.Int("next_attempt", nextAttempt),
						slog.Bool("model_only", usageLimit.ModelOnly),
					)
				} else {
					log.Warn("worker run failed, scheduling retry",
						slog.Any("error", workerResult.Error),
						slog.Int("next_attempt", nextAttempt),
						slog.Int64("delay_ms", delayMS),
					)
				}

				ScheduleRetry(state, retry, params.OnRetryFire)
				metrics.IncRetries(retry.Trigger)
				retryScheduled = true
			}
		} else {
			log.Error("worker run failed, non-retryable, releasing claim",
				slog.Any("error", workerResult.Error),
			)
			delete(state.Claimed, workerResult.IssueID)
		}
	}

	if retryDeferred {
		carryRefusalEpisode(ctx, log, params.Store, state, workerResult.IssueID, usageRefused)
	}

	if retryScheduled {
		if retryEntry, ok := state.RetryAttempts[workerResult.IssueID]; ok {
			if !isKnownReactionKind(retryEntry.ReactionKind) {
				pEntry := retryEntryRow(retryEntry)
				if err := params.Store.SaveRetryEntry(ctx, pEntry); err != nil {
					log.Error("failed to persist retry entry",
						slog.Any("error", err),
					)
				}
			}
		}
	}

	deleteLivePass(ctx, params.Store, workerResult.IssueID, persistence.LivePassRunning, log)

	runDuration := max(now.Sub(entry.StartedAt), 0)

	// A deferral leaves work queued exactly as a scheduled retry does, so the
	// comments report re-queuing for either outcome.
	retryPending := retryScheduled || retryDeferred

	// The facts are captured synchronously at exit time; the delivery is
	// detached so the event loop never blocks.
	var eventType domain.EventType
	var facts domain.MessageFacts
	var severity, agentText, lifecycle string
	switch {
	case workerResult.ExitKind == WorkerExitCancelled:
		return outcome
	case usageRefused && entry.UsageLimitNotified:
		return outcome
	case workerResult.ExitKind == WorkerExitNormal && !evidenceWithheld:
		lifecycle = "completion"
		if workerResult.SoftStop {
			eventType = domain.EventSessionStopped
			facts = domain.SessionStoppedFacts{
				Elapsed: runDuration,
				Turns:   workerResult.TurnsCompleted,
				Reason:  workerResult.SoftStopReason,
			}
			agentText = publicStatement(workerResult.SoftStopStatement)
			if workerResult.SoftStopReason == string(workspace.StatusNoChangeNeeded) {
				severity = "info"
			}
		} else {
			eventType = domain.EventSessionCompleted
			facts = domain.SessionCompletedFacts{
				Elapsed:        runDuration,
				Turns:          workerResult.TurnsCompleted,
				RetryScheduled: retryPending,
			}
		}
	default:
		lifecycle = "failure"
		eventType = domain.EventSessionFailed
		facts = domain.SessionFailedFacts{
			Elapsed:        runDuration,
			RetryScheduled: retryPending,
			NextAttempt:    nextAttempt,
		}
	}

	exitEvent := identity
	exitEvent.AgentText = agentText
	delivery := params.Router.Route(exitEvent.notification(eventType, severity, facts))

	deliverDetached(ctx, &state.TrackerOpsWg, delivery, log, func(received bool, err error) {
		if !received {
			return
		}
		if err != nil {
			log.Warn("tracker comment failed",
				slog.String("lifecycle", lifecycle),
				slog.Any("error", err),
			)
			metrics.IncTrackerComments(lifecycle, "error")
			return
		}
		log.Info("tracker comment posted",
			slog.String("lifecycle", lifecycle),
		)
		metrics.IncTrackerComments(lifecycle, "success")
	})
	return outcome
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

// runModelUsage maps per-model usage to stored parts, dropping models zero
// in every token class; nil when none remain.
func runModelUsage(totals map[string]domain.TokenUsage) map[string]persistence.ModelTokens {
	var parts map[string]persistence.ModelTokens
	for model, tokens := range totals {
		if !hasTokenClasses(tokens) {
			continue
		}
		if parts == nil {
			parts = make(map[string]persistence.ModelTokens, len(totals))
		}
		parts[model] = persistence.ModelTokens{
			InputTokens:      tokens.InputTokens,
			OutputTokens:     tokens.OutputTokens,
			CacheReadTokens:  tokens.CacheReadTokens,
			CacheWriteTokens: tokens.CacheWriteTokens,
		}
	}
	return parts
}

// runReportedAmounts maps reported amounts to stored rows; nil when none.
func runReportedAmounts(amounts []domain.ReportedAmount) []persistence.ReportedAmount {
	if len(amounts) == 0 {
		return nil
	}
	rows := make([]persistence.ReportedAmount, len(amounts))
	for i, amount := range amounts {
		rows[i] = persistence.ReportedAmount{
			Unit:       amount.Unit,
			Model:      amount.Model,
			Amount:     amount.Amount,
			LowerBound: amount.LowerBound,
		}
	}
	return rows
}

// recordRunCost prices a run under rates and stores the result on its row.
// The row's token columns and ModelUsage must be final, because the price is
// fixed when the row is written.
func recordRunCost(row *persistence.RunHistory, rates *pricing.Rates) {
	var parts map[string]domain.TokenUsage
	if len(row.ModelUsage) > 0 {
		parts = make(map[string]domain.TokenUsage, len(row.ModelUsage))
		for model, tokens := range row.ModelUsage {
			parts[model] = domain.TokenUsage{
				InputTokens:      tokens.InputTokens,
				OutputTokens:     tokens.OutputTokens,
				CacheReadTokens:  tokens.CacheReadTokens,
				CacheWriteTokens: tokens.CacheWriteTokens,
			}
		}
	}
	reported := make([]domain.ReportedAmount, len(row.ReportedAmounts))
	for i, amount := range row.ReportedAmounts {
		reported[i] = domain.ReportedAmount{
			Unit:       amount.Unit,
			Model:      amount.Model,
			Amount:     amount.Amount,
			LowerBound: amount.LowerBound,
		}
	}
	price := pricing.PriceRun(pricing.Spend{
		Kind:     row.AgentAdapter,
		Measured: row.TokensMeasured,
		Totals: domain.TokenUsage{
			InputTokens:      row.InputTokens,
			OutputTokens:     row.OutputTokens,
			TotalTokens:      row.TotalTokens,
			CacheReadTokens:  row.CacheReadTokens,
			CacheWriteTokens: row.CacheWriteTokens,
		},
		Parts:            parts,
		UnaccountedTurns: row.UnaccountedTurns,
		Reported:         reported,
	}, rates)

	row.EstimatedCostUSD = price.CostUSD
	row.CostStatus = string(price.Status)
	if len(price.Parts) == 0 {
		return
	}
	row.ModelCosts = make(map[string]persistence.ModelCost, len(price.Parts))
	for model, part := range price.Parts {
		row.ModelCosts[model] = persistence.ModelCost{
			EstimatedCostUSD:  part.CostUSD,
			CostStatus:        string(part.Status),
			PriceSource:       string(part.Source),
			PriceTableVersion: part.TableVersion,
			UnratedClasses:    part.UnratedNames(),
		}
	}
}

func errorStringPtr(err error) *string {
	if err == nil {
		return nil
	}
	return new(redact.Mask(err.Error()))
}

// maskedReviewMetadata returns a copy of m with every string field
// masked, so a registered value a verification command or the agent's
// own review text printed cannot reach the persisted run history.
func maskedReviewMetadata(m domain.ReviewMetadata) domain.ReviewMetadata {
	m.FinalVerdict = redact.Mask(m.FinalVerdict)

	iterations := make([]domain.ReviewIterationRecord, len(m.Iterations))
	for i, iteration := range m.Iterations {
		iteration.Verdict = redact.Mask(iteration.Verdict)
		iteration.VerdictRaw = redact.Mask(iteration.VerdictRaw)
		iteration.VerdictParseError = redact.Mask(iteration.VerdictParseError)

		results := make([]domain.VerificationResult, len(iteration.VerificationResults))
		for j, result := range iteration.VerificationResults {
			results[j] = domain.VerificationResult{
				Command:        redact.Mask(result.Command),
				ExitCode:       result.ExitCode,
				Stdout:         redact.Mask(result.Stdout),
				Stderr:         redact.Mask(result.Stderr),
				DurationMS:     result.DurationMS,
				TimedOut:       result.TimedOut,
				ExecutionError: redact.Mask(result.ExecutionError),
			}
		}
		iteration.VerificationResults = results
		iterations[i] = iteration
	}
	m.Iterations = iterations

	return m
}

func stringPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// declaredOutcome returns the recognized status token that ended the run and
// its public statement, or two empty strings when the run ended on none.
func declaredOutcome(result WorkerResult) (token, statement string) {
	token = declaredToken(result.SoftStop, result.SoftStopReason)
	if token == "" {
		return "", ""
	}
	return token, publicStatement(result.SoftStopStatement)
}

// declaredToken returns the soft-stop reason when the agent ended the run with
// a recognized status signal, and "" otherwise.
func declaredToken(softStop bool, reason string) string {
	if !softStop || !workspace.StatusSignal(reason).IsRecognized() {
		return ""
	}
	return reason
}

// maxPublicStatementRunes bounds the agent text a destination receives.
const maxPublicStatementRunes = 1024

// publicStatement returns the agent's stop statement in the form every
// destination may show, or "" when nothing is left to show.
//
// Control characters are removed and invalid UTF-8 replaced before
// masking, so removing one cannot assemble a registered value after the
// mask ran. A statement cut at the read bound is masked as truncated text,
// which also removes a trailing fragment of a registered value, and ends in
// an ellipsis.
func publicStatement(statement workspace.StatusStatement) string {
	text := statement.Text
	if statement.Truncated {
		text = dropIncompleteTrailingRune(text)
	}
	mask := redact.Mask
	if statement.Truncated {
		mask = redact.MaskTruncated
	}
	// A registered value may itself hold a byte the cleanup rewrites, so
	// it is masked as written before the cleanup and once more after it.
	// Both passes look for a fragment the cut left, since the first one
	// can mask a shorter value inside that fragment.
	text = mask(text)
	text = strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(text)
	text = removeControlBytes(text)
	text = strings.ToValidUTF8(text, "\uFFFD")
	text = mask(text)
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if statement.Truncated {
		text += "\u2026"
	}
	return redact.Truncate(text, maxPublicStatementRunes)
}

// dropIncompleteTrailingRune removes a final multi-byte sequence that the
// cut left without its continuation bytes.
func dropIncompleteTrailingRune(text string) string {
	for i := len(text) - 1; i >= 0 && i >= len(text)-utf8.UTFMax; i-- {
		if !utf8.RuneStart(text[i]) {
			continue
		}
		if utf8.FullRuneInString(text[i:]) {
			return text
		}
		return text[:i]
	}
	return text
}

// removeControlBytes deletes every C0 control character except tab and
// line feed, and DEL. Each is a single ASCII byte, so no multi-byte
// sequence is affected.
func removeControlBytes(text string) string {
	var kept strings.Builder
	kept.Grow(len(text))
	for i := 0; i < len(text); i++ {
		b := text[i]
		if (b < 0x20 && b != '\t' && b != '\n') || b == 0x7f {
			continue
		}
		kept.WriteByte(b)
	}
	return kept.String()
}
