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
	"github.com/sortie-ai/sortie/internal/orchestrator"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/workflow"
)

const (
	defaultServerPort = 7678
	defaultServerHost = "127.0.0.1"
)

type bootParams struct {
	args   []string
	stdout io.Writer
	stderr io.Writer
}

type bootResult struct {
	logger          *slog.Logger
	cfg             config.ServiceConfig
	mgr             *workflow.Manager
	path            string
	preflightParams orchestrator.PreflightParams
	trackerAdapter  domain.TrackerAdapter
	serverPort      int
	serverHost      string
	serverEnabled   bool
	portIsImplicit  bool
	dryRun          bool
	effectiveLevel  slog.Level
	effectiveFormat logging.Format
}

func boot(ctx context.Context, p bootParams) (bootResult, int) {
	action := interceptShortFlags(p.args)
	if action == "help" {
		printHelp(p.stdout)
		return bootResult{}, 0
	}
	if action == "version" {
		fmt.Fprint(p.stdout, versionBanner()) //nolint:errcheck // stdout write failure is unrecoverable
		return bootResult{}, 0
	}

	if len(p.args) > 0 && p.args[0] == "validate" {
		return bootResult{}, runValidate(ctx, p.args[1:], p.stdout, p.stderr)
	}
	if len(p.args) > 0 && p.args[0] == "mcp-server" {
		return bootResult{}, runMCPServer(ctx, p.args[1:], p.stdout, p.stderr)
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

	if err := fs.Parse(p.args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printHelp(p.stdout)
			return bootResult{}, 0
		}
		fmt.Fprintf(p.stderr, "sortie: %s\n", err) //nolint:errcheck // stderr write failure is unrecoverable
		return bootResult{}, 1
	}

	if *dumpVersion {
		fmt.Fprintln(p.stdout, Version) //nolint:errcheck // stdout write failure is unrecoverable
		return bootResult{}, 0
	}
	if *showVersion {
		fmt.Fprint(p.stdout, versionBanner()) //nolint:errcheck // stdout write failure is unrecoverable
		return bootResult{}, 0
	}

	path, err := resolveWorkflowPath(fs.Args())
	if err != nil {
		fmt.Fprintf(p.stderr, "sortie: %s\n", err) //nolint:errcheck // stderr write failure is unrecoverable
		return bootResult{}, 1
	}

	if *envFile != "" {
		config.SetDotEnvPath(*envFile)
		if abs, err := filepath.Abs(*envFile); err == nil {
			if err := os.Setenv("SORTIE_ENV_FILE", abs); err != nil {
				fmt.Fprintf(p.stderr, "sortie: setting SORTIE_ENV_FILE: %s\n", err) //nolint:errcheck // stderr write failure is unrecoverable
				return bootResult{}, 1
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

	effectiveLevel := slog.LevelInfo
	if logLevelSet {
		lvl, err := logging.ParseLevel(*logLevel)
		if err != nil {
			fmt.Fprintf(p.stderr, "sortie: %s\n", err) //nolint:errcheck // stderr write failure is unrecoverable
			return bootResult{}, 1
		}
		effectiveLevel = lvl
	}
	effectiveFormat := logging.FormatText
	if logFormatSet {
		parsedFmt, err := logging.ParseFormat(*logFormat)
		if err != nil {
			fmt.Fprintf(p.stderr, "sortie: %s\n", err) //nolint:errcheck // stderr write failure is unrecoverable
			return bootResult{}, 1
		}
		effectiveFormat = parsedFmt
	}
	logger := logging.Setup(p.stderr, effectiveLevel, effectiveFormat)

	mgr, err := workflow.NewManager(path, logger,
		workflow.WithValidateFunc(orchestrator.ValidateConfigForPromotion))
	if err != nil {
		fmt.Fprintf(p.stderr, "sortie: %s\n", err) //nolint:errcheck // stderr write failure is unrecoverable
		return bootResult{}, 1
	}
	if err := mgr.Start(ctx); err != nil {
		fmt.Fprintf(p.stderr, "sortie: %s\n", err) //nolint:errcheck // stderr write failure is unrecoverable
		return bootResult{}, 1
	}
	mgrStarted := true
	defer func() {
		if mgrStarted {
			mgr.Stop()
		}
	}()

	preflightParams := orchestrator.PreflightParams{
		ReloadWorkflow:  mgr.Reload,
		ConfigFunc:      mgr.Config,
		TrackerRegistry: registry.Trackers,
		AgentRegistry:   registry.Agents,
	}
	validation := orchestrator.ValidateDispatchConfig(preflightParams)
	if !validation.OK() {
		logger.Error("dispatch preflight failed", slog.Any("error", validation))
		return bootResult{}, 1
	}

	cfg := mgr.Config()

	var needResetup bool
	if !logLevelSet {
		lvl, err := resolveLogLevel("", false, cfg.Extensions)
		if err != nil {
			fmt.Fprintf(p.stderr, "sortie: %s\n", err) //nolint:errcheck // stderr write failure is unrecoverable
			return bootResult{}, 1
		}
		if lvl != slog.LevelInfo {
			effectiveLevel = lvl
			needResetup = true
		}
	}
	if !logFormatSet {
		resolvedFmt, err := resolveLogFormat("", false, cfg.Extensions)
		if err != nil {
			fmt.Fprintf(p.stderr, "sortie: %s\n", err) //nolint:errcheck // stderr write failure is unrecoverable
			return bootResult{}, 1
		}
		if resolvedFmt != logging.FormatText {
			effectiveFormat = resolvedFmt
			needResetup = true
		}
	}
	if needResetup {
		logger = logging.Setup(p.stderr, effectiveLevel, effectiveFormat)
		mgr.SetLogger(logger)
	}

	serverPort, serverEnabled, portErr := resolveServerPort(*port, portFlagSet, cfg.Extensions)
	if portErr != nil {
		logger.Error("server port configuration error", slog.Any("error", portErr))
		return bootResult{}, 1
	}
	serverHost, hostErr := resolveServerHost(*host, hostFlagSet, cfg.Extensions)
	if hostErr != nil {
		logger.Error("server host configuration error", slog.Any("error", hostErr))
		return bootResult{}, 1
	}

	trackerCtor, err := registry.Trackers.Get(cfg.Tracker.Kind)
	if err != nil {
		logger.Error("unknown tracker kind", slog.String("kind", cfg.Tracker.Kind), slog.Any("error", err))
		return bootResult{}, 1
	}
	trackerCfgMap := trackerConfigMap(cfg.Tracker)
	trackerCfgMap["user_agent"] = "sortie/" + Version
	mergeExtensions(trackerCfgMap, cfg.Extensions, cfg.Tracker.Kind)
	trackerAdapter, err := trackerCtor(trackerCfgMap)
	if err != nil {
		logger.Error("failed to construct tracker adapter", slog.Any("error", err))
		return bootResult{}, 1
	}

	// Transfer ownership of mgr to the caller — suppress the deferred Stop.
	mgrStarted = false

	return bootResult{
		logger:          logger,
		cfg:             cfg,
		mgr:             mgr,
		path:            path,
		preflightParams: preflightParams,
		trackerAdapter:  trackerAdapter,
		serverPort:      serverPort,
		serverHost:      serverHost,
		serverEnabled:   serverEnabled,
		portIsImplicit:  !portFlagSet && !hasServerPortExtension(cfg.Extensions),
		dryRun:          *dryRun,
		effectiveLevel:  effectiveLevel,
		effectiveFormat: effectiveFormat,
	}, 0
}
