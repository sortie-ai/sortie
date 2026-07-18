package gitea

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

// mustSCMAdapterWithProject constructs a *GiteaSCMAdapter against endpoint
// with a throwaway token and the given "project" config value, or fails the
// test. An empty project matches the omitted-key case: NewGiteaSCMAdapter
// treats an empty string the same as an absent key.
func mustSCMAdapterWithProject(t *testing.T, endpoint, project string) *GiteaSCMAdapter {
	t.Helper()
	a, err := NewGiteaSCMAdapter(map[string]any{
		"endpoint": endpoint,
		"api_key":  "test-token",
		"project":  project,
	})
	if err != nil {
		t.Fatalf("NewGiteaSCMAdapter: %v", err)
	}
	return a.(*GiteaSCMAdapter)
}

// --- VerifyAutoMergeScopes ---

func TestGiteaSCMVerifyAutoMergeScopes(t *testing.T) {
	t.Parallel()

	t.Run("permissions.push false yields a non-empty missing and nil err", func(t *testing.T) {
		t.Parallel()

		fixture := loadFixture(t, "repo_permissions_push_false.json")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fixture)
		}))
		defer srv.Close()

		adapter := mustSCMAdapterWithProject(t, srv.URL, testOwner+"/"+testRepo)
		scopes, missing, err := adapter.VerifyAutoMergeScopes(context.Background(), false)
		if err != nil {
			t.Fatalf("VerifyAutoMergeScopes: unexpected error: %v", err)
		}
		if scopes != nil {
			t.Errorf("VerifyAutoMergeScopes() scopes = %v, want nil", scopes)
		}
		if len(missing) != 1 || missing[0] != "write:repository" {
			t.Errorf("VerifyAutoMergeScopes() missing = %v, want [write:repository]", missing)
		}
	})

	t.Run("permissions.push true yields the fail-open sentinel", func(t *testing.T) {
		t.Parallel()

		fixture := loadFixture(t, "repo_permissions_push_true.json")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fixture)
		}))
		defer srv.Close()

		adapter := mustSCMAdapterWithProject(t, srv.URL, testOwner+"/"+testRepo)
		scopes, missing, err := adapter.VerifyAutoMergeScopes(context.Background(), false)
		if err != nil {
			t.Fatalf("VerifyAutoMergeScopes: unexpected error: %v", err)
		}
		if scopes != nil || missing != nil {
			t.Errorf("VerifyAutoMergeScopes() = (%v, %v, nil), want (nil, nil, nil)", scopes, missing)
		}
	})

	t.Run("a 401 on the probe yields an auth-kind error, not transport-class", func(t *testing.T) {
		t.Parallel()

		errorBody := loadFixture(t, "error_401.json")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write(errorBody)
		}))
		defer srv.Close()

		adapter := mustSCMAdapterWithProject(t, srv.URL, testOwner+"/"+testRepo)
		scopes, missing, err := adapter.VerifyAutoMergeScopes(context.Background(), false)
		assertSCMErrorKind(t, err, domain.ErrSCMAuth)
		if scopes != nil || missing != nil {
			t.Errorf("VerifyAutoMergeScopes() = (%v, %v, err), want (nil, nil, err)", scopes, missing)
		}
	})

	t.Run("a 5xx on the probe yields a transport-kind error", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()

		adapter := mustSCMAdapterWithProject(t, srv.URL, testOwner+"/"+testRepo)
		// domain.ErrSCMTransport is the transport-class kind the orchestrator's
		// startup preflight uses to schedule a one-shot retry; asserted directly
		// here rather than importing the orchestrator package.
		_, _, err := adapter.VerifyAutoMergeScopes(context.Background(), false)
		assertSCMErrorKind(t, err, domain.ErrSCMTransport)
	})

	t.Run("construction with a project hint issues GET /repos/{owner}/{repo}", func(t *testing.T) {
		t.Parallel()

		var gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(loadFixture(t, "repo_permissions_push_true.json"))
		}))
		defer srv.Close()

		adapter := mustSCMAdapterWithProject(t, srv.URL, testOwner+"/"+testRepo)
		if _, _, err := adapter.VerifyAutoMergeScopes(context.Background(), false); err != nil {
			t.Fatalf("VerifyAutoMergeScopes: unexpected error: %v", err)
		}

		wantPath := "/api/v1/repos/" + testOwner + "/" + testRepo
		if gotPath != wantPath {
			t.Errorf("request path = %q, want %q", gotPath, wantPath)
		}
	})

	t.Run("no project key issues no request and returns the fail-open sentinel", func(t *testing.T) {
		t.Parallel()

		var requests atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL) // no "project" key configured
		scopes, missing, err := adapter.VerifyAutoMergeScopes(context.Background(), false)
		if err != nil {
			t.Fatalf("VerifyAutoMergeScopes: unexpected error: %v", err)
		}
		if scopes != nil || missing != nil {
			t.Errorf("VerifyAutoMergeScopes() = (%v, %v, nil), want (nil, nil, nil)", scopes, missing)
		}
		if n := requests.Load(); n != 0 {
			t.Errorf("requests = %d, want 0 (an absent project hint must not reach the network)", n)
		}
	})

	t.Run("a malformed project value leaves the hints empty and fails open with no request", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name    string
			project string
		}{
			{"no slash", "a"},
			{"too many slashes", "a/b/c"},
			{"empty owner", "/b"},
			{"empty repo", "a/"},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				var requests atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					w.WriteHeader(http.StatusInternalServerError)
				}))
				defer srv.Close()

				adapter := mustSCMAdapterWithProject(t, srv.URL, tt.project)
				scopes, missing, err := adapter.VerifyAutoMergeScopes(context.Background(), false)
				if err != nil {
					t.Fatalf("VerifyAutoMergeScopes: unexpected error: %v", err)
				}
				if scopes != nil || missing != nil {
					t.Errorf("VerifyAutoMergeScopes() = (%v, %v, nil), want (nil, nil, nil)", scopes, missing)
				}
				if n := requests.Load(); n != 0 {
					t.Errorf("requests = %d, want 0 (a malformed project must not reach the network)", n)
				}
			})
		}
	})
}

// --- parseBracketList ---

func TestParseBracketList(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		detail string
		prefix string
		want   []string
	}{
		{
			name:   "comma-separated scopes inside the bracket",
			detail: "required=[write:repository, read:user]",
			prefix: "required=[",
			want:   []string{"write:repository", "read:user"},
		},
		{
			name:   "single scope",
			detail: "token scope=[read:repository]",
			prefix: "token scope=[",
			want:   []string{"read:repository"},
		},
		{
			name:   "missing prefix yields nil",
			detail: "no bracket here at all",
			prefix: "required=[",
			want:   nil,
		},
		{
			name:   "missing closing bracket (a truncated body) yields nil",
			detail: "required=[write:repository, read:user",
			prefix: "required=[",
			want:   nil,
		},
		{
			name:   "empty bracket yields nil",
			detail: "required=[]",
			prefix: "required=[",
			want:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := parseBracketList(tt.detail, tt.prefix)
			if !slices.Equal(got, tt.want) {
				t.Errorf("parseBracketList(%q, %q) = %v, want %v", tt.detail, tt.prefix, got, tt.want)
			}
		})
	}
}
