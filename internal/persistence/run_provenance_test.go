package persistence

import (
	"context"
	"fmt"
	"reflect"
	"testing"
)

func TestUsageCompletenessOf(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		measured bool
		turns    int
		want     string
	}{
		{"no figure is unmeasured", false, 0, UsageUnmeasured},
		{"no figure stays unmeasured whatever the turns", false, 2, UsageUnmeasured},
		{"measured with an uncounted turn is a lower bound", true, 1, UsageLowerBound},
		{"measured with every turn counted is complete", true, 0, UsageComplete},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := UsageCompletenessOf(tt.measured, tt.turns)

			if got != tt.want {
				t.Errorf("UsageCompletenessOf(%v, %d) = %q, want %q", tt.measured, tt.turns, got, tt.want)
			}
		})
	}
}

func TestUnmeasuredCauseOf(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		status        string
		completeness  string
		reportedUnits []string
		want          UnmeasuredCause
	}{
		{"pre-provenance run is unknown even with units", "failed", "", []string{"credits"}, UnmeasuredUnknown},
		{"units reported by a succeeded run", "succeeded", UsageUnmeasured, []string{"credits"}, UnmeasuredAmountsOnly},
		{"a US dollar unit alone is amounts only", "succeeded", UsageUnmeasured, []string{"USD"}, UnmeasuredAmountsOnly},
		{"units win over a failed status", "failed", UsageUnmeasured, []string{"credits"}, UnmeasuredAmountsOnly},
		{"non-succeeded run without units ended early", "stopped", UsageUnmeasured, nil, UnmeasuredEndedEarly},
		{"succeeded run without units sent no usage", "succeeded", UsageUnmeasured, nil, UnmeasuredNoUsage},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := UnmeasuredCauseOf(tt.status, tt.completeness, tt.reportedUnits)

			if got != tt.want {
				t.Errorf("UnmeasuredCauseOf(%q, %q, %v) = %q, want %q", tt.status, tt.completeness, tt.reportedUnits, got, tt.want)
			}
		})
	}
}

func TestUnmeasuredCauses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		adds      []causeKey
		wantCause UnmeasuredCause
		wantUnits []string
	}{
		{"nothing added", nil, "", []string{}},
		{
			"one key serves its distinct sorted units",
			[]causeKey{{cause: UnmeasuredAmountsOnly, units: []string{"credits", "USD", "credits"}}},
			UnmeasuredAmountsOnly, []string{"USD", "credits"},
		},
		{
			"the same key twice is not mixed",
			[]causeKey{
				{cause: UnmeasuredAmountsOnly, units: []string{"credits", "USD"}},
				{cause: UnmeasuredAmountsOnly, units: []string{"USD", "credits"}},
			},
			UnmeasuredAmountsOnly, []string{"USD", "credits"},
		},
		{
			"a cause other than amounts only serves no units",
			[]causeKey{{cause: UnmeasuredNoUsage}, {cause: UnmeasuredNoUsage}},
			UnmeasuredNoUsage, []string{},
		},
		{
			"the same cause over different units is mixed",
			[]causeKey{
				{cause: UnmeasuredAmountsOnly, units: []string{"credits"}},
				{cause: UnmeasuredAmountsOnly, units: []string{"USD"}},
			},
			UnmeasuredMixed, []string{},
		},
		{
			"different causes are mixed",
			[]causeKey{{cause: UnmeasuredNoUsage}, {cause: UnmeasuredEndedEarly}},
			UnmeasuredMixed, []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var causes UnmeasuredCauses
			for _, add := range tt.adds {
				causes.Add(add.cause, add.units)
			}

			gotCause, gotUnits := causes.Result()

			if gotCause != tt.wantCause || !reflect.DeepEqual(gotUnits, tt.wantUnits) {
				t.Errorf("UnmeasuredCauses.Result() = %q, %#v, want %q, %#v", gotCause, gotUnits, tt.wantCause, tt.wantUnits)
			}
		})
	}
}

func TestParseUsageProvenance(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
		want []UsagePair
	}{
		{"empty text is an empty non-nil slice", "", []UsagePair{}},
		{
			"live first, then by source, then by strategy; unknown values pass through",
			"output:records,journal:records,zeta:live,alpha:pull,alpha:live",
			[]UsagePair{
				{Source: "alpha", Strategy: "live"},
				{Source: "zeta", Strategy: "live"},
				{Source: "alpha", Strategy: "pull"},
				{Source: "journal", Strategy: "records"},
				{Source: "output", Strategy: "records"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := ParseUsageProvenance(tt.text)

			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseUsageProvenance(%q) = %+v, want %+v", tt.text, got, tt.want)
			}
		})
	}
}

func appendProvenanceRun(t *testing.T, s *Store, issueID string, attempt int, run RunHistory) {
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

func unmeasuredRun(completeness string, amounts ...ReportedAmount) RunHistory {
	return RunHistory{UsageCompleteness: completeness, ReportedAmounts: amounts}
}

func pricedRun(costs map[string]ModelCost, amounts ...ReportedAmount) RunHistory {
	run := RunHistory{TokensMeasured: true, UsageCompleteness: UsageComplete, ReportedAmounts: amounts}
	run.ModelUsage, run.ModelCosts = map[string]ModelTokens{}, costs
	for model := range costs {
		run.ModelUsage[model] = ModelTokens{InputTokens: 10, OutputTokens: 5}
	}
	return run
}

func TestIssueSpendProvenance(t *testing.T) {
	t.Parallel()

	credits := func(model string, amount float64, lowerBound bool) ReportedAmount {
		return ReportedAmount{Unit: "credits", Model: model, Amount: amount, LowerBound: lowerBound}
	}
	usd := ReportedAmount{Unit: "USD", Model: "m1", Amount: 3}
	agent := func(cost float64) ModelCost {
		return ModelCost{EstimatedCostUSD: new(cost), CostStatus: "estimated", PriceSource: "agent"}
	}
	table := ModelCost{EstimatedCostUSD: new(2.0), CostStatus: "estimated", PriceSource: "table"}

	tests := []struct {
		name string
		runs []RunHistory
		want IssueSpendProvenance
	}{
		{
			name: "mixed causes; vendor units summed apart from US dollars; agent share exact",
			runs: []RunHistory{
				unmeasuredRun(UsageUnmeasured, credits("m1", 4, false), credits("m2", 2.5, true)),
				unmeasuredRun(UsageUnmeasured, usd),
				unmeasuredRun(""),
				pricedRun(map[string]ModelCost{"m1": agent(0.5), "m2": table, "": agent(0.25)}, credits("m1", 1, false)),
				pricedRun(map[string]ModelCost{"m1": agent(0.25)}),
			},
			want: IssueSpendProvenance{
				UnmeasuredCause:   UnmeasuredMixed,
				UnmeasuredUnits:   []string{},
				VendorAmounts:     []UnitAmount{{Unit: "credits", Amount: 7.5, LowerBound: true}},
				AgentPricedUSD:    new(1.0),
				AgentPricedModels: []string{"", "m1"},
			},
		},
		{
			name: "a US dollar report alone is amounts only with no vendor amount",
			runs: []RunHistory{unmeasuredRun(UsageUnmeasured, usd)},
			want: IssueSpendProvenance{
				UnmeasuredCause: UnmeasuredAmountsOnly, UnmeasuredUnits: []string{"USD"},
				VendorAmounts: []UnitAmount{}, AgentPricedModels: []string{},
			},
		},
		{
			name: "a pre-provenance unmeasured run reads unknown",
			runs: []RunHistory{unmeasuredRun("")},
			want: IssueSpendProvenance{
				UnmeasuredCause: UnmeasuredUnknown, UnmeasuredUnits: []string{},
				VendorAmounts: []UnitAmount{}, AgentPricedModels: []string{},
			},
		},
		{
			name: "no runs leaves every slice empty and the share nil",
			want: IssueSpendProvenance{UnmeasuredUnits: []string{}, VendorAmounts: []UnitAmount{}, AgentPricedModels: []string{}},
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
			appendProvenanceRun(t, s, "ISS-OTHER", 1, unmeasuredRun(UsageUnmeasured, credits("m1", 100, false)))

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
	appendProvenanceRun(t, s, "ISS-1", 1, unmeasuredRun(UsageUnmeasured,
		ReportedAmount{Unit: "credits", Amount: 2},
		ReportedAmount{Unit: "USD", Model: "m2", Amount: 1},
		ReportedAmount{Unit: "USD", Model: "m1", Amount: 3, LowerBound: true},
	))
	appendProvenanceRun(t, s, "ISS-1", 2, unmeasuredRun(UsageUnmeasured))
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

	want := map[int64][]ReportedAmount{withAmounts: {
		{Unit: "USD", Model: "m1", Amount: 3, LowerBound: true},
		{Unit: "USD", Model: "m2", Amount: 1},
		{Unit: "credits", Amount: 2},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("QueryRunReportedAmounts(%v) = %+v, want %+v", ids, got, want)
	}
}
