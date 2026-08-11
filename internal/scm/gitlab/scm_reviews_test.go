package gitlab

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sortie-ai/sortie/internal/adaptertest"
	"github.com/sortie-ai/sortie/internal/domain"
)

// reviewsRoutingHandler serves reviewersBody at .../reviewers, notesBody at
// .../notes, and delegates GET /users/:id to userHandler. Any other
// request fails the test, so a code path calling a route no subtest
// expects is caught rather than silently ignored.
func reviewsRoutingHandler(t *testing.T, reviewersBody, notesBody []byte, userHandler http.HandlerFunc) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/users/"):
			userHandler(w, r)
		case strings.HasSuffix(r.URL.Path, "/notes"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(notesBody)
		case strings.HasSuffix(r.URL.Path, "/reviewers"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(reviewersBody)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// userDetailHandler serves body for every GET /users/:id request and
// counts how many were issued.
func userDetailHandler(t *testing.T, body []byte, requests *atomic.Int32) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}
}

// failingLookupHandler serves the fixture pair a single requested_changes
// human reviewer (bob, id 503) and their one note, with GET /users/503
// always answering 500. Two tests share this exact shape: FetchPendingReviews
// must degrade and continue, FetchBotReviewComments must abort.
func failingLookupHandler(t *testing.T) http.HandlerFunc {
	t.Helper()
	reviewersBody := loadFixture(t, "reviewers_single_human_changes_requested.json")
	notesBody := loadFixture(t, "notes_single_author.json")
	errorBody := loadFixture(t, "error_500.json")
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/users/503"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write(errorBody)
		case strings.HasSuffix(r.URL.Path, "/notes"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(notesBody)
		case strings.HasSuffix(r.URL.Path, "/reviewers"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(reviewersBody)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// --- FetchPendingReviews ---

func TestFetchPendingReviews_ExcludesBotReviewer(t *testing.T) {
	t.Parallel()

	reviewersBody := loadFixture(t, "reviewers_human_and_bot_changes_requested.json")
	notesBody := loadFixture(t, "notes_human_and_bot_mixed.json")
	humanDetail := loadFixture(t, "user_detail_bot_false.json")
	botDetail := loadFixture(t, "user_detail_bot_true.json")

	userHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/users/501"):
			_, _ = w.Write(humanDetail)
		case strings.Contains(r.URL.Path, "/users/502"):
			_, _ = w.Write(botDetail)
		default:
			t.Errorf("unexpected user lookup: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}

	srv := httptest.NewServer(reviewsRoutingHandler(t, reviewersBody, notesBody, userHandler))
	defer srv.Close()

	adapter := mustSCMAdapter(t, srv.URL)
	got, err := adapter.FetchPendingReviews(context.Background(), testPRNumber, scmOwner, scmRepo)
	if err != nil {
		t.Fatalf("FetchPendingReviews: unexpected error: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("FetchPendingReviews() len = %d, want 1 (only the human reviewer's note)", len(got))
	}
	if got[0].ID != "9002" || got[0].Reviewer != "alice" {
		t.Errorf("FetchPendingReviews()[0] = %+v, want ID=9002 Reviewer=alice", got[0])
	}
}

func TestFetchPendingReviews_NoBlockingReviewer(t *testing.T) {
	t.Parallel()

	reviewersBody := loadFixture(t, "reviewers_bot_only_changes_requested.json")
	botDetail := loadFixture(t, "user_detail_bot_true.json")

	var notesRequests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/users/502"):
			_, _ = w.Write(botDetail)
		case strings.HasSuffix(r.URL.Path, "/notes"):
			notesRequests.Add(1)
			t.Error("notes route hit; want no notes request when no reviewer survives the bot exclusion")
			_, _ = w.Write([]byte(`[]`))
		case strings.HasSuffix(r.URL.Path, "/reviewers"):
			_, _ = w.Write(reviewersBody)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	adapter := mustSCMAdapter(t, srv.URL)
	got, err := adapter.FetchPendingReviews(context.Background(), testPRNumber, scmOwner, scmRepo)
	if err != nil {
		t.Fatalf("FetchPendingReviews: unexpected error: %v", err)
	}
	adaptertest.AssertEmptyNonNil(t, got, "FetchPendingReviews")
	if n := notesRequests.Load(); n != 0 {
		t.Errorf("notes requests = %d, want 0", n)
	}
}

func TestFetchPendingReviews_BotLookupFailureDegrades(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(failingLookupHandler(t))
	defer srv.Close()

	adapter := mustSCMAdapter(t, srv.URL)
	log, buf := newCapturingLogger()
	adapter.log = log

	got, err := adapter.FetchPendingReviews(context.Background(), testPRNumber, scmOwner, scmRepo)
	if err != nil {
		t.Fatalf("FetchPendingReviews: unexpected error: %v (a lookup failure other than 404 must degrade, not fail, the read)", err)
	}
	if len(got) != 1 || got[0].ID != "9101" || got[0].Reviewer != "bob" {
		t.Errorf("FetchPendingReviews() = %+v, want the one note from the unresolved reviewer bob", got)
	}

	logOutput := buf.String()
	if !strings.Contains(logOutput, "reviewer bot flag unresolved, treating reviewer as not a bot") {
		t.Errorf("log output = %q, want the degradation WARN message", logOutput)
	}
	if !strings.Contains(logOutput, "user_id=503") {
		t.Errorf("log output = %q, want it to carry the unresolved user id", logOutput)
	}
}

func TestFetchPendingReviews_ErrorStatuses(t *testing.T) {
	t.Parallel()

	runSCMErrorStatusTable(t, func(t *testing.T, srv *httptest.Server) error {
		adapter := mustSCMAdapter(t, srv.URL)
		_, err := adapter.FetchPendingReviews(context.Background(), testPRNumber, scmOwner, scmRepo)
		return err
	})

	t.Run("malformed reviewers body is a payload error", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`not valid json {{{`))
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		_, err := adapter.FetchPendingReviews(context.Background(), testPRNumber, scmOwner, scmRepo)
		adaptertest.AssertSCMErrorKind(t, err, domain.ErrSCMPayload)
	})

	t.Run("a notes-route failure after a surviving reviewer surfaces via the shared converter", func(t *testing.T) {
		t.Parallel()

		reviewersBody := loadFixture(t, "reviewers_single_human_changes_requested.json")
		userDetail := loadFixture(t, "user_detail_bot_false.json")
		errorBody := loadFixture(t, "error_500.json")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/users/503"):
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(userDetail)
			case strings.HasSuffix(r.URL.Path, "/notes"):
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write(errorBody)
			case strings.HasSuffix(r.URL.Path, "/reviewers"):
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(reviewersBody)
			default:
				t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		_, err := adapter.FetchPendingReviews(context.Background(), testPRNumber, scmOwner, scmRepo)
		adaptertest.AssertSCMErrorKind(t, err, domain.ErrSCMTransport)
	})
}

// --- FetchBotReviewComments ---

func TestFetchBotReviewComments_AllowlistArmSkipsLookup(t *testing.T) {
	t.Parallel()

	notesBody := loadFixture(t, "notes_allowlisted_author.json")
	var userRequests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/users/"):
			userRequests.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		case strings.HasSuffix(r.URL.Path, "/notes"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(notesBody)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	adapter := mustSCMAdapter(t, srv.URL)
	got, err := adapter.FetchBotReviewComments(context.Background(), testPRNumber, scmOwner, scmRepo, []string{"release-bot"})
	if err != nil {
		t.Fatalf("FetchBotReviewComments: unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].ID != "9201" {
		t.Errorf("FetchBotReviewComments() = %+v, want the allowlisted author's one note", got)
	}
	if n := userRequests.Load(); n != 0 {
		t.Errorf("user lookup requests = %d, want 0 (an allowlisted author must cost no lookup)", n)
	}
}

func TestFetchBotReviewComments_LookupFailureAborts(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(failingLookupHandler(t))
	defer srv.Close()

	adapter := mustSCMAdapter(t, srv.URL)
	got, err := adapter.FetchBotReviewComments(context.Background(), testPRNumber, scmOwner, scmRepo, nil)
	adaptertest.AssertSCMErrorKind(t, err, domain.ErrSCMTransport)
	if got != nil {
		t.Errorf("FetchBotReviewComments() = %+v, want nil on a lookup failure", got)
	}
}

func TestFetchBotReviewComments_ErrorStatuses(t *testing.T) {
	t.Parallel()

	runSCMErrorStatusTable(t, func(t *testing.T, srv *httptest.Server) error {
		adapter := mustSCMAdapter(t, srv.URL)
		_, err := adapter.FetchBotReviewComments(context.Background(), testPRNumber, scmOwner, scmRepo, nil)
		return err
	})

	t.Run("malformed notes body is a payload error", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`not valid json {{{`))
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		_, err := adapter.FetchBotReviewComments(context.Background(), testPRNumber, scmOwner, scmRepo, nil)
		adaptertest.AssertSCMErrorKind(t, err, domain.ErrSCMPayload)
	})
}

// --- resolveBotFlag cache ---

func TestResolveBotFlag_CacheSharedAcrossCallSites(t *testing.T) {
	t.Parallel()

	t.Run("userID <= 0 resolves to false with no request", func(t *testing.T) {
		t.Parallel()

		var requests atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		for _, id := range []int64{0, -5} {
			bot, err := adapter.resolveBotFlag(context.Background(), id)
			if err != nil || bot {
				t.Errorf("resolveBotFlag(%d) = (%v, %v), want (false, nil)", id, bot, err)
			}
		}
		if n := requests.Load(); n != 0 {
			t.Errorf("requests = %d, want 0", n)
		}
	})

	t.Run("a resolved flag is cached and answers a second call with no request", func(t *testing.T) {
		t.Parallel()

		body := loadFixture(t, "user_detail_bot_false.json")
		var requests atomic.Int32
		srv := httptest.NewServer(userDetailHandler(t, body, &requests))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		for range 2 {
			bot, err := adapter.resolveBotFlag(context.Background(), 510)
			if err != nil || bot {
				t.Errorf("resolveBotFlag(510) = (%v, %v), want (false, nil)", bot, err)
			}
		}
		if n := requests.Load(); n != 1 {
			t.Errorf("requests = %d, want 1 (the second call must hit the cache)", n)
		}
	})

	t.Run("a 404 lookup is cached as not-a-bot", func(t *testing.T) {
		t.Parallel()

		errorBody := loadFixture(t, "error_404_issue.json")
		var requests atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write(errorBody)
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		for range 2 {
			bot, err := adapter.resolveBotFlag(context.Background(), 511)
			if err != nil || bot {
				t.Errorf("resolveBotFlag(511) = (%v, %v), want (false, nil)", bot, err)
			}
		}
		if n := requests.Load(); n != 1 {
			t.Errorf("requests = %d, want 1 (a 404 must be cached)", n)
		}
	})

	t.Run("a non-404 lookup failure is not cached", func(t *testing.T) {
		t.Parallel()

		errorBody := loadFixture(t, "error_500.json")
		var requests atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write(errorBody)
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		for range 2 {
			_, err := adapter.resolveBotFlag(context.Background(), 512)
			if err == nil {
				t.Error("resolveBotFlag(512): want an error")
			}
		}
		if n := requests.Load(); n != 2 {
			t.Errorf("requests = %d, want 2 (a non-404 failure must not be cached)", n)
		}
	})

	t.Run("a classification from one public method costs no lookup in the other", func(t *testing.T) {
		t.Parallel()

		reviewersBody := loadFixture(t, "reviewers_single_human_changes_requested.json")
		notesBody := loadFixture(t, "notes_single_author.json")
		userDetail := loadFixture(t, "user_detail_bot_false.json")
		var userRequests atomic.Int32
		srv := httptest.NewServer(reviewsRoutingHandler(t, reviewersBody, notesBody, userDetailHandler(t, userDetail, &userRequests)))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		if _, err := adapter.FetchPendingReviews(context.Background(), testPRNumber, scmOwner, scmRepo); err != nil {
			t.Fatalf("FetchPendingReviews: unexpected error: %v", err)
		}
		if _, err := adapter.FetchBotReviewComments(context.Background(), testPRNumber, scmOwner, scmRepo, nil); err != nil {
			t.Fatalf("FetchBotReviewComments: unexpected error: %v", err)
		}

		if n := userRequests.Load(); n != 1 {
			t.Errorf("user lookup requests across both methods = %d, want 1 (the cache is shared)", n)
		}
	})
}

// --- normalizeNotes ---

// normalizeNotesHeadSHA matches the sha field of testdata/mr_basic.json,
// the fixture these subtests serve for the conditional merge-request read.
const normalizeNotesHeadSHA = "d1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d9e0"

func TestNormalizeNotes_OutdatedDerivationAndConditionalRead(t *testing.T) {
	t.Parallel()

	t.Run("no note carries a position: no merge-request read is issued", func(t *testing.T) {
		t.Parallel()

		var requests atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(loadFixture(t, "mr_basic.json"))
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		notes := []gitlabNote{
			{ID: 1, Author: gitlabUser{Username: "alice"}, Body: "top-level note", CreatedAt: "2026-08-10T08:00:00Z"},
		}
		got, err := adapter.normalizeNotes(context.Background(), testPRNumber, scmOwner, scmRepo, notes)
		if err != nil {
			t.Fatalf("normalizeNotes: unexpected error: %v", err)
		}
		if len(got) != 1 || got[0].Outdated || got[0].FilePath != "" || got[0].StartLine != 0 {
			t.Errorf("normalizeNotes() = %+v, want one top-level comment with no position fields", got)
		}
		if n := requests.Load(); n != 0 {
			t.Errorf("requests = %d, want 0 (no note carries a position)", n)
		}
	})

	t.Run("position head_sha equal to the current sha is not outdated", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(loadFixture(t, "mr_basic.json"))
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		newLine := 5
		notes := []gitlabNote{
			{
				ID: 2, Author: gitlabUser{Username: "alice"}, Body: "inline note", CreatedAt: "2026-08-10T08:00:00Z",
				Position: &gitlabNotePosition{HeadSHA: normalizeNotesHeadSHA, NewPath: "a.go", NewLine: &newLine},
			},
		}
		got, err := adapter.normalizeNotes(context.Background(), testPRNumber, scmOwner, scmRepo, notes)
		if err != nil {
			t.Fatalf("normalizeNotes: unexpected error: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("normalizeNotes() len = %d, want 1", len(got))
		}
		if got[0].Outdated {
			t.Error("normalizeNotes()[0].Outdated = true, want false (head_sha matches the current sha)")
		}
		if got[0].FilePath != "a.go" || got[0].StartLine != 5 {
			t.Errorf("normalizeNotes()[0] = %+v, want FilePath=a.go StartLine=5", got[0])
		}
	})

	t.Run("position head_sha differing from the current sha is outdated, falling back to old_path and old_line", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(loadFixture(t, "mr_basic.json"))
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		oldLine := 9
		notes := []gitlabNote{
			{
				ID: 3, Author: gitlabUser{Username: "alice"}, Body: "stale inline note", CreatedAt: "2026-08-10T08:00:00Z",
				Position: &gitlabNotePosition{HeadSHA: "stale-sha-0000000000000000000000000", OldPath: "b.go", OldLine: &oldLine},
			},
		}
		got, err := adapter.normalizeNotes(context.Background(), testPRNumber, scmOwner, scmRepo, notes)
		if err != nil {
			t.Fatalf("normalizeNotes: unexpected error: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("normalizeNotes() len = %d, want 1", len(got))
		}
		if !got[0].Outdated {
			t.Error("normalizeNotes()[0].Outdated = false, want true (head_sha differs from the current sha)")
		}
		if got[0].FilePath != "b.go" || got[0].StartLine != 9 {
			t.Errorf("normalizeNotes()[0] = %+v, want the old_path/old_line fallback FilePath=b.go StartLine=9", got[0])
		}
	})

	t.Run("a mixed slice issues exactly one merge-request read regardless of how many notes carry a position", func(t *testing.T) {
		t.Parallel()

		var requests atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(loadFixture(t, "mr_basic.json"))
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		lineA, lineB := 1, 2
		notes := []gitlabNote{
			{ID: 4, Author: gitlabUser{Username: "alice"}, Body: "top-level", CreatedAt: "2026-08-10T08:00:00Z"},
			{ID: 5, Author: gitlabUser{Username: "alice"}, Body: "inline a", CreatedAt: "2026-08-10T08:01:00Z",
				Position: &gitlabNotePosition{HeadSHA: normalizeNotesHeadSHA, NewPath: "a.go", NewLine: &lineA}},
			{ID: 6, Author: gitlabUser{Username: "alice"}, Body: "inline b", CreatedAt: "2026-08-10T08:02:00Z",
				Position: &gitlabNotePosition{HeadSHA: normalizeNotesHeadSHA, NewPath: "b.go", NewLine: &lineB}},
		}
		got, err := adapter.normalizeNotes(context.Background(), testPRNumber, scmOwner, scmRepo, notes)
		if err != nil {
			t.Fatalf("normalizeNotes: unexpected error: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("normalizeNotes() len = %d, want 3", len(got))
		}
		if n := requests.Load(); n != 1 {
			t.Errorf("merge-request read requests = %d, want 1 (issued once regardless of how many notes carry a position)", n)
		}
	})
}

// --- GetReviewDecision ---

func TestGetReviewDecision_ArmOrder(t *testing.T) {
	t.Parallel()

	t.Run("requested_changes short-circuits before the approvals read", func(t *testing.T) {
		t.Parallel()

		reviewersBody := loadFixture(t, "reviewers_single_human_changes_requested.json")
		var approvalsRequests atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case strings.HasSuffix(r.URL.Path, "/approvals"):
				approvalsRequests.Add(1)
				_, _ = w.Write(loadFixture(t, "approvals_approved_true.json"))
			case strings.HasSuffix(r.URL.Path, "/reviewers"):
				_, _ = w.Write(reviewersBody)
			default:
				t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		got, err := adapter.GetReviewDecision(context.Background(), testPRNumber, scmOwner, scmRepo)
		if err != nil {
			t.Fatalf("GetReviewDecision: unexpected error: %v", err)
		}
		if got != domain.ReviewDecisionChangesRequested {
			t.Errorf("GetReviewDecision() = %q, want %q", got, domain.ReviewDecisionChangesRequested)
		}
		if n := approvalsRequests.Load(); n != 0 {
			t.Errorf("approvals requests = %d, want 0 (the changes_requested arm must return before the approvals read)", n)
		}
	})

	tests := []struct {
		name             string
		reviewersFixture string
		approvalsFixture string
		want             domain.ReviewDecision
	}{
		{"approved", "reviewers_no_changes_requested.json", "approvals_approved_true.json", domain.ReviewDecisionApproved},
		{"a reviewer with no approval is review-required", "reviewers_no_changes_requested.json", "approvals_approved_false.json", domain.ReviewDecisionReviewRequired},
		{"no reviewers and no approval is not-required", "reviewers_empty.json", "approvals_approved_false.json", domain.ReviewDecisionNotRequired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reviewersBody := loadFixture(t, tt.reviewersFixture)
			approvalsBody := loadFixture(t, tt.approvalsFixture)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/approvals"):
					_, _ = w.Write(approvalsBody)
				case strings.HasSuffix(r.URL.Path, "/reviewers"):
					_, _ = w.Write(reviewersBody)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()

			adapter := mustSCMAdapter(t, srv.URL)
			got, err := adapter.GetReviewDecision(context.Background(), testPRNumber, scmOwner, scmRepo)
			if err != nil {
				t.Fatalf("GetReviewDecision: unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("GetReviewDecision() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGetReviewDecision_ErrorStatuses(t *testing.T) {
	t.Parallel()

	runSCMErrorStatusTable(t, func(t *testing.T, srv *httptest.Server) error {
		adapter := mustSCMAdapter(t, srv.URL)
		_, err := adapter.GetReviewDecision(context.Background(), testPRNumber, scmOwner, scmRepo)
		return err
	})

	t.Run("malformed reviewers body is a payload error", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`not valid json {{{`))
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		_, err := adapter.GetReviewDecision(context.Background(), testPRNumber, scmOwner, scmRepo)
		adaptertest.AssertSCMErrorKind(t, err, domain.ErrSCMPayload)
	})

	t.Run("an approvals-route failure surfaces via the shared converter", func(t *testing.T) {
		t.Parallel()

		reviewersBody := loadFixture(t, "reviewers_no_changes_requested.json")
		errorBody := loadFixture(t, "error_500.json")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case strings.HasSuffix(r.URL.Path, "/approvals"):
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write(errorBody)
			case strings.HasSuffix(r.URL.Path, "/reviewers"):
				_, _ = w.Write(reviewersBody)
			default:
				t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		got, err := adapter.GetReviewDecision(context.Background(), testPRNumber, scmOwner, scmRepo)
		adaptertest.AssertSCMErrorKind(t, err, domain.ErrSCMTransport)
		if got != "" {
			t.Errorf("GetReviewDecision() = %q, want empty on failure", got)
		}
	})

	t.Run("a malformed approvals body is a payload error", func(t *testing.T) {
		t.Parallel()

		reviewersBody := loadFixture(t, "reviewers_no_changes_requested.json")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/approvals"):
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`not valid json {{{`))
			case strings.HasSuffix(r.URL.Path, "/reviewers"):
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(reviewersBody)
			default:
				t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		_, err := adapter.GetReviewDecision(context.Background(), testPRNumber, scmOwner, scmRepo)
		adaptertest.AssertSCMErrorKind(t, err, domain.ErrSCMPayload)
	})
}
