package jira

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

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

func validConfig(endpoint string) map[string]any {
	return map[string]any{
		"endpoint": endpoint,
		"api_key":  "user@test.com:api_token_123",
		"project":  "PROJ",
	}
}

func mustAdapter(t *testing.T, config map[string]any) *JiraAdapter {
	t.Helper()
	a, err := NewJiraAdapter(config)
	if err != nil {
		t.Fatalf("NewJiraAdapter: %v", err)
	}
	return a.(*JiraAdapter)
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

// --- Constructor tests ---

func TestNewJiraAdapter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		config   map[string]any
		wantErr  bool
		wantKind domain.TrackerErrorKind
	}{
		{
			name:   "valid config",
			config: validConfig("https://test.atlassian.net"),
		},
		{
			name:     "missing endpoint",
			config:   map[string]any{"api_key": "u@t.com:tok", "project": "P"},
			wantErr:  true,
			wantKind: domain.ErrTrackerPayload,
		},
		{
			name:     "empty endpoint",
			config:   map[string]any{"endpoint": "", "api_key": "u@t.com:tok", "project": "P"},
			wantErr:  true,
			wantKind: domain.ErrTrackerPayload,
		},
		{
			name:     "missing api_key",
			config:   map[string]any{"endpoint": "https://x.atlassian.net", "project": "P"},
			wantErr:  true,
			wantKind: domain.ErrMissingTrackerAPIKey,
		},
		{
			name:     "api_key without colon",
			config:   map[string]any{"endpoint": "https://x.atlassian.net", "api_key": "noatsign", "project": "P"},
			wantErr:  true,
			wantKind: domain.ErrTrackerAuth,
		},
		{
			name:     "api_key with only colon at start",
			config:   map[string]any{"endpoint": "https://x.atlassian.net", "api_key": ":token", "project": "P"},
			wantErr:  true,
			wantKind: domain.ErrTrackerAuth,
		},
		{
			name:     "api_key with trailing colon (empty token)",
			config:   map[string]any{"endpoint": "https://x.atlassian.net", "api_key": "email:", "project": "P"},
			wantErr:  true,
			wantKind: domain.ErrTrackerAuth,
		},
		{
			name:     "missing project",
			config:   map[string]any{"endpoint": "https://x.atlassian.net", "api_key": "u@t.com:tok"},
			wantErr:  true,
			wantKind: domain.ErrMissingTrackerProject,
		},
		{
			name:     "endpoint contains REST API path",
			config:   map[string]any{"endpoint": "https://x.atlassian.net/rest/api/3", "api_key": "u@t.com:tok", "project": "P"},
			wantErr:  true,
			wantKind: domain.ErrTrackerPayload,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			a, err := NewJiraAdapter(tt.config)
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

func TestNewJiraAdapter_DefaultActiveStates(t *testing.T) {
	t.Parallel()

	a := mustAdapter(t, validConfig("https://x.atlassian.net"))
	if len(a.activeStates) != 3 {
		t.Fatalf("activeStates len = %d, want 3", len(a.activeStates))
	}
	if a.activeStates[0] != "Backlog" {
		t.Errorf("activeStates[0] = %q, want Backlog", a.activeStates[0])
	}
}

func TestNewJiraAdapter_CustomActiveStates(t *testing.T) {
	t.Parallel()

	config := validConfig("https://x.atlassian.net")
	config["active_states"] = []any{"Open", "WIP"}
	a := mustAdapter(t, config)
	if len(a.activeStates) != 2 || a.activeStates[0] != "Open" || a.activeStates[1] != "WIP" {
		t.Errorf("activeStates = %v, want [Open WIP]", a.activeStates)
	}
}

func TestNewJiraAdapter_CustomActiveStates_StringSlice(t *testing.T) {
	t.Parallel()

	// Config layer passes typed []string; the adapter must accept it.
	config := validConfig("https://x.atlassian.net")
	config["active_states"] = []string{"To Do", "In Progress"}
	a := mustAdapter(t, config)
	if len(a.activeStates) != 2 || a.activeStates[0] != "To Do" || a.activeStates[1] != "In Progress" {
		t.Errorf("activeStates = %v, want [To Do In Progress]", a.activeStates)
	}
}

func TestExtractStringSlice(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input any
		want  []string
	}{
		{name: "nil", input: nil, want: nil},
		{name: "[]any strings", input: []any{"A", "B"}, want: []string{"A", "B"}},
		{name: "[]string", input: []string{"X", "Y"}, want: []string{"X", "Y"}},
		{name: "[]any mixed", input: []any{"ok", 42, "yes"}, want: []string{"ok", "yes"}},
		{name: "[]any empty", input: []any{}, want: []string{}},
		{name: "wrong type int", input: 42, want: nil},
		{name: "wrong type string", input: "single", want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := extractStringSlice(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("extractStringSlice(%v) len = %d, want %d", tt.input, len(got), len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("extractStringSlice(%v)[%d] = %q, want %q", tt.input, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestNewJiraAdapter_QueryFilter(t *testing.T) {
	t.Parallel()

	config := validConfig("https://x.atlassian.net")
	config["query_filter"] = "component = 'api'"
	a := mustAdapter(t, config)
	if a.queryFilter != "component = 'api'" {
		t.Errorf("queryFilter = %q, want %q", a.queryFilter, "component = 'api'")
	}
}

func TestNewJiraAdapter_QueryFilter_Absent(t *testing.T) {
	t.Parallel()

	a := mustAdapter(t, validConfig("https://x.atlassian.net"))
	if a.queryFilter != "" {
		t.Errorf("queryFilter = %q, want empty", a.queryFilter)
	}
}

func TestNewJiraAdapter_EndpointTrailingSlash(t *testing.T) {
	t.Parallel()

	config := validConfig("https://x.atlassian.net/")
	a := mustAdapter(t, config)
	if a.endpoint != "https://x.atlassian.net" {
		t.Errorf("endpoint = %q, want trailing slash stripped", a.endpoint)
	}
}

func TestRegistration(t *testing.T) {
	t.Parallel()

	ctor, err := registry.Trackers.Get("jira")
	if err != nil {
		t.Fatalf("registry.Trackers.Get(jira): %v", err)
	}
	if ctor == nil {
		t.Fatal("constructor is nil")
	}
}

// --- FetchCandidateIssues tests ---

func TestFetchCandidateIssues_SinglePage(t *testing.T) {
	t.Parallel()

	fixture := loadFixture(t, "search_single_page.json")
	var receivedJQL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedJQL = r.URL.Query().Get("jql")
		w.WriteHeader(http.StatusOK)
		w.Write(fixture) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	issues, err := a.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("len = %d, want 2", len(issues))
	}

	// Verify normalization on first issue
	if issues[0].Identifier != "PROJ-1" {
		t.Errorf("issues[0].Identifier = %q", issues[0].Identifier)
	}
	if issues[0].Labels[0] != "feature" || issues[0].Labels[1] != "auth" {
		t.Errorf("labels not lowercased: %v", issues[0].Labels)
	}
	if issues[0].Comments != nil {
		t.Error("Comments should be nil for search results")
	}

	// Verify second issue
	if issues[1].Identifier != "PROJ-2" {
		t.Errorf("issues[1].Identifier = %q", issues[1].Identifier)
	}
	if issues[1].Comments != nil {
		t.Error("Comments should be nil for search results")
	}

	// Verify JQL
	if !strings.Contains(receivedJQL, "ORDER BY priority ASC, created ASC") {
		t.Errorf("JQL missing ORDER BY: %q", receivedJQL)
	}
}

func TestFetchCandidateIssues_MultiPage(t *testing.T) {
	t.Parallel()

	page1 := loadFixture(t, "search_multi_page_1.json")
	page2 := loadFixture(t, "search_multi_page_2.json")

	var callCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callCount, 1)
		if n == 1 {
			w.Write(page1) //nolint:errcheck // test helper
		} else {
			if r.URL.Query().Get("nextPageToken") != "cursor_abc" {
				t.Errorf("expected nextPageToken=cursor_abc, got %q", r.URL.Query().Get("nextPageToken"))
			}
			w.Write(page2) //nolint:errcheck // test helper
		}
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	issues, err := a.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("len = %d, want 2 across 2 pages", len(issues))
	}
	if issues[0].Identifier != "PROJ-3" {
		t.Errorf("issues[0].Identifier = %q, want PROJ-3", issues[0].Identifier)
	}
	if issues[1].Identifier != "PROJ-4" {
		t.Errorf("issues[1].Identifier = %q, want PROJ-4", issues[1].Identifier)
	}
}

func TestFetchCandidateIssues_Empty(t *testing.T) {
	t.Parallel()

	fixture := loadFixture(t, "search_empty.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(fixture) //nolint:errcheck // test helper
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

func TestFetchCandidateIssues_ServerError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	_, err := a.FetchCandidateIssues(context.Background())
	assertTrackerErrorKind(t, err, domain.ErrTrackerTransport)
}

func TestFetchCandidateIssues_JQLSanitization(t *testing.T) {
	t.Parallel()

	var receivedJQL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedJQL = r.URL.Query().Get("jql")
		w.Write(loadFixture(t, "search_empty.json")) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	config := validConfig(srv.URL)
	config["active_states"] = []any{`To "Do`}
	a := mustAdapter(t, config)
	_, err := a.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	// Embedded " should be stripped
	if strings.Contains(receivedJQL, `To "Do`) {
		t.Errorf("JQL contains unsanitized state: %q", receivedJQL)
	}
	if !strings.Contains(receivedJQL, `"To Do"`) {
		t.Errorf("JQL missing sanitized state: %q", receivedJQL)
	}
}

func TestFetchCandidateIssues_StringSliceActiveStates(t *testing.T) {
	t.Parallel()

	// Regression: config layer passes []string; the adapter must use those
	// states (not fall back to defaults) in the JQL query.
	var receivedJQL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedJQL = r.URL.Query().Get("jql")
		w.Write(loadFixture(t, "search_empty.json")) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	config := validConfig(srv.URL)
	config["active_states"] = []string{"To Do", "Code Review"}
	a := mustAdapter(t, config)
	_, err := a.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	if !strings.Contains(receivedJQL, `"To Do"`) {
		t.Errorf("JQL missing 'To Do' state: %q", receivedJQL)
	}
	if !strings.Contains(receivedJQL, `"Code Review"`) {
		t.Errorf("JQL missing 'Code Review' state: %q", receivedJQL)
	}
	// Must NOT contain default states that weren't configured.
	if strings.Contains(receivedJQL, `"Backlog"`) {
		t.Errorf("JQL contains default state 'Backlog' despite custom active_states: %q", receivedJQL)
	}
}

func TestFetchCandidateIssues_QueryFilter(t *testing.T) {
	t.Parallel()

	var receivedJQL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedJQL = r.URL.Query().Get("jql")
		w.Write(loadFixture(t, "search_empty.json")) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	config := validConfig(srv.URL)
	config["query_filter"] = "component = 'api' OR component = 'web'"
	a := mustAdapter(t, config)
	_, err := a.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	if !strings.Contains(receivedJQL, "AND (component = 'api' OR component = 'web')") {
		t.Errorf("JQL missing queryFilter: %q", receivedJQL)
	}
}

func TestFetchCandidateIssues_NoQueryFilter(t *testing.T) {
	t.Parallel()

	var receivedJQL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedJQL = r.URL.Query().Get("jql")
		w.Write(loadFixture(t, "search_empty.json")) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	_, err := a.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	// Should not have dangling AND before ORDER BY
	idx := strings.Index(receivedJQL, "ORDER BY")
	if idx < 0 {
		t.Fatalf("JQL missing ORDER BY: %q", receivedJQL)
	}
	before := receivedJQL[:idx]
	if strings.HasSuffix(strings.TrimSpace(before), "AND") {
		t.Errorf("JQL has trailing AND before ORDER BY: %q", receivedJQL)
	}
}

// --- FetchIssueByID tests ---

func TestFetchIssueByID_WithComments(t *testing.T) {
	t.Parallel()

	issueData := loadFixture(t, "issue_detail.json")
	commentsData := loadFixture(t, "comments.json")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/comment"):
			w.Write(commentsData) //nolint:errcheck // test helper
		default:
			w.Write(issueData) //nolint:errcheck // test helper
		}
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	issue, err := a.FetchIssueByID(context.Background(), "PROJ-5")
	if err != nil {
		t.Fatalf("FetchIssueByID: %v", err)
	}

	if issue.Identifier != "PROJ-5" {
		t.Errorf("Identifier = %q", issue.Identifier)
	}
	if issue.Title != "Refactor database layer" {
		t.Errorf("Title = %q", issue.Title)
	}
	// ADF description flattened
	if !strings.Contains(issue.Description, "Refactor the persistence layer:") {
		t.Errorf("Description = %q, should contain flattened text", issue.Description)
	}
	// Comments populated
	if issue.Comments == nil {
		t.Fatal("Comments is nil")
	}
	if len(issue.Comments) != 2 {
		t.Fatalf("len(Comments) = %d, want 2", len(issue.Comments))
	}
	if issue.Comments[0].Author != "Alice Smith" {
		t.Errorf("Comments[0].Author = %q", issue.Comments[0].Author)
	}
	if issue.Comments[0].Body != "Looks good, please proceed." {
		t.Errorf("Comments[0].Body = %q", issue.Comments[0].Body)
	}
	// Second comment has nil author
	if issue.Comments[1].Author != "" {
		t.Errorf("Comments[1].Author = %q, want empty for nil author", issue.Comments[1].Author)
	}
}

func TestFetchIssueByID_NoComments(t *testing.T) {
	t.Parallel()

	issueData := loadFixture(t, "issue_detail.json")
	emptyComments := loadFixture(t, "comments_empty.json")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/comment") {
			w.Write(emptyComments) //nolint:errcheck // test helper
		} else {
			w.Write(issueData) //nolint:errcheck // test helper
		}
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	issue, err := a.FetchIssueByID(context.Background(), "PROJ-5")
	if err != nil {
		t.Fatalf("FetchIssueByID: %v", err)
	}
	if issue.Comments == nil {
		t.Fatal("Comments is nil, want non-nil empty slice")
	}
	if len(issue.Comments) != 0 {
		t.Errorf("len(Comments) = %d, want 0", len(issue.Comments))
	}
}

func TestFetchIssueByID_NotFound(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	_, err := a.FetchIssueByID(context.Background(), "PROJ-999")
	assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)

	var te *domain.TrackerError
	errors.As(err, &te)
	if !strings.Contains(te.Message, "PROJ-999") {
		t.Errorf("error message = %q, should contain issue ID", te.Message)
	}
}

func TestFetchIssueByID_MultiPageComments(t *testing.T) {
	t.Parallel()

	issueData := loadFixture(t, "issue_detail.json")
	commentsPage1 := loadFixture(t, "comments_multi_page_1.json")
	commentsPage2 := loadFixture(t, "comments_multi_page_2.json")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/comment") {
			startAt := r.URL.Query().Get("startAt")
			if startAt == "0" || startAt == "" {
				w.Write(commentsPage1) //nolint:errcheck // test helper
			} else {
				w.Write(commentsPage2) //nolint:errcheck // test helper
			}
		} else {
			w.Write(issueData) //nolint:errcheck // test helper
		}
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	issue, err := a.FetchIssueByID(context.Background(), "PROJ-5")
	if err != nil {
		t.Fatalf("FetchIssueByID: %v", err)
	}
	if len(issue.Comments) != 3 {
		t.Fatalf("len(Comments) = %d, want 3", len(issue.Comments))
	}
	if issue.Comments[0].ID != "30001" {
		t.Errorf("Comments[0].ID = %q, want 30001", issue.Comments[0].ID)
	}
	if issue.Comments[2].ID != "30003" {
		t.Errorf("Comments[2].ID = %q, want 30003", issue.Comments[2].ID)
	}
}

// --- FetchIssuesByStates tests ---

func TestFetchIssuesByStates_EmptyStates(t *testing.T) {
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
		t.Error("server was called, but empty states should return immediately")
	}
}

func TestFetchIssuesByStates_SingleState(t *testing.T) {
	t.Parallel()

	var receivedJQL string
	fixture := loadFixture(t, "search_single_page.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedJQL = r.URL.Query().Get("jql")
		w.Write(fixture) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	issues, err := a.FetchIssuesByStates(context.Background(), []string{"Done"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("len = %d, want 2", len(issues))
	}
	if !strings.Contains(receivedJQL, `status IN ("Done")`) {
		t.Errorf("JQL = %q, should contain status IN (\"Done\")", receivedJQL)
	}
	if !strings.Contains(receivedJQL, "ORDER BY created ASC") {
		t.Errorf("JQL = %q, should contain ORDER BY created ASC", receivedJQL)
	}
}

func TestFetchIssuesByStates_QueryFilter(t *testing.T) {
	t.Parallel()

	var receivedJQL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedJQL = r.URL.Query().Get("jql")
		w.Write(loadFixture(t, "search_empty.json")) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	config := validConfig(srv.URL)
	config["query_filter"] = "label = 'critical'"
	a := mustAdapter(t, config)
	_, err := a.FetchIssuesByStates(context.Background(), []string{"Done"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates: %v", err)
	}
	if !strings.Contains(receivedJQL, "AND (label = 'critical')") {
		t.Errorf("JQL = %q, should contain query filter", receivedJQL)
	}
}

// --- FetchIssueStatesByIDs tests ---

func TestFetchIssueStatesByIDs_Empty(t *testing.T) {
	t.Parallel()

	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	result, err := a.FetchIssueStatesByIDs(context.Background(), []string{})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs: %v", err)
	}
	if result == nil {
		t.Fatal("result is nil")
	}
	if len(result) != 0 {
		t.Errorf("len = %d, want 0", len(result))
	}
	if called {
		t.Error("server was called, but empty IDs should return immediately")
	}
}

func TestFetchIssueStatesByIDs_SingleBatch(t *testing.T) {
	t.Parallel()

	var receivedJQL string
	resp := searchResponse{
		Issues: []jiraIssue{
			{ID: "1", Key: "PROJ-1", Fields: jiraFields{Status: &jiraStatus{Name: "To Do"}}},
			{ID: "2", Key: "PROJ-2", Fields: jiraFields{Status: &jiraStatus{Name: "Done"}}},
		},
	}
	respBytes, _ := json.Marshal(resp)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedJQL = r.URL.Query().Get("jql")
		w.Write(respBytes) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	result, err := a.FetchIssueStatesByIDs(context.Background(), []string{"1", "2", "3"})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs: %v", err)
	}

	// ID "3" is missing from response — omitted from map
	if len(result) != 2 {
		t.Fatalf("len = %d, want 2", len(result))
	}
	if result["1"] != "To Do" {
		t.Errorf("result[\"1\"] = %q, want To Do", result["1"])
	}
	if result["2"] != "Done" {
		t.Errorf("result[\"2\"] = %q, want Done", result["2"])
	}
	if _, exists := result["3"]; exists {
		t.Error("ID \"3\" should be absent from result")
	}

	// Verify JQL uses id IN (numeric IDs), not key IN
	if !strings.Contains(receivedJQL, "id IN") {
		t.Errorf("JQL = %q, should use id IN", receivedJQL)
	}
	if strings.Contains(receivedJQL, "key IN") {
		t.Errorf("JQL = %q, should NOT use key IN", receivedJQL)
	}
}

func TestFetchIssueStatesByIDs_MultiBatch(t *testing.T) {
	t.Parallel()

	var requestCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestCount, 1)
		jql := r.URL.Query().Get("jql")
		if !strings.Contains(jql, "id IN") {
			t.Errorf("JQL = %q, should use id IN", jql)
		}

		// Verify the batch does not exceed 40 IDs.
		if start := strings.Index(jql, "id IN ("); start != -1 {
			inner := jql[start+len("id IN ("):]
			if end := strings.Index(inner, ")"); end != -1 {
				idCount := len(strings.Split(strings.TrimSpace(inner[:end]), ","))
				if idCount > 40 {
					t.Errorf("batch has %d IDs, max allowed 40", idCount)
				}
			}
		}

		// Return one issue per batch
		resp := searchResponse{
			Issues: []jiraIssue{
				{ID: "1", Key: "PROJ-1", Fields: jiraFields{Status: &jiraStatus{Name: "Open"}}},
			},
		}
		data, _ := json.Marshal(resp)
		w.Write(data) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	// Create 45 numeric IDs to force 2 batches (40 + 5)
	ids := make([]string, 45)
	for i := range ids {
		ids[i] = fmt.Sprintf("%d", i+1)
	}

	a := mustAdapter(t, validConfig(srv.URL))
	_, err := a.FetchIssueStatesByIDs(context.Background(), ids)
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs: %v", err)
	}

	if got := atomic.LoadInt32(&requestCount); got != 2 {
		t.Errorf("request count = %d, want 2 batches", got)
	}
}

func TestFetchIssueStatesByIDs_NoQueryFilter(t *testing.T) {
	t.Parallel()

	var receivedJQL string
	resp := searchResponse{
		Issues: []jiraIssue{
			{ID: "10001", Key: "PROJ-1", Fields: jiraFields{Status: &jiraStatus{Name: "Open"}}},
		},
	}
	respBytes, _ := json.Marshal(resp)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedJQL = r.URL.Query().Get("jql")
		w.Write(respBytes) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	config := validConfig(srv.URL)
	config["query_filter"] = "component = 'api'"
	a := mustAdapter(t, config)
	_, err := a.FetchIssueStatesByIDs(context.Background(), []string{"10001"})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs: %v", err)
	}
	// queryFilter must NOT be in the JQL for state-by-IDs
	if strings.Contains(receivedJQL, "component") {
		t.Errorf("JQL = %q, should NOT contain queryFilter", receivedJQL)
	}
	// Must use id IN, not key IN
	if !strings.Contains(receivedJQL, "id IN") {
		t.Errorf("JQL = %q, should use id IN", receivedJQL)
	}
}

// TestFetchIssueStatesByIDs_ResultKeyedByID verifies the regression fix:
// results must be keyed by numeric ID (iss.ID), not by Jira key
// (iss.Identifier). Callers (reconciliation, worker state refresh)
// look up by issue.ID which is the numeric internal Jira ID.
func TestFetchIssueStatesByIDs_ResultKeyedByID(t *testing.T) {
	t.Parallel()

	resp := searchResponse{
		Issues: []jiraIssue{
			{ID: "10037", Key: "ST-5", Fields: jiraFields{Status: &jiraStatus{Name: "Done"}}},
		},
	}
	respBytes, _ := json.Marshal(resp)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(respBytes) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	result, err := a.FetchIssueStatesByIDs(context.Background(), []string{"10037"})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs: %v", err)
	}

	// Must be keyed by numeric ID, not by Jira key
	if result["10037"] != "Done" {
		t.Errorf("result[\"10037\"] = %q, want \"Done\"", result["10037"])
	}
	if _, exists := result["ST-5"]; exists {
		t.Error("result should NOT be keyed by Jira key \"ST-5\"")
	}
}

// --- FetchIssueStatesByIdentifiers tests ---

func TestFetchIssueStatesByIdentifiers_Empty(t *testing.T) {
	t.Parallel()

	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	result, err := a.FetchIssueStatesByIdentifiers(context.Background(), []string{})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIdentifiers: %v", err)
	}
	if result == nil {
		t.Fatal("result is nil")
	}
	if len(result) != 0 {
		t.Errorf("len = %d, want 0", len(result))
	}
	if called {
		t.Error("server was called, but empty identifiers should return immediately")
	}
}

func TestFetchIssueStatesByIdentifiers_SingleBatch(t *testing.T) {
	t.Parallel()

	var receivedJQL string
	resp := searchResponse{
		Issues: []jiraIssue{
			{ID: "1", Key: "PROJ-1", Fields: jiraFields{Status: &jiraStatus{Name: "To Do"}}},
			{ID: "2", Key: "PROJ-2", Fields: jiraFields{Status: &jiraStatus{Name: "Done"}}},
		},
	}
	respBytes, _ := json.Marshal(resp)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedJQL = r.URL.Query().Get("jql")
		w.Write(respBytes) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	result, err := a.FetchIssueStatesByIdentifiers(context.Background(), []string{"PROJ-1", "PROJ-2", "PROJ-3"})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIdentifiers: %v", err)
	}

	// PROJ-3 is missing from response — omitted from map.
	if len(result) != 2 {
		t.Fatalf("len = %d, want 2", len(result))
	}
	if result["PROJ-1"] != "To Do" {
		t.Errorf("PROJ-1 = %q, want To Do", result["PROJ-1"])
	}
	if result["PROJ-2"] != "Done" {
		t.Errorf("PROJ-2 = %q, want Done", result["PROJ-2"])
	}
	if _, exists := result["PROJ-3"]; exists {
		t.Error("PROJ-3 should be absent from result")
	}

	// Verify JQL uses key IN.
	if !strings.Contains(receivedJQL, "key IN") {
		t.Errorf("JQL = %q, should use key IN", receivedJQL)
	}
}

func TestFetchIssueStatesByIdentifiers_MultiBatch(t *testing.T) {
	t.Parallel()

	var requestCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestCount, 1)
		jql := r.URL.Query().Get("jql")
		keyCount := strings.Count(jql, `"PROJ-`)
		if keyCount > batchSize {
			t.Errorf("batch has %d keys, max allowed %d", keyCount, batchSize)
		}

		resp := searchResponse{
			Issues: []jiraIssue{
				{ID: "1", Key: "PROJ-1", Fields: jiraFields{Status: &jiraStatus{Name: "Open"}}},
			},
		}
		data, _ := json.Marshal(resp)
		w.Write(data) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	ids := make([]string, 45)
	for i := range ids {
		ids[i] = fmt.Sprintf("PROJ-%d", i+1)
	}

	a := mustAdapter(t, validConfig(srv.URL))
	_, err := a.FetchIssueStatesByIdentifiers(context.Background(), ids)
	if err != nil {
		t.Fatalf("FetchIssueStatesByIdentifiers: %v", err)
	}

	if got := atomic.LoadInt32(&requestCount); got != 2 {
		t.Errorf("request count = %d, want 2 batches", got)
	}
}

func TestFetchIssueStatesByIdentifiers_NoQueryFilter(t *testing.T) {
	t.Parallel()

	var receivedJQL string
	resp := searchResponse{
		Issues: []jiraIssue{
			{ID: "1", Key: "PROJ-1", Fields: jiraFields{Status: &jiraStatus{Name: "Open"}}},
		},
	}
	respBytes, _ := json.Marshal(resp)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedJQL = r.URL.Query().Get("jql")
		w.Write(respBytes) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	config := validConfig(srv.URL)
	config["query_filter"] = "component = 'api'"
	a := mustAdapter(t, config)
	_, err := a.FetchIssueStatesByIdentifiers(context.Background(), []string{"PROJ-1"})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIdentifiers: %v", err)
	}
	if strings.Contains(receivedJQL, "component") {
		t.Errorf("JQL = %q, should NOT contain queryFilter", receivedJQL)
	}
}

// --- FetchIssueComments tests ---

func TestFetchIssueComments_MultiPage(t *testing.T) {
	t.Parallel()

	page1 := loadFixture(t, "comments_multi_page_1.json")
	page2 := loadFixture(t, "comments_multi_page_2.json")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startAt := r.URL.Query().Get("startAt")
		if startAt == "0" || startAt == "" {
			w.Write(page1) //nolint:errcheck // test helper
		} else {
			w.Write(page2) //nolint:errcheck // test helper
		}
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	comments, err := a.FetchIssueComments(context.Background(), "PROJ-1")
	if err != nil {
		t.Fatalf("FetchIssueComments: %v", err)
	}
	if len(comments) != 3 {
		t.Fatalf("len = %d, want 3", len(comments))
	}
	if comments[0].Body != "First comment." {
		t.Errorf("comments[0].Body = %q", comments[0].Body)
	}
	if comments[2].Body != "Third comment." {
		t.Errorf("comments[2].Body = %q", comments[2].Body)
	}
}

func TestFetchIssueComments_Empty(t *testing.T) {
	t.Parallel()

	fixture := loadFixture(t, "comments_empty.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(fixture) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	comments, err := a.FetchIssueComments(context.Background(), "PROJ-1")
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

func TestFetchIssueComments_NotFound(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	_, err := a.FetchIssueComments(context.Background(), "PROJ-999")
	assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)
}

// --- Full lifecycle integration test ---

func TestAdapterLifecycle(t *testing.T) {
	t.Parallel()

	searchFixture := loadFixture(t, "search_single_page.json")
	issueFixture := loadFixture(t, "issue_detail.json")
	commentsFixture := loadFixture(t, "comments.json")
	emptyComments := loadFixture(t, "comments_empty.json")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == "/rest/api/3/search/jql":
			jql := r.URL.Query().Get("jql")
			if strings.Contains(jql, "id IN") {
				// FetchIssueStatesByIDs — return minimal status keyed by numeric ID
				resp := searchResponse{
					Issues: []jiraIssue{
						{ID: "10001", Key: "PROJ-1", Fields: jiraFields{Status: &jiraStatus{Name: "To Do"}}},
						{ID: "10002", Key: "PROJ-2", Fields: jiraFields{Status: &jiraStatus{Name: "In Progress"}}},
					},
				}
				data, _ := json.Marshal(resp)
				w.Write(data) //nolint:errcheck // test helper
			} else {
				w.Write(searchFixture) //nolint:errcheck // test helper
			}
		case strings.HasSuffix(path, "/comment"):
			// Determine if we need comments or empty based on issue key
			if strings.Contains(path, "PROJ-5") {
				w.Write(commentsFixture) //nolint:errcheck // test helper
			} else {
				w.Write(emptyComments) //nolint:errcheck // test helper
			}
		case strings.Contains(path, "/rest/api/3/issue/"):
			w.Write(issueFixture) //nolint:errcheck // test helper
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	config := validConfig(srv.URL)
	config["active_states"] = []any{"To Do", "In Progress"}
	a := mustAdapter(t, config)

	ctx := context.Background()

	// 1. FetchCandidateIssues
	candidates, err := a.FetchCandidateIssues(ctx)
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("candidates len = %d, want 2", len(candidates))
	}
	// Verify normalization invariants
	for _, c := range candidates {
		if c.Labels == nil {
			t.Errorf("issue %s: Labels is nil", c.Identifier)
		}
		if c.BlockedBy == nil {
			t.Errorf("issue %s: BlockedBy is nil", c.Identifier)
		}
		if c.Comments != nil {
			t.Errorf("issue %s: Comments should be nil", c.Identifier)
		}
		if c.URL == "" {
			t.Errorf("issue %s: URL is empty", c.Identifier)
		}
	}
	// Verify label lowercasing on issue 1
	if candidates[0].Labels[0] != "feature" {
		t.Errorf("label not lowercased: %v", candidates[0].Labels)
	}
	// Verify blocker on issue 1 (only inward "Blocks")
	if len(candidates[0].BlockedBy) != 1 {
		t.Errorf("BlockedBy len = %d, want 1", len(candidates[0].BlockedBy))
	}

	// 2. FetchIssueByID
	detail, err := a.FetchIssueByID(ctx, "PROJ-5")
	if err != nil {
		t.Fatalf("FetchIssueByID: %v", err)
	}
	if detail.Comments == nil || len(detail.Comments) != 2 {
		t.Errorf("detail.Comments len = %d, want 2", len(detail.Comments))
	}
	if detail.Description == "" {
		t.Error("detail.Description is empty, want flattened ADF text")
	}

	// 3. FetchIssuesByStates
	terminal, err := a.FetchIssuesByStates(ctx, []string{"Done"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates: %v", err)
	}
	if len(terminal) != 2 {
		t.Errorf("terminal len = %d, want 2", len(terminal))
	}

	// 4. FetchIssueStatesByIDs — uses numeric IDs, results keyed by ID
	stateMap, err := a.FetchIssueStatesByIDs(ctx, []string{"10001", "10002"})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs: %v", err)
	}
	if stateMap["10001"] != "To Do" {
		t.Errorf("stateMap[\"10001\"] = %q, want To Do", stateMap["10001"])
	}
	if stateMap["10002"] != "In Progress" {
		t.Errorf("stateMap[\"10002\"] = %q, want In Progress", stateMap["10002"])
	}

	// 5. FetchIssueComments
	comments, err := a.FetchIssueComments(ctx, "PROJ-5")
	if err != nil {
		t.Fatalf("FetchIssueComments: %v", err)
	}
	if len(comments) != 2 {
		t.Fatalf("comments len = %d, want 2", len(comments))
	}
	if comments[0].Body != "Looks good, please proceed." {
		t.Errorf("comments[0].Body = %q", comments[0].Body)
	}
}

// --- TransitionIssue tests ---

func TestTransitionIssue_Success(t *testing.T) {
	t.Parallel()

	var postBody []byte
	var postPath string
	var getPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			getPath = r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "transitions.json")) //nolint:errcheck // test helper
		case "POST":
			postPath = r.URL.Path
			postBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	err := a.TransitionIssue(context.Background(), "PROJ-123", "Human Review")
	if err != nil {
		t.Fatalf("TransitionIssue() unexpected error: %v", err)
	}

	// Verify POST body contains transition.id "31"
	var req struct {
		Transition struct {
			ID string `json:"id"`
		} `json:"transition"`
	}
	if err := json.Unmarshal(postBody, &req); err != nil {
		t.Fatalf("unmarshal POST body: %v", err)
	}
	if req.Transition.ID != "31" {
		t.Errorf("POST transition.id = %q, want %q", req.Transition.ID, "31")
	}

	// Verify request paths (test case 13)
	wantPath := "/rest/api/3/issue/PROJ-123/transitions"
	if getPath != wantPath {
		t.Errorf("GET path = %q, want %q", getPath, wantPath)
	}
	if postPath != wantPath {
		t.Errorf("POST path = %q, want %q", postPath, wantPath)
	}
}

func TestTransitionIssue_CaseInsensitive(t *testing.T) {
	t.Parallel()

	// Fixture has "Human Review" but we pass "human review"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "transitions.json")) //nolint:errcheck // test helper
		case "POST":
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	err := a.TransitionIssue(context.Background(), "PROJ-123", "human review")
	if err != nil {
		t.Fatalf("TransitionIssue() unexpected error: %v", err)
	}
}

func TestTransitionIssue_DuplicateTarget_FirstMatch(t *testing.T) {
	t.Parallel()

	var postBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "transitions_duplicate_target.json")) //nolint:errcheck // test helper
		case "POST":
			postBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	err := a.TransitionIssue(context.Background(), "PROJ-123", "Human Review")
	if err != nil {
		t.Fatalf("TransitionIssue() unexpected error: %v", err)
	}

	// Must use first match (id "31"), not second (id "51")
	var req struct {
		Transition struct {
			ID string `json:"id"`
		} `json:"transition"`
	}
	if err := json.Unmarshal(postBody, &req); err != nil {
		t.Fatalf("unmarshal POST body: %v", err)
	}
	if req.Transition.ID != "31" {
		t.Errorf("POST transition.id = %q, want %q (first match)", req.Transition.ID, "31")
	}
}

func TestTransitionIssue_ErrorCases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		handler  http.HandlerFunc
		wantKind domain.TrackerErrorKind
	}{
		{
			name: "target state not found",
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				w.Write(loadFixture(t, "transitions_no_match.json")) //nolint:errcheck // test helper
			}),
			wantKind: domain.ErrTrackerPayload,
		},
		{
			name: "empty transitions list",
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				w.Write(loadFixture(t, "transitions_empty.json")) //nolint:errcheck // test helper
			}),
			wantKind: domain.ErrTrackerPayload,
		},
		{
			name: "issue not found GET 404",
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNotFound)
			}),
			wantKind: domain.ErrTrackerNotFound,
		},
		{
			name: "auth error GET 401",
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
			}),
			wantKind: domain.ErrTrackerAuth,
		},
		{
			name: "transport error GET 500",
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			}),
			wantKind: domain.ErrTrackerTransport,
		},
		{
			name: "GET OK POST 400",
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case "GET":
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					w.Write(loadFixture(t, "transitions.json")) //nolint:errcheck // test helper
				case "POST":
					w.WriteHeader(http.StatusBadRequest)
					w.Write([]byte("workflow error")) //nolint:errcheck // test helper
				}
			}),
			wantKind: domain.ErrTrackerPayload,
		},
		{
			name: "GET OK POST 500",
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case "GET":
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					w.Write(loadFixture(t, "transitions.json")) //nolint:errcheck // test helper
				case "POST":
					w.WriteHeader(http.StatusInternalServerError)
				}
			}),
			wantKind: domain.ErrTrackerTransport,
		},
		{
			name: "malformed JSON from GET",
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{invalid json`)) //nolint:errcheck // test helper
			}),
			wantKind: domain.ErrTrackerPayload,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(tt.handler)
			defer srv.Close()

			a := mustAdapter(t, validConfig(srv.URL))
			err := a.TransitionIssue(context.Background(), "PROJ-123", "Human Review")
			assertTrackerErrorKind(t, err, tt.wantKind)
		})
	}
}

func TestTransitionIssue_TargetStateNotFound_MessageContainsState(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(loadFixture(t, "transitions_no_match.json")) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	a := mustAdapter(t, validConfig(srv.URL))
	err := a.TransitionIssue(context.Background(), "PROJ-123", "Human Review")
	if err == nil {
		t.Fatal("TransitionIssue() expected error, got nil")
	}

	var te *domain.TrackerError
	if !errors.As(err, &te) {
		t.Fatalf("error type = %T, want *domain.TrackerError", err)
	}
	if !strings.Contains(te.Message, "Human Review") {
		t.Errorf("TrackerError.Message = %q, should contain target state %q", te.Message, "Human Review")
	}
}

func TestTransitionIssue_ContextCancellation(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		// Block until the request context is canceled
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	a := mustAdapter(t, validConfig(srv.URL))

	errCh := make(chan error, 1)
	go func() {
		errCh <- a.TransitionIssue(ctx, "PROJ-123", "Human Review")
	}()

	<-started
	cancel()

	err := <-errCh
	if err == nil {
		t.Fatal("TransitionIssue() expected error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("TransitionIssue() error = %v, want context.Canceled", err)
	}
}
