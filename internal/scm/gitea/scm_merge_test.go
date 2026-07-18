package gitea

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

// --- GetMergeability ---

func TestGiteaSCMGetMergeability(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		fixture        string
		wantMergeable  domain.MergeabilityState
		wantDraft      bool
		wantHeadSHA    string
		wantBranchName string
		wantBaseBranch string
	}{
		{
			name:           "mergeable true maps to clean",
			fixture:        "pr_clean.json",
			wantMergeable:  domain.MergeabilityClean,
			wantDraft:      false,
			wantHeadSHA:    "sha-clean-001",
			wantBranchName: "feature/gitea-scm-reads",
			wantBaseBranch: "main",
		},
		{
			name:           "draft true maps to blocked even when mergeable is true",
			fixture:        "pr_draft.json",
			wantMergeable:  domain.MergeabilityBlocked,
			wantDraft:      true,
			wantHeadSHA:    "sha-draft-002",
			wantBranchName: "feature/wip-cache",
			wantBaseBranch: "main",
		},
		{
			name:           "mergeable false maps to unknown",
			fixture:        "pr_conflict_unknown.json",
			wantMergeable:  domain.MergeabilityUnknown,
			wantDraft:      false,
			wantHeadSHA:    "sha-conflict-003",
			wantBranchName: "feature/conflict",
			wantBaseBranch: "develop",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := loadFixture(t, tt.fixture)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(fixture)
			}))
			defer srv.Close()

			adapter := mustSCMAdapter(t, srv.URL)
			got, err := adapter.GetMergeability(context.Background(), 6, testOwner, testRepo)
			if err != nil {
				t.Fatalf("GetMergeability: unexpected error: %v", err)
			}

			if got.Mergeability != tt.wantMergeable {
				t.Errorf("GetMergeability().Mergeability = %q, want %q", got.Mergeability, tt.wantMergeable)
			}
			if got.Draft != tt.wantDraft {
				t.Errorf("GetMergeability().Draft = %v, want %v", got.Draft, tt.wantDraft)
			}
			if got.HeadSHA != tt.wantHeadSHA {
				t.Errorf("GetMergeability().HeadSHA = %q, want %q", got.HeadSHA, tt.wantHeadSHA)
			}
			if got.BranchName != tt.wantBranchName {
				t.Errorf("GetMergeability().BranchName = %q, want %q", got.BranchName, tt.wantBranchName)
			}
			if got.BaseBranch != tt.wantBaseBranch {
				t.Errorf("GetMergeability().BaseBranch = %q, want %q", got.BaseBranch, tt.wantBaseBranch)
			}
			if got.ReviewDecision != "" {
				t.Errorf("GetMergeability().ReviewDecision = %q, want empty (left unset)", got.ReviewDecision)
			}
			if got.CIConclusion != "" {
				t.Errorf("GetMergeability().CIConclusion = %q, want empty (left unset)", got.CIConclusion)
			}
		})
	}
}

// --- GetCIStatus ---

func TestGiteaSCMGetCIStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		statusFixture string
		want          string
	}{
		{"zero total_count maps to empty, not pending", "status_no_checks.json", ""},
		{"all statuses success", "status_all_success.json", "success"},
		{"a failure among successes marks failing", "status_has_failure.json", "failing"},
		{"a pending with no failure marks pending", "status_has_pending.json", "pending"},
		{"a warning among successes is non-failing", "status_warning_only.json", "success"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			prFixture := loadFixture(t, "pr_clean.json")
			statusFixture := loadFixture(t, tt.statusFixture)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.Contains(r.URL.Path, "/status"):
					_, _ = w.Write(statusFixture)
				case strings.Contains(r.URL.Path, "/pulls/"):
					_, _ = w.Write(prFixture)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()

			adapter := mustSCMAdapter(t, srv.URL)
			got, err := adapter.GetCIStatus(context.Background(), 6, testOwner, testRepo)
			if err != nil {
				t.Fatalf("GetCIStatus: unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("GetCIStatus() = %q, want %q", got, tt.want)
			}
		})
	}

	t.Run("missing head sha is a payload error", func(t *testing.T) {
		t.Parallel()

		prFixture := loadFixture(t, "pr_missing_head_sha.json")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(prFixture)
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		_, err := adapter.GetCIStatus(context.Background(), 6, testOwner, testRepo)
		assertSCMErrorKind(t, err, domain.ErrSCMPayload)
	})
}

// --- MergePR ---

func TestGiteaSCMMergePR(t *testing.T) {
	t.Parallel()

	t.Run("success sends Do mapped from the strategy and head_commit_id, returns Merged true with an empty SHA", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name     string
			strategy domain.MergeStrategy
			wantDo   string
		}{
			{"merge strategy maps to Do merge", domain.StrategyMerge, "merge"},
			{"squash strategy maps to Do squash", domain.StrategySquash, "squash"},
			{"rebase strategy maps to Do rebase", domain.StrategyRebase, "rebase"},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				const wantSHA = "sha-expected-head-001"
				var gotMethod, gotPath string
				var gotBody giteaMergeOption
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					gotMethod = r.Method
					gotPath = r.URL.Path
					decodeRequestBody(t, r, &gotBody)
					w.WriteHeader(http.StatusOK)
				}))
				defer srv.Close()

				adapter := mustSCMAdapter(t, srv.URL)
				got, err := adapter.MergePR(context.Background(), 6, testOwner, testRepo, tt.strategy, "", "", wantSHA)
				if err != nil {
					t.Fatalf("MergePR: unexpected error: %v", err)
				}

				if gotMethod != http.MethodPost {
					t.Errorf("request method = %q, want %q", gotMethod, http.MethodPost)
				}
				wantPath := "/api/v1/repos/" + testOwner + "/" + testRepo + "/pulls/6/merge"
				if gotPath != wantPath {
					t.Errorf("request path = %q, want %q", gotPath, wantPath)
				}
				if gotBody.Do != tt.wantDo {
					t.Errorf("request body Do = %q, want %q", gotBody.Do, tt.wantDo)
				}
				if gotBody.HeadCommitID != wantSHA {
					t.Errorf("request body head_commit_id = %q, want %q", gotBody.HeadCommitID, wantSHA)
				}
				if !got.Merged {
					t.Errorf("MergePR().Merged = %v, want true", got.Merged)
				}
				if got.SHA != "" {
					t.Errorf("MergePR().SHA = %q, want empty (gitea's success body carries no sha)", got.SHA)
				}
			})
		}
	})

	t.Run("an unsupported strategy returns ErrSCMPayload before any request is issued", func(t *testing.T) {
		t.Parallel()

		var requests atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		got, err := adapter.MergePR(context.Background(), 6, testOwner, testRepo, domain.MergeStrategy("bogus"), "", "", "")
		assertSCMErrorKind(t, err, domain.ErrSCMPayload)
		if got != (domain.MergeResult{}) {
			t.Errorf("MergePR(...) result = %+v, want the zero value", got)
		}
		if n := requests.Load(); n != 0 {
			t.Errorf("requests = %d, want 0 (an unsupported strategy must not reach the network)", n)
		}
	})

	t.Run("an already-merged 405 plus a merged:true re-read yields a conflict carrying the already-merged marker", func(t *testing.T) {
		t.Parallel()

		mergeFixture := loadFixture(t, "merge_already_merged_405.json")
		prFixture := loadFixture(t, "pr_reread_merged_true.json")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case strings.HasSuffix(r.URL.Path, "/merge"):
				w.WriteHeader(http.StatusMethodNotAllowed)
				_, _ = w.Write(mergeFixture)
			case strings.Contains(r.URL.Path, "/pulls/"):
				_, _ = w.Write(prFixture)
			default:
				t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		_, err := adapter.MergePR(context.Background(), 6, testOwner, testRepo, domain.StrategyMerge, "", "", "sha-expected-head-001")

		se := assertSCMConflict(t, err)
		if !strings.Contains(strings.ToLower(se.Message), "already merged") {
			t.Errorf("SCMError.Message = %q, want it to contain %q (case-insensitive)", se.Message, "already merged")
		}
	})

	t.Run("a stale 409 plus a merged:false re-read yields a conflict without the already-merged marker", func(t *testing.T) {
		t.Parallel()

		mergeFixture := loadFixture(t, "merge_stale_conflict_409.json")
		prFixture := loadFixture(t, "pr_reread_merged_false.json")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case strings.HasSuffix(r.URL.Path, "/merge"):
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write(mergeFixture)
			case strings.Contains(r.URL.Path, "/pulls/"):
				_, _ = w.Write(prFixture)
			default:
				t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		_, err := adapter.MergePR(context.Background(), 6, testOwner, testRepo, domain.StrategyMerge, "", "", "sha-expected-head-001")

		se := assertSCMConflict(t, err)
		if strings.Contains(strings.ToLower(se.Message), "already merged") {
			t.Errorf("SCMError.Message = %q, want it NOT to contain %q (a stale precondition is not an already-merged success)", se.Message, "already merged")
		}
	})

	t.Run("a 405 whose body omits the marker still yields it when the re-read shows merged:true", func(t *testing.T) {
		t.Parallel()

		// A reworded 405 body that does not itself contain "already merged": the
		// marker must be synthesized from the merged:true re-read rather than read
		// from Gitea's phrasing, so this fails for a resolver that skips the re-read.
		const rewordedBody = `{"message":"this pull request is in a merged state"}`
		prFixture := loadFixture(t, "pr_reread_merged_true.json")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case strings.HasSuffix(r.URL.Path, "/merge"):
				w.WriteHeader(http.StatusMethodNotAllowed)
				_, _ = w.Write([]byte(rewordedBody))
			case strings.Contains(r.URL.Path, "/pulls/"):
				_, _ = w.Write(prFixture)
			default:
				t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		_, err := adapter.MergePR(context.Background(), 6, testOwner, testRepo, domain.StrategyMerge, "", "", "sha-expected-head-001")

		se := assertSCMConflict(t, err)
		if !strings.Contains(strings.ToLower(se.Message), "already merged") {
			t.Errorf("SCMError.Message = %q, want the marker synthesized from the merged:true re-read, not Gitea's 405 text", se.Message)
		}
	})

	t.Run("a 403 naming a required scope is enriched to name write:repository", func(t *testing.T) {
		t.Parallel()

		errorBody := loadFixture(t, "error_403_scope_write_repository.json")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write(errorBody)
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		_, err := adapter.MergePR(context.Background(), 6, testOwner, testRepo, domain.StrategyMerge, "", "", "sha-expected-head-001")
		assertSCMErrorKind(t, err, domain.ErrSCMAuth)

		var se *domain.SCMError
		if !errors.As(err, &se) {
			t.Fatalf("error type = %T, want *domain.SCMError", err)
		}
		if !strings.Contains(se.Message, "write:repository") {
			t.Errorf("SCMError.Message = %q, want it to contain %q", se.Message, "write:repository")
		}
	})

	t.Run("a truncated 403 scope body still names write:repository via the literal fallback", func(t *testing.T) {
		t.Parallel()

		errorBody := loadFixture(t, "error_403_scope_truncated.json")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write(errorBody)
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		_, err := adapter.MergePR(context.Background(), 6, testOwner, testRepo, domain.StrategyMerge, "", "", "sha-expected-head-001")
		assertSCMErrorKind(t, err, domain.ErrSCMAuth)

		var se *domain.SCMError
		if !errors.As(err, &se) {
			t.Fatalf("error type = %T, want *domain.SCMError", err)
		}
		if !strings.Contains(se.Message, "write:repository") {
			t.Errorf("SCMError.Message = %q, want it to contain %q even when the body is truncated", se.Message, "write:repository")
		}
	})
}

// assertSCMConflict fails the test if err is not a *domain.SCMError of kind
// ErrSCMConflict, returning the typed error for further message inspection.
func assertSCMConflict(t *testing.T, err error) *domain.SCMError {
	t.Helper()
	assertSCMErrorKind(t, err, domain.ErrSCMConflict)
	var se *domain.SCMError
	if !errors.As(err, &se) {
		t.Fatalf("error type = %T, want *domain.SCMError", err)
	}
	return se
}

// --- DeleteBranch ---

func TestGiteaSCMDeleteBranch(t *testing.T) {
	t.Parallel()

	t.Run("204 success returns nil", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		if err := adapter.DeleteBranch(context.Background(), testOwner, testRepo, "feature/done"); err != nil {
			t.Fatalf("DeleteBranch: unexpected error: %v", err)
		}
	})

	t.Run("an already-gone branch returns the not-found error rather than nil", func(t *testing.T) {
		t.Parallel()

		errorBody := loadFixture(t, "error_branch_not_found.json")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write(errorBody)
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		err := adapter.DeleteBranch(context.Background(), testOwner, testRepo, "feature/gone")
		assertSCMErrorKind(t, err, domain.ErrSCMNotFound)
	})

	t.Run("a slash-bearing branch name reaches the server percent-encoded", func(t *testing.T) {
		t.Parallel()

		var gotPath, gotEscapedPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			gotEscapedPath = r.URL.EscapedPath()
			w.WriteHeader(http.StatusNoContent)
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		if err := adapter.DeleteBranch(context.Background(), testOwner, testRepo, "feature/needs-slash"); err != nil {
			t.Fatalf("DeleteBranch: unexpected error: %v", err)
		}

		wantPath := "/api/v1/repos/" + testOwner + "/" + testRepo + "/branches/feature/needs-slash"
		if gotPath != wantPath {
			t.Errorf("decoded request path = %q, want %q", gotPath, wantPath)
		}
		if !strings.Contains(gotEscapedPath, "%2F") {
			t.Errorf("escaped request path = %q, want it to contain %q (the branch slash percent-encoded)", gotEscapedPath, "%2F")
		}
	})

	t.Run("a 403 naming a required scope is enriched to name write:repository", func(t *testing.T) {
		t.Parallel()

		errorBody := loadFixture(t, "error_403_scope_write_repository.json")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write(errorBody)
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		err := adapter.DeleteBranch(context.Background(), testOwner, testRepo, "feature/blocked")
		assertSCMErrorKind(t, err, domain.ErrSCMAuth)

		var se *domain.SCMError
		if !errors.As(err, &se) {
			t.Fatalf("error type = %T, want *domain.SCMError", err)
		}
		if !strings.Contains(se.Message, "write:repository") {
			t.Errorf("SCMError.Message = %q, want it to contain %q", se.Message, "write:repository")
		}
	})
}
