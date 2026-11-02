package commentfeed

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/storage"
)

var quiet = slog.New(slog.DiscardHandler)

type callLog struct{ calls []string }

func (l *callLog) add(call string) { l.calls = append(l.calls, call) }

func (l *callLog) count(call string) int {
	return len(slices.DeleteFunc(slices.Clone(l.calls), func(c string) bool { return c != call }))
}

type answer struct {
	granted bool
	err     error
}

type fakeTracker struct {
	domain.TrackerAdapter
	log      *callLog
	comments []domain.Comment
	readErr  error
	access   map[string]answer
	since    domain.CommentReadPoint
}

func (f *fakeTracker) FetchIssueCommentChanges(_ context.Context, _ string, since domain.CommentReadPoint) (domain.CommentChanges, error) {
	f.log.add("changes:" + string(since))
	f.since = since
	return domain.CommentChanges{Comments: f.comments, Next: "p1"}, f.readErr
}

func (f *fakeTracker) FetchIssueComments(context.Context, string) ([]domain.Comment, error) {
	f.log.add("full")
	return f.comments, f.readErr
}

func (f *fakeTracker) LookupCommentAuthorKind(context.Context, domain.Comment) (domain.CommentAuthorKind, error) {
	f.log.add("kind")
	return domain.CommentAuthorBot, nil
}

func (f *fakeTracker) CommentAuthorCanWrite(_ context.Context, c domain.Comment) (bool, error) {
	f.log.add("canwrite")
	a, listed := f.access[c.AuthorID]
	return a.granted || !listed, a.err
}

type fakeStore struct {
	log       *callLog
	posted    []string
	postedErr error
	given     []storage.GivenComment
	earlier   []storage.GivenComment
}

func (f *fakeStore) ListPostedComments(context.Context, string) ([]string, error) {
	f.log.add("posted")
	return f.posted, f.postedErr
}

func (f *fakeStore) ListGivenComments(context.Context, string, string) ([]storage.GivenComment, error) {
	return f.given, nil
}

func (f *fakeStore) ListEarlierGivenComments(context.Context, string, string) ([]storage.GivenComment, error) {
	return f.earlier, nil
}

func hashOf(text string) [sha256.Size]byte { return sha256.Sum256([]byte(text)) }

func givenRow(text string) storage.GivenComment {
	return storage.GivenComment{Source: SourceTracker, CommentID: "c1", BodySHA256: hashOf(text)}
}

func comment(id, authorID string, kind domain.CommentAuthorKind) domain.Comment {
	return domain.Comment{ID: id, Author: authorID, AuthorID: authorID, AuthorKind: kind, Body: "body " + id, CreatedAt: "2026-01-01T00:00:00Z"}
}

func feedCfg(authors config.CommentFeedAuthors, include, exclude []string) config.CommentFeedConfig {
	return config.CommentFeedConfig{Enabled: true, Authors: authors, IncludeAuthors: include, ExcludeAuthors: exclude}
}

func newRun(cfg config.CommentFeedConfig, store Store, tracker domain.TrackerAdapter, report func([]storage.GivenComment)) *Run {
	source := NewTrackerSource(TrackerSourceParams{Tracker: tracker, Store: store, Logger: quiet})
	return NewRun(RunParams{Config: cfg, IssueID: "i1", Source: source, Store: store, Report: report, Logger: quiet})
}

func mustSelect(t *testing.T, run *Run, conv Conversation) Selection {
	t.Helper()
	sel, err := run.Select(t.Context(), conv)
	if err != nil {
		t.Fatalf("Select() error = %v, want nil", err)
	}
	return sel
}

func pendingIDs(ps []Pending) (ids, replacing []string) {
	for _, p := range ps {
		ids = append(ids, p.Comment.ID)
		if p.Replacing {
			replacing = append(replacing, p.Comment.ID)
		}
	}
	return ids, replacing
}

type outcome struct {
	new, earlier, replacing []string
	since                   domain.CommentReadPoint
	canWrite, kind          int
}

func TestRun_Select(t *testing.T) {
	t.Parallel()

	members, all := config.CommentFeedAuthorsMembers, config.CommentFeedAuthorsAll
	person, authErr := domain.CommentAuthorPerson, &domain.TrackerError{Kind: domain.ErrTrackerAuth, Message: "token refused"}
	carried, fresh := Conversation{ID: "s1", Carried: true}, Conversation{ID: "s2"}
	alice := []domain.Comment{comment("c1", "alice", person)}

	tests := []struct {
		name           string
		cfg            config.CommentFeedConfig
		conv           Conversation
		given, earlier []storage.GivenComment
		comments       []domain.Comment
		access         map[string]answer
		deliverFirst   bool
		refuse         string
		want           outcome
	}{
		{
			name: "restart of a carried conversation selects nothing", cfg: feedCfg(all, nil, nil), conv: carried,
			given: []storage.GivenComment{givenRow("body c1")}, comments: alice,
		},
		{
			name: "new conversation gets earlier-given comments in the earlier group", cfg: feedCfg(all, nil, nil), conv: fresh,
			earlier: []storage.GivenComment{givenRow("an older text")}, comments: append(slices.Clone(alice), comment("c2", "alice", person)),
			want: outcome{new: []string{"c2"}, earlier: []string{"c1"}},
		},
		{
			name: "edited comment is new and replacing", cfg: feedCfg(all, nil, nil), conv: carried,
			given: []storage.GivenComment{givenRow("an older text")}, comments: alice,
			want: outcome{new: []string{"c1"}, replacing: []string{"c1"}},
		},
		{
			name: "delivered comment is not selected again", cfg: feedCfg(all, nil, nil), conv: fresh,
			comments: alice, deliverFirst: true, want: outcome{since: "p1"},
		},
		{
			name: "line endings and surrounding white space are not an edit", cfg: feedCfg(all, nil, nil), conv: carried,
			given:    []storage.GivenComment{givenRow("line1\nline2\nline3")},
			comments: []domain.Comment{{ID: "c1", AuthorID: "alice", Body: " line1\r\nline2\rline3 \n"}},
		},
		{
			name: "all delivers a bot without a lookup", cfg: feedCfg(all, nil, nil), conv: fresh,
			comments: []domain.Comment{comment("c1", "bot1", domain.CommentAuthorBot)}, want: outcome{new: []string{"c1"}},
		},
		{
			name: "include match delivers without a lookup", cfg: feedCfg(members, []string{"alice"}, nil), conv: fresh,
			comments: []domain.Comment{comment("c1", "alice", domain.CommentAuthorBot)}, want: outcome{new: []string{"c1"}},
		},
		{
			name: "exclusion beats inclusion and an empty author identity is not a member", cfg: feedCfg(members, []string{"alice"}, []string{"alice"}), conv: fresh,
			comments: append(slices.Clone(alice), comment("c2", "", person)),
		},
		{
			name: "only a person with write access is a member", cfg: feedCfg(members, nil, nil), conv: fresh,
			comments: []domain.Comment{
				comment("c1", "u1", domain.CommentAuthorBot), comment("c2", "u2", domain.CommentAuthorApp),
				comment("c3", "u3", domain.CommentAuthorExternal), comment("c4", "dave", person), comment("c5", "u5", person),
			},
			access: map[string]answer{"dave": {}}, want: outcome{new: []string{"c5"}, canWrite: 2},
		},
		{
			name: "unknown kind is resolved before the access lookup", cfg: feedCfg(members, nil, nil), conv: fresh,
			comments: []domain.Comment{comment("c1", "bot1", domain.CommentAuthorUnknown)}, want: outcome{kind: 1},
		},
		{
			name: "auth refusal stops further access lookups in the run", cfg: feedCfg(members, nil, nil), conv: fresh,
			comments: []domain.Comment{comment("c1", "dave", person), comment("c2", "erin", person)},
			access:   map[string]answer{"dave": {err: authErr}}, want: outcome{canWrite: 1},
		},
		{
			name: "failed lookup withholds the comment and keeps the read point", cfg: feedCfg(members, nil, nil), conv: fresh,
			comments: []domain.Comment{comment("c1", "dave", person), comment("c2", "erin", person)},
			access:   map[string]answer{"dave": {err: errors.New("lookup failed")}}, deliverFirst: true, want: outcome{canWrite: 3},
		},
		{
			name: "comment the channel refused stays pending and keeps the read point", cfg: feedCfg(all, nil, nil), conv: fresh,
			comments: append(slices.Clone(alice), comment("c2", "alice", person)), deliverFirst: true, refuse: "c2",
			want: outcome{new: []string{"c2"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			log := &callLog{}
			store := &fakeStore{log: log, given: tt.given, earlier: tt.earlier}
			tracker := &fakeTracker{log: log, comments: tt.comments, access: tt.access}
			run := newRun(tt.cfg, store, tracker, func([]storage.GivenComment) {})
			if tt.deliverFirst {
				first := mustSelect(t, run, tt.conv)
				delivered := slices.DeleteFunc(slices.Concat(first.Earlier, first.New), func(p Pending) bool { return p.Comment.ID == tt.refuse })
				run.Delivered("s1", time.Time{}, ChannelNextPrompt, delivered)
			}

			sel := mustSelect(t, run, tt.conv)

			got := outcome{since: tracker.since, canWrite: log.count("canwrite"), kind: log.count("kind")}
			got.new, got.replacing = pendingIDs(sel.New)
			got.earlier, _ = pendingIDs(sel.Earlier)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Select() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestRun_FinishReportsRowsHeldWithoutASessionID(t *testing.T) {
	t.Parallel()

	log := &callLog{}
	var reported []storage.GivenComment
	run := newRun(feedCfg(config.CommentFeedAuthorsAll, nil, nil), &fakeStore{log: log},
		&fakeTracker{log: log, comments: []domain.Comment{comment("c1", "alice", domain.CommentAuthorPerson)}},
		func(rows []storage.GivenComment) { reported = append(reported, rows...) })

	run.Delivered("", time.Time{}, ChannelNextPrompt, mustSelect(t, run, Conversation{}).New)
	if len(reported) != 0 {
		t.Fatalf("Delivered(\"\") reported %d rows, want 0", len(reported))
	}
	run.Finish("s1")

	if len(reported) != 1 || reported[0].ConversationID != "s1" || reported[0].CommentID != "c1" {
		t.Errorf("Finish(%q) reported %+v, want one row of c1 under s1", "s1", reported)
	}
}

func TestPlace(t *testing.T) {
	t.Parallel()

	errRender := errors.New("render failed")
	edited := Pending{Comment: domain.Comment{ID: "n1", Body: " raw\r\n<text> ", CreatedAt: "t2"}, Replacing: true}
	plain := Pending{Comment: domain.Comment{ID: "n2", Author: "bob", Body: "plain", CreatedAt: "t3"}}
	earlier := Pending{Comment: domain.Comment{ID: "e1", Author: "alice", Body: "older", CreatedAt: "t1"}}
	wantBlock := "base\n\n## Issue comments from earlier agent sessions\n\n" +
		"An earlier agent session on this issue was given these comments; this session has not seen them. Each shows its current text, oldest first. Each comment body is a JSON string.\n\n" +
		"### alice, t1\n\n<issue_comment>\"older\"</issue_comment>\n\n## New issue comments\n\n" +
		"Comments on this issue you have not been given yet, oldest first. Each comment body is a JSON string.\n\n" +
		"### unknown author, t2 (edited: this text replaces the version you were given earlier)\n\n" +
		`<issue_comment>" raw\r\n\u003ctext\u003e "</issue_comment>` + "\n\n### bob, t3\n\n<issue_comment>\"plain\"</issue_comment>"
	both, newOnly := Selection{Earlier: []Pending{earlier}, New: []Pending{plain}}, Selection{New: []Pending{plain}}

	tests := []struct {
		name                  string
		sel                   Selection
		emptied, filled       string
		emptiedErr, filledErr error
		filledBuiltin         bool
		wantPrompt            string
		wantPlaced            []string
		wantRender            []int
	}{
		{name: "empty selection renders once", emptied: "base", wantPrompt: "base", wantRender: []int{0}},
		{name: "failing filled render places nothing", sel: newOnly, emptied: "base", filledErr: errRender, wantRender: []int{0, 1}},
		{
			name: "identical renders get the block, earlier group first, edit mark only on replacing",
			sel:  Selection{Earlier: both.Earlier, New: []Pending{edited, plain}}, emptied: "base", filled: "base",
			wantPrompt: wantBlock, wantPlaced: []string{"e1", "n1", "n2"}, wantRender: []int{0, 3},
		},
		{name: "differing renders return the filled render alone", sel: both, emptied: "base", filled: "with feed", wantPrompt: "with feed", wantPlaced: []string{"e1", "n2"}, wantRender: []int{0, 2}},
		{name: "failing emptied render counts as placed", sel: newOnly, emptiedErr: errRender, filled: "with feed", wantPrompt: "with feed", wantPlaced: []string{"n2"}, wantRender: []int{0, 1}},
		{name: "blank filled render gets the block", sel: newOnly, emptied: "base", filled: " ", wantPrompt: " \n\n" + Block(newOnly), wantPlaced: []string{"n2"}, wantRender: []int{0, 1}},
		{name: "built-in filled render gets the block although it differs", sel: newOnly, emptied: "base", filled: "builtin", filledBuiltin: true, wantPrompt: "builtin\n\n" + Block(newOnly), wantPlaced: []string{"n2"}, wantRender: []int{0, 1}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var rendered []int
			render := func(sel Selection) (string, bool, error) {
				rendered = append(rendered, len(sel.Earlier)+len(sel.New))
				if rendered[len(rendered)-1] == 0 {
					return tt.emptied, false, tt.emptiedErr
				}
				return tt.filled, tt.filledBuiltin, tt.filledErr
			}

			prompt, placed, err := Place(render, tt.sel)

			placedIDs, _ := pendingIDs(placed)
			if !errors.Is(err, tt.filledErr) || prompt != tt.wantPrompt || !slices.Equal(placedIDs, tt.wantPlaced) || !slices.Equal(rendered, tt.wantRender) {
				t.Errorf("Place() = (%q, %v, %v) after renders %v, want (%q, %v, %v) after %v", prompt, placedIDs, err, rendered, tt.wantPrompt, tt.wantPlaced, tt.filledErr, tt.wantRender)
			}
		})
	}
}
