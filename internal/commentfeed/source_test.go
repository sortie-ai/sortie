package commentfeed

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

type fakePosting struct {
	log *callLog
	err error
}

func (f *fakePosting) AwaitTrackerComments(context.Context, string) error {
	f.log.add("wait")
	return f.err
}

type withoutChangeReader struct{ domain.TrackerAdapter }

type sourceOutcome struct {
	ids   []string
	next  domain.CommentReadPoint
	calls []string
}

func TestTrackerSource_Read(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")
	three := []domain.Comment{comment("c1", "a", ""), comment("c2", "a", ""), comment("c3", "a", "")}

	tests := []struct {
		name        string
		sinces      []domain.CommentReadPoint
		dispatch    []domain.Comment
		hasDispatch bool
		plain       bool
		posted      []string
		failAt      string
		want        sourceOutcome
	}{
		{
			name: "posted comments are removed in read order", sinces: []domain.CommentReadPoint{""}, posted: []string{"c2"},
			want: sourceOutcome{ids: []string{"c1", "c3"}, next: "p1", calls: []string{"changes:", "wait", "posted"}},
		},
		{
			name: "dispatch read makes no tracker call and has no read point", sinces: []domain.CommentReadPoint{"p9"},
			dispatch: []domain.Comment{comment("d1", "a", "")}, hasDispatch: true,
			want: sourceOutcome{ids: []string{"d1"}, calls: []string{"wait", "posted"}},
		},
		{
			name: "read after the dispatch read goes through the change reader from the passed point", sinces: []domain.CommentReadPoint{"", "p0"}, hasDispatch: true,
			want: sourceOutcome{ids: []string{"c1", "c2", "c3"}, next: "p1", calls: []string{"wait", "posted", "changes:p0", "wait", "posted"}},
		},
		{
			name: "tracker without a change reader gets the full list", sinces: []domain.CommentReadPoint{"p0"}, plain: true,
			want: sourceOutcome{ids: []string{"c1", "c2", "c3"}, calls: []string{"full", "wait", "posted"}},
		},
		{name: "read error is returned before the wait", sinces: []domain.CommentReadPoint{""}, failAt: "read", want: sourceOutcome{calls: []string{"changes:"}}},
		{name: "wait error is returned before the posted read", sinces: []domain.CommentReadPoint{""}, failAt: "wait", want: sourceOutcome{calls: []string{"changes:", "wait"}}},
		{name: "posted read error is returned without filtering", sinces: []domain.CommentReadPoint{""}, failAt: "posted", want: sourceOutcome{calls: []string{"changes:", "wait", "posted"}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			failure := func(at string) error {
				if at != "" && tt.failAt == at {
					return errBoom
				}
				return nil
			}
			log := &callLog{}
			var tracker domain.TrackerAdapter = &fakeTracker{log: log, comments: three, readErr: failure("read")}
			if tt.plain {
				tracker = withoutChangeReader{tracker}
			}
			source := NewTrackerSource(TrackerSourceParams{
				Tracker: tracker, Store: &fakeStore{log: log, posted: tt.posted, postedErr: failure("posted")}, Posting: &fakePosting{log: log, err: failure("wait")},
				DispatchRead: tt.dispatch, HasDispatchRead: tt.hasDispatch, Logger: quiet,
			})

			var read SourceRead
			var err error
			for _, since := range tt.sinces {
				read, err = source.Read(t.Context(), "i1", since)
			}

			got := sourceOutcome{next: read.Next, calls: log.calls}
			for _, c := range read.Comments {
				got.ids = append(got.ids, c.ID)
			}
			if !errors.Is(err, failure(tt.failAt)) || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Read() = (%+v, %v), want (%+v, %v)", got, err, tt.want, failure(tt.failAt))
			}
		})
	}
}
