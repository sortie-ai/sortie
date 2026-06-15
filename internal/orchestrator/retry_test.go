package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
)

// --- Test doubles ---

// mockRetryStore records calls to RetryTimerStore methods and returns
// configurable errors.
type mockRetryStore struct {
	savedEntries   []persistence.RetryEntry
	deletedIssueID []string

	saveRetryEntryErr         error
	deleteRetryEntryErr       error
	runHistoryCount           int
	countRunHistoryByIssueErr error
	countedIssueIDs           []string

	tokenSum                 int64
	tokenSessionCount        int
	sumTotalTokensByIssueErr error
	summedTokenIssueIDs      []string

	markDispatchedCalls   int
	markDispatchedIssueID string
	markDispatchedKind    string
	markDispatchedErr     error
}

var _ RetryTimerStore = (*mockRetryStore)(nil)

func (m *mockRetryStore) SaveRetryEntry(_ context.Context, entry persistence.RetryEntry) error {
	m.savedEntries = append(m.savedEntries, entry)
	return m.saveRetryEntryErr
}

func (m *mockRetryStore) DeleteRetryEntry(_ context.Context, issueID string) error {
	m.deletedIssueID = append(m.deletedIssueID, issueID)
	return m.deleteRetryEntryErr
}

func (m *mockRetryStore) CountRunHistoryByIssue(_ context.Context, issueID string) (int, error) {
	m.countedIssueIDs = append(m.countedIssueIDs, issueID)
	return m.runHistoryCount, m.countRunHistoryByIssueErr
}

func (m *mockRetryStore) SumTotalTokensByIssue(_ context.Context, issueID string) (int64, int, error) {
	m.summedTokenIssueIDs = append(m.summedTokenIssueIDs, issueID)
	return m.tokenSum, m.tokenSessionCount, m.sumTotalTokensByIssueErr
}

func (m *mockRetryStore) AppendRunHistory(_ context.Context, run persistence.RunHistory) (persistence.RunHistory, error) {
	return run, nil
}

func (m *mockRetryStore) DeleteReactionFingerprintsByIssue(_ context.Context, _ string) error {
	return nil
}

func (m *mockRetryStore) UpsertReactionFingerprint(_ context.Context, _, _, _ string) error {
	return nil
}

func (m *mockRetryStore) GetReactionFingerprint(_ context.Context, _, _ string) (string, bool, error) {
	return "", false, nil
}

func (m *mockRetryStore) MarkReactionDispatched(_ context.Context, issueID, kind string) error {
	m.markDispatchedCalls++
	m.markDispatchedIssueID = issueID
	m.markDispatchedKind = kind
	return m.markDispatchedErr
}

func (m *mockRetryStore) DeleteReactionFingerprint(_ context.Context, _, _ string) error {
	return nil
}

// mockRetryTracker implements domain.TrackerAdapter for retry timer tests.
// FetchIssueByID is the primary entry point; FetchCandidateIssues panics
// if called — HandleRetryTimer must not invoke it.
type mockRetryTracker struct {
	fetchedIssue domain.Issue
	fetchErr     error
	fetchCount   int
	fetchedID    string // records the last issueID arg received by FetchIssueByID
}

var _ domain.TrackerAdapter = (*mockRetryTracker)(nil)

func (m *mockRetryTracker) FetchCandidateIssues(_ context.Context) ([]domain.Issue, error) {
	panic("FetchCandidateIssues must not be called by HandleRetryTimer")
}

func (m *mockRetryTracker) FetchIssueByID(_ context.Context, issueID string) (domain.Issue, error) {
	m.fetchCount++
	m.fetchedID = issueID
	return m.fetchedIssue, m.fetchErr
}

func (m *mockRetryTracker) FetchIssuesByStates(context.Context, []string) ([]domain.Issue, error) {
	panic("FetchIssuesByStates must not be called by HandleRetryTimer")
}

func (m *mockRetryTracker) FetchIssueStatesByIDs(context.Context, []string) (map[string]string, error) {
	panic("FetchIssueStatesByIDs must not be called by HandleRetryTimer")
}

func (m *mockRetryTracker) FetchIssueStatesByIdentifiers(context.Context, []string) (map[string]string, error) {
	panic("FetchIssueStatesByIdentifiers must not be called by HandleRetryTimer")
}

func (m *mockRetryTracker) FetchIssueComments(context.Context, string) ([]domain.Comment, error) {
	panic("FetchIssueComments must not be called by HandleRetryTimer")
}

func (m *mockRetryTracker) TransitionIssue(context.Context, string, string) error {
	panic("TransitionIssue must not be called by HandleRetryTimer")
}

func (m *mockRetryTracker) CommentIssue(context.Context, string, string) error {
	panic("CommentIssue must not be called by HandleRetryTimer")
}

func (m *mockRetryTracker) AddLabel(context.Context, string, string) error {
	panic("AddLabel must not be called by HandleRetryTimer")
}

// --- Test helpers ---

// retryState creates a *State with a retry entry and claim for the given
// issue. The retry entry has the specified attempt number.
func retryState(t *testing.T, id, identifier string, attempt int) *State {
	t.Helper()
	state := NewState(5000, 4, nil, AgentTotals{})
	state.RetryAttempts[id] = &RetryEntry{
		IssueID:    id,
		Identifier: identifier,
		Attempt:    attempt,
	}
	state.Claimed[id] = struct{}{}
	return state
}

// candidateIssue returns a minimal domain.Issue suitable for retry tests.
func candidateIssue(id, identifier, st string) domain.Issue {
	return domain.Issue{
		ID:         id,
		Identifier: identifier,
		Title:      "title-" + identifier,
		State:      st,
	}
}

// defaultRetryParams returns HandleRetryTimerParams wired with the given
// mocks and a discard logger.
func defaultRetryParams(t *testing.T, store *mockRetryStore, tracker *mockRetryTracker) HandleRetryTimerParams {
	t.Helper()
	return HandleRetryTimerParams{
		Store:             store,
		TrackerAdapter:    tracker,
		ActiveStates:      []string{"To Do", "In Progress"},
		TerminalStates:    []string{"Done"},
		MaxRetryBackoffMS: 300_000,
		MakeWorkerFn: func(_, _, _, _ string, _ domain.AgentAdapter) WorkerFunc {
			return func(_ context.Context, _ domain.Issue, _ *int) {}
		},
		AgentAdapterByKind: func(_ string) (domain.AgentAdapter, error) { return &mockAgentAdapter{}, nil },
		OnRetryFire:        noopRetryFire,
		Ctx:                context.Background(),
		Logger:             discardLogger(),
	}
}

// --- Tests ---

func TestHandleRetryTimer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		issueID string
		// setup
		state   func(t *testing.T, issueID string) *State
		store   func() *mockRetryStore
		tracker func(issueID string) *mockRetryTracker
		// overrides applied after defaultRetryParams
		maxSessions int
		maxTokens   int
		workerFn    func(ch chan<- struct{}) WorkerFunc
		// assertions
		check func(t *testing.T, issueID string, state *State, store *mockRetryStore, tracker *mockRetryTracker, workerCalled bool)
	}{
		{
			name:    "entry missing is no-op",
			issueID: "ISS-1",
			state: func(t *testing.T, _ string) *State {
				t.Helper()
				// No retry entry for the issue — simulates race/cancelled timer.
				return NewState(5000, 4, nil, AgentTotals{})
			},
			store:   func() *mockRetryStore { return &mockRetryStore{} },
			tracker: func(_ string) *mockRetryTracker { return &mockRetryTracker{} },
			check: func(t *testing.T, _ string, _ *State, store *mockRetryStore, tracker *mockRetryTracker, _ bool) {
				t.Helper()
				if tracker.fetchCount != 0 {
					t.Errorf("FetchIssueByID call count = %d, want 0", tracker.fetchCount)
				}
				if len(store.savedEntries) != 0 {
					t.Errorf("SaveRetryEntry call count = %d, want 0", len(store.savedEntries))
				}
				if len(store.deletedIssueID) != 0 {
					t.Errorf("DeleteRetryEntry call count = %d, want 0", len(store.deletedIssueID))
				}
			},
		},
		{
			name:    "stale timer from replaced entry is skipped",
			issueID: "ISS-5",
			state: func(t *testing.T, id string) *State {
				t.Helper()
				state := NewState(5000, 4, nil, AgentTotals{})
				// Simulate a replaced entry: scheduledAt is recent and
				// scheduledDelayMS hasn't elapsed yet (monotonic stale check).
				state.RetryAttempts[id] = &RetryEntry{
					IssueID:          id,
					Identifier:       id,
					Attempt:          2,
					DueAtMS:          time.Now().UnixMilli() + 3_600_000,
					scheduledAt:      time.Now(),
					scheduledDelayMS: 3_600_000, // 1 hour delay, just scheduled
				}
				state.Claimed[id] = struct{}{}
				return state
			},
			store:   func() *mockRetryStore { return &mockRetryStore{} },
			tracker: func(_ string) *mockRetryTracker { return &mockRetryTracker{} },
			check: func(t *testing.T, id string, state *State, store *mockRetryStore, tracker *mockRetryTracker, _ bool) {
				t.Helper()
				// Entry was NOT popped — still in RetryAttempts.
				if _, ok := state.RetryAttempts[id]; !ok {
					t.Errorf("RetryAttempts[%s] missing, want present (stale timer should not pop)", id)
				}
				// No tracker calls.
				if tracker.fetchCount != 0 {
					t.Errorf("FetchIssueByID call count = %d, want 0", tracker.fetchCount)
				}
				// No store calls.
				if len(store.savedEntries) != 0 {
					t.Errorf("SaveRetryEntry call count = %d, want 0", len(store.savedEntries))
				}
				if len(store.deletedIssueID) != 0 {
					t.Errorf("DeleteRetryEntry call count = %d, want 0", len(store.deletedIssueID))
				}
				// Issue stays claimed.
				if _, claimed := state.Claimed[id]; !claimed {
					t.Errorf("Claimed[%s] missing, want claimed", id)
				}
			},
		},
		{
			name:    "startup-reconstructed entry with future DueAtMS proceeds to dispatch",
			issueID: "ISS-7",
			state: func(t *testing.T, id string) *State {
				t.Helper()
				state := NewState(5000, 4, nil, AgentTotals{})
				// Simulate startup recovery: zero scheduledAt, DueAtMS in
				// the future. Old wall-clock code would have treated this as
				// stale and returned early. New code proceeds normally.
				state.RetryAttempts[id] = &RetryEntry{
					IssueID:    id,
					Identifier: id,
					Attempt:    2,
					DueAtMS:    time.Now().UnixMilli() + 3_600_000,
				}
				state.Claimed[id] = struct{}{}
				return state
			},
			store: func() *mockRetryStore { return &mockRetryStore{} },
			tracker: func(id string) *mockRetryTracker {
				return &mockRetryTracker{
					fetchedIssue: candidateIssue(id, id, "To Do"),
				}
			},
			workerFn: func(ch chan<- struct{}) WorkerFunc {
				return func(_ context.Context, _ domain.Issue, _ *int) {
					ch <- struct{}{}
				}
			},
			check: func(t *testing.T, id string, state *State, store *mockRetryStore, tracker *mockRetryTracker, workerCalled bool) {
				t.Helper()
				// Tracker was called — entry was NOT treated as stale.
				if tracker.fetchCount != 1 {
					t.Errorf("FetchIssueByID call count = %d, want 1", tracker.fetchCount)
				}
				// Issue dispatched.
				if _, ok := state.Running[id]; !ok {
					t.Fatalf("Running[%s] missing after dispatch, want present", id)
				}
				if !workerCalled {
					t.Error("worker function not invoked, want invoked")
				}
				// Retry entry cleared.
				if _, ok := state.RetryAttempts[id]; ok {
					t.Errorf("RetryAttempts[%s] still present after dispatch, want cleared", id)
				}
				// DeleteRetryEntry called.
				if len(store.deletedIssueID) != 1 || store.deletedIssueID[0] != id {
					t.Errorf("DeleteRetryEntry calls = %v, want [%s]", store.deletedIssueID, id)
				}
			},
		},
		{
			name:    "fetch failure reschedules with backoff",
			issueID: "ISS-2",
			state: func(t *testing.T, id string) *State {
				t.Helper()
				state := retryState(t, id, id, 2)
				state.RetryAttempts[id].ReactionKind = ReactionKindCI
				return state
			},
			store: func() *mockRetryStore { return &mockRetryStore{} },
			tracker: func(_ string) *mockRetryTracker {
				return &mockRetryTracker{fetchErr: errors.New("connection refused")}
			},
			check: func(t *testing.T, id string, state *State, store *mockRetryStore, _ *mockRetryTracker, _ bool) {
				t.Helper()
				// Retry entry re-created with attempt+1.
				entry, ok := state.RetryAttempts[id]
				if !ok {
					t.Fatalf("RetryAttempts[%s] missing after fetch failure", id)
				}
				if entry.Attempt != 3 {
					t.Errorf("RetryAttempts[%s].Attempt = %d, want 3", id, entry.Attempt)
				}
				if entry.Error != "retry issue fetch failed" {
					t.Errorf("RetryAttempts[%s].Error = %q, want %q", id, entry.Error, "retry issue fetch failed")
				}
				if entry.DueAtMS == 0 {
					t.Errorf("RetryAttempts[%s].DueAtMS = 0, want non-zero", id)
				}
				if entry.TimerHandle == nil {
					t.Errorf("RetryAttempts[%s].TimerHandle = nil, want non-nil", id)
				} else {
					entry.TimerHandle.Stop()
				}
				// ScheduleRetry must set monotonic fields for future stale checks.
				if entry.scheduledAt.IsZero() {
					t.Errorf("RetryAttempts[%s].scheduledAt is zero, want non-zero (set by ScheduleRetry)", id)
				}
				if entry.scheduledDelayMS == 0 {
					t.Errorf("RetryAttempts[%s].scheduledDelayMS = 0, want non-zero backoff delay", id)
				}
				// Issue stays claimed.
				if _, claimed := state.Claimed[id]; !claimed {
					t.Errorf("Claimed[%s] missing after fetch failure, want claimed", id)
				}
				// Known reaction retry reschedules stay runtime-only.
				if len(store.savedEntries) != 0 {
					t.Fatalf("SaveRetryEntry call count = %d, want 0", len(store.savedEntries))
				}
				if len(store.deletedIssueID) != 1 || store.deletedIssueID[0] != id {
					t.Errorf("DeleteRetryEntry calls = %v, want [%s]", store.deletedIssueID, id)
				}
				// ReactionKind preserved across reschedule.
				if entry.ReactionKind != ReactionKindCI {
					t.Errorf("RetryAttempts[%s].ReactionKind = %q, want %q", id, entry.ReactionKind, ReactionKindCI)
				}
			},
		},
		{
			name:    "issue not found releases claim",
			issueID: "ISS-1",
			state: func(t *testing.T, id string) *State {
				t.Helper()
				return retryState(t, id, id, 1)
			},
			store: func() *mockRetryStore { return &mockRetryStore{} },
			tracker: func(_ string) *mockRetryTracker {
				return &mockRetryTracker{
					fetchErr: &domain.TrackerError{Kind: domain.ErrTrackerNotFound, Message: "issue not found: ISS-1"},
				}
			},
			check: func(t *testing.T, id string, state *State, store *mockRetryStore, tracker *mockRetryTracker, _ bool) {
				t.Helper()
				// Claim released.
				if _, claimed := state.Claimed[id]; claimed {
					t.Errorf("Claimed[%s] still present, want released", id)
				}
				// Retry entry removed (popped in step 1, not re-created).
				if _, ok := state.RetryAttempts[id]; ok {
					t.Errorf("RetryAttempts[%s] still present, want removed", id)
				}
				// DeleteRetryEntry called with correct ID.
				if len(store.deletedIssueID) != 1 || store.deletedIssueID[0] != id {
					t.Errorf("DeleteRetryEntry calls = %v, want [%s]", store.deletedIssueID, id)
				}
				// SaveRetryEntry not called.
				if len(store.savedEntries) != 0 {
					t.Errorf("SaveRetryEntry call count = %d, want 0", len(store.savedEntries))
				}
				if tracker.fetchCount != 1 {
					t.Errorf("FetchIssueByID call count = %d, want 1", tracker.fetchCount)
				}
			},
		},
		{
			name:    "no available slots reschedules with backoff",
			issueID: "ISS-1",
			state: func(t *testing.T, id string) *State {
				t.Helper()
				state := retryState(t, id, id, 1)
				state.RetryAttempts[id].ReactionKind = ReactionKindCI
				// Fill the single slot with another running issue.
				state.MaxConcurrentAgents = 1
				state.Running["OTHER-1"] = &RunningEntry{
					Identifier: "OTHER-1",
					Issue:      candidateIssue("OTHER-1", "OTHER-1", "To Do"),
				}
				return state
			},
			store: func() *mockRetryStore { return &mockRetryStore{} },
			tracker: func(id string) *mockRetryTracker {
				return &mockRetryTracker{
					fetchedIssue: candidateIssue(id, id, "To Do"),
				}
			},
			check: func(t *testing.T, id string, state *State, store *mockRetryStore, _ *mockRetryTracker, _ bool) {
				t.Helper()
				// Retry entry re-created at attempt+1.
				entry, ok := state.RetryAttempts[id]
				if !ok {
					t.Fatalf("RetryAttempts[%s] missing after no-slots", id)
				}
				if entry.Attempt != 2 {
					t.Errorf("RetryAttempts[%s].Attempt = %d, want 2", id, entry.Attempt)
				}
				if entry.Error != "no available orchestrator slots" {
					t.Errorf("RetryAttempts[%s].Error = %q, want %q", id, entry.Error, "no available orchestrator slots")
				}
				if entry.TimerHandle != nil {
					entry.TimerHandle.Stop()
				}
				// Issue stays claimed.
				if _, claimed := state.Claimed[id]; !claimed {
					t.Errorf("Claimed[%s] missing, want claimed", id)
				}
				// Running map unchanged — no dispatch occurred.
				if _, running := state.Running[id]; running {
					t.Errorf("Running[%s] present, want absent (no dispatch)", id)
				}
				// Known reaction retry reschedules stay runtime-only.
				if len(store.savedEntries) != 0 {
					t.Fatalf("SaveRetryEntry call count = %d, want 0", len(store.savedEntries))
				}
				if len(store.deletedIssueID) != 1 || store.deletedIssueID[0] != id {
					t.Errorf("DeleteRetryEntry calls = %v, want [%s]", store.deletedIssueID, id)
				}
				// ReactionKind preserved across reschedule.
				if entry.ReactionKind != ReactionKindCI {
					t.Errorf("RetryAttempts[%s].ReactionKind = %q, want %q", id, entry.ReactionKind, ReactionKindCI)
				}
			},
		},
		{
			name:    "eligible with slots dispatches issue",
			issueID: "ISS-1",
			state: func(t *testing.T, id string) *State {
				t.Helper()
				return retryState(t, id, id, 3)
			},
			store: func() *mockRetryStore { return &mockRetryStore{} },
			tracker: func(id string) *mockRetryTracker {
				return &mockRetryTracker{
					fetchedIssue: candidateIssue(id, id, "To Do"),
				}
			},
			workerFn: func(ch chan<- struct{}) WorkerFunc {
				return func(_ context.Context, _ domain.Issue, _ *int) {
					ch <- struct{}{}
				}
			},
			check: func(t *testing.T, id string, state *State, store *mockRetryStore, _ *mockRetryTracker, workerCalled bool) {
				t.Helper()
				// Issue appears in Running map.
				running, ok := state.Running[id]
				if !ok {
					t.Fatalf("Running[%s] missing after dispatch", id)
				}
				if running.RetryAttempt == nil || *running.RetryAttempt != 3 {
					t.Errorf("Running[%s].RetryAttempt = %v, want *3", id, running.RetryAttempt)
				}
				// Issue in Claimed (DispatchIssue sets it).
				if _, claimed := state.Claimed[id]; !claimed {
					t.Errorf("Claimed[%s] missing after dispatch, want claimed", id)
				}
				// Retry entry cleared.
				if _, ok := state.RetryAttempts[id]; ok {
					t.Errorf("RetryAttempts[%s] still present after dispatch, want cleared", id)
				}
				// DeleteRetryEntry called for cleanup.
				if len(store.deletedIssueID) != 1 || store.deletedIssueID[0] != id {
					t.Errorf("DeleteRetryEntry calls = %v, want [%s]", store.deletedIssueID, id)
				}
				// Worker was invoked.
				if !workerCalled {
					t.Error("worker function not invoked, want invoked")
				}
			},
		},
		{
			name:    "continuation retry dispatches with attempt 1",
			issueID: "ISS-1",
			state: func(t *testing.T, id string) *State {
				t.Helper()
				// attempt=1 matches continuation path from normal worker exit.
				return retryState(t, id, id, 1)
			},
			store: func() *mockRetryStore { return &mockRetryStore{} },
			tracker: func(id string) *mockRetryTracker {
				return &mockRetryTracker{
					fetchedIssue: candidateIssue(id, id, "In Progress"),
				}
			},
			workerFn: func(ch chan<- struct{}) WorkerFunc {
				return func(_ context.Context, _ domain.Issue, _ *int) {
					ch <- struct{}{}
				}
			},
			check: func(t *testing.T, id string, state *State, store *mockRetryStore, _ *mockRetryTracker, workerCalled bool) {
				t.Helper()
				running, ok := state.Running[id]
				if !ok {
					t.Fatalf("Running[%s] missing after continuation dispatch", id)
				}
				if running.RetryAttempt == nil || *running.RetryAttempt != 1 {
					t.Errorf("Running[%s].RetryAttempt = %v, want *1", id, running.RetryAttempt)
				}
				if !workerCalled {
					t.Error("worker function not invoked for continuation, want invoked")
				}
				if len(store.deletedIssueID) != 1 {
					t.Errorf("DeleteRetryEntry call count = %d, want 1", len(store.deletedIssueID))
				}
			},
		},
		{
			name:    "SQLite save error on reschedule is non-fatal",
			issueID: "ISS-1",
			state: func(t *testing.T, id string) *State {
				t.Helper()
				return retryState(t, id, id, 2)
			},
			store: func() *mockRetryStore {
				return &mockRetryStore{saveRetryEntryErr: errors.New("disk full")}
			},
			tracker: func(_ string) *mockRetryTracker {
				return &mockRetryTracker{fetchErr: errors.New("timeout")}
			},
			check: func(t *testing.T, id string, state *State, store *mockRetryStore, _ *mockRetryTracker, _ bool) {
				t.Helper()
				// In-memory retry entry still scheduled despite save failure.
				entry, ok := state.RetryAttempts[id]
				if !ok {
					t.Fatalf("RetryAttempts[%s] missing, want present despite save error", id)
				}
				if entry.Attempt != 3 {
					t.Errorf("RetryAttempts[%s].Attempt = %d, want 3", id, entry.Attempt)
				}
				if entry.TimerHandle != nil {
					entry.TimerHandle.Stop()
				}
				// Claim preserved.
				if _, claimed := state.Claimed[id]; !claimed {
					t.Errorf("Claimed[%s] missing, want claimed", id)
				}
				// SaveRetryEntry was attempted.
				if len(store.savedEntries) != 1 {
					t.Errorf("SaveRetryEntry call count = %d, want 1", len(store.savedEntries))
				}
			},
		},
		{
			name:    "SQLite delete error on claim release is non-fatal",
			issueID: "ISS-1",
			state: func(t *testing.T, id string) *State {
				t.Helper()
				return retryState(t, id, id, 1)
			},
			store: func() *mockRetryStore {
				return &mockRetryStore{deleteRetryEntryErr: errors.New("locked")}
			},
			tracker: func(_ string) *mockRetryTracker {
				return &mockRetryTracker{
					fetchErr: &domain.TrackerError{Kind: domain.ErrTrackerNotFound, Message: "issue not found: ISS-1"},
				}
			},
			check: func(t *testing.T, id string, state *State, store *mockRetryStore, _ *mockRetryTracker, _ bool) {
				t.Helper()
				// Claim still released despite delete error.
				if _, claimed := state.Claimed[id]; claimed {
					t.Errorf("Claimed[%s] still present, want released despite delete error", id)
				}
				// DeleteRetryEntry was attempted.
				if len(store.deletedIssueID) != 1 {
					t.Errorf("DeleteRetryEntry call count = %d, want 1", len(store.deletedIssueID))
				}
				// No retry re-created.
				if _, ok := state.RetryAttempts[id]; ok {
					t.Errorf("RetryAttempts[%s] present, want absent", id)
				}
			},
		},
		{
			name:    "non-terminal blocker releases claim",
			issueID: "ISS-3",
			state: func(t *testing.T, id string) *State {
				t.Helper()
				return retryState(t, id, id, 1)
			},
			store: func() *mockRetryStore { return &mockRetryStore{} },
			tracker: func(id string) *mockRetryTracker {
				issue := candidateIssue(id, id, "To Do")
				issue.BlockedBy = []domain.BlockerRef{
					{ID: "BLOCK-1", Identifier: "BLOCK-1", State: "In Progress"},
				}
				return &mockRetryTracker{
					fetchedIssue: issue,
				}
			},
			check: func(t *testing.T, id string, state *State, store *mockRetryStore, _ *mockRetryTracker, _ bool) {
				t.Helper()
				// Claim released.
				if _, claimed := state.Claimed[id]; claimed {
					t.Errorf("Claimed[%s] still present, want released due to blocker", id)
				}
				// Retry entry removed (popped, not re-created).
				if _, ok := state.RetryAttempts[id]; ok {
					t.Errorf("RetryAttempts[%s] still present, want removed", id)
				}
				// Not dispatched.
				if _, running := state.Running[id]; running {
					t.Errorf("Running[%s] present, want absent (blocked issue should not dispatch)", id)
				}
				// DeleteRetryEntry called.
				if len(store.deletedIssueID) != 1 || store.deletedIssueID[0] != id {
					t.Errorf("DeleteRetryEntry calls = %v, want [%s]", store.deletedIssueID, id)
				}
				// No save (no reschedule).
				if len(store.savedEntries) != 0 {
					t.Errorf("SaveRetryEntry call count = %d, want 0", len(store.savedEntries))
				}
			},
		},
		{
			name:    "missing required field releases claim",
			issueID: "ISS-4",
			state: func(t *testing.T, id string) *State {
				t.Helper()
				return retryState(t, id, id, 2)
			},
			store: func() *mockRetryStore { return &mockRetryStore{} },
			tracker: func(id string) *mockRetryTracker {
				// Issue has empty Title — fails required field check.
				return &mockRetryTracker{
					fetchedIssue: domain.Issue{ID: id, Identifier: id, Title: "", State: "To Do"},
				}
			},
			check: func(t *testing.T, id string, state *State, store *mockRetryStore, _ *mockRetryTracker, _ bool) {
				t.Helper()
				// Claim released.
				if _, claimed := state.Claimed[id]; claimed {
					t.Errorf("Claimed[%s] still present, want released due to missing title", id)
				}
				// Retry entry removed.
				if _, ok := state.RetryAttempts[id]; ok {
					t.Errorf("RetryAttempts[%s] still present, want removed", id)
				}
				// Not dispatched.
				if _, running := state.Running[id]; running {
					t.Errorf("Running[%s] present, want absent (ineligible issue should not dispatch)", id)
				}
				// DeleteRetryEntry called.
				if len(store.deletedIssueID) != 1 || store.deletedIssueID[0] != id {
					t.Errorf("DeleteRetryEntry calls = %v, want [%s]", store.deletedIssueID, id)
				}
			},
		},
		{
			name:    "terminal state releases claim",
			issueID: "ISS-6",
			state: func(t *testing.T, id string) *State {
				t.Helper()
				return retryState(t, id, id, 1)
			},
			store: func() *mockRetryStore { return &mockRetryStore{} },
			tracker: func(id string) *mockRetryTracker {
				// Issue is in terminal state "Done" — rejected by active-state check.
				return &mockRetryTracker{
					fetchedIssue: candidateIssue(id, id, "Done"),
				}
			},
			check: func(t *testing.T, id string, state *State, store *mockRetryStore, _ *mockRetryTracker, _ bool) {
				t.Helper()
				// Claim released.
				if _, claimed := state.Claimed[id]; claimed {
					t.Errorf("Claimed[%s] still present, want released due to terminal state", id)
				}
				// Retry entry removed.
				if _, ok := state.RetryAttempts[id]; ok {
					t.Errorf("RetryAttempts[%s] still present, want removed", id)
				}
				// Not dispatched.
				if _, running := state.Running[id]; running {
					t.Errorf("Running[%s] present, want absent (terminal issue should not dispatch)", id)
				}
				// DeleteRetryEntry called.
				if len(store.deletedIssueID) != 1 || store.deletedIssueID[0] != id {
					t.Errorf("DeleteRetryEntry calls = %v, want [%s]", store.deletedIssueID, id)
				}
				// No save (no reschedule).
				if len(store.savedEntries) != 0 {
					t.Errorf("SaveRetryEntry call count = %d, want 0", len(store.savedEntries))
				}
			},
		},
		{
			name:        "budget exhausted blocks dispatch",
			issueID:     "ISS-BUDGET",
			maxSessions: 3,
			state: func(t *testing.T, id string) *State {
				t.Helper()
				return retryState(t, id, "PROJ-BUDGET", 2)
			},
			store: func() *mockRetryStore {
				return &mockRetryStore{runHistoryCount: 3}
			},
			tracker: func(_ string) *mockRetryTracker {
				return &mockRetryTracker{}
			},
			check: func(t *testing.T, id string, state *State, store *mockRetryStore, tracker *mockRetryTracker, _ bool) {
				t.Helper()
				// Claim released.
				if _, claimed := state.Claimed[id]; claimed {
					t.Errorf("Claimed[%s] still present, want released (budget exhausted)", id)
				}
				// Not dispatched.
				if _, running := state.Running[id]; running {
					t.Errorf("Running[%s] present, want absent (budget exhausted)", id)
				}
				// BudgetExhausted set must contain this issue.
				if _, exhausted := state.BudgetExhausted[id]; !exhausted {
					t.Errorf("BudgetExhausted[%s] missing, want present after budget exhaustion", id)
				}
				// Tracker never called — budget check runs before fetch.
				if tracker.fetchCount != 0 {
					t.Errorf("FetchIssueByID call count = %d, want 0", tracker.fetchCount)
				}
				// DeleteRetryEntry called.
				if len(store.deletedIssueID) != 1 || store.deletedIssueID[0] != id {
					t.Errorf("DeleteRetryEntry calls = %v, want [%s]", store.deletedIssueID, id)
				}
				// CountRunHistoryByIssue was called with correct ID.
				if len(store.countedIssueIDs) != 1 || store.countedIssueIDs[0] != id {
					t.Errorf("CountRunHistoryByIssue calls = %v, want [%s]", store.countedIssueIDs, id)
				}
			},
		},
		{
			name:        "budget not exhausted allows dispatch",
			issueID:     "ISS-UNDER",
			maxSessions: 3,
			state: func(t *testing.T, id string) *State {
				t.Helper()
				return retryState(t, id, "PROJ-UNDER", 1)
			},
			store: func() *mockRetryStore {
				return &mockRetryStore{runHistoryCount: 2}
			},
			tracker: func(id string) *mockRetryTracker {
				return &mockRetryTracker{
					fetchedIssue: candidateIssue(id, "PROJ-UNDER", "To Do"),
				}
			},
			workerFn: func(ch chan<- struct{}) WorkerFunc {
				return func(_ context.Context, _ domain.Issue, _ *int) {
					ch <- struct{}{}
				}
			},
			check: func(t *testing.T, id string, state *State, store *mockRetryStore, tracker *mockRetryTracker, workerCalled bool) {
				t.Helper()
				// Tracker called.
				if tracker.fetchCount != 1 {
					t.Errorf("FetchIssueByID call count = %d, want 1", tracker.fetchCount)
				}
				// Issue dispatched.
				if _, ok := state.Running[id]; !ok {
					t.Fatalf("Running[%s] missing after dispatch, want present", id)
				}
				if !workerCalled {
					t.Error("worker function not invoked, want invoked")
				}
				// Budget was checked.
				if len(store.countedIssueIDs) != 1 || store.countedIssueIDs[0] != id {
					t.Errorf("CountRunHistoryByIssue calls = %v, want [%s]", store.countedIssueIDs, id)
				}
			},
		},
		{
			name:    "max_sessions zero skips budget check",
			issueID: "ISS-NOLIMIT",
			state: func(t *testing.T, id string) *State {
				t.Helper()
				return retryState(t, id, "PROJ-NOLIMIT", 1)
			},
			store: func() *mockRetryStore {
				return &mockRetryStore{runHistoryCount: 999}
			},
			tracker: func(id string) *mockRetryTracker {
				return &mockRetryTracker{
					fetchedIssue: candidateIssue(id, "PROJ-NOLIMIT", "To Do"),
				}
			},
			workerFn: func(ch chan<- struct{}) WorkerFunc {
				return func(_ context.Context, _ domain.Issue, _ *int) {
					ch <- struct{}{}
				}
			},
			check: func(t *testing.T, id string, state *State, store *mockRetryStore, tracker *mockRetryTracker, workerCalled bool) {
				t.Helper()
				// CountRunHistoryByIssue never called — MaxSessions is 0.
				if len(store.countedIssueIDs) != 0 {
					t.Errorf("CountRunHistoryByIssue calls = %v, want empty (MaxSessions=0)", store.countedIssueIDs)
				}
				// Tracker called.
				if tracker.fetchCount != 1 {
					t.Errorf("FetchIssueByID call count = %d, want 1", tracker.fetchCount)
				}
				// Issue dispatched.
				if _, ok := state.Running[id]; !ok {
					t.Fatalf("Running[%s] missing after dispatch, want present", id)
				}
				if !workerCalled {
					t.Error("worker function not invoked, want invoked")
				}
			},
		},
		{
			name:        "budget count store error is fail-open",
			issueID:     "ISS-FAIL",
			maxSessions: 3,
			state: func(t *testing.T, id string) *State {
				t.Helper()
				return retryState(t, id, "PROJ-FAIL", 1)
			},
			store: func() *mockRetryStore {
				return &mockRetryStore{
					countRunHistoryByIssueErr: errors.New("database locked"),
				}
			},
			tracker: func(id string) *mockRetryTracker {
				return &mockRetryTracker{
					fetchedIssue: candidateIssue(id, "PROJ-FAIL", "To Do"),
				}
			},
			workerFn: func(ch chan<- struct{}) WorkerFunc {
				return func(_ context.Context, _ domain.Issue, _ *int) {
					ch <- struct{}{}
				}
			},
			check: func(t *testing.T, id string, state *State, store *mockRetryStore, tracker *mockRetryTracker, workerCalled bool) {
				t.Helper()
				// Count was attempted.
				if len(store.countedIssueIDs) != 1 || store.countedIssueIDs[0] != id {
					t.Errorf("CountRunHistoryByIssue calls = %v, want [%s]", store.countedIssueIDs, id)
				}
				// Tracker called — fail-open.
				if tracker.fetchCount != 1 {
					t.Errorf("FetchIssueByID call count = %d, want 1 (fail-open)", tracker.fetchCount)
				}
				// Issue dispatched despite count error.
				if _, ok := state.Running[id]; !ok {
					t.Fatalf("Running[%s] missing after dispatch, want present (fail-open)", id)
				}
				if !workerCalled {
					t.Error("worker function not invoked, want invoked (fail-open)")
				}
				// Claim preserved (issue is running).
				if _, claimed := state.Claimed[id]; !claimed {
					t.Errorf("Claimed[%s] missing, want claimed", id)
				}
			},
		},
		{
			name:      "token budget exhausted blocks dispatch and records reason",
			issueID:   "ISS-TOK",
			maxTokens: 1000,
			state: func(t *testing.T, id string) *State {
				t.Helper()
				return retryState(t, id, "PROJ-TOK", 2)
			},
			store: func() *mockRetryStore {
				return &mockRetryStore{tokenSum: 1000, tokenSessionCount: 2}
			},
			tracker: func(_ string) *mockRetryTracker {
				return &mockRetryTracker{}
			},
			check: func(t *testing.T, id string, state *State, store *mockRetryStore, tracker *mockRetryTracker, _ bool) {
				t.Helper()
				// Claim released.
				if _, claimed := state.Claimed[id]; claimed {
					t.Errorf("Claimed[%s] still present, want released (token budget exhausted)", id)
				}
				// Not dispatched.
				if _, running := state.Running[id]; running {
					t.Errorf("Running[%s] present, want absent (token budget exhausted)", id)
				}
				// BudgetExhausted set must contain this issue with the token reason.
				if _, exhausted := state.BudgetExhausted[id]; !exhausted {
					t.Errorf("BudgetExhausted[%s] missing, want present after token budget exhaustion", id)
				}
				if got := state.BudgetExhaustedReason[id]; got != budgetReasonToken {
					t.Errorf("BudgetExhaustedReason[%s] = %q, want %q", id, got, budgetReasonToken)
				}
				// The dispatcher gate must refuse the issue.
				if ShouldDispatch(candidateIssue(id, "PROJ-TOK", "To Do"), state, []string{"To Do", "In Progress"}, []string{"Done"}) {
					t.Errorf("ShouldDispatch(%s) = true, want false (token budget exhausted)", id)
				}
				// Tracker never called — budget checks run before fetch.
				if tracker.fetchCount != 0 {
					t.Errorf("FetchIssueByID call count = %d, want 0", tracker.fetchCount)
				}
				// DeleteRetryEntry called.
				if len(store.deletedIssueID) != 1 || store.deletedIssueID[0] != id {
					t.Errorf("DeleteRetryEntry calls = %v, want [%s]", store.deletedIssueID, id)
				}
				// SumTotalTokensByIssue was called with the correct ID.
				if len(store.summedTokenIssueIDs) != 1 || store.summedTokenIssueIDs[0] != id {
					t.Errorf("SumTotalTokensByIssue calls = %v, want [%s]", store.summedTokenIssueIDs, id)
				}
			},
		},
		{
			name:      "token budget under limit allows dispatch",
			issueID:   "ISS-TOK-UNDER",
			maxTokens: 1000,
			state: func(t *testing.T, id string) *State {
				t.Helper()
				return retryState(t, id, "PROJ-TOK-UNDER", 1)
			},
			store: func() *mockRetryStore {
				return &mockRetryStore{tokenSum: 999, tokenSessionCount: 3}
			},
			tracker: func(id string) *mockRetryTracker {
				return &mockRetryTracker{
					fetchedIssue: candidateIssue(id, "PROJ-TOK-UNDER", "To Do"),
				}
			},
			workerFn: func(ch chan<- struct{}) WorkerFunc {
				return func(_ context.Context, _ domain.Issue, _ *int) {
					ch <- struct{}{}
				}
			},
			check: func(t *testing.T, id string, state *State, store *mockRetryStore, tracker *mockRetryTracker, workerCalled bool) {
				t.Helper()
				// Budget was checked.
				if len(store.summedTokenIssueIDs) != 1 || store.summedTokenIssueIDs[0] != id {
					t.Errorf("SumTotalTokensByIssue calls = %v, want [%s]", store.summedTokenIssueIDs, id)
				}
				// Tracker called and issue dispatched.
				if tracker.fetchCount != 1 {
					t.Errorf("FetchIssueByID call count = %d, want 1", tracker.fetchCount)
				}
				if _, ok := state.Running[id]; !ok {
					t.Fatalf("Running[%s] missing after dispatch, want present", id)
				}
				if !workerCalled {
					t.Error("worker function not invoked, want invoked")
				}
				// No reason recorded for a dispatched issue.
				if got, ok := state.BudgetExhaustedReason[id]; ok {
					t.Errorf("BudgetExhaustedReason[%s] = %q, want absent", id, got)
				}
			},
		},
		{
			name:    "max_tokens zero skips token check",
			issueID: "ISS-TOK-NOLIMIT",
			state: func(t *testing.T, id string) *State {
				t.Helper()
				return retryState(t, id, "PROJ-TOK-NOLIMIT", 1)
			},
			store: func() *mockRetryStore {
				return &mockRetryStore{tokenSum: 99999}
			},
			tracker: func(id string) *mockRetryTracker {
				return &mockRetryTracker{
					fetchedIssue: candidateIssue(id, "PROJ-TOK-NOLIMIT", "To Do"),
				}
			},
			workerFn: func(ch chan<- struct{}) WorkerFunc {
				return func(_ context.Context, _ domain.Issue, _ *int) {
					ch <- struct{}{}
				}
			},
			check: func(t *testing.T, id string, state *State, store *mockRetryStore, tracker *mockRetryTracker, workerCalled bool) {
				t.Helper()
				// SumTotalTokensByIssue never called — MaxTokens is 0.
				if len(store.summedTokenIssueIDs) != 0 {
					t.Errorf("SumTotalTokensByIssue calls = %v, want empty (MaxTokens=0)", store.summedTokenIssueIDs)
				}
				if tracker.fetchCount != 1 {
					t.Errorf("FetchIssueByID call count = %d, want 1", tracker.fetchCount)
				}
				if _, ok := state.Running[id]; !ok {
					t.Fatalf("Running[%s] missing after dispatch, want present", id)
				}
				if !workerCalled {
					t.Error("worker function not invoked, want invoked")
				}
			},
		},
		{
			name:      "token sum store error is fail-open",
			issueID:   "ISS-TOK-FAIL",
			maxTokens: 1000,
			state: func(t *testing.T, id string) *State {
				t.Helper()
				return retryState(t, id, "PROJ-TOK-FAIL", 1)
			},
			store: func() *mockRetryStore {
				return &mockRetryStore{
					tokenSum:                 99999,
					sumTotalTokensByIssueErr: errors.New("database locked"),
				}
			},
			tracker: func(id string) *mockRetryTracker {
				return &mockRetryTracker{
					fetchedIssue: candidateIssue(id, "PROJ-TOK-FAIL", "To Do"),
				}
			},
			workerFn: func(ch chan<- struct{}) WorkerFunc {
				return func(_ context.Context, _ domain.Issue, _ *int) {
					ch <- struct{}{}
				}
			},
			check: func(t *testing.T, id string, state *State, store *mockRetryStore, tracker *mockRetryTracker, workerCalled bool) {
				t.Helper()
				// Sum was attempted.
				if len(store.summedTokenIssueIDs) != 1 || store.summedTokenIssueIDs[0] != id {
					t.Errorf("SumTotalTokensByIssue calls = %v, want [%s]", store.summedTokenIssueIDs, id)
				}
				// Tracker called — fail-open.
				if tracker.fetchCount != 1 {
					t.Errorf("FetchIssueByID call count = %d, want 1 (fail-open)", tracker.fetchCount)
				}
				// Issue dispatched despite sum error — never stranded.
				if _, ok := state.Running[id]; !ok {
					t.Fatalf("Running[%s] missing after dispatch, want present (fail-open)", id)
				}
				if !workerCalled {
					t.Error("worker function not invoked, want invoked (fail-open)")
				}
				if _, exhausted := state.BudgetExhausted[id]; exhausted {
					t.Errorf("BudgetExhausted[%s] present after fail-open, want absent", id)
				}
				// Claim preserved (issue is running).
				if _, claimed := state.Claimed[id]; !claimed {
					t.Errorf("Claimed[%s] missing, want claimed", id)
				}
			},
		},
		{
			name:        "both budgets exhausted records token budget reason",
			issueID:     "ISS-BOTH",
			maxSessions: 3,
			maxTokens:   1000,
			state: func(t *testing.T, id string) *State {
				t.Helper()
				return retryState(t, id, "PROJ-BOTH", 2)
			},
			store: func() *mockRetryStore {
				return &mockRetryStore{
					runHistoryCount:   3,
					tokenSum:          1500,
					tokenSessionCount: 3,
				}
			},
			tracker: func(_ string) *mockRetryTracker {
				return &mockRetryTracker{}
			},
			check: func(t *testing.T, id string, state *State, store *mockRetryStore, tracker *mockRetryTracker, _ bool) {
				t.Helper()
				// Blocked either way: claim released, not dispatched.
				if _, claimed := state.Claimed[id]; claimed {
					t.Errorf("Claimed[%s] still present, want released (both budgets exhausted)", id)
				}
				if tracker.fetchCount != 0 {
					t.Errorf("FetchIssueByID call count = %d, want 0", tracker.fetchCount)
				}
				if _, exhausted := state.BudgetExhausted[id]; !exhausted {
					t.Errorf("BudgetExhausted[%s] missing, want present after exhaustion", id)
				}
				// A single evaluation that finds both ceilings exhausted must
				// report the token budget.
				if got := state.BudgetExhaustedReason[id]; got != budgetReasonToken {
					t.Errorf("BudgetExhaustedReason[%s] = %q, want %q (token precedence)", id, got, budgetReasonToken)
				}
			},
		},
		{
			name:        "both budgets exhausted with token sum error keeps session reason",
			issueID:     "ISS-BOTH-TOK-FAIL",
			maxSessions: 3,
			maxTokens:   1000,
			state: func(t *testing.T, id string) *State {
				t.Helper()
				return retryState(t, id, "PROJ-BOTH-TOK-FAIL", 2)
			},
			store: func() *mockRetryStore {
				return &mockRetryStore{
					runHistoryCount:          3,
					tokenSum:                 1500,
					tokenSessionCount:        3,
					sumTotalTokensByIssueErr: errors.New("database locked"),
				}
			},
			tracker: func(_ string) *mockRetryTracker {
				return &mockRetryTracker{}
			},
			check: func(t *testing.T, id string, state *State, store *mockRetryStore, tracker *mockRetryTracker, _ bool) {
				t.Helper()
				// The session ceiling's block holds even though the token
				// axis failed open: claim released, issue not dispatched.
				if _, claimed := state.Claimed[id]; claimed {
					t.Errorf("Claimed[%s] still present, want released (session budget exhausted)", id)
				}
				if _, exhausted := state.BudgetExhausted[id]; !exhausted {
					t.Errorf("BudgetExhausted[%s] missing, want present after session exhaustion", id)
				}
				// The errored token gate must not overwrite the session reason.
				if got := state.BudgetExhaustedReason[id]; got != budgetReasonSession {
					t.Errorf("BudgetExhaustedReason[%s] = %q, want %q (token axis failed open)", id, got, budgetReasonSession)
				}
				// Token sum was attempted before failing open.
				if len(store.summedTokenIssueIDs) != 1 || store.summedTokenIssueIDs[0] != id {
					t.Errorf("SumTotalTokensByIssue calls = %v, want [%s]", store.summedTokenIssueIDs, id)
				}
				// Tracker never called — the session block short-circuits the fetch.
				if tracker.fetchCount != 0 {
					t.Errorf("FetchIssueByID call count = %d, want 0", tracker.fetchCount)
				}
				// DeleteRetryEntry called exactly once, by the session block.
				if len(store.deletedIssueID) != 1 || store.deletedIssueID[0] != id {
					t.Errorf("DeleteRetryEntry calls = %v, want [%s]", store.deletedIssueID, id)
				}
			},
		},
		{
			name:    "ErrTrackerNotFound releases claim",
			issueID: "ISS-X",
			state: func(t *testing.T, id string) *State {
				t.Helper()
				return retryState(t, id, id, 1)
			},
			store: func() *mockRetryStore { return &mockRetryStore{} },
			tracker: func(_ string) *mockRetryTracker {
				return &mockRetryTracker{
					fetchErr: &domain.TrackerError{Kind: domain.ErrTrackerNotFound, Message: "issue not found: ISS-X"},
				}
			},
			check: func(t *testing.T, id string, state *State, store *mockRetryStore, tracker *mockRetryTracker, _ bool) {
				t.Helper()
				if _, claimed := state.Claimed[id]; claimed {
					t.Errorf("Claimed[%s] still present, want released", id)
				}
				if _, ok := state.RetryAttempts[id]; ok {
					t.Errorf("RetryAttempts[%s] still present, want removed", id)
				}
				if len(store.deletedIssueID) != 1 || store.deletedIssueID[0] != id {
					t.Errorf("DeleteRetryEntry calls = %v, want [%s]", store.deletedIssueID, id)
				}
				if len(store.savedEntries) != 0 {
					t.Errorf("SaveRetryEntry call count = %d, want 0", len(store.savedEntries))
				}
				if tracker.fetchCount != 1 {
					t.Errorf("FetchIssueByID call count = %d, want 1", tracker.fetchCount)
				}
			},
		},
		{
			name:    "non-NotFound tracker error reschedules with backoff",
			issueID: "ISS-TRANSPORT",
			state: func(t *testing.T, id string) *State {
				t.Helper()
				return retryState(t, id, id, 1)
			},
			store: func() *mockRetryStore { return &mockRetryStore{} },
			tracker: func(_ string) *mockRetryTracker {
				return &mockRetryTracker{
					fetchErr: &domain.TrackerError{Kind: domain.ErrTrackerTransport, Message: "timeout"},
				}
			},
			check: func(t *testing.T, id string, state *State, store *mockRetryStore, tracker *mockRetryTracker, _ bool) {
				t.Helper()
				entry, ok := state.RetryAttempts[id]
				if !ok {
					t.Fatalf("RetryAttempts[%s] missing after transport error", id)
				}
				if entry.Attempt != 2 {
					t.Errorf("RetryAttempts[%s].Attempt = %d, want 2", id, entry.Attempt)
				}
				if entry.Error != "retry issue fetch failed" {
					t.Errorf("RetryAttempts[%s].Error = %q, want %q", id, entry.Error, "retry issue fetch failed")
				}
				if _, claimed := state.Claimed[id]; !claimed {
					t.Errorf("Claimed[%s] missing after transport error, want claimed", id)
				}
				if len(store.savedEntries) != 1 {
					t.Fatalf("SaveRetryEntry call count = %d, want 1", len(store.savedEntries))
				}
				if len(store.deletedIssueID) != 0 {
					t.Errorf("DeleteRetryEntry call count = %d, want 0", len(store.deletedIssueID))
				}
				if tracker.fetchCount != 1 {
					t.Errorf("FetchIssueByID call count = %d, want 1", tracker.fetchCount)
				}
				if entry.TimerHandle != nil {
					entry.TimerHandle.Stop()
				}
			},
		},
		{
			name:    "issue in non-active state releases claim",
			issueID: "ISS-INACTIVE",
			state: func(t *testing.T, id string) *State {
				t.Helper()
				return retryState(t, id, id, 1)
			},
			store: func() *mockRetryStore { return &mockRetryStore{} },
			tracker: func(id string) *mockRetryTracker {
				return &mockRetryTracker{
					fetchedIssue: candidateIssue(id, id, "Done"),
				}
			},
			check: func(t *testing.T, id string, state *State, store *mockRetryStore, _ *mockRetryTracker, _ bool) {
				t.Helper()
				if _, claimed := state.Claimed[id]; claimed {
					t.Errorf("Claimed[%s] still present, want released (non-active state)", id)
				}
				if _, ok := state.RetryAttempts[id]; ok {
					t.Errorf("RetryAttempts[%s] still present, want removed", id)
				}
				if len(store.deletedIssueID) != 1 || store.deletedIssueID[0] != id {
					t.Errorf("DeleteRetryEntry calls = %v, want [%s]", store.deletedIssueID, id)
				}
				if len(store.savedEntries) != 0 {
					t.Errorf("SaveRetryEntry call count = %d, want 0", len(store.savedEntries))
				}
			},
		},
		{
			name:    "issue in active state proceeds to dispatch",
			issueID: "ISS-ACTIVE",
			state: func(t *testing.T, id string) *State {
				t.Helper()
				return retryState(t, id, id, 1)
			},
			store: func() *mockRetryStore { return &mockRetryStore{} },
			tracker: func(id string) *mockRetryTracker {
				return &mockRetryTracker{
					fetchedIssue: candidateIssue(id, id, "In Progress"),
				}
			},
			workerFn: func(ch chan<- struct{}) WorkerFunc {
				return func(_ context.Context, _ domain.Issue, _ *int) {
					ch <- struct{}{}
				}
			},
			check: func(t *testing.T, id string, state *State, store *mockRetryStore, tracker *mockRetryTracker, workerCalled bool) {
				t.Helper()
				if _, ok := state.Running[id]; !ok {
					t.Fatalf("Running[%s] missing after dispatch, want present", id)
				}
				if !workerCalled {
					t.Error("worker function not invoked, want invoked")
				}
				if _, ok := state.RetryAttempts[id]; ok {
					t.Errorf("RetryAttempts[%s] still present after dispatch, want cleared", id)
				}
				if len(store.deletedIssueID) != 1 || store.deletedIssueID[0] != id {
					t.Errorf("DeleteRetryEntry calls = %v, want [%s]", store.deletedIssueID, id)
				}
				if tracker.fetchCount != 1 {
					t.Errorf("FetchIssueByID call count = %d, want 1", tracker.fetchCount)
				}
			},
		},
		{
			name:    "FetchIssueByID called with correct issueID",
			issueID: "ISS-IDCHECK",
			state: func(t *testing.T, id string) *State {
				t.Helper()
				return retryState(t, id, id, 1)
			},
			store: func() *mockRetryStore { return &mockRetryStore{} },
			tracker: func(id string) *mockRetryTracker {
				return &mockRetryTracker{
					fetchedIssue: candidateIssue(id, id, "To Do"),
				}
			},
			workerFn: func(ch chan<- struct{}) WorkerFunc {
				return func(_ context.Context, _ domain.Issue, _ *int) {
					ch <- struct{}{}
				}
			},
			check: func(t *testing.T, id string, _ *State, _ *mockRetryStore, tracker *mockRetryTracker, _ bool) {
				t.Helper()
				if tracker.fetchedID != id {
					t.Errorf("FetchIssueByID issueID = %q, want %q", tracker.fetchedID, id)
				}
			},
		},
		{
			name:    "context cancellation error reschedules",
			issueID: "ISS-CTX",
			state: func(t *testing.T, id string) *State {
				t.Helper()
				return retryState(t, id, id, 1)
			},
			store: func() *mockRetryStore { return &mockRetryStore{} },
			tracker: func(_ string) *mockRetryTracker {
				return &mockRetryTracker{
					fetchErr: context.Canceled,
				}
			},
			check: func(t *testing.T, id string, state *State, store *mockRetryStore, tracker *mockRetryTracker, _ bool) {
				t.Helper()
				entry, ok := state.RetryAttempts[id]
				if !ok {
					t.Fatalf("RetryAttempts[%s] missing after context cancellation", id)
				}
				if entry.Attempt != 2 {
					t.Errorf("RetryAttempts[%s].Attempt = %d, want 2", id, entry.Attempt)
				}
				if entry.Error != "retry issue fetch failed" {
					t.Errorf("RetryAttempts[%s].Error = %q, want %q", id, entry.Error, "retry issue fetch failed")
				}
				if tracker.fetchCount != 1 {
					t.Errorf("FetchIssueByID call count = %d, want 1", tracker.fetchCount)
				}
				if entry.TimerHandle != nil {
					entry.TimerHandle.Stop()
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			id := tt.issueID
			state := tt.state(t, id)
			store := tt.store()
			tracker := tt.tracker(id)
			params := defaultRetryParams(t, store, tracker)
			params.MaxSessions = tt.maxSessions
			params.MaxTokens = tt.maxTokens

			var workerCalled bool
			if tt.workerFn != nil {
				ch := make(chan struct{}, 1)
				wf := tt.workerFn(ch)
				params.MakeWorkerFn = func(_, _, _, _ string, _ domain.AgentAdapter) WorkerFunc { return wf }
				HandleRetryTimer(state, id, params)
				select {
				case <-ch:
					workerCalled = true
				case <-time.After(time.Second):
					t.Fatal("worker goroutine did not execute within 1 second")
				}
			} else {
				HandleRetryTimer(state, id, params)
			}

			tt.check(t, id, state, store, tracker, workerCalled)
		})
	}
}

// TestHandleRetryTimer_TokenBudgetLogLine asserts the structured refusal
// log line carries the typed attributes the token gate emits.
func TestHandleRetryTimer_TokenBudgetLogLine(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	store := &mockRetryStore{tokenSum: 1500, tokenSessionCount: 2}
	tracker := &mockRetryTracker{}
	state := retryState(t, "ISS-TOK-LOG", "PROJ-TOK-LOG", 1)

	params := defaultRetryParams(t, store, tracker)
	params.MaxTokens = 1000
	params.Logger = logger

	HandleRetryTimer(state, "ISS-TOK-LOG", params)

	output := buf.String()
	if !strings.Contains(output, "token budget exhausted, blocking re-dispatch") {
		t.Fatalf("log output = %q, want to contain the token refusal message", output)
	}
	for _, attr := range []string{
		"reason=token_budget",
		"used_tokens=1500",
		"budget_tokens=1000",
		"used_sessions=2",
		"budget_sessions=0",
		"issue_id=ISS-TOK-LOG",
	} {
		if !strings.Contains(output, attr) {
			t.Errorf("log output missing attribute %q: %q", attr, output)
		}
	}
}

// TestHandleRetryTimer_TokenBudgetFailOpenLogsWarning asserts the fail-open
// path logs a warning instead of stranding the issue silently.
func TestHandleRetryTimer_TokenBudgetFailOpenLogsWarning(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	store := &mockRetryStore{sumTotalTokensByIssueErr: errors.New("database locked")}
	tracker := &mockRetryTracker{
		fetchedIssue: candidateIssue("ISS-TOK-WARN", "PROJ-TOK-WARN", "To Do"),
	}
	state := retryState(t, "ISS-TOK-WARN", "PROJ-TOK-WARN", 1)

	dispatched := make(chan struct{}, 1)
	params := defaultRetryParams(t, store, tracker)
	params.MaxTokens = 1000
	params.Logger = logger
	params.MakeWorkerFn = func(_, _, _, _ string, _ domain.AgentAdapter) WorkerFunc {
		return func(_ context.Context, _ domain.Issue, _ *int) {
			dispatched <- struct{}{}
		}
	}

	HandleRetryTimer(state, "ISS-TOK-WARN", params)

	select {
	case <-dispatched:
	case <-time.After(time.Second):
		t.Fatal("worker not dispatched within 1 second (fail-open must dispatch)")
	}

	output := buf.String()
	if !strings.Contains(output, "token budget check failed, proceeding with dispatch") {
		t.Errorf("log output = %q, want to contain the fail-open warning", output)
	}
	if !strings.Contains(output, "error=") {
		t.Errorf("log output missing error attribute: %q", output)
	}
}

func TestHandleRetryTimer_WorkerStillRunningReschedulesInsteadOfDispatching(t *testing.T) {
	t.Parallel()

	store := &mockRetryStore{}
	tracker := &mockRetryTracker{}

	state := retryState(t, "ISS-1", "ISS-1", 2)
	// Place the issue in Running to simulate a cancelled-but-not-yet-exited worker.
	state.Running["ISS-1"] = &RunningEntry{
		Identifier: "ISS-1",
		Issue:      candidateIssue("ISS-1", "ISS-1", "In Progress"),
		StartedAt:  time.Now().UTC(),
	}

	params := defaultRetryParams(t, store, tracker)

	workerCalled := false
	params.MakeWorkerFn = func(_, _, _, _ string, _ domain.AgentAdapter) WorkerFunc {
		return func(_ context.Context, _ domain.Issue, _ *int) {
			workerCalled = true
		}
	}

	HandleRetryTimer(state, "ISS-1", params)

	// Worker must NOT have been dispatched.
	if workerCalled {
		t.Error("worker dispatched while issue still in Running, want no dispatch")
	}

	// FetchIssueByID should not be called — guard returns early.
	if tracker.fetchCount != 0 {
		t.Errorf("FetchIssueByID call count = %d, want 0", tracker.fetchCount)
	}

	// Retry entry rescheduled with same attempt number.
	entry, ok := state.RetryAttempts["ISS-1"]
	if !ok {
		t.Fatal("RetryAttempts[ISS-1] missing, want rescheduled")
	}
	if entry.Attempt != 2 {
		t.Errorf("rescheduled Attempt = %d, want 2 (same as original)", entry.Attempt)
	}
	if entry.TimerHandle == nil {
		t.Error("rescheduled TimerHandle = nil, want non-nil")
	} else {
		entry.TimerHandle.Stop()
	}

	// Claim preserved.
	if _, claimed := state.Claimed["ISS-1"]; !claimed {
		t.Error("Claimed[ISS-1] missing, want preserved")
	}

	// SaveRetryEntry called for the rescheduled entry.
	if len(store.savedEntries) != 1 {
		t.Fatalf("SaveRetryEntry call count = %d, want 1", len(store.savedEntries))
	}
	if store.savedEntries[0].Attempt != 2 {
		t.Errorf("saved Attempt = %d, want 2", store.savedEntries[0].Attempt)
	}
}

func TestHandleRetryTimer_SSHHostAcquisition(t *testing.T) {
	t.Parallel()

	t.Run("acquires host with preference", func(t *testing.T) {
		t.Parallel()

		hp := NewHostPool([]string{"host-a", "host-b"}, 2)
		store := &mockRetryStore{}
		tracker := &mockRetryTracker{
			fetchedIssue: candidateIssue("ISS-SSH", "ISS-SSH", "To Do"),
		}

		state := retryState(t, "ISS-SSH", "ISS-SSH", 2)
		state.RetryAttempts["ISS-SSH"].LastSSHHost = "host-b"

		params := defaultRetryParams(t, store, tracker)
		params.HostPool = hp

		ch := make(chan struct{}, 1)
		params.MakeWorkerFn = func(_, sshHost, _, _ string, _ domain.AgentAdapter) WorkerFunc {
			return func(_ context.Context, _ domain.Issue, _ *int) {
				if sshHost != "host-b" {
					t.Errorf("MakeWorkerFn sshHost = %q, want \"host-b\" (preferred)", sshHost)
				}
				ch <- struct{}{}
			}
		}

		HandleRetryTimer(state, "ISS-SSH", params)

		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatal("worker did not execute")
		}

		// Host-b was acquired (preferred).
		if hp.HostFor("ISS-SSH") != "host-b" {
			t.Errorf("HostFor(ISS-SSH) = %q, want \"host-b\"", hp.HostFor("ISS-SSH"))
		}
	})

	t.Run("no SSH capacity reschedules", func(t *testing.T) {
		t.Parallel()

		hp := NewHostPool([]string{"host-a"}, 1)
		hp.AcquireHost("OTHER-1", "")

		store := &mockRetryStore{}
		tracker := &mockRetryTracker{
			fetchedIssue: candidateIssue("ISS-FULL", "ISS-FULL", "To Do"),
		}

		state := retryState(t, "ISS-FULL", "ISS-FULL", 1)
		state.RetryAttempts["ISS-FULL"].ReactionKind = ReactionKindCI
		params := defaultRetryParams(t, store, tracker)
		params.HostPool = hp

		HandleRetryTimer(state, "ISS-FULL", params)

		// Not dispatched.
		if _, ok := state.Running["ISS-FULL"]; ok {
			t.Error("Running[ISS-FULL] present, want absent (no SSH capacity)")
		}

		// Rescheduled with backoff.
		entry, ok := state.RetryAttempts["ISS-FULL"]
		if !ok {
			t.Fatal("RetryAttempts[ISS-FULL] missing, want rescheduled")
		}
		if entry.Attempt != 2 {
			t.Errorf("rescheduled Attempt = %d, want 2", entry.Attempt)
		}
		if entry.Error != "no available SSH hosts" {
			t.Errorf("rescheduled Error = %q, want %q", entry.Error, "no available SSH hosts")
		}
		// ReactionKind preserved across reschedule.
		if entry.ReactionKind != ReactionKindCI {
			t.Errorf("rescheduled ReactionKind = %q, want %q", entry.ReactionKind, ReactionKindCI)
		}
		if entry.TimerHandle != nil {
			entry.TimerHandle.Stop()
		}
	})
}

func TestIsStaleRetryTimer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		entry *RetryEntry
		want  bool
	}{
		{
			name: "monotonic: freshly scheduled with long delay is stale",
			entry: &RetryEntry{
				scheduledAt:      time.Now(),
				scheduledDelayMS: 60_000, // 60s delay, just scheduled
			},
			want: true,
		},
		{
			name: "monotonic: delay already elapsed is not stale",
			entry: &RetryEntry{
				scheduledAt:      time.Now().Add(-2 * time.Second),
				scheduledDelayMS: 1000, // 1s delay, scheduled 2s ago
			},
			want: false,
		},
		{
			name: "monotonic: zero delay just scheduled is not stale",
			entry: &RetryEntry{
				scheduledAt:      time.Now().Add(-time.Millisecond),
				scheduledDelayMS: 0,
			},
			want: false,
		},
		{
			name: "startup-reconstructed: always non-stale regardless of DueAtMS",
			entry: &RetryEntry{
				// scheduledAt is zero — startup-reconstructed entry.
				// No stale predecessor exists, so always non-stale.
				DueAtMS: time.Now().UnixMilli() + 3_600_000,
			},
			want: false,
		},
		{
			name: "startup-reconstructed: past DueAtMS also non-stale",
			entry: &RetryEntry{
				// scheduledAt is zero, DueAtMS in the past.
				// Old wall-clock code returned false here too, but this
				// documents that DueAtMS is irrelevant for the decision.
				DueAtMS: time.Now().UnixMilli() - 10_000,
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := isStaleRetryTimer(tt.entry)
			if got != tt.want {
				t.Errorf("isStaleRetryTimer(%+v) = %v, want %v", tt.entry, got, tt.want)
			}
		})
	}
}

func TestHandleRetryTimer_WorkflowFilePropagated(t *testing.T) {
	t.Parallel()

	// WorkflowFile captured at dispatch should appear on the
	// RunningEntry so it is persisted by HandleWorkerExit.
	store := &mockRetryStore{}
	tracker := &mockRetryTracker{
		fetchedIssue: candidateIssue("ISS-WF", "ISS-WF", "To Do"),
	}

	state := retryState(t, "ISS-WF", "ISS-WF", 1)

	params := defaultRetryParams(t, store, tracker)
	params.WorkflowFile = "infra.WORKFLOW.md"

	workerCalled := make(chan struct{}, 1)
	params.MakeWorkerFn = func(_, _, _, _ string, _ domain.AgentAdapter) WorkerFunc {
		return func(_ context.Context, _ domain.Issue, _ *int) {
			workerCalled <- struct{}{}
		}
	}

	HandleRetryTimer(state, "ISS-WF", params)

	select {
	case <-workerCalled:
	case <-time.After(time.Second):
		t.Fatal("worker goroutine did not execute within 1 second")
	}

	running, ok := state.Running["ISS-WF"]
	if !ok {
		t.Fatal("Running[ISS-WF] missing after dispatch")
	}
	if running.WorkflowFile != "infra.WORKFLOW.md" {
		t.Errorf("Running[ISS-WF].WorkflowFile = %q, want %q", running.WorkflowFile, "infra.WORKFLOW.md")
	}
}

// TestHandleRetryTimer_BudgetExhaustedBlocksShouldDispatch verifies the composed
// behavior: after HandleRetryTimer marks an issue as budget-exhausted, ShouldDispatch
// returns false for that issue and the IncDispatches metric is recorded.
func TestHandleRetryTimer_BudgetExhaustedBlocksShouldDispatch(t *testing.T) {
	t.Parallel()

	const id = "ISS-COMP"
	spy := &spyMetrics{}
	state := retryState(t, id, "PROJ-COMP", 2)
	store := &mockRetryStore{runHistoryCount: 3}
	tracker := &mockRetryTracker{
		fetchedIssue: candidateIssue(id, "PROJ-COMP", "To Do"),
	}

	params := defaultRetryParams(t, store, tracker)
	params.MaxSessions = 3
	params.Metrics = spy

	HandleRetryTimer(state, id, params)

	// BudgetExhausted must be set after the budget-exhaustion path.
	if _, exhausted := state.BudgetExhausted[id]; !exhausted {
		t.Fatalf("BudgetExhausted[%s] missing after HandleRetryTimer budget exhaustion", id)
	}

	// Budget exhaustion must not emit a dispatch metric — no actual dispatch occurs.
	if len(spy.dispatches) != 0 {
		t.Errorf("dispatches = %v, want [] (budget exhaustion is not a dispatch)", spy.dispatches)
	}

	// ShouldDispatch must return false because BudgetExhausted is set.
	if ShouldDispatch(candidateIssue(id, "PROJ-COMP", "To Do"), state, params.ActiveStates, params.TerminalStates) {
		t.Error("ShouldDispatch() = true after budget exhaustion, want false")
	}
}

func TestHandleRetryTimer_ContinuationContextPropagated(t *testing.T) {
	t.Parallel()

	// Continuation context carried on the retry entry should be forwarded to
	// the running entry so the worker can inject it into the turn prompt.
	const id = "ISS-CI-RETRY"
	contContext := map[string]any{
		"ci_failure": map[string]any{
			"status":        "failing",
			"failing_count": 3,
			"ref":           "feature/fix",
		},
	}

	state := NewState(5000, 4, nil, AgentTotals{})
	state.Claimed[id] = struct{}{}
	state.RetryAttempts[id] = &RetryEntry{
		IssueID:             id,
		Identifier:          id,
		Attempt:             2,
		ContinuationContext: contContext,
	}

	store := &mockRetryStore{}
	tracker := &mockRetryTracker{
		fetchedIssue: candidateIssue(id, id, "In Progress"),
	}
	params := defaultRetryParams(t, store, tracker)

	HandleRetryTimer(state, id, params)

	t.Cleanup(func() { state.WorkerWg.Wait() })

	entry, ok := state.Running[id]
	if !ok {
		t.Fatal("issue not dispatched; state.Running[id] missing")
	}
	if entry.ContinuationContext == nil {
		t.Fatal("RunningEntry.ContinuationContext is nil; want continuation map")
	}
	ciData, ok := entry.ContinuationContext["ci_failure"].(map[string]any)
	if !ok {
		t.Fatal("ContinuationContext[ci_failure] is not map[string]any")
	}
	if ciData["status"] != "failing" {
		t.Errorf("ContinuationContext[ci_failure][status] = %v, want %q", ciData["status"], "failing")
	}
	if ciData["failing_count"] != 3 {
		t.Errorf("ContinuationContext[ci_failure][failing_count] = %v, want 3", ciData["failing_count"])
	}
}

func TestHandleRetryTimer_NilContinuationContext_NotPropagated(t *testing.T) {
	t.Parallel()

	// When the retry entry carries no continuation context, the running entry
	// must not have one set either (field stays nil; no accidental injection).
	const id = "ISS-NO-CI"

	state := NewState(5000, 4, nil, AgentTotals{})
	state.Claimed[id] = struct{}{}
	state.RetryAttempts[id] = &RetryEntry{
		IssueID:             id,
		Identifier:          id,
		Attempt:             1,
		ContinuationContext: nil,
	}

	store := &mockRetryStore{}
	tracker := &mockRetryTracker{
		fetchedIssue: candidateIssue(id, id, "In Progress"),
	}
	params := defaultRetryParams(t, store, tracker)

	HandleRetryTimer(state, id, params)

	t.Cleanup(func() { state.WorkerWg.Wait() })

	entry, ok := state.Running[id]
	if !ok {
		t.Fatal("issue not dispatched; state.Running[id] missing")
	}
	if entry.ContinuationContext != nil {
		t.Errorf("RunningEntry.ContinuationContext = %v, want nil", entry.ContinuationContext)
	}
}

func TestHandleRetryTimer_ContinuationDispatch_MarksReactionDispatched(t *testing.T) {
	t.Parallel()

	// A retry entry carrying ReactionKindCI must call MarkReactionDispatched
	// after successful dispatch, recording the correct issue ID and kind.
	const id = "ISS-CI-1"

	state := NewState(5000, 4, nil, AgentTotals{})
	state.Claimed[id] = struct{}{}
	state.RetryAttempts[id] = &RetryEntry{
		IssueID:             id,
		Identifier:          id,
		Attempt:             1,
		ReactionKind:        ReactionKindCI,
		ContinuationContext: map[string]any{"ci_failure": map[string]any{}},
	}

	store := &mockRetryStore{}
	tracker := &mockRetryTracker{
		fetchedIssue: candidateIssue(id, id, "In Progress"),
	}
	params := defaultRetryParams(t, store, tracker)

	HandleRetryTimer(state, id, params)
	t.Cleanup(func() { state.WorkerWg.Wait() })

	if store.markDispatchedCalls != 1 {
		t.Errorf("MarkReactionDispatched calls = %d, want 1", store.markDispatchedCalls)
	}
	if store.markDispatchedIssueID != id {
		t.Errorf("MarkReactionDispatched issueID = %q, want %q", store.markDispatchedIssueID, id)
	}
	if store.markDispatchedKind != ReactionKindCI {
		t.Errorf("MarkReactionDispatched kind = %q, want %q", store.markDispatchedKind, ReactionKindCI)
	}
	if _, ok := state.Running[id]; !ok {
		t.Errorf("Running[%s] missing after dispatch, want present", id)
	}
}

func TestHandleRetryTimer_NonReactionRetry_DoesNotMarkDispatched(t *testing.T) {
	t.Parallel()

	// A retry entry with empty ReactionKind (normal error retry) must not
	// call MarkReactionDispatched even when dispatch succeeds.
	const id = "ISS-ERR-1"

	state := NewState(5000, 4, nil, AgentTotals{})
	state.Claimed[id] = struct{}{}
	state.RetryAttempts[id] = &RetryEntry{
		IssueID:    id,
		Identifier: id,
		Attempt:    2,
		// ReactionKind is intentionally empty — normal error retry.
	}

	store := &mockRetryStore{}
	tracker := &mockRetryTracker{
		fetchedIssue: candidateIssue(id, id, "To Do"),
	}
	params := defaultRetryParams(t, store, tracker)

	HandleRetryTimer(state, id, params)
	t.Cleanup(func() { state.WorkerWg.Wait() })

	if store.markDispatchedCalls != 0 {
		t.Errorf("MarkReactionDispatched calls = %d, want 0 for non-reaction retry", store.markDispatchedCalls)
	}
	if _, ok := state.Running[id]; !ok {
		t.Errorf("Running[%s] missing after dispatch, want present", id)
	}
}

func TestHandleRetryTimer_ReschedulePreservesReactionKind(t *testing.T) {
	t.Parallel()

	// When the running-guard triggers a reschedule (worker still active),
	// ReactionKind must be preserved on the rescheduled entry so that the
	// eventual dispatch can call MarkReactionDispatched.
	const id = "ISS-CI-2"

	state := NewState(5000, 4, nil, AgentTotals{})
	state.Claimed[id] = struct{}{}
	state.RetryAttempts[id] = &RetryEntry{
		IssueID:      id,
		Identifier:   id,
		Attempt:      1,
		ReactionKind: ReactionKindCI,
	}
	// Place issue in Running to trigger the running-guard reschedule path.
	state.Running[id] = &RunningEntry{
		Identifier: id,
		Issue:      candidateIssue(id, id, "In Progress"),
		StartedAt:  time.Now().UTC(),
	}

	store := &mockRetryStore{}
	tracker := &mockRetryTracker{}
	params := defaultRetryParams(t, store, tracker)

	HandleRetryTimer(state, id, params)

	if store.markDispatchedCalls != 0 {
		t.Errorf("MarkReactionDispatched calls = %d, want 0 (reschedule, no dispatch)", store.markDispatchedCalls)
	}

	entry, ok := state.RetryAttempts[id]
	if !ok {
		t.Fatal("RetryAttempts[ISS-CI-2] missing, want rescheduled")
	}
	if entry.ReactionKind != ReactionKindCI {
		t.Errorf("rescheduled RetryAttempts[%s].ReactionKind = %q, want %q", id, entry.ReactionKind, ReactionKindCI)
	}
	if entry.TimerHandle != nil {
		entry.TimerHandle.Stop()
	}
}

func TestHandleRetryTimer_ContinuationMarkDispatchedError(t *testing.T) {
	t.Parallel()

	// When MarkReactionDispatched returns an error, the dispatch is not
	// rolled back — the issue remains in Running and the error is non-fatal.
	const id = "ISS-CI-3"

	state := NewState(5000, 4, nil, AgentTotals{})
	state.Claimed[id] = struct{}{}
	state.RetryAttempts[id] = &RetryEntry{
		IssueID:             id,
		Identifier:          id,
		Attempt:             1,
		ReactionKind:        ReactionKindCI,
		ContinuationContext: map[string]any{"ci_failure": map[string]any{}},
	}

	store := &mockRetryStore{markDispatchedErr: errors.New("db locked")}
	tracker := &mockRetryTracker{
		fetchedIssue: candidateIssue(id, id, "In Progress"),
	}
	params := defaultRetryParams(t, store, tracker)

	HandleRetryTimer(state, id, params)
	t.Cleanup(func() { state.WorkerWg.Wait() })

	// Attempt was made despite the error.
	if store.markDispatchedCalls != 1 {
		t.Errorf("MarkReactionDispatched calls = %d, want 1", store.markDispatchedCalls)
	}
	// Dispatch was not rolled back — issue still running.
	if _, ok := state.Running[id]; !ok {
		t.Errorf("Running[%s] missing after dispatch, want present (error is non-fatal)", id)
	}
}

func TestHandleRetryTimer_SessionID_PassedToMakeWorkerFn(t *testing.T) {
	t.Parallel()

	const id = "ISS-SESS"
	const wantSessionID = "sess-abc"

	state := retryState(t, id, id, 2)
	state.RetryAttempts[id].SessionID = wantSessionID

	store := &mockRetryStore{}
	tracker := &mockRetryTracker{
		fetchedIssue: candidateIssue(id, id, "In Progress"),
	}
	params := defaultRetryParams(t, store, tracker)

	var gotSessionID string
	ch := make(chan struct{}, 1)
	params.MakeWorkerFn = func(resumeSessionID, _, _, _ string, _ domain.AgentAdapter) WorkerFunc {
		gotSessionID = resumeSessionID
		return func(_ context.Context, _ domain.Issue, _ *int) {
			ch <- struct{}{}
		}
	}

	HandleRetryTimer(state, id, params)

	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("worker did not execute within 1 second")
	}

	if gotSessionID != wantSessionID {
		t.Errorf("MakeWorkerFn resumeSessionID = %q, want %q", gotSessionID, wantSessionID)
	}
}

func TestHandleRetryTimer_Reschedule_PreservesSessionID(t *testing.T) {
	t.Parallel()

	const id = "ISS-SESS-RESCHEDULE"
	const wantSessionID = "sess-preserve"

	state := retryState(t, id, id, 2)
	state.RetryAttempts[id].SessionID = wantSessionID
	// Fill all slots so no dispatch occurs — forces the reschedule path.
	state.MaxConcurrentAgents = 1
	state.Running["OTHER-1"] = &RunningEntry{
		Identifier: "OTHER-1",
		Issue:      candidateIssue("OTHER-1", "OTHER-1", "To Do"),
	}

	store := &mockRetryStore{}
	tracker := &mockRetryTracker{
		fetchedIssue: candidateIssue(id, id, "To Do"),
	}
	params := defaultRetryParams(t, store, tracker)

	HandleRetryTimer(state, id, params)

	entry, ok := state.RetryAttempts[id]
	if !ok {
		t.Fatalf("RetryAttempts[%s] missing after no-slots reschedule, want present", id)
	}
	if entry.SessionID != wantSessionID {
		t.Errorf("RetryAttempts[%s].SessionID = %q, want %q", id, entry.SessionID, wantSessionID)
	}
	if entry.TimerHandle != nil {
		entry.TimerHandle.Stop()
	}
}

// --- Handoff-state reaction retry tests ---

func TestHandleRetryTimer_ReactionReviewInHandoffStateDispatches(t *testing.T) {
	t.Parallel()

	const id = "HANDOFF-REVIEW"
	contContext := map[string]any{
		"review_comments": map[string]any{"count": 3},
	}

	// No claim set — simulates post-handoff state.
	state := NewState(5000, 4, nil, AgentTotals{})
	state.RetryAttempts[id] = &RetryEntry{
		IssueID:             id,
		Identifier:          id,
		Attempt:             1,
		ReactionKind:        ReactionKindReview,
		ContinuationContext: contContext,
		SessionID:           "sess-r1",
	}

	store := &mockRetryStore{}
	tracker := &mockRetryTracker{
		fetchedIssue: candidateIssue(id, id, "Ready For Review"),
	}
	params := defaultRetryParams(t, store, tracker)
	params.HandoffState = "Ready For Review"

	HandleRetryTimer(state, id, params)
	t.Cleanup(func() { state.WorkerWg.Wait() })

	entry, ok := state.Running[id]
	if !ok {
		t.Fatalf("Running[%s] missing after handoff-state review dispatch", id)
	}
	if entry.ContinuationContext == nil {
		t.Fatal("RunningEntry.ContinuationContext is nil, want forwarded context")
	}
	if _, ok := entry.ContinuationContext["review_comments"]; !ok {
		t.Error("RunningEntry.ContinuationContext missing review_comments key")
	}
	if entry.ReactionKind != ReactionKindReview {
		t.Errorf("RunningEntry.ReactionKind = %q, want %q", entry.ReactionKind, ReactionKindReview)
	}
	if store.markDispatchedCalls != 1 {
		t.Errorf("MarkReactionDispatched calls = %d, want 1", store.markDispatchedCalls)
	}
	if store.markDispatchedIssueID != id {
		t.Errorf("MarkReactionDispatched issueID = %q, want %q", store.markDispatchedIssueID, id)
	}
	if store.markDispatchedKind != ReactionKindReview {
		t.Errorf("MarkReactionDispatched kind = %q, want %q", store.markDispatchedKind, ReactionKindReview)
	}
	if len(store.deletedIssueID) != 1 || store.deletedIssueID[0] != id {
		t.Errorf("DeleteRetryEntry calls = %v, want [%s]", store.deletedIssueID, id)
	}
	if _, claimed := state.Claimed[id]; !claimed {
		t.Errorf("Claimed[%s] missing after dispatch, want claimed (set by DispatchIssue)", id)
	}
	if _, ok := state.RetryAttempts[id]; ok {
		t.Errorf("RetryAttempts[%s] present after dispatch, want cleared", id)
	}
}

func TestHandleRetryTimer_ReactionCIInHandoffStateDispatches(t *testing.T) {
	t.Parallel()

	const id = "HANDOFF-CI"
	contContext := map[string]any{
		"ci_failure": map[string]any{"status": "failing"},
	}

	state := NewState(5000, 4, nil, AgentTotals{})
	state.RetryAttempts[id] = &RetryEntry{
		IssueID:             id,
		Identifier:          id,
		Attempt:             1,
		ReactionKind:        ReactionKindCI,
		ContinuationContext: contContext,
	}

	store := &mockRetryStore{}
	tracker := &mockRetryTracker{
		fetchedIssue: candidateIssue(id, id, "Ready For Review"),
	}
	params := defaultRetryParams(t, store, tracker)
	params.HandoffState = "Ready For Review"

	HandleRetryTimer(state, id, params)
	t.Cleanup(func() { state.WorkerWg.Wait() })

	entry, ok := state.Running[id]
	if !ok {
		t.Fatalf("Running[%s] missing after handoff-state CI dispatch", id)
	}
	if entry.ReactionKind != ReactionKindCI {
		t.Errorf("RunningEntry.ReactionKind = %q, want %q", entry.ReactionKind, ReactionKindCI)
	}
	if entry.ContinuationContext == nil {
		t.Fatal("RunningEntry.ContinuationContext is nil, want forwarded context")
	}
	if store.markDispatchedCalls != 1 {
		t.Errorf("MarkReactionDispatched calls = %d, want 1", store.markDispatchedCalls)
	}
	if store.markDispatchedKind != ReactionKindCI {
		t.Errorf("MarkReactionDispatched kind = %q, want %q", store.markDispatchedKind, ReactionKindCI)
	}
	if len(store.deletedIssueID) != 1 || store.deletedIssueID[0] != id {
		t.Errorf("DeleteRetryEntry calls = %v, want [%s]", store.deletedIssueID, id)
	}
}

func TestHandleRetryTimer_NonReactionInHandoffStateReleasesClaim(t *testing.T) {
	t.Parallel()

	const id = "HANDOFF-NONREACTION"

	state := NewState(5000, 4, nil, AgentTotals{})
	state.Claimed[id] = struct{}{}
	state.RetryAttempts[id] = &RetryEntry{
		IssueID:    id,
		Identifier: id,
		Attempt:    1,
		// ReactionKind is empty — non-reaction retry.
	}

	store := &mockRetryStore{}
	tracker := &mockRetryTracker{
		fetchedIssue: candidateIssue(id, id, "Ready For Review"),
	}
	params := defaultRetryParams(t, store, tracker)
	params.HandoffState = "Ready For Review"

	HandleRetryTimer(state, id, params)

	if _, running := state.Running[id]; running {
		t.Errorf("Running[%s] present, want absent (non-reaction in handoff state)", id)
	}
	if _, claimed := state.Claimed[id]; claimed {
		t.Errorf("Claimed[%s] still present, want released (non-reaction in handoff state)", id)
	}
	if len(store.deletedIssueID) != 1 || store.deletedIssueID[0] != id {
		t.Errorf("DeleteRetryEntry calls = %v, want [%s]", store.deletedIssueID, id)
	}
	if _, ok := state.RetryAttempts[id]; ok {
		t.Errorf("RetryAttempts[%s] present, want absent", id)
	}
	if store.markDispatchedCalls != 0 {
		t.Errorf("MarkReactionDispatched calls = %d, want 0", store.markDispatchedCalls)
	}
}

func TestHandleRetryTimer_ReactionInUnrelatedStateReschedules(t *testing.T) {
	t.Parallel()

	const id = "UNRELATED-REVIEW"
	contContext := map[string]any{
		"review_comments": map[string]any{"count": 1},
	}

	state := NewState(5000, 4, nil, AgentTotals{})
	state.Claimed[id] = struct{}{}
	state.RetryAttempts[id] = &RetryEntry{
		IssueID:             id,
		Identifier:          id,
		Attempt:             1,
		ReactionKind:        ReactionKindReview,
		ContinuationContext: contContext,
		SessionID:           "sess-x",
		LastSSHHost:         "host-a",
	}

	store := &mockRetryStore{}
	// "QA Review": not active, not terminal, not handoff.
	tracker := &mockRetryTracker{
		fetchedIssue: candidateIssue(id, id, "QA Review"),
	}
	params := defaultRetryParams(t, store, tracker)
	params.HandoffState = "Ready For Review"

	HandleRetryTimer(state, id, params)

	if _, running := state.Running[id]; running {
		t.Errorf("Running[%s] present, want absent (paused reaction reschedule)", id)
	}
	entry, ok := state.RetryAttempts[id]
	if !ok {
		t.Fatalf("RetryAttempts[%s] missing after paused reaction reschedule", id)
	}
	if entry.Attempt != 2 {
		t.Errorf("RetryAttempts[%s].Attempt = %d, want 2", id, entry.Attempt)
	}
	if entry.ReactionKind != ReactionKindReview {
		t.Errorf("RetryAttempts[%s].ReactionKind = %q, want %q", id, entry.ReactionKind, ReactionKindReview)
	}
	if entry.ContinuationContext == nil {
		t.Fatal("RetryAttempts.ContinuationContext is nil, want preserved")
	}
	if _, ok := entry.ContinuationContext["review_comments"]; !ok {
		t.Error("RetryAttempts.ContinuationContext missing review_comments key")
	}
	if entry.SessionID != "sess-x" {
		t.Errorf("RetryAttempts[%s].SessionID = %q, want %q", id, entry.SessionID, "sess-x")
	}
	if entry.LastSSHHost != "host-a" {
		t.Errorf("RetryAttempts[%s].LastSSHHost = %q, want %q", id, entry.LastSSHHost, "host-a")
	}
	if len(store.savedEntries) != 0 {
		t.Errorf("SaveRetryEntry call count = %d, want 0 (reaction retry is runtime-only)", len(store.savedEntries))
	}
	if store.markDispatchedCalls != 0 {
		t.Errorf("MarkReactionDispatched calls = %d, want 0 (no dispatch)", store.markDispatchedCalls)
	}
	if entry.TimerHandle != nil {
		entry.TimerHandle.Stop()
	}
}

func TestHandleRetryTimer_NonReactionInUnrelatedStateReleasesClaim(t *testing.T) {
	t.Parallel()

	const id = "UNRELATED-NONREACTION"

	state := NewState(5000, 4, nil, AgentTotals{})
	state.Claimed[id] = struct{}{}
	state.RetryAttempts[id] = &RetryEntry{
		IssueID:    id,
		Identifier: id,
		Attempt:    1,
		// ReactionKind empty.
	}

	store := &mockRetryStore{}
	tracker := &mockRetryTracker{
		fetchedIssue: candidateIssue(id, id, "QA Review"),
	}
	params := defaultRetryParams(t, store, tracker)
	params.HandoffState = "Ready For Review"

	HandleRetryTimer(state, id, params)

	if _, running := state.Running[id]; running {
		t.Errorf("Running[%s] present, want absent (non-reaction in unrelated state)", id)
	}
	if _, claimed := state.Claimed[id]; claimed {
		t.Errorf("Claimed[%s] still present, want released (non-reaction in unrelated state)", id)
	}
	if len(store.deletedIssueID) != 1 || store.deletedIssueID[0] != id {
		t.Errorf("DeleteRetryEntry calls = %v, want [%s]", store.deletedIssueID, id)
	}
	if _, ok := state.RetryAttempts[id]; ok {
		t.Errorf("RetryAttempts[%s] present, want absent", id)
	}
	if store.markDispatchedCalls != 0 {
		t.Errorf("MarkReactionDispatched calls = %d, want 0", store.markDispatchedCalls)
	}
}

func TestHandleRetryTimer_ReactionInTerminalStateReleasesClaim(t *testing.T) {
	t.Parallel()

	const id = "TERMINAL-REACTION"

	state := NewState(5000, 4, nil, AgentTotals{})
	state.Claimed[id] = struct{}{}
	state.RetryAttempts[id] = &RetryEntry{
		IssueID:      id,
		Identifier:   id,
		Attempt:      1,
		ReactionKind: ReactionKindCI,
	}

	store := &mockRetryStore{}
	tracker := &mockRetryTracker{
		// "Done" is in TerminalStates; "Ready For Review" is HandoffState.
		fetchedIssue: candidateIssue(id, id, "Done"),
	}
	params := defaultRetryParams(t, store, tracker)
	params.HandoffState = "Ready For Review"

	HandleRetryTimer(state, id, params)

	if _, running := state.Running[id]; running {
		t.Errorf("Running[%s] present, want absent (terminal state must not dispatch)", id)
	}
	if _, claimed := state.Claimed[id]; claimed {
		t.Errorf("Claimed[%s] still present, want released (terminal state)", id)
	}
	if len(store.deletedIssueID) != 1 || store.deletedIssueID[0] != id {
		t.Errorf("DeleteRetryEntry calls = %v, want [%s]", store.deletedIssueID, id)
	}
	if _, ok := state.RetryAttempts[id]; ok {
		t.Errorf("RetryAttempts[%s] present, want absent (terminal state)", id)
	}
	if len(store.savedEntries) != 0 {
		t.Errorf("SaveRetryEntry call count = %d, want 0 (terminal state releases)", len(store.savedEntries))
	}
}

func TestHandleRetryTimer_ReactionHandoffNoSlotsPreservesContext(t *testing.T) {
	t.Parallel()

	const id = "HANDOFF-NO-SLOTS"
	contContext := map[string]any{
		"review_comments": map[string]any{"count": 2},
	}

	// MaxConcurrentAgents = 1, one other issue already running → slots exhausted.
	state := NewState(5000, 1, nil, AgentTotals{})
	state.Claimed[id] = struct{}{}
	state.Running["OTHER-1"] = &RunningEntry{
		Identifier: "OTHER-1",
		Issue:      candidateIssue("OTHER-1", "OTHER-1", "In Progress"),
	}
	state.RetryAttempts[id] = &RetryEntry{
		IssueID:             id,
		Identifier:          id,
		Attempt:             1,
		ReactionKind:        ReactionKindReview,
		ContinuationContext: contContext,
		SessionID:           "sess-ns",
		LastSSHHost:         "host-x",
	}

	store := &mockRetryStore{}
	tracker := &mockRetryTracker{
		fetchedIssue: candidateIssue(id, id, "Ready For Review"),
	}
	params := defaultRetryParams(t, store, tracker)
	params.HandoffState = "Ready For Review"

	HandleRetryTimer(state, id, params)

	if _, running := state.Running[id]; running {
		t.Errorf("Running[%s] present, want absent (no global slots)", id)
	}
	entry, ok := state.RetryAttempts[id]
	if !ok {
		t.Fatalf("RetryAttempts[%s] missing after no-slots reschedule", id)
	}
	if entry.Attempt != 2 {
		t.Errorf("RetryAttempts[%s].Attempt = %d, want 2", id, entry.Attempt)
	}
	if entry.Error != "no available orchestrator slots" {
		t.Errorf("RetryAttempts[%s].Error = %q, want %q", id, entry.Error, "no available orchestrator slots")
	}
	if entry.ReactionKind != ReactionKindReview {
		t.Errorf("RetryAttempts[%s].ReactionKind = %q, want %q", id, entry.ReactionKind, ReactionKindReview)
	}
	if entry.ContinuationContext == nil {
		t.Fatal("RetryAttempts.ContinuationContext is nil after no-slots reschedule, want preserved")
	}
	if entry.SessionID != "sess-ns" {
		t.Errorf("RetryAttempts[%s].SessionID = %q, want %q", id, entry.SessionID, "sess-ns")
	}
	if entry.LastSSHHost != "host-x" {
		t.Errorf("RetryAttempts[%s].LastSSHHost = %q, want %q", id, entry.LastSSHHost, "host-x")
	}
	if _, claimed := state.Claimed[id]; !claimed {
		t.Errorf("Claimed[%s] missing after no-slots reschedule, want claimed", id)
	}
	if store.markDispatchedCalls != 0 {
		t.Errorf("MarkReactionDispatched calls = %d, want 0 (no dispatch)", store.markDispatchedCalls)
	}
	if len(store.savedEntries) != 0 {
		t.Errorf("SaveRetryEntry call count = %d, want 0 (reaction retry is runtime-only)", len(store.savedEntries))
	}
	if entry.TimerHandle != nil {
		entry.TimerHandle.Stop()
	}
}

func TestHandleRetryTimer_ReactionHandoffPerStateCapExhaustedPreservesContext(t *testing.T) {
	t.Parallel()

	const id = "HANDOFF-PER-STATE"
	contContext := map[string]any{
		"ci_failure": map[string]any{"status": "failing"},
	}

	// Per-state cap for "ready for review" = 1. One other issue running in that state.
	// Global capacity = 5 (plenty available).
	perStateMap := map[string]int{"ready for review": 1}
	state := NewState(5000, 5, perStateMap, AgentTotals{})
	state.Running["OTHER-RFR"] = &RunningEntry{
		Identifier: "OTHER-RFR",
		Issue:      candidateIssue("OTHER-RFR", "OTHER-RFR", "Ready For Review"),
	}
	state.RetryAttempts[id] = &RetryEntry{
		IssueID:             id,
		Identifier:          id,
		Attempt:             1,
		ReactionKind:        ReactionKindCI,
		ContinuationContext: contContext,
	}

	store := &mockRetryStore{}
	tracker := &mockRetryTracker{
		fetchedIssue: candidateIssue(id, id, "Ready For Review"),
	}
	params := defaultRetryParams(t, store, tracker)
	params.HandoffState = "Ready For Review"

	HandleRetryTimer(state, id, params)

	if _, running := state.Running[id]; running {
		t.Errorf("Running[%s] present, want absent (per-state cap exhausted)", id)
	}
	entry, ok := state.RetryAttempts[id]
	if !ok {
		t.Fatalf("RetryAttempts[%s] missing after per-state cap reschedule", id)
	}
	if entry.Attempt != 2 {
		t.Errorf("RetryAttempts[%s].Attempt = %d, want 2", id, entry.Attempt)
	}
	if entry.ReactionKind != ReactionKindCI {
		t.Errorf("RetryAttempts[%s].ReactionKind = %q, want %q", id, entry.ReactionKind, ReactionKindCI)
	}
	if entry.ContinuationContext == nil {
		t.Fatal("RetryAttempts.ContinuationContext is nil after per-state cap reschedule, want preserved")
	}
	if store.markDispatchedCalls != 0 {
		t.Errorf("MarkReactionDispatched calls = %d, want 0 (no dispatch)", store.markDispatchedCalls)
	}
	if len(store.savedEntries) != 0 {
		t.Errorf("SaveRetryEntry call count = %d, want 0 (reaction retry is runtime-only)", len(store.savedEntries))
	}
	if entry.TimerHandle != nil {
		entry.TimerHandle.Stop()
	}
}

func TestHandleRetryTimer_ReactionActiveStateBlockerReschedules(t *testing.T) {
	t.Parallel()

	const id = "ACTIVE-BLOCKER-REACTION"
	contContext := map[string]any{
		"review_comments": map[string]any{"count": 1},
	}

	state := NewState(5000, 4, nil, AgentTotals{})
	state.Claimed[id] = struct{}{}
	state.RetryAttempts[id] = &RetryEntry{
		IssueID:             id,
		Identifier:          id,
		Attempt:             1,
		ReactionKind:        ReactionKindReview,
		ContinuationContext: contContext,
		SessionID:           "sess-b",
		LastSSHHost:         "host-b",
	}

	store := &mockRetryStore{}
	issue := candidateIssue(id, id, "To Do") // in active states
	issue.BlockedBy = []domain.BlockerRef{
		{ID: "BLOCK-1", Identifier: "BLOCK-1", State: "In Progress"}, // non-terminal blocker
	}
	tracker := &mockRetryTracker{fetchedIssue: issue}
	params := defaultRetryParams(t, store, tracker)
	params.HandoffState = "Ready For Review"

	HandleRetryTimer(state, id, params)

	if _, running := state.Running[id]; running {
		t.Errorf("Running[%s] present, want absent (blocked reaction retry)", id)
	}
	entry, ok := state.RetryAttempts[id]
	if !ok {
		t.Fatalf("RetryAttempts[%s] missing after blocked reaction reschedule", id)
	}
	if entry.Attempt != 2 {
		t.Errorf("RetryAttempts[%s].Attempt = %d, want 2", id, entry.Attempt)
	}
	if entry.ReactionKind != ReactionKindReview {
		t.Errorf("RetryAttempts[%s].ReactionKind = %q, want %q", id, entry.ReactionKind, ReactionKindReview)
	}
	if entry.ContinuationContext == nil {
		t.Fatal("RetryAttempts.ContinuationContext nil after blocker reschedule, want preserved")
	}
	if entry.SessionID != "sess-b" {
		t.Errorf("RetryAttempts[%s].SessionID = %q, want %q", id, entry.SessionID, "sess-b")
	}
	if entry.LastSSHHost != "host-b" {
		t.Errorf("RetryAttempts[%s].LastSSHHost = %q, want %q", id, entry.LastSSHHost, "host-b")
	}
	if store.markDispatchedCalls != 0 {
		t.Errorf("MarkReactionDispatched calls = %d, want 0 (no dispatch)", store.markDispatchedCalls)
	}
	if len(store.savedEntries) != 0 {
		t.Errorf("SaveRetryEntry call count = %d, want 0 (reaction retry is runtime-only)", len(store.savedEntries))
	}
	if entry.TimerHandle != nil {
		entry.TimerHandle.Stop()
	}
}

func TestHandleRetryTimer_NonReactionActiveStateBlockerReleasesClaim(t *testing.T) {
	t.Parallel()

	const id = "ACTIVE-BLOCKER-NONREACTION"

	state := NewState(5000, 4, nil, AgentTotals{})
	state.Claimed[id] = struct{}{}
	state.RetryAttempts[id] = &RetryEntry{
		IssueID:    id,
		Identifier: id,
		Attempt:    1,
		// Empty ReactionKind.
	}

	store := &mockRetryStore{}
	issue := candidateIssue(id, id, "To Do") // in active states
	issue.BlockedBy = []domain.BlockerRef{
		{ID: "BLOCK-1", Identifier: "BLOCK-1", State: "In Progress"}, // non-terminal blocker
	}
	tracker := &mockRetryTracker{fetchedIssue: issue}
	params := defaultRetryParams(t, store, tracker)
	params.HandoffState = "Ready For Review"

	HandleRetryTimer(state, id, params)

	if _, running := state.Running[id]; running {
		t.Errorf("Running[%s] present, want absent (non-reaction blocked)", id)
	}
	if _, claimed := state.Claimed[id]; claimed {
		t.Errorf("Claimed[%s] still present, want released (non-reaction blocked)", id)
	}
	if len(store.deletedIssueID) != 1 || store.deletedIssueID[0] != id {
		t.Errorf("DeleteRetryEntry calls = %v, want [%s]", store.deletedIssueID, id)
	}
	if _, ok := state.RetryAttempts[id]; ok {
		t.Errorf("RetryAttempts[%s] present, want absent (non-reaction blocked)", id)
	}
	if len(store.savedEntries) != 0 {
		t.Errorf("SaveRetryEntry call count = %d, want 0 (no reschedule)", len(store.savedEntries))
	}
}

func TestHandleRetryTimer_UnknownReactionKindInHandoffStateReleasesClaim(t *testing.T) {
	t.Parallel()

	const id = "HANDOFF-UNKNOWN"

	state := NewState(5000, 4, nil, AgentTotals{})
	state.Claimed[id] = struct{}{}
	state.RetryAttempts[id] = &RetryEntry{
		IssueID:      id,
		Identifier:   id,
		Attempt:      1,
		ReactionKind: "merge_conflict", // unknown kind — not ci or review
	}

	store := &mockRetryStore{}
	tracker := &mockRetryTracker{
		fetchedIssue: candidateIssue(id, id, "Ready For Review"),
	}
	handler := &recordHandler{}
	params := defaultRetryParams(t, store, tracker)
	params.HandoffState = "Ready For Review"
	params.Logger = slog.New(handler)

	HandleRetryTimer(state, id, params)

	// Treated as non-reaction: claim released, retry deleted.
	if _, running := state.Running[id]; running {
		t.Errorf("Running[%s] present, want absent (unknown reaction kind)", id)
	}
	if _, claimed := state.Claimed[id]; claimed {
		t.Errorf("Claimed[%s] still present, want released (unknown kind treated as non-reaction)", id)
	}
	if len(store.deletedIssueID) != 1 || store.deletedIssueID[0] != id {
		t.Errorf("DeleteRetryEntry calls = %v, want [%s]", store.deletedIssueID, id)
	}
	// Warning emitted for unknown reaction kind.
	if handler.countByMessage("found unknown reaction kind in retry entry") != 1 {
		t.Errorf("Warn('found unknown reaction kind in retry entry') emitted %d times, want 1",
			handler.countByMessage("found unknown reaction kind in retry entry"))
	}
	if store.markDispatchedCalls != 0 {
		t.Errorf("MarkReactionDispatched calls = %d, want 0 (unknown kind)", store.markDispatchedCalls)
	}
}

func TestHandleRetryTimer_HandoffReactionStartsWithoutExistingClaim(t *testing.T) {
	t.Parallel()

	const id = "HANDOFF-NO-CLAIM"

	// state.Claimed is intentionally empty — no pre-existing claim for this
	// issue. Guards against nil-map panics or early-return guards that
	// incorrectly require a prior claim before dispatching a handoff-state
	// reaction retry.
	state := NewState(5000, 4, nil, AgentTotals{})
	state.RetryAttempts[id] = &RetryEntry{
		IssueID:      id,
		Identifier:   id,
		Attempt:      1,
		ReactionKind: ReactionKindReview,
	}

	store := &mockRetryStore{}
	tracker := &mockRetryTracker{
		fetchedIssue: candidateIssue(id, id, "Ready For Review"),
	}
	params := defaultRetryParams(t, store, tracker)
	params.HandoffState = "Ready For Review"

	// Must not panic.
	HandleRetryTimer(state, id, params)
	t.Cleanup(func() { state.WorkerWg.Wait() })

	if _, ok := state.Running[id]; !ok {
		t.Fatalf("Running[%s] missing after no-claim handoff dispatch, want worker dispatched", id)
	}
	if _, claimed := state.Claimed[id]; !claimed {
		t.Errorf("Claimed[%s] absent after dispatch, want DispatchIssue to add claim", id)
	}
}

// --- Frozen-selection tests ---

// TestHandleRetryTimer_FrozenFieldsPropagatedToRunningEntry verifies that
// AgentKind, RuleName, and TemplateID are copied from the retry entry into
// the RunningEntry after dispatch so the fields are preserved across retries.
func TestHandleRetryTimer_FrozenFieldsPropagatedToRunningEntry(t *testing.T) {
	t.Parallel()

	const id = "ISS-FROZEN"
	const wantAgentKind = "claude-code"
	const wantRuleName = "bug-rule"
	const wantTemplateID = "/abs/prompts/bug.md"

	state := NewState(5000, 4, nil, AgentTotals{})
	state.RetryAttempts[id] = &RetryEntry{
		IssueID:    id,
		Identifier: id,
		Attempt:    2,
		AgentKind:  wantAgentKind,
		RuleName:   wantRuleName,
		TemplateID: wantTemplateID,
	}
	state.Claimed[id] = struct{}{}

	store := &mockRetryStore{}
	tracker := &mockRetryTracker{
		fetchedIssue: candidateIssue(id, id, "To Do"),
	}

	var capturedAgentKind, capturedTemplateID string
	params := defaultRetryParams(t, store, tracker)
	params.MakeWorkerFn = func(_, _, agentKind, templateID string, _ domain.AgentAdapter) WorkerFunc {
		capturedAgentKind = agentKind
		capturedTemplateID = templateID
		return func(_ context.Context, _ domain.Issue, _ *int) {}
	}
	params.AgentAdapterByKind = func(kind string) (domain.AgentAdapter, error) {
		if kind != wantAgentKind {
			t.Errorf("AgentAdapterByKind(kind) = %q, want %q", kind, wantAgentKind)
		}
		return &mockAgentAdapter{}, nil
	}

	HandleRetryTimer(state, id, params)
	t.Cleanup(func() { state.WorkerWg.Wait() })

	entry, ok := state.Running[id]
	if !ok {
		t.Fatal("Running entry missing after retry dispatch")
	}

	if entry.AgentKind != wantAgentKind {
		t.Errorf("RunningEntry.AgentKind = %q, want %q", entry.AgentKind, wantAgentKind)
	}
	if entry.RuleName != wantRuleName {
		t.Errorf("RunningEntry.RuleName = %q, want %q", entry.RuleName, wantRuleName)
	}
	if entry.TemplateID != wantTemplateID {
		t.Errorf("RunningEntry.TemplateID = %q, want %q", entry.TemplateID, wantTemplateID)
	}
	if capturedAgentKind != wantAgentKind {
		t.Errorf("MakeWorkerFn agentKind = %q, want %q", capturedAgentKind, wantAgentKind)
	}
	if capturedTemplateID != wantTemplateID {
		t.Errorf("MakeWorkerFn templateID = %q, want %q", capturedTemplateID, wantTemplateID)
	}
}

// TestHandleRetryTimer_ReschedulePreservesFrozenFields verifies that when a
// retry is rescheduled (e.g., no available slots), the frozen AgentKind,
// RuleName, and TemplateID are preserved in the new RetryEntry.
func TestHandleRetryTimer_ReschedulePreservesFrozenFields(t *testing.T) {
	t.Parallel()

	const id = "ISS-RESCHED"
	const wantAgentKind = "copilot"
	const wantRuleName = "feature-rule"
	const wantTemplateID = "/abs/prompts/feature.md"

	// Fill all slots so dispatch is blocked and the retry is rescheduled.
	state := NewState(1, 1, nil, AgentTotals{})
	state.RetryAttempts[id] = &RetryEntry{
		IssueID:    id,
		Identifier: id,
		Attempt:    1,
		AgentKind:  wantAgentKind,
		RuleName:   wantRuleName,
		TemplateID: wantTemplateID,
	}
	state.Claimed[id] = struct{}{}
	// Occupy the single slot.
	state.Running["OTHER"] = &RunningEntry{Identifier: "OTHER"}

	store := &mockRetryStore{}
	tracker := &mockRetryTracker{
		fetchedIssue: candidateIssue(id, id, "To Do"),
	}
	params := defaultRetryParams(t, store, tracker)

	HandleRetryTimer(state, id, params)

	newEntry, ok := state.RetryAttempts[id]
	if !ok {
		t.Fatal("RetryAttempts entry missing after no-slots reschedule")
	}
	if newEntry.AgentKind != wantAgentKind {
		t.Errorf("RetryEntry.AgentKind = %q, want %q", newEntry.AgentKind, wantAgentKind)
	}
	if newEntry.RuleName != wantRuleName {
		t.Errorf("RetryEntry.RuleName = %q, want %q", newEntry.RuleName, wantRuleName)
	}
	if newEntry.TemplateID != wantTemplateID {
		t.Errorf("RetryEntry.TemplateID = %q, want %q", newEntry.TemplateID, wantTemplateID)
	}
	if newEntry.TimerHandle != nil {
		newEntry.TimerHandle.Stop()
	}
}

// TestHandleRetryTimer_FrozenFieldsPersistedOnReschedule verifies that when a
// retry is rescheduled, the SQLite row captures the frozen AgentKind, RuleName,
// and TemplateID so they survive a process restart.
func TestHandleRetryTimer_FrozenFieldsPersistedOnReschedule(t *testing.T) {
	t.Parallel()

	const id = "ISS-PERSIST"
	const wantAgentKind = "codex"
	const wantRuleName = "docs-rule"
	const wantTemplateID = "/abs/prompts/docs.md"

	// Fill all slots to force reschedule.
	state := NewState(1, 1, nil, AgentTotals{})
	state.RetryAttempts[id] = &RetryEntry{
		IssueID:    id,
		Identifier: id,
		Attempt:    1,
		AgentKind:  wantAgentKind,
		RuleName:   wantRuleName,
		TemplateID: wantTemplateID,
	}
	state.Claimed[id] = struct{}{}
	state.Running["OTHER"] = &RunningEntry{Identifier: "OTHER"}

	store := &mockRetryStore{}
	tracker := &mockRetryTracker{
		fetchedIssue: candidateIssue(id, id, "To Do"),
	}
	params := defaultRetryParams(t, store, tracker)

	HandleRetryTimer(state, id, params)

	if len(store.savedEntries) == 0 {
		t.Fatal("SaveRetryEntry not called after reschedule")
	}
	saved := store.savedEntries[0]
	if saved.AgentKind != wantAgentKind {
		t.Errorf("saved RetryEntry.AgentKind = %q, want %q", saved.AgentKind, wantAgentKind)
	}
	if saved.RuleName != wantRuleName {
		t.Errorf("saved RetryEntry.RuleName = %q, want %q", saved.RuleName, wantRuleName)
	}
	if saved.TemplateID != wantTemplateID {
		t.Errorf("saved RetryEntry.TemplateID = %q, want %q", saved.TemplateID, wantTemplateID)
	}
	// Clean up the timer.
	if entry, ok := state.RetryAttempts[id]; ok && entry.TimerHandle != nil {
		entry.TimerHandle.Stop()
	}
}

// TestHandleRetryTimer_AgentAdapterLookupUsesAgentKind verifies that when
// AgentAdapterByKind returns an error for the frozen agent kind, the claim is
// released and no dispatch occurs.
func TestHandleRetryTimer_AgentAdapterLookupUsesAgentKind(t *testing.T) {
	t.Parallel()

	const id = "ISS-BADKIND"
	const frozenKind = "unknown-agent"

	state := NewState(5000, 4, nil, AgentTotals{})
	state.RetryAttempts[id] = &RetryEntry{
		IssueID:    id,
		Identifier: id,
		Attempt:    1,
		AgentKind:  frozenKind,
		RuleName:   "some-rule",
		TemplateID: "",
	}
	state.Claimed[id] = struct{}{}

	store := &mockRetryStore{}
	tracker := &mockRetryTracker{
		fetchedIssue: candidateIssue(id, id, "To Do"),
	}
	params := defaultRetryParams(t, store, tracker)
	params.AgentAdapterByKind = func(kind string) (domain.AgentAdapter, error) {
		if kind != frozenKind {
			t.Errorf("AgentAdapterByKind(kind) = %q, want %q", kind, frozenKind)
		}
		return nil, errors.New("adapter not registered")
	}

	HandleRetryTimer(state, id, params)

	if _, claimed := state.Claimed[id]; claimed {
		t.Error("Claimed[id] still present after adapter lookup failure, want released")
	}
	if _, ok := state.Running[id]; ok {
		t.Error("Running[id] present after adapter lookup failure, want not dispatched")
	}
	if len(store.deletedIssueID) == 0 {
		t.Error("DeleteRetryEntry not called after adapter lookup failure")
	}
}
