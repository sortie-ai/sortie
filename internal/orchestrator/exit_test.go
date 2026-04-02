package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
)

// --- Test doubles ---

// mockExitStore records calls to the WorkerExitStore interface methods and
// returns configurable errors. It satisfies [WorkerExitStore].
type mockExitStore struct {
	runHistories    []persistence.RunHistory
	metrics         []persistence.AggregateMetrics
	sessionMetadata []persistence.SessionMetadata
	retryEntries    []persistence.RetryEntry

	appendRunHistoryErr       error
	upsertAggregateMetricsErr error
	upsertSessionMetadataErr  error
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

func (m *mockExitStore) UpsertSessionMetadata(_ context.Context, meta persistence.SessionMetadata) error {
	m.sessionMetadata = append(m.sessionMetadata, meta)
	return m.upsertSessionMetadataErr
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
		// Default cap (300000) — attempts 1..7.
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
		upsertSessionMetadataErr:  errors.New("db write failed"),
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
	if len(store.sessionMetadata) != 1 {
		t.Errorf("UpsertSessionMetadata called %d times, want 1", len(store.sessionMetadata))
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
	if len(store.sessionMetadata) != 0 {
		t.Errorf("UpsertSessionMetadata called %d times, want 0", len(store.sessionMetadata))
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
	if rh.Attempt != 3 {
		t.Errorf("RunHistory.Attempt = %d, want 3", rh.Attempt)
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

func TestHandleWorkerExit_SessionMetadataPersisted(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "SM-1", nil)
	// Populate session and token data on the running entry.
	entry := state.Running["SM-1"]
	entry.SessionID = "ses-abc"
	entry.AgentPID = "12345"
	entry.AgentInputTokens = 500
	entry.AgentOutputTokens = 200
	entry.AgentTotalTokens = 700
	params := defaultExitParams(t, store)

	HandleWorkerExit(state, WorkerResult{
		IssueID:      "SM-1",
		Identifier:   "SM-1-ident",
		ExitKind:     WorkerExitNormal,
		SessionID:    "ses-abc",
		AgentAdapter: "mock",
	}, params)

	if len(store.sessionMetadata) != 1 {
		t.Fatalf("UpsertSessionMetadata called %d times, want 1", len(store.sessionMetadata))
	}

	sm := store.sessionMetadata[0]
	if sm.IssueID != "SM-1" {
		t.Errorf("SessionMetadata.IssueID = %q, want %q", sm.IssueID, "SM-1")
	}
	if sm.SessionID != "ses-abc" {
		t.Errorf("SessionMetadata.SessionID = %q, want %q", sm.SessionID, "ses-abc")
	}
	if sm.AgentPID == nil || *sm.AgentPID != "12345" {
		t.Errorf("SessionMetadata.AgentPID = %v, want %q", sm.AgentPID, "12345")
	}
	if sm.InputTokens != 500 {
		t.Errorf("SessionMetadata.InputTokens = %d, want 500", sm.InputTokens)
	}
	if sm.OutputTokens != 200 {
		t.Errorf("SessionMetadata.OutputTokens = %d, want 200", sm.OutputTokens)
	}
	if sm.TotalTokens != 700 {
		t.Errorf("SessionMetadata.TotalTokens = %d, want 700", sm.TotalTokens)
	}

	wantUpdated := baseTime.Add(60 * time.Second).Format(time.RFC3339)
	if sm.UpdatedAt != wantUpdated {
		t.Errorf("SessionMetadata.UpdatedAt = %q, want %q", sm.UpdatedAt, wantUpdated)
	}
}

func TestHandleWorkerExit_SessionMetadataNilPID(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "SM-2", nil)
	// AgentPID left as empty string (default).
	params := defaultExitParams(t, store)

	HandleWorkerExit(state, WorkerResult{
		IssueID:      "SM-2",
		Identifier:   "SM-2-ident",
		ExitKind:     WorkerExitNormal,
		AgentAdapter: "mock",
	}, params)

	if len(store.sessionMetadata) != 1 {
		t.Fatalf("UpsertSessionMetadata called %d times, want 1", len(store.sessionMetadata))
	}
	if store.sessionMetadata[0].AgentPID != nil {
		t.Errorf("SessionMetadata.AgentPID = %v, want nil for empty PID", store.sessionMetadata[0].AgentPID)
	}
}

func TestHandleWorkerExit_SessionIDPrefersResult(t *testing.T) {
	t.Parallel()

	t.Run("result.SessionID overrides entry.SessionID", func(t *testing.T) {
		t.Parallel()
		store := &mockExitStore{}
		state := exitState(t, "SID-1", nil)
		state.Running["SID-1"].SessionID = "stale-ses"
		params := defaultExitParams(t, store)

		HandleWorkerExit(state, WorkerResult{
			IssueID:      "SID-1",
			Identifier:   "SID-1-ident",
			ExitKind:     WorkerExitNormal,
			SessionID:    "fresh-ses",
			AgentAdapter: "mock",
		}, params)

		if len(store.sessionMetadata) != 1 {
			t.Fatalf("UpsertSessionMetadata called %d times, want 1", len(store.sessionMetadata))
		}
		if store.sessionMetadata[0].SessionID != "fresh-ses" {
			t.Errorf("SessionMetadata.SessionID = %q, want %q (from result, not entry)",
				store.sessionMetadata[0].SessionID, "fresh-ses")
		}
	})

	t.Run("falls back to entry.SessionID when result is empty", func(t *testing.T) {
		t.Parallel()
		store := &mockExitStore{}
		state := exitState(t, "SID-2", nil)
		state.Running["SID-2"].SessionID = "entry-ses"
		params := defaultExitParams(t, store)

		HandleWorkerExit(state, WorkerResult{
			IssueID:      "SID-2",
			Identifier:   "SID-2-ident",
			ExitKind:     WorkerExitNormal,
			SessionID:    "",
			AgentAdapter: "mock",
		}, params)

		if len(store.sessionMetadata) != 1 {
			t.Fatalf("UpsertSessionMetadata called %d times, want 1", len(store.sessionMetadata))
		}
		if store.sessionMetadata[0].SessionID != "entry-ses" {
			t.Errorf("SessionMetadata.SessionID = %q, want %q (fallback from entry)",
				store.sessionMetadata[0].SessionID, "entry-ses")
		}
	})
}

// --- Cancelled exit: retry claim preservation ---

func TestHandleWorkerExit_CancelledWithPreScheduledRetryKeepsClaim(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "CAN-1", nil)
	// Pre-schedule a retry (simulates reconciliation stall detection scheduling
	// a retry before the cancelled worker exits).
	state.RetryAttempts["CAN-1"] = &RetryEntry{
		IssueID:    "CAN-1",
		Identifier: "CAN-1-ident",
		Attempt:    2,
	}
	params := defaultExitParams(t, store)

	HandleWorkerExit(state, WorkerResult{
		IssueID:      "CAN-1",
		Identifier:   "CAN-1-ident",
		ExitKind:     WorkerExitCancelled,
		AgentAdapter: "mock",
	}, params)

	// Running entry removed.
	if _, ok := state.Running["CAN-1"]; ok {
		t.Error("Running entry not removed after cancelled exit")
	}

	// Claim preserved because a retry is pre-scheduled.
	if _, ok := state.Claimed["CAN-1"]; !ok {
		t.Error("claim released despite pre-scheduled retry")
	}

	// Retry entry preserved.
	if _, ok := state.RetryAttempts["CAN-1"]; !ok {
		t.Error("pre-scheduled retry entry removed")
	}
}

func TestHandleWorkerExit_CancelledWithoutRetryReleasesClaim(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "CAN-2", nil)
	// No pre-scheduled retry.
	params := defaultExitParams(t, store)

	HandleWorkerExit(state, WorkerResult{
		IssueID:      "CAN-2",
		Identifier:   "CAN-2-ident",
		ExitKind:     WorkerExitCancelled,
		AgentAdapter: "mock",
	}, params)

	// Running entry removed.
	if _, ok := state.Running["CAN-2"]; ok {
		t.Error("Running entry not removed after cancelled exit")
	}

	// Claim released — no pre-scheduled retry.
	if _, ok := state.Claimed["CAN-2"]; ok {
		t.Error("claim preserved without pre-scheduled retry")
	}
}

// --- PendingCleanup: workspace removal on exit ---

func TestHandleWorkerExit_PendingCleanupRemovesWorkspace(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "CLEAN-1", nil)
	state.Running["CLEAN-1"].PendingCleanup = true
	state.Running["CLEAN-1"].Identifier = "CLEAN-1-ident"

	// Create a real workspace directory to verify removal.
	wsRoot := t.TempDir()
	wsDir := filepath.Join(wsRoot, "CLEAN-1-ident")
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatalf("failed to create workspace dir: %v", err)
	}

	params := defaultExitParams(t, store)

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "CLEAN-1",
		Identifier:    "CLEAN-1-ident",
		ExitKind:      WorkerExitCancelled,
		AgentAdapter:  "mock",
		WorkspacePath: wsDir,
	}, params)

	// Workspace directory removed.
	if _, err := os.Stat(wsDir); !os.IsNotExist(err) {
		t.Errorf("workspace directory still exists after PendingCleanup exit")
	}
}

func TestHandleWorkerExit_NoPendingCleanupSkipsWorkspace(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "NOCLEAN-1", nil)
	// PendingCleanup is false (default).
	state.Running["NOCLEAN-1"].Identifier = "NOCLEAN-1-ident"

	wsRoot := t.TempDir()
	wsDir := filepath.Join(wsRoot, "NOCLEAN-1-ident")
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatalf("failed to create workspace dir: %v", err)
	}

	params := defaultExitParams(t, store)

	HandleWorkerExit(state, WorkerResult{
		IssueID:      "NOCLEAN-1",
		Identifier:   "NOCLEAN-1-ident",
		ExitKind:     WorkerExitNormal,
		AgentAdapter: "mock",
	}, params)

	// Workspace directory still exists — no cleanup.
	if _, err := os.Stat(wsDir); err != nil {
		t.Errorf("workspace directory removed despite PendingCleanup=false: %v", err)
	}
}

func TestHandleWorkerExit_CleanupFailureNonFatal(t *testing.T) {
	t.Parallel()

	if os.Getuid() == 0 {
		t.Skip("skipping: test requires non-root to enforce directory permissions")
	}

	store := &mockExitStore{}
	state := exitState(t, "CFAIL-1", nil)
	state.Running["CFAIL-1"].PendingCleanup = true
	state.Running["CFAIL-1"].Identifier = "CFAIL-1-ident"

	// Create a workspace directory where os.RemoveAll will fail:
	// a child directory inside a non-writable parent prevents unlinking.
	wsDir := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(filepath.Join(wsDir, "locked"), 0o755); err != nil {
		t.Fatalf("failed to create dir: %v", err)
	}
	if err := os.Chmod(wsDir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(wsDir, 0o755) })

	params := defaultExitParams(t, store)

	// Must not panic; cleanup error is logged but not fatal.
	HandleWorkerExit(state, WorkerResult{
		IssueID:       "CFAIL-1",
		Identifier:    "CFAIL-1-ident",
		ExitKind:      WorkerExitCancelled,
		AgentAdapter:  "mock",
		WorkspacePath: wsDir,
	}, params)

	// In-memory state still updated despite cleanup failure.
	if _, ok := state.Running["CFAIL-1"]; ok {
		t.Error("Running entry not removed despite cleanup failure")
	}
}

// TestHandleWorkerExit_PendingCleanupUsesActualPath verifies that workspace
// cleanup uses the path recorded by the worker, not a path recomputed from the
// current config, preventing orphaned workspaces after a live config reload.
func TestHandleWorkerExit_PendingCleanupUsesActualPath(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "PROJ-99", nil)
	state.Running["PROJ-99"].PendingCleanup = true
	state.Running["PROJ-99"].Identifier = "PROJ-99"

	// Two separate roots: oldRoot has the actual workspace; newRoot
	// simulates config changing workspace.root at runtime.
	oldRoot := t.TempDir()
	newRoot := t.TempDir()
	actualWS := filepath.Join(oldRoot, "PROJ-99")
	if err := os.MkdirAll(actualWS, 0o755); err != nil {
		t.Fatalf("failed to create workspace dir: %v", err)
	}

	params := defaultExitParams(t, store)

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "PROJ-99",
		Identifier:    "PROJ-99",
		ExitKind:      WorkerExitCancelled,
		AgentAdapter:  "mock",
		WorkspacePath: actualWS, // actual path at old root
	}, params)

	// Actual workspace at old root is cleaned.
	if _, err := os.Stat(actualWS); !os.IsNotExist(err) {
		t.Error("workspace at old root still exists, cleanup used wrong path")
	}

	// New root was never touched — no directory created there.
	newRootWS := filepath.Join(newRoot, "PROJ-99")
	if _, err := os.Stat(newRootWS); !os.IsNotExist(err) {
		t.Error("directory exists at new root, cleanup should not touch it")
	}
}

// --- Handoff transition tests ---

// exitStateWithIssue creates a *State with a running entry whose
// Issue.State is set to issueState. Used by handoff transition tests.
func exitStateWithIssue(t *testing.T, issueID, issueState string) *State {
	t.Helper()
	state := exitState(t, issueID, nil)
	state.Running[issueID].Issue.State = issueState
	return state
}

func TestHandleWorkerExit_HandoffTransitionSucceeds(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{}
	state := exitStateWithIssue(t, "HO-1", "In Progress")
	params := defaultExitParams(t, store)
	params.TrackerAdapter = tracker
	params.HandoffState = "Human Review"
	params.ActiveStates = []string{"In Progress"}

	HandleWorkerExit(state, WorkerResult{
		IssueID:      "HO-1",
		Identifier:   "HO-1-ident",
		ExitKind:     WorkerExitNormal,
		AgentAdapter: "mock",
	}, params)

	// TransitionIssue called once with correct args.
	if len(tracker.transitionCalls) != 1 {
		t.Fatalf("TransitionIssue called %d times, want 1", len(tracker.transitionCalls))
	}
	if tracker.transitionCalls[0].IssueID != "HO-1" {
		t.Errorf("TransitionIssue IssueID = %q, want %q", tracker.transitionCalls[0].IssueID, "HO-1")
	}
	if tracker.transitionCalls[0].TargetState != "Human Review" {
		t.Errorf("TransitionIssue TargetState = %q, want %q", tracker.transitionCalls[0].TargetState, "Human Review")
	}

	// No retry scheduled.
	if _, ok := state.RetryAttempts["HO-1"]; ok {
		t.Error("retry scheduled after successful handoff transition, should not be")
	}

	// Claim released.
	if _, ok := state.Claimed["HO-1"]; ok {
		t.Error("claim preserved after successful handoff transition, should be released")
	}

	// Added to Completed set.
	if _, ok := state.Completed["HO-1"]; !ok {
		t.Error("issue not added to Completed set after handoff transition")
	}

	// No retry entry persisted.
	if len(store.retryEntries) != 0 {
		t.Errorf("SaveRetryEntry called %d times, want 0", len(store.retryEntries))
	}
}

func TestHandleWorkerExit_HandoffTransitionFails(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{
		transitionIssueFn: func(_ context.Context, _, _ string) error {
			return errors.New("permission denied")
		},
	}
	state := exitStateWithIssue(t, "HO-2", "In Progress")
	params := defaultExitParams(t, store)
	params.TrackerAdapter = tracker
	params.HandoffState = "Human Review"
	params.ActiveStates = []string{"In Progress"}

	HandleWorkerExit(state, WorkerResult{
		IssueID:      "HO-2",
		Identifier:   "HO-2-ident",
		ExitKind:     WorkerExitNormal,
		AgentAdapter: "mock",
	}, params)

	// TransitionIssue called once.
	if len(tracker.transitionCalls) != 1 {
		t.Fatalf("TransitionIssue called %d times, want 1", len(tracker.transitionCalls))
	}

	// Continuation retry scheduled (attempt=1).
	retryEntry, ok := state.RetryAttempts["HO-2"]
	if !ok {
		t.Fatal("retry not scheduled after failed handoff transition")
	}
	if retryEntry.Attempt != 1 {
		t.Errorf("retry Attempt = %d, want 1", retryEntry.Attempt)
	}

	// Claim preserved.
	if _, ok := state.Claimed["HO-2"]; !ok {
		t.Error("claim released after failed handoff transition, should be preserved")
	}

	// Added to Completed set.
	if _, ok := state.Completed["HO-2"]; !ok {
		t.Error("issue not added to Completed set after failed handoff")
	}

	// Retry entry persisted.
	if len(store.retryEntries) != 1 {
		t.Fatalf("SaveRetryEntry called %d times, want 1", len(store.retryEntries))
	}
}

func TestHandleWorkerExit_HandoffNotConfigured(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{}
	state := exitStateWithIssue(t, "HO-3", "In Progress")
	params := defaultExitParams(t, store)
	params.TrackerAdapter = tracker
	params.HandoffState = ""
	params.ActiveStates = []string{"In Progress"}

	HandleWorkerExit(state, WorkerResult{
		IssueID:      "HO-3",
		Identifier:   "HO-3-ident",
		ExitKind:     WorkerExitNormal,
		AgentAdapter: "mock",
	}, params)

	// TransitionIssue NOT called.
	if len(tracker.transitionCalls) != 0 {
		t.Errorf("TransitionIssue called %d times, want 0", len(tracker.transitionCalls))
	}

	// Continuation retry scheduled (existing behavior).
	retryEntry, ok := state.RetryAttempts["HO-3"]
	if !ok {
		t.Fatal("retry not scheduled when handoff is not configured")
	}
	if retryEntry.Attempt != 1 {
		t.Errorf("retry Attempt = %d, want 1", retryEntry.Attempt)
	}

	// Claim preserved.
	if _, ok := state.Claimed["HO-3"]; !ok {
		t.Error("claim released when handoff not configured, should be preserved")
	}
}

func TestHandleWorkerExit_HandoffConfiguredButIssueNotActive(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{}
	state := exitStateWithIssue(t, "HO-4", "Done")
	params := defaultExitParams(t, store)
	params.TrackerAdapter = tracker
	params.HandoffState = "Human Review"
	params.ActiveStates = []string{"In Progress"}

	HandleWorkerExit(state, WorkerResult{
		IssueID:      "HO-4",
		Identifier:   "HO-4-ident",
		ExitKind:     WorkerExitNormal,
		AgentAdapter: "mock",
	}, params)

	// TransitionIssue NOT called — issue is not active.
	if len(tracker.transitionCalls) != 0 {
		t.Errorf("TransitionIssue called %d times, want 0", len(tracker.transitionCalls))
	}

	// No retry scheduled.
	if _, ok := state.RetryAttempts["HO-4"]; ok {
		t.Error("retry scheduled for non-active issue, should not be")
	}

	// Claim released.
	if _, ok := state.Claimed["HO-4"]; ok {
		t.Error("claim preserved for non-active issue, should be released")
	}
}

func TestHandleWorkerExit_NormalExitIssueNotActive_NoHandoff(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitStateWithIssue(t, "HO-5", "Done")
	params := defaultExitParams(t, store)
	params.HandoffState = ""
	params.ActiveStates = []string{"In Progress"}

	HandleWorkerExit(state, WorkerResult{
		IssueID:      "HO-5",
		Identifier:   "HO-5-ident",
		ExitKind:     WorkerExitNormal,
		AgentAdapter: "mock",
	}, params)

	// No retry scheduled.
	if _, ok := state.RetryAttempts["HO-5"]; ok {
		t.Error("retry scheduled for non-active issue without handoff, should not be")
	}

	// Claim released.
	if _, ok := state.Claimed["HO-5"]; ok {
		t.Error("claim preserved for non-active issue, should be released")
	}

	// No retry entry persisted.
	if len(store.retryEntries) != 0 {
		t.Errorf("SaveRetryEntry called %d times, want 0", len(store.retryEntries))
	}
}

func TestHandleWorkerExit_EmptyActiveStatesDefaultsToContinuationRetry(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitStateWithIssue(t, "HO-6", "In Progress")
	params := defaultExitParams(t, store)
	params.HandoffState = ""
	params.ActiveStates = nil // backward compat guard

	HandleWorkerExit(state, WorkerResult{
		IssueID:      "HO-6",
		Identifier:   "HO-6-ident",
		ExitKind:     WorkerExitNormal,
		AgentAdapter: "mock",
	}, params)

	// Continuation retry scheduled (backward compat: empty ActiveStates
	// treated as "issue is active").
	if _, ok := state.RetryAttempts["HO-6"]; !ok {
		t.Error("retry not scheduled with empty ActiveStates, backward compat guard failed")
	}

	// Claim preserved.
	if _, ok := state.Claimed["HO-6"]; !ok {
		t.Error("claim released with empty ActiveStates, should be preserved")
	}
}

func TestHandleWorkerExit_PendingCleanupSkipsWhenNoWorkspacePath(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "NOWSP-1", nil)
	state.Running["NOWSP-1"].PendingCleanup = true
	state.Running["NOWSP-1"].Identifier = "NOWSP-1-ident"

	// Create a directory that would match the old ComputePath derivation;
	// it must NOT be removed when WorkspacePath is empty.
	wsRoot := t.TempDir()
	oldPathDir := filepath.Join(wsRoot, "NOWSP-1-ident")
	if err := os.MkdirAll(oldPathDir, 0o755); err != nil {
		t.Fatalf("failed to create workspace dir: %v", err)
	}

	params := defaultExitParams(t, store)

	// Worker exited before workspace preparation — WorkspacePath is empty.
	HandleWorkerExit(state, WorkerResult{
		IssueID:       "NOWSP-1",
		Identifier:    "NOWSP-1-ident",
		ExitKind:      WorkerExitCancelled,
		AgentAdapter:  "mock",
		WorkspacePath: "",
	}, params)

	// Running entry removed.
	if _, ok := state.Running["NOWSP-1"]; ok {
		t.Error("Running entry not removed")
	}

	// Directory at wsRoot is NOT removed — no workspace path means no cleanup.
	if _, err := os.Stat(oldPathDir); err != nil {
		t.Errorf("workspace dir removed despite empty WorkspacePath: %v", err)
	}

	// Claim handling proceeds normally (cancelled exit releases claim).
	if _, ok := state.Claimed["NOWSP-1"]; ok {
		t.Error("claim not released after cancelled exit")
	}
}

func TestHandleWorkerExit_RetryableErrorLogsWarn(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	store := &mockExitStore{}
	state := exitState(t, "LOGW-1", nil)
	params := defaultExitParams(t, store)
	params.Logger = debugLogger(t, &buf)

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "LOGW-1",
		Identifier:    "LOGW-1-ident",
		ExitKind:      WorkerExitError,
		Error:         &domain.AgentError{Kind: domain.ErrTurnTimeout, Message: "timed out"},
		AgentAdapter:  "mock",
		WorkspacePath: "/tmp/ws",
	}, params)

	out := buf.String()
	expectedDelayMs := computeBackoffDelay(NextAttempt(nil), params.MaxRetryBackoffMS)
	for _, want := range []string{
		"level=WARN",
		`msg="worker run failed, scheduling retry"`,
		"next_attempt=1",
		fmt.Sprintf("delay_ms=%d", expectedDelayMs),
		"timed out",
		"issue_id=LOGW-1",
		"issue_identifier=LOGW-1-ident",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log output missing %q\ngot: %s", want, out)
		}
	}

	// No "worker run failed" at ERROR level.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "level=ERROR") && strings.Contains(line, "worker run failed") {
			t.Errorf("unexpected ERROR log with 'worker run failed':\n%s", line)
		}
	}
}

func TestHandleWorkerExit_NonRetryableErrorLogsError(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	store := &mockExitStore{}
	state := exitState(t, "LOGE-1", nil)
	params := defaultExitParams(t, store)
	params.Logger = debugLogger(t, &buf)

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "LOGE-1",
		Identifier:    "LOGE-1-ident",
		ExitKind:      WorkerExitError,
		Error:         &domain.AgentError{Kind: domain.ErrAgentNotFound, Message: "binary missing"},
		AgentAdapter:  "mock",
		WorkspacePath: "/tmp/ws",
	}, params)

	out := buf.String()
	for _, want := range []string{
		"level=ERROR",
		`msg="worker run failed, non-retryable, releasing claim"`,
		"binary missing",
		"issue_id=LOGE-1",
		"issue_identifier=LOGE-1-ident",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log output missing %q\ngot: %s", want, out)
		}
	}

	// No "worker run failed" at WARN level.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, "worker run failed") {
			t.Errorf("unexpected WARN log with 'worker run failed':\n%s", line)
		}
	}
}

func TestHandleWorkerExit_NormalExitNoWorkerFailedLog(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	store := &mockExitStore{}
	state := exitState(t, "LOGN-1", nil)
	params := defaultExitParams(t, store)
	params.Logger = debugLogger(t, &buf)

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "LOGN-1",
		Identifier:    "LOGN-1-ident",
		ExitKind:      WorkerExitNormal,
		AgentAdapter:  "mock",
		WorkspacePath: "/tmp/ws",
	}, params)

	out := buf.String()
	if strings.Contains(out, "worker run failed") {
		t.Errorf("normal exit should not emit 'worker run failed' log\ngot: %s", out)
	}
}

// --- SSH Host Pool integration tests ---

func TestHandleWorkerExit_ReleasesSSHHost(t *testing.T) {
	t.Parallel()

	hp := NewHostPool([]string{"host-a", "host-b"}, 2)
	hp.AcquireHost("ISSUE-SSH", "host-a")

	store := &mockExitStore{}
	state := exitState(t, "ISSUE-SSH", nil)
	state.Running["ISSUE-SSH"].SSHHost = "host-a"
	params := defaultExitParams(t, store)
	params.HostPool = hp

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "ISSUE-SSH",
		Identifier:    "ISSUE-SSH-ident",
		ExitKind:      WorkerExitNormal,
		SSHHost:       "host-a",
		AgentAdapter:  "mock",
		WorkspacePath: "/tmp/ws",
	}, params)

	// Host slot released.
	snap := hp.Snapshot()
	if snap["host-a"] != 0 {
		t.Errorf("host-a usage = %d after exit, want 0", snap["host-a"])
	}
	if got := hp.HostFor("ISSUE-SSH"); got != "" {
		t.Errorf("HostFor(ISSUE-SSH) = %q after exit, want empty", got)
	}
}

func TestHandleWorkerExit_NilHostPoolSafe(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "ISSUE-NIL", nil)
	params := defaultExitParams(t, store)
	// HostPool is nil (default) — should not panic.

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "ISSUE-NIL",
		Identifier:    "ISSUE-NIL-ident",
		ExitKind:      WorkerExitNormal,
		SSHHost:       "some-host",
		AgentAdapter:  "mock",
		WorkspacePath: "/tmp/ws",
	}, params)

	// Normal exit path completed.
	if _, ok := state.Running["ISSUE-NIL"]; ok {
		t.Error("Running entry not removed after exit with nil HostPool")
	}
}

func TestHandleWorkerExit_LastSSHHostPropagated(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "ISSUE-PROP", nil)
	params := defaultExitParams(t, store)

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "ISSUE-PROP",
		Identifier:    "ISSUE-PROP-ident",
		ExitKind:      WorkerExitNormal,
		SSHHost:       "worker-7",
		AgentAdapter:  "mock",
		WorkspacePath: "/tmp/ws",
	}, params)

	// Continuation retry should have LastSSHHost set.
	entry, ok := state.RetryAttempts["ISSUE-PROP"]
	if !ok {
		t.Fatal("retry not scheduled after normal exit")
	}
	if entry.LastSSHHost != "worker-7" {
		t.Errorf("RetryEntry.LastSSHHost = %q, want %q", entry.LastSSHHost, "worker-7")
	}
}

func TestHandleWorkerExit_WorkflowFilePersisted(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "WF-1", nil)
	// WorkflowFile is set on the running entry to simulate it being captured at
	// dispatch time.
	state.Running["WF-1"].WorkflowFile = "backend.WORKFLOW.md"
	params := defaultExitParams(t, store)

	HandleWorkerExit(state, WorkerResult{
		IssueID:      "WF-1",
		Identifier:   "WF-1-ident",
		ExitKind:     WorkerExitNormal,
		AgentAdapter: "mock",
	}, params)

	if len(store.runHistories) != 1 {
		t.Fatalf("AppendRunHistory called %d times, want 1", len(store.runHistories))
	}
	if got := store.runHistories[0].WorkflowFile; got != "backend.WORKFLOW.md" {
		t.Errorf("RunHistory.WorkflowFile = %q, want %q", got, "backend.WORKFLOW.md")
	}
}

// --- Comment builder unit tests ---

// commentAwareMetrics wraps spyMetrics and signals done when IncTrackerComments
// is called. It lets tests synchronize with the detached comment goroutine
// spawned by HandleWorkerExit without using sleep.
type commentAwareMetrics struct {
	*spyMetrics
	done chan struct{}
}

var _ domain.Metrics = (*commentAwareMetrics)(nil)

func newCommentAwareMetrics() *commentAwareMetrics {
	return &commentAwareMetrics{
		spyMetrics: &spyMetrics{},
		done:       make(chan struct{}, 1),
	}
}

func (m *commentAwareMetrics) IncTrackerComments(lifecycle, result string) {
	m.spyMetrics.IncTrackerComments(lifecycle, result)
	m.done <- struct{}{}
}

// waitComment blocks until IncTrackerComments is called or 2 s elapses.
func (m *commentAwareMetrics) waitComment(t *testing.T) {
	t.Helper()
	select {
	case <-m.done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for tracker comment goroutine to call IncTrackerComments")
	}
}

func TestBuildCompletionComment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		sessionID      string
		elapsed        time.Duration
		turnsCompleted int
		retryScheduled bool
		wantContains   []string
		wantAbsent     []string
	}{
		{
			name:           "completed no retry",
			sessionID:      "ses-abc",
			elapsed:        90 * time.Second,
			turnsCompleted: 5,
			retryScheduled: false,
			wantContains:   []string{"Sortie session completed.", "ses-abc", "1m30s", "5"},
			wantAbsent:     []string{"re-queuing"},
		},
		{
			name:           "completed with re-queuing",
			sessionID:      "ses-def",
			elapsed:        90 * time.Second,
			turnsCompleted: 3,
			retryScheduled: true,
			wantContains:   []string{"Sortie session completed (re-queuing).", "ses-def", "3"},
		},
		{
			name:           "empty session ID replaced with unknown",
			sessionID:      "",
			elapsed:        10 * time.Second,
			turnsCompleted: 1,
			retryScheduled: false,
			wantContains:   []string{"unknown"},
		},
		{
			name:           "sub-second elapsed truncated to zero",
			sessionID:      "ses-xyz",
			elapsed:        500 * time.Millisecond,
			turnsCompleted: 0,
			retryScheduled: false,
			wantContains:   []string{"0s"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := buildCompletionComment(tt.sessionID, tt.elapsed, tt.turnsCompleted, tt.retryScheduled)
			for _, want := range tt.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("buildCompletionComment() missing %q\ngot: %q", want, got)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("buildCompletionComment() should not contain %q\ngot: %q", absent, got)
				}
			}
		})
	}
}

func TestBuildFailureComment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		sessionID      string
		elapsed        time.Duration
		exitErr        error
		retryScheduled bool
		nextAttempt    int
		wantContains   []string
	}{
		{
			name:           "failure with retry scheduled",
			sessionID:      "ses-xyz",
			elapsed:        45 * time.Second,
			exitErr:        errors.New("process killed"),
			retryScheduled: true,
			nextAttempt:    2,
			wantContains:   []string{"Sortie session failed.", "ses-xyz", "45s", "process killed", "Retry: yes (attempt 2)"},
		},
		{
			name:           "failure no retry",
			sessionID:      "ses-abc",
			elapsed:        30 * time.Second,
			exitErr:        errors.New("binary not found"),
			retryScheduled: false,
			nextAttempt:    0,
			wantContains:   []string{"Sortie session failed.", "ses-abc", "binary not found", "Retry: no"},
		},
		{
			name:           "nil error reports unknown error",
			sessionID:      "ses-def",
			elapsed:        10 * time.Second,
			exitErr:        nil,
			retryScheduled: false,
			nextAttempt:    0,
			wantContains:   []string{"Sortie session failed.", "unknown error"},
		},
		{
			name:           "empty session ID replaced with unknown",
			sessionID:      "",
			elapsed:        5 * time.Second,
			exitErr:        errors.New("crash"),
			retryScheduled: false,
			nextAttempt:    0,
			wantContains:   []string{"unknown"},
		},
		{
			name:           "long error message is truncated",
			sessionID:      "ses-long",
			elapsed:        1 * time.Second,
			exitErr:        errors.New(strings.Repeat("x", 300)),
			retryScheduled: false,
			nextAttempt:    0,
			wantContains:   []string{"..."},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := buildFailureComment(tt.sessionID, tt.elapsed, tt.exitErr, tt.retryScheduled, tt.nextAttempt)
			for _, want := range tt.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("buildFailureComment() missing %q\ngot: %q", want, got)
				}
			}
		})
	}
}

// --- HandleWorkerExit tracker comment integration tests ---

// exitParamsWithComments returns defaultExitParams extended with a tracker
// adapter and the given comments config. ActiveStates is set so the issue
// is not active, keeping retryScheduled=false on normal exit for clean assertions.
func exitParamsWithComments(t *testing.T, store *mockExitStore, tracker *mockTrackerAdapter, comments config.TrackerCommentsConfig) HandleWorkerExitParams {
	t.Helper()
	p := defaultExitParams(t, store)
	p.TrackerAdapter = tracker
	p.ActiveStates = []string{"In Progress"} // issue state "" is not active → retryScheduled=false on normal exit
	p.CommentsConfig = comments
	return p
}

// TestHandleWorkerExit_CommentOnNormalExit verifies that a normal worker exit with
// OnCompletion=true calls CommentIssue with a completion comment and records
// IncTrackerComments("completion", "success").
func TestHandleWorkerExit_CommentOnNormalExit(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{}
	spy := newCommentAwareMetrics()

	state := exitState(t, "CMT-1", nil) // issue.State="" not in ActiveStates → retryScheduled=false
	params := exitParamsWithComments(t, store, tracker, config.TrackerCommentsConfig{OnCompletion: true})
	params.Metrics = spy

	HandleWorkerExit(state, WorkerResult{
		IssueID:        "CMT-1",
		Identifier:     "CMT-1-ident",
		ExitKind:       WorkerExitNormal,
		SessionID:      "ses-cmt1",
		TurnsCompleted: 7,
		AgentAdapter:   "mock",
	}, params)

	spy.waitComment(t)

	// CommentIssue called once with the right issue and completion text.
	if len(tracker.commentCalls) != 1 {
		t.Fatalf("CommentIssue call count = %d, want 1", len(tracker.commentCalls))
	}
	if tracker.commentCalls[0].IssueID != "CMT-1" {
		t.Errorf("CommentIssue IssueID = %q, want %q", tracker.commentCalls[0].IssueID, "CMT-1")
	}
	if !strings.Contains(tracker.commentCalls[0].Text, "Sortie session completed.") {
		t.Errorf("completion comment missing headline\ngot: %q", tracker.commentCalls[0].Text)
	}
	if !strings.Contains(tracker.commentCalls[0].Text, "ses-cmt1") {
		t.Errorf("completion comment missing session ID\ngot: %q", tracker.commentCalls[0].Text)
	}

	// IncTrackerComments recorded with lifecycle=completion, result=success.
	spy.mu.Lock()
	comments := append([]trackerCommentCall(nil), spy.trackerComments...)
	spy.mu.Unlock()

	if len(comments) != 1 {
		t.Fatalf("IncTrackerComments call count = %d, want 1", len(comments))
	}
	if comments[0].lifecycle != "completion" {
		t.Errorf("IncTrackerComments lifecycle = %q, want %q", comments[0].lifecycle, "completion")
	}
	if comments[0].result != "success" {
		t.Errorf("IncTrackerComments result = %q, want %q", comments[0].result, "success")
	}
}

// TestHandleWorkerExit_NoCommentWhenOnCompletionFalse verifies that a normal exit
// with OnCompletion=false does not call CommentIssue.
func TestHandleWorkerExit_NoCommentWhenOnCompletionFalse(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{}
	state := exitState(t, "CMT-2", nil)
	params := exitParamsWithComments(t, store, tracker, config.TrackerCommentsConfig{OnCompletion: false})

	HandleWorkerExit(state, WorkerResult{
		IssueID:      "CMT-2",
		Identifier:   "CMT-2-ident",
		ExitKind:     WorkerExitNormal,
		SessionID:    "ses-cmt2",
		AgentAdapter: "mock",
	}, params)

	// No goroutine spawned — assert immediately.
	if len(tracker.commentCalls) != 0 {
		t.Errorf("CommentIssue call count = %d, want 0 (OnCompletion=false)", len(tracker.commentCalls))
	}
}

// TestHandleWorkerExit_CommentOnErrorExit verifies that an error worker exit with
// OnFailure=true calls CommentIssue with a failure comment and records
// IncTrackerComments("failure", "success").
func TestHandleWorkerExit_CommentOnErrorExit(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{}
	spy := newCommentAwareMetrics()

	state := exitState(t, "CMT-3", nil)
	params := exitParamsWithComments(t, store, tracker, config.TrackerCommentsConfig{OnFailure: true})
	params.Metrics = spy

	exitErr := &domain.AgentError{Kind: domain.ErrTurnTimeout, Message: "turn timed out"}

	HandleWorkerExit(state, WorkerResult{
		IssueID:      "CMT-3",
		Identifier:   "CMT-3-ident",
		ExitKind:     WorkerExitError,
		Error:        exitErr,
		SessionID:    "ses-cmt3",
		AgentAdapter: "mock",
	}, params)

	spy.waitComment(t)

	// CommentIssue called once with failure text.
	if len(tracker.commentCalls) != 1 {
		t.Fatalf("CommentIssue call count = %d, want 1", len(tracker.commentCalls))
	}
	if !strings.Contains(tracker.commentCalls[0].Text, "Sortie session failed.") {
		t.Errorf("failure comment missing headline\ngot: %q", tracker.commentCalls[0].Text)
	}
	if !strings.Contains(tracker.commentCalls[0].Text, "ses-cmt3") {
		t.Errorf("failure comment missing session ID\ngot: %q", tracker.commentCalls[0].Text)
	}

	// IncTrackerComments recorded with lifecycle=failure, result=success.
	spy.mu.Lock()
	comments := append([]trackerCommentCall(nil), spy.trackerComments...)
	spy.mu.Unlock()

	if len(comments) != 1 {
		t.Fatalf("IncTrackerComments call count = %d, want 1", len(comments))
	}
	if comments[0].lifecycle != "failure" {
		t.Errorf("IncTrackerComments lifecycle = %q, want %q", comments[0].lifecycle, "failure")
	}
	if comments[0].result != "success" {
		t.Errorf("IncTrackerComments result = %q, want %q", comments[0].result, "success")
	}
}

// TestHandleWorkerExit_NoCommentWhenOnFailureFalse verifies that an error exit
// with OnFailure=false does not call CommentIssue.
func TestHandleWorkerExit_NoCommentWhenOnFailureFalse(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{}
	state := exitState(t, "CMT-4", nil)
	params := exitParamsWithComments(t, store, tracker, config.TrackerCommentsConfig{OnFailure: false})

	HandleWorkerExit(state, WorkerResult{
		IssueID:      "CMT-4",
		Identifier:   "CMT-4-ident",
		ExitKind:     WorkerExitError,
		Error:        &domain.AgentError{Kind: domain.ErrTurnTimeout, Message: "timeout"},
		AgentAdapter: "mock",
	}, params)

	// No goroutine spawned — assert immediately.
	if len(tracker.commentCalls) != 0 {
		t.Errorf("CommentIssue call count = %d, want 0 (OnFailure=false)", len(tracker.commentCalls))
	}
}

// TestHandleWorkerExit_NoCommentOnCancelled verifies that a cancelled worker exit
// never posts a comment regardless of the OnCompletion/OnFailure flags.
func TestHandleWorkerExit_NoCommentOnCancelled(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{}
	state := exitState(t, "CMT-5", nil)
	// Both flags enabled — still no comment for cancellation.
	params := exitParamsWithComments(t, store, tracker, config.TrackerCommentsConfig{
		OnCompletion: true,
		OnFailure:    true,
	})

	HandleWorkerExit(state, WorkerResult{
		IssueID:      "CMT-5",
		Identifier:   "CMT-5-ident",
		ExitKind:     WorkerExitCancelled,
		AgentAdapter: "mock",
	}, params)

	// No goroutine spawned — assert immediately.
	if len(tracker.commentCalls) != 0 {
		t.Errorf("CommentIssue call count = %d, want 0 (cancelled exit)", len(tracker.commentCalls))
	}
}

// TestHandleWorkerExit_CommentErrorIsNonFatal verifies that a CommentIssue failure
// is non-fatal: the function does not panic, IncTrackerComments records an error
// result, and a WARN log entry is emitted.
func TestHandleWorkerExit_CommentErrorIsNonFatal(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	store := &mockExitStore{}
	spy := newCommentAwareMetrics()
	tracker := &mockTrackerAdapter{
		commentIssueFn: func(_ context.Context, _, _ string) error {
			return errors.New("tracker API unavailable")
		},
	}

	state := exitState(t, "CMT-6", nil)
	params := exitParamsWithComments(t, store, tracker, config.TrackerCommentsConfig{OnFailure: true})
	params.Metrics = spy
	params.Logger = debugLogger(t, &buf)

	HandleWorkerExit(state, WorkerResult{
		IssueID:      "CMT-6",
		Identifier:   "CMT-6-ident",
		ExitKind:     WorkerExitError,
		Error:        &domain.AgentError{Kind: domain.ErrTurnTimeout, Message: "timed out"},
		SessionID:    "ses-cmt6",
		AgentAdapter: "mock",
	}, params)

	// Wait for the goroutine to complete and IncTrackerComments to be called.
	spy.waitComment(t)

	// IncTrackerComments called with result=error.
	spy.mu.Lock()
	comments := append([]trackerCommentCall(nil), spy.trackerComments...)
	spy.mu.Unlock()

	if len(comments) != 1 {
		t.Fatalf("IncTrackerComments call count = %d, want 1", len(comments))
	}
	if comments[0].result != "error" {
		t.Errorf("IncTrackerComments result = %q, want %q", comments[0].result, "error")
	}
	if comments[0].lifecycle != "failure" {
		t.Errorf("IncTrackerComments lifecycle = %q, want %q", comments[0].lifecycle, "failure")
	}

	// WARN log emitted with "tracker comment failed".
	logOut := buf.String()
	if !strings.Contains(logOut, "tracker comment failed") {
		t.Errorf("log missing %q\ngot: %s", "tracker comment failed", logOut)
	}
	if !strings.Contains(logOut, "level=WARN") {
		t.Errorf("expected WARN level log\ngot: %s", logOut)
	}
}

// TestHandleWorkerExit_CommentNilTrackerAdapterSafe verifies that nil TrackerAdapter
// with a comments config enabled does not panic and does not attempt to post a comment.
func TestHandleWorkerExit_CommentNilTrackerAdapterSafe(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "CMT-7", nil)
	params := defaultExitParams(t, store)
	params.TrackerAdapter = nil // explicit nil
	params.CommentsConfig = config.TrackerCommentsConfig{OnCompletion: true, OnFailure: true}

	// Must not panic.
	HandleWorkerExit(state, WorkerResult{
		IssueID:      "CMT-7",
		Identifier:   "CMT-7-ident",
		ExitKind:     WorkerExitNormal,
		AgentAdapter: "mock",
	}, params)

	// Normal in-memory state updates still happened.
	if _, ok := state.Running["CMT-7"]; ok {
		t.Error("Running entry not removed after normal exit with nil TrackerAdapter")
	}
}

// TestHandleWorkerExit_CommentSessionIDPrefersResult verifies that when both
// result.SessionID and entry.SessionID are set, result.SessionID is used in the
// comment text, matching the comment version of the session ID resolution rule.
func TestHandleWorkerExit_CommentSessionIDPrefersResult(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{}
	spy := newCommentAwareMetrics()

	state := exitState(t, "CMT-8", nil)
	state.Running["CMT-8"].SessionID = "entry-ses" // stale value on entry
	params := exitParamsWithComments(t, store, tracker, config.TrackerCommentsConfig{OnCompletion: true})
	params.Metrics = spy

	HandleWorkerExit(state, WorkerResult{
		IssueID:      "CMT-8",
		Identifier:   "CMT-8-ident",
		ExitKind:     WorkerExitNormal,
		SessionID:    "result-ses", // authoritative value from adapter
		AgentAdapter: "mock",
	}, params)

	spy.waitComment(t)

	if len(tracker.commentCalls) != 1 {
		t.Fatalf("CommentIssue call count = %d, want 1", len(tracker.commentCalls))
	}
	text := tracker.commentCalls[0].Text
	if !strings.Contains(text, "result-ses") {
		t.Errorf("comment text should contain result.SessionID %q\ngot: %q", "result-ses", text)
	}
	if strings.Contains(text, "entry-ses") {
		t.Errorf("comment text should not contain entry.SessionID %q\ngot: %q", "entry-ses", text)
	}
}

// --- Attempt numbering and TurnsCompleted persistence ---

func TestHandleWorkerExit_FirstDispatchAttemptIsOne(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	// nil RetryAttempt simulates a first-dispatch run (never retried before).
	state := exitState(t, "FD-1", nil)
	params := defaultExitParams(t, store)

	HandleWorkerExit(state, WorkerResult{
		IssueID:      "FD-1",
		Identifier:   "FD-1-ident",
		ExitKind:     WorkerExitNormal,
		AgentAdapter: "mock",
	}, params)

	if len(store.runHistories) != 1 {
		t.Fatalf("AppendRunHistory called %d times, want 1", len(store.runHistories))
	}
	if store.runHistories[0].Attempt != 1 {
		t.Errorf("RunHistory.Attempt = %d, want 1 for first-dispatch run", store.runHistories[0].Attempt)
	}
}

func TestHandleWorkerExit_TurnsCompletedPersisted(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "TC-1", nil)
	params := defaultExitParams(t, store)

	HandleWorkerExit(state, WorkerResult{
		IssueID:        "TC-1",
		Identifier:     "TC-1-ident",
		ExitKind:       WorkerExitNormal,
		AgentAdapter:   "mock",
		TurnsCompleted: 5,
	}, params)

	if len(store.runHistories) != 1 {
		t.Fatalf("AppendRunHistory called %d times, want 1", len(store.runHistories))
	}
	if store.runHistories[0].TurnsCompleted != 5 {
		t.Errorf("RunHistory.TurnsCompleted = %d, want 5", store.runHistories[0].TurnsCompleted)
	}
}

func TestHandleWorkerExit_TurnsCompletedZeroWhenNoTurnsRan(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "TC-2", nil)
	params := defaultExitParams(t, store)

	HandleWorkerExit(state, WorkerResult{
		IssueID:      "TC-2",
		Identifier:   "TC-2-ident",
		ExitKind:     WorkerExitError,
		AgentAdapter: "mock",
		Error:        errors.New("workspace prep failed"),
		// TurnsCompleted is zero-value — worker never reached the turn loop.
	}, params)

	if len(store.runHistories) != 1 {
		t.Fatalf("AppendRunHistory called %d times, want 1", len(store.runHistories))
	}
	if store.runHistories[0].TurnsCompleted != 0 {
		t.Errorf("RunHistory.TurnsCompleted = %d, want 0 when no turns ran", store.runHistories[0].TurnsCompleted)
	}
}

// --- Soft-stop tests ---

// TestHandleWorkerExit_SoftStop verifies the A2O soft-stop exit path:
// claim released, no retry scheduled, added to Completed, metrics use
// "soft_stop" exit type, and the run history status is "succeeded".
func TestHandleWorkerExit_SoftStop(t *testing.T) {
	t.Parallel()

	t.Run("releases_claim_suppresses_retry", func(t *testing.T) {
		t.Parallel()

		store := &mockExitStore{}
		state := exitState(t, "SS-1", nil)
		state.Running["SS-1"].Issue.State = "In Progress"
		params := defaultExitParams(t, store)
		// Set ActiveStates so issue is active; without soft-stop this would
		// trigger a continuation retry.
		params.ActiveStates = []string{"In Progress"}

		HandleWorkerExit(state, WorkerResult{
			IssueID:        "SS-1",
			Identifier:     "SS-1-ident",
			ExitKind:       WorkerExitNormal,
			AgentAdapter:   "mock",
			SoftStop:       true,
			SoftStopReason: "blocked",
		}, params)

		// Claim released on soft-stop.
		if _, ok := state.Claimed["SS-1"]; ok {
			t.Error("claim preserved after soft-stop, want released")
		}

		// No continuation retry scheduled.
		if _, ok := state.RetryAttempts["SS-1"]; ok {
			t.Error("retry scheduled after soft-stop, want suppressed")
		}

		// Added to Completed.
		if _, ok := state.Completed["SS-1"]; !ok {
			t.Error("issue not added to Completed set after soft-stop")
		}

		// No retry entry persisted.
		if len(store.retryEntries) != 0 {
			t.Errorf("SaveRetryEntry called %d times, want 0", len(store.retryEntries))
		}
	})

	t.Run("run_history_status_is_succeeded", func(t *testing.T) {
		t.Parallel()

		store := &mockExitStore{}
		state := exitState(t, "SS-2", nil)
		params := defaultExitParams(t, store)

		HandleWorkerExit(state, WorkerResult{
			IssueID:        "SS-2",
			Identifier:     "SS-2-ident",
			ExitKind:       WorkerExitNormal,
			AgentAdapter:   "mock",
			TurnsCompleted: 3,
			SoftStop:       true,
			SoftStopReason: "blocked",
		}, params)

		if len(store.runHistories) != 1 {
			t.Fatalf("AppendRunHistory called %d times, want 1", len(store.runHistories))
		}
		if store.runHistories[0].Status != "succeeded" {
			t.Errorf("RunHistory.Status = %q, want %q", store.runHistories[0].Status, "succeeded")
		}
		if store.runHistories[0].TurnsCompleted != 3 {
			t.Errorf("RunHistory.TurnsCompleted = %d, want 3", store.runHistories[0].TurnsCompleted)
		}
	})

	t.Run("metrics_worker_exit_is_soft_stop_not_normal", func(t *testing.T) {
		t.Parallel()

		store := &mockExitStore{}
		spy := &spyMetrics{}
		state := exitState(t, "SS-3", nil)
		params := defaultExitParams(t, store)
		params.Metrics = spy

		HandleWorkerExit(state, WorkerResult{
			IssueID:        "SS-3",
			Identifier:     "SS-3-ident",
			ExitKind:       WorkerExitNormal,
			AgentAdapter:   "mock",
			SoftStop:       true,
			SoftStopReason: "needs-human-review",
		}, params)

		spy.mu.Lock()
		exits := append([]string(nil), spy.workerExits...)
		spy.mu.Unlock()

		if len(exits) != 1 {
			t.Fatalf("IncWorkerExits called %d times, want 1", len(exits))
		}
		if exits[0] != exitTypeSoftStop {
			t.Errorf("IncWorkerExits(%q), want %q", exits[0], exitTypeSoftStop)
		}
	})

	t.Run("normal_exit_without_soft_stop_still_schedules_retry", func(t *testing.T) {
		t.Parallel()

		// Regression guard: SoftStop=false + active issue → continuation retry.
		store := &mockExitStore{}
		state := exitState(t, "SS-4", nil)
		state.Running["SS-4"].Issue.State = "In Progress"
		params := defaultExitParams(t, store)
		params.ActiveStates = []string{"In Progress"}

		HandleWorkerExit(state, WorkerResult{
			IssueID:      "SS-4",
			Identifier:   "SS-4-ident",
			ExitKind:     WorkerExitNormal,
			AgentAdapter: "mock",
			SoftStop:     false,
		}, params)

		if _, ok := state.RetryAttempts["SS-4"]; !ok {
			t.Error("retry not scheduled for normal exit with active issue, regression guard failed")
		}
		if _, ok := state.Claimed["SS-4"]; !ok {
			t.Error("claim released after normal exit with retry, want preserved")
		}
	})

	t.Run("soft_stop_posts_comment_when_on_completion_enabled", func(t *testing.T) {
		t.Parallel()

		store := &mockExitStore{}
		tracker := &mockTrackerAdapter{}
		spy := newCommentAwareMetrics()

		state := exitState(t, "SS-5", nil)
		params := exitParamsWithComments(t, store, tracker, config.TrackerCommentsConfig{OnCompletion: true})
		params.Metrics = spy

		HandleWorkerExit(state, WorkerResult{
			IssueID:        "SS-5",
			Identifier:     "SS-5-ident",
			ExitKind:       WorkerExitNormal,
			SessionID:      "ses-ss5",
			TurnsCompleted: 2,
			AgentAdapter:   "mock",
			SoftStop:       true,
			SoftStopReason: "blocked",
		}, params)

		spy.waitComment(t)

		if len(tracker.commentCalls) != 1 {
			t.Fatalf("CommentIssue call count = %d, want 1", len(tracker.commentCalls))
		}
		text := tracker.commentCalls[0].Text
		for _, want := range []string{
			"agent signaled: blocked",
			"ses-ss5",
			"Turns: 2",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("soft-stop comment missing %q\ngot: %q", want, text)
			}
		}
		if strings.Contains(text, "re-queuing") {
			t.Errorf("soft-stop comment should not contain %q\ngot: %q", "re-queuing", text)
		}
	})

	// CancelRetry removes a pre-existing RetryAttempts entry when soft-stop fires,
	// even when a retry was pre-scheduled (e.g. from a stall-timeout reschedule)
	// before the agent exited.
	t.Run("cancels_preexisting_retry_entry", func(t *testing.T) {
		t.Parallel()

		store := &mockExitStore{}
		state := exitState(t, "SS-6", nil)
		// Seed a pre-existing retry entry, simulating a stall-timeout
		// reschedule that arrived before the worker soft-stop result.
		preexisting := &RetryEntry{
			IssueID:    "SS-6",
			Identifier: "SS-6-ident",
			Attempt:    2,
			// Use a long-lived timer so it does not fire during the test.
			TimerHandle: time.AfterFunc(1*time.Hour, func() {}),
		}
		state.RetryAttempts["SS-6"] = preexisting
		params := defaultExitParams(t, store)
		params.ActiveStates = []string{"In Progress"}
		state.Running["SS-6"].Issue.State = "In Progress"

		HandleWorkerExit(state, WorkerResult{
			IssueID:        "SS-6",
			Identifier:     "SS-6-ident",
			ExitKind:       WorkerExitNormal,
			AgentAdapter:   "mock",
			SoftStop:       true,
			SoftStopReason: "blocked",
		}, params)

		// CancelRetry must have removed the pre-existing entry.
		if _, ok := state.RetryAttempts["SS-6"]; ok {
			t.Error("pre-existing RetryAttempts entry not removed by CancelRetry on soft-stop")
		}

		// CancelRetry must have stopped the timer, not only deleted the map entry.
		// Stop() returns false when the timer was already stopped; true means it
		// was still live — a bug where CancelRetry skipped the Stop() call.
		if preexisting.TimerHandle.Stop() {
			t.Error("timer was not stopped by CancelRetry: Stop() returned true (timer was still live)")
		}

		// Claim released and no new retry entry persisted.
		if _, ok := state.Claimed["SS-6"]; ok {
			t.Error("claim preserved after soft-stop with pre-existing retry, want released")
		}
		if len(store.retryEntries) != 0 {
			t.Errorf("SaveRetryEntry called %d times, want 0", len(store.retryEntries))
		}
	})

	// SoftStop is checked before the handoff branch in the inner switch, so a
	// configured HandoffState must not trigger a tracker transition when SoftStop
	// is true.
	t.Run("handoff_skipped_when_soft_stop", func(t *testing.T) {
		t.Parallel()

		store := &mockExitStore{}
		tracker := &mockTrackerAdapter{}
		state := exitState(t, "SS-7", nil)
		state.Running["SS-7"].Issue.State = "In Progress"
		params := defaultExitParams(t, store)
		params.ActiveStates = []string{"In Progress"}
		params.HandoffState = "In Review"
		params.TrackerAdapter = tracker

		HandleWorkerExit(state, WorkerResult{
			IssueID:        "SS-7",
			Identifier:     "SS-7-ident",
			ExitKind:       WorkerExitNormal,
			AgentAdapter:   "mock",
			SoftStop:       true,
			SoftStopReason: "blocked",
		}, params)

		// Handoff must not have been attempted.
		if len(tracker.transitionCalls) != 0 {
			t.Errorf("TransitionIssue called %d times, want 0 (handoff must be skipped when SoftStop is true)",
				len(tracker.transitionCalls))
		}

		// Claim released.
		if _, ok := state.Claimed["SS-7"]; ok {
			t.Error("claim preserved after soft-stop, want released")
		}

		// No retry scheduled.
		if _, ok := state.RetryAttempts["SS-7"]; ok {
			t.Error("retry scheduled after soft-stop with handoff configured, want suppressed")
		}
	})
}

// TestBuildSoftStopComment verifies the format of the soft-stop comment string.
func TestBuildSoftStopComment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		sessionID      string
		elapsed        time.Duration
		turnsCompleted int
		reason         string
		wantContains   []string
		wantAbsent     []string
	}{
		{
			name:           "blocked reason",
			sessionID:      "ses-abc",
			elapsed:        60 * time.Second,
			turnsCompleted: 3,
			reason:         "blocked",
			wantContains: []string{
				"Sortie session completed (agent signaled: blocked).",
				"ses-abc",
				"1m0s",
				"Turns: 3",
			},
		},
		{
			name:           "needs-human-review reason",
			sessionID:      "ses-xyz",
			elapsed:        90 * time.Second,
			turnsCompleted: 5,
			reason:         "needs-human-review",
			wantContains: []string{
				"agent signaled: needs-human-review",
				"ses-xyz",
				"1m30s",
				"Turns: 5",
			},
		},
		{
			name:           "empty session ID replaced with unknown",
			sessionID:      "",
			elapsed:        10 * time.Second,
			turnsCompleted: 1,
			reason:         "blocked",
			wantContains:   []string{"unknown"},
		},
		{
			name:           "sub-second elapsed truncated",
			sessionID:      "ses-short",
			elapsed:        500 * time.Millisecond,
			turnsCompleted: 0,
			reason:         "blocked",
			wantContains:   []string{"0s"},
		},
		{
			name:           "not re-queuing",
			sessionID:      "ses-def",
			elapsed:        30 * time.Second,
			turnsCompleted: 2,
			reason:         "blocked",
			wantAbsent:     []string{"re-queuing"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := buildSoftStopComment(tt.sessionID, tt.elapsed, tt.turnsCompleted, tt.reason)
			for _, want := range tt.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("buildSoftStopComment() missing %q\ngot: %q", want, got)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("buildSoftStopComment() should not contain %q\ngot: %q", absent, got)
				}
			}
		})
	}
}
