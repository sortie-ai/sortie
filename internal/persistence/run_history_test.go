package persistence

import (
	"context"
	"slices"
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

// assertTokenFields verifies the four token counters of a RunHistory row.
func assertTokenFields(t *testing.T, reader string, got RunHistory, wantIn, wantOut, wantTotal, wantCache int64) {
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
	inserted := appendOrFatal(t, s, run)

	assertTokenFields(t, "AppendRunHistory", inserted, 1100, 2200, 3300, 440)

	byIssue, err := s.QueryRunHistoryByIssue(ctx, run.IssueID)
	if err != nil {
		t.Fatalf("QueryRunHistoryByIssue: %v", err)
	}
	if len(byIssue) != 1 {
		t.Fatalf("QueryRunHistoryByIssue returned %d entries, want 1", len(byIssue))
	}
	assertTokenFields(t, "QueryRunHistoryByIssue", byIssue[0], 1100, 2200, 3300, 440)

	recent, err := s.QueryRecentRunHistory(ctx, 1, 0)
	if err != nil {
		t.Fatalf("QueryRecentRunHistory: %v", err)
	}
	if len(recent) != 1 {
		t.Fatalf("QueryRecentRunHistory returned %d entries, want 1", len(recent))
	}
	assertTokenFields(t, "QueryRecentRunHistory", recent[0], 1100, 2200, 3300, 440)

	// The paginated branch of QueryRecentRunHistory uses a separate SELECT;
	// exercise it with a cursor past the inserted row.
	paginated, err := s.QueryRecentRunHistory(ctx, 1, inserted.ID+1)
	if err != nil {
		t.Fatalf("QueryRecentRunHistory(afterID): %v", err)
	}
	if len(paginated) != 1 {
		t.Fatalf("QueryRecentRunHistory(afterID) returned %d entries, want 1", len(paginated))
	}
	assertTokenFields(t, "QueryRecentRunHistory(afterID)", paginated[0], 1100, 2200, 3300, 440)

	recovery, err := s.LoadLatestSuccessfulRunsForReactionRecovery(ctx, time.Time{}, 10)
	if err != nil {
		t.Fatalf("LoadLatestSuccessfulRunsForReactionRecovery: %v", err)
	}
	if len(recovery) != 1 {
		t.Fatalf("LoadLatestSuccessfulRunsForReactionRecovery returned %d entries, want 1", len(recovery))
	}
	assertTokenFields(t, "LoadLatestSuccessfulRunsForReactionRecovery", recovery[0], 1100, 2200, 3300, 440)
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
	assertTokenFields(t, "QueryRunHistoryByIssue(legacy)", entries[0], 0, 0, 0, 0)
}

func TestSumTotalTokensByIssue(t *testing.T) {
	t.Parallel()

	t.Run("no rows returns zero sum and zero count", func(t *testing.T) {
		t.Parallel()
		s := openTestStore(t)
		migrateOrFatal(t, s)

		sum, count, err := s.SumTotalTokensByIssue(context.Background(), "ISS-NONE")
		if err != nil {
			t.Fatalf("SumTotalTokensByIssue(ISS-NONE) unexpected error: %v", err)
		}
		if sum != 0 || count != 0 {
			t.Errorf("SumTotalTokensByIssue(ISS-NONE) = (%d, %d), want (0, 0)", sum, count)
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

		sum, count, err := s.SumTotalTokensByIssue(context.Background(), "ISS-SUM")
		if err != nil {
			t.Fatalf("SumTotalTokensByIssue(ISS-SUM) unexpected error: %v", err)
		}
		if sum != 600 {
			t.Errorf("SumTotalTokensByIssue(ISS-SUM) sum = %d, want 600", sum)
		}
		if count != 3 {
			t.Errorf("SumTotalTokensByIssue(ISS-SUM) count = %d, want 3", count)
		}
	})
}

func TestQueryTokenExhaustedIssues(t *testing.T) {
	t.Parallel()

	t.Run("empty candidateIDs returns empty non-nil slice", func(t *testing.T) {
		t.Parallel()
		s := openTestStore(t)
		migrateOrFatal(t, s)

		result, err := s.QueryTokenExhaustedIssues(context.Background(), []string{}, 1000)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result == nil {
			t.Fatal("result = nil, want empty non-nil slice")
		}
		if len(result) != 0 {
			t.Errorf("result = %v, want empty slice", result)
		}
	})

	t.Run("candidate with sum below maxTokens not returned", func(t *testing.T) {
		t.Parallel()
		s := openTestStore(t)
		migrateOrFatal(t, s)

		appendOrFatal(t, s, tokenRun(1, "ISS-UNDER", 400))
		appendOrFatal(t, s, tokenRun(2, "ISS-UNDER", 500))

		result, err := s.QueryTokenExhaustedIssues(context.Background(), []string{"ISS-UNDER"}, 1000)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(result) != 0 {
			t.Errorf("result = %v, want empty (sum 900 < maxTokens 1000)", result)
		}
	})

	t.Run("candidate with sum equal to maxTokens is returned", func(t *testing.T) {
		t.Parallel()
		s := openTestStore(t)
		migrateOrFatal(t, s)

		appendOrFatal(t, s, tokenRun(1, "ISS-EXACT", 600))
		appendOrFatal(t, s, tokenRun(2, "ISS-EXACT", 400))

		result, err := s.QueryTokenExhaustedIssues(context.Background(), []string{"ISS-EXACT"}, 1000)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(result) != 1 || result[0] != "ISS-EXACT" {
			t.Errorf("result = %v, want [ISS-EXACT]", result)
		}
	})

	t.Run("candidate with sum exceeding maxTokens is returned", func(t *testing.T) {
		t.Parallel()
		s := openTestStore(t)
		migrateOrFatal(t, s)

		appendOrFatal(t, s, tokenRun(1, "ISS-OVER", 900))
		appendOrFatal(t, s, tokenRun(2, "ISS-OVER", 900))

		result, err := s.QueryTokenExhaustedIssues(context.Background(), []string{"ISS-OVER"}, 1000)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(result) != 1 || result[0] != "ISS-OVER" {
			t.Errorf("result = %v, want [ISS-OVER]", result)
		}
	})

	t.Run("mixed candidates only qualifying ones returned", func(t *testing.T) {
		t.Parallel()
		s := openTestStore(t)
		migrateOrFatal(t, s)

		// ISS-HOT: 1200 tokens (exhausted), ISS-COLD: 300 (not), ISS-NONE: no rows.
		appendOrFatal(t, s, tokenRun(1, "ISS-HOT", 700))
		appendOrFatal(t, s, tokenRun(2, "ISS-HOT", 500))
		appendOrFatal(t, s, tokenRun(3, "ISS-COLD", 300))

		result, err := s.QueryTokenExhaustedIssues(
			context.Background(),
			[]string{"ISS-HOT", "ISS-COLD", "ISS-NONE"},
			1000,
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(result) != 1 || result[0] != "ISS-HOT" {
			t.Errorf("result = %v, want [ISS-HOT]", result)
		}
	})

	t.Run("non-candidate issues are not returned", func(t *testing.T) {
		t.Parallel()
		s := openTestStore(t)
		migrateOrFatal(t, s)

		appendOrFatal(t, s, tokenRun(1, "ISS-OUTSIDE", 9000))

		result, err := s.QueryTokenExhaustedIssues(context.Background(), []string{"ISS-INSIDE"}, 1000)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(result) != 0 {
			t.Errorf("result = %v, want empty (exhausted issue not in candidate list)", result)
		}
	})

	// The advisory tool reads SumTotalTokensByIssue while enforcement reads
	// QueryTokenExhaustedIssues; both must agree on the same ledger value.
	t.Run("completed-session sum matches enforcement threshold", func(t *testing.T) {
		t.Parallel()
		s := openTestStore(t)
		migrateOrFatal(t, s)
		ctx := context.Background()

		appendOrFatal(t, s, tokenRun(1, "ISS-PARITY", 123))
		appendOrFatal(t, s, tokenRun(2, "ISS-PARITY", 456))
		appendOrFatal(t, s, tokenRun(3, "ISS-PARITY", 789))

		sum, _, err := s.SumTotalTokensByIssue(ctx, "ISS-PARITY")
		if err != nil {
			t.Fatalf("SumTotalTokensByIssue(ISS-PARITY) unexpected error: %v", err)
		}

		atSum, err := s.QueryTokenExhaustedIssues(ctx, []string{"ISS-PARITY"}, int(sum))
		if err != nil {
			t.Fatalf("QueryTokenExhaustedIssues(maxTokens=%d) unexpected error: %v", sum, err)
		}
		if !slices.Contains(atSum, "ISS-PARITY") {
			t.Errorf("QueryTokenExhaustedIssues(maxTokens=%d) = %v, want to contain ISS-PARITY", sum, atSum)
		}

		aboveSum, err := s.QueryTokenExhaustedIssues(ctx, []string{"ISS-PARITY"}, int(sum)+1)
		if err != nil {
			t.Fatalf("QueryTokenExhaustedIssues(maxTokens=%d) unexpected error: %v", sum+1, err)
		}
		if slices.Contains(aboveSum, "ISS-PARITY") {
			t.Errorf("QueryTokenExhaustedIssues(maxTokens=%d) = %v, want ISS-PARITY absent", sum+1, aboveSum)
		}
	})
}
