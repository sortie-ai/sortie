package server

import (
	"fmt"
	"math"
)

// TokenRateConfig holds per-token-type USD rates for cost estimation.
// All rates are in USD per 1 million tokens (per-mtok). A nil pointer
// indicates the rate is not configured; cost estimation is suppressed
// for that token type.
type TokenRateConfig struct {
	InputPerMtok     *float64
	OutputPerMtok    *float64
	CacheReadPerMtok *float64
}

// TokenRates maps agent adapter kind strings to their token rate
// configuration. A nil or empty map means no cost estimates are shown
// on the dashboard.
type TokenRates map[string]TokenRateConfig

// ParseTokenRates extracts and validates token rates from the
// Extensions map produced by config parsing. Returns nil rates when
// the "token_rates" key is absent or empty. Warnings are advisory
// and do not prevent boot.
func ParseTokenRates(extensions map[string]any) (TokenRates, []string) {
	raw, ok := extensions["token_rates"]
	if !ok || raw == nil {
		return nil, nil
	}

	topMap, ok := raw.(map[string]any)
	if !ok {
		return nil, []string{fmt.Sprintf("token_rates: expected map, got %T", raw)}
	}
	if len(topMap) == 0 {
		return nil, nil
	}

	var warnings []string
	rates := make(TokenRates, len(topMap))

	for kind, val := range topMap {
		kindMap, ok := val.(map[string]any)
		if !ok {
			warnings = append(warnings, fmt.Sprintf("token_rates.%s: expected map, got %T", kind, val))
			continue
		}

		var cfg TokenRateConfig
		if v, w := extractRate(kindMap, "input_per_mtok", kind); w != "" {
			warnings = append(warnings, w)
		} else {
			cfg.InputPerMtok = v
		}
		if v, w := extractRate(kindMap, "output_per_mtok", kind); w != "" {
			warnings = append(warnings, w)
		} else {
			cfg.OutputPerMtok = v
		}
		if v, w := extractRate(kindMap, "cache_read_per_mtok", kind); w != "" {
			warnings = append(warnings, w)
		} else {
			cfg.CacheReadPerMtok = v
		}

		if cfg.InputPerMtok == nil && cfg.OutputPerMtok == nil && cfg.CacheReadPerMtok == nil {
			continue
		}
		rates[kind] = cfg
	}

	if len(rates) == 0 {
		return nil, warnings
	}
	return rates, warnings
}

// extractRate reads a non-negative float64 from a map entry. Returns
// nil with empty warning when the key is absent, nil with a warning
// when the value is invalid.
func extractRate(m map[string]any, key, kind string) (*float64, string) {
	raw, ok := m[key]
	if !ok || raw == nil {
		return nil, ""
	}

	var f float64
	switch v := raw.(type) {
	case float64:
		f = v
	case int:
		f = float64(v)
	case int64:
		f = float64(v)
	default:
		return nil, fmt.Sprintf("token_rates.%s.%s: expected number, got %T", kind, key, raw)
	}

	if f < 0 {
		return nil, fmt.Sprintf("token_rates.%s.%s: negative rate %v", kind, key, f)
	}
	return &f, ""
}

// estimateCost computes estimated USD cost from token counts and a rate
// config. Returns nil when rates is nil or all rate fields are nil.
func estimateCost(input, output, cacheRead int64, rates *TokenRateConfig) *float64 {
	if rates == nil {
		return nil
	}

	anySet := false
	var cost float64

	if rates.InputPerMtok != nil {
		cost += float64(input) * *rates.InputPerMtok / 1_000_000
		anySet = true
	}
	if rates.OutputPerMtok != nil {
		cost += float64(output) * *rates.OutputPerMtok / 1_000_000
		anySet = true
	}
	if rates.CacheReadPerMtok != nil {
		cost += float64(cacheRead) * *rates.CacheReadPerMtok / 1_000_000
		anySet = true
	}

	if !anySet {
		return nil
	}
	return &cost
}

// fmtCost formats a USD cost value as a string with two decimal places.
// Values >= 1000 receive comma thousand separators (e.g. "$1,234.56").
// Rounding is performed on the integer-cents representation to avoid
// float splitting artifacts near boundaries (e.g. 999.999 → "$1,000.00").
func fmtCost(v float64) string {
	cents := int64(math.Round(v * 100))
	dollars := cents / 100
	remainder := cents % 100
	if dollars >= 1000 {
		return fmt.Sprintf("$%s.%02d", fmtInt(dollars), remainder)
	}
	return fmt.Sprintf("$%d.%02d", dollars, remainder)
}
