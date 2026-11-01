package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/sortie-ai/sortie/internal/storage"
)

// unrecordedRunsPageSize bounds how many run_history rows one page holds in
// memory, so a long history is priced without being loaded whole.
const unrecordedRunsPageSize = 500

// RecordUnrecordedRunCosts records the cost of every run_history row whose
// cost_status is NULL, in one transaction, and returns how many rows it
// recorded. price is called once per such row in ascending id order and must
// not perform I/O: the store's single connection is held for the whole fill.
// Any error rolls the whole fill back and returns 0.
func (s *Store) RecordUnrecordedRunCosts(ctx context.Context, price func(storage.UnrecordedRun) storage.RunCostRecord) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin record run costs: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback on error path; no-op after commit

	recorded := 0
	var afterID int64
	for {
		page, err := loadUnrecordedRuns(ctx, tx, afterID)
		if err != nil {
			return 0, err
		}
		if len(page) == 0 {
			break
		}
		for _, run := range page {
			written, err := recordRunCost(ctx, tx, run.ID, price(run))
			if err != nil {
				return 0, err
			}
			if written {
				recorded++
			}
		}
		afterID = page[len(page)-1].ID
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit record run costs: %w", err)
	}
	return recorded, nil
}

// loadUnrecordedRuns returns the next page of unrecorded rows after afterID
// with their parts, and closes its statements so the caller may write.
func loadUnrecordedRuns(ctx context.Context, tx *sql.Tx, afterID int64) ([]storage.UnrecordedRun, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, agent_adapter, tokens_measured, input_tokens, output_tokens, total_tokens,
			cache_read_tokens, cache_write_tokens, unaccounted_turns, configured_model, reported_model
		FROM run_history
		WHERE cost_status IS NULL AND id > ?
		ORDER BY id
		LIMIT ?`, afterID, unrecordedRunsPageSize)
	if err != nil {
		return nil, fmt.Errorf("query unrecorded run costs: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only query; close error is non-actionable

	var page []storage.UnrecordedRun
	byID := map[int64]*storage.UnrecordedRun{}
	for rows.Next() {
		var run storage.UnrecordedRun
		if err := rows.Scan(&run.ID, &run.AgentAdapter, &run.TokensMeasured, &run.InputTokens, &run.OutputTokens,
			&run.TotalTokens, &run.CacheReadTokens, &run.CacheWriteTokens, &run.UnaccountedTurns,
			&run.ConfiguredModel, &run.ReportedModel); err != nil {
			return nil, fmt.Errorf("scan unrecorded run cost: %w", err)
		}
		page = append(page, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query unrecorded run costs: %w", err)
	}
	for i := range page {
		byID[page[i].ID] = &page[i]
	}

	// The subquery repeats the page selection above, which stays stable
	// because nothing is written until both reads finish.
	parts, err := tx.QueryContext(ctx,
		`SELECT run_id, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens
		FROM run_model_usage
		WHERE run_id IN (
			SELECT id FROM run_history WHERE cost_status IS NULL AND id > ? ORDER BY id LIMIT ?
		)`, afterID, unrecordedRunsPageSize)
	if err != nil {
		return nil, fmt.Errorf("query unrecorded run parts: %w", err)
	}
	defer parts.Close() //nolint:errcheck // read-only query; close error is non-actionable

	for parts.Next() {
		var runID int64
		var model string
		var tokens storage.ModelTokens
		if err := parts.Scan(&runID, &model, &tokens.InputTokens, &tokens.OutputTokens,
			&tokens.CacheReadTokens, &tokens.CacheWriteTokens); err != nil {
			return nil, fmt.Errorf("scan unrecorded run part: %w", err)
		}
		run, ok := byID[runID]
		if !ok {
			continue
		}
		if run.Parts == nil {
			run.Parts = map[string]storage.ModelTokens{}
		}
		run.Parts[model] = tokens
	}
	if err := parts.Err(); err != nil {
		return nil, fmt.Errorf("query unrecorded run parts: %w", err)
	}
	return page, nil
}

// recordRunCost writes record onto run runID and its parts, and reports false
// when the row no longer has a NULL cost_status.
func recordRunCost(ctx context.Context, tx *sql.Tx, runID int64, record storage.RunCostRecord) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE run_history SET estimated_cost_usd = ?, cost_status = ? WHERE id = ? AND cost_status IS NULL`,
		nullFloat(record.EstimatedCostUSD), nullIfEmpty(record.CostStatus), runID)
	if err != nil {
		return false, fmt.Errorf("record cost of run %d: %w", runID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("record cost of run %d: %w", runID, err)
	}
	if affected == 0 {
		return false, nil
	}

	if err := insertRunModelUsage(ctx, tx, runID, record.NewParts, record.PartCosts); err != nil {
		return false, err
	}
	for model, cost := range record.PartCosts {
		if _, isNew := record.NewParts[model]; isNew {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE run_model_usage
			SET estimated_cost_usd = ?, cost_status = ?, price_source = ?, price_table_version = ?, unrated_classes = ?
			WHERE run_id = ? AND model = ? AND cost_status IS NULL`,
			nullFloat(cost.EstimatedCostUSD), nullIfEmpty(cost.CostStatus), cost.PriceSource, cost.PriceTableVersion,
			unratedColumn(cost), runID, model); err != nil {
			return false, fmt.Errorf("record cost of run %d model %q: %w", runID, model, err)
		}
	}
	return true, nil
}
