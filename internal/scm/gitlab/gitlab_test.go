package gitlab

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
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

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

const testProject = "acme/widgets"

var testEscapedProject = url.PathEscape(testProject)

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return data
}

func validConfig(project string) map[string]any {
	return map[string]any{
		"api_key": "test-token",
		"project": project,
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

// assertQueryParams asserts got holds exactly the key/value pairs in want,
// with no extra or missing keys, so a single call proves both that the
// expected params merged in and that no unexpected param leaked in. label
// identifies the request in a failure message.
func assertQueryParams(t *testing.T, label string, got url.Values, want map[string]string) {
	t.Helper()
	wantValues := make(url.Values, len(want))
	for key, val := range want {
		wantValues[key] = []string{val}
	}
	if !maps.EqualFunc(got, wantValues, slices.Equal) {
		t.Errorf("%s request query = %v, want exactly %v", label, got, wantValues)
	}
}

// fakeServer is a minimal exact-match HTTP router keyed on method and the
// escaped request path. A percent-encoded project segment (%2F) decodes to
// a literal slash in [http.Request.URL.Path], which defeats [http.ServeMux]
// pattern matching on a route containing a multi-segment project ID;
// matching on [url.URL.EscapedPath] instead preserves the encoding exactly
// as GitLab requires it. Any request with no matching route fails the test
// loudly instead of receiving a silent 404, so a stray or unexpected call
// (a write stub issuing a request, a second page beyond what a test
// expects) is caught.
type fakeServer struct {
	t      *testing.T
	routes map[string]http.HandlerFunc
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	return &fakeServer{t: t, routes: make(map[string]http.HandlerFunc)}
}

// handle registers fn for a GET request against escapedPath. Every route
// this suite exercises is a GET; a non-GET request (a write stub issuing a
// request it must not issue) falls through to ServeHTTP's unmatched-route
// failure regardless.
func (s *fakeServer) handle(escapedPath string, fn http.HandlerFunc) {
	s.routes[http.MethodGet+" "+escapedPath] = fn
}

func (s *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := r.Method + " " + r.URL.EscapedPath()
	if fn, ok := s.routes[key]; ok {
		fn(w, r)
		return
	}
	s.t.Errorf("unexpected request: %s", key)
	w.WriteHeader(http.StatusNotFound)
}

// withPreflight registers the two construction-preflight routes for
// project on s. Every test that constructs an adapter must install both
// before the operation under test, because [NewGitLabAdapter] runs the
// preflight synchronously.
func withPreflight(t *testing.T, s *fakeServer, project string) {
	t.Helper()
	escaped := url.PathEscape(project)
	s.handle("/api/v4/personal_access_tokens/self", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(loadFixture(t, "token_self.json")) //nolint:errcheck // test helper
	})
	s.handle("/api/v4/projects/"+escaped, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`)) //nolint:errcheck // test helper
	})
}

// newPreflightServer returns a [fakeServer] pre-registered with the two
// construction-preflight routes for [testProject]. Callers register only
// the routes their operation under test exercises.
func newPreflightServer(t *testing.T) *fakeServer {
	t.Helper()
	s := newFakeServer(t)
	withPreflight(t, s, testProject)
	return s
}

// mustAdapter starts an httptest.Server for s and constructs a
// [*GitLabAdapter] against it for [testProject], registering server
// cleanup. s must already carry the two construction-preflight routes; see
// [newPreflightServer].
func mustAdapter(t *testing.T, s *fakeServer) *GitLabAdapter {
	t.Helper()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)

	cfg := validConfig(testProject)
	cfg["endpoint"] = srv.URL
	a, err := NewGitLabAdapter(cfg)
	if err != nil {
		t.Fatalf("NewGitLabAdapter: %v", err)
	}
	return a.(*GitLabAdapter)
}

// loweredStates returns a lowercased copy of states, matching the
// normalization NewGitLabAdapter applies to every state list element.
func loweredStates(states []string) []string {
	out := make([]string, len(states))
	for i, s := range states {
		out[i] = strings.ToLower(s)
	}
	return out
}

// --- parseIID ---

func TestParseIID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		raw    string
		want   int
		wantOK bool
	}{
		{"simple positive", "42", 42, true},
		{"leading and trailing whitespace trimmed", "  7  ", 7, true},
		{"large value accepted", "999999", 999999, true},
		{"zero rejected", "0", 0, false},
		{"zero-padded rejected", "007", 0, false},
		{"negative rejected", "-1", 0, false},
		{"fractional rejected", "1.5", 0, false},
		{"non-numeric rejected", "abc", 0, false},
		{"whitespace-only rejected", " ", 0, false},
		{"empty rejected", "", 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := parseIID(tt.raw)
			if ok != tt.wantOK {
				t.Fatalf("parseIID(%q) ok = %v, want %v", tt.raw, ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("parseIID(%q) = %d, want %d", tt.raw, got, tt.want)
			}
		})
	}
}

// --- Constructor ---

func TestNewGitLabAdapter(t *testing.T) {
	t.Parallel()

	t.Run("config errors fail before any network call", func(t *testing.T) {
		t.Parallel()

		// endpoint points at a refused local port so an incorrect
		// reordering of the validation checks in NewGitLabAdapter would
		// fail this test with a network error instead of silently passing.
		const unreachable = "http://127.0.0.1:1"

		tests := []struct {
			name     string
			config   map[string]any
			wantKind domain.TrackerErrorKind
		}{
			{"missing api_key", map[string]any{"project": testProject, "endpoint": unreachable}, domain.ErrMissingTrackerAPIKey},
			{"empty api_key", map[string]any{"api_key": "", "project": testProject, "endpoint": unreachable}, domain.ErrMissingTrackerAPIKey},
			{"missing project", map[string]any{"api_key": "tok", "endpoint": unreachable}, domain.ErrMissingTrackerProject},
			{"empty project", map[string]any{"api_key": "tok", "project": "", "endpoint": unreachable}, domain.ErrMissingTrackerProject},
			{"non-empty query_filter rejected", map[string]any{"api_key": "tok", "project": testProject, "query_filter": "scope=assigned_to_me", "endpoint": unreachable}, domain.ErrTrackerPayload},
			{"malformed endpoint scheme", map[string]any{"api_key": "tok", "project": testProject, "endpoint": "ftp://example.com"}, domain.ErrTrackerPayload},
			{"malformed endpoint no host", map[string]any{"api_key": "tok", "project": testProject, "endpoint": "http://"}, domain.ErrTrackerPayload},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				a, err := NewGitLabAdapter(tt.config)
				assertTrackerErrorKind(t, err, tt.wantKind)
				if a != nil {
					t.Error("adapter should be nil on error")
				}
			})
		}
	})

	// validConfig omits both state keys, so the constructed adapter holds
	// the fallback lists. They must stay the ones the registration
	// declares, because the dispatch preflight rules on the declared lists.
	t.Run("successful construction applies default state lists", func(t *testing.T) {
		t.Parallel()

		meta, ok := registry.Trackers.Meta("gitlab")
		if !ok {
			t.Fatal(`Trackers.Meta("gitlab") reported not registered`)
		}
		if len(meta.DefaultActiveStates) == 0 || len(meta.DefaultTerminalStates) == 0 {
			t.Fatalf("Trackers.Meta(%q) declares DefaultActiveStates = %v, DefaultTerminalStates = %v, want both non-empty",
				"gitlab", meta.DefaultActiveStates, meta.DefaultTerminalStates)
		}
		if !meta.RequiresProject || !meta.RequiresAPIKey {
			t.Errorf("Trackers.Meta(%q) RequiresProject = %v, RequiresAPIKey = %v, want both true", "gitlab", meta.RequiresProject, meta.RequiresAPIKey)
		}

		a := mustAdapter(t, newPreflightServer(t))

		if a.project != testProject {
			t.Errorf("project = %q, want %q", a.project, testProject)
		}
		if a.projectPath != testEscapedProject {
			t.Errorf("projectPath = %q, want %q", a.projectPath, testEscapedProject)
		}
		if want := loweredStates(meta.DefaultActiveStates); !slices.Equal(a.activeStates, want) {
			t.Errorf("activeStates = %v, want %v", a.activeStates, want)
		}
		if want := loweredStates(meta.DefaultTerminalStates); !slices.Equal(a.terminalStates, want) {
			t.Errorf("terminalStates = %v, want %v", a.terminalStates, want)
		}
		if a.handoffState != "" {
			t.Errorf("handoffState = %q, want empty", a.handoffState)
		}
	})

	t.Run("empty query_filter constructs normally", func(t *testing.T) {
		t.Parallel()

		s := newPreflightServer(t)
		srv := httptest.NewServer(s)
		defer srv.Close()

		cfg := validConfig(testProject)
		cfg["endpoint"] = srv.URL
		cfg["query_filter"] = ""

		a, err := NewGitLabAdapter(cfg)
		if err != nil {
			t.Fatalf("NewGitLabAdapter(query_filter=\"\"): %v", err)
		}
		if a == nil {
			t.Fatal("adapter is nil, want a constructed adapter")
		}
	})

	t.Run("api/v4 suffix is appended exactly once regardless of source form", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name  string
			build func(base string) string
		}{
			{"bare host", func(base string) string { return base }},
			{"trailing slash", func(base string) string { return base + "/" }},
			{"already suffixed", func(base string) string { return base + "/api/v4" }},
			{"already suffixed with trailing slash", func(base string) string { return base + "/api/v4/" }},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				s := newPreflightServer(t)
				srv := httptest.NewServer(s)
				defer srv.Close()

				cfg := validConfig(testProject)
				cfg["endpoint"] = tt.build(srv.URL)
				if _, err := NewGitLabAdapter(cfg); err != nil {
					t.Fatalf("NewGitLabAdapter(endpoint=%q): %v", cfg["endpoint"], err)
				}
			})
		}
	})

	t.Run("custom state lists are lowercased and handoff_state is trimmed", func(t *testing.T) {
		t.Parallel()

		s := newPreflightServer(t)
		srv := httptest.NewServer(s)
		defer srv.Close()

		cfg := validConfig(testProject)
		cfg["endpoint"] = srv.URL
		cfg["active_states"] = []any{"Todo", "IN-PROGRESS"}
		cfg["terminal_states"] = []string{"Done", "WONTFIX"}
		cfg["handoff_state"] = "  Review  "

		a, err := NewGitLabAdapter(cfg)
		if err != nil {
			t.Fatalf("NewGitLabAdapter: %v", err)
		}
		ga := a.(*GitLabAdapter)

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

		s := newPreflightServer(t)
		srv := httptest.NewServer(s)
		defer srv.Close()

		cfg := validConfig(testProject)
		cfg["endpoint"] = srv.URL
		cfg["active_states"] = []string{"InProgress", "Review"}
		cfg["terminal_states"] = []string{"Done", "WontFix"}

		if _, err := NewGitLabAdapter(cfg); err != nil {
			t.Fatalf("NewGitLabAdapter: %v", err)
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

		s := newFakeServer(t)
		s.handle("/api/v4/personal_access_tokens/self", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "token_self.json")) //nolint:errcheck // test helper
		})
		s.handle("/api/v4/projects/"+testEscapedProject, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write(loadFixture(t, "error_401.json")) //nolint:errcheck // test helper
		})
		srv := httptest.NewServer(s)
		defer srv.Close()

		cfg := validConfig(testProject)
		cfg["endpoint"] = srv.URL

		a, err := NewGitLabAdapter(cfg)
		assertTrackerErrorKind(t, err, domain.ErrTrackerAuth)
		if a != nil {
			t.Error("adapter should be nil when the preflight fails")
		}
	})
}

// --- Percent-encoded project path ---

func TestProjectPathPercentEncoding(t *testing.T) {
	t.Parallel()

	t.Run("nested subgroup produces two %2F and no bare slash in the project segment", func(t *testing.T) {
		t.Parallel()

		const nestedProject = "acme/subgroup/widgets"
		escaped := url.PathEscape(nestedProject)

		s := newFakeServer(t)
		var gotPath string
		s.handle("/api/v4/personal_access_tokens/self", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "token_self.json")) //nolint:errcheck // test helper
		})
		s.handle("/api/v4/projects/"+escaped, func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.EscapedPath()
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`)) //nolint:errcheck // test helper
		})
		srv := httptest.NewServer(s)
		defer srv.Close()

		cfg := validConfig(nestedProject)
		cfg["endpoint"] = srv.URL
		if _, err := NewGitLabAdapter(cfg); err != nil {
			t.Fatalf("NewGitLabAdapter(project=%q): %v", nestedProject, err)
		}

		if gotPath == "" {
			t.Fatal("project route was never called")
		}
		if got := strings.Count(gotPath, "%2F"); got != 2 {
			t.Errorf("request path %q carries %d occurrences of %%2F, want 2", gotPath, got)
		}
		suffix := strings.TrimPrefix(gotPath, "/api/v4/projects/")
		if strings.Contains(suffix, "/") {
			t.Errorf("project segment %q contains a bare slash, want none (must stay percent-encoded)", suffix)
		}
	})

	t.Run("single-segment project produces exactly one %2F", func(t *testing.T) {
		t.Parallel()

		a := mustAdapter(t, newPreflightServer(t))
		if got := strings.Count(a.projectPath, "%2F"); got != 1 {
			t.Errorf("projectPath = %q carries %d occurrences of %%2F, want 1", a.projectPath, got)
		}
	})

	t.Run("numeric project ID requires no percent-encoding and still constructs", func(t *testing.T) {
		t.Parallel()

		const numericProject = "778899"

		s := newFakeServer(t)
		withPreflight(t, s, numericProject)
		srv := httptest.NewServer(s)
		defer srv.Close()

		cfg := validConfig(numericProject)
		cfg["endpoint"] = srv.URL
		a, err := NewGitLabAdapter(cfg)
		if err != nil {
			t.Fatalf("NewGitLabAdapter(project=%q): %v", numericProject, err)
		}
		ga := a.(*GitLabAdapter)
		if ga.projectPath != numericProject {
			t.Errorf("projectPath = %q, want %q", ga.projectPath, numericProject)
		}
	})
}

// --- Registration ---

func TestGitLabAdapterRegistration(t *testing.T) {
	t.Parallel()

	if !registry.Trackers.Has("gitlab") {
		t.Fatal(`Trackers.Has("gitlab") = false, want true`)
	}
	if _, err := registry.Trackers.Get("gitlab"); err != nil {
		t.Errorf(`Trackers.Get("gitlab") = %v, want registered constructor`, err)
	}
}

// --- paginateIssues ---

func TestPaginateIssues(t *testing.T) {
	t.Parallel()

	t.Run("follows Link headers across pages, ending on rel=first/rel=last with no rel=next", func(t *testing.T) {
		t.Parallel()

		page1 := loadFixture(t, "issues_paginate_page1.json") // 2 items
		page2 := loadFixture(t, "issues_paginate_page2.json") // 2 items
		page3 := loadFixture(t, "issues_paginate_page3.json") // 1 item

		s := newPreflightServer(t)
		var srvURL string
		var calls atomic.Int32
		basePath := "/api/v4/projects/" + testEscapedProject + "/issues"
		s.handle(basePath, func(w http.ResponseWriter, r *http.Request) {
			n := calls.Add(1)
			first := fmt.Sprintf(`<%s%s?page=1>; rel="first"`, srvURL, basePath)
			last := fmt.Sprintf(`<%s%s?page=3>; rel="last"`, srvURL, basePath)
			switch n {
			case 1:
				next := fmt.Sprintf(`<%s%s?page=2>; rel="next"`, srvURL, basePath)
				w.Header().Set("Link", strings.Join([]string{next, first, last}, ", "))
				w.WriteHeader(http.StatusOK)
				w.Write(page1) //nolint:errcheck // test helper
			case 2:
				next := fmt.Sprintf(`<%s%s?page=3>; rel="next"`, srvURL, basePath)
				w.Header().Set("Link", strings.Join([]string{next, first, last}, ", "))
				w.WriteHeader(http.StatusOK)
				w.Write(page2) //nolint:errcheck // test helper
			default:
				w.Header().Set("Link", strings.Join([]string{first, last}, ", "))
				w.WriteHeader(http.StatusOK)
				w.Write(page3) //nolint:errcheck // test helper
			}
		})
		srv := httptest.NewServer(s)
		defer srv.Close()
		srvURL = srv.URL

		cfg := validConfig(testProject)
		cfg["endpoint"] = srv.URL
		a, err := NewGitLabAdapter(cfg)
		if err != nil {
			t.Fatalf("NewGitLabAdapter: %v", err)
		}
		ga := a.(*GitLabAdapter)

		items, err := ga.paginateIssues(context.Background(), "/projects/"+testEscapedProject+"/issues", url.Values{"per_page": {"100"}})
		if err != nil {
			t.Fatalf("paginateIssues: %v", err)
		}
		if len(items) != 5 {
			t.Fatalf("len = %d, want 5 across 3 pages", len(items))
		}
		if got := calls.Load(); got != 3 {
			t.Errorf("call count = %d, want 3 (one per page)", got)
		}
	})

	t.Run("single page with no Link header stops after one request", func(t *testing.T) {
		t.Parallel()

		var calls atomic.Int32
		s := newPreflightServer(t)
		s.handle("/api/v4/projects/"+testEscapedProject+"/issues", func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "issues_paginate_page3.json")) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, s)

		items, err := a.paginateIssues(context.Background(), "/projects/"+testEscapedProject+"/issues", url.Values{"per_page": {"100"}})
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

	t.Run("decode failure returns ErrTrackerPayload", func(t *testing.T) {
		t.Parallel()

		s := newPreflightServer(t)
		s.handle("/api/v4/projects/"+testEscapedProject+"/issues", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`not json`)) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, s)

		_, err := a.paginateIssues(context.Background(), "/projects/"+testEscapedProject+"/issues", nil)
		assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)
	})
}

// --- paginateNotes ---

func TestPaginateNotes(t *testing.T) {
	t.Parallel()

	t.Run("follows Link headers across pages accumulating raw notes", func(t *testing.T) {
		t.Parallel()

		page1 := loadFixture(t, "notes_page1.json") // 2 notes
		page2 := loadFixture(t, "notes_page2.json") // 2 notes, final

		s := newPreflightServer(t)
		var srvURL string
		var calls atomic.Int32
		basePath := "/api/v4/projects/" + testEscapedProject + "/issues/42/notes"
		s.handle(basePath, func(w http.ResponseWriter, r *http.Request) {
			n := calls.Add(1)
			first := fmt.Sprintf(`<%s%s?page=1>; rel="first"`, srvURL, basePath)
			last := fmt.Sprintf(`<%s%s?page=2>; rel="last"`, srvURL, basePath)
			if n == 1 {
				next := fmt.Sprintf(`<%s%s?page=2>; rel="next"`, srvURL, basePath)
				w.Header().Set("Link", strings.Join([]string{next, first, last}, ", "))
				w.WriteHeader(http.StatusOK)
				w.Write(page1) //nolint:errcheck // test helper
				return
			}
			w.Header().Set("Link", strings.Join([]string{first, last}, ", "))
			w.WriteHeader(http.StatusOK)
			w.Write(page2) //nolint:errcheck // test helper
		})
		srv := httptest.NewServer(s)
		defer srv.Close()
		srvURL = srv.URL

		cfg := validConfig(testProject)
		cfg["endpoint"] = srv.URL
		a, err := NewGitLabAdapter(cfg)
		if err != nil {
			t.Fatalf("NewGitLabAdapter: %v", err)
		}
		ga := a.(*GitLabAdapter)

		notes, err := ga.paginateNotes(context.Background(), "/projects/"+testEscapedProject+"/issues/42/notes", url.Values{"per_page": {"100"}})
		if err != nil {
			t.Fatalf("paginateNotes: %v", err)
		}
		if len(notes) != 4 {
			t.Fatalf("len = %d, want 4 raw notes across 2 pages (before system-note filtering)", len(notes))
		}
		if got := calls.Load(); got != 2 {
			t.Errorf("call count = %d, want 2 (one per page)", got)
		}
	})
}

// --- FetchCandidateIssues ---

func TestFetchCandidateIssues(t *testing.T) {
	t.Parallel()

	t.Run("task filter, state derivation, terminal filter, and preserved order", func(t *testing.T) {
		t.Parallel()

		page1 := loadFixture(t, "issues_candidates_page1.json") // iid 8 (backlog), iid 6 (task, filtered)
		page2 := loadFixture(t, "issues_candidates_page2.json") // iid 1,3,2,5,4,7

		s := newPreflightServer(t)
		var srvURL string
		var calls atomic.Int32
		basePath := "/api/v4/projects/" + testEscapedProject + "/issues"
		s.handle(basePath, func(w http.ResponseWriter, r *http.Request) {
			n := calls.Add(1)
			if n == 1 {
				assertQueryParams(t, "candidate", r.URL.Query(), map[string]string{
					"state": "opened", "issue_type": "issue", "scope": "all",
					"per_page": "100", "order_by": "created_at", "sort": "asc",
				})
				w.Header().Set("Link", fmt.Sprintf(`<%s%s?page=2>; rel="next", <%s%s?page=1>; rel="first", <%s%s?page=2>; rel="last"`, srvURL, basePath, srvURL, basePath, srvURL, basePath))
				w.WriteHeader(http.StatusOK)
				w.Write(page1) //nolint:errcheck // test helper
				return
			}
			w.Header().Set("Link", fmt.Sprintf(`<%s%s?page=1>; rel="first", <%s%s?page=2>; rel="last"`, srvURL, basePath, srvURL, basePath))
			w.WriteHeader(http.StatusOK)
			w.Write(page2) //nolint:errcheck // test helper
		})
		srv := httptest.NewServer(s)
		defer srv.Close()
		srvURL = srv.URL

		cfg := validConfig(testProject)
		cfg["endpoint"] = srv.URL
		a, err := NewGitLabAdapter(cfg)
		if err != nil {
			t.Fatalf("NewGitLabAdapter: %v", err)
		}
		ga := a.(*GitLabAdapter)

		var buf bytes.Buffer
		ga.log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		issues, err := ga.FetchCandidateIssues(context.Background())
		if err != nil {
			t.Fatalf("FetchCandidateIssues: %v", err)
		}

		// iid 6 (task) is filtered before normalization; iid 3 and iid 4
		// (both terminal, one from an explicit "done" label and one from
		// the closed-native-state fallback) are filtered by the active-keep
		// set; the remaining five are returned in server order.
		wantOrder := []string{"8", "1", "2", "5", "7"}
		if len(issues) != len(wantOrder) {
			t.Fatalf("len = %d, want %d: got identifiers %v", len(issues), len(wantOrder), identifiersOf(issues))
		}
		for i, want := range wantOrder {
			if issues[i].Identifier != want {
				t.Errorf("issues[%d].Identifier = %q, want %q", i, issues[i].Identifier, want)
			}
		}
		for _, iss := range issues {
			if iss.Comments != nil {
				t.Errorf("issue %s: Comments = %v, want nil", iss.Identifier, iss.Comments)
			}
		}
		if got := calls.Load(); got != 2 {
			t.Errorf("call count = %d, want 2 (two pages)", got)
		}
		if got := strings.Count(buf.String(), "kept first of multiple matching state labels"); got != 1 {
			t.Errorf("multi-match WARN count = %d, want 1 (iid 7 carries both backlog and done labels)\noutput: %s", got, buf.String())
		}
		if !strings.Contains(buf.String(), "iid=7") {
			t.Errorf("multi-match WARN missing iid=7\noutput: %s", buf.String())
		}
	})

	t.Run("qualifies DisplayID when references.full is empty", func(t *testing.T) {
		t.Parallel()

		s := newPreflightServer(t)
		s.handle("/api/v4/projects/"+testEscapedProject+"/issues", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "issues_candidates_page2.json")) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, s)

		issues, err := a.FetchCandidateIssues(context.Background())
		if err != nil {
			t.Fatalf("FetchCandidateIssues: %v", err)
		}
		if len(issues) == 0 {
			t.Fatal("issues is empty, want at least one active issue")
		}
		for _, iss := range issues {
			want := testProject + "#" + iss.Identifier
			if iss.DisplayID != want {
				t.Errorf("issue %s: DisplayID = %q, want %q", iss.Identifier, iss.DisplayID, want)
			}
		}
	})

	t.Run("empty response returns non-nil empty slice", func(t *testing.T) {
		t.Parallel()

		s := newPreflightServer(t)
		s.handle("/api/v4/projects/"+testEscapedProject+"/issues", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("[]")) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, s)

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

		s := newPreflightServer(t)
		s.handle("/api/v4/projects/"+testEscapedProject+"/issues", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write(loadFixture(t, "error_401.json")) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, s)

		_, err := a.FetchCandidateIssues(context.Background())
		assertTrackerErrorKind(t, err, domain.ErrTrackerAuth)
	})
}

func identifiersOf(issues []domain.Issue) []string {
	out := make([]string, len(issues))
	for i, iss := range issues {
		out[i] = iss.Identifier
	}
	return out
}

// --- FetchIssuesByStates ---

func TestFetchIssuesByStates(t *testing.T) {
	t.Parallel()

	t.Run("empty states short-circuits with no API call", func(t *testing.T) {
		t.Parallel()

		a := mustAdapter(t, newPreflightServer(t)) // no issues route registered; any call fails the test

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

	t.Run("requesting only an active state queries the opened listing only", func(t *testing.T) {
		t.Parallel()

		s := newPreflightServer(t)
		var calls atomic.Int32
		s.handle("/api/v4/projects/"+testEscapedProject+"/issues", func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			switch state := r.URL.Query().Get("state"); state {
			case "opened":
				w.WriteHeader(http.StatusOK)
				w.Write(loadFixture(t, "issues_open_by_states.json")) //nolint:errcheck // test helper
			default:
				t.Errorf("unexpected state query param: %q", state)
				w.WriteHeader(http.StatusBadRequest)
			}
		})
		a := mustAdapter(t, s)

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
		if got := calls.Load(); got != 1 {
			t.Errorf("call count = %d, want 1 (exactly one opened-state query)", got)
		}
	})

	t.Run("requesting only a terminal state queries the closed listing only", func(t *testing.T) {
		t.Parallel()

		s := newPreflightServer(t)
		var calls atomic.Int32
		s.handle("/api/v4/projects/"+testEscapedProject+"/issues", func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			switch state := r.URL.Query().Get("state"); state {
			case "closed":
				w.WriteHeader(http.StatusOK)
				w.Write(loadFixture(t, "issues_closed_by_states.json")) //nolint:errcheck // test helper
			default:
				t.Errorf("unexpected state query param: %q", state)
				w.WriteHeader(http.StatusBadRequest)
			}
		})
		a := mustAdapter(t, s)

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
		if got := calls.Load(); got != 1 {
			t.Errorf("call count = %d, want 1 (exactly one closed-state query)", got)
		}
	})

	t.Run("mixed request queries both the opened and closed listings", func(t *testing.T) {
		t.Parallel()

		s := newPreflightServer(t)
		var gotOpened, gotClosed atomic.Bool
		s.handle("/api/v4/projects/"+testEscapedProject+"/issues", func(w http.ResponseWriter, r *http.Request) {
			switch state := r.URL.Query().Get("state"); state {
			case "opened":
				gotOpened.Store(true)
				w.WriteHeader(http.StatusOK)
				w.Write(loadFixture(t, "issues_open_by_states.json")) //nolint:errcheck // test helper
			case "closed":
				gotClosed.Store(true)
				w.WriteHeader(http.StatusOK)
				w.Write(loadFixture(t, "issues_closed_by_states.json")) //nolint:errcheck // test helper
			default:
				t.Errorf("unexpected state query param: %q", state)
				w.WriteHeader(http.StatusBadRequest)
			}
		})
		a := mustAdapter(t, s)

		issues, err := a.FetchIssuesByStates(context.Background(), []string{"in-progress", "done"})
		if err != nil {
			t.Fatalf("FetchIssuesByStates: %v", err)
		}
		if len(issues) != 2 {
			t.Fatalf("len = %d, want 2 (one from each listing)", len(issues))
		}
		if !gotOpened.Load() {
			t.Error("opened state was not queried")
		}
		if !gotClosed.Load() {
			t.Error("closed state was not queried")
		}
	})
}

// --- FetchIssueByID ---

func TestFetchIssueByID(t *testing.T) {
	t.Parallel()

	t.Run("full population", func(t *testing.T) {
		t.Parallel()

		s := newPreflightServer(t)
		s.handle("/api/v4/projects/"+testEscapedProject+"/issues/42", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "issue_full.json")) //nolint:errcheck // test helper
		})
		var commentCalls atomic.Int32
		s.handle("/api/v4/projects/"+testEscapedProject+"/issues/42/notes", func(w http.ResponseWriter, r *http.Request) {
			commentCalls.Add(1)
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "notes_page1.json")) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, s)

		issue, err := a.FetchIssueByID(context.Background(), "42")
		if err != nil {
			t.Fatalf("FetchIssueByID: %v", err)
		}

		if issue.ID != "42" || issue.Identifier != "42" {
			t.Errorf("ID/Identifier = %q/%q, want 42/42", issue.ID, issue.Identifier)
		}
		if issue.Parent != nil {
			t.Errorf("Parent = %v, want nil", issue.Parent)
		}
		if len(issue.BlockedBy) != 0 {
			t.Errorf("BlockedBy = %v, want empty", issue.BlockedBy)
		}
		if len(issue.Comments) != 1 {
			t.Fatalf("Comments len = %d, want 1 (the system note dropped from notes_page1.json)", len(issue.Comments))
		}
		if issue.Comments[0].Author != "alice" {
			t.Errorf("Comments[0].Author = %q, want %q", issue.Comments[0].Author, "alice")
		}
		if got := commentCalls.Load(); got != 1 {
			t.Errorf("comments request count = %d, want 1", got)
		}
	})

	t.Run("not found", func(t *testing.T) {
		t.Parallel()

		s := newPreflightServer(t)
		s.handle("/api/v4/projects/"+testEscapedProject+"/issues/999", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			w.Write(loadFixture(t, "error_404_issue.json")) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, s)

		_, err := a.FetchIssueByID(context.Background(), "999")
		assertTrackerErrorKind(t, err, domain.ErrTrackerNotFound)
	})

	t.Run("non-issue entity maps to not found", func(t *testing.T) {
		t.Parallel()

		s := newPreflightServer(t)
		s.handle("/api/v4/projects/"+testEscapedProject+"/issues/100", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "issue_incident.json")) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, s)

		_, err := a.FetchIssueByID(context.Background(), "100")
		assertTrackerErrorKind(t, err, domain.ErrTrackerNotFound)
	})

	t.Run("identifier guard rejects malformed input with no request issued", func(t *testing.T) {
		t.Parallel()

		tests := []string{"abc", "1.5", "-1", "0", " ", ""}

		for _, raw := range tests {
			t.Run(fmt.Sprintf("issueID=%q", raw), func(t *testing.T) {
				t.Parallel()

				a := mustAdapter(t, newPreflightServer(t)) // no issues route registered; any call fails the test

				_, err := a.FetchIssueByID(context.Background(), raw)
				assertTrackerErrorKind(t, err, domain.ErrTrackerNotFound)
			})
		}
	})

	t.Run("accepted value reaches the server as a canonical decimal path segment", func(t *testing.T) {
		t.Parallel()

		s := newPreflightServer(t)
		s.handle("/api/v4/projects/"+testEscapedProject+"/issues/42", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "issue_full.json")) //nolint:errcheck // test helper
		})
		s.handle("/api/v4/projects/"+testEscapedProject+"/issues/42/notes", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("[]")) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, s)

		if _, err := a.FetchIssueByID(context.Background(), "  42  "); err != nil {
			t.Fatalf("FetchIssueByID(%q): %v (whitespace must be trimmed to a canonical decimal segment)", "  42  ", err)
		}
	})
}

// --- FetchIssueComments ---

func TestFetchIssueComments(t *testing.T) {
	t.Parallel()

	t.Run("multi-page notes: system dropped, internal retained, ascending order", func(t *testing.T) {
		t.Parallel()

		s := newPreflightServer(t)
		var srvURL string
		var calls atomic.Int32
		basePath := "/api/v4/projects/" + testEscapedProject + "/issues/55/notes"
		s.handle(basePath, func(w http.ResponseWriter, r *http.Request) {
			n := calls.Add(1)
			if n == 1 {
				// Only the first request carries the base params; the
				// paginator follows subsequent pages via the absolute
				// Link-header URL, which does not repeat them.
				assertQueryParams(t, "notes", r.URL.Query(), map[string]string{
					"activity_filter": "only_comments", "sort": "asc", "per_page": "100",
				})
			}
			first := fmt.Sprintf(`<%s%s?page=1>; rel="first"`, srvURL, basePath)
			last := fmt.Sprintf(`<%s%s?page=2>; rel="last"`, srvURL, basePath)
			if n == 1 {
				next := fmt.Sprintf(`<%s%s?page=2>; rel="next"`, srvURL, basePath)
				w.Header().Set("Link", strings.Join([]string{next, first, last}, ", "))
				w.WriteHeader(http.StatusOK)
				w.Write(loadFixture(t, "notes_page1.json")) //nolint:errcheck // test helper
				return
			}
			w.Header().Set("Link", strings.Join([]string{first, last}, ", "))
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "notes_page2.json")) //nolint:errcheck // test helper
		})
		srv := httptest.NewServer(s)
		defer srv.Close()
		srvURL = srv.URL

		cfg := validConfig(testProject)
		cfg["endpoint"] = srv.URL
		a, err := NewGitLabAdapter(cfg)
		if err != nil {
			t.Fatalf("NewGitLabAdapter: %v", err)
		}

		comments, err := a.(*GitLabAdapter).FetchIssueComments(context.Background(), "55")
		if err != nil {
			t.Fatalf("FetchIssueComments: %v", err)
		}
		if len(comments) != 3 {
			t.Fatalf("len = %d, want 3 (4 raw notes minus the one system note)", len(comments))
		}
		wantIDs := []string{"501", "503", "504"}
		for i, want := range wantIDs {
			if comments[i].ID != want {
				t.Errorf("comments[%d].ID = %q, want %q", i, comments[i].ID, want)
			}
		}
		if got := calls.Load(); got != 2 {
			t.Errorf("call count = %d, want 2 (one per page)", got)
		}
	})

	t.Run("empty result returns non-nil empty slice", func(t *testing.T) {
		t.Parallel()

		s := newPreflightServer(t)
		s.handle("/api/v4/projects/"+testEscapedProject+"/issues/7/notes", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("[]")) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, s)

		comments, err := a.FetchIssueComments(context.Background(), "7")
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

		s := newPreflightServer(t)
		s.handle("/api/v4/projects/"+testEscapedProject+"/issues/999/notes", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			w.Write(loadFixture(t, "error_404_issue.json")) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, s)

		_, err := a.FetchIssueComments(context.Background(), "999")
		assertTrackerErrorKind(t, err, domain.ErrTrackerNotFound)
	})

	t.Run("identifier guard rejects malformed input with no request issued", func(t *testing.T) {
		t.Parallel()

		tests := []string{"abc", "1.5", "-1", "0", " ", ""}

		for _, raw := range tests {
			t.Run(fmt.Sprintf("issueID=%q", raw), func(t *testing.T) {
				t.Parallel()

				a := mustAdapter(t, newPreflightServer(t)) // no notes route registered; any call fails the test

				_, err := a.FetchIssueComments(context.Background(), raw)
				assertTrackerErrorKind(t, err, domain.ErrTrackerNotFound)
			})
		}
	})
}

// --- FetchIssueStatesByIDs / FetchIssueStatesByIdentifiers ---

func TestFetchIssueStatesByIDs(t *testing.T) {
	t.Parallel()

	t.Run("empty input returns empty map with no request", func(t *testing.T) {
		t.Parallel()

		a := mustAdapter(t, newPreflightServer(t)) // no issues route registered; any call fails the test

		states, err := a.FetchIssueStatesByIDs(context.Background(), nil)
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

	t.Run("all-empty or unparseable values return empty map with no request", func(t *testing.T) {
		t.Parallel()

		a := mustAdapter(t, newPreflightServer(t)) // no issues route registered; any call fails the test

		states, err := a.FetchIssueStatesByIDs(context.Background(), []string{"", " ", "abc", "0", "-1", "1.5"})
		if err != nil {
			t.Fatalf("FetchIssueStatesByIDs: %v", err)
		}
		if len(states) != 0 {
			t.Errorf("len = %d, want 0 (no value survives the identifier filter)", len(states))
		}
	})

	t.Run("unrequested response entry ignored, requested-but-absent entry omitted", func(t *testing.T) {
		t.Parallel()

		s := newPreflightServer(t)
		s.handle("/api/v4/projects/"+testEscapedProject+"/issues", func(w http.ResponseWriter, r *http.Request) {
			query := r.URL.Query()
			if got, want := query.Get("state"), "all"; got != want {
				t.Errorf("state = %q, want %q", got, want)
			}
			if got, want := query.Get("scope"), "all"; got != want {
				t.Errorf("scope = %q, want %q", got, want)
			}
			if got, want := query.Get("per_page"), "100"; got != want {
				t.Errorf("per_page = %q, want %q", got, want)
			}
			gotIIDs := query["iids[]"]
			wantIIDs := []string{"10", "20", "30"}
			if !slices.Equal(gotIIDs, wantIIDs) {
				t.Errorf("iids[] = %v, want %v (one repeated value per requested iid, in request order)", gotIIDs, wantIIDs)
			}
			w.WriteHeader(http.StatusOK)
			body := `[{"iid": 10, "state": "opened", "issue_type": "issue", "labels": ["in-progress"]},` +
				`{"iid": 20, "state": "closed", "issue_type": "issue", "labels": ["done"]},` +
				`{"iid": 999, "state": "opened", "issue_type": "issue", "labels": ["backlog"]}]`
			w.Write([]byte(body)) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, s)

		states, err := a.FetchIssueStatesByIDs(context.Background(), []string{"10", "20", "30"})
		if err != nil {
			t.Fatalf("FetchIssueStatesByIDs: %v", err)
		}
		if len(states) != 2 {
			t.Fatalf("len = %d, want 2: got %v", len(states), states)
		}
		if states["10"] != "in-progress" {
			t.Errorf(`states["10"] = %q, want %q`, states["10"], "in-progress")
		}
		if states["20"] != "done" {
			t.Errorf(`states["20"] = %q, want %q`, states["20"], "done")
		}
		if _, ok := states["30"]; ok {
			t.Error(`states["30"] present, want absent (requested but not returned by the server)`)
		}
		if _, ok := states["999"]; ok {
			t.Error(`states["999"] present, want absent (returned by the server but never requested)`)
		}
	})

	t.Run("non-issue entity is filtered from the batch result", func(t *testing.T) {
		t.Parallel()

		s := newPreflightServer(t)
		s.handle("/api/v4/projects/"+testEscapedProject+"/issues", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			body := `[{"iid": 1, "state": "opened", "issue_type": "issue", "labels": ["backlog"]},` +
				`{"iid": 2, "state": "opened", "issue_type": "task", "labels": ["backlog"]}]`
			w.Write([]byte(body)) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, s)

		states, err := a.FetchIssueStatesByIDs(context.Background(), []string{"1", "2"})
		if err != nil {
			t.Fatalf("FetchIssueStatesByIDs: %v", err)
		}
		if len(states) != 1 {
			t.Fatalf("len = %d, want 1: got %v", len(states), states)
		}
		if _, ok := states["1"]; !ok {
			t.Error(`states["1"] absent, want present`)
		}
		if _, ok := states["2"]; ok {
			t.Error(`states["2"] present, want absent (issue_type "task" is not an issue)`)
		}
	})

	t.Run("a 60-value input chunks into exactly two requests at the boundary", func(t *testing.T) {
		t.Parallel()

		requested := make([]string, 60)
		for i := range requested {
			requested[i] = strconv.Itoa(i + 1)
		}

		var mu sync.Mutex
		var queries [][]string

		s := newPreflightServer(t)
		s.handle("/api/v4/projects/"+testEscapedProject+"/issues", func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			queries = append(queries, append([]string(nil), r.URL.Query()["iids[]"]...))
			mu.Unlock()

			items := make([]string, 0, len(r.URL.Query()["iids[]"]))
			for _, raw := range r.URL.Query()["iids[]"] {
				items = append(items, fmt.Sprintf(`{"iid": %s, "state": "opened", "issue_type": "issue", "labels": []}`, raw))
			}
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("[" + strings.Join(items, ",") + "]")) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, s)

		states, err := a.FetchIssueStatesByIDs(context.Background(), requested)
		if err != nil {
			t.Fatalf("FetchIssueStatesByIDs: %v", err)
		}
		if len(states) != 60 {
			t.Fatalf("len = %d, want 60", len(states))
		}

		mu.Lock()
		defer mu.Unlock()
		if len(queries) != 2 {
			t.Fatalf("request count = %d, want 2 (60 values chunked at 50)", len(queries))
		}
		if len(queries[0]) != 50 {
			t.Errorf("first chunk size = %d, want 50", len(queries[0]))
		}
		if len(queries[1]) != 10 {
			t.Errorf("second chunk size = %d, want 10", len(queries[1]))
		}
		if queries[0][0] != "1" || queries[0][len(queries[0])-1] != "50" {
			t.Errorf("first chunk = %v, want to start at 1 and end at 50", queries[0])
		}
		if queries[1][0] != "51" || queries[1][len(queries[1])-1] != "60" {
			t.Errorf("second chunk = %v, want to start at 51 and end at 60", queries[1])
		}
	})

	t.Run("404 on the batch route is returned rather than swallowed", func(t *testing.T) {
		t.Parallel()

		s := newPreflightServer(t)
		s.handle("/api/v4/projects/"+testEscapedProject+"/issues", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			w.Write(loadFixture(t, "error_404_project.json")) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, s)

		_, err := a.FetchIssueStatesByIDs(context.Background(), []string{"1"})
		assertTrackerErrorKind(t, err, domain.ErrTrackerNotFound)
	})
}

func TestFetchIssueStatesByIdentifiers(t *testing.T) {
	t.Parallel()

	t.Run("returns the same map as FetchIssueStatesByIDs for the same input", func(t *testing.T) {
		t.Parallel()

		s := newPreflightServer(t)
		s.handle("/api/v4/projects/"+testEscapedProject+"/issues", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`[{"iid": 10, "state": "opened", "issue_type": "issue", "labels": ["in-progress"]}]`)) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, s)

		states, err := a.FetchIssueStatesByIdentifiers(context.Background(), []string{"10"})
		if err != nil {
			t.Fatalf("FetchIssueStatesByIdentifiers: %v", err)
		}
		if len(states) != 1 || states["10"] != "in-progress" {
			t.Errorf("states = %v, want {10: in-progress}", states)
		}
	})

	t.Run("empty input returns empty map with no request", func(t *testing.T) {
		t.Parallel()

		a := mustAdapter(t, newPreflightServer(t)) // no issues route registered; any call fails the test

		states, err := a.FetchIssueStatesByIdentifiers(context.Background(), []string{})
		if err != nil {
			t.Fatalf("FetchIssueStatesByIdentifiers: %v", err)
		}
		if states == nil {
			t.Fatal("states is nil, want non-nil empty map")
		}
		if len(states) != 0 {
			t.Errorf("len = %d, want 0", len(states))
		}
	})
}

// --- Write stubs ---

func TestWriteStubs(t *testing.T) {
	t.Parallel()

	a := mustAdapter(t, newPreflightServer(t)) // no write route registered; any call fails the test

	if err := a.TransitionIssue(context.Background(), "1", "done"); err == nil {
		t.Error("TransitionIssue = nil, want ErrTrackerPayload")
	} else {
		assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)
	}

	if err := a.CommentIssue(context.Background(), "1", "hello"); err == nil {
		t.Error("CommentIssue = nil, want ErrTrackerPayload")
	} else {
		assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)
	}

	if err := a.AddLabel(context.Background(), "1", "ci-failure"); err == nil {
		t.Error("AddLabel = nil, want ErrTrackerPayload")
	} else {
		assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)
	}
}
