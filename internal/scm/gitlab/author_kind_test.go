package gitlab

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

func TestGitLabAdapter_LookupCommentAuthorKind(t *testing.T) {
	t.Parallel()

	const (
		botSearch = `[{"id":3,"username":"sortie-bot"}]`
		botDetail = `{"id":3,"bot":true}`
	)

	tests := []struct {
		name         string
		authorID     string
		search       string
		detail       string
		failFirst    bool
		want         domain.CommentAuthorKind
		wantSearches int32
		wantDetails  int32
	}{
		{name: "bot account", authorID: "sortie-bot", search: botSearch, detail: botDetail, want: domain.CommentAuthorBot, wantSearches: 1, wantDetails: 1},
		{name: "person account", authorID: "sortie-bot", search: botSearch, detail: `{"id":3,"bot":false}`, want: domain.CommentAuthorPerson, wantSearches: 1, wantDetails: 1},
		{name: "username the search does not return is a person", authorID: "sortie-bot", search: `[{"id":9,"username":"sortie-bot-2"}]`, want: domain.CommentAuthorPerson, wantSearches: 1},
		{name: "username in another letter case", authorID: "Sortie-Bot", search: botSearch, detail: botDetail, want: domain.CommentAuthorBot, wantSearches: 1, wantDetails: 1},
		{name: "empty author id is unknown with no request", want: domain.CommentAuthorUnknown},
		{name: "failed search is not remembered", authorID: "sortie-bot", search: botSearch, detail: botDetail, failFirst: true, want: domain.CommentAuthorBot, wantSearches: 2, wantDetails: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var searches, details atomic.Int32
			s := newPreflightServer(t)
			s.handle("/api/v4/users", func(w http.ResponseWriter, r *http.Request) {
				if searches.Add(1) == 1 && tt.failFirst {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				w.Write([]byte(tt.search)) //nolint:errcheck // test handler
			})
			s.handle("/api/v4/users/3", func(w http.ResponseWriter, r *http.Request) {
				details.Add(1)
				w.Write([]byte(tt.detail)) //nolint:errcheck // test handler
			})
			a := mustAdapter(t, s)
			comment := domain.Comment{ID: "1", AuthorID: tt.authorID}

			first, err := a.LookupCommentAuthorKind(context.Background(), comment)
			if tt.failFirst {
				assertTrackerErrorKind(t, err, domain.ErrTrackerAuth)
				if first != domain.CommentAuthorUnknown {
					t.Errorf("LookupCommentAuthorKind(%q) = %v on error, want %v", tt.authorID, first, domain.CommentAuthorUnknown)
				}
				first, err = a.LookupCommentAuthorKind(context.Background(), comment)
			}
			if err != nil {
				t.Fatalf("LookupCommentAuthorKind(%q) unexpected error: %v", tt.authorID, err)
			}
			second, err := a.LookupCommentAuthorKind(context.Background(), comment)
			if err != nil {
				t.Fatalf("LookupCommentAuthorKind(%q) repeat unexpected error: %v", tt.authorID, err)
			}

			if first != tt.want || second != tt.want {
				t.Errorf("LookupCommentAuthorKind(%q) = %v then %v, want %v both times", tt.authorID, first, second, tt.want)
			}
			if got := searches.Load(); got != tt.wantSearches {
				t.Errorf("LookupCommentAuthorKind(%q) sent %d user searches, want %d", tt.authorID, got, tt.wantSearches)
			}
			if got := details.Load(); got != tt.wantDetails {
				t.Errorf("LookupCommentAuthorKind(%q) sent %d user reads, want %d", tt.authorID, got, tt.wantDetails)
			}
		})
	}
}
