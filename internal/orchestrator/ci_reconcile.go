package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/scm/scmcore"
)

// ciPendingBackoffBaseDefault is the fallback base interval for CI-pending
// exponential backoff when the configured poll interval is zero or negative.
const ciPendingBackoffBaseDefault = 10 * time.Second

// ciPendingBackoffCap is the maximum interval between CI status checks.
const ciPendingBackoffCap = 5 * time.Minute

// computeCIPendingDelay returns the backoff delay for a CI pending re-check
// at the given attempt count. Attempt 0 returns zero (immediate). Each
// subsequent attempt returns base * 2^attempts, capped at ciPendingBackoffCap.
// If base is zero or negative, [ciPendingBackoffBaseDefault] is used.
func computeCIPendingDelay(base time.Duration, attempts int) time.Duration {
	if base <= 0 {
		base = ciPendingBackoffBaseDefault
	}
	if attempts <= 0 {
		return 0
	}
	shift := uint(attempts)
	if shift > 30 {
		return ciPendingBackoffCap
	}
	delay := base * (1 << shift)
	if delay > ciPendingBackoffCap || delay < 0 {
		return ciPendingBackoffCap
	}
	return delay
}

// reconcileCIStatus resolves the pull request's current head for each
// CI-kind entry in state.PendingReactions and polls CI status for that
// head. Called from ReconcileRunningIssues after reconcileTrackerState.
// Skipped entirely when params.CIProvider or params.SCMAdapter is nil.
//
// Entries that are not yet due (PendingRetryAt in the future) are
// re-enqueued without making an API call, applying exponential backoff.
// An entry's age is measured from the last head this process recorded,
// falling back to the entry's creation time before any head is recorded;
// past params.CIWatchWindow the entry and its attempt counter are dropped
// and a warning is logged, unless an escalation stands, which the window
// never ages out. A zero or negative CIWatchWindow disables the bound.
func reconcileCIStatus(state *State, params ReconcileParams, log *slog.Logger, ctx context.Context, metrics domain.Metrics) {
	if params.CIProvider == nil || params.SCMAdapter == nil {
		return
	}

	now := time.Now().UTC()
	if params.NowFunc != nil {
		now = params.NowFunc().UTC()
	}

	base := time.Duration(state.PollIntervalMS) * time.Millisecond

	for key, pending := range state.PendingReactions {
		if pending.Kind != ReactionKindCI {
			continue
		}
		delete(state.PendingReactions, key)

		ciData, ok := pending.KindData.(*CIReactionData)
		if !ok {
			cancelReactionTriage(pending)
			log.ErrorContext(ctx, "unexpected KindData type for CI reaction",
				slog.String("issue_id", pending.IssueID),
				slog.String("type", fmt.Sprintf("%T", pending.KindData)),
			)
			continue
		}

		entryLog := logging.WithIssue(log, pending.IssueID, pending.Identifier)
		rkey := ReactionKey(pending.IssueID, ReactionKindCI)

		ageBasis := pending.CreatedAt
		if !pending.HeadRecordedAt.IsZero() {
			ageBasis = pending.HeadRecordedAt
		}
		if params.CIWatchWindow > 0 && !pending.escalationStands() && now.Sub(ageBasis) > params.CIWatchWindow {
			cancelReactionTriage(pending)
			delete(state.ReactionAttempts, rkey)
			entryLog.Warn("ci watch window elapsed, dropping",
				slog.Int64("window_ms", int64(params.CIWatchWindow/time.Millisecond)),
				slog.Int64("age_ms", int64(now.Sub(ageBasis)/time.Millisecond)),
			)
			continue
		}

		if reactionWaits(pending, now) {
			state.PendingReactions[key] = pending
			continue
		}
		retryEscalationSave(ctx, state, params.Store, pending, entryLog)

		// A run in flight makes every provider call this pass would
		// make redundant, so the entry waits one tick instead. The
		// backoff counter is untouched: waiting is not a fetch error.
		if pending.Triage != nil && !triageRunFinished(pending.Triage) {
			pending.PendingRetryAt = now
			requeueReaction(state, key, pending, waitTriage)
			continue
		}

		status, mergeErr := params.SCMAdapter.GetMergeability(ctx, ciData.PRNumber, ciData.Owner, ciData.Repo)
		if mergeErr != nil {
			var scmErr *domain.SCMError
			if errors.As(mergeErr, &scmErr) && scmErr.Kind == domain.ErrSCMNotFound {
				cancelReactionTriage(pending)
				endEscalation(ctx, params.Store, pending, entryLog)
				delete(state.ReactionAttempts, rkey)
				entryLog.Warn("ci watch pull request not found, dropping",
					slog.Int("pr_number", ciData.PRNumber),
				)
				continue
			}
			pending.PendingAttempts++
			delay := computeCIPendingDelay(base, pending.PendingAttempts)
			pending.PendingRetryAt = now.Add(delay)
			entryLog.Warn("ci mergeability fetch failed, retrying with backoff",
				slog.Any("error", mergeErr),
				slog.Int("pending_attempts", pending.PendingAttempts),
				slog.Int64("retry_after_ms", int64(delay/time.Millisecond)),
			)
			requeueReaction(state, key, pending, waitBackoff)
			continue
		}
		notePullRequestURL(pending, status)

		// The merged check runs before the closed check so a provider
		// whose closed state subsumes merging still ends the watch
		// through the merged branch and logs the merge rather than a
		// close.
		if status.Merged {
			cancelReactionTriage(pending)
			endEscalation(ctx, params.Store, pending, entryLog)
			delete(state.ReactionAttempts, rkey)
			entryLog.Info("ci watch ended: pull request merged",
				slog.String("head_sha", status.HeadSHA),
			)
			continue
		}
		if status.Closed {
			cancelReactionTriage(pending)
			endEscalation(ctx, params.Store, pending, entryLog)
			delete(state.ReactionAttempts, rkey)
			entryLog.Info("ci watch ended: pull request closed without merging",
				slog.Int("pr_number", ciData.PRNumber),
			)
			continue
		}

		if status.HeadSHA == "" {
			pending.PendingAttempts++
			delay := computeCIPendingDelay(base, pending.PendingAttempts)
			pending.PendingRetryAt = now.Add(delay)
			entryLog.Debug("ci deferred: empty head sha",
				slog.Int("pr_number", ciData.PRNumber),
			)
			requeueReaction(state, key, pending, waitBackoff)
			continue
		}

		// The epoch record is read before it is written, because this
		// pass needs the previous head to detect a change; an
		// upsert-then-read ordering would always read back the value
		// just written and no head change would ever be detected. The
		// durable observation helper is not used here: its timestamp
		// survives a restart, and the attribution boundary this pass
		// maintains (PendingReaction.HeadRecordedAt) deliberately must
		// not. The exception is a restored standing escalation, whose
		// escalation time bounds the query because no run of the previous
		// process can precede it.
		storedHead, dispatched, fpErr := params.Store.GetReactionFingerprint(ctx, pending.IssueID, ReactionKindCI)
		if fpErr != nil {
			entryLog.Warn("failed to get ci reaction fingerprint, proceeding without epoch detection",
				slog.Any("error", fpErr),
			)
			// A read failure suppresses epoch detection for this pass
			// rather than fabricate one: treating it as a head change
			// would reset the retry budget on a database error, which
			// is the unsafe direction.
			storedHead = status.HeadSHA
			dispatched = false
		}

		if storedHead != status.HeadSHA {
			// The durable head advances before the runtime boundary does.
			// The transition restarts the watch clock and re-arms the
			// once-per-epoch escalation, so applying it against a record
			// that did not advance would repeat both on every later pass:
			// the age basis would never grow old enough to elapse, and the
			// escalation would fire once per pass instead of once per
			// epoch. Deferring costs one poll and keeps the entry bounded.
			if upsertErr := params.Store.UpsertReactionFingerprint(ctx, pending.IssueID, ReactionKindCI, status.HeadSHA); upsertErr != nil {
				pending.PendingAttempts++
				delay := computeCIPendingDelay(base, pending.PendingAttempts)
				pending.PendingRetryAt = now.Add(delay)
				entryLog.Warn("failed to upsert reaction fingerprint, deferring the epoch transition",
					slog.Any("error", upsertErr),
					slog.Int("pending_attempts", pending.PendingAttempts),
					slog.Int64("retry_after_ms", int64(delay/time.Millisecond)),
				)
				requeueReaction(state, key, pending, waitBackoff)
				continue
			}
			// The epoch transition already clears dispatched through the
			// fingerprint upsert; the status read below proceeds
			// unconditionally on this branch, so the local dispatched
			// value needs no update here.
			applyCIEpochTransition(state, params, pending, rkey, storedHead, status.HeadSHA, now, ctx, entryLog)
		} else if dispatched && !pending.escalationStands() {
			// A standing escalation reads the status of a dispatched head
			// too: a triage escalation marks its head dispatched, and a
			// passing re-run must still end it.
			pending.PendingAttempts++
			delay := computeCIPendingDelay(base, pending.PendingAttempts)
			pending.PendingRetryAt = now.Add(delay)
			entryLog.Debug("CI reaction already dispatched for this head, skipping",
				slog.String("head_sha", status.HeadSHA),
			)
			requeueReaction(state, key, pending, waitPollInterval)
			continue
		}

		result, err := params.CIProvider.FetchCIStatus(ctx, status.HeadSHA)
		if err != nil {
			pending.PendingAttempts++
			delay := computeCIPendingDelay(base, pending.PendingAttempts)
			pending.PendingRetryAt = now.Add(delay)
			entryLog.Warn("ci status fetch failed, retrying with backoff",
				slog.String("head_sha", status.HeadSHA),
				slog.Any("error", err),
				slog.Int("pending_attempts", pending.PendingAttempts),
				slog.Int64("retry_after_ms", int64(delay/time.Millisecond)),
			)
			metrics.IncCIStatusChecks("error")
			requeueReaction(state, key, pending, waitBackoff)
			continue
		}

		metrics.IncCIStatusChecks(string(result.Status))

		switch result.Status {
		case domain.CIStatusPassing, domain.CIStatusPending, domain.CIStatusFailing:
			pending.LastCheck = ciReactionCheck(result, status.HeadSHA, now)
		}

		switch result.Status {
		case domain.CIStatusPassing:
			// The episode closes while the entry keeps watching, so the
			// head an in-flight or finished run answered for no longer
			// describes anything. A retained verdict would otherwise be
			// replayed against the next episode that recomputes the same
			// head.
			cancelReactionTriage(pending)
			if pending.EscalationTrigger == EscalationTriggerTriage {
				clearCIDispatchedMark(ctx, params.Store, pending.IssueID, status.HeadSHA, entryLog)
			}
			endEscalation(ctx, params.Store, pending, entryLog)
			delete(state.ReactionAttempts, rkey)
			pending.PendingAttempts++
			delay := computeCIPendingDelay(base, pending.PendingAttempts)
			pending.PendingRetryAt = now.Add(delay)
			entryLog.Info("CI passing on current head, continuing to watch",
				slog.String("head_sha", status.HeadSHA),
			)
			requeueReaction(state, key, pending, waitPollInterval)

		case domain.CIStatusPending:
			pending.PendingAttempts++
			delay := computeCIPendingDelay(base, pending.PendingAttempts)
			pending.PendingRetryAt = now.Add(delay)
			requeueReaction(state, key, pending, waitPollInterval)
			entryLog.Debug("CI pending, will re-check after backoff",
				slog.String("head_sha", status.HeadSHA),
				slog.Int("pending_attempts", pending.PendingAttempts),
				slog.Int64("retry_after_ms", int64(delay/time.Millisecond)),
			)

		case domain.CIStatusFailing:
			endLiftedBudgetEscalation(ctx, params.Store, pending, state.ReactionAttempts[rkey] >= params.CIFeedback.MaxRetries, entryLog)
			if pending.escalationStands() {
				pending.PendingAttempts++
				delay := computeCIPendingDelay(base, pending.PendingAttempts)
				pending.PendingRetryAt = now.Add(delay)
				requeueReaction(state, key, pending, waitPollInterval)
				continue
			}

			if incumbent := retrySlotIncumbent(state, pending.IssueID); incumbent != nil {
				logRetrySlotDeferral(entryLog, ReactionKindCI, incumbent)
				pending.PendingAttempts++
				delay := computeCIPendingDelay(base, pending.PendingAttempts)
				pending.PendingRetryAt = now.Add(delay)
				requeueReaction(state, key, pending, waitRetrySlot)
				continue
			}

			// The gate sits above handleCIFailure because that function
			// appends a run history row as its first act, which the
			// resuming pass would append a second time.
			triageVerdict := reactionTriageGate(state, params, pending, params.CITriage, ReactionTriageRequest{
				Kind:          ReactionKindCI,
				WorkspaceRoot: params.WorkspaceRoot,
				IssueID:       pending.IssueID,
				Identifier:    pending.Identifier,
				DisplayID:     pending.DisplayID,
				Attempt:       pending.Attempt,
				SSHHost:       pending.LastSSHHost,
				Fingerprint:   status.HeadSHA,
				AttemptsUsed:  state.ReactionAttempts[rkey],
				MaxAttempts:   params.CIFeedback.MaxRetries,
				Subject:       result.ToTemplateMap(),
			}, entryLog, ctx)

			switch triageVerdict {
			case triageWait:
				pending.PendingRetryAt = now.Add(computeCIPendingDelay(base, pending.PendingAttempts))
				requeueReaction(state, key, pending, waitTriage)

			case triageHandled:
				// Re-enqueued exactly as the already-dispatched branch
				// upstream does, so a handled head and a dispatched one are
				// indistinguishable from the next pass onward.
				pending.PendingAttempts++
				pending.PendingRetryAt = now.Add(computeCIPendingDelay(base, pending.PendingAttempts))
				requeueReaction(state, key, pending, waitPollInterval)

			case triageEscalate:
				escalateCIFailure(state, params, pending, result, status.HeadSHA, state.ReactionAttempts[rkey],
					EscalationTriggerTriage, now, base, entryLog, ctx, metrics)

			default:
				handleCIFailure(state, params, pending, result, status.HeadSHA, now, base, entryLog, ctx, metrics)
			}

		default:
			entryLog.Warn("CI status provider returned unrecognized status, re-enqueueing",
				slog.String("status", string(result.Status)),
				slog.String("head_sha", status.HeadSHA),
			)
			metrics.IncCIStatusChecks("error")
			pending.PendingAttempts++
			pending.PendingRetryAt = now.Add(computeCIPendingDelay(base, pending.PendingAttempts))
			requeueReaction(state, key, pending, waitBackoff)
		}
	}
}

// ciReactionCheck records what a CI status read observed on head. It copies
// check names and outcome counts only, never a log excerpt or a details link.
func ciReactionCheck(result domain.CIResult, head string, observedAt time.Time) *ReactionCheck {
	failingChecks := []string{}
	for _, run := range result.CheckRuns {
		if scmcore.IsFailingConclusion(run.Conclusion) {
			failingChecks = append(failingChecks, run.Name)
		}
	}
	tally := scmcore.TallyCheckRuns(result.CheckRuns)
	return &ReactionCheck{
		ObservedAt: observedAt,
		HeadSHA:    head,
		CI: &CICheck{
			Status:        string(result.Status),
			FailingChecks: failingChecks,
			Failing:       tally.Failing,
			Pending:       tally.Pending,
			Passing:       tally.Passing,
		},
	}
}

// applyCIEpochTransition applies the runtime consequences of a detected
// pull request head change: it resets the backoff counter and the last CI
// result and ends the standing escalation unconditionally, resets the CI retry budget
// only when [classifyHeadChange] positively establishes that the change
// is not the orchestrator's own work, and records the new head as the
// boundary for the next attribution query.
//
// applyCIEpochTransition never dispatches a continuation and never
// writes to the tracker; a continuation follows only if the caller's
// subsequent status read then finds a failing verdict.
func applyCIEpochTransition(
	state *State,
	params ReconcileParams,
	pending *PendingReaction,
	rkey string,
	previousHead, newHead string,
	now time.Time,
	ctx context.Context,
	log *slog.Logger,
) {
	attribution := classifyHeadChange(state, params, pending, now, ctx)

	// Unconditional: a changed head voids every prior conclusion.
	pending.PendingAttempts = 0
	pending.attemptsBeforeRequest = 0
	endEscalation(ctx, params.Store, pending, log)
	pending.LastCheck = nil

	// Conditional: a needless reset costs an unbounded loop.
	resetAttempts := attribution == headChangeNotOurs
	if resetAttempts {
		delete(state.ReactionAttempts, rkey)
	}

	pending.HeadRecordedAt = now

	log.Info("CI reaction head changed",
		slog.String("previous_head", previousHead),
		slog.String("head", newHead),
		slog.String("attribution", string(attribution)),
		slog.Bool("attempts_reset", resetAttempts),
	)
}

// clearCIDispatchedMark lifts the dispatched mark of a head whose triage
// escalation a passing verdict just ended, so a later failing verdict on
// the same head reaches the triage gate again instead of being skipped as
// dispatched. The store has no operation that clears the flag alone, so
// the row is deleted and written back for the same head; a failure
// between the two leaves no row, which the next pass reads as a head
// change.
func clearCIDispatchedMark(ctx context.Context, store ReconcileStore, issueID, head string, log *slog.Logger) {
	if err := store.DeleteReactionFingerprint(ctx, issueID, ReactionKindCI); err != nil {
		log.Warn("failed to clear the dispatched mark of a triage-escalated head",
			slog.String("head_sha", head),
			slog.Any("error", err),
		)
		return
	}
	if err := store.UpsertReactionFingerprint(ctx, issueID, ReactionKindCI, head); err != nil {
		log.Warn("failed to restore the reaction fingerprint after clearing the dispatched mark",
			slog.String("head_sha", head),
			slog.Any("error", err),
		)
	}
}

// handleCIFailure either schedules a CI-fix dispatch or, once the fixes
// dispatched have reached max retries, escalates.
func handleCIFailure(
	state *State,
	params ReconcileParams,
	pending *PendingReaction,
	result domain.CIResult,
	ref string,
	now time.Time,
	base time.Duration,
	log *slog.Logger,
	ctx context.Context,
	metrics domain.Metrics,
) {
	rkey := ReactionKey(pending.IssueID, ReactionKindCI)
	attempts := state.ReactionAttempts[rkey]
	maxRetries := params.CIFeedback.MaxRetries
	spent := attempts >= maxRetries

	// A standing escalation never reaches this function, so every call
	// dispatches a fix or begins an escalation, and each row counts against
	// agent.max_sessions.
	ciRunHistory := persistence.RunHistory{
		IssueID:        pending.IssueID,
		Identifier:     pending.Identifier,
		DisplayID:      pending.DisplayID,
		IssueTitle:     pending.IssueIdentity.Title,
		IssueURL:       pending.IssueIdentity.URL,
		Attempt:        pending.Attempt,
		StartedAt:      now.Format(time.RFC3339),
		CompletedAt:    now.Format(time.RFC3339),
		Status:         "ci_failed",
		Error:          stringPtr("CI checks failed on ref " + ref),
		TokensMeasured: true,
		ChainID:        pending.StageLineage.ChainID,
		DispatchedAt:   formatDispatchedAt(pending.StageLineage.DispatchedAt),
		Trigger:        new(""),
		DispatchID:     new(""),
		Skills:         new("[]"),

		UsageCompleteness: persistence.UsageComplete,
	}
	recordRunCost(&ciRunHistory, state.Rates)
	if _, err := params.Store.AppendRunHistory(ctx, ciRunHistory); err != nil {
		log.Error("failed to persist CI failure run history",
			slog.Any("error", err),
		)
	}

	if spent {
		escalateCIFailure(state, params, pending, result, ref, attempts, EscalationTriggerBudget, now, base, log, ctx, metrics)
		return
	}
	state.ReactionAttempts[rkey] = attempts + 1

	ciContext := result.ToTemplateMap()

	nextAttempt := pending.Attempt

	scheduleReactionContinuation(ctx, state, params.Store, ScheduleRetryParams{
		IssueID:     pending.IssueID,
		Identifier:  pending.Identifier,
		DisplayID:   pending.DisplayID,
		Attempt:     nextAttempt,
		DelayMS:     continuationDelayMS,
		Error:       "",
		LastSSHHost: pending.LastSSHHost,
		ContinuationContext: map[string]any{
			"ci_failure": ciContext,
		},
		ReactionKind:        ReactionKindCI,
		Continuation:        continuationTargetOf(pending, attempts+1),
		AgentKind:           pending.AgentKind,
		ConfiguredModel:     pending.ConfiguredModel,
		RuleName:            pending.RuleName,
		StageLineage:        pending.StageLineage,
		RuleSettingsApplied: pending.RuleSettingsApplied,
		TemplateID:          pending.TemplateID,
		Logger:              log,

		IssueIdentity: pending.IssueIdentity,
		Trigger:       triggerCIFix,
	}, params.OnRetryFire, now)
	metrics.IncRetries(triggerCIFix)

	log.Info("CI failure detected, scheduling CI fix dispatch",
		slog.String("ref", ref),
		slog.Int("failing_count", result.FailingCount),
		slog.Int("ci_fix_attempt", attempts+1),
		slog.Int("max_retries", maxRetries),
	)
}

// escalateCIFailure begins the escalation on the recorded head and, unless
// that head was escalated before, applies the configured escalation action
// (label, comment, or none). It cancels the retry and releases the claim
// unless a worker runs for the issue, so the issue does not stay reserved by
// a reaction that has stopped dispatching. The pending entry, its attempt
// counter, and its fingerprint row all survive; the row is the epoch
// record. The entry is re-enqueued with backoff so the watch continues past
// exhaustion. Sibling reaction kinds for the same issue are left untouched.
func escalateCIFailure(
	state *State,
	params ReconcileParams,
	pending *PendingReaction,
	result domain.CIResult,
	ref string,
	attempts int,
	trigger ReactionEscalationTrigger,
	now time.Time,
	base time.Duration,
	log *slog.Logger,
	ctx context.Context,
	metrics domain.Metrics,
) {
	// Begun regardless of the tracker write's outcome: the write runs in a
	// detached goroutine whose result this pass cannot observe.
	if beginEscalation(ctx, state, params.Store, pending, trigger, ref, now, log) {
		applyCIEscalation(state, params, pending, result, ref, attempts, trigger, log, ctx, metrics)
	}

	CancelRetry(state, pending.IssueID)

	if err := params.Store.DeleteRetryEntry(ctx, pending.IssueID); err != nil {
		log.Error("failed to delete retry entry during CI escalation",
			slog.Any("error", err),
		)
	}

	releaseClaimUnlessRunning(state, pending.IssueID)

	// The entry, its attempt counter, and its fingerprint row are left
	// untouched: this scope is limited to this kind's own slot, so a
	// sibling reaction's pending entry, counter, and fingerprint for the
	// same issue survive a CI-only escalation. The entry re-enqueues
	// with backoff so the watch continues past exhaustion.
	pending.PendingAttempts++
	delay := computeCIPendingDelay(base, pending.PendingAttempts)
	pending.PendingRetryAt = now.Add(delay)
	requeueReaction(state, ReactionKey(pending.IssueID, ReactionKindCI), pending, waitPollInterval)
}

// applyCIEscalation applies the configured escalation action of the CI
// reaction: the event, and the label when so configured.
func applyCIEscalation(
	state *State,
	params ReconcileParams,
	pending *PendingReaction,
	result domain.CIResult,
	ref string,
	attempts int,
	trigger ReactionEscalationTrigger,
	log *slog.Logger,
	ctx context.Context,
	metrics domain.Metrics,
) {
	if trigger == EscalationTriggerTriage {
		log.Warn("CI triage requested escalation",
			slog.String("ref", ref),
			slog.Int("attempts", attempts),
			slog.Int("max_retries", params.CIFeedback.MaxRetries),
		)
	} else {
		log.Warn("CI fix retries exhausted, escalating",
			slog.String("ref", ref),
			slog.Int("attempts", attempts),
			slog.Int("max_retries", params.CIFeedback.MaxRetries),
		)
	}

	var facts domain.MessageFacts = ciFailureFacts(result, ref, attempts)
	if trigger == EscalationTriggerTriage {
		facts = domain.TriageEscalationFacts{Ref: ref}
	}
	delivery := params.Router.Route(reactionNotification(domain.EventEscalationCIFailure, pending, facts))

	if params.CIFeedback.Escalation == "label" {
		label := params.CIFeedback.EscalationLabel
		if label == "" {
			label = "needs-human"
		}
		if params.TrackerAdapter != nil {
			issueID := pending.IssueID
			tracker := params.TrackerAdapter
			m := metrics
			escalLog := log
			escalAction := params.CIFeedback.Escalation

			state.TrackerOpsWg.Go(func() {
				dctx, cancel := context.WithTimeout(
					context.WithoutCancel(ctx), 30*time.Second)
				defer cancel()

				if err := tracker.AddLabel(dctx, issueID, label); err != nil {
					escalLog.Warn("CI escalation label failed",
						slog.Any("error", err),
					)
					m.IncCIEscalations("error")
				} else {
					m.IncCIEscalations(escalAction)
				}
			})
		}
	}

	// A label owns the escalation counter, so only the other modes record it.
	var record func(action string)
	if params.CIFeedback.Escalation != "label" {
		record = metrics.IncCIEscalations
	}
	publishEscalation(ctx, &state.TrackerOpsWg, delivery, log, record, func(err error) {
		log.Warn("CI escalation comment failed", slog.Any("error", err))
	})
}

// ciFailureFacts reports every check run of result in result order, each
// marked failing by [scmcore.IsFailingConclusion] so the checks a comment
// names never disagree with the verdict that triggered the escalation.
func ciFailureFacts(result domain.CIResult, ref string, attempts int) domain.CIFailureFacts {
	checks := make([]domain.CICheckFacts, 0, len(result.CheckRuns))
	for _, cr := range result.CheckRuns {
		checks = append(checks, domain.CICheckFacts{
			Name:       cr.Name,
			Conclusion: string(cr.Conclusion),
			DetailsURL: cr.DetailsURL,
			Failing:    scmcore.IsFailingConclusion(cr.Conclusion),
		})
	}
	return domain.CIFailureFacts{
		Ref:          ref,
		Attempts:     attempts,
		FailingCount: result.FailingCount,
		Checks:       checks,
	}
}
