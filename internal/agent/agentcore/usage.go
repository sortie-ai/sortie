package agentcore

import (
	"maps"

	"github.com/sortie-ai/sortie/internal/agent/usage"
	"github.com/sortie-ai/sortie/internal/domain"
)

// RunUsage tracks normalized token usage across every turn of one agent
// session. It combines a settled total (the latest run-cumulative figure
// the adapter set) with a provisional contribution from the turn
// currently in flight, and never returns a snapshot lower than any
// snapshot it has already returned, so a caller that reports each
// method's result observes a monotonically non-decreasing sequence per
// component even when a later provisional estimate would otherwise be
// smaller than an earlier one.
//
// Not safe for concurrent use; each adapter session constructs its own
// RunUsage in StartSession and uses it on the single goroutine that
// executes RunTurn for that session.
type RunUsage struct {
	settled     domain.TokenUsage
	provisional domain.TokenUsage
	high        domain.TokenUsage
	byModel     map[string]domain.TokenUsage
}

// NewRunUsage returns a zero-value RunUsage ready for use.
func NewRunUsage() *RunUsage {
	return &RunUsage{}
}

// SetTurnProvisional replaces the in-flight turn's contribution with
// turn and returns the resulting run-cumulative snapshot (settled plus
// the new provisional contribution), raised against the highest
// snapshot returned so far.
func (u *RunUsage) SetTurnProvisional(turn domain.TokenUsage) domain.TokenUsage {
	u.provisional = usage.Subtract(turn, domain.TokenUsage{})
	return u.raise(usage.Add(u.settled, u.provisional))
}

// SetRunCumulative replaces the settled total with run outright, clears
// the in-flight turn's provisional contribution, and returns the new
// settled total, raised against the highest snapshot returned so far.
// It also records runByModel, normalized, as the settled per-model split.
func (u *RunUsage) SetRunCumulative(run domain.TokenUsage, runByModel map[string]domain.TokenUsage) domain.TokenUsage {
	u.settled = usage.Subtract(run, domain.TokenUsage{})
	u.provisional = domain.TokenUsage{}
	u.byModel = NormalizeUsageByModel(runByModel)
	return u.raise(u.settled)
}

// ByModel returns a copy of the settled per-model split, or nil when none.
func (u *RunUsage) ByModel() map[string]domain.TokenUsage {
	return maps.Clone(u.byModel)
}

// Snapshot returns the highest run-cumulative snapshot returned so far
// by any method, without modifying u. Returns the zero value before any
// other method has been called.
func (u *RunUsage) Snapshot() domain.TokenUsage {
	return u.high
}

// raise sets u.high to the componentwise maximum of u.high and snapshot
// and returns the result.
func (u *RunUsage) raise(snapshot domain.TokenUsage) domain.TokenUsage {
	u.high = MaxUsage(u.high, snapshot)
	return u.high
}

// NormalizeUsageByModel returns a new map of m with names trimmed and
// merged, counts floored at zero, and empty names and zero entries
// dropped; nil when no entry remains.
func NormalizeUsageByModel(m map[string]domain.TokenUsage) map[string]domain.TokenUsage {
	return usage.NormalizeByModel(m)
}

// AddUsageByModel returns the per-name sum of a and b as a new map; nil
// when both are nil.
func AddUsageByModel(a, b map[string]domain.TokenUsage) map[string]domain.TokenUsage {
	return usage.AddByModel(a, b)
}

// SubtractUsage returns the componentwise difference a minus b, floored
// at zero in every component, with TotalTokens recomputed as
// InputTokens plus OutputTokens.
func SubtractUsage(a, b domain.TokenUsage) domain.TokenUsage {
	return usage.Subtract(a, b)
}

// MaxUsage returns the componentwise maximum of a and b, with
// TotalTokens recomputed as InputTokens plus OutputTokens.
func MaxUsage(a, b domain.TokenUsage) domain.TokenUsage {
	return usage.Max(a, b)
}

// AddUsage returns the componentwise sum of a and b, with TotalTokens
// recomputed as InputTokens plus OutputTokens.
func AddUsage(a, b domain.TokenUsage) domain.TokenUsage {
	return usage.Add(a, b)
}
