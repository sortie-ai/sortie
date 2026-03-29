package github

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
)

// newTestClient creates a githubClient pointed at the given base URL with
// a test token and user-agent.
func newTestClient(t *testing.T, baseURL string) *githubClient {
	t.Helper()
	return newGitHubClient(baseURL, "test-token", "sortie/test")
}

// assertClientError asserts that err is a *domain.TrackerError with the
// expected Kind. Fatals when err is nil.
func assertClientError(t *testing.T, err error, want domain.TrackerErrorKind) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with kind %q, got nil", want)
	}
	var te *domain.TrackerError
	if !errors.As(err, &te) {
		t.Fatalf("error type = %T, want *domain.TrackerError", err)
	}
	if te.Kind != want {
		t.Errorf("TrackerError.Kind = %q, want %q", te.Kind, want)
	}
}

// --- parseLinkNext ---

func TestParseLinkNext(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		header string
		want   string
	}{
		{
			name:   "single rel next",
			header: `<https://api.github.com/repos/o/r/issues?page=2>; rel="next"`,
			want:   "https://api.github.com/repos/o/r/issues?page=2",
		},
		{
			name:   "next among multiple rels",
			header: `<https://api.github.com/repos/o/r/issues?page=3>; rel="last", <https://api.github.com/repos/o/r/issues?page=2>; rel="next"`,
			want:   "https://api.github.com/repos/o/r/issues?page=2",
		},
		{
			name:   "no next rel returns empty",
			header: `<https://api.github.com/repos/o/r/issues?page=3>; rel="last"`,
			want:   "",
		},
		{
			name:   "empty header returns empty",
			header: "",
			want:   "",
		},
		{
			name:   "malformed header no angle brackets",
			header: `https://api.github.com; rel="next"`,
			want:   "",
		},
		{
			name:   "prev and next together",
			header: `<https://example.com/page=1>; rel="prev", <https://example.com/page=3>; rel="next"`,
			want:   "https://example.com/page=3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := parseLinkNext(tt.header)
			if got != tt.want {
				t.Errorf("parseLinkNext(%q) = %q, want %q", tt.header, got, tt.want)
			}
		})
	}
}

// --- classifyHTTPError ---

func TestClassifyHTTPError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		status   int
		headers  map[string]string
		body     string
		wantKind domain.TrackerErrorKind
	}{
		{
			name:     "400 bad request",
			status:   http.StatusBadRequest,
			body:     "bad input",
			wantKind: domain.ErrTrackerPayload,
		},
		{
			name:     "401 unauthorized",
			status:   http.StatusUnauthorized,
			wantKind: domain.ErrTrackerAuth,
		},
		{
			name:     "403 rate limited primary X-Ratelimit-Remaining=0",
			status:   http.StatusForbidden,
			headers:  map[string]string{"X-Ratelimit-Remaining": "0"},
			wantKind: domain.ErrTrackerAPI,
		},
		{
			name:     "403 rate limited secondary body contains rate limit",
			status:   http.StatusForbidden,
			body:     `{"message":"You have exceeded a secondary rate limit"}`,
			wantKind: domain.ErrTrackerAPI,
		},
		{
			name:     "403 permission denied other 403",
			status:   http.StatusForbidden,
			body:     "forbidden",
			wantKind: domain.ErrTrackerAuth,
		},
		{
			name:     "404 not found",
			status:   http.StatusNotFound,
			wantKind: domain.ErrTrackerNotFound,
		},
		{
			name:     "410 gone",
			status:   http.StatusGone,
			wantKind: domain.ErrTrackerAPI,
		},
		{
			name:     "422 validation failed",
			status:   http.StatusUnprocessableEntity,
			body:     "validation error",
			wantKind: domain.ErrTrackerPayload,
		},
		{
			name:     "429 rate limited",
			status:   http.StatusTooManyRequests,
			headers:  map[string]string{"Retry-After": "60"},
			wantKind: domain.ErrTrackerAPI,
		},
		{
			name:     "500 server error",
			status:   http.StatusInternalServerError,
			body:     "oops",
			wantKind: domain.ErrTrackerTransport,
		},
		{
			name:     "502 bad gateway",
			status:   http.StatusBadGateway,
			wantKind: domain.ErrTrackerTransport,
		},
		{
			name:     "503 service unavailable",
			status:   http.StatusServiceUnavailable,
			wantKind: domain.ErrTrackerTransport,
		},
		{
			name:     "unexpected status",
			status:   418,
			body:     "I'm a teapot",
			wantKind: domain.ErrTrackerAPI,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := make(http.Header)
			for k, v := range tt.headers {
				h.Set(k, v)
			}
			resp := &http.Response{
				StatusCode: tt.status,
				Header:     h,
				Body:       io.NopCloser(bytes.NewBufferString(tt.body)),
			}
			defer resp.Body.Close() //nolint:errcheck // NopCloser.Close always returns nil

			err := classifyHTTPError(resp, "GET", "/test/path")
			assertClientError(t, err, tt.wantKind)
		})
	}
}

func TestClassifyHTTPError_429_RetryAfterInMessage(t *testing.T) {
	t.Parallel()

	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Retry-After": []string{"30"}},
		Body:       io.NopCloser(bytes.NewBufferString("")),
	}
	defer resp.Body.Close() //nolint:errcheck // NopCloser.Close always returns nil

	err := classifyHTTPError(resp, "GET", "/repos/o/r/issues")
	var te *domain.TrackerError
	if !errors.As(err, &te) {
		t.Fatalf("error type = %T, want *domain.TrackerError", err)
	}
	if !strings.Contains(te.Message, "30") {
		t.Errorf("message = %q, should contain Retry-After value", te.Message)
	}
}

// --- do ---

func TestDo_Success(t *testing.T) {
	t.Parallel()

	var gotHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`)) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	body, linkNext, err := c.do(context.Background(), "GET", "/repos/o/r/issues", nil)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if string(body) != `{"ok":true}` {
		t.Errorf("body = %q, want %q", body, `{"ok":true}`)
	}
	if linkNext != "" {
		t.Errorf("linkNext = %q, want empty", linkNext)
	}

	// Verify required GitHub API headers.
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
}

func TestDo_LinkHeaderParsed(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `<https://api.github.com/repos/o/r/issues?page=2>; rel="next", <https://api.github.com/repos/o/r/issues?page=5>; rel="last"`)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("[]")) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, linkNext, err := c.do(context.Background(), "GET", "/repos/o/r/issues", nil)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if linkNext != "https://api.github.com/repos/o/r/issues?page=2" {
		t.Errorf("linkNext = %q, want page=2 URL", linkNext)
	}
}

func TestDo_QueryParams(t *testing.T) {
	t.Parallel()

	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("[]")) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	params := url.Values{"state": {"open"}, "per_page": {"50"}}
	_, _, err := c.do(context.Background(), "GET", "/repos/o/r/issues", params)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if got := gotQuery.Get("state"); got != "open" {
		t.Errorf("state param = %q, want %q", got, "open")
	}
	if got := gotQuery.Get("per_page"); got != "50" {
		t.Errorf("per_page param = %q, want %q", got, "50")
	}
}

func TestDo_304NotModified(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, _, err := c.do(context.Background(), "GET", "/path", nil)
	if err == nil {
		t.Fatal("do 304: expected error, got nil")
	}
	var trackerErr *domain.TrackerError
	if !errors.As(err, &trackerErr) {
		t.Fatalf("error type = %T, want *domain.TrackerError", err)
	}
	if trackerErr.Kind != domain.ErrTrackerAPI {
		t.Errorf("TrackerError.Kind = %q, want %q", trackerErr.Kind, domain.ErrTrackerAPI)
	}
}

func TestDo_ContextCancellation(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	c := newTestClient(t, srv.URL)

	errCh := make(chan error, 1)
	go func() {
		_, _, err := c.do(ctx, "GET", "/repos/o/r/issues", nil)
		errCh <- err
	}()

	<-started
	cancel()

	err := <-errCh
	if err == nil {
		t.Fatal("expected error after context cancel, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

func TestDo_ErrorStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		status   int
		wantKind domain.TrackerErrorKind
	}{
		{"401", http.StatusUnauthorized, domain.ErrTrackerAuth},
		{"403 perm", http.StatusForbidden, domain.ErrTrackerAuth},
		{"404", http.StatusNotFound, domain.ErrTrackerNotFound},
		{"500", http.StatusInternalServerError, domain.ErrTrackerTransport},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			c := newTestClient(t, srv.URL)
			_, _, err := c.do(context.Background(), "GET", "/path", nil)
			assertClientError(t, err, tt.wantKind)
		})
	}
}

// --- doURL ---

func TestDoURL_AuthHeadersSent(t *testing.T) {
	t.Parallel()

	var gotHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("[]")) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, _, err := c.doURL(context.Background(), srv.URL+"/repos/o/r/issues?page=2")
	if err != nil {
		t.Fatalf("doURL: %v", err)
	}

	if got := gotHeaders.Get("Authorization"); got != "Bearer test-token" {
		t.Errorf("Authorization = %q, want Bearer prefix", got)
	}
	if got := gotHeaders.Get("X-GitHub-Api-Version"); got != "2026-03-10" {
		t.Errorf("X-GitHub-Api-Version = %q, want %q", got, "2026-03-10")
	}
}

func TestDoURL_LinkHeaderParsed(t *testing.T) {
	t.Parallel()

	nextURL := "https://api.github.com/repos/o/r/issues?page=3"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `<`+nextURL+`>; rel="next"`)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("[]")) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, got, err := c.doURL(context.Background(), srv.URL+"/page")
	if err != nil {
		t.Fatalf("doURL: %v", err)
	}
	if got != nextURL {
		t.Errorf("linkNext = %q, want %q", got, nextURL)
	}
}

// --- doJSON ---

func TestDoJSON_Success(t *testing.T) {
	t.Parallel()

	var gotMethod string
	var gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"result":"ok"}`)) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	payload := bytes.NewBufferString(`{"labels":["review"]}`)
	body, err := c.doJSON(context.Background(), "POST", "/repos/o/r/issues/1/labels", payload)
	if err != nil {
		t.Fatalf("doJSON: %v", err)
	}
	if string(body) != `{"result":"ok"}` {
		t.Errorf("body = %q", body)
	}
	if gotMethod != "POST" {
		t.Errorf("method = %q, want %q", gotMethod, "POST")
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want %q", gotContentType, "application/json")
	}
}

func TestDoJSON_2xxRange(t *testing.T) {
	t.Parallel()

	for _, status := range []int{200, 201, 204} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
			}))
			defer srv.Close()

			c := newTestClient(t, srv.URL)
			_, err := c.doJSON(context.Background(), "PATCH", "/repos/o/r/issues/1", bytes.NewBufferString("{}"))
			if err != nil {
				t.Errorf("doJSON %d: unexpected error: %v", status, err)
			}
		})
	}
}

func TestDoJSON_Error(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write([]byte(`{"errors":["invalid"]}`)) //nolint:errcheck // test helper
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, err := c.doJSON(context.Background(), "POST", "/labels", bytes.NewBufferString("{}"))
	assertClientError(t, err, domain.ErrTrackerPayload)
}

// --- doNoBody ---

func TestDoNoBody_200(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	if err := c.doNoBody(context.Background(), "DELETE", "/repos/o/r/issues/1/labels/backlog"); err != nil {
		t.Errorf("doNoBody 200: unexpected error: %v", err)
	}
}

func TestDoNoBody_204(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" {
			t.Errorf("method = %q, want DELETE", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	if err := c.doNoBody(context.Background(), "DELETE", "/repos/o/r/issues/1/labels/backlog"); err != nil {
		t.Errorf("doNoBody 204: unexpected error: %v", err)
	}
}

func TestDoNoBody_Error(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	err := c.doNoBody(context.Background(), "DELETE", "/repos/o/r/issues/1/labels/backlog")
	assertClientError(t, err, domain.ErrTrackerAuth)
}

func TestDoNoBody_ContextCancellation(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	c := newTestClient(t, srv.URL)

	errCh := make(chan error, 1)
	go func() {
		errCh <- c.doNoBody(ctx, "DELETE", "/repos/o/r/issues/1/labels/backlog")
	}()

	<-started
	cancel()

	err := <-errCh
	if err == nil {
		t.Fatal("expected error after context cancel")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

func TestDoURL_NonOKStatus(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, _, err := c.doURL(context.Background(), srv.URL+"/repos/o/r/issues?page=2")
	assertClientError(t, err, domain.ErrTrackerAuth)
}

func TestDoURL_ContextCancellation(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	c := newTestClient(t, srv.URL)

	errCh := make(chan error, 1)
	go func() {
		_, _, err := c.doURL(ctx, srv.URL+"/repos/o/r/issues?page=2")
		errCh <- err
	}()

	<-started
	cancel()

	err := <-errCh
	if err == nil {
		t.Fatal("expected error after context cancel")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

func TestDoJSON_ContextCancellation(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release // unblocked by the test to allow srv.Close() to finish cleanly
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	c := newTestClient(t, srv.URL)

	errCh := make(chan error, 1)
	go func() {
		_, err := c.doJSON(ctx, "PATCH", "/repos/o/r/issues/1", bytes.NewBufferString(`{}`))
		errCh <- err
	}()

	<-started
	cancel()

	err := <-errCh
	close(release) // let the handler return so srv.Close() does not hang

	if err == nil {
		t.Fatal("expected error after context cancel")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}
