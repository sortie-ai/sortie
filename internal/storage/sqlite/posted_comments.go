package sqlite

import (
	"context"
	"fmt"

	"github.com/sortie-ai/sortie/internal/storage"
)

var _ storage.PostedCommentStore = (*Store)(nil)

// AddPostedComment records commentID as posted on the issue. A pair already
// stored is ignored.
func (s *Store) AddPostedComment(ctx context.Context, issueID, commentID string) error {
	if _, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO posted_comments (issue_id, comment_id)
		VALUES (?, ?)`,
		issueID, commentID,
	); err != nil {
		return fmt.Errorf("add posted comment %q for issue %q: %w", commentID, issueID, err)
	}
	return nil
}

// ListPostedComments returns the comment IDs recorded as posted on the
// issue. Ordering is unspecified. Returns an empty slice (not nil) when none
// are recorded.
func (s *Store) ListPostedComments(ctx context.Context, issueID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT comment_id FROM posted_comments
		WHERE issue_id = ?`,
		issueID,
	)
	if err != nil {
		return nil, fmt.Errorf("list posted comments for issue %q: %w", issueID, err)
	}
	defer rows.Close() //nolint:errcheck // read-only query; close error is non-actionable

	commentIDs := []string{}
	for rows.Next() {
		var commentID string
		if err := rows.Scan(&commentID); err != nil {
			return nil, fmt.Errorf("scan posted comment for issue %q: %w", issueID, err)
		}
		commentIDs = append(commentIDs, commentID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list posted comments for issue %q: %w", issueID, err)
	}
	return commentIDs, nil
}
