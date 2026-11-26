package runstats

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/storage"
)

var fullCaps = storage.RunHistoryCapabilities{
	HasTurnsCompleted:   true,
	HasReviewMetadata:   true,
	HasRuleRouting:      true,
	HasTokens:           true,
	HasTokenMeasurement: true,
	HasAgentBlocked:     true,
	HasRecordedCost:     true,
}

var recordedCaps = withTriggerBreakdown(fullCaps)

var fixedNow = time.Date(2026, 8, 7, 9, 15, 0, 0, time.UTC)

func withCost(row storage.RunStatsRow, usd *float64, costStatus string) storage.RunStatsRow {
	row.EstimatedCostUSD, row.CostStatus = usd, costStatus
	return row
}

func rangeFloats(lo, hi int) []float64 {
	out := make([]float64, 0, hi-lo+1)
	for i := lo; i <= hi; i++ {
		out = append(out, float64(i))
	}
	return out
}

func addRows(t *testing.T, agg *Aggregator, rows ...storage.RunStatsRow) {
	t.Helper()
	for _, r := range rows {
		if err := agg.Add(r); err != nil {
			t.Fatalf("Aggregator.Add(%+v): %v", r, err)
		}
	}
}

func findGroup(t *testing.T, groups []Group, name string) Group {
	t.Helper()
	for _, g := range groups {
		if g.Name == name {
			return g
		}
	}
	t.Fatalf("group %q not found in %+v", name, groups)
	return Group{}
}

func reviewMetaPtr(t *testing.T, m domain.ReviewMetadata) *string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("json.Marshal(%+v): %v", m, err)
	}
	return new(string(b))
}

func withModelBreakdown(caps storage.RunHistoryCapabilities) storage.RunHistoryCapabilities {
	caps.HasConfiguredSettings = true
	return caps
}

func withTriggerBreakdown(caps storage.RunHistoryCapabilities) storage.RunHistoryCapabilities {
	caps = withModelBreakdown(caps)
	caps.HasStageChains = true
	caps.HasTrigger = true
	caps.HasFirstAfterHop = true
	return caps
}

func triggerRow(issueID string, trigger *string) storage.RunStatsRow {
	row := modelRow("claude-code", "model-a", "succeeded", 0)
	row.IssueID, row.Trigger = issueID, trigger
	return row
}

func reportedModelRow(configured, reported string) storage.RunStatsRow {
	row := modelRow("claude-code", configured, "succeeded", 0)
	row.ReportedModel = reported
	return row
}

func modelRow(adapter, model, status string, input int64) storage.RunStatsRow {
	return storage.RunStatsRow{
		Status: status, AgentAdapter: adapter, ConfiguredModel: model,
		StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T00:10:00Z",
		InputTokens: input, TokensMeasured: true,
	}
}

func chainRow(chain, rule, previous, result, status string) storage.RunStatsRow {
	row := modelRow("claude-code", "model-a", status, 1_000)
	row.Identifier, row.ChainID, row.RuleName, row.StagePrevious, row.StageResult = "PROJ-"+chain, chain, rule, previous, result
	row.TurnsCompleted = 2
	return row
}

func sumRuns(groups []Group) int {
	var runs int
	for _, g := range groups {
		runs += g.Runs
	}
	return runs
}

func derefCost(usd *float64) any {
	if usd == nil {
		return nil
	}
	return *usd
}

func withUsageProvenance(caps storage.RunHistoryCapabilities) storage.RunHistoryCapabilities {
	caps.HasUsageProvenance = true
	return caps
}

func measuredRow(completeness string) storage.RunStatsRow {
	row := withCost(modelRow("claude-code", "model-a", "succeeded", 1_000), new(1.0), "estimated")
	row.UsageCompleteness = completeness
	return row
}

func unmeasuredRow(status, completeness string, units ...string) storage.RunStatsRow {
	row := modelRow("claude-code", "model-a", status, 0)
	row.TokensMeasured, row.UsageCompleteness, row.ReportedUnits = false, completeness, units
	return row
}

func withParts(row storage.RunStatsRow, parts ...storage.RunModelPart) storage.RunStatsRow {
	row.Parts = parts
	return row
}

func pricedPart(model, priceSource string, input int64, usd float64) storage.RunModelPart {
	return storage.RunModelPart{
		Model: model, Tokens: storage.ModelTokens{InputTokens: input},
		EstimatedCostUSD: &usd, CostStatus: "estimated", PriceSource: priceSource,
	}
}

func TestStatsSummaryFormulas(t *testing.T) {
	t.Parallel()

	t.Run("percentile and mean over the worked sample sets", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name     string
			samples  []float64
			wantP50  float64
			wantP95  float64
			wantMean float64
		}{
			{name: "single sample", samples: []float64{7}, wantP50: 7, wantP95: 7, wantMean: 7},
			{name: "four samples", samples: []float64{10, 20, 30, 40}, wantP50: 20, wantP95: 40, wantMean: 25},
			{name: "twenty samples", samples: rangeFloats(1, 20), wantP50: 10, wantP95: 19, wantMean: 10.5},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				got := durationStats(tt.samples)

				if got.Samples != len(tt.samples) {
					t.Fatalf("durationStats(%v).Samples = %d, want %d", tt.samples, got.Samples, len(tt.samples))
				}
				if got.P50 == nil || *got.P50 != tt.wantP50 {
					t.Errorf("durationStats(%v).P50 = %v, want %v", tt.samples, got.P50, tt.wantP50)
				}
				if got.P95 == nil || *got.P95 != tt.wantP95 {
					t.Errorf("durationStats(%v).P95 = %v, want %v", tt.samples, got.P95, tt.wantP95)
				}
				if got.Mean == nil || *got.Mean != tt.wantMean {
					t.Errorf("durationStats(%v).Mean = %v, want %v", tt.samples, got.Mean, tt.wantMean)
				}
			})
		}
	})

	t.Run("duration and turns normalize on succeeded rows; cost per succeeded run divides all spend", func(t *testing.T) {
		t.Parallel()

		agg := New(recordedCaps)

		addRows(t, agg,
			withCost(storage.RunStatsRow{
				Status: "succeeded", AgentAdapter: "mock", TurnsCompleted: 4,
				StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T00:10:00Z",
				InputTokens: 1_000_000, TokensMeasured: true,
			}, new(10.0), "estimated"),
			withCost(storage.RunStatsRow{
				Status: "failed", AgentAdapter: "mock", TurnsCompleted: 8,
				StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T01:00:00Z",
				InputTokens: 500_000, TokensMeasured: true,
			}, new(5.0), "estimated"),
		)

		report := agg.Report(fixedNow, "/wf", "/db", nil, nil)

		if report.Summary.Duration.Samples != 1 {
			t.Fatalf("Summary.Duration.Samples = %d, want 1 (succeeded rows only)", report.Summary.Duration.Samples)
		}
		if got, want := *report.Summary.Duration.P50, 600.0; got != want {
			t.Errorf("Summary.Duration.P50 = %v, want %v", got, want)
		}
		if report.Summary.MeanTurnsSucceeded == nil || *report.Summary.MeanTurnsSucceeded != 4 {
			t.Errorf("Summary.MeanTurnsSucceeded = %v, want 4 (succeeded rows only)", report.Summary.MeanTurnsSucceeded)
		}
		if report.Summary.CostUSD == nil || *report.Summary.CostUSD != 15 {
			t.Fatalf("Summary.CostUSD = %v, want 15 (all rows: $10 + $5)", report.Summary.CostUSD)
		}
		if report.Summary.CostPerSucceededRunUSD == nil || *report.Summary.CostPerSucceededRunUSD != 15 {
			t.Errorf("Summary.CostPerSucceededRunUSD = %v, want 15 (cost_usd / succeeded, not succeeded-only spend / succeeded)",
				report.Summary.CostPerSucceededRunUSD)
		}
	})

	t.Run("a sub-cent cost per succeeded run stays above zero in the summary and a group", func(t *testing.T) {
		t.Parallel()

		agg := New(recordedCaps)
		addRows(t, agg, withCost(modelRow("mock", "model-a", "succeeded", 1_000), new(0.004), "estimated"))

		report := agg.Report(fixedNow, "/wf", "/db", nil, nil)

		adapter := findGroup(t, report.ByAdapter, "mock")
		for name, got := range map[string]*float64{"Summary": report.Summary.CostPerSucceededRunUSD, "ByAdapter[mock]": adapter.CostPerSucceededRunUSD} {
			if got == nil || *got != 0.004 {
				t.Errorf("%s.CostPerSucceededRunUSD = %v, want 0.004", name, derefCost(got))
			}
		}
	})

	t.Run("lower-bound flags, unmeasured cause and agent-priced share", func(t *testing.T) {
		t.Parallel()

		type flags struct {
			lowerBoundRuns                   int
			tokensLowerBound, costLowerBound bool
			cause                            *string
			units                            []string
			agentPricedUSD                   *float64
		}
		none := []string{}
		complete := measuredRow("complete")
		unaccounted := measuredRow("lower_bound")
		unaccounted.UnaccountedTurns = 1
		unpriced := withCost(measuredRow("complete"), nil, "unpriced")
		partial := withCost(measuredRow("complete"), new(1.0), "partial")
		twoParts := withParts(withCost(measuredRow("complete"), new(5.0), "estimated"), pricedPart("model-a", "agent", 600, 2.0), pricedPart("model-b", "operator", 400, 3.0))
		ciFailed := withParts(measuredRow("complete"), pricedPart("model-a", "agent", 1_000, 1.0))
		ciFailed.Status = "ci_failed"

		tests := []struct {
			name string
			rows []storage.RunStatsRow
			want flags
		}{
			{"complete runs only", []storage.RunStatsRow{complete}, flags{units: none}},
			{"unaccounted turns flag tokens, not cost", []storage.RunStatsRow{complete, unaccounted}, flags{lowerBoundRuns: 1, tokensLowerBound: true, units: none}},
			{"an unpriced run flags cost", []storage.RunStatsRow{complete, unpriced}, flags{costLowerBound: true, units: none}},
			{"a partial cost flags cost", []storage.RunStatsRow{partial}, flags{costLowerBound: true, units: none}},
			{"reported units beat the status", []storage.RunStatsRow{complete, unmeasuredRow("failed", "unmeasured", "credits")},
				flags{tokensLowerBound: true, costLowerBound: true, cause: new("amounts_only"), units: []string{"credits"}}},
			{"different causes read mixed", []storage.RunStatsRow{complete, unmeasuredRow("failed", "unmeasured"), unmeasuredRow("succeeded", "unmeasured")},
				flags{tokensLowerBound: true, costLowerBound: true, cause: new("mixed"), units: none}},
			{"nothing measured leaves no figure to flag", []storage.RunStatsRow{unmeasuredRow("failed", "unmeasured")}, flags{cause: new("ended_early"), units: none}},
			{"a run recorded before provenance reads unknown", []storage.RunStatsRow{unmeasuredRow("succeeded", "")}, flags{cause: new("unknown"), units: none}},
			{"a two-part run counts its agent part once", []storage.RunStatsRow{twoParts}, flags{units: none, agentPricedUSD: new(2.0)}},
			{"a ci_failed run adds no agent part", []storage.RunStatsRow{complete, ciFailed}, flags{units: none}},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				agg := New(withUsageProvenance(recordedCaps))
				addRows(t, agg, tt.rows...)

				s := agg.Report(fixedNow, "/wf", "/db", nil, nil).Summary

				got := flags{lowerBoundRuns: s.TokensLowerBoundRuns, tokensLowerBound: s.TokensLowerBound, costLowerBound: s.CostLowerBound, cause: s.UnmeasuredCause, units: s.UnmeasuredUnits, agentPricedUSD: s.AgentPricedUSD}
				if !reflect.DeepEqual(got, tt.want) {
					t.Errorf("Summary lower-bound flags over %q = %+v, want %+v", tt.name, got, tt.want)
				}
				if s.AgentPricedUSD != nil && (s.CostUSD == nil || *s.AgentPricedUSD > *s.CostUSD) {
					t.Errorf("Summary.AgentPricedUSD = %v over %q, want at most cost_usd %v", *s.AgentPricedUSD, tt.name, derefCost(s.CostUSD))
				}
			})
		}
	})
}

func TestStatsGroupDisclosure(t *testing.T) {
	t.Parallel()

	agg := New(recordedCaps)

	addRows(t, agg, storage.RunStatsRow{
		Status:         "ci_failed",
		StartedAt:      "2026-01-01T00:00:00Z",
		CompletedAt:    "2026-01-01T00:00:00Z",
		TokensMeasured: true,
	})
	reviewRun := func(adapter string) storage.RunStatsRow {
		return storage.RunStatsRow{
			Status:         "succeeded",
			AgentAdapter:   adapter,
			RuleName:       "review",
			TemplateID:     "review",
			StartedAt:      "2026-01-01T00:00:00Z",
			CompletedAt:    "2026-01-01T00:01:00Z",
			InputTokens:    1_000_000,
			TotalTokens:    1_000_000,
			TokensMeasured: true,
		}
	}
	addRows(t, agg,
		withCost(reviewRun("mock"), new(10.0), "estimated"),
		withCost(reviewRun("unrated"), nil, "unpriced"),
	)

	report := agg.Report(fixedNow, "/wf", "/db", nil, nil)

	adapterGroup := findGroup(t, report.ByAdapter, "<none>")
	if adapterGroup.Duration.Samples != 1 {
		t.Errorf("<none> adapter group Duration.Samples = %d, want 1", adapterGroup.Duration.Samples)
	}
	if adapterGroup.Duration.P50 == nil || *adapterGroup.Duration.P50 != 0 {
		t.Errorf("<none> adapter group Duration.P50 = %v, want 0 (zero-duration sample)", adapterGroup.Duration.P50)
	}
	if adapterGroup.Tokens == nil || adapterGroup.Tokens.Total != 0 {
		t.Errorf("<none> adapter group Tokens = %v, want Total 0", adapterGroup.Tokens)
	}
	if adapterGroup.CostUSD != nil {
		t.Errorf("<none> adapter group CostUSD = %v, want nil (a ci_failed row is not priced)", *adapterGroup.CostUSD)
	}

	ruleGroup := findGroup(t, report.ByRule, "<none>")
	if ruleGroup.Runs != 1 {
		t.Errorf("<none> rule group Runs = %d, want 1", ruleGroup.Runs)
	}
	if ruleGroup.CostUnpricedRuns != 0 {
		t.Errorf("<none> rule group CostUnpricedRuns = %d, want 0 (a ci_failed row is never unpriced)", ruleGroup.CostUnpricedRuns)
	}
	pricedRuleGroup := findGroup(t, report.ByRule, "review")
	if pricedRuleGroup.CostUnpricedRuns != 1 {
		t.Errorf("review rule group CostUnpricedRuns = %d, want 1", pricedRuleGroup.CostUnpricedRuns)
	}
	if pricedRuleGroup.CostUSD == nil || *pricedRuleGroup.CostUSD != 10 {
		t.Errorf("review rule group CostUSD = %v, want 10 (the priced run only)", pricedRuleGroup.CostUSD)
	}
	templateGroup := findGroup(t, report.ByTemplate, "<none>")
	if templateGroup.Runs != 1 {
		t.Errorf("<none> template group Runs = %d, want 1", templateGroup.Runs)
	}

	if report.ByProvenance == nil || len(report.ByProvenance) != 0 {
		t.Errorf("Report.ByProvenance = %v, want [] without the provenance columns", report.ByProvenance)
	}

	if report.Summary.ZeroDurationRuns != 1 {
		t.Errorf("Summary.ZeroDurationRuns = %d, want 1", report.Summary.ZeroDurationRuns)
	}
	if report.Summary.CostUnpricedRuns != 1 {
		t.Errorf("Summary.CostUnpricedRuns = %d, want 1 (the unrated run; a ci_failed row records a CI verdict, not an agent session)", report.Summary.CostUnpricedRuns)
	}

	t.Run("the base tier serves empty unmeasured units", func(t *testing.T) {
		t.Parallel()

		agg := New(storage.RunHistoryCapabilities{})
		addRows(t, agg, modelRow("claude-code", "model-a", "succeeded", 1_000))

		report := agg.Report(fixedNow, "/wf", "/db", nil, nil)

		if group := findGroup(t, report.ByAdapter, "claude-code"); report.Summary.UnmeasuredUnits == nil || group.UnmeasuredUnits == nil {
			t.Errorf("base tier summary and group UnmeasuredUnits = %v, %v, want [] rather than nil", report.Summary.UnmeasuredUnits, group.UnmeasuredUnits)
		}
	})

	t.Run("by_provenance names every run once, live pair first", func(t *testing.T) {
		t.Parallel()

		agg := New(withUsageProvenance(recordedCaps))
		for _, stored := range [][2]string{{"", ""}, {"complete", ""}, {"complete", "journal:records,output:live"}, {"complete", "output:live,journal:records"}} {
			row := measuredRow(stored[0])
			row.UsageProvenance = stored[1]
			addRows(t, agg, row)
		}

		report := agg.Report(fixedNow, "/wf", "/db", nil, nil)

		got := map[string]int{}
		for _, g := range report.ByProvenance {
			got[g.Name] = g.Runs
		}
		want := map[string]int{"<unknown>": 1, "<none>": 1, "output:live,journal:records": 2}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Report.ByProvenance runs by name = %v, want %v", got, want)
		}
	})
}

func TestStatsSelfReview(t *testing.T) {
	t.Parallel()

	agg := New(fullCaps)
	addRows(t, agg,
		storage.RunStatsRow{
			Status: "succeeded", StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T00:01:00Z",
			ReviewMetadata: reviewMetaPtr(t, domain.ReviewMetadata{FinalVerdict: "pass", TotalIterations: 1}),
		},
		storage.RunStatsRow{
			Status: "failed", StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T00:01:00Z",
			ReviewMetadata: reviewMetaPtr(t, domain.ReviewMetadata{FinalVerdict: "iterate", TotalIterations: 3, CapReached: true}),
		},
		storage.RunStatsRow{
			Status: "failed", StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T00:01:00Z",
			ReviewMetadata: reviewMetaPtr(t, domain.ReviewMetadata{FinalVerdict: "", TotalIterations: 2}),
		},
		storage.RunStatsRow{
			Status: "failed", StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T00:01:00Z",
			ReviewMetadata: new("{not-json"),
		},
	)

	report := agg.Report(fixedNow, "/wf", "/db", nil, nil)

	sr := report.SelfReview
	if sr == nil {
		t.Fatal("SelfReview = nil, want non-nil on the full tier")
	}
	if sr.RunsWithMetadata != 3 {
		t.Errorf("RunsWithMetadata = %d, want 3", sr.RunsWithMetadata)
	}
	if sr.UnparsedRuns != 1 {
		t.Errorf("UnparsedRuns = %d, want 1", sr.UnparsedRuns)
	}
	if sr.CapReachedRuns != 1 {
		t.Errorf("CapReachedRuns = %d, want 1", sr.CapReachedRuns)
	}
	if sr.MeanIterations == nil || *sr.MeanIterations != 2 {
		t.Errorf("MeanIterations = %v, want 2 ((1+3+2)/3)", sr.MeanIterations)
	}

	wantVerdicts := map[string]int{"iterate": 1, "none": 1, "pass": 1}
	gotVerdicts := make(map[string]int, len(sr.ByFinalVerdict))
	var verdictOrder []string
	for _, vc := range sr.ByFinalVerdict {
		gotVerdicts[vc.Verdict] = vc.Runs
		verdictOrder = append(verdictOrder, vc.Verdict)
	}
	if !maps.Equal(gotVerdicts, wantVerdicts) {
		t.Errorf("ByFinalVerdict = %v, want %v", gotVerdicts, wantVerdicts)
	}
	if !slices.IsSorted(verdictOrder) {
		t.Errorf("ByFinalVerdict order = %v, want ascending by verdict", verdictOrder)
	}
}

func TestStatsStatusOrdering(t *testing.T) {
	t.Parallel()

	agg := New(fullCaps)
	for _, status := range []string{"weird_status", "succeeded", "succeeded", "failed", "cancelled"} {
		addRows(t, agg, storage.RunStatsRow{
			Status: status, StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T00:01:00Z",
		})
	}

	report := agg.Report(fixedNow, "/wf", "/db", nil, nil)

	// Runs tie at 1 for the last three; ties break ascending by name.
	want := []string{"succeeded", "cancelled", "failed", "weird_status"}
	var got []string
	for _, g := range report.ByStatus {
		got = append(got, g.Name)
	}
	if !slices.Equal(got, want) {
		t.Errorf("ByStatus order = %v, want %v", got, want)
	}

	weird := findGroup(t, report.ByStatus, "weird_status")
	if weird.Runs != 1 {
		t.Errorf("weird_status group Runs = %d, want 1 (status set must not be hard-coded)", weird.Runs)
	}
}

func TestStatsOutcomeCounts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		status string
		want   [4]int
	}{
		{status: "succeeded", want: [4]int{1, 0, 0, 0}},
		{status: "failed", want: [4]int{0, 1, 0, 0}},
		{status: "ci_failed", want: [4]int{0, 1, 0, 0}},
		{status: "needs_person", want: [4]int{0, 0, 1, 0}},
		{status: "budget_stopped", want: [4]int{0, 0, 1, 0}},
		{status: "cancelled", want: [4]int{0, 0, 0, 1}},
		{status: "weird_status", want: [4]int{0, 1, 0, 0}},
		{status: "", want: [4]int{0, 1, 0, 0}},
	}

	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			t.Parallel()

			for _, caps := range []storage.RunHistoryCapabilities{{}, fullCaps} {
				agg := New(caps)
				addRows(t, agg, storage.RunStatsRow{Status: tt.status})

				s := agg.Report(fixedNow, "/wf", "/db", nil, nil).Summary

				got := [4]int{s.Succeeded, s.Failed, s.Stopped, s.Cancelled}
				if got != tt.want || s.Runs != 1 {
					t.Errorf("Summary counts for status %q (caps %+v) = %v over %d runs, want %v over 1", tt.status, caps, got, s.Runs, tt.want)
				}
			}
		})
	}
}

func TestStatsAggregatorReport_UntilBound(t *testing.T) {
	t.Parallel()

	agg := New(fullCaps)
	until := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	report := agg.Report(fixedNow, "/wf", "/db", nil, &until)

	want := "2026-08-01T00:00:00Z"
	if report.Until == nil || *report.Until != want {
		t.Errorf("Aggregator.Report(until=%v).Until = %v, want %q", until, report.Until, want)
	}
}

func TestDegradedSchemaWarning(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		caps   storage.RunHistoryCapabilities
		want   []string
		absent []string
	}{
		{
			name: "nothing recorded names every group once",
			caps: storage.RunHistoryCapabilities{},
			want: []string{
				"before sortie recorded turns, self-review results, dispatch-rule routing, tokens and cost",
				"stage chains, what started each run, the first run after each stage hop, which runs required action",
				"Run sortie once with this workflow to add them.",
			},
			absent: []string{"does carry", "falls back"},
		},
		{
			name: "partly migrated discloses the figures it drops anyway",
			caps: storage.RunHistoryCapabilities{HasTurnsCompleted: true, HasReviewMetadata: true},
			want: []string{
				"before sortie recorded dispatch-rule routing, tokens and cost",
				"also leaves out turns, self-review results, which this database does carry",
			},
		},
		{
			name: "only tokens absent still discloses the other three",
			caps: storage.RunHistoryCapabilities{
				HasTurnsCompleted: true, HasReviewMetadata: true, HasRuleRouting: true,
			},
			want: []string{
				"before sortie recorded tokens and cost",
				"also leaves out turns, self-review results, dispatch-rule routing, which this database does carry",
			},
		},
		{
			name: "only the measurement record absent names it in plain language",
			caps: storage.RunHistoryCapabilities{
				HasTurnsCompleted: true, HasReviewMetadata: true, HasRuleRouting: true, HasTokens: true,
			},
			want: []string{
				"before sortie recorded which runs the coding agent could measure",
				"also leaves out turns, self-review results, dispatch-rule routing, tokens and cost, which this database does carry",
			},
			absent: []string{"tokens_measured", "run_history"},
		},
		{
			name: "configured model group is listed as unrecorded when absent",
			caps: storage.RunHistoryCapabilities{},
			want: []string{"before sortie recorded", "the model each run was configured with"},
		},
		{
			name:   "configured model group is listed among the carried ones when present",
			caps:   storage.RunHistoryCapabilities{HasTurnsCompleted: true, HasConfiguredSettings: true},
			want:   []string{"the model each run was configured with, which this database does carry"},
			absent: []string{"before sortie recorded turns"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := degradedSchemaWarning(tt.caps)
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("degradedSchemaWarning(%+v) = %q, want to contain %q", tt.caps, got, want)
				}
			}
			for _, absent := range tt.absent {
				if strings.Contains(got, absent) {
					t.Errorf("degradedSchemaWarning(%+v) = %q, want it not to contain %q", tt.caps, got, absent)
				}
			}
		})
	}
}

func TestStatsByModel(t *testing.T) {
	t.Parallel()

	agg := New(recordedCaps)
	addRows(t, agg,
		withCost(modelRow("claude-code", "model-a", "succeeded", 1_000_000), new(10.0), "estimated"),
		withCost(modelRow("claude-code", "model-a", "failed", 0), new(0.0), "estimated"),
		withCost(modelRow("codex", "model-b", "succeeded", 1_000_000), nil, "unpriced"),
		withCost(modelRow("claude-code", "", "succeeded", 1_000_000), new(10.0), "estimated"),
		reportedModelRow("", "model-a"),
		reportedModelRow("model-b", "model-a"),
	)

	report := agg.Report(fixedNow, "/wf", "/db", nil, nil)

	if got := sumRuns(report.ByModel); got != report.Summary.Runs {
		t.Errorf("by_model run counts sum to %d, want the report's runs: %d", got, report.Summary.Runs)
	}
	modelA, modelB, none := findGroup(t, report.ByModel, "model-a"), findGroup(t, report.ByModel, "model-b"), findGroup(t, report.ByModel, "<none>")
	if modelA.Runs != 4 || modelA.Succeeded != 3 || modelB.Runs != 1 || none.Runs != 1 {
		t.Errorf("model-a runs, succeeded, model-b runs and <none> runs = %d, %d, %d, %d, want 4, 3, 1, 1 (a reported model wins over the configured one)",
			modelA.Runs, modelA.Succeeded, modelB.Runs, none.Runs)
	}
	if modelA.CostUSD == nil || *modelA.CostUSD != 10 || modelB.CostUSD != nil {
		t.Errorf("model-a, model-b CostUSD = %v, %v, want 10 (the recorded cost) and nil (recorded unpriced)", modelA.CostUSD, modelB.CostUSD)
	}

	type share struct {
		runs, succeeded, unpriced, partial, lowerBound int
		input, output, total                           int64
		cost, agent                                    float64
	}
	part := func(model string, input, output int64, usd *float64, costStatus string) storage.RunModelPart {
		return storage.RunModelPart{
			Model: model, Tokens: storage.ModelTokens{InputTokens: input, OutputTokens: output},
			EstimatedCostUSD: usd, CostStatus: costStatus,
		}
	}
	split := func(unaccounted int, runCost *float64, runStatus string, parts ...storage.RunModelPart) storage.RunStatsRow {
		row := withCost(modelRow("claude-code", "run-model", "succeeded", 0), runCost, runStatus)
		row.Parts, row.UnaccountedTurns = parts, unaccounted
		for _, p := range parts {
			row.InputTokens += p.Tokens.InputTokens
			row.OutputTokens += p.Tokens.OutputTokens
			row.TotalTokens += p.Tokens.InputTokens + p.Tokens.OutputTokens
		}
		return row
	}
	withoutParts := withCost(reportedModelRow("model-d", "model-e"), new(1.0), "estimated")
	withoutParts.Status, withoutParts.InputTokens, withoutParts.TotalTokens = "failed", 100, 100

	tests := []struct {
		name string
		rows []storage.RunStatsRow
		want map[string]share
	}{
		{
			name: "a run priced per model lands in each spent part's row; a run without parts lands once under its reported model",
			rows: []storage.RunStatsRow{
				split(0, new(9.0), "estimated",
					part("model-a", 600, 100, new(6.0), "estimated"),
					part("model-b", 300, 50, new(3.0), "estimated"),
					part("model-z", 0, 0, new(0.0), "estimated")),
				withoutParts,
			},
			want: map[string]share{
				"model-a": {runs: 1, succeeded: 1, input: 600, output: 100, total: 700, cost: 6.0},
				"model-b": {runs: 1, succeeded: 1, input: 300, output: 50, total: 350, cost: 3.0},
				"model-e": {runs: 1, input: 100, total: 100, cost: 1.0},
			},
		},
		{
			name: "a part priced from an agent amount reaches only its own model row",
			rows: []storage.RunStatsRow{split(0, new(9.0), "estimated",
				pricedPart("model-a", "agent", 600, 6.0),
				pricedPart("model-b", "operator", 300, 3.0))},
			want: map[string]share{
				"model-a": {runs: 1, succeeded: 1, input: 600, total: 600, cost: 6.0, agent: 6.0},
				"model-b": {runs: 1, succeeded: 1, input: 300, total: 300, cost: 3.0},
			},
		},
		{
			name: "unaccounted turns mark the priced part partial and leave the unpriced part unpriced only",
			rows: []storage.RunStatsRow{split(2, new(4.0), "partial",
				part("model-a", 100, 10, new(4.0), "estimated"),
				part("model-b", 200, 20, nil, "unpriced"))},
			want: map[string]share{
				"model-a": {runs: 1, succeeded: 1, input: 100, output: 10, total: 110, cost: 4.0, partial: 1, lowerBound: 1},
				"model-b": {runs: 1, succeeded: 1, input: 200, output: 20, total: 220, unpriced: 1, lowerBound: 1},
			},
		},
		{
			name: "without unaccounted turns the estimated part counts in neither shortfall",
			rows: []storage.RunStatsRow{split(0, new(4.0), "partial",
				part("model-a", 100, 10, new(4.0), "estimated"),
				part("model-b", 200, 20, nil, "unpriced"))},
			want: map[string]share{
				"model-a": {runs: 1, succeeded: 1, input: 100, output: 10, total: 110, cost: 4.0},
				"model-b": {runs: 1, succeeded: 1, input: 200, output: 20, total: 220, unpriced: 1},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agg := New(recordedCaps)
			addRows(t, agg, tt.rows...)

			report := agg.Report(fixedNow, "/wf", "/db", nil, nil)

			got := map[string]share{}
			for _, g := range report.ByModel {
				if g.Tokens == nil {
					t.Fatalf("by_model %q Tokens = nil, want measured tokens", g.Name)
				}
				var cost, agent float64
				if g.CostUSD != nil {
					cost = *g.CostUSD
				}
				if g.AgentPricedUSD != nil {
					agent = *g.AgentPricedUSD
				}
				got[g.Name] = share{
					runs: g.Runs, succeeded: g.Succeeded, unpriced: g.CostUnpricedRuns, partial: g.CostPartialRuns, lowerBound: g.TokensLowerBoundRuns,
					input: g.Tokens.Input, output: g.Tokens.Output, total: g.Tokens.Total, cost: cost, agent: agent,
				}
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Report(%q).ByModel = %+v, want %+v", tt.name, got, tt.want)
			}
		})
	}
}

var spendNow = time.Date(2026, 10, 28, 15, 50, 0, 0, time.UTC)

func mustUTC(text string) time.Time {
	ts, err := time.Parse(time.RFC3339, text)
	if err != nil {
		panic("bad test time " + text)
	}
	return ts
}

func bounded(since, until string) Range {
	r := Range{Since: new(mustUTC(since))}
	if until != "" {
		r.Until = new(mustUTC(until))
	}
	return r
}

func trailing(now time.Time, d time.Duration) Range {
	return Range{Since: new(now.Add(-d)), SinceIsDuration: true}
}

func spendTokens(n int64) *Tokens {
	return &Tokens{Input: 1_000 * n, Output: 100 * n, Total: 1_100 * n, CacheRead: 200 * n, CacheWrite: 300 * n, Uncached: 900 * n, FreshInput: 500 * n}
}

func spendKinds(cost float64) *KindCosts {
	return &KindCosts{FreshInputUSD: cost / 2, OutputUSD: cost / 4, CacheWriteUSD: cost / 8, CacheReadUSD: cost / 8}
}

func spendTurn(seq int, endedAt string, cost float64) storage.RunTurn {
	return storage.RunTurn{
		Seq: seq, EndedAt: mustUTC(endedAt), TokensMeasured: true, ToolCalls: 2, Errors: 1,
		Models:           map[string]storage.ModelTokens{"model-a": {InputTokens: 1_000, OutputTokens: 100, CacheReadTokens: 200, CacheWriteTokens: 300}},
		EstimatedCostUSD: &cost,
		InputCostUSD:     cost / 2, OutputCostUSD: cost / 4, CacheWriteCostUSD: cost / 8, CacheReadCostUSD: cost / 8,
	}
}

func storedTurn(runID int64, dispatchID, endedAt string, cost float64) storage.RunTurnRow {
	return storage.RunTurnRow{
		RunID: runID, Identifier: fmt.Sprintf("PROJ-%d", runID), DispatchID: dispatchID,
		AgentAdapter: "claude-code", Status: "succeeded", Turn: spendTurn(1, endedAt, cost),
	}
}

func unmeasuredTurn(row storage.RunTurnRow) storage.RunTurnRow {
	row.Turn.TokensMeasured, row.Turn.Models, row.Turn.EstimatedCostUSD = false, nil, nil
	return row
}

func unpricedTurn(row storage.RunTurnRow) storage.RunTurnRow {
	row.Turn.EstimatedCostUSD = nil
	return row
}

func untrackedRun(runID int64, completedAt string, cost float64) storage.UntrackedRunRow {
	return storage.UntrackedRunRow{
		RunID: runID, Identifier: fmt.Sprintf("PROJ-%d", runID), AgentAdapter: "claude-code", Status: "succeeded",
		CompletedAt: mustUTC(completedAt), TokensMeasured: true,
		InputTokens: 1_000, OutputTokens: 100, TotalTokens: 1_100, CacheReadTokens: 200, CacheWriteTokens: 300,
		EstimatedCostUSD: &cost, CostStatus: "estimated",
	}
}

func runningSession(dispatchID string, cost float64, turns ...storage.RunTurn) RunningSession {
	return RunningSession{DispatchID: dispatchID, Identifier: dispatchID, AgentKind: "claude-code", CostUSD: &cost, Turns: turns}
}

func spendBucket(start string, turns int, cost float64) SpendBucket {
	return SpendBucket{
		Start: start, Runs: 1, Turns: turns, ToolCalls: 2 * turns, Errors: turns,
		Tokens: spendTokens(int64(turns)), CostUSD: &cost, CostByKind: spendKinds(cost),
	}
}

func untrackedBucket(start string, cost float64) SpendBucket {
	return SpendBucket{Start: start, Runs: 1, TurnsUnrecordedRuns: 1, Tokens: spendTokens(1), CostUSD: &cost}
}

func zeroBucket(start string) SpendBucket {
	return SpendBucket{Start: start, Tokens: &Tokens{}, CostUSD: new(0.0), CostByKind: &KindCosts{}}
}

func spendBucketFor(caps storage.RunHistoryCapabilities, b SpendBucket) SpendBucket {
	if !caps.RecordedCost() {
		b.CostUSD, b.CostByKind, b.AgentPricedUSD = nil, nil, nil
		b.CostUnpricedRuns, b.CostPartialRuns, b.CostLowerBound = 0, 0, false
	}
	if !caps.Full() {
		b.Tokens, b.TokensLowerBound = nil, false
		b.TokensUnmeasuredRuns, b.TokensLowerBoundRuns = 0, 0
	}
	return b
}

func TestStatsSpend(t *testing.T) {
	t.Parallel()

	const (
		lastQuarter = "2026-10-28T15:45:00Z"
		lastHour    = "2026-10-28T15:00:00Z"
	)
	lateNow := mustUTC("2028-01-01T00:00:00Z")
	atSixteen := mustUTC("2026-10-28T16:00:00Z")
	lowerBoundRun := []storage.RunTurnRow{storedTurn(1, "d1", "2026-10-27T10:05:00Z", 1.0), storedTurn(1, "d1", "2026-10-27T10:25:00Z", 2.0)}
	for i := range lowerBoundRun {
		lowerBoundRun[i].UsageCompleteness = storage.UsageLowerBound
	}
	runningBucket := spendBucket(lastHour, 2, 3.0)
	runningBucket.InProgress = true
	lowerBoundBucket := spendBucket("2026-10-27T10:00:00Z", 2, 3.0)
	lowerBoundBucket.TokensLowerBoundRuns, lowerBoundBucket.TokensLowerBound = 1, true
	partial := storedTurn(1, "d1", "2026-10-27T10:05:00Z", 1.0)
	partial.CostStatus = "partial"
	prorated := storedTurn(1, "d1", "2026-10-27T10:05:00Z", 2.0)
	prorated.RunCostUSD, prorated.RunAgentPricedUSD = new(4.0), new(1.0)
	capped := storedTurn(3, "d3", "2026-10-27T10:25:00Z", 1.0)
	capped.RunCostUSD, capped.RunAgentPricedUSD = new(1.0), new(2.0)
	agentUntracked := untrackedRun(4, "2026-10-27T10:35:00Z", 1.0)
	agentUntracked.AgentPricedUSD = new(0.25)
	unaccounted := untrackedRun(1, "2026-10-27T10:35:00Z", 0)
	unaccounted.EstimatedCostUSD, unaccounted.CostStatus, unaccounted.UnaccountedTurns = nil, "unpriced", 1

	tests := []struct {
		name           string
		r              Range
		period         Period
		now            time.Time
		turns          []storage.RunTurnRow
		untracked      []storage.UntrackedRunRow
		running        []RunningSession
		wantPeriod     Period
		wantCount      int
		wantFirst      string
		wantInProgress []string
		wantBuckets    []SpendBucket
	}{
		{name: "24 hour preset lists 96 quarter hours", r: trailing(spendNow, 24*time.Hour), wantPeriod: PeriodQuarterHour, wantCount: 96, wantFirst: "2026-10-27T16:00:00Z", wantInProgress: []string{lastQuarter}},
		{name: "24 hour preset at a boundary has none in progress", r: trailing(atSixteen, 24*time.Hour), now: atSixteen, wantPeriod: PeriodQuarterHour, wantCount: 96, wantFirst: "2026-10-27T16:00:00Z"},
		{
			name: "instant since keeps its bucket and counts from since", r: bounded("2026-10-27T15:50:00Z", ""),
			turns:      []storage.RunTurnRow{storedTurn(1, "d1", "2026-10-27T15:47:00Z", 1.0), storedTurn(2, "d2", "2026-10-27T15:55:00Z", 2.0)},
			wantPeriod: PeriodQuarterHour, wantCount: 97, wantFirst: "2026-10-27T15:45:00Z", wantInProgress: []string{lastQuarter},
			wantBuckets: []SpendBucket{spendBucket("2026-10-27T15:45:00Z", 1, 2.0)},
		},
		{name: "7 day preset lists 168 hours", r: trailing(spendNow, 168*time.Hour), wantPeriod: PeriodHour, wantCount: 168, wantFirst: "2026-10-21T16:00:00Z", wantInProgress: []string{lastHour}},
		{name: "instant since 7 days back lists 169 hours", r: bounded("2026-10-21T15:50:00Z", ""), wantPeriod: PeriodHour, wantCount: 169, wantFirst: "2026-10-21T15:00:00Z", wantInProgress: []string{lastHour}},
		{
			name: "10 hour span lists 5 minutes and leaves out a turn at until", r: bounded("2026-10-28T00:00:00Z", "2026-10-28T10:00:00Z"),
			turns:      []storage.RunTurnRow{storedTurn(1, "d1", "2026-10-28T09:59:00Z", 1.0), storedTurn(2, "d2", "2026-10-28T10:00:00Z", 2.0)},
			wantPeriod: PeriodFiveMinutes, wantCount: 120, wantFirst: "2026-10-28T00:00:00Z",
			wantBuckets: []SpendBucket{spendBucket("2026-10-28T09:55:00Z", 1, 1.0)},
		},
		{name: "30 hour span lists quarter hours", r: bounded("2026-10-27T00:00:00Z", "2026-10-28T06:00:00Z"), wantPeriod: PeriodQuarterHour, wantCount: 120, wantFirst: "2026-10-27T00:00:00Z"},
		{name: "120 day span lists days", r: bounded("2026-06-30T00:00:00Z", "2026-10-28T00:00:00Z"), wantPeriod: PeriodDay, wantCount: 120, wantFirst: "2026-06-30T00:00:00Z"},
		{
			name:       "open range lists months from the earliest through the report month",
			turns:      []storage.RunTurnRow{storedTurn(2, "d2", "2026-08-03T10:00:00Z", 2.0)},
			untracked:  []storage.UntrackedRunRow{untrackedRun(1, "2026-06-15T10:00:00Z", 1.0)},
			wantPeriod: PeriodMonth, wantCount: 5, wantFirst: "2026-06-01T00:00:00Z", wantInProgress: []string{"2026-10-01T00:00:00Z"},
			wantBuckets: []SpendBucket{untrackedBucket("2026-06-01T00:00:00Z", 1.0), spendBucket("2026-08-01T00:00:00Z", 1, 2.0)},
		},
		{name: "open range without contributions lists nothing", wantPeriod: PeriodMonth},
		{
			name: "bounded range without contributions lists zero buckets", r: bounded("2026-10-28T00:00:00Z", "2026-10-28T02:00:00Z"),
			wantPeriod: PeriodFiveMinutes, wantCount: 24, wantFirst: "2026-10-28T00:00:00Z",
			wantBuckets: []SpendBucket{zeroBucket("2026-10-28T00:00:00Z")},
		},
		{name: "requested period replaces the rule", r: trailing(spendNow, 336*time.Hour), period: PeriodHour, wantPeriod: PeriodHour, wantCount: 336, wantFirst: "2026-10-14T16:00:00Z", wantInProgress: []string{lastHour}},
		{name: "14 hours take 5 minutes", r: bounded("2026-10-28T00:00:00Z", "2026-10-28T14:00:00Z"), now: lateNow, wantPeriod: PeriodFiveMinutes, wantCount: 168, wantFirst: "2026-10-28T00:00:00Z"},
		{name: "14 hours and a second take quarter hours", r: bounded("2026-10-28T00:00:00Z", "2026-10-28T14:00:01Z"), now: lateNow, wantPeriod: PeriodQuarterHour, wantCount: 57, wantFirst: "2026-10-28T00:00:00Z"},
		{name: "42 hours take quarter hours", r: bounded("2026-10-28T00:00:00Z", "2026-10-29T18:00:00Z"), now: lateNow, wantPeriod: PeriodQuarterHour, wantCount: 168, wantFirst: "2026-10-28T00:00:00Z"},
		{name: "42 hours and a second take hours", r: bounded("2026-10-28T00:00:00Z", "2026-10-29T18:00:01Z"), now: lateNow, wantPeriod: PeriodHour, wantCount: 43, wantFirst: "2026-10-28T00:00:00Z"},
		{name: "168 hours take hours", r: bounded("2026-10-28T00:00:00Z", "2026-11-04T00:00:00Z"), now: lateNow, wantPeriod: PeriodHour, wantCount: 168, wantFirst: "2026-10-28T00:00:00Z"},
		{name: "168 hours and a second take days", r: bounded("2026-10-28T00:00:00Z", "2026-11-04T00:00:01Z"), now: lateNow, wantPeriod: PeriodDay, wantCount: 8, wantFirst: "2026-10-28T00:00:00Z"},
		{name: "4032 hours take days", r: bounded("2026-10-28T00:00:00Z", "2027-04-14T00:00:00Z"), now: lateNow, wantPeriod: PeriodDay, wantCount: 168, wantFirst: "2026-10-28T00:00:00Z"},
		{name: "4032 hours and a second take months", r: bounded("2026-10-28T00:00:00Z", "2027-04-14T00:00:01Z"), now: lateNow, wantPeriod: PeriodMonth, wantCount: 7, wantFirst: "2026-10-01T00:00:00Z"},
		{
			name: "each turn of a run counts in its own hour", r: trailing(spendNow, 168*time.Hour),
			turns: []storage.RunTurnRow{
				storedTurn(1, "d1", "2026-10-28T09:10:00Z", 1.0), storedTurn(1, "d1", "2026-10-28T10:20:00Z", 2.0), storedTurn(1, "d1", "2026-10-28T11:30:00Z", 0.5),
			},
			wantPeriod: PeriodHour, wantCount: 168, wantFirst: "2026-10-21T16:00:00Z", wantInProgress: []string{lastHour},
			wantBuckets: []SpendBucket{
				spendBucket("2026-10-28T09:00:00Z", 1, 1.0), spendBucket("2026-10-28T10:00:00Z", 1, 2.0), spendBucket("2026-10-28T11:00:00Z", 1, 0.5),
			},
		},
		{
			name: "running session counts its ended turns in the bucket in progress", r: trailing(spendNow, 168*time.Hour),
			running: []RunningSession{runningSession("r1", 7.0,
				spendTurn(1, "2026-10-28T15:05:00Z", 1.0), spendTurn(2, "2026-10-28T15:40:00Z", 2.0), spendTurn(3, "2026-10-28T15:50:00Z", 4.0))},
			wantPeriod: PeriodHour, wantCount: 168, wantFirst: "2026-10-21T16:00:00Z", wantInProgress: []string{lastHour},
			wantBuckets: []SpendBucket{runningBucket},
		},
		{
			name: "running session that stored rows name is dropped whole", r: trailing(spendNow, 168*time.Hour),
			turns: []storage.RunTurnRow{storedTurn(1, "r1", "2026-10-28T15:05:00Z", 1.0), storedTurn(1, "r1", "2026-10-28T15:40:00Z", 2.0)},
			running: []RunningSession{runningSession("r1", 3.0,
				spendTurn(1, "2026-10-28T15:05:00Z", 1.0), spendTurn(2, "2026-10-28T15:40:00Z", 2.0))},
			wantPeriod: PeriodHour, wantCount: 168, wantFirst: "2026-10-21T16:00:00Z", wantInProgress: []string{lastHour},
			wantBuckets: []SpendBucket{runningBucket},
		},
		{
			name: "run without turn records counts whole at its completion", r: trailing(spendNow, 168*time.Hour),
			untracked:  []storage.UntrackedRunRow{untrackedRun(1, "2026-10-25T12:34:00Z", 1.5)},
			wantPeriod: PeriodHour, wantCount: 168, wantFirst: "2026-10-21T16:00:00Z", wantInProgress: []string{lastHour},
			wantBuckets: []SpendBucket{untrackedBucket("2026-10-25T12:00:00Z", 1.5)},
		},
		{
			name: "run without turn records beside a turn leaves cost by kind null", r: trailing(spendNow, 168*time.Hour),
			turns:      []storage.RunTurnRow{storedTurn(1, "d1", "2026-10-25T12:10:00Z", 1.0)},
			untracked:  []storage.UntrackedRunRow{untrackedRun(2, "2026-10-25T12:34:00Z", 1.5)},
			wantPeriod: PeriodHour, wantCount: 168, wantFirst: "2026-10-21T16:00:00Z", wantInProgress: []string{lastHour},
			wantBuckets: []SpendBucket{{
				Start: "2026-10-25T12:00:00Z", Runs: 2, Turns: 1, ToolCalls: 2, Errors: 1, TurnsUnrecordedRuns: 1,
				Tokens: spendTokens(2), CostUSD: new(2.5),
			}},
		},
		{
			name: "agent priced part is prorated by run and capped at the run cost", r: trailing(spendNow, 168*time.Hour),
			turns:      []storage.RunTurnRow{prorated, storedTurn(2, "d2", "2026-10-27T10:15:00Z", 1.0), capped},
			untracked:  []storage.UntrackedRunRow{agentUntracked},
			wantPeriod: PeriodHour, wantCount: 168, wantFirst: "2026-10-21T16:00:00Z", wantInProgress: []string{lastHour},
			wantBuckets: []SpendBucket{{
				Start: "2026-10-27T10:00:00Z", Runs: 4, Turns: 3, ToolCalls: 6, Errors: 3, TurnsUnrecordedRuns: 1,
				Tokens: spendTokens(4), CostUSD: new(5.0), AgentPricedUSD: new(1.75),
			}},
		},
		{
			name: "partial, unpriced, and unmeasured runs flag their bucket", r: trailing(spendNow, 168*time.Hour),
			turns: []storage.RunTurnRow{
				partial, unpricedTurn(storedTurn(2, "d2", "2026-10-27T10:15:00Z", 1.0)), unmeasuredTurn(storedTurn(3, "d3", "2026-10-27T10:25:00Z", 1.0)),
			},
			wantPeriod: PeriodHour, wantCount: 168, wantFirst: "2026-10-21T16:00:00Z", wantInProgress: []string{lastHour},
			wantBuckets: []SpendBucket{{
				Start: "2026-10-27T10:00:00Z", Runs: 3, Turns: 3, ToolCalls: 6, Errors: 3, Tokens: spendTokens(2), CostUSD: new(1.0), CostByKind: spendKinds(1.0),
				TokensUnmeasuredRuns: 1, CostUnpricedRuns: 1, CostPartialRuns: 1, TokensLowerBound: true, CostLowerBound: true,
			}},
		},
		{
			name: "bucket of unmeasured turns carries no tokens and no cost", r: trailing(spendNow, 168*time.Hour),
			turns:      []storage.RunTurnRow{unmeasuredTurn(storedTurn(1, "d1", "2026-10-27T10:05:00Z", 1.0))},
			wantPeriod: PeriodHour, wantCount: 168, wantFirst: "2026-10-21T16:00:00Z", wantInProgress: []string{lastHour},
			wantBuckets: []SpendBucket{{Start: "2026-10-27T10:00:00Z", Runs: 1, Turns: 1, ToolCalls: 2, Errors: 1, TokensUnmeasuredRuns: 1}},
		},
		{
			name: "lower bound run counts once however many turns", r: trailing(spendNow, 168*time.Hour),
			turns:      lowerBoundRun,
			wantPeriod: PeriodHour, wantCount: 168, wantFirst: "2026-10-21T16:00:00Z", wantInProgress: []string{lastHour},
			wantBuckets: []SpendBucket{lowerBoundBucket},
		},
		{
			name: "run without turn records and unaccounted turns is a lower bound", r: trailing(spendNow, 168*time.Hour),
			untracked:  []storage.UntrackedRunRow{unaccounted},
			wantPeriod: PeriodHour, wantCount: 168, wantFirst: "2026-10-21T16:00:00Z", wantInProgress: []string{lastHour},
			wantBuckets: []SpendBucket{{
				Start: "2026-10-27T10:00:00Z", Runs: 1, TurnsUnrecordedRuns: 1, Tokens: spendTokens(1),
				CostUnpricedRuns: 1, TokensLowerBoundRuns: 1, TokensLowerBound: true,
			}},
		},
	}

	allCaps := []storage.RunHistoryCapabilities{recordedCaps, fullCaps, {}}

	for i := range len(tests) * len(allCaps) {
		tt, caps := tests[i/len(allCaps)], allCaps[i%len(allCaps)]
		t.Run(fmt.Sprintf("%s/recorded=%t/full=%t", tt.name, caps.RecordedCost(), caps.Full()), func(t *testing.T) {
			t.Parallel()

			now := cmp.Or(tt.now, spendNow)
			agg := New(caps)
			agg.SetSpend(tt.r, tt.period, now, tt.running)
			for _, row := range tt.turns {
				if err := agg.AddTurn(row); err != nil {
					t.Fatalf("AddTurn(%+v) = %v, want nil", row, err)
				}
			}
			for _, row := range tt.untracked {
				if err := agg.AddUntrackedRun(row); err != nil {
					t.Fatalf("AddUntrackedRun(%+v) = %v, want nil", row, err)
				}
			}

			spend := agg.Report(now, "/wf", "/db", tt.r.Since, tt.r.Until).Spend

			if spend.Period != tt.wantPeriod {
				t.Errorf("Report(%+v).Spend.Period = %q, want %q", tt.r, spend.Period, tt.wantPeriod)
			}
			if len(spend.Buckets) != tt.wantCount {
				t.Fatalf("Report(%+v).Spend lists %d buckets, want %d", tt.r, len(spend.Buckets), tt.wantCount)
			}
			if tt.wantCount > 0 && spend.Buckets[0].Start != tt.wantFirst {
				t.Errorf("Report(%+v).Spend.Buckets[0].Start = %q, want %q", tt.r, spend.Buckets[0].Start, tt.wantFirst)
			}
			var inProgress []string
			byStart := make(map[string]SpendBucket)
			for _, b := range spend.Buckets {
				byStart[b.Start] = b
				if b.InProgress {
					inProgress = append(inProgress, b.Start)
				}
				if b.CostUSD != nil && b.CostByKind != nil {
					kinds := b.CostByKind.FreshInputUSD + b.CostByKind.OutputUSD + b.CostByKind.CacheWriteUSD + b.CostByKind.CacheReadUSD
					if math.Abs(kinds-*b.CostUSD) > 1e-6 {
						t.Errorf("bucket %s cost_by_kind sums to %v, want cost_usd %v", b.Start, kinds, *b.CostUSD)
					}
				}
			}
			if !slices.Equal(inProgress, tt.wantInProgress) {
				t.Errorf("Report(%+v).Spend in-progress buckets = %v, want %v", tt.r, inProgress, tt.wantInProgress)
			}
			for _, want := range tt.wantBuckets {
				want = spendBucketFor(caps, want)
				if got := byStart[want.Start]; !reflect.DeepEqual(got, want) {
					t.Errorf("Report(%+v).Spend bucket %s = %+v, want %+v", tt.r, want.Start, got, want)
				}
			}
		})
	}
}

type spanView struct {
	Dispatch               string
	Turns                  int
	HasTokens, HasCost     bool
	Unpriced               bool
	CostShare, TokensShare float64
}

func viewOf(s SpanSession) spanView {
	v := spanView{Turns: -1, CostShare: -1, TokensShare: -1, HasTokens: s.Tokens != nil, HasCost: s.CostUSD != nil, Unpriced: s.CostUnpriced}
	if s.DispatchID != nil {
		v.Dispatch = *s.DispatchID
	}
	if s.Turns != nil {
		v.Turns = *s.Turns
	}
	if s.CostShare != nil {
		v.CostShare = *s.CostShare
	}
	if s.TokensShare != nil {
		v.TokensShare = *s.TokensShare
	}
	return v
}

func sameView(a, b spanView) bool {
	a.CostShare, b.CostShare = math.Round(a.CostShare*1e9), math.Round(b.CostShare*1e9)
	a.TokensShare, b.TokensShare = math.Round(a.TokensShare*1e9), math.Round(b.TokensShare*1e9)
	return a == b
}

func sumBuckets(buckets []SpendBucket) (sum SpendBucket) {
	var tokens Tokens
	var cost float64
	for _, b := range buckets {
		sum.Turns += b.Turns
		sum.ToolCalls += b.ToolCalls
		sum.Errors += b.Errors
		sum.TurnsUnrecordedRuns += b.TurnsUnrecordedRuns
		if b.Tokens != nil {
			tokens.Input += b.Tokens.Input
			tokens.Output += b.Tokens.Output
			tokens.CacheRead += b.Tokens.CacheRead
			tokens.CacheWrite += b.Tokens.CacheWrite
		}
		if b.CostUSD != nil {
			cost += *b.CostUSD
		}
	}
	sum.Tokens, sum.CostUSD = &tokens, &cost
	return sum
}

func TestStatsSpan(t *testing.T) {
	t.Parallel()

	const since, until = "2026-10-28T15:00:00Z", "2026-10-28T15:45:00Z"
	unmeasured := func(runID int64, dispatchID string, endedAt ...string) []storage.RunTurnRow {
		var rows []storage.RunTurnRow
		for _, at := range endedAt {
			rows = append(rows, unmeasuredTurn(storedTurn(runID, dispatchID, at, 1.0)))
		}
		return rows
	}
	tests := []struct {
		name           string
		until          string
		turns          []storage.RunTurnRow
		untracked      []storage.UntrackedRunRow
		running        []RunningSession
		wantInProgress bool
		wantSessions   []spanView
	}{
		{
			name: "each session lists once with its turns inside the window and its shares", until: until,
			turns: []storage.RunTurnRow{
				storedTurn(1, "d1", "2026-10-28T14:30:00Z", 4.0), storedTurn(1, "d1", "2026-10-28T15:05:00Z", 1.0),
				storedTurn(1, "d1", "2026-10-28T15:20:00Z", 2.0), storedTurn(1, "d1", until, 8.0),
			},
			running: []RunningSession{runningSession("r1", 1.0, spendTurn(1, "2026-10-28T15:30:00Z", 1.0))},
			wantSessions: []spanView{
				{Dispatch: "d1", Turns: 2, HasTokens: true, HasCost: true, CostShare: 0.75, TokensShare: 2.0 / 3},
				{Dispatch: "r1", Turns: 1, HasTokens: true, HasCost: true, CostShare: 0.25, TokensShare: 1.0 / 3},
			},
		},
		{
			name:  "an open span is in progress",
			turns: []storage.RunTurnRow{storedTurn(1, "d1", "2026-10-28T15:05:00Z", 1.0)}, wantInProgress: true,
			wantSessions: []spanView{{Dispatch: "d1", Turns: 1, HasTokens: true, HasCost: true, CostShare: 1, TokensShare: 1}},
		},
		{
			name: "run without turn records has null turns and ranks after a run with them", until: until,
			turns:     []storage.RunTurnRow{storedTurn(1, "d1", "2026-10-28T15:05:00Z", 1.0)},
			untracked: []storage.UntrackedRunRow{untrackedRun(9, "2026-10-28T15:10:00Z", 1.0)},
			wantSessions: []spanView{
				{Dispatch: "d1", Turns: 1, HasTokens: true, HasCost: true, CostShare: 0.5, TokensShare: 0.5},
				{Turns: -1, HasTokens: true, HasCost: true, CostShare: 0.5, TokensShare: 0.5},
			},
		},
		{
			name: "unmeasured sessions carry no tokens, no cost, and no share, and rank by turns", until: until,
			turns: append(append(unmeasured(5, "a", "2026-10-28T15:05:00Z"), unmeasured(6, "z", "2026-10-28T15:06:00Z", "2026-10-28T15:07:00Z")...),
				storedTurn(1, "p", "2026-10-28T15:08:00Z", 1.0)),
			wantSessions: []spanView{
				{Dispatch: "p", Turns: 1, HasTokens: true, HasCost: true, CostShare: 1, TokensShare: 1},
				{Dispatch: "z", Turns: 2, CostShare: -1, TokensShare: -1},
				{Dispatch: "a", Turns: 1, CostShare: -1, TokensShare: -1},
			},
		},
		{
			name: "unpriced session reports cost_unpriced and takes no cost share", until: until,
			turns: []storage.RunTurnRow{storedTurn(1, "d1", "2026-10-28T15:05:00Z", 1.0), unpricedTurn(storedTurn(2, "d2", "2026-10-28T15:06:00Z", 1.0))},
			wantSessions: []spanView{
				{Dispatch: "d1", Turns: 1, HasTokens: true, HasCost: true, CostShare: 1, TokensShare: 0.5},
				{Dispatch: "d2", Turns: 1, HasTokens: true, Unpriced: true, CostShare: -1, TokensShare: 0.5},
			},
		},
		{
			name: "zero cost sum leaves cost shares null and ties break by dispatch ID", until: until,
			turns: []storage.RunTurnRow{storedTurn(2, "d2", "2026-10-28T15:06:00Z", 0), storedTurn(1, "d1", "2026-10-28T15:05:00Z", 0)},
			wantSessions: []spanView{
				{Dispatch: "d1", Turns: 1, HasTokens: true, HasCost: true, CostShare: -1, TokensShare: 0.5},
				{Dispatch: "d2", Turns: 1, HasTokens: true, HasCost: true, CostShare: -1, TokensShare: 0.5},
			},
		},
		{name: "span without contributions lists no session", until: until},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var until *time.Time
			if tt.until != "" {
				until = new(mustUTC(tt.until))
			}
			from := mustUTC(since)
			span := NewSpan(recordedCaps, from, until, spendNow, tt.running)
			agg := New(recordedCaps)
			agg.SetSpend(Range{Since: &from, Until: until}, PeriodQuarterHour, spendNow, tt.running)
			for _, row := range tt.turns {
				if err := cmp.Or(span.AddTurn(row), agg.AddTurn(row)); err != nil {
					t.Fatalf("AddTurn(%+v) = %v, want nil", row, err)
				}
			}
			for _, row := range tt.untracked {
				if err := cmp.Or(span.AddUntrackedRun(row), agg.AddUntrackedRun(row)); err != nil {
					t.Fatalf("AddUntrackedRun(%+v) = %v, want nil", row, err)
				}
			}

			report := span.Report()

			if report.Sessions == nil {
				t.Fatalf("Span.Report().Sessions = nil, want a non-nil slice")
			}
			var got []spanView
			var costShares float64
			for _, s := range report.Sessions {
				got = append(got, viewOf(s))
				if s.CostShare != nil {
					costShares += *s.CostShare
				}
			}
			if !slices.EqualFunc(got, tt.wantSessions, sameView) {
				t.Errorf("Span.Report().Sessions = %+v, want %+v", got, tt.wantSessions)
			}
			if costShares != 0 && math.Abs(costShares-1) > 1e-9 {
				t.Errorf("Span.Report() cost shares sum to %v, want 1", costShares)
			}
			if report.Totals.InProgress != tt.wantInProgress {
				t.Errorf("Span.Report().Totals.InProgress = %t, want %t", report.Totals.InProgress, tt.wantInProgress)
			}
			if report.Period != PeriodFiveMinutes {
				t.Errorf("Span.Report().Period = %q, want %q", report.Period, PeriodFiveMinutes)
			}
			buckets := agg.Report(spendNow, "/wf", "/db", &from, until).Spend.Buckets
			want, got2 := sumBuckets(buckets), report.Totals
			if got2.Turns != want.Turns || got2.ToolCalls != want.ToolCalls || got2.Errors != want.Errors || got2.TurnsUnrecordedRuns != want.TurnsUnrecordedRuns ||
				got2.Tokens == nil || got2.Tokens.Input != want.Tokens.Input || got2.Tokens.Output != want.Tokens.Output ||
				got2.Tokens.CacheRead != want.Tokens.CacheRead || got2.Tokens.CacheWrite != want.Tokens.CacheWrite ||
				got2.CostUSD == nil || math.Abs(*got2.CostUSD-*want.CostUSD) > 1e-6 {
				t.Errorf("Span.Report().Totals = %+v, want the sums of the adjacent buckets %+v", got2, want)
			}
		})
	}
}

func TestStatsByTrigger(t *testing.T) {
	t.Parallel()

	noTrigger := withTriggerBreakdown(fullCaps)
	noTrigger.HasTrigger = false
	tests := []struct {
		name string
		caps storage.RunHistoryCapabilities
		want []string
	}{
		{"recorded triggers keep unknown apart from none", withTriggerBreakdown(fullCaps), []string{"ci_fix", "<none>", "<unknown>"}},
		{"column absent leaves the breakdown empty", noTrigger, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agg := New(tt.caps)
			addRows(t, agg, triggerRow("A", new("ci_fix")), triggerRow("A", nil), triggerRow("B", new("")), triggerRow("C", new("ci_fix")))

			report := agg.Report(fixedNow, "/wf", "/db", nil, nil)

			var got []string
			for _, g := range report.ByTrigger {
				got = append(got, g.Name)
			}
			if !slices.Equal(got, tt.want) || report.Summary.Issues != 3 {
				t.Errorf("Report(%+v) by_trigger names, issues = %q, %d, want %q, 3", tt.caps, got, report.Summary.Issues, tt.want)
			}
			if len(got) > 0 && (sumRuns(report.ByTrigger) != report.Summary.Runs || report.ByTrigger[0].Runs != 2) {
				t.Errorf("Report(%+v) by_trigger = %+v, want runs summing to %d with ci_fix at 2", tt.caps, report.ByTrigger, report.Summary.Runs)
			}
			if len(got) == 0 && (report.ByOrigin == nil || len(report.ByOrigin) > 0) {
				t.Errorf("Report(%+v).ByOrigin = %#v, want an empty, non-nil list beside the empty by_trigger", tt.caps, report.ByOrigin)
			}
		})
	}
}

func originRow(trigger *string, reactionKind, stagePrevious string, firstAfterHop bool) storage.RunStatsRow {
	row := modelRow("claude-code", "model-a", "succeeded", 0)
	row.Trigger, row.ReactionKind, row.StagePrevious, row.FirstAfterHop = trigger, reactionKind, stagePrevious, firstAfterHop
	return row
}

func TestStatsByOrigin(t *testing.T) {
	t.Parallel()

	noOriginColumn := withTriggerBreakdown(fullCaps)
	noOriginColumn.HasFirstAfterHop = false
	errorRetry := originRow(new("error"), "", "", false)
	errorRetry.Status = "failed"
	tests := []struct {
		name string
		caps storage.RunHistoryCapabilities
		want map[string]int
	}{
		{
			name: "each origin forms its own group and only the first run after a hop is stage_hop",
			caps: withTriggerBreakdown(fullCaps),
			want: map[string]int{"poll_tick": 2, "stage_hop": 1, "error": 1, "ci_fix": 1, "review": 1, "<unknown>": 1},
		},
		{name: "first_after_hop column absent leaves the breakdown empty beside by_trigger", caps: noOriginColumn},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agg := New(tt.caps)
			addRows(t, agg,
				originRow(new(""), "", "", false),
				originRow(new(""), "", "specify", true),
				errorRetry,
				originRow(new("ci_fix"), "ci", "", false),
				originRow(new(""), "review", "", false),
				originRow(nil, "", "", false),
				originRow(new(""), "", "specify", false),
			)

			report := agg.Report(fixedNow, "/wf", "/db", nil, nil)

			if report.ByOrigin == nil {
				t.Fatalf("Report(%+v).ByOrigin = nil, want a non-nil list", tt.caps)
			}
			got := map[string]int{}
			for _, g := range report.ByOrigin {
				got[g.Name] = g.Runs
			}
			if !maps.Equal(got, tt.want) {
				t.Errorf("Report(%+v) by_origin runs = %v, want %v", tt.caps, got, tt.want)
			}
			if len(tt.want) > 0 && sumRuns(report.ByOrigin) != report.Summary.Runs {
				t.Errorf("Report(%+v) by_origin = %+v, want runs summing to %d", tt.caps, report.ByOrigin, report.Summary.Runs)
			}
			if sumRuns(report.ByTrigger) != report.Summary.Runs {
				t.Errorf("Report(%+v) by_trigger = %+v, want runs summing to %d", tt.caps, report.ByTrigger, report.Summary.Runs)
			}
		})
	}
}

func skillRow(skills *string) storage.RunStatsRow {
	row := modelRow("claude-code", "model-a", "succeeded", 0)
	row.Skills = skills
	return row
}

func TestStatsBySkill(t *testing.T) {
	t.Parallel()

	skilled := withTriggerBreakdown(fullCaps)
	skilled.HasRecordedCost, skilled.HasSkills = true, true
	noColumn := skilled
	noColumn.HasSkills = false
	mixed := []storage.RunStatsRow{
		skillRow(new(`[{"skill":"plan"},{"skill":"test","tool_error":true},{"skill":"test"}]`)),
		skillRow(new(`[]`)),
		skillRow(nil),
		skillRow(new(`[{"skill":"plan"}]`)),
	}
	tests := []struct {
		name string
		caps storage.RunHistoryCapabilities
		rows []storage.RunStatsRow
		want *SkillCallReport
	}{
		{"runs count once per skill and a NULL run is unrecorded", skilled, mixed, &SkillCallReport{UnrecordedRuns: 1, BySkill: []SkillCount{
			{Skill: "plan", Runs: 2, Calls: 2},
			{Skill: "test", Runs: 1, Calls: 2, FailedCalls: 1},
		}}},
		{"a value that does not decode is unrecorded", skilled, []storage.RunStatsRow{skillRow(new("not json"))}, &SkillCallReport{UnrecordedRuns: 1, BySkill: []SkillCount{}}},
		{"runs that called nothing leave an empty, non-nil list", skilled, []storage.RunStatsRow{skillRow(new("[]"))}, &SkillCallReport{BySkill: []SkillCount{}}},
		{"column absent leaves the report nil", noColumn, mixed, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agg := New(tt.caps)
			addRows(t, agg, tt.rows...)

			report := agg.Report(fixedNow, "/wf", "/db", nil, nil)

			if !reflect.DeepEqual(report.SkillCalls, tt.want) {
				t.Errorf("Report(%+v).SkillCalls = %+v, want %+v", tt.caps, report.SkillCalls, tt.want)
			}
		})
	}
}

func TestStatsByChain(t *testing.T) {
	t.Parallel()

	caps := withModelBreakdown(fullCaps)
	caps.HasStageChains = true
	last := chainRow("chain-a", "", "plan", "ceiling", "failed")
	last.DisplayID = "owner/repo#1"
	agg := New(caps)
	addRows(t, agg,
		chainRow("chain-a", "specify", "", "advanced", "succeeded"),
		chainRow("chain-b", "solo", "", "", "succeeded"),
		chainRow("chain-a", "plan", "specify", "", "succeeded"),
		chainRow("chain-c", "specify", "", "", "succeeded"),
		chainRow("chain-a", "plan", "specify", "", "failed"),
		chainRow("chain-b", "solo", "", "failed", "failed"),
		chainRow("", "plan", "specify", "advanced", "succeeded"),
		last,
	)

	report := agg.Report(fixedNow, "/wf", "/db", nil, nil)

	if len(report.ByChain) != 2 {
		t.Fatalf("by_chain = %+v, want chain-a and chain-b only: a row without a chain and a chain without a hop stay out", report.ByChain)
	}
	a, b := report.ByChain[0], report.ByChain[1]
	if a.Name != "chain-a" || a.Runs != 4 || a.Issue != "owner/repo#1" || !slices.Equal(a.Stages, []string{"specify", "plan"}) || a.LastHop != "ceiling" {
		t.Errorf("by_chain[0] name, runs, issue, stages, last_hop = %q, %d, %q, %v, %q, want %q, 4, %q, [specify plan], %q", a.Name, a.Runs, a.Issue, a.Stages, a.LastHop, "chain-a", "owner/repo#1", "ceiling")
	}
	if b.Name != "chain-b" || b.Issue != "PROJ-chain-b" || b.LastHop != "failed" {
		t.Errorf("by_chain[1] name, issue, last_hop = %q, %q, %q, want %q, %q, %q", b.Name, b.Issue, b.LastHop, "chain-b", "PROJ-chain-b", "failed")
	}
	solo := findGroup(t, report.ByRule, "solo")
	solo.Name = "chain-b"
	if !reflect.DeepEqual(b.Group, solo) {
		t.Errorf("by_chain[1] figures = %+v, want the by_rule figures of the same rows: %+v", b.Group, solo)
	}
}

func TestAggregatorReport_WarningsComposition(t *testing.T) {
	t.Parallel()

	const funnelMissing = "this database was written before sortie recorded which runs required action, so the report leaves out the funnel of each stage chain, cost, and the breakdown by skill. Run sortie once with this workflow to add them."
	const funnelMissingWithoutChains = "this database was written before sortie recorded which runs required action, so the report leaves out cost and the breakdown by skill. Run sortie once with this workflow to add them."
	const everyLaterTierMissing = "this database was written before sortie recorded the model of each run, stage chains, what started each run, the first run after each stage hop, and the skill calls of each run, so the report leaves out the breakdown by model, the breakdown by stage chain, the breakdown by retry trigger, the breakdown by run origin, cost, and the breakdown by skill. Run sortie once with this workflow to add them."
	const skillsMissing = "this database was written before sortie recorded the skill calls of each run, so the report leaves out the breakdown by skill. Run sortie once with this workflow to add it."

	supportedCaps := withTriggerBreakdown(fullCaps)
	supportedCaps.HasSkills = true
	unblockedCaps := supportedCaps
	unblockedCaps.HasAgentBlocked = false
	unskilledCaps := supportedCaps
	unskilledCaps.HasSkills = false

	tests := []struct {
		name    string
		caps    storage.RunHistoryCapabilities
		stages  Stages
		want    []string
		funnels bool
	}{
		{name: "every breakdown supported", caps: supportedCaps, want: []string{}},
		{name: "chains configured and every breakdown supported", caps: supportedCaps, stages: threeStageChain, want: []string{}, funnels: true},
		{name: "funnel column missing while chains exist", caps: unblockedCaps, stages: threeStageChain, want: []string{funnelMissing}},
		{name: "funnel column missing without chains", caps: unblockedCaps, want: []string{funnelMissingWithoutChains}},
		{name: "every column after the full tier missing", caps: fullCaps, want: []string{everyLaterTierMissing}},
		{name: "skills column missing while every earlier tier holds", caps: unskilledCaps, want: []string{skillsMissing}},
		{name: "schema not full", caps: storage.RunHistoryCapabilities{}, want: []string{degradedSchemaWarning(storage.RunHistoryCapabilities{})}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agg := New(tt.caps)
			agg.SetStages(tt.stages)

			report := agg.Report(fixedNow, "/wf", "/db", nil, nil)

			got := report.Warnings
			if got == nil {
				t.Fatalf("Report(caps=%+v).Warnings = nil, want a non-nil list", tt.caps)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("Report(caps=%+v).Warnings = %q, want %q", tt.caps, got, tt.want)
			}
			if report.ChainFunnels == nil || (len(report.ChainFunnels) > 0) != tt.funnels {
				t.Errorf("Report(caps=%+v, chains=%v).ChainFunnels = %+v, want a non-nil list, non-empty %t", tt.caps, tt.stages.Chains, report.ChainFunnels, tt.funnels)
			}
		})
	}
}

func TestStatsRecordedCost(t *testing.T) {
	t.Parallel()

	row := func(status string, input int64, usd *float64, costStatus string) storage.RunStatsRow {
		return withCost(modelRow("claude-code", "model-a", status, input), usd, costStatus)
	}
	priced := row("succeeded", 1_000_000, new(15.0), "estimated")
	zero := row("failed", 0, new(0.0), "estimated")
	unpriced := row("failed", 1_000_000, nil, "unpriced")
	nullStatus := row("failed", 1_000_000, nil, "")
	cacheWriteOnly := row("failed", 0, nil, "unpriced")
	cacheWriteOnly.CacheWriteTokens = 1
	partial := row("failed", 1_000_000, new(8.0), "partial")
	partialSpentNothing := row("failed", 0, nil, "partial")
	uncosted := recordedCaps
	uncosted.HasRecordedCost = false

	tests := []struct {
		name         string
		caps         storage.RunHistoryCapabilities
		rows         []storage.RunStatsRow
		wantCost     *float64
		wantUnpriced int
		wantPartial  int
	}{
		{"priced, spent nothing, unpriced", recordedCaps, []storage.RunStatsRow{priced, zero, unpriced}, new(15.0), 1, 0},
		{"spent nothing and unpriced", recordedCaps, []storage.RunStatsRow{zero, unpriced}, nil, 1, 0},
		{"spend with a NULL status", recordedCaps, []storage.RunStatsRow{nullStatus}, nil, 1, 0},
		{"spend in the cache-write counter only", recordedCaps, []storage.RunStatsRow{cacheWriteOnly}, nil, 1, 0},
		{"ci_failed beside a priced run", recordedCaps, []storage.RunStatsRow{priced, row("ci_failed", 1_000_000, nil, "")}, new(15.0), 0, 0},
		{"ci_failed alone", recordedCaps, []storage.RunStatsRow{row("ci_failed", 1_000_000, new(0.0), "estimated")}, nil, 0, 0},
		{"partial beside a priced run", recordedCaps, []storage.RunStatsRow{priced, partial}, new(23.0), 0, 1},
		{"partial that spent nothing", recordedCaps, []storage.RunStatsRow{partialSpentNothing}, nil, 0, 1},
		{"ci_failed partial", recordedCaps, []storage.RunStatsRow{row("ci_failed", 1_000_000, new(8.0), "partial")}, nil, 0, 0},
		{"cost columns missing", uncosted, []storage.RunStatsRow{priced, unpriced, partial}, nil, 0, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agg := New(tt.caps)
			addRows(t, agg, tt.rows...)

			report := agg.Report(fixedNow, "/wf", "/db", nil, nil)
			s := report.Summary

			if !reflect.DeepEqual(s.CostUSD, tt.wantCost) || s.CostUnpricedRuns != tt.wantUnpriced || s.CostPartialRuns != tt.wantPartial {
				t.Errorf("Summary cost_usd, cost_unpriced_runs, cost_partial_runs over %q = %v, %d, %d, want %v, %d, %d",
					tt.name, derefCost(s.CostUSD), s.CostUnpricedRuns, s.CostPartialRuns, derefCost(tt.wantCost), tt.wantUnpriced, tt.wantPartial)
			}
			var groupPartial int
			for _, g := range report.ByModel {
				groupPartial += g.CostPartialRuns
			}
			if groupPartial != tt.wantPartial {
				t.Errorf("by_model cost_partial_runs over %q = %d, want %d", tt.name, groupPartial, tt.wantPartial)
			}
		})
	}
}
