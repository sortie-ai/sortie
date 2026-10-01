package route_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/notify/route"
)

type postedComment struct {
	method  string
	issueID string
	text    string
	literal string
}

type methodRecorder struct {
	domain.TrackerAdapter

	mu    sync.Mutex
	posts []postedComment
	err   error
}

func (r *methodRecorder) CommentIssue(_ context.Context, issueID, text string) error {
	r.record(postedComment{method: "CommentIssue", issueID: issueID, text: text})
	return r.err
}

func (r *methodRecorder) CommentIssueWithLiteral(_ context.Context, issueID, text, literal string) error {
	r.record(postedComment{method: "CommentIssueWithLiteral", issueID: issueID, text: text, literal: literal})
	return r.err
}

func (r *methodRecorder) record(p postedComment) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.posts = append(r.posts, p)
}

func (r *methodRecorder) recorded() []postedComment {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.posts)
}

func TestTrackerComment_SendChoosesCommentMethodByAgentText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		agentText string
		want      postedComment
	}{
		{
			name:      "agent text posts the body with the text as a literal",
			agentText: "Which one should the API expose?",
			want:      postedComment{method: "CommentIssueWithLiteral", issueID: "ISS-1", text: "body text", literal: "Which one should the API expose?"},
		},
		{
			name: "no agent text posts the body alone",
			want: postedComment{method: "CommentIssue", issueID: "ISS-1", text: "body text"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tracker := &methodRecorder{}
			router := route.NewRouter(tracker, nil)
			mustUpdate(t, router, route.Inputs{
				Entries: []config.NotificationBackend{entry(domain.TrackerCommentKind, domain.EventSessionStopped)},
			})
			notification := notificationOf(domain.EventSessionStopped)
			notification.Message.AgentText = tt.agentText

			outcomes := router.Route(notification).Deliver(context.Background())

			got := tracker.recorded()
			if len(got) != 1 || got[0] != tt.want {
				t.Errorf("tracker posts = %+v, want exactly %+v", got, tt.want)
			}
			if received, err := outcomes.TrackerComment(); !received || err != nil {
				t.Errorf("Outcomes.TrackerComment() = (%v, %v), want (true, nil)", received, err)
			}
		})
	}
}

func TestTrackerComment_SendReturnsTheLiteralPostError(t *testing.T) {
	t.Parallel()

	postErr := errors.New("tracker unavailable")
	tracker := &methodRecorder{err: postErr}
	router := route.NewRouter(tracker, nil)
	mustUpdate(t, router, route.Inputs{
		Entries: []config.NotificationBackend{entry(domain.TrackerCommentKind, domain.EventSessionStopped)},
	})
	notification := notificationOf(domain.EventSessionStopped)
	notification.Message.AgentText = "reason"

	outcomes := router.Route(notification).Deliver(context.Background())

	received, err := outcomes.TrackerComment()
	if !received || !errors.Is(err, postErr) {
		t.Errorf("Outcomes.TrackerComment() = (%v, %v), want (true, %v)", received, err, postErr)
	}
}
