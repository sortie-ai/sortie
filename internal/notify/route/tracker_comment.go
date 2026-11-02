package route

import (
	"context"
	"log/slog"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/notify/render"
)

// trackerComment posts an event's public rendering as a comment on the
// issue its envelope names and records the identity of the created comment.
type trackerComment struct {
	adapter domain.TrackerAdapter
	posted  PostedCommentRecorder
	// register, when non-nil, counts this send as in flight until it returns.
	register *postingRegister
	logger   *slog.Logger
}

// Send returns nil whenever the post succeeded, even if recording its
// identity failed: the comment exists, and a failed delivery would make a
// producer post it again.
func (c trackerComment) Send(ctx context.Context, n domain.Notification) error {
	if c.register != nil {
		c.register.begin(n.Envelope.IssueID)
		defer c.register.end(n.Envelope.IssueID)
	}
	commentID, err := c.adapter.CommentIssue(ctx, n.Envelope.IssueID, render.Notification(n).Public)
	if err != nil {
		return err
	}
	if c.posted == nil {
		return nil
	}
	if err := c.posted.AddPostedComment(ctx, n.Envelope.IssueID, commentID); err != nil {
		logging.WithIssue(c.logger, n.Envelope.IssueID, n.Envelope.Identifier).Error("failed to record posted tracker comment",
			slog.String("event_type", string(n.Envelope.EventType)),
			slog.String("comment_id", commentID),
			slog.Any("error", err))
	}
	return nil
}
