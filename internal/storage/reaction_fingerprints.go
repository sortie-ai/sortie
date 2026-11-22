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

// ReactionEpisode is the persisted episode state of one reaction kind on an
// issue: its fingerprint row with the attempt counter of the episode it
// belongs to and how the last run dispatched for the fingerprint ended.
// Found is false when no row exists.
type ReactionEpisode struct {
	Fingerprint string
	Dispatched  bool
	Attempts    int
	LastExit    string
	Found       bool
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

	// GetReactionEpisode returns the row of the issue and kind with its
	// episode state.
	GetReactionEpisode(ctx context.Context, issueID, kind string) (ReactionEpisode, error)
	// SetReactionAttempts stores the episode attempt counter and clears the
	// recorded run exit. No-op when the row does not exist.
	SetReactionAttempts(ctx context.Context, issueID, kind string, attempts int) error
	// RecordReactionExit lifts the dispatched mark and records how the run
	// dispatched for the row's fingerprint ended. No-op when the row does
	// not exist.
	RecordReactionExit(ctx context.Context, issueID, kind, lastExit string) error
	// ReleaseDispatchedReactions lifts the dispatched mark of every row of
	// kind and reports how many it lifted. With keepTriaged, a row whose
	// fingerprint a triage verdict of handled or escalate consumed keeps it.
	ReleaseDispatchedReactions(ctx context.Context, kind string, keepTriaged bool) (int64, error)
}
