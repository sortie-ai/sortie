package commentfeed

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/sortie-ai/sortie/internal/domain"
)

// PostingWaiter blocks until the comments Sortie is posting to an issue have
// been recorded.
type PostingWaiter interface {
	// AwaitTrackerComments returns nil once no tracker comment post for the
	// issue is in flight, or ctx.Err() when ctx ends first.
	AwaitTrackerComments(ctx context.Context, issueID string) error
}

// TrackerSourceParams configures [NewTrackerSource].
type TrackerSourceParams struct {
	Tracker     domain.TrackerAdapter
	TrackerKind string
	Store       Store

	// Posting is nil when nothing posts comments for the process.
	Posting PostingWaiter

	// DispatchRead is the comment list the dispatch already read; the first
	// Read returns it without a tracker request when HasDispatchRead is set.
	DispatchRead    []domain.Comment
	HasDispatchRead bool

	// AccessDenied is shared by every run of the process and may be nil.
	AccessDenied *atomic.Bool
	Logger       *slog.Logger
}

// NewTrackerSource returns the [Source] of an issue's tracker comments.
func NewTrackerSource(p TrackerSourceParams) Source {
	logger := p.Logger
	if logger == nil {
		logger = slog.Default()
	}
	s := &trackerSource{TrackerSourceParams: p, logger: logger}
	s.changes, _ = p.Tracker.(domain.CommentChangeReader)
	s.kinds, _ = p.Tracker.(domain.CommentAuthorKindReader)
	s.access, _ = p.Tracker.(domain.CommentAuthorAccessReader)
	return s
}

type trackerSource struct {
	TrackerSourceParams
	logger  *slog.Logger
	changes domain.CommentChangeReader
	kinds   domain.CommentAuthorKindReader
	access  domain.CommentAuthorAccessReader

	consumed bool
	denied   bool
}

func (s *trackerSource) Name() string { return SourceTracker }

// Read lists the posted-comment record only after the comment read and the
// wait for posts in flight: a Sortie comment the read lists comes from a post
// that began before the read returned, and its record exists once that post
// returns.
func (s *trackerSource) Read(ctx context.Context, issueID string, since domain.CommentReadPoint) (SourceRead, error) {
	var comments []domain.Comment
	var next domain.CommentReadPoint
	switch {
	case s.HasDispatchRead && !s.consumed:
		s.consumed = true
		comments = s.DispatchRead
	case s.changes != nil:
		changes, err := s.changes.FetchIssueCommentChanges(ctx, issueID, since)
		if err != nil {
			return SourceRead{}, fmt.Errorf("read changed comments of issue %s: %w", issueID, err)
		}
		comments, next = changes.Comments, changes.Next
	default:
		var err error
		comments, err = s.Tracker.FetchIssueComments(ctx, issueID)
		if err != nil {
			return SourceRead{}, fmt.Errorf("read comments of issue %s: %w", issueID, err)
		}
	}

	if s.Posting != nil {
		if err := s.Posting.AwaitTrackerComments(ctx, issueID); err != nil {
			return SourceRead{}, fmt.Errorf("wait for comments in flight on issue %s: %w", issueID, err)
		}
	}
	posted, err := s.Store.ListPostedComments(ctx, issueID)
	if err != nil {
		return SourceRead{}, fmt.Errorf("list posted comments of issue %s: %w", issueID, err)
	}

	own := make(map[string]struct{}, len(posted))
	for _, id := range posted {
		own[id] = struct{}{}
	}
	kept := make([]domain.Comment, 0, len(comments))
	for _, c := range comments {
		if _, isOwn := own[c.ID]; !isOwn {
			kept = append(kept, c)
		}
	}
	return SourceRead{Comments: kept, Next: next}, nil
}

// Member resolves an unknown author kind before the access lookup, so a bot
// on a tracker whose full reads report no kind is not taken for a person.
func (s *trackerSource) Member(ctx context.Context, c domain.Comment) (bool, error) {
	kind := c.AuthorKind
	if kind == domain.CommentAuthorUnknown && s.kinds != nil {
		var err error
		kind, err = s.kinds.LookupCommentAuthorKind(ctx, c)
		if err != nil {
			return false, err
		}
	}
	switch kind {
	case domain.CommentAuthorBot, domain.CommentAuthorApp, domain.CommentAuthorExternal:
		return false, nil
	}

	if s.access == nil {
		return true, nil
	}
	if s.denied {
		return false, nil
	}
	granted, err := s.access.CommentAuthorCanWrite(ctx, c)
	var trackerErr *domain.TrackerError
	if errors.As(err, &trackerErr) && trackerErr.Kind == domain.ErrTrackerAuth {
		s.denied = true
		if s.AccessDenied == nil || s.AccessDenied.CompareAndSwap(false, true) {
			s.logger.Error("comment author access lookup refused",
				slog.String("tracker_kind", s.TrackerKind),
				slog.String("hint", "comment_feed.include_authors or comment_feed.authors: all delivers comments without the lookup"),
				slog.Any("error", err))
		}
		return false, nil
	}
	return granted, err
}
