package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/sortie-ai/sortie/internal/storage"
)

var _ storage.RunTurnStore = (*Store)(nil)

// turnEndedAtLayout is fixed-width, with nine fractional digits, so the text
// order of ended_at is the time order and range bounds compare as text.
const turnEndedAtLayout = "2006-01-02T15:04:05.000000000Z07:00"

// insertRunTurns inserts one run_turns row per turn and one run_turn_models
// row per model of a turn, for runID.
func insertRunTurns(ctx context.Context, ex statementExecer, runID int64, turns []storage.RunTurn) error {
	for _, turn := range turns {
		priced := turn.EstimatedCostUSD != nil
		if _, err := ex.ExecContext(ctx,
			`INSERT INTO run_turns
				(run_id, seq, ended_at, tokens_measured, tool_calls, errors,
				 estimated_cost_usd, input_cost_usd, output_cost_usd, cache_read_cost_usd, cache_write_cost_usd)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			runID, turn.Seq, turn.EndedAt.UTC().Format(turnEndedAtLayout), turn.TokensMeasured, turn.ToolCalls, turn.Errors,
			nullFloat(turn.EstimatedCostUSD),
			classCost(priced, turn.InputCostUSD), classCost(priced, turn.OutputCostUSD),
			classCost(priced, turn.CacheReadCostUSD), classCost(priced, turn.CacheWriteCostUSD),
		); err != nil {
			return fmt.Errorf("append run turn for run %d seq %d: %w", runID, turn.Seq, err)
		}
		for model, tokens := range turn.Models {
			if _, err := ex.ExecContext(ctx,
				`INSERT INTO run_turn_models
					(run_id, seq, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens)
				VALUES (?, ?, ?, ?, ?, ?, ?)`,
				runID, turn.Seq, model, tokens.InputTokens, tokens.OutputTokens, tokens.CacheReadTokens, tokens.CacheWriteTokens,
			); err != nil {
				return fmt.Errorf("append run turn model for run %d seq %d model %q: %w", runID, turn.Seq, model, err)
			}
		}
	}
	return nil
}

// classCost is the value of a class cost column: NULL when the turn has no
// cost figure.
func classCost(priced bool, v float64) sql.NullFloat64 {
	return sql.NullFloat64{Float64: v, Valid: priced}
}

// ScanRunTurnsRange visits the stored turn records whose ended_at is at or
// after since and before until, ascending by ended_at, run ID, and seq. It
// issues one SELECT that joins each turn's model rows, so a turn and its
// models come from one snapshot.
//
// visit MUST NOT call any Store method. The scan holds the store's single
// pooled connection for the duration of iteration, because OpenReadOnly caps
// the pool at one connection.
func (s *Store) ScanRunTurnsRange(ctx context.Context, since, until *time.Time, visit func(storage.RunTurnRow) error) error {
	query := `SELECT run_turns.run_id, run_turns.seq, run_turns.ended_at, run_turns.tokens_measured,
		run_turns.tool_calls, run_turns.errors, run_turns.estimated_cost_usd, run_turns.input_cost_usd,
		run_turns.output_cost_usd, run_turns.cache_read_cost_usd, run_turns.cache_write_cost_usd,
		run_history.issue_id, run_history.identifier, run_history.display_identifier, run_history.issue_title,
		run_history.dispatch_id, run_history.rule_name, run_history.agent_adapter, run_history.status,
		run_history.cost_status, run_history.usage_completeness,
		run_turn_models.model, run_turn_models.input_tokens, run_turn_models.output_tokens,
		run_turn_models.cache_read_tokens, run_turn_models.cache_write_tokens
	FROM run_turns
	JOIN run_history ON run_history.id = run_turns.run_id
	LEFT JOIN run_turn_models ON run_turn_models.run_id = run_turns.run_id AND run_turn_models.seq = run_turns.seq`

	var args []any
	where := ""
	if since != nil {
		where += " WHERE run_turns.ended_at >= ?"
		args = append(args, since.UTC().Format(turnEndedAtLayout))
	}
	if until != nil {
		if where == "" {
			where += " WHERE run_turns.ended_at < ?"
		} else {
			where += " AND run_turns.ended_at < ?"
		}
		args = append(args, until.UTC().Format(turnEndedAtLayout))
	}
	query += where + " ORDER BY run_turns.ended_at, run_turns.run_id, run_turns.seq, run_turn_models.model"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("scan run turns range: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only query; close error is non-actionable

	var pending *storage.RunTurnRow
	for rows.Next() {
		var row storage.RunTurnRow
		var endedAt string
		var cost, inputCost, outputCost, cacheReadCost, cacheWriteCost sql.NullFloat64
		var displayID, dispatchID, costStatus, completeness, model sql.NullString
		var modelInput, modelOutput, modelCacheRead, modelCacheWrite sql.NullInt64
		if err := rows.Scan(
			&row.RunID, &row.Turn.Seq, &endedAt, &row.Turn.TokensMeasured,
			&row.Turn.ToolCalls, &row.Turn.Errors, &cost, &inputCost,
			&outputCost, &cacheReadCost, &cacheWriteCost,
			&row.IssueID, &row.Identifier, &displayID, &row.IssueTitle,
			&dispatchID, &row.RuleName, &row.AgentAdapter, &row.Status,
			&costStatus, &completeness,
			&model, &modelInput, &modelOutput, &modelCacheRead, &modelCacheWrite,
		); err != nil {
			return fmt.Errorf("scan run turns range: %w", err)
		}

		if pending == nil || pending.RunID != row.RunID || pending.Turn.Seq != row.Turn.Seq {
			if pending != nil {
				if err := visit(*pending); err != nil {
					return err
				}
			}
			ended, err := time.Parse(turnEndedAtLayout, endedAt)
			if err != nil {
				return fmt.Errorf("scan run turns range: parse ended_at of run %d seq %d: %w", row.RunID, row.Turn.Seq, err)
			}
			row.Turn.EndedAt = ended.UTC()
			row.DisplayID = displayID.String
			row.DispatchID = dispatchID.String
			row.CostStatus = costStatus.String
			row.UsageCompleteness = completeness.String
			if cost.Valid {
				row.Turn.EstimatedCostUSD = new(cost.Float64)
				row.Turn.InputCostUSD = inputCost.Float64
				row.Turn.OutputCostUSD = outputCost.Float64
				row.Turn.CacheReadCostUSD = cacheReadCost.Float64
				row.Turn.CacheWriteCostUSD = cacheWriteCost.Float64
			}
			pending = &row
		}
		if model.Valid {
			if pending.Turn.Models == nil {
				pending.Turn.Models = make(map[string]storage.ModelTokens)
			}
			pending.Turn.Models[model.String] = storage.ModelTokens{
				InputTokens:      modelInput.Int64,
				OutputTokens:     modelOutput.Int64,
				CacheReadTokens:  modelCacheRead.Int64,
				CacheWriteTokens: modelCacheWrite.Int64,
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("scan run turns range: %w", err)
	}
	if pending != nil {
		return visit(*pending)
	}
	return nil
}
