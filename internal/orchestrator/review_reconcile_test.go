package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
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

	botComments []domain.ReviewComment
	botErr      error
	botCalls    int
}

var _ domain.SCMAdapter = (*mockSCMAdapter)(nil)

func (m *mockSCMAdapter) FetchPendingReviews(_ context.Context, _ int, _, _ string) ([]domain.ReviewComment, error) {
	m.calls++
	if m.err != nil {
		return nil, m.err
	}
	return m.comments, nil
}

func (m *mockSCMAdapter) FetchBotReviewComments(_ context.Context, _ int, _, _ string, _ []string) ([]domain.ReviewComment, error) {
	m.botCalls++
	if m.botErr != nil {
		return nil, m.botErr
	}
	return m.botComments, nil
}

func (m *mockSCMAdapter) GetReviewDecision(_ context.Context, _ int, _, _ string) (domain.ReviewDecision, error) {
	return "", nil
}

func (m *mockSCMAdapter) GetCIStatus(_ context.Context, _ int, _, _ string) (string, error) {
	return "", nil
}

func (m *mockSCMAdapter) GetMergeability(_ context.Context, _ int, _, _ string) (domain.PRMergeStatus, error) {
	return domain.PRMergeStatus{}, nil
}

func (m *mockSCMAdapter) MergePR(_ context.Context, _ int, _, _ string, _ domain.MergeStrategy, _, _, _ string) (domain.MergeResult, error) {
	return domain.MergeResult{}, nil
}

func (m *mockSCMAdapter) DeleteBranch(_ context.Context, _, _, _ string) error {
	return nil
}

func (m *mockSCMAdapter) ListLabelEvents(_ context.Context, _ int, _, _ string) ([]domain.LabelEvent, error) {
	return nil, nil
}

func (m *mockSCMAdapter) RemoveLabel(_ context.Context, _ int, _, _, _ string) error {
	return nil
}

// reviewReconcileStore is a self-contained ReconcileStore for review tests.
type reviewReconcileStore struct {
	unsupportedReactionObservationStore

	savedEntries    []persistence.RetryEntry
	deletedIssueIDs []string

	saveRetryEntryErr   error
	deleteRetryEntryErr error

	upsertFingerprintCalls int
	getFingerprintCalls    int
	markDispatchedCalls    int
	deleteFingerprintCalls int

	getFingerprintResult     string
	getFingerprintDispatched bool
	getFingerprintErr        error
	upsertFingerprintErr     error
	markDispatchedErr        error
	deleteFingerprintErr     error
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
	return s.deleteFingerprintErr
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
		Escalation:           "label",
		EscalationLabel:      "needs-human",
		PollIntervalMS:       60000,
		DebounceMS:           30000,
		MaxContinuationTurns: 3,
	}
}

// Log messages asserted by the bot-allowlist exclusion table test, kept in
// sync with the literal strings reconcileReviewComments logs.
const (
	reviewExclusionDebugMsg = "review comments excluded by bot allowlist"
	reviewDispatchInfoMsg   = "review comments detected, scheduling review-fix dispatch"
)

// findLogLine returns the first line in logOutput whose msg attribute
// equals msg, or the empty string when no such line exists.
func findLogLine(logOutput, msg string) string {
	want := `msg="` + msg + `"`
	for line := range strings.SplitSeq(logOutput, "\n") {
		if strings.Contains(line, want) {
			return line
		}
	}
	return ""
}

// assertLogLineHasIntAttr fails the test unless logOutput contains a line
// for msg carrying key=want as a whole attribute (word-boundary matched,
// so a want of 1 cannot be satisfied by an actual value of 10).
func assertLogLineHasIntAttr(t *testing.T, logOutput, msg, key string, want int) {
	t.Helper()
	line := findLogLine(logOutput, msg)
	if line == "" {
		t.Fatalf("log output missing line with msg %q; log=%s", msg, logOutput)
	}
	pattern := regexp.MustCompile(fmt.Sprintf(`\b%s=%d\b`, regexp.QuoteMeta(key), want))
	if !pattern.MatchString(line) {
		t.Errorf("log line %q missing %s=%d", line, key, want)
	}
}

// assertLogLacksLine fails the test if logOutput contains any line for msg.
func assertLogLacksLine(t *testing.T, logOutput, msg string) {
	t.Helper()
	if line := findLogLine(logOutput, msg); line != "" {
		t.Errorf("log output contains unexpected line with msg %q: %s", msg, line)
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

// TestReconcileReviewComments_DropOnAgeReleasesCounter verifies that the
// drop-on-age branch also deletes the review reaction attempt counter,
// leaves a sibling kind's counter and the claim untouched, and performs
// no retry or fingerprint store call.
func TestReconcileReviewComments_DropOnAgeReleasesCounter(t *testing.T) {
	t.Parallel()

	issueID := "REV-AGE-1"
	state := stateWithReviewReaction(t, issueID, 10)
	reviewKey := ReactionKey(issueID, ReactionKindReview)
	state.ReactionAttempts[reviewKey] = 3
	state.PendingReactions[reviewKey].CreatedAt = reviewBaseTime.Add(-31 * time.Minute)
	ciKey := ReactionKey(issueID, ReactionKindCI)
	state.ReactionAttempts[ciKey] = 7
	delete(state.Claimed, issueID)

	store := &reviewReconcileStore{}
	metrics := newReviewMetricsSpy()
	scm := &mockSCMAdapter{}
	params := reviewParams(store, scm, nil)
	params.ReviewPendingTTL = 30 * time.Minute

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[reviewKey]; ok {
		t.Error("PendingReactions[review] present after drop-on-age; want removed")
	}
	if _, ok := state.ReactionAttempts[reviewKey]; ok {
		t.Error("ReactionAttempts[review] present after drop-on-age; want removed")
	}
	if state.ReactionAttempts[ciKey] != 7 {
		t.Errorf("ReactionAttempts[ci] = %d, want 7 (untouched)", state.ReactionAttempts[ciKey])
	}
	if _, ok := state.Claimed[issueID]; ok {
		t.Error("Claimed present after drop-on-age; want absent")
	}
	if len(store.deletedIssueIDs) != 0 {
		t.Errorf("DeleteRetryEntry calls = %d, want 0", len(store.deletedIssueIDs))
	}
	if store.upsertFingerprintCalls != 0 || store.getFingerprintCalls != 0 || store.deleteFingerprintCalls != 0 {
		t.Errorf("fingerprint calls = upsert:%d get:%d delete:%d, want all 0",
			store.upsertFingerprintCalls, store.getFingerprintCalls, store.deleteFingerprintCalls)
	}
	if scm.calls != 0 {
		t.Errorf("FetchPendingReviews calls = %d, want 0 (TTL exceeded before fetch)", scm.calls)
	}
}

// TestReconcileReviewComments_WatchWindowZeroNeverDrops verifies that a
// ReviewPendingTTL of 0 (watch_window_ms: 0) never drops an entry on age,
// however old it is.
func TestReconcileReviewComments_WatchWindowZeroNeverDrops(t *testing.T) {
	t.Parallel()

	rkey := ReactionKey("ISS-R-WW0", ReactionKindReview)
	state := stateWithReviewReaction(t, "ISS-R-WW0", 10)
	state.PendingReactions[rkey].CreatedAt = reviewBaseTime.Add(-365 * 24 * time.Hour)

	store := &reviewReconcileStore{}
	metrics := newReviewMetricsSpy()
	scm := &mockSCMAdapter{}
	params := reviewParams(store, scm, nil)
	params.ReviewPendingTTL = 0

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Error("PendingReactions entry dropped with ReviewPendingTTL=0; want never dropped on age")
	}
}

// TestReconcileReviewComments_WatchWindowNonDefaultTakesEffect verifies that
// a configured window other than the default threshold actually gates the
// drop, not a hardcoded default: an entry older than the configured window
// is dropped with its attempt counter released, and one younger survives.
func TestReconcileReviewComments_WatchWindowNonDefaultTakesEffect(t *testing.T) {
	t.Parallel()

	t.Run("older than configured window is dropped and counter released", func(t *testing.T) {
		t.Parallel()

		rkey := ReactionKey("ISS-R-WWN-OLD", ReactionKindReview)
		state := stateWithReviewReaction(t, "ISS-R-WWN-OLD", 10)
		state.PendingReactions[rkey].CreatedAt = reviewBaseTime.Add(-6 * time.Minute)
		state.ReactionAttempts[rkey] = 2

		store := &reviewReconcileStore{}
		metrics := newReviewMetricsSpy()
		scm := &mockSCMAdapter{}
		params := reviewParams(store, scm, nil)
		params.ReviewPendingTTL = 5 * time.Minute

		reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

		if _, ok := state.PendingReactions[rkey]; ok {
			t.Error("PendingReactions entry present past configured 5m window; want dropped")
		}
		if _, ok := state.ReactionAttempts[rkey]; ok {
			t.Error("ReactionAttempts present past configured 5m window; want released")
		}
	})

	t.Run("younger than configured window survives", func(t *testing.T) {
		t.Parallel()

		rkey := ReactionKey("ISS-R-WWN-NEW", ReactionKindReview)
		state := stateWithReviewReaction(t, "ISS-R-WWN-NEW", 10)
		state.PendingReactions[rkey].CreatedAt = reviewBaseTime.Add(-4 * time.Minute)

		store := &reviewReconcileStore{}
		metrics := newReviewMetricsSpy()
		scm := &mockSCMAdapter{}
		params := reviewParams(store, scm, nil)
		params.ReviewPendingTTL = 5 * time.Minute

		reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

		if _, ok := state.PendingReactions[rkey]; !ok {
			t.Error("PendingReactions entry dropped inside configured 5m window; want kept")
		}
	})
}

// TestReconcileReviewComments_WatchWindowElapsedLogsRenamedAttribute pins the
// drop-on-age log record: message text and the window_ms attribute name
// (renamed from ttl_ms). A regression that reverts the rename or the wording
// must fail this test.
func TestReconcileReviewComments_WatchWindowElapsedLogsRenamedAttribute(t *testing.T) {
	t.Parallel()

	rkey := ReactionKey("ISS-R-WWLOG", ReactionKindReview)
	state := stateWithReviewReaction(t, "ISS-R-WWLOG", 10)
	state.PendingReactions[rkey].CreatedAt = reviewBaseTime.Add(-31 * time.Minute)

	store := &reviewReconcileStore{}
	metrics := newReviewMetricsSpy()
	scm := &mockSCMAdapter{}
	params := reviewParams(store, scm, nil)
	params.ReviewPendingTTL = 30 * time.Minute
	log, buf := logCapture()

	reconcileReviewComments(state, params, log, context.Background(), metrics)

	output := buf.String()
	const msg = "review watch window elapsed, dropping"
	assertLogLineHasIntAttr(t, output, msg, "window_ms", int(30*time.Minute/time.Millisecond))
	if strings.Contains(output, "ttl_ms") {
		t.Errorf("log output contains stale attribute %q: %s", "ttl_ms", output)
	}
	if strings.Contains(output, "exceeded ttl") {
		t.Errorf("log output contains stale message wording %q: %s", "exceeded ttl", output)
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
	if store.markDispatchedCalls != 0 {
		t.Errorf("MarkReactionDispatched calls = %d, want 0 (mark deferred to dispatch site)", store.markDispatchedCalls)
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
	if store.markDispatchedCalls != 0 {
		t.Errorf("MarkReactionDispatched calls = %d, want 0 (mark deferred to dispatch site)", store.markDispatchedCalls)
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

// TestReconcileReviewComments_TurnCapExceeded_CrossKindIsolation verifies
// that review escalation clears only the review reaction's own pending
// entry, counter, and fingerprint. A sibling merge-completion entry parked
// on the same issue (seeded from the same worker exit as the review
// reaction) must survive: the escalation must not clear it.
func TestReconcileReviewComments_TurnCapExceeded_CrossKindIsolation(t *testing.T) {
	t.Parallel()

	issueID := "ISS-R-ISO"
	state := stateWithReviewReaction(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindReview)
	state.ReactionAttempts[rkey] = 3

	mcKey := ReactionKey(issueID, ReactionKindMergeCompletion)
	state.PendingReactions[mcKey] = &PendingReaction{
		IssueID:    issueID,
		Identifier: issueID + "-ident",
		Kind:       ReactionKindMergeCompletion,
		CreatedAt:  reviewBaseTime,
		KindData:   &MergeCompletionReactionData{PRNumber: 42, Owner: "owner", Repo: "repo"},
	}
	state.ReactionAttempts[ReactionKey(issueID, ReactionKindMergeCompletion)] = 1

	store := &reviewReconcileStore{}
	metrics := newReviewMetricsSpy()
	tracker := &reviewTrackerStub{}
	scm := &mockSCMAdapter{}
	params := reviewParams(store, scm, tracker)

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	if _, ok := state.PendingReactions[mcKey]; !ok {
		t.Error("sibling merge-completion PendingReactions entry removed by review escalation; want untouched")
	}
	if state.ReactionAttempts[ReactionKey(issueID, ReactionKindMergeCompletion)] != 1 {
		t.Error("sibling merge-completion ReactionAttempts counter altered by review escalation; want untouched")
	}
	if store.deleteFingerprintCalls != 1 {
		t.Errorf("DeleteReactionFingerprint calls = %d, want 1 (the review kind's own fingerprint)", store.deleteFingerprintCalls)
	}
}

// TestReconcileReviewComments_BotAllowlistExclusion verifies that
// reconcileReviewComments drops a fetched comment whose author matches
// params.BotReviewConfig.BotUsernames before the fingerprint and the
// dispatch decision, leaving comments from non-allowlisted authors and
// comments with no known author untouched.
func TestReconcileReviewComments_BotAllowlistExclusion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		issueID           string
		comments          []domain.ReviewComment
		allowlist         []string
		wantDispatch      bool
		wantUpsertCalls   int
		wantSurvivingIDs  []string
		wantExcludedCount int
	}{
		{
			name:    "single allowlisted author excluded, no dispatch",
			issueID: "ISS-R-BOT-1",
			comments: []domain.ReviewComment{
				{ID: "1", Reviewer: "houndci-bot", SubmittedAt: reviewBaseTime.Add(-5 * time.Minute)},
			},
			allowlist:         []string{"houndci-bot"},
			wantDispatch:      false,
			wantUpsertCalls:   0,
			wantExcludedCount: 1,
		},
		{
			name:    "single non-allowlisted author dispatches as today",
			issueID: "ISS-R-BOT-2",
			comments: []domain.ReviewComment{
				{ID: "2", Reviewer: "alice", SubmittedAt: reviewBaseTime.Add(-5 * time.Minute)},
			},
			allowlist:         []string{"houndci-bot"},
			wantDispatch:      true,
			wantUpsertCalls:   1,
			wantSurvivingIDs:  []string{"2"},
			wantExcludedCount: 0,
		},
		{
			name:    "mixed set dispatches only surviving comments",
			issueID: "ISS-R-BOT-3",
			comments: []domain.ReviewComment{
				{ID: "3", Reviewer: "houndci-bot", SubmittedAt: reviewBaseTime.Add(-5 * time.Minute)},
				{ID: "4", Reviewer: "alice", SubmittedAt: reviewBaseTime.Add(-5 * time.Minute)},
			},
			allowlist:         []string{"houndci-bot"},
			wantDispatch:      true,
			wantUpsertCalls:   1,
			wantSurvivingIDs:  []string{"4"},
			wantExcludedCount: 1,
		},
		{
			name:    "case-differing allowlist entry still excludes",
			issueID: "ISS-R-BOT-4",
			comments: []domain.ReviewComment{
				{ID: "5", Reviewer: "houndci-bot", SubmittedAt: reviewBaseTime.Add(-5 * time.Minute)},
			},
			allowlist:         []string{"Houndci-Bot"},
			wantDispatch:      false,
			wantUpsertCalls:   0,
			wantExcludedCount: 1,
		},
		{
			name:    "nil allowlist excludes nothing",
			issueID: "ISS-R-BOT-5",
			comments: []domain.ReviewComment{
				{ID: "6", Reviewer: "houndci-bot", SubmittedAt: reviewBaseTime.Add(-5 * time.Minute)},
			},
			allowlist:         nil,
			wantDispatch:      true,
			wantUpsertCalls:   1,
			wantSurvivingIDs:  []string{"6"},
			wantExcludedCount: 0,
		},
		{
			name:    "empty reviewer not excluded even with empty-string allowlist entry",
			issueID: "ISS-R-BOT-6",
			comments: []domain.ReviewComment{
				{ID: "7", Reviewer: "", SubmittedAt: reviewBaseTime.Add(-5 * time.Minute)},
			},
			allowlist:         []string{""},
			wantDispatch:      true,
			wantUpsertCalls:   1,
			wantSurvivingIDs:  []string{"7"},
			wantExcludedCount: 0,
		},
		{
			name:    "outdated-and-allowlisted comment does not raise excluded_count",
			issueID: "ISS-R-BOT-7",
			comments: []domain.ReviewComment{
				{ID: "8", Reviewer: "houndci-bot", Outdated: true, SubmittedAt: reviewBaseTime.Add(-1 * time.Hour)},
				{ID: "9", Reviewer: "houndci-bot", SubmittedAt: reviewBaseTime.Add(-5 * time.Minute)},
				{ID: "10", Reviewer: "alice", SubmittedAt: reviewBaseTime.Add(-5 * time.Minute)},
			},
			allowlist:         []string{"houndci-bot"},
			wantDispatch:      true,
			wantUpsertCalls:   1,
			wantSurvivingIDs:  []string{"10"},
			wantExcludedCount: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := stateWithReviewReaction(t, tt.issueID, 10)
			rkey := ReactionKey(tt.issueID, ReactionKindReview)

			store := &reviewReconcileStore{}
			metrics := newReviewMetricsSpy()
			scm := &mockSCMAdapter{comments: tt.comments}
			params := reviewParams(store, scm, nil)
			params.BotReviewConfig.BotUsernames = tt.allowlist

			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

			reconcileReviewComments(state, params, logger, context.Background(), metrics)

			_, rePending := state.PendingReactions[rkey]
			if tt.wantDispatch && rePending {
				t.Error("PendingReactions entry re-enqueued; want consumed by dispatch")
			}
			if !tt.wantDispatch && !rePending {
				t.Error("PendingReactions entry consumed; want re-enqueued (all comments excluded)")
			}

			if store.upsertFingerprintCalls != tt.wantUpsertCalls {
				t.Errorf("UpsertReactionFingerprint calls = %d, want %d", store.upsertFingerprintCalls, tt.wantUpsertCalls)
			}

			wantDispatchCount := 0
			if tt.wantDispatch {
				wantDispatchCount = 1
			}
			if metrics.reviewChecks["dispatched"] != wantDispatchCount {
				t.Errorf(`IncReviewChecks("dispatched") = %d, want %d`, metrics.reviewChecks["dispatched"], wantDispatchCount)
			}

			logOutput := buf.String()
			if tt.wantExcludedCount > 0 {
				assertLogLineHasIntAttr(t, logOutput, reviewExclusionDebugMsg, "excluded_count", tt.wantExcludedCount)
			} else {
				assertLogLacksLine(t, logOutput, reviewExclusionDebugMsg)
			}

			if !tt.wantDispatch {
				return
			}

			retry, ok := state.RetryAttempts[tt.issueID]
			if !ok {
				t.Fatal("retry not scheduled after dispatch; want scheduled")
			}
			reviewCtx, ok := retry.ContinuationContext["review_comments"].([]map[string]any)
			if !ok {
				t.Fatalf("ContinuationContext[review_comments] = %#v, want []map[string]any", retry.ContinuationContext["review_comments"])
			}
			gotIDs := make([]string, len(reviewCtx))
			for i, m := range reviewCtx {
				gotIDs[i], _ = m["id"].(string)
			}
			if !slices.Equal(gotIDs, tt.wantSurvivingIDs) {
				t.Errorf("surviving comment ids = %v, want %v", gotIDs, tt.wantSurvivingIDs)
			}

			assertLogLineHasIntAttr(t, logOutput, reviewDispatchInfoMsg, "excluded_count", tt.wantExcludedCount)
		})
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

func TestBuildReviewReactionConfig_WatchWindowMSDefault(t *testing.T) {
	t.Parallel()

	got, err := BuildReviewReactionConfig(config.ReactionConfig{})
	if err != nil {
		t.Fatalf("BuildReviewReactionConfig: %v", err)
	}
	if got.WatchWindowMS != reactionWatchWindowDefaultMS {
		t.Errorf("WatchWindowMS = %d, want %d (default)", got.WatchWindowMS, reactionWatchWindowDefaultMS)
	}
}

func TestBuildReviewReactionConfig_WatchWindowMSOverride(t *testing.T) {
	t.Parallel()

	rc := config.ReactionConfig{Extra: map[string]any{"watch_window_ms": 600000}}

	got, err := BuildReviewReactionConfig(rc)
	if err != nil {
		t.Fatalf("BuildReviewReactionConfig: %v", err)
	}
	if got.WatchWindowMS != 600000 {
		t.Errorf("WatchWindowMS = %d, want 600000", got.WatchWindowMS)
	}
}

func TestBuildReviewReactionConfig_WatchWindowMSZeroDisablesBound(t *testing.T) {
	t.Parallel()

	rc := config.ReactionConfig{Extra: map[string]any{"watch_window_ms": 0}}

	got, err := BuildReviewReactionConfig(rc)
	if err != nil {
		t.Fatalf("BuildReviewReactionConfig: unexpected error for watch_window_ms=0: %v", err)
	}
	if got.WatchWindowMS != 0 {
		t.Errorf("WatchWindowMS = %d, want 0", got.WatchWindowMS)
	}
}

func TestBuildReviewReactionConfig_WatchWindowMSNegative(t *testing.T) {
	t.Parallel()

	rc := config.ReactionConfig{Extra: map[string]any{"watch_window_ms": -1}}

	_, err := BuildReviewReactionConfig(rc)
	if err == nil {
		t.Fatal("BuildReviewReactionConfig: expected error for negative watch_window_ms, got nil")
	}
}

func TestBuildReviewReactionConfig_WatchWindowMSAboveCeiling(t *testing.T) {
	t.Parallel()

	rc := config.ReactionConfig{Extra: map[string]any{"watch_window_ms": int(config.MaxWatchWindowMS) + 1}}

	_, err := BuildReviewReactionConfig(rc)
	if err == nil {
		t.Fatal("BuildReviewReactionConfig: expected error for watch_window_ms above ceiling, got nil")
	}
}

func TestBuildReviewReactionConfig_WatchWindowMSNonNumeric(t *testing.T) {
	t.Parallel()

	rc := config.ReactionConfig{Extra: map[string]any{"watch_window_ms": "soon"}}

	_, err := BuildReviewReactionConfig(rc)
	if err == nil {
		t.Fatal("BuildReviewReactionConfig: expected error for non-numeric watch_window_ms, got nil")
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

// --- PollIntervalMS guard tests ---

// TestReconcileReviewComments_ZeroPollInterval_NoActionableComments verifies
// that a zero or negative PollIntervalMS falls back to reviewPendingBackoffBase
// when re-enqueuing after receiving no actionable review comments.
func TestReconcileReviewComments_ZeroPollInterval_NoActionableComments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		pollIntervalMS int
	}{
		{"zero poll interval", 0},
		{"negative poll interval", -1000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			state := stateWithReviewReaction(t, "ISS-R-ZP-NA", 10)
			rkey := ReactionKey("ISS-R-ZP-NA", ReactionKindReview)
			store := &reviewReconcileStore{}
			metrics := newReviewMetricsSpy()
			scm := &mockSCMAdapter{comments: []domain.ReviewComment{}}
			params := reviewParams(store, scm, nil)
			params.ReviewConfig.PollIntervalMS = tt.pollIntervalMS

			reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

			entry, ok := state.PendingReactions[rkey]
			if !ok {
				t.Fatal("PendingReactions entry dropped with no actionable comments; want re-enqueued")
			}
			want := reviewBaseTime.Add(reviewPendingBackoffBase)
			if !entry.PendingRetryAt.Equal(want) {
				t.Errorf("reconcileReviewComments(PollIntervalMS=%d) PendingRetryAt = %v, want %v (reviewPendingBackoffBase fallback)",
					tt.pollIntervalMS, entry.PendingRetryAt, want)
			}
		})
	}
}

// TestReconcileReviewComments_ZeroPollInterval_AlreadyDispatched verifies
// that a zero PollIntervalMS falls back to reviewPendingBackoffBase when
// re-enqueuing after a fingerprint match with dispatched=true.
func TestReconcileReviewComments_ZeroPollInterval_AlreadyDispatched(t *testing.T) {
	t.Parallel()

	state := stateWithReviewReaction(t, "ISS-R-ZP-D", 10)
	rkey := ReactionKey("ISS-R-ZP-D", ReactionKindReview)

	comments := []domain.ReviewComment{
		{ID: "700", Body: "fix me", SubmittedAt: reviewBaseTime.Add(-5 * time.Minute)},
	}
	fp := buildReviewFingerprint(comments)

	store := &reviewReconcileStore{
		getFingerprintResult:     fp,
		getFingerprintDispatched: true,
	}
	metrics := newReviewMetricsSpy()
	scm := &mockSCMAdapter{comments: comments}
	params := reviewParams(store, scm, nil)
	params.ReviewConfig.PollIntervalMS = 0

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	entry, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions entry dropped for already-dispatched fingerprint; want re-enqueued")
	}
	want := reviewBaseTime.Add(reviewPendingBackoffBase)
	if !entry.PendingRetryAt.Equal(want) {
		t.Errorf("reconcileReviewComments(PollIntervalMS=0, dispatched=true) PendingRetryAt = %v, want %v (reviewPendingBackoffBase fallback)",
			entry.PendingRetryAt, want)
	}
	if store.markDispatchedCalls != 0 {
		t.Errorf("MarkReactionDispatched calls = %d, want 0 (already dispatched)", store.markDispatchedCalls)
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
			got := computeReactionPendingDelay(tt.attempts)
			if got < tt.wantMin || got > tt.wantMax {
				t.Errorf("computeReactionPendingDelay(%d) = %v, want in [%v, %v]",
					tt.attempts, got, tt.wantMin, tt.wantMax)
			}
		})
	}
}

// TestReconcileReviewComments_ForeignIncumbentDefers verifies that a
// foreign incumbent occupying the retry slot leaves the review pending
// entry re-enqueued rather than dispatched, and that
// state.ReactionAttempts for the review kind is left unchanged.
func TestReconcileReviewComments_ForeignIncumbentDefers(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-R-DEFER"
	state := stateWithReviewReaction(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindReview)
	state.RetryAttempts[issueID] = &RetryEntry{
		IssueID:      issueID,
		Attempt:      1,
		ReactionKind: ReactionKindLabelReview,
	}

	// Comment submitted 5 minutes ago — outside the debounce window.
	comments := []domain.ReviewComment{
		{ID: "900", Body: "needs fix", SubmittedAt: reviewBaseTime.Add(-5 * time.Minute)},
	}
	store := &reviewReconcileStore{}
	metrics := newReviewMetricsSpy()
	scm := &mockSCMAdapter{comments: comments}
	params := reviewParams(store, scm, nil)

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	entry, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions entry dropped on a defer; want re-enqueued")
	}
	if !entry.CreatedAt.Equal(reviewBaseTime) {
		t.Errorf("CreatedAt = %v, want refreshed to %v", entry.CreatedAt, reviewBaseTime)
	}
	incumbent := state.RetryAttempts[issueID]
	if incumbent.ReactionKind != ReactionKindLabelReview {
		t.Errorf("RetryAttempts.ReactionKind = %q, want %q (incumbent unchanged)", incumbent.ReactionKind, ReactionKindLabelReview)
	}
	if incumbent.Attempt != 1 {
		t.Errorf("RetryAttempts.Attempt = %d, want 1 (unchanged)", incumbent.Attempt)
	}
	if _, ok := state.ReactionAttempts[rkey]; ok {
		t.Errorf("ReactionAttempts[%s] = %d, want absent (a defer must not increment it)", rkey, state.ReactionAttempts[rkey])
	}
	if metrics.reviewChecks["dispatched"] != 0 {
		t.Errorf(`IncReviewChecks("dispatched") = %d, want 0 (no dispatch on a defer)`, metrics.reviewChecks["dispatched"])
	}
}

// --- Triage gate integration ---

// reviewTriageParams returns reviewParams wired with a real workspace
// and the given triage script, so reactionTriageGate actually starts a
// subprocess for the pass's actionable comment set.
func reviewTriageParams(store *reviewReconcileStore, scm domain.SCMAdapter, tracker domain.TrackerAdapter, workspaceRoot, script string) ReconcileParams {
	params := reviewParams(store, scm, tracker)
	params.WorkspaceRoot = workspaceRoot
	params.ReviewConfig.Triage = config.ReactionTriageConfig{Script: script, TimeoutMS: 5000}
	return params
}

// oldEnoughReviewComments returns one actionable comment submitted well
// outside the default debounce window.
func oldEnoughReviewComments() []domain.ReviewComment {
	return []domain.ReviewComment{
		{ID: "rc-1", Body: "fix this", SubmittedAt: reviewBaseTime.Add(-time.Hour)},
	}
}

// runReviewTriageToCompletion drives a pass that starts a triage run for
// issueID, waits for the subprocess to finish, then resets the entry's
// PendingRetryAt to the past so the next pass is immediately due.
func runReviewTriageToCompletion(t *testing.T, state *State, params ReconcileParams, rkey string, metrics domain.Metrics) {
	t.Helper()
	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)
	entry, ok := state.PendingReactions[rkey]
	if !ok || entry.Triage == nil {
		t.Fatalf("PendingReactions[%s] = %+v, want a started triage run", rkey, entry)
	}
	waitTriageRunDone(t, entry.Triage)
	entry.PendingRetryAt = time.Time{}
}

// TestReconcileReviewComments_Triage_NoConfig_BehavesAsPinned verifies
// that a review reaction with no triage block dispatches exactly as the
// pinned revision.
func TestReconcileReviewComments_Triage_NoConfig_BehavesAsPinned(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-R-TRIAGE-OFF"
	state := stateWithReviewReaction(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindReview)
	store := &reviewReconcileStore{}
	metrics := newReviewMetricsSpy()
	scm := &mockSCMAdapter{comments: oldEnoughReviewComments()}
	params := reviewParams(store, scm, nil)
	// WorkspaceRoot and ReviewConfig.Triage are left at their zero values.

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry survived a scheduled continuation, want dropped (pinned behavior)")
	}
	if metrics.reviewChecks["dispatched"] != 1 {
		t.Errorf(`IncReviewChecks("dispatched") = %d, want 1`, metrics.reviewChecks["dispatched"])
	}
	if state.ReactionAttempts[rkey] != 1 {
		t.Errorf("ReactionAttempts[%s] = %d, want 1", rkey, state.ReactionAttempts[rkey])
	}
	if store.markDispatchedCalls != 0 {
		t.Errorf("MarkReactionDispatched calls = %d, want 0", store.markDispatchedCalls)
	}
}

// TestReconcileReviewComments_Triage_WaitsWithoutProviderCall verifies
// that while a triage run is in flight, the pass re-enqueues without
// making a provider call and without incrementing PendingAttempts.
func TestReconcileReviewComments_Triage_WaitsWithoutProviderCall(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-R-TRIAGE-WAIT"
	identifier := issueID + "-ident"
	root := mustTriageWorkspace(t, identifier)
	state := stateWithReviewReaction(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindReview)
	state.PendingReactions[rkey].Triage = inFlightTriageRun("fp-wait", func() {})

	store := &reviewReconcileStore{}
	metrics := newReviewMetricsSpy()
	scm := &mockSCMAdapter{comments: oldEnoughReviewComments()}
	params := reviewTriageParams(store, scm, nil, root, handledScript)

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	if scm.calls != 0 {
		t.Errorf("FetchPendingReviews calls = %d, want 0 while a triage run is in flight", scm.calls)
	}
	entry, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions entry dropped while waiting on triage, want re-enqueued")
	}
	if entry.PendingAttempts != 0 {
		t.Errorf("PendingAttempts = %d, want 0 (waiting is not a fetch error)", entry.PendingAttempts)
	}
}

// TestReconcileReviewComments_Triage_Handled verifies that a handled
// disposition marks the fingerprint dispatched and re-enqueues the
// entry with the poll interval, without incrementing ReactionAttempts
// or IncReviewChecks("dispatched").
func TestReconcileReviewComments_Triage_Handled(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-R-TRIAGE-HANDLED"
	identifier := issueID + "-ident"
	root := mustTriageWorkspace(t, identifier)
	state := stateWithReviewReaction(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindReview)

	store := &reviewReconcileStore{}
	metrics := newReviewMetricsSpy()
	scm := &mockSCMAdapter{comments: oldEnoughReviewComments()}
	params := reviewTriageParams(store, scm, nil, root, handledScript)

	runReviewTriageToCompletion(t, state, params, rkey, metrics)

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	entry, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions entry dropped after a handled verdict, want re-enqueued")
	}
	if !entry.PendingRetryAt.After(reviewBaseTime) {
		t.Errorf("PendingRetryAt = %v, want after %v (re-enqueued with the poll interval)", entry.PendingRetryAt, reviewBaseTime)
	}
	if state.ReactionAttempts[rkey] != 0 {
		t.Errorf("ReactionAttempts[%s] = %d, want 0 (a handled verdict must not spend a continuation)", rkey, state.ReactionAttempts[rkey])
	}
	if metrics.reviewChecks["dispatched"] != 0 {
		t.Errorf(`IncReviewChecks("dispatched") = %d, want 0 on a handled pass`, metrics.reviewChecks["dispatched"])
	}
	if store.markDispatchedCalls != 1 {
		t.Errorf("MarkReactionDispatched calls = %d, want 1", store.markDispatchedCalls)
	}
}

// TestReconcileReviewComments_Triage_Escalate verifies that an escalate
// disposition invokes escalateReviewFailure with EscalationTriggerTriage
// and the un-incremented turn count.
func TestReconcileReviewComments_Triage_Escalate(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-R-TRIAGE-ESCALATE"
	identifier := issueID + "-ident"
	root := mustTriageWorkspace(t, identifier)
	state := stateWithReviewReaction(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindReview)

	store := &reviewReconcileStore{}
	metrics := newReviewMetricsSpy()
	tracker := &reviewTrackerStub{}
	scm := &mockSCMAdapter{comments: oldEnoughReviewComments()}
	params := reviewTriageParams(store, scm, tracker, root, escalateTriageScript)

	runReviewTriageToCompletion(t, state, params, rkey, metrics)

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	if tracker.addLabelCalled != 1 {
		t.Errorf("AddLabel calls = %d, want 1 (triage escalation uses the kind's own escalation action)", tracker.addLabelCalled)
	}
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry survived a triage escalation, want dropped (matches a budget escalation)")
	}
	if store.markDispatchedCalls != 1 {
		t.Errorf("MarkReactionDispatched calls = %d, want 1", store.markDispatchedCalls)
	}
}

// TestReconcileReviewComments_Triage_DispatchAgent_ProceedsNormally
// verifies that a dispatch-agent disposition falls through to the
// existing dispatch block, incrementing IncReviewChecks("dispatched").
func TestReconcileReviewComments_Triage_DispatchAgent_ProceedsNormally(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-R-TRIAGE-DISPATCH"
	identifier := issueID + "-ident"
	root := mustTriageWorkspace(t, identifier)
	state := stateWithReviewReaction(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindReview)

	store := &reviewReconcileStore{}
	metrics := newReviewMetricsSpy()
	scm := &mockSCMAdapter{comments: oldEnoughReviewComments()}
	params := reviewTriageParams(store, scm, nil, root, dispatchAgentTriageScript)

	runReviewTriageToCompletion(t, state, params, rkey, metrics)

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	if metrics.reviewChecks["dispatched"] != 1 {
		t.Errorf(`IncReviewChecks("dispatched") = %d, want 1`, metrics.reviewChecks["dispatched"])
	}
	if state.ReactionAttempts[rkey] != 1 {
		t.Errorf("ReactionAttempts[%s] = %d, want 1", rkey, state.ReactionAttempts[rkey])
	}
	if store.markDispatchedCalls != 0 {
		t.Errorf("MarkReactionDispatched calls = %d, want 0 for dispatch-agent", store.markDispatchedCalls)
	}
}

// TestReconcileReviewComments_Triage_CancelOnTTLDrop verifies that an
// in-flight triage run is cancelled before the entry is dropped on TTL
// elapse.
func TestReconcileReviewComments_Triage_CancelOnTTLDrop(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-R-TRIAGE-DROP"
	state := stateWithReviewReaction(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindReview)
	spy := &triageCancelSpy{}
	state.PendingReactions[rkey].Triage = inFlightTriageRun("fp-drop", spy.cancel)
	state.PendingReactions[rkey].CreatedAt = reviewBaseTime.Add(-31 * time.Minute)

	store := &reviewReconcileStore{}
	metrics := newReviewMetricsSpy()
	scm := &mockSCMAdapter{}
	params := reviewParams(store, scm, nil)
	params.ReviewPendingTTL = 30 * time.Minute

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	if spy.calls() != 1 {
		t.Errorf("Cancel called %d times, want 1 (the in-flight run must not outlive the dropped entry)", spy.calls())
	}
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry survived past the TTL, want dropped")
	}
}

// TestReconcileReviewComments_Triage_RepeatedHandled_StillAgesOut pins
// the bound review carries and ci does not: the TTL is measured from
// the entry's creation, so a succession of handled answers still ages
// the entry out once that TTL elapses.
func TestReconcileReviewComments_Triage_RepeatedHandled_StillAgesOut(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-R-TRIAGE-AGESOUT"
	identifier := issueID + "-ident"
	root := mustTriageWorkspace(t, identifier)
	state := stateWithReviewReaction(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindReview)

	store := &reviewReconcileStore{}
	metrics := newReviewMetricsSpy()
	scm := &mockSCMAdapter{comments: oldEnoughReviewComments()}
	params := reviewTriageParams(store, scm, nil, root, handledScript)

	runReviewTriageToCompletion(t, state, params, rkey, metrics)
	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics) // applies: handled

	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Fatal("PendingReactions entry dropped right after a handled verdict, want retained until TTL")
	}

	// Age the entry's creation time past the TTL and confirm it still
	// drops despite the retained handled outcome.
	state.PendingReactions[rkey].CreatedAt = reviewBaseTime.Add(-31 * time.Minute)
	state.PendingReactions[rkey].PendingRetryAt = time.Time{}
	params.ReviewPendingTTL = 30 * time.Minute

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry survived past the TTL despite a handled verdict, want dropped")
	}
}

// TestReconcileReviewComments_Triage_EpisodeCloseClearsHandledForNextEpisode
// pins cancelReactionTriage's detach-on-close contract: a memoized
// handled verdict from one episode must not survive into the next.
// Without it, a later episode that recomputes the identical
// actionable-comment fingerprint (the same comment reappearing after
// the thread went quiet) would replay the stale handled outcome from
// memory instead of running the newly configured command, silently
// suppressing the real verdict.
func TestReconcileReviewComments_Triage_EpisodeCloseClearsHandledForNextEpisode(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-R-TRIAGE-EPISODE-CLOSE"
	identifier := issueID + "-ident"
	root := mustTriageWorkspace(t, identifier)
	state := stateWithReviewReaction(t, issueID, 10)
	rkey := ReactionKey(issueID, ReactionKindReview)

	store := &reviewReconcileStore{}
	metrics := newReviewMetricsSpy()
	tracker := &reviewTrackerStub{}
	scm := &mockSCMAdapter{comments: oldEnoughReviewComments()}
	params := reviewTriageParams(store, scm, tracker, root, handledScript)

	// Episode 1: one actionable comment; the command answers handled,
	// memoizing the verdict on pending.Triage rather than re-running it
	// on the next pass over the same comment set.
	runReviewTriageToCompletion(t, state, params, rkey, metrics)
	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	entry, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions entry dropped after a handled verdict, want re-enqueued")
	}
	if entry.Triage == nil {
		t.Fatal("PendingReaction.Triage cleared inside its own episode, want the memoized handle retained")
	}
	entry.PendingRetryAt = time.Time{}

	// The episode closes: the comment set goes empty, taking the
	// no-actionable-comments branch that must cancel the retained
	// handle before re-enqueueing.
	scm.comments = nil

	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)

	entry, ok = state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions entry dropped when the episode closed, want re-enqueued")
	}
	if entry.Triage != nil {
		t.Fatal("PendingReaction.Triage survived the episode close, want detached so a later identical comment set cannot replay it")
	}
	entry.PendingRetryAt = time.Time{}

	// Episode 2: the same comment reappears, recomputing the identical
	// fingerprint. The command now answers escalate; a cleared handle
	// must run it fresh rather than replay episode 1's memoized handled
	// verdict.
	scm.comments = oldEnoughReviewComments()
	params.ReviewConfig.Triage = config.ReactionTriageConfig{Script: escalateTriageScript, TimeoutMS: 5000}

	runReviewTriageToCompletion(t, state, params, rkey, metrics)
	reconcileReviewComments(state, params, discardLogger(), context.Background(), metrics)
	state.TrackerOpsWg.Wait()

	if tracker.addLabelCalled != 1 {
		t.Errorf("AddLabel calls = %d, want 1 (the replayed handled verdict from episode 1 must not suppress the fresh escalate)", tracker.addLabelCalled)
	}
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions entry survived a triage escalation, want dropped (the replayed handled verdict from episode 1 must not suppress it)")
	}
	if store.markDispatchedCalls != 2 {
		t.Errorf("MarkReactionDispatched calls = %d, want 2 (one real verdict per episode: handled, then escalate)", store.markDispatchedCalls)
	}
}
