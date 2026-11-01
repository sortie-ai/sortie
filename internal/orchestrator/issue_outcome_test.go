package orchestrator

import (
	"context"
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/storage"
)

type noopOutcomeRecorder struct{ storage.IssueOutcomeStore }

func (noopOutcomeRecorder) RecordIssueOutcome(context.Context, storage.IssueOutcome) error {
	return nil
}

func requireStoredOutcome(t *testing.T, store storage.IssueOutcomeStore, want storage.IssueOutcome) {
	t.Helper()
	got, err := store.IssueOutcomes(context.Background(), []string{want.IssueID})
	if err != nil {
		t.Fatalf("IssueOutcomes(%q): %v", want.IssueID, err)
	}
	if got[want.IssueID] != want {
		t.Errorf("IssueOutcomes(%q) = %+v, want %+v", want.IssueID, got[want.IssueID], want)
	}
}

func perPRSCM(statuses map[int]domain.PRMergeStatus) *controlledSCMAdapter {
	return &controlledSCMAdapter{
		getMergeabilityFn: func(_ context.Context, pr int, _, _ string) (domain.PRMergeStatus, error) {
			status, ok := statuses[pr]
			if !ok {
				return domain.PRMergeStatus{}, &domain.SCMError{Kind: domain.ErrSCMNotFound, Message: "no such pull request"}
			}
			return status, nil
		},
		mergePRFn: func(_ context.Context, pr int, _, _ string, _ domain.MergeStrategy, _, _, _ string) (domain.MergeResult, error) {
			statuses[pr] = domain.PRMergeStatus{Merged: true, MergeCommitSHA: "merge-sha"}
			return domain.MergeResult{SHA: "merge-sha"}, nil
		},
	}
}

func handoffExitParams(t *testing.T, store OrchestratorStore, handoffState string) HandleWorkerExitParams {
	t.Helper()
	params := defaultExitParams(t, nil)
	params.Store = store
	params.TrackerAdapter = &mockTrackerAdapter{}
	params.HandoffState = handoffState
	params.ActiveStates = []string{"In Progress"}
	params.TerminalStates = []string{"Done"}
	return params
}

func TestIssueOutcome_HandoffExit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		target string
		want   string
	}{
		{"handoff to a review state", "Human Review", storage.IssueOutcomeHandedOff},
		{"handoff to a terminal state", "Done", storage.IssueOutcomeTerminal},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store := openInMemoryStore(t)
			state := exitStateWithIssue(t, "OUT-1", "In Progress")

			HandleWorkerExit(state, WorkerResult{IssueID: "OUT-1", Identifier: "OUT-1-ident", ExitKind: WorkerExitNormal, AgentAdapter: "mock"}, handoffExitParams(t, store, tt.target))

			requireStoredOutcome(t, store, storage.IssueOutcome{IssueID: "OUT-1", Kind: tt.want, ObservedAt: "2026-03-15T12:01:00Z", State: tt.target})
		})
	}
}

func TestIssueOutcome_Reconcile(t *testing.T) {
	t.Parallel()

	const issueID = "OUT-2"
	merged := domain.PRMergeStatus{Merged: true, MergeCommitSHA: "merge-sha", Mergeability: domain.MergeabilityUnknown}
	open := domain.PRMergeStatus{Mergeability: domain.MergeabilityClean, HeadSHA: "head-sha"}
	mergedBy := func(kind, repo string) storage.IssueOutcome {
		return storage.IssueOutcome{IssueID: issueID, Kind: storage.IssueOutcomeMerged, ObservedAt: "2026-07-01T09:00:00Z", MergeReactionKind: kind, PRNumber: 42, PRRepository: repo}
	}
	terminal := storage.IssueOutcome{IssueID: issueID, Kind: storage.IssueOutcomeTerminal, ObservedAt: "2026-07-01T09:00:00Z", State: "Done"}

	tests := []struct {
		name            string
		trackerState    string
		mergeCompletion bool
		autoMerge       bool
		running         bool
		pr              domain.PRMergeStatus
		want            storage.IssueOutcome
	}{
		{name: "merge completion reads the merge", trackerState: "In Review", mergeCompletion: true, pr: merged, want: mergedBy(ReactionKindMergeCompletion, "owner/repo")},
		{name: "auto-merge merges the pull request", trackerState: "In Review", autoMerge: true, pr: open, want: mergedBy(ReactionKindAutoMerge, "corp/api")},
		{name: "auto-merge merges before merge completion reads it", trackerState: "In Review", autoMerge: true, mergeCompletion: true, pr: open, want: mergedBy(ReactionKindAutoMerge, "corp/api")},
		{name: "terminal issue whose merge completion pull request merged", trackerState: "Done", mergeCompletion: true, pr: merged, want: mergedBy(ReactionKindMergeCompletion, "owner/repo")},
		{name: "terminal issue whose auto-merge pull request merged", trackerState: "Done", autoMerge: true, pr: merged, want: mergedBy(ReactionKindAutoMerge, "corp/api")},
		{name: "terminal issue whose pull request is not merged", trackerState: "Done", mergeCompletion: true, pr: open, want: terminal},
		{name: "terminal issue held by a running worker", trackerState: "Done", running: true, pr: open, want: terminal},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store := openInMemoryStore(t)
			state := NewState(5000, 4, 0, nil, AgentTotals{})
			if tt.mergeCompletion {
				state.PendingReactions[ReactionKey(issueID, ReactionKindMergeCompletion)] = newMGCPending(issueID, 42)
			}
			if tt.autoMerge {
				state.PendingReactions[ReactionKey(issueID, ReactionKindAutoMerge)] = newAutoMergePending(issueID, 42)
			}
			if tt.running {
				state.Running[issueID] = &RunningEntry{Identifier: issueID + "-ident", StartedAt: mgcBaseTime}
				state.Claimed[issueID] = struct{}{}
			}
			otherPR := domain.PRMergeStatus{Mergeability: domain.MergeabilityDirty}
			if tt.want.Kind == storage.IssueOutcomeTerminal {
				otherPR = merged
			}
			params := mgcParams(store, perPRSCM(map[int]domain.PRMergeStatus{42: tt.pr, 7: otherPR}), &mgcTrackerFake{states: map[string]string{issueID: tt.trackerState}})
			params.Logger = discardLogger()
			params.AutoMergeConfig = defaultAutoMergeConfig()
			params.AutoMergeReactionConfigured = true

			ReconcileRunningIssues(state, params)

			requireStoredOutcome(t, store, tt.want)
		})
	}
}

func TestIssueOutcome_PollTickPickUpClearsIt(t *testing.T) {
	t.Parallel()

	h := newRunHarness(t, runConfig(t, stagedRulesRaw()), "kind-a", "kind-b", "kind-c")
	h.tracker.candidates = []domain.Issue{candidate("id-1", "S-1", "Stage-Plan")}
	h.build()

	h.o.handleTick(context.Background())
	h.o.state.WorkerWg.Wait()

	if want := []string{"id-1"}; !slices.Equal(h.store.clearedOutcomeIDs, want) {
		t.Errorf("DeleteIssueOutcome calls after the dispatch = %q, want %q", h.store.clearedOutcomeIDs, want)
	}
}

func TestIssueOutcome_MergedPullRequestEndsAutoMergeWatch(t *testing.T) {
	t.Parallel()

	const issueID = "OUT-9"
	store := openInMemoryStore(t)
	handedOff := storage.IssueOutcome{IssueID: issueID, Kind: storage.IssueOutcomeHandedOff, ObservedAt: "2026-02-01T00:00:00Z", State: "In Review"}
	if err := store.RecordIssueOutcome(context.Background(), handedOff); err != nil {
		t.Fatalf("RecordIssueOutcome: %v", err)
	}
	rkey := ReactionKey(issueID, ReactionKindAutoMerge)
	state := NewState(5000, 4, 0, nil, AgentTotals{})
	state.PendingReactions[rkey] = newAutoMergePending(issueID, 42)
	state.ReactionAttempts[rkey] = 2
	scm := perPRSCM(map[int]domain.PRMergeStatus{42: {Merged: true, Mergeability: domain.MergeabilityUnknown}})
	params := mgcParams(store, scm, &mgcTrackerFake{states: map[string]string{issueID: "In Review"}})
	params.Logger = discardLogger()
	params.AutoMergeConfig = defaultAutoMergeConfig()
	params.AutoMergeReactionConfigured = true
	params.MergeCompletionReactionConfigured = false

	ReconcileRunningIssues(state, params)

	requireStoredOutcome(t, store, storage.IssueOutcome{
		IssueID: issueID, Kind: storage.IssueOutcomeMerged, ObservedAt: "2026-07-01T09:00:00Z",
		MergeReactionKind: ReactionKindAutoMerge, PRNumber: 42, PRRepository: "corp/api",
	})
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions[OUT-9:merge] present after the pull request was found merged, want dropped")
	}
	if attempts, ok := state.ReactionAttempts[rkey]; ok {
		t.Errorf("ReactionAttempts[OUT-9:merge] = %d, want dropped", attempts)
	}

	wsPath := t.TempDir()
	writePRSCMMetadata(t, wsPath, 77, "corp", "api", "feature/OUT-9", "c0ffee")
	state.Running[issueID] = exitStateWithIssue(t, issueID, "In Progress").Running[issueID]
	state.Claimed[issueID] = struct{}{}
	exitParams := handoffExitParams(t, store, "In Review")
	exitParams.SCMAdapter = &scmAdapterStubExit{}
	exitParams.AutoMergeReactionConfigured = true

	HandleWorkerExit(state, WorkerResult{IssueID: issueID, Identifier: issueID + "-ident", ExitKind: WorkerExitNormal, AgentAdapter: "mock", WorkspacePath: wsPath}, exitParams)

	handedOff.ObservedAt = "2026-03-15T12:01:00Z"
	requireStoredOutcome(t, store, handedOff)
	pending, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions[OUT-9:merge] missing after the next handoff, want an entry for the new pull request")
	}
	if data, _ := pending.KindData.(*AutoMergeReactionData); data == nil || data.PRNumber != 77 {
		t.Errorf("PendingReactions[OUT-9:merge].KindData = %+v, want pull request 77", pending.KindData)
	}
}
