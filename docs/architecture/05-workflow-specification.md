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
- `dispatch`
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
  - Supported values: `jira`, `github`, `linear`; additional adapters registered separately.
- `endpoint` (string)
  - Tracker API endpoint. Interpretation is adapter-defined.
- `api_key` (string)
  - May be a literal token or `$VAR_NAME`.
  - If `$VAR_NAME` resolves to an empty string, treat the key as missing.
  - Required for dispatch when the tracker adapter declares it (e.g., Jira requires an API
    key; a file-based tracker does not).
- `project` (string)
  - Project identifier. Interpretation is adapter-defined (project key for Jira, `owner/repo`
    for GitHub, team key for Linear). Required for dispatch when the tracker adapter requires
    project scoping.
- `active_states` (list of strings)
  - Default values are adapter-defined; must be configured explicitly when the adapter's defaults
    differ from deployment expectations.
- `terminal_states` (list of strings)
  - Default values are adapter-defined; must be configured explicitly when the adapter's defaults
    differ from deployment expectations.
- `query_filter` (string, optional)
  - Adapter-defined query fragment that narrows the base candidate and terminal-state queries.
  - The orchestrator passes this value to the tracker adapter without interpretation.
  - The adapter is responsible for safe integration into its native query language. The Jira
    adapter appends the fragment to its JQL string; the Linear adapter parses it as an
    `IssueFilter` JSON object and merges it into the GraphQL filter (Section 11.6.1).
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
    phases: dispatch vs. exit).
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
- `retention_days` (integer or string integer, optional)
  - Default: `0` (disables the bound).
  - A value from `1` to `29` and any negative value are rejected when the configuration is
    parsed.
  - The window is evaluated by the periodic workspace sweep.

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
  - Other supported values: `copilot-cli`, `codex`, `opencode`, `kiro`, `http`, and any
    additionally registered adapter.
  - Parallels `tracker.kind`.
  - This is the default agent kind used when no `dispatch.rules` entry overrides it; see
    §5.3.10 for the override mechanism.
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
- `max_tokens` (integer)
  - Default: `0` (unlimited; no token budget enforced).
  - Cumulative per-issue token ceiling. The orchestrator sums `total_tokens` across the
    issue's `run_history` entries and stops re-dispatching once the sum reaches `max_tokens`.
  - When the sum reaches `max_tokens`, the claim is released and a warning is logged, exactly
    as `max_sessions` does. A failed token query fails open: dispatch proceeds.
  - Overridable through `SORTIE_AGENT_MAX_TOKENS`. `0` disables the budget.
  - Changes are re-applied at runtime and affect future retry timer evaluations.

Adapter-specific pass-through config:

Each adapter may define its own configuration fields in a sub-object named after its `kind`
value. These are pass-through values interpreted by the adapter and not by the orchestrator
core. For example, a Codex adapter may accept `codex.approval_policy` and
`codex.thread_sandbox`; a Claude Code adapter may accept `claude-code.permission_mode`; an
OpenCode adapter may accept `opencode.variant` and `opencode.allowed_tools`; a Kiro adapter
may accept `kiro.model` and `kiro.trust_tools`. The orchestrator forwards the entire
sub-object to the adapter without validation.

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

**Reaction kind: `auto_merge`**

Auto-merge applies to Sortie-managed PRs whose preconditions (review decision, CI conclusion,
mergeability) are satisfied. See §11C for the full contract. (Runtime kind value: `merge`.)

Extra fields:

- `strategy` (string, via Extra): merge strategy. Default: `squash`. One of `merge`, `squash`,
  `rebase`.
- `require_ci` (boolean, via Extra): whether merge requires CI success. Default: `true`.
- `delete_branch` (boolean, via Extra): whether to delete the head branch after merge.
  Default: `true`.
- `poll_interval_ms` (integer, via Extra): polling interval for merge-precondition checks.
  Default: `60000` (1 minute). Minimum: `30000`.

Example:

```yaml
reactions:
  auto_merge:
    provider: github
    max_retries: 3
    escalation: label
    escalation_label: needs-human
    strategy: squash
    require_ci: true
    delete_branch: true
    poll_interval_ms: 60000
```

**Validation rules:**

- Reaction kind keys MUST match `[a-z][a-z0-9_-]*`.
- Invalid kind keys are rejected with a configuration error.
- Per-kind common fields follow the same validation as the deprecated `ci_feedback` equivalents.
- Extra fields are kind-specific; the orchestrator validates them when constructing the
  kind-specific config (e.g. `BuildReviewReactionConfig`).

#### 5.3.10 `dispatch` (object, optional)

Routes the initial dispatch to an `(agent_kind, template_id)` selection per first-match-wins
rules. When absent, the orchestrator behaves identically to today: the resolver returns the
top-level defaults (`agent.kind` and the Markdown body template).

Fields:

- `rules` (list of `DispatchRule`, optional): ordered list of dispatch rules; first-match-wins.
- `default` (object, optional): carries `agent` and `template` overrides applied when no rule
  matches.

Each `DispatchRule` has four keys:

- `name` (optional): operator-supplied rule identifier used in metrics labels and
  freeze-on-dispatch persistence. When present, the value MUST match the pattern
  `^[a-z][a-z0-9_-]*$`. When absent or empty, the rule has no operator-visible name and metrics
  label the rule as the sentinel `<none>`.
- `match`: a block whose keys define the predicate evaluated against the issue.
- `agent`: optional override of the agent kind for matching issues.
- `template`: optional override of the prompt template path for matching issues.

**Match-block keys and semantics**

Match keys are evaluated with AND semantics across keys and OR semantics within a single key.
String-valued keys accept either a single string or a list of strings; a scalar is treated as a
one-element list. Comparisons against `issue_type` and `assignee` are case-insensitive:

- `labels` (string or list of glob patterns): matches when the issue carries at least one label
  matching any pattern; glob syntax (e.g. `bug/*`, `*-urgent`).
- `issue_type` (string or list of strings): case-insensitive equality match against the issue
  type field; matches when the issue type equals any list entry.
- `priority` (predicate object): numeric comparison via one operator key: `eq`, `in`, `lt`,
  `lte`, `gt`, or `gte`. The predicate object MUST have exactly one operator key.
- `identifier` (string or list of glob patterns): matches against the issue identifier string.
- `assignee` (string or list of strings): case-insensitive equality match against the issue
  assignee; matches when the assignee equals any list entry.

**Resolution semantics and freeze-on-dispatch invariant**

First-match wins: evaluation stops at the first rule whose `match` block succeeds. Absent rule
fields fall through to `dispatch.default`, then to the top-level `agent.kind` and the
Markdown-body template (the pre-dispatch top-level defaults).

The resolved `(agent_kind, template_id, rule_name)` is recorded on `RunningEntry` at the
initial dispatch and propagated through `RetryEntry`. Retries and reaction-driven continuations
reuse the frozen selection without re-evaluating rules.

**Template lifecycle**

Per-rule template paths are relative to `filepath.Dir(workflow_path)`. Absolute paths,
`~`-prefixed paths, and symlink escapes outside the workflow directory tree are rejected at load
time. The `ResolveRule` function and full algorithm details are in §5.3.10's source spec.

Example:

```yaml
dispatch:
  rules:
    - name: bug-fix
      match:
        labels: ["bug", "bug/*"]
      agent: claude-code
      template: templates/bug-fix.md
    - name: docs-update
      match:
        issue_type: documentation
      agent: codex
      template: templates/docs.md
    - name: high-priority
      match:
        priority:
          lte: 2
      agent: claude-code
  default:
    agent: claude-code
    template: templates/default.md
```

#### 5.3.11 `notifications` (list, optional)

The `notifications` list configures the backends behind the `notify_operator` tool
(Section 10.4.5). While a session runs, the agent calls the tool to escalate a decision, report
progress, or flag a blocker to a real-time channel. The tool is registered only when at least one
valid backend is configured; an empty or absent list leaves it unregistered, so the agent is
never offered a tool it cannot use.

The value is a sequence, not a single object. Each entry is a map carrying a required `kind`
discriminator and that backend's own fields. A second channel is a second list entry.

```yaml
notifications:
  - kind: slack
    webhook_url: $SORTIE_SLACK_WEBHOOK_URL
    max_per_session: 20
  - kind: webhook
    url: $SORTIE_OPS_WEBHOOK_URL
```

Per-entry fields:

- `kind` (string)
  - Required. The registry discriminator, resolved against the notifier registry
    (Section 10.4.7) at sidecar startup. v1 backends are `webhook` and `slack`.
- `max_per_session` (integer, optional)
  - The per-session `notify_operator` call cap. It is not a per-entry default: an omitted, `null`,
    or `0` value contributes nothing to cap selection, which then falls back to the default of `20`
    only when every entry is `0` or unset (see "Validation and resolution" below). `0` never means
    unlimited. A negative value is rejected at config parse time.
- backend-specific fields
  - Passed through to the backend constructor untyped, with `$VAR` references resolved. The
    `webhook` backend requires `url`; the `slack` backend requires `webhook_url`.

Validation and resolution:

- The list is structurally validated when the config is parsed: a non-sequence value, an entry
  that is not a map, an entry with an empty `kind`, or a negative `max_per_session` aborts config
  construction (Section 6.3).
- When more than one backend is configured, the effective per-session cap is the maximum non-zero
  `max_per_session` across entries, falling back to the default when every entry is `0` or unset.
  The cap counts `notify_operator` calls, not per-backend sends.
- A backend secret SHOULD be given as a reference to a `SORTIE_`-prefixed environment variable
  (`$SORTIE_NAME` or `${SORTIE_NAME}`). The `notify_operator` tool runs in a separate
  `sortie mcp-server` process whose environment is constructed by the agent's MCP host. The
  orchestrator guarantees that only its `SORTIE_`-prefixed variables are propagated into that
  process for `$VAR` resolution; the host MAY additionally inherit other variables from its own
  environment, so a reference without the prefix is not guaranteed to resolve and MAY resolve to
  the empty string. References are expanded against the sidecar process environment with no prefix
  enforcement, so the `SORTIE_` prefix is the way to guarantee a secret resolves regardless of
  host. When a required secret resolves to the empty string, the backend rejects it, which
  surfaces as a fatal sidecar startup error rather than a notification posted nowhere.

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

