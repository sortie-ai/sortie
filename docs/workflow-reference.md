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
- [3. Extensions](#3-extensions)
  - [3.1 `server.port` — HTTP Server](#31-serverport--http-server)
  - [3.2 `worker` — SSH Worker Extension](#32-worker--ssh-worker-extension)
  - [3.3 Adapter-Specific Pass-Through Config](#33-adapter-specific-pass-through-config)
- [4. Prompt Template Reference](#4-prompt-template-reference)
  - [4.1 Template Engine](#41-template-engine)
  - [4.2 Template Input Variables](#42-template-input-variables)
  - [4.3 Built-in Functions (FuncMap)](#43-built-in-functions-funcmap)
  - [4.4 Built-in Actions](#44-built-in-actions)
  - [4.5 First-Turn vs Continuation Semantics](#45-first-turn-vs-continuation-semantics)
  - [4.6 Fallback Prompt Behavior](#46-fallback-prompt-behavior)
  - [4.7 Common Patterns and Pitfalls](#47-common-patterns-and-pitfalls)
- [5. Hook Lifecycle Reference](#5-hook-lifecycle-reference)
  - [5.1 Execution Contract](#51-execution-contract)
  - [5.2 Hook Environment Variables](#52-hook-environment-variables)
  - [5.3 Failure Semantics](#53-failure-semantics)
  - [5.4 Inline Scripts vs File Paths](#54-inline-scripts-vs-file-paths)
- [6. Dynamic Reload Behavior](#6-dynamic-reload-behavior)
  - [6.1 General Reload Semantics](#61-general-reload-semantics)
  - [6.2 Per-Field Reload Behavior](#62-per-field-reload-behavior)
- [7. Dispatch Preflight Validation](#7-dispatch-preflight-validation)
- [8. Error Reference](#8-error-reference)
  - [8.1 Workflow File Errors](#81-workflow-file-errors)
  - [8.2 Configuration Errors](#82-configuration-errors)
  - [8.3 Template Errors](#83-template-errors)
- [9. Config Fields Summary (Cheat Sheet)](#9-config-fields-summary-cheat-sheet)
- [10. Complete Annotated Examples](#10-complete-annotated-examples)
  - [10.1 Minimal Workflow](#101-minimal-workflow)
  - [10.2 Production Jira + Claude Code](#102-production-jira--claude-code)

---

## 1. File Format

### 1.1 Overview

`WORKFLOW.md` is a Markdown file with optional YAML front matter. It encodes two payloads in
a single document:

| Payload            | Location                           | Purpose                                      |
| ------------------ | ---------------------------------- | -------------------------------------------- |
| **Configuration**  | YAML front matter (between `---`)  | Tracker, polling, workspace, hooks, agent     |
| **Prompt template**| Markdown body (after closing `---`)| Per-issue prompt rendered with Go `text/template` |

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
3. **Opening delimiter detection.** If the first line (after optional trailing whitespace)
   is exactly `---`, enter front matter mode.
4. **Front matter extraction.** Scan lines until a line that is exactly `---` (with
   optional trailing whitespace). Bytes between the delimiters are the YAML front matter.
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

After parsing, the loader produces:

| Field              | Type              | Description                                          |
| ------------------ | ----------------- | ---------------------------------------------------- |
| `config`           | `map[string]any`  | Front matter root object (not nested under a `config` key) |
| `prompt_template`  | `string`          | Trimmed Markdown body                                |
| `front_matter_lines` | `int`           | Line count through closing `---`; `0` when absent. Used for error line number adjustment. |

---

## 2. Front Matter Schema

### 2.1 Top-Level Keys

The core schema recognizes six top-level keys:

```yaml
tracker:     # Issue tracker connection and query settings
polling:     # Poll loop timing
workspace:   # Workspace root path
hooks:       # Workspace lifecycle hook scripts
agent:       # Coding agent adapter, timeouts, and limits
db_path:     # SQLite database file path
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
```

| Field             | Type             | Required | Default           | Dynamic Reload | Description |
| ----------------- | ---------------- | -------- | ----------------- | -------------- | ----------- |
| `kind`            | string           | **Yes** (for dispatch) | *(none)* | Future dispatches | Adapter identifier. Supported: `jira`. Additional adapters registered separately. |
| `endpoint`        | string           | Adapter-defined | Adapter-defined | Future dispatches | Tracker API endpoint URL. Supports `$VAR` indirection when the entire value is an env ref. |
| `api_key`         | string           | When adapter requires it | *(none)* | Future dispatches | API authentication token. May be a literal or `$VAR_NAME`. If `$VAR_NAME` resolves to empty, treated as missing. Jira requires this field. |
| `project`         | string           | When adapter requires it | *(none)* | Future dispatches | Project identifier (e.g., Jira project key). Supports `$VAR` indirection. |
| `active_states`   | list of strings  | No | Adapter-defined | Future dispatch and reconciliation | Issue states eligible for agent dispatch. |
| `terminal_states` | list of strings  | No | Adapter-defined | Future dispatch and reconciliation | Issue states that release claims and trigger cleanup. |
| `query_filter`    | string           | No | `""` (empty) | Future dispatches | Adapter-defined query fragment appended to candidate and terminal-state queries. Passed to the adapter without interpretation. For Jira: JQL fragment (e.g., `"labels = 'agent-ready'"`). |
| `handoff_state`   | string           | No | *(absent)* | Future worker exits | Target tracker state for orchestrator-initiated handoff after successful worker run. When absent, no handoff transition is performed. |

**`handoff_state` validation rules:**

- Supports `$VAR` environment indirection.
- When set, must be a non-empty string after `$VAR` resolution. Empty resolution is a
  configuration error.
- Must **not** appear in `active_states` (would cause immediate re-dispatch after handoff).
- Must **not** appear in `terminal_states` (handoff is not terminal — the issue may return
  to active for further work).
- Requires **write permissions** on the tracker API token. For Jira: `write:jira-work`
  (classic) or `write:issue:jira` (granular).

**`api_key` environment resolution:**

The `api_key` field uses full environment expansion (`$VAR`, `${VAR}`, and mixed content).
All other `$VAR`-supporting fields in `tracker` use targeted resolution: expansion only
when the entire value is an environment reference.

---

### 2.3 `polling` — Poll Loop Timing

```yaml
polling:
  interval_ms: 30000
```

| Field          | Type                    | Required | Default  | Dynamic Reload | Description |
| -------------- | ----------------------- | -------- | -------- | -------------- | ----------- |
| `interval_ms`  | integer or string integer | No     | `30000`  | **Yes** — affects future tick scheduling | Milliseconds between poll cycles. |

---

### 2.4 `workspace` — Workspace Root

```yaml
workspace:
  root: ~/workspace/sortie
```

| Field  | Type                   | Required | Default                           | Dynamic Reload | Description |
| ------ | ---------------------- | -------- | --------------------------------- | -------------- | ----------- |
| `root` | path string or `$VAR`  | No       | `<system-temp>/sortie_workspaces` | Future workspace operations | Base directory for per-issue workspaces. |

**Path resolution:**

- `~` is expanded to the user's home directory.
- `$VAR` is expanded when the entire value is an environment reference.
- Strings containing path separators are treated as paths and expanded.
- Bare strings without path separators are preserved as-is (relative roots are allowed but
  discouraged).

**Per-issue workspace path:** `<workspace.root>/<sanitized_issue_identifier>`

**Workspace key sanitization:** Only `[A-Za-z0-9._-]` are allowed. All other characters in
the issue identifier are replaced with `_`.

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

| Field           | Type                       | Required | Default  | Dynamic Reload | Description |
| --------------- | -------------------------- | -------- | -------- | -------------- | ----------- |
| `after_create`  | multiline shell script or null | No   | *(none)* | Future hook executions | Runs only when a workspace directory is **newly created**. |
| `before_run`    | multiline shell script or null | No   | *(none)* | Future hook executions | Runs before each agent attempt, after workspace preparation. |
| `after_run`     | multiline shell script or null | No   | *(none)* | Future hook executions | Runs after each agent attempt (success, failure, timeout, or cancellation). |
| `before_remove` | multiline shell script or null | No   | *(none)* | Future hook executions | Runs before workspace deletion, if the directory exists. |
| `timeout_ms`    | integer                    | No       | `60000`  | Future hook executions | Timeout in milliseconds for all hooks. Non-positive values fall back to the default. |

See [Section 5: Hook Lifecycle Reference](#5-hook-lifecycle-reference) for execution
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

| Field | Type | Required | Default | Dynamic Reload | Description |
| ----- | ---- | -------- | ------- | -------------- | ----------- |
| `kind` | string | No | `claude-code` | Future dispatches | Agent adapter identifier. Supported: `claude-code`, `codex`, `http`, and any additionally registered adapter. |
| `command` | string (shell command) | When adapter requires local process | Adapter-defined | Future dispatches | Shell command to launch the agent. Required for local subprocess adapters (`claude-code`, `codex`). Not required for HTTP-based adapters. |
| `turn_timeout_ms` | integer | No | `3600000` (1h) | Future turns | Total timeout for a single agent turn. |
| `read_timeout_ms` | integer | No | `5000` (5s) | Future turns | Request/response timeout during startup and synchronous operations. |
| `stall_timeout_ms` | integer | No | `300000` (5m) | Future turns | Inactivity timeout based on event stream gaps. Set to `0` or negative to **disable** stall detection. |
| `max_concurrent_agents` | integer or string integer | No | `10` | **Yes** — affects subsequent dispatch | Global concurrency limit across all issues. |
| `max_turns` | integer | No | `20` | Future dispatches | Maximum coding-agent turns per worker session. The worker re-checks tracker state after each turn and starts another turn if the issue is still active, up to this limit. |
| `max_retry_backoff_ms` | integer or string integer | No | `300000` (5m) | **Yes** — affects future retry scheduling | Maximum delay cap for exponential backoff on retries. |
| `max_concurrent_agents_by_state` | map of `state → positive integer` | No | `{}` (empty) | **Yes** — affects subsequent dispatch | Per-state concurrency limits. State keys are normalized to lowercase for lookup. Non-positive or non-numeric entries are silently ignored. |
| `max_sessions` | integer | No | `0` (unlimited) | **Yes** — affects future retry evaluations | Maximum completed worker sessions per issue before the orchestrator stops re-dispatching. Counted from run history. `0` disables the budget (unlimited). Must be non-negative. |

**Orchestrator vs adapter fields:** The fields above are consumed by the orchestrator
for scheduling, concurrency, and retry decisions. They are **not** passed through to the
agent adapter. Adapter-specific configuration uses separate pass-through blocks — see
[Section 3.3](#33-adapter-specific-pass-through-config).

---

### 2.7 `db_path` — SQLite Database Path

```yaml
db_path: /var/lib/sortie/state.db
```

| Field     | Type   | Required | Default                               | Dynamic Reload | Description |
| --------- | ------ | -------- | ------------------------------------- | -------------- | ----------- |
| `db_path` | string | No       | `.sortie.db` next to `WORKFLOW.md`    | **No** — requires restart | Path for the SQLite database file. |

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

## 3. Extensions

The front matter is extensible. Unknown top-level keys are collected into an `Extensions`
map and are not validated by the core schema. Extensions should document their own field
schemas, defaults, and reload behavior.

### 3.1 `server.port` — HTTP Server

```yaml
server:
  port: 8642
```

| Field          | Type    | Required | Default    | Dynamic Reload | Description |
| -------------- | ------- | -------- | ---------- | -------------- | ----------- |
| `server.port`  | integer | No       | *(absent — server disabled)* | **No** — requires restart | Enables the embedded HTTP server. Positive values bind that port. `0` requests an ephemeral port. |

**Precedence:** CLI `--port` argument overrides `server.port` when both are present.

**Bind address:** Loopback (`127.0.0.1`) by default.

### 3.2 `worker` — SSH Worker Extension

```yaml
worker:
  ssh_hosts:
    - build01.internal
    - build02.internal
  max_concurrent_agents_per_host: 2
```

| Field | Type | Required | Default | Description |
| ----- | ---- | -------- | ------- | ----------- |
| `worker.ssh_hosts` | list of strings | No | *(absent — work runs locally)* | SSH host targets for remote agent execution. |
| `worker.max_concurrent_agents_per_host` | positive integer | No | *(absent)* | Per-host concurrency cap shared across configured SSH hosts. Hosts at capacity are skipped. |

### 3.3 Adapter-Specific Pass-Through Config

Each agent adapter may define configuration in a top-level object named after its `kind`
value. These values are passed through to the adapter without validation by the
orchestrator core.

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

**Codex adapter (example):**

```yaml
codex:
  approval_policy: auto-edit
  thread_sandbox: true
```

The orchestrator forwards the entire sub-object to the matching adapter without
interpretation.

---

## 4. Prompt Template Reference

### 4.1 Template Engine

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

### 4.2 Template Input Variables

The data map passed to `Execute` contains exactly **three top-level keys**:

#### `issue` — Normalized Issue Object

All fields from the tracker, normalized into a stable structure regardless of the
underlying tracker system.

| Field          | Type                    | Description |
| -------------- | ----------------------- | ----------- |
| `.issue.id`          | string            | Stable tracker-internal ID. |
| `.issue.identifier`  | string            | Human-readable ticket key (e.g., `PROJ-123`). |
| `.issue.title`       | string            | Issue summary/title. |
| `.issue.description` | string or nil     | Full issue description body. |
| `.issue.priority`    | integer or nil    | Numeric priority (lower = higher priority). |
| `.issue.state`       | string            | Current tracker state name. |
| `.issue.branch_name` | string or nil     | Tracker-provided branch metadata. |
| `.issue.url`         | string or nil     | Web URL to the issue in the tracker. |
| `.issue.labels`      | list of strings   | Labels, normalized to lowercase. |
| `.issue.assignee`    | string or nil     | Assignee identity from the tracker. |
| `.issue.issue_type`  | string or nil     | Tracker-defined type (Bug, Story, Task, Epic). |
| `.issue.parent`      | object or nil     | Parent issue reference (sub-task relationship). |
| `.issue.comments`    | list or nil       | Comment records (feedback, review notes, workpad entries). |
| `.issue.blocked_by`  | list of objects   | Blocker references, each with `.id`, `.identifier`, `.state`. |
| `.issue.created_at`  | timestamp or nil  | Issue creation time. |
| `.issue.updated_at`  | timestamp or nil  | Last update time. |

#### `attempt` — Retry Counter

| Value           | Meaning |
| --------------- | ------- |
| `nil`           | First attempt — no prior failures. |
| Integer `>= 1` | Retry or continuation attempt number. |

**Template usage:** Use `{{ if .attempt }}` to test presence. On first run, `attempt` is
`nil` (present in the data map with a nil value), so `{{ if .attempt }}` evaluates to
`false`. On retries, it evaluates to `true`.

#### `run` — Per-Turn Metadata

| Field                    | Type    | Description |
| ------------------------ | ------- | ----------- |
| `.run.turn_number`       | integer | Current turn number within the session. |
| `.run.max_turns`         | integer | Configured maximum turns per session. |
| `.run.is_continuation`   | boolean | `true` when this is a continuation turn within a multi-turn session (not the first turn, not a retry after error). |

### 4.3 Built-in Functions (FuncMap)

In addition to Go `text/template` built-in actions, Sortie ships a minimal set of
prompt-essential functions. Each is permanent API surface.

| Function | Signature                    | Description | Example |
| -------- | ---------------------------- | ----------- | ------- |
| `toJSON` | `toJSON value → string`      | Serialize any value to compact JSON. Agents parse structured data more reliably from JSON than from Go's default `fmt` representation. | `{{ .issue.labels \| toJSON }}` → `["bug","urgent"]` |
| `join`   | `join separator list → string` | Join a list of strings with a separator. | `{{ .issue.labels \| join ", " }}` → `bug, urgent` |
| `lower`  | `lower string → string`      | Lowercase a string. | `{{ .issue.state \| lower }}` → `in progress` |

> **Note:** `join` uses pipe syntax with reversed arguments: `{{ .issue.labels | join ", " }}`.
> The separator comes first in the function signature because Go template pipelines pass
> the piped value as the last argument.

### 4.4 Built-in Actions

Go `text/template` provides these built-in actions, all available in workflow templates:

| Action | Purpose |
| ------ | ------- |
| `{{ if COND }}...{{ else }}...{{ end }}` | Conditional branching. |
| `{{ range LIST }}...{{ end }}` | Iterate over a list or map. |
| `{{ with VALUE }}...{{ end }}` | Set dot to value if non-empty. |
| `{{ and A B }}` | Logical AND. |
| `{{ or A B }}` | Logical OR. |
| `{{ not A }}` | Logical NOT. |
| `{{ eq A B }}`, `{{ ne A B }}` | Equality / inequality. |
| `{{ lt A B }}`, `{{ le A B }}`, `{{ gt A B }}`, `{{ ge A B }}` | Comparison. |
| `{{ len LIST }}` | Length of list, map, or string. |
| `{{ index MAP KEY }}` | Index into a map or slice. |
| `{{ print A }}`, `{{ printf FMT A }}`, `{{ println A }}` | Formatted output. |
| `{{ call FUNC ARGS }}` | Call a function value. |

### 4.5 First-Turn vs Continuation Semantics

The prompt template supports three distinct modes within a single file. Workflow authors
use `attempt` and `run.is_continuation` to branch:

| Scenario | `attempt` | `run.is_continuation` | Typical template action |
| -------- | --------- | --------------------- | ----------------------- |
| **First run** | `nil` | `false` | Full task instructions, context gathering steps. |
| **Continuation turn** | integer | `true` | Resume guidance — review state, pick up where left off. |
| **Retry after error** | integer `>= 1` | `false` | Diagnostic steps — check prior failure, approach differently. |

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

- The first turn uses the full rendered prompt.
- Continuation turns send only continuation guidance to the existing agent thread —
  the original task prompt is already present in the thread history and is not resent.
- After each turn, the worker re-checks the tracker issue state. If the issue is
  still active, another turn begins (up to `agent.max_turns`).

### 4.6 Fallback Prompt Behavior

- If the prompt body is empty, the runtime may use a minimal default prompt.
- Workflow file read/parse failures are validation errors and do **not** silently fall
  back to a default prompt.

### 4.7 Common Patterns and Pitfalls

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

Fields that may be nil (`description`, `url`, `assignee`, etc.) should be guarded:

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

## 5. Hook Lifecycle Reference

### 5.1 Execution Contract

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

### 5.2 Hook Environment Variables

All hooks receive the following environment variables:

| Variable                    | Description |
| --------------------------- | ----------- |
| `SORTIE_ISSUE_ID`           | Stable tracker-internal issue ID. |
| `SORTIE_ISSUE_IDENTIFIER`   | Human-readable ticket key (e.g., `PROJ-123`). |
| `SORTIE_WORKSPACE`          | Absolute path to the per-issue workspace directory. |
| `SORTIE_ATTEMPT`            | Current attempt number (integer). |

These allow hooks to make decisions without parsing orchestrator internals.

### 5.3 Failure Semantics

| Hook             | When it runs                        | Failure behavior |
| ---------------- | ----------------------------------- | ---------------- |
| `after_create`   | Workspace directory newly created   | **Fatal** — aborts workspace creation. The partially-prepared directory may be removed. |
| `before_run`     | Before each agent attempt           | **Fatal** — aborts the current run attempt. The orchestrator treats this as a worker failure and may retry. |
| `after_run`      | After each agent attempt            | **Logged and ignored** — the run result is already determined. |
| `before_remove`  | Before workspace deletion           | **Logged and ignored** — cleanup still proceeds. |

Timeouts are treated the same as failures for each hook's failure semantics.

### 5.4 Inline Scripts vs File Paths

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

## 6. Dynamic Reload Behavior

### 6.1 General Reload Semantics

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

### 6.2 Per-Field Reload Behavior

| Field | Reload behavior |
| ----- | --------------- |
| `tracker.kind` | Future dispatches. |
| `tracker.endpoint` | Future dispatches. |
| `tracker.api_key` | Future dispatches. |
| `tracker.project` | Future dispatches. |
| `tracker.active_states` | Future dispatch and reconciliation. |
| `tracker.terminal_states` | Future dispatch and reconciliation. |
| `tracker.query_filter` | Future dispatches. |
| `tracker.handoff_state` | Future worker exits, not in-flight sessions. |
| `polling.interval_ms` | **Immediate** — affects future tick scheduling. |
| `workspace.root` | Future workspace operations. |
| `hooks.*` | Future hook executions. |
| `hooks.timeout_ms` | Future hook executions. |
| `agent.kind` | Future dispatches. |
| `agent.command` | Future dispatches. |
| `agent.turn_timeout_ms` | Future turns. |
| `agent.read_timeout_ms` | Future turns. |
| `agent.stall_timeout_ms` | Future turns. |
| `agent.max_concurrent_agents` | **Immediate** — affects subsequent dispatch decisions. |
| `agent.max_turns` | Future dispatches. |
| `agent.max_retry_backoff_ms` | **Immediate** — affects future retry scheduling. |
| `agent.max_concurrent_agents_by_state` | **Immediate** — affects subsequent dispatch decisions. |
| `agent.max_sessions` | **Immediate** — affects future retry timer evaluations. |
| `db_path` | **No effect** — requires restart. In-memory config updated, but database connection unchanged. |
| `server.port` | **No effect** — requires restart. |
| Prompt template | Future agent launches and continuation turns. |

---

## 7. Dispatch Preflight Validation

Before dispatching work, the orchestrator validates the workflow configuration. This runs
at two points:

**Startup validation:** Before starting the scheduling loop. If validation fails, startup
is aborted with an operator-visible error.

**Per-tick validation:** Before each dispatch cycle. If validation fails, dispatch is
skipped for that tick, reconciliation remains active, and an error is emitted.

**Validation checks:**

| Check | Error condition |
| ----- | --------------- |
| Workflow file loadable and parseable | File missing, YAML syntax error, or non-map front matter. |
| `tracker.kind` present and supported | Missing, empty, or unregistered adapter. |
| `tracker.api_key` present after `$` resolution | Missing or empty when the adapter requires it (e.g., Jira). |
| `tracker.project` present | Missing when the adapter requires project scoping. |
| `agent.command` present and non-empty | Missing when `agent.kind` requires a local command. |
| Tracker adapter registered and available | No adapter registered for the configured `tracker.kind`. |
| Agent adapter registered and available | No adapter registered for the configured `agent.kind`. |

---

## 8. Error Reference

### 8.1 Workflow File Errors

These errors are raised during workflow file loading and prevent dispatch until fixed.

| Error | Cause | Fix |
| ----- | ----- | --- |
| **`missing_workflow_file`** | The workflow file cannot be read at the configured or default path. | Verify the file exists. Check path spelling. Ensure read permissions. If using a custom path, confirm the CLI argument. |
| **`workflow_parse_error`** | YAML front matter contains syntax errors. Common cause: missing closing `---` delimiter, or invalid YAML between delimiters. | Check for balanced `---` delimiters. Validate YAML syntax (indentation, colons, quoting). Look for tabs where spaces are expected. |
| **`workflow_front_matter_not_a_map`** | YAML front matter decoded to a scalar or list instead of a map/object. | Ensure front matter contains key-value pairs, not a bare value or list. The top level must be a YAML mapping. |

### 8.2 Configuration Errors

These errors are raised during typed config construction from the parsed front matter.
Each error identifies the offending field path.

| Error pattern | Cause | Fix |
| ------------- | ----- | --- |
| `config: polling.interval_ms: invalid integer value: <val>` | Non-integer value for a field expecting an integer. | Use a plain integer (e.g., `30000`) or a quoted string integer (e.g., `"30000"`). Remove units, decimals, or non-numeric characters. |
| `config: agent.max_concurrent_agents: invalid integer value: <val>` | Same as above, for any integer field. | Same fix as above. |
| `config: agent.stall_timeout_ms: invalid integer value: <val>` | Same as above. | Same fix. |
| `config: agent.max_sessions: must be non-negative` | Negative value for `max_sessions`. | Use `0` (unlimited) or a positive integer. |
| `config: tracker.handoff_state: expected string, got <type>` | `handoff_state` is not a string (e.g., integer, boolean, list). | Ensure the value is a string, quoted if necessary. |
| `config: tracker.handoff_state: must not be empty` | `handoff_state` is set to an explicit empty string. | Provide a valid state name, or omit the field entirely to disable handoff. |
| `config: tracker.handoff_state: resolved to empty (check environment variable)` | `$VAR` reference resolved to an empty string (variable unset or empty). | Set the referenced environment variable to a valid state name. |
| `config: tracker.handoff_state: "<val>" collides with active state "<state>"` | `handoff_state` matches one of the `active_states` (case-insensitive). | Use a state that is not in `active_states`. The handoff state must be distinct from active and terminal states. |
| `config: tracker.handoff_state: "<val>" collides with terminal state "<state>"` | `handoff_state` matches one of the `terminal_states` (case-insensitive). | Use a state that is not in `terminal_states`. |
| `config: workspace.root: cannot expand ~: <err>` | Home directory expansion failed. | Check that the `HOME` environment variable is set. |
| `config: db_path: expected string, got <type>` | `db_path` is not a string value. | Use a string path value, quoted if necessary. |
| `config: db_path: resolved to empty (check environment variable)` | `$VAR` reference resolved to empty. | Set the environment variable or use a literal path. |

### 8.3 Template Errors

| Error | Phase | Impact | Cause | Fix |
| ----- | ----- | ------ | ----- | --- |
| **`template_parse_error`** | Parse (workflow load) | **Blocks all dispatch** until corrected. | Syntax error in the prompt template: unclosed action, mismatched delimiters, undefined function in pipeline. | Check `{{ }}` balance. Verify function names match the FuncMap (`toJSON`, `join`, `lower`). Look for unclosed `{{ if }}`, `{{ range }}`, or `{{ with }}` blocks. |
| **`template_render_error`** | Render (per issue) | **Fails the current run attempt** only. | Runtime error: missing variable (`missingkey=error`), type mismatch in pipeline, FuncMap function error. | Check variable names against the data contract (`.issue.*`, `.attempt`, `.run.*`). Verify that fields accessed inside `{{ range }}` use `$` prefix for top-level access. Ensure `join` receives a list, not a scalar. |

**Line number adjustment:** Template error messages include line numbers adjusted to
`WORKFLOW.md`-relative positions (front matter line count is added to the
template-relative line number). The error message format:

```
template parse error in WORKFLOW.md (line 47): template: prompt:4:15: ...
template render error in WORKFLOW.md (line 52): template: prompt:9: ...
```

---

## 9. Config Fields Summary (Cheat Sheet)

A flat reference of every configuration field, for quick lookup.

| Field | Type | Default | Notes |
| ----- | ---- | ------- | ----- |
| `tracker.kind` | string | *(required)* | e.g., `jira` |
| `tracker.endpoint` | string | adapter-defined | `$VAR` supported |
| `tracker.api_key` | string or `$VAR` | *(required for Jira)* | Full env expansion |
| `tracker.project` | string | *(required for Jira)* | `$VAR` supported |
| `tracker.active_states` | `[string]` | adapter-defined | |
| `tracker.terminal_states` | `[string]` | adapter-defined | |
| `tracker.query_filter` | string | `""` | Adapter-interpreted |
| `tracker.handoff_state` | string | *(absent)* | `$VAR` supported; must not collide with active/terminal |
| `polling.interval_ms` | integer | `30000` | Dynamic reload |
| `workspace.root` | path | `<tmpdir>/sortie_workspaces` | `~` and `$VAR` expanded |
| `hooks.after_create` | shell script | *(null)* | Fatal on failure |
| `hooks.before_run` | shell script | *(null)* | Fatal on failure |
| `hooks.after_run` | shell script | *(null)* | Failure ignored |
| `hooks.before_remove` | shell script | *(null)* | Failure ignored |
| `hooks.timeout_ms` | integer | `60000` | All hooks |
| `agent.kind` | string | `claude-code` | |
| `agent.command` | shell command | adapter-defined | Required for local adapters |
| `agent.turn_timeout_ms` | integer | `3600000` | 1 hour |
| `agent.read_timeout_ms` | integer | `5000` | 5 seconds |
| `agent.stall_timeout_ms` | integer | `300000` | 5 min; `≤ 0` disables |
| `agent.max_concurrent_agents` | integer | `10` | Dynamic reload |
| `agent.max_turns` | integer | `20` | |
| `agent.max_retry_backoff_ms` | integer | `300000` | 5 min; dynamic reload |
| `agent.max_concurrent_agents_by_state` | `map[string]int` | `{}` | Keys lowercased; dynamic reload |
| `agent.max_sessions` | integer | `0` | Unlimited; dynamic reload |
| `db_path` | path | `.sortie.db` | Restart required |
| **Extensions** | | | |
| `server.port` | integer | *(absent)* | Restart required; CLI overrides |
| `worker.ssh_hosts` | `[string]` | *(absent)* | Local execution when omitted |
| `worker.max_concurrent_agents_per_host` | integer | *(absent)* | Per-host cap |

---

## 10. Complete Annotated Examples

### 10.1 Minimal Workflow

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

### 10.2 Production Jira + Claude Code

A complete, production-ready workflow demonstrating all major features:

```markdown
---
# ─── Tracker ───────────────────────────────────────────────────
tracker:
  kind: jira
  endpoint: $SORTIE_JIRA_ENDPOINT       # https://mycompany.atlassian.net
  api_key: $SORTIE_JIRA_API_KEY         # Jira API token (needs read + write scopes)
  project: PROJ                          # Jira project key
  query_filter: "labels = 'agent-ready'" # Only pick up labeled issues
  active_states:
    - To Do
    - In Progress
  terminal_states:
    - Done
    - Won't Do
  handoff_state: Human Review            # Move here after successful agent run

# ─── Polling ───────────────────────────────────────────────────
polling:
  interval_ms: 60000                     # 1-minute poll cycle

# ─── Workspace ─────────────────────────────────────────────────
workspace:
  root: ~/workspace/sortie               # Per-issue dirs created under here

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
  timeout_ms: 120000                     # 2 minutes for hook execution

# ─── Agent ─────────────────────────────────────────────────────
agent:
  kind: claude-code
  command: claude                         # CLI binary name
  max_turns: 5                           # Orchestrator turn-loop limit
  max_sessions: 3                        # Give up after 3 complete sessions
  max_concurrent_agents: 4               # Run up to 4 agents in parallel
  turn_timeout_ms: 1800000               # 30-minute turn timeout
  read_timeout_ms: 10000                 # 10-second startup timeout
  stall_timeout_ms: 300000               # 5-minute stall detection
  max_retry_backoff_ms: 120000           # 2-minute max retry delay
  max_concurrent_agents_by_state:
    in progress: 3                       # Reserve 1 slot for new issues
    to do: 1                             # Limit new issue pickup

# ─── Claude Code Adapter ──────────────────────────────────────
claude-code:
  permission_mode: bypassPermissions     # Auto-approve all tool calls
  model: claude-sonnet-4-20250514        # Model for agent sessions
  max_turns: 50                          # CLI --max-turns (distinct from agent.max_turns)
  max_budget_usd: 5                      # Per-session cost cap

# ─── Server ────────────────────────────────────────────────────
server:
  port: 8642                             # Enable HTTP observability server

# ─── Database ──────────────────────────────────────────────────
db_path: .sortie.db                      # SQLite file next to WORKFLOW.md
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

*This document is derived strictly from the Sortie Architecture Specification
(Sections 5, 6, 9.4, and 10) and informed by end-to-end testing experience (tasks
7.11–7.13). It is the authoritative user-facing reference for workflow authors.*
