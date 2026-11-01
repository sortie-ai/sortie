package orchestrator

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/storage"
)

// escalationRecordStore is the persistence contract of the shared escalation
// mechanism.
type escalationRecordStore interface {
	storage.ReactionEscalationStore
	storage.ReactionActivityStore
}

// ReactionEscalationRestoreStore is the persistence contract of
// [RestoreReactionEscalations].
type ReactionEscalationRestoreStore interface {
	storage.ReactionEscalationStore
	storage.RunHistoryStore
}

// ReactionEscalationRestoreResult reports outcome counts from a single
// [RestoreReactionEscalations] call.
type ReactionEscalationRestoreResult struct {
	Applied int // records taken by an entry recovery rebuilt
	Rebuilt int // entries the restore built from a record
	Ended   int // records no configured kind or run row could watch
}

// escalationStands reports whether an escalation began on the entry and has
// not ended.
func (p *PendingReaction) escalationStands() bool {
	return !p.EscalatedAt.IsZero()
}

// beginEscalation starts the escalation of pending unless one stands, and
// saves its record with the kind's attempt counter and the entry's pull
// request. It reports whether the kind applies its configured action: only
// when it started an escalation and the stored subject differs from
// subject or subject is empty, so a subject gets one action however often
// its escalation ends and begins again. A failed record read counts as no
// record; a failed save is retried by [retryEscalationSave].
func beginEscalation(
	ctx context.Context,
	state *State,
	store escalationRecordStore,
	pending *PendingReaction,
	trigger ReactionEscalationTrigger,
	subject string,
	now time.Time,
	log *slog.Logger,
) bool {
	if pending.escalationStands() {
		return false
	}

	recordReactionActivity(ctx, store, pending.IssueID, pending.StageLineage,
		storage.ReactionActivityEscalation, []string{pending.Kind}, now, log)

	stored, found, err := store.GetReactionEscalation(ctx, pending.IssueID, pending.Kind)
	if err != nil {
		log.Warn("failed to read reaction escalation record, treating it as absent",
			slog.String("reaction_kind", pending.Kind),
			slog.Any("error", err),
		)
		found = false
	}

	pending.EscalatedAt = now
	pending.EscalationTrigger = trigger
	pending.EscalationSubject = subject
	saveEscalationRecord(ctx, state, store, pending, log)

	return subject == "" || !found || stored.Subject != subject
}

// retryEscalationSave saves the record of a standing escalation whose
// earlier save failed. It is a no-op for any other entry.
func retryEscalationSave(ctx context.Context, state *State, store escalationRecordStore, pending *PendingReaction, log *slog.Logger) {
	if pending.escalationStands() && pending.escalationUnsaved {
		saveEscalationRecord(ctx, state, store, pending, log)
	}
}

func saveEscalationRecord(ctx context.Context, state *State, store escalationRecordStore, pending *PendingReaction, log *slog.Logger) {
	pr := pullRequestOf(pending)
	escalatedAt := pending.EscalatedAt
	err := store.SaveReactionEscalation(ctx, storage.ReactionEscalation{
		IssueID:     pending.IssueID,
		Kind:        pending.Kind,
		Subject:     pending.EscalationSubject,
		EscalatedAt: &escalatedAt,
		Trigger:     string(pending.EscalationTrigger),
		Attempts:    state.ReactionAttempts[ReactionKey(pending.IssueID, pending.Kind)],
		PRNumber:    pr.Number,
		PROwner:     pr.Owner,
		PRRepo:      pr.Repo,
		PRBranch:    pr.Branch,
	})
	pending.escalationUnsaved = err != nil
	if err != nil {
		log.Error("failed to save reaction escalation record, retrying on the next pass",
			slog.String("reaction_kind", pending.Kind),
			slog.Any("error", err),
		)
	}
}

// endEscalation ends the standing escalation of pending and its record,
// which keeps its subject. It is a no-op for an entry with no escalation.
func endEscalation(ctx context.Context, store escalationRecordStore, pending *PendingReaction, log *slog.Logger) {
	if !pending.escalationStands() {
		return
	}
	pending.EscalatedAt = time.Time{}
	pending.EscalationTrigger = ""
	pending.EscalationSubject = ""
	pending.NoStateObserved = false
	pending.escalationUnsaved = false
	pending.resumeAfterRestart = false

	if err := store.EndReactionEscalation(ctx, pending.IssueID, pending.Kind); err != nil {
		log.Warn("failed to end reaction escalation record",
			slog.String("reaction_kind", pending.Kind),
			slog.Any("error", err),
		)
	}
}

// endEscalationAndCounter ends a standing escalation together with the
// counter that stays only while it stands. An entry with no escalation
// keeps its counter.
func endEscalationAndCounter(ctx context.Context, state *State, store escalationRecordStore, pending *PendingReaction, log *slog.Logger) {
	if !pending.escalationStands() {
		return
	}
	endEscalation(ctx, store, pending, log)
	delete(state.ReactionAttempts, ReactionKey(pending.IssueID, pending.Kind))
}

// endLiftedBudgetEscalation ends a budget escalation whose ceiling no
// longer counts the stored tries as spent, and reports whether it ended.
func endLiftedBudgetEscalation(ctx context.Context, store escalationRecordStore, pending *PendingReaction, spent bool, log *slog.Logger) bool {
	if !pending.escalationStands() || pending.EscalationTrigger != EscalationTriggerBudget || spent {
		return false
	}
	endEscalation(ctx, store, pending, log)
	return true
}

// carryEscalation moves the standing escalation of previous onto the entry
// that replaces it, so a worker exit's seed does not discard it.
func carryEscalation(seeded, previous *PendingReaction) {
	if previous == nil || !previous.escalationStands() {
		return
	}
	seeded.EscalatedAt = previous.EscalatedAt
	seeded.EscalationTrigger = previous.EscalationTrigger
	seeded.EscalationSubject = previous.EscalationSubject
	seeded.escalationUnsaved = previous.escalationUnsaved
}

// isPullRequestNotFound reports whether err says the forge no longer has
// the pull request.
func isPullRequestNotFound(err error) bool {
	scmErr, ok := errors.AsType[*domain.SCMError](err)
	return ok && scmErr.Kind == domain.ErrSCMNotFound
}

// pullRequestEnded reports whether a mergeability read shows the pull
// request merged, closed, or gone.
func pullRequestEnded(status domain.PRMergeStatus, err error) bool {
	if err != nil {
		return isPullRequestNotFound(err)
	}
	return status.Merged || status.Closed
}

// watchStandingPullRequest reads the pull request of a standing comment-set
// escalation. It reports true when the pass is done with the entry: the pull
// request ended, which ends the escalation and drops the entry, or the read
// failed, which re-enqueues the entry with backoff. dropCounter says whether
// the counter leaves with the entry.
func watchStandingPullRequest(
	ctx context.Context,
	state *State,
	store escalationRecordStore,
	scm domain.SCMAdapter,
	key string,
	pending *PendingReaction,
	dropCounter bool,
	now time.Time,
	pollInterval time.Duration,
	log *slog.Logger,
) bool {
	pr := pullRequestOf(pending)
	status, err := scm.GetMergeability(ctx, pr.Number, pr.Owner, pr.Repo)
	if pullRequestEnded(status, err) {
		endEscalation(ctx, store, pending, log)
		cancelReactionTriage(pending)
		if dropCounter {
			delete(state.ReactionAttempts, ReactionKey(pending.IssueID, pending.Kind))
		}
		log.Info("escalated pull request ended, dropping",
			slog.String("reaction_kind", pending.Kind),
			slog.Int("pr_number", pr.Number),
		)
		return true
	}
	if err != nil {
		pending.PendingAttempts++
		delay := max(computeReactionPendingDelay(pending.PendingAttempts), pollInterval)
		pending.PendingRetryAt = now.Add(delay)
		requeueReaction(state, key, pending, waitBackoff)
		log.Warn("escalated pull request read failed, retrying with backoff",
			slog.String("reaction_kind", pending.Kind),
			slog.Any("error", err),
			slog.Int("pending_attempts", pending.PendingAttempts),
		)
		return true
	}
	notePullRequestURL(pending, status)
	return false
}

// commentEscalationOutcome is how a pass judged the standing escalation of
// a comment-set kind.
type commentEscalationOutcome int

const (
	commentEscalationStands commentEscalationOutcome = iota
	commentEscalationEnded
	commentEscalationUnread
)

// judgeCommentEscalation ends the standing escalation of a comment-set kind
// when its fact holds: a triage escalation ends when the comment set
// fingerprint differs from its subject, and a budget escalation when the
// ceiling no longer counts the tries as spent or the set holds no comment
// no run was given. A handed-off set that cannot be loaded judges nothing.
func judgeCommentEscalation(
	ctx context.Context,
	state *State,
	store ReconcileStore,
	pending *PendingReaction,
	actionable []domain.ReviewComment,
	fingerprint string,
	spent bool,
	log *slog.Logger,
) commentEscalationOutcome {
	if err := loadHandedOffComments(ctx, state, store, pending.IssueID, pending.Kind); err != nil {
		log.Warn("failed to load handed-off comments, deferring",
			slog.String("reaction_kind", pending.Kind),
			slog.Any("error", err),
		)
		return commentEscalationUnread
	}

	var ends bool
	if pending.EscalationTrigger == EscalationTriggerTriage {
		ends = fingerprint != pending.EscalationSubject
	} else {
		ends = !spent || ungivenCommentCount(state, pending.IssueID, pending.Kind, actionable) == 0
	}
	if !ends {
		return commentEscalationStands
	}
	endEscalation(ctx, store, pending, log)
	return commentEscalationEnded
}

// endOutsideHandoff ends every standing escalation of an issue that the
// tracker reads outside the handoff state while Sortie does not drive it,
// and drops each escalated entry with its counter. listed is the state a
// listing or batched read gave, never a terminal one; it is only a
// candidate, because a search can trail Sortie's own handoff write, so
// nothing ends until a direct read shows a non-terminal state other than
// the handoff state. A no-state reading ends the escalations at the second
// consecutive one. The record keeps its subject.
func endOutsideHandoff(
	ctx context.Context,
	state *State,
	store escalationRecordStore,
	tracker domain.TrackerAdapter,
	issueID, listed, handoff string,
	terminal []string,
	log *slog.Logger,
) {
	var escalated []*PendingReaction
	for _, entry := range state.PendingReactions {
		if entry.IssueID == issueID && entry.escalationStands() {
			escalated = append(escalated, entry)
		}
	}
	if handoff == "" || len(escalated) == 0 {
		return
	}

	if listed != "" {
		clearEscalatedNoState(escalated)
		if strings.EqualFold(listed, handoff) {
			return
		}
	}
	if issueIsDriven(state, issueID) {
		return
	}

	log = logging.WithIssue(log, issueID, escalated[0].Identifier)
	read, err := tracker.FetchIssueByID(ctx, issueID)
	if err != nil {
		log.Warn("tracker read for an escalated issue failed, keeping escalations",
			slog.Any("error", err),
		)
		return
	}

	if read.State == "" {
		confirmed := true
		for _, entry := range escalated {
			if !confirmNoState(&entry.NoStateObserved) {
				confirmed = false
			}
		}
		if !confirmed {
			return
		}
	} else {
		clearEscalatedNoState(escalated)
		if strings.EqualFold(read.State, handoff) || isTerminalState(read.State, terminal) {
			return
		}
	}

	for _, entry := range escalated {
		endEscalation(ctx, store, entry, log)
		cancelReactionTriage(entry)
		rkey := ReactionKey(entry.IssueID, entry.Kind)
		delete(state.PendingReactions, rkey)
		delete(state.ReactionAttempts, rkey)
		if entry.Kind == ReactionKindBotReview {
			delete(state.ReactionReportedComments, rkey)
		}
		log.Info("escalation ended: issue left the handoff state",
			slog.String("reaction_kind", entry.Kind),
			slog.String("state", read.State),
		)
	}
}

func clearEscalatedNoState(escalated []*PendingReaction) {
	for _, entry := range escalated {
		entry.NoStateObserved = false
	}
}

// issueIsDriven reports whether Sortie runs, retries, holds, or parks the
// issue, so a tracker reading of it is Sortie's own work and not a person's.
func issueIsDriven(state *State, issueID string) bool {
	if _, running := state.Running[issueID]; running {
		return true
	}
	if _, retrying := state.RetryAttempts[issueID]; retrying {
		return true
	}
	if _, claimed := state.Claimed[issueID]; claimed {
		return true
	}
	_, parked := state.Parked[issueID]
	return parked
}

// RestoreReactionEscalations gives the entries rebuilt at startup the
// escalations the previous process left standing, and builds the entries
// recovery could not. It must run after [RecoverPendingReactions] and
// before the event loop starts.
//
// A record whose kind is not configured, or whose issue has no successful
// run row, ends. A failure to list the records or load the run rows returns
// an error and leaves state untouched.
func RestoreReactionEscalations(
	ctx context.Context,
	state *State,
	store ReactionEscalationRestoreStore,
	params PendingReactionRecoveryParams,
) (ReactionEscalationRestoreResult, error) {
	records, err := store.ListStandingReactionEscalations(ctx)
	if err != nil {
		return ReactionEscalationRestoreResult{}, err
	}

	var unwatched []string
	seen := make(map[string]struct{})
	for _, rec := range records {
		if _, watched := state.PendingReactions[ReactionKey(rec.IssueID, rec.Kind)]; watched {
			continue
		}
		if _, dup := seen[rec.IssueID]; dup {
			continue
		}
		seen[rec.IssueID] = struct{}{}
		unwatched = append(unwatched, rec.IssueID)
	}
	runRows, err := store.LoadLatestSuccessfulRunsForIssues(ctx, unwatched)
	if err != nil {
		return ReactionEscalationRestoreResult{}, err
	}
	runs := make(map[string]storage.RunHistory, len(runRows))
	for _, run := range runRows {
		runs[run.IssueID] = run
	}

	log := params.Logger
	if log == nil {
		log = slog.Default()
	}
	now := time.Now().UTC()
	if params.NowFunc != nil {
		now = params.NowFunc().UTC()
	}
	if state.PendingReactions == nil {
		state.PendingReactions = make(map[string]*PendingReaction)
	}
	if state.ReactionAttempts == nil {
		state.ReactionAttempts = make(map[string]int)
	}

	var result ReactionEscalationRestoreResult
	for _, rec := range records {
		if rec.EscalatedAt == nil {
			continue
		}
		key := ReactionKey(rec.IssueID, rec.Kind)
		entry := state.PendingReactions[key]
		if entry != nil {
			result.Applied++
		} else {
			run, hasRun := runs[rec.IssueID]
			if !reactionKindConfigured(params, rec.Kind) || !hasRun {
				endRestoredRecord(ctx, store, rec, log)
				result.Ended++
				continue
			}
			pr := pullRequestRef{Number: rec.PRNumber, Owner: rec.PROwner, Repo: rec.PRRepo, Branch: rec.PRBranch}
			entry = recoveredPendingReaction(run, rec.Kind, pr, now)
			state.PendingReactions[key] = entry
			result.Rebuilt++
		}

		entry.EscalatedAt = rec.EscalatedAt.UTC()
		entry.EscalationTrigger = ReactionEscalationTrigger(rec.Trigger)
		entry.EscalationSubject = rec.Subject
		state.ReactionAttempts[key] = rec.Attempts
		switch rec.Kind {
		case ReactionKindCI, ReactionKindMergeConflict:
			entry.HeadRecordedAt = entry.EscalatedAt
		case ReactionKindMergeCompletion:
			entry.resumeAfterRestart = true
		}
	}
	return result, nil
}

func endRestoredRecord(ctx context.Context, store ReactionEscalationRestoreStore, rec storage.ReactionEscalation, log *slog.Logger) {
	log.Warn("reaction escalation record has no configured kind or run row, ending it",
		slog.String("issue_id", rec.IssueID),
		slog.String("reaction_kind", rec.Kind),
	)
	if err := store.EndReactionEscalation(ctx, rec.IssueID, rec.Kind); err != nil {
		log.Warn("failed to end reaction escalation record",
			slog.String("issue_id", rec.IssueID),
			slog.String("reaction_kind", rec.Kind),
			slog.Any("error", err),
		)
	}
}
