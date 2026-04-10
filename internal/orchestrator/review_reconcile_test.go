package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
)

// --- Test doubles ---

// mockSCMAdapter is a controllable SCMAdapter for review reconcile tests.
type mockSCMAdapter struct {
	comments []domain.ReviewComment
	err      error
	calls    int
}

var _ domain.SCMAdapter = (*mockSCMAdapter)(nil)

func (m *mockSCMAdapter) FetchPendingReviews(_ context.Context, _ int, _, _ string) ([]domain.ReviewComment, error) {
	m.calls++
	if m.err != nil {
		return nil, m.err
	}
	return m.comments, nil
}

// reviewReconcileStore is a self-contained ReconcileStore for review tests.
type reviewReconcileStore struct {
	savedEntries    []persistence.RetryEntry
	deletedIssueIDs []string

	saveRetryEntryErr   error
	deleteRetryEntryErr error

	upsertFingerprintCalls int
	getFingerprintCalls    int
	markDispatchedCalls    int
	deleteFingerprintCalls int
	deleteFPByIssueCalls   int

	getFingerprintResult     string
	getFingerprintDispatched bool
	getFingerprintErr        error
	upsertFingerprintErr     error
	markDispatchedErr        error
}

var _ ReconcileStore = (*reviewReconcileStore)(nil)

func (s *reviewReconcileStore) SaveRetryEntry(_ context.Context, entry persistence.RetryEntry) error {
	s.savedEntries = append(s.savedEntries, entry)
	return s.saveRetryEntryErr
}

func (s *reviewReconcileStore) DeleteRetryEntry(_ context.Context, issueID string) error {
	s.deletedIssueIDs = append(s.deletedIssueIDs, issueID)
	return s.deleteRetryEntryErr
}

func (s *reviewReconcileStore) AppendRunHistory(_ context.Context, run persistence.RunHistory) (persistence.RunHistory, error) {
	return run, nil
}

func (s *reviewReconcileStore) DeleteReactionFingerprintsByIssue(_ context.Context, _ string) error {
	s.deleteFPByIssueCalls++
	return nil
}

func (s *reviewReconcileStore) UpsertReactionFingerprint(_ context.Context, _, _, _ string) error {
	s.upsertFingerprintCalls++
	return s.upsertFingerprintErr
}

func (s *reviewReconcileStore) GetReactionFingerprint(_ context.Context, _, _ string) (string, bool, error) {
	s.getFingerprintCalls++
	return s.getFingerprintResult, s.getFingerprintDispatched, s.getFingerprintErr
}

func (s *reviewReconcileStore) MarkReactionDispatched(_ context.Context, _, _ string) error {
	s.markDispatchedCalls++
	return s.markDispatchedErr
}

func (s *reviewReconcileStore) DeleteReactionFingerprint(_ context.Context, _, _ string) error {
	s.deleteFingerprintCalls++
	return nil
}

// reviewTrackerStub satisfies domain.TrackerAdapter for escalation tests.
type reviewTrackerStub struct {
	addLabelCalled    int
	commentIssueCalls int
}

var _ domain.TrackerAdapter = (*reviewTrackerStub)(nil)

func (s *reviewTrackerStub) FetchIssuesByStates(_ context.Context, _ []string) ([]domain.Issue, error) {
	return nil, nil
}
func (s *reviewTrackerStub) FetchCandidateIssues(_ context.Context) ([]domain.Issue, error) {
	return nil, nil
}
func (s *reviewTrackerStub) FetchIssueByID(_ context.Context, _ string) (domain.Issue, error) {
	return domain.Issue{}, nil
}
func (s *reviewTrackerStub) FetchIssueStatesByIDs(_ context.Context, _ []string) (map[string]string, error) {
	return nil, nil
}
func (s *reviewTrackerStub) FetchIssueStatesByIdentifiers(_ context.Context, _ []string) (map[string]string, error) {
	return nil, nil
}
func (s *reviewTrackerStub) FetchIssueComments(_ context.Context, _ string) ([]domain.Comment, error) {
	return nil, nil
}
func (s *reviewTrackerStub) TransitionIssue(_ context.Context, _ string, _ string) error {
	return nil
}
func (s *reviewTrackerStub) CommentIssue(_ context.Context, _ string, _ string) error {
	s.commentIssueCalls++
	return nil
}
func (s *reviewTrackerStub) AddLabel(_ context.Context, _ string, _ string) error {
	s.addLabelCalled++
	return nil
}

// reviewMetricsSpy records review-specific metric calls.
type reviewMetricsSpy struct {
	domain.NoopMetrics
	reviewChecks      map[string]int
	reviewEscalations map[string]int
}

func newReviewMetricsSpy() *reviewMetricsSpy {
	return &reviewMetricsSpy{
		reviewChecks:      make(map[string]int),
		reviewEscalations: make(map[string]int),
	}
}

func (s *reviewMetricsSpy) IncReviewChecks(result string)      { s.reviewChecks[result]++ }
func (s *reviewMetricsSpy) IncReviewEscalations(action string) { s.reviewEscalations[action]++ }

// --- Test helpers ---

// reviewBaseTime is a fixed reference for review reconcile tests.
var reviewBaseTime = time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)

// newReviewPendingEntry builds a PendingReaction with Kind=ReactionKindReview.
func newReviewPendingEntry(issueID string, prNumber int) *PendingReaction {
	return &PendingReaction{
		IssueID:    issueID,
		Identifier: issueID + "-ident",
		DisplayID:  issueID + "-ident",
		Attempt:    1,
		Kind:       ReactionKindReview,
		CreatedAt:  reviewBaseTime,
		KindData: &ReviewReactionData{
			PRNumber: prNumber,
			Owner:    "owner",
			Repo:     "repo",
			Branch:   "feature/fix",
		},
	}
}

// stateWithReviewReaction creates a State with one review PendingReaction.
func stateWithReviewReaction(t *testing.T, issueID string, prNumber int) *State {
	t.Helper()
	s := NewState(5000, 4, nil, AgentTotals{})
	rkey := ReactionKey(issueID, ReactionKindReview)
	s.PendingReactions[rkey] = newReviewPendingEntry(issueID, prNumber)
	s.Claimed[issueID] = struct{}{}
	return s
}

// defaultReviewConfig returns a ReviewReactionConfig with sensible defaults.
func defaultReviewConfig() ReviewReactionConfig {
	return ReviewReactionConfig{
		MaxRetries:           2,
		Escalation:           "label",
		EscalationLabel:      "needs-human",
		PollIntervalMS:       60000,
		DebounceMS:           30000,
		MaxContinuationTurns: 3,
	}
}

// reviewParams returns ReconcileParams wired for review reconcile tests.
func reviewParams(store *reviewReconcileStore, scm domain.SCMAdapter, tracker domain.TrackerAdapter) ReconcileParams {
	return ReconcileParams{
		TrackerAdapter: tracker,
		SCMAdapter:     scm,
		ReviewConfig:   defaultReviewConfig(),
		Store:          store,
		OnRetryFire:    noopRetryFire,
		Ctx:            context.Background(),
		Logger:         discardLogger(),
		NowFunc:        func() time.Time { return reviewBaseTime },
	}
}

// --- reconcileReviewComments tests ---

func TestReconcileReviewComments_NilAdapter(t *testing.T) {
	t.Parallel()

	state := stateWithReviewReaction(t, "ISS-R-1", 42)
	store := &reviewReconcileStore{}
	metrics := newReviewMetricsSpy()
	params := reviewParams(store, nil, nil)

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	rkey := ReactionKey("ISS-R-1", ReactionKindReview)
	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Error("PendingReactions entry removed with nil SCMAdapter; want no-op")
	}
	if len(metrics.reviewChecks) != 0 {
		t.Errorf("IncReviewChecks called with nil adapter; want no calls")
	}
}

func TestReconcileReviewComments_NoPendingReviewEntries(t *testing.T) {
	t.Parallel()

	state := NewState(5000, 4, nil, AgentTotals{})
	// Add a CI reaction entry — should not be processed by review reconcile.
	rkey := ReactionKey("ISS-R-CI", ReactionKindCI)
	state.PendingReactions[rkey] = &PendingReaction{
		Kind:      ReactionKindCI,
		IssueID:   "ISS-R-CI",
		CreatedAt: reviewBaseTime,
		KindData:  &CIReactionData{Branch: "main"},
	}

	store := &reviewReconcileStore{}
	metrics := newReviewMetricsSpy()
	scm := &mockSCMAdapter{}
	params := reviewParams(store, scm, nil)

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	if scm.calls != 0 {
		t.Errorf("FetchPendingReviews calls = %d, want 0 (no review entries)", scm.calls)
	}
	// CI entry must remain untouched.
	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Error("CI PendingReactions entry removed by review reconcile; want untouched")
	}
}

func TestReconcileReviewComments_PollThrottle(t *testing.T) {
	t.Parallel()

	state := stateWithReviewReaction(t, "ISS-R-2", 10)
	rkey := ReactionKey("ISS-R-2", ReactionKindReview)
	// Set PendingRetryAt to 1 minute in the future relative to NowFunc.
	state.PendingReactions[rkey].PendingRetryAt = reviewBaseTime.Add(1 * time.Minute)

	store := &reviewReconcileStore{}
	metrics := newReviewMetricsSpy()
	scm := &mockSCMAdapter{}
	params := reviewParams(store, scm, nil)

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Error("PendingReactions entry dropped on poll throttle; want re-enqueued")
	}
	if scm.calls != 0 {
		t.Errorf("FetchPendingReviews calls = %d, want 0 (throttled)", scm.calls)
	}
}

func TestReconcileReviewComments_TTLExpired(t *testing.T) {
	t.Parallel()

	state := stateWithReviewReaction(t, "ISS-R-3", 10)
	rkey := ReactionKey("ISS-R-3", ReactionKindReview)
	// Set CreatedAt 31 minutes before NowFunc.
	state.PendingReactions[rkey].CreatedAt = reviewBaseTime.Add(-31 * time.Minute)

	store := &reviewReconcileStore{}
	metrics := newReviewMetricsSpy()
	scm := &mockSCMAdapter{}
	params := reviewParams(store, scm, nil)
	params.ReviewPendingTTL = 30 * time.Minute

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry retained after TTL expiry; want dropped")
	}
	if scm.calls != 0 {
		t.Errorf("FetchPendingReviews calls = %d, want 0 (TTL exceeded)", scm.calls)
	}
}

func TestReconcileReviewComments_SCMFetchError_ReEnqueues(t *testing.T) {
	t.Parallel()

	state := stateWithReviewReaction(t, "ISS-R-4", 10)
	rkey := ReactionKey("ISS-R-4", ReactionKindReview)
	store := &reviewReconcileStore{}
	metrics := newReviewMetricsSpy()
	scm := &mockSCMAdapter{err: errors.New("connection timeout")}
	params := reviewParams(store, scm, nil)

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Error("PendingReactions entry dropped on SCM fetch error; want re-enqueued")
	}
	// PendingAttempts should be incremented.
	entry := state.PendingReactions[rkey]
	if entry.PendingAttempts != 1 {
		t.Errorf("PendingAttempts = %d, want 1 after first error", entry.PendingAttempts)
	}
	// PendingRetryAt should be in the future (backoff applied).
	if !entry.PendingRetryAt.After(reviewBaseTime) {
		t.Error("PendingRetryAt not in future after SCM error; want backoff applied")
	}
	if metrics.reviewChecks["error"] != 1 {
		t.Errorf(`IncReviewChecks("error") = %d, want 1`, metrics.reviewChecks["error"])
	}
}

func TestReconcileReviewComments_NoActionableComments(t *testing.T) {
	t.Parallel()

	state := stateWithReviewReaction(t, "ISS-R-5", 10)
	rkey := ReactionKey("ISS-R-5", ReactionKindReview)
	store := &reviewReconcileStore{}
	metrics := newReviewMetricsSpy()
	// Empty slice — no actionable comments.
	scm := &mockSCMAdapter{comments: []domain.ReviewComment{}}
	params := reviewParams(store, scm, nil)

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Error("PendingReactions entry dropped with no actionable comments; want re-enqueued")
	}
	if _, ok := state.RetryAttempts["ISS-R-5"]; ok {
		t.Error("retry scheduled with no actionable comments; want none")
	}
}

func TestReconcileReviewComments_AllCommentsOutdated(t *testing.T) {
	t.Parallel()

	state := stateWithReviewReaction(t, "ISS-R-6", 10)
	rkey := ReactionKey("ISS-R-6", ReactionKindReview)
	store := &reviewReconcileStore{}
	metrics := newReviewMetricsSpy()
	scm := &mockSCMAdapter{
		comments: []domain.ReviewComment{
			{ID: "1", Outdated: true, SubmittedAt: reviewBaseTime.Add(-1 * time.Hour)},
			{ID: "2", Outdated: true, SubmittedAt: reviewBaseTime.Add(-2 * time.Hour)},
		},
	}
	params := reviewParams(store, scm, nil)

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Error("PendingReactions entry dropped with all outdated comments; want re-enqueued")
	}
	if _, ok := state.RetryAttempts["ISS-R-6"]; ok {
		t.Error("retry scheduled with all outdated comments; want none")
	}
}

func TestReconcileReviewComments_FingerprintMatchDispatched(t *testing.T) {
	t.Parallel()

	state := stateWithReviewReaction(t, "ISS-R-7", 10)
	rkey := ReactionKey("ISS-R-7", ReactionKindReview)

	comments := []domain.ReviewComment{
		{ID: "100", Body: "fix this", SubmittedAt: reviewBaseTime.Add(-2 * time.Minute)},
	}
	fp := buildReviewFingerprint(comments)

	store := &reviewReconcileStore{
		getFingerprintResult:     fp,
		getFingerprintDispatched: true,
	}
	metrics := newReviewMetricsSpy()
	scm := &mockSCMAdapter{comments: comments}
	params := reviewParams(store, scm, nil)

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	// Already dispatched → re-enqueue but do not call MarkReactionDispatched.
	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Error("PendingReactions entry dropped for already-dispatched fingerprint; want re-enqueued")
	}
	if store.markDispatchedCalls != 0 {
		t.Errorf("MarkReactionDispatched calls = %d, want 0 (already dispatched)", store.markDispatchedCalls)
	}
	if _, ok := state.RetryAttempts["ISS-R-7"]; ok {
		t.Error("retry scheduled for already-dispatched fingerprint; want none")
	}
}

func TestReconcileReviewComments_NewFingerprint_Dispatches(t *testing.T) {
	t.Parallel()

	state := stateWithReviewReaction(t, "ISS-R-8", 10)
	rkey := ReactionKey("ISS-R-8", ReactionKindReview)

	// Comment submitted 5 minutes ago — outside the 30s debounce window (defaultReviewConfig).
	comments := []domain.ReviewComment{
		{ID: "200", Body: "needs fix", SubmittedAt: reviewBaseTime.Add(-5 * time.Minute)},
	}
	store := &reviewReconcileStore{
		getFingerprintResult:     "",
		getFingerprintDispatched: false,
	}
	metrics := newReviewMetricsSpy()
	scm := &mockSCMAdapter{comments: comments}
	params := reviewParams(store, scm, nil)

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	// Entry consumed (not re-enqueued as pending-check).
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry still present after dispatch; want consumed")
	}
	if store.markDispatchedCalls != 1 {
		t.Errorf("MarkReactionDispatched calls = %d, want 1", store.markDispatchedCalls)
	}
	if _, ok := state.RetryAttempts["ISS-R-8"]; !ok {
		t.Fatal("retry not scheduled after review dispatch; want scheduled")
	}
	retry := state.RetryAttempts["ISS-R-8"]
	if retry.ContinuationContext == nil {
		t.Error("RetryEntry.ContinuationContext is nil; want review_comments map")
	}
	if retry.ReactionKind != ReactionKindReview {
		t.Errorf("RetryEntry.ReactionKind = %q, want %q", retry.ReactionKind, ReactionKindReview)
	}
	if state.ReactionAttempts[rkey] != 1 {
		t.Errorf("ReactionAttempts[%s] = %d, want 1", rkey, state.ReactionAttempts[rkey])
	}
	if metrics.reviewChecks["dispatched"] != 1 {
		t.Errorf(`IncReviewChecks("dispatched") = %d, want 1`, metrics.reviewChecks["dispatched"])
	}
}

func TestReconcileReviewComments_DebounceWindowActive(t *testing.T) {
	t.Parallel()

	state := stateWithReviewReaction(t, "ISS-R-9", 10)
	rkey := ReactionKey("ISS-R-9", ReactionKindReview)

	// Comment submitted 10 seconds ago; debounce window is 30s (defaultReviewConfig).
	recentTime := reviewBaseTime.Add(-10 * time.Second)
	comments := []domain.ReviewComment{
		{ID: "300", Body: "new comment", SubmittedAt: recentTime},
	}
	store := &reviewReconcileStore{
		getFingerprintResult:     "",
		getFingerprintDispatched: false,
	}
	metrics := newReviewMetricsSpy()
	scm := &mockSCMAdapter{comments: comments}
	params := reviewParams(store, scm, nil)

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	// Debounced: re-enqueued, no retry scheduled.
	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Error("PendingReactions entry dropped during debounce; want re-enqueued")
	}
	if _, ok := state.RetryAttempts["ISS-R-9"]; ok {
		t.Error("retry scheduled during debounce window; want none")
	}
	// PendingRetryAt should be set to LastEventAt + debounceMS.
	entry := state.PendingReactions[rkey]
	expectedRetryAt := recentTime.Add(time.Duration(defaultReviewConfig().DebounceMS) * time.Millisecond)
	if !entry.PendingRetryAt.Equal(expectedRetryAt) {
		t.Errorf("PendingRetryAt = %v, want %v", entry.PendingRetryAt, expectedRetryAt)
	}
}

func TestReconcileReviewComments_DebounceElapsed_Dispatches(t *testing.T) {
	t.Parallel()

	state := stateWithReviewReaction(t, "ISS-R-10", 10)
	rkey := ReactionKey("ISS-R-10", ReactionKindReview)

	// Comment submitted 120 seconds ago; debounce window is 30s → elapsed.
	comments := []domain.ReviewComment{
		{ID: "400", Body: "old enough", SubmittedAt: reviewBaseTime.Add(-120 * time.Second)},
	}
	store := &reviewReconcileStore{
		getFingerprintResult:     "",
		getFingerprintDispatched: false,
	}
	metrics := newReviewMetricsSpy()
	scm := &mockSCMAdapter{comments: comments}
	params := reviewParams(store, scm, nil)

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	// Dispatch should have happened.
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry still present after debounce elapsed; want consumed")
	}
	if _, ok := state.RetryAttempts["ISS-R-10"]; !ok {
		t.Error("retry not scheduled after debounce elapsed; want scheduled")
	}
	if store.markDispatchedCalls != 1 {
		t.Errorf("MarkReactionDispatched calls = %d, want 1", store.markDispatchedCalls)
	}
}

func TestReconcileReviewComments_TurnCapExceeded_Escalates(t *testing.T) {
	t.Parallel()

	state := stateWithReviewReaction(t, "ISS-R-11", 10)
	rkey := ReactionKey("ISS-R-11", ReactionKindReview)
	// Set ReactionAttempts to MaxContinuationTurns (3).
	state.ReactionAttempts[rkey] = 3

	store := &reviewReconcileStore{}
	metrics := newReviewMetricsSpy()
	tracker := &reviewTrackerStub{}
	// SCMAdapter is set but we should not reach the fetch call.
	scm := &mockSCMAdapter{}
	params := reviewParams(store, scm, tracker)

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	// Entry consumed (not re-enqueued).
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry still present after turn cap; want consumed")
	}
	// Claim released.
	if _, ok := state.Claimed["ISS-R-11"]; ok {
		t.Error("claim not released after turn cap escalation; want released")
	}
	// DeleteRetryEntry called.
	if len(store.deletedIssueIDs) != 1 || store.deletedIssueIDs[0] != "ISS-R-11" {
		t.Errorf("DeleteRetryEntry calls = %v, want [ISS-R-11]", store.deletedIssueIDs)
	}
	// No retry scheduled.
	if _, ok := state.RetryAttempts["ISS-R-11"]; ok {
		t.Error("retry still scheduled after escalation; want none")
	}
	// SCM fetch must not have been called.
	if scm.calls != 0 {
		t.Errorf("FetchPendingReviews called %d times; want 0 (cap check before fetch)", scm.calls)
	}
}

// --- buildReviewFingerprint tests ---

func TestBuildReviewFingerprint_EmptyInput(t *testing.T) {
	t.Parallel()

	got := buildReviewFingerprint(nil)
	if got != "" {
		t.Errorf("buildReviewFingerprint(nil) = %q, want empty", got)
	}
	got = buildReviewFingerprint([]domain.ReviewComment{})
	if got != "" {
		t.Errorf("buildReviewFingerprint([]) = %q, want empty", got)
	}
}

func TestBuildReviewFingerprint_OrderIndependent(t *testing.T) {
	t.Parallel()

	commentsABC := []domain.ReviewComment{
		{ID: "a"}, {ID: "b"}, {ID: "c"},
	}
	commentsCBA := []domain.ReviewComment{
		{ID: "c"}, {ID: "b"}, {ID: "a"},
	}

	fp1 := buildReviewFingerprint(commentsABC)
	fp2 := buildReviewFingerprint(commentsCBA)

	if fp1 == "" {
		t.Fatal("buildReviewFingerprint returned empty for non-empty input")
	}
	if fp1 != fp2 {
		t.Errorf("buildReviewFingerprint: different order produced different hashes:\n  abc: %q\n  cba: %q", fp1, fp2)
	}
}

func TestBuildReviewFingerprint_DifferentIDsProduceDifferentHash(t *testing.T) {
	t.Parallel()

	comments1 := []domain.ReviewComment{{ID: "aaa"}}
	comments2 := []domain.ReviewComment{{ID: "bbb"}}

	fp1 := buildReviewFingerprint(comments1)
	fp2 := buildReviewFingerprint(comments2)

	if fp1 == fp2 {
		t.Errorf("buildReviewFingerprint: different IDs produced same hash %q", fp1)
	}
}

func TestBuildReviewFingerprint_Deterministic(t *testing.T) {
	t.Parallel()

	comments := []domain.ReviewComment{{ID: "x"}, {ID: "y"}}
	fp1 := buildReviewFingerprint(comments)
	fp2 := buildReviewFingerprint(comments)
	if fp1 != fp2 {
		t.Errorf("buildReviewFingerprint not deterministic: %q != %q", fp1, fp2)
	}
}

// --- buildReviewTemplateMap tests ---

func TestBuildReviewTemplateMap_FieldMapping(t *testing.T) {
	t.Parallel()

	comments := []domain.ReviewComment{
		{
			ID:        "500",
			FilePath:  "main.go",
			StartLine: 10,
			EndLine:   15,
			Reviewer:  "alice",
			Body:      "Please refactor this.",
		},
	}

	result := buildReviewTemplateMap(comments)
	if len(result) != 1 {
		t.Fatalf("buildReviewTemplateMap len = %d, want 1", len(result))
	}

	m := result[0]
	wantFields := map[string]any{
		"id":         "500",
		"file":       "main.go",
		"start_line": 10,
		"end_line":   15,
		"reviewer":   "alice",
		"body":       "Please refactor this.",
	}
	for k, want := range wantFields {
		got, ok := m[k]
		if !ok {
			t.Errorf("buildReviewTemplateMap: key %q missing from result", k)
			continue
		}
		if got != want {
			t.Errorf("buildReviewTemplateMap[%q] = %v, want %v", k, got, want)
		}
	}
}

func TestBuildReviewTemplateMap_ZeroLines(t *testing.T) {
	t.Parallel()

	comments := []domain.ReviewComment{
		{ID: "600", FilePath: "", StartLine: 0, EndLine: 0, Reviewer: "bob", Body: "PR comment"},
	}

	result := buildReviewTemplateMap(comments)
	if len(result) != 1 {
		t.Fatalf("buildReviewTemplateMap len = %d, want 1", len(result))
	}

	m := result[0]
	if m["start_line"] != 0 {
		t.Errorf("start_line = %v, want 0", m["start_line"])
	}
	if m["end_line"] != 0 {
		t.Errorf("end_line = %v, want 0", m["end_line"])
	}
	if m["file"] != "" {
		t.Errorf("file = %v, want empty string", m["file"])
	}
}

func TestBuildReviewTemplateMap_MultipleComments(t *testing.T) {
	t.Parallel()

	comments := []domain.ReviewComment{
		{ID: "1", Body: "first"},
		{ID: "2", Body: "second"},
		{ID: "3", Body: "third"},
	}

	result := buildReviewTemplateMap(comments)
	if len(result) != 3 {
		t.Fatalf("buildReviewTemplateMap len = %d, want 3", len(result))
	}
	for i, m := range result {
		if m["id"] != comments[i].ID {
			t.Errorf("result[%d][id] = %v, want %q", i, m["id"], comments[i].ID)
		}
	}
}

// --- BuildReviewReactionConfig tests ---

func TestBuildReviewReactionConfig_Defaults(t *testing.T) {
	t.Parallel()

	rc := config.ReactionConfig{
		MaxRetries:      2,
		Escalation:      "label",
		EscalationLabel: "needs-human",
	}

	got, err := BuildReviewReactionConfig(rc)
	if err != nil {
		t.Fatalf("BuildReviewReactionConfig: unexpected error: %v", err)
	}
	if got.PollIntervalMS != 120000 {
		t.Errorf("PollIntervalMS = %d, want 120000 (default)", got.PollIntervalMS)
	}
	if got.DebounceMS != 60000 {
		t.Errorf("DebounceMS = %d, want 60000 (default)", got.DebounceMS)
	}
	if got.MaxContinuationTurns != 3 {
		t.Errorf("MaxContinuationTurns = %d, want 3 (default)", got.MaxContinuationTurns)
	}
	if got.Escalation != "label" {
		t.Errorf("Escalation = %q, want %q", got.Escalation, "label")
	}
	if got.EscalationLabel != "needs-human" {
		t.Errorf("EscalationLabel = %q, want %q", got.EscalationLabel, "needs-human")
	}
}

func TestBuildReviewReactionConfig_EscalationDefault(t *testing.T) {
	t.Parallel()

	rc := config.ReactionConfig{MaxRetries: 2}

	got, err := BuildReviewReactionConfig(rc)
	if err != nil {
		t.Fatalf("BuildReviewReactionConfig: %v", err)
	}
	if got.Escalation != "label" {
		t.Errorf("Escalation = %q, want %q (default)", got.Escalation, "label")
	}
	if got.EscalationLabel != "needs-human" {
		t.Errorf("EscalationLabel = %q, want %q (default)", got.EscalationLabel, "needs-human")
	}
}

func TestBuildReviewReactionConfig_ExtraFields(t *testing.T) {
	t.Parallel()

	rc := config.ReactionConfig{
		MaxRetries: 1,
		Escalation: "comment",
		Extra: map[string]any{
			"poll_interval_ms":       60000,
			"debounce_ms":            10000,
			"max_continuation_turns": 5,
		},
	}

	got, err := BuildReviewReactionConfig(rc)
	if err != nil {
		t.Fatalf("BuildReviewReactionConfig: %v", err)
	}
	if got.PollIntervalMS != 60000 {
		t.Errorf("PollIntervalMS = %d, want 60000", got.PollIntervalMS)
	}
	if got.DebounceMS != 10000 {
		t.Errorf("DebounceMS = %d, want 10000", got.DebounceMS)
	}
	if got.MaxContinuationTurns != 5 {
		t.Errorf("MaxContinuationTurns = %d, want 5", got.MaxContinuationTurns)
	}
}

func TestBuildReviewReactionConfig_PollIntervalBelowMinimum(t *testing.T) {
	t.Parallel()

	rc := config.ReactionConfig{
		Escalation: "label",
		Extra: map[string]any{
			"poll_interval_ms": 10000, // below minimum 30000
		},
	}

	_, err := BuildReviewReactionConfig(rc)
	if err == nil {
		t.Fatal("BuildReviewReactionConfig: expected error for poll_interval_ms < 30000, got nil")
	}
}

func TestBuildReviewReactionConfig_DebounceZeroIsValid(t *testing.T) {
	t.Parallel()

	rc := config.ReactionConfig{
		Escalation: "label",
		Extra: map[string]any{
			"debounce_ms": 0,
		},
	}

	got, err := BuildReviewReactionConfig(rc)
	if err != nil {
		t.Fatalf("BuildReviewReactionConfig: unexpected error for debounce_ms=0: %v", err)
	}
	if got.DebounceMS != 0 {
		t.Errorf("DebounceMS = %d, want 0", got.DebounceMS)
	}
}

func TestBuildReviewReactionConfig_MaxContinuationTurnsZero(t *testing.T) {
	t.Parallel()

	rc := config.ReactionConfig{
		Escalation: "label",
		Extra: map[string]any{
			"max_continuation_turns": 0,
		},
	}

	_, err := BuildReviewReactionConfig(rc)
	if err == nil {
		t.Fatal("BuildReviewReactionConfig: expected error for max_continuation_turns=0, got nil")
	}
}

func TestBuildReviewReactionConfig_InvalidEscalation(t *testing.T) {
	t.Parallel()

	rc := config.ReactionConfig{
		Escalation: "webhook",
	}

	_, err := BuildReviewReactionConfig(rc)
	if err == nil {
		t.Fatal("BuildReviewReactionConfig: expected error for invalid escalation, got nil")
	}
}

// --- computeReviewPendingDelay tests ---

func TestComputeReviewPendingDelay(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		attempts int
		wantMin  time.Duration
		wantMax  time.Duration
	}{
		{"zero attempts returns 0", 0, 0, 0},
		{"negative attempts returns 0", -1, 0, 0},
		{"attempt 1 returns base*2", 1, reviewPendingBackoffBase * 2, reviewPendingBackoffBase * 3},
		{"very large attempt capped at max", 100, reviewPendingBackoffCap, reviewPendingBackoffCap},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := computeReviewPendingDelay(tt.attempts)
			if got < tt.wantMin || got > tt.wantMax {
				t.Errorf("computeReviewPendingDelay(%d) = %v, want in [%v, %v]",
					tt.attempts, got, tt.wantMin, tt.wantMax)
			}
		})
	}
}
