package storage

import (
	"context"
	"time"
)

// RunTurn is one turn record of a run.
type RunTurn struct {
	// Seq is the record's position in its run, 1 for the first.
	Seq int

	// EndedAt is the turn's end instant in UTC.
	EndedAt time.Time

	// TokensMeasured is false when the turn's usage was not measured; Models
	// and the cost fields then carry no figure.
	TokensMeasured bool

	ToolCalls int
	Errors    int

	// Models is the turn's tokens per model; nil when !TokensMeasured or no
	// class is non-zero.
	Models map[string]ModelTokens

	// EstimatedCostUSD is the turn's cost; nil means no cost figure.
	EstimatedCostUSD *float64

	// The class costs split EstimatedCostUSD by pricing class and are zero
	// and unstored when it is nil. InputCostUSD is fresh input, the input
	// less cache reads and cache writes.
	InputCostUSD      float64
	OutputCostUSD     float64
	CacheReadCostUSD  float64
	CacheWriteCostUSD float64
}

// RunTurnRow is a stored turn record with the fields of its run_history row.
type RunTurnRow struct {
	RunID      int64
	IssueID    string
	Identifier string

	// DisplayID is the qualified form [RunHistory.DisplayID] holds; empty when
	// Identifier suffices.
	DisplayID  string
	IssueTitle string
	DispatchID string
	RuleName   string

	AgentAdapter string
	Status       string
	CostStatus   string

	// UsageCompleteness is the run's stored completeness.
	UsageCompleteness string

	Turn RunTurn
}

// RunTurnStore reads the stored turn records of finished runs.
type RunTurnStore interface {
	// ScanRunTurnsRange visits every stored turn record whose EndedAt is at or
	// after since and before until; a nil bound is open. Visits come in
	// ascending order of EndedAt, then RunID, then Seq, and each row carries all
	// of the turn's Models.
	//
	// visit MUST NOT call the store, which may hold its only connection for the
	// scan. An error from visit stops the scan and is returned. The scan works
	// on a database opened read-only, returns an error on a database without
	// the turn tables, and fails with an error naming the run ID and Seq when a
	// stored end instant does not parse.
	ScanRunTurnsRange(ctx context.Context, since, until *time.Time, visit func(RunTurnRow) error) error
}
