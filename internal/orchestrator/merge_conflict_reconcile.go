package orchestrator

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/storage"
)

// mergeConflictPendingBackoffBase is the floor for the merge-conflict poll
// interval and, through it, for the per-tick deferral and fetch-error
// backoff delays. The exponential backoff itself is computed by
// [computeReactionPendingDelay]; this constant only bounds it from below.
const mergeConflictPendingBackoffBase = 10 * time.Second

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
// over-limit.
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
			cancelReactionTriage(pending)
			log.ErrorContext(ctx, "unexpected KindData type for merge-conflict reaction",
				slog.String("issue_id", pending.IssueID),
				slog.String("type", fmt.Sprintf("%T", pending.KindData)),
			)
			continue
		}

		entryLog := logging.WithIssue(log, pending.IssueID, pending.Identifier)

		if ttl > 0 && !pending.escalationStands() && now.Sub(watchWindowStart(pending)) > ttl {
			cancelReactionTriage(pending)
			delete(state.ReactionAttempts, ReactionKey(pending.IssueID, ReactionKindMergeConflict))
			entryLog.Warn("merge conflict watch window elapsed, dropping",
				slog.Int64("window_ms", int64(ttl/time.Millisecond)),
				slog.Int64("age_ms", int64(now.Sub(watchWindowStart(pending))/time.Millisecond)),
			)
			continue
		}

		if reactionWaits(pending, now) {
			state.PendingReactions[key] = pending
			continue
		}
		retryEscalationSave(ctx, state, params.Store, pending, entryLog)

		// A run in flight makes this pass's mergeability read
		// redundant, so the entry waits one tick instead. The backoff
		// counter is untouched: waiting is not a fetch error.
		if pending.Triage != nil && !triageRunFinished(pending.Triage) {
			pending.PendingRetryAt = now
			requeueReaction(state, key, pending, waitTriage)
			continue
		}

		// The mergeability read runs every due tick with no cap before
		// it, so the not-dirty branch stays reachable and a resolved
		// conflict resets the counter rather than escalating.
		status, err := params.SCMAdapter.GetMergeability(ctx, data.PRNumber, data.Owner, data.Repo)
		if pending.escalationStands() && pullRequestEnded(status, err) {
			cancelReactionTriage(pending)
			endEscalationAndCounter(ctx, state, params.Store, pending, entryLog)
			entryLog.Info("escalated pull request ended, dropping",
				slog.String("reaction_kind", ReactionKindMergeConflict),
				slog.Int("pr_number", data.PRNumber),
			)
			continue
		}
		if err != nil {
			pending.PendingAttempts++
			delay := max(computeReactionPendingDelay(pending.PendingAttempts), pollInterval)
			pending.PendingRetryAt = now.Add(delay)
			requeueReaction(state, key, pending, waitBackoff)
			entryLog.Warn("merge conflict mergeability fetch failed, retrying with backoff",
				slog.Any("error", err),
				slog.Int("pending_attempts", pending.PendingAttempts),
				slog.Int64("retry_after_ms", int64(delay/time.Millisecond)),
			)
			metrics.IncMergeConflictChecks("error")
			continue
		}

		notePullRequestURL(pending, status)
		pending.LastCheck = mergeConflictReactionCheck(status, now)

		rkey := ReactionKey(pending.IssueID, ReactionKindMergeConflict)

		switch status.Mergeability {
		case domain.MergeabilityUnknown:
			pending.PendingRetryAt = now.Add(pollInterval)
			requeueReaction(state, key, pending, waitPollInterval)
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
			// The episode closes, so the fingerprint an in-flight run was
			// started for no longer describes anything.
			cancelReactionTriage(pending)
			endEscalation(ctx, params.Store, pending, entryLog)
			if delErr := params.Store.DeleteReactionFingerprint(ctx, pending.IssueID, ReactionKindMergeConflict); delErr != nil {
				entryLog.Warn("failed to delete merge conflict reaction fingerprint",
					slog.Any("error", delErr),
				)
			}
			delete(state.ReactionAttempts, rkey)
			pending.PendingRetryAt = now.Add(pollInterval)
			requeueReaction(state, key, pending, waitPollInterval)
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
// base branch), deduplicates on the head SHA, resets the per-episode
// attempt counter when the new conflicting head is positively not the
// orchestrator's own work, increments the counter, and either dispatches
// a rebase continuation or escalates when the new conflicting head
// pushes the episode strictly over the retry budget. The
// dedup-before-attribution-before-increment ordering means a same-head
// re-observation returns before either the attribution query or the
// increment runs.
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
	if pending.escalationStands() && standingMergeConflictHolds(ctx, state, params, pending, status, rkey, log) {
		pending.PendingRetryAt = now.Add(pollInterval)
		requeueReaction(state, key, pending, waitPollInterval)
		return
	}

	if incumbent := retrySlotIncumbent(state, pending.IssueID); incumbent != nil {
		logRetrySlotDeferral(log, ReactionKindMergeConflict, incumbent)
		pending.WatchRestartedAt = now
		requeueReaction(state, key, pending, waitRetrySlot)
		return
	}

	// An empty head provides no rebase anchor. Defer without burning an
	// attempt: a deferral is not a resolution attempt.
	if status.HeadSHA == "" {
		pending.PendingRetryAt = now.Add(pollInterval)
		requeueReaction(state, key, pending, waitBackoff)
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
		requeueReaction(state, key, pending, waitBackoff)
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
	// never re-increments the per-episode counter, and before the
	// attribution query below so a same-head re-observation never
	// reaches run_history either.
	storedFP, dispatched, fpErr := params.Store.GetReactionFingerprint(ctx, pending.IssueID, ReactionKindMergeConflict)
	if fpErr != nil {
		log.Warn("failed to get merge conflict reaction fingerprint, proceeding without dedup",
			slog.Any("error", fpErr),
		)
	} else if storedFP == fingerprint && dispatched {
		pending.PendingRetryAt = now.Add(pollInterval)
		requeueReaction(state, key, pending, waitPollInterval)
		log.Debug("merge conflict already dispatched for this head, skipping",
			slog.Int("pr_number", data.PRNumber),
		)
		return
	}

	// The gate sits above the head-change block so that block runs
	// once per fingerprint rather than once per pass: replaying it
	// would narrow the attribution window.
	triageVerdict := reactionTriageGate(state, params, pending, params.MergeConflictConfig.Triage, ReactionTriageRequest{
		Kind:          ReactionKindMergeConflict,
		WorkspaceRoot: params.WorkspaceRoot,
		IssueID:       pending.IssueID,
		Identifier:    pending.Identifier,
		DisplayID:     pending.DisplayID,
		Attempt:       pending.Attempt,
		SSHHost:       pending.LastSSHHost,
		Fingerprint:   fingerprint,
		AttemptsUsed:  state.ReactionAttempts[rkey],
		MaxAttempts:   params.MergeConflictConfig.MaxRetries,
		Subject:       buildMergeConflictTemplateMap(data, status),
	}, log, ctx)

	switch triageVerdict {
	case triageWait:
		pending.PendingRetryAt = now.Add(pollInterval)
		requeueReaction(state, key, pending, waitTriage)
		return
	case triageHandled:
		pending.PendingRetryAt = now.Add(pollInterval)
		requeueReaction(state, key, pending, waitPollInterval)
		return
	case triageEscalate:
		escalateMergeConflictFailure(state, params, pending, state.ReactionAttempts[rkey],
			EscalationTriggerTriage, data, fingerprint, now, pollInterval, log, ctx, metrics)
		return
	}

	// A new conflicting head this reaction has not dispatched for.
	// Reset the per-episode counter first when the change is positively
	// not the orchestrator's own work, so a person's push yields
	// attempts == 1 rather than continuing to climb across successive
	// conflicting heads regardless of who produced them.
	if classifyHeadChange(state, params, pending, now, ctx) == headChangeNotOurs {
		delete(state.ReactionAttempts, rkey)
	}
	pending.HeadRecordedAt = now

	// Per-episode increment for this new conflicting head, then the strict
	// over-limit cap. The increment happens before the cap so the new
	// conflicting head is what pushes the episode over budget, and the
	// escalation reports attempts == MaxRetries + 1.
	state.ReactionAttempts[rkey]++
	attempts := state.ReactionAttempts[rkey]
	if attempts > params.MergeConflictConfig.MaxRetries {
		escalateMergeConflictFailure(state, params, pending, attempts, EscalationTriggerBudget, data, fingerprint, now, pollInterval, log, ctx, metrics)
		return
	}

	dispatchMergeConflictContinuation(state, params, pending, data, status, now, log, ctx)
	metrics.IncMergeConflictChecks("dispatched")
}

// standingMergeConflictHolds ends the standing escalation when its ceiling
// no longer counts the rebases dispatched as spent or the conflicting head
// differs from the one escalated, and reports whether it still stands. A
// head the provider has not reported cannot differ.
func standingMergeConflictHolds(
	ctx context.Context,
	state *State,
	params ReconcileParams,
	pending *PendingReaction,
	status domain.PRMergeStatus,
	rkey string,
	log *slog.Logger,
) bool {
	spent := state.ReactionAttempts[rkey] >= params.MergeConflictConfig.MaxRetries
	if endLiftedBudgetEscalation(ctx, params.Store, pending, spent, log) {
		return false
	}
	if head := buildMergeConflictFingerprint(status.HeadSHA); head != "" && head != pending.EscalationSubject {
		endEscalation(ctx, params.Store, pending, log)
		return false
	}
	return true
}

// mergeConflictReactionCheck records what a mergeability read observed. A
// nil Mergeable means the provider had not computed mergeability.
func mergeConflictReactionCheck(status domain.PRMergeStatus, observedAt time.Time) *ReactionCheck {
	check := &MergeabilityCheck{}
	switch status.Mergeability {
	case domain.MergeabilityUnknown:
	case domain.MergeabilityDirty:
		check.Mergeable = new(false)
	default:
		check.Mergeable = new(true)
	}
	return &ReactionCheck{ObservedAt: observedAt, HeadSHA: status.HeadSHA, Mergeability: check}
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
// continuation turn carrying the PR's real base branch. The continuation
// reuses the same workspace identifier (no new workspace key). The
// fingerprint is marked dispatched only when the retry timer starts the
// worker: until then the continuation is an in-memory retry entry that a
// restart or a dropped retry loses, and a mark set here would make the
// dedup skip the head for good.
func dispatchMergeConflictContinuation(
	state *State,
	params ReconcileParams,
	pending *PendingReaction,
	data *MergeConflictReactionData,
	status domain.PRMergeStatus,
	now time.Time,
	log *slog.Logger,
	ctx context.Context,
) {
	mergeContext := buildMergeConflictTemplateMap(data, status)

	scheduleReactionContinuation(ctx, state, params.Store, ScheduleRetryParams{
		IssueID:     pending.IssueID,
		Identifier:  pending.Identifier,
		DisplayID:   pending.DisplayID,
		Attempt:     pending.Attempt,
		DelayMS:     continuationDelayMS,
		LastSSHHost: pending.LastSSHHost,
		ContinuationContext: map[string]any{
			"merge_conflict": mergeContext,
		},
		ReactionKind:        ReactionKindMergeConflict,
		Continuation:        continuationTargetOf(pending, state.ReactionAttempts[ReactionKey(pending.IssueID, ReactionKindMergeConflict)]),
		AgentKind:           pending.AgentKind,
		ConfiguredModel:     pending.ConfiguredModel,
		RuleName:            pending.RuleName,
		StageLineage:        pending.StageLineage,
		RuleSettingsApplied: pending.RuleSettingsApplied,
		TemplateID:          pending.TemplateID,
		Logger:              log,

		IssueIdentity: pending.IssueIdentity,
	}, params.OnRetryFire, now)

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

// escalateMergeConflictFailure begins the escalation on the conflicting
// head's fingerprint and applies the configured escalation action (label,
// comment, or none) unless that head was escalated before. The entry stays
// and re-enqueues at the poll interval until the escalation ends. A budget
// escalation leaves the attempt counter at the rebases dispatched, without
// the observation that tripped the cap; the counter is episodic, like the CI
// one, so the episode exit deletes it. The fingerprint row goes at the
// escalation. Sibling reactions' counters and state.Claimed are never
// touched.
func escalateMergeConflictFailure(
	state *State,
	params ReconcileParams,
	pending *PendingReaction,
	attempts int,
	trigger ReactionEscalationTrigger,
	data *MergeConflictReactionData,
	fingerprint string,
	now time.Time,
	pollInterval time.Duration,
	log *slog.Logger,
	ctx context.Context,
	metrics domain.Metrics,
) {
	rkey := ReactionKey(pending.IssueID, ReactionKindMergeConflict)
	if trigger == EscalationTriggerBudget {
		state.ReactionAttempts[rkey] = attempts - 1
	}

	if beginEscalation(ctx, state, params.Store, pending, trigger, fingerprint, now, log) {
		applyMergeConflictEscalation(state, params, pending, attempts, trigger, data, log, ctx, metrics)
	}

	if err := params.Store.DeleteReactionFingerprint(ctx, pending.IssueID, ReactionKindMergeConflict); err != nil {
		log.Warn("merge conflict fingerprint delete failed during escalation",
			slog.Any("error", err),
		)
	}

	pending.PendingRetryAt = now.Add(pollInterval)
	requeueReaction(state, ReactionKey(pending.IssueID, ReactionKindMergeConflict), pending, waitPollInterval)
}

// applyMergeConflictEscalation applies the configured escalation action of
// the merge-conflict reaction: the event, and the label when so configured.
func applyMergeConflictEscalation(
	state *State,
	params ReconcileParams,
	pending *PendingReaction,
	attempts int,
	trigger ReactionEscalationTrigger,
	data *MergeConflictReactionData,
	log *slog.Logger,
	ctx context.Context,
	metrics domain.Metrics,
) {
	if trigger == EscalationTriggerTriage {
		log.Warn("merge conflict triage requested escalation",
			slog.Int("attempts", attempts),
			slog.Int("max_retries", params.MergeConflictConfig.MaxRetries),
			slog.Int("pr_number", data.PRNumber),
		)
	} else {
		log.Warn("merge conflict resolution attempts exhausted, escalating",
			slog.Int("attempts", attempts),
			slog.Int("max_retries", params.MergeConflictConfig.MaxRetries),
			slog.Int("pr_number", data.PRNumber),
		)
	}

	var facts domain.MessageFacts = domain.MergeConflictFacts{PRNumber: data.PRNumber, Attempts: attempts}
	if trigger == EscalationTriggerTriage {
		facts = domain.TriageEscalationFacts{PRNumber: data.PRNumber}
	}
	delivery := params.Router.Route(reactionNotification(domain.EventEscalationMergeConflicts, pending, facts))

	if params.MergeConflictConfig.Escalation == "label" {
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
	}

	// A label owns the escalation counter, so only the other modes record it.
	var record func(action string)
	if params.MergeConflictConfig.Escalation != "label" {
		record = metrics.IncMergeConflictEscalations
	}
	publishEscalation(ctx, &state.TrackerOpsWg, delivery, log, record, func(err error) {
		log.Warn("merge conflict escalation comment failed", slog.Any("error", err))
	})
}

// releaseMergeConflictDispatchMark lifts the dispatched mark of the
// merge-conflict fingerprint when a rebase continuation's worker exits.
// A rebase that pushed gives the pull request a new head, which the next
// pass fingerprints afresh anyway. One that did not (the agent gave up,
// stopped for review, failed, or was cancelled) leaves the same
// conflicting head, and a standing mark would make the dedup skip it for
// good; lifted, the next pass counts another attempt against max_retries
// and escalates at the cap. A retry the exit schedules marks the head
// again when it starts. The store has no operation that clears the flag
// alone, so the row is deleted and written back for the same head; a
// failure between the two leaves no row, which the next pass reads as a
// new head.
func releaseMergeConflictDispatchMark(ctx context.Context, store storage.ReactionFingerprintStore, issueID string, log *slog.Logger) {
	fingerprint, dispatched, err := store.GetReactionFingerprint(ctx, issueID, ReactionKindMergeConflict)
	if err != nil {
		log.Warn("failed to read merge conflict fingerprint at rebase exit",
			slog.Any("error", err),
		)
		return
	}
	if !dispatched {
		return
	}
	if err := store.DeleteReactionFingerprint(ctx, issueID, ReactionKindMergeConflict); err != nil {
		log.Warn("failed to clear the dispatched mark of a rebased head",
			slog.Any("error", err),
		)
		return
	}
	if err := store.UpsertReactionFingerprint(ctx, issueID, ReactionKindMergeConflict, fingerprint); err != nil {
		log.Warn("failed to restore the merge conflict fingerprint after clearing the dispatched mark",
			slog.Any("error", err),
		)
	}
}
