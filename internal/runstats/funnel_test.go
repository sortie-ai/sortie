package runstats

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/storage"
)

var threeStageChain = Stages{
	Chains:     [][]string{{"specify", "plan", "implement"}},
	LabelRules: []string{"plan"},
}

func passRow(rule, previous, result, status string) storage.RunStatsRow {
	return chainRow("c1", rule, previous, result, status)
}

func blocked(row storage.RunStatsRow) storage.RunStatsRow {
	row.AgentBlocked = true
	return row
}

func withheld(row storage.RunStatsRow) storage.RunStatsRow {
	row.HandoffWithheld = true
	return row
}

func reaction(row storage.RunStatsRow) storage.RunStatsRow {
	row.ReactionKind = "ci"
	return row
}

func lasting(row storage.RunStatsRow, seconds int) storage.RunStatsRow {
	row.CompletedAt = time.Date(2026, 1, 1, 0, 0, seconds, 0, time.UTC).Format(time.RFC3339)
	return row
}

func untimed(row storage.RunStatsRow) storage.RunStatsRow {
	row.CompletedAt = ""
	return row
}

func livePass(rule, previous string, budget bool) LivePass {
	return LivePass{ChainID: "c1", RuleName: rule, PreviousRule: previous, BudgetHeld: budget}
}

func funnelsOf(t *testing.T, stages Stages, until *time.Time, rows ...storage.RunStatsRow) Report {
	t.Helper()
	agg := New(withTriggerBreakdown(fullCaps))
	agg.SetStages(stages)
	addRows(t, agg, rows...)
	return agg.Report(fixedNow, "/wf", "/db", nil, until)
}

func stageOf(t *testing.T, report Report, rule string) FunnelStage {
	t.Helper()
	for _, funnel := range report.ChainFunnels {
		for _, stage := range funnel.Stages {
			if stage.Rule == rule {
				return stage
			}
		}
	}
	t.Fatalf("stage %q not found in chain_funnels %+v", rule, report.ChainFunnels)
	return FunnelStage{}
}

func firing(t *testing.T, counts any) string {
	t.Helper()
	raw, err := json.Marshal(counts)
	if err != nil {
		t.Fatalf("json.Marshal(%+v): %v", counts, err)
	}
	var byName map[string]int
	if err := json.Unmarshal(raw, &byName); err != nil {
		t.Fatalf("json.Unmarshal(%s): %v", raw, err)
	}
	var names []string
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		if byName[name] > 0 {
			names = append(names, name)
		}
	}
	return strings.Join(names, ",")
}

func assertStageInvariants(t *testing.T, report Report) {
	t.Helper()
	for _, funnel := range report.ChainFunnels {
		for _, s := range funnel.Stages {
			o, a := s.Outcomes, s.Arrivals
			if sum := a.Hop + a.Match + a.Label; sum != s.Entered {
				t.Errorf("stage %q arrivals sum = %d, want entered %d", s.Rule, sum, s.Entered)
			}
			if sum := o.Advanced + o.HandedOff + o.Ceiling + o.HopFailed + o.NoHop + o.InStage; sum != s.Entered {
				t.Errorf("stage %q outcomes sum = %d, want entered %d", s.Rule, sum, s.Entered)
			}
			var reasons int
			for _, r := range s.NoHopReasons {
				reasons += r.Passes
			}
			if reasons != o.NoHop {
				t.Errorf("stage %q no_hop_reasons sum = %d, want no_hop %d", s.Rule, reasons, o.NoHop)
			}
			if (s.Last && o.Advanced != 0) || (!s.Last && o.HandedOff != 0) {
				t.Errorf("stage %q (last=%t) advanced, handed_off = %d, %d, want advanced 0 on the last stage and handed_off 0 elsewhere", s.Rule, s.Last, o.Advanced, o.HandedOff)
			}
			var runs int
			for _, g := range report.ByRule {
				if g.Name == s.Rule {
					runs = g.Runs
				}
			}
			if s.Runs != runs {
				t.Errorf("stage %q runs = %d, want the by_rule runs %d", s.Rule, s.Runs, runs)
			}
		}
	}
}

func TestChainFunnels_PassOutcomes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		rows    []storage.RunStatsRow
		live    []LivePass
		rule    string
		arrival string
		outcome string
		reason  string
	}{
		{name: "latest run decides over a failed first run", rows: []storage.RunStatsRow{passRow("plan", "specify", "", "failed"), passRow("plan", "specify", "advanced", "succeeded")}, rule: "plan", arrival: "hop", outcome: "advanced"},
		{name: "partial counts as advanced", rows: []storage.RunStatsRow{passRow("specify", "", "partial", "succeeded")}, rule: "specify", arrival: "match", outcome: "advanced"},
		{name: "advanced on the last stage is handed off", rows: []storage.RunStatsRow{passRow("implement", "plan", "advanced", "succeeded")}, rule: "implement", arrival: "hop", outcome: "handed_off"},
		{name: "ceiling", rows: []storage.RunStatsRow{passRow("specify", "", "ceiling", "failed")}, rule: "specify", arrival: "match", outcome: "ceiling"},
		{name: "hop failed beats the run status", rows: []storage.RunStatsRow{passRow("specify", "", "failed", "succeeded")}, rule: "specify", arrival: "match", outcome: "hop_failed"},
		{name: "label arrival", rows: []storage.RunStatsRow{passRow("plan", "", "advanced", "succeeded")}, rule: "plan", arrival: "label", outcome: "advanced"},
		{name: "blocked beats handoff on the last stage", rows: []storage.RunStatsRow{blocked(passRow("implement", "plan", "", "succeeded"))}, rule: "implement", arrival: "hop", outcome: "no_hop", reason: "needs_person"},
		{name: "succeeded on the last stage is handed off", rows: []storage.RunStatsRow{passRow("implement", "plan", "", "succeeded")}, rule: "implement", arrival: "hop", outcome: "handed_off"},
		{name: "budget hold after a handoff stays handed off", rows: []storage.RunStatsRow{passRow("implement", "plan", "", "succeeded")}, live: []LivePass{livePass("implement", "plan", true)}, rule: "implement", arrival: "hop", outcome: "handed_off"},
		{name: "blocked beats a budget hold", rows: []storage.RunStatsRow{blocked(passRow("plan", "specify", "", "succeeded"))}, live: []LivePass{livePass("plan", "specify", true)}, rule: "plan", arrival: "hop", outcome: "no_hop", reason: "needs_person"},
		{name: "budget hold beats succeeded on a non-last stage", rows: []storage.RunStatsRow{passRow("plan", "specify", "", "succeeded")}, live: []LivePass{livePass("plan", "specify", true)}, rule: "plan", arrival: "hop", outcome: "no_hop", reason: "budget_held"},
		{name: "budget hold beats a failed run", rows: []storage.RunStatsRow{passRow("plan", "specify", "", "failed")}, live: []LivePass{livePass("plan", "specify", true)}, rule: "plan", arrival: "hop", outcome: "no_hop", reason: "budget_held"},
		{name: "budget hold never overrides a hop decision", rows: []storage.RunStatsRow{passRow("plan", "specify", "advanced", "succeeded")}, live: []LivePass{livePass("plan", "specify", true)}, rule: "plan", arrival: "hop", outcome: "advanced"},
		{name: "succeeded without a hop on a non-last stage", rows: []storage.RunStatsRow{passRow("specify", "", "", "succeeded")}, rule: "specify", arrival: "match", outcome: "no_hop", reason: "operator_stopped"},
		{name: "withheld verdict", rows: []storage.RunStatsRow{withheld(passRow("specify", "", "", "failed"))}, rule: "specify", arrival: "match", outcome: "no_hop", reason: "verdict_withheld"},
		{name: "needs_person status", rows: []storage.RunStatsRow{passRow("specify", "", "", "needs_person")}, rule: "specify", arrival: "match", outcome: "no_hop", reason: "needs_person"},
		{name: "budget_stopped status", rows: []storage.RunStatsRow{passRow("specify", "", "", "budget_stopped")}, rule: "specify", arrival: "match", outcome: "no_hop", reason: "budget_held"},
		{name: "cancelled status", rows: []storage.RunStatsRow{passRow("specify", "", "", "cancelled")}, rule: "specify", arrival: "match", outcome: "no_hop", reason: "operator_stopped"},
		{name: "failed status without a withheld verdict", rows: []storage.RunStatsRow{passRow("specify", "", "", "failed")}, rule: "specify", arrival: "match", outcome: "no_hop", reason: "run_failed"},
		{name: "live pass beats the finished hop decision", rows: []storage.RunStatsRow{passRow("plan", "specify", "advanced", "succeeded")}, live: []LivePass{livePass("plan", "specify", false)}, rule: "plan", arrival: "hop", outcome: "in_stage"},
		{name: "live-only pass takes its arrival from the previous rule", live: []LivePass{livePass("plan", "specify", false)}, rule: "plan", arrival: "hop", outcome: "in_stage"},
		{name: "reaction run without a hop result changes only runs", rows: []storage.RunStatsRow{passRow("plan", "specify", "advanced", "succeeded"), reaction(passRow("plan", "", "", "failed"))}, rule: "plan", arrival: "hop", outcome: "advanced"},
		{name: "reaction run with a hop result decides the pass", rows: []storage.RunStatsRow{passRow("plan", "specify", "", "succeeded"), reaction(passRow("plan", "", "ceiling", "failed"))}, rule: "plan", arrival: "hop", outcome: "ceiling"},
		{name: "run without a chain identifier forms no pass", rows: []storage.RunStatsRow{chainRow("", "plan", "specify", "advanced", "succeeded")}, rule: "plan"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stages := threeStageChain
			stages.Live = tt.live

			report := funnelsOf(t, stages, nil, tt.rows...)

			stage := stageOf(t, report, tt.rule)
			wantEntered := 1
			if tt.arrival == "" {
				wantEntered = 0
			}
			if stage.Entered != wantEntered {
				t.Errorf("chain_funnels stage %q entered = %d, want %d", tt.rule, stage.Entered, wantEntered)
			}
			if got := firing(t, stage.Arrivals); got != tt.arrival {
				t.Errorf("chain_funnels stage %q arrival = %q, want %q", tt.rule, got, tt.arrival)
			}
			if got := firing(t, stage.Outcomes); got != tt.outcome {
				t.Errorf("chain_funnels stage %q outcome = %q, want %q", tt.rule, got, tt.outcome)
			}
			var wantReasons []NoHopReason
			if tt.reason != "" {
				wantReasons = []NoHopReason{{Reason: tt.reason, Passes: 1}}
			}
			if !slices.Equal(stage.NoHopReasons, wantReasons) {
				t.Errorf("chain_funnels stage %q no_hop_reasons = %+v, want %+v", tt.rule, stage.NoHopReasons, wantReasons)
			}
			assertStageInvariants(t, report)
		})
	}
}

func TestChainFunnels_StageFigures(t *testing.T) {
	t.Parallel()

	stages := threeStageChain
	stages.Live = []LivePass{{ChainID: "d", RuleName: "plan", PreviousRule: "specify"}}

	report := funnelsOf(t, stages, nil,
		lasting(chainRow("a", "plan", "specify", "", "succeeded"), 200),
		lasting(chainRow("a", "plan", "specify", "advanced", "succeeded"), 400),
		lasting(chainRow("b", "plan", "specify", "advanced", "succeeded"), 100),
		lasting(chainRow("c", "plan", "specify", "advanced", "succeeded"), 300),
		untimed(chainRow("e", "plan", "specify", "", "cancelled")),
		untimed(chainRow("f", "plan", "specify", "", "cancelled")),
		untimed(chainRow("g", "plan", "specify", "", "failed")),
		untimed(chainRow("h", "plan", "specify", "", "needs_person")),
		untimed(chainRow("i", "plan", "specify", "", "needs_person")),
	)

	stage := stageOf(t, report, "plan")
	if stage.Entered != 9 {
		t.Errorf("chain_funnels stage %q entered = %d, want 9", "plan", stage.Entered)
	}
	if stage.MedianSeconds == nil || *stage.MedianSeconds != 300 {
		t.Errorf("chain_funnels stage %q median_seconds = %v, want 300: a pass sums its runs and a live-only pass adds no sample", "plan", stage.MedianSeconds)
	}
	wantReasons := []NoHopReason{{Reason: "needs_person", Passes: 2}, {Reason: "operator_stopped", Passes: 2}, {Reason: "run_failed", Passes: 1}}
	if !slices.Equal(stage.NoHopReasons, wantReasons) {
		t.Errorf("chain_funnels stage %q no_hop_reasons = %+v, want %+v", "plan", stage.NoHopReasons, wantReasons)
	}
	assertStageInvariants(t, report)
}

func TestChainFunnels_LivePassesNeedARangeReachingNow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		until       *time.Time
		wantEntered int
	}{
		{name: "no upper bound", wantEntered: 1},
		{name: "bound after the report time", until: new(fixedNow.Add(time.Hour)), wantEntered: 1},
		{name: "bound at the report time", until: new(fixedNow)},
		{name: "bound before the report time", until: new(fixedNow.Add(-time.Hour))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stages := threeStageChain
			stages.Live = []LivePass{livePass("plan", "specify", false)}

			report := funnelsOf(t, stages, tt.until)

			if got := stageOf(t, report, "plan").Entered; got != tt.wantEntered {
				t.Errorf("Report(until=%v) chain_funnels stage %q entered = %d, want %d", tt.until, "plan", got, tt.wantEntered)
			}
		})
	}
}

func TestChainFunnels_DefinitionsMergeSharedStages(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		chains [][]string
		want   []string
	}{
		{name: "one chain", chains: [][]string{{"a", "b", "c"}}, want: []string{"a^ b c$"}},
		{name: "two entries into one shared stage", chains: [][]string{{"a", "c"}, {"b", "c"}}, want: []string{"a^ b^ c$"}},
		{name: "branch joining mid-chain", chains: [][]string{{"a", "b", "c", "d"}, {"x", "y", "c", "d"}}, want: []string{"a^ b x^ y c d$"}},
		{name: "disjoint chains keep configuration order", chains: [][]string{{"a", "b"}, {"c", "d"}}, want: []string{"a^ b$", "c^ d$"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			report := funnelsOf(t, Stages{Chains: tt.chains}, nil)

			var got []string
			for _, funnel := range report.ChainFunnels {
				var shape []string
				for _, s := range funnel.Stages {
					rule := s.Rule
					if s.Entry {
						rule += "^"
					}
					if s.Last {
						rule += "$"
					}
					shape = append(shape, rule)
				}
				got = append(got, strings.Join(shape, " "))
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("Report(chains=%v).chain_funnels stages = %q, want %q", tt.chains, got, tt.want)
			}
		})
	}
}
