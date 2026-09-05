#!/usr/bin/env bash
# Driver for the run-sortie skill: build the orchestrator, validate its
# config, and actually run a poll cycle end-to-end using the repo's built-in
# offline fixture (file tracker + mock agent, no external credentials).
#
# Usage: bash .claude/skills/run-sortie/driver.sh [build|validate|dry-run|run|clean|all]
# Default: all. Must be run from the repo root (it cds there itself).
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

# Go is often installed but not on the calling shell's PATH (e.g. a fresh
# Git Bash after `winget install GoLang.Go` in the same session). Add the
# default Windows install location if `go` isn't already resolvable.
if ! command -v go >/dev/null 2>&1 && [ -x "/c/Program Files/Go/bin/go" ]; then
  export PATH="/c/Program Files/Go/bin:$PATH"
fi

BIN=./sortie.exe
WORKFLOW=examples/WORKFLOW.test.md
DB=examples/.sortie.db
# WORKFLOW.test.md sets workspace.root to "/tmp/sortie-test-workspaces". Go's
# path handling on Windows resolves a leading "/" against the current drive,
# so this ends up at C:\tmp\sortie-test-workspaces regardless of cwd.
WORKSPACE_ROOT=/c/tmp/sortie-test-workspaces

build() {
  echo "== build =="
  go build -trimpath -o "$BIN" ./cmd/sortie
  "$BIN" --version
}

validate() {
  echo "== validate =="
  "$BIN" validate "$WORKFLOW"
}

dry_run() {
  echo "== dry-run (one poll cycle, no dispatch) =="
  "$BIN" --dry-run --port 0 "$WORKFLOW"
}

run() {
  echo "== run (real orchestration loop, 15s bounded) =="
  # This workflow's tracker never reaches a terminal state, so the process
  # polls forever by design; `timeout` cutting it off with exit 124 is
  # expected, not a failure.
  timeout 15 "$BIN" --port 0 "$WORKFLOW" || true
}

clean() {
  echo "== clean =="
  rm -f "$DB" "${DB}-shm" "${DB}-wal"
  rm -rf "$WORKSPACE_ROOT"
}

cmd="${1:-all}"
case "$cmd" in
  build) build ;;
  validate) validate ;;
  dry-run) dry_run ;;
  run) run ;;
  clean) clean ;;
  all) build; validate; dry_run; run; clean ;;
  *) echo "unknown command: $cmd (expected build|validate|dry-run|run|clean|all)" >&2; exit 1 ;;
esac
