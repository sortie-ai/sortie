package server

import (
	"context"
	"log/slog"
	"net/http"
	"testing"
	"time"

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

	// Start the server in a goroutine.
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()

	// Give server time to start.
	time.Sleep(50 * time.Millisecond)

	// Gracefully shut down.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// ListenAndServe should return ErrServerClosed.
	err := <-errCh
	if err != http.ErrServerClosed {
		t.Errorf("ListenAndServe error = %v, want %v", err, http.ErrServerClosed)
	}
}
