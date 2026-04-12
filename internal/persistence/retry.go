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
	SessionID  *string // Adapter-assigned session identifier from the previous worker attempt. Nil when no session was established.
}

// PendingRetry pairs a persisted [RetryEntry] with the computed delay
// remaining until its timer should fire. RemainingMs is zero when the entry's
// due time has already passed, meaning the retry should fire immediately on
// startup.
type PendingRetry struct {
	Entry       RetryEntry
	RemainingMs int64 // max(entry.DueAtMs - nowMs, 0); always >= 0
}

// SaveRetryEntry persists a retry entry using upsert semantics. If an entry
// with the same IssueID already exists, it is replaced entirely.
func (s *Store) SaveRetryEntry(ctx context.Context, entry RetryEntry) error {
	errVal := sql.NullString{}
	if entry.Error != nil {
		errVal = sql.NullString{String: *entry.Error, Valid: true}
	}

	ssnVal := sql.NullString{}
	if entry.SessionID != nil {
		ssnVal = sql.NullString{String: *entry.SessionID, Valid: true}
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO retry_entries (issue_id, identifier, attempt, due_at_ms, error, session_id)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (issue_id) DO UPDATE SET
			identifier = excluded.identifier,
			attempt    = excluded.attempt,
			due_at_ms  = excluded.due_at_ms,
			error      = excluded.error,
			session_id = excluded.session_id`,
		entry.IssueID, entry.Identifier, entry.Attempt, entry.DueAtMs, errVal, ssnVal,
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
		`SELECT issue_id, identifier, attempt, due_at_ms, error, session_id
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
		var ssnVal sql.NullString
		if err := rows.Scan(&e.IssueID, &e.Identifier, &e.Attempt, &e.DueAtMs, &errVal, &ssnVal); err != nil {
			return nil, fmt.Errorf("scan retry entry: %w", err)
		}
		if errVal.Valid {
			s := errVal.String
			e.Error = &s
		}
		if ssnVal.Valid {
			s := ssnVal.String
			e.SessionID = &s
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

// LoadRetryEntriesForRecovery loads all persisted retry entries and computes
// the remaining delay for each relative to nowMs (Unix epoch milliseconds).
// Entries whose due_at_ms has already passed get RemainingMs = 0, meaning the
// retry should fire immediately. Results are ordered by due_at_ms ascending
// then issue_id ascending (same order as [Store.LoadRetryEntries]).
func (s *Store) LoadRetryEntriesForRecovery(ctx context.Context, nowMs int64) ([]PendingRetry, error) {
	entries, err := s.LoadRetryEntries(ctx)
	if err != nil {
		return nil, fmt.Errorf("load retry entries for recovery: %w", err)
	}

	pending := make([]PendingRetry, len(entries))
	for i, e := range entries {
		remaining := e.DueAtMs - nowMs
		if remaining < 0 {
			remaining = 0
		}
		pending[i] = PendingRetry{Entry: e, RemainingMs: remaining}
	}
	return pending, nil
}
