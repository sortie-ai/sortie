// Package main is the entry point for the Sortie orchestration service.
// The binary accepts an optional positional workflow file path (default
// ./WORKFLOW.md) and a --port flag for the HTTP observability server.
// Start with [run] for the complete startup and shutdown lifecycle.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/orchestrator"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/workflow"
	"github.com/sortie-ai/sortie/internal/workspace"

	// Import adapter packages for their init-time registrations.
	_ "github.com/sortie-ai/sortie/internal/agent/claude"
	_ "github.com/sortie-ai/sortie/internal/agent/mock"
	_ "github.com/sortie-ai/sortie/internal/tracker/file"
	_ "github.com/sortie-ai/sortie/internal/tracker/jira"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) int {
	fs := flag.NewFlagSet("sortie", flag.ContinueOnError)
	fs.SetOutput(stderr)
	port := fs.Int("port", 0, "HTTP server port")
	showVersion := fs.Bool("version", false, "Print program's version information and quit")
	dumpVersion := fs.Bool("dumpversion", false, "Print the version of the program and don't do anything else")

	// Single-dash flags are a deliberate convention for -dumpversion (GCC
	// style). All other flags use double-dash in help text. The stdlib
	// flag package accepts both forms regardless of how they are displayed.
	singleDashFlags := map[string]bool{"dumpversion": true}
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: sortie [flags] [workflow-path]\n\nFlags:\n") //nolint:errcheck // stderr write failure is unrecoverable
		fs.VisitAll(func(f *flag.Flag) {
			prefix := "--"
			if singleDashFlags[f.Name] {
				prefix = "-"
			}
			fmt.Fprintf(fs.Output(), "  %s%s\t%s\n", prefix, f.Name, f.Usage) //nolint:errcheck // stderr write failure is unrecoverable
		})
	}

	if err := fs.Parse(args); err != nil {
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

	var portSet bool
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "port" {
			portSet = true
		}
	})

	logging.Setup(stderr, slog.LevelInfo)
	logger := slog.Default()

	logAttrs := []any{
		slog.String("version", Version),
		slog.String("workflow_path", path),
	}
	if portSet {
		logAttrs = append(logAttrs, slog.Int("port", *port))
	}
	logger.Info("sortie starting", logAttrs...)

	mgr, err := workflow.NewManager(path, logger)
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
			InputTokens:    metrics.InputTokens,
			OutputTokens:   metrics.OutputTokens,
			TotalTokens:    metrics.TotalTokens,
			SecondsRunning: metrics.SecondsRunning,
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

	// --- Adapter construction from registry ---

	trackerCtor, err := registry.Trackers.Get(cfg.Tracker.Kind)
	if err != nil {
		logger.Error("unknown tracker kind", slog.String("kind", cfg.Tracker.Kind), slog.Any("error", err))
		return 1
	}
	trackerCfgMap := trackerConfigMap(cfg.Tracker)
	mergeExtensions(trackerCfgMap, cfg.Extensions, cfg.Tracker.Kind)
	trackerAdapter, err := trackerCtor(trackerCfgMap)
	if err != nil {
		logger.Error("failed to construct tracker adapter", slog.Any("error", err))
		return 1
	}

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

	o := orchestrator.NewOrchestrator(orchestrator.OrchestratorParams{
		State:           state,
		Logger:          logger,
		TrackerAdapter:  trackerAdapter,
		AgentAdapter:    agentAdapter,
		WorkflowManager: mgr,
		Store:           store,
		PreflightParams: preflightParams,
	})

	o.Run(ctx)

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
		"kind":            tc.Kind,
		"endpoint":        tc.Endpoint,
		"api_key":         tc.APIKey,
		"project":         tc.Project,
		"active_states":   tc.ActiveStates,
		"terminal_states": tc.TerminalStates,
		"query_filter":    tc.QueryFilter,
	}
}

// agentConfigMap converts typed [config.AgentConfig] fields into the
// raw map expected by [registry.AgentConstructor]. Adapter packages
// extract their required fields from this map.
func agentConfigMap(ac config.AgentConfig) map[string]any {
	return map[string]any{
		"kind":                           ac.Kind,
		"command":                        ac.Command,
		"turn_timeout_ms":                ac.TurnTimeoutMS,
		"read_timeout_ms":                ac.ReadTimeoutMS,
		"stall_timeout_ms":               ac.StallTimeoutMS,
		"max_concurrent_agents":          ac.MaxConcurrentAgents,
		"max_turns":                      ac.MaxTurns,
		"max_retry_backoff_ms":           ac.MaxRetryBackoffMS,
		"max_concurrent_agents_by_state": ac.MaxConcurrentByState,
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
