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

	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/persistence"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/tool/history"
	"github.com/sortie-ai/sortie/internal/tool/mcpserver"
	"github.com/sortie-ai/sortie/internal/tool/status"
	"github.com/sortie-ai/sortie/internal/tool/trackerapi"
	"github.com/sortie-ai/sortie/internal/workflow"
)

func runMCPServer(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) int {
	fs := flag.NewFlagSet("sortie mcp-server", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	workflowFlag := fs.String("workflow", "", "Absolute path to the WORKFLOW.md file (required)")

	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: sortie mcp-server --workflow <path>")                             //nolint:errcheck // stderr write failure is unrecoverable
		fmt.Fprintln(stderr)                                                                           //nolint:errcheck // stderr write failure is unrecoverable
		fmt.Fprintln(stderr, "Start an MCP stdio server that exposes registered agent tools over")     //nolint:errcheck // stderr write failure is unrecoverable
		fmt.Fprintln(stderr, "JSON-RPC on stdin/stdout. Intended to be launched by an MCP-compatible") //nolint:errcheck // stderr write failure is unrecoverable
		fmt.Fprintln(stderr, "agent runtime via the generated MCP config, not run manually.")          //nolint:errcheck // stderr write failure is unrecoverable
		fmt.Fprintln(stderr)                                                                           //nolint:errcheck // stderr write failure is unrecoverable
		fmt.Fprintln(stderr, "Flags:")                                                                 //nolint:errcheck // stderr write failure is unrecoverable
		fmt.Fprintln(stderr, "  --workflow    Absolute path to the WORKFLOW.md file (required)")       //nolint:errcheck // stderr write failure is unrecoverable
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
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
			}
		}
	}

	srv := mcpserver.NewServer(toolRegistry, os.Stdin, stdout, logger, Version)
	if err := srv.Serve(ctx); err != nil {
		logger.Error("MCP server error", slog.Any("error", err))
		return 1
	}

	return 0
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
