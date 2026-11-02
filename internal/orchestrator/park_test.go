package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/notify/route"
	"github.com/sortie-ai/sortie/internal/storage"
	"github.com/sortie-ai/sortie/internal/storage/sqlite"
	"github.com/sortie-ai/sortie/internal/waittest"
	"github.com/sortie-ai/sortie/internal/workspace"
)

func assertReleaseKept(t *testing.T, store *stubStore, wantGesture string) {
	t.Helper()
	var gestures []string
	for _, row := range store.parkedIssues {
		if row.ReleasedAt != "" {
			gestures = append(gestures, row.ReleaseGesture)
		}
	}
	if !slices.Equal(gestures, []string{wantGesture}) || len(store.deletedParkedIDs) != 0 {
		t.Errorf("released rows upserted = %v, DeleteParkedIssue calls = %v, want [%s] and none: a person's release keeps the row", gestures, store.deletedParkedIDs, wantGesture)
	}
}

func TestObserveParkedStateReleaseRule(t *testing.T) {
	t.Parallel()

	t.Run("state change unparks with trigger state_changed", func(t *testing.T) {
		t.Parallel()

		store := &stubStore{}
		state := NewState(1000, 1, 0, nil, AgentTotals{})
		entry := &ParkedEntry{Identifier: "PROJ-1", Reason: parkReasonAgentBlocked, ParkedState: "In Progress"}
		state.Parked["ISS-1"] = entry

		stillParked := observeParkedState(context.Background(), state, store, discardLogger(), entry, "ISS-1", "Done")

		if stillParked {
			t.Error("observeParkedState(...) = true, want false after a state change")
		}
		if _, ok := state.Parked["ISS-1"]; ok {
			t.Error("issue remains in state.Parked after an observed state change")
		}
		assertReleaseKept(t, store, unparkTriggerStateChanged)
	})

	t.Run("empty parked_state is backfilled, not released", func(t *testing.T) {
		t.Parallel()

		store := &stubStore{}
		state := NewState(1000, 1, 0, nil, AgentTotals{})
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
		state := NewState(1000, 1, 0, nil, AgentTotals{})
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
		state := NewState(1000, 1, 0, nil, AgentTotals{})
		entry := &ParkedEntry{Identifier: "PROJ-4", Reason: parkReasonAgentBlocked, Label: "needs-human", LabelApplied: true}
		state.Parked["ISS-4"] = entry

		observeParkedLabels(context.Background(), state, store, discardLogger(), entry, "ISS-4", nil)

		if _, ok := state.Parked["ISS-4"]; ok {
			t.Error("issue remains in state.Parked after a confirmed label was observed removed")
		}
		assertReleaseKept(t, store, unparkTriggerLabelRemoved)
	})

	t.Run("unconfirmed label observed absent does not unpark", func(t *testing.T) {
		t.Parallel()

		store := &stubStore{}
		state := NewState(1000, 1, 0, nil, AgentTotals{})
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

func TestParkIssueFailedLabelWriteLeavesLabelUnconfirmed(t *testing.T) {
	t.Parallel()

	const issueID = "PARK-LABEL-FAIL"
	store := &stubStore{}
	tracker := newRecordingHandoffTracker()
	tracker.addLabelErr = errors.New("label service unavailable")
	state := NewState(1000, 1, 0, nil, AgentTotals{})
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

func TestParkIssueSuccessfulLabelWriteDoesNotConfirmWithoutObservation(t *testing.T) {
	t.Parallel()

	const issueID = "PARK-LABEL-UNCONFIRMED"
	store := &stubStore{}
	tracker := newRecordingHandoffTracker() // AddLabel returns nil by default.

	state := NewState(1000, 1, 0, nil, AgentTotals{})

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

func openParkStore(t *testing.T) *sqlite.Store {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}
	return store
}

func TestParkSurvivesRestart(t *testing.T) {
	t.Parallel()

	const issueID = "PARK-RESTART"
	ctx := context.Background()
	store := openParkStore(t)

	dispatchedAt := baseTime.Add(-time.Hour)
	state := exitStateWithIssue(t, issueID, "In Progress")
	state.StageChains, state.HopCeiling = [][]string{{"triage", "implement"}}, 4
	running := state.Running[issueID]
	running.Issue.Title = "Restart title"
	running.Issue.URL = "https://tracker.test/PROJ-RESTART"
	running.Issue.IssueType = "Story"
	running.Issue.Labels = []string{"feature", "Needs-Human", "stage-implement"}
	running.RuleName = "implement"
	running.StageLineage = StageLineage{PreviousRule: "triage", PreviousOutcome: "succeeded", ChainID: "chain-1", DispatchedAt: dispatchedAt}
	running.AgentKind, running.ConfiguredModel = "claude-code", "sonnet"
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
	state.Parked[issueID].LabelApplied = true
	if err := store.MarkParkedIssueLabelApplied(ctx, issueID); err != nil {
		t.Fatalf("MarkParkedIssueLabelApplied: %v", err)
	}

	rows, err := store.ListParkedIssues(ctx)
	if err != nil {
		t.Fatalf("ListParkedIssues: %v", err)
	}

	fresh := NewState(1000, 4, 0, nil, AgentTotals{})
	PopulateParked(fresh, rows, discardLogger())

	if _, ok := fresh.Parked[issueID]; !ok {
		t.Fatal("PopulateParked did not restore the park after the restart")
	}
	fresh.StageChains, fresh.HopCeiling = state.StageChains, state.HopCeiling

	want := SnapshotParkedEntry{
		IssueID: issueID, Identifier: "PROJ-RESTART", IssueTitle: "Restart title", IssueURL: "https://tracker.test/PROJ-RESTART",
		IssueType: "Story", IssueLabels: []string{"feature", "stage-implement"}, DispatchedAt: dispatchedAt,
		Reason: parkReasonAgentBlocked, ParkedState: "In Progress", Label: "needs-human", LabelApplied: true,
		RuleName: "implement", AgentKind: "claude-code", ConfiguredModel: "sonnet", ChainID: "chain-1",
		StagePosition: &SnapshotStagePosition{Chain: []string{"triage", "implement"}, Current: "implement", HopCeiling: 4},
	}
	parkedAt := state.Parked[issueID].ParkedAt
	for name, snapshot := range map[string]struct {
		state    *State
		parkedAt time.Time
	}{
		"before the restart": {state, parkedAt},
		"after the restart":  {fresh, parkedAt.UTC().Truncate(time.Second)},
	} {
		want.ParkedAt = snapshot.parkedAt
		if got := RuntimeSnapshot(snapshot.state, baseTime).Parked; len(got) != 1 || !reflect.DeepEqual(got[0], want) {
			t.Errorf("park snapshot %s = %+v, want %+v", name, got, want)
		}
	}

	candidate := domain.Issue{ID: issueID, Identifier: "PROJ-RESTART", Title: "T", State: "In Progress"}
	activeSet := stateSet([]string{"In Progress"})
	terminalSet := stateSet([]string{"Done"})

	if ShouldDispatchWithSets(candidate, fresh, activeSet, terminalSet) {
		t.Error("ShouldDispatchWithSets dispatched an issue whose park was restored across a restart")
	}
}

func newParkedOrchestrator(state *State, tracker domain.TrackerAdapter) *Orchestrator {
	return NewOrchestrator(OrchestratorParams{
		State:           state,
		Logger:          discardLogger(),
		TrackerAdapter:  tracker,
		AgentAdapter:    &mockAgentAdapter{},
		WorkflowManager: &stubWorkflowManager{config: config.ServiceConfig{}},
		Store:           &stubStore{},
	})
}

func TestRefreshParkedIssuesNonCandidateStateRead(t *testing.T) {
	t.Parallel()

	const parkedID, candidateID = "MISS-1", "CAND-1"
	candidates := []domain.Issue{{ID: candidateID, Identifier: "PROJ-CAND-1", Title: "T", State: "To Do"}}
	confirmed := func() *ParkedEntry {
		return &ParkedEntry{Identifier: "PROJ-MISS-1", Reason: parkReasonAgentBlocked, ParkedState: "In Progress", Label: "needs-human", LabelApplied: true}
	}

	tests := []struct {
		name       string
		reads      map[string]domain.IssueStateAndLabels
		err        error
		wantParked bool
	}{
		{
			name:  "state differs from parked_state unparks",
			reads: map[string]domain.IssueStateAndLabels{parkedID: {State: "Done", Labels: []string{"needs-human"}}},
		},
		{
			name:       "same state and label leaves the park",
			reads:      map[string]domain.IssueStateAndLabels{parkedID: {State: "In Progress", Labels: []string{"needs-human"}}},
			wantParked: true,
		},
		{
			name:       "issue omitted from the returned map leaves the park",
			reads:      map[string]domain.IssueStateAndLabels{},
			wantParked: true,
		},
		{
			name:       "read error leaves the park in place",
			err:        errors.New("tracker unavailable"),
			wantParked: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := NewState(1000, 1, 0, nil, AgentTotals{})
			state.Parked[parkedID] = confirmed()
			var fetchedIDs []string
			tracker := &mockTrackerAdapter{
				fetchStatesAndLabelsFn: func(_ context.Context, ids []string) (map[string]domain.IssueStateAndLabels, error) {
					fetchedIDs = slices.Clone(ids)
					return tt.reads, tt.err
				},
			}

			newParkedOrchestrator(state, tracker).refreshParkedIssues(context.Background(), candidates, nil)

			if !slices.Equal(fetchedIDs, []string{parkedID}) {
				t.Errorf("FetchIssueStatesAndLabelsByIDs called with %v, want [%s] only", fetchedIDs, parkedID)
			}
			if _, parked := state.Parked[parkedID]; parked != tt.wantParked {
				t.Errorf("parked after refreshParkedIssues = %v, want %v", parked, tt.wantParked)
			}
		})
	}

	t.Run("parking label is observed on a read of an issue the candidates omit", func(t *testing.T) {
		t.Parallel()

		state := NewState(1000, 1, 0, nil, AgentTotals{})
		entry := &ParkedEntry{Identifier: "PROJ-MISS-1", Reason: parkReasonAgentBlocked, ParkedState: "In Progress", Label: "needs-human"}
		state.Parked[parkedID] = entry
		var labels []string
		tracker := &mockTrackerAdapter{
			fetchStatesFn: func(context.Context, []string) (map[string]string, error) {
				return map[string]string{parkedID: "In Progress"}, nil
			},
			fetchStatesAndLabelsFn: func(context.Context, []string) (map[string]domain.IssueStateAndLabels, error) {
				return map[string]domain.IssueStateAndLabels{parkedID: {State: "In Progress", Labels: labels}}, nil
			},
		}
		orchestrator := newParkedOrchestrator(state, tracker)

		labels = []string{"needs-human"}
		orchestrator.refreshParkedIssues(context.Background(), candidates, nil)

		if state.Parked[parkedID] != entry || !entry.LabelApplied {
			t.Fatalf("after the read carrying the label: parked = %v, LabelApplied = %v, want true, true", state.Parked[parkedID] == entry, entry.LabelApplied)
		}

		labels = nil
		orchestrator.refreshParkedIssues(context.Background(), candidates, nil)

		if _, parked := state.Parked[parkedID]; parked {
			t.Error("park remains after a read of the confirmed issue without its label")
		}
	})
}

func sameStoredIdentity(a, b IssueIdentity) bool {
	return a.Title == b.Title && a.URL == b.URL && a.IssueType == b.IssueType && slices.Equal(a.Labels, b.Labels)
}

func TestRefreshParkedIssuesIdentityBackfill(t *testing.T) {
	t.Parallel()

	const id = "BACKFILL-1"
	held := IssueIdentity{Title: "held title", URL: "https://tracker.test/held"}
	read := domain.Issue{ID: id, Title: "read title", URL: "https://tracker.test/read"}
	readIdentity := IssueIdentity{Title: read.Title, URL: read.URL}

	tests := []struct {
		name         string
		held         IssueIdentity
		parkLabel    string
		candidates   []domain.Issue
		observed     string
		readErr      error
		wantIdentity IssueIdentity
		wantParked   bool
		wantReleased bool
		wantReads    []string
		wantRows     []IssueIdentity
	}{
		{
			name:         "candidate listing the issue supplies the identity",
			candidates:   []domain.Issue{{ID: id, Title: read.Title, URL: read.URL, State: "In Progress"}},
			observed:     "In Progress",
			wantIdentity: readIdentity,
			wantParked:   true,
			wantRows:     []IssueIdentity{readIdentity},
		},
		{
			name:         "partial read takes the candidate's type and labels without the parking label",
			held:         held,
			parkLabel:    "Needs-Human",
			candidates:   []domain.Issue{{ID: id, Title: read.Title, URL: read.URL, State: "In Progress", IssueType: "Story", Labels: []string{"feature", "needs-human"}}},
			observed:     "In Progress",
			wantIdentity: IssueIdentity{Title: held.Title, URL: held.URL, IssueType: "Story", Labels: []string{"feature"}},
			wantParked:   true,
			wantRows:     []IssueIdentity{{Title: held.Title, URL: held.URL, IssueType: "Story", Labels: []string{"feature"}}},
		},
		{
			name:         "issue the candidates omit takes one single-issue read",
			observed:     "In Progress",
			wantIdentity: readIdentity,
			wantParked:   true,
			wantReads:    []string{id},
			wantRows:     []IssueIdentity{readIdentity},
		},
		{
			name:         "held identity is kept and not read again",
			held:         held,
			observed:     "In Progress",
			wantIdentity: held,
			wantParked:   true,
		},
		{
			name:         "park released by the state rule is not read and is written only as a release",
			observed:     "Done",
			wantReleased: true,
			wantRows:     []IssueIdentity{{}},
		},
		{
			name:       "failed single-issue read leaves the park as it was",
			observed:   "In Progress",
			readErr:    errors.New("tracker unavailable"),
			wantParked: true,
			wantReads:  []string{id},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := NewState(1000, 1, 0, nil, AgentTotals{})
			state.Parked[id] = &ParkedEntry{
				Identifier:    "PROJ-BACKFILL-1",
				Reason:        parkReasonAgentBlocked,
				ParkedState:   "In Progress",
				Label:         tt.parkLabel,
				IssueIdentity: tt.held,
			}
			tracker := &mockTrackerAdapter{
				fetchStatesFn: func(context.Context, []string) (map[string]string, error) {
					return map[string]string{id: tt.observed}, nil
				},
				fetchIssueByIDFn: func(context.Context, string) (domain.Issue, error) {
					return read, tt.readErr
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

			orchestrator.refreshParkedIssues(context.Background(), tt.candidates, nil)

			entry, parked := state.Parked[id]
			if parked != tt.wantParked {
				t.Fatalf("parked after refreshParkedIssues = %v, want %v", parked, tt.wantParked)
			}
			if parked && !sameStoredIdentity(entry.IssueIdentity, tt.wantIdentity) {
				t.Errorf("parked IssueIdentity = %+v, want %+v", entry.IssueIdentity, tt.wantIdentity)
			}
			if !slices.Equal(tracker.fetchIssueByIDCalls, tt.wantReads) {
				t.Errorf("FetchIssueByID called with %v, want %v", tracker.fetchIssueByIDCalls, tt.wantReads)
			}
			var rows []IssueIdentity
			for _, row := range store.parkedIssues {
				rows = append(rows, IssueIdentity{Title: row.IssueTitle, URL: row.IssueURL, IssueType: row.IssueType, Labels: row.IssueLabels})
			}
			if !slices.EqualFunc(rows, tt.wantRows, sameStoredIdentity) {
				t.Errorf("upserted park identities = %+v, want %+v", rows, tt.wantRows)
			}
			if tt.wantReleased {
				assertReleaseKept(t, store, unparkTriggerStateChanged)
			}
		})
	}
}

func TestParkedRunOfHandOver(t *testing.T) {
	t.Parallel()

	lastPrompt := time.Date(2026, 3, 15, 12, 0, 30, 123_456_789, time.UTC)
	run := func(status, reaction string, errText *string) storage.RunHistory {
		return storage.RunHistory{Status: status, ReactionKind: reaction, Error: errText, SessionID: "sess-1", TemplateID: "tpl", AgentText: "stopped", LastPromptAt: lastPrompt.Format(time.RFC3339Nano)}
	}
	statementOnly := parkHandOver{Statement: "stopped", LastPromptAt: lastPrompt}
	all := parkHandOver{SessionID: "sess-1", TemplateID: "tpl", Statement: "stopped", LastPromptAt: lastPrompt}

	tests := []struct {
		name string
		row  storage.RunHistory
		want parkHandOver
	}{
		{"reaction run keeps the statement only", run("succeeded", "review", nil), statementOnly},
		{"error exit keeps the statement only", run("failed", "", new("agent crashed")), statementOnly},
		{"run that succeeded hands over session and template", run("succeeded", "", nil), all},
		{"run whose handoff was withheld hands over session and template", run("failed", "", new(storage.HandoffAbsenceErrorPrefix+"no work")), all},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := parkedRunOf(tt.row).HandOver; got != tt.want {
				t.Errorf("parkedRunOf(%+v).HandOver = %+v, want %+v", tt.row, got, tt.want)
			}
		})
	}
}

func TestParkReleaseCommentWindow(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 3, 15, 12, 1, 0, 700_000_000, time.UTC)
	at := func(id, createdAt string) domain.Comment { return domain.Comment{ID: id, CreatedAt: createdAt} }

	tests := []struct {
		name     string
		release  ParkedEntry
		comments []domain.Comment
		posted   []string
		want     []string
	}{
		{
			"window opens at the last prompt, not the later park",
			ParkedEntry{LastPromptAt: start, ParkedAt: start.Add(time.Hour)},
			[]domain.Comment{at("before", "2026-03-15T12:00:59Z"), at("own-second", "2026-03-15T12:01:00Z"), at("between", "2026-03-15T12:30:00Z")},
			nil, []string{"own-second", "between"},
		},
		{
			"zero last prompt falls back to the park",
			ParkedEntry{ParkedAt: start},
			[]domain.Comment{at("before", "2026-03-15T12:00:59Z"), at("after", "2026-03-15T12:05:00Z")},
			nil, []string{"after"},
		},
		{
			"offset without a colon compares as an instant",
			ParkedEntry{LastPromptAt: start},
			[]domain.Comment{at("before", "2026-03-15T13:00:30+0100"), at("after", "2026-03-15T13:05:00+0100")},
			nil, []string{"after"},
		},
		{
			"empty and unparseable times are in",
			ParkedEntry{LastPromptAt: start},
			[]domain.Comment{at("empty", ""), at("garbled", "yesterday")},
			nil, []string{"empty", "garbled"},
		},
		{
			"zero start keeps every comment",
			ParkedEntry{},
			[]domain.Comment{at("ancient", "2001-01-01T00:00:00Z")},
			nil, []string{"ancient"},
		},
		{
			"recorded comment is out whatever its time",
			ParkedEntry{LastPromptAt: start},
			[]domain.Comment{at("mine-after", "2026-03-15T12:05:00Z"), at("mine-garbled", "yesterday"), at("theirs", "2026-03-15T12:05:00Z")},
			[]string{"mine-after", "mine-garbled"}, []string{"theirs"},
		},
		{
			"nothing kept is an empty list, not nil",
			ParkedEntry{LastPromptAt: start},
			[]domain.Comment{at("before", "2026-03-15T12:00:59Z")},
			nil, nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := parkContextOf(&tt.release, tt.comments, tt.posted).Comments

			var ids []string
			for _, c := range got {
				ids = append(ids, c["id"].(string))
			}
			if got == nil || !slices.Equal(ids, tt.want) {
				t.Errorf("parkContextOf(%+v, %d comments, posted %v).Comments ids = %v (nil: %v), want %v as a non-nil list", tt.release, len(tt.comments), tt.posted, ids, got == nil, tt.want)
			}
		})
	}
}

const parkHandOffTemplate = `{{ if .park }}park={{ .park.reason }}|{{ .park.statement }}|{{ .park.gesture }}|{{ .park.parked_at }}|` +
	`{{ if .park.released_at }}released{{ end }}|{{ range .park.comments }}{{ .id }};{{ end }}` +
	`{{ else }}no-park{{ end }}|issue={{ range .issue.comments }}{{ .id }};{{ end }}`

type parkTickStep struct {
	noSlot    bool
	readFails bool
	wantHold  SkipReason
}

func parkBlockedRun(t *testing.T, store OrchestratorStore, issueID string) *State {
	t.Helper()
	state := exitStateWithIssue(t, issueID, "In Progress")
	running := state.Running[issueID]
	running.SessionID, running.AgentKind = "sess-parked", "mock"

	HandleWorkerExit(state, WorkerResult{
		IssueID:           issueID,
		Identifier:        "PROJ-HANDOFF",
		ExitKind:          WorkerExitNormal,
		AgentAdapter:      "mock",
		SoftStop:          true,
		SoftStopReason:    "blocked",
		SoftStopStatement: workspace.StatusStatement{Text: "which database?"},
		LastPromptAt:      time.Now().UTC().Add(-time.Minute),
	}, HandleWorkerExitParams{
		Store:               store,
		MaxRetryBackoffMS:   300_000,
		OnRetryFire:         noopRetryFire,
		NowFunc:             func() time.Time { return baseTime.Add(60 * time.Second) },
		Logger:              discardLogger(),
		HandoffParkingLabel: "needs-human",
	})
	state.TrackerOpsWg.Wait()

	state.Parked[issueID].LabelApplied = true
	if err := store.MarkParkedIssueLabelApplied(context.Background(), issueID); err != nil {
		t.Fatalf("MarkParkedIssueLabelApplied: %v", err)
	}
	return state
}

func TestParkReleaseHandOff(t *testing.T) {
	t.Parallel()

	const (
		issueID    = "PARK-HANDOFF"
		wantParked = "park=agent_blocked|which database?|%s|%s|released|c-between;c-answer;|issue=c-old;c-between;c-stop;c-answer;"
	)
	candidate := func(state string, labels ...string) domain.Issue {
		return domain.Issue{ID: issueID, Identifier: "PROJ-HANDOFF", Title: "t", State: state, Labels: labels}
	}
	slotFull := []parkTickStep{{noSlot: true, wantHold: SkipAgentSlotsFull}}

	tests := []struct {
		name        string
		candidate   domain.Issue
		parked      bool
		evidence    bool
		restart     bool
		before      []parkTickStep
		wantGesture string
	}{
		{name: "label removed", candidate: candidate("In Progress"), parked: true, wantGesture: "label_removed"},
		{name: "state changed", candidate: candidate("To Do", "needs-human"), parked: true, wantGesture: "state_changed"},
		{name: "later tick", candidate: candidate("In Progress"), parked: true, before: slotFull, wantGesture: "label_removed"},
		{name: "restart", candidate: candidate("In Progress"), parked: true, before: slotFull, restart: true, wantGesture: "label_removed"},
		{
			name: "read fails then succeeds", candidate: candidate("In Progress"), parked: true, wantGesture: "label_removed",
			before: []parkTickStep{{readFails: true, wantHold: SkipParkCommentsUnread}},
		},
		{name: "evidence observed", candidate: candidate("In Progress"), parked: true, evidence: true},
		{name: "never parked", candidate: candidate("In Progress")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			store := openParkStore(t)
			state := NewState(1000, 10, 0, nil, AgentTotals{})
			parkedAt := time.Now().UTC().Truncate(time.Second)
			lastPrompt := parkedAt.Add(-time.Minute)
			if tt.parked {
				state = parkBlockedRun(t, store, issueID)
				rows, err := store.ListParkedIssues(ctx)
				if err != nil || len(rows) != 1 {
					t.Fatalf("ListParkedIssues after the park = %+v, %v, want one row", rows, err)
				}
				parkedAt, _ = time.Parse(time.RFC3339, rows[0].ParkedAt)
				if lastPrompt, err = time.Parse(time.RFC3339Nano, rows[0].LastPromptAt); err != nil {
					t.Fatalf("stored last_prompt_at %q: %v", rows[0].LastPromptAt, err)
				}
				lastPrompt = lastPrompt.Truncate(time.Second)
				if err := store.AddPostedComment(ctx, issueID, "c-stop"); err != nil {
					t.Fatalf("AddPostedComment: %v", err)
				}
			}
			if tt.evidence {
				unparkIssue(ctx, state, issueID, unparkTriggerEvidenceObserved, store, discardLogger())
			}
			comments := []domain.Comment{
				{ID: "c-old", CreatedAt: lastPrompt.Add(-time.Second).Format(time.RFC3339)},
				{ID: "c-between", CreatedAt: lastPrompt.Add(30 * time.Second).Format(time.RFC3339)},
				{ID: "c-stop", CreatedAt: parkedAt.Format(time.RFC3339)},
				{ID: "c-answer", CreatedAt: parkedAt.Add(5 * time.Minute).Format(time.RFC3339)},
			}
			wantPrompt, wantResume := "no-park|issue=", ""
			if tt.wantGesture != "" {
				wantPrompt, wantResume = fmt.Sprintf(wantParked, tt.wantGesture, parkedAt.Format(time.RFC3339)), "sess-parked"
			}

			wm := &stubWorkflowManager{config: defaultWorkerConfig(t.TempDir()), template: mustParseTemplate(t, parkHandOffTemplate)}
			wm.config.Tracker.Kind, wm.config.Tracker.APIKey = "mock", "key"
			wm.config.Agent.MaxTurns = 1
			var readErr error
			tracker := &candidateTrackerAdapter{
				mockTrackerAdapter: &mockTrackerAdapter{fetchCommentsFn: func(context.Context, string) ([]domain.Comment, error) {
					return slices.Clone(comments), readErr
				}},
				fetchCandidatesFn: func(context.Context) ([]domain.Issue, error) { return []domain.Issue{tt.candidate}, nil },
			}
			resumes, prompts := make(chan string, 1), make(chan string, 1)
			agent := &mockAgentAdapter{
				startSessionFn: func(_ context.Context, params domain.StartSessionParams) (domain.Session, error) {
					resumes <- params.ResumeSessionID
					return domain.Session{ID: "sess-new"}, nil
				},
				runTurnFn: func(_ context.Context, session domain.Session, params domain.RunTurnParams) (domain.TurnResult, error) {
					prompts <- params.Prompt
					return domain.TurnResult{SessionID: session.ID, ExitReason: domain.EventTurnCompleted}, nil
				},
			}
			tick := func(state *State, slots int, failRead bool) {
				cfg := wm.Config()
				cfg.Agent.MaxConcurrentAgents = slots
				wm.setConfig(cfg)
				readErr = nil
				if failRead {
					readErr = errors.New("tracker unavailable")
				}
				regs := passingPreflightRegistries()
				regs.ReloadWorkflow = func() error { return nil }
				regs.ConfigFunc = wm.Config
				NewOrchestrator(OrchestratorParams{
					State: state, Logger: discardLogger(), TrackerAdapter: tracker, Router: route.NewRouter(tracker, nil, nil, nil),
					AgentAdapter: agent, WorkflowManager: wm, Store: store, PreflightParams: regs,
				}).handleTick(ctx)
			}

			for i, step := range tt.before {
				slots := 10
				if step.noSlot {
					slots = 0
				}
				tick(state, slots, step.readFails)

				if _, running := state.Running[issueID]; running {
					t.Fatalf("tick %d dispatched the issue, want it held as %q", i+1, step.wantHold)
				}
				if held := RuntimeSnapshot(state, baseTime).Held; len(held) != 1 || held[0].Hold.Reason != step.wantHold {
					t.Errorf("held after tick %d = %+v, want one entry held as %q", i+1, held, step.wantHold)
				}
			}
			rows, err := store.ListParkedIssues(ctx)
			if len(tt.before) > 0 && (err != nil || len(rows) != 1 || rows[0].ReleasedAt == "") {
				t.Fatalf("parked_issues before the dispatch = %+v, %v, want one released row", rows, err)
			}
			if tt.restart {
				state = NewState(1000, 10, 0, nil, AgentTotals{})
				PopulateParked(state, rows, discardLogger())
				if parked := RuntimeSnapshot(state, baseTime).Parked; len(parked) != 0 {
					t.Fatalf("parks after the restart = %+v, want none: a released park must not reload as a park", parked)
				}
			}

			tick(state, 10, false)
			t.Cleanup(state.WorkerWg.Wait)

			gotResume := waittest.Receive(t, resumes, "session start")
			gotPrompt, _, _ := strings.Cut(waittest.Receive(t, prompts, "first turn"), "\n")
			if gotResume != wantResume {
				t.Errorf("ResumeSessionID of the dispatch = %q, want %q", gotResume, wantResume)
			}
			if gotPrompt != wantPrompt {
				t.Errorf("first-turn prompt = %q, want %q", gotPrompt, wantPrompt)
			}
			if rows, err := store.ListParkedIssues(ctx); err != nil || len(rows) != 0 {
				t.Errorf("parked_issues after the dispatch = %+v, %v, want none", rows, err)
			}
		})
	}
}
