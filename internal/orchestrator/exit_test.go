package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/workspace"
)

// --- Test doubles ---

// mockExitStore records calls to the WorkerExitStore interface methods and
// returns configurable errors. It satisfies [WorkerExitStore].
type mockExitStore struct {
	runHistories    []persistence.RunHistory
	metrics         []persistence.AggregateMetrics
	sessionMetadata []persistence.SessionMetadata
	retryEntries    []persistence.RetryEntry
	deletedRetryIDs []string

	// absenceResetAt maps an issue ID to the number of recorded runs at
	// which its absence sequence was last reset, mirroring the run-history
	// watermark the real store keeps.
	absenceResetAt map[string]int
	absenceResetOf []string

	parkedIssues     []persistence.ParkedIssue
	deletedParkedIDs []string
	upsertParkedErr  error
	deleteParkedErr  error

	appendRunHistoryErr       error
	upsertAggregateMetricsErr error
	upsertSessionMetadataErr  error
	saveRetryEntryErr         error
	deleteRetryEntryErr       error
	absenceCountErr           error
	absenceResetErr           error
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

func (m *mockExitStore) DeleteRetryEntry(_ context.Context, issueID string) error {
	m.deletedRetryIDs = append(m.deletedRetryIDs, issueID)
	return m.deleteRetryEntryErr
}

func (m *mockExitStore) QueryConsecutiveHandoffAbsenceCounts(_ context.Context, issueIDs []string) (map[string]int, error) {
	if m.absenceCountErr != nil {
		return nil, m.absenceCountErr
	}
	counts := make(map[string]int, len(issueIDs))
	for _, issueID := range issueIDs {
		// Only a recorded reset ends the sequence. A terminal status of
		// "succeeded" does not, because it is also recorded for outcomes that
		// carry no work-observed verdict.
		for _, run := range m.runHistories[m.absenceResetAt[issueID]:] {
			if run.IssueID != issueID {
				continue
			}
			if run.Status == "failed" && run.Error != nil && strings.HasPrefix(*run.Error, persistence.HandoffAbsenceErrorPrefix) {
				counts[issueID]++
			}
		}
	}
	return counts, nil
}

func (m *mockExitStore) ResetHandoffAbsenceSequence(_ context.Context, issueID string) error {
	m.absenceResetOf = append(m.absenceResetOf, issueID)
	if m.absenceResetErr != nil {
		return m.absenceResetErr
	}
	if m.absenceResetAt == nil {
		m.absenceResetAt = make(map[string]int)
	}
	m.absenceResetAt[issueID] = len(m.runHistories)
	return nil
}

func (m *mockExitStore) UpsertSessionMetadata(_ context.Context, meta persistence.SessionMetadata) error {
	m.sessionMetadata = append(m.sessionMetadata, meta)
	return m.upsertSessionMetadataErr
}

func (m *mockExitStore) UpsertParkedIssue(_ context.Context, entry persistence.ParkedIssue) error {
	m.parkedIssues = append(m.parkedIssues, entry)
	return m.upsertParkedErr
}

func (m *mockExitStore) DeleteParkedIssue(_ context.Context, issueID string) error {
	m.deletedParkedIDs = append(m.deletedParkedIDs, issueID)
	return m.deleteParkedErr
}

// CountWorkerRunsCompletedSince returns a non-nil error, matching the
// conservative default [unsupportedReactionObservationStore] supplies
// elsewhere in this package: this reaction-exit test double is not
// expected to answer an attribution query.
func (m *mockExitStore) CountWorkerRunsCompletedSince(_ context.Context, _ string, _ time.Time) (int, error) {
	return 0, errors.New("worker run count is unsupported by this test double")
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

// TestHandleWorkerExit_RunHistoryTokenColumns verifies the exit path copies
// the running entry's accumulated token counters into the run_history row,
// matching the totals it writes to session_metadata.
func TestHandleWorkerExit_RunHistoryTokenColumns(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "ISSUE-TOK", nil)
	entry := state.Running["ISSUE-TOK"]
	entry.AgentInputTokens = 100
	entry.AgentOutputTokens = 200
	entry.AgentTotalTokens = 300
	entry.CacheReadTokens = 40
	// The event that advanced these totals also marked the entry
	// measured, so a fixture carrying totals must carry the flag.
	entry.UsageMeasured = true

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "ISSUE-TOK",
		Identifier:    "ISSUE-TOK-ident",
		ExitKind:      WorkerExitNormal,
		AgentAdapter:  "mock",
		WorkspacePath: "/tmp/ws",
	}, defaultExitParams(t, store))

	if len(store.runHistories) != 1 {
		t.Fatalf("AppendRunHistory called %d times, want 1", len(store.runHistories))
	}
	run := store.runHistories[0]
	if run.InputTokens != 100 {
		t.Errorf("RunHistory.InputTokens = %d, want 100", run.InputTokens)
	}
	if run.OutputTokens != 200 {
		t.Errorf("RunHistory.OutputTokens = %d, want 200", run.OutputTokens)
	}
	if run.TotalTokens != 300 {
		t.Errorf("RunHistory.TotalTokens = %d, want 300", run.TotalTokens)
	}
	if run.CacheReadTokens != 40 {
		t.Errorf("RunHistory.CacheReadTokens = %d, want 40", run.CacheReadTokens)
	}

	// The same totals reach session_metadata at exit (advisory parity).
	if len(store.sessionMetadata) != 1 {
		t.Fatalf("UpsertSessionMetadata called %d times, want 1", len(store.sessionMetadata))
	}
	meta := store.sessionMetadata[0]
	if meta.TotalTokens != run.TotalTokens {
		t.Errorf("SessionMetadata.TotalTokens = %d, want %d (parity with run_history)", meta.TotalTokens, run.TotalTokens)
	}
}

// TestHandleWorkerExit_UsageReconciliation_NoDoubleCount verifies that
// when every usage event has already been delivered and
// WorkerResult.Usage equals the entry's own current totals, the exit
// path reconciliation applies a zero delta: the persisted run_history
// row equals the pre-exit entry totals and neither entry nor
// state.AgentTotals advances further.
func TestHandleWorkerExit_UsageReconciliation_NoDoubleCount(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "ISSUE-USG1", nil)
	entry := state.Running["ISSUE-USG1"]
	entry.UsageMeasured = true
	entry.AgentInputTokens = 100
	entry.AgentOutputTokens = 50
	entry.AgentTotalTokens = 150
	entry.CacheReadTokens = 10
	entry.LastReportedInputTokens = 100
	entry.LastReportedOutputTokens = 50
	entry.LastReportedTotalTokens = 150
	entry.LastReportedCacheReadTokens = 10
	state.AgentTotals.InputTokens = 100
	state.AgentTotals.OutputTokens = 50
	state.AgentTotals.TotalTokens = 150
	state.AgentTotals.CacheReadTokens = 10

	HandleWorkerExit(state, WorkerResult{
		IssueID:      "ISSUE-USG1",
		Identifier:   "ISSUE-USG1-ident",
		ExitKind:     WorkerExitNormal,
		AgentAdapter: "mock",
		Usage:        domain.TokenUsage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150, CacheReadTokens: 10},
	}, defaultExitParams(t, store))

	if len(store.runHistories) != 1 {
		t.Fatalf("AppendRunHistory called %d times, want 1", len(store.runHistories))
	}
	run := store.runHistories[0]
	if run.InputTokens != 100 || run.OutputTokens != 50 || run.TotalTokens != 150 || run.CacheReadTokens != 10 {
		t.Errorf("RunHistory tokens = (%d, %d, %d, %d), want (100, 50, 150, 10) (no counter advance)",
			run.InputTokens, run.OutputTokens, run.TotalTokens, run.CacheReadTokens)
	}
	if state.AgentTotals.TotalTokens != 150 {
		t.Errorf("AgentTotals.TotalTokens = %d, want 150 (unchanged)", state.AgentTotals.TotalTokens)
	}
}

// TestHandleWorkerExit_UsageReconciliation_DroppedTrailingEvent
// verifies that when the trailing usage event was dropped before it
// reached HandleAgentEvent, WorkerResult.Usage carries the worker's own
// higher figure and HandleWorkerExit reconciles it before persistence:
// the run_history row reports the worker's figures, and
// state.AgentTotals increases by exactly the difference.
func TestHandleWorkerExit_UsageReconciliation_DroppedTrailingEvent(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "ISSUE-USG2", nil)
	entry := state.Running["ISSUE-USG2"]
	entry.UsageMeasured = true
	entry.AgentInputTokens = 100
	entry.AgentOutputTokens = 50
	entry.AgentTotalTokens = 150
	entry.CacheReadTokens = 10
	entry.LastReportedInputTokens = 100
	entry.LastReportedOutputTokens = 50
	entry.LastReportedTotalTokens = 150
	entry.LastReportedCacheReadTokens = 10
	state.AgentTotals.InputTokens = 100
	state.AgentTotals.OutputTokens = 50
	state.AgentTotals.TotalTokens = 150
	state.AgentTotals.CacheReadTokens = 10

	// The worker's own mirror observed more than the entry's last
	// processed event: the trailing token_usage event never reached
	// HandleAgentEvent (a full agentEventCh, or delivery after the
	// entry was already removed).
	workerUsage := domain.TokenUsage{InputTokens: 180, OutputTokens: 90, TotalTokens: 270, CacheReadTokens: 15}

	HandleWorkerExit(state, WorkerResult{
		IssueID:      "ISSUE-USG2",
		Identifier:   "ISSUE-USG2-ident",
		ExitKind:     WorkerExitNormal,
		AgentAdapter: "mock",
		Usage:        workerUsage,
	}, defaultExitParams(t, store))

	if len(store.runHistories) != 1 {
		t.Fatalf("AppendRunHistory called %d times, want 1", len(store.runHistories))
	}
	run := store.runHistories[0]
	if run.InputTokens != 180 || run.OutputTokens != 90 || run.TotalTokens != 270 || run.CacheReadTokens != 15 {
		t.Errorf("RunHistory tokens = (%d, %d, %d, %d), want (180, 90, 270, 15) (worker's figure)",
			run.InputTokens, run.OutputTokens, run.TotalTokens, run.CacheReadTokens)
	}

	// state.AgentTotals started at 150 (total) and must increase by
	// exactly the difference (270 - 150 = 120), not by workerUsage.TotalTokens
	// applied on top of an unreconciled baseline.
	if state.AgentTotals.TotalTokens != 270 {
		t.Errorf("AgentTotals.TotalTokens = %d, want 270 (150 + exactly the 120 difference)", state.AgentTotals.TotalTokens)
	}
	if state.AgentTotals.InputTokens != 180 {
		t.Errorf("AgentTotals.InputTokens = %d, want 180", state.AgentTotals.InputTokens)
	}
}

// TestHandleWorkerExit_TokensMeasured verifies the exit path's fold of
// entry.UsageMeasured and WorkerResult.UsageMeasured into
// RunHistory.TokensMeasured: an unmeasured run, a measured-zero run, a
// run that exits before entering a turn, a measurement recovered only
// from the worker result, and a usage figure reported without any
// measurement assertion.
func TestHandleWorkerExit_TokensMeasured(t *testing.T) {
	t.Parallel()

	t.Run("unmeasured run writes tokens_measured 0 with four zero token columns", func(t *testing.T) {
		t.Parallel()

		store := &mockExitStore{}
		state := exitState(t, "ISSUE-UNM", nil)

		HandleWorkerExit(state, WorkerResult{
			IssueID:      "ISSUE-UNM",
			Identifier:   "ISSUE-UNM-ident",
			ExitKind:     WorkerExitNormal,
			AgentAdapter: "mock",
		}, defaultExitParams(t, store))

		if len(store.runHistories) != 1 {
			t.Fatalf("AppendRunHistory called %d times, want 1", len(store.runHistories))
		}
		run := store.runHistories[0]
		if run.TokensMeasured {
			t.Error("RunHistory.TokensMeasured = true, want false for an unmeasured run")
		}
		if run.InputTokens != 0 || run.OutputTokens != 0 || run.TotalTokens != 0 || run.CacheReadTokens != 0 {
			t.Errorf("RunHistory tokens = (%d, %d, %d, %d), want all zero", run.InputTokens, run.OutputTokens, run.TotalTokens, run.CacheReadTokens)
		}
	})

	t.Run("measured-zero run writes tokens_measured 1 with four zero token columns", func(t *testing.T) {
		t.Parallel()

		store := &mockExitStore{}
		state := exitState(t, "ISSUE-MZ", nil)
		state.Running["ISSUE-MZ"].UsageMeasured = true

		HandleWorkerExit(state, WorkerResult{
			IssueID:      "ISSUE-MZ",
			Identifier:   "ISSUE-MZ-ident",
			ExitKind:     WorkerExitNormal,
			AgentAdapter: "mock",
		}, defaultExitParams(t, store))

		if len(store.runHistories) != 1 {
			t.Fatalf("AppendRunHistory called %d times, want 1", len(store.runHistories))
		}
		run := store.runHistories[0]
		if !run.TokensMeasured {
			t.Error("RunHistory.TokensMeasured = false, want true for a measured-zero run")
		}
		if run.InputTokens != 0 || run.OutputTokens != 0 || run.TotalTokens != 0 || run.CacheReadTokens != 0 {
			t.Errorf("RunHistory tokens = (%d, %d, %d, %d), want all zero", run.InputTokens, run.OutputTokens, run.TotalTokens, run.CacheReadTokens)
		}
	})

	t.Run("run exiting before entering a turn writes tokens_measured 1", func(t *testing.T) {
		t.Parallel()

		store := &mockExitStore{}
		state := exitState(t, "ISSUE-EARLY", nil)

		HandleWorkerExit(state, WorkerResult{
			IssueID:       "ISSUE-EARLY",
			Identifier:    "ISSUE-EARLY-ident",
			ExitKind:      WorkerExitError,
			AgentAdapter:  "mock",
			UsageMeasured: true,
		}, defaultExitParams(t, store))

		if len(store.runHistories) != 1 {
			t.Fatalf("AppendRunHistory called %d times, want 1", len(store.runHistories))
		}
		run := store.runHistories[0]
		if !run.TokensMeasured {
			t.Error("RunHistory.TokensMeasured = false, want true for a run that exited before entering a turn")
		}
	})

	t.Run("a measurement delivered only on WorkerResult.UsageMeasured is recovered", func(t *testing.T) {
		t.Parallel()

		store := &mockExitStore{}
		state := exitState(t, "ISSUE-RECOVER", nil)
		// entry.UsageMeasured stays at its zero value: the orchestrator
		// event loop never observed a qualifying event.

		HandleWorkerExit(state, WorkerResult{
			IssueID:       "ISSUE-RECOVER",
			Identifier:    "ISSUE-RECOVER-ident",
			ExitKind:      WorkerExitNormal,
			AgentAdapter:  "mock",
			UsageMeasured: true,
		}, defaultExitParams(t, store))

		if len(store.runHistories) != 1 {
			t.Fatalf("AppendRunHistory called %d times, want 1", len(store.runHistories))
		}
		run := store.runHistories[0]
		if !run.TokensMeasured {
			t.Error("RunHistory.TokensMeasured = false, want true (recovered from WorkerResult.UsageMeasured alone)")
		}
	})

	t.Run("usage reported without a measurement assertion is zeroed, not recorded", func(t *testing.T) {
		t.Parallel()

		store := &mockExitStore{}
		state := exitState(t, "ISSUE-NOASSERT", nil)

		// An adapter that reports a usage figure on the worker result but
		// never asserts UsageMeasured, and whose events the event loop
		// never saw. The exit path's usage reconciliation populates the
		// entry's token totals from that figure, so without the zeroing
		// the row would carry non-zero tokens against tokens_measured = 0.
		HandleWorkerExit(state, WorkerResult{
			IssueID:      "ISSUE-NOASSERT",
			Identifier:   "ISSUE-NOASSERT-ident",
			ExitKind:     WorkerExitNormal,
			AgentAdapter: "mock",
			Usage: domain.TokenUsage{
				InputTokens:     1000,
				OutputTokens:    200,
				TotalTokens:     1200,
				CacheReadTokens: 100,
			},
		}, defaultExitParams(t, store))

		if len(store.runHistories) != 1 {
			t.Fatalf("AppendRunHistory called %d times, want 1", len(store.runHistories))
		}
		run := store.runHistories[0]
		if run.TokensMeasured {
			t.Error("RunHistory.TokensMeasured = true, want false when no adapter asserted a measurement")
		}
		if run.InputTokens != 0 || run.OutputTokens != 0 || run.TotalTokens != 0 || run.CacheReadTokens != 0 {
			t.Errorf("RunHistory tokens = (%d, %d, %d, %d), want all zero alongside tokens_measured = 0",
				run.InputTokens, run.OutputTokens, run.TotalTokens, run.CacheReadTokens)
		}
	})
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

func TestHandleWorkerExit_RetryableReactionErrorPreservesContext(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "ISSUE-R", intPtr(2))
	contContext := map[string]any{
		"review_comments": map[string]any{"count": 1},
	}
	entry := state.Running["ISSUE-R"]
	entry.Issue = candidateIssue("ISSUE-R", "ISSUE-R-ident", "Ready For Review")
	entry.SessionID = "entry-session"
	entry.ContinuationContext = contContext
	entry.ReactionKind = ReactionKindReview
	params := defaultExitParams(t, store)

	turnTimeoutErr := &domain.AgentError{Kind: domain.ErrTurnTimeout, Message: "timed out"}

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "ISSUE-R",
		Identifier:    "ISSUE-R-ident",
		ExitKind:      WorkerExitError,
		Error:         turnTimeoutErr,
		SessionID:     "worker-session",
		AgentAdapter:  "mock",
		WorkspacePath: "/tmp/ws",
		SSHHost:       "host-r",
	}, params)

	retryEntry, ok := state.RetryAttempts["ISSUE-R"]
	if !ok {
		t.Fatal("retry not scheduled after retryable reaction error")
	}
	if retryEntry.Attempt != 3 {
		t.Errorf("retry Attempt = %d, want 3", retryEntry.Attempt)
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
	if retryEntry.SessionID != "" {
		t.Errorf("RetryEntry.SessionID = %q, want empty", retryEntry.SessionID)
	}
	if retryEntry.LastSSHHost != "host-r" {
		t.Errorf("RetryEntry.LastSSHHost = %q, want %q", retryEntry.LastSSHHost, "host-r")
	}
	if len(store.retryEntries) != 0 {
		t.Errorf("SaveRetryEntry called %d times, want 0 (reaction retry is runtime-only)", len(store.retryEntries))
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

// TestHandleWorkerExit_RunHistoryCompletedAtIsUTC covers R9: the
// production writer formats a UTC time with time.RFC3339, so the
// persisted value ends in "Z" and parses back as RFC3339. Unlike
// TestHandleWorkerExit_RunHistoryFields, NowFunc is left nil so the
// writer's own time.Now().UTC() call is exercised.
func TestHandleWorkerExit_RunHistoryCompletedAtIsUTC(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "HIST-UTC", nil)
	params := defaultExitParams(t, store)
	params.NowFunc = nil

	HandleWorkerExit(state, WorkerResult{
		IssueID:      "HIST-UTC",
		Identifier:   "HIST-UTC-ident",
		ExitKind:     WorkerExitNormal,
		AgentAdapter: "claude-code",
	}, params)

	if len(store.runHistories) != 1 {
		t.Fatalf("AppendRunHistory called %d times, want 1", len(store.runHistories))
	}
	completedAt := store.runHistories[0].CompletedAt
	if !strings.HasSuffix(completedAt, "Z") {
		t.Errorf("RunHistory.CompletedAt = %q, want suffix %q", completedAt, "Z")
	}
	if _, err := time.Parse(time.RFC3339, completedAt); err != nil {
		t.Errorf("time.Parse(RFC3339, %q): %v", completedAt, err)
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
	entry.UsageMeasured = true
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

func handoffEvidenceGitWorkspace(t *testing.T) (string, *workspace.HandoffEvidenceBaseline) {
	t.Helper()
	dir := t.TempDir()
	initGitRepo(t, dir)
	baseline, err := workspace.CaptureHandoffEvidenceBaseline(context.Background(), dir)
	if err != nil {
		t.Fatalf("CaptureHandoffEvidenceBaseline: %v", err)
	}
	return dir, &baseline
}

func handoffEvidenceExitParams(t *testing.T, store *mockExitStore, tracker *mockTrackerAdapter, metrics domain.Metrics) HandleWorkerExitParams {
	t.Helper()
	params := defaultExitParams(t, store)
	params.TrackerAdapter = tracker
	params.HandoffState = "Human Review"
	params.ActiveStates = []string{"In Progress"}
	params.TerminalStates = []string{"Done"}
	params.Metrics = metrics
	return params
}

func TestHandleWorkerExit_HandoffEvidenceObservedAbsenceWithholds(t *testing.T) {
	const issueID = "HE-ABSENT"
	dir, baseline := handoffEvidenceGitWorkspace(t)
	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{}
	spy := &spyMetrics{}
	state := exitStateWithIssue(t, issueID, "In Progress")
	state.Running[issueID].ContinuationContext = map[string]any{"review_comments": "preserved"}
	params := handoffEvidenceExitParams(t, store, tracker, spy)
	params.CommentsConfig.OnCompletion = true
	var logs bytes.Buffer
	params.Logger = debugLogger(t, &logs)

	HandleWorkerExit(state, WorkerResult{
		IssueID:                 issueID,
		Identifier:              issueID + "-ident",
		ExitKind:                WorkerExitNormal,
		TurnsCompleted:          2,
		WorkspacePath:           dir,
		HandoffEvidencePolicy:   config.HandoffEvidenceObserved,
		HandoffEvidenceBaseline: baseline,
		AgentAdapter:            "mock",
	}, params)
	t.Cleanup(func() { CancelRetry(state, issueID) })

	if len(tracker.transitionCalls) != 0 {
		t.Errorf("TransitionIssue called %d times, want 0", len(tracker.transitionCalls))
	}
	if len(tracker.commentCalls) != 0 {
		t.Errorf("completion comment posted for withheld run: %+v", tracker.commentCalls)
	}
	if got := tracker.fetchStatesCalls.Load(); got != 0 {
		t.Errorf("FetchIssueStatesByIDs called %d times, want 0 before a withheld handoff", got)
	}
	if _, ok := state.Claimed[issueID]; !ok {
		t.Error("claim released after withheld handoff, want issue to remain active and claimed")
	}
	retry, ok := state.RetryAttempts[issueID]
	if !ok {
		t.Fatal("retry not scheduled after withheld handoff")
	}
	if retry.scheduledDelayMS != backoffBaseMS {
		t.Errorf("retry delay = %d, want exponential-backoff base %d", retry.scheduledDelayMS, backoffBaseMS)
	}
	if retry.scheduledDelayMS == continuationDelayMS {
		t.Errorf("retry used continuation delay %d", continuationDelayMS)
	}
	if retry.ContinuationContext == nil || retry.ContinuationContext["review_comments"] != "preserved" {
		t.Errorf("retry ContinuationContext = %#v, want preserved", retry.ContinuationContext)
	}
	if !strings.Contains(retry.Error, string(handoffAbsenceObserved)) {
		t.Errorf("retry Error = %q, want verdict %q", retry.Error, handoffAbsenceObserved)
	}
	if len(store.runHistories) != 1 {
		t.Fatalf("AppendRunHistory calls = %d, want 1", len(store.runHistories))
	}
	if got := store.runHistories[0].Status; got != "failed" {
		t.Errorf("RunHistory.Status = %q, want failed", got)
	}
	if store.runHistories[0].Error == nil || !strings.Contains(*store.runHistories[0].Error, string(handoffAbsenceObserved)) {
		t.Errorf("RunHistory.Error = %v, want absence verdict", store.runHistories[0].Error)
	}
	if len(spy.handoffTransitions) != 1 || spy.handoffTransitions[0] != handoffWithheld {
		t.Errorf("handoffTransitions = %v, want [%s]", spy.handoffTransitions, handoffWithheld)
	}
	if len(spy.retries) != 1 || spy.retries[0] != triggerError {
		t.Errorf("retries = %v, want [%s]", spy.retries, triggerError)
	}
	for _, want := range []string{
		`msg="handoff withheld by evidence policy"`,
		`verdict="absence of work observed"`,
		"turns_completed=2",
		"issue_id=" + issueID,
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log output missing %q\ngot: %s", want, logs.String())
		}
	}
}

func TestHandleWorkerExit_HandoffEvidenceWorkspaceChangesPermitHandoff(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, dir string)
	}{
		{
			name: "commit moved",
			mutate: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, "work.txt"), []byte("work\n"), 0o600); err != nil {
					t.Fatalf("write work file: %v", err)
				}
				runGit(t, dir, "add", "work.txt")
				runGit(t, dir, "commit", "-m", "work")
			},
		},
		{
			name: "working tree changed",
			mutate: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, "work.txt"), []byte("work\n"), 0o600); err != nil {
					t.Fatalf("write work file: %v", err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, baseline := handoffEvidenceGitWorkspace(t)
			tt.mutate(t, dir)
			const issueID = "HE-WORK"
			store := &mockExitStore{}
			tracker := &mockTrackerAdapter{}
			spy := &spyMetrics{}
			state := exitStateWithIssue(t, issueID, "In Progress")
			params := handoffEvidenceExitParams(t, store, tracker, spy)

			HandleWorkerExit(state, WorkerResult{
				IssueID:                 issueID,
				Identifier:              issueID + "-ident",
				ExitKind:                WorkerExitNormal,
				WorkspacePath:           dir,
				HandoffEvidencePolicy:   config.HandoffEvidenceObserved,
				HandoffEvidenceBaseline: baseline,
				AgentAdapter:            "mock",
			}, params)

			if len(tracker.transitionCalls) != 1 {
				t.Fatalf("TransitionIssue called %d times, want 1", len(tracker.transitionCalls))
			}
			if len(store.runHistories) != 1 || store.runHistories[0].Status != "succeeded" {
				t.Errorf("run histories = %+v, want one succeeded row", store.runHistories)
			}
			if len(spy.handoffTransitions) != 1 || spy.handoffTransitions[0] != handoffSuccess {
				t.Errorf("handoffTransitions = %v, want [%s]", spy.handoffTransitions, handoffSuccess)
			}
		})
	}
}

func TestHandleWorkerExit_HandoffEvidencePriorSCMMetadataPermitsHandoff(t *testing.T) {
	tests := []struct {
		name  string
		write func(t *testing.T, dir string)
	}{
		{
			name: "pushed commit",
			write: func(t *testing.T, dir string) {
				writeSCMMetadata(t, dir, "feature/issue", "pushed-sha")
			},
		},
		{
			name: "pull request",
			write: func(t *testing.T, dir string) {
				writePRSCMMetadata(t, dir, 42, "acme", "repo", "feature/issue", "")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const issueID = "HE-SCM"
			dir := t.TempDir()
			tt.write(t, dir)
			store := &mockExitStore{}
			tracker := &mockTrackerAdapter{}
			state := exitStateWithIssue(t, issueID, "In Progress")
			params := handoffEvidenceExitParams(t, store, tracker, &spyMetrics{})

			HandleWorkerExit(state, WorkerResult{
				IssueID:               issueID,
				Identifier:            issueID + "-ident",
				ExitKind:              WorkerExitNormal,
				WorkspacePath:         dir,
				HandoffEvidencePolicy: config.HandoffEvidenceObserved,
				AgentAdapter:          "mock",
			}, params)

			if len(tracker.transitionCalls) != 1 {
				t.Fatalf("TransitionIssue called %d times, want 1", len(tracker.transitionCalls))
			}
			if store.runHistories[0].Status != "succeeded" {
				t.Errorf("RunHistory.Status = %q, want succeeded", store.runHistories[0].Status)
			}
		})
	}
}

func TestHandleWorkerExit_HandoffEvidenceNonGitPolicies(t *testing.T) {
	dir := t.TempDir()
	_, baselineErr := workspace.CaptureHandoffEvidenceBaseline(context.Background(), dir)
	if !errors.Is(baselineErr, workspace.ErrNotGitWorkspace) {
		t.Fatalf("baseline error = %v, want ErrNotGitWorkspace", baselineErr)
	}

	t.Run("observed permits undeterminable evidence", func(t *testing.T) {
		const issueID = "HE-NONGIT-OBS"
		store := &mockExitStore{}
		tracker := &mockTrackerAdapter{}
		spy := &spyMetrics{}
		state := exitStateWithIssue(t, issueID, "In Progress")
		params := handoffEvidenceExitParams(t, store, tracker, spy)
		var logs bytes.Buffer
		params.Logger = debugLogger(t, &logs)

		HandleWorkerExit(state, WorkerResult{
			IssueID:                      issueID,
			Identifier:                   issueID + "-ident",
			ExitKind:                     WorkerExitNormal,
			TurnsCompleted:               1,
			WorkspacePath:                dir,
			HandoffEvidencePolicy:        config.HandoffEvidenceObserved,
			HandoffEvidenceBaselineError: baselineErr,
			AgentAdapter:                 "mock",
		}, params)

		if len(tracker.transitionCalls) != 1 {
			t.Fatalf("TransitionIssue called %d times, want 1", len(tracker.transitionCalls))
		}
		if store.runHistories[0].Status != "succeeded" {
			t.Errorf("RunHistory.Status = %q, want succeeded", store.runHistories[0].Status)
		}
		if strings.Contains(logs.String(), "handoff withheld by evidence policy") {
			t.Errorf("observed policy withheld undeterminable evidence\nlogs: %s", logs.String())
		}
		if !strings.Contains(logs.String(), "handoff evidence not determinable") {
			t.Errorf("missing undeterminable info record\nlogs: %s", logs.String())
		}
	})

	t.Run("strict withholds undeterminable evidence", func(t *testing.T) {
		const issueID = "HE-NONGIT-STRICT"
		store := &mockExitStore{}
		tracker := &mockTrackerAdapter{}
		spy := &spyMetrics{}
		state := exitStateWithIssue(t, issueID, "In Progress")
		params := handoffEvidenceExitParams(t, store, tracker, spy)
		var logs bytes.Buffer
		params.Logger = debugLogger(t, &logs)

		HandleWorkerExit(state, WorkerResult{
			IssueID:                      issueID,
			Identifier:                   issueID + "-ident",
			ExitKind:                     WorkerExitNormal,
			TurnsCompleted:               1,
			WorkspacePath:                dir,
			HandoffEvidencePolicy:        config.HandoffEvidenceStrict,
			HandoffEvidenceBaselineError: baselineErr,
			AgentAdapter:                 "mock",
		}, params)
		t.Cleanup(func() { CancelRetry(state, issueID) })

		if len(tracker.transitionCalls) != 0 {
			t.Errorf("TransitionIssue called %d times, want 0", len(tracker.transitionCalls))
		}
		if store.runHistories[0].Status != "failed" || store.runHistories[0].Error == nil ||
			!strings.Contains(*store.runHistories[0].Error, string(handoffEvidenceUndetermined)) {
			t.Errorf("RunHistory = %+v, want failed with undeterminable verdict", store.runHistories[0])
		}
		if !strings.Contains(logs.String(), "handoff evidence not determinable") ||
			!strings.Contains(logs.String(), "handoff withheld by evidence policy") {
			t.Errorf("strict policy logs missing info or warning\nlogs: %s", logs.String())
		}
		if len(spy.handoffTransitions) != 1 || spy.handoffTransitions[0] != handoffWithheld {
			t.Errorf("handoffTransitions = %v, want [%s]", spy.handoffTransitions, handoffWithheld)
		}
	})
}

func TestHandleWorkerExit_HandoffEvidenceOffPreservesOldBehavior(t *testing.T) {
	const issueID = "HE-OFF"
	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{}
	spy := &spyMetrics{}
	state := exitStateWithIssue(t, issueID, "In Progress")
	params := handoffEvidenceExitParams(t, store, tracker, spy)
	var logs bytes.Buffer
	params.Logger = debugLogger(t, &logs)

	HandleWorkerExit(state, WorkerResult{
		IssueID:                      issueID,
		Identifier:                   issueID + "-ident",
		ExitKind:                     WorkerExitNormal,
		WorkspacePath:                filepath.Join(t.TempDir(), "does-not-exist"),
		HandoffEvidencePolicy:        config.HandoffEvidenceOff,
		HandoffEvidenceBaselineError: errors.New("must not be consulted"),
		AgentAdapter:                 "mock",
	}, params)

	if len(tracker.transitionCalls) != 1 {
		t.Fatalf("TransitionIssue called %d times, want 1", len(tracker.transitionCalls))
	}
	if store.runHistories[0].Status != "succeeded" || store.runHistories[0].Error != nil {
		t.Errorf("RunHistory = %+v, want old succeeded disposition", store.runHistories[0])
	}
	if strings.Contains(logs.String(), "handoff evidence") || strings.Contains(logs.String(), "handoff withheld") {
		t.Errorf("off policy emitted evidence record\nlogs: %s", logs.String())
	}
	if len(spy.handoffTransitions) != 1 || spy.handoffTransitions[0] != handoffSuccess {
		t.Errorf("handoffTransitions = %v, want only [%s]", spy.handoffTransitions, handoffSuccess)
	}
}

func TestHandleWorkerExit_HandoffEvidenceOnlyAppliesToEligibleHandoff(t *testing.T) {
	t.Run("no handoff state keeps the continuation disposition", func(t *testing.T) {
		const issueID = "HE-NO-HANDOFF"
		store := &mockExitStore{}
		tracker := &mockTrackerAdapter{}
		spy := &spyMetrics{}
		state := exitStateWithIssue(t, issueID, "In Progress")
		params := defaultExitParams(t, store)
		params.TrackerAdapter = tracker
		params.ActiveStates = []string{"In Progress"}
		params.Metrics = spy
		var logs bytes.Buffer
		params.Logger = debugLogger(t, &logs)

		HandleWorkerExit(state, WorkerResult{
			IssueID:                      issueID,
			Identifier:                   issueID + "-ident",
			ExitKind:                     WorkerExitNormal,
			HandoffEvidencePolicy:        config.HandoffEvidenceStrict,
			HandoffEvidenceBaselineError: errors.New("must not be consulted"),
			AgentAdapter:                 "mock",
		}, params)
		t.Cleanup(func() { CancelRetry(state, issueID) })

		retry := state.RetryAttempts[issueID]
		if retry == nil || retry.scheduledDelayMS != continuationDelayMS {
			t.Errorf("retry = %+v, want existing continuation disposition", retry)
		}
		if store.runHistories[0].Status != "succeeded" {
			t.Errorf("RunHistory.Status = %q, want succeeded", store.runHistories[0].Status)
		}
		if strings.Contains(logs.String(), "handoff evidence") || len(spy.handoffTransitions) != 0 {
			t.Errorf("ineligible path consulted evidence; metrics=%v logs=%s", spy.handoffTransitions, logs.String())
		}
	})

	t.Run("terminal disposition wins before evidence", func(t *testing.T) {
		const issueID = "HE-TERMINAL"
		store := &mockExitStore{}
		tracker := &mockTrackerAdapter{}
		spy := &spyMetrics{}
		state := exitStateWithIssue(t, issueID, "In Progress")
		params := handoffEvidenceExitParams(t, store, tracker, spy)
		var logs bytes.Buffer
		params.Logger = debugLogger(t, &logs)

		HandleWorkerExit(state, WorkerResult{
			IssueID:                      issueID,
			Identifier:                   issueID + "-ident",
			ExitKind:                     WorkerExitNormal,
			ObservedIssueState:           "Done",
			HandoffEvidencePolicy:        config.HandoffEvidenceStrict,
			HandoffEvidenceBaselineError: errors.New("must not be consulted"),
			AgentAdapter:                 "mock",
		}, params)

		if len(tracker.transitionCalls) != 0 {
			t.Errorf("TransitionIssue called %d times, want 0", len(tracker.transitionCalls))
		}
		if store.runHistories[0].Status != "succeeded" {
			t.Errorf("RunHistory.Status = %q, want succeeded terminal suppression", store.runHistories[0].Status)
		}
		if strings.Contains(logs.String(), "handoff evidence") {
			t.Errorf("terminal path consulted evidence\nlogs: %s", logs.String())
		}
		if len(spy.handoffTransitions) != 1 || spy.handoffTransitions[0] != handoffSkipped {
			t.Errorf("handoffTransitions = %v, want [%s]", spy.handoffTransitions, handoffSkipped)
		}
	})
}

// stallingGitDir returns a directory holding a git shim that never produces
// output, prepended to PATH so the evidence inspection stalls. The shim forks
// its sleep and then exits, so the child outlives a kill of the shim itself
// and keeps the output pipe open, which is the case a cancelled context alone
// does not unblock.
func stallingGitDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nsleep 20\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil { //nolint:gosec // test shim must be executable
		t.Fatalf("writing stalling git shim: %v", err)
	}
	return dir
}

// TestHandleWorkerExit_HandoffEvidenceInspectionBounded verifies that the
// workspace evidence inspection cannot block the caller indefinitely. The exit
// handler runs on the orchestrator's single event loop and the context it
// receives carries no deadline of its own, so a Git command that never returns
// must be cut short by the inspection's own bound. Exceeding the bound is a
// failed inspection, which is undeterminable and permits the handoff under the
// observed policy.
func TestHandleWorkerExit_HandoffEvidenceInspectionBounded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stalling git shim is a POSIX shell script")
	}

	const issueID = "HE-STALL"
	dir, baseline := handoffEvidenceGitWorkspace(t)

	// t.Setenv is incompatible with t.Parallel.
	t.Setenv("PATH", stallingGitDir(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
	originalTimeout := handoffEvidenceTimeout
	handoffEvidenceTimeout = 50 * time.Millisecond
	t.Cleanup(func() { handoffEvidenceTimeout = originalTimeout })

	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{}
	spy := &spyMetrics{}
	state := exitStateWithIssue(t, issueID, "In Progress")
	params := handoffEvidenceExitParams(t, store, tracker, spy)
	var logs bytes.Buffer
	params.Logger = debugLogger(t, &logs)

	start := time.Now()
	HandleWorkerExit(state, WorkerResult{
		IssueID:                 issueID,
		Identifier:              issueID + "-ident",
		ExitKind:                WorkerExitNormal,
		WorkspacePath:           dir,
		HandoffEvidencePolicy:   config.HandoffEvidenceObserved,
		HandoffEvidenceBaseline: baseline,
		AgentAdapter:            "mock",
	}, params)
	elapsed := time.Since(start)

	if elapsed > 15*time.Second {
		t.Errorf("HandleWorkerExit took %s, want the evidence inspection bounded well below the stalled command", elapsed)
	}
	if len(tracker.transitionCalls) != 1 {
		t.Errorf("TransitionIssue called %d times, want 1: an undeterminable verdict permits the handoff under the observed policy", len(tracker.transitionCalls))
	}
	if store.runHistories[0].Status != "succeeded" {
		t.Errorf("RunHistory.Status = %q, want succeeded", store.runHistories[0].Status)
	}
	for _, want := range []string{
		`msg="handoff evidence not determinable"`,
		`verdict="evidence not determinable"`,
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log output missing %q\ngot: %s", want, logs.String())
		}
	}
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

// --- Terminal-observation suppression tests ---

// TestHandleWorkerExit_WorkerObservationTerminalSuppressesHandoff is the
// guard for the R4 arm of R7: handoffPath is true from the dispatch-time
// snapshot, and only terminalSuppressed drives the enqueue gate false.
func TestHandleWorkerExit_WorkerObservationTerminalSuppressesHandoff(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	writeSCMMetadata(t, wsPath, "feature/T-1", "sha1")

	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{}
	state := exitStateWithIssue(t, "T-1", "ai:in-progress")
	params := defaultExitParams(t, store)
	params.TrackerAdapter = tracker
	params.ActiveStates = []string{"ai:ready", "ai:in-progress"}
	params.TerminalStates = []string{"ai:done", "ai:cancelled"}
	params.HandoffState = "ai:in-review"
	params.CIProvider = &ciProviderStubExit{}

	HandleWorkerExit(state, WorkerResult{
		IssueID:            "T-1",
		Identifier:         "T-1-ident",
		ExitKind:           WorkerExitNormal,
		AgentAdapter:       "mock",
		WorkspacePath:      wsPath,
		ObservedIssueState: "ai:cancelled",
	}, params)

	if len(tracker.transitionCalls) != 0 {
		t.Errorf("TransitionIssue called %d times, want 0", len(tracker.transitionCalls))
	}
	if _, ok := state.RetryAttempts["T-1"]; ok {
		t.Error("retry scheduled for terminal worker observation, should not be")
	}
	if _, ok := state.Claimed["T-1"]; ok {
		t.Error("claim preserved for terminal worker observation, should be released")
	}
	if len(state.PendingReactions) != 0 {
		t.Errorf("PendingReactions count = %d, want 0", len(state.PendingReactions))
	}
}

// TestHandleWorkerExit_ReconcileObservationSkipsVerificationRead proves a
// reconcile terminal observation takes precedence over an active worker
// observation and short-circuits the verification read entirely.
func TestHandleWorkerExit_ReconcileObservationSkipsVerificationRead(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	writeSCMMetadata(t, wsPath, "feature/T-2", "sha2")

	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{}
	state := exitStateWithIssue(t, "T-2", "ai:in-progress")
	state.Running["T-2"].ObservedTerminalState = "ai:cancelled"
	params := defaultExitParams(t, store)
	params.TrackerAdapter = tracker
	params.ActiveStates = []string{"ai:ready", "ai:in-progress"}
	params.TerminalStates = []string{"ai:done", "ai:cancelled"}
	params.HandoffState = "ai:in-review"
	params.CIProvider = &ciProviderStubExit{}

	HandleWorkerExit(state, WorkerResult{
		IssueID:            "T-2",
		Identifier:         "T-2-ident",
		ExitKind:           WorkerExitNormal,
		AgentAdapter:       "mock",
		WorkspacePath:      wsPath,
		ObservedIssueState: "ai:in-progress",
	}, params)

	if len(tracker.transitionCalls) != 0 {
		t.Errorf("TransitionIssue called %d times, want 0", len(tracker.transitionCalls))
	}
	if got := tracker.fetchStatesCalls.Load(); got != 0 {
		t.Errorf("FetchIssueStatesByIDs called %d times, want 0", got)
	}
}

// TestHandleWorkerExit_VerificationReadReportsTerminalSuppressesHandoff is
// the guard for the R6 arm of R7: the dispatch-time snapshot and the
// worker observation are both active, and only the verification read
// immediately before the write reports the terminal state.
func TestHandleWorkerExit_VerificationReadReportsTerminalSuppressesHandoff(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	writeSCMMetadata(t, wsPath, "feature/T-3", "sha3")

	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{
		fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
			result := make(map[string]string, len(ids))
			for _, id := range ids {
				result[id] = "ai:cancelled"
			}
			return result, nil
		},
	}
	state := exitStateWithIssue(t, "T-3", "ai:in-progress")
	params := defaultExitParams(t, store)
	params.TrackerAdapter = tracker
	params.ActiveStates = []string{"ai:ready", "ai:in-progress"}
	params.TerminalStates = []string{"ai:done", "ai:cancelled"}
	params.HandoffState = "ai:in-review"
	params.CIProvider = &ciProviderStubExit{}

	HandleWorkerExit(state, WorkerResult{
		IssueID:            "T-3",
		Identifier:         "T-3-ident",
		ExitKind:           WorkerExitNormal,
		AgentAdapter:       "mock",
		WorkspacePath:      wsPath,
		ObservedIssueState: "ai:in-progress",
	}, params)

	if got := tracker.fetchStatesCalls.Load(); got != 1 {
		t.Errorf("FetchIssueStatesByIDs called %d times, want 1", got)
	}
	if len(tracker.transitionCalls) != 0 {
		t.Errorf("TransitionIssue called %d times, want 0", len(tracker.transitionCalls))
	}
	if _, ok := state.Claimed["T-3"]; ok {
		t.Error("claim preserved after verified-terminal suppression, should be released")
	}
	if len(state.PendingReactions) != 0 {
		t.Errorf("PendingReactions count = %d, want 0", len(state.PendingReactions))
	}
}

// TestHandleWorkerExit_VerificationReadFailsProceedsWithHandoff proves the
// fail-open rule: a verification-read error does not suppress the handoff.
func TestHandleWorkerExit_VerificationReadFailsProceedsWithHandoff(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{
		fetchStatesFn: func(_ context.Context, _ []string) (map[string]string, error) {
			return nil, &domain.TrackerError{Kind: domain.ErrTrackerTransport, Message: "timeout"}
		},
	}
	state := exitStateWithIssue(t, "T-4", "ai:in-progress")
	params := defaultExitParams(t, store)
	params.TrackerAdapter = tracker
	params.ActiveStates = []string{"ai:ready", "ai:in-progress"}
	params.TerminalStates = []string{"ai:done", "ai:cancelled"}
	params.HandoffState = "ai:in-review"

	HandleWorkerExit(state, WorkerResult{
		IssueID:            "T-4",
		Identifier:         "T-4-ident",
		ExitKind:           WorkerExitNormal,
		AgentAdapter:       "mock",
		ObservedIssueState: "ai:in-progress",
	}, params)

	if len(tracker.transitionCalls) != 1 {
		t.Errorf("TransitionIssue called %d times, want 1 (fail open on verification read error)", len(tracker.transitionCalls))
	}
}

// TestHandleWorkerExit_VerificationReadOmitsIssueProceedsWithHandoff pins
// R6's rule that an issue absent from the verification read's response is
// not a terminal observation.
func TestHandleWorkerExit_VerificationReadOmitsIssueProceedsWithHandoff(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{
		fetchStatesFn: func(_ context.Context, _ []string) (map[string]string, error) {
			return map[string]string{}, nil
		},
	}
	state := exitStateWithIssue(t, "T-5", "ai:in-progress")
	params := defaultExitParams(t, store)
	params.TrackerAdapter = tracker
	params.ActiveStates = []string{"ai:ready", "ai:in-progress"}
	params.TerminalStates = []string{"ai:done", "ai:cancelled"}
	params.HandoffState = "ai:in-review"

	HandleWorkerExit(state, WorkerResult{
		IssueID:            "T-5",
		Identifier:         "T-5-ident",
		ExitKind:           WorkerExitNormal,
		AgentAdapter:       "mock",
		ObservedIssueState: "ai:in-progress",
	}, params)

	if len(tracker.transitionCalls) != 1 {
		t.Errorf("TransitionIssue called %d times, want 1 (absence is not a terminal observation)", len(tracker.transitionCalls))
	}
}

// TestHandleWorkerExit_TerminalObservationNoHandoffStateCancelsRetry pins
// R10's gating of the skipped-handoff metric on a configured handoff
// state, and R4's replacement of today's continuation retry.
func TestHandleWorkerExit_TerminalObservationNoHandoffStateCancelsRetry(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	spy := &spyMetrics{}
	state := exitStateWithIssue(t, "T-6", "ai:in-progress")
	params := defaultExitParams(t, store)
	params.Metrics = spy
	params.ActiveStates = []string{"ai:ready", "ai:in-progress"}
	params.TerminalStates = []string{"ai:done", "ai:cancelled"}
	params.HandoffState = ""

	HandleWorkerExit(state, WorkerResult{
		IssueID:            "T-6",
		Identifier:         "T-6-ident",
		ExitKind:           WorkerExitNormal,
		AgentAdapter:       "mock",
		ObservedIssueState: "ai:cancelled",
	}, params)

	if _, ok := state.RetryAttempts["T-6"]; ok {
		t.Error("continuation retry scheduled for terminal observation with no handoff state, should not be")
	}
	if _, ok := state.Claimed["T-6"]; ok {
		t.Error("claim preserved for terminal observation, should be released")
	}
	if len(spy.handoffTransitions) != 0 {
		t.Errorf("IncHandoffTransitions called %d times, want 0 (no handoff state configured)", len(spy.handoffTransitions))
	}
}

// TestHandleWorkerExit_TerminalOverridesEmptyActiveStatesFallback pins
// R5's rule that the terminal test overrides the empty-active_states
// backward-compatibility fallback.
func TestHandleWorkerExit_TerminalOverridesEmptyActiveStatesFallback(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{}
	state := exitStateWithIssue(t, "T-7", "ai:in-progress")
	params := defaultExitParams(t, store)
	params.TrackerAdapter = tracker
	params.ActiveStates = []string{}
	params.TerminalStates = []string{"ai:cancelled"}
	params.HandoffState = "ai:in-review"

	HandleWorkerExit(state, WorkerResult{
		IssueID:            "T-7",
		Identifier:         "T-7-ident",
		ExitKind:           WorkerExitNormal,
		AgentAdapter:       "mock",
		ObservedIssueState: "ai:cancelled",
	}, params)

	if len(tracker.transitionCalls) != 0 {
		t.Errorf("TransitionIssue called %d times, want 0", len(tracker.transitionCalls))
	}
	if _, ok := state.Claimed["T-7"]; ok {
		t.Error("claim preserved despite terminal test overriding the empty-active_states fallback, should be released")
	}
}

// TestHandleWorkerExit_TerminalTestIsCaseInsensitive pins the
// case-insensitive comparison R5 requires.
func TestHandleWorkerExit_TerminalTestIsCaseInsensitive(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{}
	state := exitStateWithIssue(t, "T-8", "ai:in-progress")
	params := defaultExitParams(t, store)
	params.TrackerAdapter = tracker
	params.ActiveStates = []string{"ai:ready", "ai:in-progress"}
	params.TerminalStates = []string{"ai:cancelled"}
	params.HandoffState = "ai:in-review"

	HandleWorkerExit(state, WorkerResult{
		IssueID:            "T-8",
		Identifier:         "T-8-ident",
		ExitKind:           WorkerExitNormal,
		AgentAdapter:       "mock",
		ObservedIssueState: "AI:Cancelled",
	}, params)

	if len(tracker.transitionCalls) != 0 {
		t.Errorf("TransitionIssue called %d times, want 0 (case-insensitive terminal match)", len(tracker.transitionCalls))
	}
}

// TestHandleWorkerExit_ObservationEqualsHandoffStatePerformsHandoffAndEnqueues
// is the regression test for the agent self-transition flow the handoff
// design endorses: the freshest observation equals the configured handoff
// state, which is not a terminal state, so the exit still performs the
// handoff transition and enqueues its reactions.
func TestHandleWorkerExit_ObservationEqualsHandoffStatePerformsHandoffAndEnqueues(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	writePRSCMMetadata(t, wsPath, 77, "acme", "widgets", "feature/T-9", "sha9")

	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{
		fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
			result := make(map[string]string, len(ids))
			for _, id := range ids {
				result[id] = "ai:in-review"
			}
			return result, nil
		},
	}
	state := exitStateWithIssue(t, "T-9", "ai:in-progress")
	params := defaultExitParams(t, store)
	params.TrackerAdapter = tracker
	params.ActiveStates = []string{"ai:ready", "ai:in-progress"}
	params.TerminalStates = []string{"ai:done", "ai:cancelled"}
	params.HandoffState = "ai:in-review"
	params.CIProvider = &ciProviderStubExit{}
	params.SCMAdapter = &scmAdapterStubExit{}

	HandleWorkerExit(state, WorkerResult{
		IssueID:            "T-9",
		Identifier:         "T-9-ident",
		ExitKind:           WorkerExitNormal,
		AgentAdapter:       "mock",
		WorkspacePath:      wsPath,
		ObservedIssueState: "ai:in-review",
	}, params)

	if len(tracker.transitionCalls) != 1 {
		t.Fatalf("TransitionIssue called %d times, want 1", len(tracker.transitionCalls))
	}
	if got := tracker.transitionCalls[0].TargetState; got != "ai:in-review" {
		t.Errorf("TransitionIssue TargetState = %q, want %q", got, "ai:in-review")
	}
	rkey := ReactionKey("T-9", ReactionKindCI)
	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Error("PendingReactions[T-9:ci] missing; a fresh observation equal to handoff_state must still enqueue reactions")
	}
}

// TestHandleWorkerExit_BothStateListsUnconfiguredHandoffStillFires asserts
// the documented fallback for operators who configure neither
// active_states nor terminal_states: the handoff still fires, and the
// verification read is skipped because no value could classify as
// terminal.
func TestHandleWorkerExit_BothStateListsUnconfiguredHandoffStillFires(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{
		fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
			result := make(map[string]string, len(ids))
			for _, id := range ids {
				result[id] = "ai:cancelled"
			}
			return result, nil
		},
	}
	state := exitStateWithIssue(t, "T-10", "ai:in-progress")
	params := defaultExitParams(t, store)
	params.TrackerAdapter = tracker
	params.ActiveStates = nil
	params.TerminalStates = nil
	params.HandoffState = "ai:in-review"

	HandleWorkerExit(state, WorkerResult{
		IssueID:            "T-10",
		Identifier:         "T-10-ident",
		ExitKind:           WorkerExitNormal,
		AgentAdapter:       "mock",
		ObservedIssueState: "ai:cancelled",
	}, params)

	if got := tracker.fetchStatesCalls.Load(); got != 0 {
		t.Errorf("FetchIssueStatesByIDs called %d times, want 0", got)
	}
	if len(tracker.transitionCalls) != 1 {
		t.Fatalf("TransitionIssue called %d times, want 1", len(tracker.transitionCalls))
	}
	if got := tracker.transitionCalls[0].TargetState; got != "ai:in-review" {
		t.Errorf("TransitionIssue TargetState = %q, want %q", got, "ai:in-review")
	}
}

// TestHandleWorkerExit_HappyPathUnchangedWithVerificationRead confirms the
// new verification read is transparent to the unmodified happy path.
func TestHandleWorkerExit_HappyPathUnchangedWithVerificationRead(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	writePRSCMMetadata(t, wsPath, 77, "acme", "widgets", "feature/T-11", "sha11")

	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{
		fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
			result := make(map[string]string, len(ids))
			for _, id := range ids {
				result[id] = "ai:in-progress"
			}
			return result, nil
		},
	}
	state := exitStateWithIssue(t, "T-11", "ai:in-progress")
	params := defaultExitParams(t, store)
	params.TrackerAdapter = tracker
	params.ActiveStates = []string{"ai:ready", "ai:in-progress"}
	params.TerminalStates = []string{"ai:done", "ai:cancelled"}
	params.HandoffState = "ai:in-review"
	params.CIProvider = &ciProviderStubExit{}
	params.SCMAdapter = &scmAdapterStubExit{}

	HandleWorkerExit(state, WorkerResult{
		IssueID:            "T-11",
		Identifier:         "T-11-ident",
		ExitKind:           WorkerExitNormal,
		AgentAdapter:       "mock",
		WorkspacePath:      wsPath,
		ObservedIssueState: "ai:in-progress",
	}, params)

	if got := tracker.fetchStatesCalls.Load(); got != 1 {
		t.Errorf("FetchIssueStatesByIDs called %d times, want 1", got)
	}
	if len(tracker.transitionCalls) != 1 {
		t.Fatalf("TransitionIssue called %d times, want 1", len(tracker.transitionCalls))
	}
	if got := tracker.transitionCalls[0].TargetState; got != "ai:in-review" {
		t.Errorf("TransitionIssue TargetState = %q, want %q", got, "ai:in-review")
	}
	if _, ok := state.Claimed["T-11"]; ok {
		t.Error("claim preserved after successful handoff transition, should be released")
	}
	rkey := ReactionKey("T-11", ReactionKindCI)
	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Error("PendingReactions[T-11:ci] missing, reaction enqueue should be allowed on the happy path")
	}
}

// TestHandleWorkerExit_TerminalSuppressionIsObservable verifies the
// suppression emits exactly one skipped handoff-transition metric and one
// INFO log record carrying state, state_source, and handoff_state.
func TestHandleWorkerExit_TerminalSuppressionIsObservable(t *testing.T) {
	t.Parallel()

	log, buf := logCapture()
	store := &mockExitStore{}
	spy := &spyMetrics{}
	state := exitStateWithIssue(t, "T-12", "ai:in-progress")
	params := defaultExitParams(t, store)
	params.Logger = log
	params.Metrics = spy
	params.ActiveStates = []string{"ai:ready", "ai:in-progress"}
	params.TerminalStates = []string{"ai:done", "ai:cancelled"}
	params.HandoffState = "ai:in-review"

	HandleWorkerExit(state, WorkerResult{
		IssueID:            "T-12",
		Identifier:         "T-12-ident",
		ExitKind:           WorkerExitNormal,
		AgentAdapter:       "mock",
		ObservedIssueState: "ai:cancelled",
	}, params)

	if len(spy.handoffTransitions) != 1 || spy.handoffTransitions[0] != "skipped" {
		t.Errorf("handoffTransitions = %v, want [skipped]", spy.handoffTransitions)
	}

	output := buf.String()
	if !strings.Contains(output, "handoff suppressed for terminal issue") {
		t.Errorf("log output missing %q message:\n%s", "handoff suppressed for terminal issue", output)
	}
	if !strings.Contains(output, "state=ai:cancelled") {
		t.Errorf("log output missing state attribute:\n%s", output)
	}
	if !strings.Contains(output, "state_source=worker") {
		t.Errorf("log output missing state_source attribute:\n%s", output)
	}
	if !strings.Contains(output, "handoff_state=ai:in-review") {
		t.Errorf("log output missing handoff_state attribute:\n%s", output)
	}
}

// --- Handoff pending-reaction regression tests ---

func TestHandleWorkerExit_HandoffTransitionSucceeds_PopulatesReviewPendingReaction(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	writePRSCMMetadata(t, wsPath, 42, "acme", "myrepo", "feature/HO-R1", "deadbeef")

	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{}
	state := exitStateWithIssue(t, "HO-R1", "In Progress")
	params := defaultExitParams(t, store)
	params.TrackerAdapter = tracker
	params.HandoffState = "In Review"
	params.ActiveStates = []string{"In Progress"}
	params.SCMAdapter = &scmAdapterStubExit{}

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "HO-R1",
		Identifier:    "HO-R1-ident",
		ExitKind:      WorkerExitNormal,
		AgentAdapter:  "mock",
		WorkspacePath: wsPath,
	}, params)

	// TransitionIssue called exactly once.
	if len(tracker.transitionCalls) != 1 {
		t.Fatalf("TransitionIssue called %d times, want 1", len(tracker.transitionCalls))
	}

	// Claim released after successful handoff.
	if _, ok := state.Claimed["HO-R1"]; ok {
		t.Error("state.Claimed[HO-R1] present after successful handoff, want released")
	}

	// No ordinary retry scheduled.
	if _, ok := state.RetryAttempts["HO-R1"]; ok {
		t.Error("retry scheduled after successful handoff, want none")
	}

	// Review pending reaction populated with PR metadata from fixture.
	rkey := ReactionKey("HO-R1", ReactionKindReview)
	pr, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions[HO-R1:review] missing after successful handoff with PR metadata")
	}
	reviewData, ok := pr.KindData.(*ReviewReactionData)
	if !ok {
		t.Fatalf("KindData type = %T, want *ReviewReactionData", pr.KindData)
	}
	if reviewData.PRNumber != 42 {
		t.Errorf("ReviewReactionData.PRNumber = %d, want 42", reviewData.PRNumber)
	}
	if reviewData.Owner != "acme" {
		t.Errorf("ReviewReactionData.Owner = %q, want %q", reviewData.Owner, "acme")
	}
	if reviewData.Repo != "myrepo" {
		t.Errorf("ReviewReactionData.Repo = %q, want %q", reviewData.Repo, "myrepo")
	}
	if reviewData.Branch != "feature/HO-R1" {
		t.Errorf("ReviewReactionData.Branch = %q, want %q", reviewData.Branch, "feature/HO-R1")
	}
	if reviewData.SHA != "deadbeef" {
		t.Errorf("ReviewReactionData.SHA = %q, want %q", reviewData.SHA, "deadbeef")
	}
}

func TestHandleWorkerExit_HandoffTransitionFails_PopulatesReviewPendingReaction(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	writePRSCMMetadata(t, wsPath, 42, "acme", "myrepo", "feature/HO-R2", "deadbeef")

	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{
		transitionIssueFn: func(_ context.Context, _, _ string) error {
			return errors.New("transition failed")
		},
	}
	state := exitStateWithIssue(t, "HO-R2", "In Progress")
	params := defaultExitParams(t, store)
	params.TrackerAdapter = tracker
	params.HandoffState = "In Review"
	params.ActiveStates = []string{"In Progress"}
	params.SCMAdapter = &scmAdapterStubExit{}

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "HO-R2",
		Identifier:    "HO-R2-ident",
		ExitKind:      WorkerExitNormal,
		AgentAdapter:  "mock",
		WorkspacePath: wsPath,
	}, params)

	// Continuation retry scheduled after handoff failure.
	if _, ok := state.RetryAttempts["HO-R2"]; !ok {
		t.Fatal("retry not scheduled after failed handoff transition, want continuation retry")
	}

	// Claim preserved after non-soft-stop handoff failure.
	if _, ok := state.Claimed["HO-R2"]; !ok {
		t.Error("state.Claimed[HO-R2] absent after failed handoff, want preserved")
	}

	// Review pending reaction populated despite handoff failure.
	rkey := ReactionKey("HO-R2", ReactionKindReview)
	pr, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions[HO-R2:review] missing after failed handoff with PR metadata")
	}
	reviewData, ok := pr.KindData.(*ReviewReactionData)
	if !ok {
		t.Fatalf("KindData type = %T, want *ReviewReactionData", pr.KindData)
	}
	if reviewData.PRNumber != 42 {
		t.Errorf("ReviewReactionData.PRNumber = %d, want 42", reviewData.PRNumber)
	}
	if reviewData.Owner != "acme" {
		t.Errorf("ReviewReactionData.Owner = %q, want %q", reviewData.Owner, "acme")
	}
	if reviewData.Repo != "myrepo" {
		t.Errorf("ReviewReactionData.Repo = %q, want %q", reviewData.Repo, "myrepo")
	}
}

func TestHandleWorkerExit_HandoffTransitionSucceeds_PopulatesCIPendingReaction(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	writePRSCMMetadata(t, wsPath, 77, "acme", "widgets", "feature/HO-C1", "cafebabe")

	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{}
	state := exitStateWithIssue(t, "HO-C1", "In Progress")
	params := defaultExitParams(t, store)
	params.TrackerAdapter = tracker
	params.HandoffState = "In Review"
	params.ActiveStates = []string{"In Progress"}
	params.CIProvider = &ciProviderStubExit{}
	params.SCMAdapter = &scmAdapterStubExit{}

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "HO-C1",
		Identifier:    "HO-C1-ident",
		ExitKind:      WorkerExitNormal,
		AgentAdapter:  "mock",
		WorkspacePath: wsPath,
	}, params)

	// Claim released after successful handoff.
	if _, ok := state.Claimed["HO-C1"]; ok {
		t.Error("state.Claimed[HO-C1] present after successful handoff, want released")
	}

	// No ordinary retry scheduled.
	if _, ok := state.RetryAttempts["HO-C1"]; ok {
		t.Error("retry scheduled after successful handoff, want none")
	}

	// CI pending reaction populated with branch and SHA from SCM metadata.
	rkey := ReactionKey("HO-C1", ReactionKindCI)
	ci, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions[HO-C1:ci] missing after successful handoff with branch in SCM metadata")
	}
	ciData, ok := ci.KindData.(*CIReactionData)
	if !ok {
		t.Fatalf("KindData type = %T, want *CIReactionData", ci.KindData)
	}
	if ciData.PRNumber != 77 {
		t.Errorf("CIReactionData.PRNumber = %d, want 77", ciData.PRNumber)
	}
	if ciData.Owner != "acme" {
		t.Errorf("CIReactionData.Owner = %q, want %q", ciData.Owner, "acme")
	}
	if ciData.Repo != "widgets" {
		t.Errorf("CIReactionData.Repo = %q, want %q", ciData.Repo, "widgets")
	}
	if ciData.Branch != "feature/HO-C1" {
		t.Errorf("CIReactionData.Branch = %q, want %q", ciData.Branch, "feature/HO-C1")
	}
	if ciData.SHA != "cafebabe" {
		t.Errorf("CIReactionData.SHA = %q, want %q", ciData.SHA, "cafebabe")
	}
}

func TestHandleWorkerExit_HandoffTransitionFails_PopulatesCIPendingReaction(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	writePRSCMMetadata(t, wsPath, 77, "acme", "widgets", "feature/HO-C2", "cafebabe")

	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{
		transitionIssueFn: func(_ context.Context, _, _ string) error {
			return errors.New("transition failed")
		},
	}
	state := exitStateWithIssue(t, "HO-C2", "In Progress")
	params := defaultExitParams(t, store)
	params.TrackerAdapter = tracker
	params.HandoffState = "In Review"
	params.ActiveStates = []string{"In Progress"}
	params.CIProvider = &ciProviderStubExit{}
	params.SCMAdapter = &scmAdapterStubExit{}

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "HO-C2",
		Identifier:    "HO-C2-ident",
		ExitKind:      WorkerExitNormal,
		AgentAdapter:  "mock",
		WorkspacePath: wsPath,
	}, params)

	// Continuation retry scheduled after handoff failure.
	if _, ok := state.RetryAttempts["HO-C2"]; !ok {
		t.Fatal("retry not scheduled after failed handoff transition, want continuation retry")
	}

	// CI pending reaction populated despite handoff failure.
	rkey := ReactionKey("HO-C2", ReactionKindCI)
	ci, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions[HO-C2:ci] missing after failed handoff with branch in SCM metadata")
	}
	ciData, ok := ci.KindData.(*CIReactionData)
	if !ok {
		t.Fatalf("KindData type = %T, want *CIReactionData", ci.KindData)
	}
	if ciData.PRNumber != 77 {
		t.Errorf("CIReactionData.PRNumber = %d, want 77", ciData.PRNumber)
	}
	if ciData.Owner != "acme" {
		t.Errorf("CIReactionData.Owner = %q, want %q", ciData.Owner, "acme")
	}
	if ciData.Repo != "widgets" {
		t.Errorf("CIReactionData.Repo = %q, want %q", ciData.Repo, "widgets")
	}
	if ciData.Branch != "feature/HO-C2" {
		t.Errorf("CIReactionData.Branch = %q, want %q", ciData.Branch, "feature/HO-C2")
	}
	if ciData.SHA != "cafebabe" {
		t.Errorf("CIReactionData.SHA = %q, want %q", ciData.SHA, "cafebabe")
	}
}

func TestHandleWorkerExit_HandoffReview_DoesNotOverwriteExistingPendingReaction(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	writePRSCMMetadata(t, wsPath, 42, "acme", "myrepo", "feature/HO-DUP1", "deadbeef")

	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{}
	state := exitStateWithIssue(t, "HO-DUP1", "In Progress")

	// Seed an existing review pending entry with a distinct PRNumber.
	existingEntry := &PendingReaction{
		IssueID:   "HO-DUP1",
		Kind:      ReactionKindReview,
		CreatedAt: baseTime,
		KindData: &ReviewReactionData{
			PRNumber: 99,
			Owner:    "seed-owner",
			Repo:     "seed-repo",
			Branch:   "seed-branch",
		},
	}
	rkey := ReactionKey("HO-DUP1", ReactionKindReview)
	state.PendingReactions[rkey] = existingEntry

	params := defaultExitParams(t, store)
	params.TrackerAdapter = tracker
	params.HandoffState = "In Review"
	params.ActiveStates = []string{"In Progress"}
	params.SCMAdapter = &scmAdapterStubExit{}

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "HO-DUP1",
		Identifier:    "HO-DUP1-ident",
		ExitKind:      WorkerExitNormal,
		AgentAdapter:  "mock",
		WorkspacePath: wsPath,
	}, params)

	// Seeded entry must not be replaced.
	got := state.PendingReactions[rkey]
	if got != existingEntry {
		t.Error("PendingReactions[HO-DUP1:review] was replaced; want existing entry preserved")
	}
	reviewData, ok := got.KindData.(*ReviewReactionData)
	if !ok {
		t.Fatalf("KindData type = %T, want *ReviewReactionData", got.KindData)
	}
	if reviewData.PRNumber != 99 {
		t.Errorf("ReviewReactionData.PRNumber = %d, want 99 (seeded value)", reviewData.PRNumber)
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
	for line := range strings.SplitSeq(out, "\n") {
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
	for line := range strings.SplitSeq(out, "\n") {
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

	// The narrowed first case in the inner switch matches only SoftStopReason
	// "blocked", so a configured HandoffState must not trigger a tracker
	// transition when the reason is "blocked".
	t.Run("handoff_skipped_when_blocked", func(t *testing.T) {
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

	t.Run("needs_human_review_triggers_handoff", func(t *testing.T) {
		t.Parallel()

		store := &mockExitStore{}
		tracker := &mockTrackerAdapter{}
		spy := &spyMetrics{}
		state := exitStateWithIssue(t, "SS-8", "In Progress")
		params := defaultExitParams(t, store)
		params.ActiveStates = []string{"In Progress"}
		params.HandoffState = "In Review"
		params.TrackerAdapter = tracker
		params.Metrics = spy

		HandleWorkerExit(state, WorkerResult{
			IssueID:        "SS-8",
			Identifier:     "SS-8-ident",
			ExitKind:       WorkerExitNormal,
			AgentAdapter:   "mock",
			SoftStop:       true,
			SoftStopReason: "needs-human-review",
		}, params)

		if len(tracker.transitionCalls) != 1 {
			t.Fatalf("TransitionIssue called %d times, want 1", len(tracker.transitionCalls))
		}
		if tracker.transitionCalls[0].TargetState != "In Review" {
			t.Errorf("TransitionIssue TargetState = %q, want %q", tracker.transitionCalls[0].TargetState, "In Review")
		}

		if _, ok := state.Claimed["SS-8"]; ok {
			t.Error("claim preserved after needs-human-review handoff, want released")
		}
		if _, ok := state.RetryAttempts["SS-8"]; ok {
			t.Error("retry scheduled after successful handoff, want suppressed")
		}
		if _, ok := state.Completed["SS-8"]; !ok {
			t.Error("issue not added to Completed after needs-human-review handoff")
		}
		if len(store.retryEntries) != 0 {
			t.Errorf("SaveRetryEntry called %d times, want 0", len(store.retryEntries))
		}
		if len(spy.handoffTransitions) != 1 || spy.handoffTransitions[0] != "success" {
			t.Errorf("handoffTransitions = %v, want [success]", spy.handoffTransitions)
		}
	})

	t.Run("needs_human_review_handoff_failure_no_retry", func(t *testing.T) {
		t.Parallel()

		store := &mockExitStore{}
		tracker := &mockTrackerAdapter{
			transitionIssueFn: func(_ context.Context, _, _ string) error {
				return errors.New("jira unavailable")
			},
		}
		spy := &spyMetrics{}
		state := exitStateWithIssue(t, "SS-9", "In Progress")
		params := defaultExitParams(t, store)
		params.ActiveStates = []string{"In Progress"}
		params.HandoffState = "In Review"
		params.TrackerAdapter = tracker
		params.Metrics = spy

		HandleWorkerExit(state, WorkerResult{
			IssueID:        "SS-9",
			Identifier:     "SS-9-ident",
			ExitKind:       WorkerExitNormal,
			AgentAdapter:   "mock",
			SoftStop:       true,
			SoftStopReason: "needs-human-review",
		}, params)

		if len(tracker.transitionCalls) != 1 {
			t.Fatalf("TransitionIssue called %d times, want 1", len(tracker.transitionCalls))
		}
		if _, ok := state.Claimed["SS-9"]; ok {
			t.Error("claim preserved after needs-human-review handoff failure, want released")
		}
		if _, ok := state.RetryAttempts["SS-9"]; ok {
			t.Error("retry scheduled after needs-human-review handoff failure, want suppressed")
		}
		if len(store.retryEntries) != 0 {
			t.Errorf("SaveRetryEntry called %d times, want 0", len(store.retryEntries))
		}
		if _, ok := state.Completed["SS-9"]; !ok {
			t.Error("issue not added to Completed after needs-human-review handoff failure")
		}
		if len(spy.handoffTransitions) != 1 || spy.handoffTransitions[0] != "error" {
			t.Errorf("handoffTransitions = %v, want [error]", spy.handoffTransitions)
		}
		if len(spy.retries) != 0 {
			t.Errorf("retries = %v, want [] (no retry on soft-stop handoff failure)", spy.retries)
		}
	})

	t.Run("blocked_with_handoff_configured_skips_transition", func(t *testing.T) {
		t.Parallel()

		store := &mockExitStore{}
		tracker := &mockTrackerAdapter{}
		state := exitStateWithIssue(t, "SS-10", "In Progress")
		params := defaultExitParams(t, store)
		params.ActiveStates = []string{"In Progress"}
		params.HandoffState = "In Review"
		params.TrackerAdapter = tracker

		HandleWorkerExit(state, WorkerResult{
			IssueID:        "SS-10",
			Identifier:     "SS-10-ident",
			ExitKind:       WorkerExitNormal,
			AgentAdapter:   "mock",
			SoftStop:       true,
			SoftStopReason: "blocked",
		}, params)

		if len(tracker.transitionCalls) != 0 {
			t.Errorf("TransitionIssue called %d times, want 0 (blocked skips handoff)", len(tracker.transitionCalls))
		}
		if _, ok := state.Claimed["SS-10"]; ok {
			t.Error("claim preserved after blocked soft-stop, want released")
		}
		if _, ok := state.RetryAttempts["SS-10"]; ok {
			t.Error("retry scheduled after blocked soft-stop, want suppressed")
		}
		if _, ok := state.Completed["SS-10"]; !ok {
			t.Error("issue not added to Completed after blocked soft-stop")
		}
	})

	t.Run("needs_human_review_no_handoff_configured", func(t *testing.T) {
		t.Parallel()

		store := &mockExitStore{}
		state := exitStateWithIssue(t, "SS-11", "In Progress")
		params := defaultExitParams(t, store)
		params.ActiveStates = []string{"In Progress"}

		HandleWorkerExit(state, WorkerResult{
			IssueID:        "SS-11",
			Identifier:     "SS-11-ident",
			ExitKind:       WorkerExitNormal,
			AgentAdapter:   "mock",
			SoftStop:       true,
			SoftStopReason: "needs-human-review",
		}, params)

		if _, ok := state.Claimed["SS-11"]; ok {
			t.Error("claim preserved after needs-human-review with no handoff, want released")
		}
		if _, ok := state.RetryAttempts["SS-11"]; ok {
			t.Error("retry scheduled after needs-human-review with no handoff, want suppressed")
		}
		if _, ok := state.Completed["SS-11"]; !ok {
			t.Error("issue not added to Completed after needs-human-review with no handoff")
		}
		if len(store.retryEntries) != 0 {
			t.Errorf("SaveRetryEntry called %d times, want 0", len(store.retryEntries))
		}
	})

	t.Run("unrecognized_soft_stop_reason_logs_warning", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		store := &mockExitStore{}
		state := exitStateWithIssue(t, "SS-12", "In Progress")
		params := defaultExitParams(t, store)
		params.ActiveStates = []string{"In Progress"}
		params.Logger = debugLogger(t, &buf)

		HandleWorkerExit(state, WorkerResult{
			IssueID:        "SS-12",
			Identifier:     "SS-12-ident",
			ExitKind:       WorkerExitNormal,
			AgentAdapter:   "mock",
			SoftStop:       true,
			SoftStopReason: "something-unexpected",
		}, params)

		if _, ok := state.Claimed["SS-12"]; ok {
			t.Error("claim preserved after unrecognized soft-stop reason, want released")
		}
		if _, ok := state.RetryAttempts["SS-12"]; ok {
			t.Error("retry scheduled after unrecognized soft-stop reason, want suppressed")
		}
		if _, ok := state.Completed["SS-12"]; !ok {
			t.Error("issue not added to Completed after unrecognized soft-stop reason")
		}
		if !strings.Contains(buf.String(), "unrecognized soft-stop reason") {
			t.Errorf("log output missing WARN for unrecognized soft-stop reason\ngot: %q", buf.String())
		}
		if !strings.Contains(buf.String(), "something-unexpected") {
			t.Errorf("log output missing reason value\ngot: %q", buf.String())
		}
	})

	t.Run("needs_human_review_nil_tracker_adapter_no_retry", func(t *testing.T) {
		t.Parallel()

		store := &mockExitStore{}
		spy := &spyMetrics{}
		state := exitStateWithIssue(t, "SS-13", "In Progress")
		params := defaultExitParams(t, store)
		params.ActiveStates = []string{"In Progress"}
		params.HandoffState = "In Review"
		params.TrackerAdapter = nil
		params.Metrics = spy

		HandleWorkerExit(state, WorkerResult{
			IssueID:        "SS-13",
			Identifier:     "SS-13-ident",
			ExitKind:       WorkerExitNormal,
			AgentAdapter:   "mock",
			SoftStop:       true,
			SoftStopReason: "needs-human-review",
		}, params)

		if _, ok := state.Claimed["SS-13"]; ok {
			t.Error("claim preserved after nil adapter soft-stop, want released")
		}
		if _, ok := state.RetryAttempts["SS-13"]; ok {
			t.Error("retry scheduled after nil adapter soft-stop, want suppressed")
		}
		if _, ok := state.Completed["SS-13"]; !ok {
			t.Error("issue not added to Completed after nil adapter soft-stop")
		}
		if len(store.retryEntries) != 0 {
			t.Errorf("SaveRetryEntry called %d times, want 0", len(store.retryEntries))
		}
		if len(spy.handoffTransitions) != 1 || spy.handoffTransitions[0] != "error" {
			t.Errorf("handoffTransitions = %v, want [error]", spy.handoffTransitions)
		}
		if len(spy.retries) != 0 {
			t.Errorf("retries = %v, want [] (no retry on nil adapter soft-stop)", spy.retries)
		}
	})
}

// TestHandleWorkerExitBlockedDrivingDispatchParksIssue verifies that a
// blocked soft stop from a dispatch that drives issue state records the
// durable park, releases the claim, cancels and deletes the retry, counts
// the park, and starts the parking-label write.
func TestHandleWorkerExitBlockedDrivingDispatchParksIssue(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	tracker := newRecordingHandoffTracker()
	spy := &spyMetrics{}
	state := exitStateWithIssue(t, "BLK-1", "In Progress")
	params := defaultExitParams(t, store)
	params.TrackerAdapter = tracker
	params.HandoffParkingLabel = "needs-human"
	params.Metrics = spy

	HandleWorkerExit(state, WorkerResult{
		IssueID:        "BLK-1",
		Identifier:     "BLK-1-ident",
		ExitKind:       WorkerExitNormal,
		AgentAdapter:   "mock",
		SoftStop:       true,
		SoftStopReason: "blocked",
	}, params)
	state.TrackerOpsWg.Wait()

	entry, ok := state.Parked["BLK-1"]
	if !ok {
		t.Fatal("issue not parked after a blocked soft stop that drives issue state")
	}
	if entry.Reason != parkReasonAgentBlocked {
		t.Errorf("Parked[BLK-1].Reason = %q, want %q", entry.Reason, parkReasonAgentBlocked)
	}
	if len(store.parkedIssues) != 1 || store.parkedIssues[0].IssueID != "BLK-1" {
		t.Errorf("UpsertParkedIssue calls = %+v, want one call for BLK-1", store.parkedIssues)
	}
	if _, ok := state.Claimed["BLK-1"]; ok {
		t.Error("claim remains after parking")
	}
	if _, ok := state.RetryAttempts["BLK-1"]; ok {
		t.Error("retry remains after parking")
	}
	if len(store.deletedRetryIDs) != 1 || store.deletedRetryIDs[0] != "BLK-1" {
		t.Errorf("DeleteRetryEntry calls = %v, want [BLK-1]", store.deletedRetryIDs)
	}
	spy.mu.Lock()
	parks := append([]string(nil), spy.issueParks...)
	spy.mu.Unlock()
	if len(parks) != 1 || parks[0] != parkReasonAgentBlocked {
		t.Errorf("IncIssueParks calls = %v, want one %q call", parks, parkReasonAgentBlocked)
	}
	calls := tracker.labels()
	if len(calls) != 1 || calls[0].issueID != "BLK-1" || calls[0].label != "needs-human" {
		t.Errorf("AddLabel calls = %+v, want one needs-human call for BLK-1", calls)
	}
}

// TestHandleWorkerExitBlockedLabelCommandPostureTakesUnchangedDisposition
// verifies that a blocked soft stop from a dispatch whose posture does not
// drive issue state keeps today's disposition: log, cancel the retry,
// release the claim, no park recorded, no label write started.
func TestHandleWorkerExitBlockedLabelCommandPostureTakesUnchangedDisposition(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	tracker := newRecordingHandoffTracker()
	spy := &spyMetrics{}
	state := exitStateWithIssue(t, "BLK-2", "In Progress")
	state.Running["BLK-2"].ReactionKind = ReactionKindLabelReview
	params := defaultExitParams(t, store)
	params.TrackerAdapter = tracker
	params.HandoffParkingLabel = "needs-human"
	params.Metrics = spy

	HandleWorkerExit(state, WorkerResult{
		IssueID:        "BLK-2",
		Identifier:     "BLK-2-ident",
		ExitKind:       WorkerExitNormal,
		AgentAdapter:   "mock",
		SoftStop:       true,
		SoftStopReason: "blocked",
	}, params)
	state.TrackerOpsWg.Wait()

	if _, ok := state.Parked["BLK-2"]; ok {
		t.Error("issue parked despite a posture that does not drive issue state")
	}
	if len(store.parkedIssues) != 0 {
		t.Errorf("UpsertParkedIssue calls = %+v, want none", store.parkedIssues)
	}
	if _, ok := state.Claimed["BLK-2"]; ok {
		t.Error("claim remains after soft stop")
	}
	if _, ok := state.RetryAttempts["BLK-2"]; ok {
		t.Error("retry remains after soft stop")
	}
	if calls := tracker.labels(); len(calls) != 0 {
		t.Errorf("AddLabel calls = %+v, want none", calls)
	}
	spy.mu.Lock()
	parks := append([]string(nil), spy.issueParks...)
	spy.mu.Unlock()
	if len(parks) != 0 {
		t.Errorf("IncIssueParks calls = %v, want none", parks)
	}
}

// TestHandleWorkerExitNeedsHumanReviewPerformsHandoffWithoutParking is
// issue #811's own regression requirement: a needs-human-review exit still
// performs the handoff transition where configured, records no park, and
// applies no parking label.
func TestHandleWorkerExitNeedsHumanReviewPerformsHandoffWithoutParking(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	tracker := newRecordingHandoffTracker()
	spy := &spyMetrics{}
	state := exitStateWithIssue(t, "NHR-1", "In Progress")
	params := defaultExitParams(t, store)
	params.TrackerAdapter = tracker
	params.HandoffState = "Human Review"
	params.ActiveStates = []string{"In Progress"}
	params.HandoffParkingLabel = "needs-human"
	params.Metrics = spy

	HandleWorkerExit(state, WorkerResult{
		IssueID:        "NHR-1",
		Identifier:     "NHR-1-ident",
		ExitKind:       WorkerExitNormal,
		AgentAdapter:   "mock",
		SoftStop:       true,
		SoftStopReason: "needs-human-review",
	}, params)
	state.TrackerOpsWg.Wait()

	if len(tracker.transitionCalls) != 1 || tracker.transitionCalls[0].TargetState != "Human Review" {
		t.Errorf("TransitionIssue calls = %+v, want one transition to %q", tracker.transitionCalls, "Human Review")
	}
	if _, ok := state.Parked["NHR-1"]; ok {
		t.Error("needs-human-review recorded a park, want none")
	}
	if len(store.parkedIssues) != 0 {
		t.Errorf("UpsertParkedIssue calls = %+v, want none", store.parkedIssues)
	}
	if calls := tracker.labels(); len(calls) != 0 {
		t.Errorf("AddLabel calls = %+v, want none: needs-human-review applies no parking label", calls)
	}
	spy.mu.Lock()
	parks := append([]string(nil), spy.issueParks...)
	spy.mu.Unlock()
	if len(parks) != 0 {
		t.Errorf("IncIssueParks calls = %v, want none", parks)
	}
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

// --- CI provider tests for HandleWorkerExit ---

// writeSCMMetadata writes a minimal .sortie/scm.json to the given workspace
// directory so that workspace.ReadSCMMetadata can read it.
func writeSCMMetadata(t *testing.T, wsPath, branch, sha string) {
	t.Helper()
	dotSortie := filepath.Join(wsPath, ".sortie")
	if err := os.MkdirAll(dotSortie, 0o750); err != nil {
		t.Fatalf("MkdirAll .sortie: %v", err)
	}
	content := fmt.Sprintf(`{"branch":%q,"sha":%q}`, branch, sha)
	if err := os.WriteFile(filepath.Join(dotSortie, "scm.json"), []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile scm.json: %v", err)
	}
}

// writePRSCMMetadata writes a .sortie/scm.json with full PR metadata fields to
// wsPath so that workspace.ReadSCMMetadata returns all review-reaction fields.
func writePRSCMMetadata(t *testing.T, wsPath string, prNumber int, owner, repo, branch, sha string) {
	t.Helper()
	dotSortie := filepath.Join(wsPath, ".sortie")
	if err := os.MkdirAll(dotSortie, 0o750); err != nil {
		t.Fatalf("MkdirAll .sortie: %v", err)
	}
	content := fmt.Sprintf(`{"pr_number":%d,"owner":%q,"repo":%q,"branch":%q,"sha":%q}`,
		prNumber, owner, repo, branch, sha)
	if err := os.WriteFile(filepath.Join(dotSortie, "scm.json"), []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile scm.json: %v", err)
	}
}

// ciProviderStubExit is a minimal CIStatusProvider for exit tests; FetchCIStatus
// must not be called by HandleWorkerExit (that is reconcileCIStatus's job).
type ciProviderStubExit struct{}

func (c *ciProviderStubExit) FetchCIStatus(_ context.Context, _ string) (domain.CIResult, error) {
	panic("FetchCIStatus must not be called by HandleWorkerExit")
}

// scmAdapterStubExit is a minimal SCMAdapter for exit tests; FetchPendingReviews
// must not be called by HandleWorkerExit (that is reconcileReviewComments's job).
type scmAdapterStubExit struct{}

var _ domain.SCMAdapter = (*scmAdapterStubExit)(nil)

func (s *scmAdapterStubExit) FetchPendingReviews(_ context.Context, _ int, _, _ string) ([]domain.ReviewComment, error) {
	panic("FetchPendingReviews must not be called by HandleWorkerExit")
}

func (s *scmAdapterStubExit) FetchBotReviewComments(_ context.Context, _ int, _, _ string, _ []string) ([]domain.ReviewComment, error) {
	panic("FetchBotReviewComments must not be called by HandleWorkerExit")
}

func (s *scmAdapterStubExit) GetReviewDecision(_ context.Context, _ int, _, _ string) (domain.ReviewDecision, error) {
	panic("GetReviewDecision must not be called by HandleWorkerExit")
}

func (s *scmAdapterStubExit) GetCIStatus(_ context.Context, _ int, _, _ string) (string, error) {
	panic("GetCIStatus must not be called by HandleWorkerExit")
}

func (s *scmAdapterStubExit) GetMergeability(_ context.Context, _ int, _, _ string) (domain.PRMergeStatus, error) {
	panic("GetMergeability must not be called by HandleWorkerExit")
}

func (s *scmAdapterStubExit) MergePR(_ context.Context, _ int, _, _ string, _ domain.MergeStrategy, _, _, _ string) (domain.MergeResult, error) {
	panic("MergePR must not be called by HandleWorkerExit")
}

func (s *scmAdapterStubExit) DeleteBranch(_ context.Context, _, _, _ string) error {
	panic("DeleteBranch must not be called by HandleWorkerExit")
}

func (s *scmAdapterStubExit) ListLabelEvents(_ context.Context, _ int, _, _ string) ([]domain.LabelEvent, error) {
	panic("ListLabelEvents must not be called by HandleWorkerExit")
}

func (s *scmAdapterStubExit) RemoveLabel(_ context.Context, _ int, _, _, _ string) error {
	panic("RemoveLabel must not be called by HandleWorkerExit")
}

func TestHandleWorkerExit_CIProvider_PopulatesPendingReaction(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	writePRSCMMetadata(t, wsPath, 77, "acme", "widgets", "feature/ci-test", "abc123")

	store := &mockExitStore{}
	state := exitState(t, "CI-ISS-1", nil)
	params := defaultExitParams(t, store)
	params.CIProvider = &ciProviderStubExit{}
	params.SCMAdapter = &scmAdapterStubExit{}

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "CI-ISS-1",
		Identifier:    "CI-ISS-1-ident",
		ExitKind:      WorkerExitNormal,
		AgentAdapter:  "mock",
		WorkspacePath: wsPath,
	}, params)

	rkey := ReactionKey("CI-ISS-1", ReactionKindCI)
	entry, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions[CI-ISS-1:ci] missing; want entry after normal exit with pull request identity")
	}
	ciData, ok := entry.KindData.(*CIReactionData)
	if !ok {
		t.Fatal("KindData is not *CIReactionData")
	}
	if ciData.PRNumber != 77 {
		t.Errorf("CIReactionData.PRNumber = %d, want 77", ciData.PRNumber)
	}
	if ciData.Owner != "acme" {
		t.Errorf("CIReactionData.Owner = %q, want %q", ciData.Owner, "acme")
	}
	if ciData.Repo != "widgets" {
		t.Errorf("CIReactionData.Repo = %q, want %q", ciData.Repo, "widgets")
	}
	if ciData.Branch != "feature/ci-test" {
		t.Errorf("CIReactionData.Branch = %q, want %q", ciData.Branch, "feature/ci-test")
	}
	if ciData.SHA != "abc123" {
		t.Errorf("CIReactionData.SHA = %q, want %q", ciData.SHA, "abc123")
	}
	if entry.IssueID != "CI-ISS-1" {
		t.Errorf("PendingReaction.IssueID = %q, want %q", entry.IssueID, "CI-ISS-1")
	}
}

// TestHandleWorkerExit_CIProvider_MissingPRIdentity_NoPendingReaction
// verifies that worker-exit CI seeding requires the full pull request
// identity quadruple, one subtest per missing field, mirroring the
// predicate every sibling PR-backed reaction kind already enforces.
func TestHandleWorkerExit_CIProvider_MissingPRIdentity_NoPendingReaction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		prNumber int
		owner    string
		repo     string
		branch   string
	}{
		{"zero PRNumber", 0, "acme", "widgets", "feature/x"},
		{"empty Owner", 77, "", "widgets", "feature/x"},
		{"empty Repo", 77, "acme", "", "feature/x"},
		{"empty Branch", 77, "acme", "widgets", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			wsPath := t.TempDir()
			writePRSCMMetadata(t, wsPath, tt.prNumber, tt.owner, tt.repo, tt.branch, "abc123")

			store := &mockExitStore{}
			state := exitState(t, "CI-ISS-MISSING", nil)
			params := defaultExitParams(t, store)
			params.CIProvider = &ciProviderStubExit{}
			params.SCMAdapter = &scmAdapterStubExit{}

			HandleWorkerExit(state, WorkerResult{
				IssueID:       "CI-ISS-MISSING",
				Identifier:    "CI-ISS-MISSING-ident",
				ExitKind:      WorkerExitNormal,
				AgentAdapter:  "mock",
				WorkspacePath: wsPath,
			}, params)

			if _, ok := state.PendingReactions[ReactionKey("CI-ISS-MISSING", ReactionKindCI)]; ok {
				t.Errorf("PendingReactions populated with %s; want absent (full PR identity required)", tt.name)
			}
		})
	}
}

func TestHandleWorkerExit_CIProvider_NilProvider_NoPendingReaction(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	writeSCMMetadata(t, wsPath, "feature/ci-test", "abc123")

	store := &mockExitStore{}
	state := exitState(t, "CI-ISS-2", nil)
	params := defaultExitParams(t, store)
	params.CIProvider = nil // no CI provider

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "CI-ISS-2",
		Identifier:    "CI-ISS-2-ident",
		ExitKind:      WorkerExitNormal,
		AgentAdapter:  "mock",
		WorkspacePath: wsPath,
	}, params)

	if _, ok := state.PendingReactions[ReactionKey("CI-ISS-2", ReactionKindCI)]; ok {
		t.Error("PendingReactions populated when CIProvider is nil; want absent")
	}
}

func TestHandleWorkerExit_CIProvider_EmptyWorkspace_NoPendingReaction(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "CI-ISS-3", nil)
	params := defaultExitParams(t, store)
	params.CIProvider = &ciProviderStubExit{}
	params.SCMAdapter = &scmAdapterStubExit{}

	// WorkspacePath is empty — worker exited before workspace preparation.
	HandleWorkerExit(state, WorkerResult{
		IssueID:       "CI-ISS-3",
		Identifier:    "CI-ISS-3-ident",
		ExitKind:      WorkerExitNormal,
		AgentAdapter:  "mock",
		WorkspacePath: "",
	}, params)

	if _, ok := state.PendingReactions[ReactionKey("CI-ISS-3", ReactionKindCI)]; ok {
		t.Error("PendingReactions populated for empty workspace; want absent")
	}
}

func TestHandleWorkerExit_CIProvider_NoBranchInSCM_NoPendingReaction(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	// Write SCM metadata without a branch (empty branch field).
	dotSortie := filepath.Join(wsPath, ".sortie")
	if err := os.MkdirAll(dotSortie, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dotSortie, "scm.json"), []byte(`{"branch":"","sha":"abc"}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	store := &mockExitStore{}
	state := exitState(t, "CI-ISS-4", nil)
	params := defaultExitParams(t, store)
	params.CIProvider = &ciProviderStubExit{}
	params.SCMAdapter = &scmAdapterStubExit{}

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "CI-ISS-4",
		Identifier:    "CI-ISS-4-ident",
		ExitKind:      WorkerExitNormal,
		AgentAdapter:  "mock",
		WorkspacePath: wsPath,
	}, params)

	if _, ok := state.PendingReactions[ReactionKey("CI-ISS-4", ReactionKindCI)]; ok {
		t.Error("PendingReactions populated when SCM branch is empty; want absent")
	}
}

func TestHandleWorkerExit_CIProvider_SoftStop_NoPendingReaction(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	writePRSCMMetadata(t, wsPath, 77, "acme", "widgets", "feature/ci-test", "sha999")

	store := &mockExitStore{}
	state := exitState(t, "CI-ISS-5", nil)
	params := defaultExitParams(t, store)
	params.CIProvider = &ciProviderStubExit{}
	params.SCMAdapter = &scmAdapterStubExit{}

	// SoftStop: claim is released before the CI check; PendingReactions must not be populated.
	HandleWorkerExit(state, WorkerResult{
		IssueID:        "CI-ISS-5",
		Identifier:     "CI-ISS-5-ident",
		ExitKind:       WorkerExitNormal,
		SoftStop:       true,
		SoftStopReason: "blocked",
		AgentAdapter:   "mock",
		WorkspacePath:  wsPath,
	}, params)

	if _, ok := state.PendingReactions[ReactionKey("CI-ISS-5", ReactionKindCI)]; ok {
		t.Error("PendingReactions populated after SoftStop; want absent (claim released before CI check)")
	}
}

// --- TrackerOpsWg lifecycle tests ---

// TestHandleWorkerExit_TrackerOpsWgDrains verifies that TrackerOpsWg.Add(1)
// is called before the comment goroutine starts and TrackerOpsWg.Done() is
// called when CommentIssue returns, so Wait() blocks during the call and
// unblocks once it completes.
func TestHandleWorkerExit_TrackerOpsWgDrains(t *testing.T) {
	t.Parallel()

	gate := make(chan struct{})
	tracker := &mockTrackerAdapter{
		commentIssueFn: func(_ context.Context, _, _ string) error {
			<-gate
			return nil
		},
	}

	store := &mockExitStore{}
	state := exitState(t, "WG-EXIT-1", nil)
	params := exitParamsWithComments(t, store, tracker, config.TrackerCommentsConfig{OnCompletion: true})

	HandleWorkerExit(state, WorkerResult{
		IssueID:      "WG-EXIT-1",
		Identifier:   "WG-EXIT-1-ident",
		ExitKind:     WorkerExitNormal,
		SessionID:    "ses-wg-exit",
		AgentAdapter: "mock",
	}, params)

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

func TestHandleWorkerExit_ReviewMetadata_Persisted(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "RM-1", nil)
	params := defaultExitParams(t, store)

	meta := &domain.ReviewMetadata{
		Enabled:         true,
		TotalIterations: 2,
		FinalVerdict:    "pass",
		CapReached:      false,
		Iterations: []domain.ReviewIterationRecord{
			{Iteration: 1, Verdict: "iterate"},
			{Iteration: 2, Verdict: "pass"},
		},
	}

	HandleWorkerExit(state, WorkerResult{
		IssueID:        "RM-1",
		Identifier:     "RM-1-ident",
		ExitKind:       WorkerExitNormal,
		AgentAdapter:   "mock",
		ReviewMetadata: meta,
	}, params)

	if len(store.runHistories) != 1 {
		t.Fatalf("AppendRunHistory called %d times, want 1", len(store.runHistories))
	}

	rh := store.runHistories[0]
	if rh.ReviewMetadata == nil {
		t.Fatal("RunHistory.ReviewMetadata = nil, want non-nil JSON string")
	}

	var got domain.ReviewMetadata
	if err := json.Unmarshal([]byte(*rh.ReviewMetadata), &got); err != nil {
		t.Fatalf("unmarshal RunHistory.ReviewMetadata: %v", err)
	}
	if got.FinalVerdict != "pass" {
		t.Errorf("ReviewMetadata.FinalVerdict = %q, want %q", got.FinalVerdict, "pass")
	}
	if got.TotalIterations != 2 {
		t.Errorf("ReviewMetadata.TotalIterations = %d, want 2", got.TotalIterations)
	}
	if got.CapReached {
		t.Error("ReviewMetadata.CapReached = true, want false")
	}
}

func TestHandleWorkerExit_ReviewMetadata_Nil(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "RM-2", nil)
	params := defaultExitParams(t, store)

	HandleWorkerExit(state, WorkerResult{
		IssueID:        "RM-2",
		Identifier:     "RM-2-ident",
		ExitKind:       WorkerExitNormal,
		AgentAdapter:   "mock",
		ReviewMetadata: nil,
	}, params)

	if len(store.runHistories) != 1 {
		t.Fatalf("AppendRunHistory called %d times, want 1", len(store.runHistories))
	}
	if store.runHistories[0].ReviewMetadata != nil {
		t.Errorf("RunHistory.ReviewMetadata = %q, want nil", *store.runHistories[0].ReviewMetadata)
	}
}

func TestHandleWorkerExit_ContinuationRetry_SessionID(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "SESS-1", nil)
	params := defaultExitParams(t, store)

	HandleWorkerExit(state, WorkerResult{
		IssueID:      "SESS-1",
		Identifier:   "SESS-1-ident",
		ExitKind:     WorkerExitNormal,
		AgentAdapter: "mock",
		SessionID:    "sess-abc",
	}, params)

	entry, ok := state.RetryAttempts["SESS-1"]
	if !ok {
		t.Fatal("RetryAttempts[SESS-1] missing after normal exit, want continuation retry")
	}
	if entry.SessionID != "sess-abc" {
		t.Errorf("RetryAttempts[SESS-1].SessionID = %q, want %q", entry.SessionID, "sess-abc")
	}
	if entry.TimerHandle != nil {
		entry.TimerHandle.Stop()
	}
}

func TestHandleWorkerExit_ContinuationRetry_SessionID_FromEntry(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "SESS-2", nil)
	// Populate SessionID on the running entry (simulates EventSessionStarted
	// having been processed before the worker exited).
	state.Running["SESS-2"].SessionID = "sess-xyz"
	params := defaultExitParams(t, store)

	HandleWorkerExit(state, WorkerResult{
		IssueID:      "SESS-2",
		Identifier:   "SESS-2-ident",
		ExitKind:     WorkerExitNormal,
		AgentAdapter: "mock",
		SessionID:    "", // authoritative source is empty; fall back to entry
	}, params)

	entry, ok := state.RetryAttempts["SESS-2"]
	if !ok {
		t.Fatal("RetryAttempts[SESS-2] missing after normal exit, want continuation retry")
	}
	if entry.SessionID != "sess-xyz" {
		t.Errorf("RetryAttempts[SESS-2].SessionID = %q, want %q", entry.SessionID, "sess-xyz")
	}
	if entry.TimerHandle != nil {
		entry.TimerHandle.Stop()
	}
}

func TestHandleWorkerExit_ErrorRetry_NoSessionID(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "SESS-3", nil)
	params := defaultExitParams(t, store)

	HandleWorkerExit(state, WorkerResult{
		IssueID:      "SESS-3",
		Identifier:   "SESS-3-ident",
		ExitKind:     WorkerExitError,
		AgentAdapter: "mock",
		SessionID:    "sess-abc",
		Error:        fmt.Errorf("something transient"),
	}, params)

	entry, ok := state.RetryAttempts["SESS-3"]
	if !ok {
		t.Fatal("RetryAttempts[SESS-3] missing after error exit, want error retry")
	}
	if entry.SessionID != "" {
		t.Errorf("RetryAttempts[SESS-3].SessionID = %q, want empty (error retries do not resume sessions)", entry.SessionID)
	}
	if entry.TimerHandle != nil {
		entry.TimerHandle.Stop()
	}
}

// --- Auto-merge enqueue tests ---

// TestHandleWorkerExit_AutoMergeEnqueue_PopulatesPendingReaction verifies that
// a complete PR metadata file causes HandleWorkerExit to populate the
// auto-merge PendingReaction on a normal exit (spec Test 7 positive case).
func TestHandleWorkerExit_AutoMergeEnqueue_PopulatesPendingReaction(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	writePRSCMMetadata(t, wsPath, 55, "corp", "api", "feature/AM-1", "c0ffee")

	store := &mockExitStore{}
	state := exitState(t, "AM-1", nil)
	params := defaultExitParams(t, store)
	params.SCMAdapter = &scmAdapterStubExit{}
	params.AutoMergeReactionConfigured = true

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "AM-1",
		Identifier:    "AM-1-ident",
		ExitKind:      WorkerExitNormal,
		AgentAdapter:  "mock",
		WorkspacePath: wsPath,
	}, params)

	rkey := ReactionKey("AM-1", ReactionKindAutoMerge)
	pr, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions[AM-1:merge] missing after normal exit with PR metadata")
	}
	mergeData, ok := pr.KindData.(*AutoMergeReactionData)
	if !ok {
		t.Fatalf("KindData type = %T, want *AutoMergeReactionData", pr.KindData)
	}
	if mergeData.PRNumber != 55 {
		t.Errorf("AutoMergeReactionData.PRNumber = %d, want 55", mergeData.PRNumber)
	}
	if mergeData.Owner != "corp" {
		t.Errorf("AutoMergeReactionData.Owner = %q, want %q", mergeData.Owner, "corp")
	}
	if mergeData.Repo != "api" {
		t.Errorf("AutoMergeReactionData.Repo = %q, want %q", mergeData.Repo, "api")
	}
	if mergeData.Branch != "feature/AM-1" {
		t.Errorf("AutoMergeReactionData.Branch = %q, want %q", mergeData.Branch, "feature/AM-1")
	}
	if mergeData.SHA != "c0ffee" {
		t.Errorf("AutoMergeReactionData.SHA = %q, want %q", mergeData.SHA, "c0ffee")
	}
}

// TestHandleWorkerExit_AutoMergeEnqueueRequiresPRMetadata verifies that the
// auto-merge pending reaction is NOT created when the workspace SCM metadata
// lacks required fields (spec Test 7).
func TestHandleWorkerExit_AutoMergeEnqueueRequiresPRMetadata(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string // raw JSON written to .sortie/scm.json
	}{
		{
			name:    "missing pr_number",
			content: `{"owner":"corp","repo":"api","branch":"feature/x","sha":"abc"}`,
		},
		{
			name:    "missing owner",
			content: `{"pr_number":10,"repo":"api","branch":"feature/x","sha":"abc"}`,
		},
		{
			name:    "missing repo",
			content: `{"pr_number":10,"owner":"corp","branch":"feature/x","sha":"abc"}`,
		},
		{
			name:    "missing branch",
			content: `{"pr_number":10,"owner":"corp","repo":"api","sha":"abc"}`,
		},
		{
			name:    "empty scm file",
			content: `{}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			wsPath := t.TempDir()
			dotSortie := filepath.Join(wsPath, ".sortie")
			if err := os.MkdirAll(dotSortie, 0o750); err != nil {
				t.Fatalf("MkdirAll .sortie: %v", err)
			}
			if err := os.WriteFile(filepath.Join(dotSortie, "scm.json"), []byte(tt.content), 0o600); err != nil {
				t.Fatalf("WriteFile scm.json: %v", err)
			}

			store := &mockExitStore{}
			state := exitState(t, "AM-7", nil)
			params := defaultExitParams(t, store)
			params.SCMAdapter = &scmAdapterStubExit{}
			params.AutoMergeReactionConfigured = true

			HandleWorkerExit(state, WorkerResult{
				IssueID:       "AM-7",
				Identifier:    "AM-7-ident",
				ExitKind:      WorkerExitNormal,
				AgentAdapter:  "mock",
				WorkspacePath: wsPath,
			}, params)

			rkey := ReactionKey("AM-7", ReactionKindAutoMerge)
			if _, ok := state.PendingReactions[rkey]; ok {
				t.Errorf("PendingReactions[AM-7:merge] present despite incomplete SCM metadata (%s)", tt.name)
			}
		})
	}
}

// TestHandleWorkerExit_AutoMergeEnqueueRequiresConfigured verifies that the
// auto-merge pending reaction is NOT created when AutoMergeReactionConfigured
// is false, even with a valid SCM metadata file (spec Test 6).
func TestHandleWorkerExit_AutoMergeEnqueueRequiresConfigured(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	writePRSCMMetadata(t, wsPath, 20, "corp", "api", "feature/AM-6", "deadbeef")

	store := &mockExitStore{}
	state := exitState(t, "AM-6", nil)
	params := defaultExitParams(t, store)
	params.SCMAdapter = &scmAdapterStubExit{}
	params.AutoMergeReactionConfigured = false // provider unset

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "AM-6",
		Identifier:    "AM-6-ident",
		ExitKind:      WorkerExitNormal,
		AgentAdapter:  "mock",
		WorkspacePath: wsPath,
	}, params)

	rkey := ReactionKey("AM-6", ReactionKindAutoMerge)
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions[AM-6:merge] present despite AutoMergeReactionConfigured=false")
	}
}

// TestHandleWorkerExit_AutoMergeEnqueueRequiresSCMAdapter verifies that the
// auto-merge pending reaction is NOT created when no SCM adapter is wired.
func TestHandleWorkerExit_AutoMergeEnqueueRequiresSCMAdapter(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	writePRSCMMetadata(t, wsPath, 20, "corp", "api", "feature/AM-nil", "deadbeef")

	store := &mockExitStore{}
	state := exitState(t, "AM-nil", nil)
	params := defaultExitParams(t, store)
	params.SCMAdapter = nil
	params.AutoMergeReactionConfigured = true

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "AM-nil",
		Identifier:    "AM-nil-ident",
		ExitKind:      WorkerExitNormal,
		AgentAdapter:  "mock",
		WorkspacePath: wsPath,
	}, params)

	rkey := ReactionKey("AM-nil", ReactionKindAutoMerge)
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions[AM-nil:merge] present despite nil SCMAdapter")
	}
}

// TestHandleWorkerExit_AutoMergeEnqueueOnHandoff verifies that the auto-merge
// pending reaction is created after a successful handoff transition, mirroring
// the review-kind behaviour (the merge fires after the worker exits via the
// handoff path).
func TestHandleWorkerExit_AutoMergeEnqueueOnHandoff(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	writePRSCMMetadata(t, wsPath, 99, "corp", "api", "feature/AM-HO", "beefcafe")

	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{}
	state := exitStateWithIssue(t, "AM-HO", "In Progress")
	params := defaultExitParams(t, store)
	params.TrackerAdapter = tracker
	params.HandoffState = "In Review"
	params.ActiveStates = []string{"In Progress"}
	params.SCMAdapter = &scmAdapterStubExit{}
	params.AutoMergeReactionConfigured = true

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "AM-HO",
		Identifier:    "AM-HO-ident",
		ExitKind:      WorkerExitNormal,
		AgentAdapter:  "mock",
		WorkspacePath: wsPath,
	}, params)

	rkey := ReactionKey("AM-HO", ReactionKindAutoMerge)
	pr, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions[AM-HO:merge] missing after handoff with PR metadata")
	}
	mergeData, ok := pr.KindData.(*AutoMergeReactionData)
	if !ok {
		t.Fatalf("KindData type = %T, want *AutoMergeReactionData", pr.KindData)
	}
	if mergeData.PRNumber != 99 {
		t.Errorf("AutoMergeReactionData.PRNumber = %d, want 99", mergeData.PRNumber)
	}
}

// TestHandleWorkerExit_AutoMergeEnqueueDoesNotOverwrite verifies that an
// existing merge-kind pending reaction is not replaced on re-entry.
func TestHandleWorkerExit_AutoMergeEnqueueDoesNotOverwrite(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	writePRSCMMetadata(t, wsPath, 10, "corp", "api", "feature/AM-DUP", "sha1")

	store := &mockExitStore{}
	state := exitState(t, "AM-DUP", nil)
	params := defaultExitParams(t, store)
	params.SCMAdapter = &scmAdapterStubExit{}
	params.AutoMergeReactionConfigured = true

	existingEntry := &PendingReaction{
		IssueID:    "AM-DUP",
		Identifier: "AM-DUP-ident",
		Kind:       ReactionKindAutoMerge,
		KindData: &AutoMergeReactionData{
			PRNumber: 77,
			Owner:    "original",
			Repo:     "original",
			Branch:   "original-branch",
		},
	}
	rkey := ReactionKey("AM-DUP", ReactionKindAutoMerge)
	state.PendingReactions[rkey] = existingEntry

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "AM-DUP",
		Identifier:    "AM-DUP-ident",
		ExitKind:      WorkerExitNormal,
		AgentAdapter:  "mock",
		WorkspacePath: wsPath,
	}, params)

	got := state.PendingReactions[rkey]
	if got != existingEntry {
		t.Error("PendingReactions[AM-DUP:merge] was replaced; want existing entry preserved")
	}
	mergeData, ok := got.KindData.(*AutoMergeReactionData)
	if !ok {
		t.Fatalf("KindData type = %T, want *AutoMergeReactionData", got.KindData)
	}
	if mergeData.PRNumber != 77 {
		t.Errorf("AutoMergeReactionData.PRNumber = %d, want 77 (seeded value)", mergeData.PRNumber)
	}
}

// --- Bot-review enqueue tests ---

// TestHandleWorkerExit_BotReviewEnqueue_PopulatesPendingReaction verifies that
// a complete PR metadata file causes HandleWorkerExit to populate the
// bot-review PendingReaction on a normal exit when bot-review is configured.
func TestHandleWorkerExit_BotReviewEnqueue_PopulatesPendingReaction(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	writePRSCMMetadata(t, wsPath, 55, "corp", "api", "feature/BR-1", "c0ffee")

	store := &mockExitStore{}
	state := exitState(t, "BR-1", nil)
	params := defaultExitParams(t, store)
	params.SCMAdapter = &scmAdapterStubExit{}
	params.BotReviewReactionConfigured = true

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "BR-1",
		Identifier:    "BR-1-ident",
		ExitKind:      WorkerExitNormal,
		AgentAdapter:  "mock",
		WorkspacePath: wsPath,
	}, params)

	rkey := ReactionKey("BR-1", ReactionKindBotReview)
	pr, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions[BR-1:bot-review] missing after normal exit with PR metadata")
	}
	if pr.Kind != ReactionKindBotReview {
		t.Errorf("PendingReaction.Kind = %q, want %q", pr.Kind, ReactionKindBotReview)
	}
	botData, ok := pr.KindData.(*BotReviewReactionData)
	if !ok {
		t.Fatalf("KindData type = %T, want *BotReviewReactionData", pr.KindData)
	}
	if botData.PRNumber != 55 {
		t.Errorf("BotReviewReactionData.PRNumber = %d, want 55", botData.PRNumber)
	}
	if botData.Owner != "corp" {
		t.Errorf("BotReviewReactionData.Owner = %q, want %q", botData.Owner, "corp")
	}
	if botData.Repo != "api" {
		t.Errorf("BotReviewReactionData.Repo = %q, want %q", botData.Repo, "api")
	}
	if botData.Branch != "feature/BR-1" {
		t.Errorf("BotReviewReactionData.Branch = %q, want %q", botData.Branch, "feature/BR-1")
	}
	if botData.SHA != "c0ffee" {
		t.Errorf("BotReviewReactionData.SHA = %q, want %q", botData.SHA, "c0ffee")
	}
}

// TestHandleWorkerExit_BotReviewEnqueueRequiresPRMetadata verifies that the
// bot-review pending reaction is NOT created when the workspace SCM metadata
// lacks any required field.
func TestHandleWorkerExit_BotReviewEnqueueRequiresPRMetadata(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
	}{
		{
			name:    "missing pr_number",
			content: `{"owner":"corp","repo":"api","branch":"feature/x","sha":"abc"}`,
		},
		{
			name:    "missing owner",
			content: `{"pr_number":10,"repo":"api","branch":"feature/x","sha":"abc"}`,
		},
		{
			name:    "missing repo",
			content: `{"pr_number":10,"owner":"corp","branch":"feature/x","sha":"abc"}`,
		},
		{
			name:    "missing branch",
			content: `{"pr_number":10,"owner":"corp","repo":"api","sha":"abc"}`,
		},
		{
			name:    "empty scm file",
			content: `{}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			wsPath := t.TempDir()
			dotSortie := filepath.Join(wsPath, ".sortie")
			if err := os.MkdirAll(dotSortie, 0o750); err != nil {
				t.Fatalf("MkdirAll .sortie: %v", err)
			}
			if err := os.WriteFile(filepath.Join(dotSortie, "scm.json"), []byte(tt.content), 0o600); err != nil {
				t.Fatalf("WriteFile scm.json: %v", err)
			}

			store := &mockExitStore{}
			state := exitState(t, "BR-7", nil)
			params := defaultExitParams(t, store)
			params.SCMAdapter = &scmAdapterStubExit{}
			params.BotReviewReactionConfigured = true

			HandleWorkerExit(state, WorkerResult{
				IssueID:       "BR-7",
				Identifier:    "BR-7-ident",
				ExitKind:      WorkerExitNormal,
				AgentAdapter:  "mock",
				WorkspacePath: wsPath,
			}, params)

			rkey := ReactionKey("BR-7", ReactionKindBotReview)
			if _, ok := state.PendingReactions[rkey]; ok {
				t.Errorf("PendingReactions[BR-7:bot-review] present despite incomplete SCM metadata (%s)", tt.name)
			}
		})
	}
}

// TestHandleWorkerExit_BotReviewEnqueueRequiresConfigured verifies that the
// bot-review pending reaction is NOT created when BotReviewReactionConfigured
// is false, even with a valid SCM metadata file.
func TestHandleWorkerExit_BotReviewEnqueueRequiresConfigured(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	writePRSCMMetadata(t, wsPath, 20, "corp", "api", "feature/BR-6", "deadbeef")

	store := &mockExitStore{}
	state := exitState(t, "BR-6", nil)
	params := defaultExitParams(t, store)
	params.SCMAdapter = &scmAdapterStubExit{}
	params.BotReviewReactionConfigured = false

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "BR-6",
		Identifier:    "BR-6-ident",
		ExitKind:      WorkerExitNormal,
		AgentAdapter:  "mock",
		WorkspacePath: wsPath,
	}, params)

	rkey := ReactionKey("BR-6", ReactionKindBotReview)
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions[BR-6:bot-review] present despite BotReviewReactionConfigured=false")
	}
}

// TestHandleWorkerExit_BotReviewEnqueueRequiresSCMAdapter verifies that the
// bot-review pending reaction is NOT created when no SCM adapter is wired,
// even when bot-review is configured.
func TestHandleWorkerExit_BotReviewEnqueueRequiresSCMAdapter(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	writePRSCMMetadata(t, wsPath, 20, "corp", "api", "feature/BR-nil", "deadbeef")

	store := &mockExitStore{}
	state := exitState(t, "BR-nil", nil)
	params := defaultExitParams(t, store)
	params.SCMAdapter = nil
	params.BotReviewReactionConfigured = true

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "BR-nil",
		Identifier:    "BR-nil-ident",
		ExitKind:      WorkerExitNormal,
		AgentAdapter:  "mock",
		WorkspacePath: wsPath,
	}, params)

	rkey := ReactionKey("BR-nil", ReactionKindBotReview)
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions[BR-nil:bot-review] present despite nil SCMAdapter")
	}
}

// TestHandleWorkerExit_BotReviewEnqueueOnHandoff verifies that the bot-review
// pending reaction is created after a successful handoff transition: the
// reaction fires after the worker exits via the handoff path.
func TestHandleWorkerExit_BotReviewEnqueueOnHandoff(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	writePRSCMMetadata(t, wsPath, 99, "corp", "api", "feature/BR-HO", "beefcafe")

	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{}
	state := exitStateWithIssue(t, "BR-HO", "In Progress")
	params := defaultExitParams(t, store)
	params.TrackerAdapter = tracker
	params.HandoffState = "In Review"
	params.ActiveStates = []string{"In Progress"}
	params.SCMAdapter = &scmAdapterStubExit{}
	params.BotReviewReactionConfigured = true

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "BR-HO",
		Identifier:    "BR-HO-ident",
		ExitKind:      WorkerExitNormal,
		AgentAdapter:  "mock",
		WorkspacePath: wsPath,
	}, params)

	rkey := ReactionKey("BR-HO", ReactionKindBotReview)
	pr, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions[BR-HO:bot-review] missing after handoff with PR metadata")
	}
	botData, ok := pr.KindData.(*BotReviewReactionData)
	if !ok {
		t.Fatalf("KindData type = %T, want *BotReviewReactionData", pr.KindData)
	}
	if botData.PRNumber != 99 {
		t.Errorf("BotReviewReactionData.PRNumber = %d, want 99", botData.PRNumber)
	}
}

// TestHandleWorkerExit_BotReviewEnqueueDoesNotOverwrite verifies the if-absent
// guard: an existing bot-review pending reaction is not replaced on re-entry,
// preserving in-progress debounce state.
func TestHandleWorkerExit_BotReviewEnqueueDoesNotOverwrite(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	writePRSCMMetadata(t, wsPath, 10, "corp", "api", "feature/BR-DUP", "sha1")

	store := &mockExitStore{}
	state := exitState(t, "BR-DUP", nil)
	params := defaultExitParams(t, store)
	params.SCMAdapter = &scmAdapterStubExit{}
	params.BotReviewReactionConfigured = true

	existingEntry := &PendingReaction{
		IssueID:    "BR-DUP",
		Identifier: "BR-DUP-ident",
		Kind:       ReactionKindBotReview,
		KindData: &BotReviewReactionData{
			PRNumber: 77,
			Owner:    "original",
			Repo:     "original",
			Branch:   "original-branch",
		},
	}
	rkey := ReactionKey("BR-DUP", ReactionKindBotReview)
	state.PendingReactions[rkey] = existingEntry

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "BR-DUP",
		Identifier:    "BR-DUP-ident",
		ExitKind:      WorkerExitNormal,
		AgentAdapter:  "mock",
		WorkspacePath: wsPath,
	}, params)

	got := state.PendingReactions[rkey]
	if got != existingEntry {
		t.Error("PendingReactions[BR-DUP:bot-review] was replaced; want existing entry preserved")
	}
	botData, ok := got.KindData.(*BotReviewReactionData)
	if !ok {
		t.Fatalf("KindData type = %T, want *BotReviewReactionData", got.KindData)
	}
	if botData.PRNumber != 77 {
		t.Errorf("BotReviewReactionData.PRNumber = %d, want 77 (seeded value)", botData.PRNumber)
	}
}

// TestHandleWorkerExit_BotReviewEnqueueSkippedWhenClaimReleased verifies that
// the bot-review enqueue is gated on reactionEnqueueAllowed: a soft-stop
// releases the claim before the enqueue check, so no pending reaction is
// created even with complete PR metadata.
func TestHandleWorkerExit_BotReviewEnqueueSkippedWhenClaimReleased(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	writePRSCMMetadata(t, wsPath, 12, "corp", "api", "feature/BR-SOFT", "sha999")

	store := &mockExitStore{}
	state := exitState(t, "BR-SOFT", nil)
	params := defaultExitParams(t, store)
	params.SCMAdapter = &scmAdapterStubExit{}
	params.BotReviewReactionConfigured = true

	HandleWorkerExit(state, WorkerResult{
		IssueID:        "BR-SOFT",
		Identifier:     "BR-SOFT-ident",
		ExitKind:       WorkerExitNormal,
		AgentAdapter:   "mock",
		WorkspacePath:  wsPath,
		SoftStop:       true,
		SoftStopReason: "blocked",
	}, params)

	rkey := ReactionKey("BR-SOFT", ReactionKindBotReview)
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions[BR-SOFT:bot-review] present after soft-stop; want absent (claim released before enqueue)")
	}
}

// --- merge-conflict enqueue tests ---

func TestHandleWorkerExit_MergeConflictEnqueue_PopulatesPendingReaction(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	writePRSCMMetadata(t, wsPath, 55, "corp", "api", "feature/MC-1", "c0ffee")

	store := &mockExitStore{}
	state := exitState(t, "MC-1", nil)
	params := defaultExitParams(t, store)
	params.SCMAdapter = &scmAdapterStubExit{}
	params.MergeConflictReactionConfigured = true

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "MC-1",
		Identifier:    "MC-1-ident",
		ExitKind:      WorkerExitNormal,
		AgentAdapter:  "mock",
		WorkspacePath: wsPath,
	}, params)

	rkey := ReactionKey("MC-1", ReactionKindMergeConflict)
	pr, ok := state.PendingReactions[rkey]
	if !ok {
		t.Fatal("PendingReactions[MC-1:merge-conflict] missing after normal exit with PR metadata")
	}
	if pr.Kind != ReactionKindMergeConflict {
		t.Errorf("PendingReaction.Kind = %q, want %q", pr.Kind, ReactionKindMergeConflict)
	}
	mcData, ok := pr.KindData.(*MergeConflictReactionData)
	if !ok {
		t.Fatalf("KindData type = %T, want *MergeConflictReactionData", pr.KindData)
	}
	if mcData.PRNumber != 55 {
		t.Errorf("MergeConflictReactionData.PRNumber = %d, want 55", mcData.PRNumber)
	}
	if mcData.Owner != "corp" {
		t.Errorf("MergeConflictReactionData.Owner = %q, want %q", mcData.Owner, "corp")
	}
	if mcData.Repo != "api" {
		t.Errorf("MergeConflictReactionData.Repo = %q, want %q", mcData.Repo, "api")
	}
	if mcData.Branch != "feature/MC-1" {
		t.Errorf("MergeConflictReactionData.Branch = %q, want %q", mcData.Branch, "feature/MC-1")
	}
	if mcData.SHA != "c0ffee" {
		t.Errorf("MergeConflictReactionData.SHA = %q, want %q", mcData.SHA, "c0ffee")
	}
}

// TestHandleWorkerExit_MergeConflictEnqueueRequiresPRMetadata verifies that the
// merge-conflict pending reaction is NOT created when the workspace SCM
// metadata lacks any required field.
func TestHandleWorkerExit_MergeConflictEnqueueRequiresPRMetadata(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
	}{
		{
			name:    "missing pr_number",
			content: `{"owner":"corp","repo":"api","branch":"feature/x","sha":"abc"}`,
		},
		{
			name:    "missing owner",
			content: `{"pr_number":10,"repo":"api","branch":"feature/x","sha":"abc"}`,
		},
		{
			name:    "missing repo",
			content: `{"pr_number":10,"owner":"corp","branch":"feature/x","sha":"abc"}`,
		},
		{
			name:    "missing branch",
			content: `{"pr_number":10,"owner":"corp","repo":"api","sha":"abc"}`,
		},
		{
			name:    "empty scm file",
			content: `{}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			wsPath := t.TempDir()
			dotSortie := filepath.Join(wsPath, ".sortie")
			if err := os.MkdirAll(dotSortie, 0o750); err != nil {
				t.Fatalf("MkdirAll .sortie: %v", err)
			}
			if err := os.WriteFile(filepath.Join(dotSortie, "scm.json"), []byte(tt.content), 0o600); err != nil {
				t.Fatalf("WriteFile scm.json: %v", err)
			}

			store := &mockExitStore{}
			state := exitState(t, "MC-7", nil)
			params := defaultExitParams(t, store)
			params.SCMAdapter = &scmAdapterStubExit{}
			params.MergeConflictReactionConfigured = true

			HandleWorkerExit(state, WorkerResult{
				IssueID:       "MC-7",
				Identifier:    "MC-7-ident",
				ExitKind:      WorkerExitNormal,
				AgentAdapter:  "mock",
				WorkspacePath: wsPath,
			}, params)

			rkey := ReactionKey("MC-7", ReactionKindMergeConflict)
			if _, ok := state.PendingReactions[rkey]; ok {
				t.Errorf("PendingReactions[MC-7:merge-conflict] present despite incomplete SCM metadata (%s)", tt.name)
			}
		})
	}
}

// TestHandleWorkerExit_MergeConflictEnqueueRequiresConfigured verifies that the
// merge-conflict pending reaction is NOT created when
// MergeConflictReactionConfigured is false, even with valid PR metadata.
func TestHandleWorkerExit_MergeConflictEnqueueRequiresConfigured(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	writePRSCMMetadata(t, wsPath, 20, "corp", "api", "feature/MC-6", "deadbeef")

	store := &mockExitStore{}
	state := exitState(t, "MC-6", nil)
	params := defaultExitParams(t, store)
	params.SCMAdapter = &scmAdapterStubExit{}
	params.MergeConflictReactionConfigured = false

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "MC-6",
		Identifier:    "MC-6-ident",
		ExitKind:      WorkerExitNormal,
		AgentAdapter:  "mock",
		WorkspacePath: wsPath,
	}, params)

	rkey := ReactionKey("MC-6", ReactionKindMergeConflict)
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions[MC-6:merge-conflict] present despite MergeConflictReactionConfigured=false")
	}
}

// TestHandleWorkerExit_MergeConflictEnqueueRequiresSCMAdapter verifies that the
// merge-conflict pending reaction is NOT created when no SCM adapter is wired,
// even when merge-conflict is configured.
func TestHandleWorkerExit_MergeConflictEnqueueRequiresSCMAdapter(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	writePRSCMMetadata(t, wsPath, 20, "corp", "api", "feature/MC-nil", "deadbeef")

	store := &mockExitStore{}
	state := exitState(t, "MC-nil", nil)
	params := defaultExitParams(t, store)
	params.SCMAdapter = nil
	params.MergeConflictReactionConfigured = true

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "MC-nil",
		Identifier:    "MC-nil-ident",
		ExitKind:      WorkerExitNormal,
		AgentAdapter:  "mock",
		WorkspacePath: wsPath,
	}, params)

	rkey := ReactionKey("MC-nil", ReactionKindMergeConflict)
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions[MC-nil:merge-conflict] present despite nil SCMAdapter")
	}
}

// TestHandleWorkerExit_MergeConflictEnqueueDoesNotOverwrite verifies the
// if-absent guard: an existing merge-conflict pending reaction is not replaced
// on re-entry, preserving in-progress episode state.
func TestHandleWorkerExit_MergeConflictEnqueueDoesNotOverwrite(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	writePRSCMMetadata(t, wsPath, 10, "corp", "api", "feature/MC-DUP", "sha1")

	store := &mockExitStore{}
	state := exitState(t, "MC-DUP", nil)
	params := defaultExitParams(t, store)
	params.SCMAdapter = &scmAdapterStubExit{}
	params.MergeConflictReactionConfigured = true

	existingEntry := &PendingReaction{
		IssueID:    "MC-DUP",
		Identifier: "MC-DUP-ident",
		Kind:       ReactionKindMergeConflict,
		KindData: &MergeConflictReactionData{
			PRNumber: 77,
			Owner:    "original",
			Repo:     "original",
			Branch:   "original-branch",
		},
	}
	rkey := ReactionKey("MC-DUP", ReactionKindMergeConflict)
	state.PendingReactions[rkey] = existingEntry

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "MC-DUP",
		Identifier:    "MC-DUP-ident",
		ExitKind:      WorkerExitNormal,
		AgentAdapter:  "mock",
		WorkspacePath: wsPath,
	}, params)

	got := state.PendingReactions[rkey]
	if got != existingEntry {
		t.Error("PendingReactions[MC-DUP:merge-conflict] was replaced; want existing entry preserved")
	}
	mcData, ok := got.KindData.(*MergeConflictReactionData)
	if !ok {
		t.Fatalf("KindData type = %T, want *MergeConflictReactionData", got.KindData)
	}
	if mcData.PRNumber != 77 {
		t.Errorf("MergeConflictReactionData.PRNumber = %d, want 77 (seeded value preserved)", mcData.PRNumber)
	}
}

// --- label-review enqueue tests ---

// TestHandleWorkerExit_LabelReviewEnqueue verifies that a normal exit with
// PR metadata and LabelReviewReactionConfigured==true seeds one
// label-review pending entry; the slot is not overwritten if already
// present; nothing is seeded when the flag is false, the SCM adapter is
// nil, or PR metadata is incomplete.
//
// The fixture includes a branch even though the label-review enqueue
// clause itself imposes no branch requirement: a workspace's scm.json is
// written only by a normal (non-read-only) session, and such a session
// always operates on a branch, so this reflects the common production
// case rather than a requirement of the enqueue clause or the reader.
func TestHandleWorkerExit_LabelReviewEnqueue(t *testing.T) {
	t.Parallel()

	t.Run("populates pending reaction", func(t *testing.T) {
		t.Parallel()

		wsPath := t.TempDir()
		writePRSCMMetadata(t, wsPath, 55, "corp", "api", "feature/LR-1", "c0ffee")

		store := &mockExitStore{}
		state := exitState(t, "LR-1", nil)
		params := defaultExitParams(t, store)
		params.SCMAdapter = &scmAdapterStubExit{}
		params.LabelReviewReactionConfigured = true

		HandleWorkerExit(state, WorkerResult{
			IssueID:       "LR-1",
			Identifier:    "LR-1-ident",
			ExitKind:      WorkerExitNormal,
			AgentAdapter:  "mock",
			WorkspacePath: wsPath,
		}, params)

		rkey := ReactionKey("LR-1", ReactionKindLabelReview)
		pr, ok := state.PendingReactions[rkey]
		if !ok {
			t.Fatal("PendingReactions[LR-1:label-review] missing after normal exit with PR metadata")
		}
		if pr.Kind != ReactionKindLabelReview {
			t.Errorf("PendingReaction.Kind = %q, want %q", pr.Kind, ReactionKindLabelReview)
		}
		lrData, ok := pr.KindData.(*LabelReviewReactionData)
		if !ok {
			t.Fatalf("KindData type = %T, want *LabelReviewReactionData", pr.KindData)
		}
		if lrData.PRNumber != 55 {
			t.Errorf("LabelReviewReactionData.PRNumber = %d, want 55", lrData.PRNumber)
		}
		if lrData.Owner != "corp" {
			t.Errorf("LabelReviewReactionData.Owner = %q, want %q", lrData.Owner, "corp")
		}
		if lrData.Repo != "api" {
			t.Errorf("LabelReviewReactionData.Repo = %q, want %q", lrData.Repo, "api")
		}
	})

	t.Run("does not overwrite an existing entry", func(t *testing.T) {
		t.Parallel()

		wsPath := t.TempDir()
		writePRSCMMetadata(t, wsPath, 10, "corp", "api", "feature/LR-DUP", "sha1")

		store := &mockExitStore{}
		state := exitState(t, "LR-DUP", nil)
		params := defaultExitParams(t, store)
		params.SCMAdapter = &scmAdapterStubExit{}
		params.LabelReviewReactionConfigured = true

		existingEntry := &PendingReaction{
			IssueID: "LR-DUP",
			Kind:    ReactionKindLabelReview,
			KindData: &LabelReviewReactionData{
				PRNumber: 77,
				Owner:    "original",
				Repo:     "original",
			},
		}
		rkey := ReactionKey("LR-DUP", ReactionKindLabelReview)
		state.PendingReactions[rkey] = existingEntry

		HandleWorkerExit(state, WorkerResult{
			IssueID:       "LR-DUP",
			Identifier:    "LR-DUP-ident",
			ExitKind:      WorkerExitNormal,
			AgentAdapter:  "mock",
			WorkspacePath: wsPath,
		}, params)

		got := state.PendingReactions[rkey]
		if got != existingEntry {
			t.Error("PendingReactions[LR-DUP:label-review] was replaced; want existing entry preserved")
		}
	})

	t.Run("not seeded when not configured", func(t *testing.T) {
		t.Parallel()

		wsPath := t.TempDir()
		writePRSCMMetadata(t, wsPath, 20, "corp", "api", "feature/LR-NC", "deadbeef")

		store := &mockExitStore{}
		state := exitState(t, "LR-NC", nil)
		params := defaultExitParams(t, store)
		params.SCMAdapter = &scmAdapterStubExit{}
		params.LabelReviewReactionConfigured = false

		HandleWorkerExit(state, WorkerResult{
			IssueID:       "LR-NC",
			Identifier:    "LR-NC-ident",
			ExitKind:      WorkerExitNormal,
			AgentAdapter:  "mock",
			WorkspacePath: wsPath,
		}, params)

		rkey := ReactionKey("LR-NC", ReactionKindLabelReview)
		if _, ok := state.PendingReactions[rkey]; ok {
			t.Error("PendingReactions[LR-NC:label-review] present despite LabelReviewReactionConfigured=false")
		}
	})

	t.Run("not seeded when SCM adapter is nil", func(t *testing.T) {
		t.Parallel()

		wsPath := t.TempDir()
		writePRSCMMetadata(t, wsPath, 20, "corp", "api", "feature/LR-nil", "deadbeef")

		store := &mockExitStore{}
		state := exitState(t, "LR-nil", nil)
		params := defaultExitParams(t, store)
		params.SCMAdapter = nil
		params.LabelReviewReactionConfigured = true

		HandleWorkerExit(state, WorkerResult{
			IssueID:       "LR-nil",
			Identifier:    "LR-nil-ident",
			ExitKind:      WorkerExitNormal,
			AgentAdapter:  "mock",
			WorkspacePath: wsPath,
		}, params)

		rkey := ReactionKey("LR-nil", ReactionKindLabelReview)
		if _, ok := state.PendingReactions[rkey]; ok {
			t.Error("PendingReactions[LR-nil:label-review] present despite nil SCMAdapter")
		}
	})

	t.Run("not seeded when PR metadata is incomplete", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name    string
			content string
		}{
			{name: "missing pr_number", content: `{"branch":"feature/x","owner":"corp","repo":"api"}`},
			{name: "missing owner", content: `{"branch":"feature/x","pr_number":10,"repo":"api"}`},
			{name: "missing repo", content: `{"branch":"feature/x","pr_number":10,"owner":"corp"}`},
			{name: "empty scm file", content: `{}`},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				wsPath := t.TempDir()
				dotSortie := filepath.Join(wsPath, ".sortie")
				if err := os.MkdirAll(dotSortie, 0o750); err != nil {
					t.Fatalf("MkdirAll .sortie: %v", err)
				}
				if err := os.WriteFile(filepath.Join(dotSortie, "scm.json"), []byte(tt.content), 0o600); err != nil {
					t.Fatalf("WriteFile scm.json: %v", err)
				}

				store := &mockExitStore{}
				state := exitState(t, "LR-INC", nil)
				params := defaultExitParams(t, store)
				params.SCMAdapter = &scmAdapterStubExit{}
				params.LabelReviewReactionConfigured = true

				HandleWorkerExit(state, WorkerResult{
					IssueID:       "LR-INC",
					Identifier:    "LR-INC-ident",
					ExitKind:      WorkerExitNormal,
					AgentAdapter:  "mock",
					WorkspacePath: wsPath,
				}, params)

				rkey := ReactionKey("LR-INC", ReactionKindLabelReview)
				if _, ok := state.PendingReactions[rkey]; ok {
					t.Errorf("PendingReactions[LR-INC:label-review] present despite incomplete SCM metadata (%s)", tt.name)
				}
			})
		}
	})
}

// --- label-review read-only exit tests ---

// TestHandleWorkerExit_LabelReviewReadOnlyExit_NoHandoff verifies that a
// normal exit whose running entry carries ReactionKind==label-review takes
// neither the handoff path nor the active-issue continuation-retry path,
// even when HandoffState is configured and the linked issue is still
// active: no TransitionIssue call, no continuation retry, and the claim is
// released.
func TestHandleWorkerExit_LabelReviewReadOnlyExit_NoHandoff(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{}
	state := exitStateWithIssue(t, "LR-RO-1", "In Progress")
	state.Running["LR-RO-1"].ReactionKind = ReactionKindLabelReview

	params := defaultExitParams(t, store)
	params.TrackerAdapter = tracker
	params.HandoffState = "Human Review"
	params.ActiveStates = []string{"In Progress"}

	HandleWorkerExit(state, WorkerResult{
		IssueID:      "LR-RO-1",
		Identifier:   "LR-RO-1-ident",
		ExitKind:     WorkerExitNormal,
		AgentAdapter: "mock",
	}, params)

	if len(tracker.transitionCalls) != 0 {
		t.Errorf("TransitionIssue calls = %d, want 0 (a read-only exit performs no handoff)", len(tracker.transitionCalls))
	}
	if _, ok := state.RetryAttempts["LR-RO-1"]; ok {
		t.Error("continuation retry scheduled for a read-only exit; want none")
	}
	if _, claimed := state.Claimed["LR-RO-1"]; claimed {
		t.Error("claim still held after a read-only exit; want released")
	}
}

// TestHandleWorkerExit_LabelReviewReadOnlyExit_ErrorStillRetries is the
// contrast case: an error exit whose running entry carries
// ReactionKind==label-review still schedules a retryable error-driven
// retry, confirming the read-only guard is scoped to normal exits only.
func TestHandleWorkerExit_LabelReviewReadOnlyExit_ErrorStillRetries(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "LR-RO-2", nil)
	state.Running["LR-RO-2"].ReactionKind = ReactionKindLabelReview

	params := defaultExitParams(t, store)

	HandleWorkerExit(state, WorkerResult{
		IssueID:      "LR-RO-2",
		Identifier:   "LR-RO-2-ident",
		ExitKind:     WorkerExitError,
		Error:        errors.New("agent crashed"),
		AgentAdapter: "mock",
	}, params)

	retry, ok := state.RetryAttempts["LR-RO-2"]
	if !ok {
		t.Fatal("retry not scheduled after a retryable error exit; want scheduled even for a label-review entry")
	}
	if retry.ReactionKind != ReactionKindLabelReview {
		t.Errorf("RetryEntry.ReactionKind = %q, want %q (propagated from the exiting entry)", retry.ReactionKind, ReactionKindLabelReview)
	}
}

// --- label-fix seeding tests ---

// TestHandleWorkerExit_LabelFixEnqueue verifies the label-fix seeding
// block: a normal exit with the SCM adapter configured, the label-fix
// feature configured, and branch-bearing PR metadata seeds one entry
// carrying the branch; the entry is skipped when an entry already exists,
// when the feature is not configured, when the SCM adapter is nil, when
// the branch is empty (the fix-specific difference from label-review), or
// when any other PR metadata field is missing.
func TestHandleWorkerExit_LabelFixEnqueue(t *testing.T) {
	t.Parallel()

	t.Run("populates pending reaction", func(t *testing.T) {
		t.Parallel()

		wsPath := t.TempDir()
		writePRSCMMetadata(t, wsPath, 55, "corp", "api", "feature/LF-1", "c0ffee")

		store := &mockExitStore{}
		state := exitState(t, "LF-1", nil)
		state.Running["LF-1"].AgentKind = "mock"
		state.Running["LF-1"].RuleName = "default"
		state.Running["LF-1"].TemplateID = "tmpl-1"
		params := defaultExitParams(t, store)
		params.SCMAdapter = &scmAdapterStubExit{}
		params.LabelFixReactionConfigured = true

		HandleWorkerExit(state, WorkerResult{
			IssueID:       "LF-1",
			Identifier:    "LF-1-ident",
			ExitKind:      WorkerExitNormal,
			AgentAdapter:  "mock",
			WorkspacePath: wsPath,
		}, params)

		rkey := ReactionKey("LF-1", ReactionKindLabelFix)
		pr, ok := state.PendingReactions[rkey]
		if !ok {
			t.Fatal("PendingReactions[LF-1:label-fix] missing after normal exit with branch-bearing PR metadata")
		}
		if pr.Kind != ReactionKindLabelFix {
			t.Errorf("PendingReaction.Kind = %q, want %q", pr.Kind, ReactionKindLabelFix)
		}
		if pr.AgentKind != "mock" || pr.RuleName != "default" || pr.TemplateID != "tmpl-1" {
			t.Errorf("PendingReaction frozen dispatch fields = (%q, %q, %q), want (mock, default, tmpl-1) (frozen from the exiting entry)",
				pr.AgentKind, pr.RuleName, pr.TemplateID)
		}
		lfData, ok := pr.KindData.(*LabelFixReactionData)
		if !ok {
			t.Fatalf("KindData type = %T, want *LabelFixReactionData", pr.KindData)
		}
		if lfData.PRNumber != 55 {
			t.Errorf("LabelFixReactionData.PRNumber = %d, want 55", lfData.PRNumber)
		}
		if lfData.Owner != "corp" {
			t.Errorf("LabelFixReactionData.Owner = %q, want %q", lfData.Owner, "corp")
		}
		if lfData.Repo != "api" {
			t.Errorf("LabelFixReactionData.Repo = %q, want %q", lfData.Repo, "api")
		}
		if lfData.Branch != "feature/LF-1" {
			t.Errorf("LabelFixReactionData.Branch = %q, want %q", lfData.Branch, "feature/LF-1")
		}
	})

	t.Run("does not overwrite an existing entry", func(t *testing.T) {
		t.Parallel()

		wsPath := t.TempDir()
		writePRSCMMetadata(t, wsPath, 10, "corp", "api", "feature/LF-DUP", "sha1")

		store := &mockExitStore{}
		state := exitState(t, "LF-DUP", nil)
		params := defaultExitParams(t, store)
		params.SCMAdapter = &scmAdapterStubExit{}
		params.LabelFixReactionConfigured = true

		existingEntry := &PendingReaction{
			IssueID: "LF-DUP",
			Kind:    ReactionKindLabelFix,
			KindData: &LabelFixReactionData{
				PRNumber: 77,
				Owner:    "original",
				Repo:     "original",
				Branch:   "original-branch",
			},
		}
		rkey := ReactionKey("LF-DUP", ReactionKindLabelFix)
		state.PendingReactions[rkey] = existingEntry

		HandleWorkerExit(state, WorkerResult{
			IssueID:       "LF-DUP",
			Identifier:    "LF-DUP-ident",
			ExitKind:      WorkerExitNormal,
			AgentAdapter:  "mock",
			WorkspacePath: wsPath,
		}, params)

		got := state.PendingReactions[rkey]
		if got != existingEntry {
			t.Error("PendingReactions[LF-DUP:label-fix] was replaced; want existing entry preserved")
		}
	})

	t.Run("not seeded when not configured", func(t *testing.T) {
		t.Parallel()

		wsPath := t.TempDir()
		writePRSCMMetadata(t, wsPath, 20, "corp", "api", "feature/LF-NC", "deadbeef")

		store := &mockExitStore{}
		state := exitState(t, "LF-NC", nil)
		params := defaultExitParams(t, store)
		params.SCMAdapter = &scmAdapterStubExit{}
		params.LabelFixReactionConfigured = false

		HandleWorkerExit(state, WorkerResult{
			IssueID:       "LF-NC",
			Identifier:    "LF-NC-ident",
			ExitKind:      WorkerExitNormal,
			AgentAdapter:  "mock",
			WorkspacePath: wsPath,
		}, params)

		rkey := ReactionKey("LF-NC", ReactionKindLabelFix)
		if _, ok := state.PendingReactions[rkey]; ok {
			t.Error("PendingReactions[LF-NC:label-fix] present despite LabelFixReactionConfigured=false")
		}
	})

	t.Run("not seeded when SCM adapter is nil", func(t *testing.T) {
		t.Parallel()

		wsPath := t.TempDir()
		writePRSCMMetadata(t, wsPath, 20, "corp", "api", "feature/LF-nil", "deadbeef")

		store := &mockExitStore{}
		state := exitState(t, "LF-nil", nil)
		params := defaultExitParams(t, store)
		params.SCMAdapter = nil
		params.LabelFixReactionConfigured = true

		HandleWorkerExit(state, WorkerResult{
			IssueID:       "LF-nil",
			Identifier:    "LF-nil-ident",
			ExitKind:      WorkerExitNormal,
			AgentAdapter:  "mock",
			WorkspacePath: wsPath,
		}, params)

		rkey := ReactionKey("LF-nil", ReactionKindLabelFix)
		if _, ok := state.PendingReactions[rkey]; ok {
			t.Error("PendingReactions[LF-nil:label-fix] present despite nil SCMAdapter")
		}
	})

	t.Run("not seeded when branch is empty", func(t *testing.T) {
		t.Parallel()

		wsPath := t.TempDir()
		dotSortie := filepath.Join(wsPath, ".sortie")
		if err := os.MkdirAll(dotSortie, 0o750); err != nil {
			t.Fatalf("MkdirAll .sortie: %v", err)
		}
		content := `{"pr_number":30,"owner":"corp","repo":"api"}`
		if err := os.WriteFile(filepath.Join(dotSortie, "scm.json"), []byte(content), 0o600); err != nil {
			t.Fatalf("WriteFile scm.json: %v", err)
		}

		store := &mockExitStore{}
		state := exitState(t, "LF-NOBRANCH", nil)
		params := defaultExitParams(t, store)
		params.SCMAdapter = &scmAdapterStubExit{}
		params.LabelFixReactionConfigured = true

		HandleWorkerExit(state, WorkerResult{
			IssueID:       "LF-NOBRANCH",
			Identifier:    "LF-NOBRANCH-ident",
			ExitKind:      WorkerExitNormal,
			AgentAdapter:  "mock",
			WorkspacePath: wsPath,
		}, params)

		rkey := ReactionKey("LF-NOBRANCH", ReactionKindLabelFix)
		if _, ok := state.PendingReactions[rkey]; ok {
			t.Error("PendingReactions[LF-NOBRANCH:label-fix] present despite an empty branch; want the branch-required guard to block seeding (unlike label-review)")
		}
	})

	t.Run("not seeded when PR metadata is incomplete", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name    string
			content string
		}{
			{name: "missing pr_number", content: `{"branch":"feature/x","owner":"corp","repo":"api"}`},
			{name: "missing owner", content: `{"branch":"feature/x","pr_number":10,"repo":"api"}`},
			{name: "missing repo", content: `{"branch":"feature/x","pr_number":10,"owner":"corp"}`},
			{name: "empty scm file", content: `{}`},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				wsPath := t.TempDir()
				dotSortie := filepath.Join(wsPath, ".sortie")
				if err := os.MkdirAll(dotSortie, 0o750); err != nil {
					t.Fatalf("MkdirAll .sortie: %v", err)
				}
				if err := os.WriteFile(filepath.Join(dotSortie, "scm.json"), []byte(tt.content), 0o600); err != nil {
					t.Fatalf("WriteFile scm.json: %v", err)
				}

				store := &mockExitStore{}
				state := exitState(t, "LF-INC", nil)
				params := defaultExitParams(t, store)
				params.SCMAdapter = &scmAdapterStubExit{}
				params.LabelFixReactionConfigured = true

				HandleWorkerExit(state, WorkerResult{
					IssueID:       "LF-INC",
					Identifier:    "LF-INC-ident",
					ExitKind:      WorkerExitNormal,
					AgentAdapter:  "mock",
					WorkspacePath: wsPath,
				}, params)

				rkey := ReactionKey("LF-INC", ReactionKindLabelFix)
				if _, ok := state.PendingReactions[rkey]; ok {
					t.Errorf("PendingReactions[LF-INC:label-fix] present despite incomplete SCM metadata (%s)", tt.name)
				}
			})
		}
	})
}

// --- label-fix exit tests ---

// TestHandleWorkerExit_LabelFixExit_NoHandoff verifies that a normal exit
// whose running entry carries ReactionKind==label-fix takes neither the
// handoff path nor the active-issue continuation-retry path, even when
// HandoffState is configured and the linked issue is still active: no
// TransitionIssue call, no continuation retry, the claim is released, and
// no label-fix entry is re-seeded even though the workspace carries full
// branch-bearing PR metadata (repeatability comes from the reconcile
// re-enqueue, not from a fix session's own exit).
func TestHandleWorkerExit_LabelFixExit_NoHandoff(t *testing.T) {
	t.Parallel()

	wsPath := t.TempDir()
	writePRSCMMetadata(t, wsPath, 5, "corp", "api", "feature/LF-RO-1", "c0ffee")

	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{}
	state := exitStateWithIssue(t, "LF-RO-1", "In Progress")
	state.Running["LF-RO-1"].ReactionKind = ReactionKindLabelFix

	params := defaultExitParams(t, store)
	params.TrackerAdapter = tracker
	params.HandoffState = "Human Review"
	params.ActiveStates = []string{"In Progress"}
	params.SCMAdapter = &scmAdapterStubExit{}
	params.LabelFixReactionConfigured = true

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "LF-RO-1",
		Identifier:    "LF-RO-1-ident",
		ExitKind:      WorkerExitNormal,
		AgentAdapter:  "mock",
		WorkspacePath: wsPath,
	}, params)

	if len(tracker.transitionCalls) != 0 {
		t.Errorf("TransitionIssue calls = %d, want 0 (a fix exit performs no handoff)", len(tracker.transitionCalls))
	}
	if _, ok := state.RetryAttempts["LF-RO-1"]; ok {
		t.Error("continuation retry scheduled for a fix exit; want none")
	}
	if _, claimed := state.Claimed["LF-RO-1"]; claimed {
		t.Error("claim still held after a fix exit; want released")
	}
	rkey := ReactionKey("LF-RO-1", ReactionKindLabelFix)
	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("label-fix entry re-seeded by its own exit; want none (repeatability comes from the reconcile re-enqueue)")
	}
}

// TestHandleWorkerExit_LabelFixExit_ErrorStillRetries is the contrast
// case: an error exit whose running entry carries ReactionKind==label-fix
// still schedules a retryable error-driven retry, confirming the
// claim-release guard is scoped to normal exits only.
func TestHandleWorkerExit_LabelFixExit_ErrorStillRetries(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "LF-RO-2", nil)
	state.Running["LF-RO-2"].ReactionKind = ReactionKindLabelFix

	params := defaultExitParams(t, store)

	HandleWorkerExit(state, WorkerResult{
		IssueID:      "LF-RO-2",
		Identifier:   "LF-RO-2-ident",
		ExitKind:     WorkerExitError,
		Error:        errors.New("agent crashed"),
		AgentAdapter: "mock",
	}, params)

	retry, ok := state.RetryAttempts["LF-RO-2"]
	if !ok {
		t.Fatal("retry not scheduled after a retryable error exit; want scheduled even for a label-fix entry")
	}
	if retry.ReactionKind != ReactionKindLabelFix {
		t.Errorf("RetryEntry.ReactionKind = %q, want %q (propagated from the exiting entry)", retry.ReactionKind, ReactionKindLabelFix)
	}
}

// --- merge-completion enqueue tests ---

// TestHandleWorkerExit_MergeCompletionEnqueue verifies that a normal exit
// with the SCM adapter present, the reaction configured, and workspace SCM
// metadata naming a pull request with owner and repo creates exactly one
// pending entry keyed by ReactionKey(issueID, ReactionKindMergeCompletion)
// carrying the expected *MergeCompletionReactionData; and that nothing is
// seeded when the reaction is not configured. The enqueue clause itself
// imposes no branch requirement, unlike the checkout-bearing sibling
// kinds; the fixtures below still carry a branch because that is the
// common production case, not because the reader requires one.
func TestHandleWorkerExit_MergeCompletionEnqueue(t *testing.T) {
	t.Parallel()

	t.Run("populates pending reaction with no branch check of its own", func(t *testing.T) {
		t.Parallel()

		wsPath := t.TempDir()
		writePRSCMMetadata(t, wsPath, 42, "corp", "api", "feature/MGC-1", "c0ffee")

		store := &mockExitStore{}
		state := exitState(t, "MGC-1", nil)
		params := defaultExitParams(t, store)
		params.SCMAdapter = &scmAdapterStubExit{}
		params.MergeCompletionReactionConfigured = true

		HandleWorkerExit(state, WorkerResult{
			IssueID:       "MGC-1",
			Identifier:    "MGC-1-ident",
			ExitKind:      WorkerExitNormal,
			AgentAdapter:  "mock",
			WorkspacePath: wsPath,
		}, params)

		rkey := ReactionKey("MGC-1", ReactionKindMergeCompletion)
		pr, ok := state.PendingReactions[rkey]
		if !ok {
			t.Fatal("PendingReactions[MGC-1:merge-completion] missing after normal exit with PR metadata")
		}
		if pr.Kind != ReactionKindMergeCompletion {
			t.Errorf("PendingReaction.Kind = %q, want %q", pr.Kind, ReactionKindMergeCompletion)
		}
		mcData, ok := pr.KindData.(*MergeCompletionReactionData)
		if !ok {
			t.Fatalf("KindData type = %T, want *MergeCompletionReactionData", pr.KindData)
		}
		if mcData.PRNumber != 42 {
			t.Errorf("MergeCompletionReactionData.PRNumber = %d, want 42", mcData.PRNumber)
		}
		if mcData.Owner != "corp" {
			t.Errorf("MergeCompletionReactionData.Owner = %q, want %q", mcData.Owner, "corp")
		}
		if mcData.Repo != "api" {
			t.Errorf("MergeCompletionReactionData.Repo = %q, want %q", mcData.Repo, "api")
		}
	})

	t.Run("not seeded when not configured", func(t *testing.T) {
		t.Parallel()

		wsPath := t.TempDir()
		writePRSCMMetadata(t, wsPath, 43, "corp", "api", "feature/MGC-NC", "deadbeef")

		store := &mockExitStore{}
		state := exitState(t, "MGC-NC", nil)
		params := defaultExitParams(t, store)
		params.SCMAdapter = &scmAdapterStubExit{}
		params.MergeCompletionReactionConfigured = false

		HandleWorkerExit(state, WorkerResult{
			IssueID:       "MGC-NC",
			Identifier:    "MGC-NC-ident",
			ExitKind:      WorkerExitNormal,
			AgentAdapter:  "mock",
			WorkspacePath: wsPath,
		}, params)

		rkey := ReactionKey("MGC-NC", ReactionKindMergeCompletion)
		if _, ok := state.PendingReactions[rkey]; ok {
			t.Error("PendingReactions[MGC-NC:merge-completion] present despite MergeCompletionReactionConfigured=false")
		}
	})

	t.Run("not seeded when SCM adapter is nil", func(t *testing.T) {
		t.Parallel()

		wsPath := t.TempDir()
		writePRSCMMetadata(t, wsPath, 44, "corp", "api", "feature/MGC-nil", "deadbeef")

		store := &mockExitStore{}
		state := exitState(t, "MGC-nil", nil)
		params := defaultExitParams(t, store)
		params.SCMAdapter = nil
		params.MergeCompletionReactionConfigured = true

		HandleWorkerExit(state, WorkerResult{
			IssueID:       "MGC-nil",
			Identifier:    "MGC-nil-ident",
			ExitKind:      WorkerExitNormal,
			AgentAdapter:  "mock",
			WorkspacePath: wsPath,
		}, params)

		rkey := ReactionKey("MGC-nil", ReactionKindMergeCompletion)
		if _, ok := state.PendingReactions[rkey]; ok {
			t.Error("PendingReactions[MGC-nil:merge-completion] present despite nil SCMAdapter")
		}
	})

	t.Run("does not overwrite an existing entry", func(t *testing.T) {
		t.Parallel()

		wsPath := t.TempDir()
		writePRSCMMetadata(t, wsPath, 45, "corp", "api", "feature/MGC-DUP", "sha1")

		store := &mockExitStore{}
		state := exitState(t, "MGC-DUP", nil)
		params := defaultExitParams(t, store)
		params.SCMAdapter = &scmAdapterStubExit{}
		params.MergeCompletionReactionConfigured = true

		existingEntry := &PendingReaction{
			IssueID: "MGC-DUP",
			Kind:    ReactionKindMergeCompletion,
			KindData: &MergeCompletionReactionData{
				PRNumber: 99,
				Owner:    "original",
				Repo:     "original",
			},
		}
		rkey := ReactionKey("MGC-DUP", ReactionKindMergeCompletion)
		state.PendingReactions[rkey] = existingEntry

		HandleWorkerExit(state, WorkerResult{
			IssueID:       "MGC-DUP",
			Identifier:    "MGC-DUP-ident",
			ExitKind:      WorkerExitNormal,
			AgentAdapter:  "mock",
			WorkspacePath: wsPath,
		}, params)

		got := state.PendingReactions[rkey]
		if got != existingEntry {
			t.Error("PendingReactions[MGC-DUP:merge-completion] was replaced; want existing entry preserved")
		}
	})

	t.Run("not seeded when PR metadata is incomplete", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name    string
			content string
		}{
			{name: "missing pr_number", content: `{"branch":"feature/x","owner":"corp","repo":"api"}`},
			{name: "missing owner", content: `{"branch":"feature/x","pr_number":10,"repo":"api"}`},
			{name: "missing repo", content: `{"branch":"feature/x","pr_number":10,"owner":"corp"}`},
			{name: "empty scm file", content: `{}`},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				wsPath := t.TempDir()
				dotSortie := filepath.Join(wsPath, ".sortie")
				if err := os.MkdirAll(dotSortie, 0o750); err != nil {
					t.Fatalf("MkdirAll .sortie: %v", err)
				}
				if err := os.WriteFile(filepath.Join(dotSortie, "scm.json"), []byte(tt.content), 0o600); err != nil {
					t.Fatalf("WriteFile scm.json: %v", err)
				}

				store := &mockExitStore{}
				state := exitState(t, "MGC-INC", nil)
				params := defaultExitParams(t, store)
				params.SCMAdapter = &scmAdapterStubExit{}
				params.MergeCompletionReactionConfigured = true

				HandleWorkerExit(state, WorkerResult{
					IssueID:       "MGC-INC",
					Identifier:    "MGC-INC-ident",
					ExitKind:      WorkerExitNormal,
					AgentAdapter:  "mock",
					WorkspacePath: wsPath,
				}, params)

				rkey := ReactionKey("MGC-INC", ReactionKindMergeCompletion)
				if _, ok := state.PendingReactions[rkey]; ok {
					t.Errorf("PendingReactions[MGC-INC:merge-completion] present despite incomplete SCM metadata (%s)", tt.name)
				}
			})
		}
	})
}

// --- Retry-slot arbitration: worker-exit incumbent protection ---

// TestHandleWorkerExit_HandoffFailureDeferral covers the two handoff
// branches that schedule a continuation on failure: the nil-adapter
// branch and the transition-failure branch. Both must defer to a
// foreign incumbent instead of scheduling their own retry.
func TestHandleWorkerExit_HandoffFailureDeferral(t *testing.T) {
	t.Parallel()

	t.Run("nil adapter branch defers to a foreign incumbent", func(t *testing.T) {
		t.Parallel()

		const issueID = "HO-NIL-DEFER"
		store := &mockExitStore{}
		state := exitStateWithIssue(t, issueID, "In Progress")
		state.RetryAttempts[issueID] = &RetryEntry{
			IssueID:      issueID,
			Attempt:      7,
			ReactionKind: ReactionKindCI,
		}
		spy := &spyMetrics{}
		params := defaultExitParams(t, store)
		params.HandoffState = "Human Review"
		params.ActiveStates = []string{"In Progress"}
		params.Metrics = spy
		// TrackerAdapter left nil.

		HandleWorkerExit(state, WorkerResult{
			IssueID:      issueID,
			Identifier:   issueID + "-ident",
			ExitKind:     WorkerExitNormal,
			AgentAdapter: "mock",
		}, params)

		incumbent, ok := state.RetryAttempts[issueID]
		if !ok {
			t.Fatal("incumbent removed on a deferral, want preserved")
		}
		if incumbent.Attempt != 7 {
			t.Errorf("RetryAttempts.Attempt = %d, want 7 (unchanged)", incumbent.Attempt)
		}
		if incumbent.ReactionKind != ReactionKindCI {
			t.Errorf("RetryAttempts.ReactionKind = %q, want %q (unchanged)", incumbent.ReactionKind, ReactionKindCI)
		}
		if _, ok := state.Claimed[issueID]; !ok {
			t.Error("claim released on a deferral, want held")
		}
		if len(spy.retries) != 0 {
			t.Errorf("retries = %v, want [] (triggerContinuation must not fire on a deferral)", spy.retries)
		}
		if len(spy.handoffTransitions) != 1 || spy.handoffTransitions[0] != handoffError {
			t.Errorf("handoffTransitions = %v, want [%s]", spy.handoffTransitions, handoffError)
		}
	})

	t.Run("transition-failure branch defers to a foreign incumbent", func(t *testing.T) {
		t.Parallel()

		const issueID = "HO-TXNFAIL-DEFER"
		store := &mockExitStore{}
		tracker := &mockTrackerAdapter{
			transitionIssueFn: func(_ context.Context, _, _ string) error {
				return errors.New("permission denied")
			},
		}
		state := exitStateWithIssue(t, issueID, "In Progress")
		state.RetryAttempts[issueID] = &RetryEntry{
			IssueID:      issueID,
			Attempt:      3,
			ReactionKind: ReactionKindReview,
		}
		spy := &spyMetrics{}
		params := defaultExitParams(t, store)
		params.TrackerAdapter = tracker
		params.HandoffState = "Human Review"
		params.ActiveStates = []string{"In Progress"}
		params.Metrics = spy

		HandleWorkerExit(state, WorkerResult{
			IssueID:      issueID,
			Identifier:   issueID + "-ident",
			ExitKind:     WorkerExitNormal,
			AgentAdapter: "mock",
		}, params)

		incumbent, ok := state.RetryAttempts[issueID]
		if !ok {
			t.Fatal("incumbent removed on a deferral, want preserved")
		}
		if incumbent.Attempt != 3 {
			t.Errorf("RetryAttempts.Attempt = %d, want 3 (unchanged)", incumbent.Attempt)
		}
		if _, ok := state.Claimed[issueID]; !ok {
			t.Error("claim released on a deferral, want held")
		}
		if len(spy.retries) != 0 {
			t.Errorf("retries = %v, want [] (triggerContinuation must not fire on a deferral)", spy.retries)
		}
		if len(spy.handoffTransitions) != 1 || spy.handoffTransitions[0] != handoffError {
			t.Errorf("handoffTransitions = %v, want [%s]", spy.handoffTransitions, handoffError)
		}
	})
}

// TestHandleWorkerExit_ActiveIssueContinuationDeferral covers the
// active-issue continuation branch: a foreign incumbent survives
// unchanged, no continuation retry counter fires, the claim stays held,
// no retry entry is persisted for this exit, and the completion comment
// still reports re-queuing even though this exit scheduled nothing.
func TestHandleWorkerExit_ActiveIssueContinuationDeferral(t *testing.T) {
	t.Parallel()

	const issueID = "R17-DEFER"
	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{}
	spy := newCommentAwareMetrics()
	state := exitStateWithIssue(t, issueID, "In Progress")
	state.RetryAttempts[issueID] = &RetryEntry{
		IssueID:      issueID,
		Attempt:      6,
		DueAtMS:      987654,
		ReactionKind: ReactionKindCI,
	}
	params := exitParamsWithComments(t, store, tracker, config.TrackerCommentsConfig{OnCompletion: true})
	params.Metrics = spy

	HandleWorkerExit(state, WorkerResult{
		IssueID:      issueID,
		Identifier:   issueID + "-ident",
		ExitKind:     WorkerExitNormal,
		AgentAdapter: "mock",
	}, params)

	spy.waitComment(t)

	incumbent, ok := state.RetryAttempts[issueID]
	if !ok {
		t.Fatal("incumbent removed on a deferral, want preserved")
	}
	if incumbent.ReactionKind != ReactionKindCI {
		t.Errorf("RetryAttempts.ReactionKind = %q, want %q", incumbent.ReactionKind, ReactionKindCI)
	}
	if incumbent.DueAtMS != 987654 {
		t.Errorf("RetryAttempts.DueAtMS = %d, want 987654 (unchanged)", incumbent.DueAtMS)
	}
	if len(spy.retries) != 0 {
		t.Errorf("retries = %v, want [] (triggerContinuation must not fire on a deferral)", spy.retries)
	}
	if _, ok := state.Claimed[issueID]; !ok {
		t.Error("claim released on a deferral, want held")
	}
	if len(store.retryEntries) != 0 {
		t.Errorf("SaveRetryEntry called %d times, want 0 (this exit scheduled nothing)", len(store.retryEntries))
	}
	if len(tracker.commentCalls) != 1 {
		t.Fatalf("CommentIssue call count = %d, want 1", len(tracker.commentCalls))
	}
	if !strings.HasPrefix(tracker.commentCalls[0].Text, "Sortie session completed (re-queuing).") {
		t.Errorf("completion comment = %q, want prefix %q", tracker.commentCalls[0].Text, "Sortie session completed (re-queuing).")
	}
}

// TestHandleWorkerExit_NonActiveDefaultDeferral covers the non-active
// default branch: a foreign incumbent survives and the claim stays
// held instead of being released.
func TestHandleWorkerExit_NonActiveDefaultDeferral(t *testing.T) {
	t.Parallel()

	const issueID = "R18-DEFER"
	store := &mockExitStore{}
	state := exitStateWithIssue(t, issueID, "Some Other State")
	state.RetryAttempts[issueID] = &RetryEntry{
		IssueID:      issueID,
		Attempt:      2,
		ReactionKind: ReactionKindCI,
	}
	params := defaultExitParams(t, store)
	params.ActiveStates = []string{"In Progress"}

	HandleWorkerExit(state, WorkerResult{
		IssueID:      issueID,
		Identifier:   issueID + "-ident",
		ExitKind:     WorkerExitNormal,
		AgentAdapter: "mock",
	}, params)

	if _, ok := state.RetryAttempts[issueID]; !ok {
		t.Error("ci entry removed by the non-active default branch, want preserved")
	}
	if _, ok := state.Claimed[issueID]; !ok {
		t.Error("claim released by the non-active default branch, want held")
	}
}

// TestHandleWorkerExit_RetryableErrorDeferral covers the retryable-error
// branch: a foreign incumbent survives with its own attempt, the error
// retry counter does not fire, the claim stays held, and the failure
// comment reports the incumbent's own attempt number.
func TestHandleWorkerExit_RetryableErrorDeferral(t *testing.T) {
	t.Parallel()

	const issueID = "R19-DEFER"
	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{}
	spy := newCommentAwareMetrics()
	state := exitState(t, issueID, nil)
	state.RetryAttempts[issueID] = &RetryEntry{
		IssueID:      issueID,
		Attempt:      5,
		ReactionKind: ReactionKindReview,
	}
	params := exitParamsWithComments(t, store, tracker, config.TrackerCommentsConfig{OnFailure: true})
	params.Metrics = spy

	turnTimeoutErr := &domain.AgentError{Kind: domain.ErrTurnTimeout, Message: "timed out"}

	HandleWorkerExit(state, WorkerResult{
		IssueID:      issueID,
		Identifier:   issueID + "-ident",
		ExitKind:     WorkerExitError,
		Error:        turnTimeoutErr,
		AgentAdapter: "mock",
	}, params)

	spy.waitComment(t)

	incumbent, ok := state.RetryAttempts[issueID]
	if !ok {
		t.Fatal("incumbent removed on a deferral, want preserved")
	}
	if incumbent.Attempt != 5 {
		t.Errorf("RetryAttempts.Attempt = %d, want 5 (unchanged)", incumbent.Attempt)
	}
	if incumbent.ReactionKind != ReactionKindReview {
		t.Errorf("RetryAttempts.ReactionKind = %q, want %q", incumbent.ReactionKind, ReactionKindReview)
	}
	if len(spy.retries) != 0 {
		t.Errorf("retries = %v, want [] (the error retry counter must not fire on a deferral)", spy.retries)
	}
	if _, ok := state.Claimed[issueID]; !ok {
		t.Error("claim released on a deferral, want held")
	}
	if len(tracker.commentCalls) != 1 {
		t.Fatalf("CommentIssue call count = %d, want 1", len(tracker.commentCalls))
	}
	if !strings.Contains(tracker.commentCalls[0].Text, "Retry: yes (attempt 5)") {
		t.Errorf("failure comment = %q, want to contain %q", tracker.commentCalls[0].Text, "Retry: yes (attempt 5)")
	}
}

// TestHandleWorkerExit_StopSignalDispositionsDestroyForeignIncumbent
// covers the six stop-signal dispositions that keep their destructive
// behavior even against a foreign incumbent: blocked soft stop,
// terminal observation, any other soft stop, the nil-adapter handoff
// soft stop, a verified-terminal handoff observation, and a
// transition-failure handoff soft stop.
func TestHandleWorkerExit_StopSignalDispositionsDestroyForeignIncumbent(t *testing.T) {
	t.Parallel()

	type stopCase struct {
		name       string
		issueID    string
		issueState string
		build      func(t *testing.T, store *mockExitStore) (HandleWorkerExitParams, WorkerResult)
	}

	cases := []stopCase{
		{
			name:       "blocked soft stop",
			issueID:    "STOP-BLOCKED",
			issueState: "In Progress",
			build: func(t *testing.T, store *mockExitStore) (HandleWorkerExitParams, WorkerResult) {
				t.Helper()
				params := defaultExitParams(t, store)
				params.ActiveStates = []string{"In Progress"}
				return params, WorkerResult{
					IssueID:        "STOP-BLOCKED",
					Identifier:     "STOP-BLOCKED-ident",
					ExitKind:       WorkerExitNormal,
					AgentAdapter:   "mock",
					SoftStop:       true,
					SoftStopReason: "blocked",
				}
			},
		},
		{
			name:       "terminal observation",
			issueID:    "STOP-TERMINAL",
			issueState: "ai:in-progress",
			build: func(t *testing.T, store *mockExitStore) (HandleWorkerExitParams, WorkerResult) {
				t.Helper()
				params := defaultExitParams(t, store)
				params.ActiveStates = []string{"ai:ready", "ai:in-progress"}
				params.TerminalStates = []string{"ai:done", "ai:cancelled"}
				return params, WorkerResult{
					IssueID:            "STOP-TERMINAL",
					Identifier:         "STOP-TERMINAL-ident",
					ExitKind:           WorkerExitNormal,
					AgentAdapter:       "mock",
					ObservedIssueState: "ai:cancelled",
				}
			},
		},
		{
			name:       "other soft stop",
			issueID:    "STOP-OTHER",
			issueState: "In Progress",
			build: func(t *testing.T, store *mockExitStore) (HandleWorkerExitParams, WorkerResult) {
				t.Helper()
				params := defaultExitParams(t, store)
				params.ActiveStates = []string{"In Progress"}
				return params, WorkerResult{
					IssueID:        "STOP-OTHER",
					Identifier:     "STOP-OTHER-ident",
					ExitKind:       WorkerExitNormal,
					AgentAdapter:   "mock",
					SoftStop:       true,
					SoftStopReason: "needs-human-review",
				}
			},
		},
		{
			name:       "nil-adapter handoff soft stop",
			issueID:    "STOP-NILADAPTER",
			issueState: "In Progress",
			build: func(t *testing.T, store *mockExitStore) (HandleWorkerExitParams, WorkerResult) {
				t.Helper()
				params := defaultExitParams(t, store)
				params.ActiveStates = []string{"In Progress"}
				params.HandoffState = "Human Review"
				return params, WorkerResult{
					IssueID:        "STOP-NILADAPTER",
					Identifier:     "STOP-NILADAPTER-ident",
					ExitKind:       WorkerExitNormal,
					AgentAdapter:   "mock",
					SoftStop:       true,
					SoftStopReason: "needs-human-review",
				}
			},
		},
		{
			name:       "verified-terminal handoff observation",
			issueID:    "STOP-VERIFIED",
			issueState: "ai:in-progress",
			build: func(t *testing.T, store *mockExitStore) (HandleWorkerExitParams, WorkerResult) {
				t.Helper()
				tracker := &mockTrackerAdapter{
					fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
						result := make(map[string]string, len(ids))
						for _, id := range ids {
							result[id] = "ai:cancelled"
						}
						return result, nil
					},
				}
				params := defaultExitParams(t, store)
				params.TrackerAdapter = tracker
				params.ActiveStates = []string{"ai:ready", "ai:in-progress"}
				params.TerminalStates = []string{"ai:done", "ai:cancelled"}
				params.HandoffState = "ai:in-review"
				return params, WorkerResult{
					IssueID:            "STOP-VERIFIED",
					Identifier:         "STOP-VERIFIED-ident",
					ExitKind:           WorkerExitNormal,
					AgentAdapter:       "mock",
					ObservedIssueState: "ai:in-progress",
				}
			},
		},
		{
			name:       "transition-failure handoff soft stop",
			issueID:    "STOP-TXNFAIL",
			issueState: "In Progress",
			build: func(t *testing.T, store *mockExitStore) (HandleWorkerExitParams, WorkerResult) {
				t.Helper()
				tracker := &mockTrackerAdapter{
					transitionIssueFn: func(_ context.Context, _, _ string) error {
						return errors.New("permission denied")
					},
				}
				params := defaultExitParams(t, store)
				params.TrackerAdapter = tracker
				params.ActiveStates = []string{"In Progress"}
				params.HandoffState = "Human Review"
				return params, WorkerResult{
					IssueID:        "STOP-TXNFAIL",
					Identifier:     "STOP-TXNFAIL-ident",
					ExitKind:       WorkerExitNormal,
					AgentAdapter:   "mock",
					SoftStop:       true,
					SoftStopReason: "needs-human-review",
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store := &mockExitStore{}
			state := exitStateWithIssue(t, tc.issueID, tc.issueState)
			state.RetryAttempts[tc.issueID] = &RetryEntry{
				IssueID:      tc.issueID,
				Attempt:      9,
				ReactionKind: ReactionKindCI,
			}
			params, result := tc.build(t, store)

			HandleWorkerExit(state, result, params)

			if _, ok := state.RetryAttempts[tc.issueID]; ok {
				t.Error("RetryAttempts entry survived a stop-signal disposition, want destroyed even against a foreign incumbent")
			}
			if _, ok := state.Claimed[tc.issueID]; ok {
				t.Error("Claimed entry survived a stop-signal disposition, want released even against a foreign incumbent")
			}
		})
	}

	t.Run("blocked soft stop destroys a label-review incumbent too", func(t *testing.T) {
		t.Parallel()

		const issueID = "STOP-LABELREVIEW"
		store := &mockExitStore{}
		state := exitStateWithIssue(t, issueID, "In Progress")
		state.RetryAttempts[issueID] = &RetryEntry{
			IssueID:      issueID,
			Attempt:      1,
			ReactionKind: ReactionKindLabelReview,
		}
		params := defaultExitParams(t, store)
		params.ActiveStates = []string{"In Progress"}

		HandleWorkerExit(state, WorkerResult{
			IssueID:        issueID,
			Identifier:     issueID + "-ident",
			ExitKind:       WorkerExitNormal,
			AgentAdapter:   "mock",
			SoftStop:       true,
			SoftStopReason: "blocked",
		}, params)

		if _, ok := state.RetryAttempts[issueID]; ok {
			t.Error("label-review RetryAttempts entry survived a blocked soft stop, want destroyed")
		}
		if _, ok := state.Claimed[issueID]; ok {
			t.Error("Claimed entry survived a blocked soft stop, want released")
		}
	})
}

// TestHandleWorkerExit_SuccessfulHandoffPreservesIncumbent covers the
// successful-transition arm of the handoff disposition: a foreign
// incumbent is preserved rather than cancelled, the claim stays held,
// and the incumbent's own timer later dispatches from the handoff
// state. The free-slot case is the unaffected control.
func TestHandleWorkerExit_SuccessfulHandoffPreservesIncumbent(t *testing.T) {
	t.Parallel()

	t.Run("slot occupied: incumbent preserved, claim held, then dispatches from the handoff state", func(t *testing.T) {
		t.Parallel()

		const issueID = "HO-PRESERVE"
		store := &mockExitStore{}
		tracker := &mockTrackerAdapter{}
		state := exitStateWithIssue(t, issueID, "In Progress")
		state.RetryAttempts[issueID] = &RetryEntry{
			IssueID:      issueID,
			Attempt:      4,
			DueAtMS:      123456,
			ReactionKind: ReactionKindCI,
		}
		spy := &spyMetrics{}
		params := defaultExitParams(t, store)
		params.TrackerAdapter = tracker
		params.HandoffState = "Human Review"
		params.ActiveStates = []string{"In Progress"}
		params.Metrics = spy

		HandleWorkerExit(state, WorkerResult{
			IssueID:      issueID,
			Identifier:   issueID + "-ident",
			ExitKind:     WorkerExitNormal,
			AgentAdapter: "mock",
		}, params)

		if len(tracker.transitionCalls) != 1 {
			t.Fatalf("TransitionIssue called %d times, want 1", len(tracker.transitionCalls))
		}
		if tracker.transitionCalls[0].TargetState != "Human Review" {
			t.Errorf("TransitionIssue TargetState = %q, want %q", tracker.transitionCalls[0].TargetState, "Human Review")
		}
		if len(spy.handoffTransitions) != 1 || spy.handoffTransitions[0] != handoffSuccess {
			t.Errorf("handoffTransitions = %v, want [%s]", spy.handoffTransitions, handoffSuccess)
		}
		incumbent, ok := state.RetryAttempts[issueID]
		if !ok {
			t.Fatal("incumbent removed after a successful handoff transition, want preserved")
		}
		if incumbent.ReactionKind != ReactionKindCI {
			t.Errorf("RetryAttempts.ReactionKind = %q, want %q", incumbent.ReactionKind, ReactionKindCI)
		}
		if incumbent.Attempt != 4 {
			t.Errorf("RetryAttempts.Attempt = %d, want 4 (unchanged)", incumbent.Attempt)
		}
		if incumbent.DueAtMS != 123456 {
			t.Errorf("RetryAttempts.DueAtMS = %d, want 123456 (unchanged)", incumbent.DueAtMS)
		}
		if _, ok := state.Claimed[issueID]; !ok {
			t.Error("claim released after a successful handoff transition with an incumbent, want held")
		}

		// Firing the incumbent's own timer against a tracker reporting the
		// handoff state must dispatch: isActive is false, isKnownReaction
		// is true, isHandoff is true, so HandleRetryTimer falls through to
		// dispatch instead of taking a paused-reschedule arm.
		retryStore := &mockRetryStore{}
		retryTracker := &mockRetryTracker{
			fetchedIssue: candidateIssue(issueID, issueID+"-ident", "Human Review"),
		}
		retryParams := defaultRetryParams(t, retryStore, retryTracker)
		retryParams.HandoffState = "Human Review"

		HandleRetryTimer(state, issueID, retryParams)

		if _, running := state.Running[issueID]; !running {
			t.Error("the CI-fix incumbent did not dispatch from the handoff state, want running")
		}
	})

	t.Run("slot free: claim released, matching current behavior", func(t *testing.T) {
		t.Parallel()

		const issueID = "HO-FREE-CONTROL"
		store := &mockExitStore{}
		tracker := &mockTrackerAdapter{}
		state := exitStateWithIssue(t, issueID, "In Progress")
		params := defaultExitParams(t, store)
		params.TrackerAdapter = tracker
		params.HandoffState = "Human Review"
		params.ActiveStates = []string{"In Progress"}

		HandleWorkerExit(state, WorkerResult{
			IssueID:      issueID,
			Identifier:   issueID + "-ident",
			ExitKind:     WorkerExitNormal,
			AgentAdapter: "mock",
		}, params)

		if _, ok := state.Claimed[issueID]; ok {
			t.Error("claim preserved after a successful handoff transition with a free slot, want released")
		}
		if _, ok := state.RetryAttempts[issueID]; ok {
			t.Error("retry entry present after a free-slot successful handoff, want none")
		}
	})
}

// TestHandleWorkerExit_LabelReviewExitDoesNotWidenReactionSeeding covers
// R28: retaining the claim to protect an incumbent on the non-active
// default branch must not widen which reaction kinds a read-only
// label-review exit seeds. Every reaction kind is configured and the
// workspace carries full PR metadata, so the only variable between the
// two subtests is whether the retry slot is occupied.
func TestHandleWorkerExit_LabelReviewExitDoesNotWidenReactionSeeding(t *testing.T) {
	t.Parallel()

	buildParams := func(t *testing.T, store *mockExitStore) HandleWorkerExitParams {
		t.Helper()
		params := defaultExitParams(t, store)
		params.ActiveStates = []string{"In Progress"}
		params.CIProvider = &ciProviderStubExit{}
		params.SCMAdapter = &scmAdapterStubExit{}
		params.AutoMergeReactionConfigured = true
		params.BotReviewReactionConfigured = true
		params.MergeConflictReactionConfigured = true
		params.LabelReviewReactionConfigured = true
		params.LabelFixReactionConfigured = true
		params.MergeCompletionReactionConfigured = true
		return params
	}

	t.Run("slot occupied: incumbent survives, no PendingReactions gained", func(t *testing.T) {
		t.Parallel()

		const issueID = "LR-WIDEN-OCCUPIED"
		wsPath := t.TempDir()
		writePRSCMMetadata(t, wsPath, 55, "acme", "widgets", "feature/lr", "sha-lr")

		store := &mockExitStore{}
		state := exitStateWithIssue(t, issueID, "In Progress")
		state.Running[issueID].ReactionKind = ReactionKindLabelReview
		state.RetryAttempts[issueID] = &RetryEntry{
			IssueID:      issueID,
			Attempt:      2,
			ReactionKind: ReactionKindCI,
		}
		params := buildParams(t, store)

		HandleWorkerExit(state, WorkerResult{
			IssueID:       issueID,
			Identifier:    issueID + "-ident",
			ExitKind:      WorkerExitNormal,
			AgentAdapter:  "mock",
			WorkspacePath: wsPath,
		}, params)

		incumbent, ok := state.RetryAttempts[issueID]
		if !ok {
			t.Fatal("incumbent removed by a label-review exit, want preserved")
		}
		if incumbent.ReactionKind != ReactionKindCI {
			t.Errorf("RetryAttempts.ReactionKind = %q, want %q", incumbent.ReactionKind, ReactionKindCI)
		}
		if _, ok := state.Claimed[issueID]; !ok {
			t.Error("claim released by a label-review exit with an occupied slot, want held")
		}
		if len(state.PendingReactions) != 0 {
			t.Errorf("PendingReactions count = %d, want 0 (no reaction kind may be seeded by a label-review exit)", len(state.PendingReactions))
		}
	})

	t.Run("slot free: entry cancelled, claim released, still no PendingReactions gained", func(t *testing.T) {
		t.Parallel()

		const issueID = "LR-WIDEN-FREE"
		wsPath := t.TempDir()
		writePRSCMMetadata(t, wsPath, 56, "acme", "widgets", "feature/lr2", "sha-lr2")

		store := &mockExitStore{}
		state := exitStateWithIssue(t, issueID, "In Progress")
		state.Running[issueID].ReactionKind = ReactionKindLabelReview
		params := buildParams(t, store)

		HandleWorkerExit(state, WorkerResult{
			IssueID:       issueID,
			Identifier:    issueID + "-ident",
			ExitKind:      WorkerExitNormal,
			AgentAdapter:  "mock",
			WorkspacePath: wsPath,
		}, params)

		if _, ok := state.RetryAttempts[issueID]; ok {
			t.Error("retry entry present after a free-slot label-review exit, want none")
		}
		if _, ok := state.Claimed[issueID]; ok {
			t.Error("claim preserved after a free-slot label-review exit, want released")
		}
		if len(state.PendingReactions) != 0 {
			t.Errorf("PendingReactions count = %d, want 0 (no reaction kind may be seeded by a label-review exit)", len(state.PendingReactions))
		}
	})
}

// TestHandleWorkerExit_CompletionSignalAfterSelfReview verifies that a run
// ending on the completion signal, whose self-review phase ran and
// recorded metadata, takes the same exit disposition it took before the
// phase existed: the handoff transition fires where configured and the
// issue is active, the continuation retry stays suppressed, and the claim
// is released. HandleWorkerExit reads only SoftStop and SoftStopReason
// from the result, so the phase having run ahead of the exit must not
// change the disposition those two fields already produced.
func TestHandleWorkerExit_CompletionSignalAfterSelfReview(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{}
	state := exitStateWithIssue(t, "CS-1", "In Progress")
	params := defaultExitParams(t, store)
	params.TrackerAdapter = tracker
	params.HandoffState = "Human Review"
	params.ActiveStates = []string{"In Progress"}

	reviewMeta := &domain.ReviewMetadata{
		Enabled:         true,
		TotalIterations: 1,
		FinalVerdict:    "pass",
		Iterations: []domain.ReviewIterationRecord{
			{Iteration: 1, Verdict: "pass"},
		},
	}

	HandleWorkerExit(state, WorkerResult{
		IssueID:        "CS-1",
		Identifier:     "CS-1-ident",
		ExitKind:       WorkerExitNormal,
		AgentAdapter:   "mock",
		SoftStop:       true,
		SoftStopReason: "needs-human-review",
		ReviewMetadata: reviewMeta,
	}, params)

	if len(tracker.transitionCalls) != 1 {
		t.Fatalf("TransitionIssue called %d times, want 1 (handoff must fire for a completion-signal run that went through self-review)", len(tracker.transitionCalls))
	}
	if tracker.transitionCalls[0].TargetState != "Human Review" {
		t.Errorf("TransitionIssue TargetState = %q, want %q", tracker.transitionCalls[0].TargetState, "Human Review")
	}
	if _, ok := state.RetryAttempts["CS-1"]; ok {
		t.Error("continuation retry scheduled after a completion-signal handoff, want suppressed")
	}
	if _, ok := state.Claimed["CS-1"]; ok {
		t.Error("claim preserved after a completion-signal handoff, want released")
	}
	if _, ok := state.Completed["CS-1"]; !ok {
		t.Error("issue not added to Completed set after a completion-signal handoff")
	}
	if len(store.runHistories) != 1 {
		t.Fatalf("AppendRunHistory called %d times, want 1", len(store.runHistories))
	}
	if store.runHistories[0].ReviewMetadata == nil {
		t.Error("RunHistory.ReviewMetadata = nil, want the marshaled review metadata")
	}
}
