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
  - [2.4 `workspace` — Workspace Root and Retention](#24-workspace--workspace-root-and-retention)
  - [2.5 `hooks` — Workspace Lifecycle Hooks](#25-hooks--workspace-lifecycle-hooks)
  - [2.6 `agent` — Coding Agent Configuration](#26-agent--coding-agent-configuration)
  - [2.7 `db_path` — SQLite Database Path](#27-db_path--sqlite-database-path)
  - [2.8 `ci_feedback` — CI Feedback Loop (deprecated)](#28-ci_feedback--ci-feedback-loop-deprecated)
  - [2.9 `self_review` — Self-Review Configuration](#29-self_review--self-review-configuration)
  - [2.10 `reactions` — Reaction-Based Feedback Loops](#210-reactions--reaction-based-feedback-loops)
  - [2.11 `dispatch` — Rule-based Routing](#211-dispatch--rule-based-routing)
- [3. Environment Variable Overrides](#3-environment-variable-overrides)
  - [3.1 Source Precedence](#31-source-precedence)
  - [3.2 Curated Variable List](#32-curated-variable-list)
  - [3.3 Type Coercion](#33-type-coercion)
  - [3.4 `.env` File Support](#34-env-file-support)
  - [3.5 Interaction with `$VAR` Indirection](#35-interaction-with-var-indirection)
  - [3.6 Fields Not Overridable via Env](#36-fields-not-overridable-via-env)
  - [3.7 Dynamic Reload](#37-dynamic-reload)
- [4. Extensions](#4-extensions)
  - [4.1 HTTP Server (`server.port`, `server.host`)](#41-http-server-serverport-serverhost)
  - [4.2 `logging.level` — Log Verbosity](#42-logginglevel--log-verbosity)
  - [4.3 `worker` — SSH Worker Extension](#43-worker--ssh-worker-extension)
  - [4.4 `token_rates` — Cost Estimation](#44-token_rates--cost-estimation)
  - [4.5 Adapter-Specific Pass-Through Config](#45-adapter-specific-pass-through-config)
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
  - [11.3 Self-Review with Go Verification](#113-self-review-with-go-verification)
  - [11.4 Linear + Claude Code](#114-linear--claude-code)

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

The core schema recognizes eleven top-level keys:

```yaml
tracker: # Issue tracker connection and query settings
polling: # Poll loop timing
workspace: # Workspace root path
hooks: # Workspace lifecycle hook scripts
agent: # Coding agent adapter, timeouts, and limits
db_path: # SQLite database file path
ci_feedback: # CI failure feedback loop (deprecated; use reactions.ci_failure)
reactions: # Reaction-based feedback loops (CI failure, review comments)
self_review: # Self-review verification loop (optional)
dispatch: # Rule-based dispatch routing
notifications: # Operator notification backends (notify_operator tool)
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
  api_version: "3"
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
  comments:
    on_dispatch: false
    on_completion: false
    on_failure: false
```

| Field             | Type            | Required                  | Default         | Dynamic Reload                     | Description                                                                                                                                                                                     |
| ----------------- | --------------- | ------------------------- | --------------- | ---------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `kind`            | string          | **Yes** (for dispatch)    | _(none)_        | Future dispatches                  | Adapter identifier. Supported: `jira`, `github`, `linear`, `gitea`, `gitlab`, `file`. Additional adapters are registered separately.                                                            |
| `endpoint`        | string          | Adapter-defined           | Adapter-defined | Future dispatches                  | Tracker API endpoint URL. Supports `$VAR` indirection: if the value starts with `$`, it is expanded via `os.ExpandEnv`.                                                                         |
| `api_key`         | string          | When adapter requires it  | _(none)_        | Future dispatches                  | API authentication token. May be a literal or `$VAR_NAME`. If `$VAR_NAME` resolves to empty, treated as missing. The `jira`, `github`, `linear`, `gitea`, and `gitlab` adapters require this field; `file` does not. Full env expansion applied (`$VAR` at any position). |
| `project`         | string          | When adapter requires it  | _(none)_        | Future dispatches                  | Project identifier. Interpretation is adapter-defined: Jira project key, GitHub or Gitea `owner/repo`, GitLab namespace path (`group/project`) or numeric project ID, or Linear team key (e.g., `ENG`). Supports `$VAR` indirection: if the value starts with `$`, it is expanded via `os.ExpandEnv`. |
| `api_version`     | string          | No                        | `"3"`           | Future dispatches                  | Jira REST API version selector: `"3"` (Cloud) or `"2"` (Server / Data Center). Supports `$VAR` indirection. Quote the value: a bare integer (`api_version: 2`) is coerced to its decimal string but emits a validation advisory. Adapters other than Jira ignore this field. `sortie validate` rejects a value other than `"2"` or `"3"`, and rejects `"2"` against an `.atlassian.net` endpoint. |
| `active_states`   | list of strings | **Yes** (see rules below) | `[]` (empty)    | Future dispatch and reconciliation | Issue states eligible for agent dispatch. An issue is eligible for dispatch only if its state appears in this list. An empty list means no issues will be dispatched.                           |
| `terminal_states` | list of strings | **Yes** (see rules below) | `[]` (empty)    | Future dispatch and reconciliation | Issue states that release claims and trigger cleanup.                                                                                                                                           |
| `query_filter`    | string          | No                        | `""` (empty)    | Future dispatches                  | Adapter-defined query fragment that narrows the candidate issue query and, for adapters that apply it there, the terminal-state query. Each adapter interprets it in its own query language. For Jira: JQL fragment (e.g., `"labels = 'agent-ready'"`). For Linear: an `IssueFilter` JSON object, merged rather than appended (see the Linear note below). For Gitea: a URL query fragment for the repo issue-list route, merged into candidate polling only (see the Gitea note below). For GitLab: a URL query fragment for the project issue-list route, key-checked against an allowlist and merged into candidate polling only (see the GitLab note below). |
| `handoff_state`   | string          | No                        | _(absent)_      | Future worker exits                | Target tracker state for orchestrator-initiated handoff after successful worker run. When absent, no handoff transition is performed. The transition is suppressed when the issue has already reached a terminal state, and withheld when the run's frozen `handoff_evidence` policy does not permit the write ([architecture §11.5](architecture/11-issue-tracker-integration-contract.md#115-tracker-writes-important-boundary)). |
| `handoff_evidence` | string         | No                        | `observed`      | Future worker attempts             | Policy governing the evidence condition on an otherwise-eligible handoff. `observed` withholds only when absence of work is positively observed and allows an undeterminable verdict; `strict` also withholds an undeterminable verdict; `off` restores the prior four-condition decision and performs no baseline capture, exit-time workspace inspection, or evidence logging. Values outside `observed`, `strict`, and `off` are rejected as a closed set while configuration is parsed, without contacting the tracker, so startup, reload, and `sortie validate` all reject the same value. See [architecture §6.4](architecture/06-configuration-specification.md#64-config-fields-summary-cheat-sheet) for the verdict's derivation. |
| `in_progress_state` | string        | No                        | _(absent)_      | Future dispatches                  | Target tracker state for dispatch-time transition at the start of each worker attempt. When absent, no dispatch-time transition is performed. Must be in `active_states`. Must not collide with `terminal_states` or `handoff_state`. |
| `comments`        | map of booleans | No                        | all `false`     | Future dispatches (`on_dispatch`); future worker exits (`on_completion`, `on_failure`) | Toggles for orchestrator-posted tracker comments at session lifecycle points. Keys: `on_dispatch`, `on_completion`, `on_failure`. Each is a boolean defaulting to `false`. Non-boolean values are rejected with a configuration error. See [Section 3.2](#32-curated-variable-list) for the matching `SORTIE_TRACKER_COMMENTS_*` env overrides. |

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
- Both list checks compare against the effective lists. When `active_states` or
  `terminal_states` is empty, the effective list is the tracker adapter's own fallback list, and
  the dispatch preflight reports the collision under check `tracker.handoff_state`.
- Requires **write permissions** on the tracker API token. For Jira: `write:jira-work`
  (classic) or `write:issue:jira` (granular).

**`handoff_state` runtime behavior:**

- The handoff transition is suppressed when the issue has already reached a state in
  `terminal_states`. Overwriting a terminal state with the handoff state would undo an
  operator action such as cancelling the issue mid-turn.
- The orchestrator resolves the freshest tracker observation available at worker exit, in
  order: reconciliation's observation, then the worker's own per-turn state refresh, then
  the dispatch-time snapshot. A terminal result suppresses the handoff transition, the
  continuation retry, and every pending reaction enqueue for that exit.
- When the resolved observation is not terminal, one verification read runs immediately
  before the handoff write, so a state change that landed after the last observation is
  still caught. The verification read is skipped when `terminal_states` is empty, because
  no value can classify as terminal there. A failed verification read is logged and the
  handoff proceeds.
- A transition suppressed by a terminal state, or by the issue not being in an active state at
  worker exit, increments `sortie_handoff_transitions_total` with `result="skipped"`. One withheld
  by the evidence verdict increments the same counter with `result="withheld"` instead
  (see [Section 4.1](#41-http-server-serverport-serverhost)).

**`handoff_evidence` runtime behavior:**

- The evidence condition is a fifth condition on the handoff path, consulted only where four
  conditions already select that path: a handoff state is configured, the issue is still in an
  active state, the exit is not a blocked soft stop, and the dispatch drives issue state. It can
  suppress a handoff write and can never cause one.
- The verdict has three values, not two: work observed, absence of work observed, and evidence not
  determinable. It is computed at worker exit over the run's workspace against a baseline captured
  immediately before the agent starts. Under `off` no baseline is captured and no verdict is
  computed, so the four-condition decision stands unchanged.
- A withheld run makes no handoff transition and no pre-write verification read, leaves the issue
  in its active tracker state, is recorded in run history as `failed` with an error reason composed
  of a reserved marker prefix, the verdict, and the policy that withheld it, and schedules the
  ordinary exponential-backoff failure path rather than the fixed-delay continuation path.
- Consecutive withheld outcomes are counted per issue, and reaching the ceiling parks the issue
  under a label whose name is taken from `reactions.review_comments.escalation_label`. That
  ceiling's derivation from `agent.max_sessions`, the reset rule for the count, and the two
  operator gestures that release a park are specified in
  [architecture §14.2](architecture/19-failure-model-and-recovery-strategy.md#142-recovery-behavior).

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
- The `terminal_states` check compares against the effective list. When `terminal_states` is
  empty, the effective list is the tracker adapter's own fallback list, and the dispatch preflight
  reports the collision under check `tracker.in_progress_state`. The `active_states` membership
  check compares against the workflow list as written, because dispatch and reconciliation gate on
  that list.
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

**Linear tracker (`kind: linear`):**

The Linear adapter talks to Linear's single GraphQL endpoint. Configure it with the same generic
`tracker.*` fields used for every adapter; the Linear-specific interpretation of those fields is:

- `endpoint` defaults to `https://api.linear.app/graphql` and rarely needs to be set, since there
  is no self-hosted Linear.
- `api_key` is a Linear personal API key (it carries the `lin_api_` prefix). The key is sent
  verbatim in the `Authorization` header with no `Bearer` prefix, so leading or trailing
  whitespace fails authentication. Supply it through environment indirection like any other
  tracker, for example `api_key: $SORTIE_LINEAR_API_KEY`. The key resolves through the standard
  `tracker.api_key` field (env override `SORTIE_TRACKER_API_KEY`); `SORTIE_LINEAR_API_KEY` is the
  conventional variable name the `sortie validate` advisory suggests, not a separate config path.
- `project` is the Linear **team key** (the prefix in identifiers such as `ENG-123`), for example
  `ENG`. It is not a Linear project and not an `owner/repo` path. Team scoping is required because
  Linear workflow states are team-scoped.
- `active_states`, `terminal_states`, and `handoff_state` name Linear **workflow states** by their
  display name (for example `Backlog`, `Todo`, `In Progress`, `Done`, `Canceled`). The adapter
  matches names case-insensitively and verifies at startup that every configured name exists in
  the team. When `active_states` or `terminal_states` is omitted or set to an empty list, the
  adapter applies the stock Linear defaults: active `["Backlog", "Todo", "In Progress"]`, terminal
  `["Done", "Canceled", "Duplicate"]`. The general rule that an empty `active_states` pauses
  dispatch (see the field table above) does not hold for Linear: an empty list is replaced by the
  defaults, so emptying it does not stop dispatch.

`handoff_state` and `in_progress_state` also name Linear workflow states. At transition time the
adapter resolves the configured name to its team-scoped workflow-state id and applies it. Linear
imposes no transition graph, so any state can move to any state.

**Linear `query_filter`:** For `kind: linear`, `query_filter` is a Linear `IssueFilter` JSON
**object**, not a string predicate. The adapter parses the value as JSON and merges it as
additional fields of the GraphQL `filter` argument alongside the team and state constraints it
always applies, so Linear ANDs the operator filter with the base query. This differs from Jira,
where the fragment is appended to a JQL string. Rules:

- The value MUST be a JSON object. A value that is not valid JSON, or parses to a non-object,
  is a configuration error.
- The object MUST NOT contain a top-level `team` or `state` key. Those keys are reserved for the
  adapter's own team and state constraints, and supplying either is a configuration error.

```yaml
tracker:
  kind: linear
  # Only pick up issues that also carry the "Bug" label and have an assignee.
  query_filter: '{ "labels": { "name": { "eq": "Bug" } }, "assignee": { "null": false } }'
```

A minimal valid Linear workflow:

```markdown
---
tracker:
  kind: linear
  api_key: $SORTIE_LINEAR_API_KEY # Linear personal API key (lin_api_...)
  project: ENG # Linear team key
  active_states:
    - Todo
    - In Progress
  terminal_states:
    - Done
    - Canceled
  handoff_state: In Review # Linear workflow state moved to after a successful run
---

Fix {{ .issue.identifier }}: {{ .issue.title }}
```

**Gitea tracker (`kind: gitea`):**

The Gitea adapter talks to a self-hosted Gitea instance over the Gitea REST API v1. Configure it
with the same generic `tracker.*` fields used for every adapter; the Gitea-specific interpretation
of those fields is:

- `endpoint` is the instance base URL (for example `https://gitea.example.com`) and is required:
  there is no default host, because Gitea is self-hosted. Supply the site root; the adapter trims a
  trailing slash and appends `/api/v1`, and tolerates an endpoint that already ends in `/api/v1`.
  Use `https`, since the token travels in a request header.
- `api_key` is a Gitea access token. The adapter sends it verbatim as `Authorization: token <key>`
  (the canonical Gitea scheme, not a `Bearer` prefix), so leading or trailing whitespace fails
  authentication. Supply it through environment indirection like any other tracker, for example
  `api_key: $SORTIE_GITEA_TOKEN`. The key resolves through the standard `tracker.api_key` field
  (env override `SORTIE_TRACKER_API_KEY`); `SORTIE_GITEA_TOKEN` is the conventional variable name
  the `sortie validate` advisory suggests, not a separate config path.
- `project` is the repository in **`owner/repo`** form (for example `sortie-ai/sortie`): exactly one
  slash, with a non-empty owner and repository.
- `active_states`, `terminal_states`, and `handoff_state` name repository **labels**, compared
  case-insensitively (the adapter lowercases them). The adapter carries internal fallback labels
  (active `["backlog", "in-progress", "review"]`, terminal `["done", "wontfix"]`) that it applies
  when the matching list is omitted or empty, both to derive an issue's state from its labels and to
  narrow its own candidate query. The dispatch preflight also checks `handoff_state` and
  `in_progress_state` against the fallback list when the matching workflow list is empty. But the
  orchestrator gates dispatch and reconciliation on the workflow's `tracker.active_states` and
  `tracker.terminal_states`, so the field-table rule above holds for Gitea. An empty
  `active_states` dispatches nothing, and validation rejects a workflow with both lists empty.
  Configure `active_states` with the labels a dispatched Gitea issue must carry.

Gitea has no transition workflow, so the adapter derives an issue's state from its labels: it scans
the configured active, terminal, then handoff labels in order and takes the first match. An issue
carrying no configured state label falls back to the first active label when it is open and the
first terminal label when it is closed. A configured label that does not yet exist in the
repository is created on demand the first time an issue transitions into it.

`handoff_state` and `in_progress_state` also name repository labels. At transition time the adapter
swaps the issue's current state label for the target label; a move to a terminal label also closes
the issue, and a move to an active label reopens a closed one. Gitea imposes no transition graph, so
any state can move to any state. The dispatch-time `in_progress_state` transition runs through this
same label swap, so its generic validation rules (must appear in `active_states`, must not collide
with `terminal_states` or `handoff_state`) apply unchanged.

**Gitea `query_filter`:** For `kind: gitea`, `query_filter` is a URL query fragment for Gitea's
repository issue-list route, not a string predicate or a JSON object. The adapter parses it with
`url.ParseQuery` and merges the parameters into candidate polling, so an operator can scope which
open issues the agent picks up (for example, to issues assigned to or mentioning the automation
identity). Parameters combine with `&`. Rules:

- The adapter rejects only the four keys it owns: `state`, `type`, `page`, and `limit`. Naming any
  of them fails construction with a configuration error.
- Any other key is accepted and merged. A key outside Gitea's known repo issue-list filter set
  (`labels`, `q`, `milestones`, `since`, `before`, `created_by`, `assigned_by`, `mentioned_by`) is a
  likely typo, such as `assignee=` or `assigned_to=` for the Gitea key `assigned_by`. The adapter
  warns at construction and names the unrecognized key, but still passes it through unchecked. Gitea
  silently ignores a parameter it does not honor and returns every open issue, so a misspelled key
  widens rather than narrows the candidate set; correcting it is the operator's responsibility.
- A `labels` value inherits Gitea's server-side semantics: AND across comma-separated names, and
  case-sensitive matching. A name that resolves to no repository label silently drops the entire
  filter, so Gitea returns every open issue rather than an empty set. The adapter warns at
  construction when a `labels` value does not match a repository label by exact case, turning the
  silent drop into a visible signal. The warning does not block construction, because an operator
  may reference a label that does not exist yet.
- `mentioned_by` does not count an author mentioning themselves. This behavior is verified against
  the tested Gitea version but is not documented in Gitea's REST API, so it can differ across Gitea
  or Forgejo releases. `mentioned_by=<identity>` matches an issue only when the identity differs from
  the issue author, so setting the automation identity as both author and mention target yields no
  candidates.

```yaml
tracker:
  kind: gitea
  # Scope candidate polling to issues assigned to the automation identity.
  query_filter: "assigned_by=hermes-bot"
```

A minimal valid Gitea workflow:

```markdown
---
tracker:
  kind: gitea
  endpoint: https://gitea.example.com # instance base URL; the adapter appends /api/v1
  api_key: $SORTIE_GITEA_TOKEN # Gitea access token
  project: sortie-ai/sortie # owner/repo
  active_states:
    - backlog
    - in-progress
  terminal_states:
    - done
    - wontfix
  handoff_state: review # repository label moved to after a successful run
---

Fix {{ .issue.identifier }}: {{ .issue.title }}
```

**GitLab tracker (`kind: gitlab`):**

The GitLab adapter talks to GitLab.com or a self-managed instance over the GitLab REST API v4.
Configure it with the same generic `tracker.*` fields used for every adapter; the GitLab-specific
interpretation of those fields is:

- `endpoint` is the instance base URL (for example `https://gitlab.example.com`) and is optional:
  it defaults to `https://gitlab.com`, so a GitLab.com workflow omits it and a self-managed
  workflow sets it. Supply the site root; the adapter trims a trailing slash and appends `/api/v4`,
  and tolerates an endpoint that already ends in `/api/v4`, though `sortie validate` warns about the
  redundant suffix. Use `https`, since the token travels in a request header; a cleartext `http`
  endpoint also draws a validation warning.
- `api_key` is a GitLab access token: personal, project, or group. The adapter sends it verbatim in
  the `PRIVATE-TOKEN` header, so leading or trailing whitespace fails authentication. The token
  needs the `api` scope, which is the only classic scope that permits issue writes; `read_api`
  suffices only for a deployment that never transitions an issue, posts a comment, or attaches a
  label. A project access token is the least-privilege choice, because GitLab confines it to one
  project server-side, and a self-managed instance offers that token type at any license. On
  GitLab.com it requires a Premium or Ultimate subscription, so a Free namespace uses a personal
  access token. Supply the token through environment indirection like any other tracker, for
  example `api_key: $SORTIE_GITLAB_TOKEN`. The
  key resolves through the standard `tracker.api_key` field (env override `SORTIE_TRACKER_API_KEY`);
  `SORTIE_GITLAB_TOKEN` is the conventional variable name the `sortie validate` advisory suggests,
  not a separate config path.
- `project` is the project's full namespace path (for example `group/project`) or its numeric
  project ID (for example `"1"`, quoted so YAML keeps it a string). GitLab nests subgroups to any
  depth, so `group/subgroup/project` is equally valid and the adapter enforces no one-slash rule,
  unlike the GitHub and Gitea `owner/repo` grammar. A project in a user namespace works the same
  way. Write the path unencoded: the adapter percent-encodes it once for the API path, and
  `sortie validate` rejects a value that is already percent-encoded.
- `active_states`, `terminal_states`, and `handoff_state` name project or group **labels**, compared
  case-insensitively (the adapter lowercases them). The adapter carries internal fallback labels
  (active `["backlog", "in-progress", "review"]`, terminal `["done", "wontfix"]`) that it applies
  when the matching list is omitted or empty, both to derive an issue's state from its labels and to
  decide which issues are candidates. The dispatch preflight also checks `handoff_state` and
  `in_progress_state` against the fallback list when the matching workflow list is empty. But the
  orchestrator gates dispatch and reconciliation on the workflow's `tracker.active_states` and
  `tracker.terminal_states`, so the field-table rule above holds for GitLab. An empty
  `active_states` dispatches nothing, and validation rejects a workflow with both lists empty.
  Configure `active_states` with the labels a dispatched GitLab issue must carry.

GitLab has no transition workflow, so the adapter derives an issue's state from its labels: it scans
the configured active, terminal, then handoff labels in order and takes the first match. An issue
carrying no configured state label falls back to the first active label when it is open and the
first terminal label when it is closed. An issue carrying more than one configured state label logs
a warning and keeps the first.

GitLab label names are case-sensitive, and attaching a name no label matches creates that label
rather than failing. Configuring `review` for a project that already holds `Review` would therefore
leave the project with two near-duplicate labels. To prevent that, the adapter reads the project
label catalog at startup, project labels and inherited group labels alike, and rewrites every
configured state label to the casing the project already stores. Spell the configured labels however
you like; the adapter writes the project's spelling. A configured label the project does not hold
yet is not an error: it is created the first time an issue transitions into it.

`handoff_state` and `in_progress_state` also name labels. At transition time the adapter swaps the
issue's current state label for the target label and reconciles the native status in a single
request: a move to a terminal label also closes the issue, and a move to an active label reopens a
closed one. GitLab imposes no transition graph, so any state can move to any state, and a transition
that is already converged issues no write at all. The dispatch-time `in_progress_state` transition
runs through this same label swap, so its generic validation rules (must appear in `active_states`,
must not collide with `terminal_states` or `handoff_state`) apply unchanged.

**GitLab `query_filter`:** For `kind: gitlab`, `query_filter` is a URL query fragment for GitLab's
project issue-list route, not a string predicate or a JSON object. The adapter parses it with
`url.ParseQuery` and merges the parameters into candidate polling, so an operator can scope which
open issues the agent picks up. Parameters combine with `&`. Every rejection below happens at
startup, before the first poll, and `sortie validate` reports the same verdict offline by running
the same parser. Rules:

- The adapter rejects the eight keys it owns: `state`, `issue_type`, `order_by`, `sort`, `page`,
  `per_page`, `pagination`, and `with_labels_details`. Naming any of them fails construction with a
  configuration error, because overriding one changes correctness rather than scope.
- Every other key MUST be one the project issue-list route honors: `assignee_id`,
  `assignee_username`, `author_id`, `author_username`, `confidential`, `created_after`,
  `created_before`, `due_date`, `iids`, `in`, `labels`, `milestone`, `milestone_id`,
  `my_reaction_emoji`, `scope`, `search`, `updated_after`, and `updated_before`. An unrecognized key
  fails construction rather than passing through. This is stricter than the Gitea adapter, which
  warns and forwards, because GitLab silently ignores a parameter it does not recognize: a typo such
  as `assignee=` for `assignee_username=` would return every open issue, widening the candidate set
  with no visible signal.
- Negation uses GitLab's `not[...]` hash, accepted for the subset GitLab honors there:
  `not[assignee_id]`, `not[assignee_username]`, `not[author_id]`, `not[author_username]`,
  `not[iids]`, `not[labels]`, `not[milestone]`, and `not[milestone_id]`. A `not[...]` key naming
  anything else is rejected, since the excluded parameters parse without error and then have no
  effect.
- Repeat an array parameter with the `[]` suffix (`iids[]=3&iids[]=4`). A key without the suffix
  MUST carry exactly one value: repeating it is rejected, because repeat semantics are not portable
  across GitLab versions.
- Naming one parameter under two spellings is rejected: `labels` and `labels[]` are the same
  parameter. `labels` and `not[labels]` are different parameters and MAY both appear.
- A value carrying an empty comma-separated segment (`labels=ready,,urgent`) is rejected.
- `scope` is both adapter-set and operator-settable. The adapter polls with `scope=all`; an operator
  value replaces it rather than combining with it, which is how a filter narrows polling to, for
  example, `scope=assigned_to_me`.
- A `labels` value inherits GitLab's server-side semantics: AND across comma-separated names, and
  case-sensitive matching. A name matching no label returns an empty result rather than dropping the
  filter, so a misspelling shows up as "no candidates" instead of "every candidate". The adapter
  still warns at construction for each `labels` name absent from the project catalog, once per
  distinct name, so the empty result has a stated cause. The warning does not block construction,
  because an operator may reference a label that does not exist yet. GitLab reads `none` and `any`
  as wildcards on the non-negated `labels` parameter, so neither is checked against the catalog
  there.

The filter merges into the open-issue listings that back candidate polling and not into the
closed-issue listing used for terminal-state cleanup, so an operator filter never hides a terminal
issue from reconciliation. The batched state lookups that reconcile active runs address issues by
`iid` and carry no filter either, so a running issue stays visible even after an edit moves it
outside the filter.

```yaml
tracker:
  kind: gitlab
  # Scope candidate polling to issues assigned to the automation identity
  # that do not carry the "blocked" label.
  query_filter: "assignee_username=hermes-bot&not[labels]=blocked"
```

A minimal valid GitLab workflow:

```markdown
---
tracker:
  kind: gitlab
  endpoint: https://gitlab.example.com # omit for GitLab.com; the adapter appends /api/v4
  api_key: $SORTIE_GITLAB_TOKEN # GitLab access token, api scope
  project: group/project # namespace path, or a quoted numeric project ID
  active_states:
    - backlog
    - in-progress
  terminal_states:
    - done
    - wontfix
  handoff_state: review # label moved to after a successful run
---

Fix {{ .issue.identifier }}: {{ .issue.title }}
```

Four prompt-template variables (see [Section 5.2](#52-template-input-variables)) are always empty
for this tracker, so a template that reads them renders nothing rather than failing:
`{{ .issue.priority }}`, because GitLab issues carry no priority field; `{{ .issue.blocked_by }}`,
because Community Edition has no blocking relationship between issues; `{{ .issue.parent }}`,
because the issue route exposes no sub-issue relationship; and `{{ .issue.branch_name }}`, because
GitLab computes branch names in the UI rather than storing one on the issue. Both
`{{ .issue.id }}` and `{{ .issue.identifier }}` are the project-scoped issue number (GitLab's
`iid`), the number shown as `#7` in the GitLab UI, never the instance-global issue ID.

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

### 2.4 `workspace` — Workspace Root and Retention

```yaml
workspace:
  root: ~/workspace/sortie
  retention_days: 30
```

| Field            | Type    | Required | Default                           | Dynamic Reload               | Description                                                                                                                     |
| ---------------- | ------- | -------- | ---------------------------------- | ----------------------------- | -------------------------------------------------------------------------------------------------------------------------------- |
| `root`           | path string or `$VAR` | No | `<system-temp>/sortie_workspaces` | Future workspace operations | Base directory for per-issue workspaces. |
| `retention_days` | integer | No       | `0` (disabled)                    | Applies on the next sweep pass | Maximum age in days of a swept workspace's latest recorded activity before the periodic sweep removes it. `0` disables the bound; `1`-`29` is rejected; `30` is the smallest permitted non-zero value. |

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

> **Note:** `after_create` runs only when Sortie first creates the per-issue
> workspace directory, so the directory is empty when the clone runs. If
> `after_create` fails, Sortie removes the directory before the next retry, so
> a retry also starts from an empty directory. A clone error such as
> "destination path already exists" or "directory not empty" does not come from
> this example on the normal path.
>
> Hooks also run with a restricted environment: an allowlist (including `HOME`
> and `SSH_AUTH_SOCK`) plus `SORTIE_*` variables. Sortie strips any variable
> outside that set, such as `GIT_SSH_COMMAND`, so an SSH clone must reach its
> key through the SSH agent (`SSH_AUTH_SOCK`) or through `~/.ssh` via `HOME`,
> not through a stripped variable. See Section 6.2 for the full allowlist.

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
| `kind`                           | string                            | No                                  | `claude-code`   | Future dispatches                          | Agent adapter identifier. This is the default kind used when no `dispatch.rules` entry (and no `dispatch.default.agent`) overrides it. Built-in adapters: `claude-code`, `copilot-cli`, `codex`, `opencode`, and `kiro`. Other kinds (for example, HTTP-based adapters) are available only if you register them separately. |
| `command`                        | string (shell command)            | When adapter requires local process | Adapter-defined | Future dispatches                          | Shell command to launch the agent for adapters that run as a local subprocess (such as `claude-code`, `opencode`, or `kiro`). Adapters that do not start a local process ignore this field. |
| `turn_timeout_ms`                | integer                           | No                                  | `3600000` (1h)  | Future worker attempts                     | Total timeout for a single agent turn.                                                                                                                                           |
| `read_timeout_ms`                | integer                           | No                                  | `5000` (5s)     | Future worker attempts                     | Request/response timeout during startup and synchronous operations.                                                                                                              |
| `stall_timeout_ms`               | integer                           | No                                  | `300000` (5m)   | Future worker attempts                     | Inactivity timeout based on event stream gaps. Set to `0` or negative to **disable** stall detection.                                                                            |
| `max_concurrent_agents`          | integer or string integer         | No                                  | `10`            | **Yes** — affects subsequent dispatch      | Global concurrency limit across all issues.                                                                                                                                      |
| `max_turns`                      | integer                           | No                                  | `20`            | Future dispatches                          | Maximum coding-agent turns per worker session. The worker re-checks tracker state after each turn and starts another turn if the issue is still active, up to this limit.        |
| `max_retry_backoff_ms`           | integer or string integer         | No                                  | `300000` (5m)   | **Yes** — affects future retry scheduling  | Maximum delay cap for exponential backoff on retries.                                                                                                                            |
| `max_concurrent_agents_by_state` | map of `state → positive integer` | No                                  | `{}` (empty)    | **Yes** — affects subsequent dispatch      | Per-state concurrency limits. State keys are normalized to lowercase for lookup. Non-positive or non-numeric entries are silently ignored.                                       |
| `max_sessions`                   | integer                           | No                                  | `0` (unlimited) | **Yes** — affects future retry evaluations | Maximum completed worker sessions per issue before the orchestrator stops re-dispatching. Counted from run history. `0` disables the budget (unlimited). Must be non-negative.   |
| `max_tokens`                     | integer                           | No                                  | `0` (unlimited) | **Yes** — affects future retry evaluations | Cumulative per-issue token ceiling. The orchestrator sums `total_tokens` across the issue's run history and stops re-dispatching once the sum reaches the limit. A run whose coding agent reported no token usage contributes nothing to the sum, and such a run makes the ceiling report that it could not be fully evaluated (a warning is logged and the dispatch proceeds). `0` disables the budget (unlimited). Must be non-negative. |

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

### 2.8 `ci_feedback` — CI Feedback Loop (**deprecated**)

> **Deprecated.** Use `reactions.ci_failure` instead (Section 2.10). When both `ci_feedback`
> and `reactions.ci_failure` are present, `reactions.ci_failure` takes precedence and a
> deprecation warning is logged at startup.

```yaml
ci_feedback:
  kind: github
  max_retries: 2
  max_log_lines: 50
  escalation: label
  escalation_label: needs-human
```

| Field              | Type    | Required              | Default        | Dynamic Reload    | Description                                                                                                           |
| ------------------ | ------- | --------------------- | -------------- | ----------------- | --------------------------------------------------------------------------------------------------------------------- |
| `kind`             | string  | **Yes** (to activate) | _(absent)_     | Requires restart  | CI status provider adapter identifier (e.g., `github`). When absent or empty, CI feedback is disabled entirely.       |
| `max_retries`      | integer | No                    | `2`            | Future dispatches | Maximum CI-fix continuation dispatches per issue before escalation. `0` means escalate immediately on first CI failure. Must be non-negative. |
| `max_log_lines`    | integer | No                    | `50`           | Requires restart  | Maximum lines to fetch from the first failing CI check log. `0` disables log fetching. Must be non-negative.          |
| `escalation`       | string  | No                    | `label`        | Future dispatches | Action when `max_retries` is exceeded. Valid values: `"label"` (add a label to the issue), `"comment"` (post a comment on the issue). |
| `escalation_label` | string  | No                    | `needs-human`  | Future dispatches | Label applied to the issue when `escalation` is `"label"`.                                                            |

The deprecated `ci_feedback` section exposes no `watch_window_ms` key. A deployment that still uses
this section receives the default value described in Section 2.10.

**Activation pattern:** CI feedback has no `enabled` flag. The feature is active when
`ci_feedback.kind` is present and non-empty. Omit the entire `ci_feedback` section to
disable the feature:

```yaml
# CI feedback disabled — section omitted entirely
# ci_feedback:
#   kind: github
```

**SCM coordinates:** Owner, repository, and token are not part of `ci_feedback`. They
live in the adapter pass-through block (e.g., the existing `github:` top-level section)
and are shared with the tracker adapter when both use the same SCM provider.

**Validation rules:**

- `max_retries` must be non-negative. Negative values are rejected with a configuration
  error.
- `max_log_lines` must be non-negative. Negative values are rejected with a configuration
  error.
- `escalation` must be `"label"` or `"comment"`. Other values are rejected with a
  configuration error.
- When `kind` is absent or empty, all other fields in the section are ignored and the
  `CIFeedbackConfig` is a zero value.

---

### 2.9 `self_review` — Self-Review Configuration

```yaml
self_review:
  enabled: true
  max_iterations: 3
  verification_commands:
    - "go test ./..."
    - "go vet ./..."
  verification_timeout_ms: 120000
  max_diff_bytes: 102400
  reviewer: same
```

The self-review section configures an optional post-coding verification and iterative
review phase. When enabled, the orchestrator runs verification commands after the coding
turn loop, generates a workspace diff, and presents both to the agent for a structured
verdict. If the agent identifies issues ("iterate"), a fix turn runs and the cycle repeats
up to `max_iterations`.

| Key                        | Type     | Required           | Default    | Description                                                       |
| -------------------------- | -------- | ------------------ | ---------- | ----------------------------------------------------------------- |
| `enabled`                  | boolean  | No                 | `false`    | Activates the self-review loop.                                   |
| `max_iterations`           | integer  | No                 | `3`        | Hard cap on review iterations. Range: 1–10.                       |
| `verification_commands`    | [string] | When `enabled: true` | —        | Shell commands to run during each review iteration.               |
| `verification_timeout_ms`  | integer  | No                 | `120000`   | Per-command timeout in milliseconds.                              |
| `max_diff_bytes`           | integer  | No                 | `102400`   | Max bytes of diff to include in review prompt.                    |
| `reviewer`                 | string   | No                 | `"same"`   | Which agent reviews. Only `"same"` in v1.                         |

**Turn Accounting:** `max_iterations: N` means up to `2N − 1` additional agent turns
(N review turns + N−1 fix turns). For the default `max_iterations: 3`, this is up to 5
additional turns beyond the coding turn loop. Plan token budgets accordingly.

**Validation rules:**

- When `enabled` is `true`, `verification_commands` must be a non-empty list. An empty list
  is rejected with a configuration error.
- `max_iterations` must be in the range [1, 10]. Values outside this range are rejected with
  a configuration error.
- `reviewer` must be `"same"`. Other values are reserved for future use and are rejected
  with a configuration error.
- When `enabled` is `false` or the section is absent, all other fields are ignored and the
  self-review phase adds zero overhead.

---

### 2.10 `reactions` — Reaction-Based Feedback Loops

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

The `reactions` section configures feedback loops that respond to external events (CI
failures, PR review comments) by dispatching continuation turns to the agent. Each key
under `reactions` identifies a reaction kind.

**Common fields** (shared across all reaction kinds):

| Field              | Type    | Required              | Default       | Dynamic Reload    | Description                                                                                     |
| ------------------ | ------- | --------------------- | ------------- | ----------------- | ----------------------------------------------------------------------------------------------- |
| `provider`         | string  | **Yes** (to activate) | _(absent)_    | Requires restart | Adapter identifier (e.g. `github`). When absent or empty, the reaction kind is disabled.         |
| `max_retries`      | integer | No                    | `2`           | Requires restart | Maximum fix continuation dispatches per issue before escalation. Must be non-negative.           |
| `escalation`       | string  | No                    | `label`       | Requires restart | Action when retries are exhausted. Valid: `"label"`, `"comment"`.                                |
| `escalation_label` | string  | No                    | `needs-human` | Requires restart | Label applied when `escalation` is `"label"`.                                                    |

**Reload behavior:** every field of every reaction kind is read once when the orchestrator
is constructed and is not rebuilt on a `WORKFLOW.md` reload, so a change takes effect only
on the next restart. `reactions.ci_failure` is the single exception: the orchestrator folds
it into the `ci_feedback` shape and re-reads `max_retries`, `escalation`,
`escalation_label`, and `watch_window_ms` from the reloaded config on each tick. Its
`max_log_lines` still requires a restart, because the CI provider is constructed once at
process start.

**Escalation recurrence:** `escalation: label` is idempotent (re-applying a
present label is a no-op); `escalation: comment` posts a new comment each time
it fires. For kinds whose escalation releases the issue claim (`ci_failure`,
`review_comments`), escalation fires once and the reaction stops. For kinds
whose escalation is scoped and keeps the claim (`auto_merge`, `bot_review`), the
reaction re-arms if its condition recurs and escalates again, so on a long-lived
PR `escalation: comment` can accumulate repeated comments while `escalation:
label` stays a single mark. Prefer `label` for kinds that may escalate
repeatedly.

**Release on terminal state:** each reconcile pass reads tracker state for every running
issue and for every issue holding a pending reaction entry, whether or not a worker is
still running for it. When the tracker reports an issue in a state from
`tracker.terminal_states`, the pass releases that issue's pending reaction entries, its
reaction attempt counters, its pending retry, and its dispatch claim. Polling for that
issue stops on the same pass. The release is scoped to in-memory state and leaves the
`reaction_fingerprints` rows untouched.

Remaining keys within a kind sub-object are kind-specific and collected into an `Extra` map.

**SCM and CI provider selection.** The reaction `provider` field (and the deprecated
`ci_feedback.kind`) names a registered SCM adapter or CI provider kind: `github`, `gitea`, or
`gitlab`. The reaction kinds are provider-agnostic. Setting `provider: gitea` activates the Gitea
adapter for that reaction. Its `endpoint`, `api_key`, and `project` come from the top-level `gitea:`
pass-through block ([Section 4.5](#45-adapter-specific-pass-through-config)); when `tracker.kind`
is also `gitea`, any of the three left unset in that block falls back to the matching `tracker:`
value. A Gitea reaction can therefore pair with a non-Gitea tracker, as long as the `gitea:` block
supplies the credentials, including the instance `endpoint` Gitea always requires. Setting
`provider: gitlab` activates the GitLab adapter for that reaction and resolves the same three
fields from the top-level `gitlab:` block by the same rule. `endpoint` is optional there, because
the GitLab adapter defaults it to `https://gitlab.com`; only a self-managed instance sets one. The
GitLab SCM adapter ignores `project` and takes the owner and repository from the pull request
metadata on every call, while the GitLab CI provider requires `project`, so a GitLab `ci_failure`
reaction paired with a non-GitLab tracker MUST set it in the `gitlab:` block. Every active
SCM reaction in one workflow MUST name the same provider.

#### Reaction kind: `ci_failure`

Equivalent to the deprecated `ci_feedback` section. Configures the CI failure feedback
loop. The orchestrator resolves the pull request's current head on each pass and polls CI
status for that head, dispatching continuation turns when CI fails on it.

Additional fields (via Extra):

| Field             | Type    | Default      | Dynamic Reload | Description                                                                                                             |
| ----------------- | ------- | ------------ | -------------- | ------------------------------------------------------------------------------------------------------------------------ |
| `max_log_lines`   | integer | `50`         | Requires restart | Maximum CI log tail lines for prompt injection. `0` disables. Must be non-negative.                                   |
| `watch_window_ms` | integer | `86400000`   | Every tick        | Bounds a pending entry's age, measured from the last recorded head. `0` removes the clock bound. Must be non-negative. |

Example:

```yaml
reactions:
  ci_failure:
    provider: github
    max_retries: 2
    max_log_lines: 50
    escalation: label
    escalation_label: needs-human
```

#### Reaction kind: `review_comments`

PR review comment routing. When configured, the orchestrator polls for human
`CHANGES_REQUESTED` review comments on Sortie-created PRs and dispatches continuation
turns so the agent can address the feedback.

Additional fields (via Extra):

| Field                    | Type    | Default  | Dynamic Reload    | Description                                                                                               |
| ------------------------ | ------- | -------- | ----------------- | --------------------------------------------------------------------------------------------------------- |
| `poll_interval_ms`       | integer | `120000` | Requires restart | Polling interval for review comments. Minimum: `30000` (30 sec).                                           |
| `debounce_ms`            | integer | `60000`  | Requires restart | Debounce window after the last detected comment before dispatching. Must be non-negative.                  |
| `max_continuation_turns` | integer | `3`      | Requires restart | Maximum review-fix continuation dispatches per issue before escalation. Must be positive.                  |

**Activation:** The `reactions.review_comments` block is active when `provider` is present
and non-empty. Agent-created PRs MUST write `pr_number`, `owner`, and `repo` to
`.sortie/scm.json` in the workspace for review polling to activate.

**SCM coordinates:** The `owner` and `repo` values are sourced from `.sortie/scm.json`
(written by the agent), not from the tracker project configuration. This decouples SCM
repository identity from the tracker project identifier.

**Recovery timestamp:** Hooks that push commits or create PRs SHOULD write `pushed_at` to
`.sortie/scm.json` when review or CI reaction recovery is enabled. Startup recovery uses
`pushed_at` to decide whether handoff-stage reaction work is fresh. If `pushed_at` is absent,
recovery falls back to `run_history.completed_at`, so long-lived PRs can age out based on the
agent completion time instead of the latest push time.

**Debounce behavior:** When review comments are detected but the newest comment timestamp
is within the debounce window, dispatch is deferred. This ensures the reviewer has finished
their full review before the agent starts addressing comments.

**Fingerprint dedup:** The orchestrator computes a SHA-256 fingerprint from sorted
non-outdated comment IDs. If the fingerprint has not changed since the last dispatch,
no new continuation is triggered. Fingerprints are persisted across restarts in the
`reaction_fingerprints` SQLite table.

**Bot exclusion:** Human-loop selection drops a comment whose author the platform reports as
an automated bot account, and separately drops a comment whose author matches
`reactions.bot_review.bot_usernames`. The allowlist half takes effect only while
`reactions.bot_review` is configured with a provider, because that is the only condition under
which the allowlist is built; a deployment running `review_comments` alone sees no allowlist
exclusion.

**Escalation:** When `max_continuation_turns` is exhausted, the orchestrator applies the
configured escalation action (label or comment) and releases the claim.

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

#### Reaction kind: `auto_merge`

PR auto-merge. When configured and enabled, the orchestrator polls merge preconditions
(review decision, CI status, draft state, mergeability) on Sortie-created PRs and
executes the merge directly through the SCM adapter once the compound condition is
satisfied. Auto-merge is disabled by default; enabling it is an explicit opt-in because
the merge is irreversible.

Additional fields (via Extra):

| Field              | Type    | Default  | Dynamic Reload    | Description                                                                                              |
| ------------------ | ------- | -------- | ----------------- | -------------------------------------------------------------------------------------------------------- |
| `strategy`         | string  | `squash` | Requires restart | Merge strategy. One of `merge`, `squash`, or `rebase`.                                                    |
| `require_ci`       | boolean | `true`   | Requires restart | When `true`, the merge waits until every reported check has completed with no failing conclusion; a `skipped` or `neutral` conclusion counts as non-failing. When `false`, CI is advisory only.    |
| `delete_branch`    | boolean | `true`   | Requires restart | When `true`, the PR head branch is deleted after a successful merge. Failure to delete does not roll back the merge. |
| `poll_interval_ms` | integer | `60000`  | Requires restart | Polling interval for the precondition state machine. Minimum: `30000` (30 sec).                           |

For a GitLab provider, `require_ci` reads the head pipeline the platform reports for the PR
rather than a fetched check-run list, with one exception. A head pipeline the platform reports
as `skipped` satisfies the gate, because that status is non-failing. A head pipeline the
platform reports as `manual` is the exception, and the one case that costs a second request.
GitLab reports `manual` both for a pipeline whose only remaining work is a manual job and for
one that already carries a failed job beside it, so `require_ci` reads the commit statuses
GitLab records for that pipeline and applies to them the same rule it applies to every other
check. An untriggered manual job is not a failure, so a manual pipeline carrying no failed job
satisfies the gate even though that job has not run; one carrying a failed job does not; one
whose manual gate still holds later jobs at not-yet-started defers until they run. A project's "Pipelines
must succeed" setting is the control that holds a merge on a pipeline that did not succeed.

**Activation:** The `reactions.auto_merge` block is active when `provider` is present
and non-empty. Agent-created PRs MUST write `pr_number` (positive integer), `owner`,
`repo`, and `branch` (all non-empty) to `.sortie/scm.json` in the workspace for
auto-merge to activate. Recovery on restart additionally consults `pushed_at` (when
present) to skip stale PRs older than the recovery lookback window.

**Token scopes required:** for a GitHub provider the configured token must carry
`pull_requests:write` for the merge endpoint, and when `delete_branch` is not `false`, also
`contents:write` for the branch delete endpoint. The classic `repo` scope is a superset that
covers both. The orchestrator validates scopes once at startup; failure emits an ERROR log with
the missing scope and disables auto-merge for the process lifetime. A transport-class startup
failure (network outage) schedules a single bounded retry on the next reconcile tick before
disabling auto-merge.

For a Gitea provider the scope model differs: Gitea has one coarse `write:repository` scope
covering both the merge and branch-delete routes, and it exposes no scope-introspection surface.
The startup check substitutes a user-role gate, reading the repository's `permissions.push` field
and disabling auto-merge when the token's user lacks repository write access. A read-scoped token
whose user has write access passes that gate and surfaces the missing scope only at runtime, as a
403 on the first merge or branch delete that the adapter rewrites to name `write:repository`.
Grant the token's user repository write access and the token the `write:repository` scope.

For a GitLab provider one scope covers the whole API. `api` grants complete read and write access,
so it covers the merge and branch-delete routes together and `delete_branch` does not change what
the token needs; `read_api` covers the reads and is refused on every write. The startup check reads
`GET /personal_access_tokens/self`: a classic token whose scopes omit `api` is a verified gap and
disables auto-merge, while four responses leave the check unable to classify the token at all and
auto-merge enabled. Those four are the opaque `granular` scopes value a fine-grained token reports,
an empty scopes array, an unreadable introspection body, and a 404 on the introspection route. That
skip is not a preflight failure and blocks nothing, so confirm a fine-grained token's permissions
directly. Grant the token the `api` scope.

**Fingerprint dedup:** The fingerprint is the SHA-256 of the PR head SHA concatenated
with the review decision. A new push or a change in review decision invalidates the
fingerprint and allows a new merge attempt; an unchanged fingerprint with the dispatched
flag set suppresses duplicate merge calls within the poll interval.

**Cross-kind isolation:** Auto-merge success and failure cleanup scope mutation to the
`merge` kind only. CI failure and review comment continuations on the same issue are
unaffected by a successful merge or by escalation.

**Escalation:** When `max_retries` is exhausted, the orchestrator applies the configured
escalation action (label or comment) and removes the pending merge entry. Other reaction
kinds on the same issue are preserved.

Example:

```yaml
reactions:
  auto_merge:
    provider: github
    max_retries: 2
    escalation: comment
    strategy: squash
    require_ci: true
    delete_branch: true
    poll_interval_ms: 60000
```

#### Reaction kind: `bot_review`

Automated review-bot comment routing. When configured, the orchestrator polls for
PR comments authored by automated review tools (linters, static analyzers, security
scanners, and AI reviewers such as GitHub Copilot or CodeRabbit) on Sortie-created PRs and dispatches continuation turns so
the agent can address them. This is the complement of `review_comments`, which routes
only human `CHANGES_REQUESTED` comments and excludes bot-authored ones.

Additional fields (via Extra):

| Field                    | Type         | Default | Dynamic Reload    | Description                                                                                               |
| ------------------------ | ------------ | ------- | ----------------- | --------------------------------------------------------------------------------------------------------- |
| `bot_usernames`          | list[string] | _(empty)_ | Requires restart | Allowlist of bot logins. A comment is bot-authored when the platform reports a bot user type OR its author login matches an entry here (case-insensitive). An entry here is also excluded from `review_comments`, so an allowlisted author does not drive both loops. |
| `poll_interval_ms`       | integer      | `60000` | Requires restart | Polling interval for bot comments. Minimum: `30000` (30 sec). The default is tighter than `review_comments` because bot comments arrive in bulk on push.  |
| `max_continuation_turns` | integer      | `5`     | Requires restart | Maximum bot-fix continuation dispatches per issue before escalation. Must be positive. Higher than `review_comments` because bot fixes are mechanical.    |

**Activation:** The `reactions.bot_review` block is active when `provider` is present
and non-empty, on its own, with no `reactions.review_comments` or `reactions.auto_merge`
block required. When `provider` is absent or empty, the block is inactive. Agent-created
PRs MUST write `pr_number` (positive integer), `owner`, `repo`, and `branch` (all non-empty)
to `.sortie/scm.json` in the workspace for bot-review polling to activate.

**Classification:** Bot detection is deterministic author-metadata matching, not a
content heuristic. A comment is selected when the platform marks its author as a bot OR
its author login matches a `bot_usernames` entry, case-insensitively. The `bot_usernames`
allowlist covers review tools that comment under a regular user account (`user.type == "User"`)
rather than a bot account, such as Hound (`houndci-bot`), which posts inline style comments under a regular user account. Unlike
`review_comments`, no `CHANGES_REQUESTED` review state is required, because review bots
commonly post comment-only reviews. The same `bot_usernames` entry also excludes its author from
`review_comments`, so a `bot_review` comment set and a `review_comments` comment set never both
carry the same allowlisted author's feedback.

**No debounce:** Bot comments are dispatched immediately. There is no `debounce_ms`
field; the debounce window that `review_comments` applies does not apply to `bot_review`,
because bot comments arrive in bulk on push rather than at reviewer pace.

**Fingerprint dedup:** The orchestrator computes a SHA-256 fingerprint from sorted
non-outdated comment IDs and persists it in the `reaction_fingerprints` SQLite table
under a kind distinct from `review_comments`. A new push that changes the bot comment-ID
set changes the fingerprint and re-triggers dispatch; an unchanged dispatched fingerprint
suppresses re-dispatch within the poll interval.

**Cross-kind isolation:** `bot_review` and `review_comments` never interfere on the same
PR; each owns its own pending entry, fingerprint row, and attempt counter. Escalation
cleanup is scoped to the `bot_review` kind only and does not release the issue claim or
clear sibling reaction kinds.

**Escalation:** When `max_continuation_turns` is exhausted, the orchestrator applies the
configured escalation action (`label` or `comment`, default `label`, with
`escalation_label` defaulting to `needs-human`) and removes the pending bot-review entry.
Other reaction kinds on the same issue are preserved.

Example:

```yaml
reactions:
  bot_review:
    provider: github
    escalation: label
    escalation_label: needs-human
    poll_interval_ms: 60000
    max_continuation_turns: 5
    bot_usernames:            # only for review tools that comment under a user account, not a bot account
      - houndci-bot           # e.g. Hound (houndci.com), comments as a type:User account
```

#### Reaction kind: `merge_conflicts`

Merge-conflict detection and resolution. When configured, the orchestrator polls
mergeability on Sortie-created open PRs each reconcile cycle and dispatches one
rebase-and-resolve continuation turn each time a PR transitions from no-conflict to
conflict. The continuation carries the PR's real base branch so the agent rebases the PR
head branch onto the correct target and resolves the conflicts on the existing workspace.

Retry semantics are episodic: a resolved conflict closes the episode and resets the
attempt counter, so a later independent conflict opens a fresh episode rather than
counting against the prior budget. The default `max_retries` is `1`, lower than the other
kinds, because merge-conflict resolution is less likely to succeed on retry than a CI fix.

Fields:

| Field              | Type    | Default       | Dynamic Reload    | Description                                                                                              |
| ------------------ | ------- | ------------- | ----------------- | -------------------------------------------------------------------------------------------------------- |
| `provider`         | string  | _(required)_  | Requires restart | SCM adapter kind. Must match the provider of every other active SCM reaction. Activates the kind.         |
| `max_retries`      | integer | `1`           | Requires restart | Per-episode rebase attempts before escalation. `0` escalates on first detection without a rebase attempt. |
| `escalation`       | string  | `label`       | Requires restart | Escalation action when retries are exhausted. One of `label` or `comment`.                                |
| `escalation_label` | string  | `needs-human` | Requires restart | Label applied when `escalation` is `label`.                                                               |
| `poll_interval_ms` | integer | `60000`       | Requires restart | Polling interval for the conflict-detection state machine. Minimum: `30000` (30 sec).                     |

**Activation:** The `reactions.merge_conflicts` block is active when `provider` is present
and non-empty, on its own, with no other `reactions` block required. Agent-created PRs MUST
write `pr_number` (positive integer), `owner`, `repo`, and `branch` (all non-empty) to
`.sortie/scm.json` in the workspace for merge-conflict polling to activate. To disable the
reaction, omit the `merge_conflicts` block; setting `max_retries: 0` does not disable it but
escalates on the first detected conflict.

**Fingerprint dedup:** The fingerprint is the SHA-256 of the PR head SHA, persisted in the
`reaction_fingerprints` SQLite table under a kind distinct from the other reactions. A
single conflicted head dispatches exactly one rebase turn; after the agent rebases and
pushes a new head, the new head produces a new fingerprint, so a conflict that persists
across the rebase re-arms a new attempt bounded by `max_retries`. When the conflict clears,
the row is deleted, so the next conflict observation dispatches again.

**Cross-kind isolation:** Merge-conflict detection runs independently of every other
reaction kind; each owns its own pending entry, fingerprint row, and attempt counter.
Escalation cleanup is scoped to the `merge-conflict` kind only and does not release the
issue claim or clear sibling reaction kinds. Auto-merge and merge-conflict coexist on the
same PR: auto-merge defers while a PR is conflicted, and merge-conflict drives the
resolution, after which auto-merge proceeds once the PR is clean and approved.

**Escalation:** When `max_retries` is exhausted within an episode (a new conflicting head
pushes the attempt count strictly over the budget), the orchestrator applies the configured
escalation action (`label` or `comment`, default `label`, with `escalation_label`
defaulting to `needs-human`) and removes the pending merge-conflict entry and its attempt
counter. Other reaction kinds on the same issue are preserved.

Example:

```yaml
reactions:
  merge_conflicts:
    provider: github
    max_retries: 1
    escalation: label
    escalation_label: needs-human
    poll_interval_ms: 60000
```

#### Reaction kind: `label_commands`

Label-command detection. When configured, the orchestrator polls each Sortie-managed PR's
label-event journal and dispatches a read-only, no-clone agent session when an operator
applies the configured review label. The session reads the PR diff and posts review
comments under the agent's own identity; it performs no branch mutation and requires no
repository checkout.

Unlike the other reaction kinds, `label_commands` carries no escalation semantics and does
not use the shared per-kind fields (`max_retries`, `escalation`, `escalation_label`). It
parses through a dedicated path and never appears as a generic reaction entry, so its
fields sit directly on the block.

`label_commands` is the configuration key only. The runtime and persisted reaction-kind
discriminators are `label-review` (the review command) and `label-fix` (the fix command):
these values appear in logs and in the `reaction_fingerprints.kind` column. Each command
has its own prompt continuation key, a third and fourth name: `label_review` and
`label_fix` (both underscore; see Section 5.2).

Fields:

| Field              | Type    | Default         | Dynamic Reload   | Description                                                                                             |
| ------------------ | ------- | --------------- | ---------------- | ------------------------------------------------------------------------------------------------------- |
| `provider`         | string  | _(required)_    | Requires restart | SCM adapter kind. Must match the provider of every other active SCM reaction. Activates the block.      |
| `review_label`     | string  | `sortie:review` | Requires restart | Label that triggers a read-only review. An explicit empty string disables the review command.           |
| `fix_label`        | string  | `sortie:fix`    | Requires restart | Label that triggers the fix command. An explicit empty string disables the fix command.                 |
| `poll_interval_ms` | integer | `60000`         | Requires restart | Journal poll interval. Minimum `30000` (30 sec); a lower value is clamped up to the floor with a warning. |

**Activation:** The `reactions.label_commands` block is active when `provider` is present
and non-empty and at least one command label is non-empty. Activation considers `fix_label`
exactly as it considers `review_label`: a fix-only configuration (`review_label` empty,
`fix_label` non-empty) activates the block and constructs the SCM adapter exactly as a
review-only configuration does. Agent-created PRs MUST write `pr_number` (positive
integer), `owner`, and `repo` (all non-empty) to `.sortie/scm.json` in the workspace for
label detection to activate. The review command requires no branch: the review session has
no checkout. The fix command additionally requires a non-empty `branch` in the same file,
because the fix session checks out and pushes to it; a PR record without one is not seeded
for the fix command.

**Acknowledgment:** On a confirmed command the orchestrator removes the label from the PR.
The label's disappearance is the operator-visible signal that the command was accepted:
present means "queued, not yet picked up"; gone means "accepted" (or retracted by the
operator). A removal failure leaves the label in place and logs a warning; correctness is
unaffected because deduplication rests on the journal position, not the removal. Re-applying
the label after a review completes is a new gesture and produces a new session.

**Validation:** Setting `provider` while both `review_label` and `fix_label` are empty is a
configuration error. Because the defaults are non-empty, this occurs only when the operator
explicitly sets both labels to `""`. The rule is a config-shape check and surfaces offline
via `sortie validate`. A `provider` naming an unregistered SCM adapter is also a `validate`
error, reported under the check name `scm_adapter` by the activation checks that fold every
active SCM reaction kind, `label_commands` included, into one provider set.

**Operator prerequisite:** Enabling `review_label` requires the active prompt template to
contain a `{{ if .label_review }}` branch that fetches the PR diff and posts review comments
using the agent's own SCM tooling (see the `label_review` continuation key in Section 5.2).
The orchestrator injects only the PR coordinates, never the diff text and never a posted
comment, so without this branch a label-review dispatch runs the normal work prompt and
posts no review. The orchestrator emits an info log at each dispatch and a warning at prompt
load when the template omits the `label_review` token, so the inert outcome is diagnosable.

**Operator prerequisite (fix):** Enabling `fix_label` requires the active prompt template to
contain a `{{ if .label_fix }}` branch that checks out `label_fix.branch`, fetches and
addresses the outstanding review comments, pushes the fixes to that branch, posts a summary
comment, and writes `.sortie/status` to signal completion (see the `label_fix` continuation
key in Section 5.2). The completion signal matters more here than for review: a review is
naturally one turn, but a fix is multi-turn, so without it a completed fix session runs to
`agent.max_turns` and wastes turns. Unlike the review case, a missing `label_fix` branch is
not a structural no-op: the fix command clones and checks out a real workspace with
content-write scope, so an applied fix label without the template branch runs the normal
work prompt against a real checkout that can push. The workflow loader MUST (not SHOULD)
emit a warning at prompt load when `label_commands` is active with a non-empty fix label but
the template omits the `label_fix` token, and the orchestrator emits an info log at each fix
dispatch, so the misconfiguration stays diagnosable.

**Default-on activation:** `fix_label` defaults to `sortie:fix`, so shipping the fix command
activates it for every deployment that already sets `provider` for `label_commands`,
including a deployment that enabled only the review command. A review-only deployment MUST
set `reactions.label_commands.fix_label: ""` to opt out.

Example:

```yaml
reactions:
  label_commands:
    provider: github
    review_label: sortie:review
    fix_label: sortie:fix
    poll_interval_ms: 60000
```

#### Reaction kind: `merge_completion`

Merge-completion detection. When configured, the orchestrator observes the merge state of
Sortie-managed pull requests independently of who performs the merge, and transitions the
linked issue to a single configured terminal state exactly once. The reaction is off by
default: a deployment that omits the block behaves exactly as one running without it, and
enabling it grants the tracker credential write authority it did not need before.

Fields:

| Field              | Type    | Default       | Dynamic Reload    | Description                                                                                    |
| ------------------ | ------- | ------------- | ------------------ | ------------------------------------------------------------------------------------------------ |
| `provider`         | string  | _(required)_  | Requires restart  | SCM adapter kind. Must match the provider of every other active SCM reaction. Activates the kind. |
| `target_state`     | string  | _(required)_  | Requires restart  | The single terminal state the linked issue moves to once its pull request merges. Never inferred. |
| `poll_interval_ms` | integer | `60000`       | Requires restart  | Polling interval for the merge-observation state machine. Minimum `30000` (30 sec).               |
| `max_retries`      | integer | `2`           | Requires restart  | Retryable transition attempts before escalation. `0` escalates on the first failed attempt.       |
| `escalation`       | string  | `label`       | Requires restart  | Escalation action when retries are exhausted. One of `label` or `comment`.                        |
| `escalation_label` | string  | `needs-human` | Requires restart  | Label applied when `escalation` is `label`.                                                       |

**Tracker prerequisites:** two `tracker` fields must be set before this block activates, and
each is enforced offline. `tracker.handoff_state` must be non-empty: it is the state a merge
waits in, and pending-reaction recovery across a restart is disabled entirely without it.
`tracker.terminal_states` must be written out in front matter rather than left to the tracker
adapter's default list: the reconcile pass and the terminal workspace sweep both read the
list exactly as configured, with no fallback to an adapter default, so a defaulted list would
let the validator accept a `target_state` the runtime never treats as terminal.

**Activation:** The `reactions.merge_completion` block is active when `provider` is present
and non-empty, on its own, with no other `reactions` block required. Agent-created PRs MUST
write `pr_number` (positive integer), `owner`, and `repo` (all non-empty) to
`.sortie/scm.json` in the workspace for merge-completion polling to activate; unlike the
checkout-bearing kinds, no `branch` is required because the reaction performs no checkout.

**Target-state rule and irreversibility:** the target is never inferred from the terminal
list; the operator names it explicitly, and it is applied verbatim once the merge is
observed. The transition is not reversible by the orchestrator: naming an abandonment state
where a completion state was meant closes finished work under the wrong label, and no
validator can detect that mistake, because it is a judgement about the issue rather than a
configuration shape.

**Idempotency key:** the fingerprint is the merge commit identifier reported by the provider, not
the pull request number, persisted in the `reaction_fingerprints` SQLite table under a kind
distinct from every other reaction. A row is created on the first observed merge, retained (never
deleted) once the transition succeeds, and re-armed only when a later merge reports a different
commit identifier. A pull request may remain unmerged for any length of time without starting a
failure clock. When the provider first reports `Merged: true` with no commit identifier, Sortie
records that PR identity separately and waits thirty minutes, retrying with exponential pending
backoff floored at `poll_interval_ms`. If the identifier is still absent on the first poll at or
after the deadline, Sortie stops polling that pending entry without transitioning the issue and
attempts the configured label or comment. Delivery is recorded only after that tracker write
succeeds. A failure does not restart the stopped polling loop; a later fresh pending entry,
including one recovered after restart, can retry the undelivered notification. If the tracker write
succeeds but the follow-up marker write itself fails, the notification has already reached the
operator even though the observation stays recorded as undelivered; a later fresh pending entry
then delivers it a second time. `escalation: label` repeats harmlessly, because re-applying a
present label is a no-op; `escalation: comment` posts the operator a duplicate comment. If such a
later entry instead observes a real identifier, it follows the normal merge-commit fingerprint path
and clears the temporary observation after that latch completes. `max_retries` applies only to
failed tracker transitions, not to this grace period.

**Failure matrix:**

| Transition outcome            | Posture                                                     |
| ------------------------------ | ------------------------------------------------------------ |
| Transport or API failure       | Retry with backoff, bounded by `max_retries`, then escalate  |
| Authentication failure         | Escalate immediately, no retry                                |
| Payload failure (target state unreachable) | Escalate immediately, no retry                    |
| Issue not found                | Stop, mark the fingerprint dispatched, log a warning, no escalation |

**Restart-to-apply:** reaction configuration, including `target_state`, is captured once at
orchestrator construction and is not rebuilt on a workflow reload. A change to this block, or
to the two tracker prerequisites, takes effect only on the next restart; `sortie validate`
compares the same configuration a restart would build, so the offline verdict and the
construction verdict cannot diverge. Environment indirection through `$VAR` is not supported
for any field in this block, matching every other reaction kind.

**Request cost:** each parked issue costs one tracker issue-state read and one pull-request
read per poll interval while it is unmerged, plus one tracker write per observed merge. A merged
pull request missing its commit identifier backs off rather than issuing a forge read on every
fixed poll. On a forge tracker the tracker and the SCM adapter share one credential against one
host, so the steady-state unmerged cost approaches two requests per parked issue per poll
interval. A deployment with many simultaneously parked issues SHOULD raise `poll_interval_ms`
above the default rather than accept it.

**Cross-kind isolation:** CI-failure and review escalations are scoped to their own kind:
each clears only its own pending entry, attempt counter, and fingerprint, so merge-completion
tracking for that issue survives an unrelated escalation. Other reaction kinds on the same
issue are unaffected by a merge-completion transition or escalation, and vice versa.

**Configuration drift on a running process:** editing `tracker.terminal_states` while the
process runs does not update the `target_state` a running orchestrator already captured. When
the two disagree, the reaction logs one warning naming both values and keeps transitioning
issues to the frozen target; the terminal workspace sweep stops collecting the workspaces of
issues this reaction closes until the two lists agree again. A restart re-validates the edited
file offline and rejects the same drifted configuration before the process starts.

Example:

```yaml
reactions:
  merge_completion:
    provider: github
    target_state: done
    poll_interval_ms: 60000
    max_retries: 2
    escalation: label
    escalation_label: needs-human
```

**Validation rules:**

- Reaction kind keys must match `[a-z][a-z0-9_-]*`. Invalid keys are rejected with a
  configuration error.
- `max_retries` must be non-negative for all kinds.
- `escalation` must be `"label"` or `"comment"` for all kinds.
- `poll_interval_ms` must be >= `30000` for `review_comments`.
- `debounce_ms` must be non-negative for `review_comments`.
- `max_continuation_turns` must be positive for `review_comments`.
- `strategy` for `auto_merge` must be `"merge"`, `"squash"`, or `"rebase"`.
- `require_ci` and `delete_branch` for `auto_merge` must be boolean.
- `poll_interval_ms` must be >= `30000` for `auto_merge`.
- `poll_interval_ms` must be >= `30000` for `bot_review`.
- `max_continuation_turns` must be positive for `bot_review`.
- `bot_usernames` for `bot_review` must be a list of strings.
- `poll_interval_ms` must be >= `30000` for `merge_conflicts`.
- `max_retries` for `merge_conflicts` defaults to `1` rather than `2`; an explicit value overrides.
- `review_label` and `fix_label` for `label_commands` must be strings; each defaults to a non-empty value, and an explicit `""` disables that command.
- `poll_interval_ms` below `30000` for `label_commands` is clamped up to `30000` with a warning, not rejected.
- For `label_commands`, setting `provider` while both `review_label` and `fix_label` are `""` is a configuration error surfaced offline by `sortie validate`.
- `target_state` for `merge_completion` is required, must not equal `tracker.handoff_state`, must not be a member of `tracker.active_states` (falling back to the tracker adapter's default active states when that list is empty), and must be a member of `tracker.terminal_states` as written, with no fallback to the adapter's default terminal states.
- `tracker.handoff_state` must be set and `tracker.terminal_states` must be non-empty in front matter when `reactions.merge_completion.provider` is set; each is reported as a configuration error naming the tracker field.
- `poll_interval_ms` must be >= `30000` for `merge_completion`.
- When `provider` is absent or empty, all other fields in the kind sub-object are ignored.

**Where each rule is enforced:** the rules that the config layer owns (reaction key shape,
`max_retries`, `escalation`, `escalation_label`, and every `label_commands` rule) run during
typed config construction, so `sortie validate` reports them offline. For the kind-specific
rules, `sortie validate` runs the `review_comments`, `auto_merge`, `bot_review`,
`merge_conflicts`, and `merge_completion` builders. These are the same builders used when the
orchestrator constructs the reactions at startup, so both paths report the same invalid values.

---

### 2.11 `dispatch` — Rule-based Routing

```yaml
dispatch:
  rules: # ordered list; first-match-wins; optional
    - name: <rule-name> # optional; must match ^[a-z][a-z0-9_-]*$; used in logs
      match: # optional; absent or empty match block matches every issue (catch-all)
        labels: ["bug", "p0-*"]  # string or list; glob; OR within key
        issue_type: ["Bug"]      # string or list; case-insensitive equality
        priority: { lte: 2 }    # predicate object; exactly one of eq, in, lt, lte, gt, gte
        identifier: ["FE-*"]    # string or list; glob; matched against issue.identifier
        assignee: ["alice"]     # string or list; case-insensitive equality
      agent: <kind>    # optional; overrides the agent kind for matching issues
      template: ./prompts/bug.md # optional; path relative to the WORKFLOW.md directory
  default: # optional
    agent: <kind>     # optional; defaults to top-level agent.kind
    template: <path>  # optional; defaults to the WORKFLOW.md Markdown body
```

#### Dispatch rules

| Field | Type | Required | Default | Description |
| ----- | ---- | -------- | ------- | ----------- |
| `rules` | list of rule objects | No | _(none)_ | Ordered dispatch rules; first-match-wins. Evaluated in YAML order. |
| `default` | map | No | _(none)_ | Fallback selection when no rule matches. Keys: `agent`, `template`. |

Each rule in `dispatch.rules` is a map with the following keys (all other keys are unrecognized):

| Field | Type | Required | Default | Description |
| ----- | ---- | -------- | ------- | ----------- |
| `name` | string | No | _(absent)_ | Operator-supplied identifier used in logs. Must match `^[a-z][a-z0-9_-]*$` when present. |
| `match` | map | No | _(absent)_ | Predicate block. An absent or empty block matches every issue (catch-all). |
| `agent` | string | No | _(fallback)_ | Agent adapter kind for matching issues. Falls through to `dispatch.default.agent`, then to `agent.kind`. |
| `template` | string | No | _(fallback)_ | Prompt template path (relative to `WORKFLOW.md` directory). Falls through to `dispatch.default.template`, then to the Markdown body. |

The `match` block accepts only these keys:

| Field | Type | Matching | Description |
| ----- | ---- | -------- | ----------- |
| `labels` | string or list | Glob (any element) | Matches when the issue carries at least one label matching any pattern. Patterns use `path.Match` glob syntax (e.g., `bug/*`, `*-urgent`). Matched against the adapter-normalized lowercase label set. |
| `issue_type` | string or list | Case-insensitive equality (any element) | Matches when the issue type equals any list entry. |
| `priority` | predicate object | Numeric comparison | Single-operator predicate. See Priority predicates below. An issue with no priority never matches. |
| `identifier` | string or list | Glob (any element) | Matches when `issue.identifier` glob-matches any pattern, in the case the adapter produced. |
| `assignee` | string or list | Case-insensitive equality (any element) | Matches when the issue assignee equals any list entry. |

The `dispatch.default` block accepts only `agent` and `template`, with the same types and
fallback behavior as the per-rule fields.

#### Matching semantics

Evaluation applies AND logic across keys and OR logic within a single key:

- A `match` block succeeds only when every present key is satisfied.
- A key whose value is a list succeeds when any element matches.
- An absent or empty `match` block always succeeds (catch-all).

String-valued keys (`labels`, `issue_type`, `identifier`, `assignee`) accept either a
single string or a list of strings. A scalar is treated as a one-element list.

#### Priority predicates

The `priority` key takes a predicate object with exactly one operator key:

| Operator | Meaning |
| -------- | ------- |
| `eq` | Priority equals value |
| `in` | Priority is in the list of values |
| `lt` | Priority is less than value |
| `lte` | Priority is less than or equal to value |
| `gt` | Priority is greater than value |
| `gte` | Priority is greater than or equal to value |

For `in`, the value is a list of integers (e.g., `{ in: [1, 2] }`). For all other
operators, the value is a single integer. An issue with no priority never matches a
`priority` predicate.

#### Fallback resolution chain

Rules evaluate in YAML order. The first rule whose `match` block succeeds is selected;
later rules are not consulted. For each selected rule, `agent` and `template` may each be
omitted independently. Missing fields fall through in order:

1. The matched rule's `agent` / `template`.
2. `dispatch.default.agent` / `dispatch.default.template`.
3. Top-level `agent.kind` (for agent) or the `WORKFLOW.md` Markdown body (for template).
   See [Section 2.6](#26-agent--coding-agent-configuration) for the top-level `agent.kind`
   default.

When no rule matches and `dispatch.default` supplies an agent or template, the default is
applied with the same fallback chain for any unset field.

#### Freeze-on-dispatch

The resolved `(agent_kind, template_id, rule_name)` is recorded at the initial dispatch
and reused by retries and reaction-driven continuations for the same claim. Rules are
re-evaluated only after the claim is released.

A changed rule set from a `WORKFLOW.md` reload applies to future claims only. In-flight
issues keep their frozen selection.

#### Per-rule template paths

Template paths for rules and `dispatch.default` resolve relative to the directory
containing `WORKFLOW.md` (`filepath.Dir(workflow_path)`). The following paths are rejected
at load time:

- Absolute paths (starting with `/`).
- `~`-prefixed paths.
- Paths that resolve, after symlink evaluation, outside the workflow directory tree.

An empty `template` field falls through to the Markdown body (the same behavior as
omitting the field).

#### Example

```yaml
dispatch:
  rules:
    - name: bug-fix
      match:
        labels: ["bug", "bug/*"]
        priority: { lte: 2 }
      agent: claude-code
      template: ./prompts/bug.md
    - name: docs-update
      match:
        issue_type: ["Documentation", "Docs"]
      agent: codex
      template: ./prompts/docs.md
    - name: catch-all
      agent: claude-code
      template: ./prompts/default.md
  default:
    agent: claude-code
    template: ./prompts/default.md
```

The two named rules carry `match` blocks. The third rule has no `match` block and acts as
the terminal catch-all. Each rule carries a `template:` path that is relative to the
`WORKFLOW.md` directory and uses forward-slash separators.

#### Further reading (optional)

The design rationale is in [architecture §5.3.10](architecture/05-workflow-specification.md#5310-dispatch-object-optional) and
[ADR-0011](decisions/0011-dispatch-rule-configuration.md). Neither document is required
to write a valid `dispatch` block; they explain why the feature is shaped as it is.

---

### 2.12 `notifications` (operator notification backends)

```yaml
notifications: # ordered list of notifier backends; optional
  - kind: slack
    webhook_url: $SORTIE_SLACK_WEBHOOK_URL # SORTIE_-prefixed reference (mandatory)
    max_per_session: 20                     # optional; 0 selects the default (also 20)
  - kind: webhook
    url: $SORTIE_OPS_WEBHOOK_URL            # SORTIE_-prefixed reference (mandatory)
```

The `notifications` list configures the backends behind the `notify_operator` agent tool.
While a session runs, the agent calls `notify_operator` to escalate a decision, report
progress, or flag a blocker to a real-time channel. The tool is registered only when at
least one valid backend is configured; an empty or absent list leaves it unregistered, so
the agent is never offered a tool it cannot use.

The value is a sequence, not a single object. A second channel is a second list entry. Each
entry is a map carrying a required `kind` discriminator and that backend's own fields.

| Field | Type | Required | Default | Description |
| ----- | ---- | -------- | ------- | ----------- |
| `kind` | string | Yes | _(none)_ | Backend discriminator. v1 backends are `webhook` and `slack`. |
| `max_per_session` | int | No | `20` | Per-session notification cap. `0` selects the default (`20`); it never means unlimited. A negative value is rejected. |

Per-backend fields depend on `kind` and are passed through to the backend untyped:

| `kind` | Field | Description |
| ------ | ----- | ----------- |
| `webhook` | `url` | Endpoint that receives an HTTP POST of the notification as a JSON object using generic field names. |
| `slack` | `webhook_url` | Slack incoming webhook URL that receives a Slack-shaped JSON body with a `text` field. |

The `notifications` `webhook` backend is an outbound POST to an operator-supplied endpoint.
It is unrelated to inbound tracker webhooks ([architecture §20](architecture/25-webhook-support.md)), which trigger
reconciliation. The two share a name but not a direction.

When the list configures more than one backend, the effective per-session cap is the
maximum non-zero `max_per_session` across entries, falling back to the default when every
entry is `0` or unset. The cap counts `notify_operator` calls, not per-backend sends.

**`SORTIE_`-prefixed secret rule:**

A backend secret MUST be given as a reference to a `SORTIE_`-prefixed environment variable,
written as `$SORTIE_NAME` or `${SORTIE_NAME}`. The prefix is mandatory, not stylistic. The
`notify_operator` tool runs in a separate `sortie mcp-server` process that receives only the
workflow file path and the orchestrator's `SORTIE_`-prefixed variables. A reference without
the `SORTIE_` prefix, or to an unset variable, resolves to an empty string in that process.
The backend rejects an empty required secret, which surfaces as a fatal startup error rather
than a notification posted nowhere. Name every notification secret with the `SORTIE_` prefix.

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

| Environment variable               | Config field              | Type    | Notes                                 |
| ----------------------------------- | -------------------------- | ------- | -------------------------------------- |
| `SORTIE_WORKSPACE_ROOT`             | `workspace.root`           | string  | `~` expansion applies; `$VAR` skipped |
| `SORTIE_WORKSPACE_RETENTION_DAYS`   | `workspace.retention_days` | integer | No `~` expansion or `$VAR` handling   |

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
| `SORTIE_AGENT_MAX_TOKENS`            | `agent.max_tokens`            | int    |       |

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

Non-empty real environment variables win over `.env` file values. Empty real environment
variables are treated as unset and fall back to the `.env` value. The `.env` file provides
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
| `tracker.api_version`                  | No override variable exists; set it in the front matter or through `$VAR` indirection |
| `tracker.handoff_evidence`             | No override variable exists; set it in the front matter. The value is matched against the closed set as written, so `$VAR` indirection does not apply. |
| `notifications`                        | List of pass-through backend maps; no single-value representation. Backend secrets are referenced via `$SORTIE_*` indirection from inside the entry (see Section 2.12), not as field-level overrides. |
| `ci_feedback.*`                        | No override variables exist; the section is deprecated and rarely differs per environment |
| `reactions.*` (including `reactions.label_commands`) | No override variables exist; reaction configuration comes from WORKFLOW.md |
| `dispatch.*`                           | No override variables exist; rule definitions and template paths come from WORKFLOW.md |
| `self_review.*`                        | Verification commands are security-sensitive privileged configuration that must come from the version-controlled WORKFLOW.md |
| Extensions (`server`, `worker`, etc.)  | Extension-defined; would couple core env parsing to extensions  |
| `logging.level` (via extensions)       | Resolved from `--log-level` flag; not part of typed config layer |

### 3.7 Dynamic reload

On WORKFLOW.md reload, `applyEnvOverrides` re-reads both `os.Getenv` and the `.env` file.
Env overrides merge into the fresh raw map before section builders run.

- **`.env` file changes are picked up** on each reload. The `.env` file itself is not
  watched by fsnotify — only WORKFLOW.md changes trigger reload.
- **Real environment variable changes require a process restart.** `os.Getenv` reads the
  current process environment. While Go can observe in-process changes via `os.Setenv`,
  operator-provided env vars are inherited at process launch and are effectively immutable
  from outside the running process.

For configuration values that need to change without restarting, use the `.env` file and
trigger a WORKFLOW.md reload.

---

## 4. Extensions

The front matter is extensible. Unknown top-level keys are collected into an `Extensions`
map and are not validated by the core schema. Extensions should document their own field
schemas, defaults, and reload behavior.

### 4.1 HTTP Server (`server.port`, `server.host`)

```yaml
server:
  port: 9090
  host: "0.0.0.0"
```

| Field         | Type        | Required | Default     | Dynamic Reload            | Description                                                                     |
| ------------- | ----------- | -------- | ----------- | ------------------------- | ------------------------------------------------------------------------------- |
| `server.port` | integer     | No       | `7678`      | **No** — requires restart | TCP port for the embedded HTTP observability server. `0` disables the server.    |
| `server.host` | string (IP) | No       | `127.0.0.1` | **No** — requires restart | Bind address for the HTTP server. Must be a parseable IP address.               |

Sortie starts an HTTP server by default on `127.0.0.1:7678` for runtime observability
and operational control. `server.port` overrides the default port; `server.host`
overrides the default bind address. CLI `--port` and `--host` flags take precedence
over their extension counterparts. Port `0` disables the server entirely (no TCP
listener, no Prometheus metrics).

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
| `sortie_worker_exits_total`                     | Counter   | `exit_type`                 | Worker exits (`normal`, `error`, `cancelled`, `soft_stop`).    |
| `sortie_retries_total`                          | Counter   | `trigger`                   | Retry schedule events (`error`, `continuation`, `timer`, `stall`). |
| `sortie_reconciliation_actions_total`           | Counter   | `action`                    | Reconciliation outcomes (`stop`, `cleanup`, `keep`, `sweep_cleanup`, `sweep_expired`). |
| `sortie_poll_cycles_total`                      | Counter   | `result`                    | Poll tick completions (`success`, `error`, `skipped`).         |
| `sortie_tracker_requests_total`                 | Counter   | `operation`, `result`       | Tracker adapter API calls by operation and result.             |
| `sortie_handoff_transitions_total`              | Counter   | `result`                    | Handoff-state transition attempts (`success`, `error`, `skipped`, `withheld`). `skipped` covers two suppression causes: the issue had already reached a terminal state, or the issue was not in an active state at worker exit. `withheld` is the third: the run's handoff-evidence verdict did not permit the write, and the run is recorded as failed. Recorded only when `tracker.handoff_state` is set. |
| `sortie_issue_parks_total`                      | Counter   | `reason`                    | Issue park events (`agent_blocked`, `handoff_absence`).        |
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
  ssh_strict_host_key_checking: accept-new
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
| `worker.ssh_strict_host_key_checking`   | string           | No       | `accept-new`                   | OpenSSH `StrictHostKeyChecking` value: `accept-new`, `yes`, or `no`.                       |

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
  The `StrictHostKeyChecking` value is configurable via
  `worker.ssh_strict_host_key_checking`. Set to `yes` when `known_hosts` is
  pre-populated by configuration management; set to `no` only in isolated
  test environments. Invalid values fall back to `accept-new` with a warning.
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
  ssh_strict_host_key_checking: accept-new

hooks:
  after_create: |
    ssh "$SORTIE_SSH_HOST" "mkdir -p \"$SORTIE_WORKSPACE\" && git clone https://repo.example.com/project.git \"$SORTIE_WORKSPACE\""
  before_remove: |
    ssh "$SORTIE_SSH_HOST" "rm -rf \"$SORTIE_WORKSPACE\""
---
You are a software engineer. Fix the issue described below.
{{.issue_body}}
```

### 4.4 `token_rates` — Cost estimation

```yaml
token_rates:
  claude-code:
    input_per_mtok: 3.00
    output_per_mtok: 15.00
    cache_read_per_mtok: 0.30
  copilot-cli:
    input_per_mtok: 2.00
    output_per_mtok: 8.00
    cache_read_per_mtok: 0.20
  codex:
    input_per_mtok: 2.50
    output_per_mtok: 10.00
    cache_read_per_mtok: 0.25
```

When `token_rates` is configured, the dashboard displays estimated USD cost for
currently running sessions, and the `sortie stats` subcommand prices the runs it
aggregates from run history. Keys are agent adapter kind strings (e.g., `"claude-code"`,
`"copilot-cli"`, `"codex"`, `"opencode"`). All rates are in USD per 1 million tokens.

When `token_rates` is absent or empty, the dashboard shows raw token counts without
cost estimates and `sortie stats` reports no cost figures.

The `kiro` adapter reports no token counts on the headless path, so a `token_rates.kiro`
entry has no effect. Cost is surfaced only through the abstract credits figure in the
`kiro-cli` stderr trailer, which the orchestrator does not aggregate.

| Field | Type | Default | Description |
| --- | --- | --- | --- |
| `token_rates` | map | _(absent)_ | Top-level extension key. Keys are agent adapter kind strings. |
| `token_rates.<kind>.input_per_mtok` | number | _(not set)_ | USD per million input tokens. |
| `token_rates.<kind>.output_per_mtok` | number | _(not set)_ | USD per million output tokens. |
| `token_rates.<kind>.cache_read_per_mtok` | number | _(not set)_ | USD per million cache-read tokens. |

**Validation rules:**

- `token_rates` MUST be a map when present. Non-map values produce a warning (not a
  fatal error).
- Each `<kind>` value MUST be a map. Non-map values produce a warning for that kind.
- Rate values MUST be non-negative numbers. Negative values produce a warning and are
  treated as not configured.
- Missing rate fields within a kind are valid. Partial rates (e.g., only
  `output_per_mtok`) compute cost from the configured fields only.
- Zero-valued rates are valid and produce `$0.00` for that token type.

**Reload behavior:** Token rates do not reload dynamically. Changes require a process
restart, consistent with `server.port` and `server.host`.

### 4.5 Adapter-Specific Pass-Through Config

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
  fallback_model: claude-haiku-4-5
  max_turns: 50
  max_budget_usd: 5
  effort: high
  allowed_tools: "Read Edit Bash(git diff *)"
  disallowed_tools: WebFetch
  system_prompt: Prefer table-driven tests.
  mcp_config: ./mcp-servers.json
  session_persistence: true
```

The `claude-code` block is forwarded to the Claude Code adapter, which runs
`claude -p --output-format stream-json --verbose` once per turn and maps these fields to
CLI flags. Values are forwarded unchanged, and the adapter validates none of them. What
the CLI does with an invalid value differs per flag: `--permission-mode` is rejected at
launch, `--effort` falls back to the default effort with a warning, and an unknown model
name reaches the API and fails there. A key whose YAML value has the wrong type is
ignored and the default applies.

| Field | Type | Default | Description |
| --- | --- | --- | --- |
| `claude-code.permission_mode` | string | _(absent)_ | Forwarded to `--permission-mode`. Values: `acceptEdits`, `auto`, `bypassPermissions`, `default`, `dontAsk`, `manual` (an alias for `default`), and `plan`. When absent, the adapter passes `--dangerously-skip-permissions` instead. |
| `claude-code.model` | string | _(absent)_ | Forwarded to `--model`. Accepts a model alias such as `sonnet`, or a full model name. |
| `claude-code.fallback_model` | string | _(absent)_ | Forwarded to `--fallback-model`. Accepts one model or a comma-separated chain. Covers model availability only; see the fallback note below. |
| `claude-code.max_turns` | integer | _(absent)_ | Forwarded to `--max-turns` when greater than zero. Claude Code's own agentic turn budget within one invocation. |
| `claude-code.max_budget_usd` | number | _(absent)_ | Forwarded to `--max-budget-usd` when greater than zero. Spend cap for one invocation. The counter restarts on every turn, including resumed ones, so the ceiling for a whole session is this value times `agent.max_turns`. |
| `claude-code.effort` | string | _(absent)_ | Forwarded to `--effort`. Values: `low`, `medium`, `high`, `xhigh`, `max`, and `ultracode`, which starts the session at `xhigh` with ultracode enabled. The accepted set depends on the model. |
| `claude-code.allowed_tools` | string | _(absent)_ | Forwarded to `--allowedTools` as a single argument. A comma- or space-separated list of tools that run without a permission prompt, including scoped rules such as `Bash(git diff *)`. |
| `claude-code.disallowed_tools` | string | _(absent)_ | Forwarded to `--disallowedTools` as a single argument. A comma- or space-separated deny list. A bare tool name removes that tool from the model's context; a scoped rule leaves the tool available and denies only matching calls. |
| `claude-code.system_prompt` | string | _(absent)_ | Forwarded to `--append-system-prompt`. The text is appended to the default system prompt rather than replacing it. Claude Code's built-in tool instructions stay in effect. |
| `claude-code.mcp_config` | string | _(absent)_ | Path to an MCP server configuration JSON file, resolved relative to the directory holding WORKFLOW.md when it is not absolute. The worker reads that file, merges its own `sortie-tools` server into a generated copy under the workspace, and passes the copy to `--mcp-config`, which takes a single configuration path. The operator's file is never modified. A file that already declares a `sortie-tools` server fails the attempt. |
| `claude-code.session_persistence` | boolean | `true` | When `false`, adds `--no-session-persistence` and Claude Code writes no session file to disk. The adapter continues a session by passing `--resume <session_id>` on every turn after the first, and that flag reads the persisted session, so with persistence off every turn after the first exits non-zero with `No conversation found with session ID`. |

> **Important:** `agent.max_turns` (orchestrator turn-loop limit) and
> `claude-code.max_turns` (CLI `--max-turns` flag) are distinct values. The orchestrator
> limit controls how many turns the worker runs before exiting. The adapter limit controls
> the Claude Code CLI's internal turn budget. They serve different purposes and should
> typically have different values.

> **Important:** `claude-code.fallback_model` covers model availability, not provider
> exhaustion. Claude Code switches to the fallback when the primary model is overloaded,
> unavailable (a retired model, for example), or returns another non-retryable server
> error. Authentication, billing, rate-limit, request-size, and transport errors never
> trigger a switch; they follow their normal retry and error handling. The switch lasts
> for the current turn only; the next turn starts on the primary model. Claude Code caps
> a chain at three models after removing duplicates and ignores the rest. The adapter
> forwards the configured string unchanged.

**Codex adapter:**

```yaml
codex:
  model: o3                       # Model override (e.g., "gpt-5.4", "o3")
  effort: medium                  # Reasoning effort: "low", "medium", "high"
  approval_policy: never          # "never" (default), "onRequest", "unlessTrusted", "always"
  thread_sandbox: workspaceWrite  # "workspaceWrite" (default), "readOnly", "dangerFullAccess", "externalSandbox"
  personality: concise            # Personality preset
  skip_git_repo_check: false      # Skip git repo validation for non-git workspaces
  turn_sandbox_policy:            # Per-turn sandbox policy override (optional)
    networkAccess: true
```

The `codex` block is forwarded to the Codex adapter, which uses these fields
when initializing the `codex app-server` subprocess and starting threads.

The Codex adapter uses a persistent subprocess model: the app-server is launched
once in `StartSession` and kept alive across turns, unlike the per-turn subprocess
model used by `claude-code`, `copilot-cli`, and `opencode`.

> **Sandbox defaults:** When `thread_sandbox` is omitted, the adapter defaults to
> `workspaceWrite` with `writableRoots` set to the workspace path and
> `networkAccess: false`. Use `turn_sandbox_policy` to override specific sandbox
> fields per turn.

**OpenCode adapter:**

```yaml
opencode:
  model: anthropic/claude-sonnet-4-5
  agent: build
  variant: high
  thinking: true
  pure: false
  dangerously_skip_permissions: true
  disable_autocompact: true
  allowed_tools:
    - read
    - glob
  denied_tools:
    - bash
```

The `opencode` block is forwarded to the OpenCode adapter. The adapter runs
`opencode run --format json --dir <workspace>` once per turn, appends
`--session <session_id>` when continuing a session, and recovers final token
usage with `opencode export --sanitize <session_id>` when the session ID is
known.

| Field | Type | Default | Description |
| --- | --- | --- | --- |
| `opencode.model` | string | _(absent)_ | Value forwarded to `opencode run --model`. OpenCode interprets provider and model selection from this string. |
| `opencode.agent` | string | _(absent)_ | Value forwarded to `opencode run --agent`. Selects the OpenCode agent profile for the turn. |
| `opencode.variant` | string | _(absent)_ | Value forwarded to `opencode run --variant`. Selects an OpenCode provider-specific variant. |
| `opencode.thinking` | boolean | `false` | Adds `--thinking` to the run command. |
| `opencode.pure` | boolean | `false` | Adds `--pure` to the run command. |
| `opencode.dangerously_skip_permissions` | boolean | `true` | Adds `--dangerously-skip-permissions` when `true`. When `false`, OpenCode headless permission prompts can surface as tool errors instead of auto-approved tool execution. |
| `opencode.disable_autocompact` | boolean | `true` | Sets `OPENCODE_DISABLE_AUTOCOMPACT`. The adapter always also sets `OPENCODE_AUTO_SHARE=false`, `OPENCODE_DISABLE_AUTOUPDATE=true`, and `OPENCODE_DISABLE_LSP_DOWNLOAD=true`. |
| `opencode.allowed_tools` | list of strings | `[]` | Builds `OPENCODE_PERMISSION` allow rules. Listed keys become `allow`; known OpenCode permission keys not listed become `deny`. Unknown keys are forwarded unchanged. |
| `opencode.denied_tools` | list of strings | `[]` | Builds explicit `deny` rules in `OPENCODE_PERMISSION`. |

**Validation rules:**

- `opencode.allowed_tools` and `opencode.denied_tools` MUST NOT overlap.
- The adapter always removes any inherited `OPENCODE_PERMISSION` value before
  launching OpenCode. If either tool list is non-empty, it replaces that value
  with the adapter-managed JSON policy.

**Kiro adapter:**

```yaml
agent:
  kind: kiro
  command: kiro-cli
  turn_timeout_ms: 3600000

kiro:
  model: claude-sonnet-4.6
  trust_tools:
    - read
    - grep
    - glob
  agent: my-context-profile
```

The `kiro` block is forwarded to the Kiro adapter, which runs
`kiro-cli chat --no-interactive --wrap never` once per turn and adds `--resume`
on continuation turns to attach to the cwd-scoped conversation. The Kiro CLI
is the rebranded Amazon Q Developer CLI; the binary is `kiro-cli`.

| Field | Type | Default | Description |
| --- | --- | --- | --- |
| `kiro.model` | string | _(absent)_ | Forwarded to `kiro-cli chat --model`. The `/model` slash command is unavailable headless, so the model MUST be pinned per turn (here or via `kiro-cli settings chat.defaultModel`). Use `kiro-cli chat --list-models --format json` to enumerate the account-specific model set. |
| `kiro.trust_all_tools` | boolean | `false` | Adds `--trust-all-tools`, auto-approving every tool call. Use only inside a hardened sandbox. Mutually exclusive with `kiro.trust_tools`. |
| `kiro.trust_tools` | list of strings | `[]` | Adds `--trust-tools=<names>` with the comma-joined list. An empty list trusts nothing (the adapter still passes `--trust-tools=`, which is the explicit no-trust mode). Mutually exclusive with `kiro.trust_all_tools`. Tool names include `read`, `write`, `glob`, `grep`, `shell`, `aws`, `web_search`, `web_fetch`, `code`, and `report`. |
| `kiro.agent` | string | _(absent)_ | Forwarded to `kiro-cli chat --agent` to select a named Kiro context profile (custom agent). |

**Validation rules:**

- `kiro.trust_all_tools: true` and a non-empty `kiro.trust_tools` list MUST NOT
  be set together. The adapter rejects this combination at construction time.

**Environment variables consumed by the adapter:**

| Variable | Required | Description |
| --- | --- | --- |
| `KIRO_API_KEY` | Yes (local mode) | Headless credential. Requires a Kiro Pro, Pro+, or Power subscription. The adapter rejects a missing key in `StartSession` and runs a `kiro-cli whoami` canary to reject a present-but-invalid key, because headless `chat` with no credential blocks on an interactive device-login flow with no self-timeout. In SSH mode the orchestrator forwards `KIRO_API_KEY` from its environment into the remote command. |

**Token usage and budgets:** The Kiro adapter emits no token-usage events.
`kiro-cli` does not report token counts on the headless path (only an abstract
credits figure on stderr), so `TurnResult.Usage` is the zero value and
`token_rates.kiro` produces no cost estimate. Token-based budget enforcement
does not apply to Kiro; the `agent.turn_timeout_ms` is the only effective
backstop and is mandatory because `kiro-cli` has no native per-turn timeout.

**MCP:** Under `KIRO_API_KEY` authentication the backend `GetProfile` gate
disables MCP. A workspace `mcp.json` is not loaded and `--require-mcp-startup`
is unreachable, so MCP-dependent workflows cannot run on the API-key path.

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

The data map passed to `Execute` contains **three core top-level keys** (`issue`, `attempt`,
`run`) plus **continuation context keys** (`ci_failure`, `review_comments`, `bot_review_comments`,
`merge_conflict`, `label_review`, `label_fix`) that are `nil` by default and populated on
reaction-triggered dispatches:

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

#### `ci_failure` — CI Failure Context (continuation key)

Non-nil only on turn 1 of a CI-fix continuation dispatch. Contains CI failure details
from `CIResult.ToTemplateMap()`:

| Field                     | Type            | Description                                              |
| ------------------------- | --------------- | -------------------------------------------------------- |
| `.ci_failure.status`      | string          | Aggregate CI pipeline status (`"failing"`).              |
| `.ci_failure.check_runs`  | list of maps    | Individual check runs with `name`, `status`, `conclusion`, `details_url`. |
| `.ci_failure.log_excerpt` | string          | Truncated log from the first failing check.              |
| `.ci_failure.failing_count` | integer       | Number of failing check runs.                            |
| `.ci_failure.ref`         | string          | The git ref that was queried.                            |

When `nil` (default on non-CI dispatches), `{{ if .ci_failure }}` evaluates to `false`.

#### `review_comments` — Review Comment Context (continuation key)

Non-nil only on turn 1 of a review-fix continuation dispatch. Contains a list of human
review comments from `CHANGES_REQUESTED` PR reviews:

| Field (per element)           | Type    | Description                                              |
| ----------------------------- | ------- | -------------------------------------------------------- |
| `.review_comments[].id`       | string  | SCM-platform comment identifier.                         |
| `.review_comments[].file`     | string  | File path (empty for PR-level comments).                 |
| `.review_comments[].start_line` | integer | First line of commented range (0 for non-inline).       |
| `.review_comments[].end_line` | integer | Last line of commented range (0 for single-line or non-inline). |
| `.review_comments[].reviewer` | string  | Username of the comment author.                          |
| `.review_comments[].body`     | string  | Comment text.                                            |

When `nil` (default on non-review dispatches), `{{ if .review_comments }}` evaluates to
`false`.

**Template pattern for review comments:**

```
{{ if .review_comments }}
## Review Comments to Address

The following review comments were left on the PR. Address each one:

{{ range .review_comments }}
### {{ .reviewer }} on {{ .file }}{{ if .start_line }} (line {{ .start_line }}{{ if .end_line }}-{{ .end_line }}{{ end }}){{ end }}

{{ .body }}

{{ end }}
{{ end }}
```

#### `bot_review_comments` — Bot Review Comment Context (continuation key)

Non-nil only on turn 1 of a bot-review-fix continuation dispatch. Contains a list of
comments authored by automated review bots (see `reactions.bot_review` in Section 2.10).
The per-element shape is identical to `review_comments`:

| Field (per element)               | Type    | Description                                              |
| --------------------------------- | ------- | -------------------------------------------------------- |
| `.bot_review_comments[].id`       | string  | SCM-platform comment identifier.                         |
| `.bot_review_comments[].file`     | string  | File path (empty for PR-level comments).                 |
| `.bot_review_comments[].start_line` | integer | First line of commented range (0 for non-inline).       |
| `.bot_review_comments[].end_line` | integer | Last line of commented range (0 for single-line or non-inline). |
| `.bot_review_comments[].reviewer` | string  | Login of the bot that authored the comment.             |
| `.bot_review_comments[].body`     | string  | Comment text.                                            |

When `nil` (default on non-bot-review dispatches), `{{ if .bot_review_comments }}`
evaluates to `false`.

**Template pattern for bot review comments:**

```
{{ if .bot_review_comments }}
## Bot Review Comments to Address

The following comments were left on the PR by automated review tools. Address each one:

{{ range .bot_review_comments }}
### {{ .reviewer }} on {{ .file }}{{ if .start_line }} (line {{ .start_line }}{{ if .end_line }}-{{ .end_line }}{{ end }}){{ end }}

{{ .body }}

{{ end }}
{{ end }}
```

#### `merge_conflict` — Merge Conflict Context (continuation key)

Non-nil only on turn 1 of a merge-conflict-resolution continuation dispatch. Contains the
PR identity and the rebase target read live from the PR object (see
`reactions.merge_conflicts` in Section 2.10):

| Field                      | Type    | Description                                                                 |
| -------------------------- | ------- | --------------------------------------------------------------------------- |
| `.merge_conflict.pr_number` | integer | Pull request number.                                                        |
| `.merge_conflict.branch`   | string  | PR head branch the agent rebases.                                           |
| `.merge_conflict.head_sha` | string  | Latest commit SHA on the PR head branch.                                    |
| `.merge_conflict.base`     | string  | PR's real target (base) branch, read live from the PR object. The rebase target. |

The `base` value is the PR's actual base ref, not an assumed default branch, so the agent
rebases onto the correct target for PRs that target a release branch, a GitFlow `develop`,
or a stacked-PR parent. The orchestrator defers the continuation while the base ref is
unavailable, so `base` is always a populated branch name when this context is emitted.

When `nil` (default on non-merge-conflict dispatches), `{{ if .merge_conflict }}` evaluates
to `false`.

**Template pattern for merge conflicts:**

```
{{ if .merge_conflict }}
## Resolve Merge Conflicts

PR #{{ .merge_conflict.pr_number }} ({{ .merge_conflict.branch }}) has merge conflicts with
its base branch {{ .merge_conflict.base }}. Resolve them:

1. Fetch the latest {{ .merge_conflict.base }}.
2. Rebase {{ .merge_conflict.branch }} onto {{ .merge_conflict.base }}.
3. Resolve every conflict, keeping both the intent of the PR and the base changes.
4. Push the rebased branch.
{{ end }}
```

#### `label_review` — Label Review Context (continuation key)

Non-nil only on turn 1 of a read-only label-review dispatch, triggered when an operator
applies the configured review label to a Sortie-managed PR (see `reactions.label_commands`
in Section 2.10). Carries the PR coordinates the agent needs to fetch the diff and post its
review:

| Field                        | Type    | Description                                         |
| ---------------------------- | ------- | --------------------------------------------------- |
| `.label_review.pr_number`    | integer | Pull request number to review.                      |
| `.label_review.owner`        | string  | Repository owner.                                   |
| `.label_review.repo`         | string  | Repository name.                                    |
| `.label_review.actor`        | string  | Login of the operator who applied the review label. |
| `.label_review.requested_at` | string  | RFC 3339 timestamp of the confirmed labeling gesture. |

The orchestrator injects only these coordinates. It never injects the PR diff text and never
posts a comment itself; the agent fetches the diff and posts review comments using its own
SCM tooling. A prompt template that omits the `{{ if .label_review }}` branch therefore
produces no review on a label-review dispatch.

When `nil` (default on non-label-review dispatches), `{{ if .label_review }}` evaluates to
`false`.

**Template pattern for label review:**

```
{{ if .label_review }}
## Review This Pull Request

Produce a code review of pull request #{{ .label_review.pr_number }} in
{{ .label_review.owner }}/{{ .label_review.repo }}, requested by {{ .label_review.actor }}.

1. Fetch the PR diff using your SCM tooling.
2. Review the changes for correctness, clarity, and regressions.
3. Post your review comments on the PR. Do not modify the branch.
{{ end }}
```

#### `label_fix`: Label Fix Context (continuation key)

Non-nil only on turn 1 of a fix dispatch, triggered when an operator applies the configured
fix label to a Sortie-managed PR (see `reactions.label_commands` in Section 2.10). Carries
the PR coordinates the agent needs to check out the head branch, address the review
comments, and push:

| Field                     | Type    | Description                                           |
| ------------------------- | ------- | ------------------------------------------------------ |
| `.label_fix.pr_number`    | integer | Pull request number to fix.                             |
| `.label_fix.owner`        | string  | Repository owner.                                       |
| `.label_fix.repo`         | string  | Repository name.                                        |
| `.label_fix.branch`       | string  | PR head branch to check out and push to.                |
| `.label_fix.actor`        | string  | Login of the operator who applied the fix label.        |
| `.label_fix.requested_at` | string  | RFC 3339 timestamp of the confirmed labeling gesture.    |

The orchestrator injects only these coordinates. It never fetches the review comments,
never applies changes, and never pushes or comments itself; the agent checks out
`label_fix.branch`, addresses the comments, pushes the fixes, and posts the summary comment
using its own SCM tooling. A prompt template that omits the `{{ if .label_fix }}` branch
therefore runs the normal work prompt against a real checkout with push capability instead
of producing a fix.

When `nil` (default on non-label-fix dispatches), `{{ if .label_fix }}` evaluates to
`false`.

**Template pattern for label fix:**

```
{{ if .label_fix }}
## Fix This Pull Request

Check out {{ .label_fix.branch }} for pull request #{{ .label_fix.pr_number }} in
{{ .label_fix.owner }}/{{ .label_fix.repo }}, requested by {{ .label_fix.actor }}.

1. Fetch the outstanding review comments using your SCM tooling.
2. Address the feedback and push the fixes to {{ .label_fix.branch }}.
3. Post a summary comment on the PR.
4. Write `needs-human-review` to `.sortie/status` to signal completion.
{{ end }}
```

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

| Variable                  | Description                                           |
| ------------------------- | ----------------------------------------------------- |
| `SORTIE_ISSUE_ID`         | Stable tracker-internal issue ID.                     |
| `SORTIE_ISSUE_IDENTIFIER` | Human-readable ticket key (e.g., `PROJ-123`).         |
| `SORTIE_WORKSPACE`        | Absolute path to the per-issue workspace directory.   |
| `SORTIE_ATTEMPT`          | Current attempt number (integer).                     |

`after_run` hooks also receive the following environment variables:

| Variable                          | Description                                                                                                    |
| --------------------------------- | -------------------------------------------------------------------------------------------------------------- |
| `SORTIE_SELF_REVIEW_STATUS`       | Self-review outcome for the current run: `"disabled"`, `"passed"`, `"cap_reached"`, or `"error"`.            |
| `SORTIE_SELF_REVIEW_SUMMARY_PATH` | Absolute path to `.sortie/review_summary.md` in the workspace. Absent when self-review did not run or the summary file was not written. |

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

> **Note:** `after_create` runs only when Sortie first creates the per-issue
> workspace directory, so the directory is empty when the clone runs. If
> `after_create` fails, Sortie removes the directory before the next retry, so
> a retry also starts from an empty directory. A clone error such as
> "destination path already exists" or "directory not empty" does not come from
> this example on the normal path.
>
> Hooks also run with the restricted environment described in Section 6.2: an
> allowlist (including `HOME` and `SSH_AUTH_SOCK`) plus `SORTIE_*` variables.
> Sortie strips any variable outside that set, such as `GIT_SSH_COMMAND`, so an
> SSH clone must reach its key through the SSH agent (`SSH_AUTH_SOCK`) or
> through `~/.ssh` via `HOME`, not through a stripped variable.

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
| `tracker.handoff_evidence`             | Future worker attempts, not in-flight sessions.                                                |
| `polling.interval_ms`                  | **Immediate** — affects future tick scheduling.                                                |
| `workspace.root`                       | Future workspace operations.                                                                   |
| `workspace.retention_days`             | Dynamic reload. Applies on the next sweep pass.                                                |
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
| `agent.max_tokens`                     | **Immediate** — affects future retry timer evaluations.                                        |
| `db_path`                              | **No effect** — requires restart. In-memory config updated, but database connection unchanged. |
| `ci_feedback.kind`                     | **No effect** — requires restart. CI provider is created once at process start.                |
| `ci_feedback.max_retries`              | Future dispatches.                                                                             |
| `ci_feedback.max_log_lines`            | **No effect** — requires restart. CI provider is created once at process start.                |
| `ci_feedback.escalation`               | Future dispatches.                                                                             |
| `ci_feedback.escalation_label`                  | Future dispatches.                                                                             |
| `reactions.<kind>.provider`                     | **No effect** — requires restart. Adapters are created once at process start.                  |
| `reactions.ci_failure.max_retries`              | Future dispatches.                                                                             |
| `reactions.ci_failure.escalation`               | Future dispatches.                                                                             |
| `reactions.ci_failure.escalation_label`         | Future dispatches.                                                                             |
| `reactions.ci_failure.max_log_lines`            | **No effect.** Requires restart. CI provider is created once at process start.                 |
| `reactions.review_comments.*`                   | **No effect.** Requires restart. The reaction config is built once at construction.            |
| `reactions.auto_merge.*`                        | **No effect.** Requires restart. The reaction config is built once at construction.            |
| `reactions.bot_review.*`                        | **No effect.** Requires restart. The reaction config is built once at construction.            |
| `reactions.merge_conflicts.*`                   | **No effect.** Requires restart. The reaction config is built once at construction.            |
| `reactions.label_commands.*`                    | **No effect.** Requires restart. The reaction config is built once at construction.            |
| `reactions.merge_completion.*`                  | **No effect.** Requires restart. The reaction config, `target_state` included, is built once at construction. |
| `notifications`                        | Future sessions. The `sortie mcp-server` sidecar re-reads `WORKFLOW.md` at each session start, so backend and cap changes apply to sessions started after the reload, not to in-flight sessions. |
| `server.port`                          | **No effect** — requires restart.                                                              |
| `server.host`                          | **No effect** — requires restart.                                                              |
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
| `workspace.root` writable                      | The resolved root cannot be created, or a probe file cannot be written inside it. Reported under check `workspace.root_writable`. |
| `dispatch` is a map; `dispatch.rules` is a sequence; `dispatch.default` is a map | Wrong YAML node type for `dispatch`, `dispatch.rules`, or `dispatch.default`. |
| Each rule has at least one of `match`, `agent`, `template` | A rule map carries none of the three. |
| Rule `name`, when present, matches `^[a-z][a-z0-9_-]*$` | Malformed rule name. |
| No duplicate rule name | Two rules share the same non-empty `name`. |
| No non-final catch-all (`unreachable_rules`) | A rule with no `match` block precedes another rule. |
| Every `match` key recognized | A `match` key is not one of `labels`, `issue_type`, `priority`, `identifier`, `assignee`. |
| `priority` predicate has exactly one operator | Zero or more than one of `eq`, `in`, `lt`, `lte`, `gt`, `gte`. |
| Glob patterns syntactically valid | A `labels` or `identifier` pattern fails `path.Match`. |
| Every referenced `agent` kind registered | `dispatch.rules[*].agent` or `dispatch.default.agent` names an unregistered adapter. |
| Every per-rule template path resolvable and parseable | Path is absolute, `~`-prefixed, escapes the workflow tree, is not a regular file, is unreadable, or fails template parse. |
| `tracker.handoff_state` and `tracker.in_progress_state` free of collisions against the effective state lists | The state collides with the effective `active_states` or `terminal_states`, where an empty workflow list takes the tracker adapter's own fallback list. |

**Advisory warnings vs. configuration errors:** An unknown key placed directly
under `dispatch` (alongside `rules` and `default`) produces an `unknown_sub_key`
advisory warning and does not block startup. Unknown keys nested deeper are
rejected as configuration errors that fail the load: an unrecognized key inside a
rule map (`dispatch.rules[*]`), inside `dispatch.default`, or inside a `match`
block. The asymmetry matters: a typo like `lables:` inside `match`, or a stray key
on a rule, is caught as an error so it cannot silently disable a rule, while a typo
at the top `dispatch` level is flagged as a warning without preventing startup.

**Adapter-specific tracker diagnostics.** A tracker adapter may contribute its own offline
checks, which run during the same preflight without any network call. An `error`-severity
diagnostic blocks dispatch like any other preflight error; a `warning`-severity diagnostic is
advisory and does not block startup. The Linear adapter (`kind: linear`) emits:

| Check                                            | Severity | Message                                                                                                                                            |
| ------------------------------------------------ | -------- | -------------------------------------------------------------------------------------------------------------------------------------------------- |
| `tracker.project.format`                         | error    | `tracker.project must be a Linear team key with no whitespace (e.g. "ENG")`                                                                         |
| `tracker.project.format`                         | warning  | `tracker.project is a Linear team key (e.g. "ENG"), not an owner/repo path; the "/" looks like a GitHub-style value`                                |
| `tracker.api_key.sortie_linear_api_key_hint`     | warning  | `tracker.api_key is empty but SORTIE_LINEAR_API_KEY environment variable is set; consider using api_key: $SORTIE_LINEAR_API_KEY`                    |
| `tracker.api_key.sortie_linear_api_key_missing`  | warning  | `tracker.api_key is empty and SORTIE_LINEAR_API_KEY environment variable is not set`                                                                |
| `tracker.api_key.linear_whitespace`              | warning  | `tracker.api_key has leading or trailing whitespace; the key is sent verbatim in the Authorization header, so surrounding whitespace will fail authentication` |
| `tracker.api_key.linear_prefix`                  | warning  | `tracker.api_key does not start with "lin_api_"; Linear personal API keys carry that prefix`                                                        |
| `tracker.active_states.empty_element` / `tracker.terminal_states.empty_element` | error | `tracker.active_states[<i>]: empty state name can never match a team state` (same shape for `terminal_states`)               |
| `tracker.active_states.untrimmed_element` / `tracker.terminal_states.untrimmed_element` | error | `tracker.active_states[<i>]: state name has leading or trailing whitespace and can never match a team state` (same shape for `terminal_states`) |
| `tracker.states.overlap`                         | warning  | `tracker.active_states and tracker.terminal_states overlap on "<name>"; an issue in state "<name>" would match both sets`                           |

A `handoff_state` or `in_progress_state` that collides with `active_states` or
`terminal_states` is not an adapter diagnostic. The generic config validation rejects it for
every `tracker.kind` before the adapter hook runs, reporting it as a `config.tracker.*` error.
The config loader rules on the state lists as written; the dispatch preflight then runs the same
rules against the effective lists and reports a collision that only an adapter fallback exposes
under check `tracker.handoff_state` or `tracker.in_progress_state`, without the `config.` prefix.

These offline checks never contact Linear and never log the API key value. State-name existence
against the team and credential validity are checked by the online preflight at adapter
construction, not by `sortie validate`.

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
| `config: agent.max_tokens: must be non-negative`                                | Negative value for `max_tokens`.                                         | Use `0` (unlimited) or a positive integer.                                                                                           |
| `config: tracker.handoff_state: expected string, got <type>`                    | `handoff_state` is not a string (e.g., integer, boolean, list).          | Ensure the value is a string, quoted if necessary.                                                                                   |
| `config: tracker.handoff_state: must not be empty`                              | `handoff_state` is set to an explicit empty string.                      | Provide a valid state name, or omit the field entirely to disable handoff.                                                           |
| `config: tracker.handoff_state: resolved to empty (check environment variable)` | `$VAR` reference resolved to an empty string (variable unset or empty).  | Set the referenced environment variable to a valid state name.                                                                       |
| `config: tracker.handoff_state: "<val>" collides with active state "<state>"`   | `handoff_state` matches one of the `active_states` (case-insensitive).   | Use a state that is not in `active_states`. The handoff state must be distinct from active and terminal states.                      |
| `config: tracker.handoff_state: "<val>" collides with terminal state "<state>"` | `handoff_state` matches one of the `terminal_states` (case-insensitive). | Use a state that is not in `terminal_states`.                                                                                        |
| `config: tracker.handoff_evidence: expected string, got <type>`                 | `handoff_evidence` is not a string (e.g., integer, boolean, list).       | Ensure the value is a string, quoted if necessary.                                                                                   |
| `config: tracker.handoff_evidence: must be one of observed, strict, or off`     | `handoff_evidence` is set to a value outside the closed set.             | Use `observed`, `strict`, or `off`. Omit the field to keep the default `observed`.                                                   |
| `config: tracker.in_progress_state: expected string, got <type>`                    | `in_progress_state` is not a string (e.g., integer, boolean, list).          | Ensure the value is a string, quoted if necessary.                                                                                   |
| `config: tracker.in_progress_state: must not be empty`                              | `in_progress_state` is set to an explicit empty string.                      | Provide a valid state name, or omit the field entirely to disable dispatch-time transitions.                                         |
| `config: tracker.in_progress_state: resolved to empty (check environment variable)` | `$VAR` reference resolved to an empty string (variable unset or empty).      | Set the referenced environment variable to a valid state name.                                                                       |
| `config: tracker.in_progress_state: "<val>" collides with terminal state "<state>"` | `in_progress_state` matches one of the `terminal_states` (case-insensitive). | Use a state that is not in `terminal_states`.                                                                                        |
| `config: tracker.in_progress_state: "<val>" is not in active_states...`            | `in_progress_state` is not in `active_states` (case-insensitive).            | Add the state to `active_states`, or use a state already in `active_states`.                                                         |
| `config: tracker.in_progress_state: "<val>" collides with handoff_state "<state>"` | `in_progress_state` matches `handoff_state` (case-insensitive).              | Use different states for dispatch-time and exit-time transitions.                                                                    |
| `config: workspace.root: cannot expand ~: <err>`                                | Home directory expansion failed.                                         | Check that the `HOME` environment variable is set.                                                                                   |
| `config: workspace.retention_days: must not be negative`                        | Negative value for `retention_days`.                                     | Use `0` (disabled) or a positive integer of at least `30`.                                                                           |
| `config: workspace.retention_days: must be 0 to disable or at least 30 days`    | `retention_days` is between `1` and `29` inclusive.                       | Use `0` to disable the bound, or a value of `30` or greater.                                                                         |
| `config: db_path: expected string, got <type>`                                  | `db_path` is not a string value.                                         | Use a string path value, quoted if necessary.                                                                                        |
| `config: db_path: resolved to empty (check environment variable)`               | `$VAR` reference resolved to empty.                                      | Set the environment variable or use a literal path.                                                                                  |
| `config: ci_feedback.max_retries: invalid integer value: <val>`                 | Non-integer value for `max_retries`.                                     | Use a plain integer (e.g., `2`).                                                                                                     |
| `config: ci_feedback.max_retries: must be non-negative`                         | Negative value for `max_retries`.                                        | Use `0` (escalate immediately) or a positive integer.                                                                                |
| `config: ci_feedback.max_log_lines: invalid integer value: <val>`               | Non-integer value for `max_log_lines`.                                   | Use a plain integer (e.g., `50`).                                                                                                    |
| `config: ci_feedback.max_log_lines: must be non-negative`                       | Negative value for `max_log_lines`.                                      | Use `0` (disable log fetching) or a positive integer.                                                                                |
| `config: ci_feedback.escalation: must be "label" or "comment", got "<val>"`     | Invalid escalation strategy.                                             | Use `"label"` or `"comment"`.                                                                                                        |

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
| `tracker.handoff_evidence`              | string           | `observed`                   | —                                        | `observed`, `strict`, or `off`; closed set                                             |
| `tracker.in_progress_state`             | string           | _(absent)_                   | `SORTIE_TRACKER_IN_PROGRESS_STATE`       | Must be in active; must not collide with terminal/handoff                              |
| `tracker.api_version`                   | string           | `"3"`                        | —                                        | Jira only; `"3"` (Cloud) or `"2"` (Server/DC); `$VAR` supported; quote the value       |
| `tracker.comments.on_dispatch`          | bool             | `false`                      | `SORTIE_TRACKER_COMMENTS_ON_DISPATCH`    |                                                                                        |
| `tracker.comments.on_completion`        | bool             | `false`                      | `SORTIE_TRACKER_COMMENTS_ON_COMPLETION`  |                                                                                        |
| `tracker.comments.on_failure`           | bool             | `false`                      | `SORTIE_TRACKER_COMMENTS_ON_FAILURE`     |                                                                                        |
| `polling.interval_ms`                   | integer          | `30000`                      | `SORTIE_POLLING_INTERVAL_MS`             | Dynamic reload                                                                         |
| `workspace.root`                        | path             | `<tmpdir>/sortie_workspaces` | `SORTIE_WORKSPACE_ROOT`                  | `~` expanded; `$VAR` skipped for env-sourced values                                    |
| `workspace.retention_days`              | integer          | `0` (disabled)               | `SORTIE_WORKSPACE_RETENTION_DAYS`        | Floor `30`; `1`-`29` rejected; dynamic reload, applies on the next sweep pass          |
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
| `agent.max_tokens`                      | integer          | `0`                          | `SORTIE_AGENT_MAX_TOKENS`                | Unlimited; dynamic reload                                                              |
| `db_path`                               | path             | `.sortie.db`                 | `SORTIE_DB_PATH`                         | Restart required; `$VAR` skipped for env-sourced values                                |
| `ci_feedback.kind`                      | string           | _(absent)_                   | —                                        | **Deprecated;** absent = disabled; restart required                                    |
| `ci_feedback.max_retries`               | integer          | `2`                          | —                                        | **Deprecated;** `0` = escalate immediately; non-negative                               |
| `ci_feedback.max_log_lines`             | integer          | `50`                         | —                                        | **Deprecated;** `0` = disable log fetching; restart required                           |
| `ci_feedback.escalation`                | string           | `label`                      | —                                        | **Deprecated;** `"label"` or `"comment"`                                               |
| `ci_feedback.escalation_label`          | string           | `needs-human`                | —                                        | **Deprecated;** applied when `escalation` is `"label"`                                 |
| `reactions.<kind>.provider`             | string           | _(absent)_                   | —                                        | Adapter identifier; absent = disabled; restart required                                |
| `reactions.<kind>.max_retries`          | integer          | `2`                          | —                                        | Fix continuations before escalation; non-negative; `merge_conflicts` defaults to `1`; restart required except for `ci_failure` |
| `reactions.<kind>.escalation`           | string           | `label`                      | —                                        | `"label"` or `"comment"`; restart required except for `ci_failure`                     |
| `reactions.<kind>.escalation_label`     | string           | `needs-human`                | —                                        | Applied when `escalation` is `"label"`; restart required except for `ci_failure`       |
| `reactions.ci_failure.max_log_lines`    | integer          | `50`                         | —                                        | CI log tail lines; `0` disables; non-negative; restart required                        |
| `reactions.review_comments.poll_interval_ms` | integer     | `120000`                     | —                                        | Review poll interval; min `30000`; restart required                                    |
| `reactions.review_comments.debounce_ms` | integer          | `60000`                      | —                                        | Debounce after last comment; non-negative; restart required                            |
| `reactions.review_comments.max_continuation_turns` | integer | `3`                        | —                                        | Review-fix turns before escalation; positive; restart required                         |
| `reactions.merge_completion.target_state` | string         | _(required)_                 | —                                        | Terminal state applied on observed merge; must be in `terminal_states` as written; restart required |
| `reactions.merge_completion.poll_interval_ms` | integer    | `60000`                      | —                                        | Merge-observation poll interval; min `30000`; restart required                         |
| `self_review.enabled`                   | boolean          | `false`                      | —                                        | Activates self-review loop                                                             |
| `self_review.max_iterations`            | integer          | `3`                          | —                                        | Range [1, 10]; up to `2N−1` extra turns                                                |
| `self_review.verification_commands`     | `[string]`       | _(required when enabled)_    | —                                        | Shell commands for verification                                                        |
| `self_review.verification_timeout_ms`   | integer          | `120000`                     | —                                        | Per-command timeout                                                                    |
| `self_review.max_diff_bytes`            | integer          | `102400`                     | —                                        | Diff truncation limit                                                                  |
| `self_review.reviewer`                  | string           | `"same"`                     | —                                        | Only `"same"` in v1                                                                    |
| `notifications`                         | `[map]`          | _(absent)_                   | —                                        | Notifier backend list; `notify_operator` tool; absent = tool unregistered             |
| `notifications[].kind`                  | string           | _(required)_                 | —                                        | Backend discriminator; v1: `webhook`, `slack`                                          |
| `notifications[].max_per_session`       | integer          | `20`                         | —                                        | Per-session `notify_operator` cap; `0` selects the default; never unlimited; non-negative |
| **Extensions**                          |                  |                              |                                          |                                                                                        |
| `server.port`                           | integer          | `7678`                       | —                                        | CLI `--port` overrides; `0` disables server                                    |
| `server.host`                           | string (IP)      | `127.0.0.1`                  | —                                        | CLI `--host` overrides                                                         |
| `logging.level`                         | string           | `info`                       | —                                        | CLI `--log-level` overrides                                                            |
| `worker.ssh_hosts`                      | `[string]`       | _(absent)_                   | —                                        | SSH host targets; dynamic reload                                                       |
| `worker.max_concurrent_agents_per_host` | integer          | _(absent)_                   | —                                        | Per-host cap; dynamic reload                                                           |
| `worker.ssh_strict_host_key_checking`   | string           | `accept-new`                 | —                                        | `accept-new`, `yes`, `no`; dynamic reload                                              |

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

> **Note:** In this example, `after_create` runs only when Sortie first creates
> the per-issue workspace directory, so the directory is empty when the clone
> runs. If `after_create` fails, Sortie removes the directory before the next
> retry, so a retry also starts from an empty directory. A clone error such as
> "destination path already exists" or "directory not empty" does not come from
> this example on the normal path.
>
> Hooks also run with a restricted environment: an allowlist (including `HOME`
> and `SSH_AUTH_SOCK`) plus `SORTIE_*` variables. Sortie strips any variable
> outside that set, such as `GIT_SSH_COMMAND`, so an SSH clone must reach its
> key through the SSH agent (`SSH_AUTH_SOCK`) or through `~/.ssh` via `HOME`,
> not through a stripped variable. See Section 6.2 for the full allowlist.

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

# ─── CI Feedback ───────────────────────────────────────────────
# Omit this section entirely to disable CI feedback.
# SCM coordinates (owner, repo, token) live in the github: adapter block.
ci_feedback:
  kind: github # Activate CI feedback via GitHub Checks API
  max_retries: 2 # CI-fix attempts before escalation
  max_log_lines: 50 # Lines from first failing check log
  escalation: label # "label" or "comment" on exhaustion
  escalation_label: needs-human # Label added when escalation is "label"

# ─── Reactions (preferred over ci_feedback) ────────────────────
# reactions:
#   ci_failure:
#     provider: github
#     max_retries: 2
#     max_log_lines: 50
#     escalation: label
#     escalation_label: needs-human
#   review_comments:
#     provider: github
#     max_retries: 2
#     escalation: label
#     escalation_label: needs-human
#     poll_interval_ms: 120000  # 2-minute review poll cycle
#     debounce_ms: 60000        # 60s debounce after last comment
#     max_continuation_turns: 3 # Max review-fix dispatches

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

### 11.3 Self-Review with Go Verification

A workflow enabling automated self-review after each coding session:

```yaml
---
tracker:
  kind: jira
  endpoint: $SORTIE_JIRA_ENDPOINT
  api_key: $SORTIE_JIRA_API_KEY
  project: PROJ
  active_states: [To Do, In Progress]
  terminal_states: [Done]

agent:
  kind: claude-code
  max_turns: 10

# ─── Self-Review ───────────────────────────────────────────────
# After the coding turn loop, run verification and let the agent
# review its own work. Up to 3 iterations (5 additional turns).
self_review:
  enabled: true
  max_iterations: 3           # 3 review rounds = up to 5 extra turns
  verification_commands:
    - "make fmt"              # Format check
    - "make lint"             # Static analysis
    - "make test"             # Full test suite
  verification_timeout_ms: 180000  # 3 min per command
  max_diff_bytes: 102400      # 100 KB diff cap
  reviewer: same              # Reuse the same agent session

hooks:
  after_run: |
    echo "Self-review status: $SORTIE_SELF_REVIEW_STATUS"
    if [ -f "$SORTIE_SELF_REVIEW_SUMMARY_PATH" ]; then
      cat "$SORTIE_SELF_REVIEW_SUMMARY_PATH"
    fi
---

Fix {{ .issue.identifier }}: {{ .issue.title }}
```

---

### 11.4 Linear + Claude Code

A workflow targeting a Linear team. `project` is the team key, the state names are Linear
workflow states, and `query_filter` is a Linear `IssueFilter` JSON object:

```markdown
---
tracker:
  kind: linear
  api_key: $SORTIE_LINEAR_API_KEY # Linear personal API key (lin_api_...)
  project: ENG # Linear team key (prefix in ENG-123)
  query_filter: '{ "labels": { "name": { "eq": "agent-ready" } } }' # IssueFilter object
  active_states:
    - Todo
    - In Progress
  terminal_states:
    - Done
    - Canceled
    - Duplicate
  handoff_state: In Review # Linear workflow state after a successful run

agent:
  kind: claude-code
  max_turns: 10
---

Fix {{ .issue.identifier }}: {{ .issue.title }}
{{ if .issue.description }}

{{ .issue.description }}
{{ end }}
```

---

_This document is derived strictly from the Sortie Architecture Specification
(Sections 5, 6, 9.4, and 10) and informed by end-to-end testing experience (tasks
7.11–7.13). It is the authoritative user-facing reference for workflow authors._
