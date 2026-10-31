package usagesource

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"github.com/sortie-ai/sortie/internal/agent/usage"
)

// DefaultName is the key of the declaration that measures a runtime with no
// declaration of its own. It is reserved: no declaration registers under it.
const DefaultName = "default"

// Default maps the usage object of a prompt result for any runtime. Cache
// counts inside input and reasoning inside output keep every reading at or
// below the true spend whichever convention the runtime follows: a runtime
// that counts either one outside is undercounted, never counted twice. Its
// figure is therefore always a lower bound. It also reads the standard cost
// member of a usage_update session update, a running total over the session in
// the currency the member names.
var Default = Declaration{
	Live: usage.Source{
		Category: usage.SourceProtocol,
		Strategy: usage.StrategyLive,
		Spend: usage.SpendDeclaration{
			Shape:             usage.SpendProcessCumulative,
			CacheInInput:      true,
			ReasoningInOutput: true,
			LowerBound:        true,
			Amounts:           []usage.AmountDeclaration{{Currency: true, Shape: usage.AmountSessionCumulative}},
		},
	},
	PromptSpend:          standardPromptSpend,
	SessionUpdateAmounts: standardUpdateAmounts,
}

// usageUpdateVariant is the sessionUpdate value of a usage_update.
const usageUpdateVariant = "usage_update"

type standardUpdate struct {
	SessionUpdate string          `json:"sessionUpdate"`
	Cost          json.RawMessage `json:"cost"`
}

type standardCost struct {
	Amount   json.RawMessage `json:"amount"`
	Currency json.RawMessage `json:"currency"`
}

// standardUpdateAmounts reads the cost member of a usage_update and nothing from
// any other variant. A currency that is not three ASCII letters leaves the
// reading absent, because the amount then has no unit; an amount that is not a
// finite non-negative number reads unknown in the currency it names.
func standardUpdateAmounts(update json.RawMessage) []usage.AmountReading {
	var decoded standardUpdate
	if err := json.Unmarshal(update, &decoded); err != nil || decoded.SessionUpdate != usageUpdateVariant {
		return nil
	}
	if len(decoded.Cost) == 0 || string(decoded.Cost) == "null" {
		return nil
	}
	var cost standardCost
	if err := json.Unmarshal(decoded.Cost, &cost); err != nil {
		return nil
	}
	var currency string
	if err := json.Unmarshal(cost.Currency, &currency); err != nil || !isCurrencyCode(currency) {
		return nil
	}
	return []usage.AmountReading{{Unit: strings.ToUpper(currency), Amount: jsonAmount(cost.Amount)}}
}

func isCurrencyCode(s string) bool {
	if len(s) != 3 {
		return false
	}
	for i := range len(s) {
		if c := s[i]; (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}

// jsonAmount returns the number a JSON member holds, nil for an absent, null,
// non-numeric, non-finite, or negative member.
func jsonAmount(raw json.RawMessage) *float64 {
	if string(raw) == "null" {
		return nil
	}
	var amount float64
	if err := json.Unmarshal(raw, &amount); err != nil || math.IsNaN(amount) || math.IsInf(amount, 0) || amount < 0 {
		return nil
	}
	return &amount
}

type standardPromptResult struct {
	Usage json.RawMessage `json:"usage"`
}

type standardPromptUsage struct {
	InputTokens       json.RawMessage `json:"inputTokens"`
	OutputTokens      json.RawMessage `json:"outputTokens"`
	CachedReadTokens  json.RawMessage `json:"cachedReadTokens"`
	CachedWriteTokens json.RawMessage `json:"cachedWriteTokens"`
}

// standardPromptSpend reads the process-cumulative counts of a prompt result. It
// leaves out totalTokens and thoughtTokens: the ledger derives the total, and
// reasoning is already inside the output count.
func standardPromptSpend(result json.RawMessage) *usage.SpendReading {
	var decoded standardPromptResult
	if err := json.Unmarshal(result, &decoded); err != nil || len(decoded.Usage) == 0 || string(decoded.Usage) == "null" {
		return nil
	}
	var counts standardPromptUsage
	if err := json.Unmarshal(decoded.Usage, &counts); err != nil {
		return nil
	}
	return &usage.SpendReading{
		Input:      jsonCount(counts.InputTokens),
		Output:     jsonCount(counts.OutputTokens),
		CacheRead:  jsonCount(counts.CachedReadTokens),
		CacheWrite: jsonCount(counts.CachedWriteTokens),
	}
}

// jsonCount returns the integer a JSON member holds, nil for an absent, null,
// fractional, or non-numeric member.
func jsonCount(raw json.RawMessage) *int64 {
	n, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return nil
	}
	return &n
}

type sparsePromptUsage struct {
	InputTokens       json.RawMessage `json:"inputTokens"`
	OutputTokens      json.RawMessage `json:"outputTokens"`
	ThoughtTokens     json.RawMessage `json:"thoughtTokens"`
	CachedReadTokens  json.RawMessage `json:"cachedReadTokens"`
	CachedWriteTokens json.RawMessage `json:"cachedWriteTokens"`
}

// sparsePromptSpend reads the per-turn counts of a prompt result. The runtime
// omits a counter that is zero, so an absent optional member reads zero while a
// present member that is not an integer reads unknown. The input and output
// counts are never omitted, so absent means unknown for them.
func sparsePromptSpend(result json.RawMessage) *usage.SpendReading {
	var decoded standardPromptResult
	if err := json.Unmarshal(result, &decoded); err != nil || len(decoded.Usage) == 0 || string(decoded.Usage) == "null" {
		return nil
	}
	var counts sparsePromptUsage
	if err := json.Unmarshal(decoded.Usage, &counts); err != nil {
		return nil
	}
	return &usage.SpendReading{
		Input:      jsonCount(counts.InputTokens),
		Output:     jsonCount(counts.OutputTokens),
		Reasoning:  jsonCountOmittedAtZero(counts.ThoughtTokens),
		CacheRead:  jsonCountOmittedAtZero(counts.CachedReadTokens),
		CacheWrite: jsonCountOmittedAtZero(counts.CachedWriteTokens),
	}
}

// jsonCountOmittedAtZero returns the integer of a counter the runtime leaves
// out when it is zero, so an absent member reads zero.
func jsonCountOmittedAtZero(raw json.RawMessage) *int64 {
	if len(raw) == 0 {
		var zero int64
		return &zero
	}
	return jsonCount(raw)
}
