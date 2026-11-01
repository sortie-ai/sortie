package storage

import (
	"context"
	"time"
)

// Activity values of a [ReactionActivity].
const (
	ReactionActivityWatch        = "watch"
	ReactionActivityContinuation = "continuation"
	ReactionActivityAction       = "action"
	ReactionActivityEscalation   = "escalation"
)

// ReactionActivity is one thing a pull request reaction did in one pass of an
// issue.
type ReactionActivity struct {
	IssueID    string
	ChainID    string
	Kind       string
	Activity   string
	RecordedAt time.Time
}

// PassRef names one pass of one issue and when its first run was dispatched.
type PassRef struct {
	IssueID      string
	ChainID      string
	DispatchedAt time.Time // zero for an undated pass
}

// PassKey keys a pass in a PassActivities result.
type PassKey struct {
	IssueID string
	ChainID string
}

// KindActivity sums what one reaction kind recorded in one pass.
type KindActivity struct {
	Watched       bool
	Continuations int
	Acted         bool // an action row exists
	Escalated     bool
}

// PassActivity is what the reactions recorded in one pass. Recorded is false
// for a pass without a chain identifier or a dispatch time, or dispatched
// before the second reaction activity began to be recorded; Kinds is then
// empty.
type PassActivity struct {
	Recorded bool
	Kinds    map[string]KindActivity // keyed by reaction kind
}

// ReactionActivityStore records what the reactions did in each pass.
type ReactionActivityStore interface {
	RecordReactionActivity(ctx context.Context, records []ReactionActivity) error
	PassActivities(ctx context.Context, passes []PassRef) (map[PassKey]PassActivity, error)
}
