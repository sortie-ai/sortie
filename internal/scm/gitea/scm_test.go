package gitea

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sortie-ai/sortie/internal/adaptertest"
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
	adaptertest.AssertSCMErrorKind(t, err, want)
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

	t.Run("api_key wrong type returns a payload error distinct from absent", func(t *testing.T) {
		t.Parallel()

		_, err := NewGiteaSCMAdapter(map[string]any{"api_key": 4242, "endpoint": "http://gitea.invalid"})

		var se *domain.SCMError
		if !errors.As(err, &se) {
			t.Fatalf("error type = %T, want *domain.SCMError", err)
		}
		if se.Kind != domain.ErrSCMPayload {
			t.Errorf("SCMError.Kind = %q, want %q", se.Kind, domain.ErrSCMPayload)
		}
		if se.Message != "api_key: expected string, got integer" {
			t.Errorf("SCMError.Message = %q, want %q", se.Message, "api_key: expected string, got integer")
		}
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

// TestNewGiteaSCMAdapter_RejectsInvalidEndpoint proves the endpoint values
// in endpointRejectionCases fail construction with ErrSCMPayload; none of
// the config maps here register a server, since construction performs no
// network I/O.
func TestNewGiteaSCMAdapter_RejectsInvalidEndpoint(t *testing.T) {
	t.Parallel()

	for _, tt := range endpointRejectionCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			a, err := NewGiteaSCMAdapter(map[string]any{
				"api_key":  "test-token",
				"endpoint": tt.endpoint,
			})

			assertSCMErrorKind(t, err, domain.ErrSCMPayload)
			if a != nil {
				t.Errorf("NewGiteaSCMAdapter(endpoint=%q) adapter = %v, want nil", tt.endpoint, a)
			}
		})
	}
}

// TestNewGiteaSCMAdapter_RedactsUserinfoInEndpointError proves the
// credential disclosure this fix closes stays closed for the SCM adapter,
// which previously accepted a userinfo-bearing endpoint silently.
func TestNewGiteaSCMAdapter_RedactsUserinfoInEndpointError(t *testing.T) {
	t.Parallel()

	const endpoint = "https://operator:s3cr3t@fd00::1:3000"

	_, err := NewGiteaSCMAdapter(map[string]any{
		"api_key":  "test-token",
		"endpoint": endpoint,
	})

	var se *domain.SCMError
	if !errors.As(err, &se) {
		t.Fatalf("NewGiteaSCMAdapter(endpoint=%q) error type = %T, want *domain.SCMError", endpoint, err)
	}
	if se.Kind != domain.ErrSCMPayload {
		t.Errorf("NewGiteaSCMAdapter(endpoint=%q) SCMError.Kind = %q, want %q", endpoint, se.Kind, domain.ErrSCMPayload)
	}
	if !strings.Contains(se.Message, "fd00::1:3000") {
		t.Errorf("NewGiteaSCMAdapter(endpoint=%q) message = %q, want it to contain the redacted host %q", endpoint, se.Message, "fd00::1:3000")
	}
	if strings.Contains(se.Message, "operator") || strings.Contains(se.Message, "s3cr3t") {
		t.Errorf("NewGiteaSCMAdapter(endpoint=%q) message = %q, must not leak userinfo credentials", endpoint, se.Message)
	}
}

// TestNewGiteaSCMAdapter_EmptyEndpointMessageIsPinned pins the pre-existing
// empty-endpoint message: the shared httpkit.ParseEndpoint guard must not
// have changed this operator-facing text.
func TestNewGiteaSCMAdapter_EmptyEndpointMessageIsPinned(t *testing.T) {
	t.Parallel()

	_, err := NewGiteaSCMAdapter(map[string]any{"api_key": "test-token"})

	var se *domain.SCMError
	if !errors.As(err, &se) {
		t.Fatalf("NewGiteaSCMAdapter(missing endpoint) error type = %T, want *domain.SCMError", err)
	}
	if se.Kind != domain.ErrSCMPayload {
		t.Errorf("NewGiteaSCMAdapter(missing endpoint) SCMError.Kind = %q, want %q", se.Kind, domain.ErrSCMPayload)
	}
	const want = "missing required config key: endpoint"
	if se.Message != want {
		t.Errorf("NewGiteaSCMAdapter(missing endpoint) message = %q, want %q", se.Message, want)
	}
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

// --- paginateSCM ---

func TestGiteaPaginateSCM(t *testing.T) {
	t.Parallel()

	t.Run("a canceled context converts to an SCM transport error at this package's boundary", func(t *testing.T) {
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
		_, err := paginateSCM(ctx, client, "/items", func(body []byte) ([]int, error) {
			var batch []int
			return batch, json.Unmarshal(body, &batch)
		})
		// The shared page-number walk returns the client's classified error
		// unconverted (plain context.Canceled here, since the walk checks
		// ctx.Err() before issuing a request); paginateSCM converts it to the
		// SCM boundary, which is the assertion this test proves.
		assertSCMErrorKind(t, err, domain.ErrSCMTransport)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("paginateSCM() error = %v, want to wrap context.Canceled", err)
		}
		if n := requestCount.Load(); n != 0 {
			t.Errorf("request count = %d, want 0 (a canceled context must be checked before the first request)", n)
		}
	})
}

// --- Write-path transport ---

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
}
