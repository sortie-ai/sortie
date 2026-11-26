package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/storage"
)

var _ storage.RunTurnStore = (*Store)(nil)

// turnEndedAtLayout is fixed-width, with nine fractional digits, so the text
// order of ended_at is the time order and range bounds compare as text.
const turnEndedAtLayout = "2006-01-02T15:04:05.000000000Z07:00"

// runTurnsMigration is the version of the migration that creates run_turns.
const runTurnsMigration = 54

// agentPricedOfRun is a scalar subquery over the current run_history row: the
// recorded cost of the run's parts priced from an agent-reported amount, 0 for
// such parts without a cost, and NULL when no part has that source. A scalar
// subquery leaves the row count of the outer join alone.
const agentPricedOfRun = `(SELECT CASE WHEN COUNT(*) > 0 THEN COALESCE(SUM(parts.estimated_cost_usd), 0) END
		FROM run_model_usage parts
		WHERE parts.run_id = run_history.id AND parts.price_source = 'agent')`

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
		run_history.estimated_cost_usd, ` + agentPricedOfRun + `,
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
		var cost, inputCost, outputCost, cacheReadCost, cacheWriteCost, runCost, runAgentPriced sql.NullFloat64
		var displayID, dispatchID, costStatus, completeness, model sql.NullString
		var modelInput, modelOutput, modelCacheRead, modelCacheWrite sql.NullInt64
		if err := rows.Scan(
			&row.RunID, &row.Turn.Seq, &endedAt, &row.Turn.TokensMeasured,
			&row.Turn.ToolCalls, &row.Turn.Errors, &cost, &inputCost,
			&outputCost, &cacheReadCost, &cacheWriteCost,
			&row.IssueID, &row.Identifier, &displayID, &row.IssueTitle,
			&dispatchID, &row.RuleName, &row.AgentAdapter, &row.Status,
			&costStatus, &completeness, &runCost, &runAgentPriced,
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
			if runCost.Valid {
				row.RunCostUSD = new(runCost.Float64)
			}
			if runAgentPriced.Valid {
				row.RunAgentPricedUSD = new(runAgentPriced.Float64)
			}
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

// runTurnsTrackedSince returns the time the migration creating run_turns was
// applied, in whole seconds. A run completed before it has no turn records.
func (s *Store) runTurnsTrackedSince(ctx context.Context) (time.Time, error) {
	var appliedAt string
	if err := s.db.QueryRowContext(ctx,
		`SELECT applied_at FROM schema_migrations WHERE version = ?`,
		runTurnsMigration,
	).Scan(&appliedAt); err != nil {
		return time.Time{}, fmt.Errorf("read run turns start: %w", err)
	}
	t, err := time.Parse(time.RFC3339, appliedAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse run turns start %q: %w", appliedAt, err)
	}
	return t, nil
}

// ScanUntrackedRunsRange visits the finished runs the store holds no turn
// records for because they were recorded before it kept any, ascending by
// completed_at and run ID. A row is visited when its completed_at lies in
// [since, until), its status is not ci_failed, and either the database has no
// run_turns table or the row has no run_turns row and completed before the
// migration that created the table was applied. caps selects the projection as
// [Store.ScanRunHistoryRange] does.
//
// Bounds compare as instants, so a stored completed_at in any UTC offset or
// precision orders correctly. visit MUST NOT call any Store method; the scan
// holds the store's single pooled connection for the duration of iteration.
func (s *Store) ScanUntrackedRunsRange(
	ctx context.Context,
	caps storage.RunHistoryCapabilities,
	since, until *time.Time,
	visit func(storage.UntrackedRunRow) error,
) error {
	// The row query below holds the store's only connection until it is
	// closed, so this read must finish first.
	var trackedSince time.Time
	if caps.HasRunTurns {
		var err error
		if trackedSince, err = s.runTurnsTrackedSince(ctx); err != nil {
			return err
		}
	}

	full := caps.Full()
	recordedCost := caps.RecordedCost()
	query := `SELECT run_history.id, run_history.issue_id, run_history.identifier, run_history.status,
		run_history.agent_adapter, run_history.completed_at`
	if full {
		query += ", run_history.rule_name, run_history.tokens_measured, run_history.input_tokens," +
			" run_history.output_tokens, run_history.total_tokens, run_history.cache_read_tokens"
		if caps.HasCacheWriteTokens {
			query += ", run_history.cache_write_tokens"
		}
	}
	// These columns predate the cost columns, so a database that records cost carries them.
	if recordedCost {
		query += ", run_history.display_identifier, run_history.issue_title, run_history.dispatch_id," +
			" run_history.estimated_cost_usd, run_history.cost_status, run_history.unaccounted_turns, " + agentPricedOfRun
	}
	query += "\nFROM run_history"

	conds := []string{"run_history.status <> 'ci_failed'"}
	var args []any
	if since != nil {
		conds = append(conds, "unixepoch(run_history.completed_at, 'subsec') >= unixepoch(?, 'subsec')")
		args = append(args, since.UTC().Format(time.RFC3339Nano))
	}
	if until != nil {
		conds = append(conds, "unixepoch(run_history.completed_at, 'subsec') < unixepoch(?, 'subsec')")
		args = append(args, until.UTC().Format(time.RFC3339Nano))
	}
	if caps.HasRunTurns {
		// applied_at drops the fraction of its second, so a run that finished
		// in that second, before the migration ran, still counts as untracked.
		conds = append(conds,
			"NOT EXISTS (SELECT 1 FROM run_turns WHERE run_turns.run_id = run_history.id)",
			"unixepoch(run_history.completed_at, 'subsec') < unixepoch(?, 'subsec')")
		args = append(args, trackedSince.Add(time.Second).Format(time.RFC3339))
	}
	query += " WHERE " + strings.Join(conds, " AND ") + //nolint:gosec // conds hold only constant predicates; values remain bound parameters
		" ORDER BY unixepoch(run_history.completed_at, 'subsec'), run_history.id"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("scan untracked runs range: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only query; close error is non-actionable

	for rows.Next() {
		var row storage.UntrackedRunRow
		var completedAt string
		var displayID, dispatchID, costStatus sql.NullString
		var cost, agentPriced sql.NullFloat64
		var issueTitle sql.NullString
		dest := []any{&row.RunID, &row.IssueID, &row.Identifier, &row.Status, &row.AgentAdapter, &completedAt}
		if full {
			dest = append(dest, &row.RuleName, &row.TokensMeasured, &row.InputTokens,
				&row.OutputTokens, &row.TotalTokens, &row.CacheReadTokens)
			if caps.HasCacheWriteTokens {
				dest = append(dest, &row.CacheWriteTokens)
			}
		}
		if recordedCost {
			dest = append(dest, &displayID, &issueTitle, &dispatchID, &cost, &costStatus, &row.UnaccountedTurns, &agentPriced)
		}
		if err := rows.Scan(dest...); err != nil {
			return fmt.Errorf("scan untracked runs range: %w", err)
		}

		completed, err := time.Parse(time.RFC3339, completedAt)
		if err != nil {
			// A run without a readable completion instant belongs to no bucket.
			continue
		}
		row.CompletedAt = completed.UTC()
		row.DisplayID = displayID.String
		row.IssueTitle = issueTitle.String
		row.DispatchID = dispatchID.String
		row.CostStatus = costStatus.String
		if cost.Valid {
			row.EstimatedCostUSD = new(cost.Float64)
		}
		if agentPriced.Valid {
			row.AgentPricedUSD = new(agentPriced.Float64)
		}
		if err := visit(row); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("scan untracked runs range: %w", err)
	}
	return nil
}
