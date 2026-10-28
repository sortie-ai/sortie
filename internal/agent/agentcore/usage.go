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

// ResumeBaseline separates what a run added to a resumable session from
// the usage the runtime's session counter already held when the run
// started. The counter is cumulative over every launch on the session,
// so a reading alone overstates the run.
//
// The zero value is an unknown baseline; the first reading that is not all
// zero fixes it by estimate, and the per-model baseline is tracked beside
// it. One value serves one session on the goroutine that executes its
// turns and is never reset between turns, because the baseline is what
// makes the readings of later turns comparable.
type ResumeBaseline struct {
	baseline    domain.TokenUsage
	latest      domain.TokenUsage
	beforeReset domain.TokenUsage
	known       bool

	modelBaseline    map[string]domain.TokenUsage
	modelLatest      map[string]domain.TokenUsage
	modelBeforeReset map[string]domain.TokenUsage
	modelKnown       bool
}

// NewResumeBaseline returns a baseline starting from resume, floored at
// zero in every component, and per model from resumeByModel when it sums
// to resume. A nil resume returns an unknown baseline.
func NewResumeBaseline(resume *domain.TokenUsage, resumeByModel map[string]domain.TokenUsage) *ResumeBaseline {
	if resume == nil {
		return &ResumeBaseline{}
	}
	start := usage.Subtract(*resume, domain.TokenUsage{})
	b := &ResumeBaseline{baseline: start, latest: start, known: true}
	if usage.IsZero(start) {
		b.modelBaseline, b.modelLatest, b.modelKnown = map[string]domain.TokenUsage{}, map[string]domain.TokenUsage{}, true
		return b
	}
	seed := NormalizeUsageByModel(resumeByModel)
	if seed != nil && sameCounted(SumUsage(seed), start) {
		b.modelBaseline, b.modelLatest, b.modelKnown = seed, maps.Clone(seed), true
	}
	return b
}

// Observe records one reading of the session counter and returns the
// run's usage so far. own is the part of the reading the runtime
// attributes to the current request or turn, or nil when it attributes
// none. readingByModel is the per-model split of reading, or nil when the
// runtime names none.
//
// A reading with every counted component zero has no effect. When the
// baseline is unknown, or the reading is below the latest one in any
// counted component, the counter is taken to have started over: the
// usage so far is kept and the baseline becomes the reading less own.
// estimated is then true exactly when that baseline is non-zero in a
// counted component, because own may not cover everything the reading
// added; a zero baseline loses nothing and is exact. Otherwise estimated
// is false.
//
// The per-model baseline restarts with the combined one. A first non-nil
// readingByModel on an unseeded per-model baseline fixes it and claims no
// spend, so an unusable seed cannot inflate the run.
func (b *ResumeBaseline) Observe(reading domain.TokenUsage, readingByModel map[string]domain.TokenUsage, own *domain.TokenUsage) (run domain.TokenUsage, runByModel map[string]domain.TokenUsage, estimated bool) {
	if usage.IsZero(reading) {
		run, runByModel = b.Run()
		return run, runByModel, false
	}
	perModel := NormalizeUsageByModel(readingByModel)
	if perModel == nil {
		perModel = map[string]domain.TokenUsage{}
	}
	if !b.known || usage.FellBelow(reading, b.latest) {
		b.beforeReset, b.modelBeforeReset = b.Run()
		if b.modelBeforeReset == nil {
			b.modelBeforeReset = map[string]domain.TokenUsage{}
		}
		b.baseline = reading
		if own != nil {
			b.baseline = SubtractUsage(reading, *own)
		}
		b.known = true
		estimated = !usage.IsZero(b.baseline)
		b.modelBaseline, b.modelLatest, b.modelKnown = perModel, maps.Clone(perModel), true
	} else if readingByModel != nil {
		if !b.modelKnown {
			b.modelBaseline, b.modelKnown = perModel, true
		}
		b.modelLatest = maps.Clone(perModel)
	}
	b.latest = reading
	run, runByModel = b.Run()
	return run, runByModel, estimated
}

// Run returns the usage the run added to the session: the usage kept
// across counter resets plus the latest reading less the baseline, and
// the same split by model. The combined figure is zero and the split nil
// while their baselines are unknown.
func (b *ResumeBaseline) Run() (domain.TokenUsage, map[string]domain.TokenUsage) {
	if !b.known {
		return domain.TokenUsage{}, nil
	}
	run := AddUsage(b.beforeReset, SubtractUsage(b.latest, b.baseline))
	if !b.modelKnown {
		return run, nil
	}
	since := make(map[string]domain.TokenUsage, len(b.modelLatest))
	for name, latest := range b.modelLatest {
		since[name] = SubtractUsage(latest, b.modelBaseline[name])
	}
	return run, NormalizeUsageByModel(AddUsageByModel(b.modelBeforeReset, since))
}

// Resume returns a copy of the latest reading, which a later run
// resuming the session starts from, or nil while the baseline is
// unknown. The per-model split is returned only when it sums to that
// reading.
func (b *ResumeBaseline) Resume() (*domain.TokenUsage, map[string]domain.TokenUsage) {
	if !b.known {
		return nil, nil
	}
	latest := b.latest
	if !b.modelKnown || !sameCounted(SumUsage(b.modelLatest), latest) {
		return &latest, nil
	}
	return &latest, maps.Clone(b.modelLatest)
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

// SumUsage returns the sum of every entry of parts.
func SumUsage(parts map[string]domain.TokenUsage) domain.TokenUsage {
	return usage.Sum(parts)
}

func sameCounted(a, b domain.TokenUsage) bool {
	return a.InputTokens == b.InputTokens && a.OutputTokens == b.OutputTokens &&
		a.CacheReadTokens == b.CacheReadTokens && a.CacheWriteTokens == b.CacheWriteTokens
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
