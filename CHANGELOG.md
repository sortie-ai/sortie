# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

> Versions before 1.0.0 do not follow Semantic Versioning. Any release may
> contain breaking changes without prior notice.

## [Unreleased]

## [0.0.5] - 2026-03-21

### Added

- Workspace manager: safe path computation from issue identifiers with
  containment validation and symlink rejection.
- Workspace manager: atomic directory creation and reuse with `CreatedNow`
  flag for hook gating.
- Workspace hook execution with configurable timeout, truncated output
  capture, and restricted subprocess environment (only `PATH`, `HOME`,
  `SHELL`, and `SORTIE_*` variables are inherited).
- Workspace lifecycle orchestration: `Prepare`, `Finish`, and `Cleanup`
  functions that sequence `after_create`, `before_run`, `after_run`, and
  `before_remove` hooks with appropriate failure semantics (fatal vs
  best-effort) and `context.WithoutCancel` for teardown hooks.
- Batch workspace cleanup (`CleanupTerminal`) for removing terminal-state
  issue workspaces with per-identifier error collection and best-effort
  `before_remove` hook execution.
- `ListWorkspaceKeys` for enumerating workspace directory names under a
  root, skipping non-directories and symlinks.

## [0.0.4] - 2026-03-20

### Added

- `AgentAdapter` interface and normalized event model: 13 event types,
  `TokenUsage`, `AgentConfig`, `Session`, `TurnResult`, and `AgentError` with
  9 error kinds.
- Agent adapter registry (`registry.Agents`) for registration and lookup by kind.
- Mock agent adapter (kind `"mock"`) with configurable turn outcomes, delays,
  and cumulative token accumulation for orchestrator and integration testing.
- Claude Code agent adapter (kind `"claude-code"`) that launches the CLI as a
  subprocess, reads JSONL events from stdout, and normalizes them to domain event
  types. Supports graceful SIGTERM→SIGKILL shutdown on context cancellation and
  session resumption via `ResumeSessionID`.

### Fixed

- Claude Code adapter: double-wait race between `RunTurn` and `StopSession` —
  `gracefulKill` is now fire-and-forget with timer-based SIGKILL escalation.
- Claude Code adapter: error on missing binary now includes the actual command
  name instead of a hardcoded string.

## [0.0.3] - 2026-03-20

### Added

- Normalized `Issue` model and `TrackerAdapter` interface for multi-tracker support.
- Typed adapter registry with thread-safe registration and lookup.
- File-based tracker adapter for local JSON task definitions.
- Jira Cloud REST API v3 adapter with cursor-based paginated search, issue detail
  retrieval, state tracking, and comment fetching.
- BFS flattener for Atlassian Document Format (ADF) descriptions to plain text.
- JQL builder with string escaping and optional `query_filter` clause support.
- `query_filter` field in tracker configuration for custom JQL expressions.
- GoReleaser configuration for reproducible cross-platform binary releases
  (linux/darwin/windows, amd64/arm64).

### Fixed

- Jira search endpoint migrated from retired `/rest/api/3/search` to
  `/rest/api/3/search/jql` (Atlassian returns 410 Gone on the old endpoint).
- Infinite loop guard in Jira comment pagination when the API returns
  inconsistent offsets.

## [0.0.2] - 2026-03-19

### Added

- SQLite persistence layer with WAL mode and single-writer enforcement.
- Schema migration runner with versioned SQL files.
- CRUD operations for retry entries, run history, session metadata, and
  aggregate metrics.
- Startup recovery loader that resumes incomplete retry entries on restart.

### Fixed

- Deterministic ordering for session metadata queries via `session_id`
  tie-breaker.

## [0.0.1] - 2026-03-18

### Added

- WORKFLOW.md file loader with YAML front matter and prompt body parsing.
- Typed configuration layer with `$VAR` environment variable resolution
  and `~` home directory expansion.
- Prompt template engine using Go `text/template` in strict mode
  (unknown variables and filters cause hard errors).
- Turn-based prompt builder for multi-turn agent conversations.
- Filesystem watcher for live WORKFLOW.md reload via `fsnotify`.
- CLI entry point (`sortie`) with graceful shutdown and signal handling.

### Fixed

- Environment variable expansion now preserves inline `$VAR` references
  inside URIs instead of silently dropping them.
- Fractional float values no longer silently coerced to integers during
  config parsing.

## [0.0.0] - 2026-03-18

### Added

- Go module scaffold and project directory structure.
- Structured logging built on `log/slog` with issue-aware and
  session-aware contextual fields.
- CI pipeline with `golangci-lint`, `gofmt` enforcement, and test
  execution via GitHub Actions.
- Architecture Decision Records (ADR-0001 through ADR-0005).

[Unreleased]: https://github.com/sortie-ai/sortie/compare/0.0.5...HEAD
[0.0.5]: https://github.com/sortie-ai/sortie/compare/0.0.4...0.0.5
[0.0.4]: https://github.com/sortie-ai/sortie/compare/0.0.3...0.0.4
[0.0.3]: https://github.com/sortie-ai/sortie/compare/0.0.2...0.0.3
[0.0.2]: https://github.com/sortie-ai/sortie/compare/0.0.1...0.0.2
[0.0.1]: https://github.com/sortie-ai/sortie/compare/0.0.0...0.0.1
[0.0.0]: https://github.com/sortie-ai/sortie/releases/tag/0.0.0
