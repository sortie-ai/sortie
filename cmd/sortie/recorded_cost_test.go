package main

import (
	"encoding/json"
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/pricing"
	"github.com/sortie-ai/sortie/internal/storage"
)

func TestRecordedCostOf(t *testing.T) {
	t.Parallel()

	kindEntry := pricing.TokenRateConfig{
		InputPerMtok: new(2.0), OutputPerMtok: new(10.0), CacheReadPerMtok: new(0.5), CacheWritePerMtok: new(4.0),
	}
	cfg, err := config.NewServiceConfig(map[string]any{
		"token_rates": map[string]any{"k": map[string]any{
			"input_per_mtok": 2.0, "output_per_mtok": 10.0, "cache_read_per_mtok": 0.5, "cache_write_per_mtok": 4.0,
		}},
		"price_table": map[string]any{"embedded": false},
	})
	if err != nil {
		t.Fatalf("config.NewServiceConfig: %v", err)
	}
	rates, _, err := pricing.LoadRates(cfg, pricing.LoadInput{IsKind: func(key string) bool { return key == "k" }})
	if err != nil {
		t.Fatalf("pricing.LoadRates: %v", err)
	}
	estimated := func(usd float64) storage.ModelCost {
		return storage.ModelCost{EstimatedCostUSD: &usd, CostStatus: "estimated", PriceSource: "operator", PriceTableVersion: pricing.TokenRates{"k": kindEntry}.Version()}
	}
	spent := storage.ModelTokens{InputTokens: 1_000_000, OutputTokens: 1_000_000}
	spentRun := func(kind, reported string) storage.UnrecordedRun {
		return storage.UnrecordedRun{
			AgentAdapter: kind, TokensMeasured: true, InputTokens: 1_000_000, OutputTokens: 1_000_000, TotalTokens: 2_000_000,
			ConfiguredModel: "m0", ReportedModel: reported,
		}
	}
	measured := storage.UnrecordedRun{AgentAdapter: "k", TokensMeasured: true}
	totalOnly := measured
	totalOnly.TotalTokens = 5
	withParts := storage.UnrecordedRun{
		AgentAdapter: "k", TokensMeasured: true,
		InputTokens: 4_000_000, OutputTokens: 1_000_000, TotalTokens: 5_000_000, CacheReadTokens: 2_000_000,
		Parts: map[string]storage.ModelTokens{
			"a": {InputTokens: 1_000_000, CacheReadTokens: 2_000_000},
			"b": {InputTokens: 3_000_000, OutputTokens: 1_000_000},
		},
	}

	tests := []struct {
		name string
		run  storage.UnrecordedRun
		want storage.RunCostRecord
	}{
		{"part named for the reported model", spentRun("k", "m1"), storage.RunCostRecord{
			EstimatedCostUSD: new(12.0), CostStatus: "estimated",
			NewParts:  map[string]storage.ModelTokens{"m1": spent},
			PartCosts: map[string]storage.ModelCost{"m1": estimated(12)},
		}},
		{"part named for the configured model without a reported one", spentRun("k", ""), storage.RunCostRecord{
			EstimatedCostUSD: new(12.0), CostStatus: "estimated",
			NewParts:  map[string]storage.ModelTokens{"m0": spent},
			PartCosts: map[string]storage.ModelCost{"m0": estimated(12)},
		}},
		{"kind without an entry records unpriced run and part", spentRun("u", "m1"), storage.RunCostRecord{
			CostStatus: "unpriced",
			NewParts:   map[string]storage.ModelTokens{"m1": spent},
			PartCosts:  map[string]storage.ModelCost{"m1": {CostStatus: "unpriced", UnratedClasses: []string{"input", "output"}}},
		}},
		{"spent nothing gets no part", measured, storage.RunCostRecord{EstimatedCostUSD: new(0.0), CostStatus: "estimated"}},
		{"only total_tokens gets no part", totalOnly, storage.RunCostRecord{CostStatus: "unpriced"}},
		{"parts price the run on its summed tokens and insert nothing", withParts, storage.RunCostRecord{
			EstimatedCostUSD: new(15.0), CostStatus: "estimated",
			PartCosts: map[string]storage.ModelCost{"a": estimated(1), "b": estimated(16)},
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, want := recordJSON(recordedCostOf(tt.run, rates)), recordJSON(tt.want)

			if got != want {
				t.Errorf("recordedCostOf(%+v) = %s, want %s", tt.run, got, want)
			}
		})
	}
}

func recordJSON(record storage.RunCostRecord) string {
	b, _ := json.Marshal(record)
	return string(b)
}
