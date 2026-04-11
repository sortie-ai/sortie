// Package main is the entry point for the Sortie orchestration service.
// The binary accepts an optional positional workflow file path (default
// ./WORKFLOW.md), a --log-level flag to control log verbosity,
// a --log-format flag to select text or JSON output encoding, --port
// and --host flags for the HTTP observability server, and a "validate"
// subcommand for offline workflow file validation. Short aliases -h
// (help) and -V (version) are supported alongside their long forms.
// The HTTP server starts by default on port 7678; --port 0 disables it.
// Start with [run] for the complete startup and shutdown lifecycle.
package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/orchestrator"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/server"
	"github.com/sortie-ai/sortie/internal/tool/trackerapi"
	"github.com/sortie-ai/sortie/internal/workspace"

	// Import adapter packages for their init-time registrations.
	_ "github.com/sortie-ai/sortie/internal/agent/claude"
	_ "github.com/sortie-ai/sortie/internal/agent/copilot"
	_ "github.com/sortie-ai/sortie/internal/agent/mock"
	_ "github.com/sortie-ai/sortie/internal/scm/github"
	_ "github.com/sortie-ai/sortie/internal/tracker/file"
	_ "github.com/sortie-ai/sortie/internal/tracker/jira"
)

// serverShutdownTimeout controls how long [run] waits for the HTTP server
// to drain active connections on graceful shutdown. Overridden in tests to
// exercise the shutdown-error path without a 5-second wait.
var serverShutdownTimeout = 5 * time.Second

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	// Log the signal that triggers shutdown. signal.NotifyContext
	// cancels ctx but discards the signal identity, so a parallel
	// channel captures it for operator diagnostics.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig, ok := <-sigCh
		if ok {
			slog.Info("signal received, initiating shutdown",
				slog.String("signal", sig.String()),
				slog.Int("pid", os.Getpid()),
			)
		}
	}()

	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	signal.Stop(sigCh)
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) int {
	br, code := boot(ctx, bootParams{args: args, stdout: stdout, stderr: stderr})
	if code != 0 || br.mgr == nil {
		return code
	}
	defer br.mgr.Stop()
	if closer, ok := br.trackerAdapter.(io.Closer); ok {
		defer closer.Close() //nolint:errcheck // best-effort cleanup at shutdown
	}

	logAttrs := []any{
		slog.String("version", Version),
		slog.String("workflow_path", br.path),
	}
	if br.serverEnabled && !br.dryRun {
		logAttrs = append(logAttrs, slog.String("server_addr", net.JoinHostPort(br.serverHost, strconv.Itoa(br.serverPort))))
	}
	if br.effectiveLevel != slog.LevelInfo {
		logAttrs = append(logAttrs, slog.String("log_level", br.effectiveLevel.String()))
	}
	if br.effectiveFormat != logging.FormatText {
		logAttrs = append(logAttrs, slog.String("log_format", string(br.effectiveFormat)))
	}
	if br.dryRun {
		br.logger.Info("sortie dry-run starting", logAttrs...)
		return runDryRun(ctx, br.cfg, br.logger, br.trackerAdapter)
	}
	br.logger.Info("sortie starting", logAttrs...)

	// --- Database open, migrate, and recovery ---

	workflowDir := filepath.Dir(br.path)
	dbPath := resolveDBPath(br.cfg.DBPath, workflowDir)
	br.logger.Info("database path resolved", slog.String("db_path", dbPath))
	store, err := persistence.Open(ctx, dbPath)
	if err != nil {
		br.logger.Error("failed to open database", slog.Any("error", err))
		return 1
	}
	defer store.Close() //nolint:errcheck // best-effort cleanup at shutdown

	if err := store.Migrate(ctx); err != nil {
		br.logger.Error("failed to migrate database", slog.Any("error", err))
		return 1
	}

	pendingRetries, err := store.LoadRetryEntriesForRecovery(ctx, time.Now().UnixMilli())
	if err != nil {
		br.logger.Error("failed to load retry entries", slog.Any("error", err))
		return 1
	}

	var totals orchestrator.AgentTotals
	metrics, found, err := store.LoadAggregateMetrics(ctx, "agent_totals")
	if err != nil {
		br.logger.Warn("failed to load agent totals, using zero values", slog.Any("error", err))
	} else if found {
		totals = orchestrator.AgentTotals{
			InputTokens:     metrics.InputTokens,
			OutputTokens:    metrics.OutputTokens,
			TotalTokens:     metrics.TotalTokens,
			CacheReadTokens: metrics.CacheReadTokens,
			SecondsRunning:  metrics.SecondsRunning,
		}
	}

	state := orchestrator.NewState(
		br.cfg.Polling.IntervalMS,
		br.cfg.Agent.MaxConcurrentAgents,
		br.cfg.Agent.MaxConcurrentByState,
		totals,
	)
	orchestrator.PopulateRetries(state, pendingRetries)

	// --- Agent adapter construction ---

	agentCtor, err := registry.Agents.Get(br.cfg.Agent.Kind)
	if err != nil {
		br.logger.Error("unknown agent kind", slog.String("kind", br.cfg.Agent.Kind), slog.Any("error", err))
		return 1
	}
	agentCfgMap := agentConfigMap(br.cfg.Agent)
	mergeExtensions(agentCfgMap, br.cfg.Extensions, br.cfg.Agent.Kind)
	agentAdapter, err := agentCtor(agentCfgMap)
	if err != nil {
		br.logger.Error("failed to construct agent adapter", slog.Any("error", err))
		return 1
	}
	if closer, ok := agentAdapter.(io.Closer); ok {
		defer closer.Close() //nolint:errcheck // best-effort cleanup at shutdown
	}

	// --- Startup terminal workspace cleanup ---

	keys, err := workspace.ListWorkspaceKeys(br.cfg.Workspace.Root)
	if err != nil {
		br.logger.Warn("failed to list workspace keys, skipping cleanup", slog.Any("error", err))
	} else if len(keys) > 0 {
		states, fetchErr := br.trackerAdapter.FetchIssueStatesByIdentifiers(ctx, keys)
		if fetchErr != nil {
			br.logger.Warn("failed to fetch issue states for workspace cleanup", slog.Any("error", fetchErr))
		} else {
			terminalSet := make(map[string]struct{}, len(br.cfg.Tracker.TerminalStates))
			for _, s := range br.cfg.Tracker.TerminalStates {
				terminalSet[strings.ToLower(s)] = struct{}{}
			}
			var toClean []string
			for _, key := range keys {
				if st, ok := states[key]; ok {
					if _, terminal := terminalSet[strings.ToLower(st)]; terminal {
						toClean = append(toClean, key)
					}
				}
			}
			if len(toClean) > 0 {
				workspace.CleanupTerminal(ctx, workspace.CleanupTerminalParams{
					Root:          br.cfg.Workspace.Root,
					Identifiers:   toClean,
					BeforeRemove:  br.cfg.Hooks.BeforeRemove,
					HookTimeoutMS: br.cfg.Hooks.TimeoutMS,
					Logger:        br.logger,
				})
			}
		}
	}

	// --- Orchestrator construction and event loop ---

	br.logger.Info("sortie started")

	var orchMetrics domain.Metrics
	var promMetrics *server.PromMetrics

	toolRegistry := domain.NewToolRegistry()
	if br.cfg.Tracker.Project != "" {
		toolRegistry.Register(trackerapi.New(br.trackerAdapter, br.cfg.Tracker.Project))
	}

	var ciProvider domain.CIStatusProvider
	if br.cfg.CIFeedback.Kind != "" {
		ciCtor, ciErr := registry.CIProviders.Get(br.cfg.CIFeedback.Kind)
		if ciErr != nil {
			br.logger.Error("unknown CI provider kind",
				slog.String("kind", br.cfg.CIFeedback.Kind),
				slog.Any("error", ciErr),
			)
			return 1
		}
		adapterCfgMap := make(map[string]any)
		mergeExtensions(adapterCfgMap, br.cfg.Extensions, br.cfg.CIFeedback.Kind)
		if br.cfg.CIFeedback.Kind == br.cfg.Tracker.Kind {
			mergeTrackerCredentials(adapterCfgMap, br.cfg.Tracker)
		}
		ciProvider, ciErr = ciCtor(br.cfg.CIFeedback.MaxLogLines, adapterCfgMap)
		if ciErr != nil {
			br.logger.Error("failed to construct CI provider",
				slog.String("kind", br.cfg.CIFeedback.Kind),
				slog.Any("error", ciErr),
			)
			return 1
		}
		br.logger.Info("CI feedback enabled",
			slog.String("kind", br.cfg.CIFeedback.Kind),
			slog.Int("max_retries", br.cfg.CIFeedback.MaxRetries),
			slog.String("escalation", br.cfg.CIFeedback.Escalation),
		)
	}

	var scmAdapter domain.SCMAdapter
	var reviewConfig orchestrator.ReviewReactionConfig
	if rc, ok := br.cfg.Reactions["review_comments"]; ok && rc.Provider != "" {
		scmCtor, scmErr := registry.SCMAdapters.Get(rc.Provider)
		if scmErr != nil {
			br.logger.Error("unknown SCM adapter kind",
				slog.String("kind", rc.Provider),
				slog.Any("error", scmErr),
			)
			return 1
		}
		adapterCfgMap := make(map[string]any)
		mergeExtensions(adapterCfgMap, br.cfg.Extensions, rc.Provider)
		if rc.Provider == br.cfg.Tracker.Kind {
			mergeTrackerCredentials(adapterCfgMap, br.cfg.Tracker)
		}
		for k, v := range rc.Extra {
			if _, exists := adapterCfgMap[k]; !exists {
				adapterCfgMap[k] = v
			}
		}
		scmAdapter, scmErr = scmCtor(adapterCfgMap)
		if scmErr != nil {
			br.logger.Error("failed to construct SCM adapter",
				slog.String("kind", rc.Provider),
				slog.Any("error", scmErr),
			)
			return 1
		}
		reviewConfig, scmErr = orchestrator.BuildReviewReactionConfig(rc)
		if scmErr != nil {
			br.logger.Error("invalid review reaction config", slog.Any("error", scmErr))
			return 1
		}
		br.logger.Info("review comment routing enabled",
			slog.String("kind", rc.Provider),
			slog.Int("max_continuation_turns", reviewConfig.MaxContinuationTurns),
			slog.Int("poll_interval_ms", reviewConfig.PollIntervalMS),
		)
	}

	// Attempt to bind the HTTP server before constructing metrics so
	// that graceful degradation on an implicit default port conflict
	// skips Prometheus collector creation entirely.
	var ln net.Listener
	serverEnabled := br.serverEnabled
	if serverEnabled {
		addr := net.JoinHostPort(br.serverHost, strconv.Itoa(br.serverPort))
		var listenErr error
		ln, listenErr = (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
		if listenErr != nil {
			if br.portIsImplicit {
				br.logger.Warn("http server listen failed; running without HTTP server",
					slog.String("addr", addr),
					slog.Any("error", listenErr),
				)
				serverEnabled = false
			} else {
				br.logger.Error("http server listen failed",
					slog.String("addr", addr),
					slog.Any("error", listenErr),
				)
				return 1
			}
		}
	}

	if serverEnabled {
		promMetrics = server.NewPromMetrics(Version, runtime.Version())
		orchMetrics = promMetrics
	}
	if ms, ok := br.trackerAdapter.(domain.MetricsSetter); ok && orchMetrics != nil {
		ms.SetMetrics(orchMetrics)
	}

	o := orchestrator.NewOrchestrator(orchestrator.OrchestratorParams{
		State:            state,
		Logger:           br.logger,
		TrackerAdapter:   br.trackerAdapter,
		AgentAdapter:     agentAdapter,
		WorkflowManager:  br.mgr,
		Store:            store,
		PreflightParams:  br.preflightParams,
		Metrics:          orchMetrics,
		ToolRegistry:     toolRegistry,
		WorkflowFileFunc: br.mgr.FilePath,
		DBPath:           dbPath,
		CIProvider:       ciProvider,
		SCMAdapter:       scmAdapter,
		ReviewConfig:     reviewConfig,
	})

	var srv *server.Server
	if serverEnabled {
		addr := net.JoinHostPort(br.serverHost, strconv.Itoa(br.serverPort))
		srv = server.New(server.Params{
			SnapshotFn:       o.SnapshotFunc(),
			RefreshFn:        o.RefreshFunc(),
			Logger:           br.logger,
			Addr:             addr,
			Version:          Version,
			StartedAt:        time.Now(),
			SlotFunc:         func() int { return br.mgr.Config().Agent.MaxConcurrentAgents },
			MetricsRegistry:  promMetrics.Registry(),
			DBPingFn:         func(ctx context.Context) error { return store.Ping(ctx) },
			PreflightFn:      o.PreflightOK,
			WorkflowLoadedFn: func() bool { return br.mgr.Config().Tracker.Kind != "" },
			RunHistoryFn: func(ctx context.Context, limit int) ([]server.RunHistoryEntry, error) {
				runs, err := store.QueryRecentRunHistory(ctx, limit, 0)
				if err != nil {
					return nil, err
				}
				out := make([]server.RunHistoryEntry, len(runs))
				for i, r := range runs {
					out[i] = server.RunHistoryEntry{
						Identifier:     r.Identifier,
						DisplayID:      r.DisplayID,
						Attempt:        r.Attempt,
						Status:         r.Status,
						WorkflowFile:   r.WorkflowFile,
						StartedAt:      r.StartedAt,
						CompletedAt:    r.CompletedAt,
						Error:          r.Error,
						TurnsCompleted: r.TurnsCompleted,
					}
				}
				return out, nil
			},
		})
		o.AddObserver(srv)

		go func() {
			br.logger.Info("http server listening",
				slog.String("addr", ln.Addr().String()),
			)
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				br.logger.Error("http server error", slog.Any("error", err))
			}
		}()
	}

	if srv != nil {
		drainSrv := srv
		go func() {
			<-ctx.Done()
			drainSrv.SetDraining()
		}()
	}

	o.Run(ctx)

	if srv != nil {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), serverShutdownTimeout)
		defer shutdownCancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			br.logger.Error("http server shutdown error", slog.Any("error", err))
		}
	}

	br.logger.Info("shutting down")
	return 0
}
