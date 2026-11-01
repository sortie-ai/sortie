package storage

import (
	"context"
)

// SessionMetadata represents the last known session metadata for a single
// issue, persisted by [SessionMetadataStore]. It captures the agent
// session ID, process ID, and accumulated token counters at the time of
// the last update.
type SessionMetadata struct {
	IssueID          string  // Tracker-internal issue ID (primary key).
	SessionID        string  // Last session ID assigned by the agent adapter.
	AgentPID         *string // Last known agent PID; nil when unknown.
	InputTokens      int64   // Accumulated input tokens for the session.
	OutputTokens     int64   // Accumulated output tokens for the session.
	TotalTokens      int64   // Accumulated total tokens for the session.
	CacheReadTokens  int64   // Accumulated cache-read tokens for the session.
	CacheWriteTokens int64   // Accumulated cache-write tokens for the session.
	ModelName        string  // Last reported LLM model identifier.
	APIRequestCount  int     // Measured model API requests; zero when APIRequestsMeasured is false.
	UpdatedAt        string  // ISO-8601 timestamp of last update.

	// APIRequestsMeasured is true when APIRequestCount is a
	// measurement of model API requests. False when nothing counted
	// them, in which case a current writer stores APIRequestCount as
	// zero so the row cannot contradict its own qualifier. A row last
	// written before this field existed is the exception: it keeps the
	// count it had beside a false qualifier until its issue runs again.
	APIRequestsMeasured bool

	// DispatchID is the dispatch ID of the running session whose
	// in-flight write last stored the row; empty when the session-exit
	// write last stored it or the record was stored before the field existed.
	DispatchID string

	// UnaccountedTurns is the running session's count of turns that
	// spent tokens no figure was proven to cover, as of the last
	// in-flight write; zero after the session-exit write.
	UnaccountedTurns int

	// EstimatedCostUSD and CostStatus are the running session's live
	// estimate as of the last in-flight write. Nil and "" mean no estimate.
	EstimatedCostUSD *float64
	CostStatus       string
}

// SessionMetadataStore records the agent session of each issue.
type SessionMetadataStore interface {
	UpsertSessionMetadata(ctx context.Context, meta SessionMetadata) error
	LoadSessionMetadata(ctx context.Context, issueID string) (SessionMetadata, bool, error)
	LoadAllSessionMetadata(ctx context.Context) ([]SessionMetadata, error)
	DeleteSessionMetadata(ctx context.Context, issueID string) error
}
