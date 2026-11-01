package storage

import (
	"context"
)

// Live pass kinds. A running stage session and a budget hold are stored; a
// queued retry is reported by [LivePassStore.ListLivePasses] from the stored
// [RetryEntry] records.
const (
	LivePassRunning = "running" // a running stage session
	LivePassBudget  = "budget"  // a budget hold
	LivePassRetry   = "retry"   // a [RetryEntry]; ListLivePasses reports it, nothing stores it
)

// LivePass is a stage pass the orchestrator holds outside finished runs: the
// issue's chain and rule, and the rule whose hop reached it (empty when none
// did).
type LivePass struct {
	IssueID       string
	Kind          string
	ChainID       string
	RuleName      string
	StagePrevious string
}

// LivePassStore records the stage passes held outside finished runs, which the
// stats report counts.
type LivePassStore interface {
	UpsertLivePass(ctx context.Context, pass LivePass) error
	DeleteLivePass(ctx context.Context, issueID, kind string) error
	DeleteAllLivePasses(ctx context.Context) error
	// ListLivePasses returns the stored live passes and, as kind
	// [LivePassRetry], every retry entry, all read at one instant.
	ListLivePasses(ctx context.Context) ([]LivePass, error)
}
