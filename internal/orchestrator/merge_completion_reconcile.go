package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/storage"
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

		if reactionWaits(pending, now) {
			state.PendingReactions[key] = pending
			continue
		}
		retryEscalationSave(ctx, state, params.Store, pending, logging.WithIssue(log, pending.IssueID, pending.Identifier))

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
		if pending.escalationStands() {
			confirmMergeCompletionExit(state, params, key, pending, stateName, now, pollInterval, entryLog, ctx)
			return
		}
		clearMergeCompletionMissingSHAObservation(params.Store, pending.IssueID, entryLog, ctx)
		endEscalationAndCounter(ctx, state, params.Store, pending, entryLog)
		entryLog.Warn("merge_completion issue not found in tracker state response, dropping")
		return
	}
	if _, terminal := terminalSet[strings.ToLower(stateName)]; terminal {
		clearMergeCompletionMissingSHAObservation(params.Store, pending.IssueID, entryLog, ctx)
		endEscalationAndCounter(ctx, state, params.Store, pending, entryLog)
		watch := mergeWatch{ReactionKind: ReactionKindMergeCompletion, PRNumber: data.PRNumber, Owner: data.Owner, Repo: data.Repo}
		recordIssueOutcome(ctx, params.Store, releaseReadOutcome(ctx, params.SCMAdapter, watch, pending.IssueID, stateName, now, entryLog), entryLog)
		entryLog.Info("merge_completion issue already terminal, dropping",
			slog.String("state", stateName),
		)
		return
	}
	if _, claimed := state.Claimed[pending.IssueID]; claimed {
		pending.PendingRetryAt = now.Add(pollInterval)
		requeueReaction(state, key, pending, waitRetrySlot)
		return
	}
	if !strings.EqualFold(stateName, params.HandoffState) {
		if pending.escalationStands() {
			confirmMergeCompletionExit(state, params, key, pending, stateName, now, pollInterval, entryLog, ctx)
			return
		}
		clearMergeCompletionMissingSHAObservation(params.Store, pending.IssueID, entryLog, ctx)
		endEscalationAndCounter(ctx, state, params.Store, pending, entryLog)
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
			endEscalationAndCounter(ctx, state, params.Store, pending, entryLog)
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
	notePullRequestURL(pending, status)

	if !status.Merged {
		clearMergeCompletionMissingSHAObservationForChangedPR(
			params.Store,
			pending.IssueID,
			mergeCompletionPRIdentity(data),
			entryLog,
			ctx,
		)
		pending.PendingRetryAt = now.Add(pollInterval)
		requeueReaction(state, key, pending, waitPollInterval)
		return
	}
	recordIssueOutcome(ctx, params.Store, mergedOutcome(pending.IssueID, ReactionKindMergeCompletion, data.PRNumber, data.Owner, data.Repo, now), entryLog)

	rkey := ReactionKey(pending.IssueID, ReactionKindMergeCompletion)
	endLiftedBudgetEscalation(ctx, params.Store, pending, state.ReactionAttempts[rkey] > params.MergeCompletionConfig.MaxRetries, entryLog)
	if pending.EscalationTrigger == EscalationTriggerMissingMergeCommit && status.MergeCommitSHA != "" {
		endEscalation(ctx, params.Store, pending, entryLog)
	}
	// A standing escalation makes no transition call. One restored at
	// startup runs the normal path once, because the undispatched
	// fingerprint row is what lets a restart resume the attempt.
	if pending.escalationStands() && !pending.resumeAfterRestart {
		requeueMergeCompletionAtPollInterval(state, key, pending, now, pollInterval)
		return
	}
	pending.resumeAfterRestart = false

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
		endEscalationAndCounter(ctx, state, params.Store, pending, entryLog)
		entryLog.Debug("merge_completion already transitioned for this merge",
			slog.String("merge_commit_sha", status.MergeCommitSHA),
		)
		return
	}

	transitionErr := params.TrackerAdapter.TransitionIssue(ctx, pending.IssueID, params.MergeCompletionConfig.TargetState)
	state.ReactionAttempts[rkey]++
	recordReactionActivity(ctx, params.Store, pending.IssueID, pending.StageLineage,
		storage.ReactionActivityAction, []string{ReactionKindMergeCompletion}, now, entryLog)

	if transitionErr != nil {
		routeTransitionFailure(state, params, key, pending, data, status.MergeCommitSHA, transitionErr, now, pollInterval, entryLog, ctx)
		return
	}

	markErr := params.Store.MarkReactionDispatched(ctx, pending.IssueID, ReactionKindMergeCompletion)
	delete(state.ReactionAttempts, rkey)
	endEscalation(ctx, params.Store, pending, entryLog)
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
// pending-entry backoff during the fixed grace period, then escalates and
// issues the configured operator notification. A successful notification
// marks the observation dispatched; a failed one remains eligible for the
// retry a restart makes.
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
		beginEscalation(ctx, state, params.Store, pending, EscalationTriggerMissingMergeCommit, identity, now, log)
		requeueMergeCompletionAtPollInterval(state, key, pending, now, pollInterval)
		log.Error("merge_completion missing-SHA escalation already delivered, still watching",
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

	log.Error("merge_completion escalated after merge commit identifier remained missing, still watching",
		slog.String("repository", data.Owner+"/"+data.Repo),
		slog.Int("pr_number", data.PRNumber),
		slog.Duration("waited", waited),
		slog.String("reason", "pull request is merged but the forge did not provide a merge commit identifier"),
		slog.String("manual_action", "verify the merge and transition the tracker issue manually if appropriate"),
	)
	escalateMergeCompletionMissingSHA(state, params, key, pending, data, waited, now, pollInterval, log, ctx)
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
// requeues the entry with the backoff reason.
func backoffMergeCompletionReenqueue(state *State, key string, pending *PendingReaction, now time.Time, pollInterval time.Duration) {
	pending.PendingAttempts++
	delay := max(computeReactionPendingDelay(pending.PendingAttempts), pollInterval)
	pending.PendingRetryAt = now.Add(delay)
	requeueReaction(state, key, pending, waitBackoff)
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
	mergeCommit string,
	err error,
	now time.Time,
	pollInterval time.Duration,
	log *slog.Logger,
	ctx context.Context,
) {
	rkey := ReactionKey(pending.IssueID, ReactionKindMergeCompletion)

	kind := domain.ErrTrackerTransport
	if trackerErr, ok := errors.AsType[*domain.TrackerError](err); ok {
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
		endEscalation(ctx, params.Store, pending, log)
		log.Warn("merge_completion issue not found, stopping",
			slog.Any("error", err),
		)

	case domain.ErrTrackerAuth, domain.ErrTrackerPayload:
		log.Error("merge_completion transition failed, escalating",
			slog.Any("error", err),
		)
		escalateMergeCompletion(state, params, key, pending, data, state.ReactionAttempts[rkey], EscalationTriggerRefused, mergeCommit, now, pollInterval, log, ctx)

	default:
		attempts := state.ReactionAttempts[rkey]
		if attempts > params.MergeCompletionConfig.MaxRetries {
			log.Warn("merge_completion transition attempts exhausted, escalating",
				slog.Int("attempts", attempts),
				slog.Int("max_retries", params.MergeCompletionConfig.MaxRetries),
			)
			escalateMergeCompletion(state, params, key, pending, data, attempts, EscalationTriggerBudget, mergeCommit, now, pollInterval, log, ctx)
			return
		}
		backoffMergeCompletionReenqueue(state, key, pending, now, pollInterval)
		log.Warn("merge_completion transition failed, retrying with backoff",
			slog.Any("error", err),
			slog.Int("pending_attempts", pending.PendingAttempts),
		)
	}
}

// escalateMergeCompletion begins the escalation on the merge commit and
// dispatches the configured escalation action (label, comment, or none)
// asynchronously through [State.TrackerOpsWg] unless that merge commit was
// escalated before. The entry stays and re-enqueues at the poll interval
// until the escalation ends; so does the transition call counter. The
// fingerprint row is left undispatched, so a restart resumes the attempt.
func escalateMergeCompletion(
	state *State,
	params ReconcileParams,
	key string,
	pending *PendingReaction,
	data *MergeCompletionReactionData,
	attempts int,
	trigger ReactionEscalationTrigger,
	mergeCommit string,
	now time.Time,
	pollInterval time.Duration,
	log *slog.Logger,
	ctx context.Context,
) {
	if beginEscalation(ctx, state, params.Store, pending, trigger, mergeCommit, now, log) {
		applyMergeCompletionEscalation(state, params, pending, data, attempts, log, ctx)
	}
	requeueMergeCompletionAtPollInterval(state, key, pending, now, pollInterval)
}

// applyMergeCompletionEscalation applies the configured escalation action
// of the merge-completion reaction: the event, and the label when so
// configured.
func applyMergeCompletionEscalation(
	state *State,
	params ReconcileParams,
	pending *PendingReaction,
	data *MergeCompletionReactionData,
	attempts int,
	log *slog.Logger,
	ctx context.Context,
) {
	delivery := params.Router.Route(reactionNotification(domain.EventEscalationMergeCompletion, pending, domain.MergeCompletionFacts{
		Owner:       data.Owner,
		Repo:        data.Repo,
		PRNumber:    data.PRNumber,
		TargetState: params.MergeCompletionConfig.TargetState,
		Attempts:    attempts,
	}))

	if params.MergeCompletionConfig.Escalation == "label" {
		issueID := pending.IssueID
		tracker := params.TrackerAdapter
		escalLog := log
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
	}
	publishEscalation(ctx, &state.TrackerOpsWg, delivery, log, nil, func(err error) {
		log.Warn("merge_completion escalation comment failed", slog.Any("error", err))
	})
}

// confirmMergeCompletionExit ends the standing escalation of an entry whose
// batched state read shows the issue missing or outside the handoff state,
// but only through the direct read the shared rule makes: the batched read
// is a search and can trail Sortie's own handoff write. The entry returns
// to the queue first because that rule finds escalations there; it leaves
// it again when the rule ends them.
func confirmMergeCompletionExit(
	state *State,
	params ReconcileParams,
	key string,
	pending *PendingReaction,
	listed string,
	now time.Time,
	pollInterval time.Duration,
	log *slog.Logger,
	ctx context.Context,
) {
	requeueMergeCompletionAtPollInterval(state, key, pending, now, pollInterval)
	endOutsideHandoff(ctx, state, params.Store, params.TrackerAdapter, pending.IssueID, listed, params.HandoffState, params.TerminalStates, log)
	if state.PendingReactions[key] != pending {
		clearMergeCompletionMissingSHAObservation(params.Store, pending.IssueID, log, ctx)
	}
}

// requeueMergeCompletionAtPollInterval keeps the entry pending until the
// next poll, for the paths that hold an escalated entry without a call.
func requeueMergeCompletionAtPollInterval(state *State, key string, pending *PendingReaction, now time.Time, pollInterval time.Duration) {
	pending.PendingRetryAt = now.Add(pollInterval)
	requeueReaction(state, key, pending, waitPollInterval)
}

// escalateMergeCompletionMissingSHA begins the escalation on the pull
// request identity and sends the configured operator signal for an expired
// missing-SHA observation. The entry stays and re-enqueues at the poll
// interval. A successful write durably marks the observation dispatched; a
// failed write leaves it undispatched so a restart can retry the delivery.
//
// The tracker write and the marker write share one 30-second deadline
// instead of each getting its own, so the goroutine as a whole stays
// within the budget trackerOpsDrainTimeout assumes for a single
// TrackerOpsWg goroutine.
func escalateMergeCompletionMissingSHA(
	state *State,
	params ReconcileParams,
	key string,
	pending *PendingReaction,
	data *MergeCompletionReactionData,
	waited time.Duration,
	now time.Time,
	pollInterval time.Duration,
	log *slog.Logger,
	ctx context.Context,
) {
	// Whether the signal goes out is decided by the observation row the
	// caller consulted, not by the subject latch.
	beginEscalation(ctx, state, params.Store, pending, EscalationTriggerMissingMergeCommit, mergeCompletionPRIdentity(data), now, log)
	requeueMergeCompletionAtPollInterval(state, key, pending, now, pollInterval)

	issueID := pending.IssueID
	tracker := params.TrackerAdapter
	escalLog := log
	markDelivered := func(dctx context.Context) {
		if err := params.Store.MarkReactionObservationDispatched(
			dctx,
			issueID,
			mergeCompletionMissingSHAObservationKind,
			mergeCompletionPRIdentity(data),
		); err != nil {
			escalLog.Error("merge_completion missing-SHA escalation delivered but marker write failed; a restart may repeat delivery",
				slog.Any("error", err),
				slog.String("repository", data.Owner+"/"+data.Repo),
				slog.Int("pr_number", data.PRNumber),
			)
		}
	}

	delivery := params.Router.Route(reactionNotification(domain.EventEscalationMergeCompletion, pending, domain.MergeCompletionMissingSHAFacts{
		Owner:       data.Owner,
		Repo:        data.Repo,
		PRNumber:    data.PRNumber,
		TargetState: params.MergeCompletionConfig.TargetState,
		Waited:      waited,
	}))
	label := params.MergeCompletionConfig.EscalationLabel
	labels := params.MergeCompletionConfig.Escalation == "label"

	state.TrackerOpsWg.Go(func() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()

		if labels {
			if err := tracker.AddLabel(dctx, issueID, label); err != nil {
				escalLog.Error("merge_completion missing-SHA escalation label failed; the escalation stands and a restart can retry delivery",
					slog.Any("error", err),
					slog.String("repository", data.Owner+"/"+data.Repo),
					slog.Int("pr_number", data.PRNumber),
				)
				return
			}
			markDelivered(dctx)
			deliverEvent(dctx, delivery, escalLog)
			return
		}

		// Only the tracker comment decides the marker, so a slow
		// operator-destination send must not spend the shared deadline first.
		trackerDelivery, others := delivery.SplitTrackerComment()
		var sends sync.WaitGroup
		defer sends.Wait()
		if !others.Empty() {
			sends.Go(func() { deliverEvent(dctx, others, escalLog) })
		}

		received, err := deliverEvent(dctx, trackerDelivery, escalLog).TrackerComment()
		if received && err != nil {
			escalLog.Error("merge_completion missing-SHA escalation comment failed; the escalation stands and a restart can retry delivery",
				slog.Any("error", err),
				slog.String("repository", data.Owner+"/"+data.Repo),
				slog.Int("pr_number", data.PRNumber),
			)
			return
		}
		markDelivered(dctx)
	})
}
