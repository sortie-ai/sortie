package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sortie-ai/sortie/internal/adaptertest"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/waittest"
)

func validConfig(endpoint string) map[string]any {
	return map[string]any{
		"endpoint": endpoint,
		"api_key":  "test-token",
		"project":  "owner/repo",

		"active_states":   []any{"backlog", "in-progress", "review"},
		"terminal_states": []any{"done", "wontfix"},
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
	adaptertest.AssertTrackerErrorKind(t, err, want)
}

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return data
}

func issueJSON(number int, stateLabel string) string {
	return fmt.Sprintf(`{"id":%d,"number":%d,"title":"Issue %d","body":null,"state":"open","html_url":"https://github.com/owner/repo/issues/%d","labels":[{"name":%q}],"assignees":[],"type":null,"pull_request":null,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`,
		number*100, number, number, number, stateLabel)
}

type spyMetrics struct {
	domain.NoopMetrics
	calls []string
}

func (s *spyMetrics) IncTrackerRequests(operation, result string) {
	s.calls = append(s.calls, operation+":"+result)
}

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
		{
			name:     "api_key wrong type",
			config:   map[string]any{"api_key": 4242, "project": "owner/repo"},
			wantErr:  true,
			wantKind: domain.ErrTrackerPayload,
		},
		{
			name:     "project wrong type",
			config:   map[string]any{"api_key": "tok", "project": true},
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

// TestNewGitHubAdapter_TypeFaultVsAbsentKey covers the distinction between
// a wrong-typed config key and an absent one for api_key and project: the
// type fault reports domain.ErrTrackerPayload with the typed-fault
// message, distinct from the absent key's own kind
// (domain.ErrMissingTrackerAPIKey / domain.ErrMissingTrackerProject) and
// message.
func TestNewGitHubAdapter_TypeFaultVsAbsentKey(t *testing.T) {
	t.Parallel()

	t.Run("api_key wrong type", func(t *testing.T) {
		t.Parallel()

		config := validConfig("https://api.github.com")
		config["api_key"] = 4242

		_, err := NewGitHubAdapter(config)

		var te *domain.TrackerError
		if !errors.As(err, &te) {
			t.Fatalf("error type = %T, want *domain.TrackerError", err)
		}
		if te.Kind != domain.ErrTrackerPayload {
			t.Errorf("TrackerError.Kind = %q, want %q", te.Kind, domain.ErrTrackerPayload)
		}
		if te.Message != "api_key: expected string, got integer" {
			t.Errorf("TrackerError.Message = %q, want %q", te.Message, "api_key: expected string, got integer")
		}
	})

	t.Run("api_key absent", func(t *testing.T) {
		t.Parallel()

		config := validConfig("https://api.github.com")
		delete(config, "api_key")

		_, err := NewGitHubAdapter(config)

		var te *domain.TrackerError
		if !errors.As(err, &te) {
			t.Fatalf("error type = %T, want *domain.TrackerError", err)
		}
		if te.Kind != domain.ErrMissingTrackerAPIKey {
			t.Errorf("TrackerError.Kind = %q, want %q", te.Kind, domain.ErrMissingTrackerAPIKey)
		}
		if te.Message != "missing required config key: api_key" {
			t.Errorf("TrackerError.Message = %q, want %q", te.Message, "missing required config key: api_key")
		}
	})

	t.Run("project wrong type", func(t *testing.T) {
		t.Parallel()

		config := validConfig("https://api.github.com")
		config["project"] = true

		_, err := NewGitHubAdapter(config)

		var te *domain.TrackerError
		if !errors.As(err, &te) {
			t.Fatalf("error type = %T, want *domain.TrackerError", err)
		}
		if te.Kind != domain.ErrTrackerPayload {
			t.Errorf("TrackerError.Kind = %q, want %q", te.Kind, domain.ErrTrackerPayload)
		}
		if te.Message != "project: expected string, got boolean" {
			t.Errorf("TrackerError.Message = %q, want %q", te.Message, "project: expected string, got boolean")
		}
	})

	t.Run("project absent", func(t *testing.T) {
		t.Parallel()

		config := validConfig("https://api.github.com")
		delete(config, "project")

		_, err := NewGitHubAdapter(config)

		var te *domain.TrackerError
		if !errors.As(err, &te) {
			t.Fatalf("error type = %T, want *domain.TrackerError", err)
		}
		if te.Kind != domain.ErrMissingTrackerProject {
			t.Errorf("TrackerError.Kind = %q, want %q", te.Kind, domain.ErrMissingTrackerProject)
		}
		if te.Message != "missing required config key: project" {
			t.Errorf("TrackerError.Message = %q, want %q", te.Message, "missing required config key: project")
		}
	})
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
			_, _ = io.WriteString(w, issueJSON(123, "todo"))
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

func TestFetchCandidateIssues_FiltersPullRequests(t *testing.T) {
	t.Parallel()

	// issues.json contains: backlog issue (1), PR (2, filtered), in-progress (3), done (4, non-active).
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

	// Issues 1 (backlog) and 3 (in-progress) pass. PR 2 filtered. Issue 4 (done=terminal) filtered.
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

func TestFetchCandidateIssues_RequiresConfiguredStateLabel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		labels        []string
		wantCandidate bool
	}{
		{name: "open issue without labels"},
		{name: "open issue with only unrelated labels", labels: []string{"bug"}},
		{name: "handoff label outranks an active one", labels: []string{"backlog", "needs-human"}},
		{name: "active label in another case", labels: []string{"In-Progress"}, wantCandidate: true},
	}

	for _, route := range []string{"issues", "search"} {
		for _, tt := range tests {
			t.Run(route+"/"+tt.name, func(t *testing.T) {
				t.Parallel()

				nodes := make([]string, len(tt.labels))
				for i, l := range tt.labels {
					nodes[i] = fmt.Sprintf(`{"name":%q}`, l)
				}
				issue := `{"id":7,"number":7,"title":"T","state":"open","html_url":"u","labels":[` + strings.Join(nodes, ",") + `]}`
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case r.URL.Path == "/search/issues":
						_, _ = io.WriteString(w, `{"items":[`+issue+`]}`)
					case r.URL.Path == "/repos/owner/repo/issues" && r.URL.Query().Get("state") == "open":
						_, _ = io.WriteString(w, "["+issue+"]")
					default:
						_, _ = io.WriteString(w, "[]")
					}
				}))
				defer srv.Close()
				cfg := validConfig(srv.URL)
				cfg["handoff_state"] = "needs-human"
				if route == "search" {
					cfg["query_filter"] = "label:bug"
				}
				a := mustAdapter(t, cfg)

				issues, err := a.FetchCandidateIssues(context.Background())

				if err != nil {
					t.Fatalf("FetchCandidateIssues: %v", err)
				}
				if got := len(issues) == 1; got != tt.wantCandidate {
					t.Errorf("FetchCandidateIssues with labels %v returned %d issues, want candidate = %v", tt.labels, len(issues), tt.wantCandidate)
				}
			})
		}
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
		// issues.json carries no issue_dependencies_summary field, so
		// every candidate must be marked unresolved rather than read as
		// having no blockers.
		adaptertest.AssertCandidateBlockerSource(t, registry.BlockersPerIssue, iss, 0)
	}
}

func TestFetchCandidateIssues_NonNilEmptySlice(t *testing.T) {
	t.Parallel()

	// Empty response -> non-nil empty slice, not nil.
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

func serveLabelCatalog(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != "/repos/owner/repo/labels" {
		return false
	}
	_, _ = io.WriteString(w, "[]")
	return true
}

func TestFetchCandidateIssues_Pagination(t *testing.T) {
	t.Parallel()

	page1 := loadFixture(t, "issues.json")       // issues 1, 3 active
	page2 := loadFixture(t, "issues_page2.json") // issue 5 (review)

	var srvURL string
	var callCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveLabelCatalog(w, r) {
			return
		}
		n := callCount.Add(1)
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
	if got := callCount.Load(); got != 2 {
		t.Errorf("call count = %d, want 2", got)
	}
}

func TestFetchCandidateIssues_MaxPagesGuard(t *testing.T) {
	t.Parallel()

	// Server always returns a Link header, forcing the guard to trigger.
	var callCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveLabelCatalog(w, r) {
			return
		}
		n := callCount.Add(1)
		nextURL := fmt.Sprintf("http://%s/repos/owner/repo/issues?p=%d", r.Host, n+1)
		w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, nextURL))
		w.WriteHeader(http.StatusOK)
		// Return one active issue per page.
		w.Write([]byte(`[{"id":` + fmt.Sprint(n) + `,"number":` + fmt.Sprint(n) + `,"title":"T","body":null,"state":"open","html_url":"u","labels":[{"name":"backlog"}],"assignees":[],"type":null,"pull_request":null,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]`)) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	issues, err := a.FetchCandidateIssues(waittest.Context(t))
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}

	got := int(callCount.Load())
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

	// Blockers from blockers.json: one blocker (5).
	if len(issue.BlockedBy) != 1 {
		t.Fatalf("BlockedBy len = %d, want 1", len(issue.BlockedBy))
	}
	if issue.BlockedBy[0].Identifier != "5" {
		t.Errorf("BlockedBy[0].Identifier = %q, want 5", issue.BlockedBy[0].Identifier)
	}

	// Parent from parent.json: 7.
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
	adaptertest.AssertIssueNormalized(t, issue)
	adaptertest.AssertCommentsAscending(t, issue.Comments)
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

	if te, ok := errors.AsType[*domain.TrackerError](err); ok {
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

// TestFetchIssueByID_BlockerNotFound_Propagates pins that a 404 on
// the dependencies route is not a signal that the issue has no
// blockers. It
// must surface as a *domain.TrackerError of kind ErrTrackerNotFound
// rather than degrade to a non-nil empty BlockedBy, because a caller
// that cannot tell "no blockers" from "could not read blockers" would
// dispatch an issue whose blocker state is actually unknown.
func TestFetchIssueByID_BlockerNotFound_Propagates(t *testing.T) {
	t.Parallel()

	issueFix := loadFixture(t, "issue.json")

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/issues/42/dependencies/blocked_by", func(w http.ResponseWriter, r *http.Request) {
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

// TestFetchIssueByID_ParentNotFound_Degrades pins that a 404 on the
// parent route is unaffected by the blocker-completeness change: it
// still degrades to a nil Parent rather than an error.
func TestFetchIssueByID_ParentNotFound_Degrades(t *testing.T) {
	t.Parallel()

	issueFix := loadFixture(t, "issue.json")
	commentsFix := loadFixture(t, "comments.json")

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/issues/42/dependencies/blocked_by", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("[]")) //nolint:errcheck // test helper
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

	if issue.BlockersUnresolved {
		t.Error("BlockersUnresolved = true, want false: the blocker read succeeded")
	}
	if issue.BlockedBy == nil {
		t.Error("BlockedBy is nil, want non-nil empty slice")
	}
	if len(issue.BlockedBy) != 0 {
		t.Errorf("BlockedBy len = %d, want 0", len(issue.BlockedBy))
	}

	// 404 on parent -> nil.
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

func TestFetchIssuesByStates_EmptyInput(t *testing.T) {
	t.Parallel()

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
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
	adaptertest.AssertNoRequestOnEmptyInput(t, calls, "FetchIssuesByStates")
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
	// "backlog" is an active state; should use issues endpoint.
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

	// Only backlog issue (1) should be in result.
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
	// "done" is a terminal state; must use search endpoint.
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
	// Request both active ("backlog") and terminal ("done"); issue 1 matches both.
	issues, err := a.FetchIssuesByStates(context.Background(), []string{"backlog", "done"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates: %v", err)
	}

	if len(issues) != 1 {
		t.Fatalf("len = %d, want 1 (deduplication)", len(issues))
	}
}

func TestFetchIssueStatesByIDs_Success(t *testing.T) {
	t.Parallel()

	issueData := issueJSON(42, "in-progress")

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

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/issues/1") {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(issueJSON(1, "backlog"))) //nolint:errcheck // test helper
			return
		}
		w.WriteHeader(http.StatusNotFound)
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
	adaptertest.AssertStateMapOmitsMissing(t, []string{"1", "999"}, result)
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

func TestFetchIssueStatesByIdentifiers_Success(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(issueJSON(7, "review"))) //nolint:errcheck // test helper
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

func TestFetchIssueComments_SinglePage(t *testing.T) {
	t.Parallel()

	fixture := loadFixture(t, "comments.json")
	var requests requestLog
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.add(r)
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
	adaptertest.AssertCommentsAscending(t, comments)

	wantRequests := []string{`GET /repos/owner/repo/issues/42/comments?per_page=50 if-none-match=""`}
	if got := requests.all(); !slices.Equal(got, wantRequests) {
		t.Errorf("FetchIssueComments requests = %q, want %q", got, wantRequests)
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
	adaptertest.AssertEmptyNonNil(t, comments, "FetchIssueComments")
}

func TestFetchIssueComments_Pagination(t *testing.T) {
	t.Parallel()

	page1 := loadFixture(t, "comments.json") // 2 comments
	page2 := `[{"id":9999,"user":{"login":"charlie"},"body":"Third comment.","created_at":"2026-01-17T09:00:00Z"}]`

	var srvURL string
	var callCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
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

type requestLog struct {
	mu   sync.Mutex
	list []string
}

func (l *requestLog) add(r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.list = append(l.list, fmt.Sprintf("%s %s if-none-match=%q", r.Method, r.URL.RequestURI(), r.Header.Get("If-None-Match")))
}

func (l *requestLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.list)
}

type commentPageReply struct {
	status int
	etag   string
	body   []byte
}

func TestFetchIssueCommentChanges(t *testing.T) {
	t.Parallel()

	page := loadFixture(t, "comment_changes_page.json")
	nextPage := loadFixture(t, "comment_changes_next_page.json")
	pageComments := []domain.Comment{
		{ID: "9001", Author: "alice", AuthorID: "alice", AuthorKind: domain.CommentAuthorPerson, Body: "Looks good, please proceed.", CreatedAt: "2026-01-15T10:00:00Z", UpdatedAt: "2026-01-15T10:00:00Z"},
		{ID: "9002", Author: "bob", AuthorID: "bob", AuthorKind: domain.CommentAuthorPerson, Body: "Please fix the indentation, then rebase.", CreatedAt: "2026-01-16T11:30:00Z", UpdatedAt: "2026-01-17T08:15:00Z"},
		{ID: "9003", Author: "github-actions[bot]", AuthorID: "github-actions[bot]", AuthorKind: domain.CommentAuthorBot, Body: "CI finished: all checks passed.", CreatedAt: "2026-01-17T09:00:00Z", UpdatedAt: "2026-01-17T09:00:00Z"},
	}

	const (
		page1Request  = `GET /repos/owner/repo/issues/42/comments?page=1&per_page=100 if-none-match=""`
		page1Validate = `GET /repos/owner/repo/issues/42/comments?page=1&per_page=100 if-none-match="W/\"v1\""`
		page2Request  = `GET /repos/owner/repo/issues/42/comments?page=2&per_page=100 if-none-match=""`
		page2Validate = `GET /repos/owner/repo/issues/42/comments?page=2&per_page=100 if-none-match="W/\"v2\""`
		onePageV1     = `[{"etag":"W/\"v1\"","count":2}]`
		fullPageV1    = `[{"etag":"W/\"v1\"","count":100}]`
		fullThenOne   = `[{"etag":"W/\"v1\"","count":100},{"etag":"W/\"v2\"","count":1}]`
	)

	tests := []struct {
		name         string
		since        domain.CommentReadPoint
		cacheOff     bool
		replies      map[string]commentPageReply
		wantRequests []string
		wantComments []domain.Comment
		wantNext     domain.CommentReadPoint
		wantErr      domain.TrackerErrorKind
	}{
		{
			name:         "new and edited comments come back with author fields and the new body",
			since:        onePageV1,
			replies:      map[string]commentPageReply{"1": {http.StatusOK, `W/"v2"`, page}},
			wantRequests: []string{page1Validate},
			wantComments: pageComments,
			wantNext:     `[{"etag":"W/\"v2\"","count":3}]`,
		},
		{
			name:  "every page not modified returns nothing and keeps the point",
			since: fullThenOne,
			replies: map[string]commentPageReply{
				"1": {status: http.StatusNotModified},
				"2": {status: http.StatusNotModified},
			},
			wantRequests: []string{page1Validate, page2Validate},
			wantComments: []domain.Comment{},
			wantNext:     fullThenOne,
		},
		{
			name:  "a full page that answers not modified is followed by an unconditional next page",
			since: fullPageV1,
			replies: map[string]commentPageReply{
				"1": {status: http.StatusNotModified},
				"2": {http.StatusOK, `W/"v2"`, nextPage},
			},
			wantRequests: []string{page1Validate, page2Request},
			wantComments: []domain.Comment{
				{ID: "9101", Author: "carol", AuthorID: "carol", AuthorKind: domain.CommentAuthorPerson, Body: "The 101st comment.", CreatedAt: "2026-01-18T12:00:00Z", UpdatedAt: "2026-01-18T12:00:00Z"},
			},
			wantNext: fullThenOne,
		},
		{
			name:         "undecodable point reads page one unconditionally",
			since:        "x",
			replies:      map[string]commentPageReply{"1": {http.StatusOK, `W/"v1"`, page}},
			wantRequests: []string{page1Request},
			wantComments: pageComments,
			wantNext:     `[{"etag":"W/\"v1\"","count":3}]`,
		},
		{
			name:         "disabled conditional requests send no validator and return no point",
			since:        onePageV1,
			cacheOff:     true,
			replies:      map[string]commentPageReply{"1": {http.StatusOK, `W/"v2"`, page}},
			wantRequests: []string{page1Request},
			wantComments: pageComments,
		},
		{
			name:         "body that is not an array is a payload error",
			replies:      map[string]commentPageReply{"1": {http.StatusOK, `W/"v1"`, []byte(`{"message":"unexpected"}`)}},
			wantRequests: []string{page1Request},
			wantErr:      domain.ErrTrackerPayload,
		},
		{
			name:         "missing issue is not found",
			replies:      map[string]commentPageReply{"1": {status: http.StatusNotFound}},
			wantRequests: []string{page1Request},
			wantErr:      domain.ErrTrackerNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var requests requestLog
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.add(r)
				reply, ok := tt.replies[r.URL.Query().Get("page")]
				if !ok {
					reply.status = http.StatusInternalServerError
				}
				if reply.etag != "" {
					w.Header().Set("ETag", reply.etag)
				}
				w.WriteHeader(reply.status)
				w.Write(reply.body) //nolint:errcheck // test helper
			}))
			defer srv.Close()
			cfg := validConfig(srv.URL)
			if tt.cacheOff {
				cfg["etag_cache_size"] = 0
			}

			got, err := mustAdapter(t, cfg).FetchIssueCommentChanges(context.Background(), "42", tt.since)

			if gotRequests := requests.all(); !slices.Equal(gotRequests, tt.wantRequests) {
				t.Errorf("FetchIssueCommentChanges(%q) requests = %q, want %q", tt.since, gotRequests, tt.wantRequests)
			}
			if tt.wantErr != "" {
				assertTrackerErrorKind(t, err, tt.wantErr)
				if got.Comments != nil || got.Next != "" {
					t.Errorf("FetchIssueCommentChanges(%q) on error = %+v, want zero value", tt.since, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("FetchIssueCommentChanges(%q): %v", tt.since, err)
			}
			if got.Comments == nil || !slices.Equal(got.Comments, tt.wantComments) {
				t.Errorf("FetchIssueCommentChanges(%q).Comments = %+v, want %+v", tt.since, got.Comments, tt.wantComments)
			}
			if got.Next != tt.wantNext {
				t.Errorf("FetchIssueCommentChanges(%q).Next = %q, want %q", tt.since, got.Next, tt.wantNext)
			}
			adaptertest.AssertCommentsAscending(t, got.Comments)
		})
	}
}

const labelFakeNumber = 42

type labelRequest struct {
	method string
	path   string
}

type issueLabelFake struct {
	mu           sync.Mutex
	labels       []string
	pageSize     int
	deleteStatus int
	applyDelete  bool
	labelsStatus int
	postStatus   int
	closed       bool
	writes       []string
	requests     []labelRequest
	srv          *httptest.Server
}

func newIssueLabelFake(t *testing.T, labels ...string) *issueLabelFake {
	t.Helper()

	fake := &issueLabelFake{labels: labels}
	fake.srv = httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(fake.srv.Close)
	return fake
}

func (f *issueLabelFake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, labelRequest{method: r.Method, path: r.URL.EscapedPath()})

	base := fmt.Sprintf("/repos/owner/repo/issues/%d", labelFakeNumber)
	path := r.URL.Path
	switch {
	case path == base && r.Method == http.MethodGet:
		nativeState := "open"
		if f.closed {
			nativeState = "closed"
		}
		body, _ := json.Marshal(map[string]any{
			"id":         labelFakeNumber * 100,
			"number":     labelFakeNumber,
			"title":      "Label fake issue",
			"state":      nativeState,
			"html_url":   "https://github.com/owner/repo/issues/42",
			"labels":     f.labelNodes(f.labels),
			"assignees":  []any{},
			"created_at": "2026-01-01T00:00:00Z",
			"updated_at": "2026-01-01T00:00:00Z",
		})
		w.Write(body) //nolint:errcheck // test helper
	case path == base+"/labels" && r.Method == http.MethodGet:
		f.serveLabelList(w, r)
	case path == base && r.Method == http.MethodPatch:
		f.servePatch(w, r)
	case path == base+"/labels" && r.Method == http.MethodPost:
		var req struct {
			Labels []string `json:"labels"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &req)
		f.writes = append(f.writes, "add "+strings.Join(req.Labels, ","))
		if f.postStatus != 0 {
			w.WriteHeader(f.postStatus)
			return
		}
		f.labels = append(f.labels, req.Labels...)
		w.Write(f.labelList(f.labels)) //nolint:errcheck // test helper
	case strings.HasPrefix(path, base+"/labels/") && r.Method == http.MethodDelete:
		f.serveDelete(w, strings.TrimPrefix(path, base+"/labels/"))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *issueLabelFake) servePatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		State       string `json:"state"`
		StateReason string `json:"state_reason"`
	}
	raw, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(raw, &req)
	if req.State == "open" {
		f.writes = append(f.writes, "reopen")
	} else {
		f.writes = append(f.writes, "close "+req.StateReason)
	}
	f.closed = req.State == "closed"
	w.Write([]byte(`{}`)) //nolint:errcheck // test helper
}

func (f *issueLabelFake) labelNodes(labels []string) []map[string]string {
	nodes := make([]map[string]string, len(labels))
	for i, name := range labels {
		nodes[i] = map[string]string{"name": name}
	}
	return nodes
}

func (f *issueLabelFake) labelList(labels []string) []byte {
	body, _ := json.Marshal(f.labelNodes(labels))
	return body
}

func (f *issueLabelFake) serveLabelList(w http.ResponseWriter, r *http.Request) {
	if f.labelsStatus != 0 {
		w.WriteHeader(f.labelsStatus)
		return
	}
	if f.pageSize == 0 || f.pageSize >= len(f.labels) {
		w.Write(f.labelList(f.labels)) //nolint:errcheck // test helper
		return
	}

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(page, 1)
	start := (page - 1) * f.pageSize
	end := min(start+f.pageSize, len(f.labels))
	if end < len(f.labels) {
		next := fmt.Sprintf("%s%s?per_page=100&page=%d", f.srv.URL, r.URL.Path, page+1)
		w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, next))
	}
	w.Write(f.labelList(f.labels[start:end])) //nolint:errcheck // test helper
}

func (f *issueLabelFake) serveDelete(w http.ResponseWriter, escaped string) {
	name, err := url.PathUnescape(escaped)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.writes = append(f.writes, "remove "+name)
	remove := func() {
		f.labels = slices.DeleteFunc(f.labels, func(l string) bool { return l == name })
	}

	if f.deleteStatus != 0 {
		if f.applyDelete {
			remove()
		}
		w.WriteHeader(f.deleteStatus)
		return
	}
	if !slices.Contains(f.labels, name) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	remove()
	w.Write(f.labelList(f.labels)) //nolint:errcheck // test helper
}

func (f *issueLabelFake) stored() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.labels)
}

func (f *issueLabelFake) state() (writes, labels []string, closed bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.writes), slices.Clone(f.labels), f.closed
}

func (f *issueLabelFake) requestLog() []labelRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

func (f *issueLabelFake) paths(method string) []string {
	var paths []string
	for _, r := range f.requestLog() {
		if r.method == method {
			paths = append(paths, r.path)
		}
	}
	return paths
}

func TestAddLabel_Additive(t *testing.T) {
	t.Parallel()

	var receivedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[{"name":"urgent"}]`)) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	if err := a.AddLabel(context.Background(), "42", "urgent"); err != nil {
		t.Fatalf("AddLabel: %v", err)
	}

	var body struct {
		Labels []string `json:"labels"`
	}
	if err := json.Unmarshal(receivedBody, &body); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	if !slices.Equal(body.Labels, []string{"urgent"}) {
		t.Fatalf("request labels = %v, want [urgent]", body.Labels)
	}

	// The GitHub Labels API's POST route adds without removing, so the
	// request carrying only the new label still leaves pre-existing labels
	// in place.
	before := []string{"existing"}
	after := append(slices.Clone(before), body.Labels[0])
	adaptertest.AssertLabelAddIsAdditive(t, before, after, body.Labels[0])
}

func TestRemoveLabel_DeletesEachStoredSpellingFromEveryPage(t *testing.T) {
	t.Parallel()

	fake := newIssueLabelFake(t, "backlog", "Needs Human", "ui", "needs human")
	fake.pageSize = 2
	a := mustAdapter(t, validConfig(fake.srv.URL))

	err := a.RemoveLabel(context.Background(), "42", "NEEDS HUMAN")

	if err != nil {
		t.Fatalf("RemoveLabel = %v, want nil", err)
	}
	wantPaths := []string{
		"/repos/owner/repo/issues/42/labels/Needs%20Human",
		"/repos/owner/repo/issues/42/labels/needs%20human",
	}
	if got := fake.paths(http.MethodDelete); !slices.Equal(got, wantPaths) {
		t.Errorf("DELETE paths = %v, want %v", got, wantPaths)
	}
	if got := fake.stored(); !slices.Equal(got, []string{"backlog", "ui"}) {
		t.Errorf("stored labels = %v, want [backlog ui]", got)
	}
}

func TestRemoveLabel_RejectedDeleteIsSettledByRead(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		deleteStatus int
		labelsStatus int
		applied      bool
		wantKind     domain.TrackerErrorKind
	}{
		{"not found but label gone", http.StatusNotFound, 0, true, ""},
		{"validation failed but label gone", http.StatusUnprocessableEntity, 0, true, ""},
		{"not found and label kept", http.StatusNotFound, 0, false, domain.ErrTrackerNotFound},
		{"label read not found", 0, http.StatusNotFound, false, domain.ErrTrackerNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := newIssueLabelFake(t, "backlog", "urgent")
			fake.deleteStatus = tt.deleteStatus
			fake.applyDelete = tt.applied
			fake.labelsStatus = tt.labelsStatus
			a := mustAdapter(t, validConfig(fake.srv.URL))

			err := a.RemoveLabel(context.Background(), "42", "urgent")

			if tt.wantKind == "" {
				if err != nil {
					t.Fatalf("RemoveLabel = %v, want nil", err)
				}
				return
			}
			assertTrackerErrorKind(t, err, tt.wantKind)
		})
	}
}

func TestTransitionIssue_RemovesStateLabelSpellingsAndStillAddsTarget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		labels       []string
		deleteStatus int
		wantDeleted  []string
	}{
		{"every case variant", []string{"In-Progress", "in-progress", "UI"}, 0, []string{
			"/repos/owner/repo/issues/42/labels/In-Progress",
			"/repos/owner/repo/issues/42/labels/in-progress",
		}},
		{"state label already gone", []string{"in-progress"}, http.StatusNotFound, []string{
			"/repos/owner/repo/issues/42/labels/in-progress",
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := newIssueLabelFake(t, tt.labels...)
			fake.deleteStatus = tt.deleteStatus
			fake.applyDelete = true
			a := mustAdapter(t, validConfig(fake.srv.URL))

			err := a.TransitionIssue(context.Background(), "42", "review")

			if err != nil {
				t.Fatalf("TransitionIssue = %v, want nil", err)
			}
			if got := fake.paths(http.MethodDelete); !slices.Equal(got, tt.wantDeleted) {
				t.Errorf("DELETE paths = %v, want %v", got, tt.wantDeleted)
			}
			if got := fake.paths(http.MethodPost); len(got) != 1 {
				t.Errorf("POST requests = %d, want 1 (the target label is still added)", len(got))
			}
		})
	}
}

func TestTransitionIssue_WriteOrder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		labels     []string
		closed     bool
		target     string
		postStatus int
		wantKind   domain.TrackerErrorKind
		wantWrites []string
		wantLabels []string
		wantClosed bool
	}{
		{name: "active swap adds the target before removing the old label", labels: []string{"backlog"}, target: "review", wantWrites: []string{"add review", "remove backlog"}, wantLabels: []string{"review"}},
		{name: "the label that decides the start state is removed last", labels: []string{"backlog", "review"}, target: "in-progress", wantWrites: []string{"add in-progress", "remove review", "remove backlog"}, wantLabels: []string{"in-progress"}},
		{name: "terminal target closes last", labels: []string{"in-progress"}, target: "done", wantWrites: []string{"add done", "remove in-progress", "close completed"}, wantLabels: []string{"done"}, wantClosed: true},
		{name: "terminal label reopens first", labels: []string{"done"}, closed: true, target: "review", wantWrites: []string{"reopen", "add review", "remove done"}, wantLabels: []string{"review"}},
		{name: "target already carried writes nothing", labels: []string{"review"}, target: "review", wantLabels: []string{"review"}},
		{name: "failed add keeps the old label", labels: []string{"backlog"}, target: "review", postStatus: http.StatusInternalServerError, wantKind: domain.ErrTrackerTransport, wantWrites: []string{"add review"}, wantLabels: []string{"backlog"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := newIssueLabelFake(t, tt.labels...)
			fake.closed = tt.closed
			fake.postStatus = tt.postStatus
			a := mustAdapter(t, validConfig(fake.srv.URL))

			err := a.TransitionIssue(context.Background(), "42", tt.target)

			if tt.wantKind != "" {
				assertTrackerErrorKind(t, err, tt.wantKind)
			} else if err != nil {
				t.Fatalf("TransitionIssue(%q) = %v, want nil", tt.target, err)
			}
			writes, labels, closed := fake.state()
			if !slices.Equal(writes, tt.wantWrites) {
				t.Errorf("TransitionIssue(%q) writes = %v, want %v", tt.target, writes, tt.wantWrites)
			}
			if !slices.Equal(labels, tt.wantLabels) || closed != tt.wantClosed {
				t.Errorf("TransitionIssue(%q) left labels %v closed %v, want %v closed %v", tt.target, labels, closed, tt.wantLabels, tt.wantClosed)
			}
		})
	}
}

func TestCommentIssue_Success(t *testing.T) {
	t.Parallel()

	const created = `{"id":1234567890123,"node_id":"IC_kwDOA1b2c4wAAAGF","user":{"login":"sortie-bot"},"body":"Hello from Sortie!","created_at":"2026-08-04T12:00:00Z"}`
	const listed = `[{"id":4001,"node_id":"IC_kwDOA1b2c4wAAAAB","user":{"login":"alice"},"body":"Looks good.","created_at":"2026-01-15T10:00:00Z"},` + created + `]`

	var gotBody map[string]string
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method == http.MethodGet {
			w.Write([]byte(listed)) //nolint:errcheck // test helper
			return
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody) //nolint:errcheck // test helper
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(created)) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	id, err := a.CommentIssue(context.Background(), "42", "Hello from Sortie!")
	if err != nil {
		t.Fatalf("CommentIssue: %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("request count during CommentIssue = %d, want 1", got)
	}
	if gotBody["body"] != "Hello from Sortie!" {
		t.Errorf("request body.body = %q, want %q", gotBody["body"], "Hello from Sortie!")
	}

	comments, err := a.FetchIssueComments(context.Background(), "42")
	if err != nil {
		t.Fatalf("FetchIssueComments: %v", err)
	}
	adaptertest.AssertPostedCommentListed(t, id, comments)
	if id != "1234567890123" {
		t.Errorf("CommentIssue identity = %q, want the decimal id %q", id, "1234567890123")
	}
}

func TestCommentIssue_Error(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
		body   string
		want   domain.TrackerErrorKind
	}{
		{name: "404 maps to not found", status: http.StatusNotFound, want: domain.ErrTrackerNotFound},
		{name: "201 with only a node_id maps to payload error", status: http.StatusCreated, body: `{"node_id":"IC_kwDOA1b2c4wAAAGF","body":"comment"}`, want: domain.ErrTrackerPayload},
		{name: "201 with malformed JSON maps to payload error", status: http.StatusCreated, body: `{"id":`, want: domain.ErrTrackerPayload},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				w.Write([]byte(tt.body)) //nolint:errcheck // test helper
			}))
			defer srv.Close()

			a := mustAdapter(t, validConfig(srv.URL))
			id, err := a.CommentIssue(context.Background(), "999", "comment")
			assertTrackerErrorKind(t, err, tt.want)
			if id != "" {
				t.Errorf("CommentIssue identity = %q, want empty on error", id)
			}
		})
	}
}

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

	found := slices.Contains(spy.calls, "fetch_candidates:success")
	if !found {
		t.Errorf("metrics calls = %v, want fetch_candidates:success recorded", spy.calls)
	}
}

func TestSetMetrics_RecordsAddLabelOperations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		statusCode int
		wantErr    bool
		wantCall   string
	}{
		{"success", http.StatusOK, false, "add_label:success"},
		{"error", http.StatusInternalServerError, true, "add_label:error"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
				w.Write([]byte(`[{"name":"urgent"}]`)) //nolint:errcheck // test helper
			}))
			defer srv.Close()

			spy := &spyMetrics{}
			a := mustAdapter(t, validConfig(srv.URL))
			a.SetMetrics(spy)

			err := a.AddLabel(context.Background(), "42", "urgent")
			if tt.wantErr && err == nil {
				t.Fatal("AddLabel: expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("AddLabel: %v", err)
			}

			found := slices.Contains(spy.calls, tt.wantCall)
			if !found {
				t.Errorf("metrics calls = %v, want %s recorded", spy.calls, tt.wantCall)
			}
		})
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

	// Adapter with no SetMetrics call; metrics field is nil.
	a := mustAdapter(t, validConfig(srv.URL))
	// Must not panic.
	_, err := a.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
}

func TestFetchCandidateIssues_SearchPagination(t *testing.T) {
	t.Parallel()

	// Two-page search result. query_filter non-empty routes through fetchCandidatesViaSearch.
	page1 := `{"total_count":2,"incomplete_results":false,"items":[{"id":10,"number":10,"title":"T10","body":null,"state":"open","html_url":"u","labels":[{"name":"review"}],"assignees":[],"type":null,"pull_request":null,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]}`
	page2 := `{"total_count":2,"incomplete_results":false,"items":[{"id":20,"number":20,"title":"T20","body":null,"state":"open","html_url":"u","labels":[{"name":"backlog"}],"assignees":[],"type":null,"pull_request":null,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]}`

	var srvURL string
	var callCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveLabelCatalog(w, r) {
			return
		}
		n := callCount.Add(1)
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

	// blockers endpoint returns 500 (not 404) -> error propagated.
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

	// parent endpoint returns 500 (not 404) -> error propagated.
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

func TestFetchIssuesByStates_OpenPagination(t *testing.T) {
	t.Parallel()

	// Server returns page1 (issues.json) with a Link header; second call returns page2 (issues_page2.json).
	page1 := loadFixture(t, "issues.json")
	page2 := loadFixture(t, "issues_page2.json")

	var srvURL string
	var callCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
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
	// issues.json: 1 backlog + 3 in-progress (2 non-PR active issues, 4 done is filtered out).
	// issues_page2.json: 5 review (1 issue).
	if len(issues) != 3 {
		t.Errorf("len = %d, want 3 across 2 pages", len(issues))
	}
}

func TestFetchIssuesByStates_ClosedSearchPagination(t *testing.T) {
	t.Parallel()

	page1 := `{"total_count":2,"incomplete_results":false,"items":[{"id":100,"number":100,"title":"D1","body":null,"state":"closed","html_url":"u","labels":[{"name":"done"}],"assignees":[],"type":null,"pull_request":null,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]}`
	page2 := `{"total_count":2,"incomplete_results":false,"items":[{"id":200,"number":200,"title":"D2","body":null,"state":"closed","html_url":"u","labels":[{"name":"done"}],"assignees":[],"type":null,"pull_request":null,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]}`

	var srvURL string
	var callCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
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
	var callCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
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

func TestFetchIssueStatesByIDs_SkipsPullRequest(t *testing.T) {
	t.Parallel()

	// API returns a PR for the requested identifier -> it must be omitted from the result map.
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

func TestFetchIssueComments_NonNotFoundError(t *testing.T) {
	t.Parallel()

	// 500 from comments endpoint -> ErrTrackerTransport (not wrapped as NotFound).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	_, err := a.FetchIssueComments(context.Background(), "42")
	assertTrackerErrorKind(t, err, domain.ErrTrackerTransport)
}

func TestTransitionIssue_GetIssueError(t *testing.T) {
	t.Parallel()

	// GET issue returns 500 -> error propagated; not a NotFound.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	err := a.TransitionIssue(context.Background(), "42", "review")
	assertTrackerErrorKind(t, err, domain.ErrTrackerTransport)
}

func TestNewGitHubAdapter_ETagCacheSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value any
		set   bool
		want  int
	}{
		{name: "absent uses the default", want: 1000},
		{name: "custom int", value: 50, set: true, want: 50},
		{name: "zero disables the cache", value: 0, set: true, want: 0},
		{name: "float64 from JSON decoding", value: float64(75), set: true, want: 75},
		{name: "non-numeric falls back to the default", value: "not a number", set: true, want: 1000},
		{name: "negative falls back to the default", value: -5, set: true, want: 1000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := validConfig("http://localhost")
			if tt.set {
				cfg["etag_cache_size"] = tt.value
			}

			a := mustAdapter(t, cfg)

			if a.etagCache.maxSize != tt.want {
				t.Errorf("etagCache.maxSize for etag_cache_size=%v = %d, want %d", tt.value, a.etagCache.maxSize, tt.want)
			}
		})
	}
}

func TestFetchIssueStatesByIDs_ConditionalRequest_304(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		if n == 1 {
			w.Header().Set("ETag", `"etag-v1"`)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(issueJSON(42, "in-progress"))) //nolint:errcheck // test helper
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
	if n := callCount.Load(); n != 2 {
		t.Errorf("server call count = %d, want 2", n)
	}
}

func TestFetchIssueStatesByIDs_304_DerivedUnderReloadedStates(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = io.WriteString(w, `{"number":1,"state":"closed","labels":[]}`)
	}))
	defer srv.Close()
	a := mustAdapter(t, validConfig(srv.URL))
	if _, err := a.FetchIssueStatesByIDs(context.Background(), []string{"1"}); err != nil {
		t.Fatalf("first FetchIssueStatesByIDs: %v", err)
	}
	a.SetTrackerStates(func() domain.TrackerStates {
		return domain.TrackerStates{Active: []string{"backlog"}, Terminal: []string{"shipped", "done"}}
	})

	got, err := a.FetchIssueStatesByIDs(context.Background(), []string{"1"})

	if err != nil {
		t.Fatalf("second FetchIssueStatesByIDs: %v", err)
	}
	if got["1"] != "shipped" {
		t.Errorf("FetchIssueStatesByIDs([1]) after a 304 and a reload = %q, want %q", got["1"], "shipped")
	}
}

func TestFetchIssueStatesAndLabelsByIDs_304_KeepsFirstReadLabels(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("ETag", `"v1"`)
			_, _ = io.WriteString(w, issueJSON(42, "Parked"))
			return
		}
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = io.WriteString(w, `{"number":42,"state":"open","labels":[]}`)
	}))
	defer srv.Close()
	a := mustAdapter(t, validConfig(srv.URL))
	if _, err := a.FetchIssueStatesAndLabelsByIDs(context.Background(), []string{"42"}); err != nil {
		t.Fatalf("first FetchIssueStatesAndLabelsByIDs: %v", err)
	}

	got, err := a.FetchIssueStatesAndLabelsByIDs(context.Background(), []string{"42"})

	if err != nil {
		t.Fatalf("second FetchIssueStatesAndLabelsByIDs: %v", err)
	}
	if want := []string{"parked"}; !slices.Equal(got["42"].Labels, want) {
		t.Errorf("FetchIssueStatesAndLabelsByIDs([42]) labels after a 304 = %v, want %v", got["42"].Labels, want)
	}
}

func TestFetchIssueStatesByIDs_ConditionalRequest_200_UpdatesCache(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		switch n {
		case 1:
			w.Header().Set("ETag", `"etag-v1"`)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(issueJSON(5, "in-progress"))) //nolint:errcheck // test helper
		case 2:
			// State changed on server; new ETag and new state label.
			w.Header().Set("ETag", `"etag-v2"`)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(issueJSON(5, "review"))) //nolint:errcheck // test helper
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

	var callCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		// Server intentionally omits the ETag header.
		if h := r.Header.Get("If-None-Match"); h != "" {
			t.Errorf("call %d: unexpected If-None-Match = %q (no ETag was ever returned)", n, h)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(issueJSON(10, "backlog"))) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	cfg := validConfig(srv.URL)
	cfg["etag_cache_size"] = 100
	a := mustAdapter(t, cfg)

	// Both calls must be unconditional because the server never returns an ETag.
	for i := range 2 {
		if _, err := a.FetchIssueStatesByIDs(context.Background(), []string{"10"}); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
	if n := callCount.Load(); n != 2 {
		t.Errorf("server call count = %d, want 2 (no caching when ETag absent)", n)
	}
}

func TestFetchIssueStatesByIDs_304_CachedStateUsedAfterEviction(t *testing.T) {
	t.Parallel()

	// Issue 1's server handler blocks until issue 2 has been processed and has
	// evicted issue 1 from the size-1 cache. The 304 for issue 1 must still
	// return the correct state via the local variable captured before the HTTP
	// call; not via a re-lookup of the (now-evicted) cache entry.
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
			w.Write([]byte(issueJSON(2, "review"))) //nolint:errcheck // test helper
		}
	}))
	defer srv.Close()

	cfg := validConfig(srv.URL)
	cfg["etag_cache_size"] = 1
	a := mustAdapter(t, cfg)

	// Pre-populate the cache for issue 1 so lookup returns a hit before the HTTP call.
	a.etagCache.put("/repos/owner/repo/issues/1", `"etag-1"`, []string{"in-progress"}, false)

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
	if _, _, _, ok := a.etagCache.lookup("/repos/owner/repo/issues/1"); ok {
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

	var callCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		if h := r.Header.Get("If-None-Match"); h != "" {
			t.Errorf("call %d: unexpected If-None-Match = %q (cache disabled)", n, h)
		}
		w.Header().Set("ETag", `"etag-v1"`)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(issueJSON(20, "backlog"))) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	cfg := validConfig(srv.URL)
	cfg["etag_cache_size"] = 0 // caching disabled
	a := mustAdapter(t, cfg)

	for i := range 2 {
		if _, err := a.FetchIssueStatesByIDs(context.Background(), []string{"20"}); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
	if n := callCount.Load(); n != 2 {
		t.Errorf("server call count = %d, want 2 (cache disabled, all requests unconditional)", n)
	}
}

func TestFetchIssueStatesByIDs_NetworkError_CachePreserved(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		switch n {
		case 1:
			w.Header().Set("ETag", `"etag-v1"`)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(issueJSON(30, "backlog"))) //nolint:errcheck // test helper
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

	// Call 1: success, populates cache.
	if _, err := a.FetchIssueStatesByIDs(context.Background(), []string{"30"}); err != nil {
		t.Fatalf("first call: %v", err)
	}

	// Call 2: server error, must not evict the cache entry.
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
	issue1Body := issueJSON(1, "backlog")
	issue2PRBody := `{"id":200,"number":2,"title":"A PR","body":null,"state":"open","html_url":"https://github.com/owner/repo/pull/2","labels":[{"name":"in-progress"}],"assignees":[],"type":null,"pull_request":{},"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}`
	issue3Body := issueJSON(3, "done")
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
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(emptyList)) //nolint:errcheck // test helper
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
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(emptyList)) //nolint:errcheck // test helper
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
	// Only issue 1 (backlog) appears; PR (2) filtered, done (3) non-active.
	if len(candidates) != 1 {
		t.Fatalf("FetchCandidateIssues: got %d issues, want 1 (only backlog)", len(candidates))
	}
	if candidates[0].ID != "1" {
		t.Errorf("candidates[0].ID = %q, want 1", candidates[0].ID)
	}

	activeSet := make(map[string]bool)
	for _, s := range a.model.Snapshot().Active {
		activeSet[s] = true
	}

	// Active issue: FetchIssueByID succeeds and local check accepts.
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

	// PR: FetchIssueByID returns ErrTrackerNotFound.
	_, err = a.FetchIssueByID(ctx, "2")
	assertTrackerErrorKind(t, err, domain.ErrTrackerNotFound)

	// Non-active issue: FetchIssueByID succeeds but local check rejects.
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
}
