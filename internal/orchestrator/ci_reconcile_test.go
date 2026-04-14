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

	saveRetryEntryErr                    error
	deleteRetryEntryErr                  error
	appendRunHistoryErr                  error
	deleteReactionFingerprintsByIssueErr error

	// Fingerprint dedup fields.
	upsertFingerprintCalls int
	getFingerprintCalls    int
	markDispatchedCalls    int
	deleteFingerprintCalls int

	upsertFingerprintErr     error
	getFingerprintResult     string
	getFingerprintDispatched bool
	getFingerprintErr        error
	markDispatchedErr        error
	deleteFingerprintErr     error
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

func (s *ciReconcileStore) DeleteReactionFingerprintsByIssue(_ context.Context, _ string) error {
	return s.deleteReactionFingerprintsByIssueErr
}

func (s *ciReconcileStore) UpsertReactionFingerprint(_ context.Context, _, _, _ string) error {
	s.upsertFingerprintCalls++
	return s.upsertFingerprintErr
}

func (s *ciReconcileStore) GetReactionFingerprint(_ context.Context, _, _ string) (string, bool, error) {
	s.getFingerprintCalls++
	return s.getFingerprintResult, s.getFingerprintDispatched, s.getFingerprintErr
}

func (s *ciReconcileStore) MarkReactionDispatched(_ context.Context, _, _ string) error {
	s.markDispatchedCalls++
	return s.markDispatchedErr
}

func (s *ciReconcileStore) DeleteReactionFingerprint(_ context.Context, _, _ string) error {
	s.deleteFingerprintCalls++
	return s.deleteFingerprintErr
}

// ciTrackerStub is a no-panic TrackerAdapter for CI reconcile tests.
// Escalation goroutines may call AddLabel or CommentIssue; all other
// methods return zero values.
type ciTrackerStub struct {
	addLabelCalled    int
	commentIssueCalls int
	addLabelErr       error
	commentIssueErr   error
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
	return s.commentIssueErr
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

// newPendingEntry builds a PendingReaction for a test CI issue.
func newPendingEntry(issueID, identifier, branch string, attempt int) *PendingReaction {
	return &PendingReaction{
		IssueID:    issueID,
		Identifier: identifier,
		DisplayID:  identifier,
		Attempt:    attempt,
		Kind:       ReactionKindCI,
		CreatedAt:  ciBaseTime,
		KindData: &CIReactionData{
			Branch: branch,
		},
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

// stateWithPendingReaction creates a State with one CI PendingReaction entry.
func stateWithPendingReaction(t *testing.T, issueID, branch string, attempt int) *State {
	t.Helper()
	s := NewState(5000, 4, nil, AgentTotals{})
	rkey := ReactionKey(issueID, ReactionKindCI)
	s.PendingReactions[rkey] = newPendingEntry(issueID, issueID+"-ident", branch, attempt)
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

	state := stateWithPendingReaction(t, "ISS-CI-1", "feature/fix", 1)
	store := &ciReconcileStore{}
	metrics := newCIMetricsSpy()
	params := ciParams(t, store, nil, nil)

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	// nil CIProvider: entire phase is a no-op.
	if _, ok := state.PendingReactions[ReactionKey("ISS-CI-1", ReactionKindCI)]; !ok {
		t.Error("PendingReactions entry consumed when CIProvider is nil; want no-op")
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

	state := stateWithPendingReaction(t, "ISS-CI-2", "main", 1)
	store := &ciReconcileStore{}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{err: errors.New("network timeout")}
	params := ciParams(t, store, ci, nil)

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[ReactionKey("ISS-CI-2", ReactionKindCI)]; !ok {
		t.Error("PendingReactions entry dropped on FetchCIStatus error; want re-enqueued")
	}
	if metrics.ciStatusChecks["error"] != 1 {
		t.Errorf(`IncCIStatusChecks("error") = %d, want 1`, metrics.ciStatusChecks["error"])
	}
	if len(store.runHistories) != 0 {
		t.Errorf("AppendRunHistory called %d times on fetch error; want 0", len(store.runHistories))
	}
}

func TestReconcileCIStatus_Passing_ClearsReactionAttempts(t *testing.T) {
	t.Parallel()

	state := stateWithPendingReaction(t, "ISS-CI-3", "feature/done", 1)
	state.ReactionAttempts[ReactionKey("ISS-CI-3", ReactionKindCI)] = 1 // pre-seeded
	store := &ciReconcileStore{}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusPassing}}
	params := ciParams(t, store, ci, nil)

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[ReactionKey("ISS-CI-3", ReactionKindCI)]; ok {
		t.Error("PendingReactions entry still present after passing; want consumed")
	}
	if _, ok := state.ReactionAttempts[ReactionKey("ISS-CI-3", ReactionKindCI)]; ok {
		t.Error("ReactionAttempts not cleared after CI passing; want cleared")
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

	state := stateWithPendingReaction(t, "ISS-CI-4", "feature/wip", 1)
	store := &ciReconcileStore{}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusPending}}
	params := ciParams(t, store, ci, nil)

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[ReactionKey("ISS-CI-4", ReactionKindCI)]; !ok {
		t.Error("PendingReactions entry not re-enqueued after pending; want re-enqueued")
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

	// ReactionAttempts starts at 0; maxRetries=2 → no escalation after increment to 1.
	state := stateWithPendingReaction(t, "ISS-CI-5", "feature/break", 1)
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
	if _, ok := state.PendingReactions[ReactionKey("ISS-CI-5", ReactionKindCI)]; ok {
		t.Error("PendingReactions entry re-enqueued on CI failure; want consumed")
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
	if entry.ContinuationContext == nil {
		t.Error("RetryEntry.ContinuationContext is nil; want continuation map")
	}

	// ReactionAttempts incremented.
	if state.ReactionAttempts[ReactionKey("ISS-CI-5", ReactionKindCI)] != 1 {
		t.Errorf("ReactionAttempts[ISS-CI-5] = %d, want 1", state.ReactionAttempts[ReactionKey("ISS-CI-5", ReactionKindCI)])
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

	// ReactionAttempts at 2; after increment → 3 > maxRetries(2) → escalate.
	state := stateWithPendingReaction(t, "ISS-CI-6", "feature/broken", 3)
	state.ReactionAttempts[ReactionKey("ISS-CI-6", ReactionKindCI)] = 2
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

	// ReactionAttempts cleared.
	if _, ok := state.ReactionAttempts[ReactionKey("ISS-CI-6", ReactionKindCI)]; ok {
		t.Error("ReactionAttempts not cleared after escalation; want cleared")
	}

	// Wait for the async escalation goroutine before reading metrics.
	state.TrackerOpsWg.Wait()

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

	state := stateWithPendingReaction(t, "ISS-CI-7", "main", 1)
	state.ReactionAttempts[ReactionKey("ISS-CI-7", ReactionKindCI)] = 2
	store := &ciReconcileStore{}
	metrics := newCIMetricsSpy()
	tracker := &ciTrackerStub{}
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusFailing}}
	params := ciParams(t, store, ci, tracker)
	params.CIFeedback.Escalation = "comment"

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	// Wait for the async escalation goroutine before reading metrics.
	state.TrackerOpsWg.Wait()

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

	// ReactionAttempts=2 with maxRetries=2 means next increment (→3) exceeds
	// the limit and triggers escalation.
	state := stateWithPendingReaction(t, "ESC-WG-1", "main/broken", 3)
	state.ReactionAttempts[ReactionKey("ESC-WG-1", ReactionKindCI)] = 2
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

	state := stateWithPendingReaction(t, "ESC-WG-2", "feature/broken", 2)
	state.ReactionAttempts[ReactionKey("ESC-WG-2", ReactionKindCI)] = 2
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

// --- Backoff and TTL tests ---

func TestComputeCIPendingDelay(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		attempts int
		want     time.Duration
	}{
		{"attempt 0 is immediate", 0, 0},
		{"attempt 1 is base×2", 1, 20 * time.Second},
		{"attempt 2 is base×4", 2, 40 * time.Second},
		{"attempt 3 is base×8", 3, 80 * time.Second},
		{"attempt 4 is base×16", 4, 160 * time.Second},
		{"attempt 5 is capped at 5 minutes", 5, 5 * time.Minute},
		{"large attempt is capped at 5 minutes", 100, 5 * time.Minute},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := computeCIPendingDelay(ciPendingBackoffBaseDefault, tt.attempts)
			if got != tt.want {
				t.Errorf("computeCIPendingDelay(ciPendingBackoffBaseDefault, %d) = %v, want %v", tt.attempts, got, tt.want)
			}
		})
	}
}

func TestComputeCIPendingDelay_CustomBase(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		base     time.Duration
		attempts int
		want     time.Duration
	}{
		// 5s base: 5s * 2^n
		{"5s base attempt 1", 5 * time.Second, 1, 10 * time.Second},
		{"5s base attempt 2", 5 * time.Second, 2, 20 * time.Second},
		{"5s base attempt 3", 5 * time.Second, 3, 40 * time.Second},
		{"5s base attempt 4", 5 * time.Second, 4, 80 * time.Second},
		{"5s base attempt 5", 5 * time.Second, 5, 160 * time.Second},
		{"5s base attempt 6 capped", 5 * time.Second, 6, ciPendingBackoffCap},
		// 30s base: 30s * 2^n
		{"30s base attempt 1", 30 * time.Second, 1, 60 * time.Second},
		{"30s base attempt 2", 30 * time.Second, 2, 120 * time.Second},
		{"30s base attempt 3", 30 * time.Second, 3, 240 * time.Second},
		{"30s base attempt 4 capped", 30 * time.Second, 4, ciPendingBackoffCap},
		{"30s base attempt 5 capped", 30 * time.Second, 5, ciPendingBackoffCap},
		// 60s base: 60s * 2^n
		{"60s base attempt 1", 60 * time.Second, 1, 120 * time.Second},
		{"60s base attempt 2", 60 * time.Second, 2, 240 * time.Second},
		{"60s base attempt 3 capped", 60 * time.Second, 3, ciPendingBackoffCap},
		// large base already exceeds cap on attempt 1
		{"4m base attempt 1 capped", 4 * time.Minute, 1, ciPendingBackoffCap},
		// large attempt value always caps regardless of base
		{"large attempt capped", 30 * time.Second, 100, ciPendingBackoffCap},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := computeCIPendingDelay(tt.base, tt.attempts)
			if got != tt.want {
				t.Errorf("computeCIPendingDelay(%v, %d) = %v, want %v", tt.base, tt.attempts, got, tt.want)
			}
		})
	}
}

func TestComputeCIPendingDelay_ZeroBase(t *testing.T) {
	t.Parallel()

	// Zero and negative base must fall back to ciPendingBackoffBaseDefault.
	bases := []time.Duration{0, -1 * time.Second, -5 * time.Second}

	for _, base := range bases {
		t.Run(base.String(), func(t *testing.T) {
			t.Parallel()

			for attempts := 0; attempts <= 5; attempts++ {
				got := computeCIPendingDelay(base, attempts)
				want := computeCIPendingDelay(ciPendingBackoffBaseDefault, attempts)
				if got != want {
					t.Errorf("computeCIPendingDelay(%v, %d) = %v, want %v (same as default base)", base, attempts, got, want)
				}
			}
		})
	}
}

func TestReconcileCIStatus_BackoffUsesStatePollInterval(t *testing.T) {
	t.Parallel()

	now := ciBaseTime

	// Use a non-default poll interval of 30s (30000ms).
	entry := newPendingEntry("ISS-PPI-1", "ISS-PPI-1-ident", "feature/ppi", 1)
	entry.PendingAttempts = 1

	state := NewState(30000, 4, nil, AgentTotals{})
	state.PendingReactions[ReactionKey("ISS-PPI-1", ReactionKindCI)] = entry
	state.Claimed["ISS-PPI-1"] = struct{}{}

	store := &ciReconcileStore{}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusPending}}
	params := ciParams(t, store, ci, nil)
	params.NowFunc = func() time.Time { return now }

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	got, ok := state.PendingReactions[ReactionKey("ISS-PPI-1", ReactionKindCI)]
	if !ok {
		t.Fatal("PendingReactions entry not re-enqueued after CIStatusPending; want re-enqueued")
	}

	wantAttempts := 2
	if got.PendingAttempts != wantAttempts {
		t.Errorf("PendingAttempts = %d, want %d", got.PendingAttempts, wantAttempts)
	}

	// With a 30s poll interval, retry at = now + 30s * 2^2 = now + 120s.
	wantDelay := computeCIPendingDelay(30*time.Second, wantAttempts)
	wantRetryAt := now.Add(wantDelay)
	if !got.PendingRetryAt.Equal(wantRetryAt) {
		t.Errorf("PendingRetryAt = %v, want %v (30s base backoff)", got.PendingRetryAt, wantRetryAt)
	}

	// Confirm it is NOT the old 10s-based schedule.
	oldSchedule := now.Add(computeCIPendingDelay(ciPendingBackoffBaseDefault, wantAttempts))
	if got.PendingRetryAt.Equal(oldSchedule) {
		t.Errorf("PendingRetryAt = %v matches old 10s-base schedule; want 30s-base schedule", got.PendingRetryAt)
	}
}

func TestReconcileCIStatus_BackoffSkip(t *testing.T) {
	t.Parallel()

	now := ciBaseTime
	futureRetry := now.Add(2 * time.Minute)

	entry := newPendingEntry("ISS-SKIP-1", "ISS-SKIP-1-ident", "feature/skip", 1)
	entry.PendingAttempts = 2
	entry.PendingRetryAt = futureRetry

	state := NewState(5000, 4, nil, AgentTotals{})
	state.PendingReactions[ReactionKey("ISS-SKIP-1", ReactionKindCI)] = entry
	state.Claimed["ISS-SKIP-1"] = struct{}{}

	store := &ciReconcileStore{}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{}
	params := ciParams(t, store, ci, nil)
	params.NowFunc = func() time.Time { return now }

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	// FetchCIStatus must not be called when PendingRetryAt is in the future.
	if ci.calls != 0 {
		t.Errorf("FetchCIStatus called %d times during backoff window; want 0", ci.calls)
	}

	// Entry must be re-enqueued with identical PendingAttempts and PendingRetryAt.
	got, ok := state.PendingReactions[ReactionKey("ISS-SKIP-1", ReactionKindCI)]
	if !ok {
		t.Fatal("PendingReactions entry dropped during backoff skip; want re-enqueued")
	}
	if got.PendingAttempts != 2 {
		t.Errorf("PendingAttempts = %d, want 2 (unchanged)", got.PendingAttempts)
	}
	if !got.PendingRetryAt.Equal(futureRetry) {
		t.Errorf("PendingRetryAt = %v, want %v (unchanged)", got.PendingRetryAt, futureRetry)
	}
}

func TestReconcileCIStatus_BackoffIncrements_OnPending(t *testing.T) {
	t.Parallel()

	now := ciBaseTime

	entry := newPendingEntry("ISS-BIP-1", "ISS-BIP-1-ident", "feature/wip", 1)
	entry.PendingAttempts = 2

	state := NewState(5000, 4, nil, AgentTotals{})
	state.PendingReactions[ReactionKey("ISS-BIP-1", ReactionKindCI)] = entry
	state.Claimed["ISS-BIP-1"] = struct{}{}

	store := &ciReconcileStore{}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusPending}}
	params := ciParams(t, store, ci, nil)
	params.NowFunc = func() time.Time { return now }

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	got, ok := state.PendingReactions[ReactionKey("ISS-BIP-1", ReactionKindCI)]
	if !ok {
		t.Fatal("PendingReactions entry not re-enqueued after CIStatusPending; want re-enqueued")
	}

	wantAttempts := 3
	if got.PendingAttempts != wantAttempts {
		t.Errorf("PendingAttempts = %d, want %d", got.PendingAttempts, wantAttempts)
	}

	wantRetryAt := now.Add(computeCIPendingDelay(5*time.Second, wantAttempts))
	if !got.PendingRetryAt.Equal(wantRetryAt) {
		t.Errorf("PendingRetryAt = %v, want %v", got.PendingRetryAt, wantRetryAt)
	}

	if metrics.ciStatusChecks["pending"] != 1 {
		t.Errorf(`IncCIStatusChecks("pending") = %d, want 1`, metrics.ciStatusChecks["pending"])
	}
}

func TestReconcileCIStatus_BackoffIncrements_OnError(t *testing.T) {
	t.Parallel()

	now := ciBaseTime

	entry := newPendingEntry("ISS-BIE-1", "ISS-BIE-1-ident", "feature/err", 1)
	entry.PendingAttempts = 1

	state := NewState(5000, 4, nil, AgentTotals{})
	state.PendingReactions[ReactionKey("ISS-BIE-1", ReactionKindCI)] = entry
	state.Claimed["ISS-BIE-1"] = struct{}{}

	store := &ciReconcileStore{}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{err: errors.New("transient network error")}
	params := ciParams(t, store, ci, nil)
	params.NowFunc = func() time.Time { return now }

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	got, ok := state.PendingReactions[ReactionKey("ISS-BIE-1", ReactionKindCI)]
	if !ok {
		t.Fatal("PendingReactions entry not re-enqueued after fetch error; want re-enqueued")
	}

	wantAttempts := 2
	if got.PendingAttempts != wantAttempts {
		t.Errorf("PendingAttempts = %d, want %d", got.PendingAttempts, wantAttempts)
	}

	wantRetryAt := now.Add(computeCIPendingDelay(5*time.Second, wantAttempts))
	if !got.PendingRetryAt.Equal(wantRetryAt) {
		t.Errorf("PendingRetryAt = %v, want %v", got.PendingRetryAt, wantRetryAt)
	}

	if metrics.ciStatusChecks["error"] != 1 {
		t.Errorf(`IncCIStatusChecks("error") = %d, want 1`, metrics.ciStatusChecks["error"])
	}
}

func TestReconcileCIStatus_TTLExpiry(t *testing.T) {
	t.Parallel()

	const ttl = 30 * time.Minute

	tests := []struct {
		name        string
		age         time.Duration
		wantDropped bool
	}{
		{"entry within TTL is kept", ttl - time.Second, false},
		{"entry exactly at TTL boundary is kept", ttl, false},
		{"entry just past TTL is dropped", ttl + time.Millisecond, true},
		{"entry well past TTL is dropped", 2 * ttl, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			createdAt := ciBaseTime
			now := createdAt.Add(tt.age)

			entry := newPendingEntry("ISS-TTL-1", "ISS-TTL-1-ident", "main", 1)
			entry.CreatedAt = createdAt

			state := NewState(5000, 4, nil, AgentTotals{})
			state.PendingReactions[ReactionKey("ISS-TTL-1", ReactionKindCI)] = entry
			state.Claimed["ISS-TTL-1"] = struct{}{}

			store := &ciReconcileStore{}
			metrics := newCIMetricsSpy()
			ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusPending}}
			params := ciParams(t, store, ci, nil)
			params.NowFunc = func() time.Time { return now }
			params.CIPendingTTL = ttl

			reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

			_, stillPresent := state.PendingReactions[ReactionKey("ISS-TTL-1", ReactionKindCI)]

			if tt.wantDropped && stillPresent {
				t.Error("PendingReactions entry not dropped after TTL expiry; want dropped")
			}
			if !tt.wantDropped && !stillPresent {
				t.Error("PendingReactions entry dropped before TTL expiry; want kept")
			}
			if tt.wantDropped && ci.calls != 0 {
				t.Errorf("FetchCIStatus called %d times after TTL expiry; want 0", ci.calls)
			}
		})
	}
}

// --- Fingerprint dedup tests ---

// TestReconcileCIStatus_DedupSkip verifies that when GetReactionFingerprint
// returns the current ref with dispatched=true, the entry is consumed without
// calling FetchCIStatus.
func TestReconcileCIStatus_DedupSkip(t *testing.T) {
	t.Parallel()

	const ref = "sha-already-done"
	state := stateWithPendingReaction(t, "ISS-FP-1", ref, 1)
	store := &ciReconcileStore{
		getFingerprintResult:     ref,
		getFingerprintDispatched: true,
	}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{}
	params := ciParams(t, store, ci, nil)

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	if ci.calls != 0 {
		t.Errorf("FetchCIStatus called %d times when entry already dispatched; want 0", ci.calls)
	}
	if _, ok := state.PendingReactions[ReactionKey("ISS-FP-1", ReactionKindCI)]; ok {
		t.Error("PendingReactions entry still present after dedup skip; want consumed")
	}
	if store.upsertFingerprintCalls != 1 {
		t.Errorf("UpsertReactionFingerprint calls = %d, want 1", store.upsertFingerprintCalls)
	}
	if store.getFingerprintCalls != 1 {
		t.Errorf("GetReactionFingerprint calls = %d, want 1", store.getFingerprintCalls)
	}
}

// TestReconcileCIStatus_FingerprintReset verifies that when the stored
// fingerprint differs from the current ref (ref changed), UpsertReactionFingerprint
// is called and reconciliation proceeds (FetchCIStatus is called).
func TestReconcileCIStatus_FingerprintReset(t *testing.T) {
	t.Parallel()

	// Store returns old ref as dispatched; entry's branch is the new ref.
	const newRef = "sha-new"
	state := stateWithPendingReaction(t, "ISS-FP-2", newRef, 1)
	store := &ciReconcileStore{
		getFingerprintResult:     "sha-old",
		getFingerprintDispatched: true,
	}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusPassing}}
	params := ciParams(t, store, ci, nil)

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	if ci.calls != 1 {
		t.Errorf("FetchCIStatus calls = %d, want 1 (ref changed, dedup must not skip)", ci.calls)
	}
	if store.upsertFingerprintCalls != 1 {
		t.Errorf("UpsertReactionFingerprint calls = %d, want 1", store.upsertFingerprintCalls)
	}
}

// TestReconcileCIStatus_Passing_DeletesFingerprint verifies that on a
// CI-passing result DeleteReactionFingerprint is called.
func TestReconcileCIStatus_Passing_DeletesFingerprint(t *testing.T) {
	t.Parallel()

	state := stateWithPendingReaction(t, "ISS-FP-3", "sha-pass", 1)
	store := &ciReconcileStore{}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusPassing}}
	params := ciParams(t, store, ci, nil)

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	if store.deleteFingerprintCalls != 1 {
		t.Errorf("DeleteReactionFingerprint calls = %d, want 1 on CI pass", store.deleteFingerprintCalls)
	}
}

// TestReconcileCIStatus_FingerprintGetError_Continues verifies that when
// GetReactionFingerprint returns an error the reconcile loop continues and
// FetchCIStatus is still called (best-effort dedup pattern).
func TestReconcileCIStatus_FingerprintGetError_Continues(t *testing.T) {
	t.Parallel()

	state := stateWithPendingReaction(t, "ISS-FP-4", "sha-fperr", 1)
	store := &ciReconcileStore{
		getFingerprintErr: errors.New("db unavailable"),
	}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusPassing}}
	params := ciParams(t, store, ci, nil)

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	if ci.calls != 1 {
		t.Errorf("FetchCIStatus calls = %d, want 1 even when GetReactionFingerprint errors", ci.calls)
	}
}

// TestReconcileCIStatus_Failing_DoesNotMarkDispatched verifies that
// handleCIFailure no longer calls MarkReactionDispatched at schedule time.
// The mark is deferred to HandleRetryTimer after actual dispatch.
func TestReconcileCIStatus_Failing_DoesNotMarkDispatched(t *testing.T) {
	t.Parallel()

	// ReactionAttempts=0 → under maxRetries=2, so handleCIFailure schedules retry.
	state := stateWithPendingReaction(t, "ISS-FP-5", "sha-fail", 1)
	store := &ciReconcileStore{}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusFailing, FailingCount: 1}}
	params := ciParams(t, store, ci, nil)

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	// MarkReactionDispatched must NOT be called at schedule time.
	if store.markDispatchedCalls != 0 {
		t.Errorf("MarkReactionDispatched calls = %d, want 0 (mark deferred to dispatch site)", store.markDispatchedCalls)
	}
	// Retry must still be scheduled.
	if _, ok := state.RetryAttempts["ISS-FP-5"]; !ok {
		t.Error("retry not scheduled after CI failure; want scheduled")
	}
}

// TestEscalateCIFailure_LabelFailure_IncrementsErrorMetric verifies that when
// AddLabel returns an error the escalation goroutine increments
// IncCIEscalations("error") and does not increment IncCIEscalations("label").
func TestEscalateCIFailure_LabelFailure_IncrementsErrorMetric(t *testing.T) {
	t.Parallel()

	tracker := &ciTrackerStub{addLabelErr: errors.New("tracker unavailable")}

	state := stateWithPendingReaction(t, "ESC-ERR-1", "main/broken", 3)
	state.ReactionAttempts[ReactionKey("ESC-ERR-1", ReactionKindCI)] = 2
	store := &ciReconcileStore{}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusFailing}}
	params := ciParams(t, store, ci, tracker)
	// defaultCIFeedback sets escalation: "label".

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	if metrics.ciEscalations["error"] != 1 {
		t.Errorf(`IncCIEscalations("error") = %d, want 1 on label tracker failure`, metrics.ciEscalations["error"])
	}
	if metrics.ciEscalations["label"] != 0 {
		t.Errorf(`IncCIEscalations("label") = %d, want 0 on label tracker failure`, metrics.ciEscalations["label"])
	}
}

// TestEscalateCIFailure_CommentFailure_IncrementsErrorMetric verifies that
// when CommentIssue returns an error the escalation goroutine increments
// IncCIEscalations("error") and does not increment IncCIEscalations("comment").
func TestEscalateCIFailure_CommentFailure_IncrementsErrorMetric(t *testing.T) {
	t.Parallel()

	tracker := &ciTrackerStub{commentIssueErr: errors.New("tracker unavailable")}

	state := stateWithPendingReaction(t, "ESC-ERR-2", "feature/broken", 2)
	state.ReactionAttempts[ReactionKey("ESC-ERR-2", ReactionKindCI)] = 2
	store := &ciReconcileStore{}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusFailing}}
	params := ciParams(t, store, ci, tracker)
	params.CIFeedback.Escalation = "comment"

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	if metrics.ciEscalations["error"] != 1 {
		t.Errorf(`IncCIEscalations("error") = %d, want 1 on comment tracker failure`, metrics.ciEscalations["error"])
	}
	if metrics.ciEscalations["comment"] != 0 {
		t.Errorf(`IncCIEscalations("comment") = %d, want 0 on comment tracker failure`, metrics.ciEscalations["comment"])
	}
}

// TestEscalateCIFailure_NilTracker_ZeroIncrements verifies that when
// TrackerAdapter is nil the escalation path spawns no goroutine and
// IncCIEscalations is never called.
func TestEscalateCIFailure_NilTracker_ZeroIncrements(t *testing.T) {
	t.Parallel()

	state := stateWithPendingReaction(t, "ESC-NIL-1", "main/broken", 3)
	state.ReactionAttempts[ReactionKey("ESC-NIL-1", ReactionKindCI)] = 2
	store := &ciReconcileStore{}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusFailing}}
	params := ciParams(t, store, ci, nil) // nil TrackerAdapter
	// defaultCIFeedback sets escalation: "label".

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	if len(metrics.ciEscalations) != 0 {
		t.Errorf("IncCIEscalations called with nil TrackerAdapter; want zero increments, got %v", metrics.ciEscalations)
	}
}

// TestEscalateCIFailure_NilTracker_ZeroIncrements_Comment verifies the same
// zero-increment guarantee for the comment escalation path with nil tracker.
func TestEscalateCIFailure_NilTracker_ZeroIncrements_Comment(t *testing.T) {
	t.Parallel()

	state := stateWithPendingReaction(t, "ESC-NIL-2", "feature/broken", 2)
	state.ReactionAttempts[ReactionKey("ESC-NIL-2", ReactionKindCI)] = 2
	store := &ciReconcileStore{}
	metrics := newCIMetricsSpy()
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusFailing}}
	params := ciParams(t, store, ci, nil) // nil TrackerAdapter
	params.CIFeedback.Escalation = "comment"

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	if len(metrics.ciEscalations) != 0 {
		t.Errorf("IncCIEscalations called with nil TrackerAdapter; want zero increments, got %v", metrics.ciEscalations)
	}
}

// TestEscalateCIFailure_DeletesFingerprint verifies that escalateCIFailure
// calls DeleteReactionFingerprint after clearing reactions.
func TestEscalateCIFailure_DeletesFingerprint(t *testing.T) {
	t.Parallel()

	// ReactionAttempts=2, maxRetries=2 → next increment (→3) triggers escalation.
	state := stateWithPendingReaction(t, "ISS-FP-6", "sha-escal", 3)
	state.ReactionAttempts[ReactionKey("ISS-FP-6", ReactionKindCI)] = 2
	store := &ciReconcileStore{}
	metrics := newCIMetricsSpy()
	tracker := &ciTrackerStub{}
	ci := &mockCIProvider{result: domain.CIResult{Status: domain.CIStatusFailing, FailingCount: 1}}
	params := ciParams(t, store, ci, tracker)

	reconcileCIStatus(state, params, discardLogger(), context.Background(), metrics)

	// Wait for any escalation goroutine to finish.
	state.TrackerOpsWg.Wait()

	if store.deleteFingerprintCalls != 1 {
		t.Errorf("DeleteReactionFingerprint calls = %d, want 1 during CI escalation", store.deleteFingerprintCalls)
	}
	// Claim must be released.
	if _, ok := state.Claimed["ISS-FP-6"]; ok {
		t.Error("claim not released after escalation")
	}
}
