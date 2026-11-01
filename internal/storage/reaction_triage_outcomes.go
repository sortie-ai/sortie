package storage

import (
	"context"
	"time"
)

// ReactionTriageOutcome is one triage answer a reaction applied to one
// subject of one issue.
type ReactionTriageOutcome struct {
	IssueID, Kind, Fingerprint string
	Disposition, Fallback      string
	Elapsed                    time.Duration
	ChainID                    string
	RecordedAt                 time.Time
}

// ReactionTriageOutcomeStore records the triage outcomes of reactions.
type ReactionTriageOutcomeStore interface {
	RecordReactionTriageOutcome(ctx context.Context, outcome ReactionTriageOutcome) error
	LatestReactionTriageDisposition(ctx context.Context, issueID, kind, fingerprint string) (disposition string, found bool, err error)
}
