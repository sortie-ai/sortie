package server

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/sortie-ai/sortie/internal/orchestrator"
)

// Compile-time assertion: Server satisfies orchestrator.Observer.
var _ orchestrator.Observer = (*Server)(nil)

func TestNew(t *testing.T) {
	t.Parallel()

	t.Run("returns non-nil server with mux", func(t *testing.T) {
		t.Parallel()

		srv := New(Params{
			SnapshotFn: fixedSnapshot(orchestrator.RuntimeSnapshotResult{}),
			RefreshFn:  acceptingRefresh(),
			Logger:     slog.New(slog.DiscardHandler),
			Addr:       "127.0.0.1:0",
		})
		if srv == nil {
			t.Fatal("New() returned nil")
		}
		if srv.Mux() == nil {
			t.Fatal("Mux() returned nil")
		}
	})

	t.Run("nil logger defaults to slog.Default", func(t *testing.T) {
		t.Parallel()

		srv := New(Params{
			SnapshotFn: fixedSnapshot(orchestrator.RuntimeSnapshotResult{}),
			RefreshFn:  acceptingRefresh(),
		})
		if srv.logger == nil {
			t.Fatal("logger is nil after New with nil Logger param")
		}
	})
}

func TestOnStateChange(t *testing.T) {
	t.Parallel()

	srv := New(Params{
		SnapshotFn: fixedSnapshot(orchestrator.RuntimeSnapshotResult{}),
		RefreshFn:  acceptingRefresh(),
		Logger:     slog.New(slog.DiscardHandler),
	})

	// OnStateChange is a no-op; just verify it does not panic.
	srv.OnStateChange()
}

func TestServerLifecycle(t *testing.T) {
	t.Parallel()

	srv := New(Params{
		SnapshotFn: fixedSnapshot(orchestrator.RuntimeSnapshotResult{
			GeneratedAt: time.Now().UTC(),
		}),
		RefreshFn: acceptingRefresh(),
		Logger:    slog.New(slog.DiscardHandler),
		Addr:      "127.0.0.1:0",
	})

	// Bind a listener first so we know the server is ready
	// immediately after Serve starts — no sleep needed.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}

	// Start serving on the pre-bound listener.
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.httpServer.Serve(ln)
	}()

	// Gracefully shut down.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// Serve should return ErrServerClosed.
	serveErr := <-errCh
	if serveErr != http.ErrServerClosed {
		t.Errorf("Serve error = %v, want %v", serveErr, http.ErrServerClosed)
	}
}

func TestNewPanicsOnNilSnapshotFn(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("New did not panic with nil SnapshotFn")
		}
	}()

	New(Params{
		RefreshFn: acceptingRefresh(),
		Logger:    slog.New(slog.DiscardHandler),
	})
}

func TestNewPanicsOnNilRefreshFn(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("New did not panic with nil RefreshFn")
		}
	}()

	New(Params{
		SnapshotFn: fixedSnapshot(orchestrator.RuntimeSnapshotResult{}),
		Logger:     slog.New(slog.DiscardHandler),
	})
}

func TestServeMethod(t *testing.T) {
	t.Parallel()

	srv := New(Params{
		SnapshotFn: fixedSnapshot(orchestrator.RuntimeSnapshotResult{
			GeneratedAt: time.Now().UTC(),
		}),
		RefreshFn: acceptingRefresh(),
		Logger:    slog.New(slog.DiscardHandler),
		Addr:      "127.0.0.1:0",
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ln)
	}()

	// Verify the server is actually accepting requests.
	resp, err := http.Get("http://" + ln.Addr().String() + "/api/v1/state")
	if err != nil {
		t.Fatalf("GET /api/v1/state: %v", err)
	}
	resp.Body.Close() //nolint:errcheck // test code
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	serveErr := <-errCh
	if serveErr != http.ErrServerClosed {
		t.Errorf("Serve error = %v, want %v", serveErr, http.ErrServerClosed)
	}
}

// --- Helpers for /metrics tests ---

func testServerWithRegistry(t *testing.T, reg *prometheus.Registry) *httptest.Server {
	t.Helper()
	srv := New(Params{
		SnapshotFn:      fixedSnapshot(orchestrator.RuntimeSnapshotResult{}),
		RefreshFn:       acceptingRefresh(),
		Logger:          slog.New(slog.DiscardHandler),
		MetricsRegistry: reg,
	})
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return ts
}

// --- /metrics endpoint tests ---

func TestMetricsEndpoint(t *testing.T) {
	t.Parallel()

	pm := NewPromMetrics("1.0.0-test", "go1.26.1")
	ts := testServerWithRegistry(t, pm.Registry())

	// First request primes the promhttp self-instrumentation counters
	// (they are incremented after the response is written).
	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics (prime): %v", err)
	}
	resp.Body.Close() //nolint:errcheck // test code

	// Second request captures the self-instrumentation in the output.
	resp, err = http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // test code

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want it to contain %q", ct, "text/plain")
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	bodyStr := string(body)

	if !strings.Contains(bodyStr, "sortie_build_info") {
		t.Error("response body missing sortie_build_info metric")
	}
	if !strings.Contains(bodyStr, "promhttp_metric_handler_requests_total") {
		t.Error("response body missing promhttp_metric_handler_requests_total (self-instrumentation not on dedicated registry)")
	}
}

func TestMetricsEndpointDisabled(t *testing.T) {
	t.Parallel()

	ts := testServerWithRegistry(t, nil)

	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // test code

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestMetricsEndpointMethodNotAllowed(t *testing.T) {
	t.Parallel()

	pm := NewPromMetrics("1.0.0-test", "go1.26.1")
	ts := testServerWithRegistry(t, pm.Registry())

	resp, err := http.Post(ts.URL+"/metrics", "text/plain", nil)
	if err != nil {
		t.Fatalf("POST /metrics: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // test code

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
	}
}

// --- Draining + Shutdown tests ---

func TestSetDrainingThenShutdown(t *testing.T) {
	t.Parallel()

	srv := New(Params{
		SnapshotFn: fixedSnapshot(orchestrator.RuntimeSnapshotResult{
			GeneratedAt: time.Now().UTC(),
		}),
		RefreshFn: acceptingRefresh(),
		Logger:    slog.New(slog.DiscardHandler),
		Addr:      "127.0.0.1:0",
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ln)
	}()

	// Verify /livez is pass before draining.
	resp, err := http.Get("http://" + ln.Addr().String() + "/livez")
	if err != nil {
		t.Fatalf("GET /livez (pre-drain): %v", err)
	}
	resp.Body.Close() //nolint:errcheck // test code
	if resp.StatusCode != http.StatusOK {
		t.Errorf("pre-drain livez status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	// Set draining.
	srv.SetDraining()

	// /livez should now return 503.
	resp, err = http.Get("http://" + ln.Addr().String() + "/livez")
	if err != nil {
		t.Fatalf("GET /livez (post-drain): %v", err)
	}
	resp.Body.Close() //nolint:errcheck // test code
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("post-drain livez status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}

	// Shutdown should succeed even after draining.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	serveErr := <-errCh
	if serveErr != http.ErrServerClosed {
		t.Errorf("Serve error = %v, want %v", serveErr, http.ErrServerClosed)
	}
}
