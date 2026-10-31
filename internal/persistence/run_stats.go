package persistence

import (
	"context"
	"database/sql"
	"errors"
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

	// HasStageChains is outside [RunHistoryCapabilities.Full];
	// [RunHistoryCapabilities.ChainBreakdown] depends on it.
	HasStageChains bool

	// HasTrigger is outside [RunHistoryCapabilities.Full];
	// [RunHistoryCapabilities.TriggerBreakdown] depends on it, and
	// [RunHistoryCapabilities.OriginBreakdown] through it.
	HasTrigger bool // migration 028: trigger, reaction_kind

	// HasFirstAfterHop is outside [RunHistoryCapabilities.Full];
	// [RunHistoryCapabilities.OriginBreakdown] depends on it.
	HasFirstAfterHop bool // migration 032: first_after_hop

	// HasAgentBlocked is outside [RunHistoryCapabilities.Full];
	// [RunHistoryCapabilities.FunnelBreakdown] depends on it.
	HasAgentBlocked bool // migration 037: agent_blocked

	// HasRecordedCost is outside [RunHistoryCapabilities.Full];
	// [RunHistoryCapabilities.RecordedCost] depends on it.
	HasRecordedCost bool

	// HasSkills is outside [RunHistoryCapabilities.Full];
	// [RunHistoryCapabilities.SkillBreakdown] depends on it.
	HasSkills bool

	// HasUsageProvenance is outside [RunHistoryCapabilities.Full];
	// [RunHistoryCapabilities.UsageProvenance] depends on it. It is true when
	// run_history carries usage_completeness and usage_provenance and the
	// run_reported_amounts table exists.
	HasUsageProvenance bool // migration 050
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

// ModelBreakdown reports whether a read can group runs by model: Full plus
// the configured-settings columns.
func (c RunHistoryCapabilities) ModelBreakdown() bool {
	return c.Full() && c.HasConfiguredSettings
}

// ChainBreakdown reports whether a read can group runs by stage chain:
// ModelBreakdown plus the stage-chain columns.
func (c RunHistoryCapabilities) ChainBreakdown() bool {
	return c.ModelBreakdown() && c.HasStageChains
}

// TriggerBreakdown reports whether a read can group runs by retry trigger:
// ChainBreakdown plus the trigger column.
func (c RunHistoryCapabilities) TriggerBreakdown() bool {
	return c.ChainBreakdown() && c.HasTrigger
}

// OriginBreakdown reports whether a read can group runs by run origin:
// TriggerBreakdown plus the first-run-after-hop column.
func (c RunHistoryCapabilities) OriginBreakdown() bool {
	return c.TriggerBreakdown() && c.HasFirstAfterHop
}

// FunnelBreakdown reports whether a read can fold runs into stage funnels:
// OriginBreakdown plus the blocked-run column.
func (c RunHistoryCapabilities) FunnelBreakdown() bool {
	return c.OriginBreakdown() && c.HasAgentBlocked
}

// RecordedCost reports whether the stats report can read the recorded cost.
// It requires FunnelBreakdown too, so a partly migrated database reports no
// cost rather than a partial one.
func (c RunHistoryCapabilities) RecordedCost() bool {
	return c.FunnelBreakdown() && c.HasRecordedCost
}

// SkillBreakdown reports whether a read can fold each run's skill calls:
// RecordedCost plus the skills column.
func (c RunHistoryCapabilities) SkillBreakdown() bool {
	return c.RecordedCost() && c.HasSkills
}

// UsageProvenance reports whether the stats report can read each run's usage
// completeness, provenance, and reported units: RecordedCost plus the
// provenance columns.
func (c RunHistoryCapabilities) UsageProvenance() bool {
	return c.RecordedCost() && c.HasUsageProvenance
}

// RunStatsRow is the narrow run_history projection the aggregate read
// returns. A field the schema does not carry holds its zero value.
type RunStatsRow struct {
	// IssueID is filled on every capability tier.
	IssueID string

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

	// ConfiguredModel and ReportedModel are empty when the run named or
	// reported none, or [RunHistoryCapabilities.ModelBreakdown] reports false.
	ConfiguredModel string
	ReportedModel   string

	// Each is empty unless [RunHistoryCapabilities.ChainBreakdown] reports
	// true.
	Identifier    string
	DisplayID     string
	ChainID       string
	StagePrevious string
	StageResult   string

	// Trigger is nil when the column is NULL or
	// [RunHistoryCapabilities.TriggerBreakdown] reports false.
	Trigger *string

	// ReactionKind is empty when the column is NULL or empty, or
	// [RunHistoryCapabilities.TriggerBreakdown] reports false.
	ReactionKind string

	// FirstAfterHop is false unless [RunHistoryCapabilities.OriginBreakdown]
	// reports true.
	FirstAfterHop bool

	// Each is false unless [RunHistoryCapabilities.FunnelBreakdown] reports
	// true. HandoffWithheld marks a failed run whose error begins with
	// [HandoffAbsenceErrorPrefix].
	AgentBlocked    bool
	HandoffWithheld bool

	// EstimatedCostUSD is nil when the column is NULL or
	// [RunHistoryCapabilities.RecordedCost] reports false. CostStatus is empty
	// for a NULL status and under the same condition.
	EstimatedCostUSD *float64
	CostStatus       string

	// Parts are the run's run_model_usage rows ordered by model, carrying
	// Model, Tokens, EstimatedCostUSD, CostStatus, and PriceSource;
	// UnratedClasses and UnratedRecorded stay zero. Nil when the run has none
	// or [RunHistoryCapabilities.RecordedCost] reports false.
	Parts []RunModelPart

	// UnaccountedTurns is run_history.unaccounted_turns; zero when
	// [RunHistoryCapabilities.RecordedCost] reports false.
	UnaccountedTurns int

	// Skills is run_history.skills as stored; nil when the column is NULL or
	// [RunHistoryCapabilities.SkillBreakdown] reports false.
	Skills *string

	// UsageCompleteness and UsageProvenance are the stored values; each is
	// empty when the column is NULL or [RunHistoryCapabilities.UsageProvenance]
	// reports false.
	UsageCompleteness string
	UsageProvenance   string

	// ReportedUnits holds the distinct units of the run's reported amounts,
	// ascending, US dollars included. It is nil when the run reported none or
	// [RunHistoryCapabilities.UsageProvenance] reports false.
	ReportedUnits []string
}

// joinedPart holds the run_model_usage columns of one result row, all NULL
// when the LEFT JOIN found no part for the run.
type joinedPart struct {
	model                                            sql.NullString
	inputTokens, outputTokens, cacheRead, cacheWrite sql.NullInt64
	cost                                             sql.NullFloat64
	costStatus                                       sql.NullString
	priceSource                                      sql.NullString
}

// dest lists the scan targets in the order of the part columns.
func (p *joinedPart) dest() []any {
	return []any{&p.model, &p.inputTokens, &p.outputTokens, &p.cacheRead, &p.cacheWrite, &p.cost, &p.costStatus, &p.priceSource}
}

// part returns the part the result row carries; ok is false for a run
// without parts.
func (p *joinedPart) part() (part RunModelPart, ok bool) {
	if !p.model.Valid {
		return RunModelPart{}, false
	}
	part = RunModelPart{
		Model: p.model.String,
		Tokens: ModelTokens{
			InputTokens:      p.inputTokens.Int64,
			OutputTokens:     p.outputTokens.Int64,
			CacheReadTokens:  p.cacheRead.Int64,
			CacheWriteTokens: p.cacheWrite.Int64,
		},
		CostStatus:  p.costStatus.String,
		PriceSource: p.priceSource.String,
	}
	if p.cost.Valid {
		part.EstimatedCostUSD = new(p.cost.Float64)
	}
	return part, true
}

// runStatsColumnsFull is the column list when Full reports true and
// HasCacheWriteTokens false.
const runStatsColumnsFull = `SELECT run_history.issue_id, run_history.status, run_history.agent_adapter,
	run_history.rule_name, run_history.template_id, run_history.started_at, run_history.completed_at,
	run_history.turns_completed, run_history.review_metadata, run_history.input_tokens, run_history.output_tokens,
	run_history.total_tokens, run_history.cache_read_tokens, run_history.tokens_measured`

// runStatsColumnsFullWithCacheWrite is the column list when Full and
// HasCacheWriteTokens both report true.
const runStatsColumnsFullWithCacheWrite = `SELECT run_history.issue_id, run_history.status, run_history.agent_adapter,
	run_history.rule_name, run_history.template_id, run_history.started_at, run_history.completed_at,
	run_history.turns_completed, run_history.review_metadata, run_history.input_tokens, run_history.output_tokens,
	run_history.total_tokens, run_history.cache_read_tokens, run_history.cache_write_tokens,
	run_history.tokens_measured`

// runStatsColumnsBase is the column list every schema version carries,
// used when Full reports false.
const runStatsColumnsBase = `SELECT run_history.issue_id, run_history.status, run_history.agent_adapter,
	run_history.started_at, run_history.completed_at`

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

	hasReportedAmounts, err := s.tableExists(ctx, "run_reported_amounts")
	if err != nil {
		return RunHistoryCapabilities{}, err
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
		HasStageChains:   columns["chain_id"] && columns["stage_target"] && columns["stage_result"],
		HasTrigger:       columns["trigger"] && columns["reaction_kind"],
		HasFirstAfterHop: columns["first_after_hop"],
		HasAgentBlocked:  columns["agent_blocked"],
		HasRecordedCost:  columns["estimated_cost_usd"] && columns["cost_status"],
		HasSkills:        columns["skills"],
		HasUsageProvenance: columns["usage_completeness"] && columns["usage_provenance"] &&
			hasReportedAmounts,
	}, nil
}

// tableExists reports whether the database holds a table called name. The
// caller's earlier statement MUST be closed already, because OpenReadOnly caps
// the pool at one connection.
func (s *Store) tableExists(ctx context.Context, name string) (bool, error) {
	var found int
	err := s.db.QueryRowContext(ctx, "SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?", name).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s schema: %w", name, err)
	}
	return true, nil
}

// ScanRunHistoryRange issues one SELECT over run_history bounded by a
// half-open completed_at range and calls visit once per run in ascending
// id order. caps selects the projection through [RunHistoryCapabilities.Full];
// a nil since or until omits its predicate rather than substituting a
// sentinel. since is compared with >= and until with <, both formatted as
// since.UTC().Format(time.RFC3339). A non-nil error returned by visit
// aborts iteration and is returned unmodified, so the caller can compare it
// with errors.Is.
//
// While caps.RecordedCost() holds, the statement joins each run's
// run_model_usage rows, so the runs and their parts come from one snapshot
// of the database. While caps.UsageProvenance() holds it also reads each
// run's reported units through a per-run subquery and never joins the
// amounts, which would repeat the run's row and count its tokens twice.
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
	chains := caps.ChainBreakdown()
	triggers := caps.TriggerBreakdown()
	origins := caps.OriginBreakdown()
	funnels := caps.FunnelBreakdown()
	recordedCost := caps.RecordedCost()
	skills := caps.SkillBreakdown()
	provenance := caps.UsageProvenance()
	query := runStatsColumnsBase
	switch {
	case full && caps.HasCacheWriteTokens:
		query = runStatsColumnsFullWithCacheWrite
	case full:
		query = runStatsColumnsFull
	}
	if breakdown {
		query += ", run_history.configured_model, run_history.reported_model"
	}
	if chains {
		query += ", run_history.identifier, run_history.display_identifier, run_history.chain_id," +
			" run_history.stage_previous, run_history.stage_result"
	}
	if triggers {
		query += ", run_history.trigger, run_history.reaction_kind"
	}
	if origins {
		query += ", run_history.first_after_hop"
	}
	var args []any
	if funnels {
		query += `, run_history.agent_blocked,
		CASE WHEN run_history.status = 'failed' AND substr(run_history.error, 1, ?) = ? THEN 1 ELSE 0 END`
		args = append(args, len(HandoffAbsenceErrorPrefix), HandoffAbsenceErrorPrefix)
	}
	if recordedCost {
		query += `, run_history.estimated_cost_usd, run_history.cost_status, run_history.unaccounted_turns,
		run_history.id,
		run_model_usage.model, run_model_usage.input_tokens, run_model_usage.output_tokens,
		run_model_usage.cache_read_tokens, run_model_usage.cache_write_tokens,
		run_model_usage.estimated_cost_usd, run_model_usage.cost_status, run_model_usage.price_source`
	}
	if skills {
		query += ", run_history.skills"
	}
	if provenance {
		query += ", run_history.usage_completeness, run_history.usage_provenance, " + reportedUnitsOfRun
	}
	query += "\nFROM run_history"
	if recordedCost {
		query += " LEFT JOIN run_model_usage ON run_model_usage.run_id = run_history.id"
	}

	where := ""
	if since != nil {
		where += " WHERE run_history.completed_at >= ?"
		args = append(args, since.UTC().Format(time.RFC3339))
	}
	if until != nil {
		if where == "" {
			where += " WHERE run_history.completed_at < ?"
		} else {
			where += " AND run_history.completed_at < ?"
		}
		args = append(args, until.UTC().Format(time.RFC3339))
	}
	query += where + " ORDER BY run_history.id ASC"
	if recordedCost {
		query += ", run_model_usage.model ASC"
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("scan run history range: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only query; close error is non-actionable

	var pending *RunStatsRow
	var pendingID int64
	for rows.Next() {
		var row RunStatsRow
		var reviewMeta, displayID, trigger, reactionKind, costStatus, skillCalls sql.NullString
		var completeness, provenanceText, unitsText sql.NullString
		var cost sql.NullFloat64
		var runID int64
		var joined joinedPart
		var dest []any
		switch {
		case full && caps.HasCacheWriteTokens:
			dest = []any{
				&row.IssueID, &row.Status, &row.AgentAdapter, &row.RuleName, &row.TemplateID,
				&row.StartedAt, &row.CompletedAt, &row.TurnsCompleted, &reviewMeta,
				&row.InputTokens, &row.OutputTokens, &row.TotalTokens, &row.CacheReadTokens, &row.CacheWriteTokens,
				&row.TokensMeasured,
			}
		case full:
			dest = []any{
				&row.IssueID, &row.Status, &row.AgentAdapter, &row.RuleName, &row.TemplateID,
				&row.StartedAt, &row.CompletedAt, &row.TurnsCompleted, &reviewMeta,
				&row.InputTokens, &row.OutputTokens, &row.TotalTokens, &row.CacheReadTokens, &row.TokensMeasured,
			}
		default:
			dest = []any{&row.IssueID, &row.Status, &row.AgentAdapter, &row.StartedAt, &row.CompletedAt}
		}
		if breakdown {
			dest = append(dest, &row.ConfiguredModel, &row.ReportedModel)
		}
		if chains {
			dest = append(dest, &row.Identifier, &displayID, &row.ChainID, &row.StagePrevious, &row.StageResult)
		}
		if triggers {
			dest = append(dest, &trigger, &reactionKind)
		}
		if origins {
			dest = append(dest, &row.FirstAfterHop)
		}
		if funnels {
			dest = append(dest, &row.AgentBlocked, &row.HandoffWithheld)
		}
		if recordedCost {
			dest = append(dest, &cost, &costStatus, &row.UnaccountedTurns, &runID)
			dest = append(dest, joined.dest()...)
		}
		if skills {
			dest = append(dest, &skillCalls)
		}
		if provenance {
			dest = append(dest, &completeness, &provenanceText, &unitsText)
		}
		if err := rows.Scan(dest...); err != nil {
			return fmt.Errorf("scan run history range: %w", err)
		}
		if reviewMeta.Valid {
			row.ReviewMetadata = new(reviewMeta.String)
		}
		row.DisplayID = displayID.String
		if trigger.Valid {
			row.Trigger = new(trigger.String)
		}
		row.ReactionKind = reactionKind.String
		if cost.Valid {
			row.EstimatedCostUSD = new(cost.Float64)
		}
		row.CostStatus = costStatus.String
		if skillCalls.Valid {
			row.Skills = new(skillCalls.String)
		}
		row.UsageCompleteness = completeness.String
		row.UsageProvenance = provenanceText.String
		row.ReportedUnits = splitReportedUnits(unitsText)

		if !recordedCost {
			if err := visit(row); err != nil {
				return err
			}
			continue
		}

		if pending == nil || runID != pendingID {
			if pending != nil {
				if err := visit(*pending); err != nil {
					return err
				}
			}
			pending, pendingID = &row, runID
		}
		if part, ok := joined.part(); ok {
			pending.Parts = append(pending.Parts, part)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("scan run history range: %w", err)
	}
	if pending != nil {
		return visit(*pending)
	}
	return nil
}
