package orchestrator

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
)

// mergeConflictPendingBackoffBase is the floor for the merge-conflict poll
// interval and, through it, for the per-tick deferral and fetch-error
// backoff delays. The exponential backoff itself is computed by
// [computeReactionPendingDelay]; this constant only bounds it from below.
const mergeConflictPendingBackoffBase = 10 * time.Second

// mergeConflictPendingDefaultTTL is the default lifetime of a
// merge-conflict PendingReaction entry. Entries older than this are
// dropped on the next reconcile tick.
const mergeConflictPendingDefaultTTL = 30 * time.Minute

// reconcileMergeConflicts polls mergeability for each merge-conflict-kind
// entry in state.PendingReactions and dispatches a rebase-and-resolve
// continuation on each no-conflict-to-conflict transition. Called from
// [ReconcileRunningIssues] after [reconcileBotReviewComments] and before
// [reconcileAutoMerge]. Skipped entirely when params.SCMAdapter is nil or
// params.MergeConflictReactionConfigured is false.
//
// The state machine mirrors [reconcileCIStatus]: the mergeability read
// runs on every due tick with no retry-budget check before it, so the
// not-dirty branch (which resets the per-episode attempt counter) is
// always reachable. The per-episode counter is incremented only inside the
// dirty branch on a new conflicting head, and the cap is strict
// over-limit, matching [handleCIFailure].
func reconcileMergeConflicts(state *State, params ReconcileParams, log *slog.Logger, ctx context.Context, metrics domain.Metrics) {
	if params.SCMAdapter == nil {
		return
	}
	if !params.MergeConflictReactionConfigured {
		return
	}

	now := time.Now().UTC()
	if params.NowFunc != nil {
		now = params.NowFunc().UTC()
	}

	ttl := params.MergeConflictPendingTTL
	pollInterval := max(time.Duration(params.MergeConflictConfig.PollIntervalMS)*time.Millisecond, mergeConflictPendingBackoffBase)

	for key, pending := range state.PendingReactions {
		if pending.Kind != ReactionKindMergeConflict {
			continue
		}
		delete(state.PendingReactions, key)

		data, ok := pending.KindData.(*MergeConflictReactionData)
		if !ok {
			log.ErrorContext(ctx, "unexpected KindData type for merge-conflict reaction",
				slog.String("issue_id", pending.IssueID),
				slog.String("type", fmt.Sprintf("%T", pending.KindData)),
			)
			continue
		}

		entryLog := logging.WithIssue(log, pending.IssueID, pending.Identifier)

		if ttl > 0 && now.Sub(pending.CreatedAt) > ttl {
			delete(state.ReactionAttempts, ReactionKey(pending.IssueID, ReactionKindMergeConflict))
			entryLog.Warn("merge conflict pending entry exceeded ttl, dropping",
				slog.Int64("ttl_ms", int64(ttl/time.Millisecond)),
				slog.Int64("age_ms", int64(now.Sub(pending.CreatedAt)/time.Millisecond)),
			)
			continue
		}

		if now.Before(pending.PendingRetryAt) {
			state.PendingReactions[key] = pending
			continue
		}

		// The mergeability read runs every due tick with no cap before
		// it, so the not-dirty branch stays reachable and a resolved
		// conflict resets the counter rather than escalating.
		status, err := params.SCMAdapter.GetMergeability(ctx, data.PRNumber, data.Owner, data.Repo)
		if err != nil {
			pending.PendingAttempts++
			delay := max(computeReactionPendingDelay(pending.PendingAttempts), pollInterval)
			pending.PendingRetryAt = now.Add(delay)
			state.PendingReactions[key] = pending
			entryLog.Warn("merge conflict mergeability fetch failed, retrying with backoff",
				slog.Any("error", err),
				slog.Int("pending_attempts", pending.PendingAttempts),
				slog.Int64("retry_after_ms", int64(delay/time.Millisecond)),
			)
			metrics.IncMergeConflictChecks("error")
			continue
		}

		rkey := ReactionKey(pending.IssueID, ReactionKindMergeConflict)

		switch status.Mergeability {
		case domain.MergeabilityUnknown:
			pending.PendingRetryAt = now.Add(pollInterval)
			state.PendingReactions[key] = pending
			metrics.IncMergeConflictChecks("unknown")
			entryLog.Debug("merge conflict deferred: mergeability unknown",
				slog.Int("pr_number", data.PRNumber),
			)

		case domain.MergeabilityDirty:
			handleMergeConflictDirty(state, params, key, pending, data, status, rkey, now, pollInterval, entryLog, ctx, metrics)

		default:
			// Clean, unstable, or blocked: the episode closes. Clear the
			// dedup fingerprint, reset the per-episode counter, and
			// re-enqueue. Mirrors the CIStatusPassing branch.
			if delErr := params.Store.DeleteReactionFingerprint(ctx, pending.IssueID, ReactionKindMergeConflict); delErr != nil {
				entryLog.Warn("failed to delete merge conflict reaction fingerprint",
					slog.Any("error", delErr),
				)
			}
			delete(state.ReactionAttempts, rkey)
			pending.PendingRetryAt = now.Add(pollInterval)
			state.PendingReactions[key] = pending
			metrics.IncMergeConflictChecks("clear")
			entryLog.Debug("merge conflict not present, tracking reset",
				slog.Int("pr_number", data.PRNumber),
				slog.String("state", string(status.Mergeability)),
			)
		}
	}
}

// handleMergeConflictDirty processes a PR that the provider reports as
// conflicted. It applies the precondition guards (empty head SHA, empty
// base branch), deduplicates on the head SHA, increments the per-episode
// attempt counter, and either dispatches a rebase continuation or
// escalates when the new conflicting head pushes the episode strictly over
// the retry budget. The increment-then-strict-over-limit ordering mirrors
// [handleCIFailure].
func handleMergeConflictDirty(
	state *State,
	params ReconcileParams,
	key string,
	pending *PendingReaction,
	data *MergeConflictReactionData,
	status domain.PRMergeStatus,
	rkey string,
	now time.Time,
	pollInterval time.Duration,
	log *slog.Logger,
	ctx context.Context,
	metrics domain.Metrics,
) {
	// An empty head provides no rebase anchor. Defer without burning an
	// attempt: a deferral is not a resolution attempt.
	if status.HeadSHA == "" {
		pending.PendingRetryAt = now.Add(pollInterval)
		state.PendingReactions[key] = pending
		log.Debug("merge conflict deferred: empty head sha, no rebase anchor",
			slog.Int("pr_number", data.PRNumber),
		)
		return
	}

	// An empty base provides no rebase target. This is defense-in-depth,
	// symmetric to the empty-head guard: the agent is never handed a
	// rebase against an empty target. GitHub always populates base.ref, so
	// this guard rarely fires for the only wired provider. It runs before
	// the increment for the same reason: a deferral must not burn an
	// attempt.
	if status.BaseBranch == "" {
		pending.PendingRetryAt = now.Add(pollInterval)
		state.PendingReactions[key] = pending
		log.Debug("merge conflict deferred: empty base branch, no rebase target",
			slog.Int("pr_number", data.PRNumber),
		)
		return
	}

	fingerprint := buildMergeConflictFingerprint(status.HeadSHA)
	if err := params.Store.UpsertReactionFingerprint(ctx, pending.IssueID, ReactionKindMergeConflict, fingerprint); err != nil {
		log.Warn("failed to upsert merge conflict reaction fingerprint",
			slog.Any("error", err),
		)
	}

	// Head-SHA dedup before the increment so a same-head re-observation
	// never re-increments the per-episode counter.
	storedFP, dispatched, fpErr := params.Store.GetReactionFingerprint(ctx, pending.IssueID, ReactionKindMergeConflict)
	if fpErr != nil {
		log.Warn("failed to get merge conflict reaction fingerprint, proceeding without dedup",
			slog.Any("error", fpErr),
		)
	} else if storedFP == fingerprint && dispatched {
		pending.PendingRetryAt = now.Add(pollInterval)
		state.PendingReactions[key] = pending
		log.Debug("merge conflict already dispatched for this head, skipping",
			slog.Int("pr_number", data.PRNumber),
		)
		return
	}

	// Per-episode increment for this new conflicting head, then the strict
	// over-limit cap. The increment happens before the cap so the new
	// conflicting head is what pushes the episode over budget, and the
	// escalation reports attempts == MaxRetries + 1.
	state.ReactionAttempts[rkey]++
	attempts := state.ReactionAttempts[rkey]
	if attempts > params.MergeConflictConfig.MaxRetries {
		escalateMergeConflictFailure(state, params, pending, attempts, data, log, ctx, metrics)
		return
	}

	dispatchMergeConflictContinuation(state, params, pending, data, status, log, ctx)
	metrics.IncMergeConflictChecks("dispatched")
}

// buildMergeConflictFingerprint computes the SHA-256 hex digest of the PR
// head SHA. An empty head SHA returns the empty string; the dirty branch
// guards the empty case before reaching this helper, so the empty-string
// return is a backstop rather than a relied-on path.
func buildMergeConflictFingerprint(headSHA string) string {
	if headSHA == "" {
		return ""
	}
	h := sha256.Sum256([]byte(headSHA))
	return fmt.Sprintf("%x", h)
}

// dispatchMergeConflictContinuation schedules a rebase-and-resolve
// continuation turn carrying the PR's real base branch and marks the
// merge-conflict fingerprint dispatched on the same tick. The continuation
// reuses the same workspace identifier (no new workspace key).
func dispatchMergeConflictContinuation(
	state *State,
	params ReconcileParams,
	pending *PendingReaction,
	data *MergeConflictReactionData,
	status domain.PRMergeStatus,
	log *slog.Logger,
	ctx context.Context,
) {
	mergeContext := buildMergeConflictTemplateMap(data, status)

	CancelRetry(state, pending.IssueID)

	ScheduleRetry(state, ScheduleRetryParams{
		IssueID:     pending.IssueID,
		Identifier:  pending.Identifier,
		DisplayID:   pending.DisplayID,
		Attempt:     pending.Attempt,
		DelayMS:     continuationDelayMS,
		LastSSHHost: pending.LastSSHHost,
		ContinuationContext: map[string]any{
			"merge_conflict": mergeContext,
		},
		ReactionKind: ReactionKindMergeConflict,
		AgentKind:    pending.AgentKind,
		RuleName:     pending.RuleName,
		TemplateID:   pending.TemplateID,
	}, params.OnRetryFire)

	if markErr := params.Store.MarkReactionDispatched(ctx, pending.IssueID, ReactionKindMergeConflict); markErr != nil {
		log.Warn("failed to mark merge conflict dispatched",
			slog.Any("error", markErr),
		)
	}

	log.Info("merge conflict detected, scheduling rebase dispatch",
		slog.Int("pr_number", data.PRNumber),
		slog.String("base", status.BaseBranch),
		slog.Int("merge_conflict_attempt", state.ReactionAttempts[ReactionKey(pending.IssueID, ReactionKindMergeConflict)]),
		slog.Int("max_retries", params.MergeConflictConfig.MaxRetries),
	)
}

// buildMergeConflictTemplateMap builds the continuation template context
// injected under the key "merge_conflict". The base value is the PR
// object's real base ref read live from GetMergeability on this tick (the
// rebase target), threaded so the agent rebases onto the PR's current
// target rather than a value snapshotted at enqueue time. The base key is
// always present so templates can reference .merge_conflict.base under
// missingkey=error.
func buildMergeConflictTemplateMap(data *MergeConflictReactionData, status domain.PRMergeStatus) map[string]any {
	return map[string]any{
		"pr_number": data.PRNumber,
		"branch":    data.Branch,
		"head_sha":  status.HeadSHA,
		"base":      status.BaseBranch,
	}
}

// escalateMergeConflictFailure applies the configured escalation action
// (label or comment) after the per-episode retry budget is exhausted, then
// removes the merge-conflict slot, fingerprint, and per-episode attempt
// counter. The attempts value is captured at the call boundary, so the
// scoped counter delete below cannot disturb the reported count.
//
// Unlike the bot-review and auto-merge escalations, this resets the
// per-episode attempt counter via a scoped delete because the
// merge-conflict counter is episodic, mirroring the CI reaction. The reset
// is scoped to this kind's own slot, so sibling reactions' counters and
// state.Claimed are never touched.
func escalateMergeConflictFailure(
	state *State,
	params ReconcileParams,
	pending *PendingReaction,
	attempts int,
	data *MergeConflictReactionData,
	log *slog.Logger,
	ctx context.Context,
	metrics domain.Metrics,
) {
	log.Warn("merge conflict resolution attempts exhausted, escalating",
		slog.Int("attempts", attempts),
		slog.Int("max_retries", params.MergeConflictConfig.MaxRetries),
		slog.Int("pr_number", data.PRNumber),
	)

	switch params.MergeConflictConfig.Escalation {
	case "label":
		label := params.MergeConflictConfig.EscalationLabel
		if label == "" {
			label = "needs-human"
		}
		if params.TrackerAdapter != nil {
			issueID := pending.IssueID
			tracker := params.TrackerAdapter
			m := metrics
			escalLog := log

			state.TrackerOpsWg.Go(func() {
				dctx, cancel := context.WithTimeout(
					context.WithoutCancel(ctx), 30*time.Second)
				defer cancel()

				if err := tracker.AddLabel(dctx, issueID, label); err != nil {
					escalLog.Warn("merge conflict escalation label failed",
						slog.Any("error", err),
					)
					m.IncMergeConflictEscalations("error")
				} else {
					m.IncMergeConflictEscalations("label")
				}
			})
		}

	case "comment", "":
		commentText := buildMergeConflictEscalationComment(data, attempts)
		if params.TrackerAdapter != nil {
			issueID := pending.IssueID
			tracker := params.TrackerAdapter
			m := metrics
			escalLog := log
			ct := commentText

			state.TrackerOpsWg.Go(func() {
				dctx, cancel := context.WithTimeout(
					context.WithoutCancel(ctx), 30*time.Second)
				defer cancel()

				if err := tracker.CommentIssue(dctx, issueID, ct); err != nil {
					escalLog.Warn("merge conflict escalation comment failed",
						slog.Any("error", err),
					)
					m.IncMergeConflictEscalations("error")
				} else {
					m.IncMergeConflictEscalations("comment")
				}
			})
		}
	}

	delete(state.PendingReactions, ReactionKey(pending.IssueID, ReactionKindMergeConflict))
	if err := params.Store.DeleteReactionFingerprint(ctx, pending.IssueID, ReactionKindMergeConflict); err != nil {
		log.Warn("merge conflict fingerprint delete failed during escalation",
			slog.Any("error", err),
		)
	}
	delete(state.ReactionAttempts, ReactionKey(pending.IssueID, ReactionKindMergeConflict))
}

// buildMergeConflictEscalationComment returns the plain-text escalation
// comment used when the per-episode retry budget is exhausted. Plain text
// is used so the comment renders consistently across all tracker adapters.
func buildMergeConflictEscalationComment(data *MergeConflictReactionData, attempts int) string {
	// The observation that trips the retry cap dispatches no continuation, so
	// the number of resolution turns actually dispatched is attempts-1. At
	// max_retries 0 this is 0: the reaction escalates on first detection
	// without attempting a rebase.
	dispatched := attempts - 1
	return fmt.Sprintf(
		"Sortie attempted %d merge-conflict resolution turn(s) and could not clear the conflicts for PR #%d. Manual rebase required.",
		dispatched,
		data.PRNumber,
	)
}
