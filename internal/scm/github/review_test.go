package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

// --- helpers ---

func newTestSCMAdapter(t *testing.T, baseURL string) *GitHubSCMAdapter {
	t.Helper()
	a, err := NewGitHubSCMAdapter(map[string]any{
		"endpoint": baseURL,
		"api_key":  "test-token",
	})
	if err != nil {
		t.Fatalf("NewGitHubSCMAdapter: %v", err)
	}
	gh := a.(*GitHubSCMAdapter)
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatalf("http.DefaultTransport is %T, want *http.Transport", http.DefaultTransport)
	}
	transport := defaultTransport.Clone()
	gh.client.httpClient.Transport = transport
	t.Cleanup(transport.CloseIdleConnections)
	return gh
}

func assertSCMErrorKind(t *testing.T, err error, want domain.SCMErrorKind) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected SCMError with kind %q, got nil", want)
	}
	var se *domain.SCMError
	if !errors.As(err, &se) {
		t.Fatalf("error type = %T, want *domain.SCMError", err)
	}
	if se.Kind != want {
		t.Errorf("SCMError.Kind = %q, want %q", se.Kind, want)
	}
}

// reviewsAndCommentsHandler builds an httptest handler that serves review
// and comment fixtures from the testdata directory. It handles:
//   - GET .../reviews → reviewsFixture (no Link header)
//   - GET .../reviews/{id}/comments → commentsFixture (no Link header)
func reviewsAndCommentsHandler(t *testing.T, reviewsFixture, commentsFixture []byte) http.HandlerFunc {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case strings.Contains(path, "/comments"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(commentsFixture)
		case strings.HasSuffix(path, "/reviews"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(reviewsFixture)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found"}`))
		}
	})
}

// --- Constructor tests ---

func TestNewGitHubSCMAdapter_MissingAPIKey(t *testing.T) {
	t.Parallel()

	_, err := NewGitHubSCMAdapter(map[string]any{})
	assertSCMErrorKind(t, err, domain.ErrSCMAuth)
}

func TestNewGitHubSCMAdapter_Valid(t *testing.T) {
	t.Parallel()

	a, err := NewGitHubSCMAdapter(map[string]any{
		"api_key": "tok",
	})
	if err != nil {
		t.Fatalf("NewGitHubSCMAdapter: unexpected error: %v", err)
	}
	if a == nil {
		t.Fatal("NewGitHubSCMAdapter returned nil adapter")
	}
}

func TestNewGitHubSCMAdapter_DefaultEndpoint(t *testing.T) {
	t.Parallel()

	a, err := NewGitHubSCMAdapter(map[string]any{
		"api_key": "tok",
	})
	if err != nil {
		t.Fatalf("NewGitHubSCMAdapter: %v", err)
	}
	gh := a.(*GitHubSCMAdapter)
	if !strings.Contains(gh.client.baseURL, "api.github.com") {
		t.Errorf("default endpoint = %q, want to contain \"api.github.com\"", gh.client.baseURL)
	}
}

func TestNewGitHubSCMAdapter_CustomUserAgent(t *testing.T) {
	t.Parallel()

	a, err := NewGitHubSCMAdapter(map[string]any{
		"api_key":    "tok",
		"user_agent": "my-app/1.0",
	})
	if err != nil {
		t.Fatalf("NewGitHubSCMAdapter: %v", err)
	}
	gh := a.(*GitHubSCMAdapter)
	if gh.client.userAgent != "my-app/1.0" {
		t.Errorf("userAgent = %q, want %q", gh.client.userAgent, "my-app/1.0")
	}
}

// --- FetchPendingReviews tests ---

func TestFetchPendingReviews_NoReviews(t *testing.T) {
	t.Parallel()

	fixture := loadFixture(t, "reviews_empty.json")
	srv := httptest.NewServer(reviewsAndCommentsHandler(t, fixture, loadFixture(t, "comments_empty.json")))
	defer srv.Close()

	adapter := newTestSCMAdapter(t, srv.URL)
	got, err := adapter.FetchPendingReviews(context.Background(), 1, "owner", "repo")
	if err != nil {
		t.Fatalf("FetchPendingReviews: unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("FetchPendingReviews len = %d, want 0", len(got))
	}
	if got == nil {
		t.Error("FetchPendingReviews returned nil, want non-nil empty slice")
	}
}

func TestFetchPendingReviews_ApprovedOnly(t *testing.T) {
	t.Parallel()

	fixture := loadFixture(t, "reviews_approved.json")
	srv := httptest.NewServer(reviewsAndCommentsHandler(t, fixture, loadFixture(t, "comments_empty.json")))
	defer srv.Close()

	adapter := newTestSCMAdapter(t, srv.URL)
	got, err := adapter.FetchPendingReviews(context.Background(), 1, "owner", "repo")
	if err != nil {
		t.Fatalf("FetchPendingReviews: unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("FetchPendingReviews with APPROVED review: len = %d, want 0", len(got))
	}
}

func TestFetchPendingReviews_ChangesRequestedWithInlineComment(t *testing.T) {
	t.Parallel()

	reviewsFixture := loadFixture(t, "reviews_changes_requested_no_body.json")
	commentsFixture := loadFixture(t, "comments_inline.json")
	srv := httptest.NewServer(reviewsAndCommentsHandler(t, reviewsFixture, commentsFixture))
	defer srv.Close()

	adapter := newTestSCMAdapter(t, srv.URL)
	got, err := adapter.FetchPendingReviews(context.Background(), 1, "owner", "repo")
	if err != nil {
		t.Fatalf("FetchPendingReviews: unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("FetchPendingReviews len = %d, want 1", len(got))
	}

	c := got[0]
	if c.ID != "100" {
		t.Errorf("ID = %q, want %q", c.ID, "100")
	}
	if c.FilePath != "internal/handler.go" {
		t.Errorf("FilePath = %q, want %q", c.FilePath, "internal/handler.go")
	}
	if c.StartLine != 10 {
		t.Errorf("StartLine = %d, want 10", c.StartLine)
	}
	if c.EndLine != 12 {
		t.Errorf("EndLine = %d, want 12", c.EndLine)
	}
	if c.Reviewer != "alice" {
		t.Errorf("Reviewer = %q, want %q", c.Reviewer, "alice")
	}
	if c.Body == "" {
		t.Error("Body is empty, want non-empty")
	}
	if c.Outdated {
		t.Error("Outdated = true, want false (position is non-nil)")
	}
	if c.SubmittedAt.IsZero() {
		t.Error("SubmittedAt is zero, want parsed timestamp")
	}
}

func TestFetchPendingReviews_BotCommentFiltered(t *testing.T) {
	t.Parallel()

	reviewsFixture := loadFixture(t, "reviews_bot.json")
	srv := httptest.NewServer(reviewsAndCommentsHandler(t, reviewsFixture, loadFixture(t, "comments_empty.json")))
	defer srv.Close()

	adapter := newTestSCMAdapter(t, srv.URL)
	got, err := adapter.FetchPendingReviews(context.Background(), 1, "owner", "repo")
	if err != nil {
		t.Fatalf("FetchPendingReviews: unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("FetchPendingReviews with bot review: len = %d, want 0 (bot filtered)", len(got))
	}
}

func TestFetchPendingReviews_PRLevelBodyAppended(t *testing.T) {
	t.Parallel()

	reviewsFixture := loadFixture(t, "reviews_changes_requested_with_body.json")
	commentsFixture := loadFixture(t, "comments_empty.json")
	srv := httptest.NewServer(reviewsAndCommentsHandler(t, reviewsFixture, commentsFixture))
	defer srv.Close()

	adapter := newTestSCMAdapter(t, srv.URL)
	got, err := adapter.FetchPendingReviews(context.Background(), 1, "owner", "repo")
	if err != nil {
		t.Fatalf("FetchPendingReviews: unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("FetchPendingReviews len = %d, want 1 (PR-level body comment)", len(got))
	}

	c := got[0]
	if c.ID != "review-10" {
		t.Errorf("PR-level comment ID = %q, want %q", c.ID, "review-10")
	}
	if c.FilePath != "" {
		t.Errorf("PR-level comment FilePath = %q, want empty", c.FilePath)
	}
	if c.Body != "Please fix these issues before merging." {
		t.Errorf("PR-level comment Body = %q, want review body text", c.Body)
	}
	if c.Reviewer != "alice" {
		t.Errorf("PR-level comment Reviewer = %q, want %q", c.Reviewer, "alice")
	}
	if c.Outdated {
		t.Error("PR-level comment Outdated = true, want false")
	}
}

func TestFetchPendingReviews_OutdatedComment(t *testing.T) {
	t.Parallel()

	reviewsFixture := loadFixture(t, "reviews_changes_requested_no_body.json")
	commentsFixture := loadFixture(t, "comments_outdated.json")
	srv := httptest.NewServer(reviewsAndCommentsHandler(t, reviewsFixture, commentsFixture))
	defer srv.Close()

	adapter := newTestSCMAdapter(t, srv.URL)
	got, err := adapter.FetchPendingReviews(context.Background(), 1, "owner", "repo")
	if err != nil {
		t.Fatalf("FetchPendingReviews: unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("FetchPendingReviews len = %d, want 1", len(got))
	}
	if !got[0].Outdated {
		t.Error("Outdated = false, want true (position is nil)")
	}
}

func TestFetchPendingReviews_Deduplication(t *testing.T) {
	t.Parallel()

	// Review has both a body (becomes review-10) and one inline comment (100).
	// The same comment ID should not appear twice even if somehow duplicated.
	reviewsFixture := loadFixture(t, "reviews_changes_requested_with_body.json")
	commentsFixture := loadFixture(t, "comments_inline.json")
	srv := httptest.NewServer(reviewsAndCommentsHandler(t, reviewsFixture, commentsFixture))
	defer srv.Close()

	adapter := newTestSCMAdapter(t, srv.URL)
	got, err := adapter.FetchPendingReviews(context.Background(), 1, "owner", "repo")
	if err != nil {
		t.Fatalf("FetchPendingReviews: unexpected error: %v", err)
	}

	// Expect: review-level body comment + inline comment, deduped
	seen := make(map[string]int)
	for _, c := range got {
		seen[c.ID]++
	}
	for id, count := range seen {
		if count > 1 {
			t.Errorf("comment ID %q appears %d times, want 1 (deduplication failure)", id, count)
		}
	}
}

func TestFetchPendingReviews_Pagination_Reviews(t *testing.T) {
	t.Parallel()

	page1 := loadFixture(t, "reviews_page1.json")
	page2 := loadFixture(t, "reviews_page2.json")
	commentsPage1 := loadFixture(t, "comments_page1.json")
	commentsPage2 := loadFixture(t, "comments_page2.json")

	var reviewCallCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case strings.Contains(path, "/reviews/11/comments"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(commentsPage1)
		case strings.Contains(path, "/reviews/12/comments"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(commentsPage2)
		case strings.Contains(path, "/reviews"):
			n := reviewCallCount.Add(1)
			if n == 1 {
				// Return page 1 with Link next header.
				nextURL := fmt.Sprintf("http://%s/repos/owner/repo/pulls/1/reviews?page=2", r.Host)
				w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, nextURL))
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(page1)
			} else {
				// Return page 2 without Link header (last page).
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(page2)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	adapter := newTestSCMAdapter(t, srv.URL)
	got, err := adapter.FetchPendingReviews(context.Background(), 1, "owner", "repo")
	if err != nil {
		t.Fatalf("FetchPendingReviews: unexpected error: %v", err)
	}

	// Reviews page1 has review id=11 (comment 300), page2 has review id=12 (comment 301).
	// Total: 2 unique inline comments.
	if len(got) != 2 {
		t.Errorf("FetchPendingReviews paginated len = %d, want 2", len(got))
	}
	if reviewCallCount.Load() < 2 {
		t.Errorf("review page calls = %d, want >= 2 (pagination)", reviewCallCount.Load())
	}
}

func TestFetchPendingReviews_Pagination_Comments(t *testing.T) {
	t.Parallel()

	reviewsFixture := loadFixture(t, "reviews_changes_requested_no_body.json")
	commentsPage1 := loadFixture(t, "comments_page1.json")
	commentsPage2 := loadFixture(t, "comments_page2.json")

	var commentCallCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		switch {
		case strings.Contains(path, "/comments"):
			n := commentCallCount.Add(1)
			if n == 1 {
				nextURL := fmt.Sprintf("http://%s/repos/owner/repo/pulls/1/reviews/20/comments?page=2", r.Host)
				w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, nextURL))
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(commentsPage1)
			} else {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(commentsPage2)
			}
		case strings.Contains(path, "/reviews"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(reviewsFixture)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	adapter := newTestSCMAdapter(t, srv.URL)
	got, err := adapter.FetchPendingReviews(context.Background(), 1, "owner", "repo")
	if err != nil {
		t.Fatalf("FetchPendingReviews: unexpected error: %v", err)
	}

	// Comments page1 has comment 300, page2 has comment 301 — both non-outdated.
	if len(got) != 2 {
		t.Errorf("FetchPendingReviews with paginated comments len = %d, want 2", len(got))
	}
	if commentCallCount.Load() < 2 {
		t.Errorf("comment page calls = %d, want >= 2 (pagination)", commentCallCount.Load())
	}
}

func TestFetchPendingReviews_HTTP401(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	defer srv.Close()

	adapter := newTestSCMAdapter(t, srv.URL)
	_, err := adapter.FetchPendingReviews(context.Background(), 1, "owner", "repo")
	assertSCMErrorKind(t, err, domain.ErrSCMAuth)
}

func TestFetchPendingReviews_HTTP404(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer srv.Close()

	adapter := newTestSCMAdapter(t, srv.URL)
	_, err := adapter.FetchPendingReviews(context.Background(), 1, "owner", "repo")
	assertSCMErrorKind(t, err, domain.ErrSCMNotFound)
}

func TestFetchPendingReviews_TransportError(t *testing.T) {
	t.Parallel()

	// Server closes immediately after listen — all connections refused.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	adapter := newTestSCMAdapter(t, srv.URL)
	_, err := adapter.FetchPendingReviews(context.Background(), 1, "owner", "repo")
	assertSCMErrorKind(t, err, domain.ErrSCMTransport)
}

func TestFetchPendingReviews_JSONPayloadError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`not valid json {{{`))
	}))
	defer srv.Close()

	adapter := newTestSCMAdapter(t, srv.URL)
	_, err := adapter.FetchPendingReviews(context.Background(), 1, "owner", "repo")
	assertSCMErrorKind(t, err, domain.ErrSCMPayload)
}

// --- TestToSCMError ---

func TestToSCMError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    error
		wantKind domain.SCMErrorKind
	}{
		{
			name:     "ErrTrackerTransport → ErrSCMTransport",
			input:    &domain.TrackerError{Kind: domain.ErrTrackerTransport, Message: "net"},
			wantKind: domain.ErrSCMTransport,
		},
		{
			name:     "ErrTrackerAuth → ErrSCMAuth",
			input:    &domain.TrackerError{Kind: domain.ErrTrackerAuth, Message: "auth"},
			wantKind: domain.ErrSCMAuth,
		},
		{
			name:     "ErrTrackerAPI → ErrSCMAPI",
			input:    &domain.TrackerError{Kind: domain.ErrTrackerAPI, Message: "api"},
			wantKind: domain.ErrSCMAPI,
		},
		{
			name:     "ErrTrackerNotFound → ErrSCMNotFound",
			input:    &domain.TrackerError{Kind: domain.ErrTrackerNotFound, Message: "nf"},
			wantKind: domain.ErrSCMNotFound,
		},
		{
			name:     "ErrTrackerPayload → ErrSCMPayload",
			input:    &domain.TrackerError{Kind: domain.ErrTrackerPayload, Message: "pl"},
			wantKind: domain.ErrSCMPayload,
		},
		{
			name:     "unknown TrackerErrorKind → ErrSCMAPI",
			input:    &domain.TrackerError{Kind: "unknown_kind", Message: "x"},
			wantKind: domain.ErrSCMAPI,
		},
		{
			name:     "non-TrackerError → ErrSCMTransport",
			input:    fmt.Errorf("some generic transport error"),
			wantKind: domain.ErrSCMTransport,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := toSCMError(tt.input)
			var se *domain.SCMError
			if !errors.As(got, &se) {
				t.Fatalf("toSCMError returned %T, want *domain.SCMError", got)
			}
			if se.Kind != tt.wantKind {
				t.Errorf("SCMError.Kind = %q, want %q", se.Kind, tt.wantKind)
			}
		})
	}
}
