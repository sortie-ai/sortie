package pricing

import (
	"cmp"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

func kindRates(t *testing.T) *Rates {
	t.Helper()

	return mustLoadRates(t, map[string]any{
		"price_table": map[string]any{"embedded": false},
		"token_rates": map[string]any{"k": map[string]any{
			"input_per_mtok": 2.0, "output_per_mtok": 10.0, "cache_read_per_mtok": 0.5, "cache_write_per_mtok": 4.0,
		}},
	}, "k")
}

func fixtureRates(inputPerMtok float64) TokenRates {
	return TokenRates{"k": {
		InputPerMtok:      new(inputPerMtok),
		OutputPerMtok:     new(10.0),
		CacheReadPerMtok:  new(0.5),
		CacheWritePerMtok: new(4.0),
	}}
}

func asJSON(t *testing.T, v any) string {
	t.Helper()
	encoded, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal(%T): %v", v, err)
	}
	return string(encoded)
}

var (
	usageS     = domain.TokenUsage{InputTokens: 1_000_000, OutputTokens: 1_000_000, TotalTokens: 2_000_000}
	usagePartA = domain.TokenUsage{InputTokens: 1_000_000, CacheReadTokens: 2_000_000}
	usagePartB = domain.TokenUsage{InputTokens: 3_000_000, OutputTokens: 1_000_000}
	usageM     = domain.TokenUsage{InputTokens: 4_000_000, OutputTokens: 1_000_000, CacheReadTokens: 2_000_000}
	onlyTotal  = domain.TokenUsage{TotalTokens: 500}
	usageIn    = domain.TokenUsage{InputTokens: 1_000_000, TotalTokens: 1_000_000}
	usageRead  = domain.TokenUsage{InputTokens: 2_000_000, CacheReadTokens: 1_000_000, OutputTokens: 1_000_000, TotalTokens: 3_000_000}
	usageWrite = domain.TokenUsage{InputTokens: 2_000_000, CacheWriteTokens: 1_000_000, OutputTokens: 1_000_000, TotalTokens: 3_000_000}
)

func operatorOf(cost float64) PartPrice {
	return PartPrice{CostUSD: new(cost), Status: CostEstimated, Source: SourceOperator, TableVersion: fixtureRates(2).Version()}
}

func versionOf(t *testing.T, tokenRates map[string]any) string {
	t.Helper()

	rates, _ := ParseTokenRates(tokenRates, true)
	return rates.Version()
}

func pricedOnce(model string, cost float64, status CostStatus, source PriceSource, version string) Price {
	part := PartPrice{CostUSD: new(cost), Status: status, Source: source, TableVersion: version}
	return Price{CostUSD: new(cost), Status: status, Parts: map[string]PartPrice{model: part}}
}

func unpricedOnce(model string) Price {
	return Price{Status: CostUnpriced, Parts: map[string]PartPrice{model: {Status: CostUnpriced}}}
}

func withUnrated(p Price, classes []TokenClass) Price {
	for model, part := range p.Parts {
		part.Unrated = classes
		p.Parts[model] = part
	}
	return p
}

func spendOnce(model string, usage domain.TokenUsage) Spend {
	return Spend{Kind: "m", Measured: true, Totals: usage, Parts: map[string]domain.TokenUsage{model: usage}}
}

func usd(model string, amount float64, lowerBound bool) domain.ReportedAmount {
	return domain.ReportedAmount{Unit: domain.UnitUSD, Model: model, Amount: amount, LowerBound: lowerBound}
}

func spendOfParts(parts map[string]domain.TokenUsage, reported ...domain.ReportedAmount) Spend {
	spend := Spend{Kind: "m", Measured: true, Parts: parts, Reported: reported}
	for _, usage := range parts {
		spend.Totals.InputTokens += usage.InputTokens
		spend.Totals.OutputTokens += usage.OutputTokens
		spend.Totals.TotalTokens += usage.TotalTokens
	}
	return spend
}

func TestPriceRun(t *testing.T) {
	t.Parallel()

	const listed = "listed-model"
	tableVersion := mustParse(t, listedTable, nil).Version()
	embedded, err := EmbeddedTable()
	if err != nil {
		t.Fatalf("EmbeddedTable() = %v, want nil", err)
	}
	sonnet, ok := embedded.Lookup("claude-sonnet-4-5")
	if !ok {
		t.Fatal("EmbeddedTable().Lookup(claude-sonnet-4-5) found no entry")
	}
	sonnetCost := 1_000_000**sonnet.InputPerToken + 1_000_000**sonnet.OutputPerToken

	overridden := map[string]any{listed: map[string]any{"input_per_mtok": 3.0}}
	overrideRates := mustLoadRates(t, map[string]any{"price_table": operatorTable(), "token_rates": overridden})
	overrideVersion := versionOf(t, overridden)

	unlistedEntries := map[string]any{
		"local":         fullPerMtok(3, 15, 0.3, 6),
		"local-nocw":    map[string]any{"input_per_mtok": 3.0, "output_per_mtok": 15.0, "cache_read_per_mtok": 0.3},
		"local-nocache": perMtok(2, 10),
		"My-Local-2.5":  map[string]any{"input_per_mtok": 3.0},
		"kc":            perMtok(2, 10),
	}
	unlistedRates := mustLoadRates(t, map[string]any{"price_table": operatorTable(), "token_rates": unlistedEntries}, "kc")
	unlistedVersion := versionOf(t, unlistedEntries)

	twinEntries := map[string]any{"LISTED-MODEL": map[string]any{"input_per_mtok": 3.0}, "Listed-Model": map[string]any{"input_per_mtok": 4.0}}
	incompleteKind := map[string]any{"k": map[string]any{"input_per_mtok": 2.0}}

	tests := []struct {
		name    string
		rates   *Rates
		spend   Spend
		want    Price
		unrated []TokenClass
	}{
		{
			name:  "unmeasured run",
			spend: Spend{Kind: "k", Totals: usageS},
			want:  Price{Status: CostUnmeasured},
		},
		{
			name:  "measured run that spent nothing",
			spend: Spend{Kind: "u", Measured: true},
			want:  Price{CostUSD: new(0.0), Status: CostEstimated},
		},
		{
			name:  "run that spent nothing with an unaccounted turn",
			spend: Spend{Kind: "u", Measured: true, UnaccountedTurns: 1},
			want:  Price{CostUSD: new(0.0), Status: CostPartial},
		},
		{
			name:  "run that spent only total_tokens",
			spend: Spend{Kind: "k", Measured: true, Totals: onlyTotal},
			want:  Price{Status: CostUnpriced},
		},
		{
			name:    "kind without an entry",
			spend:   Spend{Kind: "u", Measured: true, Totals: usageS, Parts: map[string]domain.TokenUsage{"m": usageS}},
			want:    Price{Status: CostUnpriced, Parts: map[string]PartPrice{"m": {Status: CostUnpriced}}},
			unrated: []TokenClass{ClassInput, ClassOutput},
		},
		{
			name:  "S",
			spend: Spend{Kind: "k", Measured: true, Totals: usageS, Parts: map[string]domain.TokenUsage{"m": usageS}},
			want:  Price{CostUSD: new(12.0), Status: CostEstimated, Parts: map[string]PartPrice{"m": operatorOf(12)}},
		},
		{
			name:  "S with one unaccounted turn",
			spend: Spend{Kind: "k", Measured: true, Totals: usageS, UnaccountedTurns: 1},
			want:  Price{CostUSD: new(12.0), Status: CostPartial},
		},
		{
			name: "M prices on summed tokens, not the sum of its parts",
			spend: Spend{Kind: "k", Measured: true, Totals: usageM,
				Parts: map[string]domain.TokenUsage{"a": usagePartA, "b": usagePartB}},
			want: Price{CostUSD: new(15.0), Status: CostEstimated,
				Parts: map[string]PartPrice{"a": operatorOf(1), "b": operatorOf(16)}},
		},
		{
			name:  "default install prices a model the embedded table lists",
			rates: mustLoadRates(t, map[string]any{}),
			spend: spendOnce("claude-sonnet-4-5", usageS),
			want:  pricedOnce("claude-sonnet-4-5", sonnetCost, CostEstimated, SourceEmbedded, embedded.Version()),
		},
		{
			name:  "model entry overrides one rate and takes the rest from the table row",
			rates: overrideRates,
			spend: spendOnce(listed, usageRead),
			want:  pricedOnce(listed, 8.5, CostEstimated, SourceOperator, overrideVersion+"+"+tableVersion),
		},
		{
			name:    "model entry never matches a longer model name by prefix",
			rates:   overrideRates,
			spend:   spendOnce(listed+"-20250101", usageIn),
			want:    unpricedOnce(listed + "-20250101"),
			unrated: []TokenClass{ClassInput},
		},
		{
			name:  "model entry for an unlisted model with every rate set",
			rates: unlistedRates,
			spend: spendOnce("local", usageWrite),
			want:  pricedOnce("local", 24, CostEstimated, SourceOperator, unlistedVersion),
		},
		{
			name:    "unset cache write rate leaves a cache-writing run partial and unpriced for those tokens",
			rates:   unlistedRates,
			spend:   spendOnce("local-nocw", usageWrite),
			want:    pricedOnce("local-nocw", 18, CostPartial, SourceOperator, unlistedVersion),
			unrated: []TokenClass{ClassCacheWrite},
		},
		{
			name:    "unset cache read rate is not priced at the input rate on a model entry",
			rates:   unlistedRates,
			spend:   spendOnce("local-nocache", usageRead),
			want:    pricedOnce("local-nocache", 12, CostPartial, SourceOperator, unlistedVersion),
			unrated: []TokenClass{ClassCacheRead},
		},
		{
			name:  "unset cache read rate is priced at the input rate on a kind entry",
			rates: unlistedRates,
			spend: Spend{Kind: "kc", Measured: true, Totals: usageRead},
			want:  Price{CostUSD: new(14.0), Status: CostEstimated},
		},
		{
			name:  "unlisted model entry matches ignoring case and a dot between digits",
			rates: unlistedRates,
			spend: spendOnce("my-local-2-5", usageIn),
			want:  pricedOnce("my-local-2-5", 3, CostEstimated, SourceOperator, unlistedVersion),
		},
		{
			name:  "model entry keyed by another spelling of a listed model applies",
			rates: mustLoadRates(t, map[string]any{"price_table": operatorTable(), "token_rates": map[string]any{"LISTED-MODEL": map[string]any{"input_per_mtok": 3.0}}}),
			spend: spendOnce(listed, usageIn),
			want:  pricedOnce(listed, 3, CostEstimated, SourceOperator, versionOf(t, map[string]any{"LISTED-MODEL": map[string]any{"input_per_mtok": 3.0}})+"+"+tableVersion),
		},
		{
			name:  "two model entries for one listed model apply to neither",
			rates: mustLoadRates(t, map[string]any{"price_table": operatorTable(), "token_rates": twinEntries}),
			spend: spendOnce(listed, usageIn),
			want:  pricedOnce(listed, 1, CostEstimated, SourceOperator, tableVersion),
		},
		{
			name:  "operator table prices a model it lists",
			rates: mustLoadRates(t, map[string]any{"price_table": operatorTable()}),
			spend: spendOnce(listed, usageIn),
			want:  pricedOnce(listed, 1, CostEstimated, SourceOperator, tableVersion),
		},
		{
			name:    "operator table replaces the embedded table",
			rates:   mustLoadRates(t, map[string]any{"price_table": operatorTable()}),
			spend:   spendOnce("claude-sonnet-4-5", usageIn),
			want:    unpricedOnce("claude-sonnet-4-5"),
			unrated: []TokenClass{ClassInput},
		},
		{
			name:    "no rate source prices nothing",
			rates:   mustLoadRates(t, map[string]any{"price_table": noTables()}),
			spend:   spendOnce(listed, usageIn),
			want:    unpricedOnce(listed),
			unrated: []TokenClass{ClassInput},
		},
		{
			name:    "incomplete kind entry leaves the run unpriced although a table lists the model",
			rates:   mustLoadRates(t, map[string]any{"price_table": operatorTable(), "token_rates": incompleteKind}, "k"),
			spend:   Spend{Kind: "k", Measured: true, Totals: usageIn, Parts: map[string]domain.TokenUsage{listed: usageIn}},
			want:    unpricedOnce(listed),
			unrated: []TokenClass{ClassOutput},
		},
	}

	kinds := kindRates(t)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := PriceRun(tt.spend, cmp.Or(tt.rates, kinds))

			want := withUnrated(tt.want, tt.unrated)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("PriceRun(%s) = %s, want %s", asJSON(t, tt.spend), asJSON(t, got), asJSON(t, want))
			}
		})
	}
}

func TestPriceSession(t *testing.T) {
	t.Parallel()

	const listed = "listed-model"
	tableRates := mustLoadRates(t, map[string]any{"price_table": operatorTable()})
	incompleteKind := mustLoadRates(t, map[string]any{"token_rates": map[string]any{"k": map[string]any{"input_per_mtok": 2.0}}}, "k")

	tests := []struct {
		name       string
		spend      Spend
		wantCost   *float64
		wantStatus CostStatus
		rates      *Rates
	}{
		{"unmeasured", Spend{Kind: "k", Totals: usageS}, nil, CostUnmeasured, nil},
		{"kind without an entry that spent nothing", Spend{Kind: "u", Measured: true}, nil, CostUnpriced, nil},
		{"kind with an entry that spent nothing", Spend{Kind: "k", Measured: true}, new(0.0), CostEstimated, nil},
		{"only total_tokens", Spend{Kind: "k", Measured: true, Totals: onlyTotal}, nil, CostUnpriced, nil},
		{"S", Spend{Kind: "k", Measured: true, Totals: usageS}, new(12.0), CostEstimated, nil},
		{
			name:       "incomplete kind entry leaves a session that spent nothing unpriced",
			spend:      Spend{Kind: "k", Measured: true},
			wantStatus: CostUnpriced,
			rates:      incompleteKind,
		},
		{
			name:       "session that spent nothing on a model a table lists",
			spend:      Spend{Kind: "m", Model: listed, Measured: true},
			wantCost:   new(0.0),
			wantStatus: CostEstimated,
			rates:      tableRates,
		},
		{
			name:       "session that spent nothing on a model no rate prices",
			spend:      Spend{Kind: "m", Model: "unlisted", Measured: true},
			wantStatus: CostUnpriced,
			rates:      tableRates,
		},
	}

	kinds := kindRates(t)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotCost, gotStatus := PriceSession(tt.spend, cmp.Or(tt.rates, kinds))

			if !reflect.DeepEqual(gotCost, tt.wantCost) || gotStatus != tt.wantStatus {
				t.Errorf("PriceSession(%s) = %s, %q, want %s, %q",
					asJSON(t, tt.spend), asJSON(t, gotCost), gotStatus, asJSON(t, tt.wantCost), tt.wantStatus)
			}
		})
	}
}

func TestPricingWithoutRates(t *testing.T) {
	t.Parallel()

	spend := spendOnce("listed-model", usageIn)
	var rates *Rates

	run := PriceRun(spend, rates)
	cost, status := PriceSession(spend, rates)

	if !reflect.DeepEqual(run, withUnrated(unpricedOnce("listed-model"), []TokenClass{ClassInput})) || cost != nil || status != CostUnpriced {
		t.Errorf("PriceRun, PriceSession with nil rates = %s, %s, %q, want unpriced and no cost", asJSON(t, run), asJSON(t, cost), status)
	}
	if rates.SourceAvailable() || rates.SourceConfigured() || rates.Entries() != nil {
		t.Errorf("nil rates SourceAvailable, SourceConfigured, Entries = %t, %t, %v, want false, false, nil",
			rates.SourceAvailable(), rates.SourceConfigured(), rates.Entries())
	}
}

func TestPriceReportedAmounts(t *testing.T) {
	t.Parallel()

	tableEntries := map[string]any{"half-priced": map[string]any{"input_per_mtok": 3.0}}
	rates := mustLoadRates(t, map[string]any{"price_table": noTables(), "token_rates": tableEntries})
	halfPriced := PartPrice{CostUSD: new(3.0), Status: CostPartial, Source: SourceOperator, TableVersion: versionOf(t, tableEntries), Unrated: []TokenClass{ClassOutput}}
	unpriced := PartPrice{Status: CostUnpriced, Unrated: []TokenClass{ClassInput}}

	incompleteKind := mustLoadRates(t, map[string]any{"token_rates": map[string]any{"k": map[string]any{"input_per_mtok": 2.0}}}, "k")

	tests := []struct {
		name  string
		spend Spend
		rates *Rates
		want  Price
	}{
		{
			name:  "attributed amount prices an unpriced part",
			spend: spendOfParts(map[string]domain.TokenUsage{"m1": usageIn}, usd("m1", 4.5, false)),
			want:  pricedOnce("m1", 4.5, CostEstimated, SourceAgent, ""),
		},
		{
			name:  "lower-bound amount prices the part partial",
			spend: spendOfParts(map[string]domain.TokenUsage{"m1": usageIn}, usd("m1", 4.5, true)),
			want:  pricedOnce("m1", 4.5, CostPartial, SourceAgent, ""),
		},
		{
			name:  "partial table price is kept",
			spend: spendOfParts(map[string]domain.TokenUsage{"half-priced": usageS}, usd("half-priced", 9, false)),
			want:  Price{CostUSD: new(3.0), Status: CostPartial, Parts: map[string]PartPrice{"half-priced": halfPriced}},
		},
		{
			name: "vendor-unit amount leaves the part unpriced",
			spend: spendOfParts(map[string]domain.TokenUsage{"m1": usageIn},
				domain.ReportedAmount{Unit: "credits", Model: "m1", Amount: 2}),
			want: Price{Status: CostUnpriced, Parts: map[string]PartPrice{"m1": unpriced}},
		},
		{
			name:  "unattributed amount prices a one-part run",
			spend: spendOfParts(map[string]domain.TokenUsage{"m1": usageIn}, usd("", 4.5, false)),
			want:  pricedOnce("m1", 4.5, CostEstimated, SourceAgent, ""),
		},
		{
			name:  "unattributed amount prices no part of a two-part run",
			spend: spendOfParts(map[string]domain.TokenUsage{"m1": usageIn, "m2": usageIn}, usd("", 4.5, false)),
			want:  Price{Status: CostUnpriced, Parts: map[string]PartPrice{"m1": unpriced, "m2": unpriced}},
		},
		{
			name: "incomplete kind entry leaves the part to the amount",
			spend: Spend{
				Kind: "k", Measured: true, Totals: usageIn, Parts: map[string]domain.TokenUsage{"m1": usageIn},
				Reported: []domain.ReportedAmount{usd("m1", 4.5, false)},
			},
			rates: incompleteKind,
			want:  pricedOnce("m1", 4.5, CostEstimated, SourceAgent, ""),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := PriceRun(tt.spend, cmp.Or(tt.rates, rates))
			sessionCost, sessionStatus := PriceSession(tt.spend, cmp.Or(tt.rates, rates))

			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("PriceRun(%s) = %s, want %s", asJSON(t, tt.spend), asJSON(t, got), asJSON(t, tt.want))
			}
			if !reflect.DeepEqual(sessionCost, tt.want.CostUSD) || sessionStatus != tt.want.Status {
				t.Errorf("PriceSession(%s) = %s, %q, want %s, %q",
					asJSON(t, tt.spend), asJSON(t, sessionCost), sessionStatus, asJSON(t, tt.want.CostUSD), tt.want.Status)
			}
		})
	}
}
