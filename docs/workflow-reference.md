# WORKFLOW.md Syntax Reference

> **Authoritative user-facing reference for workflow authors.**
> Derived from the Sortie Architecture Specification — Sections 5, 6, 9.4, and 10.
>
> A reviewer can write a valid `WORKFLOW.md` using only this document.

---

## Table of Contents

- [1. File Format](#1-file-format)
  - [1.1 Overview](#11-overview)
  - [1.2 Parsing Rules](#12-parsing-rules)
  - [1.3 Returned Workflow Object](#13-returned-workflow-object)
- [2. Front Matter Schema](#2-front-matter-schema)
  - [2.1 Top-Level Keys](#21-top-level-keys)
  - [2.2 `tracker` — Issue Tracker Configuration](#22-tracker--issue-tracker-configuration)
  - [2.3 `polling` — Poll Loop Timing](#23-polling--poll-loop-timing)
  - [2.4 `workspace` — Workspace Root](#24-workspace--workspace-root)
  - [2.5 `hooks` — Workspace Lifecycle Hooks](#25-hooks--workspace-lifecycle-hooks)
  - [2.6 `agent` — Coding Agent Configuration](#26-agent--coding-agent-configuration)
  - [2.7 `db_path` — SQLite Database Path](#27-db_path--sqlite-database-path)
- [3. Environment Variable Overrides](#3-environment-variable-overrides)
  - [3.1 Source Precedence](#31-source-precedence)
  - [3.2 Curated Variable List](#32-curated-variable-list)
  - [3.3 Type Coercion](#33-type-coercion)
  - [3.4 `.env` File Support](#34-env-file-support)
  - [3.5 Interaction with `$VAR` Indirection](#35-interaction-with-var-indirection)
  - [3.6 Fields Not Overridable via Env](#36-fields-not-overridable-via-env)
  - [3.7 Dynamic Reload](#37-dynamic-reload)
- [4. Extensions](#4-extensions)
  - [4.1 `server.port` — HTTP Server](#41-serverport--http-server)
  - [4.2 `logging.level` — Log Verbosity](#42-logginglevel--log-verbosity)
  - [4.3 `worker` — SSH Worker Extension](#43-worker--ssh-worker-extension)
  - [4.4 Adapter-Specific Pass-Through Config](#44-adapter-specific-pass-through-config)
- [5. Prompt Template Reference](#5-prompt-template-reference)
  - [5.1 Template Engine](#51-template-engine)
  - [5.2 Template Input Variables](#52-template-input-variables)
  - [5.3 Built-in Functions (FuncMap)](#53-built-in-functions-funcmap)
  - [5.4 Built-in Actions](#54-built-in-actions)
  - [5.5 First-Turn vs Continuation Semantics](#55-first-turn-vs-continuation-semantics)
  - [5.6 Fallback Prompt Behavior](#56-fallback-prompt-behavior)
  - [5.7 Common Patterns and Pitfalls](#57-common-patterns-and-pitfalls)
- [6. Hook Lifecycle Reference](#6-hook-lifecycle-reference)
  - [6.1 Execution Contract](#61-execution-contract)
  - [6.2 Hook Environment Variables](#62-hook-environment-variables)
  - [6.3 Failure Semantics](#63-failure-semantics)
  - [6.4 Inline Scripts vs File Paths](#64-inline-scripts-vs-file-paths)
- [7. Dynamic Reload Behavior](#7-dynamic-reload-behavior)
  - [7.1 General Reload Semantics](#71-general-reload-semantics)
  - [7.2 Per-Field Reload Behavior](#72-per-field-reload-behavior)
- [8. Dispatch Preflight Validation](#8-dispatch-preflight-validation)
- [9. Error Reference](#9-error-reference)
  - [9.1 Workflow File Errors](#91-workflow-file-errors)
  - [9.2 Configuration Errors](#92-configuration-errors)
  - [9.3 Environment Variable Errors](#93-environment-variable-errors)
  - [9.4 Template Errors](#94-template-errors)
- [10. Config Fields Summary (Cheat Sheet)](#10-config-fields-summary-cheat-sheet)
- [11. Complete Annotated Examples](#11-complete-annotated-examples)
  - [11.1 Minimal Workflow](#111-minimal-workflow)
  - [11.2 Production Jira + Claude Code](#112-production-jira--claude-code)

---

## 1. File Format

### 1.1 Overview

`WORKFLOW.md` is a Markdown file with optional YAML front matter. It encodes two payloads in
a single document:

| Payload             | Location                            | Purpose                                           |
| ------------------- | ----------------------------------- | ------------------------------------------------- |
| **Configuration**   | YAML front matter (between `---`)   | Tracker, polling, workspace, hooks, agent         |
| **Prompt template** | Markdown body (after closing `---`) | Per-issue prompt rendered with Go `text/template` |

The file is repository-owned and version-controlled. It is self-contained enough to describe
a complete workflow — prompt, runtime settings, hooks, and tracker selection — without
requiring out-of-band service-specific configuration.

**File discovery precedence:**

1. Explicit path provided via CLI startup argument.
2. Default: `WORKFLOW.md` in the current process working directory.

### 1.2 Parsing Rules

The parser applies the following steps in order:

1. **BOM stripping.** Remove a leading UTF-8 byte order mark (`\xef\xbb\xbf`) if present.
2. **Line ending normalization.** Replace all `\r\n` with `\n`.
3. **Opening delimiter detection.** If the first line is exactly `---` followed by a
   newline (with optional trailing whitespace), enter front matter mode. A file whose
   entire content is `---` with no trailing newline is treated as having no front matter.
4. **Front matter extraction.** Scan lines until a line that is exactly `---` (with
   optional trailing whitespace). Bytes between the delimiters are the YAML front matter.
   If no closing delimiter is found, the entire content after the opening delimiter is
   treated as front matter and the prompt body is empty (this is not an error).
5. **YAML decoding.** Decode front matter bytes to a map. Non-map YAML (scalar, list) is
   a parse error. Empty or comment-only YAML between delimiters produces an empty map.
6. **Prompt body extraction.** All remaining bytes after the closing delimiter become the
   prompt template, trimmed of leading and trailing whitespace.

**When front matter is absent** (file does not start with `---`):

- `config` is an empty map (`{}`).
- The entire file content is the prompt template (trimmed).

```
┌──────────────────────────────┐
│ ---                          │ ← Opening delimiter
│ tracker:                     │
│   kind: jira                 │ ← YAML front matter (config)
│   project: PROJ              │
│ ---                          │ ← Closing delimiter
│                              │
│ You are an engineer.         │ ← Prompt template body
│ Fix {{ .issue.identifier }}  │
└──────────────────────────────┘
```

### 1.3 Returned Workflow Object

After parsing, the loader produces a struct with three fields:

| Output             | Type             | Description                                                                               |
| ------------------ | ---------------- | ----------------------------------------------------------------------------------------- |
| Config             | `map[string]any` | Front matter root object (not nested under a `config` key).                               |
| Prompt template    | `string`         | Trimmed Markdown body.                                                                    |
| Front matter lines | `int`            | Line count through closing `---`; `0` when absent. Used for error line number adjustment. |

---

## 2. Front Matter Schema

### 2.1 Top-Level Keys

The core schema recognizes six top-level keys:

```yaml
tracker: # Issue tracker connection and query settings
polling: # Poll loop timing
workspace: # Workspace root path
hooks: # Workspace lifecycle hook scripts
agent: # Coding agent adapter, timeouts, and limits
db_path: # SQLite database file path
```

**Unknown top-level keys are ignored** by the core schema for forward compatibility. They
are collected into an `Extensions` map and made available to consumers (e.g., `server`,
`worker`, adapter-specific blocks like `claude-code`).

---

### 2.2 `tracker` — Issue Tracker Configuration

```yaml
tracker:
  kind: jira
  endpoint: https://mycompany.atlassian.net
  api_key: $JIRA_API_TOKEN
  project: PROJ
  active_states:
    - To Do
    - In Progress
  terminal_states:
    - Done
    - Won't Do
  query_filter: "labels = 'agent-ready'"
  handoff_state: Human Review
  in_progress_state: In Progress
```

| Field             | Type            | Required                  | Default         | Dynamic Reload                     | Description                                                                                                                                                                                     |
| ----------------- | --------------- | ------------------------- | --------------- | ---------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `kind`            | string          | **Yes** (for dispatch)    | _(none)_        | Future dispatches                  | Adapter identifier. Supported: `jira`, `file`. Additional adapters are registered separately.                                                                                                   |
| `endpoint`        | string          | Adapter-defined           | Adapter-defined | Future dispatches                  | Tracker API endpoint URL. Supports `$VAR` indirection: if the value starts with `$`, it is expanded via `os.ExpandEnv`.                                                                         |
| `api_key`         | string          | When adapter requires it  | _(none)_        | Future dispatches                  | API authentication token. May be a literal or `$VAR_NAME`. If `$VAR_NAME` resolves to empty, treated as missing. Jira requires this field. Full env expansion applied (`$VAR` at any position). |
| `project`         | string          | When adapter requires it  | _(none)_        | Future dispatches                  | Project identifier (e.g., Jira project key). Supports `$VAR` indirection: if the value starts with `$`, it is expanded via `os.ExpandEnv`.                                                      |
| `active_states`   | list of strings | **Yes** (see rules below) | `[]` (empty)    | Future dispatch and reconciliation | Issue states eligible for agent dispatch. An issue is eligible for dispatch only if its state appears in this list. An empty list means no issues will be dispatched.                           |
| `terminal_states` | list of strings | **Yes** (see rules below) | `[]` (empty)    | Future dispatch and reconciliation | Issue states that release claims and trigger cleanup.                                                                                                                                           |
| `query_filter`    | string          | No                        | `""` (empty)    | Future dispatches                  | Adapter-defined query fragment appended to candidate and terminal-state queries. Passed to the adapter without interpretation. For Jira: JQL fragment (e.g., `"labels = 'agent-ready'"`).       |
| `handoff_state`   | string          | No                        | _(absent)_      | Future worker exits                | Target tracker state for orchestrator-initiated handoff after successful worker run. When absent, no handoff transition is performed.                                                           |
| `in_progress_state` | string        | No                        | _(absent)_      | Future dispatches                  | Target tracker state for dispatch-time transition at the start of each worker attempt. When absent, no dispatch-time transition is performed. Must be in `active_states`. Must not collide with `terminal_states` or `handoff_state`. |

**`active_states` / `terminal_states` validation rules:**

- Both default to empty. At startup, if **both** lists are empty,
  validation fails with an error — at least one of the two must be configured.
- An issue is dispatch-eligible only if its tracker state appears in `active_states`.
  With an empty `active_states` list no issues will be dispatched even if other configuration
  is valid.

**`handoff_state` validation rules:**

- Supports `$VAR` environment indirection.
- When set, must be a non-empty string after `$VAR` resolution. Empty resolution is a
  configuration error.
- Must **not** appear in `active_states` (would cause immediate re-dispatch after handoff).
- Must **not** appear in `terminal_states` (handoff is not terminal — the issue may return
  to active for further work).
- Requires **write permissions** on the tracker API token. For Jira: `write:jira-work`
  (classic) or `write:issue:jira` (granular).

**`in_progress_state` validation rules:**

- Supports `$VAR` environment indirection.
- When set, must be a non-empty string after `$VAR` resolution. Empty resolution is a
  configuration error.
- Must appear in `active_states` (case-insensitive). If the issue transitions to a state
  outside `active_states`, reconciliation would immediately cancel the worker.
- Must **not** appear in `terminal_states` (case-insensitive). A terminal state would
  trigger workspace cleanup on the next reconciliation tick.
- Must **not** collide with `handoff_state` (case-insensitive). The two transitions represent
  different lifecycle phases — dispatch vs. exit.
- Transition failure at runtime is non-fatal: the worker logs a warning and continues to
  workspace preparation.
- If the issue is already in the target state (case-insensitive), the transition API call
  is skipped and a debug-level message is logged.
- Requires **write permissions** on the tracker API token (same as `handoff_state`).

**`api_key` environment resolution:**

The `api_key` field uses full environment expansion (`$VAR`, `${VAR}`, and mixed content
at any position in the string).
All other `$VAR`-supporting fields in `tracker` use targeted resolution: if the trimmed
value starts with `$`, the entire string is expanded via `os.ExpandEnv` (for example,
`$HOST/api/rest` expands as expected). Values that do not start with `$` are returned
unchanged, preserving literal URI strings.

---

### 2.3 `polling` — Poll Loop Timing

```yaml
polling:
  interval_ms: 30000
```

| Field         | Type                      | Required | Default | Dynamic Reload                           | Description                       |
| ------------- | ------------------------- | -------- | ------- | ---------------------------------------- | --------------------------------- |
| `interval_ms` | integer or string integer | No       | `30000` | **Yes** — affects future tick scheduling | Milliseconds between poll cycles. |

---

### 2.4 `workspace` — Workspace Root

```yaml
workspace:
  root: ~/workspace/sortie
```

| Field  | Type                  | Required | Default                           | Dynamic Reload              | Description                              |
| ------ | --------------------- | -------- | --------------------------------- | --------------------------- | ---------------------------------------- |
| `root` | path string or `$VAR` | No       | `<system-temp>/sortie_workspaces` | Future workspace operations | Base directory for per-issue workspaces. |

**Path resolution:**

- `~` and `~/...` prefixes are expanded to the user's home directory via `os.UserHomeDir()`.
- All `$VAR` and `${VAR}` references anywhere in the string are then expanded via
  `os.ExpandEnv`. This applies in any position, not only to pure `$VAR` values.
- Bare strings with no `~` prefix or `$` references are used as-is. Relative roots are
  allowed but discouraged.

**Per-issue workspace path:** `<workspace.root>/<sanitized_issue_identifier>`

**Workspace key sanitization:** Only `[A-Za-z0-9._-]` are allowed. All other characters in
the issue identifier are replaced with `_`.

#### Changing `workspace.root`

> **Warning:** Changing `workspace.root` and restarting the orchestrator will leave the
> old workspace directory on disk. Sortie's startup cleanup scans only the path that is
> currently configured, so any directories under the previous root become orphans and
> accumulate disk space until removed manually.

**Why this happens:** On startup, Sortie lists workspace subdirectory names under
`workspace.root`, queries the tracker for their states, and removes any whose issues are
in terminal states. Because the scan is anchored to the configured root, a prior root is
never consulted.

**Disk leak scenario:**

1. Sortie runs with `workspace.root: /data/old_runs` (contains `BUG-1/`, `BUG-2/`).
2. You update config to `workspace.root: /mnt/new_runs` and restart the process.
3. Startup cleanup scans `/mnt/new_runs` — finds nothing to clean.
4. `/data/old_runs/BUG-1` and `/data/old_runs/BUG-2` remain on disk permanently.

**How to migrate `workspace.root` safely:**

1. Stop the Sortie process.
2. Remove workspace directories from the old root: `rm -rf /data/old_runs/*`
3. Update `workspace.root` in `WORKFLOW.md`.
4. Restart Sortie.

> **Note:** Dynamic changes to `workspace.root` at runtime (without a restart) are safe
> for in-flight sessions — cleanup for already-running sessions uses the path stored in
> memory at the time the session was started.

---

### 2.5 `hooks` — Workspace Lifecycle Hooks

```yaml
hooks:
  after_create: |
    git clone --depth 1 git@github.com:org/repo.git .
    go mod download
  before_run: |
    git fetch origin main
    git checkout -B "sortie/${SORTIE_ISSUE_IDENTIFIER}" origin/main
  after_run: |
    make fmt 2>/dev/null || true
    git add -A
    git diff --cached --quiet || git commit -m "sortie(${SORTIE_ISSUE_IDENTIFIER}): auto"
  before_remove: |
    git push origin --delete "sortie/${SORTIE_ISSUE_IDENTIFIER}" 2>/dev/null || true
  timeout_ms: 120000
```

| Field           | Type                           | Required | Default  | Dynamic Reload         | Description                                                                          |
| --------------- | ------------------------------ | -------- | -------- | ---------------------- | ------------------------------------------------------------------------------------ |
| `after_create`  | multiline shell script or null | No       | _(none)_ | Future hook executions | Runs only when a workspace directory is **newly created**.                           |
| `before_run`    | multiline shell script or null | No       | _(none)_ | Future hook executions | Runs before each agent attempt, after workspace preparation.                         |
| `after_run`     | multiline shell script or null | No       | _(none)_ | Future hook executions | Runs after each agent attempt (success, failure, timeout, or cancellation).          |
| `before_remove` | multiline shell script or null | No       | _(none)_ | Future hook executions | Runs before workspace deletion, if the directory exists.                             |
| `timeout_ms`    | integer                        | No       | `60000`  | Future hook executions | Timeout in milliseconds for all hooks. Non-positive values fall back to the default. |

See [Section 6: Hook Lifecycle Reference](#6-hook-lifecycle-reference) for execution
contract, environment variables, and failure semantics.

---

### 2.6 `agent` — Coding Agent Configuration

```yaml
agent:
  kind: claude-code
  command: claude
  max_turns: 5
  max_sessions: 3
  max_concurrent_agents: 4
  turn_timeout_ms: 3600000
  read_timeout_ms: 5000
  stall_timeout_ms: 300000
  max_retry_backoff_ms: 300000
  max_concurrent_agents_by_state:
    in progress: 3
    to do: 1
```

| Field                            | Type                              | Required                            | Default         | Dynamic Reload                             | Description                                                                                                                                                                      |
| -------------------------------- | --------------------------------- | ----------------------------------- | --------------- | ------------------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `kind`                           | string                            | No                                  | `claude-code`   | Future dispatches                          | Agent adapter identifier. This codebase ships with the `claude-code` adapter. Other kinds (for example, HTTP-based adapters) are available only if you register them separately. |
| `command`                        | string (shell command)            | When adapter requires local process | Adapter-defined | Future dispatches                          | Shell command to launch the agent for adapters that run as a local subprocess (such as `claude-code`). Adapters that do not start a local process ignore this field.             |
| `turn_timeout_ms`                | integer                           | No                                  | `3600000` (1h)  | Future worker attempts                     | Total timeout for a single agent turn.                                                                                                                                           |
| `read_timeout_ms`                | integer                           | No                                  | `5000` (5s)     | Future worker attempts                     | Request/response timeout during startup and synchronous operations.                                                                                                              |
| `stall_timeout_ms`               | integer                           | No                                  | `300000` (5m)   | Future worker attempts                     | Inactivity timeout based on event stream gaps. Set to `0` or negative to **disable** stall detection.                                                                            |
| `max_concurrent_agents`          | integer or string integer         | No                                  | `10`            | **Yes** — affects subsequent dispatch      | Global concurrency limit across all issues.                                                                                                                                      |
| `max_turns`                      | integer                           | No                                  | `20`            | Future dispatches                          | Maximum coding-agent turns per worker session. The worker re-checks tracker state after each turn and starts another turn if the issue is still active, up to this limit.        |
| `max_retry_backoff_ms`           | integer or string integer         | No                                  | `300000` (5m)   | **Yes** — affects future retry scheduling  | Maximum delay cap for exponential backoff on retries.                                                                                                                            |
| `max_concurrent_agents_by_state` | map of `state → positive integer` | No                                  | `{}` (empty)    | **Yes** — affects subsequent dispatch      | Per-state concurrency limits. State keys are normalized to lowercase for lookup. Non-positive or non-numeric entries are silently ignored.                                       |
| `max_sessions`                   | integer                           | No                                  | `0` (unlimited) | **Yes** — affects future retry evaluations | Maximum completed worker sessions per issue before the orchestrator stops re-dispatching. Counted from run history. `0` disables the budget (unlimited). Must be non-negative.   |

**Orchestrator vs adapter fields:** The fields above are consumed by the orchestrator
for scheduling, concurrency, and retry decisions. They are **not** passed through to the
agent adapter. Adapter-specific configuration uses separate pass-through blocks — see
[Section 4.4](#44-adapter-specific-pass-through-config).

---

### 2.7 `db_path` — SQLite Database Path

```yaml
db_path: /var/lib/sortie/state.db
```

| Field     | Type   | Required | Default                            | Dynamic Reload            | Description                        |
| --------- | ------ | -------- | ---------------------------------- | ------------------------- | ---------------------------------- |
| `db_path` | string | No       | `.sortie.db` next to `WORKFLOW.md` | **No** — requires restart | Path for the SQLite database file. |

**Path resolution:**

- Supports `$VAR` environment indirection and `~` home directory expansion.
- Absolute paths are used as-is.
- Relative paths are resolved against the directory containing `WORKFLOW.md`.
- An explicit empty string (`db_path: ""`) is equivalent to omitting the field.
- Non-string values are rejected with a configuration error.
- If the value resolves to empty after environment expansion (e.g., unset `$VAR`), startup
  fails.

**Runtime behavior:** `db_path` is read once at startup to open the database connection.
Dynamic reloads update the in-memory config value but have no effect on the already-open
connection. A restart is required to change the database file.

> **Migration note:** Changing `db_path` and restarting causes Sortie to open (or create) the
> new file with a fresh schema. Retry queues and run history from the previous database file
> are **not** migrated automatically. If you need to preserve state, copy the old `.sortie.db`
> to the new path before restarting.

---

## 3. Environment Variable Overrides

Sortie supports a curated set of `SORTIE_*` environment variables that override YAML front
matter values. This enables twelve-factor app deployment patterns: operators inject secrets,
endpoint URLs, and tuning parameters via environment rather than committing them to a
workflow file.

### 3.1 Source precedence

Configuration sources are resolved in the following order (highest to lowest):

1. **Workflow file path selection** (runtime setting → cwd default).
2. **`SORTIE_*` real environment variables**.
3. **`.env` file values** (when `SORTIE_ENV_FILE` or `--env-file` is set).
4. **YAML front matter values**.
5. **`$VAR` indirection** inside YAML values — applies only to values that survive the
   merge (fields not overridden by env).
6. **Built-in defaults**.

An env override replaces the YAML value in the raw config map *before* `$VAR` expansion
and section builders run. If `WORKFLOW.md` says `api_key: $MY_TOKEN` and
`SORTIE_TRACKER_API_KEY=secret`, the env var value `secret` replaces the YAML value
entirely. The `$MY_TOKEN` indirection never executes for that field.

### 3.2 Curated variable list

Each variable maps to exactly one config field. The naming convention is
`SORTIE_<SECTION>_<FIELD>` with underscores separating words.

#### Tracker

| Environment variable                       | Config field                     | Type   | Notes                          |
| ------------------------------------------ | -------------------------------- | ------ | ------------------------------ |
| `SORTIE_TRACKER_KIND`                      | `tracker.kind`                   | string |                                |
| `SORTIE_TRACKER_ENDPOINT`                  | `tracker.endpoint`               | string |                                |
| `SORTIE_TRACKER_API_KEY`                   | `tracker.api_key`                | string | Secret; MUST NOT be logged     |
| `SORTIE_TRACKER_PROJECT`                   | `tracker.project`                | string |                                |
| `SORTIE_TRACKER_ACTIVE_STATES`             | `tracker.active_states`          | csv    | Comma-separated list           |
| `SORTIE_TRACKER_TERMINAL_STATES`           | `tracker.terminal_states`        | csv    | Comma-separated list           |
| `SORTIE_TRACKER_QUERY_FILTER`              | `tracker.query_filter`           | string |                                |
| `SORTIE_TRACKER_HANDOFF_STATE`             | `tracker.handoff_state`          | string |                                |
| `SORTIE_TRACKER_IN_PROGRESS_STATE`         | `tracker.in_progress_state`      | string |                                |
| `SORTIE_TRACKER_COMMENTS_ON_DISPATCH`      | `tracker.comments.on_dispatch`   | bool   | `true`/`false`/`1`/`0`        |
| `SORTIE_TRACKER_COMMENTS_ON_COMPLETION`    | `tracker.comments.on_completion` | bool   | `true`/`false`/`1`/`0`        |
| `SORTIE_TRACKER_COMMENTS_ON_FAILURE`       | `tracker.comments.on_failure`    | bool   | `true`/`false`/`1`/`0`        |

#### Polling

| Environment variable         | Config field          | Type | Notes |
| ---------------------------- | --------------------- | ---- | ----- |
| `SORTIE_POLLING_INTERVAL_MS` | `polling.interval_ms` | int  |       |

#### Workspace

| Environment variable    | Config field     | Type   | Notes                                 |
| ----------------------- | ---------------- | ------ | ------------------------------------- |
| `SORTIE_WORKSPACE_ROOT` | `workspace.root` | string | `~` expansion applies; `$VAR` skipped |

#### Agent

| Environment variable                 | Config field                  | Type   | Notes |
| ------------------------------------ | ----------------------------- | ------ | ----- |
| `SORTIE_AGENT_KIND`                  | `agent.kind`                  | string |       |
| `SORTIE_AGENT_COMMAND`               | `agent.command`               | string |       |
| `SORTIE_AGENT_TURN_TIMEOUT_MS`       | `agent.turn_timeout_ms`       | int    |       |
| `SORTIE_AGENT_READ_TIMEOUT_MS`       | `agent.read_timeout_ms`       | int    |       |
| `SORTIE_AGENT_STALL_TIMEOUT_MS`      | `agent.stall_timeout_ms`      | int    |       |
| `SORTIE_AGENT_MAX_CONCURRENT_AGENTS` | `agent.max_concurrent_agents` | int    |       |
| `SORTIE_AGENT_MAX_TURNS`             | `agent.max_turns`             | int    |       |
| `SORTIE_AGENT_MAX_RETRY_BACKOFF_MS`  | `agent.max_retry_backoff_ms`  | int    |       |
| `SORTIE_AGENT_MAX_SESSIONS`          | `agent.max_sessions`          | int    |       |

#### Top-level

| Environment variable | Config field | Type   | Notes                                 |
| -------------------- | ------------ | ------ | ------------------------------------- |
| `SORTIE_DB_PATH`     | `db_path`    | string | `~` expansion applies; `$VAR` skipped |

#### Control variable

| Environment variable | Purpose                     | Type   | Notes                              |
| -------------------- | --------------------------- | ------ | ---------------------------------- |
| `SORTIE_ENV_FILE`    | Path to `.env` file to load | string | Default: empty (no `.env` loading) |

### 3.3 Type coercion

All environment variable values are strings. The override layer coerces them to the
expected type before section builders run.

| Target type  | Coercion rule                                                                         | Error behavior                              |
| ------------ | ------------------------------------------------------------------------------------- | ------------------------------------------- |
| `string`     | Used as-is.                                                                           | N/A                                         |
| `int`        | `strconv.Atoi` after trimming whitespace.                                             | `*ConfigError` naming the `SORTIE_*` env var |
| `bool`       | `"true"`, `"1"` → `true`; `"false"`, `"0"` → `false` (case-insensitive).            | `*ConfigError` naming the `SORTIE_*` env var |
| `csv`        | `strings.Split(val, ",")` then trim each element; empty elements discarded.           | N/A                                         |

**CSV encoding for list fields** (`active_states`, `terminal_states`):

```
SORTIE_TRACKER_ACTIVE_STATES="To Do,In Progress"
SORTIE_TRACKER_TERMINAL_STATES="Done,Won't Do"
```

- Items are trimmed of leading/trailing whitespace.
- Empty items (from trailing commas or `,,`) are discarded.
- If the environment variable is unset or set to an empty string, the YAML-configured
  states are used; there is no environment override to force an empty list.
- State values preserve original casing.

### 3.4 `.env` file support

Sortie supports an optional `.env` file for operators who prefer file-based secrets over
shell environment.

#### Loading

- `.env` loading is **opt-in**: set `SORTIE_ENV_FILE=/path/to/.env` as a real environment
  variable, or pass `--env-file /path/to/.env` on the CLI.
- Sortie does **not** auto-discover `.env` in the working directory. Operators MUST opt
  in explicitly.
- When the path is set but the file does not exist, Sortie logs a warning and continues
  without `.env` values.
- When the file exists but has parse errors, Sortie fails startup with an error
  identifying the file and line number.

#### File format

```
# Comment lines start with #
SORTIE_TRACKER_API_KEY="tok_abc123"
SORTIE_TRACKER_ENDPOINT=https://mycompany.atlassian.net
SORTIE_POLLING_INTERVAL_MS=60000
```

Rules:

- One `KEY=VALUE` per line.
- Lines starting with `#` (after optional whitespace) are comments.
- Empty lines are ignored.
- Leading/trailing whitespace on keys and values is trimmed.
- Values MAY be quoted with single or double quotes; quotes are stripped but no escape
  processing is performed.
- Keys MUST match `[A-Za-z_][A-Za-z0-9_]*`.
- Only `SORTIE_*` prefixed keys are loaded; other keys are silently ignored.
- Variable interpolation within `.env` values is **not** supported.

#### Precedence

Real environment variables always win over `.env` file values. The `.env` file provides
defaults for env vars not already set in the process environment. The `--env-file` CLI
flag takes precedence over the `SORTIE_ENV_FILE` environment variable when resolving the
file path.

### 3.5 Interaction with `$VAR` indirection

Values injected by environment overrides are already fully resolved. They MUST NOT be
passed through `os.ExpandEnv`, `resolveEnv`, or `resolveEnvRef`. This prevents
double-expansion that would corrupt values containing `$` characters.

For path fields (`workspace.root`, `db_path`), tilde (`~`) expansion still applies to
env-sourced values. Only `$VAR` expansion is skipped.

**Example:** If `SORTIE_TRACKER_API_KEY=tok$5abc` is set, the literal value `tok$5abc` is
used as the API key. Without this guard, `os.ExpandEnv` would attempt to expand `$5abc`
as an environment variable reference.

### 3.6 Fields not overridable via env

| Config field                           | Reason                                                          |
| -------------------------------------- | --------------------------------------------------------------- |
| `hooks.after_create`                   | Multiline shell scripts; not representable as single env var    |
| `hooks.before_run`                     | Same as above                                                   |
| `hooks.after_run`                      | Same as above                                                   |
| `hooks.before_remove`                  | Same as above                                                   |
| `hooks.timeout_ms`                     | Low-risk tuning; hooks are rarely changed per-environment       |
| `agent.max_concurrent_agents_by_state` | Complex map type; no clean single-value representation          |
| Extensions (`server`, `worker`, etc.)  | Extension-defined; would couple core env parsing to extensions  |
| `logging.level` (via extensions)       | Resolved from `--log-level` flag; not part of typed config layer |

### 3.7 Dynamic reload

On WORKFLOW.md reload, `applyEnvOverrides` re-reads both `os.Getenv` and the `.env` file.
Env overrides merge into the fresh raw map before section builders run.

- **`.env` file changes are picked up** on each reload. The `.env` file itself is not
  watched by fsnotify — only WORKFLOW.md changes trigger reload.
- **Real environment variable changes require a process restart.** `os.Getenv` reads the
  process environment block set at startup. This is standard Unix process semantics.

For configuration values that need to change without restarting, use the `.env` file and
trigger a WORKFLOW.md reload.

---

## 4. Extensions

The front matter is extensible. Unknown top-level keys are collected into an `Extensions`
map and are not validated by the core schema. Extensions should document their own field
schemas, defaults, and reload behavior.

### 4.1 `server.port` — HTTP Server

```yaml
server:
  port: 8642
```

| Field         | Type    | Required | Default                      | Dynamic Reload            | Description                                                  |
| ------------- | ------- | -------- | ---------------------------- | ------------------------- | ------------------------------------------------------------ |
| `server.port` | integer | No       | _(absent — server disabled)_ | **No** — requires restart | TCP port for the embedded HTTP observability server.          |

When `server.port` is set (or `--port` is passed on the CLI), Sortie starts an HTTP
server on `127.0.0.1:<port>` exposing a JSON API for runtime observability and
operational control. The CLI `--port` flag takes precedence over `server.port`.
Port `0` requests an ephemeral OS-assigned port.

#### API Endpoints

| Method | Path                     | Description                                                        |
| ------ | ------------------------ | ------------------------------------------------------------------ |
| GET    | `/`                      | HTML dashboard — server-rendered status page with running sessions, retry queue, token totals, timing breakdown, and recent events. Auto-refreshes. |
| GET    | `/livez`                 | Liveness probe. Returns 200 while the process is running and not draining, 503 during graceful shutdown. No I/O. |
| GET    | `/readyz`                | Readiness probe. Returns 200 when database, preflight, and workflow are healthy. Returns 503 with per-check status when any dependency fails. |
| GET    | `/api/v1/state`          | System-wide runtime snapshot (running sessions, retry queue, aggregate token/runtime totals, rate limits). |
| GET    | `/api/v1/{identifier}`   | Per-issue detail for a specific issue identifier. Returns 404 for unknown issues. |
| POST   | `/api/v1/refresh`        | Trigger an immediate poll+reconciliation cycle. Returns 202 Accepted normally, 409 Conflict during graceful shutdown. Best-effort; repeated requests are coalesced. |
| GET    | `/metrics`               | Prometheus exposition-format scrape endpoint. Present only when `github.com/prometheus/client_golang` metrics are enabled (always co-located with the HTTP server). |

All responses use `Content-Type: application/json; charset=utf-8` (JSON endpoints).
Error responses use a standard envelope: `{"error": {"code": "...", "message": "..."}}`.
API endpoints (`/api/v1/*`) return 405 with the JSON error envelope.
Health probes (`/livez`, `/readyz`) return the standard HTTP 405 plain-text response.
The `/metrics` endpoint returns `text/plain` in Prometheus exposition format.
The `/` dashboard returns `text/html`.

#### Health Endpoints

Sortie exposes Kubernetes z-pages health endpoints (`/livez` and `/readyz`) for liveness
and readiness probes.

**`GET /livez`** — Liveness probe. Returns 200 when the process is alive, 503 during
graceful shutdown. No I/O; a single atomic flag check:

```json
{"status": "pass"}
```

During graceful shutdown:

```json
{"status": "fail"}
```

**`GET /readyz`** — Readiness probe. Returns 200 when all dependencies are healthy,
503 when any check fails. Checks: SQLite database ping, dispatch preflight validation,
workflow file loaded:

```json
{
  "status": "pass",
  "version": "0.4.0",
  "uptime_seconds": 3842,
  "checks": {
    "database": "pass",
    "preflight": "pass",
    "workflow": "pass"
  }
}
```

When a check fails, the overall status is `"fail"` and the failing check is identified:

```json
{
  "status": "fail",
  "version": "0.4.0",
  "uptime_seconds": 3842,
  "checks": {
    "database": "fail",
    "preflight": "pass",
    "workflow": "pass"
  }
}
```

**Draining behavior.** When `SIGTERM` arrives, Sortie sets a draining flag before the
orchestrator begins its worker drain phase. Both `/livez` and `/readyz` return 503 once
the flag is set. The HTTP listener remains open during drain so K8s probes receive proper
HTTP responses. After the orchestrator drain completes, the listener closes and new
connections are refused.

#### `GET /api/v1/state` — Runtime Snapshot

Returns the system-wide runtime state including running sessions, retry queue,
aggregate token/runtime totals, and rate limits.

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
      "workspace_path": "/tmp/sortie_workspaces/MT-649",
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
  "rate_limits": {}
}
```

**Per-session fields:**

| Field               | Type              | Description                                                                                                                                |
| ------------------- | ----------------- | ------------------------------------------------------------------------------------------------------------------------------------------ |
| `tokens`                  | object            | Token counts for this session: `input_tokens`, `output_tokens`, `total_tokens`, `cache_read_tokens`.                                      |
| `tokens.cache_read_tokens` | integer          | Cumulative cache-read token count. Reflects tokens served from the LLM provider's prompt cache rather than reprocessed. Zero when the agent adapter does not report cache data. |
| `model_name`              | string or absent  | LLM model identifier reported by the agent (e.g. `"claude-sonnet-4-20250514"`). Omitted when the adapter does not report a model.         |
| `api_request_count` | integer           | Number of LLM API requests made during this session. Incremented once per `token_usage` event from the agent adapter.                     |
| `requests_by_model` | object or absent  | Map of model name to request count (e.g. `{"claude-sonnet-4-20250514": 3}`). Omitted when no model data is available. Enables tracking model usage when the agent switches models mid-session. |
| `tool_time_percent` | number or `null`  | Cumulative tool call execution time as a percentage of session wall-clock time. Computed at response time. `null` when no tool timing data has been received. |
| `api_time_percent`  | number or `null`  | Cumulative LLM API response wait time as a percentage of session wall-clock time. Computed at response time. `null` when no API timing data has been received. |

**Aggregate totals:**

| Field               | Type    | Description                                                                                             |
| ------------------- | ------- | ------------------------------------------------------------------------------------------------------- |
| `input_tokens`      | integer | Total input tokens consumed across all sessions (current and completed).                                |
| `output_tokens`     | integer | Total output tokens consumed.                                                                           |
| `total_tokens`      | integer | Total tokens consumed.                                                                                  |
| `cache_read_tokens` | integer | Total cache-read tokens across all sessions. Follows the same cumulative-delta accounting as other token counters. |
| `seconds_running`   | number  | Aggregate wall-clock runtime — completed-session time plus elapsed time from currently running sessions. |

#### `GET /api/v1/{identifier}` — Per-Issue Detail

Returns issue-specific runtime and debug details for a single issue. Returns `404`
with `{"error":{"code":"issue_not_found","message":"..."}}` when the identifier is
not in current orchestrator state.

```json
{
  "issue_identifier": "MT-649",
  "issue_id": "abc123",
  "status": "running",
  "workspace": {
    "path": "/tmp/sortie_workspaces/MT-649"
  },
  "attempts": {
    "restart_count": 0,
    "current_retry_attempt": 0
  },
  "running": {
    "session_id": "thread-1-turn-1",
    "turn_count": 7,
    "state": "In Progress",
    "started_at": "2026-02-24T20:10:12Z",
    "last_event": "turn_completed",
    "last_message": "Working on tests",
    "last_event_at": "2026-02-24T20:14:59Z",
    "workspace_path": "/tmp/sortie_workspaces/MT-649",
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
  },
  "retry": null,
  "recent_events": [],
  "last_error": null,
  "tracked": {}
}
```

The `running` object uses the same per-session field schema as `GET /api/v1/state`
(see the per-session fields table above). When the issue is retrying rather than
running, `running` is `null` and `retry` contains the retry entry.

#### `POST /api/v1/refresh`

Triggers an immediate poll+reconciliation cycle. The endpoint is best-effort: repeated
requests while a refresh is already pending are coalesced. During graceful shutdown the
endpoint rejects requests with `409 Conflict`.

**Normal response** — refresh signal accepted:

```http
HTTP/1.1 202 Accepted
{"queued": true, "coalesced": false, "requested_at": "...", "operations": ["poll", "reconcile"]}
```

**Coalesced response** — a refresh was already pending:

```http
HTTP/1.1 202 Accepted
{"queued": true, "coalesced": true, "requested_at": "...", "operations": ["poll", "reconcile"]}
```

**Drain rejection** — orchestrator is shutting down:

```http
HTTP/1.1 409 Conflict
{"queued": false, "coalesced": false, "requested_at": "...", "operations": []}
```

Callers should check the `queued` field or HTTP status to determine whether the refresh
will be processed. A `409` response indicates the server is draining and the caller
should retry against another instance or wait for the restart.

**Kubernetes probe configuration.** Sortie's graceful shutdown cancels running agents
before closing the HTTP listener. Configure `terminationGracePeriodSeconds` and liveness
probe tolerance to exceed the expected drain duration (default: 30 seconds):

```yaml
livenessProbe:
  httpGet:
    path: /livez
    port: 8642
  periodSeconds: 10
  failureThreshold: 6    # 60s tolerance for drain
readinessProbe:
  httpGet:
    path: /readyz
    port: 8642
  periodSeconds: 10
  failureThreshold: 1
terminationGracePeriodSeconds: 90
```

#### `GET /metrics` — Prometheus Scrape Endpoint

When the HTTP server is enabled, Sortie exposes a Prometheus exposition-format endpoint
at `/metrics` for integration with Prometheus, Grafana, and other monitoring stacks. The
endpoint is co-located with the JSON API and dashboard on the same address and port — no
separate configuration is required.

The endpoint uses a dedicated `prometheus.Registry` (not the Go default global) to
prevent metric pollution. Standard Go runtime (`go_*`) and process (`process_*`) metrics
are included alongside Sortie-specific metrics.

**Sortie-defined metrics:**

| Name                                            | Type      | Labels                      | Description                                                    |
| ----------------------------------------------- | --------- | --------------------------- | -------------------------------------------------------------- |
| `sortie_sessions_running`                       | Gauge     | —                           | Number of agent sessions currently executing.                  |
| `sortie_sessions_retrying`                      | Gauge     | —                           | Number of issues in the retry queue.                           |
| `sortie_slots_available`                        | Gauge     | —                           | Remaining dispatch capacity under current concurrency limits.  |
| `sortie_active_sessions_elapsed_seconds`        | Gauge     | —                           | Cumulative wall-clock elapsed time across running sessions.    |
| `sortie_tokens_total`                           | Counter   | `type`                      | Tokens consumed, by type (`input`, `output`, `cache_read`).    |
| `sortie_agent_runtime_seconds_total`            | Counter   | —                           | Cumulative agent-session wall-clock time for completed sessions. |
| `sortie_dispatches_total`                       | Counter   | `outcome`                   | Dispatch attempts (`success`, `error`).                        |
| `sortie_worker_exits_total`                     | Counter   | `exit_type`                 | Worker exits (`normal`, `error`, `cancelled`).                 |
| `sortie_retries_total`                          | Counter   | `trigger`                   | Retry schedule events (`error`, `continuation`, `timer`, `stall`). |
| `sortie_reconciliation_actions_total`           | Counter   | `action`                    | Reconciliation outcomes (`stop`, `cleanup`, `keep`).           |
| `sortie_poll_cycles_total`                      | Counter   | `result`                    | Poll tick completions (`success`, `error`, `skipped`).         |
| `sortie_tracker_requests_total`                 | Counter   | `operation`, `result`       | Tracker adapter API calls by operation and result.             |
| `sortie_handoff_transitions_total`              | Counter   | `result`                    | Handoff-state transition attempts (`success`, `error`, `skipped`). |
| `sortie_tool_calls_total`                       | Counter   | `tool`, `result`            | Agent tool call completions by tool name and result.           |
| `sortie_poll_duration_seconds`                  | Histogram | —                           | Wall-clock time per poll cycle.                                |
| `sortie_worker_duration_seconds`                | Histogram | `exit_type`                 | Worker session wall-clock time.                                |
| `sortie_build_info`                             | Gauge     | `version`, `go_version`     | Always `1`; carries build metadata as labels.                  |
| `sortie_ssh_host_usage`                         | Gauge     | `host`                      | Current session count per SSH host.                            |

Example scrape:

```
$ curl -s http://localhost:8642/metrics | grep sortie_sessions_running
# HELP sortie_sessions_running Number of agent sessions currently executing.
# TYPE sortie_sessions_running gauge
sortie_sessions_running 2
```

### 4.2 `logging.level` — Log Verbosity

```yaml
logging:
  level: debug
```

| Field | Type | Required | Default | Dynamic Reload | Description |
|---|---|---|---|---|---|
| `logging.level` | string | No | `info` | **No** — requires restart | Log verbosity: `debug`, `info`, `warn`, `error`. CLI `--log-level` overrides. |

When `logging.level` is set, Sortie initializes the log handler at the specified
verbosity after the workflow config is loaded. The CLI `--log-level` flag takes
precedence when both are present. Accepted values: `debug`, `info`, `warn`,
`error` (case-insensitive). Unknown values cause startup failure with exit code 1.

### 4.3 `worker` — SSH Worker Extension

```yaml
worker:
  ssh_hosts:
    - build01.internal
    - build02.internal
  max_concurrent_agents_per_host: 2
```

When `worker.ssh_hosts` is configured, Sortie dispatches agent runs to remote
hosts over SSH using the system `ssh` binary. Each dispatch selects the host
with the fewest active sessions (least-loaded selection). When a per-host
concurrency cap is set, hosts at capacity are skipped. On retry, the previous
host is preferred if it still has capacity.

When `worker.ssh_hosts` is absent or empty, all agents run locally on the
host where Sortie is started (the default behavior).

| Field                                   | Type             | Required | Default                        | Description                                                                                 |
| --------------------------------------- | ---------------- | -------- | ------------------------------ | ------------------------------------------------------------------------------------------- |
| `worker.ssh_hosts`                      | list of strings  | No       | _(absent — work runs locally)_ | SSH host targets for remote agent execution.                                                |
| `worker.max_concurrent_agents_per_host` | positive integer | No       | _(absent)_                     | Per-host concurrency cap shared across configured SSH hosts. Hosts at capacity are skipped. |

#### SSH Hook Environment

When SSH mode is active, all lifecycle hooks (`after_create`, `before_run`,
`after_run`, `before_remove`) receive the `SORTIE_SSH_HOST` environment variable
set to the target host for the current session. Hooks can use this variable to
interact with the remote host — for example:

```bash
# after_create — clone repo on the remote host
ssh "$SORTIE_SSH_HOST" "git clone https://repo.example.com/project.git \"$SORTIE_WORKSPACE\""

# before_run — install dependencies on the remote host
ssh "$SORTIE_SSH_HOST" "cd \"$SORTIE_WORKSPACE\" && npm install"

# after_run — collect artifacts from the remote host
scp "$SORTIE_SSH_HOST:\"$SORTIE_WORKSPACE\"/coverage.out" ./artifacts/

# before_remove — clean up the remote workspace directory
ssh "$SORTIE_SSH_HOST" "rm -rf \"$SORTIE_WORKSPACE\""
```

#### Operator Guidance

- **SSH connectivity is validated at dispatch time**, not at startup. Hosts
  that are temporarily unreachable cause the worker to fail and retry with
  exponential backoff.
- **Process lifecycle:** The remote agent process receives stdin EOF when
  the SSH connection closes (e.g., on cancellation or stall timeout). The
  agent should terminate on stdin EOF or SIGHUP.
- **SSH options:** Sortie sets `ServerAliveInterval=15`,
  `ServerAliveCountMax=3`, and `StrictHostKeyChecking=accept-new` by default.
  Operators should ensure SSH key-based authentication is configured for all
  target hosts.

#### Complete SSH-Mode Example

```yaml
---
tracker:
  kind: jira
  project: PROJ
  active_states:
    - To Do
    - In Progress
  terminal_states:
    - Done

agent:
  kind: claude-code
  max_sessions: 4
  max_turns: 10

workspace:
  root: /srv/sortie/workspaces

worker:
  ssh_hosts:
    - build01.internal
    - build02.internal
    - build03.internal
  max_concurrent_agents_per_host: 2

hooks:
  after_create: |
    ssh "$SORTIE_SSH_HOST" "mkdir -p \"$SORTIE_WORKSPACE\" && git clone https://repo.example.com/project.git \"$SORTIE_WORKSPACE\""
  before_remove: |
    ssh "$SORTIE_SSH_HOST" "rm -rf \"$SORTIE_WORKSPACE\""
---
You are a software engineer. Fix the issue described below.
{{.issue_body}}
```

### 4.4 Adapter-Specific Pass-Through Config

Each adapter (tracker or agent) may define configuration in a top-level object named
after its `kind` value. These values are passed through to the adapter without validation
by the orchestrator core.

**File tracker adapter:**

```yaml
tracker:
  kind: file
  active_states:
    - To Do
    - In Progress
  terminal_states:
    - Done

file:
  path: /path/to/issues.json
```

The `file:` block is forwarded to the file tracker adapter. The `path` field is required
and specifies the filesystem path to a JSON file containing issue records. This adapter
is intended for local testing and CI workflows where a live tracker is not available.

**Claude Code adapter:**

```yaml
claude-code:
  permission_mode: bypassPermissions
  model: claude-sonnet-4-20250514
  max_turns: 50
  max_budget_usd: 5
```

The `claude-code` block is forwarded to the Claude Code adapter, which maps these fields
to CLI flags.

> **Important:** `agent.max_turns` (orchestrator turn-loop limit) and
> `claude-code.max_turns` (CLI `--max-turns` flag) are distinct values. The orchestrator
> limit controls how many turns the worker runs before exiting. The adapter limit controls
> the Claude Code CLI's internal turn budget. They serve different purposes and should
> typically have different values.

**Custom or future adapters (illustrative example):**

```yaml
my-custom-adapter:
  option_one: value
  option_two: true
```

The orchestrator forwards the entire sub-object to the matching adapter without
interpretation. Any adapter you register can read its fields from this block.

---

## 5. Prompt Template Reference

### 5.1 Template Engine

Sortie uses Go [`text/template`](https://pkg.go.dev/text/template) with strict mode
enabled:

```go
template.New("prompt").
    Option("missingkey=error").
    Funcs(promptFuncMap).
    Parse(body)
```

**Strict mode guarantees:**

- Referencing an **unknown variable** fails rendering immediately (does not produce empty
  string).
- Calling an **unknown function** fails rendering immediately.
- `missingkey=error` distinguishes between a map key that is absent (error) and a key
  that is present with a `nil` value (evaluates as falsy in `{{ if }}`).

### 5.2 Template Input Variables

The data map passed to `Execute` contains exactly **three top-level keys**:

#### `issue` — Normalized Issue Object

All fields from the tracker, normalized into a stable structure regardless of the
underlying tracker system.

| Field                | Type            | Description                                                                                                                                      |
| -------------------- | --------------- | ------------------------------------------------------------------------------------------------------------------------------------------------ |
| `.issue.id`          | string          | Stable tracker-internal ID.                                                                                                                      |
| `.issue.identifier`  | string          | Human-readable ticket key (e.g., `PROJ-123`).                                                                                                    |
| `.issue.title`       | string          | Issue summary/title.                                                                                                                             |
| `.issue.description` | string          | Full issue description body. Empty string when absent.                                                                                           |
| `.issue.priority`    | integer or nil  | Numeric priority (lower = higher priority). `nil` when unavailable.                                                                              |
| `.issue.state`       | string          | Current tracker state name.                                                                                                                      |
| `.issue.branch_name` | string          | Tracker-provided branch metadata. Empty string when absent.                                                                                      |
| `.issue.url`         | string          | Web URL to the issue in the tracker. Empty string when absent.                                                                                   |
| `.issue.labels`      | list of strings | Labels, normalized to lowercase. Non-nil empty slice when no labels exist.                                                                       |
| `.issue.assignee`    | string          | Assignee identity from the tracker. Empty string when absent.                                                                                    |
| `.issue.issue_type`  | string          | Tracker-defined type (Bug, Story, Task, Epic). Empty string when absent.                                                                         |
| `.issue.parent`      | object or nil   | Parent issue reference (sub-task relationship). `nil` when no parent.                                                                            |
| `.issue.comments`    | list or nil     | Comment records (feedback, review notes, workpad entries). `nil` means comments were not fetched; an empty non-nil list means no comments exist. |
| `.issue.blocked_by`  | list of objects | Blocker references, each with `.id`, `.identifier`, `.state`. Non-nil empty list when no blockers.                                               |
| `.issue.created_at`  | string          | ISO-8601 creation timestamp. Empty string when absent.                                                                                           |
| `.issue.updated_at`  | string          | ISO-8601 last-update timestamp. Empty string when absent.                                                                                        |

#### `attempt` — Retry Counter

| Value          | Meaning                                                                                   |
| -------------- | ----------------------------------------------------------------------------------------- |
| Integer `0`    | First try, no prior worker failures in this session.                                      |
| Integer `>= 1` | Retry try number after a worker failure. The value does not change on continuation turns. |

**Template usage:** Use `{{ if .attempt }}` to distinguish first tries from retries.
`attempt` is always an integer; on the first try it is `0`, so `{{ if .attempt }}`
evaluates to `false`, and on retries it is `>= 1`, so `{{ if .attempt }}` evaluates to
`true`. Continuation turns within the same session reuse the same `attempt` value.

#### `run` — Per-Turn Metadata

| Field                  | Type    | Description                                                                                                        |
| ---------------------- | ------- | ------------------------------------------------------------------------------------------------------------------ |
| `.run.turn_number`     | integer | Current turn number within the session.                                                                            |
| `.run.max_turns`       | integer | Configured maximum turns per session.                                                                              |
| `.run.is_continuation` | boolean | `true` when this is a continuation turn within a multi-turn session (not the first turn, not a retry after error). |

### 5.3 Built-in Functions (FuncMap)

In addition to Go `text/template` built-in actions, Sortie ships a minimal set of
prompt-essential functions. Each is permanent API surface.

| Function | Signature                      | Description                                                                                                                            | Example                                              |
| -------- | ------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------- |
| `toJSON` | `toJSON value → string`        | Serialize any value to compact JSON. Agents parse structured data more reliably from JSON than from Go's default `fmt` representation. | `{{ .issue.labels \| toJSON }}` → `["bug","urgent"]` |
| `join`   | `join separator list → string` | Join a list of strings with a separator.                                                                                               | `{{ .issue.labels \| join ", " }}` → `bug, urgent`   |
| `lower`  | `lower string → string`        | Lowercase a string.                                                                                                                    | `{{ .issue.state \| lower }}` → `in progress`        |

> **Note:** `join` uses pipe syntax with reversed arguments: `{{ .issue.labels | join ", " }}`.
> The separator comes first in the function signature because Go template pipelines pass
> the piped value as the last argument.

### 5.4 Built-in Actions

Go `text/template` provides these built-in actions, all available in workflow templates:

| Action                                                         | Purpose                         |
| -------------------------------------------------------------- | ------------------------------- |
| `{{ if COND }}...{{ else }}...{{ end }}`                       | Conditional branching.          |
| `{{ range LIST }}...{{ end }}`                                 | Iterate over a list or map.     |
| `{{ with VALUE }}...{{ end }}`                                 | Set dot to value if non-empty.  |
| `{{ and A B }}`                                                | Logical AND.                    |
| `{{ or A B }}`                                                 | Logical OR.                     |
| `{{ not A }}`                                                  | Logical NOT.                    |
| `{{ eq A B }}`, `{{ ne A B }}`                                 | Equality / inequality.          |
| `{{ lt A B }}`, `{{ le A B }}`, `{{ gt A B }}`, `{{ ge A B }}` | Comparison.                     |
| `{{ len LIST }}`                                               | Length of list, map, or string. |
| `{{ index MAP KEY }}`                                          | Index into a map or slice.      |
| `{{ print A }}`, `{{ printf FMT A }}`, `{{ println A }}`       | Formatted output.               |
| `{{ call FUNC ARGS }}`                                         | Call a function value.          |

### 5.5 First-Turn vs Continuation Semantics

The prompt template supports three distinct modes within a single file. Workflow authors
use `attempt` and `run.is_continuation` to branch:

| Scenario              | `attempt`      | `run.is_continuation` | Typical template action                                       |
| --------------------- | -------------- | --------------------- | ------------------------------------------------------------- |
| **First run**         | `0`            | `false`               | Full task instructions, context gathering steps.              |
| **Continuation turn** | same as turn 1 | `true`                | Resume guidance — review state, pick up where left off.       |
| **Retry after error** | integer `>= 1` | `false`               | Diagnostic steps — check prior failure, approach differently. |

**Template pattern:**

```
{{ if not .run.is_continuation }}
## First-Run Instructions
Start from scratch. Read the spec. Write tests.
{{ end }}

{{ if .run.is_continuation }}
## Continuation
You are resuming (turn {{ .run.turn_number }} of {{ .run.max_turns }}).
Review current state and continue.
{{ end }}

{{ if and .attempt (not .run.is_continuation) }}
## Retry — Attempt {{ .attempt }}
A previous attempt failed. Diagnose the root cause.
{{ end }}
```

**Turn semantics within a session:**

- The full prompt template is rendered on **every** turn. The runtime always passes the
  full rendered result to the agent, regardless of turn number.
- Workflow authors control what the agent receives on continuation turns by branching on
  `.run.is_continuation`. Without such branching, the agent receives identical instructions
  every turn.
- After each turn, the worker re-checks the tracker issue state. If the issue is
  still active, another turn begins (up to `agent.max_turns`).

### 5.6 Fallback Prompt Behavior

- On **continuation turns** (turn number > 1), if the rendered prompt is empty, the
  runtime substitutes a built-in default continuation prompt as a safety net. This covers
  templates that omit `{{ if .run.is_continuation }}` branching.
- On the **first turn**, if the rendered prompt is empty, no fallback is applied — the
  empty string is passed to the agent as-is.
- Workflow file read/parse failures are validation errors and do **not** silently fall
  back to a default prompt.

### 5.7 Common Patterns and Pitfalls

#### Dot context inside `{{ range }}`

Inside `{{ range .issue.labels }}`, the dot (`.`) refers to the **current list element**,
not the root data map. To access top-level variables inside a range block, use the
dollar-sign prefix:

```
{{ range .issue.labels }}
- Label: {{ . }}  (issue: {{ $.issue.identifier }})
{{ end }}
```

> **Common mistake:** Writing `{{ .issue.identifier }}` inside `{{ range }}` produces an
> error because `.issue` does not exist on a string element. Use `{{ $.issue.identifier }}`
> instead.

#### Nil-safe conditionals

Fields that may be empty (`description`, `url`, `assignee`, etc.) should be guarded to
avoid rendering blank sections. Empty string evaluates to `false` in `{{ if }}`, making
this pattern safe whether the field is empty or absent:

```
{{ if .issue.description }}
### Description
{{ .issue.description }}
{{ end }}
```

#### Rendering blockers

```
{{ if .issue.blocked_by }}
## Blockers
{{ range .issue.blocked_by }}- **{{ .identifier }}**{{ if .state }} ({{ .state }}){{ end }}
{{ end }}
{{ end }}
```

#### JSON serialization for complex data

When the agent needs structured data, use `toJSON` instead of verbose range loops:

```
Blockers: {{ .issue.blocked_by | toJSON }}
```

---

## 6. Hook Lifecycle Reference

### 6.1 Execution Contract

Hooks execute as shell scripts in a local shell context:

- **Shell:** `sh -c <script>` (POSIX default). The orchestrator invokes hooks via `sh`,
  not `bash`. There is no `hooks.shell` configuration field.
- **Working directory:** The per-issue workspace directory.
- **Timeout:** Controlled by `hooks.timeout_ms` (default: 60,000 ms).
- **Logging:** Hook start, completion, failures, and timeouts are logged by the
  orchestrator.

> **Login shell environments:** If a hook requires a login shell (e.g., for `nvm`, `rbenv`,
> or other profile-dependent tooling), nest the invocation explicitly inside the script:
>
> ```yaml
> hooks:
>   after_create: |
>     bash -lc 'nvm use 20 && npm ci'
> ```
>
> The outer `sh -c` invocation is transparent — it executes the `bash -lc` command, which
> then sources the login profile and runs the inner script with the full environment.

**Execution order in a typical lifecycle:**

```
Issue dispatched
  │
  ├─ Workspace directory created (first time only)
  │   └─ hooks.after_create
  │
  ├─ hooks.before_run
  │   └─ Agent session starts
  │       └─ Agent turns execute...
  │           └─ Agent session ends
  │               └─ hooks.after_run
  │
  ├─ (retry or continuation — repeat before_run → agent → after_run)
  │
  └─ Issue reaches terminal state
      ├─ hooks.before_remove
      └─ Workspace directory deleted
```

### 6.2 Hook Environment Variables

All hooks receive the following environment variables:

| Variable                  | Description                                         |
| ------------------------- | --------------------------------------------------- |
| `SORTIE_ISSUE_ID`         | Stable tracker-internal issue ID.                   |
| `SORTIE_ISSUE_IDENTIFIER` | Human-readable ticket key (e.g., `PROJ-123`).       |
| `SORTIE_WORKSPACE`        | Absolute path to the per-issue workspace directory. |
| `SORTIE_ATTEMPT`          | Current attempt number (integer).                   |

These allow hooks to make decisions without parsing orchestrator internals.

**Restricted environment inheritance:**

Hook subprocesses **do not** inherit the full environment of the Sortie process. They
receive a restricted environment consisting of:

- A small allowlist of standard POSIX and infrastructure variables: `PATH`, `HOME`,
  `SHELL`, `TMPDIR`, `USER`, `LOGNAME`, `TERM`, `LANG`, `LC_ALL`, `SSH_AUTH_SOCK`.
- All parent environment variables whose names start with `SORTIE_`.
- The `SORTIE_ISSUE_ID`, `SORTIE_ISSUE_IDENTIFIER`, `SORTIE_WORKSPACE`, and
  `SORTIE_ATTEMPT` variables injected by the orchestrator (listed above).

All other parent variables — including secrets such as `JIRA_API_TOKEN`,
`AWS_ACCESS_KEY_ID`, `GOOGLE_APPLICATION_CREDENTIALS`, and similar values — are
**stripped and not available** inside hooks.

If a hook needs additional secrets or environment values, arrange for them explicitly:

- Expose the value under a `SORTIE_`-prefixed name in the Sortie process environment
  (for example, `SORTIE_JIRA_API_TOKEN`) and read that variable inside the hook.
- Load credentials from a file or external secrets manager inside the hook script
  (for example, `source /etc/sortie/hooks-env` or `aws sts get-caller-identity`).

### 6.3 Failure Semantics

| Hook            | When it runs                      | Failure behavior                                                                                            |
| --------------- | --------------------------------- | ----------------------------------------------------------------------------------------------------------- |
| `after_create`  | Workspace directory newly created | **Fatal** — aborts workspace creation. The partially-prepared directory may be removed.                     |
| `before_run`    | Before each agent attempt         | **Fatal** — aborts the current run attempt. The orchestrator treats this as a worker failure and may retry. |
| `after_run`     | After each agent attempt          | **Logged and ignored** — the run result is already determined.                                              |
| `before_remove` | Before workspace deletion         | **Logged and ignored** — cleanup still proceeds.                                                            |

Timeouts are treated the same as failures for each hook's failure semantics.

### 6.4 Inline Scripts vs File Paths

Hook values are multiline shell script strings defined inline using YAML literal block
syntax (`|`). For complex hooks, consider extracting scripts to separate files and
referencing them:

```yaml
hooks:
  # Inline one-liner
  before_run: git checkout -B "sortie/${SORTIE_ISSUE_IDENTIFIER}" origin/main

  # Inline multi-line
  after_create: |
    git clone --depth 1 git@github.com:org/repo.git .
    go mod download

  # File reference (script must be executable)
  after_run: ./hooks/post-run.sh
```

> **Caveat:** Inline scripts are triple-nested (Bash in YAML in Markdown). IDEs cannot
> provide syntax highlighting or shell linting for inline scripts. For non-trivial logic,
> external scripts are more maintainable.

---

## 7. Dynamic Reload Behavior

### 7.1 General Reload Semantics

Sortie watches `WORKFLOW.md` for filesystem changes and automatically re-reads and
re-applies configuration and prompt template without restart.

**Key guarantees:**

- Reloaded config applies to **future** dispatch, retry scheduling, reconciliation,
  hook execution, and agent launches.
- **In-flight agent sessions are not restarted** when config changes.
- **Invalid reloads do not crash** the service. Sortie continues operating with the last
  known good configuration and emits an operator-visible error.
- Sortie also performs **defensive re-validation before dispatch** (per tick) in case
  filesystem watch events are missed.
- The file watcher monitors the parent directory to detect atomic-rename saves
  (vim, `sed -i`).

### 7.2 Per-Field Reload Behavior

| Field                                  | Reload behavior                                                                                |
| -------------------------------------- | ---------------------------------------------------------------------------------------------- |
| `tracker.kind`                         | Future dispatches.                                                                             |
| `tracker.endpoint`                     | Future dispatches.                                                                             |
| `tracker.api_key`                      | Future dispatches.                                                                             |
| `tracker.project`                      | Future dispatches.                                                                             |
| `tracker.active_states`                | Future dispatch and reconciliation.                                                            |
| `tracker.terminal_states`              | Future dispatch and reconciliation.                                                            |
| `tracker.query_filter`                 | Future dispatches.                                                                             |
| `tracker.handoff_state`                | Future worker exits, not in-flight sessions.                                                   |
| `tracker.in_progress_state`            | Future dispatches, not in-flight sessions.                                                     |
| `polling.interval_ms`                  | **Immediate** — affects future tick scheduling.                                                |
| `workspace.root`                       | Future workspace operations.                                                                   |
| `hooks.*`                              | Future hook executions.                                                                        |
| `hooks.timeout_ms`                     | Future hook executions.                                                                        |
| `agent.kind`                           | Future dispatches.                                                                             |
| `agent.command`                        | Future dispatches.                                                                             |
| `agent.turn_timeout_ms`                | Future worker attempts, not in-flight sessions.                                                |
| `agent.read_timeout_ms`                | Future worker attempts, not in-flight sessions.                                                |
| `agent.stall_timeout_ms`               | Future worker attempts, not in-flight sessions.                                                |
| `agent.max_concurrent_agents`          | **Immediate** — affects subsequent dispatch decisions.                                         |
| `agent.max_turns`                      | Future dispatches.                                                                             |
| `agent.max_retry_backoff_ms`           | **Immediate** — affects future retry scheduling.                                               |
| `agent.max_concurrent_agents_by_state` | **Immediate** — affects subsequent dispatch decisions.                                         |
| `agent.max_sessions`                   | **Immediate** — affects future retry timer evaluations.                                        |
| `db_path`                              | **No effect** — requires restart. In-memory config updated, but database connection unchanged. |
| `server.port`                          | **No effect** — requires restart.                                                              |
| `logging.level`                        | **No effect** — requires restart.                                                              |
| Prompt template                        | Future worker attempts (including continuation retries), not in-flight continuation turns.     |

**Environment variable overrides and reload:** On each WORKFLOW.md reload, `SORTIE_*`
environment variables and the `.env` file are re-read and merged into the fresh config.
`.env` file changes are picked up on reload. Real environment variable changes require a
process restart (standard Unix process semantics). See
[Section 3.7](#37-dynamic-reload) for details.

---

## 8. Dispatch Preflight Validation

Before dispatching work, the orchestrator validates the workflow configuration. This runs
at two points:

**Startup validation:** Before starting the scheduling loop. If validation fails, startup
is aborted with an operator-visible error.

**Per-tick validation:** Before each dispatch cycle. If validation fails, dispatch is
skipped for that tick, reconciliation remains active, and an error is emitted.

**Validation checks:**

| Check                                          | Error condition                                             |
| ---------------------------------------------- | ----------------------------------------------------------- |
| Workflow file loadable and parseable           | File missing, YAML syntax error, or non-map front matter.   |
| `tracker.kind` present and supported           | Missing, empty, or unregistered adapter.                    |
| `tracker.api_key` present after `$` resolution | Missing or empty when the adapter requires it (e.g., Jira). |
| `tracker.project` present                      | Missing when the adapter requires project scoping.          |
| `agent.command` present and non-empty          | Missing when `agent.kind` requires a local command.         |
| Tracker adapter registered and available       | No adapter registered for the configured `tracker.kind`.    |
| Agent adapter registered and available         | No adapter registered for the configured `agent.kind`.      |

---

## 9. Error Reference

### 9.1 Workflow File Errors

These errors are raised during workflow file loading and prevent dispatch until fixed.

| Error                               | Cause                                                                                                                                                                                                                                                           | Fix                                                                                                                                                                                         |
| ----------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **Missing workflow file**           | The workflow file cannot be read at the configured or default path.                                                                                                                                                                                             | Verify the file exists. Check path spelling. Ensure read permissions. If using a custom path, confirm the CLI argument.                                                                     |
| **Workflow parse error**            | YAML front matter contains syntax errors: invalid YAML between delimiters. A missing closing `---` is **not** itself an error (see [parsing rule 4](#12-parsing-rules)), but it causes the prompt body to be consumed as YAML, which often triggers this error. | Validate YAML syntax (indentation, colons, quoting). Look for tabs where spaces are expected. If the error text includes unexpected content, verify the closing `---` delimiter is present. |
| **Workflow front matter not a map** | YAML front matter decoded to a scalar or list instead of a map/object.                                                                                                                                                                                          | Ensure front matter contains key-value pairs, not a bare value or list. The top level must be a YAML mapping.                                                                               |

### 9.2 Configuration Errors

These errors are raised during typed config construction from the parsed front matter.
Each error identifies the offending field path.

| Error pattern                                                                   | Cause                                                                    | Fix                                                                                                                                  |
| ------------------------------------------------------------------------------- | ------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------ |
| `config: polling.interval_ms: invalid integer value: <val>`                     | Non-integer value for a field expecting an integer.                      | Use a plain integer (e.g., `30000`) or a quoted string integer (e.g., `"30000"`). Remove units, decimals, or non-numeric characters. |
| `config: agent.max_concurrent_agents: invalid integer value: <val>`             | Same as above, for any integer field.                                    | Same fix as above.                                                                                                                   |
| `config: agent.stall_timeout_ms: invalid integer value: <val>`                  | Same as above.                                                           | Same fix.                                                                                                                            |
| `config: agent.max_sessions: must be non-negative`                              | Negative value for `max_sessions`.                                       | Use `0` (unlimited) or a positive integer.                                                                                           |
| `config: tracker.handoff_state: expected string, got <type>`                    | `handoff_state` is not a string (e.g., integer, boolean, list).          | Ensure the value is a string, quoted if necessary.                                                                                   |
| `config: tracker.handoff_state: must not be empty`                              | `handoff_state` is set to an explicit empty string.                      | Provide a valid state name, or omit the field entirely to disable handoff.                                                           |
| `config: tracker.handoff_state: resolved to empty (check environment variable)` | `$VAR` reference resolved to an empty string (variable unset or empty).  | Set the referenced environment variable to a valid state name.                                                                       |
| `config: tracker.handoff_state: "<val>" collides with active state "<state>"`   | `handoff_state` matches one of the `active_states` (case-insensitive).   | Use a state that is not in `active_states`. The handoff state must be distinct from active and terminal states.                      |
| `config: tracker.handoff_state: "<val>" collides with terminal state "<state>"` | `handoff_state` matches one of the `terminal_states` (case-insensitive). | Use a state that is not in `terminal_states`.                                                                                        |
| `config: tracker.in_progress_state: expected string, got <type>`                    | `in_progress_state` is not a string (e.g., integer, boolean, list).          | Ensure the value is a string, quoted if necessary.                                                                                   |
| `config: tracker.in_progress_state: must not be empty`                              | `in_progress_state` is set to an explicit empty string.                      | Provide a valid state name, or omit the field entirely to disable dispatch-time transitions.                                         |
| `config: tracker.in_progress_state: resolved to empty (check environment variable)` | `$VAR` reference resolved to an empty string (variable unset or empty).      | Set the referenced environment variable to a valid state name.                                                                       |
| `config: tracker.in_progress_state: "<val>" collides with terminal state "<state>"` | `in_progress_state` matches one of the `terminal_states` (case-insensitive). | Use a state that is not in `terminal_states`.                                                                                        |
| `config: tracker.in_progress_state: "<val>" is not in active_states...`            | `in_progress_state` is not in `active_states` (case-insensitive).            | Add the state to `active_states`, or use a state already in `active_states`.                                                         |
| `config: tracker.in_progress_state: "<val>" collides with handoff_state "<state>"` | `in_progress_state` matches `handoff_state` (case-insensitive).              | Use different states for dispatch-time and exit-time transitions.                                                                    |
| `config: workspace.root: cannot expand ~: <err>`                                | Home directory expansion failed.                                         | Check that the `HOME` environment variable is set.                                                                                   |
| `config: db_path: expected string, got <type>`                                  | `db_path` is not a string value.                                         | Use a string path value, quoted if necessary.                                                                                        |
| `config: db_path: resolved to empty (check environment variable)`               | `$VAR` reference resolved to empty.                                      | Set the environment variable or use a literal path.                                                                                  |

### 9.3 Environment Variable Errors

These errors are raised when `SORTIE_*` environment variables or `.env` file values fail
type coercion. Each error identifies the env var as the source.

| Error pattern                                                                                                  | Cause                                                     | Fix                                                                              |
| -------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------- | -------------------------------------------------------------------------------- |
| `config: polling.interval_ms: invalid integer value: <val> (from SORTIE_POLLING_INTERVAL_MS)`                  | Non-integer value in an integer env var.                  | Set the env var to a plain integer (e.g., `30000`). Remove units or decimals.    |
| `config: agent.<field>: invalid integer value: <val> (from SORTIE_AGENT_<FIELD>)`                              | Same, for any agent integer field.                        | Same fix as above.                                                               |
| `config: tracker.comments.<field>: invalid boolean value: <val> (expected true/false/1/0) (from SORTIE_TRACKER_COMMENTS_<FIELD>)` | Invalid boolean in a comments env var.                    | Use `true`, `false`, `1`, or `0` (case-insensitive).                             |
| `config: dotenv <path>:<line>: missing '=' in line`                                                            | `.env` file line has no `=` separator.                    | Ensure each non-comment line in the `.env` file is `KEY=VALUE`.                  |
| `config: dotenv <path>:<line>: invalid key "<key>"`                                                            | `.env` file key contains invalid characters.              | Keys MUST match `[A-Za-z_][A-Za-z0-9_]*`.                                       |

### 9.4 Template Errors

| Error                       | Phase                 | Impact                                   | Cause                                                                                                        | Fix                                                                                                                                                                                                                   |
| --------------------------- | --------------------- | ---------------------------------------- | ------------------------------------------------------------------------------------------------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **`template_parse_error`**  | Parse (workflow load) | **Blocks all dispatch** until corrected. | Syntax error in the prompt template: unclosed action, mismatched delimiters, undefined function in pipeline. | Check `{{ }}` balance. Verify function names match the FuncMap (`toJSON`, `join`, `lower`). Look for unclosed `{{ if }}`, `{{ range }}`, or `{{ with }}` blocks.                                                      |
| **`template_render_error`** | Render (per issue)    | **Fails the current run attempt** only.  | Runtime error: missing variable (`missingkey=error`), type mismatch in pipeline, FuncMap function error.     | Check variable names against the data contract (`.issue.*`, `.attempt`, `.run.*`). Verify that fields accessed inside `{{ range }}` use `$` prefix for top-level access. Ensure `join` receives a list, not a scalar. |

**Line number adjustment:** Template error messages include line numbers adjusted to
`WORKFLOW.md`-relative positions (front matter line count is added to the
template-relative line number). The error message format:

```
template parse error in WORKFLOW.md (line 47): template: prompt:4:15: ...
template render error in WORKFLOW.md (line 52): template: prompt:9: ...
```

---

## 10. Config Fields Summary (Cheat Sheet)

A flat reference of every configuration field, for quick lookup. The "Env Override" column
lists the `SORTIE_*` variable that overrides the field, or "—" if not overridable (see
[Section 3](#3-environment-variable-overrides)).

| Field                                   | Type             | Default                      | Env Override                             | Notes                                                                                  |
| --------------------------------------- | ---------------- | ---------------------------- | ---------------------------------------- | -------------------------------------------------------------------------------------- |
| `tracker.kind`                          | string           | _(required)_                 | `SORTIE_TRACKER_KIND`                    | e.g., `jira`                                                                           |
| `tracker.endpoint`                      | string           | adapter-defined              | `SORTIE_TRACKER_ENDPOINT`                | `$VAR` supported                                                                       |
| `tracker.api_key`                       | string or `$VAR` | _(required for Jira)_        | `SORTIE_TRACKER_API_KEY`                 | Full env expansion                                                                     |
| `tracker.project`                       | string           | _(required for Jira)_        | `SORTIE_TRACKER_PROJECT`                 | `$VAR` supported                                                                       |
| `tracker.active_states`                 | `[string]`       | `[]` (empty)                 | `SORTIE_TRACKER_ACTIVE_STATES`           | CSV; at least one of active/terminal required                                          |
| `tracker.terminal_states`               | `[string]`       | `[]` (empty)                 | `SORTIE_TRACKER_TERMINAL_STATES`         | CSV; at least one of active/terminal required                                          |
| `tracker.query_filter`                  | string           | `""`                         | `SORTIE_TRACKER_QUERY_FILTER`            | Adapter-interpreted                                                                    |
| `tracker.handoff_state`                 | string           | _(absent)_                   | `SORTIE_TRACKER_HANDOFF_STATE`           | Must not collide with active/terminal                                                  |
| `tracker.in_progress_state`             | string           | _(absent)_                   | `SORTIE_TRACKER_IN_PROGRESS_STATE`       | Must be in active; must not collide with terminal/handoff                              |
| `tracker.comments.on_dispatch`          | bool             | `false`                      | `SORTIE_TRACKER_COMMENTS_ON_DISPATCH`    |                                                                                        |
| `tracker.comments.on_completion`        | bool             | `false`                      | `SORTIE_TRACKER_COMMENTS_ON_COMPLETION`  |                                                                                        |
| `tracker.comments.on_failure`           | bool             | `false`                      | `SORTIE_TRACKER_COMMENTS_ON_FAILURE`     |                                                                                        |
| `polling.interval_ms`                   | integer          | `30000`                      | `SORTIE_POLLING_INTERVAL_MS`             | Dynamic reload                                                                         |
| `workspace.root`                        | path             | `<tmpdir>/sortie_workspaces` | `SORTIE_WORKSPACE_ROOT`                  | `~` expanded; `$VAR` skipped for env-sourced values                                    |
| `hooks.after_create`                    | shell script     | _(null)_                     | —                                        | Fatal on failure                                                                       |
| `hooks.before_run`                      | shell script     | _(null)_                     | —                                        | Fatal on failure                                                                       |
| `hooks.after_run`                       | shell script     | _(null)_                     | —                                        | Failure ignored                                                                        |
| `hooks.before_remove`                   | shell script     | _(null)_                     | —                                        | Failure ignored                                                                        |
| `hooks.timeout_ms`                      | integer          | `60000`                      | —                                        | All hooks                                                                              |
| `agent.kind`                            | string           | `claude-code`                | `SORTIE_AGENT_KIND`                      |                                                                                        |
| `agent.command`                         | shell command    | adapter-defined              | `SORTIE_AGENT_COMMAND`                   | Required for local adapters                                                            |
| `agent.turn_timeout_ms`                 | integer          | `3600000`                    | `SORTIE_AGENT_TURN_TIMEOUT_MS`           | 1 hour                                                                                 |
| `agent.read_timeout_ms`                 | integer          | `5000`                       | `SORTIE_AGENT_READ_TIMEOUT_MS`           | 5 seconds                                                                              |
| `agent.stall_timeout_ms`               | integer          | `300000`                     | `SORTIE_AGENT_STALL_TIMEOUT_MS`          | 5 min; `≤ 0` disables                                                                  |
| `agent.max_concurrent_agents`           | integer          | `10`                         | `SORTIE_AGENT_MAX_CONCURRENT_AGENTS`     | Dynamic reload                                                                         |
| `agent.max_turns`                       | integer          | `20`                         | `SORTIE_AGENT_MAX_TURNS`                 |                                                                                        |
| `agent.max_retry_backoff_ms`            | integer          | `300000`                     | `SORTIE_AGENT_MAX_RETRY_BACKOFF_MS`      | 5 min; dynamic reload                                                                  |
| `agent.max_concurrent_agents_by_state`  | `map[string]int` | `{}`                         | —                                        | Keys lowercased; dynamic reload                                                        |
| `agent.max_sessions`                    | integer          | `0`                          | `SORTIE_AGENT_MAX_SESSIONS`              | Unlimited; dynamic reload                                                              |
| `db_path`                               | path             | `.sortie.db`                 | `SORTIE_DB_PATH`                         | Restart required; `$VAR` skipped for env-sourced values                                |
| **Extensions**                          |                  |                              |                                          |                                                                                        |
| `server.port`                           | integer          | _(absent)_                   | —                                        | CLI `--port` overrides                                                                 |
| `logging.level`                         | string           | `info`                       | —                                        | CLI `--log-level` overrides                                                            |
| `worker.ssh_hosts`                      | `[string]`       | _(absent)_                   | —                                        | SSH host targets; dynamic reload                                                       |
| `worker.max_concurrent_agents_per_host` | integer          | _(absent)_                   | —                                        | Per-host cap; dynamic reload                                                           |

---

## 11. Complete Annotated Examples

### 11.1 Minimal Workflow

The simplest valid workflow — a prompt-only file with no front matter:

```markdown
You are a software engineer. Fix the following issue:

**{{ .issue.identifier }}**: {{ .issue.title }}

{{ if .issue.description }}
{{ .issue.description }}
{{ end }}
```

This uses all defaults: `claude-code` agent, 30-second polling, system-temp workspace root,
no tracker (dispatch validation will fail — `tracker.kind` is required for actual dispatch).

A minimal workflow with tracker configuration:

```markdown
---
tracker:
  kind: jira
  endpoint: $JIRA_URL
  api_key: $JIRA_TOKEN
  project: PROJ
---

Fix {{ .issue.identifier }}: {{ .issue.title }}
```

### 11.2 Production Jira + Claude Code

A complete, production-ready workflow demonstrating all major features:

```markdown
---
# ─── Tracker ───────────────────────────────────────────────────
tracker:
  kind: jira
  endpoint: $SORTIE_JIRA_ENDPOINT # https://mycompany.atlassian.net
  api_key: $SORTIE_JIRA_API_KEY # Jira API token (needs read + write scopes)
  project: PROJ # Jira project key
  query_filter: "labels = 'agent-ready'" # Only pick up labeled issues
  active_states:
    - To Do
    - In Progress
  terminal_states:
    - Done
    - Won't Do
  handoff_state: Human Review # Move here after successful agent run
  in_progress_state: In Progress # Move here when agent picks up the issue

# ─── Polling ───────────────────────────────────────────────────
polling:
  interval_ms: 60000 # 1-minute poll cycle

# ─── Workspace ─────────────────────────────────────────────────
workspace:
  root: ~/workspace/sortie # Per-issue dirs created under here

# ─── Hooks ─────────────────────────────────────────────────────
hooks:
  after_create: |
    # Clone the repo into the fresh workspace
    git clone --depth 1 git@github.com:myorg/myrepo.git .
    go mod download
  before_run: |
    # Create a fresh branch from main for each attempt
    git fetch origin main
    git checkout -B "sortie/${SORTIE_ISSUE_IDENTIFIER}" origin/main
  after_run: |
    # Auto-commit any changes (best-effort)
    make fmt 2>/dev/null || true
    git add -A
    git diff --cached --quiet || \
      git commit -m "sortie(${SORTIE_ISSUE_IDENTIFIER}): automated changes"
  before_remove: |
    # Clean up remote branch
    git push origin --delete "sortie/${SORTIE_ISSUE_IDENTIFIER}" 2>/dev/null || true
  timeout_ms: 120000 # 2 minutes for hook execution

# ─── Agent ─────────────────────────────────────────────────────
agent:
  kind: claude-code
  command: claude # CLI binary name
  max_turns: 5 # Orchestrator turn-loop limit
  max_sessions: 3 # Give up after 3 complete sessions
  max_concurrent_agents: 4 # Run up to 4 agents in parallel
  turn_timeout_ms: 1800000 # 30-minute turn timeout
  read_timeout_ms: 10000 # 10-second startup timeout
  stall_timeout_ms: 300000 # 5-minute stall detection
  max_retry_backoff_ms: 120000 # 2-minute max retry delay
  max_concurrent_agents_by_state:
    in progress: 3 # Reserve 1 slot for new issues
    to do: 1 # Limit new issue pickup

# ─── Claude Code Adapter ──────────────────────────────────────
claude-code:
  permission_mode: bypassPermissions # Auto-approve all tool calls
  model: claude-sonnet-4-20250514 # Model for agent sessions
  max_turns: 50 # CLI --max-turns (distinct from agent.max_turns)
  max_budget_usd: 5 # Per-session cost cap

# ─── Server ────────────────────────────────────────────────────
server:
  port: 8642 # Enable HTTP observability server

# ─── Database ──────────────────────────────────────────────────
db_path: .sortie.db # SQLite file next to WORKFLOW.md
---

{{/* ─── Prompt Template ─────────────────────────────────────── */}}

You are a senior Go systems engineer working on the **{{ .issue.identifier }}** codebase.
Your work is managed by Sortie, an automated orchestrator that dispatches issues, retries
failures, and monitors your progress.

## Task

**{{ .issue.identifier }}**: {{ .issue.title }}
{{ if .issue.description }}

### Description

{{ .issue.description }}
{{ end }}
{{ if .issue.labels }}
**Labels:** {{ .issue.labels | join ", " }}
{{ end }}
{{ if .issue.url }}
**Ticket:** {{ .issue.url }}
{{ end }}

## Guidelines

1. Read the relevant documentation before writing code.
2. Implement the minimal change that satisfies the task.
3. Write table-driven tests covering edge cases.
4. Run `make lint && make test && make build` — all must pass.
5. If blocked, write `blocked` to `.sortie/status` and stop.
   {{ if not .run.is_continuation }}

## First Run

This is a fresh attempt. Start by reading the specification and existing code.
Understand the problem before writing any solution.
{{ end }}
{{ if .run.is_continuation }}

## Continuation (Turn {{ .run.turn_number }}/{{ .run.max_turns }})

You are resuming a multi-turn session. Do not restart from scratch.
Review the workspace state (`git diff`, `git status`, test output) and
continue from where the previous turn ended.
{{ end }}
{{ if and .attempt (not .run.is_continuation) }}

## Retry — Attempt {{ .attempt }}

A previous attempt failed. Do not repeat the same approach.
Check `.sortie/status` for notes. Run `make test` to identify the failure.
Diagnose the root cause before making changes.
{{ end }}
{{ if .issue.blocked_by }}

## Blockers

{{ range .issue.blocked_by }}- **{{ .identifier }}**{{ if .state }} ({{ .state }}){{ end }}
{{ end }}
{{ end }}
{{ if .issue.parent }}

## Parent Issue

{{ .issue.parent.identifier }}
{{ end }}
```

---

_This document is derived strictly from the Sortie Architecture Specification
(Sections 5, 6, 9.4, and 10) and informed by end-to-end testing experience (tasks
7.11–7.13). It is the authoritative user-facing reference for workflow authors._
