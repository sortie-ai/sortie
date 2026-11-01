package storage

// UnrecordedRun is a run_history row whose cost was not recorded.
type UnrecordedRun struct {
	ID               int64
	AgentAdapter     string
	TokensMeasured   bool
	InputTokens      int64
	OutputTokens     int64
	TotalTokens      int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	UnaccountedTurns int
	ConfiguredModel  string
	ReportedModel    string

	// Parts holds the row's run_model_usage rows; nil when it has none.
	Parts map[string]ModelTokens
}

// RunCostRecord is the cost to record for one [UnrecordedRun]. CostStatus is
// stored as given, so an empty value leaves the row unrecorded.
type RunCostRecord struct {
	EstimatedCostUSD *float64
	CostStatus       string

	// NewParts are run_model_usage rows to insert, nil when the row has its own.
	NewParts map[string]ModelTokens

	PartCosts map[string]ModelCost
}
