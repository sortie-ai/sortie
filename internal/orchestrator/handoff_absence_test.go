package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
)

type handoffLabelCall struct {
	issueID string
	label   string
}

type recordingHandoffTracker struct {
	*mockTrackerAdapter
	mu          sync.Mutex
	labelCalls  []handoffLabelCall
	addLabelErr error
}

func newRecordingHandoffTracker() *recordingHandoffTracker {
	return &recordingHandoffTracker{mockTrackerAdapter: &mockTrackerAdapter{}}
}

func (t *recordingHandoffTracker) AddLabel(_ context.Context, issueID, label string) error {
	t.mu.Lock()
	t.labelCalls = append(t.labelCalls, handoffLabelCall{issueID: issueID, label: label})
	t.mu.Unlock()
	return t.addLabelErr
}

func (t *recordingHandoffTracker) labels() []handoffLabelCall {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]handoffLabelCall(nil), t.labelCalls...)
}

func seedMockHandoffAbsences(store *mockExitStore, issueID string, count int) {
	for range count {
		errText := persistence.HandoffAbsenceErrorPrefix + "absence of work observed under observed policy"
		store.runHistories = append(store.runHistories, persistence.RunHistory{
			IssueID: issueID,
			Status:  "failed",
			Error:   &errText,
		})
	}
}

func TestResolveHandoffParkingLabel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		reactions map[string]config.ReactionConfig
		want      string
	}{
		{name: "missing review reaction", want: "needs-human"},
		{
			name: "disabled review reaction still supplies label",
			reactions: map[string]config.ReactionConfig{
				reviewCommentsConfigKey: {Provider: "", Escalation: "comment", EscalationLabel: "human-queue"},
			},
			want: "human-queue",
		},
		{
			name: "empty review label falls back",
			reactions: map[string]config.ReactionConfig{
				reviewCommentsConfigKey: {EscalationLabel: ""},
			},
			want: "needs-human",
		},
		{
			name: "other reaction label is ignored",
			reactions: map[string]config.ReactionConfig{
				"ci_failure": {EscalationLabel: "ci-human"},
			},
			want: "needs-human",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := resolveHandoffParkingLabel(tt.reactions); got != tt.want {
				t.Errorf("resolveHandoffParkingLabel() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHandleWorkerExitParksAtHandoffAbsenceCeiling(t *testing.T) {
	tests := []struct {
		name             string
		maxSessions      int
		priorAbsences    int
		wantCeiling      int
		configuredLabel  string
		wantAppliedLabel string
	}{
		{
			name:             "positive max_sessions is the exact ceiling",
			maxSessions:      2,
			priorAbsences:    1,
			wantCeiling:      2,
			configuredLabel:  "manual-review",
			wantAppliedLabel: "manual-review",
		},
		{
			name:             "zero max_sessions derives three total absences",
			maxSessions:      0,
			priorAbsences:    2,
			wantCeiling:      3,
			configuredLabel:  "",
			wantAppliedLabel: "needs-human",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const issueID = "ABS-PARK"
			dir, baseline := handoffEvidenceGitWorkspace(t)
			store := &mockExitStore{}
			seedMockHandoffAbsences(store, issueID, tt.priorAbsences)
			tracker := newRecordingHandoffTracker()
			state := exitStateWithIssue(t, issueID, "In Progress")
			params := handoffEvidenceExitParams(t, store, tracker.mockTrackerAdapter, &spyMetrics{})
			params.TrackerAdapter = tracker
			params.MaxSessions = tt.maxSessions
			params.HandoffParkingLabel = tt.configuredLabel
			var logs bytes.Buffer
			params.Logger = debugLogger(t, &logs)

			HandleWorkerExit(state, WorkerResult{
				IssueID:                 issueID,
				Identifier:              "PROJ-769",
				ExitKind:                WorkerExitNormal,
				WorkspacePath:           dir,
				HandoffEvidencePolicy:   config.HandoffEvidenceObserved,
				HandoffEvidenceBaseline: baseline,
				AgentAdapter:            "mock",
			}, params)
			state.TrackerOpsWg.Wait()

			if len(tracker.transitionCalls) != 0 {
				t.Errorf("TransitionIssue calls = %d, want 0", len(tracker.transitionCalls))
			}
			if _, ok := state.Claimed[issueID]; ok {
				t.Error("claim remains after parking")
			}
			if _, ok := state.RetryAttempts[issueID]; ok {
				t.Error("retry remains after parking")
			}
			entry, ok := state.Parked[issueID]
			if !ok {
				t.Error("durable dispatch gate missing after parking")
			}
			if entry != nil && entry.Reason != parkReasonHandoffAbsence {
				t.Errorf("Parked[%s].Reason = %q, want %q", issueID, entry.Reason, parkReasonHandoffAbsence)
			}
			calls := tracker.labels()
			if len(calls) != 1 || calls[0].issueID != issueID || calls[0].label != tt.wantAppliedLabel {
				t.Errorf("AddLabel calls = %+v, want one %q call", calls, tt.wantAppliedLabel)
			}
			if len(store.deletedRetryIDs) != 1 || store.deletedRetryIDs[0] != issueID {
				t.Errorf("DeleteRetryEntry calls = %v, want [%s]", store.deletedRetryIDs, issueID)
			}
			for _, want := range []string{
				"issue_id=" + issueID,
				"consecutive_absences=" + strconv.Itoa(tt.wantCeiling),
				"absence_ceiling=" + strconv.Itoa(tt.wantCeiling),
				`label=` + tt.wantAppliedLabel,
			} {
				if !strings.Contains(logs.String(), want) {
					t.Errorf("parking log missing %q\nlogs: %s", want, logs.String())
				}
			}
		})
	}
}

func TestHandleWorkerExitDefaultCeilingAllowsSecondAbsenceRetry(t *testing.T) {
	const issueID = "ABS-SECOND"
	dir, baseline := handoffEvidenceGitWorkspace(t)
	store := &mockExitStore{}
	seedMockHandoffAbsences(store, issueID, 1)
	tracker := newRecordingHandoffTracker()
	state := exitStateWithIssue(t, issueID, "In Progress")
	params := handoffEvidenceExitParams(t, store, tracker.mockTrackerAdapter, &spyMetrics{})
	params.TrackerAdapter = tracker
	params.MaxSessions = 0

	HandleWorkerExit(state, WorkerResult{
		IssueID:                 issueID,
		Identifier:              "PROJ-SECOND",
		ExitKind:                WorkerExitNormal,
		WorkspacePath:           dir,
		HandoffEvidencePolicy:   config.HandoffEvidenceObserved,
		HandoffEvidenceBaseline: baseline,
		AgentAdapter:            "mock",
	}, params)
	t.Cleanup(func() { CancelRetry(state, issueID) })

	if _, ok := state.RetryAttempts[issueID]; !ok {
		t.Fatal("second consecutive absence did not schedule the second retry")
	}
	if _, ok := state.Claimed[issueID]; !ok {
		t.Error("claim released before the third consecutive absence")
	}
	if _, ok := state.Parked[issueID]; ok {
		t.Error("issue parked before the default ceiling of three")
	}
	state.TrackerOpsWg.Wait()
	if calls := tracker.labels(); len(calls) != 0 {
		t.Errorf("AddLabel calls = %+v, want none before the ceiling", calls)
	}
}

func TestHandleWorkerExitWorkObservedResetsAbsenceSequenceBeforeHandoffWrite(t *testing.T) {
	const issueID = "ABS-RESET"
	dir, baseline := handoffEvidenceGitWorkspace(t)
	if err := os.WriteFile(filepath.Join(dir, "work.txt"), []byte("work\n"), 0o600); err != nil {
		t.Fatalf("write work file: %v", err)
	}

	store := &mockExitStore{}
	seedMockHandoffAbsences(store, issueID, 2)
	tracker := newRecordingHandoffTracker()
	tracker.transitionIssueFn = func(context.Context, string, string) error {
		return errors.New("handoff unavailable")
	}
	state := exitStateWithIssue(t, issueID, "In Progress")
	state.Parked[issueID] = &ParkedEntry{
		Identifier: "PROJ-RESET",
		Reason:     parkReasonHandoffAbsence,
	}
	params := handoffEvidenceExitParams(t, store, tracker.mockTrackerAdapter, &spyMetrics{})
	params.TrackerAdapter = tracker

	HandleWorkerExit(state, WorkerResult{
		IssueID:                 issueID,
		Identifier:              "PROJ-RESET",
		ExitKind:                WorkerExitNormal,
		WorkspacePath:           dir,
		HandoffEvidencePolicy:   config.HandoffEvidenceObserved,
		HandoffEvidenceBaseline: baseline,
		AgentAdapter:            "mock",
	}, params)
	t.Cleanup(func() { CancelRetry(state, issueID) })

	if _, ok := state.Parked[issueID]; ok {
		t.Error("handoff-absence park survived a work-observed run")
	}
	counts, err := store.QueryConsecutiveHandoffAbsenceCounts(context.Background(), []string{issueID})
	if err != nil {
		t.Fatalf("QueryConsecutiveHandoffAbsenceCounts: %v", err)
	}
	if got := counts[issueID]; got != 0 {
		t.Errorf("consecutive absences after work observed = %d, want 0", got)
	}
	if len(tracker.transitionCalls) != 1 {
		t.Errorf("TransitionIssue calls = %d, want 1 failing handoff attempt", len(tracker.transitionCalls))
	}
	// Two calls are expected here, not one: unparking the handoff-absence
	// park resets the sequence, and the standalone reset that already runs
	// on every work-observed verdict (independent of whether a park
	// existed) also fires. Both target the same issue and are idempotent.
	for _, id := range store.absenceResetOf {
		if id != issueID {
			t.Errorf("ResetHandoffAbsenceSequence calls = %v, want every call for %s", store.absenceResetOf, issueID)
		}
	}
	if len(store.absenceResetOf) != 2 {
		t.Errorf("ResetHandoffAbsenceSequence call count = %d, want 2", len(store.absenceResetOf))
	}
}

func TestHandleWorkerExitSucceededWithoutVerdictKeepsAbsenceSequence(t *testing.T) {
	const issueID = "ABS-BLOCKED"
	store := &mockExitStore{}
	seedMockHandoffAbsences(store, issueID, 2)
	tracker := newRecordingHandoffTracker()
	blockedState := exitStateWithIssue(t, issueID, "In Progress")
	params := handoffEvidenceExitParams(t, store, tracker.mockTrackerAdapter, &spyMetrics{})
	params.TrackerAdapter = tracker

	// A blocked soft stop records "succeeded" and carries no evidence verdict,
	// so it must neither reset the sequence nor advance it.
	blockedDir, blockedBaseline := handoffEvidenceGitWorkspace(t)
	HandleWorkerExit(blockedState, WorkerResult{
		IssueID:                 issueID,
		Identifier:              "PROJ-BLOCKED",
		ExitKind:                WorkerExitNormal,
		WorkspacePath:           blockedDir,
		SoftStop:                true,
		SoftStopReason:          "blocked",
		HandoffEvidencePolicy:   config.HandoffEvidenceObserved,
		HandoffEvidenceBaseline: blockedBaseline,
		AgentAdapter:            "mock",
	}, params)

	if len(store.runHistories) == 0 || store.runHistories[len(store.runHistories)-1].Status != "succeeded" {
		t.Fatalf("blocked soft stop recorded %+v, want a succeeded row", store.runHistories)
	}
	if len(store.absenceResetOf) != 0 {
		t.Errorf("ResetHandoffAbsenceSequence calls = %v, want none without a work-observed verdict", store.absenceResetOf)
	}
	counts, err := store.QueryConsecutiveHandoffAbsenceCounts(context.Background(), []string{issueID})
	if err != nil {
		t.Fatalf("QueryConsecutiveHandoffAbsenceCounts: %v", err)
	}
	if got := counts[issueID]; got != 2 {
		t.Fatalf("consecutive absences after a blocked soft stop = %d, want 2", got)
	}

	// The next absence is therefore the third, and reaches the ceiling instead
	// of restarting a sequence the soft stop appeared to have cleared.
	absentDir, absentBaseline := handoffEvidenceGitWorkspace(t)
	absentState := exitStateWithIssue(t, issueID, "In Progress")
	HandleWorkerExit(absentState, WorkerResult{
		IssueID:                 issueID,
		Identifier:              "PROJ-BLOCKED",
		ExitKind:                WorkerExitNormal,
		WorkspacePath:           absentDir,
		HandoffEvidencePolicy:   config.HandoffEvidenceObserved,
		HandoffEvidenceBaseline: absentBaseline,
		AgentAdapter:            "mock",
	}, params)
	absentState.TrackerOpsWg.Wait()
	t.Cleanup(func() { CancelRetry(absentState, issueID) })

	if _, ok := absentState.Parked[issueID]; !ok {
		t.Error("third consecutive absence did not park the issue")
	}
	if calls := tracker.labels(); len(calls) != 1 {
		t.Errorf("AddLabel calls = %+v, want one parking call", calls)
	}
}

func TestHandleRetryTimerKeepsRecoveredExhaustedAbsenceParkedWhenLabelFails(t *testing.T) {
	const issueID = "ABS-RECOVERED"
	store := &mockRetryStore{absenceCounts: map[string]int{issueID: 3}}
	tracker := &mockRetryTracker{addLabelErr: errors.New("label service unavailable")}
	state := retryState(t, issueID, "PROJ-RECOVERED", 3)
	params := defaultRetryParams(t, store, tracker)
	params.MaxSessions = 0
	params.HandoffParkingLabel = "operator-needed"
	var logs bytes.Buffer
	params.Logger = debugLogger(t, &logs)

	HandleRetryTimer(state, issueID, params)
	state.TrackerOpsWg.Wait()

	if tracker.fetchCount != 0 {
		t.Errorf("FetchIssueByID calls = %d, want 0 for an exhausted recovered retry", tracker.fetchCount)
	}
	if len(tracker.addedLabels) != 1 || tracker.addedLabels[0] != "operator-needed" {
		t.Errorf("AddLabel calls = %v, want [operator-needed]", tracker.addedLabels)
	}
	if _, ok := state.Claimed[issueID]; ok {
		t.Error("claim remains after recovered absence parking")
	}
	if _, ok := state.RetryAttempts[issueID]; ok {
		t.Error("retry was restored after recovered absence parking")
	}
	if entry := state.Parked[issueID]; entry == nil || entry.Reason != parkReasonHandoffAbsence {
		t.Errorf("Parked[%s] = %+v, want reason %q", issueID, entry, parkReasonHandoffAbsence)
	}
	if ShouldDispatch(candidateIssue(issueID, "PROJ-RECOVERED", "In Progress"), state, params.ActiveStates, params.TerminalStates) {
		t.Error("ordinary polling would dispatch after the parking label failed")
	}
	if !strings.Contains(logs.String(), "park label write failed") {
		t.Errorf("missing label failure log\nlogs: %s", logs.String())
	}
}

func TestHandleWorkerExitReportsAbsenceResetFailure(t *testing.T) {
	const issueID = "ABS-RESET-ERR"
	dir, baseline := handoffEvidenceGitWorkspace(t)
	if err := os.WriteFile(filepath.Join(dir, "work.txt"), []byte("work\n"), 0o600); err != nil {
		t.Fatalf("write work file: %v", err)
	}

	store := &mockExitStore{absenceResetErr: errors.New("database is locked")}
	seedMockHandoffAbsences(store, issueID, 1)
	tracker := newRecordingHandoffTracker()
	state := exitStateWithIssue(t, issueID, "In Progress")
	params := handoffEvidenceExitParams(t, store, tracker.mockTrackerAdapter, &spyMetrics{})
	params.TrackerAdapter = tracker
	var logs bytes.Buffer
	params.Logger = debugLogger(t, &logs)

	HandleWorkerExit(state, WorkerResult{
		IssueID:                 issueID,
		Identifier:              "PROJ-RESET-ERR",
		ExitKind:                WorkerExitNormal,
		WorkspacePath:           dir,
		HandoffEvidencePolicy:   config.HandoffEvidenceObserved,
		HandoffEvidenceBaseline: baseline,
		AgentAdapter:            "mock",
	}, params)
	t.Cleanup(func() { CancelRetry(state, issueID) })

	if !strings.Contains(logs.String(), "failed to reset handoff absence sequence") {
		t.Errorf("missing reset failure log\nlogs: %s", logs.String())
	}
	if len(tracker.transitionCalls) != 1 {
		t.Errorf("TransitionIssue calls = %d, want 1: the handoff was withheld by a bookkeeping failure", len(tracker.transitionCalls))
	}
}

func TestHandleRetryTimerFallsBackWhenAbsenceQueryFails(t *testing.T) {
	const issueID = "ABS-QUERY-ERR"
	store := &mockRetryStore{absenceCountErr: errors.New("database is locked")}
	tracker := &mockRetryTracker{fetchedIssue: candidateIssue(issueID, "PROJ-QUERY", "In Progress")}
	state := retryState(t, issueID, "PROJ-QUERY", 3)
	params := defaultRetryParams(t, store, tracker)
	params.MaxSessions = 0
	var logs bytes.Buffer
	params.Logger = debugLogger(t, &logs)

	HandleRetryTimer(state, issueID, params)
	state.TrackerOpsWg.Wait()

	if !strings.Contains(logs.String(), "handoff absence count query failed") {
		t.Errorf("missing query failure log\nlogs: %s", logs.String())
	}
	if _, ok := state.Parked[issueID]; !ok {
		t.Error("an unanswerable absence query resumed an exhausted sequence")
	}
	if tracker.fetchCount != 0 {
		t.Errorf("FetchIssueByID calls = %d, want 0 after parking", tracker.fetchCount)
	}
}

func TestRebuildBudgetExhaustedRetainsAbsenceParkingOnQueryError(t *testing.T) {
	const issueID = "ABS-REBUILD-ERR"
	issue := candidateIssue(issueID, "PROJ-REBUILD", "To Do")
	cfg := config.ServiceConfig{}
	store := &stubStore{absenceCountErr: errors.New("database is locked")}
	state := NewState(1000, 1, nil, AgentTotals{})
	state.Parked[issueID] = &ParkedEntry{Identifier: "PROJ-REBUILD", Reason: parkReasonHandoffAbsence}
	orchestrator := NewOrchestrator(OrchestratorParams{
		State:           state,
		Logger:          discardLogger(),
		TrackerAdapter:  newRecordingHandoffTracker(),
		AgentAdapter:    &mockAgentAdapter{},
		WorkflowManager: &stubWorkflowManager{config: cfg},
		Store:           store,
	})

	orchestrator.parkExhaustedAbsences(context.Background(), cfg, []domain.Issue{issue})

	entry := state.Parked[issueID]
	if entry == nil || entry.Reason != parkReasonHandoffAbsence {
		t.Errorf("Parked[%s] = %+v, want the parking retained across a query error", issueID, entry)
	}
	if ShouldDispatch(issue, state, []string{"To Do"}, []string{"Done"}) {
		t.Error("a query error released a parked issue")
	}
}

func TestHandleRetryTimerExemptsReactionContinuationFromAbsenceGate(t *testing.T) {
	const issueID = "ABS-REACTION"
	store := &mockRetryStore{absenceCounts: map[string]int{issueID: 3}}
	tracker := &mockRetryTracker{fetchedIssue: candidateIssue(issueID, "PROJ-REACTION", "In Progress")}
	state := retryState(t, issueID, "PROJ-REACTION", 1)
	state.RetryAttempts[issueID].ReactionKind = ReactionKindReview
	params := defaultRetryParams(t, store, tracker)
	params.MaxSessions = 0

	HandleRetryTimer(state, issueID, params)
	state.TrackerOpsWg.Wait()

	if tracker.fetchCount != 1 {
		t.Errorf("FetchIssueByID calls = %d, want 1: the reaction retry was cancelled by unrelated absences", tracker.fetchCount)
	}
	if len(tracker.addedLabels) != 0 {
		t.Errorf("AddLabel calls = %v, want none for a reaction continuation", tracker.addedLabels)
	}
	if _, ok := state.Parked[issueID]; ok {
		t.Error("reaction continuation parked by the handoff-absence ceiling")
	}
}

func TestHandleRetryTimerSkipsAbsenceGateUnderOffPolicy(t *testing.T) {
	const issueID = "ABS-OFF-RETRY"
	store := &mockRetryStore{absenceCounts: map[string]int{issueID: 3}}
	tracker := &mockRetryTracker{fetchedIssue: candidateIssue(issueID, "PROJ-OFF", "In Progress")}
	state := retryState(t, issueID, "PROJ-OFF", 1)
	params := defaultRetryParams(t, store, tracker)
	params.MaxSessions = 0
	params.HandoffEvidencePolicy = config.HandoffEvidenceOff

	HandleRetryTimer(state, issueID, params)
	state.TrackerOpsWg.Wait()

	if len(store.absenceCountedIssueIDs) != 0 {
		t.Errorf("absence query ran for %v under the off policy, want no query", store.absenceCountedIssueIDs)
	}
	if _, ok := state.Parked[issueID]; ok {
		t.Error("issue parked under the off policy")
	}
}

func TestRebuildBudgetExhaustedSkipsAbsenceGateUnderOffPolicy(t *testing.T) {
	const issueID = "ABS-OFF"
	issue := candidateIssue(issueID, "PROJ-OFF", "To Do")
	cfg := config.ServiceConfig{
		Tracker: config.TrackerConfig{HandoffEvidence: config.HandoffEvidenceOff},
	}
	store := &stubStore{absenceCounts: map[string]int{issueID: 5}}
	tracker := newRecordingHandoffTracker()
	state := NewState(1000, 1, nil, AgentTotals{})
	orchestrator := NewOrchestrator(OrchestratorParams{
		State:           state,
		Logger:          discardLogger(),
		TrackerAdapter:  tracker,
		AgentAdapter:    &mockAgentAdapter{},
		WorkflowManager: &stubWorkflowManager{config: cfg},
		Store:           store,
	})

	orchestrator.parkExhaustedAbsences(context.Background(), cfg, []domain.Issue{issue})
	state.TrackerOpsWg.Wait()

	if store.absenceQueryCalls != 0 {
		t.Errorf("absence query ran %d times under the off policy, want 0", store.absenceQueryCalls)
	}
	if _, ok := state.Parked[issueID]; ok {
		t.Error("the off policy parked an issue by the absence ceiling")
	}
	if !ShouldDispatch(issue, state, []string{"To Do"}, []string{"Done"}) {
		t.Error("the off policy left an issue gated by the absence ceiling")
	}
	if calls := tracker.labels(); len(calls) != 0 {
		t.Errorf("AddLabel calls = %+v, want none under the off policy", calls)
	}
}

func TestRebuildBudgetExhaustedRestoresAbsenceParkingAfterRestart(t *testing.T) {
	const issueID = "ABS-POLL"
	issue := candidateIssue(issueID, "PROJ-POLL", "To Do")
	startupCfg := config.ServiceConfig{
		Agent: config.AgentConfig{MaxSessions: 0},
		Reactions: map[string]config.ReactionConfig{
			reviewCommentsConfigKey: {
				Provider:        "",
				Escalation:      "comment",
				EscalationLabel: "captured-human-label",
			},
		},
	}
	manager := &stubWorkflowManager{config: startupCfg}
	store := &stubStore{absenceCounts: map[string]int{issueID: 3}}
	tracker := newRecordingHandoffTracker()
	state := NewState(1000, 1, nil, AgentTotals{})
	orchestrator := NewOrchestrator(OrchestratorParams{
		State:           state,
		Logger:          discardLogger(),
		TrackerAdapter:  tracker,
		AgentAdapter:    &mockAgentAdapter{},
		WorkflowManager: manager,
		Store:           store,
	})

	// Reaction configuration reloads do not change the captured parking
	// label, and escalation: comment does not change the primary action.
	reloadedCfg := startupCfg
	reloadedCfg.Reactions = map[string]config.ReactionConfig{
		reviewCommentsConfigKey: {
			Escalation:      "comment",
			EscalationLabel: "reloaded-label",
		},
	}
	manager.setConfig(reloadedCfg)
	orchestrator.parkExhaustedAbsences(context.Background(), reloadedCfg, []domain.Issue{issue})
	state.TrackerOpsWg.Wait()

	if ShouldDispatch(issue, state, []string{"To Do"}, []string{"Done"}) {
		t.Error("ordinary polling would dispatch an absence-exhausted issue after restart")
	}
	entry := state.Parked[issueID]
	if entry == nil || entry.Reason != parkReasonHandoffAbsence {
		t.Errorf("Parked[%s] = %+v, want reason %q", issueID, entry, parkReasonHandoffAbsence)
	}
	calls := tracker.labels()
	if len(calls) != 1 || calls[0].label != "captured-human-label" {
		t.Errorf("AddLabel calls = %+v, want captured-human-label despite reload", calls)
	}
}

// TestParkExhaustedAbsencesLeavesBudgetExhaustedEmpty verifies that an
// absence park taken by the tick pass produces a parked_issues row with
// reason handoff_absence and leaves BudgetExhausted and
// BudgetExhaustedReason empty: the absence park is the sole gate, not a
// second budget-exhaustion mechanism.
func TestParkExhaustedAbsencesLeavesBudgetExhaustedEmpty(t *testing.T) {
	t.Parallel()

	const issueID = "ABS-BUDGET"
	issue := candidateIssue(issueID, "PROJ-BUDGET", "To Do")
	cfg := config.ServiceConfig{}
	store := &stubStore{absenceCounts: map[string]int{issueID: 3}}
	tracker := newRecordingHandoffTracker()
	state := NewState(1000, 1, nil, AgentTotals{})
	orchestrator := NewOrchestrator(OrchestratorParams{
		State:           state,
		Logger:          discardLogger(),
		TrackerAdapter:  tracker,
		AgentAdapter:    &mockAgentAdapter{},
		WorkflowManager: &stubWorkflowManager{config: cfg},
		Store:           store,
	})

	orchestrator.parkExhaustedAbsences(context.Background(), cfg, []domain.Issue{issue})
	state.TrackerOpsWg.Wait()

	entry := state.Parked[issueID]
	if entry == nil || entry.Reason != parkReasonHandoffAbsence {
		t.Fatalf("Parked[%s] = %+v, want reason %q", issueID, entry, parkReasonHandoffAbsence)
	}
	if len(state.BudgetExhausted) != 0 {
		t.Errorf("BudgetExhausted = %v, want empty", state.BudgetExhausted)
	}
	if len(state.BudgetExhaustedReason) != 0 {
		t.Errorf("BudgetExhaustedReason = %v, want empty", state.BudgetExhaustedReason)
	}
}

// TestUnparkIssueAgentBlockedDoesNotResetAbsenceSequence verifies that
// releasing a park whose reason is agent_blocked does not call
// ResetHandoffAbsenceSequence: that reset belongs only to the
// handoff_absence reason, which owns a sequence to reset.
func TestUnparkIssueAgentBlockedDoesNotResetAbsenceSequence(t *testing.T) {
	t.Parallel()

	const issueID = "BLK-UNPARK"
	store := &mockExitStore{}
	state := NewState(1000, 1, nil, AgentTotals{})
	state.Parked[issueID] = &ParkedEntry{Identifier: "PROJ-BLK", Reason: parkReasonAgentBlocked}

	unparkIssue(context.Background(), state, issueID, unparkTriggerStateChanged, store, discardLogger())

	if _, ok := state.Parked[issueID]; ok {
		t.Error("issue remains parked after unparkIssue")
	}
	if len(store.absenceResetOf) != 0 {
		t.Errorf("ResetHandoffAbsenceSequence calls = %v, want none for an agent_blocked park", store.absenceResetOf)
	}
	if len(store.deletedParkedIDs) != 1 || store.deletedParkedIDs[0] != issueID {
		t.Errorf("DeleteParkedIssue calls = %v, want [%s]", store.deletedParkedIDs, issueID)
	}
}

// TestHandleTickAbsenceReleaseOrdering drives the real release pass and
// the real absence trigger, in tick order, against a real store. A
// pre-existing handoff-absence park is released by a candidate state
// change; the release must reset the absence sequence before the trigger
// re-evaluates the same candidate, so the issue is not re-parked on the
// same tick. A fake store cannot substitute here: the defect this guards
// is the two passes disagreeing about a real, persisted sequence.
func TestHandleTickAbsenceReleaseOrdering(t *testing.T) {
	t.Parallel()

	const issueID = "ORD-1"
	ctx := context.Background()
	store, err := persistence.Open(ctx, ":memory:")
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

	absenceErr := persistence.HandoffAbsenceErrorPrefix + "absence of work observed under observed policy"
	for range 3 {
		if _, err := store.AppendRunHistory(ctx, persistence.RunHistory{
			IssueID:      issueID,
			Identifier:   "PROJ-ORD",
			Attempt:      1,
			AgentAdapter: "mock",
			Workspace:    "/tmp/" + issueID,
			StartedAt:    "2026-08-17T00:00:00Z",
			CompletedAt:  "2026-08-17T00:01:00Z",
			Status:       "failed",
			Error:        &absenceErr,
		}); err != nil {
			t.Fatalf("AppendRunHistory: %v", err)
		}
	}

	if err := store.UpsertParkedIssue(ctx, persistence.ParkedIssue{
		IssueID:     issueID,
		Identifier:  "PROJ-ORD",
		Reason:      parkReasonHandoffAbsence,
		ParkedState: "In Progress",
		Label:       "needs-human",
		ParkedAt:    "2026-08-17T00:01:00Z",
	}); err != nil {
		t.Fatalf("UpsertParkedIssue: %v", err)
	}

	state := NewState(1000, 4, nil, AgentTotals{})
	state.Parked[issueID] = &ParkedEntry{
		Identifier:  "PROJ-ORD",
		Reason:      parkReasonHandoffAbsence,
		ParkedState: "In Progress",
		Label:       "needs-human",
	}

	tracker := newRecordingHandoffTracker()
	orchestrator := NewOrchestrator(OrchestratorParams{
		State:           state,
		Logger:          discardLogger(),
		TrackerAdapter:  tracker,
		AgentAdapter:    &mockAgentAdapter{},
		WorkflowManager: &stubWorkflowManager{config: config.ServiceConfig{}},
		Store:           store,
	})

	candidates := []domain.Issue{
		{ID: issueID, Identifier: "PROJ-ORD", Title: "T", State: "Done"},
	}

	orchestrator.refreshParkedIssues(ctx, candidates)
	orchestrator.parkExhaustedAbsences(ctx, config.ServiceConfig{}, candidates)
	state.TrackerOpsWg.Wait()

	if _, ok := state.Parked[issueID]; ok {
		t.Error("the issue was re-parked on the same tick it was released")
	}
	counts, err := store.QueryConsecutiveHandoffAbsenceCounts(ctx, []string{issueID})
	if err != nil {
		t.Fatalf("QueryConsecutiveHandoffAbsenceCounts: %v", err)
	}
	if got := counts[issueID]; got != 0 {
		t.Errorf("consecutive absences after release = %d, want 0", got)
	}
	if calls := tracker.labels(); len(calls) != 0 {
		t.Errorf("AddLabel calls = %+v, want none: the issue must not be re-parked", calls)
	}
}

// TestParkExhaustedAbsencesOffPolicyHoldsExistingParkAndSkipsNewPark
// verifies both halves of the off-policy behavior in one pass: a park
// already held stands, because the policy governs the trigger and not the
// gate, and a candidate whose absence count has just reached the ceiling
// is not newly parked, because the trigger is skipped entirely under the
// off policy.
func TestParkExhaustedAbsencesOffPolicyHoldsExistingParkAndSkipsNewPark(t *testing.T) {
	t.Parallel()

	const heldIssueID = "OFF-HELD"
	const freshIssueID = "OFF-FRESH"
	cfg := config.ServiceConfig{Tracker: config.TrackerConfig{HandoffEvidence: config.HandoffEvidenceOff}}
	store := &stubStore{absenceCounts: map[string]int{freshIssueID: 5}}
	tracker := newRecordingHandoffTracker()
	state := NewState(1000, 1, nil, AgentTotals{})
	state.Parked[heldIssueID] = &ParkedEntry{
		Identifier:  "PROJ-HELD",
		Reason:      parkReasonHandoffAbsence,
		ParkedState: "In Progress",
		Label:       "needs-human",
	}

	orchestrator := NewOrchestrator(OrchestratorParams{
		State:           state,
		Logger:          discardLogger(),
		TrackerAdapter:  tracker,
		AgentAdapter:    &mockAgentAdapter{},
		WorkflowManager: &stubWorkflowManager{config: cfg},
		Store:           store,
	})

	candidates := []domain.Issue{
		{ID: heldIssueID, Identifier: "PROJ-HELD", Title: "T", State: "In Progress"},
		{ID: freshIssueID, Identifier: "PROJ-FRESH", Title: "T", State: "To Do"},
	}

	orchestrator.refreshParkedIssues(context.Background(), candidates)
	orchestrator.parkExhaustedAbsences(context.Background(), cfg, candidates)
	state.TrackerOpsWg.Wait()

	if _, ok := state.Parked[heldIssueID]; !ok {
		t.Error("the off policy released a park it does not govern")
	}
	if _, ok := state.Parked[freshIssueID]; ok {
		t.Error("the off policy took a new absence park")
	}
	if store.absenceQueryCalls != 0 {
		t.Errorf("absence query ran %d times under the off policy, want 0", store.absenceQueryCalls)
	}
}

// TestParkRetryLaneBackfillAndRelease verifies that a retry-lane absence
// park records an empty parked_state, the next release pass backfills it
// without releasing, and a state change observed after the backfill
// releases the park.
func TestParkRetryLaneBackfillAndRelease(t *testing.T) {
	t.Parallel()

	const issueID = "BACKFILL-1"
	store := &stubStore{absenceCounts: map[string]int{issueID: 3}}
	retryTracker := &mockRetryTracker{}
	state := retryState(t, issueID, "PROJ-BACKFILL", 1)
	retryParams := HandleRetryTimerParams{
		Store:             store,
		TrackerAdapter:    retryTracker,
		ActiveStates:      []string{"To Do", "In Progress"},
		TerminalStates:    []string{"Done"},
		MaxRetryBackoffMS: 300_000,
		MakeWorkerFn: func(_, _, _, _, _ string, _ domain.AgentAdapter) WorkerFunc {
			return func(context.Context, domain.Issue, *int) {}
		},
		AgentAdapterByKind: func(string) (domain.AgentAdapter, error) { return &mockAgentAdapter{}, nil },
		OnRetryFire:        noopRetryFire,
		Ctx:                context.Background(),
		Logger:             discardLogger(),
	}
	retryParams.MaxSessions = 0

	HandleRetryTimer(state, issueID, retryParams)
	state.TrackerOpsWg.Wait()

	entry := state.Parked[issueID]
	if entry == nil {
		t.Fatal("issue not parked after the retry-lane absence ceiling was reached")
	}
	if entry.ParkedState != "" {
		t.Fatalf("Parked[%s].ParkedState = %q, want empty for a retry-lane park", issueID, entry.ParkedState)
	}

	tickTracker := &mockTrackerAdapter{
		fetchStatesFn: func(_ context.Context, _ []string) (map[string]string, error) {
			return map[string]string{issueID: "In Progress"}, nil
		},
	}
	orchestrator := NewOrchestrator(OrchestratorParams{
		State:           state,
		Logger:          discardLogger(),
		TrackerAdapter:  tickTracker,
		AgentAdapter:    &mockAgentAdapter{},
		WorkflowManager: &stubWorkflowManager{config: config.ServiceConfig{}},
		Store:           store,
	})

	orchestrator.refreshParkedIssues(context.Background(), nil)

	if entry.ParkedState != "In Progress" {
		t.Errorf("Parked[%s].ParkedState after backfill = %q, want %q", issueID, entry.ParkedState, "In Progress")
	}
	if _, ok := state.Parked[issueID]; !ok {
		t.Error("the backfill tick released the park, want it to stand")
	}

	tickTracker.fetchStatesFn = func(_ context.Context, _ []string) (map[string]string, error) {
		return map[string]string{issueID: "Done"}, nil
	}
	orchestrator.refreshParkedIssues(context.Background(), nil)

	if _, ok := state.Parked[issueID]; ok {
		t.Error("a state change observed after the backfill did not release the park")
	}
	if len(store.deletedParkedIDs) != 1 || store.deletedParkedIDs[0] != issueID {
		t.Errorf("DeleteParkedIssue calls = %v, want [%s]", store.deletedParkedIDs, issueID)
	}
}

// TestParkSingleLogRecordPerTrigger verifies that one park emits exactly
// one issue-parked log record and one counter increment, whatever the
// trigger, that the absence path's record carries the absence attributes
// and the blocked path's does not, and that the retired
// ceiling-reached log message never appears.
func TestParkSingleLogRecordPerTrigger(t *testing.T) {
	t.Parallel()

	t.Run("blocked path", func(t *testing.T) {
		t.Parallel()

		store := &mockExitStore{}
		spy := &spyMetrics{}
		state := exitStateWithIssue(t, "LOG-BLOCKED", "In Progress")
		params := defaultExitParams(t, store)
		params.Metrics = spy
		var logs bytes.Buffer
		params.Logger = debugLogger(t, &logs)

		HandleWorkerExit(state, WorkerResult{
			IssueID:        "LOG-BLOCKED",
			Identifier:     "LOG-BLOCKED-ident",
			ExitKind:       WorkerExitNormal,
			AgentAdapter:   "mock",
			SoftStop:       true,
			SoftStopReason: "blocked",
		}, params)
		state.TrackerOpsWg.Wait()

		output := logs.String()
		if got := strings.Count(output, `msg="issue parked"`); got != 1 {
			t.Errorf("issue-parked log record count = %d, want 1\nlogs: %s", got, output)
		}
		if strings.Contains(output, "handoff absence ceiling reached, parking issue") {
			t.Errorf("retired log message present\nlogs: %s", output)
		}
		if strings.Contains(output, "consecutive_absences") || strings.Contains(output, "absence_ceiling") {
			t.Errorf("blocked-path log carries absence attributes, want neither\nlogs: %s", output)
		}
		spy.mu.Lock()
		parks := len(spy.issueParks)
		spy.mu.Unlock()
		if parks != 1 {
			t.Errorf("IncIssueParks call count = %d, want 1", parks)
		}
	})

	t.Run("absence path", func(t *testing.T) {
		t.Parallel()

		const issueID = "LOG-ABSENCE"
		issue := candidateIssue(issueID, "PROJ-LOG", "To Do")
		cfg := config.ServiceConfig{}
		store := &stubStore{absenceCounts: map[string]int{issueID: 3}}
		tracker := newRecordingHandoffTracker()
		spy := &spyMetrics{}
		state := NewState(1000, 1, nil, AgentTotals{})
		var logs bytes.Buffer
		orchestrator := NewOrchestrator(OrchestratorParams{
			State:           state,
			Logger:          debugLogger(t, &logs),
			TrackerAdapter:  tracker,
			AgentAdapter:    &mockAgentAdapter{},
			WorkflowManager: &stubWorkflowManager{config: cfg},
			Store:           store,
			Metrics:         spy,
		})

		orchestrator.parkExhaustedAbsences(context.Background(), cfg, []domain.Issue{issue})
		state.TrackerOpsWg.Wait()

		output := logs.String()
		if got := strings.Count(output, `msg="issue parked"`); got != 1 {
			t.Errorf("issue-parked log record count = %d, want 1\nlogs: %s", got, output)
		}
		if strings.Contains(output, "handoff absence ceiling reached, parking issue") {
			t.Errorf("retired log message present\nlogs: %s", output)
		}
		if !strings.Contains(output, "consecutive_absences=3") || !strings.Contains(output, "absence_ceiling=3") {
			t.Errorf("absence-path log missing absence attributes\nlogs: %s", output)
		}
		spy.mu.Lock()
		parks := len(spy.issueParks)
		spy.mu.Unlock()
		if parks != 1 {
			t.Errorf("IncIssueParks call count = %d, want 1", parks)
		}
	})
}

// TestHandleWorkerExitAbsenceParkRecordsObservedStateAndReleasesWithoutBackfill
// verifies that the worker-exit absence park records the resolved
// terminal observation as parked_state rather than an empty string, and
// that the resulting park is released by a later state change with no
// intervening backfill tick.
func TestHandleWorkerExitAbsenceParkRecordsObservedStateAndReleasesWithoutBackfill(t *testing.T) {
	t.Parallel()

	const issueID = "ABS-OBSERVED"
	dir, baseline := handoffEvidenceGitWorkspace(t)
	store := &mockExitStore{}
	seedMockHandoffAbsences(store, issueID, 2)
	tracker := newRecordingHandoffTracker()
	state := exitStateWithIssue(t, issueID, "In Progress")
	params := handoffEvidenceExitParams(t, store, tracker.mockTrackerAdapter, &spyMetrics{})
	params.TrackerAdapter = tracker

	HandleWorkerExit(state, WorkerResult{
		IssueID:                 issueID,
		Identifier:              "PROJ-OBS",
		ExitKind:                WorkerExitNormal,
		WorkspacePath:           dir,
		HandoffEvidencePolicy:   config.HandoffEvidenceObserved,
		HandoffEvidenceBaseline: baseline,
		AgentAdapter:            "mock",
	}, params)
	state.TrackerOpsWg.Wait()

	entry := state.Parked[issueID]
	if entry == nil {
		t.Fatal("the third consecutive absence did not park the issue")
	}
	if entry.ParkedState == "" {
		t.Error("the worker-exit absence park recorded an empty parked_state, want the resolved observation")
	}
	if entry.ParkedState != "In Progress" {
		t.Errorf("Parked[%s].ParkedState = %q, want %q", issueID, entry.ParkedState, "In Progress")
	}

	tickStore := &stubStore{}
	orchestrator := NewOrchestrator(OrchestratorParams{
		State:           state,
		Logger:          discardLogger(),
		TrackerAdapter:  tracker,
		AgentAdapter:    &mockAgentAdapter{},
		WorkflowManager: &stubWorkflowManager{config: config.ServiceConfig{}},
		Store:           tickStore,
	})
	candidates := []domain.Issue{{ID: issueID, Identifier: "PROJ-OBS", Title: "T", State: "Done"}}

	orchestrator.refreshParkedIssues(context.Background(), candidates)

	if _, ok := state.Parked[issueID]; ok {
		t.Error("a state change did not release the worker-exit absence park")
	}
	if len(tickStore.deletedParkedIDs) != 1 || tickStore.deletedParkedIDs[0] != issueID {
		t.Errorf("DeleteParkedIssue calls = %v, want [%s]", tickStore.deletedParkedIDs, issueID)
	}
}
