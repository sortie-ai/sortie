package usage

import (
	"maps"

	"github.com/sortie-ai/sortie/internal/domain"
)

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
	start := Subtract(*resume, domain.TokenUsage{})
	b := &ResumeBaseline{baseline: start, latest: start, known: true}
	if IsZero(start) {
		b.modelBaseline, b.modelLatest, b.modelKnown = map[string]domain.TokenUsage{}, map[string]domain.TokenUsage{}, true
		return b
	}
	seed := NormalizeByModel(resumeByModel)
	if seed != nil && sameCounted(Sum(seed), start) {
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
	if IsZero(reading) {
		run, runByModel = b.Run()
		return run, runByModel, false
	}
	perModel := NormalizeByModel(readingByModel)
	if perModel == nil {
		perModel = map[string]domain.TokenUsage{}
	}
	if !b.known || FellBelow(reading, b.latest) {
		b.beforeReset, b.modelBeforeReset = b.Run()
		if b.modelBeforeReset == nil {
			b.modelBeforeReset = map[string]domain.TokenUsage{}
		}
		b.baseline = reading
		if own != nil {
			b.baseline = Subtract(reading, *own)
		}
		b.known = true
		estimated = !IsZero(b.baseline)
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
	run := Add(b.beforeReset, Subtract(b.latest, b.baseline))
	if !b.modelKnown {
		return run, nil
	}
	since := make(map[string]domain.TokenUsage, len(b.modelLatest))
	for name, latest := range b.modelLatest {
		since[name] = Subtract(latest, b.modelBaseline[name])
	}
	return run, NormalizeByModel(AddByModel(b.modelBeforeReset, since))
}

// anchor fixes the baseline at the latest reading so that Run returns run, and
// runByModel per model when every model it names is in the latest per-model
// reading at or above its amount; otherwise the per-model baseline is unknown.
func (b *ResumeBaseline) anchor(run domain.TokenUsage, runByModel map[string]domain.TokenUsage) {
	b.beforeReset, b.baseline, b.known = run, b.latest, true
	b.modelBeforeReset = map[string]domain.TokenUsage{}

	if !b.modelKnown {
		return
	}
	baseline := maps.Clone(b.modelLatest)
	for name, amount := range runByModel {
		latest, present := b.modelLatest[name]
		if !present || FellBelow(latest, amount) {
			b.modelBaseline, b.modelKnown = nil, false
			return
		}
		baseline[name] = Subtract(latest, amount)
	}
	b.modelBaseline = baseline
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
	if !b.modelKnown || !sameCounted(Sum(b.modelLatest), latest) {
		return &latest, nil
	}
	return &latest, maps.Clone(b.modelLatest)
}

func sameCounted(a, b domain.TokenUsage) bool {
	return a.InputTokens == b.InputTokens && a.OutputTokens == b.OutputTokens &&
		a.CacheReadTokens == b.CacheReadTokens && a.CacheWriteTokens == b.CacheWriteTokens
}
