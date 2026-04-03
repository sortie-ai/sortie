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

func (m *mockRetryStore) AppendRunHistory(_ context.Context, run persistence.RunHistory) (persistence.RunHistory, error) {
	return run, nil
}

// mockRetryTracker implements domain.TrackerAdapter for retry timer tests.
// Only FetchCandidateIssues is used by HandleRetryTimer; the remaining
// methods panic if called.
type mockRetryTracker struct {
	candidates []domain.Issue
	fetchErr   error
	fetchCount int
}

var _ domain.TrackerAdapter = (*mockRetryTracker)(nil)

func (m *mockRetryTracker) FetchCandidateIssues(_ context.Context) ([]domain.Issue, error) {
	m.fetchCount++
	return m.candidates, m.fetchErr
}

func (m *mockRetryTracker) FetchIssueByID(context.Context, string) (domain.Issue, error) {
	panic("FetchIssueByID must not be called by HandleRetryTimer")
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
		MakeWorkerFn:      func(_, _ string) WorkerFunc { return func(_ context.Context, _ domain.Issue, _ *int) {} },
		OnRetryFire:       noopRetryFire,
		Ctx:               context.Background(),
		Logger:            discardLogger(),
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
					t.Errorf("FetchCandidateIssues call count = %d, want 0", tracker.fetchCount)
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
					t.Errorf("FetchCandidateIssues call count = %d, want 0", tracker.fetchCount)
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
					candidates: []domain.Issue{
						candidateIssue(id, id, "To Do"),
					},
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
					t.Errorf("FetchCandidateIssues call count = %d, want 1", tracker.fetchCount)
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
				return retryState(t, id, id, 2)
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
				if entry.Error != "retry poll failed" {
					t.Errorf("RetryAttempts[%s].Error = %q, want %q", id, entry.Error, "retry poll failed")
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
				// SaveRetryEntry called once.
				if len(store.savedEntries) != 1 {
					t.Fatalf("SaveRetryEntry call count = %d, want 1", len(store.savedEntries))
				}
				if store.savedEntries[0].Attempt != 3 {
					t.Errorf("saved entry Attempt = %d, want 3", store.savedEntries[0].Attempt)
				}
				// DeleteRetryEntry not called.
				if len(store.deletedIssueID) != 0 {
					t.Errorf("DeleteRetryEntry call count = %d, want 0", len(store.deletedIssueID))
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
				// Return candidates that do NOT contain the target issue.
				return &mockRetryTracker{
					candidates: []domain.Issue{
						candidateIssue("OTHER-1", "OTHER-1", "To Do"),
					},
				}
			},
			check: func(t *testing.T, id string, state *State, store *mockRetryStore, _ *mockRetryTracker, _ bool) {
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
			},
		},
		{
			name:    "no available slots reschedules with backoff",
			issueID: "ISS-1",
			state: func(t *testing.T, id string) *State {
				t.Helper()
				state := retryState(t, id, id, 1)
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
					candidates: []domain.Issue{
						candidateIssue(id, id, "To Do"),
						candidateIssue("OTHER-1", "OTHER-1", "To Do"),
					},
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
				// SaveRetryEntry called.
				if len(store.savedEntries) != 1 {
					t.Fatalf("SaveRetryEntry call count = %d, want 1", len(store.savedEntries))
				}
				if store.savedEntries[0].Attempt != 2 {
					t.Errorf("saved entry Attempt = %d, want 2", store.savedEntries[0].Attempt)
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
					candidates: []domain.Issue{
						candidateIssue(id, id, "To Do"),
					},
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
					candidates: []domain.Issue{
						candidateIssue(id, id, "In Progress"),
					},
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
				// Candidates don't contain the target issue → release path.
				return &mockRetryTracker{
					candidates: []domain.Issue{
						candidateIssue("OTHER-1", "OTHER-1", "To Do"),
					},
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
					candidates: []domain.Issue{issue},
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
					candidates: []domain.Issue{
						{ID: id, Identifier: id, Title: "", State: "To Do"},
					},
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
				// Issue is in terminal state "Done" — rejected by Step 3b.
				return &mockRetryTracker{
					candidates: []domain.Issue{
						candidateIssue(id, id, "Done"),
					},
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
			tracker: func(id string) *mockRetryTracker {
				return &mockRetryTracker{
					candidates: []domain.Issue{candidateIssue(id, "PROJ-BUDGET", "To Do")},
				}
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
					t.Errorf("FetchCandidateIssues call count = %d, want 0", tracker.fetchCount)
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
					candidates: []domain.Issue{candidateIssue(id, "PROJ-UNDER", "To Do")},
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
					t.Errorf("FetchCandidateIssues call count = %d, want 1", tracker.fetchCount)
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
					candidates: []domain.Issue{candidateIssue(id, "PROJ-NOLIMIT", "To Do")},
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
					t.Errorf("FetchCandidateIssues call count = %d, want 1", tracker.fetchCount)
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
					candidates: []domain.Issue{candidateIssue(id, "PROJ-FAIL", "To Do")},
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
					t.Errorf("FetchCandidateIssues call count = %d, want 1 (fail-open)", tracker.fetchCount)
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

			var workerCalled bool
			if tt.workerFn != nil {
				ch := make(chan struct{}, 1)
				wf := tt.workerFn(ch)
				params.MakeWorkerFn = func(_, _ string) WorkerFunc { return wf }
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

func TestHandleRetryTimer_WorkerStillRunningReschedulesInsteadOfDispatching(t *testing.T) {
	t.Parallel()

	store := &mockRetryStore{}
	tracker := &mockRetryTracker{
		// If the guard works, FetchCandidateIssues should never be called.
		candidates: []domain.Issue{candidateIssue("ISS-1", "ISS-1", "To Do")},
	}

	state := retryState(t, "ISS-1", "ISS-1", 2)
	// Place the issue in Running to simulate a cancelled-but-not-yet-exited worker.
	state.Running["ISS-1"] = &RunningEntry{
		Identifier: "ISS-1",
		Issue:      candidateIssue("ISS-1", "ISS-1", "In Progress"),
		StartedAt:  time.Now().UTC(),
	}

	params := defaultRetryParams(t, store, tracker)

	workerCalled := false
	params.MakeWorkerFn = func(_, _ string) WorkerFunc {
		return func(_ context.Context, _ domain.Issue, _ *int) {
			workerCalled = true
		}
	}

	HandleRetryTimer(state, "ISS-1", params)

	// Worker must NOT have been dispatched.
	if workerCalled {
		t.Error("worker dispatched while issue still in Running, want no dispatch")
	}

	// FetchCandidateIssues should not have been called — guard returns early.
	if tracker.fetchCount != 0 {
		t.Errorf("FetchCandidateIssues call count = %d, want 0", tracker.fetchCount)
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
			candidates: []domain.Issue{candidateIssue("ISS-SSH", "ISS-SSH", "To Do")},
		}

		state := retryState(t, "ISS-SSH", "ISS-SSH", 2)
		state.RetryAttempts["ISS-SSH"].LastSSHHost = "host-b"

		params := defaultRetryParams(t, store, tracker)
		params.HostPool = hp

		ch := make(chan struct{}, 1)
		params.MakeWorkerFn = func(_, sshHost string) WorkerFunc {
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
			candidates: []domain.Issue{candidateIssue("ISS-FULL", "ISS-FULL", "To Do")},
		}

		state := retryState(t, "ISS-FULL", "ISS-FULL", 1)
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
		if entry.TimerHandle != nil {
			entry.TimerHandle.Stop()
		}
	})
}

func TestFindIssueByID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		issues []domain.Issue
		id     string
		want   bool
		wantID string
	}{
		{
			name:   "empty list",
			issues: nil,
			id:     "ISS-1",
			want:   false,
		},
		{
			name: "found",
			issues: []domain.Issue{
				candidateIssue("A", "A-1", "To Do"),
				candidateIssue("B", "B-1", "To Do"),
			},
			id:     "B",
			want:   true,
			wantID: "B",
		},
		{
			name: "not found",
			issues: []domain.Issue{
				candidateIssue("A", "A-1", "To Do"),
			},
			id:   "Z",
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, found := findIssueByID(tt.issues, tt.id)

			if found != tt.want {
				t.Fatalf("findIssueByID(%v, %q) found = %v, want %v", tt.issues, tt.id, found, tt.want)
			}
			if found && got.ID != tt.wantID {
				t.Errorf("findIssueByID(%v, %q).ID = %q, want %q", tt.issues, tt.id, got.ID, tt.wantID)
			}
		})
	}
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
		candidates: []domain.Issue{candidateIssue("ISS-WF", "ISS-WF", "To Do")},
	}

	state := retryState(t, "ISS-WF", "ISS-WF", 1)

	params := defaultRetryParams(t, store, tracker)
	params.WorkflowFile = "infra.WORKFLOW.md"

	workerCalled := make(chan struct{}, 1)
	params.MakeWorkerFn = func(_, _ string) WorkerFunc {
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
		candidates: []domain.Issue{candidateIssue(id, "PROJ-COMP", "To Do")},
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

func TestHandleRetryTimer_CIFailureContextPropagated(t *testing.T) {
	t.Parallel()

	// CI failure context carried on the retry entry should be forwarded to
	// the running entry so the worker can inject it into the turn prompt.
	const id = "ISS-CI-RETRY"
	ciContext := map[string]any{
		"status":        "failing",
		"failing_count": 3,
		"ref":           "feature/fix",
	}

	state := NewState(5000, 4, nil, AgentTotals{})
	state.Claimed[id] = struct{}{}
	state.RetryAttempts[id] = &RetryEntry{
		IssueID:          id,
		Identifier:       id,
		Attempt:          2,
		CIFailureContext: ciContext,
	}

	store := &mockRetryStore{}
	tracker := &mockRetryTracker{
		candidates: []domain.Issue{candidateIssue(id, id, "In Progress")},
	}
	params := defaultRetryParams(t, store, tracker)

	HandleRetryTimer(state, id, params)

	t.Cleanup(func() { state.WorkerWg.Wait() })

	entry, ok := state.Running[id]
	if !ok {
		t.Fatal("issue not dispatched; state.Running[id] missing")
	}
	if entry.CIFailureContext == nil {
		t.Fatal("RunningEntry.CIFailureContext is nil; want CI failure map")
	}
	if entry.CIFailureContext["status"] != "failing" {
		t.Errorf("CIFailureContext[status] = %v, want %q", entry.CIFailureContext["status"], "failing")
	}
	if entry.CIFailureContext["failing_count"] != 3 {
		t.Errorf("CIFailureContext[failing_count] = %v, want 3", entry.CIFailureContext["failing_count"])
	}
}

func TestHandleRetryTimer_NilCIFailureContext_NotPropagated(t *testing.T) {
	t.Parallel()

	// When the retry entry carries no CI failure context, the running entry
	// must not have one set either (field stays nil; no accidental injection).
	const id = "ISS-NO-CI"

	state := NewState(5000, 4, nil, AgentTotals{})
	state.Claimed[id] = struct{}{}
	state.RetryAttempts[id] = &RetryEntry{
		IssueID:          id,
		Identifier:       id,
		Attempt:          1,
		CIFailureContext: nil, // explicit nil
	}

	store := &mockRetryStore{}
	tracker := &mockRetryTracker{
		candidates: []domain.Issue{candidateIssue(id, id, "In Progress")},
	}
	params := defaultRetryParams(t, store, tracker)

	HandleRetryTimer(state, id, params)

	t.Cleanup(func() { state.WorkerWg.Wait() })

	entry, ok := state.Running[id]
	if !ok {
		t.Fatal("issue not dispatched; state.Running[id] missing")
	}
	if entry.CIFailureContext != nil {
		t.Errorf("RunningEntry.CIFailureContext = %v, want nil", entry.CIFailureContext)
	}
}
