package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/workspace"
)

type mockExitStore struct {
	runHistories    []persistence.RunHistory
	metrics         []persistence.AggregateMetrics
	sessionMetadata []persistence.SessionMetadata
	retryEntries    []persistence.RetryEntry
	deletedRetryIDs []string

	// absenceResetAt maps an issue ID to the run-history watermark at
	// which its absence sequence was last reset.
	absenceResetAt map[string]int
	absenceResetOf []string

	absenceCountedIssueIDs []string

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
	m.absenceCountedIssueIDs = append(m.absenceCountedIssueIDs, issueIDs...)
	if m.absenceCountErr != nil {
		return nil, m.absenceCountErr
	}
	counts := make(map[string]int, len(issueIDs))
	for _, issueID := range issueIDs {
		// Only a recorded reset ends the sequence; a "succeeded" status
		// does not, since it also covers outcomes with no verdict.
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

// CountWorkerRunsCompletedSince errs: this double is not expected to
// answer an attribution query.
func (m *mockExitStore) CountWorkerRunsCompletedSince(_ context.Context, _ string, _ time.Time) (int, error) {
	return 0, errors.New("worker run count is unsupported by this test double")
}

var baseTime = time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)

func noopRetryFire(_ string) {}

// exitState builds a *State with a running entry and claim, StartedAt at
// baseTime.
func exitState(t *testing.T, issueID string, retryAttempt *int) *State {
	t.Helper()
	state := NewState(5000, 4, 0, nil, AgentTotals{})
	state.Running[issueID] = &RunningEntry{
		Identifier:   issueID + "-ident",
		StartedAt:    baseTime,
		RetryAttempt: retryAttempt,
	}
	state.Claimed[issueID] = struct{}{}
	return state
}

// defaultExitParams returns params with NowFunc fixed at baseTime + 60s.
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

func TestComputeBackoffDelay(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		attempt           int
		maxRetryBackoffMS int
		want              int64
	}{
		// Default cap (300000), attempts 1..7.
		{name: "attempt 1 default cap", attempt: 1, maxRetryBackoffMS: 300_000, want: 10_000},
		{name: "attempt 2 default cap", attempt: 2, maxRetryBackoffMS: 300_000, want: 20_000},
		{name: "attempt 3 default cap", attempt: 3, maxRetryBackoffMS: 300_000, want: 40_000},
		{name: "attempt 4 default cap", attempt: 4, maxRetryBackoffMS: 300_000, want: 80_000},
		{name: "attempt 5 default cap", attempt: 5, maxRetryBackoffMS: 300_000, want: 160_000},
		{name: "attempt 6 default cap", attempt: 6, maxRetryBackoffMS: 300_000, want: 300_000},
		{name: "attempt 7 default cap", attempt: 7, maxRetryBackoffMS: 300_000, want: 300_000},

		{name: "attempt 1 custom cap 60000", attempt: 1, maxRetryBackoffMS: 60_000, want: 10_000},
		{name: "attempt 2 custom cap 60000", attempt: 2, maxRetryBackoffMS: 60_000, want: 20_000},
		{name: "attempt 3 custom cap 60000", attempt: 3, maxRetryBackoffMS: 60_000, want: 40_000},
		{name: "attempt 4 custom cap 60000", attempt: 4, maxRetryBackoffMS: 60_000, want: 60_000},

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

	if _, ok := state.Running["ISSUE-1"]; ok {
		t.Error("Running entry not removed after normal exit")
	}

	if state.AgentTotals.SecondsRunning != 60 {
		t.Errorf("AgentTotals.SecondsRunning = %f, want 60", state.AgentTotals.SecondsRunning)
	}

	if _, ok := state.Completed["ISSUE-1"]; !ok {
		t.Error("issue not added to Completed set after normal exit")
	}

	if _, ok := state.Claimed["ISSUE-1"]; !ok {
		t.Error("claim released after normal exit, should be preserved")
	}

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

	if len(store.runHistories) != 1 {
		t.Fatalf("AppendRunHistory called %d times, want 1", len(store.runHistories))
	}
	if store.runHistories[0].Status != "succeeded" {
		t.Errorf("RunHistory.Status = %q, want %q", store.runHistories[0].Status, "succeeded")
	}
	if store.runHistories[0].Error != nil {
		t.Errorf("RunHistory.Error = %v, want nil", store.runHistories[0].Error)
	}

	if len(store.metrics) != 1 {
		t.Fatalf("UpsertAggregateMetrics called %d times, want 1", len(store.metrics))
	}
	if store.metrics[0].SecondsRunning != 60 {
		t.Errorf("AggregateMetrics.SecondsRunning = %f, want 60", store.metrics[0].SecondsRunning)
	}

	if len(store.retryEntries) != 1 {
		t.Fatalf("SaveRetryEntry called %d times, want 1", len(store.retryEntries))
	}
	if store.retryEntries[0].Attempt != 1 {
		t.Errorf("persisted retry Attempt = %d, want 1", store.retryEntries[0].Attempt)
	}
}

func TestHandleWorkerExit_NoneArrivalDiscardIsPreserved(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "ISS-NONE-EXIT", nil)
	entry := state.Running["ISS-NONE-EXIT"]
	entry.UsageArrival = registry.UsageArrivalNone

	HandleAgentEvent(state, "ISS-NONE-EXIT", domain.AgentEvent{
		Type:      domain.EventTokenUsage,
		Timestamp: time.Now().UTC(),
		Model:     "discarded-model",
		Usage:     domain.TokenUsage{InputTokens: 120, OutputTokens: 30, TotalTokens: 150, CacheReadTokens: 10},
	}, discardLogger(), nil)

	HandleWorkerExit(state, WorkerResult{
		IssueID:      "ISS-NONE-EXIT",
		Identifier:   "ISS-NONE-EXIT-ident",
		ExitKind:     WorkerExitNormal,
		AgentAdapter: "mock",
	}, defaultExitParams(t, store))

	if len(store.runHistories) != 1 {
		t.Fatalf("AppendRunHistory called %d times, want 1", len(store.runHistories))
	}
	run := store.runHistories[0]
	if run.TokensMeasured {
		t.Error("RunHistory.TokensMeasured = true, want false")
	}
	if run.InputTokens != 0 || run.OutputTokens != 0 || run.TotalTokens != 0 || run.CacheReadTokens != 0 {
		t.Errorf("RunHistory tokens = (%d, %d, %d, %d), want all zero",
			run.InputTokens, run.OutputTokens, run.TotalTokens, run.CacheReadTokens)
	}

	if len(store.sessionMetadata) != 1 {
		t.Fatalf("UpsertSessionMetadata called %d times, want 1", len(store.sessionMetadata))
	}
	meta := store.sessionMetadata[0]
	if meta.InputTokens != 0 || meta.OutputTokens != 0 || meta.TotalTokens != 0 || meta.CacheReadTokens != 0 {
		t.Errorf("SessionMetadata tokens = (%d, %d, %d, %d), want all zero",
			meta.InputTokens, meta.OutputTokens, meta.TotalTokens, meta.CacheReadTokens)
	}
	if meta.ModelName != "" {
		t.Errorf("SessionMetadata.ModelName = %q, want empty", meta.ModelName)
	}
	if meta.APIRequestsMeasured {
		t.Error("SessionMetadata.APIRequestsMeasured = true, want false")
	}

	if state.AgentTotals.InputTokens != 0 || state.AgentTotals.OutputTokens != 0 ||
		state.AgentTotals.TotalTokens != 0 || state.AgentTotals.CacheReadTokens != 0 {
		t.Errorf("State.AgentTotals token components = (%d, %d, %d, %d), want all zero (unchanged since before dispatch)",
			state.AgentTotals.InputTokens, state.AgentTotals.OutputTokens,
			state.AgentTotals.TotalTokens, state.AgentTotals.CacheReadTokens)
	}
}

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

	if len(store.sessionMetadata) != 1 {
		t.Fatalf("UpsertSessionMetadata called %d times, want 1", len(store.sessionMetadata))
	}
	meta := store.sessionMetadata[0]
	if meta.TotalTokens != run.TotalTokens {
		t.Errorf("SessionMetadata.TotalTokens = %d, want %d (parity with run_history)", meta.TotalTokens, run.TotalTokens)
	}
}

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
	// HandleAgentEvent because a full agentEventCh dropped it.
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

	t.Run("a measured run carries its unaccounted-turn count onto the row", func(t *testing.T) {
		t.Parallel()

		store := &mockExitStore{}
		state := exitState(t, "ISSUE-UNACC", nil)
		state.Running["ISSUE-UNACC"].UsageMeasured = true

		HandleWorkerExit(state, WorkerResult{
			IssueID:          "ISSUE-UNACC",
			Identifier:       "ISSUE-UNACC-ident",
			ExitKind:         WorkerExitNormal,
			AgentAdapter:     "mock",
			UsageMeasured:    true,
			UnaccountedTurns: 2,
		}, defaultExitParams(t, store))

		if len(store.runHistories) != 1 {
			t.Fatalf("AppendRunHistory called %d times, want 1", len(store.runHistories))
		}
		run := store.runHistories[0]
		if run.UnaccountedTurns != 2 {
			t.Errorf("RunHistory.UnaccountedTurns = %d, want 2", run.UnaccountedTurns)
		}
		if !run.TokensMeasured {
			t.Error("RunHistory.TokensMeasured = false, want true: an unaccounted turn does not deny a measurement")
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

func TestHandleWorkerExit_ModelNameReconciliation(t *testing.T) {
	t.Parallel()

	t.Run("a non-empty result model name overwrites an empty entry across every exit kind", func(t *testing.T) {
		t.Parallel()

		for _, kind := range []WorkerExitKind{WorkerExitNormal, WorkerExitError, WorkerExitCancelled} {
			t.Run(string(kind), func(t *testing.T) {
				t.Parallel()

				store := &mockExitStore{}
				issueID := "ISSUE-MODEL-" + string(kind)
				state := exitState(t, issueID, nil)

				HandleWorkerExit(state, WorkerResult{
					IssueID:      issueID,
					Identifier:   issueID + "-ident",
					ExitKind:     kind,
					AgentAdapter: "mock",
					ModelName:    "m",
				}, defaultExitParams(t, store))

				if len(store.sessionMetadata) != 1 {
					t.Fatalf("UpsertSessionMetadata called %d times, want 1", len(store.sessionMetadata))
				}
				if got := store.sessionMetadata[0].ModelName; got != "m" {
					t.Errorf("SessionMetadata.ModelName = %q, want %q", got, "m")
				}
			})
		}
	})

	t.Run("result model name wins over a non-empty entry", func(t *testing.T) {
		t.Parallel()

		store := &mockExitStore{}
		state := exitState(t, "ISSUE-MODEL-AB", nil)
		state.Running["ISSUE-MODEL-AB"].ModelName = "a"

		HandleWorkerExit(state, WorkerResult{
			IssueID:      "ISSUE-MODEL-AB",
			Identifier:   "ISSUE-MODEL-AB-ident",
			ExitKind:     WorkerExitNormal,
			AgentAdapter: "mock",
			ModelName:    "b",
		}, defaultExitParams(t, store))

		if got := store.sessionMetadata[0].ModelName; got != "b" {
			t.Errorf("SessionMetadata.ModelName = %q, want %q (result overwrites entry)", got, "b")
		}
	})

	t.Run("empty result model name leaves the entry's own value in place", func(t *testing.T) {
		t.Parallel()

		store := &mockExitStore{}
		state := exitState(t, "ISSUE-MODEL-A-EMPTY", nil)
		state.Running["ISSUE-MODEL-A-EMPTY"].ModelName = "a"

		HandleWorkerExit(state, WorkerResult{
			IssueID:      "ISSUE-MODEL-A-EMPTY",
			Identifier:   "ISSUE-MODEL-A-EMPTY-ident",
			ExitKind:     WorkerExitNormal,
			AgentAdapter: "mock",
		}, defaultExitParams(t, store))

		if got := store.sessionMetadata[0].ModelName; got != "a" {
			t.Errorf("SessionMetadata.ModelName = %q, want %q (unchanged when the result carries none)", got, "a")
		}
	})

	t.Run("both empty persists empty", func(t *testing.T) {
		t.Parallel()

		store := &mockExitStore{}
		state := exitState(t, "ISSUE-MODEL-EMPTY", nil)

		HandleWorkerExit(state, WorkerResult{
			IssueID:      "ISSUE-MODEL-EMPTY",
			Identifier:   "ISSUE-MODEL-EMPTY-ident",
			ExitKind:     WorkerExitNormal,
			AgentAdapter: "mock",
		}, defaultExitParams(t, store))

		if got := store.sessionMetadata[0].ModelName; got != "" {
			t.Errorf("SessionMetadata.ModelName = %q, want empty", got)
		}
	})
}

func TestHandleWorkerExit_RequestCountReconciliation(t *testing.T) {
	t.Parallel()

	t.Run("a lower entry count is raised to the worker's, and measured turns true", func(t *testing.T) {
		t.Parallel()

		store := &mockExitStore{}
		state := exitState(t, "ISSUE-REQ-RAISE", nil)
		entry := state.Running["ISSUE-REQ-RAISE"]
		entry.UsageArrival = registry.UsageArrivalIncremental
		entry.TurnCount = 1
		entry.APIRequestCount = 0

		HandleWorkerExit(state, WorkerResult{
			IssueID:         "ISSUE-REQ-RAISE",
			Identifier:      "ISSUE-REQ-RAISE-ident",
			ExitKind:        WorkerExitNormal,
			AgentAdapter:    "mock",
			APIRequestCount: 3,
		}, defaultExitParams(t, store))

		meta := store.sessionMetadata[0]
		if !meta.APIRequestsMeasured {
			t.Fatal("SessionMetadata.APIRequestsMeasured = false, want true")
		}
		if meta.APIRequestCount != 3 {
			t.Errorf("SessionMetadata.APIRequestCount = %d, want 3", meta.APIRequestCount)
		}
	})

	t.Run("a higher entry count is kept, the worker's lower figure never lowers it", func(t *testing.T) {
		t.Parallel()

		store := &mockExitStore{}
		state := exitState(t, "ISSUE-REQ-KEEP", nil)
		entry := state.Running["ISSUE-REQ-KEEP"]
		entry.UsageArrival = registry.UsageArrivalIncremental
		entry.TurnCount = 1
		entry.APIRequestCount = 5

		HandleWorkerExit(state, WorkerResult{
			IssueID:         "ISSUE-REQ-KEEP",
			Identifier:      "ISSUE-REQ-KEEP-ident",
			ExitKind:        WorkerExitNormal,
			AgentAdapter:    "mock",
			APIRequestCount: 3,
		}, defaultExitParams(t, store))

		if got := store.sessionMetadata[0].APIRequestCount; got != 5 {
			t.Errorf("SessionMetadata.APIRequestCount = %d, want 5 (entry's own higher tally)", got)
		}
	})

	t.Run("a turn_end session still stores zero with the verdict false", func(t *testing.T) {
		t.Parallel()

		store := &mockExitStore{}
		state := exitState(t, "ISSUE-REQ-TURNEND", nil)
		entry := state.Running["ISSUE-REQ-TURNEND"]
		entry.UsageArrival = registry.UsageArrivalTurnEnd

		HandleWorkerExit(state, WorkerResult{
			IssueID:         "ISSUE-REQ-TURNEND",
			Identifier:      "ISSUE-REQ-TURNEND-ident",
			ExitKind:        WorkerExitNormal,
			AgentAdapter:    "mock",
			APIRequestCount: 2,
		}, defaultExitParams(t, store))

		meta := store.sessionMetadata[0]
		if meta.APIRequestsMeasured {
			t.Error("SessionMetadata.APIRequestsMeasured = true, want false (turn_end never reports during the turn)")
		}
		if meta.APIRequestCount != 0 {
			t.Errorf("SessionMetadata.APIRequestCount = %d, want 0", meta.APIRequestCount)
		}
	})
}

func TestHandleWorkerExit_ReconciliationNoOpWhenResultMatchesEntry(t *testing.T) {
	t.Parallel()

	buildRecords := func(t *testing.T, result WorkerResult) (persistence.RunHistory, persistence.AggregateMetrics, persistence.SessionMetadata) {
		t.Helper()

		store := &mockExitStore{}
		state := exitState(t, "ISSUE-P4-NOOP", nil)
		entry := state.Running["ISSUE-P4-NOOP"]
		entry.UsageArrival = registry.UsageArrivalIncremental
		entry.TurnCount = 1
		entry.UsageMeasured = true
		entry.ModelName = "m"
		entry.APIRequestCount = 5
		entry.AgentInputTokens = 100
		entry.AgentOutputTokens = 50
		entry.AgentTotalTokens = 150
		entry.CacheReadTokens = 10
		entry.LastReportedInputTokens = 100
		entry.LastReportedOutputTokens = 50
		entry.LastReportedTotalTokens = 150
		entry.LastReportedCacheReadTokens = 10

		result.IssueID = "ISSUE-P4-NOOP"
		result.Identifier = "ISSUE-P4-NOOP-ident"
		result.ExitKind = WorkerExitNormal
		result.AgentAdapter = "mock"

		HandleWorkerExit(state, result, defaultExitParams(t, store))

		if len(store.runHistories) != 1 || len(store.metrics) != 1 || len(store.sessionMetadata) != 1 {
			t.Fatalf("AppendRunHistory/UpsertAggregateMetrics/UpsertSessionMetadata called (%d, %d, %d) times, want (1, 1, 1)",
				len(store.runHistories), len(store.metrics), len(store.sessionMetadata))
		}
		return store.runHistories[0], store.metrics[0], store.sessionMetadata[0]
	}

	matchingResult := WorkerResult{
		Usage:           domain.TokenUsage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150, CacheReadTokens: 10},
		UsageMeasured:   true,
		ModelName:       "m",
		APIRequestCount: 5,
	}
	zeroResult := WorkerResult{}

	matchingRun, matchingMetrics, matchingMeta := buildRecords(t, matchingResult)
	zeroRun, zeroMetrics, zeroMeta := buildRecords(t, zeroResult)

	if !reflect.DeepEqual(matchingRun, zeroRun) {
		t.Errorf("RunHistory = %+v, want %+v (equal to the zero-result record)", matchingRun, zeroRun)
	}
	if !reflect.DeepEqual(matchingMetrics, zeroMetrics) {
		t.Errorf("AggregateMetrics = %+v, want %+v (equal to the zero-result record)", matchingMetrics, zeroMetrics)
	}
	if !reflect.DeepEqual(matchingMeta, zeroMeta) {
		t.Errorf("SessionMetadata = %+v, want %+v (equal to the zero-result record)", matchingMeta, zeroMeta)
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

	if _, ok := state.Running["ISSUE-2"]; ok {
		t.Error("Running entry not removed after error exit")
	}

	if _, ok := state.Completed["ISSUE-2"]; ok {
		t.Error("issue added to Completed set after error exit, should not be")
	}

	if _, ok := state.Claimed["ISSUE-2"]; !ok {
		t.Error("claim released after retryable error exit, should be preserved")
	}

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

	if len(store.runHistories) != 1 {
		t.Fatalf("AppendRunHistory called %d times, want 1", len(store.runHistories))
	}
	if store.runHistories[0].Status != "failed" {
		t.Errorf("RunHistory.Status = %q, want %q", store.runHistories[0].Status, "failed")
	}
	if store.runHistories[0].Error == nil {
		t.Error("RunHistory.Error is nil, want error string")
	}

	if len(store.retryEntries) != 1 {
		t.Fatalf("SaveRetryEntry called %d times, want 1", len(store.retryEntries))
	}
}

func TestHandleWorkerExit_RetryableReactionErrorPreservesContext(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "ISSUE-R", new(2))
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

	if _, ok := state.Running["ISSUE-3"]; ok {
		t.Error("Running entry not removed after non-retryable error exit")
	}

	if _, ok := state.Claimed["ISSUE-3"]; ok {
		t.Error("claim preserved after non-retryable error exit, should be released")
	}

	if _, ok := state.RetryAttempts["ISSUE-3"]; ok {
		t.Error("retry scheduled after non-retryable error exit, should not be")
	}

	if _, ok := state.Completed["ISSUE-3"]; ok {
		t.Error("issue added to Completed set after non-retryable error exit")
	}

	if len(store.runHistories) != 1 {
		t.Fatalf("AppendRunHistory called %d times, want 1", len(store.runHistories))
	}
	if store.runHistories[0].Status != "failed" {
		t.Errorf("RunHistory.Status = %q, want %q", store.runHistories[0].Status, "failed")
	}

	if len(store.retryEntries) != 0 {
		t.Errorf("SaveRetryEntry called %d times, want 0", len(store.retryEntries))
	}
}

// TestHandleWorkerExit_InputRequiredStatus records status "needs_person"
// for a wrapped ErrTurnInputRequired. The error is seeded wrapped as the
// worker delivers it, so a raw type assertion would miss it: this is the
// fixture that catches a regression from errors.AsType to a raw
// assertion.
func TestHandleWorkerExit_InputRequiredStatus(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "ISSUE-IR-1", nil)
	params := defaultExitParams(t, store)

	wrappedErr := fmt.Errorf("agent turn 1: %w", &domain.AgentError{
		Kind:    domain.ErrTurnInputRequired,
		Message: "agent asked for a decision only a person can make",
	})

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "ISSUE-IR-1",
		Identifier:    "ISSUE-IR-1-ident",
		ExitKind:      WorkerExitError,
		Error:         wrappedErr,
		AgentAdapter:  "mock",
		WorkspacePath: "/tmp/ws",
	}, params)

	if len(store.runHistories) != 1 {
		t.Fatalf("AppendRunHistory called %d times, want 1", len(store.runHistories))
	}
	if got := store.runHistories[0].Status; got != "needs_person" {
		t.Errorf("RunHistory.Status = %q, want %q", got, "needs_person")
	}
}

func TestHandleWorkerExit_CancelledWithInputRequiredErrorStaysCancelled(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "ISSUE-IR-2", nil)
	params := defaultExitParams(t, store)

	wrappedErr := fmt.Errorf("agent turn 1: %w", &domain.AgentError{
		Kind:    domain.ErrTurnInputRequired,
		Message: "agent asked for a decision only a person can make",
	})

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "ISSUE-IR-2",
		Identifier:    "ISSUE-IR-2-ident",
		ExitKind:      WorkerExitCancelled,
		Error:         wrappedErr,
		AgentAdapter:  "mock",
		WorkspacePath: "/tmp/ws",
	}, params)

	if len(store.runHistories) != 1 {
		t.Fatalf("AppendRunHistory called %d times, want 1", len(store.runHistories))
	}
	if got := store.runHistories[0].Status; got != "cancelled" {
		t.Errorf("RunHistory.Status = %q, want %q", got, "cancelled")
	}
}

func TestHandleWorkerExit_InputRequiredMessageDistinctFromFailedAndZeroWork(t *testing.T) {
	t.Parallel()

	const stem = "agent asked for a decision only a person can make"

	runExit := func(t *testing.T, issueID string, agentErr *domain.AgentError) string {
		t.Helper()

		store := &mockExitStore{}
		state := exitState(t, issueID, nil)
		params := defaultExitParams(t, store)

		HandleWorkerExit(state, WorkerResult{
			IssueID:       issueID,
			Identifier:    issueID + "-ident",
			ExitKind:      WorkerExitError,
			Error:         fmt.Errorf("agent turn 1: %w", agentErr),
			AgentAdapter:  "mock",
			WorkspacePath: "/tmp/ws",
		}, params)

		if len(store.runHistories) != 1 {
			t.Fatalf("AppendRunHistory called %d times, want 1", len(store.runHistories))
		}
		if store.runHistories[0].Error == nil {
			t.Fatalf("RunHistory.Error = nil, want non-nil")
		}
		return *store.runHistories[0].Error
	}

	needsPersonMessage := runExit(t, "ISSUE-IR-3", &domain.AgentError{Kind: domain.ErrTurnInputRequired, Message: stem})
	turnFailedMessage := runExit(t, "ISSUE-IR-4", &domain.AgentError{Kind: domain.ErrTurnFailed, Message: "turn failed"})
	zeroWorkMessage := runExit(t, "ISSUE-IR-5", &domain.AgentError{Kind: domain.ErrTurnFailed, Message: "agent exited without producing output"})

	if !strings.Contains(needsPersonMessage, stem) {
		t.Errorf("needs_person RunHistory.Error = %q, want to contain %q", needsPersonMessage, stem)
	}
	if strings.Contains(strings.ToLower(needsPersonMessage), "timeout") {
		t.Errorf("needs_person RunHistory.Error = %q, want no timeout vocabulary", needsPersonMessage)
	}
	if needsPersonMessage == turnFailedMessage {
		t.Errorf("needs_person and turn_failed RunHistory.Error are identical: %q", needsPersonMessage)
	}
	if needsPersonMessage == zeroWorkMessage {
		t.Errorf("needs_person and zero-work RunHistory.Error are identical: %q", needsPersonMessage)
	}
}

func TestHandleWorkerExit_InputRequiredReleasesClaimNoRetry(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "ISSUE-IR-6", nil)
	params := defaultExitParams(t, store)

	wrappedErr := fmt.Errorf("agent turn 1: %w", &domain.AgentError{
		Kind:    domain.ErrTurnInputRequired,
		Message: "agent asked for a decision only a person can make",
	})

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "ISSUE-IR-6",
		Identifier:    "ISSUE-IR-6-ident",
		ExitKind:      WorkerExitError,
		Error:         wrappedErr,
		AgentAdapter:  "mock",
		WorkspacePath: "/tmp/ws",
	}, params)

	if _, ok := state.Claimed["ISSUE-IR-6"]; ok {
		t.Error("claim preserved after human-input-required exit, should be released")
	}
	if _, ok := state.RetryAttempts["ISSUE-IR-6"]; ok {
		t.Error("retry scheduled after human-input-required exit, should not be")
	}
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

	if _, ok := state.Running["ISSUE-4"]; ok {
		t.Error("Running entry not removed after cancelled exit")
	}

	if _, ok := state.Claimed["ISSUE-4"]; ok {
		t.Error("claim preserved after cancelled exit, should be released")
	}

	if _, ok := state.RetryAttempts["ISSUE-4"]; ok {
		t.Error("retry scheduled after cancelled exit, should not be")
	}

	if _, ok := state.Completed["ISSUE-4"]; ok {
		t.Error("issue added to Completed set after cancelled exit")
	}

	if len(store.runHistories) != 1 {
		t.Fatalf("AppendRunHistory called %d times, want 1", len(store.runHistories))
	}
	if store.runHistories[0].Status != "cancelled" {
		t.Errorf("RunHistory.Status = %q, want %q", store.runHistories[0].Status, "cancelled")
	}

	if len(store.retryEntries) != 0 {
		t.Errorf("SaveRetryEntry called %d times, want 0", len(store.retryEntries))
	}
}

func TestHandleWorkerExit_TokenCeilingStoppedRecordsBudgetStoppedStatus(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "ISSUE-CEIL", nil)
	// A reload moved the configured ceiling between the stop and this
	// exit. The record must name the ceiling the run actually hit, not
	// whichever one is current.
	state.MaxTokens = 900
	entry := state.Running["ISSUE-CEIL"]
	entry.TokenCeilingStopped = true
	entry.TokenCeilingAtStop = 500
	entry.IssueTokensCompleted = 300
	entry.AgentTotalTokens = 250

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "ISSUE-CEIL",
		Identifier:    "ISSUE-CEIL-ident",
		ExitKind:      WorkerExitCancelled,
		AgentAdapter:  "mock",
		WorkspacePath: "/tmp/ws",
	}, defaultExitParams(t, store))

	if len(store.runHistories) != 1 {
		t.Fatalf("AppendRunHistory called %d times, want 1", len(store.runHistories))
	}
	run := store.runHistories[0]
	if run.Status != "budget_stopped" {
		t.Errorf("RunHistory.Status = %q, want %q", run.Status, "budget_stopped")
	}
	if run.Error == nil || !strings.Contains(*run.Error, "550") || !strings.Contains(*run.Error, "500") {
		t.Errorf("RunHistory.Error = %v, want it to name used tokens 550 and budgeted tokens 500", run.Error)
	}
	if run.Error != nil && strings.Contains(*run.Error, "900") {
		t.Errorf("RunHistory.Error = %q names the reloaded ceiling instead of the one the run hit", *run.Error)
	}
	if len(store.retryEntries) != 0 {
		t.Errorf("SaveRetryEntry called %d times, want 0 (a ceiling stop schedules no retry of its own)", len(store.retryEntries))
	}
	if _, ok := state.RetryAttempts["ISSUE-CEIL"]; ok {
		t.Error("RetryAttempts[ISSUE-CEIL] present after a ceiling-stopped exit, want none")
	}
}

func TestHandleWorkerExit_CancelledWithoutTokenCeilingStopStaysCancelled(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "ISSUE-STALL", nil)
	state.MaxTokens = 500

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "ISSUE-STALL",
		Identifier:    "ISSUE-STALL-ident",
		ExitKind:      WorkerExitCancelled,
		AgentAdapter:  "mock",
		WorkspacePath: "/tmp/ws",
	}, defaultExitParams(t, store))

	if len(store.runHistories) != 1 {
		t.Fatalf("AppendRunHistory called %d times, want 1", len(store.runHistories))
	}
	if got := store.runHistories[0].Status; got != "cancelled" {
		t.Errorf("RunHistory.Status = %q, want %q", got, "cancelled")
	}
}

// TestHandleWorkerExit_TokenCeilingLatchIgnoredOnNonCancelledExit: a
// latched TokenCeilingStopped entry does not produce "budget_stopped" on
// a non-cancelled exit, since the run may finish on its own in the window
// between the cancel and the worker noticing it.
func TestHandleWorkerExit_TokenCeilingLatchIgnoredOnNonCancelledExit(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "ISSUE-RACE", nil)
	state.MaxTokens = 500
	entry := state.Running["ISSUE-RACE"]
	entry.TokenCeilingStopped = true
	entry.IssueTokensCompleted = 300
	entry.AgentTotalTokens = 250

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "ISSUE-RACE",
		Identifier:    "ISSUE-RACE-ident",
		ExitKind:      WorkerExitNormal,
		AgentAdapter:  "mock",
		WorkspacePath: "/tmp/ws",
	}, defaultExitParams(t, store))

	if len(store.runHistories) != 1 {
		t.Fatalf("AppendRunHistory called %d times, want 1", len(store.runHistories))
	}
	if got := store.runHistories[0].Status; got != "succeeded" {
		t.Errorf("RunHistory.Status = %q, want %q (a latched ceiling stop must not override a non-cancelled exit)", got, "succeeded")
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

	if len(store.metrics) != 1 {
		t.Fatalf("UpsertAggregateMetrics called %d times, want 1", len(store.metrics))
	}
	if store.metrics[0].SecondsRunning != want {
		t.Errorf("AggregateMetrics.SecondsRunning = %f, want %f", store.metrics[0].SecondsRunning, want)
	}
}

func TestHandleWorkerExit_UnmeasuredSessionsCounter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		entryMeasured   bool
		entryArrival    registry.UsageArrival
		resultMeasured  bool
		wantIncremented bool
	}{
		{
			name:            "neither source measured: increments",
			wantIncremented: true,
		},
		{
			name:            "arrival none, never measured: increments",
			entryArrival:    registry.UsageArrivalNone,
			wantIncremented: true,
		},
		{
			name:            "entry itself measured: does not increment",
			entryMeasured:   true,
			wantIncremented: false,
		},
		{
			name:            "measurement recovered only from WorkerResult: does not increment",
			resultMeasured:  true,
			wantIncremented: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store := &mockExitStore{}
			state := exitState(t, "ISSUE-CTR", nil)
			state.Running["ISSUE-CTR"].UsageMeasured = tt.entryMeasured
			state.Running["ISSUE-CTR"].UsageArrival = tt.entryArrival
			state.AgentTotals.UnmeasuredSessions = 5

			HandleWorkerExit(state, WorkerResult{
				IssueID:       "ISSUE-CTR",
				Identifier:    "ISSUE-CTR-ident",
				ExitKind:      WorkerExitNormal,
				AgentAdapter:  "mock",
				UsageMeasured: tt.resultMeasured,
			}, defaultExitParams(t, store))

			want := int64(5)
			if tt.wantIncremented {
				want = 6
			}
			if state.AgentTotals.UnmeasuredSessions != want {
				t.Errorf("AgentTotals.UnmeasuredSessions = %d, want %d", state.AgentTotals.UnmeasuredSessions, want)
			}

			if len(store.metrics) != 1 {
				t.Fatalf("UpsertAggregateMetrics called %d times, want 1", len(store.metrics))
			}
			if store.metrics[0].UnmeasuredSessions != want {
				t.Errorf("persisted AggregateMetrics.UnmeasuredSessions = %d, want %d", store.metrics[0].UnmeasuredSessions, want)
			}
		})
	}
}

func TestHandleWorkerExit_UnmeasuredSessionsAccumulatesAcrossExits(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := NewState(5000, 4, 0, nil, AgentTotals{})

	for _, issueID := range []string{"ISSUE-A", "ISSUE-B", "ISSUE-C"} {
		state.Running[issueID] = &RunningEntry{
			Identifier: issueID + "-ident",
			StartedAt:  baseTime,
		}
		state.Claimed[issueID] = struct{}{}
	}

	measured := map[string]bool{"ISSUE-B": true}

	for _, issueID := range []string{"ISSUE-A", "ISSUE-B", "ISSUE-C"} {
		HandleWorkerExit(state, WorkerResult{
			IssueID:       issueID,
			Identifier:    issueID + "-ident",
			ExitKind:      WorkerExitNormal,
			AgentAdapter:  "mock",
			UsageMeasured: measured[issueID],
		}, defaultExitParams(t, store))
	}

	if state.AgentTotals.UnmeasuredSessions != 2 {
		t.Errorf("AgentTotals.UnmeasuredSessions = %d, want 2 (ISSUE-A and ISSUE-C)", state.AgentTotals.UnmeasuredSessions)
	}
}

func TestHandleWorkerExit_UnmeasuredSessionsSurvivesRestart(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dbPath := t.TempDir() + "/test.db"

	store1, err := persistence.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("persistence.Open: %v", err)
	}
	// Close is idempotent, so this only matters when a failure skips the
	// explicit close before the reopen below.
	t.Cleanup(func() { _ = store1.Close() })
	if err := store1.Migrate(ctx); err != nil {
		t.Fatalf("store1.Migrate: %v", err)
	}

	state1 := NewState(5000, 4, 0, nil, AgentTotals{})
	state1.Running["ISSUE-RESTART"] = &RunningEntry{
		Identifier: "PROJ-RESTART",
		StartedAt:  baseTime,
	}
	state1.Claimed["ISSUE-RESTART"] = struct{}{}

	HandleWorkerExit(state1, WorkerResult{
		IssueID:      "ISSUE-RESTART",
		Identifier:   "PROJ-RESTART",
		ExitKind:     WorkerExitNormal,
		AgentAdapter: "mock",
	}, HandleWorkerExitParams{
		Store:             store1,
		MaxRetryBackoffMS: 300_000,
		OnRetryFire:       noopRetryFire,
		NowFunc:           func() time.Time { return baseTime.Add(60 * time.Second) },
		Logger:            discardLogger(),
		Ctx:               ctx,
	})

	if state1.AgentTotals.UnmeasuredSessions != 1 {
		t.Fatalf("AgentTotals.UnmeasuredSessions before restart = %d, want 1", state1.AgentTotals.UnmeasuredSessions)
	}

	if err := store1.Close(); err != nil {
		t.Fatalf("store1.Close: %v", err)
	}

	store2, err := persistence.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("persistence.Open (reopen): %v", err)
	}
	t.Cleanup(func() {
		if err := store2.Close(); err != nil {
			t.Errorf("store2.Close: %v", err)
		}
	})

	metrics, found, err := store2.LoadAggregateMetrics(ctx, "agent_totals")
	if err != nil {
		t.Fatalf("LoadAggregateMetrics after reopening: %v", err)
	}
	if !found {
		t.Fatal("LoadAggregateMetrics after reopening: found = false, want true")
	}
	if metrics.UnmeasuredSessions != 1 {
		t.Errorf("reloaded AggregateMetrics.UnmeasuredSessions = %d, want 1", metrics.UnmeasuredSessions)
	}

	state2 := NewState(5000, 4, 0, nil, AgentTotals{
		InputTokens:        metrics.InputTokens,
		OutputTokens:       metrics.OutputTokens,
		TotalTokens:        metrics.TotalTokens,
		CacheReadTokens:    metrics.CacheReadTokens,
		SecondsRunning:     metrics.SecondsRunning,
		UnmeasuredSessions: metrics.UnmeasuredSessions,
	})
	if state2.AgentTotals.UnmeasuredSessions != 1 {
		t.Errorf("post-restart State.AgentTotals.UnmeasuredSessions = %d, want 1", state2.AgentTotals.UnmeasuredSessions)
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

	HandleWorkerExit(state, WorkerResult{
		IssueID:      "ISSUE-6",
		Identifier:   "ISSUE-6-ident",
		ExitKind:     WorkerExitNormal,
		AgentAdapter: "mock",
	}, params)

	if _, ok := state.Running["ISSUE-6"]; ok {
		t.Error("Running entry not removed despite persistence failure")
	}
	if _, ok := state.Completed["ISSUE-6"]; !ok {
		t.Error("Completed set not updated despite persistence failure")
	}
	if _, ok := state.RetryAttempts["ISSUE-6"]; !ok {
		t.Error("retry not scheduled despite persistence failure")
	}

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
	state := NewState(5000, 4, 0, nil, AgentTotals{})
	params := defaultExitParams(t, store)

	HandleWorkerExit(state, WorkerResult{
		IssueID:    "GHOST-999",
		Identifier: "GHOST-999",
		ExitKind:   WorkerExitNormal,
	}, params)

	if len(state.Running) != 0 {
		t.Errorf("Running map modified: len=%d, want 0", len(state.Running))
	}
	if len(state.Completed) != 0 {
		t.Errorf("Completed set modified: len=%d, want 0", len(state.Completed))
	}
	if state.AgentTotals != (AgentTotals{}) {
		t.Errorf("AgentTotals modified: %+v, want zero value", state.AgentTotals)
	}

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
	state := exitState(t, "ISSUE-8", new(3)) // RetryAttempt = 3
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
	state := exitState(t, "HIST-1", new(2))
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

type orderRecordingExitStore struct {
	mockExitStore
	callOrder []string
}

var _ WorkerExitStore = (*orderRecordingExitStore)(nil)

func (s *orderRecordingExitStore) UpsertSessionMetadata(ctx context.Context, meta persistence.SessionMetadata) error {
	s.callOrder = append(s.callOrder, "UpsertSessionMetadata")
	return s.mockExitStore.UpsertSessionMetadata(ctx, meta)
}

func (s *orderRecordingExitStore) AppendRunHistory(ctx context.Context, run persistence.RunHistory) (persistence.RunHistory, error) {
	s.callOrder = append(s.callOrder, "AppendRunHistory")
	return s.mockExitStore.AppendRunHistory(ctx, run)
}

func TestHandleWorkerExit_SessionExitWriteOrder(t *testing.T) {
	t.Parallel()

	t.Run("session-exit write precedes the run_history append and clears DispatchID", func(t *testing.T) {
		t.Parallel()

		store := &orderRecordingExitStore{}
		state := exitState(t, "SM-ORDER", nil)
		entry := state.Running["SM-ORDER"]
		entry.DispatchID = "dispatch-order-1"
		entry.SessionID = "ses-order"
		params := defaultExitParams(t, &store.mockExitStore)
		params.Store = store

		HandleWorkerExit(state, WorkerResult{
			IssueID:      "SM-ORDER",
			Identifier:   "SM-ORDER-ident",
			ExitKind:     WorkerExitNormal,
			SessionID:    "ses-order",
			AgentAdapter: "mock",
		}, params)

		if len(store.sessionMetadata) != 1 {
			t.Fatalf("UpsertSessionMetadata called %d times, want 1", len(store.sessionMetadata))
		}
		if got := store.sessionMetadata[0].DispatchID; got != "" {
			t.Errorf("SessionMetadata.DispatchID = %q, want empty (session-exit write always clears it)", got)
		}

		if len(store.callOrder) != 2 || store.callOrder[0] != "UpsertSessionMetadata" || store.callOrder[1] != "AppendRunHistory" {
			t.Errorf("call order = %v, want [UpsertSessionMetadata AppendRunHistory]", store.callOrder)
		}
	})

	t.Run("AppendRunHistory still runs once when the session-exit write fails", func(t *testing.T) {
		t.Parallel()

		store := &orderRecordingExitStore{}
		store.upsertSessionMetadataErr = fmt.Errorf("disk full")
		state := exitState(t, "SM-ORDER-ERR", nil)
		params := defaultExitParams(t, &store.mockExitStore)
		params.Store = store

		HandleWorkerExit(state, WorkerResult{
			IssueID:      "SM-ORDER-ERR",
			Identifier:   "SM-ORDER-ERR-ident",
			ExitKind:     WorkerExitNormal,
			AgentAdapter: "mock",
		}, params)

		if len(store.runHistories) != 1 {
			t.Fatalf("AppendRunHistory called %d times, want 1 (still runs after a failed session-exit write)", len(store.runHistories))
		}
		if len(store.callOrder) != 2 || store.callOrder[0] != "UpsertSessionMetadata" || store.callOrder[1] != "AppendRunHistory" {
			t.Errorf("call order = %v, want [UpsertSessionMetadata AppendRunHistory]", store.callOrder)
		}
	})
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

	if _, ok := state.Running["CAN-1"]; ok {
		t.Error("Running entry not removed after cancelled exit")
	}

	if _, ok := state.Claimed["CAN-1"]; !ok {
		t.Error("claim released despite pre-scheduled retry")
	}

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

	if _, ok := state.Running["CAN-2"]; ok {
		t.Error("Running entry not removed after cancelled exit")
	}

	if _, ok := state.Claimed["CAN-2"]; ok {
		t.Error("claim preserved without pre-scheduled retry")
	}
}

func TestHandleWorkerExit_PendingCleanupRemovesWorkspace(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "CLEAN-1", nil)
	state.Running["CLEAN-1"].PendingCleanup = true
	state.Running["CLEAN-1"].Identifier = "CLEAN-1-ident"

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

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "CFAIL-1",
		Identifier:    "CFAIL-1-ident",
		ExitKind:      WorkerExitCancelled,
		AgentAdapter:  "mock",
		WorkspacePath: wsDir,
	}, params)

	if _, ok := state.Running["CFAIL-1"]; ok {
		t.Error("Running entry not removed despite cleanup failure")
	}
}

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

	if _, err := os.Stat(actualWS); !os.IsNotExist(err) {
		t.Error("workspace at old root still exists, cleanup used wrong path")
	}

	newRootWS := filepath.Join(newRoot, "PROJ-99")
	if _, err := os.Stat(newRootWS); !os.IsNotExist(err) {
		t.Error("directory exists at new root, cleanup should not touch it")
	}
}

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
	if got := tracker.fetchStatesCalls.Load(); got != 1 {
		t.Errorf("FetchIssueStatesByIDs called %d times, want 1 (the verification read that precedes a withheld handoff)", got)
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

type suppressedExitWant struct {
	VerifiedState string
	Policy        config.HandoffEvidencePolicy
	Verdict       handoffEvidenceVerdict
}

type suppressedShapeFixture struct {
	Store         *mockExitStore
	Tracker       *mockTrackerAdapter
	Metrics       *spyMetrics
	State         *State
	Params        HandleWorkerExitParams
	Logs          *bytes.Buffer
	WorkspacePath string
	Baseline      *workspace.HandoffEvidenceBaseline
}

func newSuppressedShapeFixture(t *testing.T, issueID string) suppressedShapeFixture {
	t.Helper()
	dir, baseline := handoffEvidenceGitWorkspace(t)
	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{
		fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
			result := make(map[string]string, len(ids))
			for _, id := range ids {
				result[id] = "Done"
			}
			return result, nil
		},
	}
	spy := &spyMetrics{}
	state := exitStateWithIssue(t, issueID, "In Progress")
	params := handoffEvidenceExitParams(t, store, tracker, spy)
	params.CommentsConfig.OnFailure = true
	var logs bytes.Buffer
	params.Logger = debugLogger(t, &logs)
	return suppressedShapeFixture{
		Store: store, Tracker: tracker, Metrics: spy, State: state, Params: params, Logs: &logs,
		WorkspacePath: dir, Baseline: baseline,
	}
}

func assertSuppressedExitEffects(t *testing.T, f suppressedShapeFixture, issueID string, want suppressedExitWant) {
	t.Helper()

	if got := f.Tracker.fetchStatesCalls.Load(); got != 1 {
		t.Errorf("FetchIssueStatesByIDs called %d times, want 1", got)
	}
	if len(f.Tracker.transitionCalls) != 0 {
		t.Errorf("TransitionIssue called %d times, want 0", len(f.Tracker.transitionCalls))
	}
	if len(f.Tracker.commentCalls) != 0 {
		t.Errorf("CommentIssue called %d times, want 0: %+v", len(f.Tracker.commentCalls), f.Tracker.commentCalls)
	}
	if len(f.Store.runHistories) != 1 {
		t.Fatalf("AppendRunHistory calls = %d, want 1", len(f.Store.runHistories))
	}
	if got := f.Store.runHistories[0].Status; got != "succeeded" {
		t.Errorf("RunHistory.Status = %q, want %q", got, "succeeded")
	}
	if f.Store.runHistories[0].Error != nil {
		t.Errorf("RunHistory.Error = %q, want nil", *f.Store.runHistories[0].Error)
	}
	if len(f.Store.absenceCountedIssueIDs) != 0 {
		t.Errorf("QueryConsecutiveHandoffAbsenceCounts issue IDs = %v, want none", f.Store.absenceCountedIssueIDs)
	}
	if len(f.Store.parkedIssues) != 0 {
		t.Errorf("UpsertParkedIssue calls = %d, want 0", len(f.Store.parkedIssues))
	}
	if _, ok := f.State.Parked[issueID]; ok {
		t.Error("issue parked after a suppressed exit, want not parked")
	}
	if _, ok := f.State.RetryAttempts[issueID]; ok {
		t.Error("RetryAttempts entry present after a suppressed exit, want none")
	}
	if len(f.Store.retryEntries) != 0 {
		t.Errorf("SaveRetryEntry calls = %d, want 0", len(f.Store.retryEntries))
	}
	if _, ok := f.State.Claimed[issueID]; ok {
		t.Error("claim retained after a suppressed exit, want released")
	}
	if len(f.State.PendingReactions) != 0 {
		t.Errorf("PendingReactions = %v, want none", f.State.PendingReactions)
	}
	if len(f.Metrics.handoffTransitions) != 1 || f.Metrics.handoffTransitions[0] != handoffSkipped {
		t.Errorf("handoffTransitions = %v, want [%s]", f.Metrics.handoffTransitions, handoffSkipped)
	}

	output := f.Logs.String()
	if !strings.Contains(output, "withheld handoff suppressed for terminal issue") {
		t.Errorf("log output missing %q\ngot: %s", "withheld handoff suppressed for terminal issue", output)
	}
	if !strings.Contains(output, "state="+want.VerifiedState) {
		t.Errorf("log output missing verified state %q\ngot: %s", want.VerifiedState, output)
	}
	if !strings.Contains(output, "state_source=verified") {
		t.Errorf("log output missing state_source=verified\ngot: %s", output)
	}
	if !strings.Contains(output, "policy="+string(want.Policy)) {
		t.Errorf("log output missing policy=%q\ngot: %s", want.Policy, output)
	}
	if !strings.Contains(output, `verdict="`+string(want.Verdict)+`"`) {
		t.Errorf("log output missing verdict=%q\ngot: %s", want.Verdict, output)
	}
	if strings.Contains(output, "handoff withheld by evidence policy") {
		t.Errorf("log output contains the withheld-disposition warning, want suppressed:\n%s", output)
	}
}

func TestHandleWorkerExit_SuppressedTerminalShape1SoftStopWorkerSource(t *testing.T) {
	t.Parallel()

	const issueID = "SUP-SHAPE1"
	f := newSuppressedShapeFixture(t, issueID)

	HandleWorkerExit(f.State, WorkerResult{
		IssueID:                 issueID,
		Identifier:              issueID + "-ident",
		ExitKind:                WorkerExitNormal,
		TurnsCompleted:          2,
		WorkspacePath:           f.WorkspacePath,
		HandoffEvidencePolicy:   config.HandoffEvidenceObserved,
		HandoffEvidenceBaseline: f.Baseline,
		AgentAdapter:            "mock",
		SoftStop:                true,
		SoftStopReason:          "needs-human-review",
		ObservedIssueState:      "In Progress",
	}, f.Params)

	assertSuppressedExitEffects(t, f, issueID, suppressedExitWant{
		VerifiedState: "Done",
		Policy:        config.HandoffEvidenceObserved,
		Verdict:       handoffAbsenceObserved,
	})
}

func TestHandleWorkerExit_SuppressedTerminalShape2NonSoftStopWorkerSource(t *testing.T) {
	t.Parallel()

	const issueID = "SUP-SHAPE2"
	f := newSuppressedShapeFixture(t, issueID)

	HandleWorkerExit(f.State, WorkerResult{
		IssueID:                 issueID,
		Identifier:              issueID + "-ident",
		ExitKind:                WorkerExitNormal,
		TurnsCompleted:          2,
		WorkspacePath:           f.WorkspacePath,
		HandoffEvidencePolicy:   config.HandoffEvidenceObserved,
		HandoffEvidenceBaseline: f.Baseline,
		AgentAdapter:            "mock",
		ObservedIssueState:      "In Progress",
	}, f.Params)

	assertSuppressedExitEffects(t, f, issueID, suppressedExitWant{
		VerifiedState: "Done",
		Policy:        config.HandoffEvidenceObserved,
		Verdict:       handoffAbsenceObserved,
	})
}

func TestHandleWorkerExit_SuppressedTerminalShape3NonSoftStopSnapshotSource(t *testing.T) {
	t.Parallel()

	const issueID = "SUP-SHAPE3"
	f := newSuppressedShapeFixture(t, issueID)

	HandleWorkerExit(f.State, WorkerResult{
		IssueID:                 issueID,
		Identifier:              issueID + "-ident",
		ExitKind:                WorkerExitNormal,
		TurnsCompleted:          2,
		WorkspacePath:           f.WorkspacePath,
		HandoffEvidencePolicy:   config.HandoffEvidenceObserved,
		HandoffEvidenceBaseline: f.Baseline,
		AgentAdapter:            "mock",
	}, f.Params)

	assertSuppressedExitEffects(t, f, issueID, suppressedExitWant{
		VerifiedState: "Done",
		Policy:        config.HandoffEvidenceObserved,
		Verdict:       handoffAbsenceObserved,
	})
}

func TestHandleWorkerExit_SuppressedTerminalShape4SoftStopSnapshotSource(t *testing.T) {
	t.Parallel()

	const issueID = "SUP-SHAPE4"
	f := newSuppressedShapeFixture(t, issueID)

	HandleWorkerExit(f.State, WorkerResult{
		IssueID:                 issueID,
		Identifier:              issueID + "-ident",
		ExitKind:                WorkerExitNormal,
		TurnsCompleted:          2,
		WorkspacePath:           f.WorkspacePath,
		HandoffEvidencePolicy:   config.HandoffEvidenceObserved,
		HandoffEvidenceBaseline: f.Baseline,
		AgentAdapter:            "mock",
		SoftStop:                true,
		SoftStopReason:          "needs-human-review",
	}, f.Params)

	assertSuppressedExitEffects(t, f, issueID, suppressedExitWant{
		VerifiedState: "Done",
		Policy:        config.HandoffEvidenceObserved,
		Verdict:       handoffAbsenceObserved,
	})
}

func TestHandleWorkerExit_WithheldReadGating(t *testing.T) {
	t.Parallel()

	t.Run("active-state control: withheld disposition holds in full", func(t *testing.T) {
		t.Parallel()

		const issueID = "GATE-ACTIVE"
		dir, baseline := handoffEvidenceGitWorkspace(t)
		store := &mockExitStore{}
		tracker := &mockTrackerAdapter{
			fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
				result := make(map[string]string, len(ids))
				for _, id := range ids {
					result[id] = "In Progress"
				}
				return result, nil
			},
		}
		spy := &spyMetrics{}
		state := exitStateWithIssue(t, issueID, "In Progress")
		params := handoffEvidenceExitParams(t, store, tracker, spy)
		params.CommentsConfig.OnFailure = true

		HandleWorkerExit(state, WorkerResult{
			IssueID:                 issueID,
			Identifier:              issueID + "-ident",
			ExitKind:                WorkerExitNormal,
			WorkspacePath:           dir,
			HandoffEvidencePolicy:   config.HandoffEvidenceObserved,
			HandoffEvidenceBaseline: baseline,
			AgentAdapter:            "mock",
		}, params)
		t.Cleanup(func() { CancelRetry(state, issueID) })
		state.TrackerOpsWg.Wait()

		if got := tracker.fetchStatesCalls.Load(); got != 1 {
			t.Errorf("FetchIssueStatesByIDs called %d times, want 1", got)
		}
		if len(store.runHistories) != 1 || store.runHistories[0].Status != "failed" {
			t.Fatalf("run histories = %+v, want one failed row", store.runHistories)
		}
		if store.runHistories[0].Error == nil || !strings.HasPrefix(*store.runHistories[0].Error, persistence.HandoffAbsenceErrorPrefix) {
			t.Errorf("RunHistory.Error = %v, want the %q prefix", store.runHistories[0].Error, persistence.HandoffAbsenceErrorPrefix)
		}
		if len(spy.handoffTransitions) != 1 || spy.handoffTransitions[0] != handoffWithheld {
			t.Errorf("handoffTransitions = %v, want [%s]", spy.handoffTransitions, handoffWithheld)
		}
		if len(store.absenceCountedIssueIDs) != 1 {
			t.Errorf("QueryConsecutiveHandoffAbsenceCounts issue IDs = %v, want one call", store.absenceCountedIssueIDs)
		}
		retry, ok := state.RetryAttempts[issueID]
		if !ok {
			t.Fatal("retry not scheduled, want the withheld disposition to schedule one")
		}
		if retry.scheduledDelayMS != backoffBaseMS {
			t.Errorf("retry delay = %d, want exponential-backoff base %d", retry.scheduledDelayMS, backoffBaseMS)
		}
		if _, ok := state.Claimed[issueID]; !ok {
			t.Error("claim released, want retained on the withheld disposition")
		}
		if len(tracker.commentCalls) != 1 {
			t.Errorf("CommentIssue called %d times, want 1 (on_failure enabled)", len(tracker.commentCalls))
		}
	})

	t.Run("TerminalStates empty: no read, unchanged from pinned revision", func(t *testing.T) {
		t.Parallel()

		const issueID = "GATE-NOTERMINAL"
		dir, baseline := handoffEvidenceGitWorkspace(t)
		store := &mockExitStore{}
		tracker := &mockTrackerAdapter{}
		spy := &spyMetrics{}
		state := exitStateWithIssue(t, issueID, "In Progress")
		params := handoffEvidenceExitParams(t, store, tracker, spy)
		params.TerminalStates = nil

		HandleWorkerExit(state, WorkerResult{
			IssueID:                 issueID,
			Identifier:              issueID + "-ident",
			ExitKind:                WorkerExitNormal,
			WorkspacePath:           dir,
			HandoffEvidencePolicy:   config.HandoffEvidenceObserved,
			HandoffEvidenceBaseline: baseline,
			AgentAdapter:            "mock",
		}, params)
		t.Cleanup(func() { CancelRetry(state, issueID) })

		if got := tracker.fetchStatesCalls.Load(); got != 0 {
			t.Errorf("FetchIssueStatesByIDs called %d times, want 0 (no terminal states configured)", got)
		}
		if len(store.runHistories) != 1 || store.runHistories[0].Status != "failed" {
			t.Fatalf("run histories = %+v, want one failed row", store.runHistories)
		}
		if len(spy.handoffTransitions) != 1 || spy.handoffTransitions[0] != handoffWithheld {
			t.Errorf("handoffTransitions = %v, want [%s]", spy.handoffTransitions, handoffWithheld)
		}
		if _, ok := state.RetryAttempts[issueID]; !ok {
			t.Error("retry not scheduled, want the withheld disposition unchanged")
		}
	})

	t.Run("TrackerAdapter nil: no read, no panic, withheld disposition preserved", func(t *testing.T) {
		t.Parallel()

		const issueID = "GATE-NILADAPTER"
		dir, baseline := handoffEvidenceGitWorkspace(t)
		store := &mockExitStore{}
		state := exitStateWithIssue(t, issueID, "In Progress")
		params := defaultExitParams(t, store)
		params.HandoffState = "Human Review"
		params.ActiveStates = []string{"In Progress"}
		params.TerminalStates = []string{"Done"}
		// TrackerAdapter is left nil: the gate must exclude the read
		// without panicking on a nil-interface call.

		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("HandleWorkerExit panicked: %v", r)
				}
			}()
			HandleWorkerExit(state, WorkerResult{
				IssueID:                 issueID,
				Identifier:              issueID + "-ident",
				ExitKind:                WorkerExitNormal,
				WorkspacePath:           dir,
				HandoffEvidencePolicy:   config.HandoffEvidenceObserved,
				HandoffEvidenceBaseline: baseline,
				AgentAdapter:            "mock",
			}, params)
		}()
		t.Cleanup(func() { CancelRetry(state, issueID) })

		if len(store.runHistories) != 1 || store.runHistories[0].Status != "failed" {
			t.Fatalf("run histories = %+v, want one failed row", store.runHistories)
		}
		if _, ok := state.RetryAttempts[issueID]; !ok {
			t.Error("retry not scheduled, want the withheld disposition preserved")
		}
	})

	t.Run("off policy: no verdict computed, zero reads", func(t *testing.T) {
		t.Parallel()

		const issueID = "GATE-OFF"
		dir, baseline := handoffEvidenceGitWorkspace(t)
		store := &mockExitStore{}
		tracker := &mockTrackerAdapter{}
		state := exitStateWithIssue(t, issueID, "In Progress")
		params := defaultExitParams(t, store)
		params.TrackerAdapter = tracker
		params.HandoffState = "Human Review"
		params.ActiveStates = []string{"In Progress"}
		// TerminalStates is intentionally left unset: the handoff-write
		// path's own pre-existing verification read (unrelated to this
		// change) also gates only on len(TerminalStates) > 0, and
		// configuring it here would let that read fire too and confound
		// the zero-call assertion below. The property under test is that
		// the off policy skips evidence evaluation entirely, before
		// either read's gate is reached.

		HandleWorkerExit(state, WorkerResult{
			IssueID:                 issueID,
			Identifier:              issueID + "-ident",
			ExitKind:                WorkerExitNormal,
			WorkspacePath:           dir,
			HandoffEvidencePolicy:   config.HandoffEvidenceOff,
			HandoffEvidenceBaseline: baseline,
			AgentAdapter:            "mock",
		}, params)

		if got := tracker.fetchStatesCalls.Load(); got != 0 {
			t.Errorf("FetchIssueStatesByIDs called %d times, want 0 under the off policy", got)
		}
		if len(tracker.transitionCalls) != 1 {
			t.Fatalf("TransitionIssue called %d times, want 1 (the off policy never withholds)", len(tracker.transitionCalls))
		}
		if len(store.runHistories) != 1 || store.runHistories[0].Status != "succeeded" {
			t.Errorf("run histories = %+v, want one succeeded row", store.runHistories)
		}
	})

	t.Run("work observed: only the write path's own read fires", func(t *testing.T) {
		t.Parallel()

		const issueID = "GATE-WORKOBSERVED"
		dir, baseline := handoffEvidenceGitWorkspace(t)
		if err := os.WriteFile(filepath.Join(dir, "work.txt"), []byte("work\n"), 0o600); err != nil {
			t.Fatalf("write work file: %v", err)
		}
		store := &mockExitStore{}
		tracker := &mockTrackerAdapter{
			fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
				result := make(map[string]string, len(ids))
				for _, id := range ids {
					result[id] = "In Progress"
				}
				return result, nil
			},
		}
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

		if got := tracker.fetchStatesCalls.Load(); got != 1 {
			t.Errorf("FetchIssueStatesByIDs called %d times, want 1 (the write path's own read, not this change's)", got)
		}
		if len(tracker.transitionCalls) != 1 {
			t.Errorf("TransitionIssue called %d times, want 1", len(tracker.transitionCalls))
		}
	})
}

func TestHandleWorkerExit_WithheldReadFailOpen(t *testing.T) {
	t.Parallel()

	t.Run("read error: fail open, one Warn, positive retry line", func(t *testing.T) {
		t.Parallel()

		const issueID = "FAILOPEN-ERROR"
		dir, baseline := handoffEvidenceGitWorkspace(t)
		store := &mockExitStore{}
		readErr := errors.New("tracker unavailable")
		tracker := &mockTrackerAdapter{
			fetchStatesFn: func(_ context.Context, _ []string) (map[string]string, error) {
				return nil, readErr
			},
		}
		spy := newCommentAwareMetrics()
		state := exitStateWithIssue(t, issueID, "In Progress")
		params := handoffEvidenceExitParams(t, store, tracker, spy)
		params.CommentsConfig.OnFailure = true
		var logs bytes.Buffer
		params.Logger = debugLogger(t, &logs)

		HandleWorkerExit(state, WorkerResult{
			IssueID:                 issueID,
			Identifier:              issueID + "-ident",
			ExitKind:                WorkerExitNormal,
			WorkspacePath:           dir,
			HandoffEvidencePolicy:   config.HandoffEvidenceObserved,
			HandoffEvidenceBaseline: baseline,
			AgentAdapter:            "mock",
		}, params)
		t.Cleanup(func() { CancelRetry(state, issueID) })
		spy.waitComment(t)

		if len(store.runHistories) != 1 || store.runHistories[0].Status != "failed" {
			t.Fatalf("run histories = %+v, want one failed row", store.runHistories)
		}
		if len(spy.handoffTransitions) != 1 || spy.handoffTransitions[0] != handoffWithheld {
			t.Errorf("handoffTransitions = %v, want [%s]", spy.handoffTransitions, handoffWithheld)
		}
		retry, ok := state.RetryAttempts[issueID]
		if !ok {
			t.Fatal("retry not scheduled, want the withheld disposition preserved on a fail-open read")
		}
		if _, ok := state.Claimed[issueID]; !ok {
			t.Error("claim released, want retained on the withheld disposition")
		}

		output := logs.String()
		if !strings.Contains(output, "withheld handoff verification read failed, recording withheld handoff") {
			t.Errorf("log output missing the fail-open Warn\ngot: %s", output)
		}
		if !strings.Contains(output, readErr.Error()) {
			t.Errorf("log output missing the read error %q\ngot: %s", readErr.Error(), output)
		}

		if len(tracker.commentCalls) != 1 {
			t.Fatalf("CommentIssue called %d times, want 1", len(tracker.commentCalls))
		}
		wantRetryLine := fmt.Sprintf("Retry: yes (attempt %d)", retry.Attempt)
		if !strings.Contains(tracker.commentCalls[0].Text, wantRetryLine) {
			t.Errorf("failure comment = %q, want it to contain %q", tracker.commentCalls[0].Text, wantRetryLine)
		}
	})

	t.Run("read omits the issue: fail open, no new log record", func(t *testing.T) {
		t.Parallel()

		const issueID = "FAILOPEN-OMIT"
		dir, baseline := handoffEvidenceGitWorkspace(t)
		store := &mockExitStore{}
		tracker := &mockTrackerAdapter{
			fetchStatesFn: func(_ context.Context, _ []string) (map[string]string, error) {
				return map[string]string{}, nil
			},
		}
		spy := &spyMetrics{}
		state := exitStateWithIssue(t, issueID, "In Progress")
		params := handoffEvidenceExitParams(t, store, tracker, spy)
		var logs bytes.Buffer
		params.Logger = debugLogger(t, &logs)

		HandleWorkerExit(state, WorkerResult{
			IssueID:                 issueID,
			Identifier:              issueID + "-ident",
			ExitKind:                WorkerExitNormal,
			WorkspacePath:           dir,
			HandoffEvidencePolicy:   config.HandoffEvidenceObserved,
			HandoffEvidenceBaseline: baseline,
			AgentAdapter:            "mock",
		}, params)
		t.Cleanup(func() { CancelRetry(state, issueID) })

		if len(store.runHistories) != 1 || store.runHistories[0].Status != "failed" {
			t.Fatalf("run histories = %+v, want one failed row", store.runHistories)
		}
		if _, ok := state.RetryAttempts[issueID]; !ok {
			t.Error("retry not scheduled, want the withheld disposition preserved")
		}
		if _, ok := state.Claimed[issueID]; !ok {
			t.Error("claim released, want retained on the withheld disposition")
		}

		output := logs.String()
		if strings.Contains(output, "withheld handoff verification read failed") {
			t.Errorf("log output contains the fail-open Warn, want none for an omitted issue:\n%s", output)
		}
		if strings.Contains(output, "withheld handoff suppressed for terminal issue") {
			t.Errorf("log output contains the suppression Info, want none for an omitted issue:\n%s", output)
		}
	})
}

func TestHandleWorkerExit_StrictUndeterminableTerminalSuppresses(t *testing.T) {
	t.Parallel()

	const issueID = "STRICT-UNDETERMINED"
	f := newSuppressedShapeFixture(t, issueID)

	HandleWorkerExit(f.State, WorkerResult{
		IssueID:               issueID,
		Identifier:            issueID + "-ident",
		ExitKind:              WorkerExitNormal,
		TurnsCompleted:        1,
		HandoffEvidencePolicy: config.HandoffEvidenceStrict,
		AgentAdapter:          "mock",
		// WorkspacePath and HandoffEvidenceBaseline are deliberately not
		// taken from the fixture: an absent baseline makes the verdict
		// undeterminable regardless of workspace state, and this test's
		// whole point is that path, not the unchanged-workspace path the
		// fixture's Git repository exists to support for its other callers.
	}, f.Params)

	assertSuppressedExitEffects(t, f, issueID, suppressedExitWant{
		VerifiedState: "Done",
		Policy:        config.HandoffEvidenceStrict,
		Verdict:       handoffEvidenceUndetermined,
	})
}

func TestHandleWorkerExit_SuppressedExitDropsForeignIncumbent(t *testing.T) {
	t.Parallel()

	const issueID = "SUPPRESSED-INCUMBENT"
	dir, baseline := handoffEvidenceGitWorkspace(t)
	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{
		fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
			result := make(map[string]string, len(ids))
			for _, id := range ids {
				result[id] = "Done"
			}
			return result, nil
		},
	}
	state := exitStateWithIssue(t, issueID, "In Progress")
	state.RetryAttempts[issueID] = &RetryEntry{
		IssueID:      issueID,
		Attempt:      9,
		ReactionKind: ReactionKindCI,
	}
	params := handoffEvidenceExitParams(t, store, tracker, &spyMetrics{})
	var logs bytes.Buffer
	params.Logger = debugLogger(t, &logs)

	HandleWorkerExit(state, WorkerResult{
		IssueID:                 issueID,
		Identifier:              issueID + "-ident",
		ExitKind:                WorkerExitNormal,
		WorkspacePath:           dir,
		HandoffEvidencePolicy:   config.HandoffEvidenceObserved,
		HandoffEvidenceBaseline: baseline,
		AgentAdapter:            "mock",
	}, params)

	if _, ok := state.RetryAttempts[issueID]; ok {
		t.Error("RetryAttempts entry survived a suppressed exit, want destroyed even against a foreign incumbent")
	}
	if _, ok := state.Claimed[issueID]; ok {
		t.Error("claim survived a suppressed exit, want released")
	}
	if len(store.deletedRetryIDs) != 0 {
		t.Errorf("DeleteRetryEntry called for %v, want none: a suppressed exit never persists a deletion for a foreign incumbent's row", store.deletedRetryIDs)
	}
	if strings.Contains(logs.String(), "retry slot occupied, deferring") {
		t.Errorf("log output contains a retry-slot deferral, want none for a suppressed exit:\n%s", logs.String())
	}
}

func TestHandleWorkerExit_RetryPromiseInvariantAcrossWithheldFamily(t *testing.T) {
	t.Parallel()

	t.Run("suppressed: no comment mentions a retry", func(t *testing.T) {
		t.Parallel()

		const issueID = "PROMISE-SUPPRESSED"
		dir, baseline := handoffEvidenceGitWorkspace(t)
		store := &mockExitStore{}
		tracker := &mockTrackerAdapter{
			fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
				result := make(map[string]string, len(ids))
				for _, id := range ids {
					result[id] = "Done"
				}
				return result, nil
			},
		}
		spy := newCommentAwareMetrics()
		state := exitStateWithIssue(t, issueID, "In Progress")
		params := handoffEvidenceExitParams(t, store, tracker, spy)
		params.CommentsConfig.OnFailure = true
		params.CommentsConfig.OnCompletion = true

		HandleWorkerExit(state, WorkerResult{
			IssueID:                 issueID,
			Identifier:              issueID + "-ident",
			ExitKind:                WorkerExitNormal,
			WorkspacePath:           dir,
			HandoffEvidencePolicy:   config.HandoffEvidenceObserved,
			HandoffEvidenceBaseline: baseline,
			AgentAdapter:            "mock",
		}, params)
		spy.waitComment(t)

		if len(tracker.commentCalls) != 1 {
			t.Fatalf("CommentIssue called %d times, want 1", len(tracker.commentCalls))
		}
		if strings.Contains(strings.ToLower(tracker.commentCalls[0].Text), "retry") {
			t.Errorf("comment mentions a retry, want none for a suppressed exit\ngot: %q", tracker.commentCalls[0].Text)
		}
	})

	t.Run("parked at ceiling: negative form, no retry entry", func(t *testing.T) {
		t.Parallel()

		const issueID = "PROMISE-PARKED"
		dir, baseline := handoffEvidenceGitWorkspace(t)
		store := &mockExitStore{}
		seedMockHandoffAbsences(store, issueID, 2) // default ceiling is 3
		tracker := &mockTrackerAdapter{
			fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
				result := make(map[string]string, len(ids))
				for _, id := range ids {
					result[id] = "In Progress"
				}
				return result, nil
			},
		}
		spy := newCommentAwareMetrics()
		state := exitStateWithIssue(t, issueID, "In Progress")
		params := handoffEvidenceExitParams(t, store, tracker, spy)
		params.CommentsConfig.OnFailure = true

		HandleWorkerExit(state, WorkerResult{
			IssueID:                 issueID,
			Identifier:              issueID + "-ident",
			ExitKind:                WorkerExitNormal,
			WorkspacePath:           dir,
			HandoffEvidencePolicy:   config.HandoffEvidenceObserved,
			HandoffEvidenceBaseline: baseline,
			AgentAdapter:            "mock",
		}, params)
		spy.waitComment(t)

		if _, ok := state.RetryAttempts[issueID]; ok {
			t.Error("RetryAttempts entry present after parking, want none")
		}
		if len(tracker.commentCalls) != 1 {
			t.Fatalf("CommentIssue called %d times, want 1", len(tracker.commentCalls))
		}
		if !strings.Contains(tracker.commentCalls[0].Text, "Retry: no") {
			t.Errorf("comment = %q, want the negative retry form", tracker.commentCalls[0].Text)
		}
	})

	t.Run("scheduled retry: positive form with the scheduled attempt", func(t *testing.T) {
		t.Parallel()

		const issueID = "PROMISE-SCHEDULED"
		dir, baseline := handoffEvidenceGitWorkspace(t)
		store := &mockExitStore{}
		tracker := &mockTrackerAdapter{
			fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
				result := make(map[string]string, len(ids))
				for _, id := range ids {
					result[id] = "In Progress"
				}
				return result, nil
			},
		}
		spy := newCommentAwareMetrics()
		state := exitStateWithIssue(t, issueID, "In Progress")
		params := handoffEvidenceExitParams(t, store, tracker, spy)
		params.CommentsConfig.OnFailure = true

		HandleWorkerExit(state, WorkerResult{
			IssueID:                 issueID,
			Identifier:              issueID + "-ident",
			ExitKind:                WorkerExitNormal,
			WorkspacePath:           dir,
			HandoffEvidencePolicy:   config.HandoffEvidenceObserved,
			HandoffEvidenceBaseline: baseline,
			AgentAdapter:            "mock",
		}, params)
		t.Cleanup(func() { CancelRetry(state, issueID) })
		spy.waitComment(t)

		retry, ok := state.RetryAttempts[issueID]
		if !ok {
			t.Fatal("retry not scheduled")
		}
		if len(tracker.commentCalls) != 1 {
			t.Fatalf("CommentIssue called %d times, want 1", len(tracker.commentCalls))
		}
		wantLine := fmt.Sprintf("Retry: yes (attempt %d)", retry.Attempt)
		if !strings.Contains(tracker.commentCalls[0].Text, wantLine) {
			t.Errorf("comment = %q, want it to contain %q", tracker.commentCalls[0].Text, wantLine)
		}
	})

	t.Run("deferred to a foreign incumbent: positive form with the incumbent's attempt", func(t *testing.T) {
		t.Parallel()

		const issueID = "PROMISE-DEFERRED"
		dir, baseline := handoffEvidenceGitWorkspace(t)
		store := &mockExitStore{}
		tracker := &mockTrackerAdapter{
			fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
				result := make(map[string]string, len(ids))
				for _, id := range ids {
					result[id] = "In Progress"
				}
				return result, nil
			},
		}
		spy := newCommentAwareMetrics()
		state := exitStateWithIssue(t, issueID, "In Progress")
		state.RetryAttempts[issueID] = &RetryEntry{
			IssueID:      issueID,
			Attempt:      5,
			ReactionKind: ReactionKindCI,
		}
		params := handoffEvidenceExitParams(t, store, tracker, spy)
		params.CommentsConfig.OnFailure = true

		HandleWorkerExit(state, WorkerResult{
			IssueID:                 issueID,
			Identifier:              issueID + "-ident",
			ExitKind:                WorkerExitNormal,
			WorkspacePath:           dir,
			HandoffEvidencePolicy:   config.HandoffEvidenceObserved,
			HandoffEvidenceBaseline: baseline,
			AgentAdapter:            "mock",
		}, params)
		t.Cleanup(func() { CancelRetry(state, issueID) })
		spy.waitComment(t)

		incumbent, ok := state.RetryAttempts[issueID]
		if !ok || incumbent.Attempt != 5 {
			t.Fatalf("RetryAttempts[%s] = %+v, want the incumbent preserved at attempt 5", issueID, incumbent)
		}
		if len(tracker.commentCalls) != 1 {
			t.Fatalf("CommentIssue called %d times, want 1", len(tracker.commentCalls))
		}
		wantLine := "Retry: yes (attempt 5)"
		if !strings.Contains(tracker.commentCalls[0].Text, wantLine) {
			t.Errorf("comment = %q, want it to contain %q", tracker.commentCalls[0].Text, wantLine)
		}
	})
}

func TestHandleWorkerExit_SuppressedExitPostsCompletionCommentNotFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		issueID        string
		softStop       bool
		softStopReason string
		wantHeadline   string
	}{
		{
			name:           "soft-stop variant",
			issueID:        "SUP-COMMENT-SOFT",
			softStop:       true,
			softStopReason: "needs-human-review",
			wantHeadline:   "Sortie session completed (agent signaled: needs-human-review).",
		},
		{
			name:         "ordinary completion",
			issueID:      "SUP-COMMENT-COMPLETE",
			wantHeadline: "Sortie session completed.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir, baseline := handoffEvidenceGitWorkspace(t)
			store := &mockExitStore{}
			tracker := &mockTrackerAdapter{
				fetchStatesFn: func(_ context.Context, ids []string) (map[string]string, error) {
					result := make(map[string]string, len(ids))
					for _, id := range ids {
						result[id] = "Done"
					}
					return result, nil
				},
			}
			spy := newCommentAwareMetrics()
			state := exitStateWithIssue(t, tt.issueID, "In Progress")
			params := handoffEvidenceExitParams(t, store, tracker, spy)
			params.CommentsConfig.OnFailure = true
			params.CommentsConfig.OnCompletion = true

			HandleWorkerExit(state, WorkerResult{
				IssueID:                 tt.issueID,
				Identifier:              tt.issueID + "-ident",
				ExitKind:                WorkerExitNormal,
				WorkspacePath:           dir,
				HandoffEvidencePolicy:   config.HandoffEvidenceObserved,
				HandoffEvidenceBaseline: baseline,
				AgentAdapter:            "mock",
				SoftStop:                tt.softStop,
				SoftStopReason:          tt.softStopReason,
			}, params)
			spy.waitComment(t)

			if len(tracker.commentCalls) != 1 {
				t.Fatalf("CommentIssue called %d times, want 1", len(tracker.commentCalls))
			}
			text := tracker.commentCalls[0].Text
			if !strings.Contains(text, tt.wantHeadline) {
				t.Errorf("comment = %q, want headline %q", text, tt.wantHeadline)
			}
			if strings.Contains(text, "Sortie session failed.") {
				t.Errorf("comment contains the failure headline, want none:\n%s", text)
			}
			if strings.Contains(text, "Retry:") {
				t.Errorf("comment contains a retry line, want none:\n%s", text)
			}
			if strings.Contains(text, "re-queuing") {
				t.Errorf("comment contains the re-queuing headline, want none:\n%s", text)
			}

			spy.mu.Lock()
			comments := append([]trackerCommentCall(nil), spy.trackerComments...)
			spy.mu.Unlock()
			if len(comments) != 1 || comments[0].lifecycle != "completion" {
				t.Errorf("IncTrackerComments calls = %+v, want one completion call", comments)
			}
		})
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

	if len(tracker.transitionCalls) != 1 {
		t.Fatalf("TransitionIssue called %d times, want 1", len(tracker.transitionCalls))
	}
	if tracker.transitionCalls[0].IssueID != "HO-1" {
		t.Errorf("TransitionIssue IssueID = %q, want %q", tracker.transitionCalls[0].IssueID, "HO-1")
	}
	if tracker.transitionCalls[0].TargetState != "Human Review" {
		t.Errorf("TransitionIssue TargetState = %q, want %q", tracker.transitionCalls[0].TargetState, "Human Review")
	}

	if _, ok := state.RetryAttempts["HO-1"]; ok {
		t.Error("retry scheduled after successful handoff transition, should not be")
	}

	if _, ok := state.Claimed["HO-1"]; ok {
		t.Error("claim preserved after successful handoff transition, should be released")
	}

	if _, ok := state.Completed["HO-1"]; !ok {
		t.Error("issue not added to Completed set after handoff transition")
	}

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

	if len(tracker.transitionCalls) != 1 {
		t.Fatalf("TransitionIssue called %d times, want 1", len(tracker.transitionCalls))
	}

	retryEntry, ok := state.RetryAttempts["HO-2"]
	if !ok {
		t.Fatal("retry not scheduled after failed handoff transition")
	}
	if retryEntry.Attempt != 1 {
		t.Errorf("retry Attempt = %d, want 1", retryEntry.Attempt)
	}

	if _, ok := state.Claimed["HO-2"]; !ok {
		t.Error("claim released after failed handoff transition, should be preserved")
	}

	if _, ok := state.Completed["HO-2"]; !ok {
		t.Error("issue not added to Completed set after failed handoff")
	}

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

	if len(tracker.transitionCalls) != 0 {
		t.Errorf("TransitionIssue called %d times, want 0", len(tracker.transitionCalls))
	}

	retryEntry, ok := state.RetryAttempts["HO-3"]
	if !ok {
		t.Fatal("retry not scheduled when handoff is not configured")
	}
	if retryEntry.Attempt != 1 {
		t.Errorf("retry Attempt = %d, want 1", retryEntry.Attempt)
	}

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

	if len(tracker.transitionCalls) != 0 {
		t.Errorf("TransitionIssue called %d times, want 0", len(tracker.transitionCalls))
	}

	if _, ok := state.RetryAttempts["HO-4"]; ok {
		t.Error("retry scheduled for non-active issue, should not be")
	}

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

	if _, ok := state.RetryAttempts["HO-5"]; ok {
		t.Error("retry scheduled for non-active issue without handoff, should not be")
	}

	if _, ok := state.Claimed["HO-5"]; ok {
		t.Error("claim preserved for non-active issue, should be released")
	}

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

	if _, ok := state.RetryAttempts["HO-6"]; !ok {
		t.Error("retry not scheduled with empty ActiveStates, backward compat guard failed")
	}

	if _, ok := state.Claimed["HO-6"]; !ok {
		t.Error("claim released with empty ActiveStates, should be preserved")
	}
}

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

	if len(tracker.transitionCalls) != 1 {
		t.Fatalf("TransitionIssue called %d times, want 1", len(tracker.transitionCalls))
	}

	if _, ok := state.Claimed["HO-R1"]; ok {
		t.Error("state.Claimed[HO-R1] present after successful handoff, want released")
	}

	if _, ok := state.RetryAttempts["HO-R1"]; ok {
		t.Error("retry scheduled after successful handoff, want none")
	}

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

	if _, ok := state.RetryAttempts["HO-R2"]; !ok {
		t.Fatal("retry not scheduled after failed handoff transition, want continuation retry")
	}

	if _, ok := state.Claimed["HO-R2"]; !ok {
		t.Error("state.Claimed[HO-R2] absent after failed handoff, want preserved")
	}

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

	if _, ok := state.Claimed["HO-C1"]; ok {
		t.Error("state.Claimed[HO-C1] present after successful handoff, want released")
	}

	if _, ok := state.RetryAttempts["HO-C1"]; ok {
		t.Error("retry scheduled after successful handoff, want none")
	}

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

	if _, ok := state.RetryAttempts["HO-C2"]; !ok {
		t.Fatal("retry not scheduled after failed handoff transition, want continuation retry")
	}

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

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "NOWSP-1",
		Identifier:    "NOWSP-1-ident",
		ExitKind:      WorkerExitCancelled,
		AgentAdapter:  "mock",
		WorkspacePath: "",
	}, params)

	if _, ok := state.Running["NOWSP-1"]; ok {
		t.Error("Running entry not removed")
	}

	if _, err := os.Stat(oldPathDir); err != nil {
		t.Errorf("workspace dir removed despite empty WorkspacePath: %v", err)
	}

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

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "ISSUE-NIL",
		Identifier:    "ISSUE-NIL-ident",
		ExitKind:      WorkerExitNormal,
		SSHHost:       "some-host",
		AgentAdapter:  "mock",
		WorkspacePath: "/tmp/ws",
	}, params)

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

func exitParamsWithComments(t *testing.T, store *mockExitStore, tracker *mockTrackerAdapter, comments config.TrackerCommentsConfig) HandleWorkerExitParams {
	t.Helper()
	p := defaultExitParams(t, store)
	p.TrackerAdapter = tracker
	p.ActiveStates = []string{"In Progress"} // issue state "" is not active → retryScheduled=false on normal exit
	p.CommentsConfig = comments
	return p
}

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

	if len(tracker.commentCalls) != 0 {
		t.Errorf("CommentIssue call count = %d, want 0 (OnCompletion=false)", len(tracker.commentCalls))
	}
}

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

	if len(tracker.commentCalls) != 1 {
		t.Fatalf("CommentIssue call count = %d, want 1", len(tracker.commentCalls))
	}
	if !strings.Contains(tracker.commentCalls[0].Text, "Sortie session failed.") {
		t.Errorf("failure comment missing headline\ngot: %q", tracker.commentCalls[0].Text)
	}
	if !strings.Contains(tracker.commentCalls[0].Text, "ses-cmt3") {
		t.Errorf("failure comment missing session ID\ngot: %q", tracker.commentCalls[0].Text)
	}

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

	if len(tracker.commentCalls) != 0 {
		t.Errorf("CommentIssue call count = %d, want 0 (OnFailure=false)", len(tracker.commentCalls))
	}
}

func TestHandleWorkerExit_NoCommentOnCancelled(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{}
	state := exitState(t, "CMT-5", nil)
	// Both flags enabled, still no comment for cancellation.
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

	if len(tracker.commentCalls) != 0 {
		t.Errorf("CommentIssue call count = %d, want 0 (cancelled exit)", len(tracker.commentCalls))
	}
}

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

	spy.waitComment(t)

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

	logOut := buf.String()
	if !strings.Contains(logOut, "tracker comment failed") {
		t.Errorf("log missing %q\ngot: %s", "tracker comment failed", logOut)
	}
	if !strings.Contains(logOut, "level=WARN") {
		t.Errorf("expected WARN level log\ngot: %s", logOut)
	}
}

func TestHandleWorkerExit_CommentNilTrackerAdapterSafe(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "CMT-7", nil)
	params := defaultExitParams(t, store)
	params.TrackerAdapter = nil // explicit nil
	params.CommentsConfig = config.TrackerCommentsConfig{OnCompletion: true, OnFailure: true}

	HandleWorkerExit(state, WorkerResult{
		IssueID:      "CMT-7",
		Identifier:   "CMT-7-ident",
		ExitKind:     WorkerExitNormal,
		AgentAdapter: "mock",
	}, params)

	if _, ok := state.Running["CMT-7"]; ok {
		t.Error("Running entry not removed after normal exit with nil TrackerAdapter")
	}
}

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
		// TurnsCompleted is zero-value, worker never reached the turn loop.
	}, params)

	if len(store.runHistories) != 1 {
		t.Fatalf("AppendRunHistory called %d times, want 1", len(store.runHistories))
	}
	if store.runHistories[0].TurnsCompleted != 0 {
		t.Errorf("RunHistory.TurnsCompleted = %d, want 0 when no turns ran", store.runHistories[0].TurnsCompleted)
	}
}

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

		if _, ok := state.Claimed["SS-1"]; ok {
			t.Error("claim preserved after soft-stop, want released")
		}

		if _, ok := state.RetryAttempts["SS-1"]; ok {
			t.Error("retry scheduled after soft-stop, want suppressed")
		}

		if _, ok := state.Completed["SS-1"]; !ok {
			t.Error("issue not added to Completed set after soft-stop")
		}

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

		if _, ok := state.RetryAttempts["SS-6"]; ok {
			t.Error("pre-existing RetryAttempts entry not removed by CancelRetry on soft-stop")
		}

		// CancelRetry must have stopped the timer, not only deleted the map entry.
		// Stop() returns false when the timer was already stopped; true means it
		// was still live, a bug where CancelRetry skipped the Stop() call.
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

		if len(tracker.transitionCalls) != 0 {
			t.Errorf("TransitionIssue called %d times, want 0 (handoff must be skipped when SoftStop is true)",
				len(tracker.transitionCalls))
		}

		if _, ok := state.Claimed["SS-7"]; ok {
			t.Error("claim preserved after soft-stop, want released")
		}

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

func TestHandleWorkerExitBlockedWithReviewMetadataTakesBlockedDisposition(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{}
	spy := &spyMetrics{}
	state := exitStateWithIssue(t, "BLK-RM", "In Progress")
	params := handoffEvidenceExitParams(t, store, tracker, spy)
	params.HandoffParkingLabel = "needs-human"

	HandleWorkerExit(state, WorkerResult{
		IssueID:        "BLK-RM",
		Identifier:     "BLK-RM-ident",
		ExitKind:       WorkerExitNormal,
		AgentAdapter:   "mock",
		SoftStop:       true,
		SoftStopReason: "blocked",
		ReviewMetadata: &domain.ReviewMetadata{FinalVerdict: "iterate"},
	}, params)
	state.TrackerOpsWg.Wait()

	if len(tracker.transitionCalls) != 0 {
		t.Errorf("TransitionIssue called %d times, want 0", len(tracker.transitionCalls))
	}
	if _, ok := state.RetryAttempts["BLK-RM"]; ok {
		t.Error("retry entry remains after parking")
	}
	if _, ok := state.Claimed["BLK-RM"]; ok {
		t.Error("claim remains after parking")
	}
	entry, ok := state.Parked["BLK-RM"]
	if !ok {
		t.Fatal("issue not parked after a blocked soft stop with review metadata")
	}
	if entry.Reason != parkReasonAgentBlocked {
		t.Errorf("Parked[BLK-RM].Reason = %q, want %q", entry.Reason, parkReasonAgentBlocked)
	}
	if len(store.parkedIssues) != 1 || store.parkedIssues[0].IssueID != "BLK-RM" {
		t.Errorf("UpsertParkedIssue calls = %+v, want one call for BLK-RM", store.parkedIssues)
	}
	if len(store.runHistories) != 1 {
		t.Fatalf("AppendRunHistory calls = %d, want 1", len(store.runHistories))
	}
	if got := store.runHistories[0].Status; got != "succeeded" {
		t.Errorf("RunHistory.Status = %q, want succeeded", got)
	}
	if store.runHistories[0].Error != nil {
		t.Errorf("RunHistory.Error = %v, want nil", store.runHistories[0].Error)
	}
	if len(spy.handoffTransitions) != 0 {
		t.Errorf("handoffTransitions = %v, want none", spy.handoffTransitions)
	}
	if len(store.absenceCountedIssueIDs) != 0 {
		t.Errorf("QueryConsecutiveHandoffAbsenceCounts calls = %v, want none", store.absenceCountedIssueIDs)
	}
	if len(store.absenceResetOf) != 0 {
		t.Errorf("ResetHandoffAbsenceSequence calls = %v, want none", store.absenceResetOf)
	}
}

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

type ciProviderStubExit struct{}

func (c *ciProviderStubExit) FetchCIStatus(_ context.Context, _ string) (domain.CIResult, error) {
	panic("FetchCIStatus must not be called by HandleWorkerExit")
}

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

	select {
	case <-waitDone:
		t.Fatal("TrackerOpsWg.Wait() returned before CommentIssue goroutine completed")
	case <-time.After(20 * time.Millisecond):
	}

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

// TestHandleWorkerExit_LabelReviewEnqueue covers the label-review seeding
// block. The fixture carries a branch though the clause imposes no branch
// requirement, reflecting the common production case rather than the
// reader's need.
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

// TestHandleWorkerExit_LabelFixEnqueue covers the label-fix seeding
// block. Unlike label-review, an empty branch skips the entry.
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

// TestHandleWorkerExit_MergeCompletionEnqueue covers the merge-completion
// seeding block. The clause imposes no branch requirement, unlike the
// checkout-bearing kinds; the fixtures carry one only as the common case.
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

// TestHandleWorkerExit_HandoffFailureDeferral pins that the nil-adapter
// and transition-failure handoff branches defer to a foreign incumbent
// instead of scheduling their own retry.
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

// TestHandleWorkerExit_StopSignalDispositionsDestroyForeignIncumbent pins
// the six stop-signal dispositions that cancel a foreign incumbent
// regardless: blocked soft stop, terminal observation, other soft stop,
// and the three handoff soft-stop variants.
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

// TestHandleWorkerExit_SuccessfulHandoffPreservesIncumbent pins that a
// successful handoff preserves a foreign incumbent, keeps the claim held,
// and lets the incumbent's own timer dispatch from the handoff state.
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

// TestHandleWorkerExit_LabelReviewExitDoesNotWidenReactionSeeding pins
// that retaining the claim to protect an incumbent does not widen which
// reactions a read-only label-review exit seeds. The only variable
// between subtests is whether the retry slot is occupied.
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

// TestHandleWorkerExit_CompletionSignalAfterSelfReview pins that a run
// whose self-review phase ran takes the same disposition it would
// without it: HandleWorkerExit reads only SoftStop and SoftStopReason, so
// the phase must not change the outcome.
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

func TestResolveExitTarget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		params       HandleWorkerExitParams
		result       WorkerResult
		wantTarget   string
		wantDeclared bool
	}{
		{
			name:       "undeclared run falls back to handoff_state",
			params:     HandleWorkerExitParams{HandoffState: "Human Review", NoChangeState: "Done"},
			result:     WorkerResult{},
			wantTarget: "Human Review",
		},
		{
			name:         "declared run with no_change_state set resolves to it",
			params:       HandleWorkerExitParams{HandoffState: "Human Review", NoChangeState: "Done"},
			result:       WorkerResult{SoftStop: true, SoftStopReason: "no-change-needed"},
			wantTarget:   "Done",
			wantDeclared: true,
		},
		{
			name:         "declared run with no_change_state unset falls back to handoff_state",
			params:       HandleWorkerExitParams{HandoffState: "Human Review"},
			result:       WorkerResult{SoftStop: true, SoftStopReason: "no-change-needed"},
			wantTarget:   "Human Review",
			wantDeclared: true,
		},
		{
			name:       "a different soft-stop reason is not a declaration",
			params:     HandleWorkerExitParams{HandoffState: "Human Review", NoChangeState: "Done"},
			result:     WorkerResult{SoftStop: true, SoftStopReason: "needs-human-review"},
			wantTarget: "Human Review",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			target, declared := resolveExitTarget(tt.params, tt.result)
			if target != tt.wantTarget {
				t.Errorf("resolveExitTarget() target = %q, want %q", target, tt.wantTarget)
			}
			if declared != tt.wantDeclared {
				t.Errorf("resolveExitTarget() declared = %v, want %v", declared, tt.wantDeclared)
			}
		})
	}
}

func TestHandleWorkerExit_DeclaredRunReachesConfiguredNoChangeState(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{}
	spy := &spyMetrics{}
	state := exitStateWithIssue(t, "NC-1", "In Progress")
	params := defaultExitParams(t, store)
	params.TrackerAdapter = tracker
	params.HandoffState = "Human Review"
	params.NoChangeState = "Done"
	params.ActiveStates = []string{"In Progress"}
	params.TerminalStates = []string{"Done"}
	params.Metrics = spy

	HandleWorkerExit(state, WorkerResult{
		IssueID:        "NC-1",
		Identifier:     "NC-1-ident",
		ExitKind:       WorkerExitNormal,
		AgentAdapter:   "mock",
		SoftStop:       true,
		SoftStopReason: "no-change-needed",
	}, params)

	if len(tracker.transitionCalls) != 1 || tracker.transitionCalls[0].TargetState != "Done" {
		t.Fatalf("TransitionIssue calls = %+v, want one transition to %q", tracker.transitionCalls, "Done")
	}
	if len(store.runHistories) != 1 {
		t.Fatalf("AppendRunHistory calls = %d, want 1", len(store.runHistories))
	}
	if got := store.runHistories[0].Status; got != "succeeded" {
		t.Errorf("RunHistory.Status = %q, want %q", got, "succeeded")
	}
	if store.runHistories[0].Error != nil {
		t.Errorf("RunHistory.Error = %v, want nil", store.runHistories[0].Error)
	}
	if len(store.absenceCountedIssueIDs) != 0 {
		t.Errorf("QueryConsecutiveHandoffAbsenceCounts issue IDs = %v, want none", store.absenceCountedIssueIDs)
	}
	if _, ok := state.RetryAttempts["NC-1"]; ok {
		t.Error("RetryAttempts entry present, want none (no retry scheduled)")
	}
	if len(store.retryEntries) != 0 {
		t.Errorf("SaveRetryEntry calls = %d, want 0", len(store.retryEntries))
	}
	if len(tracker.commentCalls) != 0 {
		t.Errorf("CommentIssue calls = %+v, want 0 (no comment configured)", tracker.commentCalls)
	}
	if len(spy.handoffTransitions) != 1 || spy.handoffTransitions[0] != handoffSuccess {
		t.Errorf("handoffTransitions = %v, want [%s]", spy.handoffTransitions, handoffSuccess)
	}
}

func TestHandleWorkerExit_DeclaredRunCompletionComment(t *testing.T) {
	t.Parallel()

	t.Run("on_completion enabled posts the comment", func(t *testing.T) {
		t.Parallel()
		store := &mockExitStore{}
		tracker := &mockTrackerAdapter{}
		state := exitStateWithIssue(t, "NC-CMT-ON", "In Progress")
		params := defaultExitParams(t, store)
		params.TrackerAdapter = tracker
		params.HandoffState = "Human Review"
		params.NoChangeState = "Done"
		params.ActiveStates = []string{"In Progress"}
		params.CommentsConfig.OnCompletion = true

		HandleWorkerExit(state, WorkerResult{
			IssueID:        "NC-CMT-ON",
			Identifier:     "ident",
			ExitKind:       WorkerExitNormal,
			AgentAdapter:   "mock",
			SoftStop:       true,
			SoftStopReason: "no-change-needed",
		}, params)
		state.TrackerOpsWg.Wait()

		if len(tracker.commentCalls) != 1 {
			t.Fatalf("CommentIssue calls = %d, want 1", len(tracker.commentCalls))
		}
		if !strings.Contains(tracker.commentCalls[0].Text, "no-change-needed") {
			t.Errorf("comment text = %q, want it to name the declaration", tracker.commentCalls[0].Text)
		}
	})

	t.Run("on_completion disabled posts nothing", func(t *testing.T) {
		t.Parallel()
		store := &mockExitStore{}
		tracker := &mockTrackerAdapter{}
		state := exitStateWithIssue(t, "NC-CMT-OFF", "In Progress")
		params := defaultExitParams(t, store)
		params.TrackerAdapter = tracker
		params.HandoffState = "Human Review"
		params.NoChangeState = "Done"
		params.ActiveStates = []string{"In Progress"}

		HandleWorkerExit(state, WorkerResult{
			IssueID:        "NC-CMT-OFF",
			Identifier:     "ident",
			ExitKind:       WorkerExitNormal,
			AgentAdapter:   "mock",
			SoftStop:       true,
			SoftStopReason: "no-change-needed",
		}, params)

		if len(tracker.commentCalls) != 0 {
			t.Errorf("CommentIssue calls = %+v, want 0", tracker.commentCalls)
		}
	})
}

func TestHandleWorkerExit_NoChangeStateUnsetMatchesPriorBehavior(t *testing.T) {
	t.Parallel()

	t.Run("permitted handoff unaffected", func(t *testing.T) {
		t.Parallel()
		store := &mockExitStore{}
		tracker := &mockTrackerAdapter{}
		state := exitStateWithIssue(t, "PRE-PERMIT", "In Progress")
		params := defaultExitParams(t, store)
		params.TrackerAdapter = tracker
		params.HandoffState = "Human Review"
		params.ActiveStates = []string{"In Progress"}

		HandleWorkerExit(state, WorkerResult{
			IssueID:      "PRE-PERMIT",
			Identifier:   "ident",
			ExitKind:     WorkerExitNormal,
			AgentAdapter: "mock",
		}, params)

		if len(tracker.transitionCalls) != 1 || tracker.transitionCalls[0].TargetState != "Human Review" {
			t.Errorf("TransitionIssue calls = %+v, want one transition to %q", tracker.transitionCalls, "Human Review")
		}
	})

	t.Run("withheld absence unaffected", func(t *testing.T) {
		t.Parallel()
		dir, baseline := handoffEvidenceGitWorkspace(t)
		store := &mockExitStore{}
		tracker := &mockTrackerAdapter{}
		state := exitStateWithIssue(t, "PRE-WITHHOLD", "In Progress")
		params := handoffEvidenceExitParams(t, store, tracker, &spyMetrics{})

		HandleWorkerExit(state, WorkerResult{
			IssueID:                 "PRE-WITHHOLD",
			Identifier:              "ident",
			ExitKind:                WorkerExitNormal,
			AgentAdapter:            "mock",
			WorkspacePath:           dir,
			HandoffEvidencePolicy:   config.HandoffEvidenceObserved,
			HandoffEvidenceBaseline: baseline,
		}, params)
		t.Cleanup(func() { CancelRetry(state, "PRE-WITHHOLD") })

		if len(tracker.transitionCalls) != 0 {
			t.Errorf("TransitionIssue calls = %d, want 0", len(tracker.transitionCalls))
		}
		if store.runHistories[0].Status != "failed" {
			t.Errorf("RunHistory.Status = %q, want %q", store.runHistories[0].Status, "failed")
		}
	})

	t.Run("terminal observation unaffected", func(t *testing.T) {
		t.Parallel()
		store := &mockExitStore{}
		tracker := &mockTrackerAdapter{}
		spy := &spyMetrics{}
		state := exitStateWithIssue(t, "PRE-TERM", "Done")
		params := defaultExitParams(t, store)
		params.TrackerAdapter = tracker
		params.HandoffState = "Human Review"
		params.TerminalStates = []string{"Done"}
		params.Metrics = spy

		HandleWorkerExit(state, WorkerResult{
			IssueID:      "PRE-TERM",
			Identifier:   "ident",
			ExitKind:     WorkerExitNormal,
			AgentAdapter: "mock",
		}, params)

		if len(tracker.transitionCalls) != 0 {
			t.Errorf("TransitionIssue calls = %d, want 0", len(tracker.transitionCalls))
		}
		if len(spy.handoffTransitions) != 1 || spy.handoffTransitions[0] != handoffSkipped {
			t.Errorf("handoffTransitions = %v, want [%s]", spy.handoffTransitions, handoffSkipped)
		}
	})
}

func TestHandleWorkerExit_DeclaredRunReleasesAbsencePark(t *testing.T) {
	t.Parallel()

	t.Run("observed releases the park and resets the sequence", func(t *testing.T) {
		t.Parallel()
		store := &mockExitStore{}
		tracker := &mockTrackerAdapter{}
		state := exitStateWithIssue(t, "NC-PARK-OBS", "In Progress")
		state.Parked["NC-PARK-OBS"] = &ParkedEntry{Reason: parkReasonHandoffAbsence, Identifier: "NC-PARK-OBS-ident"}
		params := defaultExitParams(t, store)
		params.TrackerAdapter = tracker
		params.HandoffState = "Human Review"
		params.ActiveStates = []string{"In Progress"}

		HandleWorkerExit(state, WorkerResult{
			IssueID:               "NC-PARK-OBS",
			Identifier:            "NC-PARK-OBS-ident",
			ExitKind:              WorkerExitNormal,
			AgentAdapter:          "mock",
			SoftStop:              true,
			SoftStopReason:        "no-change-needed",
			HandoffEvidencePolicy: config.HandoffEvidenceObserved,
		}, params)

		if _, ok := state.Parked["NC-PARK-OBS"]; ok {
			t.Error("issue still parked after a declared run under observed, want released")
		}
		if len(store.deletedParkedIDs) != 1 {
			t.Errorf("DeleteParkedIssue calls = %v, want one call", store.deletedParkedIDs)
		}
		// Once from unparkIssue and once from the general work-observed
		// reset below the run-history write; both name this issue.
		if len(store.absenceResetOf) != 2 {
			t.Errorf("ResetHandoffAbsenceSequence calls = %v, want two calls naming %q", store.absenceResetOf, "NC-PARK-OBS")
		}
	})

	t.Run("off leaves the park untouched", func(t *testing.T) {
		t.Parallel()
		store := &mockExitStore{}
		tracker := &mockTrackerAdapter{}
		state := exitStateWithIssue(t, "NC-PARK-OFF", "In Progress")
		state.Parked["NC-PARK-OFF"] = &ParkedEntry{Reason: parkReasonHandoffAbsence, Identifier: "NC-PARK-OFF-ident"}
		params := defaultExitParams(t, store)
		params.TrackerAdapter = tracker
		params.HandoffState = "Human Review"
		params.ActiveStates = []string{"In Progress"}

		HandleWorkerExit(state, WorkerResult{
			IssueID:               "NC-PARK-OFF",
			Identifier:            "NC-PARK-OFF-ident",
			ExitKind:              WorkerExitNormal,
			AgentAdapter:          "mock",
			SoftStop:              true,
			SoftStopReason:        "no-change-needed",
			HandoffEvidencePolicy: config.HandoffEvidenceOff,
		}, params)

		if _, ok := state.Parked["NC-PARK-OFF"]; !ok {
			t.Error("park released under off, want it to remain (no verdict is computed)")
		}
		if len(store.deletedParkedIDs) != 0 {
			t.Errorf("DeleteParkedIssue calls = %v, want none under off", store.deletedParkedIDs)
		}
	})
}

func TestHandleWorkerExit_DeclaredRunTargetAcrossPolicies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		noChangeState string
		policy        config.HandoffEvidencePolicy
		wantTarget    string
	}{
		{"set observed", "Done", config.HandoffEvidenceObserved, "Done"},
		{"set strict", "Done", config.HandoffEvidenceStrict, "Done"},
		{"set off", "Done", config.HandoffEvidenceOff, "Done"},
		{"unset observed", "", config.HandoffEvidenceObserved, "Human Review"},
		{"unset strict", "", config.HandoffEvidenceStrict, "Human Review"},
		{"unset off", "", config.HandoffEvidenceOff, "Human Review"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			const issueID = "MATRIX"
			store := &mockExitStore{}
			tracker := &mockTrackerAdapter{}
			state := exitStateWithIssue(t, issueID, "In Progress")
			params := defaultExitParams(t, store)
			params.TrackerAdapter = tracker
			params.HandoffState = "Human Review"
			params.NoChangeState = tt.noChangeState
			params.ActiveStates = []string{"In Progress"}
			params.TerminalStates = []string{"Done"}

			HandleWorkerExit(state, WorkerResult{
				IssueID:               issueID,
				Identifier:            "ident",
				ExitKind:              WorkerExitNormal,
				AgentAdapter:          "mock",
				SoftStop:              true,
				SoftStopReason:        "no-change-needed",
				HandoffEvidencePolicy: tt.policy,
			}, params)

			if len(tracker.transitionCalls) != 1 || tracker.transitionCalls[0].TargetState != tt.wantTarget {
				t.Errorf("TransitionIssue calls = %+v, want one transition to %q", tracker.transitionCalls, tt.wantTarget)
			}
		})
	}
}

func TestHandleWorkerExit_DeclaredRunUndeterminableEvidenceProceedsUnderStrict(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{}
	state := exitStateWithIssue(t, "NC-STRICT-UNDET", "In Progress")
	params := defaultExitParams(t, store)
	params.TrackerAdapter = tracker
	params.HandoffState = "Human Review"
	params.ActiveStates = []string{"In Progress"}

	HandleWorkerExit(state, WorkerResult{
		IssueID:                      "NC-STRICT-UNDET",
		Identifier:                   "ident",
		ExitKind:                     WorkerExitNormal,
		AgentAdapter:                 "mock",
		SoftStop:                     true,
		SoftStopReason:               "no-change-needed",
		HandoffEvidencePolicy:        config.HandoffEvidenceStrict,
		HandoffEvidenceBaselineError: errors.New("simulated undeterminable baseline"),
	}, params)

	if len(tracker.transitionCalls) != 1 {
		t.Fatalf("TransitionIssue calls = %d, want 1 (a declared run proceeds under strict)", len(tracker.transitionCalls))
	}
	if len(store.absenceCountedIssueIDs) != 0 {
		t.Errorf("QueryConsecutiveHandoffAbsenceCounts issue IDs = %v, want none", store.absenceCountedIssueIDs)
	}
	if store.runHistories[0].Status != "succeeded" {
		t.Errorf("RunHistory.Status = %q, want %q", store.runHistories[0].Status, "succeeded")
	}
}

func TestHandleWorkerExit_TerminalObservationSuppressesRegardlessOfDeclaration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		softStop       bool
		softStopReason string
	}{
		{"undeclared", false, ""},
		{"declared", true, "no-change-needed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issueID := "TERM-" + tt.name
			store := &mockExitStore{}
			tracker := &mockTrackerAdapter{}
			spy := &spyMetrics{}
			state := exitStateWithIssue(t, issueID, "Done")
			params := defaultExitParams(t, store)
			params.TrackerAdapter = tracker
			params.HandoffState = "Human Review"
			params.TerminalStates = []string{"Done"}
			params.Metrics = spy

			HandleWorkerExit(state, WorkerResult{
				IssueID:        issueID,
				Identifier:     "ident",
				ExitKind:       WorkerExitNormal,
				AgentAdapter:   "mock",
				SoftStop:       tt.softStop,
				SoftStopReason: tt.softStopReason,
			}, params)

			if len(tracker.transitionCalls) != 0 {
				t.Errorf("TransitionIssue calls = %d, want 0", len(tracker.transitionCalls))
			}
			if len(spy.handoffTransitions) != 1 || spy.handoffTransitions[0] != handoffSkipped {
				t.Errorf("handoffTransitions = %v, want [%s]", spy.handoffTransitions, handoffSkipped)
			}
			if _, ok := state.Claimed[issueID]; ok {
				t.Error("claim remains after a terminal observation, want released")
			}
		})
	}
}

func TestDrainRunningWorkers_NoChangeState(t *testing.T) {
	t.Parallel()

	const issueID = "DRAIN-NC"
	state := NewState(60000, 1, 0, nil, AgentTotals{})
	state.Running[issueID] = &RunningEntry{
		Identifier: "PROJ-DRAIN-NC",
		Issue:      domain.Issue{ID: issueID, Identifier: "PROJ-DRAIN-NC", State: "In Progress"},
		StartedAt:  time.Now().UTC(),
		CancelFunc: func() {},
	}
	store := &stubStore{}
	wm := budgetTickConfig(0)
	wm.config.Tracker.ActiveStates = []string{"In Progress"}
	wm.config.Tracker.TerminalStates = []string{"Done"}
	wm.config.Tracker.HandoffState = "Human Review"
	wm.config.Tracker.NoChangeState = "Done"
	tracker := &candidateTrackerAdapter{mockTrackerAdapter: &mockTrackerAdapter{}}
	o := budgetOrchestrator(state, wm, store, tracker)
	o.drainTimeout = 5 * time.Second

	done := make(chan struct{})
	go func() {
		o.drainRunningWorkers()
		close(done)
	}()

	o.workerExitCh <- WorkerResult{
		IssueID:        issueID,
		Identifier:     "PROJ-DRAIN-NC",
		ExitKind:       WorkerExitNormal,
		AgentAdapter:   "mock",
		SoftStop:       true,
		SoftStopReason: "no-change-needed",
	}

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("drainRunningWorkers did not return within 10 seconds")
	}
	state.TrackerOpsWg.Wait()

	if len(tracker.transitionCalls) != 1 || tracker.transitionCalls[0].TargetState != "Done" {
		t.Errorf("TransitionIssue calls = %+v, want one transition to %q", tracker.transitionCalls, "Done")
	}
}

func TestHandleWorkerExit_NoChangeStateLiveReload(t *testing.T) {
	t.Parallel()

	cfg := lifecycleConfig(t.TempDir())
	cfg.Tracker.HandoffState = "Human Review"
	cfg.Tracker.ActiveStates = []string{"In Progress"}
	cfg.Tracker.TerminalStates = []string{"Done"}
	wm := &stubWorkflowManager{config: cfg}

	store := &mockExitStore{}

	exitWithLiveConfig := func(issueID string, tracker *mockTrackerAdapter) {
		state := exitStateWithIssue(t, issueID, "In Progress")
		params := defaultExitParams(t, store)
		params.TrackerAdapter = tracker
		params.HandoffState = wm.Config().Tracker.HandoffState
		params.NoChangeState = wm.Config().Tracker.NoChangeState
		params.ActiveStates = wm.Config().Tracker.ActiveStates
		params.TerminalStates = wm.Config().Tracker.TerminalStates

		HandleWorkerExit(state, WorkerResult{
			IssueID:        issueID,
			Identifier:     issueID + "-ident",
			ExitKind:       WorkerExitNormal,
			AgentAdapter:   "mock",
			SoftStop:       true,
			SoftStopReason: "no-change-needed",
		}, params)
	}

	trackerBefore := &mockTrackerAdapter{}
	exitWithLiveConfig("NC-RELOAD-1", trackerBefore)
	if len(trackerBefore.transitionCalls) != 1 || trackerBefore.transitionCalls[0].TargetState != "Human Review" {
		t.Fatalf("first exit TransitionIssue calls = %+v, want one transition to %q (no_change_state unset)", trackerBefore.transitionCalls, "Human Review")
	}

	cfg.Tracker.NoChangeState = "Done"
	wm.setConfig(cfg)

	trackerAfter := &mockTrackerAdapter{}
	exitWithLiveConfig("NC-RELOAD-2", trackerAfter)
	if len(trackerAfter.transitionCalls) != 1 || trackerAfter.transitionCalls[0].TargetState != "Done" {
		t.Errorf("second exit TransitionIssue calls = %+v, want one transition to %q (the reload took effect with no restart)", trackerAfter.transitionCalls, "Done")
	}
}

func TestHandleWorkerExit_HandoffLogRecordCarriesTargetStateAttrs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		softStop       bool
		softStopReason string
		noChangeState  string
		wantTarget     string
		wantDeclared   string
	}{
		{"ordinary handoff", false, "", "", `target_state="Human Review"`, "no_change_declared=false"},
		{"declared handoff", true, "no-change-needed", "Done", "target_state=Done", "no_change_declared=true"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			issueID := "LOGATTR-" + tt.name
			store := &mockExitStore{}
			tracker := &mockTrackerAdapter{}
			state := exitStateWithIssue(t, issueID, "In Progress")
			params := defaultExitParams(t, store)
			params.TrackerAdapter = tracker
			params.HandoffState = "Human Review"
			params.NoChangeState = tt.noChangeState
			params.ActiveStates = []string{"In Progress"}
			var logs bytes.Buffer
			params.Logger = debugLogger(t, &logs)

			HandleWorkerExit(state, WorkerResult{
				IssueID:        issueID,
				Identifier:     "ident",
				ExitKind:       WorkerExitNormal,
				AgentAdapter:   "mock",
				SoftStop:       tt.softStop,
				SoftStopReason: tt.softStopReason,
			}, params)

			output := logs.String()
			if !strings.Contains(output, tt.wantTarget) {
				t.Errorf("log output missing %s\ngot: %s", tt.wantTarget, output)
			}
			if !strings.Contains(output, tt.wantDeclared) {
				t.Errorf("log output missing %s\ngot: %s", tt.wantDeclared, output)
			}
		})
	}
}

func TestHandleWorkerExit_DeclaredRunLabelReviewPostureNoWarning(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	tracker := &mockTrackerAdapter{}
	state := exitStateWithIssue(t, "LR-NC", "In Progress")
	state.Running["LR-NC"].ReactionKind = ReactionKindLabelReview
	params := defaultExitParams(t, store)
	params.TrackerAdapter = tracker
	params.HandoffState = "Human Review"
	params.ActiveStates = []string{"In Progress"}
	var logs bytes.Buffer
	params.Logger = debugLogger(t, &logs)

	HandleWorkerExit(state, WorkerResult{
		IssueID:        "LR-NC",
		Identifier:     "ident",
		ExitKind:       WorkerExitNormal,
		AgentAdapter:   "mock",
		SoftStop:       true,
		SoftStopReason: "no-change-needed",
	}, params)

	if len(tracker.transitionCalls) != 0 {
		t.Errorf("TransitionIssue calls = %d, want 0 (label-review does not drive issue state)", len(tracker.transitionCalls))
	}
	if _, ok := state.RetryAttempts["LR-NC"]; ok {
		t.Error("retry scheduled for a declared run on a label-review posture, want none")
	}
	if _, ok := state.Claimed["LR-NC"]; ok {
		t.Error("claim retained after a declared run on a label-review posture, want released")
	}
	if strings.Contains(logs.String(), "unrecognized soft-stop reason") {
		t.Errorf("log output contains the unrecognized-reason warning for a value the orchestrator itself instructs the agent to write\ngot: %s", logs.String())
	}
}

func TestHandleWorkerExit_DeclaredRunSeedsReactionsReleasedOnTerminalReconcile(t *testing.T) {
	t.Parallel()

	const issueID = "NC-SEED"
	wsPath := t.TempDir()
	writePRSCMMetadata(t, wsPath, 77, "corp", "api", "feature/NC-SEED", "c0ffee")

	store := &mockExitStore{}
	state := exitState(t, issueID, nil)
	params := defaultExitParams(t, store)
	params.TrackerAdapter = &mockTrackerAdapter{}
	params.HandoffState = "Human Review"
	params.NoChangeState = "Done"
	params.TerminalStates = []string{"Done"}
	params.SCMAdapter = &scmAdapterStubExit{}
	params.AutoMergeReactionConfigured = true

	HandleWorkerExit(state, WorkerResult{
		IssueID:        issueID,
		Identifier:     issueID + "-ident",
		ExitKind:       WorkerExitNormal,
		AgentAdapter:   "mock",
		SoftStop:       true,
		SoftStopReason: "no-change-needed",
		WorkspacePath:  wsPath,
	}, params)

	rkey := ReactionKey(issueID, ReactionKindAutoMerge)
	if _, ok := state.PendingReactions[rkey]; !ok {
		t.Fatal("PendingReactions missing after a declared run with PR metadata reaching a terminal target")
	}

	releaseStore := &mockReconcileStore{}
	releaseTerminalIssueState(context.Background(), state, releaseStore, issueID, discardLogger())

	if _, ok := state.PendingReactions[rkey]; ok {
		t.Error("PendingReactions present after the terminal-issue release, want removed")
	}
}

func TestHandleWorkerExit_RequestVerdictUsesWorkerTurnTally(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "ISSUE-REQV3", nil)
	entry := state.Running["ISSUE-REQV3"]
	entry.UsageArrival = registry.UsageArrivalIncremental
	entry.TurnCount = 0
	entry.APIRequestCount = 0

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "ISSUE-REQV3",
		Identifier:    "ISSUE-REQV3-ident",
		ExitKind:      WorkerExitNormal,
		AgentAdapter:  "mock",
		WorkspacePath: "/tmp/ws",
		// The turn began and then failed, so nothing completed.
		TurnsCompleted: 0,
		TurnsStarted:   1,
	}, defaultExitParams(t, store))

	if len(store.sessionMetadata) != 1 {
		t.Fatalf("UpsertSessionMetadata called %d times, want 1", len(store.sessionMetadata))
	}
	if store.sessionMetadata[0].APIRequestsMeasured {
		t.Error("SessionMetadata.APIRequestsMeasured = true, want false (the worker ran a turn and no request was counted)")
	}
}

func TestHandleWorkerExit_SessionMetadataRequestVerdict(t *testing.T) {
	t.Parallel()

	t.Run("unmeasured entry: persisted count is zero despite a non-zero raw count", func(t *testing.T) {
		t.Parallel()

		store := &mockExitStore{}
		state := exitState(t, "ISSUE-REQV1", nil)
		entry := state.Running["ISSUE-REQV1"]
		// turn_end never reports during the turn, so the verdict is
		// false regardless of the raw count; a non-zero raw count here
		// models a runtime that contradicted its own declaration.
		entry.UsageArrival = registry.UsageArrivalTurnEnd
		entry.TurnCount = 3
		entry.APIRequestCount = 11

		HandleWorkerExit(state, WorkerResult{
			IssueID:       "ISSUE-REQV1",
			Identifier:    "ISSUE-REQV1-ident",
			ExitKind:      WorkerExitNormal,
			AgentAdapter:  "mock",
			WorkspacePath: "/tmp/ws",
		}, defaultExitParams(t, store))

		if len(store.sessionMetadata) != 1 {
			t.Fatalf("UpsertSessionMetadata called %d times, want 1", len(store.sessionMetadata))
		}
		meta := store.sessionMetadata[0]
		if meta.APIRequestsMeasured {
			t.Fatal("SessionMetadata.APIRequestsMeasured = true, want false (turns began, no request arrived)")
		}
		if meta.APIRequestCount != 0 {
			t.Errorf("SessionMetadata.APIRequestCount = %d, want 0 for an unmeasured row", meta.APIRequestCount)
		}
	})

	t.Run("measured entry: persisted count is the raw count", func(t *testing.T) {
		t.Parallel()

		store := &mockExitStore{}
		state := exitState(t, "ISSUE-REQV2", nil)
		entry := state.Running["ISSUE-REQV2"]
		entry.UsageArrival = registry.UsageArrivalIncremental
		entry.TurnCount = 1
		entry.APIRequestCount = 6

		HandleWorkerExit(state, WorkerResult{
			IssueID:       "ISSUE-REQV2",
			Identifier:    "ISSUE-REQV2-ident",
			ExitKind:      WorkerExitNormal,
			AgentAdapter:  "mock",
			WorkspacePath: "/tmp/ws",
		}, defaultExitParams(t, store))

		if len(store.sessionMetadata) != 1 {
			t.Fatalf("UpsertSessionMetadata called %d times, want 1", len(store.sessionMetadata))
		}
		meta := store.sessionMetadata[0]
		if !meta.APIRequestsMeasured {
			t.Fatal("SessionMetadata.APIRequestsMeasured = false, want true (a request arrived)")
		}
		if meta.APIRequestCount != 6 {
			t.Errorf("SessionMetadata.APIRequestCount = %d, want 6 for a measured row", meta.APIRequestCount)
		}
	})
}

func TestHandleWorkerExit_TurnEndPairPersistedRow(t *testing.T) {
	t.Parallel()

	const (
		issueID = "ISSUE-TE1"
		model   = "claude-sonnet-5"
	)
	s1 := domain.TokenUsage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120, CacheReadTokens: 5}
	s2 := domain.TokenUsage{InputTokens: 250, OutputTokens: 55, TotalTokens: 305, CacheReadTokens: 12}
	ts := time.Now().UTC()

	state := exitState(t, issueID, nil)
	entry := state.Running[issueID]
	entry.UsageArrival = registry.UsageArrivalTurnEnd
	entry.UsageAttribution = registry.UsageAttributionPerModel

	for _, ev := range []domain.AgentEvent{
		{Type: domain.EventTokenUsage, Timestamp: ts, Model: model, Usage: s1},
		{Type: domain.EventTurnCompleted, Timestamp: ts, Usage: s1},
		{Type: domain.EventTokenUsage, Timestamp: ts, Model: model, Usage: s2},
		{Type: domain.EventTurnCompleted, Timestamp: ts, Usage: s2},
	} {
		HandleAgentEvent(state, issueID, ev, discardLogger(), nil)
	}

	if entry.ModelName != model {
		t.Errorf("entry.ModelName = %q, want %q", entry.ModelName, model)
	}
	if got := entry.RequestsByModel[model]; got != 2 {
		t.Errorf("entry.RequestsByModel[%q] = %d, want 2", model, got)
	}
	if entry.AgentTotalTokens != s2.TotalTokens {
		t.Fatalf("entry.AgentTotalTokens = %d, want %d", entry.AgentTotalTokens, s2.TotalTokens)
	}

	snap := RuntimeSnapshot(state, ts)
	if len(snap.Running) != 1 {
		t.Fatalf("RuntimeSnapshot.Running = %d entries, want 1", len(snap.Running))
	}
	runningSnap := snap.Running[0]
	if runningSnap.ModelName != model {
		t.Errorf("RuntimeSnapshot ModelName = %q, want %q", runningSnap.ModelName, model)
	}
	if runningSnap.UsageAttribution != registry.UsageAttributionPerModel {
		t.Errorf("RuntimeSnapshot UsageAttribution = %q, want %q", runningSnap.UsageAttribution, registry.UsageAttributionPerModel)
	}
	if runningSnap.RequestsByModel != nil {
		t.Errorf("RuntimeSnapshot RequestsByModel = %v, want nil (turn_end never reports during the turn)", runningSnap.RequestsByModel)
	}

	store := &mockExitStore{}
	HandleWorkerExit(state, WorkerResult{
		IssueID:        issueID,
		Identifier:     issueID + "-ident",
		ExitKind:       WorkerExitNormal,
		AgentAdapter:   "copilot-cli",
		WorkspacePath:  "/tmp/ws",
		Usage:          s2,
		UsageMeasured:  true,
		TurnsCompleted: 2,
		TurnsStarted:   2,
	}, defaultExitParams(t, store))

	if len(store.runHistories) != 1 {
		t.Fatalf("AppendRunHistory called %d times, want 1", len(store.runHistories))
	}
	if got := store.runHistories[0].TotalTokens; got != s2.TotalTokens {
		t.Errorf("run_history TotalTokens = %d, want %d", got, s2.TotalTokens)
	}

	if len(store.sessionMetadata) != 1 {
		t.Fatalf("UpsertSessionMetadata called %d times, want 1", len(store.sessionMetadata))
	}
	if got := store.sessionMetadata[0].ModelName; got != model {
		t.Errorf("session_metadata.model_name = %q, want %q", got, model)
	}
}

func TestHandleWorkerExit_UnreachableFromRuntimeSnapshot(t *testing.T) {
	t.Parallel()

	store := &mockExitStore{}
	state := exitState(t, "ISSUE-P17", nil)
	entry := state.Running["ISSUE-P17"]
	entry.UsageMeasured = false

	HandleWorkerExit(state, WorkerResult{
		IssueID:       "ISSUE-P17",
		Identifier:    "ISSUE-P17-ident",
		ExitKind:      WorkerExitNormal,
		AgentAdapter:  "mock",
		WorkspacePath: "/tmp/ws",
		Usage:         domain.TokenUsage{InputTokens: 500, OutputTokens: 300, TotalTokens: 800},
	}, defaultExitParams(t, store))

	snap := RuntimeSnapshot(state, baseTime.Add(120*time.Second))
	for _, row := range snap.Running {
		if row.IssueID == "ISSUE-P17" {
			t.Fatalf("RuntimeSnapshot still carries a running row for the exited issue: %+v", row)
		}
	}
	if len(snap.Running) != 0 {
		t.Errorf("len(snap.Running) = %d, want 0 after HandleWorkerExit", len(snap.Running))
	}
}
