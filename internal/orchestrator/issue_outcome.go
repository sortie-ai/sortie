package orchestrator

import (
	"context"
	"log/slog"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/storage"
)

// mergeWatch is the pull request a pending merge-completion or auto-merge
// entry watches, with that entry's reaction kind.
type mergeWatch struct {
	ReactionKind string
	PRNumber     int
	Owner, Repo  string
}

// recordIssueOutcome stores outcome under the store's record rule. A failed
// write is logged and changes nothing else; log must carry the issue.
func recordIssueOutcome(ctx context.Context, store storage.IssueOutcomeStore, outcome storage.IssueOutcome, log *slog.Logger) {
	if err := store.RecordIssueOutcome(ctx, outcome); err != nil {
		log.Error("failed to persist issue outcome",
			slog.Any("error", err),
			slog.String("outcome", outcome.Kind),
		)
	}
}

// clearIssueOutcome deletes the issue's outcome when the poll tick picks the
// issue up again. A failed delete is logged.
func clearIssueOutcome(ctx context.Context, store storage.IssueOutcomeStore, issueID string, log *slog.Logger) {
	if err := store.DeleteIssueOutcome(ctx, issueID); err != nil {
		log.Error("failed to clear issue outcome",
			slog.Any("error", err),
		)
	}
}

// handoffOutcome builds the outcome of a successful handoff transition. A
// target that is itself a terminal state records a terminal outcome, because
// no person takes that issue over.
func handoffOutcome(issueID, target string, terminalStates []string, at time.Time) storage.IssueOutcome {
	kind := storage.IssueOutcomeHandedOff
	if isTerminalState(target, terminalStates) {
		kind = storage.IssueOutcomeTerminal
	}
	return storage.IssueOutcome{
		IssueID:    issueID,
		Kind:       kind,
		ObservedAt: at.UTC().Format(time.RFC3339),
		State:      target,
	}
}

// mergedOutcome builds the outcome of a pull request observed merged by the
// reaction of kind reactionKind.
func mergedOutcome(issueID, reactionKind string, prNumber int, owner, repo string, at time.Time) storage.IssueOutcome {
	return storage.IssueOutcome{
		IssueID:           issueID,
		Kind:              storage.IssueOutcomeMerged,
		ObservedAt:        at.UTC().Format(time.RFC3339),
		MergeReactionKind: reactionKind,
		PRNumber:          prNumber,
		PRRepository:      owner + "/" + repo,
	}
}

// terminalOutcome builds the outcome of a terminal tracker state read.
func terminalOutcome(issueID, state string, at time.Time) storage.IssueOutcome {
	return storage.IssueOutcome{
		IssueID:    issueID,
		Kind:       storage.IssueOutcomeTerminal,
		ObservedAt: at.UTC().Format(time.RFC3339),
		State:      state,
	}
}

// pendingMergeWatch returns the watch of the issue's pending merge-completion
// entry, else of its pending auto-merge entry. Merge completion wins because
// it observes a merge whoever performs it.
func pendingMergeWatch(state *State, issueID string) (mergeWatch, bool) {
	if pending := state.PendingReactions[ReactionKey(issueID, ReactionKindMergeCompletion)]; pending != nil {
		if data, ok := pending.KindData.(*MergeCompletionReactionData); ok {
			return mergeWatch{ReactionKind: ReactionKindMergeCompletion, PRNumber: data.PRNumber, Owner: data.Owner, Repo: data.Repo}, true
		}
	}
	if pending := state.PendingReactions[ReactionKey(issueID, ReactionKindAutoMerge)]; pending != nil {
		if data, ok := pending.KindData.(*AutoMergeReactionData); ok {
			return mergeWatch{ReactionKind: ReactionKindAutoMerge, PRNumber: data.PRNumber, Owner: data.Owner, Repo: data.Repo}, true
		}
	}
	return mergeWatch{}, false
}

// releaseReadOutcome reads the watched pull request once and returns the
// merged outcome when the read reports it merged, the terminal outcome for
// stateName otherwise. A zero watch or a nil scm reads nothing. A failed read
// is logged and yields the terminal outcome.
//
// A forge that closes the issue on merge makes the tracker report terminal
// before any reaction observes the merge; the read keeps the pull request
// from being lost when the release deletes the entry that would have seen it.
func releaseReadOutcome(ctx context.Context, scm domain.SCMAdapter, watch mergeWatch, issueID, stateName string, at time.Time, log *slog.Logger) storage.IssueOutcome {
	if scm == nil || watch.ReactionKind == "" {
		return terminalOutcome(issueID, stateName, at)
	}
	status, err := scm.GetMergeability(ctx, watch.PRNumber, watch.Owner, watch.Repo)
	if err != nil {
		log.Warn("failed to read merge state at terminal release",
			slog.Any("error", err),
			slog.Int("pr_number", watch.PRNumber),
		)
		return terminalOutcome(issueID, stateName, at)
	}
	if status.Merged {
		return mergedOutcome(issueID, watch.ReactionKind, watch.PRNumber, watch.Owner, watch.Repo, at)
	}
	return terminalOutcome(issueID, stateName, at)
}
