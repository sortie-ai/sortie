package agentcore

import "github.com/sortie-ai/sortie/internal/domain"

// UsageAccumulator tracks cumulative token usage across the events of a
// single agent turn. It is not safe for concurrent use; each RunTurn
// invocation constructs its own UsageAccumulator and uses it on a single
// goroutine.
type UsageAccumulator struct {
	current domain.TokenUsage
}

// NewUsageAccumulator returns a zero-value UsageAccumulator ready for use.
func NewUsageAccumulator() *UsageAccumulator {
	return &UsageAccumulator{}
}

// AddDelta adds per-request token delta counts to the running cumulative
// totals and returns the updated snapshot. ready is true when cumulative
// output tokens are non-zero, indicating the snapshot is suitable for
// emission as an EventTokenUsage event. When ready is false the snapshot is
// valid but the caller should defer emission.
//
// TotalTokens in the returned snapshot is always InputTokens + OutputTokens.
// All arguments must be non-negative; negative values are clamped to 0.
func (u *UsageAccumulator) AddDelta(input, output, cacheRead int64) (domain.TokenUsage, bool) {
	if input < 0 {
		input = 0
	}
	if output < 0 {
		output = 0
	}
	if cacheRead < 0 {
		cacheRead = 0
	}
	u.current.InputTokens += input
	u.current.OutputTokens += output
	u.current.CacheReadTokens += cacheRead
	u.current.TotalTokens = u.current.InputTokens + u.current.OutputTokens
	return u.current, u.current.OutputTokens > 0
}

// ReplaceCumulative replaces the accumulator's internal state with the
// complete snapshot in and returns it unchanged. Use this for adapters that
// receive a single authoritative usage event (e.g., Codex's turn/completed).
// Subsequent calls to [UsageAccumulator.Snapshot] return in.
func (u *UsageAccumulator) ReplaceCumulative(in domain.TokenUsage) domain.TokenUsage {
	u.current = in
	return in
}

// Snapshot returns the current cumulative token usage without modifying the
// accumulator. Returns a zero [domain.TokenUsage] when no deltas have been
// added and ReplaceCumulative has not been called.
func (u *UsageAccumulator) Snapshot() domain.TokenUsage {
	return u.current
}
