package storage

import (
	"context"
)

// Issue outcome kinds.
const (
	IssueOutcomeHandedOff = "handed_off"
	IssueOutcomeMerged    = "merged"
	IssueOutcomeTerminal  = "terminal"
)

// IssueOutcome is how an issue's latest work ended, as the orchestrator
// observed it. ObservedAt is RFC 3339 in UTC. State is empty for a merged
// outcome; MergeReactionKind, PRNumber, and PRRepository are set only for a
// merged outcome.
type IssueOutcome struct {
	IssueID           string
	Kind              string
	ObservedAt        string
	State             string
	MergeReactionKind string
	PRNumber          int
	PRRepository      string
}

// IssueOutcomeStore records the outcome observed for each issue.
type IssueOutcomeStore interface {
	RecordIssueOutcome(ctx context.Context, o IssueOutcome) error
	DeleteIssueOutcome(ctx context.Context, issueID string) error
	IssueOutcomes(ctx context.Context, issueIDs []string) (map[string]IssueOutcome, error)
}
