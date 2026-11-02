package github

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/waittest"
)

func TestWriteAccessMapsPermissionAnswers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		status   int
		body     string
		want     bool
		wantKind domain.TrackerErrorKind
	}{
		{name: "admin is granted", status: http.StatusOK, body: `{"permission":"admin","role_name":"admin"}`, want: true},
		{name: "write with maintain role is granted", status: http.StatusOK, body: `{"permission":"write","role_name":"maintain"}`, want: true},
		{name: "read is refused", status: http.StatusOK, body: `{"permission":"read","role_name":"read"}`},
		{name: "none with empty role is refused", status: http.StatusOK, body: `{"permission":"none","role_name":""}`},
		{name: "unknown login is refused without error", status: http.StatusNotFound, body: `{"message":"octocat is not a user"}`},
		{name: "missing push access fails as auth, not a refusal", status: http.StatusForbidden, body: `{"message":"Must have push access to view collaborator permission."}`, wantKind: domain.ErrTrackerAuth},
		{name: "undecodable body fails as payload", status: http.StatusOK, body: `not json`, wantKind: domain.ErrTrackerPayload},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/repos/owner/repo/collaborators/octocat/permission" {
					http.Error(w, `{"message":"unexpected request"}`, http.StatusTeapot)
					return
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(srv.Close)
			adapter := mustAdapter(t, validConfig(srv.URL))

			got, err := adapter.CommentAuthorCanWrite(waittest.Context(t), domain.Comment{ID: "1", AuthorID: "octocat"})

			if tt.wantKind != "" {
				var te *domain.TrackerError
				if !errors.As(err, &te) {
					t.Fatalf("CommentAuthorCanWrite() error = %v, want *domain.TrackerError of kind %q", err, tt.wantKind)
				}
				if te.Kind != tt.wantKind {
					t.Errorf("CommentAuthorCanWrite() error kind = %q, want %q", te.Kind, tt.wantKind)
				}
				return
			}
			if err != nil {
				t.Fatalf("CommentAuthorCanWrite() error = %v, want nil", err)
			}
			if got != tt.want {
				t.Errorf("CommentAuthorCanWrite() = %v, want %v", got, tt.want)
			}
		})
	}
}
