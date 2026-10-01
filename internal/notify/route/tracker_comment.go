package route

import (
	"context"

	"github.com/sortie-ai/sortie/internal/domain"
)

// trackerComment posts an event's body as a comment on the issue its
// envelope names. It never renders the envelope, so a tracker comment
// carries no error text, session identifier, or agent kind.
type trackerComment struct {
	adapter domain.TrackerAdapter
}

func (c trackerComment) Send(ctx context.Context, n domain.Notification) error {
	return c.adapter.CommentIssue(ctx, n.Envelope.IssueID, n.Message.Body)
}
