package gitea

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

func TestNewGiteaClient(t *testing.T) {
	t.Parallel()

	t.Run("sends token header, accept, user-agent, and api/v1 path", func(t *testing.T) {
		t.Parallel()

		var gotHeaders http.Header
		var gotPath string
		var gotRawQuery string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotHeaders = r.Header.Clone()
			gotPath = r.URL.Path
			gotRawQuery = r.URL.RawQuery
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"login":"sortie-bot"}`)) //nolint:errcheck // test helper
		}))
		defer srv.Close()

		client := newGiteaClient(srv.URL+"/api/v1", "abc123", "sortie/test")
		_, _, err := client.Get(context.Background(), "/user", nil)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}

		if got := gotHeaders.Get("Authorization"); got != "token abc123" {
			t.Errorf("Authorization = %q, want %q", got, "token abc123")
		}
		if got := gotHeaders.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q, want %q", got, "application/json")
		}
		if got := gotHeaders.Get("User-Agent"); got != "sortie/test" {
			t.Errorf("User-Agent = %q, want %q", got, "sortie/test")
		}
		if gotPath != "/api/v1/user" {
			t.Errorf("path = %q, want %q", gotPath, "/api/v1/user")
		}
		if gotRawQuery != "" {
			t.Errorf("raw query = %q, want empty (token must never travel as a query parameter)", gotRawQuery)
		}
	})

	t.Run("trims a trailing slash from baseURL", func(t *testing.T) {
		t.Parallel()

		var gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`)) //nolint:errcheck // test helper
		}))
		defer srv.Close()

		client := newGiteaClient(srv.URL+"/api/v1/", "abc123", "sortie/test")
		_, _, err := client.Get(context.Background(), "/user", nil)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if gotPath != "/api/v1/user" {
			t.Errorf("path = %q, want %q (no doubled slash)", gotPath, "/api/v1/user")
		}
	})
}

func TestClassifyHTTPError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		status   int
		fixture  string
		headers  map[string]string
		wantKind domain.TrackerErrorKind
	}{
		{"400 bad request", http.StatusBadRequest, "error_400.json", nil, domain.ErrTrackerPayload},
		{"401 unauthorized", http.StatusUnauthorized, "error_401.json", nil, domain.ErrTrackerAuth},
		{"403 forbidden", http.StatusForbidden, "error_403.json", nil, domain.ErrTrackerAuth},
		{"404 not found", http.StatusNotFound, "error_404.json", nil, domain.ErrTrackerNotFound},
		{"412 precondition failed", http.StatusPreconditionFailed, "error_412.json", nil, domain.ErrTrackerPayload},
		{"422 unprocessable entity", http.StatusUnprocessableEntity, "error_422.json", nil, domain.ErrTrackerPayload},
		{"423 locked", http.StatusLocked, "error_423.json", nil, domain.ErrTrackerAPI},
		{"429 too many requests", http.StatusTooManyRequests, "", map[string]string{"Retry-After": "30"}, domain.ErrTrackerAPI},
		{"500 server error", http.StatusInternalServerError, "", nil, domain.ErrTrackerTransport},
		{"502 bad gateway", http.StatusBadGateway, "", nil, domain.ErrTrackerTransport},
		{"unexpected status", http.StatusTeapot, "", nil, domain.ErrTrackerAPI},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var body []byte
			if tt.fixture != "" {
				body = loadFixture(t, tt.fixture)
			}
			headers := make(http.Header)
			for key, value := range tt.headers {
				headers.Set(key, value)
			}
			resp := &http.Response{
				StatusCode: tt.status,
				Header:     headers,
				Body:       io.NopCloser(bytes.NewReader(body)),
			}
			defer resp.Body.Close() //nolint:errcheck // NopCloser.Close always returns nil

			err := classifyHTTPError(resp, http.MethodGet, "/repos/acme/widgets/issues/1")
			assertTrackerErrorKind(t, err, tt.wantKind)
		})
	}

	t.Run("403 has no rate-limit-header branch", func(t *testing.T) {
		t.Parallel()

		// Gitea sends no X-Ratelimit-* headers (unlike GitHub); a 403 must
		// classify as an auth error even if a header that looks rate-limit-like
		// is present, proving no such branch exists in the classifier.
		headers := make(http.Header)
		headers.Set("X-Ratelimit-Remaining", "0")
		resp := &http.Response{
			StatusCode: http.StatusForbidden,
			Header:     headers,
			Body:       io.NopCloser(bytes.NewReader(loadFixture(t, "error_403.json"))),
		}
		defer resp.Body.Close() //nolint:errcheck // NopCloser.Close always returns nil

		err := classifyHTTPError(resp, http.MethodGet, "/user")
		assertTrackerErrorKind(t, err, domain.ErrTrackerAuth)
	})

	t.Run("includes the decoded gitea message for diagnostics", func(t *testing.T) {
		t.Parallel()

		// 403 is one of the branches that includes the decoded detail (401 and
		// 404 use fixed messages instead, since the gitea body cannot add
		// useful detail beyond the status itself for those two).
		resp := &http.Response{
			StatusCode: http.StatusForbidden,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader(loadFixture(t, "error_403.json"))),
		}
		defer resp.Body.Close() //nolint:errcheck // NopCloser.Close always returns nil

		err := classifyHTTPError(resp, http.MethodGet, "/user")
		var te *domain.TrackerError
		if !errors.As(err, &te) {
			t.Fatalf("error type = %T, want *domain.TrackerError", err)
		}
		if !strings.Contains(te.Message, "token does not have at least one of required scope(s)") {
			t.Errorf("TrackerError.Message = %q, want it to contain the gitea error body message", te.Message)
		}
	})
}

func TestClassifyTransportError(t *testing.T) {
	t.Parallel()

	wrapped := errors.New("dial tcp: connection refused")
	err := classifyTransportError(wrapped, http.MethodGet, "/user")

	assertTrackerErrorKind(t, err, domain.ErrTrackerTransport)
	if !errors.Is(err, wrapped) {
		t.Errorf("classifyTransportError(%v) chain missing wrapped error", wrapped)
	}
}
