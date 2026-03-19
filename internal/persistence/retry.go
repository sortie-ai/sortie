package persistence

import (
	"context"
	"database/sql"
	"fmt"
)

// RetryEntry represents a persisted pending retry for a single issue.
// The timer_handle field from the domain model is runtime-only and is not
// included in the persisted representation.
type RetryEntry struct {
	IssueID    string  // Tracker-internal issue ID (primary key).
	Identifier string  // Human-readable ticket key (best-effort, for logs).
	Attempt    int     // Retry attempt number, 1-based.
	DueAtMs    int64   // Unix epoch milliseconds when the retry timer should fire.
	Error      *string // Last error message; nil when no error.
}

// SaveRetryEntry persists a retry entry using upsert semantics. If an entry
// with the same IssueID already exists, it is replaced entirely.
func (s *Store) SaveRetryEntry(ctx context.Context, entry RetryEntry) error {
	errVal := sql.NullString{}
	if entry.Error != nil {
		errVal = sql.NullString{String: *entry.Error, Valid: true}
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO retry_entries (issue_id, identifier, attempt, due_at_ms, error)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (issue_id) DO UPDATE SET
			identifier = excluded.identifier,
			attempt    = excluded.attempt,
			due_at_ms  = excluded.due_at_ms,
			error      = excluded.error`,
		entry.IssueID, entry.Identifier, entry.Attempt, entry.DueAtMs, errVal,
	)
	if err != nil {
		return fmt.Errorf("save retry entry %q: %w", entry.IssueID, err)
	}
	return nil
}

// LoadRetryEntries returns all persisted retry entries ordered by due_at_ms
// ascending for deterministic startup reconstruction. Returns an empty slice
// (not nil) when no entries exist.
func (s *Store) LoadRetryEntries(ctx context.Context) ([]RetryEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT issue_id, identifier, attempt, due_at_ms, error
		FROM retry_entries
		ORDER BY due_at_ms ASC, issue_id ASC`)
	if err != nil {
		return nil, fmt.Errorf("load retry entries: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only query; close error is non-actionable

	entries := []RetryEntry{}
	for rows.Next() {
		var e RetryEntry
		var errVal sql.NullString
		if err := rows.Scan(&e.IssueID, &e.Identifier, &e.Attempt, &e.DueAtMs, &errVal); err != nil {
			return nil, fmt.Errorf("scan retry entry: %w", err)
		}
		if errVal.Valid {
			e.Error = &errVal.String
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load retry entries: %w", err)
	}
	return entries, nil
}

// DeleteRetryEntry removes the retry entry for the given issue ID. It is a
// no-op if no entry exists for that issue ID.
func (s *Store) DeleteRetryEntry(ctx context.Context, issueID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM retry_entries WHERE issue_id = ?`, issueID)
	if err != nil {
		return fmt.Errorf("delete retry entry %q: %w", issueID, err)
	}
	return nil
}
