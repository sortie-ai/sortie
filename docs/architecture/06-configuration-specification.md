## 6. Configuration Specification

### 6.1 Source Precedence and Resolution Semantics

Configuration precedence (highest to lowest):

1. Workflow file path selection (runtime setting -> cwd default).
2. `SORTIE_*` real environment variables (curated set; see below).
3. `.env` file values (opt-in via `SORTIE_ENV_FILE` env var or `--env-file` CLI flag).
4. YAML front matter values.
5. Environment indirection via `$VAR_NAME` inside selected YAML values; applies only to values
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

- Path fields support:
  - `~` home expansion
  - `$VAR` expansion for env-backed path values
  - Apply expansion only to values intended to be local filesystem paths; do not rewrite URIs or
    arbitrary command strings.

### 6.2 Dynamic Reload Semantics

Dynamic reload is required:

- Sortie watches `WORKFLOW.md` for changes.
- On change, it re-reads and re-applies workflow config and prompt template without restart.
- Sortie adjusts live behavior to the new config (for example polling cadence, concurrency limits,
  active/terminal states, agent settings, workspace paths/hooks, the workspace retention window,
  and prompt content for future runs).
- Reaction configuration is the documented exception: it is captured once at orchestrator
  construction and is not rebuilt on reload, so a change to a reaction block, including whether a
  kind is active at all, takes effect only on the next restart (Section 11G.3). The CI-failure kind
  is the one carve-out: its configuration is folded into the CI feedback block, which reconciliation
  re-reads from the current snapshot on every tick, so its fields do reload.
- Primary-dispatch parking after repeated withheld handoffs takes its label name from the captured
  `reactions.review_comments.escalation_label` value. This is a name lookup only: it does not require
  the review-comments reaction to be enabled and does not inherit that reaction's `escalation`
  action. Because the source value is reaction configuration, changing it takes effect only after a
  restart as well.
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
- A `tracker`, `agent`, `ci_feedback` or `notifications` key the config layer reads as a string,
  together with `db_path`, is rejected when its value carries another YAML type, distinct from the
  diagnostic for an absent key. This verdict is identical at startup, at `sortie validate`, and on
  the reload fail-safe path, because all three read the same config-construction result. The
  `workspace.root`, hook-script and reaction-escalation keys are not covered: a wrong-typed value
  there still reads as the empty string.
- `tracker.kind` is present and supported.
- `tracker.api_key` is present after `$` resolution, when required by the selected tracker adapter.
- `tracker.project` is present when required by the selected tracker adapter.
- `agent.command` is present and non-empty when `agent.kind` requires a local command.
- Tracker adapter for the configured `tracker.kind` is registered and available.
- Agent adapter for the configured `agent.kind` is registered and available.
- `agent.kind.session_resume`: for every agent kind the effective configuration can reach, the
  adapter's declared blocking key is evaluated against that kind's resolved pass-through, and
  the configuration is refused when the key is non-empty.
- `dispatch.agent.missing_block`: for every registered agent kind named by `dispatch.default.agent`
  or by `dispatch.rules[*].agent` that differs from the top-level `agent.kind`, the workflow front
  matter must carry a top-level settings block for that kind. `agent.kind` itself is never covered:
  a top-level `agent.kind` with no matching block is the ordinary minimal workflow. The check is
  skipped for a kind the agent registry reports unregistered, since that is already reported as an
  unrecognized kind when the workflow loads. A block present but empty, or present as a bare key
  with nothing following, satisfies the requirement; a block present as a scalar or a list does not.
- Rule-set validation: `dispatch.rules` parses; every referenced `agent` kind is registered;
  every referenced per-rule template path resolves and parses; no rule name is duplicated;
  no non-final rule is a catch-all; every match key is recognized; every glob pattern is
  syntactically valid; every priority predicate has exactly one operator key.
- `tracker.handoff_state` and `tracker.in_progress_state` are re-checked against the effective
  state lists. An empty `tracker.active_states` or `tracker.terminal_states` takes the tracker
  adapter's declared fallback list, so a collision the config layer cannot see (because it rules
  on the lists as written) is reported here.
- `tracker.no_change_state`, unlike the two fields above, is validated entirely when configuration
  is parsed (Section 5.3.1) and is not re-checked here: it must equal `tracker.handoff_state` or
  name a member of `tracker.terminal_states` exactly as written in front matter, with no fallback
  to the tracker adapter's declared default state lists. This keeps `sortie validate` offline for
  this field, at the cost of not catching a collision that only the adapter's fallback list would
  expose.
- `tracker.handoff_evidence` is validated while configuration is parsed as the closed set
  `observed`, `strict`, and `off`. This is a shape check and never contacts the tracker, so the same
  invalid value is rejected by startup, reload, and `sortie validate`.
- `reactions.merge_completion.target_state`, checked once at construction against
  `tracker.handoff_state`, `tracker.active_states`, and `tracker.terminal_states`: required and
  non-empty when `reactions.merge_completion.provider` is set, must not equal
  `tracker.handoff_state` (case-insensitive), must not be a member of `tracker.active_states`
  (case-insensitive; the tracker adapter's fallback active-state list applies when that field is
  empty), and must be a member of `tracker.terminal_states` exactly as written in configuration
  (case-insensitive), with no fallback to the adapter's default terminal-state list. Reaction
  configuration is not rebuilt on `WORKFLOW.md` reload (Section 6.4), so this check runs at
  startup and in `sortie validate`, never on a dispatch tick.
- `workspace.root_writable`: when `workspace.root` is set, the directory must be writable; a
  missing directory is created.

Effort-budget and notification config are validated outside this preflight, by design:

- The per-issue token ceiling (`agent.max_tokens`) is not a scheduler preflight check. It is a
  re-dispatch gate evaluated on both the retry path and the poll tick's own rebuild, alongside
  `agent.max_sessions` (Section 8.4), so the ceiling stops a blocked issue from being
  re-dispatched rather than failing startup or a poll tick. Config-level validation rejects a
  negative `agent.max_tokens` when the config is parsed, which both startup validation and the
  live-reload fail-safe path consume.
- The `notifications` backend list (Section 5.3.11) is structurally validated when the config is
  parsed: the value must be a sequence, every entry must carry a non-empty string `kind`, and
  `max_per_session`, when present, must be a non-negative integer. `max_per_session` is optional:
  an omitted, `null`, or `0` value is accepted and selects the default cap. A malformed section
  aborts config construction. Backend resolution (an unknown `kind` or a required secret that resolved to the
  empty string) is validated separately at `sortie mcp-server` sidecar startup, where it is a
  fatal startup error rather than a partial registration (Section 10.4.5). The scheduler preflight
  does not resolve notifier backends, because the orchestrator process never delivers
  notifications; the sidecar does.

**Startup token-scope preflight**

When `reactions.auto_merge.provider` is non-empty at startup, the orchestrator invokes the SCM
adapter's scope-verification path before the first reconcile tick. The verifier reads OAuth scopes
from `GET /rate_limit` response headers and validates that the token carries `pull_requests:write`
and, when `reactions.auto_merge.delete_branch != false`, `contents:write`. A classic `repo` scope
satisfies both. Fine-grained PAT permission names are accepted.

Fail-open paths: the preflight does NOT block startup when scope information is unavailable.
Two paths fail open and let auto-merge proceed:

- The configured SCM adapter does not implement the scope-verifier interface
  (`AutoMergeScopeVerifier`). The orchestrator logs WARN that the preflight was skipped and
  treats the check as passed.
- The provider returns no scope information (an empty scopes list and no missing entries with no
  error). This is the normal response for fine-grained PATs and GitHub App installation tokens,
  whose tokens do not populate `X-OAuth-Scopes`. The orchestrator logs WARN that scope
  verification was skipped and treats the check as passed. The runtime auth-failure path on the
  first `MergePR` attempt surfaces any genuine scope gap as `ErrSCMAuth`, with deduplicated
  ERROR logging.

Auth-class sticky posture: a missing-scope failure (the verifier returns one or more missing
scope names) is an operator configuration error. The orchestrator logs ERROR once at startup
with the missing scope name and continues running (it does NOT `os.Exit`).
`state.AutoMergePreflightFailed` is set to true and remains true for the lifetime of the
process. Every subsequent `reconcileAutoMerge` tick drops `merge`-kind pending entries with a
WARN log. Operator recovery requires a token rotation and an orchestrator restart.

Bounded transport-class retry: a transport-class failure on the preflight call is environmental.
The orchestrator logs WARN once, sets `state.AutoMergePreflightFailed = true` and
`state.AutoMergePreflightRetryDueAt = startTime + AutoMergePreflightRetryDelay` (default 5
minutes), and returns. The scheduled retry runs once, on the first `reconcileAutoMerge` tick
whose `state.NowFunc().UTC() >= state.AutoMergePreflightRetryDueAt`. Success clears both fields.
Another transport failure leaves the sticky flag set and clears the retry timestamp (no further
retries this lifetime). The retry timer is consumed by the existing reconcile loop, not by a new
goroutine or ticker.

The asymmetry between auth-class and transport-class postures is intentional: auth failures are
sticky because the orchestrator cannot self-heal a configuration error; transport failures get one
bounded retry because they are environmental and the operator's restart lever remains the
documented escape hatch.

For cross-reference, see §11C.9.

### 6.4 Config Fields Summary (Cheat Sheet)

This section is intentionally redundant so a coding agent can implement the config layer quickly.

- `tracker.kind`: string, required, no default (e.g., `jira`)
- `tracker.endpoint`: string, adapter-defined default
- `tracker.api_key`: string or `$VAR`, required when the tracker adapter declares it
- `tracker.project`: string, required when the tracker adapter requires project scoping
- `tracker.active_states`: list of strings, defaults to empty; an empty value leaves the tracker
  adapter to apply its own internal fallback list
- `tracker.terminal_states`: list of strings, defaults to empty; an empty value leaves the tracker
  adapter to apply its own internal fallback list; must be written non-empty in front matter when
  `reactions.merge_completion.provider` is set, because `target_state` is checked against the list
  exactly as written in configuration (case-insensitive), with no fallback to the adapter's
  default terminal-state list in that case
- `tracker.query_filter`: string, optional, default empty (adapter-defined filter fragment)
- `tracker.handoff_state`: string, optional, default absent; target state for
  orchestrator-initiated handoff after a worker run whose exit disposition and handoff-evidence
  policy permit the write; must not collide with
  `active_states` or `terminal_states`, evaluated against the effective lists, so the tracker
  adapter's fallback participates whenever the matching field is empty; required, non-empty, when
  `reactions.merge_completion.provider` is set; supports `$VAR`
- `tracker.handoff_evidence`: string, default `observed`; policy governing the evidence condition
  on an otherwise-eligible handoff. `observed` withholds only when absence of work is positively
  observed and allows an undeterminable verdict; `strict` also withholds an undeterminable verdict;
  `off` restores the prior four-condition decision and performs no baseline capture, exit-time
  workspace inspection, or evidence logging. Values outside `observed`, `strict`, and `off` are
  rejected offline. The policy is frozen for each run before its baseline decision, so a reload
  applies to future run launches and does not change an in-flight run's evidence contract
- `tracker.no_change_state`: string, optional, default `tracker.handoff_state`; target state for a
  worker run whose agent declared, through `.sortie/status`, that the requested outcome already
  held and nothing needed changing; requires `tracker.handoff_state` to be non-empty; must equal
  `tracker.handoff_state` (case-insensitive) or name a member of `tracker.terminal_states` exactly
  as written in front matter (case-insensitive), with no fallback to the adapter's default
  terminal-state list; supports `$VAR` and the `SORTIE_TRACKER_NO_CHANGE_STATE` environment
  override; changes take effect for future worker exits, not in-flight sessions
- `tracker.in_progress_state`: string, optional, default absent; target state for
  dispatch-time transition at the start of each worker attempt; must be in `active_states`,
  must not collide with `terminal_states` or `handoff_state`, and the `terminal_states` rule is
  evaluated against the effective list, so the tracker adapter's fallback participates when that
  field is empty; supports `$VAR`
- `tracker.api_version`: string (`"2"` or `"3"`), optional, default `"3"`; selects
  Jira REST API v3 (Cloud) or v2 (Server / Data Center); quote the value to avoid
  a validation advisory (`api_version: "2"`); supports `$VAR`; the Jira adapter's
  offline configuration diagnostics reject a value outside `"2"` and `"3"`, and reject
  `"2"` against an Atlassian Cloud endpoint, so `sortie validate` reports both faults
  without a network call
- `polling.interval_ms`: integer, default `30000`
- `workspace.root`: path, default `<system-temp>/sortie_workspaces`
- `workspace.retention_days`: integer, default `0` (disabled); the maximum age in days of a swept
  workspace's latest recorded activity before the periodic sweep removes it; `0` disables the
  bound, a value from `1` to `29` is rejected, and `30` (`WorkspaceRetentionMinDays`) is the
  smallest permitted non-zero value; expressed in days rather than milliseconds because every
  other duration field is a sub-hour timing where the millisecond unit is proportionate to the
  value, while a thirty-day window in milliseconds is unreadable and a dropped digit is
  destructive; supports the `SORTIE_WORKSPACE_RETENTION_DAYS` environment override; validated
  offline when the config is parsed, so both startup and the live-reload fail-safe path reject an
  out-of-range value; reloads dynamically, taking effect on the next sweep pass with no restart
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
- `agent.command`: whitespace-delimited argument-vector string, adapter-defined default
- `agent.turn_timeout_ms`: positive integer, default `3600000`; the deadline the orchestrator
  places on the context of every agent turn it runs, self-review turns included; the expiry of
  that deadline cancels the turn's context, and the attempt then fails with the `turn_timeout`
  failure class, retryable per the retry path, once the adapter returns from the cancelled turn,
  which for an adapter that does not observe a done context promptly is later than the deadline;
  a non-positive value is rejected when the config is parsed, so startup, `sortie validate`, and
  the live-reload fail-safe path all reject it
- `agent.read_timeout_ms`: integer, default `5000`
- `agent.stall_timeout_ms`: integer, default `300000`
- `agent.max_concurrent_agents`: integer, default `10`
- `agent.max_turns`: integer, default `20`
- `agent.max_retry_backoff_ms`: integer, default `300000` (5m)
- `agent.max_concurrent_agents_by_state`: map of positive integers, default `{}`
- `agent.max_sessions`: non-negative integer, default `0` (unlimited); the total per-issue session
  budget. The separate `agent.max_consecutive_absences` governs the consecutive-absence ceiling
- `agent.max_tokens`: integer, default `0` (unlimited)
- `agent.max_consecutive_absences`: positive integer, default `3`; `0` and negative values are
  rejected as a configuration error. Bounds the count of consecutive observed absences, not
  lifetime effort: any run that produces evidence of work resets the count to zero. Unreachable
  under `tracker.handoff_evidence: off`, since no verdict is computed and no absence is ever
  recorded. The separate `agent.max_sessions` governs the total per-issue session budget
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
- `reactions.<kind>.max_retries`: integer, default `2`, except `merge_conflicts`, which defaults to
  `1`; fix continuation attempts before escalation. Not consumed by `review_comments` or
  `bot_review`, each of which bounds its dispatches with its own `max_continuation_turns`
- `reactions.<kind>.escalation`: string, default `label`; `label` or `comment`
- `reactions.<kind>.escalation_label`: string, default `needs-human`. Primary-dispatch parking uses
  the resolved non-empty `reactions.review_comments.escalation_label`; when that block or value is
  absent or empty it uses `needs-human`. The lookup ignores
  `reactions.review_comments.provider`, ignores whether its `escalation` value is `label` or
  `comment`, and never falls through to another reaction kind's label. The primary path always
  parks by label; it borrows only this label name and no other review-reaction behavior
- `reactions.ci_failure.max_log_lines`: integer, default `50`; non-negative; `0` fetches no log
  excerpt; the CI provider is constructed once with this value, so a change takes effect only
  after a restart
- `reactions.ci_failure.watch_window_ms`: integer, default `86400000` (24 h); non-negative, not
  above `9223372036854`; `0` removes the bound; re-read on every tick
- `reactions.review_comments.poll_interval_ms`: integer, default `120000` (2 min); minimum `30000`
- `reactions.review_comments.debounce_ms`: integer, default `60000` (60 sec); non-negative
- `reactions.review_comments.max_continuation_turns`: integer, default `3`; positive
- `reactions.review_comments.watch_window_ms`: integer, default `1800000` (30 min); non-negative,
  not above `9223372036854`; `0` removes the bound; not rebuilt on `WORKFLOW.md` reload
- `reactions.bot_review.poll_interval_ms`: integer, default `60000` (1 minute); minimum `30000`;
  not rebuilt on `WORKFLOW.md` reload
- `reactions.bot_review.max_continuation_turns`: integer, default `5`; positive; not rebuilt on
  `WORKFLOW.md` reload
- `reactions.bot_review.bot_usernames`: list of strings, default empty; allowlist of bot logins,
  matched case-insensitively; a value that is not a list, or a list holding a non-string element,
  is rejected; not rebuilt on `WORKFLOW.md` reload
- `reactions.bot_review.watch_window_ms`: integer, default `1800000` (30 min); non-negative, not
  above `9223372036854`; `0` removes the bound; not rebuilt on `WORKFLOW.md` reload
- `reactions.merge_conflicts.poll_interval_ms`: integer, default `60000` (1 minute); minimum
  `30000`; not rebuilt on `WORKFLOW.md` reload
- `reactions.merge_conflicts.watch_window_ms`: integer, default `1800000` (30 min); non-negative,
  not above `9223372036854`; `0` removes the bound; not rebuilt on `WORKFLOW.md` reload
- `reactions.auto_merge.strategy`: string, default `squash`; one of `merge`, `squash`, `rebase`
- `reactions.auto_merge.require_ci`: boolean, default `true`
- `reactions.auto_merge.delete_branch`: boolean, default `true`
- `reactions.auto_merge.poll_interval_ms`: integer, default `60000` (1 minute); minimum `30000`
- `reactions.auto_merge.watch_window_ms`: integer, default `1800000` (30 min); non-negative, not
  above `9223372036854`; `0` removes the bound; not rebuilt on `WORKFLOW.md` reload
- `reactions.merge_completion.target_state`: string, required when
  `reactions.merge_completion.provider` is set, no default; the single tracker terminal state the
  linked issue moves to once its managed pull request merges; must not equal
  `tracker.handoff_state` (case-insensitive); must not be a member of `tracker.active_states`
  (case-insensitive, falling back to the tracker adapter's default active-state list when that
  field is empty); must be a member of `tracker.terminal_states` exactly as written in
  configuration (case-insensitive), with no fallback to the adapter's default terminal-state
  list; not rebuilt on `WORKFLOW.md` reload; see §11G
- `reactions.merge_completion.poll_interval_ms`: integer, default `60000` (1 minute); minimum
  `30000`
- `reactions.label_commands.provider`: string, optional; SCM adapter identifier; absent or empty
  makes the block inert and leaves its other fields unvalidated. The block parses through its own
  path rather than the generic per-kind schema, so it carries no `max_retries`, `escalation`, or
  `escalation_label`; not rebuilt on `WORKFLOW.md` reload; see §11F
- `reactions.label_commands.review_label`: string, default `sortie:review`; an explicit empty
  string disables the read-only review command; not rebuilt on `WORKFLOW.md` reload
- `reactions.label_commands.fix_label`: string, default `sortie:fix`; an explicit empty string
  disables the fix command; not rebuilt on `WORKFLOW.md` reload. An active provider with both
  command labels empty is rejected as a configuration error
- `reactions.label_commands.poll_interval_ms`: integer, default `60000` (1 minute); a value below
  `30000` is clamped up to that floor with a warning rather than rejected; not rebuilt on
  `WORKFLOW.md` reload
- `dispatch.rules`: list of rule objects, optional; first-match-wins routing; see §5.3.10
- `dispatch.default.agent`: string, optional; default agent kind when no rule matches; falls through to top-level `agent.kind`
- `dispatch.default.template`: path, optional; default template when no rule matches; falls through to the Markdown body
- `notifications`: list of notifier backend objects, optional; default empty; configures the
  backends behind the `notify_operator` tool (Section 5.3.11). An empty or absent list leaves
  the tool unregistered
- `notifications[].kind`: string, required per entry; registry discriminator; v1 backends are
  `webhook` and `slack`
- `notifications[].max_per_session`: integer, optional; per-session `notify_operator` call cap;
  not a per-entry default; omitted/`null`/`0` contributes nothing and the cap falls back to `20`
  only when every entry is `0` or unset; never unlimited; negative is rejected
- `notifications[].<backend fields>`: pass-through per `kind`; `webhook` requires `url`, `slack`
  requires `webhook_url`; secrets SHOULD be `$SORTIE_*` references (only those are guaranteed
  propagated to the sidecar), resolved at sidecar startup
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
