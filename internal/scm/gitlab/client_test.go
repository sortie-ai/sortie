package gitlab

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

func TestNewGitLabClient(t *testing.T) {
	t.Parallel()

	t.Run("sends PRIVATE-TOKEN, accept, and user-agent, never a query parameter", func(t *testing.T) {
		t.Parallel()

		var gotHeaders http.Header
		var gotPath string
		var gotRawQuery string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotHeaders = r.Header.Clone()
			gotPath = r.URL.Path
			gotRawQuery = r.URL.RawQuery
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{}`)) //nolint:errcheck // test helper
		}))
		defer srv.Close()

		client := newGitLabClient(srv.URL+"/api/v4", "secret-token", "sortie/test")
		_, _, err := client.Get(context.Background(), "/projects/1", nil)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}

		if got := gotHeaders.Get("PRIVATE-TOKEN"); got != "secret-token" {
			t.Errorf("PRIVATE-TOKEN = %q, want %q", got, "secret-token")
		}
		if got := gotHeaders.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q, want %q", got, "application/json")
		}
		if got := gotHeaders.Get("User-Agent"); got != "sortie/test" {
			t.Errorf("User-Agent = %q, want %q", got, "sortie/test")
		}
		if gotPath != "/api/v4/projects/1" {
			t.Errorf("path = %q, want %q", gotPath, "/api/v4/projects/1")
		}
		if gotRawQuery != "" {
			t.Errorf("raw query = %q, want empty (the token must never travel as a query parameter)", gotRawQuery)
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

		client := newGitLabClient(srv.URL+"/api/v4/", "secret-token", "sortie/test")
		_, _, err := client.Get(context.Background(), "/projects/1", nil)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if gotPath != "/api/v4/projects/1" {
			t.Errorf("path = %q, want %q (no doubled slash)", gotPath, "/api/v4/projects/1")
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
		{"400 bad request, non-string message", http.StatusBadRequest, "error_400.json", nil, domain.ErrTrackerPayload},
		{"401 unauthorized, string message", http.StatusUnauthorized, "error_401.json", nil, domain.ErrTrackerAuth},
		{"403 forbidden, error plus error_description", http.StatusForbidden, "error_403.json", nil, domain.ErrTrackerAuth},
		{"404 project not found", http.StatusNotFound, "error_404_project.json", nil, domain.ErrTrackerNotFound},
		{"404 issue not found", http.StatusNotFound, "error_404_issue.json", nil, domain.ErrTrackerNotFound},
		{"409 conflict", http.StatusConflict, "", nil, domain.ErrTrackerAPI},
		{"414 request uri too long", http.StatusRequestURITooLong, "", nil, domain.ErrTrackerPayload},
		{"422 unprocessable entity", http.StatusUnprocessableEntity, "", nil, domain.ErrTrackerPayload},
		{"429 too many requests with Retry-After", http.StatusTooManyRequests, "", map[string]string{"Retry-After": "30"}, domain.ErrTrackerAPI},
		{"500 server error, bare error shape", http.StatusInternalServerError, "error_500.json", nil, domain.ErrTrackerTransport},
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

			err := classifyHTTPError(resp, http.MethodGet, "/projects/1/issues/1")
			assertTrackerErrorKind(t, err, tt.wantKind)
		})
	}

	t.Run("429 body is non-JSON and the message still carries the raw snippet", func(t *testing.T) {
		t.Parallel()

		resp := &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{"Retry-After": {"30"}},
			Body:       io.NopCloser(bytes.NewReader([]byte("Retry later"))),
		}
		defer resp.Body.Close() //nolint:errcheck // NopCloser.Close always returns nil

		err := classifyHTTPError(resp, http.MethodGet, "/projects/1/issues")
		assertTrackerErrorKind(t, err, domain.ErrTrackerAPI)

		var te *domain.TrackerError
		if !errors.As(err, &te) {
			t.Fatalf("error type = %T, want *domain.TrackerError", err)
		}
		if !bytes.Contains([]byte(te.Message), []byte("Retry later")) {
			t.Errorf("TrackerError.Message = %q, want it to contain the raw non-JSON snippet %q", te.Message, "Retry later")
		}
	})

	t.Run("includes the decoded detail for diagnostics", func(t *testing.T) {
		t.Parallel()

		resp := &http.Response{
			StatusCode: http.StatusForbidden,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader(loadFixture(t, "error_403.json"))),
		}
		defer resp.Body.Close() //nolint:errcheck // NopCloser.Close always returns nil

		err := classifyHTTPError(resp, http.MethodGet, "/projects/1")
		var te *domain.TrackerError
		if !errors.As(err, &te) {
			t.Fatalf("error type = %T, want *domain.TrackerError", err)
		}
		if !bytes.Contains([]byte(te.Message), []byte("insufficient_scope")) {
			t.Errorf("TrackerError.Message = %q, want it to contain the decoded error body", te.Message)
		}
		if !bytes.Contains([]byte(te.Message), []byte("higher privileges")) {
			t.Errorf("TrackerError.Message = %q, want it to contain the error_description", te.Message)
		}
	})

	t.Run("never includes the request token", func(t *testing.T) {
		t.Parallel()

		resp := &http.Response{
			StatusCode: http.StatusUnauthorized,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader(loadFixture(t, "error_401.json"))),
		}
		defer resp.Body.Close() //nolint:errcheck // NopCloser.Close always returns nil

		err := classifyHTTPError(resp, http.MethodGet, "/projects/1")
		if bytes.Contains([]byte(err.Error()), []byte("secret-token")) {
			t.Errorf("classifyHTTPError error message unexpectedly contains a token-shaped value: %v", err)
		}
	})
}

func TestErrorDetail(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		snippet string
		want    string
	}{
		{"string message", `{"message":"invalid state"}`, "invalid state"},
		{"non-string message compacts to JSON", `{"message":["email is invalid","name is invalid"]}`, `["email is invalid","name is invalid"]`},
		{"bare error", `{"error":"boom"}`, "boom"},
		{"error with error_description", `{"error":"invalid_token","error_description":"token expired"}`, "invalid_token: token expired"},
		{"non-JSON snippet falls back to the raw bytes", "not json at all", "not json at all"},
		{"empty snippet", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := errorDetail([]byte(tt.snippet))
			if got != tt.want {
				t.Errorf("errorDetail(%q) = %q, want %q", tt.snippet, got, tt.want)
			}
		})
	}
}
