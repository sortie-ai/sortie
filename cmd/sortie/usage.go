package main

import (
	"fmt"
	"io"
)

func printHelp(w io.Writer) {
	fmt.Fprint(w, //nolint:errcheck // help output write failure is unrecoverable
		`Turn issue tracker tickets into autonomous coding agent sessions.

Usage:
  sortie [flags] [workflow-path]
  sortie <command> [flags]

Commands:
  validate                  Validate a workflow file without running it
  mcp-server                Start the MCP stdio server for agent-to-orchestrator communication

Flags:
  -h, --help                Print this help message and quit
  -V, --version             Print program's version information and quit
  -dumpversion              Print the version of the program and don't do anything else

Run options:
  --dry-run                 Run one poll cycle without spawning agents, then exit
  --env-file PATH           Path to .env file for config overrides
  --log-level LEVEL         Log verbosity: debug, info, warn, error (default: info)
  --log-format FORMAT       Log output format: text, json (default: text)
  --host ADDRESS            HTTP server bind address (default: 127.0.0.1)
  --port PORT               HTTP server port, 0 to disable (default: 7678)

Examples:
  sortie WORKFLOW.md                     Run orchestrator with a workflow
  sortie --dry-run WORKFLOW.md           Validate config and poll once without side effects
  sortie validate --format json w.md     Check workflow syntax, output as JSON

Learn more:
  https://docs.sortie-ai.com
`)
}

func printValidateHelp(w io.Writer) {
	fmt.Fprint(w, //nolint:errcheck // help output write failure is unrecoverable
		`Validate a workflow file without running the orchestrator.

Checks syntax, required fields, and adapter configuration. Exits with
a non-zero code if validation fails.

Usage:
  sortie validate [flags] [workflow-path]

Flags:
  --format FORMAT   Output format: text, json (default: text)

Global flags:
  -h, --help        Print this help message and quit

Examples:
  sortie validate WORKFLOW.md
  sortie validate --format json WORKFLOW.md
`)
}

func printMCPServerHelp(w io.Writer) {
	fmt.Fprint(w, //nolint:errcheck // help output write failure is unrecoverable
		`Start the MCP stdio server for agent-to-orchestrator communication.

The server communicates over stdin/stdout using JSON-RPC per the Model
Context Protocol specification. It is intended to be spawned by the
agent runtime, not run manually.

Usage:
  sortie mcp-server [flags]

Flags:
  --workflow PATH   Absolute path to workflow file (required)

Global flags:
  -h, --help        Print this help message and quit
`)
}

// interceptShortFlags scans args for short help (-h, -help) and short
// version (-V) flags before the flag package sees them. Subcommand
// tokens and the POSIX "--" terminator stop the scan immediately.
func interceptShortFlags(args []string) string {
	for _, arg := range args {
		if arg == "validate" || arg == "mcp-server" {
			return ""
		}
		if arg == "--" {
			return ""
		}
		if arg == "-h" || arg == "-help" {
			return "help"
		}
		if arg == "-V" {
			return "version"
		}
	}
	return ""
}

// containsHelpFlag reports whether args contains a help flag (-h, -help,
// or --help) before a POSIX "--" terminator.
func containsHelpFlag(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if arg == "-h" || arg == "-help" || arg == "--help" {
			return true
		}
	}
	return false
}
