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

- `WORKFLOW.md` should be self-contained enough to describe and run different workflows (prompt, runtime settings, hooks, and tracker selection/config) without requiring out-of-band service-specific configuration.

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
- `db_path`
- `self_review`
- `reactions`
- `dispatch`
- `notifications`

Unknown keys should be ignored for forward compatibility.

Note:

- The workflow front matter is extensible. Optional extensions may define additional top-level keys (for example `server`) without changing the core schema above.
- Extensions should document their field schema, defaults, validation rules, and whether changes apply dynamically or require restart.
- Common extensions: `server.port` (integer) overrides the default HTTP server port (7678); `server.host` (string, IP address) overrides the default bind address (`127.0.0.1`). The HTTP server starts unconditionally unless `server.port` or `--port` is `0`. See Section 13.7 for full semantics.

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
  - Required for dispatch when the tracker adapter declares it (e.g., Jira requires an API key; a file-based tracker does not).
- `project` (string)
  - Project identifier. Interpretation is adapter-defined (project key for Jira, `owner/repo` for GitHub, team key for Linear). Required for dispatch when the tracker adapter requires project scoping.
- `active_states` (list of strings)
  - Default values are adapter-defined; must be configured explicitly when the adapter's defaults differ from deployment expectations.
- `terminal_states` (list of strings)
  - Default values are adapter-defined; must be configured explicitly when the adapter's defaults differ from deployment expectations.
- `query_filter` (string, optional)
  - Adapter-defined query fragment that narrows the base candidate and terminal-state queries.
  - The orchestrator passes this value to the tracker adapter without interpretation.
  - The adapter is responsible for safe integration into its native query language. The Jira adapter appends the fragment to its JQL string; the Linear adapter parses it as an `IssueFilter` JSON object and merges it into the GraphQL filter (Section 11.6.1).
  - Default: empty string (no additional filtering).
- `handoff_state` (string, optional)
  - Target tracker state for orchestrator-initiated handoff transitions after a successful worker run (see ADR-0007).
  - Supports `$VAR` environment indirection.
  - When absent, no handoff transition is performed; the orchestrator uses continuation retry as before.
  - Empty values, including `$VAR` references that resolve to empty, are treated as configuration errors.
  - Must not appear in `active_states` (would cause immediate re-dispatch after handoff).
  - Must not appear in `terminal_states` (handoff is not terminal; the issue may return to active).
  - Changes take effect for future worker exits, not in-flight sessions.
- `no_change_state` (string, optional)
  - Target tracker state for a worker run whose agent declared, through `.sortie/status`, that the requested outcome already held and nothing needed changing (see ADR-0028).
  - Supports `$VAR` environment indirection.
  - Supports the `SORTIE_TRACKER_NO_CHANGE_STATE` environment override.
  - When absent, a declared run targets `handoff_state` instead; a deployment that does not set this field sees no change in where its issues land.
  - Empty values, including `$VAR` references that resolve to empty, are treated as configuration errors.
  - Requires `handoff_state` to be non-empty: a declared run performs no transition where no handoff path applies.
  - Must equal `handoff_state` (case-insensitive) or name a member of `terminal_states` exactly as written in front matter (case-insensitive); no other value is permitted, and no tracker adapter's default terminal-state list is consulted.
  - Changes take effect for future worker exits, not in-flight sessions.
- `in_progress_state` (string, optional)
  - Target tracker state for dispatch-time transitions. When configured, the worker calls `TransitionIssue` as the first step of each attempt, before workspace preparation.
  - Supports `$VAR` environment indirection.
  - When absent, no dispatch-time transition is performed. This is the default.
  - Empty values, including `$VAR` references that resolve to empty, are treated as configuration errors.
  - MUST appear in `active_states` (otherwise reconciliation would immediately cancel the worker after the transition changes the issue's tracker state).
  - MUST NOT appear in `terminal_states` (a terminal state would trigger workspace cleanup on the next reconciliation tick).
  - MUST NOT collide with `handoff_state` (the two transitions represent different lifecycle phases: dispatch vs. exit).
  - Transition failure is non-fatal: the worker logs a warning and continues to workspace preparation.
  - If the issue is already in the target state (case-insensitive comparison), the `TransitionIssue` call is skipped and a debug-level message is logged.
  - Changes take effect for future dispatches, not in-flight sessions.
- `comments` (object, optional; deprecated)
  - Toggles for the comments the orchestrator posts at session lifecycle points. Keys: `on_dispatch`, `on_completion`, `on_failure`. Each is a boolean defaulting to `false`; a non-boolean value is rejected with a configuration error. The `SORTIE_TRACKER_COMMENTS_ON_DISPATCH`, `SORTIE_TRACKER_COMMENTS_ON_COMPLETION`, and `SORTIE_TRACKER_COMMENTS_ON_FAILURE` environment overrides set the three keys before the mapping below applies.
  - Each key that resolves to `true` subscribes the built-in `tracker_comment` destination (Section 5.3.10) to a set of events: `on_dispatch` to `session.started`; `on_completion` to `session.completed` and `session.stopped`; `on_failure` to `session.failed`. The mapping unions with the `events` of an explicit `tracker_comment` entry, so holding both forms never posts a comment twice.
  - The settings keep working. A resolved `true` value draws a deprecation advisory (Section 5.3.10) that names the events to list in a `tracker_comment` entry before the key is removed. Sortie never rewrites `WORKFLOW.md`, and no removal date is set.
  - Changes take effect for future dispatches and worker exits, not in-flight sessions.

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
  - Bare strings without path separators are preserved as-is (relative roots are allowed but discouraged).
- `retention_days` (integer or string integer, optional)
  - Default: `0` (disables the bound).
  - A value from `1` to `29` and any negative value are rejected when the configuration is parsed.
  - The window is evaluated by the periodic workspace sweep.

#### 5.3.4 `hooks` (object)

Fields:

- `after_create` (multiline shell script string, optional)
  - Runs only when a workspace directory is newly created.
  - Failure aborts workspace creation.
- `before_run` (multiline shell script string, optional)
  - Runs before each agent attempt after workspace preparation and before launching the coding agent.
  - Failure aborts the current attempt.
- `after_run` (multiline shell script string, optional)
  - Runs after each agent attempt (success, failure, timeout, or cancellation) once the workspace exists.
  - Failure is logged but ignored.
- `before_remove` (multiline shell script string, optional)
  - Runs before workspace deletion if the directory exists.
  - Failure is logged but ignored; cleanup still proceeds.
- `timeout_ms` (integer, optional)
  - Default: `60000`
  - Applies to all workspace hooks.
  - A value outside the range an integer setting accepts, positive or negative, is rejected when the configuration is parsed; other non-positive values should be treated as invalid and fall back to the default.
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
  - Other supported values: `copilot-cli`, `codex`, `opencode`, `mock`, and `agent-client-protocol`.
  - Other kinds (for example, HTTP-based adapters) are available only if you register them separately.
  - Parallels `tracker.kind`.
  - This is the default agent kind used when no `dispatch.rules` entry overrides it; see §5.3.9 for the override mechanism.
  - `kiro` is a retired kind whose replacement is `agent-client-protocol`. A configuration naming it is converted at load, as the next bullet describes.
  - A retired kind has no adapter, and its registry declaration names a replacement kind. A configuration that names a retired kind in `agent.kind`, `dispatch.default.agent`, or a `dispatch.rules` entry's `agent` is converted at load onto the replacement kind, together with the settings block the retired kind read. The conversion runs on startup, on reload, and in `sortie validate`, rewrites the configuration in memory only, and leaves the workflow file untouched. Each conversion emits one `agent.kind.retired` advisory, and a setting the conversion cannot carry fails the load as a configuration error (§6.1, §6.3). A kind that is registered as a live adapter is never converted, even when the registry also declares it retired.
- `command` (string or list of strings)
  - The command the default agent kind's adapter uses to launch the agent process. The default kind is `dispatch.default.agent` when set, and `agent.kind` otherwise. Adapter-defined default.
  - String form: a local subprocess adapter splits the string on whitespace into an argument vector; an SSH worker passes it to the remote shell unsplit.
  - List form: element zero names the executable and each later element is one argument. A local launch resolves element zero whole, whitespace included, and passes the other elements verbatim, without splitting. An SSH worker quotes every element as one word for the remote shell, so the remote shell does not expand or split any of them. The list form is how a path or argument that holds whitespace reaches the agent process.
  - A list MUST NOT be empty, every element MUST be a non-empty string, and element zero MUST hold a non-whitespace character. A violation is rejected when the configuration is parsed, and the failing check names the element (`agent.command[0]`). `SORTIE_AGENT_COMMAND` sets the string form and replaces a list the file holds.
  - Command governance: a session launches only a command written for its own kind. `agent.command`, in either form, is the default kind's command. A kind that replaces a converted retired kind launches the converted command. Every other kind launches its adapter's default command, so `agent.command` MAY be omitted for a kind that has one.
  - Preflight reports `agent.command` for each kind a selection can reach (`agent.kind`, `dispatch.default.agent`, and every `dispatch.rules` entry's `agent`) that launches a command and has neither a command written for it nor a default command. A kind other than the default kind draws a message naming the selector that reaches it.
  - HTTP-based agent adapters do not launch a command.
- `turn_timeout_ms` (integer)
  - Default: `3600000` (1 hour)
  - Must be positive. A non-positive value is rejected when the configuration is parsed.
  - Unlike `stall_timeout_ms` below, this bound cannot be disabled: it is the last wall-clock stop on an unattended turn, so no non-positive value switches it off.
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
  - An entry outside the range an integer setting accepts, positive or negative, is rejected when the configuration is parsed; other invalid entries (non-positive or non-numeric) are ignored.
- `max_sessions` (integer)
  - Default: `0` (unlimited; no effort budget enforced).
  - Maximum number of completed worker sessions for a single issue before the orchestrator stops re-dispatching it. Counted from `run_history` entries.
  - When the count reaches `max_sessions`, a warning is logged. The retry handler releases the claim it holds; the poll tick's own rebuild of the exhausted-issue set writes the candidate into that set instead, because a held candidate never held a claim to release.
  - `0` disables the budget (unlimited retries).
  - Changes are re-applied at runtime and affect future retry timer evaluations.
  - The separate `max_consecutive_absences` governs the consecutive-absence ceiling.
  - Reaching the ceiling also posts one comment on the issue naming the session budget and `agent.max_sessions` as the setting that raises it.
- `max_tokens` (integer)
  - Default: `0` (unlimited; no token budget enforced).
  - Cumulative per-issue token ceiling. The orchestrator sums `total_tokens` across the issue's completed `run_history` entries, adds the running session's own reported spend, and stops re-dispatching once the sum reaches `max_tokens`.
  - Reaching the ceiling also stops a run already in flight, on the event loop, as soon as a usage figure carries the sum to the ceiling, not only at the next dispatch decision.
  - When the sum reaches `max_tokens`, a warning is logged, on the same two dispatch lanes and for the same reason `max_sessions` states above. A failed token query fails open, but not the same way on both lanes: the retry handler's check is skipped and dispatch proceeds for that issue, while the rebuild folds the prior set's entries for the failing axis forward unchanged and keeps the other axis fresh.
  - A run whose coding agent reported no token usage is recorded unmeasured and contributes nothing to the sum. A sum below `max_tokens` that includes at least one unmeasured run allows the dispatch and logs a warning naming the issue, the sum, the ceiling, and the unmeasured count.
  - Overridable through `SORTIE_AGENT_MAX_TOKENS`. `0` disables the budget.
  - Changes are re-applied at runtime: a lowered ceiling reaches a run already in flight from the next poll tick onward, and it affects future retry timer evaluations.
  - Reaching the ceiling also posts one comment on the issue naming the token budget and `agent.max_tokens` as the setting that raises it, stating whether a session was stopped in flight.
  - `token_warning_percent`, below, warns before this ceiling stops a run.
- `token_warning_percent` (integer)
  - Default: `0` (off).
  - Must be between `0` and `99`; rejected as a configuration error at parse time otherwise. It is never validated against `agent.max_tokens`, so lowering the ceiling can never make this field reject a reload.
  - The warning threshold, in tokens, is this percentage of `agent.max_tokens`, rounded up to the nearest whole token. It has no effect while `agent.max_tokens` is `0`.
  - Evaluated on the event loop, against the same live per-issue figure the ceiling reads, ahead of the ceiling's own evaluation. Once a run's figure reaches the threshold, one warning is logged for that run and the running session's `cost_budget` tool result reports the condition, so the agent can wrap up or hand off before the ceiling stops the run.
  - Overridable through `SORTIE_AGENT_TOKEN_WARNING_PERCENT`.
  - Changes are re-applied at runtime: a run already in flight that has not yet reached the previous threshold is evaluated against the new one from its next usage figure, and a run dispatched after the reload is evaluated against the new value from dispatch.
- `max_consecutive_absences` (integer)
  - Default: `3`.
  - Bounds how many runs in a row may be observed to have produced no evidence of work before the issue is parked. Any run that produces evidence of work resets the count to zero.
  - `0` and negative values are rejected as a configuration error at parse time, so startup, `sortie validate`, and the reload fail-safe path all reject them.
  - Overridable through `SORTIE_AGENT_MAX_CONSECUTIVE_ABSENCES`.
  - Changes are re-applied at runtime and affect every lane that evaluates the ceiling: future worker exits, retry timer evaluations, and the poll tick's park sweep.
  - The separate `max_sessions` governs the total per-issue session budget.
- `stop_grace_ms` (integer)
  - Default: `5000`.
  - The period an adapter waits, after sending a catchable termination signal, for the agent to exit on its own before it force-terminates the process group.
  - `0`, a negative value, and a value above the largest millisecond count whose conversion to a duration stays positive are rejected as a configuration error at parse time, so startup, `sortie validate`, and the reload fail-safe path all reject them.
  - Overridable through `SORTIE_AGENT_STOP_GRACE_MS`.
  - Takes effect for future worker attempts, not an in-flight session.
  - In `claude-code`, `copilot-cli`, and `opencode`, the same value also bounds a cancelled turn's escalation to a force kill.

Adapter-specific pass-through config:

Each adapter may define its own configuration fields in a sub-object named after its `kind` value. These are pass-through values interpreted by the adapter and not by the orchestrator core. For example, a Codex adapter may accept `codex.approval_policy` and `codex.thread_sandbox`; a Claude Code adapter may accept `claude-code.permission_mode`; an OpenCode adapter may accept `opencode.variant` and `opencode.allowed_tools`. The orchestrator resolves the sub-object for each attempt and hands it to the adapter as a session input (§10.1); an adapter holds none of it between sessions. The keys `model` and `effort` mean the same in the block of every kind that reads them (§10.1): a string handed to the runtime as written, where an absent key, a null, and an empty string all leave the runtime's own default in charge. The orchestrator reads only those two keys, for validation advisories and for recording, and never interprets their values. A change to a block applies from the next attempt without a restart, while a running session keeps the block it started with (§5.3.9). An adapter may declare a validator that preflight runs over its own sub-object, and an adapter may declare metadata that a core preflight rule reads to refuse, or to warn about, a value of that sub-object.

#### 5.3.6 `db_path` (string, optional)

Filesystem path for the SQLite database file.

- Supports `$VAR` environment indirection and `~` home directory expansion.
- Absolute paths are used as-is.
- Relative paths are resolved against the directory containing `WORKFLOW.md`.
- Default: `.sortie.db` in the same directory as `WORKFLOW.md`.
- An explicit empty string (`db_path: ""`) is equivalent to omitting the field; the default path is used.
- Non-string values are rejected with a configuration error.
- If the value resolves to an empty string after environment expansion (e.g., an unset `$VAR`), startup fails with a configuration error.
- Changes to `db_path` during dynamic reload update the in-memory config but have no effect on the already-open database connection; a restart is required.

#### 5.3.7 `self_review` (object, optional)

Self-review loop configuration. When `enabled` is true and `verification_commands` is non-empty, the orchestrator runs a bounded review-fix cycle after the coding turn loop completes. Each iteration executes verification commands, generates a workspace diff, and presents both to the agent for a structured verdict. Disabled by default; zero overhead when disabled.

Fields:

- `enabled` (boolean)
  - Activates the self-review loop. Default: `false`.
  - When `true`, `verification_commands` must be non-empty or a configuration error is raised.
- `max_iterations` (integer)
  - Hard cap on review iterations. Default: `3`. Range: [1, 10].
  - Each iteration consists of a review turn and (if the verdict is `iterate`) a fix turn. `max_iterations: N` means up to `2N − 1` additional agent turns.
- `verification_commands` (list of strings)
  - Shell commands executed during each review iteration. Required when `enabled` is true.
  - Each command runs in its own subprocess with the workspace as `cwd` and a per-command timeout. Its process group, its Job Object on Windows, is terminated when it exits, times out, or is cancelled, resending that termination until no process of the group or job is still alive or a 2-second bound elapses, and its exit status decides whether it passed. A Windows command running without a Job Object, because one could not be created or assigned, has only its direct process reached by that termination. A termination that cannot confirm that no process of the group or job is still alive once the bound elapses is reported as a warning.
- `verification_timeout_ms` (integer)
  - Per-command timeout in milliseconds. Default: `120000` (2 minutes).
- `max_diff_bytes` (integer)
  - Maximum bytes of workspace diff included in the review prompt. Default: `102400` (100 KB). Diffs exceeding this limit are truncated with a marker.
- `reviewer` (string)
  - Which agent performs the review. Default: `"same"`. Only `"same"` (reuse the current session) is supported in v1.

#### 5.3.8 `reactions` (object, optional)

Reaction configuration. Each key under `reactions` identifies a reaction kind (e.g. `ci_failure`, `review_comments`). The orchestrator creates pending reaction entries on normal worker exit and processes them during the reconcile tick. Reaction kinds are extensible: unknown kind keys are parsed into a generic `ReactionConfig` and made available to future consumers.

**Common fields per reaction kind:**

Each reaction kind sub-object shares a common field schema:

- `provider` (string)
  - Identifies the external system adapter for this reaction kind (e.g. `github`). Empty string or absent means the reaction kind is disabled.
- `max_retries` (integer)
  - Maximum fix continuation dispatches per issue before escalation. Default: `2`, except `merge_conflicts`, which defaults to `1`.
  - MUST be non-negative; negative values are rejected with a configuration error.
  - `review_comments` and `bot_review` do not consume this field. Each bounds its dispatches with its own `max_continuation_turns` instead, and a value set here has no effect on those two kinds.
- `escalation` (string)
  - Action taken when the kind stops dispatching continuations and hands the subject to a person. Two conditions reach it: the kind's dispatch budget is exhausted, whether that budget is `max_retries` or `max_continuation_turns`, or a `triage` command answers `escalate`. Valid values: `label` (default), `comment` (deprecated), and `none`. Every escalation emits its `escalation.<kind>` event (Section 5.3.10) whatever the value. `label` applies the label, and the event reaches only the destinations subscribed to it. `none` applies no label, and the event reaches only the destinations subscribed to it. `comment` applies no label and subscribes the `tracker_comment` destination to the event, keeps working, and draws a deprecation advisory whose replacement is `escalation: none` plus the event in the `events` of a `tracker_comment` entry. An omitted `escalation` resolves to `label` for every kind, `auto_merge` included, and draws no advisory.
- `escalation_label` (string)
  - Label applied when `escalation` is `label`. Default: `needs-human`.
- `triage` (object)
  - Operator-owned command that runs in the issue workspace once the reaction has found a new subject and is about to dispatch an agent continuation. The command answers `handled`, `dispatch-agent`, or `escalate`, so deterministic work is resolved without spending an agent session, a continuation attempt, or a token budget. See Section 9.4 for the execution contract.
  - Recognized under exactly four reaction kinds: `ci_failure`, `review_comments`, `bot_review`, and `merge_conflicts`. Under any other key of `reactions`, including `auto_merge`, `merge_completion`, and `label_commands`, the block is rejected with a configuration error.
  - `triage.script` (string): the shell script body, executed through the same machinery as the workspace hooks. Required when the block is present. MUST be a string and MUST NOT be blank after whitespace trimming.
  - `triage.timeout_ms` (integer): bounds one triage run. Default: `60000`. MUST lie in the closed range `1` to `600000`. The ceiling sits below the smallest default reaction watch window, so a pending entry whose triage run hangs still ages out of that window.
  - Every field of the block is read once when the orchestrator is constructed, so a change takes effect on the next restart rather than on the next configuration reload.

Remaining keys within a kind sub-object are collected into an `Extra` map for kind-specific consumption.

**Reaction kind: `ci_failure`**

See Section 11A for the CI feedback contract. Extra fields:

- `max_log_lines` (integer, via Extra): maximum number of log lines in the CI failure log excerpt (Section 11A.2). Default: `50`.
- `watch_window_ms` (integer, via Extra): bounds a pending CI entry's age, measured from the last recorded head. Default: `86400000` (twenty-four hours). MUST be non-negative and MUST NOT exceed `9223372036854`. `0` removes the clock bound.

**Reaction kind: `review_comments`**

PR review comment routing. When configured, the orchestrator polls for human `CHANGES_REQUESTED` review comments on Sortie-created PRs and dispatches continuation turns so the agent can address the feedback. See Section 11B for the full contract.

Extra fields:

- `poll_interval_ms` (integer, via Extra): polling interval for review comments. Default: `120000` (2 minutes). Minimum: `30000`.
- `debounce_ms` (integer, via Extra): debounce window after the last detected comment before dispatching. Default: `60000` (60 seconds). MUST be non-negative.
- `max_continuation_turns` (integer, via Extra): maximum review-fix continuation dispatches per issue before escalation. Default: `3`. MUST be positive.
- `watch_window_ms` (integer, via Extra): bounds a pending review-comments entry's age, measured from the entry's creation. Default: `1800000` (thirty minutes). MUST be non-negative and MUST NOT exceed `9223372036854`. `0` removes the clock bound.

Example:

```yaml
reactions:
  review_comments:
    provider: github
    escalation: label
    escalation_label: needs-human
    poll_interval_ms: 120000
    debounce_ms: 60000
    max_continuation_turns: 3
```

**Reaction kind: `auto_merge`**

Auto-merge applies to Sortie-managed PRs whose preconditions (review decision, CI conclusion, mergeability) are satisfied. See §11C for the full contract. (Runtime kind value: `merge`.)

Extra fields:

- `strategy` (string, via Extra): merge strategy. Default: `squash`. One of `merge`, `squash`, `rebase`.
- `require_ci` (boolean, via Extra): whether merge requires CI success. Default: `true`.
- `delete_branch` (boolean, via Extra): whether to delete the head branch after merge. Default: `true`.
- `poll_interval_ms` (integer, via Extra): polling interval for merge-precondition checks. Default: `60000` (1 minute). Minimum: `30000`.
- `watch_window_ms` (integer, via Extra): bounds a pending auto-merge entry's age, measured from the entry's creation. Default: `1800000` (thirty minutes). MUST be non-negative and MUST NOT exceed `9223372036854`. `0` removes the clock bound.

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

**Reaction kind: `bot_review`**

Automated review-bot comment routing. When configured, the orchestrator polls for PR comments authored by automated review tools on Sortie-created PRs and dispatches continuation turns so the agent can address them. This is the complement of `review_comments`, which routes only human `CHANGES_REQUESTED` comments and excludes bot-authored ones. See Section 11D for the full contract. (Runtime kind value: `bot-review`.)

Extra fields:

- `bot_usernames` (list of strings, via Extra): allowlist of bot logins. A comment is bot-authored when the platform reports a bot user type or when its author login matches an entry here, case-insensitively. Default: empty. A value that is not a list, or a list holding a non-string element, is rejected with a configuration error.
- `poll_interval_ms` (integer, via Extra): polling interval for bot comments. Default: `60000` (1 minute). Minimum: `30000`.
- `max_continuation_turns` (integer, via Extra): maximum bot-fix continuation dispatches per issue before escalation. Default: `5`. MUST be positive.
- `watch_window_ms` (integer, via Extra): bounds a pending bot-review entry's age, measured from the entry's creation. Default: `1800000` (thirty minutes). MUST be non-negative and MUST NOT exceed `9223372036854`. `0` removes the clock bound.

The kind reads no `debounce_ms` field: bot comments arrive in bulk on push and dispatch immediately.

**Reaction kind: `merge_conflicts`**

Merge-conflict detection and resolution. When configured, the orchestrator polls mergeability on Sortie-created open PRs each reconcile cycle. Mergeability is evaluated on every due tick, and while a PR remains conflicted the orchestrator dispatches one rebase-and-resolve continuation turn per distinct conflicting head commit, subject to the retry budget; re-observing the same head dispatches nothing further. A return to no-conflict is not required between attempts. See Section 11E for the full contract. (Runtime kind value: `merge-conflict`.)

`max_retries` defaults to `1` for this kind rather than the common default of `2`, because merge-conflict resolution by a coding agent is less likely to succeed on a second attempt.

Extra fields:

- `poll_interval_ms` (integer, via Extra): polling interval for the conflict-detection state machine. Default: `60000` (1 minute). Minimum: `30000`.
- `watch_window_ms` (integer, via Extra): bounds a pending merge-conflict entry's age, measured from the entry's creation rather than from the last recorded head. Default: `1800000` (thirty minutes). MUST be non-negative and MUST NOT exceed `9223372036854`. `0` removes the clock bound.

**Reaction kind: `merge_completion`**

Merge-completion detection. When configured, the orchestrator observes the merge state of Sortie-managed PRs independently of who performs the merge and transitions the linked issue to a single configured terminal state exactly once. See Section 11G for the full contract. (Runtime kind value: `merge-completion`.)

The kind constrains two `tracker` fields, both checked when its configuration is constructed: `handoff_state` MUST be non-empty, and `terminal_states` MUST be written non-empty in front matter rather than left to the tracker adapter's default list.

Extra fields:

- `target_state` (string, via Extra): the single tracker terminal state the linked issue moves to once its pull request merges. Required, no default. MUST NOT equal `tracker.handoff_state`; MUST NOT be a member of `tracker.active_states`, falling back to the tracker adapter's default active-state list when that field is empty; MUST be a member of `tracker.terminal_states` exactly as written in configuration, with no fallback to the adapter's default terminal-state list. All three comparisons are case-insensitive.
- `poll_interval_ms` (integer, via Extra): polling interval for the merge-observation state machine. Default: `60000` (1 minute). Minimum: `30000`.

The kind carries no `watch_window_ms`: a pull request may remain unmerged for any length of time without starting a failure clock.

**Validation rules:**

- Reaction kind keys MUST match `[a-z][a-z0-9_-]*`.
- Invalid kind keys are rejected with a configuration error.
- Per-kind common fields are validated as follows: `max_retries` MUST be a non-negative integer, `escalation` MUST be `label`, `comment`, or `none`, and `provider`, `escalation`, and `escalation_label` MUST be strings. Each violation is rejected with a configuration error.
- Extra fields are kind-specific; the orchestrator validates them when constructing the kind-specific config (e.g. `BuildReviewReactionConfig`).
- A `triage` block under any reaction key other than `ci_failure`, `review_comments`, `bot_review`, or `merge_conflicts` is rejected with a configuration error, as is a block whose `script` is absent, not a string, or blank, and one whose `timeout_ms` falls outside the closed range `1` to `600000`.

#### 5.3.9 `dispatch` (object, optional)

Routes the initial dispatch to an `(agent_kind, template_id)` selection per first-match-wins rules. When absent, the orchestrator behaves identically to today: the resolver returns the top-level defaults (`agent.kind` and the Markdown body template).

Fields:

- `rules` (list of `DispatchRule`, optional): ordered list of dispatch rules; first-match-wins.
- `default` (object, optional): carries `agent` and `template` overrides applied when no rule matches.

Each `DispatchRule` has four keys and an optional settings block:

- `name` (optional unless the rule carries a settings block): operator-supplied rule identifier used in metrics labels, in run history, and to find the rule again after a reload. When present, the value MUST match the pattern `^[a-z][a-z0-9_-]*$`. When absent or empty, the rule has no operator-visible name and metrics label the rule as the sentinel `<none>`. A rule that carries a settings block MUST have a name, and the name MUST NOT be `default`, which run history and statistics give the `dispatch.default` selection.
- `match`: a block whose keys define the predicate evaluated against the issue.
- `agent`: optional override of the agent kind for matching issues.
- `template`: optional override of the prompt template path for matching issues.
- `<kind>`: an optional settings block named for the agent kind the rule runs.

A rule MUST carry at least one of `match`, `agent`, `template`, or a settings block. A rule key that names a registered agent kind, or equals the rule's own `agent` value, is its settings block; any other unrecognized key is a configuration error. `dispatch.default` carries no settings block: a key in it that names an agent kind is a configuration error, because the top-level block of each kind holds the default settings.

**Rule settings blocks**

The rule's kind is its `agent`, else `dispatch.default.agent`, else `agent.kind`. The block MUST be named for that kind, MUST be a mapping (an empty block is written `{}`), and MUST NOT write `kind`, `command`, or any of the four `agent` timeouts, which the orchestrator derives from the `agent` section and which stay workflow-wide. Each violation is a configuration error, so a reload that introduces one keeps the last good configuration, and so does a change of `agent.kind` or `dispatch.default.agent` that leaves a block under a rule now running another kind.

The block an attempt runs with is the top-level block of the rule's kind with the rule's block laid over it, one level deep:

- A key the rule writes replaces the inherited value whole, whatever its type; maps are not merged and lists are not appended.
- A key the rule writes as null removes the inherited key, so the adapter applies its own default.
- Every other key is inherited.
- A kind with no top-level block inherits nothing, and the rule's block is the whole block.
- A block is laid only over the top-level block of its own kind.

No key is defaulted or coerced before the overlay. `$VAR` references in a rule's block resolve as they do in a top-level block. A block for a removed agent kind converts together with the kind; a conversion that would change the command the replacement kind launches fails the load, because a rule cannot set a command.

**Match-block keys and semantics**

Match keys are evaluated with AND semantics across keys and OR semantics within a single key. String-valued keys accept either a single string or a list of strings; a scalar is treated as a one-element list. Comparisons against `issue_type`, `assignee`, and `title` are case-insensitive:

- `labels` (string or list of glob patterns): matches when the issue carries at least one label matching any pattern; glob syntax (e.g. `bug/*`, `*-urgent`).
- `issue_type` (string or list of strings): case-insensitive equality match against the issue type field; matches when the issue type equals any list entry.
- `priority` (predicate object): numeric comparison via one operator key: `eq`, `in`, `lt`, `lte`, `gt`, or `gte`. The predicate object MUST have exactly one operator key.
- `identifier` (string or list of glob patterns): matches against the issue identifier string.
- `assignee` (string or list of strings): case-insensitive equality match against the issue assignee; matches when the assignee equals any list entry.
- `title` (string or list of phrases): matches when any phrase appears in the issue title as whole words. A `title` key that is null, bare, or an empty list is a configuration error, unlike the other list-valued keys, where it leaves the key out of the match, so a rule whose only key is `title` is never a catch-all. A phrase that is empty or only white space is a configuration error. Phrases are kept as written; normalization happens at match time:
  - Every case form of a letter compares equal on both sides, so σ, ς, and Σ match one another, as do s, S, and ſ. This is simple case folding, not full case folding: ß stays distinct from ss. The emoji variation selectors U+FE0E and U+FE0F are dropped on both sides, each run of white space becomes one space, and white space at either end is dropped. Nothing else is folded: no Unicode normalization form, no accent folding, no stemming, and no other variation selector or combining mark is dropped.
  - A word is a run of letters, digits, and combining marks; every other character, underscore and hyphen included, separates words. An occurrence of a phrase in the title matches only where it neither starts nor ends inside a word. A phrase whose first character is not part of a word carries no constraint at its start, and one whose last character is not part of a word is constrained at its end only by a combining mark that follows it in the title. Every occurrence is tried, overlapping ones included.
  - Han, Hiragana, Katakana, Thai, Lao, Khmer, and Myanmar are written without spaces between words, so each character of these scripts is a word of its own. Hangul is not in this set. A combining mark always joins the character before it, in every script, and is never a word boundary.
  - The phrase search is literal: `*`, `?`, and brackets match only themselves, unlike in `labels` and `identifier`. There is no regular expression and no wildcard.
  - An issue with an empty title never matches.

**Resolution semantics and freeze-on-dispatch invariant**

First-match wins: evaluation stops at the first rule whose `match` block succeeds. Absent rule fields fall through to `dispatch.default`, then to the top-level `agent.kind` and the Markdown-body template (the pre-dispatch top-level defaults).

The default agent kind is `dispatch.default.agent` when set, and `agent.kind` otherwise. It is the kind every selection without an agent of its own runs, and the only kind that launches `agent.command` as written.

The resolved `(agent_kind, template_id, rule_name)` is recorded on `RunningEntry` at dispatch and propagated through `RetryEntry`. The selection is frozen per claim; the settings are not. Every attempt, whether the first dispatch, a retry, or a reaction continuation, resolves its settings block from the configuration in force when it starts: the top-level block of the frozen kind with the frozen rule's block laid over it. A running session never changes settings. When no rule carries the frozen name any longer, or the rule no longer carries a block for the frozen kind, the attempt runs on the top-level block and logs one `Info` record naming the rule. A retry or reaction-driven continuation does not re-evaluate rules while the configuration in force still launches its frozen selection. At each retry timer the orchestrator selects again from the configuration in force:

- The frozen selection stands, with its session identifier, when its kind is still named by `agent.kind`, `dispatch.default.agent`, or a rule, and its template is still held. The kind launches its own command as the configuration now states it.
- A frozen kind that a conversion record retired is replaced by its replacement kind, keeping the frozen template and rule name.
- Otherwise the issue is routed afresh by first-match rule evaluation. This covers a kind the configuration no longer names, a kind this binary does not register, and a template the workflow no longer holds.
- A selection that differs from the frozen one in kind or template starts without a resume session identifier, because session identifiers are adapter-specific. The continuation context, reaction kind, attempt number, and last SSH host carry over.
- When the selected kind's adapter is unavailable because its construction failed at startup, the retry is rescheduled with backoff and keeps its claim and continuation (§8.4).

A change of resolved settings between attempts never clears the session identifier; only a changed kind or template does. A selection the retry changes emits one `Info` record (§13.1).

An attempt whose resolved block fails an error-severity settings check starts no session (§8.4).

**Template lifecycle**

Per-rule template paths are relative to `filepath.Dir(workflow_path)`. Absolute paths, `~`-prefixed paths, and symlink escapes outside the workflow directory tree are rejected at load time. The `ResolveRule` function and full algorithm details are in §5.3.9's source spec.

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

#### 5.3.10 `notifications` (list, optional)

The `notifications` list configures every outbound message Sortie sends to a destination other than the issue's own tracker fields: Slack and webhook backends, and the built-in `tracker_comment` destination that posts comments on the tracker issue. Each entry names the event types it receives. Two producers feed the list: the agent, through the `notify_operator` tool (Section 10.4.5), which produces `agent.message`, and the orchestrator, which produces every other event in the catalog below (Section 10.4.7). The tool is registered only when at least one entry receives `agent.message`, so the agent is never offered a tool it cannot use.

The value is a sequence, not a single object. Each entry is a map carrying a required `kind` discriminator, an `events` list, and that backend's own fields. A second channel is a second list entry.

```yaml
notifications:
  - kind: tracker_comment
    events: [session.completed, session.stopped, session.failed, escalation.ci_failure, auto_merge.merged, budget.held]
  - kind: slack
    webhook_url: $SORTIE_SLACK_WEBHOOK_URL
    events: [agent.message, session.stopped, session.failed]
    max_per_session: 20
  - kind: webhook
    url: $SORTIE_OPS_WEBHOOK_URL
    events: [session.started, session.completed, session.stopped, session.failed]
```

Per-entry fields:

- `kind` (string)
  - Required. The registry discriminator, resolved against the notifier registry (Section 10.4.7). v1 backends are `webhook` and `slack`. The reserved value `tracker_comment` selects the built-in destination and is not a registry entry.
- `events` (list of strings)
  - The event types the entry receives, listed explicitly. There is no wildcard and no filter beyond the event type.
  - Optional on a registered kind: an entry that omits it receives `agent.message` only, as every entry did before the key existed, and draws a deprecation advisory that suggests `events: [agent.message]`.
  - Required on `tracker_comment`. An empty list (`[]`) means only the subscriptions synthesized from deprecated settings.
  - Each element MUST be a string naming a type in the catalog, and no type repeats. `agent.message` MUST NOT appear on `tracker_comment`.
- `max_per_session` (integer, optional)
  - The `notify_operator` call cap for the whole agent run, shared by every tool server process of the dispatch (Section 10.4.5). It is not a per-entry default: an omitted, `null`, or `0` value contributes nothing to cap selection, which then falls back to the default of `20` only when every entry that receives `agent.message` is `0` or unset. `0` never means unlimited. A negative value is rejected at config parse time. The value has no effect on an entry that does not receive `agent.message`, and an orchestrator event never counts against it.
- backend-specific fields
  - Passed through to the backend constructor untyped, with `$VAR` references resolved. The `webhook` backend requires `url`; the `slack` backend requires `webhook_url`. A `tracker_comment` entry carries no key other than `kind` and `events`.

**Event catalog.** The catalog is closed and lives in the domain model. Validation accepts exactly these names.

| Event type | Meaning |
|---|---|
| `session.started` | A session was dispatched on an issue whose dispatch drives issue state |
| `session.completed` | A worker exited normally |
| `session.stopped` | A worker exited on a soft stop: `blocked`, `needs-human-review`, or `no-change-needed`; it carries the agent's reason when the agent wrote one (Section 21.1) |
| `session.failed` | A worker exited with an error, or its handoff was withheld by the evidence verdict |
| `escalation.ci_failure`, `escalation.review_comments`, `escalation.bot_review`, `escalation.merge_conflicts`, `escalation.auto_merge`, `escalation.merge_completion` | The matching reaction (Section 5.3.8) handed its subject to a person, under any `escalation` value |
| `auto_merge.merged` | The auto-merge reaction merged a pull request |
| `budget.held` | An issue was held out of dispatch by `agent.max_sessions` or `agent.max_tokens` |
| `agent.message` | The agent called `notify_operator` |

**The `tracker_comment` destination.** The destination posts an event's body as a comment on the event's issue through the tracker adapter, and renders nothing else of the event except the agent's reason on a `session.stopped` event, which it posts after the body as one literal block (Section 10.4.7, Section 21.1). It is available only when `tracker.kind` is configured. At most one entry has this kind. The destination receives the union of three sets: the `events` of its explicit entry, the subscriptions synthesized from `tracker.comments` and from `escalation: comment` (Section 5.3.1, Section 5.3.8), and, while no explicit entry exists, implicit subscriptions to `auto_merge.merged` and `budget.held`. The union is monotone: writing the new form never withdraws a comment an old key enabled, and one event is delivered at most once to a destination however many subscriptions select it. An operator who keeps `tracker.comments.on_completion: true` and also lists `session.completed` on a `tracker_comment` entry gets one comment per completion.

**An explicit entry is authoritative for the two formerly ungated comments.** The auto-merge success comment and the budget-hold comment were posted without any configuration gate. They keep posting through implicit subscriptions only while no `tracker_comment` entry exists. An entry is the operator's own statement of what the issue receives, so it receives `auto_merge.merged` and `budget.held` only when it lists them. With no tracker configured no implicit destination exists.

A front matter fragment written in the old form, with completion and failure comments, a commenting CI reaction, and one Slack channel for the agent:

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

The same comments and messages in the new form. The two formerly ungated events are listed because an explicit entry ends their implicit subscriptions; neither feature is configured here, so they post nothing until it is:

```yaml
tracker:
  kind: github
  api_key: $SORTIE_GITHUB_TOKEN
  project: acme/billing-api

reactions:
  ci_failure:
    provider: github
    escalation: none                 # no label; the comment comes from the subscription below

notifications:
  - kind: tracker_comment
    events: [session.completed, session.stopped, session.failed, escalation.ci_failure, auto_merge.merged, budget.held]
  - kind: slack
    webhook_url: $SORTIE_SLACK_WEBHOOK_URL
    events: [agent.message]
```

**Delivery.** The orchestrator delivers its events to Slack and webhook destinations from the main process. A destination for orchestrator events is an entry of a registered kind whose `events` hold at least one orchestrator event; the main process builds it with the backend constructor the sidecar uses. A failed delivery to one destination is logged and does not prevent delivery to the others, and nothing is retried (Section 10.4.7). The orchestrator never claims a notification slot, so its events cannot interact with the `notify_operator` cap.

Validation and resolution:

- The list is structurally validated when the config is parsed: a non-sequence value, an entry that is not a map, an entry with an empty `kind`, or a negative `max_per_session` aborts config construction (Section 6.3).
- `events` is validated at the same point. A non-sequence value, a non-string element, a name outside the catalog, a repeated name, `agent.message` on `tracker_comment`, a `tracker_comment` entry without `events`, a second `tracker_comment` entry, a `tracker_comment` entry while `tracker.kind` is empty, and a `tracker_comment` entry carrying any key other than `kind` and `events` each abort config construction.
- Workflow validation constructs every destination for orchestrator events, so an unknown `kind` or a required secret that resolves to the empty string on such an entry fails startup and rejects a reload (Section 6.2).
- When more than one entry receives `agent.message`, the effective cap is the maximum non-zero `max_per_session` across those entries, falling back to the default when each is `0` or unset. The cap belongs to the dispatch (Section 10.4.5): a runtime that starts a new tool server process for each turn shares the same count across every turn of the run rather than restarting it.
- A backend secret SHOULD be given as a reference to a `SORTIE_`-prefixed environment variable (`$SORTIE_NAME` or `${SORTIE_NAME}`). The `notify_operator` tool runs in a separate `sortie mcp-server` process whose environment is constructed by the agent's MCP host. The orchestrator guarantees that only its `SORTIE_`-prefixed variables are propagated into that process for `$VAR` resolution; the host MAY additionally inherit other variables from its own environment, so a reference without the prefix is not guaranteed to resolve and MAY resolve to the empty string. References are expanded against the sidecar process environment with no prefix enforcement, so the `SORTIE_` prefix is the way to guarantee a secret resolves regardless of host. When a required secret resolves to the empty string, the backend rejects it, which surfaces as a fatal sidecar startup error rather than a notification posted nowhere. The prefix is mandatory for an entry that receives `agent.message`, because the sidecar builds it; an entry that receives only orchestrator events is built only in the main process.

**Deprecation advisories.** Each deprecated form the loaded configuration relies on produces one configuration advisory that names its replacement. An advisory is logged once per configuration change, appears as a warning in `sortie validate` without changing `valid` or the exit status, and appears in the dry run. Nothing is logged per event. The conditions are:

- `tracker.comments.on_dispatch`, `on_completion`, or `on_failure` resolves to `true`, from the file or from its environment override.
- A reaction with a non-empty `provider` among `ci_failure`, `review_comments`, `bot_review`, `merge_conflicts`, `auto_merge`, and `merge_completion` sets `escalation: comment`.
- `tracker.kind` is configured, no `tracker_comment` entry exists, and `reactions.auto_merge.provider` is non-empty: the auto-merge success comment posts only through the implicit subscription.
- `tracker.kind` is configured, no `tracker_comment` entry exists, and `agent.max_sessions` or `agent.max_tokens` is above `0`: the budget-hold comment posts only through the implicit subscription.
- An entry that is not `tracker_comment` omits `events`.

No removal date or removal version is set for any deprecated form.

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
  - `is_continuation` (boolean): true when this is a continuation turn in a multi-turn session, as distinct from a retry after an error.

Fallback prompt behavior:

- If the workflow prompt body is empty, the runtime may use a minimal default prompt.
- Workflow file read/parse failures are configuration/validation errors and should not silently fall back to a prompt.

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

