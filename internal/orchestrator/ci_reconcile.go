package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/persistence"
)

// ciPendingBackoffBaseDefault is the fallback base interval for CI-pending
// exponential backoff when the configured poll interval is zero or negative.
const ciPendingBackoffBaseDefault = 10 * time.Second

// ciPendingBackoffCap is the maximum interval between CI status checks.
const ciPendingBackoffCap = 5 * time.Minute

// ciPendingDefaultTTL is the default lifetime of a PendingReaction entry.
// Entries older than this are dropped on the next reconcile tick.
const ciPendingDefaultTTL = 30 * time.Minute

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

// reconcileCIStatus polls CI status for each CI-kind entry in
// state.PendingReactions. Called from ReconcileRunningIssues after
// reconcileTrackerState. Skipped entirely when params.CIProvider is nil.
//
// Entries that are not yet due (PendingRetryAt in the future) are
// re-enqueued without making an API call, applying exponential backoff.
// Entries older than the configured TTL are dropped and a warning is logged.
func reconcileCIStatus(state *State, params ReconcileParams, log *slog.Logger, ctx context.Context, metrics domain.Metrics) {
	if params.CIProvider == nil {
		return
	}

	now := time.Now().UTC()
	if params.NowFunc != nil {
		now = params.NowFunc().UTC()
	}

	ttl := params.CIPendingTTL
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

		if ttl > 0 && now.Sub(pending.CreatedAt) > ttl {
			entryLog.Warn("ci pending entry exceeded ttl, dropping",
				slog.Int64("ttl_ms", int64(ttl/time.Millisecond)),
				slog.Int64("age_ms", int64(now.Sub(pending.CreatedAt)/time.Millisecond)),
			)
			continue
		}

		if now.Before(pending.PendingRetryAt) {
			state.PendingReactions[key] = pending
			continue
		}

		ref := ciData.SHA
		if ref == "" {
			ref = ciData.Branch
		}

		// Fingerprint dedup: upsert the fingerprint row (resets dispatched
		// when the ref changes) and skip entries already dispatched for this
		// exact ref. Errors are non-fatal — best-effort dedup.
		if err := params.Store.UpsertReactionFingerprint(ctx, pending.IssueID, ReactionKindCI, ref); err != nil {
			entryLog.Warn("failed to upsert reaction fingerprint",
				slog.Any("error", err),
			)
		}
		storedFP, dispatched, fpErr := params.Store.GetReactionFingerprint(ctx, pending.IssueID, ReactionKindCI)
		if fpErr != nil {
			entryLog.Warn("failed to get reaction fingerprint, proceeding without dedup",
				slog.Any("error", fpErr),
			)
		} else if storedFP == ref && dispatched {
			entryLog.Debug("CI reaction already dispatched for this ref, skipping",
				slog.String("ref", ref),
			)
			continue
		}

		result, err := params.CIProvider.FetchCIStatus(ctx, ref)
		if err != nil {
			pending.PendingAttempts++
			delay := computeCIPendingDelay(base, pending.PendingAttempts)
			pending.PendingRetryAt = now.Add(delay)
			entryLog.Warn("ci status fetch failed, retrying with backoff",
				slog.String("ref", ref),
				slog.Any("error", err),
				slog.Int("pending_attempts", pending.PendingAttempts),
				slog.Int64("retry_after_ms", int64(delay/time.Millisecond)),
			)
			metrics.IncCIStatusChecks("error")
			state.PendingReactions[key] = pending
			continue
		}

		metrics.IncCIStatusChecks(string(result.Status))

		rkey := ReactionKey(pending.IssueID, ReactionKindCI)

		switch result.Status {
		case domain.CIStatusPassing:
			delete(state.ReactionAttempts, rkey)
			if err := params.Store.DeleteReactionFingerprint(ctx, pending.IssueID, ReactionKindCI); err != nil {
				entryLog.Warn("failed to delete reaction fingerprint on CI pass",
					slog.Any("error", err),
				)
			}
			entryLog.Info("CI passing, no action needed",
				slog.String("ref", ref),
			)

		case domain.CIStatusPending:
			pending.PendingAttempts++
			delay := computeCIPendingDelay(base, pending.PendingAttempts)
			pending.PendingRetryAt = now.Add(delay)
			state.PendingReactions[key] = pending
			entryLog.Debug("CI pending, will re-check after backoff",
				slog.String("ref", ref),
				slog.Int("pending_attempts", pending.PendingAttempts),
				slog.Int64("retry_after_ms", int64(delay/time.Millisecond)),
			)

		case domain.CIStatusFailing:
			handleCIFailure(state, params, pending, result, ref, entryLog, ctx, metrics)

		default:
			entryLog.Warn("CI status provider returned unrecognized status, re-enqueueing",
				slog.String("status", string(result.Status)),
				slog.String("ref", ref),
			)
			metrics.IncCIStatusChecks("error")
			pending.PendingAttempts++
			pending.PendingRetryAt = now.Add(computeCIPendingDelay(base, pending.PendingAttempts))
			state.PendingReactions[key] = pending
		}
	}
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
	log *slog.Logger,
	ctx context.Context,
	metrics domain.Metrics,
) {
	now := time.Now().UTC()

	ciRunHistory := persistence.RunHistory{
		IssueID:     pending.IssueID,
		Identifier:  pending.Identifier,
		DisplayID:   pending.DisplayID,
		Attempt:     pending.Attempt,
		StartedAt:   now.Format(time.RFC3339),
		CompletedAt: now.Format(time.RFC3339),
		Status:      "ci_failed",
		Error:       stringPtr("CI checks failed on ref " + ref),
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
		escalateCIFailure(state, params, pending, result, ref, attempts, log, ctx, metrics)
		return
	}

	ciContext := result.ToTemplateMap()

	CancelRetry(state, pending.IssueID)

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
	}, params.OnRetryFire)
	metrics.IncRetries(triggerCIFix)

	log.Info("CI failure detected, scheduling CI fix dispatch",
		slog.String("ref", ref),
		slog.Int("failing_count", result.FailingCount),
		slog.Int("ci_fix_attempt", attempts),
		slog.Int("max_retries", maxRetries),
	)
}

// escalateCIFailure handles the case where CI fix retries are exhausted.
// It applies the configured escalation action (label or comment), cancels
// the retry, and releases the claim.
func escalateCIFailure(
	state *State,
	params ReconcileParams,
	pending *PendingReaction,
	result domain.CIResult,
	ref string,
	attempts int,
	log *slog.Logger,
	ctx context.Context,
	metrics domain.Metrics,
) {
	log.Warn("CI fix retries exhausted, escalating",
		slog.String("ref", ref),
		slog.Int("attempts", attempts),
		slog.Int("max_retries", params.CIFeedback.MaxRetries),
	)

	metrics.IncCIEscalations(params.CIFeedback.Escalation)

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

			state.TrackerOpsWg.Add(1)
			go func() {
				defer state.TrackerOpsWg.Done()
				dctx, cancel := context.WithTimeout(
					context.WithoutCancel(ctx), 30*time.Second)
				defer cancel()

				if err := tracker.AddLabel(dctx, issueID, label); err != nil {
					escalLog.Warn("CI escalation label failed",
						slog.Any("error", err),
					)
					m.IncCIEscalations("error")
				}
			}()
		}

	case "comment", "":
		commentText := buildCIEscalationComment(result, ref, attempts)
		if params.TrackerAdapter != nil {
			issueID := pending.IssueID
			tracker := params.TrackerAdapter
			m := metrics
			escalLog := log
			ct := commentText

			state.TrackerOpsWg.Add(1)
			go func() {
				defer state.TrackerOpsWg.Done()
				dctx, cancel := context.WithTimeout(
					context.WithoutCancel(ctx), 30*time.Second)
				defer cancel()

				if err := tracker.CommentIssue(dctx, issueID, ct); err != nil {
					escalLog.Warn("CI escalation comment failed",
						slog.Any("error", err),
					)
					m.IncCIEscalations("error")
				}
			}()
		}
	}

	CancelRetry(state, pending.IssueID)

	if err := params.Store.DeleteRetryEntry(ctx, pending.IssueID); err != nil {
		log.Error("failed to delete retry entry during CI escalation",
			slog.Any("error", err),
		)
	}

	delete(state.Claimed, pending.IssueID)
	ClearReactionsForIssue(ctx, state, params.Store, pending.IssueID, log)

	if err := params.Store.DeleteReactionFingerprint(ctx, pending.IssueID, ReactionKindCI); err != nil {
		log.Warn("failed to delete reaction fingerprint during CI escalation",
			slog.Any("error", err),
		)
	}
}

// buildCIEscalationComment builds a plain-text escalation comment for
// CI failures that exceeded the retry budget. Plain text is used so the
// comment renders consistently across all tracker adapters.
func buildCIEscalationComment(result domain.CIResult, ref string, attempts int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CI fix retries exhausted\n\n")
	fmt.Fprintf(&b, "Sortie attempted %d CI-fix continuation(s) on ref %s but CI is still failing.\n\n", attempts, ref)

	if result.FailingCount > 0 {
		fmt.Fprintf(&b, "Failing checks: %d\n", result.FailingCount)
	}

	for _, cr := range result.CheckRuns {
		switch cr.Conclusion {
		case domain.CheckConclusionFailure, domain.CheckConclusionTimedOut, domain.CheckConclusionCancelled:
			fmt.Fprintf(&b, "- %s: %s", cr.Name, cr.Conclusion)
			if cr.DetailsURL != "" {
				fmt.Fprintf(&b, " (details: %s)", cr.DetailsURL)
			}
			b.WriteString("\n")
		}
	}

	b.WriteString("\nManual intervention required.")
	return b.String()
}
