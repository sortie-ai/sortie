package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
)

// --- Test doubles ---

// unsupportedReactionObservationStore lets unrelated store doubles satisfy
// the compile-time interface while failing loudly if a merge-completion test
// accidentally uses a double without real observation semantics. It also
// supplies a safe, conservative default for CountWorkerRunsCompletedSince
// for any store double that does not implement its own.
type unsupportedReactionObservationStore struct{}

func (unsupportedReactionObservationStore) UpsertReactionObservation(
	context.Context,
	string, string, string,
	time.Time,
) (persistence.ReactionObservation, error) {
	panic("reaction observation persistence is unsupported by this test double")
}

func (unsupportedReactionObservationStore) MarkReactionObservationDispatched(context.Context, string, string, string) error {
	panic("reaction observation persistence is unsupported by this test double")
}

// CountWorkerRunsCompletedSince returns a non-nil error rather than
// (0, nil), so a store double that does not override this method drives
// classifyHeadChange to the conservative "unknown" verdict instead of
// silently resolving "notOurs" for every test that never considered
// attribution.
func (unsupportedReactionObservationStore) CountWorkerRunsCompletedSince(context.Context, string, time.Time) (int, error) {
	return 0, errors.New("worker run count is unsupported by this test double")
}

// mockReconcileStore records calls to ReconcileStore methods and returns
// configurable errors.
type mockReconcileStore struct {
	unsupportedReactionObservationStore

	savedEntries   []persistence.RetryEntry
	deletedIssueID []string

	saveRetryEntryErr   error
	deleteRetryEntryErr error

	upsertFingerprintCalls int
	getFingerprintCalls    int
	markDispatchedCalls    int
	deleteFingerprintCalls int
}

var _ ReconcileStore = (*mockReconcileStore)(nil)

func (m *mockReconcileStore) SaveRetryEntry(_ context.Context, entry persistence.RetryEntry) error {
	m.savedEntries = append(m.savedEntries, entry)
	return m.saveRetryEntryErr
}

func (m *mockReconcileStore) DeleteRetryEntry(_ context.Context, issueID string) error {
	m.deletedIssueID = append(m.deletedIssueID, issueID)
	return m.deleteRetryEntryErr
}

func (m *mockReconcileStore) AppendRunHistory(_ context.Context, run persistence.RunHistory) (persistence.RunHistory, error) {
	return run, nil
}

func (m *mockReconcileStore) UpsertReactionFingerprint(_ context.Context, _, _, _ string) error {
	m.upsertFingerprintCalls++
	return nil
}

func (m *mockReconcileStore) GetReactionFingerprint(_ context.Context, _, _ string) (string, bool, error) {
	m.getFingerprintCalls++
	return "", false, nil
}

func (m *mockReconcileStore) MarkReactionDispatched(_ context.Context, _, _ string) error {
	m.markDispatchedCalls++
	return nil
}

func (m *mockReconcileStore) DeleteReactionFingerprint(_ context.Context, _, _ string) error {
	m.deleteFingerprintCalls++
	return nil
}

// mockReconcileTracker implements domain.TrackerAdapter for reconcile tests.
// Only FetchIssueStatesByIDs is exercised; the other methods panic if called.
type mockReconcileTracker struct {
	states   map[string]string
	fetchErr error

	calls      int
	calledWith []string // copy of the ids passed to the most recent FetchIssueStatesByIDs call
}

var _ domain.TrackerAdapter = (*mockReconcileTracker)(nil)

func (m *mockReconcileTracker) FetchIssueStatesByIDs(_ context.Context, ids []string) (map[string]string, error) {
	m.calls++
	cp := make([]string, len(ids))
	copy(cp, ids)
	m.calledWith = cp
	return m.states, m.fetchErr
}

func (m *mockReconcileTracker) FetchCandidateIssues(context.Context) ([]domain.Issue, error) {
	panic("FetchCandidateIssues must not be called by ReconcileRunningIssues")
}

func (m *mockReconcileTracker) FetchIssueByID(context.Context, string) (domain.Issue, error) {
	panic("FetchIssueByID must not be called by ReconcileRunningIssues")
}

func (m *mockReconcileTracker) FetchIssuesByStates(context.Context, []string) ([]domain.Issue, error) {
	panic("FetchIssuesByStates must not be called by ReconcileRunningIssues")
}

func (m *mockReconcileTracker) FetchIssueStatesByIdentifiers(context.Context, []string) (map[string]string, error) {
	panic("FetchIssueStatesByIdentifiers must not be called by ReconcileRunningIssues")
}

func (m *mockReconcileTracker) FetchIssueComments(context.Context, string) ([]domain.Comment, error) {
	panic("FetchIssueComments must not be called by ReconcileRunningIssues")
}

func (m *mockReconcileTracker) TransitionIssue(context.Context, string, string) error {
	panic("TransitionIssue must not be called by ReconcileRunningIssues")
}

func (m *mockReconcileTracker) CommentIssue(context.Context, string, string) error {
	panic("CommentIssue must not be called by ReconcileRunningIssues")
}

func (m *mockReconcileTracker) AddLabel(context.Context, string, string) error {
	panic("AddLabel must not be called by ReconcileRunningIssues")
}

// sweepTracker is a test double for [SweepWorkspaces]. Its
// FetchIssueStatesByIdentifiers records the identifiers it receives and
// returns the configured statesByKey map and fetchErr. All other methods
// panic if called, matching the guard pattern in [mockReconcileTracker].
type sweepTracker struct {
	calledWith  []string          // copy of ids passed to FetchIssueStatesByIdentifiers
	statesByKey map[string]string // keyed by workspace key (sanitized identifier)
	fetchErr    error
}

var _ domain.TrackerAdapter = (*sweepTracker)(nil)

func (s *sweepTracker) FetchIssueStatesByIdentifiers(_ context.Context, ids []string) (map[string]string, error) {
	cp := make([]string, len(ids))
	copy(cp, ids)
	s.calledWith = cp
	return s.statesByKey, s.fetchErr
}

func (s *sweepTracker) FetchCandidateIssues(context.Context) ([]domain.Issue, error) {
	panic("FetchCandidateIssues must not be called by SweepWorkspaces")
}

func (s *sweepTracker) FetchIssueByID(context.Context, string) (domain.Issue, error) {
	panic("FetchIssueByID must not be called by SweepWorkspaces")
}

func (s *sweepTracker) FetchIssuesByStates(context.Context, []string) ([]domain.Issue, error) {
	panic("FetchIssuesByStates must not be called by SweepWorkspaces")
}

func (s *sweepTracker) FetchIssueStatesByIDs(context.Context, []string) (map[string]string, error) {
	panic("FetchIssueStatesByIDs must not be called by SweepWorkspaces")
}

func (s *sweepTracker) FetchIssueComments(context.Context, string) ([]domain.Comment, error) {
	panic("FetchIssueComments must not be called by SweepWorkspaces")
}

func (s *sweepTracker) TransitionIssue(context.Context, string, string) error {
	panic("TransitionIssue must not be called by SweepWorkspaces")
}

func (s *sweepTracker) CommentIssue(context.Context, string, string) error {
	panic("CommentIssue must not be called by SweepWorkspaces")
}

func (s *sweepTracker) AddLabel(context.Context, string, string) error {
	panic("AddLabel must not be called by SweepWorkspaces")
}

// panicOnAnySCMAdapter panics on every domain.SCMAdapter method. Used to
// prove that a terminal-issue release triggers no SCM call of any kind.
type panicOnAnySCMAdapter struct{}

var _ domain.SCMAdapter = panicOnAnySCMAdapter{}

func (panicOnAnySCMAdapter) FetchPendingReviews(context.Context, int, string, string) ([]domain.ReviewComment, error) {
	panic("FetchPendingReviews must not be called after a terminal release")
}

func (panicOnAnySCMAdapter) FetchBotReviewComments(context.Context, int, string, string, []string) ([]domain.ReviewComment, error) {
	panic("FetchBotReviewComments must not be called after a terminal release")
}

func (panicOnAnySCMAdapter) GetReviewDecision(context.Context, int, string, string) (domain.ReviewDecision, error) {
	panic("GetReviewDecision must not be called after a terminal release")
}

func (panicOnAnySCMAdapter) GetCIStatus(context.Context, int, string, string) (string, error) {
	panic("GetCIStatus must not be called after a terminal release")
}

func (panicOnAnySCMAdapter) GetMergeability(context.Context, int, string, string) (domain.PRMergeStatus, error) {
	panic("GetMergeability must not be called after a terminal release")
}

func (panicOnAnySCMAdapter) MergePR(context.Context, int, string, string, domain.MergeStrategy, string, string, string) (domain.MergeResult, error) {
	panic("MergePR must not be called after a terminal release")
}

func (panicOnAnySCMAdapter) DeleteBranch(context.Context, string, string, string) error {
	panic("DeleteBranch must not be called after a terminal release")
}

func (panicOnAnySCMAdapter) ListLabelEvents(context.Context, int, string, string) ([]domain.LabelEvent, error) {
	panic("ListLabelEvents must not be called after a terminal release")
}

func (panicOnAnySCMAdapter) RemoveLabel(context.Context, int, string, string, string) error {
	panic("RemoveLabel must not be called after a terminal release")
}

// panicOnAnyCIProvider panics on every domain.CIStatusProvider method. Used
// to prove that a terminal-issue release triggers no CI status read.
type panicOnAnyCIProvider struct{}

var _ domain.CIStatusProvider = panicOnAnyCIProvider{}

func (panicOnAnyCIProvider) FetchCIStatus(context.Context, string) (domain.CIResult, error) {
	panic("FetchCIStatus must not be called after a terminal release")
}

// --- Test helpers ---

// reconcileBaseTime is a fixed reference for reconcile tests.
var reconcileBaseTime = time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)

// defaultReconcileParams returns ReconcileParams with sensible defaults and
// the given mocks wired in.
func defaultReconcileParams(t *testing.T, store *mockReconcileStore, tracker *mockReconcileTracker) ReconcileParams {
	t.Helper()
	return ReconcileParams{
		TrackerAdapter:    tracker,
		ActiveStates:      []string{"In Progress", "In Review"},
		TerminalStates:    []string{"Done", "Closed"},
		StallTimeoutMS:    60_000,
		MaxRetryBackoffMS: 300_000,
		Store:             store,
		OnRetryFire:       noopRetryFire,
		NowFunc:           func() time.Time { return reconcileBaseTime },
		Ctx:               context.Background(),
		Logger:            discardLogger(),
	}
}

// cancelCounter tracks the number of times a CancelFunc was called.
type cancelCounter struct {
	count int
}

func (c *cancelCounter) cancel() {
	c.count++
}

// terminalReleaseIssueID is the issue id used by the terminal-release test
// fixture shared across the running-issue, fetch-failure, and
// no-side-effects release tests.
const terminalReleaseIssueID = "REL-1"

// terminalReleaseFixtureKinds are the reaction kinds seeded on the shared
// terminal-release fixture, one non-expiring (label-review) and two
// expiring (ci, review), so the release is proven to span both shapes.
var terminalReleaseFixtureKinds = []string{ReactionKindCI, ReactionKindReview, ReactionKindLabelReview}

// stateWithTerminalReleaseFixture builds a State with a running entry for
// [terminalReleaseIssueID] backed by cc, a live retry, a claim, and a
// pending entry plus a non-zero attempt counter for each kind in
// [terminalReleaseFixtureKinds].
func stateWithTerminalReleaseFixture(t *testing.T, cc *cancelCounter) *State {
	t.Helper()

	state := NewState(5000, 4, nil, AgentTotals{})
	state.Running[terminalReleaseIssueID] = &RunningEntry{
		Identifier: terminalReleaseIssueID + "-ident",
		StartedAt:  reconcileBaseTime,
		CancelFunc: cc.cancel,
		Issue:      domain.Issue{State: "In Progress"},
	}
	state.Claimed[terminalReleaseIssueID] = struct{}{}
	state.RetryAttempts[terminalReleaseIssueID] = &RetryEntry{
		IssueID:    terminalReleaseIssueID,
		Identifier: terminalReleaseIssueID + "-ident",
		Attempt:    1,
	}
	for _, kind := range terminalReleaseFixtureKinds {
		key := ReactionKey(terminalReleaseIssueID, kind)
		state.PendingReactions[key] = &PendingReaction{
			IssueID:    terminalReleaseIssueID,
			Identifier: terminalReleaseIssueID + "-ident",
			Kind:       kind,
			CreatedAt:  reconcileBaseTime,
		}
		state.ReactionAttempts[key] = 2
	}
	return state
}

// --- Part A: Stall detection tests ---

func TestReconcileStalled_Disabled(t *testing.T) {
	t.Parallel()

	store := &mockReconcileStore{}
	tracker := &mockReconcileTracker{states: map[string]string{}}
	params := defaultReconcileParams(t, store, tracker)
	params.StallTimeoutMS = 0 // disabled

	state := NewState(5000, 4, nil, AgentTotals{})
	cc := &cancelCounter{}
	state.Running["ISSUE-1"] = &RunningEntry{
		Identifier: "ISSUE-1-ident",
		StartedAt:  reconcileBaseTime.Add(-120 * time.Second),
		CancelFunc: cc.cancel,
	}
	state.Claimed["ISSUE-1"] = struct{}{}

	ReconcileRunningIssues(state, params)

	if cc.count != 0 {
		t.Error("CancelFunc called despite stall detection being disabled")
	}
	if _, ok := state.RetryAttempts["ISSUE-1"]; ok {
		t.Error("retry scheduled despite stall detection being disabled")
	}
}

func TestReconcileStalled_NoStalls(t *testing.T) {
	t.Parallel()

	store := &mockReconcileStore{}
	tracker := &mockReconcileTracker{states: map[string]string{
		"ISSUE-1": "In Progress",
	}}
	params := defaultReconcileParams(t, store, tracker)
	params.StallTimeoutMS = 60_000
	// Now = reconcileBaseTime; entry started 30s ago → not stalled.
	state := NewState(5000, 4, nil, AgentTotals{})
	state.Running["ISSUE-1"] = &RunningEntry{
		Identifier: "ISSUE-1-ident",
		StartedAt:  reconcileBaseTime.Add(-30 * time.Second),
	}
	state.Claimed["ISSUE-1"] = struct{}{}

	ReconcileRunningIssues(state, params)

	if _, ok := state.RetryAttempts["ISSUE-1"]; ok {
		t.Error("retry scheduled for non-stalled entry")
	}
	if len(store.savedEntries) != 0 {
		t.Errorf("SaveRetryEntry called %d times, want 0", len(store.savedEntries))
	}
}

func TestReconcileStalled_ViaLastAgentTimestamp(t *testing.T) {
	t.Parallel()

	store := &mockReconcileStore{}
	tracker := &mockReconcileTracker{states: map[string]string{
		"ISSUE-1": "In Progress",
	}}
	params := defaultReconcileParams(t, store, tracker)
	params.StallTimeoutMS = 60_000

	state := NewState(5000, 4, nil, AgentTotals{})
	cc := &cancelCounter{}
	state.Running["ISSUE-1"] = &RunningEntry{
		Identifier:         "ISSUE-1-ident",
		StartedAt:          reconcileBaseTime.Add(-30 * time.Second),
		LastAgentTimestamp: reconcileBaseTime.Add(-90 * time.Second), // 90s ago > 60s threshold
		CancelFunc:         cc.cancel,
	}
	state.Claimed["ISSUE-1"] = struct{}{}

	ReconcileRunningIssues(state, params)

	if cc.count != 1 {
		t.Errorf("CancelFunc called %d times, want 1", cc.count)
	}

	retryEntry, ok := state.RetryAttempts["ISSUE-1"]
	if !ok {
		t.Fatal("retry not scheduled for stalled entry")
	}
	if retryEntry.Attempt != 1 {
		t.Errorf("retry Attempt = %d, want 1", retryEntry.Attempt)
	}
	if retryEntry.Error != "stall timeout exceeded" {
		t.Errorf("retry Error = %q, want %q", retryEntry.Error, "stall timeout exceeded")
	}
	if len(store.savedEntries) != 1 {
		t.Fatalf("SaveRetryEntry called %d times, want 1", len(store.savedEntries))
	}
	if store.savedEntries[0].IssueID != "ISSUE-1" {
		t.Errorf("saved IssueID = %q, want %q", store.savedEntries[0].IssueID, "ISSUE-1")
	}
}

func TestReconcileStalled_ReactionRetryPreservesContext(t *testing.T) {
	t.Parallel()

	store := &mockReconcileStore{}
	tracker := &mockReconcileTracker{states: map[string]string{
		"ISSUE-R": "Ready For Review",
	}}
	params := defaultReconcileParams(t, store, tracker)
	params.StallTimeoutMS = 60_000
	params.HandoffState = "Ready For Review"

	contContext := map[string]any{
		"review_comments": map[string]any{"count": 2},
	}
	state := NewState(5000, 4, nil, AgentTotals{})
	cc := &cancelCounter{}
	state.Running["ISSUE-R"] = &RunningEntry{
		Identifier:          "ISSUE-R-ident",
		Issue:               candidateIssue("ISSUE-R", "ISSUE-R-ident", "Ready For Review"),
		SessionID:           "sess-r",
		SSHHost:             "host-r",
		StartedAt:           reconcileBaseTime.Add(-120 * time.Second),
		CancelFunc:          cc.cancel,
		ContinuationContext: contContext,
		ReactionKind:        ReactionKindReview,
	}
	state.Claimed["ISSUE-R"] = struct{}{}

	ReconcileRunningIssues(state, params)

	if cc.count != 1 {
		t.Errorf("CancelFunc called %d times, want 1", cc.count)
	}
	retryEntry, ok := state.RetryAttempts["ISSUE-R"]
	if !ok {
		t.Fatal("retry not scheduled for stalled reaction entry")
	}
	if retryEntry.ReactionKind != ReactionKindReview {
		t.Errorf("RetryEntry.ReactionKind = %q, want %q", retryEntry.ReactionKind, ReactionKindReview)
	}
	if retryEntry.ContinuationContext == nil {
		t.Fatal("RetryEntry.ContinuationContext is nil, want preserved")
	}
	if _, ok := retryEntry.ContinuationContext["review_comments"]; !ok {
		t.Error("RetryEntry.ContinuationContext missing review_comments key")
	}
	if retryEntry.SessionID != "sess-r" {
		t.Errorf("RetryEntry.SessionID = %q, want %q", retryEntry.SessionID, "sess-r")
	}
	if retryEntry.LastSSHHost != "host-r" {
		t.Errorf("RetryEntry.LastSSHHost = %q, want %q", retryEntry.LastSSHHost, "host-r")
	}
	if len(store.savedEntries) != 0 {
		t.Errorf("SaveRetryEntry called %d times, want 0 (reaction retry is runtime-only)", len(store.savedEntries))
	}
	if len(store.deletedIssueID) != 1 || store.deletedIssueID[0] != "ISSUE-R" {
		t.Errorf("DeleteRetryEntry calls = %v, want [ISSUE-R]", store.deletedIssueID)
	}
}

func TestReconcileStalled_ViaStartedAtFallback(t *testing.T) {
	t.Parallel()

	store := &mockReconcileStore{}
	tracker := &mockReconcileTracker{states: map[string]string{
		"ISSUE-1": "In Progress",
	}}
	params := defaultReconcileParams(t, store, tracker)
	params.StallTimeoutMS = 60_000

	state := NewState(5000, 4, nil, AgentTotals{})
	cc := &cancelCounter{}
	// LastAgentTimestamp is zero → falls back to StartedAt.
	state.Running["ISSUE-1"] = &RunningEntry{
		Identifier: "ISSUE-1-ident",
		StartedAt:  reconcileBaseTime.Add(-90 * time.Second), // 90s ago > 60s threshold
		CancelFunc: cc.cancel,
	}
	state.Claimed["ISSUE-1"] = struct{}{}

	ReconcileRunningIssues(state, params)

	if cc.count != 1 {
		t.Errorf("CancelFunc called %d times, want 1", cc.count)
	}
	if _, ok := state.RetryAttempts["ISSUE-1"]; !ok {
		t.Error("retry not scheduled for stalled entry using StartedAt fallback")
	}
}

func TestReconcileStalled_SelectiveStallingMultipleEntries(t *testing.T) {
	t.Parallel()

	store := &mockReconcileStore{}
	tracker := &mockReconcileTracker{states: map[string]string{
		"STALE-1": "In Progress",
		"FRESH-1": "In Progress",
	}}
	params := defaultReconcileParams(t, store, tracker)
	params.StallTimeoutMS = 60_000

	state := NewState(5000, 4, nil, AgentTotals{})
	ccStale := &cancelCounter{}
	ccFresh := &cancelCounter{}

	state.Running["STALE-1"] = &RunningEntry{
		Identifier: "STALE-1-ident",
		StartedAt:  reconcileBaseTime.Add(-120 * time.Second),
		CancelFunc: ccStale.cancel,
	}
	state.Claimed["STALE-1"] = struct{}{}

	state.Running["FRESH-1"] = &RunningEntry{
		Identifier: "FRESH-1-ident",
		StartedAt:  reconcileBaseTime.Add(-10 * time.Second),
		CancelFunc: ccFresh.cancel,
	}
	state.Claimed["FRESH-1"] = struct{}{}

	ReconcileRunningIssues(state, params)

	if ccStale.count != 1 {
		t.Errorf("stale CancelFunc called %d times, want 1", ccStale.count)
	}
	if ccFresh.count != 0 {
		t.Errorf("fresh CancelFunc called %d times, want 0", ccFresh.count)
	}
	if _, ok := state.RetryAttempts["STALE-1"]; !ok {
		t.Error("retry not scheduled for stale entry")
	}
	if _, ok := state.RetryAttempts["FRESH-1"]; ok {
		t.Error("retry incorrectly scheduled for fresh entry")
	}
}

func TestReconcileStalled_PersistenceError(t *testing.T) {
	t.Parallel()

	store := &mockReconcileStore{saveRetryEntryErr: errors.New("db write failed")}
	tracker := &mockReconcileTracker{states: map[string]string{
		"ISSUE-1": "In Progress",
	}}
	params := defaultReconcileParams(t, store, tracker)
	params.StallTimeoutMS = 60_000

	state := NewState(5000, 4, nil, AgentTotals{})
	cc := &cancelCounter{}
	state.Running["ISSUE-1"] = &RunningEntry{
		Identifier: "ISSUE-1-ident",
		StartedAt:  reconcileBaseTime.Add(-90 * time.Second),
		CancelFunc: cc.cancel,
	}
	state.Claimed["ISSUE-1"] = struct{}{}

	// Must not panic despite persistence failure.
	ReconcileRunningIssues(state, params)

	// In-memory retry still scheduled.
	if _, ok := state.RetryAttempts["ISSUE-1"]; !ok {
		t.Error("retry not scheduled despite persistence failure")
	}
	// Store was still called.
	if len(store.savedEntries) != 1 {
		t.Errorf("SaveRetryEntry called %d times, want 1", len(store.savedEntries))
	}
}

// --- Part B: Tracker state refresh tests ---

func TestReconcileTrackerState_NoRunningEntries(t *testing.T) {
	t.Parallel()

	store := &mockReconcileStore{}
	tracker := &mockReconcileTracker{
		states: map[string]string{"GHOST-1": "Done"},
	}
	params := defaultReconcileParams(t, store, tracker)
	params.StallTimeoutMS = 0 // disable stall detection for Part B isolation

	state := NewState(5000, 4, nil, AgentTotals{})

	ReconcileRunningIssues(state, params)

	// tracker.FetchIssueStatesByIDs should not be called when Running is empty.
	// No state changes expected.
	if len(store.deletedIssueID) != 0 {
		t.Errorf("DeleteRetryEntry called %d times, want 0", len(store.deletedIssueID))
	}
}

func TestReconcileTrackerState_FetchFailure(t *testing.T) {
	t.Parallel()

	store := &mockReconcileStore{}
	tracker := &mockReconcileTracker{
		fetchErr: errors.New("connection timeout"),
	}
	params := defaultReconcileParams(t, store, tracker)
	params.StallTimeoutMS = 0

	state := NewState(5000, 4, nil, AgentTotals{})
	state.Running["ISSUE-1"] = &RunningEntry{
		Identifier: "ISSUE-1-ident",
		StartedAt:  reconcileBaseTime,
		Issue:      domain.Issue{State: "In Progress"},
	}
	state.Claimed["ISSUE-1"] = struct{}{}

	ReconcileRunningIssues(state, params)

	// Workers kept running on fetch failure.
	if _, ok := state.Running["ISSUE-1"]; !ok {
		t.Error("running entry removed despite fetch failure")
	}
	if state.Running["ISSUE-1"].PendingCleanup {
		t.Error("PendingCleanup set despite fetch failure")
	}
}

func TestReconcileTrackerState_TerminalSetsPendingCleanup(t *testing.T) {
	t.Parallel()

	store := &mockReconcileStore{}
	tracker := &mockReconcileTracker{
		states: map[string]string{"ISSUE-1": "Done"},
	}
	params := defaultReconcileParams(t, store, tracker)
	params.StallTimeoutMS = 0

	state := NewState(5000, 4, nil, AgentTotals{})
	cc := &cancelCounter{}
	state.Running["ISSUE-1"] = &RunningEntry{
		Identifier: "ISSUE-1-ident",
		StartedAt:  reconcileBaseTime,
		CancelFunc: cc.cancel,
		Issue:      domain.Issue{State: "In Progress"},
	}
	state.Claimed["ISSUE-1"] = struct{}{}
	// Pre-seed a retry to verify it is cancelled.
	state.RetryAttempts["ISSUE-1"] = &RetryEntry{
		IssueID:    "ISSUE-1",
		Identifier: "ISSUE-1-ident",
		Attempt:    1,
	}

	ReconcileRunningIssues(state, params)

	if cc.count != 1 {
		t.Errorf("CancelFunc called %d times, want 1", cc.count)
	}
	if !state.Running["ISSUE-1"].PendingCleanup {
		t.Error("PendingCleanup not set for terminal issue")
	}
	if got := state.Running["ISSUE-1"].ObservedTerminalState; got != "Done" {
		t.Errorf("ObservedTerminalState = %q, want %q", got, "Done")
	}
	// Retry cancelled for terminal issue.
	if _, ok := state.RetryAttempts["ISSUE-1"]; ok {
		t.Error("retry not cancelled for terminal issue")
	}
	// DeleteRetryEntry called.
	if len(store.deletedIssueID) != 1 {
		t.Fatalf("DeleteRetryEntry called %d times, want 1", len(store.deletedIssueID))
	}
	if store.deletedIssueID[0] != "ISSUE-1" {
		t.Errorf("deleted issue ID = %q, want %q", store.deletedIssueID[0], "ISSUE-1")
	}
}

func TestReconcileTrackerState_ActiveUpdatesState(t *testing.T) {
	t.Parallel()

	store := &mockReconcileStore{}
	tracker := &mockReconcileTracker{
		states: map[string]string{"ISSUE-1": "In Review"},
	}
	params := defaultReconcileParams(t, store, tracker)
	params.StallTimeoutMS = 0

	state := NewState(5000, 4, nil, AgentTotals{})
	state.Running["ISSUE-1"] = &RunningEntry{
		Identifier: "ISSUE-1-ident",
		StartedAt:  reconcileBaseTime,
		Issue:      domain.Issue{State: "In Progress"},
	}
	state.Claimed["ISSUE-1"] = struct{}{}

	ReconcileRunningIssues(state, params)

	if state.Running["ISSUE-1"].Issue.State != "In Review" {
		t.Errorf("Issue.State = %q, want %q", state.Running["ISSUE-1"].Issue.State, "In Review")
	}
	if state.Running["ISSUE-1"].PendingCleanup {
		t.Error("PendingCleanup set for active issue")
	}
}

func TestReconcileTrackerState_NonActiveNonTerminalCancelsWithoutCleanup(t *testing.T) {
	t.Parallel()

	store := &mockReconcileStore{}
	// "Backlog" is neither in ActiveStates nor TerminalStates.
	tracker := &mockReconcileTracker{
		states: map[string]string{"ISSUE-1": "Backlog"},
	}
	params := defaultReconcileParams(t, store, tracker)
	params.StallTimeoutMS = 0

	state := NewState(5000, 4, nil, AgentTotals{})
	cc := &cancelCounter{}
	state.Running["ISSUE-1"] = &RunningEntry{
		Identifier: "ISSUE-1-ident",
		StartedAt:  reconcileBaseTime,
		CancelFunc: cc.cancel,
		Issue:      domain.Issue{State: "In Progress"},
	}
	state.Claimed["ISSUE-1"] = struct{}{}

	ReconcileRunningIssues(state, params)

	if cc.count != 1 {
		t.Errorf("CancelFunc called %d times, want 1", cc.count)
	}
	if state.Running["ISSUE-1"].PendingCleanup {
		t.Error("PendingCleanup set for non-active non-terminal issue")
	}
	// DeleteRetryEntry called.
	if len(store.deletedIssueID) != 1 {
		t.Fatalf("DeleteRetryEntry called %d times, want 1", len(store.deletedIssueID))
	}
}

// TestReconcileTrackerState_NonActiveNonTerminalPreservesIncumbent verifies
// that when the retry slot is occupied, the non-active stop still cancels
// the worker and still counts the reconciliation action, but leaves the
// incumbent retry entry alone — neither CancelRetry nor
// Store.DeleteRetryEntry runs.
func TestReconcileTrackerState_NonActiveNonTerminalPreservesIncumbent(t *testing.T) {
	t.Parallel()

	store := &mockReconcileStore{}
	// "Backlog" is neither in ActiveStates nor TerminalStates.
	tracker := &mockReconcileTracker{
		states: map[string]string{"ISSUE-1": "Backlog"},
	}
	metrics := &spyMetrics{}
	params := defaultReconcileParams(t, store, tracker)
	params.StallTimeoutMS = 0
	params.Metrics = metrics

	state := NewState(5000, 4, nil, AgentTotals{})
	cc := &cancelCounter{}
	state.Running["ISSUE-1"] = &RunningEntry{
		Identifier:   "ISSUE-1-ident",
		StartedAt:    reconcileBaseTime,
		CancelFunc:   cc.cancel,
		Issue:        domain.Issue{State: "In Progress"},
		ReactionKind: "", // continuation entry
	}
	state.Claimed["ISSUE-1"] = struct{}{}
	state.RetryAttempts["ISSUE-1"] = &RetryEntry{
		IssueID:      "ISSUE-1",
		Identifier:   "ISSUE-1-ident",
		Attempt:      1,
		DueAtMS:      reconcileBaseTime.UnixMilli() + 60_000,
		ReactionKind: ReactionKindCI,
	}

	ReconcileRunningIssues(state, params)

	if cc.count != 1 {
		t.Errorf("CancelFunc called %d times, want 1", cc.count)
	}
	entry, ok := state.RetryAttempts["ISSUE-1"]
	if !ok {
		t.Fatal("RetryAttempts entry removed while the slot was occupied; want preserved")
	}
	if entry.Attempt != 1 {
		t.Errorf("RetryAttempts.Attempt = %d, want 1 (unchanged)", entry.Attempt)
	}
	if entry.DueAtMS != reconcileBaseTime.UnixMilli()+60_000 {
		t.Errorf("RetryAttempts.DueAtMS = %d, want unchanged", entry.DueAtMS)
	}
	if len(store.deletedIssueID) != 0 {
		t.Errorf("DeleteRetryEntry called %d times, want 0 (incumbent protected)", len(store.deletedIssueID))
	}
	if len(metrics.reconciliationActs) != 1 || metrics.reconciliationActs[0] != actionStop {
		t.Errorf("IncReconciliationActions calls = %v, want [%q]", metrics.reconciliationActs, actionStop)
	}
}

func TestReconcileTrackerState_OmittedIssueKeptRunning(t *testing.T) {
	t.Parallel()

	store := &mockReconcileStore{}
	// Tracker returns state for ISSUE-1 but not ISSUE-2.
	tracker := &mockReconcileTracker{
		states: map[string]string{"ISSUE-1": "In Progress"},
	}
	params := defaultReconcileParams(t, store, tracker)
	params.StallTimeoutMS = 0

	state := NewState(5000, 4, nil, AgentTotals{})
	cc := &cancelCounter{}
	state.Running["ISSUE-1"] = &RunningEntry{
		Identifier: "ISSUE-1-ident",
		StartedAt:  reconcileBaseTime,
		Issue:      domain.Issue{State: "In Progress"},
	}
	state.Claimed["ISSUE-1"] = struct{}{}

	state.Running["ISSUE-2"] = &RunningEntry{
		Identifier: "ISSUE-2-ident",
		StartedAt:  reconcileBaseTime,
		CancelFunc: cc.cancel,
		Issue:      domain.Issue{State: "In Progress"},
	}
	state.Claimed["ISSUE-2"] = struct{}{}

	ReconcileRunningIssues(state, params)

	// ISSUE-2 omitted from response → no action taken.
	if cc.count != 0 {
		t.Errorf("CancelFunc called for omitted issue %d times, want 0", cc.count)
	}
	if state.Running["ISSUE-2"].PendingCleanup {
		t.Error("PendingCleanup set for omitted issue")
	}
}

func TestReconcileTrackerState_TerminalCaseInsensitive(t *testing.T) {
	t.Parallel()

	store := &mockReconcileStore{}
	// "DONE" uppercase — should match "Done" in TerminalStates.
	tracker := &mockReconcileTracker{
		states: map[string]string{"ISSUE-1": "DONE"},
	}
	params := defaultReconcileParams(t, store, tracker)
	params.StallTimeoutMS = 0

	state := NewState(5000, 4, nil, AgentTotals{})
	cc := &cancelCounter{}
	state.Running["ISSUE-1"] = &RunningEntry{
		Identifier: "ISSUE-1-ident",
		StartedAt:  reconcileBaseTime,
		CancelFunc: cc.cancel,
		Issue:      domain.Issue{State: "In Progress"},
	}
	state.Claimed["ISSUE-1"] = struct{}{}

	ReconcileRunningIssues(state, params)

	if !state.Running["ISSUE-1"].PendingCleanup {
		t.Error("PendingCleanup not set for terminal issue with uppercase state")
	}
	if cc.count != 1 {
		t.Errorf("CancelFunc called %d times, want 1", cc.count)
	}
}

func TestReconcileTrackerState_DeleteRetryEntryError(t *testing.T) {
	t.Parallel()

	store := &mockReconcileStore{deleteRetryEntryErr: errors.New("db error")}
	tracker := &mockReconcileTracker{
		states: map[string]string{"ISSUE-1": "Done"},
	}
	params := defaultReconcileParams(t, store, tracker)
	params.StallTimeoutMS = 0

	state := NewState(5000, 4, nil, AgentTotals{})
	cc := &cancelCounter{}
	state.Running["ISSUE-1"] = &RunningEntry{
		Identifier: "ISSUE-1-ident",
		StartedAt:  reconcileBaseTime,
		CancelFunc: cc.cancel,
		Issue:      domain.Issue{State: "In Progress"},
	}
	state.Claimed["ISSUE-1"] = struct{}{}

	// Must not panic despite delete error.
	ReconcileRunningIssues(state, params)

	// Terminal processing still completed.
	if !state.Running["ISSUE-1"].PendingCleanup {
		t.Error("PendingCleanup not set despite delete error")
	}
	if cc.count != 1 {
		t.Errorf("CancelFunc called %d times, want 1", cc.count)
	}
	if len(store.deletedIssueID) != 1 {
		t.Errorf("DeleteRetryEntry called %d times, want 1", len(store.deletedIssueID))
	}
}

func TestReconcileTrackerState_RunningIssueReleasesReactionState(t *testing.T) {
	t.Parallel()

	cc := &cancelCounter{}
	state := stateWithTerminalReleaseFixture(t, cc)

	store := &mockReconcileStore{}
	tracker := &mockReconcileTracker{states: map[string]string{terminalReleaseIssueID: "Done"}}
	params := defaultReconcileParams(t, store, tracker)
	params.StallTimeoutMS = 0

	ReconcileRunningIssues(state, params)

	for _, kind := range terminalReleaseFixtureKinds {
		key := ReactionKey(terminalReleaseIssueID, kind)
		if _, ok := state.PendingReactions[key]; ok {
			t.Errorf("PendingReactions[%q] present after terminal release; want removed", key)
		}
		if _, ok := state.ReactionAttempts[key]; ok {
			t.Errorf("ReactionAttempts[%q] present after terminal release; want removed", key)
		}
	}
	if _, ok := state.Claimed[terminalReleaseIssueID]; ok {
		t.Error("Claimed entry present after terminal release; want removed")
	}
	if _, ok := state.RetryAttempts[terminalReleaseIssueID]; ok {
		t.Error("RetryAttempts entry present after terminal release; want removed")
	}
	if len(store.deletedIssueID) != 1 || store.deletedIssueID[0] != terminalReleaseIssueID {
		t.Errorf("DeleteRetryEntry calls = %v, want exactly one call for %q", store.deletedIssueID, terminalReleaseIssueID)
	}
	if !state.Running[terminalReleaseIssueID].PendingCleanup {
		t.Error("PendingCleanup not set for terminal running issue")
	}
}

func TestReconcileTrackerState_PendingOnlyIssueReleasesReactionState(t *testing.T) {
	t.Parallel()

	issueID := "REL-2"
	store := &mockReconcileStore{}
	tracker := &mockReconcileTracker{states: map[string]string{issueID: "Done"}}

	log, buf := logCapture()

	params := defaultReconcileParams(t, store, tracker)
	params.StallTimeoutMS = 0
	params.Logger = log

	state := NewState(5000, 4, nil, AgentTotals{})
	key := ReactionKey(issueID, ReactionKindCI)
	state.PendingReactions[key] = &PendingReaction{
		IssueID:    issueID,
		Identifier: issueID + "-ident",
		Kind:       ReactionKindCI,
		CreatedAt:  reconcileBaseTime,
	}
	state.ReactionAttempts[key] = 1
	state.Claimed[issueID] = struct{}{}

	ReconcileRunningIssues(state, params)

	if tracker.calls != 1 {
		t.Fatalf("FetchIssueStatesByIDs calls = %d, want 1", tracker.calls)
	}
	if !slices.Contains(tracker.calledWith, issueID) {
		t.Errorf("FetchIssueStatesByIDs called with %v, want it to contain %q", tracker.calledWith, issueID)
	}
	if _, ok := state.PendingReactions[key]; ok {
		t.Error("PendingReactions entry present after terminal release; want removed")
	}
	if _, ok := state.ReactionAttempts[key]; ok {
		t.Error("ReactionAttempts entry present after terminal release; want removed")
	}
	if _, ok := state.Claimed[issueID]; ok {
		t.Error("Claimed entry present after terminal release; want removed")
	}
	if len(store.deletedIssueID) != 1 || store.deletedIssueID[0] != issueID {
		t.Errorf("DeleteRetryEntry calls = %v, want exactly one call for %q", store.deletedIssueID, issueID)
	}

	out := buf.String()
	if !strings.Contains(out, "released reaction state for terminal issue") {
		t.Fatalf("log output = %q, want the terminal-release message", out)
	}
	if !strings.Contains(out, "issue_identifier="+issueID+"-ident") {
		t.Errorf("log output = %q, want issue_identifier=%s-ident", out, issueID)
	}
}

func TestTrackerObservationIDs_DeduplicatedUnion(t *testing.T) {
	t.Parallel()

	state := NewState(5000, 4, nil, AgentTotals{})

	state.Running["I1"] = &RunningEntry{Identifier: "I1-ident"}
	state.PendingReactions[ReactionKey("I1", ReactionKindCI)] = &PendingReaction{IssueID: "I1", Identifier: "I1-ident", Kind: ReactionKindCI}
	state.PendingReactions[ReactionKey("I1", ReactionKindReview)] = &PendingReaction{IssueID: "I1", Identifier: "I1-ident", Kind: ReactionKindReview}

	state.PendingReactions[ReactionKey("I2", ReactionKindCI)] = &PendingReaction{IssueID: "I2", Identifier: "I2-ident", Kind: ReactionKindCI}
	state.PendingReactions[ReactionKey("I2", ReactionKindLabelReview)] = &PendingReaction{IssueID: "I2", Identifier: "I2-ident", Kind: ReactionKindLabelReview}

	state.Running["I3"] = &RunningEntry{Identifier: "I3-ident"}

	got := trackerObservationIDs(state)
	slices.Sort(got)

	want := []string{"I1", "I2", "I3"}
	if !slices.Equal(got, want) {
		t.Errorf("trackerObservationIDs(state) sorted = %v, want %v", got, want)
	}
}

func TestReconcileTrackerState_FetchFailureReleasesNothing(t *testing.T) {
	t.Parallel()

	cc := &cancelCounter{}
	state := stateWithTerminalReleaseFixture(t, cc)

	store := &mockReconcileStore{}
	tracker := &mockReconcileTracker{fetchErr: errors.New("connection timeout")}
	params := defaultReconcileParams(t, store, tracker)
	params.StallTimeoutMS = 0

	ReconcileRunningIssues(state, params)

	for _, kind := range terminalReleaseFixtureKinds {
		key := ReactionKey(terminalReleaseIssueID, kind)
		if _, ok := state.PendingReactions[key]; !ok {
			t.Errorf("PendingReactions[%q] missing after fetch failure; want unchanged", key)
		}
		if state.ReactionAttempts[key] != 2 {
			t.Errorf("ReactionAttempts[%q] = %d, want 2 (unchanged)", key, state.ReactionAttempts[key])
		}
	}
	if _, ok := state.Claimed[terminalReleaseIssueID]; !ok {
		t.Error("Claimed entry missing after fetch failure; want unchanged")
	}
	if _, ok := state.RetryAttempts[terminalReleaseIssueID]; !ok {
		t.Error("RetryAttempts entry missing after fetch failure; want unchanged")
	}
	if len(store.deletedIssueID) != 0 {
		t.Errorf("DeleteRetryEntry calls = %d, want 0", len(store.deletedIssueID))
	}
}

func TestReconcileTrackerState_NonTerminalPendingOnlyReleasesNothing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state string
	}{
		{"active state", "In Progress"},
		{"handoff state", "In Review"},
		{"state named in no list", "Backlog"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issueID := "REL-NONTERM"
			store := &mockReconcileStore{}
			tracker := &mockReconcileTracker{states: map[string]string{issueID: tt.state}}
			params := defaultReconcileParams(t, store, tracker)
			params.StallTimeoutMS = 0
			params.HandoffState = "In Review"

			state := NewState(5000, 4, nil, AgentTotals{})
			key := ReactionKey(issueID, ReactionKindCI)
			state.PendingReactions[key] = &PendingReaction{
				IssueID:    issueID,
				Identifier: issueID + "-ident",
				Kind:       ReactionKindCI,
				CreatedAt:  reconcileBaseTime,
			}
			state.ReactionAttempts[key] = 1
			state.Claimed[issueID] = struct{}{}

			ReconcileRunningIssues(state, params)

			if _, ok := state.PendingReactions[key]; !ok {
				t.Error("PendingReactions entry removed for a non-terminal pending-only issue; want unchanged")
			}
			if state.ReactionAttempts[key] != 1 {
				t.Errorf("ReactionAttempts[%q] = %d, want 1 (unchanged)", key, state.ReactionAttempts[key])
			}
			if _, ok := state.Claimed[issueID]; !ok {
				t.Error("Claimed entry removed for a non-terminal pending-only issue; want unchanged")
			}
		})
	}
}

func TestReleaseTerminalIssueState_IssueIsolation(t *testing.T) {
	t.Parallel()

	state := NewState(5000, 4, nil, AgentTotals{})

	keyI := ReactionKey("I", ReactionKindCI)
	state.PendingReactions[keyI] = &PendingReaction{IssueID: "I", Identifier: "I-ident", Kind: ReactionKindCI, CreatedAt: reconcileBaseTime}
	state.ReactionAttempts[keyI] = 3
	state.Claimed["I"] = struct{}{}

	keyI2 := ReactionKey("I2", ReactionKindCI)
	state.PendingReactions[keyI2] = &PendingReaction{IssueID: "I2", Identifier: "I2-ident", Kind: ReactionKindCI, CreatedAt: reconcileBaseTime}
	state.ReactionAttempts[keyI2] = 5
	state.Claimed["I2"] = struct{}{}

	store := &mockReconcileStore{}
	releaseTerminalIssueState(context.Background(), state, store, "I", discardLogger())

	if _, ok := state.PendingReactions[keyI]; ok {
		t.Error("PendingReactions[I] present after its own release; want removed")
	}
	if _, ok := state.PendingReactions[keyI2]; !ok {
		t.Error("PendingReactions[I2] removed by the release of I; want untouched")
	}
	if state.ReactionAttempts[keyI2] != 5 {
		t.Errorf("ReactionAttempts[I2] = %d, want 5 (untouched)", state.ReactionAttempts[keyI2])
	}
	if _, ok := state.Claimed["I2"]; !ok {
		t.Error("Claimed[I2] removed by the release of I; want untouched")
	}
}

func TestReleaseTerminalIssueState_NoSideEffects(t *testing.T) {
	t.Parallel()

	cc := &cancelCounter{}
	state := stateWithTerminalReleaseFixture(t, cc)

	store := &mockReconcileStore{}
	tracker := &mockReconcileTracker{states: map[string]string{terminalReleaseIssueID: "Done"}}
	params := defaultReconcileParams(t, store, tracker)
	params.StallTimeoutMS = 0
	params.SCMAdapter = panicOnAnySCMAdapter{}
	params.CIProvider = panicOnAnyCIProvider{}
	params.LabelReviewReactionConfigured = true

	ReconcileRunningIssues(state, params)

	if store.upsertFingerprintCalls != 0 || store.getFingerprintCalls != 0 ||
		store.markDispatchedCalls != 0 || store.deleteFingerprintCalls != 0 {
		t.Errorf("fingerprint store calls = upsert:%d get:%d mark:%d delete:%d, want all 0",
			store.upsertFingerprintCalls, store.getFingerprintCalls, store.markDispatchedCalls, store.deleteFingerprintCalls)
	}
	if len(store.savedEntries) != 0 {
		t.Errorf("SaveRetryEntry calls = %d, want 0", len(store.savedEntries))
	}
	if len(store.deletedIssueID) != 1 {
		t.Errorf("DeleteRetryEntry calls = %d, want 1", len(store.deletedIssueID))
	}
}

func TestReconcileTrackerState_NilTrackerAdapterPendingOnly(t *testing.T) {
	t.Parallel()

	issueID := "REL-NIL"
	state := NewState(5000, 4, nil, AgentTotals{})
	key := ReactionKey(issueID, ReactionKindCI)
	state.PendingReactions[key] = &PendingReaction{
		IssueID:    issueID,
		Identifier: issueID + "-ident",
		Kind:       ReactionKindCI,
		CreatedAt:  reconcileBaseTime,
	}
	state.ReactionAttempts[key] = 1
	state.Claimed[issueID] = struct{}{}

	store := &mockReconcileStore{}
	params := ReconcileParams{
		TrackerAdapter: nil,
		ActiveStates:   []string{"In Progress", "In Review"},
		TerminalStates: []string{"Done", "Closed"},
		Store:          store,
		OnRetryFire:    noopRetryFire,
		NowFunc:        func() time.Time { return reconcileBaseTime },
		Ctx:            context.Background(),
		Logger:         discardLogger(),
	}

	reconcileTrackerState(state, params, discardLogger(), context.Background(), &domain.NoopMetrics{})

	if _, ok := state.PendingReactions[key]; !ok {
		t.Error("PendingReactions entry removed despite nil TrackerAdapter; want unchanged")
	}
	if state.ReactionAttempts[key] != 1 {
		t.Errorf("ReactionAttempts[%q] = %d, want 1 (unchanged)", key, state.ReactionAttempts[key])
	}
	if _, ok := state.Claimed[issueID]; !ok {
		t.Error("Claimed entry removed despite nil TrackerAdapter; want unchanged")
	}
	if len(store.deletedIssueID) != 0 {
		t.Errorf("DeleteRetryEntry calls = %d, want 0", len(store.deletedIssueID))
	}
}

// --- Combined: stall + tracker state ---

func TestReconcile_StalledAndTerminal(t *testing.T) {
	t.Parallel()

	store := &mockReconcileStore{}
	tracker := &mockReconcileTracker{
		states: map[string]string{
			"STALE-1":    "In Progress", // active, stalled
			"TERMINAL-1": "Done",        // terminal, not stalled
		},
	}
	params := defaultReconcileParams(t, store, tracker)
	params.StallTimeoutMS = 60_000

	state := NewState(5000, 4, nil, AgentTotals{})
	ccStale := &cancelCounter{}
	ccTerminal := &cancelCounter{}

	state.Running["STALE-1"] = &RunningEntry{
		Identifier: "STALE-1-ident",
		StartedAt:  reconcileBaseTime.Add(-120 * time.Second),
		CancelFunc: ccStale.cancel,
		Issue:      domain.Issue{State: "In Progress"},
	}
	state.Claimed["STALE-1"] = struct{}{}

	state.Running["TERMINAL-1"] = &RunningEntry{
		Identifier: "TERMINAL-1-ident",
		StartedAt:  reconcileBaseTime.Add(-10 * time.Second), // not stalled
		CancelFunc: ccTerminal.cancel,
		Issue:      domain.Issue{State: "In Progress"},
	}
	state.Claimed["TERMINAL-1"] = struct{}{}

	ReconcileRunningIssues(state, params)

	// STALE-1: cancelled by stall detection (Part A).
	if ccStale.count < 1 {
		t.Error("stale entry CancelFunc not called")
	}
	if _, ok := state.RetryAttempts["STALE-1"]; !ok {
		t.Error("retry not scheduled for stalled entry")
	}

	// TERMINAL-1: cancelled by tracker state refresh (Part B), cleanup marked.
	if ccTerminal.count != 1 {
		t.Errorf("terminal CancelFunc called %d times, want 1", ccTerminal.count)
	}
	if !state.Running["TERMINAL-1"].PendingCleanup {
		t.Error("PendingCleanup not set for terminal entry")
	}
}

func TestReconcile_SameIssueStalledAndTerminal(t *testing.T) {
	t.Parallel()

	store := &mockReconcileStore{}
	// Same issue is stalled (Part A) AND terminal (Part B).
	tracker := &mockReconcileTracker{
		states: map[string]string{"ISSUE-1": "Done"},
	}
	params := defaultReconcileParams(t, store, tracker)
	params.StallTimeoutMS = 60_000

	state := NewState(5000, 4, nil, AgentTotals{})
	cc := &cancelCounter{}
	state.Running["ISSUE-1"] = &RunningEntry{
		Identifier: "ISSUE-1-ident",
		StartedAt:  reconcileBaseTime.Add(-120 * time.Second), // stalled: 120s > 60s
		CancelFunc: cc.cancel,
		Issue:      domain.Issue{State: "In Progress"},
	}
	state.Claimed["ISSUE-1"] = struct{}{}

	ReconcileRunningIssues(state, params)

	// CancelFunc called at least once (Part A cancels, Part B cancels again - idempotent).
	if cc.count < 1 {
		t.Error("CancelFunc never called")
	}

	// Part A scheduled a retry AND persisted it (SaveRetryEntry).
	if len(store.savedEntries) != 1 {
		t.Errorf("SaveRetryEntry called %d times, want 1 (from Part A)", len(store.savedEntries))
	}

	// Part B then cancelled that retry AND deleted it (DeleteRetryEntry).
	if len(store.deletedIssueID) != 1 {
		t.Fatalf("DeleteRetryEntry called %d times, want 1 (from Part B)", len(store.deletedIssueID))
	}
	if store.deletedIssueID[0] != "ISSUE-1" {
		t.Errorf("deleted issue ID = %q, want %q", store.deletedIssueID[0], "ISSUE-1")
	}

	// Final state: retry removed by Part B, PendingCleanup set.
	if _, ok := state.RetryAttempts["ISSUE-1"]; ok {
		t.Error("retry still present after Part B should have cancelled it")
	}
	if !state.Running["ISSUE-1"].PendingCleanup {
		t.Error("PendingCleanup not set for stalled+terminal issue")
	}
}

// --- Stall-retry guard tests ---

func TestReconcileStalled_SecondTickSkipsReschedule(t *testing.T) {
	t.Parallel()

	store := &mockReconcileStore{}
	tracker := &mockReconcileTracker{
		states: map[string]string{"ISSUE-1": "In Progress"},
	}
	params := defaultReconcileParams(t, store, tracker)
	params.StallTimeoutMS = 60_000

	state := NewState(5000, 4, nil, AgentTotals{})
	cc := &cancelCounter{}
	state.Running["ISSUE-1"] = &RunningEntry{
		Identifier: "ISSUE-1-ident",
		StartedAt:  reconcileBaseTime.Add(-120 * time.Second), // stalled
		CancelFunc: cc.cancel,
		Issue:      domain.Issue{State: "In Progress"},
	}
	state.Claimed["ISSUE-1"] = struct{}{}

	// First tick: should schedule retry.
	ReconcileRunningIssues(state, params)

	firstEntry, ok := state.RetryAttempts["ISSUE-1"]
	if !ok {
		t.Fatal("retry not scheduled after first tick")
	}
	firstDueAt := firstEntry.DueAtMS
	firstAttempt := firstEntry.Attempt
	if firstAttempt != 1 {
		t.Fatalf("first retry Attempt = %d, want 1", firstAttempt)
	}
	if len(store.savedEntries) != 1 {
		t.Fatalf("SaveRetryEntry called %d times after first tick, want 1", len(store.savedEntries))
	}

	// Second tick: same entry still stalled but retry already present.
	// Guard should skip rescheduling — DueAtMS and save count unchanged.
	ReconcileRunningIssues(state, params)

	secondEntry, ok := state.RetryAttempts["ISSUE-1"]
	if !ok {
		t.Fatal("retry entry removed after second tick")
	}
	if secondEntry.DueAtMS != firstDueAt {
		t.Errorf("DueAtMS changed from %d to %d after second tick, want unchanged", firstDueAt, secondEntry.DueAtMS)
	}
	if secondEntry.Attempt != firstAttempt {
		t.Errorf("Attempt changed from %d to %d after second tick, want unchanged", firstAttempt, secondEntry.Attempt)
	}
	// No additional SaveRetryEntry call.
	if len(store.savedEntries) != 1 {
		t.Errorf("SaveRetryEntry called %d times after second tick, want 1 (no additional call)", len(store.savedEntries))
	}
	// CancelFunc called on both ticks (stall detected both times).
	if cc.count != 2 {
		t.Errorf("CancelFunc called %d times after two ticks, want 2", cc.count)
	}
}

// --- Log spy ---

// logRecord captures the level and message of a single slog record.
type logRecord struct {
	Level   slog.Level
	Message string
}

// recordHandler is a slog.Handler that captures records for test assertions.
type recordHandler struct {
	records []logRecord
}

func (h *recordHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *recordHandler) Handle(_ context.Context, r slog.Record) error {
	h.records = append(h.records, logRecord{Level: r.Level, Message: r.Message})
	return nil
}
func (h *recordHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *recordHandler) WithGroup(_ string) slog.Handler      { return h }

// countByMessage returns the number of captured records with the given message.
func (h *recordHandler) countByMessage(msg string) int {
	n := 0
	for _, r := range h.records {
		if r.Message == msg {
			n++
		}
	}
	return n
}

// --- PendingCleanup idempotency tests ---

func TestReconcileTerminal_PendingCleanupSkipsSecondTick(t *testing.T) {
	t.Parallel()

	store := &mockReconcileStore{}
	tracker := &mockReconcileTracker{
		states: map[string]string{"ISSUE-1": "Done"},
	}
	params := defaultReconcileParams(t, store, tracker)
	params.StallTimeoutMS = 0 // disable stall detection for Part B isolation

	state := NewState(5000, 4, nil, AgentTotals{})
	cc := &cancelCounter{}
	state.Running["ISSUE-1"] = &RunningEntry{
		Identifier: "ISSUE-1-ident",
		StartedAt:  reconcileBaseTime,
		CancelFunc: cc.cancel,
		Issue:      domain.Issue{State: "In Progress"},
	}
	state.Claimed["ISSUE-1"] = struct{}{}

	// First tick: marks terminal, sets PendingCleanup.
	ReconcileRunningIssues(state, params)

	if !state.Running["ISSUE-1"].PendingCleanup {
		t.Fatal("PendingCleanup not set after first tick")
	}
	if cc.count != 1 {
		t.Fatalf("CancelFunc called %d times after first tick, want 1", cc.count)
	}
	firstDeleteCount := len(store.deletedIssueID)
	if firstDeleteCount != 1 {
		t.Fatalf("DeleteRetryEntry called %d times after first tick, want 1", firstDeleteCount)
	}

	// Second tick: PendingCleanup already set — should be a no-op.
	ReconcileRunningIssues(state, params)

	if cc.count != 1 {
		t.Errorf("CancelFunc called %d times after second tick, want 1 (no additional call)", cc.count)
	}
	if len(store.deletedIssueID) != firstDeleteCount {
		t.Errorf("DeleteRetryEntry called %d times after second tick, want %d (no additional call)", len(store.deletedIssueID), firstDeleteCount)
	}
	// PendingCleanup remains set.
	if !state.Running["ISSUE-1"].PendingCleanup {
		t.Error("PendingCleanup cleared after second tick, want still set")
	}
}

func TestReconcileTerminal_PendingCleanupSkipsLogAndRetryDeletion(t *testing.T) {
	t.Parallel()

	store := &mockReconcileStore{}
	tracker := &mockReconcileTracker{
		states: map[string]string{"ISSUE-1": "Done"},
	}

	handler := &recordHandler{}
	params := ReconcileParams{
		TrackerAdapter:    tracker,
		ActiveStates:      []string{"In Progress", "In Review"},
		TerminalStates:    []string{"Done", "Closed"},
		MaxRetryBackoffMS: 300_000,
		Store:             store,
		OnRetryFire:       noopRetryFire,
		NowFunc:           func() time.Time { return reconcileBaseTime },
		Ctx:               context.Background(),
		Logger:            slog.New(handler),
	}

	state := NewState(5000, 4, nil, AgentTotals{})
	cc := &cancelCounter{}
	// Pre-set PendingCleanup to simulate prior terminal detection.
	state.Running["ISSUE-1"] = &RunningEntry{
		Identifier:     "ISSUE-1-ident",
		StartedAt:      reconcileBaseTime,
		CancelFunc:     cc.cancel,
		Issue:          domain.Issue{State: "Done"},
		PendingCleanup: true,
	}
	state.Claimed["ISSUE-1"] = struct{}{}

	ReconcileRunningIssues(state, params)

	if cc.count != 0 {
		t.Errorf("CancelFunc called %d times, want 0 (PendingCleanup already set)", cc.count)
	}
	if len(store.deletedIssueID) != 0 {
		t.Errorf("DeleteRetryEntry called %d times, want 0", len(store.deletedIssueID))
	}
	if handler.countByMessage("stopping worker for terminal issue") != 0 {
		t.Error("Info log emitted for already-pending-cleanup issue, want 0")
	}
}

// --- Stall Warn log emitted every stalled tick test ---

func TestReconcileStalled_WarnLogEveryStalledTick(t *testing.T) {
	t.Parallel()

	store := &mockReconcileStore{}
	tracker := &mockReconcileTracker{
		states: map[string]string{"ISSUE-1": "In Progress"},
	}

	handler := &recordHandler{}
	params := defaultReconcileParams(t, store, tracker)
	params.StallTimeoutMS = 60_000
	params.Logger = slog.New(handler)

	state := NewState(5000, 4, nil, AgentTotals{})
	cc := &cancelCounter{}
	state.Running["ISSUE-1"] = &RunningEntry{
		Identifier: "ISSUE-1-ident",
		StartedAt:  reconcileBaseTime.Add(-120 * time.Second), // stalled
		CancelFunc: cc.cancel,
		Issue:      domain.Issue{State: "In Progress"},
	}
	state.Claimed["ISSUE-1"] = struct{}{}

	// First tick: schedules the stall retry and emits the Warn.
	ReconcileRunningIssues(state, params)

	warnCount := handler.countByMessage("stall detected, cancelling worker")
	if warnCount != 1 {
		t.Fatalf("Warn('stall detected, cancelling worker') emitted %d times after first tick, want 1", warnCount)
	}

	// Second tick: the stall retry now occupies the slot, so the pass
	// defers instead of rescheduling, but the cancellation Warn still
	// fires because the worker cancellation is unconditional.
	ReconcileRunningIssues(state, params)

	warnCount = handler.countByMessage("stall detected, cancelling worker")
	if warnCount != 2 {
		t.Errorf("Warn('stall detected, cancelling worker') emitted %d times after second tick, want 2 (fires on every stalled tick)", warnCount)
	}

	// The second tick's deferral is reported through the shared
	// retry-slot Debug record instead of a stall-specific message.
	deferCount := handler.countByMessage("retry slot occupied, deferring")
	if deferCount != 1 {
		t.Errorf("Debug('retry slot occupied, deferring') emitted %d times, want 1", deferCount)
	}
}

// TestReconcileStalled_DeferralSkipsMutations verifies that a stall
// deferral (the slot already occupied by the first tick's own stall
// retry) still cancels the worker unconditionally but performs none of
// the mutations the dispatch arm would: no additional ScheduleRetry (via
// an unchanged DueAtMS and Attempt), no Store.SaveRetryEntry, no
// Store.DeleteRetryEntry, and no IncRetries(triggerStall).
func TestReconcileStalled_DeferralSkipsMutations(t *testing.T) {
	t.Parallel()

	store := &mockReconcileStore{}
	tracker := &mockReconcileTracker{
		states: map[string]string{"ISSUE-1": "In Progress"},
	}
	metrics := &spyMetrics{}
	params := defaultReconcileParams(t, store, tracker)
	params.StallTimeoutMS = 60_000
	params.Metrics = metrics

	state := NewState(5000, 4, nil, AgentTotals{})
	cc := &cancelCounter{}
	state.Running["ISSUE-1"] = &RunningEntry{
		Identifier: "ISSUE-1-ident",
		StartedAt:  reconcileBaseTime.Add(-120 * time.Second), // stalled
		CancelFunc: cc.cancel,
		Issue:      domain.Issue{State: "In Progress"},
	}
	state.Claimed["ISSUE-1"] = struct{}{}

	// First tick: schedules the stall retry, occupying the slot.
	ReconcileRunningIssues(state, params)

	firstEntry, ok := state.RetryAttempts["ISSUE-1"]
	if !ok {
		t.Fatal("retry not scheduled after first tick")
	}
	firstDueAt := firstEntry.DueAtMS
	firstAttempt := firstEntry.Attempt
	firstStallTriggers := len(metrics.retries)

	// Second tick: still stalled, but the slot is occupied by the
	// first tick's own retry. The pass must defer, not reschedule.
	ReconcileRunningIssues(state, params)

	if cc.count != 2 {
		t.Errorf("CancelFunc called %d times, want 2 (unconditional on every stalled tick)", cc.count)
	}
	secondEntry, ok := state.RetryAttempts["ISSUE-1"]
	if !ok {
		t.Fatal("retry entry removed after deferring tick")
	}
	if secondEntry.DueAtMS != firstDueAt {
		t.Errorf("DueAtMS changed from %d to %d on a defer, want unchanged", firstDueAt, secondEntry.DueAtMS)
	}
	if secondEntry.Attempt != firstAttempt {
		t.Errorf("Attempt changed from %d to %d on a defer, want unchanged", firstAttempt, secondEntry.Attempt)
	}
	if len(store.savedEntries) != 1 {
		t.Errorf("SaveRetryEntry called %d times total, want 1 (none on the deferring tick)", len(store.savedEntries))
	}
	if len(store.deletedIssueID) != 0 {
		t.Errorf("DeleteRetryEntry called %d times, want 0", len(store.deletedIssueID))
	}
	if len(metrics.retries) != firstStallTriggers {
		t.Errorf("IncRetries called %d additional time(s) on the deferring tick, want 0", len(metrics.retries)-firstStallTriggers)
	}
}

// --- SweepWorkspaces helpers ---

// defaultSweepParams returns SweepWorkspacesParams with the given
// root and tracker. TerminalStates, Ctx, Logger, and Metrics use
// test-suitable defaults.
func defaultSweepParams(t *testing.T, root string, tracker *sweepTracker) SweepWorkspacesParams {
	t.Helper()
	return SweepWorkspacesParams{
		WorkspaceRoot:  root,
		TrackerAdapter: tracker,
		TerminalStates: []string{"Done"},
		Ctx:            context.Background(),
		Logger:         discardLogger(),
		Metrics:        &domain.NoopMetrics{},
	}
}

// mustMkdirSweep creates a directory under path or fatals the test.
func mustMkdirSweep(t *testing.T, path string) {
	t.Helper()
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("os.Mkdir(%q): %v", path, err)
	}
}

// assertSweepDirExists fails if the directory at path does not exist.
func assertSweepDirExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("directory %q should exist: %v", path, err)
	}
}

// assertSweepDirRemoved fails if path still exists on disk.
func assertSweepDirRemoved(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("path %q should have been removed, want ErrNotExist", path)
	}
}

// --- TestSweepWorkspaces ---

func TestSweepWorkspaces(t *testing.T) {
	t.Parallel()

	t.Run("EmptyWorkspaceRoot", func(t *testing.T) {
		t.Parallel()

		tracker := &sweepTracker{}
		state := NewState(5000, 4, nil, AgentTotals{})
		SweepWorkspaces(state, SweepWorkspacesParams{
			WorkspaceRoot:  "",
			TrackerAdapter: tracker,
			TerminalStates: []string{"Done"},
			Ctx:            context.Background(),
			Logger:         discardLogger(),
			Metrics:        &domain.NoopMetrics{},
		})

		if tracker.calledWith != nil {
			t.Errorf("FetchIssueStatesByIdentifiers called with %v, want not called", tracker.calledWith)
		}
	})

	t.Run("NoWorkspaceDirectories", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		tracker := &sweepTracker{}
		state := NewState(5000, 4, nil, AgentTotals{})

		SweepWorkspaces(state, defaultSweepParams(t, tmpDir, tracker))

		if tracker.calledWith != nil {
			t.Errorf("FetchIssueStatesByIdentifiers called with %v, want not called", tracker.calledWith)
		}
	})

	t.Run("AllRunning", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		mustMkdirSweep(t, filepath.Join(tmpDir, "PROJ-1"))

		tracker := &sweepTracker{}
		state := NewState(5000, 4, nil, AgentTotals{})
		state.Running["id1"] = &RunningEntry{Identifier: "PROJ-1"}

		SweepWorkspaces(state, defaultSweepParams(t, tmpDir, tracker))

		if tracker.calledWith != nil {
			t.Errorf("FetchIssueStatesByIdentifiers called with %v, want not called", tracker.calledWith)
		}
		assertSweepDirExists(t, filepath.Join(tmpDir, "PROJ-1"))
	})

	t.Run("AllRetryAttempts", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		mustMkdirSweep(t, filepath.Join(tmpDir, "PROJ-2"))

		tracker := &sweepTracker{}
		state := NewState(5000, 4, nil, AgentTotals{})
		state.RetryAttempts["id2"] = &RetryEntry{Identifier: "PROJ-2"}

		SweepWorkspaces(state, defaultSweepParams(t, tmpDir, tracker))

		if tracker.calledWith != nil {
			t.Errorf("FetchIssueStatesByIdentifiers called with %v, want not called", tracker.calledWith)
		}
		assertSweepDirExists(t, filepath.Join(tmpDir, "PROJ-2"))
	})

	t.Run("AllPendingReactions", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		mustMkdirSweep(t, filepath.Join(tmpDir, "PROJ-3"))

		tracker := &sweepTracker{}
		state := NewState(5000, 4, nil, AgentTotals{})
		state.PendingReactions["id3:ci"] = &PendingReaction{Identifier: "PROJ-3"}

		SweepWorkspaces(state, defaultSweepParams(t, tmpDir, tracker))

		if tracker.calledWith != nil {
			t.Errorf("FetchIssueStatesByIdentifiers called with %v, want not called", tracker.calledWith)
		}
		assertSweepDirExists(t, filepath.Join(tmpDir, "PROJ-3"))
	})

	t.Run("SanitizeKeyError_Running", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		mustMkdirSweep(t, filepath.Join(tmpDir, "PROJ-4"))

		// Empty identifier fails SanitizeKey; the running entry is
		// skipped when building inFlightKeys, leaving PROJ-4 unclaimed.
		tracker := &sweepTracker{
			statesByKey: map[string]string{"PROJ-4": "Done"},
		}
		state := NewState(5000, 4, nil, AgentTotals{})
		state.Running["id1"] = &RunningEntry{Identifier: ""}

		SweepWorkspaces(state, defaultSweepParams(t, tmpDir, tracker))

		if tracker.calledWith == nil {
			t.Fatal("FetchIssueStatesByIdentifiers not called, want called with [PROJ-4]")
		}
		got := append([]string(nil), tracker.calledWith...)
		sort.Strings(got)
		want := []string{"PROJ-4"}
		if !slices.Equal(got, want) {
			t.Errorf("FetchIssueStatesByIdentifiers received %v, want %v", got, want)
		}
		assertSweepDirRemoved(t, filepath.Join(tmpDir, "PROJ-4"))
	})

	t.Run("MixedRunningAndTerminal", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		mustMkdirSweep(t, filepath.Join(tmpDir, "PROJ-5"))
		mustMkdirSweep(t, filepath.Join(tmpDir, "PROJ-6"))

		tracker := &sweepTracker{
			statesByKey: map[string]string{"PROJ-6": "Done"},
		}
		state := NewState(5000, 4, nil, AgentTotals{})
		state.Running["id5"] = &RunningEntry{Identifier: "PROJ-5"}

		SweepWorkspaces(state, defaultSweepParams(t, tmpDir, tracker))

		assertSweepDirExists(t, filepath.Join(tmpDir, "PROJ-5"))
		assertSweepDirRemoved(t, filepath.Join(tmpDir, "PROJ-6"))
	})

	t.Run("MixedTerminalAndActive", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		mustMkdirSweep(t, filepath.Join(tmpDir, "PROJ-7"))
		mustMkdirSweep(t, filepath.Join(tmpDir, "PROJ-8"))

		tracker := &sweepTracker{
			statesByKey: map[string]string{
				"PROJ-7": "Done",
				"PROJ-8": "In Progress",
			},
		}
		state := NewState(5000, 4, nil, AgentTotals{})

		SweepWorkspaces(state, defaultSweepParams(t, tmpDir, tracker))

		assertSweepDirRemoved(t, filepath.Join(tmpDir, "PROJ-7"))
		assertSweepDirExists(t, filepath.Join(tmpDir, "PROJ-8"))
	})

	t.Run("TrackerFetchFailure", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		mustMkdirSweep(t, filepath.Join(tmpDir, "PROJ-9"))

		tracker := &sweepTracker{
			fetchErr: errors.New("tracker unavailable"),
		}
		state := NewState(5000, 4, nil, AgentTotals{})

		SweepWorkspaces(state, defaultSweepParams(t, tmpDir, tracker))

		assertSweepDirExists(t, filepath.Join(tmpDir, "PROJ-9"))
	})

	t.Run("UnclaimedKeyMissingFromTracker", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		mustMkdirSweep(t, filepath.Join(tmpDir, "PROJ-9"))

		// Tracker returns an empty map; PROJ-9 is absent from it.
		tracker := &sweepTracker{
			statesByKey: map[string]string{},
		}
		state := NewState(5000, 4, nil, AgentTotals{})

		SweepWorkspaces(state, defaultSweepParams(t, tmpDir, tracker))

		assertSweepDirExists(t, filepath.Join(tmpDir, "PROJ-9"))
	})

	t.Run("MetricEmitted", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		mustMkdirSweep(t, filepath.Join(tmpDir, "PROJ-10"))

		tracker := &sweepTracker{
			statesByKey: map[string]string{"PROJ-10": "Done"},
		}
		state := NewState(5000, 4, nil, AgentTotals{})

		spy := &spyMetrics{}
		params := defaultSweepParams(t, tmpDir, tracker)
		params.Metrics = spy

		SweepWorkspaces(state, params)

		spy.mu.Lock()
		acts := append([]string(nil), spy.reconciliationActs...)
		spy.mu.Unlock()

		var sweepCount int
		for _, a := range acts {
			if a == actionSweepCleanup {
				sweepCount++
			}
		}
		if sweepCount != 1 {
			t.Errorf("IncReconciliationActions(%q) called %d times, want 1", actionSweepCleanup, sweepCount)
		}
		assertSweepDirRemoved(t, filepath.Join(tmpDir, "PROJ-10"))
	})
}

// --- Handoff-state reconcile tests ---

func TestReconcileRunningIssues_ReactionContinuationInHandoffStateKeepsRunning(t *testing.T) {
	t.Parallel()

	store := &mockReconcileStore{}
	tracker := &mockReconcileTracker{
		states: map[string]string{"ISSUE-1": "Ready For Review"},
	}
	params := defaultReconcileParams(t, store, tracker)
	params.StallTimeoutMS = 0
	params.HandoffState = "Ready For Review"
	// defaultReconcileParams ActiveStates = ["In Progress", "In Review"] — handoff excluded.

	state := NewState(5000, 4, nil, AgentTotals{})
	cc := &cancelCounter{}
	state.Running["ISSUE-1"] = &RunningEntry{
		Identifier:   "ISSUE-1-ident",
		StartedAt:    reconcileBaseTime,
		CancelFunc:   cc.cancel,
		Issue:        domain.Issue{State: "Ready For Review"},
		ReactionKind: ReactionKindReview,
	}
	state.Claimed["ISSUE-1"] = struct{}{}

	ReconcileRunningIssues(state, params)

	if cc.count != 0 {
		t.Errorf("CancelFunc called %d times, want 0 (reaction worker in handoff state must keep running)", cc.count)
	}
	if _, ok := state.Running["ISSUE-1"]; !ok {
		t.Error("running entry removed, want kept for reaction in handoff state")
	}
	if state.Running["ISSUE-1"].PendingCleanup {
		t.Error("PendingCleanup set for reaction in handoff state, want false")
	}
	if state.Running["ISSUE-1"].Issue.State != "Ready For Review" {
		t.Errorf("Issue.State = %q, want %q", state.Running["ISSUE-1"].Issue.State, "Ready For Review")
	}
	if len(store.deletedIssueID) != 0 {
		t.Errorf("DeleteRetryEntry called %d times, want 0 (keeping running)", len(store.deletedIssueID))
	}
}

func TestReconcileRunningIssues_NonReactionInHandoffStateCancels(t *testing.T) {
	t.Parallel()

	store := &mockReconcileStore{}
	tracker := &mockReconcileTracker{
		states: map[string]string{"ISSUE-1": "Ready For Review"},
	}
	params := defaultReconcileParams(t, store, tracker)
	params.StallTimeoutMS = 0
	params.HandoffState = "Ready For Review"

	state := NewState(5000, 4, nil, AgentTotals{})
	cc := &cancelCounter{}
	state.Running["ISSUE-1"] = &RunningEntry{
		Identifier:   "ISSUE-1-ident",
		StartedAt:    reconcileBaseTime,
		CancelFunc:   cc.cancel,
		Issue:        domain.Issue{State: "In Progress"},
		ReactionKind: "", // non-reaction
	}
	state.Claimed["ISSUE-1"] = struct{}{}

	ReconcileRunningIssues(state, params)

	if cc.count != 1 {
		t.Errorf("CancelFunc called %d times, want 1 (non-reaction in handoff must cancel)", cc.count)
	}
	if state.Running["ISSUE-1"].PendingCleanup {
		t.Error("PendingCleanup set for non-active non-terminal cancel, want false")
	}
	if len(store.deletedIssueID) != 1 {
		t.Fatalf("DeleteRetryEntry called %d times, want 1", len(store.deletedIssueID))
	}
	if store.deletedIssueID[0] != "ISSUE-1" {
		t.Errorf("deleted issue ID = %q, want %q", store.deletedIssueID[0], "ISSUE-1")
	}
}

func TestReconcileRunningIssues_ReactionInTerminalStateCancelsAndCleans(t *testing.T) {
	t.Parallel()

	store := &mockReconcileStore{}
	tracker := &mockReconcileTracker{
		states: map[string]string{"ISSUE-1": "Done"},
	}
	params := defaultReconcileParams(t, store, tracker)
	params.StallTimeoutMS = 0
	params.HandoffState = "Ready For Review"

	state := NewState(5000, 4, nil, AgentTotals{})
	cc := &cancelCounter{}
	state.Running["ISSUE-1"] = &RunningEntry{
		Identifier:   "ISSUE-1-ident",
		StartedAt:    reconcileBaseTime,
		CancelFunc:   cc.cancel,
		Issue:        domain.Issue{State: "Ready For Review"},
		ReactionKind: ReactionKindCI,
	}
	state.Claimed["ISSUE-1"] = struct{}{}

	ReconcileRunningIssues(state, params)

	if cc.count != 1 {
		t.Errorf("CancelFunc called %d times, want 1 (terminal state must cancel)", cc.count)
	}
	if !state.Running["ISSUE-1"].PendingCleanup {
		t.Error("PendingCleanup not set for reaction in terminal state, want true")
	}
	if len(store.deletedIssueID) != 1 {
		t.Fatalf("DeleteRetryEntry called %d times, want 1", len(store.deletedIssueID))
	}
	if store.deletedIssueID[0] != "ISSUE-1" {
		t.Errorf("deleted issue ID = %q, want %q", store.deletedIssueID[0], "ISSUE-1")
	}
}

func TestReconcileRunningIssues_ReactionInUnrelatedStateCancels(t *testing.T) {
	t.Parallel()

	store := &mockReconcileStore{}
	// "Blocked": not in active, not in terminal, not handoff.
	tracker := &mockReconcileTracker{
		states: map[string]string{"ISSUE-1": "Blocked"},
	}
	params := defaultReconcileParams(t, store, tracker)
	params.StallTimeoutMS = 0
	params.HandoffState = "Ready For Review"

	state := NewState(5000, 4, nil, AgentTotals{})
	cc := &cancelCounter{}
	state.Running["ISSUE-1"] = &RunningEntry{
		Identifier:   "ISSUE-1-ident",
		StartedAt:    reconcileBaseTime,
		CancelFunc:   cc.cancel,
		Issue:        domain.Issue{State: "In Progress"},
		ReactionKind: ReactionKindReview,
	}
	state.Claimed["ISSUE-1"] = struct{}{}

	ReconcileRunningIssues(state, params)

	if cc.count != 1 {
		t.Errorf("CancelFunc called %d times, want 1 (reaction in unrelated state must cancel)", cc.count)
	}
	if state.Running["ISSUE-1"].PendingCleanup {
		t.Error("PendingCleanup set for non-active non-terminal cancel, want false")
	}
	if len(store.deletedIssueID) != 1 {
		t.Fatalf("DeleteRetryEntry called %d times, want 1", len(store.deletedIssueID))
	}
}

// --- Age pass (workspace.retention_days) test doubles and helpers ---

// sweepStoreDouble is a test double for [SweepStore] returning a fixed
// completions map or a fixed error.
type sweepStoreDouble struct {
	completions map[string]string
	err         error
}

var _ SweepStore = (*sweepStoreDouble)(nil)

func (s *sweepStoreDouble) LatestRunCompletionByIdentifier(_ context.Context, identifiers []string) (map[string]string, error) {
	if s.err != nil {
		return nil, s.err
	}
	out := make(map[string]string, len(identifiers))
	for _, id := range identifiers {
		if v, ok := s.completions[id]; ok {
			out[id] = v
		}
	}
	return out, nil
}

// sweepRecord captures one slog record's message and attributes, keyed
// by attribute name, for the age-pass summary and log assertions.
type sweepRecord struct {
	message string
	attrs   map[string]any
}

// sweepLogHandler is a slog.Handler that captures every record it
// receives for later inspection.
type sweepLogHandler struct {
	records []sweepRecord
}

func (h *sweepLogHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *sweepLogHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := make(map[string]any, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Any()
		return true
	})
	h.records = append(h.records, sweepRecord{message: r.Message, attrs: attrs})
	return nil
}

func (h *sweepLogHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *sweepLogHandler) WithGroup(_ string) slog.Handler      { return h }

// countByMessage returns the number of captured records with the given message.
func (h *sweepLogHandler) countByMessage(msg string) int {
	n := 0
	for _, r := range h.records {
		if r.message == msg {
			n++
		}
	}
	return n
}

// findByMessage returns the first captured record with the given message.
func (h *sweepLogHandler) findByMessage(msg string) (sweepRecord, bool) {
	for _, r := range h.records {
		if r.message == msg {
			return r, true
		}
	}
	return sweepRecord{}, false
}

// intAttr returns the record's attribute at key as an int, failing the
// test if the attribute is absent or not an int64 value.
func intAttr(t *testing.T, rec sweepRecord, key string) int {
	t.Helper()
	v, ok := rec.attrs[key]
	if !ok {
		t.Fatalf("record %q missing attribute %q", rec.message, key)
	}
	n, ok := v.(int64)
	if !ok {
		t.Fatalf("record %q attribute %q = %T, want int64", rec.message, key, v)
	}
	return int(n)
}

// stringAttr returns the record's attribute at key as a string, failing
// the test if the attribute is absent or not a string value.
func stringAttr(t *testing.T, rec sweepRecord, key string) string {
	t.Helper()
	v, ok := rec.attrs[key]
	if !ok {
		t.Fatalf("record %q missing attribute %q", rec.message, key)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("record %q attribute %q = %T, want string", rec.message, key, v)
	}
	return s
}

// assertSweepPartitionIdentity asserts that the summary record's
// candidates attribute equals the sum of the other nine outcome
// counters, the partition identity the summary record must hold on
// every pass.
func assertSweepPartitionIdentity(t *testing.T, rec sweepRecord) {
	t.Helper()
	candidates := intAttr(t, rec, "candidates")
	sum := intAttr(t, rec, "excluded_running") +
		intAttr(t, rec, "excluded_retry") +
		intAttr(t, rec, "excluded_reaction") +
		intAttr(t, rec, "removed_terminal") +
		intAttr(t, rec, "removed_age") +
		intAttr(t, rec, "retained_in_window") +
		intAttr(t, rec, "retained_no_activity") +
		intAttr(t, rec, "retained_not_evaluated") +
		intAttr(t, rec, "failed")
	if candidates != sum {
		t.Errorf("sweep summary candidates = %d, want sum of other counters %d", candidates, sum)
	}
}

// writeSweepSCMMetadata writes a minimal .sortie/scm.json under
// workspacePath carrying pushedAt as the pushed_at field. Branch is set
// to a fixed non-empty value because workspace.ReadSCMMetadata discards
// the whole record when Branch is empty.
func writeSweepSCMMetadata(t *testing.T, workspacePath, pushedAt string) {
	t.Helper()
	dotSortie := filepath.Join(workspacePath, ".sortie")
	if err := os.MkdirAll(dotSortie, 0o755); err != nil {
		t.Fatalf("os.MkdirAll(%q): %v", dotSortie, err)
	}
	meta := fmt.Sprintf(`{"branch":"feature/sweep-test","pushed_at":%q}`, pushedAt)
	if err := os.WriteFile(filepath.Join(dotSortie, "scm.json"), []byte(meta), 0o644); err != nil {
		t.Fatalf("os.WriteFile(scm.json): %v", err)
	}
}

// oldSweepTimestamp returns an RFC3339 timestamp well outside a
// [config.WorkspaceRetentionMinDays] window, for age-removal cases.
func oldSweepTimestamp() string {
	return time.Now().UTC().Add(-40 * 24 * time.Hour).Format(time.RFC3339)
}

// recentSweepTimestamp returns an RFC3339 timestamp inside a
// [config.WorkspaceRetentionMinDays] window, for age-retention cases.
func recentSweepTimestamp() string {
	return time.Now().UTC().Add(-2 * 24 * time.Hour).Format(time.RFC3339)
}

// --- R6: the retention floor and the recovery lookback are coupled ---

// TestWorkspaceRetentionFloorMatchesRecoveryLookback asserts the equality
// [Spec-706 §3.3.5] relies on: the age pass never removes a workspace
// pending-reaction recovery would not already have treated as stale.
// Changing either constant without the other reintroduces that defect.
func TestWorkspaceRetentionFloorMatchesRecoveryLookback(t *testing.T) {
	t.Parallel()

	floor := time.Duration(config.WorkspaceRetentionMinDays) * 24 * time.Hour
	if floor != PendingReactionRecoveryLookback {
		t.Errorf("WorkspaceRetentionMinDays as a duration = %v, want equal to PendingReactionRecoveryLookback %v",
			floor, PendingReactionRecoveryLookback)
	}
}

// --- R10: the narrowed pending-reaction exclusion ---

func TestSweepWorkspaces_NarrowedReactionExclusion(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	mustMkdirSweep(t, filepath.Join(tmpDir, "PROJ-LR"))
	mustMkdirSweep(t, filepath.Join(tmpDir, "PROJ-RV"))

	tracker := &sweepTracker{statesByKey: map[string]string{"PROJ-LR": "In Progress"}}
	state := NewState(5000, 4, nil, AgentTotals{})
	state.PendingReactions["id-lr:label-review"] = &PendingReaction{Identifier: "PROJ-LR", Kind: ReactionKindLabelReview}
	state.PendingReactions["id-rv:review"] = &PendingReaction{Identifier: "PROJ-RV", Kind: ReactionKindReview}

	SweepWorkspaces(state, defaultSweepParams(t, tmpDir, tracker))

	if !slices.Contains(tracker.calledWith, "PROJ-LR") {
		t.Errorf("FetchIssueStatesByIdentifiers received %v, want to contain %q (label-review must not exclude)", tracker.calledWith, "PROJ-LR")
	}
	if slices.Contains(tracker.calledWith, "PROJ-RV") {
		t.Errorf("FetchIssueStatesByIdentifiers received %v, want to omit %q (review must exclude)", tracker.calledWith, "PROJ-RV")
	}
}

// --- R12: the summary partition identity across six pass shapes ---

func TestSweepWorkspaces_SummaryPartitionIdentity(t *testing.T) {
	t.Parallel()

	t.Run("ZeroKeysOnDisk", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		tracker := &sweepTracker{}
		state := NewState(5000, 4, nil, AgentTotals{})
		handler := &sweepLogHandler{}
		params := defaultSweepParams(t, tmpDir, tracker)
		params.Logger = slog.New(handler)

		SweepWorkspaces(state, params)

		if got := handler.countByMessage("sweep: pass complete"); got != 1 {
			t.Fatalf(`"sweep: pass complete" logged %d times, want 1`, got)
		}
		rec, _ := handler.findByMessage("sweep: pass complete")
		if got := intAttr(t, rec, "candidates"); got != 0 {
			t.Errorf("candidates = %d, want 0", got)
		}
		assertSweepPartitionIdentity(t, rec)
	})

	t.Run("EveryListedKeyInFlight", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		mustMkdirSweep(t, filepath.Join(tmpDir, "PROJ-RUN"))
		mustMkdirSweep(t, filepath.Join(tmpDir, "PROJ-RETRY"))
		mustMkdirSweep(t, filepath.Join(tmpDir, "PROJ-REACT"))

		tracker := &sweepTracker{}
		state := NewState(5000, 4, nil, AgentTotals{})
		state.Running["id-run"] = &RunningEntry{Identifier: "PROJ-RUN"}
		state.RetryAttempts["id-retry"] = &RetryEntry{Identifier: "PROJ-RETRY"}
		state.PendingReactions["id-react:ci"] = &PendingReaction{Identifier: "PROJ-REACT", Kind: ReactionKindCI}
		handler := &sweepLogHandler{}
		params := defaultSweepParams(t, tmpDir, tracker)
		params.Logger = slog.New(handler)

		SweepWorkspaces(state, params)

		if tracker.calledWith != nil {
			t.Errorf("FetchIssueStatesByIdentifiers called with %v, want not called (nothing remains)", tracker.calledWith)
		}
		rec, _ := handler.findByMessage("sweep: pass complete")
		if got := intAttr(t, rec, "candidates"); got != 3 {
			t.Errorf("candidates = %d, want 3", got)
		}
		if got := intAttr(t, rec, "excluded_running"); got != 1 {
			t.Errorf("excluded_running = %d, want 1", got)
		}
		if got := intAttr(t, rec, "excluded_retry"); got != 1 {
			t.Errorf("excluded_retry = %d, want 1", got)
		}
		if got := intAttr(t, rec, "excluded_reaction"); got != 1 {
			t.Errorf("excluded_reaction = %d, want 1", got)
		}
		assertSweepPartitionIdentity(t, rec)
	})

	t.Run("FailedTrackerRead", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		mustMkdirSweep(t, filepath.Join(tmpDir, "PROJ-FAIL"))

		tracker := &sweepTracker{fetchErr: errors.New("tracker unavailable")}
		state := NewState(5000, 4, nil, AgentTotals{})
		handler := &sweepLogHandler{}
		params := defaultSweepParams(t, tmpDir, tracker)
		params.Logger = slog.New(handler)

		SweepWorkspaces(state, params)

		rec, _ := handler.findByMessage("sweep: pass complete")
		if got := stringAttr(t, rec, "tracker_read"); got != "failed" {
			t.Errorf("tracker_read = %q, want %q", got, "failed")
		}
		if got := intAttr(t, rec, "retained_not_evaluated"); got != 1 {
			t.Errorf("retained_not_evaluated = %d, want 1 (bound off by default)", got)
		}
		assertSweepPartitionIdentity(t, rec)
	})

	t.Run("BoundOffRetainsNotEvaluatedCount", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		mustMkdirSweep(t, filepath.Join(tmpDir, "PROJ-EXCL"))
		mustMkdirSweep(t, filepath.Join(tmpDir, "PROJ-CAND1"))
		mustMkdirSweep(t, filepath.Join(tmpDir, "PROJ-CAND2"))

		tracker := &sweepTracker{statesByKey: map[string]string{
			"PROJ-CAND1": "In Progress",
			"PROJ-CAND2": "In Progress",
		}}
		state := NewState(5000, 4, nil, AgentTotals{})
		state.Running["id-excl"] = &RunningEntry{Identifier: "PROJ-EXCL"}
		handler := &sweepLogHandler{}
		params := defaultSweepParams(t, tmpDir, tracker) // RetentionDays zero value: bound off

		params.Logger = slog.New(handler)

		SweepWorkspaces(state, params)

		rec, _ := handler.findByMessage("sweep: pass complete")
		if got := stringAttr(t, rec, "age_pass"); got != "off" {
			t.Errorf("age_pass = %q, want %q", got, "off")
		}
		if got := intAttr(t, rec, "retained_not_evaluated"); got != 2 {
			t.Errorf("retained_not_evaluated = %d, want 2", got)
		}
		assertSweepPartitionIdentity(t, rec)
	})

	t.Run("BoundOnNilStore", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		mustMkdirSweep(t, filepath.Join(tmpDir, "PROJ-NILSTORE"))

		tracker := &sweepTracker{statesByKey: map[string]string{}}
		state := NewState(5000, 4, nil, AgentTotals{})
		handler := &sweepLogHandler{}
		params := defaultSweepParams(t, tmpDir, tracker)
		params.RetentionDays = config.WorkspaceRetentionMinDays
		params.Store = nil
		params.Logger = slog.New(handler)

		SweepWorkspaces(state, params)

		rec, _ := handler.findByMessage("sweep: pass complete")
		if got := stringAttr(t, rec, "age_pass"); got != "unavailable" {
			t.Errorf("age_pass = %q, want %q", got, "unavailable")
		}
		if got := intAttr(t, rec, "retained_not_evaluated"); got != 1 {
			t.Errorf("retained_not_evaluated = %d, want 1", got)
		}
		assertSweepPartitionIdentity(t, rec)
	})

	t.Run("BoundOnStoreReadError", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		mustMkdirSweep(t, filepath.Join(tmpDir, "PROJ-STOREERR"))

		tracker := &sweepTracker{statesByKey: map[string]string{}}
		state := NewState(5000, 4, nil, AgentTotals{})
		handler := &sweepLogHandler{}
		params := defaultSweepParams(t, tmpDir, tracker)
		params.RetentionDays = config.WorkspaceRetentionMinDays
		params.Store = &sweepStoreDouble{err: errors.New("db unavailable")}
		params.Logger = slog.New(handler)

		SweepWorkspaces(state, params)

		rec, _ := handler.findByMessage("sweep: pass complete")
		if got := stringAttr(t, rec, "age_pass"); got != "unavailable" {
			t.Errorf("age_pass = %q, want %q", got, "unavailable")
		}
		if got := intAttr(t, rec, "retained_not_evaluated"); got != 1 {
			t.Errorf("retained_not_evaluated = %d, want 1", got)
		}
		assertSweepPartitionIdentity(t, rec)
	})
}

// --- R13: terminal-and-old is counted once, under removed_terminal ---

func TestSweepWorkspaces_TerminalAndOldCountedOnceUnderTerminal(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	wsPath := filepath.Join(tmpDir, "PROJ-BOTH")
	mustMkdirSweep(t, wsPath)
	writeSweepSCMMetadata(t, wsPath, oldSweepTimestamp())

	tracker := &sweepTracker{statesByKey: map[string]string{"PROJ-BOTH": "Done"}}
	state := NewState(5000, 4, nil, AgentTotals{})
	handler := &sweepLogHandler{}
	params := defaultSweepParams(t, tmpDir, tracker)
	params.RetentionDays = config.WorkspaceRetentionMinDays
	params.Store = &sweepStoreDouble{}
	params.Logger = slog.New(handler)

	SweepWorkspaces(state, params)

	assertSweepDirRemoved(t, wsPath)
	rec, _ := handler.findByMessage("sweep: pass complete")
	if got := intAttr(t, rec, "removed_terminal"); got != 1 {
		t.Errorf("removed_terminal = %d, want 1", got)
	}
	if got := intAttr(t, rec, "removed_age"); got != 0 {
		t.Errorf("removed_age = %d, want 0 (terminal check runs first and claims the key)", got)
	}
}

// --- R14: Cleanup receives Identifier and IssueID both set to the key ---

func TestSweepWorkspaces_AgeRemovalUsesIdentifierAndIssueIDAsKey(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("probe hook uses printf and $VAR expansion, unavailable under cmd.exe")
	}
	t.Parallel()

	tmpDir := t.TempDir()
	const key = "PROJ-HOOK"
	wsPath := filepath.Join(tmpDir, key)
	mustMkdirSweep(t, wsPath)
	writeSweepSCMMetadata(t, wsPath, oldSweepTimestamp())

	markerDir := t.TempDir()
	envFile := filepath.Join(markerDir, "env.txt")
	script := `printf "%s\n%s" "$SORTIE_ISSUE_ID" "$SORTIE_ISSUE_IDENTIFIER" > "` + envFile + `"`

	tracker := &sweepTracker{statesByKey: map[string]string{}}
	state := NewState(5000, 4, nil, AgentTotals{})
	params := defaultSweepParams(t, tmpDir, tracker)
	params.RetentionDays = config.WorkspaceRetentionMinDays
	params.Store = &sweepStoreDouble{}
	params.BeforeRemoveHook = script
	params.HookTimeoutMS = 5000

	SweepWorkspaces(state, params)

	assertSweepDirRemoved(t, wsPath)
	data, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("os.ReadFile(%q): %v", envFile, err)
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) != 2 || lines[0] != key || lines[1] != key {
		t.Errorf("before_remove hook env (SORTIE_ISSUE_ID, SORTIE_ISSUE_IDENTIFIER) = %v, want both %q", lines, key)
	}
}

// --- R15: the age pass removes eligible candidates on a failed tracker read ---

func TestSweepWorkspaces_AgePassRemovesOnTrackerReadFailure(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	wsPath := filepath.Join(tmpDir, "PROJ-TRKFAIL")
	mustMkdirSweep(t, wsPath)
	writeSweepSCMMetadata(t, wsPath, oldSweepTimestamp())

	tracker := &sweepTracker{fetchErr: errors.New("tracker unavailable")}
	state := NewState(5000, 4, nil, AgentTotals{})
	handler := &sweepLogHandler{}
	params := defaultSweepParams(t, tmpDir, tracker)
	params.RetentionDays = config.WorkspaceRetentionMinDays
	params.Store = &sweepStoreDouble{}
	params.Logger = slog.New(handler)

	SweepWorkspaces(state, params)

	assertSweepDirRemoved(t, wsPath)
	rec, _ := handler.findByMessage("sweep: pass complete")
	if got := intAttr(t, rec, "removed_age"); got != 1 {
		t.Errorf("removed_age = %d, want 1", got)
	}
	if got := stringAttr(t, rec, "tracker_read"); got != "failed" {
		t.Errorf("tracker_read = %q, want %q", got, "failed")
	}
}

// --- R16: removal by age is independent of the issue's tracker condition ---

func TestSweepWorkspaces_RemovedByAgeRegardlessOfTrackerCondition(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		stateName string
		absent    bool
	}{
		{name: "HandoffState", stateName: "Ready For Review"},
		{name: "ActiveState", stateName: "In Progress"},
		{name: "UnnamedState", stateName: "Some Other State"},
		{name: "AbsentFromTrackerResponse", absent: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tmpDir := t.TempDir()
			key := "PROJ-" + tc.name
			wsPath := filepath.Join(tmpDir, key)
			mustMkdirSweep(t, wsPath)
			writeSweepSCMMetadata(t, wsPath, oldSweepTimestamp())

			statesByKey := map[string]string{}
			if !tc.absent {
				statesByKey[key] = tc.stateName
			}
			tracker := &sweepTracker{statesByKey: statesByKey}
			state := NewState(5000, 4, nil, AgentTotals{})
			params := defaultSweepParams(t, tmpDir, tracker)
			params.RetentionDays = config.WorkspaceRetentionMinDays
			params.Store = &sweepStoreDouble{}

			SweepWorkspaces(state, params)

			assertSweepDirRemoved(t, wsPath)
		})
	}
}

// --- R17: no parseable activity retains the workspace regardless of age ---

func TestSweepWorkspaces_RetainedNoActivity(t *testing.T) {
	t.Parallel()

	t.Run("NoRunHistoryAndNoSCMMetadata", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		wsPath := filepath.Join(tmpDir, "PROJ-NOACT")
		mustMkdirSweep(t, wsPath)

		tracker := &sweepTracker{}
		state := NewState(5000, 4, nil, AgentTotals{})
		handler := &sweepLogHandler{}
		params := defaultSweepParams(t, tmpDir, tracker)
		params.RetentionDays = config.WorkspaceRetentionMinDays
		params.Store = &sweepStoreDouble{}
		params.Logger = slog.New(handler)

		SweepWorkspaces(state, params)

		assertSweepDirExists(t, wsPath)
		rec, _ := handler.findByMessage("sweep: pass complete")
		if got := intAttr(t, rec, "retained_no_activity"); got != 1 {
			t.Errorf("retained_no_activity = %d, want 1", got)
		}
	})

	t.Run("UnparseableCompletion", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		const key = "PROJ-BADTIME"
		wsPath := filepath.Join(tmpDir, key)
		mustMkdirSweep(t, wsPath)

		tracker := &sweepTracker{}
		state := NewState(5000, 4, nil, AgentTotals{})
		handler := &sweepLogHandler{}
		params := defaultSweepParams(t, tmpDir, tracker)
		params.RetentionDays = config.WorkspaceRetentionMinDays
		params.Store = &sweepStoreDouble{completions: map[string]string{key: "not-a-time"}}
		params.Logger = slog.New(handler)

		SweepWorkspaces(state, params)

		assertSweepDirExists(t, wsPath)
		rec, _ := handler.findByMessage("sweep: pass complete")
		if got := intAttr(t, rec, "retained_no_activity"); got != 1 {
			t.Errorf("retained_no_activity = %d, want 1", got)
		}
	})
}

// --- R18: the anchor is the later of the two parsed timestamps ---

func TestSweepWorkspaces_AnchorIsLaterTimestamp(t *testing.T) {
	t.Parallel()

	t.Run("OldCompletionRecentPush_Retained", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		const key = "PROJ-ANCHOR1"
		wsPath := filepath.Join(tmpDir, key)
		mustMkdirSweep(t, wsPath)
		writeSweepSCMMetadata(t, wsPath, recentSweepTimestamp())

		tracker := &sweepTracker{}
		state := NewState(5000, 4, nil, AgentTotals{})
		params := defaultSweepParams(t, tmpDir, tracker)
		params.RetentionDays = config.WorkspaceRetentionMinDays
		params.Store = &sweepStoreDouble{completions: map[string]string{key: oldSweepTimestamp()}}

		SweepWorkspaces(state, params)

		assertSweepDirExists(t, wsPath)
	})

	t.Run("RecentCompletionOldPush_Retained", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		const key = "PROJ-ANCHOR2"
		wsPath := filepath.Join(tmpDir, key)
		mustMkdirSweep(t, wsPath)
		writeSweepSCMMetadata(t, wsPath, oldSweepTimestamp())

		tracker := &sweepTracker{}
		state := NewState(5000, 4, nil, AgentTotals{})
		params := defaultSweepParams(t, tmpDir, tracker)
		params.RetentionDays = config.WorkspaceRetentionMinDays
		params.Store = &sweepStoreDouble{completions: map[string]string{key: recentSweepTimestamp()}}

		SweepWorkspaces(state, params)

		assertSweepDirExists(t, wsPath)
	})

	t.Run("BothOld_Removed", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		const key = "PROJ-ANCHOR3"
		wsPath := filepath.Join(tmpDir, key)
		mustMkdirSweep(t, wsPath)
		writeSweepSCMMetadata(t, wsPath, oldSweepTimestamp())

		tracker := &sweepTracker{}
		state := NewState(5000, 4, nil, AgentTotals{})
		params := defaultSweepParams(t, tmpDir, tracker)
		params.RetentionDays = config.WorkspaceRetentionMinDays
		params.Store = &sweepStoreDouble{completions: map[string]string{key: oldSweepTimestamp()}}

		SweepWorkspaces(state, params)

		assertSweepDirRemoved(t, wsPath)
	})
}

// --- R19: running/retry precedence in the in-flight exclusion ---

func TestSweepWorkspaces_InFlightPrecedence(t *testing.T) {
	t.Parallel()

	t.Run("RunningOnly", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		mustMkdirSweep(t, filepath.Join(tmpDir, "PROJ-RUNONLY"))

		tracker := &sweepTracker{}
		state := NewState(5000, 4, nil, AgentTotals{})
		state.Running["id-run"] = &RunningEntry{Identifier: "PROJ-RUNONLY"}
		handler := &sweepLogHandler{}
		params := defaultSweepParams(t, tmpDir, tracker)
		params.Logger = slog.New(handler)

		SweepWorkspaces(state, params)

		rec, _ := handler.findByMessage("sweep: pass complete")
		if got := intAttr(t, rec, "excluded_running"); got != 1 {
			t.Errorf("excluded_running = %d, want 1", got)
		}
		if got := intAttr(t, rec, "excluded_retry"); got != 0 {
			t.Errorf("excluded_retry = %d, want 0", got)
		}
	})

	t.Run("RetryOnly", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		mustMkdirSweep(t, filepath.Join(tmpDir, "PROJ-RETRYONLY"))

		tracker := &sweepTracker{}
		state := NewState(5000, 4, nil, AgentTotals{})
		state.RetryAttempts["id-retry"] = &RetryEntry{Identifier: "PROJ-RETRYONLY"}
		handler := &sweepLogHandler{}
		params := defaultSweepParams(t, tmpDir, tracker)
		params.Logger = slog.New(handler)

		SweepWorkspaces(state, params)

		rec, _ := handler.findByMessage("sweep: pass complete")
		if got := intAttr(t, rec, "excluded_running"); got != 0 {
			t.Errorf("excluded_running = %d, want 0", got)
		}
		if got := intAttr(t, rec, "excluded_retry"); got != 1 {
			t.Errorf("excluded_retry = %d, want 1", got)
		}
	})

	t.Run("RunningTakesPrecedenceOverRetry", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		mustMkdirSweep(t, filepath.Join(tmpDir, "PROJ-BOTHFLIGHT"))

		tracker := &sweepTracker{}
		state := NewState(5000, 4, nil, AgentTotals{})
		state.Running["id-run"] = &RunningEntry{Identifier: "PROJ-BOTHFLIGHT"}
		state.RetryAttempts["id-retry"] = &RetryEntry{Identifier: "PROJ-BOTHFLIGHT"}
		handler := &sweepLogHandler{}
		params := defaultSweepParams(t, tmpDir, tracker)
		params.Logger = slog.New(handler)

		SweepWorkspaces(state, params)

		rec, _ := handler.findByMessage("sweep: pass complete")
		if got := intAttr(t, rec, "excluded_running"); got != 1 {
			t.Errorf("excluded_running = %d, want 1 (running takes precedence)", got)
		}
		if got := intAttr(t, rec, "excluded_retry"); got != 0 {
			t.Errorf("excluded_retry = %d, want 0 (already counted as running)", got)
		}
	})
}

// --- R20: the sweep performs no reaction- or fingerprint-state mutation ---

func TestSweepWorkspaces_AgeRemovalLeavesPendingReactionsAndFingerprintsUnchanged(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	wsPath := filepath.Join(tmpDir, "PROJ-NOSIDEFX")
	mustMkdirSweep(t, wsPath)
	writeSweepSCMMetadata(t, wsPath, oldSweepTimestamp())

	ctx := context.Background()
	store := openInMemoryStore(t)
	if err := store.UpsertReactionFingerprint(ctx, "issue-nosidefx", ReactionKindCI, "fp-nosidefx"); err != nil {
		t.Fatalf("UpsertReactionFingerprint: %v", err)
	}

	state := NewState(5000, 4, nil, AgentTotals{})
	state.PendingReactions["other:ci"] = &PendingReaction{Identifier: "OTHER-1", Kind: ReactionKindCI}

	tracker := &sweepTracker{}
	params := defaultSweepParams(t, tmpDir, tracker)
	params.RetentionDays = config.WorkspaceRetentionMinDays
	params.Store = store
	params.Ctx = ctx

	SweepWorkspaces(state, params)

	assertSweepDirRemoved(t, wsPath)
	if len(state.PendingReactions) != 1 {
		t.Errorf("len(state.PendingReactions) = %d, want 1 (unchanged)", len(state.PendingReactions))
	}
	fp, dispatched, err := store.GetReactionFingerprint(ctx, "issue-nosidefx", ReactionKindCI)
	if err != nil {
		t.Fatalf("GetReactionFingerprint: %v", err)
	}
	if fp != "fp-nosidefx" || dispatched {
		t.Errorf("GetReactionFingerprint = (%q, %v), want (%q, false)", fp, dispatched, "fp-nosidefx")
	}
}

// --- R22: both removal mechanisms record their own metric in one pass ---

func TestSweepWorkspaces_MetricsRecordBothMechanismsInSamePass(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	terminalWsPath := filepath.Join(tmpDir, "PROJ-METRIC-TERM")
	mustMkdirSweep(t, terminalWsPath)
	ageWsPath := filepath.Join(tmpDir, "PROJ-METRIC-AGE")
	mustMkdirSweep(t, ageWsPath)
	writeSweepSCMMetadata(t, ageWsPath, oldSweepTimestamp())

	tracker := &sweepTracker{statesByKey: map[string]string{"PROJ-METRIC-TERM": "Done"}}
	state := NewState(5000, 4, nil, AgentTotals{})
	spy := &spyMetrics{}
	params := defaultSweepParams(t, tmpDir, tracker)
	params.Metrics = spy
	params.RetentionDays = config.WorkspaceRetentionMinDays
	params.Store = &sweepStoreDouble{}

	SweepWorkspaces(state, params)

	assertSweepDirRemoved(t, terminalWsPath)
	assertSweepDirRemoved(t, ageWsPath)

	spy.mu.Lock()
	acts := append([]string(nil), spy.reconciliationActs...)
	spy.mu.Unlock()

	var cleanupCount, expiredCount int
	for _, a := range acts {
		switch a {
		case actionSweepCleanup:
			cleanupCount++
		case actionSweepExpired:
			expiredCount++
		}
	}
	if cleanupCount != 1 {
		t.Errorf("IncReconciliationActions(%q) called %d times, want 1", actionSweepCleanup, cleanupCount)
	}
	if expiredCount != 1 {
		t.Errorf("IncReconciliationActions(%q) called %d times, want 1", actionSweepExpired, expiredCount)
	}
}

// --- R23: an age removal log record carries the required attributes ---

func TestSweepWorkspaces_AgeRemovalLogCarriesRequiredAttributes(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	const key = "PROJ-LOGATTRS"
	wsPath := filepath.Join(tmpDir, key)
	mustMkdirSweep(t, wsPath)
	writeSweepSCMMetadata(t, wsPath, oldSweepTimestamp())

	tracker := &sweepTracker{}
	state := NewState(5000, 4, nil, AgentTotals{})
	handler := &sweepLogHandler{}
	params := defaultSweepParams(t, tmpDir, tracker)
	params.RetentionDays = config.WorkspaceRetentionMinDays
	params.Store = &sweepStoreDouble{}
	params.Logger = slog.New(handler)

	SweepWorkspaces(state, params)

	rec, ok := handler.findByMessage("sweep: removed expired workspace")
	if !ok {
		t.Fatal(`"sweep: removed expired workspace" not logged`)
	}
	for _, attrKey := range []string{"workspace_key", "last_activity", "age_days"} {
		if _, ok := rec.attrs[attrKey]; !ok {
			t.Errorf("removed-workspace log missing attribute %q", attrKey)
		}
	}
	if got := stringAttr(t, rec, "workspace_key"); got != key {
		t.Errorf("workspace_key = %q, want %q", got, key)
	}
}

// --- reconcileOverdueRetries tests ---

// overdueRetryStore is a store double that records every method call so a
// re-arm can be proven to perform none of them.
type overdueRetryStore struct {
	mockReconcileStore
}

// callCount returns the total number of ReconcileStore method calls
// recorded across every method this double tracks.
func (s *overdueRetryStore) callCount() int {
	return len(s.savedEntries) + len(s.deletedIssueID) +
		s.upsertFingerprintCalls + s.getFingerprintCalls +
		s.markDispatchedCalls + s.deleteFingerprintCalls
}

func TestReconcileOverdueRetries_ReArmsOverdueEntry(t *testing.T) {
	t.Parallel()

	store := &overdueRetryStore{}
	tracker := &mockReconcileTracker{}
	fired := make(chan string, 4)
	handler := &sweepLogHandler{}
	params := ReconcileParams{
		TrackerAdapter:    tracker,
		ActiveStates:      []string{"In Progress", "In Review"},
		TerminalStates:    []string{"Done", "Closed"},
		MaxRetryBackoffMS: 300_000,
		Store:             store,
		OnRetryFire:       func(issueID string) { fired <- issueID },
		NowFunc:           func() time.Time { return reconcileBaseTime },
		Ctx:               context.Background(),
		Logger:            slog.New(handler),
	}

	state := NewState(5000, 4, nil, AgentTotals{})
	oldTimer := time.AfterFunc(time.Hour, func() {})
	overdueDue := reconcileBaseTime.Add(-2 * time.Minute).UnixMilli()
	state.RetryAttempts["OVERDUE-1"] = &RetryEntry{
		IssueID:             "OVERDUE-1",
		Identifier:          "OVERDUE-1-ident",
		DisplayID:           "OVERDUE-1-display",
		SessionID:           "sess-1",
		Attempt:             3,
		DueAtMS:             overdueDue,
		Error:               "some error",
		TimerHandle:         oldTimer,
		LastSSHHost:         "host-a",
		ContinuationContext: map[string]any{"ci_failure": map[string]any{"x": 1}},
		ReactionKind:        ReactionKindCI,
		RuleName:            "rule-1",
		TemplateID:          "tmpl-1",
		AgentKind:           "claude",
	}
	state.Claimed["OVERDUE-1"] = struct{}{}

	before := time.Now().UnixMilli()
	ReconcileRunningIssues(state, params)
	after := time.Now().UnixMilli()

	entry, ok := state.RetryAttempts["OVERDUE-1"]
	if !ok {
		t.Fatal("RetryAttempts entry missing after re-arm")
	}
	if entry.DueAtMS < before || entry.DueAtMS > after {
		t.Errorf("DueAtMS = %d, want between %d and %d (re-armed at the tick's now)", entry.DueAtMS, before, after)
	}
	if entry.ReactionKind != ReactionKindCI {
		t.Errorf("ReactionKind = %q, want %q", entry.ReactionKind, ReactionKindCI)
	}
	if entry.Attempt != 3 {
		t.Errorf("Attempt = %d, want 3", entry.Attempt)
	}
	if entry.Identifier != "OVERDUE-1-ident" {
		t.Errorf("Identifier = %q, want %q", entry.Identifier, "OVERDUE-1-ident")
	}
	if entry.DisplayID != "OVERDUE-1-display" {
		t.Errorf("DisplayID = %q, want %q", entry.DisplayID, "OVERDUE-1-display")
	}
	if entry.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want %q", entry.SessionID, "sess-1")
	}
	if entry.LastSSHHost != "host-a" {
		t.Errorf("LastSSHHost = %q, want %q", entry.LastSSHHost, "host-a")
	}
	if entry.RuleName != "rule-1" {
		t.Errorf("RuleName = %q, want %q", entry.RuleName, "rule-1")
	}
	if entry.TemplateID != "tmpl-1" {
		t.Errorf("TemplateID = %q, want %q", entry.TemplateID, "tmpl-1")
	}
	if entry.AgentKind != "claude" {
		t.Errorf("AgentKind = %q, want %q", entry.AgentKind, "claude")
	}
	if entry.ContinuationContext == nil {
		t.Fatal("ContinuationContext is nil, want preserved")
	}
	if _, ok := state.Claimed["OVERDUE-1"]; !ok {
		t.Error("Claimed entry removed by re-arm, want unchanged")
	}
	if handler.countByMessage("overdue retry re-armed") != 1 {
		t.Errorf(`Warn("overdue retry re-armed") emitted %d times, want 1`, handler.countByMessage("overdue retry re-armed"))
	}
	if handler.countByMessage("retry slot displaced") != 0 {
		t.Errorf(`Warn("retry slot displaced") emitted %d times, want 0`, handler.countByMessage("retry slot displaced"))
	}
	if rec, ok := handler.findByMessage("overdue retry re-armed"); ok {
		if got := stringAttr(t, rec, "kind"); got != ReactionKindCI {
			t.Errorf("kind attribute = %q, want %q", got, ReactionKindCI)
		}
		if got := intAttr(t, rec, "attempt"); got != 3 {
			t.Errorf("attempt attribute = %d, want 3", got)
		}
		if _, hasOverdueMS := rec.attrs["overdue_ms"]; !hasOverdueMS {
			t.Error("overdue_ms attribute missing")
		}
	}
	if got := store.callCount(); got != 0 {
		t.Errorf("Store method calls = %d, want 0", got)
	}

	select {
	case id := <-fired:
		if id != "OVERDUE-1" {
			t.Errorf("OnRetryFire received %q, want %q", id, "OVERDUE-1")
		}
	case <-time.After(time.Second):
		t.Fatal("OnRetryFire not invoked within 1 second")
	}

	if entry.TimerHandle != nil {
		entry.TimerHandle.Stop()
	}
}

func TestReconcileOverdueRetries_FutureDueAtMSUntouched(t *testing.T) {
	t.Parallel()

	store := &overdueRetryStore{}
	tracker := &mockReconcileTracker{}
	handler := &sweepLogHandler{}
	params := ReconcileParams{
		TrackerAdapter:    tracker,
		ActiveStates:      []string{"In Progress", "In Review"},
		TerminalStates:    []string{"Done", "Closed"},
		MaxRetryBackoffMS: 300_000,
		Store:             store,
		OnRetryFire:       noopRetryFire,
		NowFunc:           func() time.Time { return reconcileBaseTime },
		Ctx:               context.Background(),
		Logger:            slog.New(handler),
	}

	state := NewState(5000, 4, nil, AgentTotals{})
	timer := time.AfterFunc(time.Hour, func() {})
	t.Cleanup(func() { timer.Stop() })
	futureDue := reconcileBaseTime.Add(5 * time.Minute).UnixMilli()
	original := &RetryEntry{
		IssueID:      "FUTURE-1",
		Identifier:   "FUTURE-1-ident",
		Attempt:      1,
		DueAtMS:      futureDue,
		TimerHandle:  timer,
		ReactionKind: ReactionKindCI,
	}
	state.RetryAttempts["FUTURE-1"] = original

	ReconcileRunningIssues(state, params)

	entry, ok := state.RetryAttempts["FUTURE-1"]
	if !ok {
		t.Fatal("RetryAttempts entry removed for a future-DueAtMS entry, want untouched")
	}
	if entry != original {
		t.Error("RetryAttempts entry replaced for a future-DueAtMS entry, want the same pointer")
	}
	if entry.DueAtMS != futureDue {
		t.Errorf("DueAtMS = %d, want unchanged %d", entry.DueAtMS, futureDue)
	}
	if handler.countByMessage("overdue retry re-armed") != 0 {
		t.Errorf(`Warn("overdue retry re-armed") emitted %d times, want 0`, handler.countByMessage("overdue retry re-armed"))
	}
	if got := store.callCount(); got != 0 {
		t.Errorf("Store method calls = %d, want 0", got)
	}
}

func TestReconcileOverdueRetries_StartupReconstructedEntrySkipped(t *testing.T) {
	t.Parallel()

	store := &overdueRetryStore{}
	tracker := &mockReconcileTracker{}
	fired := make(chan string, 1)
	handler := &sweepLogHandler{}
	params := ReconcileParams{
		TrackerAdapter:    tracker,
		ActiveStates:      []string{"In Progress", "In Review"},
		TerminalStates:    []string{"Done", "Closed"},
		MaxRetryBackoffMS: 300_000,
		Store:             store,
		OnRetryFire:       func(issueID string) { fired <- issueID },
		NowFunc:           func() time.Time { return reconcileBaseTime },
		Ctx:               context.Background(),
		Logger:            slog.New(handler),
	}

	state := NewState(5000, 4, nil, AgentTotals{})
	overdueDueMs := reconcileBaseTime.Add(-5 * time.Minute).UnixMilli()
	errStr := "prior error"
	PopulateRetries(state, []persistence.PendingRetry{
		{
			Entry: persistence.RetryEntry{
				IssueID:    "STARTUP-1",
				Identifier: "STARTUP-1-ident",
				Attempt:    2,
				DueAtMs:    overdueDueMs,
				Error:      &errStr,
			},
			RemainingMs: 0,
		},
	}, discardLogger())

	ReconcileRunningIssues(state, params)

	entry, ok := state.RetryAttempts["STARTUP-1"]
	if !ok {
		t.Fatal("RetryAttempts entry removed for a startup-reconstructed entry, want untouched")
	}
	if entry.TimerHandle != nil {
		t.Error("TimerHandle became non-nil, want still nil (never activated)")
	}
	if entry.DueAtMS != overdueDueMs {
		t.Errorf("DueAtMS = %d, want unchanged %d", entry.DueAtMS, overdueDueMs)
	}
	if handler.countByMessage("overdue retry re-armed") != 0 {
		t.Errorf(`Warn("overdue retry re-armed") emitted %d times, want 0`, handler.countByMessage("overdue retry re-armed"))
	}

	select {
	case id := <-fired:
		t.Errorf("OnRetryFire invoked with %q, want no invocation for a startup-reconstructed entry", id)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestReconcileOverdueRetries_LargeStartupBatchFiresNothing(t *testing.T) {
	t.Parallel()

	store := &overdueRetryStore{}
	tracker := &mockReconcileTracker{}
	fired := make(chan string, 200)
	handler := &sweepLogHandler{}
	params := ReconcileParams{
		TrackerAdapter:    tracker,
		ActiveStates:      []string{"In Progress", "In Review"},
		TerminalStates:    []string{"Done", "Closed"},
		MaxRetryBackoffMS: 300_000,
		Store:             store,
		OnRetryFire:       func(issueID string) { fired <- issueID },
		NowFunc:           func() time.Time { return reconcileBaseTime },
		Ctx:               context.Background(),
		Logger:            slog.New(handler),
	}

	state := NewState(5000, 4, nil, AgentTotals{})
	overdueDueMs := reconcileBaseTime.Add(-5 * time.Minute).UnixMilli()
	entries := make([]persistence.PendingRetry, 0, 200)
	for i := range 200 {
		entries = append(entries, persistence.PendingRetry{
			Entry: persistence.RetryEntry{
				IssueID:    fmt.Sprintf("STARTUP-BATCH-%d", i),
				Identifier: fmt.Sprintf("STARTUP-BATCH-%d-ident", i),
				Attempt:    1,
				DueAtMs:    overdueDueMs,
			},
			RemainingMs: 0,
		})
	}
	PopulateRetries(state, entries, discardLogger())

	ReconcileRunningIssues(state, params)

	if handler.countByMessage("overdue retry re-armed") != 0 {
		t.Errorf(`Warn("overdue retry re-armed") emitted %d times, want 0`, handler.countByMessage("overdue retry re-armed"))
	}

	select {
	case id := <-fired:
		t.Errorf("OnRetryFire invoked with %q, want no invocation for a 200-entry startup batch", id)
	case <-time.After(200 * time.Millisecond):
	}
}
