package storage

import (
	"context"
	"time"
)

// ReactionEscalation is the durable record of one reaction kind's
// escalation on an issue. EscalatedAt is nil once the escalation ended;
// the row then keeps Subject so a later escalation of the same subject
// applies no second action.
type ReactionEscalation struct {
	IssueID     string
	Kind        string
	Subject     string
	EscalatedAt *time.Time
	Trigger     string
	Attempts    int
	PRNumber    int
	PROwner     string
	PRRepo      string
	PRBranch    string
}

// ReactionEscalationStore records the reaction escalations of each issue.
type ReactionEscalationStore interface {
	GetReactionEscalation(ctx context.Context, issueID, kind string) (ReactionEscalation, bool, error)
	SaveReactionEscalation(ctx context.Context, rec ReactionEscalation) error
	EndReactionEscalation(ctx context.Context, issueID, kind string) error
	DeleteReactionEscalations(ctx context.Context, issueID string) error
	ListStandingReactionEscalations(ctx context.Context) ([]ReactionEscalation, error)
}
