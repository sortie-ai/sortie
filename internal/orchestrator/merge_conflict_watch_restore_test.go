package orchestrator

import (
	"errors"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/workspace"
)

// TestHandleWorkerExit_RestoresMergeConflictWatch covers the exits of a
// rebase continuation that neither schedule a retry nor seed a watch from
// the workspace: the watch the dispatch consumed is put back, or a later
// conflict on the pull request goes unnoticed until a restart.
func TestHandleWorkerExit_RestoresMergeConflictWatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		result           WorkerResult
		issueState       string
		observedTerminal string
		wantWatch        bool
		wantRetried      bool
	}{
		{name: "cancelled with no retry", result: WorkerResult{ExitKind: WorkerExitCancelled}, issueState: "queue-pr", wantWatch: true},
		{name: "normal exit without pull request metadata", result: WorkerResult{ExitKind: WorkerExitNormal}, issueState: "queue-pr", wantWatch: true},
		{name: "error exit retries instead", result: WorkerResult{ExitKind: WorkerExitError, Error: errors.New("agent crashed")}, issueState: "queue-pr", wantRetried: true},
		{name: "terminal issue", result: WorkerResult{ExitKind: WorkerExitNormal}, issueState: "done"},
		{name: "cancelled after reconciliation saw the issue terminal", result: WorkerResult{ExitKind: WorkerExitCancelled}, issueState: "queue-pr", observedTerminal: "done"},
		{name: "blocked stop parks", result: WorkerResult{ExitKind: WorkerExitNormal, SoftStop: true, SoftStopReason: string(workspace.StatusBlocked)}, issueState: "queue-pr"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			const id = "MCW-1"
			state := exitStateWithIssue(t, id, tt.issueState)
			state.Running[id].ReactionKind = ReactionKindMergeConflict
			state.Running[id].resumeWatch = newMergeConflictPending(id, 77)
			state.Running[id].ObservedTerminalState = tt.observedTerminal

			params := defaultExitParams(t, &mockExitStore{})
			params.TrackerAdapter = &mockTrackerAdapter{}
			params.HandoffState = "queue-pr"
			params.ActiveStates = []string{"queue"}
			params.TerminalStates = []string{"done"}
			params.SCMAdapter = &scmAdapterStubExit{}
			params.MergeConflictReactionConfigured = true

			result := tt.result
			result.IssueID, result.Identifier, result.AgentAdapter = id, id+"-ident", "mock"
			HandleWorkerExit(state, result, params)
			state.TrackerOpsWg.Wait()

			watch, watched := state.PendingReactions[ReactionKey(id, ReactionKindMergeConflict)]
			if watched != tt.wantWatch {
				t.Fatalf("merge-conflict watch present = %v, want %v", watched, tt.wantWatch)
			}
			if watched {
				data, ok := watch.KindData.(*MergeConflictReactionData)
				if !ok || data.PRNumber != 77 {
					t.Errorf("restored watch KindData = %#v, want the pull request it was dispatched for", watch.KindData)
				}
			}
			retry, retried := state.RetryAttempts[id]
			if retried != tt.wantRetried {
				t.Errorf("retry scheduled = %v, want %v", retried, tt.wantRetried)
			}
			if retried {
				retry.TimerHandle.Stop()
				if retry.resumeWatch == nil {
					t.Error("retry of a rebase run carries no watch to restore, want the dispatching one")
				}
			}
		})
	}
}

// TestHandleRetryTimer_DroppedRebaseLeavesNoWatch covers the retry-timer
// arms that drop a rebase retry because the issue left the handoff state
// without going terminal: none puts the watch back, so the next pass has
// nothing to schedule and the cycle of schedule, drop, and restore never
// starts. Startup recovery watches the pull request again once the issue is
// back in the handoff state.
func TestHandleRetryTimer_DroppedRebaseLeavesNoWatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		prepare func(entry *RetryEntry, tracker *mockRetryTracker)
	}{
		{name: "no state label twice", prepare: func(entry *RetryEntry, tracker *mockRetryTracker) {
			entry.noStateObserved = true
			tracker.fetchedIssue = candidateIssue("MCW-2", "MCW-2", "")
		}},
		{name: "paused past the maximum dwell", prepare: func(entry *RetryEntry, tracker *mockRetryTracker) {
			entry.pausedSinceMS = time.Now().Add(-2 * pausedRetryMaxDwell).UnixMilli()
			tracker.fetchedIssue = candidateIssue("MCW-2", "MCW-2", "Backlog")
		}},
		{name: "issue missing a required field", prepare: func(_ *RetryEntry, tracker *mockRetryTracker) {
			tracker.fetchedIssue = domain.Issue{ID: "MCW-2", Identifier: "MCW-2", State: "queue-pr"}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			const id = "MCW-2"
			state := retryState(t, id, id, 2)
			entry := state.RetryAttempts[id]
			entry.ReactionKind = ReactionKindMergeConflict
			entry.resumeWatch = newMergeConflictPending(id, 78)
			tracker := &mockRetryTracker{}
			tt.prepare(entry, tracker)

			params := defaultRetryParams(t, &mockRetryStore{}, tracker)
			params.HandoffState = "queue-pr"
			HandleRetryTimer(state, id, params)

			if retry, ok := state.RetryAttempts[id]; ok {
				retry.TimerHandle.Stop()
				t.Fatalf("retry still scheduled, want it dropped")
			}
			if _, ok := state.PendingReactions[ReactionKey(id, ReactionKindMergeConflict)]; ok {
				t.Error("merge-conflict watch put back after the retry was dropped, want none")
			}
			if _, claimed := state.Claimed[id]; claimed {
				t.Error("claim kept after the retry was dropped, want released")
			}
		})
	}
}

func TestRestoreMergeConflictWatch_KeepsExistingState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		prepare func(state *State, id string)
	}{
		{name: "parked", prepare: func(state *State, id string) { state.Parked[id] = &ParkedEntry{} }},
		{name: "already watched", prepare: func(state *State, id string) {
			state.PendingReactions[ReactionKey(id, ReactionKindMergeConflict)] = &PendingReaction{IssueID: id, Kind: ReactionKindMergeConflict}
		}},
		{name: "retry waiting", prepare: func(state *State, id string) { state.RetryAttempts[id] = &RetryEntry{IssueID: id} }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			const id = "MCW-3"
			state := NewState(5000, 4, 0, nil, AgentTotals{})
			tt.prepare(state, id)
			before := state.PendingReactions[ReactionKey(id, ReactionKindMergeConflict)]

			restoreMergeConflictWatch(state, id, newMergeConflictPending(id, 79), time.Now(), "test", discardLogger())

			if got := state.PendingReactions[ReactionKey(id, ReactionKindMergeConflict)]; got != before {
				t.Errorf("watch = %+v, want unchanged %+v", got, before)
			}
		})
	}
}
