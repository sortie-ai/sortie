package storage

import (
	"context"
	"time"
)

// ReactionObservation is a persisted first-seen observation. Dispatched
// records whether the one-shot action associated with the observation has
// already been performed.
type ReactionObservation struct {
	FirstObservedAt time.Time
	Dispatched      bool
}

// ReactionFingerprintStore records the fingerprints and observations reactions
// saw on each issue, and whether each was dispatched.
type ReactionFingerprintStore interface {
	UpsertReactionFingerprint(ctx context.Context, issueID, kind, fingerprint string) error
	GetReactionFingerprint(ctx context.Context, issueID, kind string) (fingerprint string, dispatched bool, err error)
	MarkReactionDispatched(ctx context.Context, issueID, kind string) error
	DeleteReactionFingerprint(ctx context.Context, issueID, kind string) error
	DeleteReactionFingerprintsByIssue(ctx context.Context, issueID string) error
	UpsertReactionObservation(ctx context.Context, issueID, kind, fingerprint string, observedAt time.Time) (ReactionObservation, error)
	MarkReactionObservationDispatched(ctx context.Context, issueID, kind, fingerprint string) error
}
