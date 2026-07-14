package gitea

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

const (
	testOwner = "acme"
	testRepo  = "widgets"
)

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return data
}

func validConfig(endpoint string) map[string]any {
	return map[string]any{
		"endpoint": endpoint,
		"api_key":  "test-token",
		"project":  testOwner + "/" + testRepo,
	}
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

// newPreflightMux returns a ServeMux pre-registered with the two
// construction-preflight routes (GET /api/v1/user, GET
// /api/v1/repos/{owner}/{repo}) plus a catch-all that fails the test on any
// unregistered request. Callers register only the routes their operation
// under test exercises; an unexpected call (a write stub issuing a request, a
// parent lookup that must never happen, or a stray second page) fails loudly
// instead of receiving a silent 404.
func newPreflightMux(t *testing.T) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/user", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(loadFixture(t, "user.json")) //nolint:errcheck // test helper
	})
	mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`)) //nolint:errcheck // test helper
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})
	return mux
}

// mustAdapter starts an httptest.Server for mux and constructs a
// *GiteaAdapter against it, registering server cleanup. mux must already
// carry the two construction-preflight routes; see [newPreflightMux].
func mustAdapter(t *testing.T, mux *http.ServeMux) *GiteaAdapter {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a, err := NewGiteaAdapter(validConfig(srv.URL))
	if err != nil {
		t.Fatalf("NewGiteaAdapter: %v", err)
	}
	return a.(*GiteaAdapter)
}

// buildBulkComments returns a JSON array of n gitea comment objects, ascending
// by id and created_at, modeling the verified 60-comment single-response
// shape without hand-authoring a repetitive fixture file.
func buildBulkComments(t *testing.T, n int) []byte {
	t.Helper()
	comments := make([]map[string]any, n)
	for i := range n {
		comments[i] = map[string]any{
			"id":         9000 + i,
			"user":       map[string]string{"login": fmt.Sprintf("user%d", i)},
			"body":       fmt.Sprintf("comment %d", i),
			"created_at": fmt.Sprintf("2026-01-01T%02d:00:00Z", i%24),
		}
	}
	data, err := json.Marshal(comments)
	if err != nil {
		t.Fatalf("marshal bulk comments: %v", err)
	}
	return data
}

// --- Constructor ---

func TestNewGiteaAdapter(t *testing.T) {
	t.Parallel()

	t.Run("config errors fail before any network call", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name     string
			config   map[string]any
			wantKind domain.TrackerErrorKind
		}{
			{"missing api_key", map[string]any{"project": "acme/widgets", "endpoint": "http://localhost"}, domain.ErrMissingTrackerAPIKey},
			{"empty api_key", map[string]any{"api_key": "", "project": "acme/widgets", "endpoint": "http://localhost"}, domain.ErrMissingTrackerAPIKey},
			{"missing project", map[string]any{"api_key": "tok", "endpoint": "http://localhost"}, domain.ErrMissingTrackerProject},
			{"empty project", map[string]any{"api_key": "tok", "project": "", "endpoint": "http://localhost"}, domain.ErrMissingTrackerProject},
			{"project with no slash", map[string]any{"api_key": "tok", "project": "acmewidgets", "endpoint": "http://localhost"}, domain.ErrTrackerPayload},
			{"project with two slashes", map[string]any{"api_key": "tok", "project": "acme/widgets/extra", "endpoint": "http://localhost"}, domain.ErrTrackerPayload},
			{"project with empty owner", map[string]any{"api_key": "tok", "project": "/widgets", "endpoint": "http://localhost"}, domain.ErrTrackerPayload},
			{"project with empty repo", map[string]any{"api_key": "tok", "project": "acme/", "endpoint": "http://localhost"}, domain.ErrTrackerPayload},
			{"missing endpoint", map[string]any{"api_key": "tok", "project": "acme/widgets"}, domain.ErrTrackerPayload},
			{"empty endpoint", map[string]any{"api_key": "tok", "project": "acme/widgets", "endpoint": ""}, domain.ErrTrackerPayload},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				a, err := NewGiteaAdapter(tt.config)
				assertTrackerErrorKind(t, err, tt.wantKind)
				if a != nil {
					t.Error("adapter should be nil on error")
				}
			})
		}
	})

	t.Run("successful construction applies default state lists", func(t *testing.T) {
		t.Parallel()

		a := mustAdapter(t, newPreflightMux(t))

		if a.owner != testOwner {
			t.Errorf("owner = %q, want %q", a.owner, testOwner)
		}
		if a.repo != testRepo {
			t.Errorf("repo = %q, want %q", a.repo, testRepo)
		}
		if len(a.activeStates) != 3 || a.activeStates[0] != "backlog" {
			t.Errorf("activeStates = %v, want defaults starting with backlog", a.activeStates)
		}
		if len(a.terminalStates) != 2 || a.terminalStates[0] != "done" {
			t.Errorf("terminalStates = %v, want defaults starting with done", a.terminalStates)
		}
		if a.handoffState != "" {
			t.Errorf("handoffState = %q, want empty", a.handoffState)
		}
	})

	t.Run("api/v1 suffix is appended exactly once regardless of source form", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name  string
			build func(base string) string
		}{
			{"bare host", func(base string) string { return base }},
			{"trailing slash", func(base string) string { return base + "/" }},
			{"already suffixed", func(base string) string { return base + "/api/v1" }},
			{"already suffixed with trailing slash", func(base string) string { return base + "/api/v1/" }},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				srv := httptest.NewServer(newPreflightMux(t))
				defer srv.Close()

				cfg := validConfig(tt.build(srv.URL))
				if _, err := NewGiteaAdapter(cfg); err != nil {
					t.Fatalf("NewGiteaAdapter(endpoint=%q): %v", cfg["endpoint"], err)
				}
			})
		}
	})

	t.Run("custom state lists are lowercased and handoff_state is trimmed", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(newPreflightMux(t))
		defer srv.Close()

		cfg := validConfig(srv.URL)
		cfg["active_states"] = []any{"Todo", "IN-PROGRESS"}
		cfg["terminal_states"] = []string{"Done", "WONTFIX"}
		cfg["handoff_state"] = "  Review  "

		a, err := NewGiteaAdapter(cfg)
		if err != nil {
			t.Fatalf("NewGiteaAdapter: %v", err)
		}
		ga := a.(*GiteaAdapter)

		if ga.activeStates[0] != "todo" || ga.activeStates[1] != "in-progress" {
			t.Errorf("activeStates = %v, want [todo in-progress]", ga.activeStates)
		}
		if ga.terminalStates[0] != "done" || ga.terminalStates[1] != "wontfix" {
			t.Errorf("terminalStates = %v, want [done wontfix]", ga.terminalStates)
		}
		if ga.handoffState != "review" {
			t.Errorf("handoffState = %q, want %q (trimmed and lowercased)", ga.handoffState, "review")
		}
	})

	t.Run("does not mutate caller-supplied state slices", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(newPreflightMux(t))
		defer srv.Close()

		cfg := validConfig(srv.URL)
		cfg["active_states"] = []string{"InProgress", "Review"}
		cfg["terminal_states"] = []string{"Done", "WontFix"}

		if _, err := NewGiteaAdapter(cfg); err != nil {
			t.Fatalf("NewGiteaAdapter: %v", err)
		}

		active := cfg["active_states"].([]string)
		if active[0] != "InProgress" || active[1] != "Review" {
			t.Errorf("active_states mutated: got %v, want original casing preserved", active)
		}
		terminal := cfg["terminal_states"].([]string)
		if terminal[0] != "Done" || terminal[1] != "WontFix" {
			t.Errorf("terminal_states mutated: got %v, want original casing preserved", terminal)
		}
	})

	t.Run("preflight failure blocks construction", func(t *testing.T) {
		t.Parallel()

		mux := http.NewServeMux()
		mux.HandleFunc("GET /api/v1/user", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write(loadFixture(t, "error_401.json")) //nolint:errcheck // test helper
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		a, err := NewGiteaAdapter(validConfig(srv.URL))
		assertTrackerErrorKind(t, err, domain.ErrTrackerAuth)
		if a != nil {
			t.Error("adapter should be nil when the preflight fails")
		}
	})
}

// --- Registration ---

func TestGiteaAdapterRegistration(t *testing.T) {
	t.Parallel()

	if !registry.Trackers.Has("gitea") {
		t.Fatal(`Trackers.Has("gitea") = false, want true`)
	}
	if _, err := registry.Trackers.Get("gitea"); err != nil {
		t.Errorf(`Trackers.Get("gitea") = %v, want registered constructor`, err)
	}
}

// --- paginateIssues ---

func TestPaginateIssues(t *testing.T) {
	t.Parallel()

	t.Run("follows Link headers across pages below the requested cap", func(t *testing.T) {
		t.Parallel()

		page1 := loadFixture(t, "issues_paginate_page1.json") // 2 items
		page2 := loadFixture(t, "issues_paginate_page2.json") // 2 items
		page3 := loadFixture(t, "issues_paginate_page3.json") // 1 item

		var srvURL string
		var calls atomic.Int32
		mux := newPreflightMux(t)
		basePath := "/api/v1/repos/" + testOwner + "/" + testRepo + "/issues"
		mux.HandleFunc("GET "+basePath, func(w http.ResponseWriter, r *http.Request) {
			n := calls.Add(1)
			switch n {
			case 1:
				w.Header().Set("Link", fmt.Sprintf(`<%s%s?page=2>; rel="next"`, srvURL, basePath))
				w.WriteHeader(http.StatusOK)
				w.Write(page1) //nolint:errcheck // test helper
			case 2:
				w.Header().Set("Link", fmt.Sprintf(`<%s%s?page=3>; rel="next"`, srvURL, basePath))
				w.WriteHeader(http.StatusOK)
				w.Write(page2) //nolint:errcheck // test helper
			default:
				w.WriteHeader(http.StatusOK)
				w.Write(page3) //nolint:errcheck // test helper
			}
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()
		srvURL = srv.URL

		a, err := NewGiteaAdapter(validConfig(srv.URL))
		if err != nil {
			t.Fatalf("NewGiteaAdapter: %v", err)
		}
		ga := a.(*GiteaAdapter)

		items, err := ga.paginateIssues(context.Background(), "/repos/"+testOwner+"/"+testRepo+"/issues", url.Values{"limit": {"50"}})
		if err != nil {
			t.Fatalf("paginateIssues: %v", err)
		}
		if len(items) != 5 {
			t.Fatalf("len = %d, want 5 across 3 pages, each well below the requested limit=50", len(items))
		}
		if got := calls.Load(); got != 3 {
			t.Errorf("call count = %d, want 3 (one per page)", got)
		}
	})

	t.Run("single page with no Link header stops after one request", func(t *testing.T) {
		t.Parallel()

		var calls atomic.Int32
		mux := newPreflightMux(t)
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues", func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "issues_paginate_page3.json")) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, mux)

		items, err := a.paginateIssues(context.Background(), "/repos/"+testOwner+"/"+testRepo+"/issues", url.Values{"limit": {"50"}})
		if err != nil {
			t.Fatalf("paginateIssues: %v", err)
		}
		if len(items) != 1 {
			t.Errorf("len = %d, want 1", len(items))
		}
		if got := calls.Load(); got != 1 {
			t.Errorf("call count = %d, want 1 (no Link header means end of results)", got)
		}
	})

	t.Run("max pages guard stops early and logs a warning", func(t *testing.T) {
		t.Parallel()

		var srvURL string
		var calls atomic.Int32
		mux := newPreflightMux(t)
		basePath := "/api/v1/repos/" + testOwner + "/" + testRepo + "/issues"
		mux.HandleFunc("GET "+basePath, func(w http.ResponseWriter, r *http.Request) {
			n := calls.Add(1)
			w.Header().Set("Link", fmt.Sprintf(`<%s%s?page=%d>; rel="next"`, srvURL, basePath, n+1))
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`[{"number":1,"state":"open","labels":[{"name":"backlog"}]}]`)) //nolint:errcheck // test helper
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()
		srvURL = srv.URL

		a, err := NewGiteaAdapter(validConfig(srv.URL))
		if err != nil {
			t.Fatalf("NewGiteaAdapter: %v", err)
		}
		ga := a.(*GiteaAdapter)

		var buf bytes.Buffer
		ga.log = slog.New(slog.NewTextHandler(&buf, nil))

		items, err := ga.paginateIssues(context.Background(), "/repos/"+testOwner+"/"+testRepo+"/issues", url.Values{"limit": {"50"}})
		if err != nil {
			t.Fatalf("paginateIssues: %v", err)
		}
		if len(items) != maxPages {
			t.Errorf("len = %d, want %d (maxPages cap)", len(items), maxPages)
		}
		if got := calls.Load(); int(got) != maxPages {
			t.Errorf("call count = %d, want %d", got, maxPages)
		}
		if !strings.Contains(buf.String(), "pagination limit reached") {
			t.Errorf("expected a WARN for the pagination limit\noutput: %s", buf.String())
		}
	})
}

// --- FetchCandidateIssues ---

func TestFetchCandidateIssues(t *testing.T) {
	t.Parallel()

	t.Run("pagination to exhaustion, PR guard, terminal filter, and ascending resort", func(t *testing.T) {
		t.Parallel()

		page1 := loadFixture(t, "issues_candidates_page1.json") // #8 backlog, #6 PR
		page2 := loadFixture(t, "issues_candidates_page2.json") // #1 backlog, #3 done, #2 in-progress

		var srvURL string
		var calls atomic.Int32
		mux := newPreflightMux(t)
		basePath := "/api/v1/repos/" + testOwner + "/" + testRepo + "/issues"
		mux.HandleFunc("GET "+basePath, func(w http.ResponseWriter, r *http.Request) {
			n := calls.Add(1)
			if n == 1 {
				w.Header().Set("Link", fmt.Sprintf(`<%s%s?page=2>; rel="next"`, srvURL, basePath))
				w.WriteHeader(http.StatusOK)
				w.Write(page1) //nolint:errcheck // test helper
				return
			}
			w.WriteHeader(http.StatusOK)
			w.Write(page2) //nolint:errcheck // test helper
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()
		srvURL = srv.URL

		a, err := NewGiteaAdapter(validConfig(srv.URL))
		if err != nil {
			t.Fatalf("NewGiteaAdapter: %v", err)
		}

		issues, err := a.FetchCandidateIssues(context.Background())
		if err != nil {
			t.Fatalf("FetchCandidateIssues: %v", err)
		}

		// #6 (pull request) and #3 (done, terminal) are filtered out; #8, #1,
		// #2 remain, re-sorted ascending by CreatedAt regardless of page order.
		if len(issues) != 3 {
			t.Fatalf("len = %d, want 3 (pull request and terminal issue filtered)", len(issues))
		}
		wantOrder := []string{"1", "2", "8"}
		for i, want := range wantOrder {
			if issues[i].Identifier != want {
				t.Errorf("issues[%d].Identifier = %q, want %q (ascending CreatedAt order)", i, issues[i].Identifier, want)
			}
		}
		if got := calls.Load(); got != 2 {
			t.Errorf("call count = %d, want 2 (two pages)", got)
		}
	})

	t.Run("qualifies DisplayID and nils Comments", func(t *testing.T) {
		t.Parallel()

		mux := newPreflightMux(t)
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "issues_candidates_page2.json")) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, mux)

		issues, err := a.FetchCandidateIssues(context.Background())
		if err != nil {
			t.Fatalf("FetchCandidateIssues: %v", err)
		}
		if len(issues) == 0 {
			t.Fatal("issues is empty, want at least one active issue")
		}
		for _, iss := range issues {
			if iss.Comments != nil {
				t.Errorf("issue %s: Comments = %v, want nil", iss.Identifier, iss.Comments)
			}
			want := testOwner + "/" + testRepo + "#" + iss.Identifier
			if iss.DisplayID != want {
				t.Errorf("issue %s: DisplayID = %q, want %q", iss.Identifier, iss.DisplayID, want)
			}
		}
	})

	t.Run("empty response returns non-nil empty slice", func(t *testing.T) {
		t.Parallel()

		mux := newPreflightMux(t)
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("[]")) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, mux)

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
	})

	t.Run("auth error propagates", func(t *testing.T) {
		t.Parallel()

		mux := newPreflightMux(t)
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write(loadFixture(t, "error_401.json")) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, mux)

		_, err := a.FetchCandidateIssues(context.Background())
		assertTrackerErrorKind(t, err, domain.ErrTrackerAuth)
	})
}

// --- FetchIssuesByStates ---

func TestFetchIssuesByStates(t *testing.T) {
	t.Parallel()

	t.Run("empty states short-circuits with no API call", func(t *testing.T) {
		t.Parallel()

		a := mustAdapter(t, newPreflightMux(t)) // no issues route registered; any call fails the test

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
	})

	t.Run("requesting only an active state queries the open listing only", func(t *testing.T) {
		t.Parallel()

		mux := newPreflightMux(t)
		var gotStates []string
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues", func(w http.ResponseWriter, r *http.Request) {
			state := r.URL.Query().Get("state")
			gotStates = append(gotStates, state)
			switch state {
			case "open":
				w.WriteHeader(http.StatusOK)
				w.Write(loadFixture(t, "issues_open_by_states.json")) //nolint:errcheck // test helper
			default:
				t.Errorf("unexpected state query param: %q", state)
				w.WriteHeader(http.StatusBadRequest)
			}
		})
		a := mustAdapter(t, mux)

		issues, err := a.FetchIssuesByStates(context.Background(), []string{"in-progress"})
		if err != nil {
			t.Fatalf("FetchIssuesByStates: %v", err)
		}
		if len(issues) != 1 {
			t.Fatalf("len = %d, want 1 (only the in-progress issue)", len(issues))
		}
		if issues[0].Identifier != "10" {
			t.Errorf("Identifier = %q, want %q", issues[0].Identifier, "10")
		}
		if len(gotStates) != 1 || gotStates[0] != "open" {
			t.Errorf("state queries = %v, want exactly one open query", gotStates)
		}
	})

	t.Run("requesting only a terminal state queries the closed listing only", func(t *testing.T) {
		t.Parallel()

		mux := newPreflightMux(t)
		var gotStates []string
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues", func(w http.ResponseWriter, r *http.Request) {
			state := r.URL.Query().Get("state")
			gotStates = append(gotStates, state)
			switch state {
			case "closed":
				w.WriteHeader(http.StatusOK)
				w.Write(loadFixture(t, "issues_closed_by_states.json")) //nolint:errcheck // test helper
			default:
				t.Errorf("unexpected state query param: %q", state)
				w.WriteHeader(http.StatusBadRequest)
			}
		})
		a := mustAdapter(t, mux)

		issues, err := a.FetchIssuesByStates(context.Background(), []string{"done"})
		if err != nil {
			t.Fatalf("FetchIssuesByStates: %v", err)
		}
		if len(issues) != 1 {
			t.Fatalf("len = %d, want 1 (only the done issue)", len(issues))
		}
		if issues[0].Identifier != "20" {
			t.Errorf("Identifier = %q, want %q", issues[0].Identifier, "20")
		}
		if len(gotStates) != 1 || gotStates[0] != "closed" {
			t.Errorf("state queries = %v, want exactly one closed query", gotStates)
		}
	})

	t.Run("mixed request queries both the open and closed listings", func(t *testing.T) {
		t.Parallel()

		mux := newPreflightMux(t)
		gotStates := make(map[string]bool)
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues", func(w http.ResponseWriter, r *http.Request) {
			state := r.URL.Query().Get("state")
			gotStates[state] = true
			switch state {
			case "open":
				w.WriteHeader(http.StatusOK)
				w.Write(loadFixture(t, "issues_open_by_states.json")) //nolint:errcheck // test helper
			case "closed":
				w.WriteHeader(http.StatusOK)
				w.Write(loadFixture(t, "issues_closed_by_states.json")) //nolint:errcheck // test helper
			default:
				t.Errorf("unexpected state query param: %q", state)
				w.WriteHeader(http.StatusBadRequest)
			}
		})
		a := mustAdapter(t, mux)

		issues, err := a.FetchIssuesByStates(context.Background(), []string{"in-progress", "done"})
		if err != nil {
			t.Fatalf("FetchIssuesByStates: %v", err)
		}
		if len(issues) != 2 {
			t.Fatalf("len = %d, want 2 (one from each listing)", len(issues))
		}
		if !gotStates["open"] || !gotStates["closed"] {
			t.Errorf("state queries = %v, want both open and closed", gotStates)
		}
	})
}

// --- FetchIssueByID ---

func TestFetchIssueByID(t *testing.T) {
	t.Parallel()

	t.Run("full population", func(t *testing.T) {
		t.Parallel()

		mux := newPreflightMux(t)
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues/{index}", func(w http.ResponseWriter, r *http.Request) {
			if got := r.PathValue("index"); got != "42" {
				t.Errorf("unexpected issue index: %s", got)
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "issue_full.json")) //nolint:errcheck // test helper
		})
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues/42/dependencies", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "dependencies_one.json")) //nolint:errcheck // test helper
		})
		var commentCalls atomic.Int32
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues/42/comments", func(w http.ResponseWriter, r *http.Request) {
			commentCalls.Add(1)
			if r.URL.RawQuery != "" {
				t.Errorf("comments request carried a query string %q, want none (unpaginated route)", r.URL.RawQuery)
			}
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "comments_two.json")) //nolint:errcheck // test helper
		})
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues/42/parent", func(w http.ResponseWriter, r *http.Request) {
			t.Error("parent request must not be issued: gitea has no parent concept")
			w.WriteHeader(http.StatusNotFound)
		})

		a := mustAdapter(t, mux)
		issue, err := a.FetchIssueByID(context.Background(), "42")
		if err != nil {
			t.Fatalf("FetchIssueByID: %v", err)
		}

		if issue.ID != "42" || issue.Identifier != "42" {
			t.Errorf("ID/Identifier = %q/%q, want 42/42", issue.ID, issue.Identifier)
		}
		if want := testOwner + "/" + testRepo + "#42"; issue.DisplayID != want {
			t.Errorf("DisplayID = %q, want %q", issue.DisplayID, want)
		}
		if issue.Parent != nil {
			t.Errorf("Parent = %v, want nil", issue.Parent)
		}
		if len(issue.BlockedBy) != 1 || issue.BlockedBy[0].Identifier != "1" {
			t.Errorf("BlockedBy = %v, want one blocker with Identifier 1", issue.BlockedBy)
		}
		if len(issue.Comments) != 2 {
			t.Fatalf("Comments len = %d, want 2", len(issue.Comments))
		}
		if issue.Comments[0].Author != "alice" {
			t.Errorf("Comments[0].Author = %q, want %q", issue.Comments[0].Author, "alice")
		}
		if got := commentCalls.Load(); got != 1 {
			t.Errorf("comments request count = %d, want 1 (unpaginated, single request)", got)
		}
	})

	t.Run("not found", func(t *testing.T) {
		t.Parallel()

		mux := newPreflightMux(t)
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues/{index}", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			w.Write(loadFixture(t, "error_404.json")) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, mux)

		_, err := a.FetchIssueByID(context.Background(), "999")
		assertTrackerErrorKind(t, err, domain.ErrTrackerNotFound)
	})

	t.Run("pull request index maps to not found", func(t *testing.T) {
		t.Parallel()

		mux := newPreflightMux(t)
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues/{index}", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "issue_pr.json")) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, mux)

		_, err := a.FetchIssueByID(context.Background(), "6")
		assertTrackerErrorKind(t, err, domain.ErrTrackerNotFound)
	})
}

// --- FetchIssueComments ---

func TestFetchIssueComments(t *testing.T) {
	t.Parallel()

	t.Run("single unpaginated request returns comments in order", func(t *testing.T) {
		t.Parallel()

		mux := newPreflightMux(t)
		var calls atomic.Int32
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues/42/comments", func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			if r.URL.RawQuery != "" {
				t.Errorf("unexpected query string %q on the unpaginated comments route", r.URL.RawQuery)
			}
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "comments_two.json")) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, mux)

		comments, err := a.FetchIssueComments(context.Background(), "42")
		if err != nil {
			t.Fatalf("FetchIssueComments: %v", err)
		}
		if len(comments) != 2 {
			t.Fatalf("len = %d, want 2", len(comments))
		}
		if comments[0].Author != "alice" || comments[1].Author != "bob" {
			t.Errorf("authors = [%s %s], want [alice bob]", comments[0].Author, comments[1].Author)
		}
		if got := calls.Load(); got != 1 {
			t.Errorf("request count = %d, want 1 (comments route must never be followed for a second page)", got)
		}
	})

	t.Run("60-comment response returns all comments from one request", func(t *testing.T) {
		t.Parallel()

		bulk := buildBulkComments(t, 60)
		mux := newPreflightMux(t)
		var calls atomic.Int32
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues/5/comments", func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set("X-Total-Count", "60")
			w.WriteHeader(http.StatusOK)
			w.Write(bulk) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, mux)

		comments, err := a.FetchIssueComments(context.Background(), "5")
		if err != nil {
			t.Fatalf("FetchIssueComments: %v", err)
		}
		if len(comments) != 60 {
			t.Fatalf("len = %d, want 60", len(comments))
		}
		if got := calls.Load(); got != 1 {
			t.Errorf("request count = %d, want 1 (no Link header, no pagination)", got)
		}
	})

	t.Run("empty is a non-nil slice", func(t *testing.T) {
		t.Parallel()

		mux := newPreflightMux(t)
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues/42/comments", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("[]")) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, mux)

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
	})

	t.Run("not found", func(t *testing.T) {
		t.Parallel()

		mux := newPreflightMux(t)
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues/999/comments", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			w.Write(loadFixture(t, "error_404.json")) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, mux)

		_, err := a.FetchIssueComments(context.Background(), "999")
		assertTrackerErrorKind(t, err, domain.ErrTrackerNotFound)
	})
}

// --- FetchIssueStatesByIDs / FetchIssueStatesByIdentifiers ---

func TestFetchIssueStatesByIDs(t *testing.T) {
	t.Parallel()

	t.Run("empty input returns empty map with no request", func(t *testing.T) {
		t.Parallel()

		a := mustAdapter(t, newPreflightMux(t))

		states, err := a.FetchIssueStatesByIDs(context.Background(), []string{})
		if err != nil {
			t.Fatalf("FetchIssueStatesByIDs: %v", err)
		}
		if states == nil {
			t.Fatal("states is nil, want non-nil empty map")
		}
		if len(states) != 0 {
			t.Errorf("len = %d, want 0", len(states))
		}
	})

	t.Run("404 and pull-request entries are omitted, found entries keep the derived state", func(t *testing.T) {
		t.Parallel()

		mux := newPreflightMux(t)
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues/{index}", func(w http.ResponseWriter, r *http.Request) {
			switch r.PathValue("index") {
			case "1":
				w.WriteHeader(http.StatusOK)
				w.Write(loadFixture(t, "issue_full.json")) //nolint:errcheck // test helper (in-progress label)
			case "6":
				w.WriteHeader(http.StatusOK)
				w.Write(loadFixture(t, "issue_pr.json")) //nolint:errcheck // test helper
			default:
				w.WriteHeader(http.StatusNotFound)
				w.Write(loadFixture(t, "error_404.json")) //nolint:errcheck // test helper
			}
		})
		a := mustAdapter(t, mux)

		states, err := a.FetchIssueStatesByIDs(context.Background(), []string{"1", "6", "999"})
		if err != nil {
			t.Fatalf("FetchIssueStatesByIDs: %v", err)
		}
		if got, want := states["1"], "in-progress"; got != want {
			t.Errorf(`states["1"] = %q, want %q`, got, want)
		}
		if _, ok := states["6"]; ok {
			t.Error(`states["6"] present, want omitted (pull request)`)
		}
		if _, ok := states["999"]; ok {
			t.Error(`states["999"] present, want omitted (not found)`)
		}
		if len(states) != 1 {
			t.Errorf("len = %d, want 1", len(states))
		}
	})

	t.Run("context cancellation between requests returns ctx.Err", func(t *testing.T) {
		t.Parallel()

		started := make(chan struct{})
		mux := newPreflightMux(t)
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues/{index}", func(w http.ResponseWriter, r *http.Request) {
			close(started)
			<-r.Context().Done()
		})
		a := mustAdapter(t, mux)

		ctx, cancel := context.WithCancel(context.Background())
		errCh := make(chan error, 1)
		go func() {
			_, err := a.FetchIssueStatesByIDs(ctx, []string{"1", "2"})
			errCh <- err
		}()

		<-started
		cancel()

		err := <-errCh
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want context.Canceled", err)
		}
	})
}

func TestFetchIssueStatesByIdentifiers(t *testing.T) {
	t.Parallel()

	// FetchIssueStatesByIdentifiers delegates to the same fetchStatesByIndexes
	// helper as FetchIssueStatesByIDs (ID and Identifier are both the index);
	// this proves the delegation rather than repeating every case above.
	mux := newPreflightMux(t)
	mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues/7", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"number":7,"state":"open","labels":[{"name":"review"}]}`)) //nolint:errcheck // test helper
	})
	a := mustAdapter(t, mux)

	states, err := a.FetchIssueStatesByIdentifiers(context.Background(), []string{"7"})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIdentifiers: %v", err)
	}
	if got, want := states["7"], "review"; got != want {
		t.Errorf(`states["7"] = %q, want %q`, got, want)
	}
}

// --- fetchBlockers ---

func TestFetchBlockers(t *testing.T) {
	t.Parallel()

	t.Run("extracts blockers from the dependencies route", func(t *testing.T) {
		t.Parallel()

		mux := newPreflightMux(t)
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues/42/dependencies", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "dependencies_one.json")) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, mux)

		blockers, err := a.fetchBlockers(context.Background(), "42")
		if err != nil {
			t.Fatalf("fetchBlockers: %v", err)
		}
		if len(blockers) != 1 {
			t.Fatalf("len = %d, want 1", len(blockers))
		}
		if blockers[0].ID != "1" || blockers[0].Identifier != "1" {
			t.Errorf("blockers[0] ID/Identifier = %q/%q, want 1/1", blockers[0].ID, blockers[0].Identifier)
		}
		if blockers[0].State != "in-progress" {
			t.Errorf("blockers[0].State = %q, want %q", blockers[0].State, "in-progress")
		}
	})

	t.Run("404 is tolerated and returns a non-nil empty slice", func(t *testing.T) {
		t.Parallel()

		mux := newPreflightMux(t)
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues/42/dependencies", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			w.Write(loadFixture(t, "error_404.json")) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, mux)

		blockers, err := a.fetchBlockers(context.Background(), "42")
		if err != nil {
			t.Fatalf("fetchBlockers should tolerate 404: %v", err)
		}
		if blockers == nil {
			t.Fatal("blockers is nil, want non-nil empty slice")
		}
		if len(blockers) != 0 {
			t.Errorf("len = %d, want 0", len(blockers))
		}
	})
}

// --- Write-path stubs ---

func TestWriteStubs(t *testing.T) {
	t.Parallel()

	t.Run("TransitionIssue", func(t *testing.T) {
		t.Parallel()

		a := mustAdapter(t, newPreflightMux(t)) // no write routes registered; any call fails the test
		err := a.TransitionIssue(context.Background(), "1", "done")
		assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)
	})

	t.Run("CommentIssue", func(t *testing.T) {
		t.Parallel()

		a := mustAdapter(t, newPreflightMux(t))
		err := a.CommentIssue(context.Background(), "1", "hello")
		assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)
	})

	t.Run("AddLabel", func(t *testing.T) {
		t.Parallel()

		a := mustAdapter(t, newPreflightMux(t))
		err := a.AddLabel(context.Background(), "1", "bug")
		assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)
	})
}
