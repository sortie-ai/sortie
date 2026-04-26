package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
)

// --- helpers ---

func validConfig(endpoint string) map[string]any {
	return map[string]any{
		"endpoint": endpoint,
		"api_key":  "test-token",
		"project":  "owner/repo",
	}
}

func mustAdapter(t *testing.T, config map[string]any) *GitHubAdapter {
	t.Helper()
	a, err := NewGitHubAdapter(config)
	if err != nil {
		t.Fatalf("NewGitHubAdapter: %v", err)
	}
	return a.(*GitHubAdapter)
}

func assertTrackerErrorKind(t *testing.T, err error, want domain.TrackerErrorKind) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with kind %q, got nil", want)
	}
	var te *domain.TrackerError
	if !errors.As(err, &te) {
		t.Fatalf("error type = %T, want *domain.TrackerError", err)
	}
	if te.Kind != want {
		t.Errorf("TrackerError.Kind = %q, want %q", te.Kind, want)
	}
}

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return data
}

// issueJSON returns a minimal valid GitHub issue JSON with the given number
// and state label. Used in server handlers that need simple inline responses.
func issueJSON(number int, stateLabel string, nativeState string) string {
	return fmt.Sprintf(`{"id":%d,"number":%d,"title":"Issue %d","body":null,"state":%q,"html_url":"https://github.com/owner/repo/issues/%d","labels":[{"name":%q}],"assignees":[],"type":null,"pull_request":null,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`,
		number*100, number, number, nativeState, number, stateLabel)
}

// spyMetrics records IncTrackerRequests calls for assertions.
type spyMetrics struct {
	domain.NoopMetrics
	calls []string
}

func (s *spyMetrics) IncTrackerRequests(operation, result string) {
	s.calls = append(s.calls, operation+":"+result)
}

// --- Constructor tests ---

func TestNewGitHubAdapter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		config   map[string]any
		wantErr  bool
		wantKind domain.TrackerErrorKind
	}{
		{
			name:   "valid config",
			config: validConfig("https://api.github.com"),
		},
		{
			name:     "missing api_key",
			config:   map[string]any{"project": "owner/repo"},
			wantErr:  true,
			wantKind: domain.ErrMissingTrackerAPIKey,
		},
		{
			name:     "empty api_key",
			config:   map[string]any{"api_key": "", "project": "owner/repo"},
			wantErr:  true,
			wantKind: domain.ErrMissingTrackerAPIKey,
		},
		{
			name:     "missing project",
			config:   map[string]any{"api_key": "tok"},
			wantErr:  true,
			wantKind: domain.ErrMissingTrackerProject,
		},
		{
			name:     "empty project",
			config:   map[string]any{"api_key": "tok", "project": ""},
			wantErr:  true,
			wantKind: domain.ErrMissingTrackerProject,
		},
		{
			name:     "project with no slash",
			config:   map[string]any{"api_key": "tok", "project": "ownerrepo"},
			wantErr:  true,
			wantKind: domain.ErrTrackerPayload,
		},
		{
			name:     "project with two slashes",
			config:   map[string]any{"api_key": "tok", "project": "owner/repo/extra"},
			wantErr:  true,
			wantKind: domain.ErrTrackerPayload,
		},
		{
			name:     "project with empty owner",
			config:   map[string]any{"api_key": "tok", "project": "/repo"},
			wantErr:  true,
			wantKind: domain.ErrTrackerPayload,
		},
		{
			name:     "project with empty repo",
			config:   map[string]any{"api_key": "tok", "project": "owner/"},
			wantErr:  true,
			wantKind: domain.ErrTrackerPayload,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			a, err := NewGitHubAdapter(tt.config)
			if tt.wantErr {
				assertTrackerErrorKind(t, err, tt.wantKind)
				if a != nil {
					t.Error("adapter should be nil on error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if a == nil {
				t.Fatal("adapter is nil")
			}
		})
	}
}

func TestNewGitHubAdapter_Defaults(t *testing.T) {
	t.Parallel()

	a := mustAdapter(t, validConfig("https://api.github.com"))

	// Default active states: lowercased.
	if len(a.activeStates) != 3 {
		t.Fatalf("activeStates len = %d, want 3", len(a.activeStates))
	}
	if a.activeStates[0] != "backlog" {
		t.Errorf("activeStates[0] = %q, want %q", a.activeStates[0], "backlog")
	}

	// Default terminal states.
	if len(a.terminalStates) != 2 {
		t.Fatalf("terminalStates len = %d, want 2", len(a.terminalStates))
	}
	if a.terminalStates[0] != "done" {
		t.Errorf("terminalStates[0] = %q, want %q", a.terminalStates[0], "done")
	}

	// Default query filter is empty.
	if a.queryFilter != "" {
		t.Errorf("queryFilter = %q, want empty", a.queryFilter)
	}

	// Owner and repo split correctly.
	if a.owner != "owner" {
		t.Errorf("owner = %q, want %q", a.owner, "owner")
	}
	if a.repo != "repo" {
		t.Errorf("repo = %q, want %q", a.repo, "repo")
	}
}

func TestNewGitHubAdapter_CustomStatesLowercased(t *testing.T) {
	t.Parallel()

	cfg := validConfig("https://api.github.com")
	cfg["active_states"] = []any{"Todo", "IN-PROGRESS"}
	cfg["terminal_states"] = []string{"Done", "WONTFIX"}
	a := mustAdapter(t, cfg)

	if a.activeStates[0] != "todo" {
		t.Errorf("activeStates[0] = %q, want %q", a.activeStates[0], "todo")
	}
	if a.activeStates[1] != "in-progress" {
		t.Errorf("activeStates[1] = %q, want %q", a.activeStates[1], "in-progress")
	}
	if a.terminalStates[0] != "done" {
		t.Errorf("terminalStates[0] = %q, want %q", a.terminalStates[0], "done")
	}
	if a.terminalStates[1] != "wontfix" {
		t.Errorf("terminalStates[1] = %q, want %q", a.terminalStates[1], "wontfix")
	}
}

func TestNewGitHubAdapter_EndpointTrailingSlashStripped(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "//") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"unexpected doubled slash"}`)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/owner/repo/issues/123":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, issueJSON(123, "todo", "open"))
		case "/repos/owner/repo/issues/123/comments":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `[]`)
		case "/repos/owner/repo/issues/123/dependencies/blocked_by":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `[]`)
		case "/repos/owner/repo/issues/123/parent":
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"not found"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"unexpected path"}`)
		}
	}))
	defer srv.Close()

	cfg := validConfig(srv.URL + "/")
	a := mustAdapter(t, cfg)

	issue, err := a.FetchIssueByID(context.Background(), "123")
	if err != nil {
		t.Fatalf("FetchIssueByID: %v", err)
	}
	if issue.ID != "123" {
		t.Errorf("issue.ID = %q, want %q", issue.ID, "123")
	}
}

func TestNewGitHubAdapter_DoesNotMutateConfigSlices(t *testing.T) {
	t.Parallel()

	// NewGitHubAdapter lowercases state values internally; it must not write
	// those lowercased values back into the caller-supplied slices.
	cfg := validConfig("https://api.github.com")
	cfg["active_states"] = []string{"InProgress", "Review"}
	cfg["terminal_states"] = []string{"Done", "WontFix"}

	mustAdapter(t, cfg)

	active := cfg["active_states"].([]string)
	if active[0] != "InProgress" {
		t.Errorf("active_states[0] = %q after construction, want original %q", active[0], "InProgress")
	}
	if active[1] != "Review" {
		t.Errorf("active_states[1] = %q after construction, want original %q", active[1], "Review")
	}

	terminal := cfg["terminal_states"].([]string)
	if terminal[0] != "Done" {
		t.Errorf("terminal_states[0] = %q after construction, want original %q", terminal[0], "Done")
	}
	if terminal[1] != "WontFix" {
		t.Errorf("terminal_states[1] = %q after construction, want original %q", terminal[1], "WontFix")
	}
}

func TestNewGitHubAdapter_HandoffStateExtraction(t *testing.T) {
	t.Parallel()

	// Positive cases: the handoff state must be accepted as a valid transition
	// target, proving the field was extracted and stored correctly.
	t.Run("trimmed and lowercased", func(t *testing.T) {
		t.Parallel()

		// Issue currently has the handoff label, native state open.
		ts := newTransitionServer(t, 1, "review", "open")
		cfg := validConfig(ts.srv.URL)
		cfg["active_states"] = []any{"backlog", "in-progress"}
		cfg["terminal_states"] = []any{"done"}
		cfg["handoff_state"] = "  Review  " // whitespace + mixed-case
		a := mustAdapter(t, cfg)

		// TransitionIssue to the handoff state must not return ErrTrackerPayload.
		if err := a.TransitionIssue(context.Background(), "1", "review"); err != nil {
			t.Fatalf("TransitionIssue(review) with handoff_state=\"  Review  \": %v", err)
		}
	})

	t.Run("already lowercase", func(t *testing.T) {
		t.Parallel()

		ts := newTransitionServer(t, 1, "review", "open")
		cfg := validConfig(ts.srv.URL)
		cfg["active_states"] = []any{"backlog", "in-progress"}
		cfg["terminal_states"] = []any{"done"}
		cfg["handoff_state"] = "review"
		a := mustAdapter(t, cfg)

		if err := a.TransitionIssue(context.Background(), "1", "review"); err != nil {
			t.Fatalf("TransitionIssue(review) with handoff_state=\"review\": %v", err)
		}
	})

	// Negative cases: when handoffState is empty or absent, calling
	// TransitionIssue with a value not in active or terminal must still
	// return ErrTrackerPayload (no unintended bypass).
	t.Run("empty string preserves rejection", func(t *testing.T) {
		t.Parallel()

		cfg := validConfig("http://localhost")
		cfg["active_states"] = []any{"backlog"}
		cfg["terminal_states"] = []any{"done"}
		cfg["handoff_state"] = ""
		a := mustAdapter(t, cfg)

		err := a.TransitionIssue(context.Background(), "1", "review")
		assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)
	})

	t.Run("key absent preserves rejection", func(t *testing.T) {
		t.Parallel()

		cfg := validConfig("http://localhost")
		cfg["active_states"] = []any{"backlog"}
		cfg["terminal_states"] = []any{"done"}
		// handoff_state key omitted entirely.
		a := mustAdapter(t, cfg)

		err := a.TransitionIssue(context.Background(), "1", "review")
		assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)
	})
}

// --- FetchCandidateIssues ---

func TestFetchCandidateIssues_FiltersPullRequests(t *testing.T) {
	t.Parallel()

	// issues.json contains: backlog issue (#1), PR (#2, filtered), in-progress (#3), done (#4, non-active).
	fixture := loadFixture(t, "issues.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(fixture) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	issues, err := a.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}

	// Issues 1 (backlog) and 3 (in-progress) pass. PR #2 filtered. #4 (done=terminal) filtered.
	if len(issues) != 2 {
		t.Fatalf("len = %d, want 2 (PR and non-active filtered)", len(issues))
	}
	ids := make(map[string]bool)
	for _, iss := range issues {
		ids[iss.Identifier] = true
	}
	if ids["2"] {
		t.Error("PR #2 should be filtered out")
	}
	if ids["4"] {
		t.Error("issue #4 (done state) should be filtered out (non-active)")
	}
}

func TestFetchCandidateIssues_CommentsNil(t *testing.T) {
	t.Parallel()

	fixture := loadFixture(t, "issues.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(fixture) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	issues, err := a.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}

	for _, iss := range issues {
		if iss.Comments != nil {
			t.Errorf("issue %s: Comments should be nil in candidate list", iss.Identifier)
		}
		if iss.BlockedBy == nil {
			t.Errorf("issue %s: BlockedBy should be non-nil empty slice", iss.Identifier)
		}
		if iss.Labels == nil {
			t.Errorf("issue %s: Labels should be non-nil", iss.Identifier)
		}
	}
}

func TestFetchCandidateIssues_NonNilEmptySlice(t *testing.T) {
	t.Parallel()

	// Empty response → non-nil empty slice, not nil.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("[]")) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	issues, err := a.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	if issues == nil {
		t.Fatal("issues is nil, want non-nil empty slice")
	}
	if len(issues) != 0 {
		t.Errorf("len = %d, want 0", len(issues))
	}
}

func TestFetchCandidateIssues_Pagination(t *testing.T) {
	t.Parallel()

	page1 := loadFixture(t, "issues.json")       // issues 1, 3 active
	page2 := loadFixture(t, "issues_page2.json") // issue 5 (review)

	var srvURL string
	var callCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callCount, 1)
		if n == 1 {
			// First page: set Link header pointing to page 2.
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/owner/repo/issues?page=2>; rel="next"`, srvURL))
			w.WriteHeader(http.StatusOK)
			w.Write(page1) //nolint:errcheck // test helper
		} else {
			w.WriteHeader(http.StatusOK)
			w.Write(page2) //nolint:errcheck // test helper
		}
	}))
	defer srv.Close()
	srvURL = srv.URL

	a := mustAdapter(t, validConfig(srv.URL))
	issues, err := a.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}

	// page1 has 2 active issues; page2 has 1 active issue.
	if len(issues) != 3 {
		t.Fatalf("len = %d, want 3 across 2 pages", len(issues))
	}
	if got := atomic.LoadInt32(&callCount); got != 2 {
		t.Errorf("call count = %d, want 2", got)
	}
}

func TestFetchCandidateIssues_MaxPagesGuard(t *testing.T) {
	t.Parallel()

	// Server always returns a Link header, forcing the guard to trigger.
	var callCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callCount, 1)
		nextURL := fmt.Sprintf("http://%s/repos/owner/repo/issues?p=%d", r.Host, n+1)
		w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, nextURL))
		w.WriteHeader(http.StatusOK)
		// Return one active issue per page.
		w.Write([]byte(`[{"id":` + fmt.Sprint(n) + `,"number":` + fmt.Sprint(n) + `,"title":"T","body":null,"state":"open","html_url":"u","labels":[{"name":"backlog"}],"assignees":[],"type":null,"pull_request":null,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]`)) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	a := mustAdapter(t, validConfig(srv.URL))
	issues, err := a.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}

	got := int(atomic.LoadInt32(&callCount))
	if got > maxPages {
		t.Errorf("call count = %d, exceeded maxPages = %d", got, maxPages)
	}
	if len(issues) == 0 {
		t.Error("issues should not be empty")
	}
}

func TestFetchCandidateIssues_SearchEndpoint(t *testing.T) {
	t.Parallel()

	fixture := loadFixture(t, "search.json")
	var gotPath string
	var gotQ string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQ = r.URL.Query().Get("q")
		w.WriteHeader(http.StatusOK)
		w.Write(fixture) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	cfg := validConfig(srv.URL)
	cfg["query_filter"] = "label:critical"
	a := mustAdapter(t, cfg)
	issues, err := a.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}

	if gotPath != "/search/issues" {
		t.Errorf("path = %q, want /search/issues", gotPath)
	}
	if !strings.Contains(gotQ, "repo:owner/repo") {
		t.Errorf("q = %q, should contain repo qualifier", gotQ)
	}
	if !strings.Contains(gotQ, "type:issue") {
		t.Errorf("q = %q, should contain type:issue", gotQ)
	}
	if !strings.Contains(gotQ, "label:critical") {
		t.Errorf("q = %q, should contain queryFilter", gotQ)
	}

	// search.json has one "review" state issue.
	if len(issues) != 1 {
		t.Fatalf("len = %d, want 1", len(issues))
	}
	if issues[0].State != "review" {
		t.Errorf("issues[0].State = %q, want %q", issues[0].State, "review")
	}
}

func TestFetchCandidateIssues_AuthError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	_, err := a.FetchCandidateIssues(context.Background())
	assertTrackerErrorKind(t, err, domain.ErrTrackerAuth)
}

// --- FetchIssueByID ---

func TestFetchIssueByID_FullPopulation(t *testing.T) {
	t.Parallel()

	issueFix := loadFixture(t, "issue.json")
	blockersFix := loadFixture(t, "blockers.json")
	parentFix := loadFixture(t, "parent.json")
	commentsFix := loadFixture(t, "comments.json")

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/issues/42/dependencies/blocked_by", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(blockersFix) //nolint:errcheck // test helper
	})
	mux.HandleFunc("/repos/owner/repo/issues/42/parent", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(parentFix) //nolint:errcheck // test helper
	})
	mux.HandleFunc("/repos/owner/repo/issues/42/comments", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(commentsFix) //nolint:errcheck // test helper
	})
	mux.HandleFunc("/repos/owner/repo/issues/42", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(issueFix) //nolint:errcheck // test helper
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	issue, err := a.FetchIssueByID(context.Background(), "42")
	if err != nil {
		t.Fatalf("FetchIssueByID: %v", err)
	}

	// issue.json: number=42, title="Add dark mode support", in-progress label.
	if issue.ID != "42" || issue.Identifier != "42" {
		t.Errorf("ID/Identifier = %q/%q, want 42/42", issue.ID, issue.Identifier)
	}
	if issue.Title != "Add dark mode support" {
		t.Errorf("Title = %q", issue.Title)
	}
	if issue.State != "in-progress" {
		t.Errorf("State = %q, want in-progress", issue.State)
	}
	if issue.Description != "Users want a **dark mode** option." {
		t.Errorf("Description = %q", issue.Description)
	}
	if issue.IssueType != "Feature" {
		t.Errorf("IssueType = %q, want Feature", issue.IssueType)
	}
	if issue.Assignee != "alice" {
		t.Errorf("Assignee = %q, want alice", issue.Assignee)
	}

	// Blockers from blockers.json: one blocker (#5).
	if len(issue.BlockedBy) != 1 {
		t.Fatalf("BlockedBy len = %d, want 1", len(issue.BlockedBy))
	}
	if issue.BlockedBy[0].Identifier != "5" {
		t.Errorf("BlockedBy[0].Identifier = %q, want 5", issue.BlockedBy[0].Identifier)
	}

	// Parent from parent.json: #7.
	if issue.Parent == nil {
		t.Fatal("Parent is nil, want non-nil")
	}
	if issue.Parent.Identifier != "7" {
		t.Errorf("Parent.Identifier = %q, want 7", issue.Parent.Identifier)
	}

	// Comments from comments.json: two comments.
	if len(issue.Comments) != 2 {
		t.Fatalf("Comments len = %d, want 2", len(issue.Comments))
	}
	if issue.Comments[0].Author != "alice" {
		t.Errorf("Comments[0].Author = %q", issue.Comments[0].Author)
	}
	if issue.Comments[0].Body != "Looks good, please proceed." {
		t.Errorf("Comments[0].Body = %q", issue.Comments[0].Body)
	}
}

func TestFetchIssueByID_NotFound(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	_, err := a.FetchIssueByID(context.Background(), "99")
	assertTrackerErrorKind(t, err, domain.ErrTrackerNotFound)

	var te *domain.TrackerError
	if errors.As(err, &te) {
		if !strings.Contains(te.Message, "99") {
			t.Errorf("TrackerError.Message = %q, should contain issue ID 99", te.Message)
		}
	}
}

func TestFetchIssueByID_PRReturnsNotFound(t *testing.T) {
	t.Parallel()

	// Response is a GitHub "issue" that has a pull_request field set.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":1,"number":1,"title":"PR","body":null,"state":"open","html_url":"u","labels":[],"assignees":[],"pull_request":{},"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`)) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	_, err := a.FetchIssueByID(context.Background(), "1")
	assertTrackerErrorKind(t, err, domain.ErrTrackerNotFound)
}

func TestFetchIssueByID_BlockerNotFound_Degrades(t *testing.T) {
	t.Parallel()

	// blockers endpoint returns 404 → BlockedBy should be empty non-nil slice.
	issueFix := loadFixture(t, "issue.json")
	commentsFix := loadFixture(t, "comments.json")

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/issues/42/dependencies/blocked_by", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/repos/owner/repo/issues/42/parent", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/repos/owner/repo/issues/42/comments", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(commentsFix) //nolint:errcheck // test helper
	})
	mux.HandleFunc("/repos/owner/repo/issues/42", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(issueFix) //nolint:errcheck // test helper
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	issue, err := a.FetchIssueByID(context.Background(), "42")
	if err != nil {
		t.Fatalf("FetchIssueByID: %v", err)
	}

	// 404 on blockers → empty non-nil slice.
	if issue.BlockedBy == nil {
		t.Error("BlockedBy is nil, want non-nil empty slice on 404")
	}
	if len(issue.BlockedBy) != 0 {
		t.Errorf("BlockedBy len = %d, want 0", len(issue.BlockedBy))
	}

	// 404 on parent → nil.
	if issue.Parent != nil {
		t.Errorf("Parent = %v, want nil on 404", issue.Parent)
	}
}

func TestFetchIssueByID_CommentsNotFound(t *testing.T) {
	t.Parallel()

	issueFix := loadFixture(t, "issue.json")

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/issues/42/dependencies/blocked_by", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("[]")) //nolint:errcheck // test helper
	})
	mux.HandleFunc("/repos/owner/repo/issues/42/parent", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/repos/owner/repo/issues/42/comments", func(w http.ResponseWriter, r *http.Request) {
		// 404 on comments is propagated as ErrTrackerNotFound.
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/repos/owner/repo/issues/42", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(issueFix) //nolint:errcheck // test helper
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	_, err := a.FetchIssueByID(context.Background(), "42")
	assertTrackerErrorKind(t, err, domain.ErrTrackerNotFound)
}

// --- FetchIssuesByStates ---

func TestFetchIssuesByStates_EmptyInput(t *testing.T) {
	t.Parallel()

	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	issues, err := a.FetchIssuesByStates(context.Background(), []string{})
	if err != nil {
		t.Fatalf("FetchIssuesByStates: %v", err)
	}
	if issues == nil {
		t.Fatal("issues is nil, want non-nil empty slice")
	}
	if len(issues) != 0 {
		t.Errorf("len = %d, want 0", len(issues))
	}
	if called {
		t.Error("server called for empty states — should short-circuit")
	}
}

func TestFetchIssuesByStates_ActiveStatesUsesIssuesEndpoint(t *testing.T) {
	t.Parallel()

	fixture := loadFixture(t, "issues.json")
	var gotPath string
	var gotState string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotState = r.URL.Query().Get("state")
		w.WriteHeader(http.StatusOK)
		w.Write(fixture) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	// "backlog" is an active state — should use issues endpoint.
	issues, err := a.FetchIssuesByStates(context.Background(), []string{"backlog"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates: %v", err)
	}

	if gotPath != "/repos/owner/repo/issues" {
		t.Errorf("path = %q, want /repos/owner/repo/issues", gotPath)
	}
	if gotState != "open" {
		t.Errorf("state param = %q, want open", gotState)
	}

	// Only backlog issue (#1) should be in result.
	if len(issues) != 1 {
		t.Fatalf("len = %d, want 1 (backlog only)", len(issues))
	}
	if issues[0].Identifier != "1" {
		t.Errorf("Identifier = %q, want 1", issues[0].Identifier)
	}
}

func TestFetchIssuesByStates_TerminalStatesUsesSearchEndpoint(t *testing.T) {
	t.Parallel()

	// Return an empty search result; we only care about the endpoint used.
	var gotPath string
	var gotQ string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQ = r.URL.Query().Get("q")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"total_count":0,"incomplete_results":false,"items":[]}`)) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	// "done" is a terminal state — must use search endpoint.
	_, err := a.FetchIssuesByStates(context.Background(), []string{"done"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates: %v", err)
	}

	if gotPath != "/search/issues" {
		t.Errorf("path = %q, want /search/issues", gotPath)
	}
	if !strings.Contains(gotQ, "state:closed") {
		t.Errorf("q = %q, should contain state:closed for terminal", gotQ)
	}
	if !strings.Contains(gotQ, `label:"done"`) {
		t.Errorf("q = %q, should contain label:\"done\" (quoted)", gotQ)
	}
}

func TestFetchIssuesByStates_TerminalStateMultiWordLabelQuoted(t *testing.T) {
	t.Parallel()

	// Multi-word terminal state must produce label:"code review" (quoted) in the
	// search query so GitHub parses it as a single label, not two separate terms.
	var gotQ string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQ = r.URL.Query().Get("q")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"total_count":0,"incomplete_results":false,"items":[]}`)) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	cfg := validConfig(srv.URL)
	cfg["terminal_states"] = []any{"code review"}
	a := mustAdapter(t, cfg)

	_, err := a.FetchIssuesByStates(context.Background(), []string{"code review"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates: %v", err)
	}

	// Quoted form prevents "code" and "review" from being treated as separate tokens.
	if !strings.Contains(gotQ, `label:"code review"`) {
		t.Errorf("q = %q, should contain label:\"code review\" (quoted for multi-word label)", gotQ)
	}
	// Unquoted form must not appear.
	if strings.Contains(gotQ, "label:code review") {
		t.Errorf("q = %q, must not contain unquoted label:code review", gotQ)
	}
}

func TestFetchIssuesByStates_Dedup(t *testing.T) {
	t.Parallel()

	// Serve the same issue for both the open endpoint and search endpoint.
	// The adapter should deduplicate by identifier.
	singleItem := `[{"id":100,"number":1,"title":"Dedup issue","body":null,"state":"open","html_url":"u","labels":[{"name":"backlog"}],"assignees":[],"type":null,"pull_request":null,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]`
	singleSearch := `{"total_count":1,"incomplete_results":false,"items":[{"id":100,"number":1,"title":"Dedup issue","body":null,"state":"closed","html_url":"u","labels":[{"name":"done"}],"assignees":[],"type":null,"pull_request":null,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if r.URL.Path == "/search/issues" {
			w.Write([]byte(singleSearch)) //nolint:errcheck // test helper
		} else {
			w.Write([]byte(singleItem)) //nolint:errcheck // test helper
		}
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	// Request both active ("backlog") and terminal ("done") — issue #1 matches both.
	issues, err := a.FetchIssuesByStates(context.Background(), []string{"backlog", "done"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates: %v", err)
	}

	if len(issues) != 1 {
		t.Fatalf("len = %d, want 1 (deduplication)", len(issues))
	}
}

// --- FetchIssueStatesByIDs ---

func TestFetchIssueStatesByIDs_Success(t *testing.T) {
	t.Parallel()

	issueData := issueJSON(42, "in-progress", "open")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(issueData)) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	result, err := a.FetchIssueStatesByIDs(context.Background(), []string{"42"})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs: %v", err)
	}

	if result["42"] != "in-progress" {
		t.Errorf("result[\"42\"] = %q, want in-progress", result["42"])
	}
}

func TestFetchIssueStatesByIDs_Empty(t *testing.T) {
	t.Parallel()

	a := mustAdapter(t, validConfig("http://localhost"))
	result, err := a.FetchIssueStatesByIDs(context.Background(), []string{})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs: %v", err)
	}
	if result == nil {
		t.Fatal("result is nil, want non-nil empty map")
	}
	if len(result) != 0 {
		t.Errorf("len = %d, want 0", len(result))
	}
}

func TestFetchIssueStatesByIDs_NotFoundOmitted(t *testing.T) {
	t.Parallel()

	// First issue found, second returns 404 (omit from map).
	var callNum int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callNum, 1)
		if n == 1 {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(issueJSON(1, "backlog", "open"))) //nolint:errcheck // test helper
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	result, err := a.FetchIssueStatesByIDs(context.Background(), []string{"1", "999"})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs: %v", err)
	}

	if _, ok := result["1"]; !ok {
		t.Error("result should contain issue 1")
	}
	if _, ok := result["999"]; ok {
		t.Error("result should NOT contain 999 (not found)")
	}
}

func TestFetchIssueStatesByIDs_ContextCancellation(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	a := mustAdapter(t, validConfig(srv.URL))

	errCh := make(chan error, 1)
	go func() {
		_, err := a.FetchIssueStatesByIDs(ctx, []string{"1"})
		errCh <- err
	}()

	<-started
	cancel()

	err := <-errCh
	if err == nil {
		t.Fatal("expected error after context cancel, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

// --- FetchIssueStatesByIdentifiers ---

func TestFetchIssueStatesByIdentifiers_Success(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(issueJSON(7, "review", "open"))) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	result, err := a.FetchIssueStatesByIdentifiers(context.Background(), []string{"7"})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIdentifiers: %v", err)
	}

	// Since ID == Identifier == number, keyed by number.
	if result["7"] != "review" {
		t.Errorf("result[\"7\"] = %q, want review", result["7"])
	}
}

// --- FetchIssueComments ---

func TestFetchIssueComments_SinglePage(t *testing.T) {
	t.Parallel()

	fixture := loadFixture(t, "comments.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(fixture) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	comments, err := a.FetchIssueComments(context.Background(), "42")
	if err != nil {
		t.Fatalf("FetchIssueComments: %v", err)
	}

	if len(comments) != 2 {
		t.Fatalf("len = %d, want 2", len(comments))
	}
	if comments[0].Author != "alice" {
		t.Errorf("comments[0].Author = %q, want alice", comments[0].Author)
	}
	if comments[1].Author != "bob" {
		t.Errorf("comments[1].Author = %q, want bob", comments[1].Author)
	}
}

func TestFetchIssueComments_EmptyIsNonNil(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("[]")) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	comments, err := a.FetchIssueComments(context.Background(), "42")
	if err != nil {
		t.Fatalf("FetchIssueComments: %v", err)
	}
	if comments == nil {
		t.Fatal("comments is nil, want non-nil empty slice")
	}
	if len(comments) != 0 {
		t.Errorf("len = %d, want 0", len(comments))
	}
}

func TestFetchIssueComments_Pagination(t *testing.T) {
	t.Parallel()

	page1 := loadFixture(t, "comments.json") // 2 comments
	page2 := `[{"id":9999,"user":{"login":"charlie"},"body":"Third comment.","created_at":"2026-01-17T09:00:00Z"}]`

	var srvURL string
	var callCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callCount, 1)
		if n == 1 {
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/owner/repo/issues/42/comments?page=2>; rel="next"`, srvURL))
			w.WriteHeader(http.StatusOK)
			w.Write(page1) //nolint:errcheck // test helper
		} else {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(page2)) //nolint:errcheck // test helper
		}
	}))
	defer srv.Close()
	srvURL = srv.URL

	a := mustAdapter(t, validConfig(srv.URL))
	comments, err := a.FetchIssueComments(context.Background(), "42")
	if err != nil {
		t.Fatalf("FetchIssueComments: %v", err)
	}

	if len(comments) != 3 {
		t.Fatalf("len = %d, want 3 across 2 pages", len(comments))
	}
	if comments[2].Author != "charlie" {
		t.Errorf("comments[2].Author = %q, want charlie", comments[2].Author)
	}
}

func TestFetchIssueComments_NotFound(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	_, err := a.FetchIssueComments(context.Background(), "999")
	assertTrackerErrorKind(t, err, domain.ErrTrackerNotFound)
}

// --- TransitionIssue ---

// transitionServer sets up an httptest server that simulates GitHub label and
// state operations for TransitionIssue. It tracks which API calls were made.
type transitionServer struct {
	srv            *httptest.Server
	deleteCount    int32
	postCount      int32
	patchCount     int32
	deleteFailWith int // if non-zero, respond with this status on DELETE
	postFailWith   int // if non-zero, respond with this status on POST labels
	patchFailWith  int // if non-zero, respond with this status on PATCH
}

func newTransitionServer(t *testing.T, number int, currentLabel, nativeState string) *transitionServer {
	t.Helper()
	ts := &transitionServer{}
	base := fmt.Sprintf("/repos/owner/repo/issues/%d", number)

	mux := http.NewServeMux()
	// GET issue
	mux.HandleFunc(base, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PATCH" {
			// Open/close call.
			if ts.patchFailWith != 0 {
				w.WriteHeader(ts.patchFailWith)
				return
			}
			atomic.AddInt32(&ts.patchCount, 1)
			w.WriteHeader(http.StatusOK)
			body, _ := io.ReadAll(r.Body)
			w.Write(body) //nolint:errcheck // test helper
			return
		}
		// GET
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(issueJSON(number, currentLabel, nativeState))) //nolint:errcheck // test helper
	})
	// DELETE label
	mux.HandleFunc(base+"/labels/", func(w http.ResponseWriter, r *http.Request) {
		if ts.deleteFailWith != 0 {
			w.WriteHeader(ts.deleteFailWith)
			return
		}
		atomic.AddInt32(&ts.deleteCount, 1)
		w.WriteHeader(http.StatusOK)
	})
	// POST labels
	mux.HandleFunc(base+"/labels", func(w http.ResponseWriter, r *http.Request) {
		if ts.postFailWith != 0 {
			w.WriteHeader(ts.postFailWith)
			return
		}
		atomic.AddInt32(&ts.postCount, 1)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[]`)) //nolint:errcheck // test helper
	})

	ts.srv = httptest.NewServer(mux)
	t.Cleanup(ts.srv.Close)
	return ts
}

func TestTransitionIssue_LabelSwap(t *testing.T) {
	t.Parallel()

	// Current: "backlog" (active), native: open → Target: "review" (active).
	// Expected: DELETE backlog, POST review, no PATCH.
	ts := newTransitionServer(t, 1, "backlog", "open")

	a := mustAdapter(t, validConfig(ts.srv.URL))
	if err := a.TransitionIssue(context.Background(), "1", "review"); err != nil {
		t.Fatalf("TransitionIssue: %v", err)
	}

	if got := atomic.LoadInt32(&ts.deleteCount); got != 1 {
		t.Errorf("DELETE count = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&ts.postCount); got != 1 {
		t.Errorf("POST count = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&ts.patchCount); got != 0 {
		t.Errorf("PATCH count = %d, want 0 (no native state change needed)", got)
	}
}

func TestTransitionIssue_CloseOnTerminal(t *testing.T) {
	t.Parallel()

	// Current: "in-progress" (active), native: open → Target: "done" (terminal).
	// Expected: DELETE in-progress, POST done, PATCH close.
	ts := newTransitionServer(t, 5, "in-progress", "open")

	a := mustAdapter(t, validConfig(ts.srv.URL))
	if err := a.TransitionIssue(context.Background(), "5", "done"); err != nil {
		t.Fatalf("TransitionIssue: %v", err)
	}

	if got := atomic.LoadInt32(&ts.deleteCount); got != 1 {
		t.Errorf("DELETE count = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&ts.postCount); got != 1 {
		t.Errorf("POST count = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&ts.patchCount); got != 1 {
		t.Errorf("PATCH count = %d, want 1 (close issue)", got)
	}
}

func TestTransitionIssue_ReopenOnActive(t *testing.T) {
	t.Parallel()

	// Current: "done" (terminal), native: closed → Target: "review" (active).
	// Expected: DELETE done, POST review, PATCH reopen.
	ts := newTransitionServer(t, 8, "done", "closed")

	a := mustAdapter(t, validConfig(ts.srv.URL))
	if err := a.TransitionIssue(context.Background(), "8", "review"); err != nil {
		t.Fatalf("TransitionIssue: %v", err)
	}

	if got := atomic.LoadInt32(&ts.deleteCount); got != 1 {
		t.Errorf("DELETE count = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&ts.postCount); got != 1 {
		t.Errorf("POST count = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&ts.patchCount); got != 1 {
		t.Errorf("PATCH count = %d, want 1 (reopen issue)", got)
	}
}

func TestTransitionIssue_IdempotentNoOp(t *testing.T) {
	t.Parallel()

	// Current label already matches target → no label API calls.
	ts := newTransitionServer(t, 3, "review", "open")

	a := mustAdapter(t, validConfig(ts.srv.URL))
	if err := a.TransitionIssue(context.Background(), "3", "review"); err != nil {
		t.Fatalf("TransitionIssue idempotent: %v", err)
	}

	if got := atomic.LoadInt32(&ts.deleteCount); got != 0 {
		t.Errorf("DELETE count = %d, want 0", got)
	}
	if got := atomic.LoadInt32(&ts.postCount); got != 0 {
		t.Errorf("POST count = %d, want 0", got)
	}
}

func TestTransitionIssue_PartialFailure_AddLabel(t *testing.T) {
	t.Parallel()

	// DELETE succeeds, POST fails → error returned.
	// On retry, DELETE is 404 (already removed) and POST should succeed.
	ts := newTransitionServer(t, 2, "backlog", "open")
	ts.postFailWith = http.StatusInternalServerError

	a := mustAdapter(t, validConfig(ts.srv.URL))
	err := a.TransitionIssue(context.Background(), "2", "review")
	assertTrackerErrorKind(t, err, domain.ErrTrackerTransport)

	if got := atomic.LoadInt32(&ts.deleteCount); got != 1 {
		t.Errorf("DELETE count = %d, want 1 (delete succeeded before add failed)", got)
	}
}

func TestTransitionIssue_InvalidTargetState(t *testing.T) {
	t.Parallel()

	// "unknown" is not in the default active or terminal state lists.
	a := mustAdapter(t, validConfig("http://localhost"))
	err := a.TransitionIssue(context.Background(), "1", "unknown")
	assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)
}

func TestTransitionIssue_HandoffToActive(t *testing.T) {
	t.Parallel()

	// Issue currently carries the handoff label ("review"), native state open.
	// TransitionIssue to an active state must DELETE the handoff label and
	// POST the active label. No PATCH needed (already open).
	ts := newTransitionServer(t, 10, "review", "open")
	cfg := validConfig(ts.srv.URL)
	cfg["active_states"] = []any{"backlog", "in-progress"}
	cfg["terminal_states"] = []any{"done"}
	cfg["handoff_state"] = "review"
	a := mustAdapter(t, cfg)

	if err := a.TransitionIssue(context.Background(), "10", "in-progress"); err != nil {
		t.Fatalf("TransitionIssue: %v", err)
	}

	if got := atomic.LoadInt32(&ts.deleteCount); got != 1 {
		t.Errorf("DELETE count = %d, want 1 (stale handoff label must be removed)", got)
	}
	if got := atomic.LoadInt32(&ts.postCount); got != 1 {
		t.Errorf("POST count = %d, want 1 (active label must be added)", got)
	}
	if got := atomic.LoadInt32(&ts.patchCount); got != 0 {
		t.Errorf("PATCH count = %d, want 0 (issue already open, no native state change)", got)
	}
}

func TestTransitionIssue_HandoffAsTarget(t *testing.T) {
	t.Parallel()

	// Issue currently carries an active label ("in-progress"), native state open.
	// TransitionIssue to the handoff state must:
	//   - Be accepted (not rejected as ErrTrackerPayload).
	//   - DELETE the active label.
	//   - POST the handoff label.
	//   - Not PATCH native state (handoff issues stay open).
	ts := newTransitionServer(t, 11, "in-progress", "open")
	cfg := validConfig(ts.srv.URL)
	cfg["active_states"] = []any{"backlog", "in-progress"}
	cfg["terminal_states"] = []any{"done"}
	cfg["handoff_state"] = "review"
	a := mustAdapter(t, cfg)

	if err := a.TransitionIssue(context.Background(), "11", "review"); err != nil {
		t.Fatalf("TransitionIssue: %v", err)
	}

	if got := atomic.LoadInt32(&ts.deleteCount); got != 1 {
		t.Errorf("DELETE count = %d, want 1 (active label must be removed)", got)
	}
	if got := atomic.LoadInt32(&ts.postCount); got != 1 {
		t.Errorf("POST count = %d, want 1 (handoff label must be added)", got)
	}
	if got := atomic.LoadInt32(&ts.patchCount); got != 0 {
		t.Errorf("PATCH count = %d, want 0 (handoff state never changes native open/closed)", got)
	}
}

func TestTransitionIssue_UnknownStateWithHandoffConfigured(t *testing.T) {
	t.Parallel()

	// When handoff_state is configured, states outside the three valid categories
	// (active, terminal, handoff) must still be rejected.
	cfg := validConfig("http://localhost")
	cfg["active_states"] = []any{"backlog"}
	cfg["terminal_states"] = []any{"done"}
	cfg["handoff_state"] = "review"
	a := mustAdapter(t, cfg)

	err := a.TransitionIssue(context.Background(), "1", "unknown-state")
	assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)
}

func TestTransitionIssue_PartialFailure_Close(t *testing.T) {
	t.Parallel()

	// DELETE and POST succeed, PATCH close fails → error returned.
	ts := newTransitionServer(t, 6, "review", "open")
	ts.patchFailWith = http.StatusInternalServerError

	a := mustAdapter(t, validConfig(ts.srv.URL))
	err := a.TransitionIssue(context.Background(), "6", "done")
	assertTrackerErrorKind(t, err, domain.ErrTrackerTransport)

	if got := atomic.LoadInt32(&ts.deleteCount); got != 1 {
		t.Errorf("DELETE count = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&ts.postCount); got != 1 {
		t.Errorf("POST count = %d, want 1", got)
	}
}

func TestTransitionIssue_LabelURLEncoding(t *testing.T) {
	t.Parallel()

	// Label with spaces must be URL-path-escaped in DELETE request.
	var deletedLabelPath string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/issues/1", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// Current label has a space.
		w.Write([]byte(`{"id":1,"number":1,"title":"T","body":null,"state":"open","html_url":"u","labels":[{"name":"code review"}],"assignees":[],"type":null,"pull_request":null,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`)) //nolint:errcheck // test helper
	})
	mux.HandleFunc("/repos/owner/repo/issues/1/labels/", func(w http.ResponseWriter, r *http.Request) {
		// EscapedPath() returns the percent-encoded form, confirming the label
		// was properly URL-encoded before sending (spaces → %20).
		deletedLabelPath = r.URL.EscapedPath()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/repos/owner/repo/issues/1/labels", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("[]")) //nolint:errcheck // test helper
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := validConfig(srv.URL)
	cfg["active_states"] = []any{"code review", "done-review"}
	cfg["terminal_states"] = []any{"complete"}
	a := mustAdapter(t, cfg)

	if err := a.TransitionIssue(context.Background(), "1", "done-review"); err != nil {
		t.Fatalf("TransitionIssue: %v", err)
	}

	// Verify "code review" was URL-encoded in the DELETE path.
	if !strings.Contains(deletedLabelPath, "code%20review") {
		t.Errorf("DELETE path = %q, should contain URL-encoded label 'code%%20review'", deletedLabelPath)
	}
}

// --- CommentIssue ---

func TestCommentIssue_Success(t *testing.T) {
	t.Parallel()

	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("method = %q, want POST", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody) //nolint:errcheck // test helper
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":12345}`)) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	if err := a.CommentIssue(context.Background(), "42", "Hello from Sortie!"); err != nil {
		t.Fatalf("CommentIssue: %v", err)
	}

	if gotBody["body"] != "Hello from Sortie!" {
		t.Errorf("request body.body = %q, want %q", gotBody["body"], "Hello from Sortie!")
	}
}

func TestCommentIssue_Error(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	err := a.CommentIssue(context.Background(), "999", "comment")
	assertTrackerErrorKind(t, err, domain.ErrTrackerNotFound)
}

// --- SetMetrics ---

func TestSetMetrics_RecordsOperations(t *testing.T) {
	t.Parallel()

	fixture := loadFixture(t, "issues.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(fixture) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	spy := &spyMetrics{}
	a := mustAdapter(t, validConfig(srv.URL))
	a.SetMetrics(spy)

	_, err := a.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}

	found := false
	for _, call := range spy.calls {
		if call == "fetch_candidates:success" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("metrics calls = %v, want fetch_candidates:success recorded", spy.calls)
	}
}

func TestSetMetrics_NilMetricsDoesNotPanic(t *testing.T) {
	t.Parallel()

	fixture := loadFixture(t, "issues.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(fixture) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	// Adapter with no SetMetrics call — metrics field is nil.
	a := mustAdapter(t, validConfig(srv.URL))
	// Must not panic.
	_, err := a.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
}

// --- FetchCandidateIssues (search path) ---

func TestFetchCandidateIssues_SearchPagination(t *testing.T) {
	t.Parallel()

	// Two-page search result. query_filter non-empty routes through fetchCandidatesViaSearch.
	page1 := `{"total_count":2,"incomplete_results":false,"items":[{"id":10,"number":10,"title":"T10","body":null,"state":"open","html_url":"u","labels":[{"name":"review"}],"assignees":[],"type":null,"pull_request":null,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]}`
	page2 := `{"total_count":2,"incomplete_results":false,"items":[{"id":20,"number":20,"title":"T20","body":null,"state":"open","html_url":"u","labels":[{"name":"backlog"}],"assignees":[],"type":null,"pull_request":null,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]}`

	var srvURL string
	var callCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callCount, 1)
		if n == 1 {
			w.Header().Set("Link", fmt.Sprintf(`<%s/search/issues?q=...&page=2>; rel="next"`, srvURL))
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(page1)) //nolint:errcheck // test helper
		} else {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(page2)) //nolint:errcheck // test helper
		}
	}))
	defer srv.Close()
	srvURL = srv.URL

	cfg := validConfig(srv.URL)
	cfg["query_filter"] = "label:important"
	a := mustAdapter(t, cfg)
	issues, err := a.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues search pagination: %v", err)
	}
	if len(issues) != 2 {
		t.Errorf("len = %d, want 2 (one from each search page)", len(issues))
	}
}

func TestFetchCandidateIssues_SearchIncompleteResults(t *testing.T) {
	t.Parallel()

	// incomplete_results=true should log a warning but not return an error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"total_count":100,"incomplete_results":true,"items":[{"id":1,"number":1,"title":"T","body":null,"state":"open","html_url":"u","labels":[{"name":"review"}],"assignees":[],"type":null,"pull_request":null,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]}`)) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	cfg := validConfig(srv.URL)
	cfg["query_filter"] = "type:bug"
	a := mustAdapter(t, cfg)
	issues, err := a.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues incomplete results: unexpected error: %v", err)
	}
	if len(issues) != 1 {
		t.Errorf("len = %d, want 1", len(issues))
	}
}

func TestFetchCandidateIssues_SearchAPIError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := validConfig(srv.URL)
	cfg["query_filter"] = "label:critical"
	a := mustAdapter(t, cfg)
	_, err := a.FetchCandidateIssues(context.Background())
	assertTrackerErrorKind(t, err, domain.ErrTrackerTransport)
}

// --- FetchIssueByID additional error paths ---

func TestFetchIssueByID_NonNotFoundError(t *testing.T) {
	t.Parallel()

	// 500 on the issue fetch should return ErrTrackerTransport (not ErrTrackerNotFound).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	_, err := a.FetchIssueByID(context.Background(), "42")
	assertTrackerErrorKind(t, err, domain.ErrTrackerTransport)
}

func TestFetchIssueByID_BlockerAPIError(t *testing.T) {
	t.Parallel()

	// blockers endpoint returns 500 (not 404) → error propagated.
	issueFix := loadFixture(t, "issue.json")

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/issues/42/dependencies/blocked_by", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	mux.HandleFunc("/repos/owner/repo/issues/42", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(issueFix) //nolint:errcheck // test helper
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	_, err := a.FetchIssueByID(context.Background(), "42")
	assertTrackerErrorKind(t, err, domain.ErrTrackerTransport)
}

func TestFetchIssueByID_ParentAPIError(t *testing.T) {
	t.Parallel()

	// parent endpoint returns 500 (not 404) → error propagated.
	issueFix := loadFixture(t, "issue.json")

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/issues/42/dependencies/blocked_by", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("[]")) //nolint:errcheck // test helper
	})
	mux.HandleFunc("/repos/owner/repo/issues/42/parent", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	mux.HandleFunc("/repos/owner/repo/issues/42", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(issueFix) //nolint:errcheck // test helper
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	_, err := a.FetchIssueByID(context.Background(), "42")
	assertTrackerErrorKind(t, err, domain.ErrTrackerTransport)
}

// --- FetchIssuesByStates additional paths ---

func TestFetchIssuesByStates_OpenPagination(t *testing.T) {
	t.Parallel()

	// Server returns page1 (issues.json) with a Link header; second call returns page2 (issues_page2.json).
	page1 := loadFixture(t, "issues.json")
	page2 := loadFixture(t, "issues_page2.json")

	var srvURL string
	var callCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callCount, 1)
		if n == 1 {
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/owner/repo/issues?state=open&page=2>; rel="next"`, srvURL))
			w.WriteHeader(http.StatusOK)
			w.Write(page1) //nolint:errcheck // test helper
		} else {
			w.WriteHeader(http.StatusOK)
			w.Write(page2) //nolint:errcheck // test helper
		}
	}))
	defer srv.Close()
	srvURL = srv.URL

	a := mustAdapter(t, validConfig(srv.URL))
	// Request only active states; none are terminal so uses fetchOpenIssuesByStates.
	issues, err := a.FetchIssuesByStates(context.Background(), []string{"backlog", "in-progress", "review"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates open pagination: %v", err)
	}
	// issues.json: #1 backlog + #3 in-progress (2 non-PR active issues, #4 done is filtered out).
	// issues_page2.json: #5 review (1 issue).
	if len(issues) != 3 {
		t.Errorf("len = %d, want 3 across 2 pages", len(issues))
	}
}

func TestFetchIssuesByStates_ClosedSearchPagination(t *testing.T) {
	t.Parallel()

	page1 := `{"total_count":2,"incomplete_results":false,"items":[{"id":100,"number":100,"title":"D1","body":null,"state":"closed","html_url":"u","labels":[{"name":"done"}],"assignees":[],"type":null,"pull_request":null,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]}`
	page2 := `{"total_count":2,"incomplete_results":false,"items":[{"id":200,"number":200,"title":"D2","body":null,"state":"closed","html_url":"u","labels":[{"name":"done"}],"assignees":[],"type":null,"pull_request":null,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]}`

	var srvURL string
	var callCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callCount, 1)
		if n == 1 {
			w.Header().Set("Link", fmt.Sprintf(`<%s/search/issues?q=...&page=2>; rel="next"`, srvURL))
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(page1)) //nolint:errcheck // test helper
		} else {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(page2)) //nolint:errcheck // test helper
		}
	}))
	defer srv.Close()
	srvURL = srv.URL

	a := mustAdapter(t, validConfig(srv.URL))
	issues, err := a.FetchIssuesByStates(context.Background(), []string{"done"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates closed search pagination: %v", err)
	}
	if len(issues) != 2 {
		t.Errorf("len = %d, want 2 across 2 search pages", len(issues))
	}
}

func TestFetchIssuesByStates_ClosedSearchIncompleteResults(t *testing.T) {
	t.Parallel()

	// incomplete_results=true for closed search: should log warning, not error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"total_count":999,"incomplete_results":true,"items":[{"id":50,"number":50,"title":"Partial","body":null,"state":"closed","html_url":"u","labels":[{"name":"done"}],"assignees":[],"type":null,"pull_request":null,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]}`)) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	issues, err := a.FetchIssuesByStates(context.Background(), []string{"done"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates closed incomplete: unexpected error: %v", err)
	}
	if len(issues) != 1 {
		t.Errorf("len = %d, want 1", len(issues))
	}
}

func TestFetchIssuesByStates_ActiveStatesError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	_, err := a.FetchIssuesByStates(context.Background(), []string{"backlog"})
	assertTrackerErrorKind(t, err, domain.ErrTrackerTransport)
}

func TestFetchIssuesByStates_TerminalStateError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	_, err := a.FetchIssuesByStates(context.Background(), []string{"done"})
	assertTrackerErrorKind(t, err, domain.ErrTrackerTransport)
}

func TestFetchIssuesByStates_ContextCancelledDuringTerminal(t *testing.T) {
	t.Parallel()

	// Two terminal states: first search succeeds, context is cancelled before
	// the second search, exercising the ctx.Err() guard between iterations.
	started := make(chan struct{}, 1)
	var callCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callCount, 1)
		if n == 1 {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"total_count":0,"incomplete_results":false,"items":[]}`)) //nolint:errcheck // test helper
			started <- struct{}{}
			return
		}
		// Second search: block until context is cancelled.
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := mustAdapter(t, validConfig(srv.URL))
	errCh := make(chan error, 1)
	go func() {
		// defaultTerminalStates = ["done", "wontfix"]; both are terminal so both
		// iterate through the ctx.Err() guarded loop in FetchIssuesByStates.
		_, err := a.FetchIssuesByStates(ctx, []string{"done", "wontfix"})
		errCh <- err
	}()

	<-started
	cancel()

	err := <-errCh
	if err == nil {
		t.Fatal("expected error after context cancel, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

// --- FetchIssueStatesByIDs additional paths ---

func TestFetchIssueStatesByIDs_SkipsPullRequest(t *testing.T) {
	t.Parallel()

	// API returns a PR for the requested identifier → it must be omitted from the result map.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// pull_request:{} marks this as a PR.
		w.Write([]byte(`{"id":1,"number":1,"title":"PR","body":null,"state":"open","html_url":"u","labels":[],"assignees":[],"type":null,"pull_request":{},"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`)) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	result, err := a.FetchIssueStatesByIDs(context.Background(), []string{"1"})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs: %v", err)
	}
	if _, ok := result["1"]; ok {
		t.Error("pull request should be omitted from the result map")
	}
}

func TestFetchIssueStatesByIdentifiers_Error(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	_, err := a.FetchIssueStatesByIdentifiers(context.Background(), []string{"5"})
	assertTrackerErrorKind(t, err, domain.ErrTrackerAuth)
}

// --- FetchIssueComments additional paths ---

func TestFetchIssueComments_NonNotFoundError(t *testing.T) {
	t.Parallel()

	// 500 from comments endpoint → ErrTrackerTransport (not wrapped as NotFound).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	_, err := a.FetchIssueComments(context.Background(), "42")
	assertTrackerErrorKind(t, err, domain.ErrTrackerTransport)
}

// --- TransitionIssue additional paths ---

func TestTransitionIssue_GetIssueError(t *testing.T) {
	t.Parallel()

	// Step 1 (GET issue) returns 500 → error propagated; not a NotFound.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	err := a.TransitionIssue(context.Background(), "42", "review")
	assertTrackerErrorKind(t, err, domain.ErrTrackerTransport)
}

func TestTransitionIssue_DeleteLabelIsNotFound(t *testing.T) {
	t.Parallel()

	// DELETE existing label returns 404 (already removed) → treated as no-op; POST still executes.
	var postCount int32
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/issues/1", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(issueJSON(1, "review", "open"))) //nolint:errcheck // test helper
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(issueJSON(1, "backlog", "open"))) //nolint:errcheck // test helper
	})
	mux.HandleFunc("/repos/owner/repo/issues/1/labels/backlog", func(w http.ResponseWriter, r *http.Request) {
		// Label was already removed on a prior attempt.
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/repos/owner/repo/issues/1/labels", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&postCount, 1)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("[]")) //nolint:errcheck // test helper
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	if err := a.TransitionIssue(context.Background(), "1", "review"); err != nil {
		t.Fatalf("TransitionIssue 404-on-delete: %v", err)
	}
	if got := atomic.LoadInt32(&postCount); got != 1 {
		t.Errorf("POST count = %d, want 1 (add-label must still execute)", got)
	}
}

// --- ETag cache constructor tests ---

func TestNewGitHubAdapter_ETagCacheSizeDefault(t *testing.T) {
	t.Parallel()

	a := mustAdapter(t, validConfig("http://localhost"))
	if a.etagCache.maxSize != 1000 {
		t.Errorf("etagCache.maxSize = %d, want 1000 (default)", a.etagCache.maxSize)
	}
}

func TestNewGitHubAdapter_ETagCacheSizeCustom(t *testing.T) {
	t.Parallel()

	cfg := validConfig("http://localhost")
	cfg["etag_cache_size"] = 50
	a := mustAdapter(t, cfg)
	if a.etagCache.maxSize != 50 {
		t.Errorf("etagCache.maxSize = %d, want 50", a.etagCache.maxSize)
	}
}

func TestNewGitHubAdapter_ETagCacheSizeZero(t *testing.T) {
	t.Parallel()

	cfg := validConfig("http://localhost")
	cfg["etag_cache_size"] = 0
	a := mustAdapter(t, cfg)
	if a.etagCache.maxSize != 0 {
		t.Errorf("etagCache.maxSize = %d, want 0 (disabled)", a.etagCache.maxSize)
	}
}

func TestNewGitHubAdapter_ETagCacheSizeFloat64(t *testing.T) {
	t.Parallel()

	// encoding/json deserializes integers as float64; verify the adapter handles it.
	cfg := validConfig("http://localhost")
	cfg["etag_cache_size"] = float64(75)
	a := mustAdapter(t, cfg)
	if a.etagCache.maxSize != 75 {
		t.Errorf("etagCache.maxSize = %d, want 75 (float64 key)", a.etagCache.maxSize)
	}
}

func TestNewGitHubAdapter_ETagCacheSizeInvalid(t *testing.T) {
	t.Parallel()

	cfg := validConfig("http://localhost")
	cfg["etag_cache_size"] = "not a number"
	a := mustAdapter(t, cfg)
	if a.etagCache.maxSize != 1000 {
		t.Errorf("etagCache.maxSize = %d, want 1000 (invalid falls back to default)", a.etagCache.maxSize)
	}
}

func TestNewGitHubAdapter_ETagCacheSizeNegative(t *testing.T) {
	t.Parallel()

	cfg := validConfig("http://localhost")
	cfg["etag_cache_size"] = -5
	a := mustAdapter(t, cfg)
	if a.etagCache.maxSize != 1000 {
		t.Errorf("etagCache.maxSize = %d, want 1000 (negative falls back to default)", a.etagCache.maxSize)
	}
}

// --- FetchIssueStatesByIDs — ETag conditional request tests ---

func TestFetchIssueStatesByIDs_ConditionalRequest_304(t *testing.T) {
	t.Parallel()

	var callCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callCount, 1)
		if n == 1 {
			w.Header().Set("ETag", `"etag-v1"`)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(issueJSON(42, "in-progress", "open"))) //nolint:errcheck // test helper
			return
		}
		// Second call must include If-None-Match carrying the cached ETag.
		if h := r.Header.Get("If-None-Match"); h != `"etag-v1"` {
			t.Errorf("second call If-None-Match = %q, want %q", h, `"etag-v1"`)
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()

	cfg := validConfig(srv.URL)
	cfg["etag_cache_size"] = 100
	a := mustAdapter(t, cfg)

	result1, err := a.FetchIssueStatesByIDs(context.Background(), []string{"42"})
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if result1["42"] != "in-progress" {
		t.Errorf("first call result[\"42\"] = %q, want in-progress", result1["42"])
	}

	// Second call: server returns 304; adapter must return the cached state.
	result2, err := a.FetchIssueStatesByIDs(context.Background(), []string{"42"})
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if result2["42"] != "in-progress" {
		t.Errorf("second call result[\"42\"] = %q, want in-progress (304 hit)", result2["42"])
	}
	if n := atomic.LoadInt32(&callCount); n != 2 {
		t.Errorf("server call count = %d, want 2", n)
	}
}

func TestFetchIssueStatesByIDs_ConditionalRequest_200_UpdatesCache(t *testing.T) {
	t.Parallel()

	var callCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callCount, 1)
		switch n {
		case 1:
			w.Header().Set("ETag", `"etag-v1"`)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(issueJSON(5, "in-progress", "open"))) //nolint:errcheck // test helper
		case 2:
			// State changed on server; new ETag and new state label.
			w.Header().Set("ETag", `"etag-v2"`)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(issueJSON(5, "review", "open"))) //nolint:errcheck // test helper
		default:
			// Third call: cache must carry the new ETag from call 2.
			if h := r.Header.Get("If-None-Match"); h != `"etag-v2"` {
				t.Errorf("third call If-None-Match = %q, want %q", h, `"etag-v2"`)
			}
			w.WriteHeader(http.StatusNotModified)
		}
	}))
	defer srv.Close()

	cfg := validConfig(srv.URL)
	cfg["etag_cache_size"] = 100
	a := mustAdapter(t, cfg)

	result1, err := a.FetchIssueStatesByIDs(context.Background(), []string{"5"})
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if result1["5"] != "in-progress" {
		t.Errorf("first call result[\"5\"] = %q, want in-progress", result1["5"])
	}

	// Second call: server returns new state; cache must be updated.
	result2, err := a.FetchIssueStatesByIDs(context.Background(), []string{"5"})
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if result2["5"] != "review" {
		t.Errorf("second call result[\"5\"] = %q, want review", result2["5"])
	}

	// Third call: cache must use updated ETag from call 2.
	result3, err := a.FetchIssueStatesByIDs(context.Background(), []string{"5"})
	if err != nil {
		t.Fatalf("third call: %v", err)
	}
	if result3["5"] != "review" {
		t.Errorf("third call result[\"5\"] = %q, want review (cached from call 2)", result3["5"])
	}
}

func TestFetchIssueStatesByIDs_NoETagHeader_NoCacheEntry(t *testing.T) {
	t.Parallel()

	var callCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callCount, 1)
		// Server intentionally omits the ETag header.
		if h := r.Header.Get("If-None-Match"); h != "" {
			t.Errorf("call %d: unexpected If-None-Match = %q (no ETag was ever returned)", n, h)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(issueJSON(10, "backlog", "open"))) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	cfg := validConfig(srv.URL)
	cfg["etag_cache_size"] = 100
	a := mustAdapter(t, cfg)

	// Both calls must be unconditional because the server never returns an ETag.
	for i := 0; i < 2; i++ {
		if _, err := a.FetchIssueStatesByIDs(context.Background(), []string{"10"}); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
	if n := atomic.LoadInt32(&callCount); n != 2 {
		t.Errorf("server call count = %d, want 2 (no caching when ETag absent)", n)
	}
}

func TestFetchIssueStatesByIDs_304_CachedStateUsedAfterEviction(t *testing.T) {
	t.Parallel()

	// Issue 1's server handler blocks until issue 2 has been processed and has
	// evicted issue 1 from the size-1 cache. The 304 for issue 1 must still
	// return the correct state via the local variable captured before the HTTP
	// call — not via a re-lookup of the (now-evicted) cache entry.
	issue1Arrived := make(chan struct{}, 1)
	releaseGA := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/issues/1"):
			select {
			case issue1Arrived <- struct{}{}:
			default:
			}
			<-releaseGA
			w.WriteHeader(http.StatusNotModified)
		case strings.HasSuffix(r.URL.Path, "/issues/2"):
			w.Header().Set("ETag", `"etag-2"`)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(issueJSON(2, "review", "open"))) //nolint:errcheck // test helper
		}
	}))
	defer srv.Close()

	cfg := validConfig(srv.URL)
	cfg["etag_cache_size"] = 1
	a := mustAdapter(t, cfg)

	// Pre-populate the cache for issue 1 so lookup returns a hit before the HTTP call.
	a.etagCache.put("/repos/owner/repo/issues/1", `"etag-1"`, "in-progress")

	// GA: fetch issue 1; the server blocks until releaseGA is closed.
	resultCh := make(chan map[string]string, 1)
	errCh := make(chan error, 1)
	go func() {
		r, err := a.FetchIssueStatesByIDs(context.Background(), []string{"1"})
		errCh <- err
		resultCh <- r
	}()

	// Wait until GA's HTTP request is in flight, then run GB.
	<-issue1Arrived

	// GB: fetch issue 2 synchronously; put(path/2) evicts issue 1 from the size-1 cache.
	if _, err := a.FetchIssueStatesByIDs(context.Background(), []string{"2"}); err != nil {
		t.Fatalf("GB FetchIssueStatesByIDs: %v", err)
	}

	// Verify issue 1 was evicted (cache size=1; issue 2 now occupies the slot).
	if _, _, ok := a.etagCache.lookup("/repos/owner/repo/issues/1"); ok {
		t.Error("issue 1 should have been evicted after issue 2 was inserted")
	}

	// Allow GA's server handler to return 304.
	close(releaseGA)

	if err := <-errCh; err != nil {
		t.Fatalf("GA FetchIssueStatesByIDs: %v", err)
	}
	result := <-resultCh
	// result["1"] must come from the local cachedState, not a re-lookup.
	if result["1"] != "in-progress" {
		t.Errorf("result[\"1\"] = %q, want in-progress (local cachedState preserved after eviction + 304)", result["1"])
	}
}

func TestFetchIssueStatesByIDs_CacheDisabled_ZeroSize(t *testing.T) {
	t.Parallel()

	var callCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callCount, 1)
		if h := r.Header.Get("If-None-Match"); h != "" {
			t.Errorf("call %d: unexpected If-None-Match = %q (cache disabled)", n, h)
		}
		w.Header().Set("ETag", `"etag-v1"`)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(issueJSON(20, "backlog", "open"))) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	cfg := validConfig(srv.URL)
	cfg["etag_cache_size"] = 0 // caching disabled
	a := mustAdapter(t, cfg)

	for i := 0; i < 2; i++ {
		if _, err := a.FetchIssueStatesByIDs(context.Background(), []string{"20"}); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
	if n := atomic.LoadInt32(&callCount); n != 2 {
		t.Errorf("server call count = %d, want 2 (cache disabled, all requests unconditional)", n)
	}
}

func TestFetchIssueStatesByIDs_NetworkError_CachePreserved(t *testing.T) {
	t.Parallel()

	var callCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callCount, 1)
		switch n {
		case 1:
			w.Header().Set("ETag", `"etag-v1"`)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(issueJSON(30, "backlog", "open"))) //nolint:errcheck // test helper
		case 2:
			// Transient server error; must not evict the cache entry.
			w.WriteHeader(http.StatusInternalServerError)
		default:
			// Third call: cache should still carry etag-v1 (not evicted by error).
			if h := r.Header.Get("If-None-Match"); h != `"etag-v1"` {
				t.Errorf("third call If-None-Match = %q, want %q", h, `"etag-v1"`)
			}
			w.WriteHeader(http.StatusNotModified)
		}
	}))
	defer srv.Close()

	cfg := validConfig(srv.URL)
	cfg["etag_cache_size"] = 100
	a := mustAdapter(t, cfg)

	// Call 1: success — populates cache.
	if _, err := a.FetchIssueStatesByIDs(context.Background(), []string{"30"}); err != nil {
		t.Fatalf("first call: %v", err)
	}

	// Call 2: server error — must not evict the cache entry.
	_, err := a.FetchIssueStatesByIDs(context.Background(), []string{"30"})
	assertTrackerErrorKind(t, err, domain.ErrTrackerTransport)

	// Call 3: cache entry preserved; 304 returns correct state.
	result3, err := a.FetchIssueStatesByIDs(context.Background(), []string{"30"})
	if err != nil {
		t.Fatalf("third call: %v", err)
	}
	if result3["30"] != "backlog" {
		t.Errorf("third call result[\"30\"] = %q, want backlog (304 from preserved cache)", result3["30"])
	}
}

func TestFetchCandidateIssueByIDEquivalence(t *testing.T) {
	t.Parallel()

	// Three cases:
	// - Issue 1 ("backlog" label): active, returned by FetchCandidateIssues, local check accepts.
	// - Issue 3 ("done" label): non-active, not returned by FetchCandidateIssues, local check rejects.
	// - Issue 2 (pull request): skipped by FetchCandidateIssues, FetchIssueByID returns ErrTrackerNotFound.
	issue1Body := issueJSON(1, "backlog", "open")
	issue2PRBody := `{"id":200,"number":2,"title":"A PR","body":null,"state":"open","html_url":"https://github.com/owner/repo/pull/2","labels":[{"name":"in-progress"}],"assignees":[],"type":null,"pull_request":{},"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`
	issue3Body := issueJSON(3, "done", "open")
	issueListBody := "[" + issue1Body + "," + issue2PRBody + "," + issue3Body + "]"
	emptyList := "[]"
	emptyComments := "[]"

	mux := http.NewServeMux()

	// FetchCandidateIssues endpoint.
	mux.HandleFunc("/repos/owner/repo/issues", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(issueListBody)) //nolint:errcheck // test helper
	})

	// Issue 1: active, exists.
	mux.HandleFunc("/repos/owner/repo/issues/1/dependencies/blocked_by", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/repos/owner/repo/issues/1/parent", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/repos/owner/repo/issues/1/comments", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(emptyComments)) //nolint:errcheck // test helper
	})
	mux.HandleFunc("/repos/owner/repo/issues/1", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(issue1Body)) //nolint:errcheck // test helper
	})

	// Issue 2 (PR): FetchIssueByID returns ErrTrackerNotFound for PRs.
	mux.HandleFunc("/repos/owner/repo/issues/2", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(issue2PRBody)) //nolint:errcheck // test helper
	})

	// Issue 3: non-active, exists.
	mux.HandleFunc("/repos/owner/repo/issues/3/dependencies/blocked_by", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/repos/owner/repo/issues/3/parent", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/repos/owner/repo/issues/3/comments", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(emptyComments)) //nolint:errcheck // test helper
	})
	mux.HandleFunc("/repos/owner/repo/issues/3", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(issue3Body)) //nolint:errcheck // test helper
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Configure with active states matching the test labels.
	cfg := validConfig(srv.URL)
	cfg["active_states"] = []any{"backlog", "in-progress"}
	cfg["terminal_states"] = []any{"done"}
	a := mustAdapter(t, cfg)
	ctx := context.Background()

	candidates, err := a.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	// Only issue #1 (backlog) appears — PR (#2) filtered, done (#3) non-active.
	if len(candidates) != 1 {
		t.Fatalf("FetchCandidateIssues: got %d issues, want 1 (only backlog)", len(candidates))
	}
	if candidates[0].ID != "1" {
		t.Errorf("candidates[0].ID = %q, want 1", candidates[0].ID)
	}

	activeSet := make(map[string]bool, len(a.activeStates))
	for _, s := range a.activeStates {
		activeSet[s] = true // GitHub adapter already lowercases
	}

	// Case 1: active issue — FetchIssueByID succeeds and local check accepts.
	issue1, err := a.FetchIssueByID(ctx, "1")
	if err != nil {
		t.Fatalf("FetchIssueByID(1): %v", err)
	}
	if issue1.ID != candidates[0].ID {
		t.Errorf("FetchIssueByID(1).ID = %q, want %q", issue1.ID, candidates[0].ID)
	}
	if issue1.State != candidates[0].State {
		t.Errorf("FetchIssueByID(1).State = %q, want %q (detail vs candidate differ)", issue1.State, candidates[0].State)
	}
	if !activeSet[issue1.State] {
		t.Errorf("issue 1 (state=%q): local active check rejects it but it was a candidate", issue1.State)
	}

	// Case 2: PR — FetchIssueByID returns ErrTrackerNotFound.
	_, err = a.FetchIssueByID(ctx, "2")
	assertTrackerErrorKind(t, err, domain.ErrTrackerNotFound)

	// Case 3: non-active issue — FetchIssueByID succeeds but local check rejects.
	issue3, err := a.FetchIssueByID(ctx, "3")
	if err != nil {
		t.Fatalf("FetchIssueByID(3): %v", err)
	}
	if issue3.State == "" {
		t.Fatalf("FetchIssueByID(3).State is empty")
	}
	if activeSet[issue3.State] {
		t.Errorf("issue 3 (state=%q): local active check accepts it but it was not a candidate", issue3.State)
	}
	// Verify issue 3 was not in candidates (ensuring the exclusion is consistent).
	for _, c := range candidates {
		if c.ID == "3" {
			t.Errorf("issue 3 appeared in candidates, want excluded (non-active state)")
		}
	}

	_ = emptyList // suppress unused warning
}
