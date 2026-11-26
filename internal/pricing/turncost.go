package pricing

import (
	"cmp"
	"maps"
	"slices"

	"github.com/sortie-ai/sortie/internal/domain"
)

// TurnSpend is one turn's tokens per model.
type TurnSpend struct {
	// Measured is false when the turn's usage was never reported.
	Measured bool

	Parts map[string]domain.TokenUsage
}

// TurnPrice is one turn's share of a run's cost.
type TurnPrice struct {
	// CostUSD is nil when the turn has no cost figure.
	CostUSD *float64

	// Classes splits CostUSD by pricing class and is zero when CostUSD is nil.
	Classes ClassCosts
}

// ClassCosts splits a cost by pricing class. Input is fresh input, which
// excludes cache reads and writes.
type ClassCosts struct {
	Input, Output, CacheRead, CacheWrite float64
}

// PriceTurns divides a run's cost among its turns and returns one [TurnPrice]
// per element of turns, in order. price is [PriceRun] of s under r.
//
// Each priced part's cost is spread in full over the turns that spent on its
// model, in proportion to each class's tokens at the rate that priced it, and
// the total is then scaled to the run's cost, so the turns' costs sum to it up
// to float64 rounding. A part no class of which has a rate is spread by token
// count. Unmeasured turns, and every turn of a run without a cost, have a nil
// cost. PriceTurns is pure and never mutates its inputs.
func PriceTurns(s Spend, price Price, turns []TurnSpend, r *Rates) []TurnPrice {
	out := make([]TurnPrice, len(turns))
	if price.CostUSD == nil {
		return out
	}

	shares := make([][rateClasses]float64, len(turns))
	allocated := 0.0
	for _, model := range slices.Sorted(maps.Keys(price.Parts)) {
		part := price.Parts[model]
		if part.CostUSD == nil {
			continue
		}
		weights, total := turnWeights(turns, model, classRates(s.Kind, model, part, r))
		if total == 0 {
			weights, total = turnWeights(turns, model, [rateClasses]float64{1, 1, 1, 1})
		}
		if total == 0 {
			continue
		}
		allocated += *part.CostUSD
		for t := range weights {
			for class := range rateClasses {
				shares[t][class] += *part.CostUSD * weights[t][class] / total
			}
		}
	}

	factor := 1.0
	if allocated > 0 {
		factor = *price.CostUSD / allocated
	}
	for t, turn := range turns {
		if !turn.Measured {
			continue
		}
		classes := ClassCosts{
			Input:      shares[t][rateInput] * factor,
			Output:     shares[t][rateOutput] * factor,
			CacheRead:  shares[t][rateCacheRead] * factor,
			CacheWrite: shares[t][rateCacheWrite] * factor,
		}
		cost := classes.Input + classes.Output + classes.CacheRead + classes.CacheWrite
		out[t] = TurnPrice{CostUSD: &cost, Classes: classes}
	}
	return out
}

// turnWeights weighs each class of each measured turn's tokens on model by
// its rate and returns the weights with their sum.
func turnWeights(turns []TurnSpend, model string, rates [rateClasses]float64) ([][rateClasses]float64, float64) {
	weights := make([][rateClasses]float64, len(turns))
	var total float64
	for t, turn := range turns {
		if !turn.Measured {
			continue
		}
		tokens := classTokens(turn.Parts[model])
		for class := range rateClasses {
			weights[t][class] = float64(tokens[class]) * rates[class]
			total += weights[t][class]
		}
	}
	return weights, total
}

// classTokens gives the tokens of each pricing class, with fresh input
// floored at zero as [pricePart] floors it.
func classTokens(u domain.TokenUsage) [rateClasses]int64 {
	return [rateClasses]int64{
		rateInput:      max(u.InputTokens-u.CacheReadTokens-u.CacheWriteTokens, 0),
		rateOutput:     u.OutputTokens,
		rateCacheRead:  u.CacheReadTokens,
		rateCacheWrite: u.CacheWriteTokens,
	}
}

// classRates gives the per-token rate of each class that priced part for
// model, zero for a class without one. A part the rates did not price,
// including one priced from an agent-reported amount, has no rate in any
// class.
func classRates(kind, model string, part PartPrice, r *Rates) [rateClasses]float64 {
	var rated [rateClasses]*float64
	if entry, complete := kindEntry(r, kind); complete {
		rated = [rateClasses]*float64{
			rateInput:      perToken(entry.InputPerMtok),
			rateOutput:     perToken(entry.OutputPerMtok),
			rateCacheRead:  perToken(cmp.Or(entry.CacheReadPerMtok, entry.InputPerMtok)),
			rateCacheWrite: perToken(cmp.Or(entry.CacheWritePerMtok, entry.InputPerMtok)),
		}
	} else if res := r.resolve(model); res != nil && pricedByRates(part.Source) {
		rated = res.perToken
	}

	var rates [rateClasses]float64
	for class, rate := range rated {
		if rate != nil {
			rates[class] = *rate
		}
	}
	return rates
}

func kindEntry(r *Rates, kind string) (TokenRateConfig, bool) {
	if r == nil || !r.kinds[kind] {
		return TokenRateConfig{}, false
	}
	return completeEntry(r, kind)
}

func pricedByRates(source PriceSource) bool {
	return source == SourceOperator || source == SourceFetched || source == SourceEmbedded
}
