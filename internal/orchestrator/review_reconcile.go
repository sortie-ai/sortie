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
	"github.com/sortie-ai/sortie/internal/scm/scmcore"
	"github.com/sortie-ai/sortie/internal/storage"
	"github.com/sortie-ai/sortie/internal/workspace"
)

// reviewPendingBackoffBase is the base interval for review-pending
// exponential backoff.
const reviewPendingBackoffBase = 10 * time.Second

// reviewCommentsKey and botReviewCommentsKey are the prompt variables that
// carry the comments of the review and bot-review reactions.
const (
	reviewCommentsKey    = "review_comments"
	botReviewCommentsKey = "bot_review_comments"
)

// reviewPendingBackoffCap is the maximum interval between review fetch
// retries.
const reviewPendingBackoffCap = 5 * time.Minute

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
	if pollInterval <= 0 {
		pollInterval = reviewPendingBackoffBase
	}
	debounceDuration := time.Duration(params.ReviewConfig.DebounceMS) * time.Millisecond

	for key, pending := range state.PendingReactions {
		if pending.Kind != ReactionKindReview {
			continue
		}
		delete(state.PendingReactions, key)

		reviewData, ok := pending.KindData.(*ReviewReactionData)
		if !ok {
			cancelReactionTriage(pending)
			log.ErrorContext(ctx, "unexpected KindData type for review reaction",
				slog.String("issue_id", pending.IssueID),
				slog.String("type", fmt.Sprintf("%T", pending.KindData)),
			)
			continue
		}

		entryLog := logging.WithIssue(log, pending.IssueID, pending.Identifier)
		rkey := ReactionKey(pending.IssueID, ReactionKindReview)

		// A standing escalation is never aged out; its own facts end it.
		windowElapsed := ttl > 0 && !pending.escalationStands() && now.Sub(watchWindowStart(pending)) > ttl

		dropWatch := func() {
			cancelReactionTriage(pending)
			// A spent counter outlives the drop: an entry re-seeded by
			// another kind's worker exit would otherwise open a fresh
			// budget over comments every turn already carried. The
			// handed-off cache key goes at any counter value because the
			// stored rows are the record and the next check reloads them.
			if state.ReactionAttempts[rkey] < params.ReviewConfig.MaxContinuationTurns {
				delete(state.ReactionAttempts, rkey)
			}
			delete(state.ReactionHandedOffComments, rkey)
		}
		dropWindowElapsed := func() {
			dropWatch()
			entryLog.Warn("review watch window elapsed, dropping",
				slog.Int64("window_ms", int64(ttl/time.Millisecond)),
				slog.Int64("age_ms", int64(now.Sub(watchWindowStart(pending))/time.Millisecond)),
			)
		}
		dropEnded := func() {
			dropWatch()
			entryLog.Info("pull request awaiting approval ended, dropping",
				slog.Int("pr_number", reviewData.PRNumber),
			)
		}

		// A watch waiting on an approval outlives its window: the person who
		// approves takes longer than the window, and the pull request read
		// below ends the wait when the pull request ends.
		if windowElapsed && !awaits(pending.LastCheck) {
			dropWindowElapsed()
			continue
		}

		// Poll throttle: respect PendingRetryAt.
		if reactionWaits(pending, now) {
			state.PendingReactions[key] = pending
			continue
		}
		retryEscalationSave(ctx, state, params.Store, pending, entryLog)

		// A run in flight makes this pass's fetch redundant, so the
		// entry waits one tick instead. The backoff counter is
		// untouched: waiting is not a fetch error.
		if pending.Triage != nil && !triageRunFinished(pending.Triage) {
			pending.PendingRetryAt = now
			requeueReaction(state, key, pending, waitTriage)
			continue
		}

		// The decision arm below reuses this pass's pull request read so a
		// pass reads the pull request at most once.
		var read pullRequestRead
		switch {
		case pending.escalationStands():
			if watchStandingPullRequest(ctx, state, params.Store, params.SCMAdapter, key, pending, true, now, pollInterval, entryLog) {
				continue
			}
			read.made = true
		case awaits(pending.LastCheck):
			read = readPullRequest(ctx, params.SCMAdapter, pending, entryLog)
			if pullRequestEnded(read.status, read.err) {
				dropEnded()
				continue
			}
		default:
			read = readPullRequestURL(ctx, params.SCMAdapter, pending, entryLog)
		}

		// Fetch reviews from SCM.
		comments, err := params.SCMAdapter.FetchPendingReviews(ctx, reviewData.PRNumber, reviewData.Owner, reviewData.Repo)
		if err != nil {
			pending.PendingAttempts++
			delay := max(computeReactionPendingDelay(pending.PendingAttempts), pollInterval)
			pending.PendingRetryAt = now.Add(delay)
			requeueReaction(state, key, pending, waitBackoff)
			entryLog.Warn("review fetch failed, retrying with backoff",
				slog.Any("error", err),
				slog.Int("pending_attempts", pending.PendingAttempts),
				slog.Int64("retry_after_ms", int64(delay/time.Millisecond)),
			)
			metrics.IncReviewChecks("error")
			continue
		}

		actionable := actionableComments(ReactionKindReview, comments, params.BotReviewConfig.BotUsernames)
		excluded := countCurrentComments(comments) - len(actionable)
		if excluded > 0 {
			entryLog.Debug("review comments excluded by bot allowlist",
				slog.Int("excluded_count", excluded),
			)
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

		// No actionable comments; re-enqueue with poll interval delay.
		if len(actionable) == 0 {
			// The episode closes while the entry keeps watching, so the
			// comment set a run answered for no longer describes
			// anything. A retained verdict would otherwise be replayed
			// against the next episode that recomputes the same set.
			cancelReactionTriage(pending)
			endEscalationAndCounter(ctx, state, params.Store, pending, entryLog)

			awaiting := awaits(pending.LastCheck)
			decision, decisionErr := params.SCMAdapter.GetReviewDecision(ctx, reviewData.PRNumber, reviewData.Owner, reviewData.Repo)
			if decisionErr != nil {
				entryLog.WarnContext(ctx, "review decision fetch failed",
					slog.Any("error", decisionErr),
					slog.Int("pr_number", reviewData.PRNumber),
				)
			} else {
				awaiting = decision == domain.ReviewDecisionReviewRequired
			}

			// A reading written without a pull request read of this pass
			// would show a merged pull request as waiting on approval.
			if awaiting && !awaits(pending.LastCheck) {
				if !read.made {
					read = readPullRequest(ctx, params.SCMAdapter, pending, entryLog)
				}
				if pullRequestEnded(read.status, read.err) {
					dropEnded()
					continue
				}
				if read.err != nil {
					awaiting = false
				}
			}

			if windowElapsed && !awaiting {
				dropWindowElapsed()
				continue
			}

			pending.PendingRetryAt = now.Add(pollInterval)
			pending.LastCheck = commentReactionCheck(now, 0, 0)
			pending.LastCheck.Comments.AwaitingApproval = awaiting
			requeueReaction(state, key, pending, waitPollInterval)
			continue
		}

		if windowElapsed {
			dropWindowElapsed()
			continue
		}

		// Build fingerprint from sorted actionable comment IDs.
		fingerprint := buildReviewFingerprint(actionable)

		if pending.escalationStands() {
			spent := state.ReactionAttempts[rkey] >= params.ReviewConfig.MaxContinuationTurns
			switch judgeCommentEscalation(ctx, state, params.Store, pending, actionable, fingerprint, spent, entryLog) {
			case commentEscalationUnread:
				pending.PendingRetryAt = now.Add(pollInterval)
				requeueReaction(state, key, pending, waitBackoff)
				continue
			case commentEscalationStands:
				pending.PendingRetryAt = now.Add(pollInterval)
				pending.LastCheck = commentReactionCheck(now, len(actionable), 0)
				requeueReaction(state, key, pending, waitPollInterval)
				continue
			}
			// The counter stays only while the escalation stands.
			delete(state.ReactionAttempts, rkey)
		}

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
			pending.LastCheck = commentReactionCheck(now, len(actionable), 0)
			requeueReaction(state, key, pending, waitPollInterval)
			entryLog.Debug("review comments already dispatched for this fingerprint")
			continue
		}

		switch settleHandedOffCommentSet(ctx, state, params.Store, pending.IssueID, ReactionKindReview, actionable, entryLog) {
		case settleLoadFailed:
			pending.PendingRetryAt = now.Add(pollInterval)
			requeueReaction(state, key, pending, waitBackoff)
			continue
		case settleSettled:
			pending.PendingRetryAt = now.Add(pollInterval)
			pending.LastCheck = commentReactionCheck(now, len(actionable), 0)
			requeueReaction(state, key, pending, waitPollInterval)
			continue
		}

		pending.LastCheck = commentReactionCheck(now, len(actionable),
			newCommentCount(state, pending.IssueID, ReactionKindReview, actionable))

		// Debounce: if LastEventAt is recent, defer dispatch.
		if !reviewData.LastEventAt.IsZero() && now.Sub(reviewData.LastEventAt) < debounceDuration {
			pending.PendingRetryAt = reviewData.LastEventAt.Add(debounceDuration)
			requeueReaction(state, key, pending, waitDebounce)
			entryLog.Debug("review comments within debounce window, deferring")
			continue
		}

		// Arbitrate the retry slot before dispatching.
		if incumbent := retrySlotIncumbent(state, pending.IssueID); incumbent != nil {
			logRetrySlotDeferral(entryLog, ReactionKindReview, incumbent)
			pending.WatchRestartedAt = now
			requeueReaction(state, key, pending, waitRetrySlot)
			continue
		}

		// A set that reaches this point holds a comment no run was given,
		// so a spent budget escalates it.
		turnCount := state.ReactionAttempts[rkey]
		if turnCount >= params.ReviewConfig.MaxContinuationTurns {
			escalateReviewFailure(state, params, pending, turnCount, EscalationTriggerBudget, reviewData, fingerprint, now, pollInterval, entryLog, ctx, metrics)
			continue
		}

		reviewContext := buildReviewTemplateMap(actionable)

		// The gate sits above the dispatch counter so no pass that
		// dispatches nothing is counted as one.
		triageVerdict := reactionTriageGate(state, params, pending, params.ReviewConfig.Triage, ReactionTriageRequest{
			Kind:          ReactionKindReview,
			WorkspaceRoot: params.WorkspaceRoot,
			IssueID:       pending.IssueID,
			Identifier:    pending.Identifier,
			DisplayID:     pending.DisplayID,
			Attempt:       pending.Attempt,
			SSHHost:       pending.LastSSHHost,
			Fingerprint:   fingerprint,
			AttemptsUsed:  turnCount,
			MaxAttempts:   params.ReviewConfig.MaxContinuationTurns,
			Subject:       reviewContext,
		}, entryLog, ctx)

		switch triageVerdict {
		case triageWait:
			pending.PendingRetryAt = now.Add(pollInterval)
			requeueReaction(state, key, pending, waitTriage)
			continue
		case triageHandled:
			pending.PendingRetryAt = now.Add(pollInterval)
			pending.LastCheck.Comments.New = 0
			requeueReaction(state, key, pending, waitPollInterval)
			continue
		case triageEscalate:
			escalateReviewFailure(state, params, pending, turnCount, EscalationTriggerTriage, reviewData, fingerprint, now, pollInterval, entryLog, ctx, metrics)
			continue
		}

		// Dispatch.
		metrics.IncReviewChecks("dispatched")

		scheduleReactionContinuation(ctx, state, params.Store, ScheduleRetryParams{
			IssueID:     pending.IssueID,
			Identifier:  pending.Identifier,
			DisplayID:   pending.DisplayID,
			Attempt:     pending.Attempt,
			DelayMS:     continuationDelayMS,
			LastSSHHost: pending.LastSSHHost,
			ContinuationContext: map[string]any{
				reviewCommentsKey: reviewContext,
			},
			ReactionKind:        ReactionKindReview,
			Continuation:        continuationTargetOf(pending, turnCount+1),
			AgentKind:           pending.AgentKind,
			ConfiguredModel:     pending.ConfiguredModel,
			RuleName:            pending.RuleName,
			StageLineage:        pending.StageLineage,
			RuleSettingsApplied: pending.RuleSettingsApplied,
			TemplateID:          pending.TemplateID,
			Logger:              entryLog,

			IssueIdentity: pending.IssueIdentity,
		}, params.OnRetryFire, now)

		state.ReactionAttempts[rkey]++

		entryLog.Info("review comments detected, scheduling review-fix dispatch",
			slog.Int("comment_count", len(actionable)),
			slog.Int("excluded_count", excluded),
			slog.Int("review_fix_attempt", state.ReactionAttempts[rkey]),
			slog.Int("max_continuation_turns", params.ReviewConfig.MaxContinuationTurns),
		)
	}
}

// computeReactionPendingDelay returns the backoff delay for a reaction
// pending re-check at the given attempt count. Attempt 0 returns zero
// (immediate). Each subsequent attempt returns
// reviewPendingBackoffBase * 2^attempts, capped at
// [reviewPendingBackoffCap]. Shared by the review and bot-review
// reconcile passes for fetch-error backoff.
func computeReactionPendingDelay(attempts int) time.Duration {
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
// turns are exhausted or the triage command asked for a person. It begins
// the escalation on the comment set fingerprint and applies the configured
// escalation action unless the set was escalated before, cancels the retry,
// releases the claim unless a worker runs for the issue, and clears the
// review reaction's own fingerprint. The entry and its counter stay, and
// the entry re-enqueues at the poll interval, until the escalation ends.
// The handed-off comment set stays, so a comment already given to a run is
// never reported as new. Sibling reaction kinds for the same issue are left
// untouched.
func escalateReviewFailure(
	state *State,
	params ReconcileParams,
	pending *PendingReaction,
	turnCount int,
	trigger ReactionEscalationTrigger,
	reviewData *ReviewReactionData,
	fingerprint string,
	now time.Time,
	pollInterval time.Duration,
	log *slog.Logger,
	ctx context.Context,
	metrics domain.Metrics,
) {
	if beginEscalation(ctx, state, params.Store, pending, trigger, fingerprint, now, log) {
		applyReviewEscalation(state, params, pending, turnCount, trigger, reviewData, log, ctx, metrics)
	}

	CancelRetry(state, pending.IssueID)

	if err := params.Store.DeleteRetryEntry(ctx, pending.IssueID); err != nil {
		log.Error("failed to delete retry entry during review escalation",
			slog.Any("error", err),
		)
	}

	releaseClaimUnlessRunning(state, pending.IssueID)

	// Scoped to this kind's own slot: a sibling reaction's pending
	// entry, counter, and fingerprint for the same issue survive a
	// review-only escalation.
	if err := params.Store.DeleteReactionFingerprint(ctx, pending.IssueID, ReactionKindReview); err != nil {
		log.Warn("failed to delete reaction fingerprint during review escalation",
			slog.Any("error", err),
		)
	}

	pending.PendingRetryAt = now.Add(pollInterval)
	requeueReaction(state, ReactionKey(pending.IssueID, ReactionKindReview), pending, waitPollInterval)
}

// applyReviewEscalation applies the configured escalation action of the
// review reaction: the event, and the label when so configured.
func applyReviewEscalation(
	state *State,
	params ReconcileParams,
	pending *PendingReaction,
	turnCount int,
	trigger ReactionEscalationTrigger,
	reviewData *ReviewReactionData,
	log *slog.Logger,
	ctx context.Context,
	metrics domain.Metrics,
) {
	if trigger == EscalationTriggerTriage {
		log.Warn("review triage requested escalation",
			slog.Int("turn_count", turnCount),
			slog.Int("max_continuation_turns", params.ReviewConfig.MaxContinuationTurns),
		)
	} else {
		log.Warn("review fix continuation turns exhausted, escalating",
			slog.Int("turn_count", turnCount),
			slog.Int("max_continuation_turns", params.ReviewConfig.MaxContinuationTurns),
		)
	}

	var facts domain.MessageFacts = domain.ReviewEscalationFacts{
		PRNumber: reviewData.PRNumber,
		Branch:   reviewData.Branch,
		Turns:    turnCount,
	}
	if trigger == EscalationTriggerTriage {
		facts = domain.TriageEscalationFacts{PRNumber: reviewData.PRNumber}
	}
	delivery := params.Router.Route(reactionNotification(domain.EventEscalationReviewComments, pending, facts))

	if params.ReviewConfig.Escalation == "label" {
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

			state.TrackerOpsWg.Go(func() {
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
			})
		} else {
			metrics.IncReviewEscalations(params.ReviewConfig.Escalation)
		}
	}

	// A label owns the escalation counter, so only the other modes record it.
	var record func(action string)
	if params.ReviewConfig.Escalation != "label" {
		record = metrics.IncReviewEscalations
	}
	publishEscalation(ctx, &state.TrackerOpsWg, delivery, log, record, func(err error) {
		log.Warn("review escalation comment failed", slog.Any("error", err))
	})
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

// commentTemplateKey returns the prompt variable that carries the comments
// of a review-family reaction kind, or "" for any other kind.
func commentTemplateKey(kind string) string {
	switch kind {
	case ReactionKindReview:
		return reviewCommentsKey
	case ReactionKindBotReview:
		return botReviewCommentsKey
	default:
		return ""
	}
}

// actionableComments returns the comments a reaction pass hands to an
// agent: outdated comments are dropped and, for the review kind, so are
// comments by an allowlisted bot author. The platform-marker half of bot
// classification already ran inside the adapter, and a comment with no
// known author is never treated as a bot.
func actionableComments(kind string, comments []domain.ReviewComment, botUsernames []string) []domain.ReviewComment {
	var actionable []domain.ReviewComment
	for _, c := range comments {
		if c.Outdated {
			continue
		}
		if kind == ReactionKindReview && c.Reviewer != "" && scmcore.IsBotAuthor(c.Reviewer, false, botUsernames) {
			continue
		}
		actionable = append(actionable, c)
	}
	return actionable
}

// countCurrentComments returns the number of comments that are not outdated.
func countCurrentComments(comments []domain.ReviewComment) int {
	n := 0
	for _, c := range comments {
		if !c.Outdated {
			n++
		}
	}
	return n
}

// loadHandedOffComments caches the stored handed-off set of the issue and
// kind unless the key is already cached. A key with no rows is cached as
// empty.
func loadHandedOffComments(ctx context.Context, state *State, store storage.ReactionHandoffStore, issueID, kind string) error {
	rkey := ReactionKey(issueID, kind)
	if _, loaded := state.ReactionHandedOffComments[rkey]; loaded {
		return nil
	}
	commentIDs, err := store.ListReactionHandedOffComments(ctx, issueID, kind)
	if err != nil {
		return err
	}
	if state.ReactionHandedOffComments == nil {
		state.ReactionHandedOffComments = make(map[string]map[string]struct{})
	}
	set := make(map[string]struct{}, len(commentIDs))
	for _, id := range commentIDs {
		set[id] = struct{}{}
	}
	state.ReactionHandedOffComments[rkey] = set
	return nil
}

// settleOutcome is how [settleHandedOffCommentSet] judged an actionable set.
type settleOutcome int

const (
	// settleOpen means the set holds a comment that is new.
	settleOpen settleOutcome = iota

	// settleSettled means the set holds no new comment.
	settleSettled

	// settleLoadFailed means the handed-off set could not be loaded, so
	// nothing was judged.
	settleLoadFailed
)

// settleHandedOffCommentSet judges whether the pass re-enqueues an
// actionable set instead of dispatching it. The set settles when it holds
// no comment that is new; a handed-off set that cannot be loaded defers the
// pass without marking anything and reports apart from a settled set. A
// settled set is marked dispatched only when the handed-off and reported
// sets cover every ID: a set covered only by the running turn's IDs stays
// unmarked so it is judged again once that run has exited.
func settleHandedOffCommentSet(ctx context.Context, state *State, store ReconcileStore, issueID, kind string, actionable []domain.ReviewComment, log *slog.Logger) settleOutcome {
	if err := loadHandedOffComments(ctx, state, store, issueID, kind); err != nil {
		log.Warn("failed to load handed-off comments, deferring",
			slog.String("reaction_kind", kind),
			slog.Any("error", err),
		)
		return settleLoadFailed
	}
	if carriesNewComment(state, issueID, kind, actionable) {
		return settleOpen
	}

	rkey := ReactionKey(issueID, kind)
	handedOff := state.ReactionHandedOffComments[rkey]
	reported := state.ReactionReportedComments[rkey]
	for _, c := range actionable {
		_, inHandedOff := handedOff[c.ID]
		_, inReported := reported[c.ID]
		if !inHandedOff && !inReported {
			log.Debug("comment set held by a running turn, not dispatching",
				slog.String("reaction_kind", kind),
				slog.Int("comment_count", len(actionable)),
			)
			return settleSettled
		}
	}

	if err := store.MarkReactionDispatched(ctx, issueID, kind); err != nil {
		log.Warn("failed to mark handed-off comment set dispatched",
			slog.String("reaction_kind", kind),
			slog.Any("error", err),
		)
	}
	log.Debug("comment set already handed off, not dispatching",
		slog.String("reaction_kind", kind),
		slog.Int("comment_count", len(actionable)),
	)
	return settleSettled
}

// commentReactionCheck records what a review or bot-review check found:
// how many comments are actionable and how many of those the reaction acts
// on next.
func commentReactionCheck(observedAt time.Time, actionable, fresh int) *ReactionCheck {
	return &ReactionCheck{
		ObservedAt: observedAt,
		Comments:   &CommentCheck{Actionable: actionable, New: fresh},
	}
}

// carriesNewComment reports whether any comment is new. It never modifies
// state.
func carriesNewComment(state *State, issueID, kind string, comments []domain.ReviewComment) bool {
	return newCommentCount(state, issueID, kind, comments) > 0
}

// newCommentCount returns how many comments are new: outside the cached
// handed-off set, the reported set, and the IDs the issue's running turn
// carries. The handed-off key must already be loaded. It never modifies
// state.
func newCommentCount(state *State, issueID, kind string, comments []domain.ReviewComment) int {
	return countCommentsOutsideRuns(state, issueID, kind, comments, true)
}

// ungivenCommentCount returns how many comments no run was given: outside
// the cached handed-off set and the IDs the issue's running turn carries.
// The reported set does not count, because the bot-review escalation puts
// the comments it escalates there. Only an inline comment counts for
// bot-review, as for its escalation. The handed-off key must already be
// loaded.
func ungivenCommentCount(state *State, issueID, kind string, comments []domain.ReviewComment) int {
	if kind == ReactionKindBotReview {
		inline := make([]domain.ReviewComment, 0, len(comments))
		for _, c := range comments {
			if c.FilePath != "" {
				inline = append(inline, c)
			}
		}
		comments = inline
	}
	return countCommentsOutsideRuns(state, issueID, kind, comments, false)
}

func countCommentsOutsideRuns(state *State, issueID, kind string, comments []domain.ReviewComment, reportedCounts bool) int {
	rkey := ReactionKey(issueID, kind)
	handedOff := state.ReactionHandedOffComments[rkey]
	var reported map[string]struct{}
	if reportedCounts {
		reported = state.ReactionReportedComments[rkey]
	}
	var running map[string]struct{}
	if entry := state.Running[issueID]; entry != nil {
		running = make(map[string]struct{})
		for _, id := range continuationCommentIDs(kind, entry.ContinuationContext) {
			running[id] = struct{}{}
		}
	}
	fresh := 0
	for _, c := range comments {
		_, inHandedOff := handedOff[c.ID]
		_, inReported := reported[c.ID]
		_, inRunning := running[c.ID]
		if !inHandedOff && !inReported && !inRunning {
			fresh++
		}
	}
	return fresh
}

// recordHandedOffComments adds commentIDs to the handed-off set of the
// issue and kind, writing through to the store after the cache. A cached
// key gains the absent IDs and only those are stored. For an uncached key
// every ID is stored and the key stays uncached, because a partial key
// would read as authoritative. A store error keeps the cache.
func recordHandedOffComments(ctx context.Context, state *State, store storage.ReactionHandoffStore, issueID, kind string, commentIDs []string, log *slog.Logger) {
	toStore := commentIDs
	if cached, loaded := state.ReactionHandedOffComments[ReactionKey(issueID, kind)]; loaded {
		toStore = nil
		for _, id := range commentIDs {
			if _, known := cached[id]; known {
				continue
			}
			cached[id] = struct{}{}
			toStore = append(toStore, id)
		}
	}
	if len(toStore) == 0 {
		return
	}
	if err := store.AddReactionHandedOffComments(ctx, issueID, kind, toStore); err != nil {
		log.Warn("failed to persist handed-off comments",
			slog.String("reaction_kind", kind),
			slog.Any("error", err),
		)
	}
}

// recordReportedComments adds the ID of every comment to the reported set of
// the issue under the bot-review kind, the only kind that reports comments.
// The reported set is runtime-only.
func recordReportedComments(state *State, issueID string, comments []domain.ReviewComment) {
	if len(comments) == 0 {
		return
	}
	if state.ReactionReportedComments == nil {
		state.ReactionReportedComments = make(map[string]map[string]struct{})
	}
	rkey := ReactionKey(issueID, ReactionKindBotReview)
	reported := state.ReactionReportedComments[rkey]
	if reported == nil {
		reported = make(map[string]struct{}, len(comments))
		state.ReactionReportedComments[rkey] = reported
	}
	for _, c := range comments {
		reported[c.ID] = struct{}{}
	}
}

// continuationCommentIDs returns the IDs of the comments that
// [buildReviewTemplateMap] wrote under the kind's key in a continuation
// map, or nil when the key is absent or the kind carries none.
func continuationCommentIDs(kind string, continuation map[string]any) []string {
	key := commentTemplateKey(kind)
	if key == "" {
		return nil
	}
	items, ok := continuation[key].([]map[string]any)
	if !ok {
		return nil
	}
	var ids []string
	for _, item := range items {
		if id, ok := item["id"].(string); ok && id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// freshRunSeedParams holds the dependencies of [freshRunSeed].
type freshRunSeedParams struct {
	SCMAdapter          domain.SCMAdapter
	Store               ReconcileStore
	WorkspaceRoot       string
	ReviewConfigured    bool
	ReviewConfig        ReviewReactionConfig
	BotReviewConfigured bool
	BotReviewConfig     BotReviewReactionConfig
}

// freshRunSeed returns the continuation map a fresh dispatch of issue
// carries, holding for each seedable kind the pull request's actionable
// comments when one of them is new. A kind is seedable when it is configured
// and has no triage block, since triage decides whether an agent gets the
// comments. Nothing is seeded for a workspace whose scm.json names no pull
// request. A fetch or load failure skips only the kind concerned. It returns
// nil when nothing is seeded.
func freshRunSeed(ctx context.Context, state *State, p freshRunSeedParams, issue domain.Issue, log *slog.Logger) map[string]any {
	var kinds []string
	if p.ReviewConfigured && !p.ReviewConfig.Triage.Enabled() {
		kinds = append(kinds, ReactionKindReview)
	}
	if p.BotReviewConfigured && !p.BotReviewConfig.Triage.Enabled() {
		kinds = append(kinds, ReactionKindBotReview)
	}
	if p.SCMAdapter == nil || len(kinds) == 0 {
		return nil
	}

	log = logging.WithIssue(log, issue.ID, issue.Identifier)
	pathResult, err := workspace.ComputePath(p.WorkspaceRoot, issue.Identifier)
	if err != nil {
		for _, kind := range kinds {
			warnFreshRunNotSeeded(log, kind, err)
		}
		return nil
	}
	meta := workspace.ReadSCMMetadata(pathResult.Path, log)
	if meta.PRNumber <= 0 || meta.Owner == "" || meta.Repo == "" || meta.Branch == "" {
		return nil
	}

	var seed map[string]any
	for _, kind := range kinds {
		comments, err := fetchFreshRunComments(ctx, state, p, issue.ID, kind, meta)
		if err != nil {
			warnFreshRunNotSeeded(log, kind, err)
			continue
		}
		if len(comments) == 0 {
			continue
		}
		if seed == nil {
			seed = make(map[string]any, len(kinds))
		}
		seed[commentTemplateKey(kind)] = buildReviewTemplateMap(comments)
	}
	return seed
}

// fetchFreshRunComments returns the pull request's actionable comments of
// the kind when at least one is new, loading the kind's handed-off set on
// the way. It returns nil when none is new.
func fetchFreshRunComments(ctx context.Context, state *State, p freshRunSeedParams, issueID, kind string, meta domain.SCMMetadata) ([]domain.ReviewComment, error) {
	var comments []domain.ReviewComment
	var err error
	switch kind {
	case ReactionKindReview:
		comments, err = p.SCMAdapter.FetchPendingReviews(ctx, meta.PRNumber, meta.Owner, meta.Repo)
	default:
		comments, err = p.SCMAdapter.FetchBotReviewComments(ctx, meta.PRNumber, meta.Owner, meta.Repo, p.BotReviewConfig.BotUsernames)
	}
	if err != nil {
		return nil, fmt.Errorf("fetch %s comments: %w", kind, err)
	}

	actionable := actionableComments(kind, comments, p.BotReviewConfig.BotUsernames)
	if len(actionable) == 0 {
		return nil, nil
	}
	if err := loadHandedOffComments(ctx, state, p.Store, issueID, kind); err != nil {
		return nil, fmt.Errorf("load handed-off %s comments: %w", kind, err)
	}
	if !carriesNewComment(state, issueID, kind, actionable) {
		return nil, nil
	}
	return actionable, nil
}

func warnFreshRunNotSeeded(log *slog.Logger, kind string, err error) {
	log.Warn("fresh run not given review comments",
		slog.String("reaction_kind", kind),
		slog.Any("error", err),
	)
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
