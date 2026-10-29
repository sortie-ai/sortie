package usagesource

import (
	"encoding/json"
	"strconv"

	"github.com/sortie-ai/sortie/internal/agent/usage"
)

// DefaultName is the key of the declaration that measures a runtime with no
// declaration of its own. It is reserved: no declaration registers under it.
const DefaultName = "default"

// Default maps the usage object of a prompt result for any runtime. Cache
// counts inside input and reasoning inside output keep every reading at or
// below the true spend whichever convention the runtime follows: a runtime
// that counts either one outside is undercounted, never counted twice. Its
// figure is therefore always a lower bound.
var Default = Declaration{
	Live: usage.Source{
		Category: usage.SourceProtocol,
		Strategy: usage.StrategyLive,
		Spend: usage.SpendDeclaration{
			Shape:             usage.SpendProcessCumulative,
			CacheInInput:      true,
			ReasoningInOutput: true,
			LowerBound:        true,
		},
	},
	PromptSpend: standardPromptSpend,
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
