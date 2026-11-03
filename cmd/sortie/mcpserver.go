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
	"os"
	"path/filepath"
	"strconv"

	"github.com/sortie-ai/sortie/internal/agent/mcpconfig"
	"github.com/sortie-ai/sortie/internal/config"
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/notify/route"
	"github.com/sortie-ai/sortie/internal/pricing"
	"github.com/sortie-ai/sortie/internal/redact"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/storage"
	"github.com/sortie-ai/sortie/internal/tool/budget"
	"github.com/sortie-ai/sortie/internal/tool/history"
	"github.com/sortie-ai/sortie/internal/tool/mcpserver"
	"github.com/sortie-ai/sortie/internal/tool/notify"
	"github.com/sortie-ai/sortie/internal/workflow"
)

// defaultMaxPerSession is the notification cap selected when no backend
// declares a non-zero max_per_session. 0 in config selects this default;
// it never means unlimited.
const defaultMaxPerSession = 20

func runMCPServer(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) int {
	if containsHelpFlag(args) {
		printMCPServerHelp(stdout)
		return 0
	}

	fs := flag.NewFlagSet("sortie mcp-server", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	workflowFlag := fs.String("workflow", "", "Absolute path to the WORKFLOW.md file (required)")
	httpFlag := fs.Bool("http", false, "Serve MCP over streamable HTTP on a loopback port instead of stdio")

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

	bearer := os.Getenv(mcpconfig.BearerEnv)
	if *httpFlag && bearer == "" {
		fmt.Fprintf(stderr, "sortie mcp-server: --http requires the %s environment variable to be non-empty\n", mcpconfig.BearerEnv) //nolint:errcheck // stderr write failure is unrecoverable
		return 1
	}

	var ln net.Listener
	if *httpFlag {
		var listenErr error
		ln, listenErr = listenMCPHTTP(ctx, stdout)
		if listenErr != nil {
			fmt.Fprintf(stderr, "sortie mcp-server: %s\n", listenErr) //nolint:errcheck // stderr write failure is unrecoverable
			return 1
		}
		defer ln.Close() //nolint:errcheck // ServeStreamableHTTP closes it first; this covers the early exits
	}

	logger := logging.Setup(stderr, slog.LevelInfo, logging.FormatText)
	redact.AddEnviron(os.Environ())
	if bearer != "" {
		redact.Add("tool server bearer", bearer)
	}

	wf, err := workflow.Load(*workflowFlag)
	if err != nil {
		logger.Error("failed to load workflow", slog.Any("error", err))
		return 1
	}

	cfg, err := config.NewServiceConfig(wf.Config, config.WithLiteralSettings(registry.LiteralSettingsOf))
	if err != nil {
		logger.Error("failed to parse config", slog.Any("error", err))
		return 1
	}

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
		config.MergeAdapterExtensions(trackerCfgMap, cfg, cfg.Tracker.Kind)

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

	// A notifier misconfiguration is fatal; a read-only DB-open failure is
	// non-fatal and skips the two database-backed tools.
	sessionTools, err := BuildSessionToolRegistry(ctx, logger, sessionToolParamsFromEnv(os.Getenv, cfg, trackerAdapter))
	if err != nil {
		logger.Error("failed to build session tool registry", slog.Any("error", err))
		return 1
	}
	if sessionTools.Store != nil {
		defer sessionTools.Store.Close() //nolint:errcheck // best-effort cleanup at shutdown
	}

	srv := mcpserver.NewServer(sessionTools.Registry, logger, Version)
	if ln != nil {
		if err := serveMCPHTTP(ctx, srv, ln, bearer, os.Stdin, stdout); err != nil {
			logger.Error("MCP server error", slog.Any("error", err))
			return 1
		}
		return 0
	}
	if err := srv.Serve(ctx, os.Stdin, writerOnly{stdout}); err != nil {
		logger.Error("MCP server error", slog.Any("error", err))
		return 1
	}

	return 0
}

// writerOnly keeps the transport from closing the caller's writer.
type writerOnly struct{ io.Writer }

func (writerOnly) Close() error { return nil }

// listenMCPHTTP announces the port before the workflow loads, so the spawning
// process need not wait for startup work.
func listenMCPHTTP(ctx context.Context, stdout io.Writer) (net.Listener, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen on loopback: %w", err)
	}
	line, err := json.Marshal(mcpserver.HTTPEvent{Listening: ln.Addr().String()})
	if err != nil {
		ln.Close() //nolint:errcheck,gosec // best-effort; the marshal error is what the caller needs
		return nil, fmt.Errorf("encode listening event: %w", err)
	}
	if _, err := stdout.Write(append(line, '\n')); err != nil {
		ln.Close() //nolint:errcheck,gosec // best-effort; the write error is what the caller needs
		return nil, fmt.Errorf("write listening event: %w", err)
	}
	return ln, nil
}

// serveMCPHTTP serves until ctx ends or stdin closes, which is how the
// spawning process ends the server's life.
func serveMCPHTTP(ctx context.Context, srv *mcpserver.Server, ln net.Listener, bearer string, stdin io.Reader, stdout io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// The copy may stay blocked in a read nothing can interrupt; the process
	// exits right after.
	go func() {
		io.Copy(io.Discard, stdin) //nolint:errcheck,gosec // any read error ends the server the same way as end of file
		cancel()
	}()

	return srv.ServeStreamableHTTP(ctx, ln, mcpserver.HTTPOptions{Bearer: bearer, Events: stdout})
}

// sessionToolParamsFromEnv builds the per-session tool registry inputs
// from the sidecar's process environment via getenv. SORTIE_ATTEMPT maps
// to Attempt when it parses as an integer; otherwise Attempt stays nil.
func sessionToolParamsFromEnv(getenv func(string) string, cfg config.ServiceConfig, trackerAdapter domain.TrackerAdapter) SessionToolParams {
	var attempt *int
	if raw := getenv("SORTIE_ATTEMPT"); raw != "" {
		if n, atoiErr := strconv.Atoi(raw); atoiErr == nil {
			attempt = &n
		}
	}
	return SessionToolParams{
		TrackerAdapter:        trackerAdapter,
		Project:               cfg.Tracker.Project,
		WorkspacePath:         getenv("SORTIE_WORKSPACE"),
		DBPath:                getenv("SORTIE_DB_PATH"),
		IssueID:               getenv("SORTIE_ISSUE_ID"),
		Identifier:            getenv("SORTIE_ISSUE_IDENTIFIER"),
		DispatchID:            getenv("SORTIE_DISPATCH_ID"),
		Attempt:               attempt,
		AgentKind:             getenv("SORTIE_SESSION_AGENT_KIND"),
		MaxTokens:             cfg.Agent.MaxTokens,
		MaxCostUSD:            cfg.Agent.MaxCostUSD,
		MaxSessions:           cfg.Agent.MaxSessions,
		TokenWarningThreshold: cfg.Agent.TokenWarningThreshold(),
		Notifications:         cfg.Notifications.Backends,
	}
}

// buildNotifyTool resolves the configured notifier backends that receive
// agent.message into the notify_operator tool, returning (nil, nil) when
// none does. An
// unknown kind or constructor error (including a required secret that
// resolved empty) is fatal and returned as a non-nil error rather than a
// partial registration.
func buildNotifyTool(configured []config.NotificationBackend, env notify.NotificationEnvelopeContext, sessionID notify.SessionIDFunc, reserveSlot notify.SlotReserver) (domain.AgentTool, error) {
	configured = route.AgentMessageEntries(configured)
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

	return notify.New(backends, env, sessionID, resolveNotificationCap(configured), reserveSlot), nil
}

// resolveNotificationCap returns the maximum non-zero max_per_session
// across the backends, falling back to defaultMaxPerSession when every
// entry is 0. The cap counts notify_operator calls, not per-backend
// sends.
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

// budgetSource is what the budget query reads: the run history and the
// running session's stored usage.
type budgetSource interface {
	storage.RunHistoryStore
	storage.SessionMetadataStore
}

func buildBudgetQuery(store budgetSource) budget.BudgetQueryFunc {
	return func(ctx context.Context, issueID string, runningDispatchID string) (budget.BudgetUsage, error) {
		completed, err := store.TokenUsageByIssue(ctx, issueID)
		if err != nil {
			return budget.BudgetUsage{}, err
		}

		usage := budget.BudgetUsage{
			CompletedUncachedTokens: completed.UncachedTokens,
			CompletedSessions:       completed.Sessions,
			UnmeasuredSessions:      completed.UnmeasuredSessions,
			UnaccountedTurns:        completed.UnaccountedTurns,
		}

		// Add the running session's spend only when its stored dispatch ID
		// matches the live one, so neither a stale dispatch's row nor a
		// session-exit cleared row is double counted.
		var session pricing.SessionCost
		if runningDispatchID != "" {
			meta, found, err := store.LoadSessionMetadata(ctx, issueID)
			if err != nil {
				return budget.BudgetUsage{}, err
			}
			if found && meta.DispatchID == runningDispatchID {
				usage.RunningUncachedTokens = domain.TokenUsage{TotalTokens: meta.TotalTokens, CacheReadTokens: meta.CacheReadTokens}.Uncached()
				usage.RunningMeasured = true
				usage.UnaccountedTurns += meta.UnaccountedTurns
				session = sessionCostOf(meta)
			}
		}

		spendComplete := usage.UnmeasuredSessions == 0 && usage.UnaccountedTurns == 0 &&
			runningDispatchID != "" && usage.RunningMeasured
		var status pricing.CostStatus
		usage.UsedUSD, status = pricing.IssueCost(recordedOf(completed), session, spendComplete)
		usage.UsedUSDComplete = status == pricing.CostEstimated
		return usage, nil
	}
}

// recordedOf maps the cost aggregates of an issue's usage read onto the input
// of [pricing.IssueCost].
func recordedOf(usage storage.IssueTokenUsage) pricing.RecordedCost {
	return pricing.RecordedCost{
		USD:      usage.RecordedCostUSD,
		Priced:   usage.PricedSessions,
		Partial:  usage.PartialSessions,
		Unpriced: usage.UnpricedSessions,
	}
}

// sessionCostOf reads the running session's stored estimate; a row recorded
// without a status reads as unmeasured.
func sessionCostOf(meta storage.SessionMetadata) pricing.SessionCost {
	status := pricing.CostStatus(meta.CostStatus)
	if status == "" {
		status = pricing.CostUnmeasured
	}
	return pricing.SessionCost{
		USD:    meta.EstimatedCostUSD,
		Status: status,
		Spent: meta.InputTokens != 0 || meta.OutputTokens != 0 || meta.TotalTokens != 0 ||
			meta.CacheReadTokens != 0 || meta.CacheWriteTokens != 0,
	}
}

func buildHistoryQuery(store storage.RunHistoryStore) history.QueryFunc {
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
