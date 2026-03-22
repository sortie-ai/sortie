package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
)

// --- Test doubles ---

// mockReconcileStore records calls to ReconcileStore methods and returns
// configurable errors.
type mockReconcileStore struct {
	savedEntries   []persistence.RetryEntry
	deletedIssueID []string

	saveRetryEntryErr   error
	deleteRetryEntryErr error
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

// mockReconcileTracker implements domain.TrackerAdapter for reconcile tests.
// Only FetchIssueStatesByIDs is exercised; the other methods panic if called.
type mockReconcileTracker struct {
	states   map[string]string
	fetchErr error
}

var _ domain.TrackerAdapter = (*mockReconcileTracker)(nil)

func (m *mockReconcileTracker) FetchIssueStatesByIDs(_ context.Context, _ []string) (map[string]string, error) {
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

func (m *mockReconcileTracker) FetchIssueComments(context.Context, string) ([]domain.Comment, error) {
	panic("FetchIssueComments must not be called by ReconcileRunningIssues")
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
