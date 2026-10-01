package route

import (
	"context"

	"github.com/sortie-ai/sortie/internal/domain"
)

// trackerComment posts an event's body as a comment on the issue its
// envelope names. It never renders the envelope, so a tracker comment
// carries no error text, session identifier, or agent kind. Agent-authored
// text travels apart from the body, as the tracker's literal block.
type trackerComment struct {
	adapter domain.TrackerAdapter
}

func (c trackerComment) Send(ctx context.Context, n domain.Notification) error {
	if n.Message.AgentText != "" {
		return c.adapter.CommentIssueWithLiteral(ctx, n.Envelope.IssueID, n.Message.Body, n.Message.AgentText)
	}
	return c.adapter.CommentIssue(ctx, n.Envelope.IssueID, n.Message.Body)
}
