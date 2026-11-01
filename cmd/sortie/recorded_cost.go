package main

import (
	"cmp"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/pricing"
	"github.com/sortie-ai/sortie/internal/storage"
)

// recordedCostOf prices one run written before costs were recorded under
// rates. It does no I/O, which [storage.RunHistoryStore.RecordUnrecordedRunCosts]
// requires of its pricer.
//
// A measured run with no per-model rows gets one new part, so its cost has a
// part to sit on like every run written since.
func recordedCostOf(run storage.UnrecordedRun, rates *pricing.Rates) storage.RunCostRecord {
	parts := run.Parts
	var newParts map[string]storage.ModelTokens
	if parts == nil && run.TokensMeasured && spentInClass(run) {
		newParts = map[string]storage.ModelTokens{
			cmp.Or(run.ReportedModel, run.ConfiguredModel): {
				InputTokens:      run.InputTokens,
				OutputTokens:     run.OutputTokens,
				CacheReadTokens:  run.CacheReadTokens,
				CacheWriteTokens: run.CacheWriteTokens,
			},
		}
		parts = newParts
	}

	price := pricing.PriceRun(pricing.Spend{
		Kind:     run.AgentAdapter,
		Measured: run.TokensMeasured,
		Totals: domain.TokenUsage{
			InputTokens:      run.InputTokens,
			OutputTokens:     run.OutputTokens,
			TotalTokens:      run.TotalTokens,
			CacheReadTokens:  run.CacheReadTokens,
			CacheWriteTokens: run.CacheWriteTokens,
		},
		Parts:            partUsages(parts),
		UnaccountedTurns: run.UnaccountedTurns,
	}, rates)

	return storage.RunCostRecord{
		EstimatedCostUSD: price.CostUSD,
		CostStatus:       string(price.Status),
		NewParts:         newParts,
		PartCosts:        modelCosts(price.Parts),
	}
}

// spentInClass is false for a run that reported only a bare total, which no
// rate can price.
func spentInClass(run storage.UnrecordedRun) bool {
	return run.InputTokens != 0 || run.OutputTokens != 0 ||
		run.CacheReadTokens != 0 || run.CacheWriteTokens != 0
}

func partUsages(parts map[string]storage.ModelTokens) map[string]domain.TokenUsage {
	if parts == nil {
		return nil
	}
	usages := make(map[string]domain.TokenUsage, len(parts))
	for model, tokens := range parts {
		usages[model] = domain.TokenUsage{
			InputTokens:      tokens.InputTokens,
			OutputTokens:     tokens.OutputTokens,
			TotalTokens:      tokens.InputTokens + tokens.OutputTokens,
			CacheReadTokens:  tokens.CacheReadTokens,
			CacheWriteTokens: tokens.CacheWriteTokens,
		}
	}
	return usages
}

func modelCosts(prices map[string]pricing.PartPrice) map[string]storage.ModelCost {
	if prices == nil {
		return nil
	}
	costs := make(map[string]storage.ModelCost, len(prices))
	for model, price := range prices {
		costs[model] = storage.ModelCost{
			EstimatedCostUSD:  price.CostUSD,
			CostStatus:        string(price.Status),
			PriceSource:       string(price.Source),
			PriceTableVersion: price.TableVersion,
			UnratedClasses:    price.UnratedNames(),
		}
	}
	return costs
}
