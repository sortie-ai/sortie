package persistence

import (
	"context"
	"database/sql"
	"fmt"
)

// RunHistory represents a single completed run attempt persisted in the
// run_history table. The ID field is assigned by the database on insert and
// should be left zero when calling [Store.AppendRunHistory].
type RunHistory struct {
	ID           int64   // Auto-increment primary key; zero on insert, set on read.
	IssueID      string  // Tracker-internal issue ID.
	Identifier   string  // Human-readable ticket key (e.g. "PROJ-42").
	Attempt      int     // Attempt number at time of run (1-based).
	AgentAdapter string  // Agent adapter kind used (e.g. "claude-code", "mock").
	Workspace    string  // Workspace path used for this run.
	StartedAt    string  // ISO-8601 timestamp of run start.
	CompletedAt  string  // ISO-8601 timestamp of run completion.
	Status       string  // Terminal status: "succeeded", "failed", "timed_out", "stalled", etc.
	Error        *string // Error message if failed; nil on success.
	WorkflowFile string  // Base filename of the WORKFLOW.md file; empty for pre-migration rows.
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

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO run_history
			(issue_id, identifier, attempt, agent_adapter, workspace, started_at, completed_at, status, error, workflow_file)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.IssueID, run.Identifier, run.Attempt, run.AgentAdapter,
		run.Workspace, run.StartedAt, run.CompletedAt, run.Status, errVal, wfVal,
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
		`SELECT id, issue_id, identifier, attempt, agent_adapter, workspace,
			started_at, completed_at, status, error, workflow_file
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
		var errVal, wfVal sql.NullString
		if err := rows.Scan(
			&r.ID, &r.IssueID, &r.Identifier, &r.Attempt, &r.AgentAdapter,
			&r.Workspace, &r.StartedAt, &r.CompletedAt, &r.Status, &errVal, &wfVal,
		); err != nil {
			return nil, fmt.Errorf("scan run history: %w", err)
		}
		if errVal.Valid {
			s := errVal.String
			r.Error = &s
		}
		if wfVal.Valid {
			r.WorkflowFile = wfVal.String
		}
		entries = append(entries, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query run history by issue: %w", err)
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
			`SELECT id, issue_id, identifier, attempt, agent_adapter, workspace,
				started_at, completed_at, status, error, workflow_file
			FROM run_history
			WHERE id < ?
			ORDER BY id DESC
			LIMIT ?`, afterID, limit)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT id, issue_id, identifier, attempt, agent_adapter, workspace,
				started_at, completed_at, status, error, workflow_file
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
		var errVal, wfVal sql.NullString
		if err := rows.Scan(
			&r.ID, &r.IssueID, &r.Identifier, &r.Attempt, &r.AgentAdapter,
			&r.Workspace, &r.StartedAt, &r.CompletedAt, &r.Status, &errVal, &wfVal,
		); err != nil {
			return nil, fmt.Errorf("scan run history: %w", err)
		}
		if errVal.Valid {
			s := errVal.String
			r.Error = &s
		}
		if wfVal.Valid {
			r.WorkflowFile = wfVal.String
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
