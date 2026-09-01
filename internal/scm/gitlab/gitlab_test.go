package gitlab

import (
	"bytes"
	"context"
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

const testProject = "acme/widgets"

var testEscapedProject = url.PathEscape(testProject)

func assertMessageRedacted(t *testing.T, message, want string, forbidden ...string) {
	t.Helper()
	if !strings.Contains(message, want) {
		t.Errorf("message = %q, want to contain %q", message, want)
	}
	for _, value := range forbidden {
		if strings.Contains(message, value) {
			t.Errorf("message = %q, must not contain %q", message, value)
		}
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
	adaptertest.AssertTrackerErrorKind(t, err, want)
}

// assertTrackerErrorMessageContains asserts err unwraps to a
// *domain.TrackerError whose Message contains want. Tests use this to pin
// the offending key's presence in a rejection message rather than its
// exact wording, which is free to change without breaking the test.
func assertTrackerErrorMessageContains(t *testing.T, err error, want string) {
	t.Helper()
	var te *domain.TrackerError
	if !errors.As(err, &te) {
		t.Fatalf("error type = %T, want *domain.TrackerError", err)
	}
	if !strings.Contains(te.Message, want) {
		t.Errorf("TrackerError.Message = %q, want substring %q", te.Message, want)
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

// handleMethod registers fn for a request matching method against
// escapedPath, so a PUT or POST route registers and dispatches exactly
// like a GET route.
func (s *fakeServer) handleMethod(method, escapedPath string, fn http.HandlerFunc) {
	s.routes[method+" "+escapedPath] = fn
}

// handle registers fn for a GET request against escapedPath, the
// shorthand every read-path test uses.
func (s *fakeServer) handle(escapedPath string, fn http.HandlerFunc) {
	s.handleMethod(http.MethodGet, escapedPath, fn)
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

// withPreflight registers the three construction-preflight routes for
// project on s. Every test that constructs an adapter must install all
// three before the operation under test, because [NewGitLabAdapter] runs
// the preflight synchronously and, with the stock default state lists in
// effect, always performs the label-catalog read.
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
	s.handle("/api/v4/projects/"+escaped+"/labels", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[]`)) //nolint:errcheck // test helper
	})
}

// newPreflightServer returns a [fakeServer] pre-registered with the three
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
// cleanup. s must already carry the three construction-preflight routes;
// see [newPreflightServer].
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

// mustAdapterWithConfig behaves like [mustAdapter] but merges overrides
// onto [validConfig] before construction, letting a test customize state
// lists, handoff_state, or any other adapter config key. s must already
// carry the three construction-preflight routes.
func mustAdapterWithConfig(t *testing.T, s *fakeServer, overrides map[string]any) *GitLabAdapter {
	t.Helper()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)

	cfg := validConfig(testProject)
	cfg["endpoint"] = srv.URL
	maps.Copy(cfg, overrides)
	a, err := NewGitLabAdapter(cfg)
	if err != nil {
		t.Fatalf("NewGitLabAdapter: %v", err)
	}
	return a.(*GitLabAdapter)
}

// readRequestBody reads and returns r's full body, failing the test if the
// read itself fails.
func readRequestBody(t *testing.T, r *http.Request) []byte {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("reading request body: %v", err)
	}
	return body
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

// registerTokenAndProject installs only the token and project preflight
// routes on s for [testProject], leaving the labels route for the caller
// to register. Unlike [withPreflight] it installs no default empty-array
// labels catalog, so a test can substitute its own labels handler.
func registerTokenAndProject(t *testing.T, s *fakeServer) {
	t.Helper()
	s.handle("/api/v4/personal_access_tokens/self", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(loadFixture(t, "token_self.json")) //nolint:errcheck // test helper
	})
	s.handle("/api/v4/projects/"+testEscapedProject, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`)) //nolint:errcheck // test helper
	})
}

// registerPagedLabels installs a two-page project label catalog (page one
// "bug"/"documentation", page two "Review"/"URGENT") on s at the labels
// route for [testProject]. It routes on the request's own "page" query
// parameter rather than on call order, so two independent fetches within
// one construction (the preflight's state-casing read and the
// query_filter diagnostic's read) each see the complete two-page catalog.
func registerPagedLabels(t *testing.T, s *fakeServer, srvURL *string) {
	t.Helper()
	basePath := "/api/v4/projects/" + testEscapedProject + "/labels"
	s.handle(basePath, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "labels_page2.json")) //nolint:errcheck // test helper
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s%s?page=2>; rel="next"`, *srvURL, basePath))
		w.WriteHeader(http.StatusOK)
		w.Write(loadFixture(t, "labels_page1.json")) //nolint:errcheck // test helper
	})
}

// swapDefaultLogger installs a buffer-backed slog default logger and
// restores the prior default in cleanup, returning the buffer.
// NewGitLabAdapter takes its logger from [slog.Default] and offers no
// injection parameter, so this is the only way to capture its diagnostic
// output. The default logger is process-global: a test calling this
// helper, and every one of its subtests, must not call t.Parallel(), or
// its logging assertions and any concurrently running test's adapter
// construction can observe each other's logger.
func swapDefaultLogger(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prior := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prior) })
	return &buf
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
			name                string
			config              map[string]any
			wantKind            domain.TrackerErrorKind
			wantMessageContains string
		}{
			{"missing api_key", map[string]any{"project": testProject, "endpoint": unreachable}, domain.ErrMissingTrackerAPIKey, ""},
			{"empty api_key", map[string]any{"api_key": "", "project": testProject, "endpoint": unreachable}, domain.ErrMissingTrackerAPIKey, ""},
			{"missing project", map[string]any{"api_key": "tok", "endpoint": unreachable}, domain.ErrMissingTrackerProject, ""},
			{"empty project", map[string]any{"api_key": "tok", "project": "", "endpoint": unreachable}, domain.ErrMissingTrackerProject, ""},
			{"malformed endpoint scheme", map[string]any{"api_key": "tok", "project": testProject, "endpoint": "ftp://example.com"}, domain.ErrTrackerPayload, ""},
			{"malformed endpoint no host", map[string]any{"api_key": "tok", "project": testProject, "endpoint": "http://"}, domain.ErrTrackerPayload, ""},

			{"api_key wrong type", map[string]any{"api_key": 4242, "project": testProject, "endpoint": unreachable}, domain.ErrTrackerPayload, "api_key: expected string, got integer"},
			{"project wrong type", map[string]any{"api_key": "tok", "project": true, "endpoint": unreachable}, domain.ErrTrackerPayload, "project: expected string, got boolean"},
			{"query_filter wrong type", map[string]any{"api_key": "tok", "project": testProject, "endpoint": unreachable, "query_filter": 4242}, domain.ErrTrackerPayload, "query_filter: expected string, got integer"},

			{"query_filter malformed percent-encoding", map[string]any{"api_key": "tok", "project": testProject, "endpoint": unreachable, "query_filter": "labels=%zz"}, domain.ErrTrackerPayload, ""},
			{"query_filter semicolon separator", map[string]any{"api_key": "tok", "project": testProject, "endpoint": unreachable, "query_filter": "a=1;b=2"}, domain.ErrTrackerPayload, ""},

			{"query_filter reserved key state", map[string]any{"api_key": "tok", "project": testProject, "endpoint": unreachable, "query_filter": "state=closed"}, domain.ErrTrackerPayload, "state"},
			{"query_filter reserved key issue_type", map[string]any{"api_key": "tok", "project": testProject, "endpoint": unreachable, "query_filter": "issue_type=incident"}, domain.ErrTrackerPayload, "issue_type"},
			{"query_filter reserved key order_by", map[string]any{"api_key": "tok", "project": testProject, "endpoint": unreachable, "query_filter": "order_by=updated_at"}, domain.ErrTrackerPayload, "order_by"},
			{"query_filter reserved key sort", map[string]any{"api_key": "tok", "project": testProject, "endpoint": unreachable, "query_filter": "sort=desc"}, domain.ErrTrackerPayload, "sort"},
			{"query_filter reserved key page", map[string]any{"api_key": "tok", "project": testProject, "endpoint": unreachable, "query_filter": "page=2"}, domain.ErrTrackerPayload, "page"},
			{"query_filter reserved key per_page", map[string]any{"api_key": "tok", "project": testProject, "endpoint": unreachable, "query_filter": "per_page=50"}, domain.ErrTrackerPayload, "per_page"},
			{"query_filter reserved key pagination", map[string]any{"api_key": "tok", "project": testProject, "endpoint": unreachable, "query_filter": "pagination=keyset"}, domain.ErrTrackerPayload, "pagination"},
			{"query_filter reserved key with_labels_details", map[string]any{"api_key": "tok", "project": testProject, "endpoint": unreachable, "query_filter": "with_labels_details=true"}, domain.ErrTrackerPayload, "with_labels_details"},
			{"query_filter multiple reserved keys reports the first in fixed slice order", map[string]any{"api_key": "tok", "project": testProject, "endpoint": unreachable, "query_filter": "sort=desc&per_page=10&state=closed"}, domain.ErrTrackerPayload, "state"},

			{"query_filter unknown key mentioned_by", map[string]any{"api_key": "tok", "project": testProject, "endpoint": unreachable, "query_filter": "mentioned_by=bot"}, domain.ErrTrackerPayload, "mentioned_by"},
			{"query_filter unknown key assignee", map[string]any{"api_key": "tok", "project": testProject, "endpoint": unreachable, "query_filter": "assignee=bot"}, domain.ErrTrackerPayload, "assignee"},
			{"query_filter unknown nested key or[label_name][]", map[string]any{"api_key": "tok", "project": testProject, "endpoint": unreachable, "query_filter": "or[label_name][]=x"}, domain.ErrTrackerPayload, "or[label_name][]"},
			{"query_filter unknown not[] subkey search", map[string]any{"api_key": "tok", "project": testProject, "endpoint": unreachable, "query_filter": "not[search]=x"}, domain.ErrTrackerPayload, "not[search]"},
			{"query_filter unknown not[] subkey bogus", map[string]any{"api_key": "tok", "project": testProject, "endpoint": unreachable, "query_filter": "not[bogus]=x"}, domain.ErrTrackerPayload, "not[bogus]"},
			{"query_filter reserved name inside not[] falls through to the unknown-key rejection", map[string]any{"api_key": "tok", "project": testProject, "endpoint": unreachable, "query_filter": "not[state]=opened"}, domain.ErrTrackerPayload, "not[state]"},
			{"query_filter bare not key", map[string]any{"api_key": "tok", "project": testProject, "endpoint": unreachable, "query_filter": "not=x"}, domain.ErrTrackerPayload, "not"},
			{"query_filter unclosed not[", map[string]any{"api_key": "tok", "project": testProject, "endpoint": unreachable, "query_filter": "not[=x"}, domain.ErrTrackerPayload, "not["},
			{"query_filter bracket nested inside not[] instead of after it", map[string]any{"api_key": "tok", "project": testProject, "endpoint": unreachable, "query_filter": "not[labels[]]=x"}, domain.ErrTrackerPayload, "not[labels[]]"},
			{"query_filter empty key", map[string]any{"api_key": "tok", "project": testProject, "endpoint": unreachable, "query_filter": "=x"}, domain.ErrTrackerPayload, ""},
			{"query_filter multiple unknown keys reports the lexicographically smaller one", map[string]any{"api_key": "tok", "project": testProject, "endpoint": unreachable, "query_filter": "zzz_unknown=1&aaa_unknown=2"}, domain.ErrTrackerPayload, "aaa_unknown"},

			{"query_filter labels value empty", map[string]any{"api_key": "tok", "project": testProject, "endpoint": unreachable, "query_filter": "labels="}, domain.ErrTrackerPayload, "labels"},
			{"query_filter labels bare key with no value", map[string]any{"api_key": "tok", "project": testProject, "endpoint": unreachable, "query_filter": "labels"}, domain.ErrTrackerPayload, "labels"},
			{"query_filter labels whitespace-only value", map[string]any{"api_key": "tok", "project": testProject, "endpoint": unreachable, "query_filter": "labels=%20"}, domain.ErrTrackerPayload, "labels"},
			{"query_filter labels single comma value", map[string]any{"api_key": "tok", "project": testProject, "endpoint": unreachable, "query_filter": "labels=,"}, domain.ErrTrackerPayload, "labels"},
			{"query_filter labels doubled comma value", map[string]any{"api_key": "tok", "project": testProject, "endpoint": unreachable, "query_filter": "labels=,,"}, domain.ErrTrackerPayload, "labels"},
			{"query_filter labels leading comma", map[string]any{"api_key": "tok", "project": testProject, "endpoint": unreachable, "query_filter": "labels=,backlog"}, domain.ErrTrackerPayload, "labels"},
			{"query_filter labels trailing comma", map[string]any{"api_key": "tok", "project": testProject, "endpoint": unreachable, "query_filter": "labels=backlog,"}, domain.ErrTrackerPayload, "labels"},
			{"query_filter negated labels trailing comma", map[string]any{"api_key": "tok", "project": testProject, "endpoint": unreachable, "query_filter": "not[labels]=review,"}, domain.ErrTrackerPayload, "not[labels]"},

			{"query_filter repeated non-array key scope", map[string]any{"api_key": "tok", "project": testProject, "endpoint": unreachable, "query_filter": "scope=all&scope=assigned_to_me"}, domain.ErrTrackerPayload, "scope"},
			{"query_filter repeated non-array key labels", map[string]any{"api_key": "tok", "project": testProject, "endpoint": unreachable, "query_filter": "labels=a&labels=b"}, domain.ErrTrackerPayload, "labels"},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				a, err := NewGitLabAdapter(tt.config)
				assertTrackerErrorKind(t, err, tt.wantKind)
				if a != nil {
					t.Error("adapter should be nil on error")
				}
				if tt.wantMessageContains != "" {
					assertTrackerErrorMessageContains(t, err, tt.wantMessageContains)
				}
			})
		}
	})

	t.Run("invalid endpoint redacts userinfo", func(t *testing.T) {
		t.Parallel()

		const endpoint = "operator:secret@gitlab.example.com/group"
		a, err := NewGitLabAdapter(map[string]any{
			"api_key":  "tok",
			"project":  testProject,
			"endpoint": endpoint,
		})

		assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)
		if a != nil {
			t.Error("adapter should be nil on error")
		}
		var trackerErr *domain.TrackerError
		if !errors.As(err, &trackerErr) {
			t.Fatalf("error type = %T, want *domain.TrackerError", err)
		}
		assertMessageRedacted(t, trackerErr.Message, "gitlab.example.com/group", "operator", "secret")
	})

	t.Run("query_filter colliding spellings name both keys and are order-independent", func(t *testing.T) {
		t.Parallel()

		const unreachable = "http://127.0.0.1:1"

		tests := []struct {
			name    string
			filterA string
			filterB string
			keyOne  string
			keyTwo  string
		}{
			{"labels vs labels[]", "labels=a&labels[]=b", "labels[]=b&labels=a", "labels", "labels[]"},
			{"not[labels] vs not[labels][]", "not[labels]=x&not[labels][]=y", "not[labels][]=y&not[labels]=x", "not[labels]", "not[labels][]"},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				cfgA := map[string]any{"api_key": "tok", "project": testProject, "endpoint": unreachable, "query_filter": tt.filterA}
				cfgB := map[string]any{"api_key": "tok", "project": testProject, "endpoint": unreachable, "query_filter": tt.filterB}

				_, errA := NewGitLabAdapter(cfgA)
				_, errB := NewGitLabAdapter(cfgB)

				assertTrackerErrorKind(t, errA, domain.ErrTrackerPayload)
				assertTrackerErrorKind(t, errB, domain.ErrTrackerPayload)
				assertTrackerErrorMessageContains(t, errA, tt.keyOne)
				assertTrackerErrorMessageContains(t, errA, tt.keyTwo)

				var teA, teB *domain.TrackerError
				if !errors.As(errA, &teA) || !errors.As(errB, &teB) {
					t.Fatalf("error type = %T / %T, want *domain.TrackerError for both orderings", errA, errB)
				}
				if teA.Message != teB.Message {
					t.Errorf("colliding-spelling message depends on fragment order: %q vs %q", teA.Message, teB.Message)
				}
			})
		}
	})

	t.Run("query_filter labels value with commas but no empty segment constructs and reaches the request verbatim", func(t *testing.T) {
		t.Parallel()

		s := newPreflightServer(t)
		var gotLabels string
		s.handle("/api/v4/projects/"+testEscapedProject+"/issues", func(w http.ResponseWriter, r *http.Request) {
			gotLabels = r.URL.Query().Get("labels")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("[]")) //nolint:errcheck // test helper
		})
		a := mustAdapterWithConfig(t, s, map[string]any{"query_filter": "labels=bug,documentation"})

		if _, err := a.FetchCandidateIssues(context.Background()); err != nil {
			t.Fatalf("FetchCandidateIssues: %v", err)
		}
		if gotLabels != "bug,documentation" {
			t.Errorf("labels param = %q, want %q", gotLabels, "bug,documentation")
		}
	})

	t.Run("query_filter iids[] array key with multiple values constructs and both reach the wire", func(t *testing.T) {
		t.Parallel()

		s := newPreflightServer(t)
		var gotIIDs []string
		s.handle("/api/v4/projects/"+testEscapedProject+"/issues", func(w http.ResponseWriter, r *http.Request) {
			gotIIDs = r.URL.Query()["iids[]"]
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("[]")) //nolint:errcheck // test helper
		})
		a := mustAdapterWithConfig(t, s, map[string]any{"query_filter": "iids[]=1&iids[]=3"})

		if _, err := a.FetchCandidateIssues(context.Background()); err != nil {
			t.Fatalf("FetchCandidateIssues: %v", err)
		}
		if want := []string{"1", "3"}; !slices.Equal(gotIIDs, want) {
			t.Errorf("iids[] = %v, want %v", gotIIDs, want)
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
		if ga := a.(*GitLabAdapter); ga.queryFilter != nil {
			t.Errorf("queryFilter = %v, want nil", ga.queryFilter)
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

// --- parseQueryFilter ---

func TestParseQueryFilter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		raw        string
		wantErr    bool
		wantKind   domain.TrackerErrorKind
		wantValues url.Values
	}{
		{"empty fragment returns nil values and no error", "", false, "", nil},
		{"whitespace-only fragment returns nil values and no error", "   ", false, "", nil},
		{"malformed percent-encoding is rejected", "labels=%zz", true, domain.ErrTrackerPayload, nil},
		{"semicolon separator is rejected", "a=1;b=2", true, domain.ErrTrackerPayload, nil},
		{"a reserved key is rejected", "state=closed", true, domain.ErrTrackerPayload, nil},
		{"an unknown key is rejected", "mentioned_by=bot", true, domain.ErrTrackerPayload, nil},
		{"an empty comma segment is rejected", "labels=backlog,", true, domain.ErrTrackerPayload, nil},
		{"a repeated non-array key is rejected", "scope=all&scope=assigned_to_me", true, domain.ErrTrackerPayload, nil},
		{"colliding spellings of one parameter are rejected", "labels=a&labels[]=b", true, domain.ErrTrackerPayload, nil},
		{
			"a valid fragment is accepted",
			"scope=assigned_to_me&not[labels]=needs-triage",
			false, "",
			url.Values{"scope": {"assigned_to_me"}, "not[labels]": {"needs-triage"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			values, err := parseQueryFilter(tt.raw)

			if tt.wantErr {
				assertTrackerErrorKind(t, err, tt.wantKind)
				if values != nil {
					t.Errorf("parseQueryFilter(%q) values = %v, want nil on error", tt.raw, values)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseQueryFilter(%q) unexpected error: %v", tt.raw, err)
			}
			if !maps.EqualFunc(values, tt.wantValues, slices.Equal) {
				t.Errorf("parseQueryFilter(%q) = %v, want %v", tt.raw, values, tt.wantValues)
			}
		})
	}
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
		s.handle("/api/v4/projects/"+escaped+"/labels", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`[]`)) //nolint:errcheck // test helper
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
			// GitLab Community Edition's issue-links route carries no
			// "blocks" relation, so every candidate's empty list is
			// declared complete rather than something a resolver owes
			// a read for.
			adaptertest.AssertCandidateBlockerSource(t, registry.BlockersUnsupported, iss, 0)
		}
		if got := calls.Load(); got != 2 {
			t.Errorf("call count = %d, want 2 (two pages)", got)
		}
		if got := strings.Count(buf.String(), "kept first of multiple matching state labels"); got != 1 {
			t.Errorf("multi-match WARN count = %d, want 1 (iid 7 carries both backlog and done labels)\noutput: %s", got, buf.String())
		}
		if !strings.Contains(buf.String(), "issue_identifier=7") {
			t.Errorf("multi-match WARN missing issue_identifier=7\noutput: %s", buf.String())
		}

		// The multi-label WARN identifies the issue with issue_identifier
		// on every forge; it must not also carry iid or issue_index on
		// that one record, even though iid legitimately survives on other
		// GitLab log records this work does not touch.
		var warnLine string
		for line := range strings.SplitSeq(buf.String(), "\n") {
			if strings.Contains(line, "kept first of multiple matching state labels") {
				warnLine = line
				break
			}
		}
		if warnLine == "" {
			t.Fatal("could not locate the multi-match WARN record in the captured log")
		}
		if !strings.Contains(warnLine, "backlog") || !strings.Contains(warnLine, "done") {
			t.Errorf("multi-match WARN does not name both matching labels: %q", warnLine)
		}
		if strings.Contains(warnLine, "iid=") {
			t.Errorf("multi-match WARN record carries iid: %q", warnLine)
		}
		if strings.Contains(warnLine, "issue_index=") {
			t.Errorf("multi-match WARN record carries issue_index: %q", warnLine)
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

	t.Run("query_filter merges into the request and the returned set matches active-state filtering", func(t *testing.T) {
		t.Parallel()

		s := newPreflightServer(t)
		var gotQuery url.Values
		s.handle("/api/v4/projects/"+testEscapedProject+"/issues", func(w http.ResponseWriter, r *http.Request) {
			gotQuery = r.URL.Query()
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "issues_candidates_page2.json")) //nolint:errcheck // test helper
		})
		a := mustAdapterWithConfig(t, s, map[string]any{"query_filter": "scope=assigned_to_me&not[labels]=needs-triage"})

		issues, err := a.FetchCandidateIssues(context.Background())
		if err != nil {
			t.Fatalf("FetchCandidateIssues: %v", err)
		}

		assertQueryParams(t, "candidate", gotQuery, map[string]string{
			"state": "opened", "issue_type": "issue", "per_page": "100",
			"order_by": "created_at", "sort": "asc",
			"scope": "assigned_to_me", "not[labels]": "needs-triage",
		})
		wantOrder := []string{"1", "2", "5", "7"}
		if len(issues) != len(wantOrder) {
			t.Fatalf("len = %d, want %d: got identifiers %v", len(issues), len(wantOrder), identifiersOf(issues))
		}
		for i, want := range wantOrder {
			if issues[i].Identifier != want {
				t.Errorf("issues[%d].Identifier = %q, want %q", i, issues[i].Identifier, want)
			}
		}
	})

	t.Run("unset query_filter produces exactly the six adapter-owned params", func(t *testing.T) {
		t.Parallel()

		s := newPreflightServer(t)
		var gotQuery url.Values
		s.handle("/api/v4/projects/"+testEscapedProject+"/issues", func(w http.ResponseWriter, r *http.Request) {
			gotQuery = r.URL.Query()
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("[]")) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, s)

		if _, err := a.FetchCandidateIssues(context.Background()); err != nil {
			t.Fatalf("FetchCandidateIssues: %v", err)
		}

		assertQueryParams(t, "candidate", gotQuery, map[string]string{
			"state": "opened", "issue_type": "issue", "scope": "all",
			"per_page": "100", "order_by": "created_at", "sort": "asc",
		})
		if a.queryFilter != nil {
			t.Errorf("queryFilter = %v, want nil", a.queryFilter)
		}
	})

	t.Run("query_filter is not mutated across two sequential fetches", func(t *testing.T) {
		t.Parallel()

		s := newPreflightServer(t)
		var queries []url.Values
		s.handle("/api/v4/projects/"+testEscapedProject+"/issues", func(w http.ResponseWriter, r *http.Request) {
			queries = append(queries, r.URL.Query())
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("[]")) //nolint:errcheck // test helper
		})
		a := mustAdapterWithConfig(t, s, map[string]any{"query_filter": "scope=assigned_to_me&not[labels]=needs-triage"})

		before := maps.Clone(a.queryFilter)

		if _, err := a.FetchCandidateIssues(context.Background()); err != nil {
			t.Fatalf("FetchCandidateIssues (first): %v", err)
		}
		if _, err := a.FetchCandidateIssues(context.Background()); err != nil {
			t.Fatalf("FetchCandidateIssues (second): %v", err)
		}

		if len(queries) != 2 {
			t.Fatalf("request count = %d, want 2", len(queries))
		}
		if !maps.EqualFunc(queries[0], queries[1], slices.Equal) {
			t.Errorf("sequential fetches produced different queries: %v vs %v", queries[0], queries[1])
		}
		if !maps.EqualFunc(a.queryFilter, before, slices.Equal) {
			t.Errorf("queryFilter mutated across fetches: got %v, want %v", a.queryFilter, before)
		}
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

		s := newPreflightServer(t)
		var calls int
		s.handle("/api/v4/projects/"+testEscapedProject+"/issues", func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("[]")) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, s)

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

	t.Run("query_filter merges into the opened half and leaves the closed half untouched", func(t *testing.T) {
		t.Parallel()

		s := newPreflightServer(t)
		var openedQuery, closedQuery url.Values
		s.handle("/api/v4/projects/"+testEscapedProject+"/issues", func(w http.ResponseWriter, r *http.Request) {
			switch state := r.URL.Query().Get("state"); state {
			case "opened":
				openedQuery = r.URL.Query()
				w.WriteHeader(http.StatusOK)
				w.Write(loadFixture(t, "issues_open_by_states.json")) //nolint:errcheck // test helper
			case "closed":
				closedQuery = r.URL.Query()
				w.WriteHeader(http.StatusOK)
				w.Write(loadFixture(t, "issues_closed_by_states.json")) //nolint:errcheck // test helper
			default:
				t.Errorf("unexpected state query param: %q", state)
				w.WriteHeader(http.StatusBadRequest)
			}
		})
		a := mustAdapterWithConfig(t, s, map[string]any{"query_filter": "scope=assigned_to_me&not[labels]=needs-triage"})

		if _, err := a.FetchIssuesByStates(context.Background(), []string{"in-progress", "done"}); err != nil {
			t.Fatalf("FetchIssuesByStates: %v", err)
		}

		assertQueryParams(t, "opened-state", openedQuery, map[string]string{
			"state": "opened", "issue_type": "issue", "per_page": "100",
			"order_by": "created_at", "sort": "asc",
			"scope": "assigned_to_me", "not[labels]": "needs-triage",
		})
		assertQueryParams(t, "closed-state", closedQuery, map[string]string{
			"state": "closed", "issue_type": "issue", "scope": "all",
			"per_page": "100", "order_by": "created_at", "sort": "asc",
		})
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
		adaptertest.AssertIssueNormalized(t, issue)
		adaptertest.AssertCommentsAscending(t, issue.Comments)
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
		adaptertest.AssertEmptyNonNil(t, comments, "FetchIssueComments")
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
		adaptertest.AssertStateMapOmitsMissing(t, []string{"10", "20", "30"}, states)
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

	t.Run("query_filter is configured but never merged into the batch request", func(t *testing.T) {
		t.Parallel()

		s := newPreflightServer(t)
		var gotQuery url.Values
		s.handle("/api/v4/projects/"+testEscapedProject+"/issues", func(w http.ResponseWriter, r *http.Request) {
			gotQuery = r.URL.Query()
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`[{"iid": 10, "state": "opened", "issue_type": "issue", "labels": ["in-progress"]}]`)) //nolint:errcheck // test helper
		})
		a := mustAdapterWithConfig(t, s, map[string]any{"query_filter": "scope=assigned_to_me&not[labels]=needs-triage"})

		if _, err := a.FetchIssueStatesByIDs(context.Background(), []string{"10"}); err != nil {
			t.Fatalf("FetchIssueStatesByIDs: %v", err)
		}

		if got, want := gotQuery.Get("state"), "all"; got != want {
			t.Errorf("state = %q, want %q", got, want)
		}
		if got, want := gotQuery.Get("scope"), "all"; got != want {
			t.Errorf("scope = %q, want %q (query_filter must not reach the batch route)", got, want)
		}
		if got, want := gotQuery.Get("per_page"), "100"; got != want {
			t.Errorf("per_page = %q, want %q", got, want)
		}
		if want := []string{"10"}; !slices.Equal(gotQuery["iids[]"], want) {
			t.Errorf("iids[] = %v, want %v", gotQuery["iids[]"], want)
		}
		if len(gotQuery) != 4 {
			t.Errorf("batch request query = %v, want exactly 4 keys (state, scope, per_page, iids[]), no operator parameter", gotQuery)
		}
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

	t.Run("query_filter is configured but never merged into the batch request", func(t *testing.T) {
		t.Parallel()

		s := newPreflightServer(t)
		var gotQuery url.Values
		s.handle("/api/v4/projects/"+testEscapedProject+"/issues", func(w http.ResponseWriter, r *http.Request) {
			gotQuery = r.URL.Query()
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`[{"iid": 10, "state": "opened", "issue_type": "issue", "labels": ["in-progress"]}]`)) //nolint:errcheck // test helper
		})
		a := mustAdapterWithConfig(t, s, map[string]any{"query_filter": "scope=assigned_to_me&not[labels]=needs-triage"})

		if _, err := a.FetchIssueStatesByIdentifiers(context.Background(), []string{"10"}); err != nil {
			t.Fatalf("FetchIssueStatesByIdentifiers: %v", err)
		}

		if got, want := gotQuery.Get("scope"), "all"; got != want {
			t.Errorf("scope = %q, want %q (query_filter must not reach the batch route)", got, want)
		}
		if len(gotQuery) != 4 {
			t.Errorf("batch request query = %v, want exactly 4 keys (state, scope, per_page, iids[]), no operator parameter", gotQuery)
		}
	})
}

// --- TransitionIssue ---

func TestTransitionIssue(t *testing.T) {
	t.Parallel()

	t.Run("active to terminal transition swaps the state label and closes the issue", func(t *testing.T) {
		t.Parallel()

		s := newPreflightServer(t)
		var getCalls, putCalls atomic.Int32
		var putBody []byte
		s.handle("/api/v4/projects/"+testEscapedProject+"/issues/42", func(w http.ResponseWriter, r *http.Request) {
			getCalls.Add(1)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"iid":42,"state":"opened","issue_type":"issue","labels":["review"]}`)) //nolint:errcheck // test helper
		})
		s.handleMethod(http.MethodPut, "/api/v4/projects/"+testEscapedProject+"/issues/42", func(w http.ResponseWriter, r *http.Request) {
			putCalls.Add(1)
			putBody = readRequestBody(t, r)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`)) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, s)

		if err := a.TransitionIssue(context.Background(), "42", "done"); err != nil {
			t.Fatalf("TransitionIssue: %v", err)
		}
		if got := getCalls.Load(); got != 1 {
			t.Errorf("GET call count = %d, want 1", got)
		}
		if got := putCalls.Load(); got != 1 {
			t.Errorf("PUT call count = %d, want 1", got)
		}
		want := `{"state_event":"close","add_labels":["done"],"remove_labels":["review"]}`
		if string(putBody) != want {
			t.Errorf("PUT body = %s, want %s", putBody, want)
		}
	})

	t.Run("handoff target also configured active leaves native state untouched", func(t *testing.T) {
		t.Parallel()

		s := newPreflightServer(t)
		var putBody []byte
		s.handle("/api/v4/projects/"+testEscapedProject+"/issues/7", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"iid":7,"state":"opened","issue_type":"issue","labels":["backlog"]}`)) //nolint:errcheck // test helper
		})
		s.handleMethod(http.MethodPut, "/api/v4/projects/"+testEscapedProject+"/issues/7", func(w http.ResponseWriter, r *http.Request) {
			putBody = readRequestBody(t, r)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`)) //nolint:errcheck // test helper
		})
		a := mustAdapterWithConfig(t, s, map[string]any{"handoff_state": "review"})

		if err := a.TransitionIssue(context.Background(), "7", "review"); err != nil {
			t.Fatalf("TransitionIssue: %v", err)
		}
		want := `{"add_labels":["review"],"remove_labels":["backlog"]}`
		if string(putBody) != want {
			t.Errorf("PUT body = %s, want %s (no state_event: review is also an active state and the issue is already open)", putBody, want)
		}
	})

	t.Run("handoff target outside both lists leaves a closed issue closed", func(t *testing.T) {
		t.Parallel()

		s := newPreflightServer(t)
		var putBody []byte
		s.handle("/api/v4/projects/"+testEscapedProject+"/issues/9", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"iid":9,"state":"closed","issue_type":"issue","labels":["done"]}`)) //nolint:errcheck // test helper
		})
		s.handleMethod(http.MethodPut, "/api/v4/projects/"+testEscapedProject+"/issues/9", func(w http.ResponseWriter, r *http.Request) {
			putBody = readRequestBody(t, r)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`)) //nolint:errcheck // test helper
		})
		a := mustAdapterWithConfig(t, s, map[string]any{
			"active_states":   []string{"backlog", "in-progress"},
			"terminal_states": []string{"done", "wontfix"},
			"handoff_state":   "needs-review",
		})

		if err := a.TransitionIssue(context.Background(), "9", "needs-review"); err != nil {
			t.Fatalf("TransitionIssue: %v", err)
		}
		want := `{"add_labels":["needs-review"],"remove_labels":["done"]}`
		if string(putBody) != want {
			t.Errorf("PUT body = %s, want %s (no state_event: a handoff-only target never drives native state, and the issue stays closed)", putBody, want)
		}
	})

	t.Run("invalid target state rejected before any request", func(t *testing.T) {
		t.Parallel()

		tests := []string{"", "not-a-configured-state"}
		for _, target := range tests {
			t.Run(fmt.Sprintf("target=%q", target), func(t *testing.T) {
				t.Parallel()

				a := mustAdapter(t, newPreflightServer(t)) // no issues route registered; any request fails the test

				err := a.TransitionIssue(context.Background(), "1", target)
				assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)
			})
		}
	})

	t.Run("non-issue work item rejected before any write", func(t *testing.T) {
		t.Parallel()

		s := newPreflightServer(t)
		s.handle("/api/v4/projects/"+testEscapedProject+"/issues/100", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"iid":100,"state":"opened","issue_type":"task","labels":["review"]}`)) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, s) // no PUT route registered; a PUT would fail the test

		err := a.TransitionIssue(context.Background(), "100", "done")
		assertTrackerErrorKind(t, err, domain.ErrTrackerNotFound)
	})

	t.Run("a converged transition issues no request", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name   string
			labels string
		}{
			{"exact casing match", `["done"]`},
			{"issue holds a different stored casing of the same state", `["Done"]`},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				s := newPreflightServer(t)
				var getCalls atomic.Int32
				s.handle("/api/v4/projects/"+testEscapedProject+"/issues/5", func(w http.ResponseWriter, r *http.Request) {
					getCalls.Add(1)
					w.WriteHeader(http.StatusOK)
					w.Write(fmt.Appendf(nil, `{"iid":5,"state":"closed","issue_type":"issue","labels":%s}`, tt.labels)) //nolint:errcheck // test helper
				})
				a := mustAdapter(t, s) // no PUT route registered; a PUT would fail the test

				if err := a.TransitionIssue(context.Background(), "5", "done"); err != nil {
					t.Fatalf("TransitionIssue: %v", err)
				}
				if got := getCalls.Load(); got != 1 {
					t.Errorf("GET call count = %d, want 1", got)
				}
			})
		}
	})

	t.Run("the target label is sent in its canonical stored casing", func(t *testing.T) {
		t.Parallel()

		s := newPreflightServer(t)
		s.handle("/api/v4/projects/"+testEscapedProject+"/labels", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`[{"name":"Review"},{"name":"Done"}]`)) //nolint:errcheck // test helper
		})
		var putBody []byte
		s.handle("/api/v4/projects/"+testEscapedProject+"/issues/3", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"iid":3,"state":"opened","issue_type":"issue","labels":["review"]}`)) //nolint:errcheck // test helper
		})
		s.handleMethod(http.MethodPut, "/api/v4/projects/"+testEscapedProject+"/issues/3", func(w http.ResponseWriter, r *http.Request) {
			putBody = readRequestBody(t, r)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`)) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, s)

		if err := a.TransitionIssue(context.Background(), "3", "done"); err != nil {
			t.Fatalf("TransitionIssue: %v", err)
		}
		want := `{"state_event":"close","add_labels":["Done"],"remove_labels":["review"]}`
		if string(putBody) != want {
			t.Errorf("PUT body = %s, want %s (add_labels must carry the catalog's stored casing, not the configured spelling)", putBody, want)
		}
	})

	t.Run("every case variant of the outgoing state label is removed in the same request", func(t *testing.T) {
		t.Parallel()

		s := newPreflightServer(t)
		var putBody []byte
		var putCalls atomic.Int32
		s.handle("/api/v4/projects/"+testEscapedProject+"/issues/11", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"iid":11,"state":"opened","issue_type":"issue","labels":["REVIEW","review"]}`)) //nolint:errcheck // test helper
		})
		s.handleMethod(http.MethodPut, "/api/v4/projects/"+testEscapedProject+"/issues/11", func(w http.ResponseWriter, r *http.Request) {
			putCalls.Add(1)
			putBody = readRequestBody(t, r)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`)) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, s)

		if err := a.TransitionIssue(context.Background(), "11", "done"); err != nil {
			t.Fatalf("TransitionIssue: %v", err)
		}
		if got := putCalls.Load(); got != 1 {
			t.Errorf("PUT call count = %d, want 1", got)
		}
		want := `{"state_event":"close","add_labels":["done"],"remove_labels":["REVIEW","review"]}`
		if string(putBody) != want {
			t.Errorf("PUT body = %s, want %s (both case variants removed, in issue-label order)", putBody, want)
		}
	})

	t.Run("a pre-existing case variant of the incoming label is removed without cancelling the attach", func(t *testing.T) {
		t.Parallel()

		s := newPreflightServer(t)
		var putBody []byte
		s.handle("/api/v4/projects/"+testEscapedProject+"/issues/13", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"iid":13,"state":"opened","issue_type":"issue","labels":["backlog","DONE"]}`)) //nolint:errcheck // test helper
		})
		s.handleMethod(http.MethodPut, "/api/v4/projects/"+testEscapedProject+"/issues/13", func(w http.ResponseWriter, r *http.Request) {
			putBody = readRequestBody(t, r)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`)) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, s)

		if err := a.TransitionIssue(context.Background(), "13", "done"); err != nil {
			t.Fatalf("TransitionIssue: %v", err)
		}
		want := `{"state_event":"close","add_labels":["done"],"remove_labels":["backlog","DONE"]}`
		if string(putBody) != want {
			t.Errorf("PUT body = %s, want %s (remove_labels must not carry the byte-identical done, which would cancel the attach)", putBody, want)
		}
	})
}

// --- CommentIssue ---

func TestCommentIssue(t *testing.T) {
	t.Parallel()

	t.Run("posts the note verbatim including newlines", func(t *testing.T) {
		t.Parallel()

		const text = "Automated check failed.\nSee the CI log for detail."

		s := newPreflightServer(t)
		var postBody []byte
		var postCalls atomic.Int32
		s.handleMethod(http.MethodPost, "/api/v4/projects/"+testEscapedProject+"/issues/1/notes", func(w http.ResponseWriter, r *http.Request) {
			postCalls.Add(1)
			postBody = readRequestBody(t, r)
			w.WriteHeader(http.StatusCreated)
			w.Write(loadFixture(t, "note_created.json")) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, s)

		if err := a.CommentIssue(context.Background(), "1", text); err != nil {
			t.Fatalf("CommentIssue: %v", err)
		}
		if got := postCalls.Load(); got != 1 {
			t.Errorf("POST call count = %d, want 1", got)
		}
		want := `{"body":` + strconv.Quote(text) + `}`
		if string(postBody) != want {
			t.Errorf("POST body = %s, want %s (text must pass through byte-identical, including newlines)", postBody, want)
		}
	})

	t.Run("a 2xx that created no note reports ErrTrackerPayload", func(t *testing.T) {
		t.Parallel()

		s := newPreflightServer(t)
		s.handleMethod(http.MethodPost, "/api/v4/projects/"+testEscapedProject+"/issues/2/notes", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusAccepted)
			w.Write(loadFixture(t, "note_quick_action.json")) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, s)

		err := a.CommentIssue(context.Background(), "2", "/close")
		assertTrackerErrorKind(t, err, domain.ErrTrackerPayload)
	})

	t.Run("a note that also executed quick actions logs a WARN naming the command keys", func(t *testing.T) {
		t.Parallel()

		s := newPreflightServer(t)
		s.handleMethod(http.MethodPost, "/api/v4/projects/"+testEscapedProject+"/issues/3/notes", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":900,"body":"","commands_changes":{"add_label_ids":[5]}}`)) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, s)

		var buf bytes.Buffer
		a.log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		if err := a.CommentIssue(context.Background(), "3", "/close\nSome text"); err != nil {
			t.Fatalf("CommentIssue: %v", err)
		}
		output := buf.String()
		if !strings.Contains(output, "gitlab executed quick actions in a comment body") {
			t.Errorf("log output missing the quick-action WARN\noutput: %s", output)
		}
		if !strings.Contains(output, "add_label_ids") {
			t.Errorf("log output missing the executed command key add_label_ids\noutput: %s", output)
		}
	})
}

// --- AddLabel ---

func TestAddLabel(t *testing.T) {
	t.Parallel()

	t.Run("additive attach preserving existing labels sends only add_labels", func(t *testing.T) {
		t.Parallel()

		s := newPreflightServer(t) // labels catalog route returns an empty array; no issue-read route registered
		var putBody []byte
		var putCalls atomic.Int32
		s.handleMethod(http.MethodPut, "/api/v4/projects/"+testEscapedProject+"/issues/4", func(w http.ResponseWriter, r *http.Request) {
			putCalls.Add(1)
			putBody = readRequestBody(t, r)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`)) //nolint:errcheck // test helper
		})
		a := mustAdapter(t, s)

		if err := a.AddLabel(context.Background(), "4", "needs-human"); err != nil {
			t.Fatalf("AddLabel: %v", err)
		}
		if got := putCalls.Load(); got != 1 {
			t.Errorf("PUT call count = %d, want 1", got)
		}
		want := `{"add_labels":["needs-human"]}`
		if string(putBody) != want {
			t.Errorf("PUT body = %s, want %s (no labels key, no remove_labels key)", putBody, want)
		}

		// The request carries only add_labels, no labels or remove_labels
		// key, so it adds without touching labels already on the issue.
		before := []string{"existing"}
		after := append(slices.Clone(before), "needs-human")
		adaptertest.AssertLabelAddIsAdditive(t, before, after, "needs-human")
	})

	t.Run("empty or whitespace-only label attaches nothing and warns", func(t *testing.T) {
		t.Parallel()

		tests := []string{"", "  "}
		for _, label := range tests {
			t.Run(fmt.Sprintf("label=%q", label), func(t *testing.T) {
				t.Parallel()

				s := newFakeServer(t)
				withPreflight(t, s, testProject)
				var labelCalls atomic.Int32
				s.handle("/api/v4/projects/"+testEscapedProject+"/labels", func(w http.ResponseWriter, r *http.Request) {
					labelCalls.Add(1)
					w.WriteHeader(http.StatusOK)
					w.Write([]byte(`[]`)) //nolint:errcheck // test helper
				})
				a := mustAdapter(t, s) // no PUT route registered; a PUT would fail the test

				afterConstruction := labelCalls.Load()

				var buf bytes.Buffer
				a.log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

				if err := a.AddLabel(context.Background(), "1", label); err != nil {
					t.Fatalf("AddLabel: %v", err)
				}
				if got := labelCalls.Load(); got != afterConstruction {
					t.Errorf("labels-route call count = %d, want %d (no catalog read for an empty label)", got, afterConstruction)
				}
				if !strings.Contains(buf.String(), "gitlab add_label received an empty label; nothing attached") {
					t.Errorf("log output missing the empty-label WARN\noutput: %s", buf.String())
				}
			})
		}
	})
}

// --- Label catalog pagination (multi-page regression) ---

func TestLabelCatalogPagination(t *testing.T) {
	t.Parallel()

	t.Run("construction resolves a state label defined only on page two", func(t *testing.T) {
		t.Parallel()

		s := newFakeServer(t)
		s.handle("/api/v4/personal_access_tokens/self", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "token_self.json")) //nolint:errcheck // test helper
		})
		s.handle("/api/v4/projects/"+testEscapedProject, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`)) //nolint:errcheck // test helper
		})

		var srvURL string
		var calls atomic.Int32
		basePath := "/api/v4/projects/" + testEscapedProject + "/labels"
		s.handle(basePath, func(w http.ResponseWriter, r *http.Request) {
			n := calls.Add(1)
			if n == 1 {
				next := fmt.Sprintf(`<%s%s?page=2>; rel="next"`, srvURL, basePath)
				w.Header().Set("Link", next)
				w.WriteHeader(http.StatusOK)
				w.Write(loadFixture(t, "labels_page1.json")) //nolint:errcheck // test helper
				return
			}
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "labels_page2.json")) //nolint:errcheck // test helper
		})

		srv := httptest.NewServer(s)
		defer srv.Close()
		srvURL = srv.URL

		cfg := validConfig(testProject)
		cfg["endpoint"] = srv.URL
		adapter, err := NewGitLabAdapter(cfg)
		if err != nil {
			t.Fatalf("NewGitLabAdapter: %v", err)
		}
		a := adapter.(*GitLabAdapter)

		if got := calls.Load(); got != 2 {
			t.Errorf("labels-route call count = %d, want 2 (page one plus page two)", got)
		}
		if got := a.stateLabelCasing["review"]; got != "Review" {
			t.Errorf(`stateLabelCasing["review"] = %q, want %q (page two carries the stored casing)`, got, "Review")
		}
	})

	t.Run("AddLabel sends page two's stored casing on the attach", func(t *testing.T) {
		t.Parallel()

		s := newFakeServer(t)
		withPreflight(t, s, testProject) // construction consumes the default empty-array catalog once

		var putBody []byte
		s.handleMethod(http.MethodPut, "/api/v4/projects/"+testEscapedProject+"/issues/6", func(w http.ResponseWriter, r *http.Request) {
			putBody = readRequestBody(t, r)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`)) //nolint:errcheck // test helper
		})

		srv := httptest.NewServer(s)
		defer srv.Close()
		srvURL := srv.URL

		cfg := validConfig(testProject)
		cfg["endpoint"] = srv.URL
		adapter, err := NewGitLabAdapter(cfg)
		if err != nil {
			t.Fatalf("NewGitLabAdapter: %v", err)
		}
		a := adapter.(*GitLabAdapter)

		// Re-registered only after construction, so the two-page catalog
		// below is exercised by AddLabel's own fresh call, not by the
		// preflight's earlier, already-consumed empty-array read.
		var calls atomic.Int32
		basePath := "/api/v4/projects/" + testEscapedProject + "/labels"
		s.handle(basePath, func(w http.ResponseWriter, r *http.Request) {
			n := calls.Add(1)
			if n == 1 {
				next := fmt.Sprintf(`<%s%s?page=2>; rel="next"`, srvURL, basePath)
				w.Header().Set("Link", next)
				w.WriteHeader(http.StatusOK)
				w.Write(loadFixture(t, "labels_page1.json")) //nolint:errcheck // test helper
				return
			}
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "labels_page2.json")) //nolint:errcheck // test helper
		})

		if err := a.AddLabel(context.Background(), "6", "review"); err != nil {
			t.Fatalf("AddLabel: %v", err)
		}
		if got := calls.Load(); got != 2 {
			t.Errorf("labels-route call count = %d, want 2 (page one plus page two)", got)
		}
		want := `{"add_labels":["Review"]}`
		if string(putBody) != want {
			t.Errorf("PUT body = %s, want %s (the attach must use page two's stored casing)", putBody, want)
		}
	})
}

// --- query_filter labels diagnostic (W1, W2, wildcard exemptions) ---
//
// NewGitLabAdapter takes its logger from slog.Default(), so every subtest
// here calls swapDefaultLogger and none of them, nor this function itself,
// calls t.Parallel(): the process-global default logger must not be
// swapped concurrently with another test's adapter construction.

const labelAbsentFromCatalogMessage = "gitlab query_filter names a label absent from the project catalog"

func TestQueryFilterLabelsDiagnostic(t *testing.T) {
	t.Run("a name absent from the catalog logs one WARN and the filter still reaches the request", func(t *testing.T) {
		tests := []struct {
			name   string
			filter string
			label  string
		}{
			{"catalog holds a different case", "labels=review", "review"},
			{"catalog holds no such name", "labels=needs-triage", "needs-triage"},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				buf := swapDefaultLogger(t)

				s := newFakeServer(t)
				var srvURL string
				registerTokenAndProject(t, s)
				registerPagedLabels(t, s, &srvURL)
				var gotLabelsParam string
				s.handle("/api/v4/projects/"+testEscapedProject+"/issues", func(w http.ResponseWriter, r *http.Request) {
					gotLabelsParam = r.URL.Query().Get("labels")
					w.WriteHeader(http.StatusOK)
					w.Write([]byte("[]")) //nolint:errcheck // test helper
				})
				srv := httptest.NewServer(s)
				defer srv.Close()
				srvURL = srv.URL

				cfg := validConfig(testProject)
				cfg["endpoint"] = srv.URL
				cfg["query_filter"] = tt.filter

				adapter, err := NewGitLabAdapter(cfg)
				if err != nil {
					t.Fatalf("NewGitLabAdapter(query_filter=%q): %v", tt.filter, err)
				}
				ga := adapter.(*GitLabAdapter)

				if got := strings.Count(buf.String(), labelAbsentFromCatalogMessage); got != 1 {
					t.Errorf("WARN count for %q = %d, want 1\noutput: %s", tt.filter, got, buf.String())
				}
				if want := "label=" + tt.label; !strings.Contains(buf.String(), want) {
					t.Errorf("WARN output missing %q\noutput: %s", want, buf.String())
				}

				if _, err := ga.FetchCandidateIssues(context.Background()); err != nil {
					t.Fatalf("FetchCandidateIssues: %v", err)
				}
				if gotLabelsParam != tt.label {
					t.Errorf("candidate request labels param = %q, want %q", gotLabelsParam, tt.label)
				}
			})
		}
	})

	t.Run("wildcard exemptions", func(t *testing.T) {
		tests := []struct {
			name           string
			filter         string
			wantWarnLabels []string
		}{
			{"bare None is exempt", "labels=None", nil},
			{"bare Any is exempt", "labels=Any", nil},
			{"lowercase any is exempt", "labels=any", nil},
			{"array-form None is exempt", "labels[]=None", nil},
			{"None exempt alongside a real name", "labels=documentation,None", nil},
			{"negated Any is a literal name", "not[labels]=Any", []string{"Any"}},
			{"negated None is a literal name", "not[labels]=None", []string{"None"}},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				buf := swapDefaultLogger(t)

				s := newFakeServer(t)
				var srvURL string
				registerTokenAndProject(t, s)
				registerPagedLabels(t, s, &srvURL)
				srv := httptest.NewServer(s)
				defer srv.Close()
				srvURL = srv.URL

				cfg := validConfig(testProject)
				cfg["endpoint"] = srv.URL
				cfg["query_filter"] = tt.filter

				if _, err := NewGitLabAdapter(cfg); err != nil {
					t.Fatalf("NewGitLabAdapter(query_filter=%q): %v", tt.filter, err)
				}

				if got := strings.Count(buf.String(), labelAbsentFromCatalogMessage); got != len(tt.wantWarnLabels) {
					t.Errorf("WARN count for %q = %d, want %d\noutput: %s", tt.filter, got, len(tt.wantWarnLabels), buf.String())
				}
				for _, label := range tt.wantWarnLabels {
					if want := "label=" + label; !strings.Contains(buf.String(), want) {
						t.Errorf("WARN output missing %q\noutput: %s", want, buf.String())
					}
				}
			})
		}
	})

	t.Run("a name repeated across segments warns once per negation family", func(t *testing.T) {
		tests := []struct {
			name             string
			filter           string
			wantWarnCount    int
			wantNegatedAttrs []string
		}{
			{"comma-separated duplicate dedupes to one warn", "labels=needs-triage,needs-triage", 1, []string{"negated=false"}},
			{"repeated array key with the same value dedupes to one warn", "labels[]=needs-triage&labels[]=needs-triage", 1, []string{"negated=false"}},
			{
				"the same text under labels and not[labels] each warn once, distinguished by the negated attribute",
				"labels=needs-triage&not[labels]=needs-triage",
				2,
				[]string{"negated=false", "negated=true"},
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				buf := swapDefaultLogger(t)

				s := newFakeServer(t)
				var srvURL string
				registerTokenAndProject(t, s)
				registerPagedLabels(t, s, &srvURL)
				srv := httptest.NewServer(s)
				defer srv.Close()
				srvURL = srv.URL

				cfg := validConfig(testProject)
				cfg["endpoint"] = srv.URL
				cfg["query_filter"] = tt.filter

				if _, err := NewGitLabAdapter(cfg); err != nil {
					t.Fatalf("NewGitLabAdapter(query_filter=%q): %v", tt.filter, err)
				}

				if got := strings.Count(buf.String(), labelAbsentFromCatalogMessage); got != tt.wantWarnCount {
					t.Errorf("WARN count for %q = %d, want %d\noutput: %s", tt.filter, got, tt.wantWarnCount, buf.String())
				}
				for _, attr := range tt.wantNegatedAttrs {
					if !strings.Contains(buf.String(), attr) {
						t.Errorf("WARN output missing %q\noutput: %s", attr, buf.String())
					}
				}
			})
		}
	})

	t.Run("a failed catalog read during the diagnostic does not fail construction", func(t *testing.T) {
		buf := swapDefaultLogger(t)

		s := newFakeServer(t)
		var srvURL string
		registerTokenAndProject(t, s)
		basePath := "/api/v4/projects/" + testEscapedProject + "/labels"
		var rootCalls atomic.Int32
		s.handle(basePath, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("page") == "2" {
				w.WriteHeader(http.StatusOK)
				w.Write(loadFixture(t, "labels_page2.json")) //nolint:errcheck // test helper
				return
			}
			if rootCalls.Add(1) > 1 {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(`{"message":"internal server error"}`)) //nolint:errcheck // test helper
				return
			}
			w.Header().Set("Link", fmt.Sprintf(`<%s%s?page=2>; rel="next"`, srvURL, basePath))
			w.WriteHeader(http.StatusOK)
			w.Write(loadFixture(t, "labels_page1.json")) //nolint:errcheck // test helper
		})
		srv := httptest.NewServer(s)
		defer srv.Close()
		srvURL = srv.URL

		cfg := validConfig(testProject)
		cfg["endpoint"] = srv.URL
		cfg["query_filter"] = "labels=review"

		if _, err := NewGitLabAdapter(cfg); err != nil {
			t.Fatalf("NewGitLabAdapter: unexpected error from a failed diagnostic catalog read: %v", err)
		}

		if got := strings.Count(buf.String(), "gitlab query_filter labels catalog unavailable; label names were not validated"); got != 1 {
			t.Errorf("W2 WARN count = %d, want 1\noutput: %s", got, buf.String())
		}
		if !strings.Contains(buf.String(), "error=") {
			t.Errorf("W2 WARN missing an \"error\" attribute\noutput: %s", buf.String())
		}
	})
}

// --- Error-category mapping for a rejected write ---

func TestWriteErrorMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		status   int
		fixture  string
		inline   string
		wantKind domain.TrackerErrorKind
	}{
		{"400 bad request", http.StatusBadRequest, "error_400.json", "", domain.ErrTrackerPayload},
		{"401 unauthorized", http.StatusUnauthorized, "error_401.json", "", domain.ErrTrackerAuth},
		{"403 insufficient scope", http.StatusForbidden, "error_403.json", "", domain.ErrTrackerAuth},
		{"404 issue not found", http.StatusNotFound, "error_404_issue.json", "", domain.ErrTrackerNotFound},
		{"404 project not found", http.StatusNotFound, "error_404_project.json", "", domain.ErrTrackerNotFound},
		{"429 rate limited", http.StatusTooManyRequests, "", "{}", domain.ErrTrackerAPI},
		{"500 server error", http.StatusInternalServerError, "error_500.json", "", domain.ErrTrackerTransport},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := newPreflightServer(t)
			s.handleMethod(http.MethodPut, "/api/v4/projects/"+testEscapedProject+"/issues/1", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				if tt.fixture != "" {
					w.Write(loadFixture(t, tt.fixture)) //nolint:errcheck // test helper
					return
				}
				w.Write([]byte(tt.inline)) //nolint:errcheck // test helper
			})
			a := mustAdapter(t, s)

			err := a.AddLabel(context.Background(), "1", "needs-human")
			assertTrackerErrorKind(t, err, tt.wantKind)
		})
	}
}

// --- Identifier guard across all three writes ---

func TestWriteIdentifierGuard(t *testing.T) {
	t.Parallel()

	tests := []string{"abc", "1.5", "-1", "0", "01", " ", ""}

	for _, raw := range tests {
		t.Run(fmt.Sprintf("issueID=%q", raw), func(t *testing.T) {
			t.Parallel()

			a := mustAdapter(t, newPreflightServer(t)) // no write route registered; any request fails the test

			err := a.TransitionIssue(context.Background(), raw, "done")
			assertTrackerErrorKind(t, err, domain.ErrTrackerNotFound)

			err = a.CommentIssue(context.Background(), raw, "text")
			assertTrackerErrorKind(t, err, domain.ErrTrackerNotFound)

			err = a.AddLabel(context.Background(), raw, "needs-human")
			assertTrackerErrorKind(t, err, domain.ErrTrackerNotFound)
		})
	}
}
