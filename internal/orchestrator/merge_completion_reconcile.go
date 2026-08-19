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
)

// mergeCompletionPendingBackoffBase is the floor for the merge-completion
// poll interval and, through it, for the per-tick deferral and
// fetch-error backoff delays. The exponential backoff itself is computed
// by [computeReactionPendingDelay]; this constant only bounds it from
// below.
const mergeCompletionPendingBackoffBase = 10 * time.Second

// mergeCompletionMissingSHAGracePeriod bounds the time spent waiting for a
// forge to populate a merge commit identifier after it first reports the pull
// request as merged.
const mergeCompletionMissingSHAGracePeriod = 30 * time.Minute

// mergeCompletionMissingSHAObservationKind is an internal persistence kind,
// not a pending reaction kind. Its fingerprint is the normalized pull-request
// identity, its updated_at is the first missing-SHA observation time, and its
// dispatched bit records whether the operator escalation was delivered.
const mergeCompletionMissingSHAObservationKind = "merge-completion-missing-sha"

// mergeCompletionDueEntry is a pending merge-completion entry that has
// passed its retry-delay check and is ready for this tick's tracker
// state read.
type mergeCompletionDueEntry struct {
	key     string
	pending *PendingReaction
	data    *MergeCompletionReactionData
}

// reconcileMergeCompletion observes the merge state of managed pull
// requests for pending merge-completion entries, latches on the merge
// commit identifier, and transitions the linked issue to the configured
// target state exactly once per merge.
//
// Called from [ReconcileRunningIssues] last, after
// [reconcileLabelFixCommands]. Skipped entirely when params.SCMAdapter or
// params.TrackerAdapter is nil, or params.MergeCompletionReactionConfigured
// is false.
func reconcileMergeCompletion(state *State, params ReconcileParams, log *slog.Logger, ctx context.Context, _ domain.Metrics) {
	if params.SCMAdapter == nil || params.TrackerAdapter == nil {
		return
	}
	if !params.MergeCompletionReactionConfigured {
		return
	}

	now := time.Now().UTC()
	if params.NowFunc != nil {
		now = params.NowFunc().UTC()
	}

	warnMergeCompletionTerminalDrift(state, params, log)

	pollInterval := max(time.Duration(params.MergeCompletionConfig.PollIntervalMS)*time.Millisecond, mergeCompletionPendingBackoffBase)

	var due []mergeCompletionDueEntry
	for key, pending := range state.PendingReactions {
		if pending.Kind != ReactionKindMergeCompletion {
			continue
		}
		delete(state.PendingReactions, key)

		data, ok := pending.KindData.(*MergeCompletionReactionData)
		if !ok {
			log.ErrorContext(ctx, "unexpected KindData type for merge_completion reaction",
				slog.String("issue_id", pending.IssueID),
				slog.String("type", fmt.Sprintf("%T", pending.KindData)),
			)
			continue
		}

		if now.Before(pending.PendingRetryAt) {
			state.PendingReactions[key] = pending
			continue
		}

		due = append(due, mergeCompletionDueEntry{key: key, pending: pending, data: data})
	}
	if len(due) == 0 {
		return
	}

	issueIDs := make([]string, 0, len(due))
	for _, entry := range due {
		issueIDs = append(issueIDs, entry.pending.IssueID)
	}

	statesByID, err := params.TrackerAdapter.FetchIssueStatesByIDs(ctx, issueIDs)
	if err != nil {
		for _, entry := range due {
			backoffMergeCompletionReenqueue(state, entry.key, entry.pending, now, pollInterval)
		}
		log.Warn("merge_completion issue state read failed",
			slog.Any("error", err),
		)
		return
	}

	terminalSet := stateSet(params.TerminalStates)

	for _, entry := range due {
		processMergeCompletionEntry(state, params, entry, statesByID, terminalSet, now, pollInterval, log, ctx)
	}
}

// warnMergeCompletionTerminalDrift tests, once per pass and before any
// pending entry is examined, whether the target state captured at
// construction is still a member of the runtime terminal-state list. A
// reloaded tracker.terminal_states that no longer contains the target
// state emits one WARN per onset, suppressed by
// [State.MergeCompletionTerminalDriftLogged] while the condition
// persists. The check never blocks a transition, changes an entry's
// disposition, or reads whether any entry is due.
func warnMergeCompletionTerminalDrift(state *State, params ReconcileParams, log *slog.Logger) {
	_, terminal := stateSet(params.TerminalStates)[strings.ToLower(params.MergeCompletionConfig.TargetState)]
	if terminal {
		state.MergeCompletionTerminalDriftLogged = false
		return
	}
	if state.MergeCompletionTerminalDriftLogged {
		return
	}
	log.Warn("merge_completion target state is not in the configured terminal states",
		slog.String("target_state", params.MergeCompletionConfig.TargetState),
		slog.Any("terminal_states", params.TerminalStates),
	)
	state.MergeCompletionTerminalDriftLogged = true
}

// processMergeCompletionEntry evaluates one due entry against the
// tracker state read already performed for the whole due set: it drops
// an entry whose issue is gone, terminal, or has left the handoff state;
// defers one still claimed by the orchestrator; and otherwise observes
// the merge, latches on the merge commit identifier, and performs the
// transition.
func processMergeCompletionEntry(
	state *State,
	params ReconcileParams,
	entry mergeCompletionDueEntry,
	statesByID map[string]string,
	terminalSet map[string]struct{},
	now time.Time,
	pollInterval time.Duration,
	log *slog.Logger,
	ctx context.Context,
) {
	key, pending, data := entry.key, entry.pending, entry.data
	entryLog := logging.WithIssue(log, pending.IssueID, pending.Identifier)

	stateName, known := statesByID[pending.IssueID]
	if !known {
		clearMergeCompletionMissingSHAObservation(params.Store, pending.IssueID, entryLog, ctx)
		entryLog.Warn("merge_completion issue not found in tracker state response, dropping")
		return
	}
	if _, terminal := terminalSet[strings.ToLower(stateName)]; terminal {
		clearMergeCompletionMissingSHAObservation(params.Store, pending.IssueID, entryLog, ctx)
		entryLog.Info("merge_completion issue already terminal, dropping",
			slog.String("state", stateName),
		)
		return
	}
	if _, claimed := state.Claimed[pending.IssueID]; claimed {
		pending.PendingRetryAt = now.Add(pollInterval)
		state.PendingReactions[key] = pending
		return
	}
	if !strings.EqualFold(stateName, params.HandoffState) {
		clearMergeCompletionMissingSHAObservation(params.Store, pending.IssueID, entryLog, ctx)
		entryLog.Info("merge_completion stopped: issue left the handoff state",
			slog.String("state", stateName),
		)
		return
	}

	status, mergeErr := params.SCMAdapter.GetMergeability(ctx, data.PRNumber, data.Owner, data.Repo)
	if mergeErr != nil {
		var scmErr *domain.SCMError
		if errors.As(mergeErr, &scmErr) && scmErr.Kind == domain.ErrSCMNotFound {
			clearMergeCompletionMissingSHAObservation(params.Store, pending.IssueID, entryLog, ctx)
			entryLog.Warn("merge_completion pull request not found, dropping",
				slog.Int("pr_number", data.PRNumber),
			)
			return
		}
		backoffMergeCompletionReenqueue(state, key, pending, now, pollInterval)
		entryLog.Warn("merge_completion mergeability fetch failed, retrying with backoff",
			slog.Any("error", mergeErr),
			slog.Int("pending_attempts", pending.PendingAttempts),
		)
		return
	}

	if !status.Merged {
		clearMergeCompletionMissingSHAObservationForChangedPR(
			params.Store,
			pending.IssueID,
			mergeCompletionPRIdentity(data),
			entryLog,
			ctx,
		)
		pending.PendingRetryAt = now.Add(pollInterval)
		state.PendingReactions[key] = pending
		return
	}
	if status.MergeCommitSHA == "" {
		handleMergeCompletionMissingSHA(state, params, key, pending, data, now, pollInterval, entryLog, ctx)
		return
	}

	if upsertErr := params.Store.UpsertReactionFingerprint(ctx, pending.IssueID, ReactionKindMergeCompletion, status.MergeCommitSHA); upsertErr != nil {
		backoffMergeCompletionReenqueue(state, key, pending, now, pollInterval)
		entryLog.Warn("failed to upsert merge_completion reaction fingerprint",
			slog.Any("error", upsertErr),
		)
		return
	}
	storedFP, dispatched, fpErr := params.Store.GetReactionFingerprint(ctx, pending.IssueID, ReactionKindMergeCompletion)
	if fpErr != nil {
		backoffMergeCompletionReenqueue(state, key, pending, now, pollInterval)
		entryLog.Warn("failed to read merge_completion reaction fingerprint",
			slog.Any("error", fpErr),
		)
		return
	}
	if storedFP == status.MergeCommitSHA && dispatched {
		clearMergeCompletionMissingSHAObservation(params.Store, pending.IssueID, entryLog, ctx)
		entryLog.Debug("merge_completion already transitioned for this merge",
			slog.String("merge_commit_sha", status.MergeCommitSHA),
		)
		return
	}

	rkey := ReactionKey(pending.IssueID, ReactionKindMergeCompletion)
	transitionErr := params.TrackerAdapter.TransitionIssue(ctx, pending.IssueID, params.MergeCompletionConfig.TargetState)
	state.ReactionAttempts[rkey]++

	if transitionErr != nil {
		routeTransitionFailure(state, params, key, pending, data, transitionErr, now, pollInterval, entryLog, ctx)
		return
	}

	markErr := params.Store.MarkReactionDispatched(ctx, pending.IssueID, ReactionKindMergeCompletion)
	delete(state.ReactionAttempts, rkey)
	if markErr != nil {
		entryLog.Warn("merge_completion dispatched mark failed",
			slog.String("merge_commit_sha", status.MergeCommitSHA),
			slog.Any("error", markErr),
		)
		return
	}
	clearMergeCompletionMissingSHAObservation(params.Store, pending.IssueID, entryLog, ctx)
	entryLog.Info("merge_completion transitioned issue to terminal state",
		slog.String("target_state", params.MergeCompletionConfig.TargetState),
		slog.Int("pr_number", data.PRNumber),
		slog.String("merge_commit_sha", status.MergeCommitSHA),
	)
}

// handleMergeCompletionMissingSHA persists the first time a specific pull
// request is reported merged without a commit identifier. It retries with the
// pending-entry backoff during the fixed grace period, then drops the entry
// and issues the configured operator notification. A successful notification
// marks the observation dispatched; a failed one remains eligible for a later
// fresh pending entry to retry without reopening the current polling loop.
func handleMergeCompletionMissingSHA(
	state *State,
	params ReconcileParams,
	key string,
	pending *PendingReaction,
	data *MergeCompletionReactionData,
	now time.Time,
	pollInterval time.Duration,
	log *slog.Logger,
	ctx context.Context,
) {
	identity := mergeCompletionPRIdentity(data)
	observation, err := params.Store.UpsertReactionObservation(
		ctx,
		pending.IssueID,
		mergeCompletionMissingSHAObservationKind,
		identity,
		now,
	)
	if err != nil {
		backoffMergeCompletionReenqueue(state, key, pending, now, pollInterval)
		log.Warn("failed to persist merge_completion missing-SHA observation",
			slog.Any("error", err),
			slog.String("repository", data.Owner+"/"+data.Repo),
			slog.Int("pr_number", data.PRNumber),
		)
		return
	}

	waited := max(now.Sub(observation.FirstObservedAt), time.Duration(0))
	if observation.Dispatched {
		delete(state.ReactionAttempts, ReactionKey(pending.IssueID, ReactionKindMergeCompletion))
		log.Warn("merge_completion missing-SHA escalation already delivered, stopping",
			slog.String("repository", data.Owner+"/"+data.Repo),
			slog.Int("pr_number", data.PRNumber),
			slog.Duration("waited", waited),
			slog.String("manual_action", "verify the merge and transition the tracker issue manually if appropriate"),
		)
		return
	}
	if waited < mergeCompletionMissingSHAGracePeriod {
		backoffMergeCompletionReenqueue(state, key, pending, now, pollInterval)
		log.Warn("merge_completion merged pull request reported no merge commit, waiting with backoff",
			slog.String("repository", data.Owner+"/"+data.Repo),
			slog.Int("pr_number", data.PRNumber),
			slog.Duration("waited", waited),
			slog.Duration("grace_period", mergeCompletionMissingSHAGracePeriod),
			slog.Int("pending_attempts", pending.PendingAttempts),
		)
		return
	}

	delete(state.ReactionAttempts, ReactionKey(pending.IssueID, ReactionKindMergeCompletion))
	log.Error("merge_completion stopped after merge commit identifier remained missing",
		slog.String("repository", data.Owner+"/"+data.Repo),
		slog.Int("pr_number", data.PRNumber),
		slog.Duration("waited", waited),
		slog.String("reason", "pull request is merged but the forge did not provide a merge commit identifier"),
		slog.String("manual_action", "verify the merge and transition the tracker issue manually if appropriate"),
	)
	escalateMergeCompletionMissingSHA(state, params, pending, data, waited, log, ctx)
}

// mergeCompletionPRIdentity returns the normalized identity used only by the
// missing-SHA observation. It deliberately does not replace the real merge
// commit identifier used by the normal merge-completion idempotency latch.
func mergeCompletionPRIdentity(data *MergeCompletionReactionData) string {
	owner := strings.ToLower(strings.TrimSpace(data.Owner))
	repo := strings.ToLower(strings.TrimSpace(data.Repo))
	return fmt.Sprintf("%s/%s#%d", owner, repo, data.PRNumber)
}

// clearMergeCompletionMissingSHAObservation removes stale anomaly state when
// the issue or pull request leaves the merge-completion lifecycle. Cleanup is
// best effort because these paths already have a definitive stop condition.
func clearMergeCompletionMissingSHAObservation(
	store ReconcileStore,
	issueID string,
	log *slog.Logger,
	ctx context.Context,
) {
	if err := store.DeleteReactionFingerprint(ctx, issueID, mergeCompletionMissingSHAObservationKind); err != nil {
		log.Warn("failed to clear merge_completion missing-SHA observation",
			slog.Any("error", err),
		)
	}
}

// clearMergeCompletionMissingSHAObservationForChangedPR removes an old
// anomaly marker when a fresh pending entry identifies a different, currently
// unmerged pull request. The new PR does not receive an observation until it
// is itself first reported merged without a commit identifier.
func clearMergeCompletionMissingSHAObservationForChangedPR(
	store ReconcileStore,
	issueID, currentIdentity string,
	log *slog.Logger,
	ctx context.Context,
) {
	storedIdentity, _, err := store.GetReactionFingerprint(ctx, issueID, mergeCompletionMissingSHAObservationKind)
	if err != nil {
		log.Warn("failed to inspect merge_completion missing-SHA observation for PR change",
			slog.Any("error", err),
		)
		return
	}
	if storedIdentity == "" || storedIdentity == currentIdentity {
		return
	}
	clearMergeCompletionMissingSHAObservation(store, issueID, log, ctx)
}

// backoffMergeCompletionReenqueue increments the pending entry's attempt
// counter, applies exponential backoff floored at pollInterval, and
// reinserts the entry into state.PendingReactions.
func backoffMergeCompletionReenqueue(state *State, key string, pending *PendingReaction, now time.Time, pollInterval time.Duration) {
	pending.PendingAttempts++
	delay := max(computeReactionPendingDelay(pending.PendingAttempts), pollInterval)
	pending.PendingRetryAt = now.Add(delay)
	state.PendingReactions[key] = pending
}

// routeTransitionFailure routes a failing TransitionIssue call: a
// not-found issue stops without escalating, an auth or payload failure
// escalates immediately consuming no retry budget, and a transport,
// API, or unclassified failure retries with backoff until the call
// count exceeds MaxRetries, then escalates. An unclassified error (not
// a [*domain.TrackerError]) routes with the retryable kinds, the
// non-destructive default.
func routeTransitionFailure(
	state *State,
	params ReconcileParams,
	key string,
	pending *PendingReaction,
	data *MergeCompletionReactionData,
	err error,
	now time.Time,
	pollInterval time.Duration,
	log *slog.Logger,
	ctx context.Context,
) {
	rkey := ReactionKey(pending.IssueID, ReactionKindMergeCompletion)

	var trackerErr *domain.TrackerError
	kind := domain.ErrTrackerTransport
	if errors.As(err, &trackerErr) {
		kind = trackerErr.Kind
	}

	switch kind {
	case domain.ErrTrackerNotFound:
		if markErr := params.Store.MarkReactionDispatched(ctx, pending.IssueID, ReactionKindMergeCompletion); markErr != nil {
			log.Warn("failed to mark merge_completion dispatched on not-found stop",
				slog.Any("error", markErr),
			)
		}
		clearMergeCompletionMissingSHAObservation(params.Store, pending.IssueID, log, ctx)
		delete(state.ReactionAttempts, rkey)
		log.Warn("merge_completion issue not found, stopping",
			slog.Any("error", err),
		)

	case domain.ErrTrackerAuth, domain.ErrTrackerPayload:
		log.Error("merge_completion transition failed, escalating",
			slog.Any("error", err),
		)
		escalateMergeCompletion(state, params, pending, data, state.ReactionAttempts[rkey], log, ctx)

	default:
		attempts := state.ReactionAttempts[rkey]
		if attempts > params.MergeCompletionConfig.MaxRetries {
			log.Warn("merge_completion transition attempts exhausted, escalating",
				slog.Int("attempts", attempts),
				slog.Int("max_retries", params.MergeCompletionConfig.MaxRetries),
			)
			escalateMergeCompletion(state, params, pending, data, attempts, log, ctx)
			return
		}
		backoffMergeCompletionReenqueue(state, key, pending, now, pollInterval)
		log.Warn("merge_completion transition failed, retrying with backoff",
			slog.Any("error", err),
			slog.Int("pending_attempts", pending.PendingAttempts),
		)
	}
}

// escalateMergeCompletion dispatches the configured escalation action
// (label or comment) asynchronously through [State.TrackerOpsWg] and
// clears the transition call counter. The pending entry is already
// removed by the caller; the fingerprint row is left undispatched, the
// accepted residue of an escalated merge-completion attempt.
func escalateMergeCompletion(
	state *State,
	params ReconcileParams,
	pending *PendingReaction,
	data *MergeCompletionReactionData,
	attempts int,
	log *slog.Logger,
	ctx context.Context,
) {
	issueID := pending.IssueID
	tracker := params.TrackerAdapter
	escalLog := log

	switch params.MergeCompletionConfig.Escalation {
	case "label":
		label := params.MergeCompletionConfig.EscalationLabel

		state.TrackerOpsWg.Go(func() {
			dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()

			if err := tracker.AddLabel(dctx, issueID, label); err != nil {
				escalLog.Warn("merge_completion escalation label failed",
					slog.Any("error", err),
				)
			}
		})

	case "comment":
		commentText := buildMergeCompletionEscalationComment(data, params.MergeCompletionConfig.TargetState, attempts)

		state.TrackerOpsWg.Go(func() {
			dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()

			if err := tracker.CommentIssue(dctx, issueID, commentText); err != nil {
				escalLog.Warn("merge_completion escalation comment failed",
					slog.Any("error", err),
				)
			}
		})
	}

	delete(state.ReactionAttempts, ReactionKey(pending.IssueID, ReactionKindMergeCompletion))
}

// escalateMergeCompletionMissingSHA sends the configured operator signal for
// an expired missing-SHA observation. The pending entry is already absent. A
// successful write durably marks the observation dispatched; a failed write
// leaves it undispatched so only delivery can be retried by a later fresh
// pending entry, without reopening this process's polling loop.
func escalateMergeCompletionMissingSHA(
	state *State,
	params ReconcileParams,
	pending *PendingReaction,
	data *MergeCompletionReactionData,
	waited time.Duration,
	log *slog.Logger,
	ctx context.Context,
) {
	issueID := pending.IssueID
	tracker := params.TrackerAdapter
	escalLog := log
	markDelivered := func() {
		markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()

		if err := params.Store.MarkReactionObservationDispatched(
			markCtx,
			issueID,
			mergeCompletionMissingSHAObservationKind,
			mergeCompletionPRIdentity(data),
		); err != nil {
			escalLog.Error("merge_completion missing-SHA escalation delivered but marker write failed; a fresh pending entry may repeat delivery",
				slog.Any("error", err),
				slog.String("repository", data.Owner+"/"+data.Repo),
				slog.Int("pr_number", data.PRNumber),
			)
		}
	}

	switch params.MergeCompletionConfig.Escalation {
	case "label":
		label := params.MergeCompletionConfig.EscalationLabel
		state.TrackerOpsWg.Go(func() {
			dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()

			if err := tracker.AddLabel(dctx, issueID, label); err != nil {
				escalLog.Error("merge_completion missing-SHA escalation label failed; polling remains stopped and a fresh pending entry can retry delivery",
					slog.Any("error", err),
					slog.String("repository", data.Owner+"/"+data.Repo),
					slog.Int("pr_number", data.PRNumber),
				)
				return
			}
			markDelivered()
		})

	case "comment":
		commentText := buildMergeCompletionMissingSHAEscalationComment(
			data,
			params.MergeCompletionConfig.TargetState,
			waited,
		)
		state.TrackerOpsWg.Go(func() {
			dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()

			if err := tracker.CommentIssue(dctx, issueID, commentText); err != nil {
				escalLog.Error("merge_completion missing-SHA escalation comment failed; polling remains stopped and a fresh pending entry can retry delivery",
					slog.Any("error", err),
					slog.String("repository", data.Owner+"/"+data.Repo),
					slog.Int("pr_number", data.PRNumber),
				)
				return
			}
			markDelivered()
		})
	}
}

// buildMergeCompletionEscalationComment returns the plain-text escalation
// comment used when a merge-completion transition cannot be performed.
// It names the pull request number, repository, configured target
// state, and the number of transition calls made, and no internal
// symbol, table, or file path.
func buildMergeCompletionEscalationComment(data *MergeCompletionReactionData, targetState string, attempts int) string {
	return fmt.Sprintf(
		"Sortie attempted %d time(s) and could not transition the issue to %q after PR #%d (%s/%s) merged. Manual transition required.",
		attempts,
		targetState,
		data.PRNumber,
		data.Owner,
		data.Repo,
	)
}

// buildMergeCompletionMissingSHAEscalationComment returns the operator-facing
// explanation used after the missing-SHA grace period. It includes the
// affected repository and pull request, elapsed wait, stop reason, and the
// required manual follow-up.
func buildMergeCompletionMissingSHAEscalationComment(
	data *MergeCompletionReactionData,
	targetState string,
	waited time.Duration,
) string {
	return fmt.Sprintf(
		"Sortie stopped monitoring %s/%s PR #%d after waiting %s because the pull request is reported merged but no merge commit identifier was provided. Verify the merge in the forge and transition this issue to %q manually if appropriate.",
		data.Owner,
		data.Repo,
		data.PRNumber,
		waited.Round(time.Second),
		targetState,
	)
}
