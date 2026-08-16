package gitea

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/adaptertest"
	"github.com/sortie-ai/sortie/internal/domain"
)

// giteaReviewsAndCommentsHandler builds an httptest handler that serves review
// and comment fixtures from testdata. It handles:
//   - GET .../reviews -> reviewsFixture
//   - GET .../reviews/{id}/comments -> commentsFixture
func giteaReviewsAndCommentsHandler(t *testing.T, reviewsFixture, commentsFixture []byte) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/comments"):
			_, _ = w.Write(commentsFixture)
		case strings.HasSuffix(r.URL.Path, "/reviews"):
			_, _ = w.Write(reviewsFixture)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// giteaReviewDecisionHandler builds an httptest handler that serves a reviews
// list and a pull request object, the two reads GetReviewDecision issues.
func giteaReviewDecisionHandler(t *testing.T, reviewsFixture, prFixture []byte) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/reviews"):
			_, _ = w.Write(reviewsFixture)
		case strings.Contains(r.URL.Path, "/pulls/"):
			_, _ = w.Write(prFixture)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// --- FetchPendingReviews ---

func TestGiteaSCMFetchPendingReviews(t *testing.T) {
	t.Parallel()

	t.Run("selects only the non-dismissed REQUEST_CHANGES review", func(t *testing.T) {
		t.Parallel()

		reviewsFixture := loadFixture(t, "reviews_pending_mixed.json")
		commentsFixture := loadFixture(t, "review_comments_mixed.json")
		srv := httptest.NewServer(giteaReviewsAndCommentsHandler(t, reviewsFixture, commentsFixture))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		got, err := adapter.FetchPendingReviews(context.Background(), 6, testOwner, testRepo)
		if err != nil {
			t.Fatalf("FetchPendingReviews: unexpected error: %v", err)
		}

		// review-101's body plus its two inline comments (1001 new-side, 1002
		// old-side); the APPROVED, COMMENT, and dismissed REQUEST_CHANGES
		// reviews must all be excluded.
		if len(got) != 3 {
			t.Fatalf("FetchPendingReviews() len = %d, want 3 (review body + 2 inline comments)", len(got))
		}

		byID := make(map[string]domain.ReviewComment, len(got))
		for _, c := range got {
			byID[c.ID] = c
			if c.Outdated {
				t.Errorf("comment %q Outdated = true, want false for every comment this method returns", c.ID)
			}
		}

		body, ok := byID["review-101"]
		if !ok {
			t.Fatalf("FetchPendingReviews() missing PR-level comment %q", "review-101")
		}
		if body.Reviewer != "alice" {
			t.Errorf(`comment "review-101" Reviewer = %q, want %q`, body.Reviewer, "alice")
		}
		if body.FilePath != "" {
			t.Errorf(`comment "review-101" FilePath = %q, want empty`, body.FilePath)
		}
		if body.StartLine != 0 {
			t.Errorf(`comment "review-101" StartLine = %d, want 0`, body.StartLine)
		}

		newSide, ok := byID["1001"]
		if !ok {
			t.Fatalf("FetchPendingReviews() missing inline comment %q", "1001")
		}
		if newSide.StartLine != 12 {
			t.Errorf(`comment "1001" StartLine = %d, want 12 (position)`, newSide.StartLine)
		}
		if newSide.EndLine != 0 {
			t.Errorf(`comment "1001" EndLine = %d, want 0`, newSide.EndLine)
		}

		oldSide, ok := byID["1002"]
		if !ok {
			t.Fatalf("FetchPendingReviews() missing inline comment %q", "1002")
		}
		if oldSide.StartLine != 12 {
			t.Errorf(`comment "1002" StartLine = %d, want 12 (falls back to original_position)`, oldSide.StartLine)
		}
	})

	t.Run("new-side and old-side comments select distinct anchored lines", func(t *testing.T) {
		t.Parallel()

		// The committed fixture's new-side and old-side comments both anchor
		// to line 12 of the live-captured probe PR, so a defect that read the
		// wrong field (or a decode-level swap of Position and
		// OriginalPosition) would still produce the fixture's expected
		// StartLine by coincidence. These two comments carry distinct lines
		// specifically so the two fields cannot be confused; they are inline
		// JSON, not a committed testdata fixture, so the fixture shape
		// contract's live-capture requirement does not apply.
		const reviewsFixture = `[{"id":401,"state":"REQUEST_CHANGES","body":"","user":{"login":"alice"},"dismissed":false,"submitted_at":"2026-07-14T09:00:00Z"}]`
		const commentsFixture = `[
			{"id":4001,"path":"a.go","body":"new side","position":7,"original_position":0,"user":{"login":"alice"},"created_at":"2026-07-14T09:00:00Z"},
			{"id":4002,"path":"a.go","body":"old side","position":0,"original_position":19,"user":{"login":"alice"},"created_at":"2026-07-14T09:00:00Z"}
		]`
		srv := httptest.NewServer(giteaReviewsAndCommentsHandler(t, []byte(reviewsFixture), []byte(commentsFixture)))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		got, err := adapter.FetchPendingReviews(context.Background(), 6, testOwner, testRepo)
		if err != nil {
			t.Fatalf("FetchPendingReviews: unexpected error: %v", err)
		}

		byID := make(map[string]domain.ReviewComment, len(got))
		for _, c := range got {
			byID[c.ID] = c
		}

		newSide, ok := byID["4001"]
		if !ok {
			t.Fatalf("FetchPendingReviews() missing inline comment %q", "4001")
		}
		if newSide.StartLine != 7 {
			t.Errorf(`comment "4001" StartLine = %d, want 7 (position)`, newSide.StartLine)
		}

		oldSide, ok := byID["4002"]
		if !ok {
			t.Fatalf("FetchPendingReviews() missing inline comment %q", "4002")
		}
		if oldSide.StartLine != 19 {
			t.Errorf(`comment "4002" StartLine = %d, want 19 (original_position)`, oldSide.StartLine)
		}
	})

	t.Run("a file-scoped comment normalizes to StartLine 0 with its FilePath preserved", func(t *testing.T) {
		t.Parallel()

		reviewsFixture := loadFixture(t, "reviews_pending_mixed.json")
		commentsFixture := loadFixture(t, "review_comments_file_scoped.json")
		srv := httptest.NewServer(giteaReviewsAndCommentsHandler(t, reviewsFixture, commentsFixture))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		got, err := adapter.FetchPendingReviews(context.Background(), 6, testOwner, testRepo)
		if err != nil {
			t.Fatalf("FetchPendingReviews: unexpected error: %v", err)
		}

		var found bool
		for _, c := range got {
			if c.ID != "1003" {
				continue
			}
			found = true
			if c.StartLine != 0 {
				t.Errorf(`comment "1003" StartLine = %d, want 0`, c.StartLine)
			}
			if c.FilePath != "spec778-target.txt" {
				t.Errorf(`comment "1003" FilePath = %q, want %q`, c.FilePath, "spec778-target.txt")
			}
			if c.Outdated {
				t.Error(`comment "1003" Outdated = true, want false`)
			}
		}
		if !found {
			t.Fatalf("FetchPendingReviews() missing file-scoped inline comment %q", "1003")
		}
	})

	t.Run("side selection precedence", func(t *testing.T) {
		t.Parallel()

		// Every row constructs a giteaReviewComment inline rather than
		// loading a testdata fixture: the "both non-zero" and "negative"
		// rows are not producible by the upstream converter, and the fixture
		// shape contract's live-capture requirement applies only to
		// committed testdata files.
		tests := []struct {
			name             string
			position         int
			originalPosition int
			want             int
		}{
			{"new side only", 42, 0, 42},
			{"old side only", 0, 17, 17},
			{"neither side (file-scoped)", 0, 0, 0},
			{"both non-zero prefers the new side", 99, 55, 99},
			{"negative position falls back to the positive original", -3, 8, 8},
			{"negative in both fields yields zero", -1, -1, 0},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				c := giteaReviewComment{
					ID:               9001,
					Path:             "both-sides.go",
					Body:             "constructed, not a platform shape",
					Position:         tt.position,
					OriginalPosition: tt.originalPosition,
					User:             giteaUser{Login: "alice"},
					CreatedAt:        "2026-07-14T09:00:00Z",
				}

				got := normalizeReviewComment(c)
				if got.StartLine != tt.want {
					t.Errorf("normalizeReviewComment(Position=%d, OriginalPosition=%d) StartLine = %d, want %d",
						tt.position, tt.originalPosition, got.StartLine, tt.want)
				}
				if got.Outdated {
					t.Errorf("normalizeReviewComment(Position=%d, OriginalPosition=%d) Outdated = true, want false",
						tt.position, tt.originalPosition)
				}
			})
		}
	})

	t.Run("empty reviews returns a non-nil empty slice", func(t *testing.T) {
		t.Parallel()

		reviewsFixture := loadFixture(t, "reviews_empty.json")
		commentsFixture := loadFixture(t, "review_comments_empty.json")
		srv := httptest.NewServer(giteaReviewsAndCommentsHandler(t, reviewsFixture, commentsFixture))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		got, err := adapter.FetchPendingReviews(context.Background(), 6, testOwner, testRepo)
		if err != nil {
			t.Fatalf("FetchPendingReviews: unexpected error: %v", err)
		}
		adaptertest.AssertEmptyNonNil(t, got, "FetchPendingReviews")
	})

	t.Run("malformed reviews response is a payload error", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`not valid json {{{`))
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		_, err := adapter.FetchPendingReviews(context.Background(), 6, testOwner, testRepo)
		assertSCMErrorKind(t, err, domain.ErrSCMPayload)
	})

	t.Run("a malformed review submitted_at does not fail the read and normalizes to the zero time", func(t *testing.T) {
		t.Parallel()

		const reviewsFixture = `[{"id":201,"state":"REQUEST_CHANGES","body":"Fix this","user":{"login":"alice"},"dismissed":false,"submitted_at":"not-a-timestamp"}]`
		const commentsFixture = `[{"id":3001,"path":"a.go","body":"nit","position":5,"original_position":5,"user":{"login":"alice"},"created_at":"2026-07-14T09:00:00Z"}]`
		srv := httptest.NewServer(giteaReviewsAndCommentsHandler(t, []byte(reviewsFixture), []byte(commentsFixture)))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		got, err := adapter.FetchPendingReviews(context.Background(), 6, testOwner, testRepo)
		adaptertest.AssertReviewCommentTimestampTolerated(t, got, err)
	})
}

// --- FetchBotReviewComments ---

func TestGiteaSCMFetchBotReviewComments(t *testing.T) {
	t.Parallel()

	t.Run("allowlisted author selected across COMMENT and REQUEST_CHANGES", func(t *testing.T) {
		t.Parallel()

		reviewsFixture := loadFixture(t, "reviews_bot_allowlisted.json")
		commentsFixture := loadFixture(t, "review_comments_empty.json")
		srv := httptest.NewServer(giteaReviewsAndCommentsHandler(t, reviewsFixture, commentsFixture))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		got, err := adapter.FetchBotReviewComments(context.Background(), 6, testOwner, testRepo, []string{"sortie-ci-bot"})
		if err != nil {
			t.Fatalf("FetchBotReviewComments: unexpected error: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("FetchBotReviewComments() len = %d, want 2 (COMMENT and REQUEST_CHANGES bodies)", len(got))
		}

		ids := make(map[string]bool, len(got))
		for _, c := range got {
			ids[c.ID] = true
			if c.Reviewer != "sortie-ci-bot" {
				t.Errorf("FetchBotReviewComments() comment %q Reviewer = %q, want %q", c.ID, c.Reviewer, "sortie-ci-bot")
			}
		}
		if !ids["review-201"] || !ids["review-202"] {
			t.Errorf("FetchBotReviewComments() ids = %v, want review-201 and review-202", ids)
		}
	})

	t.Run("empty allowlist selects nothing", func(t *testing.T) {
		t.Parallel()

		reviewsFixture := loadFixture(t, "reviews_bot_allowlisted.json")
		commentsFixture := loadFixture(t, "review_comments_empty.json")
		srv := httptest.NewServer(giteaReviewsAndCommentsHandler(t, reviewsFixture, commentsFixture))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		got, err := adapter.FetchBotReviewComments(context.Background(), 6, testOwner, testRepo, nil)
		if err != nil {
			t.Fatalf("FetchBotReviewComments: unexpected error: %v", err)
		}
		adaptertest.AssertEmptyNonNil(t, got, "FetchBotReviewComments")
	})

	t.Run("allowlist match is case-insensitive", func(t *testing.T) {
		t.Parallel()

		reviewsFixture := loadFixture(t, "reviews_bot_allowlisted.json")
		commentsFixture := loadFixture(t, "review_comments_empty.json")
		srv := httptest.NewServer(giteaReviewsAndCommentsHandler(t, reviewsFixture, commentsFixture))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		got, err := adapter.FetchBotReviewComments(context.Background(), 6, testOwner, testRepo, []string{"SORTIE-CI-BOT"})
		if err != nil {
			t.Fatalf("FetchBotReviewComments: unexpected error: %v", err)
		}
		if len(got) != 2 {
			t.Errorf("FetchBotReviewComments() len = %d, want 2 (case-insensitive allowlist match)", len(got))
		}
	})

	t.Run("inline comments are filtered by comment author, not just review author", func(t *testing.T) {
		t.Parallel()

		reviewsFixture := loadFixture(t, "reviews_bot_single.json")
		commentsFixture := loadFixture(t, "review_comments_bot_mixed.json")
		srv := httptest.NewServer(giteaReviewsAndCommentsHandler(t, reviewsFixture, commentsFixture))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		got, err := adapter.FetchBotReviewComments(context.Background(), 6, testOwner, testRepo, []string{"sortie-ci-bot"})
		if err != nil {
			t.Fatalf("FetchBotReviewComments: unexpected error: %v", err)
		}

		// The allowlisted bot review contributes its body (review-301) and its
		// one bot-authored inline comment (3001, old-side); the inline comment
		// by the non-allowlisted author (3002) is dropped by the
		// comment-author predicate even though its parent review is
		// allowlisted.
		if len(got) != 2 {
			t.Fatalf("FetchBotReviewComments() len = %d, want 2 (review body + bot inline comment)", len(got))
		}

		byID := make(map[string]domain.ReviewComment, len(got))
		for _, c := range got {
			byID[c.ID] = c
			if c.Outdated {
				t.Errorf("comment %q Outdated = true, want false for every comment this method returns", c.ID)
			}
		}

		if _, ok := byID["review-301"]; !ok {
			t.Errorf("FetchBotReviewComments() missing PR-level comment %q", "review-301")
		}

		botComment, ok := byID["3001"]
		if !ok {
			t.Fatalf("FetchBotReviewComments() missing bot inline comment %q", "3001")
		}
		if botComment.Reviewer != "sortie-ci-bot" {
			t.Errorf(`comment "3001" Reviewer = %q, want %q`, botComment.Reviewer, "sortie-ci-bot")
		}
		if botComment.StartLine != 12 {
			t.Errorf(`comment "3001" StartLine = %d, want 12 (falls back to original_position)`, botComment.StartLine)
		}
		if botComment.Outdated {
			t.Error(`comment "3001" Outdated = true, want false`)
		}

		if _, present := byID["3002"]; present {
			t.Errorf(`comment "3002" present, want excluded (author "mallory" is not in the allowlist)`)
		}
	})
}

// --- GetReviewDecision ---

func TestGiteaSCMGetReviewDecision(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		reviewsFixture string
		prFixture      string
		want           domain.ReviewDecision
	}{
		{
			name:           "a later approval supersedes an earlier changes-requested by the same reviewer",
			reviewsFixture: "reviews_supersede_same_reviewer.json",
			prFixture:      "pr_clean.json",
			want:           domain.ReviewDecisionApproved,
		},
		{
			name:           "one reviewer's changes-requested alongside another's approval",
			reviewsFixture: "reviews_multi_reviewer_changes_requested.json",
			prFixture:      "pr_clean.json",
			want:           domain.ReviewDecisionChangesRequested,
		},
		{
			name:           "a dismissed changes-requested does not block a live approval",
			reviewsFixture: "reviews_dismissed_with_live_approval.json",
			prFixture:      "pr_clean.json",
			want:           domain.ReviewDecisionApproved,
		},
		{
			name:           "a dismissed changes-requested alone is not required",
			reviewsFixture: "reviews_dismissed_only.json",
			prFixture:      "pr_clean.json",
			want:           domain.ReviewDecisionNotRequired,
		},
		{
			name:           "a pending reviewer request with no submitted decision",
			reviewsFixture: "reviews_empty.json",
			prFixture:      "pr_requested_reviewers.json",
			want:           domain.ReviewDecisionReviewRequired,
		},
		{
			name:           "no reviews and no requested reviewers",
			reviewsFixture: "reviews_empty.json",
			prFixture:      "pr_clean.json",
			want:           domain.ReviewDecisionNotRequired,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reviewsFixture := loadFixture(t, tt.reviewsFixture)
			prFixture := loadFixture(t, tt.prFixture)
			srv := httptest.NewServer(giteaReviewDecisionHandler(t, reviewsFixture, prFixture))
			defer srv.Close()

			adapter := mustSCMAdapter(t, srv.URL)
			got, err := adapter.GetReviewDecision(context.Background(), 6, testOwner, testRepo)
			if err != nil {
				t.Fatalf("GetReviewDecision: unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("GetReviewDecision() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestGiteaSCMGetReviewDecision_EqualTimestampTiebreak pins the review id
// as the tiebreaker when two decision-bearing reviews by one reviewer share
// a submitted_at. The sort is not stable, so without the tiebreaker the
// winning review is unspecified. The fixture serves the reviews in
// descending id order so a fold that preserved wire order would yield the
// opposite verdict.
func TestGiteaSCMGetReviewDecision_EqualTimestampTiebreak(t *testing.T) {
	t.Parallel()

	const reviewsFixture = `[
		{"id":607,"state":"REQUEST_CHANGES","body":"","user":{"login":"alice"},"dismissed":false,"submitted_at":"2026-07-14T09:00:00Z"},
		{"id":606,"state":"APPROVED","body":"","user":{"login":"alice"},"dismissed":false,"submitted_at":"2026-07-14T09:00:00Z"}
	]`
	prFixture := loadFixture(t, "pr_clean.json")
	srv := httptest.NewServer(giteaReviewDecisionHandler(t, []byte(reviewsFixture), prFixture))
	defer srv.Close()

	adapter := mustSCMAdapter(t, srv.URL)
	got, err := adapter.GetReviewDecision(context.Background(), 6, testOwner, testRepo)
	if err != nil {
		t.Fatalf("GetReviewDecision: unexpected error: %v", err)
	}
	if got != domain.ReviewDecisionChangesRequested {
		t.Errorf("GetReviewDecision() = %q, want %q (review 607 outranks 606 on the id tiebreak)",
			got, domain.ReviewDecisionChangesRequested)
	}
}

// TestGiteaSCMGetReviewDecision_MalformedTimestamp pins the fold's strict
// disposition: a decision-bearing review's malformed submitted_at fails
// the read, while the same malformed value on a review the fold never
// parses (dismissed, or a non-decision state) leaves the verdict
// unaffected.
func TestGiteaSCMGetReviewDecision_MalformedTimestamp(t *testing.T) {
	t.Parallel()

	t.Run("a decision-bearing review with a malformed submitted_at fails the read", func(t *testing.T) {
		t.Parallel()

		const reviewsFixture = `[{"id":601,"state":"APPROVED","body":"","user":{"login":"alice"},"dismissed":false,"submitted_at":"not-a-timestamp"}]`
		prFixture := loadFixture(t, "pr_clean.json")
		srv := httptest.NewServer(giteaReviewDecisionHandler(t, []byte(reviewsFixture), prFixture))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		got, err := adapter.GetReviewDecision(context.Background(), 6, testOwner, testRepo)
		adaptertest.AssertSCMErrorKind(t, err, domain.ErrSCMPayload)
		if got != "" {
			t.Errorf("GetReviewDecision() = %q, want empty on failure", got)
		}
	})

	t.Run("a dismissed review with a malformed submitted_at does not block a live changes-requested verdict", func(t *testing.T) {
		t.Parallel()

		const reviewsFixture = `[
			{"id":602,"state":"REQUEST_CHANGES","body":"","user":{"login":"alice"},"dismissed":false,"submitted_at":"2026-07-14T09:00:00Z"},
			{"id":603,"state":"REQUEST_CHANGES","body":"","user":{"login":"bob"},"dismissed":true,"submitted_at":"not-a-timestamp"}
		]`
		prFixture := loadFixture(t, "pr_clean.json")
		srv := httptest.NewServer(giteaReviewDecisionHandler(t, []byte(reviewsFixture), prFixture))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		got, err := adapter.GetReviewDecision(context.Background(), 6, testOwner, testRepo)
		if err != nil {
			t.Fatalf("GetReviewDecision: unexpected error: %v", err)
		}
		if got != domain.ReviewDecisionChangesRequested {
			t.Errorf("GetReviewDecision() = %q, want %q", got, domain.ReviewDecisionChangesRequested)
		}
	})

	t.Run("a COMMENT review with a malformed submitted_at does not block a live approved verdict", func(t *testing.T) {
		t.Parallel()

		const reviewsFixture = `[
			{"id":604,"state":"APPROVED","body":"","user":{"login":"alice"},"dismissed":false,"submitted_at":"2026-07-14T09:00:00Z"},
			{"id":605,"state":"COMMENT","body":"","user":{"login":"bob"},"dismissed":false,"submitted_at":"not-a-timestamp"}
		]`
		prFixture := loadFixture(t, "pr_clean.json")
		srv := httptest.NewServer(giteaReviewDecisionHandler(t, []byte(reviewsFixture), prFixture))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		got, err := adapter.GetReviewDecision(context.Background(), 6, testOwner, testRepo)
		if err != nil {
			t.Fatalf("GetReviewDecision: unexpected error: %v", err)
		}
		if got != domain.ReviewDecisionApproved {
			t.Errorf("GetReviewDecision() = %q, want %q", got, domain.ReviewDecisionApproved)
		}
	})
}
