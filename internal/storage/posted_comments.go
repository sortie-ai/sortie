package storage

import (
	"context"
)

// PostedCommentStore records the identity of each tracker comment Sortie
// posts, per issue.
//
// AddPostedComment records the pair; a pair already recorded is ignored and
// returns nil. ListPostedComments returns the identities recorded for the
// issue in no particular order, and an empty non-nil slice when there are
// none. A comment is listed on its issue from creation, but its row exists
// only once the delivery that posted it has returned.
type PostedCommentStore interface {
	AddPostedComment(ctx context.Context, issueID, commentID string) error
	ListPostedComments(ctx context.Context, issueID string) ([]string, error)
}
