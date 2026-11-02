package orchestrator

import (
	"context"
	"log/slog"
	"slices"

	"github.com/sortie-ai/sortie/internal/commentfeed"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/prompt"
	"github.com/sortie-ai/sortie/internal/storage"
)

// dispatchCommentRead wraps a dispatch's comment read so that a read of zero
// comments stays distinguishable from no read.
type dispatchCommentRead struct {
	comments []domain.Comment
}

type dispatchCommentReadCtxKey struct{}

func withDispatchCommentRead(ctx context.Context, comments []domain.Comment) context.Context {
	return context.WithValue(ctx, dispatchCommentReadCtxKey{}, dispatchCommentRead{comments: comments})
}

func dispatchCommentReadFrom(ctx context.Context) ([]domain.Comment, bool) {
	read, ok := ctx.Value(dispatchCommentReadCtxKey{}).(dispatchCommentRead)
	return read.comments, ok
}

// commentsGivenMsg carries the comments a worker's turns handed to the agent
// for recording through commentsGivenCh. DispatchID keeps a report of an
// earlier run from landing on a later run of the same issue.
type commentsGivenMsg struct {
	IssueID    string
	DispatchID string
	Rows       []storage.GivenComment
}

// applyCommentsGiven records a worker's delivery report, the only write to
// the given-comment record. A cleanup-pending run still records, because its
// agent received the prompt. A report of a run that no longer holds the issue
// is dropped.
func (o *Orchestrator) applyCommentsGiven(ctx context.Context, msg commentsGivenMsg) {
	if len(msg.Rows) == 0 {
		return
	}
	entry, ok := o.state.Running[msg.IssueID]
	if !ok || entry.DispatchID != msg.DispatchID {
		o.logger.Debug("dropping given comments of a run that no longer holds the issue",
			slog.String("issue_id", msg.IssueID),
			slog.Int("comments", len(msg.Rows)),
		)
		return
	}
	if err := o.store.RecordGivenComments(ctx, msg.IssueID, msg.Rows); err != nil {
		logging.WithIssue(o.logger, msg.IssueID, entry.Issue.Identifier).Error("failed to record given comments",
			slog.String("conversation_id", msg.Rows[0].ConversationID),
			slog.Int("comments", len(msg.Rows)),
			slog.Any("error", err),
		)
	}
}

// nonNilComments returns comments, or an empty non-nil slice when comments is
// nil, so a completed read of zero comments stays distinguishable from no read.
func nonNilComments(comments []domain.Comment) []domain.Comment {
	if comments == nil {
		return []domain.Comment{}
	}
	return comments
}

// commentFeedContextOf maps the views of sel to the comment_feed template
// variable. Only a comment that replaces a version the conversation holds
// carries replacing.
func commentFeedContextOf(sel commentfeed.Selection) prompt.CommentFeedContext {
	return prompt.CommentFeedContext{
		New:     pendingTemplateMaps(sel.New),
		Earlier: pendingTemplateMaps(sel.Earlier),
	}
}

func pendingTemplateMaps(pending []commentfeed.Pending) []map[string]any {
	maps := domain.CommentTemplateMaps(pendingComments(pending))
	for i, p := range pending {
		maps[i]["replacing"] = p.Replacing
	}
	return maps
}

// pendingTemplateComments returns the comments of sel as .park.comments
// lists them: the earlier ones first, then the new ones. The result is never
// nil.
func pendingTemplateComments(sel commentfeed.Selection) []map[string]any {
	return domain.CommentTemplateMaps(pendingComments(slices.Concat(sel.Earlier, sel.New)))
}

func pendingComments(pending []commentfeed.Pending) []domain.Comment {
	comments := make([]domain.Comment, len(pending))
	for i, p := range pending {
		comments[i] = p.Comment
	}
	return comments
}
