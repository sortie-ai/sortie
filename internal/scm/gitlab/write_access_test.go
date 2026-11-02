package gitlab

import (
	"context"
	"net/http"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

func TestWriteAccessMapsMembershipToGrant(t *testing.T) {
	t.Parallel()

	const (
		login     = "sortie-bot"
		botSearch = `[{"id":3,"username":"sortie-bot"}]`
	)

	tests := []struct {
		name             string
		search           string
		membershipID     string
		membershipStatus int
		membershipBody   string
		want             bool
		wantKind         domain.TrackerErrorKind
	}{
		{name: "developer is granted", search: botSearch, membershipID: "3", membershipStatus: http.StatusOK, membershipBody: `{"access_level":30}`, want: true},
		{name: "reporter is refused", search: botSearch, membershipID: "3", membershipStatus: http.StatusOK, membershipBody: `{"access_level":20}`},
		{name: "non-member is refused", search: botSearch, membershipID: "3", membershipStatus: http.StatusNotFound, membershipBody: `{"message":"404 Not found"}`},
		{name: "search holding only another username is refused without a membership request", search: `[{"id":9,"username":"sortie-bot-2"}]`},
		{name: "forbidden membership read is an auth error, not a refusal", search: botSearch, membershipID: "3", membershipStatus: http.StatusForbidden, membershipBody: `{"message":"403 Forbidden"}`, wantKind: domain.ErrTrackerAuth},
		{name: "undecodable membership body is a payload error", search: botSearch, membershipID: "3", membershipStatus: http.StatusOK, membershipBody: `not json`, wantKind: domain.ErrTrackerPayload},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := newPreflightServer(t)
			s.handle("/api/v4/users", func(w http.ResponseWriter, r *http.Request) {
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

			got, err := a.CommentAuthorCanWrite(context.Background(), domain.Comment{ID: "1", AuthorID: login})

			if tt.wantKind != "" {
				assertTrackerErrorKind(t, err, tt.wantKind)
			} else if err != nil {
				t.Fatalf("CommentAuthorCanWrite(%q) unexpected error: %v", login, err)
			}
			if got != tt.want {
				t.Errorf("CommentAuthorCanWrite(%q) = %v, want %v", login, got, tt.want)
			}
		})
	}
}
