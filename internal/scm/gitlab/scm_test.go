package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sortie-ai/sortie/internal/adaptertest"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

// scmOwner and scmRepo are the single-segment owner and repo used by every
// SCM read test that does not specifically exercise project addressing.
const (
	scmOwner     = "acme"
	scmRepo      = "widgets"
	testPRNumber = 6
)

// mustSCMAdapter constructs a *GitLabSCMAdapter against endpoint with a
// throwaway token, or fails the test.
func mustSCMAdapter(t *testing.T, endpoint string) *GitLabSCMAdapter {
	t.Helper()
	a, err := NewGitLabSCMAdapter(map[string]any{
		"endpoint": endpoint,
		"api_key":  "test-token",
	})
	if err != nil {
		t.Fatalf("NewGitLabSCMAdapter: %v", err)
	}
	return a.(*GitLabSCMAdapter)
}

// newCapturingLogger returns a logger backed by a buffer so a test can
// assert on WARN attributes without depending on slog.Default().
func newCapturingLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

// scmErrorStatusCase pins one row of the §3.7 HTTP-status-to-SCMErrorKind
// mapping every read method's error coverage exercises.
type scmErrorStatusCase struct {
	name   string
	status int
	body   []byte
	want   domain.SCMErrorKind
}

// scmErrorStatusCases returns the four statuses R26 requires every read
// method to cover: 401, 404, 429, and a 5xx.
func scmErrorStatusCases(t *testing.T) []scmErrorStatusCase {
	t.Helper()
	return []scmErrorStatusCase{
		{"401 unauthorized maps to ErrSCMAuth", http.StatusUnauthorized, loadFixture(t, "error_401.json"), domain.ErrSCMAuth},
		{"404 not found maps to ErrSCMNotFound", http.StatusNotFound, loadFixture(t, "error_404_issue.json"), domain.ErrSCMNotFound},
		{"429 rate limited maps to ErrSCMAPI", http.StatusTooManyRequests, loadFixture(t, "error_429.json"), domain.ErrSCMAPI},
		{"500 server error maps to ErrSCMTransport", http.StatusInternalServerError, loadFixture(t, "error_500.json"), domain.ErrSCMTransport},
	}
}

// runSCMErrorStatusTable exercises the shared statuses (R26's four plus
// 405 and 409, which R27 pins to ErrSCMAPI rather than ErrSCMConflict)
// against call, which issues exactly one adapter read against a server
// that answers every request with the given status and body.
func runSCMErrorStatusTable(t *testing.T, call func(t *testing.T, srv *httptest.Server) error) {
	t.Helper()

	cases := append(scmErrorStatusCases(t),
		scmErrorStatusCase{"405 on a read route maps to ErrSCMAPI, never ErrSCMConflict", http.StatusMethodNotAllowed, loadFixture(t, "error_405.json"), domain.ErrSCMAPI},
		scmErrorStatusCase{"409 on a read route maps to ErrSCMAPI, never ErrSCMConflict", http.StatusConflict, loadFixture(t, "error_409.json"), domain.ErrSCMAPI},
	)

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = w.Write(tt.body)
			}))
			defer srv.Close()

			err := call(t, srv)
			adaptertest.AssertSCMErrorKind(t, err, tt.want)
		})
	}
}

// --- NewGitLabSCMAdapter validation ---

func TestNewGitLabSCMAdapter_Validation(t *testing.T) {
	t.Parallel()

	t.Run("missing api_key returns ErrSCMAuth", func(t *testing.T) {
		t.Parallel()

		_, err := NewGitLabSCMAdapter(map[string]any{"endpoint": "http://gitlab.invalid"})
		adaptertest.AssertSCMErrorKind(t, err, domain.ErrSCMAuth)
	})

	t.Run("empty api_key returns ErrSCMAuth", func(t *testing.T) {
		t.Parallel()

		_, err := NewGitLabSCMAdapter(map[string]any{"api_key": "", "endpoint": "http://gitlab.invalid"})
		adaptertest.AssertSCMErrorKind(t, err, domain.ErrSCMAuth)
	})

	t.Run("malformed endpoint returns ErrSCMPayload", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name     string
			endpoint string
		}{
			{"no scheme or host", "not-a-url-at-all"},
			{"unsupported scheme", "ftp://gitlab.example.com"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				_, err := NewGitLabSCMAdapter(map[string]any{"api_key": "test-token", "endpoint": tt.endpoint})
				adaptertest.AssertSCMErrorKind(t, err, domain.ErrSCMPayload)
			})
		}
	})
}

// --- NewGitLabSCMAdapter defaults ---

func TestNewGitLabSCMAdapter_Defaults(t *testing.T) {
	t.Parallel()

	t.Run("endpoint omitted constructs without error", func(t *testing.T) {
		t.Parallel()

		// No httptest server: verifying the default host is gitlab.com would
		// require dialing it, which no test in this package may do. Success
		// here proves the default is applied rather than treated as a
		// missing required key.
		if _, err := NewGitLabSCMAdapter(map[string]any{"api_key": "test-token"}); err != nil {
			t.Fatalf("NewGitLabSCMAdapter: unexpected error: %v", err)
		}
	})

	t.Run("appends /api/v4 exactly once regardless of a trailing slash or existing suffix", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name     string
			endpoint func(base string) string
		}{
			{"bare host", func(base string) string { return base }},
			{"trailing slash", func(base string) string { return base + "/" }},
			{"already suffixed", func(base string) string { return base + "/api/v4" }},
			{"already suffixed with trailing slash", func(base string) string { return base + "/api/v4/" }},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				fixture := loadFixture(t, "mr_basic.json")
				var gotPath string
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					gotPath = r.URL.EscapedPath()
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(fixture)
				}))
				defer srv.Close()

				a, err := NewGitLabSCMAdapter(map[string]any{
					"api_key":  "test-token",
					"endpoint": tt.endpoint(srv.URL),
				})
				if err != nil {
					t.Fatalf("NewGitLabSCMAdapter: %v", err)
				}
				if _, err := a.GetMergeability(context.Background(), testPRNumber, scmOwner, scmRepo); err != nil {
					t.Fatalf("GetMergeability: unexpected error: %v", err)
				}

				wantPath := "/api/v4/projects/" + projectPath(scmOwner, scmRepo) + "/merge_requests/" + strconv.Itoa(testPRNumber)
				if gotPath != wantPath {
					t.Errorf("request path = %q, want %q", gotPath, wantPath)
				}
			})
		}
	})

	t.Run("user_agent defaults to sortie/dev", func(t *testing.T) {
		t.Parallel()

		fixture := loadFixture(t, "mr_basic.json")
		var gotUserAgent string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotUserAgent = r.Header.Get("User-Agent")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fixture)
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		if _, err := adapter.GetMergeability(context.Background(), testPRNumber, scmOwner, scmRepo); err != nil {
			t.Fatalf("GetMergeability: unexpected error: %v", err)
		}
		if gotUserAgent != "sortie/dev" {
			t.Errorf("User-Agent = %q, want %q", gotUserAgent, "sortie/dev")
		}
	})

	t.Run("a configured user_agent and token reach the request", func(t *testing.T) {
		t.Parallel()

		fixture := loadFixture(t, "mr_basic.json")
		var gotHeaders http.Header
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotHeaders = r.Header.Clone()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fixture)
		}))
		defer srv.Close()

		a, err := NewGitLabSCMAdapter(map[string]any{
			"api_key":    "s3cr3t-token",
			"endpoint":   srv.URL,
			"user_agent": "sortie-test/1.0",
		})
		if err != nil {
			t.Fatalf("NewGitLabSCMAdapter: %v", err)
		}
		if _, err := a.GetMergeability(context.Background(), testPRNumber, scmOwner, scmRepo); err != nil {
			t.Fatalf("GetMergeability: unexpected error: %v", err)
		}

		if got := gotHeaders.Get("PRIVATE-TOKEN"); got != "s3cr3t-token" {
			t.Errorf("PRIVATE-TOKEN = %q, want %q", got, "s3cr3t-token")
		}
		if got := gotHeaders.Get("User-Agent"); got != "sortie-test/1.0" {
			t.Errorf("User-Agent = %q, want %q", got, "sortie-test/1.0")
		}
	})

	t.Run("a project key is ignored: owner and repo arrive per call", func(t *testing.T) {
		t.Parallel()

		fixture := loadFixture(t, "mr_basic.json")
		var gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.EscapedPath()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fixture)
		}))
		defer srv.Close()

		a, err := NewGitLabSCMAdapter(map[string]any{
			"api_key":  "test-token",
			"endpoint": srv.URL,
			"project":  "ignored/project",
		})
		if err != nil {
			t.Fatalf("NewGitLabSCMAdapter: %v", err)
		}
		if _, err := a.GetMergeability(context.Background(), testPRNumber, scmOwner, scmRepo); err != nil {
			t.Fatalf("GetMergeability: unexpected error: %v", err)
		}

		wantPath := "/api/v4/projects/" + projectPath(scmOwner, scmRepo) + "/merge_requests/" + strconv.Itoa(testPRNumber)
		if gotPath != wantPath {
			t.Errorf("request path = %q, want %q (the project config key must not be read)", gotPath, wantPath)
		}
	})
}

// --- NewGitLabSCMAdapter issues no network request ---

func TestNewGitLabSCMAdapter_NoNetworkRequest(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := NewGitLabSCMAdapter(map[string]any{
		"api_key":  "test-token",
		"endpoint": srv.URL,
	}); err != nil {
		t.Fatalf("NewGitLabSCMAdapter: unexpected error: %v", err)
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("requests during construction = %d, want 0", n)
	}
}

// --- Project addressing ---

func TestProjectPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		owner, repo string
	}{
		{"single-segment owner", "acme", "widgets"},
		{"namespace depth three", "acme/team/squad", "widgets"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fixture := loadFixture(t, "mr_basic.json")
			var gotEscapedPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotEscapedPath = r.URL.EscapedPath()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(fixture)
			}))
			defer srv.Close()

			adapter := mustSCMAdapter(t, srv.URL)
			if _, err := adapter.GetMergeability(context.Background(), testPRNumber, tt.owner, tt.repo); err != nil {
				t.Fatalf("GetMergeability: unexpected error: %v", err)
			}

			wantPath := "/api/v4/projects/" + projectPath(tt.owner, tt.repo) + "/merge_requests/" + strconv.Itoa(testPRNumber)
			if gotEscapedPath != wantPath {
				t.Errorf("projectPath(%q, %q): request path = %q, want %q", tt.owner, tt.repo, gotEscapedPath, wantPath)
			}
		})
	}

	t.Run("owner is joined and encoded as one segment, never split or separately encoded", func(t *testing.T) {
		t.Parallel()

		got := projectPath("acme/team/squad", "widgets")
		want := "acme%2Fteam%2Fsquad%2Fwidgets"
		if got != want {
			t.Errorf("projectPath(%q, %q) = %q, want %q", "acme/team/squad", "widgets", got, want)
		}
	})
}

// --- paginateSCM ---

func TestPaginateSCM_FollowsLinkHeader(t *testing.T) {
	t.Parallel()

	decodeStrings := func(body []byte) ([]string, error) {
		var batch []string
		return batch, json.Unmarshal(body, &batch)
	}

	t.Run("follows rel=next across two pages and returns both pages' items", func(t *testing.T) {
		t.Parallel()

		var srv *httptest.Server
		var requestCount atomic.Int32
		srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestCount.Add(1)
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Query().Get("page") == "2" {
				_, _ = w.Write([]byte(`["c"]`))
				return
			}
			w.Header().Set("Link", `<`+srv.URL+`/api/v4/items?page=2>; rel="next"`)
			_, _ = w.Write([]byte(`["a","b"]`))
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		items, err := paginateSCM(context.Background(), adapter.client, adapter.log, "/items", nil, decodeStrings)
		if err != nil {
			t.Fatalf("paginateSCM: unexpected error: %v", err)
		}

		want := []string{"a", "b", "c"}
		if len(items) != len(want) {
			t.Fatalf("paginateSCM() len = %d, want %d", len(items), len(want))
		}
		for i, v := range want {
			if items[i] != v {
				t.Errorf("items[%d] = %q, want %q", i, items[i], v)
			}
		}
		if n := requestCount.Load(); n != 2 {
			t.Errorf("request count = %d, want 2", n)
		}
	})

	t.Run("stops at maxPages and logs a WARN naming the path and the cap, never interpolated", func(t *testing.T) {
		t.Parallel()

		var srv *httptest.Server
		var requestCount atomic.Int32
		srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestCount.Add(1)
			w.Header().Set("Link", `<`+srv.URL+`/api/v4/items?page=next>; rel="next"`)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`["x"]`))
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		log, buf := newCapturingLogger()
		adapter.log = log

		items, err := paginateSCM(context.Background(), adapter.client, adapter.log, "/items", nil, decodeStrings)
		if err != nil {
			t.Fatalf("paginateSCM: unexpected error: %v", err)
		}
		if n := requestCount.Load(); n != int32(maxPages) {
			t.Errorf("request count = %d, want %d (the walk must stop at the cap)", n, maxPages)
		}
		if len(items) != maxPages {
			t.Errorf("paginateSCM() len = %d, want %d", len(items), maxPages)
		}

		out := buf.String()
		if !bytes.Contains(buf.Bytes(), []byte("pagination limit reached")) {
			t.Errorf("log output = %q, want it to contain %q", out, "pagination limit reached")
		}
		if !bytes.Contains(buf.Bytes(), []byte("endpoint=/items")) {
			t.Errorf("log output = %q, want the path carried as the endpoint attribute", out)
		}
		if !bytes.Contains(buf.Bytes(), []byte("max_pages="+strconv.Itoa(maxPages))) {
			t.Errorf("log output = %q, want the cap carried as the max_pages attribute", out)
		}
	})
}

// --- asSCMError passthrough (rule 4 of §3.3.3) ---

func TestAsSCMError_PassthroughDecodeFailure(t *testing.T) {
	t.Parallel()

	t.Run("a paginated route's decode failure surfaces as ErrSCMPayload, not ErrSCMTransport", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`not valid json {{{`))
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		_, err := adapter.ListLabelEvents(context.Background(), testPRNumber, scmOwner, scmRepo)
		adaptertest.AssertSCMErrorKind(t, err, domain.ErrSCMPayload)
	})

	t.Run("a single-object route's decode failure surfaces as ErrSCMPayload, not ErrSCMTransport", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`not valid json {{{`))
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		_, err := adapter.GetMergeability(context.Background(), testPRNumber, scmOwner, scmRepo)
		adaptertest.AssertSCMErrorKind(t, err, domain.ErrSCMPayload)
	})
}

// --- Error status mapping (§3.7) ---

func TestErrorStatusMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
		body   []byte
		want   domain.SCMErrorKind
	}{
		{"400 bad request maps to ErrSCMPayload", http.StatusBadRequest, loadFixture(t, "error_400.json"), domain.ErrSCMPayload},
		{"401 unauthorized maps to ErrSCMAuth", http.StatusUnauthorized, loadFixture(t, "error_401.json"), domain.ErrSCMAuth},
		{"403 forbidden maps to ErrSCMAuth", http.StatusForbidden, loadFixture(t, "error_403.json"), domain.ErrSCMAuth},
		{"404 not found maps to ErrSCMNotFound", http.StatusNotFound, loadFixture(t, "error_404_issue.json"), domain.ErrSCMNotFound},
		{"414 request-uri too long maps to ErrSCMPayload", http.StatusRequestURITooLong, loadFixture(t, "error_414.json"), domain.ErrSCMPayload},
		{"422 unprocessable entity maps to ErrSCMPayload", http.StatusUnprocessableEntity, loadFixture(t, "error_422.json"), domain.ErrSCMPayload},
		{"429 rate limited maps to ErrSCMAPI", http.StatusTooManyRequests, loadFixture(t, "error_429.json"), domain.ErrSCMAPI},
		{"500 server error maps to ErrSCMTransport", http.StatusInternalServerError, loadFixture(t, "error_500.json"), domain.ErrSCMTransport},
		{"405 on a read route maps to ErrSCMAPI, never ErrSCMConflict", http.StatusMethodNotAllowed, loadFixture(t, "error_405.json"), domain.ErrSCMAPI},
		{"409 on a read route maps to ErrSCMAPI, never ErrSCMConflict", http.StatusConflict, loadFixture(t, "error_409.json"), domain.ErrSCMAPI},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = w.Write(tt.body)
			}))
			defer srv.Close()

			adapter := mustSCMAdapter(t, srv.URL)
			_, err := adapter.GetMergeability(context.Background(), testPRNumber, scmOwner, scmRepo)
			adaptertest.AssertSCMErrorKind(t, err, tt.want)
		})
	}
}

// --- Registration (R34, AC1) ---

func TestGitLabSCMRegistration(t *testing.T) {
	t.Parallel()

	if !registry.SCMAdapters.Has("gitlab") {
		t.Fatal(`SCMAdapters.Has("gitlab") = false, want true`)
	}

	constructor, err := registry.SCMAdapters.Get("gitlab")
	if err != nil {
		t.Fatalf(`SCMAdapters.Get("gitlab") = %v, want registered constructor`, err)
	}

	adapter, err := constructor(map[string]any{
		"api_key":  "test-token",
		"endpoint": "http://gitlab.invalid",
	})
	if err != nil {
		t.Fatalf("registered gitlab SCM constructor(...) = %v, want nil error", err)
	}
	if _, ok := adapter.(*GitLabSCMAdapter); !ok {
		t.Errorf("registered gitlab SCM constructor(...) = %T, want *GitLabSCMAdapter", adapter)
	}

	if !registry.Trackers.Has("gitlab") {
		t.Error(`Trackers.Has("gitlab") = false, want true (SCM registration must not disturb the tracker registration)`)
	}
}

// --- Write-path import boundary (R38) ---

func TestGitLabSCMWriteBoundary(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir(.): %v", err)
	}

	banned := []string{
		"github.com/sortie-ai/sortie/internal/scm/github",
		"github.com/sortie-ai/sortie/internal/scm/gitea",
		"github.com/sortie-ai/sortie/internal/tracker/",
		"github.com/sortie-ai/sortie/internal/orchestrator",
	}

	fset := token.NewFileSet()
	checked := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		checked++

		f, parseErr := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if parseErr != nil {
			t.Fatalf("ParseFile(%s): %v", name, parseErr)
		}

		for _, imp := range f.Imports {
			importPath := strings.Trim(imp.Path.Value, `"`)
			for _, forbidden := range banned {
				if strings.Contains(importPath, forbidden) {
					t.Errorf("%s imports %q, want no import matching %q", name, importPath, forbidden)
				}
			}
		}
	}

	if checked == 0 {
		t.Fatal("no .go files were checked, want at least one")
	}
}
