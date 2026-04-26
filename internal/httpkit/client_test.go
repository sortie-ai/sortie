package httpkit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func mustClient(t *testing.T, opts ClientOptions) *Client {
	t.Helper()
	return NewClient(opts)
}

func TestClient_Get_success(t *testing.T) {
	t.Parallel()

	var authCalled atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Auth") == "token" {
			authCalled.Store(true)
		}
		w.Header().Set("X-Response-ID", "42")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "hello")
	}))
	defer srv.Close()

	c := mustClient(t, ClientOptions{
		BaseURL: srv.URL,
		Authorize: func(req *http.Request) {
			req.Header.Set("X-Auth", "token")
		},
	})

	body, headers, err := c.Get(context.Background(), "/test", nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(body) != "hello" {
		t.Errorf("Get body = %q, want %q", body, "hello")
	}
	if !authCalled.Load() {
		t.Error("Get: authorizer was not called")
	}
	if got := headers.Get("X-Response-ID"); got != "42" {
		t.Errorf("Get X-Response-ID = %q, want %q", got, "42")
	}
}

func TestClient_Get_nonSuccess(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("classified-404")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := mustClient(t, ClientOptions{
		BaseURL: srv.URL,
		ClassifyError: func(resp *http.Response, method, path string) error {
			return fmt.Errorf("%w: %s %s %d", sentinel, method, path, resp.StatusCode)
		},
	})

	_, _, err := c.Get(context.Background(), "/missing", nil)
	if !errors.Is(err, sentinel) {
		t.Errorf("Get non-success error = %v, want to wrap %v", err, sentinel)
	}
}

func TestClient_GetConditional_200(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "payload")
	}))
	defer srv.Close()

	c := mustClient(t, ClientOptions{BaseURL: srv.URL})

	body, etag, notModified, err := c.GetConditional(context.Background(), "/item", `"v1"`, nil)
	if err != nil {
		t.Fatalf("GetConditional 200: %v", err)
	}
	if notModified {
		t.Error("GetConditional 200 notModified = true, want false")
	}
	if string(body) != "payload" {
		t.Errorf("GetConditional 200 body = %q, want %q", body, "payload")
	}
	if etag != `"v1"` {
		t.Errorf("GetConditional 200 etag = %q, want %q", etag, `"v1"`)
	}
}

func TestClient_GetConditional_304(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()

	c := mustClient(t, ClientOptions{BaseURL: srv.URL})

	body, etag, notModified, err := c.GetConditional(context.Background(), "/item", `"v1"`, nil)
	if err != nil {
		t.Fatalf("GetConditional 304: %v", err)
	}
	if !notModified {
		t.Error("GetConditional 304 notModified = false, want true")
	}
	if len(body) != 0 {
		t.Errorf("GetConditional 304 body = %q, want empty", body)
	}
	if etag != "" {
		t.Errorf("GetConditional 304 etag = %q, want empty", etag)
	}
}

func TestClient_GetConditional_weakETag(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `W/"abc123"`)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "body")
	}))
	defer srv.Close()

	c := mustClient(t, ClientOptions{BaseURL: srv.URL})

	_, etag, _, err := c.GetConditional(context.Background(), "/item", "", nil)
	if err != nil {
		t.Fatalf("GetConditional weakETag: %v", err)
	}
	if etag != `W/"abc123"` {
		t.Errorf("GetConditional weakETag etag = %q, want %q", etag, `W/"abc123"`)
	}
}

func TestClient_GetConditional_emptyIfNoneMatch(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	c := mustClient(t, ClientOptions{BaseURL: srv.URL})

	_, _, notModified, err := c.GetConditional(context.Background(), "/item", "", nil)
	if err != nil {
		t.Fatalf("GetConditional emptyIfNoneMatch: %v", err)
	}
	if notModified {
		t.Error("GetConditional emptyIfNoneMatch notModified = true, want false")
	}
}

func TestClient_GetRaw_truncation(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "hello world")
	}))
	defer srv.Close()

	c := mustClient(t, ClientOptions{BaseURL: srv.URL})

	body, err := c.GetRaw(context.Background(), "/file", 5)
	if err != nil {
		t.Fatalf("GetRaw: %v", err)
	}
	if string(body) != "hello" {
		t.Errorf("GetRaw body = %q, want %q", body, "hello")
	}
}

func TestClient_GetRaw_non200(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("classified-500")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := mustClient(t, ClientOptions{
		BaseURL: srv.URL,
		ClassifyError: func(resp *http.Response, method, path string) error {
			return fmt.Errorf("%w: %d", sentinel, resp.StatusCode)
		},
	})

	_, err := c.GetRaw(context.Background(), "/file", 1024)
	if !errors.Is(err, sentinel) {
		t.Errorf("GetRaw non-200 error = %v, want to wrap %v", err, sentinel)
	}
}

func TestClient_Send_2xx(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
	}{
		{"201 Created", http.StatusCreated},
		{"204 No Content", http.StatusNoContent},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Content-Type") != "application/json" {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			c := mustClient(t, ClientOptions{BaseURL: srv.URL})

			_, err := c.Send(context.Background(), http.MethodPost, "/items", strings.NewReader("{}"))
			if err != nil {
				t.Errorf("Send(%d): %v", tt.status, err)
			}
		})
	}
}

func TestClient_SendNoBody_success(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
	}{
		{"200 OK", http.StatusOK},
		{"204 No Content", http.StatusNoContent},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			c := mustClient(t, ClientOptions{BaseURL: srv.URL})

			if err := c.SendNoBody(context.Background(), http.MethodDelete, "/items/1"); err != nil {
				t.Errorf("SendNoBody(%d): %v", tt.status, err)
			}
		})
	}
}

func TestClient_SendNoBody_doesNotSetJSONContentType(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(w, "unexpected Content-Type: %q", ct)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := mustClient(t, ClientOptions{BaseURL: srv.URL})

	if err := c.SendNoBody(context.Background(), http.MethodDelete, "/items/1"); err != nil {
		t.Fatalf("SendNoBody set Content-Type unexpectedly: %v", err)
	}
}

func TestClient_Get_doesNotSetJSONContentType(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	c := mustClient(t, ClientOptions{BaseURL: srv.URL})

	_, _, err := c.Get(context.Background(), "/items", nil)
	if err != nil {
		t.Fatalf("Get set Content-Type unexpectedly: %v", err)
	}
}

func TestClient_TransportError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("transport-classified")

	c := mustClient(t, ClientOptions{
		BaseURL: "://bad",
		ClassifyTransport: func(err error, method, path string) error {
			return fmt.Errorf("%w: %s %s", sentinel, method, path)
		},
	})

	_, _, err := c.Get(context.Background(), "/test", nil)
	if !errors.Is(err, sentinel) {
		t.Errorf("Get transport error = %v, want to wrap %v", err, sentinel)
	}
}

func TestClient_Cancellation(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	bypass := errors.New("classifyTransport bypassed")
	c := mustClient(t, ClientOptions{
		BaseURL: srv.URL,
		ClassifyTransport: func(err error, method, path string) error {
			return bypass
		},
	})

	_, _, err := c.Get(ctx, "/test", nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Get cancelled error = %v, want %v", err, context.Canceled)
	}
}
