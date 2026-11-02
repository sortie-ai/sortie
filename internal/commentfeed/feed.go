// Package commentfeed selects the comments an agent conversation has not been
// given and keeps the record of the ones it was.
//
// Sources supply comments and channels carry them to the agent, and neither
// is named here: [Run] decides what is pending, [Place] puts it in a prompt,
// and a channel reports what it delivered through [Recorder].
package commentfeed

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/storage"
)

const (
	// SourceTracker is the given_comments source of tracker comments.
	SourceTracker = "tracker"

	// ChannelNextPrompt is the given_comments channel of the next prompt.
	ChannelNextPrompt = "next_prompt"
)

// Pending is a comment selected for delivery.
type Pending struct {
	Source     string
	Comment    domain.Comment
	BodySHA256 [sha256.Size]byte

	// Replacing reports that the conversation holds this comment with
	// another hash.
	Replacing bool
}

// Selection is what one boundary delivers.
type Selection struct {
	// New holds the comments the conversation has not been given, in read
	// order, oldest first.
	New []Pending

	// Earlier holds comments only earlier conversations were given, in read
	// order. It is filled only in a new conversation.
	Earlier []Pending
}

// Conversation identifies the agent conversation a boundary belongs to.
type Conversation struct {
	// ID is the accepted session identifier; empty until the session
	// reports one.
	ID string

	// Carried reports that the session carries its predecessor's history.
	Carried bool
}

// SourceRead is the result of one [Source] read.
type SourceRead struct {
	// Comments are the comments changed since the read point, Sortie's own
	// removed, in read order.
	Comments []domain.Comment

	// Next is the point the next read passes once this one is delivered.
	Next domain.CommentReadPoint
}

// Source supplies the comments of one issue.
type Source interface {
	// Name is the given_comments source value of this source's comments.
	Name() string

	// Read returns the issue's comments changed since the point, or the
	// full list for the zero point.
	Read(ctx context.Context, issueID string, since domain.CommentReadPoint) (SourceRead, error)

	// Member reports whether the author of c is a member of the project. An
	// error means the question is undecided.
	Member(ctx context.Context, c domain.Comment) (bool, error)
}

// Recorder is how a channel reports what the agent received through it. A
// channel that refuses a comment does not list it, so the comment stays
// pending.
type Recorder interface {
	Delivered(sessionID string, at time.Time, channel string, comments []Pending)
}

var _ Recorder = (*Run)(nil)

// Store is the read side of the record of posted and given comments.
type Store interface {
	ListPostedComments(ctx context.Context, issueID string) ([]string, error)
	ListGivenComments(ctx context.Context, issueID, conversationID string) ([]storage.GivenComment, error)
	ListEarlierGivenComments(ctx context.Context, issueID, conversationID string) ([]storage.GivenComment, error)
}

// RunParams configures [NewRun].
type RunParams struct {
	Config  config.CommentFeedConfig
	IssueID string
	Source  Source
	Store   Store

	// Report receives the rows to record, each already stamped with its
	// conversation.
	Report func(rows []storage.GivenComment)
	Logger *slog.Logger
}

type commentKey struct {
	source    string
	commentID string
}

// Run is the feed of one worker attempt. Every method runs on the worker
// goroutine; a Run is not safe for concurrent use.
type Run struct {
	cfg     config.CommentFeedConfig
	issueID string
	source  Source
	store   Store
	report  func(rows []storage.GivenComment)
	logger  *slog.Logger

	loaded  bool
	carried bool
	held    map[commentKey]storage.GivenComment
	earlier map[commentKey]struct{}

	point       domain.CommentReadPoint
	candidate   domain.CommentReadPoint
	withheld    bool
	outstanding bool
	selected    []Pending
	unreported  []storage.GivenComment
}

// NewRun returns the feed of one worker attempt.
func NewRun(p RunParams) *Run {
	logger := p.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Run{
		cfg:     p.Config,
		issueID: p.IssueID,
		source:  p.Source,
		store:   p.Store,
		report:  p.Report,
		logger:  logger,
		held:    make(map[commentKey]storage.GivenComment),
	}
}

// Select returns the comments the conversation has not been given. It makes
// at most one source read, and a failure leaves the read point where it was.
func (r *Run) Select(ctx context.Context, conv Conversation) (Selection, error) {
	r.outstanding = false
	r.withheld = false
	r.selected = nil

	if !r.loaded {
		if err := r.load(ctx, conv); err != nil {
			return Selection{}, err
		}
	}

	read, err := r.source.Read(ctx, r.issueID, r.point)
	if err != nil {
		return Selection{}, err
	}
	r.candidate = read.Next
	r.outstanding = true

	var sel Selection
	var withheldCount int
	var firstErr error
	for _, c := range read.Comments {
		key := commentKey{source: r.source.Name(), commentID: c.ID}
		hash := BodySHA256(c.Body)
		prior, holds := r.held[key]
		if holds && prior.BodySHA256 == hash {
			continue
		}
		if slices.Contains(r.cfg.ExcludeAuthors, c.AuthorID) {
			continue
		}
		if r.cfg.Authors == config.CommentFeedAuthorsMembers && !slices.Contains(r.cfg.IncludeAuthors, c.AuthorID) {
			if c.AuthorID == "" {
				continue
			}
			member, err := r.source.Member(ctx, c)
			if err != nil {
				withheldCount++
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			if !member {
				continue
			}
		}
		p := Pending{Source: key.source, Comment: c, BodySHA256: hash, Replacing: holds}
		if _, wasGiven := r.earlier[key]; !r.carried && !holds && wasGiven {
			sel.Earlier = append(sel.Earlier, p)
			continue
		}
		sel.New = append(sel.New, p)
	}

	r.selected = slices.Concat(sel.Earlier, sel.New)
	if withheldCount > 0 {
		r.withheld = true
		r.logger.Warn("issue comments withheld after author lookup failed",
			slog.Int("count", withheldCount),
			slog.Any("error", firstErr))
	}
	return sel, nil
}

func (r *Run) load(ctx context.Context, conv Conversation) error {
	if conv.Carried {
		rows, err := r.store.ListGivenComments(ctx, r.issueID, conv.ID)
		if err != nil {
			return err
		}
		for _, row := range rows {
			r.held[commentKey{source: row.Source, commentID: row.CommentID}] = row
		}
	} else {
		rows, err := r.store.ListEarlierGivenComments(ctx, r.issueID, conv.ID)
		if err != nil {
			return err
		}
		r.earlier = make(map[commentKey]struct{}, len(rows))
		for _, row := range rows {
			r.earlier[commentKey{source: row.Source, commentID: row.CommentID}] = struct{}{}
		}
	}
	r.loaded = true
	r.carried = conv.Carried
	return nil
}

// Delivered records the comments a channel gave the agent and commits the
// read point of the last Select unless that Select withheld a comment or the
// channel left a selected one out.
// Rows wait for a non-empty sessionID before they are reported.
func (r *Run) Delivered(sessionID string, at time.Time, channel string, comments []Pending) {
	if r.outstanding && !r.withheld && deliversAll(r.selected, comments) {
		r.point = r.candidate
	}
	r.outstanding = false
	r.selected = nil

	for _, p := range comments {
		row := storage.GivenComment{
			Source:     p.Source,
			CommentID:  p.Comment.ID,
			BodySHA256: p.BodySHA256,
			Channel:    channel,
			GivenAt:    at,
		}
		r.held[commentKey{source: p.Source, commentID: p.Comment.ID}] = row
		r.unreported = append(r.unreported, row)
	}
	r.flush(sessionID)
}

func deliversAll(selected, delivered []Pending) bool {
	given := make(map[commentKey]struct{}, len(delivered))
	for _, p := range delivered {
		given[commentKey{source: p.Source, commentID: p.Comment.ID}] = struct{}{}
	}
	for _, p := range selected {
		if _, ok := given[commentKey{source: p.Source, commentID: p.Comment.ID}]; !ok {
			return false
		}
	}
	return true
}

// Finish reports the rows still waiting for a session identifier and drops
// the ones it still cannot place.
func (r *Run) Finish(sessionID string) {
	r.flush(sessionID)
	r.unreported = nil
}

func (r *Run) flush(sessionID string) {
	if sessionID == "" || len(r.unreported) == 0 {
		return
	}
	rows := r.unreported
	r.unreported = nil
	for i := range rows {
		rows[i].ConversationID = sessionID
	}
	r.report(rows)
}

// BodySHA256 hashes the text a person wrote: line endings become line feeds
// and surrounding white space is removed first, so neither counts as an edit.
func BodySHA256(body string) [sha256.Size]byte {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	return sha256.Sum256([]byte(strings.TrimSpace(body)))
}
