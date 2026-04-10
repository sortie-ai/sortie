# Sortie Architecture

This document is derived from the [Symphony Service Specification](https://github.com/openai/symphony/blob/main/SPEC.md).
Sortie is a concrete Go implementation, not a language-agnostic specification. Key adaptations from
Symphony include: agent-agnostic design with Claude Code as the first supported runtime,
tracker-agnostic design with Jira as the first supported tracker, SQLite-backed persistence for
retry queues and run history, adapter-based extensibility for both agent runtimes and issue
trackers, and orchestrator-level handoff transitions that break the continuation retry loop without
relying on agent behavior.

## 1. Problem Statement

Sortie is a long-running automation service that continuously reads work from an issue tracker,
creates an isolated workspace for each issue, and runs a coding agent session for that issue inside
the workspace.

The service solves four operational problems:

- It turns issue execution into a repeatable daemon workflow instead of manual scripts.
- It isolates agent execution in per-issue workspaces so agent commands run only inside per-issue
  workspace directories.
- It keeps the workflow policy in-repo (`WORKFLOW.md`) so teams version the agent prompt and runtime
  settings with their code.
- It provides enough observability to operate and debug multiple concurrent agent runs.

Sortie documents its trust and safety posture explicitly. It does not mandate a single approval,
sandbox, or operator-confirmation policy; some deployments may target trusted environments with a
high-trust configuration, while others may require stricter approvals or sandboxing.

Important boundary:

- Sortie is a scheduler/runner and tracker reader.
- Ticket writes (state transitions, comments, PR links) are typically performed by the coding agent
  using tools available in the workflow/runtime environment.
- A successful run may end at a workflow-defined handoff state (for example `Human Review`), not
  necessarily `Done`.

## 2. Goals and Non-Goals

### 2.1 Goals

- Poll the issue tracker on a fixed cadence and dispatch work with bounded concurrency.
- Maintain a single authoritative orchestrator state for dispatch, retries, and reconciliation.
- Create deterministic per-issue workspaces and preserve them across runs.
- Stop active runs when issue state changes make them ineligible.
- Recover from transient failures with exponential backoff.
- Load runtime behavior from a repository-owned `WORKFLOW.md` contract.
- Expose operator-visible observability (at minimum structured logs).
- Survive process restarts without losing retry queues, session metadata, or run history.
- Support pluggable issue tracker adapters without modifying core orchestration logic.
- Support pluggable coding agent runtimes without modifying core orchestration logic.

### 2.2 Non-Goals

- Multi-tenant control plane or separately deployed frontend application.
- Prescribing a specific dashboard or terminal UI implementation.
- General-purpose workflow engine or distributed job scheduler.
- Built-in business logic for how to edit tickets, PRs, or comments. (That logic lives in the
  workflow prompt and agent tooling.)
- Mandating strong sandbox controls beyond what the coding agent and host OS provide.
- Mandating a single default approval, sandbox, or operator-confirmation posture for all
  deployments.

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
    - Provides read-only access to SCM platform features beyond CI status: PR review comment
      fetching, review state queries.
    - Read-only, multi-method contract (`FetchPendingReviews`); does not create PRs, push code,
      or manage branches.
    - Activated by `reactions.review_comments.provider` presence in workflow front matter.
    - Distinct from CI Status Provider: the CI provider queries pipeline status for a git ref;
      the SCM adapter queries PR-level data (reviews, comments) for a pull request number.

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
     (Claude Code, Codex, …), CI status providers (GitHub Checks, …), and SCM adapters
     (GitHub, …).

6. `Observability Layer` (logs + status surface)
   - Operator visibility into orchestrator and agent behavior.

### 3.3 External Dependencies

- Issue tracker API (Jira REST API for `tracker.kind: jira`, with additional tracker adapters
  available).
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

## 4. Core Domain Model

### 4.1 Entities

#### 4.1.1 Issue

Normalized issue record used by orchestration, prompt rendering, and observability output.

Fields:

- `id` (string)
  - Stable tracker-internal ID.
- `identifier` (string)
  - Human-readable ticket key (example: `ABC-123`).
- `title` (string)
- `description` (string or null)
- `priority` (integer or null)
  - Lower numbers are higher priority in dispatch sorting.
- `state` (string)
  - Current tracker state name.
- `branch_name` (string or null)
  - Tracker-provided branch metadata if available.
- `url` (string or null)
- `labels` (list of strings)
  - Normalized to lowercase.
- `assignee` (string or null)
  - Assignee identity as provided by the tracker. Used for prompt rendering and observability.
- `issue_type` (string or null)
  - Tracker-defined type (for example Bug, Story, Task, Epic). Used for prompt rendering and
    per-type concurrency limits.
- `parent` (object or null)
  - Parent issue reference for sub-tasks. Relevant for blocking/dependency logic and prompt
    context.
- `comments` (list or null)
  - Comment records containing human feedback, review notes, and prior agent workpad entries.
    Needed for continuation runs where the agent must understand prior communication.
- `blocked_by` (list of blocker refs)
  - Each blocker ref contains:
    - `id` (string or null)
    - `identifier` (string or null)
    - `state` (string or null)
  - If `blocker.state` is null or unknown, treat it as non-terminal (conservative).
- `created_at` (timestamp or null)
- `updated_at` (timestamp or null)

#### 4.1.2 Workflow Definition

Parsed `WORKFLOW.md` payload:

- `config` (map)
  - YAML front matter root object.
- `prompt_template` (string)
  - Markdown body after front matter, trimmed.

#### 4.1.3 Service Config (Typed View)

Typed runtime values derived from `WorkflowDefinition.config` plus environment resolution.

Examples:

- poll interval
- workspace root
- active and terminal issue states
- concurrency limits
- coding-agent executable/args/timeouts
- workspace hooks

#### 4.1.4 Workspace

Filesystem workspace assigned to one issue identifier.

Fields (logical):

- `path` (workspace path; current runtime typically uses absolute paths, but relative roots are
  possible if configured without path separators)
- `workspace_key` (sanitized issue identifier)
- `created_now` (boolean, used to gate `after_create` hook)

#### 4.1.5 Run Attempt

One execution attempt for one issue.

Fields (logical):

- `issue_id`
- `issue_identifier`
- `attempt` (integer or null, `null` for first run, `>=1` for retries/continuation)
- `workspace_path`
- `started_at`
- `completed_at` (timestamp or null)
  - Populated when the attempt finishes. Used for run duration calculation and persistent history.
- `status`
- `agent_adapter` (string)
  - Which agent adapter was used for this attempt. Relevant when multiple agent types are
    configured.
- `error` (optional)

#### 4.1.6 Live Session (Agent Session Metadata)

State tracked while a coding-agent subprocess is running.

Fields:

- `session_id` (string)
  - Opaque string assigned by the agent adapter. For adapters that expose thread/turn concepts,
    the composition rule (e.g., `<thread_id>-<turn_id>`) is adapter-specific, not universal.
- `thread_id` (string)
- `turn_id` (string)
- `agent_pid` (string or null)
- `last_agent_event` (string/enum or null)
- `last_agent_timestamp` (timestamp or null)
- `last_agent_message` (summarized payload)
- `agent_input_tokens` (integer)
- `agent_output_tokens` (integer)
- `agent_total_tokens` (integer)
- `last_reported_input_tokens` (integer)
- `last_reported_output_tokens` (integer)
- `last_reported_total_tokens` (integer)
- `turn_count` (integer)
  - Number of coding-agent turns started within the current worker lifetime.

#### 4.1.7 Retry Entry

Scheduled retry state for an issue.

Fields:

- `issue_id`
- `identifier` (best-effort human ID for status surfaces/logs)
- `attempt` (integer, 1-based for retry queue)
- `due_at_ms` (monotonic clock timestamp)
- `timer_handle` (runtime-specific timer reference; runtime-only, not persisted to SQLite)
- `error` (string or null)

Note: `timer_handle` is a runtime-only field and is not persisted. On restart, pending retries
are reconstructed from the persisted `due_at` timestamps stored in SQLite.

#### 4.1.8 Orchestrator Runtime State

Single authoritative state owned by the orchestrator. The running map and active timers are
in-memory for performance; retry_attempts, completed set, and agent_totals are backed by SQLite
and survive restarts.

Fields:

- `poll_interval_ms` (current effective poll interval)
- `max_concurrent_agents` (current effective global concurrency limit)
- `running` (map `issue_id -> running entry`)
- `claimed` (set of issue IDs reserved/running/retrying)
- `retry_attempts` (map `issue_id -> RetryEntry`)
- `completed` (set of issue IDs; bookkeeping only, not dispatch gating)
- `agent_totals` (aggregate tokens + runtime seconds)
- `agent_rate_limits` (latest rate-limit snapshot from agent events)
- `reaction_attempts` (map `issue_id:kind -> integer`; number of reaction-fix continuations
  dispatched per issue and reaction kind; reset when the issue leaves the running/retry maps;
  runtime-only, not persisted)
- `pending_reactions` (map `issue_id:kind -> PendingReaction`; populated by worker exit on normal
  exits with SCM metadata when a CI status provider or SCM adapter is configured; consumed by
  per-kind reconcile functions during the reconcile tick — `reconcile_ci_status` for kind `ci`,
  `reconcile_review_comments` for kind `review`; runtime-only, not persisted)

### 4.2 Stable Identifiers and Normalization Rules

- `Issue ID`
  - Use for tracker lookups and internal map keys.
- `Issue Identifier`
  - Use for human-readable logs and workspace naming.
- `Workspace Key`
  - Derive from `issue.identifier` by replacing any character not in `[A-Za-z0-9._-]` with `_`.
  - Use the sanitized value for the workspace directory name.
- `Normalized Issue State`
  - Compare states after `lowercase`.
- `Session ID`
  - Opaque string assigned by the agent adapter. Composition is adapter-specific.

## 5. Workflow Specification (Repository Contract)

### 5.1 File Discovery and Path Resolution

Workflow file path precedence:

1. Explicit application/runtime setting (set by CLI startup path).
2. Default: `WORKFLOW.md` in the current process working directory.

Loader behavior:

- If the file cannot be read, return `missing_workflow_file` error.
- The workflow file is expected to be repository-owned and version-controlled.

### 5.2 File Format

`WORKFLOW.md` is a Markdown file with optional YAML front matter.

Design note:

- `WORKFLOW.md` should be self-contained enough to describe and run different workflows (prompt,
  runtime settings, hooks, and tracker selection/config) without requiring out-of-band
  service-specific configuration.

Parsing rules:

- If file starts with `---`, parse lines until the next `---` as YAML front matter.
- Remaining lines become the prompt body.
- If front matter is absent, treat the entire file as prompt body and use an empty config map.
- YAML front matter must decode to a map/object; non-map YAML is an error.
- Prompt body is trimmed before use.

Returned workflow object:

- `config`: front matter root object (not nested under a `config` key).
- `prompt_template`: trimmed Markdown body.

### 5.3 Front Matter Schema

Top-level keys:

- `tracker`
- `polling`
- `workspace`
- `hooks`
- `agent`
- `ci_feedback` (deprecated; use `reactions.ci_failure` instead)
- `reactions`
- `db_path`

Unknown keys should be ignored for forward compatibility.

Note:

- The workflow front matter is extensible. Optional extensions may define additional top-level keys
  (for example `server`) without changing the core schema above.
- Extensions should document their field schema, defaults, validation rules, and whether changes
  apply dynamically or require restart.
- Common extensions: `server.port` (integer) overrides the default HTTP server port (7678);
  `server.host` (string, IP address) overrides the default bind address (`127.0.0.1`). The
  HTTP server starts unconditionally unless `server.port` or `--port` is `0`. See Section
  13.7 for full semantics.

#### 5.3.1 `tracker` (object)

Fields:

- `kind` (string)
  - Required for dispatch. No default; must be explicitly specified.
  - Supported values: `jira` (initial implementation); additional adapters registered separately.
- `endpoint` (string)
  - Tracker API endpoint. Interpretation is adapter-defined.
- `api_key` (string)
  - May be a literal token or `$VAR_NAME`.
  - If `$VAR_NAME` resolves to an empty string, treat the key as missing.
  - Required for dispatch when the tracker adapter declares it (e.g., Jira requires an API
    key; a file-based tracker does not).
- `project` (string)
  - Project identifier. Interpretation is adapter-defined (e.g., project key for Jira, slug for
    Linear). Required for dispatch when the tracker adapter requires project scoping.
- `active_states` (list of strings)
  - Default values are adapter-defined; must be configured explicitly when the adapter's defaults
    differ from deployment expectations.
- `terminal_states` (list of strings)
  - Default values are adapter-defined; must be configured explicitly when the adapter's defaults
    differ from deployment expectations.
- `query_filter` (string, optional)
  - Adapter-defined query fragment appended to base candidate and terminal-state queries.
  - The orchestrator passes this value to the tracker adapter without interpretation.
  - The adapter is responsible for safe integration into its native query language.
  - Default: empty string (no additional filtering).
- `handoff_state` (string, optional)
  - Target tracker state for orchestrator-initiated handoff transitions after a successful
    worker run (see ADR-0007).
  - Supports `$VAR` environment indirection.
  - When absent, no handoff transition is performed; the orchestrator uses
    continuation retry as before.
  - Empty values, including `$VAR` references that resolve to empty, are treated as
    configuration errors.
  - Must not appear in `active_states` (would cause immediate re-dispatch after handoff).
  - Must not appear in `terminal_states` (handoff is not terminal; the issue may return
    to active).
  - Changes take effect for future worker exits, not in-flight sessions.
- `in_progress_state` (string, optional)
  - Target tracker state for dispatch-time transitions. When configured, the worker calls
    `TransitionIssue` as the first step of each attempt, before workspace preparation.
  - Supports `$VAR` environment indirection.
  - When absent, no dispatch-time transition is performed. This is the default.
  - Empty values, including `$VAR` references that resolve to empty, are treated as
    configuration errors.
  - MUST appear in `active_states` (otherwise reconciliation would immediately cancel the
    worker after the transition changes the issue's tracker state).
  - MUST NOT appear in `terminal_states` (a terminal state would trigger workspace cleanup
    on the next reconciliation tick).
  - MUST NOT collide with `handoff_state` (the two transitions represent different lifecycle
    phases — dispatch vs. exit).
  - Transition failure is non-fatal: the worker logs a warning and continues to workspace
    preparation.
  - If the issue is already in the target state (case-insensitive comparison), the
    `TransitionIssue` call is skipped and a debug-level message is logged.
  - Changes take effect for future dispatches, not in-flight sessions.

#### 5.3.2 `polling` (object)

Fields:

- `interval_ms` (integer or string integer)
  - Default: `30000`
  - Changes should be re-applied at runtime and affect future tick scheduling without restart.

#### 5.3.3 `workspace` (object)

Fields:

- `root` (path string or `$VAR`)
  - Default: `<system-temp>/sortie_workspaces`
  - `~` and strings containing path separators are expanded.
  - Bare strings without path separators are preserved as-is (relative roots are allowed but
    discouraged).

#### 5.3.4 `hooks` (object)

Fields:

- `after_create` (multiline shell script string, optional)
  - Runs only when a workspace directory is newly created.
  - Failure aborts workspace creation.
- `before_run` (multiline shell script string, optional)
  - Runs before each agent attempt after workspace preparation and before launching the coding
    agent.
  - Failure aborts the current attempt.
- `after_run` (multiline shell script string, optional)
  - Runs after each agent attempt (success, failure, timeout, or cancellation) once the workspace
    exists.
  - Failure is logged but ignored.
- `before_remove` (multiline shell script string, optional)
  - Runs before workspace deletion if the directory exists.
  - Failure is logged but ignored; cleanup still proceeds.
- `timeout_ms` (integer, optional)
  - Default: `60000`
  - Applies to all workspace hooks.
  - Non-positive values should be treated as invalid and fall back to the default.
  - Changes should be re-applied at runtime for future hook executions.

Hook environment variables (minimum set available to all hooks):

- `SORTIE_ISSUE_ID`
- `SORTIE_ISSUE_IDENTIFIER`
- `SORTIE_WORKSPACE`
- `SORTIE_ATTEMPT`

These allow hooks to make decisions without parsing orchestrator internals.

#### 5.3.5 `agent` (object)

Fields:

- `kind` (string)
  - Specifies which agent adapter to use. Default: `claude-code`.
  - Other supported values: `codex`, `http`, and any additionally registered adapter.
  - Parallels `tracker.kind`.
- `command` (string shell command)
  - The command the agent adapter uses to launch the agent process. Adapter-defined default.
  - When `agent.kind` requires a local command, this field must be present and non-empty.
  - HTTP-based agent adapters do not require a local command.
- `turn_timeout_ms` (integer)
  - Default: `3600000` (1 hour)
- `read_timeout_ms` (integer)
  - Default: `5000`
- `stall_timeout_ms` (integer)
  - Default: `300000` (5 minutes)
  - If `<= 0`, stall detection is disabled.
- `max_concurrent_agents` (integer or string integer)
  - Default: `10`
  - Changes should be re-applied at runtime and affect subsequent dispatch decisions.
- `max_retry_backoff_ms` (integer or string integer)
  - Default: `300000` (5 minutes)
  - Changes should be re-applied at runtime and affect future retry scheduling.
- `max_concurrent_agents_by_state` (map `state_name -> positive integer`)
  - Default: empty map.
  - State keys are normalized (`lowercase`) for lookup.
  - Invalid entries (non-positive or non-numeric) are ignored.
- `max_sessions` (integer)
  - Default: `0` (unlimited; no effort budget enforced).
  - Maximum number of completed worker sessions for a single issue before the orchestrator
    stops re-dispatching it. Counted from `run_history` entries.
  - When the count reaches `max_sessions`, the claim is released and a warning is logged.
  - `0` disables the budget (unlimited retries).
  - Changes are re-applied at runtime and affect future retry timer evaluations.

Adapter-specific pass-through config:

Each adapter may define its own configuration fields in a sub-object named after its `kind`
value. These are pass-through values interpreted by the adapter and not by the orchestrator
core. For example, a Codex adapter may accept `codex.approval_policy` and
`codex.thread_sandbox`; a Claude Code adapter may accept `claude-code.permission_mode`.
The orchestrator forwards the entire sub-object to the adapter without validation.

#### 5.3.6 `ci_feedback` (object, optional, **deprecated**)

**Deprecated.** Use `reactions.ci_failure` instead (Section 5.3.9). When both `ci_feedback` and
`reactions.ci_failure` are present, `reactions.ci_failure` takes precedence and a deprecation
warning is logged.

CI feedback loop configuration. Feature activation follows the same pattern as other optional
sections (`server.port`, `worker.ssh_hosts`): presence of the `kind` field activates the feature;
there is no separate `enabled` flag. When the section is absent or `kind` is empty, CI feedback is
disabled and no `CIStatusProvider` is constructed.

Fields:

- `kind` (string)
  - Identifies the CI status provider adapter (e.g. `github`). Empty string or absent means CI
    feedback is disabled.
  - The orchestrator resolves the adapter via the CI provider registry at startup.
- `max_retries` (integer)
  - Maximum number of CI-fix continuation dispatches per issue before escalation.
  - Default: `2`. Zero means escalate immediately on first CI failure (no fix attempts).
  - MUST be non-negative; negative values are rejected with a configuration error.
- `max_log_lines` (integer)
  - Maximum number of log tail lines fetched from the first failing check run for prompt
    injection.
  - Default: `50`. Zero disables log fetching.
  - MUST be non-negative; negative values are rejected with a configuration error.
- `escalation` (string)
  - Action taken when `max_retries` is exceeded.
  - Valid values: `label` (default), `comment`.
  - `label`: adds `escalation_label` to the tracker issue.
  - `comment`: posts a plain-text escalation comment listing failing checks and the ref.
  - Invalid values are rejected with a configuration error.
- `escalation_label` (string)
  - Label applied when escalation is `label`.
  - Default: `needs-human`.

The CI provider adapter receives `max_log_lines` and the pass-through config sub-object named by
`ci_feedback.kind` from `Extensions[kind]`. The orchestrator merges tracker credentials (API key,
project, endpoint) into that CI adapter config only when the tracker and CI feedback `kind`
values match.

#### 5.3.7 `db_path` (string, optional)

Filesystem path for the SQLite database file.

- Supports `$VAR` environment indirection and `~` home directory expansion.
- Absolute paths are used as-is.
- Relative paths are resolved against the directory containing `WORKFLOW.md`.
- Default: `.sortie.db` in the same directory as `WORKFLOW.md`.
- An explicit empty string (`db_path: ""`) is equivalent to omitting the field; the
  default path is used.
- Non-string values are rejected with a configuration error.
- If the value resolves to an empty string after environment expansion (e.g., an unset
  `$VAR`), startup fails with a configuration error.
- Changes to `db_path` during dynamic reload update the in-memory config but have no
  effect on the already-open database connection; a restart is required.

#### 5.3.8 `self_review` (object, optional)

Self-review loop configuration. When `enabled` is true and `verification_commands` is
non-empty, the orchestrator runs a bounded review-fix cycle after the coding turn loop
completes. Each iteration executes verification commands, generates a workspace diff, and
presents both to the agent for a structured verdict. Disabled by default; zero overhead
when disabled.

Fields:

- `enabled` (boolean)
  - Activates the self-review loop. Default: `false`.
  - When `true`, `verification_commands` must be non-empty or a configuration error is
    raised.
- `max_iterations` (integer)
  - Hard cap on review iterations. Default: `3`. Range: [1, 10].
  - Each iteration consists of a review turn and (if the verdict is `iterate`) a fix turn.
    `max_iterations: N` means up to `2N − 1` additional agent turns.
- `verification_commands` (list of strings)
  - Shell commands executed during each review iteration. Required when `enabled` is true.
  - Each command runs in its own subprocess with the workspace as `cwd`, process group
    isolation, and per-command timeout.
- `verification_timeout_ms` (integer)
  - Per-command timeout in milliseconds. Default: `120000` (2 minutes).
- `max_diff_bytes` (integer)
  - Maximum bytes of workspace diff included in the review prompt. Default: `102400`
    (100 KB). Diffs exceeding this limit are truncated with a marker.
- `reviewer` (string)
  - Which agent performs the review. Default: `"same"`. Only `"same"` (reuse the current
    session) is supported in v1.

#### 5.3.9 `reactions` (object, optional)

Reaction configuration. Each key under `reactions` identifies a reaction kind (e.g.
`ci_failure`, `review_comments`). The orchestrator creates pending reaction entries on normal
worker exit and processes them during the reconcile tick. Reaction kinds are extensible: unknown
kind keys are parsed into a generic `ReactionConfig` and made available to future consumers.

The `reactions` section supersedes the deprecated `ci_feedback` top-level key. When both
`ci_feedback` and `reactions.ci_failure` are present, `reactions.ci_failure` takes precedence
and a deprecation warning is logged.

**Common fields per reaction kind:**

Each reaction kind sub-object shares a common field schema:

- `provider` (string)
  - Identifies the external system adapter for this reaction kind (e.g. `github`). Empty string
    or absent means the reaction kind is disabled.
- `max_retries` (integer)
  - Maximum fix continuation dispatches per issue before escalation. Default: `2`.
  - MUST be non-negative; negative values are rejected with a configuration error.
- `escalation` (string)
  - Action when `max_retries` is exceeded. Valid values: `label` (default), `comment`.
- `escalation_label` (string)
  - Label applied when `escalation` is `label`. Default: `needs-human`.

Remaining keys within a kind sub-object are collected into an `Extra` map for kind-specific
consumption.

**Reaction kind: `ci_failure`**

Equivalent to the deprecated `ci_feedback` section. See Section 11A for the CI feedback contract.
Extra fields:

- `max_log_lines` (integer, via Extra): maximum CI log tail lines. Default: `50`.

**Reaction kind: `review_comments`**

PR review comment routing. When configured, the orchestrator polls for human `CHANGES_REQUESTED`
review comments on Sortie-created PRs and dispatches continuation turns so the agent can address
the feedback. See Section 11B for the full contract.

Extra fields:

- `poll_interval_ms` (integer, via Extra): polling interval for review comments. Default:
  `120000` (2 minutes). Minimum: `30000`.
- `debounce_ms` (integer, via Extra): debounce window after the last detected comment before
  dispatching. Default: `60000` (60 seconds). MUST be non-negative.
- `max_continuation_turns` (integer, via Extra): maximum review-fix continuation dispatches per
  issue before escalation. Default: `3`. MUST be positive.

Example:

```yaml
reactions:
  review_comments:
    provider: github
    max_retries: 2
    escalation: label
    escalation_label: needs-human
    poll_interval_ms: 120000
    debounce_ms: 60000
    max_continuation_turns: 3
```

**Validation rules:**

- Reaction kind keys MUST match `[a-z][a-z0-9_-]*`.
- Invalid kind keys are rejected with a configuration error.
- Per-kind common fields follow the same validation as the deprecated `ci_feedback` equivalents.
- Extra fields are kind-specific; the orchestrator validates them when constructing the
  kind-specific config (e.g. `BuildReviewReactionConfig`).

### 5.4 Prompt Template Contract

The Markdown body of `WORKFLOW.md` is the per-issue prompt template.

Sortie uses Go `text/template` for prompt rendering.

Rendering requirements:

- Use a strict template engine that fails on unknown variables.
- Unknown variables must fail rendering.
- Unknown filters must fail rendering.

Template input variables:

- `issue` (object)
  - Includes all normalized issue fields, including labels and blockers.
- `attempt` (integer or null)
  - `null`/absent on first attempt.
  - Integer on retry or continuation run.
- `run` (object)
  - `turn_number` (integer): current turn number within the session.
  - `max_turns` (integer): configured maximum turns per session.
  - `is_continuation` (boolean): true when this is a continuation turn in a multi-turn session,
    as distinct from a retry after an error.

Fallback prompt behavior:

- If the workflow prompt body is empty, the runtime may use a minimal default prompt.
- Workflow file read/parse failures are configuration/validation errors and should not silently fall
  back to a prompt.

### 5.5 Workflow Validation and Error Surface

Error classes:

- `missing_workflow_file`
- `workflow_parse_error`
- `workflow_front_matter_not_a_map`
- `template_parse_error` (during prompt rendering)
- `template_render_error` (unknown variable/filter, invalid interpolation)

Dispatch gating behavior:

- Workflow file read/YAML errors block new dispatches until fixed.
- Template errors fail only the affected run attempt.

## 6. Configuration Specification

### 6.1 Source Precedence and Resolution Semantics

Configuration precedence (highest to lowest):

1. Workflow file path selection (runtime setting -> cwd default).
2. `SORTIE_*` real environment variables (curated set; see below).
3. `.env` file values (opt-in via `SORTIE_ENV_FILE` env var or `--env-file` CLI flag).
4. YAML front matter values.
5. Environment indirection via `$VAR_NAME` inside selected YAML values — applies only to values
   not overridden by env (layers 2–3).
6. Built-in defaults.

**Environment variable overrides:** A curated set of `SORTIE_*` environment variables map to
specific config fields. Env overrides replace the YAML value in the raw map before `$VAR`
expansion and section builders run. The curated variable list, type coercion rules, `.env` file
format, and exclusions are documented in the WORKFLOW.md Syntax Reference (Section 3). The override
merge runs inside `NewServiceConfig` as a pre-processing step; all existing validation, coercion,
and default logic applies uniformly regardless of source. On dynamic reload, env vars and the `.env`
file are re-read. Real env var changes require a process restart; `.env` file changes are picked up
on each reload.

**Double-expansion prevention:** Values sourced from environment overrides MUST NOT be passed through
`os.ExpandEnv`, `resolveEnv`, or `resolveEnvRef`. Section builders use the override set returned by
`applyEnvOverrides` to skip `$VAR` expansion for env-sourced fields. Only tilde (`~`) expansion is
permitted for path fields.

Value coercion semantics:

- Path/command fields support:
  - `~` home expansion
  - `$VAR` expansion for env-backed path values
  - Apply expansion only to values intended to be local filesystem paths; do not rewrite URIs or
    arbitrary shell command strings.

### 6.2 Dynamic Reload Semantics

Dynamic reload is required:

- Sortie watches `WORKFLOW.md` for changes.
- On change, it re-reads and re-applies workflow config and prompt template without restart.
- Sortie adjusts live behavior to the new config (for example polling cadence, concurrency limits,
  active/terminal states, agent settings, workspace paths/hooks, and prompt content for future
  runs).
- Reloaded config applies to future dispatch, retry scheduling, reconciliation decisions, hook
  execution, and agent launches.
- In-flight agent sessions are not restarted automatically when config changes.
- Extensions that manage their own listeners/resources (for example an HTTP server port change) may
  require restart unless live rebind is explicitly supported.
- Sortie also re-validates/reloads defensively during runtime operations (for example before
  dispatch) in case filesystem watch events are missed.
- Invalid reloads do not crash the service; Sortie keeps operating with the last known good
  effective configuration and emits an operator-visible error.

### 6.3 Dispatch Preflight Validation

This validation is a scheduler preflight run before attempting to dispatch new work. It validates
the workflow/config needed to poll and launch workers, not a full audit of all possible workflow
behavior.

Startup validation:

- Validate configuration before starting the scheduling loop.
- If startup validation fails, fail startup and emit an operator-visible error.

Per-tick dispatch validation:

- Re-validate before each dispatch cycle.
- If validation fails, skip dispatch for that tick, keep reconciliation active, and emit an
  operator-visible error.

Validation checks:

- Workflow file can be loaded and parsed.
- `tracker.kind` is present and supported.
- `tracker.api_key` is present after `$` resolution, when required by the selected tracker adapter.
- `tracker.project` is present when required by the selected tracker adapter.
- `agent.command` is present and non-empty when `agent.kind` requires a local command.
- Tracker adapter for the configured `tracker.kind` is registered and available.
- Agent adapter for the configured `agent.kind` is registered and available.

### 6.4 Config Fields Summary (Cheat Sheet)

This section is intentionally redundant so a coding agent can implement the config layer quickly.

- `tracker.kind`: string, required, no default (e.g., `jira`)
- `tracker.endpoint`: string, adapter-defined default
- `tracker.api_key`: string or `$VAR`, required when the tracker adapter declares it
- `tracker.project`: string, required when the tracker adapter requires project scoping
- `tracker.active_states`: list of strings, adapter-defined defaults
- `tracker.terminal_states`: list of strings, adapter-defined defaults
- `tracker.query_filter`: string, optional, default empty (adapter-defined filter fragment)
- `tracker.handoff_state`: string, optional, default absent; target state for
  orchestrator-initiated handoff after successful worker run; must not collide with
  `active_states` or `terminal_states`; supports `$VAR`
- `tracker.in_progress_state`: string, optional, default absent; target state for
  dispatch-time transition at the start of each worker attempt; must be in `active_states`,
  must not collide with `terminal_states` or `handoff_state`; supports `$VAR`
- `polling.interval_ms`: integer, default `30000`
- `workspace.root`: path, default `<system-temp>/sortie_workspaces`
- `worker.ssh_hosts` (extension): list of SSH host strings, optional; when omitted, work runs
  locally
- `worker.max_concurrent_agents_per_host` (extension): positive integer, optional; shared per-host
  cap applied across configured SSH hosts
- `hooks.after_create`: shell script or null
- `hooks.before_run`: shell script or null
- `hooks.after_run`: shell script or null
- `hooks.before_remove`: shell script or null
- `hooks.timeout_ms`: integer, default `60000`
- `agent.kind`: string, default `claude-code`
- `agent.command`: shell command string, adapter-defined default
- `agent.turn_timeout_ms`: integer, default `3600000`
- `agent.read_timeout_ms`: integer, default `5000`
- `agent.stall_timeout_ms`: integer, default `300000`
- `agent.max_concurrent_agents`: integer, default `10`
- `agent.max_turns`: integer, default `20`
- `agent.max_retry_backoff_ms`: integer, default `300000` (5m)
- `agent.max_concurrent_agents_by_state`: map of positive integers, default `{}`
- `agent.max_sessions`: integer, default `0` (unlimited)
- `ci_feedback.kind`: string, optional, **deprecated**; identifies the CI status provider adapter;
  presence activates CI feedback; use `reactions.ci_failure` instead
- `ci_feedback.max_retries`: integer, default `2`; CI-fix continuation attempts before escalation
- `ci_feedback.max_log_lines`: integer, default `50`; log tail lines from failing checks (`0`
  disables)
- `ci_feedback.escalation`: string, default `label`; action on retry exhaustion (`label` or
  `comment`)
- `ci_feedback.escalation_label`: string, default `needs-human`; label applied during `label`
  escalation
- `reactions.<kind>.provider`: string, optional; adapter identifier; absent = disabled
- `reactions.<kind>.max_retries`: integer, default `2`; fix continuation attempts before escalation
- `reactions.<kind>.escalation`: string, default `label`; `label` or `comment`
- `reactions.<kind>.escalation_label`: string, default `needs-human`
- `reactions.review_comments.poll_interval_ms`: integer, default `120000` (2 min); minimum `30000`
- `reactions.review_comments.debounce_ms`: integer, default `60000` (60 sec); non-negative
- `reactions.review_comments.max_continuation_turns`: integer, default `3`; positive
- `self_review.enabled`: boolean, default `false`; activates the self-review loop
- `self_review.max_iterations`: integer, default `3`, range [1, 10]; review iteration cap
- `self_review.verification_commands`: list of strings, required when enabled; shell commands
- `self_review.verification_timeout_ms`: integer, default `120000`; per-command timeout
- `self_review.max_diff_bytes`: integer, default `102400`; diff truncation limit
- `self_review.reviewer`: string, default `"same"`; only `"same"` supported in v1
- `server.port` (extension): integer, optional; overrides the default server port (7678);
  `0` disables the HTTP server; CLI `--port` takes precedence
- `server.host` (extension): string (IP address), optional; overrides the default bind
  address (`127.0.0.1`); must be a parseable IP; CLI `--host` takes precedence; restart
  required
- `db_path`: path, default `.sortie.db` next to `WORKFLOW.md`; supports `$VAR` and `~`
  expansion; requires restart to take effect

## 7. Orchestration State Machine

The orchestrator is the only component that mutates scheduling state. All worker outcomes are
reported back to it and converted into explicit state transitions.

### 7.1 Issue Orchestration States

This is not the same as tracker states (`To Do`, `In Progress`, etc.). This is the service's
internal claim state.

1. `Unclaimed`
   - Issue is not running and has no retry scheduled.

2. `Claimed`
   - Orchestrator has reserved the issue to prevent duplicate dispatch.
   - In practice, claimed issues are either `Running` or `RetryQueued`.

3. `Running`
   - Worker task exists and the issue is tracked in `running` map.

4. `RetryQueued`
   - Worker is not running, but a retry timer exists in `retry_attempts`.

5. `Released`
   - Claim removed because issue is terminal, non-active, missing, or retry path completed without
     re-dispatch.

Important nuance:

- A successful worker exit does not mean the issue is done forever.
- The worker may continue through multiple back-to-back coding-agent turns before it exits.
- After each normal turn completion, the worker re-checks the tracker issue state.
- If the issue is still in an active state, the worker should start another turn on the same live
  coding-agent thread in the same workspace, up to `agent.max_turns`.
- The first turn should use the full rendered task prompt.
- Continuation turns should send only continuation guidance to the existing thread, not resend the
  original task prompt that is already present in thread history.
- Once the worker exits normally, the orchestrator still schedules a short continuation retry
  (about 1 second) so it can re-check whether the issue remains active and needs another worker
  session.

### 7.2 Run Attempt Lifecycle

A run attempt transitions through these phases:

1. `PreparingWorkspace`
2. `BuildingPrompt`
3. `LaunchingAgentProcess`
4. `InitializingSession`
5. `StreamingTurn`
6. `SelfReviewing` — entered only when `self_review.enabled` is true and the coding turn
   loop completed successfully (not on turn failure).
7. `Finishing`
8. `Succeeded`
9. `Failed`
10. `TimedOut`
11. `Stalled`
12. `CanceledByReconciliation`

Distinct terminal reasons are important because retry logic and logs differ.

### 7.3 Transition Triggers

- `Poll Tick`
  - Reconcile active runs.
  - Validate config.
  - Fetch candidate issues.
  - Dispatch until slots are exhausted.
  - Dispatched workers perform the optional dispatch-time in-progress transition
    (via `tracker.in_progress_state`) as their first step, before workspace preparation.

- `Worker Exit (normal)`
  - Remove running entry.
  - Update aggregate runtime totals.
  - Persist completed run attempt to SQLite.
  - Schedule continuation retry (attempt `1`) after the worker exhausts or finishes its in-process
    turn loop.
  - When a CI status provider is configured, the workspace contains SCM metadata
    (`.sortie/scm.json` with a non-empty `branch`), and the issue is still claimed: record a
    pending CI check entry for reconciliation.
  - When an SCM adapter is configured, the workspace contains SCM metadata with
    `pr_number > 0`, non-empty `owner`, and non-empty `repo`, and the issue is still claimed:
    record a pending review comment entry for reconciliation. Only created if no entry already
    exists (preserves in-progress debounce state).

- `Worker Exit (abnormal)`
  - Remove running entry.
  - Update aggregate runtime totals.
  - Persist completed run attempt to SQLite.
  - Schedule exponential-backoff retry.

- `Agent Update Event`
  - Update live session fields, token counters, and rate limits.

- `Retry Timer Fired`
  - Re-fetch active candidates and attempt re-dispatch, or release claim if no longer eligible.

- `Reconciliation State Refresh`
  - Stop runs whose issue states are terminal or no longer active.

- `Stall Timeout`
  - Kill worker and schedule retry.

- `CI Status Failing`
  - Persist CI failure run history.
  - Increment CI fix attempt counter.
  - If within `ci_feedback.max_retries` (or `reactions.ci_failure.max_retries`): cancel the
    existing continuation retry, schedule a CI-fix dispatch with failure context injected into
    the prompt.
  - If retries exhausted: escalate (add label or post comment per escalation config),
    cancel retry, release claim.

- `Review Comments Detected`
  - Compute fingerprint from non-outdated review comment IDs.
  - If fingerprint is unchanged and already dispatched: skip.
  - If within debounce window: defer to next tick.
  - If within `reactions.review_comments.max_continuation_turns`: cancel the existing
    continuation retry, schedule a review-fix dispatch with review comment context injected
    into the prompt.
  - If continuation turns exhausted: escalate (add label or post comment per escalation
    config), cancel retry, release claim.

### 7.4 Idempotency and Recovery Rules

- The orchestrator serializes state mutations through one authority to avoid duplicate dispatch.
- `claimed` and `running` checks are required before launching any worker.
- Reconciliation runs before dispatch on every tick.
- Restart recovery uses persisted state from SQLite for retry queues and session metadata,
  supplemented by tracker polling for current issue states and filesystem inspection for workspace
  existence.
- Startup terminal cleanup removes stale workspaces for issues already in terminal states.

#### Startup Recovery Sequence (SQLite)

1. Open or create the SQLite database and run schema migrations.
2. Load persisted retry entries from SQLite.
3. Reconstruct retry timers from persisted `due_at` timestamps.
4. Query tracker for terminal-state issues and clean corresponding workspaces.
5. Query tracker for active issues and reconcile with persisted state.
6. Begin normal polling loop.

## 8. Polling, Scheduling, and Reconciliation

### 8.1 Poll Loop

At startup, Sortie validates config, performs startup cleanup, schedules an immediate tick, and
then repeats every `polling.interval_ms`.

The effective poll interval should be updated when workflow config changes are re-applied.

Tick sequence:

1. Reconcile running issues.
2. Run dispatch preflight validation.
3. Fetch candidate issues from tracker using active states.
4. Sort issues by dispatch priority.
5. Dispatch eligible issues while slots remain.
6. Notify observability/status consumers of state changes.

If per-tick validation fails, dispatch is skipped for that tick, but reconciliation still happens
first.

### 8.2 Candidate Selection Rules

An issue is dispatch-eligible only if all are true:

- It has `id`, `identifier`, `title`, and `state`.
- Its state is in `active_states` and not in `terminal_states`.
- It is not already in `running`.
- It is not already in `claimed`.
- Global concurrency slots are available.
- Per-state concurrency slots are available.
- Blocker rule passes: for issues in any non-running active state, do not dispatch when any blocker
  is non-terminal. The blocker-gating states are the configured active states, not a hardcoded
  state name.

Sorting order (stable intent):

1. `priority` ascending (1..4 are preferred; null/unknown sorts last)
2. `created_at` oldest first
3. `identifier` lexicographic tie-breaker

### 8.3 Concurrency Control

Global limit:

- `available_slots = max(max_concurrent_agents - running_count, 0)`

Per-state limit:

- `max_concurrent_agents_by_state[state]` if present (state key normalized)
- otherwise fallback to global limit

The runtime counts issues by their current tracked state in the `running` map.

Optional SSH host limit:

- When `worker.max_concurrent_agents_per_host` is set, each configured SSH host may run at most
  that many concurrent agents at once.
- Hosts at that cap are skipped for new dispatch until capacity frees up.

### 8.4 Retry and Backoff

Retry entry creation:

- Cancel any existing retry timer for the same issue.
- Store `attempt`, `identifier`, `error`, `due_at_ms`, and new timer handle.

Backoff formula:

- Normal continuation retries after a clean worker exit use a short fixed delay of `1000` ms.
- Failure-driven retries use `delay = min(10000 * 2^(attempt - 1), agent.max_retry_backoff_ms)`.
- Power is capped by the configured max retry backoff (default `300000` / 5m).

Retry handling behavior:

1. Fetch active candidate issues (not all issues).
2. Find the specific issue by `issue_id`.
3. If not found, release claim.
4. If found and still candidate-eligible:
   - Dispatch if slots are available.
   - Otherwise requeue with error `no available orchestrator slots`.
5. If found but no longer active, release claim.

Per-issue effort budget (defense-in-depth):

- When `agent.max_sessions > 0`, the retry handler counts completed sessions for the issue
  from `run_history` before fetching candidates.
- If the count reaches `max_sessions`, the claim is released and a warning is logged instead
  of re-dispatching.
- If the count query fails, the budget check is skipped (fail-open) and dispatch proceeds
  normally.
- `max_sessions = 0` (default) disables the budget entirely.

Note:

- Terminal-state workspace cleanup is handled by startup cleanup and active-run reconciliation
  (including terminal transitions for currently running issues).
- Retry handling mainly operates on active candidates and releases claims when the issue is absent,
  rather than performing terminal cleanup itself.

### 8.5 Active Run Reconciliation

Reconciliation runs every tick and has four parts.

Part A: Stall detection

- For each running issue, compute `elapsed_ms` since:
  - `last_agent_timestamp` if any event has been seen, else
  - `started_at`
- If `elapsed_ms > agent.stall_timeout_ms`, terminate the worker and queue a retry.
- If `stall_timeout_ms <= 0`, skip stall detection entirely.

Part B: Tracker state refresh

- Fetch current issue states for all running issue IDs.
- For each running issue:
  - If tracker state is terminal: terminate worker and clean workspace.
  - If tracker state is still active: update the in-memory issue snapshot.
  - If tracker state is neither active nor terminal: terminate worker without workspace cleanup.
- If state refresh fails, keep workers running and try again on the next tick.

Part C: CI status reconciliation (when `ci_feedback.kind` or `reactions.ci_failure` is configured)

- For each entry in `pending_reactions` with kind `ci`:
  - Call `CIStatusProvider.FetchCIStatus` with the SCM ref (SHA preferred, branch as fallback).
  - If the call fails: log a warning, re-enqueue the entry, and continue to the next entry.
  - If status is `passing`: clear reaction attempts for the issue and kind.
  - If status is `pending`: re-enqueue the entry for the next tick.
  - If status is `failing`: handle as a CI failure (see Section 7.3, "CI Status Failing").

Part D: Review comment reconciliation (when `reactions.review_comments` is configured)

- Skip entirely when no SCM adapter is configured (no `reactions.review_comments.provider`).
- For each entry in `pending_reactions` with kind `review`:
  - Remove entry from the map (prevents reprocessing within the same tick).
  - Respect `PendingRetryAt` poll throttle: if not yet due, re-enqueue and continue.
  - Check continuation turn cap (`reactions.review_comments.max_continuation_turns`): if
    exceeded, escalate (Section 11B.4) and continue.
  - Call `SCMAdapter.FetchPendingReviews` with the PR number, owner, and repo from
    `ReviewReactionData`.
  - If the call fails: increment backoff counter, set `PendingRetryAt` with exponential
    backoff, re-enqueue, and continue.
  - Filter out outdated comments. Compute max timestamp for debounce gating.
  - If no actionable comments: re-enqueue with poll interval delay.
  - Build fingerprint from sorted non-outdated comment IDs (SHA-256 hash).
  - Check `reaction_fingerprints` table: if fingerprint matches and is marked dispatched, skip.
  - If within debounce window (`now - LastEventAt < debounce_ms`): defer and re-enqueue.
  - Otherwise: mark dispatched in `reaction_fingerprints`, cancel existing retry, schedule a
    review-fix dispatch with review comment context, increment `reaction_attempts`.

### 8.6 Startup Terminal Workspace Cleanup

When Sortie starts:

1. Enumerate workspace directories on disk.
2. Map directory names back to issue identifiers.
3. Query the tracker for the states of those specific issues.
4. For each issue in a terminal state, remove the corresponding workspace directory.
5. If the terminal-issues fetch fails, log a warning and continue startup.

This approach scopes the query to workspaces that actually exist on disk, avoiding expensive
full-project terminal issue sweeps for large trackers.

## 9. Workspace Management and Safety

### 9.1 Workspace Layout

Workspace root:

- `workspace.root` (normalized path; the current config layer expands path-like values and preserves
  bare relative names)

Per-issue workspace path:

- `<workspace.root>/<sanitized_issue_identifier>`

Workspace persistence:

- Workspaces are reused across runs for the same issue.
- Successful runs do not auto-delete workspaces.

### 9.2 Workspace Creation and Reuse

Input: `issue.identifier`

Algorithm summary:

1. Sanitize identifier to `workspace_key`.
2. Compute workspace path under workspace root.
3. Ensure the workspace path exists as a directory.
4. Mark `created_now=true` only if the directory was created during this call; otherwise
   `created_now=false`.
5. If `created_now=true`, run `after_create` hook if configured.

Notes:

- This section does not assume any specific repository/VCS workflow.
- Workspace preparation beyond directory creation (for example dependency bootstrap, checkout/sync,
  code generation) is implementation-defined and is typically handled via hooks.

### 9.3 Optional Workspace Population (Implementation-Defined)

Sortie does not require any built-in VCS or repository bootstrap behavior.

Implementations may populate or synchronize the workspace using implementation-defined logic and/or
hooks (for example `after_create` and/or `before_run`).

Failure handling:

- Workspace population/synchronization failures return an error for the current attempt.
- If failure happens while creating a brand-new workspace, implementations may remove the partially
  prepared directory.
- Reused workspaces should not be destructively reset on population failure unless that policy is
  explicitly chosen and documented.

### 9.4 Workspace Hooks

Supported hooks:

- `hooks.after_create`
- `hooks.before_run`
- `hooks.after_run`
- `hooks.before_remove`

Execution contract:

- Execute in a local shell context appropriate to the host OS, with the workspace directory as
  `cwd`.
- On POSIX systems, `sh -c <script>` is the conforming default; `bash -lc <script>` may be used
  when a login shell environment is required.
- On Windows, `cmd.exe /C <script>` is the conforming default. The hook subprocess is assigned to
  a Job Object with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` so that timeout-triggered termination
  kills the entire process tree, not just the direct child.
- Hook timeout uses `hooks.timeout_ms`; default: `60000 ms`.
- Log hook start, failures, and timeouts.

Failure semantics:

- `after_create` failure or timeout is fatal to workspace creation.
- `before_run` failure or timeout is fatal to the current run attempt.
- `after_run` failure or timeout is logged and ignored.
- `before_remove` failure or timeout is logged and ignored.

Hook environment variables available to all hooks:

- `SORTIE_ISSUE_ID` — tracker-internal issue ID.
- `SORTIE_ISSUE_IDENTIFIER` — human-readable ticket key.
- `SORTIE_WORKSPACE` — absolute path to the per-issue workspace directory.
- `SORTIE_ATTEMPT` — current attempt number.

Hook environment variables available only to `after_run`:

- `SORTIE_SELF_REVIEW_STATUS` — self-review outcome for the current run: `"disabled"`,
  `"passed"`, `"cap_reached"`, or `"error"`. Defaults to `"disabled"` when self-review
  is not configured.
- `SORTIE_SELF_REVIEW_SUMMARY_PATH` — absolute path to `.sortie/review_summary.md` in
  the workspace. Absent when self-review did not run or summary was not written.

### 9.5 Workspace SCM metadata (`.sortie/scm.json`)

The `.sortie/scm.json` file is a workspace-level file that carries SCM metadata written by the
agent, a post-push hook, or any process running inside the workspace. The orchestrator reads this
file after a normal worker exit to determine the git ref for CI status queries and the PR identity
for review comment polling. The file is a shared workspace-level SCM metadata contract that CI
feedback, review comment routing, and other features can reuse.

`SCMMetadata` fields:

- `branch` (string, required for CI): the branch name (e.g. `feature/PROJ-42`). If empty or
  absent, the file is treated as missing and CI status queries are skipped.
- `sha` (string, optional): the commit SHA at push time. When present, the orchestrator passes
  this to `CIStatusProvider.FetchCIStatus` instead of the branch name for deterministic results.
- `pushed_at` (string, optional): ISO-8601 timestamp of the push. This field is reserved metadata
  for producers and future consumers of `.sortie/scm.json`. The orchestrator does not use it for
  CI gating today.
- `pr_number` (integer, optional): the pull request number associated with this branch. Zero or
  absent when no PR has been created. Written by the agent or post-push hook. When positive and
  `owner` and `repo` are non-empty, the orchestrator creates a pending review comment reaction
  on normal worker exit. Review polling is skipped when `pr_number` is `0`.
- `owner` (string, optional): the SCM repository owner (e.g. GitHub org or user). Written by
  the agent alongside `pr_number`. Required for review comment polling; when empty, review
  polling is skipped. The `owner` field is the authoritative source of SCM repository identity —
  it is never derived from the tracker project configuration, which may be a Jira key or other
  non-SCM identifier.
- `repo` (string, optional): the SCM repository name. Written by the agent alongside
  `pr_number`. Required for review comment polling; when empty, review polling is skipped.

Safety and parsing rules:

- Maximum file size: 4096 bytes. Oversized files are rejected and logged at warn level.
- Symlink rejection: both `.sortie/` and `.sortie/scm.json` are checked via `Lstat` before
  reading. If either is a symbolic link, the file is rejected and logged at warn level. This
  prevents symlink-based path escape attacks.
- Malformed JSON is rejected and logged at warn level.
- The function never returns an error to the caller; all failure modes degrade gracefully to a
  zero-value metadata struct (CI queries are skipped).

### 9.6 Safety Invariants

This is the most important portability constraint.

Invariant 1: Run the coding agent only in the per-issue workspace path.

- Before launching the coding-agent subprocess, validate:
  - `cwd == workspace_path`

Invariant 2: Workspace path must stay inside workspace root.

- Normalize both paths to absolute.
- Require `workspace_path` to have `workspace_root` as a prefix directory.
- Reject any path outside the workspace root.

Invariant 3: Workspace key is sanitized.

- Only `[A-Za-z0-9._-]` allowed in workspace directory names.
- Replace all other characters with `_`.

## 10. Agent Adapter Contract

This section defines the interface contract that all agent adapters must satisfy. Adapter-specific
protocol details (handshake sequences, JSON-RPC framing, stdio vs. HTTP transport) are documented
separately per adapter.

### 10.1 Agent Adapter Interface

An agent adapter must implement the following operations:

- `StartSession(workspace, config) -> Session`
  - Launch or connect to an agent process/service in the given workspace.
  - Returns an opaque session handle.
- `RunTurn(session, prompt, issue, on_event) -> TurnResult`
  - Execute one agent turn with the given prompt.
  - Delivers events to the orchestrator via `on_event` callback (push adapters) or returns
    them in the result (synchronous adapters).
  - Returns when the turn completes (success, failure, or timeout).
- `StopSession(session)`
  - Terminate the agent process/service cleanly.
- `EventStream() -> <event channel>`
  - Optional: adapters that push events asynchronously may expose an event channel.

### 10.2 Session Lifecycle

The orchestrator interacts with an agent session as follows:

1. Call `StartSession` before the first turn.
2. Call `RunTurn` for each turn. Continuation turns reuse the same session.
3. Call `StopSession` when the worker run is ending.

The session handle and any session identifiers are adapter-specific. The orchestrator treats
`session_id` as an opaque string.

### 10.3 Normalized Event Types

The orchestrator expects the following event types from any agent adapter. Adapters map their
native protocol events to this normalized set:

- `session_started` — session initialized successfully
- `startup_failed` — session could not be initialized
- `turn_completed` — turn finished successfully
- `turn_failed` — turn finished with failure
- `turn_cancelled` — turn was cancelled
- `turn_ended_with_error` — turn ended due to an error condition
- `turn_input_required` — agent requested user input (hard failure per policy)
- `approval_auto_approved` — approval request was auto-resolved
- `unsupported_tool_call` — agent requested an unsupported tool
- `token_usage` — normalized token usage event: `{input_tokens, output_tokens, total_tokens, cache_read_tokens}`. Optional `model` field (string) identifies the LLM model when available. Optional `api_duration_ms` field (int64, milliseconds) carries per-request or per-turn API response wait time when the adapter can measure it.
- `tool_result` — a tool call completed. Optional fields: `tool_name` (string), `duration_ms` (int64).
- `notification` — informational message from the agent
- `other_message` — unclassified message
- `malformed` — unparseable or unrecognized message

Each event should include:

- `event` (enum/string)
- `timestamp` (UTC timestamp)
- `agent_pid` (if available)
- optional `usage` map: `{input_tokens, output_tokens, total_tokens, cache_read_tokens}`
- optional `model` string: LLM model identifier when available
- payload fields as needed

Token accounting is normalized at the adapter boundary. The orchestrator receives
`{input_tokens, output_tokens, total_tokens, cache_read_tokens}` directly and does not parse
adapter-specific payload shapes.

### 10.4 Approval, Tools, and User Input Policy

This section covers the approval posture, tool subsystem, and user-input handling for agent
sessions.

#### 10.4.1 Approval policy

Approval, sandbox, and user-input behavior is implementation-defined.

Policy requirements:

- Each deployment MUST document its chosen approval, sandbox, and operator-confirmation posture.
- Approval requests and user-input-required events MUST NOT leave a run stalled indefinitely.
  Sortie MUST either satisfy them, surface them to an operator, auto-resolve them, or fail the
  run according to its documented policy.

Example high-trust behavior:

- Auto-approve command execution approvals for the session.
- Auto-approve file-change approvals for the session.
- Treat user-input-required turns as hard failure.

Unsupported dynamic tool calls:

- If the agent adapter receives a tool call request for a name not in the `ToolRegistry`, the
  adapter returns a tool failure response and continues the session.
- This is adapter-level behavior; the orchestrator does not intercept tool call routing.

Hard failure on user input requirement:

- If the agent requests user input, fail the run attempt immediately.

#### 10.4.2 Tool interface contract

All tools that Sortie exposes to agents implement the `AgentTool` interface
(`internal/domain/tool.go`):

- `Name() string` — stable tool identifier used for matching tool call requests to
  implementations. MUST be unique within a `ToolRegistry`.
- `Description() string` — human-readable summary suitable for inclusion in agent prompts and
  MCP `tools/list` responses.
- `InputSchema() json.RawMessage` — JSON Schema describing the tool's expected input. Used for
  MCP tool registration and prompt-based documentation.
- `Execute(ctx context.Context, input json.RawMessage) (json.RawMessage, error)` — runs the tool
  and returns a structured JSON result. Domain-level errors (missing auth, API failures, invalid
  input) are encoded in the JSON result with `success: false`. The Go `error` return is reserved
  for internal failures (nil adapter, marshal failure) that indicate programming errors.

#### 10.4.3 Tool registry

`ToolRegistry` (`internal/domain/tool.go`) is the central registration point for all agent tools.

Invariants:

- Registration is static at build time. All tools are registered during orchestrator
  initialization, before the first dispatch. No dynamic plugin loading.
- The registry is safe for concurrent reads after construction. Concurrent `Register` + `Get` is
  a data race; callers MUST NOT call `Register` after passing the registry to the orchestrator.
- Duplicate names panic (programming error, not runtime input).
- The registry feeds prompt-time tool advertisement. The runtime execution channel through
  which agents invoke tools at call time (e.g., MCP sidecar, HTTP, in-process) is not yet
  designed. See issue #224 for the execution channel design discussion.

#### 10.4.4 Tool tiers

Tools are classified by their dependency profile. The tier determines security posture, test
strategy, and failure characteristics.

**Tier 1 — pure orchestrator state.** These tools read from local orchestrator state (in-memory
session context or SQLite database) with zero external calls. They are deterministic, fast, and
have no failure modes beyond internal bugs. No Tier 1 tools are currently implemented. See
issues #226 and #227 for planned tools.

**Tier 2 — external dependencies.** These tools interact with external services (tracker APIs,
future SCM APIs) through network calls using orchestrator-managed credentials. They are subject
to transport failures, authentication errors, rate limits, and per-tool timeouts.

- `tracker_api` (Section 10.4.5)

Future tools follow the same classification.

#### 10.4.5 Built-in tool: `tracker_api`

`tracker_api` is a Tier 2 tool that executes queries and mutations against the configured issue
tracker using the orchestrator's tracker credentials.

Availability: only meaningful when valid tracker auth is configured. When auth is absent, the
tool SHOULD NOT be registered.

Project scoping: the tool is scoped to the configured project. An agent working on project PROJ
MUST NOT be able to query or mutate issues in unrelated projects through this passthrough tool.

Supported operations:

| Operation | Required fields | Description |
|---|---|---|
| `fetch_issue` | `issue_id` | Fetch a single issue by ID |
| `fetch_comments` | `issue_id` | Fetch comments for an issue |
| `search_issues` | (none) | Return issues in configured active states |
| `transition_issue` | `issue_id`, `target_state` | Transition an issue to a target state |

The `TrackerAdapter.CommentIssue` method exists on the adapter interface but is not yet exposed
through `tracker_api`.

Tracker dispatch:

The tool delegates to the configured `TrackerAdapter` implementation. Transport, input shape,
and query semantics are adapter-defined.

Result semantics:

- Transport success + no API-level errors -> `success: true` with response payload.
- API-level errors -> `success: false` with a normalized error envelope
  `{"success": false, "error": {"kind": "...", "message": "..."}}`.
- Invalid input, missing auth, or transport failure -> `success: false` with the same normalized
  error envelope (`error.kind` indicates the failure category).

The response payload or error envelope is returned as structured JSON that the agent can inspect
in-session.

#### 10.4.6 Tools vs. agent-authored files

The tool subsystem (this section) and the `.sortie/status` file protocol (Section 21) are
independent communication channels between agents and the orchestrator. They address different
concerns, operate on different transports, and have deliberately different failure
characteristics. This separation is a design choice, not an implementation accident.

**Communication patterns.**

| Property | Agent tools | `.sortie/status` file |
|---|---|---|
| Direction | Agent <-> Orchestrator (request-response) | Agent -> Orchestrator (one-way advisory) |
| Transport | Tool call (mechanism TBD per Section 10.4.3) | Filesystem sentinel file |
| Timing | Synchronous, during a turn | Asynchronous, read after turn completes |
| Purpose | Data access (tracker queries, orchestrator state) | Control flow (retry suppression, soft stop) |
| Failure mode | Tool call fails; agent receives error and continues | File absent or unreadable; orchestrator proceeds normally |
| Agent requirement | MCP client or equivalent tool-calling capability | Write a file to disk (`echo "blocked" > .sortie/status`) |

**Why two channels exist.** The channels serve orthogonal roles:

- Tools are the **data plane**: the agent requests information or performs a mutation and
  receives a structured result within the same turn. The agent needs the response to continue
  its work.
- The `.sortie/status` file is the **control plane**: the agent advises the orchestrator about
  task feasibility after the turn completes. The orchestrator uses this signal to suppress
  continuation retries. No response flows back to the agent.

Collapsing both into a single MCP-based channel was evaluated and rejected during the A2O
protocol design (see `docs/agent-to-orchestrator-protocol.md`, Section 5.1, Alternative 2).
The MCP approach fails the agent-agnostic requirement: an agent without MCP client support
cannot send the control signal. The file-based channel satisfies all six A2O requirements
(agent-agnostic, fail-safe, advisory, zero-dependency, forward-compatible, inspectable)
simultaneously; no tool-call-based mechanism achieves this.

**Coexistence.** An agent MAY use both channels in the same session. Typical sequence:

1. Agent calls a tool (e.g., `tracker_api.fetch_issue`) to gather context.
2. Agent determines the task requires a human architectural decision.
3. Agent writes `mkdir -p .sortie && echo "blocked" > .sortie/status`.
4. Turn completes; orchestrator reads the status file and suppresses retries.

The two channels do not interact. A tool call cannot write to `.sortie/status` on behalf of
the agent, and the `.sortie/status` file cannot trigger tool execution. The orchestrator
processes them at different points in the worker lifecycle: tool calls during the turn (via the
execution channel), status file after the turn (Section 21.1, read timing per
`agent-to-orchestrator-protocol.md` Section 3.1).

**Defense in depth.** The independence of the two channels provides resilience. If the MCP
execution channel is unavailable (not yet implemented, sidecar crash, agent lacks MCP support),
the file-based advisory signal still functions. If the filesystem is read-only or the workspace
is on a remote host with restricted write access, tool calls still function. Neither channel is
a single point of failure for the other.

### 10.5 Timeouts and Error Mapping

Timeouts:

- `agent.read_timeout_ms`: request/response timeout during startup and sync requests
- `agent.turn_timeout_ms`: total turn stream timeout
- `agent.stall_timeout_ms`: enforced by orchestrator based on event inactivity

Error mapping (recommended normalized categories):

- `agent_not_found`
- `invalid_workspace_cwd`
- `response_timeout`
- `turn_timeout`
- `port_exit`
- `response_error`
- `turn_failed`
- `turn_cancelled`
- `turn_input_required`

### 10.6 Agent Runner Contract

The `Agent Runner` wraps workspace + prompt + agent adapter.

Behavior:

1. Create/reuse workspace for issue.
2. Build prompt from workflow template.
3. Start agent session via adapter.
4. Relay agent events to orchestrator.
5. On any error, fail the worker attempt (the orchestrator will retry).

Note:

- Workspaces are intentionally preserved after successful runs.

### 10.7 Local Subprocess Launch Contract

This subsection applies only to adapters that launch a local subprocess (e.g., Claude Code,
Copilot CLI). HTTP-based and remote adapters define their own connection semantics.

When `agent.kind` requires a local subprocess:

- Command: `agent.command`
- Invocation:
  - POSIX: `sh -c <agent.command>` (or `bash -lc` when a login shell is required by the agent).
    The shell used for invocation is configurable to support minimal Docker images and CI
    environments where bash may not be present.
  - Windows: the adapter invokes the command directly (no shell wrapper). The subprocess receives
    `CREATE_NEW_PROCESS_GROUP` so it can be signaled independently of the orchestrator.
- Working directory: workspace path
- Stdout/stderr: separate streams

Process group isolation:

- The adapter MUST place the subprocess in its own process group before starting it.
  - POSIX: `Setpgid = true` (new process group at fork time).
  - Windows: `CREATE_NEW_PROCESS_GROUP` creation flag, followed by Job Object assignment after
    process start. The Job Object is configured with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` so
    the entire process tree is terminated if the orchestrator crashes.

Graceful shutdown sequence:

- On context cancellation or `StopSession`, the adapter sends a platform-appropriate graceful
  shutdown signal to the process group:
  - POSIX: `SIGTERM` to the process group (`kill(-pgid, SIGTERM)`).
  - Windows: `CTRL_BREAK_EVENT` via `GenerateConsoleCtrlEvent` to the process group.
- After a grace period (default 5 seconds), force-terminate the process tree:
  - POSIX: `SIGKILL` to the process group.
  - Windows: `TerminateJobObject` to kill all processes in the Job Object.
- After `cmd.Wait()` returns, a best-effort force kill is sent to the process group to reap any
  children that survived the graceful signal.

Recommended additional process settings:

- Max line size: 10 MB (for safe buffering)

## 11. Issue Tracker Integration Contract

### 11.1 Required Operations

A tracker adapter must support these operations:

1. `fetch_candidate_issues()`
   - Return issues in configured active states for the configured project.

2. `fetch_issue_by_id(issue_id)`
   - Return a single fully-populated issue including comments and attachments. Used for
     pre-dispatch revalidation and prompt rendering.

3. `fetch_issues_by_states(state_names)`
   - Used for startup terminal cleanup.

4. `fetch_issue_states_by_ids(issue_ids)`
   - Used for active-run reconciliation.

5. `fetch_issue_states_by_identifiers(identifiers)`
   - Return the current state for each requested issue identifier (human-readable key).
     Issues not found are omitted. Used for startup terminal workspace cleanup.

6. `fetch_issue_comments(issue_id)`
   - Return comments for an issue. Used for continuation runs and the agent workpad pattern.

7. `transition_issue(issue_id, target_state)`
   - Transition an issue to the named target state in the tracker's native workflow system.
   - Used for orchestrator-initiated handoff transitions (ADR-0007).
   - Error semantics: returns normalized error categories per Section 11.4.
   - All errors are non-fatal from the orchestrator's perspective.

### 11.2 Query Semantics

Each tracker adapter defines its own query semantics. The architecture specifies only the
normalized interface and the minimum field set that must be returned (see Section 4.1.1). Adapters
are responsible for translating their native API responses to the normalized issue model.

Pagination is required for candidate issue fetches. Page size default: `50`. Network timeout:
`30000 ms`.

### 11.3 Normalization Rules

Candidate issue normalization should produce fields listed in Section 4.1.1.

Additional normalization details:

- `labels` -> lowercase strings
- `blocked_by` -> derived from inverse relations where relation type is `blocks`
- `priority` -> integer only (non-integers become null)
- `created_at` and `updated_at` -> parse ISO-8601 timestamps

### 11.4 Error Handling Contract

Recommended error categories:

- `unsupported_tracker_kind`
- `missing_tracker_api_key`
- `missing_tracker_project`
- `tracker_transport_error` (transport failures)
- `tracker_auth_error` (authentication/authorization failures)
- `tracker_api_error` (non-200 HTTP or API-level error)
- `tracker_payload_error` (malformed or unexpected response structure)
- `tracker_missing_end_cursor` (pagination integrity error)

Each adapter maps its native errors to these categories.

Orchestrator behavior on tracker errors:

- Candidate fetch failure: log and skip dispatch for this tick.
- Running-state refresh failure: log and keep active workers running.
- Startup terminal cleanup failure: log warning and continue startup.

### 11.5 Tracker Writes (Important Boundary)

Sortie does not require first-class tracker write APIs in the orchestrator.

- Ticket mutations (state transitions, comments, PR metadata) are typically handled by the coding
  agent using tools defined by the workflow prompt.
- Sortie remains a scheduler/runner and tracker reader.
- Workflow-specific success often means "reached the next handoff state" (for example
  `Human Review`) rather than tracker terminal state `Done`.
- The agent tool subsystem (Section 10.4) is part of the agent toolchain rather than
  orchestrator business logic. `tracker_api` executes tracker operations through agent-initiated
  tool calls, not orchestrator-driven writes.

## 11A. CI Feedback Contract

This section defines the CI status provider interface and the orchestrator's CI feedback loop.
The CI feedback system is a read-only integration: it queries CI pipeline status for git refs
and injects failure context into agent continuation prompts. It does not trigger builds, manage
pipelines, or write CI configuration.

### Naming convention

CI status providers use the `*Provider` suffix rather than `*Adapter`. The distinction is
intentional: a provider is a read-only, single-method contract (`FetchCIStatus`), while an adapter
(tracker, agent) manages a full lifecycle with multiple operations and bidirectional state. The
naming signals to implementers that the contract surface is minimal.

### 11A.1 CIStatusProvider interface

```go
type CIStatusProvider interface {
    FetchCIStatus(ctx context.Context, ref string) (CIResult, error)
}
```

- `ref` is a git ref string (branch name or commit SHA). Adapters that require a full commit SHA
  MUST resolve branch names to SHAs internally.
- Returns a `CIResult` on success or a `*CIError` on failure. All error categories are non-fatal
  from the orchestrator's perspective.
- Implementations MUST be safe for concurrent use. The orchestrator's reconcile loop may call
  `FetchCIStatus` for multiple workspaces concurrently.

### 11A.2 CIResult structure

```text
CIResult:
  status:        CIStatus         # aggregate pipeline status
  check_runs:    []CheckRun       # individual check runs
  log_excerpt:   string           # truncated log from first failing check
  failing_count: int              # precomputed count of failing check runs
  ref:           string           # echoed git ref for observability
```

`CIStatus` is an enum with three values:

| Value | Meaning |
|-------|---------|
| `pending` | CI checks are still running or no checks have been reported. |
| `passing` | All checks completed successfully. |
| `failing` | At least one check completed with a failure conclusion. |

Each `CheckRun` contains:

- `name`: check name as defined by the CI platform (e.g. `test`, `lint`, `build`).
- `status`: execution status (`queued`, `in_progress`, `completed`).
- `conclusion`: normalized outcome (`success`, `failure`, `cancelled`, `timed_out`, `neutral`,
  `skipped`, `pending`). Meaningful only when status is `completed`. Unknown or unmappable
  platform conclusions map to `pending`.
- `details_url`: web URL to the check run's detail page. Empty string when unavailable.

`log_excerpt` handling:

- CI logs may contain secrets accidentally printed by build scripts. Adapters MUST truncate to
  the configured `max_log_lines` and SHOULD strip ANSI escape sequences.
- Consumers MUST NOT persist log excerpts to the database or expose them via unauthenticated API
  endpoints.
- The orchestrator omits the log section from the continuation prompt when `log_excerpt` is empty.

`CIResult.ToTemplateMap()` converts the result to a `map[string]any` with snake_case keys
(`status`, `check_runs`, `log_excerpt`, `failing_count`, `ref`) for use as the `ci_failure`
prompt template variable (Section 12.1).

### 11A.3 CIError type

```text
CIError:
  kind:    CIErrorKind    # normalized error category
  message: string         # operator-friendly description
  err:     error          # underlying error (may be nil)
```

Error categories:

| Kind | Meaning |
|------|---------|
| `ci_transport_error` | Network or transport failure (connection, DNS, TLS). |
| `ci_auth_error` | Authentication or authorization failure (expired token, insufficient scopes). |
| `ci_api_error` | Non-success HTTP status or API-level error (rate limiting, server error). |
| `ci_not_found` | Requested ref or repository does not exist (HTTP 404). |
| `ci_payload_error` | Malformed or unexpected response structure. |

`CIError` implements `Error()` and `Unwrap()` for use with `errors.Is`/`errors.As`.

Orchestrator behavior on CI errors:

- Log a warning with the ref and error category.
- Re-enqueue the pending CI check entry for the next reconciliation tick.
- Increment `sortie_ci_status_checks_total{result="error"}`.

### 11A.4 Reconcile loop integration

CI status reconciliation runs as Part C of active run reconciliation (Section 8.5), after tracker
state refresh. The flow is:

1. Skip entirely when neither `ci_feedback.kind` nor `reactions.ci_failure.provider` is configured
   (no `CIStatusProvider` constructed).
2. For each entry in `pending_reactions` with kind `ci`:
   a. Remove the entry from the map (prevents reprocessing within the same tick).
   b. Call `CIStatusProvider.FetchCIStatus` with the SCM ref (SHA preferred, branch as fallback).
   c. On fetch error: re-enqueue the entry; continue.
   d. On `passing`: clear `reaction_attempts` for the issue and kind.
   e. On `pending`: re-enqueue the entry.
   f. On `failing`: handle CI failure (see Section 11A.5).

### 11A.5 CI failure handling

When CI status is `failing`:

1. Persist a CI-failure run history entry (`status: ci_failed`).
2. Increment `reaction_attempts[issue_id:ci]`.
3. If `reaction_attempts[issue_id:ci]` exceeds `ci_feedback.max_retries`: escalate (Section 11A.6).
4. Otherwise:
   a. Convert `CIResult` to a template map via `ToTemplateMap()`.
   b. Cancel the existing continuation retry for the issue.
   c. Schedule a CI-fix dispatch carrying the CI failure context. The retry entry's
      `ContinuationContext` field carries the map through the timer to the worker goroutine.
   d. The worker injects the context into the prompt on turn 1 via `prompt.WithContinuationContext`.

CI-fix dispatches count toward the regular retry machinery but use a fixed delay rather than
exponential backoff.

### 11A.6 Escalation behavior

When `reaction_attempts[issue_id:ci]` exceeds `ci_feedback.max_retries`:

- `escalation: label` (default): add `escalation_label` (default `needs-human`) to the tracker
  issue via `TrackerAdapter.AddLabel`. The label call runs in a detached goroutine with a 30-second
  timeout.
- `escalation: comment`: post a plain-text comment listing the ref, attempt count, failing check
  names, conclusions, and details URLs. The comment call runs in a detached goroutine with a
  30-second timeout.

After escalation:

- Cancel any pending retry for the issue.
- Delete the persisted retry entry from SQLite.
- Release the claim (`delete claimed[issue_id]`).
- Clear all `reaction_attempts` and `pending_reactions` entries for the issue.

Escalation failures are logged and counted (`sortie_ci_escalations_total{action="error"}`) but do
not block claim release.

### 11A.7 Adapter registration

CI status providers register via the CI provider registry using `init()` functions, following the
same pattern as tracker and agent adapters:

```go
func init() {
    registry.CIProviders.Register("github", NewGitHubCIProvider)
}
```

The `CIProviderConstructor` signature is:

```go
type CIProviderConstructor func(maxLogLines int, adapterConfig map[string]any) (domain.CIStatusProvider, error)
```

The `maxLogLines` parameter comes from `ci_feedback.max_log_lines`. The `adapterConfig` parameter
comes from the `extensions` sub-object keyed by `ci_feedback.kind`. Startup merges tracker
credentials (API key, project, endpoint) into that config only when `tracker.kind` and
`ci_feedback.kind` match.

## 11B. PR Review Comment Feedback Contract

This section defines the SCM adapter interface for PR review comment fetching and the
orchestrator's review comment feedback loop. Review comment routing is a read-only integration: it
queries human review comments on Sortie-created PRs and injects structured context into agent
continuation prompts. It does not create PRs, approve reviews, or resolve comments.

### Naming convention

SCM adapters use the `*Adapter` suffix rather than `*Provider`. The distinction matches the
tracker and agent naming: an adapter manages a broader integration surface with multiple
operations and may carry per-instance state (HTTP client, auth token). The current contract
is single-method (`FetchPendingReviews`) but the interface is designed for future expansion
(e.g. `PostComment`, `RequestReReview`).

### 11B.1 SCMAdapter interface

```go
type SCMAdapter interface {
    FetchPendingReviews(ctx context.Context, prNumber int, owner, repo string) ([]ReviewComment, error)
}
```

- `prNumber` is the pull request number. `owner` and `repo` identify the repository. These
  values are sourced from `SCMMetadata` (written by the agent to `.sortie/scm.json`), never
  from the tracker project configuration.
- Returns a non-nil (possibly empty) `[]ReviewComment` on success or a `*SCMError` on failure.
- Implementations MUST be safe for concurrent use.
- Only comments from `CHANGES_REQUESTED` reviews are returned. Approved reviews, comment-only
  reviews, and bot comments (`user.type == "Bot"`) are excluded.

### 11B.2 ReviewComment structure

```text
ReviewComment:
  id:           string      # SCM-platform comment identifier
  file_path:    string      # file the comment is attached to; empty for PR-level comments
  start_line:   int         # first line of commented range; 0 for non-inline comments
  end_line:     int         # last line of commented range; 0 for single-line or non-inline
  reviewer:     string      # username of the comment author
  body:         string      # comment text
  submitted_at: time.Time   # UTC timestamp from the platform
  outdated:     bool        # true when the commented code was modified by a subsequent push
```

### 11B.3 SCMError type

```text
SCMError:
  kind:    SCMErrorKind    # normalized error category
  message: string          # operator-friendly description
  err:     error           # underlying error (may be nil)
```

Error categories:

| Kind | Meaning |
|------|---------|
| `scm_transport_error` | Network or transport failure. |
| `scm_auth_error` | Authentication or authorization failure. |
| `scm_api_error` | Non-success HTTP status or API-level error. |
| `scm_not_found` | PR or repository does not exist. |
| `scm_payload_error` | Malformed or unexpected response structure. |

`SCMError` implements `Error()` and `Unwrap()` for use with `errors.Is`/`errors.As`.

Orchestrator behavior on SCM errors:

- Log a warning with the PR number and error category.
- Increment backoff counter and set `PendingRetryAt` with exponential backoff.
- Re-enqueue the pending review entry.
- Increment `sortie_review_checks_total{result="error"}`.

### 11B.4 Reconcile loop integration

Review comment reconciliation runs as Part D of active run reconciliation (Section 8.5), after
CI status reconciliation. The flow is:

1. Skip entirely when `reactions.review_comments` is not configured (no `SCMAdapter`
   constructed).
2. For each entry in `pending_reactions` with kind `review`:
   a. Remove the entry from the map (prevents reprocessing within the same tick).
   b. Respect `PendingRetryAt` poll throttle: if `now < PendingRetryAt`, re-enqueue and continue.
   c. Check continuation turn cap: if `reaction_attempts[issue_id:review]` >=
      `max_continuation_turns`, escalate (Section 11B.6) and continue.
   d. Call `SCMAdapter.FetchPendingReviews(ctx, pr_number, owner, repo)`.
   e. On fetch error: increment backoff, set `PendingRetryAt`, re-enqueue, continue.
   f. Filter outdated comments. Compute max `submitted_at` timestamp for debounce.
   g. If no actionable comments: re-enqueue with poll interval delay and continue.
   h. Build fingerprint: `sha256(sorted(comment_id_1, comment_id_2, ...))` of non-outdated IDs.
   i. Upsert fingerprint in `reaction_fingerprints` (kind `review`). If stored fingerprint
      matches and is marked dispatched: skip, re-enqueue with poll interval delay.
   j. If `now - LastEventAt < debounce_ms`: set `PendingRetryAt = LastEventAt + debounce_ms`,
      re-enqueue.
   k. Mark dispatched in `reaction_fingerprints` synchronously (prevents duplicate dispatch on
      entry recreation by concurrent worker exit).
   l. Cancel existing retry for the issue.
   m. Schedule review-fix dispatch with `ContinuationContext{"review_comments": [...]}`.
   n. Increment `reaction_attempts[issue_id:review]`.

### 11B.5 Review comment handling

When actionable review comments are detected and debounce has elapsed:

1. Build a template map from actionable comments (Section 12.1).
2. Cancel existing continuation retry for the issue.
3. Schedule a review-fix dispatch carrying the review comment context via `ContinuationContext`.
4. The worker injects the context into the prompt on turn 1 via `prompt.WithContinuationContext`.

Review-fix dispatches count toward the regular retry machinery but use a fixed delay rather than
exponential backoff.

### 11B.6 Escalation behavior

When `reaction_attempts[issue_id:review]` reaches `max_continuation_turns`:

- `escalation: label` (default): add `escalation_label` (default `needs-human`) to the tracker
  issue via `TrackerAdapter.AddLabel`. The label call runs in a detached goroutine with a 30-second
  timeout.
- `escalation: comment`: post a plain-text comment:
  ```
  Review fix continuation turns exhausted for PR #{pr_number} on branch {branch}.
  {turn_count} continuation turns attempted. Remaining review comments require human attention.
  ```

After escalation:

- Cancel any pending retry for the issue.
- Delete the persisted retry entry from SQLite.
- Release the claim (`delete claimed[issue_id]`).
- Clear all `reaction_attempts` and `pending_reactions` entries for the issue.

Escalation failures are logged and counted (`sortie_review_escalations_total{action="error"}`)
but do not block claim release.

### 11B.7 Fingerprint and debounce

The fingerprint is a deterministic hash of the current set of actionable review comments:

```text
fingerprint = sha256(sorted(comment_id_1, comment_id_2, ...))
```

Only non-outdated comment IDs are included. This means:

- New comments → fingerprint changes → dispatch triggered.
- Comment resolved/outdated → fingerprint changes → dispatch triggered with remaining comments.
- Same comments, no changes → fingerprint unchanged → skip.

The fingerprint is stored in `reaction_fingerprints` (Section 19.2) with kind `review`.

Debounce uses `PendingRetryAt` — the same mechanism as CI pending backoff. When review comments
are detected but the newest comment timestamp is within the debounce window
(`reactions.review_comments.debounce_ms`):

1. Set `LastEventAt` to the maximum `submitted_at` among fetched comments.
2. Set `PendingRetryAt = LastEventAt + debounce_ms`.
3. Re-enqueue the entry. The next reconcile tick re-checks after the debounce window expires.

### 11B.8 Adapter registration

SCM adapters register via the SCM adapter registry using `init()` functions:

```go
func init() {
    registry.SCMAdapters.Register("github", NewGitHubSCMAdapter)
}
```

The `SCMAdapterConstructor` signature is:

```go
type SCMAdapterConstructor func(adapterConfig map[string]any) (domain.SCMAdapter, error)
```

The `adapterConfig` parameter receives the merged config: `reactions.review_comments.Extra`
plus tracker credentials (API key, endpoint) when `tracker.kind` and the review comments
provider match.

### 11B.9 Scope filtering

Review comment reconciliation only processes PRs created by Sortie:

1. `SCMMetadata.pr_number > 0` — only workspaces where the agent created a PR have this field.
   Since `.sortie/scm.json` is written by the agent inside a Sortie-managed workspace, this is
   inherently scoped.
2. Claimed check: only issues in `claimed` get review polling. Released issues are not polled.

## 12. Prompt Construction and Context Assembly

### 12.1 Inputs

Inputs to prompt rendering:

- `workflow.prompt_template`
- normalized `issue` object
- optional `attempt` integer (retry/continuation metadata)
- `run` object: `turn_number`, `max_turns`, `is_continuation`
- `ci_failure` (map or nil): CI failure context injected into CI-fix continuation prompts via
  `CIResult.ToTemplateMap()`. Nil on initial dispatch and non-CI retries. When non-nil, contains:
  - `status`: aggregate CI pipeline status string (`failing`)
  - `check_runs`: list of individual check run maps (each with `name`, `status`, `conclusion`,
    `details_url`)
  - `log_excerpt`: truncated log from the first failing check (empty string when unavailable)
  - `failing_count`: number of check runs with a failure conclusion
  - `ref`: the git ref that was queried

CI failure context is injected only on turn 1 of a CI-fix dispatch. The worker reads the context
from the dispatch site (carried via `context.WithValue` or the retry entry's `ContinuationContext`
field) and passes it to `prompt.WithContinuationContext`. Templates SHOULD use a conditional guard:
`{{ if .ci_failure }}...{{ end }}`. When `ci_failure` is nil, the template variable is still
present in the data map (set to nil) so strict `missingkey=error` evaluation does not reject
templates that reference the field.

- `review_comments` (list of maps or nil): review comment context injected into review-fix
  continuation prompts. Nil on initial dispatch and non-review retries. When non-nil, each
  element contains:
  - `id`: SCM-platform comment identifier
  - `file`: file path the comment is attached to (empty for PR-level comments)
  - `start_line`: first line of commented range (0 for non-inline)
  - `end_line`: last line of commented range (0 for single-line or non-inline)
  - `reviewer`: username of the comment author
  - `body`: comment text

Review comment context is injected only on turn 1 of a review-fix dispatch, following the same
`ContinuationContext` pathway as CI failure context. Templates SHOULD use a conditional guard:
`{{ if .review_comments }}...{{ end }}`. When `review_comments` is nil, the template variable is
still present in the data map (set to nil) so strict `missingkey=error` evaluation does not
reject templates that reference the field.

### 12.2 Rendering Rules

- Render with strict variable checking.
- Render with strict filter checking.
- Convert issue object keys to strings for template compatibility.
- Preserve nested arrays/maps (labels, blockers) so templates can iterate.

### 12.3 Retry/Continuation Semantics

`attempt` and `run` should be passed to the template because the workflow prompt may provide
different instructions for:

- first run (`attempt` null or absent)
- continuation turn within an active multi-turn session (`run.is_continuation == true`)
- retry after error/timeout/stall (`attempt >= 1`, `run.is_continuation == false`)

### 12.4 Failure Semantics

If prompt rendering fails:

- Fail the run attempt immediately.
- Let the orchestrator treat it like any other worker failure and decide retry behavior.

## 13. Logging, Status, and Observability

### 13.1 Logging Conventions

Required context fields for issue-related logs:

- `issue_id`
- `issue_identifier`

Required context for coding-agent session lifecycle logs:

- `session_id`

Message formatting requirements:

- Use stable `key=value` phrasing.
- Include action outcome (`completed`, `failed`, `retrying`, etc.).
- Include concise failure reason when present.
- Avoid logging large raw payloads unless necessary.

### 13.2 Logging Outputs and Sinks

Sortie does not prescribe where logs must go (stderr, file, remote sink, etc.).

Requirements:

- Operators must be able to see startup/validation/dispatch failures without attaching a debugger.
- Sortie may write to one or more sinks.
- If a configured log sink fails, Sortie continues running when possible and emits an
  operator-visible warning through any remaining sink.

### 13.3 Runtime Snapshot / Monitoring Interface

If the implementation exposes a synchronous runtime snapshot (for dashboards or monitoring), it
should return:

- `running` (list of running session rows)
- each running row should include `turn_count`
- `retrying` (list of retry queue rows)
- `agent_totals`
  - `input_tokens`
  - `output_tokens`
  - `total_tokens`
  - `cache_read_tokens`
  - `seconds_running` (aggregate runtime seconds as of snapshot time, including active sessions)
- `rate_limits` (latest coding-agent rate limit payload, if available)

Recommended snapshot error modes:

- `timeout`
- `unavailable`

### 13.4 Optional Human-Readable Status Surface

A human-readable status surface is optional and implementation-defined. When the HTTP server is
enabled (Section 13.7), the HTML dashboard served at `/` (Section 13.7.1) is the concrete
realization of this surface.

If present, it should draw from orchestrator state/metrics only and must not be required for
correctness.

### 13.5 Session Metrics and Token Accounting

Token accounting rules:

- Agent adapters normalize token counts before emitting events. The orchestrator receives
  `{input_tokens, output_tokens, total_tokens, cache_read_tokens}` directly.
- For absolute totals, track deltas relative to last reported totals to avoid double-counting.
  The `cache_read_tokens` field follows the same cumulative-delta accounting as
  `input_tokens` / `output_tokens`.
- `api_request_count` is incremented monotonically per `token_usage` event.
- Accumulate aggregate totals in orchestrator state (`agent_totals`).

Timing accounting rules:

- `api_time_ms` is the cumulative LLM API wait time in milliseconds for the session.
  Accumulated from `api_duration_ms` fields on any agent event that carries timing data.
- `tool_time_ms` is the cumulative tool execution time in milliseconds.
  Accumulated from `tool_result` events that carry `duration_ms`.
- `tool_time_percent` and `api_time_percent` are computed at render time as
  `(cumulative_ms / session_elapsed_ms) * 100`. Displayed as null/"N/A" when no timing
  data has been received.

Runtime accounting:

- Runtime should be reported as a live aggregate at snapshot/render time.
- Sortie maintains a cumulative counter for ended sessions and adds active-session elapsed time
  derived from `running` entries (for example `started_at`) when producing a snapshot/status view.
- Add run duration seconds to the cumulative ended-session runtime when a session ends (normal exit
  or cancellation/termination).
- Continuous background ticking of runtime totals is not required.

Rate-limit tracking:

- Track the latest rate-limit payload seen in any agent update.
- Any human-readable presentation of rate-limit data is implementation-defined.

### 13.6 Humanized Agent Event Summaries (Optional)

Humanized summaries of raw agent protocol events are optional.

If implemented:

- Treat them as observability-only output.
- Do not make orchestrator logic depend on humanized strings.

### 13.7 HTTP Server

Sortie includes an embedded HTTP server for observability and operational control. The
server starts unconditionally on port **7678** unless explicitly disabled. It is not
required for orchestrator correctness, but its absence silently removes health probes,
Prometheus metrics, and the dashboard.

Enablement and configuration:

- The HTTP server starts by default on `127.0.0.1:7678` with no flags required.
- `--port N` overrides the listening port. Port `0` disables the server entirely.
- `--host ADDR` overrides the bind address. `ADDR` must be a parseable IP address.
  Default: `127.0.0.1`. Container deployments use `0.0.0.0`.
- `server.port` and `server.host` extension keys provide the same overrides via workflow
  front matter. CLI flags take precedence over extension keys.
- `server.port` must be an integer in the range 1–65535, or `0` to disable.
- `server.host` must be a parseable IP address string. DNS hostnames are not accepted.
- When the default port (7678) is occupied and the operator did not explicitly request a
  port (`--port` absent, `server.port` extension absent), Sortie logs a warning and starts
  without the HTTP server; the orchestrator continues normally. When the operator explicitly
  requested a port (via `--port` or `server.port`) and it is already in use, Sortie exits
  with code 1 and a descriptive error. No automatic port selection occurs.
- The `--dry-run` flag suppresses server startup regardless of port or host settings.
- Changes to HTTP listener settings require restart (hot-rebind is not supported).

#### 13.7.1 Human-Readable Dashboard (`/`)

- Host a human-readable dashboard at `/`.
- The returned document should depict the current state of the system (for example active sessions,
  retry delays, token consumption, runtime totals, recent events, health/error indicators, and run
  history from SQLite).
- It is up to the implementation whether this is server-generated HTML or a client-side app that
  consumes the JSON API below.

#### 13.7.2 JSON REST API (`/api/v1/*`)

Provide a JSON REST API under `/api/v1/*` for current runtime state and operational debugging.

Minimum endpoints:

- `GET /api/v1/state`
  - Returns a summary view of the current system state (running sessions, retry queue/delays,
    aggregate token/runtime totals, latest rate limits, and any additional tracked summary fields).
  - Suggested response shape:

    ```json
    {
      "generated_at": "2026-02-24T20:15:30Z",
      "counts": {
        "running": 2,
        "retrying": 1
      },
      "running": [
        {
          "issue_id": "abc123",
          "issue_identifier": "MT-649",
          "state": "In Progress",
          "session_id": "thread-1-turn-1",
          "turn_count": 7,
          "last_event": "turn_completed",
          "last_message": "",
          "started_at": "2026-02-24T20:10:12Z",
          "last_event_at": "2026-02-24T20:14:59Z",
          "tokens": {
            "input_tokens": 1200,
            "output_tokens": 800,
            "total_tokens": 2000,
            "cache_read_tokens": 400
          },
          "model_name": "claude-sonnet-4-20250514",
          "api_request_count": 3,
          "requests_by_model": {"claude-sonnet-4-20250514": 3},
          "tool_time_percent": 12.3,
          "api_time_percent": 45.6
        }
      ],
      "retrying": [
        {
          "issue_id": "def456",
          "issue_identifier": "MT-650",
          "attempt": 3,
          "due_at": "2026-02-24T20:16:00Z",
          "error": "no available orchestrator slots"
        }
      ],
      "agent_totals": {
        "input_tokens": 5000,
        "output_tokens": 2400,
        "total_tokens": 7400,
        "cache_read_tokens": 1500,
        "seconds_running": 1834.2
      },
      "rate_limits": null
    }
    ```

- `GET /api/v1/<issue_identifier>`
  - Returns issue-specific runtime/debug details for the identified issue, including any information
    tracked that is useful for debugging.
  - Suggested response shape:

    ```json
    {
      "issue_identifier": "MT-649",
      "issue_id": "abc123",
      "status": "running",
      "workspace": {
        "path": "/tmp/sortie_workspaces/MT-649"
      },
      "attempts": {
        "restart_count": 1,
        "current_retry_attempt": 2
      },
      "running": {
        "session_id": "thread-1-turn-1",
        "turn_count": 7,
        "state": "In Progress",
        "started_at": "2026-02-24T20:10:12Z",
        "last_event": "notification",
        "last_message": "Working on tests",
        "last_event_at": "2026-02-24T20:14:59Z",
        "tokens": {
          "input_tokens": 1200,
          "output_tokens": 800,
          "total_tokens": 2000
        }
      },
      "retry": null,
      "logs": {
        "agent_session_logs": [
          {
            "label": "latest",
            "path": "/var/log/sortie/agent/MT-649/latest.log",
            "url": null
          }
        ]
      },
      "recent_events": [
        {
          "at": "2026-02-24T20:14:59Z",
          "event": "notification",
          "message": "Working on tests"
        }
      ],
      "last_error": null,
      "tracked": {}
    }
    ```

  - If the issue is unknown to the current in-memory state, return `404` with an error response
    (for example `{"error":{"code":"issue_not_found","message":"..."}}`).

- `POST /api/v1/refresh`
  - Queues an immediate tracker poll + reconciliation cycle (best-effort trigger; implementations
    may coalesce repeated requests).
  - Suggested request body: empty body or `{}`.
  - Suggested response (`202 Accepted`) shape:

    ```json
    {
      "queued": true,
      "coalesced": false,
      "requested_at": "2026-02-24T20:15:30Z",
      "operations": ["poll", "reconcile"]
    }
    ```

API design notes:

- The JSON shapes above are the recommended baseline for interoperability and debugging ergonomics.
- Implementations may add fields, but should avoid breaking existing fields within a version.
- Endpoints should be read-only except for operational triggers like `/refresh`.
- Unsupported methods on defined routes should return `405 Method Not Allowed`.
- API errors should use a JSON envelope such as `{"error":{"code":"...","message":"..."}}`.
- If the dashboard is a client-side app, it should consume this API rather than duplicating state
  logic.

#### 13.7.3 Prometheus Metrics Endpoint (`/metrics`)

When the HTTP server is enabled, Sortie exposes a Prometheus exposition-format scrape endpoint at
`/metrics` via `github.com/prometheus/client_golang`. The endpoint is co-located with the JSON
API and HTML dashboard on the same address and port — no separate configuration is required.

Implementation requirements:

- Use a dedicated `prometheus.Registry` (not the global default) to prevent pollution from
  unrelated collectors and to enable isolated test assertions.
- Register the handler via `promhttp.InstrumentMetricHandler(registry, promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))`.
  `InstrumentMetricHandler` wraps `HandlerFor` and registers `promhttp_metric_handler_*` counters
  on the same dedicated registry automatically, ensuring scrape self-instrumentation appears in
  scrape output rather than landing silently on the global default.
- Register standard Go runtime and process collectors (`collectors.NewGoCollector`,
  `collectors.NewProcessCollector`) on the dedicated registry so that `go_*` and `process_*`
  metrics appear in scrape output alongside Sortie's own metrics.

Defined metrics (label sets and buckets are specified here; see ADR-0008 for historical rationale):

| Name | Type | Description |
|------|------|-------------|
| `sortie_sessions_running` | Gauge | Number of agent sessions currently executing. |
| `sortie_sessions_retrying` | Gauge | Number of issues in the retry queue. |
| `sortie_slots_available` | Gauge | Remaining dispatch capacity under current concurrency limits. |
| `sortie_active_sessions_elapsed_seconds` | Gauge | Cumulative wall-clock elapsed time across all currently running sessions. |
| `sortie_tokens_total{type}` | Counter | Tokens consumed, partitioned by type (`input`, `output`). |
| `sortie_agent_runtime_seconds_total` | Counter | Cumulative agent-session wall-clock time for completed sessions. |
| `sortie_dispatches_total{outcome}` | Counter | Dispatch attempts, partitioned by outcome (`success`, `error`). |
| `sortie_worker_exits_total{exit_type}` | Counter | Worker exits, partitioned by exit type (`normal`, `error`, `cancelled`). |
| `sortie_retries_total{trigger}` | Counter | Retry schedule events, partitioned by trigger (`error`, `continuation`, `timer`, `stall`). |
| `sortie_reconciliation_actions_total{action}` | Counter | Reconciliation outcomes per issue, partitioned by action (`stop`, `cleanup`, `keep`). |
| `sortie_poll_cycles_total{result}` | Counter | Poll tick completions, partitioned by result (`success`, `error`, `skipped`). |
| `sortie_tracker_requests_total{operation,result}` | Counter | Tracker adapter API calls, partitioned by operation (`fetch_candidates`, `fetch_issue`, `fetch_by_states`, `fetch_states_by_ids`, `fetch_states_by_identifiers`, `fetch_comments`, `transition`) and result (`success`, `error`). |
| `sortie_handoff_transitions_total{result}` | Counter | Handoff-state transition attempts, partitioned by result (`success`, `error`, `skipped`). |
| `sortie_dispatch_transitions_total{result}` | Counter | Dispatch-time in-progress transition attempts, partitioned by result (`success`, `error`, `skipped`). `skipped` indicates the issue was already in the target state. |
| `sortie_tool_calls_total{tool,result}` | Counter | Agent tool call completions, partitioned by tool name and result (`success`, `error`). |
| `sortie_ci_status_checks_total{result}` | Counter | CI status check outcomes, partitioned by result (`passing`, `pending`, `failing`, `error`). |
| `sortie_ci_escalations_total{action}` | Counter | CI escalation actions when fix retries are exhausted, partitioned by action (`label`, `comment`, `error`). |
| `sortie_review_checks_total{result}` | Counter | Review comment check outcomes, partitioned by result (`dispatched`, `error`, `skipped`). |
| `sortie_review_escalations_total{action}` | Counter | Review escalation actions when continuation turns are exhausted, partitioned by action (`label`, `comment`, `error`). |
| `sortie_poll_duration_seconds` | Histogram | Wall-clock time per poll cycle; buckets via `ExponentialBuckets(0.1, 2, 10)` (0.1 s–51.2 s). |
| `sortie_worker_duration_seconds{exit_type}` | Histogram | Worker session wall-clock time; buckets via `ExponentialBuckets(10, 2, 12)` (10 s–5.7 h). |
| `sortie_build_info{version,go_version}` | Gauge | Always `1`; carries build metadata as labels. |

## 14. Failure Model and Recovery Strategy

### 14.1 Failure Classes

1. `Workflow/Config Failures`
   - Missing `WORKFLOW.md`
   - Invalid YAML front matter
   - Unsupported tracker kind or missing tracker credentials/project
   - Missing coding-agent executable

2. `Workspace Failures`
   - Workspace directory creation failure
   - Workspace population/synchronization failure (implementation-defined; may come from hooks)
   - Invalid workspace path configuration
   - Hook timeout/failure

3. `Agent Session Failures`
   - Startup handshake failure
   - Turn failed/cancelled
   - Turn timeout
   - User input requested (hard fail)
   - Subprocess exit
   - Stalled session (no activity)

4. `Tracker Failures`
   - API transport errors
   - Non-200 status
   - API-level errors
   - Malformed payloads

5. `Observability Failures`
   - Snapshot timeout
   - Dashboard render errors
   - Log sink configuration failure

6. `CI Feedback Failures`
   - CI status fetch errors (transport, auth, API, not-found, payload)
   - Escalation failures (label or comment write to tracker)
   - Missing or malformed `.sortie/scm.json`

### 14.2 Recovery Behavior

- Dispatch validation failures:
  - Skip new dispatches.
  - Keep service alive.
  - Continue reconciliation where possible.

- Worker failures:
  - Convert to retries with exponential backoff.

- Tracker candidate-fetch failures:
  - Skip this tick.
  - Try again on next tick.

- Reconciliation state-refresh failures:
  - Keep current workers.
  - Retry on next tick.

- Dashboard/log failures:
  - Do not crash the orchestrator.

- CI feedback failures:
  - CI status fetch failure: re-enqueue pending check for next tick.
  - Escalation failure: log and count error, but release claim anyway.
  - Missing/malformed SCM metadata: skip CI check silently (degrade to no-CI behavior).

### 14.3 Partial State Recovery (Restart)

Sortie uses SQLite persistence to improve restart recovery semantics:

- Retry entries with future `due_at` timestamps are restored from SQLite and rescheduled on
  startup.
- Session metadata from the previous run is available for observability and debugging.
- Run history is preserved in SQLite for operational review.
- Running sessions are not recoverable (agent subprocesses do not survive restart), but the
  orchestrator knows which issues were in-flight at shutdown and re-dispatches them immediately
  rather than waiting for the next polling cycle to discover them.

### 14.4 Operator Intervention Points

Operators can control behavior by:

- Editing `WORKFLOW.md` (prompt and most runtime settings).
- `WORKFLOW.md` changes should be detected and re-applied automatically without restart.
- Changing issue states in the tracker:
  - terminal state -> running session is stopped and workspace cleaned when reconciled
  - non-active state -> running session is stopped without cleanup
- Restarting the service for process recovery or deployment (not as the normal path for applying
  workflow config changes).

## 15. Security and Operational Safety

### 15.1 Trust Boundary Assumption

Each deployment defines its own trust boundary.

Operational safety requirements:

- Deployments should state clearly whether they are intended for trusted environments, more
  restrictive environments, or both.
- Deployments should state clearly whether they rely on auto-approved actions, operator approvals,
  stricter sandboxing, or some combination of those controls.
- Workspace isolation and path validation are important baseline controls, but they are not a
  substitute for whatever approval and sandbox policy a deployment chooses.

### 15.2 Filesystem Safety Requirements

Mandatory:

- Workspace path must remain under configured workspace root.
- Coding-agent cwd must be the per-issue workspace path for the current run.
- Workspace directory names must use sanitized identifiers.

Recommended additional hardening:

- Run under a dedicated OS user.
- Restrict workspace root permissions.
- Mount workspace root on a dedicated volume if possible.

### 15.3 Secret Handling

- Support `$VAR` indirection in workflow config.
- Do not log API tokens or secret env values.
- Validate presence of secrets without printing them.

### 15.4 Hook Script Safety

Workspace hooks are arbitrary shell scripts from `WORKFLOW.md`.

Implications:

- Hooks are fully trusted configuration.
- Hooks run inside the workspace directory.
- Hook output should be truncated in logs.
- Hook timeouts are required to avoid hanging the orchestrator.

### 15.5 Harness Hardening Guidance

Running coding agents against repositories, issue trackers, and other inputs that may contain
sensitive data or externally-controlled content can be dangerous. A permissive deployment can lead
to data leaks, destructive mutations, or full machine compromise if the agent is induced to execute
harmful commands or use overly-powerful integrations.

Deployments should explicitly evaluate their own risk profile and harden the execution harness
where appropriate. Sortie does not mandate a single hardening posture, but deployments should not
assume that tracker data, repository contents, prompt inputs, or tool arguments are fully
trustworthy just because they originate inside a normal workflow.

Prompt injection risk: issue descriptions, comments, and labels are untrusted input that flows
directly into agent prompts. Workflow prompts should include defensive instructions to reduce the
risk of injected content manipulating agent behavior.

Possible hardening measures include:

- Tightening agent approval and sandbox settings instead of running with a maximally permissive
  configuration.
- Adding external isolation layers such as OS/container/VM sandboxing, network restrictions, or
  separate credentials beyond the built-in agent policy controls.
- Using `tracker.query_filter` to restrict which issues are eligible for dispatch
  (e.g., by label, component, epic, or other tracker-native criteria) so untrusted or
  out-of-scope tasks do not automatically reach the agent.
- Scoping the `tracker_api` tool (Section 10.4.5) so it can only read or mutate data inside the
  intended project scope, rather than exposing general tracker access.
- Reducing the set of registered tools, credentials, filesystem paths, and network destinations
  available to the agent to the minimum needed for the workflow.

The correct controls are deployment-specific, but deployments should document them clearly and
treat harness hardening as part of the core safety model rather than an optional afterthought.

## 16. Reference Algorithms

### 16.1 Service Startup

```text
function start_service():
  configure_logging()
  start_observability_outputs()
  start_workflow_watch(on_change=reload_and_reapply_workflow)

  validation = validate_dispatch_config()
  if validation is not ok:
    log_validation_error(validation)
    fail_startup(validation)

  open_or_create_sqlite_db()
  run_schema_migrations()

  persisted_retries = sqlite.load_retry_entries()

  state = {
    poll_interval_ms: get_config_poll_interval_ms(),
    max_concurrent_agents: get_config_max_concurrent_agents(),
    running: {},
    claimed: set(),
    retry_attempts: {},
    completed: set(),
    agent_totals: {input_tokens: 0, output_tokens: 0, total_tokens: 0, seconds_running: 0},
    agent_rate_limits: null
  }

  for entry in persisted_retries:
    state = reconstruct_retry_timer(state, entry)

  startup_terminal_workspace_cleanup()
  schedule_tick(delay_ms=0)

  event_loop(state)
```

### 16.2 Poll-and-Dispatch Tick

```text
on_tick(state):
  state = reconcile_running_issues(state)

  validation = validate_dispatch_config()
  if validation is not ok:
    log_validation_error(validation)
    notify_observers()
    schedule_tick(state.poll_interval_ms)
    return state

  issues = tracker.fetch_candidate_issues()
  if issues failed:
    log_tracker_error()
    notify_observers()
    schedule_tick(state.poll_interval_ms)
    return state

  for issue in sort_for_dispatch(issues):
    if no_available_slots(state):
      break

    if should_dispatch(issue, state):
      state = dispatch_issue(issue, state, attempt=null)

  notify_observers()
  schedule_tick(state.poll_interval_ms)
  return state
```

### 16.3 Reconcile Active Runs

```text
function reconcile_running_issues(state):
  state = reconcile_stalled_runs(state)

  running_ids = keys(state.running)
  if running_ids is empty:
    state = reconcile_ci_status(state)
    state = reconcile_review_comments(state)
    return state

  refreshed = tracker.fetch_issue_states_by_ids(running_ids)
  if refreshed failed:
    log_debug("keep workers running")
    state = reconcile_ci_status(state)
    state = reconcile_review_comments(state)
    return state

  for issue in refreshed:
    if issue.state in terminal_states:
      state = terminate_running_issue(state, issue.id, cleanup_workspace=true)
    else if issue.state in active_states:
      state.running[issue.id].issue = issue
    else:
      state = terminate_running_issue(state, issue.id, cleanup_workspace=false)

  state = reconcile_ci_status(state)
  state = reconcile_review_comments(state)
  return state
```

```text
function reconcile_ci_status(state):
  if ci_provider is nil:
    return state

  for key, pending in state.pending_reactions where pending.kind == "ci":
    delete(state.pending_reactions, key)

    ref = pending.sha or pending.branch
    result, err = ci_provider.fetch_ci_status(ref)

    if err:
      log_warn("CI status fetch failed, will retry next tick")
      state.pending_reactions[key] = pending
      continue

    switch result.status:
      case "passing":
        delete(state.reaction_attempts, key)
      case "pending":
        state.pending_reactions[key] = pending
      case "failing":
        handle_ci_failure(state, pending, result)

  return state
```

```text
function reconcile_review_comments(state):
  if scm_adapter is nil:
    return state

  now = utc_now()

  for key, pending in state.pending_reactions where pending.kind == "review":
    delete(state.pending_reactions, key)
    data = pending.kind_data  # ReviewReactionData

    # Poll throttle
    if now < pending.pending_retry_at:
      state.pending_reactions[key] = pending
      continue

    # Continuation turn cap
    rkey = reaction_key(pending.issue_id, "review")
    turn_count = state.reaction_attempts[rkey]
    if turn_count >= review_config.max_continuation_turns:
      escalate_review_failure(state, pending, turn_count)
      continue

    # Fetch reviews from SCM adapter
    comments, err = scm_adapter.fetch_pending_reviews(data.pr_number, data.owner, data.repo)
    if err:
      pending.pending_attempts++
      pending.pending_retry_at = now + backoff(pending.pending_attempts)
      state.pending_reactions[key] = pending
      log_warn("review fetch failed, retrying with backoff")
      continue

    # Filter outdated, compute debounce timestamp
    actionable = filter(comments, c -> not c.outdated)
    max_time = max(c.submitted_at for c in actionable)

    if len(actionable) == 0:
      pending.pending_retry_at = now + poll_interval
      state.pending_reactions[key] = pending
      continue

    # Fingerprint from sorted non-outdated comment IDs
    fingerprint = sha256(sorted(c.id for c in actionable))

    # Dedup via reaction_fingerprints table
    store.upsert_reaction_fingerprint(pending.issue_id, "review", fingerprint)
    stored_fp, dispatched = store.get_reaction_fingerprint(pending.issue_id, "review")
    if stored_fp == fingerprint and dispatched:
      pending.pending_retry_at = now + poll_interval
      state.pending_reactions[key] = pending
      continue

    # Debounce
    if max_time is set and now - max_time < debounce_ms:
      pending.pending_retry_at = max_time + debounce_ms
      state.pending_reactions[key] = pending
      continue

    # Mark dispatched synchronously before scheduling retry
    store.mark_reaction_dispatched(pending.issue_id, "review")

    review_context = build_review_template_map(actionable)
    cancel_retry(state, pending.issue_id)
    schedule_retry(state, pending.issue_id, pending.attempt, {
      identifier: pending.identifier,
      delay_type: continuation,
      continuation_context: {"review_comments": review_context},
      reaction_kind: "review"
    })
    state.reaction_attempts[rkey]++

  return state
```

### 16.4 Dispatch One Issue

```text
function dispatch_issue(issue, state, attempt):
  worker = spawn_worker(
    fn -> run_agent_attempt(issue, attempt, parent_orchestrator_pid) end
  )

  if worker spawn failed:
    return schedule_retry(state, issue.id, next_attempt(attempt), {
      identifier: issue.identifier,
      error: "failed to spawn agent"
    })

  state.running[issue.id] = {
    worker_handle,
    monitor_handle,
    identifier: issue.identifier,
    issue,
    session_id: null,
    agent_pid: null,
    last_agent_message: null,
    last_agent_event: null,
    last_agent_timestamp: null,
    agent_input_tokens: 0,
    agent_output_tokens: 0,
    agent_total_tokens: 0,
    last_reported_input_tokens: 0,
    last_reported_output_tokens: 0,
    last_reported_total_tokens: 0,
    retry_attempt: normalize_attempt(attempt),
    started_at: now_utc()
  }

  state.claimed.add(issue.id)
  state.retry_attempts.remove(issue.id)
  return state
```

### 16.5 Worker Attempt (Workspace + Prompt + Agent)

```text
function run_agent_attempt(issue, attempt, orchestrator_channel):
  cfg = current_config()

  // Dispatch-time in-progress transition (non-fatal).
  if cfg.tracker.in_progress_state is configured:
    if issue.state == cfg.tracker.in_progress_state (case-insensitive):
      log_debug("issue already in in-progress state, skipping transition")
      metrics.inc_dispatch_transitions("skipped")
    else:
      result = tracker.transition_issue(issue.id, cfg.tracker.in_progress_state)
      if result failed:
        log_warn("dispatch in-progress transition failed", issue.id, error)
        metrics.inc_dispatch_transitions("error")
      else:
        log_info("dispatch in-progress transition succeeded", issue.id)
        metrics.inc_dispatch_transitions("success")

  workspace = workspace_manager.create_for_issue(issue.identifier)
  if workspace failed:
    fail_worker("workspace error")

  if run_hook("before_run", workspace.path) failed:
    fail_worker("before_run hook error")

  session = agent_adapter.start_session(workspace=workspace.path)
  if session failed:
    run_hook_best_effort("after_run", workspace.path)
    fail_worker("agent session startup error")

  max_turns = config.agent.max_turns
  turn_number = 1

  while true:
    prompt = build_turn_prompt(workflow_template, issue, attempt, turn_number, max_turns)
    if prompt failed:
      agent_adapter.stop_session(session)
      run_hook_best_effort("after_run", workspace.path)
      fail_worker("prompt error")

    turn_result = agent_adapter.run_turn(
      session=session,
      prompt=prompt,
      issue=issue,
      on_message=(msg) -> send(orchestrator_channel, {agent_update, issue.id, msg})
    )

    if turn_result failed:
      agent_adapter.stop_session(session)
      run_hook_best_effort("after_run", workspace.path)
      fail_worker("agent turn error")

    refreshed_issue = tracker.fetch_issue_states_by_ids([issue.id])
    if refreshed_issue failed:
      agent_adapter.stop_session(session)
      run_hook_best_effort("after_run", workspace.path)
      fail_worker("issue state refresh error")

    issue = refreshed_issue[0] or issue

    if issue.state is not active:
      break

    if turn_number >= max_turns:
      break

    turn_number = turn_number + 1

  // Self-review phase (between turn loop exit and session teardown).
  review_metadata = null
  cfg = current_config()  // re-read for dynamic reload
  if cfg.self_review.enabled AND issue.state is active AND context not cancelled:
    review_metadata = run_self_review_loop(
      session, workspace, issue, cfg.self_review, agent_adapter, orchestrator_channel
    )

  self_review_status = "disabled"
  if review_metadata != null:
    if review_metadata.final_verdict == "pass":
      self_review_status = "passed"
    else if review_metadata.cap_reached:
      self_review_status = "cap_reached"
    else:
      self_review_status = "error"

  agent_adapter.stop_session(session)
  run_hook_best_effort("after_run", workspace.path, {
    SORTIE_SELF_REVIEW_STATUS: self_review_status,
    SORTIE_SELF_REVIEW_SUMMARY_PATH: workspace.path + "/.sortie/review_summary.md"
  })

  exit_normal()
```

### 16.6 Worker Exit and Retry Handling

```text
on_worker_exit(issue_id, reason, state):
  running_entry = state.running.remove(issue_id)
  state = add_runtime_seconds_to_totals(state, running_entry)
  sqlite.persist_run_attempt(running_entry, reason)  # persist before scheduling retry

  if reason == normal:
    state.completed.add(issue_id)  # bookkeeping only
    state = schedule_retry(state, issue_id, 1, {
      identifier: running_entry.identifier,
      delay_type: continuation
    })

    # Enqueue CI check when provider is configured and workspace has SCM metadata
    if ci_provider is not nil and workspace_path is not empty:
      if issue_id in state.claimed:
        scm = read_scm_metadata(workspace_path)
        if scm.branch is not empty:
          rkey = reaction_key(issue_id, "ci")
          state.pending_reactions[rkey] = {
            issue_id, identifier, display_id, attempt,
            kind: "ci", branch: scm.branch, sha: scm.sha
          }

    # Enqueue review check when SCM adapter is configured and workspace has PR metadata
    if scm_adapter is not nil and workspace_path is not empty:
      if issue_id in state.claimed:
        scm = read_scm_metadata(workspace_path)
        if scm.pr_number > 0 and scm.owner is not empty and scm.repo is not empty:
          rkey = reaction_key(issue_id, "review")
          # Only create if not already present (preserves in-progress debounce)
          if rkey not in state.pending_reactions:
            state.pending_reactions[rkey] = {
              issue_id, identifier, display_id, attempt,
              kind: "review",
              pr_number: scm.pr_number, owner: scm.owner, repo: scm.repo,
              branch: scm.branch, sha: scm.sha
            }
  else:
    state = schedule_retry(state, issue_id, next_attempt_from(running_entry), {
      identifier: running_entry.identifier,
      error: format("worker exited: %reason")
    })

  notify_observers()
  return state
```

```text
on_retry_timer(issue_id, state):
  retry_entry = state.retry_attempts.pop(issue_id)
  if missing:
    return state

  candidates = tracker.fetch_candidate_issues()
  if fetch failed:
    return schedule_retry(state, issue_id, retry_entry.attempt + 1, {
      identifier: retry_entry.identifier,
      error: "retry poll failed"
    })

  issue = find_by_id(candidates, issue_id)
  if issue is null:
    state.claimed.remove(issue_id)
    return state

  if available_slots(state) == 0:
    return schedule_retry(state, issue_id, retry_entry.attempt + 1, {
      identifier: issue.identifier,
      error: "no available orchestrator slots"
    })

  return dispatch_issue(issue, state, attempt=retry_entry.attempt)
```

## 17. Test and Validation Matrix

Sortie's tests cover the behaviors defined in this architecture document.

Validation profiles:

- `Core Conformance`: deterministic tests required for all core features.
- `Extension Conformance`: required only for optional features that are implemented.
- `Real Integration Profile`: environment-dependent smoke/integration checks recommended before
  production use.

Unless otherwise noted, Sections 17.1 through 17.7 are `Core Conformance`. Bullets that begin with
`If ... is implemented` are `Extension Conformance`.

### 17.1 Workflow and Config Parsing

- Workflow file path precedence:
  - explicit runtime path is used when provided
  - cwd default is `WORKFLOW.md` when no explicit runtime path is provided
- Workflow file changes are detected and trigger re-read/re-apply without restart
- Invalid workflow reload keeps last known good effective configuration and emits an
  operator-visible error
- Missing `WORKFLOW.md` returns typed error
- Invalid YAML front matter returns typed error
- Front matter non-map returns typed error
- Config defaults apply when optional values are missing
- `tracker.kind` validation enforces registered tracker adapters
- `tracker.api_key` works (including `$VAR` indirection)
- `$VAR` resolution works for tracker API key and path values
- `~` path expansion works
- `agent.command` is preserved as a shell command string
- Per-state concurrency override map normalizes state names and ignores invalid values
- Prompt template renders `issue`, `attempt`, and `run`
- Prompt rendering fails on unknown variables (strict mode)

### 17.2 Workspace Manager and Safety

- Deterministic workspace path per issue identifier
- Missing workspace directory is created
- Existing workspace directory is reused
- Existing non-directory path at workspace location is handled safely (replace or fail per
  implementation policy)
- Optional workspace population/synchronization errors are surfaced
- `after_create` hook runs only on new workspace creation
- `before_run` hook runs before each attempt and failure/timeouts abort the current attempt
- `after_run` hook runs after each attempt and failure/timeouts are logged and ignored
- `before_remove` hook runs on cleanup and failures/timeouts are ignored
- Workspace path sanitization and root containment invariants are enforced before agent launch
- Agent launch uses the per-issue workspace path as cwd and rejects out-of-root paths
- Hook environment variables (`SORTIE_ISSUE_ID`, `SORTIE_ISSUE_IDENTIFIER`, `SORTIE_WORKSPACE`,
  `SORTIE_ATTEMPT`) are set correctly

### 17.3 Issue Tracker Client

- Candidate issue fetch uses active states and project identifier
- Adapter contract tests cover: normalized field mapping, pagination order, error categories
- Each tracker adapter ships its own integration test suite
- Empty `fetch_issues_by_states([])` returns empty without API call
- Pagination preserves order across multiple pages
- Labels are normalized to lowercase
- Issue state refresh by ID returns minimal normalized issues
- Error mapping covers transport errors, auth errors, API errors, and malformed payloads
- `query_filter` is appended to candidate fetch JQL when non-empty
- `query_filter` is appended to terminal-state fetch JQL when non-empty
- `query_filter` is NOT appended to state-refresh-by-IDs JQL
- Empty `query_filter` produces the same JQL as before (no trailing AND)
- `query_filter` containing OR operators is wrapped in parentheses
- SQLite persistence layer correctly saves and restores retry entries across simulated restart
- Startup recovery from SQLite reconstructs retry timers with correct remaining delays
- Run history is queryable after session completion

### 17.4 Orchestrator Dispatch, Reconciliation, and Retry

- Dispatch sort order is priority then oldest creation time
- Issue with non-terminal blockers in a non-running active state is not eligible
- Issue with terminal blockers is eligible
- Active-state issue refresh updates running entry state
- Non-active state stops running agent without workspace cleanup
- Terminal state stops running agent and cleans workspace
- Reconciliation with no running issues is a no-op
- Normal worker exit schedules a short continuation retry (attempt 1)
- Abnormal worker exit increments retries with 10s-based exponential backoff
- Retry backoff cap uses configured `agent.max_retry_backoff_ms`
- Retry queue entries include attempt, due time, identifier, and error
- Stall detection kills stalled sessions and schedules retry
- Slot exhaustion requeues retries with explicit error reason
- Dispatch-time in-progress transition calls `TransitionIssue` when `tracker.in_progress_state`
  is configured
- Dispatch-time transition failure is non-fatal: the worker continues to workspace preparation
- Dispatch-time transition is skipped when `tracker.in_progress_state` is absent
- Dispatch-time transition is skipped (debug log only) when the issue is already in the target state
- If a snapshot API is implemented, it returns running rows, retry rows, token totals, and rate
  limits
- If a snapshot API is implemented, timeout/unavailable cases are surfaced
- CI status reconciliation is skipped when neither `ci_feedback.kind` nor `reactions.ci_failure`
  is configured
- CI status passing clears CI fix attempts for the issue
- CI status pending re-enqueues the pending check for the next tick
- CI status failing within `max_retries` schedules a CI-fix dispatch with failure context
- CI status failing beyond `max_retries` escalates (label or comment) and releases the claim
- CI failure context is injected into turn 1 prompt via `prompt.WithContinuationContext`
- Escalation label failure is logged but does not block claim release
- `.sortie/scm.json` symlink rejection prevents CI check enqueue
- `.sortie/scm.json` oversized or malformed files degrade to no-CI behavior
- Review comment reconciliation is skipped when `reactions.review_comments` is not configured
- Review comment poll throttle respected (PendingRetryAt in future → skip)
- Review comment fetch error increments backoff and re-enqueues
- No actionable review comments re-enqueues with poll interval delay
- Review comment fingerprint unchanged and dispatched → skip
- Review comment fingerprint changed, debounce not elapsed → defer
- Review comment fingerprint changed, debounce elapsed → dispatch with review context
- Review comment continuation turn cap exceeded → escalate and release claim
- Review comment outdated comments filtered before fingerprint computation
- Review comment context injected into turn 1 prompt via `prompt.WithContinuationContext`
- Review escalation failure is logged but does not block claim release
- Worker exit with `scm.json` containing `pr_number > 0`, `owner`, and `repo` creates review
  pending reaction; missing fields degrade to no-review behavior
- Worker exit does not overwrite existing pending review entry (preserves debounce state)
- Self-review disabled adds zero overhead (no review turns, no review metadata)
- Self-review runs verification commands and passes results to agent
- Review verdict "pass" terminates loop
- Review verdict "iterate" triggers fix turn and next iteration
- Iteration cap enforced; worker exits with cap_reached metadata
- Missing verdict treated as iterate (non-final) / pass (final)
- Verification command timeout does not block remaining commands
- Review progress visible in runtime snapshot via selfReviewCh

### 17.5 Coding-Agent Adapter Client

- Launch command uses workspace cwd and invokes the configured shell
- Startup handshake sequence is adapter-defined and tested per adapter
- Policy-related startup payloads use the implementation's documented approval/sandbox settings
- Session identifiers are parsed and `session_started` event is emitted
- Request/response read timeout is enforced
- Turn timeout is enforced
- Partial JSON lines are buffered until newline (for adapters using line-delimited protocols)
- Stdout and stderr are handled separately; protocol JSON is parsed from stdout only
- Non-JSON stderr lines are logged but do not crash parsing
- Command/file-change approvals are handled according to the implementation's documented policy
- Unsupported dynamic tool calls are handled at the adapter level without stalling the session
- User input requests are handled according to the implementation's documented policy and do not
  stall indefinitely
- Normalized token usage events are emitted with `{input_tokens, output_tokens, total_tokens}`
- `ToolRegistry` is populated at startup and all registered tools appear in prompt-time
  advertisement
- `tracker_api` tool:
  - inputs execute against configured tracker auth
  - API-level errors produce `success: false` with a normalized `{kind, message}` error envelope
  - invalid arguments, missing auth, and transport failures return structured failure payloads
  - the tool is scoped to the configured project
- Unsupported tool names return a failure result at the adapter level without stalling the
  session

### 17.6 Observability

- Validation failures are operator-visible
- Structured logging includes issue/session context fields
- Logging sink failures do not crash orchestration
- Token/rate-limit aggregation remains correct across repeated agent updates
- If a human-readable status surface is implemented, it is driven from orchestrator state and does
  not affect correctness
- If humanized event summaries are implemented, they cover key agent event classes without changing
  orchestrator behavior

### 17.7 CLI and Host Lifecycle

- CLI accepts an optional positional workflow path argument (`path-to-WORKFLOW.md`)
- CLI uses `./WORKFLOW.md` when no workflow path argument is provided
- CLI errors on nonexistent explicit workflow path or missing default `./WORKFLOW.md`
- CLI surfaces startup failure cleanly
- CLI exits with success when application starts and shuts down normally
- CLI exits nonzero when startup fails or the host process exits abnormally

### 17.8 Real Integration Profile (Recommended)

These checks are recommended for production readiness and may be skipped in CI when credentials,
network access, or external service permissions are unavailable.

- A real tracker smoke test can be run with valid credentials supplied by the appropriate tracker
  credential environment variable or a documented local bootstrap mechanism.
- Real integration tests should use isolated test identifiers/workspaces and clean up tracker
  artifacts when practical.
- A skipped real-integration test should be reported as skipped, not silently treated as passed.
- If a real-integration profile is explicitly enabled in CI or release validation, failures should
  fail that job.

## 18. Implementation Checklist (Definition of Done)

Use the same validation profiles as Section 17:

- Section 18.1 = `Core Conformance`
- Section 18.2 = `Extension Conformance`
- Section 18.3 = `Real Integration Profile`

### 18.1 Required for Conformance

- Workflow path selection supports explicit runtime path and cwd default
- `WORKFLOW.md` loader with YAML front matter + prompt body split
- Typed config layer with defaults and `$` resolution
- Dynamic `WORKFLOW.md` watch/reload/re-apply for config and prompt
- Polling orchestrator with single-authority mutable state
- Issue tracker client with candidate fetch + state refresh + terminal fetch
- Tracker adapter interface with at least one implementation (Jira)
- Agent adapter interface with at least one implementation (Claude Code)
- Workspace manager with sanitized per-issue workspaces
- Workspace lifecycle hooks (`after_create`, `before_run`, `after_run`, `before_remove`)
- Hook timeout config (`hooks.timeout_ms`, default `60000`)
- Hook environment variables (`SORTIE_ISSUE_ID`, `SORTIE_ISSUE_IDENTIFIER`, `SORTIE_WORKSPACE`,
  `SORTIE_ATTEMPT`)
- SQLite persistence layer with schema migrations
- Startup recovery from persisted state (retry timers reconstructed from SQLite `due_at`)
- Agent launch command config (`agent.command`, adapter-defined default)
- Strict prompt rendering with `issue`, `attempt`, and `run` variables
- Exponential retry queue with continuation retries after normal exit
- Configurable retry backoff cap (`agent.max_retry_backoff_ms`, default 5m)
- Reconciliation that stops runs on terminal/non-active tracker states
- Workspace cleanup for terminal issues (startup sweep + active transition)
- Structured logs with `issue_id`, `issue_identifier`, and `session_id`
- Operator-visible observability (structured logs; optional snapshot/status surface)

### 18.2 Recommended Extensions (Not Required for Conformance)

- HTTP server honors CLI `--port` over `server.port`, uses a safe default bind host, and exposes
  the baseline endpoints/error semantics in Section 13.7 if shipped.
- Prometheus `/metrics` endpoint exposes defined gauges, counters, and histograms when the HTTP
  server is enabled (Section 13.7.3). Backed by `github.com/prometheus/client_golang` with a
  dedicated registry; no external Prometheus server required.
- Agent tool subsystem: `ToolRegistry` populated at startup with `tracker_api` per Section 10.4.
  Execution channel design and additional built-in tools are tracked in issues #224, #226, #227.
- Make observability settings configurable in workflow front matter without prescribing UI
  implementation details.
- First-class tracker write APIs (comments/state transitions) in the orchestrator, supplementing
  agent tool-based mutations.

### 18.3 Operational Validation Before Production (Recommended)

- Run the `Real Integration Profile` from Section 17.8 with valid credentials and network access.
- Verify hook execution and workflow path resolution on the target host OS/shell environment.
- If the HTTP server is shipped, verify the configured port behavior and loopback/default bind
  expectations on the target environment.

## 19. Persistence Schema

### 19.1 Overview

Sortie uses an embedded SQLite database for durable state. The database file path defaults to
`.sortie.db` in the same directory as `WORKFLOW.md` and can be overridden with the `db_path`
front matter field (see Section 5.3.7). On startup, Sortie opens or creates the database and
runs all pending schema migrations before beginning normal operation.

### 19.2 Tables

**`retry_entries`** — pending retries to be reconstructed on restart

| Column       | Type    | Notes                                              |
| ------------ | ------- | -------------------------------------------------- |
| `issue_id`   | TEXT PK | Tracker-internal issue ID                          |
| `identifier` | TEXT    | Human-readable ticket key                          |
| `attempt`    | INTEGER | Retry attempt number (1-based)                     |
| `due_at_ms`  | INTEGER | Unix epoch milliseconds; used to reconstruct timer |
| `error`      | TEXT    | Last error message, may be null                    |

Note: `timer_handle` is runtime-only and is not stored.

**`run_history`** — completed run attempts

| Column          | Type    | Notes                                     |
| --------------- | ------- | ----------------------------------------- |
| `id`            | INTEGER | Auto-increment primary key                |
| `issue_id`      | TEXT    | Tracker-internal issue ID                 |
| `identifier`    | TEXT    | Human-readable ticket key                 |
| `attempt`       | INTEGER | Attempt number at time of run             |
| `agent_adapter` | TEXT    | Agent adapter kind used                   |
| `workspace`     | TEXT    | Workspace path                            |
| `started_at`    | TEXT    | ISO-8601 timestamp                        |
| `completed_at`  | TEXT    | ISO-8601 timestamp                        |
| `status`          | TEXT    | Terminal status (succeeded, failed, etc.) |
| `error`           | TEXT    | Error message if failed, may be null      |
| `review_metadata` | TEXT    | JSON-encoded self-review metadata, may be null (migration 007) |

**`session_metadata`** — last known session metadata per issue (for observability and debug)

| Column              | Type    | Notes                             |
| ------------------- | ------- | --------------------------------- |
| `issue_id`          | TEXT PK | Tracker-internal issue ID         |
| `session_id`        | TEXT    | Last session ID                   |
| `agent_pid`         | TEXT    | Last known agent PID, may be null |
| `input_tokens`      | INTEGER | Accumulated input tokens          |
| `output_tokens`     | INTEGER | Accumulated output tokens         |
| `total_tokens`      | INTEGER | Accumulated total tokens          |
| `cache_read_tokens` | INTEGER | Accumulated cache-read tokens (migration 002) |
| `model_name`        | TEXT    | Last reported LLM model identifier (migration 002) |
| `api_request_count` | INTEGER | Number of API round-trips observed (migration 002) |
| `updated_at`        | TEXT    | ISO-8601 timestamp of last update |

**`aggregate_metrics`** — global token and runtime totals

| Column              | Type    | Notes                             |
| ------------------- | ------- | --------------------------------- |
| `key`               | TEXT PK | Metric key (e.g., `agent_totals`) |
| `input_tokens`      | INTEGER |                                   |
| `output_tokens`     | INTEGER |                                   |
| `total_tokens`      | INTEGER |                                   |
| `cache_read_tokens` | INTEGER | Cumulative cache-read tokens (migration 002) |
| `seconds_running`   | REAL    | Cumulative runtime seconds        |
| `updated_at`        | TEXT    | ISO-8601 timestamp                |

**`reaction_fingerprints`** — cross-restart reaction deduplication (migration 008)

| Column        | Type    | Notes                                                         |
| ------------- | ------- | ------------------------------------------------------------- |
| `issue_id`    | TEXT    | Tracker-internal issue ID (composite PK with `kind`)          |
| `kind`        | TEXT    | Reaction kind (`ci`, `review`)                                |
| `fingerprint` | TEXT    | Deterministic hash of the current reaction state              |
| `dispatched`  | INTEGER | `1` when a fix dispatch has been sent for this fingerprint    |
| `updated_at`  | TEXT    | ISO-8601 timestamp                                            |

Primary key: `(issue_id, kind)`. Upserts reset `dispatched` to `0` when the fingerprint value
changes (new comments detected). Used by CI and review reconcile functions to prevent duplicate
dispatches across restarts.

### 19.3 Migration Strategy

- Migrations are numbered sequentially and applied in order at startup.
- Applied migrations are tracked in a `schema_migrations` table.
- Migrations are additive (new columns/tables) where possible; destructive migrations require
  explicit versioning.

## 20. Webhook Support (Future Extension)

Sortie currently uses polling as the sole mechanism for tracker event delivery. This section
documents a planned extension point so the polling layer is designed to coexist with push-based
event delivery.

A future webhook receiver would accept HTTP POST events from the configured tracker (e.g., Jira
webhooks), parse them into the normalized issue model, and deliver them to the orchestrator as
immediate state updates. This would reduce polling latency for state changes without replacing the
polling loop entirely (polling remains the fallback and the source of truth for reconciliation).

Design constraints for coexistence:

- The polling loop must remain correct in the absence of webhooks.
- Webhook events should be treated as advisory triggers for an immediate reconciliation cycle, not
  as authoritative state transitions.
- Duplicate delivery (webhook + next poll cycle seeing the same state) must be idempotent.

This extension is not implemented in v1. Tracker adapters should not assume webhook availability.

## 21. Agent-Authored Workspace Files

Agents may create files in the workspace that Sortie reads for orchestration decisions. This is an
optional extension that enables richer agent-to-orchestrator communication beyond the event stream.

### 21.1 `.sortie/status`

An agent may write a `.sortie/status` file in the workspace root to signal progress or request
specific orchestrator behavior. Sortie reads this file after each turn completes.

Recognized values:

- `blocked` — agent signals it cannot proceed without human intervention. The orchestrator treats
  this as a soft stop: it completes the current turn normally but does not schedule continuation
  retries until the issue state changes in the tracker.
- `needs-human-review` — agent signals that work is complete and requires review. Treated the
  same as `blocked` from the orchestrator's perspective; the distinction is informational for
  operators and status surfaces.

If the file is absent or contains an unrecognized value, it is ignored.

The `.sortie/status` file is not required for any core orchestration behavior. It is an advisory
channel only.

The full protocol specification — including file format, parsing rules, read timing, cleanup
obligations, versioning, security considerations, and design rationale — is in
[agent-to-orchestrator-protocol.md](agent-to-orchestrator-protocol.md).

### 21.2 `.sortie/review_verdict.json`

During the self-review phase, the agent writes a structured review verdict to
`.sortie/review_verdict.json`. The orchestrator reads this file after each review turn to
determine the next action.

JSON schema:

```json
{
  "verdict": "pass | iterate",
  "issues": [
    {
      "file": "path/to/file.go",
      "line": 42,
      "severity": "error | warning | info",
      "message": "Description of the issue"
    }
  ]
}
```

- `verdict` (string): `"pass"` ends the review loop; `"iterate"` requests a fix turn.
  Any other value is rejected as invalid.
- `issues` (array, optional): structured list of review findings for the fix prompt.

Safety rules:

- Maximum file size: 65536 bytes (64 KB). Oversized files are rejected.
- Symlink protection: both `.sortie/` and `.sortie/review_verdict.json` are checked via
  `Lstat` before reading. If either is a symbolic link, the file is rejected. This follows
  the same pattern as `.sortie/status` (Section 21.1).
- Missing or invalid verdict content on a non-final iteration is treated as `"iterate"`.
  Missing or invalid verdict content on the final iteration does not count as `"pass"`; the
  run ends with no final verdict recorded and `CapReached=true`.

## Appendix A. SSH Worker Extension (Optional)

This appendix describes a common extension profile in which Sortie keeps one central orchestrator
but executes worker runs on one or more remote hosts over SSH.

### A.1 Execution Model

- The orchestrator remains the single source of truth for polling, claims, retries, and
  reconciliation.
- `worker.ssh_hosts` provides the candidate SSH destinations for remote execution.
- Each worker run is assigned to one host at a time, and that host becomes part of the run's
  effective execution identity along with the issue workspace.
- `workspace.root` is interpreted on the remote host, not on the orchestrator host. The
  orchestrator validates workspace safety invariants locally (path sanitization, no traversal)
  and trusts the remote host to enforce filesystem permissions.
- The coding-agent is launched over SSH stdio instead of as a local subprocess, so the orchestrator
  still owns the session lifecycle even though commands execute remotely.
- Continuation turns inside one worker lifetime should stay on the same host and workspace.
- A remote host should satisfy the same basic contract as a local worker environment: reachable
  shell, writable workspace root, coding-agent executable, and any required auth or repository
  prerequisites.

### A.2 Scheduling Notes

- SSH hosts may be treated as a pool for dispatch.
- Implementations may prefer the previously used host on retries when that host is still available.
- `worker.max_concurrent_agents_per_host` is an optional shared per-host cap across configured SSH
  hosts.
- When all SSH hosts are at capacity, dispatch should wait rather than silently falling back to a
  different execution mode.
- Implementations may fail over to another host when the original host is unavailable before work
  has meaningfully started.
- Once a run has already produced side effects, a transparent rerun on another host should be
  treated as a new attempt, not as invisible failover.

### A.3 Problems to Consider

- Remote environment drift:
  - Each host needs the expected shell environment, coding-agent executable, auth, and repository
    prerequisites.
- Workspace locality:
  - Workspaces are usually host-local, so moving an issue to a different host is typically a cold
    restart unless shared storage exists.
- Path and command safety:
  - Remote path resolution, shell quoting, and workspace-boundary checks matter more once execution
    crosses a machine boundary.
- Startup and failover semantics:
  - Implementations should distinguish host-connectivity/startup failures from in-workspace agent
    failures so the same ticket is not accidentally re-executed on multiple hosts.
- Host health and saturation:
  - A dead or overloaded host should reduce available capacity, not cause duplicate execution or an
    accidental fallback to local work.
- Cleanup and observability:
  - Operators need to know which host owns a run, where its workspace lives, and whether cleanup
    happened on the right machine.
