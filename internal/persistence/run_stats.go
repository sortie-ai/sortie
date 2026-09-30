package persistence

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// RunHistoryCapabilities reports which optional run_history columns a
// database carries, so a read-only caller can pick a projection the schema
// supports. A field the underlying table does not carry holds its zero
// value in every [RunStatsRow] the read returns.
type RunHistoryCapabilities struct {
	HasTurnsCompleted   bool // migration 005
	HasReviewMetadata   bool // migration 007
	HasRuleRouting      bool // migration 010: rule_name, template_id
	HasTokens           bool // migration 011: the four token columns
	HasTokenMeasurement bool // migration 012: tokens_measured
	HasCacheWriteTokens bool // migration 020

	// HasConfiguredSettings is outside [RunHistoryCapabilities.Full];
	// [RunHistoryCapabilities.ModelBreakdown] depends on it.
	HasConfiguredSettings bool
}

// Full reports whether the database carries every optional run_history
// column group. It is the single owner of the capability tier decision:
// [Store.ScanRunHistoryRange] selects its projection from this predicate and
// no caller may re-derive the conjunction or branch on an individual flag,
// because a database migrated only partway is the common case rather than
// an exceptional one.
func (c RunHistoryCapabilities) Full() bool {
	return c.HasTurnsCompleted && c.HasReviewMetadata && c.HasRuleRouting && c.HasTokens && c.HasTokenMeasurement
}

// ModelBreakdown reports whether a read can group runs by configured
// model: Full plus the configured-settings columns.
func (c RunHistoryCapabilities) ModelBreakdown() bool {
	return c.Full() && c.HasConfiguredSettings
}

// RunStatsRow is the narrow run_history projection the aggregate read
// returns. A field the schema does not carry holds its zero value.
type RunStatsRow struct {
	Status           string
	AgentAdapter     string
	RuleName         string
	TemplateID       string
	StartedAt        string // ISO-8601 as stored
	CompletedAt      string // ISO-8601 as stored
	TurnsCompleted   int
	ReviewMetadata   *string // nil when the column is NULL or absent
	InputTokens      int64
	OutputTokens     int64
	TotalTokens      int64
	CacheReadTokens  int64
	CacheWriteTokens int64 // 0 when the database lacks the column
	TokensMeasured   bool

	// ConfiguredModel is empty when the run named none or
	// [RunHistoryCapabilities.ModelBreakdown] reports false.
	ConfiguredModel string
}

// runStatsColumnsFull is the column list when Full reports true and
// HasCacheWriteTokens false.
const runStatsColumnsFull = `SELECT status, agent_adapter, rule_name, template_id, started_at, completed_at,
	turns_completed, review_metadata, input_tokens, output_tokens, total_tokens, cache_read_tokens, tokens_measured`

// runStatsColumnsFullWithCacheWrite is the column list when Full and
// HasCacheWriteTokens both report true.
const runStatsColumnsFullWithCacheWrite = `SELECT status, agent_adapter, rule_name, template_id, started_at, completed_at,
	turns_completed, review_metadata, input_tokens, output_tokens, total_tokens, cache_read_tokens, cache_write_tokens, tokens_measured`

// runStatsColumnsBase is the column list every schema version carries,
// used when Full reports false.
const runStatsColumnsBase = `SELECT status, agent_adapter, started_at, completed_at`

// RunHistoryCapabilities reports which optional run_history column groups
// this store's database carries, by reading the table's live column set
// with PRAGMA table_info rather than assuming a fixed schema version.
//
// RunHistoryCapabilities returns a non-nil error, naming run_history, when
// the table is absent or lacks any of the base columns status,
// agent_adapter, started_at, or completed_at, so the caller can report that
// the file is not a Sortie database without inspecting the error further.
func (s *Store) RunHistoryCapabilities(ctx context.Context) (RunHistoryCapabilities, error) {
	rows, err := s.db.QueryContext(ctx, "PRAGMA table_info(run_history)")
	if err != nil {
		return RunHistoryCapabilities{}, fmt.Errorf("read run_history schema: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only pragma; close error is non-actionable

	columns := make(map[string]bool)
	for rows.Next() {
		var cid, pk int
		var name, colType string
		var notNull int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			return RunHistoryCapabilities{}, fmt.Errorf("scan run_history schema: %w", err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		return RunHistoryCapabilities{}, fmt.Errorf("read run_history schema: %w", err)
	}

	if len(columns) == 0 || !columns["status"] || !columns["agent_adapter"] ||
		!columns["started_at"] || !columns["completed_at"] {
		return RunHistoryCapabilities{}, fmt.Errorf("run_history table not found or missing base columns")
	}

	return RunHistoryCapabilities{
		HasTurnsCompleted: columns["turns_completed"],
		HasReviewMetadata: columns["review_metadata"],
		HasRuleRouting:    columns["rule_name"] && columns["template_id"],
		HasTokens: columns["input_tokens"] && columns["output_tokens"] &&
			columns["total_tokens"] && columns["cache_read_tokens"],
		HasTokenMeasurement: columns["tokens_measured"],
		HasCacheWriteTokens: columns["cache_write_tokens"],
		HasConfiguredSettings: columns["configured_model"] && columns["configured_effort"] &&
			columns["reported_model"],
	}, nil
}

// ScanRunHistoryRange issues one SELECT over run_history bounded by a
// half-open completed_at range and calls visit once per row in ascending
// id order. caps selects the projection through [RunHistoryCapabilities.Full];
// a nil since or until omits its predicate rather than substituting a
// sentinel. since is compared with >= and until with <, both formatted as
// since.UTC().Format(time.RFC3339). A non-nil error returned by visit
// aborts iteration and is returned unmodified, so the caller can compare it
// with errors.Is.
//
// visit MUST NOT call any Store method. The scan holds the store's single
// pooled connection for the duration of iteration, because OpenReadOnly
// caps the pool at one connection; a second query issued from visit would
// block until the context is cancelled, since only this scan's completion
// can release that connection.
func (s *Store) ScanRunHistoryRange(
	ctx context.Context,
	caps RunHistoryCapabilities,
	since, until *time.Time,
	visit func(RunStatsRow) error,
) error {
	full := caps.Full()
	breakdown := caps.ModelBreakdown()
	query := runStatsColumnsBase
	switch {
	case full && caps.HasCacheWriteTokens:
		query = runStatsColumnsFullWithCacheWrite
	case full:
		query = runStatsColumnsFull
	}
	if breakdown {
		query += ", configured_model"
	}
	query += "\nFROM run_history"

	var args []any
	where := ""
	if since != nil {
		where += " WHERE completed_at >= ?"
		args = append(args, since.UTC().Format(time.RFC3339))
	}
	if until != nil {
		if where == "" {
			where += " WHERE completed_at < ?"
		} else {
			where += " AND completed_at < ?"
		}
		args = append(args, until.UTC().Format(time.RFC3339))
	}
	query += where + " ORDER BY id ASC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("scan run history range: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only query; close error is non-actionable

	for rows.Next() {
		var row RunStatsRow
		var reviewMeta sql.NullString
		var dest []any
		switch {
		case full && caps.HasCacheWriteTokens:
			dest = []any{
				&row.Status, &row.AgentAdapter, &row.RuleName, &row.TemplateID,
				&row.StartedAt, &row.CompletedAt, &row.TurnsCompleted, &reviewMeta,
				&row.InputTokens, &row.OutputTokens, &row.TotalTokens, &row.CacheReadTokens, &row.CacheWriteTokens,
				&row.TokensMeasured,
			}
		case full:
			dest = []any{
				&row.Status, &row.AgentAdapter, &row.RuleName, &row.TemplateID,
				&row.StartedAt, &row.CompletedAt, &row.TurnsCompleted, &reviewMeta,
				&row.InputTokens, &row.OutputTokens, &row.TotalTokens, &row.CacheReadTokens, &row.TokensMeasured,
			}
		default:
			dest = []any{&row.Status, &row.AgentAdapter, &row.StartedAt, &row.CompletedAt}
		}
		if breakdown {
			dest = append(dest, &row.ConfiguredModel)
		}
		if err := rows.Scan(dest...); err != nil {
			return fmt.Errorf("scan run history range: %w", err)
		}
		if reviewMeta.Valid {
			row.ReviewMetadata = new(reviewMeta.String)
		}

		if err := visit(row); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("scan run history range: %w", err)
	}
	return nil
}
