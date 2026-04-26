package jira

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/httpkit"
)

func newTestJiraClient(t *testing.T, baseURL, email, token string) *httpkit.Client {
	t.Helper()
	return newJiraClient(baseURL, email, token, "sortie/test")
}

func assertClientTrackerErrorKind(t *testing.T, err error, want domain.TrackerErrorKind) {
	t.Helper()
	var trackerErr *domain.TrackerError
	if !errors.As(err, &trackerErr) {
		t.Fatalf("error type = %T, want *domain.TrackerError", err)
	}
	if trackerErr.Kind != want {
		t.Errorf("TrackerError.Kind = %q, want %q", trackerErr.Kind, want)
	}
}

func TestClientGet_Success(t *testing.T) {
	t.Parallel()

	var gotHeaders http.Header
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		gotQuery = r.URL.Query()
		w.Header().Set("X-Test-Header", "ok")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`)) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	c := newTestJiraClient(t, srv.URL, "user@test.com", "tok123")
	body, headers, err := c.Get(context.Background(), "/test", url.Values{"jql": {"project = X"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(body) != `{"ok":true}` {
		t.Errorf("body = %q, want %q", body, `{"ok":true}`)
	}
	if got := headers.Get("X-Test-Header"); got != "ok" {
		t.Errorf("headers.Get(X-Test-Header) = %q, want %q", got, "ok")
	}
	if got := gotQuery.Get("jql"); got != "project = X" {
		t.Errorf("jql param = %q, want %q", got, "project = X")
	}
	if got := gotHeaders.Get("Authorization"); !strings.HasPrefix(got, "Basic ") {
		t.Errorf("Authorization header = %q, want Basic prefix", got)
	}
	if got := gotHeaders.Get("Accept"); got != "application/json" {
		t.Errorf("Accept header = %q, want application/json", got)
	}
	if got := gotHeaders.Get("Content-Type"); got != "" {
		t.Errorf("Content-Type header = %q, want empty", got)
	}
	if got := gotHeaders.Get("User-Agent"); got != "sortie/test" {
		t.Errorf("User-Agent header = %q, want %q", got, "sortie/test")
	}
}

func TestClientGet_ErrorMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		status   int
		headers  map[string]string
		wantKind domain.TrackerErrorKind
		wantMsg  string
	}{
		{
			name:     "400 bad request",
			status:   http.StatusBadRequest,
			wantKind: domain.ErrTrackerPayload,
		},
		{
			name:     "401 unauthorized",
			status:   http.StatusUnauthorized,
			wantKind: domain.ErrTrackerAuth,
		},
		{
			name:     "403 forbidden",
			status:   http.StatusForbidden,
			wantKind: domain.ErrTrackerAuth,
		},
		{
			name:     "403 with CAPTCHA",
			status:   http.StatusForbidden,
			headers:  map[string]string{"X-Seraph-LoginReason": "AUTHENTICATION_DENIED"},
			wantKind: domain.ErrTrackerAuth,
			wantMsg:  "CAPTCHA",
		},
		{
			name:     "404 not found",
			status:   http.StatusNotFound,
			wantKind: domain.ErrTrackerNotFound,
		},
		{
			name:     "429 rate limited",
			status:   http.StatusTooManyRequests,
			headers:  map[string]string{"Retry-After": "30"},
			wantKind: domain.ErrTrackerAPI,
			wantMsg:  "30",
		},
		{
			name:     "500 server error",
			status:   http.StatusInternalServerError,
			wantKind: domain.ErrTrackerTransport,
		},
		{
			name:     "502 bad gateway",
			status:   http.StatusBadGateway,
			wantKind: domain.ErrTrackerTransport,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tt.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tt.status)
				w.Write([]byte("error body")) //nolint:errcheck // test helper
			}))
			defer srv.Close()

			c := newTestJiraClient(t, srv.URL, "u@t.com", "t")
			_, _, err := c.Get(context.Background(), "/test", nil)
			if err == nil {
				t.Fatal("Get expected error, got nil")
			}

			assertClientTrackerErrorKind(t, err, tt.wantKind)
			var trackerErr *domain.TrackerError
			if errors.As(err, &trackerErr) && tt.wantMsg != "" && !strings.Contains(trackerErr.Message, tt.wantMsg) {
				t.Errorf("Message = %q, should contain %q", trackerErr.Message, tt.wantMsg)
			}
		})
	}
}

func TestClientGet_NetworkFailure(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // close immediately to cause connection refused

	c := newTestJiraClient(t, srv.URL, "u@t.com", "t")
	_, _, err := c.Get(context.Background(), "/test", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	assertClientTrackerErrorKind(t, err, domain.ErrTrackerTransport)
}

func TestClientGet_ContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before request

	c := newTestJiraClient(t, "https://example.invalid", "u@t.com", "t")
	_, _, err := c.Get(ctx, "/test", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	// Must NOT be wrapped in TrackerError
	var te *domain.TrackerError
	if errors.As(err, &te) {
		t.Errorf("context cancellation should not be wrapped in TrackerError, got Kind=%q", te.Kind)
	}
}

func TestClientSend_Success(t *testing.T) {
	t.Parallel()

	var gotMethod string
	var gotBody []byte
	var gotHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotHeaders = r.Header
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newTestJiraClient(t, srv.URL, "user@test.com", "tok123")
	body, err := c.Send(context.Background(), "POST", "/transitions", strings.NewReader(`{"transition":{"id":"31"}}`))
	if err != nil {
		t.Fatalf("Send() unexpected error: %v", err)
	}
	if len(body) != 0 {
		t.Errorf("Send() body = %q, want empty", body)
	}
	if gotMethod != "POST" {
		t.Errorf("request method = %q, want POST", gotMethod)
	}
	if got := string(gotBody); got != `{"transition":{"id":"31"}}` {
		t.Errorf("request body = %q, want %q", got, `{"transition":{"id":"31"}}`)
	}
	if got := gotHeaders.Get("Authorization"); !strings.HasPrefix(got, "Basic ") {
		t.Errorf("Authorization header = %q, want Basic prefix", got)
	}
	if got := gotHeaders.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type header = %q, want application/json", got)
	}
	if got := gotHeaders.Get("Accept"); got != "application/json" {
		t.Errorf("Accept header = %q, want application/json", got)
	}
	if got := gotHeaders.Get("User-Agent"); got != "sortie/test" {
		t.Errorf("User-Agent header = %q, want %q", got, "sortie/test")
	}
}

func TestClientSend_200_WithBody(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`)) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	c := newTestJiraClient(t, srv.URL, "u@t.com", "t")
	body, err := c.Send(context.Background(), "POST", "/test", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("Send() unexpected error: %v", err)
	}
	if string(body) != `{"ok":true}` {
		t.Errorf("Send() body = %q, want %q", body, `{"ok":true}`)
	}
}

func TestClientSend_ErrorMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		status   int
		wantKind domain.TrackerErrorKind
	}{
		{"400 bad request", http.StatusBadRequest, domain.ErrTrackerPayload},
		{"401 unauthorized", http.StatusUnauthorized, domain.ErrTrackerAuth},
		{"403 forbidden", http.StatusForbidden, domain.ErrTrackerAuth},
		{"404 not found", http.StatusNotFound, domain.ErrTrackerNotFound},
		{"429 rate limited", http.StatusTooManyRequests, domain.ErrTrackerAPI},
		{"500 server error", http.StatusInternalServerError, domain.ErrTrackerTransport},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				w.Write([]byte("error body")) //nolint:errcheck // test helper
			}))
			defer srv.Close()

			c := newTestJiraClient(t, srv.URL, "u@t.com", "t")
			_, err := c.Send(context.Background(), "POST", "/test", strings.NewReader("{}"))
			if err == nil {
				t.Fatal("Send() expected error, got nil")
			}
			assertClientTrackerErrorKind(t, err, tt.wantKind)
		})
	}
}

func TestClientSend_ContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := newTestJiraClient(t, "https://example.invalid", "u@t.com", "t")
	_, err := c.Send(ctx, "POST", "/test", strings.NewReader("{}"))
	if err == nil {
		t.Fatal("Send() expected error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Send() error = %v, want context.Canceled", err)
	}
	var te *domain.TrackerError
	if errors.As(err, &te) {
		t.Errorf("context cancellation should not be wrapped in TrackerError, got Kind=%q", te.Kind)
	}
}
