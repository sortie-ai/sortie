package gitea

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	"github.com/sortie-ai/sortie/internal/adaptertest"
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

		"active_states":   []string{"backlog", "in-progress", "review"},
		"terminal_states": []string{"done", "wontfix"},
		"handoff_state":   "escalated",
	}
}

func assertTrackerErrorKind(t *testing.T, err error, want domain.TrackerErrorKind) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with kind %q, got nil", want)
	}
	adaptertest.AssertTrackerErrorKind(t, err, want)
}

// decodeRequestBody decodes r's JSON body into v, recording a test failure
// with t.Errorf on a read or decode error. Handlers run on a server goroutine,
// so t.Fatalf (unsafe outside the test goroutine) must never be used here.
func decodeRequestBody(t *testing.T, r *http.Request, v any) {
	t.Helper()
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		t.Errorf("decode request body: %v", err)
	}
}

// assertQueryParams asserts got holds exactly the key/value pairs in want, with
// no extra or missing keys, so a single call proves both that the expected
// operator params merged in and that no unexpected param leaked in. label
// identifies the request in a failure message (for example "candidate" or
// "open-state").
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

func newCandidateMux(t *testing.T) *http.ServeMux {
	t.Helper()
	mux := newPreflightMux(t)
	mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/labels", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":1,"name":"backlog"},{"id":2,"name":"in-progress"},{"id":3,"name":"review"}]`)) //nolint:errcheck // test helper
	})
	mux.HandleFunc("GET /api/v1/orgs/"+testOwner+"/labels", func(w http.ResponseWriter, r *http.Request) {
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
			{"api_key wrong type", map[string]any{"api_key": 4242, "project": "acme/widgets", "endpoint": "http://localhost"}, domain.ErrTrackerPayload},
			{"project wrong type", map[string]any{"api_key": "tok", "project": true, "endpoint": "http://localhost"}, domain.ErrTrackerPayload},
			{"endpoint wrong type", map[string]any{"api_key": "tok", "project": "acme/widgets", "endpoint": 4242}, domain.ErrTrackerPayload},
			{"handoff_state wrong type", map[string]any{"api_key": "tok", "project": "acme/widgets", "endpoint": "http://localhost", "handoff_state": 4242}, domain.ErrTrackerPayload},
			{"user_agent wrong type", map[string]any{"api_key": "tok", "project": "acme/widgets", "endpoint": "http://localhost", "user_agent": 4242}, domain.ErrTrackerPayload},
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

	// endpoint's wrong-type and absent-key faults share domain.ErrTrackerPayload,
	// unlike api_key and project (which get a distinct kind for the absent
	// case), so the two are distinguished here by message instead of kind.
	t.Run("endpoint wrong type vs absent produce distinct messages", func(t *testing.T) {
		t.Parallel()

		wrongType, err := NewGiteaAdapter(map[string]any{"api_key": "tok", "project": "acme/widgets", "endpoint": 4242})
		var te *domain.TrackerError
		if !errors.As(err, &te) {
			t.Fatalf("wrong-type endpoint: error type = %T, want *domain.TrackerError", err)
		}
		if te.Message != "endpoint: expected string, got integer" {
			t.Errorf("wrong-type endpoint: TrackerError.Message = %q, want %q", te.Message, "endpoint: expected string, got integer")
		}
		if wrongType != nil {
			t.Error("adapter should be nil on error")
		}

		absent, err := NewGiteaAdapter(map[string]any{"api_key": "tok", "project": "acme/widgets"})
		if !errors.As(err, &te) {
			t.Fatalf("absent endpoint: error type = %T, want *domain.TrackerError", err)
		}
		if te.Message != "tracker.endpoint is required for gitea" {
			t.Errorf("absent endpoint: TrackerError.Message = %q, want %q", te.Message, "tracker.endpoint is required for gitea")
		}
		if absent != nil {
			t.Error("adapter should be nil on error")
		}
	})

	// query_filter is read ahead of the network preflight, so a mistyped
	// value must be reported without any request reaching the server: the
	// mux here registers no routes at all, and any request fails the test.
	t.Run("query_filter wrong type is reported without a preflight call", func(t *testing.T) {
		t.Parallel()

		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("unexpected request: %s %s (query_filter fault must short-circuit before preflight)", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		cfg := validConfig(srv.URL)
		cfg["query_filter"] = 4242

		a, err := NewGiteaAdapter(cfg)

		var te *domain.TrackerError
		if !errors.As(err, &te) {
			t.Fatalf("error type = %T, want *domain.TrackerError", err)
		}
		if te.Kind != domain.ErrTrackerPayload {
			t.Errorf("TrackerError.Kind = %q, want %q", te.Kind, domain.ErrTrackerPayload)
		}
		if te.Message != "query_filter: expected string, got integer" {
			t.Errorf("TrackerError.Message = %q, want %q", te.Message, "query_filter: expected string, got integer")
		}
		if a != nil {
			t.Error("adapter should be nil on error")
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

	t.Run("query_filter validation runs after preflight and rejects malformed or reserved fragments", func(t *testing.T) {
		t.Parallel()

		// Unlike the pre-network config-error table above, each case here
		// registers the preflight routes: parseGiteaQueryFilter runs only
		// after runPreflight succeeds, so construction must reach the network
		// before the query_filter error surfaces.
		tests := []struct {
			name        string
			queryFilter string
			wantKeyword string
		}{
			{"malformed percent-encoding", "labels=%zz", ""},
			{"reserved key state", "state=open", `"state"`},
			{"reserved key type", "type=issues", `"type"`},
			{"reserved key page", "page=2", `"page"`},
			{"reserved key limit", "limit=10", `"limit"`},
			{"multiple reserved keys report state first", "limit=10&state=open&type=issues&page=2", `"state"`},
			{"page and limit without state or type report page first", "limit=10&page=2", `"page"`},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				srv := httptest.NewServer(newPreflightMux(t))
				defer srv.Close()

				cfg := validConfig(srv.URL)
				cfg["query_filter"] = tt.queryFilter

				a, err := NewGiteaAdapter(cfg)
				assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)
				if a != nil {
					t.Error("adapter should be nil when query_filter fails validation")
				}
				if tt.wantKeyword == "" {
					return
				}
				var te *domain.TrackerError
				if errors.As(err, &te) && !strings.Contains(te.Message, tt.wantKeyword) {
					t.Errorf("NewGiteaAdapter(query_filter=%q) message = %q, want it to name %s", tt.queryFilter, te.Message, tt.wantKeyword)
				}
			})
		}
	})

	t.Run("labels query_filter fetches the label catalog and does not block construction when a label is unresolved", func(t *testing.T) {
		t.Parallel()

		mux := newPreflightMux(t)
		var labelsCalls atomic.Int32
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/labels", func(w http.ResponseWriter, r *http.Request) {
			labelsCalls.Add(1)
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "labels_page1.json")) //nolint:errcheck // test helper
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		cfg := validConfig(srv.URL)
		cfg["query_filter"] = "labels=does-not-exist"

		a, err := NewGiteaAdapter(cfg)
		if err != nil {
			t.Fatalf("NewGiteaAdapter(query_filter with unresolved label): %v", err)
		}
		if a == nil {
			t.Fatal("adapter is nil, want a constructed adapter (an unresolved label must not block construction)")
		}
		if got := labelsCalls.Load(); got != 1 {
			t.Errorf("labels catalog request count = %d, want 1 (construction-time diagnostic fetch)", got)
		}
	})

	t.Run("empty labels query_filter performs no label catalog fetch", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name        string
			queryFilter string
		}{
			{"empty value", "labels="},
			{"whitespace-only value", "labels=%20%20"},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				mux := newPreflightMux(t)
				var labelsCalls atomic.Int32
				mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/labels", func(w http.ResponseWriter, r *http.Request) {
					labelsCalls.Add(1)
					w.WriteHeader(http.StatusOK)
					w.Write(loadFixture(t, "labels_page1.json")) //nolint:errcheck // test helper
				})
				srv := httptest.NewServer(mux)
				defer srv.Close()

				cfg := validConfig(srv.URL)
				cfg["query_filter"] = tt.queryFilter

				a, err := NewGiteaAdapter(cfg)
				if err != nil {
					t.Fatalf("NewGiteaAdapter(query_filter=%q): %v", tt.queryFilter, err)
				}
				if a == nil {
					t.Fatal("adapter is nil, want a constructed adapter")
				}
				if got := labelsCalls.Load(); got != 0 {
					t.Errorf("labels catalog request count = %d, want 0 (empty labels= carries no constraint, so no diagnostic fetch or warning)", got)
				}
			})
		}
	})
}

// endpointRejectionCases names endpoint values that must be rejected as not
// a valid absolute http(s) URL by every Gitea constructor, per
// httpkit.ParseEndpoint. The userinfo-credential-leak case is covered
// separately by TestNewGiteaAdapter_RedactsUserinfoInEndpointError, since it
// needs its own message assertions.
var endpointRejectionCases = []struct {
	name     string
	endpoint string
}{
	{"bare host with no scheme", "gitea.example.com"},
	{"non-http scheme", "ftp://gitea.example.com"},
	{"scheme with no host", "https://"},
	{"unbracketed ipv6 with port", "http://fd00::1:3000"},
	{"doubled port", "http://host:80:80/"},
}

// TestNewGiteaAdapter_RejectsInvalidEndpoint proves the endpoint values in
// endpointRejectionCases fail construction before any network call, since
// none of the config maps here register preflight routes.
func TestNewGiteaAdapter_RejectsInvalidEndpoint(t *testing.T) {
	t.Parallel()

	for _, tt := range endpointRejectionCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := validConfig(tt.endpoint)

			a, err := NewGiteaAdapter(cfg)

			assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)
			if a != nil {
				t.Errorf("NewGiteaAdapter(endpoint=%q) adapter = %v, want nil", tt.endpoint, a)
			}
		})
	}
}

// TestNewGiteaAdapter_RedactsUserinfoInEndpointError proves the credential
// disclosure this fix closes stays closed: an endpoint carrying userinfo is
// rejected, and the error names only the redacted host, never the
// credential.
func TestNewGiteaAdapter_RedactsUserinfoInEndpointError(t *testing.T) {
	t.Parallel()

	const endpoint = "https://operator:s3cr3t@fd00::1:3000"

	_, err := NewGiteaAdapter(validConfig(endpoint))

	var te *domain.TrackerError
	if !errors.As(err, &te) {
		t.Fatalf("NewGiteaAdapter(endpoint=%q) error type = %T, want *domain.TrackerError", endpoint, err)
	}
	if te.Kind != domain.ErrTrackerPayload {
		t.Errorf("NewGiteaAdapter(endpoint=%q) TrackerError.Kind = %q, want %q", endpoint, te.Kind, domain.ErrTrackerPayload)
	}
	if !strings.Contains(te.Message, "fd00::1:3000") {
		t.Errorf("NewGiteaAdapter(endpoint=%q) message = %q, want it to contain the redacted host %q", endpoint, te.Message, "fd00::1:3000")
	}
	if strings.Contains(te.Message, "operator") || strings.Contains(te.Message, "s3cr3t") {
		t.Errorf("NewGiteaAdapter(endpoint=%q) message = %q, must not leak userinfo credentials", endpoint, te.Message)
	}
}

// TestNewGiteaAdapter_EmptyEndpointMessageIsPinned pins the pre-existing
// empty-endpoint message: the shared httpkit.ParseEndpoint guard must not
// have changed this operator-facing text.
func TestNewGiteaAdapter_EmptyEndpointMessageIsPinned(t *testing.T) {
	t.Parallel()

	_, err := NewGiteaAdapter(validConfig(""))

	var te *domain.TrackerError
	if !errors.As(err, &te) {
		t.Fatalf("NewGiteaAdapter(empty endpoint) error type = %T, want *domain.TrackerError", err)
	}
	if te.Kind != domain.ErrTrackerPayload {
		t.Errorf("NewGiteaAdapter(empty endpoint) TrackerError.Kind = %q, want %q", te.Kind, domain.ErrTrackerPayload)
	}
	const want = "tracker.endpoint is required for gitea"
	if te.Message != want {
		t.Errorf("NewGiteaAdapter(empty endpoint) message = %q, want %q", te.Message, want)
	}
}

func TestGiteaAdapterRegistration(t *testing.T) {
	t.Parallel()

	if !registry.Trackers.Has("gitea") {
		t.Fatal(`Trackers.Has("gitea") = false, want true`)
	}
	if _, err := registry.Trackers.Get("gitea"); err != nil {
		t.Errorf(`Trackers.Get("gitea") = %v, want registered constructor`, err)
	}
}

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

func TestFetchCandidateIssues(t *testing.T) {
	t.Parallel()

	t.Run("pagination to exhaustion, PR guard, terminal filter, and ascending resort", func(t *testing.T) {
		t.Parallel()

		page1 := loadFixture(t, "issues_candidates_page1.json") // 8 backlog, 6 PR
		page2 := loadFixture(t, "issues_candidates_page2.json") // 1 backlog, 3 done, 2 in-progress, 4 no state label

		var srvURL string
		var calls atomic.Int32
		mux := newCandidateMux(t)
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

		// Issue 6 (pull request), 3 (done, terminal) and 4 (no state label) are
		// filtered out; 8, 1, 2 remain, re-sorted ascending by CreatedAt
		// regardless of page order.
		if len(issues) != 3 {
			t.Fatalf("len = %d, want 3 (pull request, terminal and unlabeled issues filtered)", len(issues))
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

		mux := newCandidateMux(t)
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
			// giteaIssue carries no dependency field, so every candidate
			// this adapter produces must be marked unresolved rather
			// than read as having no blockers.
			adaptertest.AssertCandidateBlockerSource(t, registry.BlockersPerIssue, iss, 0)
		}
	})

	t.Run("empty active_states returns no candidate and sends no request", func(t *testing.T) {
		t.Parallel()

		a := mustAdapter(t, newPreflightMux(t))
		a.SetTrackerStates(func() domain.TrackerStates { return domain.TrackerStates{} })

		issues, err := a.FetchCandidateIssues(context.Background())

		if err != nil || len(issues) != 0 {
			t.Errorf("FetchCandidateIssues() = %v, %v, want no issues and nil error", issues, err)
		}
	})

	t.Run("empty response returns non-nil empty slice", func(t *testing.T) {
		t.Parallel()

		mux := newCandidateMux(t)
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

		mux := newCandidateMux(t)
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write(loadFixture(t, "error_401.json")) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, mux)

		_, err := a.FetchCandidateIssues(context.Background())
		assertTrackerErrorKind(t, err, domain.ErrTrackerAuth)
	})

	t.Run("operator query_filter merges into the request params", func(t *testing.T) {
		t.Parallel()

		mux := newCandidateMux(t)
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues", func(w http.ResponseWriter, r *http.Request) {
			assertQueryParams(t, "candidate", r.URL.Query(), map[string]string{
				"state":       "open",
				"type":        "issues",
				"limit":       "50",
				"assigned_by": "hermes-bot",
			})
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "issues_candidates_page2.json")) //nolint:errcheck // test helper
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		cfg := validConfig(srv.URL)
		cfg["query_filter"] = "assigned_by=hermes-bot"
		adapter, err := NewGiteaAdapter(cfg)
		if err != nil {
			t.Fatalf("NewGiteaAdapter: %v", err)
		}

		issues, err := adapter.(*GiteaAdapter).FetchCandidateIssues(context.Background())
		if err != nil {
			t.Fatalf("FetchCandidateIssues: %v", err)
		}

		// Issues 1 (backlog) and 2 (in-progress) are active; 3 (done) is terminal
		// and is filtered client-side regardless of the server-side filter.
		if len(issues) != 2 {
			t.Fatalf("len = %d, want 2 (active-state issues only)", len(issues))
		}
	})

	t.Run("unset query_filter sends only the adapter-owned params", func(t *testing.T) {
		t.Parallel()

		mux := newCandidateMux(t)
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues", func(w http.ResponseWriter, r *http.Request) {
			assertQueryParams(t, "candidate", r.URL.Query(), map[string]string{
				"state": "open",
				"type":  "issues",
				"limit": "50",
			})
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "issues_candidates_page2.json")) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, mux)

		if _, err := a.FetchCandidateIssues(context.Background()); err != nil {
			t.Fatalf("FetchCandidateIssues: %v", err)
		}
	})

	t.Run("unrecognized query_filter key is not dropped and still merges into the request", func(t *testing.T) {
		t.Parallel()

		mux := newCandidateMux(t)
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues", func(w http.ResponseWriter, r *http.Request) {
			if got, want := r.URL.Query().Get("assignee"), "hermes-bot"; got != want {
				t.Errorf("request assignee = %q, want %q (unrecognized key is warned but still passed through)", got, want)
			}
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "issues_candidates_page2.json")) //nolint:errcheck // test helper
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		cfg := validConfig(srv.URL)
		cfg["query_filter"] = "assignee=hermes-bot"
		adapter, err := NewGiteaAdapter(cfg)
		if err != nil {
			t.Fatalf("NewGiteaAdapter(query_filter with unrecognized key): %v", err)
		}

		if _, err := adapter.(*GiteaAdapter).FetchCandidateIssues(context.Background()); err != nil {
			t.Fatalf("FetchCandidateIssues: %v", err)
		}
	})

	t.Run("concurrent fetches leave the stored query_filter unmutated", func(t *testing.T) {
		t.Parallel()

		const fetches = 5

		mux := newCandidateMux(t)
		// Buffered to fetches so every handler invocation's send completes
		// without a waiting receiver; the handoff to the test goroutine is
		// then made explicit below rather than relying on the network
		// round-trip's implicit happens-before edge.
		queries := make(chan url.Values, fetches)
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues", func(w http.ResponseWriter, r *http.Request) {
			queries <- r.URL.Query()
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "issues_candidates_page2.json")) //nolint:errcheck // test helper
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		cfg := validConfig(srv.URL)
		cfg["query_filter"] = "assigned_by=hermes-bot"
		adapter, err := NewGiteaAdapter(cfg)
		if err != nil {
			t.Fatalf("NewGiteaAdapter: %v", err)
		}
		a := adapter.(*GiteaAdapter)

		var wg sync.WaitGroup
		for range fetches {
			wg.Go(func() {
				if _, err := a.FetchCandidateIssues(context.Background()); err != nil {
					t.Errorf("FetchCandidateIssues: %v", err)
				}
			})
		}
		wg.Wait()
		close(queries)

		// Each channel send in the handler happens before the corresponding
		// receive here completes (Go memory model), so every query is
		// observed race-free regardless of how many requests overlapped.
		i := 0
		for q := range queries {
			assertQueryParams(t, fmt.Sprintf("concurrent fetch #%d", i), q, map[string]string{
				"state":       "open",
				"type":        "issues",
				"limit":       "50",
				"assigned_by": "hermes-bot",
			})
			i++
		}
		if i != fetches {
			t.Fatalf("received %d queries, want %d", i, fetches)
		}

		if len(a.queryFilter) != 1 {
			t.Errorf("queryFilter has %d keys after concurrent fetches, want 1 (must remain unchanged)", len(a.queryFilter))
		}
		if got := a.queryFilter["assigned_by"]; len(got) != 1 || got[0] != "hermes-bot" {
			t.Errorf("queryFilter[assigned_by] = %v, want [hermes-bot] after concurrent fetches", got)
		}
	})
}

func TestFetchIssuesByStates(t *testing.T) {
	t.Parallel()

	t.Run("empty states short-circuits with no API call", func(t *testing.T) {
		t.Parallel()

		mux := newPreflightMux(t)
		var calls int
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues", func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("[]")) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, mux)

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
	})

	t.Run("requesting only an active state queries the open listing only", func(t *testing.T) {
		t.Parallel()

		mux := newPreflightMux(t)
		var calls atomic.Int32
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues", func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			switch state := r.URL.Query().Get("state"); state {
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
		if got := calls.Load(); got != 1 {
			t.Errorf("call count = %d, want 1 (exactly one open-state query)", got)
		}
	})

	t.Run("requesting only a terminal state queries the closed listing only", func(t *testing.T) {
		t.Parallel()

		mux := newPreflightMux(t)
		var calls atomic.Int32
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues", func(w http.ResponseWriter, r *http.Request) {
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
		if got := calls.Load(); got != 1 {
			t.Errorf("call count = %d, want 1 (exactly one closed-state query)", got)
		}
	})

	t.Run("mixed request queries both the open and closed listings", func(t *testing.T) {
		t.Parallel()

		mux := newPreflightMux(t)
		// atomic.Bool, not a plain map: a Go map is unsafe for concurrent
		// writes even to distinct keys, and these two handler invocations
		// must be provably race-free independent of request ordering.
		var gotOpen, gotClosed atomic.Bool
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues", func(w http.ResponseWriter, r *http.Request) {
			switch state := r.URL.Query().Get("state"); state {
			case "open":
				gotOpen.Store(true)
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
		a := mustAdapter(t, mux)

		issues, err := a.FetchIssuesByStates(context.Background(), []string{"in-progress", "done"})
		if err != nil {
			t.Fatalf("FetchIssuesByStates: %v", err)
		}
		if len(issues) != 2 {
			t.Fatalf("len = %d, want 2 (one from each listing)", len(issues))
		}
		if got, want := gotOpen.Load(), true; got != want {
			t.Errorf("open state queried = %t, want %t", got, want)
		}
		if got, want := gotClosed.Load(), true; got != want {
			t.Errorf("closed state queried = %t, want %t", got, want)
		}
	})

	t.Run("query_filter merges into the open half only; the closed half carries none", func(t *testing.T) {
		t.Parallel()

		mux := newPreflightMux(t)
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues", func(w http.ResponseWriter, r *http.Request) {
			switch state := r.URL.Query().Get("state"); state {
			case "open":
				assertQueryParams(t, "open-state", r.URL.Query(), map[string]string{
					"state": "open", "type": "issues", "limit": "50", "assigned_by": "hermes-bot",
				})
				w.WriteHeader(http.StatusOK)
				w.Write(loadFixture(t, "issues_open_by_states.json")) //nolint:errcheck // test helper
			case "closed":
				assertQueryParams(t, "closed-state", r.URL.Query(), map[string]string{
					"state": "closed", "type": "issues", "limit": "50",
				})
				w.WriteHeader(http.StatusOK)
				w.Write(loadFixture(t, "issues_closed_by_states.json")) //nolint:errcheck // test helper
			default:
				t.Errorf("unexpected state query param: %q", state)
				w.WriteHeader(http.StatusBadRequest)
			}
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()

		cfg := validConfig(srv.URL)
		cfg["query_filter"] = "assigned_by=hermes-bot"
		adapter, err := NewGiteaAdapter(cfg)
		if err != nil {
			t.Fatalf("NewGiteaAdapter: %v", err)
		}

		if _, err := adapter.(*GiteaAdapter).FetchIssuesByStates(context.Background(), []string{"in-progress", "done"}); err != nil {
			t.Fatalf("FetchIssuesByStates: %v", err)
		}
	})

	t.Run("unset query_filter sends only the adapter-owned params on both halves", func(t *testing.T) {
		t.Parallel()

		mux := newPreflightMux(t)
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues", func(w http.ResponseWriter, r *http.Request) {
			switch state := r.URL.Query().Get("state"); state {
			case "open":
				assertQueryParams(t, "open-state", r.URL.Query(), map[string]string{
					"state": "open", "type": "issues", "limit": "50",
				})
				w.WriteHeader(http.StatusOK)
				w.Write(loadFixture(t, "issues_open_by_states.json")) //nolint:errcheck // test helper
			case "closed":
				assertQueryParams(t, "closed-state", r.URL.Query(), map[string]string{
					"state": "closed", "type": "issues", "limit": "50",
				})
				w.WriteHeader(http.StatusOK)
				w.Write(loadFixture(t, "issues_closed_by_states.json")) //nolint:errcheck // test helper
			default:
				t.Errorf("unexpected state query param: %q", state)
				w.WriteHeader(http.StatusBadRequest)
			}
		})
		a := mustAdapter(t, mux)

		if _, err := a.FetchIssuesByStates(context.Background(), []string{"in-progress", "done"}); err != nil {
			t.Fatalf("FetchIssuesByStates: %v", err)
		}
	})
}

func TestReportUnresolvedLabels(t *testing.T) {
	t.Parallel()

	t.Run("case-sensitive miss logs exactly one warning naming the requested label", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))
		known := map[string]struct{}{"agent-ready": {}}

		reportUnresolvedLabels(logger, []string{"Agent-Ready"}, known)

		output := buf.String()
		if got := strings.Count(output, "query_filter label does not resolve against repository labels"); got != 1 {
			t.Fatalf("warn count = %d, want 1\noutput: %s", got, output)
		}
		if !strings.Contains(output, "label=Agent-Ready") {
			t.Errorf("output missing label=Agent-Ready (case-sensitive miss against known %q)\noutput: %s", "agent-ready", output)
		}
	})

	t.Run("exact case match resolves and logs nothing", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))
		known := map[string]struct{}{"agent-ready": {}}

		reportUnresolvedLabels(logger, []string{"agent-ready"}, known)

		if buf.Len() != 0 {
			t.Errorf("output = %q, want empty (exact-case match must not warn)", buf.String())
		}
	})

	t.Run("comma-separated values are split and trimmed before the case-sensitive comparison", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))
		known := map[string]struct{}{"agent-ready": {}, "ci-failure": {}}

		reportUnresolvedLabels(logger, []string{"agent-ready, Ci-Failure"}, known)

		output := buf.String()
		if got := strings.Count(output, "query_filter label does not resolve against repository labels"); got != 1 {
			t.Fatalf("warn count = %d, want 1 (only Ci-Failure is a case-sensitive miss)\noutput: %s", got, output)
		}
		if !strings.Contains(output, "label=Ci-Failure") {
			t.Errorf("output missing label=Ci-Failure\noutput: %s", output)
		}
	})
}

func TestWarnUnrecognizedFilterKeys(t *testing.T) {
	t.Parallel()

	t.Run("unrecognized key logs exactly one warning naming the key", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))

		warnUnrecognizedFilterKeys(logger, url.Values{"assignee": {"hermes-bot"}})

		output := buf.String()
		if got := strings.Count(output, "query_filter contains an unrecognized key"); got != 1 {
			t.Fatalf("warn count = %d, want 1\noutput: %s", got, output)
		}
		if !strings.Contains(output, "key=assignee") {
			t.Errorf("output missing key=assignee\noutput: %s", output)
		}
	})

	t.Run("known keys produce no warning", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))

		warnUnrecognizedFilterKeys(logger, url.Values{"assigned_by": {"hermes-bot"}, "labels": {"agent-ready"}})

		if buf.Len() != 0 {
			t.Errorf("output = %q, want empty (all keys are known)", buf.String())
		}
	})

	t.Run("mixed known and unrecognized keys warn only for the unrecognized one", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))

		warnUnrecognizedFilterKeys(logger, url.Values{"assigned_by": {"hermes-bot"}, "assignee": {"hermes-bot"}})

		output := buf.String()
		if got := strings.Count(output, "query_filter contains an unrecognized key"); got != 1 {
			t.Fatalf("warn count = %d, want 1\noutput: %s", got, output)
		}
		if !strings.Contains(output, "key=assignee") {
			t.Errorf("output missing key=assignee\noutput: %s", output)
		}
		if strings.Contains(output, "key=assigned_by") {
			t.Errorf("output unexpectedly warned about the known key assigned_by\noutput: %s", output)
		}
	})
}

func TestFetchLabelNames(t *testing.T) {
	t.Parallel()

	t.Run("preserves original casing, unlike resolveLabelIndex's lowercased keys", func(t *testing.T) {
		t.Parallel()

		mux := newPreflightMux(t)
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/labels", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "labels_mixed_case.json")) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, mux)

		names, err := a.fetchLabelNames(context.Background())
		if err != nil {
			t.Fatalf("fetchLabelNames: %v", err)
		}

		if _, ok := names["In-Progress"]; !ok {
			t.Errorf("names = %v, want the original-case key %q present", names, "In-Progress")
		}
		if _, ok := names["in-progress"]; ok {
			t.Errorf("names = %v, want the lowercased key %q absent (fetchLabelNames must not lowercase)", names, "in-progress")
		}
	})
}

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
		if issue.BlockersUnresolved {
			t.Error("BlockersUnresolved = true, want false: the blocker read succeeded")
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
		adaptertest.AssertIssueNormalized(t, issue)
		adaptertest.AssertCommentsAscending(t, issue.Comments)
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
			if got := r.Header.Get("If-None-Match"); got != "" {
				t.Errorf("If-None-Match = %q, want none on the full read", got)
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
		adaptertest.AssertEmptyNonNil(t, comments, "FetchIssueComments")
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

func TestFetchIssueCommentChanges(t *testing.T) {
	t.Parallel()

	botComment := domain.Comment{
		ID: "9003", Author: "renovate-bot", AuthorID: "renovate-bot", AuthorKind: domain.CommentAuthorBot,
		Body: "Rebased onto main.", CreatedAt: "2026-01-17T08:00:00Z", UpdatedAt: "2026-01-17T08:00:00Z",
	}
	editedComment := domain.Comment{
		ID: "9002", Author: "bob", AuthorID: "bob", AuthorKind: domain.CommentAuthorPerson,
		Body: "Please fix the indentation and the import order.", CreatedAt: "2026-01-16T11:30:00Z", UpdatedAt: "2026-01-18T10:15:00Z",
	}

	tests := []struct {
		name      string
		since     domain.CommentReadPoint
		fixture   string
		wantSince string
		want      []domain.Comment
		wantNext  domain.CommentReadPoint
		wantKind  domain.TrackerErrorKind
	}{
		{"new comment", "2026-01-16T11:30:00Z", "comment_changes_new.json", "2026-01-16T11:30:00Z", []domain.Comment{botComment}, "2026-01-17T08:00:00Z", ""},
		{"edited comment", "2026-01-17T08:00:00Z", "comment_changes_edited.json", "2026-01-17T08:00:00Z", []domain.Comment{editedComment}, "2026-01-18T10:15:00Z", ""},
		{"unchanged list keeps the point", "2026-01-17T08:00:00.5Z", "comment_changes_new.json", "2026-01-17T08:00:00Z", []domain.Comment{botComment}, "2026-01-17T08:00:00.5Z", ""},
		{"undecodable point reads the full list", "x", "comment_changes_new.json", "", []domain.Comment{botComment}, "2026-01-17T08:00:00Z", ""},
		{"unparseable updated_at", "2026-01-16T11:30:00Z", "comment_changes_bad_updated_at.json", "2026-01-16T11:30:00Z", nil, "", domain.ErrTrackerPayload},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := loadFixture(t, tt.fixture)
			mux := newPreflightMux(t)
			var mu sync.Mutex
			var queries []url.Values
			mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues/42/comments", func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				queries = append(queries, r.URL.Query())
				mu.Unlock()
				w.Write(fixture) //nolint:errcheck // test helper
			})
			a := mustAdapter(t, mux)

			got, err := a.FetchIssueCommentChanges(context.Background(), "42", tt.since)

			if tt.wantKind != "" {
				assertTrackerErrorKind(t, err, tt.wantKind)
			} else if err != nil {
				t.Fatalf("FetchIssueCommentChanges(%q): %v", tt.since, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(queries) != 1 {
				t.Fatalf("FetchIssueCommentChanges(%q) sent %d requests, want 1", tt.since, len(queries))
			}
			if q := queries[0]; q.Get("since") != tt.wantSince || len(q) > 1 {
				t.Errorf("FetchIssueCommentChanges(%q) query = %v, want only since=%q", tt.since, q, tt.wantSince)
			}
			if !slices.Equal(got.Comments, tt.want) || got.Next != tt.wantNext {
				t.Errorf("FetchIssueCommentChanges(%q) = %+v, want {%+v %q}", tt.since, got, tt.want, tt.wantNext)
			}
			adaptertest.AssertCommentsAscending(t, got.Comments)
		})
	}
}

func TestFetchIssueStatesByIDs(t *testing.T) {
	t.Parallel()

	t.Run("empty input returns empty map with no request", func(t *testing.T) {
		t.Parallel()

		mux := newPreflightMux(t)
		var calls int
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues/{index}", func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.WriteHeader(http.StatusNotFound)
		})
		a := mustAdapter(t, mux)

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
		adaptertest.AssertNoRequestOnEmptyInput(t, calls, "FetchIssueStatesByIDs")
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
		adaptertest.AssertStateMapOmitsMissing(t, []string{"1", "6", "999"}, states)
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

		blockers, err := a.fetchBlockers(context.Background(), "42", a.model.Snapshot())
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
		if want := testOwner + "/" + testRepo + "#1"; blockers[0].DisplayID != want {
			t.Errorf("blockers[0].DisplayID = %q, want %q", blockers[0].DisplayID, want)
		}

		adaptertest.AssertBlockerRefsNormalized(t, blockers)
		qualifiedIssue := domain.Issue{Identifier: "42", DisplayID: testOwner + "/" + testRepo + "#42"}
		adaptertest.AssertBlockerIdentifiersMatchIssue(t, qualifiedIssue, blockers)
	})

	t.Run("404 propagates as a not-found error rather than degrading to no blockers", func(t *testing.T) {
		t.Parallel()

		// A 404 on the dependencies route is not a signal that the issue
		// has no blockers: it is a condition the resolver must surface,
		// so the candidate stays held rather than dispatched on an
		// unknown blocker list.
		mux := newPreflightMux(t)
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues/42/dependencies", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			w.Write(loadFixture(t, "error_404.json")) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, mux)

		_, err := a.fetchBlockers(context.Background(), "42", a.model.Snapshot())
		assertTrackerErrorKind(t, err, domain.ErrTrackerNotFound)
	})
}

func TestTransitionIssue(t *testing.T) {
	t.Parallel()

	t.Run("issue not found maps to not found error", func(t *testing.T) {
		t.Parallel()

		mux := newPreflightMux(t)
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues/{index}", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			w.Write(loadFixture(t, "error_404.json")) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, mux)

		err := a.TransitionIssue(context.Background(), "999", "done")
		assertTrackerErrorKind(t, err, domain.ErrTrackerNotFound)
	})

	t.Run("pull request index maps to not found error", func(t *testing.T) {
		t.Parallel()

		mux := newPreflightMux(t)
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues/{index}", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "issue_pr.json")) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, mux)

		err := a.TransitionIssue(context.Background(), "6", "done")
		assertTrackerErrorKind(t, err, domain.ErrTrackerNotFound)
	})
}

func TestCommentIssue(t *testing.T) {
	t.Parallel()

	t.Run("posts the markdown body verbatim and returns the created comment's identity", func(t *testing.T) {
		t.Parallel()

		const markdown = "## Status update\n\n- retried the failing step\n- **no repro** locally\n"
		const commentsPath = "/api/v1/repos/" + testOwner + "/" + testRepo + "/issues/42/comments"
		const created = `{"id":7312450,"user":{"login":"sortie-bot"},"body":"status update","created_at":"2026-08-04T12:00:00Z"}`
		const listed = `[{"id":9001,"user":{"login":"alice"},"body":"Looks good.","created_at":"2026-01-15T10:00:00Z"},` + created + `]`

		mux := newPreflightMux(t)
		var gotBody struct {
			Body string `json:"body"`
		}
		var requests atomic.Int32
		mux.HandleFunc("POST "+commentsPath, func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			decodeRequestBody(t, r, &gotBody)
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(created)) //nolint:errcheck // test helper
		})
		mux.HandleFunc("GET "+commentsPath, func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			w.Write([]byte(listed)) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, mux)

		id, err := a.CommentIssue(context.Background(), "42", markdown)
		if err != nil {
			t.Fatalf("CommentIssue: %v", err)
		}
		if got := requests.Load(); got != 1 {
			t.Errorf("request count during CommentIssue = %d, want 1", got)
		}
		if gotBody.Body != markdown {
			t.Errorf("comment body = %q, want %q (verbatim, no conversion)", gotBody.Body, markdown)
		}

		comments, err := a.FetchIssueComments(context.Background(), "42")
		if err != nil {
			t.Fatalf("FetchIssueComments: %v", err)
		}
		adaptertest.AssertPostedCommentListed(t, id, comments)
	})
}

func TestWriteErrorMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		statusCode int
		fixture    string
		wantKind   domain.TrackerErrorKind
	}{
		{"403 forbidden maps to auth error", http.StatusForbidden, "error_403.json", domain.ErrTrackerAuth},
		{"422 unprocessable maps to payload error", http.StatusUnprocessableEntity, "error_422.json", domain.ErrTrackerPayload},
		{"201 without a comment id maps to payload error", http.StatusCreated, "comment_created_no_id.json", domain.ErrTrackerPayload},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mux := newPreflightMux(t)
			mux.HandleFunc("POST /api/v1/repos/"+testOwner+"/"+testRepo+"/issues/42/comments", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
				w.Write(loadFixture(t, tt.fixture)) //nolint:errcheck // test helper
			})
			a := mustAdapter(t, mux)

			id, err := a.CommentIssue(context.Background(), "42", "body")
			assertTrackerErrorKind(t, err, tt.wantKind)
			if id != "" {
				t.Errorf("CommentIssue identity = %q, want empty on error", id)
			}
		})
	}
}

func TestAddLabel(t *testing.T) {
	t.Parallel()

	t.Run("creates a missing label with the default color and attaches it additively", func(t *testing.T) {
		t.Parallel()

		mux := newPreflightMux(t)
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/labels", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "labels_page1.json")) //nolint:errcheck // test helper
		})
		var createBody struct {
			Name  string `json:"name"`
			Color string `json:"color"`
		}
		mux.HandleFunc("POST /api/v1/repos/"+testOwner+"/"+testRepo+"/labels", func(w http.ResponseWriter, r *http.Request) {
			decodeRequestBody(t, r, &createBody)
			w.WriteHeader(http.StatusCreated)
			w.Write(loadFixture(t, "label_created.json")) //nolint:errcheck // test helper
		})
		var attachBody struct {
			Labels []int64 `json:"labels"`
		}
		mux.HandleFunc("POST /api/v1/repos/"+testOwner+"/"+testRepo+"/issues/42/labels", func(w http.ResponseWriter, r *http.Request) {
			decodeRequestBody(t, r, &attachBody)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`[{"id":701,"name":"needs-human"}]`)) //nolint:errcheck // test helper
		})
		// No issue GET and no DELETE route registered: the response carries
		// the label, so AddLabel needs no read and removes nothing.
		a := mustAdapter(t, mux)

		if err := a.AddLabel(context.Background(), "42", "Needs-Human"); err != nil {
			t.Fatalf("AddLabel: %v", err)
		}

		if createBody.Name != "needs-human" {
			t.Errorf("create-label name = %q, want %q (lowercased)", createBody.Name, "needs-human")
		}
		if createBody.Color != "cccccc" {
			t.Errorf("create-label color = %q, want %q", createBody.Color, "cccccc")
		}
		if want := []int64{701}; !slices.Equal(attachBody.Labels, want) {
			t.Errorf("attach body labels = %v, want %v (numeric created id, not the name)", attachBody.Labels, want)
		}

		// The attach route's request body carries only the new label's id,
		// so it adds without touching labels already on the issue.
		before := []string{"existing"}
		after := append(slices.Clone(before), "needs-human")
		adaptertest.AssertLabelAddIsAdditive(t, before, after, "needs-human")
	})

	t.Run("resolves a label defined only on page two of the catalog with no duplicate create", func(t *testing.T) {
		t.Parallel()

		page1 := loadFixture(t, "labels_page1.json")
		page2 := loadFixture(t, "labels_page2.json")

		var srvURL string
		var calls atomic.Int32
		mux := newPreflightMux(t)
		basePath := "/api/v1/repos/" + testOwner + "/" + testRepo + "/labels"
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
		var attachBody struct {
			Labels []int64 `json:"labels"`
		}
		mux.HandleFunc("POST /api/v1/repos/"+testOwner+"/"+testRepo+"/issues/42/labels", func(w http.ResponseWriter, r *http.Request) {
			decodeRequestBody(t, r, &attachBody)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`[{"id":601,"name":"ci-failure"}]`)) //nolint:errcheck // test helper
		})
		// No create-label route registered: a spurious create for a label
		// that resolves from page two would fail the test.
		srv := httptest.NewServer(mux)
		defer srv.Close()
		srvURL = srv.URL

		adapter, err := NewGiteaAdapter(validConfig(srv.URL))
		if err != nil {
			t.Fatalf("NewGiteaAdapter: %v", err)
		}
		a := adapter.(*GiteaAdapter)

		if err := a.AddLabel(context.Background(), "42", "ci-failure"); err != nil {
			t.Fatalf("AddLabel: %v", err)
		}

		if got := calls.Load(); got != 2 {
			t.Errorf("labels catalog request count = %d, want 2 (paged to exhaustion)", got)
		}
		if want := []int64{601}; !slices.Equal(attachBody.Labels, want) {
			t.Errorf("attach body labels = %v, want %v (numeric id of ci-failure resolved from page two)", attachBody.Labels, want)
		}
	})
}

type fakeLabel struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type issueLabelFake struct {
	mu           sync.Mutex
	catalog      []fakeLabel
	labels       []fakeLabel
	deleteStatus int
	applyDelete  bool
	labelsStatus int
	deletedIDs   []string
	closed       bool
	ops          []string
}

const labelFakeIndex = "42"

func newIssueLabelFake(t *testing.T, catalog, labels []fakeLabel) (*GiteaAdapter, *issueLabelFake) {
	t.Helper()

	fake := &issueLabelFake{catalog: catalog, labels: labels}
	base := "/api/v1/repos/" + testOwner + "/" + testRepo
	issuePath := base + "/issues/" + labelFakeIndex

	mux := newPreflightMux(t)
	mux.HandleFunc("GET "+issuePath, fake.serveIssue)
	mux.HandleFunc("PATCH "+issuePath, fake.serveIssue)
	mux.HandleFunc("GET "+issuePath+"/labels", fake.serveIssueLabels)
	mux.HandleFunc("POST "+issuePath+"/labels", fake.serveAttach)
	mux.HandleFunc("DELETE "+issuePath+"/labels/{id}", fake.serveDetach)
	mux.HandleFunc("GET "+base+"/labels", fake.serveCatalog)

	return mustAdapter(t, mux), fake
}

func (f *issueLabelFake) serveIssue(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method == http.MethodPatch {
		var req struct {
			State string `json:"state"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.ops = append(f.ops, "state:"+req.State)
		f.closed = req.State == "closed"
	}
	body, _ := json.Marshal(map[string]any{
		"id":           9042,
		"number":       42,
		"title":        "Label fake issue",
		"state":        map[bool]string{false: "open", true: "closed"}[f.closed],
		"html_url":     "https://git.example.com/acme/widgets/issues/42",
		"labels":       slices.Clone(f.labels),
		"assignees":    []any{},
		"pull_request": nil,
		"created_at":   "2026-01-01T00:00:00Z",
		"updated_at":   "2026-01-02T00:00:00Z",
	})
	w.Write(body) //nolint:errcheck // test helper
}

func labelPage(labels []fakeLabel, r *http.Request) []fakeLabel {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if limit <= 0 {
		return labels
	}
	start := min((max(page, 1)-1)*limit, len(labels))
	end := min(start+limit, len(labels))
	return labels[start:end]
}

func (f *issueLabelFake) serveIssueLabels(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.labelsStatus != 0 {
		w.WriteHeader(f.labelsStatus)
		return
	}
	body, _ := json.Marshal(slices.Clone(labelPage(f.labels, r)))
	w.Write(body) //nolint:errcheck // test helper
}

func (f *issueLabelFake) serveCatalog(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := json.Marshal(slices.Clone(labelPage(f.catalog, r)))
	w.Write(body) //nolint:errcheck // test helper
}

func (f *issueLabelFake) serveAttach(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Labels []int64 `json:"labels"`
	}
	raw, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(raw, &req)

	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range req.Labels {
		if i := slices.IndexFunc(f.catalog, func(l fakeLabel) bool { return l.ID == id }); i >= 0 {
			f.labels = append(f.labels, f.catalog[i])
			f.ops = append(f.ops, "add:"+f.catalog[i].Name)
		}
	}
	body, _ := json.Marshal(slices.Clone(f.labels))
	w.Write(body) //nolint:errcheck // test helper
}

func (f *issueLabelFake) serveDetach(w http.ResponseWriter, r *http.Request) {
	rawID := r.PathValue("id")
	id, _ := strconv.ParseInt(rawID, 10, 64)

	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletedIDs = append(f.deletedIDs, rawID)
	remove := func() {
		if i := slices.IndexFunc(f.labels, func(l fakeLabel) bool { return l.ID == id }); i >= 0 {
			f.ops = append(f.ops, "remove:"+f.labels[i].Name)
		}
		f.labels = slices.DeleteFunc(f.labels, func(l fakeLabel) bool { return l.ID == id })
	}

	if f.deleteStatus != 0 {
		if f.applyDelete {
			remove()
		}
		w.WriteHeader(f.deleteStatus)
		return
	}
	if !slices.ContainsFunc(f.labels, func(l fakeLabel) bool { return l.ID == id }) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	remove()
	w.WriteHeader(http.StatusNoContent)
}

func (f *issueLabelFake) deleted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.deletedIDs)
}

func (f *issueLabelFake) writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.ops)
}

func TestRemoveLabel_DeletesEverySpellingByTheIDTheIssueReports(t *testing.T) {
	t.Parallel()

	labels := make([]fakeLabel, 0, 60)
	for i := range 55 {
		labels = append(labels, fakeLabel{ID: int64(100 + i), Name: fmt.Sprintf("label-%02d", i)})
	}
	labels = append(labels, fakeLabel{ID: 11, Name: "Stage-Plan"}, fakeLabel{ID: 900, Name: "stage-plan"})
	a, fake := newIssueLabelFake(t, []fakeLabel{{ID: 11, Name: "Stage-Plan"}}, labels)

	err := a.RemoveLabel(context.Background(), labelFakeIndex, "STAGE-PLAN")

	if err != nil {
		t.Fatalf("RemoveLabel = %v, want nil", err)
	}
	if got, want := fake.deleted(), []string{"11", "900"}; !slices.Equal(got, want) {
		t.Errorf("DELETEd label ids = %v, want %v (the repository label and the organization label, from the second page)", got, want)
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
		{"unprocessable but label gone", http.StatusUnprocessableEntity, 0, true, ""},
		{"not found and label kept", http.StatusNotFound, 0, false, domain.ErrTrackerNotFound},
		{"label read not found", 0, http.StatusNotFound, false, domain.ErrTrackerNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			a, fake := newIssueLabelFake(t, nil, []fakeLabel{{ID: 700, Name: "needs-human"}})
			fake.deleteStatus = tt.deleteStatus
			fake.applyDelete = tt.applied
			fake.labelsStatus = tt.labelsStatus

			err := a.RemoveLabel(context.Background(), labelFakeIndex, "needs-human")

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

func TestTransitionIssue_WriteOrder(t *testing.T) {
	t.Parallel()

	catalog := []fakeLabel{{ID: 501, Name: "in-progress"}, {ID: 502, Name: "done"}, {ID: 503, Name: "escalated"}, {ID: 504, Name: "backlog"}}
	inProgress := fakeLabel{ID: 501, Name: "in-progress"}
	done := fakeLabel{ID: 502, Name: "done"}

	tests := []struct {
		name    string
		labels  []fakeLabel
		closed  bool
		target  string
		wantOps []string
	}{
		{"handoff adds the target before removing the start label", []fakeLabel{inProgress, {ID: 9, Name: "ui"}}, false, "escalated", []string{"add:escalated", "remove:in-progress"}},
		{"terminal adds, removes, then closes last", []fakeLabel{inProgress}, false, "done", []string{"add:done", "remove:in-progress", "state:closed"}},
		{"every spelling of the start label goes after the add", []fakeLabel{{ID: 9001, Name: "In-Progress"}, {ID: 9002, Name: "in-progress"}}, false, "done", []string{"add:done", "remove:In-Progress", "remove:in-progress", "state:closed"}},
		{"reopen precedes the label writes", []fakeLabel{done}, true, "backlog", []string{"state:open", "add:backlog", "remove:done"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			a, fake := newIssueLabelFake(t, catalog, tt.labels)
			fake.closed = tt.closed

			if err := a.TransitionIssue(context.Background(), labelFakeIndex, tt.target); err != nil {
				t.Fatalf("TransitionIssue(%s, %q) = %v, want nil", labelFakeIndex, tt.target, err)
			}

			if got := fake.writes(); !slices.Equal(got, tt.wantOps) {
				t.Errorf("TransitionIssue(%s, %q) wrote %v, want %v", labelFakeIndex, tt.target, got, tt.wantOps)
			}
		})
	}
}

func TestEnsureLabelID(t *testing.T) {
	t.Parallel()

	t.Run("returns the indexed id without a create when the label is present", func(t *testing.T) {
		t.Parallel()

		// Only the preflight routes are registered: a create request would hit
		// the catch-all and fail, proving no create is issued.
		a := mustAdapter(t, newPreflightMux(t))

		id, err := a.ensureLabelID(context.Background(), map[string]int64{"in-progress": 501}, "in-progress")
		if err != nil {
			t.Fatalf("ensureLabelID: %v", err)
		}
		if id != 501 {
			t.Errorf("ensureLabelID id = %d, want %d (resolved from the index)", id, 501)
		}
	})

	t.Run("creates the label and returns the new id when absent", func(t *testing.T) {
		t.Parallel()

		mux := newPreflightMux(t)
		var createCalls atomic.Int32
		mux.HandleFunc("POST /api/v1/repos/"+testOwner+"/"+testRepo+"/labels", func(w http.ResponseWriter, r *http.Request) {
			createCalls.Add(1)
			w.WriteHeader(http.StatusCreated)
			w.Write(loadFixture(t, "label_created.json")) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, mux)

		index := map[string]int64{}
		id, err := a.ensureLabelID(context.Background(), index, "needs-human")
		if err != nil {
			t.Fatalf("ensureLabelID: %v", err)
		}
		if id != 701 {
			t.Errorf("ensureLabelID id = %d, want %d (created label id)", id, 701)
		}
		if got := index["needs-human"]; got != 701 {
			t.Errorf("index[needs-human] = %d, want %d (cached after create)", got, 701)
		}
		if got := createCalls.Load(); got != 1 {
			t.Errorf("create call count = %d, want 1", got)
		}
	})

	t.Run("propagates the create error when the label is absent", func(t *testing.T) {
		t.Parallel()

		// No labels GET route is registered: a create failure returns the
		// classifier-mapped error directly and never re-reads the catalog, so a
		// stray catalog GET would hit the catch-all and fail.
		mux := newPreflightMux(t)
		mux.HandleFunc("POST /api/v1/repos/"+testOwner+"/"+testRepo+"/labels", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			w.Write(loadFixture(t, "error_403.json")) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, mux)

		_, err := a.ensureLabelID(context.Background(), map[string]int64{}, "still-missing")
		assertTrackerErrorKind(t, err, domain.ErrTrackerAuth)
	})
}

func TestNewGiteaAdapter_ProjectURL(t *testing.T) {
	t.Parallel()

	mux := newPreflightMux(t)
	root := http.NewServeMux()
	root.Handle("/gitea/", http.StripPrefix("/gitea", mux))
	root.Handle("/", mux)
	srv := httptest.NewServer(root)
	t.Cleanup(srv.Close)

	tests := []struct{ name, endpoint, want string }{
		{"bare host", srv.URL, srv.URL + "/acme/widgets"},
		{"api suffix is dropped", srv.URL + "/api/v1", srv.URL + "/acme/widgets"},
		{"subpath install keeps its subpath", srv.URL + "/gitea/api/v1", srv.URL + "/gitea/acme/widgets"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			adapter, err := NewGiteaAdapter(validConfig(tt.endpoint))
			if err != nil {
				t.Fatalf("NewGiteaAdapter(%q): %v", tt.endpoint, err)
			}

			if got := adapter.(*GiteaAdapter).ProjectURL(); got != tt.want {
				t.Errorf("ProjectURL() with endpoint %q = %q, want %q", tt.endpoint, got, tt.want)
			}
		})
	}
}
