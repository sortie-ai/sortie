package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/tool/budget"
	"github.com/sortie-ai/sortie/internal/tool/history"
	"github.com/sortie-ai/sortie/internal/tool/mcpserver"
	"github.com/sortie-ai/sortie/internal/tool/notify"
	"github.com/sortie-ai/sortie/internal/tool/status"
	"github.com/sortie-ai/sortie/internal/tool/trackerapi"
	"github.com/sortie-ai/sortie/internal/workflow"
)

// defaultMaxPerSession is the per-session notification cap selected when
// no backend declares a non-zero max_per_session. 0 in config selects
// this default; it never means unlimited.
const defaultMaxPerSession = 20

func runMCPServer(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) int {
	if containsHelpFlag(args) {
		printMCPServerHelp(stdout)
		return 0
	}

	fs := flag.NewFlagSet("sortie mcp-server", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	workflowFlag := fs.String("workflow", "", "Absolute path to the WORKFLOW.md file (required)")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printMCPServerHelp(stdout)
			return 0
		}
		fmt.Fprintf(stderr, "sortie mcp-server: %s\n", err) //nolint:errcheck // stderr write failure is unrecoverable
		return 1
	}

	if *workflowFlag == "" {
		fmt.Fprintln(stderr, "sortie mcp-server: --workflow flag is required") //nolint:errcheck // stderr write failure is unrecoverable
		return 1
	}

	if !filepath.IsAbs(*workflowFlag) {
		fmt.Fprintln(stderr, "sortie mcp-server: --workflow must be an absolute path") //nolint:errcheck // stderr write failure is unrecoverable
		return 1
	}

	logger := logging.Setup(stderr, slog.LevelInfo, logging.FormatText)

	wf, err := workflow.Load(*workflowFlag)
	if err != nil {
		logger.Error("failed to load workflow", slog.Any("error", err))
		return 1
	}

	cfg, err := config.NewServiceConfig(wf.Config)
	if err != nil {
		logger.Error("failed to parse config", slog.Any("error", err))
		return 1
	}

	// Construct tracker adapter if the tracker section is present.
	var trackerAdapter domain.TrackerAdapter
	if cfg.Tracker.Kind != "" {
		trackerCtor, trackerErr := registry.Trackers.Get(cfg.Tracker.Kind)
		if trackerErr != nil {
			logger.Error("unknown tracker kind",
				slog.String("kind", cfg.Tracker.Kind),
				slog.Any("error", trackerErr),
			)
			return 1
		}

		trackerCfgMap := trackerConfigMap(cfg.Tracker)
		trackerCfgMap["user_agent"] = "sortie-mcp/" + Version
		mergeExtensions(trackerCfgMap, cfg.Extensions, cfg.Tracker.Kind)

		adapter, adapterErr := trackerCtor(trackerCfgMap)
		if adapterErr != nil {
			logger.Error("failed to construct tracker adapter", slog.Any("error", adapterErr))
			return 1
		}
		if closer, ok := adapter.(io.Closer); ok {
			defer closer.Close() //nolint:errcheck // best-effort cleanup at shutdown
		}
		trackerAdapter = adapter
	}

	// Build tool registry.
	toolRegistry := domain.NewToolRegistry()
	if trackerAdapter != nil && cfg.Tracker.Project != "" {
		toolRegistry.Register(trackerapi.New(trackerAdapter, cfg.Tracker.Project))
	}
	if workspacePath := os.Getenv("SORTIE_WORKSPACE"); workspacePath != "" {
		toolRegistry.Register(status.New(workspacePath))
	}
	if dbPath := os.Getenv("SORTIE_DB_PATH"); dbPath != "" {
		if issueID := os.Getenv("SORTIE_ISSUE_ID"); issueID != "" {
			store, storeErr := persistence.OpenReadOnly(ctx, dbPath)
			if storeErr != nil {
				logger.Warn("failed to open read-only db for workspace_history",
					slog.String("db_path", dbPath),
					slog.Any("error", storeErr))
			} else {
				defer store.Close() //nolint:errcheck // best-effort cleanup at shutdown
				toolRegistry.Register(history.New(buildHistoryQuery(store), issueID))
				runningSessionID := os.Getenv("SORTIE_SESSION_ID")
				toolRegistry.Register(budget.New(buildBudgetQuery(store), issueID, runningSessionID, cfg.Agent.MaxTokens, cfg.Agent.MaxSessions))
			}
		}
	}

	// Register notify_operator only when at least one backend is
	// configured. An invalid backend is a fatal startup error, never a
	// partial registration, so the sidecar and main process agree on the
	// advertised tool set.
	notifyTool, notifyErr := buildNotifyTool(cfg.Notifications.Backends)
	if notifyErr != nil {
		logger.Error("failed to configure notify_operator", slog.Any("error", notifyErr))
		return 1
	}
	if notifyTool != nil {
		toolRegistry.Register(notifyTool)
	}

	srv := mcpserver.NewServer(toolRegistry, os.Stdin, stdout, logger, Version)
	if err := srv.Serve(ctx); err != nil {
		logger.Error("MCP server error", slog.Any("error", err))
		return 1
	}

	return 0
}

// buildNotifyTool resolves the configured notifier backends and returns
// the notify_operator tool. It returns (nil, nil) when no backend is
// configured, so the caller skips registration. An unknown kind or a
// constructor error (including a required secret that resolved to the
// empty string) is fatal and returned as a non-nil error rather than a
// partial registration. The envelope context is read from the sidecar
// environment.
func buildNotifyTool(configured []config.NotificationBackend) (domain.AgentTool, error) {
	if len(configured) == 0 {
		return nil, nil
	}

	backends := make([]domain.Notifier, 0, len(configured))
	for _, entry := range configured {
		ctor, err := registry.Notifiers.Get(entry.Kind)
		if err != nil {
			return nil, err
		}
		notifier, err := ctor(entry.Config)
		if err != nil {
			return nil, fmt.Errorf("notifier %q: %w", entry.Kind, err)
		}
		backends = append(backends, notifier)
	}

	var attempt *int
	if raw := os.Getenv("SORTIE_ATTEMPT"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			attempt = &n
		}
	}
	envContext := notify.NotificationEnvelopeContext{
		IssueID:    os.Getenv("SORTIE_ISSUE_ID"),
		Identifier: os.Getenv("SORTIE_ISSUE_IDENTIFIER"),
		SessionID:  os.Getenv("SORTIE_SESSION_ID"),
		Attempt:    attempt,
		Agent:      os.Getenv("SORTIE_SESSION_AGENT_KIND"),
	}

	return notify.New(backends, envContext, resolveNotificationCap(configured)), nil
}

// resolveNotificationCap selects the single per-session cap for the tool
// from the configured backends. It returns the maximum non-zero
// max_per_session across entries and falls back to defaultMaxPerSession
// when every entry is 0 or unset. The cap counts notify_operator calls,
// not per-backend sends, so it is a tool-level property.
func resolveNotificationCap(backends []config.NotificationBackend) int {
	maxCap := 0
	for _, b := range backends {
		if b.MaxPerSession > maxCap {
			maxCap = b.MaxPerSession
		}
	}
	if maxCap == 0 {
		return defaultMaxPerSession
	}
	return maxCap
}

func buildBudgetQuery(store *persistence.Store) budget.BudgetQueryFunc {
	return func(ctx context.Context, issueID string, runningSessionID string) (budget.BudgetUsage, error) {
		completedTotal, completedSessions, err := store.SumTotalTokensByIssue(ctx, issueID)
		if err != nil {
			return budget.BudgetUsage{}, err
		}

		usage := budget.BudgetUsage{
			CompletedTotalTokens: completedTotal,
			CompletedSessions:    completedSessions,
		}

		// The running session's recorded spend lives in session_metadata,
		// which survives session exit. Add it only when the stored
		// session ID matches the live session ID supplied out of band, so
		// a stale earlier session's row is never double counted.
		if runningSessionID == "" {
			return usage, nil
		}
		meta, found, err := store.LoadSessionMetadata(ctx, issueID)
		if err != nil {
			return budget.BudgetUsage{}, err
		}
		if found && meta.SessionID == runningSessionID {
			usage.RunningTotalTokens = meta.TotalTokens
		}
		return usage, nil
	}
}

func buildHistoryQuery(store *persistence.Store) history.QueryFunc {
	return func(ctx context.Context, issueID string, limit int) ([]history.Entry, error) {
		rows, err := store.QueryRunHistoryByIssue(ctx, issueID)
		if err != nil {
			return nil, err
		}
		if limit > 0 && len(rows) > limit {
			rows = rows[:limit]
		}
		entries := make([]history.Entry, len(rows))
		for i, r := range rows {
			entries[i] = history.Entry{
				Attempt:      r.Attempt,
				AgentAdapter: r.AgentAdapter,
				StartedAt:    r.StartedAt,
				CompletedAt:  r.CompletedAt,
				Status:       r.Status,
				Error:        r.Error,
			}
		}
		return entries, nil
	}
}
