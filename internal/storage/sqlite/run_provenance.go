package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/storage"
)

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
func (s *Store) QueryRunReportedAmounts(ctx context.Context, runIDs []int64) (map[int64][]storage.ReportedAmount, error) {
	amounts := make(map[int64][]storage.ReportedAmount)
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
		var amount storage.ReportedAmount
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
func (s *Store) IssueSpendProvenance(ctx context.Context, issueID string) (storage.IssueSpendProvenance, error) {
	out := storage.IssueSpendProvenance{
		UnmeasuredUnits:   []string{},
		VendorAmounts:     []storage.UnitAmount{},
		AgentPricedModels: []string{},
	}

	cause, units, err := s.issueUnmeasuredCause(ctx, issueID)
	if err != nil {
		return storage.IssueSpendProvenance{}, err
	}
	out.UnmeasuredCause, out.UnmeasuredUnits = cause, units

	if out.VendorAmounts, err = s.issueVendorAmounts(ctx, issueID); err != nil {
		return storage.IssueSpendProvenance{}, err
	}
	if out.AgentPricedUSD, out.AgentPricedModels, err = s.issueAgentPriced(ctx, issueID); err != nil {
		return storage.IssueSpendProvenance{}, err
	}
	return out, nil
}

func (s *Store) issueUnmeasuredCause(ctx context.Context, issueID string) (storage.UnmeasuredCause, []string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT status, usage_completeness, `+reportedUnitsOfRun+`
		FROM run_history
		WHERE issue_id = ? AND tokens_measured = 0`, issueID)
	if err != nil {
		return "", nil, fmt.Errorf("query issue unmeasured runs: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only query; close error is non-actionable

	var causes storage.UnmeasuredCauses
	for rows.Next() {
		var status string
		var completeness, unitsText sql.NullString
		if err := rows.Scan(&status, &completeness, &unitsText); err != nil {
			return "", nil, fmt.Errorf("scan issue unmeasured runs: %w", err)
		}
		units := splitReportedUnits(unitsText)
		causes.Add(storage.UnmeasuredCauseOf(status, completeness.String, units), units)
	}
	if err := rows.Err(); err != nil {
		return "", nil, fmt.Errorf("query issue unmeasured runs: %w", err)
	}
	cause, units := causes.Result()
	return cause, units, nil
}

func (s *Store) issueVendorAmounts(ctx context.Context, issueID string) ([]storage.UnitAmount, error) {
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

	vendor := []storage.UnitAmount{}
	for rows.Next() {
		var amount storage.UnitAmount
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
