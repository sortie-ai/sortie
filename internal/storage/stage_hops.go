package storage

import (
	"context"
)

// StageHop is one stored record of an issue's consecutive automatic stage
// hops since the last reset, and the latest hop.
type StageHop struct {
	IssueID          string
	Identifier       string
	HopCount         int
	SourceRule       string
	TargetRule       string
	TargetLabel      string
	PreviousOutcome  string // "succeeded" or "no_change"
	SourceDispatchID string // dispatch of the run whose exit made the hop
	TargetObserved   bool   // a read has shown TargetLabel on the issue
	HoppedAt         string // RFC 3339
	ChainID          string // chain of the run whose exit made the hop; empty for a hop stored before the field existed

	// DisplayID, IssueTitle, and IssueURL name the issue to a person; each
	// is empty when the hop was recorded without a read of the issue.
	DisplayID  string
	IssueTitle string
	IssueURL   string

	// IssueType, IssueLabels, and IssueState are the facts about the issue
	// the hop leaves; IssueLabels is nil when the hop was recorded without a
	// read of the labels.
	IssueType   string
	IssueLabels []string
	IssueState  string

	// DispatchedAt is the RFC 3339 start of the first run of the pass
	// ChainID names; empty when the pass could not be dated.
	DispatchedAt string

	// RunDispatched is true once a run of the issue was dispatched after
	// the hop.
	RunDispatched bool
}

// StageHopStore records the stage hop of each issue.
type StageHopStore interface {
	// RecordStageHop stores hop and deletes the retry entry of hop.IssueID in
	// one write: both happen or neither does.
	RecordStageHop(ctx context.Context, hop StageHop) error
	MarkStageHopObserved(ctx context.Context, issueID string) error
	MarkStageHopDispatched(ctx context.Context, issueID string) error
	DeleteStageHop(ctx context.Context, issueID string) error
	ListStageHops(ctx context.Context) ([]StageHop, error)
}
