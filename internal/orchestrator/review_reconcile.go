package orchestrator

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
)

// reviewPendingBackoffBase is the base interval for review-pending
// exponential backoff.
const reviewPendingBackoffBase = 10 * time.Second

// reviewPendingBackoffCap is the maximum interval between review fetch
// retries.
const reviewPendingBackoffCap = 5 * time.Minute

// reviewPendingDefaultTTL is the default lifetime of a review
// PendingReaction entry.
const reviewPendingDefaultTTL = 30 * time.Minute

// reconcileReviewComments polls review comments for each review-kind
// entry in state.PendingReactions. Called from [ReconcileRunningIssues]
// after [reconcileCIStatus]. Skipped entirely when params.SCMAdapter
// is nil.
func reconcileReviewComments(state *State, params ReconcileParams, log *slog.Logger, ctx context.Context, metrics domain.Metrics) {
	if params.SCMAdapter == nil {
		return
	}

	now := time.Now().UTC()
	if params.NowFunc != nil {
		now = params.NowFunc().UTC()
	}

	ttl := params.ReviewPendingTTL
	pollInterval := time.Duration(params.ReviewConfig.PollIntervalMS) * time.Millisecond
	debounceDuration := time.Duration(params.ReviewConfig.DebounceMS) * time.Millisecond

	for key, pending := range state.PendingReactions {
		if pending.Kind != ReactionKindReview {
			continue
		}
		delete(state.PendingReactions, key)

		reviewData, ok := pending.KindData.(*ReviewReactionData)
		if !ok {
			log.ErrorContext(ctx, "unexpected KindData type for review reaction",
				slog.String("issue_id", pending.IssueID),
				slog.String("type", fmt.Sprintf("%T", pending.KindData)),
			)
			continue
		}

		entryLog := logging.WithIssue(log, pending.IssueID, pending.Identifier)

		// TTL enforcement.
		if ttl > 0 && now.Sub(pending.CreatedAt) > ttl {
			entryLog.Warn("review pending entry exceeded ttl, dropping",
				slog.Int64("ttl_ms", int64(ttl/time.Millisecond)),
				slog.Int64("age_ms", int64(now.Sub(pending.CreatedAt)/time.Millisecond)),
			)
			continue
		}

		// Poll throttle: respect PendingRetryAt.
		if now.Before(pending.PendingRetryAt) {
			state.PendingReactions[key] = pending
			continue
		}

		// Continuation turn cap check.
		rkey := ReactionKey(pending.IssueID, ReactionKindReview)
		turnCount := state.ReactionAttempts[rkey]
		if turnCount >= params.ReviewConfig.MaxContinuationTurns {
			escalateReviewFailure(state, params, pending, turnCount, reviewData, entryLog, ctx, metrics)
			continue
		}

		// Fetch reviews from SCM.
		comments, err := params.SCMAdapter.FetchPendingReviews(ctx, reviewData.PRNumber, reviewData.Owner, reviewData.Repo)
		if err != nil {
			pending.PendingAttempts++
			delay := computeReviewPendingDelay(pending.PendingAttempts)
			pending.PendingRetryAt = now.Add(delay)
			state.PendingReactions[key] = pending
			entryLog.Warn("review fetch failed, retrying with backoff",
				slog.Any("error", err),
				slog.Int("pending_attempts", pending.PendingAttempts),
				slog.Int64("retry_after_ms", int64(delay/time.Millisecond)),
			)
			metrics.IncReviewChecks("error")
			continue
		}

		// Filter outdated comments.
		var actionable []domain.ReviewComment
		for _, c := range comments {
			if !c.Outdated {
				actionable = append(actionable, c)
			}
		}

		// Compute maximum comment timestamp for debounce gating.
		var maxTime time.Time
		for _, c := range actionable {
			if c.SubmittedAt.After(maxTime) {
				maxTime = c.SubmittedAt
			}
		}
		if !maxTime.IsZero() {
			reviewData.LastEventAt = maxTime
		}

		// No actionable comments — re-enqueue with poll interval delay.
		if len(actionable) == 0 {
			pending.PendingRetryAt = now.Add(pollInterval)
			state.PendingReactions[key] = pending
			continue
		}

		// Build fingerprint from sorted actionable comment IDs.
		fingerprint := buildReviewFingerprint(actionable)

		// Dedup check via reaction_fingerprints table.
		if err := params.Store.UpsertReactionFingerprint(ctx, pending.IssueID, ReactionKindReview, fingerprint); err != nil {
			entryLog.Warn("failed to upsert review reaction fingerprint",
				slog.Any("error", err),
			)
		}
		storedFP, dispatched, fpErr := params.Store.GetReactionFingerprint(ctx, pending.IssueID, ReactionKindReview)
		if fpErr != nil {
			entryLog.Warn("failed to get review reaction fingerprint, proceeding without dedup",
				slog.Any("error", fpErr),
			)
		} else if storedFP == fingerprint && dispatched {
			// Already dispatched for this exact set of comments.
			pending.PendingRetryAt = now.Add(pollInterval)
			state.PendingReactions[key] = pending
			entryLog.Debug("review comments already dispatched for this fingerprint")
			continue
		}

		// Debounce: if LastEventAt is recent, defer dispatch.
		if !reviewData.LastEventAt.IsZero() && now.Sub(reviewData.LastEventAt) < debounceDuration {
			pending.PendingRetryAt = reviewData.LastEventAt.Add(debounceDuration)
			state.PendingReactions[key] = pending
			entryLog.Debug("review comments within debounce window, deferring")
			continue
		}

		// Dispatch.
		metrics.IncReviewChecks("dispatched")

		// Mark dispatched synchronously before scheduling the retry to
		// prevent duplicate dispatch on entry recreation.
		if err := params.Store.MarkReactionDispatched(ctx, pending.IssueID, ReactionKindReview); err != nil {
			entryLog.Warn("failed to mark review reaction dispatched",
				slog.Any("error", err),
			)
		}

		reviewContext := buildReviewTemplateMap(actionable)

		CancelRetry(state, pending.IssueID)

		ScheduleRetry(state, ScheduleRetryParams{
			IssueID:     pending.IssueID,
			Identifier:  pending.Identifier,
			DisplayID:   pending.DisplayID,
			Attempt:     pending.Attempt,
			DelayMS:     continuationDelayMS,
			LastSSHHost: pending.LastSSHHost,
			ContinuationContext: map[string]any{
				"review_comments": reviewContext,
			},
			ReactionKind: ReactionKindReview,
		}, params.OnRetryFire)

		state.ReactionAttempts[rkey]++

		entryLog.Info("review comments detected, scheduling review-fix dispatch",
			slog.Int("comment_count", len(actionable)),
			slog.Int("review_fix_attempt", state.ReactionAttempts[rkey]),
			slog.Int("max_continuation_turns", params.ReviewConfig.MaxContinuationTurns),
		)
	}
}

// computeReviewPendingDelay returns the backoff delay for a review
// pending re-check at the given attempt count.
func computeReviewPendingDelay(attempts int) time.Duration {
	if attempts <= 0 {
		return 0
	}
	shift := uint(attempts)
	if shift > 30 {
		return reviewPendingBackoffCap
	}
	delay := reviewPendingBackoffBase * (1 << shift)
	if delay > reviewPendingBackoffCap || delay < 0 {
		return reviewPendingBackoffCap
	}
	return delay
}

// escalateReviewFailure handles the case where review fix continuation
// turns are exhausted. It applies the configured escalation action,
// cancels the retry, and releases the claim.
func escalateReviewFailure(
	state *State,
	params ReconcileParams,
	pending *PendingReaction,
	turnCount int,
	reviewData *ReviewReactionData,
	log *slog.Logger,
	ctx context.Context,
	metrics domain.Metrics,
) {
	log.Warn("review fix continuation turns exhausted, escalating",
		slog.Int("turn_count", turnCount),
		slog.Int("max_continuation_turns", params.ReviewConfig.MaxContinuationTurns),
	)

	switch params.ReviewConfig.Escalation {
	case "label":
		label := params.ReviewConfig.EscalationLabel
		if label == "" {
			label = "needs-human"
		}
		if params.TrackerAdapter != nil {
			issueID := pending.IssueID
			tracker := params.TrackerAdapter
			m := metrics
			escalLog := log
			escalAction := params.ReviewConfig.Escalation

			state.TrackerOpsWg.Add(1)
			go func() {
				defer state.TrackerOpsWg.Done()
				dctx, cancel := context.WithTimeout(
					context.WithoutCancel(ctx), 30*time.Second)
				defer cancel()

				if err := tracker.AddLabel(dctx, issueID, label); err != nil {
					escalLog.Warn("review escalation label failed",
						slog.Any("error", err),
					)
					m.IncReviewEscalations("error")
				} else {
					m.IncReviewEscalations(escalAction)
				}
			}()
		} else {
			metrics.IncReviewEscalations(params.ReviewConfig.Escalation)
		}

	case "comment", "":
		commentText := buildReviewEscalationComment(reviewData, turnCount)
		if params.TrackerAdapter != nil {
			issueID := pending.IssueID
			tracker := params.TrackerAdapter
			m := metrics
			escalLog := log
			ct := commentText
			escalAction := params.ReviewConfig.Escalation
			if escalAction == "" {
				escalAction = "comment"
			}

			state.TrackerOpsWg.Add(1)
			go func() {
				defer state.TrackerOpsWg.Done()
				dctx, cancel := context.WithTimeout(
					context.WithoutCancel(ctx), 30*time.Second)
				defer cancel()

				if err := tracker.CommentIssue(dctx, issueID, ct); err != nil {
					escalLog.Warn("review escalation comment failed",
						slog.Any("error", err),
					)
					m.IncReviewEscalations("error")
				} else {
					m.IncReviewEscalations(escalAction)
				}
			}()
		} else {
			action := params.ReviewConfig.Escalation
			if action == "" {
				action = "comment"
			}
			metrics.IncReviewEscalations(action)
		}
	}

	CancelRetry(state, pending.IssueID)

	if err := params.Store.DeleteRetryEntry(ctx, pending.IssueID); err != nil {
		log.Error("failed to delete retry entry during review escalation",
			slog.Any("error", err),
		)
	}

	delete(state.Claimed, pending.IssueID)
	ClearReactionsForIssue(ctx, state, params.Store, pending.IssueID, log)
}

// buildReviewEscalationComment builds a plain-text escalation comment
// for review fixes that exceeded the turn budget.
func buildReviewEscalationComment(reviewData *ReviewReactionData, turnCount int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Review fix continuation turns exhausted for PR #%d on branch %s.\n", reviewData.PRNumber, reviewData.Branch)
	fmt.Fprintf(&b, "%d continuation turns attempted. Remaining review comments require human attention.", turnCount)
	return b.String()
}

// buildReviewFingerprint constructs a deterministic fingerprint from the
// set of non-outdated review comments. The fingerprint is the lowercase
// hex SHA-256 hash of sorted, newline-joined comment IDs. Returns an
// empty string when the input is empty.
func buildReviewFingerprint(comments []domain.ReviewComment) string {
	if len(comments) == 0 {
		return ""
	}

	ids := make([]string, len(comments))
	for i, c := range comments {
		ids[i] = c.ID
	}
	sort.Strings(ids)

	h := sha256.Sum256([]byte(strings.Join(ids, "\n")))
	return fmt.Sprintf("%x", h)
}

// buildReviewTemplateMap converts review comments to the map format
// expected by the prompt template's review_comments variable.
func buildReviewTemplateMap(comments []domain.ReviewComment) []map[string]any {
	result := make([]map[string]any, len(comments))
	for i, c := range comments {
		result[i] = map[string]any{
			"id":         c.ID,
			"file":       c.FilePath,
			"start_line": c.StartLine,
			"end_line":   c.EndLine,
			"reviewer":   c.Reviewer,
			"body":       c.Body,
		}
	}
	return result
}
