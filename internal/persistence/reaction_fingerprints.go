package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ReactionObservation is a persisted first-seen observation. Dispatched
// records whether the one-shot action associated with the observation has
// already been performed.
type ReactionObservation struct {
	FirstObservedAt time.Time
	Dispatched      bool
}

// UpsertReactionFingerprint inserts or updates a reaction fingerprint.
// If the fingerprint value changes, dispatched is reset to 0.
func (s *Store) UpsertReactionFingerprint(ctx context.Context, issueID, kind, fingerprint string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO reaction_fingerprints (issue_id, kind, fingerprint, dispatched, updated_at)
		VALUES (?, ?, ?, 0, ?)
		ON CONFLICT (issue_id, kind) DO UPDATE SET
			fingerprint = CASE
				WHEN excluded.fingerprint != reaction_fingerprints.fingerprint
				THEN excluded.fingerprint
				ELSE reaction_fingerprints.fingerprint
			END,
			dispatched = CASE
				WHEN excluded.fingerprint != reaction_fingerprints.fingerprint
				THEN 0
				ELSE reaction_fingerprints.dispatched
			END,
			updated_at = excluded.updated_at`,
		issueID, kind, fingerprint, now,
	)
	return err
}

// UpsertReactionObservation inserts a first-seen observation or returns the
// existing one. Re-observing the same fingerprint preserves both its original
// timestamp and dispatched flag. A different fingerprint starts a new
// observation at observedAt and resets dispatched to false.
func (s *Store) UpsertReactionObservation(
	ctx context.Context,
	issueID, kind, fingerprint string,
	observedAt time.Time,
) (ReactionObservation, error) {
	observedAtText := observedAt.UTC().Format(time.RFC3339Nano)
	var (
		firstObservedAtText string
		dispatchedInt       int
	)
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO reaction_fingerprints (issue_id, kind, fingerprint, dispatched, updated_at)
		VALUES (?, ?, ?, 0, ?)
		ON CONFLICT (issue_id, kind) DO UPDATE SET
			fingerprint = excluded.fingerprint,
			dispatched = CASE
				WHEN excluded.fingerprint != reaction_fingerprints.fingerprint
				THEN 0
				ELSE reaction_fingerprints.dispatched
			END,
			updated_at = CASE
				WHEN excluded.fingerprint != reaction_fingerprints.fingerprint
				THEN excluded.updated_at
				ELSE reaction_fingerprints.updated_at
			END
		RETURNING updated_at, dispatched`,
		issueID, kind, fingerprint, observedAtText,
	).Scan(&firstObservedAtText, &dispatchedInt)
	if err != nil {
		return ReactionObservation{}, err
	}

	firstObservedAt, err := time.Parse(time.RFC3339Nano, firstObservedAtText)
	if err != nil {
		return ReactionObservation{}, fmt.Errorf("parse reaction observation timestamp: %w", err)
	}
	return ReactionObservation{
		FirstObservedAt: firstObservedAt,
		Dispatched:      dispatchedInt != 0,
	}, nil
}

// MarkReactionObservationDispatched sets dispatched=1 without changing the
// observation's first-seen timestamp, but only while the row still carries
// the expected fingerprint. It is a no-op when the row is absent or has been
// replaced by a newer observation.
func (s *Store) MarkReactionObservationDispatched(ctx context.Context, issueID, kind, fingerprint string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE reaction_fingerprints SET dispatched = 1
		WHERE issue_id = ? AND kind = ? AND fingerprint = ?`,
		issueID, kind, fingerprint,
	)
	return err
}

// GetReactionFingerprint returns the stored fingerprint and dispatched
// flag for the given issue and kind. Returns ("", false, nil) when no
// row exists.
func (s *Store) GetReactionFingerprint(ctx context.Context, issueID, kind string) (fingerprint string, dispatched bool, err error) {
	var dispatchedInt int
	err = s.db.QueryRowContext(ctx, `
		SELECT fingerprint, dispatched FROM reaction_fingerprints
		WHERE issue_id = ? AND kind = ?`,
		issueID, kind,
	).Scan(&fingerprint, &dispatchedInt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return fingerprint, dispatchedInt != 0, nil
}

// MarkReactionDispatched sets dispatched=1 for the given issue and kind.
// No-op if the row does not exist.
func (s *Store) MarkReactionDispatched(ctx context.Context, issueID, kind string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		UPDATE reaction_fingerprints SET dispatched = 1, updated_at = ?
		WHERE issue_id = ? AND kind = ?`,
		now, issueID, kind,
	)
	return err
}

// DeleteReactionFingerprint removes the fingerprint row for the given
// issue and kind.
func (s *Store) DeleteReactionFingerprint(ctx context.Context, issueID, kind string) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM reaction_fingerprints WHERE issue_id = ? AND kind = ?`,
		issueID, kind,
	)
	return err
}

// DeleteReactionFingerprintsByIssue removes all fingerprint rows for
// the given issue (all kinds).
func (s *Store) DeleteReactionFingerprintsByIssue(ctx context.Context, issueID string) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM reaction_fingerprints WHERE issue_id = ?`,
		issueID,
	)
	return err
}
