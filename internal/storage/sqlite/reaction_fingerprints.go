package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sortie-ai/sortie/internal/storage"
)

var _ storage.ReactionFingerprintStore = (*Store)(nil)

// UpsertReactionFingerprint inserts or updates a reaction fingerprint.
// If the fingerprint value changes, dispatched is reset to 0 and the
// recorded run exit is cleared; the episode attempt counter is kept.
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
			last_exit = CASE
				WHEN excluded.fingerprint != reaction_fingerprints.fingerprint
				THEN ''
				ELSE reaction_fingerprints.last_exit
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
) (storage.ReactionObservation, error) {
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
		return storage.ReactionObservation{}, err
	}

	firstObservedAt, err := time.Parse(time.RFC3339Nano, firstObservedAtText)
	if err != nil {
		return storage.ReactionObservation{}, fmt.Errorf("parse reaction observation timestamp: %w", err)
	}
	return storage.ReactionObservation{
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

// GetReactionEpisode returns the fingerprint row of the issue and kind with
// its episode attempt counter and recorded run exit. Found is false when no
// row exists.
func (s *Store) GetReactionEpisode(ctx context.Context, issueID, kind string) (storage.ReactionEpisode, error) {
	var (
		episode       storage.ReactionEpisode
		dispatchedInt int
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT fingerprint, dispatched, attempts, last_exit FROM reaction_fingerprints
		WHERE issue_id = ? AND kind = ?`,
		issueID, kind,
	).Scan(&episode.Fingerprint, &dispatchedInt, &episode.Attempts, &episode.LastExit)
	if errors.Is(err, sql.ErrNoRows) {
		return storage.ReactionEpisode{}, nil
	}
	if err != nil {
		return storage.ReactionEpisode{}, err
	}
	episode.Dispatched = dispatchedInt != 0
	episode.Found = true
	return episode, nil
}

// SetReactionAttempts stores the episode attempt counter and clears the
// recorded run exit. No-op if the row does not exist.
func (s *Store) SetReactionAttempts(ctx context.Context, issueID, kind string, attempts int) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE reaction_fingerprints SET attempts = ?, last_exit = ''
		WHERE issue_id = ? AND kind = ?`,
		attempts, issueID, kind,
	)
	return err
}

// RecordReactionExit sets dispatched=0 and records how the run dispatched
// for the row's fingerprint ended. No-op if the row does not exist.
func (s *Store) RecordReactionExit(ctx context.Context, issueID, kind, lastExit string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		UPDATE reaction_fingerprints SET dispatched = 0, last_exit = ?, updated_at = ?
		WHERE issue_id = ? AND kind = ?`,
		lastExit, now, issueID, kind,
	)
	return err
}

// ReleaseDispatchedReactions sets dispatched=0 on every row of kind and
// returns the number of rows changed. With keepTriaged, a row whose
// fingerprint has a recorded triage outcome of handled or escalate is left
// marked.
func (s *Store) ReleaseDispatchedReactions(ctx context.Context, kind string, keepTriaged bool) (int64, error) {
	query := `
		UPDATE reaction_fingerprints SET dispatched = 0
		WHERE kind = ? AND dispatched = 1`
	if keepTriaged {
		query += `
		AND NOT EXISTS (
			SELECT 1 FROM reaction_triage_outcomes t
			WHERE t.issue_id = reaction_fingerprints.issue_id
			AND t.kind = reaction_fingerprints.kind
			AND t.fingerprint = reaction_fingerprints.fingerprint
			AND t.disposition IN ('handled', 'escalate')
		)`
	}
	res, err := s.db.ExecContext(ctx, query, kind)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
