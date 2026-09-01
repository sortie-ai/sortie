package gitea

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
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

// loweredStates returns a lowercased copy of states, matching the
// normalization NewGiteaAdapter applies to every state list element.
func loweredStates(states []string) []string {
	out := make([]string, len(states))
	for i, s := range states {
		out[i] = strings.ToLower(s)
	}
	return out
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

	// validConfig omits both state keys, so the constructed adapter holds the
	// fallback lists. They must stay the ones the registration declares,
	// because the dispatch preflight rules on the declared lists.
	t.Run("successful construction applies default state lists", func(t *testing.T) {
		t.Parallel()

		meta, ok := registry.Trackers.Meta("gitea")
		if !ok {
			t.Fatal(`Trackers.Meta("gitea") reported not registered`)
		}
		if len(meta.DefaultActiveStates) == 0 || len(meta.DefaultTerminalStates) == 0 {
			t.Fatalf("Trackers.Meta(%q) declares DefaultActiveStates = %v, DefaultTerminalStates = %v, want both non-empty",
				"gitea", meta.DefaultActiveStates, meta.DefaultTerminalStates)
		}

		a := mustAdapter(t, newPreflightMux(t))

		if a.owner != testOwner {
			t.Errorf("owner = %q, want %q", a.owner, testOwner)
		}
		if a.repo != testRepo {
			t.Errorf("repo = %q, want %q", a.repo, testRepo)
		}
		if want := loweredStates(meta.DefaultActiveStates); !slices.Equal(a.activeStates, want) {
			t.Errorf("NewGiteaAdapter(config without active_states).activeStates = %v, want %v", a.activeStates, want)
		}
		if want := loweredStates(meta.DefaultTerminalStates); !slices.Equal(a.terminalStates, want) {
			t.Errorf("NewGiteaAdapter(config without terminal_states).terminalStates = %v, want %v", a.terminalStates, want)
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
			// giteaIssue carries no dependency field, so every candidate
			// this adapter produces must be marked unresolved rather
			// than read as having no blockers.
			adaptertest.AssertCandidateBlockerSource(t, registry.BlockersPerIssue, iss, 0)
		}
	})

	t.Run("multi-label WARN carries issue_identifier, not issue_index", func(t *testing.T) {
		t.Parallel()

		// #9 carries both "backlog" and "in-progress", two configured
		// active-state labels.
		const twoLabels = `[{"id":9009,"number":9,"title":"Two state labels","body":null,"state":"open","ref":"",
			"html_url":"https://git.example.com/acme/widgets/issues/9",
			"labels":[{"id":1,"name":"backlog","color":"cccccc"},{"id":2,"name":"in-progress","color":"cccccc"}],
			"assignees":[],"pull_request":null,"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}]`

		mux := newPreflightMux(t)
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(twoLabels)) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, mux)

		var buf bytes.Buffer
		a.log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		if _, err := a.FetchCandidateIssues(context.Background()); err != nil {
			t.Fatalf("FetchCandidateIssues: %v", err)
		}

		var warnLine string
		for line := range strings.SplitSeq(buf.String(), "\n") {
			if strings.Contains(line, "kept first of multiple matching state labels") {
				warnLine = line
				break
			}
		}
		if warnLine == "" {
			t.Fatalf("multi-match WARN not found in captured log: %s", buf.String())
		}
		if !strings.Contains(warnLine, "issue_identifier=9") {
			t.Errorf("multi-match WARN missing issue_identifier=9: %q", warnLine)
		}
		if !strings.Contains(warnLine, "backlog") || !strings.Contains(warnLine, "in-progress") {
			t.Errorf("multi-match WARN does not name both matching labels: %q", warnLine)
		}
		if strings.Contains(warnLine, "issue_index=") {
			t.Errorf("multi-match WARN record carries issue_index: %q", warnLine)
		}
		if strings.Contains(warnLine, "iid=") {
			t.Errorf("multi-match WARN record carries iid: %q", warnLine)
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

	t.Run("operator query_filter merges into the request params", func(t *testing.T) {
		t.Parallel()

		mux := newPreflightMux(t)
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

		// #1 (backlog) and #2 (in-progress) are active; #3 (done) is terminal
		// and is filtered client-side regardless of the server-side filter.
		if len(issues) != 2 {
			t.Fatalf("len = %d, want 2 (active-state issues only)", len(issues))
		}
	})

	t.Run("unset query_filter sends only the adapter-owned params", func(t *testing.T) {
		t.Parallel()

		mux := newPreflightMux(t)
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

		mux := newPreflightMux(t)
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

		mux := newPreflightMux(t)
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

// --- FetchIssuesByStates ---

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

// --- reportUnresolvedLabels ---

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

// --- warnUnrecognizedFilterKeys ---

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

// --- fetchLabelNames ---

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

// --- FetchIssueStatesByIDs / FetchIssueStatesByIdentifiers ---

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

		_, err := a.fetchBlockers(context.Background(), "42")
		assertTrackerErrorKind(t, err, domain.ErrTrackerNotFound)
	})
}

// --- TransitionIssue ---

func TestTransitionIssue(t *testing.T) {
	t.Parallel()

	t.Run("invalid target state is rejected before any request", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name   string
			target string
		}{
			{"empty target", ""},
			{"unconfigured target", "not-a-configured-state"},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				a := mustAdapter(t, newPreflightMux(t)) // no write route registered; any request fails the test

				err := a.TransitionIssue(context.Background(), "42", tt.target)
				assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)
			})
		}
	})

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

	t.Run("no-op transition performs no label work", func(t *testing.T) {
		t.Parallel()

		mux := newPreflightMux(t)
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues/{index}", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"number":42,"state":"closed","labels":[{"id":502,"name":"done"}],"pull_request":null}`)) //nolint:errcheck // test helper
		})
		// No labels/DELETE/POST/PATCH route registered: currentLabel already
		// equals the target, so no label work and no native reconcile is due.
		a := mustAdapter(t, mux)

		if err := a.TransitionIssue(context.Background(), "42", "done"); err != nil {
			t.Fatalf("TransitionIssue(42, done) = %v, want nil (no-op, already in target state)", err)
		}
	})

	t.Run("active-to-terminal transition removes the prior label by id, attaches the target by id, and closes natively", func(t *testing.T) {
		t.Parallel()

		mux := newPreflightMux(t)
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues/{index}", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "issue_full.json")) //nolint:errcheck // test helper (open, in-progress label id 3)
		})
		var labelsCalls atomic.Int32
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/labels", func(w http.ResponseWriter, r *http.Request) {
			labelsCalls.Add(1)
			if got := r.URL.Query().Get("limit"); got != "50" {
				t.Errorf("labels request limit = %q, want %q", got, "50")
			}
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "labels_page1.json")) //nolint:errcheck // test helper
		})
		var deleteCalls atomic.Int32
		mux.HandleFunc("DELETE /api/v1/repos/"+testOwner+"/"+testRepo+"/issues/42/labels/{id}", func(w http.ResponseWriter, r *http.Request) {
			deleteCalls.Add(1)
			if got := r.PathValue("id"); got != "501" {
				t.Errorf("DELETE label id = %q, want %q (catalog id of in-progress, not the issue's own label id or the name)", got, "501")
			}
			w.WriteHeader(http.StatusNoContent)
		})
		var attachBody struct {
			Labels []int64 `json:"labels"`
		}
		mux.HandleFunc("POST /api/v1/repos/"+testOwner+"/"+testRepo+"/issues/42/labels", func(w http.ResponseWriter, r *http.Request) {
			decodeRequestBody(t, r, &attachBody)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`)) //nolint:errcheck // test helper
		})
		var patchBody map[string]json.RawMessage
		mux.HandleFunc("PATCH /api/v1/repos/"+testOwner+"/"+testRepo+"/issues/{index}", func(w http.ResponseWriter, r *http.Request) {
			decodeRequestBody(t, r, &patchBody)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`)) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, mux)

		if err := a.TransitionIssue(context.Background(), "42", "done"); err != nil {
			t.Fatalf("TransitionIssue(42, done): %v", err)
		}

		if got := labelsCalls.Load(); got != 1 {
			t.Errorf("labels catalog request count = %d, want 1", got)
		}
		if got := deleteCalls.Load(); got != 1 {
			t.Errorf("DELETE call count = %d, want 1", got)
		}
		if want := []int64{502}; !slices.Equal(attachBody.Labels, want) {
			t.Errorf("attach body labels = %v, want %v (numeric id of done, not the name)", attachBody.Labels, want)
		}
		if len(patchBody) != 1 {
			t.Fatalf("PATCH body has %d keys, want 1 (state only, no state_reason)", len(patchBody))
		}
		var state string
		if err := json.Unmarshal(patchBody["state"], &state); err != nil {
			t.Fatalf("decode PATCH state: %v", err)
		}
		if state != "closed" {
			t.Errorf("PATCH state = %q, want %q", state, "closed")
		}
	})

	t.Run("handoff target swaps the state label but issues no native patch", func(t *testing.T) {
		t.Parallel()

		mux := newPreflightMux(t)
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues/{index}", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "issue_full.json")) //nolint:errcheck // test helper (open, in-progress label)
		})
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/labels", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "labels_page1.json")) //nolint:errcheck // test helper
		})
		var deleteCalls atomic.Int32
		mux.HandleFunc("DELETE /api/v1/repos/"+testOwner+"/"+testRepo+"/issues/42/labels/{id}", func(w http.ResponseWriter, r *http.Request) {
			deleteCalls.Add(1)
			if got := r.PathValue("id"); got != "501" {
				t.Errorf("DELETE label id = %q, want %q", got, "501")
			}
			w.WriteHeader(http.StatusNoContent)
		})
		var attachBody struct {
			Labels []int64 `json:"labels"`
		}
		mux.HandleFunc("POST /api/v1/repos/"+testOwner+"/"+testRepo+"/issues/42/labels", func(w http.ResponseWriter, r *http.Request) {
			decodeRequestBody(t, r, &attachBody)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`)) //nolint:errcheck // test helper
		})
		// No PATCH route registered: a handoff-only target must not touch
		// native open/closed status, so a stray PATCH falls to the catch-all.

		srv := httptest.NewServer(mux)
		defer srv.Close()
		cfg := validConfig(srv.URL)
		cfg["handoff_state"] = "escalated"
		adapter, err := NewGiteaAdapter(cfg)
		if err != nil {
			t.Fatalf("NewGiteaAdapter: %v", err)
		}
		a := adapter.(*GiteaAdapter)

		if err := a.TransitionIssue(context.Background(), "42", "escalated"); err != nil {
			t.Fatalf("TransitionIssue(42, escalated): %v", err)
		}

		if got := deleteCalls.Load(); got != 1 {
			t.Errorf("DELETE call count = %d, want 1", got)
		}
		if want := []int64{503}; !slices.Equal(attachBody.Labels, want) {
			t.Errorf("attach body labels = %v, want %v (numeric id of escalated)", attachBody.Labels, want)
		}
	})

	t.Run("state-label delete 404 is tolerated as already removed", func(t *testing.T) {
		t.Parallel()

		mux := newPreflightMux(t)
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/issues/{index}", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "issue_full.json")) //nolint:errcheck // test helper (open, in-progress label)
		})
		mux.HandleFunc("GET /api/v1/repos/"+testOwner+"/"+testRepo+"/labels", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "labels_page1.json")) //nolint:errcheck // test helper
		})
		mux.HandleFunc("DELETE /api/v1/repos/"+testOwner+"/"+testRepo+"/issues/42/labels/{id}", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			w.Write(loadFixture(t, "error_404.json")) //nolint:errcheck // test helper
		})
		mux.HandleFunc("POST /api/v1/repos/"+testOwner+"/"+testRepo+"/issues/42/labels", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`)) //nolint:errcheck // test helper
		})
		mux.HandleFunc("PATCH /api/v1/repos/"+testOwner+"/"+testRepo+"/issues/{index}", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`)) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, mux)

		if err := a.TransitionIssue(context.Background(), "42", "done"); err != nil {
			t.Fatalf("TransitionIssue(42, done) = %v, want nil (a 404 on label removal is already-removed)", err)
		}
	})
}

// --- CommentIssue ---

func TestCommentIssue(t *testing.T) {
	t.Parallel()

	t.Run("posts the markdown body verbatim and returns nil on 201", func(t *testing.T) {
		t.Parallel()

		const markdown = "## Status update\n\n- retried the failing step\n- **no repro** locally\n"

		mux := newPreflightMux(t)
		var gotBody struct {
			Body string `json:"body"`
		}
		var calls atomic.Int32
		mux.HandleFunc("POST /api/v1/repos/"+testOwner+"/"+testRepo+"/issues/42/comments", func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			decodeRequestBody(t, r, &gotBody)
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":1}`)) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, mux)

		if err := a.CommentIssue(context.Background(), "42", markdown); err != nil {
			t.Fatalf("CommentIssue: %v", err)
		}
		if gotBody.Body != markdown {
			t.Errorf("comment body = %q, want %q (verbatim, no conversion)", gotBody.Body, markdown)
		}
		if got := calls.Load(); got != 1 {
			t.Errorf("call count = %d, want 1", got)
		}
	})
}

// --- Write error mapping ---

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

			err := a.CommentIssue(context.Background(), "42", "body")
			assertTrackerErrorKind(t, err, tt.wantKind)
		})
	}
}

// --- AddLabel ---

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
			w.Write([]byte(`{}`)) //nolint:errcheck // test helper
		})
		// No issue GET and no DELETE route registered: AddLabel is additive
		// and must never read or remove the issue's existing labels.
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
			w.Write([]byte(`{}`)) //nolint:errcheck // test helper
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

// --- ensureLabelID ---

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
