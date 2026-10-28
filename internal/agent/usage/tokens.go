package usage

import (
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
)

// Add returns the componentwise sum of a and b, with TotalTokens
// recomputed as InputTokens plus OutputTokens.
func Add(a, b domain.TokenUsage) domain.TokenUsage {
	sum := domain.TokenUsage{
		InputTokens:      a.InputTokens + b.InputTokens,
		OutputTokens:     a.OutputTokens + b.OutputTokens,
		CacheReadTokens:  a.CacheReadTokens + b.CacheReadTokens,
		CacheWriteTokens: a.CacheWriteTokens + b.CacheWriteTokens,
	}
	sum.TotalTokens = sum.InputTokens + sum.OutputTokens
	return sum
}

// Subtract returns the componentwise difference a minus b, floored at
// zero in every component, with TotalTokens recomputed as InputTokens
// plus OutputTokens.
func Subtract(a, b domain.TokenUsage) domain.TokenUsage {
	diff := domain.TokenUsage{
		InputTokens:      max(a.InputTokens-b.InputTokens, 0),
		OutputTokens:     max(a.OutputTokens-b.OutputTokens, 0),
		CacheReadTokens:  max(a.CacheReadTokens-b.CacheReadTokens, 0),
		CacheWriteTokens: max(a.CacheWriteTokens-b.CacheWriteTokens, 0),
	}
	diff.TotalTokens = diff.InputTokens + diff.OutputTokens
	return diff
}

// Max returns the componentwise maximum of a and b, with TotalTokens
// recomputed as InputTokens plus OutputTokens.
func Max(a, b domain.TokenUsage) domain.TokenUsage {
	result := domain.TokenUsage{
		InputTokens:      max(a.InputTokens, b.InputTokens),
		OutputTokens:     max(a.OutputTokens, b.OutputTokens),
		CacheReadTokens:  max(a.CacheReadTokens, b.CacheReadTokens),
		CacheWriteTokens: max(a.CacheWriteTokens, b.CacheWriteTokens),
	}
	result.TotalTokens = result.InputTokens + result.OutputTokens
	return result
}

// Sum returns the sum of every entry of parts.
func Sum(parts map[string]domain.TokenUsage) domain.TokenUsage {
	var total domain.TokenUsage
	for _, part := range parts {
		total = Add(total, part)
	}
	return total
}

// AddByModel returns the per-name sum of a and b as a new map; nil when
// both are nil.
func AddByModel(a, b map[string]domain.TokenUsage) map[string]domain.TokenUsage {
	if a == nil && b == nil {
		return nil
	}
	sum := make(map[string]domain.TokenUsage, len(a)+len(b))
	for _, part := range []map[string]domain.TokenUsage{a, b} {
		for name, u := range part {
			sum[name] = Add(sum[name], u)
		}
	}
	return sum
}

// NormalizeByModel returns a new map of m with names trimmed and merged,
// counts floored at zero, and empty names and zero entries dropped; nil
// when no entry remains.
func NormalizeByModel(m map[string]domain.TokenUsage) map[string]domain.TokenUsage {
	var normalized map[string]domain.TokenUsage
	for name, u := range m {
		name = strings.TrimSpace(name)
		u = Subtract(u, domain.TokenUsage{})
		if name == "" || IsZero(u) {
			continue
		}
		if normalized == nil {
			normalized = make(map[string]domain.TokenUsage, len(m))
		}
		normalized[name] = Add(normalized[name], u)
	}
	return normalized
}

// FellBelow reports whether reading is below latest in any counted
// component, which marks a counter that started over.
func FellBelow(reading, latest domain.TokenUsage) bool {
	return reading.InputTokens < latest.InputTokens ||
		reading.OutputTokens < latest.OutputTokens ||
		reading.CacheReadTokens < latest.CacheReadTokens ||
		reading.CacheWriteTokens < latest.CacheWriteTokens
}

// IsZero reports whether every counted component of u is zero.
func IsZero(u domain.TokenUsage) bool {
	return u.InputTokens == 0 && u.OutputTokens == 0 &&
		u.CacheReadTokens == 0 && u.CacheWriteTokens == 0
}
