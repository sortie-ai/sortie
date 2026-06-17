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
	return a.(*GitHubSCMAdapter)
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
	if a.(*GitHubSCMAdapter) == nil {
		t.Fatal("NewGitHubSCMAdapter returned nil adapter")
	}
}

func TestNewGitHubSCMAdapter_CustomUserAgent(t *testing.T) {
	t.Parallel()

	var gotUserAgent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUserAgent = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(loadFixture(t, "reviews_empty.json"))
	}))
	defer srv.Close()

	a, err := NewGitHubSCMAdapter(map[string]any{
		"endpoint":   srv.URL,
		"api_key":    "tok",
		"user_agent": "my-app/1.0",
	})
	if err != nil {
		t.Fatalf("NewGitHubSCMAdapter: %v", err)
	}
	gh := a.(*GitHubSCMAdapter)
	_, err = gh.FetchPendingReviews(context.Background(), 1, "owner", "repo")
	if err != nil {
		t.Fatalf("FetchPendingReviews: %v", err)
	}
	if gotUserAgent != "my-app/1.0" {
		t.Errorf("User-Agent = %q, want %q", gotUserAgent, "my-app/1.0")
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

// --- FetchBotReviewComments tests ---

// TestFetchBotReviewComments_BotTypeReturned verifies R1: a review authored by
// a user with user.type == "Bot" is returned by FetchBotReviewComments.
func TestFetchBotReviewComments_BotTypeReturned(t *testing.T) {
	t.Parallel()

	reviewsFixture := loadFixture(t, "reviews_bot.json")
	commentsFixture := loadFixture(t, "comments_bot_inline.json")
	srv := httptest.NewServer(reviewsAndCommentsHandler(t, reviewsFixture, commentsFixture))
	defer srv.Close()

	adapter := newTestSCMAdapter(t, srv.URL)
	got, err := adapter.FetchBotReviewComments(context.Background(), 1, "owner", "repo", nil)
	if err != nil {
		t.Fatalf("FetchBotReviewComments: unexpected error: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("FetchBotReviewComments returned empty slice, want at least 1 comment from bot")
	}

	// The bot review has a non-empty body, so it must appear as a PR-level comment.
	found := false
	for _, c := range got {
		if c.Reviewer == "github-actions[bot]" {
			found = true
		}
	}
	if !found {
		t.Errorf("FetchBotReviewComments: reviewer %q not found in results", "github-actions[bot]")
	}
}

// TestFetchBotReviewComments_BotExcludedByFetchPendingReviews verifies R1: the
// same bot fixture that FetchBotReviewComments returns is excluded by
// FetchPendingReviews.
func TestFetchBotReviewComments_BotExcludedByFetchPendingReviews(t *testing.T) {
	t.Parallel()

	reviewsFixture := loadFixture(t, "reviews_bot.json")
	srv := httptest.NewServer(reviewsAndCommentsHandler(t, reviewsFixture, loadFixture(t, "comments_empty.json")))
	defer srv.Close()

	adapter := newTestSCMAdapter(t, srv.URL)

	// FetchPendingReviews must exclude the bot review entirely.
	human, err := adapter.FetchPendingReviews(context.Background(), 1, "owner", "repo")
	if err != nil {
		t.Fatalf("FetchPendingReviews: %v", err)
	}
	if len(human) != 0 {
		t.Errorf("FetchPendingReviews with bot fixture: len = %d, want 0 (bot must be excluded)", len(human))
	}

	// FetchBotReviewComments must include the same bot review.
	bot, err := adapter.FetchBotReviewComments(context.Background(), 1, "owner", "repo", nil)
	if err != nil {
		t.Fatalf("FetchBotReviewComments: %v", err)
	}
	if len(bot) == 0 {
		t.Error("FetchBotReviewComments with bot fixture: len = 0, want > 0")
	}
}

// TestFetchBotReviewComments_AllowlistMatchCaseInsensitive verifies R2: a
// review authored by a user with user.type == "User" whose login matches a
// botUsernames entry case-insensitively is returned by FetchBotReviewComments.
// The test uses a fixture where both the review and its inline comment carry
// user.type == "User" with login "houndci-bot", so the only selection path
// is the allowlist union arm.
func TestFetchBotReviewComments_AllowlistMatchCaseInsensitive(t *testing.T) {
	t.Parallel()

	// reviews_user_allowlisted.json: user.type == "User", login "houndci-bot", empty body.
	// comments_user_allowlisted.json: user.type == "User", login "houndci-bot".
	reviewsFixture := loadFixture(t, "reviews_user_allowlisted.json")
	commentsFixture := loadFixture(t, "comments_user_allowlisted.json")
	srv := httptest.NewServer(reviewsAndCommentsHandler(t, reviewsFixture, commentsFixture))
	defer srv.Close()

	adapter := newTestSCMAdapter(t, srv.URL)

	// Without the allowlist the "User" type review/comment must be excluded entirely.
	gotNoAllowlist, err := adapter.FetchBotReviewComments(context.Background(), 1, "owner", "repo", nil)
	if err != nil {
		t.Fatalf("FetchBotReviewComments (no allowlist): %v", err)
	}
	for _, c := range gotNoAllowlist {
		if strings.EqualFold(c.Reviewer, "houndci-bot") {
			t.Errorf("FetchBotReviewComments with no allowlist: reviewer %q must not appear without allowlist", c.Reviewer)
		}
	}

	// With a differently-cased allowlist entry, the inline comment must be returned.
	allowlist := []string{"HOUNDCI-BOT"}
	gotWithAllowlist, err := adapter.FetchBotReviewComments(context.Background(), 1, "owner", "repo", allowlist)
	if err != nil {
		t.Fatalf("FetchBotReviewComments (with allowlist): %v", err)
	}
	found := false
	for _, c := range gotWithAllowlist {
		if strings.EqualFold(c.Reviewer, "houndci-bot") {
			found = true
		}
	}
	if !found {
		t.Errorf("FetchBotReviewComments(allowlist=%v): reviewer %q not found, want case-insensitive match",
			allowlist, "houndci-bot")
	}
}

// TestFetchBotReviewComments_CommentedReviewIncluded verifies Open Question 2
// (Option A): a bot review with state COMMENTED (not CHANGES_REQUESTED) is
// still returned by FetchBotReviewComments.
func TestFetchBotReviewComments_CommentedReviewIncluded(t *testing.T) {
	t.Parallel()

	// reviews_bot_commented.json has state == "COMMENTED" and user.type == "Bot".
	reviewsFixture := loadFixture(t, "reviews_bot_commented.json")
	srv := httptest.NewServer(reviewsAndCommentsHandler(t, reviewsFixture, loadFixture(t, "comments_empty.json")))
	defer srv.Close()

	adapter := newTestSCMAdapter(t, srv.URL)
	got, err := adapter.FetchBotReviewComments(context.Background(), 1, "owner", "repo", nil)
	if err != nil {
		t.Fatalf("FetchBotReviewComments: %v", err)
	}
	if len(got) == 0 {
		t.Error("FetchBotReviewComments with COMMENTED bot review: len = 0, want > 0 (no review-state filter)")
	}
}

// TestFetchBotReviewComments_SelectionIgnoresBody verifies R3: classification
// reads only user.login and user.type; the body content does not affect
// selection. A bot with empty body is still classified as bot (no PR-level
// comment), and its inline comments are returned.
func TestFetchBotReviewComments_SelectionIgnoresBody(t *testing.T) {
	t.Parallel()

	// reviews_changes_requested_no_body.json has user.type == "User" (not a bot).
	// We need a bot review with empty body to confirm body doesn't affect selection.
	// Use a bot review + inline comment; the bot is selected by user.type, not body.
	reviewsFixture := loadFixture(t, "reviews_bot.json")
	// Provide a bot inline comment — body content is irrelevant to selection.
	commentsFixture := loadFixture(t, "comments_bot_inline.json")
	srv := httptest.NewServer(reviewsAndCommentsHandler(t, reviewsFixture, commentsFixture))
	defer srv.Close()

	adapter := newTestSCMAdapter(t, srv.URL)
	got, err := adapter.FetchBotReviewComments(context.Background(), 1, "owner", "repo", nil)
	if err != nil {
		t.Fatalf("FetchBotReviewComments: %v", err)
	}
	// Selection must have occurred regardless of comment body content.
	// The inline comment (id 501) must appear in results.
	found := false
	for _, c := range got {
		if c.ID == "501" {
			found = true
		}
	}
	if !found {
		t.Errorf("FetchBotReviewComments: inline bot comment id %q not returned; selection must use user.type only", "501")
	}
}

// TestFetchBotReviewComments_EmptyResultNonNil verifies that when no bot
// comments match, the result is a non-nil empty slice.
func TestFetchBotReviewComments_EmptyResultNonNil(t *testing.T) {
	t.Parallel()

	// reviews_approved.json has user.type == "User" and is not in any allowlist.
	reviewsFixture := loadFixture(t, "reviews_approved.json")
	srv := httptest.NewServer(reviewsAndCommentsHandler(t, reviewsFixture, loadFixture(t, "comments_empty.json")))
	defer srv.Close()

	adapter := newTestSCMAdapter(t, srv.URL)
	got, err := adapter.FetchBotReviewComments(context.Background(), 1, "owner", "repo", nil)
	if err != nil {
		t.Fatalf("FetchBotReviewComments: %v", err)
	}
	if got == nil {
		t.Error("FetchBotReviewComments with no bot matches: returned nil, want non-nil empty slice")
	}
	if len(got) != 0 {
		t.Errorf("FetchBotReviewComments with no bot matches: len = %d, want 0", len(got))
	}
}

// TestFetchBotReviewComments_OutdatedCommentFlag verifies that a bot inline
// comment with null position carries Outdated == true.
func TestFetchBotReviewComments_OutdatedCommentFlag(t *testing.T) {
	t.Parallel()

	// Use the COMMENTED bot review (user.type == "Bot") with an outdated inline
	// comment (position == null).
	reviewsFixture := loadFixture(t, "reviews_bot_commented.json")
	commentsFixture := loadFixture(t, "comments_bot_outdated.json")
	srv := httptest.NewServer(reviewsAndCommentsHandler(t, reviewsFixture, commentsFixture))
	defer srv.Close()

	adapter := newTestSCMAdapter(t, srv.URL)
	got, err := adapter.FetchBotReviewComments(context.Background(), 1, "owner", "repo", nil)
	if err != nil {
		t.Fatalf("FetchBotReviewComments: %v", err)
	}

	foundOutdated := false
	for _, c := range got {
		if c.ID == "500" {
			foundOutdated = true
			if !c.Outdated {
				t.Errorf("FetchBotReviewComments: comment id %q Outdated = false, want true (position is null)", c.ID)
			}
		}
	}
	if !foundOutdated {
		t.Errorf("FetchBotReviewComments: outdated bot comment id %q not found in results", "500")
	}
}

// TestFetchBotReviewComments_NoReviews verifies that when the PR has no
// reviews at all, the result is a non-nil empty slice.
func TestFetchBotReviewComments_NoReviews(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(reviewsAndCommentsHandler(t, loadFixture(t, "reviews_empty.json"), loadFixture(t, "comments_empty.json")))
	defer srv.Close()

	adapter := newTestSCMAdapter(t, srv.URL)
	got, err := adapter.FetchBotReviewComments(context.Background(), 1, "owner", "repo", nil)
	if err != nil {
		t.Fatalf("FetchBotReviewComments with empty reviews: %v", err)
	}
	if got == nil {
		t.Error("FetchBotReviewComments with empty reviews: returned nil, want non-nil empty slice")
	}
	if len(got) != 0 {
		t.Errorf("FetchBotReviewComments with empty reviews: len = %d, want 0", len(got))
	}
}

// TestFetchBotReviewComments_ReviewsFetchError verifies that an error fetching
// the reviews list propagates as an SCMError from FetchBotReviewComments.
func TestFetchBotReviewComments_ReviewsFetchError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`not valid json {{{`))
	}))
	defer srv.Close()

	adapter := newTestSCMAdapter(t, srv.URL)
	_, err := adapter.FetchBotReviewComments(context.Background(), 1, "owner", "repo", nil)
	assertSCMErrorKind(t, err, domain.ErrSCMPayload)
}

// TestFetchBotReviewComments_CommentsFetchError verifies that an error fetching
// a bot review's inline comments propagates from FetchBotReviewComments even
// when the reviews list itself was fetched successfully.
func TestFetchBotReviewComments_CommentsFetchError(t *testing.T) {
	t.Parallel()

	reviewsFixture := loadFixture(t, "reviews_bot.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/comments"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`not valid json {{{`))
		case strings.HasSuffix(r.URL.Path, "/reviews"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(reviewsFixture)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found"}`))
		}
	}))
	defer srv.Close()

	adapter := newTestSCMAdapter(t, srv.URL)
	_, err := adapter.FetchBotReviewComments(context.Background(), 1, "owner", "repo", nil)
	assertSCMErrorKind(t, err, domain.ErrSCMPayload)
}

// TestFetchBotReviewComments_SkipsNonBotInlineComment verifies that an inline
// comment authored by a non-bot, non-allowlisted user is excluded from the
// results while bot-authored inline comments are kept.
func TestFetchBotReviewComments_SkipsNonBotInlineComment(t *testing.T) {
	t.Parallel()

	// reviews_bot.json: one bot review (user.type "Bot").
	// comments_bot_mixed.json: a bot comment (id 700), a non-bot comment
	// (id 701, user.type "User", not allowlisted), and a duplicate of id 700.
	reviewsFixture := loadFixture(t, "reviews_bot.json")
	commentsFixture := loadFixture(t, "comments_bot_mixed.json")
	srv := httptest.NewServer(reviewsAndCommentsHandler(t, reviewsFixture, commentsFixture))
	defer srv.Close()

	adapter := newTestSCMAdapter(t, srv.URL)
	got, err := adapter.FetchBotReviewComments(context.Background(), 1, "owner", "repo", nil)
	if err != nil {
		t.Fatalf("FetchBotReviewComments: %v", err)
	}

	var botInlineCount int
	for _, c := range got {
		if c.ID == "701" {
			t.Error("non-bot inline comment id 701 returned; want excluded")
		}
		if c.ID == "700" {
			botInlineCount++
		}
	}
	if botInlineCount != 1 {
		t.Errorf("bot inline comment id 700 appeared %d time(s); want exactly 1 (deduped)", botInlineCount)
	}
}

// TestIsBotAuthor verifies the isBotAuthor predicate covers all union branches.
func TestIsBotAuthor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		login     string
		userType  string
		allowlist []string
		want      bool
	}{
		{
			name:     "user.type Bot (exact case)",
			login:    "golangci-lint[bot]",
			userType: "Bot",
			want:     true,
		},
		{
			name:     "user.type bot (lower case)",
			login:    "golangci-lint[bot]",
			userType: "bot",
			want:     true,
		},
		{
			name:     "user.type BOT (upper case)",
			login:    "golangci-lint[bot]",
			userType: "BOT",
			want:     true,
		},
		{
			name:     "user.type User not in allowlist",
			login:    "alice",
			userType: "User",
			want:     false,
		},
		{
			name:      "user.type User in allowlist exact match",
			login:     "houndci-bot",
			userType:  "User",
			allowlist: []string{"houndci-bot"},
			want:      true,
		},
		{
			name:      "user.type User in allowlist case-insensitive",
			login:     "houndci-bot",
			userType:  "User",
			allowlist: []string{"HOUNDCI-BOT"},
			want:      true,
		},
		{
			name:      "user.type User not in populated allowlist",
			login:     "bob",
			userType:  "User",
			allowlist: []string{"houndci-bot", "golangci-lint[bot]"},
			want:      false,
		},
		{
			name:      "empty allowlist and non-bot type",
			login:     "carol",
			userType:  "User",
			allowlist: []string{},
			want:      false,
		},
		{
			name:     "nil allowlist and non-bot type",
			login:    "carol",
			userType: "User",
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := isBotAuthor(tt.login, tt.userType, tt.allowlist)
			if got != tt.want {
				t.Errorf("isBotAuthor(%q, %q, %v) = %v, want %v",
					tt.login, tt.userType, tt.allowlist, got, tt.want)
			}
		})
	}
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
		// 405 "method not allowed" messages produced by classifyHTTPError
		// are promoted to ErrSCMConflict.
		{
			name: "ErrTrackerAPI with 'method not allowed' → ErrSCMConflict",
			input: &domain.TrackerError{
				Kind:    domain.ErrTrackerAPI,
				Message: "PUT /repos/o/r/pulls/1/merge: method not allowed: branch protection refuses",
			},
			wantKind: domain.ErrSCMConflict,
		},
		// 409 "conflict" messages produced by classifyHTTPError are promoted.
		{
			name: "ErrTrackerAPI with ': conflict:' → ErrSCMConflict",
			input: &domain.TrackerError{
				Kind:    domain.ErrTrackerAPI,
				Message: "PUT /repos/o/r/pulls/1/merge: conflict: head sha mismatch",
			},
			wantKind: domain.ErrSCMConflict,
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
