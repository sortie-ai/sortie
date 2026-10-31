package pricing

import (
	"cmp"
	"slices"

	"github.com/sortie-ai/sortie/internal/domain"
)

// CostStatus says how far a cost figure can be trusted.
type CostStatus string

const (
	// CostEstimated marks a cost priced from every class the usage spent.
	CostEstimated CostStatus = "estimated"

	// CostPartial marks a cost that leaves out usage the run could not
	// account for.
	CostPartial CostStatus = "partial"

	// CostUnpriced marks usage that spent tokens but has no rate to price it.
	CostUnpriced CostStatus = "unpriced"

	// CostUnmeasured marks usage the agent never reported.
	CostUnmeasured CostStatus = "unmeasured"
)

// PriceSource names the rate source that priced a part.
type PriceSource string

const (
	// SourceOperator is the operator's token_rates section, alone or laid
	// over a table.
	SourceOperator PriceSource = "operator"

	// SourceFetched is the shared price table downloaded at runtime.
	SourceFetched PriceSource = "fetched"

	// SourceEmbedded is the price table compiled into the binary.
	SourceEmbedded PriceSource = "embedded"

	// SourceAgent is the US-dollar amount the agent reported for the part,
	// used for a model no rate layer prices.
	SourceAgent PriceSource = "agent"
)

// Spend is the usage of one finished run or one running session.
type Spend struct {
	// Kind is the dispatch's agent kind, the key of a kind entry in [Rates].
	Kind string

	// Model is the model tokens arriving now are attributed to: the model
	// the session's events named last, else its configured model. Only
	// [PriceSession] reads it, for a session that has spent nothing.
	Model string

	// Measured is false when the agent reported no usage at all.
	Measured bool

	Totals domain.TokenUsage

	// Parts is the usage per model and sums to Totals class by class.
	Parts map[string]domain.TokenUsage

	// UnaccountedTurns counts turns that spent tokens the agent did not
	// report. [PriceSession] ignores it.
	UnaccountedTurns int

	// Reported is every amount the agent reported for the run, in the unit it
	// reported it in.
	Reported []domain.ReportedAmount
}

// TokenClass names a class of tokens a rate prices.
type TokenClass string

const (
	// ClassInput is fresh input, which excludes cache reads and writes.
	ClassInput TokenClass = "input"

	// ClassOutput is generated tokens.
	ClassOutput TokenClass = "output"

	// ClassCacheRead is input served from the prompt cache.
	ClassCacheRead TokenClass = "cache_read"

	// ClassCacheWrite is input written to the prompt cache.
	ClassCacheWrite TokenClass = "cache_write"
)

// tokenClasses lists the classes in the order [PartPrice.Unrated] reports
// them, indexed by rate class.
var tokenClasses = [rateClasses]TokenClass{
	rateInput:      ClassInput,
	rateOutput:     ClassOutput,
	rateCacheRead:  ClassCacheRead,
	rateCacheWrite: ClassCacheWrite,
}

// PartPrice is the price of one model's share of a run.
type PartPrice struct {
	CostUSD *float64

	// Status is [CostEstimated], [CostPartial], or [CostUnpriced].
	Status CostStatus

	// Source is empty when Status is [CostUnpriced].
	Source PriceSource

	// TableVersion is the version of the rates that priced the part, empty
	// when Source is empty.
	TableVersion string

	// Unrated lists the classes whose missing rate left the part partial or
	// unpriced, in the order input, output, cache_read, cache_write; nil when
	// none.
	Unrated []TokenClass
}

// UnratedNames returns [PartPrice.Unrated] as the strings persistence stores;
// nil when none.
func (p PartPrice) UnratedNames() []string {
	var names []string
	for _, class := range p.Unrated {
		names = append(names, string(class))
	}
	return names
}

// Price is the cost of a finished run and of each of its parts.
type Price struct {
	CostUSD *float64
	Status  CostStatus

	// Parts holds one entry per [Spend.Parts] key, and is nil when the run
	// is unmeasured, spent nothing, or spent tokens in no class.
	Parts map[string]PartPrice
}

// PriceRun prices a finished run under r. A kind entry for [Spend.Kind]
// prices the run once, on its summed tokens, so the run's cost is not the
// sum of its parts' costs: the fresh-input floor applies per priced usage
// and would otherwise move every multi-model run. Without one, each part is
// priced on its own tokens and the run's cost is their sum.
//
// A part the rates leave unpriced is priced at the US-dollar amount in
// [Spend.Reported] attributed to its model, and a part the rates price keeps
// their price. PriceRun is pure and never mutates its inputs.
func PriceRun(s Spend, r *Rates) Price {
	if !s.Measured {
		return Price{Status: CostUnmeasured}
	}
	status := CostEstimated
	if s.UnaccountedTurns > 0 {
		status = CostPartial
	}
	if !hasSpent(s.Totals) {
		zero := 0.0
		return Price{CostUSD: &zero, Status: status}
	}
	if !hasSpentInClass(s.Totals) {
		return Price{Status: CostUnpriced}
	}

	if r != nil && r.kinds[s.Kind] {
		return priceRunByKind(s, r, status)
	}
	if len(s.Parts) == 0 {
		return Price{Status: CostUnpriced}
	}

	parts, cost, priced, degraded := priceParts(s.Parts, s.Reported, tableRates(r))
	if !priced {
		return Price{Status: CostUnpriced, Parts: parts}
	}
	if degraded {
		status = CostPartial
	}
	return Price{CostUSD: &cost, Status: status, Parts: parts}
}

func priceRunByKind(s Spend, r *Rates, status CostStatus) Price {
	entry, priceable := completeEntry(r, s.Kind)
	if !priceable {
		if len(s.Parts) == 0 {
			return Price{Status: CostUnpriced}
		}
		parts, cost, priced, degraded := priceParts(s.Parts, s.Reported, incompleteEntry(entry))
		if !priced {
			return Price{Status: CostUnpriced, Parts: parts}
		}
		if degraded {
			status = CostPartial
		}
		return Price{CostUSD: &cost, Status: status, Parts: parts}
	}

	return Price{
		CostUSD: EstimateCost(s.Totals, &entry),
		Status:  status,
		Parts: partPrices(s.Parts, func(part domain.TokenUsage) PartPrice {
			return PartPrice{
				CostUSD:      EstimateCost(part, &entry),
				Status:       CostEstimated,
				Source:       SourceOperator,
				TableVersion: r.version,
			}
		}),
	}
}

// PriceSession prices a running session's spend so far under r. A session
// under an incomplete kind entry is unpriced even before it spends, since it
// still can. A part the rates leave unpriced is priced as [PriceRun] prices
// it. PriceSession is pure and never mutates its inputs.
func PriceSession(s Spend, r *Rates) (cost *float64, status CostStatus) {
	if !s.Measured {
		return nil, CostUnmeasured
	}
	if r != nil && r.kinds[s.Kind] {
		return priceSessionByKind(s, r)
	}
	if !hasSpent(s.Totals) {
		if !r.resolve(s.Model).pricesAnything() {
			return nil, CostUnpriced
		}
		zero := 0.0
		return &zero, CostEstimated
	}
	if !hasSpentInClass(s.Totals) || len(s.Parts) == 0 {
		return nil, CostUnpriced
	}

	_, total, priced, degraded := priceParts(s.Parts, s.Reported, tableRates(r))
	return sessionCost(total, priced, degraded)
}

func sessionCost(total float64, priced, degraded bool) (cost *float64, status CostStatus) {
	switch {
	case !priced:
		return nil, CostUnpriced
	case degraded:
		return &total, CostPartial
	}
	return &total, CostEstimated
}

func priceSessionByKind(s Spend, r *Rates) (cost *float64, status CostStatus) {
	entry, priceable := completeEntry(r, s.Kind)
	if !priceable {
		if !hasSpentInClass(s.Totals) || len(s.Parts) == 0 {
			return nil, CostUnpriced
		}
		_, total, priced, degraded := priceParts(s.Parts, s.Reported, incompleteEntry(entry))
		return sessionCost(total, priced, degraded)
	}
	if !hasSpent(s.Totals) {
		zero := 0.0
		return &zero, CostEstimated
	}
	if !hasSpentInClass(s.Totals) {
		return nil, CostUnpriced
	}
	return EstimateCost(s.Totals, &entry), CostEstimated
}

// partRater prices one model's usage from the rates alone.
type partRater func(model string, usage domain.TokenUsage) PartPrice

func tableRates(r *Rates) partRater {
	return func(model string, usage domain.TokenUsage) PartPrice {
		return pricePart(usage, r.resolve(model))
	}
}

// incompleteEntry leaves every part unpriced, naming the classes entry gives
// no rate.
func incompleteEntry(entry TokenRateConfig) partRater {
	unrated := unratedByEntry(entry)
	return func(string, domain.TokenUsage) PartPrice {
		return PartPrice{Status: CostUnpriced, Unrated: slices.Clone(unrated)}
	}
}

// priceParts prices every part with rate and sums their costs. A part that
// spent and that rate leaves unpriced is priced from the amount in reported
// instead. priced is false when no part that spent in a class got a price, and
// degraded is true when such a part is partial or unpriced.
func priceParts(parts map[string]domain.TokenUsage, reported []domain.ReportedAmount, rate partRater) (prices map[string]PartPrice, cost float64, priced, degraded bool) {
	prices = make(map[string]PartPrice, len(parts))
	for model, usage := range parts {
		part := rate(model, usage)
		if part.Status == CostUnpriced && hasSpent(usage) {
			if fromAgent, ok := reportedPart(reported, model, len(parts)); ok {
				part = fromAgent
			}
		}
		prices[model] = part
		if part.CostUSD != nil {
			cost += *part.CostUSD
		}
		if !hasSpentInClass(usage) && part.Source != SourceAgent {
			continue
		}
		switch part.Status {
		case CostEstimated:
			priced = true
		case CostPartial:
			priced, degraded = true, true
		default:
			degraded = true
		}
	}
	return prices, cost, priced, degraded
}

// reportedPart prices a part from the US-dollar amount reported for its
// model. An amount attributed to no model belongs to the part only when the
// run has no other part and no amount names a model.
func reportedPart(reported []domain.ReportedAmount, model string, partCount int) (PartPrice, bool) {
	amount, ok := usdAmount(reported, model, partCount)
	if !ok {
		return PartPrice{}, false
	}
	status := CostEstimated
	if amount.LowerBound {
		status = CostPartial
	}
	return PartPrice{CostUSD: &amount.Amount, Status: status, Source: SourceAgent}, true
}

func usdAmount(reported []domain.ReportedAmount, model string, partCount int) (domain.ReportedAmount, bool) {
	attributed := false
	for _, amount := range reported {
		if amount.Unit != domain.UnitUSD {
			continue
		}
		if model != "" && amount.Model == model {
			return amount, true
		}
		attributed = attributed || amount.Model != ""
	}
	if partCount != 1 || attributed {
		return domain.ReportedAmount{}, false
	}
	for _, amount := range reported {
		if amount.Unit == domain.UnitUSD {
			return amount, true
		}
	}
	return domain.ReportedAmount{}, false
}

// pricePart prices one model's usage at res; a nil res holds no rate.
// A class with tokens and no rate is missing and is never priced at another
// class's rate.
func pricePart(usage domain.TokenUsage, res *resolution) PartPrice {
	if !hasSpentInClass(usage) {
		if usage.TotalTokens != 0 {
			return PartPrice{Status: CostUnpriced}
		}
		zero := 0.0
		part := PartPrice{CostUSD: &zero, Status: CostEstimated}
		if res != nil {
			part.Source, part.TableVersion = res.source, res.version
		}
		return part
	}

	tokens := [rateClasses]int64{
		rateInput:      max(usage.InputTokens-usage.CacheReadTokens-usage.CacheWriteTokens, 0),
		rateOutput:     usage.OutputTokens,
		rateCacheRead:  usage.CacheReadTokens,
		rateCacheWrite: usage.CacheWriteTokens,
	}
	var cost float64
	var unrated [rateClasses]bool
	priced := false
	for _, class := range [...]int{rateInput, rateCacheRead, rateCacheWrite, rateOutput} {
		if tokens[class] <= 0 {
			continue
		}
		if res == nil || res.perToken[class] == nil {
			unrated[class] = true
			continue
		}
		cost += float64(tokens[class]) * *res.perToken[class]
		priced = true
	}
	var missing []TokenClass
	for class, isUnrated := range unrated {
		if isUnrated {
			missing = append(missing, tokenClasses[class])
		}
	}
	if !priced {
		return PartPrice{Status: CostUnpriced, Unrated: missing}
	}
	status := CostEstimated
	if missing != nil {
		status = CostPartial
	}
	return PartPrice{CostUSD: &cost, Status: status, Source: res.source, TableVersion: res.version, Unrated: missing}
}

// unratedByEntry lists the classes entry gives no rate, after an unset cache
// rate falls back to the input rate as [EstimateCost] prices it.
func unratedByEntry(entry TokenRateConfig) []TokenClass {
	var unrated []TokenClass
	for class, rate := range [rateClasses]*float64{
		rateInput:      entry.InputPerMtok,
		rateOutput:     entry.OutputPerMtok,
		rateCacheRead:  cmp.Or(entry.CacheReadPerMtok, entry.InputPerMtok),
		rateCacheWrite: cmp.Or(entry.CacheWritePerMtok, entry.InputPerMtok),
	} {
		if rate == nil {
			unrated = append(unrated, tokenClasses[class])
		}
	}
	return unrated
}

func completeEntry(r *Rates, kind string) (TokenRateConfig, bool) {
	entry := r.entries[kind]
	return entry, entry.Complete()
}

func partPrices(parts map[string]domain.TokenUsage, price func(domain.TokenUsage) PartPrice) map[string]PartPrice {
	if len(parts) == 0 {
		return nil
	}
	prices := make(map[string]PartPrice, len(parts))
	for model, usage := range parts {
		prices[model] = price(usage)
	}
	return prices
}

func hasSpent(u domain.TokenUsage) bool {
	return hasSpentInClass(u) || u.TotalTokens != 0
}

// hasSpentInClass is false for usage reported as a bare total, which no rate
// can price.
func hasSpentInClass(u domain.TokenUsage) bool {
	return u.InputTokens != 0 || u.OutputTokens != 0 || u.CacheReadTokens != 0 || u.CacheWriteTokens != 0
}
