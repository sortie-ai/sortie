package persistence

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func tokenRun(i int, issueID string, totalTokens int64) RunHistory {
	run := newTestRun(i)
	run.IssueID = issueID
	run.TotalTokens = totalTokens
	return run
}

func queryAllRuns(t *testing.T, s *Store, limit int) []RunHistory {
	t.Helper()
	rows, err := s.QueryRunHistory(context.Background(), RunHistoryFilter{Limit: limit})
	if err != nil {
		t.Fatalf("QueryRunHistory(limit=%d): %v", limit, err)
	}
	return rows
}

func triggerText(trigger *string) string {
	if trigger == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%q", *trigger)
}

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

func appendWithPartsOrFatal(t *testing.T, s *Store, run RunHistory) RunHistory {
	t.Helper()
	dispatchID := "dispatch-" + run.IssueID
	run.DispatchID = &dispatchID
	got, err := s.AppendRunHistoryWithEvents(context.Background(), run, nil, unboundedRunEvents)
	if err != nil {
		t.Fatalf("AppendRunHistoryWithEvents: %v", err)
	}
	return got
}

func queryModelUsage(t *testing.T, s *Store, runID int64) map[string]ModelTokens {
	t.Helper()
	got, err := s.QueryRunModelUsage(context.Background(), []int64{runID})
	if err != nil {
		t.Fatalf("QueryRunModelUsage(%d): %v", runID, err)
	}

	parts := map[string]ModelTokens{}
	for _, part := range got[runID] {
		parts[part.Model] = part.Tokens
	}
	return parts
}

func TestQueryRunModelUsage(t *testing.T) {
	t.Parallel()

	usage := map[string]ModelTokens{"m": {InputTokens: 10, OutputTokens: 5}}
	part := RunModelPart{Model: "m", Tokens: usage["m"]}
	classes := []string{"output", "cache_write"}

	tests := []struct {
		name  string
		usage map[string]ModelTokens
		costs map[string]ModelCost
		want  []RunModelPart
	}{
		{"classes read back in order", usage, map[string]ModelCost{"m": {CostStatus: "partial", UnratedClasses: classes}},
			[]RunModelPart{{Model: "m", Tokens: usage["m"], CostStatus: "partial", UnratedClasses: classes, UnratedRecorded: true}}},
		{"status without classes reads recorded-empty", usage, map[string]ModelCost{"m": {CostStatus: "estimated"}},
			[]RunModelPart{{Model: "m", Tokens: usage["m"], CostStatus: "estimated", UnratedRecorded: true}}},
		{"agent price source reads back and a part with none reads empty", usage, map[string]ModelCost{"m": {CostStatus: "estimated", PriceSource: "agent"}},
			[]RunModelPart{{Model: "m", Tokens: usage["m"], CostStatus: "estimated", UnratedRecorded: true, PriceSource: "agent"}}},
		{"no status reads not recorded", usage, map[string]ModelCost{"m": {UnratedClasses: classes}}, []RunModelPart{part}},
		{"run without parts has no key", nil, nil, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := openTestStore(t)
			migrateOrFatal(t, s)
			run := newTestRun(1)
			run.ModelUsage, run.ModelCosts = tt.usage, tt.costs
			inserted := appendWithPartsOrFatal(t, s, run)
			want := map[int64][]RunModelPart{}
			if tt.want != nil {
				want[inserted.ID] = tt.want
			}

			got, err := s.QueryRunModelUsage(context.Background(), []int64{inserted.ID})
			if err != nil {
				t.Fatalf("QueryRunModelUsage(%d): %v", inserted.ID, err)
			}

			if !reflect.DeepEqual(got, want) {
				t.Errorf("QueryRunModelUsage(%d) = %+v, want %+v", inserted.ID, got, want)
			}
		})
	}
}

// A token column missed in one reader's SELECT or Scan compiles cleanly and
// fails only at runtime.
func TestRunHistoryTokenColumns_RoundTrip(t *testing.T) {
	t.Parallel()

	twoModels := map[string]ModelTokens{
		"model-a": {InputTokens: 600, OutputTokens: 1200, CacheReadTokens: 200, CacheWriteTokens: 30},
		"model-b": {InputTokens: 500, OutputTokens: 1000, CacheReadTokens: 240},
	}

	tests := []struct {
		name                  string
		in, out, total, cache int64
		measured              bool
		parts                 map[string]ModelTokens
		wantParts             map[string]ModelTokens
	}{
		{name: "measured", in: 1100, out: 2200, total: 3300, cache: 440, measured: true},
		{name: "unmeasured"},
		{name: "parts land with the row", in: 1100, out: 2200, total: 3300, cache: 440, measured: true, parts: twoModels, wantParts: twoModels},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := openTestStore(t)
			migrateOrFatal(t, s)
			ctx := context.Background()

			run := newTestRun(1)
			run.InputTokens, run.OutputTokens, run.TotalTokens, run.CacheReadTokens = tt.in, tt.out, tt.total, tt.cache
			run.TokensMeasured = tt.measured
			run.ModelUsage = tt.parts
			appendRun := appendOrFatal
			if tt.parts != nil {
				appendRun = appendWithPartsOrFatal
			}
			inserted := appendRun(t, s, run)

			byIssue, err := s.QueryRunHistoryByIssue(ctx, run.IssueID)
			if err != nil {
				t.Fatalf("QueryRunHistoryByIssue: %v", err)
			}
			recovery, err := s.LoadLatestSuccessfulRunsForReactionRecovery(ctx, time.Time{}, 10)
			if err != nil {
				t.Fatalf("LoadLatestSuccessfulRunsForReactionRecovery: %v", err)
			}
			readers := map[string][]RunHistory{
				"AppendRunHistory":                            {inserted},
				"QueryRunHistoryByIssue":                      byIssue,
				"QueryRunHistory":                             queryAllRuns(t, s, 1),
				"LoadLatestSuccessfulRunsForReactionRecovery": recovery,
			}
			for name, rows := range readers {
				if len(rows) != 1 {
					t.Fatalf("%s returned %d entries, want 1", name, len(rows))
				}
				assertTokenFields(t, name, rows[0], tt.in, tt.out, tt.total, tt.cache, tt.measured)
			}
			if got := queryModelUsage(t, s, inserted.ID); !maps.Equal(got, tt.wantParts) {
				t.Errorf("run_model_usage of run %d = %+v, want %+v", inserted.ID, got, tt.wantParts)
			}
		})
	}
}

func TestRunHistoryConfiguredSettingsColumns_RoundTrip(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	run := newTestRun(1)
	run.ConfiguredModel, run.ConfiguredEffort, run.ReportedModel = "provider/cheap", "low", "provider/cheap-20260101"
	run.ChainID, run.StagePrevious, run.StageTarget, run.StageResult = "chain-1", "specify", "plan", "advanced"
	run.IssueTitle, run.IssueURL = "Fix the login redirect", "https://example.atlassian.net/browse/PROJ-1"
	run.DispatchedAt = "2026-03-19T09:00:00Z"
	run.Trigger, run.ReactionKind, run.SessionID, run.SSHHost = new("ci_fix"), "ci", "s-1", "worker-1"
	inserted := appendOrFatal(t, s, run)

	readers := map[string]func() ([]RunHistory, error){
		"QueryRunHistoryByIssue": func() ([]RunHistory, error) { return s.QueryRunHistoryByIssue(ctx, run.IssueID) },
		"QueryRunHistory":        func() ([]RunHistory, error) { return s.QueryRunHistory(ctx, RunHistoryFilter{Limit: 1}) },
		"LoadLatestSuccessfulRunsForReactionRecovery": func() ([]RunHistory, error) {
			return s.LoadLatestSuccessfulRunsForReactionRecovery(ctx, time.Time{}, 10)
		},
	}
	got := map[string][]RunHistory{"AppendRunHistory": {inserted}}
	for name, read := range readers {
		rows, err := read()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got[name] = rows
	}

	for name, rows := range got {
		if len(rows) != 1 || rows[0].ConfiguredModel != "provider/cheap" || rows[0].ConfiguredEffort != "low" || rows[0].ReportedModel != "provider/cheap-20260101" {
			t.Errorf("%s = %+v, want one row with ConfiguredModel, ConfiguredEffort, ReportedModel %q, %q, %q", name, rows, "provider/cheap", "low", "provider/cheap-20260101")
		}
	}

	type stageCols struct{ chain, previous, target, result string }
	full := stageCols{"chain-1", "specify", "plan", "advanced"}
	wantStage := map[string]stageCols{
		"AppendRunHistory": full,
		"QueryRunHistory":  full,
		"LoadLatestSuccessfulRunsForReactionRecovery": {chain: "chain-1", previous: "specify"},
	}
	for name, want := range wantStage {
		r := got[name][0]
		if gotCols := (stageCols{r.ChainID, r.StagePrevious, r.StageTarget, r.StageResult}); gotCols != want {
			t.Errorf("%s chain, previous, target, result = %+v, want %+v", name, gotCols, want)
		}
	}

	if r := got["QueryRunHistory"][0]; r.Trigger == nil || *r.Trigger != "ci_fix" || r.ReactionKind != "ci" || r.SessionID != "s-1" || r.SSHHost != "worker-1" || !r.OriginRecorded {
		t.Errorf("QueryRunHistory trigger, reaction_kind, session_id, ssh_host, origin recorded = %s, %q, %q, %q, %v, want %q, %q, %q, %q, true",
			triggerText(r.Trigger), r.ReactionKind, r.SessionID, r.SSHHost, r.OriginRecorded, "ci_fix", "ci", "s-1", "worker-1")
	}

	if r := got["LoadLatestSuccessfulRunsForReactionRecovery"][0]; r.IssueTitle != run.IssueTitle || r.IssueURL != run.IssueURL || r.DispatchedAt != run.DispatchedAt {
		t.Errorf("LoadLatestSuccessfulRunsForReactionRecovery title, url, dispatched_at = %q, %q, %q, want %q, %q, %q", r.IssueTitle, r.IssueURL, r.DispatchedAt, run.IssueTitle, run.IssueURL, run.DispatchedAt)
	}
}

func TestListReportedModels(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	for i, model := range []string{"model-a", "model-a", "", "model-b"} {
		run := newTestRun(i + 1)
		run.ReportedModel = model
		appendOrFatal(t, s, run)
	}
	spent := newTestRun(5)
	spent.ModelUsage = map[string]ModelTokens{"model-a": {}, "model-c": {}, "": {}}
	appendWithPartsOrFatal(t, s, spent)

	got, err := s.ListReportedModels(context.Background())
	if err != nil {
		t.Fatalf("ListReportedModels: %v", err)
	}

	slices.Sort(got)
	if want := []string{"model-a", "model-b", "model-c"}; !slices.Equal(got, want) {
		t.Errorf("ListReportedModels() = %q, want %q", got, want)
	}
}

func TestListModelUse(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	runs := []struct {
		kind, completedAt string
	}{
		{"kind-a", "2026-03-19T10:00:00Z"},
		{"kind-a", "2026-03-19T11:30:00+03:00"},
		{"kind-b", "2026-03-19T09:00:00Z"},
	}
	for i, r := range runs {
		run := newTestRun(i + 1)
		run.AgentAdapter, run.CompletedAt = r.kind, r.completedAt
		run.ModelUsage = map[string]ModelTokens{"shared-model": {}}
		appendWithPartsOrFatal(t, s, run)
	}

	got, err := s.ListModelUse(context.Background())
	if err != nil {
		t.Fatalf("ListModelUse: %v", err)
	}

	slices.SortFunc(got, func(a, b ModelUse) int { return strings.Compare(a.AgentKind, b.AgentKind) })
	want := []ModelUse{
		{AgentKind: "kind-a", Model: "shared-model", Runs: 2, LastUsedAt: time.Date(2026, 3, 19, 10, 0, 0, 0, time.UTC)},
		{AgentKind: "kind-b", Model: "shared-model", Runs: 1, LastUsedAt: time.Date(2026, 3, 19, 9, 0, 0, 0, time.UTC)},
	}
	if !slices.Equal(got, want) {
		t.Errorf("ListModelUse() = %+v, want %+v", got, want)
	}
}

func TestRecordRunStageResult(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()
	first, second := appendOrFatal(t, s, newTestRun(1)), appendOrFatal(t, s, newTestRun(2))

	if err := s.RecordRunStageResult(ctx, second.ID, "plan", "ceiling"); err != nil {
		t.Fatalf("RecordRunStageResult(%d): %v", second.ID, err)
	}
	if err := s.RecordRunStageResult(ctx, second.ID+100, "plan", "failed"); err != nil {
		t.Errorf("RecordRunStageResult(missing id) = %v, want nil", err)
	}

	rows := queryAllRuns(t, s, 2)
	if len(rows) != 2 {
		t.Fatalf("QueryRunHistory = %+v, want two rows", rows)
	}
	for _, r := range rows {
		want := [2]string{}
		if r.ID == second.ID {
			want = [2]string{"plan", "ceiling"}
		}
		if got := [2]string{r.StageTarget, r.StageResult}; got != want {
			t.Errorf("row %d (the untouched one is %d) target, result = %q, want %q", r.ID, first.ID, got, want)
		}
	}
}

func TestRunHistoryErrorAndReviewMetadata_RoundTrip(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	meta := `{"enabled":true,"iterations":[],"total_iterations":1,"final_verdict":"pass","cap_reached":false}`
	run := newTestRun(1)
	run.Error = new("transient network error")
	run.ReviewMetadata = &meta
	skills := `[{"skill":"ce-plan"},{"skill":"ce-work","tool_error":true}]`
	run.Skills = &skills
	appendOrFatal(t, s, run)

	recent := queryAllRuns(t, s, 1)
	if len(recent) != 1 {
		t.Fatalf("QueryRunHistory returned %d entries, want 1", len(recent))
	}
	if recent[0].Error == nil || *recent[0].Error != "transient network error" {
		t.Errorf("QueryRunHistory Error = %v, want %q", recent[0].Error, "transient network error")
	}
	if recent[0].ReviewMetadata == nil || *recent[0].ReviewMetadata != meta {
		t.Errorf("QueryRunHistory ReviewMetadata = %v, want %q", recent[0].ReviewMetadata, meta)
	}

	recovery, err := s.LoadLatestSuccessfulRunsForReactionRecovery(ctx, time.Time{}, 10)
	if err != nil {
		t.Fatalf("LoadLatestSuccessfulRunsForReactionRecovery: %v", err)
	}
	if len(recovery) != 1 {
		t.Fatalf("LoadLatestSuccessfulRunsForReactionRecovery returned %d entries, want 1", len(recovery))
	}
	if recovery[0].Error == nil || *recovery[0].Error != "transient network error" {
		t.Errorf("LoadLatestSuccessfulRunsForReactionRecovery Error = %v, want %q", recovery[0].Error, "transient network error")
	}
	if recovery[0].ReviewMetadata == nil || *recovery[0].ReviewMetadata != meta {
		t.Errorf("LoadLatestSuccessfulRunsForReactionRecovery ReviewMetadata = %v, want %q", recovery[0].ReviewMetadata, meta)
	}

	byIssue, err := s.QueryRunHistoryByIssue(ctx, "ISS-1")
	if err != nil {
		t.Fatalf("QueryRunHistoryByIssue(ISS-1): %v", err)
	}
	for name, rows := range map[string][]RunHistory{
		"QueryRunHistory":                             recent,
		"QueryRunHistoryByIssue":                      byIssue,
		"LoadLatestSuccessfulRunsForReactionRecovery": recovery,
	} {
		if len(rows) != 1 || rows[0].Skills == nil || *rows[0].Skills != skills {
			t.Errorf("%s Skills = %+v, want %q", name, rows, skills)
		}
	}

	appendOrFatal(t, s, newTestRun(2))
	none, err := s.QueryRunHistoryByIssue(ctx, "ISS-2")
	if err != nil || len(none) != 1 || none[0].Skills != nil {
		t.Errorf("QueryRunHistoryByIssue(ISS-2) = %+v, %v, want one row with nil Skills", none, err)
	}
}

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

	t.Run("class sums count measured rows only", func(t *testing.T) {
		t.Parallel()
		s := openTestStore(t)
		migrateOrFatal(t, s)

		const issueID = "ISS-CLASSES"
		rows := []RunHistory{
			{InputTokens: 100, CacheReadTokens: 30, CacheWriteTokens: 20, OutputTokens: 10, TotalTokens: 160, TokensMeasured: true},
			{InputTokens: 10, CacheReadTokens: 40, OutputTokens: 5, TotalTokens: 55, TokensMeasured: true},
			{InputTokens: 500, CacheReadTokens: 7, CacheWriteTokens: 3, OutputTokens: 50},
			{TokensMeasured: true},
		}
		for i, row := range rows {
			run := tokenRun(i+1, issueID, row.TotalTokens)
			run.InputTokens, run.OutputTokens = row.InputTokens, row.OutputTokens
			run.CacheReadTokens, run.CacheWriteTokens = row.CacheReadTokens, row.CacheWriteTokens
			run.TokensMeasured = row.TokensMeasured
			appendOrFatal(t, s, run)
		}

		usage, err := s.TokenUsageByIssue(context.Background(), issueID)
		if err != nil {
			t.Fatalf("TokenUsageByIssue(%s) unexpected error: %v", issueID, err)
		}

		wantTotals := IssueTokenUsage{
			TotalTokens: 215, UncachedTokens: 145, Sessions: 4, UnmeasuredSessions: 1,
			InputTokens: 110, OutputTokens: 15, CacheReadTokens: 70, CacheWriteTokens: 20,
			UnpricedSessions: 3,
		}
		if usage != wantTotals {
			t.Errorf("TokenUsageByIssue(%s) = %+v, want totals %+v", issueID, usage, wantTotals)
		}
	})

	t.Run("recorded cost aggregates count only rows that spent tokens", func(t *testing.T) {
		t.Parallel()
		s := openTestStore(t)
		migrateOrFatal(t, s)

		cost := func(v float64) *float64 { return &v }
		rows := []struct {
			cost   *float64
			status string
			tokens int64
		}{
			{cost(15), "estimated", 100},
			{cost(0), "estimated", 0},
			{cost(12), "partial", 100},
			{nil, "unpriced", 100},
			{nil, "", 100},
			{nil, "", 0},
		}
		for i, row := range rows {
			run := tokenRun(i+1, "ISS-COST", row.tokens)
			run.TokensMeasured = true
			run.EstimatedCostUSD, run.CostStatus = row.cost, row.status
			appendOrFatal(t, s, run)
		}

		usage, err := s.TokenUsageByIssue(context.Background(), "ISS-COST")
		if err != nil {
			t.Fatalf("TokenUsageByIssue(ISS-COST) unexpected error: %v", err)
		}

		got := []float64{usage.RecordedCostUSD, float64(usage.PricedSessions), float64(usage.PartialSessions), float64(usage.UnpricedSessions)}
		want := []float64{27, 2, 1, 2}
		if !slices.Equal(got, want) {
			t.Errorf("TokenUsageByIssue(ISS-COST) (RecordedCostUSD, PricedSessions, PartialSessions, UnpricedSessions) = %v, want %v", got, want)
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

	t.Run("sums unaccounted turns per candidate", func(t *testing.T) {
		t.Parallel()
		s := openTestStore(t)
		migrateOrFatal(t, s)

		measured := tokenRun(1, "ISS-PARTIAL", 250)
		measured.TokensMeasured = true
		measured.UnaccountedTurns = 2
		appendOrFatal(t, s, measured)

		clean := tokenRun(2, "ISS-PARTIAL", 100)
		clean.TokensMeasured = true
		appendOrFatal(t, s, clean)

		batch, err := s.QueryTokenBudgetUsage(context.Background(), []string{"ISS-PARTIAL"})
		if err != nil {
			t.Fatalf("QueryTokenBudgetUsage() error = %v", err)
		}
		if got := batch["ISS-PARTIAL"].UnaccountedTurns; got != 2 {
			t.Errorf("QueryTokenBudgetUsage()[ISS-PARTIAL].UnaccountedTurns = %d, want 2", got)
		}
		if got := batch["ISS-PARTIAL"].UnmeasuredSessions; got != 0 {
			t.Errorf("QueryTokenBudgetUsage()[ISS-PARTIAL].UnmeasuredSessions = %d, want 0: "+
				"an unaccounted turn is not an unmeasured session", got)
		}

		single, err := s.TokenUsageByIssue(context.Background(), "ISS-PARTIAL")
		if err != nil {
			t.Fatalf("TokenUsageByIssue() error = %v", err)
		}
		if single.UnaccountedTurns != 2 {
			t.Errorf("TokenUsageByIssue().UnaccountedTurns = %d, want 2", single.UnaccountedTurns)
		}
		if single.TotalTokens != 350 {
			t.Errorf("TokenUsageByIssue().TotalTokens = %d, want 350: an unaccounted turn adds no number",
				single.TotalTokens)
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
}

func TestTokenUsageByIssue_QueryTokenBudgetUsage_StoppedInFlightParity(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	const issueID = "ISS-STOPPED-PARITY"

	cost := func(v float64) *float64 { return &v }
	fixture := []struct {
		totalTokens, cacheReadTokens int64
		agentAdapter, status         string
		measured                     bool
		cost                         *float64
		costStatus                   string
	}{
		{totalTokens: 1500, cacheReadTokens: 900, agentAdapter: "claude", status: "budget_stopped", measured: true, cost: cost(0.5), costStatus: "estimated"},
		{totalTokens: 100, cacheReadTokens: 150, agentAdapter: "claude", status: "budget_stopped", measured: true},
		{totalTokens: 2500, cacheReadTokens: 1400, agentAdapter: "codex", status: "cancelled", measured: true, cost: cost(0.25), costStatus: "partial"},
		{agentAdapter: "mock", status: "succeeded"},
	}
	for i, row := range fixture {
		run := tokenRun(i+1, issueID, row.totalTokens)
		run.CacheReadTokens = row.cacheReadTokens
		run.AgentAdapter = row.agentAdapter
		run.Status = row.status
		run.TokensMeasured = row.measured
		run.EstimatedCostUSD, run.CostStatus = row.cost, row.costStatus
		appendOrFatal(t, s, run)
	}

	byIssue, err := s.TokenUsageByIssue(ctx, issueID)
	if err != nil {
		t.Fatalf("TokenUsageByIssue(%s) unexpected error: %v", issueID, err)
	}
	byBudget, err := s.QueryTokenBudgetUsage(ctx, []string{issueID})
	if err != nil {
		t.Fatalf("QueryTokenBudgetUsage unexpected error: %v", err)
	}
	budgetUsage, ok := byBudget[issueID]
	if !ok {
		t.Fatalf("QueryTokenBudgetUsage result = %v, want an entry for %s", byBudget, issueID)
	}

	want := IssueTokenUsage{
		TotalTokens:        4100,
		UncachedTokens:     1700,
		Sessions:           4,
		UnmeasuredSessions: 1,
		StoppedInFlight:    2,
		RecordedCostUSD:    0.75,
		PricedSessions:     2,
		PartialSessions:    1,
		UnpricedSessions:   1,
	}
	wantByIssue := want
	wantByIssue.CacheReadTokens = 2450
	if byIssue != wantByIssue {
		t.Errorf("TokenUsageByIssue(%s) = %+v, want %+v", issueID, byIssue, wantByIssue)
	}
	if budgetUsage != want {
		t.Errorf("QueryTokenBudgetUsage()[%s] = %+v, want %+v", issueID, budgetUsage, want)
	}
}

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

func countRun(i int, issueID, status, completedAt string) RunHistory {
	run := newTestRun(i)
	run.IssueID = issueID
	run.Status = status
	run.CompletedAt = completedAt
	return run
}

func mustParseRFC3339(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("time.Parse(RFC3339, %q): %v", s, err)
	}
	return parsed
}

func TestCountWorkerRunsCompletedSince(t *testing.T) {
	t.Parallel()

	const issueID = "ISS-CWR-1"
	since := mustParseRFC3339(t, "2026-04-01T09:00:00Z")

	s := openTestStore(t)
	migrateOrFatal(t, s)

	appendOrFatal(t, s, countRun(1, issueID, "succeeded", "2026-04-01T09:00:00Z"))
	appendOrFatal(t, s, countRun(2, issueID, "failed", "2026-04-01T10:00:00Z"))
	appendOrFatal(t, s, countRun(3, issueID, "cancelled", "2026-04-01T11:00:00Z"))
	appendOrFatal(t, s, countRun(4, issueID, "ci_failed", "2026-04-01T10:30:00Z"))
	appendOrFatal(t, s, countRun(5, issueID, "succeeded", "2026-03-31T23:59:59Z"))
	appendOrFatal(t, s, countRun(6, "ISS-CWR-OTHER", "succeeded", "2026-04-01T12:00:00Z"))

	got, err := s.CountWorkerRunsCompletedSince(context.Background(), issueID, since)
	if err != nil {
		t.Fatalf("CountWorkerRunsCompletedSince: %v", err)
	}
	if got != 3 {
		t.Errorf("CountWorkerRunsCompletedSince(%q, %v) = %d, want 3", issueID, since, got)
	}

	t.Run("issue with no rows returns zero", func(t *testing.T) {
		t.Parallel()

		s := openTestStore(t)
		migrateOrFatal(t, s)

		got, err := s.CountWorkerRunsCompletedSince(context.Background(), "ISS-CWR-NONE", since)
		if err != nil {
			t.Fatalf("CountWorkerRunsCompletedSince: %v", err)
		}
		if got != 0 {
			t.Errorf("CountWorkerRunsCompletedSince(%q, %v) = %d, want 0", "ISS-CWR-NONE", since, got)
		}
	})

	t.Run("needs_person row counts as a worker session", func(t *testing.T) {
		t.Parallel()

		const npIssueID = "ISS-CWR-NP"
		s := openTestStore(t)
		migrateOrFatal(t, s)

		appendOrFatal(t, s, countRun(1, npIssueID, "needs_person", "2026-04-01T09:30:00Z"))

		got, err := s.CountWorkerRunsCompletedSince(context.Background(), npIssueID, since)
		if err != nil {
			t.Fatalf("CountWorkerRunsCompletedSince: %v", err)
		}
		if got != 1 {
			t.Errorf("CountWorkerRunsCompletedSince(%q, %v) = %d, want 1 (a needs_person row is a worker session, not a CI verdict)", npIssueID, since, got)
		}
	})
}

func TestLoadLatestSuccessfulRunsForReactionRecovery_ExcludesNeedsPerson(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()
	cutoff := recoveryRefTime.Add(-30 * 24 * time.Hour)

	appendOrFatal(t, s, RunHistory{
		IssueID: "ISS-NEEDS-PERSON", Identifier: "PROJ-NP", Attempt: 1,
		AgentAdapter: "mock", Workspace: "/ws/PROJ-NP",
		StartedAt: recentCompletedAt(2), CompletedAt: recentCompletedAt(2),
		Status: "needs_person",
	})
	eligible := appendRecoveryRun(t, s, "ISS-OK-NP", "PROJ-OK-NP", 1, recentCompletedAt(2))

	got, err := s.LoadLatestSuccessfulRunsForReactionRecovery(ctx, cutoff, 200)
	if err != nil {
		t.Fatalf("LoadLatestSuccessfulRunsForReactionRecovery: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("LoadLatestSuccessfulRunsForReactionRecovery() len = %d, want 1", len(got))
	}
	if got[0].IssueID != eligible.IssueID {
		t.Errorf("LoadLatestSuccessfulRunsForReactionRecovery().IssueID = %q, want %q", got[0].IssueID, eligible.IssueID)
	}
}

func dispatchRun(i int, issueID, adapter string) RunHistory {
	run := RunHistory{
		IssueID: issueID, AgentAdapter: adapter, RuleName: fmt.Sprintf("rule-%d", i), ConfiguredModel: fmt.Sprintf("model-%d", i),
		StagePrevious: fmt.Sprintf("previous-%d", i), StagePreviousOutcome: fmt.Sprintf("outcome-%d", i),
		ChainID: fmt.Sprintf("chain-%d", i), DispatchedAt: fmt.Sprintf("2026-03-19T09:%02d:00Z", i),
	}
	if adapter == "" {
		run.Status = "ci_failed"
	}
	return run
}

func TestLatestAgentRuns(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		seed []RunHistory
		ask  []string
		want []int
	}{
		{
			name: "newest agent run per issue, skipping ci_failed, agentless, and unasked issues",
			seed: []RunHistory{
				dispatchRun(1, "ISS-A", "mock"),
				dispatchRun(2, "ISS-A", "mock"),
				dispatchRun(3, "ISS-A", ""),
				dispatchRun(4, "ISS-B", ""),
				dispatchRun(5, "ISS-E", "mock"),
				dispatchRun(6, "ISS-UNASKED", "mock"),
			},
			ask:  []string{"ISS-A", "ISS-B", "ISS-E", "ISS-NO-ROWS"},
			want: []int{1, 4},
		},
		{
			name: "empty ask",
			seed: []RunHistory{dispatchRun(1, "ISS-A", "mock")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := openTestStore(t)
			migrateOrFatal(t, s)
			appended := make([]RunHistory, len(tt.seed))
			for i, run := range tt.seed {
				appended[i] = appendOrFatal(t, s, run)
			}
			want := map[string]RunHistory{}
			for _, i := range tt.want {
				want[appended[i].IssueID] = appended[i]
			}

			got, err := s.LatestAgentRuns(context.Background(), tt.ask)

			if err != nil || !reflect.DeepEqual(got, want) {
				t.Errorf("LatestAgentRuns(%v) = %+v, %v, want %+v, nil", tt.ask, got, err, want)
			}
		})
	}
}

func TestLoadLatestSuccessfulRunsForIssues(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()

	old := appendRecoveryRun(t, s, "ISS-OLD", "PROJ-1", 1, recentCompletedAt(400))
	appendRecoveryRun(t, s, "ISS-LATER", "PROJ-2", 1, recentCompletedAt(5))
	latest := appendRecoveryRun(t, s, "ISS-LATER", "PROJ-2", 2, recentCompletedAt(4))
	appendOrFatal(t, s, RunHistory{
		IssueID: "ISS-LATER", Identifier: "PROJ-2", Attempt: 3,
		AgentAdapter: "mock", Workspace: "/ws/PROJ-2",
		StartedAt: recentCompletedAt(3), CompletedAt: recentCompletedAt(3), Status: "failed",
	})
	appendOrFatal(t, s, RunHistory{
		IssueID: "ISS-NOPATH", Identifier: "PROJ-3", Attempt: 1,
		AgentAdapter: "mock", StartedAt: recentCompletedAt(2), CompletedAt: recentCompletedAt(2), Status: "succeeded",
	})
	appendRecoveryRun(t, s, "ISS-OTHER", "PROJ-4", 1, recentCompletedAt(2))

	tests := []struct {
		name     string
		issueIDs []string
		want     []int64
	}{
		{"run older than any recovery lookback", []string{"ISS-OLD"}, []int64{old.ID}},
		{"latest successful run beats a later failed one", []string{"ISS-LATER"}, []int64{latest.ID}},
		{"empty workspace and unrequested issues are absent", []string{"ISS-NOPATH", "ISS-OLD"}, []int64{old.ID}},
		{"no issues", nil, []int64{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rows, err := s.LoadLatestSuccessfulRunsForIssues(ctx, tt.issueIDs)
			if err != nil {
				t.Fatalf("LoadLatestSuccessfulRunsForIssues(%v): %v", tt.issueIDs, err)
			}

			got := []int64{}
			for _, row := range rows {
				got = append(got, row.ID)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("LoadLatestSuccessfulRunsForIssues(%v) run ids = %v, want %v", tt.issueIDs, got, tt.want)
			}
		})
	}
}

func TestUsageCompletedBetween(t *testing.T) {
	t.Parallel()

	since := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	until := since.AddDate(0, 0, 1)
	s := openTestStore(t)
	migrateOrFatal(t, s)
	for i, completedAt := range []string{"2026-03-31T23:59:59Z", "2026-04-01T00:00:00Z", "2026-04-01T23:59:59Z", "2026-04-02T00:00:00Z"} {
		run := tokenRun(i+1, "ISS-WINDOW", 100)
		run.TokensMeasured, run.EstimatedCostUSD, run.CostStatus, run.CompletedAt = true, new(float64(int(1)<<i)), "estimated", completedAt
		appendOrFatal(t, s, run)
	}
	inWindow := IssueTokenUsage{TotalTokens: 200, UncachedTokens: 200, Sessions: 2, RecordedCostUSD: 6, PricedSessions: 2}

	tests := []struct {
		name         string
		since, until time.Time
		want         IssueTokenUsage
	}{
		{"includes since and excludes until", since, until, inWindow},
		{"binds bounds given in another zone as UTC", since.In(time.FixedZone("UTC+2", 2*60*60)), until.In(time.FixedZone("UTC+2", 2*60*60)), inWindow},
		{"empty window returns zero usage", until.Add(time.Hour), until.Add(2 * time.Hour), IssueTokenUsage{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := s.UsageCompletedBetween(context.Background(), tt.since, tt.until)
			if err != nil {
				t.Fatalf("UsageCompletedBetween(%v, %v): %v", tt.since, tt.until, err)
			}

			if got != tt.want {
				t.Errorf("UsageCompletedBetween(%v, %v) = %+v, want %+v", tt.since, tt.until, got, tt.want)
			}
		})
	}
}

func queryUsageRecord(t *testing.T, s *Store, runID int64) (columns, amounts string) {
	t.Helper()
	err := s.db.QueryRowContext(context.Background(),
		`SELECT quote(usage_provenance) || ',' || quote(usage_completeness),
			(SELECT COALESCE(group_concat(unit || '/' || model || '/' || amount || '/' || lower_bound, ';'), '')
			   FROM (SELECT * FROM run_reported_amounts WHERE run_id = ? ORDER BY unit, model))
		 FROM run_history WHERE id = ?`, runID, runID,
	).Scan(&columns, &amounts)
	if err != nil {
		t.Fatalf("select usage record of run %d: %v", runID, err)
	}
	return columns, amounts
}

func TestRunHistoryUsageProvenance_RowBeforeMigrationReadsNull(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateToVersion(t, s, 49)
	insertRunHistoryRow(t, s, "ISS-OLD", "succeeded", true)
	insertRunHistoryRow(t, s, "ISS-OLD-UNCOUNTED", "succeeded", true)
	insertRunHistoryRow(t, s, "ISS-OLD-UNMEASURED", "succeeded", false)
	if _, err := s.db.ExecContext(context.Background(), "UPDATE run_history SET unaccounted_turns = 2 WHERE issue_id = 'ISS-OLD-UNCOUNTED'"); err != nil {
		t.Fatalf("set unaccounted_turns: %v", err)
	}

	migrateOrFatal(t, s)

	columns, amounts := queryUsageRecord(t, s, 1)
	if columns != "NULL,NULL" || amounts != "" {
		t.Errorf("usage record of a row written before the migration = %q, %q, want %q, %q", columns, amounts, "NULL,NULL", "")
	}
	wantCompleteness := map[string]string{
		"ISS-OLD":            UsageComplete,
		"ISS-OLD-UNCOUNTED":  UsageLowerBound,
		"ISS-OLD-UNMEASURED": UsageUnmeasured,
	}
	for _, run := range queryAllRuns(t, s, 10) {
		if run.UsageCompleteness != "" || run.UsageProvenance != "" {
			t.Errorf("QueryRunHistory(%s) UsageCompleteness, UsageProvenance = %q, %q, want empty", run.IssueID, run.UsageCompleteness, run.UsageProvenance)
		}
		if got := run.Completeness(); got != wantCompleteness[run.IssueID] {
			t.Errorf("QueryRunHistory(%s).Completeness() = %q, want %q", run.IssueID, got, wantCompleteness[run.IssueID])
		}
	}
}

func TestAppendRunHistoryWithEvents_WritesProvenanceAndReportedAmounts(t *testing.T) {
	t.Parallel()

	recorded := []ReportedAmount{{Unit: "USD", Model: "m1", Amount: 1.5, LowerBound: true}, {Unit: "credits", Amount: 2}}
	const recordedRows = "USD/m1/1.5/1;credits//2.0/0"
	tests := []struct {
		name, completeness, provenance, wantColumns, wantAmounts string
		amounts                                                  []ReportedAmount
	}{
		{
			name: "recorded run", completeness: UsageLowerBound, provenance: "journal:records,output:live", amounts: recorded,
			wantColumns: "'journal:records,output:live','lower_bound'", wantAmounts: recordedRows,
		},
		{
			name: "unmeasured run keeps its amounts and an empty provenance", completeness: UsageUnmeasured, amounts: recorded,
			wantColumns: "'','unmeasured'", wantAmounts: recordedRows,
		},
		{name: "writer that sets neither field writes NULL", wantColumns: "NULL,NULL"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := openTestStore(t)
			migrateOrFatal(t, s)
			run := newTestRun(1)
			run.UsageCompleteness, run.UsageProvenance, run.ReportedAmounts = tt.completeness, tt.provenance, tt.amounts

			row := appendWithPartsOrFatal(t, s, run)

			columns, amounts := queryUsageRecord(t, s, row.ID)
			if columns != tt.wantColumns || amounts != tt.wantAmounts {
				t.Errorf("usage record = %q, %q, want %q, %q", columns, amounts, tt.wantColumns, tt.wantAmounts)
			}
			if read := queryAllRuns(t, s, 1)[0]; read.UsageCompleteness != tt.completeness || read.UsageProvenance != tt.provenance {
				t.Errorf("QueryRunHistory UsageCompleteness, UsageProvenance = %q, %q, want %q, %q", read.UsageCompleteness, read.UsageProvenance, tt.completeness, tt.provenance)
			}
		})
	}
}

func TestAppendRunHistoryWithEvents_FailedReportedAmountInsertLeavesNoRun(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	run := newTestRun(1)
	run.DispatchID = new("dispatch-1")
	run.UsageCompleteness = UsageComplete
	run.ReportedAmounts = []ReportedAmount{{Unit: "USD", Model: "m1", Amount: 1}, {Unit: "USD", Model: "m1", Amount: 2}}

	_, err := s.AppendRunHistoryWithEvents(context.Background(), run, nil, unboundedRunEvents)

	if err == nil {
		t.Fatal("AppendRunHistoryWithEvents(duplicate amount key) = nil, want error")
	}
	if rows := queryAllRuns(t, s, 10); len(rows) != 0 {
		t.Errorf("run rows after a failed amount insert = %d, want 0", len(rows))
	}
}
