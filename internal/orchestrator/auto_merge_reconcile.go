package orchestrator

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
)

// autoMergePendingBackoffBase is the base interval for auto-merge
// pending exponential backoff.
const autoMergePendingBackoffBase = 10 * time.Second

// autoMergePendingBackoffCap is the maximum interval between
// auto-merge precondition checks.
const autoMergePendingBackoffCap = 5 * time.Minute

// autoMergePendingDefaultTTL is the default lifetime of an auto-merge
// PendingReaction entry.
const autoMergePendingDefaultTTL = 30 * time.Minute

// autoMergeDocsURL is the operator-facing reference for GitHub merge
// scope and token guidance, surfaced in structured logs.
const autoMergeDocsURL = "https://docs.github.com/en/rest/pulls/pulls#merge-a-pull-request"

// reconcileAutoMerge polls auto-merge preconditions for each merge-kind
// entry in state.PendingReactions and, when all preconditions are
// satisfied and the per-fingerprint dedup has not seen this attempt,
// executes the merge call.
//
// Called from [ReconcileRunningIssues] last so the CI and review
// reconcile passes have already updated their state. Skipped entirely
// when params.SCMAdapter is nil or params.AutoMergeReactionConfigured
// is false.
func reconcileAutoMerge(state *State, params ReconcileParams, log *slog.Logger, ctx context.Context, metrics domain.Metrics) {
	if params.SCMAdapter == nil {
		return
	}
	if !params.AutoMergeReactionConfigured {
		return
	}

	now := time.Now().UTC()
	if params.NowFunc != nil {
		now = params.NowFunc().UTC()
	}

	pollInterval := max(time.Duration(params.AutoMergeConfig.PollIntervalMS)*time.Millisecond, 30*time.Second)
	ttl := params.AutoMergePendingTTL

	// Consume a scheduled preflight retry at the top of the function
	// so a successful retry restores merge processing in the same
	// tick. The retry runs at most once per orchestrator lifetime;
	// clear the timestamp eagerly so a verifier panic does not leave
	// the retry available on a future tick.
	if !state.AutoMergePreflightRetryDueAt.IsZero() && !now.Before(state.AutoMergePreflightRetryDueAt) {
		state.AutoMergePreflightRetryDueAt = time.Time{}
		passed, _, retryErr := RunAutoMergePreflightRetry(ctx, params.SCMAdapter, params.AutoMergeConfig.DeleteBranch, log)
		switch {
		case retryErr == nil && passed:
			state.AutoMergePreflightFailed = false
		default:
			// Auth-class scope failure and transport-class retry
			// failure both keep the sticky flag set. The verifier
			// emitted the appropriate ERROR or WARN log before
			// returning; no further re-scheduling occurs.
			state.AutoMergePreflightFailed = true
		}
	}

	for key, pending := range state.PendingReactions {
		if pending.Kind != ReactionKindAutoMerge {
			continue
		}
		delete(state.PendingReactions, key)

		autoMergeData, ok := pending.KindData.(*AutoMergeReactionData)
		if !ok {
			log.ErrorContext(ctx, "unexpected KindData type for auto_merge reaction",
				slog.String("issue_id", pending.IssueID),
				slog.String("type", fmt.Sprintf("%T", pending.KindData)),
			)
			continue
		}

		entryLog := logging.WithIssue(log, pending.IssueID, pending.Identifier)

		if state.AutoMergePreflightFailed {
			entryLog.Warn("auto_merge skipped: preflight failed",
				slog.Int("pr_number", autoMergeData.PRNumber),
			)
			continue
		}

		if ttl > 0 && now.Sub(pending.CreatedAt) > ttl {
			entryLog.Warn("auto_merge pending entry exceeded ttl, dropping",
				slog.Int64("ttl_ms", int64(ttl/time.Millisecond)),
				slog.Int64("age_ms", int64(now.Sub(pending.CreatedAt)/time.Millisecond)),
			)
			continue
		}

		if now.Before(pending.PendingRetryAt) {
			state.PendingReactions[key] = pending
			continue
		}

		rkey := ReactionKey(pending.IssueID, ReactionKindAutoMerge)
		turnCount := state.ReactionAttempts[rkey]
		if params.AutoMergeConfig.MaxRetries > 0 && turnCount >= params.AutoMergeConfig.MaxRetries {
			escalateAutoMergeFailure(state, params, pending, turnCount, autoMergeData, entryLog, ctx, metrics)
			continue
		}

		mergeStatus, err := params.SCMAdapter.GetMergeability(ctx, autoMergeData.PRNumber, autoMergeData.Owner, autoMergeData.Repo)
		if err != nil {
			delay := applyAutoMergeBackoff(state, key, pending, now, pollInterval)
			entryLog.Warn("auto_merge mergeability fetch failed",
				slog.Any("error", err),
				slog.Int("pending_attempts", pending.PendingAttempts),
				slog.Int64("retry_after_ms", int64(delay/time.Millisecond)),
			)
			metrics.IncAutoMergeReactions(outcomeError)
			continue
		}

		if mergeStatus.Draft {
			pending.PendingRetryAt = now.Add(pollInterval)
			state.PendingReactions[key] = pending
			entryLog.Debug("auto_merge skipped: PR is draft",
				slog.Int("pr_number", autoMergeData.PRNumber),
			)
			continue
		}

		if mergeStatus.Mergeability == domain.MergeabilityUnknown {
			pending.PendingRetryAt = now.Add(pollInterval)
			state.PendingReactions[key] = pending
			entryLog.Debug("auto_merge deferred: mergeability unknown",
				slog.Int("pr_number", autoMergeData.PRNumber),
			)
			continue
		}

		if mergeStatus.Mergeability != domain.MergeabilityClean && mergeStatus.Mergeability != domain.MergeabilityUnstable {
			pending.PendingRetryAt = now.Add(pollInterval)
			state.PendingReactions[key] = pending
			entryLog.Debug("auto_merge deferred: PR not mergeable",
				slog.String("state", string(mergeStatus.Mergeability)),
				slog.Int("pr_number", autoMergeData.PRNumber),
			)
			continue
		}

		reviewDecision, err := params.SCMAdapter.GetReviewDecision(ctx, autoMergeData.PRNumber, autoMergeData.Owner, autoMergeData.Repo)
		if err != nil {
			delay := applyAutoMergeBackoff(state, key, pending, now, pollInterval)
			entryLog.Warn("auto_merge review decision fetch failed",
				slog.Any("error", err),
				slog.Int("pending_attempts", pending.PendingAttempts),
				slog.Int64("retry_after_ms", int64(delay/time.Millisecond)),
			)
			metrics.IncAutoMergeReactions(outcomeError)
			continue
		}

		if reviewDecision != domain.ReviewDecisionApproved && reviewDecision != domain.ReviewDecisionNotRequired {
			pending.PendingRetryAt = now.Add(pollInterval)
			state.PendingReactions[key] = pending
			entryLog.Debug("auto_merge deferred: review decision not approved",
				slog.String("decision", string(reviewDecision)),
				slog.Int("pr_number", autoMergeData.PRNumber),
			)
			continue
		}

		if params.AutoMergeConfig.RequireCI {
			ciConclusion, ciErr := params.SCMAdapter.GetCIStatus(ctx, autoMergeData.PRNumber, autoMergeData.Owner, autoMergeData.Repo)
			if ciErr != nil {
				delay := applyAutoMergeBackoff(state, key, pending, now, pollInterval)
				entryLog.Warn("auto_merge ci status fetch failed",
					slog.Any("error", ciErr),
					slog.Int("pending_attempts", pending.PendingAttempts),
					slog.Int64("retry_after_ms", int64(delay/time.Millisecond)),
				)
				metrics.IncAutoMergeReactions(outcomeError)
				continue
			}
			if ciConclusion == "pending" {
				pending.PendingRetryAt = now.Add(pollInterval)
				state.PendingReactions[key] = pending
				entryLog.Debug("auto_merge deferred: CI pending",
					slog.Int("pr_number", autoMergeData.PRNumber),
				)
				continue
			}
			if ciConclusion != "success" && ciConclusion != "" {
				pending.PendingRetryAt = now.Add(pollInterval)
				state.PendingReactions[key] = pending
				entryLog.Debug("auto_merge deferred: CI not green",
					slog.String("conclusion", ciConclusion),
					slog.Int("pr_number", autoMergeData.PRNumber),
				)
				continue
			}
		}

		if mergeStatus.HeadSHA == "" {
			pending.PendingRetryAt = now.Add(pollInterval)
			state.PendingReactions[key] = pending
			entryLog.Debug("auto_merge deferred: missing head sha",
				slog.Int("pr_number", autoMergeData.PRNumber),
			)
			continue
		}

		fingerprint := buildAutoMergeFingerprint(mergeStatus.HeadSHA, reviewDecision)

		if err := params.Store.UpsertReactionFingerprint(ctx, pending.IssueID, ReactionKindAutoMerge, fingerprint); err != nil {
			entryLog.Warn("failed to upsert auto_merge reaction fingerprint",
				slog.Any("error", err),
			)
		}
		storedFP, dispatched, fpErr := params.Store.GetReactionFingerprint(ctx, pending.IssueID, ReactionKindAutoMerge)
		if fpErr == nil && storedFP == fingerprint && dispatched {
			pending.PendingRetryAt = now.Add(pollInterval)
			state.PendingReactions[key] = pending
			entryLog.Debug("auto_merge already attempted for this fingerprint",
				slog.Int("pr_number", autoMergeData.PRNumber),
			)
			continue
		}

		mergeResult, err := params.SCMAdapter.MergePR(
			ctx,
			autoMergeData.PRNumber,
			autoMergeData.Owner,
			autoMergeData.Repo,
			params.AutoMergeConfig.Strategy,
			"",
			"",
			mergeStatus.HeadSHA,
		)

		state.ReactionAttempts[rkey]++

		if err != nil {
			handleMergeError(state, params, key, pending, autoMergeData, err, now, pollInterval, entryLog, ctx, metrics)
			continue
		}

		// MarkReactionDispatched runs only on a successful merge so the
		// fingerprint dedup branch above does not short-circuit a retry
		// after a transient or transport-class failure. The duplicate
		// merge path inside handleMergeError marks the same fingerprint
		// dispatched before clearing it on idempotent success.
		if markErr := params.Store.MarkReactionDispatched(ctx, pending.IssueID, ReactionKindAutoMerge); markErr != nil {
			entryLog.Warn("failed to mark auto_merge dispatched",
				slog.Any("error", markErr),
			)
		}

		entryLog.Info("auto_merge merged PR",
			slog.Int("pr_number", autoMergeData.PRNumber),
			slog.String("strategy", string(params.AutoMergeConfig.Strategy)),
			slog.String("merge_sha", mergeResult.SHA),
		)
		metrics.IncAutoMergeReactions("merged")

		postAutoMergeSuccess(state, params, pending, autoMergeData, mergeResult, entryLog, ctx)
	}
}

// applyAutoMergeBackoff applies exponential-backoff delay and rewrites
// the pending entry to state.PendingReactions on a transient failure.
// Returns the chosen delay so the caller can populate the structured
// log it emits at its own (stable, literal) message.
func applyAutoMergeBackoff(state *State, key string, pending *PendingReaction, now time.Time, pollInterval time.Duration) time.Duration {
	pending.PendingAttempts++
	delay := max(computeAutoMergePendingDelay(pending.PendingAttempts), pollInterval)
	pending.PendingRetryAt = now.Add(delay)
	state.PendingReactions[key] = pending
	return delay
}

// computeAutoMergePendingDelay returns the backoff delay for a
// precondition recheck at the given attempt count. Attempt 0 returns
// zero (immediate). Each subsequent attempt returns
// autoMergePendingBackoffBase * 2^attempts, capped at
// autoMergePendingBackoffCap.
func computeAutoMergePendingDelay(attempts int) time.Duration {
	if attempts <= 0 {
		return 0
	}
	shift := uint(attempts)
	if shift > 30 {
		return autoMergePendingBackoffCap
	}
	delay := autoMergePendingBackoffBase * (1 << shift)
	if delay > autoMergePendingBackoffCap || delay < 0 {
		return autoMergePendingBackoffCap
	}
	return delay
}

// buildAutoMergeFingerprint computes the SHA-256 hex digest of
// headSHA + "\n" + reviewDecision. Empty headSHA returns the empty
// string; the caller treats that as "do not merge".
func buildAutoMergeFingerprint(headSHA string, decision domain.ReviewDecision) string {
	if headSHA == "" {
		return ""
	}
	input := headSHA + "\n" + string(decision)
	h := sha256.Sum256([]byte(input))
	return fmt.Sprintf("%x", h)
}

// postAutoMergeSuccess runs the after-merge cleanup: removes the
// pending entry, posts a tracker comment, deletes the source branch
// when configured, and clears the merge-kind fingerprint. Only the
// merge-kind state for this issue is touched; CI and review entries
// for the same issue are preserved.
func postAutoMergeSuccess(state *State, params ReconcileParams, pending *PendingReaction, autoMergeData *AutoMergeReactionData, mergeResult domain.MergeResult, log *slog.Logger, ctx context.Context) {
	delete(state.PendingReactions, ReactionKey(pending.IssueID, ReactionKindAutoMerge))

	if params.AutoMergeConfig.DeleteBranch {
		owner := autoMergeData.Owner
		repo := autoMergeData.Repo
		branch := autoMergeData.Branch
		adapter := params.SCMAdapter
		branchLog := log

		state.TrackerOpsWg.Go(func() {
			dctx, cancel := context.WithTimeout(
				context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()

			if err := adapter.DeleteBranch(dctx, owner, repo, branch); err != nil {
				var scmErr *domain.SCMError
				if errors.As(err, &scmErr) && scmErr.Kind == domain.ErrSCMNotFound {
					branchLog.Debug("auto_merge branch already deleted",
						slog.String("branch", branch),
					)
					return
				}
				branchLog.Warn("auto_merge branch delete failed",
					slog.String("branch", branch),
					slog.Any("error", err),
				)
			}
		})
	}

	if params.TrackerAdapter != nil {
		commentText := buildAutoMergeComment(autoMergeData, mergeResult, params.AutoMergeConfig.Strategy)
		issueID := pending.IssueID
		tracker := params.TrackerAdapter
		commentLog := log

		state.TrackerOpsWg.Go(func() {
			dctx, cancel := context.WithTimeout(
				context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()

			if err := tracker.CommentIssue(dctx, issueID, commentText); err != nil {
				commentLog.Warn("auto_merge tracker comment failed",
					slog.Any("error", err),
				)
			}
		})
	}

	if err := params.Store.DeleteReactionFingerprint(ctx, pending.IssueID, ReactionKindAutoMerge); err != nil {
		log.Warn("auto_merge fingerprint delete failed",
			slog.Any("error", err),
		)
	}
}

// buildAutoMergeComment returns the fixed three-line tracker comment
// template used on a successful auto-merge.
func buildAutoMergeComment(autoMergeData *AutoMergeReactionData, mergeResult domain.MergeResult, strategy domain.MergeStrategy) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Sortie auto-merged PR #%d (%s/%s).\n", autoMergeData.PRNumber, autoMergeData.Owner, autoMergeData.Repo)
	fmt.Fprintf(&b, "Strategy: %s.\n", string(strategy))
	fmt.Fprintf(&b, "Merge commit: %s.", mergeResult.SHA)
	return b.String()
}

// handleMergeError routes a failing MergePR call to the disposition
// documented in the auto-merge error model: idempotent-on-duplicate
// merge as success, transport and API errors back off, auth and
// payload errors escalate immediately, conflict otherwise re-enqueues
// at poll interval.
func handleMergeError(state *State, params ReconcileParams, key string, pending *PendingReaction, autoMergeData *AutoMergeReactionData, err error, now time.Time, pollInterval time.Duration, log *slog.Logger, ctx context.Context, metrics domain.Metrics) {
	var scmErr *domain.SCMError
	if !errors.As(err, &scmErr) {
		delay := applyAutoMergeBackoff(state, key, pending, now, pollInterval)
		log.Error("auto_merge unexpected error",
			slog.Any("error", err),
			slog.Int("pending_attempts", pending.PendingAttempts),
			slog.Int64("retry_after_ms", int64(delay/time.Millisecond)),
		)
		metrics.IncAutoMergeReactions(outcomeError)
		return
	}

	switch scmErr.Kind {
	case domain.ErrSCMConflict:
		lower := strings.ToLower(scmErr.Message)
		if strings.Contains(lower, "already merged") {
			log.Info("auto_merge pr already merged, treating as success",
				slog.Int("pr_number", autoMergeData.PRNumber),
			)
			metrics.IncAutoMergeReactions("merged")
			if markErr := params.Store.MarkReactionDispatched(ctx, pending.IssueID, ReactionKindAutoMerge); markErr != nil {
				log.Warn("failed to mark auto_merge dispatched",
					slog.Any("error", markErr),
				)
			}
			delete(state.PendingReactions, key)
			if delErr := params.Store.DeleteReactionFingerprint(ctx, pending.IssueID, ReactionKindAutoMerge); delErr != nil {
				log.Warn("auto_merge fingerprint delete failed on duplicate merge",
					slog.Any("error", delErr),
				)
			}
			return
		}
		pending.PendingRetryAt = now.Add(pollInterval)
		state.PendingReactions[key] = pending
		log.Warn("auto_merge conflict, retrying after poll interval",
			slog.Any("error", scmErr),
			slog.Int("pr_number", autoMergeData.PRNumber),
		)
		metrics.IncAutoMergeReactions(outcomeError)

	case domain.ErrSCMAuth:
		if _, already := state.AutoMergeAuthLogged[pending.IssueID]; !already {
			log.Error("auto_merge runtime auth failure: token revoked or scope removed",
				slog.Any("error", scmErr),
				slog.Int("pr_number", autoMergeData.PRNumber),
				slog.String("docs_url", autoMergeDocsURL),
			)
			state.AutoMergeAuthLogged[pending.IssueID] = struct{}{}
		}
		escalateAutoMergeFailure(state, params, pending, state.ReactionAttempts[ReactionKey(pending.IssueID, ReactionKindAutoMerge)], autoMergeData, log, ctx, metrics)

	case domain.ErrSCMPayload:
		log.Error("auto_merge payload error, escalating",
			slog.Any("error", scmErr),
			slog.Int("pr_number", autoMergeData.PRNumber),
		)
		escalateAutoMergeFailure(state, params, pending, state.ReactionAttempts[ReactionKey(pending.IssueID, ReactionKindAutoMerge)], autoMergeData, log, ctx, metrics)

	case domain.ErrSCMTransport, domain.ErrSCMAPI:
		delay := applyAutoMergeBackoff(state, key, pending, now, pollInterval)
		log.Warn("auto_merge transient failure, retrying with backoff",
			slog.Any("error", scmErr),
			slog.Int("pending_attempts", pending.PendingAttempts),
			slog.Int64("retry_after_ms", int64(delay/time.Millisecond)),
		)
		metrics.IncAutoMergeReactions(outcomeError)

	default:
		delay := applyAutoMergeBackoff(state, key, pending, now, pollInterval)
		log.Warn("auto_merge unexpected scm error kind, retrying",
			slog.String("kind", string(scmErr.Kind)),
			slog.Any("error", scmErr),
			slog.Int("pending_attempts", pending.PendingAttempts),
			slog.Int64("retry_after_ms", int64(delay/time.Millisecond)),
		)
		metrics.IncAutoMergeReactions(outcomeError)
	}
}

// escalateAutoMergeFailure applies the configured escalation action
// (label or comment) after MaxRetries exhaustion or on a hard failure,
// then removes the merge-kind pending entry and clears the fingerprint.
// MUST NOT touch state.Claimed or any other reaction kind's entries.
func escalateAutoMergeFailure(state *State, params ReconcileParams, pending *PendingReaction, turnCount int, autoMergeData *AutoMergeReactionData, log *slog.Logger, ctx context.Context, metrics domain.Metrics) {
	log.Warn("auto_merge attempts exhausted, escalating",
		slog.Int("turn_count", turnCount),
		slog.Int("max_retries", params.AutoMergeConfig.MaxRetries),
		slog.Int("pr_number", autoMergeData.PRNumber),
	)

	switch params.AutoMergeConfig.Escalation {
	case "label":
		label := params.AutoMergeConfig.EscalationLabel
		if label == "" {
			label = "needs-human"
		}
		if params.TrackerAdapter != nil {
			issueID := pending.IssueID
			tracker := params.TrackerAdapter
			escalLog := log

			state.TrackerOpsWg.Go(func() {
				dctx, cancel := context.WithTimeout(
					context.WithoutCancel(ctx), 30*time.Second)
				defer cancel()

				if err := tracker.AddLabel(dctx, issueID, label); err != nil {
					escalLog.Warn("auto_merge escalation label failed",
						slog.Any("error", err),
					)
				}
			})
		}

	case "comment", "":
		commentText := buildAutoMergeEscalationComment(autoMergeData, turnCount)
		if params.TrackerAdapter != nil {
			issueID := pending.IssueID
			tracker := params.TrackerAdapter
			escalLog := log

			state.TrackerOpsWg.Go(func() {
				dctx, cancel := context.WithTimeout(
					context.WithoutCancel(ctx), 30*time.Second)
				defer cancel()

				if err := tracker.CommentIssue(dctx, issueID, commentText); err != nil {
					escalLog.Warn("auto_merge escalation comment failed",
						slog.Any("error", err),
					)
				}
			})
		}
	}

	metrics.IncAutoMergeReactions("escalated")

	delete(state.PendingReactions, ReactionKey(pending.IssueID, ReactionKindAutoMerge))
	if err := params.Store.DeleteReactionFingerprint(ctx, pending.IssueID, ReactionKindAutoMerge); err != nil {
		log.Warn("auto_merge fingerprint delete failed during escalation",
			slog.Any("error", err),
		)
	}
}

// buildAutoMergeEscalationComment returns the escalation comment used
// when MaxRetries is exhausted.
func buildAutoMergeEscalationComment(autoMergeData *AutoMergeReactionData, turnCount int) string {
	return fmt.Sprintf(
		"Sortie auto_merge attempted %d time(s) and could not complete the merge for PR #%d. Manual merge required.",
		turnCount,
		autoMergeData.PRNumber,
	)
}
