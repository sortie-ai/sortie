package gitlab

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

func TestWriteAccessMapsMembershipToGrant(t *testing.T) {
	t.Parallel()

	const botSearch = `[{"id":3,"username":"sortie-bot"}]`

	tests := []struct {
		name             string
		login            string
		search           string
		membershipID     string
		membershipStatus int
		membershipBody   string
		want             bool
		wantKind         domain.TrackerErrorKind
	}{
		{name: "developer is granted", login: "sortie-bot", search: botSearch, membershipID: "3", membershipStatus: http.StatusOK, membershipBody: `{"access_level":30}`, want: true},
		{name: "owner is granted", login: "sortie-bot", search: botSearch, membershipID: "3", membershipStatus: http.StatusOK, membershipBody: `{"access_level":50}`, want: true},
		{name: "reporter is refused", login: "sortie-bot", search: botSearch, membershipID: "3", membershipStatus: http.StatusOK, membershipBody: `{"access_level":20}`},
		{name: "non-member is refused", login: "sortie-bot", search: botSearch, membershipID: "3", membershipStatus: http.StatusNotFound, membershipBody: `{"message":"404 Not found"}`},
		{name: "login in other case is granted at developer", login: "Sortie-Bot", search: botSearch, membershipID: "3", membershipStatus: http.StatusOK, membershipBody: `{"access_level":30}`, want: true},
		{name: "search holding only another username is refused without a membership request", login: "sortie-bot", search: `[{"id":9,"username":"sortie-bot-2"}]`},
		{name: "forbidden membership read is an auth error, not a refusal", login: "sortie-bot", search: botSearch, membershipID: "3", membershipStatus: http.StatusForbidden, membershipBody: `{"message":"403 Forbidden"}`, wantKind: domain.ErrTrackerAuth},
		{name: "undecodable membership body is a payload error", login: "sortie-bot", search: botSearch, membershipID: "3", membershipStatus: http.StatusOK, membershipBody: `not json`, wantKind: domain.ErrTrackerPayload},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var gotUsername atomic.Value
			s := newPreflightServer(t)
			s.handle("/api/v4/users", func(w http.ResponseWriter, r *http.Request) {
				gotUsername.Store(r.URL.Query().Get("username"))
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(tt.search)) //nolint:errcheck // test handler
			})
			if tt.membershipID != "" {
				s.handle("/api/v4/projects/"+testEscapedProject+"/members/all/"+tt.membershipID, func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(tt.membershipStatus)
					w.Write([]byte(tt.membershipBody)) //nolint:errcheck // test handler
				})
			}
			a := mustAdapter(t, s)

			got, err := a.CommentAuthorCanWrite(context.Background(), domain.Comment{ID: "1", AuthorID: tt.login})

			if tt.wantKind != "" {
				assertTrackerErrorKind(t, err, tt.wantKind)
			} else if err != nil {
				t.Fatalf("CommentAuthorCanWrite(%q) unexpected error: %v", tt.login, err)
			}
			if got != tt.want {
				t.Errorf("CommentAuthorCanWrite(%q) = %v, want %v", tt.login, got, tt.want)
			}
			if user, _ := gotUsername.Load().(string); user != tt.login {
				t.Errorf("CommentAuthorCanWrite(%q) searched username %q, want %q", tt.login, user, tt.login)
			}
		})
	}
}
