package agentcore

import "github.com/sortie-ai/sortie/internal/domain"

// RunUsage accumulates normalized token usage across every turn of one
// agent session. It combines a settled total (closed turns and
// run-cumulative snapshots) with a provisional contribution from the
// turn currently in flight, and never returns a snapshot lower than any
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
	u.provisional = clamp(turn)
	return u.raise(add(u.settled, u.provisional))
}

// AddTurn adds turn to the settled total, clears the in-flight turn's
// provisional contribution, and returns the new settled total, raised
// against the highest snapshot returned so far.
func (u *RunUsage) AddTurn(turn domain.TokenUsage) domain.TokenUsage {
	u.settled = add(u.settled, clamp(turn))
	u.provisional = domain.TokenUsage{}
	return u.raise(u.settled)
}

// SetRunCumulative replaces the settled total with run outright, clears
// the in-flight turn's provisional contribution, and returns the new
// settled total, raised against the highest snapshot returned so far.
func (u *RunUsage) SetRunCumulative(run domain.TokenUsage) domain.TokenUsage {
	u.settled = clamp(run)
	u.provisional = domain.TokenUsage{}
	return u.raise(u.settled)
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
	u.high = domain.TokenUsage{
		InputTokens:     max(u.high.InputTokens, snapshot.InputTokens),
		OutputTokens:    max(u.high.OutputTokens, snapshot.OutputTokens),
		CacheReadTokens: max(u.high.CacheReadTokens, snapshot.CacheReadTokens),
	}
	u.high.TotalTokens = u.high.InputTokens + u.high.OutputTokens
	return u.high
}

// clamp floors every negative component of usage at zero and recomputes
// TotalTokens as InputTokens plus OutputTokens.
func clamp(usage domain.TokenUsage) domain.TokenUsage {
	usage.InputTokens = max(usage.InputTokens, 0)
	usage.OutputTokens = max(usage.OutputTokens, 0)
	usage.CacheReadTokens = max(usage.CacheReadTokens, 0)
	usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	return usage
}

// add returns the componentwise sum of a and b, with TotalTokens
// recomputed as InputTokens plus OutputTokens.
func add(a, b domain.TokenUsage) domain.TokenUsage {
	sum := domain.TokenUsage{
		InputTokens:     a.InputTokens + b.InputTokens,
		OutputTokens:    a.OutputTokens + b.OutputTokens,
		CacheReadTokens: a.CacheReadTokens + b.CacheReadTokens,
	}
	sum.TotalTokens = sum.InputTokens + sum.OutputTokens
	return sum
}
