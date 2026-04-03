package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
)

// --- Test doubles ---

// mockCIProvider is a controllable CIStatusProvider for CI reconcile tests.
type mockCIProvider struct {
	result domain.CIResult
	err    error
	calls  int
}

var _ domain.CIStatusProvider = (*mockCIProvider)(nil)

func (m *mockCIProvider) FetchCIStatus(_ context.Context, _ string) (domain.CIResult, error) {
	m.calls++
	return m.result, m.err
}

// ciReconcileStore records calls to ReconcileStore methods and returns
// configurable errors. Parallel to mockReconcileStore but distinct so
// ci_reconcile_test.go is self-contained.
type ciReconcileStore struct {
	savedEntries    []persistence.RetryEntry
	deletedIssueIDs []string
	runHistories    []persistence.RunHistory

	saveRetryEntryErr   error
	deleteRetryEntryErr error
	appendRunHistoryErr error
}

var _ ReconcileStore = (*ciReconcileStore)(nil)

func (s *ciReconcileStore) SaveRetryEntry(_ context.Context, entry persistence.RetryEntry) error {
	s.savedEntries = append(s.savedEntries, entry)
	return s.saveRetryEntryErr
}

func (s *ciReconcileStore) DeleteRetryEntry(_ context.Context, issueID string) error {
	s.deletedIssueIDs = append(s.deletedIssueIDs, issueID)
	return s.deleteRetryEntryErr
}

func (s *ciReconcileStore) AppendRunHistory(_ context.Context, run persistence.RunHistory) (persistence.RunHistory, error) {
	s.runHistories = append(s.runHistories, run)
	return run, s.appendRunHistoryErr
}

// ciTrackerStub is a no-panic TrackerAdapter for CI reconcile tests.
// Escalation goroutines may call AddLabel or CommentIssue; all other
// methods return zero values.
type ciTrackerStub struct {
	addLabelCalled    int
	commentIssueCalls int
	addLabelErr       error
}

var _ domain.TrackerAdapter = (*ciTrackerStub)(nil)

func (s *ciTrackerStub) FetchIssuesByStates(_ context.Context, _ []string) ([]domain.Issue, error) {
	return nil, nil
}
func (s *ciTrackerStub) FetchCandidateIssues(_ context.Context) ([]domain.Issue, error) {
	return nil, nil
}
func (s *ciTrackerStub) FetchIssueByID(_ context.Context, _ string) (domain.Issue, error) {
	return domain.Issue{}, nil
}
func (s *ciTrackerStub) FetchIssueStatesByIDs(_ context.Context, _ []string) (map[string]string, error) {
	return nil, nil
}
func (s *ciTrackerStub) FetchIssueStatesByIdentifiers(_ context.Context, _ []string) (map[string]string, error) {
	return nil, nil
}
func (s *ciTrackerStub) FetchIssueComments(_ context.Context, _ string) ([]domain.Comment, error) {
	return nil, nil
}
func (s *ciTrackerStub) TransitionIssue(_ context.Context, _ string, _ string) error { return nil }
func (s *ciTrackerStub) CommentIssue(_ context.Context, _ string, _ string) error {
	s.commentIssueCalls++
	return nil
}
func (s *ciTrackerStub) AddLabel(_ context.Context, _ string, _ string) error {
	s.addLabelCalled++
	return s.addLabelErr
}

// ciMetricsSpy records calls to CI-specific metric methods while delegating
// all other methods to NoopMetrics.
type ciMetricsSpy struct {
	domain.NoopMetrics
	ciStatusChecks   map[string]int
	ciEscalations    map[string]int
	retriesByTrigger map[string]int
}

func newCIMetricsSpy() *ciMetricsSpy {
	return &ciMetricsSpy{
		ciStatusChecks:   make(map[string]int),
		ciEscalations:    make(map[string]int),
		retriesByTrigger: make(map[string]int),
	}
}

func (s *ciMetricsSpy) IncCIStatusChecks(result string) { s.ciStatusChecks[result]++ }
func (s *ciMetricsSpy) IncCIEscalations(action string)  { s.ciEscalations[action]++ }
func (s *ciMetricsSpy) IncRetries(trigger string)       { s.retriesByTrigger[trigger]++ }

// --- Test helpers ---

// ciBaseTime is a fixed reference for CI reconcile tests.
var ciBaseTime = time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)

// newPendingEntry builds a PendingCICheckEntry for a test issue.
func newPendingEntry(issueID, identifier, branch string, attempt int) *PendingCICheckEntry {
	return &PendingCICheckEntry{
		IssueID:    issueID,
		Identifier: identifier,
		DisplayID:  identifier,
		Attempt:    attempt,
		Branch:     branch,
		SHA:        "",
		CreatedAt:  ciBaseTime,
	}
}

// defaultCIFeedback returns a CIFeedbackConfig with max_retries=2, label escalation.
func defaultCIFeedback() config.CIFeedbackConfig {
	return config.CIFeedbackConfig{
		Kind:            "github",
		MaxRetries:      2,
		Escalation:      "label",
		EscalationLabel: "needs-human",
	}
}

// stateWithPendingCICheck creates a State with one PendingCICheck entry.
func stateWithPendingCICheck(t *testing.T, issueID, branch string, attempt int) *State {
	t.Helper()
	s := NewState(5000, 4, nil, AgentTotals{})
	s.PendingCICheck[issueID] = newPendingEntry(issueID, issueID+"-ident", branch, attempt)
	s.Claimed[issueID] = struct{}{}
	return s
}

// ciParams returns ReconcileParams wired for CI reconcile unit tests.
func ciParams(t *testing.T, store *ciReconcileStore, ci domain.CIStatusProvider, tracker domain.TrackerAdapter) ReconcileParams {
	t.Helper()
	return ReconcileParams{
		TrackerAdapter: tracker,
		CIProvider:     ci,
		CIFeedback:     defaultCIFeedback(),
		Store:          store,
		OnRetryFire:    noopRetryFire,
		Ctx:            context.Background(),
		Logger:         discardLogger(),
		ActiveStates:   []string{"In Progress"},
		TerminalStates: []string{"Done"},
	}
}

// --- Tests ---

func TestReconcileCIStatus_NilProvider(t *testing.T) {
	t.Parallel()

	state := stateWithPendingCICheck(t, "ISS-CI-1", "feature/fix", 1)
	store := &ciReconcileStore{}
	metrics := newCIMetricsSpy()
	params := ciParams(t, store, nil, nil)

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	// nil CIProvider: entire phase is a no-op.
	if _, ok := state.PendingCICheck["ISS-CI-1"]; !ok {
		t.Error("PendingCICheck entry consumed when CIProvider is nil; want no-op")
	}
	if len(metrics.ciStatusChecks) != 0 {
		t.Errorf("IncCIStatusChecks called with nil provider; want no calls")
	}
	if len(store.runHistories) != 0 {
		t.Errorf("AppendRunHistory called %d times with nil provider; want 0", len(store.runHistories))
	}
}

func TestReconcileCIStatus_FetchError_ReEnqueues(t *testing.T) {
	t.Parallel()

	state := stateWithPendingCICheck(t, "ISS-CI-2", "main", 1)
	store := &ciReconcileStore{}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{err: errors.New("network timeout")}
	params := ciParams(t, store, ci, nil)

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingCICheck["ISS-CI-2"]; !ok {
		t.Error("PendingCICheck entry dropped on FetchCIStatus error; want re-enqueued")
	}
	if metrics.ciStatusChecks["error"] != 1 {
		t.Errorf(`IncCIStatusChecks("error") = %d, want 1`, metrics.ciStatusChecks["error"])
	}
	if len(store.runHistories) != 0 {
		t.Errorf("AppendRunHistory called %d times on fetch error; want 0", len(store.runHistories))
	}
}

func TestReconcileCIStatus_Passing_ClearsCIFixAttempts(t *testing.T) {
	t.Parallel()

	state := stateWithPendingCICheck(t, "ISS-CI-3", "feature/done", 1)
	state.CIFixAttempts["ISS-CI-3"] = 1 // pre-seeded
	store := &ciReconcileStore{}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusPassing}}
	params := ciParams(t, store, ci, nil)

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingCICheck["ISS-CI-3"]; ok {
		t.Error("PendingCICheck entry still present after passing; want consumed")
	}
	if _, ok := state.CIFixAttempts["ISS-CI-3"]; ok {
		t.Error("CIFixAttempts not cleared after CI passing; want cleared")
	}
	if _, ok := state.RetryAttempts["ISS-CI-3"]; ok {
		t.Error("retry scheduled after CI passing; want none")
	}
	if metrics.ciStatusChecks["passing"] != 1 {
		t.Errorf(`IncCIStatusChecks("passing") = %d, want 1`, metrics.ciStatusChecks["passing"])
	}
}

func TestReconcileCIStatus_Pending_ReEnqueues(t *testing.T) {
	t.Parallel()

	state := stateWithPendingCICheck(t, "ISS-CI-4", "feature/wip", 1)
	store := &ciReconcileStore{}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusPending}}
	params := ciParams(t, store, ci, nil)

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingCICheck["ISS-CI-4"]; !ok {
		t.Error("PendingCICheck entry not re-enqueued after pending; want re-enqueued")
	}
	if _, ok := state.RetryAttempts["ISS-CI-4"]; ok {
		t.Error("retry scheduled after CI pending; want none")
	}
	if metrics.ciStatusChecks["pending"] != 1 {
		t.Errorf(`IncCIStatusChecks("pending") = %d, want 1`, metrics.ciStatusChecks["pending"])
	}
}

func TestReconcileCIStatus_Failing_UnderMaxRetries(t *testing.T) {
	t.Parallel()

	// CIFixAttempts starts at 0; maxRetries=2 → no escalation after increment to 1.
	state := stateWithPendingCICheck(t, "ISS-CI-5", "feature/break", 1)
	store := &ciReconcileStore{}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{
		Status:       domain.CIStatusFailing,
		FailingCount: 2,
		CheckRuns: []domain.CheckRun{
			{Name: "lint", Status: domain.CheckRunStatusCompleted, Conclusion: domain.CheckConclusionFailure},
		},
	}}
	params := ciParams(t, store, ci, nil)

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	// Entry consumed (not re-enqueued as pending-check).
	if _, ok := state.PendingCICheck["ISS-CI-5"]; ok {
		t.Error("PendingCICheck entry re-enqueued on CI failure; want consumed")
	}

	// RunHistory appended with "ci_failed".
	if len(store.runHistories) != 1 {
		t.Fatalf("AppendRunHistory call count = %d, want 1", len(store.runHistories))
	}
	if store.runHistories[0].Status != "ci_failed" {
		t.Errorf("RunHistory.Status = %q, want %q", store.runHistories[0].Status, "ci_failed")
	}
	if store.runHistories[0].IssueID != "ISS-CI-5" {
		t.Errorf("RunHistory.IssueID = %q, want %q", store.runHistories[0].IssueID, "ISS-CI-5")
	}

	// SaveRetryEntry NOT called: CI fix retries are in-memory until HandleRetryTimer.
	if len(store.savedEntries) != 0 {
		t.Errorf("SaveRetryEntry called %d times; want 0 (in-memory retry only)", len(store.savedEntries))
	}

	// In-memory retry scheduled with CI failure context.
	entry, ok := state.RetryAttempts["ISS-CI-5"]
	if !ok {
		t.Fatal("retry not scheduled after CI failure; want scheduled")
	}
	if entry.CIFailureContext == nil {
		t.Error("RetryEntry.CIFailureContext is nil; want CI failure map")
	}

	// CIFixAttempts incremented.
	if state.CIFixAttempts["ISS-CI-5"] != 1 {
		t.Errorf("CIFixAttempts[ISS-CI-5] = %d, want 1", state.CIFixAttempts["ISS-CI-5"])
	}

	// Metrics.
	if metrics.ciStatusChecks["failing"] != 1 {
		t.Errorf(`IncCIStatusChecks("failing") = %d, want 1`, metrics.ciStatusChecks["failing"])
	}
	if metrics.retriesByTrigger["ci_fix"] != 1 {
		t.Errorf(`IncRetries("ci_fix") = %d, want 1`, metrics.retriesByTrigger["ci_fix"])
	}

	// Claim preserved.
	if _, ok := state.Claimed["ISS-CI-5"]; !ok {
		t.Error("claim released after CI failure under max retries; want preserved")
	}
}

func TestReconcileCIStatus_Failing_ExceedsMaxRetries_Escalates(t *testing.T) {
	t.Parallel()

	// CIFixAttempts at 2; after increment → 3 > maxRetries(2) → escalate.
	state := stateWithPendingCICheck(t, "ISS-CI-6", "feature/broken", 3)
	state.CIFixAttempts["ISS-CI-6"] = 2
	store := &ciReconcileStore{}
	metrics := newCIMetricsSpy()
	tracker := &ciTrackerStub{}
	ci := &mockCIProvider{result: domain.CIResult{
		Status:       domain.CIStatusFailing,
		FailingCount: 1,
	}}
	params := ciParams(t, store, ci, tracker)

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	// DeleteRetryEntry called for the issue.
	if len(store.deletedIssueIDs) != 1 || store.deletedIssueIDs[0] != "ISS-CI-6" {
		t.Errorf("DeleteRetryEntry calls = %v, want [ISS-CI-6]", store.deletedIssueIDs)
	}

	// Claim released.
	if _, ok := state.Claimed["ISS-CI-6"]; ok {
		t.Error("claim not released after CI escalation; want released")
	}

	// CIFixAttempts cleared.
	if _, ok := state.CIFixAttempts["ISS-CI-6"]; ok {
		t.Error("CIFixAttempts not cleared after escalation; want cleared")
	}

	// Escalation metric incremented (label mode from defaultCIFeedback).
	if metrics.ciEscalations["label"] != 1 {
		t.Errorf(`IncCIEscalations("label") = %d, want 1`, metrics.ciEscalations["label"])
	}

	// RunHistory appended for the failing attempt.
	if len(store.runHistories) != 1 {
		t.Fatalf("AppendRunHistory call count = %d, want 1", len(store.runHistories))
	}

	// No retry scheduled.
	if _, ok := state.RetryAttempts["ISS-CI-6"]; ok {
		t.Error("retry scheduled after escalation; want none")
	}
}

func TestReconcileCIStatus_Failing_CommentEscalation(t *testing.T) {
	t.Parallel()

	state := stateWithPendingCICheck(t, "ISS-CI-7", "main", 1)
	state.CIFixAttempts["ISS-CI-7"] = 2
	store := &ciReconcileStore{}
	metrics := newCIMetricsSpy()
	tracker := &ciTrackerStub{}
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusFailing}}
	params := ciParams(t, store, ci, tracker)
	params.CIFeedback.Escalation = "comment"

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	if metrics.ciEscalations["comment"] != 1 {
		t.Errorf(`IncCIEscalations("comment") = %d, want 1`, metrics.ciEscalations["comment"])
	}
	// Claim released and retry cleared in both escalation modes.
	if _, ok := state.Claimed["ISS-CI-7"]; ok {
		t.Error("claim not released after comment escalation")
	}
}

func TestBuildCIEscalationComment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		result         domain.CIResult
		ref            string
		attempts       int
		wantContains   []string
		wantNotContain []string
	}{
		{
			name: "basic format with failing count and details URL",
			result: domain.CIResult{
				FailingCount: 2,
				CheckRuns: []domain.CheckRun{
					{
						Name:       "test",
						Status:     domain.CheckRunStatusCompleted,
						Conclusion: domain.CheckConclusionFailure,
						DetailsURL: "https://ci.example.com/runs/42",
					},
				},
			},
			ref:      "abc1234",
			attempts: 3,
			wantContains: []string{
				"CI fix retries exhausted",
				"abc1234",
				"3 CI-fix continuation(s)",
				"Failing checks:",
				"2",
				"test",
				"https://ci.example.com/runs/42",
				"Manual intervention required",
			},
		},
		{
			name: "zero failing count omits count line",
			result: domain.CIResult{
				FailingCount: 0,
				CheckRuns:    []domain.CheckRun{},
			},
			ref:      "main",
			attempts: 1,
			wantContains: []string{
				"CI fix retries exhausted",
				"main",
				"1 CI-fix continuation(s)",
				"Manual intervention required",
			},
		},
		{
			name: "only failure check runs included",
			result: domain.CIResult{
				FailingCount: 3,
				CheckRuns: []domain.CheckRun{
					{Name: "lint", Status: domain.CheckRunStatusCompleted, Conclusion: domain.CheckConclusionSuccess},
					{Name: "test", Status: domain.CheckRunStatusCompleted, Conclusion: domain.CheckConclusionFailure},
					{Name: "deploy", Status: domain.CheckRunStatusCompleted, Conclusion: domain.CheckConclusionTimedOut},
					{Name: "e2e", Status: domain.CheckRunStatusCompleted, Conclusion: domain.CheckConclusionCancelled},
				},
			},
			ref:            "feature/x",
			attempts:       2,
			wantContains:   []string{"test", "deploy", "e2e"},
			wantNotContain: []string{"lint"},
		},
		{
			name: "check run without details URL omits link",
			result: domain.CIResult{
				FailingCount: 1,
				CheckRuns: []domain.CheckRun{
					{Name: "build", Status: domain.CheckRunStatusCompleted, Conclusion: domain.CheckConclusionFailure, DetailsURL: ""},
				},
			},
			ref:            "sha9999",
			attempts:       1,
			wantContains:   []string{"build"},
			wantNotContain: []string{"details"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := buildCIEscalationComment(tt.result, tt.ref, tt.attempts)

			for _, want := range tt.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("buildCIEscalationComment() output missing %q\ngot: %s", want, got)
				}
			}
			for _, notWant := range tt.wantNotContain {
				if strings.Contains(got, notWant) {
					t.Errorf("buildCIEscalationComment() output unexpectedly contains %q\ngot: %s", notWant, got)
				}
			}
		})
	}
}

// --- TrackerOpsWg lifecycle tests ---

// blockingCITracker is a TrackerAdapter whose AddLabel and CommentIssue
// methods block on channel gates. Used to pace fire-and-forget goroutines
// spawned by escalateCIFailure so TrackerOpsWg tracking can be verified.
type blockingCITracker struct {
	addLabelGate chan struct{} // if non-nil, AddLabel blocks until closed
	commentGate  chan struct{} // if non-nil, CommentIssue blocks until closed
}

var _ domain.TrackerAdapter = (*blockingCITracker)(nil)

func (b *blockingCITracker) FetchIssuesByStates(_ context.Context, _ []string) ([]domain.Issue, error) {
	return nil, nil
}
func (b *blockingCITracker) FetchCandidateIssues(_ context.Context) ([]domain.Issue, error) {
	return nil, nil
}
func (b *blockingCITracker) FetchIssueByID(_ context.Context, _ string) (domain.Issue, error) {
	return domain.Issue{}, nil
}
func (b *blockingCITracker) FetchIssueStatesByIDs(_ context.Context, _ []string) (map[string]string, error) {
	return nil, nil
}
func (b *blockingCITracker) FetchIssueStatesByIdentifiers(_ context.Context, _ []string) (map[string]string, error) {
	return nil, nil
}
func (b *blockingCITracker) FetchIssueComments(_ context.Context, _ string) ([]domain.Comment, error) {
	return nil, nil
}
func (b *blockingCITracker) TransitionIssue(_ context.Context, _ string, _ string) error {
	return nil
}
func (b *blockingCITracker) AddLabel(ctx context.Context, _ string, _ string) error {
	if b.addLabelGate != nil {
		select {
		case <-b.addLabelGate:
		case <-ctx.Done():
		}
	}
	return nil
}
func (b *blockingCITracker) CommentIssue(ctx context.Context, _ string, _ string) error {
	if b.commentGate != nil {
		select {
		case <-b.commentGate:
		case <-ctx.Done():
		}
	}
	return nil
}

// TestEscalateCIFailure_LabelTracksTrackerOps verifies that the AddLabel
// goroutine spawned by the label escalation path increments TrackerOpsWg
// before starting and decrements it on return, so Wait() blocks during the
// call and resolves once it completes.
func TestEscalateCIFailure_LabelTracksTrackerOps(t *testing.T) {
	t.Parallel()

	gate := make(chan struct{})
	tracker := &blockingCITracker{addLabelGate: gate}

	// CIFixAttempts=2 with maxRetries=2 means next increment (→3) exceeds
	// the limit and triggers escalation.
	state := stateWithPendingCICheck(t, "ESC-WG-1", "main/broken", 3)
	state.CIFixAttempts["ESC-WG-1"] = 2
	store := &ciReconcileStore{}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusFailing}}
	params := ciParams(t, store, ci, tracker)
	// defaultCIFeedback sets escalation: "label".

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	waitDone := make(chan struct{})
	go func() {
		state.TrackerOpsWg.Wait()
		close(waitDone)
	}()

	// TrackerOpsWg must not resolve while AddLabel blocks on the gate.
	select {
	case <-waitDone:
		t.Fatal("TrackerOpsWg.Wait() returned before AddLabel goroutine completed")
	case <-time.After(20 * time.Millisecond):
	}

	// Release the gate to let AddLabel return and Done() fire.
	close(gate)

	select {
	case <-waitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("TrackerOpsWg.Wait() did not return after AddLabel goroutine completed")
	}
}

// TestEscalateCIFailure_CommentTracksTrackerOps verifies that the
// CommentIssue goroutine spawned by the comment escalation path increments
// TrackerOpsWg before starting and decrements it on return.
func TestEscalateCIFailure_CommentTracksTrackerOps(t *testing.T) {
	t.Parallel()

	gate := make(chan struct{})
	tracker := &blockingCITracker{commentGate: gate}

	state := stateWithPendingCICheck(t, "ESC-WG-2", "feature/broken", 2)
	state.CIFixAttempts["ESC-WG-2"] = 2
	store := &ciReconcileStore{}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusFailing}}
	params := ciParams(t, store, ci, tracker)
	params.CIFeedback.Escalation = "comment"

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	waitDone := make(chan struct{})
	go func() {
		state.TrackerOpsWg.Wait()
		close(waitDone)
	}()

	// TrackerOpsWg must not resolve while CommentIssue blocks on the gate.
	select {
	case <-waitDone:
		t.Fatal("TrackerOpsWg.Wait() returned before CommentIssue goroutine completed")
	case <-time.After(20 * time.Millisecond):
	}

	// Release the gate to let CommentIssue return and Done() fire.
	close(gate)

	select {
	case <-waitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("TrackerOpsWg.Wait() did not return after CommentIssue goroutine completed")
	}
}
