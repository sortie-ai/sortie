package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

// --- GetReviewDecision tests ---

func TestGetReviewDecision_Approved(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/reviews") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			// Two reviewers: one approved, one bot-approved. Only human counts.
			_, _ = w.Write([]byte(`[
				{"user":{"login":"alice","type":"User"},"state":"APPROVED","submitted_at":"2026-01-02T10:00:00Z"},
				{"user":{"login":"ci-bot","type":"Bot"},"state":"APPROVED","submitted_at":"2026-01-02T11:00:00Z"}
			]`))
		}
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	decision, err := a.GetReviewDecision(t.Context(), 1, "owner", "repo")
	if err != nil {
		t.Fatalf("GetReviewDecision: %v", err)
	}
	if decision != domain.ReviewDecisionApproved {
		t.Errorf("GetReviewDecision() = %q, want %q", decision, domain.ReviewDecisionApproved)
	}
}

func TestGetReviewDecision_ChangesRequested(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/reviews") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[
				{"user":{"login":"alice","type":"User"},"state":"APPROVED","submitted_at":"2026-01-02T10:00:00Z"},
				{"user":{"login":"bob","type":"User"},"state":"CHANGES_REQUESTED","submitted_at":"2026-01-02T11:00:00Z"}
			]`))
		}
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	decision, err := a.GetReviewDecision(t.Context(), 1, "owner", "repo")
	if err != nil {
		t.Fatalf("GetReviewDecision: %v", err)
	}
	if decision != domain.ReviewDecisionChangesRequested {
		t.Errorf("GetReviewDecision() = %q, want %q", decision, domain.ReviewDecisionChangesRequested)
	}
}

func TestGetReviewDecision_BotChangesRequestedIgnored(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/reviews") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			// Only the bot has CHANGES_REQUESTED; human approved.
			_, _ = w.Write([]byte(`[
				{"user":{"login":"alice","type":"User"},"state":"APPROVED","submitted_at":"2026-01-02T10:00:00Z"},
				{"user":{"login":"ci-bot","type":"Bot"},"state":"CHANGES_REQUESTED","submitted_at":"2026-01-02T11:00:00Z"}
			]`))
		}
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	decision, err := a.GetReviewDecision(t.Context(), 1, "owner", "repo")
	if err != nil {
		t.Fatalf("GetReviewDecision: %v", err)
	}
	if decision != domain.ReviewDecisionApproved {
		t.Errorf("GetReviewDecision() = %q, want %q (bot CHANGES_REQUESTED must be ignored)", decision, domain.ReviewDecisionApproved)
	}
}

func TestGetReviewDecision_NoReviews_ReturnsNotRequired(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/reviews") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	decision, err := a.GetReviewDecision(t.Context(), 1, "owner", "repo")
	if err != nil {
		t.Fatalf("GetReviewDecision: %v", err)
	}
	if decision != domain.ReviewDecisionNotRequired {
		t.Errorf("GetReviewDecision() = %q, want %q (no reviews)", decision, domain.ReviewDecisionNotRequired)
	}
}

// --- GetCIStatus tests ---

func TestGetCIStatus_Success(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/pulls/"):
			_, _ = w.Write([]byte(`{"head":{"sha":"abc123","ref":"feature/x"},"draft":false,"mergeable_state":"clean"}`))
		case strings.Contains(r.URL.Path, "/status"):
			_, _ = w.Write([]byte(`{"state":"success","statuses":[{"state":"success"}]}`))
		case strings.Contains(r.URL.Path, "/check-runs"):
			_, _ = w.Write([]byte(`{"total_count":1,"check_runs":[{"status":"completed","conclusion":"success"}]}`))
		}
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	status, err := a.GetCIStatus(t.Context(), 1, "owner", "repo")
	if err != nil {
		t.Fatalf("GetCIStatus: %v", err)
	}
	if status != "success" {
		t.Errorf("GetCIStatus() = %q, want %q", status, "success")
	}
}

func TestGetCIStatus_Pending(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/pulls/"):
			_, _ = w.Write([]byte(`{"head":{"sha":"abc123"},"draft":false,"mergeable_state":"clean"}`))
		case strings.Contains(r.URL.Path, "/status"):
			_, _ = w.Write([]byte(`{"state":"pending","statuses":[{"state":"pending"}]}`))
		case strings.Contains(r.URL.Path, "/check-runs"):
			_, _ = w.Write([]byte(`{"total_count":0,"check_runs":[]}`))
		}
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	status, err := a.GetCIStatus(t.Context(), 1, "owner", "repo")
	if err != nil {
		t.Fatalf("GetCIStatus: %v", err)
	}
	if status != "pending" {
		t.Errorf("GetCIStatus() = %q, want %q", status, "pending")
	}
}

func TestGetCIStatus_NoSignals_ReturnsEmpty(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/pulls/"):
			_, _ = w.Write([]byte(`{"head":{"sha":"abc123"},"draft":false,"mergeable_state":"clean"}`))
		case strings.Contains(r.URL.Path, "/status"):
			_, _ = w.Write([]byte(`{"state":"","statuses":[]}`))
		case strings.Contains(r.URL.Path, "/check-runs"):
			_, _ = w.Write([]byte(`{"total_count":0,"check_runs":[]}`))
		}
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	status, err := a.GetCIStatus(t.Context(), 1, "owner", "repo")
	if err != nil {
		t.Fatalf("GetCIStatus: %v", err)
	}
	if status != "" {
		t.Errorf("GetCIStatus() = %q, want empty (no signals)", status)
	}
}

// --- GetMergeability tests ---

func TestGetMergeability_Clean(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"head":{"sha":"sha1","ref":"feature/x"},"draft":false,"mergeable_state":"clean"}`))
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	status, err := a.GetMergeability(t.Context(), 1, "owner", "repo")
	if err != nil {
		t.Fatalf("GetMergeability: %v", err)
	}
	if status.Mergeability != domain.MergeabilityClean {
		t.Errorf("GetMergeability().Mergeability = %q, want %q", status.Mergeability, domain.MergeabilityClean)
	}
	if status.Draft {
		t.Error("GetMergeability().Draft = true, want false")
	}
	if status.HeadSHA != "sha1" {
		t.Errorf("GetMergeability().HeadSHA = %q, want %q", status.HeadSHA, "sha1")
	}
}

func TestGetMergeability_Draft(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"head":{"sha":"sha-draft"},"draft":true,"mergeable_state":"clean"}`))
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	status, err := a.GetMergeability(t.Context(), 1, "owner", "repo")
	if err != nil {
		t.Fatalf("GetMergeability: %v", err)
	}
	if !status.Draft {
		t.Error("GetMergeability().Draft = false, want true for draft PR")
	}
}

// --- mapMergeableState tests ---

func TestMapMergeableState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  domain.MergeabilityState
	}{
		{"clean", domain.MergeabilityClean},
		{"CLEAN", domain.MergeabilityClean},
		{"unstable", domain.MergeabilityUnstable},
		{"blocked", domain.MergeabilityBlocked},
		{"behind", domain.MergeabilityBlocked},
		{"draft", domain.MergeabilityBlocked},
		{"dirty", domain.MergeabilityDirty},
		{"unknown", domain.MergeabilityUnknown},
		{"", domain.MergeabilityUnknown},
		{"computing", domain.MergeabilityUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()
			got := mapMergeableState(tt.input)
			if got != tt.want {
				t.Errorf("mapMergeableState(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// --- MergePR tests ---

func TestMergePR_Success(t *testing.T) {
	t.Parallel()

	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"sha":"merged-sha-1","merged":true,"message":"Pull Request successfully merged"}`))
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	result, err := a.MergePR(t.Context(), 1, "owner", "repo", domain.StrategySquash, "Title", "Body", "expected-sha")
	if err != nil {
		t.Fatalf("MergePR: %v", err)
	}
	if !result.Merged {
		t.Error("MergeResult.Merged = false, want true")
	}
	if result.SHA != "merged-sha-1" {
		t.Errorf("MergeResult.SHA = %q, want %q", result.SHA, "merged-sha-1")
	}
	if gotBody["merge_method"] != "squash" {
		t.Errorf("request merge_method = %q, want %q", gotBody["merge_method"], "squash")
	}
	if gotBody["sha"] != "expected-sha" {
		t.Errorf("request sha = %q, want %q", gotBody["sha"], "expected-sha")
	}
}

func TestMergePR_MergedFalse_ReturnsConflict(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"sha":"","merged":false,"message":"not merged"}`))
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	_, err := a.MergePR(t.Context(), 1, "owner", "repo", domain.StrategyMerge, "", "", "")
	assertSCMErrorKind(t, err, domain.ErrSCMConflict)
}

func TestMergePR_405_ReturnsSCMConflict(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = w.Write([]byte(`{"message":"Branch protection rules reject merge"}`))
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	_, err := a.MergePR(t.Context(), 1, "owner", "repo", domain.StrategySquash, "", "", "sha")
	assertSCMErrorKind(t, err, domain.ErrSCMConflict)
}

func TestMergePR_409_ReturnsSCMConflict(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"message":"Head branch was modified. Review and try the merge again."}`))
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	_, err := a.MergePR(t.Context(), 1, "owner", "repo", domain.StrategySquash, "", "", "sha")
	assertSCMErrorKind(t, err, domain.ErrSCMConflict)
}

// --- DeleteBranch tests ---

func TestDeleteBranch_Success(t *testing.T) {
	t.Parallel()

	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	if err := a.DeleteBranch(t.Context(), "owner", "repo", "feature/done"); err != nil {
		t.Fatalf("DeleteBranch: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("request method = %q, want DELETE", gotMethod)
	}
	if !strings.Contains(gotPath, "feature") {
		t.Errorf("request path = %q; want path containing branch name", gotPath)
	}
}

func TestDeleteBranch_404_ReturnsSCMNotFound(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Reference does not exist"}`))
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	err := a.DeleteBranch(t.Context(), "owner", "repo", "already-gone")
	assertSCMErrorKind(t, err, domain.ErrSCMNotFound)
}

// --- VerifyAutoMergeScopes tests ---

func TestVerifyAutoMergeScopes_LegacyRepoScope(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-OAuth-Scopes", "repo, gist")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"rate":{"limit":5000}}`))
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	scopes, missing, err := a.VerifyAutoMergeScopes(t.Context(), true)
	if err != nil {
		t.Fatalf("VerifyAutoMergeScopes: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("missing = %v, want nil (legacy repo scope covers all)", missing)
	}
	if len(scopes) == 0 {
		t.Error("scopes is empty; want at least [repo gist]")
	}
}

func TestVerifyAutoMergeScopes_MissingScope(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Provide contents:write but not pull_requests:write.
		w.Header().Set("X-OAuth-Scopes", "contents:write")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"rate":{"limit":5000}}`))
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	_, missing, err := a.VerifyAutoMergeScopes(t.Context(), true)
	if err != nil {
		t.Fatalf("VerifyAutoMergeScopes: %v", err)
	}
	if len(missing) == 0 {
		t.Fatal("missing is empty; want [pull_requests:write]")
	}
	if missing[0] != "pull_requests:write" {
		t.Errorf("missing[0] = %q, want %q", missing[0], "pull_requests:write")
	}
}

func TestVerifyAutoMergeScopes_AllFineGrainedScopes(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-OAuth-Scopes", "pull_requests:write, contents:write")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"rate":{"limit":5000}}`))
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	_, missing, err := a.VerifyAutoMergeScopes(t.Context(), true)
	if err != nil {
		t.Fatalf("VerifyAutoMergeScopes: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("missing = %v, want nil", missing)
	}
}

func TestVerifyAutoMergeScopes_TransportError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	_, _, err := a.VerifyAutoMergeScopes(t.Context(), false)
	if err == nil {
		t.Fatal("VerifyAutoMergeScopes err = nil; want error on 503")
	}
	assertSCMErrorKind(t, err, domain.ErrSCMTransport)
}

// TestVerifyAutoMergeScopes_EmptyHeaderReturnsUnableToVerify verifies that an
// empty X-OAuth-Scopes header is reported as "unable to verify" (nil scopes,
// nil missing, nil error) rather than as every required scope missing.
// Fine-grained PATs and GitHub App installation tokens commonly omit content
// from this header.
func TestVerifyAutoMergeScopes_EmptyHeaderReturnsUnableToVerify(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-OAuth-Scopes", "")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"rate":{"limit":5000}}`))
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	scopes, missing, err := a.VerifyAutoMergeScopes(t.Context(), true)
	if err != nil {
		t.Fatalf("VerifyAutoMergeScopes: %v", err)
	}
	if scopes != nil {
		t.Errorf("scopes = %v, want nil (unable to verify)", scopes)
	}
	if missing != nil {
		t.Errorf("missing = %v, want nil (unable to verify)", missing)
	}
}

// TestVerifyAutoMergeScopes_AbsentHeaderReturnsUnableToVerify verifies that a
// response with no X-OAuth-Scopes header at all is treated identically to an
// empty header: nil scopes, nil missing, nil error.
func TestVerifyAutoMergeScopes_AbsentHeaderReturnsUnableToVerify(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"rate":{"limit":5000}}`))
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	scopes, missing, err := a.VerifyAutoMergeScopes(t.Context(), true)
	if err != nil {
		t.Fatalf("VerifyAutoMergeScopes: %v", err)
	}
	if scopes != nil {
		t.Errorf("scopes = %v, want nil (unable to verify)", scopes)
	}
	if missing != nil {
		t.Errorf("missing = %v, want nil (unable to verify)", missing)
	}
}

// TestVerifyAutoMergeScopes_WhitespaceOnlyHeaderReturnsUnableToVerify verifies
// that a header containing only whitespace is treated as empty.
func TestVerifyAutoMergeScopes_WhitespaceOnlyHeaderReturnsUnableToVerify(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-OAuth-Scopes", "   ")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"rate":{"limit":5000}}`))
	}))
	defer srv.Close()

	a := newTestSCMAdapter(t, srv.URL)
	scopes, missing, err := a.VerifyAutoMergeScopes(t.Context(), true)
	if err != nil {
		t.Fatalf("VerifyAutoMergeScopes: %v", err)
	}
	if scopes != nil {
		t.Errorf("scopes = %v, want nil (unable to verify)", scopes)
	}
	if missing != nil {
		t.Errorf("missing = %v, want nil (unable to verify)", missing)
	}
}

// --- splitScopes tests ---

func TestSplitScopes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  []string
	}{
		{"", nil},
		{"repo", []string{"repo"}},
		{"repo, gist", []string{"repo", "gist"}},
		{"pull_requests:write, contents:write", []string{"pull_requests:write", "contents:write"}},
		{"  repo ,  gist  ", []string{"repo", "gist"}},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()
			got := splitScopes(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("splitScopes(%q) = %v, want %v", tt.input, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("splitScopes(%q)[%d] = %q, want %q", tt.input, i, got[i], tt.want[i])
				}
			}
		})
	}
}
