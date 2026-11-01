package sqlite

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/sortie-ai/sortie/internal/storage"
)

func appendProvenanceRun(t *testing.T, s *Store, issueID string, attempt int, run storage.RunHistory) {
	t.Helper()
	run.IssueID, run.Attempt = issueID, attempt
	run.Identifier = fmt.Sprintf("%s-%d", issueID, attempt)
	run.StartedAt, run.CompletedAt = "2026-03-19T10:00:00Z", "2026-03-19T10:00:30Z"
	run.Status = "succeeded"
	run.DispatchID = new(fmt.Sprintf("dispatch-%s-%d", issueID, attempt))
	if _, err := s.AppendRunHistoryWithEvents(context.Background(), run, nil, unboundedRunEvents); err != nil {
		t.Fatalf("AppendRunHistoryWithEvents(%s attempt %d): %v", issueID, attempt, err)
	}
}

func unmeasuredRun(completeness string, amounts ...storage.ReportedAmount) storage.RunHistory {
	return storage.RunHistory{UsageCompleteness: completeness, ReportedAmounts: amounts}
}

func pricedRun(costs map[string]storage.ModelCost, amounts ...storage.ReportedAmount) storage.RunHistory {
	run := storage.RunHistory{TokensMeasured: true, UsageCompleteness: storage.UsageComplete, ReportedAmounts: amounts}
	run.ModelUsage, run.ModelCosts = map[string]storage.ModelTokens{}, costs
	for model := range costs {
		run.ModelUsage[model] = storage.ModelTokens{InputTokens: 10, OutputTokens: 5}
	}
	return run
}

func TestIssueSpendProvenance(t *testing.T) {
	t.Parallel()

	credits := func(model string, amount float64, lowerBound bool) storage.ReportedAmount {
		return storage.ReportedAmount{Unit: "credits", Model: model, Amount: amount, LowerBound: lowerBound}
	}
	usd := storage.ReportedAmount{Unit: "USD", Model: "m1", Amount: 3}
	agent := func(cost float64) storage.ModelCost {
		return storage.ModelCost{EstimatedCostUSD: new(cost), CostStatus: "estimated", PriceSource: "agent"}
	}
	table := storage.ModelCost{EstimatedCostUSD: new(2.0), CostStatus: "estimated", PriceSource: "table"}

	tests := []struct {
		name string
		runs []storage.RunHistory
		want storage.IssueSpendProvenance
	}{
		{
			name: "mixed causes; vendor units summed apart from US dollars; agent share exact",
			runs: []storage.RunHistory{
				unmeasuredRun(storage.UsageUnmeasured, credits("m1", 4, false), credits("m2", 2.5, true)),
				unmeasuredRun(storage.UsageUnmeasured, usd),
				unmeasuredRun(""),
				pricedRun(map[string]storage.ModelCost{"m1": agent(0.5), "m2": table, "": agent(0.25)}, credits("m1", 1, false)),
				pricedRun(map[string]storage.ModelCost{"m1": agent(0.25)}),
			},
			want: storage.IssueSpendProvenance{
				UnmeasuredCause:   storage.UnmeasuredMixed,
				UnmeasuredUnits:   []string{},
				VendorAmounts:     []storage.UnitAmount{{Unit: "credits", Amount: 7.5, LowerBound: true}},
				AgentPricedUSD:    new(1.0),
				AgentPricedModels: []string{"", "m1"},
			},
		},
		{
			name: "a US dollar report alone is amounts only with no vendor amount",
			runs: []storage.RunHistory{unmeasuredRun(storage.UsageUnmeasured, usd)},
			want: storage.IssueSpendProvenance{
				UnmeasuredCause: storage.UnmeasuredAmountsOnly, UnmeasuredUnits: []string{"USD"},
				VendorAmounts: []storage.UnitAmount{}, AgentPricedModels: []string{},
			},
		},
		{
			name: "a pre-provenance unmeasured run reads unknown",
			runs: []storage.RunHistory{unmeasuredRun("")},
			want: storage.IssueSpendProvenance{
				UnmeasuredCause: storage.UnmeasuredUnknown, UnmeasuredUnits: []string{},
				VendorAmounts: []storage.UnitAmount{}, AgentPricedModels: []string{},
			},
		},
		{
			name: "no runs leaves every slice empty and the share nil",
			want: storage.IssueSpendProvenance{UnmeasuredUnits: []string{}, VendorAmounts: []storage.UnitAmount{}, AgentPricedModels: []string{}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := openTestStore(t)
			migrateOrFatal(t, s)
			for i, run := range tt.runs {
				appendProvenanceRun(t, s, "ISS-1", i+1, run)
			}
			appendProvenanceRun(t, s, "ISS-OTHER", 1, unmeasuredRun(storage.UsageUnmeasured, credits("m1", 100, false)))

			got, err := s.IssueSpendProvenance(context.Background(), "ISS-1")
			if err != nil {
				t.Fatalf("IssueSpendProvenance(ISS-1): %v", err)
			}

			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("IssueSpendProvenance(ISS-1) = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestQueryRunReportedAmounts(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	appendProvenanceRun(t, s, "ISS-1", 1, unmeasuredRun(storage.UsageUnmeasured,
		storage.ReportedAmount{Unit: "credits", Amount: 2},
		storage.ReportedAmount{Unit: "USD", Model: "m2", Amount: 1},
		storage.ReportedAmount{Unit: "USD", Model: "m1", Amount: 3, LowerBound: true},
	))
	appendProvenanceRun(t, s, "ISS-1", 2, unmeasuredRun(storage.UsageUnmeasured))
	runs := queryAllRuns(t, s, 10)
	var ids []int64
	var withAmounts int64
	for _, run := range runs {
		ids = append(ids, run.ID)
		if run.Attempt == 1 {
			withAmounts = run.ID
		}
	}

	got, err := s.QueryRunReportedAmounts(context.Background(), ids)
	if err != nil {
		t.Fatalf("QueryRunReportedAmounts(%v): %v", ids, err)
	}

	want := map[int64][]storage.ReportedAmount{withAmounts: {
		{Unit: "USD", Model: "m1", Amount: 3, LowerBound: true},
		{Unit: "USD", Model: "m2", Amount: 1},
		{Unit: "credits", Amount: 2},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("QueryRunReportedAmounts(%v) = %+v, want %+v", ids, got, want)
	}
}
