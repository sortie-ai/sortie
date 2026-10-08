package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
)

// reconcileBotReviewComments polls bot-authored review comments for each
// bot-review-kind entry in state.PendingReactions. Called from
// [ReconcileRunningIssues] after [reconcileReviewComments] and before
// [reconcileAutoMerge]. Skipped entirely when params.SCMAdapter is nil
// or params.BotReviewConfigured is false.
//
// Unlike [reconcileReviewComments], there is no debounce gate: bot
// comments arrive in bulk on push and dispatch on the same tick they are
// detected.
func reconcileBotReviewComments(state *State, params ReconcileParams, log *slog.Logger, ctx context.Context, metrics domain.Metrics) {
	if params.SCMAdapter == nil {
		return
	}
	if !params.BotReviewConfigured {
		return
	}

	now := time.Now().UTC()
	if params.NowFunc != nil {
		now = params.NowFunc().UTC()
	}

	ttl := params.BotReviewPendingTTL
	pollInterval := time.Duration(params.BotReviewConfig.PollIntervalMS) * time.Millisecond
	if pollInterval <= 0 {
		pollInterval = reviewPendingBackoffBase
	}

	for key, pending := range state.PendingReactions {
		if pending.Kind != ReactionKindBotReview {
			continue
		}
		delete(state.PendingReactions, key)

		botReviewData, ok := pending.KindData.(*BotReviewReactionData)
		if !ok {
			cancelReactionTriage(pending)
			log.ErrorContext(ctx, "unexpected KindData type for bot-review reaction",
				slog.String("issue_id", pending.IssueID),
				slog.String("type", fmt.Sprintf("%T", pending.KindData)),
			)
			continue
		}

		entryLog := logging.WithIssue(log, pending.IssueID, pending.Identifier)
		rkey := ReactionKey(pending.IssueID, ReactionKindBotReview)

		// TTL enforcement.
		if ttl > 0 && now.Sub(pending.CreatedAt) > ttl {
			cancelReactionTriage(pending)
			// A spent counter outlives the drop, as it outlives a budget
			// escalation, so the next seeded entry cannot reopen the
			// budget or re-report comments already reported. The
			// handed-off cache key goes at any counter value because the
			// stored rows are the record and the next check reloads them.
			if state.ReactionAttempts[rkey] < params.BotReviewConfig.MaxContinuationTurns {
				delete(state.ReactionAttempts, rkey)
				delete(state.ReactionReportedComments, rkey)
			}
			delete(state.ReactionHandedOffComments, rkey)
			entryLog.Warn("bot review watch window elapsed, dropping",
				slog.Int64("window_ms", int64(ttl/time.Millisecond)),
				slog.Int64("age_ms", int64(now.Sub(pending.CreatedAt)/time.Millisecond)),
			)
			continue
		}

		// Poll throttle: respect PendingRetryAt.
		if now.Before(pending.PendingRetryAt) {
			state.PendingReactions[key] = pending
			continue
		}

		// A run in flight makes this pass's fetch redundant, so the
		// entry waits one tick instead. The backoff counter is
		// untouched: waiting is not a fetch error.
		if pending.Triage != nil && !triageRunFinished(pending.Triage) {
			pending.PendingRetryAt = now
			state.PendingReactions[key] = pending
			continue
		}

		// Fetch bot review comments from SCM.
		comments, err := params.SCMAdapter.FetchBotReviewComments(ctx, botReviewData.PRNumber, botReviewData.Owner, botReviewData.Repo, params.BotReviewConfig.BotUsernames)
		if err != nil {
			pending.PendingAttempts++
			delay := max(computeReactionPendingDelay(pending.PendingAttempts), pollInterval)
			pending.PendingRetryAt = now.Add(delay)
			state.PendingReactions[key] = pending
			entryLog.Warn("bot review fetch failed, retrying with backoff",
				slog.Any("error", err),
				slog.Int("pending_attempts", pending.PendingAttempts),
				slog.Int64("retry_after_ms", int64(delay/time.Millisecond)),
			)
			metrics.IncBotReviewChecks("error")
			continue
		}

		actionable := actionableComments(ReactionKindBotReview, comments, nil)

		// No actionable comments; re-enqueue with poll interval delay.
		if len(actionable) == 0 {
			// The episode closes while the entry keeps watching, so the
			// comment set a run answered for no longer describes
			// anything. A retained verdict would otherwise be replayed
			// against the next episode that recomputes the same set.
			cancelReactionTriage(pending)
			pending.PendingRetryAt = now.Add(pollInterval)
			state.PendingReactions[key] = pending
			continue
		}

		// Build fingerprint from sorted actionable comment IDs.
		fingerprint := buildReviewFingerprint(actionable)

		// Dedup check via reaction_fingerprints table.
		if err := params.Store.UpsertReactionFingerprint(ctx, pending.IssueID, ReactionKindBotReview, fingerprint); err != nil {
			entryLog.Warn("failed to upsert bot review reaction fingerprint",
				slog.Any("error", err),
			)
		}
		storedFP, dispatched, fpErr := params.Store.GetReactionFingerprint(ctx, pending.IssueID, ReactionKindBotReview)
		if fpErr != nil {
			entryLog.Warn("failed to get bot review reaction fingerprint, proceeding without dedup",
				slog.Any("error", fpErr),
			)
		} else if storedFP == fingerprint && dispatched {
			// Already dispatched for this exact set of comments.
			pending.PendingRetryAt = now.Add(pollInterval)
			state.PendingReactions[key] = pending
			entryLog.Debug("bot review comments already dispatched for this fingerprint")
			continue
		}

		if settleHandedOffCommentSet(ctx, state, params.Store, pending.IssueID, ReactionKindBotReview, actionable, entryLog) {
			pending.PendingRetryAt = now.Add(pollInterval)
			state.PendingReactions[key] = pending
			continue
		}

		// Arbitrate the retry slot before dispatching.
		if incumbent := retrySlotIncumbent(state, pending.IssueID); incumbent != nil {
			logRetrySlotDeferral(entryLog, ReactionKindBotReview, incumbent)
			pending.CreatedAt = now
			state.PendingReactions[key] = pending
			continue
		}

		// A summary-only item never escalates, because a bot re-review of
		// the last turn's push adds one every time and its body cannot be
		// read for findings.
		turnCount := state.ReactionAttempts[rkey]
		if turnCount >= params.BotReviewConfig.MaxContinuationTurns {
			var inline []domain.ReviewComment
			for _, c := range actionable {
				if c.FilePath != "" {
					inline = append(inline, c)
				}
			}
			escalating := carriesNewComment(state, pending.IssueID, ReactionKindBotReview, inline)
			recordReportedComments(state, pending.IssueID, ReactionKindBotReview, actionable)
			if escalating {
				escalateBotReviewFailure(state, params, pending, turnCount, EscalationTriggerBudget, botReviewData, entryLog, ctx, metrics)
				continue
			}
			pending.PendingRetryAt = now.Add(pollInterval)
			state.PendingReactions[key] = pending
			entryLog.Debug("bot review continuation turns exhausted, no new inline comment, not escalating",
				slog.Int("turn_count", turnCount),
				slog.Int("max_continuation_turns", params.BotReviewConfig.MaxContinuationTurns),
				slog.Int("comment_count", len(actionable)),
			)
			continue
		}

		botContext := buildReviewTemplateMap(actionable)

		// The gate sits above the dispatch counter so no pass that
		// dispatches nothing is counted as one.
		triageVerdict := reactionTriageGate(state, params, pending, params.BotReviewConfig.Triage, ReactionTriageRequest{
			Kind:          ReactionKindBotReview,
			WorkspaceRoot: params.WorkspaceRoot,
			IssueID:       pending.IssueID,
			Identifier:    pending.Identifier,
			DisplayID:     pending.DisplayID,
			Attempt:       pending.Attempt,
			SSHHost:       pending.LastSSHHost,
			Fingerprint:   fingerprint,
			AttemptsUsed:  turnCount,
			MaxAttempts:   params.BotReviewConfig.MaxContinuationTurns,
			Subject:       botContext,
		}, entryLog, ctx)

		switch triageVerdict {
		case triageWait, triageHandled:
			pending.PendingRetryAt = now.Add(pollInterval)
			state.PendingReactions[key] = pending
			continue
		case triageEscalate:
			recordReportedComments(state, pending.IssueID, ReactionKindBotReview, actionable)
			escalateBotReviewFailure(state, params, pending, turnCount, EscalationTriggerTriage, botReviewData, entryLog, ctx, metrics)
			continue
		}

		// Dispatch immediately: bot comments are not debounced.
		metrics.IncBotReviewChecks("dispatched")

		ScheduleRetry(state, ScheduleRetryParams{
			IssueID:     pending.IssueID,
			Identifier:  pending.Identifier,
			DisplayID:   pending.DisplayID,
			Attempt:     pending.Attempt,
			DelayMS:     continuationDelayMS,
			LastSSHHost: pending.LastSSHHost,
			ContinuationContext: map[string]any{
				botReviewCommentsKey: botContext,
			},
			ReactionKind:        ReactionKindBotReview,
			AgentKind:           pending.AgentKind,
			RuleName:            pending.RuleName,
			StagePrevious:       pending.StagePrevious,
			RuleSettingsApplied: pending.RuleSettingsApplied,
			TemplateID:          pending.TemplateID,
			Logger:              entryLog,
		}, params.OnRetryFire)

		state.ReactionAttempts[rkey]++

		entryLog.Info("bot review comments detected, scheduling bot-review-fix dispatch",
			slog.Int("comment_count", len(actionable)),
			slog.Int("bot_review_fix_attempt", state.ReactionAttempts[rkey]),
			slog.Int("max_continuation_turns", params.BotReviewConfig.MaxContinuationTurns),
		)
	}
}

// escalateBotReviewFailure handles the case where bot review continuation
// turns are exhausted. It applies the configured escalation action, then
// removes only the bot-review-kind pending entry and clears the
// bot-review fingerprint. MUST NOT touch state.Claimed, the retry timer,
// the persisted retry row, the residual ReactionAttempts counter, or any
// other reaction kind's entries; those are owned by whichever kind holds
// the claim and are released at terminal-state cleanup.
func escalateBotReviewFailure(
	state *State,
	params ReconcileParams,
	pending *PendingReaction,
	turnCount int,
	trigger ReactionEscalationTrigger,
	botReviewData *BotReviewReactionData,
	log *slog.Logger,
	ctx context.Context,
	metrics domain.Metrics,
) {
	if trigger == EscalationTriggerTriage {
		log.Warn("bot review triage requested escalation",
			slog.Int("turn_count", turnCount),
			slog.Int("max_continuation_turns", params.BotReviewConfig.MaxContinuationTurns),
			slog.Int("pr_number", botReviewData.PRNumber),
		)
	} else {
		log.Warn("bot review continuation turns exhausted, escalating",
			slog.Int("turn_count", turnCount),
			slog.Int("max_continuation_turns", params.BotReviewConfig.MaxContinuationTurns),
			slog.Int("pr_number", botReviewData.PRNumber),
		)
	}

	commentText := buildBotReviewEscalationComment(botReviewData, turnCount)
	if trigger == EscalationTriggerTriage {
		commentText = buildTriageEscalationComment("bot-review", fmt.Sprintf("PR #%d", botReviewData.PRNumber))
	}
	delivery := params.Router.Route(reactionNotification(domain.EventEscalationBotReview, pending, commentText))

	if params.BotReviewConfig.Escalation == "label" {
		label := params.BotReviewConfig.EscalationLabel
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
					escalLog.Warn("bot review escalation label failed",
						slog.Any("error", err),
					)
					m.IncBotReviewEscalations("error")
				} else {
					m.IncBotReviewEscalations("label")
				}
			})
		}
	}

	// A label owns the escalation counter, so only the other modes record it.
	var record func(action string)
	if params.BotReviewConfig.Escalation != "label" {
		record = metrics.IncBotReviewEscalations
	}
	publishEscalation(ctx, &state.TrackerOpsWg, delivery, log, record, func(err error) {
		log.Warn("bot review escalation comment failed", slog.Any("error", err))
	})

	delete(state.PendingReactions, ReactionKey(pending.IssueID, ReactionKindBotReview))
	if err := params.Store.DeleteReactionFingerprint(ctx, pending.IssueID, ReactionKindBotReview); err != nil {
		log.Warn("bot review fingerprint delete failed during escalation",
			slog.Any("error", err),
		)
	}
}

// buildBotReviewEscalationComment returns the escalation comment used when
// the bot review continuation turn budget is exhausted.
func buildBotReviewEscalationComment(botReviewData *BotReviewReactionData, turnCount int) string {
	return fmt.Sprintf(
		"Sortie attempted %d bot-review continuation turn(s) and could not resolve the automated review comments for PR #%d. Remaining bot review comments require human attention.",
		turnCount,
		botReviewData.PRNumber,
	)
}
