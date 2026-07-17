package gitea

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
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
