package persistence

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const reactionRecoveryMaxCandidates = 200

// HandoffAbsenceErrorPrefix is the reserved prefix used when a handoff is
// withheld because the orchestrator observed no work (or treats an
// undeterminable verdict as absence under the strict policy). Keeping the
// marker stable lets the retry and polling paths reconstruct the consecutive
// absence sequence from run_history without adding a verdict column.
const HandoffAbsenceErrorPrefix = "handoff withheld: "

// RunHistory represents a single completed run attempt persisted in the
// run_history table. The ID field is assigned by the database on insert and
// should be left zero when calling [Store.AppendRunHistory].
type RunHistory struct {
	ID             int64   // Auto-increment primary key; zero on insert, set on read.
	IssueID        string  // Tracker-internal issue ID.
	Identifier     string  // Human-readable ticket key (e.g. "PROJ-42").
	DisplayID      string  // Qualified display form (e.g. "owner/repo#9"); empty when Identifier is self-explanatory.
	Attempt        int     // Attempt number at time of run (1-based).
	AgentAdapter   string  // Agent adapter kind used (e.g. "claude-code", "mock").
	Workspace      string  // Workspace path used for this run.
	StartedAt      string  // ISO-8601 timestamp of run start.
	CompletedAt    string  // ISO-8601 timestamp of run completion.
	Status         string  // Terminal status: "succeeded", "failed", "cancelled", or "ci_failed".
	Error          *string // Error message if failed; nil on success.
	WorkflowFile   string  // Base filename of the WORKFLOW.md file; empty for pre-migration rows.
	TurnsCompleted int     // Number of coding turns completed in this run.
	ReviewMetadata *string // JSON-serialized ReviewMetadata; nil when self-review did not run.
	RuleName       string  // Dispatch rule name frozen at initial dispatch; empty for legacy rows and fallback dispatches.
	TemplateID     string  // Resolved template path frozen at initial dispatch; empty for legacy rows and the workflow body template.

	InputTokens     int64 // Accumulated input tokens for the run; 0 for pre-migration rows.
	OutputTokens    int64 // Accumulated output tokens for the run; 0 for pre-migration rows.
	TotalTokens     int64 // Accumulated total tokens for the run; 0 for pre-migration rows.
	CacheReadTokens int64 // Accumulated cache-read tokens for the run; 0 for pre-migration rows.

	// TokensMeasured is true when the row's four token columns carry a
	// figure the coding agent's runtime reported, and false when the
	// run's spend is unknown rather than zero. Every writer must set
	// this field explicitly: the column's SQL default is 1, which does
	// not match this field's Go zero value of false, so a writer that
	// omits it records an unmeasured run rather than inheriting the
	// column default.
	TokensMeasured bool
}

// AppendRunHistory inserts a completed run attempt into run_history. The ID
// field of the input is ignored; the database assigns an auto-increment key.
// Returns the inserted record with ID populated.
func (s *Store) AppendRunHistory(ctx context.Context, run RunHistory) (RunHistory, error) {
	errVal := sql.NullString{}
	if run.Error != nil {
		errVal = sql.NullString{String: *run.Error, Valid: true}
	}

	wfVal := sql.NullString{}
	if run.WorkflowFile != "" {
		wfVal = sql.NullString{String: run.WorkflowFile, Valid: true}
	}

	dispIDVal := sql.NullString{}
	if run.DisplayID != "" {
		dispIDVal = sql.NullString{String: run.DisplayID, Valid: true}
	}

	reviewMetaVal := sql.NullString{}
	if run.ReviewMetadata != nil {
		reviewMetaVal = sql.NullString{String: *run.ReviewMetadata, Valid: true}
	}

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO run_history
			(issue_id, identifier, display_identifier, attempt, agent_adapter, workspace, started_at, completed_at, status, error, workflow_file, turns_completed, review_metadata, rule_name, template_id, input_tokens, output_tokens, total_tokens, cache_read_tokens, tokens_measured)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.IssueID, run.Identifier, dispIDVal, run.Attempt, run.AgentAdapter,
		run.Workspace, run.StartedAt, run.CompletedAt, run.Status, errVal, wfVal,
		run.TurnsCompleted, reviewMetaVal, run.RuleName, run.TemplateID,
		run.InputTokens, run.OutputTokens, run.TotalTokens, run.CacheReadTokens, run.TokensMeasured,
	)
	if err != nil {
		return RunHistory{}, fmt.Errorf("append run history for %q: %w", run.IssueID, err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return RunHistory{}, fmt.Errorf("append run history last insert id: %w", err)
	}
	run.ID = id
	return run, nil
}

// QueryRunHistoryByIssue returns all run history entries for the given issue
// ID, ordered by id descending (most recent first). Returns an empty non-nil
// slice when no entries exist.
func (s *Store) QueryRunHistoryByIssue(ctx context.Context, issueID string) ([]RunHistory, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, issue_id, identifier, display_identifier, attempt, agent_adapter, workspace,
			started_at, completed_at, status, error, workflow_file, turns_completed, review_metadata, rule_name, template_id,
			input_tokens, output_tokens, total_tokens, cache_read_tokens, tokens_measured
		FROM run_history
		WHERE issue_id = ?
		ORDER BY id DESC`, issueID)
	if err != nil {
		return nil, fmt.Errorf("query run history by issue %q: %w", issueID, err)
	}
	defer rows.Close() //nolint:errcheck // read-only query; close error is non-actionable

	entries := []RunHistory{}
	for rows.Next() {
		var r RunHistory
		var errVal, wfVal, dispIDVal, reviewMetaVal sql.NullString
		if err := rows.Scan(
			&r.ID, &r.IssueID, &r.Identifier, &dispIDVal, &r.Attempt, &r.AgentAdapter,
			&r.Workspace, &r.StartedAt, &r.CompletedAt, &r.Status, &errVal, &wfVal,
			&r.TurnsCompleted, &reviewMetaVal, &r.RuleName, &r.TemplateID,
			&r.InputTokens, &r.OutputTokens, &r.TotalTokens, &r.CacheReadTokens, &r.TokensMeasured,
		); err != nil {
			return nil, fmt.Errorf("scan run history: %w", err)
		}
		if errVal.Valid {
			r.Error = new(errVal.String)
		}
		if wfVal.Valid {
			r.WorkflowFile = wfVal.String
		}
		if dispIDVal.Valid {
			r.DisplayID = dispIDVal.String
		}
		if reviewMetaVal.Valid {
			r.ReviewMetadata = new(reviewMetaVal.String)
		}
		entries = append(entries, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query run history by issue: %w", err)
	}
	return entries, nil
}

// LoadLatestSuccessfulRunsForReactionRecovery returns at most limit latest
// successful run_history rows, one per issue, for rows with non-empty
// workspace paths and completed_at >= completedAfter. Results are ordered by
// descending run_history id. The limit is clamped to the recovery maximum
// before querying. Returns an empty non-nil slice when no rows qualify.
func (s *Store) LoadLatestSuccessfulRunsForReactionRecovery(ctx context.Context, completedAfter time.Time, limit int) ([]RunHistory, error) {
	if limit <= 0 {
		limit = 1
	}
	if limit > reactionRecoveryMaxCandidates {
		limit = reactionRecoveryMaxCandidates
	}

	rows, err := s.db.QueryContext(ctx,
		`WITH latest AS (
			SELECT issue_id, MAX(id) AS latest_id
			FROM run_history
			WHERE status = 'succeeded'
			  AND workspace <> ''
			  AND completed_at >= ?
			GROUP BY issue_id
		), bounded AS (
			SELECT latest_id
			FROM latest
			ORDER BY latest_id DESC
			LIMIT ?
		)
		SELECT r.id, r.issue_id, r.identifier, r.display_identifier, r.attempt, r.agent_adapter,
			r.workspace, r.started_at, r.completed_at, r.status, r.error, r.workflow_file,
			r.turns_completed, r.review_metadata, r.rule_name, r.template_id,
			r.input_tokens, r.output_tokens, r.total_tokens, r.cache_read_tokens, r.tokens_measured
		FROM run_history AS r
		JOIN bounded ON bounded.latest_id = r.id
		ORDER BY r.id DESC`, completedAfter.UTC().Format(time.RFC3339), limit)
	if err != nil {
		return nil, fmt.Errorf("load recovery runs: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only query; close error is non-actionable

	entries := []RunHistory{}
	for rows.Next() {
		var run RunHistory
		var errVal, wfVal, dispIDVal, reviewMetaVal sql.NullString
		if err := rows.Scan(
			&run.ID, &run.IssueID, &run.Identifier, &dispIDVal, &run.Attempt, &run.AgentAdapter,
			&run.Workspace, &run.StartedAt, &run.CompletedAt, &run.Status, &errVal, &wfVal,
			&run.TurnsCompleted, &reviewMetaVal, &run.RuleName, &run.TemplateID,
			&run.InputTokens, &run.OutputTokens, &run.TotalTokens, &run.CacheReadTokens, &run.TokensMeasured,
		); err != nil {
			return nil, fmt.Errorf("load recovery runs: %w", err)
		}
		if errVal.Valid {
			run.Error = new(errVal.String)
		}
		if wfVal.Valid {
			run.WorkflowFile = wfVal.String
		}
		if dispIDVal.Valid {
			run.DisplayID = dispIDVal.String
		}
		if reviewMetaVal.Valid {
			run.ReviewMetadata = new(reviewMetaVal.String)
		}
		entries = append(entries, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load recovery runs: %w", err)
	}
	return entries, nil
}

// QueryRecentRunHistory returns the most recent run history entries across all
// issues, ordered by id descending. The limit parameter caps the number of
// returned rows (clamped to a minimum of 1). For cursor-based pagination, pass
// the smallest id from the previous page as afterID; pass 0 to start from the
// most recent entry. Returns an empty non-nil slice when no entries exist.
func (s *Store) QueryRecentRunHistory(ctx context.Context, limit int, afterID int64) ([]RunHistory, error) {
	if limit <= 0 {
		limit = 1
	}

	var rows *sql.Rows
	var err error
	if afterID > 0 {
		rows, err = s.db.QueryContext(ctx,
			`SELECT id, issue_id, identifier, display_identifier, attempt, agent_adapter, workspace,
				started_at, completed_at, status, error, workflow_file, turns_completed, review_metadata, rule_name, template_id,
				input_tokens, output_tokens, total_tokens, cache_read_tokens, tokens_measured
			FROM run_history
			WHERE id < ?
			ORDER BY id DESC
			LIMIT ?`, afterID, limit)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT id, issue_id, identifier, display_identifier, attempt, agent_adapter, workspace,
				started_at, completed_at, status, error, workflow_file, turns_completed, review_metadata, rule_name, template_id,
				input_tokens, output_tokens, total_tokens, cache_read_tokens, tokens_measured
			FROM run_history
			ORDER BY id DESC
			LIMIT ?`, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("query recent run history: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only query; close error is non-actionable

	entries := []RunHistory{}
	for rows.Next() {
		var r RunHistory
		var errVal, wfVal, dispIDVal, reviewMetaVal sql.NullString
		if err := rows.Scan(
			&r.ID, &r.IssueID, &r.Identifier, &dispIDVal, &r.Attempt, &r.AgentAdapter,
			&r.Workspace, &r.StartedAt, &r.CompletedAt, &r.Status, &errVal, &wfVal,
			&r.TurnsCompleted, &reviewMetaVal, &r.RuleName, &r.TemplateID,
			&r.InputTokens, &r.OutputTokens, &r.TotalTokens, &r.CacheReadTokens, &r.TokensMeasured,
		); err != nil {
			return nil, fmt.Errorf("scan run history: %w", err)
		}
		if errVal.Valid {
			r.Error = new(errVal.String)
		}
		if wfVal.Valid {
			r.WorkflowFile = wfVal.String
		}
		if dispIDVal.Valid {
			r.DisplayID = dispIDVal.String
		}
		if reviewMetaVal.Valid {
			r.ReviewMetadata = new(reviewMetaVal.String)
		}
		entries = append(entries, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query recent run history: %w", err)
	}
	return entries, nil
}

// CountRunHistoryByIssue returns the number of run_history entries for the
// given issue ID. Returns (0, nil) when no entries exist.
func (s *Store) CountRunHistoryByIssue(ctx context.Context, issueID string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run_history WHERE issue_id = ?`, issueID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count run history by issue %q: %w", issueID, err)
	}
	return count, nil
}

// CountWorkerRunsCompletedSince returns the number of worker-session
// run_history rows for the given issue whose completed_at is at or
// after since. Rows whose status is "ci_failed" are excluded by name
// rather than by an inclusion list of qualifying statuses, because that
// status records a CI verdict the reconcile pass observed rather than a
// worker session, and a status added later must count as a worker
// session by default. Returns (0, nil) when no row qualifies.
func (s *Store) CountWorkerRunsCompletedSince(ctx context.Context, issueID string, since time.Time) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run_history
		WHERE issue_id = ?
		  AND status <> 'ci_failed'
		  AND completed_at >= ?`,
		issueID, since.UTC().Format(time.RFC3339),
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count worker runs completed since for issue %q: %w", issueID, err)
	}
	return count, nil
}

// QueryConsecutiveHandoffAbsenceCounts returns the number of handoff-absence
// failures for each requested issue since the run at which
// [Store.ResetHandoffAbsenceSequence] last ended that issue's sequence. Issues
// with no qualifying rows are omitted from the returned map.
//
// Only a work-observed verdict resets the sequence. A terminal status of
// "succeeded" does not, because it is also recorded for outcomes that carry no
// verdict at all: a blocked soft stop, a run that does not drive issue state, a
// run whose evidence was not determinable, and every run under the off policy.
// Counting those as a reset would let an absence sequence alternate below the
// ceiling indefinitely.
func (s *Store) QueryConsecutiveHandoffAbsenceCounts(ctx context.Context, issueIDs []string) (map[string]int, error) {
	counts := make(map[string]int)
	if len(issueIDs) == 0 {
		return counts, nil
	}

	placeholders := strings.Repeat("?,", len(issueIDs))
	placeholders = placeholders[:len(placeholders)-1]

	args := make([]any, 0, len(issueIDs)+2)
	for _, id := range issueIDs {
		args = append(args, id)
	}
	args = append(args, len(HandoffAbsenceErrorPrefix), HandoffAbsenceErrorPrefix)

	query := fmt.Sprintf( //nolint:gosec // placeholders is built only from len(issueIDs); values remain bound parameters
		`SELECT r.issue_id, COUNT(*)
		 FROM run_history AS r
		 WHERE r.issue_id IN (%s)
		   AND r.status = 'failed'
		   AND substr(r.error, 1, ?) = ?
		   AND r.id > COALESCE((
		       SELECT reset.reset_run_id
		       FROM handoff_absence_resets AS reset
		       WHERE reset.issue_id = r.issue_id
		   ), 0)
		 GROUP BY r.issue_id`,
		placeholders,
	)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query consecutive handoff absences: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only query; close error is non-actionable

	for rows.Next() {
		var issueID string
		var count int
		if err := rows.Scan(&issueID, &count); err != nil {
			return nil, fmt.Errorf("scan consecutive handoff absences: %w", err)
		}
		counts[issueID] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query consecutive handoff absences: %w", err)
	}
	return counts, nil
}

// ResetHandoffAbsenceSequence ends the issue's consecutive handoff-absence
// sequence at its most recently recorded run, so
// [Store.QueryConsecutiveHandoffAbsenceCounts] reports zero until a further
// absence is recorded.
//
// The reset point is read from run_history inside the statement rather than
// supplied by the caller, so a work-observed run whose own history row could
// not be persisted still clears the absences recorded before it.
func (s *Store) ResetHandoffAbsenceSequence(ctx context.Context, issueID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO handoff_absence_resets (issue_id, reset_run_id, updated_at)
		VALUES (?, COALESCE((SELECT MAX(id) FROM run_history WHERE issue_id = ?), 0), ?)
		ON CONFLICT (issue_id) DO UPDATE SET
			reset_run_id = excluded.reset_run_id,
			updated_at = excluded.updated_at`,
		issueID, issueID, now,
	)
	if err != nil {
		return fmt.Errorf("reset handoff absence sequence for %q: %w", issueID, err)
	}
	return nil
}

// QueryBudgetExhaustedIssues returns issue IDs from candidateIDs whose
// run_history entry count meets or exceeds maxSessions. Returns an empty
// non-nil slice when no issues qualify or candidateIDs is empty.
func (s *Store) QueryBudgetExhaustedIssues(ctx context.Context, candidateIDs []string, maxSessions int) ([]string, error) {
	if len(candidateIDs) == 0 {
		return []string{}, nil
	}

	placeholders := strings.Repeat("?,", len(candidateIDs))
	placeholders = placeholders[:len(placeholders)-1]

	args := make([]any, 0, len(candidateIDs)+1)
	for _, id := range candidateIDs {
		args = append(args, id)
	}
	args = append(args, maxSessions)

	query := fmt.Sprintf( //nolint:gosec // placeholders is "?,?,..." built from len(candidateIDs); no user data in format string
		`SELECT issue_id FROM run_history WHERE issue_id IN (%s) GROUP BY issue_id HAVING COUNT(*) >= ?`,
		placeholders,
	)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query budget exhausted issues: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only query; close error is non-actionable

	exhaustedIDs := []string{}
	for rows.Next() {
		var issueID string
		if err := rows.Scan(&issueID); err != nil {
			return nil, fmt.Errorf("scan budget exhausted issue: %w", err)
		}
		exhaustedIDs = append(exhaustedIDs, issueID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query budget exhausted issues: %w", err)
	}
	return exhaustedIDs, nil
}

// IssueTokenUsage is the per-issue token spend read by the token
// ceiling and by the cost_budget tool: a summed total, a row count,
// and a count of rows whose spend is unknown rather than zero.
type IssueTokenUsage struct {
	TotalTokens        int64
	Sessions           int
	UnmeasuredSessions int
}

// TokenUsageByIssue returns the summed total_tokens, the row count, and
// the count of unmeasured rows across all run_history rows for the
// issue. Returns the zero [IssueTokenUsage] and a nil error when the
// issue has no rows. The summed total is exact even though an
// unmeasured row's token columns are always zero.
func (s *Store) TokenUsageByIssue(ctx context.Context, issueID string) (IssueTokenUsage, error) {
	var usage IssueTokenUsage
	row := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(total_tokens), 0), COUNT(*), SUM(CASE WHEN tokens_measured = 0 THEN 1 ELSE 0 END)
		FROM run_history WHERE issue_id = ?`, issueID,
	)
	var unmeasured sql.NullInt64
	if err := row.Scan(&usage.TotalTokens, &usage.Sessions, &unmeasured); err != nil {
		return IssueTokenUsage{}, fmt.Errorf("token usage by issue %q: %w", issueID, err)
	}
	usage.UnmeasuredSessions = int(unmeasured.Int64)
	return usage, nil
}

// latestRunCompletionChunkSize is the maximum number of identifiers
// batched into a single IN (...) query. The on-disk workspace directory
// count that feeds this lookup is unbounded by construction, unlike the
// tracker-page-bounded inputs of the other IN (...) queries in this file.
const latestRunCompletionChunkSize = 500

// LatestRunCompletionByIdentifier returns the most recent completed_at
// value for each of the given identifiers.
//
// Identifiers with no run_history rows are omitted from the result. An
// empty input returns an empty non-nil map without querying. Identifiers
// are queried in batches of at most [latestRunCompletionChunkSize] and
// the per-batch results are merged.
func (s *Store) LatestRunCompletionByIdentifier(ctx context.Context, identifiers []string) (map[string]string, error) {
	result := make(map[string]string, len(identifiers))
	if len(identifiers) == 0 {
		return result, nil
	}

	for start := 0; start < len(identifiers); start += latestRunCompletionChunkSize {
		end := min(start+latestRunCompletionChunkSize, len(identifiers))
		chunk := identifiers[start:end]

		placeholders := strings.Repeat("?,", len(chunk))
		placeholders = placeholders[:len(placeholders)-1]

		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}

		query := fmt.Sprintf( //nolint:gosec // placeholders is "?,?,..." built from len(chunk); no user data in format string
			`SELECT identifier, MAX(completed_at) FROM run_history WHERE identifier IN (%s) GROUP BY identifier`,
			placeholders,
		)

		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("query latest run completion by identifier: %w", err)
		}

		scanErr := func() error {
			defer rows.Close() //nolint:errcheck // read-only query; close error is non-actionable
			for rows.Next() {
				var identifier, completedAt string
				if err := rows.Scan(&identifier, &completedAt); err != nil {
					return fmt.Errorf("scan latest run completion by identifier: %w", err)
				}
				result[identifier] = completedAt
			}
			return rows.Err()
		}()
		if scanErr != nil {
			return nil, fmt.Errorf("query latest run completion by identifier: %w", scanErr)
		}
	}

	return result, nil
}

// QueryTokenBudgetUsage returns one [IssueTokenUsage] per candidate in
// candidateIDs that has at least one run_history row. A candidate with
// no rows is absent from the returned map; the caller reads that as
// zero spend and zero unmeasured sessions. An empty candidateIDs
// returns an empty non-nil map without querying. The threshold
// comparison against a token ceiling is the caller's responsibility.
func (s *Store) QueryTokenBudgetUsage(ctx context.Context, candidateIDs []string) (map[string]IssueTokenUsage, error) {
	usage := map[string]IssueTokenUsage{}
	if len(candidateIDs) == 0 {
		return usage, nil
	}

	placeholders := strings.Repeat("?,", len(candidateIDs))
	placeholders = placeholders[:len(placeholders)-1]

	args := make([]any, 0, len(candidateIDs))
	for _, id := range candidateIDs {
		args = append(args, id)
	}

	query := fmt.Sprintf( //nolint:gosec // placeholders is "?,?,..." built from len(candidateIDs); no user data in format string
		`SELECT issue_id, COALESCE(SUM(total_tokens), 0), COUNT(*), SUM(CASE WHEN tokens_measured = 0 THEN 1 ELSE 0 END)
		FROM run_history WHERE issue_id IN (%s) GROUP BY issue_id`,
		placeholders,
	)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query token budget usage: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only query; close error is non-actionable

	for rows.Next() {
		var issueID string
		var issueUsage IssueTokenUsage
		var unmeasured sql.NullInt64
		if err := rows.Scan(&issueID, &issueUsage.TotalTokens, &issueUsage.Sessions, &unmeasured); err != nil {
			return nil, fmt.Errorf("scan token budget usage: %w", err)
		}
		issueUsage.UnmeasuredSessions = int(unmeasured.Int64)
		usage[issueID] = issueUsage
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query token budget usage: %w", err)
	}
	return usage, nil
}
