package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
)

// --- Test doubles ---

// mockExitStore records calls to the WorkerExitStore interface methods and
// returns configurable errors. It satisfies [WorkerExitStore].
type mockExitStore struct {
	runHistories []persistence.RunHistory
	metrics      []persistence.AggregateMetrics
	retryEntries []persistence.RetryEntry

	appendRunHistoryErr       error
	upsertAggregateMetricsErr error
	saveRetryEntryErr         error
}

var _ WorkerExitStore = (*mockExitStore)(nil)

func (m *mockExitStore) AppendRunHistory(_ context.Context, run persistence.RunHistory) (persistence.RunHistory, error) {
	m.runHistories = append(m.runHistories, run)
	if m.appendRunHistoryErr != nil {
		return persistence.RunHistory{}, m.appendRunHistoryErr
	}
	run.ID = int64(len(m.runHistories))
	return run, nil
}

func (m *mockExitStore) UpsertAggregateMetrics(_ context.Context, metrics persistence.AggregateMetrics) error {
	m.metrics = append(m.metrics, metrics)
	return m.upsertAggregateMetricsErr
}

func (m *mockExitStore) SaveRetryEntry(_ context.Context, entry persistence.RetryEntry) error {
	m.retryEntries = append(m.retryEntries, entry)
	return m.saveRetryEntryErr
}

// --- Test helpers ---

// baseTime is a fixed reference time for deterministic tests.
var baseTime = time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)

// noopRetryFire is an OnRetryFire callback that does nothing.
func noopRetryFire(_ string) {}

// exitState creates a *State with a running entry and claim for the given
// issueID. The running entry's StartedAt is set to baseTime.
func exitState(t *testing.T, issueID string, retryAttempt *int) *State {
	t.Helper()
	state := NewState(5000, 4, nil, AgentTotals{})
	state.Running[issueID] = &RunningEntry{
		Identifier:   issueID + "-ident",
		StartedAt:    baseTime,
		RetryAttempt: retryAttempt,
	}
	state.Claimed[issueID] = struct{}{}
	return state
}

// defaultExitParams returns HandleWorkerExitParams with NowFunc fixed at
// baseTime + 60s, a fresh mockExitStore, and a discard logger.
func defaultExitParams(t *testing.T, store *mockExitStore) HandleWorkerExitParams {
	t.Helper()
	return HandleWorkerExitParams{
		Store:             store,
		MaxRetryBackoffMS: 300_000,
		OnRetryFire:       noopRetryFire,
		NowFunc:           func() time.Time { return baseTime.Add(60 * time.Second) },
		Logger:            discardLogger(),
	}
}

// --- Pure helper tests ---

func TestComputeBackoffDelay(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		attempt           int
		maxRetryBackoffMS int
		want              int64
	}{
		// Default cap (300000) — attempts 1..7 per spec §3.4
		{name: "attempt 1 default cap", attempt: 1, maxRetryBackoffMS: 300_000, want: 10_000},
		{name: "attempt 2 default cap", attempt: 2, maxRetryBackoffMS: 300_000, want: 20_000},
		{name: "attempt 3 default cap", attempt: 3, maxRetryBackoffMS: 300_000, want: 40_000},
		{name: "attempt 4 default cap", attempt: 4, maxRetryBackoffMS: 300_000, want: 80_000},
		{name: "attempt 5 default cap", attempt: 5, maxRetryBackoffMS: 300_000, want: 160_000},
		{name: "attempt 6 default cap", attempt: 6, maxRetryBackoffMS: 300_000, want: 300_000},
		{name: "attempt 7 default cap", attempt: 7, maxRetryBackoffMS: 300_000, want: 300_000},

		// Custom cap (60000)
		{name: "attempt 1 custom cap 60000", attempt: 1, maxRetryBackoffMS: 60_000, want: 10_000},
		{name: "attempt 2 custom cap 60000", attempt: 2, maxRetryBackoffMS: 60_000, want: 20_000},
		{name: "attempt 3 custom cap 60000", attempt: 3, maxRetryBackoffMS: 60_000, want: 40_000},
		{name: "attempt 4 custom cap 60000", attempt: 4, maxRetryBackoffMS: 60_000, want: 60_000},

		// Edge cases
		{name: "attempt 0 clamped to 1", attempt: 0, maxRetryBackoffMS: 300_000, want: 10_000},
		{name: "negative attempt clamped to 1", attempt: -5, maxRetryBackoffMS: 300_000, want: 10_000},
		{name: "zero cap uses default 300000", attempt: 6, maxRetryBackoffMS: 0, want: 300_000},
		{name: "negative cap uses default 300000", attempt: 6, maxRetryBackoffMS: -100, want: 300_000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := computeBackoffDelay(tt.attempt, tt.maxRetryBackoffMS)
			if got != tt.want {
				t.Errorf("computeBackoffDelay(%d, %d) = %d, want %d",
					tt.attempt, tt.maxRetryBackoffMS, got, tt.want)
			}
		})
	}
}

func TestMapExitKindToStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		kind WorkerExitKind
		want string
	}{
		{name: "normal", kind: WorkerExitNormal, want: "succeeded"},
		{name: "error", kind: WorkerExitError, want: "failed"},
		{name: "cancelled", kind: WorkerExitCancelled, want: "cancelled"},
		{name: "unknown", kind: WorkerExitKind("unknown"), want: "failed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := mapExitKindToStatus(tt.kind)
			if got != tt.want {
				t.Errorf("mapExitKindToStatus(%q) = %q, want %q", tt.kind, got, tt.want)
			}
		})
	}
}

func TestClassifyWorkerError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		err           error
		wantRetryable bool
	}{
		{
			name:          "AgentError agent_not_found is non-retryable",
			err:           &domain.AgentError{Kind: domain.ErrAgentNotFound, Message: "not found"},
			wantRetryable: false,
		},
		{
			name:          "AgentError turn_timeout is retryable",
			err:           &domain.AgentError{Kind: domain.ErrTurnTimeout, Message: "timeout"},
			wantRetryable: true,
		},
		{
			name:          "AgentError turn_input_required is non-retryable",
			err:           &domain.AgentError{Kind: domain.ErrTurnInputRequired, Message: "needs input"},
			wantRetryable: false,
		},
		{
			name:          "TrackerError tracker_auth_error is non-retryable",
			err:           &domain.TrackerError{Kind: domain.ErrTrackerAuth, Message: "unauthorized"},
			wantRetryable: false,
		},
		{
			name:          "TrackerError tracker_transport_error is retryable",
			err:           &domain.TrackerError{Kind: domain.ErrTrackerTransport, Message: "connection reset"},
			wantRetryable: true,
		},
		{
			name:          "wrapped AgentError preserves classification",
			err:           fmt.Errorf("worker failed: %w", &domain.AgentError{Kind: domain.ErrAgentNotFound, Message: "gone"}),
			wantRetryable: false,
		},
		{
			name:          "generic error defaults to retryable",
			err:           fmt.Errorf("something went wrong"),
			wantRetryable: true,
		},
		{
			name:          "nil error defaults to retryable",
			err:           nil,
			wantRetryable: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := classifyWorkerError(tt.err)
			if got.Retryable != tt.wantRetryable {
				t.Errorf("classifyWorkerError(%v).Retryable = %v, want %v",
					tt.err, got.Retryable, tt.wantRetryable)
			}
		})
	}
}

// --- HandleWorkerExit tests ---

func TestHandleWorkerExit_NormalExit(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "ISSUE-1", nil)
	params := defaultExitParams(t, store)

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "ISSUE-1",
		Identifier:    "ISSUE-1-ident",
		ExitKind:      WorkerExitNormal,
		AgentAdapter:  "mock",
		WorkspacePath: "/tmp/ws",
	}, params)

	// Running entry removed.
	if _, ok := state.Running["ISSUE-1"]; ok {
		t.Error("Running entry not removed after normal exit")
	}

	// Runtime seconds added (60s elapsed).
	if state.AgentTotals.SecondsRunning != 60 {
		t.Errorf("AgentTotals.SecondsRunning = %f, want 60", state.AgentTotals.SecondsRunning)
	}

	// Added to Completed set.
	if _, ok := state.Completed["ISSUE-1"]; !ok {
		t.Error("issue not added to Completed set after normal exit")
	}

	// Claim preserved.
	if _, ok := state.Claimed["ISSUE-1"]; !ok {
		t.Error("claim released after normal exit, should be preserved")
	}

	// Continuation retry scheduled: attempt=1.
	retryEntry, ok := state.RetryAttempts["ISSUE-1"]
	if !ok {
		t.Fatal("retry not scheduled after normal exit")
	}
	if retryEntry.Attempt != 1 {
		t.Errorf("retry Attempt = %d, want 1", retryEntry.Attempt)
	}
	if retryEntry.Error != "" {
		t.Errorf("retry Error = %q, want empty", retryEntry.Error)
	}

	// RunHistory persisted with status "succeeded".
	if len(store.runHistories) != 1 {
		t.Fatalf("AppendRunHistory called %d times, want 1", len(store.runHistories))
	}
	if store.runHistories[0].Status != "succeeded" {
		t.Errorf("RunHistory.Status = %q, want %q", store.runHistories[0].Status, "succeeded")
	}
	if store.runHistories[0].Error != nil {
		t.Errorf("RunHistory.Error = %v, want nil", store.runHistories[0].Error)
	}

	// AggregateMetrics persisted.
	if len(store.metrics) != 1 {
		t.Fatalf("UpsertAggregateMetrics called %d times, want 1", len(store.metrics))
	}
	if store.metrics[0].SecondsRunning != 60 {
		t.Errorf("AggregateMetrics.SecondsRunning = %f, want 60", store.metrics[0].SecondsRunning)
	}

	// Retry entry persisted.
	if len(store.retryEntries) != 1 {
		t.Fatalf("SaveRetryEntry called %d times, want 1", len(store.retryEntries))
	}
	if store.retryEntries[0].Attempt != 1 {
		t.Errorf("persisted retry Attempt = %d, want 1", store.retryEntries[0].Attempt)
	}
}

func TestHandleWorkerExit_RetryableError(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "ISSUE-2", nil) // RetryAttempt nil → NextAttempt returns 1
	params := defaultExitParams(t, store)

	turnTimeoutErr := &domain.AgentError{Kind: domain.ErrTurnTimeout, Message: "timed out"}

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "ISSUE-2",
		Identifier:    "ISSUE-2-ident",
		ExitKind:      WorkerExitError,
		Error:         turnTimeoutErr,
		AgentAdapter:  "mock",
		WorkspacePath: "/tmp/ws",
	}, params)

	// Running entry removed.
	if _, ok := state.Running["ISSUE-2"]; ok {
		t.Error("Running entry not removed after error exit")
	}

	// NOT added to Completed set.
	if _, ok := state.Completed["ISSUE-2"]; ok {
		t.Error("issue added to Completed set after error exit, should not be")
	}

	// Claim preserved.
	if _, ok := state.Claimed["ISSUE-2"]; !ok {
		t.Error("claim released after retryable error exit, should be preserved")
	}

	// Backoff retry scheduled: attempt=1, delay=10000ms.
	retryEntry, ok := state.RetryAttempts["ISSUE-2"]
	if !ok {
		t.Fatal("retry not scheduled after retryable error exit")
	}
	if retryEntry.Attempt != 1 {
		t.Errorf("retry Attempt = %d, want 1", retryEntry.Attempt)
	}
	if !strings.Contains(retryEntry.Error, "worker exited:") {
		t.Errorf("retry Error = %q, want to contain %q", retryEntry.Error, "worker exited:")
	}

	// RunHistory persisted with status "failed".
	if len(store.runHistories) != 1 {
		t.Fatalf("AppendRunHistory called %d times, want 1", len(store.runHistories))
	}
	if store.runHistories[0].Status != "failed" {
		t.Errorf("RunHistory.Status = %q, want %q", store.runHistories[0].Status, "failed")
	}
	if store.runHistories[0].Error == nil {
		t.Error("RunHistory.Error is nil, want error string")
	}

	// Retry entry persisted.
	if len(store.retryEntries) != 1 {
		t.Fatalf("SaveRetryEntry called %d times, want 1", len(store.retryEntries))
	}
}

func TestHandleWorkerExit_NonRetryableError(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "ISSUE-3", nil)
	params := defaultExitParams(t, store)

	notFoundErr := &domain.AgentError{Kind: domain.ErrAgentNotFound, Message: "binary missing"}

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "ISSUE-3",
		Identifier:    "ISSUE-3-ident",
		ExitKind:      WorkerExitError,
		Error:         notFoundErr,
		AgentAdapter:  "mock",
		WorkspacePath: "/tmp/ws",
	}, params)

	// Running entry removed.
	if _, ok := state.Running["ISSUE-3"]; ok {
		t.Error("Running entry not removed after non-retryable error exit")
	}

	// Claim released.
	if _, ok := state.Claimed["ISSUE-3"]; ok {
		t.Error("claim preserved after non-retryable error exit, should be released")
	}

	// No retry scheduled.
	if _, ok := state.RetryAttempts["ISSUE-3"]; ok {
		t.Error("retry scheduled after non-retryable error exit, should not be")
	}

	// NOT added to Completed set.
	if _, ok := state.Completed["ISSUE-3"]; ok {
		t.Error("issue added to Completed set after non-retryable error exit")
	}

	// RunHistory persisted with status "failed".
	if len(store.runHistories) != 1 {
		t.Fatalf("AppendRunHistory called %d times, want 1", len(store.runHistories))
	}
	if store.runHistories[0].Status != "failed" {
		t.Errorf("RunHistory.Status = %q, want %q", store.runHistories[0].Status, "failed")
	}

	// No retry entry persisted.
	if len(store.retryEntries) != 0 {
		t.Errorf("SaveRetryEntry called %d times, want 0", len(store.retryEntries))
	}
}

func TestHandleWorkerExit_CancelledExit(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "ISSUE-4", nil)
	params := defaultExitParams(t, store)

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "ISSUE-4",
		Identifier:    "ISSUE-4-ident",
		ExitKind:      WorkerExitCancelled,
		AgentAdapter:  "mock",
		WorkspacePath: "/tmp/ws",
	}, params)

	// Running entry removed.
	if _, ok := state.Running["ISSUE-4"]; ok {
		t.Error("Running entry not removed after cancelled exit")
	}

	// Claim released.
	if _, ok := state.Claimed["ISSUE-4"]; ok {
		t.Error("claim preserved after cancelled exit, should be released")
	}

	// No retry scheduled.
	if _, ok := state.RetryAttempts["ISSUE-4"]; ok {
		t.Error("retry scheduled after cancelled exit, should not be")
	}

	// NOT added to Completed set.
	if _, ok := state.Completed["ISSUE-4"]; ok {
		t.Error("issue added to Completed set after cancelled exit")
	}

	// RunHistory persisted with status "cancelled".
	if len(store.runHistories) != 1 {
		t.Fatalf("AppendRunHistory called %d times, want 1", len(store.runHistories))
	}
	if store.runHistories[0].Status != "cancelled" {
		t.Errorf("RunHistory.Status = %q, want %q", store.runHistories[0].Status, "cancelled")
	}

	// No retry entry persisted.
	if len(store.retryEntries) != 0 {
		t.Errorf("SaveRetryEntry called %d times, want 0", len(store.retryEntries))
	}
}

func TestHandleWorkerExit_RuntimeSecondsAccounting(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "ISSUE-5", nil)
	// Pre-seed some existing seconds to verify additive behavior.
	state.AgentTotals.SecondsRunning = 100.0

	params := defaultExitParams(t, store)
	// Return baseTime + 90.5s to get exactly 90.5 seconds elapsed.
	params.NowFunc = func() time.Time {
		return baseTime.Add(90*time.Second + 500*time.Millisecond)
	}

	HandleWorkerExit(state, WorkerResult{
		IssueID:      "ISSUE-5",
		Identifier:   "ISSUE-5-ident",
		ExitKind:     WorkerExitNormal,
		AgentAdapter: "mock",
	}, params)

	// 100.0 (pre-existing) + 90.5 (this run) = 190.5
	want := 190.5
	if state.AgentTotals.SecondsRunning != want {
		t.Errorf("AgentTotals.SecondsRunning = %f, want %f", state.AgentTotals.SecondsRunning, want)
	}

	// Persisted metrics reflect the updated total.
	if len(store.metrics) != 1 {
		t.Fatalf("UpsertAggregateMetrics called %d times, want 1", len(store.metrics))
	}
	if store.metrics[0].SecondsRunning != want {
		t.Errorf("AggregateMetrics.SecondsRunning = %f, want %f", store.metrics[0].SecondsRunning, want)
	}
}

func TestHandleWorkerExit_PersistenceFailureNonFatal(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{
		appendRunHistoryErr:       errors.New("db write failed"),
		upsertAggregateMetricsErr: errors.New("db write failed"),
		saveRetryEntryErr:         errors.New("db write failed"),
	}
	state := exitState(t, "ISSUE-6", nil)
	params := defaultExitParams(t, store)

	// Must not panic despite all store operations failing.
	HandleWorkerExit(state, WorkerResult{
		IssueID:      "ISSUE-6",
		Identifier:   "ISSUE-6-ident",
		ExitKind:     WorkerExitNormal,
		AgentAdapter: "mock",
	}, params)

	// In-memory state mutations still occurred.
	if _, ok := state.Running["ISSUE-6"]; ok {
		t.Error("Running entry not removed despite persistence failure")
	}
	if _, ok := state.Completed["ISSUE-6"]; !ok {
		t.Error("Completed set not updated despite persistence failure")
	}
	if _, ok := state.RetryAttempts["ISSUE-6"]; !ok {
		t.Error("retry not scheduled despite persistence failure")
	}

	// Store was still called (errors were returned but calls were made).
	if len(store.runHistories) != 1 {
		t.Errorf("AppendRunHistory called %d times, want 1", len(store.runHistories))
	}
	if len(store.metrics) != 1 {
		t.Errorf("UpsertAggregateMetrics called %d times, want 1", len(store.metrics))
	}
	if len(store.retryEntries) != 1 {
		t.Errorf("SaveRetryEntry called %d times, want 1", len(store.retryEntries))
	}
}

func TestHandleWorkerExit_UnknownIssueNoOp(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := NewState(5000, 4, nil, AgentTotals{})
	params := defaultExitParams(t, store)

	// Call with an issueID not in state.Running.
	HandleWorkerExit(state, WorkerResult{
		IssueID:    "GHOST-999",
		Identifier: "GHOST-999",
		ExitKind:   WorkerExitNormal,
	}, params)

	// No state changes.
	if len(state.Running) != 0 {
		t.Errorf("Running map modified: len=%d, want 0", len(state.Running))
	}
	if len(state.Completed) != 0 {
		t.Errorf("Completed set modified: len=%d, want 0", len(state.Completed))
	}
	if state.AgentTotals != (AgentTotals{}) {
		t.Errorf("AgentTotals modified: %+v, want zero value", state.AgentTotals)
	}

	// No store calls.
	if len(store.runHistories) != 0 {
		t.Errorf("AppendRunHistory called %d times, want 0", len(store.runHistories))
	}
	if len(store.metrics) != 0 {
		t.Errorf("UpsertAggregateMetrics called %d times, want 0", len(store.metrics))
	}
	if len(store.retryEntries) != 0 {
		t.Errorf("SaveRetryEntry called %d times, want 0", len(store.retryEntries))
	}
}

func TestHandleWorkerExit_RetryAttemptNilIncrementsToOne(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "ISSUE-7", nil) // RetryAttempt nil
	params := defaultExitParams(t, store)

	HandleWorkerExit(state, WorkerResult{
		IssueID:      "ISSUE-7",
		Identifier:   "ISSUE-7-ident",
		ExitKind:     WorkerExitError,
		Error:        &domain.AgentError{Kind: domain.ErrTurnTimeout, Message: "timeout"},
		AgentAdapter: "mock",
	}, params)

	retryEntry, ok := state.RetryAttempts["ISSUE-7"]
	if !ok {
		t.Fatal("retry not scheduled for retryable error with nil RetryAttempt")
	}
	if retryEntry.Attempt != 1 {
		t.Errorf("retry Attempt = %d, want 1 (NextAttempt from nil)", retryEntry.Attempt)
	}
}

func TestHandleWorkerExit_RetryAttemptIncrements(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	attempt := 3
	state := exitState(t, "ISSUE-8", &attempt) // RetryAttempt = 3
	params := defaultExitParams(t, store)

	HandleWorkerExit(state, WorkerResult{
		IssueID:      "ISSUE-8",
		Identifier:   "ISSUE-8-ident",
		ExitKind:     WorkerExitError,
		Error:        &domain.AgentError{Kind: domain.ErrPortExit, Message: "crashed"},
		AgentAdapter: "mock",
	}, params)

	retryEntry, ok := state.RetryAttempts["ISSUE-8"]
	if !ok {
		t.Fatal("retry not scheduled for retryable error with RetryAttempt=3")
	}
	// NextAttempt(3) = 4; computeBackoffDelay(4, 300000) = 80000.
	if retryEntry.Attempt != 4 {
		t.Errorf("retry Attempt = %d, want 4", retryEntry.Attempt)
	}
}

func TestHandleWorkerExit_ClaimPreservedOnRetryablePaths(t *testing.T) {
	t.Parallel()

	t.Run("normal exit preserves claim", func(t *testing.T) {
		t.Parallel()
		store := &mockExitStore{}
		state := exitState(t, "CLAIM-1", nil)
		params := defaultExitParams(t, store)

		HandleWorkerExit(state, WorkerResult{
			IssueID:      "CLAIM-1",
			Identifier:   "CLAIM-1-ident",
			ExitKind:     WorkerExitNormal,
			AgentAdapter: "mock",
		}, params)

		if _, ok := state.Claimed["CLAIM-1"]; !ok {
			t.Error("claim released after normal exit, should be preserved")
		}
	})

	t.Run("retryable error preserves claim", func(t *testing.T) {
		t.Parallel()
		store := &mockExitStore{}
		state := exitState(t, "CLAIM-2", nil)
		params := defaultExitParams(t, store)

		HandleWorkerExit(state, WorkerResult{
			IssueID:      "CLAIM-2",
			Identifier:   "CLAIM-2-ident",
			ExitKind:     WorkerExitError,
			Error:        &domain.AgentError{Kind: domain.ErrTurnTimeout, Message: "timeout"},
			AgentAdapter: "mock",
		}, params)

		if _, ok := state.Claimed["CLAIM-2"]; !ok {
			t.Error("claim released after retryable error, should be preserved")
		}
	})
}

func TestHandleWorkerExit_ClaimReleasedOnNonRetryableAndCancelled(t *testing.T) {
	t.Parallel()

	t.Run("non-retryable error releases claim", func(t *testing.T) {
		t.Parallel()
		store := &mockExitStore{}
		state := exitState(t, "REL-1", nil)
		params := defaultExitParams(t, store)

		HandleWorkerExit(state, WorkerResult{
			IssueID:      "REL-1",
			Identifier:   "REL-1-ident",
			ExitKind:     WorkerExitError,
			Error:        &domain.AgentError{Kind: domain.ErrAgentNotFound, Message: "not found"},
			AgentAdapter: "mock",
		}, params)

		if _, ok := state.Claimed["REL-1"]; ok {
			t.Error("claim preserved after non-retryable error, should be released")
		}
		if _, ok := state.RetryAttempts["REL-1"]; ok {
			t.Error("retry scheduled after non-retryable error, should not be")
		}
	})

	t.Run("cancelled exit releases claim", func(t *testing.T) {
		t.Parallel()
		store := &mockExitStore{}
		state := exitState(t, "REL-2", nil)
		params := defaultExitParams(t, store)

		HandleWorkerExit(state, WorkerResult{
			IssueID:      "REL-2",
			Identifier:   "REL-2-ident",
			ExitKind:     WorkerExitCancelled,
			AgentAdapter: "mock",
		}, params)

		if _, ok := state.Claimed["REL-2"]; ok {
			t.Error("claim preserved after cancelled exit, should be released")
		}
		if _, ok := state.RetryAttempts["REL-2"]; ok {
			t.Error("retry scheduled after cancelled exit, should not be")
		}
	})
}

func TestHandleWorkerExit_RunHistoryFields(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	attempt := 2
	state := exitState(t, "HIST-1", &attempt)
	state.Running["HIST-1"].Identifier = "PROJ-42"
	params := defaultExitParams(t, store)

	exitErr := &domain.AgentError{Kind: domain.ErrTurnFailed, Message: "assertion failed"}

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "HIST-1",
		Identifier:    "PROJ-42",
		ExitKind:      WorkerExitError,
		Error:         exitErr,
		AgentAdapter:  "claude-code",
		WorkspacePath: "/workspaces/PROJ-42",
	}, params)

	if len(store.runHistories) != 1 {
		t.Fatalf("AppendRunHistory called %d times, want 1", len(store.runHistories))
	}

	rh := store.runHistories[0]
	if rh.IssueID != "HIST-1" {
		t.Errorf("RunHistory.IssueID = %q, want %q", rh.IssueID, "HIST-1")
	}
	if rh.Identifier != "PROJ-42" {
		t.Errorf("RunHistory.Identifier = %q, want %q", rh.Identifier, "PROJ-42")
	}
	if rh.Attempt != 2 {
		t.Errorf("RunHistory.Attempt = %d, want 2", rh.Attempt)
	}
	if rh.AgentAdapter != "claude-code" {
		t.Errorf("RunHistory.AgentAdapter = %q, want %q", rh.AgentAdapter, "claude-code")
	}
	if rh.Workspace != "/workspaces/PROJ-42" {
		t.Errorf("RunHistory.Workspace = %q, want %q", rh.Workspace, "/workspaces/PROJ-42")
	}
	if rh.StartedAt != baseTime.Format(time.RFC3339) {
		t.Errorf("RunHistory.StartedAt = %q, want %q", rh.StartedAt, baseTime.Format(time.RFC3339))
	}

	wantCompleted := baseTime.Add(60 * time.Second).Format(time.RFC3339)
	if rh.CompletedAt != wantCompleted {
		t.Errorf("RunHistory.CompletedAt = %q, want %q", rh.CompletedAt, wantCompleted)
	}
	if rh.Status != "failed" {
		t.Errorf("RunHistory.Status = %q, want %q", rh.Status, "failed")
	}
	if rh.Error == nil {
		t.Fatal("RunHistory.Error = nil, want error string")
	}
	if !strings.Contains(*rh.Error, "assertion failed") {
		t.Errorf("RunHistory.Error = %q, want to contain %q", *rh.Error, "assertion failed")
	}
}
