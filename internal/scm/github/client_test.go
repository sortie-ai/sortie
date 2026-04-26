package github

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/httpkit"
)

func newTestClient(t *testing.T, baseURL string) *httpkit.Client {
	t.Helper()
	return newGitHubClient(baseURL, "test-token", "sortie/test")
}

func assertClientError(t *testing.T, err error, want domain.TrackerErrorKind) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with kind %q, got nil", want)
	}
	var trackerErr *domain.TrackerError
	if !errors.As(err, &trackerErr) {
		t.Fatalf("error type = %T, want *domain.TrackerError", err)
	}
	if trackerErr.Kind != want {
		t.Errorf("TrackerError.Kind = %q, want %q", trackerErr.Kind, want)
	}
}

func TestClassifyHTTPError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		status   int
		headers  map[string]string
		body     string
		wantKind domain.TrackerErrorKind
	}{
		{"400 bad request", http.StatusBadRequest, nil, "bad input", domain.ErrTrackerPayload},
		{"401 unauthorized", http.StatusUnauthorized, nil, "", domain.ErrTrackerAuth},
		{"403 rate limited primary", http.StatusForbidden, map[string]string{"X-Ratelimit-Remaining": "0"}, "", domain.ErrTrackerAPI},
		{"403 rate limited secondary", http.StatusForbidden, nil, `{"message":"You have exceeded a secondary rate limit"}`, domain.ErrTrackerAPI},
		{"403 permission denied", http.StatusForbidden, nil, "forbidden", domain.ErrTrackerAuth},
		{"404 not found", http.StatusNotFound, nil, "", domain.ErrTrackerNotFound},
		{"410 gone", http.StatusGone, nil, "", domain.ErrTrackerAPI},
		{"422 validation failed", http.StatusUnprocessableEntity, nil, "validation error", domain.ErrTrackerPayload},
		{"429 rate limited", http.StatusTooManyRequests, map[string]string{"Retry-After": "60"}, "", domain.ErrTrackerAPI},
		{"500 server error", http.StatusInternalServerError, nil, "oops", domain.ErrTrackerTransport},
		{"502 bad gateway", http.StatusBadGateway, nil, "", domain.ErrTrackerTransport},
		{"503 service unavailable", http.StatusServiceUnavailable, nil, "", domain.ErrTrackerTransport},
		{"unexpected status", 418, nil, "I'm a teapot", domain.ErrTrackerAPI},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			headers := make(http.Header)
			for key, value := range tt.headers {
				headers.Set(key, value)
			}
			resp := &http.Response{
				StatusCode: tt.status,
				Header:     headers,
				Body:       io.NopCloser(bytes.NewBufferString(tt.body)),
			}
			defer resp.Body.Close() //nolint:errcheck // NopCloser.Close always returns nil

			err := classifyHTTPError(resp, http.MethodGet, "/test/path")
			assertClientError(t, err, tt.wantKind)
		})
	}
}

func TestClientGet_Success(t *testing.T) {
	t.Parallel()

	var gotHeaders http.Header
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		gotQuery = r.URL.Query()
		w.Header().Set("X-Result", "ok")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`)) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	body, headers, err := client.Get(context.Background(), "/repos/o/r/issues", url.Values{"state": {"open"}})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(body) != `{"ok":true}` {
		t.Errorf("body = %q, want %q", body, `{"ok":true}`)
	}
	if got := headers.Get("X-Result"); got != "ok" {
		t.Errorf("headers.Get(X-Result) = %q, want %q", got, "ok")
	}
	if got := gotQuery.Get("state"); got != "open" {
		t.Errorf("state param = %q, want %q", got, "open")
	}
	if got := gotHeaders.Get("Authorization"); got != "Bearer test-token" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer test-token")
	}
	if got := gotHeaders.Get("Accept"); got != "application/vnd.github+json" {
		t.Errorf("Accept = %q, want %q", got, "application/vnd.github+json")
	}
	if got := gotHeaders.Get("X-GitHub-Api-Version"); got != "2026-03-10" {
		t.Errorf("X-GitHub-Api-Version = %q, want %q", got, "2026-03-10")
	}
	if got := gotHeaders.Get("User-Agent"); got != "sortie/test" {
		t.Errorf("User-Agent = %q, want %q", got, "sortie/test")
	}
	if got := gotHeaders.Get("Content-Type"); got != "" {
		t.Errorf("Content-Type = %q, want empty", got)
	}
}

func TestClientGet_ErrorStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		status   int
		wantKind domain.TrackerErrorKind
	}{
		{"401 unauthorized", http.StatusUnauthorized, domain.ErrTrackerAuth},
		{"403 forbidden", http.StatusForbidden, domain.ErrTrackerAuth},
		{"404 not found", http.StatusNotFound, domain.ErrTrackerNotFound},
		{"500 server error", http.StatusInternalServerError, domain.ErrTrackerTransport},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			client := newTestClient(t, srv.URL)
			_, _, err := client.Get(context.Background(), "/path", nil)
			assertClientError(t, err, tt.wantKind)
		})
	}
}

func TestClientGet_NetworkFailure(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	client := newTestClient(t, srv.URL)
	_, _, err := client.Get(context.Background(), "/repos/o/r/issues", nil)
	assertClientError(t, err, domain.ErrTrackerTransport)
}

func TestClientGet_ContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	client := newTestClient(t, "https://example.invalid")
	_, _, err := client.Get(ctx, "/repos/o/r/issues", nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

func TestClientGetURL_AuthHeadersSent(t *testing.T) {
	t.Parallel()

	var gotHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[]`)) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, _, err := client.GetURL(context.Background(), srv.URL+"/repos/o/r/issues?page=2")
	if err != nil {
		t.Fatalf("GetURL: %v", err)
	}
	if got := gotHeaders.Get("Authorization"); got != "Bearer test-token" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer test-token")
	}
	if got := gotHeaders.Get("X-GitHub-Api-Version"); got != "2026-03-10" {
		t.Errorf("X-GitHub-Api-Version = %q, want %q", got, "2026-03-10")
	}
	if got := gotHeaders.Get("Content-Type"); got != "" {
		t.Errorf("Content-Type = %q, want empty", got)
	}
}

func TestClientSend_Success(t *testing.T) {
	t.Parallel()

	var gotMethod string
	var gotHeaders http.Header
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotHeaders = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"result":"ok"}`)) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	body, err := client.Send(context.Background(), http.MethodPost, "/repos/o/r/issues/1/labels", bytes.NewBufferString(`{"labels":["review"]}`))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if string(body) != `{"result":"ok"}` {
		t.Errorf("body = %q, want %q", body, `{"result":"ok"}`)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want %q", gotMethod, http.MethodPost)
	}
	if string(gotBody) != `{"labels":["review"]}` {
		t.Errorf("request body = %q, want %q", gotBody, `{"labels":["review"]}`)
	}
	if got := gotHeaders.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want %q", got, "application/json")
	}
	if got := gotHeaders.Get("Authorization"); got != "Bearer test-token" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer test-token")
	}
}

func TestClientSend_Error(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write([]byte(`{"errors":["invalid"]}`)) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, err := client.Send(context.Background(), http.MethodPost, "/labels", bytes.NewBufferString("{}"))
	assertClientError(t, err, domain.ErrTrackerPayload)
}

func TestClientSendNoBody_Success(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusOK, http.StatusNoContent} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()

			var gotContentType string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotContentType = r.Header.Get("Content-Type")
				w.WriteHeader(status)
			}))
			defer srv.Close()

			client := newTestClient(t, srv.URL)
			if err := client.SendNoBody(context.Background(), http.MethodDelete, "/repos/o/r/issues/1/labels/backlog"); err != nil {
				t.Fatalf("SendNoBody(%d): %v", status, err)
			}
			if gotContentType != "" {
				t.Errorf("Content-Type = %q, want empty", gotContentType)
			}
		})
	}
}

func TestClientSendNoBody_Error(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	err := client.SendNoBody(context.Background(), http.MethodDelete, "/repos/o/r/issues/1/labels/backlog")
	assertClientError(t, err, domain.ErrTrackerAuth)
}

func TestClientGetConditional_200_ReturnsBodyAndETag(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"abc123"`)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"number":1}`)) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	body, etag, notModified, err := client.GetConditional(context.Background(), "/repos/o/r/issues/1", "", nil)
	if err != nil {
		t.Fatalf("GetConditional: %v", err)
	}
	if notModified {
		t.Error("notModified = true, want false")
	}
	if string(body) != `{"number":1}` {
		t.Errorf("body = %q, want %q", body, `{"number":1}`)
	}
	if etag != `"abc123"` {
		t.Errorf("etag = %q, want %q", etag, `"abc123"`)
	}
}

func TestClientGetConditional_304_NotModified(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	body, etag, notModified, err := client.GetConditional(context.Background(), "/repos/o/r/issues/3", `"cached"`, nil)
	if err != nil {
		t.Fatalf("GetConditional: %v", err)
	}
	if !notModified {
		t.Error("notModified = false, want true")
	}
	if body != nil {
		t.Errorf("body = %v, want nil on 304", body)
	}
	if etag != "" {
		t.Errorf("etag = %q, want empty on 304", etag)
	}
}

func TestClientGetConditional_SetsIfNoneMatchHeader(t *testing.T) {
	t.Parallel()

	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("If-None-Match")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`)) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, _, _, err := client.GetConditional(context.Background(), "/repos/o/r/issues/4", `W/"etag-1"`, nil)
	if err != nil {
		t.Fatalf("GetConditional: %v", err)
	}
	if gotHeader != `W/"etag-1"` {
		t.Errorf("If-None-Match = %q, want %q", gotHeader, `W/"etag-1"`)
	}
}

func TestClientGetConditional_OmitsIfNoneMatchWhenEmpty(t *testing.T) {
	t.Parallel()

	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("If-None-Match")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`)) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, _, _, err := client.GetConditional(context.Background(), "/repos/o/r/issues/5", "", nil)
	if err != nil {
		t.Fatalf("GetConditional: %v", err)
	}
	if gotHeader != "" {
		t.Errorf("If-None-Match = %q, want empty", gotHeader)
	}
}

func TestClientGetConditional_WeakETagPreserved(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `W/"weak-1"`)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"number":5}`)) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, etag, _, err := client.GetConditional(context.Background(), "/repos/o/r/issues/5", "", nil)
	if err != nil {
		t.Fatalf("GetConditional: %v", err)
	}
	if etag != `W/"weak-1"` {
		t.Errorf("etag = %q, want %q", etag, `W/"weak-1"`)
	}
}

func TestClientGetConditional_Error(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	_, _, _, err := client.GetConditional(context.Background(), "/repos/o/r/issues/6", "", nil)
	assertClientError(t, err, domain.ErrTrackerTransport)
}

func TestClientGetRaw_Success(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("hello world")) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	body, err := client.GetRaw(context.Background(), "/repos/o/r/actions/jobs/1/logs", 5)
	if err != nil {
		t.Fatalf("GetRaw: %v", err)
	}
	if string(body) != "hello" {
		t.Errorf("body = %q, want %q", body, "hello")
	}
}
