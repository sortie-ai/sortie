package sqlite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/sortie-ai/sortie/internal/storage"
)

var _ storage.GivenCommentStore = (*Store)(nil)

// RecordGivenComments implements storage.GivenCommentStore.
func (s *Store) RecordGivenComments(ctx context.Context, issueID string, comments []storage.GivenComment) error {
	if len(comments) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin record given comments for issue %q: %w", issueID, err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback on error path; no-op after commit

	for _, c := range comments {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO given_comments
				(issue_id, conversation_id, source, comment_id, body_sha256, channel, given_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (issue_id, conversation_id, source, comment_id) DO UPDATE SET
				body_sha256 = excluded.body_sha256,
				channel     = excluded.channel,
				given_at    = excluded.given_at`,
			issueID, c.ConversationID, c.Source, c.CommentID,
			hex.EncodeToString(c.BodySHA256[:]), c.Channel, c.GivenAt.UTC().Format(time.RFC3339Nano),
		); err != nil {
			return fmt.Errorf("record given comment %q for issue %q conversation %q: %w", c.CommentID, issueID, c.ConversationID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit record given comments for issue %q: %w", issueID, err)
	}
	return nil
}

// ListGivenComments implements storage.GivenCommentStore.
func (s *Store) ListGivenComments(ctx context.Context, issueID, conversationID string) ([]storage.GivenComment, error) {
	return s.queryGivenComments(ctx, issueID, conversationID, `
		SELECT conversation_id, source, comment_id, body_sha256, channel, given_at
		FROM given_comments
		WHERE issue_id = ? AND conversation_id = ?`)
}

// ListEarlierGivenComments implements storage.GivenCommentStore.
func (s *Store) ListEarlierGivenComments(ctx context.Context, issueID, conversationID string) ([]storage.GivenComment, error) {
	return s.queryGivenComments(ctx, issueID, conversationID, `
		SELECT conversation_id, source, comment_id, body_sha256, channel, given_at
		FROM given_comments
		WHERE issue_id = ? AND conversation_id <> ?`)
}

func (s *Store) queryGivenComments(ctx context.Context, issueID, conversationID, query string) ([]storage.GivenComment, error) {
	rows, err := s.db.QueryContext(ctx, query, issueID, conversationID)
	if err != nil {
		return nil, fmt.Errorf("list given comments for issue %q conversation %q: %w", issueID, conversationID, err)
	}
	defer rows.Close() //nolint:errcheck // read-only query; close error is non-actionable

	comments := []storage.GivenComment{}
	for rows.Next() {
		var (
			c       storage.GivenComment
			hash    string
			givenAt string
		)
		if err := rows.Scan(&c.ConversationID, &c.Source, &c.CommentID, &hash, &c.Channel, &givenAt); err != nil {
			return nil, fmt.Errorf("scan given comment for issue %q conversation %q: %w", issueID, conversationID, err)
		}

		decoded, err := hex.DecodeString(hash)
		if err != nil {
			return nil, fmt.Errorf("decode hash of given comment %q for issue %q conversation %q: %w", c.CommentID, issueID, conversationID, err)
		}
		if len(decoded) != sha256.Size {
			return nil, fmt.Errorf("decode hash of given comment %q for issue %q conversation %q: got %d bytes, want %d", c.CommentID, issueID, conversationID, len(decoded), sha256.Size)
		}
		copy(c.BodySHA256[:], decoded)

		parsed, err := time.Parse(time.RFC3339Nano, givenAt)
		if err != nil {
			return nil, fmt.Errorf("parse time of given comment %q for issue %q conversation %q: %w", c.CommentID, issueID, conversationID, err)
		}
		c.GivenAt = parsed.UTC()

		comments = append(comments, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list given comments for issue %q conversation %q: %w", issueID, conversationID, err)
	}
	return comments, nil
}
