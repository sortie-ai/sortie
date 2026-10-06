package persistence

import (
	"context"
	"fmt"
)

// AddReactionHandedOffComments records commentIDs in the handed-off set of
// the issue and kind in one transaction. IDs already stored are ignored and
// an empty slice writes nothing.
func (s *Store) AddReactionHandedOffComments(ctx context.Context, issueID, kind string, commentIDs []string) error {
	if len(commentIDs) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin add handed-off comments for issue %q kind %q: %w", issueID, kind, err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback on error path; no-op after commit

	for _, commentID := range commentIDs {
		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO reaction_handoffs (issue_id, kind, comment_id)
			VALUES (?, ?, ?)`,
			issueID, kind, commentID,
		); err != nil {
			return fmt.Errorf("add handed-off comment %q for issue %q kind %q: %w", commentID, issueID, kind, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit add handed-off comments for issue %q kind %q: %w", issueID, kind, err)
	}
	return nil
}

// ListReactionHandedOffComments returns the comment IDs in the handed-off
// set of the issue and kind. Ordering is unspecified. Returns an empty slice
// (not nil) when the set is empty.
func (s *Store) ListReactionHandedOffComments(ctx context.Context, issueID, kind string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT comment_id FROM reaction_handoffs
		WHERE issue_id = ? AND kind = ?`,
		issueID, kind,
	)
	if err != nil {
		return nil, fmt.Errorf("list handed-off comments for issue %q kind %q: %w", issueID, kind, err)
	}
	defer rows.Close() //nolint:errcheck // read-only query; close error is non-actionable

	commentIDs := []string{}
	for rows.Next() {
		var commentID string
		if err := rows.Scan(&commentID); err != nil {
			return nil, fmt.Errorf("scan handed-off comment for issue %q kind %q: %w", issueID, kind, err)
		}
		commentIDs = append(commentIDs, commentID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list handed-off comments for issue %q kind %q: %w", issueID, kind, err)
	}
	return commentIDs, nil
}
