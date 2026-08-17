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
			if _, ok := state.BudgetExhausted[issueID]; !ok {
				t.Error("durable dispatch gate missing after parking")
			}
			if got := state.BudgetExhaustedReason[issueID]; got != budgetReasonHandoffAbsence {
				t.Errorf("BudgetExhaustedReason = %q, want %q", got, budgetReasonHandoffAbsence)
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
				`escalation_label=` + tt.wantAppliedLabel,
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
	if _, ok := state.BudgetExhausted[issueID]; ok {
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
	state.BudgetExhausted[issueID] = struct{}{}
	state.BudgetExhaustedReason[issueID] = budgetReasonHandoffAbsence
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

	if _, ok := state.BudgetExhausted[issueID]; ok {
		t.Error("handoff-absence gate survived a work-observed run")
	}
	if _, ok := state.BudgetExhaustedReason[issueID]; ok {
		t.Error("handoff-absence reason survived a work-observed run")
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
	if len(store.absenceResetOf) != 1 || store.absenceResetOf[0] != issueID {
		t.Errorf("ResetHandoffAbsenceSequence calls = %v, want [%s]", store.absenceResetOf, issueID)
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

	if _, ok := absentState.BudgetExhausted[issueID]; !ok {
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
	if got := state.BudgetExhaustedReason[issueID]; got != budgetReasonHandoffAbsence {
		t.Errorf("BudgetExhaustedReason = %q, want %q", got, budgetReasonHandoffAbsence)
	}
	if ShouldDispatch(candidateIssue(issueID, "PROJ-RECOVERED", "In Progress"), state, params.ActiveStates, params.TerminalStates) {
		t.Error("ordinary polling would dispatch after the parking label failed")
	}
	if !strings.Contains(logs.String(), "handoff absence escalation label failed") {
		t.Errorf("missing label failure log\nlogs: %s", logs.String())
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
	if _, ok := state.BudgetExhausted[issueID]; ok {
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
	if _, ok := state.BudgetExhausted[issueID]; ok {
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
	state.BudgetExhausted[issueID] = struct{}{}
	state.BudgetExhaustedReason[issueID] = budgetReasonHandoffAbsence
	orchestrator := NewOrchestrator(OrchestratorParams{
		State:           state,
		Logger:          discardLogger(),
		TrackerAdapter:  tracker,
		AgentAdapter:    &mockAgentAdapter{},
		WorkflowManager: &stubWorkflowManager{config: cfg},
		Store:           store,
	})

	orchestrator.rebuildBudgetExhausted(context.Background(), cfg, []domain.Issue{issue})
	state.TrackerOpsWg.Wait()

	if store.absenceQueryCalls != 0 {
		t.Errorf("absence query ran %d times under the off policy, want 0", store.absenceQueryCalls)
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
	orchestrator.rebuildBudgetExhausted(context.Background(), reloadedCfg, []domain.Issue{issue})
	state.TrackerOpsWg.Wait()

	if ShouldDispatch(issue, state, []string{"To Do"}, []string{"Done"}) {
		t.Error("ordinary polling would dispatch an absence-exhausted issue after restart")
	}
	if got := state.BudgetExhaustedReason[issueID]; got != budgetReasonHandoffAbsence {
		t.Errorf("BudgetExhaustedReason = %q, want %q", got, budgetReasonHandoffAbsence)
	}
	calls := tracker.labels()
	if len(calls) != 1 || calls[0].label != "captured-human-label" {
		t.Errorf("AddLabel calls = %+v, want captured-human-label despite reload", calls)
	}
}
