package gitea

import (
	"context"
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

// mustSCMAdapter constructs a *GiteaSCMAdapter against endpoint with a
// throwaway token, or fails the test.
func mustSCMAdapter(t *testing.T, endpoint string) *GiteaSCMAdapter {
	t.Helper()
	a, err := NewGiteaSCMAdapter(map[string]any{
		"endpoint": endpoint,
		"api_key":  "test-token",
	})
	if err != nil {
		t.Fatalf("NewGiteaSCMAdapter: %v", err)
	}
	return a.(*GiteaSCMAdapter)
}

func assertSCMErrorKind(t *testing.T, err error, want domain.SCMErrorKind) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with kind %q, got nil", want)
	}
	var se *domain.SCMError
	if !errors.As(err, &se) {
		t.Fatalf("error type = %T, want *domain.SCMError", err)
	}
	if se.Kind != want {
		t.Errorf("SCMError.Kind = %q, want %q", se.Kind, want)
	}
}

// buildIntRange returns a JSON array of n consecutive integers starting at
// start, modeling a page-number response without hand-authoring a repetitive
// fixture file.
func buildIntRange(t *testing.T, start, n int) []byte {
	t.Helper()
	values := make([]int, n)
	for i := range n {
		values[i] = start + i
	}
	data, err := json.Marshal(values)
	if err != nil {
		t.Fatalf("marshal int range: %v", err)
	}
	return data
}

// --- NewGiteaSCMAdapter ---

func TestNewGiteaSCMAdapter(t *testing.T) {
	t.Parallel()

	t.Run("missing api_key returns an auth error", func(t *testing.T) {
		t.Parallel()

		_, err := NewGiteaSCMAdapter(map[string]any{"endpoint": "http://gitea.invalid"})
		assertSCMErrorKind(t, err, domain.ErrSCMAuth)
	})

	t.Run("missing endpoint returns a payload error", func(t *testing.T) {
		t.Parallel()

		_, err := NewGiteaSCMAdapter(map[string]any{"api_key": "test-token"})
		assertSCMErrorKind(t, err, domain.ErrSCMPayload)
	})

	t.Run("constructs without any network request", func(t *testing.T) {
		t.Parallel()

		var requests atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		if _, err := NewGiteaSCMAdapter(map[string]any{
			"endpoint": srv.URL,
			"api_key":  "test-token",
		}); err != nil {
			t.Fatalf("NewGiteaSCMAdapter: unexpected error: %v", err)
		}
		if n := requests.Load(); n != 0 {
			t.Errorf("requests during construction = %d, want 0 (the SCM adapter has nothing to preflight)", n)
		}
	})

	t.Run("appends /api/v1 exactly once regardless of a trailing slash or existing suffix", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name     string
			endpoint func(base string) string
		}{
			{"bare host", func(base string) string { return base }},
			{"trailing slash", func(base string) string { return base + "/" }},
			{"already suffixed", func(base string) string { return base + "/api/v1" }},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				prFixture := loadFixture(t, "pr_clean.json")
				var gotPath string
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					gotPath = r.URL.Path
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(prFixture)
				}))
				defer srv.Close()

				adapter := mustSCMAdapter(t, tt.endpoint(srv.URL))
				if _, err := adapter.GetMergeability(context.Background(), 6, testOwner, testRepo); err != nil {
					t.Fatalf("GetMergeability: unexpected error: %v", err)
				}
				wantPath := "/api/v1/repos/" + testOwner + "/" + testRepo + "/pulls/6"
				if gotPath != wantPath {
					t.Errorf("request path = %q, want %q", gotPath, wantPath)
				}
			})
		}
	})

	t.Run("sends the token and user-agent headers", func(t *testing.T) {
		t.Parallel()

		prFixture := loadFixture(t, "pr_clean.json")
		var gotHeaders http.Header
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotHeaders = r.Header.Clone()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(prFixture)
		}))
		defer srv.Close()

		a, err := NewGiteaSCMAdapter(map[string]any{
			"endpoint":   srv.URL,
			"api_key":    "test-token",
			"user_agent": "sortie-test/1.0",
		})
		if err != nil {
			t.Fatalf("NewGiteaSCMAdapter: %v", err)
		}
		if _, err := a.(*GiteaSCMAdapter).GetMergeability(context.Background(), 6, testOwner, testRepo); err != nil {
			t.Fatalf("GetMergeability: unexpected error: %v", err)
		}

		if got := gotHeaders.Get("Authorization"); got != "token test-token" {
			t.Errorf("Authorization = %q, want %q", got, "token test-token")
		}
		if got := gotHeaders.Get("User-Agent"); got != "sortie-test/1.0" {
			t.Errorf("User-Agent = %q, want %q", got, "sortie-test/1.0")
		}
	})

	t.Run("defaults user-agent to sortie/dev", func(t *testing.T) {
		t.Parallel()

		prFixture := loadFixture(t, "pr_clean.json")
		var gotUserAgent string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotUserAgent = r.Header.Get("User-Agent")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(prFixture)
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		if _, err := adapter.GetMergeability(context.Background(), 6, testOwner, testRepo); err != nil {
			t.Fatalf("GetMergeability: unexpected error: %v", err)
		}
		if gotUserAgent != "sortie/dev" {
			t.Errorf("User-Agent = %q, want %q", gotUserAgent, "sortie/dev")
		}
	})

	t.Run("a 404 response reaches the caller as ErrSCMNotFound", func(t *testing.T) {
		t.Parallel()

		errorBody := loadFixture(t, "error_404.json")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write(errorBody)
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		_, err := adapter.GetMergeability(context.Background(), 6, testOwner, testRepo)
		assertSCMErrorKind(t, err, domain.ErrSCMNotFound)
	})
}

// --- SCM registration ---

func TestGiteaSCMRegistration(t *testing.T) {
	t.Parallel()

	if !registry.SCMAdapters.Has("gitea") {
		t.Fatal(`SCMAdapters.Has("gitea") = false, want true`)
	}

	constructor, err := registry.SCMAdapters.Get("gitea")
	if err != nil {
		t.Fatalf(`SCMAdapters.Get("gitea") = %v, want registered constructor`, err)
	}

	adapter, err := constructor(map[string]any{
		"api_key":  "test-token",
		"endpoint": "http://gitea.invalid",
	})
	if err != nil {
		t.Fatalf("registered gitea SCM constructor(...) = %v, want nil error", err)
	}
	if _, ok := adapter.(*GiteaSCMAdapter); !ok {
		t.Errorf("registered gitea SCM constructor(...) = %T, want *GiteaSCMAdapter", adapter)
	}

	if !registry.Trackers.Has("gitea") {
		t.Error(`Trackers.Has("gitea") = false, want true (SCM registration must not disturb the tracker registration)`)
	}
}

// --- giteaToSCMError ---

func TestGiteaToSCMError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    error
		wantKind domain.SCMErrorKind
	}{
		{
			name:     "401 unauthorized classifies as an auth error",
			input:    &domain.TrackerError{Kind: domain.ErrTrackerAuth, Message: "invalid credentials"},
			wantKind: domain.ErrSCMAuth,
		},
		{
			name:     "403 forbidden classifies as an auth error",
			input:    &domain.TrackerError{Kind: domain.ErrTrackerAuth, Message: "insufficient permissions"},
			wantKind: domain.ErrSCMAuth,
		},
		{
			name:     "404 not found classifies as a not-found error",
			input:    &domain.TrackerError{Kind: domain.ErrTrackerNotFound, Message: "not found"},
			wantKind: domain.ErrSCMNotFound,
		},
		{
			name:     "5xx server error classifies as a transport error",
			input:    &domain.TrackerError{Kind: domain.ErrTrackerTransport, Message: "server error"},
			wantKind: domain.ErrSCMTransport,
		},
		{
			name:     "api error classifies as an api error",
			input:    &domain.TrackerError{Kind: domain.ErrTrackerAPI, Message: "rate limited"},
			wantKind: domain.ErrSCMAPI,
		},
		{
			name:     "payload error classifies as a payload error",
			input:    &domain.TrackerError{Kind: domain.ErrTrackerPayload, Message: "bad request"},
			wantKind: domain.ErrSCMPayload,
		},
		{
			name:     "unrecognized tracker error kind falls back to an api error",
			input:    &domain.TrackerError{Kind: "unknown_kind", Message: "x"},
			wantKind: domain.ErrSCMAPI,
		},
		{
			name:     "a non-tracker error wraps as a transport error",
			input:    errors.New("dial tcp: connection refused"),
			wantKind: domain.ErrSCMTransport,
		},
		{
			name:     "405 method not allowed promotes to a conflict error",
			input:    &domain.TrackerError{Kind: domain.ErrTrackerAPI, Message: "POST /repos/acme/widgets/pulls/6/merge: method not allowed: The PR is already merged"},
			wantKind: domain.ErrSCMConflict,
		},
		{
			name:     "409 conflict promotes to a conflict error",
			input:    &domain.TrackerError{Kind: domain.ErrTrackerAPI, Message: "POST /repos/acme/widgets/pulls/6/merge: conflict: head out of date"},
			wantKind: domain.ErrSCMConflict,
		},
		{
			name:     "the word conflict alone, without the formatted marker, does not promote",
			input:    &domain.TrackerError{Kind: domain.ErrTrackerAPI, Message: "GET /repos/acme/widgets: there was a conflict in the request"},
			wantKind: domain.ErrSCMAPI,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := giteaToSCMError(tt.input)

			if got.Kind != tt.wantKind {
				t.Errorf("giteaToSCMError(%v).Kind = %q, want %q", tt.input, got.Kind, tt.wantKind)
			}
			if got.Err != tt.input {
				t.Errorf("giteaToSCMError(%v).Err = %v, want the original error preserved", tt.input, got.Err)
			}
		})
	}

	t.Run("decode failure surfaces as a payload error", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`not valid json {{{`))
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		_, err := adapter.GetMergeability(context.Background(), 6, testOwner, testRepo)
		assertSCMErrorKind(t, err, domain.ErrSCMPayload)
	})
}

// --- paginatePages ---

func TestGiteaPaginatePages(t *testing.T) {
	t.Parallel()

	t.Run("accumulates across a full page and a short page", func(t *testing.T) {
		t.Parallel()

		const limit = 50
		page1 := buildIntRange(t, 1, limit)
		page2 := buildIntRange(t, limit+1, 2)

		var requestCount atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := requestCount.Add(1)
			w.Header().Set("Content-Type", "application/json")
			if n == 1 {
				assertQueryParams(t, "page 1", r.URL.Query(), map[string]string{"page": "1", "limit": "50"})
				_, _ = w.Write(page1)
				return
			}
			assertQueryParams(t, "page 2", r.URL.Query(), map[string]string{"page": "2", "limit": "50"})
			_, _ = w.Write(page2)
		}))
		defer srv.Close()

		client := newGiteaClient(srv.URL, "test-token", "sortie/test")
		got, err := paginatePages(context.Background(), client, "/items", func(body []byte) ([]int, error) {
			var batch []int
			if jsonErr := json.Unmarshal(body, &batch); jsonErr != nil {
				return nil, jsonErr
			}
			return batch, nil
		})
		if err != nil {
			t.Fatalf("paginatePages: unexpected error: %v", err)
		}
		if len(got) != limit+2 {
			t.Fatalf("paginatePages() len = %d, want %d", len(got), limit+2)
		}
		if got[0] != 1 || got[len(got)-1] != limit+2 {
			t.Errorf("paginatePages() = [%d...%d], want [1...%d]", got[0], got[len(got)-1], limit+2)
		}
		if n := requestCount.Load(); n != 2 {
			t.Errorf("request count = %d, want 2", n)
		}
	})

	t.Run("a single short page stops after one request", func(t *testing.T) {
		t.Parallel()

		var requestCount atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestCount.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[1,2,3]`))
		}))
		defer srv.Close()

		client := newGiteaClient(srv.URL, "test-token", "sortie/test")
		got, err := paginatePages(context.Background(), client, "/items", func(body []byte) ([]int, error) {
			var batch []int
			if jsonErr := json.Unmarshal(body, &batch); jsonErr != nil {
				return nil, jsonErr
			}
			return batch, nil
		})
		if err != nil {
			t.Fatalf("paginatePages: unexpected error: %v", err)
		}
		if len(got) != 3 {
			t.Errorf("paginatePages() len = %d, want 3", len(got))
		}
		if n := requestCount.Load(); n != 1 {
			t.Errorf("request count = %d, want 1 (a page shorter than the limit must stop pagination)", n)
		}
	})

	t.Run("a canceled context returns immediately with no request", func(t *testing.T) {
		t.Parallel()

		var requestCount atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestCount.Add(1)
			_, _ = w.Write([]byte(`[]`))
		}))
		defer srv.Close()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		client := newGiteaClient(srv.URL, "test-token", "sortie/test")
		_, err := paginatePages(ctx, client, "/items", func(body []byte) ([]int, error) {
			var batch []int
			return batch, json.Unmarshal(body, &batch)
		})
		// A canceled context is surfaced through giteaToSCMError like every
		// other error path in paginatePages, so callers get the *domain.SCMError
		// their interface contract promises while the cause stays unwrappable.
		assertSCMErrorKind(t, err, domain.ErrSCMTransport)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("paginatePages() error = %v, want to wrap context.Canceled", err)
		}
		if n := requestCount.Load(); n != 0 {
			t.Errorf("request count = %d, want 0 (a canceled context must be checked before the first request)", n)
		}
	})
}

// --- Write-path transport and import boundary ---

func TestGiteaSCMWriteBoundary(t *testing.T) {
	t.Parallel()

	t.Run("a captured write request carries the token header and the api/v1 path prefix", func(t *testing.T) {
		t.Parallel()

		var gotAuth, gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			gotPath = r.URL.Path
			w.WriteHeader(http.StatusNoContent)
		}))
		defer srv.Close()

		adapter := mustSCMAdapter(t, srv.URL)
		if err := adapter.DeleteBranch(context.Background(), testOwner, testRepo, "feature/done"); err != nil {
			t.Fatalf("DeleteBranch: unexpected error: %v", err)
		}

		if gotAuth != "token test-token" {
			t.Errorf("Authorization = %q, want %q", gotAuth, "token test-token")
		}
		if !strings.HasPrefix(gotPath, "/api/v1/") {
			t.Errorf("request path = %q, want it to start with %q", gotPath, "/api/v1/")
		}
	})

	t.Run("package imports no sibling adapter, no orchestrator, and no gitea sdk", func(t *testing.T) {
		t.Parallel()

		entries, err := os.ReadDir(".")
		if err != nil {
			t.Fatalf("ReadDir(.): %v", err)
		}

		banned := []string{
			"github.com/sortie-ai/sortie/internal/scm/github",
			"github.com/sortie-ai/sortie/internal/orchestrator",
			"github.com/sortie-ai/sortie/internal/tracker/",
			"code.gitea.io/sdk",
		}

		fset := token.NewFileSet()
		checked := 0
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
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
			t.Fatal("no production .go files were checked, want at least one")
		}
	})
}
