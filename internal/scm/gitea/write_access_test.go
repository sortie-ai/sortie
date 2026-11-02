package gitea

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

func TestCommentAuthorWriteAccess(t *testing.T) {
	t.Parallel()

	const collaborators = `[{"login":"hermes"},{"login":"review-bot"},{"login":"sortie"}]`

	tests := []struct {
		name        string
		login       string
		status      int
		body        string
		want        bool
		wantErrKind domain.TrackerErrorKind
	}{
		{name: "login in the assignee list is granted", login: "hermes", status: http.StatusOK, body: collaborators, want: true},
		{name: "login in other case is granted", login: "Review-Bot", status: http.StatusOK, body: collaborators, want: true},
		{name: "login missing from the list is refused", login: "drive-by", status: http.StatusOK, body: collaborators},
		{name: "404 is a failure, not a refusal", login: "hermes", status: http.StatusNotFound, body: `{"message":"not found"}`, wantErrKind: domain.ErrTrackerNotFound},
		{name: "403 is an auth failure, not a refusal", login: "hermes", status: http.StatusForbidden, body: `{"message":"token does not have at least one of required scope(s)"}`, wantErrKind: domain.ErrTrackerAuth},
		{name: "undecodable body is a payload failure", login: "hermes", status: http.StatusOK, body: `{"login":`, wantErrKind: domain.ErrTrackerPayload},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mux := newPreflightMux(t)
			mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/assignees", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				w.Write([]byte(tt.body)) //nolint:errcheck // test handler
			})
			a := mustAdapter(t, mux)

			got, err := a.CommentAuthorCanWrite(context.Background(), domain.Comment{ID: "1", AuthorID: tt.login})

			if tt.wantErrKind != "" {
				var te *domain.TrackerError
				if !errors.As(err, &te) {
					t.Fatalf("CommentAuthorCanWrite(%q) error = %v, want *domain.TrackerError", tt.login, err)
				}
				if te.Kind != tt.wantErrKind {
					t.Errorf("CommentAuthorCanWrite(%q) error kind = %q, want %q", tt.login, te.Kind, tt.wantErrKind)
				}
			} else if err != nil {
				t.Fatalf("CommentAuthorCanWrite(%q) unexpected error: %v", tt.login, err)
			}
			if got != tt.want {
				t.Errorf("CommentAuthorCanWrite(%q) = %v, want %v", tt.login, got, tt.want)
			}
		})
	}
}
