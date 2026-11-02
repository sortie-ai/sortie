package storage

import (
	"context"
)

// ParkedIssue is one stored park: an issue held out of
// primary dispatch until the orchestrator observes that a person acted.
type ParkedIssue struct {
	IssueID      string
	Identifier   string
	DisplayID    string // empty when the tracker needs no qualified form
	Reason       string // "agent_blocked" or "handoff_absence"
	ParkedState  string // tracker state observed at park time; empty when unobserved
	Label        string // parking label the orchestrator applied
	LabelApplied bool
	ParkedAt     string // RFC 3339
	IssueTitle   string // tracker title as read for the park; empty when unread
	IssueURL     string // tracker web link as read for the park; empty when unread

	// IssueType and IssueLabels are the tracker's issue type and labels as
	// read for the park; IssueLabels is nil when the labels were never read.
	IssueType   string
	IssueLabels []string

	// RuleName, AgentKind, ConfiguredModel, StagePrevious,
	// StagePreviousOutcome, ChainID, and DispatchedAt describe the run the
	// park follows. All are empty when the park records no run.
	RuleName             string
	AgentKind            string
	ConfiguredModel      string
	StagePrevious        string
	StagePreviousOutcome string
	ChainID              string
	DispatchedAt         string // RFC 3339 with nanoseconds, UTC

	// SessionID, TemplateID, Statement, and LastPromptAt are what the park
	// hands to the first run after a release; each is empty when the park
	// recorded none. TemplateID is empty for the workflow body template.
	// LastPromptAt is the moment the run began its last turn, RFC 3339 with
	// nanoseconds, UTC.
	SessionID    string
	TemplateID   string
	Statement    string
	LastPromptAt string

	// ReleasedAt (RFC 3339 UTC, whole seconds) and ReleaseGesture
	// ("label_removed" or "state_changed") record a person's release; both
	// are empty while the park holds.
	ReleasedAt     string
	ReleaseGesture string
}

// ParkedIssueStore records the parked issues.
type ParkedIssueStore interface {
	UpsertParkedIssue(ctx context.Context, entry ParkedIssue) error
	MarkParkedIssueLabelApplied(ctx context.Context, issueID string) error
	DeleteParkedIssue(ctx context.Context, issueID string) error
	ListParkedIssues(ctx context.Context) ([]ParkedIssue, error)
}
