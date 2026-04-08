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
	"encoding/json"
	"errors"
	"flag"
	"fmt"
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

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/orchestrator"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/prompt"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/server"
	"github.com/sortie-ai/sortie/internal/tool/trackerapi"
	"github.com/sortie-ai/sortie/internal/workflow"
	"github.com/sortie-ai/sortie/internal/workspace"

	// Import adapter packages for their init-time registrations.
	_ "github.com/sortie-ai/sortie/internal/agent/claude"
	_ "github.com/sortie-ai/sortie/internal/agent/copilot"
	_ "github.com/sortie-ai/sortie/internal/agent/mock"
	_ "github.com/sortie-ai/sortie/internal/tracker/file"
	_ "github.com/sortie-ai/sortie/internal/tracker/github"
	_ "github.com/sortie-ai/sortie/internal/tracker/jira"
)

const (
	defaultServerPort = 7678
	defaultServerHost = "127.0.0.1"
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
	// Intercept short flags (-h, -V) before subcommand dispatch or flag
	// parsing, because the flag package does not recognize single-dash
	// aliases for long flags.
	action := interceptShortFlags(args)
	if action == "help" {
		printHelp(stdout)
		return 0
	}
	if action == "version" {
		fmt.Fprint(stdout, versionBanner()) //nolint:errcheck // stdout write failure is unrecoverable
		return 0
	}

	// Subcommand dispatch — must occur before top-level flag parsing
	// because subcommands define their own flag sets.
	if len(args) > 0 && args[0] == "validate" {
		return runValidate(ctx, args[1:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "mcp-server" {
		return runMCPServer(ctx, args[1:], stdout, stderr)
	}

	fs := flag.NewFlagSet("sortie", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	logFormat := fs.String("log-format", "", `Log output format: "text", "json" (default "text")`)
	logLevel := fs.String("log-level", "", `Log verbosity: "debug", "info", "warn", "error" (default "info")`)
	dryRun := fs.Bool("dry-run", false, "Run one poll cycle without spawning agents or writing to the database, then exit")
	port := fs.Int("port", defaultServerPort, "HTTP server port (0 to disable)")
	host := fs.String("host", defaultServerHost, "HTTP server bind address (IP address)")
	envFile := fs.String("env-file", "", "Path to .env file for config overrides")
	showVersion := fs.Bool("version", false, "Print program's version information and quit")
	dumpVersion := fs.Bool("dumpversion", false, "Print the version of the program and don't do anything else")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printHelp(stdout)
			return 0
		}
		fmt.Fprintf(stderr, "sortie: %s\n", err) //nolint:errcheck // stderr write failure is unrecoverable
		return 1
	}

	if *dumpVersion {
		fmt.Fprintln(stdout, Version) //nolint:errcheck // stdout write failure is unrecoverable
		return 0
	}

	if *showVersion {
		fmt.Fprint(stdout, versionBanner()) //nolint:errcheck // stdout write failure is unrecoverable
		return 0
	}

	path, err := resolveWorkflowPath(fs.Args())
	if err != nil {
		fmt.Fprintf(stderr, "sortie: %s\n", err) //nolint:errcheck // stderr write failure is unrecoverable
		return 1
	}

	if *envFile != "" {
		config.SetDotEnvPath(*envFile)
		// Resolve to absolute and export to the process environment so
		// CollectSortieEnv propagates the path to the MCP server via
		// the config env block. The MCP server's working directory
		// differs from the orchestrator's, so a relative path would
		// not resolve correctly.
		if abs, err := filepath.Abs(*envFile); err == nil {
			if err := os.Setenv("SORTIE_ENV_FILE", abs); err != nil {
				fmt.Fprintf(stderr, "sortie: setting SORTIE_ENV_FILE: %s\n", err) //nolint:errcheck // stderr write failure is unrecoverable
				return 1
			}
		}
	}

	var portFlagSet, hostFlagSet, logLevelSet, logFormatSet bool
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "port":
			portFlagSet = true
		case "host":
			hostFlagSet = true
		case "log-level":
			logLevelSet = true
		case "log-format":
			logFormatSet = true
		}
	})

	// Early logging setup before workflow load — if the CLI flags are set,
	// apply them immediately so all subsequent output respects the operator's
	// choice. Otherwise, start at the default info level with text format.
	var effectiveLevel = slog.LevelInfo
	if logLevelSet {
		lvl, err := logging.ParseLevel(*logLevel)
		if err != nil {
			fmt.Fprintf(stderr, "sortie: %s\n", err) //nolint:errcheck // stderr write failure is unrecoverable
			return 1
		}
		effectiveLevel = lvl
	}
	var effectiveFormat = logging.FormatText
	if logFormatSet {
		parsedFmt, err := logging.ParseFormat(*logFormat)
		if err != nil {
			fmt.Fprintf(stderr, "sortie: %s\n", err) //nolint:errcheck // stderr write failure is unrecoverable
			return 1
		}
		effectiveFormat = parsedFmt
	}
	logger := logging.Setup(stderr, effectiveLevel, effectiveFormat)

	mgr, err := workflow.NewManager(path, logger,
		workflow.WithValidateFunc(orchestrator.ValidateConfigForPromotion))
	if err != nil {
		fmt.Fprintf(stderr, "sortie: %s\n", err) //nolint:errcheck // stderr write failure is unrecoverable
		return 1
	}

	if err := mgr.Start(ctx); err != nil {
		fmt.Fprintf(stderr, "sortie: %s\n", err) //nolint:errcheck // stderr write failure is unrecoverable
		return 1
	}
	defer mgr.Stop()

	// --- Preflight validation ---

	preflightParams := orchestrator.PreflightParams{
		ReloadWorkflow:  mgr.Reload,
		ConfigFunc:      mgr.Config,
		TrackerRegistry: registry.Trackers,
		AgentRegistry:   registry.Agents,
	}

	validation := orchestrator.ValidateDispatchConfig(preflightParams)
	if !validation.OK() {
		logger.Error("dispatch preflight failed", slog.Any("error", validation))
		return 1
	}

	// Read config after preflight reload so state and adapters reflect
	// the validated configuration.
	cfg := mgr.Config()

	// Post-config log level and format adjustment. When the CLI flags
	// were not set, check the workflow extensions for logging overrides.
	var needResetup bool
	if !logLevelSet {
		lvl, err := resolveLogLevel("", false, cfg.Extensions)
		if err != nil {
			fmt.Fprintf(stderr, "sortie: %s\n", err) //nolint:errcheck // stderr write failure is unrecoverable
			return 1
		}
		if lvl != slog.LevelInfo {
			effectiveLevel = lvl
			needResetup = true
		}
	}
	if !logFormatSet {
		resolvedFmt, err := resolveLogFormat("", false, cfg.Extensions)
		if err != nil {
			fmt.Fprintf(stderr, "sortie: %s\n", err) //nolint:errcheck // stderr write failure is unrecoverable
			return 1
		}
		if resolvedFmt != logging.FormatText {
			effectiveFormat = resolvedFmt
			needResetup = true
		}
	}
	if needResetup {
		logger = logging.Setup(stderr, effectiveLevel, effectiveFormat)
	}

	serverPort, serverEnabled, portErr := resolveServerPort(*port, portFlagSet, cfg.Extensions)
	if portErr != nil {
		logger.Error("server port configuration error", slog.Any("error", portErr))
		return 1
	}

	serverHost, hostErr := resolveServerHost(*host, hostFlagSet, cfg.Extensions)
	if hostErr != nil {
		logger.Error("server host configuration error", slog.Any("error", hostErr))
		return 1
	}

	logAttrs := []any{
		slog.String("version", Version),
		slog.String("workflow_path", path),
	}
	if serverEnabled && !*dryRun {
		logAttrs = append(logAttrs, slog.String("server_addr", net.JoinHostPort(serverHost, strconv.Itoa(serverPort))))
	}
	if effectiveLevel != slog.LevelInfo {
		logAttrs = append(logAttrs, slog.String("log_level", effectiveLevel.String()))
	}
	if effectiveFormat != logging.FormatText {
		logAttrs = append(logAttrs, slog.String("log_format", string(effectiveFormat)))
	}
	if *dryRun {
		logger.Info("sortie dry-run starting", logAttrs...)
	} else {
		logger.Info("sortie starting", logAttrs...)
	}

	// --- Tracker adapter construction (shared by normal and dry-run paths) ---

	trackerCtor, err := registry.Trackers.Get(cfg.Tracker.Kind)
	if err != nil {
		logger.Error("unknown tracker kind", slog.String("kind", cfg.Tracker.Kind), slog.Any("error", err))
		return 1
	}
	trackerCfgMap := trackerConfigMap(cfg.Tracker)
	trackerCfgMap["user_agent"] = "sortie/" + Version
	mergeExtensions(trackerCfgMap, cfg.Extensions, cfg.Tracker.Kind)
	trackerAdapter, err := trackerCtor(trackerCfgMap)
	if err != nil {
		logger.Error("failed to construct tracker adapter", slog.Any("error", err))
		return 1
	}
	if closer, ok := trackerAdapter.(io.Closer); ok {
		defer closer.Close() //nolint:errcheck // best-effort cleanup at shutdown
	}

	// --- Dry-run branch: single poll cycle, no database or agents ---

	if *dryRun {
		return runDryRun(ctx, cfg, logger, trackerAdapter)
	}

	// --- Database open, migrate, and recovery ---

	workflowDir := filepath.Dir(path)
	dbPath := resolveDBPath(cfg.DBPath, workflowDir)
	logger.Info("database path resolved", slog.String("db_path", dbPath))
	store, err := persistence.Open(ctx, dbPath)
	if err != nil {
		logger.Error("failed to open database", slog.Any("error", err))
		return 1
	}
	defer store.Close() //nolint:errcheck // best-effort cleanup at shutdown

	if err := store.Migrate(ctx); err != nil {
		logger.Error("failed to migrate database", slog.Any("error", err))
		return 1
	}

	pendingRetries, err := store.LoadRetryEntriesForRecovery(ctx, time.Now().UnixMilli())
	if err != nil {
		logger.Error("failed to load retry entries", slog.Any("error", err))
		return 1
	}

	var totals orchestrator.AgentTotals
	metrics, found, err := store.LoadAggregateMetrics(ctx, "agent_totals")
	if err != nil {
		logger.Warn("failed to load agent totals, using zero values", slog.Any("error", err))
	} else if found {
		totals = orchestrator.AgentTotals{
			InputTokens:     metrics.InputTokens,
			OutputTokens:    metrics.OutputTokens,
			TotalTokens:     metrics.TotalTokens,
			CacheReadTokens: metrics.CacheReadTokens,
			SecondsRunning:  metrics.SecondsRunning,
		}
	}

	// --- State construction and retry population ---

	state := orchestrator.NewState(
		cfg.Polling.IntervalMS,
		cfg.Agent.MaxConcurrentAgents,
		cfg.Agent.MaxConcurrentByState,
		totals,
	)
	orchestrator.PopulateRetries(state, pendingRetries)

	// --- Agent adapter construction ---

	agentCtor, err := registry.Agents.Get(cfg.Agent.Kind)
	if err != nil {
		logger.Error("unknown agent kind", slog.String("kind", cfg.Agent.Kind), slog.Any("error", err))
		return 1
	}
	agentCfgMap := agentConfigMap(cfg.Agent)
	mergeExtensions(agentCfgMap, cfg.Extensions, cfg.Agent.Kind)
	agentAdapter, err := agentCtor(agentCfgMap)
	if err != nil {
		logger.Error("failed to construct agent adapter", slog.Any("error", err))
		return 1
	}
	if closer, ok := agentAdapter.(io.Closer); ok {
		defer closer.Close() //nolint:errcheck // best-effort cleanup at shutdown
	}

	// --- Startup terminal workspace cleanup ---

	keys, err := workspace.ListWorkspaceKeys(cfg.Workspace.Root)
	if err != nil {
		logger.Warn("failed to list workspace keys, skipping cleanup", slog.Any("error", err))
	} else if len(keys) > 0 {
		states, fetchErr := trackerAdapter.FetchIssueStatesByIdentifiers(ctx, keys)
		if fetchErr != nil {
			logger.Warn("failed to fetch issue states for workspace cleanup", slog.Any("error", fetchErr))
		} else {
			terminalSet := make(map[string]struct{}, len(cfg.Tracker.TerminalStates))
			for _, s := range cfg.Tracker.TerminalStates {
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
					Root:          cfg.Workspace.Root,
					Identifiers:   toClean,
					BeforeRemove:  cfg.Hooks.BeforeRemove,
					HookTimeoutMS: cfg.Hooks.TimeoutMS,
					Logger:        logger,
				})
			}
		}
	}

	// --- Orchestrator construction and event loop ---

	logger.Info("sortie started")

	// Metrics and server are wired after a successful listen so that
	// graceful degradation on port conflict does not leave dead
	// references to unused Prometheus collectors.
	var orchMetrics domain.Metrics
	var promMetrics *server.PromMetrics

	toolRegistry := domain.NewToolRegistry()
	if cfg.Tracker.Project != "" {
		toolRegistry.Register(trackerapi.New(trackerAdapter, cfg.Tracker.Project))
	}

	// Resolve CI status provider when ci_feedback.kind is configured.
	var ciProvider domain.CIStatusProvider
	if cfg.CIFeedback.Kind != "" {
		ciCtor, ciErr := registry.CIProviders.Get(cfg.CIFeedback.Kind)
		if ciErr != nil {
			logger.Error("unknown CI provider kind",
				slog.String("kind", cfg.CIFeedback.Kind),
				slog.Any("error", ciErr),
			)
			return 1
		}
		adapterCfgMap := make(map[string]any)
		mergeExtensions(adapterCfgMap, cfg.Extensions, cfg.CIFeedback.Kind)
		if cfg.CIFeedback.Kind == cfg.Tracker.Kind {
			mergeTrackerCredentials(adapterCfgMap, cfg.Tracker)
		}
		ciProvider, ciErr = ciCtor(cfg.CIFeedback.MaxLogLines, adapterCfgMap)
		if ciErr != nil {
			logger.Error("failed to construct CI provider",
				slog.String("kind", cfg.CIFeedback.Kind),
				slog.Any("error", ciErr),
			)
			return 1
		}
		logger.Info("CI feedback enabled",
			slog.String("kind", cfg.CIFeedback.Kind),
			slog.Int("max_retries", cfg.CIFeedback.MaxRetries),
			slog.String("escalation", cfg.CIFeedback.Escalation),
		)
	}

	// Attempt to bind the HTTP server before constructing metrics so
	// that graceful degradation on an implicit default port conflict
	// skips Prometheus collector creation entirely.
	var ln net.Listener
	portIsImplicit := !portFlagSet && !hasServerPortExtension(cfg.Extensions)
	if serverEnabled {
		addr := net.JoinHostPort(serverHost, strconv.Itoa(serverPort))
		var listenErr error
		ln, listenErr = (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
		if listenErr != nil {
			if portIsImplicit {
				logger.Warn("http server listen failed; running without HTTP server",
					slog.String("addr", addr),
					slog.Any("error", listenErr),
				)
				serverEnabled = false
			} else {
				logger.Error("http server listen failed",
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

	if ms, ok := trackerAdapter.(domain.MetricsSetter); ok && orchMetrics != nil {
		ms.SetMetrics(orchMetrics)
	}

	o := orchestrator.NewOrchestrator(orchestrator.OrchestratorParams{
		State:            state,
		Logger:           logger,
		TrackerAdapter:   trackerAdapter,
		AgentAdapter:     agentAdapter,
		WorkflowManager:  mgr,
		Store:            store,
		PreflightParams:  preflightParams,
		Metrics:          orchMetrics,
		ToolRegistry:     toolRegistry,
		WorkflowFileFunc: mgr.FilePath,
		DBPath:           dbPath,
		CIProvider:       ciProvider,
	})

	var srv *server.Server
	if serverEnabled {
		addr := net.JoinHostPort(serverHost, strconv.Itoa(serverPort))
		srv = server.New(server.Params{
			SnapshotFn:       o.SnapshotFunc(),
			RefreshFn:        o.RefreshFunc(),
			Logger:           logger,
			Addr:             addr,
			Version:          Version,
			StartedAt:        time.Now(),
			SlotFunc:         func() int { return mgr.Config().Agent.MaxConcurrentAgents },
			MetricsRegistry:  promMetrics.Registry(),
			DBPingFn:         func(ctx context.Context) error { return store.Ping(ctx) },
			PreflightFn:      o.PreflightOK,
			WorkflowLoadedFn: func() bool { return mgr.Config().Tracker.Kind != "" },
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
			logger.Info("http server listening",
				slog.String("addr", ln.Addr().String()),
			)
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("http server error", slog.Any("error", err))
			}
		}()
	}

	// Set draining flag as soon as the context is cancelled so
	// health probes return 503 during the orchestrator's drain phase
	// while the listener is still open.
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
			logger.Error("http server shutdown error", slog.Any("error", err))
		}
	}

	logger.Info("shutting down")
	return 0
}

func resolveWorkflowPath(args []string) (string, error) {
	if len(args) > 1 {
		return "", fmt.Errorf("too many arguments")
	}
	raw := "./WORKFLOW.md"
	if len(args) == 1 {
		raw = args[0]
	}
	return filepath.Abs(raw)
}

// trackerConfigMap converts typed [config.TrackerConfig] fields into
// the raw map expected by [registry.TrackerConstructor]. Adapter
// packages extract their required fields from this map.
func trackerConfigMap(tc config.TrackerConfig) map[string]any {
	return map[string]any{
		"kind":              tc.Kind,
		"endpoint":          tc.Endpoint,
		"api_key":           tc.APIKey,
		"project":           tc.Project,
		"active_states":     tc.ActiveStates,
		"terminal_states":   tc.TerminalStates,
		"query_filter":      tc.QueryFilter,
		"handoff_state":     tc.HandoffState,
		"in_progress_state": tc.InProgressState,
		"comments": map[string]any{
			"on_dispatch":   tc.Comments.OnDispatch,
			"on_completion": tc.Comments.OnCompletion,
			"on_failure":    tc.Comments.OnFailure,
		},
	}
}

// agentConfigMap converts typed [config.AgentConfig] fields into the
// raw map expected by [registry.AgentConstructor]. Adapter packages
// extract their required fields from this map.
//
// Orchestrator-only fields (max_turns, max_concurrent_agents,
// max_retry_backoff_ms, max_concurrent_agents_by_state) are
// intentionally excluded. They are consumed by the orchestrator via
// the typed [config.AgentConfig] before this map reaches the adapter
// constructor, and including them would shadow adapter-specific
// extension keys of the same name during [mergeExtensions].
func agentConfigMap(ac config.AgentConfig) map[string]any {
	return map[string]any{
		"kind":             ac.Kind,
		"command":          ac.Command,
		"turn_timeout_ms":  ac.TurnTimeoutMS,
		"read_timeout_ms":  ac.ReadTimeoutMS,
		"stall_timeout_ms": ac.StallTimeoutMS,
	}
}

// mergeExtensions copies adapter-specific config from the Extensions
// map into dst. Adapters may define their own configuration fields
// in a sub-object named after their kind value. Existing keys in dst
// are not overwritten.
func mergeExtensions(dst map[string]any, extensions map[string]any, kind string) {
	sub, ok := extensions[kind].(map[string]any)
	if !ok {
		return
	}
	for k, v := range sub {
		if _, exists := dst[k]; !exists {
			dst[k] = v
		}
	}
}

// mergeTrackerCredentials copies api_key, project, and endpoint from
// the tracker config into dst when the corresponding key is absent.
// Called only when the CI provider kind matches the tracker kind so
// that shared-platform credentials flow to the CI adapter without
// operator duplication.
func mergeTrackerCredentials(dst map[string]any, tc config.TrackerConfig) {
	if _, ok := dst["api_key"]; !ok && tc.APIKey != "" {
		dst["api_key"] = tc.APIKey
	}
	if _, ok := dst["project"]; !ok && tc.Project != "" {
		dst["project"] = tc.Project
	}
	if _, ok := dst["endpoint"]; !ok && tc.Endpoint != "" {
		dst["endpoint"] = tc.Endpoint
	}
}

// resolveDBPath returns the effective database file path. An absolute
// cfgPath is used as-is. A relative cfgPath is joined with workflowDir
// so database location is deterministic regardless of the process CWD.
// An empty cfgPath falls back to .sortie.db inside workflowDir.
func resolveDBPath(cfgPath, workflowDir string) string {
	if cfgPath == "" {
		return filepath.Join(workflowDir, ".sortie.db")
	}
	if filepath.IsAbs(cfgPath) {
		return cfgPath
	}
	return filepath.Join(workflowDir, cfgPath)
}

// resolveServerPort determines the effective HTTP server port from the
// CLI flag and workflow extensions. Returns the port, whether the
// server should be started, and an error if an explicitly configured
// port is invalid. When no port is configured, the default port is
// returned with the server enabled. Port 0 disables the server.
func resolveServerPort(portFlag int, portFlagSet bool, extensions map[string]any) (int, bool, error) {
	if portFlagSet {
		if portFlag < 0 || portFlag > 65535 {
			return 0, false, fmt.Errorf("invalid --port value %d: must be between 0 and 65535", portFlag)
		}
		return portFlag, portFlag != 0, nil
	}

	serverExt, ok := extensions["server"].(map[string]any)
	if !ok {
		return defaultServerPort, true, nil
	}

	portVal, exists := serverExt["port"]
	if !exists {
		return defaultServerPort, true, nil
	}

	var port int
	switch v := portVal.(type) {
	case int:
		port = v
	case float64:
		if v != float64(int(v)) {
			return 0, false, fmt.Errorf("invalid server.port value %v: must be an integer", v)
		}
		port = int(v)
	default:
		return 0, false, fmt.Errorf("invalid server.port value: unsupported type %T, must be an integer", portVal)
	}

	if port < 0 || port > 65535 {
		return 0, false, fmt.Errorf("invalid server.port value %d: must be between 0 and 65535", port)
	}
	return port, port != 0, nil
}

// resolveServerHost determines the effective HTTP server bind address
// from the CLI flag and workflow extensions. Returns an error if the
// resolved value is not a parseable IP address. When no host is
// configured, the loopback address is returned.
func resolveServerHost(hostFlag string, hostFlagSet bool, extensions map[string]any) (string, error) {
	if hostFlagSet {
		if net.ParseIP(hostFlag) == nil {
			return "", fmt.Errorf("invalid --host value %q: not a valid IP address", hostFlag)
		}
		return hostFlag, nil
	}

	serverExt, ok := extensions["server"].(map[string]any)
	if !ok {
		return defaultServerHost, nil
	}

	hostVal, exists := serverExt["host"]
	if !exists {
		return defaultServerHost, nil
	}

	hostStr, ok := hostVal.(string)
	if !ok {
		return "", fmt.Errorf("invalid server.host value: unsupported type %T, must be a string", hostVal)
	}

	if net.ParseIP(hostStr) == nil {
		return "", fmt.Errorf("invalid server.host value %q: not a valid IP address", hostStr)
	}
	return hostStr, nil
}

// hasServerPortExtension reports whether the extensions map contains a
// server object with a port key. The check is presence-based: even
// server.port matching the default counts as explicit configuration.
func hasServerPortExtension(extensions map[string]any) bool {
	serverExt, ok := extensions["server"].(map[string]any)
	if !ok {
		return false
	}
	_, exists := serverExt["port"]
	return exists
}

// resolveLogLevel determines the effective log level from the CLI flag
// and workflow extensions. Precedence: CLI flag > logging.level
// extension > default (info).
func resolveLogLevel(flagValue string, flagSet bool, extensions map[string]any) (slog.Level, error) {
	if flagSet {
		return logging.ParseLevel(flagValue)
	}

	loggingExt, ok := extensions["logging"].(map[string]any)
	if !ok {
		return slog.LevelInfo, nil
	}

	rawLevel, ok := loggingExt["level"]
	if !ok || rawLevel == nil {
		return slog.LevelInfo, nil
	}

	levelStr, ok := rawLevel.(string)
	if !ok {
		return 0, fmt.Errorf("invalid logging.level: expected string, got %T", rawLevel)
	}

	return logging.ParseLevel(levelStr)
}

// resolveLogFormat determines the effective log output format from the CLI
// flag and workflow extensions. Precedence: CLI flag > logging.format
// extension > default (text). All map and type accesses use the comma-ok
// idiom to avoid panics on unexpected extension shapes.
func resolveLogFormat(flagValue string, flagSet bool, extensions map[string]any) (logging.Format, error) {
	if flagSet {
		return logging.ParseFormat(flagValue)
	}

	loggingRaw, ok := extensions["logging"]
	if !ok {
		return logging.FormatText, nil
	}
	loggingExt, ok := loggingRaw.(map[string]any)
	if !ok {
		return logging.FormatText, nil
	}

	formatRaw, ok := loggingExt["format"]
	if !ok {
		return logging.FormatText, nil
	}

	formatStr, ok := formatRaw.(string)
	if !ok {
		return "", fmt.Errorf("invalid logging.format: expected string, got %T", formatRaw)
	}

	return logging.ParseFormat(formatStr)
}

// --- Dry-run mode ---

// runDryRun executes a single poll cycle in read-only mode: fetches
// candidate issues, computes dispatch eligibility, logs results, and
// returns an exit code. No database is opened, no agents are spawned,
// and no state is written. The caller constructs and defers closing
// the tracker adapter.
func runDryRun(ctx context.Context, cfg config.ServiceConfig, logger *slog.Logger, trackerAdapter domain.TrackerAdapter) int {
	issues, err := trackerAdapter.FetchCandidateIssues(ctx)
	if err != nil {
		logger.Error("dry-run: failed to fetch candidate issues", slog.Any("error", err))
		return 1
	}

	sorted := orchestrator.SortForDispatch(issues)

	state := orchestrator.NewState(
		cfg.Polling.IntervalMS,
		cfg.Agent.MaxConcurrentAgents,
		cfg.Agent.MaxConcurrentByState,
		orchestrator.AgentTotals{},
	)

	wc := orchestrator.ParseWorkerConfig(cfg.Extensions)
	hostPool := orchestrator.NewHostPool(wc.SSHHosts, wc.MaxPerHost)

	activeSet := dryRunStateSet(cfg.Tracker.ActiveStates)
	terminalSet := dryRunStateSet(cfg.Tracker.TerminalStates)

	var eligible, ineligible int
	for i, issue := range sorted {
		globalAvail := orchestrator.GlobalAvailableSlots(
			state.MaxConcurrentAgents, len(state.Running))

		if hostPool.IsSSHEnabled() && !hostPool.HasCapacity() {
			for _, remaining := range sorted[i:] {
				ineligible++
				logger.Info("dry-run: candidate",
					slog.String("issue_id", remaining.ID),
					slog.String("issue_identifier", remaining.Identifier),
					slog.String("state", remaining.State),
					slog.Bool("would_dispatch", false),
					slog.String("skip_reason", "ssh_hosts_at_capacity"),
				)
			}
			break
		}

		stateRunning := orchestrator.RunningCountByState(state.Running, issue.State)
		stateAvail := orchestrator.StateAvailableSlots(
			issue.State, state.MaxConcurrentByState, stateRunning, globalAvail)

		wouldDispatch := orchestrator.ShouldDispatchWithSets(
			issue, state, activeSet, terminalSet) && globalAvail > 0 && stateAvail > 0

		if wouldDispatch && hostPool.IsSSHEnabled() {
			_, ok := hostPool.AcquireHost(issue.ID, "")
			if !ok {
				wouldDispatch = false
			}
		}

		logFields := []any{
			slog.String("issue_id", issue.ID),
			slog.String("issue_identifier", issue.Identifier),
			slog.String("title", issue.Title),
			slog.String("state", issue.State),
			slog.Bool("would_dispatch", wouldDispatch),
			slog.Int("global_slots_available", globalAvail),
			slog.Int("state_slots_available", stateAvail),
		}
		if issue.Priority != nil {
			logFields = append(logFields, slog.Int("priority", *issue.Priority))
		}
		if hostPool.IsSSHEnabled() {
			logFields = append(logFields, slog.String("ssh_host", hostPool.HostFor(issue.ID)))
		}

		logger.Info("dry-run: candidate", logFields...)

		if wouldDispatch {
			eligible++
			state.Claimed[issue.ID] = struct{}{}
			state.Running[issue.ID] = &orchestrator.RunningEntry{
				Identifier: issue.Identifier,
				Issue:      issue,
			}
		} else {
			ineligible++
		}
	}

	logger.Info("dry-run: complete",
		slog.Int("candidates_fetched", len(issues)),
		slog.Int("would_dispatch", eligible),
		slog.Int("ineligible", ineligible),
		slog.Int("max_concurrent_agents", cfg.Agent.MaxConcurrentAgents),
	)

	return 0
}

// dryRunStateSet builds a set of lowercase state names for O(1) membership
// testing. Mirrors orchestrator.stateSet which is unexported.
func dryRunStateSet(states []string) map[string]struct{} {
	set := make(map[string]struct{}, len(states))
	for _, s := range states {
		set[strings.ToLower(s)] = struct{}{}
	}
	return set
}

// --- Validate subcommand ---

type validateOutput struct {
	Valid    bool           `json:"valid"`
	Errors   []validateDiag `json:"errors"`
	Warnings []validateDiag `json:"warnings"`
}

type validateDiag struct {
	Severity string `json:"severity"`
	Check    string `json:"check"`
	Message  string `json:"message"`
}

func runValidate(_ context.Context, args []string, stdout io.Writer, stderr io.Writer) int {
	if containsHelpFlag(args) {
		printValidateHelp(stdout)
		return 0
	}

	fs := flag.NewFlagSet("sortie validate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	format := fs.String("format", "text", `Output format: "text" or "json"`)

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printValidateHelp(stdout)
			return 0
		}
		emitDiags(stdout, stderr, *format, []validateDiag{{Severity: "error", Check: "args", Message: err.Error()}}, nil)
		return 1
	}

	if *format != "text" && *format != "json" {
		fmt.Fprintf(stderr, "sortie validate: invalid --format value %q: must be \"text\" or \"json\"\n", *format) //nolint:errcheck // stderr write failure is unrecoverable
		return 1
	}

	path, err := resolveWorkflowPath(fs.Args())
	if err != nil {
		emitDiags(stdout, stderr, *format, []validateDiag{{Severity: "error", Check: "args", Message: err.Error()}}, nil)
		return 1
	}

	// Load raw workflow for schema analysis (owned by this goroutine).
	wf, err := workflow.Load(path)
	if err != nil {
		emitDiags(stdout, stderr, *format, mapManagerError(err), nil)
		return 1
	}

	cfg, err := config.NewServiceConfig(wf.Config)
	if err != nil {
		emitDiags(stdout, stderr, *format, mapManagerError(err), nil)
		return 1
	}

	// wf.Config is the post-env-override raw map. Sole ownership — safe to read.
	var warningDiags []validateDiag
	for _, w := range config.ValidateFrontMatter(wf.Config, cfg) {
		msg := w.Message
		if w.Field != "" {
			msg = w.Field + ": " + msg
		}
		warningDiags = append(warningDiags, validateDiag{
			Severity: "warning",
			Check:    w.Check,
			Message:  msg,
		})
	}

	// Template static analysis.
	tmpl, parseErr := prompt.Parse(wf.PromptTemplate, path, wf.FrontMatterLines)
	if parseErr == nil {
		for _, w := range prompt.AnalyzeTemplate(tmpl) {
			warningDiags = append(warningDiags, validateDiag{
				Severity: "warning",
				Check:    templateWarnCheck(w.Kind),
				Message:  w.Message,
			})
		}
	}
	// parseErr is not emitted here — NewManager below will re-parse
	// and produce the same error through the existing error path.
	// TODO: accept a pre-parsed template in NewManager to avoid the
	// double-parse; negligible overhead for validate but worth
	// cleaning up in a future refactor.

	logger := slog.New(slog.DiscardHandler)

	mgr, err := workflow.NewManager(path, logger,
		workflow.WithValidateFunc(orchestrator.ValidateConfigForPromotion))
	if err != nil {
		emitDiags(stdout, stderr, *format, mapManagerError(err), warningDiags)
		return 1
	}

	preflightParams := orchestrator.PreflightParams{
		ReloadWorkflow:  mgr.Reload,
		ConfigFunc:      mgr.Config,
		TrackerRegistry: registry.Trackers,
		AgentRegistry:   registry.Agents,
	}

	validation := orchestrator.ValidateDispatchConfig(preflightParams)

	for _, w := range validation.Warnings {
		warningDiags = append(warningDiags, validateDiag{
			Severity: "warning",
			Check:    w.Check,
			Message:  w.Message,
		})
	}

	if !validation.OK() {
		emitDiags(stdout, stderr, *format, mapPreflightErrors(validation.Errors), warningDiags)
		return 1
	}

	// Success path: emit warnings (if any) with valid=true.
	emitDiags(stdout, stderr, *format, nil, warningDiags)
	return 0
}

func emitDiags(stdout io.Writer, stderr io.Writer, format string, errs []validateDiag, warnings []validateDiag) {
	if errs == nil {
		errs = []validateDiag{}
	}
	if warnings == nil {
		warnings = []validateDiag{}
	}
	if format == "json" {
		out := validateOutput{
			Valid:    len(errs) == 0,
			Errors:   errs,
			Warnings: warnings,
		}
		if err := writeJSON(stdout, out); err != nil {
			for _, d := range errs {
				fmt.Fprintf(stderr, "error: %s: %s\n", d.Check, d.Message) //nolint:errcheck // stderr write failure is unrecoverable
			}
			for _, d := range warnings {
				fmt.Fprintf(stderr, "warning: %s: %s\n", d.Check, d.Message) //nolint:errcheck // stderr write failure is unrecoverable
			}
		}
		return
	}
	for _, d := range errs {
		fmt.Fprintf(stderr, "error: %s: %s\n", d.Check, d.Message) //nolint:errcheck // stderr write failure is unrecoverable
	}
	for _, d := range warnings {
		fmt.Fprintf(stderr, "warning: %s: %s\n", d.Check, d.Message) //nolint:errcheck // stderr write failure is unrecoverable
	}
}

func writeJSON(w io.Writer, v any) error {
	return json.NewEncoder(w).Encode(v)
}

func mapManagerError(err error) []validateDiag {
	var we *workflow.WorkflowError
	if errors.As(err, &we) {
		check := "workflow_load"
		if we.Kind == workflow.ErrFrontMatterNotMap {
			check = "workflow_front_matter"
		}
		return []validateDiag{{Severity: "error", Check: check, Message: err.Error()}}
	}

	var ce *config.ConfigError
	if errors.As(err, &ce) {
		return []validateDiag{{Severity: "error", Check: "config." + ce.Field, Message: ce.Message}}
	}

	var te *prompt.TemplateError
	if errors.As(err, &te) {
		return []validateDiag{{Severity: "error", Check: "template_parse", Message: err.Error()}}
	}

	return []validateDiag{{Severity: "error", Check: "workflow_load", Message: err.Error()}}
}

func mapPreflightErrors(errs []orchestrator.PreflightError) []validateDiag {
	diags := make([]validateDiag, len(errs))
	for i, e := range errs {
		diags[i] = validateDiag{Severity: "error", Check: e.Check, Message: e.Message}
	}
	return diags
}

func templateWarnCheck(k prompt.WarnKind) string {
	switch k {
	case prompt.WarnDotContext:
		return "dot_context"
	case prompt.WarnUnknownVar:
		return "unknown_var"
	case prompt.WarnUnknownField:
		return "unknown_field"
	default:
		return "template_warning"
	}
}
