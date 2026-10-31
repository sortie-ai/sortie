package persistence

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
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

	// VendorAmounts sums, per unit other than [domain.UnitUSD], the amounts the
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

// reportedUnitsOfRun is the scalar subquery that yields the distinct units
// run_history.id reported as one text value, so a read never joins the amounts
// and repeats the run's row.
const reportedUnitsOfRun = "(SELECT group_concat(unit, char(31)) FROM (SELECT DISTINCT unit FROM " +
	"run_reported_amounts WHERE run_id = run_history.id ORDER BY unit))"

// splitReportedUnits parses the value of [reportedUnitsOfRun]; NULL reads as
// no units.
func splitReportedUnits(text sql.NullString) []string {
	if text.String == "" {
		return nil
	}
	units := strings.Split(text.String, "\x1f")
	slices.Sort(units)
	return units
}

// QueryRunReportedAmounts returns the run_reported_amounts rows of runIDs keyed
// by run ID, each run's rows ordered by unit then model; a run without rows has
// no key. It holds one statement and closes it before returning.
func (s *Store) QueryRunReportedAmounts(ctx context.Context, runIDs []int64) (map[int64][]ReportedAmount, error) {
	amounts := make(map[int64][]ReportedAmount)
	if len(runIDs) == 0 {
		return amounts, nil
	}

	args := make([]any, len(runIDs))
	for i, id := range runIDs {
		args[i] = id
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(runIDs)), ",")
	query := fmt.Sprintf( //nolint:gosec // placeholders is "?,?,..." built from len(runIDs); values remain bound parameters
		`SELECT run_id, unit, model, amount, lower_bound
		FROM run_reported_amounts
		WHERE run_id IN (%s)
		ORDER BY run_id, unit, model`, placeholders)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query run reported amounts: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only query; close error is non-actionable

	for rows.Next() {
		var runID int64
		var amount ReportedAmount
		if err := rows.Scan(&runID, &amount.Unit, &amount.Model, &amount.Amount, &amount.LowerBound); err != nil {
			return nil, fmt.Errorf("scan run reported amounts: %w", err)
		}
		amounts[runID] = append(amounts[runID], amount)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query run reported amounts: %w", err)
	}
	return amounts, nil
}

// IssueSpendProvenance reads, from the recorded rows of every run of
// issueID, the cause of each missing token figure, the amounts reported in
// vendor units, and the share priced from an agent-reported amount. It runs a
// fixed number of statements, each keyed by issue_id, and recomputes no
// recorded token or cost figure.
func (s *Store) IssueSpendProvenance(ctx context.Context, issueID string) (IssueSpendProvenance, error) {
	out := IssueSpendProvenance{
		UnmeasuredUnits:   []string{},
		VendorAmounts:     []UnitAmount{},
		AgentPricedModels: []string{},
	}

	cause, units, err := s.issueUnmeasuredCause(ctx, issueID)
	if err != nil {
		return IssueSpendProvenance{}, err
	}
	out.UnmeasuredCause, out.UnmeasuredUnits = cause, units

	if out.VendorAmounts, err = s.issueVendorAmounts(ctx, issueID); err != nil {
		return IssueSpendProvenance{}, err
	}
	if out.AgentPricedUSD, out.AgentPricedModels, err = s.issueAgentPriced(ctx, issueID); err != nil {
		return IssueSpendProvenance{}, err
	}
	return out, nil
}

func (s *Store) issueUnmeasuredCause(ctx context.Context, issueID string) (UnmeasuredCause, []string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT status, usage_completeness, `+reportedUnitsOfRun+`
		FROM run_history
		WHERE issue_id = ? AND tokens_measured = 0`, issueID)
	if err != nil {
		return "", nil, fmt.Errorf("query issue unmeasured runs: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only query; close error is non-actionable

	var causes UnmeasuredCauses
	for rows.Next() {
		var status string
		var completeness, unitsText sql.NullString
		if err := rows.Scan(&status, &completeness, &unitsText); err != nil {
			return "", nil, fmt.Errorf("scan issue unmeasured runs: %w", err)
		}
		units := splitReportedUnits(unitsText)
		causes.Add(UnmeasuredCauseOf(status, completeness.String, units), units)
	}
	if err := rows.Err(); err != nil {
		return "", nil, fmt.Errorf("query issue unmeasured runs: %w", err)
	}
	cause, units := causes.Result()
	return cause, units, nil
}

func (s *Store) issueVendorAmounts(ctx context.Context, issueID string) ([]UnitAmount, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT amounts.unit, SUM(amounts.amount), MAX(amounts.lower_bound)
		FROM run_history
		JOIN run_reported_amounts AS amounts ON amounts.run_id = run_history.id
		WHERE run_history.issue_id = ? AND amounts.unit <> ?
		GROUP BY amounts.unit
		ORDER BY amounts.unit`, issueID, domain.UnitUSD)
	if err != nil {
		return nil, fmt.Errorf("query issue vendor amounts: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only query; close error is non-actionable

	vendor := []UnitAmount{}
	for rows.Next() {
		var amount UnitAmount
		if err := rows.Scan(&amount.Unit, &amount.Amount, &amount.LowerBound); err != nil {
			return nil, fmt.Errorf("scan issue vendor amounts: %w", err)
		}
		vendor = append(vendor, amount)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query issue vendor amounts: %w", err)
	}
	return vendor, nil
}

func (s *Store) issueAgentPriced(ctx context.Context, issueID string) (*float64, []string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT parts.model, parts.estimated_cost_usd
		FROM run_history
		JOIN run_model_usage AS parts ON parts.run_id = run_history.id
		WHERE run_history.issue_id = ? AND parts.price_source = 'agent'
		ORDER BY parts.model`, issueID)
	if err != nil {
		return nil, nil, fmt.Errorf("query issue agent-priced parts: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only query; close error is non-actionable

	var total *float64
	models := []string{}
	for rows.Next() {
		var model string
		var cost sql.NullFloat64
		if err := rows.Scan(&model, &cost); err != nil {
			return nil, nil, fmt.Errorf("scan issue agent-priced parts: %w", err)
		}
		if total == nil {
			total = new(float64)
		}
		*total += cost.Float64
		models = append(models, model)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("query issue agent-priced parts: %w", err)
	}
	return total, slices.Compact(models), nil
}
