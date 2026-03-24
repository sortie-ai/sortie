// Package server implements Sortie's embedded HTTP server for
// observability and operational control. Start with [New] to construct
// a server and [Server.ListenAndServe] to begin accepting connections.
package server

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/sortie-ai/sortie/internal/orchestrator"
)

// SnapshotFunc returns a point-in-time capture of the orchestrator's
// runtime state. The server calls this on each API request; it must
// be safe to call from any goroutine.
type SnapshotFunc func() (orchestrator.RuntimeSnapshotResult, error)

// RefreshFunc signals the orchestrator to perform an immediate
// poll+reconciliation cycle. Returns true if the signal was accepted,
// false if it was coalesced (channel already has a pending signal).
type RefreshFunc func() bool

// Params holds construction-time dependencies for [New].
type Params struct {
	// SnapshotFn returns the current runtime state. Called on each
	// GET /api/v1/state and GET /api/v1/{identifier}.
	SnapshotFn SnapshotFunc

	// RefreshFn signals an immediate poll+reconciliation cycle.
	// Called on POST /api/v1/refresh.
	RefreshFn RefreshFunc

	// Logger is the structured logger for request and error logging.
	Logger *slog.Logger

	// Addr is the TCP address to listen on (e.g. "127.0.0.1:8080").
	Addr string
}

// Server is the embedded HTTP server for JSON API, dashboard, and
// metrics endpoints. Construct via [New]. Safe for concurrent use
// after construction.
type Server struct {
	httpServer *http.Server
	mux        *http.ServeMux
	logger     *slog.Logger
	snapshotFn SnapshotFunc
	refreshFn  RefreshFunc
}

// Compile-time assertion: Server satisfies orchestrator.Observer.
var _ orchestrator.Observer = (*Server)(nil)

// New creates a [Server] with all API routes registered on an internal
// [http.ServeMux]. Does not start listening — call
// [Server.ListenAndServe].
func New(params Params) *Server {
	if params.SnapshotFn == nil {
		panic("server.New: SnapshotFn must not be nil")
	}
	if params.RefreshFn == nil {
		panic("server.New: RefreshFn must not be nil")
	}

	logger := params.Logger
	if logger == nil {
		logger = slog.Default()
	}

	mux := http.NewServeMux()

	s := &Server{
		mux:        mux,
		logger:     logger,
		snapshotFn: params.SnapshotFn,
		refreshFn:  params.RefreshFn,
	}

	// API routes. Use method-agnostic patterns with internal method
	// checking so 405 responses use JSON error envelopes instead of
	// the default plain-text body from Go's ServeMux.
	mux.HandleFunc("/api/v1/state", s.routeState)
	mux.HandleFunc("/api/v1/refresh", s.routeRefresh)
	mux.HandleFunc("/api/v1/{identifier}", s.routeIssueDetail)

	s.httpServer = &http.Server{
		Addr:              params.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	return s
}

// Mux returns the underlying [*http.ServeMux] so callers can register
// additional routes (dashboard at "/", metrics at "/metrics") without
// refactoring the server.
func (s *Server) Mux() *http.ServeMux {
	return s.mux
}

// ListenAndServe starts the HTTP listener. Blocks until the server is
// shut down or an unrecoverable error occurs. Returns
// [http.ErrServerClosed] on graceful shutdown.
func (s *Server) ListenAndServe() error {
	return s.httpServer.ListenAndServe()
}

// Serve accepts connections on the provided [net.Listener]. Use this
// instead of [Server.ListenAndServe] when the listener is pre-bound
// (e.g. to discover the actual port for ephemeral port 0).
func (s *Server) Serve(ln net.Listener) error {
	return s.httpServer.Serve(ln)
}

// Shutdown gracefully shuts down the server without interrupting
// in-flight requests. Delegates to [http.Server.Shutdown].
func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

// OnStateChange satisfies [orchestrator.Observer]. Currently a no-op;
// reserved for future SSE push or dashboard cache invalidation.
func (s *Server) OnStateChange() {}
