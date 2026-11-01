package storage

import (
	"context"
)

// AggregateMetrics represents global token and runtime totals persisted by
// [AggregateMetricsStore]. The Key field identifies the metric category
// (e.g., "agent_totals"). These counters survive process restarts and are
// restored during startup recovery.
type AggregateMetrics struct {
	Key              string // Primary key identifying the metric category (e.g. "agent_totals").
	InputTokens      int64
	OutputTokens     int64
	TotalTokens      int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	SecondsRunning   float64

	// UnmeasuredSessions is a cumulative count of ended sessions whose
	// usage was never recorded, marking the totals above as a lower
	// bound.
	UnmeasuredSessions int64

	UpdatedAt string // ISO-8601 timestamp of last update.
}

// AggregateMetricsStore records metric totals under a key.
type AggregateMetricsStore interface {
	UpsertAggregateMetrics(ctx context.Context, metrics AggregateMetrics) error
	LoadAggregateMetrics(ctx context.Context, key string) (AggregateMetrics, bool, error)
}
