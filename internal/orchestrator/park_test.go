package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
)

// The release rule: a state change unparks, a confirmed label removed
// unparks, an unconfirmed label absent stays parked, and a label observed
// while unconfirmed sets and persists the confirmation flag.

func TestObserveParkedStateReleaseRule(t *testing.T) {
	t.Parallel()

	t.Run("state change unparks with trigger state_changed", func(t *testing.T) {
		t.Parallel()

		store := &stubStore{}
		state := NewState(1000, 1, nil, AgentTotals{})
		entry := &ParkedEntry{Identifier: "PROJ-1", Reason: parkReasonAgentBlocked, ParkedState: "In Progress"}
		state.Parked["ISS-1"] = entry

		stillParked := observeParkedState(context.Background(), state, store, discardLogger(), entry, "ISS-1", "Done")

		if stillParked {
			t.Error("observeParkedState(...) = true, want false after a state change")
		}
		if _, ok := state.Parked["ISS-1"]; ok {
			t.Error("issue remains in state.Parked after an observed state change")
		}
		if len(store.deletedParkedIDs) != 1 || store.deletedParkedIDs[0] != "ISS-1" {
			t.Errorf("DeleteParkedIssue calls = %v, want [ISS-1]", store.deletedParkedIDs)
		}
	})

	t.Run("empty parked_state is backfilled, not released", func(t *testing.T) {
		t.Parallel()

		store := &stubStore{}
		state := NewState(1000, 1, nil, AgentTotals{})
		entry := &ParkedEntry{Identifier: "PROJ-2", Reason: parkReasonAgentBlocked}
		state.Parked["ISS-2"] = entry

		stillParked := observeParkedState(context.Background(), state, store, discardLogger(), entry, "ISS-2", "In Progress")

		if !stillParked {
			t.Error("observeParkedState(...) = false, want true: an empty parked_state backfills rather than releases")
		}
		if entry.ParkedState != "In Progress" {
			t.Errorf("ParkedState after backfill = %q, want %q", entry.ParkedState, "In Progress")
		}
		if _, ok := state.Parked["ISS-2"]; !ok {
			t.Error("issue was released by the backfill observation")
		}
		if len(store.deletedParkedIDs) != 0 {
			t.Errorf("DeleteParkedIssue calls = %v, want none", store.deletedParkedIDs)
		}
	})
}

func TestObserveParkedLabelsReleaseRule(t *testing.T) {
	t.Parallel()

	t.Run("label observed while unconfirmed sets and persists LabelApplied", func(t *testing.T) {
		t.Parallel()

		store := &stubStore{}
		state := NewState(1000, 1, nil, AgentTotals{})
		entry := &ParkedEntry{Identifier: "PROJ-3", Reason: parkReasonAgentBlocked, Label: "needs-human"}
		state.Parked["ISS-3"] = entry

		observeParkedLabels(context.Background(), state, store, discardLogger(), entry, "ISS-3", []string{"Needs-Human"})

		if !entry.LabelApplied {
			t.Error("LabelApplied = false, want true after observing the parking label")
		}
		if len(store.labelAppliedIDs) != 1 || store.labelAppliedIDs[0] != "ISS-3" {
			t.Errorf("MarkParkedIssueLabelApplied calls = %v, want [ISS-3]", store.labelAppliedIDs)
		}
		if _, ok := state.Parked["ISS-3"]; !ok {
			t.Error("label confirmation released the park, want it to stay")
		}
	})

	t.Run("confirmed label observed absent unparks with trigger label_removed", func(t *testing.T) {
		t.Parallel()

		store := &stubStore{}
		state := NewState(1000, 1, nil, AgentTotals{})
		entry := &ParkedEntry{Identifier: "PROJ-4", Reason: parkReasonAgentBlocked, Label: "needs-human", LabelApplied: true}
		state.Parked["ISS-4"] = entry

		observeParkedLabels(context.Background(), state, store, discardLogger(), entry, "ISS-4", nil)

		if _, ok := state.Parked["ISS-4"]; ok {
			t.Error("issue remains in state.Parked after a confirmed label was observed removed")
		}
		if len(store.deletedParkedIDs) != 1 || store.deletedParkedIDs[0] != "ISS-4" {
			t.Errorf("DeleteParkedIssue calls = %v, want [ISS-4]", store.deletedParkedIDs)
		}
	})

	t.Run("unconfirmed label observed absent does not unpark", func(t *testing.T) {
		t.Parallel()

		store := &stubStore{}
		state := NewState(1000, 1, nil, AgentTotals{})
		entry := &ParkedEntry{Identifier: "PROJ-5", Reason: parkReasonAgentBlocked, Label: "needs-human"}
		state.Parked["ISS-5"] = entry

		observeParkedLabels(context.Background(), state, store, discardLogger(), entry, "ISS-5", nil)

		if _, ok := state.Parked["ISS-5"]; !ok {
			t.Error("unconfirmed label absence released the park, want it held")
		}
		if len(store.deletedParkedIDs) != 0 {
			t.Errorf("DeleteParkedIssue calls = %v, want none", store.deletedParkedIDs)
		}
	})
}

// A failed label write must not cost the park: it stays in place with the
// label unconfirmed, and a later release pass that still observes the
// label absent leaves it exactly where a failed write left it.

func TestParkIssueFailedLabelWriteLeavesLabelUnconfirmed(t *testing.T) {
	t.Parallel()

	const issueID = "PARK-LABEL-FAIL"
	store := &stubStore{}
	tracker := newRecordingHandoffTracker()
	tracker.addLabelErr = errors.New("label service unavailable")
	state := NewState(1000, 1, nil, AgentTotals{})
	var logs bytes.Buffer

	parkIssue(state, parkIssueParams{
		IssueID:        issueID,
		Identifier:     "PROJ-LABEL-FAIL",
		ObservedState:  "In Progress",
		Reason:         parkReasonAgentBlocked,
		Label:          "needs-human",
		Store:          store,
		TrackerAdapter: tracker,
		Metrics:        &domain.NoopMetrics{},
		Logger:         debugLogger(t, &logs),
		Ctx:            context.Background(),
	})
	state.TrackerOpsWg.Wait()

	entry := state.Parked[issueID]
	if entry == nil {
		t.Fatal("issue not parked")
	}
	if entry.LabelApplied {
		t.Error("LabelApplied = true after a failed label write, want false")
	}
	if !strings.Contains(logs.String(), "park label write failed") {
		t.Errorf("missing label failure log\nlogs: %s", logs.String())
	}

	observeParkedLabels(context.Background(), state, store, discardLogger(), entry, issueID, nil)

	if _, ok := state.Parked[issueID]; !ok {
		t.Error("release pass unparked an issue whose label write failed, want it held")
	}
	if entry.LabelApplied {
		t.Error("LabelApplied became true after the release pass, want it to stay false")
	}
}

// A successful AddLabel write is not, by itself, confirmation: only a
// later observation of the label on a tracker read sets LabelApplied. A
// tracker whose AddLabel reports success without the label ever becoming
// visible on a read must not release the park it never confirmed.

func TestParkIssueSuccessfulLabelWriteDoesNotConfirmWithoutObservation(t *testing.T) {
	t.Parallel()

	const issueID = "PARK-LABEL-UNCONFIRMED"
	store := &stubStore{}
	tracker := newRecordingHandoffTracker() // AddLabel returns nil by default.

	state := NewState(1000, 1, nil, AgentTotals{})

	parkIssue(state, parkIssueParams{
		IssueID:        issueID,
		Identifier:     "PROJ-UNCONFIRMED",
		ObservedState:  "In Progress",
		Reason:         parkReasonAgentBlocked,
		Label:          "needs-human",
		Store:          store,
		TrackerAdapter: tracker,
		Metrics:        &domain.NoopMetrics{},
		Logger:         discardLogger(),
		Ctx:            context.Background(),
	})
	state.TrackerOpsWg.Wait()

	if calls := tracker.labels(); len(calls) != 1 {
		t.Fatalf("AddLabel calls = %+v, want exactly one successful write", calls)
	}

	entry := state.Parked[issueID]
	if entry == nil {
		t.Fatal("issue not parked")
	}
	if entry.LabelApplied {
		t.Error("LabelApplied = true immediately after a successful write, want false: confirmation comes from observation, not the write result")
	}
	if len(store.labelAppliedIDs) != 0 {
		t.Errorf("MarkParkedIssueLabelApplied calls = %v, want none before any observation", store.labelAppliedIDs)
	}

	observeParkedLabels(context.Background(), state, store, discardLogger(), entry, issueID, nil)

	if entry.LabelApplied {
		t.Error("LabelApplied became true after a read carrying no label")
	}
	if _, ok := state.Parked[issueID]; !ok {
		t.Error("issue was unparked despite label_applied never having been confirmed")
	}
	if len(store.labelAppliedIDs) != 0 {
		t.Errorf("MarkParkedIssueLabelApplied calls = %v, want none", store.labelAppliedIDs)
	}
}

// A park recorded through a store opened on a temporary file survives a
// simulated restart: the in-memory State is discarded and rebuilt purely
// from the durable rows, and the rebuilt state still refuses dispatch.

func TestParkSurvivesRestart(t *testing.T) {
	t.Parallel()

	const issueID = "PARK-RESTART"
	ctx := context.Background()
	dbPath := t.TempDir() + "/test.db"
	store, err := persistence.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("persistence.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}

	state := exitStateWithIssue(t, issueID, "In Progress")
	params := HandleWorkerExitParams{
		Store:               store,
		MaxRetryBackoffMS:   300_000,
		OnRetryFire:         noopRetryFire,
		NowFunc:             func() time.Time { return baseTime.Add(60 * time.Second) },
		Logger:              discardLogger(),
		HandoffParkingLabel: "needs-human",
	}

	HandleWorkerExit(state, WorkerResult{
		IssueID:        issueID,
		Identifier:     "PROJ-RESTART",
		ExitKind:       WorkerExitNormal,
		AgentAdapter:   "mock",
		SoftStop:       true,
		SoftStopReason: "blocked",
	}, params)
	state.TrackerOpsWg.Wait()

	if _, ok := state.Parked[issueID]; !ok {
		t.Fatal("issue not parked before the simulated restart")
	}

	rows, err := store.ListParkedIssues(ctx)
	if err != nil {
		t.Fatalf("ListParkedIssues: %v", err)
	}

	fresh := NewState(1000, 4, nil, AgentTotals{})
	PopulateParked(fresh, rows, discardLogger())

	if _, ok := fresh.Parked[issueID]; !ok {
		t.Fatal("PopulateParked did not restore the park after the restart")
	}

	candidate := domain.Issue{ID: issueID, Identifier: "PROJ-RESTART", Title: "T", State: "In Progress"}
	activeSet := stateSet([]string{"In Progress"})
	terminalSet := stateSet([]string{"Done"})

	if ShouldDispatchWithSets(candidate, fresh, activeSet, terminalSet) {
		t.Error("ShouldDispatchWithSets dispatched an issue whose park was restored across a restart")
	}
}

// The release pass reads the state of every parked issue the tick's
// candidate slice does not carry, through one batched tracker call, and
// applies the state rule to whatever it returns.

func TestRefreshParkedIssuesNonCandidateStateRead(t *testing.T) {
	t.Parallel()

	t.Run("state differs from parked_state unparks with trigger state_changed", func(t *testing.T) {
		t.Parallel()

		const parkedID, candidateID = "MISS-1", "CAND-1"
		state := NewState(1000, 1, nil, AgentTotals{})
		state.Parked[parkedID] = &ParkedEntry{Identifier: "PROJ-MISS-1", Reason: parkReasonAgentBlocked, ParkedState: "In Progress"}

		var fetchedIDs []string
		tracker := &mockTrackerAdapter{
			fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
				fetchedIDs = append([]string(nil), ids...)
				return map[string]string{parkedID: "Done"}, nil
			},
		}
		store := &stubStore{}
		orchestrator := NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          discardLogger(),
			TrackerAdapter:  tracker,
			AgentAdapter:    &mockAgentAdapter{},
			WorkflowManager: &stubWorkflowManager{config: config.ServiceConfig{}},
			Store:           store,
		})
		candidates := []domain.Issue{{ID: candidateID, Identifier: "PROJ-CAND-1", Title: "T", State: "To Do"}}

		orchestrator.refreshParkedIssues(context.Background(), candidates)

		// The regression this guards against: a read that fetches every
		// parked issue on every tick instead of only the ones the
		// candidate slice did not carry.
		if !slices.Equal(fetchedIDs, []string{parkedID}) {
			t.Errorf("FetchIssueStatesByIDs called with %v, want [%s] only", fetchedIDs, parkedID)
		}
		if _, ok := state.Parked[parkedID]; ok {
			t.Error("issue remains parked after the non-candidate read reported a different state")
		}
	})

	t.Run("same state leaves the park", func(t *testing.T) {
		t.Parallel()

		const parkedID = "MISS-2"
		state := NewState(1000, 1, nil, AgentTotals{})
		state.Parked[parkedID] = &ParkedEntry{Identifier: "PROJ-MISS-2", Reason: parkReasonAgentBlocked, ParkedState: "In Progress"}
		tracker := &mockTrackerAdapter{
			fetchStatesFn: func(_ context.Context, _ []string) (map[string]string, error) {
				return map[string]string{parkedID: "In Progress"}, nil
			},
		}
		store := &stubStore{}
		orchestrator := NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          discardLogger(),
			TrackerAdapter:  tracker,
			AgentAdapter:    &mockAgentAdapter{},
			WorkflowManager: &stubWorkflowManager{config: config.ServiceConfig{}},
			Store:           store,
		})

		orchestrator.refreshParkedIssues(context.Background(), nil)

		if _, ok := state.Parked[parkedID]; !ok {
			t.Error("issue was unparked despite an unchanged observed state")
		}
		if len(store.deletedParkedIDs) != 0 {
			t.Errorf("DeleteParkedIssue calls = %v, want none", store.deletedParkedIDs)
		}
	})

	t.Run("issue omitted from the returned map leaves the park", func(t *testing.T) {
		t.Parallel()

		const parkedID = "MISS-3"
		state := NewState(1000, 1, nil, AgentTotals{})
		state.Parked[parkedID] = &ParkedEntry{Identifier: "PROJ-MISS-3", Reason: parkReasonAgentBlocked, ParkedState: "In Progress"}
		tracker := &mockTrackerAdapter{
			fetchStatesFn: func(_ context.Context, _ []string) (map[string]string, error) {
				return map[string]string{}, nil
			},
		}
		store := &stubStore{}
		orchestrator := NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          discardLogger(),
			TrackerAdapter:  tracker,
			AgentAdapter:    &mockAgentAdapter{},
			WorkflowManager: &stubWorkflowManager{config: config.ServiceConfig{}},
			Store:           store,
		})

		orchestrator.refreshParkedIssues(context.Background(), nil)

		if _, ok := state.Parked[parkedID]; !ok {
			t.Error("issue was unparked despite being omitted from the state-read response")
		}
	})

	t.Run("read error leaves every park in place and logs at warn", func(t *testing.T) {
		t.Parallel()

		const parkedID1, parkedID2 = "ERR-1", "ERR-2"
		state := NewState(1000, 1, nil, AgentTotals{})
		state.Parked[parkedID1] = &ParkedEntry{Identifier: "PROJ-ERR-1", Reason: parkReasonAgentBlocked, ParkedState: "In Progress"}
		state.Parked[parkedID2] = &ParkedEntry{Identifier: "PROJ-ERR-2", Reason: parkReasonHandoffAbsence, ParkedState: "In Progress"}
		tracker := &mockTrackerAdapter{
			fetchStatesFn: func(context.Context, []string) (map[string]string, error) {
				return nil, errors.New("tracker unavailable")
			},
		}
		store := &stubStore{}
		var logs bytes.Buffer
		orchestrator := NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          debugLogger(t, &logs),
			TrackerAdapter:  tracker,
			AgentAdapter:    &mockAgentAdapter{},
			WorkflowManager: &stubWorkflowManager{config: config.ServiceConfig{}},
			Store:           store,
		})

		orchestrator.refreshParkedIssues(context.Background(), nil)

		if _, ok := state.Parked[parkedID1]; !ok {
			t.Error("park released after a failed non-candidate state read")
		}
		if _, ok := state.Parked[parkedID2]; !ok {
			t.Error("park released after a failed non-candidate state read")
		}
		if !strings.Contains(logs.String(), "parked issue state read failed, retaining parks") {
			t.Errorf("missing read-failure warning\nlogs: %s", logs.String())
		}
	})
}
