## 3. System Overview

### 3.1 Main Components

1. `Workflow Loader`
   - Reads `WORKFLOW.md`.
   - Parses YAML front matter and prompt body.
   - Returns `{config, prompt_template}`.

2. `Config Layer`
   - Exposes typed getters for workflow config values.
   - Applies defaults and environment variable indirection.
   - Performs validation used by the orchestrator before dispatch.

3. `Issue Tracker Client`
   - Adapter interface over one or more issue trackers.
   - Fetches candidate issues in active states.
   - Fetches current states for specific issue IDs (reconciliation).
   - Fetches terminal-state issues during startup cleanup.
   - Normalizes tracker payloads into a stable issue model regardless of the underlying tracker.

4. `Orchestrator`
   - Owns the poll tick.
   - Owns the authoritative runtime state, backed by SQLite for durability.
   - Decides which issues to dispatch, retry, stop, or release.
   - Tracks session metrics and retry queue state.

5. `Workspace Manager`
   - Maps issue identifiers to workspace paths.
   - Ensures per-issue workspace directories exist.
   - Runs workspace lifecycle hooks.
   - Cleans workspaces for terminal issues.

6. `Agent Runner`
   - Creates workspace.
   - Builds prompt from issue + workflow template.
   - Launches the coding agent session via the configured agent adapter.
   - Relays agent updates back to the orchestrator.
   - Optionally runs a bounded self-review loop after the coding turn loop completes:
     configurable verification commands, workspace diff generation, and structured
     agent feedback for iterative fix cycles. Opt-in; disabled by default.

7. `Persistence Layer`
   - SQLite database for retry queues, session metadata, workspace registry, token accounting, and
     run history.
   - Enables restart recovery without data loss.

8. `Status Surface`
   - Presents human-readable runtime status (terminal output, dashboard, or other operator-facing
     view).
   - Enabled by default when a port is configured; not required for orchestrator correctness.

9. `Logging`
   - Emits structured runtime logs to one or more configured sinks.

10. `CI Status Provider`
    - Fetches CI pipeline status for a given git ref.
    - Returns a normalized result including aggregate status, individual check runs, and an optional
      truncated log excerpt from the first failing check.
    - Read-only, single-method contract (`FetchCIStatus`); does not manage CI pipelines or trigger
      builds.
    - Activated by `ci_feedback.kind` or `reactions.ci_failure.provider` presence in workflow
      front matter.

11. `SCM Adapter`
    - Provides read and write access to SCM platform features beyond CI status: PR review comment
      fetching, review state queries, merge precondition reads, and orchestrator-driven PR merge
      with optional branch deletion.
    - Read-write, multi-method contract (`FetchPendingReviews`, `GetReviewDecision`, `GetCIStatus`,
      `GetMergeability`, `MergePR`, `DeleteBranch`). Write methods are exercised only by the
      auto-merge reaction.
    - Surfaces a sixth error kind, `ErrSCMConflict`, for HTTP 405 and HTTP 409 responses from
      the merge endpoint (precondition raced, branch protection refused, or PR already merged).
    - Activated by `reactions.review_comments.provider` or `reactions.auto_merge.provider`
      presence in workflow front matter. Auto-merge activation requires the provider token to
      carry `pull_requests:write` and, when branch deletion is enabled, `contents:write`.
    - Distinct from CI Status Provider: the CI provider queries pipeline status for a git ref;
      the SCM adapter queries PR-level data (reviews, comments, mergeability) and performs the
      merge call for a pull request number.

### 3.2 Abstraction Levels

Sortie is organized into these layers:

1. `Policy Layer` (repo-defined)
   - `WORKFLOW.md` prompt body.
   - Team-specific rules for ticket handling, validation, and handoff.

2. `Configuration Layer` (typed getters)
   - Parses front matter into typed runtime settings.
   - Handles defaults, environment tokens, and path normalization.

3. `Coordination Layer` (orchestrator)
   - Polling loop, issue eligibility, concurrency, retries, reconciliation.

4. `Execution Layer` (workspace + agent subprocess)
   - Filesystem lifecycle, workspace preparation, coding-agent protocol.

5. `Integration Layer` (tracker adapters, agent adapters, CI status providers, and SCM adapters)
   - API calls and normalization for tracker data; session lifecycle for agent runtimes; CI
     pipeline status queries; PR review comment fetching.
   - Multiple adapters per dimension: tracker adapters (Jira, GitHub, …), agent adapters
     (Claude Code, Copilot CLI, Codex CLI, OpenCode CLI, Kiro CLI, …), CI status providers
     (GitHub Checks, …), and SCM adapters
     (GitHub, …).

6. `Observability Layer` (logs + status surface)
   - Operator visibility into orchestrator and agent behavior.

### 3.3 External Dependencies

- Issue tracker API (Jira REST API for `tracker.kind: jira`, GitHub REST API for
  `tracker.kind: github`, Linear GraphQL API for `tracker.kind: linear`, with additional tracker
  adapters registered separately).
- Local filesystem for workspaces and logs.
- Optional workspace population tooling (for example Git CLI, if used).
- Coding agent CLI or executable reachable via the configured agent adapter.
- Host environment authentication for the issue tracker and coding agent.
- SQLite library (embedded, no external server).
- Filesystem event library (`github.com/fsnotify/fsnotify`) for `WORKFLOW.md` live reload.
  Pure Go, no CGo, no external daemon. See ADR-0006.
- Metrics exposition library (`github.com/prometheus/client_golang`) for the Prometheus
  `/metrics` endpoint when the HTTP server is enabled. Pure Go; does not require an external
  Prometheus server. See ADR-0008.
- CI platform API (GitHub Checks API for `ci_feedback.kind: github` or `reactions.ci_failure.provider: github`,
  with additional providers registered separately). Only required when CI feedback is configured.
- SCM platform API (GitHub REST API for `reactions.review_comments.provider: github`, with
  additional adapters registered separately). Only required when `reactions.review_comments` is
  configured.

