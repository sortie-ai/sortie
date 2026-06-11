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
	"maps"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
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
	_ "github.com/sortie-ai/sortie/internal/agent/codex"
	_ "github.com/sortie-ai/sortie/internal/agent/copilot"
	_ "github.com/sortie-ai/sortie/internal/agent/kiro"
	_ "github.com/sortie-ai/sortie/internal/agent/mock"
	_ "github.com/sortie-ai/sortie/internal/agent/opencode"
	_ "github.com/sortie-ai/sortie/internal/notify/slack"
	_ "github.com/sortie-ai/sortie/internal/notify/webhook"
	_ "github.com/sortie-ai/sortie/internal/scm/github"
	_ "github.com/sortie-ai/sortie/internal/tracker/file"
	_ "github.com/sortie-ai/sortie/internal/tracker/jira"
)

// serverShutdownTimeout controls how long [run] waits for the HTTP server
// to drain active connections on graceful shutdown. Overridden in tests to
// exercise the shutdown-error path without a 5-second wait.
var serverShutdownTimeout = 5 * time.Second

// buildAgentAdapterCache eagerly constructs every registered agent
// adapter so dispatch-rule routing can resolve any referenced kind
// without per-issue construction. The workflow default kind reuses
// the already-constructed adapter to avoid double-initialization.
// Adapter resolution and construction failures for non-default kinds
// are logged at warn level and skipped, so a missing or misconfigured
// optional adapter does not block startup; the dispatch builder's
// preflight probe rejects unknown kinds before they reach this cache,
// and any rule that references a skipped kind surfaces as an unknown
// agent kind at dispatch time. If log is nil, [slog.Default] is used.
func buildAgentAdapterCache(cfg config.ServiceConfig, defaultAdapter domain.AgentAdapter, log *slog.Logger) map[string]domain.AgentAdapter {
	if log == nil {
		log = slog.Default()
	}
	cache := map[string]domain.AgentAdapter{
		cfg.Agent.Kind: defaultAdapter,
	}
	for _, kind := range registry.Agents.Kinds() {
		if _, exists := cache[kind]; exists {
			continue
		}
		ctor, err := registry.Agents.Get(kind)
		if err != nil {
			log.Warn("skipping agent adapter cache entry, registry lookup failed",
				slog.String("kind", kind),
				slog.Any("error", err),
			)
			continue
		}
		cfgMap := agentConfigMap(cfg.Agent)
		cfgMap["kind"] = kind
		mergeExtensions(cfgMap, cfg.Extensions, kind)
		adapter, err := ctor(cfgMap)
		if err != nil {
			log.Warn("skipping agent adapter cache entry, construction failed",
				slog.String("kind", kind),
				slog.Any("error", err),
			)
			continue
		}
		cache[kind] = adapter
	}
	return cache
}

// makeAgentAdapterByKind returns the closure passed into
// [orchestrator.OrchestratorParams.AgentAdapterByKind] and the retry
// timer params. The returned function reads from cache for O(1)
// lookup and returns a wrapped *[registry.RegistryError] for unknown
// kinds so callers can detect the missing-adapter category. The
// Available list on the returned error is sorted for stable diagnostic
// output, matching the contract of [registry.Registry.Get].
func makeAgentAdapterByKind(cache map[string]domain.AgentAdapter) func(kind string) (domain.AgentAdapter, error) {
	return func(kind string) (domain.AgentAdapter, error) {
		if adapter, ok := cache[kind]; ok {
			return adapter, nil
		}
		return nil, &registry.RegistryError{
			Dimension: "agent",
			Kind:      kind,
			Available: slices.Sorted(maps.Keys(cache)),
		}
	}
}

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
	orchestrator.PopulateRetries(state, pendingRetries, br.logger)

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

	agentAdapterCache := buildAgentAdapterCache(br.cfg, agentAdapter, br.logger)
	defer func() {
		for kind, adapter := range agentAdapterCache {
			if kind == br.cfg.Agent.Kind {
				// Already closed via the top-level defer.
				continue
			}
			if closer, ok := adapter.(io.Closer); ok {
				if cerr := closer.Close(); cerr != nil {
					br.logger.Warn("agent adapter close failed",
						slog.String("kind", kind),
						slog.Any("error", cerr),
					)
				}
			}
		}
	}()
	agentAdapterByKind := makeAgentAdapterByKind(agentAdapterCache)

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
	var autoMergeConfig orchestrator.AutoMergeReactionConfig
	var autoMergeConfigured bool

	reviewRC, hasReview := br.cfg.Reactions["review_comments"]
	autoMergeRC, hasAutoMerge := br.cfg.Reactions["auto_merge"]
	reviewActive := hasReview && reviewRC.Provider != ""
	autoMergeActive := hasAutoMerge && autoMergeRC.Provider != ""

	if reviewActive && autoMergeActive && reviewRC.Provider != autoMergeRC.Provider {
		br.logger.Error("unsupported: reactions.review_comments and reactions.auto_merge must use the same provider",
			slog.String("review_provider", reviewRC.Provider),
			slog.String("auto_merge_provider", autoMergeRC.Provider),
		)
		return 1
	}

	if reviewActive || autoMergeActive {
		provider := reviewRC.Provider
		if provider == "" {
			provider = autoMergeRC.Provider
		}
		scmCtor, scmErr := registry.SCMAdapters.Get(provider)
		if scmErr != nil {
			br.logger.Error("unknown SCM adapter kind",
				slog.String("kind", provider),
				slog.Any("error", scmErr),
			)
			return 1
		}
		adapterCfgMap := make(map[string]any)
		mergeExtensions(adapterCfgMap, br.cfg.Extensions, provider)
		if provider == br.cfg.Tracker.Kind {
			mergeTrackerCredentials(adapterCfgMap, br.cfg.Tracker)
		}
		if reviewActive {
			for k, v := range reviewRC.Extra {
				if _, exists := adapterCfgMap[k]; !exists {
					adapterCfgMap[k] = v
				}
			}
		}
		if autoMergeActive {
			for k, v := range autoMergeRC.Extra {
				if _, exists := adapterCfgMap[k]; !exists {
					adapterCfgMap[k] = v
				}
			}
		}
		scmAdapter, scmErr = scmCtor(adapterCfgMap)
		if scmErr != nil {
			br.logger.Error("failed to construct SCM adapter",
				slog.String("kind", provider),
				slog.Any("error", scmErr),
			)
			return 1
		}

		if reviewActive {
			reviewConfig, scmErr = orchestrator.BuildReviewReactionConfig(reviewRC)
			if scmErr != nil {
				br.logger.Error("invalid review reaction config", slog.Any("error", scmErr))
				return 1
			}
			br.logger.Info("review comment routing enabled",
				slog.String("kind", reviewRC.Provider),
				slog.Int("max_continuation_turns", reviewConfig.MaxContinuationTurns),
				slog.Int("poll_interval_ms", reviewConfig.PollIntervalMS),
			)
		}

		if autoMergeActive {
			autoMergeConfig, scmErr = orchestrator.BuildAutoMergeReactionConfig(autoMergeRC)
			if scmErr != nil {
				br.logger.Error("invalid auto_merge reaction config", slog.Any("error", scmErr))
				return 1
			}
			autoMergeConfigured = true

			passed, _, preflightErr := orchestrator.RunAutoMergePreflight(ctx, scmAdapter, autoMergeConfig.DeleteBranch, br.logger)
			state.AutoMergePreflightFailed = !passed
			if preflightErr != nil && orchestrator.IsAutoMergePreflightTransportClass(preflightErr) {
				state.AutoMergePreflightRetryDueAt = time.Now().UTC().Add(orchestrator.AutoMergePreflightRetryDelay)
			}

			br.logger.Info("auto_merge reaction enabled",
				slog.String("provider", autoMergeRC.Provider),
				slog.String("strategy", string(autoMergeConfig.Strategy)),
				slog.Bool("require_ci", autoMergeConfig.RequireCI),
				slog.Bool("delete_branch", autoMergeConfig.DeleteBranch),
				slog.Int("poll_interval_ms", autoMergeConfig.PollIntervalMS),
				slog.Int("max_retries", autoMergeConfig.MaxRetries),
			)
		}
	}

	recoveryEnabled := br.cfg.Tracker.HandoffState != "" && (ciProvider != nil || scmAdapter != nil)
	recoveryOutcome := orchestrator.PendingReactionRecoveryResult{}
	var recoveryErr error
	if recoveryEnabled {
		recoveryNow := time.Now().UTC()
		recoveryLookback := orchestrator.PendingReactionRecoveryLookback
		recoveryCutoff := recoveryNow.Add(-recoveryLookback)
		var recoveryRuns []persistence.RunHistory
		recoveryRuns, recoveryErr = store.LoadLatestSuccessfulRunsForReactionRecovery(ctx, recoveryCutoff, orchestrator.PendingReactionRecoveryMaxCandidates)
		if recoveryErr == nil {
			recoveryOutcome, recoveryErr = orchestrator.RecoverPendingReactions(ctx, state, recoveryRuns, orchestrator.PendingReactionRecoveryParams{
				WorkspaceRoot:               br.cfg.Workspace.Root,
				TrackerAdapter:              br.trackerAdapter,
				HandoffState:                br.cfg.Tracker.HandoffState,
				TerminalStates:              br.cfg.Tracker.TerminalStates,
				CIProvider:                  ciProvider,
				SCMAdapter:                  scmAdapter,
				AutoMergeReactionConfigured: autoMergeConfigured,
				RecoveryLookback:            recoveryLookback,
				MaxCandidates:               orchestrator.PendingReactionRecoveryMaxCandidates,
				NowFunc: func() time.Time {
					return recoveryNow
				},
				Logger: br.logger,
			})
		}
	}
	recoveryLogAttrs := []any{
		slog.Bool("enabled", recoveryEnabled),
		slog.Int("candidates", recoveryOutcome.Candidates),
		slog.Int("cap_skipped", recoveryOutcome.CapSkipped),
		slog.Int("state_checked", recoveryOutcome.StateChecked),
		slog.Int("review_recovered", recoveryOutcome.ReviewRecovered),
		slog.Int("ci_recovered", recoveryOutcome.CIRecovered),
		slog.Int("auto_merge_recovered", recoveryOutcome.AutoMergeRecovered),
		slog.Int("stale_skipped", recoveryOutcome.StaleSkipped),
		slog.Int("skipped", recoveryOutcome.Skipped),
	}
	if recoveryErr != nil {
		recoveryLogAttrs = append(recoveryLogAttrs, slog.Bool("success", false), slog.Any("error", recoveryErr))
		br.logger.Warn("pending reaction recovery failed", recoveryLogAttrs...)
	} else {
		recoveryLogAttrs = append(recoveryLogAttrs, slog.Bool("success", true))
		br.logger.Info("pending reaction recovery completed", recoveryLogAttrs...)
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

	// sessionToolFunc builds the per-session tool registry the worker
	// renders into the first-turn advertisement, so the advertised set
	// matches the set the MCP sidecar serves over tools/list. It captures
	// the session-invariant gating inputs and receives the late-bound
	// inputs (issue id, workspace path, session id) at call time. The
	// read-only store the builder opens to make the database-backed tools
	// constructible is closed before the registry is returned: the worker
	// reads only tool metadata, never executing the tools, so the
	// connection is not needed beyond construction.
	sessionToolFunc := func(ctx context.Context, issueID, workspacePath, sessionID string) (*domain.ToolRegistry, error) {
		sessionTools, err := BuildSessionToolRegistry(ctx, br.logger, SessionToolParams{
			TrackerAdapter: br.trackerAdapter,
			Project:        br.cfg.Tracker.Project,
			DBPath:         dbPath,
			MaxTokens:      br.cfg.Agent.MaxTokens,
			MaxSessions:    br.cfg.Agent.MaxSessions,
			Notifications:  br.cfg.Notifications.Backends,
			IssueID:        issueID,
			WorkspacePath:  workspacePath,
			SessionID:      sessionID,
		})
		if err != nil {
			return nil, err
		}
		if sessionTools.Store != nil {
			sessionTools.Store.Close() //nolint:errcheck,gosec // best-effort close after construction; tools are never executed in this path
		}
		return sessionTools.Registry, nil
	}

	o := orchestrator.NewOrchestrator(orchestrator.OrchestratorParams{
		State:                       state,
		Logger:                      br.logger,
		TrackerAdapter:              br.trackerAdapter,
		AgentAdapter:                agentAdapter,
		AgentAdapterByKind:          agentAdapterByKind,
		WorkflowManager:             br.mgr,
		Store:                       store,
		PreflightParams:             br.preflightParams,
		Metrics:                     orchMetrics,
		ToolRegistry:                toolRegistry,
		SessionToolRegistryFunc:     sessionToolFunc,
		WorkflowFileFunc:            br.mgr.FilePath,
		DBPath:                      dbPath,
		CIProvider:                  ciProvider,
		SCMAdapter:                  scmAdapter,
		ReviewConfig:                reviewConfig,
		AutoMergeConfig:             autoMergeConfig,
		AutoMergeReactionConfigured: autoMergeConfigured,
	})

	var srv *server.Server
	if serverEnabled {
		tokenRates, trWarnings := server.ParseTokenRates(br.cfg.Extensions)
		for _, w := range trWarnings {
			br.logger.Warn("skipped invalid token rate entry", slog.String("detail", w))
		}

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
			TokenRates:       tokenRates,
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
