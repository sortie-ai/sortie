package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// SessionMetadata represents the last known session metadata for a single
// issue, persisted in the session_metadata table. It captures the agent
// session ID, process ID, and accumulated token counters at the time of
// the last update.
type SessionMetadata struct {
	IssueID      string  // Tracker-internal issue ID (primary key).
	SessionID    string  // Last session ID assigned by the agent adapter.
	AgentPID     *string // Last known agent PID; nil when unknown.
	InputTokens  int64   // Accumulated input tokens for the session.
	OutputTokens int64   // Accumulated output tokens for the session.
	TotalTokens  int64   // Accumulated total tokens for the session.
	UpdatedAt    string  // ISO-8601 timestamp of last update.
}

// UpsertSessionMetadata inserts or replaces session metadata for the given
// issue. If an entry with the same IssueID already exists, all fields are
// updated.
func (s *Store) UpsertSessionMetadata(ctx context.Context, meta SessionMetadata) error {
	pidVal := sql.NullString{}
	if meta.AgentPID != nil {
		pidVal = sql.NullString{String: *meta.AgentPID, Valid: true}
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO session_metadata
			(issue_id, session_id, agent_pid, input_tokens, output_tokens, total_tokens, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (issue_id) DO UPDATE SET
			session_id    = excluded.session_id,
			agent_pid     = excluded.agent_pid,
			input_tokens  = excluded.input_tokens,
			output_tokens = excluded.output_tokens,
			total_tokens  = excluded.total_tokens,
			updated_at    = excluded.updated_at`,
		meta.IssueID, meta.SessionID, pidVal,
		meta.InputTokens, meta.OutputTokens, meta.TotalTokens, meta.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert session metadata %q: %w", meta.IssueID, err)
	}
	return nil
}

// LoadSessionMetadata returns the session metadata for the given issue ID.
// Returns the metadata and true if found, or a zero-value [SessionMetadata]
// and false if no entry exists.
func (s *Store) LoadSessionMetadata(ctx context.Context, issueID string) (SessionMetadata, bool, error) {
	var m SessionMetadata
	var pidVal sql.NullString

	err := s.db.QueryRowContext(ctx,
		`SELECT issue_id, session_id, agent_pid, input_tokens, output_tokens, total_tokens, updated_at
		FROM session_metadata
		WHERE issue_id = ?`, issueID,
	).Scan(&m.IssueID, &m.SessionID, &pidVal,
		&m.InputTokens, &m.OutputTokens, &m.TotalTokens, &m.UpdatedAt)

	if errors.Is(err, sql.ErrNoRows) {
		return SessionMetadata{}, false, nil
	}
	if err != nil {
		return SessionMetadata{}, false, fmt.Errorf("load session metadata %q: %w", issueID, err)
	}
	if pidVal.Valid {
		v := pidVal.String
		m.AgentPID = &v
	}
	return m, true, nil
}

// LoadAllSessionMetadata returns all session metadata entries ordered by
// updated_at descending (most recently updated first). Returns an empty
// non-nil slice when no entries exist.
func (s *Store) LoadAllSessionMetadata(ctx context.Context) ([]SessionMetadata, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT issue_id, session_id, agent_pid, input_tokens, output_tokens, total_tokens, updated_at
		FROM session_metadata
		ORDER BY updated_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("load all session metadata: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only query; close error is non-actionable

	entries := []SessionMetadata{}
	for rows.Next() {
		var m SessionMetadata
		var pidVal sql.NullString
		if err := rows.Scan(&m.IssueID, &m.SessionID, &pidVal,
			&m.InputTokens, &m.OutputTokens, &m.TotalTokens, &m.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan session metadata: %w", err)
		}
		if pidVal.Valid {
			v := pidVal.String
			m.AgentPID = &v
		}
		entries = append(entries, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load all session metadata: %w", err)
	}
	return entries, nil
}

// DeleteSessionMetadata removes the session metadata entry for the given
// issue ID. It is a no-op if no entry exists for that issue ID.
func (s *Store) DeleteSessionMetadata(ctx context.Context, issueID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM session_metadata WHERE issue_id = ?`, issueID)
	if err != nil {
		return fmt.Errorf("delete session metadata %q: %w", issueID, err)
	}
	return nil
}
