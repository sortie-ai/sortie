package storage

import (
	"cmp"
	"slices"
	"strings"
)

// UnmeasuredCause names why a run has no token figure.
type UnmeasuredCause string

// Values of [UnmeasuredCause]. UnmeasuredMixed is the cause of an aggregate
// whose unmeasured runs disagree.
const (
	// UnmeasuredAmountsOnly means the run reported amounts, in any unit US
	// dollars included, and no token figure.
	UnmeasuredAmountsOnly UnmeasuredCause = "amounts_only"

	// UnmeasuredEndedEarly means the run ended without a usage report.
	UnmeasuredEndedEarly UnmeasuredCause = "ended_early"

	// UnmeasuredNoUsage means a run that succeeded sent no usage.
	UnmeasuredNoUsage UnmeasuredCause = "no_usage"

	// UnmeasuredUnknown means the run was recorded before provenance was kept.
	UnmeasuredUnknown UnmeasuredCause = "unknown"

	// UnmeasuredMixed means the unmeasured runs of an aggregate differ in cause
	// or in reported units.
	UnmeasuredMixed UnmeasuredCause = "mixed"
)

// UsagePair is one source and strategy that settled a run's token figure.
type UsagePair struct {
	Source   string
	Strategy string
}

// UnitAmount is the sum of the amounts reported in one unit.
type UnitAmount struct {
	Unit       string
	Amount     float64
	LowerBound bool
}

// IssueSpendProvenance is what an issue's recorded runs say about the origin
// of their figures. No slice is nil.
type IssueSpendProvenance struct {
	// UnmeasuredCause is empty when no run is unmeasured.
	UnmeasuredCause UnmeasuredCause

	// UnmeasuredUnits is empty unless UnmeasuredCause is [UnmeasuredAmountsOnly].
	UnmeasuredUnits []string

	// VendorAmounts sums, per unit other than [github.com/sortie-ai/sortie/internal/domain.UnitUSD], the amounts the
	// issue's runs reported, ascending by unit.
	VendorAmounts []UnitAmount

	// AgentPricedUSD is nil when no part was priced from an agent-reported
	// amount.
	AgentPricedUSD *float64

	// AgentPricedModels holds the models of those parts, ascending; an
	// unknown model reads as the empty string.
	AgentPricedModels []string
}

// UsageCompletenessOf classifies how far a run's token figure reaches:
// [UsageUnmeasured] when no figure arrived, [UsageLowerBound] when a turn
// spent tokens no source counted, [UsageComplete] otherwise.
func UsageCompletenessOf(measured bool, unaccountedTurns int) string {
	switch {
	case !measured:
		return UsageUnmeasured
	case unaccountedTurns > 0:
		return UsageLowerBound
	default:
		return UsageComplete
	}
}

// UnmeasuredCauseOf names why an unmeasured run has no token figure.
// recordedCompleteness is the run's stored completeness, empty for a run
// recorded before provenance was kept; reportedUnits holds every unit the run
// reported, US dollars included.
func UnmeasuredCauseOf(status, recordedCompleteness string, reportedUnits []string) UnmeasuredCause {
	switch {
	case recordedCompleteness == "":
		return UnmeasuredUnknown
	case len(reportedUnits) > 0:
		return UnmeasuredAmountsOnly
	case status != "succeeded":
		return UnmeasuredEndedEarly
	default:
		return UnmeasuredNoUsage
	}
}

// UnmeasuredCauses folds the causes of the unmeasured runs of one aggregate.
// The zero value is ready to use.
type UnmeasuredCauses struct {
	keys map[string]causeKey
}

type causeKey struct {
	cause UnmeasuredCause
	units []string
}

// Add records cause with the distinct units of one run, in any order.
func (c *UnmeasuredCauses) Add(cause UnmeasuredCause, units []string) {
	distinct := slices.Clone(units)
	slices.Sort(distinct)
	distinct = slices.Compact(distinct)
	if c.keys == nil {
		c.keys = make(map[string]causeKey)
	}
	c.keys[string(cause)+"\x00"+strings.Join(distinct, "\x00")] = causeKey{cause: cause, units: distinct}
}

// Result returns the cause every added run shares and, for
// [UnmeasuredAmountsOnly], their units. It returns an empty cause when
// nothing was added and [UnmeasuredMixed] when the runs differ. The units
// slice is never nil.
func (c *UnmeasuredCauses) Result() (UnmeasuredCause, []string) {
	switch len(c.keys) {
	case 0:
		return "", []string{}
	case 1:
		for _, key := range c.keys {
			if key.cause == UnmeasuredAmountsOnly {
				return key.cause, key.units
			}
			return key.cause, []string{}
		}
	}
	return UnmeasuredMixed, []string{}
}

// ParseUsageProvenance splits the stored provenance text into its pairs,
// the live strategy first, then by source and strategy. Empty text yields an
// empty non-nil slice.
func ParseUsageProvenance(text string) []UsagePair {
	pairs := []UsagePair{}
	if text == "" {
		return pairs
	}
	for item := range strings.SplitSeq(text, ",") {
		source, strategy, _ := strings.Cut(item, ":")
		pairs = append(pairs, UsagePair{Source: source, Strategy: strategy})
	}
	slices.SortStableFunc(pairs, func(x, y UsagePair) int {
		return cmp.Or(
			cmp.Compare(boolRank(y.Strategy == "live"), boolRank(x.Strategy == "live")),
			cmp.Compare(x.Source, y.Source),
			cmp.Compare(x.Strategy, y.Strategy),
		)
	})
	return pairs
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}
