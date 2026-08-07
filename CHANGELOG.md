# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `sortie stats` subcommand: summarizes how past runs went and what they
  cost, opening the database read-only so it never blocks a running
  orchestrator. `--format text|json` selects the output; `--since` and
  `--until` bound the report by when a run finished, accepting an exact
  timestamp, a `YYYY-MM-DD` date, or an age such as `24h`. The report
  breaks runs down by outcome, by coding agent, by dispatch rule, and by
  prompt template, with run counts, success rate, p50/p95/mean duration,
  mean turns, and token totals for each. When the workflow configures
  `token_rates`, USD cost is derived through the same formula and
  renderers the dashboard uses, reported as total spend and as spend per
  succeeded run; without `token_rates` the report shows token counts and
  no cost figures rather than zeros. Against a database written by an
  older binary the command still works: it reports the figures that
  database can supply and warns which ones are missing, rather than
  failing outright.
  ([#274](https://github.com/sortie-ai/sortie/issues/274))

## [1.17.0] - 2026-08-06

### Added

- GitLab tracker adapter: set `tracker.kind: gitlab` to run Sortie
  against GitLab.com or a self-managed instance, with `tracker.project`
  the target project as a `group/project` path or numeric project ID,
  `tracker.api_key` a GitLab access token, and `tracker.endpoint` the
  instance base URL (optional; defaults to `https://gitlab.com`, and
  `/api/v4` is appended when absent). It runs the same autonomous
  workflow as the Jira, GitHub, Linear, and Gitea adapters: candidate
  polling, handoff transitions, lifecycle comments, and CI-failure
  escalation labels. State is label-driven through `active_states`,
  `terminal_states`, and `handoff_state`; a transition to a terminal
  state closes the issue and a transition back to an active state
  reopens it, escalation labels are attached without disturbing the
  issue's other labels, and a state label the project does not yet hold
  is created on first use. At startup the adapter checks the project and
  reads its label catalog, so a bad token, a wrong project, or an
  unreachable instance fails before the first poll. Because GitLab
  issues carry no priority field and Community Edition has no blocking
  relationship, `issue.priority` and `issue.blocked_by` are always empty
  in prompt templates. `tracker.query_filter` takes a GitLab issue-list
  query fragment (for example
  `assignee_username=review-bot&labels=ready`, `not[...]` negation
  included) to scope candidate polling to matching issues; the
  adapter-owned keys `state`, `issue_type`, `order_by`, `sort`, `page`,
  `per_page`, `pagination`, and `with_labels_details` are rejected, as
  is any key GitLab's issue list does not support, so a typo fails at
  startup instead of being silently ignored.
  ([#676](https://github.com/sortie-ai/sortie/issues/676),
  [#677](https://github.com/sortie-ai/sortie/issues/677),
  [#679](https://github.com/sortie-ai/sortie/issues/679))
- `sortie validate` GitLab adapter config validation: emits offline
  diagnostics for `tracker.kind: gitlab` covering `tracker.endpoint` URL
  shape (flagging a cleartext `http` scheme and a base URL that already
  ends in `/api/v4`), `tracker.project` as a `group/project` path or a
  numeric project ID, a `$SORTIE_GITLAB_TOKEN` environment-variable
  hint, `tracker.query_filter` syntax and adapter-owned keys, and empty,
  untrimmed, or overlapping active/terminal state labels. Errors block
  dispatch; warnings are advisory.
  ([#678](https://github.com/sortie-ai/sortie/issues/678))

## [1.16.1] - 2026-08-03

### Fixed

- `sortie validate` no longer reports `unknown template variable` for the
  reaction continuation variables (`ci_failure`, `review_comments`,
  `bot_review_comments`, `merge_conflict`, `label_review`, `label_fix`). A
  mistyped sub-field of one of them is now reported as an unknown field with
  the valid field list, and a reference to one of them from inside a
  `{{ range }}` or `{{ with }}` body is now reported as dot-context misuse.
  ([#696](https://github.com/sortie-ai/sortie/issues/696))
- `sortie validate` and startup now reject a `tracker.handoff_state` or
  `tracker.in_progress_state` that collides with the tracker adapter's own
  fallback state list when the matching workflow list is empty. Leaving
  `tracker.active_states` or `tracker.terminal_states` out no longer hides the
  collision, so a workflow that passed validation before this release can now
  fail: either write the list out or pick a state outside the adapter's
  fallback. ([#695](https://github.com/sortie-ai/sortie/issues/695))

## [1.16.0] - 2026-07-19

### Added

- Gitea SCM provider: Sortie's pull-request automation now runs against a
  self-hosted Gitea instance (Forgejo and Codeberg included), at parity with
  the GitHub SCM provider. Set `provider: gitea` on a `reactions.auto_merge`,
  `reactions.review_comments`, `reactions.bot_review`,
  `reactions.merge_conflicts`, `reactions.ci_failure`, or
  `reactions.label_commands` block to drive it through Gitea: Sortie routes
  human and bot review comments back into the agent session, reacts to merge
  conflicts, to a failing CI run on an agent's pull request (surfacing an
  excerpt of the first failing check when available), and to the
  `sortie:review` / `sortie:fix` label commands, and, with
  `reactions.auto_merge`, merges an approved pull request once its review
  decision, CI status, and mergeability satisfy the configured preconditions
  (sending the expected head SHA so a moved head aborts the merge rather than
  merging stale work) and deletes the merged branch. Auto-merge on Gitea
  requires the configured token's user to hold repository write access; a
  token that cannot push is reported at startup.
  ([#656](https://github.com/sortie-ai/sortie/issues/656),
  [#657](https://github.com/sortie-ai/sortie/issues/657),
  [#658](https://github.com/sortie-ai/sortie/issues/658))
- `sortie validate` reaction and CI feedback checks: before dispatch,
  `sortie validate` now reports a reaction or `ci_feedback` block that
  names an SCM or CI provider Sortie does not recognize, active reactions
  that disagree on the SCM provider, a `bot_review` `bot_usernames`
  allowlist that is not a list of names, and an `auto_merge` `strategy`
  that is not `merge`, `squash`, or `rebase`. These faults block dispatch,
  and apply to every SCM provider including the new Gitea one.
  ([#659](https://github.com/sortie-ai/sortie/issues/659))

## [1.15.0] - 2026-07-16

### Added

- Gitea tracker adapter: set `tracker.kind: gitea` to run Sortie against a
  self-hosted Gitea instance (Forgejo and Codeberg included), with
  `tracker.endpoint` the instance URL (required; there is no default
  host), `tracker.api_key` a Gitea access token, and `tracker.project` the
  target `owner/repo`. It runs the same autonomous workflow as the Jira,
  GitHub, and Linear adapters: candidate polling, handoff transitions,
  lifecycle comments, and CI-failure escalation labels. State is
  label-driven through `active_states`, `terminal_states`, and
  `handoff_state`; a terminal transition closes the issue and an active
  transition reopens it, and any missing state or escalation label is
  created in the repository on demand. `tracker.query_filter` takes a Gitea
  issue-list query fragment (for example
  `assigned_by=review-bot&labels=ready`) to scope candidate polling to
  matching issues; the adapter-owned keys `state`, `type`, `page`, and
  `limit` are rejected.
  ([#629](https://github.com/sortie-ai/sortie/issues/629),
  [#630](https://github.com/sortie-ai/sortie/issues/630),
  [#632](https://github.com/sortie-ai/sortie/issues/632))
- `sortie validate` Gitea adapter config validation: emits offline
  diagnostics for `tracker.kind: gitea` covering `tracker.endpoint`
  presence and URL shape (required for a self-hosted instance, which has
  no default host to fall back on), `tracker.project` as `owner/repo`, a
  `$SORTIE_GITEA_TOKEN` environment-variable hint, empty state labels,
  and active/terminal state overlap. Errors block dispatch; warnings are
  advisory.
  ([#631](https://github.com/sortie-ai/sortie/issues/631))

## [1.14.1] - 2026-07-13

### Fixed

- Failing lifecycle hooks (`after_create`, `before_run`, `after_run`,
  `before_remove`) now log their captured stdout and stderr in a
  `hook_output` attribute on the failure WARN record, so the output is
  visible at the default log level without enabling debug; previously
  the output was discarded and a failed hook, such as a `git clone` in
  `after_create`, could not be diagnosed from the logs even at
  `--log-level debug`. A hook that succeeds while printing output logs
  it at debug level on a `hook completed` record. `hook_output` keeps
  the last 8 KiB of output and starts with a truncation marker when
  longer.
  ([#643](https://github.com/sortie-ai/sortie/issues/643))

## [1.14.0] - 2026-07-11

### Added

- Bot-review reaction kind: review-bot comments (linters, static
  analyzers, security scanners, and AI reviewers) on a Sortie-created
  pull request are now detected and routed back into the agent session
  as continuation turns, separately from human review comments.
  Configure it with a `reactions.bot_review` block in WORKFLOW.md, where
  `provider` activates the kind, `bot_usernames` allowlists bot logins,
  and `max_continuation_turns`, `poll_interval_ms`, and `escalation`
  tune the retry budget, poll cadence, and handoff. A comment is
  classified as bot-authored by its platform author type or the
  allowlist, never by its content; bot comments dispatch immediately
  with no debounce window and own an independent retry budget,
  fingerprint, and escalation, so the bot-review and human-review kinds
  never interfere on the same pull request.
  ([#415](https://github.com/sortie-ai/sortie/issues/415))

- Merge-conflict reaction kind: the orchestrator now polls mergeability
  of each open Sortie-managed pull request per reconcile cycle and, on a
  no-conflict-to-conflict transition, dispatches a single continuation
  turn that rebases the PR branch onto its base and resolves the
  conflicts on the existing workspace. Configure it with a
  `reactions.merge_conflicts` block in WORKFLOW.md, where `provider`
  activates the kind and `max_retries`, `poll_interval_ms`, and
  `escalation` tune the retry budget, poll cadence, and handoff; the
  retry budget defaults lower than other reaction kinds because conflict
  resolution rarely succeeds on retry. Tracking is episodic: resolving a
  conflict resets the budget, so a later independent conflict gets a
  fresh attempt rather than immediate escalation.
  ([#416](https://github.com/sortie-ai/sortie/issues/416))

- PR label commands: apply a label to a Sortie-managed pull request to
  trigger an agent action on it. Two commands share a
  `reactions.label_commands` block in WORKFLOW.md. Applying `sortie:review`
  (the `review_label`) runs a read-only session that posts review comments
  and changes no code; applying `sortie:fix` (the `fix_label`) runs a
  session that checks out the PR branch, addresses the outstanding review
  comments, pushes the fixes, and posts a summary comment. `provider`
  activates the feature (for example `provider: github`); both labels
  default to their `sortie:` names and are active once `provider` is set,
  so disable either command by setting its label to `""`.
  `poll_interval_ms` (default 60000, minimum 30000) sets how often the
  labels are checked. Sortie removes the label once it accepts the
  command, so re-applying it after the run finishes starts a new one. The
  operator creates the labels (Sortie never creates them) and adds the
  matching `{{ if .label_review }}` or `{{ if .label_fix }}` branch to the
  prompt template.
  ([#584](https://github.com/sortie-ai/sortie/issues/584),
  [#585](https://github.com/sortie-ai/sortie/issues/585))

### Changed

- Homebrew installs now use `brew install --cask sortie-ai/tap/sortie`.
  The tap distributes a Homebrew cask covering both macOS and Linux,
  replacing the previous formula.
  ([#613](https://github.com/sortie-ai/sortie/issues/613))

## [1.13.0] - 2026-06-15

### Added

- Linear tracker adapter: configure with `tracker.kind: linear` and
  `tracker.project` set to a Linear team key (the prefix in identifiers
  such as `ABC-123`). The adapter speaks Linear's GraphQL API over a
  single endpoint and authenticates with a personal API key,
  validating the key against the workspace at construction time. It
  implements the full `TrackerAdapter` interface: cursor-paginated
  candidate fetch, issue and comment retrieval, and state reconciliation
  on the read path; `TransitionIssue`, `CommentIssue`, and `AddLabel` on
  the write path, so a Linear-backed deployment performs handoff
  transitions, posts lifecycle comments, and attaches escalation labels
  on par with the Jira and GitHub adapters. Workflow states are mapped
  by display name, matched case-insensitively and verified against the
  team at startup, rather than by Linear's immutable state `type`.
  `tracker.query_filter` accepts a Linear `IssueFilter` JSON fragment
  merged with the adapter-owned team and state constraints; a top-level
  `team` or `state` key is reserved and rejected. Linear returns
  application errors inside HTTP 200 bodies, so the adapter classifies
  the response body before any HTTP-status check; the request rate limit
  is read from response headers rather than hardcoded. Ships with an
  `examples/WORKFLOW.linear.md` sample workflow.
  ([#237](https://github.com/sortie-ai/sortie/issues/237),
  [#589](https://github.com/sortie-ai/sortie/issues/589),
  [#599](https://github.com/sortie-ai/sortie/issues/599),
  [#593](https://github.com/sortie-ai/sortie/issues/593))
- `sortie validate` Linear adapter config validation: emits offline
  diagnostics for `tracker.kind: linear` covering `tracker.project` as a
  Linear team key, a `$SORTIE_LINEAR_API_KEY` environment-variable hint,
  empty state labels, and active/terminal state overlap, matching the
  checks already provided for the Jira and GitHub adapters. Errors block
  dispatch; warnings are advisory.
  ([#590](https://github.com/sortie-ai/sortie/issues/590))

## [1.12.0] - 2026-06-12

### Added

- `cost_budget` agent tool with per-issue token budget enforcement: a
  new Tier 1 MCP tool reports cumulative token spend and remaining
  budget for the current issue so an agent can adjust strategy - skip
  expensive work, return a partial result, or hand off - before hitting
  a ceiling. It reads cumulative totals from `run_history` in read-only
  mode and returns the standard `{"success": true, "data": ...}`
  envelope. A companion hard ceiling, the new optional `agent.max_tokens`
  field (sibling to `agent.max_sessions`, default `0` for unlimited),
  blocks dispatch at preflight once an issue's cumulative token spend is
  exhausted; when the session and token budgets are exceeded on the same
  evaluation, the token reason takes precedence in the recorded
  budget-exhaustion state. `cost_budget` calls are counted on
  `sortie_tool_calls_total`.
  ([#240](https://github.com/sortie-ai/sortie/issues/240))
- `notify_operator` agent tool: a new Tier 2 MCP tool lets an agent send
  real-time notifications to operator-configured channels during a
  session - to escalate a decision, report progress on a long-running
  task, or flag a blocker - without terminating the session. Version 1
  ships Slack and generic-webhook backends, configured under a new
  optional top-level `notifications` block in `WORKFLOW.md`, with
  per-session volume bounded by `max_per_session`. Backend secrets must
  use the `$SORTIE_`-prefixed environment indirection or they stay
  invisible to the sidecar process. Envelope fields (issue, session,
  attempt, agent kind) are injected by the orchestrator and cannot be
  forged by the agent, and error paths never echo the endpoint URL or
  payload. Registration is all-or-nothing: an invalid backend config
  fails sidecar startup rather than partially registering, so the prompt
  advertisement and `tools/list` always agree. When no backend is
  configured the tool is not registered.
  ([#242](https://github.com/sortie-ai/sortie/issues/242))
- Jira adapter: Jira Server and Data Center support via a new optional
  `tracker.api_version` field. The default `"3"` targets Jira Cloud
  (REST v3) and leaves existing configurations unchanged; `"2"` targets
  Server / Data Center (REST v2), switching to `/rest/api/2` endpoints,
  offset-based search pagination, and raw issue and comment bodies in
  Jira wiki markup (the v3 ADF-to-text flattening does not run on v2, so
  descriptions reach prompts as markup rather than plain text). On v2 the
  `api_key` shape selects authentication: a colon-free value is sent as a
  Personal Access Token (`Authorization: Bearer`), while a `user:password`
  value uses HTTP Basic; Cloud v3 continues to use Basic `email:token`.
  Comment creation posts a raw `{"body": ...}` payload on v2 instead of an
  ADF document. A construction-time guard rejects inconsistent
  configuration at startup: a Cloud (`*.atlassian.net`) endpoint combined
  with `api_version: 2`, or an endpoint that is not a URL with a scheme and
  host; it also warns when a self-hosted endpoint is left on the default
  v3. A bare YAML integer (`api_version: 2`) is coerced rather than treated
  as absent, though `sortie validate` still advises quoting it.
  ([#549](https://github.com/sortie-ai/sortie/issues/549))

### Changed

- Agent tools: the built-in tools now share one uniform result
  envelope - `{"success": true, "data": <payload>}` on success and
  `{"success": false, "error": {"kind": "...", "message": "..."}}` on a
  domain failure. For operators upgrading, this changes the result shape
  of the two pre-existing Tier 1 tools: `sortie_status` and
  `workspace_history` previously returned a bare success object and a
  flat `{"error": "message"}` failure, and now nest their payload under
  `data` and report failures with a closed `error.kind`
  (`state_unavailable` / `state_malformed` for `sortie_status`,
  `query_failed` for `workspace_history`). `tracker_api`'s output is
  unchanged, and the new `cost_budget` and `notify_operator` tools adopt
  the envelope natively. Agent prompts or downstream consumers that
  parsed the previous bare or flat shapes must now read the payload under
  `data` and read failures as `error.kind` and `error.message`.
  ([#567](https://github.com/sortie-ai/sortie/issues/567))

### Fixed

- OpenCode adapter: restore the actionable "model not found" diagnostic
  on invalid-model turns. OpenCode 1.16.0 replaced its per-run
  unknown-model error with a generic masked server error, so a turn
  configured with a model absent from the catalog failed with no
  actionable detail. The adapter now detects the masked placeholder,
  queries `opencode models`, and emits a "model not found" turn failure
  when the configured model is missing - including over SSH, reusing the
  existing remote-command path.
  ([#562](https://github.com/sortie-ai/sortie/issues/562))
- Agent tool advertisement: the first-turn prompt now lists the same
  tools the MCP server serves for the session. Previously the prompt
  advertised only `tracker_api` while the sidecar also served the Tier 1
  `sortie_status` and `workspace_history` tools, so an agent that relied
  on the prompt for tool discovery was never told those tools existed.
  The orchestrator worker and the MCP sidecar now build the session tool
  set through a single shared path, keeping the advertised set and the
  MCP `tools/list` response identical.
  ([#565](https://github.com/sortie-ai/sortie/issues/565))
- Windows: workspace hook cleanup no longer fails with a sharing
  violation when a hook spawns child processes. `TerminateJobObject` and
  `KILL_ON_JOB_CLOSE` can return before dying descendants release their
  handles, so a child still holding the hook working directory open made
  the caller's cleanup fail. `RunHook` now terminates any survivors and
  polls the Job Object until its active-process count reaches zero
  (2-second cap) before returning.
  ([PR #575](https://github.com/sortie-ai/sortie/pull/575))

### Migrations

- Add token-accounting columns (`input_tokens`, `output_tokens`,
  `total_tokens`, `cache_read_tokens`) to `run_history` as
  `NOT NULL DEFAULT 0`; pre-migration rows read back as zero.

## [1.11.0] - 2026-05-29

### Added

- Kiro CLI agent adapter: configure with `agent.kind: kiro` for
  autonomous issue-to-code workflows using the Kiro CLI via
  `kiro-cli chat --no-interactive`, following the subprocess-per-turn
  model of the `claude-code` and `copilot-cli` adapters. Kiro headless
  mode emits no structured event stream and no token counts, so turn
  outcome is classified from process exit status and stderr: the
  `▸ Credits:` cost trailer marks success, an `Authentication failed.`
  line on a bare exit 0 marks failure, and signal exits map to
  cancellation. `StartSession` requires `KIRO_API_KEY` and validates it
  up front to avoid the silent device-login hang an invalid key would
  otherwise trigger. The model is pinned per turn with `--model`,
  continuation turns resume the workspace conversation with `--resume`,
  and a `kiro` passthrough config block exposes the model selector, the
  tool-trust mode (`--trust-tools` / `--trust-all-tools`), and an
  optional `--agent` selector. Because the headless path reports no
  tokens, only time-based budget enforcement applies and no
  `token_usage` events are emitted; MCP tool injection is unavailable on
  the `KIRO_API_KEY` path. `sortie validate` accepts `agent.kind: kiro`
  and flags unknown `kiro` subkeys. Ships with a companion
  `examples/docker/kiro.Dockerfile` (a glibc Debian base, since
  `kiro-cli` is dynamically linked) and an `examples/WORKFLOW.kiro.md`
  sample workflow.
  ([#515](https://github.com/sortie-ai/sortie/issues/515),
  [#517](https://github.com/sortie-ai/sortie/issues/517))
- `install.ps1` PowerShell installer for Windows: install with the
  one-liner `irm 'https://get.sortie-ai.com/install.ps1' | iex`,
  mirroring the POSIX `install.sh`. Detects architecture, resolves the
  release tag (honoring `SORTIE_VERSION` when set), downloads the
  matching `sortie_<version>_windows_<arch>.zip`, verifies its SHA-256
  against `checksums.txt` (skippable with `SORTIE_NO_VERIFY=1`),
  installs to `%LOCALAPPDATA%\Programs\sortie` by default (override with
  `SORTIE_INSTALL_DIR`), and appends the install directory to the
  User-scope `PATH`. Compatible with Windows PowerShell 5.1 and
  PowerShell 7+, depends only on built-in cmdlets, and forces TLS 1.2.
  ([#541](https://github.com/sortie-ai/sortie/issues/541))
- Authenticode-signed Windows binaries: the release pipeline now signs
  Windows `.exe` artifacts via SignPath before they are archived, so
  downloaded binaries are no longer blocked by Microsoft SmartScreen or
  Smart App Control. Signing runs as a GoReleaser post-build hook and is
  a no-op for local, snapshot, and pull-request builds.
  ([PR #548](https://github.com/sortie-ai/sortie/pull/548))

## [1.10.0] - 2026-05-27

### Added

- Auto-merge reaction for Sortie-created PRs: a new opt-in
  `reactions.auto_merge` block in `WORKFLOW.md` instructs the
  orchestrator to merge an agent-created pull request directly through
  the SCM adapter once review decision, CI conclusion, draft state,
  and mergeability all satisfy the configured preconditions. The
  reconcile loop polls every `poll_interval_ms` (default 60 s,
  minimum 30 s), calls `MergePR` with the expected head SHA to close
  the TOCTOU window, and treats an "already merged" 409 response as
  success. Merge strategy (`squash` default, also `merge` or
  `rebase`), `require_ci` (default `true`), `delete_branch` (default
  `true`), and the standard `max_retries` / `escalation` /
  `escalation_label` fields are configurable. At startup the
  orchestrator runs a one-shot scope preflight against the SCM
  provider; an auth-class failure sets a sticky
  `auto_merge_preflight_failed` flag that disables merge attempts for
  the process lifetime, while a transport-class failure schedules a
  single retry after 5 minutes. `reactions.review_comments` and
  `reactions.auto_merge` must declare the same SCM provider; a
  mismatch or an unknown provider fails startup. Workflows without an
  `auto_merge` block are unaffected. The `SCMAdapter` interface gains
  five write methods (`GetReviewDecision`, `GetCIStatus`,
  `GetMergeability`, `MergePR`, `DeleteBranch`) and a new
  `ErrSCMConflict` error kind; see
  [ADR-0012](https://github.com/sortie-ai/sortie/blob/main/docs/decisions/0012-auto-merge-reaction.md).
  ([#417](https://github.com/sortie-ai/sortie/issues/417))
- Extension `$VAR` resolution: every string leaf inside top-level
  front matter keys outside the core schema (for example
  `github.api_key`, `worker.ssh_hosts[0]`, `server.host`) now resolves
  `$VAR` and `${VAR}` environment indirection in a single pass during
  `NewServiceConfig`, after `SORTIE_*` overrides are applied. Nested
  maps and lists are traversed recursively; non-string leaves
  (integers, booleans, floats, timestamps, nil) are returned
  unchanged. `sortie validate` now emits a new advisory
  `unresolved_extension_var` warning naming the field path and the
  unset variable name when a referenced variable is absent from the
  process environment; the variable's resolved value never appears in
  any warning, log, or error. Exit code remains 0 and `valid` remains
  `true` when only this advisory warning is present. Operators of
  cross-platform setups (for example a Jira tracker paired with a
  GitHub SCM adapter) may now reference secrets such as
  `$SORTIE_GITHUB_TOKEN` directly inside the adapter extension block
  without an external `envsubst` step.
  ([#512](https://github.com/sortie-ai/sortie/issues/512))
- Dispatch rule routing: a new optional `dispatch:` block in
  `WORKFLOW.md` front matter routes each issue to a specific agent
  kind and prompt template based on issue metadata. Rules match
  first-wins on `labels`, `issue_type`, `priority`, `identifier`, and
  `assignee` (AND across keys, OR within a key), with optional
  `dispatch.default` and a final fallback to the workflow-wide
  `agent.kind` and body template. Per-rule templates live as
  Markdown files under the workflow tree and must not carry their
  own front matter; absolute paths, `~` expansion, and symlink
  targets that escape the tree are rejected at load time.
  `sortie validate` reports unknown agent kinds, unreachable
  catch-all rules, duplicate names, malformed globs and priority
  predicates, and missing or out-of-tree templates before dispatch.
  The resolved `(agent_kind, template_id, rule_name)` is frozen at
  first dispatch and reused across every retry and reaction
  continuation. Routing outcomes are exposed via the
  `sortie_dispatch_rule_match_total{layer,rule}` Prometheus counter
  and new `dispatched_by_rule` / `dispatched_by_default` /
  `dispatched_by_fallback` fields on the `tick completed` log line.
  Workflows without a `dispatch:` section are unaffected.
  ([#435](https://github.com/sortie-ai/sortie/issues/435))

### Fixed

- Orchestrator: CI-failure and review-comment retries now continue from
  the configured `tracker.handoff_state` instead of being dropped when
  the issue is no longer in an active state. Fresh dispatch remains
  limited to active states.
  ([#513](https://github.com/sortie-ai/sortie/issues/513))

### Migrations

- Add `rule_name`, `template_id`, and `agent_kind` to
  `retry_entries` and `rule_name`, `template_id` to `run_history`.
  Existing rows read back as empty strings and are treated as legacy
  fallback dispatches.

## [1.9.1] - 2026-05-14

### Fixed

- Orchestrator: CI and PR review pending reactions are now enqueued when
  `tracker.handoff_state` is configured. Previously, a successful handoff
  released the claim before `HandleWorkerExit` checked reaction
  eligibility, so the reconcile loop had no entry to poll - post-run CI
  failures and review comments on agent-created PRs went unobserved and
  no continuation turn was dispatched. Eligibility now derives from the
  exit-time claim state and the handoff path; blocked soft stops remain
  ineligible and existing review-reaction idempotency is preserved.
  ([#506](https://github.com/sortie-ai/sortie/issues/506))
- Orchestrator: handoff-stage pending review and CI reactions are now
  reconstructed on startup. `state.PendingReactions` is a runtime-only
  map, so a restart after a successful handoff previously left the
  issue with no pending review entry - the tracker issue was no longer
  active, the dispatch loop did not rediscover it, and human review
  comments on agent-created PRs went unobserved until an operator
  manually re-engaged the issue. Startup now rebuilds eligible review
  and CI pending entries from `run_history`, tracker state, and
  `.sortie/scm.json`, bounded to the most recent 200 unique issues
  completed within the last 30 days and gated by a single batched
  `FetchIssueStatesByIDs` call. Stale candidates age out via a new
  optional `pushed_at` field in `.sortie/scm.json` (falling back to
  `run_history.completed_at` when absent), and existing
  `reaction_fingerprints` continue to suppress duplicate review-fix
  dispatches.
  ([#507](https://github.com/sortie-ai/sortie/issues/507))

## [1.9.0] - 2026-04-26

### Added

- OpenCode CLI agent adapter: configure with `agent.kind: opencode` for
  autonomous issue-to-code workflows using the OpenCode CLI via
  `opencode run --format json`. Supports fork-per-turn execution, JSON
  event normalization, SSH remote dispatch, permission policy synthesis,
  token accounting, and companion deployment examples via
  `examples/docker/opencode.Dockerfile` and `examples/WORKFLOW.opencode.md`.
  ([#476](https://github.com/sortie-ai/sortie/issues/476),
  [#478](https://github.com/sortie-ai/sortie/issues/478),
  [#479](https://github.com/sortie-ai/sortie/issues/479))

### Fixed

- SSH worker command assembly: multi-token remote commands are now
  appended verbatim instead of shell-quoted as a single word, so remote
  agent launches such as `codex app-server` and other pre-formed shell
  fragments no longer fail with `command not found`.
  ([#493](https://github.com/sortie-ai/sortie/issues/493))
- GitHub and Jira tracker adapters no longer return partially populated
  issues, slices, or state maps when tracked fetch operations fail; the
  nil-on-error contract is preserved for issue detail and state lookups.
  ([PR #496](https://github.com/sortie-ai/sortie/pull/496))

## [1.8.0] - 2026-04-17

### Added

- Codex CLI agent adapter: configure with `agent.kind: codex` for
  autonomous issue-to-code workflows using OpenAI Codex CLI via the
  `codex app-server` JSON-RPC 2.0 protocol. Supports the same structured
  lifecycle as Claude Code and Copilot CLI adapters - event normalization,
  token tracking, timeout enforcement, graceful SIGTERM→SIGKILL shutdown,
  and session resume via `ResumeSessionID`. Tool calls are serialized
  through a channel to prevent concurrent stdin corruption. Handshake
  reads are cancellable via context, preventing stalled subprocesses from
  hanging the worker indefinitely.
  ([#238](https://github.com/sortie-ai/sortie/issues/238))

## [1.7.1] - 2026-04-15

### Changed

- CLI: `--version` now outputs a single diagnostic line including commit SHA,
  build date, Go version, and OS/architecture - e.g.
  `sortie 1.7.0 (commit: a1b2c3d, built: 2026-04-15, go1.26.1, linux/amd64)`.
  The previous GNU-style copyright/warranty block is removed. Build tooling
  (`Makefile`, `Dockerfile`, `.goreleaser.yaml`, and the release workflow) now
  injects `Commit` and `Date` via `-ldflags` at all build sites.

### Fixed

- Orchestrator: `sortie_ci_escalations_total` over-counted during CI escalation.
  `escalateCIFailure` incremented the metric unconditionally before calling the
  tracker API, then incremented again on error - producing two increments for one
  failed operation. It also incremented when `TrackerAdapter` was nil, recording a
  phantom escalation that was never performed. Both defects are fixed; the metric
  now increments exactly once per operation outcome, matching the pattern in
  `escalateReviewFailure`.
  ([#449](https://github.com/sortie-ai/sortie/issues/449))

## [1.7.0] - 2026-04-13

### Added

- Cross-retry session resume: continuation retries now propagate the
  session ID from the exiting worker through the retry entry so the next
  worker can resume the agent conversation instead of starting a fresh
  session. The session ID is persisted to SQLite and restored on startup
  recovery.
  ([#207](https://github.com/sortie-ai/sortie/issues/207))
- Token usage cost estimation on the dashboard and JSON API: operators
  configure per-adapter token rates in WORKFLOW.md front matter
  (`token_rates` block); the dashboard surfaces per-session and aggregate
  USD cost estimates computed from running sessions. The JSON API includes
  `active_estimated_cost_usd` when token rates are configured.
  ([#436](https://github.com/sortie-ai/sortie/issues/436))
- Dashboard accordion tables: Running Sessions, Retry Queue, and Run
  History tables use an expand/collapse accordion pattern. Primary status
  columns are visible at a glance; secondary detail expands on click.
  Expansion state survives the 5-second auto-refresh via sessionStorage.
  ([#432](https://github.com/sortie-ai/sortie/issues/432),
  [#444](https://github.com/sortie-ai/sortie/issues/444))
- `StderrCollector` buffer hardening: agent stderr collection now uses a
  10 MiB scanner buffer cap and a head/tail ring-buffer retention strategy
  with a configurable byte budget, preventing silent line truncation and
  unbounded memory growth during long agent turns.
  ([#387](https://github.com/sortie-ai/sortie/issues/387))

### Changed

- Retry timer tracker validation: `HandleRetryTimer` now calls
  `FetchIssueByID` instead of scanning all candidate issues via
  `FetchCandidateIssues`, reducing each retry timer fire from O(pages)
  tracker API calls to exactly one.
  ([#206](https://github.com/sortie-ai/sortie/issues/206))

### Migrations

- Add nullable `session_id TEXT` column to `retry_entries`

## [1.6.1] - 2026-04-11

### Added

- Runtime terminal workspace sweep: workspace directories for issues that
  reach a terminal tracker state after their worker has exited are now
  cleaned up periodically by the event loop. This closes the gap between
  the startup sweep and in-flight reconciliation, preventing unbounded
  disk accumulation on long-running instances.
  ([#428](https://github.com/sortie-ai/sortie/issues/428))

### Fixed

- Orchestrator: `needs-human-review` soft-stop now correctly triggers the
  handoff transition. Previously, the `SoftStop` branch in `HandleWorkerExit`
  matched all soft-stop reasons before the handoff case was reached, leaving
  the issue active and causing an infinite re-dispatch loop.
  ([#426](https://github.com/sortie-ai/sortie/issues/426))

## [1.6.0] - 2026-04-10

### Added

- Self-review loop before PR creation: the orchestrator generates a workspace
  diff, executes configurable verification commands (tests, linters), assembles
  a structured review prompt, and iterates with the agent up to a configurable
  cap before proceeding. Opt-in via the `self_review:` block in WORKFLOW.md
  front matter (`max_iterations`, `verify_commands`, `diff_max_bytes`).
  ([#312](https://github.com/sortie-ai/sortie/issues/312))
- PR review comment routing: when a reviewer requests changes on an
  agent-created PR, the orchestrator detects `CHANGES_REQUESTED` reviews,
  extracts the review comments, and dispatches a continuation turn so the
  agent can address feedback automatically. Configurable via
  `reactions.review_comments` in WORKFLOW.md (`max_retries`, `debounce_ms`,
  `escalation`, `escalation_label`). Includes `SCMAdapter` domain interface
  for PR and review operations with a GitHub Checks/Reviews API
  implementation.
  ([#305](https://github.com/sortie-ai/sortie/issues/305))
- Unified `reactions` config block in WORKFLOW.md for event-driven
  continuation triggers. `reactions.ci_failure` replaces the top-level
  `ci_feedback` key (which remains supported for backward compatibility).
  Each reaction type shares `provider`, `max_retries`, `escalation`, and
  `escalation_label` fields.
  ([#418](https://github.com/sortie-ai/sortie/issues/418))
- Windows process lifecycle support for agent adapters and workspace hooks.
  Agent subprocesses are now placed in Windows Job Objects with
  `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`, enabling full process tree cleanup on
  timeout or cancellation. Graceful shutdown sends `CTRL_BREAK_EVENT` to the
  process group; force-terminate uses `TerminateJobObject`. Workspace hooks
  execute via `cmd.exe /C` on Windows with their own Job Object for timeout
  enforcement. The `procutil` package exposes cross-platform functions -
  `SignalGraceful`, `AssignProcess`, `CleanupProcess`, `SetProcessGroup`,
  `KillProcessGroup` - and `WasSignaled` is now platform-aware. Adapters
  no longer reference `syscall.SIGTERM` directly.
  ([#390](https://github.com/sortie-ai/sortie/issues/390),
  [#391](https://github.com/sortie-ai/sortie/issues/391))

### Changed

- CI pending backoff base now derives from the operator-configured
  `poll_interval` instead of a hardcoded 10 s default. A 30 s poll interval
  produces a `(60 s, 120 s, 240 s, 300 s…)` backoff schedule. Falls back to
  10 s when `poll_interval` is zero or negative.
  ([#385](https://github.com/sortie-ai/sortie/issues/385))

### Deprecated

- `ci_feedback` top-level config key in WORKFLOW.md. Use
  `reactions.ci_failure` instead. The legacy key continues to work; when both
  are present, `reactions.ci_failure` takes precedence.
  ([#418](https://github.com/sortie-ai/sortie/issues/418))

### Fixed

- Workflow Manager continued using the pre-reconfiguration logger after
  `logging.level` was applied from WORKFLOW.md extensions, causing reload
  diagnostics to use the wrong log level. The Manager now updates its
  logger after every reconfiguration.
  ([#394](https://github.com/sortie-ai/sortie/issues/394))
- Reaction dispatch fingerprinting: `MarkReactionDispatched` was called at
  schedule time rather than actual dispatch time. If the process restarted
  between scheduling and dispatch, the fingerprint was permanently marked
  dispatched while the retry entry was lost, silencing future CI-fix
  reactions for that SHA.
  ([#420](https://github.com/sortie-ai/sortie/issues/420))
- Config: non-string YAML values in `reactions` fields (`provider`,
  `escalation`, `escalation_label`) now produce a `ConfigError` instead of
  silently coercing to an empty string.
  ([#423](https://github.com/sortie-ai/sortie/issues/423))
- `install.sh`: detect Rosetta 2 on macOS and prefer the native `arm64`
  binary over `amd64`.
  ([#391](https://github.com/sortie-ai/sortie/issues/391))

### Migrations

- Add nullable `review_metadata TEXT` column to `run_history`
- Add `reaction_fingerprints` table for cross-restart reaction
  deduplication

## [1.5.1] - 2026-04-08

### Added

- GNU-style CLI help output with grouped sections, column-aligned
  descriptions, usage examples, and a "Learn more" link. Short aliases
  `-h` (help) and `-V` (version) are now recognized. Help text prints
  to stdout instead of stderr. Covers all three commands: `sortie`,
  `sortie validate`, `sortie mcp-server`.
  ([#398](https://github.com/sortie-ai/sortie/issues/398))

### Fixed

- Copilot CLI adapter: prefix `--additional-mcp-config` file paths with
  `@` to match the documented Copilot CLI syntax. Bare file paths
  worked in Copilot CLI ≤1.0.18 via an undocumented fallback that was
  removed in v1.0.21, causing `Invalid JSON in --additional-mcp-config`
  on every turn. Operator-provided `copilot-cli.mcp_config` values are
  now auto-detected: inline JSON is passed unchanged, `@`-prefixed
  paths are preserved, and bare file paths receive the `@` prefix
  automatically.
  ([#404](https://github.com/sortie-ai/sortie/issues/404))
- Agent adapters: reclassify exit-code-0 turns with zero output tokens
  and no `result` event as `turn_failed` instead of `turn_completed`.
  Previously, when an agent subprocess crashed immediately (e.g., MCP
  config parse error) but exited 0, all turns were counted as
  successful, causing the orchestrator to exhaust `max_turns` and
  trigger a false-positive handoff transition. Failed turns now retry
  with exponential backoff. Applies to both Claude Code and Copilot CLI
  adapters.
  ([#404](https://github.com/sortie-ai/sortie/issues/404))

## [1.5.0] - 2026-04-07

### Added

- Always-on HTTP server with default port 7678 (mnemonic: SORT on T9).
  The server now starts unconditionally; the `--port` flag overrides the
  default but no longer acts as an activation trigger. Pass `--port=0`
  to disable. Prometheus metrics, health probes, and dashboard are
  available out of the box without flags.
- `--host` CLI flag and `server.host` workflow config field for
  configurable bind address. Default `127.0.0.1`; container deployments
  override with `--host 0.0.0.0`. Resolution order: CLI flag > config >
  default.
- `--log-format` CLI flag with values `text` (default) and `json`.
  JSON format emits one JSON object per log line with `time`, `level`,
  `msg`, and structured fields (`issue_id`, `session_id`, etc.) for
  integration with Loki, Datadog, CloudWatch, and ELK.
- Dockerfile: multi-stage build producing a distroless container image
  with only `/usr/bin/sortie`. Users consume via
  `COPY --from=ghcr.io/sortie-ai/sortie:latest /usr/bin/sortie /usr/bin/sortie`
  in their own Dockerfile. Build flags match `.goreleaser.yaml`:
  `CGO_ENABLED=0`, `-trimpath`, `-s -w`, tags `osusergo,netgo`.
- `.dockerignore` excluding build artifacts, `.git`, test fixtures, and
  documentation.
- Agent-specific example Dockerfiles:
  [`docker/claude-code.Dockerfile`](https://github.com/sortie-ai/sortie/blob/1.5.0/examples/docker/claude-code.Dockerfile)
  and [`docker/copilot.Dockerfile`](https://github.com/sortie-ai/sortie/blob/1.5.0/examples/docker/copilot.Dockerfile)
  with non-root user, health checks, and volume mounts.
- Kubernetes deployment examples: [`k8s/deployment.yaml`](https://github.com/sortie-ai/sortie/blob/1.5.0/examples/k8s/deployment.yaml)
  (Recreate strategy, liveness/readiness probes on `/livez` and `/readyz`),
  [`k8s/configmap.yaml`](https://github.com/sortie-ai/sortie/blob/1.5.0/examples/k8s/configmap.yaml),
  [`k8s/service.yaml`](https://github.com/sortie-ai/sortie/blob/1.5.0/examples/k8s/service.yaml),
  [`k8s/pvc.yaml`](https://github.com/sortie-ai/sortie/blob/1.5.0/examples/k8s/pvc.yaml).
- Grafana dashboard template at [`grafana-dashboard.json`](https://github.com/sortie-ai/sortie/blob/1.5.0/examples/grafana-dashboard.json)
  covering all 22 Prometheus metrics. Panels for `dispatch_transitions_total`,
  `tracker_comments_total`, `ci_status_checks_total`, and
  `ci_escalations_total` added in dedicated CI Feedback and Integration
  rows. Uses `__inputs`/`DS_PROMETHEUS` pattern for portable data source
  selection.

### Fixed

- Agent stderr is now surfaced at WARN level when a turn fails instead
  of DEBUG. Both Claude Code and Copilot CLI adapters buffer stderr
  during a turn and log at WARN on non-zero exit, making startup
  rejections (e.g., `--dangerously-skip-permissions` under root)
  visible at default log level. Successful turns continue to log stderr
  at DEBUG.

## [1.4.0] - 2026-04-04

### Added

- CI feedback loop: when a CI pipeline fails on an agent-created branch,
  the orchestrator detects the failure, injects the CI failure logs into
  the next agent turn, and dispatches a continuation session so the agent
  can diagnose and fix the issue automatically. Controlled by a new
  `ci_feedback` config section in `WORKFLOW.md` (`kind`, `max_retries`,
  `max_log_lines`, `escalation`). Feature activation follows kind-based
  convention: present `ci_feedback.kind` enables, absent disables.
- `CIStatusProvider` domain interface: adapter contract for fetching CI
  check status from an SCM platform. Returns structured `CIResult` with
  overall status (pending/passing/failing), individual check runs, and
  an optional log excerpt from the first failing check.
- GitHub `CIStatusProvider` implementation via the Checks API: fetches
  check runs for a git ref, computes aggregate status, and retrieves
  truncated log output from the first failing GitHub Actions job. Log
  fetching is controlled by `max_log_lines` (0 disables). ANSI escape
  sequences are stripped from log output.
- CI failure escalation: when `ci_feedback.max_retries` is exceeded, the
  orchestrator applies a configurable escalation action -- add a label
  (default `needs-human`) or post a comment on the issue.
- Exponential backoff for CI pending re-enqueue: `reconcileCIStatus`
  now applies `base * 2^attempts` backoff (capped at 5 minutes) when CI
  checks remain pending or on transient API errors, reducing GitHub
  Checks API request volume from ~120 to ~15 per 20-minute CI run per
  issue. Stale pending entries expire after a 30-minute TTL.
- `TrackerOpsWg` shutdown drain: fire-and-forget tracker API goroutines
  (comment posting, label adding) are now tracked by a dedicated
  `sync.WaitGroup` and drained during graceful shutdown with a 35-second
  timeout, preventing orphaned goroutines on process exit.
- Automatic credential merging: when `ci_feedback.kind` matches
  `tracker.kind`, the orchestrator merges tracker credentials (`api_key`,
  `project`, `endpoint`) into the CI provider adapter config at startup,
  eliminating the need to duplicate credentials in a pass-through block.

## [1.3.0] - 2026-04-03

### Added

- MCP tool execution channel: agents can now call registered tools at
  runtime via the Model Context Protocol. The worker generates
  `.sortie/mcp.json` per session and passes it to the agent runtime via
  `--mcp-config` (Claude Code) or `--additional-mcp-config` (Copilot CLI).
  The agent runtime spawns `sortie mcp-server` as a stdio sidecar; the
  orchestrator does not manage the sidecar lifecycle.
- `sortie mcp-server` subcommand: MCP stdio JSON-RPC server that exposes
  registered `AgentTool` implementations via `tools/list` and `tools/call`.
  Constructs its own `TrackerAdapter` and `ToolRegistry` by re-reading
  WORKFLOW.md from an absolute path passed via `--workflow`.
- `sortie_status` MCP tool (Tier 1): returns live session runtime
  metadata -- current turn number, remaining turns, attempt number,
  session duration, and cumulative token usage. Reads from
  `.sortie/state.json`, a worker-written file updated at session start,
  each turn start, and on token usage events.
- `workspace_history` MCP tool (Tier 1): returns up to 10 most recent
  completed run attempts for the current issue from the `run_history`
  SQLite table. Opens the database in read-only mode (`?mode=ro`).
  Non-fatal on database open failure -- the MCP server continues with
  other tools available.
- Agent-to-orchestrator file protocol: agents can write `blocked` or
  `needs-human-review` to `.sortie/status` to suppress continuation
  retries. The orchestrator reads the file after each turn and before
  the tracker state refresh. Absent, unrecognized, or unreadable files
  degrade to normal behavior. Symlinks on either path component are
  rejected via `Lstat`.
- `RuntimeStatusSuffix` auto-injection: the orchestrator appends A2O
  protocol instructions to the first-turn prompt so agents know how to
  signal blocked status without workflow author intervention.
  Continuation turns omit the suffix.
- Soft-stop exit path in `HandleWorkerExit`: when the worker exits with
  a recognized status file signal, the orchestrator releases the claim
  and suppresses continuation retry. The issue re-dispatches only on
  tracker state change.
- Operator MCP config merging: if `mcp_config` is set in WORKFLOW.md,
  the worker merges the operator's config with the `sortie-tools` entry.
  Name collision on `sortie-tools` is a validation error.

### Documentation

- Agent-to-orchestrator file protocol specification
  (`docs/agent-to-orchestrator-protocol.md`): 9-section normative
  document covering file format, recognized values, read timing,
  cleanup lifecycle, symlink rejection, and conformance checklist.
- ADR-0009: MCP stdio sidecar for tool execution -- documents the
  chosen transport mechanism, process model, credential handling,
  adapter integration, and alternatives analysis.
- Architecture Section 10.4 rewrite: `AgentTool` interface contract,
  `ToolRegistry` invariants, tier classification framework, and
  `tracker_api` tool specification aligned with implementation.

## [1.2.1] - 2026-04-01

### Fixed

- Prevent re-dispatch loop when effort budget (`agent.max_sessions`) is
  exhausted for an issue; the orchestrator now records a durable
  budget-exhaustion guard so the dispatch loop does not restart the issue
  on the next tick
- Persist and display `turns_completed` per run in the dashboard and run
  history table
- Show fully qualified `owner/repo#N` display identifiers for GitHub
  issues in the dashboard and API instead of bare issue numbers
- Pass `handoff_state` to `findCurrentStateLabel`, `extractState`,
  `normalizeIssue`, and `normalizeBlockers` in the GitHub adapter so
  `TransitionIssue` accepts the handoff state and stale labels are
  removed during transitions
- Rename dashboard footer cache label from "Cache:" to "Cache Read:" and
  add explanatory tooltips in the footer and Running Sessions table
- Defer `token_usage` event emission in the Claude adapter when an
  assistant message carries zero output tokens (tool_use-only messages
  in Claude Code 2.x `stream-json` format); the adapter now accumulates
  input tokens and falls back to the result event which carries correct
  totals

### Migrations

- Add index `idx_run_history_issue_id` on `run_history(issue_id)`
- Add `turns_completed INTEGER NOT NULL DEFAULT 0` to `run_history`
- Add nullable `display_identifier` column to `run_history`

## [1.2.0] - 2026-03-31

### Added

- GitHub Copilot CLI adapter: configure with `agent.kind: copilot` for
  fully automated issue-to-code workflows using GitHub's headless Copilot
  CLI. Supports local execution and SSH remote dispatch via `worker.ssh_hosts`.
  Tool scope is controlled by `allowed_tools`, `denied_tools`,
  `available_tools`, and `excluded_tools`; `--allow-all` is the default when
  none are set. Session continuity across turns via `--resume`. Authentication
  uses token env vars when present, falling back to `gh auth status`.
- `worker.ssh_strict_host_key_checking`: new optional worker config field
  controlling OpenSSH `StrictHostKeyChecking` for remote SSH agent sessions.
  Accepts `accept-new` (default, Trust On First Use), `yes` (strict
  verification, requires a pre-populated `known_hosts`), or `no` (disable
  host-key checking). Applies to both the Claude Code and Copilot CLI
  adapters.

## [1.1.0] - 2026-03-30

### Added

- GitHub Issues tracker adapter: configure with `tracker.kind: github` and
  `tracker.project: OWNER/REPO`. State management is label-based;
  `TransitionIssue` applies and removes GitHub labels with convergent retry on
  partial failure.
- GitHub adapter: in-memory ETag cache for reconciliation polls -
  `If-None-Match` conditional requests return `304 Not Modified` on unchanged
  issues, reducing GitHub API rate limit consumption during active runs.
- `sortie validate` GitHub adapter config validation: emits diagnostics for
  `tracker.project` format (`OWNER/REPO`), `GITHUB_TOKEN` environment variable
  hint, empty state labels, and active/terminal state label overlap. Errors
  block dispatch; warnings are advisory.

### Fixed

- `install.sh`: checksum verification used substring matching (`grep | awk`)
  that could accept a checksum entry for the wrong archive entry; now uses
  exact field matching (`awk '$2 == f'`).

## [1.0.0] - 2026-03-29

### Added

- SPDX JSON Software Bill of Materials (SBOM) included with every release
  archive, generated via `syft` in the GoReleaser pipeline for supply-chain
  auditing.

## [0.0.10] - 2026-03-28

### Added

- `sortie validate` template static analysis: three advisory warning
  classes - `WarnDotContext` (top-level key referenced inside `{{ range }}`
  or `{{ with }}` where dot is redefined), `WarnUnknownVar` (variable not in
  the `{issue, attempt, run}` contract), and `WarnUnknownField` (valid
  top-level key with an unknown sub-field, including depth-4+ field chains on
  known level-3 scalars). Warnings appear in both text and JSON output
  without blocking dispatch or changing the exit code.
- `sortie validate` front matter schema validation: detects unknown
  top-level keys, unknown sub-keys within known sections, type mismatches,
  and semantic issues (non-positive `hooks.timeout_ms`, non-numeric
  `max_concurrent_agents_by_state` entries). Field paths are included in
  every diagnostic so operators can locate the offending key. Warnings are
  advisory only.
- `SORTIE_*` environment variable config overrides: any workflow front
  matter key can be overridden via a `SORTIE_`-prefixed environment variable
  (e.g., `SORTIE_POLLING_INTERVAL_MS=5000`). Non-empty real env vars take
  precedence over `.env` file values. Raw line content is removed from `.env`
  parse errors and override values are excluded from debug logs to prevent
  secret leakage.
- Orchestrator: tracker comments posted at session lifecycle points -
  session start, successful completion, and failure - with run duration and
  attempt metadata. Comments fire from a detached goroutine to avoid blocking
  the event loop.
- Orchestrator: issues are transitioned to the configured `in_progress_state`
  on dispatch, with a no-op skip when the issue is already in the
  target state. `sortie_dispatch_transitions_total` Prometheus counter
  tracks `success`, `error`, and `skipped` outcomes.

## [0.0.9] - 2026-03-27

### Added

- `sortie validate` subcommand for one-shot workflow file validation
  without starting the orchestrator, opening the database, or spawning a
  filesystem watcher. Supports `--format text` (stderr diagnostics) and
  `--format json` (structured stdout output) for CI pipelines and pre-commit
  hooks. Flag-parse errors are routed through the diagnostics emitter.
- `--dry-run` flag for a single read-only poll cycle that validates
  the full startup sequence (workflow load, preflight, database, adapter
  wiring) without dispatching work or persisting state.
- `--log-level` flag and `logging.level` workflow extension key to set
  the minimum log severity at startup (`debug`, `info`, `warn`, `error`).
- Dashboard: Workflow column in Active Sessions table and a new Run History
  table showing completed session outcomes, timing, and workflow file.
  SQL migration 003 adds a nullable `workflow_file` column to `run_history`.
- Workspace root write-permission check in dispatch preflight -
  surfaces a clear diagnostic instead of failing mid-dispatch.
- Homebrew tap distribution via GoReleaser-managed tap repository
  (`brew install sortie-ai/tap/sortie`).
- Jira adapter: `User-Agent` header (`sortie/<version>`) sent on
  every HTTP request.
- Claude Code adapter: per-request `APIDurationMS` on `token_usage` events
  for API-call-level latency visibility, clamped to a minimum of
  1 ms.
- Claude Code adapter: tool error text now included in
  `EventToolResult.Message`.

### Fixed

- CLI: adapters implementing `io.Closer` are now closed during graceful
  shutdown, preventing resource leaks.
- Orchestrator: `TurnCount` increments on `session_started` instead of
  session finalization, correctly reflecting in-progress turns.
- Claude Code adapter: tool error messages stripped of XML markup and
  tail-truncated to prevent oversized events.

## [0.0.8] - 2026-03-26

### Added

- JSON API server with `GET /api/v1/state`, `GET /api/v1/<identifier>`, and
  `POST /api/v1/refresh` endpoints for programmatic access to orchestrator
  state. Enabled via `--port` flag or `server.port` config.
- HTML dashboard at `/` with auto-refreshing view of running sessions, retry
  queue, token totals, and runtime statistics when the HTTP server is enabled.
- `/livez` and `/readyz` health endpoints following Kubernetes z-pages
  conventions. `/readyz` checks database accessibility, preflight validation,
  and workflow loading.
- Prometheus `/metrics` endpoint exposing session gauges, dispatch/worker/retry
  counters, token counters, tracker request counters, tool call counters,
  poll and worker duration histograms, and `sortie_build_info`. Uses a dedicated
  `prometheus.Registry` - compatible with standard Prometheus scrape configs.
- `tracker_api` client-side tool: agents can query the tracker during sessions
  to fetch issues and comments, scoped to the configured project.
- SSH worker extension via `worker.ssh_hosts` config: dispatch agent runs to
  remote hosts over SSH with round-robin host selection and per-host concurrency
  limits (`worker.max_concurrent_agents_per_host`).
- Per-session token breakdown in JSON API and dashboard: `input_tokens`,
  `output_tokens`, `cache_creation_tokens`, `cache_read_tokens`.
- Per-session timing breakdown in JSON API and dashboard: `elapsed`,
  `agent_time`, `idle_time`, `agent_pct`.
- Claude Code adapter: `tool_result` events now emitted, making agent tool
  invocations visible in the dashboard and API.
- Worker failure logging in `HandleWorkerExit`: WARN with `next_attempt` and
  `delay_ms` for retryable errors, ERROR for non-retryable errors.
- Structured logging: `issue_id`, `issue_identifier`, and `session_id` context
  fields now present on all orchestrator lifecycle log lines. Agent tool calls
  logged at INFO level.
- POSIX-compatible install script (`install.sh`) for automated binary
  installation.

### Changed

- `POST /api/v1/refresh` returns `409 Conflict` during graceful shutdown
  instead of accepting requests that cannot be fulfilled.

### Fixed

- Claude Code adapter: duplicate `token_usage` events no longer emitted when
  assistant-level usage is already reported in the result message.
- HTTP server: `405 Method Not Allowed` responses now include the `Allow`
  header per RFC 9110.
- Jira adapter: `sortie_tracker_requests_total` counter no longer increments
  on no-op calls with empty ID lists.

## [0.0.7] - 2026-03-24

### Added

- Graceful shutdown: on SIGTERM/SIGINT the orchestrator now drains running
  workers (up to 30 s), persists final state to SQLite, flushes pending
  agent events, and cancels retry timers before exiting.
- Issue handoff via `tracker.handoff_state` config field - when an agent
  session completes normally and the issue is still in an active state, the
  orchestrator transitions it to the configured handoff state (e.g.,
  "In Review") and skips the continuation retry.
- `TransitionIssue` operation on the `TrackerAdapter` interface - Jira
  adapter uses the workflow transitions API; file adapter uses an in-memory
  override map.
- Per-issue effort budget via `agent.max_sessions` - limits total agent
  sessions dispatched per issue before releasing the claim. Default 0
  (unlimited).
- Documentation site at https://docs.sortie-ai.com/ with initial
  configuration reference.

### Fixed

- Orchestrator: continuation retry attempt counter now increments correctly
  across sessions instead of resetting to 1 on every normal exit.
- CLI: orchestrator-only fields (`max_turns`, `max_concurrent_agents`,
  `max_retry_backoff_ms`, `max_concurrent_agents_by_state`) removed from
  the adapter config map, fixing silent shadowing of adapter extension keys
  such as `claude-code.max_turns`.
- Jira adapter: `extractStringSlice` now handles `[]string` from the config
  layer - previously only `[]any` was handled, silently reverting to default
  states and causing configured `active_states` / `terminal_states` to be
  ignored.
- Jira adapter: `FetchIssueStatesByIDs` now queries by numeric `id` instead
  of `key`, and results are keyed by issue ID, fixing reconciliation failures
  where state changes on running issues were never detected.
- Jira adapter: non-numeric IDs are now rejected instead of silently mangled,
  and empty ID lists no longer produce invalid `id IN ()` JQL.
- File and Jira adapters now return `ErrTrackerNotFound` for missing issues
  in `FetchIssueByID` and `FetchIssueComments`.
- Orchestrator: INFO-level tick summary log after each dispatch cycle with
  candidate, dispatched, running, and retrying counters to distinguish normal
  operation from a stall.

## [0.0.6] - 2026-03-23

### Added

- Orchestrator engine with state management, concurrency-limited dispatch,
  worker lifecycle, exponential-backoff retry scheduling, active-run
  reconciliation, and event-driven poll loop with graceful shutdown.
- Full startup sequence: workflow load, preflight validation, database open,
  state reconciliation, and poll loop - in that order.
- Dispatch preflight checks that validate adapter availability, required API
  keys, and agent configuration before dispatching work.
- Adapter metadata via `AdapterMeta` and `RegisterWithMeta` so adapters can
  declare requirements (e.g., `RequiresAPIKey`) checked during preflight.
- Retry classification on `TrackerErrorKind` and `AgentErrorKind` - errors
  are now classified as retryable or permanent for dispatch decisions.
- `ErrTrackerNotFound` error kind for HTTP 404 responses from tracker
  adapters.
- Configurable `db_path` field in workflow configuration with `~` and `$VAR`
  expansion.
- Workflow validation callback (`ValidateFunc`) that guards config promotion
  during hot-reload.

### Fixed

- Workspace `CleanupByPath` now rejects non-canonical paths and uses the
  actual workspace path for pending cleanup instead of reconstructing it
  from config.
- Startup: preflight checks now run before opening the database, preventing
  `.sortie.db` creation when configuration is invalid.
- Startup: `.sortie.db` is now created adjacent to WORKFLOW.md instead of in
  the working directory.

## [0.0.5] - 2026-03-21

### Added

- Workspace manager: safe path computation from issue identifiers with
  containment validation and symlink rejection.
- Workspace manager: atomic directory creation and reuse with `CreatedNow`
  flag for hook gating.
- Workspace hook execution with configurable timeout, truncated output
  capture, and restricted subprocess environment (only `PATH`, `HOME`,
  `SHELL`, and `SORTIE_*` variables are inherited).
- Workspace lifecycle orchestration: `Prepare`, `Finish`, and `Cleanup`
  functions that sequence `after_create`, `before_run`, `after_run`, and
  `before_remove` hooks with appropriate failure semantics (fatal vs
  best-effort) and `context.WithoutCancel` for teardown hooks.
- Batch workspace cleanup (`CleanupTerminal`) for removing terminal-state
  issue workspaces with per-identifier error collection and best-effort
  `before_remove` hook execution.
- `ListWorkspaceKeys` for enumerating workspace directory names under a
  root, skipping non-directories and symlinks.

## [0.0.4] - 2026-03-20

### Added

- `AgentAdapter` interface and normalized event model: 13 event types,
  `TokenUsage`, `AgentConfig`, `Session`, `TurnResult`, and `AgentError` with
  9 error kinds.
- Agent adapter registry (`registry.Agents`) for registration and lookup by kind.
- Mock agent adapter (kind `"mock"`) with configurable turn outcomes, delays,
  and cumulative token accumulation for orchestrator and integration testing.
- Claude Code agent adapter (kind `"claude-code"`) that launches the CLI as a
  subprocess, reads JSONL events from stdout, and normalizes them to domain event
  types. Supports graceful SIGTERM→SIGKILL shutdown on context cancellation and
  session resumption via `ResumeSessionID`.

### Fixed

- Claude Code adapter: double-wait race between `RunTurn` and `StopSession` -
  `gracefulKill` is now fire-and-forget with timer-based SIGKILL escalation.
- Claude Code adapter: error on missing binary now includes the actual command
  name instead of a hardcoded string.

## [0.0.3] - 2026-03-20

### Added

- Normalized `Issue` model and `TrackerAdapter` interface for multi-tracker support.
- Typed adapter registry with thread-safe registration and lookup.
- File-based tracker adapter for local JSON task definitions.
- Jira Cloud REST API v3 adapter with cursor-based paginated search, issue detail
  retrieval, state tracking, and comment fetching.
- BFS flattener for Atlassian Document Format (ADF) descriptions to plain text.
- JQL builder with string escaping and optional `query_filter` clause support.
- `query_filter` field in tracker configuration for custom JQL expressions.
- GoReleaser configuration for reproducible cross-platform binary releases
  (linux/darwin/windows, amd64/arm64).

### Fixed

- Jira search endpoint migrated from retired `/rest/api/3/search` to
  `/rest/api/3/search/jql` (Atlassian returns 410 Gone on the old endpoint).
- Infinite loop guard in Jira comment pagination when the API returns
  inconsistent offsets.

## [0.0.2] - 2026-03-19

### Added

- SQLite persistence layer with WAL mode and single-writer enforcement.
- Schema migration runner with versioned SQL files.
- CRUD operations for retry entries, run history, session metadata, and
  aggregate metrics.
- Startup recovery loader that resumes incomplete retry entries on restart.

### Fixed

- Deterministic ordering for session metadata queries via `session_id`
  tie-breaker.

## [0.0.1] - 2026-03-18

### Added

- WORKFLOW.md file loader with YAML front matter and prompt body parsing.
- Typed configuration layer with `$VAR` environment variable resolution
  and `~` home directory expansion.
- Prompt template engine using Go `text/template` in strict mode
  (unknown variables and filters cause hard errors).
- Turn-based prompt builder for multi-turn agent conversations.
- Filesystem watcher for live WORKFLOW.md reload via `fsnotify`.
- CLI entry point (`sortie`) with graceful shutdown and signal handling.

### Fixed

- Environment variable expansion now preserves inline `$VAR` references
  inside URIs instead of silently dropping them.
- Fractional float values no longer silently coerced to integers during
  config parsing.

## [0.0.0] - 2026-03-18

### Added

- Go module scaffold and project directory structure.
- Structured logging built on `log/slog` with issue-aware and
  session-aware contextual fields.
- CI pipeline with `golangci-lint`, `gofmt` enforcement, and test
  execution via GitHub Actions.
- Architecture Decision Records (ADR-0001 through ADR-0005).

[Unreleased]: https://github.com/sortie-ai/sortie/compare/1.17.0...HEAD
[1.17.0]: https://github.com/sortie-ai/sortie/compare/1.16.1...1.17.0
[1.16.1]: https://github.com/sortie-ai/sortie/compare/1.16.0...1.16.1
[1.16.0]: https://github.com/sortie-ai/sortie/compare/1.15.0...1.16.0
[1.15.0]: https://github.com/sortie-ai/sortie/compare/1.14.1...1.15.0
[1.14.1]: https://github.com/sortie-ai/sortie/compare/1.14.0...1.14.1
[1.14.0]: https://github.com/sortie-ai/sortie/compare/1.13.0...1.14.0
[1.13.0]: https://github.com/sortie-ai/sortie/compare/1.12.0...1.13.0
[1.12.0]: https://github.com/sortie-ai/sortie/compare/1.11.0...1.12.0
[1.11.0]: https://github.com/sortie-ai/sortie/compare/1.10.0...1.11.0
[1.10.0]: https://github.com/sortie-ai/sortie/compare/1.9.1...1.10.0
[1.9.1]: https://github.com/sortie-ai/sortie/compare/1.9.0...1.9.1
[1.9.0]: https://github.com/sortie-ai/sortie/compare/1.8.0...1.9.0
[1.8.0]: https://github.com/sortie-ai/sortie/compare/1.7.1...1.8.0
[1.7.1]: https://github.com/sortie-ai/sortie/compare/1.7.0...1.7.1
[1.7.0]: https://github.com/sortie-ai/sortie/compare/1.6.1...1.7.0
[1.6.1]: https://github.com/sortie-ai/sortie/compare/1.6.0...1.6.1
[1.6.0]: https://github.com/sortie-ai/sortie/compare/1.5.1...1.6.0
[1.5.1]: https://github.com/sortie-ai/sortie/compare/1.5.0...1.5.1
[1.5.0]: https://github.com/sortie-ai/sortie/compare/1.4.0...1.5.0
[1.4.0]: https://github.com/sortie-ai/sortie/compare/1.3.0...1.4.0
[1.3.0]: https://github.com/sortie-ai/sortie/compare/1.2.1...1.3.0
[1.2.1]: https://github.com/sortie-ai/sortie/compare/1.2.0...1.2.1
[1.2.0]: https://github.com/sortie-ai/sortie/compare/1.1.0...1.2.0
[1.1.0]: https://github.com/sortie-ai/sortie/compare/1.0.0...1.1.0
[1.0.0]: https://github.com/sortie-ai/sortie/compare/0.0.10...1.0.0
[0.0.10]: https://github.com/sortie-ai/sortie/compare/0.0.9...0.0.10
[0.0.9]: https://github.com/sortie-ai/sortie/compare/0.0.8...0.0.9
[0.0.8]: https://github.com/sortie-ai/sortie/compare/0.0.7...0.0.8
[0.0.7]: https://github.com/sortie-ai/sortie/compare/0.0.6...0.0.7
[0.0.6]: https://github.com/sortie-ai/sortie/compare/0.0.5...0.0.6
[0.0.5]: https://github.com/sortie-ai/sortie/compare/0.0.4...0.0.5
[0.0.4]: https://github.com/sortie-ai/sortie/compare/0.0.3...0.0.4
[0.0.3]: https://github.com/sortie-ai/sortie/compare/0.0.2...0.0.3
[0.0.2]: https://github.com/sortie-ai/sortie/compare/0.0.1...0.0.2
[0.0.1]: https://github.com/sortie-ai/sortie/compare/0.0.0...0.0.1
[0.0.0]: https://github.com/sortie-ai/sortie/releases/tag/0.0.0
