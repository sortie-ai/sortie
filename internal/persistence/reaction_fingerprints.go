package persistence

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

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
