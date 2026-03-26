package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// AggregateMetrics represents global token and runtime totals persisted in
// the aggregate_metrics table. The Key field identifies the metric category
// (e.g., "agent_totals"). These counters survive process restarts and are
// restored during startup recovery.
type AggregateMetrics struct {
	Key             string  // Metric key (primary key, e.g. "agent_totals").
	InputTokens     int64   // Cumulative input tokens.
	OutputTokens    int64   // Cumulative output tokens.
	TotalTokens     int64   // Cumulative total tokens.
	CacheReadTokens int64   // Cumulative cache-read tokens.
	SecondsRunning  float64 // Cumulative runtime seconds.
	UpdatedAt       string  // ISO-8601 timestamp of last update.
}

// UpsertAggregateMetrics inserts or replaces aggregate metrics for the given
// key. If an entry with the same Key already exists, all fields are updated.
// The caller computes cumulative totals before calling; this method stores
// provided values directly.
func (s *Store) UpsertAggregateMetrics(ctx context.Context, metrics AggregateMetrics) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO aggregate_metrics
			(key, input_tokens, output_tokens, total_tokens, cache_read_tokens, seconds_running, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET
			input_tokens      = excluded.input_tokens,
			output_tokens     = excluded.output_tokens,
			total_tokens      = excluded.total_tokens,
			cache_read_tokens = excluded.cache_read_tokens,
			seconds_running   = excluded.seconds_running,
			updated_at        = excluded.updated_at`,
		metrics.Key, metrics.InputTokens, metrics.OutputTokens,
		metrics.TotalTokens, metrics.CacheReadTokens, metrics.SecondsRunning, metrics.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert aggregate metrics %q: %w", metrics.Key, err)
	}
	return nil
}

// LoadAggregateMetrics returns the aggregate metrics for the given key.
// Returns the metrics and true if found, or a zero-value [AggregateMetrics]
// and false if no entry exists.
func (s *Store) LoadAggregateMetrics(ctx context.Context, key string) (AggregateMetrics, bool, error) {
	var m AggregateMetrics

	err := s.db.QueryRowContext(ctx,
		`SELECT key, input_tokens, output_tokens, total_tokens, cache_read_tokens, seconds_running, updated_at
		FROM aggregate_metrics
		WHERE key = ?`, key,
	).Scan(&m.Key, &m.InputTokens, &m.OutputTokens,
		&m.TotalTokens, &m.CacheReadTokens, &m.SecondsRunning, &m.UpdatedAt)

	if errors.Is(err, sql.ErrNoRows) {
		return AggregateMetrics{}, false, nil
	}
	if err != nil {
		return AggregateMetrics{}, false, fmt.Errorf("load aggregate metrics %q: %w", key, err)
	}
	return m, true, nil
}
