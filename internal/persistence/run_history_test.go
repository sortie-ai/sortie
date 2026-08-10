package persistence

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// tokenRun returns a run_history row for the given issue carrying the
// given total_tokens, with the remaining fields from newTestRun.
func tokenRun(i int, issueID string, totalTokens int64) RunHistory {
	run := newTestRun(i)
	run.IssueID = issueID
	run.TotalTokens = totalTokens
	return run
}

// assertTokenFields verifies the four token counters and the measurement
// flag of a RunHistory row.
func assertTokenFields(t *testing.T, reader string, got RunHistory, wantIn, wantOut, wantTotal, wantCache int64, wantMeasured bool) {
	t.Helper()
	if got.InputTokens != wantIn {
		t.Errorf("%s InputTokens = %d, want %d", reader, got.InputTokens, wantIn)
	}
	if got.OutputTokens != wantOut {
		t.Errorf("%s OutputTokens = %d, want %d", reader, got.OutputTokens, wantOut)
	}
	if got.TotalTokens != wantTotal {
		t.Errorf("%s TotalTokens = %d, want %d", reader, got.TotalTokens, wantTotal)
	}
	if got.CacheReadTokens != wantCache {
		t.Errorf("%s CacheReadTokens = %d, want %d", reader, got.CacheReadTokens, wantCache)
	}
	if got.TokensMeasured != wantMeasured {
		t.Errorf("%s TokensMeasured = %v, want %v", reader, got.TokensMeasured, wantMeasured)
	}
}

// TestRunHistoryTokenColumns_RoundTrip writes one row with non-zero token
// counters and reads it back through every reader. A token column missed
// in one reader's SELECT or Scan list compiles cleanly and fails only at
// runtime, so each reader is asserted explicitly.
func TestRunHistoryTokenColumns_RoundTrip(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	run := newTestRun(1)
	run.InputTokens = 1100
	run.OutputTokens = 2200
	run.TotalTokens = 3300
	run.CacheReadTokens = 440
	run.TokensMeasured = true
	inserted := appendOrFatal(t, s, run)

	assertTokenFields(t, "AppendRunHistory", inserted, 1100, 2200, 3300, 440, true)

	byIssue, err := s.QueryRunHistoryByIssue(ctx, run.IssueID)
	if err != nil {
		t.Fatalf("QueryRunHistoryByIssue: %v", err)
	}
	if len(byIssue) != 1 {
		t.Fatalf("QueryRunHistoryByIssue returned %d entries, want 1", len(byIssue))
	}
	assertTokenFields(t, "QueryRunHistoryByIssue", byIssue[0], 1100, 2200, 3300, 440, true)

	recent, err := s.QueryRecentRunHistory(ctx, 1, 0)
	if err != nil {
		t.Fatalf("QueryRecentRunHistory: %v", err)
	}
	if len(recent) != 1 {
		t.Fatalf("QueryRecentRunHistory returned %d entries, want 1", len(recent))
	}
	assertTokenFields(t, "QueryRecentRunHistory", recent[0], 1100, 2200, 3300, 440, true)

	// The paginated branch of QueryRecentRunHistory uses a separate SELECT;
	// exercise it with a cursor past the inserted row.
	paginated, err := s.QueryRecentRunHistory(ctx, 1, inserted.ID+1)
	if err != nil {
		t.Fatalf("QueryRecentRunHistory(afterID): %v", err)
	}
	if len(paginated) != 1 {
		t.Fatalf("QueryRecentRunHistory(afterID) returned %d entries, want 1", len(paginated))
	}
	assertTokenFields(t, "QueryRecentRunHistory(afterID)", paginated[0], 1100, 2200, 3300, 440, true)

	recovery, err := s.LoadLatestSuccessfulRunsForReactionRecovery(ctx, time.Time{}, 10)
	if err != nil {
		t.Fatalf("LoadLatestSuccessfulRunsForReactionRecovery: %v", err)
	}
	if len(recovery) != 1 {
		t.Fatalf("LoadLatestSuccessfulRunsForReactionRecovery returned %d entries, want 1", len(recovery))
	}
	assertTokenFields(t, "LoadLatestSuccessfulRunsForReactionRecovery", recovery[0], 1100, 2200, 3300, 440, true)
}

// TestRunHistoryTokenColumns_UnmeasuredRoundTrip verifies that a row
// written with TokensMeasured false round-trips that value, alongside its
// zero token columns, through every reader that projects the token
// columns.
func TestRunHistoryTokenColumns_UnmeasuredRoundTrip(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	run := newTestRun(1)
	run.TokensMeasured = false
	inserted := appendOrFatal(t, s, run)

	assertTokenFields(t, "AppendRunHistory", inserted, 0, 0, 0, 0, false)

	byIssue, err := s.QueryRunHistoryByIssue(ctx, run.IssueID)
	if err != nil {
		t.Fatalf("QueryRunHistoryByIssue: %v", err)
	}
	if len(byIssue) != 1 {
		t.Fatalf("QueryRunHistoryByIssue returned %d entries, want 1", len(byIssue))
	}
	assertTokenFields(t, "QueryRunHistoryByIssue", byIssue[0], 0, 0, 0, 0, false)

	recent, err := s.QueryRecentRunHistory(ctx, 1, 0)
	if err != nil {
		t.Fatalf("QueryRecentRunHistory: %v", err)
	}
	if len(recent) != 1 {
		t.Fatalf("QueryRecentRunHistory returned %d entries, want 1", len(recent))
	}
	assertTokenFields(t, "QueryRecentRunHistory", recent[0], 0, 0, 0, 0, false)

	recovery, err := s.LoadLatestSuccessfulRunsForReactionRecovery(ctx, time.Time{}, 10)
	if err != nil {
		t.Fatalf("LoadLatestSuccessfulRunsForReactionRecovery: %v", err)
	}
	if len(recovery) != 1 {
		t.Fatalf("LoadLatestSuccessfulRunsForReactionRecovery returned %d entries, want 1", len(recovery))
	}
	assertTokenFields(t, "LoadLatestSuccessfulRunsForReactionRecovery", recovery[0], 0, 0, 0, 0, false)
}

// TestRunHistoryTokenColumns_LegacyRowsReadZero verifies that rows written
// without token values (matching pre-migration rows, which rely on the
// NOT NULL DEFAULT 0 column definition) scan as zero.
func TestRunHistoryTokenColumns_LegacyRowsReadZero(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO run_history (issue_id, identifier, attempt, agent_adapter, workspace, started_at, completed_at, status)
		 VALUES ('ISS-LEGACY', 'PROJ-LEGACY', 1, 'mock', '/tmp/ws', '2026-03-19T10:00:00Z', '2026-03-19T10:30:00Z', 'succeeded')`); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}

	entries, err := s.QueryRunHistoryByIssue(ctx, "ISS-LEGACY")
	if err != nil {
		t.Fatalf("QueryRunHistoryByIssue: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("QueryRunHistoryByIssue returned %d entries, want 1", len(entries))
	}
	assertTokenFields(t, "QueryRunHistoryByIssue(legacy)", entries[0], 0, 0, 0, 0, true)
}

func TestTokenUsageByIssue(t *testing.T) {
	t.Parallel()

	t.Run("no rows returns zero usage", func(t *testing.T) {
		t.Parallel()
		s := openTestStore(t)
		migrateOrFatal(t, s)

		usage, err := s.TokenUsageByIssue(context.Background(), "ISS-NONE")
		if err != nil {
			t.Fatalf("TokenUsageByIssue(ISS-NONE) unexpected error: %v", err)
		}
		if usage != (IssueTokenUsage{}) {
			t.Errorf("TokenUsageByIssue(ISS-NONE) = %+v, want zero value", usage)
		}
	})

	t.Run("sums total_tokens and counts rows for the issue only", func(t *testing.T) {
		t.Parallel()
		s := openTestStore(t)
		migrateOrFatal(t, s)

		appendOrFatal(t, s, tokenRun(1, "ISS-SUM", 100))
		appendOrFatal(t, s, tokenRun(2, "ISS-SUM", 200))
		appendOrFatal(t, s, tokenRun(3, "ISS-SUM", 300))
		appendOrFatal(t, s, tokenRun(4, "ISS-OTHER", 5000))

		usage, err := s.TokenUsageByIssue(context.Background(), "ISS-SUM")
		if err != nil {
			t.Fatalf("TokenUsageByIssue(ISS-SUM) unexpected error: %v", err)
		}
		if usage.TotalTokens != 600 {
			t.Errorf("TokenUsageByIssue(ISS-SUM).TotalTokens = %d, want 600", usage.TotalTokens)
		}
		if usage.Sessions != 3 {
			t.Errorf("TokenUsageByIssue(ISS-SUM).Sessions = %d, want 3", usage.Sessions)
		}
	})

	t.Run("counts unmeasured sessions for a mixed-measurement issue", func(t *testing.T) {
		t.Parallel()
		s := openTestStore(t)
		migrateOrFatal(t, s)

		measured := tokenRun(1, "ISS-MIXED", 100)
		measured.TokensMeasured = true
		appendOrFatal(t, s, measured)

		unmeasured1 := tokenRun(2, "ISS-MIXED", 0)
		unmeasured1.TokensMeasured = false
		appendOrFatal(t, s, unmeasured1)

		unmeasured2 := tokenRun(3, "ISS-MIXED", 0)
		unmeasured2.TokensMeasured = false
		appendOrFatal(t, s, unmeasured2)

		usage, err := s.TokenUsageByIssue(context.Background(), "ISS-MIXED")
		if err != nil {
			t.Fatalf("TokenUsageByIssue(ISS-MIXED) unexpected error: %v", err)
		}
		if usage.TotalTokens != 100 {
			t.Errorf("TokenUsageByIssue(ISS-MIXED).TotalTokens = %d, want 100", usage.TotalTokens)
		}
		if usage.Sessions != 3 {
			t.Errorf("TokenUsageByIssue(ISS-MIXED).Sessions = %d, want 3", usage.Sessions)
		}
		if usage.UnmeasuredSessions != 2 {
			t.Errorf("TokenUsageByIssue(ISS-MIXED).UnmeasuredSessions = %d, want 2", usage.UnmeasuredSessions)
		}
	})
}

func TestQueryTokenBudgetUsage(t *testing.T) {
	t.Parallel()

	t.Run("empty candidateIDs returns empty non-nil map", func(t *testing.T) {
		t.Parallel()
		s := openTestStore(t)
		migrateOrFatal(t, s)

		result, err := s.QueryTokenBudgetUsage(context.Background(), []string{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result == nil {
			t.Fatal("result = nil, want empty non-nil map")
		}
		if len(result) != 0 {
			t.Errorf("result = %v, want empty map", result)
		}
	})

	t.Run("candidate with rows is returned with its usage", func(t *testing.T) {
		t.Parallel()
		s := openTestStore(t)
		migrateOrFatal(t, s)

		appendOrFatal(t, s, tokenRun(1, "ISS-UNDER", 400))
		appendOrFatal(t, s, tokenRun(2, "ISS-UNDER", 500))

		result, err := s.QueryTokenBudgetUsage(context.Background(), []string{"ISS-UNDER"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		usage, ok := result["ISS-UNDER"]
		if !ok {
			t.Fatalf("result = %v, want an entry for ISS-UNDER", result)
		}
		if usage.TotalTokens != 900 || usage.Sessions != 2 {
			t.Errorf("result[ISS-UNDER] = %+v, want TotalTokens=900 Sessions=2", usage)
		}
	})

	t.Run("counts unmeasured sessions per candidate", func(t *testing.T) {
		t.Parallel()
		s := openTestStore(t)
		migrateOrFatal(t, s)

		measured := tokenRun(1, "ISS-MIXED", 250)
		measured.TokensMeasured = true
		appendOrFatal(t, s, measured)

		unmeasured := tokenRun(2, "ISS-MIXED", 0)
		unmeasured.TokensMeasured = false
		appendOrFatal(t, s, unmeasured)

		result, err := s.QueryTokenBudgetUsage(context.Background(), []string{"ISS-MIXED"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		usage, ok := result["ISS-MIXED"]
		if !ok {
			t.Fatalf("result = %v, want an entry for ISS-MIXED", result)
		}
		if usage.TotalTokens != 250 || usage.Sessions != 2 || usage.UnmeasuredSessions != 1 {
			t.Errorf("result[ISS-MIXED] = %+v, want TotalTokens=250 Sessions=2 UnmeasuredSessions=1", usage)
		}
	})

	t.Run("mixed candidates report per-candidate usage", func(t *testing.T) {
		t.Parallel()
		s := openTestStore(t)
		migrateOrFatal(t, s)

		// ISS-HOT: 1200 tokens, ISS-COLD: 300, ISS-NONE: no rows.
		appendOrFatal(t, s, tokenRun(1, "ISS-HOT", 700))
		appendOrFatal(t, s, tokenRun(2, "ISS-HOT", 500))
		appendOrFatal(t, s, tokenRun(3, "ISS-COLD", 300))

		result, err := s.QueryTokenBudgetUsage(
			context.Background(),
			[]string{"ISS-HOT", "ISS-COLD", "ISS-NONE"},
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := result["ISS-HOT"].TotalTokens; got != 1200 {
			t.Errorf("result[ISS-HOT].TotalTokens = %d, want 1200", got)
		}
		if got := result["ISS-COLD"].TotalTokens; got != 300 {
			t.Errorf("result[ISS-COLD].TotalTokens = %d, want 300", got)
		}
		if _, ok := result["ISS-NONE"]; ok {
			t.Errorf("result = %v, want no entry for ISS-NONE (no rows)", result)
		}
	})

	t.Run("non-candidate issues are not returned", func(t *testing.T) {
		t.Parallel()
		s := openTestStore(t)
		migrateOrFatal(t, s)

		appendOrFatal(t, s, tokenRun(1, "ISS-OUTSIDE", 9000))

		result, err := s.QueryTokenBudgetUsage(context.Background(), []string{"ISS-INSIDE"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(result) != 0 {
			t.Errorf("result = %v, want empty (outside issue not in candidate list)", result)
		}
	})

	// The advisory tool reads TokenUsageByIssue while enforcement reads
	// QueryTokenBudgetUsage; both must agree on the same ledger value.
	t.Run("completed-session sum matches across both queries", func(t *testing.T) {
		t.Parallel()
		s := openTestStore(t)
		migrateOrFatal(t, s)
		ctx := context.Background()

		appendOrFatal(t, s, tokenRun(1, "ISS-PARITY", 123))
		appendOrFatal(t, s, tokenRun(2, "ISS-PARITY", 456))
		appendOrFatal(t, s, tokenRun(3, "ISS-PARITY", 789))

		usage, err := s.TokenUsageByIssue(ctx, "ISS-PARITY")
		if err != nil {
			t.Fatalf("TokenUsageByIssue(ISS-PARITY) unexpected error: %v", err)
		}

		budget, err := s.QueryTokenBudgetUsage(ctx, []string{"ISS-PARITY"})
		if err != nil {
			t.Fatalf("QueryTokenBudgetUsage unexpected error: %v", err)
		}
		if budget["ISS-PARITY"].TotalTokens != usage.TotalTokens {
			t.Errorf("QueryTokenBudgetUsage total = %d, want %d (parity with TokenUsageByIssue)",
				budget["ISS-PARITY"].TotalTokens, usage.TotalTokens)
		}
	})
}

// completionRun returns a run_history row for identifier carrying
// completedAt, with the remaining fields from newTestRun.
func completionRun(i int, identifier, completedAt string) RunHistory {
	run := newTestRun(i)
	run.Identifier = identifier
	run.IssueID = identifier
	run.CompletedAt = completedAt
	return run
}

func TestLatestRunCompletionByIdentifier(t *testing.T) {
	t.Parallel()

	t.Run("returns the later completion per identifier and omits identifiers with no rows", func(t *testing.T) {
		t.Parallel()
		s := openTestStore(t)
		migrateOrFatal(t, s)

		appendOrFatal(t, s, completionRun(1, "PROJ-A", "2026-01-01T00:00:00Z"))
		appendOrFatal(t, s, completionRun(2, "PROJ-A", "2026-01-03T00:00:00Z"))
		appendOrFatal(t, s, completionRun(3, "PROJ-B", "2026-02-01T00:00:00Z"))
		appendOrFatal(t, s, completionRun(4, "PROJ-B", "2026-01-15T00:00:00Z"))
		appendOrFatal(t, s, completionRun(5, "PROJ-C", "2026-03-01T00:00:00Z"))
		appendOrFatal(t, s, completionRun(6, "PROJ-C", "2026-03-05T00:00:00Z"))

		got, err := s.LatestRunCompletionByIdentifier(context.Background(), []string{"PROJ-A", "PROJ-B", "PROJ-C", "PROJ-D"})
		if err != nil {
			t.Fatalf("LatestRunCompletionByIdentifier: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("len(result) = %d, want 3; result = %v", len(got), got)
		}

		want := map[string]string{
			"PROJ-A": "2026-01-03T00:00:00Z",
			"PROJ-B": "2026-02-01T00:00:00Z",
			"PROJ-C": "2026-03-05T00:00:00Z",
		}
		for identifier, wantCompletedAt := range want {
			if got[identifier] != wantCompletedAt {
				t.Errorf("result[%q] = %q, want %q", identifier, got[identifier], wantCompletedAt)
			}
		}
		if _, ok := got["PROJ-D"]; ok {
			t.Error(`result["PROJ-D"] present, want omitted (no run_history rows)`)
		}
	})

	t.Run("empty input returns empty non-nil map without querying", func(t *testing.T) {
		t.Parallel()
		s := openTestStore(t)
		migrateOrFatal(t, s)

		got, err := s.LatestRunCompletionByIdentifier(context.Background(), []string{})
		if err != nil {
			t.Fatalf("LatestRunCompletionByIdentifier: %v", err)
		}
		if got == nil {
			t.Fatal("result = nil, want empty non-nil map")
		}
		if len(got) != 0 {
			t.Errorf("result = %v, want empty map", got)
		}
	})

	t.Run("batches identifiers at 500 per query and merges results", func(t *testing.T) {
		t.Parallel()
		s := openTestStore(t)
		migrateOrFatal(t, s)

		const total = 1200
		identifiers := make([]string, total)
		for i := range total {
			id := fmt.Sprintf("PROJ-CHUNK-%d", i)
			identifiers[i] = id
			appendOrFatal(t, s, completionRun(i, id, "2026-01-01T00:00:00Z"))
		}

		got, err := s.LatestRunCompletionByIdentifier(context.Background(), identifiers)
		if err != nil {
			t.Fatalf("LatestRunCompletionByIdentifier: %v", err)
		}
		if len(got) != total {
			t.Fatalf("len(result) = %d, want %d", len(got), total)
		}
		for _, id := range identifiers {
			if _, ok := got[id]; !ok {
				t.Fatalf("result missing identifier %q", id)
			}
		}
	})
}
