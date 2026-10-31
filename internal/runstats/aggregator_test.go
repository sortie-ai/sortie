package runstats

import (
	"encoding/json"
	"maps"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/persistence"
)

var fullCaps = persistence.RunHistoryCapabilities{
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

func withCost(row persistence.RunStatsRow, usd *float64, costStatus string) persistence.RunStatsRow {
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

func addRows(t *testing.T, agg *Aggregator, rows ...persistence.RunStatsRow) {
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

func withModelBreakdown(caps persistence.RunHistoryCapabilities) persistence.RunHistoryCapabilities {
	caps.HasConfiguredSettings = true
	return caps
}

func withTriggerBreakdown(caps persistence.RunHistoryCapabilities) persistence.RunHistoryCapabilities {
	caps = withModelBreakdown(caps)
	caps.HasStageChains = true
	caps.HasTrigger = true
	caps.HasFirstAfterHop = true
	return caps
}

func triggerRow(issueID string, trigger *string) persistence.RunStatsRow {
	row := modelRow("claude-code", "model-a", "succeeded", 0)
	row.IssueID, row.Trigger = issueID, trigger
	return row
}

func reportedModelRow(configured, reported string) persistence.RunStatsRow {
	row := modelRow("claude-code", configured, "succeeded", 0)
	row.ReportedModel = reported
	return row
}

func modelRow(adapter, model, status string, input int64) persistence.RunStatsRow {
	return persistence.RunStatsRow{
		Status: status, AgentAdapter: adapter, ConfiguredModel: model,
		StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T00:10:00Z",
		InputTokens: input, TokensMeasured: true,
	}
}

func chainRow(chain, rule, previous, result, status string) persistence.RunStatsRow {
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

func withUsageProvenance(caps persistence.RunHistoryCapabilities) persistence.RunHistoryCapabilities {
	caps.HasUsageProvenance = true
	return caps
}

func measuredRow(completeness string) persistence.RunStatsRow {
	row := withCost(modelRow("claude-code", "model-a", "succeeded", 1_000), new(1.0), "estimated")
	row.UsageCompleteness = completeness
	return row
}

func unmeasuredRow(status, completeness string, units ...string) persistence.RunStatsRow {
	row := modelRow("claude-code", "model-a", status, 0)
	row.TokensMeasured, row.UsageCompleteness, row.ReportedUnits = false, completeness, units
	return row
}

func withParts(row persistence.RunStatsRow, parts ...persistence.RunModelPart) persistence.RunStatsRow {
	row.Parts = parts
	return row
}

func pricedPart(model, priceSource string, input int64, usd float64) persistence.RunModelPart {
	return persistence.RunModelPart{
		Model: model, Tokens: persistence.ModelTokens{InputTokens: input},
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
			withCost(persistence.RunStatsRow{
				Status: "succeeded", AgentAdapter: "mock", TurnsCompleted: 4,
				StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T00:10:00Z",
				InputTokens: 1_000_000, TokensMeasured: true,
			}, new(10.0), "estimated"),
			withCost(persistence.RunStatsRow{
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
			rows []persistence.RunStatsRow
			want flags
		}{
			{"complete runs only", []persistence.RunStatsRow{complete}, flags{units: none}},
			{"unaccounted turns flag tokens, not cost", []persistence.RunStatsRow{complete, unaccounted}, flags{lowerBoundRuns: 1, tokensLowerBound: true, units: none}},
			{"an unpriced run flags cost", []persistence.RunStatsRow{complete, unpriced}, flags{costLowerBound: true, units: none}},
			{"a partial cost flags cost", []persistence.RunStatsRow{partial}, flags{costLowerBound: true, units: none}},
			{"reported units beat the status", []persistence.RunStatsRow{complete, unmeasuredRow("failed", "unmeasured", "credits")},
				flags{tokensLowerBound: true, costLowerBound: true, cause: new("amounts_only"), units: []string{"credits"}}},
			{"different causes read mixed", []persistence.RunStatsRow{complete, unmeasuredRow("failed", "unmeasured"), unmeasuredRow("succeeded", "unmeasured")},
				flags{tokensLowerBound: true, costLowerBound: true, cause: new("mixed"), units: none}},
			{"nothing measured leaves no figure to flag", []persistence.RunStatsRow{unmeasuredRow("failed", "unmeasured")}, flags{cause: new("ended_early"), units: none}},
			{"a run recorded before provenance reads unknown", []persistence.RunStatsRow{unmeasuredRow("succeeded", "")}, flags{cause: new("unknown"), units: none}},
			{"a two-part run counts its agent part once", []persistence.RunStatsRow{twoParts}, flags{units: none, agentPricedUSD: new(2.0)}},
			{"a ci_failed run adds no agent part", []persistence.RunStatsRow{complete, ciFailed}, flags{units: none}},
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

	addRows(t, agg, persistence.RunStatsRow{
		Status:         "ci_failed",
		StartedAt:      "2026-01-01T00:00:00Z",
		CompletedAt:    "2026-01-01T00:00:00Z",
		TokensMeasured: true,
	})
	reviewRun := func(adapter string) persistence.RunStatsRow {
		return persistence.RunStatsRow{
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

		agg := New(persistence.RunHistoryCapabilities{})
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
		persistence.RunStatsRow{
			Status: "succeeded", StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T00:01:00Z",
			ReviewMetadata: reviewMetaPtr(t, domain.ReviewMetadata{FinalVerdict: "pass", TotalIterations: 1}),
		},
		persistence.RunStatsRow{
			Status: "failed", StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T00:01:00Z",
			ReviewMetadata: reviewMetaPtr(t, domain.ReviewMetadata{FinalVerdict: "iterate", TotalIterations: 3, CapReached: true}),
		},
		persistence.RunStatsRow{
			Status: "failed", StartedAt: "2026-01-01T00:00:00Z", CompletedAt: "2026-01-01T00:01:00Z",
			ReviewMetadata: reviewMetaPtr(t, domain.ReviewMetadata{FinalVerdict: "", TotalIterations: 2}),
		},
		persistence.RunStatsRow{
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
		addRows(t, agg, persistence.RunStatsRow{
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

			for _, caps := range []persistence.RunHistoryCapabilities{{}, fullCaps} {
				agg := New(caps)
				addRows(t, agg, persistence.RunStatsRow{Status: tt.status})

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
		caps   persistence.RunHistoryCapabilities
		want   []string
		absent []string
	}{
		{
			name: "nothing recorded names every group once",
			caps: persistence.RunHistoryCapabilities{},
			want: []string{
				"before sortie recorded turns, self-review results, dispatch-rule routing, tokens and cost",
				"stage chains, what started each run, the first run after each stage hop, which runs required action",
				"Run sortie once with this workflow to add them.",
			},
			absent: []string{"does carry", "falls back"},
		},
		{
			name: "partly migrated discloses the figures it drops anyway",
			caps: persistence.RunHistoryCapabilities{HasTurnsCompleted: true, HasReviewMetadata: true},
			want: []string{
				"before sortie recorded dispatch-rule routing, tokens and cost",
				"also leaves out turns, self-review results, which this database does carry",
			},
		},
		{
			name: "only tokens absent still discloses the other three",
			caps: persistence.RunHistoryCapabilities{
				HasTurnsCompleted: true, HasReviewMetadata: true, HasRuleRouting: true,
			},
			want: []string{
				"before sortie recorded tokens and cost",
				"also leaves out turns, self-review results, dispatch-rule routing, which this database does carry",
			},
		},
		{
			name: "only the measurement record absent names it in plain language",
			caps: persistence.RunHistoryCapabilities{
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
			caps: persistence.RunHistoryCapabilities{},
			want: []string{"before sortie recorded", "the model each run was configured with"},
		},
		{
			name:   "configured model group is listed among the carried ones when present",
			caps:   persistence.RunHistoryCapabilities{HasTurnsCompleted: true, HasConfiguredSettings: true},
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
	part := func(model string, input, output int64, usd *float64, costStatus string) persistence.RunModelPart {
		return persistence.RunModelPart{
			Model: model, Tokens: persistence.ModelTokens{InputTokens: input, OutputTokens: output},
			EstimatedCostUSD: usd, CostStatus: costStatus,
		}
	}
	split := func(unaccounted int, runCost *float64, runStatus string, parts ...persistence.RunModelPart) persistence.RunStatsRow {
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
		rows []persistence.RunStatsRow
		want map[string]share
	}{
		{
			name: "a run priced per model lands in each spent part's row; a run without parts lands once under its reported model",
			rows: []persistence.RunStatsRow{
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
			rows: []persistence.RunStatsRow{split(0, new(9.0), "estimated",
				pricedPart("model-a", "agent", 600, 6.0),
				pricedPart("model-b", "operator", 300, 3.0))},
			want: map[string]share{
				"model-a": {runs: 1, succeeded: 1, input: 600, total: 600, cost: 6.0, agent: 6.0},
				"model-b": {runs: 1, succeeded: 1, input: 300, total: 300, cost: 3.0},
			},
		},
		{
			name: "unaccounted turns mark the priced part partial and leave the unpriced part unpriced only",
			rows: []persistence.RunStatsRow{split(2, new(4.0), "partial",
				part("model-a", 100, 10, new(4.0), "estimated"),
				part("model-b", 200, 20, nil, "unpriced"))},
			want: map[string]share{
				"model-a": {runs: 1, succeeded: 1, input: 100, output: 10, total: 110, cost: 4.0, partial: 1, lowerBound: 1},
				"model-b": {runs: 1, succeeded: 1, input: 200, output: 20, total: 220, unpriced: 1, lowerBound: 1},
			},
		},
		{
			name: "without unaccounted turns the estimated part counts in neither shortfall",
			rows: []persistence.RunStatsRow{split(0, new(4.0), "partial",
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

func TestStatsSpend(t *testing.T) {
	t.Parallel()

	run := func(completedAt string, usd *float64, costStatus string) persistence.RunStatsRow {
		row := withCost(modelRow("claude-code", "model-a", "succeeded", 1_000), usd, costStatus)
		row.CompletedAt = completedAt
		return row
	}
	unmeasured := run("2026-08-04T12:00:00Z", nil, "")
	unmeasured.TokensMeasured = false
	ciFailed := run("2026-08-04T13:00:00Z", nil, "")
	ciFailed.Status = "ci_failed"
	sevenDays := new(fixedNow.Add(-168 * time.Hour))

	tests := []struct {
		name        string
		since       *time.Time
		rows        []persistence.RunStatsRow
		wantPeriod  string
		wantBuckets []SpendBucket
	}{
		{
			name:  "runs on several UTC days of a 168 hour range land in their own days",
			since: sevenDays,
			rows: []persistence.RunStatsRow{
				run("2026-07-31T12:00:00Z", new(1.0), "estimated"),
				run("2026-08-02T08:00:00Z", new(0.5), "estimated"),
				run("2026-08-02T23:30:00Z", new(2.0), "estimated"),
				run("2026-08-05T10:00:00Z", nil, "unpriced"),
			},
			wantPeriod: "day",
			wantBuckets: []SpendBucket{
				{Start: "2026-07-31T00:00:00Z", Runs: 1, CostUSD: new(1.0)},
				{Start: "2026-08-01T00:00:00Z"},
				{Start: "2026-08-02T00:00:00Z", Runs: 2, CostUSD: new(2.5)},
				{Start: "2026-08-03T00:00:00Z"},
				{Start: "2026-08-04T00:00:00Z"},
				{Start: "2026-08-05T00:00:00Z", Runs: 1, CostUnpricedRuns: 1},
				{Start: "2026-08-06T00:00:00Z"},
				{Start: "2026-08-07T00:00:00Z"},
			},
		},
		{
			name: "an open range lists months and folds each run as the summary does",
			rows: []persistence.RunStatsRow{
				run("2026-06-15T10:00:00Z", new(4.0), "estimated"),
				run("2026-07-20T10:00:00Z", new(3.0), "partial"),
				run("2026-08-03T10:00:00Z", nil, "unpriced"),
				unmeasured,
				ciFailed,
			},
			wantPeriod: "month",
			wantBuckets: []SpendBucket{
				{Start: "2026-06-01T00:00:00Z", Runs: 1, CostUSD: new(4.0)},
				{Start: "2026-07-01T00:00:00Z", Runs: 1, CostUSD: new(3.0), CostPartialRuns: 1, CostLowerBound: true},
				{Start: "2026-08-01T00:00:00Z", Runs: 3, CostUnpricedRuns: 1, TokensUnmeasuredRuns: 1},
			},
		},
		{
			name: "an unmeasured run flags a priced month and its days sum their agent-priced parts",
			rows: []persistence.RunStatsRow{
				withParts(run("2026-08-02T10:00:00Z", new(2.0), "estimated"), pricedPart("model-a", "agent", 1_000, 1.0)),
				withParts(run("2026-08-03T10:00:00Z", new(1.0), "estimated"), pricedPart("model-a", "agent", 1_000, 0.5)),
				unmeasured,
			},
			wantPeriod: "month",
			wantBuckets: []SpendBucket{
				{Start: "2026-08-01T00:00:00Z", Runs: 3, CostUSD: new(3.0), TokensUnmeasuredRuns: 1, CostLowerBound: true, AgentPricedUSD: new(1.5)},
			},
		},
		{
			name: "an unpriced run flags a priced month",
			rows: []persistence.RunStatsRow{
				run("2026-08-02T10:00:00Z", new(1.0), "estimated"),
				run("2026-08-03T10:00:00Z", nil, "unpriced"),
			},
			wantPeriod: "month",
			wantBuckets: []SpendBucket{
				{Start: "2026-08-01T00:00:00Z", Runs: 2, CostUSD: new(1.0), CostUnpricedRuns: 1, CostLowerBound: true},
			},
		},
		{
			name:       "a 2160 hour span gives days",
			since:      new(fixedNow.Add(-2160 * time.Hour)),
			rows:       []persistence.RunStatsRow{run("2026-08-06T10:00:00Z", new(1.0), "estimated")},
			wantPeriod: "day",
		},
		{
			name:       "a span above 2160 hours gives months",
			since:      new(fixedNow.Add(-2160*time.Hour - time.Second)),
			rows:       []persistence.RunStatsRow{run("2026-08-06T10:00:00Z", new(1.0), "estimated")},
			wantPeriod: "month",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agg := New(recordedCaps)
			addRows(t, agg, tt.rows...)

			report := agg.Report(fixedNow, "/wf", "/db", tt.since, nil)

			spend := report.Spend
			if spend == nil {
				t.Fatalf("Report(since=%v).Spend = nil, want a listing on a database with recorded cost", tt.since)
			}
			if spend.Period != tt.wantPeriod {
				t.Errorf("Report(since=%v).Spend.Period = %q, want %q", tt.since, spend.Period, tt.wantPeriod)
			}
			if tt.wantBuckets != nil && !reflect.DeepEqual(spend.Buckets, tt.wantBuckets) {
				t.Errorf("Report(since=%v).Spend.Buckets = %+v, want %+v", tt.since, spend.Buckets, tt.wantBuckets)
			}
			var sum SpendBucket
			var cost float64
			for _, b := range spend.Buckets {
				sum.Runs += b.Runs
				sum.CostUnpricedRuns += b.CostUnpricedRuns
				sum.CostPartialRuns += b.CostPartialRuns
				sum.TokensUnmeasuredRuns += b.TokensUnmeasuredRuns
				if b.CostUSD != nil {
					cost += *b.CostUSD
				}
			}
			s := report.Summary
			if sum.Runs != s.Runs || sum.CostUnpricedRuns != s.CostUnpricedRuns || sum.CostPartialRuns != s.CostPartialRuns || sum.TokensUnmeasuredRuns != s.TokensUnmeasuredRuns {
				t.Errorf("Spend bucket runs, unpriced, partial, unmeasured sums = %d, %d, %d, %d, want the summary's %d, %d, %d, %d",
					sum.Runs, sum.CostUnpricedRuns, sum.CostPartialRuns, sum.TokensUnmeasuredRuns,
					s.Runs, s.CostUnpricedRuns, s.CostPartialRuns, s.TokensUnmeasuredRuns)
			}
			var want float64
			if s.CostUSD != nil {
				want = *s.CostUSD
			}
			if math.Abs(cost-want) > 1e-6 {
				t.Errorf("Spend bucket cost_usd sums to %v, want summary.cost_usd %v", cost, want)
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
		caps persistence.RunHistoryCapabilities
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

func originRow(trigger *string, reactionKind, stagePrevious string, firstAfterHop bool) persistence.RunStatsRow {
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
		caps persistence.RunHistoryCapabilities
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

func skillRow(skills *string) persistence.RunStatsRow {
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
	mixed := []persistence.RunStatsRow{
		skillRow(new(`[{"skill":"plan"},{"skill":"test","tool_error":true},{"skill":"test"}]`)),
		skillRow(new(`[]`)),
		skillRow(nil),
		skillRow(new(`[{"skill":"plan"}]`)),
	}
	tests := []struct {
		name string
		caps persistence.RunHistoryCapabilities
		rows []persistence.RunStatsRow
		want *SkillCallReport
	}{
		{"runs count once per skill and a NULL run is unrecorded", skilled, mixed, &SkillCallReport{UnrecordedRuns: 1, BySkill: []SkillCount{
			{Skill: "plan", Runs: 2, Calls: 2},
			{Skill: "test", Runs: 1, Calls: 2, FailedCalls: 1},
		}}},
		{"a value that does not decode is unrecorded", skilled, []persistence.RunStatsRow{skillRow(new("not json"))}, &SkillCallReport{UnrecordedRuns: 1, BySkill: []SkillCount{}}},
		{"runs that called nothing leave an empty, non-nil list", skilled, []persistence.RunStatsRow{skillRow(new("[]"))}, &SkillCallReport{BySkill: []SkillCount{}}},
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
		caps    persistence.RunHistoryCapabilities
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
		{name: "schema not full", caps: persistence.RunHistoryCapabilities{}, want: []string{degradedSchemaWarning(persistence.RunHistoryCapabilities{})}},
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

	row := func(status string, input int64, usd *float64, costStatus string) persistence.RunStatsRow {
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
		caps         persistence.RunHistoryCapabilities
		rows         []persistence.RunStatsRow
		wantCost     *float64
		wantUnpriced int
		wantPartial  int
	}{
		{"priced, spent nothing, unpriced", recordedCaps, []persistence.RunStatsRow{priced, zero, unpriced}, new(15.0), 1, 0},
		{"spent nothing and unpriced", recordedCaps, []persistence.RunStatsRow{zero, unpriced}, nil, 1, 0},
		{"spend with a NULL status", recordedCaps, []persistence.RunStatsRow{nullStatus}, nil, 1, 0},
		{"spend in the cache-write counter only", recordedCaps, []persistence.RunStatsRow{cacheWriteOnly}, nil, 1, 0},
		{"ci_failed beside a priced run", recordedCaps, []persistence.RunStatsRow{priced, row("ci_failed", 1_000_000, nil, "")}, new(15.0), 0, 0},
		{"ci_failed alone", recordedCaps, []persistence.RunStatsRow{row("ci_failed", 1_000_000, new(0.0), "estimated")}, nil, 0, 0},
		{"partial beside a priced run", recordedCaps, []persistence.RunStatsRow{priced, partial}, new(23.0), 0, 1},
		{"partial that spent nothing", recordedCaps, []persistence.RunStatsRow{partialSpentNothing}, nil, 0, 1},
		{"ci_failed partial", recordedCaps, []persistence.RunStatsRow{row("ci_failed", 1_000_000, new(8.0), "partial")}, nil, 0, 0},
		{"cost columns missing", uncosted, []persistence.RunStatsRow{priced, unpriced, partial}, nil, 0, 0},
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
