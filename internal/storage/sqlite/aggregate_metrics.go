package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/sortie-ai/sortie/internal/storage"
)

var _ storage.AggregateMetricsStore = (*Store)(nil)

// UpsertAggregateMetrics inserts or replaces aggregate metrics for the given
// key. If an entry with the same Key already exists, all fields are updated.
// The caller computes cumulative totals before calling; this method stores
// provided values directly.
func (s *Store) UpsertAggregateMetrics(ctx context.Context, metrics storage.AggregateMetrics) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO aggregate_metrics
			(key, input_tokens, output_tokens, total_tokens, cache_read_tokens, cache_write_tokens, seconds_running, unmeasured_sessions, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET
			input_tokens        = excluded.input_tokens,
			output_tokens       = excluded.output_tokens,
			total_tokens        = excluded.total_tokens,
			cache_read_tokens   = excluded.cache_read_tokens,
			cache_write_tokens  = excluded.cache_write_tokens,
			seconds_running     = excluded.seconds_running,
			unmeasured_sessions = excluded.unmeasured_sessions,
			updated_at          = excluded.updated_at`,
		metrics.Key, metrics.InputTokens, metrics.OutputTokens,
		metrics.TotalTokens, metrics.CacheReadTokens, metrics.CacheWriteTokens, metrics.SecondsRunning,
		metrics.UnmeasuredSessions, metrics.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert aggregate metrics %q: %w", metrics.Key, err)
	}
	return nil
}

// LoadAggregateMetrics returns the aggregate metrics for the given key.
// Returns the metrics and true if found, or a zero-value [storage.AggregateMetrics]
// and false if no entry exists.
func (s *Store) LoadAggregateMetrics(ctx context.Context, key string) (storage.AggregateMetrics, bool, error) {
	var m storage.AggregateMetrics

	err := s.db.QueryRowContext(ctx,
		`SELECT key, input_tokens, output_tokens, total_tokens, cache_read_tokens, cache_write_tokens, seconds_running, unmeasured_sessions, updated_at
		FROM aggregate_metrics
		WHERE key = ?`, key,
	).Scan(&m.Key, &m.InputTokens, &m.OutputTokens,
		&m.TotalTokens, &m.CacheReadTokens, &m.CacheWriteTokens, &m.SecondsRunning, &m.UnmeasuredSessions, &m.UpdatedAt)

	if errors.Is(err, sql.ErrNoRows) {
		return storage.AggregateMetrics{}, false, nil
	}
	if err != nil {
		return storage.AggregateMetrics{}, false, fmt.Errorf("load aggregate metrics %q: %w", key, err)
	}
	return m, true, nil
}
