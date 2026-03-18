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
	"syscall"

	"github.com/sortie-ai/sortie/internal/logging"
	"github.com/sortie-ai/sortie/internal/workflow"
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

	logger.Info("sortie started")

	<-ctx.Done()

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
