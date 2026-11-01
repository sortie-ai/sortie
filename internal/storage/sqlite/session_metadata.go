package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/sortie-ai/sortie/internal/storage"
)

var _ storage.SessionMetadataStore = (*Store)(nil)

// UpsertSessionMetadata inserts or replaces session metadata for the given
// issue. If an entry with the same IssueID already exists, all fields are
// updated.
func (s *Store) UpsertSessionMetadata(ctx context.Context, meta storage.SessionMetadata) error {
	var nullPID sql.NullString
	if meta.AgentPID != nil {
		nullPID = sql.NullString{String: *meta.AgentPID, Valid: true}
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO session_metadata
			(issue_id, session_id, agent_pid, input_tokens, output_tokens, total_tokens,
			 cache_read_tokens, cache_write_tokens, model_name, api_request_count, api_requests_measured,
			 dispatch_id, unaccounted_turns, estimated_cost_usd, cost_status, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (issue_id) DO UPDATE SET
			session_id        = excluded.session_id,
			agent_pid         = excluded.agent_pid,
			input_tokens      = excluded.input_tokens,
			output_tokens     = excluded.output_tokens,
			total_tokens      = excluded.total_tokens,
			cache_read_tokens = excluded.cache_read_tokens,
			cache_write_tokens = excluded.cache_write_tokens,
			model_name        = excluded.model_name,
			api_request_count = excluded.api_request_count,
			api_requests_measured = excluded.api_requests_measured,
			dispatch_id       = excluded.dispatch_id,
			unaccounted_turns = excluded.unaccounted_turns,
			estimated_cost_usd = excluded.estimated_cost_usd,
			cost_status       = excluded.cost_status,
			updated_at        = excluded.updated_at`,
		meta.IssueID, meta.SessionID, nullPID,
		meta.InputTokens, meta.OutputTokens, meta.TotalTokens,
		meta.CacheReadTokens, meta.CacheWriteTokens, meta.ModelName, meta.APIRequestCount,
		meta.APIRequestsMeasured, meta.DispatchID, meta.UnaccountedTurns,
		nullFloat(meta.EstimatedCostUSD), meta.CostStatus, meta.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert session metadata %q: %w", meta.IssueID, err)
	}
	return nil
}

// LoadSessionMetadata returns the session metadata for the given issue ID.
// Returns the metadata and true if found, or a zero-value [storage.SessionMetadata]
// and false if no entry exists.
func (s *Store) LoadSessionMetadata(ctx context.Context, issueID string) (storage.SessionMetadata, bool, error) {
	var m storage.SessionMetadata
	var nullPID sql.NullString
	var nullCost sql.NullFloat64

	err := s.db.QueryRowContext(ctx,
		`SELECT issue_id, session_id, agent_pid, input_tokens, output_tokens, total_tokens,
		        cache_read_tokens, cache_write_tokens, model_name, api_request_count, api_requests_measured,
		        dispatch_id, unaccounted_turns, estimated_cost_usd, cost_status, updated_at
		FROM session_metadata
		WHERE issue_id = ?`, issueID,
	).Scan(&m.IssueID, &m.SessionID, &nullPID,
		&m.InputTokens, &m.OutputTokens, &m.TotalTokens,
		&m.CacheReadTokens, &m.CacheWriteTokens, &m.ModelName, &m.APIRequestCount,
		&m.APIRequestsMeasured, &m.DispatchID, &m.UnaccountedTurns, &nullCost, &m.CostStatus, &m.UpdatedAt)

	if errors.Is(err, sql.ErrNoRows) {
		return storage.SessionMetadata{}, false, nil
	}
	if err != nil {
		return storage.SessionMetadata{}, false, fmt.Errorf("load session metadata %q: %w", issueID, err)
	}
	if nullPID.Valid {
		m.AgentPID = new(nullPID.String)
	}
	if nullCost.Valid {
		m.EstimatedCostUSD = new(nullCost.Float64)
	}
	return m, true, nil
}

// LoadAllSessionMetadata returns all session metadata entries ordered by
// updated_at descending (most recently updated first). Returns an empty
// non-nil slice when no entries exist.
func (s *Store) LoadAllSessionMetadata(ctx context.Context) ([]storage.SessionMetadata, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT issue_id, session_id, agent_pid, input_tokens, output_tokens, total_tokens,
		        cache_read_tokens, cache_write_tokens, model_name, api_request_count, api_requests_measured,
		        dispatch_id, unaccounted_turns, updated_at
		FROM session_metadata
		ORDER BY updated_at DESC, issue_id ASC`)
	if err != nil {
		return nil, fmt.Errorf("load all session metadata: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only query; close error is non-actionable

	entries := []storage.SessionMetadata{}
	for rows.Next() {
		var m storage.SessionMetadata
		var nullPID sql.NullString
		if err := rows.Scan(&m.IssueID, &m.SessionID, &nullPID,
			&m.InputTokens, &m.OutputTokens, &m.TotalTokens,
			&m.CacheReadTokens, &m.CacheWriteTokens, &m.ModelName, &m.APIRequestCount,
			&m.APIRequestsMeasured, &m.DispatchID, &m.UnaccountedTurns, &m.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan session metadata: %w", err)
		}
		if nullPID.Valid {
			m.AgentPID = new(nullPID.String)
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
