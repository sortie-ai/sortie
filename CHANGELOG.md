# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [1.2.1] - 2026-04-01

### Fixed

- Prevent re-dispatch loop when effort budget (`agent.max_sessions`) is
  exhausted for an issue; the orchestrator now records a durable
  budget-exhaustion guard so the dispatch loop does not restart the issue
  on the next tick
- Persist and display `turns_completed` per run in the dashboard and run
  history table
- Show fully qualified `owner/repo#N` display identifiers for GitHub
  issues in the dashboard and API instead of bare issue numbers
- Pass `handoff_state` to `findCurrentStateLabel`, `extractState`,
  `normalizeIssue`, and `normalizeBlockers` in the GitHub adapter so
  `TransitionIssue` accepts the handoff state and stale labels are
  removed during transitions
- Rename dashboard footer cache label from "Cache:" to "Cache Read:" and
  add explanatory tooltips in the footer and Running Sessions table
- Defer `token_usage` event emission in the Claude adapter when an
  assistant message carries zero output tokens (tool_use-only messages
  in Claude Code 2.x `stream-json` format); the adapter now accumulates
  input tokens and falls back to the result event which carries correct
  totals

### Migrations

- 004: Add index `idx_run_history_issue_id` on `run_history(issue_id)`
- 005: Add `turns_completed INTEGER NOT NULL DEFAULT 0` to `run_history`
- 006: Add nullable `display_identifier` column to `run_history`

## [1.2.0] - 2026-03-31

### Added

- GitHub Copilot CLI adapter: configure with `agent.kind: copilot` for
  fully automated issue-to-code workflows using GitHub's headless Copilot
  CLI. Supports local execution and SSH remote dispatch via `worker.ssh_hosts`.
  Tool scope is controlled by `allowed_tools`, `denied_tools`,
  `available_tools`, and `excluded_tools`; `--allow-all` is the default when
  none are set. Session continuity across turns via `--resume`. Authentication
  uses token env vars when present, falling back to `gh auth status`.
- `worker.ssh_strict_host_key_checking`: new optional worker config field
  controlling OpenSSH `StrictHostKeyChecking` for remote SSH agent sessions.
  Accepts `accept-new` (default, Trust On First Use), `yes` (strict
  verification, requires a pre-populated `known_hosts`), or `no` (disable
  host-key checking). Applies to both the Claude Code and Copilot CLI
  adapters.

## [1.1.0] - 2026-03-30

### Added

- GitHub Issues tracker adapter: configure with `tracker.kind: github` and
  `tracker.project: OWNER/REPO`. State management is label-based;
  `TransitionIssue` applies and removes GitHub labels with convergent retry on
  partial failure.
- GitHub adapter: in-memory ETag cache for reconciliation polls —
  `If-None-Match` conditional requests return `304 Not Modified` on unchanged
  issues, reducing GitHub API rate limit consumption during active runs.
- `sortie validate` GitHub adapter config validation: emits diagnostics for
  `tracker.project` format (`OWNER/REPO`), `GITHUB_TOKEN` environment variable
  hint, empty state labels, and active/terminal state label overlap. Errors
  block dispatch; warnings are advisory.

### Fixed

- `install.sh`: checksum verification used substring matching (`grep | awk`)
  that could accept a checksum entry for the wrong archive entry; now uses
  exact field matching (`awk '$2 == f'`).

## [1.0.0] - 2026-03-29

### Added

- SPDX JSON Software Bill of Materials (SBOM) included with every release
  archive, generated via `syft` in the GoReleaser pipeline for supply-chain
  auditing.

## [0.0.10] - 2026-03-28

### Added

- `sortie validate` template static analysis: three advisory warning
  classes — `WarnDotContext` (top-level key referenced inside `{{ range }}`
  or `{{ with }}` where dot is redefined), `WarnUnknownVar` (variable not in
  the `{issue, attempt, run}` contract), and `WarnUnknownField` (valid
  top-level key with an unknown sub-field, including depth-4+ field chains on
  known level-3 scalars). Warnings appear in both text and JSON output
  without blocking dispatch or changing the exit code.
- `sortie validate` front matter schema validation: detects unknown
  top-level keys, unknown sub-keys within known sections, type mismatches,
  and semantic issues (non-positive `hooks.timeout_ms`, non-numeric
  `max_concurrent_agents_by_state` entries). Field paths are included in
  every diagnostic so operators can locate the offending key. Warnings are
  advisory only.
- `SORTIE_*` environment variable config overrides: any workflow front
  matter key can be overridden via a `SORTIE_`-prefixed environment variable
  (e.g., `SORTIE_POLLING_INTERVAL_MS=5000`). Non-empty real env vars take
  precedence over `.env` file values. Raw line content is removed from `.env`
  parse errors and override values are excluded from debug logs to prevent
  secret leakage.
- Orchestrator: tracker comments posted at session lifecycle points —
  session start, successful completion, and failure — with run duration and
  attempt metadata. Comments fire from a detached goroutine to avoid blocking
  the event loop.
- Orchestrator: issues are transitioned to the configured `in_progress_state`
  on dispatch, with a no-op skip when the issue is already in the
  target state. `sortie_dispatch_transitions_total` Prometheus counter
  tracks `success`, `error`, and `skipped` outcomes.

## [0.0.9] - 2026-03-27

### Added

- `sortie validate` subcommand for one-shot workflow file validation
  without starting the orchestrator, opening the database, or spawning a
  filesystem watcher. Supports `--format text` (stderr diagnostics) and
  `--format json` (structured stdout output) for CI pipelines and pre-commit
  hooks. Flag-parse errors are routed through the diagnostics emitter.
- `--dry-run` flag for a single read-only poll cycle that validates
  the full startup sequence (workflow load, preflight, database, adapter
  wiring) without dispatching work or persisting state.
- `--log-level` flag and `logging.level` workflow extension key to set
  the minimum log severity at startup (`debug`, `info`, `warn`, `error`).
- Dashboard: Workflow column in Active Sessions table and a new Run History
  table showing completed session outcomes, timing, and workflow file.
  SQL migration 003 adds a nullable `workflow_file` column to `run_history`.
- Workspace root write-permission check in dispatch preflight —
  surfaces a clear diagnostic instead of failing mid-dispatch.
- Homebrew tap distribution via GoReleaser-managed tap repository
  (`brew install sortie-ai/tap/sortie`).
- Jira adapter: `User-Agent` header (`sortie/<version>`) sent on
  every HTTP request.
- Claude Code adapter: per-request `APIDurationMS` on `token_usage` events
  for API-call-level latency visibility, clamped to a minimum of
  1 ms.
- Claude Code adapter: tool error text now included in
  `EventToolResult.Message`.

### Fixed

- CLI: adapters implementing `io.Closer` are now closed during graceful
  shutdown, preventing resource leaks.
- Orchestrator: `TurnCount` increments on `session_started` instead of
  session finalization, correctly reflecting in-progress turns.
- Claude Code adapter: tool error messages stripped of XML markup and
  tail-truncated to prevent oversized events.

## [0.0.8] - 2026-03-26

### Added

- JSON API server with `GET /api/v1/state`, `GET /api/v1/<identifier>`, and
  `POST /api/v1/refresh` endpoints for programmatic access to orchestrator
  state. Enabled via `--port` flag or `server.port` config.
- HTML dashboard at `/` with auto-refreshing view of running sessions, retry
  queue, token totals, and runtime statistics when the HTTP server is enabled.
- `/livez` and `/readyz` health endpoints following Kubernetes z-pages
  conventions. `/readyz` checks database accessibility, preflight validation,
  and workflow loading.
- Prometheus `/metrics` endpoint exposing session gauges, dispatch/worker/retry
  counters, token counters, tracker request counters, tool call counters,
  poll and worker duration histograms, and `sortie_build_info`. Uses a dedicated
  `prometheus.Registry` — compatible with standard Prometheus scrape configs.
- `tracker_api` client-side tool: agents can query the tracker during sessions
  to fetch issues and comments, scoped to the configured project.
- SSH worker extension via `worker.ssh_hosts` config: dispatch agent runs to
  remote hosts over SSH with round-robin host selection and per-host concurrency
  limits (`worker.max_concurrent_agents_per_host`).
- Per-session token breakdown in JSON API and dashboard: `input_tokens`,
  `output_tokens`, `cache_creation_tokens`, `cache_read_tokens`.
- Per-session timing breakdown in JSON API and dashboard: `elapsed`,
  `agent_time`, `idle_time`, `agent_pct`.
- Claude Code adapter: `tool_result` events now emitted, making agent tool
  invocations visible in the dashboard and API.
- Worker failure logging in `HandleWorkerExit`: WARN with `next_attempt` and
  `delay_ms` for retryable errors, ERROR for non-retryable errors.
- Structured logging: `issue_id`, `issue_identifier`, and `session_id` context
  fields now present on all orchestrator lifecycle log lines. Agent tool calls
  logged at INFO level.
- POSIX-compatible install script (`install.sh`) for automated binary
  installation.

### Changed

- `POST /api/v1/refresh` returns `409 Conflict` during graceful shutdown
  instead of accepting requests that cannot be fulfilled.

### Fixed

- Claude Code adapter: duplicate `token_usage` events no longer emitted when
  assistant-level usage is already reported in the result message.
- HTTP server: `405 Method Not Allowed` responses now include the `Allow`
  header per RFC 9110.
- Jira adapter: `sortie_tracker_requests_total` counter no longer increments
  on no-op calls with empty ID lists.

## [0.0.7] - 2026-03-24

### Added

- Graceful shutdown: on SIGTERM/SIGINT the orchestrator now drains running
  workers (up to 30 s), persists final state to SQLite, flushes pending
  agent events, and cancels retry timers before exiting.
- Issue handoff via `tracker.handoff_state` config field — when an agent
  session completes normally and the issue is still in an active state, the
  orchestrator transitions it to the configured handoff state (e.g.,
  "In Review") and skips the continuation retry.
- `TransitionIssue` operation on the `TrackerAdapter` interface — Jira
  adapter uses the workflow transitions API; file adapter uses an in-memory
  override map.
- Per-issue effort budget via `agent.max_sessions` — limits total agent
  sessions dispatched per issue before releasing the claim. Default 0
  (unlimited).
- Documentation site at https://docs.sortie-ai.com/ with initial
  configuration reference.

### Fixed

- Orchestrator: continuation retry attempt counter now increments correctly
  across sessions instead of resetting to 1 on every normal exit.
- CLI: orchestrator-only fields (`max_turns`, `max_concurrent_agents`,
  `max_retry_backoff_ms`, `max_concurrent_agents_by_state`) removed from
  the adapter config map, fixing silent shadowing of adapter extension keys
  such as `claude-code.max_turns`.
- Jira adapter: `extractStringSlice` now handles `[]string` from the config
  layer — previously only `[]any` was handled, silently reverting to default
  states and causing configured `active_states` / `terminal_states` to be
  ignored.
- Jira adapter: `FetchIssueStatesByIDs` now queries by numeric `id` instead
  of `key`, and results are keyed by issue ID, fixing reconciliation failures
  where state changes on running issues were never detected.
- Jira adapter: non-numeric IDs are now rejected instead of silently mangled,
  and empty ID lists no longer produce invalid `id IN ()` JQL.
- File and Jira adapters now return `ErrTrackerNotFound` for missing issues
  in `FetchIssueByID` and `FetchIssueComments`.
- Orchestrator: INFO-level tick summary log after each dispatch cycle with
  candidate, dispatched, running, and retrying counters to distinguish normal
  operation from a stall.

## [0.0.6] - 2026-03-23

### Added

- Orchestrator engine with state management, concurrency-limited dispatch,
  worker lifecycle, exponential-backoff retry scheduling, active-run
  reconciliation, and event-driven poll loop with graceful shutdown.
- Full startup sequence: workflow load, preflight validation, database open,
  state reconciliation, and poll loop — in that order.
- Dispatch preflight checks that validate adapter availability, required API
  keys, and agent configuration before dispatching work.
- Adapter metadata via `AdapterMeta` and `RegisterWithMeta` so adapters can
  declare requirements (e.g., `RequiresAPIKey`) checked during preflight.
- Retry classification on `TrackerErrorKind` and `AgentErrorKind` — errors
  are now classified as retryable or permanent for dispatch decisions.
- `ErrTrackerNotFound` error kind for HTTP 404 responses from tracker
  adapters.
- Configurable `db_path` field in workflow configuration with `~` and `$VAR`
  expansion.
- Workflow validation callback (`ValidateFunc`) that guards config promotion
  during hot-reload.

### Fixed

- Workspace `CleanupByPath` now rejects non-canonical paths and uses the
  actual workspace path for pending cleanup instead of reconstructing it
  from config.
- Startup: preflight checks now run before opening the database, preventing
  `.sortie.db` creation when configuration is invalid.
- Startup: `.sortie.db` is now created adjacent to WORKFLOW.md instead of in
  the working directory.

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

[Unreleased]: https://github.com/sortie-ai/sortie/compare/1.2.1...HEAD
[1.2.1]: https://github.com/sortie-ai/sortie/compare/1.2.0...1.2.1
[1.2.0]: https://github.com/sortie-ai/sortie/compare/1.1.0...1.2.0
[1.1.0]: https://github.com/sortie-ai/sortie/compare/1.0.0...1.1.0
[1.0.0]: https://github.com/sortie-ai/sortie/compare/0.0.10...1.0.0
[0.0.10]: https://github.com/sortie-ai/sortie/compare/0.0.9...0.0.10
[0.0.9]: https://github.com/sortie-ai/sortie/compare/0.0.8...0.0.9
[0.0.8]: https://github.com/sortie-ai/sortie/compare/0.0.7...0.0.8
[0.0.7]: https://github.com/sortie-ai/sortie/compare/0.0.6...0.0.7
[0.0.6]: https://github.com/sortie-ai/sortie/compare/0.0.5...0.0.6
[0.0.5]: https://github.com/sortie-ai/sortie/compare/0.0.4...0.0.5
[0.0.4]: https://github.com/sortie-ai/sortie/compare/0.0.3...0.0.4
[0.0.3]: https://github.com/sortie-ai/sortie/compare/0.0.2...0.0.3
[0.0.2]: https://github.com/sortie-ai/sortie/compare/0.0.1...0.0.2
[0.0.1]: https://github.com/sortie-ai/sortie/compare/0.0.0...0.0.1
[0.0.0]: https://github.com/sortie-ai/sortie/releases/tag/0.0.0
