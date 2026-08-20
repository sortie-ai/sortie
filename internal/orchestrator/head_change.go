package orchestrator

import (
	"context"
	"time"
)

// headChangeAttribution is the verdict [classifyHeadChange] returns for an
// observed pull request head change. There is no third value: the
// predicate never names a person.
type headChangeAttribution string

const (
	// headChangeNotOurs reports that no worker session for the issue ran
	// between the recorded head and the read, so the change is
	// positively not the orchestrator's own work.
	headChangeNotOurs headChangeAttribution = "not_orchestrator"

	// headChangeUnknown reports that the change cannot be positively
	// attributed away from the orchestrator's own work. This is the
	// conservative answer on every failure path.
	headChangeUnknown headChangeAttribution = "unknown"
)

// classifyHeadChange reports whether an observed pull request head change
// is positively not the orchestrator's own work, or unknown.
//
// Three sources are consulted in order: whether pending.HeadRecordedAt
// carries a boundary this process recorded, whether the issue holds a
// live session in state.Running, state.RetryAttempts, or state.Claimed,
// and whether params.Store.CountWorkerRunsCompletedSince reports a
// worker session completed inside the interval. Every failure path,
// including a zero boundary, a live or queued session, and a store
// error, answers unknown; only the absence of all three answers
// notOurs. A positive run-history count proves only that a session
// could have produced the head, never that it did, which is why it
// answers unknown rather than a stronger claim.
//
//nolint:unparam // now completes the predicate's public signature, matching every call site (applyCIEpochTransition, handleMergeConflictDirty) and the interval the caller is reasoning about; the predicate itself bounds its query by pending.HeadRecordedAt rather than by the current instant.
func classifyHeadChange(state *State, params ReconcileParams, pending *PendingReaction, now time.Time, ctx context.Context) headChangeAttribution {
	if pending.HeadRecordedAt.IsZero() {
		return headChangeUnknown
	}

	issueID := pending.IssueID
	if state.Running[issueID] != nil {
		return headChangeUnknown
	}
	if state.RetryAttempts[issueID] != nil {
		return headChangeUnknown
	}
	if _, claimed := state.Claimed[issueID]; claimed {
		return headChangeUnknown
	}

	count, err := params.Store.CountWorkerRunsCompletedSince(ctx, issueID, pending.HeadRecordedAt)
	if err != nil {
		return headChangeUnknown
	}
	if count > 0 {
		return headChangeUnknown
	}

	return headChangeNotOurs
}
