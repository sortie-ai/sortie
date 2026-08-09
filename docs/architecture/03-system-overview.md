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
   - Cleans workspaces for terminal issues, and removes workspaces whose recorded activity has
     aged past the configured retention window when that opt-in bound is enabled.

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
    - Provides read and write access to SCM platform features beyond CI status: human and bot PR
      review comment fetching, review state queries, merge precondition reads, label-event journal
      reads, orchestrator-driven PR merge with optional branch deletion, and label removal.
    - Read-write, multi-method contract (`FetchPendingReviews`, `FetchBotReviewComments`,
      `GetReviewDecision`, `GetCIStatus`, `GetMergeability`, `MergePR`, `DeleteBranch`,
      `ListLabelEvents`, `RemoveLabel`). The write methods are exercised by two reactions only: the
      auto-merge reaction performs the merge and the optional branch deletion, and the label-command
      reactions retract a consumed command label. The merge-completion reaction (Section 11G) reads
      merge state through this adapter and writes nothing through it.
    - Surfaces an additional error kind, `ErrSCMConflict`, for HTTP 405 and HTTP 409 responses
      from the merge endpoint (precondition raced, branch protection refused, or PR already
      merged).
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

This section names what the system depends on outside its own process, by role. Concrete library
choices are recorded in the decision records, not here, because a name repeated in two places rots
in one of them.

- Issue tracker API, reached through whichever tracker adapter the `tracker.kind` names. Each
  adapter carries its own protocol and dialect; the core depends on the contract in Section 11,
  not on any one vendor.
- Local filesystem for workspaces and logs.
- Optional workspace population tooling, for example a version-control client invoked from a hook.
- Coding agent CLI or executable, reached through the configured agent adapter.
- Host environment authentication for the issue tracker and the coding agent.
- Embedded SQL storage, in-process with no external server. See ADR-0002.
- Filesystem event notification, for live reload of the workflow file. Pure Go, no CGo, no
  external daemon. See ADR-0006.
- Metrics exposition, for the Prometheus scrape endpoint when the HTTP server is enabled. Pure Go;
  it does not require an external metrics server to be present. See ADR-0008.
- CI platform API, reached through whichever CI status provider is configured. Required only when
  CI feedback is configured.
- Source-control platform API, reached through whichever SCM adapter is configured. Required only
  when a reaction that observes or writes to a pull request is configured.

