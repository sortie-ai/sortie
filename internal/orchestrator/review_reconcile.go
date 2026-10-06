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

		// TTL enforcement.
		if ttl > 0 && now.Sub(pending.CreatedAt) > ttl {
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
			entryLog.Warn("review watch window elapsed, dropping",
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

		// Fetch reviews from SCM.
		comments, err := params.SCMAdapter.FetchPendingReviews(ctx, reviewData.PRNumber, reviewData.Owner, reviewData.Repo)
		if err != nil {
			pending.PendingAttempts++
			delay := max(computeReactionPendingDelay(pending.PendingAttempts), pollInterval)
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

		if settleHandedOffCommentSet(ctx, state, params.Store, pending.IssueID, ReactionKindReview, actionable, entryLog) {
			pending.PendingRetryAt = now.Add(pollInterval)
			state.PendingReactions[key] = pending
			continue
		}

		// Debounce: if LastEventAt is recent, defer dispatch.
		if !reviewData.LastEventAt.IsZero() && now.Sub(reviewData.LastEventAt) < debounceDuration {
			pending.PendingRetryAt = reviewData.LastEventAt.Add(debounceDuration)
			state.PendingReactions[key] = pending
			entryLog.Debug("review comments within debounce window, deferring")
			continue
		}

		// Arbitrate the retry slot before dispatching.
		if incumbent := retrySlotIncumbent(state, pending.IssueID); incumbent != nil {
			logRetrySlotDeferral(entryLog, ReactionKindReview, incumbent)
			pending.CreatedAt = now
			state.PendingReactions[key] = pending
			continue
		}

		// A set that reaches this point holds a comment no run was given,
		// so a spent budget escalates it.
		turnCount := state.ReactionAttempts[rkey]
		if turnCount >= params.ReviewConfig.MaxContinuationTurns {
			escalateReviewFailure(state, params, pending, turnCount, EscalationTriggerBudget, reviewData, entryLog, ctx, metrics)
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
		case triageWait, triageHandled:
			pending.PendingRetryAt = now.Add(pollInterval)
			state.PendingReactions[key] = pending
			continue
		case triageEscalate:
			escalateReviewFailure(state, params, pending, turnCount, EscalationTriggerTriage, reviewData, entryLog, ctx, metrics)
			continue
		}

		// Dispatch.
		metrics.IncReviewChecks("dispatched")

		ScheduleRetry(state, ScheduleRetryParams{
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
			AgentKind:           pending.AgentKind,
			RuleName:            pending.RuleName,
			RuleSettingsApplied: pending.RuleSettingsApplied,
			TemplateID:          pending.TemplateID,
			Logger:              entryLog,
		}, params.OnRetryFire)

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
// turns are exhausted. It applies the configured escalation action,
// cancels the retry, releases the claim, and clears the review reaction's
// own pending entry, counter, and fingerprint. The handed-off comment set
// stays, so a comment already given to a run is never reported as new.
// Sibling reaction kinds for the same issue are left untouched.
func escalateReviewFailure(
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

	commentText := buildReviewEscalationComment(reviewData, turnCount)
	if trigger == EscalationTriggerTriage {
		commentText = buildTriageEscalationComment("review", fmt.Sprintf("PR #%d", reviewData.PRNumber))
	}
	delivery := params.Router.Route(reactionNotification(domain.EventEscalationReviewComments, pending, commentText))

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

	CancelRetry(state, pending.IssueID)

	if err := params.Store.DeleteRetryEntry(ctx, pending.IssueID); err != nil {
		log.Error("failed to delete retry entry during review escalation",
			slog.Any("error", err),
		)
	}

	delete(state.Claimed, pending.IssueID)

	// Scoped to this kind's own slot: a sibling reaction's pending
	// entry, counter, and fingerprint for the same issue survive a
	// review-only escalation.
	delete(state.PendingReactions, ReactionKey(pending.IssueID, ReactionKindReview))
	delete(state.ReactionAttempts, ReactionKey(pending.IssueID, ReactionKindReview))
	if err := params.Store.DeleteReactionFingerprint(ctx, pending.IssueID, ReactionKindReview); err != nil {
		log.Warn("failed to delete reaction fingerprint during review escalation",
			slog.Any("error", err),
		)
	}
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

// handedOffCommentWriter persists the comment IDs a run was given.
type handedOffCommentWriter interface {
	AddReactionHandedOffComments(ctx context.Context, issueID, kind string, commentIDs []string) error
}

// handedOffCommentReader loads the comment IDs an issue's runs were given.
type handedOffCommentReader interface {
	ListReactionHandedOffComments(ctx context.Context, issueID, kind string) ([]string, error)
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
func loadHandedOffComments(ctx context.Context, state *State, store handedOffCommentReader, issueID, kind string) error {
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

// settleHandedOffCommentSet reports whether the pass re-enqueues an
// actionable set instead of dispatching it. The set settles when it holds
// no comment that is new, and also when its handed-off set cannot be
// loaded, which defers the pass without marking anything. A settled set is
// marked dispatched only when the handed-off and reported sets cover every
// ID: a set covered only by the running turn's IDs stays unmarked so it is
// judged again once that run has exited.
func settleHandedOffCommentSet(ctx context.Context, state *State, store ReconcileStore, issueID, kind string, actionable []domain.ReviewComment, log *slog.Logger) bool {
	if err := loadHandedOffComments(ctx, state, store, issueID, kind); err != nil {
		log.Warn("failed to load handed-off comments, deferring",
			slog.String("reaction_kind", kind),
			slog.Any("error", err),
		)
		return true
	}
	if carriesNewComment(state, issueID, kind, actionable) {
		return false
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
			return true
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
	return true
}

// carriesNewComment reports whether any comment is new: its ID is outside
// the cached handed-off set, the reported set, and the IDs the issue's
// running turn carries. The handed-off key must already be loaded. It never
// modifies state.
func carriesNewComment(state *State, issueID, kind string, comments []domain.ReviewComment) bool {
	rkey := ReactionKey(issueID, kind)
	handedOff := state.ReactionHandedOffComments[rkey]
	reported := state.ReactionReportedComments[rkey]
	var running map[string]struct{}
	if entry := state.Running[issueID]; entry != nil {
		running = make(map[string]struct{})
		for _, id := range continuationCommentIDs(kind, entry.ContinuationContext) {
			running[id] = struct{}{}
		}
	}
	for _, c := range comments {
		_, inHandedOff := handedOff[c.ID]
		_, inReported := reported[c.ID]
		_, inRunning := running[c.ID]
		if !inHandedOff && !inReported && !inRunning {
			return true
		}
	}
	return false
}

// recordHandedOffComments adds commentIDs to the handed-off set of the
// issue and kind, writing through to the store after the cache. A cached
// key gains the absent IDs and only those are stored. For an uncached key
// every ID is stored and the key stays uncached, because a partial key
// would read as authoritative. A store error keeps the cache.
func recordHandedOffComments(ctx context.Context, state *State, store handedOffCommentWriter, issueID, kind string, commentIDs []string, log *slog.Logger) {
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
// the issue and kind. The reported set is runtime-only.
func recordReportedComments(state *State, issueID, kind string, comments []domain.ReviewComment) {
	if len(comments) == 0 {
		return
	}
	if state.ReactionReportedComments == nil {
		state.ReactionReportedComments = make(map[string]map[string]struct{})
	}
	rkey := ReactionKey(issueID, kind)
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
