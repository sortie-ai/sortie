package storage

import (
	"context"
	"crypto/sha256"
	"time"
)

// GivenComment is one comment a conversation was given, in the version
// identified by BodySHA256.
type GivenComment struct {
	ConversationID string
	Source         string
	CommentID      string
	BodySHA256     [sha256.Size]byte
	Channel        string
	GivenAt        time.Time
}

// GivenCommentStore records which comments each agent conversation on an
// issue was given. Rows are never deleted, and the store validates no value.
type GivenCommentStore interface {
	// RecordGivenComments stores all comments or none. A stored key takes the
	// new hash, channel and time, so an edited comment replaces its earlier
	// version.
	RecordGivenComments(ctx context.Context, issueID string, comments []GivenComment) error
	// ListGivenComments returns the conversation's rows in no particular order.
	ListGivenComments(ctx context.Context, issueID, conversationID string) ([]GivenComment, error)
	// ListEarlierGivenComments returns the rows of every other conversation on
	// the issue in no particular order.
	ListEarlierGivenComments(ctx context.Context, issueID, conversationID string) ([]GivenComment, error)
}
