# WORKFLOW.md Syntax Reference

> **Authoritative user-facing reference for workflow authors.** Derived from the Sortie Architecture Specification — Sections 5, 6, 9.4, and 10.
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
  - [2.8 `self_review` — Self-Review Configuration](#28-self_review--self-review-configuration)
  - [2.9 `reactions` — Reaction-Based Feedback Loops](#29-reactions--reaction-based-feedback-loops)
  - [2.10 `dispatch` — Rule-based Routing](#210-dispatch--rule-based-routing)
  - [2.11 `notifications` (operator notification backends)](#211-notifications-operator-notification-backends)
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
  - [5.8 Define Blocks and Calls](#58-define-blocks-and-calls)
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
  - [9.5 Agent Errors](#95-agent-errors)
- [10. Config Fields Summary (Cheat Sheet)](#10-config-fields-summary-cheat-sheet)
- [11. Complete Annotated Examples](#11-complete-annotated-examples)
  - [11.1 Minimal Workflow](#111-minimal-workflow)
  - [11.2 Production Jira + Claude Code](#112-production-jira--claude-code)
  - [11.3 Self-Review with Go Verification](#113-self-review-with-go-verification)
  - [11.4 Linear + Claude Code](#114-linear--claude-code)

---

## 1. File Format

### 1.1 Overview

`WORKFLOW.md` is a Markdown file with optional YAML front matter. It encodes two payloads in a single document:

| Payload             | Location                            | Purpose                                           |
| ------------------- | ----------------------------------- | ------------------------------------------------- |
| **Configuration**   | YAML front matter (between `---`)   | Tracker, polling, workspace, hooks, agent         |
| **Prompt template** | Markdown body (after closing `---`) | Per-issue prompt rendered with Go `text/template` |

The file is repository-owned and version-controlled. It is self-contained enough to describe a complete workflow — prompt, runtime settings, hooks, and tracker selection — without requiring out-of-band service-specific configuration.

**File discovery precedence:**

1. Explicit path provided via CLI startup argument.
2. Default: `WORKFLOW.md` in the current process working directory.

### 1.2 Parsing Rules

The parser applies the following steps in order:

1. **BOM stripping.** Remove a leading UTF-8 byte order mark (`\xef\xbb\xbf`) if present.
2. **Line ending normalization.** Replace all `\r\n` with `\n`.
3. **Opening delimiter detection.** If the first line is exactly `---` followed by a newline (with optional trailing whitespace), enter front matter mode. A file whose entire content is `---` with no trailing newline is treated as having no front matter.
4. **Front matter extraction.** Scan lines until a line that is exactly `---` (with optional trailing whitespace). Bytes between the delimiters are the YAML front matter. If no closing delimiter is found, the entire content after the opening delimiter is treated as front matter and the prompt body is empty (this is not an error).
5. **YAML decoding.** Decode front matter bytes to a map. Non-map YAML (scalar, list) is a parse error. Empty or comment-only YAML between delimiters produces an empty map.
6. **Prompt body extraction.** All remaining bytes after the closing delimiter become the prompt template, trimmed of leading and trailing whitespace.

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

The core schema recognizes ten top-level keys:

```yaml
tracker: # Issue tracker connection and query settings
polling: # Poll loop timing
workspace: # Workspace root path
hooks: # Workspace lifecycle hook scripts
agent: # Coding agent adapter, timeouts, and limits
db_path: # SQLite database file path
reactions: # Reaction-based feedback loops (CI failure, review comments)
self_review: # Self-review verification loop (optional)
dispatch: # Rule-based dispatch routing
notifications: # Operator notification backends (notify_operator tool)
```

**Unknown top-level keys are ignored** by the core schema for forward compatibility. They are collected into an `Extensions` map and made available to consumers (e.g., `server`, `worker`, adapter-specific blocks like `claude-code`).

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
```

| Field             | Type            | Required                  | Default         | Dynamic Reload                     | Description                                                                                                                                                                                     |
| ----------------- | --------------- | ------------------------- | --------------- | ---------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `kind`            | string          | **Yes** (for dispatch)    | _(none)_        | Requires restart                   | Adapter identifier. Supported: `jira`, `github`, `linear`, `gitea`, `gitlab`, `file`. Additional adapters are registered separately.                                                            |
| `endpoint`        | string          | Adapter-defined           | Adapter-defined | Requires restart                   | Tracker API endpoint URL. Supports `$VAR` indirection: if the value starts with `$`, it is expanded via `os.ExpandEnv`.                                                                         |
| `api_key`         | string          | When adapter requires it  | _(none)_        | Requires restart                   | API authentication token. May be a literal or `$VAR_NAME`. If `$VAR_NAME` resolves to empty, treated as missing. The `jira`, `github`, `linear`, `gitea`, and `gitlab` adapters require this field; `file` does not. Full env expansion applied (`$VAR` at any position). |
| `project`         | string          | When adapter requires it  | _(none)_        | Requires restart                   | Project identifier. Interpretation is adapter-defined: Jira project key, GitHub or Gitea `owner/repo`, GitLab namespace path (`group/project`) or numeric project ID, or Linear team key (e.g., `ENG`). Supports `$VAR` indirection: if the value starts with `$`, it is expanded via `os.ExpandEnv`. |
| `api_version`     | string          | No                        | `"3"`           | Requires restart                   | Jira REST API version selector: `"3"` (Cloud) or `"2"` (Server / Data Center). Supports `$VAR` indirection. Quote the value: a bare integer (`api_version: 2`) is coerced to its decimal string but emits a validation advisory. Adapters other than Jira ignore this field. `sortie validate` rejects a value other than `"2"` or `"3"`, and rejects `"2"` against an `.atlassian.net` endpoint. |
| `active_states`   | list of strings | **Yes** (see rules below) | `[]` (empty)    | Future dispatch, reconciliation, and tracker operations | Issue states eligible for agent dispatch. An issue is eligible for dispatch only if its state appears in this list. An empty list dispatches nothing. No tracker supplies a default list, and an entry cannot be blank.                           |
| `terminal_states` | list of strings | **Yes** (see rules below) | `[]` (empty)    | Future dispatch, reconciliation, and tracker operations | Issue states that release claims and trigger cleanup. Required and non-empty for `github`, `gitea`, and `gitlab`, where a closed issue reads as the first entry. No tracker supplies a default list, and an entry cannot be blank. |
| `query_filter`    | string          | No                        | `""` (empty)    | Requires restart                   | Adapter-defined query fragment that narrows the candidate issue query and, for adapters that apply it there, the terminal-state query. Each adapter interprets it in its own query language. For Jira: JQL fragment (e.g., `"labels = 'agent-ready'"`). For Linear: an `IssueFilter` JSON object, merged rather than appended (see the Linear note below). For Gitea: a URL query fragment for the repo issue-list route, merged into candidate polling only (see the Gitea note below). For GitLab: a URL query fragment for the project issue-list route, key-checked against an allowlist and merged into candidate polling only (see the GitLab note below). |
| `handoff_state`   | string          | No                        | _(absent)_      | Future worker exits                | Target tracker state for orchestrator-initiated handoff after successful worker run. When absent, no handoff transition is performed. The transition is suppressed when the issue has already reached a terminal state, and withheld when the run's frozen `handoff_evidence` policy does not permit the write ([architecture §11.5](architecture/11-issue-tracker-integration-contract.md#115-tracker-writes-important-boundary)). When the run's rule carries `next` and the hop is made, the exit adds the next rule's stage label in place of this transition, so the transition happens only when the hop is not made ([Stage chains](#stage-chains)). Required when any dispatch rule carries `next`. |
| `handoff_evidence` | string         | No                        | `observed`      | Future worker attempts             | Policy governing the evidence condition on an otherwise-eligible handoff. `observed` withholds only when absence of work is positively observed and allows an undeterminable verdict; `strict` also withholds an undeterminable verdict; `off` restores the prior four-condition decision and performs no baseline capture, exit-time workspace inspection, or evidence logging. Values outside `observed`, `strict`, and `off` are rejected as a closed set while configuration is parsed, without contacting the tracker, so startup, reload, and `sortie validate` all reject the same value. See [architecture §6.4](architecture/06-configuration-specification.md#64-config-fields-summary-cheat-sheet) for the verdict's derivation. |
| `no_change_state` | string          | No                        | `handoff_state` | Future worker exits                | Target tracker state for a worker run whose agent declared, through `.sortie/status`, that the requested outcome already held and nothing needed changing. When absent, a declared run targets `handoff_state` instead, so a deployment that does not set this field sees no change in where its issues land. Is the one target-state field permitted to name a terminal state. A declared run on a rule that carries `next` hops like any other successful run, so this target applies to such a run only when the hop is not made. |
| `in_progress_state` | string        | No                        | _(absent)_      | Future dispatches                  | Target tracker state for dispatch-time transition at the start of each worker attempt. When absent, no dispatch-time transition is performed. Must be in `active_states`. Must not collide with `terminal_states` or `handoff_state`. |
| `comments`        | map of booleans | No                        | all `false`     | Future dispatches (`on_dispatch`); future worker exits (`on_completion`, `on_failure`) | **Deprecated.** Toggles for the comments Sortie posts at session lifecycle points. Keys: `on_dispatch`, `on_completion`, `on_failure`. Each is a boolean defaulting to `false`; non-boolean values are rejected with a configuration error. A key set to `true` subscribes the `tracker_comment` destination to events: `on_dispatch` to `session.started`, `on_completion` to `session.completed` and `session.stopped`, and `on_failure` to `session.failed`. The keys keep working and draw a deprecation warning that names the replacement: list those events in a `tracker_comment` entry of [`notifications`](#211-notifications-operator-notification-backends), then remove the key. See [Section 3.2](#32-curated-variable-list) for the matching `SORTIE_TRACKER_COMMENTS_*` env overrides, which are deprecated with them. |

**`active_states` / `terminal_states` validation rules:**

- Neither list has a default, on any tracker. At startup, if **both** lists are empty, validation fails with an error, because at least one of the two must be configured.
- An issue is dispatch-eligible only if its tracker state appears in `active_states`. With an empty `active_states` list no issues will be dispatched even if other configuration is valid, and `sortie validate` warns under check `tracker.active_states.empty`.
- The `github`, `gitea`, and `gitlab` trackers require a non-empty `terminal_states`, because a closed issue reads as its first entry. An empty list fails the load under check `config.tracker.terminal_states`.
- An entry that is empty or only white space fails the load on every tracker, under check `config.tracker.active_states[<i>]` or `config.tracker.terminal_states[<i>]`.
- A reload that breaks any of these rules keeps the previous configuration.

**`handoff_state` validation rules:**

- Supports `$VAR` environment indirection.
- When set, must be a non-empty string after `$VAR` resolution. Empty resolution is a configuration error.
- Must **not** appear in `active_states` (would cause immediate re-dispatch after handoff).
- Must **not** appear in `terminal_states` (handoff is not terminal — the issue may return to active for further work).
- Both list checks compare against the lists as written, and the dispatch preflight reports the collision under check `tracker.handoff_state`.
- Must be set when any `dispatch.rules[*]` entry carries `next`: the last stage of a chain, and every stage whose hop is not made, ends on this state. The load fails otherwise (see [Section 9.2](#92-configuration-errors)).
- Requires **write permissions** on the tracker API token. For Jira: `write:jira-work` (classic) or `write:issue:jira` (granular).

**`handoff_state` runtime behavior:**

- The handoff transition is suppressed when the issue has already reached a state in `terminal_states`. Overwriting a terminal state with the handoff state would undo an operator action such as cancelling the issue mid-turn.
- Where the run's rule carries `next`, the exit adds the next rule's stage label instead of writing this state, and the issue stays in its active state. The handoff transition is the fallback when the hop is not made. See [Stage chains](#stage-chains).
- The orchestrator resolves the freshest tracker observation available at worker exit, in order: reconciliation's observation, then the worker's own per-turn state refresh, then the dispatch-time snapshot. A terminal result suppresses the handoff transition, the continuation retry, and every pending reaction enqueue for that exit.
- When the resolved observation is not terminal, one verification read runs immediately before the handoff write or the hop, so a state change that landed after the last observation is still caught. The verification read is skipped when `terminal_states` is empty, because no value can classify as terminal there. A failed verification read is logged and the handoff or the hop proceeds.
- A transition suppressed by a terminal state, or by the issue not being in an active state at worker exit, increments `sortie_handoff_transitions_total` with `result="skipped"`. A run withheld by the evidence verdict increments the same counter with `result="withheld"` instead, but only when its own verification read below does not find the issue terminal; when that read does find the issue terminal, the run increments `result="skipped"` instead (see [Section 4.1](#41-http-server-serverport-serverhost)). A made hop writes no handoff transition and increments the counter with no result.

**`handoff_evidence` runtime behavior:**

- The evidence condition is a fifth condition on the handoff path, consulted only where four conditions already select that path: a handoff state is configured, the issue is still in an active state, the exit is not a blocked soft stop, and the dispatch drives issue state. It can suppress a handoff write or a hop and can never cause either.
- The verdict has three values, not two: work observed, absence of work observed, and evidence not determinable. It is computed at worker exit over the run's workspace against a baseline captured immediately before the agent starts. Under `off` no baseline is captured and no verdict is computed, so the four-condition decision stands unchanged.
- A withheld verdict is, immediately before any of its effects, checked once more against the tracker: a verification read for the issue, gated the same way the permit path's own pre-write read above is gated. When that read reports the issue terminal, the run takes the terminal disposition instead: no handoff transition, no failure record, no retry, and the completion comment where `tracker.comments.on_completion` is enabled. Only when that read does not find the issue terminal does the withheld run make no handoff transition, leave the issue in its active tracker state, get recorded in run history as `failed` with an error reason composed of a reserved marker prefix, the verdict, and the policy that withheld it, and schedule the ordinary exponential-backoff failure path rather than the fixed-delay continuation path.
- Consecutive withheld outcomes are counted per issue, and reaching the ceiling parks the issue under a label whose name is taken from `reactions.review_comments.escalation_label`. That ceiling is governed by `agent.max_consecutive_absences`; the reset rule for the count, and the two operator gestures that release a park, are specified in [architecture §14.2](architecture/19-failure-model-and-recovery-strategy.md#142-recovery-behavior).

**`no_change_state` validation rules:**

- Supports `$VAR` environment indirection and the `SORTIE_TRACKER_NO_CHANGE_STATE` environment override.
- When set, must be a non-empty string after `$VAR` resolution. Empty resolution is a configuration error.
- Requires `handoff_state` to be non-empty. A declared run performs no transition where no handoff path applies, so a `no_change_state` set with `handoff_state` unset is a configuration error rather than a silently inert field.
- Must equal `handoff_state` (case-insensitive) or name a member of `terminal_states` exactly as written in front matter (case-insensitive). The check is entirely offline and confined to the two values written in configuration.
- This is the one target-state field permitted to equal a value that is also a member of `terminal_states`; every sibling target-state field is rejected on that collision.

**`no_change_state` runtime behavior:**

- The declaration is the agent's own assertion. It is checked only by the self-review phase, through its `pass` verdict and its verification commands (Section 2.8); on a deployment with self-review disabled, the declaration is taken on the agent's word with no check at all.
- Under `tracker.handoff_evidence: observed` or `strict`, a run whose declaration stands is always treated as positive evidence: no absence verdict is computed, the run is recorded as `succeeded`, the consecutive-absence count is reset rather than advanced, and a park held for consecutive absences is released, on the same terms as any other work-observed verdict.
- Under `tracker.handoff_evidence: off`, no verdict is computed for a declared run, so neither the count reset nor the park release occurs; the declaration's only effect there is the transition target.
- A terminal `no_change_state` ends the issue with no reaction running for a pull request the run left behind: no CI feedback, no review comments, no bot review, no auto-merge, no merge-conflict handling, and no merge completion. This is the same residue any handoff to a terminal state carries, made the ordinary outcome rather than an operator's own terminal action once this field names a terminal state.

**`in_progress_state` validation rules:**

- Supports `$VAR` environment indirection.
- When set, must be a non-empty string after `$VAR` resolution. Empty resolution is a configuration error.
- Must appear in `active_states` (case-insensitive). If the issue transitions to a state outside `active_states`, reconciliation would immediately cancel the worker.
- Must **not** appear in `terminal_states` (case-insensitive). A terminal state would trigger workspace cleanup on the next reconciliation tick.
- Must **not** collide with `handoff_state` (case-insensitive). The two transitions represent different lifecycle phases — dispatch vs. exit.
- Both list checks compare against the lists as written, because dispatch and reconciliation gate on those lists. The dispatch preflight reports a collision under check `tracker.in_progress_state`.
- Transition failure at runtime is non-fatal: the worker logs a warning and continues to workspace preparation.
- If the issue is already in the target state (case-insensitive), the transition API call is skipped and a debug-level message is logged.
- Requires **write permissions** on the tracker API token (same as `handoff_state`).

**`api_key` environment resolution:**

The `api_key` field uses full environment expansion (`$VAR`, `${VAR}`, and mixed content at any position in the string). All other `$VAR`-supporting fields in `tracker` use targeted resolution: if the trimmed value starts with `$`, the entire string is expanded via `os.ExpandEnv` (for example, `$HOST/api/rest` expands as expected). Values that do not start with `$` are returned unchanged, preserving literal URI strings.

**Linear tracker (`kind: linear`):**

The Linear adapter talks to Linear's single GraphQL endpoint. Configure it with the same generic `tracker.*` fields used for every adapter; the Linear-specific interpretation of those fields is:

- `endpoint` defaults to `https://api.linear.app/graphql` and rarely needs to be set, since there is no self-hosted Linear.
- `api_key` is a Linear personal API key (it carries the `lin_api_` prefix). The key is sent verbatim in the `Authorization` header with no `Bearer` prefix, so leading or trailing whitespace fails authentication. Supply it through environment indirection like any other tracker, for example `api_key: $SORTIE_LINEAR_API_KEY`. The key resolves through the standard `tracker.api_key` field (env override `SORTIE_TRACKER_API_KEY`); `SORTIE_LINEAR_API_KEY` is the conventional variable name the `sortie validate` advisory suggests, not a separate config path.
- `project` is the Linear **team key** (the prefix in identifiers such as `ENG-123`), for example `ENG`. It is not a Linear project and not an `owner/repo` path. Team scoping is required because Linear workflow states are team-scoped.
- `active_states`, `terminal_states`, and `handoff_state` name Linear **workflow states** by their display name (for example `Backlog`, `Todo`, `In Progress`, `Done`, `Canceled`). The adapter matches names case-insensitively and verifies at startup that every configured name exists in the team. The adapter applies no default lists: both lists are configured like any other tracker's, and an empty `active_states` dispatches nothing.

`handoff_state` and `in_progress_state` also name Linear workflow states. At transition time the adapter resolves the configured name to its team-scoped workflow-state id and applies it. Linear imposes no transition graph, so any state can move to any state.

**Linear `query_filter`:** For `kind: linear`, `query_filter` is a Linear `IssueFilter` JSON **object**, not a string predicate. The adapter parses the value as JSON and merges it as additional fields of the GraphQL `filter` argument alongside the team and state constraints it always applies, so Linear ANDs the operator filter with the base query. This differs from Jira, where the fragment is appended to a JQL string. Rules:

- The value MUST be a JSON object. A value that is not valid JSON, or parses to a non-object, is a configuration error.
- The object MUST NOT contain a top-level `team` or `state` key. Those keys are reserved for the adapter's own team and state constraints, and supplying either is a configuration error.

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

The Gitea adapter talks to a self-hosted Gitea instance over the Gitea REST API v1. Configure it with the same generic `tracker.*` fields used for every adapter; the Gitea-specific interpretation of those fields is:

- `endpoint` is the instance base URL (for example `https://gitea.example.com`) and is required: there is no default host, because Gitea is self-hosted. Supply the site root; the adapter trims a trailing slash and appends `/api/v1`, and tolerates an endpoint that already ends in `/api/v1`. Use `https`, since the token travels in a request header.
- `api_key` is a Gitea access token. The adapter sends it verbatim as `Authorization: token <key>` (the canonical Gitea scheme, not a `Bearer` prefix), so leading or trailing whitespace fails authentication. Supply it through environment indirection like any other tracker, for example `api_key: $SORTIE_GITEA_TOKEN`. The key resolves through the standard `tracker.api_key` field (env override `SORTIE_TRACKER_API_KEY`); `SORTIE_GITEA_TOKEN` is the conventional variable name the `sortie validate` advisory suggests, not a separate config path.
- `project` is the repository in **`owner/repo`** form (for example `sortie-ai/sortie`): exactly one slash, with a non-empty owner and repository.
- `active_states`, `terminal_states`, and `handoff_state` name repository **labels**, compared case-insensitively (the adapter lowercases them). The adapter carries no default labels. An open issue is dispatched only when it carries a label from `active_states`, so labeling an issue is how an operator hands it to Sortie, and an open issue with none of the configured state labels is left alone. `terminal_states` must be non-empty, because a closed issue reads as its first entry. An empty `active_states` dispatches nothing, and validation rejects a workflow with both lists empty. Configure `active_states` with the labels a dispatched Gitea issue must carry.

Gitea has no transition workflow, so the adapter derives an issue's state from its labels: it scans the configured terminal, then handoff, then active labels in list order and takes the first match. A closed issue reads as the first terminal label it carries, otherwise as the first entry of `terminal_states`. An open issue carrying no configured state label is in no state and is not dispatched. A running issue whose state label is removed stops at the second consecutive poll that reads no state label, and its workspace stays. An issue carrying more than one configured state label logs a warning and keeps the first match. The adapter warns once about each active label that neither the repository nor its organization has, `in_progress_state` excepted, and `sortie --dry-run` logs how many open issues it skipped for carrying no state label. A configured label that does not yet exist in the repository is created on demand the first time an issue transitions into it.

`handoff_state` and `in_progress_state` also name repository labels. At transition time the adapter adds the target label first, then removes the labels of the other configured states, so the issue never reads as being in no state between the two writes. A move to a terminal label then closes the issue, and a move to an active label reopens a closed one. A move to the handoff state is refused for a closed issue. The issue carries the labels of exactly one configured state afterward, and labels outside the configured states are never touched. Gitea imposes no transition graph, so any open issue can move to any configured state. The dispatch-time `in_progress_state` transition runs through this same label swap, so its generic validation rules (must appear in `active_states`, must not collide with `terminal_states` or `handoff_state`) apply unchanged.

**Gitea `query_filter`:** For `kind: gitea`, `query_filter` is a URL query fragment for Gitea's repository issue-list route, not a string predicate or a JSON object. The adapter parses it with `url.ParseQuery` and merges the parameters into candidate polling, so an operator can scope which open issues the agent picks up (for example, to issues assigned to or mentioning the automation identity). Parameters combine with `&`. Rules:

- The adapter rejects only the four keys it owns: `state`, `type`, `page`, and `limit`. Naming any of them fails construction with a configuration error.
- Any other key is accepted and merged. A key outside Gitea's known repo issue-list filter set (`labels`, `q`, `milestones`, `since`, `before`, `created_by`, `assigned_by`, `mentioned_by`) is a likely typo, such as `assignee=` or `assigned_to=` for the Gitea key `assigned_by`. The adapter warns at construction and names the unrecognized key, but still passes it through unchecked. Gitea silently ignores a parameter it does not honor and returns every open issue, so a misspelled key widens rather than narrows the candidate set; correcting it is the operator's responsibility.
- A `labels` value inherits Gitea's server-side semantics: AND across comma-separated names, and case-sensitive matching. A name that resolves to no repository label silently drops the entire filter, so Gitea returns every open issue rather than an empty set. The adapter warns at construction when a `labels` value does not match a repository label by exact case, turning the silent drop into a visible signal. The warning does not block construction, because an operator may reference a label that does not exist yet.
- `mentioned_by` does not count an author mentioning themselves. This behavior is verified against the tested Gitea version but is not documented in Gitea's REST API, so it can differ across Gitea or Forgejo releases. `mentioned_by=<identity>` matches an issue only when the identity differs from the issue author, so setting the automation identity as both author and mention target yields no candidates.

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

The GitLab adapter talks to GitLab.com or a self-managed instance over the GitLab REST API v4. Configure it with the same generic `tracker.*` fields used for every adapter; the GitLab-specific interpretation of those fields is:

- `endpoint` is the instance base URL (for example `https://gitlab.example.com`) and is optional: it defaults to `https://gitlab.com`, so a GitLab.com workflow omits it and a self-managed workflow sets it. Supply the site root; the adapter trims a trailing slash and appends `/api/v4`, and tolerates an endpoint that already ends in `/api/v4`, though `sortie validate` warns about the redundant suffix. Use `https`, since the token travels in a request header; a cleartext `http` endpoint also draws a validation warning.
- `api_key` is a GitLab access token: personal, project, or group. The adapter sends it verbatim in the `PRIVATE-TOKEN` header, so leading or trailing whitespace fails authentication. The token needs the `api` scope, which is the only classic scope that permits issue writes; `read_api` suffices only for a deployment that never transitions an issue, posts a comment, or attaches a label. A project access token is the least-privilege choice, because GitLab confines it to one project server-side, and a self-managed instance offers that token type at any license. On GitLab.com it requires a Premium or Ultimate subscription, so a Free namespace uses a personal access token. Supply the token through environment indirection like any other tracker, for example `api_key: $SORTIE_GITLAB_TOKEN`. The key resolves through the standard `tracker.api_key` field (env override `SORTIE_TRACKER_API_KEY`); `SORTIE_GITLAB_TOKEN` is the conventional variable name the `sortie validate` advisory suggests, not a separate config path.
- `project` is the project's full namespace path (for example `group/project`) or its numeric project ID (for example `"1"`, quoted so YAML keeps it a string). GitLab nests subgroups to any depth, so `group/subgroup/project` is equally valid and the adapter enforces no one-slash rule, unlike the GitHub and Gitea `owner/repo` grammar. A project in a user namespace works the same way. Write the path unencoded: the adapter percent-encodes it once for the API path, and `sortie validate` rejects a value that is already percent-encoded.
- `active_states`, `terminal_states`, and `handoff_state` name project or group **labels**, compared case-insensitively (the adapter lowercases them). The adapter carries no default labels. An open issue is dispatched only when it carries a label from `active_states`, so labeling an issue is how an operator hands it to Sortie, and an open issue with none of the configured state labels is left alone. `terminal_states` must be non-empty, because a closed issue reads as its first entry. An empty `active_states` dispatches nothing, and validation rejects a workflow with both lists empty. Configure `active_states` with the labels a dispatched GitLab issue must carry.

GitLab has no transition workflow, so the adapter derives an issue's state from its labels: it scans the configured terminal, then handoff, then active labels in list order and takes the first match. A closed issue reads as the first terminal label it carries, otherwise as the first entry of `terminal_states`. An open issue carrying no configured state label is in no state and is not dispatched. A running issue whose state label is removed stops at the second consecutive poll that reads no state label, and its workspace stays. An issue carrying more than one configured state label logs a warning and keeps the first match. The adapter warns once about each active label the project does not hold, inherited group labels included and `in_progress_state` excepted, and `sortie --dry-run` logs how many open issues it skipped for carrying no state label.

GitLab label names are case-sensitive, and attaching a name no label matches creates that label rather than failing. Configuring `review` for a project that already holds `Review` would therefore leave the project with two near-duplicate labels. To prevent that, the adapter reads the project label catalog at startup, project labels and inherited group labels alike, and rewrites every configured state label to the casing the project already stores. Spell the configured labels however you like; the adapter writes the project's spelling. A configured label the project does not hold yet is not an error: it is created the first time an issue transitions into it.

`handoff_state` and `in_progress_state` also name labels. At transition time the adapter adds the target label and removes the labels of the other configured states in one update request, and checks the labels in the response. A move to a terminal label then closes the issue, and a move to an active label reopens a closed one, each in a request of its own, because GitLab commits a state change before the labels of the same request. A move to the handoff state is refused for a closed issue. The issue carries the labels of exactly one configured state afterward, and labels outside the configured states are never touched. GitLab imposes no transition graph, so any open issue can move to any configured state, and a transition that is already converged issues no write at all. The dispatch-time `in_progress_state` transition runs through this same label swap, so its generic validation rules (must appear in `active_states`, must not collide with `terminal_states` or `handoff_state`) apply unchanged.

**GitLab `query_filter`:** For `kind: gitlab`, `query_filter` is a URL query fragment for GitLab's project issue-list route, not a string predicate or a JSON object. The adapter parses it with `url.ParseQuery` and merges the parameters into candidate polling, so an operator can scope which open issues the agent picks up. Parameters combine with `&`. Every rejection below happens at startup, before the first poll, and `sortie validate` reports the same verdict offline by running the same parser. Rules:

- The adapter rejects the eight keys it owns: `state`, `issue_type`, `order_by`, `sort`, `page`, `per_page`, `pagination`, and `with_labels_details`. Naming any of them fails construction with a configuration error, because overriding one changes correctness rather than scope.
- Every other key MUST be one the project issue-list route honors: `assignee_id`, `assignee_username`, `author_id`, `author_username`, `confidential`, `created_after`, `created_before`, `due_date`, `iids`, `in`, `labels`, `milestone`, `milestone_id`, `my_reaction_emoji`, `scope`, `search`, `updated_after`, and `updated_before`. An unrecognized key fails construction rather than passing through. This is stricter than the Gitea adapter, which warns and forwards, because GitLab silently ignores a parameter it does not recognize: a typo such as `assignee=` for `assignee_username=` would return every open issue, widening the candidate set with no visible signal.
- Negation uses GitLab's `not[...]` hash, accepted for the subset GitLab honors there: `not[assignee_id]`, `not[assignee_username]`, `not[author_id]`, `not[author_username]`, `not[iids]`, `not[labels]`, `not[milestone]`, and `not[milestone_id]`. A `not[...]` key naming anything else is rejected, since the excluded parameters parse without error and then have no effect.
- Repeat an array parameter with the `[]` suffix (`iids[]=3&iids[]=4`). A key without the suffix MUST carry exactly one value: repeating it is rejected, because repeat semantics are not portable across GitLab versions.
- Naming one parameter under two spellings is rejected: `labels` and `labels[]` are the same parameter. `labels` and `not[labels]` are different parameters and MAY both appear.
- A value carrying an empty comma-separated segment (`labels=ready,,urgent`) is rejected.
- `scope` is both adapter-set and operator-settable. The adapter polls with `scope=all`; an operator value replaces it rather than combining with it, which is how a filter narrows polling to, for example, `scope=assigned_to_me`.
- A `labels` value inherits GitLab's server-side semantics: AND across comma-separated names, and case-sensitive matching. A name matching no label returns an empty result rather than dropping the filter, so a misspelling shows up as "no candidates" instead of "every candidate". The adapter still warns at construction for each `labels` name absent from the project catalog, once per distinct name, so the empty result has a stated cause. The warning does not block construction, because an operator may reference a label that does not exist yet. GitLab reads `none` and `any` as wildcards on the non-negated `labels` parameter, so neither is checked against the catalog there.

The filter merges into the open-issue listings that back candidate polling and not into the closed-issue listing used for terminal-state cleanup, so an operator filter never hides a terminal issue from reconciliation. The batched state lookups that reconcile active runs address issues by `iid` and carry no filter either, so a running issue stays visible even after an edit moves it outside the filter.

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

Four prompt-template variables (see [Section 5.2](#52-template-input-variables)) are always empty for this tracker, so a template that reads them renders nothing rather than failing: `{{ .issue.priority }}`, because GitLab issues carry no priority field; `{{ .issue.blocked_by }}`, because Community Edition has no blocking relationship between issues; `{{ .issue.parent }}`, because the issue route exposes no sub-issue relationship; and `{{ .issue.branch_name }}`, because GitLab computes branch names in the UI rather than storing one on the issue. Both `{{ .issue.id }}` and `{{ .issue.identifier }}` are the project-scoped issue number (GitLab's `iid`), the number shown as `#7` in the GitLab UI, never the instance-global issue ID.

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
- All `$VAR` and `${VAR}` references anywhere in the string are then expanded via `os.ExpandEnv`. This applies in any position, not only to pure `$VAR` values.
- Bare strings with no `~` prefix or `$` references are used as-is. Relative roots are allowed but discouraged.

**Per-issue workspace path:** `<workspace.root>/<sanitized_issue_identifier>`

**Workspace key sanitization:** Only `[A-Za-z0-9._-]` are allowed. All other characters in the issue identifier are replaced with `_`.

#### Changing `workspace.root`

> **Warning:** Changing `workspace.root` and restarting the orchestrator will leave the old workspace directory on disk. Sortie's startup cleanup scans only the path that is currently configured, so any directories under the previous root become orphans and accumulate disk space until removed manually.

**Why this happens:** On startup, Sortie lists workspace subdirectory names under `workspace.root`, queries the tracker for their states, and removes any whose issues are in terminal states. Because the scan is anchored to the configured root, a prior root is never consulted.

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

> **Note:** Dynamic changes to `workspace.root` at runtime (without a restart) are safe for in-flight sessions — cleanup for already-running sessions uses the path stored in memory at the time the session was started.

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

> **Note:** `after_create` runs only when Sortie first creates the per-issue workspace directory, so the directory is empty when the clone runs. If `after_create` fails, Sortie removes the directory before the next retry, so a retry also starts from an empty directory. A clone error such as "destination path already exists" or "directory not empty" does not come from this example on the normal path.
>
> Hooks also run with a restricted environment: an allowlist (including `HOME` and `SSH_AUTH_SOCK`) plus `SORTIE_*` variables. Sortie strips any variable outside that set, such as `GIT_SSH_COMMAND`, so an SSH clone must reach its key through the SSH agent (`SSH_AUTH_SOCK`) or through `~/.ssh` via `HOME`, not through a stripped variable. See Section 6.2 for the full allowlist.

| Field           | Type                           | Required | Default  | Dynamic Reload         | Description                                                                          |
| --------------- | ------------------------------ | -------- | -------- | ---------------------- | ------------------------------------------------------------------------------------ |
| `after_create`  | multiline shell script or null | No       | _(none)_ | Future hook executions | Runs only when a workspace directory is **newly created**.                           |
| `before_run`    | multiline shell script or null | No       | _(none)_ | Future hook executions | Runs before each agent attempt, after workspace preparation.                         |
| `after_run`     | multiline shell script or null | No       | _(none)_ | Future hook executions | Runs after each agent attempt (success, failure, timeout, or cancellation).          |
| `before_remove` | multiline shell script or null | No       | _(none)_ | Future hook executions | Runs before workspace deletion, if the directory exists.                             |
| `timeout_ms`    | integer                        | No       | `60000`  | Future hook executions | Timeout in milliseconds for all hooks. A value outside the integer range, positive or negative, fails config load; other non-positive values fall back to the default. |

See [Section 6: Hook Lifecycle Reference](#6-hook-lifecycle-reference) for execution contract, environment variables, and failure semantics.

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
  stop_grace_ms: 5000
  max_retry_backoff_ms: 300000
  max_concurrent_agents_by_state:
    in progress: 3
    to do: 1
```

| Field                            | Type                              | Required                            | Default         | Dynamic Reload                             | Description                                                                                                                                                                      |
| -------------------------------- | --------------------------------- | ----------------------------------- | --------------- | ------------------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `kind`                           | string                            | No                                  | `claude-code`   | Future dispatches                          | Agent adapter identifier. This is the default kind used when no `dispatch.rules` entry (and no `dispatch.default.agent`) overrides it. Built-in adapters: `claude-code`, `copilot-cli`, `codex`, `opencode`, `mock`, and `agent-client-protocol`. Other kinds (for example, HTTP-based adapters) are available only if you register them separately. When a release removes an agent kind, a workflow that still names it in `agent.kind`, `dispatch.default.agent`, or a `dispatch.rules` entry's `agent` loads converted onto the kind that replaces it, together with the settings block the removed kind read. `sortie validate` and the running orchestrator each warn once, and the warning names what was converted, the launch the replacement kind now performs, and the settings that were not carried. `WORKFLOW.md` itself is never rewritten. The conversion is temporary, so change the file to name the replacement kind. |
| `command`                        | string (argument vector) or list of strings | Only when the default kind has no default command | Adapter-defined | Future dispatches                          | Command to launch the agent, for the default kind: `dispatch.default.agent` when set, otherwise `agent.kind`. It applies to adapters that run as a local subprocess (`claude-code`, `copilot-cli`, `codex`, `opencode`, and `agent-client-protocol`). Every one of them except `agent-client-protocol` has a default command of its own, so you can leave `command` out for it; `agent-client-protocol` has none, so it needs `command`. A kind other than the default kind always launches its own default command and never reads this field. Written as a string, the value is split on whitespace into an argument vector when the agent runs locally and run without a shell, so shell syntax is not interpreted and `~` and `$VAR` are not expanded; when `worker.ssh_hosts` sends the agent to a remote host, the string is passed to the remote shell unsplit. Written as a list, element zero is the program and each later element is one argument, passed exactly as written: nothing splits, expands, or quotes it locally, and over SSH each element reaches the remote shell as one word. Use the list form when the program path or an argument contains a space. The list must not be empty, and every element must be a non-empty string. Sortie waits for the agent it starts and talks to it, so a string value ending in `&` detaches the agent and the session cannot work. Adapters that do not start a local process ignore this field. |
| `turn_timeout_ms`                | integer                           | No                                  | `3600000` (1h)  | Future worker attempts                     | Wall-clock bound on a single agent turn, enforced by the orchestrator. Must be positive.                                                                                         |
| `read_timeout_ms`                | integer                           | No                                  | `5000` (5s)     | Future worker attempts                     | Request/response timeout during startup and synchronous operations. A wait that ends only once the model starts answering is the larger of this value and 60 seconds: the Agent Client Protocol adapter's `initialize` wait, since a runtime may establish its credential with its backend before answering, and the `opencode` wait for a turn's first output, since OpenCode prints nothing until the model answers. Neither fails a healthy run at a short configured value. For `codex` and `agent-client-protocol` the timeout also bounds the wait at session start for the agent's first connection to Sortie's tool server: an agent that has not connected by then opens with the tool-server gap declared and a prompt that names no Sortie tools.                                                                                                              |
| `stall_timeout_ms`               | integer                           | No                                  | `300000` (5m)   | **Immediate**, in-flight sessions included | Inactivity timeout: each poll tick stops a session that has emitted no agent event for longer than this, measured from its last event or, before the first, from its start. Set to `0` or negative to **disable** stall detection. While it is on, running rows of the JSON API serve `quiet_at` and `stall_at` from it. |
| `stop_grace_ms`                  | integer                           | No                                  | `5000` (5s)     | Future worker attempts                     | The period an adapter waits, after sending a catchable termination signal, for the agent to exit on its own before it force-terminates the process group. On an SSH worker host the agent gets the same period, rounded up to whole seconds, once its SSH connection closes. Must be positive; overridable through `SORTIE_AGENT_STOP_GRACE_MS`. In `claude-code`, `copilot-cli`, and `opencode`, the same value also bounds a cancelled turn's escalation to a force kill; `mock` launches no process, so it has no such period. The per-session stop deadline derives from this field alone, no longer from `agent.read_timeout_ms`, which shortens that deadline for a deployment that had set `read_timeout_ms` above `20000`. Raising this field also lengthens graceful shutdown by the same amount: a session still stopping when Sortie is asked to shut down is waited for rather than abandoned, and at the default the worker drain alone is 50s. A second Ctrl-C during shutdown ends every remaining shutdown wait at once, so a raised grace never strands the operator without an escape. |
| `max_concurrent_agents`          | integer or string integer         | No                                  | `10`            | **Yes** — affects subsequent dispatch      | Global concurrency limit across all issues.                                                                                                                                      |
| `max_turns`                      | integer                           | No                                  | `20`            | Future dispatches                          | Maximum coding-agent turns per worker session. The worker re-checks tracker state after each turn and starts another turn if the issue is still active, up to this limit.        |
| `max_retry_backoff_ms`           | integer or string integer         | No                                  | `300000` (5m)   | **Yes** — affects future retry scheduling  | Maximum delay cap for exponential backoff on retries. A refusal by a provider usage limit that reports when it resets is the exception: the retry waits for that reset, past this cap, for at most 7 days.                                                                                                                            |
| `max_concurrent_agents_by_state` | map of `state → positive integer` | No                                  | `{}` (empty)    | **Yes** — affects subsequent dispatch      | Per-state concurrency limits. State keys are normalized to lowercase for lookup. An entry outside the integer range, positive or negative, fails config load; other non-positive or non-numeric entries are silently ignored.                                       |
| `max_sessions`                   | integer                           | No                                  | `0` (unlimited) | **Yes** — affects future retry evaluations | Maximum completed worker sessions per issue before the orchestrator stops re-dispatching. Counted from run history. A run that a provider usage limit refused is not counted when the provider reported a future reset time and the run's usage report shows no output tokens. Every other refused run counts, including one refused with no reset time and one with no usage report. `0` disables the budget (unlimited). Must be non-negative. The separate `max_consecutive_absences` governs the consecutive-absence ceiling. Reaching the ceiling also posts one comment on the issue naming the session budget and `agent.max_sessions` as the setting that raises it. |
| `max_tokens`                     | integer                           | No                                  | `0` (unlimited) | **Yes** — stops a run already in flight and affects future retry evaluations | Cumulative per-issue token ceiling, counted in tokens without prompt-cache reads (`total_tokens` less `cache_read_tokens`, floored at zero for each run; cache writes count). The orchestrator sums those tokens across the issue's run history, adds the running session's own, and stops re-dispatching once the sum reaches the limit; reaching the ceiling also stops a run already in flight, on the event loop, as soon as a usage figure carries the sum there. A run whose coding agent reported no token usage contributes nothing to the sum, and a turn whose spend was never fully reported contributes less than it cost; either one makes the ceiling report that it could not be fully evaluated (a warning is logged and the dispatch proceeds). `0` disables the budget (unlimited). Must be non-negative. Reaching the ceiling also posts one comment on the issue naming the token budget and `agent.max_tokens` as the setting that raises it, stating whether a session was stopped in flight. A session that can carry no token figure never reaches this ceiling at all: `sortie validate` reports that combination under the check `agent.kind.no_usage_reporting`, naming the kind, and every dispatch of such a run logs it. A `copilot-cli` run on an SSH worker is such a session, because Sortie reads that kind's token figure from a file on the agent's host and does not read it over SSH. The usage sources belong to the kind, not to the runtime the kind reaches, so a transport kind whose sources do carry figures, driving a runtime that reports none, leaves the ceiling equally unreachable and draws neither of those two signals; each such run instead logs once as it ends, naming the kind and the ceiling it could not be bound by. `token_warning_percent`, below, warns before this ceiling stops a run. |
| `max_cost_usd`                   | number                            | No                                  | `0` (unlimited) | **Yes** – stops a run already in flight and affects future retry evaluations | Per-issue ceiling in US dollars on the issue's estimated cost: the cost recorded with each of the issue's finished runs plus the running session's live estimate, each priced under the rates in force ([Section 4.4](#44-token_rates--cost-estimation)). The figure is Sortie's estimate, not your provider's bill, and can differ from a cost your runtime reports. Once the figure reaches the ceiling, Sortie stops re-dispatching the issue and holds it, and stops a run already in flight on the event loop as soon as a usage figure carries the figure there. The hold records the reason `money_budget`, fires `budget.held`, and posts the budget-hold comment, which names the money budget and `agent.max_cost_usd` as the setting that raises it and states the estimated cost against the ceiling; a run stopped in flight is recorded `budget_stopped` with the error `money ceiling reached`. An issue past more than one ceiling records `money_budget` first, then `token_budget`, then `session_budget`. A run that Sortie cannot fully price (a model no rate layer prices, or a token class with no rate) or whose usage was not fully reported leaves the figure a lower bound; the ceiling counts only what it priced, and while that lower bound is below the ceiling a warning is logged and the dispatch proceeds. The ceiling stops a run at the next usage figure after the spend, so a run can pass it by what the agent spends before that report: about one usage event for a kind whose `usage_arrival` is `incremental`, a whole turn for `turn_end`. Leave headroom below the amount you cannot afford to exceed. A per-kind `token_rates` entry that sets no `cache_read_per_mtok` prices cache reads at the input rate and overstates the figure; set the cache-read rate or use a model entry. `0` disables the ceiling (unlimited). Must be a non-negative, finite number, written as an integer, a decimal, or a quoted string of one. Overridable through `SORTIE_AGENT_MAX_COST_USD`. While the ceiling is above `0`, the `cost_budget` tool result of a running session also carries `used_usd` (the issue's estimated cost), `budget_usd`, `remaining_usd` (`0` once the ceiling is reached), and `used_usd_complete` (`false` when `used_usd` is a lower bound); with the ceiling at `0` the result carries none of them. The ceiling never takes effect for a kind whose usage-reporting declaration resolves to no token usage, which `sortie validate` reports under the check `agent.max_cost_usd.no_usage_reporting`, and it holds no issue while no rate source is available, which it reports under `agent.max_cost_usd.no_rate_source`; both are warnings and dispatch continues ([Section 8](#8-dispatch-preflight-validation)). `agent.token_warning_percent` stays a token threshold and does not apply to the money ceiling. |
| `max_daily_cost_usd`             | number                            | No                                  | `0` (unlimited) | **Yes** – applies on the next poll tick or retry timer fire | Instance ceiling in US dollars on the estimated cost of the current UTC calendar day, across every issue. The spend is the cost recorded with each run that ended in the day, wherever the run started, plus the live estimate of the sessions running now, each priced under the rates in force ([Section 4.4](#44-token_rates--cost-estimation)). A rate change moves the estimate of a running session and never the recorded cost of an ended run. The figure is Sortie's estimate, not your provider's bill. Once the spend reaches the ceiling, Sortie starts no new agent session for any issue: no candidate dispatches, no fired retry starts, and no pull request reaction continuation starts. A retry or continuation that comes due during the hold stays scheduled with its attempt unchanged and is checked again one `polling.interval_ms` later. A session already running, with its later turns and continuations inside its run, finishes, so the day's final spend can pass the ceiling by what those sessions spend. Leave headroom below the amount you cannot afford to exceed. Polling, reconciliation, reaction checks, reaction triage, and the per-issue ceilings keep running; a triage command that answers `dispatch-agent` schedules a continuation, which the hold keeps. A run that Sortie cannot fully price (a model no rate layer prices, or a token class with no rate) or whose usage was not fully reported counts only what Sortie priced and leaves the spend a lower bound. A lower bound below the ceiling does not hold dispatch: Sortie logs one warning per window and dispatches. The day runs from 00:00 UTC to the next 00:00 UTC. The hold ends with no operator action when the day resets, or when a reload raises the setting above the day's spend or sets it to `0`, and held retries then fire within one `polling.interval_ms`. Under `sortie --once`, a candidate or retry the ceiling holds ends that issue's work as not succeeded, and the retry is not checked again. The dashboard's "today" starts at midnight in the browser's time zone, so for a viewer off UTC its today total and the daily window cover different spans. `0` disables the ceiling (unlimited). Must be a non-negative, finite number, written as an integer, a decimal, or a quoted string of one. Overridable through `SORTIE_AGENT_MAX_DAILY_COST_USD`. It can be set without the other instance ceilings, and `agent.max_cost_usd` stays independent of it. `sortie validate` reports under `agent.max_daily_cost_usd.no_usage_reporting` a kind that reports no token usage, and under `agent.max_daily_cost_usd.no_rate_source` the absence of any price source; both are warnings and dispatch continues ([Section 8](#8-dispatch-preflight-validation)). Overview shows the day's spend against the ceiling with its reset time in UTC, the attention list shows the hold, and `GET /api/v1/state` serves the window under `spend_ceiling`. |
| `max_weekly_cost_usd`            | number                            | No                                  | `0` (unlimited) | **Yes** – applies on the next poll tick or retry timer fire | Instance ceiling in US dollars on the estimated cost of the current UTC week, from Monday 00:00 UTC to the next Monday 00:00 UTC, across every issue. The spend is the cost recorded with each run that ended in the week, wherever the run started, plus the live estimate of the sessions running now, each priced under the rates in force ([Section 4.4](#44-token_rates--cost-estimation)). A rate change moves the estimate of a running session and never the recorded cost of an ended run. The figure is Sortie's estimate, not your provider's bill. Once the spend reaches the ceiling, Sortie starts no new agent session for any issue: no candidate dispatches, no fired retry starts, and no pull request reaction continuation starts. A retry or continuation that comes due during the hold stays scheduled with its attempt unchanged and is checked again one `polling.interval_ms` later. A session already running, with its later turns and continuations inside its run, finishes, so the week's final spend can pass the ceiling by what those sessions spend. Leave headroom below the amount you cannot afford to exceed. Polling, reconciliation, reaction checks, reaction triage, and the per-issue ceilings keep running; a triage command that answers `dispatch-agent` schedules a continuation, which the hold keeps. A run that Sortie cannot fully price (a model no rate layer prices, or a token class with no rate) or whose usage was not fully reported counts only what Sortie priced and leaves the spend a lower bound. A lower bound below the ceiling does not hold dispatch: Sortie logs one warning per window and dispatches. The week runs from 00:00 UTC on Monday to 00:00 UTC on the next Monday. A finished run counts in the week that holds its completion time, so a run that crosses Monday 00:00 UTC counts in full in the new week. The hold ends with no operator action when the week resets, or when a reload raises the setting above the week's spend or sets it to `0`, and held retries then fire within one `polling.interval_ms`. Under `sortie --once`, a candidate or retry the ceiling holds ends that issue's work as not succeeded, and the retry is not checked again. The dashboard's "today" starts at midnight in the browser's time zone, so for a viewer off UTC its today total and the daily window cover different spans. `0` disables the ceiling (unlimited). Must be a non-negative, finite number, written as an integer, a decimal, or a quoted string of one. Overridable through `SORTIE_AGENT_MAX_WEEKLY_COST_USD`. It can be set without the other instance ceilings, and `agent.max_cost_usd` stays independent of it. `sortie validate` reports under `agent.max_weekly_cost_usd.no_usage_reporting` a kind that reports no token usage, and under `agent.max_weekly_cost_usd.no_rate_source` the absence of any price source; both are warnings and dispatch continues ([Section 8](#8-dispatch-preflight-validation)). Overview shows the week's spend against the ceiling with its reset time in UTC, the attention list shows the hold, and `GET /api/v1/state` serves the window under `spend_ceiling`. |
| `max_monthly_cost_usd`           | number                            | No                                  | `0` (unlimited) | **Yes** – applies on the next poll tick or retry timer fire | Instance ceiling in US dollars on the estimated cost of the current UTC calendar month, across every issue. The spend is the cost recorded with each run that ended in the month, wherever the run started, plus the live estimate of the sessions running now, each priced under the rates in force ([Section 4.4](#44-token_rates--cost-estimation)). A rate change moves the estimate of a running session and never the recorded cost of an ended run. The figure is Sortie's estimate, not your provider's bill. Once the spend reaches the ceiling, Sortie starts no new agent session for any issue: no candidate dispatches, no fired retry starts, and no pull request reaction continuation starts. A retry or continuation that comes due during the hold stays scheduled with its attempt unchanged and is checked again one `polling.interval_ms` later. A session already running, with its later turns and continuations inside its run, finishes, so the month's final spend can pass the ceiling by what those sessions spend. Leave headroom below the amount you cannot afford to exceed. Polling, reconciliation, reaction checks, reaction triage, and the per-issue ceilings keep running; a triage command that answers `dispatch-agent` schedules a continuation, which the hold keeps. A run that Sortie cannot fully price (a model no rate layer prices, or a token class with no rate) or whose usage was not fully reported counts only what Sortie priced and leaves the spend a lower bound. A lower bound below the ceiling does not hold dispatch: Sortie logs one warning per window and dispatches. The month runs from 00:00 UTC on its 1st to 00:00 UTC on the 1st of the next month. The hold ends with no operator action when the month resets, or when a reload raises the setting above the month's spend or sets it to `0`, and held retries then fire within one `polling.interval_ms`. Under `sortie --once`, a candidate or retry the ceiling holds ends that issue's work as not succeeded, and the retry is not checked again. The dashboard's "today" starts at midnight in the browser's time zone, so for a viewer off UTC its today total and the daily window cover different spans. `0` disables the ceiling (unlimited). Must be a non-negative, finite number, written as an integer, a decimal, or a quoted string of one. Overridable through `SORTIE_AGENT_MAX_MONTHLY_COST_USD`. It can be set without the other instance ceilings, and `agent.max_cost_usd` stays independent of it. `sortie validate` reports under `agent.max_monthly_cost_usd.no_usage_reporting` a kind that reports no token usage, and under `agent.max_monthly_cost_usd.no_rate_source` the absence of any price source; both are warnings and dispatch continues ([Section 8](#8-dispatch-preflight-validation)). Overview shows the month's spend against the ceiling with its reset time in UTC, the attention list shows the hold, and `GET /api/v1/state` serves the window under `spend_ceiling`. |
| `token_warning_percent`          | integer                           | No                                  | `0` (off)       | **Yes** — an unwarned run in flight is evaluated against the new value from its next usage figure; future dispatches use it from the start | Token warning threshold, as a percentage of `agent.max_tokens`, `0` to `99`. The threshold in tokens is that percentage of the ceiling, rounded up; it is never validated against `agent.max_tokens`, so lowering the ceiling can never make this field reject a reload. Evaluated on the event loop against the same live per-issue figure the ceiling reads, ahead of the ceiling's own evaluation. Once a run's figure reaches the threshold, one warning is logged for that run and the running session's `cost_budget` tool result reports the condition, so the agent can wrap up or hand off before the ceiling stops the run. `0` disables the threshold; it also has no effect while `agent.max_tokens` is `0`, which `sortie validate` reports under the check `ineffective_setting`. Overridable through `SORTIE_AGENT_TOKEN_WARNING_PERCENT`. |
| `max_consecutive_absences`       | integer                           | No                                  | `3`             | **Yes** — affects future worker exits, retry evaluations, and poll-tick park sweeps | Bounds how many runs in a row may be observed to have produced no evidence of work before the issue is parked. Any run that produces evidence of work resets the count to zero. `0` and negative values are rejected as a configuration error. The separate `max_sessions` governs the total per-issue session budget. |

**Orchestrator vs adapter fields:** The fields above are consumed by the orchestrator for scheduling, concurrency, and retry decisions. They are **not** passed through to the agent adapter. Adapter-specific configuration uses separate pass-through blocks — see [Section 4.5](#45-adapter-specific-pass-through-config).

**Credential verification:** Before the working session's first turn, a worker run proves the runtime completes one model request with its credential, on every agent kind and every remote host, unless Sortie proved the same agent setup within the last 15 minutes. The setup is the agent kind, its command, its SSH host, the names of the environment variables carried to that host, and the kind's settings block; the workspace is not part of it. Every successful turn renews the proof. A proof is dropped when a verification fails, when a run ends in an error after none of its turns succeeded, when a run's error says the credential was refused, and when any part of the setup changes, so a reload that edits the command or the settings starts a new proof. Sortie keeps proofs in memory, so a restart forgets them. A credential revoked inside the window fails the first turn of the next run, which drops the proof, and the retry verifies and names the credential. A workspace whose own project configuration selects another provider or model behaves the same way while another workspace's proof stands. There is nothing to configure. A run that performs the step pays one model request at the configured model's rate and one short runtime start, and the request counts in the run's token usage, API request count, and cost estimate. A run that skips the step pays neither, and the log says `agent credential verification skipped`.

The verification request drops the agent's instructions, tools, and skills wherever the runtime lets a single launch drop them without changing the credential, model, provider, or endpoint, and every other setting reaches it unchanged. What the request still carries:

- `claude-code`: nothing of the agent's context. Instruction files, memory, plugins, skills, custom agents, tools, and `claude-code.system_prompt` are left out, and a short prompt replaces the system prompt. The login, the model, and the `env` block of Claude Code's own settings still apply.
- `copilot-cli`: the built-in system prompt and the read-only `view` tool. Custom instruction files and the `copilot-cli.available_tools` and `copilot-cli.excluded_tools` lists are left out. `copilot-cli.agent` still applies, because a profile can set the model.
- `codex`: the instructions in the Codex home and the built-in tools that have no switch. Project instruction files, web search, the shell and other feature tools, every MCP server in the configuration, and `codex.personality` are left out, and a short prompt replaces the base instructions. A Codex build that does not know one of the configuration keys ignores it and saves less.
- `opencode`: instruction files and the built-in system prompt. Every tool is denied, so the request has no tool definitions, calls no tool, and writes nothing into the issue workspace; `opencode.allowed_tools`, `opencode.denied_tools`, and the `external_directory` default do not apply to it.
- `agent-client-protocol`: everything, except on a local launch whose command names Gemini CLI, Copilot CLI, OpenCode, or Kiro CLI. Gemini CLI then gets a short system prompt from a temporary file Sortie removes when the session ends, and keeps its tools and instructions; OpenCode gets the reductions listed above for its own kind, and Copilot CLI gets the read-only `view` tool only, because it keeps loading its custom instruction files over the protocol. Kiro CLI runs under an agent that has no tools, defined in a temporary directory Sortie removes when the session ends, and keeps its instruction files; it gets this only when its command sets `--model`, because selecting an agent replaces your default agent, which could carry the model. A remote launch, a command that names another runtime, and a launch that already sets the switch itself (`--available-tools`, `--no-custom-instructions`, or `--agent` in the command, `GEMINI_SYSTEM_MD`, `OPENCODE_CONFIG_CONTENT`, or `KIRO_AGENT_CONFIG_DIR` in the environment) carry everything, because replacing your own setting could change the model, provider, or credential.

A runtime build that rejects one of these switches and exits before it responds is verified once more with its full context, and the log carries a warning with the first failure; Claude Code 2.1.169 is the first release that saves, and an older one is always verified with its full context. A credential that Claude Code, Codex, or a protocol runtime refuses during a working turn is reported as `credential_unverified`, as it is at session start.

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
- If the value resolves to empty after environment expansion (e.g., unset `$VAR`), startup fails.

**Runtime behavior:** `db_path` is read once at startup to open the database connection. Dynamic reloads update the in-memory config value but have no effect on the already-open connection. A restart is required to change the database file.

> **Migration note:** Changing `db_path` and restarting causes Sortie to open (or create) the new file with a fresh schema. Retry queues and run history from the previous database file are **not** migrated automatically. If you need to preserve state, copy the old `.sortie.db` to the new path before restarting.

---

### 2.8 `self_review` — Self-Review Configuration

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

The self-review section configures an optional post-coding verification and iterative review phase. When enabled, the orchestrator runs verification commands after the coding turn loop, generates a workspace diff, and presents both to the agent for a structured verdict. If the agent identifies issues ("iterate"), a fix turn runs and the cycle repeats up to `max_iterations`.

| Key                        | Type     | Required           | Default    | Description                                                       |
| -------------------------- | -------- | ------------------ | ---------- | ----------------------------------------------------------------- |
| `enabled`                  | boolean  | No                 | `false`    | Activates the self-review loop.                                   |
| `max_iterations`           | integer  | No                 | `3`        | Hard cap on review iterations. Range: 1–10.                       |
| `verification_commands`    | [string] | When `enabled: true` | —        | Shell commands to run during each review iteration.               |
| `verification_timeout_ms`  | integer  | No                 | `120000`   | Per-command timeout in milliseconds.                              |
| `max_diff_bytes`           | integer  | No                 | `102400`   | Max bytes of diff to include in review prompt.                    |
| `reviewer`                 | string   | No                 | `"same"`   | Which agent reviews. Only `"same"` in v1.                         |

**Turn Accounting:** `max_iterations: N` means up to `2N − 1` additional agent turns (N review turns + N−1 fix turns). For the default `max_iterations: 3`, this is up to 5 additional turns beyond the coding turn loop. Plan token budgets accordingly.

**Process lifetime:** Each verification command's process group (its Job Object on Windows) is terminated when it exits, times out, or is cancelled, resending that termination until no process of the group or job is still alive or a 2-second bound elapses, and its exit status alone decides whether it passed. On Windows, a command whose Job Object could not be created or assigned still runs, with the failure logged, and that teardown then reaches only the command itself. When a command exits on its own and its termination reached a process it left running, Sortie logs one INFO record, `leftover processes terminated after the command exited`, carrying `command`; a termination that cannot confirm that no process of the group or job is still alive once the bound elapses logs the WARN record `subprocess group termination failed after the launch returned`, which names the launch by `command` as the shell's executable name, not as the configured command. On Windows, a Job Object member that Sortie cannot open or query counts as still alive, not as gone.

**Validation rules:**

- When `enabled` is `true`, `verification_commands` must be a non-empty list. An empty list is rejected with a configuration error.
- `max_iterations` must be in the range [1, 10]. Values outside this range are rejected with a configuration error.
- `reviewer` must be `"same"`. Other values are reserved for future use and are rejected with a configuration error.
- When `enabled` is `false` or the section is absent, all other fields are ignored and the self-review phase adds zero overhead.

---

### 2.9 `reactions` — Reaction-Based Feedback Loops

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

The `reactions` section configures feedback loops that respond to external events (CI failures, PR review comments) by dispatching continuation turns to the agent. Each key under `reactions` identifies a reaction kind.

**Common fields** (shared across all reaction kinds):

| Field              | Type    | Required              | Default       | Dynamic Reload    | Description                                                                                     |
| ------------------ | ------- | --------------------- | ------------- | ----------------- | ----------------------------------------------------------------------------------------------- |
| `provider`         | string  | **Yes** (to activate) | _(absent)_    | Requires restart | Adapter identifier (e.g. `github`). When absent or empty, the reaction kind is disabled.         |
| `max_retries`      | integer | No                    | `2`           | Requires restart | Maximum fix continuation dispatches per issue before escalation. Must be non-negative.           |
| `escalation`       | string  | No                    | `label`       | Requires restart | Action taken when the kind hands the subject to a person, either because retries are exhausted or because a `triage` command answered `escalate`. Valid: `"label"`, `"comment"` (deprecated), `"none"`. See "Escalation postures" below. |
| `escalation_label` | string  | No                    | `needs-human` | Requires restart | Label applied when `escalation` is `"label"`.                                                    |

**Reload behavior:** every field of every reaction kind is read once when the orchestrator is constructed and is not rebuilt on a `WORKFLOW.md` reload, so a change takes effect only on the next restart. `reactions.ci_failure` is the single exception: the orchestrator re-reads `max_retries`, `escalation`, `escalation_label`, and `watch_window_ms` from the reloaded config on each tick. The `escalation` value of every other kind stays the value read at startup, and so does the comment that `escalation: comment` posts for it, whatever a reload changes in `notifications`. Its `max_log_lines` and its `triage` block still require a restart: the CI provider is constructed once at process start, and the triage configuration is frozen at construction for every kind that offers it, so a script or timeout changed mid-run cannot apply one configuration's timeout to another configuration's script.

**Escalation postures:** whenever a kind hands its subject to a person, it emits one escalation event, whatever `escalation` says: `escalation.ci_failure`, `escalation.review_comments`, `escalation.bot_review`, `escalation.merge_conflicts`, `escalation.auto_merge`, or `escalation.merge_completion`. A destination in [`notifications`](#211-notifications-operator-notification-backends) receives the event when its `events` list names it. The value of `escalation` decides whether the label is applied and whether the issue gets a comment without any `notifications` entry:

| Value | Label | Comment on the issue |
| ----- | ----- | -------------------- |
| `label` (default) | Applied. | None, unless a `tracker_comment` entry lists the event. |
| `comment` (deprecated) | None. | Posted, as before. A deprecation warning names the replacement. |
| `none` | None. | None, unless a `tracker_comment` entry lists the event. |

To get a comment on the issue and no label, write `escalation: none` and list the event in a `tracker_comment` entry. A reaction block that omits `escalation` resolves to `label`, `auto_merge` included, and draws no warning.

**Escalation recurrence:** an escalation stands until a fact Sortie observes ends it, and it applies its action once for the subject it escalated. While an escalation stands, the reaction dispatches nothing, applies no second action, and keeps watching the pull request, past `watch_window_ms` and across a restart. `escalation: label` is idempotent (re-applying a present label is a no-op); a comment on the issue, whether it comes from `escalation: comment` or from a `tracker_comment` subscription to the escalation event, posts a new comment each time an escalation applies its action.

A kind escalates in these cases: its own budget is exhausted, a `triage` command answers `escalate`, the forge refuses the merge call of `auto_merge` or the tracker refuses the issue move of `merge_completion` with an authentication or payload error, or, for `merge_completion`, the forge reports a merged pull request without a merge commit for thirty minutes. For `review_comments` and `bot_review`, an exhausted budget fires the escalation only together with a new comment that no earlier run of the issue was given (for `bot_review`, a comment on the code).

The subject is what the escalation judged: the pull request head for `ci_failure` and `merge_conflicts`, the set of comments for `review_comments` and `bot_review`, the merge call for `auto_merge`, and the merge commit for `merge_completion`. When an escalation ends and one for the same subject begins again, Sortie records the new time and applies no second action, so a head gets one escalation action however often its escalation ends and begins again. A triage escalation is not re-posted for a subject already escalated.

An escalation ends when any of these holds, and on no other fact:

- The pull request merges, closes, or no longer exists. For `merge_completion` the pull request has already merged, so only a pull request that no longer exists ends it.
- The issue reaches a terminal state.
- A person answers it: they move the issue out of `tracker.handoff_state`, or release its park so Sortie runs the issue again. This ends the escalation of every kind and drops its attempt counter, so the next run starts the reaction with a fresh budget. Sortie acts on this only when no run, retry, or park of its own holds the issue, and only after a direct read of the issue shows the new state; an issue the tracker reads with no state ends the escalation at the second reading in a row. Without `tracker.handoff_state` set, no state marks a person's answer.
- A fact of the kind itself, in the table below.

| Kind | The escalation also ends when |
| ---- | ----------------------------- |
| `ci_failure` | A check of the pull request passes, a new commit lands on it, or `max_retries` rises above the CI fixes used. A later escalation of the same head records a new time and applies no second action; only `max_retries: 0` and a triage command's `escalate` answer escalate a head again after a passing check. |
| `review_comments`, `bot_review` | For a budget escalation, no comment is left that no run was given (for `bot_review`, on the code). For a triage escalation, the comments change. A comment that arrives while a budget escalation stands joins it. A restart with `max_continuation_turns` above the turns used also ends a budget escalation. |
| `merge_conflicts` | A check finds no merge conflict, or a new commit brings a different conflict. A restart with `max_retries` above the rebases used also ends a budget escalation. |
| `auto_merge` | A restart with `max_retries` above the merge calls made, or at `0`. |
| `merge_completion` | For a missing merge commit, the forge reports the commit. A restart with `max_retries` above the failed moves used also ends a budget escalation. |

Prefer `label` for kinds that may escalate often, and list the event in a `tracker_comment` entry only when you want each escalation on the issue.

**Release on terminal state:** each reconcile pass reads tracker state for every running issue and for every issue holding a pending reaction entry, whether or not a worker is still running for it. When the tracker reports an issue in a state from `tracker.terminal_states`, the pass releases that issue's pending reaction entries, its reaction attempt counters, its pending retry, and its dispatch claim. Polling for that issue stops on the same pass. The release deletes the stored escalation records of the issue, and leaves the `reaction_fingerprints` rows and the stored record of the review comments each run was given untouched.

Remaining keys within a kind sub-object are kind-specific and collected into an `Extra` map.

**SCM and CI provider selection.** The reaction `provider` field names a registered SCM adapter or CI provider kind: `github`, `gitea`, or `gitlab`. The reaction kinds are provider-agnostic. Setting `provider: gitea` activates the Gitea adapter for that reaction. Its `endpoint`, `api_key`, and `project` come from the top-level `gitea:` pass-through block ([Section 4.5](#45-adapter-specific-pass-through-config)); when `tracker.kind` is also `gitea`, any of the three left unset in that block falls back to the matching `tracker:` value. A Gitea reaction can therefore pair with a non-Gitea tracker, as long as the `gitea:` block supplies the credentials, including the instance `endpoint` Gitea always requires. Setting `provider: gitlab` activates the GitLab adapter for that reaction and resolves the same three fields from the top-level `gitlab:` block by the same rule. `endpoint` is optional there, because the GitLab adapter defaults it to `https://gitlab.com`; only a self-managed instance sets one. The GitLab SCM adapter ignores `project` and takes the owner and repository from the pull request metadata on every call, while the GitLab CI provider requires `project`, so a GitLab `ci_failure` reaction paired with a non-GitLab tracker MUST set it in the `gitlab:` block. Every active SCM reaction in one workflow MUST name the same provider.

#### Triage command (`triage`)

An optional operator-owned command that runs in the issue workspace once the reaction has found a new subject and is about to dispatch an agent continuation. The command answers `handled`, `dispatch-agent`, or `escalate`, so deterministic work is resolved without spending an agent session, a continuation attempt, or a token budget.

The block is recognized under exactly four reaction kinds: `ci_failure`, `review_comments`, `bot_review`, and `merge_conflicts`. `label_commands`, `auto_merge`, and `merge_completion` do not offer it, and a `triage` block under any of them, or under any other key of `reactions`, is a configuration error that `sortie validate` reports offline. `auto_merge` and `merge_completion` never dispatch an agent, so a pre-dispatch gate has nothing to gate there; the label commands carry no `escalation` field, so one of the three dispositions would be undefined for them.

| Field        | Type    | Required | Default | Dynamic Reload   | Description                                                                                                                              |
| ------------ | ------- | -------- | ------- | ---------------- | ---------------------------------------------------------------------------------------------------------------------------------------- |
| `script`     | string  | **Yes**  | _(none)_ | Requires restart | Shell script body, run the same way a workspace hook is. Must be a non-blank string.                                                     |
| `timeout_ms` | integer | No       | `60000` | Requires restart | Bounds one triage run. Must lie in the closed range `1` to `600000`. The ceiling sits below the smallest default `watch_window_ms`, so an entry whose triage run hangs still ages out. |

```yaml
reactions:
  merge_conflicts:
    provider: github
    max_retries: 1
    escalation: label
    escalation_label: needs-human
    triage:
      script: |
        ./scripts/merge-conflict-triage.sh
      timeout_ms: 120000
```

**Execution environment.** The command runs with the per-issue workspace directory as its working directory, through the same machinery as `hooks.before_run`: the same restricted environment, the termination of its whole process group when it exits, times out, or is cancelled, and the same 8 KiB captured output tail. It receives the four variables every hook receives (`SORTIE_ISSUE_ID`, `SORTIE_ISSUE_IDENTIFIER`, `SORTIE_WORKSPACE`, `SORTIE_ATTEMPT`), `SORTIE_SSH_HOST` when a host preference is set, and three of its own:

| Variable                | Value                                                                                                    |
| ----------------------- | -------------------------------------------------------------------------------------------------------- |
| `SORTIE_REACTION_KIND`  | The runtime kind discriminator: `ci`, `review`, `bot-review`, or `merge-conflict`.                        |
| `SORTIE_REACTION_INPUT` | Absolute path to a readable JSON document describing the subject.                                        |
| `SORTIE_REACTION_RESULT` | Absolute path the command writes its answer to. The file does not exist when the command starts.         |

The command never creates the workspace directory. A workspace that does not exist ends the run before a subprocess starts, and the reaction dispatches the agent exactly as it would have without the block.

**Input document.** Both paths sit in a fresh temporary directory created for the run and removed when it returns. Externally authored text, including review comment bodies, CI check names, and branch names, reaches the command only inside the input file, never through an environment variable or a shell word.

```json
{
  "schema_version": 1,
  "reaction_kind": "merge-conflict",
  "issue": { "id": "10432", "identifier": "MT-649", "display_id": "MT-649" },
  "attempt": 3,
  "workspace": "/var/sortie/workspaces/MT-649",
  "fingerprint": "9f2c...",
  "attempts_used": 0,
  "max_attempts": 1,
  "subject": { "pr_number": 128, "branch": "sortie/MT-649",
               "head_sha": "abc123", "base": "main" }
}
```

`subject` is the same value the continuation prompt template receives for that kind, so the command sees what the agent would have seen. For `review` and `bot-review` it is a JSON array of comment objects rather than a single object. `attempts_used` is the kind's continuation counter at the moment the run starts, and `max_attempts` is `max_continuation_turns` for `review` and `bot_review` and `max_retries` for `ci_failure` and `merge_conflicts`.

**Result document.** The command writes one JSON object to the path in `SORTIE_REACTION_RESULT`. Unknown keys are ignored, so a later extension can add fields without breaking an existing script.

```json
{ "disposition": "handled" }
```

| Disposition      | Meaning                                                                                                                                      |
| ---------------- | ---------------------------------------------------------------------------------------------------------------------------------------------- |
| `handled`        | The command resolved the subject. The orchestrator marks the reaction fingerprint dispatched and keeps watching, spending no attempt, scheduling no continuation, and writing nothing to the tracker. |
| `dispatch-agent` | The command wants the agent turn the reaction would have scheduled anyway. Every counter, fingerprint, and entry is left exactly as it would have been with no `triage` block. |
| `escalate`       | The command wants a person. The orchestrator marks the fingerprint dispatched and applies the kind's configured `escalation`, with copy stating that a triage command asked for a person rather than that a budget was exhausted. |

Exit code 0 together with a well-formed result file naming one of the three values is the only path to an answer other than `dispatch-agent`. Every other outcome logs one warning record naming the reason and falls back to `dispatch-agent`: a missing or non-directory workspace, a failed start, a timeout, a non-zero exit, a missing, oversized (over 64 KiB), unreadable, or malformed result file, and an unrecognized `disposition` value. A non-zero exit is never honored, even when the result file holds a valid answer. A broken script therefore costs one log record and one agent turn, never a stranded reaction.

**Recorded answers.** Sortie records every answer it applies, once per run, for the reaction kind and the subject it answered. A bot review answered `handled` reads as handled on the issue page, the Pipeline, and the status sentence, also after a restart, until the comment set changes. Sortie never replays a recorded answer: it only reads the record back, and a subject whose deduplication mark never landed is triaged again.

**Timing and concurrency.** The command runs asynchronously: the pass that starts it re-enqueues its entry and makes no further provider call for it, and a later pass reads the answer. Worst-case latency from the command returning to the answer being applied is one poll interval (`polling.interval_ms`, default 30 seconds). Runs in flight are capped at `max(agent.max_concurrent_agents, 1)` across every issue and kind; an entry that finds the cap reached starts nothing and is reconsidered on the next tick. The cap adds to agent concurrency rather than sharing it, so a host configured for N agents can run N agent processes and N triage subprocesses at the same time.

**Three obligations on the script.** Each is load-bearing. A script written without them works in the common case and produces an incident in the uncommon one.

1. **It MUST tolerate being killed at any instruction.** The orchestrator kills the script and its process group when `timeout_ms` elapses, when shutdown drains in-flight runs, and whenever a pass computes a different fingerprint for the subject. A script that force-pushes, rewrites history, or leaves a repository mid-rebase when killed turns a routine cancellation into an operator incident. The orchestrator also ends every process the script leaves in its process group when the script exits on its own, resending that termination until no process of the group is still alive or a 2-second bound elapses, and logging one INFO record, `leftover processes terminated after the command exited`, carrying `reaction_kind`; a termination that cannot confirm that no process of the group is still alive once the bound elapses is logged as a warning instead.
2. **It MUST be idempotent for a given subject.** A cancelled run, a run whose outcome is discarded because the fingerprint moved, and every run in flight or finished at restart are each followed by a fresh run for the same subject. A restart is the ordinary case: runs in flight are runtime-only, and recorded answers are never replayed, so a subject whose durable deduplication row never landed is triaged again from scratch.
3. **`handled` is a terminal claim about that subject, not a hint.** Nothing re-checks it. A script that answers `handled` without resolving the subject suppresses both the agent continuation and the escalation for that fingerprint until the fingerprint moves or the kind's watch window elapses, which is 24 hours by default for `ci_failure` and 30 minutes for the other three. The watch window bounds the claim only while the fingerprint stands, and on `ci_failure` the two clauses are not independent: that kind's window is measured from the last recorded head, and every head change records a new one, so a script that pushes restarts the window its own answer would otherwise age out against. A `ci_failure` script that answers `handled` on each of a succession of heads it pushed itself is bounded by nothing: it spends no agent turn, fires no escalation, and the entry never ages out.

#### Reaction kind: `ci_failure`

Configures the CI failure feedback loop. The orchestrator resolves the pull request's current head on each pass and polls CI status for that head, dispatching continuation turns when CI fails on it.

Its `provider` must match the provider of every other active SCM reaction.

Additional fields (via Extra):

| Field             | Type    | Default      | Dynamic Reload | Description                                                                                                             |
| ----------------- | ------- | ------------ | -------------- | ------------------------------------------------------------------------------------------------------------------------ |
| `max_log_lines`   | integer | `50`         | Requires restart | Maximum log lines of the CI excerpt injected into the prompt: the failing step's opening lines and last lines, or the end of the job log when that step cannot be found. Up to three extra lines starting with `[sortie]` come on top, so the excerpt holds at most this value plus three lines. `0` disables. Must be non-negative. |
| `watch_window_ms` | integer | `86400000`   | Every tick        | Bounds the age of a pending entry that holds no standing escalation, measured from the last recorded head. `0` removes the clock bound. Must be non-negative and must not exceed `9223372036854`. |

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

PR review comment routing. When configured, the orchestrator polls for human `CHANGES_REQUESTED` review comments on Sortie-created PRs and dispatches continuation turns so the agent can address the feedback.

Additional fields (via Extra):

| Field                    | Type    | Default  | Dynamic Reload    | Description                                                                                               |
| ------------------------ | ------- | -------- | ----------------- | --------------------------------------------------------------------------------------------------------- |
| `poll_interval_ms`       | integer | `120000` | Requires restart | Polling interval for review comments. Minimum: `30000` (30 sec).                                           |
| `debounce_ms`            | integer | `60000`  | Requires restart | Debounce window after the last detected comment before dispatching. Must be non-negative.                  |
| `max_continuation_turns` | integer | `3`      | Requires restart | Maximum review-fix continuation dispatches per issue before escalation. Must be positive.                  |
| `watch_window_ms`        | integer | `1800000` | Requires restart | Bounds a pending entry's age, measured from the entry's creation or from the last time it waited for the issue's retry slot. `0` removes the bound. Must be non-negative and must not exceed `9223372036854`. A watch whose latest check found the pull request waiting on a required approval outlives the window (see Required approval below). |

**Activation:** The `reactions.review_comments` block is active when `provider` is present and non-empty. Agent-created PRs MUST write `pr_number`, `owner`, and `repo` to `.sortie/scm.json` in the workspace for review polling to activate.

**SCM coordinates:** The `owner` and `repo` values are sourced from `.sortie/scm.json` (written by the agent), not from the tracker project configuration. This decouples SCM repository identity from the tracker project identifier.

**Recovery timestamp:** Hooks that push commits or create PRs SHOULD write `pushed_at` to `.sortie/scm.json` when review or CI reaction recovery is enabled. Startup recovery uses `pushed_at` to decide whether handoff-stage reaction work is fresh. If `pushed_at` is absent, recovery falls back to `run_history.completed_at`, so long-lived PRs can age out based on the agent completion time instead of the latest push time.

**Debounce behavior:** When review comments are detected but the newest comment timestamp is within the debounce window, dispatch is deferred. This ensures the reviewer has finished their full review before the agent starts addressing comments.

**Fingerprint dedup:** The orchestrator computes a SHA-256 fingerprint from sorted non-outdated comment IDs. If the fingerprint has not changed since the last dispatch, no new continuation is triggered. Fingerprints are persisted across restarts in the `reaction_fingerprints` SQLite table.

**Required approval:** When a check finds no comment to answer and the forge reports that the branch rules still need an approval, Sortie marks the review as waiting on approval (`last_check.awaiting_approval` on the pending reaction) and keeps watching past `watch_window_ms` until the approval arrives, the pull request merges or closes, a reviewer requests changes, or the tracker issue reaches a terminal state. `watch_ends_at` is `null` while the watch is held, and the dashboard and the status sentence show the wait. Each check during the hold reads the pull request and the review decision, so a held pull request costs API calls at every poll interval for as long as the approval takes; a longer `poll_interval_ms` lowers the cost. A pull request with no approval rule, or with a reviewer requested but no rule, is not waiting on approval and its watch ends at the window. A check that cannot read the review decision keeps the previous reading. Without `review_comments` in the workflow, no review decision is read and nothing shows a wait on approval.

**Comments already given:** A continuation turn starts only when the pull request holds a comment that no earlier run of the issue was given, and it carries every remaining comment, the ones the agent already has included. A set that only lost members, because a fix made some comments outdated or a review was dismissed, starts no turn, spends none of `max_continuation_turns`, and escalates nothing, and neither does a set that comes back after a larger one. The record of what each run was given is stored in the database, so a restart does not start a turn that only repeats it. A comment the agent received but left unaddressed is not sent again when its siblings go away; it comes back with the next new comment, which for this kind is a comment in a new review that requests changes. A run is given the comments its first prompt shows: a template that never prints `review_comments` hands none over. A new run of an issue, such as after a reopen, is given the comments no run was given when the pull request already exists in its workspace, this kind has no `triage` block, and the template shows them as an added section, for example an `{{ if .review_comments }}` block, with the rest of the prompt unchanged. A template that switches to different text when `review_comments` is set keeps its normal prompt for a new run.

**Bot exclusion:** Human-loop selection drops a comment whose author the platform reports as an automated bot account, and separately drops a comment whose author matches `reactions.bot_review.bot_usernames`. The allowlist half takes effect only while `reactions.bot_review` is configured with a provider, because that is the only condition under which the allowlist is built; a deployment running `review_comments` alone sees no allowlist exclusion.

**Escalation:** When `max_continuation_turns` is exhausted, the orchestrator escalates when a review comment arrives that no earlier run was given. It applies the configured escalation (`label`, `comment`, or `none`) and releases the claim unless a worker runs for the issue, in which case that worker's exit decides the claim. Comments that a run already received, including ones the last turn left in place, do not escalate, and neither does a poll that finds the review resolved. A review summary that no run was given counts as a new comment. An escalation records nothing in the stored set, so comments an earlier run was given stay given. The reaction stays pending while the escalation stands and dispatches nothing; see Escalation recurrence for what ends it.

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

PR auto-merge. When configured and enabled, the orchestrator polls merge preconditions (review decision, CI status, draft state, mergeability) on Sortie-created PRs and executes the merge directly through the SCM adapter once the compound condition is satisfied. Auto-merge is disabled by default; enabling it is an explicit opt-in because the merge is irreversible.

Additional fields (via Extra):

| Field              | Type    | Default  | Dynamic Reload    | Description                                                                                              |
| ------------------ | ------- | -------- | ----------------- | -------------------------------------------------------------------------------------------------------- |
| `strategy`         | string  | `squash` | Requires restart | Merge strategy. One of `merge`, `squash`, or `rebase`.                                                    |
| `require_ci`       | boolean | `true`   | Requires restart | When `true`, the merge waits until every reported check has completed with no failing conclusion; a `skipped` or `neutral` conclusion counts as non-failing. When `false`, CI is advisory only.    |
| `delete_branch`    | boolean | `true`   | Requires restart | When `true`, the PR head branch is deleted after a successful merge. Failure to delete does not roll back the merge. |
| `poll_interval_ms` | integer | `60000`  | Requires restart | Polling interval for the precondition state machine. Minimum: `30000` (30 sec).                           |
| `watch_window_ms`  | integer | `1800000` | Requires restart | Bounds a pending entry's age, measured from the entry's creation. `0` removes the bound. Must be non-negative and must not exceed `9223372036854`. |

For a GitLab provider, `require_ci` reads the head pipeline the platform reports for the PR rather than a fetched check-run list, with one exception. A head pipeline the platform reports as `skipped` satisfies the gate, because that status is non-failing. A head pipeline the platform reports as `manual` is the exception, and the one case that costs a second request. GitLab reports `manual` both for a pipeline whose only remaining work is a manual job and for one that already carries a failed job beside it, so `require_ci` reads the commit statuses GitLab records for that pipeline and applies to them the same rule it applies to every other check. An untriggered manual job is not a failure, so a manual pipeline carrying no failed job satisfies the gate even though that job has not run; one carrying a failed job does not; one whose manual gate still holds later jobs at not-yet-started defers until they run. A project's "Pipelines must succeed" setting is the control that holds a merge on a pipeline that did not succeed.

**Activation:** The `reactions.auto_merge` block is active when `provider` is present and non-empty. Agent-created PRs MUST write `pr_number` (positive integer), `owner`, `repo`, and `branch` (all non-empty) to `.sortie/scm.json` in the workspace for auto-merge to activate. Recovery on restart additionally consults `pushed_at` (when present) to skip stale PRs older than the recovery lookback window.

**Token scopes required:** for a GitHub provider the configured token must carry `pull_requests:write` for the merge endpoint, and when `delete_branch` is not `false`, also `contents:write` for the branch delete endpoint. The classic `repo` scope is a superset that covers both. The orchestrator validates scopes once at startup; failure emits an ERROR log with the missing scope and disables auto-merge for the process lifetime. A transport-class startup failure (network outage) schedules a single bounded retry on the next reconcile tick before disabling auto-merge.

For a Gitea provider the scope model differs: Gitea has one coarse `write:repository` scope covering both the merge and branch-delete routes, and it exposes no scope-introspection surface. The startup check substitutes a user-role gate, reading the repository's `permissions.push` field and disabling auto-merge when the token's user lacks repository write access. A read-scoped token whose user has write access passes that gate and surfaces the missing scope only at runtime, as a 403 on the first merge or branch delete that the adapter rewrites to name `write:repository`. Grant the token's user repository write access and the token the `write:repository` scope.

For a GitLab provider one scope covers the whole API. `api` grants complete read and write access, so it covers the merge and branch-delete routes together and `delete_branch` does not change what the token needs; `read_api` covers the reads and is refused on every write. The startup check reads `GET /personal_access_tokens/self`: a classic token whose scopes omit `api` is a verified gap and disables auto-merge, while four responses leave the check unable to classify the token at all and auto-merge enabled. Those four are the opaque `granular` scopes value a fine-grained token reports, an empty scopes array, an unreadable introspection body, and a 404 on the introspection route. That skip is not a preflight failure and blocks nothing, so confirm a fine-grained token's permissions directly. Grant the token the `api` scope.

**Fingerprint dedup:** The fingerprint is the SHA-256 of the PR head SHA concatenated with the review decision. A new push or a change in review decision invalidates the fingerprint and allows a new merge attempt; an unchanged fingerprint with the dispatched flag set suppresses duplicate merge calls within the poll interval.

**Cross-kind isolation:** Auto-merge success and failure cleanup scope mutation to the `merge` kind only. CI failure and review comment continuations on the same issue are unaffected by a successful merge or by escalation.

**Escalation:** When `max_retries` is exhausted, or the forge refuses the merge call with an authentication or payload error, the orchestrator applies the configured escalation (`label`, `comment`, or `none`) and keeps the pending merge entry in the escalated state: it makes no further merge call, and the escalation ends on the facts under Escalation recurrence. Other reaction kinds on the same issue are preserved.

**Success comment:** after a merge the orchestrator emits the `auto_merge.merged` event. The issue receives its comment when a `tracker_comment` entry in [`notifications`](#211-notifications-operator-notification-backends) lists `auto_merge.merged`. While the workflow has a tracker and no `tracker_comment` entry, the comment keeps posting without any entry and draws a deprecation warning; adding an entry that omits `auto_merge.merged` stops it.

The example below escalates with no label. The issue gets an escalation comment only when a `tracker_comment` entry lists `escalation.auto_merge`.

Example:

```yaml
reactions:
  auto_merge:
    provider: github
    max_retries: 2
    escalation: none
    strategy: squash
    require_ci: true
    delete_branch: true
    poll_interval_ms: 60000
```

#### Reaction kind: `bot_review`

Automated review-bot comment routing. When configured, the orchestrator polls for PR comments authored by automated review tools (linters, static analyzers, security scanners, and AI reviewers such as GitHub Copilot or CodeRabbit) on Sortie-created PRs and dispatches continuation turns so the agent can address them. This is the complement of `review_comments`, which routes only human `CHANGES_REQUESTED` comments and excludes bot-authored ones.

Additional fields (via Extra):

| Field                    | Type         | Default | Dynamic Reload    | Description                                                                                               |
| ------------------------ | ------------ | ------- | ----------------- | --------------------------------------------------------------------------------------------------------- |
| `bot_usernames`          | list[string] | _(empty)_ | Requires restart | Allowlist of bot logins. A comment is bot-authored when the platform reports a bot user type OR its author login matches an entry here (case-insensitive). An entry here is also excluded from `review_comments`, so an allowlisted author does not drive both loops. |
| `poll_interval_ms`       | integer      | `60000` | Requires restart | Polling interval for bot comments. Minimum: `30000` (30 sec). The default is tighter than `review_comments` because bot comments arrive in bulk on push.  |
| `watch_window_ms`        | integer      | `1800000` | Requires restart | Bounds a pending entry's age, measured from the entry's creation or from the last time it waited for the issue's retry slot. `0` removes the bound. Must be non-negative and must not exceed `9223372036854`. |
| `max_continuation_turns` | integer      | `5`     | Requires restart | Maximum bot-fix continuation dispatches per issue before escalation. Must be positive. Higher than `review_comments` because bot fixes are mechanical.    |

**Activation:** The `reactions.bot_review` block is active when `provider` is present and non-empty, on its own, with no `reactions.review_comments` or `reactions.auto_merge` block required. When `provider` is absent or empty, the block is inactive. Agent-created PRs MUST write `pr_number` (positive integer), `owner`, `repo`, and `branch` (all non-empty) to `.sortie/scm.json` in the workspace for bot-review polling to activate.

**Classification:** Bot detection is deterministic author-metadata matching, not a content heuristic. A comment is selected when the platform marks its author as a bot OR its author login matches a `bot_usernames` entry, case-insensitively. The `bot_usernames` allowlist covers review tools that comment under a regular user account (`user.type == "User"`) rather than a bot account, such as Hound (`houndci-bot`), which posts inline style comments under a regular user account. Unlike `review_comments`, no `CHANGES_REQUESTED` review state is required, because review bots commonly post comment-only reviews. The same `bot_usernames` entry also excludes its author from `review_comments`, so a `bot_review` comment set and a `review_comments` comment set never both carry the same allowlisted author's feedback.

**No debounce:** Bot comments are dispatched immediately. There is no `debounce_ms` field; the debounce window that `review_comments` applies does not apply to `bot_review`, because bot comments arrive in bulk on push rather than at reviewer pace.

**Fingerprint dedup:** The orchestrator computes a SHA-256 fingerprint from sorted non-outdated comment IDs and persists it in the `reaction_fingerprints` SQLite table under a kind distinct from `review_comments`. A new push that changes the bot comment-ID set changes the fingerprint, and a turn starts when the set holds a comment that no earlier run of the issue was given; an unchanged dispatched fingerprint suppresses re-dispatch within the poll interval.

**Comments already given:** The rules of `review_comments` apply: a turn starts only for a comment no earlier run was given, also after a restart, and carries every remaining one. A set that only lost members starts no turn and spends none of `max_continuation_turns`, and an unaddressed comment comes back with the next new comment. While turns remain, a new review summary counts as a new comment and still starts a turn. A new run of an issue whose workspace already holds a pull request is given the bot comments no run was given, when this kind has no `triage` block and the template shows `bot_review_comments` as an added section.

**Cross-kind isolation:** `bot_review` and `review_comments` never interfere on the same PR; each owns its own pending entry, fingerprint row, and attempt counter. Escalation cleanup is scoped to the `bot_review` kind only and does not release the issue claim or clear sibling reaction kinds.

**Escalation:** When `max_continuation_turns` is exhausted, the orchestrator escalates only when the bot adds a comment on the code (a file or a line of the diff) that no earlier run was given. It then applies the configured escalation (`label`, `comment`, or `none`, default `label`, with `escalation_label` defaulting to `needs-human`) and keeps the pending bot-review entry in the escalated state, which dispatches nothing and ends on the facts under Escalation recurrence. Other reaction kinds on the same issue are preserved. After the last continuation turn, findings a bot posts only in its review summary (for example CodeRabbit nitpick or outside-diff sections, or Copilot "previously missed" or "suppressed" comments) start no escalation and stay visible on the pull request. While turns remain, a summary-only re-review still starts a continuation turn; a `triage` command that answers `handled` keeps re-reviews the operator knows are clean from spending one.

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

Merge-conflict detection and resolution. When configured, the orchestrator polls mergeability on Sortie-created open PRs each reconcile cycle and dispatches one rebase-and-resolve continuation turn each time a PR transitions from no-conflict to conflict. The continuation carries the PR's real base branch so the agent rebases the PR head branch onto the correct target and resolves the conflicts on the existing workspace.

Retry semantics are episodic: a resolved conflict closes the episode and resets the attempt counter, so a later independent conflict opens a fresh episode rather than counting against the prior budget. The default `max_retries` is `1`, lower than the other kinds, because merge-conflict resolution is less likely to succeed on retry than a CI fix.

Fields:

| Field              | Type    | Default       | Dynamic Reload    | Description                                                                                              |
| ------------------ | ------- | ------------- | ----------------- | -------------------------------------------------------------------------------------------------------- |
| `provider`         | string  | _(required)_  | Requires restart | SCM adapter kind. Must match the provider of every other active SCM reaction. Activates the kind.         |
| `max_retries`      | integer | `1`           | Requires restart | Per-episode rebase attempts before escalation. `0` escalates on first detection without a rebase attempt. |
| `escalation`       | string  | `label`       | Requires restart | Escalation action when retries are exhausted. One of `label`, `comment` (deprecated), or `none`.          |
| `escalation_label` | string  | `needs-human` | Requires restart | Label applied when `escalation` is `label`.                                                               |
| `poll_interval_ms` | integer | `60000`       | Requires restart | Polling interval for the conflict-detection state machine. Minimum: `30000` (30 sec).                     |
| `watch_window_ms`  | integer | `1800000`     | Requires restart | Bounds a pending entry's age, measured from the entry's creation or from the last time it waited for the issue's retry slot, rather than from the last recorded head. `0` removes the bound. Must be non-negative and must not exceed `9223372036854`. |

**Activation:** The `reactions.merge_conflicts` block is active when `provider` is present and non-empty, on its own, with no other `reactions` block required. Agent-created PRs MUST write `pr_number` (positive integer), `owner`, `repo`, and `branch` (all non-empty) to `.sortie/scm.json` in the workspace for merge-conflict polling to activate. To disable the reaction, omit the `merge_conflicts` block; setting `max_retries: 0` does not disable it but escalates on the first detected conflict.

**Fingerprint dedup:** The fingerprint is the SHA-256 of the PR head SHA, persisted in the `reaction_fingerprints` SQLite table under a kind distinct from the other reactions. A single conflicted head dispatches exactly one rebase turn; after the agent rebases and pushes a new head, the new head produces a new fingerprint, so a conflict that persists across the rebase re-arms a new attempt bounded by `max_retries`. When the conflict clears, the row is deleted, so the next conflict observation dispatches again.

**Cross-kind isolation:** Merge-conflict detection runs independently of every other reaction kind; each owns its own pending entry, fingerprint row, and attempt counter. Escalation cleanup is scoped to the `merge-conflict` kind only and does not release the issue claim or clear sibling reaction kinds. Auto-merge and merge-conflict coexist on the same PR: auto-merge defers while a PR is conflicted, and merge-conflict drives the resolution, after which auto-merge proceeds once the PR is clean and approved.

**Escalation:** When `max_retries` is exhausted within an episode (a new conflicting head pushes the attempt count strictly over the budget), the orchestrator applies the configured escalation (`label`, `comment`, or `none`, default `label`, with `escalation_label` defaulting to `needs-human`) and keeps the pending merge-conflict entry in the escalated state, with its attempt counter at the rebases dispatched. The entry dispatches nothing and ends on the facts under Escalation recurrence. Other reaction kinds on the same issue are preserved.

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

**`watch_window_ms` across `review_comments`, `bot_review`, `merge_conflicts`, and `auto_merge`.** All four default to `1800000` (thirty minutes), the same lifetime these kinds carried before the key existed, so an upgrade with nothing set in `WORKFLOW.md` changes no behavior. A workflow without `auto_merge`, where a person reviews and merges, normally wants a value on the order of `reactions.ci_failure.watch_window_ms`'s default (twenty-four hours) rather than the thirty-minute default: raising it costs one additional poll call per interval for as long as the entry lives, at the same poll rate the kind already uses. Unlike `reactions.ci_failure`, `bot_review` and `merge_conflicts` carry no merged-or-closed drop branch, and `review_comments` carries one only while its pull request waits on a required approval, so setting `watch_window_ms: 0` leaves one of their entries polling after a person merges the pull request until the tracker issue reaches a terminal state. An `auto_merge` entry ends at its first check after its pull request merges, whoever merges it; under `0` it polls on only while the pull request stays open or closes unmerged, until the tracker issue reaches a terminal state. A quoted numeric value (for example `"1800000"`) is rejected for these four keys, although `reactions.ci_failure.watch_window_ms` accepts one. A `review_comments` entry whose latest check found the pull request waiting on a required approval is likewise not aged out while it waits. A standing escalation is never aged out by any window: whatever the value, it ends on a merge, a close, a missing pull request, or the issue leaving `tracker.handoff_state`, and on the other facts under Escalation recurrence.

#### Reaction kind: `label_commands`

Label-command detection. When configured, the orchestrator polls each Sortie-managed PR's label-event journal and dispatches a read-only, no-clone agent session when an operator applies the configured review label. The session reads the PR diff and posts review comments under the agent's own identity; it performs no branch mutation and requires no repository checkout.

Unlike the other reaction kinds, `label_commands` carries no escalation semantics and does not use the shared per-kind fields (`max_retries`, `escalation`, `escalation_label`). It parses through a dedicated path and never appears as a generic reaction entry, so its fields sit directly on the block.

`label_commands` is the configuration key only. The runtime and persisted reaction-kind discriminators are `label-review` (the review command) and `label-fix` (the fix command): these values appear in logs and in the `reaction_fingerprints.kind` column. Each command has its own prompt continuation key, a third and fourth name: `label_review` and `label_fix` (both underscore; see Section 5.2).

Fields:

| Field              | Type    | Default         | Dynamic Reload   | Description                                                                                             |
| ------------------ | ------- | --------------- | ---------------- | ------------------------------------------------------------------------------------------------------- |
| `provider`         | string  | _(required)_    | Requires restart | SCM adapter kind. Must match the provider of every other active SCM reaction. Activates the block.      |
| `review_label`     | string  | `sortie:review` | Requires restart | Label that triggers a read-only review. An explicit empty string disables the review command.           |
| `fix_label`        | string  | `sortie:fix`    | Requires restart | Label that triggers the fix command. An explicit empty string disables the fix command.                 |
| `poll_interval_ms` | integer | `60000`         | Requires restart | Journal poll interval. Minimum `30000` (30 sec); a lower value is clamped up to the floor with a warning. |

**Activation:** The `reactions.label_commands` block is active when `provider` is present and non-empty and at least one command label is non-empty. Activation considers `fix_label` exactly as it considers `review_label`: a fix-only configuration (`review_label` empty, `fix_label` non-empty) activates the block and constructs the SCM adapter exactly as a review-only configuration does. Agent-created PRs MUST write `pr_number` (positive integer), `owner`, and `repo` (all non-empty) to `.sortie/scm.json` in the workspace for label detection to activate. The review command requires no branch: the review session has no checkout. The fix command additionally requires a non-empty `branch` in the same file, because the fix session checks out and pushes to it; a PR record without one is not seeded for the fix command.

**Acknowledgment:** On a confirmed command the orchestrator removes the label from the PR. The label's disappearance is the operator-visible signal that the command was accepted: present means "queued, not yet picked up"; gone means "accepted" (or retracted by the operator). A removal failure leaves the label in place and logs a warning; correctness is unaffected because deduplication rests on the journal position, not the removal. Re-applying the label after a review completes is a new gesture and produces a new session.

**Validation:** Setting `provider` while both `review_label` and `fix_label` are empty is a configuration error. Because the defaults are non-empty, this occurs only when the operator explicitly sets both labels to `""`. The rule is a config-shape check and surfaces offline via `sortie validate`. A `provider` naming an unregistered SCM adapter is also a `validate` error, reported under the check name `scm_adapter` by the activation checks that fold every active SCM reaction kind, `label_commands` included, into one provider set.

**Operator prerequisite:** A `{{ if .label_review }}` branch in the active prompt template is optional. Without it, a label-review dispatch runs Sortie's built-in review prompt, which asks the agent to fetch the diff of the pull request, review it, and post its review comments. A branch replaces the built-in prompt; it fetches the PR diff and posts review comments using the agent's own SCM tooling (see the `label_review` continuation key in Section 5.2, and [Built-in reaction prompts](#built-in-reaction-prompts) for how Sortie chooses between the branch and the built-in prompt). The orchestrator injects only the PR coordinates, never the diff text and never a posted comment. The orchestrator emits an info log at each dispatch.

**Operator prerequisite (fix):** A `{{ if .label_fix }}` branch in the active prompt template is optional. Without it, a fix dispatch runs Sortie's built-in fix prompt, which asks the agent to check out the pull request's head branch, address the outstanding review comments, push the fixes to that branch, and post a summary comment. A branch replaces the built-in prompt; it checks out `label_fix.branch`, fetches and addresses the outstanding review comments, pushes the fixes to that branch, posts a summary comment, and writes `.sortie/status` to signal completion (see the `label_fix` continuation key in Section 5.2, and [Built-in reaction prompts](#built-in-reaction-prompts)). The completion signal matters more here than for review: a review is naturally one turn, but a fix is multi-turn, so without it a completed fix session runs to `agent.max_turns` and wastes turns. The built-in fix prompt asks for that signal. The orchestrator emits an info log at each fix dispatch.

**Default-on activation:** `fix_label` defaults to `sortie:fix`, so shipping the fix command activates it for every deployment that already sets `provider` for `label_commands`, including a deployment that enabled only the review command. A review-only deployment MUST set `reactions.label_commands.fix_label: ""` to opt out.

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

Merge-completion detection. When configured, the orchestrator observes the merge state of Sortie-managed pull requests independently of who performs the merge, and transitions the linked issue to a single configured terminal state exactly once. The reaction is off by default: a deployment that omits the block behaves exactly as one running without it, and enabling it grants the tracker credential write authority it did not need before.

Fields:

| Field              | Type    | Default       | Dynamic Reload    | Description                                                                                    |
| ------------------ | ------- | ------------- | ------------------ | ------------------------------------------------------------------------------------------------ |
| `provider`         | string  | _(required)_  | Requires restart  | SCM adapter kind. Must match the provider of every other active SCM reaction. Activates the kind. |
| `target_state`     | string  | _(required)_  | Requires restart  | The single terminal state the linked issue moves to once its pull request merges. Never inferred. |
| `poll_interval_ms` | integer | `60000`       | Requires restart  | Polling interval for the merge-observation state machine. Minimum `30000` (30 sec).               |
| `max_retries`      | integer | `2`           | Requires restart  | Retryable transition attempts before escalation. `0` escalates on the first failed attempt.       |
| `escalation`       | string  | `label`       | Requires restart  | Escalation action when retries are exhausted. One of `label`, `comment` (deprecated), or `none`.  |
| `escalation_label` | string  | `needs-human` | Requires restart  | Label applied when `escalation` is `label`.                                                       |

**Tracker prerequisites:** two `tracker` fields must be set before this block activates, and each is enforced offline. `tracker.handoff_state` must be non-empty: it is the state a merge waits in, and pending-reaction recovery across a restart is disabled entirely without it. `tracker.terminal_states` must be written out in front matter: the reconcile pass and the terminal workspace sweep both read the list exactly as configured, and no tracker supplies a default, so the validator accepts a `target_state` only from a list the runtime treats as terminal.

**Activation:** The `reactions.merge_completion` block is active when `provider` is present and non-empty, on its own, with no other `reactions` block required. Agent-created PRs MUST write `pr_number` (positive integer), `owner`, and `repo` (all non-empty) to `.sortie/scm.json` in the workspace for merge-completion polling to activate; unlike the checkout-bearing kinds, no `branch` is required because the reaction performs no checkout.

**Target-state rule and irreversibility:** the target is never inferred from the terminal list; the operator names it explicitly, and it is applied verbatim once the merge is observed. The transition is not reversible by the orchestrator: naming an abandonment state where a completion state was meant closes finished work under the wrong label, and no validator can detect that mistake, because it is a judgement about the issue rather than a configuration shape.

**Idempotency key:** the fingerprint is the merge commit identifier reported by the provider, not the pull request number, persisted in the `reaction_fingerprints` SQLite table under a kind distinct from every other reaction. A row is created on the first observed merge, retained (never deleted) once the transition succeeds, and re-armed only when a later merge reports a different commit identifier. A pull request may remain unmerged for any length of time without starting a failure clock. When the provider first reports `Merged: true` with no commit identifier, Sortie records that PR identity separately and waits thirty minutes, retrying with exponential pending backoff floored at `poll_interval_ms`. If the identifier is still absent on the first poll at or after the deadline, Sortie escalates without transitioning the issue: it emits `escalation.merge_completion` under the configured `escalation` and keeps watching the pull request, and the escalation ends when the forge reports the merge commit. Delivery is recorded only after the action that confirms it succeeds: the label under `escalation: label`, and the comment on the issue under `escalation: comment` or `none` when a `tracker_comment` subscription receives the event. A send to a backend destination never confirms delivery and never blocks it, so it never suppresses the retry of a failed tracker write. Under `none` with no subscriber the event has no destination and delivery is recorded at once. A failure does not restart the grace period; the first poll after a restart, or a later fresh pending entry, can retry the undelivered notification once. If the tracker write succeeds but the follow-up marker write itself fails, the notification has already reached the operator even though the observation stays recorded as undelivered; the retry then delivers it a second time. `escalation: label` repeats harmlessly, because re-applying a present label is a no-op; a comment on the issue is posted a second time. If such a later entry instead observes a real identifier, it follows the normal merge-commit fingerprint path and clears the temporary observation after that latch completes. `max_retries` applies only to failed tracker transitions, not to this grace period.

**Failure matrix:**

| Transition outcome            | Posture                                                     |
| ------------------------------ | ------------------------------------------------------------ |
| Transport or API failure       | Retry with backoff, bounded by `max_retries`, then escalate  |
| Authentication failure         | Escalate immediately, no retry                                |
| Payload failure (target state unreachable) | Escalate immediately, no retry                    |
| Issue not found                | Stop, mark the fingerprint dispatched, log a warning, no escalation |

An escalation keeps the pending entry and makes no further transition call while it stands. A restart resumes one call: a success ends the escalation, and a failure leaves it standing without a second action.

**Restart-to-apply:** reaction configuration, including `target_state`, is captured once at orchestrator construction and is not rebuilt on a workflow reload. A change to this block, or to the two tracker prerequisites, takes effect only on the next restart; `sortie validate` compares the same configuration a restart would build, so the offline verdict and the construction verdict cannot diverge. Environment indirection through `$VAR` is not supported for any field in this block, matching every other reaction kind.

**Request cost:** each parked issue costs one tracker issue-state read and one pull-request read per poll interval while it is unmerged, plus one tracker write per observed merge. A merged pull request missing its commit identifier backs off rather than issuing a forge read on every fixed poll. On a forge tracker the tracker and the SCM adapter share one credential against one host, so the steady-state unmerged cost approaches two requests per parked issue per poll interval. A deployment with many simultaneously parked issues SHOULD raise `poll_interval_ms` above the default rather than accept it.

**Cross-kind isolation:** CI-failure and review escalations are scoped to their own kind: each keeps its own pending entry and attempt counter while it stands and clears nothing of another kind, so merge-completion tracking for that issue survives an unrelated escalation. Other reaction kinds on the same issue are unaffected by a merge-completion transition or escalation, and vice versa.

**Configuration drift on a running process:** editing `tracker.terminal_states` while the process runs does not update the `target_state` a running orchestrator already captured. When the two disagree, the reaction logs one warning naming both values and keeps transitioning issues to the frozen target; the terminal workspace sweep stops collecting the workspaces of issues this reaction closes until the two lists agree again. A restart re-validates the edited file offline and rejects the same drifted configuration before the process starts.

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

- Reaction kind keys must match `[a-z][a-z0-9_-]*`. Invalid keys are rejected with a configuration error.
- `max_retries` must be non-negative for all kinds.
- `escalation` must be `"label"`, `"comment"`, or `"none"` for all kinds.
- `watch_window_ms` must be non-negative and must not exceed `9223372036854` for `ci_failure`, `review_comments`, `bot_review`, `merge_conflicts`, and `auto_merge`.
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
- `target_state` for `merge_completion` is required, must not equal `tracker.handoff_state`, must not be a member of `tracker.active_states`, and must be a member of `tracker.terminal_states`, both as written.
- `tracker.handoff_state` must be set and `tracker.terminal_states` must be non-empty in front matter when `reactions.merge_completion.provider` is set; each is reported as a configuration error naming the tracker field.
- `poll_interval_ms` must be >= `30000` for `merge_completion`.
- When `provider` is absent or empty, all other fields in the kind sub-object are ignored.

**Where each rule is enforced:** the rules that the config layer owns (reaction key shape, `max_retries`, `escalation`, `escalation_label`, every `label_commands` rule, and `reactions.ci_failure`'s Extra keys) run during typed config construction, so `sortie validate` reports them offline. For the kind-specific rules, `sortie validate` runs the `review_comments`, `auto_merge`, `bot_review`, `merge_conflicts`, and `merge_completion` builders. These are the same builders used when the orchestrator constructs the reactions at startup, so both paths report the same invalid values.

---

### 2.10 `dispatch` — Rule-based Routing

```yaml
dispatch:
  max_consecutive_hops: 10 # optional; positive integer; per-issue ceiling on automatic stage hops; at least the hops of the longest chain
  partials: # optional; paths or glob patterns relative to the WORKFLOW.md directory; files whose define blocks every prompt template can call
    - ./partials/*.md
  rules: # ordered list; optional; a rule with a stage label is selected by that label, the others first-match-wins
    - name: <rule-name> # optional; must match ^[a-z][a-z0-9_-]*$; required with a settings block, a stage label, or next; never "default" with a settings block
      stage: <label> # optional; selects the rule by an issue label; excludes match; taken literally
      next: <rule-name> # optional; the rule a successful run advances the issue to; the named rule must carry stage
      match: # optional; absent or empty match block matches every issue (catch-all) on a rule without stage
        labels: ["bug", "p0-*"]  # string or list; glob; OR within key
        issue_type: ["Bug"]      # string or list; case-insensitive equality
        priority: { lte: 2 }    # predicate object; exactly one of eq, in, lt, lte, gt, gte
        identifier: ["FE-*"]    # string or list; glob; matched against issue.identifier
        assignee: ["alice"]     # string or list; case-insensitive equality
        title: ["[docs]", "wip:"] # string or list; whole words; case-insensitive
      agent: <kind>    # optional; overrides the agent kind for matching issues
      template: ./prompts/bug.md # optional; path relative to the WORKFLOW.md directory
      <kind>: # optional; settings block for the agent kind this rule runs
        model: <model>
        effort: <level>
  default: # optional
    agent: <kind>     # optional; defaults to top-level agent.kind
    template: <path>  # optional; defaults to the WORKFLOW.md Markdown body
```

#### Dispatch rules

| Field | Type | Required | Default | Description |
| ----- | ---- | -------- | ------- | ----------- |
| `max_consecutive_hops` | integer | No | the larger of `10` and the hops of the longest chain | Per-issue ceiling on consecutive automatic stage hops. Must be greater than `0` and at least the number of hops in the longest chain. Absent or null takes the default. See [Stage chains](#stage-chains). |
| `partials` | list of strings | No | _(none)_ | Files whose define blocks every prompt template can call with `{{ template "name" . }}`. Each entry is a path or a glob pattern relative to the `WORKFLOW.md` directory. Absent, null, and an empty list select no file. See [Partials](#partials). |
| `rules` | list of rule objects | No | _(none)_ | Dispatch rules. A rule with `stage` is selected by its label first; the other rules are evaluated in YAML order and the first match wins. |
| `default` | map | No | _(none)_ | Fallback selection when no rule matches. Keys: `agent`, `template`. |

Each rule in `dispatch.rules` is a map with the following keys. A rule must carry at least one of `match`, `stage`, `agent`, `template`, or a settings block; `next` alone does not count. A key that names a registered agent kind is the rule's settings block; any other key is unrecognized and fails the load.

| Field | Type | Required | Default | Description |
| ----- | ---- | -------- | ------- | ----------- |
| `name` | string | With a settings block, `stage`, or `next` | _(absent)_ | Operator-supplied identifier used in logs, in run history, and to find the rule again after a reload. Must match `^[a-z][a-z0-9_-]*$` when present, and must not be `default`, the name run history and statistics give the `dispatch.default` selection, when the rule carries a settings block. Names are unique. |
| `stage` | string | No | _(absent)_ | Label that selects this rule. An issue that carries the label runs on the rule wherever it sits in the list. Compared case-insensitively against the adapter-normalized label set; no glob characters and no `$VAR` resolution. A rule with `stage` cannot carry `match` and must have a `name`. See [Stage labels](#stage-labels). |
| `next` | string | No | _(absent)_ | Name of the rule a successful run advances the issue to. The named rule must exist, must differ from this rule, and must carry `stage`. The rule that carries `next` must have a `name` and may be any rule, with or without `stage`. See [Stage chains](#stage-chains). |
| `match` | map | No | _(absent)_ | Predicate block. On a rule without `stage`, an absent or empty block matches every issue (catch-all). |
| `agent` | string | No | _(fallback)_ | Agent adapter kind for matching issues. Falls through to `dispatch.default.agent`, then to `agent.kind`. |
| `template` | string | No | _(fallback)_ | Prompt template path (relative to `WORKFLOW.md` directory). Falls through to `dispatch.default.template`, then to the Markdown body. |
| `<kind>` | map | No | _(absent)_ | Settings block named for the agent kind the rule runs: the rule's `agent`, else `dispatch.default.agent`, else `agent.kind`. Holds the keys that kind's top-level block accepts. See [Rule settings blocks](#rule-settings-blocks). |

The `match` block accepts only these keys:

| Field | Type | Matching | Description |
| ----- | ---- | -------- | ----------- |
| `labels` | string or list | Glob (any element) | Matches when the issue carries at least one label matching any pattern. Patterns use `path.Match` glob syntax (e.g., `bug/*`, `*-urgent`). Matched against the adapter-normalized lowercase label set. |
| `issue_type` | string or list | Case-insensitive equality (any element) | Matches when the issue type equals any list entry. |
| `priority` | predicate object | Numeric comparison | Single-operator predicate. See Priority predicates below. An issue with no priority never matches. |
| `identifier` | string or list | Glob (any element) | Matches when `issue.identifier` glob-matches any pattern, in the case the adapter produced. |
| `assignee` | string or list | Case-insensitive equality (any element) | Matches when the issue assignee equals any list entry. |
| `title` | string or list | Whole-word phrase (any element) | Matches when any phrase appears in the issue title as whole words, ignoring letter case. A phrase has no wildcards. See [Title phrases](#title-phrases). |

The `dispatch.default` block accepts only `agent` and `template`, with the same types and fallback behavior as the per-rule fields. It carries no settings block: a key that names an agent kind fails the load, and the top-level block of each kind holds the default settings.

#### Stage labels

A rule that carries `stage` is selected by a label on the issue instead of by a `match` block, so a person puts an issue on that rule by applying one label. Stage labels are checked before the ordered rules: an issue that carries the label of a staged rule runs on that rule wherever it sits in the list, and an earlier rule or a catch-all cannot capture it. An issue that carries no stage label is routed by the rules without `stage`, in order, exactly as it is without stage labels, and a staged rule never matches it.

A rule is selected in three steps, and the first step that yields a rule decides:

1. The target of the issue's latest stage hop, when the issue carries the label that hop added and a rule of that name still exists. See [Stage chains](#stage-chains).
2. The rules that carry `stage`. An issue that carries the labels of several of them runs on the one furthest down a chain: a staged rule that leads on, through `next`, to another stage the issue carries is set aside, and of the rules left the one listed first runs. Without `next` in the workflow, that is the one listed first.
3. The rules without `stage`, in order, then `dispatch.default`, then the workflow-wide defaults. See [Fallback resolution chain](#fallback-resolution-chain).

```yaml
dispatch:
  rules:
    - name: everything
      template: ./prompts/default.md
    - name: plan
      stage: Stage-Plan
      template: ./prompts/plan.md
    - name: implement
      stage: stage-implement
      template: ./prompts/implement.md
```

In this workflow:

| Issue labels | Rule that runs | Warning |
| ------------ | -------------- | ------- |
| `bug` | `everything` | none |
| `stage-plan` | `plan` | none |
| `stage-implement`, `bug` | `implement` | none |
| `stage-plan`, `stage-implement` | `plan` | one `several stage labels found` record |
| `stage-review` | `everything` | none |

The label is taken literally. It is compared case-insensitively with the labels the tracker adapter reports, which are lowercase, so `Stage-Plan` and `stage-plan` are one label. It has no glob characters and no `$VAR` resolution. Surrounding white space is part of the label and tracker labels carry none, so a label written with edge white space in quoted YAML loads and never selects its rule.

A rule with `stage` must have a `name` and cannot carry `match`. The `stage` value must be a label with at least one character other than white space. Two rules cannot share a label, and labels that differ only in case count as the same label. A catch-all rule may precede staged rules, because the stage label selects them first. A rule without `stage` after a catch-all is still an error.

An issue that carries the labels of several staged rules runs on the one step 2 selects. Sortie logs one `Warn` record, `several stage labels found`, that names every stage label the issue carries in list order and the rule it selected. The record appears on a poll tick that evaluates the issue and on a retry that is routed afresh, once per evaluation, and never for a retry whose recorded selection stands.

A stage label must not equal a state name or a label that Sortie applies to issues, so that placing an issue on a stage never doubles as a state change or an escalation. `sortie validate`, startup, and the check before each tick reject a stage label that equals, ignoring case:

- an entry of `tracker.active_states` or `tracker.terminal_states`;
- `tracker.handoff_state`, `tracker.in_progress_state`, or `tracker.no_change_state`, when set;
- the `escalation_label` of a reaction;
- the parking label, which is the `escalation_label` of `reactions.review_comments` and `needs-human` when that reaction does not write one.

A label that equals several of these draws one error for each. Every reaction block present carries an escalation label, `needs-human` unless the block writes another, and each one is compared; a `ci_failure` block without a `provider` is not compared. A stage label of `needs-human` therefore collides once with each reaction block present and once with the parking label.

The escalation labels and the parking label are compared as the workflow writes them. Reaction settings other than `ci_failure` take effect only after a restart, so a label that a reload renames is compared at once while the running process keeps applying the old one until it restarts.

As for every rule, the selected rule is recorded for the claim. Moving a stage label on an issue whose claim is held re-routes nothing until the claim is released. See [Freeze-on-dispatch](#freeze-on-dispatch).

#### Stage chains

A rule that carries `next` names the rule an issue moves to after a successful run. The rule it names must carry `stage`, so the move is the swap of one stage label for another. A chain of any length is a series of rules linked by `next`; the last rule has no `next` and ends on `tracker.handoff_state` as every rule does without chains.

```yaml
tracker:
  active_states: ["To Do", "In Progress"]
  handoff_state: Human Review

dispatch:
  rules:
    - name: specify
      match:
        labels: ["feature"]
      next: implement
      template: ./prompts/specify.md
    - name: implement
      stage: stage-implement
      template: ./prompts/implement.md
```

An issue labeled `feature` runs `specify`. When that run succeeds, Sortie adds `stage-implement` to the issue and writes no tracker state, so the issue stays in `To Do` or `In Progress`. The next poll tick selects `implement` by its label ahead of `specify`, in a new session. When `implement` succeeds, the issue moves to `Human Review` and its reactions take over. Moving the issue back to an active state reruns `implement`, because its label is still there; removing `stage-implement` sends the issue back to `specify`. The rule that carries `next` does not need `stage` itself, so a person labels the issue once, with an ordinary label, to start the chain.

**What a hop does.** On an exit that would otherwise write `tracker.handoff_state` (or `tracker.no_change_state` for a declared run), a rule that carries `next` instead:

1. Adds the stage label of the rule `next` names.
2. Records the hop and its count, which survive a restart.
3. Cancels the issue's queued retry and its pending reactions, and releases the claim, so nothing from the finished stage runs again while the next stage holds the issue.
4. Removes every other configured stage label that the issue carried when the run was dispatched. A label that someone adds while the run is going stays on the issue, and step 2 of the selection routes past it.

The hop writes no tracker state and increments no `sortie_handoff_transitions_total` result. The dispatch of the next rule writes `tracker.in_progress_state` like any dispatch. A declared `no-change-needed` run on a rule with `next` hops too; the next stage sees the outcome as `no_change`, and `tracker.no_change_state` applies only when the hop is not made.

A hop is made only where the handoff write would have been taken: not after a blocked soft stop, a failed or timed-out run, a terminal exit, a run whose evidence verdict withholds the handoff, a label-command dispatch, or an issue that is no longer in an active state.

**When the hop is not made.** The exit takes the handoff write instead, so the issue lands on `tracker.handoff_state` (or `tracker.no_change_state`) and waits for a person, exactly as a rule without `next`. That happens when:

- the label add fails; the log record `stage hop not made` carries `reason=add_failed`, the `error`, and `missing_stage_labels`, the stage labels the issue carried at dispatch and no longer carries, and Sortie sends `stage.not_advanced` with reason `failed`;
- the issue's hop count has reached `dispatch.max_consecutive_hops`; the record carries `reason=ceiling`, and Sortie sends `stage.not_advanced` with reason `ceiling`;
- a reload removed the run's rule, removed `next` from it, removed the rule `next` named, or removed that rule's `stage`. No hop is due in that case, so Sortie logs nothing and sends nothing, and the issue takes the handoff write.

A failed label removal after a successful add still counts as a hop. The old label stays on the issue, the next stage runs, and Sortie logs a `Warn` record, `stage hop made, stage labels left on the issue`, that names the labels left. Step 1 of the selection picks the new stage over the old label.

**Waiting for the label.** After a hop, Sortie dispatches the issue on no rule until a read shows the label the hop added. A tracker listing can lag a write, and without this wait the previous stage would run again. When the poll listing lacks the label, Sortie reads the issue once, directly. A read that shows the label dispatches the next stage. A read that shows an issue in a terminal state, or outside the active states, resets the count and dispatches nothing. A read that shows an issue with no state label holds it for that tick, and reconciliation resets the count only when the next reading also shows none, because a healthy label edit can show an issue with no state label for a moment. A read that shows neither stage label releases the wait and routes the issue by the labels the read returned, with a log record, `stage hop target label absent, hold released`. A failed read holds the issue until the next tick. Once a read has shown the label, the wait is over, and it stays over across a restart.

**The ceiling.** `dispatch.max_consecutive_hops` bounds the hops Sortie makes on one issue in a row, so a chain that label automation or a mistake keeps re-entering cannot run without end. The value is a positive integer and at least the number of hops in the longest chain, which is the length of that chain minus one. When it is absent or null, the ceiling is the larger of `10` and that number of hops. A count at the ceiling makes the exit take the handoff write, and that write resets the count.

The count is kept per issue in the database and resets when:

- an exit writes the handoff state, or the run ends on a terminal state or an issue that is not active;
- Sortie sees the issue in a terminal state or outside the active states, during reconciliation, a retry, or the wait above, where a reading with no state label counts only when the next reading also shows none;
- a park is placed on the issue, or released.

Each reset logs `stage hop count reset` with the `trigger`. Moving a stage label while the issue stays active does not reset the count, and neither does a failed handoff write, a withheld verdict, a reload, or reaching the ceiling. Reconciliation reads every issue that holds a hop record on each tick, whether or not it is running.

**Recording.** Every run carries a chain identifier, a random value that ties together the runs of one pass through a chain. A run that no hop led to starts a new chain. The run a hop leads to continues the chain of the run that made the hop, and a retry or a restart keeps the chain of the run it continues. The run that reaches a hop decision also records the rule the hop targeted and the result: `advanced`, `partial` (the label was added but an old stage label was not removed), `failed` (the label add failed), or `ceiling`. A run recorded before the upgrade has no chain and no result. Sortie sends `stage.advanced` when a hop is made, `advanced` or `partial`, and `stage.not_advanced` when a due hop is not made, with the reason; both reach only the notification entries that list them ([Event catalog](#event-catalog)). `sortie stats` groups the runs by stage chain, listing only the chains in which a hop was due or made, and draws a funnel for each stage chain: for every stage, the passes that reached it and how each one left it. A run in an issue's detail in the dashboard shows its rule and, when its exit reached a hop decision, the target rule and the result.

**The `stage` object.** Every prompt render carries `stage` with `stage.current`, `stage.previous`, and `stage.previous_outcome`, so a template can tell what the earlier stage was and read what it left in the workspace. See [Section 5.2](#52-template-input-variables).

**Validation.** `sortie validate`, startup, reload, and the check before each tick reject a `next` that is not a rule name, names no rule, names its own rule, names a rule without `stage`, or forms a cycle; a `next` on a rule without a `name`; `next` without `tracker.handoff_state`; and a `dispatch.max_consecutive_hops` that is not an integer, is not positive, or is below the longest chain's hops. When `agent.max_sessions` is above `0` and below the number of runs the longest chain needs, an advisory warns that an issue on the chain exhausts its session budget before the chain ends. The checks are listed in [Section 8](#8-dispatch-preflight-validation) and the errors in [Section 9.2](#92-configuration-errors).

#### Matching semantics

Stage labels are checked first (see [Stage labels](#stage-labels)). For an issue that carries none, evaluation of the rules without `stage` applies AND logic across keys and OR logic within a single key:

- A `match` block succeeds only when every present key is satisfied.
- A key whose value is a list succeeds when any element matches. A `labels`, `issue_type`, `identifier`, or `assignee` key whose value is null or an empty list is left out of the match instead, while for `title` it fails the load.
- An absent or empty `match` block on a rule without `stage` always succeeds (catch-all). A rule with `stage` has no `match` block and is never part of this evaluation.

String-valued keys (`labels`, `issue_type`, `identifier`, `assignee`, `title`) accept either a single string or a list of strings. A scalar is treated as a one-element list.

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

For `in`, the value is a list of integers (e.g., `{ in: [1, 2] }`). For all other operators, the value is a single integer. An issue with no priority never matches a `priority` predicate.

#### Title phrases

The `title` key takes one phrase or a list of phrases. The key matches when any phrase appears in the issue title as whole words. Matching follows these rules:

- Letter case is ignored, and so are the invisible emoji variation selectors U+FE0E and U+FE0F, on both the phrase and the title. White space at either end of a phrase or a title is ignored, and each run of white space, tabs, no-break spaces, and ideographic spaces included, compares as one space.
- A word is a run of letters, digits, and combining marks. Every other character, hyphen and underscore included, separates words. A phrase matches only where it neither starts nor ends inside a word, so `fix` matches `Fix login redirect` but neither `Add prefix to logs` nor `Fixes typo in docs`, and `low-cost` does not match `Speed up slow-costly import`.
- A phrase that starts or ends with a character that is not part of a word, such as `[docs]` or `wip:`, is held to no word boundary on that side. Punctuation written in a phrase must appear in the title: `[infra]` matches `[INFRA] rotate keys` but not `Improve infra docs`, while `infra` matches both.
- Chinese, Japanese, Thai, Lao, Khmer, and Myanmar text is written without spaces between words, so each character of these scripts counts as a word of its own: `设计` matches inside `缓存设计文档`. A combining mark, such as a Thai vowel or tone mark, stays with the character before it in every script, so `เก` does not match `เก่ง`.
- Korean is written with spaces, and its particles attach to the noun. A bare noun such as `버그` does not match its in-title form `버그를`; write the phrase as the title spells it.
- Title phrases have no wildcards. `*` matches a literal asterisk, and `?` and brackets match themselves, unlike in `labels` and `identifier`, where they are glob characters. A phrase `*infra*` matches `*INFRA* rotate keys`, not `Improve infra docs`.
- Accents and every other combining mark are compared as written: `cafe` does not match `Café opening hours`. An emoji matches whether or not the title or the phrase carries a variation selector after it, so the warning sign U+26A0 matches `⚠️ deprecate v1 API` and a phrase written with the selector matches a title written without it.
- An issue with an empty title never matches. A `title` key that is null, bare, or an empty list fails the load, and so does a phrase that is empty or only white space; remove the key to leave the title out of the match. Each fault is listed in [Section 9.2](#92-configuration-errors).

YAML reads an unquoted phrase that starts with `[` as a list, one that contains `: ` as a map, and a bare number or date as a number or a timestamp. Quote a phrase in these cases:

```yaml
match:
  title: "[infra]"
```

```yaml
match:
  title: ["[infra]", "WIP:"]
```

Two unquoted forms escape the checks. `title: [infra]` is the list holding the word `infra`, so it loads and matches the bare word `infra`, not `[infra]`. `title: fix: typo` does not parse, and the load fails with a front matter error. A list element that YAML read as a list, a map, a number, or a date fails the load with a message that names the quoting that fixes it.

A title is written for people, and one issue can match one rule by its title and another by its label. The first rule in YAML order that matches wins. In this workflow an issue titled `[docs] Fix broken link` and labeled `bug` runs the `docs` rule, because it comes first; with the two rules swapped it would run `bug-fix`:

```yaml
dispatch:
  rules:
    - name: docs
      match:
        title: "[docs]"
      template: ./prompts/docs.md
    - name: bug-fix
      match:
        labels: ["bug"]
      template: ./prompts/bug.md
```

A rule that should run only for issues that satisfy both keys carries both in one `match` block: `match: { title: "[docs]", labels: ["bug"] }`. A rule whose only key is `title` is not a catch-all and may precede other rules.

The title counts when a claim is first dispatched, as a label does. Renaming an issue whose claim is held does not re-route it, and renaming it before the claim routes it by the new title on the next tick. See [Freeze-on-dispatch](#freeze-on-dispatch).

Whoever can edit an issue's title can change which rule it matches, and on many trackers that includes the issue's author, who may not be allowed to change its labels. A rule whose settings or template not every author should reach pairs `title` with `labels` or `assignee`.

#### Fallback resolution chain

The target of the issue's latest stage hop is checked first, while the issue carries the label that hop added. Stage labels come next: an issue that carries the stage label of a rule is selected by that rule, the one furthest down a `next` chain and then the one listed first when it carries several (see [Stage labels](#stage-labels)). Otherwise the rules without `stage` evaluate in YAML order, and the first rule whose `match` block succeeds is selected; later rules are not consulted. For each selected rule, `agent` and `template` may each be omitted independently. Missing fields fall through in order:

1. The matched rule's `agent` / `template`.
2. `dispatch.default.agent` / `dispatch.default.template`.
3. Top-level `agent.kind` (for agent) or the `WORKFLOW.md` Markdown body (for template). See [Section 2.6](#26-agent--coding-agent-configuration) for the top-level `agent.kind` default.

The kind that steps 2 and 3 produce for an agent is the default kind: `dispatch.default.agent` when set, otherwise `agent.kind`. It is the only kind that launches `agent.command`; a kind a rule selects launches its own default command.

When no rule matches and `dispatch.default` supplies an agent or template, the default is applied with the same fallback chain for any unset field.

A kind an `agent` selector introduces here, rather than falling through to the top-level `agent.kind`, must carry its own settings block, either as a top-level block or, when every selector of that kind is a rule that carries the kind's block, in those rules; see [Section 4.5](#45-adapter-specific-pass-through-config) for the requirement and the `dispatch.agent.missing_block` check that enforces it.

#### Rule settings blocks

A rule may carry one settings block, named for the agent kind the rule runs and holding the keys that kind's top-level block accepts. `model` and `effort` are the usual keys; every other key the kind reads from its block is overridable the same way, including the per-invocation limits some kinds keep there. A block for any other kind fails the load, which also catches a block left behind after the rule's `agent`, `dispatch.default.agent`, or `agent.kind` changed.

The block an attempt runs with is the top-level block of the rule's kind with the rule's block laid over it, one level deep:

- A key the rule writes replaces the inherited value whole, whatever its type. A map or a list is replaced, never merged or appended: `codex.turn_sandbox_policy`, `opencode.allowed_tools`, and `opencode.denied_tools` are the keys that hold one.
- A key the rule writes as `null` removes the inherited key, so the adapter sees it as never written and applies its own default. For `model` and `effort` that is the runtime's own default.
- Every key the rule does not write is inherited.
- A kind with no top-level block inherits nothing; the rule's block is the whole block.
- A rule's block is laid only over the top-level block of its own kind.

No key is defaulted or coerced before the overlay; the adapter applies its defaults after it.

A rule's block cannot write `kind`, `command`, `turn_timeout_ms`, `read_timeout_ms`, `stall_timeout_ms`, or `stop_grace_ms`. Sortie derives them from the `agent` section, which stays workflow-wide, so writing one in a rule would read as an override that does not happen. A block must be a map; write `{}` for an empty one.

```yaml
agent:
  kind: opencode

opencode:
  model: provider/strong-model
  effort: high

dispatch:
  rules:
    - name: low-cost
      match:
        labels: ["model:low-cost"]
      opencode:
        model: provider/cheap-model
        effort: null          # clear the inherited level; the cheap model may not offer it
    - name: hard
      match:
        labels: ["model:strong"]
      opencode:
        effort: max           # model inherited from the top-level block
```

Issues with neither label run on the top-level block. `sortie stats` groups the runs by rule, by model, by stage chain, by retry trigger, and by what started each run, and draws a funnel for each stage chain.

Two work profiles on two kinds fit in one workflow. A rule that introduces a kind other than `agent.kind` and carries that kind's block needs no top-level block for it:

```yaml
agent:
  kind: claude-code

claude-code:
  permission_mode: bypassPermissions
  model: strong-model
  effort: high

dispatch:
  rules:
    - name: specify
      match:
        labels: ["specify"]
      template: ./prompts/specify.md
      claude-code:
        effort: max
    - name: implement
      match:
        labels: ["implement"]
      agent: opencode
      template: ./prompts/implement.md
      opencode:
        model: provider/coding-model
        effort: high
```

Level names depend on the model. A rule that writes `model`, does not write `effort`, and inherits a non-empty `effort` draws the `agent.effort.inherited` warning: write `effort` in the rule to choose the level for its model, or `effort: null` to clear it.

Changing `agent.kind` or `dispatch.default.agent` is rejected when a rule without its own `agent` carries a block for the old kind, because the rule would run another kind than its block names. Fix the block or the rule's `agent`, and the next reload applies.

#### Freeze-on-dispatch

An issue keeps its agent kind, template, and rule until its claim is released. The resolved `(agent_kind, template_id, rule_name)` is recorded at dispatch and reused by retries and reaction-driven continuations for the same claim. Rules are re-evaluated only after the claim is released, with one exception: a waiting retry checks its recorded selection against the configuration in force when its timer fires. Moving a label on, or renaming, an issue whose claim is still held does not re-route it, and that includes moving a stage label; the next claim does. The pair `stage.previous` and `stage.previous_outcome` is part of the selection: a retry or a reaction continuation that keeps its rule renders the same pair, also after a restart, while `stage.current` is read from the configuration in force at each dispatch. A retry that is routed again to another rule renders `stage` as a poll tick does. A stage hop releases the claim, so the next stage always starts on a new claim.

What the selection contains is read from `WORKFLOW.md` at the start of every attempt: the agent settings (`model`, `effort`, and the rest of the kind's block, from the rule and from the top-level block), the template text, and the `agent.*` timeouts. A reload therefore reaches the next attempt of a claim that is already held, while a running session keeps the settings it started with. A retry or continuation that resumes a session resumes it with the settings it resolved, and a change of `model` or `effort` between attempts does not end the session.

When the rule named in a held claim no longer exists, or no longer carries a block for the claim's kind, the attempt runs on the top-level block of that kind and Sortie logs one `Info` record naming the rule.

An attempt whose resolved block fails an error-severity check (see [Section 8](#8-dispatch-preflight-validation)) starts no session. The first dispatch skips the issue for that tick; a retry or continuation is rescheduled with backoff and keeps its claim, its continuation data, and its session to resume.

A changed rule set from a `WORKFLOW.md` reload applies to future claims only. In-flight issues keep their frozen selection, and a waiting retry keeps its own too, as long as the configuration still names its agent kind and still holds its template. When it does not, for example after `agent.kind` moved to another kind or a rule's prompt file was renamed, the retry is routed again by the rules, stage labels first, and starts without resuming the earlier session. Sortie logs one `Info` record when a retry dispatches on a different selection. A retry whose agent adapter is unavailable, because it failed to start with Sortie, is rescheduled with backoff and keeps its claim until Sortie is restarted.

#### Per-rule template paths

Template paths for rules and `dispatch.default` resolve relative to the directory containing `WORKFLOW.md` (`filepath.Dir(workflow_path)`). The following paths are rejected at load time:

- Absolute paths (starting with `/`).
- `~`-prefixed paths.
- Paths that resolve, after symlink evaluation, outside the workflow directory tree.

An empty `template` field falls through to the Markdown body (the same behavior as omitting the field).

#### Partials

A partial is a file that holds shared text for your prompt templates. It contains only `define` blocks, named pieces of template text, and every prompt template of the workflow can call them with `{{ template "name" . }}`. The Markdown body of `WORKFLOW.md`, the default template, and each rule template are all prompt templates. Without partials, each template file compiles alone, so shared text is copied into every file.

```yaml
dispatch:
  partials:
    - ./partials/*.md
  rules:
    - name: bug-fix
      match:
        issue_type: ["Bug"]
      template: ./prompts/bug.md
    - name: feature
      match:
        issue_type: ["Feature"]
      template: ./prompts/feature.md
```

```text
partials/shared.md
  {{ define "context" -}}
  Resolve {{ .issue.identifier }}: {{ .issue.title }}
  {{- end }}
  {{ define "layout" -}}
  {{ template "context" . }}
  {{ template "first_run_steps" . }}
  {{- end }}

prompts/bug.md
  {{ define "first_run_steps" }}Reproduce the bug first.{{ end }}
  {{- template "layout" . }}

prompts/feature.md
  {{ define "first_run_steps" }}Write the specification first.{{ end }}
  {{- template "layout" . }}
```

Both rule templates load, and each renders its own `first_run_steps` through the shared `layout`. The Markdown body of the same workflow is a prompt template too, so it loads only if it does not call `layout`, or defines `first_run_steps` itself.

To give prompt templates a shared default for a block they would otherwise each define, define the default in a partial under another name, and let a prompt template forward to it:

```text
partials/shared.md
  {{ define "default_first_run_steps" }}Read the issue, then plan before you edit.{{ end }}

prompts/docs.md
  {{ define "first_run_steps" }}{{ template "default_first_run_steps" . }}{{ end }}
  {{- template "layout" . }}
```

The rules:

- **Paths.** An entry follows the rules of a rule template path: relative to the `WORKFLOW.md` directory, with no `$VAR` and no `~` expansion. An absolute entry, a `~`-prefixed entry, an entry that leaves the workflow directory tree, and a file that resolves outside it after symlink evaluation fail the load. A selected file must be a regular file.
- **Patterns.** An entry that contains `*`, `?`, or `[` is a glob pattern and expands to the matching files in lexical order. Only the entry is a pattern: the path of the workflow directory never takes part in the match, so a `[` in its name does no harm. `*` and `?` do not match `/`, and `**` is not recursive. Write `/` between path elements on every OS; on Windows `\` also separates elements, and elsewhere `\` escapes the next character. Write `[[]` to match a literal `[` on every OS.
- **Dot files.** A pattern skips files whose name starts with `.`, such as an editor lock file (`.#shared.md`) or a macOS `._shared.md`, unless the last element of the entry starts with `.`.
- **Empty and missing.** An entry that matches no file fails the load. An entry without a pattern character that names a missing file fails with the cause the filesystem reports.
- **Once per file.** Files load in list order, in lexical order within an entry. A file that several entries select loads once, at its first position.
- **Content.** A partial carries no front matter. It holds only `define` blocks: white space and comments may stand outside them, and any other text or action outside a block fails the load, because the text of a partial is never callable.
- **Names.** A block name has one source. Within a prompt template, a name is defined by one partial or by the template itself, never by two files. That includes a `{{ block }}` and an empty block. The name `prompt` is reserved. Two prompt templates may define the same name, since each compiles on its own.
- **Calls.** A call to a name that nothing defines fails the load for every prompt template, also in a workflow without partials, and also when the call sits in a branch no issue takes.
- **Location.** Every error names the file and the line in that file that hold the fault.

Keep partials in a directory of their own and select them with a `*.md` pattern inside it, as in the example. A pattern that selects a prompt template or `WORKFLOW.md` makes the load fail, because their text sits outside a `define` block.

Sortie reads every partial again on every load, and the file watcher does not watch partials. See [Section 7.2](#72-per-field-reload-behavior) for when an edit applies, and [Section 8](#8-dispatch-preflight-validation) for what `sortie validate` checks.

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

The two named rules carry `match` blocks. The third rule has no `match` block and acts as the terminal catch-all. Each rule carries a `template:` path that is relative to the `WORKFLOW.md` directory and uses forward-slash separators.

#### Further reading (optional)

The design rationale is in [architecture §5.3.9](architecture/05-workflow-specification.md#539-dispatch-object-optional) and [ADR-0031](decisions/0031-let-a-dispatch-rule-carry-its-agent-kinds-settings.md). Neither document is required to write a valid `dispatch` block; they explain why the feature is shaped as it is.

---

### 2.11 `notifications` (operator notification backends)

```yaml
notifications: # ordered list of destinations; optional
  - kind: tracker_comment # built-in: comments on the issue
    events: [session.completed, session.stopped, session.failed, escalation.ci_failure, auto_merge.merged, budget.held]
  - kind: slack
    webhook_url: $SORTIE_SLACK_WEBHOOK_URL # SORTIE_-prefixed reference (mandatory)
    events: [agent.message, session.failed]
    max_per_session: 20                     # optional; 0 selects the default (also 20)
  - kind: telegram
    bot_token: $SORTIE_TELEGRAM_BOT_TOKEN  # SORTIE_-prefixed reference (mandatory)
    chat_id: -1001234567890                 # required; or "@channelusername", or a $VAR reference
    events: [agent.message, session.failed]
  - kind: discord
    webhook_url: $SORTIE_DISCORD_WEBHOOK_URL # SORTIE_-prefixed reference (mandatory)
    events: [agent.message, session.failed]
  - kind: webhook
    url: $SORTIE_OPS_WEBHOOK_URL            # SORTIE_-prefixed reference (mandatory)
    events: [session.started, session.completed, session.stopped, session.failed]
```

The `notifications` list decides where Sortie sends its messages. Each entry is one destination, and its `events` list names the event types it receives. Two things produce events: the agent, which calls the `notify_operator` tool to escalate a decision, report progress, or flag a blocker (event `agent.message`), and Sortie itself, which produces every other event in the catalog below. Routing is a pure function of the configuration and the event type: the agent never chooses a destination, and an event reaches only the destinations that list it.

The `notify_operator` tool is registered only when at least one entry receives `agent.message`, so the agent is never offered a tool whose messages reach no destination.

The value is a sequence, not a single object. A second channel is a second list entry. Each entry is a map carrying a required `kind` discriminator, an `events` list, and that backend's own fields.

| Field | Type | Required | Default | Description |
| ----- | ---- | -------- | ------- | ----------- |
| `kind` | string | Yes | _(none)_ | Destination discriminator. v1 backends are `webhook`, `slack`, `telegram`, and `discord`. The reserved value `tracker_comment` selects the built-in destination that comments on the issue. |
| `events` | list of strings | On `tracker_comment`; otherwise optional | `[agent.message]` for every backend kind | The event types the entry receives, written explicitly. Each name must come from the catalog below, none may repeat, and there is no wildcard. An entry of a backend kind that omits the key receives `agent.message` only, as before the key existed, and draws a deprecation warning that suggests `events: [agent.message]`. A `tracker_comment` entry must write the key; `[]` means only the comments that deprecated settings enable. |
| `max_per_session` | int | No | `20` | Notification cap for the whole agent run. `0` selects the default (`20`); it never means unlimited. A negative value is rejected. It counts only `agent.message` calls and has no effect on an entry that does not receive that event. |

Per-backend fields depend on `kind` and are passed through to the backend untyped:

| `kind` | Field | Description |
| ------ | ----- | ----------- |
| `webhook` | `url` | Endpoint that receives an HTTP POST of the notification as a JSON object using generic field names. |
| `slack` | `webhook_url` | Slack incoming webhook URL that receives a Slack-shaped JSON body with a `text` field. |
| `telegram` | `bot_token` | The bot's token, given as a `SORTIE_`-prefixed reference. Required. Sortie never logs it. |
| `telegram` | `chat_id` | The target chat. A numeric id written as a number or as a string, or a public channel's or supergroup's `@username`. Required. |
| `discord` | `webhook_url` | The Discord channel webhook URL, given as a `SORTIE_`-prefixed reference. Required. Sortie never logs it. |
| `tracker_comment` | _(none)_ | Takes only `kind` and `events`. Any other key is rejected. |

A `telegram` entry sends each message as plain text through the Bot API `sendMessage` method. The text has the same content as a Slack message and is cut to Telegram's 4096-character limit, never rejected. A failed send is logged with a category, and nothing is retried. For this kind, `unauthorized (HTTP 403)` means the bot is not a member or an admin of the chat, not that the token is bad; a bad token is `unauthorized (HTTP 401)`. `unexpected response (HTTP 400)` is usually a wrong `chat_id` or a group that was upgraded to a supergroup, which changes its id. `rate limited (HTTP 429)` is a burst past about 20 messages per minute to one group.

A `discord` entry posts each message to the webhook as the message content. The text has the same content as a Slack message and is cut to Discord's 2000-character limit, never rejected. Sortie turns off mention parsing, so a message never pings a user, a role, `@everyone`, or `@here`, whatever the issue or the agent wrote. A failed send is logged with a category, and nothing is retried.

#### Event catalog

The catalog is closed. Validation accepts exactly these names and rejects any other.

| Event type | Sent when | Severity |
| ---------- | --------- | -------- |
| `session.started` | A session is dispatched on an issue whose dispatch drives issue state, after the in-progress transition and before the workspace is prepared. A retry that follows a run refused by a provider usage limit does not publish this event. | `info` |
| `session.completed` | A worker exits normally. | `info` |
| `session.stopped` | A worker exits on a soft stop: `blocked`, `needs-human-review`, or `no-change-needed`. It carries the agent's reason when the agent wrote one ([The agent's reason on a stop](#the-agents-reason-on-a-stop)). | `warning`; `info` for `no-change-needed` |
| `session.failed` | A worker exits with an error, or its handoff is withheld by the handoff-evidence policy. When a provider usage limit refuses an issue's runs one after another, only the first refusal publishes this event; after a run of the issue that the limit did not refuse, the next refusal publishes it again. | `warning` |
| `escalation.ci_failure`, `escalation.review_comments`, `escalation.bot_review`, `escalation.merge_conflicts`, `escalation.auto_merge`, `escalation.merge_completion` | The matching reaction ([Section 2.9](#29-reactions--reaction-based-feedback-loops)) hands its subject to a person, under any `escalation` value. | `warning` |
| `auto_merge.merged` | The auto-merge reaction merges a pull request, or its merge call finds the pull request already merged. | `info` |
| `budget.held` | An issue is held out of dispatch by `agent.max_sessions`, `agent.max_tokens`, or `agent.max_cost_usd`. | `warning` |
| `stage.advanced` | A stage hop is made: the issue moves to the next stage, even when removing an old stage label failed ([Stage chains](#stage-chains)). | `info` |
| `stage.not_advanced` | A stage hop is due and not made, because the label add failed or the hop ceiling is reached. The event carries the reason, `failed` or `ceiling`. | `warning` |
| `agent.message` | The agent calls `notify_operator`. | Set by the agent. |

A run cancelled by Sortie produces no session event. Sortie never counts its own events against `max_per_session`, and an agent that has used its cap never suppresses one.

#### The `tracker_comment` destination

A `tracker_comment` entry posts each event it receives as a comment on the event's issue, using the same text Sortie has always posted for that moment. Sortie's own text never carries error text, the agent's session identifier, or the agent kind, because a tracker issue can be read by people outside your deployment. The `webhook` destination receives the full notification. The one addition is the agent's reason on a stop, which follows Sortie's text in the same comment ([The agent's reason on a stop](#the-agents-reason-on-a-stop)).

- At most one entry has this kind, and it requires a configured `tracker.kind`.
- It requires `events`, and `agent.message` is rejected there: messages from the agent never reach the issue.
- It receives the union of three sets: the events its entry lists, the events that the deprecated `tracker.comments` settings and `escalation: comment` enable, and, while no `tracker_comment` entry exists, `auto_merge.merged` and `budget.held`. Writing the new form never withdraws a comment an old setting enabled, and one event is delivered at most once to a destination however many of these select it. Keeping `tracker.comments.on_completion: true` and also listing `session.completed` on the entry gives one comment per completion, not two.

**An explicit entry is authoritative for two comments.** The auto-merge success comment and the budget-hold comment have never had a setting of their own: they post whenever a tracker is configured. They keep posting only while no `tracker_comment` entry exists. Once you write an entry, it posts `auto_merge.merged` and `budget.held` only when it lists them, so an entry that omits them stops those comments. With no `tracker.kind` there is no issue to comment on and nothing posts.

#### The agent's reason on a stop

When an agent stops on `blocked`, `needs-human-review`, or `no-change-needed`, it can write its reason on the lines after the status in `.sortie/status`. The first line is the status and everything after it is the reason. Sortie asks the agent for one in the instructions it adds to the first prompt of every run, and asks it to keep the whole file under 1024 bytes, because Sortie reads no more than that.

Sortie publishes the reason with the `session.stopped` event. It also stores the reason with the run and serves it on the run's row in the run history API ([run fields](#get-apiv1runs-run-history)), a withheld `needs-human-review` run included. The dashboard shows the reason beside the run on the issue page, the session view, and History. A `session.failed` event never carries one, even when a `needs-human-review` run ended as a failure because its handoff was withheld, and neither does an `agent.message` notification. A reason that comes with an unrecognized status is ignored. When the agent writes a status inside a self-review turn, only a `blocked` there replaces the reason already held, and a `no-change-needed` declaration that self-review retracts takes its reason with it.

Before a reason leaves Sortie, Sortie removes control characters, replaces bytes that are not valid text, masks every secret value it knows from its own configuration and environment (values shorter than 8 bytes are left readable), and, when the file was cut at 1024 bytes, also drops any partial secret left at the cut and ends the reason with an ellipsis. Only those secrets are masked. Sortie does not recognize other credentials or paths, so a statement that quotes one is published as written.

| Destination | What it receives |
| ----------- | ---------------- |
| `tracker_comment` | Sortie's usual comment, a blank line, and the reason. |
| `slack` | Sortie's usual text, a blank line, and the reason. |
| `telegram` | The same text as Slack, cut to Telegram's 4096-character limit. |
| `discord` | The same text as Slack, cut to Discord's 2000-character limit. |
| `webhook` | The reason in the `agent_text` key, apart from `body`. |

A status with no reason, or a reason that is empty after this cleanup, changes nothing: the comment and the notifications are exactly what Sortie sent before the reason existed.

#### Moving from the deprecated settings

A configuration written before this key existed, with completion and failure comments, a commenting CI reaction, and one Slack channel for the agent:

```yaml
tracker:
  kind: github
  api_key: $SORTIE_GITHUB_TOKEN
  project: acme/billing-api
  comments:
    on_completion: true
    on_failure: true

reactions:
  ci_failure:
    provider: github
    escalation: comment

notifications:
  - kind: slack
    webhook_url: $SORTIE_SLACK_WEBHOOK_URL
```

The same comments and messages in the new form. The two comments that had no setting are listed because an explicit entry ends their implicit delivery; neither feature is configured here, so they post nothing until it is:

```yaml
tracker:
  kind: github
  api_key: $SORTIE_GITHUB_TOKEN
  project: acme/billing-api

reactions:
  ci_failure:
    provider: github
    escalation: none                 # no label; the comment comes from the entry below

notifications:
  - kind: tracker_comment
    events: [session.completed, session.stopped, session.failed, escalation.ci_failure, auto_merge.merged, budget.held]
  - kind: slack
    webhook_url: $SORTIE_SLACK_WEBHOOK_URL
    events: [agent.message]
```

The old form keeps working. Each deprecated form the configuration relies on draws one warning that names its replacement, once per configuration change and not per event. `sortie validate` shows the same warnings without changing its verdict or exit status, and the dry run shows them too. Sortie sets no removal date.

| Deprecated form | Replacement |
| --------------- | ----------- |
| `tracker.comments.on_dispatch: true`, or `SORTIE_TRACKER_COMMENTS_ON_DISPATCH` | List `session.started` in a `tracker_comment` entry, then remove the key. |
| `tracker.comments.on_completion: true`, or `SORTIE_TRACKER_COMMENTS_ON_COMPLETION` | List `session.completed` and `session.stopped`, then remove the key. |
| `tracker.comments.on_failure: true`, or `SORTIE_TRACKER_COMMENTS_ON_FAILURE` | List `session.failed`, then remove the key. |
| `reactions.<kind>.escalation: comment` on an active reaction | Set `escalation: none` and list `escalation.<kind>` in a `tracker_comment` entry. |
| The auto-merge success comment, with the auto-merge reaction active, a tracker configured, and no `tracker_comment` entry | Add an entry that lists `auto_merge.merged`. |
| The budget-hold comment, with `agent.max_sessions`, `agent.max_tokens`, or `agent.max_cost_usd` above `0`, a tracker configured, and no `tracker_comment` entry | Add an entry that lists `budget.held`. |
| An entry of a backend kind without `events` | Add `events: [agent.message]`. |

#### Delivery

Sortie sends an orchestrator event to every destination that lists it, all at once. A failure to deliver to one destination is logged and never stops the others, and nothing is retried. A slow notification endpoint never delays an agent's start and never delays the comment on the issue. A reload whose destinations for orchestrator events cannot be built, such as an unknown `kind` or a required secret that resolved to an empty string, is rejected, and the previous configuration stays in force ([Section 7.2](#72-per-field-reload-behavior)).

The `notifications` `webhook` backend is an outbound POST to an operator-supplied endpoint. It is unrelated to inbound tracker webhooks ([architecture §20](architecture/25-webhook-support.md)), which trigger reconciliation. The two share a name but not a direction.

When more than one entry receives `agent.message`, the effective cap is the maximum non-zero `max_per_session` across those entries, falling back to the default when every one is `0` or unset. The cap covers one agent run on every agent kind: every turn and every tool server process of that run share one count, which Sortie keeps as files in the workspace's `.sortie/notification_slots/` directory. A retry or a continuation starts a new run and a new count. A call counts once at least one backend accepts the notification, not once per backend that accepts it; if no backend accepts it, the reserved slot is released and the call does not count.

The `webhook` backend posts a JSON object whose keys use the generic notifier vocabulary, so any consumer can correlate and route without backend-specific knowledge:

| Key | Type | Meaning |
| --- | ---- | ------- |
| `notification_id` | string | Generated unique id for this notification. |
| `timestamp` | string | Send time, ISO-8601 UTC. |
| `source` | string | The Sortie instance identifier; the hostname by default. |
| `issue_id` | string | Tracker-internal issue id. |
| `identifier` | string | Human-readable issue key. |
| `dispatch_id` | string | Identifies the one agent run that sent the notification. New on every retry and continuation. |
| `session_id` | string | The agent's own session id once reported; `""` until then, and always `""` for an agent that reports none. Repeats across runs that continue one conversation. |
| `attempt` | integer or null | Retry or continuation attempt; `null` on the first run. |
| `agent` | string | Dispatch-frozen agent kind. |
| `severity` | string | `info`, `warning`, or `critical`. |
| `title` | string | Short summary: the agent's own for `agent.message`, and `<issue>: <event type>` for an event Sortie produces. |
| `body` | string | Notification detail: the agent's own for `agent.message`, and the text Sortie posts as the comment for an event it produces. |
| `category` | string | Optional: `decision_needed`, `progress`, `blocked`, `completed`, or `other`. Absent when the agent did not set it. |
| `event_type` | string | The event type from the catalog above. Present only on an event Sortie produces. An `agent.message` payload carries exactly the keys above and no `event_type`, so an endpoint configured before this key existed receives the same JSON as before. |
| `agent_text` | string | The agent's reason for stopping, after the cleanup described in [The agent's reason on a stop](#the-agents-reason-on-a-stop). Present only on a `session.stopped` event whose agent wrote a reason, and never inside `body`. |
| `stage` | object | The hop decision, with the keys `source_rule`, `target_rule`, `chain_id`, `hop_count`, and, on `stage.not_advanced` only, `reason`. Present only on a `stage.advanced` or `stage.not_advanced` event. |

**`SORTIE_`-prefixed secret rule:**

A backend secret MUST be given as a reference to a `SORTIE_`-prefixed environment variable, written as `$SORTIE_NAME` or `${SORTIE_NAME}`. The prefix is mandatory for an entry that receives `agent.message`, and not stylistic: the `notify_operator` tool runs in a separate `sortie mcp-server` process that receives only the workflow file path and the orchestrator's `SORTIE_`-prefixed variables. A reference without the `SORTIE_` prefix, or to an unset variable, resolves to an empty string in that process. The backend rejects an empty required secret, which surfaces as a fatal startup error rather than a notification posted nowhere. An entry that receives only events Sortie produces is built only in the main process, where the prefix is not required, but name every notification secret with the `SORTIE_` prefix so that adding `agent.message` to the entry later does not break it.

---

## 3. Environment Variable Overrides

Sortie supports a curated set of `SORTIE_*` environment variables that override YAML front matter values. This enables twelve-factor app deployment patterns: operators inject secrets, endpoint URLs, and tuning parameters via environment rather than committing them to a workflow file.

### 3.1 Source precedence

Configuration sources are resolved in the following order (highest to lowest):

1. **Workflow file path selection** (runtime setting → cwd default).
2. **`SORTIE_*` real environment variables**.
3. **`.env` file values** (when `SORTIE_ENV_FILE` or `--env-file` is set).
4. **YAML front matter values**.
5. **`$VAR` indirection** inside YAML values — applies only to values that survive the merge (fields not overridden by env).
6. **Built-in defaults**.

An env override replaces the YAML value in the raw config map *before* `$VAR` expansion and section builders run. If `WORKFLOW.md` says `api_key: $MY_TOKEN` and `SORTIE_TRACKER_API_KEY=secret`, the env var value `secret` replaces the YAML value entirely. The `$MY_TOKEN` indirection never executes for that field.

After the overrides and `$VAR` resolution, and before the typed sections are built, Sortie converts any agent kind that a release has removed onto its replacement kind. The conversion rewrites the configuration in memory on every load, at startup, on reload, and in `sortie validate`; it never writes `WORKFLOW.md`.

### 3.2 Curated variable list

Each variable maps to exactly one config field. The naming convention is `SORTIE_<SECTION>_<FIELD>` with underscores separating words.

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
| `SORTIE_TRACKER_NO_CHANGE_STATE`           | `tracker.no_change_state`        | string |                                |
| `SORTIE_TRACKER_IN_PROGRESS_STATE`         | `tracker.in_progress_state`      | string |                                |
| `SORTIE_TRACKER_COMMENTS_ON_DISPATCH`      | `tracker.comments.on_dispatch`   | bool   | `true`/`false`/`1`/`0`; deprecated, see [Section 2.11](#211-notifications-operator-notification-backends) |
| `SORTIE_TRACKER_COMMENTS_ON_COMPLETION`    | `tracker.comments.on_completion` | bool   | `true`/`false`/`1`/`0`; deprecated, see [Section 2.11](#211-notifications-operator-notification-backends) |
| `SORTIE_TRACKER_COMMENTS_ON_FAILURE`       | `tracker.comments.on_failure`    | bool   | `true`/`false`/`1`/`0`; deprecated, see [Section 2.11](#211-notifications-operator-notification-backends) |

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
| `SORTIE_AGENT_COMMAND`               | `agent.command`               | string | Sets the string form. It replaces an `agent.command` list written in the file. |
| `SORTIE_AGENT_TURN_TIMEOUT_MS`       | `agent.turn_timeout_ms`       | int    |       |
| `SORTIE_AGENT_READ_TIMEOUT_MS`       | `agent.read_timeout_ms`       | int    |       |
| `SORTIE_AGENT_STALL_TIMEOUT_MS`      | `agent.stall_timeout_ms`      | int    |       |
| `SORTIE_AGENT_STOP_GRACE_MS`         | `agent.stop_grace_ms`         | int    |       |
| `SORTIE_AGENT_MAX_CONCURRENT_AGENTS` | `agent.max_concurrent_agents` | int    |       |
| `SORTIE_AGENT_MAX_TURNS`             | `agent.max_turns`             | int    |       |
| `SORTIE_AGENT_MAX_RETRY_BACKOFF_MS`  | `agent.max_retry_backoff_ms`  | int    |       |
| `SORTIE_AGENT_MAX_SESSIONS`          | `agent.max_sessions`          | int    |       |
| `SORTIE_AGENT_MAX_TOKENS`            | `agent.max_tokens`            | int    |       |
| `SORTIE_AGENT_MAX_COST_USD`          | `agent.max_cost_usd`          | number |       |
| `SORTIE_AGENT_MAX_DAILY_COST_USD`    | `agent.max_daily_cost_usd`    | number |       |
| `SORTIE_AGENT_MAX_WEEKLY_COST_USD`   | `agent.max_weekly_cost_usd`   | number |       |
| `SORTIE_AGENT_MAX_MONTHLY_COST_USD`  | `agent.max_monthly_cost_usd`  | number |       |
| `SORTIE_AGENT_TOKEN_WARNING_PERCENT` | `agent.token_warning_percent` | int    |       |
| `SORTIE_AGENT_MAX_CONSECUTIVE_ABSENCES` | `agent.max_consecutive_absences` | int |    |

#### Top-level

| Environment variable | Config field | Type   | Notes                                 |
| -------------------- | ------------ | ------ | ------------------------------------- |
| `SORTIE_DB_PATH`     | `db_path`    | string | `~` expansion applies; `$VAR` skipped |
| `SORTIE_PRICE_TABLE_FETCH` | `price_table.fetch` | bool | On by default; `false` turns off the download of the shared price table; `price_table.replace_url` is not affected |

#### Control variable

| Environment variable | Purpose                     | Type   | Notes                              |
| -------------------- | --------------------------- | ------ | ---------------------------------- |
| `SORTIE_ENV_FILE`    | Path to `.env` file to load | string | Default: empty (no `.env` loading) |

### 3.3 Type coercion

All environment variable values are strings. The override layer coerces them to the expected type before section builders run.

| Target type  | Coercion rule                                                                         | Error behavior                              |
| ------------ | ------------------------------------------------------------------------------------- | ------------------------------------------- |
| `string`     | Used as-is.                                                                           | N/A                                         |
| `int`        | `strconv.Atoi` after trimming whitespace.                                             | `*ConfigError` naming the `SORTIE_*` env var |
| `number`     | `strconv.ParseFloat` after trimming whitespace; accepts an integer or a decimal.      | `*ConfigError` naming the `SORTIE_*` env var |
| `bool`       | `"true"`, `"1"` → `true`; `"false"`, `"0"` → `false` (case-insensitive).            | `*ConfigError` naming the `SORTIE_*` env var |
| `csv`        | `strings.Split(val, ",")` then trim each element; empty elements discarded.           | N/A                                         |

A `number` value of `NaN`, an infinity, or below `0` passes coercion and fails the field's own check ([Section 9.3](#93-environment-variable-errors)).

**CSV encoding for list fields** (`active_states`, `terminal_states`):

```
SORTIE_TRACKER_ACTIVE_STATES="To Do,In Progress"
SORTIE_TRACKER_TERMINAL_STATES="Done,Won't Do"
```

- Items are trimmed of leading/trailing whitespace.
- Empty items (from trailing commas or `,,`) are discarded.
- If the environment variable is unset or set to an empty string, the YAML-configured states are used; there is no environment override to force an empty list.
- State values preserve original casing.

### 3.4 `.env` file support

Sortie supports an optional `.env` file for operators who prefer file-based secrets over shell environment.

#### Loading

- `.env` loading is **opt-in**: set `SORTIE_ENV_FILE=/path/to/.env` as a real environment variable, or pass `--env-file /path/to/.env` on the CLI.
- Sortie does **not** auto-discover `.env` in the working directory. Operators MUST opt in explicitly.
- When the path is set but the file does not exist, Sortie logs a warning and continues without `.env` values.
- When the file exists but has parse errors, Sortie fails startup with an error identifying the file and line number.

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
- Values MAY be quoted with single or double quotes; quotes are stripped but no escape processing is performed.
- Keys MUST match `[A-Za-z_][A-Za-z0-9_]*`.
- Only `SORTIE_*` prefixed keys are loaded; other keys are silently ignored.
- Variable interpolation within `.env` values is **not** supported.

#### Precedence

Non-empty real environment variables win over `.env` file values. Empty real environment variables are treated as unset and fall back to the `.env` value. The `.env` file provides defaults for env vars not already set in the process environment. The `--env-file` CLI flag takes precedence over the `SORTIE_ENV_FILE` environment variable when resolving the file path.

### 3.5 Interaction with `$VAR` indirection

Values injected by environment overrides are already fully resolved. They MUST NOT be passed through `os.ExpandEnv`, `resolveEnv`, or `resolveEnvRef`. This prevents double-expansion that would corrupt values containing `$` characters.

For path fields (`workspace.root`, `db_path`), tilde (`~`) expansion still applies to env-sourced values. Only `$VAR` expansion is skipped.

**Example:** If `SORTIE_TRACKER_API_KEY=tok$5abc` is set, the literal value `tok$5abc` is used as the API key. Without this guard, `os.ExpandEnv` would attempt to expand `$5abc` as an environment variable reference.

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
| `notifications`                        | List of pass-through destination maps; no single-value representation. Backend secrets are referenced via `$SORTIE_*` indirection from inside the entry (see Section 2.11), not as field-level overrides. |
| `reactions.*` (including `reactions.label_commands`) | No override variables exist; reaction configuration comes from WORKFLOW.md |
| `dispatch.*`                           | No override variables exist; rule definitions and template paths come from WORKFLOW.md |
| `self_review.*`                        | Verification commands are security-sensitive privileged configuration that must come from the version-controlled WORKFLOW.md |
| Extensions (`server`, `worker`, `token_rates`, the rest of `price_table`, etc.) | Extension-defined; would couple core env parsing to extensions. `price_table.fetch` is the one extension field with an override, `SORTIE_PRICE_TABLE_FETCH` |
| `logging.level` (via extensions)       | Resolved from `--log-level` flag; not part of typed config layer |

### 3.7 Dynamic reload

On WORKFLOW.md reload, `applyEnvOverrides` re-reads both `os.Getenv` and the `.env` file. Env overrides merge into the fresh raw map before section builders run.

- **`.env` file changes are picked up** on each reload. The `.env` file itself is not watched by fsnotify — only WORKFLOW.md changes trigger reload.
- **Real environment variable changes require a process restart.** `os.Getenv` reads the current process environment. While Go can observe in-process changes via `os.Setenv`, operator-provided env vars are inherited at process launch and are effectively immutable from outside the running process.

For configuration values that need to change without restarting, use the `.env` file and trigger a WORKFLOW.md reload. The tracker fields `tracker.kind`, `tracker.endpoint`, `tracker.api_key`, `tracker.project`, `tracker.query_filter`, and `tracker.api_version` are the exception: the tracker adapter reads them once at startup, so they take effect at the next restart, also when set through `.env`.

---

## 4. Extensions

The front matter is extensible. Unknown top-level keys are collected into an `Extensions` map and are not validated by the core schema. Extensions should document their own field schemas, defaults, and reload behavior.

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

Sortie starts an HTTP server by default on `127.0.0.1:7678` for runtime observability and operational control. `server.port` overrides the default port; `server.host` overrides the default bind address. CLI `--port` and `--host` flags take precedence over their extension counterparts. Port `0` disables the server entirely (no TCP listener, no Prometheus metrics).

A reverse proxy in front of Sortie must pass on the `Host` header the browser sent unchanged, including its port. When a browser sends no `Sec-Fetch-Site` header, Sortie compares it with the `Origin` header of a state-changing request, and a proxy that rewrites it makes Sortie refuse every dashboard action from that browser.

#### API Endpoints

| Method | Path                     | Description                                                        |
| ------ | ------------------------ | ------------------------------------------------------------------ |
| GET    | `/`                      | Redirects to `/ui/` with `302 Found`. |
| GET    | `/ui/`                   | The dashboard: a client-side application that reads the JSON API and refetches when the change stream signals a change: run-history reads (History, the older runs of an issue page or a session view, and the issue lookup of the command palette) at most once per 5 seconds and Analytics at most once per 30 seconds. Prices reads its route each time it opens and never refetches on a change signal. |
| GET    | `/livez`                 | Liveness probe. Returns 200 while the process is running and not draining, 503 during graceful shutdown. No I/O. |
| GET    | `/readyz`                | Readiness probe. Returns 200 when database, preflight, and workflow are healthy. Returns 503 with per-check status when any dependency fails. |
| GET    | `/api/v1/state`          | System-wide runtime snapshot (running sessions, retry queue, pending pull request reactions, parked issues, the eligible issues Sortie holds back from dispatch, one entry per active issue in a stable order, the list of what needs a person, aggregate token/runtime totals, the usage windows each agent kind reported, the effective workflow shape, the health of Sortie's own checks, the Sortie version, and the process start time). Every row of an issue carries a plain English status sentence. |
| GET    | `/api/v1/runs`           | Run history, newest first, one page at a time, narrowed by issue, dispatch rule, status, stage, completion-time range, and the range in which the issue ended. Every `issue_outcome` carries the sentence of how its issue ended. |
| GET    | `/api/v1/runs/issues`    | Run history grouped by issue, the issue with the newest matching run first, one page of issues at a time, narrowed by issue key (the `owner/repo#9` form included), search text matched against the key and the title, dispatch rule, status, and stage. Each group carries the sentence of how its issue ended or what it is doing now, the issue's newest runs, and its totals. |
| GET    | `/api/v1/runs/{dispatch_id}/events` | The agent events Sortie kept for one run, oldest first, each with a plain English sentence. A running run is served from memory and a finished run from the store, for at least 365 days after it ends. Returns 404 for a run whose events are not retained. |
| GET    | `/api/v1/stats`          | Run statistics over the runs completed in a `since` and `until` range: the report `sortie stats --format json` prints, with the issue count, the runs grouped by retry trigger and by what started them, and a funnel for each stage chain, the skill calls of the runs counted per skill, with the cost recorded with each run and that cost per UTC day or month. |
| GET    | `/api/v1/prices`         | The rates in force when the request is served, per agent kind and model, with the layer each rate comes from and whether that run is priced in full, in part, or not at all, and the state of every price source: your `token_rates`, your own price table, the downloaded table, and the embedded table. It is what the dashboard's Prices screen shows. See [`GET /api/v1/prices`: prices in force](#get-apiv1prices-prices-in-force). |
| GET    | `/api/v1/{identifier}`   | Per-issue detail for a specific issue identifier, including a parked one, one that only waits on a pending pull request reaction, and one known only through run history, with its runs. Carries a plain English status sentence for the issue, lists what Sortie does next for it as condition and sentence pairs, and lists the latest 50 agent events of its running or latest run as `recent_events`. Returns 404 only when neither the current state nor run history knows the identifier. |
| POST   | `/api/v1/refresh`        | Trigger an immediate poll+reconciliation cycle. Returns 202 Accepted normally, 409 Conflict during graceful shutdown. Best-effort; repeated requests are coalesced. Every response says in one sentence what it queued. |
| POST   | `/api/v1/retries/{identifier}/run` | Make an issue's scheduled retry due now; the retry then goes through the same checks as a retry whose time came, and one that still cannot start keeps the time it was due before. Returns 202 Accepted, 404 when no retry is scheduled for the identifier, and 409 Conflict during graceful shutdown. Every 202 and 409 response says in one sentence what it did. |
| POST   | `/api/v1/attention/{id}/acknowledge` | Record that a person has seen an item that needs a person: it leaves the attention count and stays listed with the time it was acknowledged until its cause clears, and a new cause shows as a new item. Changes nothing in the tracker or the pull request. Returns 200 OK, 404 when no current item has the ID, and 409 Conflict during graceful shutdown. Every 200 and 409 response says in one sentence what it did. |
| POST   | `/api/v1/reactions/{identifier}/{kind}/check` | Make a pending pull request reaction check its pull request on the next poll tick instead of at its scheduled time. The check follows every rule of the reaction, and the request itself starts no agent and changes nothing in the tracker or the pull request. A reaction still waiting after that check keeps the time its next check was due before, so asking early never makes it wait longer. Returns 202 Accepted, 404 when the issue has no pending reaction of that kind, and 409 Conflict during graceful shutdown. Every 202 and 409 response says in one sentence what it did. |
| GET    | `/api/v1/events`         | Server-sent event stream: one `change` event each time anything the JSON API serves changes, at most one a second, a `heartbeat` event every 15 seconds, and, for each run named with `run`, the run's agent events. A change signal carries no data; a client refetches what it shows. See [`GET /api/v1/events`: change signals and agent events](#get-apiv1events-change-signals-and-agent-events). |
| GET    | `/metrics`               | Prometheus exposition-format scrape endpoint. Present only when `github.com/prometheus/client_golang` metrics are enabled (always co-located with the HTTP server). |

All responses use `Content-Type: application/json; charset=utf-8` (JSON endpoints), except `GET /api/v1/events`, which uses `text/event-stream`. Error responses use a standard envelope: `{"error": {"code": "...", "message": "..."}}`. A state-changing request that a browser sends from a page on another origin gets `403` with the error code `cross_origin_request_rejected` on every path, health probes included, and this comes before every 404 or 405 stated here for a route. Every GET, HEAD, and OPTIONS request is not affected, and neither is a state-changing request that carries `Sec-Fetch-Site: same-origin` or `none`, or that carries no `Sec-Fetch-Site` header and either no `Origin` header or an `Origin` whose host equals the `Host` header. Any other `Sec-Fetch-Site` value is refused whatever `Origin` says. API endpoints (`/api/v1/*`) return 405 with the JSON error envelope. Health probes (`/livez`, `/readyz`) return the standard HTTP 405 plain-text response. The `/metrics` endpoint returns `text/plain` in Prometheus exposition format. The `/ui/` dashboard returns `text/html` for its document and the content type of each file for its assets.

#### Health Endpoints

Sortie exposes Kubernetes z-pages health endpoints (`/livez` and `/readyz`) for liveness and readiness probes.

**`GET /livez`** — Liveness probe. Returns 200 when the process is alive, 503 during graceful shutdown. No I/O; a single atomic flag check:

```json
{"status": "pass"}
```

During graceful shutdown:

```json
{"status": "fail"}
```

**`GET /readyz`** — Readiness probe. Returns 200 when all dependencies are healthy, 503 when any check fails. Checks: SQLite database ping, dispatch preflight validation, workflow file loaded:

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

The readiness checks keep their meaning. `workflow` passes while a configuration is in effect, even after an edit to WORKFLOW.md fails to load, because Sortie keeps running on the last configuration that loaded. The `health` block of `GET /api/v1/state` reports each check's last success, consecutive failures, and error text, a failed reload included.

**Draining behavior.** When `SIGTERM` arrives, Sortie sets a draining flag before the orchestrator begins its worker drain phase. Both `/livez` and `/readyz` return 503 once the flag is set. The HTTP listener remains open during drain so K8s probes receive proper HTTP responses. After the orchestrator drain completes, the listener closes and new connections are refused.

#### `GET /api/v1/state` — Runtime Snapshot

Returns the system-wide runtime state including running sessions, retry queue, budget-exhausted and parked issues, pending pull request reactions, issues between stages, the eligible issues Sortie holds back from dispatch (`held`), one entry per active issue in a stable order (`active`), the list of what needs a person (`attention`) with the time a person acknowledged each item, aggregate token/runtime totals, the usage windows each agent kind reported (`rate_limits`, keyed by agent kind: each member holds `reported_at` and `windows`, each window holds `label`, `used_percent`, `status`, `resets_at`, `reported_at`, and `amount`, a window past its reset time is not served, and a share, status, or reset the report did not carry is `null`). `amount` holds `used`, `limit`, and `unit` for a window the agent counts in its own unit, such as credits, and is `null` for a window without one. Sortie never converts the unit, and a window that reports an amount and no share serves their ratio as `used_percent`, and the effective workflow shape (`config`), which names the tracker kind and project. It also names the Sortie version and the time the process started, and carries the `health` block, the health of Sortie's own checks.

```json
{
  "generated_at": "2026-02-24T20:15:30Z",
  "version": "0.4.0",
  "started_at": "2026-02-24T19:11:28Z",
  "counts": {
    "running": 2,
    "retrying": 1,
    "budget_exhausted": 1,
    "parked": 1,
    "pending_reactions": 2,
    "between_stages": 1,
    "held": 1,
    "attention": 3
  },
  "running": [
    {
      "issue_id": "abc123",
      "issue_identifier": "MT-649",
      "issue_title": "Fix the flaky login test",
      "issue_url": "https://tracker.example.com/browse/MT-649",
      "issue_labels": ["bug", "backend"],
      "issue_type": "Bug",
      "state": "In Progress",
      "session_id": "thread-1-turn-1",
      "turn_count": 7,
      "self_review_active": false,
      "self_review_iteration": 0,
      "last_event": "turn_completed",
      "last_message": "",
      "started_at": "2026-02-24T20:10:12Z",
      "dispatched_at": "2026-02-24T19:58:40Z",
      "dispatch_id": "RLQFSIOFERWGDW2HZY45W55VXK",
      "last_event_at": "2026-02-24T20:14:59Z",
      "quiet_at": "2026-02-24T20:17:29Z",
      "stall_at": "2026-02-24T20:19:59Z",
      "stall_text": null,
      "stall_text_full": null,
      "stall_off_text": null,
      "workspace_path": "/tmp/sortie_workspaces/MT-649",
      "tokens": {
        "input_tokens": 1200,
        "output_tokens": 800,
        "total_tokens": 2000,
        "cache_read_tokens": 400,
        "cache_write_tokens": 100
      },
      "uncached_tokens": 1600,
      "model_name": "claude-sonnet-4-20250514",
      "rule_name": "implement",
      "stage_lineage": {
        "chain_id": "3c8v5n2w7r1t4y6u9i0o8p2asd",
        "previous": "specify",
        "previous_outcome": "succeeded"
      },
      "stage_position": {
        "chain": ["specify", "implement"],
        "current": "implement",
        "hop_count": 1,
        "hop_ceiling": 10
      },
      "configured_model": "claude-sonnet-4-20250514",
      "configured_effort": "high",
      "workflow_file": "WORKFLOW.md",
      "agent_kind": "claude-code",
      "reaction_kind": "ci",
      "reaction_config_key": "ci_failure",
      "reaction_attempt": 1,
      "pull_request": {
        "number": 927,
        "repository": "acme/web"
      },
      "api_request_count": 3,
      "requests_by_model": {"claude-sonnet-4-20250514": 3},
      "skills": [{"skill": "ce-plan"}, {"skill": "ce-work", "tool_error": true}],
      "tool_time_percent": 12.3,
      "api_time_percent": 45.6,
      "tokens_measured": true,
      "usage_arrival": "incremental",
      "usage_attribution": "per_model",
      "tokens_pending": false,
      "tokens_awaited": false,
      "api_requests_measured": true,
      "used_sessions": 2,
      "budget_sessions": 5,
      "issue_tokens_completed": 53400,
      "unmeasured_sessions": 0,
      "used_tokens": 55000,
      "used_tokens_complete": true,
      "budget_tokens": 500000,
      "budget_usd": null,
      "estimated_cost_usd": 0.014595,
      "cost_status": "estimated",
      "issue_estimated_cost_usd": 0.235095,
      "issue_cost_status": "estimated",
      "status_text": "Fixing CI on #927, fix 1 of 2",
      "status_text_full": "Fixing CI on PR #927, fix 1 of 2."
    }
  ],
  "retrying": [
    {
      "issue_id": "def456",
      "issue_identifier": "MT-650",
      "issue_title": "Retry the export job",
      "issue_url": "https://tracker.example.com/browse/MT-650",
      "issue_type": "Task",
      "issue_labels": ["export"],
      "state": "In Progress",
      "dispatched_at": "2026-02-24T19:41:05Z",
      "attempt": 3,
      "due_at": "2026-02-24T20:16:00Z",
      "error": "no available orchestrator slots",
      "trigger": "timer",
      "rule_name": "implement",
      "agent_kind": "claude-code",
      "configured_model": "claude-sonnet-4-20250514",
      "stage_lineage": {
        "chain_id": "7q2m4x9b1k5d8f3h6j0n2p4rst",
        "previous": "specify",
        "previous_outcome": "succeeded"
      },
      "stage_position": {
        "chain": ["specify", "implement"],
        "current": "implement",
        "hop_count": 1,
        "hop_ceiling": 10
      },
      "paused": false,
      "status_text": "Retry 3 in 30s: no agent slot was free",
      "status_text_full": "The retry came due and was not dispatched: no agent slot was free. Retry 3 in 30s."
    }
  ],
  "budget_exhausted": [
    {
      "issue_id": "ghi789",
      "issue_identifier": "MT-651",
      "issue_title": "Migrate the billing tables",
      "issue_url": "https://tracker.example.com/browse/MT-651",
      "issue_type": "Task",
      "issue_labels": [],
      "state": "In Progress",
      "dispatched_at": "2026-02-24T19:20:00Z",
      "reason": "session_budget",
      "used_sessions": 3,
      "budget_sessions": 3,
      "used_tokens": null,
      "budget_tokens": null,
      "used_usd": null,
      "used_usd_complete": null,
      "budget_usd": null,
      "unmeasured_sessions": null,
      "stopped_in_flight": null,
      "exhausted_at": "2026-02-24T20:12:00Z",
      "rule_name": "everything",
      "stage_lineage": null,
      "stage_position": null,
      "agent_kind": "claude-code",
      "configured_model": "claude-sonnet-4-20250514",
      "status_text": "Held: session budget reached",
      "status_text_full": "Held at the session budget of 3. Sessions used: 3."
    }
  ],
  "parked": [
    {
      "issue_id": "jkl012",
      "issue_identifier": "MT-652",
      "issue_title": "Rotate the signing keys",
      "issue_url": "https://tracker.example.com/browse/MT-652",
      "issue_type": "Task",
      "issue_labels": ["security"],
      "state": "In Progress",
      "dispatched_at": "2026-02-24T19:40:12Z",
      "reason": "agent_blocked",
      "parked_state": "In Progress",
      "label": "needs-human",
      "label_applied": true,
      "parked_at": "2026-02-24T20:05:00Z",
      "rule_name": "default",
      "stage_position": null,
      "agent_kind": "claude-code",
      "configured_model": "claude-sonnet-4-20250514",
      "status_text": "Parked: the agent needs a person",
      "status_text_full": "Parked: the agent stopped because it cannot go on without a person. Parking label: `needs-human`."
    }
  ],
  "pending_reactions": [
    {
      "issue_id": "mno345",
      "issue_identifier": "MT-653",
      "issue_title": "Add the export button",
      "issue_url": "https://tracker.example.com/browse/MT-653",
      "issue_type": "Story",
      "issue_labels": ["export"],
      "state": "In Review",
      "kind": "ci",
      "config_key": "ci_failure",
      "pull_request": {
        "number": 931,
        "repository": "acme/web",
        "url": "https://github.com/acme/web/pull/931",
        "branch": "sortie/MT-653",
        "head_sha": "4f1c2a9e8b7d6c5b4a3f2e1d0c9b8a7f6e5d4c3b"
      },
      "created_at": "2026-02-24T20:09:30Z",
      "dispatched_at": "2026-02-24T19:52:10Z",
      "next_poll_at": "2026-02-24T20:17:40Z",
      "wait": "poll_interval",
      "used_attempts": 2,
      "max_attempts": 2,
      "escalated_at": "2026-02-24T20:12:40Z",
      "escalation_trigger": "budget_exhausted",
      "watch_ends_at": null,
      "last_check": {
        "observed_at": "2026-02-24T20:12:40Z",
        "head_sha": "9b7e3d1c0a2f4e6b8d0c1a3e5f7b9d2c4e6a8f0b",
        "status": "failing",
        "failing_checks": ["Test"],
        "failing": 1,
        "pending": 0,
        "passing": 4
      },
      "rule_name": "default",
      "stage_position": null,
      "agent_kind": "claude-code",
      "configured_model": "claude-sonnet-4-20250514",
      "status_text": "CI escalated on #931",
      "status_text_full": "Sortie escalated failing CI on PR #931; it keeps watching the pull request. CI fixes used: 2 of 2."
    },
    {
      "issue_id": "mno345",
      "issue_identifier": "MT-653",
      "issue_title": "Add the export button",
      "issue_url": "https://tracker.example.com/browse/MT-653",
      "issue_type": "Story",
      "issue_labels": ["export"],
      "state": "In Review",
      "kind": "review",
      "config_key": "review_comments",
      "pull_request": {
        "number": 931,
        "repository": "acme/web",
        "url": "https://github.com/acme/web/pull/931",
        "branch": "sortie/MT-653",
        "head_sha": "4f1c2a9e8b7d6c5b4a3f2e1d0c9b8a7f6e5d4c3b"
      },
      "created_at": "2026-02-24T20:09:30Z",
      "dispatched_at": "2026-02-24T19:52:10Z",
      "next_poll_at": "2026-02-24T20:16:05Z",
      "wait": "debounce",
      "used_attempts": 1,
      "max_attempts": 3,
      "escalated_at": null,
      "watch_ends_at": "2026-02-24T20:39:30Z",
      "last_check": {
        "observed_at": "2026-02-24T20:15:10Z",
        "actionable_comments": 3,
        "new_comments": 1
      },
      "rule_name": "default",
      "stage_position": null,
      "agent_kind": "claude-code",
      "configured_model": "claude-sonnet-4-20250514",
      "status_text": "1 new review comment on #931; next check in 35s",
      "status_text_full": "Waiting for 1 new review comment on PR #931 (3 actionable) to settle; next check in 35s. Review fix runs used: 1 of 3."
    }
  ],
  "between_stages": [
    {
      "issue_id": "pqr678",
      "issue_identifier": "MT-654",
      "issue_title": "Plan the export rollout",
      "issue_url": "https://tracker.example.com/browse/MT-654",
      "issue_type": "Task",
      "issue_labels": ["export", "stage-implement"],
      "state": "In Progress",
      "source_rule": "specify",
      "target_rule": "implement",
      "target_label": "stage-implement",
      "hopped_at": "2026-02-24T20:13:45Z",
      "dispatched_at": "2026-02-24T19:47:30Z",
      "rule_name": "implement",
      "stage_position": {
        "chain": ["specify", "implement"],
        "current": "implement",
        "hop_count": 1,
        "hop_ceiling": 10
      },
      "agent_kind": "claude-code",
      "configured_model": "claude-sonnet-4-20250514",
      "hold": null,
      "status_text": "Between stages: specify to implement",
      "status_text_full": "Moved from stage specify to stage implement, and Sortie added `stage-implement`; implement starts when a poll tick reads that label on the issue."
    }
  ],
  "held": [
    {
      "issue_id": "stu901",
      "issue_identifier": "MT-655",
      "issue_title": "Add the export button",
      "issue_url": "https://tracker.example.com/browse/MT-655",
      "issue_type": "Story",
      "issue_labels": ["export"],
      "state": "To Do",
      "hold": {
        "reason": "blocked_by",
        "held_since": "2026-02-24T20:09:44Z",
        "blockers": [
          {"issue_id": "abc124", "issue_identifier": "MT-648", "state": "In Review"}
        ],
        "slot": null,
        "resets_at": null,
        "reason_text": "Blocked by MT-648 (In Review)",
        "release_text": "Released when the blocker reaches a terminal state."
      },
      "status_text": "Blocked by MT-648 (In Review)",
      "status_text_full": "Blocked by MT-648 (In Review). Sortie dispatches the issue once the blocker reaches a terminal state."
    }
  ],
  "active": [
    {
      "issue_id": "ghi789",
      "issue_identifier": "MT-651",
      "issue_title": "Migrate the billing tables",
      "issue_url": "https://tracker.example.com/browse/MT-651",
      "status": "budget_exhausted",
      "rule_name": "everything",
      "stage_position": null,
      "agent_kind": "claude-code",
      "configured_model": "claude-sonnet-4-20250514",
      "dispatched_at": "2026-02-24T19:20:00Z",
      "status_text": "Held: session budget reached",
      "status_text_full": "Held at the session budget of 3. Sessions used: 3.",
      "pr_loop": {"ci_failure": {"result": "unrecorded", "continuations": 0}, "review_comments": {"result": "unrecorded", "continuations": 0}, "bot_review": {"result": "unrecorded", "continuations": 0}, "auto_merge": {"result": "unrecorded", "continuations": 0}}
    },
    {
      "issue_id": "jkl012",
      "issue_identifier": "MT-652",
      "issue_title": "Rotate the signing keys",
      "issue_url": "https://tracker.example.com/browse/MT-652",
      "status": "parked",
      "rule_name": "default",
      "stage_position": null,
      "agent_kind": "claude-code",
      "configured_model": "claude-sonnet-4-20250514",
      "dispatched_at": "2026-02-24T19:40:12Z",
      "status_text": "Parked: the agent needs a person",
      "status_text_full": "Parked: the agent stopped because it cannot go on without a person. Parking label: `needs-human`.",
      "pr_loop": {"ci_failure": {"result": "unrecorded", "continuations": 0}, "review_comments": {"result": "unrecorded", "continuations": 0}, "bot_review": {"result": "unrecorded", "continuations": 0}, "auto_merge": {"result": "unrecorded", "continuations": 0}}
    },
    {
      "issue_id": "def456",
      "issue_identifier": "MT-650",
      "issue_title": "Retry the export job",
      "issue_url": "https://tracker.example.com/browse/MT-650",
      "status": "retrying",
      "rule_name": "implement",
      "stage_position": {
        "chain": ["specify", "implement"],
        "current": "implement",
        "hop_count": 1,
        "hop_ceiling": 10
      },
      "agent_kind": "claude-code",
      "configured_model": "claude-sonnet-4-20250514",
      "dispatched_at": "2026-02-24T19:41:05Z",
      "status_text": "Retry 3 in 30s: no agent slot was free",
      "status_text_full": "The retry came due and was not dispatched: no agent slot was free. Retry 3 in 30s.",
      "pr_loop": {"ci_failure": {"result": "acted", "continuations": 1}, "review_comments": {"result": "quiet", "continuations": 0}, "bot_review": {"result": "quiet", "continuations": 0}, "auto_merge": {"result": "quiet", "continuations": 0}}
    },
    {
      "issue_id": "pqr678",
      "issue_identifier": "MT-654",
      "issue_title": "Plan the export rollout",
      "issue_url": "https://tracker.example.com/browse/MT-654",
      "status": "between_stages",
      "rule_name": "implement",
      "stage_position": {
        "chain": ["specify", "implement"],
        "current": "implement",
        "hop_count": 1,
        "hop_ceiling": 10
      },
      "agent_kind": "claude-code",
      "configured_model": "claude-sonnet-4-20250514",
      "dispatched_at": "2026-02-24T19:47:30Z",
      "status_text": "Between stages: specify to implement",
      "status_text_full": "Moved from stage specify to stage implement, and Sortie added `stage-implement`; implement starts when a poll tick reads that label on the issue.",
      "pr_loop": {"ci_failure": {"result": "unrecorded", "continuations": 0}, "review_comments": {"result": "unrecorded", "continuations": 0}, "bot_review": {"result": "unrecorded", "continuations": 0}, "auto_merge": {"result": "unrecorded", "continuations": 0}}
    },
    {
      "issue_id": "mno345",
      "issue_identifier": "MT-653",
      "issue_title": "Add the export button",
      "issue_url": "https://tracker.example.com/browse/MT-653",
      "status": "pending_reactions",
      "rule_name": "default",
      "stage_position": null,
      "agent_kind": "claude-code",
      "configured_model": "claude-sonnet-4-20250514",
      "reaction_kind": "ci",
      "dispatched_at": "2026-02-24T19:52:10Z",
      "status_text": "CI escalated on #931",
      "status_text_full": "Sortie escalated failing CI on PR #931; it keeps watching the pull request. CI fixes used: 2 of 2.",
      "pr_loop": {"ci_failure": {"result": "escalated", "continuations": 2}, "review_comments": {"result": "quiet", "continuations": 0}, "bot_review": {"result": "quiet", "continuations": 0}, "auto_merge": {"result": "quiet", "continuations": 0}}
    },
    {
      "issue_id": "abc123",
      "issue_identifier": "MT-649",
      "issue_title": "Fix the flaky login test",
      "issue_url": "https://tracker.example.com/browse/MT-649",
      "status": "running",
      "rule_name": "implement",
      "stage_position": {
        "chain": ["specify", "implement"],
        "current": "implement",
        "hop_count": 1,
        "hop_ceiling": 10
      },
      "agent_kind": "claude-code",
      "configured_model": "claude-sonnet-4-20250514",
      "dispatched_at": "2026-02-24T19:58:40Z",
      "status_text": "Fixing CI on #927, fix 1 of 2",
      "status_text_full": "Fixing CI on PR #927, fix 1 of 2.",
      "pr_loop": {"ci_failure": {"result": "acted", "continuations": 1}, "review_comments": {"result": "quiet", "continuations": 0}, "bot_review": {"result": "quiet", "continuations": 0}, "auto_merge": {"result": "quiet", "continuations": 0}}
    }
  ],
  "attention": [
    {
      "id": "d028c459215208a709b5ecc3ff4535b1",
      "kind": "workflow_reload_failed",
      "severity": "critical",
      "since": "2026-02-24T20:14:51Z",
      "acknowledged_at": null,
      "reason_text": "WORKFLOW.md failed to load",
      "why_text": "WORKFLOW.md failed to load (config: agent.turn_timeout_ms: must be greater than 0), so poll ticks dispatch nothing until it loads, and the configuration that last loaded stays in effect.",
      "clears_text": "Clears when WORKFLOW.md loads again, which Sortie tries each time the file is saved and on every poll tick.",
      "action": {"kind": "acknowledge"},
      "evidence": {
        "file": "WORKFLOW.md",
        "error": "config: agent.turn_timeout_ms: must be greater than 0"
      }
    },
    {
      "id": "8a6c3b55f9b8dd4b16eb33baf67df78d",
      "kind": "parked",
      "severity": "warning",
      "since": "2026-02-24T20:05:00Z",
      "acknowledged_at": null,
      "issue_id": "jkl012",
      "issue_identifier": "MT-652",
      "issue_title": "Rotate the signing keys",
      "issue_url": "https://tracker.example.com/browse/MT-652",
      "reason_text": "Parked: the agent needs a person",
      "why_text": "The agent stopped because it cannot go on without a person, so Sortie parked the issue under the label `needs-human`.",
      "clears_text": "Clears when a poll tick finds `needs-human` removed from the issue, or the issue moved out of In Progress.",
      "action": {"kind": "open_issue", "url": "https://tracker.example.com/browse/MT-652"},
      "evidence": {
        "reason": "agent_blocked",
        "label": "needs-human"
      }
    },
    {
      "id": "464f9ff8ea54c4c6cfb43af394234d57",
      "kind": "budget_held",
      "severity": "warning",
      "since": "2026-02-24T20:12:00Z",
      "acknowledged_at": "2026-02-24T20:13:05Z",
      "issue_id": "ghi789",
      "issue_identifier": "MT-651",
      "issue_title": "Migrate the billing tables",
      "issue_url": "https://tracker.example.com/browse/MT-651",
      "reason_text": "Session budget reached",
      "why_text": "The issue has used 3 sessions of its budget of 3.",
      "clears_text": "Clears on the next poll tick after `agent.max_sessions` is raised above 3 or is removed, or after the issue leaves the active tracker states.",
      "action": {"kind": "acknowledge"},
      "evidence": {
        "reason": "session_budget",
        "used_sessions": 3,
        "budget_sessions": 3
      }
    },
    {
      "id": "cd2e4d3c2c309f9ae99caa324f6e5d9d",
      "kind": "reaction_escalated",
      "severity": "warning",
      "since": "2026-02-24T20:12:40Z",
      "acknowledged_at": null,
      "issue_id": "mno345",
      "issue_identifier": "MT-653",
      "issue_title": "Add the export button",
      "issue_url": "https://tracker.example.com/browse/MT-653",
      "reason_text": "CI escalated on #931",
      "why_text": "Sortie escalated failing CI on PR #931 after 2 of 2 CI fixes; the latest check found Test failing.",
      "clears_text": "Clears when a check of PR #931 passes, a new commit lands on it, it merges or closes, `reactions.ci_failure.max_retries` rises above 2, or the issue reaches a terminal tracker state or leaves In Review.",
      "action": {"kind": "open_pull_request", "url": "https://github.com/acme/web/pull/931"},
      "evidence": {
        "reaction_kind": "ci",
        "config_key": "ci_failure",
        "pull_request": {
          "number": 931,
          "repository": "acme/web",
          "url": "https://github.com/acme/web/pull/931",
          "branch": "sortie/MT-653",
          "head_sha": "4f1c2a9e8b7d6c5b4a3f2e1d0c9b8a7f6e5d4c3b"
        },
        "escalation_trigger": "budget_exhausted",
        "used_attempts": 2,
        "max_attempts": 2,
        "last_check": {
          "observed_at": "2026-02-24T20:12:40Z",
          "head_sha": "9b7e3d1c0a2f4e6b8d0c1a3e5f7b9d2c4e6a8f0b",
          "status": "failing",
          "failing_checks": ["Test"],
          "failing": 1,
          "pending": 0,
          "passing": 4
        }
      }
    }
  ],
  "agent_totals": {
    "input_tokens": 5000,
    "output_tokens": 2400,
    "total_tokens": 7400,
    "cache_read_tokens": 1500,
    "cache_write_tokens": 300,
    "seconds_running": 1834.2,
    "unmeasured_sessions": 2,
    "running_unreported": 1,
    "running_non_reporting": 0
  },
  "rate_limits": {
    "claude-code": {
      "reported_at": "2026-02-24T20:12:00Z",
      "windows": [
        {"label": "5-hour limit", "used_percent": null, "status": "allowed", "resets_at": "2026-02-24T23:00:00Z", "reported_at": "2026-02-24T20:12:00Z", "amount": null},
        {"label": "Weekly limit", "used_percent": 82, "status": "warning", "resets_at": "2026-02-26T09:00:00Z", "reported_at": "2026-02-24T18:40:00Z", "amount": null}
      ]
    },
    "agent-client-protocol": {
      "reported_at": "2026-10-28T13:50:00Z",
      "windows": [
        {"label": "Monthly limit", "used_percent": 0.708, "status": null, "resets_at": "2026-11-01T00:00:00Z", "reported_at": "2026-10-28T13:50:00Z", "amount": {"used": 7.08, "limit": 1000, "unit": "credits"}}
      ]
    }
  },
  "config": {
    "tracker": {"kind": "jira", "project": "PROJ", "active_states": ["To Do", "In Progress"], "in_progress_state": "In Progress", "handoff_state": "In Review", "terminal_states": ["Done"]},
    "polling": {"interval_ms": 30000},
    "agent": {
      "kind": "claude-code", "usage_arrival": "incremental", "max_turns": 20, "stall_timeout_ms": 300000,
      "max_retry_backoff_ms": 300000, "max_sessions": 6, "max_tokens": null, "max_cost_usd": null,
      "max_concurrent_agents": 6, "max_concurrent_agents_by_state": {}
    },
    "worker": {"ssh_hosts": [], "max_concurrent_agents_per_host": null},
    "dispatch": {
      "rules": [
        {"name": "specify", "selected_by": "match", "match": {"issue_type": ["Story"]}, "next": "implement", "agent_kind": "claude-code", "usage_arrival": "incremental"},
        {"name": "implement", "selected_by": "stage", "stage": "stage-implement", "agent_kind": "claude-code", "usage_arrival": "incremental"},
        {"name": "deps", "selected_by": "match", "match": {"labels": ["dependencies"], "priority": {"op": "lte", "value": 2}}, "agent_kind": "codex", "usage_arrival": "incremental"},
        {"name": "everything", "selected_by": "catch_all", "agent_kind": "claude-code", "usage_arrival": "incremental"}
      ],
      "default": null,
      "chains": [["specify", "implement"]],
      "max_consecutive_hops": null,
      "hop_ceiling": 10
    },
    "reactions": {
      "ci_failure": {"max_retries": 2, "triage": false, "escalation": "label", "escalation_label": "needs-human"},
      "review_comments": {"max_continuation_turns": 3, "debounce_ms": 60000, "poll_interval_ms": 120000, "triage": false, "escalation": "label", "escalation_label": "needs-human"},
      "bot_review": {"max_continuation_turns": 5, "poll_interval_ms": 60000, "bot_usernames": ["review-bot"], "triage": false, "escalation": "none"},
      "auto_merge": {"strategy": "squash", "require_ci": true, "delete_branch": true, "poll_interval_ms": 60000, "max_retries": null, "escalation": "label", "escalation_label": "needs-human"}
    },
    "token_rates": {
      "claude-code": {"input_per_mtok": 3.0, "output_per_mtok": 15.0, "cache_read_per_mtok": 0.3, "cache_write_per_mtok": null, "complete": true, "per_kind": true},
      "codex": {"input_per_mtok": 2.5, "output_per_mtok": null, "cache_read_per_mtok": null, "cache_write_per_mtok": null, "complete": false, "per_kind": true}
    }
  },
  "health": {
    "database": {"status": "pass", "checked_at": "2026-02-24T20:15:03Z", "last_success_at": "2026-02-24T20:15:03Z", "consecutive_failures": 0, "failing_since": null},
    "preflight": {"status": "fail", "checked_at": "2026-02-24T20:15:03Z", "last_success_at": "2026-02-24T20:14:32Z", "consecutive_failures": 1, "failing_since": "2026-02-24T20:15:03Z", "error": "dispatch preflight failed: workflow file cannot be loaded: config: agent.turn_timeout_ms: must be greater than 0"},
    "workflow": {"file": "WORKFLOW.md", "status": "fail", "checked_at": "2026-02-24T20:15:03Z", "last_success_at": "2026-02-24T20:14:32Z", "consecutive_failures": 2, "failing_since": "2026-02-24T20:14:51Z", "error": "config: agent.turn_timeout_ms: must be greater than 0"},
    "poll": {"status": "fail", "checked_at": "2026-02-24T20:15:04Z", "last_success_at": "2026-02-24T20:14:33Z", "consecutive_failures": 1, "failing_since": "2026-02-24T20:15:04Z", "outcome": "preflight_failed", "next_poll_at": "2026-02-24T20:15:34Z", "summary_text": "The latest poll stopped at the dispatch preflight before reading the tracker; the last successful poll was 57s ago."},
    "scm": {"provider": "github", "status": "pass", "checked_at": "2026-02-24T20:15:04Z", "last_success_at": "2026-02-24T20:15:04Z", "consecutive_failures": 0, "failing_since": null}
  }
}
```

**Per-session fields:**

| Field               | Type              | Description                                                                                                                                |
| ------------------- | ----------------- | ------------------------------------------------------------------------------------------------------------------------------------------ |
| `issue_title`             | string or absent  | The tracker's title of the issue, as read when the session was dispatched and unchanged for the whole run. Omitted when the tracker returned none. Masked: a value registered as a secret is replaced by `[redacted]`. |
| `issue_url`               | string or absent  | The tracker's web link of the issue, as read at dispatch. Omitted when the tracker returned none. A Jira link carries no user name or password of `tracker.endpoint`. Masked like `issue_title`. |
| `issue_labels`            | array of strings  | The issue's labels as last read for the session, from the same read as `state`, lowercase and in the tracker's order. Always present; `[]` when the issue carries none. Masked like `issue_title`. |
| `issue_type`              | string or absent  | The tracker's issue type, as read at dispatch. Omitted when the tracker returned none. Masked like `issue_title`. |
| `state`                   | string            | The tracker state of the issue as Sortie last read it for the session. Masked like `issue_title`. |
| `dispatched_at`           | string            | When Sortie dispatched the first run of the issue's current pass, as an RFC 3339 timestamp in UTC. A pass is the runs that share a stage chain identifier: a dispatch that no stage hop reached starts one, and the next stage after a hop, a retry, a reaction continuation, and a restart continue it. So the first run of a pass reads the same as its `started_at`, and every later run reads an earlier time. Always present, never `null`. `active` serves the issues in a stable order, so a client that shows one line per issue keeps that order. A client that orders the rows of one list by `dispatched_at` places a `null` before every timestamp and breaks ties, which rows dispatched in one poll tick can share, by `issue_identifier`, then `issue_id`. |
| `dispatch_id`             | string            | The dispatch ID Sortie minted for the run, never reused, across restarts included. It keys the run's agent events: [`GET /api/v1/runs/{dispatch_id}/events`](#get-apiv1runsdispatch_idevents-run-events) serves them. Always present, never empty. |
| `last_event_at`           | string or `null`  | When the session's latest agent event arrived, in UTC. Liveness pings and events that are not retained count. `null` before the first event. |
| `quiet_at`                | string or `null`  | Half of `agent.stall_timeout_ms` after `last_event_at`, else after `started_at`, in UTC. `null` when stall detection is off. |
| `stall_at`                | string or `null`  | All of `agent.stall_timeout_ms` after `last_event_at`, else after `started_at`, in UTC: the instant after which the next poll tick stops the session as stalled. `null` when stall detection is off. |
| `stall_text`, `stall_text_full` | string or `null` | The short and long sentence that states the silence and the timeout, such as `No events for 2m; Sortie stops it after 5m`. The silence is rounded down, so the sentence is true when served and stays true until the next event. `null` until `generated_at` reaches `quiet_at`, and while stall detection is off. A client refetches at `quiet_at` to get it. |
| `stall_off_text`          | string or `null`  | The sentence that says stall detection is off and Sortie will not stop the session as stalled. `null` while stall detection is on, so it is non-`null` exactly when `quiet_at` is `null`. |
| `ssh_host`                | string or absent  | The SSH host the session runs on. Omitted for a local session. |
| `workflow_file`           | string or absent  | Base filename of the `WORKFLOW.md` in force at dispatch. Omitted when unset. |
| `agent_kind`              | string or absent  | The agent kind the session runs, from the configuration in force at dispatch. Omitted when unset. |
| `reaction_kind`           | string or absent  | The reaction kind of a continuation session: `ci`, `review`, `bot-review`, `merge-conflict`, `label-review`, or `label-fix`. Omitted for every other session. |
| `reaction_config_key`     | string or absent  | The key of the `reactions` section of `WORKFLOW.md` that `reaction_kind` is configured under, as a retry row's `reaction_config_key` names it. Present exactly when `reaction_kind` is. |
| `reaction_attempt`        | integer or absent | Which try of its reaction the session is: the reaction's count once its pass counted this continuation, which is the value the reaction's pending reaction row serves as `used_attempts`. Present for `ci`, `review`, `bot-review`, and `merge-conflict`; omitted for `label-review` and `label-fix`, which count nothing. |
| `pull_request`            | object or absent  | The pull request the continuation works on. Present exactly when `reaction_kind` is. Holds `number` and `repository` (`owner/repo`, masked), named as in a pending reaction row's `pull_request`. |
| `self_review_active`      | boolean           | True while the session is in its self-review phase. |
| `self_review_iteration`   | integer           | The self-review iterations completed in the current phase: `0` while the first runs and whenever `self_review_active` is `false`. |
| `tokens`                  | object            | Token counts for this session. Its members `input_tokens`, `output_tokens`, `total_tokens`, `cache_read_tokens`, and `cache_write_tokens` are each an integer or `null`. They are `null` together, exactly when `tokens_measured` is `false`, and each carries its figure otherwise. |
| `tokens.cache_read_tokens` | integer or `null` | Cumulative cache-read token count. Reflects tokens served from the LLM provider's prompt cache rather than reprocessed. `null` when `tokens_measured` is `false`; `0` when the session is measured and the agent adapter reports no cache data. |
| `tokens.cache_write_tokens` | integer or `null` | Cumulative cache-write token count. Reflects input tokens written to the LLM provider's prompt cache. `null` when `tokens_measured` is `false`; `0` when the session is measured and the agent adapter reports no cache-write count. |
| `uncached_tokens` | integer or `null` | The session's tokens without cache reads: `tokens.total_tokens` less `tokens.cache_read_tokens`, floored at zero, so `uncached_tokens` plus `tokens.cache_read_tokens` equals `tokens.total_tokens` and cache writes stay in it. A sibling of `tokens`, not a member of it. `null` exactly when `tokens_measured` is `false`. |
| `model_name`              | string or absent  | LLM model identifier the agent reported running (e.g. `"claude-sonnet-4-20250514"`). Omitted when the adapter does not report a model, and when `usage_arrival` is `none`. It can differ from `configured_model` under routing aliases and runtime fallbacks. |
| `rule_name`               | string or absent  | The dispatch rule the session runs under. Omitted when the workflow-wide fallback routed the issue. |
| `stage_lineage`           | object or `null`  | Always present. `null` unless a stage hop reached the session, otherwise the object a retry row's `stage_lineage` describes. |
| `stage_position`          | object or `null`  | Always present. `null` exactly when the session's rule is on no stage chain, so a workflow without chains serves `null` on every row. Otherwise an object with `chain` (the chain's rule names, equal to a member of `config.dispatch.chains`), `current` (the session's rule, which is the chain's first rule on its first stage), `hop_count` (the issue's consecutive automatic stage hops, `0` when it has made none), and `hop_ceiling` (the limit a worker exit compares with `hop_count`, as of the latest poll tick). `chain` and `hop_ceiling` follow the configuration as of the latest poll tick, while `rule_name` and `stage_lineage` stay as dispatched. A reload that removes the rule from every chain leaves `rule_name` and turns `stage_position` to `null`. |
| `configured_model`        | string or absent  | The `model` the session's resolved settings block carries, which is what the operator asked for, as distinct from `model_name`, which is what the runtime reported. Omitted when no model is configured. |
| `configured_effort`       | string or absent  | The `effort` the session's resolved settings block carries. Omitted when no level is configured. |
| `api_request_count` | integer or `null` | Number of `token_usage` events received during this session, and a count of actual API requests only when `api_requests_measured` is `true`. `null` exactly when that field is `false`. |
| `requests_by_model` | object or absent  | Map of model name to request count (e.g. `{"claude-sonnet-4-20250514": 3}`). Omitted when `api_requests_measured` is `false`, and when `usage_attribution` does not name a model. Enables tracking model usage when the agent switches models mid-session. |
| `skills` | array or absent | The run's skill calls so far, in completion order, each an object with `skill` (the skill name) and, for a failed call, `tool_error` set to `true`. Repeated calls are listed each time. Omitted until the run has completed a skill call, and for an agent that reports none. |
| `tool_time_percent` | number or `null`  | Cumulative tool call execution time as a percentage of session wall-clock time. Computed at response time. `null` when no tool timing data has been received. |
| `api_time_percent`  | number or `null`  | Cumulative LLM API response wait time as a percentage of session wall-clock time. Computed at response time. `null` when no API timing data has been received. |
| `tokens_measured`    | boolean           | True once at least one usage measurement has been reported in this session. The members of `tokens` and `uncached_tokens` are `null` when it is `false`. False for a session whose `usage_arrival` is `none`, whatever its runtime reports.   |
| `usage_arrival`      | string            | The usage-reporting arrival derived from the sources the session's kind attaches, frozen at dispatch: `incremental`, `turn_end`, `none`, or `""` when the kind is not registered. It follows the kind, not the launch, so a Copilot CLI session on an SSH worker reports `turn_end` although it carries no token figure.    |
| `usage_attribution`  | string            | The usage-reporting attribution derived from the sources the session's kind attaches, frozen at dispatch: `per_model`, `session_total`, `none`, or `""` when the kind is not registered. Sessions of the `agent-client-protocol` kind report `session_total`, and their figure can still name a model. |
| `tokens_pending`     | boolean           | True only when `usage_arrival` is `turn_end`, the session is measured, and the turn that figure would settle for is still in flight.      |
| `tokens_awaited`     | boolean           | True only when the session has recorded no figure and the moment its `usage_arrival` names for one has not passed. A session whose `usage_arrival` reports figures and whose `tokens_awaited` is `false` recorded nothing and will record nothing. |
| `api_requests_measured` | boolean        | True when `api_request_count` is a measurement of model API requests: `usage_arrival` is `incremental` and either a figure has arrived or no turn has begun. False for every other arrival, and for an `incremental` session past its first turn that has reported nothing. |
| `used_sessions`           | integer or `null` | The issue's completed sessions: the run-history count that `agent.max_sessions` is compared with. The running session is not counted, and a recorded CI result counts as a session. `null` when no read of the issue's run history has succeeded for this run. |
| `budget_sessions`         | integer or `null` | `agent.max_sessions` as of the latest poll tick. `null` means no session budget (`agent.max_sessions` is `0`), never `0`. The sessions left are `budget_sessions` minus `used_sessions`; there is no value when either is `null`. |
| `issue_tokens_completed`  | integer or `null` | The summed tokens without cache reads of the issue's completed sessions (`total_tokens` less `cache_read_tokens`, floored at zero for each session), the baseline the token ceiling adds this session's spend to. `0` when the issue has no completed session or its completed sessions spent exactly nothing. `null` when no read of the issue's run history has succeeded for this run, and when the sum is `0` while a completed session is unmeasured or recorded an unaccounted turn. |
| `unmeasured_sessions`     | integer or `null` | Completed sessions whose token usage was never recorded, which `issue_tokens_completed` and `used_tokens` leave out. `null` exactly when `used_sessions` is. |
| `used_tokens`             | integer or `null` | `issue_tokens_completed` plus this session's `uncached_tokens`, each counted as `0` when `null`: the figure the token ceiling compares with `agent.max_tokens`, which leaves out cache reads and advances with each usage figure of the session. `null` when that sum is `0` and `used_tokens_complete` is `false`, so unknown spend never reads as zero. |
| `used_tokens_complete`    | boolean           | True exactly when `used_tokens` is the issue's whole spend: the completed sessions were read, none is unmeasured, none recorded an unaccounted turn, this session is measured, and it has recorded no unaccounted turn. When `false`, `used_tokens` is a lower bound. It ignores `tokens_pending`, which keeps telling a consumer that the figure excludes a turn in flight. |
| `budget_tokens`           | integer or `null` | `agent.max_tokens` as of the latest poll tick. `null` means no token ceiling (`agent.max_tokens` is `0`), never `0`. The token headroom is `budget_tokens` minus `used_tokens`; there is no value when either is `null`, and it turns negative when a run overshoots, because the ceiling stops a run at the first usage figure at or past it. |
| `budget_usd`              | number or `null`  | `agent.max_cost_usd` as of the latest poll tick. `null` means no money ceiling (`agent.max_cost_usd` is `0`), never `0`. The ceiling compares it with the issue's estimated cost, the figure `issue_estimated_cost_usd` serves, which is a lower bound unless `issue_cost_status` is `estimated`. |
| `estimated_cost_usd`      | number or `null` | The live estimated USD cost of this session, from its tokens per model under the rates in force ([Section 4.4](#44-token_rates--cost-estimation)). Unrounded. Non-`null` exactly when `cost_status` is `estimated` or `partial`. |
| `cost_status`             | string            | `estimated`, which a zero rate also gives; `partial` when the estimate is a lower bound because a model or a token class the session spent has no rate beside priced spend; `unpriced` when the session is measured and no rate prices any of its spend, which is every measured session when no rate source is available; `unmeasured` when `tokens_measured` is `false`. |
| `issue_estimated_cost_usd` | number or `null` | The estimated USD cost of the whole issue: the sum of the estimates of its priced parts: the cost recorded with each completed session's run, and this session's live estimate under the rates in force. Non-`null` exactly when `issue_cost_status` is `estimated` or `partial`. |
| `issue_cost_status`       | string            | `estimated` when the estimate covers the issue's whole spend; `partial` when it is a lower bound, because a part that spent tokens is left out; `unpriced` when no priced part spent tokens and a part that did has no rate; `unmeasured` when `used_tokens` is `null`, and when no part is priced or unpriced. A session measured at zero tokens leaves nothing out, so a missing rate for its model does not make a complete, otherwise priced issue `partial`. |

A limit key (`budget_tokens`, `budget_sessions`, `budget_usd`) is always present: `null` means no limit is configured. A usage key (`used_tokens`, `issue_tokens_completed`, `used_sessions`, `unmeasured_sessions`) set to `null` means unknown. `used_tokens` is `null` only when `issue_cost_status` is `unmeasured`, and `issue_cost_status` is `estimated` only when `used_tokens_complete` is `true`. While a rate source is available, the top-level `active_estimated_cost_usd` equals the sum of `estimated_cost_usd` over the running rows whose `cost_status` is `estimated` or `partial`, and `cost_unpriced_running` counts the rows whose `cost_status` is `unpriced`.

**Retry and budget-exhausted identity fields:**

Each row of `retrying` and each record of `budget_exhausted` carries these flat keys beside `issue_id`, `issue_identifier`, and `display_identifier`. Every one is masked like the running row's `issue_title`.

| Field         | Type             | Description                                                                                                                                  |
| ------------- | ---------------- | -------------------------------------------------------------------------------------------------------------------------------------------- |
| `issue_title` | string or absent | The tracker's title of the issue. Omitted when empty, which on a retry row means the retry holds no read of its issue yet. |
| `issue_url`   | string or absent | The tracker's web link of the issue. Omitted when empty, under the same rule as `issue_title`. |
| `issue_type`  | string or absent | The tracker's issue type. Omitted when empty or unread. |
| `issue_labels` | array of strings or absent | The issue's labels, lowercase and in the tracker's order. `[]` when the issue was read and carries none; omitted while the row holds no read of its labels, so an unread issue never reads as one without labels. |
| `state`       | string or absent | The tracker state of the issue. Omitted when empty or unread. |

A retry row carries the read of the issue the retry was scheduled with. Sortie stores the read with the retry and restores it at startup, so a restart serves the title, link, display identifier, type, labels, and state it served before. A retry restored from a row stored by an earlier release holds no read; it takes one from the next poll tick whose candidates list its issue, or from its next fire. A retry restored from a row stored before the type, labels, and state were recorded holds a title and link and no labels; it takes the type, labels, and state of the same next read and keeps its title and link. A title or link already held is never replaced. A `budget_exhausted` record carries the read of the poll tick that last rebuilt the set, or of the retry the hold was found on until the next tick. A retry that a pending reaction schedules carries the reaction's issue facts, so its `state` is the handoff target after that reaction's handoff, and so does a `budget_exhausted` record made from that retry.

Each record of `budget_exhausted` also carries `stopped_in_flight`, an integer or `null`: the number of the issue's completed sessions that a budget ceiling stopped mid-run. `stopped_in_flight` and `unmeasured_sessions` are `null` exactly when no ceiling read the issue's usage. `used_tokens` leaves cache reads out, like the running row's. It is `null` when no ceiling read the issue's usage, and also when its sum is `0` while `unmeasured_sessions` or an unaccounted turn leaves spend out. The record also carries `used_usd`, `used_usd_complete`, and `budget_usd`. `used_usd` is the issue's estimated cost from the cost recorded with its completed runs, and is `null`, with `used_usd_complete`, when the money ceiling did not read the issue. `used_usd_complete` is `false` exactly when `used_usd` is a lower bound, because a run that spent tokens is unpriced or priced only in part, a run reported no token usage, or a turn's spend was never fully reported. The record's own `budget_tokens`, `budget_sessions`, and `budget_usd` serve `null` when the ceiling is not set, as do the same keys on a running row.

A `budget_exhausted` record also carries `dispatched_at`, `agent_kind`, `configured_model`, and `stage_position` as the retry fields below describe them, plus `rule_name` and `stage_lineage`. All describe the dispatch the hold blocks, which is the run that would start next. A hold set on a retry takes them from that retry. A hold found at a poll tick keeps the ones a prior record held and otherwise takes them from the stage the issue waits to enter after a stage hop, or failing that from the issue's latest agent run. A record with no such context, because the read failed or the issue has no agent run, omits `rule_name`, `agent_kind`, and `configured_model` and serves `null` for `dispatched_at`, `stage_lineage`, and `stage_position`; the next poll tick reads again.

**Retry fields:**

Each row of `retrying`, and the `retry` object of the per-issue response, carries these keys beside the identity fields above.

| Field                 | Type              | Description |
| --------------------- | ----------------- | ----------- |
| `trigger`             | string or absent  | The `sortie_retries_total` label of the schedule that created the retry: `error`, `usage_limit`, `continuation`, `timer`, `stall`, or `ci_fix`. Omitted for a review, bot review, merge conflict, or label command continuation, which `reaction_kind` names instead, and for a retry restored from a row stored by an earlier release until it next comes due. |
| `reaction_kind`       | string or absent  | The reaction kind of the retry: `ci`, `review`, `bot-review`, `merge`, `merge-conflict`, `label-review`, `label-fix`, or `merge-completion`. Omitted when the retry carries none. An `error` or `stall` retry of a run a reaction dispatched carries that run's kind, and a `timer` row keeps the kind of the retry it replaced. |
| `reaction_config_key` | string or absent  | The key of the `reactions` section of `WORKFLOW.md` that `reaction_kind` is configured under: `ci_failure`, `review_comments`, `bot_review`, `auto_merge`, `merge_conflicts`, `label_commands` (for both `label-review` and `label-fix`), or `merge_completion`. Present exactly when `reaction_kind` is. |
| `rule_name`           | string or absent  | The dispatch rule frozen with the retry, `default` for `dispatch.default`. Omitted when the workflow-wide fallback routed the run, and on a retry stored before rules were recorded. When the configuration in force no longer reaches that selection, the retry dispatches on another rule. |
| `agent_kind`          | string or absent  | The agent kind of the run the retry follows. Omitted when empty. |
| `configured_model`    | string or absent  | The `model` the resolved settings block of the run the retry follows carries. Omitted when empty. Sortie stores it with the retry and restores it at startup. |
| `dispatched_at`       | string or `null`  | When Sortie dispatched the first run of the pass the retry belongs to, in UTC, as a running row's `dispatched_at` describes it. Always present. `null` when the retry was restored from a record that predates it and run history holds no run to date it from. |
| `stage_lineage`       | object or `null`  | Always present. `null` unless a stage hop reached the run the retry follows, which includes a chain's first stage. Otherwise an object with `chain_id` (the run's stage chain), `previous` (the rule whose hop reached the run), and `previous_outcome` (`succeeded` or `no_change`), each omitted when empty. |
| `stage_position`      | object or `null`  | Always present. Where the retry's rule stands on its stage chain, under the rules of a running row's `stage_position`. |
| `paused`              | boolean           | Always present. `true` when the retry was last put back because its issue's state does not permit dispatching it, which is when `error` is `reaction retry paused by an open blocking issue` or `reaction retry paused by issue state`. |

When a retry comes due and cannot start, Sortie puts it back with the trigger `timer` and sets `error` to the reason. The error of the run before it stays in the logs and in that run's run history, and the per-issue `last_error` carries the reason too. A retry serves `error`, and `last_error`, as the same plain sentence a run row's `error` carries for a withheld handoff, an agent error or a stall, not the stored text.

| `error` after a reschedule                        | Meaning |
| ------------------------------------------------- | ------- |
| `worker still running`                            | The issue's previous run has not finished stopping. |
| `retry issue fetch failed`                        | The tracker read of the issue failed. |
| `issue without a state label`                     | The issue reads no state; the first reading waits, and a second consecutive one releases the retry. |
| `reaction retry paused by an open blocking issue` | Another issue that is not done yet blocks this issue, so its reaction retry waits until the blocker is done. |
| `reaction retry paused by issue state`            | The issue sits in a state that is neither active nor the handoff state, so its reaction retry waits. |
| `no available orchestrator slots`                 | `agent.max_concurrent_agents` is reached. |
| `no available SSH hosts`                          | Every SSH host is at its session limit. |
| `retry agent kind unavailable`                    | The adapter for the retry's agent kind failed to start. |
| `retry agent settings refused`                    | The agent settings in force for the retry fail an error-severity check. |

While an instance spend ceiling holds, a retry or reaction continuation that comes due starts no run and stays in `retrying` with its `attempt`, `error`, `trigger`, and continuation fields unchanged. Its `due_at` moves one `polling.interval_ms` ahead on each fire, and the retry fires again within one `polling.interval_ms` of the hold ending. A retry that then finds no free agent slot or SSH host keeps its `attempt` instead of raising it.

**Parked fields:**

Each record of `parked`, and the `parked` object of the per-issue response, carries these keys. The list is always present, `[]` when no issue is parked, and sorted by `issue_identifier` and then `issue_id`; `counts.parked` equals its length. An issue appears in every list that holds it, so a parked issue with a review continuation queued is in `parked` and `retrying` both.

| Field                | Type              | Description |
| -------------------- | ----------------- | ----------- |
| `issue_id`           | string            | The tracker-internal issue ID. |
| `issue_identifier`   | string            | The human-readable issue key. |
| `display_identifier` | string or absent  | The qualified form of the key. Omitted when the tracker needs none. |
| `issue_title`        | string or absent  | The tracker's title as read when the park was recorded or, for a park recorded or restored without a read, at the first poll tick on which the tracker answers for the issue. Omitted when empty. Masked like the running row's `issue_title`. |
| `issue_url`          | string or absent  | The tracker's web link under the same rule as `issue_title`. Omitted when empty. Masked. |
| `issue_type`         | string or absent  | The tracker's issue type as read for the park. Omitted when empty or unread. Masked. |
| `issue_labels`       | array of strings or absent | The issue's labels as read for the park, lowercase and in the tracker's order, without the parking label. `[]` when the issue was read and carries none; omitted while the park holds no read of its labels. Masked. |
| `state`              | string or absent  | The tracker state observed when the park was recorded; equal to `parked_state`. Omitted while unobserved. Masked. |
| `dispatched_at`      | string or `null`  | When Sortie dispatched the first run of the pass of the run the park follows, in UTC, as a running row's `dispatched_at` describes it; never the time of the park. Always present. `null` while the park records no run or that run's pass cannot be dated. |
| `reason`             | string            | `agent_blocked` or `handoff_absence`. |
| `parked_state`       | string or absent  | The tracker state observed when the park was recorded, or at the first observation after a park recorded before any read. Omitted while unobserved. Masked. |
| `label`              | string            | The parking label Sortie applied. |
| `label_applied`      | boolean           | `true` once a read showed the label on the issue. |
| `parked_at`          | string            | When the park was recorded, as an RFC 3339 timestamp in UTC. |
| `rule_name`          | string or absent  | The dispatch rule of the run the park follows, `default` for `dispatch.default`. Omitted when the workflow-wide fallback routed the run and while the park records no run. |
| `stage_position`     | object or `null`  | Always present. Where that rule stands on its stage chain, under the rules of a running row's `stage_position`, choosing the chain with the previous rule of the run's stage lineage. `null` while the park records no run. |
| `agent_kind`         | string or absent  | The agent kind of the run the park follows. Omitted while the park records no run. |
| `configured_model`   | string or absent  | The `model` the resolved settings block of that run carries. Omitted when empty. |

Sortie stores the run the park follows, with the issue's title, link, type, and labels, and restores them at startup, so a restart serves what it served before. The run is the one that stopped for a park made when a run ends or when a retry comes due, and the issue's newest agent run for a park made at a poll tick. A park stored by an earlier release takes its run from the newest agent run of the issue that started at or before the park, when the database is upgraded, and holds no type or labels until a poll tick lists its issue. If the poll tick cannot read run history, it still parks the issue, without a run: the rule, agent, and model are omitted, and the position and dispatch time are `null`, until the park is released. A park that holds no read takes one from the first poll tick on which the tracker answers for its issue, through the candidate list or, where the candidates omit the issue, one single-issue read. A park that holds a title or link and no labels takes the type and labels of the candidate list only. A title or link already held is never replaced.

**Pending reaction fields:**

Each row of `pending_reactions`, on both routes, describes one pull request reaction that Sortie keeps watching after a run hands off. The list is always present, `[]` when none, with one row per issue and kind, sorted by `issue_identifier`, then `issue_id`, then `kind`; `counts.pending_reactions` equals its length. An issue appears in every list that holds it, so an issue with a CI fix queued and review comments pending is in `retrying` and `pending_reactions` both. Pending reactions are runtime-only, except for an escalation, which Sortie stores: after a restart it rebuilds the rows without a pull request link, wait reason, or check result, and they fill in as the reactions are checked, while a standing escalation keeps its time, trigger, and the tries it used. A rebuilt row carries the issue's title and link only, with no type, labels, or state, because run history records neither.

| Field                | Type                 | Description |
| -------------------- | -------------------- | ----------- |
| `issue_id`           | string               | The tracker-internal issue ID. |
| `issue_identifier`   | string               | The human-readable issue key. |
| `display_identifier` | string or absent     | The qualified form of the key. Omitted when the tracker needs none. |
| `issue_title`        | string or absent     | The issue's title as the run that created the reaction read it, or as startup recovery restored it from run history. Omitted when empty. Masked. |
| `issue_url`          | string or absent     | The issue's tracker link under the same rule as `issue_title`. Omitted when empty. Masked. |
| `issue_type`         | string or absent     | The tracker's issue type as the run that created the reaction read it. Omitted when empty or unread, and on a rebuilt row. Masked. |
| `issue_labels`       | array of strings or absent | The issue's labels as that run read them, lowercase and in the tracker's order. `[]` when the issue was read and carries none; omitted on a rebuilt row. Masked. |
| `state`              | string or absent     | The tracker state of the issue as that run read it, or the handoff state when that run's exit moved the issue to it, because the run's read is the state the handoff replaced. Omitted when empty or unread, and on a rebuilt row. Masked. |
| `kind`               | string               | `ci`, `review`, `bot-review`, `merge`, `merge-conflict`, `label-review`, `label-fix`, or `merge-completion`. |
| `config_key`         | string               | The `reactions` key of `WORKFLOW.md` the kind is configured under, as a retry row's `reaction_config_key` names it: `ci_failure`, `review_comments`, `bot_review`, `auto_merge`, `merge_conflicts`, `label_commands` (for both `label-review` and `label-fix`), or `merge_completion`. The reaction's settings are at `config.reactions[config_key]`. |
| `pull_request`       | object               | Always present. Keys below. |
| `created_at`         | string               | When this process created the reaction, at the worker exit that seeded it or at the startup that restored it, as an RFC 3339 timestamp in UTC. A retry-slot deferral leaves it unchanged. |
| `dispatched_at`      | string or `null`     | Always present. When Sortie dispatched the first run of the pass of the run the reaction follows, in UTC, as a running row's `dispatched_at` describes it. A startup restores it from run history. `null` when run history holds no pass time for the run. |
| `next_poll_at`       | string               | When the next check is due, in UTC. The check runs on the first reconcile pass (poll tick or refresh) at or after it, so a value at or before `generated_at` means the next pass checks the reaction. A request to check the reaction now moves a later value to the time Sortie takes the request. The check that request brings forward, when it keeps the reaction pending and started before the value the request replaced, gives that value back in place of the time the check would otherwise schedule. |
| `wait`               | string or absent     | Why the reaction waits for that check (table below). Omitted until a pass first returns the reaction to the pending set, because a reaction no pass has taken has had no check and is due at once. |
| `used_attempts`      | integer or `null`    | What the kind counts (table below). `null` for `label-review` and `label-fix`, which count nothing. |
| `max_attempts`       | integer or `null`    | The ceiling the kind's pass applies, as of the latest poll tick. `null` when the pass applies none. |
| `escalated_at`       | string or `null`     | Always present. When the standing escalation of the reaction began, in UTC; `null` while none stands. Every kind that escalates stays pending while its escalation stands and serves the time the escalation began, which a recheck does not move. The escalation survives a restart. What ends it depends on the kind (see Escalation recurrence), and a person moving the issue out of `tracker.handoff_state`, or releasing its park so Sortie runs the issue again, ends it for every kind. The label the escalation applies is `config.reactions[config_key].escalation_label`. |
| `escalation_trigger` | string or absent     | Present exactly while `escalated_at` is non-null. Why the standing escalation began: `budget_exhausted` (the kind used up its tries), `triage` (the kind's triage command asked for a person), `refused` (the forge or the tracker answered a write with an authentication or payload error, on `merge` and `merge-completion`), or `missing_merge_commit` (the forge reports the pull request merged but gives no merge commit after the grace period, on `merge-completion`). |
| `watch_ends_at`      | string or `null`     | Always present. When the reaction stops watching unless its watch restarts first, in UTC. `null` for `merge-completion`, `label-review`, and `label-fix`, which have no watch window, when the window is `0`, while an escalation stands, because no window ages out a standing escalation, and for a `review` reaction whose latest check is awaiting approval, because the watch is held past its window. Each kind measures the window from its own start of the watch: `ci` from the head commit its latest check recorded, or from the creation of the reaction while it has recorded none; `review`, `bot-review`, and `merge-conflict` from the creation or the last retry-slot deferral; `merge` from the creation. The window is the kind's `watch_window_ms`. |
| `last_check`         | object or `null`     | Always present. What the latest completed check observed (keys below). `null` before the first check completes, and always `null` for `merge`, `merge-completion`, `label-review`, and `label-fix`. A result replaces the previous one, leaves with its entry, and for `ci` is cleared when the head changes. |
| `rule_name`          | string or absent     | The dispatch rule of the run the reaction follows, `default` for `dispatch.default`. Omitted when the workflow-wide fallback routed the run. A startup restores it from run history. |
| `stage_position`     | object or `null`     | Always present. Where that rule stands on its stage chain, under the rules of a running row's `stage_position`, choosing the chain with the previous rule of the run's stage lineage. |
| `agent_kind`         | string or absent     | The agent kind of the run the reaction follows. Omitted when empty. |
| `configured_model`   | string or absent     | The `model` the resolved settings block of that run carries. Omitted when empty. |

`pull_request` keys:

| Field        | Type             | Description |
| ------------ | ---------------- | ----------- |
| `number`     | integer          | The pull request number. Always present. |
| `repository` | string           | `owner/repo` as the pull request record the run left in its workspace holds them. Masked. |
| `url`        | string or absent | The pull request's web page as the provider reported it on the latest read of the pull request. Omitted until a read reports one. Masked. Sortie never builds the link from the configured API endpoint, because a forge can serve its web pages from another host. |
| `branch`     | string or absent | The head branch that record holds for the run that created the reaction. Omitted for `label-review` and `merge-completion`, which keep none. Masked. |
| `head_sha`   | string or absent | The head commit that record holds for that run. Omitted when none was recorded and for `label-review`, `label-fix`, and `merge-completion`. Masked. A later push does not change it. |

`last_check` keys, a flat object whose members follow the kind:

| Field                 | Kinds                      | Description |
| --------------------- | -------------------------- | ----------- |
| `observed_at`         | every kind that records    | When the check completed, in UTC. |
| `head_sha`            | `ci`, `merge-conflict`     | The head commit the check read. Omitted when the provider reported none. Masked. |
| `status`              | `ci`                       | The verdict the reaction acted on, `passing`, `pending`, or `failing`, as the provider reported it. An empty check set is `pending`, not `passing`. |
| `failing_checks`      | `ci`                       | The names of the check runs the verdict counts as failing, in the provider's order, uncapped. `[]` when none. Masked. The log excerpt and each check's details link are never served. |
| `failing`, `pending`, `passing` | `ci`             | The check runs by outcome. Each run counts once, in the class the verdict reads it as, so the counts and `status` cannot disagree. |
| `actionable_comments` | `review`, `bot-review`     | The comments the check found actionable: current, and for `review` outside `bot_usernames`. A comment a run was already given counts until the forge reports it outdated. |
| `new_comments`        | `review`, `bot-review`     | Those of `actionable_comments` the reaction acts on next: no run was given them, the reaction has not already reported them, and the running turn does not carry them. `0` whenever `wait` is `poll_interval`, since the check then found nothing to act on. |
| `handled`             | `bot-review`               | `true` while the newest triage answer Sortie recorded for the comment set the check found is `handled`, so no session is started for it. Omitted otherwise, never `false`. A set that differs from the answered one carries no `handled`. The answer survives a restart and returns with the first completed check. |
| `awaiting_approval`   | `review`                   | `true` when the check found no comment to answer and the forge reports a required approval still missing. Omitted otherwise, never `false`, and never present on `bot-review`. The watch is held past `watch_window_ms` while it is `true`. |
| `mergeable`           | `merge-conflict`           | `false` when the provider reports merge conflicts, `true` when it reports none, and `null` while it has not computed mergeability. A branch-protection block is not a conflict. |

`wait` values, by the pass that last returned the reaction to the pending set:

| `wait`          | Meaning |
| --------------- | ------- |
| `poll_interval` | The pass completed its check and found nothing to act on yet, or acted and keeps watching. The kind checks again at its poll cadence. |
| `debounce`      | For `review`: the check found new actionable comments within `debounce_ms` of the newest. `next_poll_at` ends the window unless a request to check the reaction now moved it, and the reaction dispatches then unless a newer comment arrives. The check that request brings forward applies the window again and gives `next_poll_at` back the window end from before the request; if a newer comment extended the window, the check at that time applies it once more. |
| `backoff`       | The pass could not complete its check: a provider, tracker, or store call failed, or the provider answered without an identifier the check needs. |
| `retry_slot`    | The pass found work to dispatch while another retry holds the issue's retry slot. For `merge-completion`, while a run or retry claims the issue. For `label-review` and `label-fix`, a confirmed command that waits for a run of another kind to end. |
| `triage`        | The reaction waits on its triage command: a run for the current subject is in flight, was just started, or waits for a free run slot. |

`used_attempts` and `max_attempts` by kind:

| `kind`                       | `used_attempts` counts | `max_attempts` | At the ceiling |
| ---------------------------- | ---------------------- | -------------- | -------------- |
| `ci`                         | CI-fix continuations dispatched since a passing verdict, or a head change attributed to someone else, last reset the count. | `reactions.ci_failure.max_retries` | The next failing verdict escalates. The count exceeds the ceiling only after a reload lowers `max_retries` below the fixes already dispatched. |
| `review`                     | Review-fix continuations dispatched. | `max_continuation_turns` | The next set holding a new comment escalates. |
| `bot-review`                 | Bot-review-fix continuations dispatched. | `max_continuation_turns` | The next new comment on the code escalates. |
| `merge-conflict`             | Rebase continuations dispatched since the conflict episode began. | `max_retries` | The next new conflicting head escalates. |
| `merge`                      | Merge calls made. | `max_retries`; `null` at `0` | The next check escalates. |
| `merge-completion`           | Tracker transitions that failed. | `max_retries` | The next failed transition escalates. |
| `label-review`, `label-fix`  | `null` | `null` | None. |

**Between-stages fields:**

Each row of `between_stages`, and the `between_stages` object of the per-issue response, describes an issue that a stage hop advanced and whose next stage has no run yet. The list is always present, `[]` when no issue waits, and sorted by `issue_identifier` and then `issue_id`; `counts.between_stages` equals its length. An issue joins the list when the exit of a run makes the hop, and leaves it when a run of the issue is dispatched. It is never also in `running` or `retrying`, and it can also be in `budget_exhausted`. Sortie stores the hop record and restores the list at startup.

| Field                | Type             | Description |
| -------------------- | ---------------- | ----------- |
| `issue_id`           | string           | The tracker-internal issue ID. |
| `issue_identifier`   | string           | The human-readable issue key. |
| `display_identifier` | string or absent | The qualified form of the key. Omitted when the tracker needs none. |
| `issue_title`        | string or absent | The issue's title as the run that made the hop read it. Omitted when empty. Masked like the running row's `issue_title`. |
| `issue_url`          | string or absent | The issue's tracker link under the same rule as `issue_title`. Omitted when empty. Masked. |
| `issue_type`         | string or absent | The tracker's issue type as the run that made the hop read it. Omitted when empty or unread. Masked. |
| `issue_labels`       | array of strings or absent | The issue's labels as the hop leaves them: the run's dispatch read without the stage labels the hop removes, followed by the stage label it adds. `[]` when the issue was read and carries none; omitted while the record holds no read. A stage label that Sortie failed to remove is on the issue and not in this list until a run is dispatched. Masked. |
| `state`              | string or absent | The tracker state of the issue as the run that made the hop read it. A hop writes no state, so it stays the issue's state. Omitted when empty or unread. Masked. |
| `source_rule`        | string           | The stage the issue left: the rule whose run made the hop. |
| `target_rule`        | string           | The stage the issue reached: the rule the hop advanced it to. |
| `target_label`       | string           | The stage label the hop added. Configuration, so unmasked. |
| `hopped_at`          | string or `null` | When the hop was made, as an RFC 3339 timestamp in UTC. Always present. `null` when the record holds no time. |
| `dispatched_at`      | string or `null` | When Sortie dispatched the first run of the pass of the run that made the hop, in UTC, which the next stage's run continues, as a running row's `dispatched_at` describes it. Always present. `null` when the record holds no pass time. |
| `rule_name`          | string           | The stage reached; equal to `target_rule`. |
| `stage_position`     | object or `null` | Always present. Where `target_rule` stands on its stage chain, under the rules of a running row's `stage_position`, choosing the chain with `source_rule` as the previous rule. It is the position the next stage's running row serves. |
| `agent_kind`         | string or absent | The agent kind a dispatch of `target_rule` resolves to under the configuration as of the latest poll tick. Omitted when that configuration names no such rule with a stage label. |
| `configured_model`   | string or absent | The `model` that dispatch's resolved settings block carries, under the same rule as `agent_kind`. Omitted when empty. |
| `hold`               | object or `null` | Always present. The hold the latest dispatch pass gave the issue, in the shape of the `hold` object of a held row, or `null` while the pass recorded no hold for it. Every other member of the row is the same with or without it. |

A hop record stores the type, labels, and state with the hop, and Sortie restores them at startup. A hop recorded before the upgrade that introduced them serves none of them until the next stage dispatches.

**Held issues (`held`):**

Each element of `held` describes one eligible issue in an active state that the latest dispatch pass held back instead of dispatching, with why and what releases it. The list is always present, `[]` when the pass held nothing, and sorted by `issue_identifier` and then `issue_id`; `counts.held` equals its length. An issue appears in one place: it is not listed here while it is running, scheduled for a retry, held by a budget, parked, waiting on a pending reaction, or between stages, and an issue between stages serves its hold on its `between_stages` row. A held issue is not an active issue, so it is not in `active` and adds nothing to `counts.attention` or `attention`. Sortie keeps the list in memory only: it is empty until a poll tick has run the dispatch loop and after a restart, and a tick that returns before the loop, because its dispatch preflight or its candidate fetch failed, leaves it as it was.

| Field                | Type             | Description |
| -------------------- | ---------------- | ----------- |
| `issue_id`, `issue_identifier` | string | The tracker-internal issue ID and the human-readable key. Always present. |
| `display_identifier`, `issue_title`, `issue_url`, `issue_type`, `state` | string or absent | The qualified form of the key, the title, the tracker link, the issue type, and the tracker state, as the pass read the issue. Each is omitted when empty and masked like the running row's `issue_title`. |
| `issue_labels`       | array of strings | The issue's labels as the pass read them, `[]` when it carries none. Always present. Masked. |
| `hold`               | object           | Why the issue is held, as the table below describes. Always present. |
| `status_text`, `status_text_full` | string | The status sentence pair. `status_text` equals `hold.reason_text`. Always present. |

**Hold fields:**

The `hold` object of a held row and of a between-stages row has these members.

| Field          | Type              | Description |
| -------------- | ----------------- | ----------- |
| `reason`       | string            | Why the pass held the issue, one of the values below, and the API schema enumerates them. A client MUST tolerate a `reason` its copy of the schema does not list, one a newer version serves, and show `reason_text` and `release_text`. |
| `held_since`   | string            | When a dispatch pass first held the issue, as an RFC 3339 timestamp in UTC. Never `null`. It does not change while later passes keep the issue held, whatever their reasons, nor across a candidate fetch that fails. It is new when a pass that runs the loop does not hold the issue and a later pass does, and after a restart. |
| `blockers`     | array or `null`   | Non-null exactly for `blocked_by`: every blocker whose state is empty or not terminal, in the order the tracker returned them. Each has `issue_id`, `issue_identifier`, `display_identifier` (omitted when the tracker needs no qualified form), and `state` (omitted when the tracker returned none, masked). |
| `slot`         | object or `null` | Non-null exactly for `agent_slots_full`. `scope` is `global` or `state`; `state` is the issue's tracker state as the pass read it, present exactly for `state` and masked; `used` is the sessions running against the limit; `limit` is the limit. |
| `resets_at`    | string or `null`  | Non-null exactly for the spend reasons and `usage_limit`: when the holding window or usage limit resets, as an RFC 3339 timestamp in UTC. |
| `agent_kind`   | string or absent  | Present exactly for `usage_limit`: the agent kind whose new sessions the hold stops. |
| `model_only`   | boolean or absent | Present and `true` for a `usage_limit` hold that stops the agent kind on one configured model alone. Absent when the hold stops every model of the kind. |
| `model`        | string or absent  | The configured model a `model_only` hold stops. Absent for a hold on every model and for the runtime's default model. |
| `reason_text`  | string            | A short sentence without a final period. |
| `release_text` | string            | One sentence, ending with a period, that says what releases the hold. |

| `reason`                                                      | The pass held the issue because |
| ------------------------------------------------------------- | ------------------------------- |
| `blocked_by`                                                  | A blocker is in a non-terminal state or has none. |
| `blockers_unresolved`                                         | Reading the blockers failed, or an earlier failure halted blocker reads for the pass. |
| `blockers_not_read`                                           | The pass reads the blockers of a limited number of issues in turn and did not reach this one. |
| `blockers_incomplete`                                         | The tracker declared the blocker list incomplete. |
| `agent_slots_full`                                            | The global agent slot limit or the limit of the issue's state is taken. |
| `ssh_hosts_full`                                              | Every SSH host runs as many sessions as its per-host limit allows. |
| `daily_spend_ceiling`, `weekly_spend_ceiling`, `monthly_spend_ceiling` | A spend window holds dispatch. |
| `stage_hop_unconfirmed`                                       | The read that should confirm the issue's stage move failed or did not show the hop's label. |
| `usage_limit`                                                 | The agent provider refused a session because the account's usage limit is reached, and Sortie starts no session of the issue's agent kind (on that model alone, for a hold with `model_only`) until it resets. |
| `park_comments_unread`                                        | A person released the issue's park, and the read of the comments the next run receives failed. |

While a spend window holds, an issue whose blockers are clear shows the spend reason, and so does every issue a full capacity limit stops, because a freed slot would not dispatch it while the window holds. A usage-limit hold on an issue's agent kind does the same with the reason `usage_limit`, and a spend window ranks above it. Without a spend window, a full global agent slot limit or SSH capacity stops the pass before it checks blockers, so an issue that also has an open blocker shows the capacity reason until a pass reaches it with capacity free; the pass reads no blocker for the issues it classifies this way.

**Active issues (`active`):**

Each element of `active` describes one active issue, so a client that shows one line per issue merges, chooses, and orders nothing itself. The list is always present, never `null`, and `[]` when no row list holds an issue. It holds one entry per distinct `issue_id` across `running`, `retrying`, `budget_exhausted`, `parked`, `pending_reactions`, and `between_stages`; a held issue is not an entry. The row lists, `counts`, and the per-issue response stay as they are, and `counts` gains no member: the length of `active` is the number of active issues.

The deciding row of an entry is the issue's row in the list that `status` names. In `pending_reactions` it is the issue's first served row whose `escalated_at` is non-null, and its first served row when none is, the row that decides the per-issue sentences. Every value below is the deciding row's, as that row serves it, so a masked string stays masked.

| Field                | Type             | Description |
| -------------------- | ---------------- | ----------- |
| `issue_id`, `issue_identifier` | string | The tracker-internal issue ID and the human-readable key. Always present. |
| `display_identifier`, `issue_title`, `issue_url` | string or absent | The qualified form of the key, the title, and the tracker link. Omitted when the deciding row holds none. Masked as that row serves them. |
| `status`             | string           | The per-issue status by the precedence of `GET /api/v1/{identifier}`: `running`, else `retrying`, else `budget_exhausted`, else `parked`, else `pending_reactions`, else `between_stages`. Always present. Never `history`. It is also the name of the list the entry comes from, so no second key repeats it. |
| `rule_name`, `agent_kind`, `configured_model` | string or absent | The dispatch rule, the agent kind, and the configured model. Omitted when the deciding row holds none. |
| `stage_position`     | object or `null` | Where the rule stands on its stage chain. Always present, and `null` when the deciding row serves `null`. |
| `dispatched_at`      | string or `null` | When Sortie dispatched the first run of the pass the deciding row belongs to, in UTC. Always present, and `null` when the deciding row serves `null`. |
| `reaction_kind`      | string or absent | The `kind` of the deciding row. On every entry whose `status` is `pending_reactions`, and omitted on every other entry. |
| `status_text`, `status_text_full` | string | The deciding row's status sentence pair, equal to the pair `GET /api/v1/{identifier}` serves for the issue at its top level. Always present. |
| `pr_loop`            | object or `null` | What each configured pull request reaction recorded in the pass of the deciding row, as the `pr_loop` description after this table gives. Always present, and `null` when the read of reaction activity failed. |

Only `pending_reactions` holds more than one row for an issue, so a client finds the deciding row by `issue_id` in the list that `status` names, and in `pending_reactions` by `issue_id` and by `kind` equal to `reaction_kind`. The `reactions` key a kind is configured under follows from the kind, so the entry serves none, and a running or retrying entry carries no reaction key because its row already serves the continuation's pair.

The entries are in the order Sortie keeps for the issues: the place of each issue, then `issue_identifier`, then `issue_id`, each in byte order. An issue's place is the earliest `dispatched_at` its rows carried when it joined the row lists, and an issue none of whose rows is dated is placed before every dated issue. The place holds through a move from one list to another, through a new pass begun while the issue is listed (a park released and dispatched on one poll tick, a budget hold cleared by a raised ceiling, an issue a person returns to an active state while a reaction waits), and when a `dispatched_at` turns from `null` into a time. An issue that no list held and that returns takes a new place. Sortie keeps the places in memory and serves none, so a client that filters the list keeps the served order, and after a restart each issue takes its place from its restored rows.

**Pull request reaction history (`pr_loop`):**

`pr_loop` says what each configured pull request reaction recorded in one pass of an issue, so a client draws a reaction that is no longer pending, or never was. It is on each entry of `active`, on `GET /api/v1/{identifier}`, and on each run of `GET /api/v1/runs`. A pass is the runs that share a `chain_id`. On an `active` entry it is the pass of the deciding row. On the per-issue response it is the pass of the row that decides `status` for a live issue and the pass of the newest run for a `history` issue. On a run of `GET /api/v1/runs` it is the pass the run's own `chain_id` and `dispatched_at` name.

The value is an object with one member for each key of `config.reactions`, named as in `WORKFLOW.md`, and `{}` when `config.reactions` is empty. The `label_commands` member covers the review and fix label commands together. Each member holds two keys.

| Field           | Type    | Description |
| --------------- | ------- | ----------- |
| `result`        | string  | `escalated` when the reaction began an escalation in the pass, one that has since ended included. Otherwise `acted` when Sortie applied a triage verdict, merged the pull request, moved the issue after the merge, or scheduled a run for the reaction in the pass. Otherwise `quiet` when Sortie watched the pull request for the reaction and had nothing to act on. Otherwise `unrecorded`. |
| `continuations` | integer | The runs scheduled for the reaction in the pass, `0` when none. It counts runs scheduled, the quantity the pull request panel calls the fixes used, so a run that a budget hold kept from starting still counts. |

`unrecorded` means Sortie has no record, which differs from `quiet`, where Sortie watched and found nothing to act on. Every member of a pass that began before Sortie recorded reaction activity reads `unrecorded`, and so does the member of a reaction Sortie began watching after a restart until that reaction acts. A pass without a `chain_id` or a `dispatched_at` reads `unrecorded` throughout.

`pr_loop` is `null` when the server holds no run history or no workflow shape. The runs in the groups of `GET /api/v1/runs/issues` and in `history.runs` serve `null` always. When the read of reaction activity fails, `GET /api/v1/state` answers `200` with a `null` `pr_loop` on every entry, and `GET /api/v1/{identifier}` and `GET /api/v1/runs` answer `503` with `history_unavailable`.

**Attention (`attention`):**

Each element of `attention` describes one thing that needs a person. Sortie derives the list on each request from the rows above, the `health` block, and the `spend_ceiling` object, and stores it nowhere, so an item exists exactly while its cause holds and disappears on the first request after the cause clears. The one thing Sortie stores is the acknowledgement of an item, which `POST /api/v1/attention/{id}/acknowledge` records. The list is always present, `[]` when nothing needs a person, and `counts.attention` counts the items whose `acknowledged_at` is `null`, so it is `0` when nothing needs a person and an acknowledged item stays listed without being counted. A client that shows the list counts, orders, and words nothing itself.

| Field                | Type             | Description |
| -------------------- | ---------------- | ----------- |
| `id`                 | string           | A stable identifier of 32 lowercase hexadecimal characters. It stays the same while the cause holds, across requests and, for `parked` and `reaction_escalated`, across a restart, and a recurrence of the cause gets a new one, except a hold that returns under the same reason and ceiling. Sortie keys stored acknowledgements by it, so the encoding below never changes. |
| `kind`               | string           | What the item is about (table below). |
| `severity`           | string           | `critical` marks a cause under which poll ticks dispatch nothing for any issue. `warning` marks a cause about one issue. The severity belongs to the kind and never grows with age. |
| `since`              | string or `null` | When the cause began, in UTC. `null` only for a park whose stored time cannot be read. |
| `acknowledged_at`    | string or `null` | When a person acknowledged the item, in UTC, or `null` while nobody has. Always present. An acknowledged item stays in the list and leaves `counts.attention`. |
| `issue_id`, `issue_identifier` | string or absent | The tracker-internal issue ID and the human-readable key, copied from the source row. Absent on an item about the process. |
| `display_identifier`, `issue_title`, `issue_url` | string or absent | Copied from the source row as that row serves them. Omitted when empty and absent on an item about the process. Masked. |
| `reason_text`        | string           | A short phrase that says what needs a person, without a final period. |
| `why_text`           | string           | One sentence that says why it needs a person. |
| `clears_text`        | string           | One sentence that says what clears the item. |
| `action`             | object           | The one action a client should offer (keys below). |
| `evidence`           | object           | The facts behind the item (table below). |

`kind` values, with the cause that raises each, its `since`, its `severity`, and its `action.kind`:

| `kind`                   | Cause | `since` | `severity` | `action.kind` |
| ------------------------ | ----- | ------- | ---------- | ------------- |
| `parked`                 | A row of `parked` | `parked_at`, truncated to whole seconds | `warning` | `open_issue`; `acknowledge` when the row has no `issue_url` |
| `budget_held`            | A row of `budget_exhausted` | `exhausted_at` | `warning` | `acknowledge` |
| `reaction_escalated`     | A row of `pending_reactions` whose `escalated_at` is non-null, whatever its kind | `escalated_at` | `warning` | For `merge-completion`: `open_issue`, else `acknowledge`. For every other kind: `open_pull_request`, else `open_issue`, else `acknowledge` |
| `workflow_reload_failed` | `health.workflow.consecutive_failures` above `0` | `health.workflow.failing_since` | `critical` | `acknowledge` |
| `preflight_failed`       | `health.preflight.consecutive_failures` above `0`, unless the latest failed run failed because the workflow file did not load | `health.preflight.failing_since` | `critical` | `acknowledge` |
| `spend_ceiling_reached`  | A window of `spend_ceiling` whose `holds_dispatch` is true, one item per holding window | The first evaluation that found the window at its ceiling; after a restart, the first evaluation after it | `critical` | `acknowledge` |

`action` keys: `kind` is `open_issue`, `open_pull_request`, or `acknowledge`. `url` is present exactly for `open_issue` (the issue's link) and `open_pull_request` (the pull request's link). `acknowledge` is the action of an item with nothing to open. Every item can be acknowledged, whatever its `action`.

A preflight that fails because the workflow file did not load yields no `preflight_failed` item, because the `workflow_reload_failed` item already names the cause. A `preflight_failed` item's `since` is when the preflight began failing, which can be earlier than the item's first appearance when the failing streak began on the workflow load.

`evidence` members. A member is present exactly on the kinds named and absent on every other:

| Member | Kind | Value |
| ------ | ---- | ----- |
| `reason` | `parked`, `budget_held`, `spend_ceiling_reached` | The row's `reason`: `agent_blocked` or `handoff_absence`; `money_budget`, `token_budget`, or `session_budget`. For a spend ceiling hold, `daily_spend_ceiling`, `weekly_spend_ceiling`, or `monthly_spend_ceiling`. |
| `label` | `parked` | The row's `label`. |
| `used_sessions`, `budget_sessions` | `budget_held` with `session_budget` | The row's values. |
| `used_tokens`, `used_tokens_complete`, `budget_tokens` | `budget_held` with `token_budget` | The row's `used_tokens` and `budget_tokens`. `used_tokens_complete` is `false` exactly when `used_tokens` is a lower bound. |
| `used_usd`, `used_usd_complete`, `budget_usd` | `budget_held` with `money_budget`, `spend_ceiling_reached` | The row's values; for a spend ceiling hold, the window's `used_usd`, `used_usd_complete`, and `budget_usd`. `used_usd_complete` is `false` exactly when `used_usd` is a lower bound. |
| `resets_at` | `spend_ceiling_reached` | The end of the window, in UTC, when the hold ends with no action. |
| `reaction_kind`, `pull_request`, `escalation_trigger` | `reaction_escalated` | The row's `kind`, `pull_request`, and `escalation_trigger`, in the shapes the row serves. |
| `config_key` | `reaction_escalated`, `spend_ceiling_reached` | The row's `config_key`; for a spend ceiling hold, the setting of the window, `agent.max_daily_cost_usd`, `agent.max_weekly_cost_usd`, or `agent.max_monthly_cost_usd`. |
| `used_attempts`, `max_attempts` | `reaction_escalated` | The row's values, each omitted when `null`. |
| `last_check` | `reaction_escalated` | The row's `last_check`, omitted when `null`. |
| `file` | `workflow_reload_failed` | `health.workflow.file`. |
| `error` | `workflow_reload_failed`, `preflight_failed` | The check's `error`, exactly as `health` serves it. |

`id` is the lowercase hexadecimal form of the first 16 bytes of the SHA-256 hash of the JSON array `[kind, part, ...]`. The parts are the following, with times in UTC. No masked value enters an `id`.

| `kind`                   | Parts |
| ------------------------ | ----- |
| `parked`                 | The issue ID; the park time truncated to whole seconds in RFC 3339, or `""` when the stored time cannot be read. |
| `budget_held`            | The issue ID; the `reason`; the ceiling the reason names as a decimal number, `budget_usd` for `money_budget`, `budget_tokens` for `token_budget`, and `budget_sessions` for `session_budget`. A `money_budget` ceiling is written in its shortest decimal form, with no exponent and no trailing zeros, so `5`, `5.0`, and `5.00` all give `5`. |
| `reaction_escalated`     | The issue ID; the row's `kind`; `escalated_at` in RFC 3339 with nanoseconds. |
| `spend_ceiling_reached`  | The window (`daily`, `weekly`, or `monthly`); the window's start in UTC in RFC 3339; the window's ceiling in its shortest decimal form, as a `money_budget` ceiling is written. |
| `workflow_reload_failed` | `failing_since` in RFC 3339 with nanoseconds. |
| `preflight_failed`       | `failing_since` in RFC 3339 with nanoseconds. |

The park time enters at whole seconds because Sortie stores a park at that precision, so a park keeps its `id` and `since` across a restart. A hold keeps its `id` across a restart too, but its `since` becomes the first poll tick after the restart, because Sortie keeps the moment a hold began only in memory. A spend ceiling hold keeps its `id` and its acknowledgement across a restart in the same way, because its window and ceiling come from the clock and the setting, and its `since` is the first evaluation after the restart. A reaction escalation keeps its `id` and `since` across a restart, because the restored escalation carries its stored time. A workflow reload or preflight failure cannot span a restart, because startup exits when the first load or the startup preflight fails. A recurrence changes a part, so an acknowledgement never hides a new cause: a new park has a new time, a hold reached again after a raise or under an edited ceiling has a new ceiling, a spend ceiling hold in a new UTC day, week, or month has a new window start, a different budget reason is a new reason, an escalation that ended and began again has a new time, and a failure after a pass has a new `failing_since`. A hold that clears and returns under the same reason and ceiling, for example after its issue left the active states and came back or after the ceiling was raised and restored, gets its old `id` back. No acknowledgement carries over to it, because Sortie deletes an acknowledgement once a poll tick that reads the candidate issues no longer finds its item.

An acknowledgement lasts as long as its item's cause holds. The first poll tick that reads the candidate issues and no longer finds the item deletes the acknowledgement, so the same reason coming back after it cleared shows as a new, unacknowledged item. A tick that cannot read the candidate issues, because its preflight or its tracker fetch failed, deletes none. Sortie stores each acknowledgement and loads it at startup. A park and a reaction escalation keep theirs across a restart, and a hold keeps its own once the first poll tick that reads the candidate issues rebuilds the hold. A failed reload or preflight cannot span a restart, so its acknowledgement goes at the first such tick. A hold whose issue left the active states and came back returns unacknowledged with its earlier `since`. An acknowledgement changes no item's place in the order.

Items are ordered by `severity` (`critical` before `warning`), then by `since` (`null` first, then ascending), then by `id` ascending. The order is total, so the same state serves the same list.

`reason_text`, `why_text`, and `clears_text` are English sentences that Sortie takes from the same table of wording as the status sentences, one template per park reason, per budget reason, per spend ceiling window, per check, and per kind of escalating reaction. Each follows the rules of the status sentences: it states only a fact Sortie records, it is masked like `issue_title`, and it states no cost, clock time, date, or stage, except the figures of a `money_budget` hold and of a spend ceiling hold. A part that cannot be rendered reads `Status not available`, and the item stays, so the list and its count never drop a cause. The `reason_text` of each kind:

| `kind` | `reason_text` |
| ------ | ------------- |
| `parked` | `Parked: the agent needs a person` for `agent_blocked`; `Parked: no evidence of work` for `handoff_absence`. |
| `budget_held` | `Money budget reached` for `money_budget`; `Token budget reached` for `token_budget`; `Session budget reached` for `session_budget`. |
| `spend_ceiling_reached` | `Daily spend ceiling reached` for the daily window; `Weekly spend ceiling reached` for the weekly window; `Monthly spend ceiling reached` for the monthly window. |
| `reaction_escalated` | `CI escalated on #<number>`, `Review comments escalated on #<number>`, `Bot review escalated on #<number>`, `Merge conflict escalated on #<number>`, `Auto-merge escalated on #<number>`, or `Merge completion escalated on #<number>`, by reaction kind. |
| `workflow_reload_failed` | `<file> failed to load`. |
| `preflight_failed` | `Dispatch preflight failed`. |

What clears each cause is the source's own rule, and `clears_text` words it. A park is released when a person removes its label after Sortie has seen it there, or moves the issue to another tracker state. A hold leaves the set when a poll tick finds the issue no longer at a ceiling or among the candidates, for example after the ceiling is raised or removed. A reaction escalation ends on the facts listed under Escalation recurrence in the reaction settings. A spend ceiling hold ends when its UTC day, week, or month ends, or when its setting is raised above the window's spend or removed. A reload failure ends with the next load that succeeds, and a preflight failure with the next preflight that passes.

The `attention` array of the example above holds the failed reload first, because it is `critical`, and then the park, the hold, and the CI escalation in the order of their `since`. It holds no `preflight_failed` item, because the preflight error is the load error. The hold of MT-651 is acknowledged, so it carries its time and `counts.attention` is `3`.

**Status sentence fields:**

Every row of `running`, `retrying`, `budget_exhausted`, `parked`, `pending_reactions`, `between_stages`, and `held`, on both routes that serve them, carries these two keys. So does every entry of `active`, with the pair of its deciding row, the per-issue response itself, and every `issue_outcome`. Both are always present and never empty.

| Field              | Type   | Description |
| ------------------ | ------ | ----------- |
| `status_text`      | string | The short sentence, for a row or a lane. It ends without a period. |
| `status_text_full` | string | The long sentence, for the issue page. It ends with a period. |

A sentence says in plain English what the issue is doing, what it waits on, or how it ended. The rules are the same for every row kind:

- Sentences are English. Sortie renders them on each request from the snapshot, the `config` the response serves, and run history, and stores none. A relative time such as "in 40s" is measured from `generated_at`.
- A sentence states only a fact Sortie records for the row, in `config`, or in run history. A figure Sortie does not have reads as not measured, never as zero, and a figure that is only a lower bound reads as at least.
- A sentence states no cost and no clock time or date, and uses stage wording only when the workflow has stage chains.
- A sentence does not repeat what the response carrying it already shows: `status_text_full` never names the issue key. Neither do a next outcome or an attention item's `why_text` and `clears_text`; only an operator action's `outcome_text` names the issue.
- A sentence is masked like `issue_title`, so a value registered as a secret after it was stored is masked too.
- A hold's `reason_text` and `release_text` are the short and the clears form of the table entry whose short and long forms are the row's `status_text` and `status_text_full`. Neither the long form nor `release_text` names the held issue's key; a blocker's key appears because it names another issue.
- A sentence that fails to render is served as `Status not available`, and Sortie logs the failure.
- Every structured field a sentence is built from stays served. A client that needs data reads the fields and does not parse the text.

**Aggregate totals:**

| Field               | Type    | Description                                                                                             |
| ------------------- | ------- | ------------------------------------------------------------------------------------------------------- |
| `input_tokens`      | integer | Total input tokens consumed across all sessions (current and completed).                                |
| `output_tokens`     | integer | Total output tokens consumed.                                                                           |
| `total_tokens`      | integer | Total tokens consumed.                                                                                  |
| `cache_read_tokens` | integer | Total cache-read tokens across all sessions. Follows the same cumulative-delta accounting as other token counters. |
| `cache_write_tokens` | integer | Total cache-write tokens across all sessions. Follows the same cumulative-delta accounting as other token counters. |
| `seconds_running`   | number  | Aggregate wall-clock runtime — completed-session time plus elapsed time from currently running sessions. |
| `unmeasured_sessions` | integer | Ended sessions whose token usage was never recorded, which the token totals above leave out. Survives a restart. |
| `running_unreported` | integer | Running sessions whose `usage_arrival` reports usage, that have not reported a figure yet, and whose `tokens_awaited` is `true`, so a figure may still arrive. |
| `running_non_reporting` | integer | Running sessions that report no usage: those whose `usage_arrival` is `none`, and those whose `usage_arrival` reports figures but whose moment for one passed with nothing recorded. |

A session whose `usage_arrival` is `none` contributes nothing to the token totals, even when its runtime reports a figure.

`cost_unpriced_running` is a top-level field, present, zero included, whenever a rate source is available (any `token_rates` entry, an operator price table, a downloaded table, or the embedded table), and omitted otherwise, so its presence alone tells a client whether Sortie can price at all. It counts the running, measured sessions that `active_estimated_cost_usd` leaves out because no rate prices any of their spend. `active_estimated_cost_usd` sums the live estimates of the running, measured sessions that price, in full or in part, and is omitted when none of them prices.

`spend_ceiling` is a top-level object, present while `agent.max_daily_cost_usd`, `agent.max_weekly_cost_usd`, or `agent.max_monthly_cost_usd` is above `0` and omitted otherwise, so a workflow with none of them serves the response it served before. Its members `daily`, `weekly`, and `monthly` are always present: an object while the matching setting is above `0`, and `null` otherwise. Each object carries `budget_usd` (the setting, above `0`), `used_usd` (the window's estimated cost: the cost recorded with each run that ended in the window plus the live estimate of the sessions running now), `used_usd_complete` (`false` exactly when `used_usd` is a lower bound because a run or session could not be fully priced or its usage was not fully reported), `holds_dispatch` (`true` while the window has reached its ceiling and Sortie starts no new agent session), and `resets_at` (the end of the window, in UTC). The route serves the latest evaluation and computes nothing, so `holds_dispatch` equals the decision the poll tick and the retry timer last applied, and the figures are at most one `polling.interval_ms` old. The windows are the UTC calendar day, the UTC week from Monday, and the UTC calendar month. Under `sortie --once`, a candidate or retry that a window holds ends that issue's work as not succeeded, and the retry is not checked again. The dashboard's "today" starts at midnight in the browser's time zone, so for a viewer off UTC its today total and the daily window cover different spans.

**Workflow shape (`config`):**

`config` describes the workflow in effect, so a client draws the rules, stages, and reactions the workflow has and assumes none it lacks. It reports the configuration promoted at the last successful load, kept when a reload fails. A setting whose change needs a restart reports what the process applies: every reaction except `ci_failure` is the one built at startup, and `tracker.kind` and `tracker.project` are the ones the tracker adapter was built with. So `config` names no reaction a reload added, keeps naming one a reload removed, and keeps the tracker kind and project of startup, until the restart that applies the edit. A member reports the WORKFLOW.md key of its name; the derived members are `selected_by`, `agent_kind`, `usage_arrival`, `chains`, `hop_ceiling`, and `complete`. A member whose setting imposes no ceiling is `null`, present and never `0`. An optional string is omitted when unset, and a list member is `[]` when empty.

| Field | Type | Description |
| ----- | ---- | ----------- |
| `tracker.kind` | string | The tracker adapter kind the process started with. A reload that edits `tracker.kind` does not change it. |
| `tracker.project` | string or absent | The `tracker.project` the tracker adapter was built with, after environment resolution and unmasked: a Jira project key, a GitHub or Gitea `owner/repo`, a GitLab namespace path or numeric project ID, or a Linear team key. Omitted when unset. A reload that edits `tracker.project` does not change it. |
| `tracker.active_states` | array of strings | `tracker.active_states` as written. |
| `tracker.in_progress_state` | string or absent | The state Sortie writes at dispatch. Omitted when unset. |
| `tracker.handoff_state` | string or absent | The state Sortie writes after a successful run. Omitted when unset. |
| `tracker.no_change_state` | string or absent | The state a successful run ends at when its agent declared that nothing needed changing. Omitted when unset. |
| `tracker.terminal_states` | array of strings | `tracker.terminal_states` as written. |
| `polling.interval_ms` | integer | The poll interval. |
| `agent.kind` | string | The workflow-wide agent kind, which serves every issue that no rule and no `dispatch.default` selects. |
| `agent.usage_arrival` | string | Whether sessions on the workflow-wide agent report token usage: `incremental`, `turn_end`, `none`, or `""` for a kind that is not registered. Derived the way a session derives it at its start; with `worker.ssh_hosts` set, a kind whose token figure cannot arrive over SSH reports `none`. |
| `agent.max_turns` | integer | The most turns a session runs: `agent.max_turns`, or `1` when it is below `1`. |
| `agent.stall_timeout_ms` | integer or `null` | `agent.stall_timeout_ms` when positive. `null` when stall detection is off. |
| `agent.max_retry_backoff_ms` | integer | `agent.max_retry_backoff_ms`. |
| `agent.max_sessions` | integer or `null` | `agent.max_sessions` when positive. `null` means no session budget. A running row's `budget_sessions` is the value as of the latest poll tick, so for at most one poll interval after a reload this member can lead it. Show a row's own `budget_sessions` on that row and this member where the workflow is described. |
| `agent.max_tokens` | integer or `null` | `agent.max_tokens` when positive. `null` means no token ceiling. The same display rule applies against a running row's `budget_tokens`. |
| `agent.max_cost_usd` | number or `null` | `agent.max_cost_usd` when positive. `null` means no money ceiling. The same display rule applies against a running row's `budget_usd`. |
| `agent.max_concurrent_agents` | integer | `agent.max_concurrent_agents`. |
| `agent.max_concurrent_agents_by_state` | object | The per-state caps, keyed by lowercase state with positive values. `{}` when none. |
| `worker.ssh_hosts` | array of strings | The configured SSH hosts, each once, in the order written. `[]` in local mode. The launch mode is remote exactly when this list is non-empty, because with hosts configured every dispatch takes one. |
| `worker.max_concurrent_agents_per_host` | integer or `null` | The per-host cap when hosts are configured and the cap is positive. `null` otherwise. |
| `dispatch.rules` | array of objects | The dispatch rules in configuration order, which is the order match rules are tried; a rule selected by its stage label is chosen ahead of them. `[]` when there are none. |
| `dispatch.rules[].name` | string or absent | The rule's name. Omitted when it has none. |
| `dispatch.rules[].selected_by` | string | `stage` for a rule that carries a stage label, `catch_all` for a rule with no `match` block, and `match` otherwise. |
| `dispatch.rules[].stage` | string or absent | The stage label as written. Present only when `selected_by` is `stage`. |
| `dispatch.rules[].match` | object or absent | Present exactly when `selected_by` is `match`. Holds each non-empty key of the rule's `match` block: `labels`, `issue_type`, `title`, `identifier`, and `assignee` as lists in the order written, and `priority` as `{"op": "<op>", "value": n}` for `eq`, `lt`, `lte`, `gt`, and `gte`, or `{"op": "in", "values": [...]}` for `in`. |
| `dispatch.rules[].next` | string or absent | The rule that follows this one. Omitted when the rule has none. |
| `dispatch.rules[].agent_kind` | string | The agent kind dispatch resolves for the rule. |
| `dispatch.rules[].usage_arrival` | string | The rule's usage-reporting arrival, derived like `agent.usage_arrival` from the rule's kind. The rule's settings block does not change it. |
| `dispatch.default` | object or `null` | `{"agent_kind", "usage_arrival"}` of the `dispatch.default` block when it names an agent or a template. `null` otherwise. Running, retry, and run-history rows name this selection `default`; a row with no `rule_name` ran on the workflow-wide fallback, which `agent` describes. A catch-all rule takes every issue that no stage rule and no earlier match rule takes, so while one is configured neither `dispatch.default` nor the workflow-wide fallback selects an issue. `config` still serves both, and a client draws no dispatch path through them. |
| `dispatch.chains` | array of arrays of strings | Each stage chain the `next` links form, as rule names in `next` order, in the order of the rules that start them. A chain starts at a rule that carries `next` and that no `next` names, and ends at a rule without `next`. Chains that share a tail list it in each chain. A rule that carries no `next` and that no `next` names belongs to no chain. `[]` when no rule carries `next`. |
| `dispatch.max_consecutive_hops` | integer or `null` | The value `dispatch.max_consecutive_hops` writes. `null` when the key is absent. |
| `dispatch.hop_ceiling` | integer | The limit a worker exit compares with an issue's hop count: the written `dispatch.max_consecutive_hops`, or the larger of `10` and the hops of the longest chain. With no chain no hop occurs, and the value is the ceiling a reload that adds `next` would apply. |
| `reactions` | object | A member per reaction kind the process applies, keyed as in `reactions` of WORKFLOW.md. `{}` when it applies none. Key order carries no meaning. |
| `reactions.ci_failure` | object or absent | Present exactly while the CI check runs, which needs both a CI provider and a source-control adapter. Holds `max_retries`, `triage`, and the escalation members. |
| `reactions.review_comments` | object or absent | Present when the reaction was configured at startup. Holds `max_continuation_turns`, `debounce_ms`, `poll_interval_ms`, `triage`, and the escalation members. |
| `reactions.bot_review` | object or absent | Present when the reaction was configured at startup. Holds `max_continuation_turns`, `poll_interval_ms`, `bot_usernames` (`[]` when empty), `triage`, and the escalation members. |
| `reactions.merge_conflicts` | object or absent | Present when the reaction was configured at startup. Holds `max_retries`, `poll_interval_ms`, `triage`, and the escalation members. `max_retries` of `0` escalates on the first detection. |
| `reactions.auto_merge` | object or absent | Present when the reaction was configured at startup. Holds `strategy`, `require_ci`, `delete_branch`, `poll_interval_ms`, `max_retries`, and the escalation members. `max_retries` is `null` at `0`, because `0` turns count-based escalation off. |
| `reactions.merge_completion` | object or absent | Present when the reaction was configured at startup. Holds `target_state`, `max_retries`, `poll_interval_ms`, and the escalation members. |
| `reactions.label_commands` | object or absent | Present when the review command or the fix command was configured at startup. Holds `review_label` while the review command is configured, `fix_label` while the fix command is, and the one `poll_interval_ms` both share. It has no escalation members. |
| `escalation` | string | A member of every reaction that escalates: the mode the reaction applies, `label`, `comment`, or `none`. For `ci_failure`, a configuration in force that carries no `ci_failure` block while the CI check still runs from startup serves `none` and a `max_retries` of `0`. |
| `triage` | boolean | A member of `ci_failure`, `review_comments`, `bot_review`, and `merge_conflicts`: `true` exactly when the kind runs a triage command, as configured at startup. |
| `escalation_label` | string or absent | Present exactly when `escalation` is `label`. The label the reaction applies: the configured value, or `needs-human` where the block writes none. |
| `token_rates` | object or `null` | The `token_rates` in force, which reload with the workflow. `null` when it holds no entry, which does not mean nothing is priced: the price tables can still price runs. Otherwise a member per entry, keyed by the agent kind or the model name as written. |
| `token_rates.<key>.input_per_mtok`, `output_per_mtok`, `cache_read_per_mtok`, `cache_write_per_mtok` | number or `null` | The rate as configured, `null` when not set. An unset cache rate of a kind entry prices that class at the input rate, an unset rate of a model entry comes from the table that lists the model, and `config` serves the configured `null`, never the substituted rate. |
| `token_rates.<key>.complete` | boolean | `true` exactly when the entry prices on its own: both `input_per_mtok` and `output_per_mtok` set for a kind entry, all four rates set for a model entry. |
| `token_rates.<key>.per_kind` | boolean | `true` exactly when the key names a registered agent kind, so the entry prices every model of that kind alike; `false` for a model entry. |

`config` holds no credential. It is an allowlist of typed settings: it carries no tracker endpoint, key, or query filter, no agent command, hook, workspace or database path, or notification destination, no reaction extra key or triage script, and no settings block. Its configuration strings are served unmasked, as the parked record's `label` is, because they are configuration.

A row's `rule_name`, `stage_lineage.previous`, and `reaction_config_key` are those in force when its run was dispatched or the row was stored, while `config` follows the rule above, so each can name a member `config` does not hold. A reload that renames or removes a rule leaves running and retry rows naming it, as does a restart on the edited file for the retry rows it restores, and a row whose `rule_name` is `default` keeps it while `dispatch.default` serves `null`. Draw such a row from its own keys.

**Health (`health`):**

`health` reports Sortie's own checks, so a client draws a failed reload, a failed preflight, or a failing tracker without reading the log. It holds a member per check: `database`, `preflight`, `workflow`, `poll`, and `scm`. Every check carries the members of the first table; `workflow`, `poll`, and `scm` add members of their own. A time Sortie does not have is `null`, never a zero time.

| Field | Type | Description |
| ----- | ---- | ----------- |
| `status` | string | `fail` exactly when `consecutive_failures` is above `0`, otherwise `pass`. A check that has not run reads `pass`, so `status` says whether the latest run failed, not that health was proven. |
| `checked_at` | string or `null` | When the latest run ended, in UTC. `null` until the check has run. |
| `last_success_at` | string or `null` | When a run last passed, in UTC. `null` until one has. |
| `consecutive_failures` | integer | The runs that failed since `last_success_at`, the run that reports it included. `0` while the check passes. |
| `failing_since` | string or `null` | When the first of those runs ended, in UTC. `null` while the check passes. |
| `error` | string or absent | The latest failure's text, masked: a value registered as a secret is replaced by `[redacted]`. The workflow file's full path is shortened to its base name. Present exactly when `status` is `fail`, except on `poll`. |

| Check | Run that feeds it | Members of its own |
| ----- | ----------------- | ------------------ |
| `database` | A ping of the database, made once per poll tick after the configuration is applied and before reconciliation. `error` is the ping's error text. | None. |
| `preflight` | The dispatch preflight each poll tick runs. `error` is every failure the preflight found, joined by `; ` after `dispatch preflight failed:`. Warnings neither fail the check nor appear in it. The preflight Sortie runs at startup is not recorded. | None. |
| `workflow` | Every load of the workflow file: the file watcher's, and the reload each poll tick forces before its preflight. `checked_at` is the latest load, whether it succeeded or failed. `last_success_at` is the latest load that put its configuration in effect, the configuration `config` serves. `failing_since` is the first failed load since then, the one an edit caused. `error` is the latest load's error text. | `file`: the base name of the workflow file in effect, for example `WORKFLOW.md`. |
| `poll` | One run per poll tick, whether the timer or `POST /api/v1/refresh` ran it. `checked_at` is the last poll time. `error` is the text of the latest failed candidate read since the last `succeeded` poll, and is absent while no read has failed since then, so it can be present beside a `preflight_failed` outcome and absent beside a failed status. | `outcome`: `succeeded` when the tracker's candidate read returned without an error, `tracker_failed` when it returned one, `preflight_failed` when the tick ended before the read because preflight failed. Omitted before the first poll. `succeeded` passes and the others fail. `next_poll_at`: when the poll timer fires next, in UTC, or `null` once shutdown stops the timer. A refresh does not move it. A value at or before `generated_at` means a poll is due or running. `summary_text`: one plain English sentence on the poll check that ends with a period, always present. It states no clock time, measures a relative time from `generated_at`, and carries no error text. |
| `scm` | The reads Sortie makes of the pull request provider the reactions use: pull request reviews and review comments, the review decision, CI status, mergeability, label events, and the auto-merge token-scope check. The check reflects the most recent provider read across every pull request, and `checked_at` dates that read, so a read that fails for one pull request can be followed by one that passes for another. A read fails the check with the error's text when it returns an error, a not-found answer included. A read that shutdown canceled is not recorded. Writes (merging, deleting a branch, removing a label) are never recorded, and neither are the calls made at startup. The check runs only when a reaction reads the provider. | `provider`: the provider kind every reaction shares, for example `github`. The whole member is `null` when no reaction uses a source-control provider. |

A reload that fails to load changes nothing else. The previous configuration stays in effect and `config` keeps serving it. The failed load also fails that tick's `preflight`, whose `error` carries the load error after `workflow file cannot be loaded:`, and ends that tick's `poll` as `preflight_failed`, so the tick dispatches nothing, though it still applies the configuration, reconciles, and sweeps. `/readyz` keeps reporting `workflow: pass` beside a failing `health.workflow`.

`version` is the Sortie version, the value `/readyz` serves. `started_at` is when the process started, in UTC, taken before the startup work, so it and the `uptime_seconds` of `/readyz` name one instant. The keys of `health`, `version`, and `started_at` are additive; no existing key changes name, type, or meaning.

#### `GET /api/v1/{identifier}` — Per-Issue Detail

Returns issue-specific runtime and debug details for a single issue, and the issue's run history. Returns `404` with `{"error":{"code":"issue_not_found","message":"..."}}` when neither the current orchestrator state nor run history knows the identifier.

```json
{
  "issue_identifier": "MT-649",
  "issue_id": "abc123",
  "status": "running",
  "status_text": "Fixing CI on #927, fix 1 of 2",
  "status_text_full": "Fixing CI on PR #927, fix 1 of 2.",
  "next_outcomes": [],
  "issue": {
    "title": "Fix the flaky login test",
    "url": "https://tracker.example.com/browse/MT-649",
    "issue_type": "Bug",
    "labels": ["bug", "backend"],
    "state": "In Progress"
  },
  "workspace": {
    "path": "/tmp/sortie_workspaces/MT-649"
  },
  "attempts": {
    "restart_count": 0,
    "current_retry_attempt": 0
  },
  "running": {
    "issue_title": "Fix the flaky login test",
    "issue_url": "https://tracker.example.com/browse/MT-649",
    "issue_labels": ["bug", "backend"],
    "issue_type": "Bug",
    "session_id": "thread-1-turn-1",
    "turn_count": 7,
    "state": "In Progress",
    "started_at": "2026-02-24T20:10:12Z",
    "dispatched_at": "2026-02-24T19:58:40Z",
    "last_event": "turn_completed",
    "last_message": "Working on tests",
    "dispatch_id": "RLQFSIOFERWGDW2HZY45W55VXK",
    "last_event_at": "2026-02-24T20:14:59Z",
    "quiet_at": "2026-02-24T20:17:29Z",
    "stall_at": "2026-02-24T20:19:59Z",
    "stall_text": null,
    "stall_text_full": null,
    "stall_off_text": null,
    "workspace_path": "/tmp/sortie_workspaces/MT-649",
    "tokens": {
      "input_tokens": 1200,
      "output_tokens": 800,
      "total_tokens": 2000,
      "cache_read_tokens": 400,
      "cache_write_tokens": 100
    },
    "uncached_tokens": 1600,
    "model_name": "claude-sonnet-4-20250514",
    "rule_name": "implement",
    "stage_lineage": {
      "chain_id": "3c8v5n2w7r1t4y6u9i0o8p2asd",
      "previous": "specify",
      "previous_outcome": "succeeded"
    },
    "stage_position": {
      "chain": ["specify", "implement"],
      "current": "implement",
      "hop_count": 1,
      "hop_ceiling": 10
    },
    "configured_model": "claude-sonnet-4-20250514",
    "configured_effort": "high",
    "reaction_kind": "ci",
    "reaction_config_key": "ci_failure",
    "reaction_attempt": 1,
    "pull_request": {
      "number": 927,
      "repository": "acme/web"
    },
    "api_request_count": 3,
    "requests_by_model": {"claude-sonnet-4-20250514": 3},
    "skills": [{"skill": "ce-plan"}, {"skill": "ce-work", "tool_error": true}],
    "tool_time_percent": 12.3,
    "api_time_percent": 45.6,
    "tokens_measured": true,
    "usage_arrival": "incremental",
    "usage_attribution": "per_model",
    "tokens_pending": false,
    "api_requests_measured": true,
    "used_sessions": 2,
    "budget_sessions": 5,
    "issue_tokens_completed": 53400,
    "unmeasured_sessions": 0,
    "used_tokens": 55000,
    "used_tokens_complete": true,
    "budget_tokens": 500000,
    "budget_usd": null,
    "estimated_cost_usd": 0.014595,
    "cost_status": "estimated",
    "issue_estimated_cost_usd": 0.235095,
    "issue_cost_status": "estimated",
    "status_text": "Fixing CI on #927, fix 1 of 2",
    "status_text_full": "Fixing CI on PR #927, fix 1 of 2."
  },
  "retry": null,
  "budget_exhausted": null,
  "parked": null,
  "pending_reactions": [],
  "between_stages": null,
  "pr_loop": {"ci_failure": {"result": "acted", "continuations": 1}, "review_comments": {"result": "quiet", "continuations": 0}, "bot_review": {"result": "quiet", "continuations": 0}, "auto_merge": {"result": "quiet", "continuations": 0}},
  "history": {
    "runs": [
      {
        "run_id": 41,
        "issue_id": "abc123",
        "issue_identifier": "MT-649",
        "attempt": 1,
        "status": "succeeded",
        "error": null,
        "declared_outcome": null,
        "agent_text": null,
        "rule_name": "specify",
        "agent_kind": "claude-code",
        "started_at": "2026-02-24T19:48:20Z",
        "completed_at": "2026-02-24T20:05:31Z",
        "dispatched_at": "2026-02-24T19:48:20Z",
        "turns_completed": 6,
        "tokens": {
          "input_tokens": null,
          "output_tokens": null,
          "total_tokens": null,
          "cache_read_tokens": null,
          "cache_write_tokens": null
        },
        "uncached_tokens": null,
        "tokens_measured": false,
        "unaccounted_turns": 0,
        "estimated_cost_usd": null,
        "cost_status": "unmeasured",
        "model_usage": [],
        "chain_id": "3c8v5n2w7r1t4y6u9i0o8p2asd",
        "stage_target": "implement",
        "stage_result": "advanced",
        "self_review": null,
        "origin_recorded": true,
        "trigger": "",
        "reaction_kind": "",
        "reaction_config_key": "",
        "session_id": "thread-1-turn-1",
        "ssh_host": "",
        "stage_chain": ["specify", "implement"],
        "pr_loop": null,
        "issue_outcome": null
      }
    ],
    "next_cursor": null,
    "totals": {
      "run_count": 1,
      "unmeasured_runs": 1,
      "tokens": {
        "input_tokens": null,
        "output_tokens": null,
        "total_tokens": null,
        "cache_read_tokens": null,
        "cache_write_tokens": null
      },
      "uncached_tokens": null,
      "tokens_complete": false,
      "estimated_cost_usd": null,
      "cost_status": "unmeasured"
    },
    "hops": [
      {
        "run_id": 41,
        "chain_id": "3c8v5n2w7r1t4y6u9i0o8p2asd",
        "source_rule": "specify",
        "target_rule": "implement",
        "result": "advanced",
        "completed_at": "2026-02-24T20:05:31Z"
      }
    ],
    "last_stage": {
      "chain": ["specify", "implement"],
      "rule": "specify"
    },
    "issue_outcome": null
  },
  "recent_events": [
    {
      "seq": 41,
      "at": "2026-02-24T20:14:31Z",
      "event": "tool_result",
      "message": "",
      "tool_name": "Bash",
      "tool_kind": "shell",
      "tool_argument": "go test ./...",
      "duration_ms": 2400,
      "tool_error": false,
      "text": "Ran go test ./... in 2.4s.",
      "tool_line": "go test ./..."
    },
    {
      "seq": 42,
      "at": "2026-02-24T20:14:52Z",
      "event": "turn_completed",
      "message": "",
      "usage": {
        "input_tokens": 1200,
        "output_tokens": 800,
        "total_tokens": 2000,
        "cache_read_tokens": 400,
        "cache_write_tokens": 100
      },
      "text": "Turn completed."
    },
    {
      "seq": 43,
      "at": "2026-02-24T20:14:59Z",
      "event": "notification",
      "message": "Working on tests",
      "text": "Working on tests"
    }
  ],
  "last_error": null,
  "tracked": {}
}
```

The `running` object uses the same per-session field schema as `GET /api/v1/state` (see the per-session fields table above). `status` is `"running"` when the issue has a running session, `"retrying"` when it does not but has a pending retry, `"budget_exhausted"` when it has neither but is held out of dispatch by a per-issue budget ceiling, `"parked"` when it is none of those but is parked, `"pending_reactions"` when it is none of those but a pending pull request reaction holds it, `"between_stages"` when it is none of those but a stage hop advanced it and its next stage has no run yet, and `"history"` when it is none of those but run history holds runs of it. The fields for whichever cases do not apply are `null`. `budget_exhausted` carries the same record shape as the `budget_exhausted` array on `GET /api/v1/state`, `parked` the record shape of the `parked` array, and `between_stages` the row shape of the `between_stages` array, `null` when the issue is not between stages, with its `hold` like the array's row. The route does not read the `held` list, so an issue that only the list holds is answered from the other row lists and run history alone. `pending_reactions` is always present: the issue's rows in the record shape and order of the `pending_reactions` array on `GET /api/v1/state`, `[]` when the issue has none, whatever the status. A parked-only, pending-reaction-only, between-stages-only, or history-only response carries a `null` `workspace`, zero `attempts`, and a `null` `last_error`, as a budget-exhausted one does. `last_error` is the retry's `error`, so after a reschedule it is the reschedule reason.

`status_text` and `status_text_full` at the top level are the status sentence pair of the row that decides `status`. For `pending_reactions` they are the pair of the first served row whose `escalated_at` is non-null, and of the first served row when none is. For `history` they word how the issue ended from its outcome and the newest run's hop decision, so a handoff that followed a stage hop that was not made names the stage it did not reach, and an issue with no outcome says that Sortie recorded no ending. The pair is present on every `200` response.

`pr_loop` is present on every `200` response and describes the pass of the row that decides `status` for a live issue, and the pass of the newest run for a `history` issue, in the shape described with the `active` entries. It is `null` when the server holds no run history or no workflow shape.

`issue` is present on every `200` response, built from the entry that decides `status`. Its members take the names of the prompt template's `issue` variable, and each is omitted when that entry holds none, so the object is `{}` when it holds none of them. All are masked like the running row's `issue_title`. `labels` is `[]` when the entry holds labels and the issue carries none.

| `status`                                         | `issue` members |
| ------------------------------------------------ | --------------- |
| `running`, `retrying`, `budget_exhausted`        | `title`, `url`, `issue_type`, `labels`, and `state`, from the row's `issue_title`, `issue_url`, `issue_type`, `issue_labels`, and `state` |
| `parked`                                         | `title`, `url`, `issue_type`, and `labels` of the record, and `state` from its `parked_state` |
| `pending_reactions`                              | `title`, `url`, `issue_type`, `labels`, and `state` of the issue's first row |
| `between_stages`                                 | `title`, `url`, `issue_type`, `labels`, and `state` of the issue's row |
| `history`                                        | `title` and `url` of the issue's newest run, each omitted when empty |

`recent_events` is present on every `200` response: the newest 50 agent events of one run, oldest first, in the event shape of [`GET /api/v1/runs/{dispatch_id}/events`](#get-apiv1runsdispatch_idevents-run-events). The run is the issue's running run, the one `running.dispatch_id` names; otherwise the issue's newest run in run history that has a dispatch ID, so the recorded CI result, which has none, is passed over. It is `[]` when the issue has no such run or the run's events are not retained. The route serves 50 events so a client can refetch it often; the run-events route serves all of them.

`next_outcomes` is present on every `200` response: a list of condition and sentence pairs that say what Sortie does next for the issue, with the exit of the work session first, then the retry or the wait for a stage label, then the pending reactions. It is `[]` when no pair applies, as in the example above, where a running CI fix has no pending reaction. A `history` response serves `[]`.

**Next outcome fields:**

| Field       | Type   | Description |
| ----------- | ------ | ----------- |
| `condition` | string | What leads to the outcome. It starts with a capital letter and ends without a period. |
| `text`      | string | What Sortie then does, in one or two sentences that each end with a period. |

The rules are the same for every pair:

- Sentences are English. Sortie renders them on each request from the snapshot, the `config` that `GET /api/v1/state` serves, and run history, and stores none. Each is masked like `issue_title`, and a sentence states no cost and no clock time or date.
- The list follows the workflow in effect. Its pairs come from the exit of the issue's work session, its retry, its wait for a stage label, and each pending reaction of the issue. A running reaction continuation or label command run contributes no exit pair, because the issue's pending reactions say what follows it. A held or finished issue lists only the pairs of its pending reactions.
- A pair never names a reaction that `config` lacks or a stage the workflow's rules do not hold, and uses stage wording only when the workflow has stage chains.
- A pair calls the pull request it watches "the pull request", and names it by number only when the issue's pending reactions watch more than one pull request.
- The condition of a watched reaction is worded as what a check finds, and carries the time left in which a check can still lead to the outcome, measured from `generated_at`. A reaction with no time left yields no pair.
- An outcome that starts a session says so when the money budget, the token budget, or the session budget would stop it, in that precedence, and names the hold. It names the money budget by its setting, `agent.max_cost_usd`, never by an amount. The exit pairs of a running session list the stop at the money budget before the stop at the token budget. An outcome that a configured triage command replaces says that the triage command runs, and an outcome that a spent fix count replaces says that Sortie escalates.
- The list is not exhaustive. It leaves out what the configuration does not decide: a stop the agent requests, a person's tracker or pull request edit other than a label command, a failed tracker, forge, or store call, a withheld handoff and the park it leads to, a wait for a free agent slot, and a hold by an instance spend ceiling.
- A pair that fails to render is left out of the list, and Sortie logs the failure.

For a work session of `MT-654` on the `specify` stage, whose rule names `implement` as the next stage with the stage label `stage-implement`, when `tracker.handoff_state` is `Human Review`, the hop count is below the ceiling, and no budget applies, the list is:

```json
{
  "next_outcomes": [
    {"condition": "If the run succeeds", "text": "Sortie adds `stage-implement`, and a poll tick then starts stage implement in a new session."},
    {"condition": "If the run fails", "text": "Sortie retries with exponential backoff."}
  ]
}
```

`history` is present on every `200` response: `null` when the issue has no run, and otherwise the issue's newest runs, so a running issue with earlier runs serves its live detail and its history together. `runs` holds the newest page of the issue's runs, newest first, each in the run shape of [`GET /api/v1/runs`](#get-apiv1runs-run-history). `next_cursor` is `null` on the last page and otherwise continues on `GET /api/v1/runs?issue_id=<issue_id>`, which selects exactly the runs the page was cut from. `totals` covers all of the issue's runs: `run_count` counts every run, a recorded CI result included, and `unmeasured_runs` counts the runs that never reported usage. Each member of `totals.tokens` is the sum over the measured runs, except `total_tokens`, which sums every run, and is `null` when its sum is `0` and `tokens_complete` is `false`. `totals.uncached_tokens` is that `total_tokens` less the sum of `cache_read_tokens`, floored at zero, and is `null` under the same rule. `tokens_complete` is `true` exactly when no run is unmeasured and none recorded an unaccounted turn, and when it is `false` the sums are lower bounds. `totals.estimated_cost_usd` and `totals.cost_status` follow the rule of `issue_estimated_cost_usd` and `issue_cost_status` over the issue's runs alone. `hops` holds the stage hop decisions of the runs in `runs`, newest first: `source_rule` is the rule of the deciding run, `target_rule` the rule its exit reached, and `result` is `advanced` or `partial` when the hop was made and `failed` or `ceiling` when it was not. A hop decision older than the page arrives with its run on the pages `next_cursor` continues. `last_stage` is the stage chain the issue's last stage run stood on, or `null`, so a client places a finished issue on its chain, which no live row serves for it. It is always present and holds `chain`, the rule names of the chain in `next` order, and `rule`, the rule of the deciding run. The deciding run is the newest run in `runs` that no pull request reaction started and whose `rule_name` is set, so a reaction run and the recorded CI result, which carries no `rule_name`, never decide. `chain` is the chain `stage_position.chain` serves for that rule and the run's `stage_previous`, so it equals the deciding run's `stage_chain`, and it follows the configuration in effect, so a reload that edits the chains changes it for runs recorded before the reload. `last_stage` is `null` when no run in `runs` decides, when the deciding rule is on no chain, and when the server holds no workflow shape. `issue_outcome` is how the issue's latest work ended, or `null` when Sortie observed no ending since it last picked the issue up. It is the object every run in `runs` carries, as the `issue_outcome` row of the run fields describes.

A request by an identifier that no live entry holds is answered through run history. When the newest run recorded under it belongs to an issue that is live under a newer identifier (a tracker move keeps the issue ID), the response is that issue's live detail with `issue_identifier` set to the identifier the issue carries now. The `history` of a response always covers the issue by its ID, so runs recorded under an older identifier stay on its page.

A read of run history that fails, the read of the issue's outcome and of reaction activity included, answers `503` with `history_unavailable`, for a live and for a non-live identifier, rather than `history` `null` or `404`.

#### `GET /api/v1/runs`: run history

Returns the finished runs of run history, newest first, one page at a time. Every filter narrows the result on the server before the page is cut, so a client asks for the runs that finished today with `since` and reads only those.

**Query parameters.** Every parameter is optional. An empty value is the same as an absent one, the way a form's "any" option sends `rule=`. Filters combine with AND and match exactly, case included.

| Parameter  | Value                                         | Selects |
| ---------- | --------------------------------------------- | ------- |
| `issue`    | An issue identifier                           | The runs whose `issue_identifier` equals it. |
| `issue_id` | A tracker issue ID                            | The runs whose `issue_id` equals it. |
| `rule`     | A dispatch rule name, or `<none>`             | The runs whose `rule_name` equals it, `default` for `dispatch.default`. `<none>` selects the runs served without `rule_name`, which ran on the workflow-wide fallback. |
| `status`   | A run status                                  | The runs whose `status` equals it. |
| `stage`    | A dispatch rule name                          | The runs whose exit reached a stage hop decision targeting that rule, whatever its result. |
| `since`    | An RFC 3339 timestamp, any offset, fraction allowed | The runs completed at or after it. |
| `until`    | An RFC 3339 timestamp, as `since`             | The runs completed before it. When both are given, `since` MUST be before `until`. |
| `ended_since` | An RFC 3339 timestamp, as `since`          | Every run of an issue whose `issue_outcome` was observed at or after it, a run that completed earlier included. A run of an issue with no outcome matches no ending bound. |
| `ended_until` | An RFC 3339 timestamp, as `since`          | Every run of an issue whose `issue_outcome` was observed before it. When both are given, `ended_since` MUST be before `ended_until`. |
| `cursor`   | A `next_cursor` value                         | The runs recorded before the last run of the previous page. |
| `limit`    | An integer from 1 to 200, default 50          | The page size. |

The response is ordered by descending `run_id`, the order Sortie recorded the runs. Each run is appended when it completes, so this is completion order. `next_cursor` is `null` on the last page and otherwise the `run_id` of the page's last run, as a string. A client treats it as opaque and passes it back with any filters. A run recorded between two requests never lands on a later page and never displaces a served run; reading the first page again shows it.

```json
{
  "runs": [
    {
      "run_id": 42,
      "issue_id": "abc123",
      "issue_identifier": "MT-649",
      "issue_title": "Fix the flaky login test",
      "issue_url": "https://tracker.example.com/browse/MT-649",
      "attempt": 2,
      "status": "succeeded",
      "error": null,
      "declared_outcome": "needs-human-review",
      "agent_text": "The migration needs a decision on the retention period before it can ship.",
      "rule_name": "implement",
      "agent_kind": "claude-code",
      "configured_model": "claude-sonnet-4-20250514",
      "configured_effort": "high",
      "model_name": "claude-sonnet-4-20250514",
      "workflow_file": "WORKFLOW.md",
      "started_at": "2026-02-24T20:10:12Z",
      "completed_at": "2026-02-24T20:31:47Z",
      "dispatched_at": "2026-02-24T19:48:20Z",
      "turns_completed": 9,
      "tokens": {
        "input_tokens": 52000,
        "output_tokens": 9100,
        "total_tokens": 61100,
        "cache_read_tokens": 30000,
        "cache_write_tokens": 4000
      },
      "uncached_tokens": 31100,
      "tokens_measured": true,
      "unaccounted_turns": 0,
      "estimated_cost_usd": 0.1923,
      "cost_status": "estimated",
      "model_usage": [
        {
          "model": "claude-sonnet-4-20250514",
          "tokens": {
            "input_tokens": 52000,
            "output_tokens": 9100,
            "total_tokens": 61100,
            "cache_read_tokens": 30000,
            "cache_write_tokens": 4000
          },
          "uncached_tokens": 31100,
          "estimated_cost_usd": 0.1923,
          "cost_status": "estimated",
          "unrated_classes": []
        }
      ],
      "skills": [{"skill": "ce-plan"}, {"skill": "ce-work", "tool_error": true}],
      "chain_id": "3c8v5n2w7r1t4y6u9i0o8p2asd",
      "stage_previous": "specify",
      "stage_previous_outcome": "succeeded",
      "stage_target": "review",
      "stage_result": "advanced",
      "self_review": {
        "final_verdict": "pass",
        "total_iterations": 2,
        "cap_reached": false
      },
      "origin_recorded": true,
      "trigger": "ci_fix",
      "reaction_kind": "ci",
      "reaction_config_key": "ci_failure",
      "session_id": "thread-1-turn-1",
      "ssh_host": "",
      "dispatch_id": "RLQFSIOFERWGDW2HZY45W55VXK",
      "stage_chain": ["specify", "implement", "review"],
      "pr_loop": {"ci_failure": {"result": "acted", "continuations": 1}, "review_comments": {"result": "quiet", "continuations": 0}, "bot_review": {"result": "quiet", "continuations": 0}, "auto_merge": {"result": "quiet", "continuations": 0}},
      "issue_outcome": {
        "kind": "merged",
        "observed_at": "2026-02-24T21:02:10Z",
        "merge_path": "auto_merge",
        "pull_request": {"number": 936, "repository": "acme/web"},
        "status_text": "Merged #936",
        "status_text_full": "Ended merged: auto-merge merged PR #936."
      }
    }
  ],
  "next_cursor": "42"
}
```

`runs` is `[]` when no run matches. The same run shape serves `history.runs` of the per-issue response.

**Run fields:**

| Field                    | Type               | Description |
| ------------------------ | ------------------ | ----------- |
| `run_id`                 | integer            | The run's key in run history, and the paging key. |
| `issue_id`               | string             | The tracker's issue ID. |
| `issue_identifier`       | string             | The issue identifier the run was recorded under. |
| `display_identifier`     | string or absent   | The qualified form of the identifier, omitted when the identifier suffices. |
| `issue_title`            | string or absent   | The tracker's title of the issue as read when the run was dispatched. Omitted when empty. Masked: a value registered as a secret after the run is replaced by the mask marker too. |
| `issue_url`              | string or absent   | The tracker's web link as read then. Omitted when empty. Masked like `issue_title`. |
| `attempt`                | integer            | The attempt number at the time of the run. |
| `status`                 | string             | The run's disposition. |
| `error`                  | string or `null`   | The run's error message, `null` when it recorded none. A run whose handoff Sortie withheld because the agent changed nothing, or because Sortie could not tell, reads as a plain sentence saying so, such as `The agent finished without changing anything, so Sortie did not hand the issue off.`; so does a run that ended on an agent error, such as `Sortie stopped the session before the agent finished turn 1.` for a cancelled turn, which never shows Go error-chain prefixes or `context canceled`; so does a run Sortie stopped itself, such as `The agent sent no event within the stall timeout, so Sortie stopped the run.`, `Sortie stopped the run because the issue moved to Done, a final state.`, `Sortie stopped the run because the service was shutting down.`, `Sortie stopped the run because the issue's estimated cost, $12.40, reached its limit of $10.` or `The CI checks failed on commit <sha>.`. A message the agent supplied that only repeats the sentence is left out. The stored text is not served for any of these. Masked like `issue_title`. |
| `declared_outcome`       | string or `null`   | The status the agent ended the run on: `blocked`, `needs-human-review`, or `no-change-needed`. It states what the agent declared, and `status` stays the disposition Sortie recorded, so a `needs-human-review` run whose handoff Sortie withheld reads `failed` with its declaration. `null` when the run ended on no recognized status, and for a run recorded before this field existed. Never `""`. |
| `agent_text`             | string or `null`   | The agent's reason for the status in `declared_outcome`, after the cleanup described in [The agent's reason on a stop](#the-agents-reason-on-a-stop). `null` when the agent wrote none and when `declared_outcome` is `null`. Never `""`. Masked when the response is built, so a secret registered after the run is replaced by the mask marker too. |
| `rule_name`              | string or absent   | The dispatch rule that routed the run, `default` for `dispatch.default`, omitted when the workflow-wide fallback routed it. |
| `agent_kind`             | string or absent   | The agent kind the run used. Omitted when empty. |
| `configured_model`, `configured_effort` | string or absent | The `model` and `effort` the run's resolved settings block carried. Omitted when unset. |
| `model_name`             | string or absent   | The model the runtime reported running. Omitted when it reported none. |
| `workflow_file`          | string or absent   | The base filename of the `WORKFLOW.md` the run was driven by. Omitted when empty. |
| `started_at`, `completed_at` | string         | RFC 3339 timestamps in UTC, as recorded. |
| `dispatched_at`          | string or `null`   | When Sortie dispatched the first run of the pass the run belongs to, as on a running row. `null` when the run could not be dated. |
| `turns_completed`        | integer            | The agent turns the run completed. |
| `tokens`                 | object             | The token members of a running row's `tokens`. Each member is `null` exactly when `tokens_measured` is `false`; a measured zero is `0`. |
| `uncached_tokens`        | integer or `null`  | `tokens.total_tokens` less `tokens.cache_read_tokens`, floored at zero, so `uncached_tokens` plus `tokens.cache_read_tokens` equals `tokens.total_tokens`. A sibling of `tokens`. `null` exactly when `tokens_measured` is `false`. |
| `tokens_measured`        | boolean            | True when the runtime reported a usage figure for the run, or the run never entered an agent turn. |
| `unaccounted_turns`      | integer            | The turns that issued a model request no token figure was proven to account for. When it is non-zero, the token figures are lower bounds. |
| `estimated_cost_usd`     | number or `null`   | The estimated USD cost recorded when the run finished, from its tokens per model under the rates in force then, and never re-priced ([Section 4.4](#44-token_rates--cost-estimation)). Unrounded. Non-`null` exactly when `cost_status` is `estimated` or `partial`. |
| `cost_status`            | string             | The status recorded with the cost: `estimated`, which a zero rate also gives, and which a run that spent nothing with no unaccounted turn gives whether or not a rate prices its model; `partial` when the estimate is a lower bound because the run has unaccounted turns, or because a model or a token class it spent had no rate beside priced spend; `unpriced` when the run spent tokens and no rate priced any of them; `unmeasured` when `tokens_measured` is `false`. |
| `model_usage`            | array              | The run's spend split by model, one element per model the run spent tokens on, ordered by `model`. Always present, never `null`. `[]` when the run recorded no per-model spend: an unmeasured run, a run that spent nothing, and a run recorded before per-model spend was kept that no start has priced yet. The members of an element are below. |
| `skills`                 | array or absent    | Every skill call of the run, in completion order, repeats included and with no limit. Each element holds `skill`, the skill name, and `tool_error` as `true` when the call failed; a failed call loaded no skill. Omitted when the run made no skill call, when its agent reports none, and for a run recorded before the list was kept. The list stays complete when the run's oldest events are no longer kept. |
| `chain_id`               | string or absent   | The stage chain identifier of the run. Omitted when empty. |
| `stage_previous`, `stage_previous_outcome` | string or absent | The rule whose hop led to the run's dispatch and that hop's outcome, `succeeded` or `no_change`. Omitted when empty. |
| `stage_target`, `stage_result` | string or absent | The rule the run's exit targeted with a hop decision and its result: `advanced`, `partial`, `failed`, or `ceiling`. Omitted when the run reached no decision. A read taken just after the run completes can precede the decision, and the next read carries it. |
| `self_review`            | object or `null`   | `null` when the run carries no self-review record. Otherwise `final_verdict` (`none` when the run recorded an empty verdict, masked like `issue_title`), `total_iterations`, and `cap_reached`. |
| `origin_recorded`        | boolean            | `false` for a run recorded before the origin keys below existed. Every origin key is then `null`. |
| `trigger`                | string or `null`   | What started the retry whose fire dispatched the run: `error`, `usage_limit`, `continuation`, `stall`, or `ci_fix`. `""` for a dispatch from the poll tick and for a retry a reaction other than the CI fix started, which `reaction_kind` names. `null` when not recorded, and for a run dispatched by a retry queued before the upgrade that never stored what started it. |
| `reaction_kind`          | string or `null`   | The reaction kind of the retry that dispatched the run, `""` when none, `null` when not recorded. |
| `reaction_config_key`    | string or `null`   | The key of the `reactions` section of `WORKFLOW.md` that `reaction_kind` is configured under. `""` or `null` exactly when `reaction_kind` is. |
| `session_id`             | string or `null`   | The agent session the run used, `""` when it started none, `null` when not recorded. Not masked. |
| `ssh_host`               | string or `null`   | The SSH worker the run executed on, `""` for a local run, `null` when not recorded. |
| `dispatch_id`            | string or `null`   | The dispatch ID of the run, the key of its agent events on [`GET /api/v1/runs/{dispatch_id}/events`](#get-apiv1runsdispatch_idevents-run-events). `""` on the recorded CI result, which no dispatch produced, and `null` for a run recorded before the dispatch ID existed. A run with either has no retained events. |
| `stage_chain`            | array of strings or `null` | The stage chain the run's rule stands on, as the rule names of the chain in `next` order. It is the chain `stage_position.chain` serves for the run's `rule_name` and `stage_previous`, and it follows the configuration in effect, so a reload that edits the chains changes it for runs recorded before the reload. Always present. `null` when the run has no `rule_name`, when its rule is on no chain, and when the server holds no workflow shape. Not masked. On the per-issue response, `history.last_stage.chain` equals the `stage_chain` of the deciding run. |
| `issue_outcome`          | object or `null`   | How the issue's latest work ended, as Sortie observed it, or `null` when Sortie observed no ending since it last picked the issue up, or when the issue ended before outcomes were recorded. Always present. It belongs to the issue, so every run of one issue carries the same object, and it is served as recorded whatever the issue's live state. `kind` is `handed_off`, `merged`, or `terminal`, and `observed_at` is when Sortie made or observed the ending, in UTC. `state` is present for `handed_off` and `terminal`: the state the handoff wrote, or the terminal state Sortie read. `merge_path` and `pull_request` are present for `merged`: `merge_path` is `auto_merge` or `merge_completion`, the `reactions` key that observed the merge, and `pull_request` holds `number` and `repository`. `state` and `repository` are masked like `issue_title`. `status_text` and `status_text_full` are always present: the sentence of how the issue ended, worded from the outcome alone, so a stage hop that was not made appears only in the top-level pair of a `history` response. |
| `pr_loop`                | object or `null`   | What each configured pull request reaction recorded in the run's own pass, in the shape described with the `active` entries. Always present, `null` when the server holds no workflow shape, and `null` on every run in the groups of `GET /api/v1/runs/issues` and in `history.runs`. |

**`model_usage` members.** Each element is one model's part of the run's spend. It carries no price source and no price table version.

| Field                | Type              | Description |
| -------------------- | ----------------- | ----------- |
| `model`              | string            | The model the tokens were spent on, as the runtime named it, `""` when it named none. Not masked. |
| `tokens`             | object            | The token members of a running row's `tokens`, for this model. No member is `null`, and `total_tokens` is `input_tokens` plus `output_tokens`. |
| `uncached_tokens`    | integer           | `tokens.total_tokens` less `tokens.cache_read_tokens`, floored at zero. |
| `estimated_cost_usd` | number or `null`  | The USD cost recorded for this model's part, unrounded and never re-priced. Non-`null` exactly when `cost_status` is `estimated` or `partial`. |
| `cost_status`        | string            | `estimated`, `partial` when a token class the model spent has no rate, or `unpriced`. A part recorded without a price reads `unpriced` with a `null` cost until a start prices it. |
| `unrated_classes`    | array or `null`   | The token classes whose missing rate left the part `partial` or `unpriced`, from `input`, `output`, `cache_read`, and `cache_write`, in that order. `[]` when the part names none: every class it spent had a rate, or its spend was reported as a total without classes. `null` when the part records none, because it has no recorded price or was priced before the classes were kept; `null` never means `[]`. |

The recorded CI result of a failing check is a run with `status` `ci_failed`, no agent kind, and `""` in each origin key and in `dispatch_id`, because it records a verdict and nothing started it.

**Errors:**

| Condition | Status | `error.code` |
| --------- | ------ | ------------ |
| An unknown parameter, a parameter given more than once, a malformed or out-of-range value, `since` not before `until`, or `ended_since` not before `ended_until` | 400 | `invalid_parameter`; the message names the parameter and the rule, for example `parameter "limit" must be an integer from 1 to 200`. No run is read. |
| Run history, the outcomes of the served issues, or their reaction activity cannot be read, or the server has none | 503 | `history_unavailable` |
| A method other than `GET` | 405 | `method_not_allowed` |

A request whose filters match fewer runs than the page holds reads run history down to its start, and a read waits for the database connection the orchestrator uses, so a narrow filter on a long history takes longer than a page of recent runs.

#### `GET /api/v1/runs/issues`: run history by issue

Returns the finished runs of run history grouped by issue, one page of issues at a time, the issue with the newest matching run first. A group holds one issue however many runs it has, so an issue that retried many times is one group and never fills the page. Each group says how its issue ended or what it is doing now, lists the issue's newest runs, and carries the issue's totals.

**Query parameters.** Every parameter is optional. An empty value is the same as an absent one, a parameter given twice is refused, and so is any parameter not in this table. Filters combine with AND and match exactly, case included.

| Parameter | Value                                          | Selects |
| --------- | ---------------------------------------------- | ------- |
| `issue`   | An issue key                                   | The issues with a run whose `issue_identifier` or `display_identifier` equals it, so the `owner/repo#9` form a GitHub, GitLab, or Gitea tracker shows finds its issue. A selected issue's group lists its runs whatever key each run recorded. On [`GET /api/v1/runs`](#get-apiv1runs-run-history), `issue` matches `issue_identifier` alone. |
| `rule`    | A dispatch rule name, `default`, or `<none>`   | The runs whose `rule_name` equals it, as on `GET /api/v1/runs`. |
| `status`  | A run status                                   | The runs whose `status` equals it. |
| `stage`   | A dispatch rule name                           | The runs whose exit reached a stage hop decision targeting that rule, whatever its result. |
| `cursor`  | A `next_cursor` value                          | The groups after the previous page. |
| `limit`   | An integer from 1 to 50, default 20            | The page size, in groups. |

A group is one issue with at least one run that matches `rule`, `status`, and `stage`, and the groups are ordered by each issue's newest matching run, descending `run_id`. `next_cursor` is `null` on the last page and otherwise the `run_id` of the page's last group's newest matching run, as a string. A client treats it as opaque and passes it back with any filters. The next page holds the groups whose newest matching run is older and that have no matching run at or above the cursor, so a traversal serves each group once. A run recorded between two requests moves its issue's group to the first page and never repeats a group already served on a later page.

```json
{
  "groups": [
    {
      "issue_id": "abc123",
      "issue_identifier": "9",
      "display_identifier": "acme/web#9",
      "issue_title": "Fix the flaky login test",
      "issue_url": "https://github.com/acme/web/issues/9",
      "status": "history",
      "status_text": "Merged #936",
      "status_text_full": "Ended merged: auto-merge merged PR #936.",
      "issue_outcome": {
        "kind": "merged",
        "observed_at": "2026-02-24T21:02:10Z",
        "merge_path": "auto_merge",
        "pull_request": {"number": 936, "repository": "acme/web"},
        "status_text": "Merged #936",
        "status_text_full": "Ended merged: auto-merge merged PR #936."
      },
      "runs": [
        {
          "run_id": 42,
          "issue_id": "abc123",
          "issue_identifier": "9",
          "display_identifier": "acme/web#9",
          "issue_title": "Fix the flaky login test",
          "issue_url": "https://github.com/acme/web/issues/9",
          "attempt": 2,
          "status": "succeeded",
          "error": null,
          "rule_name": "implement",
          "agent_kind": "claude-code",
          "started_at": "2026-02-24T20:10:12Z",
          "completed_at": "2026-02-24T20:31:47Z",
          "turns_completed": 9,
          "tokens_measured": true,
          "estimated_cost_usd": 0.1923,
          "cost_status": "estimated",
          "pr_loop": null
        }
      ],
      "next_cursor": "37",
      "totals": {
        "run_count": 6,
        "unmeasured_runs": 0,
        "tokens": {
          "input_tokens": 310000,
          "output_tokens": 52000,
          "total_tokens": 362000,
          "cache_read_tokens": 150000,
          "cache_write_tokens": 20000
        },
        "uncached_tokens": 212000,
        "tokens_complete": true,
        "estimated_cost_usd": 1.1425,
        "cost_status": "estimated"
      }
    }
  ],
  "next_cursor": "31"
}
```

The example abridges the run to some of its fields; `runs` serves the full run shape of [`GET /api/v1/runs`](#get-apiv1runs-run-history), and `groups` is `[]` when no issue has a matching run.

**Group fields:**

| Field | Type | Description |
| ----- | ---- | ----------- |
| `issue_id` | string | The tracker's issue ID. |
| `issue_identifier` | string | The issue identifier of the issue's newest run. |
| `display_identifier` | string or absent | The qualified form of the identifier, omitted when empty. |
| `issue_title`, `issue_url` | string or absent | The title and link of the issue's newest run. Omitted when empty. Masked: a value registered as a secret after the run is replaced by the mask marker too. |
| `status` | string | What [`GET /api/v1/{identifier}`](#get-apiv1identifier--per-issue-detail) serves for the issue at the same instant. While a live entry of the state holds the issue, its status, such as `running` or `retrying`. Otherwise `history`. |
| `status_text`, `status_text_full` | string | The status sentence pair. While a live entry holds the issue, the entry's pair. Otherwise the pair worded from the issue's outcome and its newest run's hop decision. |
| `issue_outcome` | object or `null` | How the issue's latest work ended, in the shape of the `issue_outcome` run field, or `null` when Sortie observed no ending. |
| `runs` | array | The issue's newest matching runs, at most 5, newest first, none newer than the group's newest matching run. |
| `next_cursor` | string or `null` | Non-`null` exactly when the issue has an older matching run. It continues on `GET /api/v1/runs` with `issue_id` set to the group's `issue_id` and the request's `rule`, `status`, and `stage`. |
| `totals` | object | The `totals` of the per-issue response, over every run of the issue whatever the filters select. |

Sortie reads the state before run history, as the per-issue route does, so a run that ends between the two reads appears in both and never in neither.

**Errors:**

| Condition | Status | `error.code` |
| --------- | ------ | ------------ |
| An unknown parameter, a parameter given more than once, a `limit` that is not an integer from 1 to 50, or a `cursor` that is not an integer of 1 or more | 400 | `invalid_parameter`; the message names the parameter and the rule, for example `parameter "limit" must be an integer from 1 to 50`. Nothing is read. |
| Run history or the outcomes of the served issues cannot be read, or the server has none | 503 | `history_unavailable` |
| The runtime snapshot cannot be read | 503 | `snapshot_unavailable` |
| A method other than `GET` | 405 | `method_not_allowed` |

A client that disconnects gets no response, and the reads stop.

The route reads through the connection the orchestrator uses. One statement selects the page's groups and stops once it holds them, and Sortie reads each served issue's runs, newest run, and totals only after that statement closes, through an index on the issue ID. An `issue` key resolves through the indexes on the identifier and on the display identifier. A filter that matches few runs on a long history reads down to the start of run history, so a narrow filter takes longer than a page of recent issues.

#### `GET /api/v1/runs/{dispatch_id}/events`: run events

Returns the agent events Sortie retained for one run, oldest first. `{dispatch_id}` is the `dispatch_id` of a running row or of a run, one path segment. The route ignores the query string.

A running run keeps its newest 1,000 retained events, within 256 KiB, in memory, so a very long run serves `first_seq` above `1`. At worker exit the events are written with the run's history row, and a finished run's events stay readable for at least 365 days after it ends, within 2 GiB across all runs. A write can fail while the run's history row succeeds; Sortie then retries the events from memory up to five times, 4 to 64 seconds apart, and until one retry succeeds, and after Sortie restarts, the run answers `404`. Liveness pings, `token_usage` events, and an event that repeats the newest retained notification, other message, or malformed message are not retained.

```json
{
  "dispatch_id": "RLQFSIOFERWGDW2HZY45W55VXK",
  "first_seq": 41,
  "events": [
    {
      "seq": 41,
      "at": "2026-02-24T20:14:31Z",
      "event": "tool_result",
      "message": "",
      "tool_name": "Bash",
      "tool_kind": "shell",
      "tool_argument": "go test ./...",
      "duration_ms": 2400,
      "tool_error": false,
      "text": "Ran go test ./... in 2.4s.",
      "tool_line": "go test ./..."
    },
    {
      "seq": 42,
      "at": "2026-02-24T20:14:52Z",
      "event": "turn_completed",
      "message": "",
      "usage": {
        "input_tokens": 1200,
        "output_tokens": 800,
        "total_tokens": 2000,
        "cache_read_tokens": 400,
        "cache_write_tokens": 100
      },
      "text": "Turn completed."
    }
  ]
}
```

`first_seq` is the `seq` of the first event served, `null` when `events` is `[]`, which only a running run that has retained no event yet serves. A value above `1` says earlier events were evicted.

**Event members:**

| Member | Present on | Value |
| ------ | ---------- | ----- |
| `seq` | every event | The event's number within the run, from 1, rising by one per retained event. Eviction never renumbers. |
| `at` | every event | When the event arrived, an RFC 3339 timestamp in UTC. |
| `event` | every event | The normalized event type. |
| `message` | every event | The event's message, masked and cut to 999 runes followed by `…` when it exceeds 1,000 runes. `""` when the event carries none. Masked: a value registered as a secret after the event was retained is replaced by `[redacted]`. |
| `text` | every event | One plain English sentence that words the event. The `text` of a `notification` and an `other_message` is the message itself. A session that fails to start gets a `startup_failed` event whose sentence states the failure. |
| `session_id` | `session_started` | The session the run started, `""` when the event carries none. |
| `tool_name` | `tool_result` | The name the agent runtime gives the tool, or `{server}.{tool}` for an MCP or dynamic tool, such as `github.get_issue`. Omitted when the runtime names none. It is never a command or a protocol item type. |
| `tool_kind` | `tool_result` | `shell`, `read`, `edit`, or `skill`. Omitted for any other tool and for an event recorded before this member existed. |
| `tool_argument` | `tool_result` | The call's one main argument: a shell call's command without its `bash -lc` or `sh -c` wrapper, a read or edit call's path relative to the workspace (several paths joined by `, `), a skill call's skill name. Masked, stripped of control characters, and cut to 999 runes followed by `…`. No other argument of a call is served. Omitted when the call has none and for an event recorded before this member existed. |
| `tool_line` | `tool_result` | The call as one line of text: a shell call's command; otherwise the tool's name followed by its main argument, such as `Read internal/foo.go` or `skill ce-plan`; otherwise the name alone. The session view draws it. Omitted on no other event. |
| `duration_ms` | `tool_result` | The tool call's duration in milliseconds. Omitted when the adapter reported none. |
| `tool_error` | `tool_result` | Whether the tool call returned an error. |
| `usage` | `turn_completed`, `turn_failed`, `turn_cancelled`, `turn_ended_with_error`, `turn_input_required` | The run's token totals after the event, with the members of a running row's `tokens`. `null` while the run has measured no usage, so an unmeasured figure never reads as zero. |

**Errors:**

| Condition | Status | `error.code` |
| --------- | ------ | ------------ |
| No run carries the dispatch ID, or its events were pruned, lost in a crash, recorded before retention existed, never retained, or not written | 404 | `run_events_not_retained` |
| Run history cannot be read, or the server has none and the run is not running | 503 | `history_unavailable` |
| A method other than `GET` | 405 | `method_not_allowed`, with `Allow: GET` |

#### `GET /api/v1/stats`: run statistics

Returns the report that `sortie stats --format json` prints, over the finished runs of run history in a range: how many runs and issues there were, how many succeeded and how many failed, stopped, or were cancelled, how long they took, the tokens they spent, what they cost as recorded with each run, and that cost per UTC day or month, with breakdowns by outcome, agent, dispatch rule, prompt template, model (a run counts under every model it spent tokens on), stage chain, retry trigger, and run origin, and a funnel for each stage chain whose stages also count the passes running, retrying, or held by a budget now, and the runs, calls, and failed calls of each skill the runs called. The route computes the report through the aggregation the command uses and serves it unchanged, so for one database, one workflow, and absolute bounds the body equals the command's output except `generated_at`.

**Query parameters.** Every parameter is optional. An empty value is the same as an absent one, and an open bound is unlimited, so a request without parameters covers every run on record.

| Parameter | Value                                         | Selects |
| --------- | --------------------------------------------- | ------- |
| `since`   | A range bound                                 | The runs completed at or after it. |
| `until`   | A range bound                                 | The runs completed before it. When both are given, `since` MUST be before `until`. |

A range bound is an RFC 3339 timestamp in any offset (fractions allowed), a date such as `2026-07-01` (00:00:00 UTC that day), or a positive Go duration such as `24h` or `168h`, which counts back from the instant of the request. The command takes the same values for `--since` and `--until`. A timestamp without an offset, a zero or negative duration, a duration in days such as `7d`, and any other text are malformed.

```json
{
  "generated_at": "2026-02-24T20:15:30Z",
  "workflow_path": "/srv/sortie/WORKFLOW.md",
  "db_path": "/srv/sortie/.sortie.db",
  "since": "2026-02-23T20:15:30Z",
  "until": null,
  "schema_tier": "full",
  "warnings": [],
  "summary": {
    "runs": 6,
    "issues": 5,
    "succeeded": 3,
    "failed": 3,
    "stopped": 0,
    "cancelled": 0,
    "success_rate": 0.5,
    "duration_seconds": {"p50": 1200, "p95": 1800, "mean": 1200, "samples": 3},
    "mean_turns_succeeded": 4,
    "tokens": {"input": 3900000, "output": 760000, "total": 4660000, "cache_read": 50000, "cache_write": 10000, "uncached": 4610000},
    "cost_usd": 19.52,
    "cost_per_succeeded_run_usd": 6.506666667,
    "zero_duration_runs": 1,
    "duration_excluded_runs": 1,
    "cost_unpriced_runs": 1,
    "cost_partial_runs": 1,
    "tokens_unmeasured_runs": 1
  },
  "spend": {
    "period": "day",
    "buckets": [
      {"start": "2026-02-23T00:00:00Z", "runs": 2, "cost_usd": 7.27, "tokens_unmeasured_runs": 1, "cost_unpriced_runs": 0, "cost_partial_runs": 0},
      {"start": "2026-02-24T00:00:00Z", "runs": 4, "cost_usd": 12.25, "tokens_unmeasured_runs": 0, "cost_unpriced_runs": 1, "cost_partial_runs": 1}
    ]
  },
  "by_status": [
    {
      "name": "succeeded",
      "runs": 3,
      "succeeded": 3,
      "success_rate": 1,
      "share": 0.5,
      "duration_seconds": {"p50": 1200, "p95": 1800, "mean": 1200, "samples": 3},
      "mean_turns": 4,
      "tokens": {"input": 3500000, "output": 700000, "total": 4200000, "cache_read": 50000, "cache_write": 10000, "uncached": 4150000},
      "cost_usd": 17.87,
      "cost_per_succeeded_run_usd": 5.956666667,
      "tokens_unmeasured_runs": 0,
      "cost_unpriced_runs": 0,
      "cost_partial_runs": 0
    }
  ],
  "by_adapter": [],
  "by_rule": [],
  "by_template": [],
  "by_model": [],
  "by_chain": [
    {
      "name": "3c8v1n6w0z4y7t2s5r9q8p1mkd",
      "runs": 2,
      "succeeded": 2,
      "success_rate": 1,
      "share": 0.3333,
      "duration_seconds": {"p50": 600, "p95": 1800, "mean": 1200, "samples": 2},
      "mean_turns": 4,
      "tokens": {"input": 3000000, "output": 600000, "total": 3600000, "cache_read": 50000, "cache_write": 10000, "uncached": 3550000},
      "cost_usd": 17.87,
      "cost_per_succeeded_run_usd": 8.935,
      "tokens_unmeasured_runs": 0,
      "cost_unpriced_runs": 0,
      "cost_partial_runs": 0,
      "issue": "MT-649",
      "stages": ["implement", "review"],
      "last_hop": "advanced"
    }
  ],
  "chain_funnels": [
    {
      "stages": [
        {
          "rule": "implement", "entry": true, "last": false, "runs": 3, "entered": 3,
          "arrivals": {"hop": 0, "match": 3, "label": 0},
          "outcomes": {"advanced": 2, "handed_off": 0, "ceiling": 0, "hop_failed": 0, "no_hop": 1, "in_stage": 0},
          "no_hop_reasons": [{"reason": "run_failed", "passes": 1}],
          "median_seconds": 900
        },
        {
          "rule": "review", "entry": false, "last": true, "runs": 3, "entered": 2,
          "arrivals": {"hop": 2, "match": 0, "label": 0},
          "outcomes": {"advanced": 0, "handed_off": 1, "ceiling": 0, "hop_failed": 0, "no_hop": 0, "in_stage": 1},
          "no_hop_reasons": [],
          "median_seconds": 600
        }
      ]
    }
  ],
  "by_trigger": [
    {
      "name": "<unknown>",
      "runs": 4,
      "succeeded": 2,
      "success_rate": 0.5,
      "share": 0.6667,
      "duration_seconds": {"p50": 600, "p95": 1800, "mean": 900, "samples": 4},
      "mean_turns": 3,
      "tokens": {"input": 3000000, "output": 600000, "total": 3600000, "cache_read": 0, "cache_write": 0, "uncached": 3600000},
      "cost_usd": 12.25,
      "cost_per_succeeded_run_usd": 6.13,
      "tokens_unmeasured_runs": 1,
      "cost_unpriced_runs": 1,
      "cost_partial_runs": 1
    }
  ],
  "by_origin": [
    {
      "name": "<unknown>",
      "runs": 4,
      "succeeded": 2,
      "success_rate": 0.5,
      "share": 0.6667,
      "duration_seconds": {"p50": 600, "p95": 1800, "mean": 900, "samples": 4},
      "mean_turns": 3,
      "tokens": {"input": 3000000, "output": 600000, "total": 3600000, "cache_read": 0, "cache_write": 0, "uncached": 3600000},
      "cost_usd": 12.25,
      "cost_per_succeeded_run_usd": 6.13,
      "tokens_unmeasured_runs": 1,
      "cost_unpriced_runs": 1,
      "cost_partial_runs": 1
    }
  ],
  "skill_calls": {
    "unrecorded_runs": 1,
    "by_skill": [
      {"skill": "plan", "runs": 2, "calls": 2, "failed_calls": 0},
      {"skill": "test", "runs": 1, "calls": 2, "failed_calls": 1}
    ]
  },
  "self_review": {
    "runs_with_metadata": 2,
    "by_final_verdict": [{"verdict": "iterate", "runs": 1}, {"verdict": "pass", "runs": 1}],
    "cap_reached_runs": 1,
    "mean_iterations": 2.5,
    "unparsed_runs": 1
  }
}
```

The example omits the members of the breakdowns other than `by_status`, `by_chain`, `by_trigger`, and `by_origin`; every breakdown is a list of groups of the same shape. It shows one funnel of two stages. Its `skill_calls` come from runs that called `plan`, `test` (failed), and `test`; `plan`; no skill; and from one run recorded before skill calls were kept.

**Envelope fields:**

| Field            | Type              | Description |
| ---------------- | ----------------- | ----------- |
| `generated_at`   | string            | The instant of the request, as an RFC 3339 timestamp in UTC. Relative bounds count back from it. |
| `workflow_path`, `db_path` | string  | The host paths of the `WORKFLOW.md` and the database the report reads, as the command prints them. |
| `since`, `until` | string or `null`  | The resolved bounds in UTC, `null` when open. |
| `schema_tier`    | string            | `full`, or `base` for a database that lacks columns the full report needs. On `base`, `by_rule`, `by_template`, `by_model`, `by_chain`, `chain_funnels`, `by_trigger`, and `by_origin` are `[]`, `spend`, `skill_calls`, and `self_review` are `null`, and the figures that need the missing columns are `null`. |
| `warnings`       | array of strings  | Always present, `[]` when there is nothing to report. For an older database it holds one entry that names every part of the report the database leaves out and how to get it. A database whose runs carry no recorded cost, because no service start has applied the schema change that records it, names cost among what it leaves out. |
| `summary`        | object            | The report-wide figures, below. |
| `spend`          | object or `null`  | The recorded cost per UTC day or month, below. `null` on the `base` tier and on a database whose runs carry no recorded cost. |
| `by_status`      | array of groups   | One group per run status. |
| `by_adapter`     | array of groups   | One group per agent kind. `<none>` names the runs that carry no agent kind, a recorded CI result included. |
| `by_rule`        | array of groups   | One group per dispatch rule name, as in the `rule_name` of a run row. `<none>` names the runs that ran on the workflow-wide fallback. |
| `by_template`    | array of groups   | One group per prompt template the rule selected, named as the run recorded it, which can be a host path. `<none>` names the runs that recorded none. |
| `by_model`       | array of groups   | One group per model a run spent tokens on, below. A run counts in the group of every such model, so it is not a partition. `<none>` names the runs with no model. |
| `by_chain`       | array of chain groups | One entry per stage chain in which a hop was due or made. It is a listing, not a partition: runs outside a chain, and chains without a hop, are absent. |
| `chain_funnels`  | array of funnels  | One funnel per stage chain definition, below. It is not a breakdown: its entries are stages, not groups of runs. |
| `by_trigger`     | array of groups   | One group per stored retry trigger, below. |
| `by_origin`      | array of groups   | One group per run origin, below. |
| `skill_calls`    | object or `null`  | The skill calls of the counted runs, below. `null` on a database that lacks the skills column or a column an earlier tier needs, which the `warnings` entry then states; a database with the column counts runs written before Sortie kept skill calls in `unrecorded_runs`. |
| `self_review`    | object or `null`  | The self-review results, below. `null` on the `base` tier. |

Every breakdown is sorted by descending `runs`, then ascending `name`, and is `[]` rather than `null` when it holds no group.

**`summary` fields:**

| Field                        | Type             | Description |
| ---------------------------- | ---------------- | ----------- |
| `runs`                       | integer          | The runs completed in the range. |
| `issues`                     | integer          | The distinct issues the counted runs belong to, `0` when no run is in range. A recorded CI result counts its issue like any other run. |
| `succeeded`                  | integer          | The runs whose status is `succeeded`. |
| `failed`                     | integer          | The runs whose status is `failed` or `ci_failed`, and the runs whose status the report does not know. |
| `stopped`                    | integer          | The runs whose status is `needs_person`, `budget_stopped`, or `usage_limited`. |
| `cancelled`                  | integer          | The runs whose status is `cancelled`. |
| `success_rate`               | number           | `succeeded` divided by `runs`, from `0` to `1`, `0` when no run is in range. |
| `duration_seconds`           | object           | `p50`, `p95`, `mean`, and `samples` over the succeeded runs. Each of them is `null` when `samples` is `0`. |
| `mean_turns_succeeded`       | number or `null` | The mean turns of the succeeded runs. `null` on the `base` tier and when no run succeeded. |
| `tokens`                     | object or `null` | The sums of `input`, `output`, `total`, `cache_read`, and `cache_write` over the runs that reported usage, and `uncached`, which is `total` less `cache_read`, floored at zero, so `uncached` plus `cache_read` equals `total` and cache writes stay in it. `null` on the `base` tier and when no run reported usage. |
| `cost_usd`                   | number or `null` | The estimated cost of the priced runs, not rounded to cents, so a total under one cent never reads as zero. `null` on the `base` tier, when no run that spent tokens is priced, and when the runs carry no recorded cost. |
| `cost_per_succeeded_run_usd` | number or `null` | `cost_usd` divided by the succeeded runs that reported usage, rounded as `cost_usd` is and not to cents, so a figure under one cent never reads as zero. `null` when `cost_usd` is, and when no succeeded run reported usage. |
| `zero_duration_runs`         | integer          | The runs that completed at the instant they started; each counts as a zero-second sample. |
| `duration_excluded_runs`     | integer          | The runs left out of every duration figure because their recorded timestamps cannot be read or end before they start. |
| `cost_unpriced_runs`         | integer          | The runs that reported usage and were recorded `unpriced`. |
| `cost_partial_runs`          | integer          | The runs recorded `partial`, whose recorded cost is a lower bound that `cost_usd` includes. A run that spent nothing and recorded an unaccounted turn counts. A recorded CI result does not. `0` on the `base` tier. |
| `tokens_unmeasured_runs`     | integer          | The runs that reported no usage. They add nothing to `tokens` and `cost_usd`. |

The outcome counts `succeeded`, `failed`, `stopped`, and `cancelled` add up to `runs`.

**Group fields.** Each breakdown entry is a group.

| Field                        | Type             | Description |
| ---------------------------- | ---------------- | ----------- |
| `name`                       | string           | The value the group collects. |
| `runs`, `succeeded`          | integer          | The runs in the group and how many succeeded. |
| `success_rate`               | number           | `succeeded` divided by `runs`. In `by_status` it is structural: the `succeeded` group reports `1`. |
| `share`                      | number           | `runs` divided by `summary.runs`, from `0` to `1`. |
| `duration_seconds`           | object           | As in `summary`, over every run in the group rather than the succeeded ones. |
| `mean_turns`                 | number or `null` | The mean turns of the group's runs. `null` on the `base` tier. |
| `tokens`                     | object or `null` | As in `summary`, over the group's runs. |
| `cost_usd`, `cost_per_succeeded_run_usd` | number or `null` | As in `summary`, over the group's runs. |
| `tokens_unmeasured_runs`     | integer          | The group's runs that reported no usage. |
| `cost_unpriced_runs`         | integer          | The group's runs that `cost_usd` leaves out for want of a rate, counted as in `summary`. `0` on the `base` tier. |
| `cost_partial_runs`          | integer          | The group's runs recorded `partial`, counted as in `summary`. `0` on the `base` tier. |

**`spend` fields.** `spend` holds `period` and `buckets`. `period` is `day` when the range has a `since` and `since` lies no more than 2160 hours before the end of the range, which is `until` or else `generated_at`, and `month` otherwise: the last 24 hours and the last 7, 30, and 90 days get days, and an open `since` gets months. `buckets` ascend by `start` and list every UTC day or month from the one holding `since` (the one holding the earliest run when `since` is open) through the later of the one holding the end of the range and the one holding the latest run, so a period without runs appears with `runs` `0` and `cost_usd` `null`. `buckets` is `[]`, never `null`, when no run falls in a period.

| Field                    | Type             | Description |
| ------------------------ | ---------------- | ----------- |
| `start`                  | string           | The first instant of the period, as an RFC 3339 timestamp in UTC. |
| `runs`                   | integer          | The runs of the report that completed in the period. |
| `cost_usd`               | number or `null` | The sum of the recorded costs of the period's priced runs, not rounded to cents. `null` when no run of the period is priced. |
| `tokens_unmeasured_runs`, `cost_unpriced_runs`, `cost_partial_runs` | integer | Counted as in `summary`, over the period's runs. |

A run counts in the bucket whose period holds its completion time, the span the daily and monthly spend ceilings count, so a day bucket and `agent.max_daily_cost_usd` over the same UTC day count the same finished runs. The buckets partition the runs the report counts: their `runs`, `tokens_unmeasured_runs`, `cost_unpriced_runs`, and `cost_partial_runs` sum to the `summary` figures, and their `cost_usd` values sum to `summary.cost_usd` within $0.000001. The first bucket of a range whose `since` lies inside a period holds only the runs at or after `since`. A bucket reads the cost recorded with each run and prices nothing. A run whose completion time does not parse as an RFC 3339 timestamp counts in the summary and the breakdowns and in no bucket; Sortie writes none.

**`by_model` groups.** A run counts once in the group of every model it spent tokens on, with the tokens and the recorded cost of that model's share of the run and with the run's outcome, turns, and duration, so a group's success rate, mean turns, duration, and cost per succeeded run cover the runs that used the model. A run with no model share that spent tokens counts once, with its own tokens and cost, under the model its runtime reported, else its configured model, whatever its age. The groups' `runs` can add up to more than `summary.runs`, and their `share` values to more than `1`. When a run is priced per model, its groups' tokens and costs add up to the run's. A `token_rates` entry for an agent kind prices a run once on its summed tokens and each model's share on its own tokens, so the shares' recorded costs can differ in sum from the run's recorded cost; the groups carry the shares as recorded and never reconcile them. A group counts `tokens_unmeasured_runs`, `cost_unpriced_runs`, and `cost_partial_runs` per share. A share counts as partial when its recorded status is `partial`, or when it is `estimated` and the run recorded unaccounted turns: the spend those turns leave out belongs to no recorded model, so the run counts as partial in every group whose share carries a recorded cost. A share recorded `unpriced` counts in `cost_unpriced_runs` only. On a database whose runs carry no recorded cost, every run counts under the one model it reported, else its configured model.

**`by_chain` extras.** A `by_chain` entry is a group whose `name` is the chain identifier, with more members: `issue`, the display identifier of the issue the chain belongs to, or its identifier when there is none; `stages`, the dispatch rule names the chain's runs used, in order; and `last_hop`, the result of the latest hop decision the chain recorded, `""` when none.

**`chain_funnels` fields.** A funnel is one stage chain definition: the chains of your workflow that share a stage count as one, so a stage reached by two chains has one row. Funnels follow the order of the chains in the workflow. A funnel is `{"stages": [...]}`, with one entry per stage in chain order, the stages of a chain that joins another before the stage where it joins. A funnel never grows with the number of passes. A pass is the runs that share a chain identifier, as [Stage chains](#stage-chains) describes; it counts once on a stage, however many runs it makes there.

| Field            | Type            | Description |
| ---------------- | --------------- | ----------- |
| `rule`           | string          | The dispatch rule of the stage. |
| `entry`          | boolean         | `true` when a chain starts at the stage. |
| `last`           | boolean         | `true` on the final stage of the funnel, the stage whose finished pass is handed off. |
| `runs`           | integer         | The finished runs of the rule in the range, retries and pull request fixes included. It equals the `runs` of the `by_rule` group of the rule. |
| `entered`        | integer         | The passes through the stage in the range, plus the passes held there now. |
| `arrivals`       | object          | `entered` split by how the pass reached the stage: `hop`, from the previous stage; `label`, a stage label that a person put on the issue; `match`, the rule matched the issue. |
| `outcomes`       | object          | `entered` split by how the pass left the stage, below. |
| `no_hop_reasons` | array           | The passes counted in `outcomes.no_hop`, as `{"reason", "passes"}` pairs, largest first. A reason without passes is absent. |
| `median_seconds` | number or `null` | The median, over the passes, of the seconds the pass spent on the stage, summing its finished runs there. `null` when no pass has a finished run with a readable duration there. |

The latest finished run of a pass on the stage decides its outcome. `outcomes` holds these members, and each pass counts in exactly one of them:

| Member       | The pass |
| ------------ | -------- |
| `in_stage`   | Has a session running or a retry queued now. |
| `advanced`   | Hopped to the next stage, with the hop result `advanced` or `partial`. Always `0` on the last stage. |
| `handed_off` | Was handed off: its run succeeded on the last stage and the issue moved on. Always `0` on any other stage. |
| `ceiling`    | Reached the hop ceiling, `dispatch.max_consecutive_hops`, so the hop was refused. |
| `hop_failed` | Failed to hop, with the hop result `failed`. |
| `no_hop`     | Made no hop, for the reason `no_hop_reasons` names. |

The reasons of `no_hop`, in the order they break a tie: `run_failed`, the run failed; `needs_person`, the agent declared `blocked`, or the run needed input; `budget_held`, a budget stopped the pass or holds it now; `usage_limited`, the latest run was stopped by a provider usage limit before it produced output; `operator_stopped`, the run was cancelled, or it succeeded on a stage that is not the last without hopping, as when the issue moved to a terminal state; `verdict_withheld`, the run failed because the evidence verdict withheld its handoff. A pass that a budget holds counts as `budget_held` unless its latest run made a hop decision or was handed off.

A pass that is running, queued for a retry, or held by a budget now is counted from the database, and only when the range is open at its end or ends after `generated_at`; for an earlier end, each pass is decided by its runs in the range. A funnel lists every stage with zero counts when no pass reached it, a range without runs included. `chain_funnels` is `[]` when the workflow has no stage chains and on a database written before Sortie recorded which runs their agent declared `blocked`, whatever its `schema_tier`; `warnings` says so in the second case while chains exist, and running Sortie once with the workflow adds what is missing. A run recorded before the upgrade is never marked as declared `blocked`, so its pass counts as handed off on the last stage and as stopped by an operator on any other stage.

**`by_trigger` names.** A group is named by the trigger value the runs stored: `error`, `usage_limit`, `continuation`, `stall`, or `ci_fix`, as in the `trigger` of a run row. `<none>` names the runs whose trigger is the empty string: runs dispatched from the poll tick, runs a reaction other than the CI fix started, and recorded CI results. `<unknown>` names the runs whose row records no trigger: runs recorded before Sortie kept the trigger, and runs dispatched by a retry that never stored what started it. The groups partition the counted runs, so their `runs` sum to `summary.runs`. `by_trigger` carries no other names, and is `[]` on a database that lacks the trigger column, which the first `warnings` entry then states.

**`by_origin` names.** A group is named by what started its runs, and the first rule that matches decides a run's group.

| Name | Runs it holds |
| ---- | ------------- |
| `<unknown>` | Runs whose row records no trigger: runs recorded before Sortie kept what started a run, and runs dispatched by a retry that never stored what started it. |
| `error`, `usage_limit`, `continuation`, `stall`, `ci_fix` | Runs a retry dispatched, named by what queued the retry: a failed run, a run that a provider usage limit refused, a run that ended with its issue still active, a stalled run, or a CI failure (the CI fix). |
| `review`, `bot-review`, `merge-conflict`, `label-review`, `label-fix` | Runs a pull request reaction dispatched: a review fix, a bot review fix, a rebase after a merge conflict, a label review, or a label fix. The name is the `reaction_kind` of a run row. |
| `<none>` | Recorded CI results, which no dispatch started. |
| `stage_hop` | The first run dispatched after a stage hop, into the stage the hop led to. A later run in that stage does not count. |
| `poll_tick` | Every other run the poll tick dispatched, including a later dispatch into a stage a hop led to, as after a restart, a released hold, or a lost retry. |

The groups partition the counted runs, so their `runs` sum to `summary.runs`. A group named by a retry trigger holds the runs of the `by_trigger` group of that name. `by_origin` carries no other names, and is `[]` on a database an older Sortie last wrote, which the first `warnings` entry then states.

**`skill_calls` fields:** `unrecorded_runs`, the counted runs whose row records no skill calls: a run recorded before Sortie kept them, or one whose calls could not be stored; and `by_skill`, one entry per distinct skill name among the other counted runs, by descending `runs` and then ascending `skill` in byte order, `[]` when none of them called a skill or no run is in range. An entry holds `skill`, the name exactly as recorded, with no trimming or case folding; `runs`, the runs that called it at least once; `calls`, every call, repeats included; and `failed_calls`, the calls that failed. `failed_calls` never exceeds `calls`, and `runs` never exceeds `calls` or `summary.runs`. A run that called no skill adds nothing to `unrecorded_runs` or `by_skill`, and a run that called several skills counts in each of their entries, so the `runs` of the entries can add up to more than `summary.runs`. While `unrecorded_runs` is above `0`, read every count as a lower bound. The skill names are the ones stored with each run, masked when they were recorded. The text form of `sortie stats` does not show `skill_calls`.

**`self_review` fields:** `runs_with_metadata`, the runs that carry a self-review record; `by_final_verdict`, a list of `verdict` and `runs` pairs in ascending verdict order, where the verdict is `pass`, `iterate`, or `none` for a record with an empty verdict; `cap_reached_runs`, the runs whose self-review stopped at its iteration cap; `mean_iterations`, `null` when `runs_with_metadata` is `0`; and `unparsed_runs`, the records that could not be read. With no run in range the full tier still serves the object, with zero counts and `mean_iterations` `null`.

**`null` versus `0`.** A count is `0` when nothing matches. A figure that needs a measurement is `null` when no run supplies one: a run that reported no usage adds nothing to `tokens`, `cost_usd`, or their per-run figures, and a run recorded `unpriced` adds nothing to `cost_usd`. Neither is reported as a zero.

Every cost is the cost recorded with each run when it finished ([Section 4.4](#44-token_rates--cost-estimation)). The report parses no rates and prices nothing, so `sortie stats` and `GET /api/v1/stats` report the same cost for the same range, and a later change of rates moves no figure.

`sortie stats` loads the dispatch rules of the workflow to build the funnels, so it stops with the diagnostics `sortie validate` prints, and exits `1`, when they do not load, for example when a rule's prompt template file is missing. The route reads the dispatch rules in force on each request, so it follows a reload of the workflow.

The report holds no issue titles, error text, or agent output, so the route masks nothing in it. Its strings are names and identifiers, including the rule, template, and model names your workflow defines. The host paths in `workflow_path` and `db_path`, and any in the `by_template` names, are served as the command prints them.

**Errors:**

| Condition | Status | `error.code` |
| --------- | ------ | ------------ |
| A parameter other than `since` and `until`, a parameter given more than once, a value that is not a range bound, or `since` not before `until` | 400 | `invalid_parameter`; the message names the parameter and the rule, for example `parameter "since" must be before until`. No run is read. |
| The read fails, or the server has no run history to read | 503 | `history_unavailable` |
| A method other than `GET` | 405 | `method_not_allowed` |

A client that disconnects during the read gets no response, and the read stops.

Each request opens its own read-only connection to the database and closes it when the report is built. It never uses the connection the orchestrator holds, so a report over a long history does not delay the orchestrator's writes.

#### `GET /api/v1/prices`: prices in force

Returns the rates in force when the request is served, per agent kind and model, the layer each rate comes from, and the state of every price source. It is the data behind the dashboard's Prices screen. The route states rates and prices no run: a recorded run keeps the price it was written with ([Section 4.4](#44-token_rates--cost-estimation)). It takes no query parameters, writes nothing, and answers the same for the same configuration.

The route reads the rates the workflow loader holds, so a successful workflow load, a `token_rates` edit, a table that has just landed, and a failed download try show on the next request, without a restart. Times are RFC 3339 in UTC. A time that is unknown, a string that is empty, and the model count of a source with no table are `null`.

```json
{
  "generated_at": "2026-02-24T20:15:30Z",
  "table_in_force": {"layer": "fetched", "as_of": "2026-02-24T04:00:12Z", "age_days": 0},
  "token_rates": {"kind_entries": 0, "model_entries": 1, "version": "sha256:3b1f0c8d5a7e2f94b6c1d8e0a3f5b7c9d2e4f6a8b0c1d3e5f7a9b1c3d5e7f9a1"},
  "operator": null,
  "fetched": {
    "status": "loaded",
    "in_force": true,
    "off_by": null,
    "file": null,
    "address": "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json",
    "version": "sha256:8d2a6c4e0b1f3a5c7e9d1b3f5a7c9e0b2d4f6a8c0e1b3d5f7a9c1e3b5d7f9a0c",
    "as_of": "2026-02-24T04:00:12Z",
    "models": 2315,
    "next_try_at": "2026-02-25T04:00:12Z",
    "failures": 0,
    "failed_at": null,
    "error": null
  },
  "embedded": {
    "status": "loaded",
    "in_force": true,
    "off_by": null,
    "file": null,
    "address": null,
    "version": "9f3c2e1a7b4d5c6e8f0a1b2c3d4e5f6a7b8c9d0e",
    "as_of": "2026-02-20T11:42:07Z",
    "models": 2290,
    "next_try_at": null,
    "failures": 0,
    "failed_at": null,
    "error": null
  },
  "models": [
    {
      "agent_kind": "claude-code",
      "model": "claude-sonnet-4-5",
      "table_key": "claude-sonnet-4-5",
      "layer": "model_entry",
      "entry_key": "claude-sonnet-4-5",
      "input": {"per_mtok": 2.7, "layer": "model_entry", "from_input": false},
      "output": {"per_mtok": 15, "layer": "fetched", "from_input": false},
      "cache_read": {"per_mtok": 0.3, "layer": "fetched", "from_input": false},
      "cache_write": {"per_mtok": 3.75, "layer": "fetched", "from_input": false},
      "cost_status": "estimated",
      "unrated_classes": [],
      "last_used_at": "2026-02-24T19:58:41Z",
      "runs": 12
    },
    {
      "agent_kind": "codex",
      "model": "acme-1",
      "table_key": null,
      "layer": null,
      "entry_key": null,
      "input": {"per_mtok": null, "layer": null, "from_input": false},
      "output": {"per_mtok": null, "layer": null, "from_input": false},
      "cache_read": {"per_mtok": null, "layer": null, "from_input": false},
      "cache_write": {"per_mtok": null, "layer": null, "from_input": false},
      "cost_status": "unpriced",
      "unrated_classes": ["input", "output", "cache_read", "cache_write"],
      "last_used_at": null,
      "runs": 0
    }
  ]
}
```

**Top-level members.**

| Member | Type | Meaning |
| ------ | ---- | ------- |
| `generated_at` | string | The instant of the request. |
| `table_in_force` | object or `null` | The highest-precedence price table in force: `layer` (`operator_table`, `fetched`, or `embedded`), `as_of` (the date of its prices), and `age_days` (whole days from `as_of` to `generated_at`, never below `0`). `null` while no price table is in force. A model takes its rates from the first table that lists it, so a lower table can price a name this one lacks. |
| `token_rates` | object | `kind_entries` and `model_entries`, the number of `token_rates` entries in force, and `version`, their version, `null` when there are none. |
| `operator` | object or `null` | The state of your own table, in the source shape below. `null` unless `price_table.replace_file` or `price_table.replace_url` is set. |
| `fetched` | object | The state of the shared table downloaded from `price_table.fetch_url`. |
| `embedded` | object | The state of the table built into the binary. |
| `models` | array | One row per agent kind and model, below. `[]` when there are none. |

**A price source** (`operator`, `fetched`, and `embedded`):

| Member | Type | Meaning |
| ------ | ---- | ------- |
| `status` | string | `off`, `not_loaded`, `loaded`, or `failing`. `off` is the shared download turned off, or `price_table.embedded` set to `false`; it is decided from the settings in force before any download state is read, so an `off` source is never `failing`. Otherwise `failing` while the source has failed tries or its latest workflow load failed on `price_table.replace_file`, else `loaded` once it holds a table, else `not_loaded`. |
| `in_force` | boolean | The table is one of the tables that price runs. A `price_table.replace_file` table replaces the downloaded and embedded tables. A `price_table.replace_url` table replaces them once a download of it has passed validation, and they price until then. |
| `off_by` | string or `null` | On `fetched` while `off`: `SORTIE_PRICE_TABLE_FETCH` when the environment variable turned the download off, else `price_table.fetch`. `null` otherwise. |
| `file` | string or `null` | On `operator` for `price_table.replace_file`: the setting value, after `$VAR` resolution and before `~` expansion and path joining. It is never a path Sortie resolved. |
| `address` | string or `null` | The download address as its scheme, host, and path only, so a credential or query in the setting never leaves Sortie. `null` when the source has no address or the setting does not parse; a download whose address does not parse reads `failing` with the error `invalid address`. |
| `version` | string or `null` | The version of the table the source holds: the pinned upstream revision of the embedded table, a content digest of any other table. `null` when it holds none. |
| `as_of` | string or `null` | The date of the table's prices. For the embedded table, the commit date of its upstream revision. For a downloaded table, the time of the download, and a kept copy returns after a restart with its original download time. For a `price_table.replace_file` table, when this process first loaded the file's current content; a reload that reads unchanged content keeps it. |
| `models` | integer or `null` | The number of models the table lists. |
| `next_try_at` | string or `null` | When the next download try is due. `null` while a try is due or running, and for a source that is not downloaded. |
| `failures` | integer | The consecutive failed download tries since the last accepted one; `0` otherwise. |
| `failed_at` | string or `null` | When the latest failed try or failed load ended. `null` unless the source is `failing`. |
| `error` | string or `null` | The error of the latest failed try or failed load, masked as the Health screen masks the error of the workflow check. `null` unless the source is `failing`. |

The download state is kept in memory: a restart keeps the copy of a table that passed validation, with its download time and version, and starts the failure count again at `0`. A `price_table.replace_file` failure shows only while the configuration in effect names `price_table.replace_file`. A workflow load that fails for another reason shows on the Health and Workflow screens, not here. The `fetched` source carries no table facts while it is `off`, because a kept copy never prices while the download is off. The `embedded` source carries the binary's revision, its date, and its model count whether it is on or off.

**A model row.** Rows with a recorded run come first, the latest use first, then rows that only the workflow in effect names, which have `last_used_at` `null` and `runs` `0`. Ties break by `agent_kind` and then `model`, in byte order. A pair is listed when run history records it or when a setting of the workflow in effect names a model for an agent kind: the top-level `agent.kind`, the dispatch default, and each dispatch rule's settings.

| Member | Type | Meaning |
| ------ | ---- | ------- |
| `agent_kind` | string | The agent kind. The same model name under two kinds is two rows, because a `token_rates` entry keyed by the kind prices the kind's models first. |
| `model` | string | The model name, possibly empty for a run that reported none. |
| `table_key` | string or `null` | The key the model name maps to in the first price table that lists it. `null` when none does. |
| `layer` | string or `null` | The layer that prices the model: `kind_entry`, `model_entry`, `operator_table`, `fetched`, or `embedded`. `null` when none does. A model entry that takes some rates from a table is `model_entry`, and each class names its own layer. The layer is for display: a recorded run keeps its `operator`, `fetched`, or `embedded` price source ([Section 4.4](#44-token_rates--cost-estimation)), which does not tell your `token_rates` from your own table. |
| `entry_key` | string or `null` | The `token_rates` key of the kind entry or model entry. `null` otherwise. |
| `input`, `output`, `cache_read`, `cache_write` | object | `per_mtok`, the rate in USD per 1M tokens; `layer`, the layer that supplies it; and `from_input`. `per_mtok` and `layer` are `null` when no layer rates the class, so an unset rate never reads as zero, and `per_mtok` is `0` when a layer sets a rate of zero. A table rate is served to 10 significant digits. `from_input` is `true` for the cache rate of a kind entry that leaves it unset and so prices it at its input rate. |
| `cost_status` | string | What a run of the model records under these rates: `estimated` when every token class is priced, `partial` when a class has no rate beside priced ones, `unpriced` when no class is priced. |
| `unrated_classes` | array | The classes with no rate, from `input`, `output`, `cache_read`, and `cache_write`. `[]` when none. |
| `last_used_at` | string or `null` | The completion time of the latest recorded run. `null` for a model that only the workflow names. |
| `runs` | integer | The recorded runs that used the model under the agent kind. |

**Errors:**

| Condition | Status | `error.code` |
| --------- | ------ | ------------ |
| The server has no run history to read, or the read fails | 503 | `history_unavailable` |
| A method other than `GET` | 405 | `method_not_allowed` |

A server that holds no rates answers `200` with every row `unpriced` and the downloaded and embedded tables `off`.

#### `POST /api/v1/refresh`

Triggers an immediate poll+reconciliation cycle. The endpoint is best-effort: repeated requests while a refresh is already pending are coalesced. During graceful shutdown the endpoint rejects requests with `409 Conflict`.

**Normal response** — refresh signal accepted:

```http
HTTP/1.1 202 Accepted
{"queued": true, "coalesced": false, "requested_at": "...", "operations": ["poll", "reconcile"], "outcome_text": "Tracker poll and reconciliation queued."}
```

**Coalesced response** — a refresh was already pending:

```http
HTTP/1.1 202 Accepted
{"queued": true, "coalesced": true, "requested_at": "...", "operations": ["poll", "reconcile"], "outcome_text": "A tracker poll and reconciliation was already queued; this request joined it."}
```

**Drain rejection** — orchestrator is shutting down:

```http
HTTP/1.1 409 Conflict
{"queued": false, "coalesced": false, "requested_at": "...", "operations": [], "outcome_text": "Nothing queued: Sortie is shutting down."}
```

Callers should check the `queued` field or HTTP status to determine whether the refresh will be processed. A `409` response indicates the server is draining and the caller should retry against another instance or wait for the restart.

Every response carries `outcome_text`, one sentence that states what the response confirms: a poll and reconciliation that was queued, one that was already queued and that this request joined, or that nothing was queued. It never states an effect still to come.

**Kubernetes probe configuration.** Sortie's graceful shutdown cancels running agents before closing the HTTP listener. Configure `terminationGracePeriodSeconds` and liveness probe tolerance to exceed the expected drain duration (default: 30 seconds):

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

#### `POST /api/v1/retries/{identifier}/run`

Makes the scheduled retry of an issue due now, so the retry runs without waiting out its delay. `{identifier}` is the `issue_identifier` of the retry row, one path segment that a client percent-encodes, and it is matched exactly. The endpoint ignores the request body and the query string.

The retry then takes the path of a retry whose time came: it passes the same checks, among them the limit on agents running at once, the session and token budgets, and the issue's tracker state, and a retry the issue's state pauses stays paused. The one check it skips is a usage-limit hold: a retry of an issue that a provider usage limit refused starts at once even before the reset, which makes the request the way to test whether the limit has reopened. A retry that still cannot start keeps the attempt number and the due time it had before the request, so asking for it early never makes it wait longer. A retry whose row Sortie stores has the row saved with the new due time, so a restart before the retry runs does not undo the request.

The response arrives once Sortie has taken the request: at once while it is idle, and at the end of the work in progress otherwise. The wait has no fixed bound. A client that disconnects while it waits, or a shutdown that begins, withdraws the request and changes nothing.

**Normal response**: the retry is due now:

```http
HTTP/1.1 202 Accepted
{"queued": true, "requested_at": "2026-02-24T20:15:30Z", "issue_id": "def456", "issue_identifier": "MT-650", "attempt": 3, "outcome_text": "Retry 3 of MT-650 is due now."}
```

**Unknown retry**: no retry is scheduled for the identifier, because the issue is unknown, has no queued retry, or its retry ran or was released before Sortie took the request:

```http
HTTP/1.1 404 Not Found
{"error": {"code": "retry_not_found", "message": "no retry is scheduled for issue \"MT-999\""}}
```

**Drain rejection**: Sortie is shutting down:

```http
HTTP/1.1 409 Conflict
{"queued": false, "requested_at": "...", "outcome_text": "Nothing queued: Sortie is shutting down."}
```

A `202` also carries `display_identifier` when the issue has one. `issue_id`, `issue_identifier`, `display_identifier`, and `attempt` are the facts `outcome_text` is built from. Every `202` and `409` response carries `outcome_text`, one sentence that states what the response confirms: a retry due now, or that nothing was queued. It never states an effect still to come, because whether the retry starts is decided after the response.

> **Important:** Sortie's HTTP server has no authentication, so anyone who can reach it can run a retry early. A deployment that others can reach must sit behind a reverse proxy with authentication or a VPN.

#### `POST /api/v1/attention/{id}/acknowledge`

Records that a person has seen an item that needs a person. `{id}` is the `id` of an element of `attention` on `GET /api/v1/state`, one path segment, and it is matched exactly against the items Sortie derives when it takes the request. The endpoint ignores the request body and the query string. Every item can be acknowledged, whatever its `action`.

The item leaves `counts.attention` and stays in the list with `acknowledged_at` set, until its cause clears. Sortie saves the acknowledgement before it answers, so a `200` survives a restart. A different reason on the issue, or the same reason returning after it cleared, shows as a new item with `acknowledged_at` set to `null`; the Attention block of `GET /api/v1/state` states how long an acknowledgement lasts. A repeat request changes nothing: it answers `200` with `already_acknowledged` true and the time of the first acknowledgement.

The request changes nothing in the tracker or the pull request, and nothing about the park, hold, escalation, or retry behind the item. Acknowledging a park releases nothing, acknowledging a hold lifts nothing, and acknowledging an escalation ends nothing.

The response arrives once Sortie has taken the request: at once while it is idle, and at the end of the work in progress otherwise. The wait has no fixed bound. A client that disconnects while it waits withdraws the request and changes nothing, and so does a shutdown that begins, which answers `409`.

**Normal response**: the item is acknowledged now:

```http
HTTP/1.1 200 OK
{
  "acknowledged": true,
  "already_acknowledged": false,
  "requested_at": "2026-02-24T20:13:05Z",
  "id": "464f9ff8ea54c4c6cfb43af394234d57",
  "kind": "budget_held",
  "issue_id": "ghi789",
  "issue_identifier": "MT-651",
  "acknowledged_at": "2026-02-24T20:13:05Z",
  "outcome_text": "Acknowledged MT-651 (Session budget reached). It comes back only if a new cause appears."
}
```

**Repeat response**: the item was acknowledged before, and `acknowledged_at` is the first time:

```http
HTTP/1.1 200 OK
{"acknowledged": true, "already_acknowledged": true, "requested_at": "2026-02-24T20:15:30Z", "id": "464f9ff8ea54c4c6cfb43af394234d57", "kind": "budget_held", "issue_id": "ghi789", "issue_identifier": "MT-651", "acknowledged_at": "2026-02-24T20:13:05Z", "outcome_text": "Already acknowledged MT-651 (Session budget reached). It comes back only if a new cause appears."}
```

**Unknown item**: no current item has the ID, because the ID is unknown or the item cleared before Sortie took the request. The client refetches `GET /api/v1/state`, where the item is gone:

```http
HTTP/1.1 404 Not Found
{"error": {"code": "attention_item_not_found", "message": "no current attention item has ID \"0f1e2d3c4b5a69788796a5b4c3d2e1f0\""}}
```

**Drain rejection**: Sortie is shutting down:

```http
HTTP/1.1 409 Conflict
{"acknowledged": false, "already_acknowledged": false, "requested_at": "...", "outcome_text": "Nothing acknowledged: Sortie is shutting down."}
```

A `200` also carries `display_identifier` when the item concerns an issue that has one. The issue keys are omitted when empty and absent for an item about the process, such as a failed load of `WORKFLOW.md`. `id`, `kind`, and the issue keys are the facts `outcome_text` is built from. Every `200` and `409` response carries `outcome_text`, one sentence that states what the response confirms: that the item is acknowledged, that it already was, or that nothing was acknowledged.

When Sortie cannot save the acknowledgement, it answers `500` with the error code `acknowledgement_not_saved` and the message `Sortie could not save the acknowledgement; nothing changed`. The item stays unacknowledged, and the client can acknowledge it again.

> **Important:** Sortie's HTTP server has no authentication, so anyone who can reach it can acknowledge an attention item and take it out of the attention count. A deployment that others can reach must sit behind a reverse proxy with authentication or a VPN.

#### `POST /api/v1/reactions/{identifier}/{kind}/check`

Makes the next check of one pending pull request reaction due now, so the reaction checks its pull request on the next poll tick instead of at its scheduled time. `{identifier}` is the `issue_identifier` and `{kind}` is the `kind` of a row of `pending_reactions`, each one path segment that a client percent-encodes, and each is matched exactly. The route takes the kind and not `config_key`, because `label-review` and `label-fix` share one key. The endpoint ignores the request body and the query string.

The request moves `next_poll_at` of the row to the time Sortie takes the request when that is later, and changes nothing else about the reaction: `wait`, the tries used, any standing escalation, and `last_check` stay as they were. A reaction that is already due is left as it is, and the request still answers `202`. The request makes no call to the tracker, the forge, or a CI provider, starts no agent, schedules no retry, and writes nothing to the tracker or the pull request. It stores nothing, because pending reactions are runtime-only.

The check runs on the next poll tick, or on a refresh before it. The route runs no tick and dispatches no new issues. The check follows every rule of the reaction's kind: the review debounce, the retry slot, triage, the limit on tries and the escalation at it, the watch window, and the budgets that apply to a fix the check schedules. What the check finds is handled as it is at the reaction's due time.

When the check keeps the reaction pending and started before the time the request replaced, `next_poll_at` goes back to that time, and the backoff count goes back to the lower of the count the check left and the count before the request, in place of the ones the check sets. After a new pull request head the count is zero. A check that starts at or after the replaced time is one the reaction had without the request, and its own schedule stands. A request never leaves a reaction's next check later, or its backoff count higher, than without the request.

The response arrives once Sortie has taken the request: at once while it is idle, and at the end of the work in progress otherwise. The wait has no fixed bound. A client that disconnects while it waits withdraws the request and changes nothing, and so does a shutdown that begins, which answers `409`. A request that arrives while a tick checks the reaction applies when that tick ends and moves the check the tick scheduled, so the reaction is checked once more on the next tick. Further requests before that tick change nothing. The answer is `202 Accepted` and not `200 OK`, because the check runs after the response.

**Normal response**: the check is queued for the next poll tick:

```http
HTTP/1.1 202 Accepted
{
  "queued": true,
  "requested_at": "2026-02-24T20:15:30Z",
  "issue_id": "mno345",
  "issue_identifier": "MT-653",
  "kind": "ci",
  "pull_request": {"number": 931, "repository": "acme/web"},
  "outcome_text": "MT-653: a check of PR #931 for CI is queued for the next poll tick."
}
```

**Unknown reaction**: the issue has no pending reaction of that kind, because the issue is unknown, the kind is not a reaction kind, the issue has no reaction of it, or the reaction left the pending set before Sortie took the request (its watch ended, or its check dispatched a fix). The client refetches `GET /api/v1/state`, which shows what became of the reaction:

```http
HTTP/1.1 404 Not Found
{"error": {"code": "reaction_not_found", "message": "no pending \"bot-review\" reaction for issue \"MT-653\""}}
```

**Drain rejection**: Sortie is shutting down:

```http
HTTP/1.1 409 Conflict
{"queued": false, "requested_at": "...", "outcome_text": "Nothing queued: Sortie is shutting down."}
```

A `202` also carries `display_identifier` when the issue has one. `issue_id`, `issue_identifier`, `display_identifier`, `kind`, and `pull_request` are the facts `outcome_text` is built from, and `pull_request` carries `number` and `repository` only. Every `202` and `409` response carries `outcome_text`, one sentence that states what the response confirms: a check queued for the next poll tick, or that nothing was queued. It never states what the check finds or does, because that is decided after the response.

> **Important:** Sortie's HTTP server has no authentication, so anyone who can reach it can make a pull request reaction check early. A deployment that others can reach must sit behind a reverse proxy with authentication or a VPN.

#### `GET /api/v1/events`: change signals and agent events

Streams a signal each time anything the JSON API serves changes, so a client refetches what it shows instead of polling, and streams the agent events of the runs the client names with `run`. The response is a server-sent event stream (`text/event-stream`) that stays open until the client leaves or Sortie shuts down. A change signal carries no data of its own: it names no issue and no agent message, and the stream shows nothing the JSON API does not. A method other than `GET` answers `405`.

> **Important:** Sortie's HTTP server has no authentication, so anyone who can reach it can read the agent events of every retained run. A deployment that others can reach must sit behind a reverse proxy with authentication or a VPN.

**The `run` parameter.** Repeat `run=<dispatch_id>` or `run=<dispatch_id>:<seq>` once for each run the client shows, up to 16 per connection. The `dispatch_id` is the one the JSON API reports for a running row or a run. Without a `seq` the stream starts with every retained event of the run; with one, it starts after that `seq`. A `seq` above the run's newest `seq` reads as that `seq`. Every other query parameter is ignored. The cursor is a query parameter because `EventSource` cannot set a header on its first connection.

A dispatch ID has 1 to 64 characters, each from `A` to `Z` or `2` to `7`. A `seq` has 1 to 19 digits and fits a signed 64-bit integer. An empty value is invalid.

```bash
curl -N 'http://127.0.0.1:7678/api/v1/events?run=RLQFSIOFERWGDW2HZY45W55VXK:42'
```

**Errors:**

| Condition | Status | `error.code` |
| --------- | ------ | ------------ |
| A `run` value that is not a dispatch ID optionally followed by `:` and a `seq`, a dispatch ID given twice, or more than 16 `run` values | 400 | `invalid_parameter`; the message names the broken rule and echoes no input |
| A method other than `GET` | 405 | `method_not_allowed`, with `Allow: GET` |

A `400` opens no stream. A malformed `Last-Event-ID` never causes one.

A change signal is one frame:

```
id: 1792000000000000000
event: change
data: {}

```

The revision part of the `id` is a counter that rises with every change. A client keeps the last `id` it saw for the reconnect rule below.

**The `id` of a frame.** Every frame, a change signal, a heartbeat, and each run frame, carries one `id`: the revision, followed by `;<dispatch_id>:<seq>` for each subscribed run in the order of the `run` parameters. The `seq` is the run's cursor as the stream holds it: the `seq` of the last event of the run the stream wrote, or the cursor the subscription started from. A run frame never advances the revision, so the revision part is that of the last change signal on the stream, or the one the stream opened with when it has sent none. A stream with no `run` carries the revision alone.

```
id: 1792000000000000000;RLQFSIOFERWGDW2HZY45W55VXK:43
event: agent_event
data: {"dispatch_id":"RLQFSIOFERWGDW2HZY45W55VXK","seq":43,"at":"2026-02-24T20:14:59Z","event":"notification","message":"Working on tests","text":"Working on tests"}

```

**Agent events.** A subscribed run gets these frames beside the change signals:

| Event | Data | When |
| ----- | ---- | ---- |
| `agent_event` | `dispatch_id` and the members of the [event shape](#get-apiv1runsdispatch_idevents-run-events) | One for each retained event after the cursor, then one for each new event as Sortie applies it, in `seq` order |
| `run_ended` | `dispatch_id` and `last_seq`, the `seq` of the run's newest retained event | After the run's last event, once the run has ended |
| `reset` | `dispatch_id` and `reason` `cursor_expired`, with `first_seq` | The cursor is behind retention: the events between the cursor and `first_seq` were evicted. Every retained event of the run follows in the same stream, and the `id` of the reset carries `first_seq` minus one as the run's cursor |
| `reset` | `dispatch_id` and `reason` `not_retained` | The run's events are not retained, by the rule of the run events route's `404`. The run leaves the `id` and no other frame of it follows |

```
id: 1792000000000000000;RLQFSIOFERWGDW2HZY45W55VXK:43
event: run_ended
data: {"dispatch_id":"RLQFSIOFERWGDW2HZY45W55VXK","last_seq":43}

id: 1792000000000000000;RLQFSIOFERWGDW2HZY45W55VXK:11
event: reset
data: {"dispatch_id":"RLQFSIOFERWGDW2HZY45W55VXK","reason":"cursor_expired","first_seq":12}

id: 1792000000000000000
event: reset
data: {"dispatch_id":"RLQFSIOFERWGDW2HZY45W55VXK","reason":"not_retained"}

```

A client keeps the events it holds after a `cursor_expired` reset and marks the gap; it fetches nothing. A subscription without a cursor to a run whose oldest retained `seq` is above 1 starts with that reset. After `run_ended` the run keeps its cursor in the `id`, so a reconnect with it gets `run_ended` again and no event twice; after `not_retained` a reconnect gets `not_retained` again. Run frames are never coalesced and are not bound by the one signal a second: they go out as soon as the event is applied, in `seq` order within a run. When Sortie cannot read the stored events of a finished run, it writes nothing for that run, reads them again at the next heartbeat, and keeps the other runs and the change signals going; the run events route answers that run `503`.

**When a signal arrives.** A signal follows every tick, refresh, worker exit, and retry, every retry made due on request, every reaction check made due on request, every acknowledgement recorded on request, every message from a running agent session (so a session's token counts and latest message reach a client within a second), the start of shutdown, and every load of WORKFLOW.md, whether the load succeeds or fails. A stream carries at most one change signal per second. The first change after a quiet second arrives at once; changes made during a second arrive together as one signal when it ends. Values that move with the clock alone change without a signal: `agent_totals.seconds_running`, elapsed times computed from `started_at`, and the window of a relative `since` on `GET /api/v1/stats`.

**Reconnect.** A client reconnecting sends the `id` of its last frame in the `Last-Event-ID` header; a browser's `EventSource` does this on its own. When the text before the first `;` equals the current counter exactly, Sortie sends no change signal until the next change. Any other value, an absent header, or a value from before a restart gets one change signal at once, so the client refetches. For each run the `run` parameters name, a cursor in the header replaces the cursor of the query, so the stream resumes after the last event the client received. The query alone decides which runs are subscribed. A header part that is not `<dispatch_id>:<seq>` is ignored, and the first part for a run counts.

**Heartbeat.** Every 15 seconds Sortie writes a heartbeat frame so that a proxy does not close an idle stream:

```
id: 1792000000000000000
event: heartbeat
data: {}

```

The heartbeat carries the revision part of the last change signal on the stream, or of the `Last-Event-ID` the stream opened with when it has sent none, followed by the cursor of every subscribed run. A browser delivers it only to `heartbeat` listeners, not to `message` or `change` listeners.

**Reconnecting.** When a stream closes for good (`EventSource.readyState` is `CLOSED` after an `error` event, as a non-`200` answer or a wrong `Content-Type` leaves it), open a new one after a delay that doubles from 1 second up to 30 seconds while new streams keep failing, and go back to 1 second once one opens. A new stream carries no `Last-Event-ID`, so its opening change signal makes the client refetch, and the client writes the last `seq` it holds of each run into that run's `run` parameter, so no event is replayed twice or missed.

**Idle timeout.** When an open stream delivers no event, change signal or heartbeat, for 45 seconds, close it and open a new one the same way. This recovers a stream that died without the browser being told, for example after a laptop wakes holding a connection Sortie dropped while it slept.

**Shutdown.** When shutdown begins, Sortie ends every open stream, also one in the middle of writing agent events, before its next frame, so a connected client does not delay it. A stream opened during shutdown answers `200` and ends at once. The route never answers `503`, because a browser does not reconnect after a non-`200` answer. Behind a reverse proxy, a client reconnecting while Sortie restarts gets the proxy's `502`, which closes its stream; the client's reconnect with backoff reopens it.

**Slow clients.** The 10 second write timeout of the HTTP server does not end a stream. Each frame has its own write deadline of 2 seconds. A client that stops reading is dropped once Sortie cannot write to it within 2 seconds, and it delays no other client and not the agent. A connection that keeps reading is never dropped for the length of a catch-up, however long the retained events take to send. A dropped client reconnects with the `id` of its last frame.

**Browser connection limit.** Over plain HTTP a browser allows six connections per origin across all its tabs, and every open stream holds one. Open one stream per browser and share it across tabs instead of one stream per tab.

**Reverse proxies.** Sortie sends `X-Accel-Buffering: no`, which nginx honors, so the stream is not buffered. Set the proxy's read timeout above 15 seconds; nginx's `proxy_read_timeout` defaults to 60 seconds.

#### `GET /metrics` — Prometheus Scrape Endpoint

When the HTTP server is enabled, Sortie exposes a Prometheus exposition-format endpoint at `/metrics` for integration with Prometheus, Grafana, and other monitoring stacks. The endpoint is co-located with the JSON API and dashboard on the same address and port — no separate configuration is required.

The endpoint uses a dedicated `prometheus.Registry` (not the Go default global) to prevent metric pollution. Standard Go runtime (`go_*`) and process (`process_*`) metrics are included alongside Sortie-specific metrics.

**Sortie-defined metrics:**

| Name                                            | Type      | Labels                      | Description                                                    |
| ----------------------------------------------- | --------- | --------------------------- | -------------------------------------------------------------- |
| `sortie_sessions_running`                       | Gauge     | —                           | Number of agent sessions currently executing.                  |
| `sortie_sessions_retrying`                      | Gauge     | —                           | Number of issues in the retry queue.                           |
| `sortie_slots_available`                        | Gauge     | —                           | Remaining dispatch capacity under current concurrency limits.  |
| `sortie_active_sessions_elapsed_seconds`        | Gauge     | —                           | Cumulative wall-clock elapsed time across running sessions.    |
| `sortie_tokens_total`                           | Counter   | `type`, `model`             | Tokens consumed, by type (`input`, `output`, `cache_read`, `cache_write`) and by model. `cache_read` and `cache_write` are subsets of `input`, so summing across every `type` double-counts them. A run that spends tokens on several models counts each model's share under that model. |
| `sortie_agent_runtime_seconds_total`            | Counter   | `model`                     | Cumulative agent-session wall-clock time for completed sessions, by model. Every model a settings block configures (`<none>` when a block configures none) and every model an earlier run reported or spent tokens on report `0` from start-up; a model none of these name gets its series when a run first reports it, so a query filtered to such a model returns no data before then and an alert on it uses `or vector(0)`. |
| `sortie_dispatches_total`                       | Counter   | `outcome`                   | Dispatch attempts (`success`, `error`).                        |
| `sortie_worker_exits_total`                     | Counter   | `exit_type`, `model`        | Worker exits (`normal`, `error`, `cancelled`, `soft_stop`), by model. |
| `sortie_retries_total`                          | Counter   | `trigger`                   | Retry schedule events (`error`, `usage_limit`, `continuation`, `timer`, `stall`, `ci_fix`). A review, bot review, merge conflict, or label command continuation records no trigger. |
| `sortie_reconciliation_actions_total`           | Counter   | `action`                    | Reconciliation outcomes (`stop`, `cleanup`, `keep`, `sweep_cleanup`, `sweep_expired`). |
| `sortie_poll_cycles_total`                      | Counter   | `result`                    | Poll tick completions (`success`, `error`, `skipped`).         |
| `sortie_tracker_requests_total`                 | Counter   | `operation`, `result`       | Tracker adapter API calls by operation and result.             |
| `sortie_handoff_transitions_total`              | Counter   | `result`                    | Handoff-state transition attempts (`success`, `error`, `skipped`, `withheld`). `skipped` covers three suppression causes: the issue had already reached a terminal state, the issue was not in an active state at worker exit, or a withheld verdict's own verification read reported the issue terminal, suppressing the absence-failure recording. `withheld` covers the remaining case: the run's handoff-evidence verdict did not permit the write and its own verification read did not find the issue terminal, so the run is recorded as failed. Recorded only when `tracker.handoff_state` is set. A made stage hop records no result, because it writes no handoff transition. |
| `sortie_issue_parks_total`                      | Counter   | `reason`                    | Issue park events (`agent_blocked`, `handoff_absence`).        |
| `sortie_tool_calls_total`                       | Counter   | `tool`, `result`            | Agent tool call completions by tool and result. `tool` is the name the runtime gives the tool, `{server}.{tool}` for an MCP or dynamic tool, the call's kind when the runtime names none, and `other` when the name holds anything but ASCII letters, digits, `_`, `-`, `.`, and `:`. A command is never a label. |
| `sortie_poll_duration_seconds`                  | Histogram | —                           | Wall-clock time per poll cycle.                                |
| `sortie_worker_duration_seconds`                | Histogram | `exit_type`                 | Worker session wall-clock time.                                |
| `sortie_build_info`                             | Gauge     | `version`, `go_version`     | Always `1`; carries build metadata as labels.                  |
| `sortie_ssh_host_usage`                         | Gauge     | `host`                      | Current session count per SSH host.                            |
| `sortie_budget_exhaustions_total`               | Counter   | `reason`                    | Issue entries into the per-issue budget-exhausted set (`money_budget`, `token_budget`, `session_budget`). |
| `sortie_budget_exhausted_issues`                | Gauge     | `reason`                    | Issues currently held out of dispatch by a per-issue budget ceiling.                     |

The `model` label is the model the agent reported, otherwise the configured `model`, otherwise `<none>`, the placeholder `sortie stats` uses for the same case. Token samples count as usage arrives, so usage recorded before a run's first model report counts under the configured model or `<none>` and is not moved when the model arrives, a run whose runtime switches model splits its tokens between the models, and when the agent reports how many tokens each model spent, each model's share of a sample counts under that model while usage the agent reports without a per-model count counts under the run's model. Runtime and exit samples are recorded when the session ends, so they carry the run's final model: the one the agent reported, else the configured `model`.

On a default Prometheus, `rate()` and `increase()` miss the first sample of a model Sortie has never run before. Enabling the Prometheus feature flag `--enable-feature=created-timestamp-zero-ingestion`, still experimental in Prometheus, closes that gap with no Sortie setting: `/metrics` sends each series' creation time in the protobuf format the flag makes Prometheus request, so a scrape job that sets `scrape_protocols` keeps `PrometheusProto` in the list. A configured model name that the agent reports under a different, resolved name keeps its series at its true total, usually a flat `0`, beside the series of the reported name. This is expected, as is a flat series for a model only earlier runs used.

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

When `logging.level` is set, Sortie initializes the log handler at the specified verbosity after the workflow config is loaded. The CLI `--log-level` flag takes precedence when both are present. Accepted values: `debug`, `info`, `warn`, `error` (case-insensitive). Unknown values cause startup failure with exit code 1.

### 4.3 `worker` — SSH Worker Extension

```yaml
worker:
  ssh_hosts:
    - build01.internal
    - build02.internal
  max_concurrent_agents_per_host: 2
  ssh_strict_host_key_checking: accept-new
  ssh_pass_env:
    - EXAMPLE_API_KEY
    - EXAMPLE_PROJECT
  ssh_disallow_pass_env:
    - GITHUB_TOKEN
```

When `worker.ssh_hosts` is configured, Sortie dispatches agent runs to remote hosts over SSH using the system `ssh` binary. Each dispatch selects the host with the fewest active sessions (least-loaded selection). When a per-host concurrency cap is set, hosts at capacity are skipped. On retry, the previous host is preferred if it still has capacity.

When `worker.ssh_hosts` is absent or empty, all agents run locally on the host where Sortie is started (the default behavior).

| Field                                   | Type             | Required | Default                        | Description                                                                                 |
| --------------------------------------- | ---------------- | -------- | ------------------------------ | ------------------------------------------------------------------------------------------- |
| `worker.ssh_hosts`                      | list of strings  | No       | _(absent — work runs locally)_ | SSH host targets for remote agent execution.                                                |
| `worker.max_concurrent_agents_per_host` | positive integer | No       | _(absent)_                     | Per-host concurrency cap shared across configured SSH hosts. Hosts at capacity are skipped. |
| `worker.ssh_strict_host_key_checking`   | string           | No       | `accept-new`                   | OpenSSH `StrictHostKeyChecking` value: `accept-new`, `yes`, or `no`.                       |
| `worker.ssh_pass_env`                   | list of strings  | No       | _(absent)_                     | Names of environment variables to carry from Sortie's own environment into a remote session. Written literally; an entry given as a `$VAR` reference is ignored. |
| `worker.ssh_disallow_pass_env`          | list of strings  | No       | _(absent)_                     | Names of environment variables Sortie never carries into a remote session. Written literally; an entry given as a `$VAR` reference is ignored. |

#### SSH Hook Environment

When SSH mode is active, all lifecycle hooks (`after_create`, `before_run`, `after_run`, `before_remove`) receive the `SORTIE_SSH_HOST` environment variable set to the target host for the current session. Hooks can use this variable to interact with the remote host — for example:

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

- **SSH connectivity is validated at dispatch time**, not at startup. Hosts that are temporarily unreachable cause the worker to fail and retry with exponential backoff.
- **Process lifecycle:** The agent does not have to stop itself when the SSH connection closes, whether Sortie ended it (cancellation, timeout, session stop, shutdown) or the network dropped it. The remote shell that launched the agent watches the connection once per second and, once it is gone, sends `SIGTERM` to the session's process group, waits up to `agent.stop_grace_ms` (rounded up to whole seconds), and sends `SIGKILL`. The agent still sees end-of-input on its standard input and may exit on it within that period. A process the agent moves to another process group is not reached.
- **SSH options:** Sortie sets `ServerAliveInterval=15`, `ServerAliveCountMax=3`, and `StrictHostKeyChecking=accept-new` by default, and sets `ControlPath=none` on every launch, so a launch never shares a connection that `ControlMaster` in the operator's `ssh_config` would keep open after the launch ends. The `StrictHostKeyChecking` value is configurable via `worker.ssh_strict_host_key_checking`. Set to `yes` when `known_hosts` is pre-populated by configuration management; set to `no` only in isolated test environments. Invalid values fall back to `accept-new` with a warning. A launch that delivers Sortie's tools adds `ExitOnForwardFailure=no`, `LogLevel=INFO`, and a remote forward of a loopback port the remote host allocates. Each session start also opens one extra connection, a probe that settles the forward before the first prompt. Operators should ensure SSH key-based authentication is configured for all target hosts.
- **Carrying environment variables to a remote agent:** `worker.ssh_pass_env` names environment variables, never values, that Sortie reads from its own process environment and sends to every remote agent launch. Each agent kind also carries a fixed set of credential variables on a remote launch, without being listed: `claude-code` carries `ANTHROPIC_AUTH_TOKEN`, `ANTHROPIC_API_KEY`, and `CLAUDE_CODE_OAUTH_TOKEN`; `copilot-cli` carries `COPILOT_GITHUB_TOKEN`, `GH_TOKEN`, and `GITHUB_TOKEN`. A workflow converted from a removed kind carries that kind's credential variables on a remote launch of the replacement kind in the same way, whether or not it lists them, and `worker.ssh_disallow_pass_env` still keeps a name off the wire. A variable carried this way, whether listed or carried automatically for the kind, overrides any value or login already present on the remote host. A listed name that is unset in Sortie's own environment, or whose value is empty or only whitespace, is skipped and logged as a warning; a name carried automatically for the kind is skipped silently in those same cases, unless `worker.ssh_pass_env` lists that name too, which restores the warning. `worker.ssh_disallow_pass_env` names variables Sortie never carries, whether they come from `worker.ssh_pass_env` or from the kind's own credential set; Sortie sends nothing of its own for a disallowed name, so the remote host's own value or login stays in effect. Both keys take variable names literally: an entry written as a `$VAR` reference arrives holding that variable's value rather than a name, so Sortie ignores the entry and warns with its position, never its value. Neither key affects a local (non-SSH) launch, and neither key affects a setting an agent kind computes for itself, including `SORTIE_TOOL_SERVER_BEARER` and the `SORTIE_MCP_HEADER_<key>` variables that carry the header values of an HTTP server listed under the agent's `mcp_config`. Sortie sets those two automatically on a launch that delivers its tools; an operator variable of the same name carried through `worker.ssh_pass_env` is overridden. A change to either key on reload takes effect for sessions dispatched after the reload; a session already running keeps the names it started with.
- **Keeping a remote host's own login:** an agent kind's automatically carried credential takes precedence over a login already stored on the remote host. For example, if Sortie's own environment sets `GITHUB_TOKEN` for another purpose (say, `tracker.api_key: $GITHUB_TOKEN`) while `copilot-cli` hosts are meant to sign in on their own, name `GITHUB_TOKEN` under `worker.ssh_disallow_pass_env` to keep the host's own login in effect.
- **What is exposed and how to avoid it:** a carried variable's value travels over the SSH connection's standard input, never as part of a command line, so it never appears in the argument list of a process on either the machine running Sortie or the remote host. The local `ssh` process still inherits Sortie's own environment, as every process Sortie starts does. On the remote host the value sits in the launched agent's own environment, readable by that account and by root, the same as it would be on a local launch. To keep Sortie's own value from reaching a remote host, name it under `worker.ssh_disallow_pass_env`; Sortie then sends nothing of its own for that name, and the host's own value or login stays in effect. This governs what Sortie sends, not what the SSH client sends on its own: a variable listed under `SendEnv` in your `ssh_config` that the remote server accepts under `AcceptEnv` is forwarded by `ssh` out of Sortie's environment, and Sortie cannot suppress that from the command line. Remove the `SendEnv` entry from your SSH configuration when a variable must not reach the host. The bearer and the header values never appear on a command line either. For `agent-client-protocol` they travel inside the session-creation request on the same connection. The bearer authorizes only its own session's tool server and stops working when that session ends.
- **Remote host requirements:** a remote host must offer a POSIX-compatible login shell and the standard `sleep` utility, which the remote shell runs while it supervises every remote launch, and it must have the standard `dd` utility installed for any launch that carries a variable, which includes every remote `opencode` launch regardless of configuration and every remote launch that delivers Sortie's tools. A host missing `dd` fails the launch, logs `sortie: dd is required on the remote host to receive environment variables`, and the failed launch is retried the same way any other failed remote launch is.
- **Remote port forwarding:** a launch that delivers Sortie's tools needs the remote host to allow it: `AllowTcpForwarding` set to `yes` or `remote`, no `DisableForwarding`, and no `restrict` or `no-port-forwarding` option on the key Sortie connects with. A host that does not allow it runs the session without Sortie's tools, and the session says so in its opening notice and in the log.

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
  claude-sonnet-4-5:
    input_per_mtok: 2.70
  my-local-model:
    input_per_mtok: 0.50
    output_per_mtok: 1.50
    cache_read_per_mtok: 0.05
    cache_write_per_mtok: 0.60
```

Sortie estimates the cost in USD of every measured run, per model, and shows it on the dashboard (an estimate on each run, each issue, and each breakdown of its History and Analytics screens, the cost of today's runs, a chart of the spend per UTC day or month on Analytics, and the cost per succeeded run in Analytics' summary and in each of its breakdowns that shows a cost), in the JSON API (each running row's session estimate and the estimate of its whole issue, `estimated_cost_usd` and `issue_estimated_cost_usd`, each with a status, and an estimate with a status on every run row and on the history totals of an issue, including each issue of run history by issue), and in `sortie stats` and `GET /api/v1/stats`. All rates are in USD per 1 million tokens.

**Where rates come from.** A run's tokens are split by model, and each model's share takes its rates from the first of these layers that has an entry for it:

1. The operator's rates in `token_rates`. A key that names a registered agent kind (for example `claude-code`) prices every model of that kind alike; any other key names a model, matched to the model a run reports, or the model it was configured with when it reports none, by the same name matching the price tables use.
2. The shared price table downloaded at runtime, unless `price_table.fetch` is `false`, once a downloaded table has passed validation.
3. The price table embedded in the binary: a snapshot of the published LiteLLM `model_prices_and_context_window.json`, refreshed from upstream regularly; each release embeds the copy its source holds, unless `price_table.embedded` is `false`. The snapshot carries its upstream revision and the date of that revision's commit, which the Prices screen shows with the age of the prices in force.

`price_table.replace_file` names a table of your own, in the same format as the shared table, that replaces layers 2 and 3 entirely. `price_table.replace_url` names one that replaces layers 2 and 3 once a download of it has passed validation, and layers 2 and 3 price until then. `token_rates` entries still lie over either. A deployment with no `token_rates` and no network access is priced from the embedded table. The Prices screen of the dashboard shows the rates in force for every model your runs used and every model your workflow configures, the layer that supplies each, and the state of every price source; [`GET /api/v1/prices`](#get-apiv1prices-prices-in-force) serves the same data.

**Matching model names.** Runtimes and tables spell the same model differently (a provider prefix such as `anthropic/claude-sonnet-4-5`, or a dotted version such as `claude-opus-4.6`), and one matching rule maps every runtime's names onto table keys. A name that maps to no key has no rate; matching never guesses, so `claude-sonnet-4` is never priced at the rates of `claude-sonnet-4-5`. A model no layer prices leaves its share unpriced, and one `token_rates` line for that model fixes it.

**Per-model entries.** A model entry prices the models whose name it matches: a name equal to its key; a name that maps to the same table entry as its key (`anthropic/claude-sonnet-4-5` takes the entry keyed `claude-sonnet-4-5`); or, for a model no table lists, a name equal to its key once letter case and a dot or hyphen between two digits are set aside (`My-Local-Model` takes `my-local-model`). When two entries match a name only by the second or third of these, neither prices it, and a name that is only a prefix of a key never matches. An entry for a model that a table in force lists may set any subset of the four rates and takes the rest from that table, so `claude-sonnet-4-5` above overrides only the input rate. An entry for a model no table lists should set every rate; one that does not draws the `token_rates.model_incomplete` warning and prices the classes it sets.

**Per-kind entries.** An entry keyed by an agent kind takes precedence over every table and over model entries, prices every model of its kind alike, and needs both `input_per_mtok` and `output_per_mtok` to price anything. An unset `cache_read_per_mtok` or `cache_write_per_mtok` in it prices that class at `input_per_mtok`, so a workflow that priced with per-kind entries sees the same figures as before. `sortie validate` warns that such an entry turns off per-model pricing for its kind.

**How a share is priced.** Fresh input, cache reads, cache writes, and output are each priced once at their own rate. Outside a per-kind entry, no rate is ever substituted: a token class a model spent with no rate contributes nothing and marks the run `partial`, a lower bound. A run whose spend no rate prices is `unpriced`. Outside a per-kind entry a run's cost is the sum of its shares' costs; a per-kind entry prices all of its kind's shares together on their summed tokens. The dashboard words a `partial` cost as a lower bound ("at least $4.20"; Analytics shows the amount cut down with a dotted underline and opens its hover with "At least this much") and a non-zero cost under one cent as "under $0.01", shows the exact amount of a cost on hover on every screen but Analytics, and lists a finished run's cost per model on its session view, naming the token classes that had no rate.

**The price is recorded with the run.** When a run finishes, Sortie records its cost and status, and for each model's share the price source (`operator` for `token_rates` and your own table, `fetched`, or `embedded`) and the version of the rates (the embedded table's pinned upstream revision, or a digest of the downloaded table, your table, or `token_rates`; a model entry that takes some of its rates from a table records the `token_rates` digest, a `+`, and that table's version). Every surface reads the recorded cost, so a later change of rates, a restart, an upgrade, or a newly downloaded table never moves a past figure, and `sortie stats` and `GET /api/v1/stats` report the same cost for the same range. A running session's figure is a live estimate under the rates in force, fixed when its run is recorded. Runs recorded before an upgrade that introduced recorded costs are priced once, at the first start after it, under the rates in force at that start.

**The runtime download.** On by default: a default install downloads the table from `price_table.fetch_url` at startup and every 24 hours. Set `price_table.fetch: false`, or `SORTIE_PRICE_TABLE_FETCH=false` in the environment, to turn it off, for example on an audited network; Sortie then prices from `token_rates` and the embedded table, and a shared table kept from an earlier run does not price. The shared download runs whenever `price_table.fetch` is on, whether or not you configure a table of your own. A table named by `price_table.replace_url` is downloaded the same way whenever it is set, whatever `price_table.fetch` says. A download, failed or not, never blocks startup, dispatch, or a reload, and until a downloaded table is in force the layers beneath it price runs. Every start tries each download at once, whatever the age of its kept copy. A download that fails is tried again after 1 minute, then after 2, 4, and so on up to 1 hour between tries, until one succeeds, so a table arrives within an hour of the network's return; a download that succeeds is repeated 24 hours later. A response that does not parse as a price table, holds a rate that is not a finite non-negative number, carries no rate at all, or carries rates in fewer than half as many entries as the last table kept from the same address is rejected, and a model whose price fell to zero against the last table kept from the same address (for the shared table, the embedded table until one is kept) keeps its previous price; your own table is never compared with the shared one. Each table that passes validation is kept as a file beside the database: the database path followed by `.price-table-shared.json` for the shared table and `.price-table-operator.json` for the table named by `price_table.replace_url`. The copy is loaded at startup, so a restart without network access keeps it, and it prices only while its download is configured with the address it came from. The request carries no run data and no configuration. `SORTIE_PRICE_TABLE_FETCH` overrides `price_table.fetch`, and like that setting it governs only the download of the shared table.

**An estimate, not a bill.** The four rates per model do not express long-context price tiers, the separate price of one-hour cache writes, batch, flex, and priority service tiers, data-residency surcharges, negotiated discounts, subscription plans, or fees for tools such as web search, so the figure can differ from what the provider charges. Sortie never uses a cost figure an agent runtime reports on its own.

When no rate source is available (the embedded table disabled, no downloaded table, no operator table, and no `token_rates`), the dashboard shows no cost, its session view states a session's cost as unpriced, every measured running row of the JSON API carries `cost_status` `unpriced`, and every finished run that spent tokens is recorded `unpriced`, so `sortie stats` and `GET /api/v1/stats` report no cost for it. A finished run that spent nothing is still recorded at `0`, and `estimated` unless it recorded an unaccounted turn.

The `config.token_rates` member of `GET /api/v1/state` serves the `token_rates` in force: an entry per key as written, each rate `null` when not set, `complete`, `true` exactly when the entry prices on its own (both `input_per_mtok` and `output_per_mtok` for a kind entry, all four rates for a model entry), and `per_kind`, `true` exactly when the key names a registered agent kind. It is `null` when `token_rates` is absent or empty, which does not mean that nothing is priced.

A kind entry that prices its kind (both `input_per_mtok` and `output_per_mtok` set) has no effect when the sessions a configuration produces can carry no token figure: no cost can be estimated for it. `sortie validate` reports this under the check `agent.kind.no_cost_estimate`, naming the kind, so an operator who prices a non-reporting kind learns why the dashboard shows no cost for that kind rather than discovering it by reading source.

| Field | Type | Default | Description |
| --- | --- | --- | --- |
| `token_rates` | map | _(absent)_ | Top-level extension key. Keys are agent adapter kind strings or model names. |
| `token_rates.<key>.input_per_mtok` | number | _(not set)_ | USD per million input tokens. A kind entry needs it, with `output_per_mtok`, to price at all. |
| `token_rates.<key>.output_per_mtok` | number | _(not set)_ | USD per million output tokens. A kind entry needs it, with `input_per_mtok`, to price at all. |
| `token_rates.<key>.cache_read_per_mtok` | number | _(not set)_ | USD per million cache-read tokens. Unset in a kind entry, cache reads are priced at `input_per_mtok`; unset in a model entry, the rate comes from the table that lists the model, and the class is not priced when no table does. |
| `token_rates.<key>.cache_write_per_mtok` | number | _(not set)_ | USD per million cache-write tokens, unset under the same rule as `cache_read_per_mtok`. |
| `price_table` | map | _(absent)_ | Top-level extension key governing the price tables beneath `token_rates`. |
| `price_table.embedded` | boolean | `true` | `false` removes the table embedded in the binary from the layers. |
| `price_table.fetch` | boolean | `true` | Downloads the shared price table at startup and every 24 hours; `false` turns the download off. Overridden by `SORTIE_PRICE_TABLE_FETCH`. |
| `price_table.fetch_url` | string | `https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json` | Address of the shared price table. |
| `price_table.replace_file` | path | _(not set)_ | A price table of your own, in the shared table's format, that replaces the downloaded and embedded tables. Supports `$VAR` and `~` expansion; a relative path is read from the directory of WORKFLOW.md. |
| `price_table.replace_url` | string | _(not set)_ | A price table of your own, downloaded like the shared table whenever it is set, whatever `price_table.fetch` says, that replaces the downloaded and embedded tables once a download of it has passed validation; they price until then. |

**Validation rules:**

- `token_rates` MUST be a map when present. Non-map values produce a warning (not a fatal error).
- Each entry value MUST be a map. Non-map values produce a warning for that key.
- Rate values MUST be non-negative, finite numbers. A negative, non-finite, or non-numeric value produces a warning and is treated as not configured.
- On a reload, a `token_rates` that is not a map keeps every entry in force, and an entry that is not a map or holds a rate that is not a finite, non-negative number keeps the rates it had before the reload; an entry the reload adds is read as at startup. Every other entry applies.
- An unrecognized key inside an entry produces a warning and is ignored.
- A kind entry needs both `input_per_mtok` and `output_per_mtok` to price anything. A kind entry missing either one, or holding neither, prices nothing: its kind's runs are unpriced on the dashboard, the JSON API, and `sortie stats`, and the price tables do not price them either. This holds even when the incomplete entry is the only one `token_rates` configures.
- Zero-valued rates are valid and produce `$0.00` for that token type.
- An entry keyed to the empty string is dropped and produces a warning; it prices nothing.
- Every warning above is a configuration advisory under check `token_rates` (Section 8): it reaches the run log once per configuration change, `sortie validate`'s diagnostics without affecting `valid` or the exit status, and the dry run.
- A kind entry draws a configuration advisory under check `token_rates.per_kind`, saying that it turns off per-model pricing for its kind.
- A model entry for a model that the price table the configuration selects (your own table, else the embedded table) does not list, and that leaves a rate unset, draws a configuration advisory under check `token_rates.model_incomplete`, naming the unset rates. It never fails the load: the entry prices the classes it sets, and a run that spends an unset class is `partial`. A downloaded table is not consulted, so `sortie validate` and the running service report the same advisories.
- `price_table` MUST be a map, `price_table.embedded` and `price_table.fetch` booleans, and `price_table.fetch_url`, `price_table.replace_file`, and `price_table.replace_url` strings, when present. Any other type fails the load. An empty `price_table.fetch_url` takes the default address.
- A `price_table.fetch_url` or `price_table.replace_url` set through a `$VAR` that resolves to an empty string is an error under the check named for the key, at startup, in `sortie validate`, and on reload, which keeps the previous configuration; it applies whatever `price_table.fetch` says. The load does not check the shape of either address: one that cannot be requested fails each download try with a warning in the log.
- `price_table.replace_file` and `price_table.replace_url` MUST NOT both be set; setting both is an error under check `price_table.replace_conflict`. A `price_table.replace_file` that cannot be read or fails the price-table validation is an error under check `price_table.replace_file`, at startup, in `sortie validate`, and on reload, which keeps the previous configuration. So is a `price_table.replace_file` that is not a string, and one set through a `$VAR` that resolves to an empty string. A table that fails to load never falls back to the embedded table or any other table.

**Reload behavior:** `token_rates` and `price_table` reload with the rest of WORKFLOW.md, and `price_table.replace_file` is read again on every load. New rates apply to the live estimate of running sessions and to every run recorded after the reload; a cost already recorded never changes. `config.token_rates` serves the rates in force from the reload on, and a reload that fails keeps the previous rates, and so does an entry a reload gives an invalid value (Validation rules above).

### 4.5 Adapter-Specific Pass-Through Config

Each adapter (tracker or agent) may define configuration in a top-level object named after its `kind` value. These values are passed through to the adapter without validation by the orchestrator core.

A session uses the block belonging to the agent kind it was dispatched on. That kind is the one a matching `dispatch.rules` entry selected, otherwise `dispatch.default.agent`, otherwise `agent.kind`, following the fallback chain in [Section 2.10](#210-dispatch--rule-based-routing). The block named by `agent.kind` therefore applies only when neither a matching rule nor the dispatch default chose another kind. The block an attempt runs with is resolved from `WORKFLOW.md` at the start of every attempt, so a change to a block applies from the next attempt without a restart, while a running session keeps the settings it started with. A rule may carry its own block for the kind it runs, laid over the kind's top-level block key by key; see [Rule settings blocks](#rule-settings-blocks). A key whose value is a map or a list, such as `codex.turn_sandbox_policy`, `opencode.allowed_tools`, and `opencode.denied_tools`, is replaced whole by a rule that writes it, never merged. A key with the wrong YAML type fails validation and, if it reaches a session, the session start; nothing is constructed from a block in advance.

**Model:** `model` is an optional string key that means the same in the block of every agent kind that reads it, as `effort` does. An absent key, a YAML null, and an empty string leave the model unset, and the agent runs at its own default. Any other string is handed to the runtime exactly as written, and the runtime judges the name. A value of any other YAML type fails validation under the check `<kind>.model.wrong_type` and fails the session start. The `claude-code`, `codex`, `copilot-cli`, and `opencode` kinds read it.

**Source order for `model` and `effort`:** the effective value comes from the first of these that sets it: the rule's block, the kind's top-level block, the runtime's own host settings, and the runtime's default. A cap the runtime's own settings impose applies on top of all of them. A rule that writes the key as `null` removes the top-level value, which leaves the runtime's own settings and default in charge.

**Reasoning effort:** `effort` is an optional string key that means the same in the block of every agent kind that reads it. An absent key, a YAML null, and an empty string leave the level unset, and the agent runs at its own default. Any other string is the level, delivered exactly as written: Sortie does not trim it, change its case, or check it against a list of names, because the names a runtime accepts depend on its version and on the model. A set level rides every turn of every session, credential verification included, and outranks every host setting that names a level, `CLAUDE_CODE_EFFORT_LEVEL` included. A cap a runtime's own settings impose, such as Claude Code's `maxEffortLevel`, still applies. The runtime judges the name, and it takes one of three courses with a name it does not accept: it refuses the run before any work, it runs at its default level with a warning in Sortie's log, or it runs at another level and reports nothing. The table of each adapter below states which course its runtime takes. A value of any other YAML type fails the session start and, offline, is reported by `sortie validate` under the check `<kind>.effort.wrong_type`. The `claude-code`, `codex`, `copilot-cli`, and `opencode` kinds forward the level. The `agent-client-protocol` and `mock` kinds pass none, and a workflow that reaches either one with `effort` set in its block draws the advisory `agent.effort.not_forwarded`.

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

The `file:` block is forwarded to the file tracker adapter. The `path` field is required and specifies the filesystem path to a JSON file containing issue records. This adapter is intended for local testing and CI workflows where a live tracker is not available. It is also how a CI job hands Sortie one work item: write the issue to the file, then run `sortie --once WORKFLOW.md`, which finishes the issues ready when it starts and exits. The exit status is `0` when every issue's last run succeeded or nothing was ready, `1` when startup or the single poll failed, and `2` when an issue's last run did not succeed, a spend ceiling held an issue, or a signal stopped the work. Add `--once-result PATH` to also write a JSON file with the latest outcome of each issue (`succeeded`, `stopped`, `failed`, or `cancelled`) for the job to read after the run.

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

The `claude-code` block is forwarded to the Claude Code adapter, which runs `claude -p --output-format stream-json --verbose` once per turn and maps these fields to CLI flags. Two keys are checked before a run starts, `permission_mode` and `session_persistence`; the preflight refuses a value it cannot support, while the adapter itself refuses nothing and would launch on either. Every other value reaches the CLI as written, except `mcp_config`, which the generated configuration the worker writes supersedes. What the CLI does with an invalid value differs per flag: `--effort` with a name Claude Code does not recognize falls back to the model's default level and writes a warning to Claude Code's standard error, which Sortie logs as a warning, and an unknown model name reaches the API and fails there. A string key whose YAML value carries another type fails the session start and, offline, is reported by `sortie validate` under the check `claude-code.<key>.wrong_type`.

| Field | Type | Default | Description |
| --- | --- | --- | --- |
| `claude-code.permission_mode` | string | _(absent)_ | Forwarded to `--permission-mode`. `bypassPermissions` is the only value that survives the dispatch preflight; every other non-empty value fails the check `claude-code.permission_mode.interactive` and no agent is dispatched while that value stands. When absent, the adapter passes `--dangerously-skip-permissions` instead. See the note below. |
| `claude-code.model` | string | _(absent)_ | Forwarded to `--model`. Accepts a model alias such as `sonnet`, or a full model name. |
| `claude-code.fallback_model` | string | _(absent)_ | Forwarded to `--fallback-model`. Accepts one model or a comma-separated chain. Covers model availability only; see the fallback note below. |
| `claude-code.max_turns` | integer | _(absent)_ | Forwarded to `--max-turns` when greater than zero. Claude Code's own agentic turn budget within one invocation. |
| `claude-code.max_budget_usd` | number | _(absent)_ | Forwarded to `--max-budget-usd` when greater than zero. Spend cap for one invocation. The counter restarts on every turn, including resumed ones, so the ceiling for a whole session is this value times `agent.max_turns`. |
| `claude-code.effort` | string | _(absent)_ | Forwarded to `--effort` on every turn; see the reasoning effort paragraph above. Values: `low`, `medium`, `high`, `xhigh`, `max`, and `ultracode`, which starts the session at `xhigh` with ultracode enabled. The accepted set depends on the model. A set key outranks `CLAUDE_CODE_EFFORT_LEVEL`: every launch of the session runs without that variable, locally and over SSH, and an unset key leaves it in effect. A `maxEffortLevel` cap in Claude Code's settings still applies. An unrecognized name, `auto` included, runs at the model's default level, and Sortie logs Claude Code's warning once per session. A level the model lacks runs at the highest level the model supports below it, and a model without levels runs with none; neither is reported. |
| `claude-code.allowed_tools` | string | _(absent)_ | Forwarded to `--allowedTools` as a single argument. A comma- or space-separated list of tools that run without a permission prompt, including scoped rules such as `Bash(git diff *)`. |
| `claude-code.disallowed_tools` | string | _(absent)_ | Forwarded to `--disallowedTools` as a single argument. A comma- or space-separated deny list. A bare tool name removes that tool from the model's context; a scoped rule leaves the tool available and denies only matching calls. |
| `claude-code.system_prompt` | string | _(absent)_ | Forwarded to `--append-system-prompt`. The text is appended to the default system prompt rather than replacing it. Claude Code's built-in tool instructions stay in effect. The credential-verification request does not send it and uses a short fixed prompt instead. |
| `claude-code.mcp_config` | string | _(absent)_ | Path to an MCP server configuration JSON file, resolved relative to the directory holding WORKFLOW.md when it is not absolute. The worker reads that file, merges its own `sortie-tools` server into a generated copy under the workspace, and passes a file derived from the copy to `--mcp-config`, which takes a single configuration path. The derived file names a header value by the environment variable that carries it instead of holding the value, and Sortie sets that variable in the runtime's environment. A server marked disabled reaches no runtime. On a remote launch the declaration travels inline and carries Sortie's tool server and the HTTP servers the file lists, and the stdio servers are left out and the session says so. The operator's file is never modified. A file that already declares a `sortie-tools` server fails the attempt. |
| `claude-code.session_persistence` | boolean | `true` | When `false`, adds `--no-session-persistence` and Claude Code writes no session file to disk. Setting it to `false` is refused before the run starts, because the adapter passes `--resume <session_id>` on every turn but the first of a session this run created, which uses `--session-id` instead, and `--resume` needs the persisted session. |

> **Important:** `bypassPermissions` is the only `permission_mode` value that survives the dispatch preflight. Any other non-empty value fails the check `claude-code.permission_mode.interactive` and no agent is dispatched while that value stands, because a mode that lets the agent stop and ask for approval leaves an unattended run with no one to answer the prompt. The check is an allowlist rather than a list of asking modes, so a mode the CLI adds later is refused until someone establishes what it does rather than passing validation unexamined.

> **Important:** `agent.max_turns` (orchestrator turn-loop limit) and `claude-code.max_turns` (CLI `--max-turns` flag) are distinct values. The orchestrator limit controls how many turns the worker runs before exiting. The adapter limit controls the Claude Code CLI's internal turn budget. They serve different purposes and should typically have different values.

> **Important:** `claude-code.fallback_model` covers model availability, not provider exhaustion. Claude Code switches to the fallback when the primary model is overloaded, unavailable (a retired model, for example), or returns another non-retryable server error. Authentication, billing, rate-limit, request-size, and transport errors never trigger a switch; they follow their normal retry and error handling. The switch lasts for the current turn only; the next turn starts on the primary model. Claude Code caps a chain at three models after removing duplicates and ignores the rest. The adapter forwards the configured string unchanged.

**Copilot CLI adapter:**

```yaml
copilot-cli:
  model: gpt-5-mini
  effort: high
  max_autopilot_continues: 100
  agent: coding-agent
  allowed_tools: shell
  denied_tools: write
  mcp_config: ./mcp-servers.json
  disable_builtin_mcps: true
  no_custom_instructions: true
  experimental: true
```

The `copilot-cli` block is forwarded to the Copilot CLI adapter, which runs `copilot -p --output-format json -s --autopilot --no-ask-user` once per turn and maps these fields to CLI flags. The adapter mints its own session identifier, a v4 UUID, when it starts a session: the first turn of a session it created carries `--session-id <uuid>`, and every later turn carries `--resume <uuid>`. No launch ever carries `--continue`, which resumed whichever session the home directory saw most recently, possibly another issue's. The adapter validates none of these values and forwards each as written, except in the two cases described below: a non-positive `max_autopilot_continues` is replaced by `50`, and `mcp_config` is reformatted before it reaches `--additional-mcp-config`. A string key whose YAML value carries another type fails the session start and, offline, is reported by `sortie validate` under the check `copilot-cli.<key>.wrong_type`. The adapter reads the runtime's own task-completion report as the turn's outcome, not solely the terminal event's exit code. Copilot CLI 1.0.51 or later is required for `--session-id`; an older CLI rejects the flag. The credential-verification session (Section 2.6) is deleted at the end of that step with a `session.delete` call over the runtime's `--server --stdio` interface, so it leaves no stored conversation behind.

When each turn ends, Sortie asks Copilot CLI for its usage limits with Copilot CLI's own GitHub sign-in, and the dashboard shows the monthly quotas that come back. Sortie's host sends the request to `api.github.com` with the first of `COPILOT_GITHUB_TOKEN`, `GH_TOKEN`, and `GITHUB_TOKEN` that is set in the agent's environment, and the request leaves from Sortie's host on an SSH worker too. When none is set, or GitHub does not accept it, Sortie asks Copilot CLI itself, which answers from its own sign-in and, on an SSH worker, runs on the worker host. Limits are reported only for a GitHub sign-in: with `COPILOT_PROVIDER_BASE_URL` set, the custom provider spends no Copilot quota and nothing is asked. A cancelled or timed out turn, or one whose output Sortie could not read, asks nothing, and a failed request reports nothing and leaves the limits already shown in place. Sortie never stores or logs the token.

A `copilot-cli` run on an SSH worker reports no token usage: Sortie reads the token figure from a file on the agent's host and does not read it over SSH. `agent.max_tokens` and the money ceilings cannot bound such a run, preflight warns when one of them is set, and the spend of running sessions reads as a lower bound.

| Field | Type | Default | Description |
| --- | --- | --- | --- |
| `copilot-cli.model` | string | _(absent)_ | Forwarded to `--model`. The accepted model identifiers depend on the installed CLI and the account; `copilot --help` lists the set. |
| `copilot-cli.effort` | string | _(absent)_ | Forwarded to `--reasoning-effort` on every turn, credential verification included; see the reasoning effort paragraph above. `copilot --help` lists the names the installed CLI accepts. The flag outranks `effortLevel` in the user and repository Copilot settings, and no environment variable carries a level. The CLI exits with status 1 before any model request on an unknown name, on a level the chosen model does not list, and on any level for a model without levels. Sortie reports the runtime's text on the credential-verification session, before any working turn, and retries with exponential backoff. With a custom provider (`COPILOT_PROVIDER_BASE_URL`) the CLI skips the model check, so the name reaches the provider as written. |
| `copilot-cli.max_autopilot_continues` | integer | `50` | Forwarded to `--max-autopilot-continues`, the ceiling on autopilot continuation steps inside one turn. The flag is always passed. An absent key, a non-integer value, and any value of zero or less all send `50`. Reaching the ceiling ends the turn without the runtime's task-completion report, which the adapter reports as a failed turn with error kind `turn_incomplete`: the attempt ends there and the scheduled retry resumes the same session with a fresh ceiling. |
| `copilot-cli.agent` | string | _(absent)_ | Forwarded to `--agent`. Selects a named Copilot agent persona for the turn. |
| `copilot-cli.allowed_tools` | string | _(absent)_ | Forwarded to `--allow-tool` as a single argument. A non-whitespace value replaces `--allow-all`, so it changes what the launch approves. See the tool-scoping note below. Each rule takes the form `kind(argument)`, with the argument optional, so a bare kind names the whole kind; for example `write` or `shell(git:*)`. |
| `copilot-cli.denied_tools` | string | _(absent)_ | Forwarded to `--deny-tool` as a single argument. It composes with `--allow-all` rather than replacing it, so it does not change what the launch approves elsewhere. See the tool-scoping note below. Takes the same `kind(argument)` form as `copilot-cli.allowed_tools`. |
| `copilot-cli.available_tools` | string | _(absent)_ | Forwarded to `--available-tools` as a single argument. Names the set of tools the agent may see; it composes with `--allow-all` rather than replacing it, so it does not change what the launch approves. See the tool-scoping note below. The credential-verification request does not send it and offers the read-only `view` tool instead. |
| `copilot-cli.excluded_tools` | string | _(absent)_ | Forwarded to `--excluded-tools` as a single argument. Names the set of tools withheld from the agent; it composes with `--allow-all` rather than replacing it, so it does not change what the launch approves. See the tool-scoping note below. The credential-verification request does not send it, because it offers only the read-only `view` tool. |
| `copilot-cli.mcp_config` | string | _(absent)_ | Path to an MCP server configuration JSON file, resolved relative to the directory holding WORKFLOW.md when it is not absolute. The worker reads that file, merges its own `sortie-tools` server into a generated copy under the workspace, and passes a file derived from the copy to `--additional-mcp-config` prefixed with `@`, which is how the Copilot CLI reads a configuration from a file. The derived file names a header value by the environment variable that carries it instead of holding the value, and Sortie sets that variable in the runtime's environment. A server marked disabled reaches no runtime. On a remote launch the declaration travels inline and carries Sortie's tool server and the HTTP servers the file lists, and the stdio servers are left out and the session says so. The operator's file is never modified. A file that already declares a `sortie-tools` server fails the attempt. See the value-handling note below. |
| `copilot-cli.disable_builtin_mcps` | boolean | `false` | Adds `--disable-builtin-mcps` when `true`, withholding the CLI's built-in MCP servers. The credential-verification request always adds it, whatever this key says. |
| `copilot-cli.no_custom_instructions` | boolean | `false` | Adds `--no-custom-instructions` when `true`, so the CLI skips the custom instruction files it would otherwise read. The credential-verification request always adds it, once, whatever this key says. |
| `copilot-cli.experimental` | boolean | `false` | Adds `--experimental` when `true`, enabling the CLI's experimental features. |

> **Important:** `allowed_tools` is the only one of the four tool-scoping keys that changes what the launch approves. With it unset, the adapter passes `--allow-all` and `denied_tools`, `available_tools`, and `excluded_tools` keep their own effect beside that grant. Setting `allowed_tools` to a non-whitespace value replaces `--allow-all` with the list, so a call the list does not match is denied without a prompt, the turn continues, and a fully denied turn can still finish reporting success. `--allow-all` bundles tool approval, file-path verification, and URL access, so replacing it also restores the runtime's default confinement of file access to the working directory. `sortie validate` reports that one configuration as a warning under check `copilot-cli.allowed_tools.auto_deny` and blocks nothing. The example block above sets `allowed_tools`, so it is the narrowed configuration and the one that draws the warning.

> **Important:** the generated MCP configuration file supersedes `copilot-cli.mcp_config` as the value of `--additional-mcp-config`, which is why the operator's servers reach the agent through the merge described above rather than through a second flag. The configured value is forwarded on its own only when no generated file exists. In that case its form decides the argument: a value beginning with `{` is passed as inline JSON, a value already beginning with `@` is passed unchanged, and anything else is read as a file path and prefixed with `@`.

**Codex adapter:**

```yaml
codex:
  model: o3                       # Model override (e.g., "gpt-5.4", "o3")
  effort: medium                  # Reasoning effort: the model's own level name, for example "low", "medium", "high"
  approval_policy: never          # "never" is the only value the preflight allows
  thread_sandbox: workspaceWrite  # "workspaceWrite" (default), "readOnly", "dangerFullAccess", "externalSandbox"
  personality: concise            # Personality preset
  turn_sandbox_policy:            # Per-turn sandbox policy override (optional)
    networkAccess: true
  mcp_config: ./mcp-servers.json
```

The `codex` block is forwarded to the Codex adapter, which launches `codex app-server` once per session and sends these fields on the `thread/start` and `turn/start` JSON-RPC requests as the session starts and as each turn runs. One key is checked before a run starts, `approval_policy`; the preflight refuses any value other than `never`, while the adapter itself refuses nothing further once a run starts. A string key whose YAML value carries another type fails the session start and, offline, is reported by `sortie validate` under the check `codex.<key>.wrong_type`.

The Codex adapter uses a persistent subprocess model: the app-server is launched once in `StartSession` and kept alive across turns, unlike the per-turn subprocess model used by `claude-code`, `copilot-cli`, and `opencode`.

| Field | Type | Default | Description |
| --- | --- | --- | --- |
| `codex.model` | string | _(absent)_ | Forwarded to both `thread/start`'s `model` field and `turn/start`'s `model` field when non-empty. |
| `codex.effort` | string | _(absent)_ | Forwarded to `turn/start`'s `effort` field on every turn when non-empty; see the reasoning effort paragraph above. The value overrides the configured `model_reasoning_effort` for that turn and later turns of the thread. Codex sends the name as written, so the provider refuses an unknown name or a level the model lacks with HTTP 400, and the turn fails with its text, the credential-verification turn first. Not sent on `thread/start`. |
| `codex.approval_policy` | string | `never` | Forwarded to `thread/start`'s `approvalPolicy` field, defaulting to `never` when absent. `never` is the only value that survives the dispatch preflight; every other non-empty value fails the check `codex.approval_policy.interactive` and no agent is dispatched while that value stands. See the note below. |
| `codex.thread_sandbox` | string | `workspaceWrite` | Reaches two protocol fields: normalized to kebab-case (`workspace-write`, `read-only`, `danger-full-access`, `external-sandbox`) for `thread/start`'s `sandbox` field, and denormalized back to camelCase for `turn/start`'s `sandboxPolicy.type` field. Values: `workspaceWrite`, `readOnly`, `dangerFullAccess`, `externalSandbox`. |
| `codex.turn_sandbox_policy` | map | _(absent)_ | Merged over the default `sandboxPolicy` sent on `turn/start` (`writableRoots` defaulting to the workspace path, `networkAccess` defaulting to `false`); a key here replaces the matching default key, including `writableRoots` and `networkAccess`. Sent on the first turn of every session, and on every later turn once it is set. |
| `codex.personality` | string | _(absent)_ | Forwarded to `thread/start`'s `personality` field when non-empty. The credential-verification request does not send it. |
| `codex.mcp_config` | string | _(absent)_ | Path to an MCP server configuration JSON file, resolved relative to the directory holding WORKFLOW.md when it is not absolute, same as the other adapters. On a local launch, the generated file's servers are re-expressed as `-c mcp_servers.<name>=<table>` overrides on the app-server launch arguments, each approving every tool on its server in advance, because the `never` approval policy would otherwise refuse every call to a tool that asks for approval. A server marked disabled is left out. A header value reaches the runtime through an environment variable Sortie sets, so a value held in no environment variable does not stop the session. A stdio server's env values reach the runtime through its environment; only a name the environment already holds with another value, or that stdio servers declare with different values, stays on the command line, and Sortie logs a warning naming the server and the variable. On an SSH launch the HTTP servers reach the runtime and the stdio servers are left out, and the session says so; see the MCP configuration section above. |

> **Important:** `never` is the only `approval_policy` value that survives the dispatch preflight. Any other non-empty value, including `untrusted` and `on-request`, fails the check `codex.approval_policy.interactive` and no agent is dispatched while that value stands. Codex also accepts an object form of this policy, a `granular` member whose booleans decide each approval category, but this key is read as a string only, so a map value is a type fault that fails the session start rather than a value Codex sees.

> **Important:** When `thread_sandbox` is omitted, the adapter defaults to `workspaceWrite` with `writableRoots` set to the workspace path and `networkAccess: false`. Use `turn_sandbox_policy` to override specific sandbox fields per turn.

**OpenCode adapter:**

```yaml
opencode:
  model: anthropic/claude-sonnet-4-5
  agent: build
  effort: high
  thinking: true
  dangerously_skip_permissions: true
  disable_autocompact: true
  allowed_tools:
    - read
    - glob
  denied_tools:
    - bash
```

The `opencode` block is forwarded to the OpenCode adapter, which drives OpenCode 2.x. It queries `agent.command` for its version at the start of each session and refuses a version it cannot read and any major other than 2. OpenCode 2.x is published on npm as `@opencode/cli`. A string key whose YAML value carries another type fails the session start and, offline, is reported by `sortie validate` under the check `opencode.<key>.wrong_type`.

The adapter runs `opencode run --format json --standalone` once per turn with the prompt on standard input, appends `--session <session_id>` when continuing a session, and recovers final token usage with `opencode session export --standalone --sanitize <session_id>` when the session ID is known.

When each turn ends, Sortie asks OpenCode for its usage limits by running `opencode api --standalone experimental.usage.limits`, which answers from OpenCode's own sign-in, and the dashboard shows the rolling, weekly, and monthly limits that come back. Limits are reported only while OpenCode is signed in to its console and the installed release has that command; otherwise nothing is reported. The command runs with a private server of its own and leaves no background service behind. It runs on this host, or on the worker host for an SSH worker, and Sortie's host contacts no other host for it. OpenCode also reports a free usage limit while it is reached, with the time it resets. A cancelled or timed out turn, or one whose output Sortie could not read, asks nothing, and a failed command reports nothing and leaves the limits already shown in place.

| Field | Type | Default | Description |
| --- | --- | --- | --- |
| `opencode.model` | string | _(absent)_ | Value forwarded to `opencode run --model`. A non-empty `opencode.effort` or `opencode.variant` is appended after a `#`. |
| `opencode.agent` | string | _(absent)_ | Value forwarded to `opencode run --agent`. Selects the OpenCode agent profile for the turn. |
| `opencode.effort` | string | _(absent)_ | Fills OpenCode's model-variant slot on every turn, credential verification included; see the reasoning effort paragraph above. Folded into `opencode.model` as a `#`-separated suffix; setting it without `opencode.model`, or with a model that already carries a `#`, fails session start. A variant the model does not define is refused before any model request, with `Variant unavailable for <provider>/<model>: <variant>`, and the turn fails with that text, the credential-verification session first. Setting it together with `opencode.variant` fails under `opencode.effort.conflict`. |
| `opencode.variant` | string | _(absent)_ | Fills the same slot as `opencode.effort`; setting both fails the session start and, offline, is reported by `sortie validate` under the check `opencode.effort.conflict`. Folded into `opencode.model` as a `#`-separated suffix; setting it without `opencode.model`, or with a model that already carries a `#`, fails session start. |
| `opencode.thinking` | boolean | `false` | Adds `--thinking` to the run command. |
| `opencode.dangerously_skip_permissions` | boolean | `true` | Adds `--dangerously-skip-permissions` when `true`. When `false`, the runtime refuses every permissioned tool call instead of performing it, and access outside the workspace is refused unless `opencode.allowed_tools` or `opencode.denied_tools` names `external_directory`. In this mode the setting overrides an `external_directory` rule in the operator's own OpenCode configuration. |
| `opencode.disable_autocompact` | boolean | `true` | Sets the turn's inline configuration document's `compaction.auto` to `false`; the adapter always sets that document's `share` to `disabled` and, in the launch environment, `OPENCODE_DISABLE_AUTOUPDATE=true`. |
| `opencode.allowed_tools` | list of strings | `[]` | Builds the turn's inline configuration document's `permission` member: listed keys become `allow`; known OpenCode permission keys not listed become `deny`; unknown keys are forwarded unchanged. The credential-verification request ignores it and denies every tool instead. |
| `opencode.denied_tools` | list of strings | `[]` | Builds explicit `deny` rules in the same policy `opencode.allowed_tools` builds. The credential-verification request ignores it and denies every tool instead. |

**Validation rules:**

- `opencode.allowed_tools` and `opencode.denied_tools` MUST NOT overlap.
- `opencode.effort` and `opencode.variant` MUST NOT both be set, because both fill the model-variant slot.
- On a local launch, the adapter removes any inherited `OPENCODE_PERMISSION` or `OPENCODE_CONFIG_CONTENT` value before launching OpenCode. On an SSH worker, the adapter's configuration document replaces any inherited `OPENCODE_CONFIG_CONTENT` value, while an `OPENCODE_PERMISSION` value set in the remote login environment is left in place. If either tool list is non-empty or `opencode.dangerously_skip_permissions` is `false`, the adapter-managed tool policy replaces the tool policy on a local launch. On an SSH worker, OpenCode applies an `OPENCODE_PERMISSION` value from the remote login environment after the adapter's configuration document, so that value wins for every tool it names. The configuration document the adapter writes disables OpenCode's title agent, so a turn, local or on an SSH worker, makes no title request and a session keeps OpenCode's default title.

**Agent Client Protocol adapter:**

```yaml
agent-client-protocol:
  mcp_config: ./mcp-servers.json
```

The `agent-client-protocol` block is forwarded to the Agent Client Protocol adapter. The block's keys are `mcp_config` and `model`. The kind carries no reasoning setting. Write the runtime's own reasoning option in `agent.command`, for example `copilot --acp --reasoning-effort high` or `kiro-cli acp -a --effort high`; a runtime without one runs at its own default. The option and its value are the operator's call, and Sortie checks neither. An `effort` key in this block draws the advisory `agent.effort.not_forwarded`. An `mcp_config` whose YAML value carries another type fails the session start and, offline, is reported by `sortie validate` under the check `agent-client-protocol.mcp_config.wrong_type`. A `model` that is not a string reads as absent. When the agent's provider refuses a turn because its usage limit is reached, the kind reports a usage limit for Copilot CLI, OpenCode, Gemini CLI, and Kiro CLI, as the `copilot-cli` and `opencode` kinds do, and `model` names the model whose new sessions the hold stops.

Sortie measures every Agent Client Protocol session by the usage the runtime returns when a turn ends, on an SSH worker as on the machine running Sortie. A runtime that reports no usage fields leaves its turns as spend of unknown amount, and a runtime without dedicated support is read as a lower bound that never counts as the whole spend. On an SSH worker Sortie also reads the runtime's own records on the worker host at every turn end, over a new SSH connection, so the run is split by served model and priced as it is on a local worker. Each read moves the whole journal, at most 64 MiB, and must finish within `agent.read_timeout_ms`. A read that fails, is cut, or runs out of time never turns a figure into zero: a turn the live figure settled keeps its amount without the split by served model, and a turn without one keeps its live reading as a lower bound or, with none, counts as spend of unknown amount.

When each turn ends, Sortie asks Copilot CLI, OpenCode, Gemini CLI, or Kiro CLI for its usage limits with the agent's own sign-in, and the dashboard shows what comes back. Copilot CLI and OpenCode get the same request as on the `copilot-cli` and `opencode` agent kinds, run with the agent's launch environment. For an SSH worker, the OpenCode command and the Copilot CLI headless server run on the worker host, while the Copilot CLI quota endpoint request leaves from Sortie's host. Limits are reported for Gemini CLI signed in with Google, for Kiro CLI, and for Copilot CLI and OpenCode as described for their own kinds; a Gemini CLI on an API key or on Vertex AI reports none, and a runtime that is none of these asks nothing. For Gemini CLI, Sortie reads the sign-in where Gemini CLI itself keeps it, on the machine that runs the agent: the access token in `GOOGLE_CLOUD_ACCESS_TOKEN` when `GOOGLE_GENAI_USE_GCA` is also set in the agent's environment, otherwise the file `.gemini/oauth_creds.json` under `GEMINI_CLI_HOME`, or under the home directory when that is not set, and with `GEMINI_FORCE_ENCRYPTED_FILE_STORAGE=true` the operating system keychain or the encrypted file `.gemini/gemini-credentials.json` in the same place. Sortie's host sends its access token to `cloudcode-pa.googleapis.com`, or to the endpoint `CODE_ASSIST_ENDPOINT` names in the agent's environment. The project comes from `GOOGLE_CLOUD_PROJECT` in the agent's environment, then from `GOOGLE_CLOUD_PROJECT_ID`, and otherwise from the project Google assigns to the account. A sign-in whose access token has expired is skipped. Sortie never stores or logs the token. For Kiro CLI, Sortie sends the request over the session's own connection, with no agent text, and contacts no host; the credits used against the plan and the date the billing cycle resets come back. A cancelled or timed out turn, or one whose output Sortie could not read, asks nothing, and a failed request reports nothing and leaves the limits already shown in place.

| Field | Type | Default | Description |
| --- | --- | --- | --- |
| `agent-client-protocol.mcp_config` | string | _(absent)_ | Path to an MCP server configuration JSON file, resolved relative to the directory holding WORKFLOW.md when it is not absolute, same as the other adapters. On a local launch, the generated file's servers are re-expressed inside the `session/new` request, which carries a header value itself. A server marked disabled is left out. When the runtime advertises HTTP tool-server support, the HTTP servers reach it on a remote launch too, and the stdio servers are left out; see the MCP configuration section above. |
| `agent-client-protocol.model` | string | _(absent)_ | The model a model-only usage-limit hold stops, and whose pulled usage window supplies a refusal's reset where each window meters one model. It does not select the runtime's model, which `agent.command` does. A value that is not a string reads as absent. |

**MCP configuration and the generated file:**

For every agent kind, the worker writes a generated MCP configuration file into the session workspace at `.sortie/mcp.json`. It always carries Sortie's own `sortie-tools` server. When the session's own agent block sets `mcp_config`, the servers declared in the file that key names are merged into the generated copy; the operator's file itself is never modified, and a file that already declares a server named `sortie-tools` fails the attempt. A server the file marks disabled reaches no agent kind. A server entry member Sortie does not read, such as `timeout`, reaches `claude-code` and `copilot-cli` unchanged; `codex`, `opencode`, and `agent-client-protocol` have no syntax for it and fail the session start, naming the server and the member. A header value of an HTTP server reaches the runtime through an environment variable Sortie sets, named `SORTIE_MCP_HEADER_<k>` with `<k>` numbered from 1, so no declaration holds the value; `agent-client-protocol` sends the value inside the session-creation request instead, because the protocol has no reference form.

Which adapters hand that generated file's servers to the agent process differs by kind, and by launch:

| Agent kind | Local launch | Remote launch |
| --- | --- | --- |
| `claude-code` | a file derived from the generated one is passed to `--mcp-config`: it marks `sortie-tools` to load upfront, so the model sees Sortie's tools without first searching for them, and it names header values by environment variable instead of carrying them | the same declaration, inline on the command line, with Sortie's tool server and the HTTP servers |
| `copilot-cli` | a file derived from the generated one, which names header values by environment variable instead of carrying them, is passed to `--additional-mcp-config` as `@<path>` | the same declaration, inline on the command line, with Sortie's tool server and the HTTP servers |
| `codex` | the generated servers are re-expressed as configuration overrides on the app-server command line, which the runtime parses into its own configuration | the same overrides, with Sortie's tool server and the HTTP servers |
| `opencode` | the generated servers are re-expressed as the runtime's own configuration document, delivered through an inline configuration environment variable | the same document, with Sortie's tool server and the HTTP servers |
| `mock` | none; the adapter launches no process | none; the adapter launches no process |
| `agent-client-protocol` | when the runtime advertises HTTP tool-server support, Sortie's tool server as an HTTP server on the machine running Sortie and the HTTP servers, inside the session-creation request; otherwise the stdio servers in the same request | when the runtime advertises HTTP tool-server support, the same request as a local launch; otherwise no server |

On a remote launch, Sortie's tool server keeps running on the machine running Sortie, so its tracker credentials and database stay there. The agent reaches it over a reverse SSH forward on the launch's own connection, and a per-session bearer, valid only while the session runs, authorizes every request. The HTTP servers listed under `mcp_config` reach the runtime as written, and their URLs resolve on the remote host; the stdio servers are left out, because a stdio server's command would run on the remote host, and the session says so in its opening notice and in the log. A remote host that forbids port forwarding, a forward whose outcome Sortie cannot confirm, and an agent that never connects within `agent.read_timeout_ms` each leave the session running without Sortie's tools: the opening notice declares the gap and the prompt names no Sortie tool.

Setting `mcp_config` in a block belonging to `mock` therefore has no effect on the agent. `sortie validate` reports that combination as a warning naming the kind, under the check `agent.mcp_config`. A separate warning, `agent.kind.no_tool_channel`, fires for any agent kind that has no way to deliver Sortie's tools, stating that Sortie's tools will be neither advertised nor callable for it. Both are warnings and not errors: such a configuration stays valid, the run proceeds, and the exit code is unchanged.

A kind named by `dispatch.default.agent` or by `dispatch.rules[*].agent` that differs from the top-level `agent.kind` must carry its own top-level settings block; `agent.kind` itself never requires one. An empty mapping (`codex: {}`) or a bare key with nothing following (`codex:`) satisfies the requirement; a scalar or a list value does not. `sortie validate` reports a missing or malformed block as an error under the check `dispatch.agent.missing_block`, naming the selector that introduced the kind and the block it expects. `agent.command` belongs to the default kind (`dispatch.default.agent`, otherwise `agent.kind`), and a routed kind's own settings block cannot override it ([architecture §5.3.5](architecture/05-workflow-specification.md#535-agent-object)). A routed kind launches its own default command, and `sortie validate` reports a route to a kind that has none, such as `agent-client-protocol` beside a different default kind, as an error under the check `agent.command`. Adding the block satisfies the `dispatch.agent.missing_block` check without giving the route a command.

`claude-code.mcp_config`, `copilot-cli.mcp_config`, `codex.mcp_config`, and `agent-client-protocol.mcp_config` are documented in the Claude Code, Copilot CLI, Codex, and Agent Client Protocol tables above.

**Custom or future adapters (illustrative example):**

```yaml
my-custom-adapter:
  option_one: value
  option_two: true
```

The orchestrator forwards the entire sub-object to the matching adapter without interpretation. Any adapter you register can read its fields from this block.

---

## 5. Prompt Template Reference

### 5.1 Template Engine

Sortie uses Go [`text/template`](https://pkg.go.dev/text/template) with strict mode enabled:

```go
template.New("prompt").
    Option("missingkey=error").
    Funcs(promptFuncMap).
    Parse(body)
```

**Strict mode guarantees:**

- Referencing an **unknown variable** fails rendering immediately (does not produce empty string).
- Calling an **unknown function** fails rendering immediately.
- `missingkey=error` distinguishes between a map key that is absent (error) and a key that is present with a `nil` value (evaluates as falsy in `{{ if }}`).

### 5.2 Template Input Variables

The data map passed to `Execute` contains the **core top-level keys** (`issue`, `attempt`, `run`, `stage`, `park`) plus **continuation context keys** (`ci_failure`, `review_comments`, `bot_review_comments`, `merge_conflict`, `label_review`, `label_fix`) that are `nil` by default and populated on reaction-triggered dispatches. `park` is `nil` except on turn 1 of the first run after a person releases a park (see the `park` variable below). A reaction run is a run that a reaction starts, error retries included. Its key stays set on every turn of the run, so a `.run.is_continuation` test inside the key's branch tells the first turn from later ones (see Section 5.5). `review_comments` and `bot_review_comments` are also populated on turn 1 of a new run of an issue whose pull request holds comments no earlier run was given (see `reactions.review_comments` and `reactions.bot_review` in Section 2.9):

#### `issue` — Normalized Issue Object

All fields from the tracker, normalized into a stable structure regardless of the underlying tracker system.

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
| `.issue.comments`    | list or nil     | Comment records (feedback, review notes, workpad entries), oldest first. Each carries `id`, `author`, `body`, and `created_at`, the tracker's timestamp text: RFC 3339, or the same with a `±hhmm` offset. Filled on runs a retry timer starts and on the first run after a person releases a park; `nil` on other runs the poll tick starts, meaning comments were not fetched. An empty non-nil list means no comments exist. |
| `.issue.blocked_by`  | list or nil     | Blocker references, each with `.id`, `.identifier`, `.state`, `.display_id`. `nil` means the blockers were not resolved; an empty non-nil list means no blockers exist. `.display_id` is the qualified form when the tracker's identifier alone is ambiguous (for example GitHub's `owner/repo#5` against an `identifier` of `5`) and empty otherwise. |
| `.issue.created_at`  | string          | ISO-8601 creation timestamp. Empty string when absent.                                                                                           |
| `.issue.updated_at`  | string          | ISO-8601 last-update timestamp. Empty string when absent.                                                                                        |

#### `attempt` — Retry Counter

| Value          | Meaning                                                                                   |
| -------------- | ----------------------------------------------------------------------------------------- |
| Integer `0`    | First try, no prior worker failures in this session.                                      |
| Integer `>= 1` | Retry try number after a worker failure. The value does not change on continuation turns. |

**Template usage:** Use `{{ if .attempt }}` to distinguish first tries from retries. `attempt` is always an integer; on the first try it is `0`, so `{{ if .attempt }}` evaluates to `false`, and on retries it is `>= 1`, so `{{ if .attempt }}` evaluates to `true`. Continuation turns within the same session reuse the same `attempt` value.

#### `run` — Per-Turn Metadata

| Field                  | Type    | Description                                                                                                        |
| ---------------------- | ------- | ------------------------------------------------------------------------------------------------------------------ |
| `.run.turn_number`     | integer | Current turn number within the session.                                                                            |
| `.run.max_turns`       | integer | Configured maximum turns per session.                                                                              |
| `.run.is_continuation` | boolean | `true` when this is a continuation turn within a multi-turn session (not the first turn, not a retry after error). |

#### `stage`: Stage Chain Context

Present on every render, first turn and continuation turns alike, and never nil, so `{{ .stage.previous }}` renders under `missingkey=error` in a workflow without chains. Each value is a string, empty when it does not apply.

| Field                    | Type   | Description                                                                                                                                                                  |
| ------------------------ | ------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `.stage.current`         | string | Name of the rule this dispatch runs, when that rule carries `stage`. Empty for a rule without `stage`, for `dispatch.default`, and when no rule applies. It is read from the configuration in force at each dispatch. |
| `.stage.previous`        | string | Name of the rule whose hop led to this dispatch. Filled when the dispatch selects the target of the issue's latest hop and the hop count has not reset since; empty otherwise.   |
| `.stage.previous_outcome` | string | `succeeded`, or `no_change` when the previous stage's run declared that nothing needed changing. Empty when `.stage.previous` is empty.                                         |

A retry or reaction continuation that keeps its rule renders the `previous` pair its dispatch had, also after a restart. See [Stage chains](#stage-chains) and [Freeze-on-dispatch](#freeze-on-dispatch).

```markdown
{{ if .stage.previous }}The {{ .stage.previous }} stage finished first ({{ .stage.previous_outcome }}). Read its output in the workspace before you start.{{ end }}
```

#### `park`: Park Hand-Over Context

Present as a key on every render, so `{{ .park }}` and `{{ if .park }}` render under `missingkey=error` in every template. It is non-nil only on turn 1 of the first run after a person releases a park, by removing the parking label or moving the issue to another tracker state. It is `nil` on every other render: later turns of that run, later runs, retry fires, reaction runs, and a run after a release that observed work (`evidence_observed`).

| Field                | Type   | Description                                                                                                                                                                                                                  |
| -------------------- | ------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `.park.reason`       | string | `agent_blocked` or `handoff_absence`.                                                                                                                                                                                        |
| `.park.statement`    | string | The stop statement of the run the park follows: the reason after `blocked` for `agent_blocked`, and for `handoff_absence` the statement of the declaration whose handoff Sortie withheld. Empty when that run declared none or the park recorded no run. |
| `.park.parked_at`    | string | Park time, RFC 3339 in UTC, whole seconds. Empty when the stored park time cannot be read.                                                                                                                                   |
| `.park.released_at`  | string | Release time in the same form.                                                                                                                                                                                               |
| `.park.gesture`      | string | `label_removed` or `state_changed`.                                                                                                                                                                                          |
| `.park.comments`     | list   | The comments created since the park, in tracker order, oldest first. Never `nil`; empty when nobody commented since the park. Each element has the shape of an `.issue.comments` element: `id`, `author`, `body`, `created_at`. |

A comment is in `.park.comments` when its creation time is at or after the park time truncated to the whole second. A comment whose creation time cannot be read is included, and when the park time cannot be read every comment is. `.issue.comments` carries every comment of the issue, on every turn of that run.

The run resumes the agent session of the run the park follows when that run had no reaction and ended normally, and the agent kind and template are the ones that run used. Otherwise it starts a new session, and `.park.statement` still tells it why the run stopped. When Sortie posts a stop comment to the tracker, that comment is among `.park.comments` and the run sees the statement twice. When the comments cannot be read, Sortie does not start the run: the issue waits for the next poll and appears under `held` with the reason `park_comments_unread`.

```markdown
{{ if .park }}A person released this issue ({{ .park.gesture }}). You stopped earlier: {{ .park.statement }}
{{ range .park.comments }}{{ .author }}: {{ .body }}
{{ end }}{{ end }}
```

#### `ci_failure` — CI Failure Context (continuation key)

Non-nil on every turn of a CI-fix continuation run. Contains CI failure details from `CIResult.ToTemplateMap()`:

| Field                     | Type            | Description                                              |
| ------------------------- | --------------- | -------------------------------------------------------- |
| `.ci_failure.status`      | string          | Aggregate CI pipeline status (`"failing"`).              |
| `.ci_failure.check_runs`  | list of maps    | Individual check runs with `name`, `status`, `conclusion`, `details_url`. |
| `.ci_failure.log_excerpt` | string          | Output of the failing step of the first failing check, or the end of its job log when that step cannot be found. The first line starts with `[sortie]` and takes one of four forms: it names the failing step; it names the failing step and says other steps ran at the same time, so their output is mixed in; it says the failing step could not be found and the lines are the end of the job log; or it says the step could not be found and the log was not read to its end. Up to two `[sortie] N lines omitted.` lines mark spans cut from a long step, so the excerpt holds at most `max_log_lines` plus three lines. A Gitea excerpt is the status description and URL and has no `[sortie]` line. Empty when no log is available. |
| `.ci_failure.failing_count` | integer       | Number of failing check runs.                            |
| `.ci_failure.ref`         | string          | The git ref that was queried.                            |

When `nil` (default on non-CI dispatches), `{{ if .ci_failure }}` evaluates to `false`.

#### `review_comments` — Review Comment Context (continuation key)

Non-nil on every turn of a review-fix continuation run, and on turn 1 only of a new run of an issue whose pull request holds review comments no earlier run was given, when `reactions.review_comments` has no `triage` block and the template keeps all of its own text with the variable set. Contains a list of human review comments from `CHANGES_REQUESTED` PR reviews:

| Field (per element)           | Type    | Description                                              |
| ----------------------------- | ------- | -------------------------------------------------------- |
| `.review_comments[].id`       | string  | SCM-platform comment identifier.                         |
| `.review_comments[].file`     | string  | File path (empty for PR-level comments).                 |
| `.review_comments[].start_line` | integer | First line of commented range (0 for non-inline).       |
| `.review_comments[].end_line` | integer | Last line of commented range (0 for single-line or non-inline). |
| `.review_comments[].reviewer` | string  | Username of the comment author.                          |
| `.review_comments[].body`     | string  | Comment text.                                            |

When `nil` (default on other dispatches), `{{ if .review_comments }}` evaluates to `false`. Write the comments as an added block, as the pattern below does, so a new run keeps the rest of the prompt. A template whose other text changes when `review_comments` is set renders without the comments on a new run. [Built-in reaction prompts](#built-in-reaction-prompts) describes what each shape sends a review-fix run.

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

Non-nil on every turn of a bot-review-fix continuation run, and on turn 1 only of a new run of an issue whose pull request holds bot comments no earlier run was given, under the same conditions as `review_comments`. Contains a list of comments authored by automated review bots (see `reactions.bot_review` in Section 2.9). The per-element shape is identical to `review_comments`:

| Field (per element)               | Type    | Description                                              |
| --------------------------------- | ------- | -------------------------------------------------------- |
| `.bot_review_comments[].id`       | string  | SCM-platform comment identifier.                         |
| `.bot_review_comments[].file`     | string  | File path (empty for PR-level comments).                 |
| `.bot_review_comments[].start_line` | integer | First line of commented range (0 for non-inline).       |
| `.bot_review_comments[].end_line` | integer | Last line of commented range (0 for single-line or non-inline). |
| `.bot_review_comments[].reviewer` | string  | Login of the bot that authored the comment.             |
| `.bot_review_comments[].body`     | string  | Comment text.                                            |

When `nil` (default on other dispatches), `{{ if .bot_review_comments }}` evaluates to `false`.

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

Non-nil on every turn of a merge-conflict-resolution continuation run. Contains the PR identity and the rebase target read live from the PR object (see `reactions.merge_conflicts` in Section 2.9):

| Field                      | Type    | Description                                                                 |
| -------------------------- | ------- | --------------------------------------------------------------------------- |
| `.merge_conflict.pr_number` | integer | Pull request number.                                                        |
| `.merge_conflict.branch`   | string  | PR head branch the agent rebases.                                           |
| `.merge_conflict.head_sha` | string  | Latest commit SHA on the PR head branch.                                    |
| `.merge_conflict.base`     | string  | PR's real target (base) branch, read live from the PR object. The rebase target. |

The `base` value is the PR's actual base ref, not an assumed default branch, so the agent rebases onto the correct target for PRs that target a release branch, a GitFlow `develop`, or a stacked-PR parent. The orchestrator defers the continuation while the base ref is unavailable, so `base` is always a populated branch name when this context is emitted.

When `nil` (default on non-merge-conflict dispatches), `{{ if .merge_conflict }}` evaluates to `false`.

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

Non-nil on every turn of a read-only label-review run, triggered when an operator applies the configured review label to a Sortie-managed PR (see `reactions.label_commands` in Section 2.9). Carries the PR coordinates the agent needs to fetch the diff and post its review:

| Field                        | Type    | Description                                         |
| ---------------------------- | ------- | --------------------------------------------------- |
| `.label_review.pr_number`    | integer | Pull request number to review.                      |
| `.label_review.owner`        | string  | Repository owner.                                   |
| `.label_review.repo`         | string  | Repository name.                                    |
| `.label_review.actor`        | string  | Login of the operator who applied the review label. |
| `.label_review.requested_at` | string  | RFC 3339 timestamp of the confirmed labeling gesture. |

The orchestrator injects only these coordinates. It never injects the PR diff text and never posts a comment itself; the agent fetches the diff and posts review comments using its own SCM tooling. A prompt template that omits the `{{ if .label_review }}` branch gets the built-in review prompt (see [Built-in reaction prompts](#built-in-reaction-prompts)).

When `nil` (default on non-label-review dispatches), `{{ if .label_review }}` evaluates to `false`.

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

Non-nil on every turn of a fix run, triggered when an operator applies the configured fix label to a Sortie-managed PR (see `reactions.label_commands` in Section 2.9). Carries the PR coordinates the agent needs to check out the head branch, address the review comments, and push:

| Field                     | Type    | Description                                           |
| ------------------------- | ------- | ------------------------------------------------------ |
| `.label_fix.pr_number`    | integer | Pull request number to fix.                             |
| `.label_fix.owner`        | string  | Repository owner.                                       |
| `.label_fix.repo`         | string  | Repository name.                                        |
| `.label_fix.branch`       | string  | PR head branch to check out and push to.                |
| `.label_fix.actor`        | string  | Login of the operator who applied the fix label.        |
| `.label_fix.requested_at` | string  | RFC 3339 timestamp of the confirmed labeling gesture.    |

The orchestrator injects only these coordinates. It never fetches the review comments, never applies changes, and never pushes or comments itself; the agent checks out `label_fix.branch`, addresses the comments, pushes the fixes, and posts the summary comment using its own SCM tooling. A prompt template that omits the `{{ if .label_fix }}` branch gets the built-in fix prompt (see [Built-in reaction prompts](#built-in-reaction-prompts)).

When `nil` (default on non-label-fix dispatches), `{{ if .label_fix }}` evaluates to `false`.

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

#### Built-in reaction prompts

Sortie ships a built-in prompt for each continuation key, so a reaction run always has instructions for its reaction. A branch in your template replaces the built-in prompt for its key. No setting changes the built-in text.

Sortie chooses between the branch and the built-in prompt on every turn of a reaction run, against the template that run renders: the template a dispatch rule routes the run to, or the template the run was frozen with. The choice happens when the prompt is rendered, not when the workflow loads, so `sortie validate` has no missing-branch warning to give. Sortie renders the template twice, once with the run's reaction data and once without it:

- When the render with the data is not blank and differs from the render without it, Sortie sends the render with the data. This is your branch.
- Otherwise Sortie renders the built-in prompt for the key with the same data and sends that.

These cases follow from the rule:

- A branch that renders only whitespace counts as no branch, because it would give the agent no instructions.
- A template that renders the same text with and without the data counts as no branch, because it does not react to the key.
- A template that cannot render without the data, such as one that reads `{{ .label_fix.branch }}` outside any `{{ if .label_fix }}` block, counts as having a branch. Its render with the data is sent, and a failure of that render fails the run like any other template error.

The built-in prompts ask for the following.

- `ci_failure`: fix the failing CI checks, listing each check with the excerpt of the log of the first failing one, then commit and push the fix to the branch the checks ran on.
- `review_comments`: address every review comment, each listed with its reviewer and location, then commit and push the changes to the pull request's branch.
- `bot_review_comments`: the same for the comments that automated review tools left.

The log excerpt and each comment body come from outside Sortie, so the built-in prompts pass them as JSON strings inside `<ci_log>` and `<review_comment>` blocks and tell the agent to treat them as data. JSON encoding escapes quotes, line breaks and angle brackets, so the text cannot close its block.
- `merge_conflict`: fetch the base branch, rebase the pull request's branch onto it, resolve every conflict while keeping both the intent of the pull request and the base changes, and push.
- `label_review`: fetch the diff of the pull request, review it, and post the review comments, without modifying the branch or pushing.
- `label_fix`: check out the pull request's branch, fetch the outstanding review comments, address them, push to that branch only, and post a summary comment.

Each first-turn built-in prompt ends by asking the agent to signal the outcome as the first-turn status instructions describe, so a run that finishes its task ends without using every turn. On later turns the built-in prompt sends the agent back to the instructions it received at the start of the session and adds no task. That text suits a session whose first turn came from your branch as well as one whose first turn came from the built-in prompt.

A branch is rendered again on every turn, so it sends the same text each time unless it tests `.run.is_continuation`. Put that test inside the branch, as Section 5.5 shows, to send shorter resume text on later turns.

**A reaction block inside the work prompt.** A block appended to the work prompt counts as your branch, because the render differs with the data. The run then receives the whole work prompt with the block on every turn, a label-fix run included. Sortie does not separate the block from the surrounding text; your branch always wins. To send only the reaction's instructions, put the branches in one exclusive chain, with the work prompt in the last `else`:

```
{{ if .label_review }}
...review instructions...
{{ else if .label_fix }}
...fix instructions...
{{ else if .merge_conflict }}
...conflict instructions...
{{ else }}
...work prompt...
{{ end }}
```

An appended block keeps the task description in front of the agent on a reaction run, which suits instructions that build on the work prompt. An exclusive chain keeps the work prompt out of a reaction run, which suits a fix on a checkout that can push. The shipped examples use the exclusive chain.

**Review comments.** `review_comments` and `bot_review_comments` also reach turn 1 of a new run, and the shape of the branch decides what each kind of run receives:

| Template shape | New run seeded with comments | Review-fix run |
| --- | --- | --- |
| An added block, as in the `review_comments` pattern above | The work prompt and the comments | The work prompt and the comments |
| An exclusive branch, or no block | No comments | The comments in the branch's text or the built-in prompt, and no work prompt |

A review-fix run counts every comment it carries as given to the agent, and the built-in prompt lists all of them.

### 5.3 Built-in Functions (FuncMap)

In addition to Go `text/template` built-in actions, Sortie ships a minimal set of prompt-essential functions. Each is permanent API surface.

| Function | Signature                      | Description                                                                                                                            | Example                                              |
| -------- | ------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------- |
| `toJSON` | `toJSON value → string`        | Serialize any value to compact JSON. Agents parse structured data more reliably from JSON than from Go's default `fmt` representation. | `{{ .issue.labels \| toJSON }}` → `["bug","urgent"]` |
| `join`   | `join separator list → string` | Join a list of strings with a separator.                                                                                               | `{{ .issue.labels \| join ", " }}` → `bug, urgent`   |
| `lower`  | `lower string → string`        | Lowercase a string.                                                                                                                    | `{{ .issue.state \| lower }}` → `in progress`        |

> **Note:** `join` uses pipe syntax with reversed arguments: `{{ .issue.labels | join ", " }}`. The separator comes first in the function signature because Go template pipelines pass the piped value as the last argument.

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
| `{{ define "NAME" }}...{{ end }}`                              | Define a named block (see [5.8](#58-define-blocks-and-calls)). |
| `{{ template "NAME" . }}`                                      | Call a define block, passing dot. |
| `{{ block "NAME" . }}...{{ end }}`                             | Define a block and call it in place. |

### 5.5 First-Turn vs Continuation Semantics

The prompt template supports three distinct modes within a single file. Workflow authors use `attempt` and `run.is_continuation` to branch:

| Scenario              | `attempt`      | `run.is_continuation` | Typical template action                                       |
| --------------------- | -------------- | --------------------- | ------------------------------------------------------------- |
| **First run**         | `0`            | `false`               | Full task instructions, context gathering steps.              |
| **Continuation turn** | same as turn 1 | `true`                | Resume guidance — review state, pick up where left off.       |
| **Retry after error** | integer `>= 1` | `false`               | Diagnostic steps — check prior failure, approach differently. |
| **First run after a released park** | `0` | `false` | `park` is set on turn 1 only. The run continues the agent's earlier session where the parked run recorded one and the agent kind can resume it; answer the comments in `park.comments`. |

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

- The full prompt template is rendered on **every** turn. The runtime always passes the full rendered result to the agent, regardless of turn number.
- Workflow authors control what the agent receives on continuation turns by branching on `.run.is_continuation`. Without such branching, the agent receives identical instructions every turn.
- After each turn, the worker re-checks the tracker issue state. If the issue is still active, another turn begins (up to `agent.max_turns`).
- On a reaction run, the reaction's key stays set on every turn. A template that switches on `.run.is_continuation` at the top level, with the reaction branches only in the first-turn text, renders its resume text on later turns with and without the key. Sortie treats that as no branch for the key and sends the built-in continuation text, which points back to the instructions of the first turn:

  ```
  {{ if .run.is_continuation }}
  <resume text>
  {{ else }}
  <prompt with reaction branches>
  {{ end }}
  ```

  To send your own resume text on later turns of a reaction run, put the `.run.is_continuation` test inside the key's branch:

  ```
  {{ if .label_fix }}
  {{ if .run.is_continuation }}
  <fix resume text>
  {{ else }}
  <fix instructions>
  {{ end }}
  {{ else if .run.is_continuation }}
  <resume text>
  {{ else }}
  <work prompt>
  {{ end }}
  ```

  See [Built-in reaction prompts](#built-in-reaction-prompts) for the rule that chooses between a branch and the built-in prompt.

### 5.6 Fallback Prompt Behavior

- On **continuation turns** (turn number > 1) of a run that no reaction started, if the rendered prompt is empty, the runtime substitutes a built-in default continuation prompt as a safety net. This covers templates that omit `{{ if .run.is_continuation }}` branching.
- On the **first turn** of a run that no reaction started, if the rendered prompt is empty, no fallback is applied; the empty string is passed to the agent as-is.
- On a **reaction run**, a blank render, or one that ignores the reaction's key, yields the built-in prompt for the key on any turn. The default continuation prompt and the empty first-turn prompt apply to other runs only. See [Built-in reaction prompts](#built-in-reaction-prompts).
- Workflow file read/parse failures are validation errors and do **not** silently fall back to a default prompt.

### 5.7 Common Patterns and Pitfalls

#### Dot context inside `{{ range }}`

Inside `{{ range .issue.labels }}`, the dot (`.`) refers to the **current list element**, not the root data map. To access top-level variables inside a range block, use the dollar-sign prefix:

```
{{ range .issue.labels }}
- Label: {{ . }}  (issue: {{ $.issue.identifier }})
{{ end }}
```

> **Common mistake:** Writing `{{ .issue.identifier }}` inside `{{ range }}` produces an error because `.issue` does not exist on a string element. Use `{{ $.issue.identifier }}` instead.

#### A call without a dot

`{{ template "context" }}` passes nothing, so the block runs with nil data and any field reference inside it fails the render. Pass the data explicitly: `{{ template "context" . }}`.

#### `$` and dot inside a define block

Inside a define block, `.` and `$` are the value the call passed, not the root data map. A block reached through `{{ template "context" . }}` outside every `range` and `with` body sees the whole data map, so `.issue.identifier` and `$.issue.identifier` both work. A call inside a `{{ range }}` body that passes `.` hands over the current element. Pass `$` to hand over the root data from there: `{{ template "context" $ }}`.

#### Nil-safe conditionals

Fields that may be empty (`description`, `url`, `assignee`, etc.) should be guarded to avoid rendering blank sections. Empty string evaluates to `false` in `{{ if }}`, making this pattern safe whether the field is empty or absent:

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

### 5.8 Define Blocks and Calls

A define block is a named piece of template text: `{{ define "name" }}...{{ end }}`. A call runs it: `{{ template "name" . }}`. The last argument is the value the block receives as dot; pass `.` outside `range` and `with` bodies to hand over the whole data map described in [Section 5.2](#52-template-input-variables).

- Every prompt template (the Markdown body, the default template, and each rule template) can call the define blocks of every partial listed in `dispatch.partials` and the define blocks it holds itself. See [Partials](#partials) for how to list partials.
- A name is a string literal, and a call cannot compute it. A file is not callable by its name.
- A `{{ block "name" . }}...{{ end }}` defines its name as `define` does, and runs it in place. A partial that defines the same name is an error, so `block` is not a way to override a partial.
- Every block runs under the strict rules of [Section 5.1](#51-template-engine) and with the same functions as the template that calls it.
- A call to a name that nothing defines fails the workflow load, so a typo never reaches a run. A call cycle loads, and an endless one fails at render when the engine reaches its depth limit.
- Trim white space around a block with `{{-` and `-}}` so that a call does not add blank lines.

---

## 6. Hook Lifecycle Reference

### 6.1 Execution Contract

Hooks execute as shell scripts in a local shell context:

- **Shell:** `sh -c <script>` (POSIX default). The orchestrator invokes hooks via `sh`, not `bash`. There is no `hooks.shell` configuration field.
- **Working directory:** The per-issue workspace directory.
- **Timeout:** Controlled by `hooks.timeout_ms` (default: 60,000 ms).
- **Logging:** Hook start, completion, failures, and timeouts are logged by the orchestrator.
- **Process lifetime:** When a hook's shell exits, whatever its exit status, Sortie terminates every process still in its process group (its Job Object on Windows), resending that termination until no process of the group or job is still alive or a 2-second bound elapses. On Windows, a hook whose Job Object could not be created or assigned still runs, with the failure logged, and that teardown then reaches only the shell itself; Sortie then checks after the shell exits for processes descended from it that are still running, and logs the WARN record `subprocess tree did not settle` when it finds one or cannot check one. A background command inside the script ends with the hook: on Linux and macOS `&`, `nohup … &`, `( … & )`, and on Linux `systemd-run --scope`; on Windows `start /b`, `pg_ctl start`, and `pm2 start`. A process meant to outlive the hook needs a supervisor outside that tree:

  | Platform | Route | Prerequisite |
  | --- | --- | --- |
  | Linux, container engine | `docker compose up -d` or `docker run -d` | The hook's user reaches the Docker daemon: group membership or `sudo` for a rootful daemon, or `DOCKER_HOST` / a chosen context for rootless Docker. |
  | Linux, systemd user unit | `systemctl --user start`, `systemd-run --user` (without `--scope`), or `brew services start` as a non-root user | A running user manager (an active login session, or lingering enabled), and `XDG_RUNTIME_DIR` or `DBUS_SESSION_BUS_ADDRESS` in the hook's environment. |
  | Linux, systemd system unit | `systemctl start` | Sortie runs as `root`, or a polkit rule grants its user `org.freedesktop.systemd1.manage-units`. |
  | macOS, launchd | `brew services start` | The user Sortie runs as is logged in at the graphical console. |
  | macOS, Docker Desktop | `docker compose up -d` or `docker run -d` | Docker Desktop has started for the user Sortie runs as. |
  | Windows, Service Control Manager | `Start-Service` or `sc start` | The service is installed, and the account Sortie runs as holds the `SERVICE_START` right on it. |
  | Windows, Docker Desktop | `docker compose up -d` or `docker run -d` | Docker Desktop is running for the user Sortie runs as. |
  | Windows, Task Scheduler | `schtasks /run /tn <task>` or `Start-ScheduledTask` on a task the user registered | The task exists and runs in the user's own security context. |

  Prefer a supervisor that owns the service across runs, such as `docker compose up -d` on Linux and macOS, which recreates a service's containers only when its configuration or image changed, or a service manager. A hook whose next step uses the service should wait until the service accepts connections, since these start commands can return before it does. When a hook that exits on its own leaves a process running that it did not start through one of these routes, Sortie logs one INFO record, `leftover processes terminated after the command exited`, carrying `hook` and `workspace`; a termination that cannot confirm that no process of the group or job is still alive once the bound elapses logs the WARN record `subprocess group termination failed after the launch returned`. Neither WARN record carries `hook` or `workspace`: they name the launch by `command`, the shell's executable name. On Windows, a Job Object member that Sortie cannot open or query counts as still alive, not as gone.

> **Login shell environments:** If a hook requires a login shell (e.g., for `nvm`, `rbenv`, or other profile-dependent tooling), nest the invocation explicitly inside the script:
>
> ```yaml hooks: after_create: | bash -lc 'nvm use 20 && npm ci' ```
>
> The outer `sh -c` invocation is transparent — it executes the `bash -lc` command, which then sources the login profile and runs the inner script with the full environment.

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
| `SORTIE_RUN_OUTCOME`              | How the attempt ended: `"succeeded"`, `"stopped"` (the run needed input, stopped at a budget or a usage limit, or the agent declared `blocked`), `"failed"`, or `"cancelled"`. A hook that commits and pushes can skip every value other than `"succeeded"`. Sortie decides the value before it checks the run for handoff evidence, so an attempt whose handoff `tracker.handoff_evidence` then withholds reads `"succeeded"` and is recorded as failed. A cancellation that lands while the hook runs can make it read `"failed"` for an attempt recorded as cancelled. It is set with or without `--once`, and it overrides a `SORTIE_RUN_OUTCOME` in the Sortie process environment. |

These allow hooks to make decisions without parsing orchestrator internals.

**Restricted environment inheritance:**

Hook subprocesses **do not** inherit the full environment of the Sortie process. They receive a restricted environment consisting of:

- A small allowlist of standard POSIX and infrastructure variables: `PATH`, `HOME`, `SHELL`, `TMPDIR`, `USER`, `LOGNAME`, `TERM`, `LANG`, `LC_ALL`, `SSH_AUTH_SOCK`, `XDG_RUNTIME_DIR`, `DBUS_SESSION_BUS_ADDRESS`. The last two locate the user's service manager and carry no secret.
- All parent environment variables whose names start with `SORTIE_`.
- The variables injected by the orchestrator, listed in both tables above. The orchestrator's value wins over a parent variable of the same name.

All other parent variables — including secrets such as `JIRA_API_TOKEN`, `AWS_ACCESS_KEY_ID`, `GOOGLE_APPLICATION_CREDENTIALS`, and similar values — are **stripped and not available** inside hooks.

If a hook needs additional secrets or environment values, arrange for them explicitly:

- Expose the value under a `SORTIE_`-prefixed name in the Sortie process environment (for example, `SORTIE_JIRA_API_TOKEN`) and read that variable inside the hook.
- Load credentials from a file or external secrets manager inside the hook script (for example, `source /etc/sortie/hooks-env` or `aws sts get-caller-identity`).

### 6.3 Failure Semantics

| Hook            | When it runs                      | Failure behavior                                                                                            |
| --------------- | --------------------------------- | ----------------------------------------------------------------------------------------------------------- |
| `after_create`  | Workspace directory newly created | **Fatal** — aborts workspace creation. The partially-prepared directory may be removed.                     |
| `before_run`    | Before each agent attempt         | **Fatal** — aborts the current run attempt. The orchestrator treats this as a worker failure and may retry. |
| `after_run`     | After each agent attempt          | **Logged and ignored** — the run result is already determined.                                              |
| `before_remove` | Before workspace deletion         | **Logged and ignored** — cleanup still proceeds.                                                            |

Timeouts are treated the same as failures for each hook's failure semantics.

### 6.4 Inline Scripts vs File Paths

Hook values are multiline shell script strings defined inline using YAML literal block syntax (`|`). For complex hooks, consider extracting scripts to separate files and referencing them:

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

> **Note:** `after_create` runs only when Sortie first creates the per-issue workspace directory, so the directory is empty when the clone runs. If `after_create` fails, Sortie removes the directory before the next retry, so a retry also starts from an empty directory. A clone error such as "destination path already exists" or "directory not empty" does not come from this example on the normal path.
>
> Hooks also run with the restricted environment described in Section 6.2: an allowlist (including `HOME` and `SSH_AUTH_SOCK`) plus `SORTIE_*` variables. Sortie strips any variable outside that set, such as `GIT_SSH_COMMAND`, so an SSH clone must reach its key through the SSH agent (`SSH_AUTH_SOCK`) or through `~/.ssh` via `HOME`, not through a stripped variable.

> **Caveat:** Inline scripts are triple-nested (Bash in YAML in Markdown). IDEs cannot provide syntax highlighting or shell linting for inline scripts. For non-trivial logic, external scripts are more maintainable.

---

## 7. Dynamic Reload Behavior

### 7.1 General Reload Semantics

Sortie watches `WORKFLOW.md` for filesystem changes and automatically re-reads and re-applies configuration and prompt template without restart.

**Key guarantees:**

- Reloaded config applies to **future** dispatch, retry scheduling, reconciliation, hook execution, and agent launches.
- **In-flight agent sessions are not restarted** when config changes.
- **Invalid reloads do not crash** the service. Sortie continues operating with the last known good configuration. The operator-visible error is logged and reported by the `health` block of `GET /api/v1/state`, beside the time of the last successful load.
- Sortie also performs **defensive re-validation before dispatch** (per tick) in case filesystem watch events are missed.
- The file watcher monitors the parent directory to detect atomic-rename saves (vim, `sed -i`).

### 7.2 Per-Field Reload Behavior

| Field                                  | Reload behavior                                                                                |
| -------------------------------------- | ---------------------------------------------------------------------------------------------- |
| `tracker.kind`                         | Requires restart.                                                                              |
| `tracker.endpoint`                     | Requires restart.                                                                              |
| `tracker.api_key`                      | Requires restart.                                                                              |
| `tracker.project`                      | Requires restart.                                                                              |
| `tracker.api_version`                  | Requires restart.                                                                              |
| `tracker.active_states`                | Future dispatch and reconciliation, and every tracker operation that starts after the reload.  |
| `tracker.terminal_states`              | Future dispatch and reconciliation, and every tracker operation that starts after the reload.  |
| `tracker.query_filter`                 | Requires restart.                                                                              |
| `tracker.handoff_state`                | Future worker exits, not in-flight sessions.                                                   |
| `tracker.no_change_state`              | Future worker exits, not in-flight sessions.                                                   |
| `tracker.in_progress_state`            | Future dispatches, not in-flight sessions.                                                     |
| `tracker.handoff_evidence`             | Future worker attempts, not in-flight sessions.                                                |
| `tracker.comments.*`                   | Deprecated. The next event routes by the reloaded value: future dispatches (`on_dispatch`) and future worker exits (`on_completion`, `on_failure`). |
| `polling.interval_ms`                  | **Immediate** — affects future tick scheduling.                                                |
| `workspace.root`                       | Future workspace operations.                                                                   |
| `workspace.retention_days`             | Dynamic reload. Applies on the next sweep pass.                                                |
| `hooks.*`                              | Future hook executions.                                                                        |
| `hooks.timeout_ms`                     | Future hook executions.                                                                        |
| `agent.kind`                           | Future dispatches.                                                                             |
| `agent.command`                        | Future dispatches.                                                                             |
| `agent.turn_timeout_ms`                | Future worker attempts, not in-flight sessions.                                                |
| `agent.read_timeout_ms`                | Future worker attempts, not in-flight sessions.                                                |
| `agent.stall_timeout_ms`               | **Immediate**: the next poll tick's stall check applies it to in-flight sessions too.          |
| `agent.stop_grace_ms`                  | Future worker attempts, not in-flight sessions.                                                |
| `agent.max_concurrent_agents`          | **Immediate** — affects subsequent dispatch decisions.                                         |
| `agent.max_turns`                      | Future dispatches.                                                                             |
| `agent.max_retry_backoff_ms`           | **Immediate** — affects future retry scheduling.                                               |
| `agent.max_concurrent_agents_by_state` | **Immediate** — affects subsequent dispatch decisions.                                         |
| `agent.max_sessions`                   | **Immediate** — affects future retry timer evaluations.                                        |
| `agent.max_tokens`                     | **Immediate** — the poll tick carries the new ceiling to the event loop, and the first usage figure after it stops a run already over the ceiling; also affects future retry timer evaluations. |
| `agent.max_cost_usd`                   | **Immediate** – the poll tick carries the new ceiling to the event loop, and the first usage figure after it stops a run already over the ceiling; also affects future retry timer evaluations. |
| `agent.max_daily_cost_usd`             | **Immediate** – the next poll tick or retry timer fire evaluates the new ceiling, and a hold the new setting no longer reaches ends there. |
| `agent.max_weekly_cost_usd`            | **Immediate** – the next poll tick or retry timer fire evaluates the new ceiling, and a hold the new setting no longer reaches ends there. |
| `agent.max_monthly_cost_usd`           | **Immediate** – the next poll tick or retry timer fire evaluates the new ceiling, and a hold the new setting no longer reaches ends there. |
| `agent.token_warning_percent`          | **Immediate** — the poll tick carries the new threshold to the event loop; an unwarned run in flight is evaluated against it from its next usage figure, a warned run never warns again, and a run dispatched after the tick is evaluated against it from dispatch. |
| `agent.max_consecutive_absences`       | **Immediate** — affects future worker exits, retry timer evaluations, and poll-tick park sweeps. |
| `db_path`                              | **No effect** — requires restart. In-memory config updated, but database connection unchanged. |
| `reactions.<kind>.provider`                     | **No effect** — requires restart. Adapters are created once at process start.                  |
| `reactions.ci_failure.max_retries`              | Future dispatches.                                                                             |
| `reactions.ci_failure.escalation`               | Future dispatches.                                                                             |
| `reactions.ci_failure.escalation_label`         | Future dispatches.                                                                             |
| `reactions.ci_failure.max_log_lines`            | **No effect.** Requires restart. CI provider is created once at process start.                 |
| `reactions.ci_failure.triage.*`                 | **No effect.** Requires restart. The triage configuration is frozen at construction.           |
| `reactions.review_comments.*`                   | **No effect.** Requires restart. The reaction config is built once at construction.            |
| `reactions.auto_merge.*`                        | **No effect.** Requires restart. The reaction config is built once at construction.            |
| `reactions.bot_review.*`                        | **No effect.** Requires restart. The reaction config is built once at construction.            |
| `reactions.merge_conflicts.*`                   | **No effect.** Requires restart. The reaction config is built once at construction.            |
| `reactions.label_commands.*`                    | **No effect.** Requires restart. The reaction config is built once at construction.            |
| `reactions.merge_completion.*`                  | **No effect.** Requires restart. The reaction config, `target_state` included, is built once at construction. |
| `claude-code.*`, `codex.*`, `copilot-cli.*`, `opencode.*`, `agent-client-protocol.*` | Future worker attempts, not in-flight sessions. Each attempt resolves its kind's block when it starts. |
| `dispatch.rules[].next`                | Future worker exits, not in-flight sessions. A reload that removes `next`, or the rule it names, makes the next exit take the handoff write. |
| `dispatch.max_consecutive_hops`        | Future worker exits, not in-flight sessions. The count already kept for an issue is unchanged. |
| `dispatch.partials`                    | Future worker attempts, not in-flight sessions. Sortie reads every partial again on every load and does not watch the files, so an edit to a partial applies at the next tick's defensive reload, or at once when `WORKFLOW.md` is touched. |
| `dispatch.rules[].<kind>` (rule settings block) | Future worker attempts of claims that hold the rule, not in-flight sessions. The rule's own selection (match, stage, agent, template) reaches future claims only. |
| `notifications` (agent messages)       | Future sessions. The `sortie mcp-server` sidecar re-reads `WORKFLOW.md` at each session start, so backend and cap changes apply to sessions started after the reload, not to in-flight sessions. |
| `notifications` (events Sortie produces) | The next event. Sortie routes each event against the configuration in force when it decides to send it, so a change to `events` or to a destination applies to the next event and never to one already routed. A reload whose destinations for these events cannot be built, such as an unknown `kind` or a required secret that resolved to an empty string, is rejected and the previous configuration stays in force. The `escalation: comment` of a reaction other than `ci_failure` keeps the value read at startup. |
| `server.port`                          | **No effect** — requires restart.                                                              |
| `server.host`                          | **No effect** — requires restart.                                                              |
| `logging.level`                        | **No effect** — requires restart.                                                              |
| Prompt template                        | Future worker attempts (including continuation retries), not in-flight continuation turns.     |

**Environment variable overrides and reload:** On each WORKFLOW.md reload, `SORTIE_*` environment variables and the `.env` file are re-read and merged into the fresh config. `.env` file changes are picked up on reload. Real environment variable changes require a process restart (standard Unix process semantics). See [Section 3.7](#37-dynamic-reload) for details.

---

## 8. Dispatch Preflight Validation

Before dispatching work, the orchestrator validates the workflow configuration. This runs at two points:

**Startup validation:** Before starting the scheduling loop. If validation fails, startup is aborted with an operator-visible error.

**Per-tick validation:** Before each dispatch cycle. If validation fails, dispatch is skipped for that tick, reconciliation remains active, and an error is emitted.

**Validation checks:**

| Check                                          | Error condition                                             |
| ---------------------------------------------- | ----------------------------------------------------------- |
| Workflow file loadable and parseable           | File missing, YAML syntax error, or non-map front matter.   |
| `tracker.kind` present and supported           | Missing, empty, or unregistered adapter.                    |
| `tracker.api_key` present after `$` resolution | Missing or empty when the adapter requires it (e.g., Jira). |
| `tracker.project` present                      | Missing when the adapter requires project scoping.          |
| `agent.command` names a program for every kind a selection reaches | A kind reached through `agent.kind`, `dispatch.default.agent`, or a rule's `agent` launches a local command but would run nothing: it is the default kind with neither `agent.command` nor a default command, or another kind with no default command. The message names the selector. |
| Tracker adapter registered and available       | No adapter registered for the configured `tracker.kind`.    |
| Agent adapter registered and available         | No adapter registered for the configured `agent.kind`.      |
| `workspace.root` writable                      | The resolved root cannot be created, or a probe file cannot be written inside it. Reported under check `workspace.root_writable`. |
| `dispatch` is a map; `dispatch.rules` is a sequence; `dispatch.default` is a map | Wrong YAML node type for `dispatch`, `dispatch.rules`, or `dispatch.default`. |
| Each rule has at least one of `match`, `stage`, `agent`, `template`, or a settings block | A rule map carries none of them; `next` does not count. |
| Every `stage` value is a label | A `stage` value is not a string, or is null, empty, or only white space. The errors and their fields are listed in [Section 9.2](#92-configuration-errors). |
| A rule with `stage` has no `match` and has a `name` | A rule carries `stage` beside `match`, or carries `stage` and no `name`. |
| No duplicate stage label | Two rules carry stage labels that differ at most in case. |
| Rule `name`, when present, matches `^[a-z][a-z0-9_-]*$` | Malformed rule name. |
| No duplicate rule name | Two rules share the same non-empty `name`. |
| No non-final catch-all (`unreachable_rules`) | A rule with no `match` block and no `stage` precedes a rule without `stage`. A rule with `stage` may follow a catch-all. |
| Every `next` is a rule name | A `next` value is not a string, or is null, empty, or only white space. The errors and their fields are listed in [Section 9.2](#92-configuration-errors). |
| A rule with `next` has a `name` | A rule carries `next` and no `name`. |
| Every `next` names another rule that carries `stage` | `next` names no rule, names the rule that carries it, or names a rule without `stage`. |
| No cycle of `next` links | Following `next` from a rule returns to a rule already on the path. The error shows the path of the loop, starting at its first rule in list order. |
| `dispatch.max_consecutive_hops` is an integer, greater than `0`, at least the longest chain's hops | The value is not an integer, is zero or negative, or is below the number of hops in the longest chain. The error shows that chain. |
| `next` requires `tracker.handoff_state` | A rule carries `next` while `handoff_state` is empty. Checked entirely offline, with no tracker call. |
| Every `match` key recognized | A `match` key is not one of `labels`, `issue_type`, `priority`, `identifier`, `assignee`, `title`. |
| Every `title` key lists phrases | A `title` key is null, bare, or an empty list; its value is not a string or a list; a list element is not a string; or a phrase is empty or only white space. The errors and their fields are listed in [Section 9.2](#92-configuration-errors). |
| `priority` predicate has exactly one operator | Zero or more than one of `eq`, `in`, `lt`, `lte`, `gt`, `gte`. |
| Glob patterns syntactically valid | A `labels` or `identifier` pattern fails `path.Match`. |
| Every referenced `agent` kind registered | `dispatch.rules[*].agent` or `dispatch.default.agent` names an unregistered adapter. |
| Every rule settings block well formed | A block is named for a kind other than the one the rule runs, is not a map, writes `kind`, `command`, or one of the four `agent` timeouts, belongs to a rule with no `name` or named `default`, or a key of `dispatch.default` names an agent kind. The errors and their fields are listed in [Section 9.2](#92-configuration-errors). |
| `dispatch.stage.collision` | A rule's stage label equals, ignoring case, a state or a label Sortie applies to issues: an active or terminal state, `tracker.handoff_state`, `tracker.in_progress_state`, `tracker.no_change_state`, a reaction's `escalation_label`, or the parking label. The message names the rule, the stage label, and what it collides with, and one error is reported for each rule and each collision. |
| `dispatch.agent.missing_block` | A kind other than `agent.kind` that `dispatch.default.agent` or a rule's `agent` names has a top-level block that is not a map, or has no top-level block while some selector of the kind is not a rule carrying the kind's block. The message names the first such selector. |
| Every rule's resolved settings block passes the adapter checks | The top-level block of the rule's kind with the rule's block laid over it fails a check the top-level block would fail: key types, the adapter's own checks, `agent.kind.session_resume`, and conflicts between keys such as `opencode.allowed_tools.overlap` and `opencode.effort.conflict`. The message opens with `dispatch rule "<name>" (dispatch.rules[<i>].<kind>): `, and the check key is the one the top-level block draws. |
| Every per-rule template path resolvable and parseable | Path is absolute, `~`-prefixed, escapes the workflow tree, is not a regular file, is unreadable, or fails template parse. |
| Every `dispatch.partials` entry resolvable | `dispatch.partials` is not a list; an entry is not a string, is only white space, is absolute, is `~`-prefixed, or leaves the workflow tree; a pattern is malformed or matches no file; an explicit entry names a missing file; a selected file resolves outside the tree after symlink evaluation or is not a regular file. The errors and their fields are listed in [Section 9.2](#92-configuration-errors). |
| Every partial loadable | A partial is unreadable, carries front matter, has a syntax error, holds text or an action outside a `define` block, defines the reserved name `prompt`, or defines a name another partial already defines. The message names the partial file and the line. |
| Every prompt template free of name clashes and undefined calls | A prompt template defines a name a partial also defines, or calls a name that its partials and the template itself do not define, in any branch of an `{{ if }}`, `{{ range }}`, or `{{ with }}`. The message names the file and the line of the call, and the template that reaches it when the call sits in a partial. |
| `tracker.handoff_state` and `tracker.in_progress_state` free of collisions against the state lists | The state collides with `active_states` or `terminal_states` as written. |
| `tracker.no_change_state` requires `tracker.handoff_state` | `no_change_state` is set while `handoff_state` is empty. Checked entirely offline, with no tracker call. |
| `tracker.no_change_state` names a permitted value | `no_change_state` is neither equal to `handoff_state` nor a member of `terminal_states` exactly as written in front matter. Checked entirely offline, with no tracker call. |
| `price_table.replace_conflict` | `price_table.replace_file` and `price_table.replace_url` are both set. |
| `price_table.fetch_url` | `price_table.fetch_url` is set through a `$VAR` that resolves to an empty string. |
| `price_table.replace_url` | `price_table.replace_url` is set through a `$VAR` that resolves to an empty string. |
| `price_table.replace_file` | `price_table.replace_file` is not a string, or is set through a `$VAR` that resolves to an empty string, or names a file that cannot be read or fails the price-table validation: it does not parse as a price table, a rate is not a finite non-negative number, or no entry carries a rate. |

An empty `tracker.active_states` draws the warning `tracker.active_states.empty` (`tracker.active_states is empty, so no issue is dispatched`) and does not block startup.

**Rule settings blocks in preflight.** Every adapter check that reads a settings block runs once for the top-level block of each kind the configuration reaches and again for each rule's resolved block, so a fault a rule inherits from the top-level block is reported for the top-level block and again for each rule that inherits it. A kind other than `agent.kind` whose every selector is a rule carrying that kind's block needs no top-level block, and its top-level block alone is not checked, because no attempt reads it alone. The same checks run for each attempt when it starts: an attempt whose resolved block fails an error-severity check starts no session (see [Freeze-on-dispatch](#freeze-on-dispatch)), which keeps a reload from carrying a bad block into a retry or a continuation that never passes the per-tick preflight.

**Advisory warnings vs. configuration errors:** An unknown key placed directly under `dispatch` (alongside `partials`, `rules`, `default`, and `max_consecutive_hops`) produces an `unknown_sub_key` advisory warning and does not block startup. Unknown keys nested deeper are rejected as configuration errors that fail the load: an unrecognized key inside a rule map (`dispatch.rules[*]`) that does not name a registered agent kind, inside `dispatch.default`, or inside a `match` block. The key that names a rule's own agent kind is its settings block and draws no `unknown_sub_key` warning. The asymmetry matters: a typo like `lables:` inside `match`, or a stray key on a rule, is caught as an error so it cannot silently disable a rule, while a typo at the top `dispatch` level is flagged as a warning without preventing startup.

**Template analysis in `sortie validate`.** `sortie validate` analyzes every prompt template of the workflow: the Markdown body, the default template, and each rule template. The analysis follows a call into a define block when the call passes the root data (`.` outside every `range` and `with` body, or `$`), so a variable that a partial reads and the data does not hold is reported as `unknown_var`. Each template warning starts with the file and the line that hold it, in the form `<file> (line <n>): <message>`, and a warning that sits in a partial names the partial. A define block of a partial that no prompt template calls draws an `unused_partial` warning, one for each such block. Template warnings never change the result or the exit status. A workflow that fails to load reports the load error and no template warnings.

**Adapter-specific tracker diagnostics.** A tracker adapter may contribute its own offline checks, which run during the same preflight without any network call. An `error`-severity diagnostic blocks dispatch like any other preflight error; a `warning`-severity diagnostic is advisory and does not block startup. The Linear adapter (`kind: linear`) emits:

| Check                                            | Severity | Message                                                                                                                                            |
| ------------------------------------------------ | -------- | -------------------------------------------------------------------------------------------------------------------------------------------------- |
| `tracker.project.format`                         | error    | `tracker.project must be a Linear team key with no whitespace (e.g. "ENG")`                                                                         |
| `tracker.project.format`                         | warning  | `tracker.project is a Linear team key (e.g. "ENG"), not an owner/repo path; the "/" looks like a GitHub-style value`                                |
| `tracker.api_key.sortie_linear_api_key_hint`     | warning  | `tracker.api_key is empty but SORTIE_LINEAR_API_KEY environment variable is set; consider using api_key: $SORTIE_LINEAR_API_KEY`                    |
| `tracker.api_key.sortie_linear_api_key_missing`  | warning  | `tracker.api_key is empty and SORTIE_LINEAR_API_KEY environment variable is not set`                                                                |
| `tracker.api_key.linear_whitespace`              | warning  | `tracker.api_key has leading or trailing whitespace; the key is sent verbatim in the Authorization header, so surrounding whitespace will fail authentication` |
| `tracker.api_key.linear_prefix`                  | warning  | `tracker.api_key does not start with "lin_api_"; Linear personal API keys carry that prefix`                                                        |
| `tracker.active_states.untrimmed_element` / `tracker.terminal_states.untrimmed_element` | error | `tracker.active_states[<i>]: state name has leading or trailing whitespace and can never match a team state` (same shape for `terminal_states`) |
| `tracker.states.overlap`                         | warning  | `tracker.active_states and tracker.terminal_states overlap on "<name>"; an issue in state "<name>" would match both sets`                           |

A `handoff_state` or `in_progress_state` that collides with `active_states` or `terminal_states` is not an adapter diagnostic. The generic config validation rejects it for every `tracker.kind` before the adapter hook runs, reporting it as a `config.tracker.*` error. Both rules compare against the state lists as written.

These offline checks never contact Linear and never log the API key value. State-name existence against the team and credential validity are checked by the online preflight at adapter construction, not by `sortie validate`.

**Configuration advisories.** A configuration advisory is recorded once, when the configuration that draws it is built or loaded, and never printed at that point. Every advisory reaches an operator through the same three surfaces: `sortie validate` reports it once as a `warning` diagnostic and never fails the run for it; the running orchestrator logs it once at the tick that first draws it, and again only after a tick whose configuration did not draw it; a dry run logs it once, before it fetches candidate issues. `sortie mcp-server` never prints an advisory.

| Check                                                     | Condition                                                                                          |
| ---------------------------------------------------------- | ---------------------------------------------------------------------------------------------------- |
| `reactions.label_commands.poll_interval_ms.clamped`         | `reactions.label_commands` names a provider and its `poll_interval_ms` is below 30000.               |
| `env_file.missing`                                          | An env file path is set (`--env-file` or `SORTIE_ENV_FILE`) and no file exists there.                |
| `env_override.section_replaced`                             | A `SORTIE_*` override targets a section whose value is present and not a mapping.                    |
| `dispatch.next.max_sessions`                                | `agent.max_sessions` is above `0` and below the number of runs the longest stage chain needs. The text names both numbers and the chain; an issue on the chain exhausts its session budget before the chain ends. |
| `agent.kind.deprecated`                                     | A kind the configuration reaches (`agent.kind`, `dispatch.default.agent`, or a `dispatch.rules[*].agent`) is registered as deprecated. |
| `agent.kind.retired`                                        | A kind the configuration reaches (`agent.kind`, `dispatch.default.agent`, or a `dispatch.rules[*].agent`) was removed and converted onto its replacement kind. The warning names the converted fields, the launch the replacement kind performs, and the settings not carried, and states that the conversion is temporary. |
| `agent.effort.not_forwarded`                                | A kind the configuration reaches whose adapter passes no reasoning level (`agent-client-protocol`, `mock`) has a non-empty `effort` in its settings block, or a value of another type there. A rule's settings block for such a kind whose resolved `effort` is non-empty draws it again, with the text opening `dispatch rule "<name>" (dispatch.rules[<i>].<kind>)`. |
| `agent.effort.inherited`                                    | A rule's settings block writes `model` (`null` included), does not write `effort`, and inherits a non-empty `effort` from its kind's top-level block. Level names depend on the model; write `effort` in the rule to choose the level for its model, or `effort: null` to clear it. |
| `tracker.comments.on_dispatch.deprecated`                   | `tracker.comments.on_dispatch` resolves to `true`, from the file or from `SORTIE_TRACKER_COMMENTS_ON_DISPATCH`. |
| `tracker.comments.on_completion.deprecated`                 | `tracker.comments.on_completion` resolves to `true`, from the file or from `SORTIE_TRACKER_COMMENTS_ON_COMPLETION`. |
| `tracker.comments.on_failure.deprecated`                    | `tracker.comments.on_failure` resolves to `true`, from the file or from `SORTIE_TRACKER_COMMENTS_ON_FAILURE`. |
| `reactions.<key>.escalation.deprecated`                     | A reaction among `ci_failure`, `review_comments`, `bot_review`, `merge_conflicts`, `auto_merge`, and `merge_completion` names a provider and sets `escalation: comment`. |
| `notifications.tracker_comment.implicit_auto_merge`         | `tracker.kind` is set, no `notifications` entry has kind `tracker_comment`, and `reactions.auto_merge` names a provider. |
| `notifications.tracker_comment.implicit_budget_hold`        | `tracker.kind` is set, no `notifications` entry has kind `tracker_comment`, and `agent.max_sessions`, `agent.max_tokens`, or `agent.max_cost_usd` is above `0`. |
| `notifications[<i>].events.missing`                         | A `notifications` entry other than `tracker_comment` omits `events`. |
| `token_rates`                                               | `token_rates` or one of its entries is not a map, an entry has an empty key, a rate is not a finite, non-negative number, an entry holds a key other than its four `*_per_mtok` rates, or a kind entry lacks `input_per_mtok` or `output_per_mtok`. |
| `token_rates.per_kind`                                      | A `token_rates` entry is keyed by an agent kind. It prices every model of that kind alike, so it turns off per-model pricing for the kind. |
| `token_rates.model_incomplete`                              | A `token_rates` entry keyed by a model that the price table the configuration selects (your own table, else the embedded table) does not list leaves one of `input_per_mtok`, `output_per_mtok`, `cache_read_per_mtok`, and `cache_write_per_mtok` unset. The text names the unset rates, and the entry prices the classes it sets. |

**Money ceiling warnings.** The preflight warnings below apply to each money setting that is above `0`: `agent.max_cost_usd`, `agent.max_daily_cost_usd`, `agent.max_weekly_cost_usd`, and `agent.max_monthly_cost_usd`. None fires for a setting at `0`. All are warnings: `sortie validate` prints them and exits `0`, and none changes `valid`. The running service logs them and keeps dispatching, including after a live reload that sets or raises a ceiling. Under either condition the per-issue ceiling holds no issue whose figure stays unmeasured or unpriced, and an instance ceiling counts none of the spend it cannot measure or price.

| Check | Condition |
| ----- | --------- |
| `agent.max_cost_usd.no_usage_reporting` | A kind the configuration reaches reports no token usage for the sessions the configuration produces, so its sessions never carry the issue to the ceiling. The message names the kind and the setting. It is reported for the top-level settings of each kind and for each rule's resolved settings block. |
| `agent.max_daily_cost_usd.no_usage_reporting` | The same condition under `agent.max_daily_cost_usd`: the daily spend ceiling counts none of the kind's spend. |
| `agent.max_weekly_cost_usd.no_usage_reporting` | The same condition under `agent.max_weekly_cost_usd`: the weekly spend ceiling counts none of the kind's spend. |
| `agent.max_monthly_cost_usd.no_usage_reporting` | The same condition under `agent.max_monthly_cost_usd`: the monthly spend ceiling counts none of the kind's spend. |
| `agent.max_cost_usd.no_rate_source` | No price source is configured: `price_table.embedded` and `price_table.fetch` are off, and no `token_rates` entry or operator price table is set, so no run can be priced. |
| `agent.max_daily_cost_usd.no_rate_source` | The same condition under `agent.max_daily_cost_usd`. |
| `agent.max_weekly_cost_usd.no_rate_source` | The same condition under `agent.max_weekly_cost_usd`. |
| `agent.max_monthly_cost_usd.no_rate_source` | The same condition under `agent.max_monthly_cost_usd`. |

A model that no configured layer prices draws no preflight warning; its runs are recorded `unpriced`, the issue's figure becomes a lower bound, and the service logs that the figure is incomplete.

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

These errors are raised during typed config construction from the parsed front matter. Each error identifies the offending field path.

| Error pattern                                                                   | Cause                                                                    | Fix                                                                                                                                  |
| ------------------------------------------------------------------------------- | ------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------ |
| `config: polling.interval_ms: invalid integer value: <val>`                     | Non-integer value for a field expecting an integer.                      | Use a plain integer (e.g., `30000`) or a quoted string integer (e.g., `"30000"`). Remove units, decimals, or non-numeric characters. |
| `config: agent.max_concurrent_agents: invalid integer value: <val>`             | Same as above, for any integer field.                                    | Same fix as above.                                                                                                                   |
| `config: agent.stall_timeout_ms: invalid integer value: <val>`                  | Same as above.                                                           | Same fix.                                                                                                                            |
| `config: <field>: value is outside the range an integer setting accepts, -9223372036854775808 to 9223372036854775807` | An integer too large or too small for Sortie to hold, in any integer field. | Use a value within the range. The field's own limits still apply.                                                                   |
| `config: agent.max_sessions: must be non-negative`                              | Negative value for `max_sessions`.                                       | Use `0` (unlimited) or a positive integer.                                                                                           |
| `config: agent.max_cost_usd: invalid number value: <val>`                       | Value for `max_cost_usd` that is not a number. | Use an integer or decimal (e.g., `5` or `2.5`) or a quoted string of one (e.g., `"2.5"`). |
| `config: agent.max_cost_usd: must be a finite number`                           | `NaN` or an infinity for `max_cost_usd`.                                 | Use `0` (unlimited) or a positive number.                                                                                            |
| `config: agent.max_cost_usd: must be non-negative`                              | Negative value for `max_cost_usd`.                                       | Use `0` (unlimited) or a positive number.                                                                                            |
| `config: agent.max_daily_cost_usd: invalid number value: <val>` | Value for `max_daily_cost_usd` that is not a number. | Use an integer or decimal (e.g., `5` or `2.5`) or a quoted string of one (e.g., `"2.5"`). |
| `config: agent.max_daily_cost_usd: must be a finite number` | `NaN` or an infinity for `max_daily_cost_usd`. | Use `0` (unlimited) or a positive number. |
| `config: agent.max_daily_cost_usd: must be non-negative` | Negative value for `max_daily_cost_usd`. | Use `0` (unlimited) or a positive number. |
| `config: agent.max_weekly_cost_usd: invalid number value: <val>` | Value for `max_weekly_cost_usd` that is not a number. | Use an integer or decimal (e.g., `5` or `2.5`) or a quoted string of one (e.g., `"2.5"`). |
| `config: agent.max_weekly_cost_usd: must be a finite number` | `NaN` or an infinity for `max_weekly_cost_usd`. | Use `0` (unlimited) or a positive number. |
| `config: agent.max_weekly_cost_usd: must be non-negative` | Negative value for `max_weekly_cost_usd`. | Use `0` (unlimited) or a positive number. |
| `config: agent.max_monthly_cost_usd: invalid number value: <val>` | Value for `max_monthly_cost_usd` that is not a number. | Use an integer or decimal (e.g., `5` or `2.5`) or a quoted string of one (e.g., `"2.5"`). |
| `config: agent.max_monthly_cost_usd: must be a finite number` | `NaN` or an infinity for `max_monthly_cost_usd`. | Use `0` (unlimited) or a positive number. |
| `config: agent.max_monthly_cost_usd: must be non-negative` | Negative value for `max_monthly_cost_usd`. | Use `0` (unlimited) or a positive number. |
| `config: agent.max_tokens: must be non-negative`                                | Negative value for `max_tokens`.                                         | Use `0` (unlimited) or a positive integer.                                                                                           |
| `config: agent.token_warning_percent: must be between 0 and 99`                 | Value for `token_warning_percent` below `0` or above `99`.               | Use an integer from `0` (off) to `99`.                                                                                               |
| `config: agent.max_consecutive_absences: must be greater than 0`                | `0` or a negative value for `max_consecutive_absences`.                  | Use a positive integer, or remove the key to take the default of `3`.                                                                |
| `config: agent.max_consecutive_absences: invalid integer value: <val>`          | Non-integer value for `max_consecutive_absences`.                        | Use a plain integer (e.g., `3`) or a quoted string integer (e.g., `"3"`).                                                            |
| `config: price_table: expected map, got <type>` | `price_table` is not a map. | Write `price_table` as a map, or remove it. |
| `config: price_table.embedded: expected boolean, got <type>` | `price_table.embedded` is not a boolean. | Write `true` or `false`. |
| `config: price_table.fetch: expected boolean, got <type>` | `price_table.fetch` is not a boolean. | Write `true` or `false`. |
| `config: price_table.fetch_url: expected string, got <type>` | `price_table.fetch_url` is not a string. | Write the address as a string, quoted if necessary. |
| `config: price_table.replace_url: expected string, got <type>` | `price_table.replace_url` is not a string. | Write the address as a string, quoted if necessary. |
| `config: price_table.replace_url: resolved to empty (check environment variable)` | `price_table.replace_url` is set through a `$VAR` that resolves to an empty string. | Set the variable, or remove the key. |
| `config: price_table.fetch_url: resolved to empty (check environment variable)` | `price_table.fetch_url` is set through a `$VAR` that resolves to an empty string. Sortie does not use the default address instead. | Set the variable, or remove the key to take the default address. |
| `config: price_table.replace_file: resolved to empty (check environment variable)` | `price_table.replace_file` is set through a `$VAR` that resolves to an empty string. Sortie does not price from the embedded table instead. | Set the variable, or remove the key. |
| `config: price_table: replace_file and replace_url are mutually exclusive` | Both `price_table.replace_file` and `price_table.replace_url` are set. | Keep the one that names your table. |
| `config: price_table.replace_file: <reason>` | The value is not a string (`expected string, got <type>`), the file cannot be read, does not parse as a price table, holds a rate that is not a finite non-negative number, or carries no rate in any entry. | Write the path as a string, or fix the path or the file, in the format of the published LiteLLM `model_prices_and_context_window.json`. |
| `config: agent.kind: expected string, got <type>`                               | `agent.kind` is not a string (e.g., integer, boolean, list).             | Ensure the value is a string, quoted if necessary.                                                                                   |
| `config: agent.command: expected string or list, got <type>`                    | `agent.command` is neither a string nor a list (e.g., integer, boolean, mapping). | Write the value as a string, quoted if necessary, or as a list of strings.                  |
| `config: agent.command: must not be an empty list`                              | `agent.command` is a list with no elements.                              | Add the program as the first element, or remove the key to take the kind's default command.          |
| `config: agent.command[<i>]: expected string, got <type>`                       | Element `<i>` of an `agent.command` list is not a string.                | Write every element as a string, quoted if necessary.                                                |
| `config: agent.command[<i>]: must not be empty`                                 | Element `<i>` of an `agent.command` list is empty; element `0` may not be only whitespace. | Give the element a value, or remove it. Element `0` names the program.                 |
| `config: <kind>.<key>: agent kind "<kind>" was removed and this configuration cannot be converted to agent kind "<replacement>": <reason>` | The workflow names a removed agent kind, and `<key>` in that kind's settings block has a value the replacement kind cannot carry. `sortie validate` reports it as `config.<kind>.<key>`. | Fix the setting as the reason states, or name the replacement kind in the workflow file. |
| `config: <field>: agent kinds "<kind>" and "<kind>" were removed and convert to agent kind "<replacement>" with different commands` | Two removed kinds in one workflow convert onto the same replacement kind and would launch different commands. | Name the replacement kind in the workflow file with one `agent.command`.                    |
| `config: dispatch.rules[<i>].<kind>.<key>: agent kind "<kind>" was removed and this configuration cannot be converted to agent kind "<replacement>": <reason>` | A rule runs a removed agent kind and its settings block for that kind has a value the replacement kind cannot carry. | Fix the setting as the reason states, or name the replacement kind in the rule. |
| `config: dispatch.rules[<i>].<kind>: agent kind "<kind>" was removed and this rule's settings cannot be converted to agent kind "<replacement>": they change the command the replacement kind launches, which a dispatch rule cannot set` | A rule's block for a removed kind converts to a launch command other than the one the kind's own conversion produces. | Remove the setting that changes the command, or name the replacement kind in the workflow file with the launch you want in `agent.command`. |
| `config: dispatch.rules[<i>]: rule must specify at least one of match, stage, agent, template, or a settings block` | A rule map carries none of those keys; `next` alone does not count. | Add one of them. |
| `config: dispatch.rules[<i>].stage: expected a label, got <shape>` | The `stage` value is not text. `<shape>` is `a number`, `a true/false value`, `a list`, `a map`, or `a value of an unexpected type`. | Write the label as text, quoted if necessary. |
| `config: dispatch.rules[<i>].stage: needs a label with a character other than white space` | The `stage` value is null, a bare `stage:` line, empty, or only white space. | Write a label, or remove the key. |
| `config: dispatch.rules[<i>]: a rule with a stage label is selected by that label and cannot also carry match` | A rule carries `stage` and a `match` block, an empty one included. | Remove `match`, or remove `stage` and select the rule by `match`. |
| `config: dispatch.rules[<i>]: a rule that carries a stage label must have a name` | A rule carries `stage` and no `name`. | Give the rule a `name`. |
| `config: dispatch.rules[<j>].stage: duplicate stage label "<label>" (first at index <i>)` | Rule `<j>` carries a stage label equal, ignoring case, to the label of rule `<i>`. | Give each rule its own label. |
| `config: dispatch.rules[<i>].next: expected a rule name, got <shape>` | The `next` value is not text. `<shape>` is `a number`, `a true/false value`, `a list`, `a map`, or `a value of an unexpected type`. | Write the name of a rule as text, quoted if necessary. |
| `config: dispatch.rules[<i>].next: needs a rule name` | The `next` value is null, a bare `next:` line, empty, or only white space. | Write the name of a rule, or remove the key. |
| `config: dispatch.rules[<i>]: a rule that carries next must have a name` | A rule carries `next` and no `name`. | Give the rule a `name`. |
| `config: dispatch.rules[<i>].next: next "<value>" names no dispatch rule` | No rule has the name `<value>`. | Write the `name` of a rule in `dispatch.rules`. |
| `config: dispatch.rules[<i>].next: next names the rule that carries it` | A rule names itself. | Name the rule that follows it. |
| `config: dispatch.rules[<i>].next: next "<value>" names a rule without a stage label; the rule a next names must carry stage` | The rule `next` names has no `stage`. | Add `stage` to the named rule. |
| `config: dispatch.rules[<s>].next: next links form a cycle: <a> -> <b> -> <a>` | Following `next` leads back to a rule already on the path. `<s>` is the first rule of the loop in list order, and the path starts at it. | Remove one `next` of the loop. |
| `config: dispatch.max_consecutive_hops: invalid integer value: <val>` | Non-integer value. | Use a plain integer, or remove the key for the default. |
| `config: dispatch.max_consecutive_hops: must be greater than 0` | `0` or a negative value. | Use a positive integer, or remove the key for the default. |
| `config: dispatch.max_consecutive_hops: must be at least <n>, the number of hops in the longest stage chain (<path>)` | The value is below the hops of the longest chain, `<path>`. | Raise the value to at least `<n>`, or shorten the chain. |
| `config: dispatch.rules[<i>].next: next requires tracker.handoff_state, the state a stage ends on when a hop is not made` | A rule carries `next` and `tracker.handoff_state` is unset. | Set `tracker.handoff_state`. |
| `config: dispatch.rules[<i>].<key>: settings block for agent kind "<key>", but this rule runs agent kind "<kind>"` | A rule carries a block named for a kind other than the one it runs. The message ends with `, taken from dispatch.default.agent` or `, taken from agent.kind` when the rule has no `agent`. It also appears after `agent.kind`, `dispatch.default.agent`, or a rule's `agent` changed under an existing block. | Name the block for the rule's kind, or set the rule's `agent` to the block's kind. |
| `config: dispatch.rules[<i>].<kind>: a rule's settings block must hold the kind's settings as keys, got <shape>; write {} for an empty block` | A rule's block is not a map. `<shape>` is `no value` for a bare `<kind>:` line. | Write the settings as keys under the block, or `{}` for an empty block. |
| `config: dispatch.rules[<i>].<kind>.kind: a rule chooses its agent kind with its agent key` | A rule's block writes `kind`. | Remove the key and set the rule's `agent`. |
| `config: dispatch.rules[<i>].<kind>.command: agent.command belongs to the default agent kind; a dispatch rule cannot set a command` | A rule's block writes `command`. | Remove the key. |
| `config: dispatch.rules[<i>].<kind>.<key>: agent.<key> is workflow-wide; a dispatch rule cannot override it` | A rule's block writes `turn_timeout_ms`, `read_timeout_ms`, `stall_timeout_ms`, or `stop_grace_ms`. | Remove the key and set the limit in the `agent` section. |
| `config: dispatch.rules[<i>]: a rule that carries a settings block must have a name` | A rule carries a settings block and no `name`. | Give the rule a `name`. |
| `config: dispatch.rules[<i>].name: "default" is the name run history and statistics give the dispatch.default selection; a rule that carries a settings block must use another name` | A rule named `default` carries a settings block. | Rename the rule. |
| `config: dispatch.rules[<i>].match.title: needs at least one phrase; remove the key to leave the title out of the match` | The `title` key is null, bare, or an empty list. | Write one phrase or a list of phrases, or remove the key to leave the title out of the match. |
| `config: dispatch.rules[<i>].match.title: expected a phrase or a list of phrases, got <shape>` followed by `; quote the phrase` (`; quote a phrase that contains ": ", as in "fix: typo"` for a map) | The `title` value is not text or a list. `<shape>` is `a number`, `a true/false value`, `a map`, or `a value of an unexpected type`, such as an unquoted date. | Quote the phrase, as in `title: "404"`. |
| `config: dispatch.rules[<i>].match.title[<j>]: expected a phrase, got <shape>` followed by a quoting hint | Element `<j>` of a `title` list is not text. `<shape>` is `a list` for an unquoted `- [infra]` (hint `; quote a phrase that starts with "[", as in "[infra]"`), `a map` for an unquoted `- fix: typo` (hint `; quote a phrase that contains ": ", as in "fix: typo"`), `a number`, `a true/false value`, or `a value of an unexpected type` (hint `; quote the phrase`), or `no value` for a bare `-` (no hint). | Quote the phrase, or remove the element. |
| `config: dispatch.rules[<i>].match.title[<j>]: a phrase needs a character other than white space` | Phrase `<j>` of a `title` value is empty or only white space; a single phrase reports `title[0]`. | Write a phrase with a visible character, or remove it. |
| `config: dispatch.default.<key>: dispatch.default carries no settings block; the top-level <key> block holds the default settings` | A key of `dispatch.default` names an agent kind. | Move the settings to the top-level block of that kind. |
| `config: tracker.kind: expected string, got <type>`                             | `tracker.kind` is not a string (e.g., integer, boolean, list).           | Ensure the value is a string, quoted if necessary.                                                                                   |
| `config: tracker.endpoint: expected string, got <type>`                         | `tracker.endpoint` is not a string (e.g., integer, boolean, list).       | Ensure the value is a string, quoted if necessary.                                                                                   |
| `config: tracker.api_key: expected string, got <type>`                          | `tracker.api_key` is not a string (e.g., integer, boolean, list).        | Ensure the value is a string, quoted if necessary.                                                                                   |
| `config: tracker.project: expected string, got <type>`                          | `tracker.project` is not a string (e.g., integer, boolean, list).        | Ensure the value is a string, quoted if necessary.                                                                                   |
| `config: tracker.query_filter: expected string, got <type>`                     | `tracker.query_filter` is not a string (e.g., integer, boolean, list).   | Ensure the value is a string, quoted if necessary.                                                                                   |
| `config: tracker.api_version: expected string, got <type>`                      | `tracker.api_version` is a type other than string and other than a whole number (e.g., boolean, list). A bare whole number (e.g., `2`) is coerced to its decimal string instead of raising this error. | Ensure the value is a string or a whole number, quoted if necessary. |
| `config: <field>: expected string, got timestamp`                               | An unquoted date (e.g., `2026-01-01`) decodes to a YAML timestamp rather than a string. | Quote the value (e.g., `"2026-01-01"`) so it decodes as a string.                                                        |
| `config: tracker.handoff_state: expected string, got <type>`                    | `handoff_state` is not a string (e.g., integer, boolean, list).          | Ensure the value is a string, quoted if necessary.                                                                                   |
| `config: tracker.handoff_state: must not be empty`                              | `handoff_state` is set to an explicit empty string.                      | Provide a valid state name, or omit the field entirely to disable handoff.                                                           |
| `config: tracker.terminal_states: must name at least one state for tracker kind "<kind>": a closed issue reads as its first entry` | `tracker.kind` is `github`, `gitea`, or `gitlab` and `terminal_states` is empty. | Name at least one terminal state. A reload that empties the list keeps the previous configuration. |
| `config: tracker.active_states[<i>]: needs a state name with a character other than white space` | An entry of `active_states` is empty or only white space (same shape for `terminal_states`). | Remove the entry or write a state name. |
| `config: tracker.handoff_state: resolved to empty (check environment variable)` | `$VAR` reference resolved to an empty string (variable unset or empty).  | Set the referenced environment variable to a valid state name.                                                                       |
| `config: tracker.handoff_state: "<val>" collides with active state "<state>"`   | `handoff_state` matches one of the `active_states` (case-insensitive).   | Use a state that is not in `active_states`. The handoff state must be distinct from active and terminal states.                      |
| `config: tracker.handoff_state: "<val>" collides with terminal state "<state>"` | `handoff_state` matches one of the `terminal_states` (case-insensitive). | Use a state that is not in `terminal_states`.                                                                                        |
| `config: tracker.no_change_state: expected string, got <type>`                  | `no_change_state` is not a string (e.g., integer, boolean, list).        | Ensure the value is a string, quoted if necessary.                                                                                   |
| `config: tracker.no_change_state: must not be empty`                           | `no_change_state` is set to an explicit empty string.                    | Provide a valid state name, or omit the field entirely to fall back to `handoff_state`.                                              |
| `config: tracker.no_change_state: resolved to empty (check environment variable)` | `$VAR` reference resolved to an empty string (variable unset or empty). | Set the referenced environment variable to a valid state name.                                                                       |
| `config: tracker.no_change_state: requires tracker.handoff_state; a declared run performs no transition where no handoff path applies` | `no_change_state` is set while `handoff_state` is empty.                | Set `tracker.handoff_state`, or remove `no_change_state`.                                                                            |
| `config: tracker.no_change_state: "<val>" must equal tracker.handoff_state or name a member of tracker.terminal_states` | `no_change_state` is neither `handoff_state` nor a member of the `terminal_states` written in front matter. | Set `no_change_state` to the same value as `handoff_state`, or to one of the states listed in `terminal_states`.                     |
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
| `config: reactions.ci_failure.watch_window_ms: must not exceed 9223372036854 (about 292 years); use 0 for no time limit, got <val>` | `watch_window_ms` exceeds the ceiling. | Lower the value, or use `0` for no time limit. |
| `config: reactions.ci_failure.watch_window_ms: must be non-negative, got <val>` | Negative value for `watch_window_ms`.                                    | Use `0` (no time limit) or a positive integer.                                                                                       |
| `config: notifications[N].kind: expected string, got <type>`                   | The `kind` of the `N`th `notifications` entry is not a string (e.g., integer, boolean, list). | Ensure the value is a string, quoted if necessary.                                                                 |
| `config: notifications[N].events: expected sequence, got <type>`               | `events` of the `N`th entry is not a list. | Write a list, such as `events: [session.failed]`. |
| `config: notifications[N].events[M]: expected string, got <type>`              | Element `M` of `events` is not a string. | Write every event name as a string. |
| `config: notifications[N].events[M]: unknown event type "<v>"; valid: <catalog>` | Element `M` of `events` is not in the event catalog. | Use one of the names the message lists; see [Section 2.11](#211-notifications-operator-notification-backends). |
| `config: notifications[N].events[M]: event type "<v>" is listed more than once` | Element `M` repeats an earlier event in the same list. | Remove the repeat. |
| `config: notifications[N].events[M]: agent.message cannot be posted as a tracker comment` | A `tracker_comment` entry lists `agent.message`. | Remove it; messages from the agent never reach the issue. |
| `config: notifications[N].events: a tracker_comment entry must list its events; write [] to post only what the deprecated settings enable` | A `tracker_comment` entry has no `events` key. | Add `events`, using `[]` to post only what the deprecated settings enable. |
| `config: notifications[N].kind: only one notifications entry may have kind tracker_comment` | A second `tracker_comment` entry. | Merge the two entries into one. |
| `config: notifications[N].kind: kind tracker_comment requires a configured tracker` | A `tracker_comment` entry while `tracker.kind` is empty. | Set `tracker.kind`, or remove the entry. |
| `config: notifications[N].<key>: kind tracker_comment takes only kind and events` | A `tracker_comment` entry carries a key other than `kind` and `events`. | Remove the key. |
| `config: notifications[N].kind: <reason>` | The entry receives an event Sortie produces and its `kind` is not a registered backend. | Use `webhook`, `slack`, `telegram`, `discord`, or `tracker_comment`. |
| `config: notifications[N]: <reason>` | The entry receives an event Sortie produces and its backend cannot be built, such as a required secret that resolved to an empty string. | Fix the field the reason names. |
| `config: reactions.<kind>.escalation: must be "label", "comment", or "none", got "<v>"` | `escalation` of a reaction is any other value. | Use `label`, `none`, or the deprecated `comment`. |

### 9.3 Environment Variable Errors

These errors are raised when `SORTIE_*` environment variables or `.env` file values fail type coercion. Each error identifies the env var as the source.

| Error pattern                                                                                                  | Cause                                                     | Fix                                                                              |
| -------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------- | -------------------------------------------------------------------------------- |
| `config: polling.interval_ms: invalid integer value: <val> (from SORTIE_POLLING_INTERVAL_MS)`                  | Non-integer value in an integer env var.                  | Set the env var to a plain integer (e.g., `30000`). Remove units or decimals.    |
| `config: agent.<field>: invalid integer value: <val> (from SORTIE_AGENT_<FIELD>)`                              | Same, for any agent integer field.                        | Same fix as above.                                                               |
| `config: <section>.<field>: value is outside the range an integer setting accepts, -9223372036854775808 to 9223372036854775807 (from SORTIE_<SECTION>_<FIELD>)` | An integer too large or too small for Sortie to hold, in an integer env var. | Set the env var to a value within the range.                                     |
| `config: agent.max_cost_usd: invalid number value: <val> (from SORTIE_AGENT_MAX_COST_USD)`                     | Value in `SORTIE_AGENT_MAX_COST_USD` that is not a number. | Set the env var to an integer or decimal (e.g., `5` or `2.5`).                  |
| `config: agent.max_daily_cost_usd: invalid number value: <val> (from SORTIE_AGENT_MAX_DAILY_COST_USD)` | Value in `SORTIE_AGENT_MAX_DAILY_COST_USD` that is not a number. | Set the env var to an integer or decimal (e.g., `5` or `2.5`). |
| `config: agent.max_weekly_cost_usd: invalid number value: <val> (from SORTIE_AGENT_MAX_WEEKLY_COST_USD)` | Value in `SORTIE_AGENT_MAX_WEEKLY_COST_USD` that is not a number. | Set the env var to an integer or decimal (e.g., `5` or `2.5`). |
| `config: agent.max_monthly_cost_usd: invalid number value: <val> (from SORTIE_AGENT_MAX_MONTHLY_COST_USD)` | Value in `SORTIE_AGENT_MAX_MONTHLY_COST_USD` that is not a number. | Set the env var to an integer or decimal (e.g., `5` or `2.5`). |
| `config: tracker.comments.<field>: invalid boolean value: <val> (expected true/false/1/0) (from SORTIE_TRACKER_COMMENTS_<FIELD>)` | Invalid boolean in a comments env var.                    | Use `true`, `false`, `1`, or `0` (case-insensitive).                             |
| `config: dotenv <path>:<line>: missing '=' in line`                                                            | `.env` file line has no `=` separator.                    | Ensure each non-comment line in the `.env` file is `KEY=VALUE`.                  |
| `config: dotenv <path>:<line>: invalid key "<key>"`                                                            | `.env` file key contains invalid characters.              | Keys MUST match `[A-Za-z_][A-Za-z0-9_]*`.                                       |

### 9.4 Template Errors

| Error                       | Phase                 | Impact                                   | Cause                                                                                                        | Fix                                                                                                                                                                                                                   |
| --------------------------- | --------------------- | ---------------------------------------- | ------------------------------------------------------------------------------------------------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **`template_parse_error`**  | Parse (workflow load) | **Blocks all dispatch** until corrected. | Syntax error in the prompt template: unclosed action, mismatched delimiters, undefined function in pipeline. | Check `{{ }}` balance. Verify function names match the FuncMap (`toJSON`, `join`, `lower`). Look for unclosed `{{ if }}`, `{{ range }}`, or `{{ with }}` blocks.                                                      |
| **`template_render_error`** | Render (per issue)    | **Fails the current run attempt** only.  | Runtime error: missing variable (`missingkey=error`), type mismatch in pipeline, FuncMap function error.     | Check variable names against the data contract (`.issue.*`, `.attempt`, `.run.*`). Verify that fields accessed inside `{{ range }}` use `$` prefix for top-level access. Ensure `join` receives a list, not a scalar. |
| **`template_parse_error`**: undefined call | Parse (workflow load) | **Blocks all dispatch** until corrected. | A prompt template calls a name that nothing defines. The message reads `calls template "<name>", which is not defined`, and ends with `, reached from <file>` when the call sits in a partial. | Define the block in a partial or in the template, or correct the name in the call. Check every branch of the template, because a call in a branch no issue takes also fails. |
| **`template_parse_error`**: name defined twice | Parse (workflow load) | **Blocks all dispatch** until corrected. | Two files define one name for the same prompt template, a partial and the template or two partials, or a partial defines `prompt`. The message reads `template "<name>" is already defined in <file>`. | Keep one definition. To vary a block per template, define it in each prompt template and call it from the partial. |
| **`template_parse_error`**: text outside a define block | Parse (workflow load) | **Blocks all dispatch** until corrected. | A partial holds text or an action outside its `define` blocks. The message reads `only define blocks, white space, and comments may appear outside a define block`. A `dispatch.partials` pattern that selects a prompt template or `WORKFLOW.md` causes this too. | Move the text into a `define` block, or narrow the pattern to a directory that holds only partials. |
| **`template_render_error`** in a partial | Render (per issue) | **Fails the current run attempt** only. | A define block of a partial fails at run time, for example on a missing variable. The error names the partial file and the line within it, with no front matter offset. | Open the named file at the named line, and check that the call passed the data the block reads (`{{ template "name" . }}`). |

**Line number adjustment:** Template error messages name the file that holds the fault. For the Markdown body of `WORKFLOW.md` the line is adjusted to a `WORKFLOW.md`-relative position (the front matter line count is added to the template-relative line number). A fault in a partial or in a rule template file names that file, and its line counts from the top of that file. The error message format:

```
template parse error in WORKFLOW.md (line 47): template: prompt:4:15: ...
template render error in WORKFLOW.md (line 52): template: prompt:9: ...
template parse error in /srv/wf/partials/shared.md (line 3): template: ./partials/shared.md:3: ...
```

### 9.5 Agent Errors

These errors are raised when an agent session starts, before any turn runs, and fail the session start with the error kind `agent_not_found`.

| Error | Cause | Fix |
| --- | --- | --- |
| `opencode.pure is not supported by OpenCode 2.x; remove it` | The `opencode` block sets `pure: true`. | Remove `opencode.pure`. |
| `OpenCode <version> is not supported; install OpenCode 2.x, published on npm as @opencode/cli` | `agent.command` reports a major other than 2. | Install OpenCode 2.x, or point `agent.command` at an installation of it. |

---

## 10. Config Fields Summary (Cheat Sheet)

A flat reference of every configuration field, for quick lookup. The "Env Override" column lists the `SORTIE_*` variable that overrides the field, or "—" if not overridable (see [Section 3](#3-environment-variable-overrides)).

| Field                                   | Type             | Default                      | Env Override                             | Notes                                                                                  |
| --------------------------------------- | ---------------- | ---------------------------- | ---------------------------------------- | -------------------------------------------------------------------------------------- |
| `tracker.kind`                          | string           | _(required)_                 | `SORTIE_TRACKER_KIND`                    | e.g., `jira`                                                                           |
| `tracker.endpoint`                      | string           | adapter-defined              | `SORTIE_TRACKER_ENDPOINT`                | `$VAR` supported                                                                       |
| `tracker.api_key`                       | string or `$VAR` | _(required for `jira`, `github`, `linear`, `gitea`, `gitlab`)_        | `SORTIE_TRACKER_API_KEY`                 | Full env expansion                                                                     |
| `tracker.project`                       | string           | _(required for `jira`, `github`, `linear`, `gitea`, `gitlab`)_        | `SORTIE_TRACKER_PROJECT`                 | `$VAR` supported                                                                       |
| `tracker.active_states`                 | `[string]`       | `[]` (empty)                 | `SORTIE_TRACKER_ACTIVE_STATES`           | CSV; at least one of active/terminal required; no blank entry; no tracker default list |
| `tracker.terminal_states`               | `[string]`       | `[]` (empty)                 | `SORTIE_TRACKER_TERMINAL_STATES`         | CSV; at least one of active/terminal required; required for `github`, `gitea`, `gitlab`; no blank entry |
| `tracker.query_filter`                  | string           | `""`                         | `SORTIE_TRACKER_QUERY_FILTER`            | Adapter-interpreted                                                                    |
| `tracker.handoff_state`                 | string           | _(absent)_                   | `SORTIE_TRACKER_HANDOFF_STATE`           | Must not collide with active/terminal                                                  |
| `tracker.no_change_state`               | string           | `handoff_state`               | `SORTIE_TRACKER_NO_CHANGE_STATE`         | Must equal `handoff_state` or name a member of `terminal_states`; requires `handoff_state` |
| `tracker.handoff_evidence`              | string           | `observed`                   | —                                        | `observed`, `strict`, or `off`; closed set                                             |
| `tracker.in_progress_state`             | string           | _(absent)_                   | `SORTIE_TRACKER_IN_PROGRESS_STATE`       | Must be in active; must not collide with terminal/handoff                              |
| `tracker.api_version`                   | string           | `"3"`                        | —                                        | Jira only; `"3"` (Cloud) or `"2"` (Server/DC); `$VAR` supported; quote the value       |
| `tracker.comments.on_dispatch`          | bool             | `false`                      | `SORTIE_TRACKER_COMMENTS_ON_DISPATCH`    | Deprecated; subscribes `tracker_comment` to `session.started`                          |
| `tracker.comments.on_completion`        | bool             | `false`                      | `SORTIE_TRACKER_COMMENTS_ON_COMPLETION`  | Deprecated; subscribes `tracker_comment` to `session.completed` and `session.stopped`  |
| `tracker.comments.on_failure`           | bool             | `false`                      | `SORTIE_TRACKER_COMMENTS_ON_FAILURE`     | Deprecated; subscribes `tracker_comment` to `session.failed`                           |
| `polling.interval_ms`                   | integer          | `30000`                      | `SORTIE_POLLING_INTERVAL_MS`             | Dynamic reload                                                                         |
| `workspace.root`                        | path             | `<tmpdir>/sortie_workspaces` | `SORTIE_WORKSPACE_ROOT`                  | `~` expanded; `$VAR` skipped for env-sourced values                                    |
| `workspace.retention_days`              | integer          | `0` (disabled)               | `SORTIE_WORKSPACE_RETENTION_DAYS`        | Floor `30`; `1`-`29` rejected; dynamic reload, applies on the next sweep pass          |
| `hooks.after_create`                    | shell script     | _(null)_                     | —                                        | Fatal on failure                                                                       |
| `hooks.before_run`                      | shell script     | _(null)_                     | —                                        | Fatal on failure                                                                       |
| `hooks.after_run`                       | shell script     | _(null)_                     | —                                        | Failure ignored                                                                        |
| `hooks.before_remove`                   | shell script     | _(null)_                     | —                                        | Failure ignored                                                                        |
| `hooks.timeout_ms`                      | integer          | `60000`                      | —                                        | All hooks                                                                              |
| `agent.kind`                            | string           | `claude-code`                | `SORTIE_AGENT_KIND`                      |                                                                                        |
| `agent.command`                         | string or list   | adapter-defined              | `SORTIE_AGENT_COMMAND` (string form)     | Default kind's command. A string splits on whitespace locally and goes unsplit over SSH; a list is program then one argument per element. Optional for a kind with a default command |
| `agent.turn_timeout_ms`                 | integer          | `3600000`                    | `SORTIE_AGENT_TURN_TIMEOUT_MS`           | 1 hour; wall-clock bound per turn; `≤ 0` rejected                                      |
| `agent.read_timeout_ms`                 | integer          | `5000`                       | `SORTIE_AGENT_READ_TIMEOUT_MS`           | 5 seconds                                                                              |
| `agent.stall_timeout_ms`               | integer          | `300000`                     | `SORTIE_AGENT_STALL_TIMEOUT_MS`          | 5 min; `≤ 0` disables                                                                  |
| `agent.stop_grace_ms`                    | integer          | `5000`                       | `SORTIE_AGENT_STOP_GRACE_MS`             | 5 seconds; graceful-shutdown period before a force kill; `≤ 0` rejected                |
| `agent.max_concurrent_agents`           | integer          | `10`                         | `SORTIE_AGENT_MAX_CONCURRENT_AGENTS`     | Dynamic reload                                                                         |
| `agent.max_turns`                       | integer          | `20`                         | `SORTIE_AGENT_MAX_TURNS`                 |                                                                                        |
| `agent.max_retry_backoff_ms`            | integer          | `300000`                     | `SORTIE_AGENT_MAX_RETRY_BACKOFF_MS`      | 5 min; dynamic reload                                                                  |
| `agent.max_concurrent_agents_by_state`  | `map[string]int` | `{}`                         | —                                        | Keys lowercased; dynamic reload                                                        |
| `agent.max_sessions`                    | integer          | `0`                          | `SORTIE_AGENT_MAX_SESSIONS`              | Unlimited; dynamic reload                                                              |
| `agent.max_tokens`                      | integer          | `0`                          | `SORTIE_AGENT_MAX_TOKENS`                | Unlimited; dynamic reload                                                              |
| `agent.max_cost_usd`                    | number           | `0`                          | `SORTIE_AGENT_MAX_COST_USD`              | Unlimited; non-negative and finite; dynamic reload                                     |
| `agent.max_daily_cost_usd`              | number           | `0`                          | `SORTIE_AGENT_MAX_DAILY_COST_USD`        | Unlimited; UTC calendar day; non-negative and finite; dynamic reload                   |
| `agent.max_weekly_cost_usd`             | number           | `0`                          | `SORTIE_AGENT_MAX_WEEKLY_COST_USD`       | Unlimited; UTC week from Monday; non-negative and finite; dynamic reload               |
| `agent.max_monthly_cost_usd`            | number           | `0`                          | `SORTIE_AGENT_MAX_MONTHLY_COST_USD`      | Unlimited; UTC calendar month; non-negative and finite; dynamic reload                 |
| `agent.token_warning_percent`           | integer          | `0`                          | `SORTIE_AGENT_TOKEN_WARNING_PERCENT`     | Off; `0` to `99`; dynamic reload                                                       |
| `agent.max_consecutive_absences`        | integer          | `3`                          | `SORTIE_AGENT_MAX_CONSECUTIVE_ABSENCES`  | `0` and negative rejected; dynamic reload                                              |
| `db_path`                               | path             | `.sortie.db`                 | `SORTIE_DB_PATH`                         | Restart required; `$VAR` skipped for env-sourced values                                |
| `reactions.<kind>.provider`             | string           | _(absent)_                   | —                                        | Adapter identifier; absent = disabled; restart required                                |
| `reactions.<kind>.max_retries`          | integer          | `2`                          | —                                        | Fix continuations before escalation; non-negative; `merge_conflicts` defaults to `1`; restart required except for `ci_failure` |
| `reactions.<kind>.escalation`           | string           | `label`                      | —                                        | `"label"`, `"comment"` (deprecated), or `"none"`; every escalation emits its event; restart required except for `ci_failure` |
| `reactions.<kind>.escalation_label`     | string           | `needs-human`                | —                                        | Applied when `escalation` is `"label"`; restart required except for `ci_failure`       |
| `reactions.ci_failure.max_log_lines`    | integer          | `50`                         | —                                        | CI log excerpt lines; `0` disables; non-negative; restart required                      |
| `reactions.<kind>.watch_window_ms`      | integer          | `86400000` for `ci_failure`, `1800000` for the other four | — | Bounds the age of a pending entry that holds no standing escalation (and, for `review_comments`, is not waiting on a required approval); non-negative; not above `9223372036854`; `0` removes the bound; restart required except for `ci_failure` |
| `reactions.review_comments.poll_interval_ms` | integer     | `120000`                     | —                                        | Review poll interval; min `30000`; restart required                                    |
| `reactions.review_comments.debounce_ms` | integer          | `60000`                      | —                                        | Debounce after last comment; non-negative; restart required                            |
| `reactions.review_comments.max_continuation_turns` | integer | `3`                        | —                                        | Review-fix turns before escalation; positive; restart required                         |
| `reactions.auto_merge.strategy`          | string           | `squash`                     | —                                        | `merge`, `squash`, or `rebase`; restart required                                        |
| `reactions.auto_merge.require_ci`        | boolean          | `true`                       | —                                        | Merge waits until every reported check completes with no failing conclusion; `false` makes CI advisory; restart required |
| `reactions.auto_merge.delete_branch`     | boolean          | `true`                       | —                                        | Deletes the PR head branch after a successful merge; a delete failure does not roll back the merge; restart required |
| `reactions.auto_merge.poll_interval_ms`  | integer          | `60000`                      | —                                        | Precondition poll interval; min `30000`; restart required                               |
| `reactions.bot_review.poll_interval_ms`  | integer          | `60000`                      | —                                        | Bot comment poll interval; min `30000`; restart required                                |
| `reactions.bot_review.max_continuation_turns` | integer     | `5`                          | —                                        | Bot-fix turns before escalation; positive; restart required                             |
| `reactions.bot_review.bot_usernames`     | `[string]`       | _(empty)_                    | —                                        | Bot login allowlist, matched case-insensitively; an allowlisted author is also excluded from `review_comments`; a non-list value or non-string element is rejected; restart required |
| `reactions.merge_conflicts.poll_interval_ms` | integer      | `60000`                      | —                                        | Conflict-detection poll interval; min `30000`; restart required                          |
| `reactions.label_commands.provider`      | string           | _(absent)_                   | —                                        | SCM adapter identifier; absent or empty leaves the block inert; parses through its own path, so it carries no `max_retries`, `escalation`, or `escalation_label`; restart required |
| `reactions.label_commands.review_label`  | string           | `sortie:review`              | —                                        | Triggers the read-only review command; an explicit empty string disables it; restart required |
| `reactions.label_commands.fix_label`     | string           | `sortie:fix`                 | —                                        | Triggers the fix command; an explicit empty string disables it; an active provider with both command labels empty is rejected; restart required |
| `reactions.label_commands.poll_interval_ms` | integer       | `60000`                      | —                                        | Journal poll interval; a value below `30000` is clamped up to the floor with a warning; restart required |
| `reactions.merge_completion.target_state` | string         | _(required)_                 | —                                        | Terminal state applied on observed merge; must be in `terminal_states` as written; restart required |
| `reactions.merge_completion.poll_interval_ms` | integer    | `60000`                      | —                                        | Merge-observation poll interval; min `30000`; restart required                         |
| `self_review.enabled`                   | boolean          | `false`                      | —                                        | Activates self-review loop                                                             |
| `self_review.max_iterations`            | integer          | `3`                          | —                                        | Range [1, 10]; up to `2N−1` extra turns                                                |
| `self_review.verification_commands`     | `[string]`       | _(required when enabled)_    | —                                        | Shell commands for verification                                                        |
| `self_review.verification_timeout_ms`   | integer          | `120000`                     | —                                        | Per-command timeout                                                                    |
| `self_review.max_diff_bytes`            | integer          | `102400`                     | —                                        | Diff truncation limit                                                                  |
| `self_review.reviewer`                  | string           | `"same"`                     | —                                        | Only `"same"` in v1                                                                    |
| `dispatch.partials`                     | `[string]`       | `[]` (empty)                 | —                                        | Paths or glob patterns relative to the `WORKFLOW.md` directory; files whose define blocks every prompt template can call; read again on every load |
| `notifications`                         | `[map]`          | _(absent)_                   | —                                        | Destination list; `notify_operator` is registered only when an entry receives `agent.message` |
| `notifications[].kind`                  | string           | _(required)_                 | —                                        | Destination discriminator; v1: `webhook`, `slack`, `telegram`, `discord`, and the built-in `tracker_comment`  |
| `notifications[].events`                | `[string]`       | `[agent.message]` for every backend kind; _(required)_ for `tracker_comment` | — | Event types the entry receives; names from the closed catalog, none repeated; omitting it on a backend kind is deprecated; `agent.message` is rejected on `tracker_comment` |
| `notifications[].max_per_session`       | integer          | `20`                         | —                                        | `notify_operator` cap for the whole agent run, drawn from entries that receive `agent.message`; `0` selects the default; never unlimited; non-negative |
| **Extensions**                          |                  |                              |                                          |                                                                                        |
| `server.port`                           | integer          | `7678`                       | —                                        | CLI `--port` overrides; `0` disables server                                    |
| `server.host`                           | string (IP)      | `127.0.0.1`                  | —                                        | CLI `--host` overrides                                                         |
| `logging.level`                         | string           | `info`                       | —                                        | CLI `--log-level` overrides                                                            |
| `worker.ssh_hosts`                      | `[string]`       | _(absent)_                   | —                                        | SSH host targets; dynamic reload                                                       |
| `worker.max_concurrent_agents_per_host` | integer          | _(absent)_                   | —                                        | Per-host cap; dynamic reload                                                           |
| `worker.ssh_strict_host_key_checking`   | string           | `accept-new`                 | —                                        | `accept-new`, `yes`, `no`; dynamic reload                                              |
| `worker.ssh_pass_env`                   | `[string]`       | _(absent)_                   | —                                        | Names of variables to carry into a remote session; dynamic reload                     |
| `worker.ssh_disallow_pass_env`          | `[string]`       | _(absent)_                   | —                                        | Names of variables Sortie never carries into a remote session; dynamic reload          |

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

This uses all defaults: `claude-code` agent, 30-second polling, system-temp workspace root, no tracker (dispatch validation will fail — `tracker.kind` is required for actual dispatch).

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

> **Note:** In this example, `after_create` runs only when Sortie first creates the per-issue workspace directory, so the directory is empty when the clone runs. If `after_create` fails, Sortie removes the directory before the next retry, so a retry also starts from an empty directory. A clone error such as "destination path already exists" or "directory not empty" does not come from this example on the normal path.
>
> Hooks also run with a restricted environment: an allowlist (including `HOME` and `SSH_AUTH_SOCK`) plus `SORTIE_*` variables. Sortie strips any variable outside that set, such as `GIT_SSH_COMMAND`, so an SSH clone must reach its key through the SSH agent (`SSH_AUTH_SOCK`) or through `~/.ssh` via `HOME`, not through a stripped variable. See Section 6.2 for the full allowlist.

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
  stop_grace_ms: 5000 # 5-second graceful-shutdown period before a force kill
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

# ─── Reactions ─────────────────────────────────────────────────
# Omit ci_failure to disable CI feedback.
# SCM coordinates (owner, repo, token) live in the github: adapter block.
reactions:
  ci_failure:
    provider: github # Activate CI feedback via GitHub Checks API
    max_retries: 2 # CI-fix attempts before escalation
    max_log_lines: 50 # Log lines in the failing check excerpt
    escalation: label # "label" or "none" on exhaustion
    escalation_label: needs-human # Label added when escalation is "label"
  # review_comments:
  #   provider: github
  #   max_retries: 2
  #   escalation: label
  #   escalation_label: needs-human
  #   poll_interval_ms: 120000  # 2-minute review poll cycle
  #   debounce_ms: 60000        # 60s debounce after last comment
  #   max_continuation_turns: 3 # Max review-fix dispatches

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

A workflow targeting a Linear team. `project` is the team key, the state names are Linear workflow states, and `query_filter` is a Linear `IssueFilter` JSON object:

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

_This document is derived strictly from the Sortie Architecture Specification (Sections 5, 6, 9.4, and 10) and informed by end-to-end testing experience (tasks 7.11–7.13). It is the authoritative user-facing reference for workflow authors._
