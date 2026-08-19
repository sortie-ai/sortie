package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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
// and a warning is logged. A zero or negative CIWatchWindow disables the
// bound.
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
		if params.CIWatchWindow > 0 && now.Sub(ageBasis) > params.CIWatchWindow {
			delete(state.ReactionAttempts, rkey)
			entryLog.Warn("ci watch window elapsed, dropping",
				slog.Int64("window_ms", int64(params.CIWatchWindow/time.Millisecond)),
				slog.Int64("age_ms", int64(now.Sub(ageBasis)/time.Millisecond)),
			)
			continue
		}

		if now.Before(pending.PendingRetryAt) {
			state.PendingReactions[key] = pending
			continue
		}

		status, mergeErr := params.SCMAdapter.GetMergeability(ctx, ciData.PRNumber, ciData.Owner, ciData.Repo)
		if mergeErr != nil {
			var scmErr *domain.SCMError
			if errors.As(mergeErr, &scmErr) && scmErr.Kind == domain.ErrSCMNotFound {
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
			state.PendingReactions[key] = pending
			continue
		}

		// The merged check runs before the closed check so a provider
		// whose closed state subsumes merging still ends the watch
		// through the merged branch and logs the merge rather than a
		// close.
		if status.Merged {
			delete(state.ReactionAttempts, rkey)
			entryLog.Info("ci watch ended: pull request merged",
				slog.String("head_sha", status.HeadSHA),
			)
			continue
		}
		if status.Closed {
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
			state.PendingReactions[key] = pending
			continue
		}

		// The epoch record is read before it is written, because this
		// pass needs the previous head to detect a change; an
		// upsert-then-read ordering would always read back the value
		// just written and no head change would ever be detected. The
		// durable observation helper is not used here: its timestamp
		// survives a restart, and the attribution boundary this pass
		// maintains (PendingReaction.HeadRecordedAt) deliberately must
		// not.
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
				state.PendingReactions[key] = pending
				continue
			}
			// The epoch transition already clears dispatched through the
			// fingerprint upsert; the status read below proceeds
			// unconditionally on this branch, so the local dispatched
			// value needs no update here.
			applyCIEpochTransition(state, params, pending, rkey, storedHead, status.HeadSHA, now, ctx, entryLog)
		} else if dispatched {
			pending.PendingAttempts++
			delay := computeCIPendingDelay(base, pending.PendingAttempts)
			pending.PendingRetryAt = now.Add(delay)
			entryLog.Debug("CI reaction already dispatched for this head, skipping",
				slog.String("head_sha", status.HeadSHA),
			)
			state.PendingReactions[key] = pending
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
			state.PendingReactions[key] = pending
			continue
		}

		metrics.IncCIStatusChecks(string(result.Status))

		switch result.Status {
		case domain.CIStatusPassing:
			delete(state.ReactionAttempts, rkey)
			pending.PendingAttempts++
			delay := computeCIPendingDelay(base, pending.PendingAttempts)
			pending.PendingRetryAt = now.Add(delay)
			entryLog.Info("CI passing on current head, continuing to watch",
				slog.String("head_sha", status.HeadSHA),
			)
			state.PendingReactions[key] = pending

		case domain.CIStatusPending:
			pending.PendingAttempts++
			delay := computeCIPendingDelay(base, pending.PendingAttempts)
			pending.PendingRetryAt = now.Add(delay)
			state.PendingReactions[key] = pending
			entryLog.Debug("CI pending, will re-check after backoff",
				slog.String("head_sha", status.HeadSHA),
				slog.Int("pending_attempts", pending.PendingAttempts),
				slog.Int64("retry_after_ms", int64(delay/time.Millisecond)),
			)

		case domain.CIStatusFailing:
			if incumbent := retrySlotIncumbent(state, pending.IssueID); incumbent != nil {
				logRetrySlotDeferral(entryLog, ReactionKindCI, incumbent)
				pending.PendingAttempts++
				delay := computeCIPendingDelay(base, pending.PendingAttempts)
				pending.PendingRetryAt = now.Add(delay)
				state.PendingReactions[key] = pending
			} else {
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
			state.PendingReactions[key] = pending
		}
	}
}

// applyCIEpochTransition applies the runtime consequences of a detected
// pull request head change: it resets the backoff counter and the
// per-head escalation flag unconditionally, resets the CI retry budget
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
	pending.EscalatedForCurrentHead = false

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

// handleCIFailure records a CI failure in run_history, increments the
// CI fix attempt counter, and either schedules a CI-fix dispatch or
// escalates if max retries is exceeded.
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
	ciRunHistory := persistence.RunHistory{
		IssueID:        pending.IssueID,
		Identifier:     pending.Identifier,
		DisplayID:      pending.DisplayID,
		Attempt:        pending.Attempt,
		StartedAt:      now.Format(time.RFC3339),
		CompletedAt:    now.Format(time.RFC3339),
		Status:         "ci_failed",
		Error:          stringPtr("CI checks failed on ref " + ref),
		TokensMeasured: true,
	}
	if _, err := params.Store.AppendRunHistory(ctx, ciRunHistory); err != nil {
		log.Error("failed to persist CI failure run history",
			slog.Any("error", err),
		)
	}

	rkey := ReactionKey(pending.IssueID, ReactionKindCI)
	state.ReactionAttempts[rkey]++
	attempts := state.ReactionAttempts[rkey]

	maxRetries := params.CIFeedback.MaxRetries

	if attempts > maxRetries {
		escalateCIFailure(state, params, pending, result, ref, attempts, now, base, log, ctx, metrics)
		return
	}

	ciContext := result.ToTemplateMap()

	nextAttempt := pending.Attempt

	ScheduleRetry(state, ScheduleRetryParams{
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
		ReactionKind: ReactionKindCI,
		AgentKind:    pending.AgentKind,
		RuleName:     pending.RuleName,
		TemplateID:   pending.TemplateID,
		Logger:       log,
	}, params.OnRetryFire)
	metrics.IncRetries(triggerCIFix)

	log.Info("CI failure detected, scheduling CI fix dispatch",
		slog.String("ref", ref),
		slog.Int("failing_count", result.FailingCount),
		slog.Int("ci_fix_attempt", attempts),
		slog.Int("max_retries", maxRetries),
	)
}

// escalateCIFailure handles the case where the CI retry budget has been
// spent for the commit currently recorded. It applies the configured
// escalation action (label or comment) at most once per recorded head,
// cancels the retry, and releases the claim so the issue does not stay
// reserved by a reaction that has stopped dispatching. The pending
// entry, its attempt counter, and its fingerprint row all survive: the
// counter must stay over budget so no further continuation dispatches
// until a new epoch resets it, and the row is the epoch record. The
// entry is re-enqueued with backoff so the watch continues past
// exhaustion. Sibling reaction kinds for the same issue are left
// untouched.
func escalateCIFailure(
	state *State,
	params ReconcileParams,
	pending *PendingReaction,
	result domain.CIResult,
	ref string,
	attempts int,
	now time.Time,
	base time.Duration,
	log *slog.Logger,
	ctx context.Context,
	metrics domain.Metrics,
) {
	if !pending.EscalatedForCurrentHead {
		log.Warn("CI fix retries exhausted, escalating",
			slog.String("ref", ref),
			slog.Int("attempts", attempts),
			slog.Int("max_retries", params.CIFeedback.MaxRetries),
		)

		switch params.CIFeedback.Escalation {
		case "label":
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

		case "comment", "":
			commentText := buildCIEscalationComment(result, ref, attempts)
			if params.TrackerAdapter != nil {
				issueID := pending.IssueID
				tracker := params.TrackerAdapter
				m := metrics
				escalLog := log
				ct := commentText
				escalAction := params.CIFeedback.Escalation
				if escalAction == "" {
					escalAction = "comment"
				}

				state.TrackerOpsWg.Go(func() {
					dctx, cancel := context.WithTimeout(
						context.WithoutCancel(ctx), 30*time.Second)
					defer cancel()

					if err := tracker.CommentIssue(dctx, issueID, ct); err != nil {
						escalLog.Warn("CI escalation comment failed",
							slog.Any("error", err),
						)
						m.IncCIEscalations("error")
					} else {
						m.IncCIEscalations(escalAction)
					}
				})
			}
		}
	}

	// Set regardless of the tracker write's outcome: the write runs in
	// a detached goroutine whose result this pass cannot observe.
	pending.EscalatedForCurrentHead = true

	CancelRetry(state, pending.IssueID)

	if err := params.Store.DeleteRetryEntry(ctx, pending.IssueID); err != nil {
		log.Error("failed to delete retry entry during CI escalation",
			slog.Any("error", err),
		)
	}

	delete(state.Claimed, pending.IssueID)

	// The entry, its attempt counter, and its fingerprint row are left
	// untouched: this scope is limited to this kind's own slot, so a
	// sibling reaction's pending entry, counter, and fingerprint for the
	// same issue survive a CI-only escalation. The entry re-enqueues
	// with backoff so the watch continues past exhaustion.
	pending.PendingAttempts++
	delay := computeCIPendingDelay(base, pending.PendingAttempts)
	pending.PendingRetryAt = now.Add(delay)
	state.PendingReactions[ReactionKey(pending.IssueID, ReactionKindCI)] = pending
}

// buildCIEscalationComment builds a plain-text escalation comment for
// CI failures that exceeded the retry budget. Plain text is used so the
// comment renders consistently across all tracker adapters.
//
// The comment names exactly the checks the verdict counted as failing,
// obtaining that classification from [scmcore.IsFailingConclusion]
// rather than restating it, so the named checks never disagree with the
// verdict that triggered the escalation.
func buildCIEscalationComment(result domain.CIResult, ref string, attempts int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CI fix retries exhausted\n\n")
	fmt.Fprintf(&b, "Sortie attempted %d CI-fix continuation(s) on ref %s but CI is still failing.\n\n", attempts, ref)

	if result.FailingCount > 0 {
		fmt.Fprintf(&b, "Failing checks: %d\n", result.FailingCount)
	}

	for _, cr := range result.CheckRuns {
		if !scmcore.IsFailingConclusion(cr.Conclusion) {
			continue
		}
		fmt.Fprintf(&b, "- %s: %s", cr.Name, cr.Conclusion)
		if cr.DetailsURL != "" {
			fmt.Fprintf(&b, " (details: %s)", cr.DetailsURL)
		}
		b.WriteString("\n")
	}

	b.WriteString("\nManual intervention required.")
	return b.String()
}
