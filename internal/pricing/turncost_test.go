package pricing

import (
	"math"
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

func measuredTurn(parts map[string]domain.TokenUsage) TurnSpend {
	return TurnSpend{Measured: true, Parts: parts}
}

func spendOfTurns(kind string, turns []TurnSpend, reported []domain.ReportedAmount) Spend {
	spend := Spend{Kind: kind, Measured: true, Parts: map[string]domain.TokenUsage{}, Reported: reported}
	for _, turn := range turns {
		for model, usage := range turn.Parts {
			part := spend.Parts[model]
			part.InputTokens += usage.InputTokens
			part.OutputTokens += usage.OutputTokens
			part.CacheReadTokens += usage.CacheReadTokens
			part.CacheWriteTokens += usage.CacheWriteTokens
			spend.Parts[model] = part
			spend.Totals.InputTokens += usage.InputTokens
			spend.Totals.OutputTokens += usage.OutputTokens
			spend.Totals.CacheReadTokens += usage.CacheReadTokens
			spend.Totals.CacheWriteTokens += usage.CacheWriteTokens
		}
	}
	return spend
}

func TestPriceTurns(t *testing.T) {
	t.Parallel()

	rated := mustLoadRates(t, map[string]any{
		"price_table": noTables(),
		"token_rates": map[string]any{"m1": fullPerMtok(2, 10, 0.5, 4), "m2": fullPerMtok(3, 15, 0.3, 6)},
	})
	type usages = map[string]domain.TokenUsage

	tests := []struct {
		name     string
		kind     string
		rates    *Rates
		turns    []TurnSpend
		reported []domain.ReportedAmount
		want     []float64
	}{
		{
			name:  "rated run on two models costs each turn its own tokens at each model's rates",
			kind:  "m",
			rates: rated,
			turns: []TurnSpend{
				measuredTurn(usages{"m1": {InputTokens: 1_000_000, OutputTokens: 1_000_000}}),
				measuredTurn(usages{
					"m1": {OutputTokens: 1_000_000},
					"m2": {InputTokens: 3_000_000, CacheReadTokens: 1_000_000, OutputTokens: 1_000_000},
				}),
			},
			want: []float64{12, 31.3},
		},
		{
			name:  "kind entry scales turns to the run's cost where the fresh-input floor is active",
			kind:  "k",
			rates: kindRates(t),
			turns: []TurnSpend{
				measuredTurn(usages{"m1": {InputTokens: 1_000_000, CacheReadTokens: 2_000_000}}),
				measuredTurn(usages{"m2": {InputTokens: 3_000_000, OutputTokens: 1_000_000}}),
			},
			want: []float64{15.0 / 17, 240.0 / 17},
		},
		{
			name:  "agent-priced part splits its amount by token count beside a rated part",
			kind:  "m",
			rates: rated,
			turns: []TurnSpend{
				measuredTurn(usages{"m1": {InputTokens: 1_000_000, OutputTokens: 1_000_000}, "ag": {InputTokens: 1_000_000}}),
				measuredTurn(usages{"ag": {OutputTokens: 3_000_000}}),
			},
			reported: []domain.ReportedAmount{usd("ag", 9, false)},
			want:     []float64{14.25, 6.75},
		},
		{
			name:  "turn that spent only on an unpriced part costs nothing beside a rated part",
			kind:  "m",
			rates: rated,
			turns: []TurnSpend{
				measuredTurn(usages{"m1": {InputTokens: 1_000_000, OutputTokens: 1_000_000}, "x": {InputTokens: 1_000_000}}),
				measuredTurn(usages{"x": {OutputTokens: 1_000_000}}),
			},
			want: []float64{12, 0},
		},
		{
			name: "agent-reported amount is split under nil rates",
			kind: "m",
			turns: []TurnSpend{
				measuredTurn(usages{"ag": {InputTokens: 1_000_000}}),
				measuredTurn(usages{"ag": {OutputTokens: 3_000_000}}),
			},
			reported: []domain.ReportedAmount{usd("ag", 6, false)},
			want:     []float64{1.5, 4.5},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			turns := append(slices.Clip(tt.turns), TurnSpend{})
			spend := spendOfTurns(tt.kind, tt.turns, tt.reported)
			price := PriceRun(spend, tt.rates)
			if price.CostUSD == nil {
				t.Fatalf("PriceRun(%s) cost = nil, want a cost", asJSON(t, spend))
			}

			got := PriceTurns(spend, price, turns, tt.rates)

			if len(got) != len(turns) {
				t.Fatalf("PriceTurns(%s) returned %d turns, want %d", asJSON(t, spend), len(got), len(turns))
			}
			sum := 0.0
			for i, turn := range got[:len(tt.want)] {
				if turn.CostUSD == nil {
					t.Fatalf("PriceTurns(%s)[%d].CostUSD = nil, want %v", asJSON(t, spend), i, tt.want[i])
				}
				classes := turn.Classes
				if min(classes.Input, classes.Output, classes.CacheRead, classes.CacheWrite) < 0 {
					t.Errorf("PriceTurns(%s)[%d].Classes = %s, want no negative class cost", asJSON(t, spend), i, asJSON(t, classes))
				}
				if classSum := classes.Input + classes.Output + classes.CacheRead + classes.CacheWrite; math.Abs(classSum-*turn.CostUSD) > 1e-6 {
					t.Errorf("PriceTurns(%s)[%d] class costs sum to %v, want CostUSD %v", asJSON(t, spend), i, classSum, *turn.CostUSD)
				}
				if math.Abs(*turn.CostUSD-tt.want[i]) > 1e-6 {
					t.Errorf("PriceTurns(%s)[%d].CostUSD = %v, want %v", asJSON(t, spend), i, *turn.CostUSD, tt.want[i])
				}
				sum += *turn.CostUSD
			}
			if math.Abs(sum-*price.CostUSD) > 1e-6 {
				t.Errorf("PriceTurns(%s) costs sum to %v, want PriceRun cost %v", asJSON(t, spend), sum, *price.CostUSD)
			}
			if unmeasured := got[len(got)-1]; unmeasured.CostUSD != nil || unmeasured.Classes != (ClassCosts{}) {
				t.Errorf("PriceTurns(%s) unmeasured turn = %s, want nil cost and zero classes", asJSON(t, spend), asJSON(t, unmeasured))
			}
		})
	}
}
