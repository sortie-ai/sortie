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

	// RunCostUSD is the run's recorded cost; nil when it has none.
	RunCostUSD *float64

	// RunAgentPricedUSD is the recorded cost of the run's parts priced from an
	// agent-reported amount; nil when no part is.
	RunAgentPricedUSD *float64

	Turn RunTurn
}

// UntrackedRunRow is a finished run recorded before the store kept turn
// records, with the fields of its run_history row. A field the schema does not
// carry holds its zero value.
type UntrackedRunRow struct {
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

	// CompletedAt is the run's completion instant in UTC.
	CompletedAt time.Time

	TokensMeasured   bool
	InputTokens      int64
	OutputTokens     int64
	TotalTokens      int64
	CacheReadTokens  int64
	CacheWriteTokens int64

	// EstimatedCostUSD is the run's recorded cost; nil means no cost figure.
	EstimatedCostUSD *float64
	CostStatus       string
	UnaccountedTurns int

	// AgentPricedUSD is the recorded cost of the run's parts priced from an
	// agent-reported amount; nil when no part is.
	AgentPricedUSD *float64
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

	// ScanUntrackedRunsRange visits every finished run that has no turn
	// records because it was recorded before the store kept them. A run is
	// visited if and only if its completed_at is at or after since and before
	// until (a nil bound is open), its status is not ci_failed, and either the
	// database has no turn tables or the run has no turn record and completed
	// before the turn tables were created. Visits come in ascending order of
	// CompletedAt, then RunID. caps selects the projection as
	// [RunHistoryStore.ScanRunHistoryRange] does.
	//
	// visit MUST NOT call the store, which may hold its only connection for the
	// scan. An error from visit stops the scan and is returned. The scan works
	// on a database opened read-only and fails with an error when the creation
	// time of the turn tables does not parse.
	ScanUntrackedRunsRange(ctx context.Context, caps RunHistoryCapabilities, since, until *time.Time, visit func(UntrackedRunRow) error) error
}
