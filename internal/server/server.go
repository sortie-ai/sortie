// Package server implements Sortie's embedded HTTP server for
// observability and operational control. Start with [New] to construct
// a server and [Server.ListenAndServe] to begin accepting connections.
package server

import (
	"context"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

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

// SlotFunc returns the current maximum concurrent agent count.
// Called on each dashboard render to compute available slots.
// If nil, available slots defaults to 0.
type SlotFunc func() int

// RunHistoryFunc returns recent run history entries for the dashboard.
// Called on each dashboard render. If nil, the run history section
// is omitted.
type RunHistoryFunc func(ctx context.Context, limit int) ([]RunHistoryEntry, error)

// RunHistoryEntry is a server-layer view of a completed run attempt.
// Decoupled from persistence types to avoid leaking internal packages.
type RunHistoryEntry struct {
	Identifier     string
	DisplayID      string
	Attempt        int
	Status         string
	WorkflowFile   string
	StartedAt      string
	CompletedAt    string
	Error          *string
	TurnsCompleted int
}

// DBPingFunc checks whether the SQLite database is accessible.
// Returns nil on success, an error on failure. Called on each
// GET /readyz request.
type DBPingFunc func(ctx context.Context) error

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

	// Version is the build version string displayed on the dashboard.
	// Falls back to "dev" when empty.
	Version string

	// StartedAt is the time the process started, used to compute
	// uptime on the dashboard.
	StartedAt time.Time

	// SlotFunc returns the current max concurrent agents from config.
	// If nil, available slots displays as 0 on the dashboard.
	SlotFunc SlotFunc

	// MetricsRegistry is an optional Prometheus registry for the
	// /metrics endpoint. When non-nil, New registers a handler at
	// GET /metrics that serves the Prometheus text exposition format.
	// Obtain via [PromMetrics.Registry]. When nil, /metrics is
	// not registered.
	MetricsRegistry *prometheus.Registry

	// DBPingFn checks database accessibility for /readyz.
	// When nil, the database check always reports "pass".
	DBPingFn DBPingFunc

	// PreflightFn returns the result of the most recent dispatch
	// preflight validation. Called on each GET /readyz request.
	// When nil, the preflight check always reports "pass".
	PreflightFn func() bool

	// WorkflowLoadedFn returns whether the workflow file has been
	// successfully loaded at least once. Called on each GET /readyz.
	// When nil, the workflow check always reports "pass".
	WorkflowLoadedFn func() bool

	// RunHistoryFn returns recent run history entries for the
	// dashboard. Called on each dashboard render. When nil, the run
	// history section is omitted.
	RunHistoryFn RunHistoryFunc
}

// Server is the embedded HTTP server for JSON API, dashboard, and
// metrics endpoints. Construct via [New]. Safe for concurrent use
// after construction.
type Server struct {
	httpServer    *http.Server
	mux           *http.ServeMux
	logger        *slog.Logger
	snapshotFn    SnapshotFunc
	refreshFn     RefreshFunc
	dashboardTmpl *template.Template
	version       string
	startedAt     time.Time
	slotFunc      SlotFunc

	drainingFlag     atomic.Bool
	dbPingFn         DBPingFunc
	preflightFn      func() bool
	workflowLoadedFn func() bool
	runHistoryFn     RunHistoryFunc
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

	tmpl := template.Must(
		template.New("dashboard").Funcs(template.FuncMap{
			"fmtInt": fmtInt,
		}).Parse(dashboardHTML),
	)

	s := &Server{
		mux:              mux,
		logger:           logger,
		snapshotFn:       params.SnapshotFn,
		refreshFn:        params.RefreshFn,
		dashboardTmpl:    tmpl,
		version:          params.Version,
		startedAt:        params.StartedAt,
		slotFunc:         params.SlotFunc,
		dbPingFn:         params.DBPingFn,
		preflightFn:      params.PreflightFn,
		workflowLoadedFn: params.WorkflowLoadedFn,
		runHistoryFn:     params.RunHistoryFn,
	}

	// Dashboard route. Method-specific pattern so non-GET methods
	// receive the default 405 response from Go's ServeMux.
	mux.HandleFunc("GET /{$}", s.handleDashboard)
	mux.HandleFunc("GET /favicon.ico", handleFavicon)

	// Health probe routes. Method-specific patterns ensure non-GET
	// methods receive the default 405 response from Go's ServeMux.
	mux.HandleFunc("GET /livez", s.handleLivez)
	mux.HandleFunc("GET /readyz", s.handleReadyz)

	// API routes. Use method-agnostic patterns with internal method
	// checking so 405 responses use JSON error envelopes instead of
	// the default plain-text body from Go's ServeMux.
	mux.HandleFunc("/api/v1/state", s.routeState)
	mux.HandleFunc("/api/v1/refresh", s.routeRefresh)
	mux.HandleFunc("/api/v1/{identifier}", s.routeIssueDetail)

	if params.MetricsRegistry != nil {
		mux.Handle("GET /metrics", promhttp.InstrumentMetricHandler(
			params.MetricsRegistry,
			promhttp.HandlerFor(params.MetricsRegistry, promhttp.HandlerOpts{}),
		))
	}

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
// additional routes beyond those registered by [New]. When
// [Params.MetricsRegistry] is non-nil, New registers GET /metrics
// internally; Mux remains available for any further external routes.
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
// in-flight requests. Sets the draining flag as defense-in-depth
// before delegating to [http.Server.Shutdown].
func (s *Server) Shutdown(ctx context.Context) error {
	s.drainingFlag.Store(true)
	return s.httpServer.Shutdown(ctx)
}

// SetDraining marks the server as draining. After this call, /livez
// and /readyz return 503. The listener remains open so K8s probes
// receive HTTP responses rather than connection refused. Safe to call
// from any goroutine.
func (s *Server) SetDraining() {
	s.drainingFlag.Store(true)
}

// OnStateChange satisfies [orchestrator.Observer]. Currently a no-op;
// reserved for future SSE push or dashboard cache invalidation.
func (s *Server) OnStateChange() {}
