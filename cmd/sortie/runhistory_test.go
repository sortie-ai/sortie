package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/orchestrator"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/server"
)

type runRef struct {
	RunID int64 `json:"run_id"`
}

type runsHarness struct {
	store *persistence.Store
	url   string
}

func newRunsHarness(t *testing.T, snap orchestrator.RuntimeSnapshotResult) *runsHarness {
	t.Helper()
	store, err := persistence.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatalf("persistence.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(t.Context()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	srv := server.New(server.Params{
		SnapshotFn: func() (orchestrator.RuntimeSnapshotResult, error) { return snap, nil },
		RefreshFn:  func() bool { return true },
		RunHistory: runHistoryReader{store: store},
	})
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return &runsHarness{store: store, url: ts.URL}
}

func (h *runsHarness) append(t *testing.T, run persistence.RunHistory) int64 {
	t.Helper()
	got, err := h.store.AppendRunHistory(t.Context(), run)
	if err != nil {
		t.Fatalf("AppendRunHistory(%q): %v", run.Identifier, err)
	}
	return got.ID
}

func (h *runsHarness) appendWithUsage(t *testing.T, run persistence.RunHistory, dispatchID string) {
	t.Helper()
	run.DispatchID = &dispatchID
	if _, err := h.store.AppendRunHistoryWithEvents(t.Context(), run, nil, persistence.RunEventLimits{}); err != nil {
		t.Fatalf("AppendRunHistoryWithEvents(%q): %v", run.Identifier, err)
	}
}

func (h *runsHarness) get(t *testing.T, route string, out any) int {
	t.Helper()
	resp, err := http.Get(h.url + route)
	if err != nil {
		t.Fatalf("GET %s: %v", route, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_ = json.NewDecoder(resp.Body).Decode(out)
	return resp.StatusCode
}

func (h *runsHarness) drain(t *testing.T, query string, afterFirstPage func()) (ids []int64, moreAfter []bool) {
	t.Helper()
	cursor := ""
	for range 10 {
		var body struct {
			Runs       []runRef `json:"runs"`
			NextCursor *string  `json:"next_cursor"`
		}
		route := "/api/v1/runs?" + query + cursor
		if status := h.get(t, route, &body); status != http.StatusOK {
			t.Fatalf("GET %s = %d, want %d", route, status, http.StatusOK)
		}
		for _, run := range body.Runs {
			ids = append(ids, run.RunID)
		}
		moreAfter = append(moreAfter, body.NextCursor != nil)
		if body.NextCursor == nil {
			return ids, moreAfter
		}
		cursor = "&cursor=" + *body.NextCursor
		if afterFirstPage != nil && len(moreAfter) == 1 {
			afterFirstPage()
		}
	}
	t.Fatalf("GET /api/v1/runs?%s: next_cursor never ended", query)
	return nil, nil
}

func (h *runsHarness) recordOutcome(t *testing.T, outcome persistence.IssueOutcome) {
	t.Helper()
	if err := h.store.RecordIssueOutcome(t.Context(), outcome); err != nil {
		t.Fatalf("RecordIssueOutcome(%q): %v", outcome.IssueID, err)
	}
}

func issueOutcomeMember(t *testing.T, where string, row map[string]any) any {
	t.Helper()
	member, ok := row["issue_outcome"]
	if !ok {
		t.Fatalf("%s has no issue_outcome member, want it always present", where)
	}
	return member
}

func runRow(issueID, identifier string) persistence.RunHistory {
	return persistence.RunHistory{IssueID: issueID, Identifier: identifier, AgentAdapter: "claude-code", Status: "succeeded", RuleName: "implement", CompletedAt: "2026-03-01T09:00:00Z", TokensMeasured: true}
}

func TestRunHistoryPagesWithoutGapsOrRepeats(t *testing.T) {
	t.Parallel()

	h := newRunsHarness(t, orchestrator.RuntimeSnapshotResult{})
	var newestFirst []int64
	for range 5 {
		newestFirst = slices.Insert(newestFirst, 0, h.append(t, runRow("i-0", "OTHER-0")))
	}
	var appended int64

	ids, moreAfter := h.drain(t, "limit=2", func() { appended = h.append(t, runRow("i-0", "OTHER-0")) })
	fresh, _ := h.drain(t, "limit=2", nil)

	if got, want := fmt.Sprint(ids, moreAfter), fmt.Sprint(newestFirst, []bool{true, true, false}); got != want {
		t.Errorf("run IDs and next_cursor set per page = %s, want %s", got, want)
	}
	if fresh[0] != appended {
		t.Errorf("fresh first page starts with run %d, want %d", fresh[0], appended)
	}
}

func TestRunHistoryFilters(t *testing.T) {
	t.Parallel()

	const ten, twelve = "2026-03-01T10:00:00Z", "2026-03-01T12:00:00Z"
	tests := []struct {
		name, query, field, hit, miss string
	}{
		{"issue", "issue=PROJ-1", "Identifier", "PROJ-1", "PROJ-9"},
		{"issue_id", "issue_id=i-1", "IssueID", "i-1", "i-9"},
		{"rule", "rule=review", "RuleName", "review", "implement"},
		{"rule none", "rule=%3Cnone%3E", "RuleName", "", "implement"},
		{"status", "status=failed", "Status", "failed", "succeeded"},
		{"stage", "stage=review", "StageTarget", "review", "deploy"},
		{"since includes its bound", "since=" + ten, "CompletedAt", ten, "2026-03-01T09:59:59Z"},
		{"until excludes its bound", "until=" + twelve, "CompletedAt", "2026-03-01T11:59:59Z", twelve},
		{"since with until", "since=" + ten + "&until=" + twelve, "CompletedAt", ten, twelve},
		{"ended_since selects by the ending of the issue", "ended_since=2026-03-02T00:00:00Z", "IssueID", "i-in", "i-none"},
		{"ended_until excludes a later ending", "ended_until=2026-03-03T00:00:00Z", "IssueID", "i-in", "i-late"},
	}
	endedAt := map[string]string{"i-in": "2026-03-02T10:00:00Z", "i-late": "2026-03-04T10:00:00Z"}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newRunsHarness(t, orchestrator.RuntimeSnapshotResult{})
			seed := func(value string) int64 {
				run := runRow("i-0", "OTHER-0")
				reflect.ValueOf(&run).Elem().FieldByName(tt.field).SetString(value)
				return h.append(t, run)
			}
			for _, issueID := range []string{tt.hit, tt.miss} {
				if at, ended := endedAt[issueID]; ended {
					h.recordOutcome(t, persistence.IssueOutcome{IssueID: issueID, Kind: persistence.IssueOutcomeTerminal, ObservedAt: at, State: "Done"})
				}
			}
			seed(tt.miss)
			older := seed(tt.hit)
			newer := seed(tt.hit)
			seed(tt.miss)
			seed(tt.miss)

			ids, moreAfter := h.drain(t, tt.query+"&limit=1", nil)

			if got, want := fmt.Sprint(ids, moreAfter), fmt.Sprint([]int64{newer, older}, []bool{true, false}); got != want {
				t.Errorf("GET /api/v1/runs?%s&limit=1 run IDs and next_cursor set per page = %s, want %s", tt.query, got, want)
			}
		})
	}
}

func TestRunHistoryServesIssueOutcome(t *testing.T) {
	t.Parallel()

	const at = "2026-03-02T10:00:00Z"
	tests := []struct {
		name   string
		stored *persistence.IssueOutcome
		want   any
	}{
		{name: "none while the issue runs"},
		{
			name:   "handed off",
			stored: &persistence.IssueOutcome{Kind: persistence.IssueOutcomeHandedOff, ObservedAt: at, State: "In Review"},
			want: map[string]any{"kind": "handed_off", "observed_at": at, "state": "In Review",
				"status_text": "Handed off to In Review", "status_text_full": "Handed off: Sortie moved the issue to In Review."},
		},
		{
			name:   "terminal",
			stored: &persistence.IssueOutcome{Kind: persistence.IssueOutcomeTerminal, ObservedAt: at, State: "Done"},
			want: map[string]any{"kind": "terminal", "observed_at": at, "state": "Done",
				"status_text": "Reached Done", "status_text_full": "Reached the tracker state Done."},
		},
		{
			name:   "merged by auto-merge",
			stored: &persistence.IssueOutcome{Kind: persistence.IssueOutcomeMerged, ObservedAt: at, MergeReactionKind: "merge", PRNumber: 88, PRRepository: "acme/api"},
			want: map[string]any{"kind": "merged", "observed_at": at, "merge_path": "auto_merge", "pull_request": map[string]any{"number": 88.0, "repository": "acme/api"},
				"status_text": "Merged #88", "status_text_full": "Ended merged: auto-merge merged PR #88."},
		},
		{
			name:   "merged by merge completion",
			stored: &persistence.IssueOutcome{Kind: persistence.IssueOutcomeMerged, ObservedAt: at, MergeReactionKind: "merge-completion", PRNumber: 89, PRRepository: "acme/web"},
			want: map[string]any{"kind": "merged", "observed_at": at, "merge_path": "merge_completion", "pull_request": map[string]any{"number": 89.0, "repository": "acme/web"},
				"status_text": "Merged #89", "status_text_full": "Ended merged: merge completion saw PR #89 merge."},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newRunsHarness(t, orchestrator.RuntimeSnapshotResult{Running: []orchestrator.SnapshotRunningEntry{{IssueID: "i-1", Identifier: "PROJ-1"}}})
			h.append(t, runRow("i-1", "PROJ-1"))
			h.append(t, runRow("i-1", "PROJ-1"))
			h.append(t, runRow("i-2", "PROJ-2"))
			if tt.stored != nil {
				tt.stored.IssueID = "i-1"
				h.recordOutcome(t, *tt.stored)
			}

			var list struct{ Runs []map[string]any }
			h.get(t, "/api/v1/runs", &list)
			var detail struct{ History map[string]any }
			h.get(t, "/api/v1/PROJ-1", &detail)

			if len(list.Runs) != 3 || detail.History == nil {
				t.Fatalf("GET /api/v1/runs and /api/v1/PROJ-1 = %d runs, history %v, want 3 runs and a history", len(list.Runs), detail.History)
			}
			for _, run := range list.Runs {
				want := tt.want
				if run["issue_id"] == "i-2" {
					want = nil
				}
				if got := issueOutcomeMember(t, "GET /api/v1/runs row", run); !reflect.DeepEqual(got, want) {
					t.Errorf("GET /api/v1/runs row of %v issue_outcome = %v, want %v", run["issue_id"], got, want)
				}
			}
			if got := issueOutcomeMember(t, "GET /api/v1/PROJ-1 history", detail.History); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("GET /api/v1/PROJ-1 history.issue_outcome = %v, want %v", got, tt.want)
			}
			historyRuns, _ := detail.History["runs"].([]any)
			if len(historyRuns) != 2 {
				t.Fatalf("GET /api/v1/PROJ-1 history.runs = %d runs, want 2", len(historyRuns))
			}
			for _, entry := range historyRuns {
				row, _ := entry.(map[string]any)
				if got := issueOutcomeMember(t, "GET /api/v1/PROJ-1 history run", row); !reflect.DeepEqual(got, tt.want) {
					t.Errorf("GET /api/v1/PROJ-1 history run issue_outcome = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestRunHistoryServesCostAndOrigin(t *testing.T) {
	t.Parallel()

	tokens := func(input, output, total, cacheRead, cacheWrite any) map[string]any {
		return map[string]any{"input_tokens": input, "output_tokens": output, "total_tokens": total, "cache_read_tokens": cacheRead, "cache_write_tokens": cacheWrite}
	}
	measured := runRow("i-0", "OTHER-0")
	measured.EstimatedCostUSD, measured.CostStatus = new(0.010035), "estimated"
	measured.Trigger, measured.ReactionKind, measured.InputTokens, measured.OutputTokens, measured.TotalTokens, measured.CacheReadTokens, measured.CacheWriteTokens = new("ci_fix"), "ci", 1000, 500, 1500, 200, 100
	measured.DeclaredOutcome, measured.AgentText = "blocked", "Which one should the API expose?"
	measured.ModelUsage = map[string]persistence.ModelTokens{"m-a": {InputTokens: 1000, OutputTokens: 500, CacheReadTokens: 200, CacheWriteTokens: 100}}
	measured.ModelCosts = map[string]persistence.ModelCost{"m-a": {EstimatedCostUSD: new(0.010035), CostStatus: "partial", PriceSource: "agent", UnratedClasses: []string{"cache_write"}}}
	measured.UnaccountedTurns = 2
	unmeasured := runRow("i-0", "OTHER-0")
	unmeasured.TokensMeasured, unmeasured.CostStatus = false, "unmeasured"
	creditsOnly := unmeasured
	creditsOnly.UsageCompleteness = persistence.UsageUnmeasured
	creditsOnly.ReportedAmounts = []persistence.ReportedAmount{
		{Unit: "credits", Model: "m-b", Amount: 1.5, LowerBound: true},
		{Unit: "credits", Amount: 3},
		{Unit: "USD", Amount: 0.2},
	}

	h := newRunsHarness(t, orchestrator.RuntimeSnapshotResult{})
	h.appendWithUsage(t, measured, "d-1")
	h.append(t, unmeasured)
	h.appendWithUsage(t, creditsOnly, "d-2")
	var body struct{ Runs []map[string]any }
	if status := h.get(t, "/api/v1/runs", &body); status != http.StatusOK || len(body.Runs) != 3 {
		t.Fatalf("GET /api/v1/runs = %d with %d runs, want %d with 3", status, len(body.Runs), http.StatusOK)
	}

	noTokens := tokens(nil, nil, nil, nil, nil)
	wants := []map[string]any{
		{"tokens": noTokens, "estimated_cost_usd": nil, "cost_status": "unmeasured", "model_usage": []any{}, "usage_completeness": "unmeasured", "usage_provenance": []any{},
			"vendor_amounts": []any{
				map[string]any{"unit": "credits", "model": "", "amount": 3.0, "lower_bound": false},
				map[string]any{"unit": "credits", "model": "m-b", "amount": 1.5, "lower_bound": true},
			}, "unmeasured_cause": "amounts_only", "unmeasured_units": []any{"USD", "credits"}},
		{"tokens": noTokens, "estimated_cost_usd": nil, "cost_status": "unmeasured", "model_usage": []any{}, "trigger": nil, "reaction_kind": "", "reaction_config_key": "", "declared_outcome": nil, "agent_text": nil,
			"usage_completeness": "unmeasured", "usage_provenance": nil, "vendor_amounts": nil, "unmeasured_cause": "unknown", "unmeasured_units": []any{}},
		{"tokens": tokens(1000.0, 500.0, 1500.0, 200.0, 100.0), "estimated_cost_usd": 0.010035, "cost_status": "estimated", "trigger": "ci_fix",
			"model_usage": []any{map[string]any{
				"model": "m-a", "tokens": tokens(1000.0, 500.0, 1500.0, 200.0, 100.0), "uncached_tokens": 1300.0,
				"estimated_cost_usd": 0.010035, "cost_status": "partial", "unrated_classes": []any{"cache_write"}, "price_source": "agent",
			}}, "usage_completeness": "lower_bound", "usage_provenance": nil, "vendor_amounts": nil, "unmeasured_cause": nil, "unmeasured_units": []any{},
			"reaction_kind": "ci", "reaction_config_key": "ci_failure", "declared_outcome": "blocked", "agent_text": "Which one should the API expose?"},
	}
	for i, want := range wants {
		for member, wantValue := range want {
			if got := body.Runs[i][member]; !reflect.DeepEqual(got, wantValue) {
				t.Errorf("GET /api/v1/runs runs[%d].%s = %v, want %v", i, member, got, wantValue)
			}
		}
	}

	var detail struct {
		History struct{ Totals map[string]any }
	}
	h.get(t, "/api/v1/OTHER-0", &detail)
	wantTotals := map[string]any{
		"unmeasured_cause": "mixed", "unmeasured_units": []any{}, "agent_priced_usd": 0.010035, "agent_priced_models": []any{"m-a"},
		"vendor_amounts": []any{map[string]any{"unit": "credits", "amount": 4.5, "lower_bound": true}},
	}
	for member, wantValue := range wantTotals {
		if got := detail.History.Totals[member]; !reflect.DeepEqual(got, wantValue) {
			t.Errorf("GET /api/v1/OTHER-0 history.totals.%s = %v, want %v", member, got, wantValue)
		}
	}
}

func TestRunHistoryFinishedIssueResolvesByOlderIdentifier(t *testing.T) {
	t.Parallel()

	running := orchestrator.RuntimeSnapshotResult{Running: []orchestrator.SnapshotRunningEntry{{IssueID: "i-1", Identifier: "PROJ-2", IssueTitle: "Fix login"}}}
	tests := []struct {
		name, route, want string
		snap              orchestrator.RuntimeSnapshotResult
	}{
		{name: "nothing live", route: "/api/v1/PROJ-1", want: "200|history||PROJ-2|Fix login"},
		{name: "live under the newer identifier", route: "/api/v1/PROJ-1", snap: running, want: "200|running||PROJ-2|Fix login"},
		{name: "no run and no live entry", route: "/api/v1/NOPE-1", want: "404||issue_not_found||"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newRunsHarness(t, tt.snap)
			hopRun := runRow("i-1", "PROJ-1")
			hopRun.StageTarget, hopRun.StageResult = "review", "advanced"
			newestRun := runRow("i-1", "PROJ-2")
			newestRun.IssueTitle = "Fix login"
			oldest := h.append(t, runRow("i-1", "PROJ-1"))
			h.append(t, runRow("i-2", "PROJ-7"))
			hop := h.append(t, hopRun)
			newest := h.append(t, newestRun)

			var got struct {
				IssueIdentifier string `json:"issue_identifier"`
				Status          string
				Issue           struct{ Title string }
				History         *struct {
					Runs   []runRef
					Totals struct {
						RunCount int `json:"run_count"`
					}
					Hops []struct {
						runRef
						TargetRule string `json:"target_rule"`
					}
				}
				Error struct{ Code string }
			}
			status := h.get(t, tt.route, &got)

			gotSummary := strings.Join([]string{strconv.Itoa(status), got.Status, got.Error.Code, got.IssueIdentifier, got.Issue.Title}, "|")
			if gotSummary != tt.want {
				t.Fatalf("GET %s status|status|error|issue_identifier|title = %q, want %q", tt.route, gotSummary, tt.want)
			}
			if status == http.StatusNotFound {
				return
			}
			gotHistory := fmt.Sprint(got.History.Runs, got.History.Totals.RunCount, got.History.Hops)
			if wantHistory := fmt.Sprintf("[{%d} {%d} {%d}] 3 [{{%d} review}]", newest, hop, oldest, hop); gotHistory != wantHistory {
				t.Errorf("GET %s history runs, run_count, hops = %s, want %s", tt.route, gotHistory, wantHistory)
			}
		})
	}
}
