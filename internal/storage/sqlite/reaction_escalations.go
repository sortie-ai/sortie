package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sortie-ai/sortie/internal/storage"
)

var _ storage.ReactionEscalationStore = (*Store)(nil)

const reactionEscalationColumns = `issue_id, kind, subject, escalated_at, escalation_trigger,
	attempts, pr_number, pr_owner, pr_repo, pr_branch`

// GetReactionEscalation returns the record of the given issue and kind.
// The bool is false when no row exists.
func (s *Store) GetReactionEscalation(ctx context.Context, issueID, kind string) (storage.ReactionEscalation, bool, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT `+reactionEscalationColumns+`
		FROM reaction_escalations
		WHERE issue_id = ? AND kind = ?`,
		issueID, kind,
	)
	rec, err := scanReactionEscalation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return storage.ReactionEscalation{}, false, nil
	}
	if err != nil {
		return storage.ReactionEscalation{}, false, err
	}
	return rec, true, nil
}

// SaveReactionEscalation inserts the record or replaces the row with the
// same issue and kind.
func (s *Store) SaveReactionEscalation(ctx context.Context, rec storage.ReactionEscalation) error {
	var escalatedAt sql.NullString
	if rec.EscalatedAt != nil {
		escalatedAt = sql.NullString{String: rec.EscalatedAt.UTC().Format(time.RFC3339Nano), Valid: true}
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO reaction_escalations (`+reactionEscalationColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (issue_id, kind) DO UPDATE SET
			subject = excluded.subject,
			escalated_at = excluded.escalated_at,
			escalation_trigger = excluded.escalation_trigger,
			attempts = excluded.attempts,
			pr_number = excluded.pr_number,
			pr_owner = excluded.pr_owner,
			pr_repo = excluded.pr_repo,
			pr_branch = excluded.pr_branch`,
		rec.IssueID, rec.Kind, rec.Subject, escalatedAt, rec.Trigger,
		rec.Attempts, rec.PRNumber, rec.PROwner, rec.PRRepo, rec.PRBranch,
	)
	return err
}

// EndReactionEscalation clears escalated_at of the given issue and kind
// and keeps the row. It is a no-op when the row does not exist.
func (s *Store) EndReactionEscalation(ctx context.Context, issueID, kind string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE reaction_escalations SET escalated_at = NULL
		WHERE issue_id = ? AND kind = ?`,
		issueID, kind,
	)
	return err
}

// DeleteReactionEscalations removes every record of the given issue, all
// kinds.
func (s *Store) DeleteReactionEscalations(ctx context.Context, issueID string) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM reaction_escalations WHERE issue_id = ?`,
		issueID,
	)
	return err
}

// ListStandingReactionEscalations returns the records whose escalation
// has not ended, ordered by issue and kind. It returns an empty non-nil
// slice when none stand.
func (s *Store) ListStandingReactionEscalations(ctx context.Context) ([]storage.ReactionEscalation, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+reactionEscalationColumns+`
		FROM reaction_escalations
		WHERE escalated_at IS NOT NULL
		ORDER BY issue_id, kind`)
	if err != nil {
		return nil, fmt.Errorf("list standing reaction escalations: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only query; close error is non-actionable

	records := []storage.ReactionEscalation{}
	for rows.Next() {
		rec, err := scanReactionEscalation(rows)
		if err != nil {
			return nil, fmt.Errorf("list standing reaction escalations: %w", err)
		}
		records = append(records, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list standing reaction escalations: %w", err)
	}
	return records, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanReactionEscalation(row rowScanner) (storage.ReactionEscalation, error) {
	var (
		rec         storage.ReactionEscalation
		escalatedAt sql.NullString
	)
	if err := row.Scan(
		&rec.IssueID, &rec.Kind, &rec.Subject, &escalatedAt, &rec.Trigger,
		&rec.Attempts, &rec.PRNumber, &rec.PROwner, &rec.PRRepo, &rec.PRBranch,
	); err != nil {
		return storage.ReactionEscalation{}, err
	}
	if escalatedAt.Valid {
		at, err := time.Parse(time.RFC3339Nano, escalatedAt.String)
		if err != nil {
			return storage.ReactionEscalation{}, fmt.Errorf("parse reaction escalation time: %w", err)
		}
		rec.EscalatedAt = &at
	}
	return rec, nil
}
